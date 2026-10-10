// Package web exposes the bus over HTTP: JSON API, SSE stream, embedded UI.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/babs/airc/pkg/bus"
	"github.com/babs/airc/pkg/driver"
	"github.com/babs/airc/pkg/proto"
	"github.com/babs/airc/pkg/store"
)

// StatusFunc reports live agent state; nil means no driver is attached.
type StatusFunc func(context.Context) ([]driver.AgentStatus, error)

//go:embed ui/index.html
var ui embed.FS

// Server serves the API.
type Server struct {
	bus        *bus.Bus
	adminToken string
	log        *zap.Logger
	status     StatusFunc
	boot       string
}

// New builds a server; adminToken authorises humans and commands. boot identifies this process
// lifetime: the UI reloads itself when it changes.
func New(b *bus.Bus, adminToken string, log *zap.Logger, status StatusFunc, boot string) *Server {
	return &Server{bus: b, adminToken: adminToken, log: log, status: status, boot: boot}
}

// Handler returns the routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, ui, "ui/index.html")
	})
	mux.Handle("POST /msg", s.auth(s.postMsg))
	mux.Handle("GET /msgs", s.auth(s.getMsgs))
	mux.Handle("GET /events", s.auth(s.events))
	mux.Handle("GET /agents", s.auth(s.getAgents))
	mux.Handle("GET /channels", s.auth(s.getChannels))
	mux.Handle("GET /status", s.auth(s.getStatus))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Airc-Boot", s.boot)
		mux.ServeHTTP(w, r)
	})
}

// principal is who holds the token: the admin, or one agent.
type principal struct {
	admin bool
	agent *store.Agent
}

func (s *Server) auth(next func(http.ResponseWriter, *http.Request, principal)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if tok == "" {
			// EventSource cannot set headers.
			tok = r.URL.Query().Get("token")
		}
		if tok == "" {
			httpError(w, http.StatusUnauthorized, "missing token")
			return
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) == 1 {
			next(w, r, principal{admin: true})
			return
		}
		ag, err := s.bus.Store.AgentByToken(r.Context(), tok)
		if errors.Is(err, store.ErrNotFound) {
			httpError(w, http.StatusUnauthorized, "bad token")
			return
		}
		if err != nil {
			s.log.Error("auth_lookup_failed", zap.Error(err))
			httpError(w, http.StatusInternalServerError, "auth failed")
			return
		}
		if !ag.Enabled {
			httpError(w, http.StatusUnauthorized, "agent stopped")
			return
		}
		next(w, r, principal{agent: ag})
	})
}

type postReq struct {
	Channel string `json:"channel"`
	From    string `json:"from"`
	To      string `json:"to"`
	Body    string `json:"body"`
}

func (s *Server) postMsg(w http.ResponseWriter, r *http.Request, p principal) {
	var req postReq
	// 256 KiB: a message is a chat line plus a diff, and up to ContextLimit of them form one prompt.
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<10)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if req.Body == "" {
		httpError(w, http.StatusBadRequest, "empty body")
		return
	}
	if p.agent != nil {
		// An agent token pins identity and channel server-side: no impersonation.
		req.From, req.Channel = p.agent.Nick, p.agent.Channel
	}
	if !proto.ValidNick(req.From) || req.From == proto.SystemNick {
		httpError(w, http.StatusBadRequest, "invalid from")
		return
	}
	if !proto.ValidNick(req.Channel) {
		httpError(w, http.StatusBadRequest, "invalid channel")
		return
	}
	if req.To == "" {
		req.To = proto.Address(req.Body)
	}
	if req.To != "" && !proto.ValidAddress(req.To) {
		httpError(w, http.StatusBadRequest, "invalid to")
		return
	}
	if toChan, toNick := proto.SplitAddress(req.To); p.agent != nil && req.To != "" && toChan == "" {
		// A bare nick must be a peer of the agent's own channel; "channel:nick" is relayed by the driver.
		if peer, err := s.bus.Store.Agent(r.Context(), toNick); err == nil && peer.Channel != p.agent.Channel {
			httpError(w, http.StatusForbidden, "addressee is in another channel; use @"+peer.Channel+":"+toNick)
			return
		}
	}
	m := &store.Message{Channel: req.Channel, From: req.From, To: req.To, Body: req.Body}
	if err := s.bus.Post(r.Context(), m); err != nil {
		s.log.Error("post_failed", zap.Error(err))
		httpError(w, http.StatusInternalServerError, "store failed")
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) getMsgs(w http.ResponseWriter, r *http.Request, p principal) {
	q := r.URL.Query()
	channel := s.channelFor(p, q.Get("channel"))
	since, _ := strconv.ParseInt(q.Get("since"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 1000 // one page at most; clients paginate with since=
	}
	msgs, err := s.bus.Store.Messages(r.Context(), channel, since, 0, limit)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if msgs == nil {
		msgs = []store.Message{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// channelFor restricts agents to their own channel.
func (s *Server) channelFor(p principal, requested string) string {
	if p.agent != nil {
		return p.agent.Channel
	}
	return requested
}

func (s *Server) events(w http.ResponseWriter, r *http.Request, p principal) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	channel := s.channelFor(p, r.URL.Query().Get("channel"))
	last, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)

	// The live channel is only a wake-up signal: every send reads from the store after the last id,
	// so a dropped or out-of-order broadcast can never leave a gap in the stream.
	live, cancel := s.bus.Subscribe(64)
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	catchUp := func() bool {
		for {
			msgs, err := s.bus.Store.Messages(r.Context(), channel, last, 0, 500)
			if err != nil {
				return false
			}
			for _, m := range msgs {
				last = m.ID
				b, _ := json.Marshal(m)
				if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", m.ID, b); err != nil {
					return false
				}
			}
			flusher.Flush()
			if len(msgs) < 500 {
				return true
			}
		}
	}
	if !catchUp() {
		return
	}
	// Heartbeat keeps proxies and the browser from timing out an idle stream.
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
			// The heartbeat doubles as a safety net for a live signal lost to a full buffer.
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			if !catchUp() {
				return
			}
		case m := <-live:
			if channel != "" && m.Channel != channel {
				continue
			}
			if !catchUp() {
				return
			}
		}
	}
}

func (s *Server) getAgents(w http.ResponseWriter, r *http.Request, p principal) {
	agents, err := s.bus.Store.Agents(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []store.Agent{}
	for _, a := range agents {
		if p.agent == nil || a.Channel == p.agent.Channel {
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getChannels(w http.ResponseWriter, r *http.Request, p principal) {
	chs, err := s.bus.Store.Channels(r.Context())
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []store.Channel{}
	for _, c := range chs {
		if p.agent == nil || c.Name == p.agent.Channel {
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request, p principal) {
	out := []driver.AgentStatus{}
	if s.status != nil {
		st, err := s.status(r.Context())
		if err != nil {
			httpError(w, http.StatusInternalServerError, err.Error())
			return
		}
		for _, a := range st {
			if p.agent == nil || a.Channel == p.agent.Channel {
				out = append(out, a)
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// Serve runs srv until ctx ends, then drains it.
func Serve(ctx context.Context, srv *http.Server, log *zap.Logger) error {
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// SSE streams never finish on their own and browsers reconnect by themselves, so the drain
	// is short: it only has to let in-flight POSTs complete.
	sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return srv.Close()
	}
	return nil
}
