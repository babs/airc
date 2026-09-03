package store

import (
	"context"
	"strings"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMessagesRangeAndTail(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i, body := range []string{"!spawn ignored", "topic line\nmore", "b", "c", "d"} {
		m := &Message{Channel: "task-1", From: "human", Body: body, Tags: []string{"done", "waiting for human"}}
		if i == 4 {
			m.Channel = "lobby"
		}
		if err := s.Insert(ctx, m); err != nil {
			t.Fatal(err)
		}
		if m.ID != int64(i+1) {
			t.Fatalf("id: want %d got %d", i+1, m.ID)
		}
	}
	got, err := s.Messages(ctx, "task-1", 2, 4, 0)
	if err != nil || len(got) != 1 || got[0].Body != "b" || len(got[0].Tags) != 2 || got[0].Tags[1] != "waiting for human" {
		t.Fatalf("range: %v %+v", err, got)
	}
	all, _ := s.Messages(ctx, "", 0, 0, 0)
	if len(all) != 5 {
		t.Fatalf("all channels: %d", len(all))
	}
	latest, _ := s.Latest(ctx, "task-1", 0, 0, 2)
	if len(latest) != 2 || latest[0].Body != "b" || latest[1].Body != "c" {
		t.Fatalf("latest keeps the newest rows: %+v", latest)
	}
	tail, _ := s.Tail(ctx, "task-1", 2)
	if len(tail) != 2 || tail[0].Body != "b" || tail[1].Body != "c" {
		t.Fatalf("tail order: %+v", tail)
	}
	_ = s.SetSetting(ctx, "cwd:task-1", "/wt/task-1")
	chs, _ := s.Channels(ctx)
	if len(chs) != 2 || chs[0].Name != "lobby" || chs[1].Topic != "topic line" || chs[1].Cwd != "/wt/task-1" || chs[0].Cwd != "" ||
		chs[1].LastFrom != "human" || chs[1].LastBody != "c" || chs[1].LastTS == "" {
		t.Fatalf("channels: %+v", chs)
	}
	// A channel holding only commands and system replies has no topic candidate: must not fail.
	_ = s.Insert(ctx, &Message{Channel: "ctl", From: "human", Body: "!status"})
	_ = s.Insert(ctx, &Message{Channel: "ctl", From: "airc", Body: "no agents"})
	chs, err = s.Channels(ctx)
	if err != nil || len(chs) != 3 || chs[0].Name != "ctl" || chs[0].Topic != "" {
		t.Fatalf("command-only channel: %v %+v", err, chs)
	}
	_ = s.SetSetting(ctx, "archived:ctl", "1")
	chs, _ = s.Channels(ctx)
	if len(chs) != 2 || chs[0].Name == "ctl" {
		t.Fatalf("archived channel must be hidden: %+v", chs)
	}
	_ = s.SetSetting(ctx, "topic:task-1", "Explicit title")
	chs, _ = s.Channels(ctx)
	if chs[1].Topic != "Explicit title" {
		t.Fatalf("topic setting must win: %+v", chs[1])
	}
	if err := s.RenameChannel(ctx, "task-1", "task-9"); err != nil {
		t.Fatal(err)
	}
	chs, _ = s.Channels(ctx)
	if len(chs) != 2 || chs[1].Name != "task-9" || chs[1].Topic != "Explicit title" || chs[1].Cwd != "/wt/task-1" {
		t.Fatalf("rename: %+v", chs)
	}
	if n, _ := s.Messages(ctx, "task-1", 0, 0, 0); len(n) != 0 {
		t.Fatal("old channel must be empty after rename")
	}
}

func TestAgentsRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := &Agent{Nick: "claude-impl", Provider: "claude", Channel: "task-1", Cwd: "/tmp", Home: "/home/x/.claude-agent", Sandbox: "rw",
		Tools: []string{"Bash(git:*)", "Bash(curl:*)"}, Token: "tok", Enabled: true}
	if err := s.PutAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	a.Turns = 3
	a.SessionID = "sid"
	if err := s.PutAgent(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := s.AgentByToken(ctx, "tok")
	if err != nil || got.Turns != 3 || got.SessionID != "sid" || len(got.Tools) != 2 || !got.Enabled || got.Home != "/home/x/.claude-agent" {
		t.Fatalf("agent: %v %+v", err, got)
	}
	if _, err := s.Agent(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	n, _ := s.ChannelTurns(ctx, "task-1")
	if n != 3 {
		t.Fatalf("channel turns: %d", n)
	}
	genOf := func(nick string) int { a, _ := s.Agent(ctx, nick); return a.Gen }
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "task-1", genOf("claude-impl")+1, 42); ok {
		t.Fatal("claim with a stale generation must fail")
	}
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "task-1", genOf("claude-impl"), 42); !ok {
		t.Fatal("claim on an idle agent must succeed")
	}
	_ = s.SetSession(ctx, "claude-impl", "sid2")
	_ = s.SetEnabled(ctx, "claude-impl", false)
	_ = s.SetStatus(ctx, "claude-impl", "reviewing")
	got, _ = s.Agent(ctx, "claude-impl")
	if got.Turns != 4 || got.LastMsgID != 42 || got.SessionID != "sid2" || got.Enabled || got.Cwd != "/tmp" {
		t.Fatalf("partial updates: %+v", got)
	}
	_ = s.ReleaseTurns(ctx, "task-1", 10)
	got, _ = s.Agent(ctx, "claude-impl")
	if got.Turns != 0 {
		t.Fatalf("release floor: %d", got.Turns)
	}
	// Refused claims: stopped, still in flight, wrong channel, stale trigger.
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "task-1", genOf("claude-impl"), 50); ok {
		t.Fatal("claim while stopped")
	}
	_ = s.SetEnabled(ctx, "claude-impl", true)
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "task-1", genOf("claude-impl"), 50); ok {
		t.Fatal("claim while in flight")
	}
	_ = s.RollbackTurn(ctx, "claude-impl")
	got, _ = s.Agent(ctx, "claude-impl")
	if got.Turns != 0 || got.LastMsgID != 0 {
		t.Fatalf("rollback: %+v", got)
	}
	_ = s.RollbackTurn(ctx, "claude-impl")
	got, _ = s.Agent(ctx, "claude-impl")
	if got.LastMsgID != 0 {
		t.Fatalf("second rollback must be a no-op: %+v", got)
	}
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "elsewhere", genOf("claude-impl"), 50); ok {
		t.Fatal("claim from another channel")
	}
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "task-1", genOf("claude-impl"), 0); ok {
		t.Fatal("claim with a stale trigger")
	}
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "task-1", genOf("claude-impl"), 60); !ok {
		t.Fatal("claim after rollback")
	}
	claimGen := genOf("claude-impl")
	_ = s.SetSession(ctx, "claude-impl", "operator-attached") // bumps gen: the running turn is stale
	reply := &Message{Channel: "task-1", From: "claude-impl", To: "codex-review", Body: "@codex-review done [ask]", Tags: []string{"ask"}}
	stale, err := s.FinishTurn(ctx, "claude-impl", claimGen, "from-provider", reply)
	if err != nil || !stale || reply.ID == 0 {
		t.Fatalf("stale finish: %v stale=%v %+v", err, stale, reply)
	}
	got, _ = s.Agent(ctx, "claude-impl")
	if got.SessionID != "operator-attached" || got.LastMsgID != 60 || got.Turns != 1 {
		t.Fatalf("stale finish must keep the session and consume the trigger: %+v", got)
	}
	if msgs, _ := s.Messages(ctx, "task-1", reply.ID-1, 0, 0); len(msgs) != 1 || msgs[0].From != "airc" || msgs[0].To != "" || !strings.Contains(msgs[0].Body, "done [ask]") {
		t.Fatalf("stale reply must be kept as a non-addressing audit line: %+v", msgs)
	}
	// First-turn reset: empty old and new session must not be confused for a match.
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "task-1", genOf("claude-impl"), 61); !ok {
		t.Fatal("claim 61")
	}
	claimGen = genOf("claude-impl")
	_ = s.UpdateAgentConfig(ctx, &Agent{Nick: "claude-impl", Provider: "codex", Channel: "task-1", Cwd: "/x", Sandbox: "rw"}, true)
	reply = &Message{Channel: "task-1", From: "claude-impl", Body: "old provider done"}
	if stale, _ := s.FinishTurn(ctx, "claude-impl", claimGen, "old-provider-session", reply); !stale {
		t.Fatal("finish after a reset respawn must be stale")
	}
	got, _ = s.Agent(ctx, "claude-impl")
	if got.SessionID != "" || got.Provider != "codex" {
		t.Fatalf("reset row must keep an empty session: %+v", got)
	}
	// Fresh finish.
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "task-1", genOf("claude-impl"), 62); !ok {
		t.Fatal("claim 62")
	}
	reply = &Message{Channel: "task-1", From: "claude-impl", Body: "done [done]", Tags: []string{"done"}}
	if stale, err := s.FinishTurn(ctx, "claude-impl", genOf("claude-impl"), "from-provider", reply); err != nil || stale || reply.ID == 0 {
		t.Fatalf("finish turn: %v stale=%v %+v", err, stale, reply)
	}
	got, _ = s.Agent(ctx, "claude-impl")
	if got.SessionID != "from-provider" || got.LastMsgID != 62 {
		t.Fatalf("finish must record the session: %+v", got)
	}
	if msgs, _ := s.Messages(ctx, "task-1", reply.ID-1, 0, 0); len(msgs) != 1 || msgs[0].Body != "done [done]" {
		t.Fatalf("reply must be stored with the finish: %+v", msgs)
	}
	if ok, _ := s.ClaimTurn(ctx, "claude-impl", "task-1", genOf("claude-impl"), 70); !ok {
		t.Fatal("claim after finish")
	}
	nicks, _ := s.RecoverInflight(ctx)
	got, _ = s.Agent(ctx, "claude-impl")
	if len(nicks) != 1 || got.LastMsgID != 62 {
		t.Fatalf("recover inflight: %v %+v", nicks, got)
	}
	if r, _ := s.Recovering(ctx, "claude-impl"); !r {
		t.Fatal("recovery block must be persisted")
	}
	_ = s.SetRecovering(ctx, "claude-impl", false)
	if r, _ := s.Recovering(ctx, "claude-impl"); r {
		t.Fatal("recovery block must clear")
	}
	g := genOf("claude-impl")
	_ = s.UpdateAgentConfig(ctx, &Agent{Nick: "claude-impl", Provider: "codex", Channel: "task-1", Cwd: "/x", Sandbox: "rw"}, false)
	if genOf("claude-impl") != g+1 {
		t.Fatalf("config update must bump gen: %d -> %d", g, genOf("claude-impl"))
	}
	got, _ = s.Agent(ctx, "claude-impl")
	if got.Provider != "codex" || got.Cwd != "/x" || !got.Enabled || got.SessionID != "from-provider" || got.LastMsgID != 62 {
		t.Fatalf("update config keeps session and cursor: %+v", got)
	}
	_ = s.UpdateAgentConfig(ctx, got, true)
	got, _ = s.Agent(ctx, "claude-impl")
	if got.SessionID != "" || got.LastMsgID != 0 {
		t.Fatalf("update config reset: %+v", got)
	}
	if ok, _ := s.ConsumeGrant(ctx, "task-1"); ok {
		t.Fatal("no grant must not consume")
	}
	_ = s.SetSetting(ctx, "grant:task-1", "2")
	a1, _ := s.ConsumeGrant(ctx, "task-1")
	a2, _ := s.ConsumeGrant(ctx, "task-1")
	a3, _ := s.ConsumeGrant(ctx, "task-1")
	if !a1 || !a2 || a3 {
		t.Fatalf("grant consumption: %v %v %v", a1, a2, a3)
	}
	if v, _ := s.Setting(ctx, "k"); v != "" {
		t.Fatal("empty setting")
	}
	_ = s.SetSetting(ctx, "k", "v")
	if v, _ := s.Setting(ctx, "k"); v != "v" {
		t.Fatal("setting round trip")
	}
}

