package driver

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"airc/pkg/bus"
	"airc/pkg/provider"
	"airc/pkg/store"
)

// fake replies with a scripted function and records turns.
type fake struct {
	name  string
	mu    sync.Mutex
	turns []provider.Turn
	ctx   context.Context
	reply func(t provider.Turn) (provider.Result, error)
}

func (f *fake) Name() string { return f.name }
func (f *fake) Run(ctx context.Context, t provider.Turn) (provider.Result, error) {
	f.mu.Lock()
	f.turns = append(f.turns, t)
	f.ctx = ctx
	f.mu.Unlock()
	if f.reply == nil {
		return provider.Result{SessionID: "s-" + f.name, Text: "ok [done]"}, nil
	}
	return f.reply(t)
}
func (f *fake) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.turns) }
func (f *fake) last() provider.Turn {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.turns[len(f.turns)-1]
}

type env struct {
	b    *bus.Bus
	d    *Driver
	sub  <-chan store.Message
	seen []store.Message
	t    *testing.T
}

func newEnv(t *testing.T, cfg Config, provs ...provider.Provider) *env {
	t.Helper()
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b := bus.New(s)
	sub, cancel := b.Subscribe(256)
	t.Cleanup(cancel)
	d := New(b, provs, cfg, zap.NewNop())
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go d.Run(ctx)
	return &env{b: b, d: d, sub: sub, t: t}
}

func (e *env) post(channel, from, to, body string) *store.Message {
	m := &store.Message{Channel: channel, From: from, To: to, Body: body}
	if err := e.b.Post(context.Background(), m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

// wait blocks until a message satisfying pred arrives (or fails after 3s).
func (e *env) wait(pred func(store.Message) bool) store.Message {
	e.t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case m := <-e.sub:
			e.seen = append(e.seen, m)
			if pred(m) {
				return m
			}
		case <-deadline:
			var b strings.Builder
			for _, m := range e.seen {
				fmt.Fprintf(&b, "  %d %s->%s: %s\n", m.ID, m.From, m.To, m.Body)
			}
			e.t.Fatalf("timeout waiting; seen:\n%s", b.String())
		}
	}
}

func (e *env) spawn(nick, prov, channel string, flags ...string) {
	e.t.Helper()
	e.post(channel, "human", "", "!spawn "+nick+" "+prov+" "+channel+" "+e.t.TempDir()+" "+strings.Join(flags, " "))
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "@"+nick) })
}

func from(nick string) func(store.Message) bool {
	return func(m store.Message) bool { return m.From == nick }
}

func TestWakeOnAddressAndReply(t *testing.T) {
	f := &fake{name: "claude", reply: func(t provider.Turn) (provider.Result, error) {
		return provider.Result{SessionID: "sid-1", Text: "@codex-review please look [ask]", Denials: []string{"Bash rm"}}, nil
	}}
	e := newEnv(t, Config{BaseURL: "http://x"}, f)
	e.spawn("claude-impl", "claude", "task-1", "--sandbox rw", "--tools Bash(git:*)")
	e.post("task-1", "human", "", "hello everyone")             // not addressed: no wake
	e.post("task-1", "human", "claude-impl", "@claude-impl go") // addressed
	reply := e.wait(from("claude-impl"))
	if reply.To != "codex-review" || reply.Tags[0] != "ask" {
		t.Fatalf("reply %+v", reply)
	}
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "denied") })
	if f.count() != 1 {
		t.Fatalf("turns: %d", f.count())
	}
	turn := f.last()
	if turn.SessionID != "" || turn.Sandbox != "rw" || turn.Tools[0] != "Bash(git:*)" ||
		!strings.Contains(turn.Prompt, "hello everyone") || !strings.Contains(turn.Prompt, "@claude-impl go") ||
		!strings.Contains(turn.Protocol, `agent "claude-impl"`) {
		t.Fatalf("turn %+v", turn)
	}
	var url, tok string
	for _, kv := range turn.Env {
		if strings.HasPrefix(kv, "AIRC_URL=") {
			url = kv
		}
		if strings.HasPrefix(kv, "AIRC_TOKEN=") {
			tok = kv
		}
	}
	if url != "AIRC_URL=http://x" || len(tok) < 20 {
		t.Fatalf("env %v", turn.Env)
	}
	ag, _ := e.b.Store.Agent(context.Background(), "claude-impl")
	if ag.SessionID != "sid-1" || ag.Turns != 1 || ag.LastMsgID == 0 {
		t.Fatalf("agent %+v", ag)
	}
	// Second turn resumes and only sees the delta.
	e.post("task-1", "human", "", "@claude-impl again")
	e.wait(from("claude-impl"))
	turn = f.last()
	if turn.SessionID != "sid-1" || strings.Contains(turn.Prompt, "hello everyone") || !strings.Contains(turn.Prompt, "again") {
		t.Fatalf("second turn %+v", turn)
	}
}

