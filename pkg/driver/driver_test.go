package driver

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/babs/airc/pkg/bus"
	"github.com/babs/airc/pkg/provider"
	"github.com/babs/airc/pkg/store"
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
			e.t.Fatalf("timeout waiting; seen:\n%s", e.dump())
		}
	}
}

func (e *env) dump() string {
	var b strings.Builder
	for _, m := range e.seen {
		fmt.Fprintf(&b, "  %d %s->%s: %s\n", m.ID, m.From, m.To, m.Body)
	}
	return b.String()
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
		return provider.Result{SessionID: "sid-1", Text: "@codex-review please look [status: waiting for review] [ask]", Denials: []string{"Bash rm"}}, nil
	}}
	e := newEnv(t, Config{BaseURL: "http://x"}, f)
	e.spawn("claude-impl", "claude", "task-1", "--sandbox rw", "--tools Bash(git:*)", "--home "+t.TempDir(), "--model claude-opus-5")
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
	if turn.SessionID != "" || turn.Sandbox != "rw" || turn.Model != "claude-opus-5" || turn.Tools[0] != "Bash(git:*)" ||
		!strings.Contains(turn.Prompt, "hello everyone") || !strings.Contains(turn.Prompt, "@claude-impl go") ||
		!strings.Contains(turn.Protocol, `agent "claude-impl"`) {
		t.Fatalf("turn %+v", turn)
	}
	var url, tok, home string
	for _, kv := range turn.Env {
		if strings.HasPrefix(kv, "AIRC_URL=") {
			url = kv
		}
		if strings.HasPrefix(kv, "AIRC_TOKEN=") {
			tok = kv
		}
		if strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") {
			home = kv
		}
	}
	if url != "AIRC_URL=http://x" || len(tok) < 20 || len(home) <= len("CLAUDE_CONFIG_DIR=") {
		t.Fatalf("env %v", turn.Env)
	}
	// The declared status is written by the driver loop after the reply is posted: poll briefly.
	var ag *store.Agent
	for i := 0; i < 50; i++ {
		ag, _ = e.b.Store.Agent(context.Background(), "claude-impl")
		if ag.Status != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ag.SessionID != "sid-1" || ag.Turns != 1 || ag.LastMsgID == 0 || ag.Status != "waiting for review" {
		t.Fatalf("agent %+v", ag)
	}
	if st, _ := e.d.Status(context.Background()); st[0].Activity != "waiting for review" {
		t.Fatalf("status activity: %+v", st)
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
		return provider.Result{SessionID: "b", Text: "@claude-impl back to you [status: ping-pong] [ask]"}, nil
	}}
	e := newEnv(t, Config{BotCap: 3}, a, b)
	e.spawn("claude-impl", "claude", "task-1")
	e.spawn("codex-review", "codex", "task-1", "--sandbox", "ro")
	e.post("task-1", "human", "", "@claude-impl start")
	paused := e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "consecutive") })
	if paused.Tags[0] != "waiting for human" {
		t.Fatalf("pause tags %v", paused.Tags)
	}
	// The third turn was codex's second: its protocol must describe claude with its last message.
	if p := a.last().Protocol; !strings.Contains(p, `doing: "ping-pong"`) {
		t.Fatalf("declared activity missing in peer topology:\n%s", p)
	}
	proto := b.last().Protocol
	if !strings.Contains(proto, "@claude-impl (claude, rw): idle, 1 turn(s)") || !strings.Contains(proto, `last: "@codex-review your turn [ask]"`) || !strings.Contains(proto, "humans seen here: human") {
		t.Fatalf("topology missing in protocol:\n%s", proto)
	}
	total := a.count() + b.count()
	if total != 3 {
		t.Fatalf("turns before pause: %d\n%s", total, e.dump())
	}
	// An agent spawned elsewhere shows up in the next prompt's topology, as @channel:nick.
	e.spawn("claude-else", "claude", "task-2")
	// Release 2 turns: the dropped wake is replayed, exactly 2 turns run, then paused again.
	e.post("task-1", "human", "", "!release 2")
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "resuming 1 paused") })
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "consecutive") })
	if got := a.count() + b.count(); got != 3+2 {
		t.Fatalf("turns after release: %d", got)
	}
	if p := b.last().Protocol; !strings.Contains(p, "@task-2:claude-else (claude, idle)") {
		t.Fatalf("other channels missing from topology:\n%s", p)
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

func TestStatusReportsRunningTurn(t *testing.T) {
	f := &fake{name: "claude"}
	started := make(chan struct{})
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		close(started)
		<-f.ctx.Done()
		return provider.Result{}, f.ctx.Err()
	}
	e := newEnv(t, Config{}, f)
	e.spawn("claude-impl", "claude", "task-1")
	trig := e.post("task-1", "human", "", "@claude-impl long")
	<-started
	st, err := e.d.Status(context.Background())
	if err != nil || len(st) != 1 || st[0].State != "running" || st[0].Trigger != trig.ID || st[0].Since.IsZero() {
		t.Fatalf("%v %+v", err, st)
	}
	if e.d.Idle() {
		t.Fatal("Idle must be false while a turn runs")
	}
	e.post("task-1", "human", "", "!status")
	m := e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "| claude-impl |") })
	if !strings.Contains(m.Body, "running") || !strings.Contains(m.Body, fmt.Sprintf("on #%d", trig.ID)) {
		t.Fatalf("status: %s", m.Body)
	}
	e.post("task-1", "human", "", "!kill claude-impl")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "turn failed") })
	st, _ = e.d.Status(context.Background())
	if st[0].State != "idle" || !e.d.Idle() {
		t.Fatalf("after kill: %+v idle=%v", st, e.d.Idle())
	}
}

