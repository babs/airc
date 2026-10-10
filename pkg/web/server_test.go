package web

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/airc/pkg/bus"
	"github.com/babs/airc/pkg/driver"
	"github.com/babs/airc/pkg/store"
)

func newSrv(t *testing.T) (*httptest.Server, *bus.Bus) {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b := bus.New(s)
	status := func(context.Context) ([]driver.AgentStatus, error) {
		return []driver.AgentStatus{{Nick: "claude-impl", Channel: "task-1", State: "running"}, {Nick: "far-away", Channel: "task-2", State: "idle"}}, nil
	}
	ts := httptest.NewServer(New(b, "admin", zap.NewNop(), status, "boot-1").Handler())
	t.Cleanup(ts.Close)
	return ts, b
}

func do(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), method, ts.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// io.ReadAll, not a Scanner: a 1000-row JSON page exceeds the scanner's 64 KiB token limit.
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, strings.ReplaceAll(string(data), "\n", "")
}

func TestPostMsgAuthAndValidation(t *testing.T) {
	ts, b := newSrv(t)
	_ = b.Store.PutAgent(context.Background(), &store.Agent{Nick: "claude-impl", Provider: "claude", Channel: "task-1", Cwd: "/", Sandbox: "ro", Token: "agent-tok", Enabled: true})
	_ = b.Store.PutAgent(context.Background(), &store.Agent{Nick: "far-away", Provider: "codex", Channel: "task-2", Cwd: "/secret", Sandbox: "ro", Token: "far-tok", Enabled: true})
	_ = b.Store.PutAgent(context.Background(), &store.Agent{Nick: "stopped", Provider: "codex", Channel: "task-1", Cwd: "/", Sandbox: "ro", Token: "stopped-tok", Enabled: false})
	_ = b.Post(context.Background(), &store.Message{Channel: "task-2", From: "h", Body: "confidential topic"})
	cases := []struct {
		name, token, body string
		code              int
		want              string
	}{
		{"no token", "", `{"channel":"c","from":"h","body":"x"}`, 401, "missing token"},
		{"bad token", "nope", `{"channel":"c","from":"h","body":"x"}`, 401, "bad token"},
		{"bad json", "admin", `{`, 400, "bad json"},
		{"empty body", "admin", `{"channel":"c","from":"h","body":"  "}`, 400, "empty body"},
		{"bad from", "admin", `{"channel":"c","from":"Bad!","body":"x"}`, 400, "invalid from"},
		{"system nick", "admin", `{"channel":"c","from":"airc","body":"x"}`, 400, "invalid from"},
		{"bad channel", "admin", `{"channel":"#c","from":"h","body":"x"}`, 400, "invalid channel"},
		{"bad to", "admin", `{"channel":"c","from":"h","to":"X","body":"x"}`, 400, "invalid to"},
		{"admin ok", "admin", `{"channel":"c","from":"h","body":"@claude-impl hi [ask]"}`, 201, `"to":"claude-impl"`},
		{"agent pinned", "agent-tok", `{"channel":"other","from":"human","body":"note"}`, 201, `"channel":"task-1","from":"claude-impl"`},
		{"agent cross-channel to", "agent-tok", `{"body":"@far-away wake up"}`, 403, "use @task-2:far-away"},
		{"agent relayed to", "agent-tok", `{"body":"@task-2:far-away wake up"}`, 201, `"to":"task-2:far-away"`},
		{"bad relay form", "admin", `{"channel":"c","from":"h","to":"a:b:c","body":"x"}`, 400, "invalid to"},
		{"stopped agent token", "stopped-tok", `{"body":"x"}`, 401, "agent stopped"},
	}
	for _, c := range cases {
		code, body := do(t, ts, "POST", "/msg", c.token, c.body)
		if code != c.code || !strings.Contains(body, c.want) {
			t.Errorf("%s: got %d %s, want %d %q", c.name, code, body, c.code, c.want)
		}
	}
	code, body := do(t, ts, "GET", "/msgs?channel=task-1", "agent-tok", "")
	if code != 200 || strings.Contains(body, `"channel":"c"`) || !strings.Contains(body, `"body":"note"`) {
		t.Fatalf("agent msgs: %d %s", code, body)
	}
	code, body = do(t, ts, "GET", "/agents", "admin", "")
	if code != 200 || strings.Contains(body, "agent-tok") || !strings.Contains(body, `"nick":"claude-impl"`) {
		t.Fatalf("agents must not leak tokens: %d %s", code, body)
	}
	code, body = do(t, ts, "GET", "/agents", "agent-tok", "")
	if code != 200 || strings.Contains(body, "far-away") || !strings.Contains(body, `"nick":"claude-impl"`) {
		t.Fatalf("agent must only see its channel: %d %s", code, body)
	}
	// /msgs is paged: a huge limit is clamped to one page of 1000.
	for i := 0; i < 1005; i++ {
		_ = b.Post(context.Background(), &store.Message{Channel: "paged", From: "h", Body: "x"})
	}
	code, body = do(t, ts, "GET", "/msgs?channel=paged&limit=99999999", "admin", "")
	if code != 200 || strings.Count(body, `"channel":"paged"`) != 1000 {
		t.Fatalf("limit clamp: %d, %d rows", code, strings.Count(body, `"channel":"paged"`))
	}
	code, body = do(t, ts, "GET", "/status", "agent-tok", "")
	if code != 200 || strings.Contains(body, "far-away") || !strings.Contains(body, `"state":"running"`) {
		t.Fatalf("status filtered per agent: %d %s", code, body)
	}
	code, body = do(t, ts, "GET", "/channels", "agent-tok", "")
	if code != 200 || strings.Contains(body, `"name":"task-2"`) || strings.Contains(body, "confidential") {
		t.Fatalf("agent must not see other channels: %d %s", code, body)
	}
	code, body = do(t, ts, "GET", "/channels", "admin", "")
	if code != 200 || !strings.Contains(body, `"name":"task-1"`) {
		t.Fatalf("channels: %d %s", code, body)
	}
	if code, _ := do(t, ts, "GET", "/healthz", "", ""); code != 200 {
		t.Fatal("healthz needs no auth")
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/healthz", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Airc-Boot") != "boot-1" {
		t.Fatalf("boot header: %q", resp.Header.Get("X-Airc-Boot"))
	}
	if code, body := do(t, ts, "GET", "/", "", ""); code != 200 || !strings.Contains(body, "<title>airc") {
		t.Fatalf("ui: %d", code)
	}
}