func TestBotCapAndRelease(t *testing.T) {
	// Two agents ping-pong forever unless capped.
	a := &fake{name: "claude", reply: func(provider.Turn) (provider.Result, error) {
		return provider.Result{SessionID: "a", Text: "@codex-review your turn [ask]"}, nil
	}}
	b := &fake{name: "codex", reply: func(provider.Turn) (provider.Result, error) {
		return provider.Result{SessionID: "b", Text: "@claude-impl back to you [ask]"}, nil
	}}
	e := newEnv(t, Config{BotCap: 3}, a, b)
	e.spawn("claude-impl", "claude", "task-1")
	e.spawn("codex-review", "codex", "task-1")
	e.post("task-1", "human", "", "@claude-impl start")
	paused := e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "consecutive") })
	if paused.Tags[0] != "waiting for human" {
		t.Fatalf("pause tags %v", paused.Tags)
	}
	total := a.count() + b.count()
	if total != 3 {
		t.Fatalf("turns before pause: %d", total)
	}
	// Release 2 turns: the dropped wake is replayed, exactly 2 turns run, then paused again.
	e.post("task-1", "human", "", "!release 2")
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "resuming 1 paused") })
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "consecutive") })
	if got := a.count() + b.count(); got != 3+2 {
		t.Fatalf("turns after release: %d", got)
	}
	// A human message resets the streak: cap turns again, plus nothing (grant is spent).
	e.post("task-1", "human", "", "@claude-impl continue")
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "consecutive") })
	if got := a.count() + b.count(); got != 5+3 {
		t.Fatalf("turns after human message: %d", got)
	}
	// Lock: even a human address does not wake.
	e.post("task-1", "human", "", "!lock")
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "locked") })
	e.post("task-1", "human", "", "@claude-impl anyone?")
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "is locked") })
	if got := a.count() + b.count(); got != 8 {
		t.Fatalf("turn ran while locked: %d", got)
	}
}

func TestSessionLostRetriesWithFullLog(t *testing.T) {
	calls := 0
	f := &fake{name: "claude", reply: func(t provider.Turn) (provider.Result, error) {
		calls++
		if t.SessionID != "" {
			return provider.Result{}, fmt.Errorf("%w: gone", provider.ErrSessionLost)
		}
		return provider.Result{SessionID: "new-" + fmt.Sprint(calls), Text: "recovered [done]"}, nil
	}}
	e := newEnv(t, Config{}, f)
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "human", "", "@claude-impl first")
	e.wait(from("claude-impl"))
	e.post("task-1", "human", "", "@claude-impl second")
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "session was lost") })
	e.wait(from("claude-impl"))
	turn := f.last()
	if turn.SessionID != "" || !strings.Contains(turn.Prompt, "Full log") || !strings.Contains(turn.Prompt, "first") {
		t.Fatalf("retry turn %+v", turn)
	}
	ag, _ := e.b.Store.Agent(context.Background(), "claude-impl")
	if ag.SessionID != "new-3" {
		t.Fatalf("session after recovery: %s", ag.SessionID)
	}
}

func TestTimeoutThenReleaseRetries(t *testing.T) {
	var calls atomic.Int32
	f := &fake{name: "claude"}
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		if calls.Add(1) == 1 {
			<-f.ctx.Done()
			return provider.Result{}, f.ctx.Err()
		}
		return provider.Result{SessionID: "s", Text: "second time lucky [done]"}, nil
	}
	e := newEnv(t, Config{TurnTimeout: 50 * time.Millisecond}, f)
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "human", "", "@claude-impl slow")
	m := e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "turn failed") })
	if m.Tags[0] != "blocked" || !strings.Contains(m.Body, "deadline") {
		t.Fatalf("%+v", m)
	}
	ag, _ := e.b.Store.Agent(context.Background(), "claude-impl")
	if ag.Turns != 0 || ag.LastMsgID != 0 {
		t.Fatalf("failed turn must be rolled back: %+v", ag)
	}
	e.post("task-1", "human", "", "!release")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "resuming 1 paused") })
	reply := e.wait(from("claude-impl"))
	if reply.Body != "second time lucky [done]" || !strings.Contains(f.last().Prompt, "slow") {
		t.Fatalf("retry: %+v prompt=%q", reply, f.last().Prompt)
	}
}

func TestCrossChannelWakeRefused(t *testing.T) {
	f := &fake{name: "claude"}
	e := newEnv(t, Config{}, f)
	e.spawn("claude-impl", "claude", "task-1")
	e.post("lobby", "human", "", "@claude-impl hello from lobby")
	m := e.wait(from("airc"))
	if m.Channel != "lobby" || !strings.Contains(m.Body, "lives in task-1") || f.count() != 0 {
		t.Fatalf("%+v turns=%d", m, f.count())
	}
}