func TestQueueFullParksWake(t *testing.T) {
	f := &fake{name: "claude"}
	release := make(chan struct{})
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		<-release
		return provider.Result{SessionID: "s", Text: "ok [done]"}, nil
	}
	e := newEnv(t, Config{BotCap: 1000}, f)
	e.spawn("claude-impl", "claude", "task-1")
	for i := 0; i < 66; i++ { // 1 running + 64 queued + 1 overflow
		e.post("task-1", "human", "", fmt.Sprintf("@claude-impl msg %d", i))
	}
	m := e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "parked") })
	if !strings.Contains(m.Body, "message 68") { // ids: 1 spawn, 2 its reply, 3..68 the posts; 68 overflows
		t.Fatalf("%s", m.Body)
	}
	close(release)
	e.post("task-1", "human", "", "!release")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "resuming 1 paused") })
}

func TestOneMessageWakesEveryAddressee(t *testing.T) {
	a := &fake{name: "claude"}
	b := &fake{name: "codex"}
	e := newEnv(t, Config{}, a, b)
	e.spawn("claude-impl", "claude", "task-1")
	e.spawn("codex-review", "codex", "task-1")
	// The two agents reply in any order; wait consumes, so collect both rather than sequence them.
	both := func() {
		t.Helper()
		seen := map[string]bool{}
		for len(seen) < 2 {
			seen[e.wait(func(m store.Message) bool { return m.From == "claude-impl" || m.From == "codex-review" }).From] = true
		}
	}
	e.post("task-1", "human", "", "@claude-impl implement A\n@codex-review review B when done")
	both()
	if a.count() != 1 || b.count() != 1 {
		t.Fatalf("turns claude=%d codex=%d", a.count(), b.count())
	}
	// Same line, comma separated, still both.
	e.post("task-1", "human", "", "@claude-impl, @codex-review both: status?")
	both()
	if a.count() != 2 || b.count() != 2 {
		t.Fatalf("turns claude=%d codex=%d", a.count(), b.count())
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
	// IRC style @channel:nick is relayed into the agent's channel and wakes it there.
	e.post("lobby", "human", "", "@task-1:claude-impl hello via relay")
	relayed := e.wait(func(m store.Message) bool { return m.Channel == "task-1" && m.From == "human" })
	if relayed.To != "claude-impl" || !strings.Contains(relayed.Body, "(relayed from lobby)") || !strings.Contains(relayed.Body, "@claude-impl hello via relay") {
		t.Fatalf("relayed %+v", relayed)
	}
	e.wait(func(m store.Message) bool {
		return m.Channel == "lobby" && strings.Contains(m.Body, "relayed to #task-1")
	})
	reply := e.wait(from("claude-impl"))
	if reply.Channel != "task-1" || !strings.Contains(f.last().Prompt, "hello via relay") {
		t.Fatalf("reply %+v", reply)
	}
	e.post("lobby", "human", "", "@task-9:nobody hi")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "no agent @nobody in task-9") })
	if !strings.Contains(f.last().Protocol, "the `lobby` channel") {
		t.Fatalf("lobby missing from protocol:\n%s", f.last().Protocol)
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
	if f.count() != 1 || !strings.Contains(f.last().Protocol, "curl -s") || !strings.Contains(f.last().Protocol, "delegate") {
		t.Fatalf("turns=%d protocol=%q", f.count(), f.last().Protocol)
	}
	// Re-spawn keeps the session, without curl the protocol drops the say line.
	e.spawn("claude-impl", "claude", "task-1")
	ag, _ := e.b.Store.Agent(context.Background(), "claude-impl")
	if ag.SessionID != "s-claude" || ag.Turns != 1 {
		t.Fatalf("respawn must keep session: %+v", ag)
	}
	// --session attaches an existing CLI session; --tools none drops the curl tool and its say line.
	e.spawn("claude-impl", "claude", "task-1", "--session", "external-1234", "--tools", "none")
	ag, _ = e.b.Store.Agent(context.Background(), "claude-impl")
	if ag.SessionID != "external-1234" {
		t.Fatalf("--session: %+v", ag)
	}
	e.post("task-1", "human", "", "@claude-impl again")
	e.wait(from("claude-impl"))
	if strings.Contains(f.last().Protocol, "curl -s") {
		t.Fatal("say line must be absent without a curl tool")
	}
}