func TestTopicSkipsRelayHeaderAndMessageChannel(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	relay := &Message{Channel: "t", From: "claude-x", Body: "(relayed from lobby)\n@codex-y look"}
	_ = s.Insert(ctx, relay)
	_ = s.Insert(ctx, &Message{Channel: "t", From: "human", Body: "real topic"})
	chs, _ := s.Channels(ctx)
	if len(chs) != 1 || chs[0].Topic != "real topic" {
		t.Fatalf("topic must skip relay headers: %+v", chs)
	}
	if ch, err := s.MessageChannel(ctx, relay.ID); err != nil || ch != "t" {
		t.Fatalf("message channel: %v %q", err, ch)
	}
	_ = s.RenameChannel(ctx, "t", "u")
	if ch, _ := s.MessageChannel(ctx, relay.ID); ch != "u" {
		t.Fatalf("message channel after rename: %q", ch)
	}
	if _, err := s.MessageChannel(ctx, 9999); err != ErrNotFound {
		t.Fatalf("missing message: %v", err)
	}
}

func TestRenameBumpsGen(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	_ = s.PutAgent(ctx, &Agent{Nick: "a", Provider: "claude", Channel: "t", Cwd: "/", Sandbox: "rw", Token: "k", Enabled: true})
	before, _ := s.Agent(ctx, "a")
	_ = s.RenameChannel(ctx, "t", "u")
	after, _ := s.Agent(ctx, "a")
	if after.Channel != "u" || after.Gen != before.Gen+1 {
		t.Fatalf("rename must move the agent and bump gen: %+v -> %+v", before, after)
	}
}