func TestUnlockReplaysAndToolsValidation(t *testing.T) {
	f := &fake{name: "claude"}
	e := newEnv(t, Config{}, f)
	e.post("task-1", "human", "", "!spawn claude-impl claude task-1 "+t.TempDir()+" --tools=--dangerously-skip-permissions")
	if m := e.wait(from("airc")); !strings.Contains(m.Body, "invalid tool pattern") {
		t.Fatalf("%s", m.Body)
	}
	e.spawn("claude-impl", "claude", "task-1", "--tools", "Bash(curl:*)")
	e.post("task-1", "human", "", "!lock")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "channel locked") })
	e.post("task-1", "human", "", "@claude-impl go")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "is locked") })
	e.post("task-1", "human", "", "!unlock")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "unlocked, resuming 1") })
	e.wait(from("claude-impl"))
	if f.count() != 1 || !strings.Contains(f.last().Protocol, "curl -s") {
		t.Fatalf("turns=%d protocol=%q", f.count(), f.last().Protocol)
	}
	// Re-spawn keeps the session, without curl the protocol drops the say line.
	e.spawn("claude-impl", "claude", "task-1")
	ag, _ := e.b.Store.Agent(context.Background(), "claude-impl")
	if ag.SessionID != "s-claude" || ag.Turns != 1 {
		t.Fatalf("respawn must keep session: %+v", ag)
	}
	e.post("task-1", "human", "", "@claude-impl again")
	e.wait(from("claude-impl"))
	if strings.Contains(f.last().Protocol, "curl -s") {
		t.Fatal("say line must be absent without a curl tool")
	}
}

func TestCommandsValidation(t *testing.T) {
	e := newEnv(t, Config{}, &fake{name: "claude"})
	cases := []struct{ cmd, want string }{
		{"!spawn x claude task-1", "usage"},
		{"!spawn airc claude task-1 /tmp", "invalid nick"},
		{"!spawn a gpt task-1 /tmp", "unknown provider"},
		{"!spawn a claude task-1 /nonexistent-dir-xyz", "not a directory"},
		{"!spawn a claude task-1 /tmp --sandbox yes", "sandbox must be"},
		{"!stop nobody", "not found"},
		{"!kill nobody", "no running turn"},
		{"!release zero", "usage"},
		{"!nope", "unknown command"},
		{"!status", "no agents"},
		{"!help", "Commands"},
	}
	for _, c := range cases {
		e.post("lobby", "human", "", c.cmd)
		m := e.wait(from("airc"))
		if !strings.Contains(m.Body, c.want) {
			t.Errorf("%s: got %q, want %q", c.cmd, m.Body, c.want)
		}
	}
	// Agents may not run commands.
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "claude-impl", "", "!status")
	e.post("task-1", "human", "", "!status")
	m := e.wait(from("airc"))
	if !strings.Contains(m.Body, "| claude-impl |") {
		t.Fatalf("status: %s", m.Body)
	}
	// Stopped agent is not woken.
	e.post("task-1", "human", "", "!stop claude-impl")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "stopped") })
	e.post("task-1", "human", "", "@claude-impl hi")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "is stopped") })
}

func TestRescanReplaysPendingWake(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	ag := &store.Agent{Nick: "claude-impl", Provider: "claude", Channel: "task-1", Cwd: t.TempDir(), Sandbox: "ro", Token: "tk", Enabled: true}
	if err := s.PutAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(ctx, &store.Message{Channel: "task-1", From: "human", To: "claude-impl", Body: "@claude-impl pending"}); err != nil {
		t.Fatal(err)
	}
	// A system notice addressing the agent is not a wake.
	if err := s.Insert(ctx, &store.Message{Channel: "task-1", From: "airc", To: "claude-impl", Body: "@claude-impl had 1 tool call denied"}); err != nil {
		t.Fatal(err)
	}
	b := bus.New(s)
	sub, cancel := b.Subscribe(16)
	defer cancel()
	f := &fake{name: "claude"}
	d := New(b, []provider.Provider{f}, Config{}, zap.NewNop())
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go d.Run(rctx)
	select {
	case m := <-sub:
		if m.From != "claude-impl" || f.count() != 1 {
			t.Fatalf("replayed %+v turns=%d", m, f.count())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending wake not replayed")
	}
	select {
	case m := <-sub:
		t.Fatalf("system notice woke the agent: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestShutdownRollsBackRunningTurn(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	_ = s.PutAgent(ctx, &store.Agent{Nick: "claude-impl", Provider: "claude", Channel: "task-1", Cwd: t.TempDir(), Sandbox: "ro", Token: "tk", Enabled: true})
	b := bus.New(s)
	f := &fake{name: "claude"}
	started := make(chan struct{})
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		close(started)
		<-f.ctx.Done()
		return provider.Result{}, f.ctx.Err()
	}
	d := New(b, []provider.Provider{f}, Config{}, zap.NewNop())
	rctx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- d.Run(rctx) }()
	_ = b.Post(ctx, &store.Message{Channel: "task-1", From: "human", Body: "@claude-impl long task"})
	<-started
	stop()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not wait for its worker")
	}
	ag, _ := s.Agent(ctx, "claude-impl")
	if ag.LastMsgID != 0 || ag.Turns != 0 {
		t.Fatalf("interrupted turn must be rolled back for rescan: %+v", ag)
	}
}