func TestCommandsValidation(t *testing.T) {
	f0 := &fake{name: "claude"}
	e := newEnv(t, Config{}, f0)
	cases := []struct{ cmd, want string }{
		{"!spawn", "usage"},
		{"!spawn claude-x", "no workspace set for lobby"},
		{"!spawn claude-x task-1 ~/nonexistent-dir-xyz", "/nonexistent-dir-xyz\" is not a directory"},
		{"!spawn gpt-x task-1 /tmp", "unknown provider"},
		{"!cwd", "no workspace set"},
		{"!cwd /nonexistent-dir-xyz", "not a directory"},
		{"!cwd /tmp", "workspace of lobby set to /tmp"},
		{"!cwd", "workspace of lobby: /tmp"},
		{"!topic", "no explicit topic"},
		{"!topic Ship the thing", "topic of lobby set to: Ship the thing"},
		{"!topic", "topic of lobby: Ship the thing"},
		{"!rename Bad Name", "usage"},
		{"!open", "usage"},
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
	// Rename moves everything; the reply lands in the new channel. Close hides and stops; open restores.
	e.post("scratch", "human", "", "hello scratch")
	e.post("scratch", "human", "", "!rename scratch-2")
	m := e.wait(func(m store.Message) bool {
		return m.Channel == "scratch-2" && strings.Contains(m.Body, "renamed to scratch-2")
	})
	if old, _ := e.b.Store.Messages(context.Background(), "scratch", 0, 0, 0); len(old) != 0 {
		t.Fatalf("old channel still has %d messages", len(old))
	}
	e.post("scratch-2", "human", "", "!close")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "closed: hidden") })
	if chs, _ := e.b.Store.Channels(context.Background()); slices.ContainsFunc(chs, func(c store.Channel) bool { return c.Name == "scratch-2" }) {
		t.Fatal("closed channel still listed")
	}
	e.post("lobby", "human", "", "!open scratch-2")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "reopened") })
	if chs, _ := e.b.Store.Channels(context.Background()); !slices.ContainsFunc(chs, func(c store.Channel) bool { return c.Name == "scratch-2" }) {
		t.Fatal("reopened channel not listed")
	}
	_ = m
	// With a channel workspace, nick alone is enough; provider and channel come from context.
	e.post("lobby", "human", "", "!spawn claude-alone")
	m = e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "@claude-alone") })
	if !strings.Contains(m.Body, "spawned @claude-alone (claude, rw, cwd /tmp) in lobby") {
		t.Fatalf("spawn defaults: %s", m.Body)
	}
	// Re-spawning an existing agent changes only what is given: sandbox flips, tools and model stay.
	e.post("lobby", "human", "", "!spawn claude-alone --tools Bash(git:*) --model m1")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "updated @claude-alone") })
	e.post("lobby", "human", "", "!spawn claude-alone --sandbox ro")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "updated @claude-alone (claude, ro") })
	if ag, _ := e.b.Store.Agent(context.Background(), "claude-alone"); ag.Sandbox != "ro" || ag.Model != "m1" || len(ag.Tools) != 1 || ag.Cwd != "/tmp" {
		t.Fatalf("respawn must merge, got %+v", ag)
	}
	e.post("lobby", "human", "", "!spawn claude-alone --tools none")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "updated @claude-alone") })
	if ag, _ := e.b.Store.Agent(context.Background(), "claude-alone"); len(ag.Tools) != 0 || ag.Model != "m1" {
		t.Fatalf("--tools none must clear tools only, got %+v", ag)
	}
	e.post("lobby", "human", "", "@claude-alone hi")
	e.wait(from("claude-alone"))
	if !strings.Contains(f0.last().Protocol, "channel workspace: /tmp") {
		t.Fatalf("workspace missing from topology:\n%s", f0.last().Protocol)
	}
	// Agents may not run commands.
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "claude-impl", "", "!status")
	e.post("task-1", "human", "", "!status")
	m = e.wait(from("airc"))
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

