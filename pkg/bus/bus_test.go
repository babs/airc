package bus

import (
	"context"
	"testing"

	"github.com/babs/airc/pkg/store"
)

func TestPostFansOut(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := New(s)
	live, cancel := b.Subscribe(1)
	full, cancelFull := b.Subscribe(0) // never drained: must not block Post
	defer cancelFull()

	m := &store.Message{Channel: "c", From: "h", Body: "x"}
	if err := b.Post(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	got := <-live
	if got.ID != m.ID || got.Body != "x" {
		t.Fatalf("got %+v", got)
	}
	cancel()
	if err := b.Post(context.Background(), &store.Message{Channel: "c", From: "h", Body: "y"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-live:
		t.Fatal("received after cancel")
	default:
	}
	_ = full
	if b.Dropped() != 2 {
		t.Fatalf("dropped: %d", b.Dropped())
	}
}

func TestPostDerivesAddressAndTags(t *testing.T) {
	s, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := New(s)
	m := &store.Message{Channel: "c", From: "h", Body: "@codex-review look [ask]"}
	if err := b.Post(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if m.To != "codex-review" || len(m.Tags) != 1 || m.Tags[0] != "ask" {
		t.Fatalf("derived %+v", m)
	}
	explicit := &store.Message{Channel: "c", From: "h", To: "other", Body: "@codex-review x"}
	_ = b.Post(context.Background(), explicit)
	if explicit.To != "other" {
		t.Fatal("explicit To must win")
	}
}