func TestEventsReplayThenLive(t *testing.T) {
	ts, b := newSrv(t)
	ctx := context.Background()
	_ = b.Post(ctx, &store.Message{Channel: "c", From: "h", Body: "old"})
	_ = b.Post(ctx, &store.Message{Channel: "other", From: "h", Body: "elsewhere"})

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, ts.URL+"/events?channel=c&since=0&token=admin", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %s", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	next := func() store.Message {
		t.Helper()
		done := make(chan store.Message, 1)
		go func() {
			for sc.Scan() {
				if line := sc.Text(); strings.HasPrefix(line, "data: ") {
					var m store.Message
					_ = json.Unmarshal([]byte(line[6:]), &m)
					done <- m
					return
				}
			}
		}()
		select {
		case m := <-done:
			return m
		case <-time.After(3 * time.Second):
			t.Fatal("no event")
		}
		return store.Message{}
	}
	if m := next(); m.Body != "old" {
		t.Fatalf("replay: %+v", m)
	}
	_ = b.Post(ctx, &store.Message{Channel: "other", From: "h", Body: "filtered"})
	_ = b.Post(ctx, &store.Message{Channel: "c", From: "h", Body: "live"})
	if m := next(); m.Body != "live" {
		t.Fatalf("live: %+v", m)
	}
}