func TestDeltaKeepsNewestAndRescanPaginates(t *testing.T) {
	f := &fake{name: "claude"}
	e := newEnv(t, Config{ContextLimit: 5}, f)
	e.spawn("claude-impl", "claude", "task-1")
	for i := 0; i < 20; i++ {
		e.post("task-1", "human", "", fmt.Sprintf("filler %d", i))
	}
	e.post("task-1", "human", "", "@claude-impl the trigger")
	e.wait(from("claude-impl"))
	p := f.last().Prompt
	if !strings.Contains(p, "the trigger") || strings.Contains(p, "filler 0\n") || !strings.Contains(p, "filler 19") {
		t.Fatalf("delta must keep the newest rows:\n%s", p)
	}
	// Rescan across more than one page: 1200 rows, the wake is the last one.
	s, err := store.Open(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	_ = s.PutAgent(ctx, &store.Agent{Nick: "a", Provider: "claude", Channel: "c", Cwd: t.TempDir(), Sandbox: "ro", Token: "t", Enabled: true})
	for i := 0; i < 1200; i++ {
		_ = s.Insert(ctx, &store.Message{Channel: "c", From: "human", Body: "x"})
	}
	_ = s.Insert(ctx, &store.Message{Channel: "c", From: "human", To: "a", Body: "@a late"})
	b := bus.New(s)
	sub, cancel := b.Subscribe(4)
	defer cancel()
	g := &fake{name: "claude"}
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go New(b, []provider.Provider{g}, Config{}, zap.NewNop()).Run(rctx)
	select {
	case m := <-sub:
		if m.From != "a" {
			t.Fatalf("%+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wake beyond the first page not replayed")
	}
}

func TestCrashRecoveryParksInflightTurn(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	_ = s.PutAgent(ctx, &store.Agent{Nick: "a", Provider: "claude", Channel: "c", Cwd: t.TempDir(), Sandbox: "ro", Token: "t", Enabled: true})
	_ = s.Insert(ctx, &store.Message{Channel: "c", From: "human", To: "a", Body: "@a work"})
	_ = s.Insert(ctx, &store.Message{Channel: "c", From: "human", To: "a", Body: "@a more work"})
	// A crash after the claim, maybe after the provider ran: cursor advanced, in-flight state
	// persisted, no reply stored. Recovery must not rerun it on its own, nor let a later wake
	// (persisted or live) run past it.
	if ok, _ := s.ClaimTurn(ctx, "a", "c", 0, 1); !ok {
		t.Fatal("claim")
	}
	b := bus.New(s)
	sub, cancel := b.Subscribe(8)
	defer cancel()
	f := &fake{name: "claude"}
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go New(b, []provider.Provider{f}, Config{}, zap.NewNop()).Run(rctx)
	next := func(what string) store.Message {
		t.Helper()
		select {
		case m := <-sub:
			return m
		case <-time.After(3 * time.Second):
			t.Fatal(what)
		}
		return store.Message{}
	}
	// #1 and #2 are replayed together and coalesce: one parked notice, for the newest.
	if m := next("no recovery notice"); m.From != "airc" || !strings.Contains(m.Body, "#2 parked") || f.count() != 0 {
		t.Fatalf("expected a parked notice for #2, got %+v (turns=%d)", m, f.count())
	}
	// A live wake after the restart is parked too.
	_ = b.Post(ctx, &store.Message{Channel: "c", From: "human", Body: "@a live work"})
	next("live echo")
	if m := next("no parked notice for the live wake"); !strings.Contains(m.Body, "parked") || f.count() != 0 {
		t.Fatalf("%+v turns=%d", m, f.count())
	}
	ag, _ := s.Agent(ctx, "a")
	if ag.Turns != 0 || ag.LastMsgID != 0 {
		t.Fatalf("rolled back for a later !release: %+v", ag)
	}
	_ = b.Post(ctx, &store.Message{Channel: "c", From: "human", Body: "!release"})
	next("release echo")
	if m := next("no release notice"); !strings.Contains(m.Body, "resuming 2 paused") {
		t.Fatalf("%+v", m)
	}
	// Both replays are enqueued back to back and usually coalesce into one turn; either way
	// every wake is covered exactly once, #1 riding in the delta of #2.
	replies := 0
	for replies == 0 || !strings.Contains(f.last().Prompt, "live work") {
		if m := next("no reply"); m.From != "a" {
			t.Fatalf("%+v", m)
		}
		replies++
	}
	var all strings.Builder
	for _, tn := range f.turns {
		all.WriteString(tn.Prompt)
	}
	for _, want := range []string{"@a work", "more work", "live work"} {
		if strings.Count(all.String(), want) != 1 {
			t.Fatalf("%q must appear in exactly one prompt:\n%s", want, all.String())
		}
	}
	ag, _ = s.Agent(ctx, "a")
	if replies > 2 || ag.Turns != replies || ag.LastMsgID <= 2 {
		t.Fatalf("after rerun: %+v", ag)
	}
	if r, _ := s.Recovering(ctx, "a"); r {
		t.Fatal("release must lift the recovery block")
	}
}

func TestRespawnToAnotherChannelDropsQueuedTriggers(t *testing.T) {
	f := &fake{name: "claude"}
	block := make(chan struct{})
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		select {
		case <-block:
		case <-f.ctx.Done():
		}
		return provider.Result{SessionID: "s", Text: "ok [done]"}, nil
	}
	e := newEnv(t, Config{BotCap: 1000}, f)
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "human", "", "@claude-impl first")
	for f.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	queued := e.post("task-1", "human", "", "@claude-impl second")
	// The operator moves the agent to another channel while the first turn is still running: the
	// queued task-1 trigger must not run against the task-2 configuration.
	e.post("task-1", "human", "", "!spawn claude-impl claude task-2 "+t.TempDir())
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "in task-2") })
	close(block)
	// The first turn finished under the old configuration: its reply is archived as a system
	// audit line, not posted as the agent, and the queued task-1 trigger is dropped.
	audit := e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "Stale reply") })
	if audit.To != "" || !strings.Contains(audit.Body, "ok [done]") {
		t.Fatalf("audit line: %+v", audit)
	}
	time.Sleep(200 * time.Millisecond)
	ag, _ := e.b.Store.Agent(context.Background(), "claude-impl")
	if f.count() != 1 || ag.LastMsgID >= queued.ID || ag.Channel != "task-2" || ag.SessionID != "" {
		t.Fatalf("stale trigger ran or session leaked: turns=%d agent=%+v", f.count(), ag)
	}
}

