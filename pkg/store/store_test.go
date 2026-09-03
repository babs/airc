package store

import (
	"context"
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
	for i, body := range []string{"topic line\nmore", "b", "c", "d"} {
		m := &Message{Channel: "task-1", From: "human", Body: body, Tags: []string{"done", "waiting for human"}}
		if i == 3 {
			m.Channel = "lobby"
		}
		if err := s.Insert(ctx, m); err != nil {
			t.Fatal(err)
		}
		if m.ID != int64(i+1) {
			t.Fatalf("id: want %d got %d", i+1, m.ID)
		}
	}
	got, err := s.Messages(ctx, "task-1", 1, 3, 0)
	if err != nil || len(got) != 1 || got[0].Body != "b" || len(got[0].Tags) != 2 || got[0].Tags[1] != "waiting for human" {
		t.Fatalf("range: %v %+v", err, got)
	}
	all, _ := s.Messages(ctx, "", 0, 0, 0)
	if len(all) != 4 {
		t.Fatalf("all channels: %d", len(all))
	}
	tail, _ := s.Tail(ctx, "task-1", 2)
	if len(tail) != 2 || tail[0].Body != "b" || tail[1].Body != "c" {
		t.Fatalf("tail order: %+v", tail)
	}
	chs, _ := s.Channels(ctx)
	if len(chs) != 2 || chs[0].Name != "lobby" || chs[1].Topic != "topic line" {
		t.Fatalf("channels: %+v", chs)
	}
}

func TestAgentsRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a := &Agent{Nick: "claude-impl", Provider: "claude", Channel: "task-1", Cwd: "/tmp", Sandbox: "rw",
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
	if err != nil || got.Turns != 3 || got.SessionID != "sid" || len(got.Tools) != 2 || !got.Enabled {
		t.Fatalf("agent: %v %+v", err, got)
	}
	if _, err := s.Agent(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	n, _ := s.ChannelTurns(ctx, "task-1")
	if n != 3 {
		t.Fatalf("channel turns: %d", n)
	}
	_ = s.BumpTurn(ctx, "claude-impl", 42)
	_ = s.SetSession(ctx, "claude-impl", "sid2")
	_ = s.SetEnabled(ctx, "claude-impl", false)
	got, _ = s.Agent(ctx, "claude-impl")
	if got.Turns != 4 || got.LastMsgID != 42 || got.SessionID != "sid2" || got.Enabled || got.Cwd != "/tmp" {
		t.Fatalf("partial updates: %+v", got)
	}
	_ = s.ReleaseTurns(ctx, "task-1", 10)
	got, _ = s.Agent(ctx, "claude-impl")
	if got.Turns != 0 {
		t.Fatalf("release floor: %d", got.Turns)
	}
	_ = s.BumpTurn(ctx, "claude-impl", 50)
	_ = s.RollbackTurn(ctx, "claude-impl", 42)
	got, _ = s.Agent(ctx, "claude-impl")
	if got.Turns != 0 || got.LastMsgID != 42 {
		t.Fatalf("rollback: %+v", got)
	}
	_ = s.UpdateAgentConfig(ctx, &Agent{Nick: "claude-impl", Provider: "codex", Channel: "task-1", Cwd: "/x", Sandbox: "rw"}, false)
	got, _ = s.Agent(ctx, "claude-impl")
	if got.Provider != "codex" || got.Cwd != "/x" || !got.Enabled || got.SessionID != "sid2" || got.LastMsgID != 42 {
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