func TestStaleSuccessDoesNotRedeliverTrigger(t *testing.T) {
	f := &fake{name: "claude"}
	block := make(chan struct{})
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		select {
		case <-block:
		case <-f.ctx.Done():
		}
		return provider.Result{SessionID: "s", Text: "did the thing [done]"}, nil
	}
	e := newEnv(t, Config{BotCap: 1000}, f)
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "human", "", "@claude-impl first request")
	for f.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// Same channel, same provider, new sandbox: a generation bump without reset while the turn runs.
	e.post("task-1", "human", "", "!spawn claude-impl claude task-1 "+t.TempDir()+" --sandbox rw")
	e.wait(func(m store.Message) bool {
		return m.From == "airc" && strings.Contains(m.Body, "updated @claude-impl")
	})
	close(block)
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "Stale reply") })
	e.post("task-1", "human", "", "@claude-impl second request")
	e.wait(from("claude-impl"))
	if f.count() != 2 || strings.Contains(f.last().Prompt, "first request") || !strings.Contains(f.last().Prompt, "second request") {
		t.Fatalf("completed trigger redelivered: turns=%d prompt=%q", f.count(), f.last().Prompt)
	}
	if f.last().Sandbox != "rw" {
		t.Fatalf("new configuration not used: %+v", f.last())
	}
}

func TestQueueOverflowIsParked(t *testing.T) {
	f := &fake{name: "claude"}
	block := make(chan struct{})
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		select {
		case <-block:
		case <-f.ctx.Done():
		}
		return provider.Result{SessionID: "s", Text: "ok [done]"}, nil
	}
	e := newEnv(t, Config{BotCap: 1000}, f)
	e.spawn("claude-impl", "claude", "task-1")
	for i := 0; i < 66; i++ {
		e.post("task-1", "human", "", fmt.Sprintf("@claude-impl n%d", i))
	}
	e.wait(func(m store.Message) bool { return m.From == "airc" && strings.Contains(m.Body, "parked") })
	close(block)
	e.post("task-1", "human", "", "!release")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "resuming") })
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

func TestRenameMidTurnLandsReplyInNewChannel(t *testing.T) {
	f := &fake{name: "claude"}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		once.Do(func() { close(started) })
		<-release
		return provider.Result{SessionID: "s", Text: "@codex-review work done [ask]"}, nil
	}
	e := newEnv(t, Config{}, f)
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "human", "", "@claude-impl go")
	<-started
	e.post("task-1", "human", "", "!rename task-9")
	e.wait(func(m store.Message) bool {
		return m.Channel == "task-9" && strings.Contains(m.Body, "renamed to task-9")
	})
	close(release)
	reply := e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "work done") })
	if reply.Channel != "task-9" || reply.From != "airc" || reply.To != "" {
		t.Fatalf("reply must land in the new channel as a stale audit line: %+v", reply)
	}
	if old, _ := e.b.Store.Messages(context.Background(), "task-1", 0, 0, 0); len(old) != 0 {
		t.Fatalf("old channel resurrected with %d message(s)", len(old))
	}
	// The next wake, addressed in the new channel, runs normally.
	e.post("task-9", "human", "", "@claude-impl again")
	e.wait(func(m store.Message) bool { return m.From == "claude-impl" && m.Channel == "task-9" })
}

func TestRenameFollowsPausedWakes(t *testing.T) {
	f := &fake{name: "claude"}
	e := newEnv(t, Config{}, f)
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "human", "", "!lock")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "channel locked") })
	e.post("task-1", "human", "", "@claude-impl parked")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "is locked") })
	e.post("task-1", "human", "", "!rename task-9")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "renamed to task-9") })
	e.post("task-9", "human", "", "!unlock")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "unlocked, resuming 1") })
	reply := e.wait(from("claude-impl"))
	if reply.Channel != "task-9" || !strings.Contains(f.last().Prompt, "parked") {
		t.Fatalf("replayed wake must run in the renamed channel: %+v", reply)
	}
}

func TestCloseCancelsRunningTurn(t *testing.T) {
	f := &fake{name: "claude"}
	started := make(chan struct{})
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		close(started)
		<-f.ctx.Done()
		return provider.Result{}, f.ctx.Err()
	}
	e := newEnv(t, Config{}, f)
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "human", "", "@claude-impl long")
	<-started
	e.post("task-1", "human", "", "!close")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "closed: hidden") })
	select {
	case <-f.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("!close must cancel the running turn")
	}
}

func TestSpawnChannelFlag(t *testing.T) {
	e := newEnv(t, Config{}, &fake{name: "claude"})
	e.post("lobby", "human", "", "!cwd /tmp")
	e.wait(func(m store.Message) bool { return strings.Contains(m.Body, "workspace of lobby set") })
	e.post("lobby", "human", "", "!spawn claude-x --channel codex /tmp")
	m := e.wait(from("airc"))
	if !strings.Contains(m.Body, "(claude, rw, cwd /tmp) in codex") {
		t.Fatalf("--channel must name a channel called like a provider: %s", m.Body)
	}
}

func TestQueuedWakesCoalesceIntoOneTurn(t *testing.T) {
	f := &fake{name: "claude"}
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	f.reply = func(tn provider.Turn) (provider.Result, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return provider.Result{SessionID: "s", Text: "ok [done]"}, nil
	}
	e := newEnv(t, Config{}, f)
	e.spawn("claude-impl", "claude", "task-1")
	e.post("task-1", "human", "", "@claude-impl first")
	<-started
	e.post("task-1", "human", "", "@claude-impl second")
	e.post("task-1", "human", "", "aside, nobody woken")
	e.post("task-1", "human", "", "@claude-impl third")
	deadline := time.Now().Add(3 * time.Second)
	for {
		st, _ := e.d.Status(context.Background())
		if st[0].Queued == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue never filled: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	e.wait(from("claude-impl"))
	e.wait(from("claude-impl"))
	time.Sleep(50 * time.Millisecond)
	if n := f.count(); n != 2 {
		t.Fatalf("expected 2 turns for 3 wakes, got %d", n)
	}
	p := f.last().Prompt
	for _, want := range []string{"second", "aside", "third"} {
		if !strings.Contains(p, want) {
			t.Fatalf("coalesced prompt lacks %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "first") {
		t.Fatalf("coalesced prompt must start after the previous trigger:\n%s", p)
	}
}

func TestRefusedClaimRetriesOnceWithFreshRead(t *testing.T) {
	f := &fake{name: "claude"}
	e := newEnv(t, Config{}, f)
	e.spawn("claude-impl", "claude", "task-1")
	// A session attach lands between the agent read and the claim: the first claim is refused,
	// the retry reads the new generation and runs.
	var bumps atomic.Int32
	e.d.testBeforeClaim = func() {
		if bumps.Add(1) == 1 {
			_ = e.b.Store.SetSession(context.Background(), "claude-impl", "attached")
		}
	}
	e.post("task-1", "human", "", "@claude-impl work")
	e.wait(from("claude-impl"))
	if f.count() != 1 || bumps.Load() != 2 || !strings.Contains(f.last().Prompt, "work") || f.last().SessionID != "attached" {
		t.Fatalf("turns=%d claims=%d session=%q", f.count(), bumps.Load(), f.last().SessionID)
	}
	// The agent keeps moving under every read: two attempts, then the trigger is dropped.
	bumps.Store(0)
	e.d.testBeforeClaim = func() {
		bumps.Add(1)
		_ = e.b.Store.SetSession(context.Background(), "claude-impl", "again")
	}
	e.post("task-1", "human", "", "@claude-impl more")
	for bumps.Load() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if f.count() != 1 || bumps.Load() != 2 {
		t.Fatalf("turns=%d claims=%d", f.count(), bumps.Load())
	}
}
