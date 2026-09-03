package restart

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatchWaitsForStableChangeAndIdle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(p, []byte("v1"), 0o700); err != nil {
		t.Fatal(err)
	}
	var idle atomic.Bool
	var logs atomic.Int32
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { done <- Watch(ctx, p, 20*time.Millisecond, idle.Load, func(string) { logs.Add(1) }) }()

	time.Sleep(60 * time.Millisecond)
	if err := os.WriteFile(p, []byte("v2-longer"), 0o700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("returned while busy: %v", err)
	default:
	}
	idle.Store(true)
	select {
	case err := <-done:
		if !errors.Is(err, ErrUpdated) {
			t.Fatalf("want ErrUpdated, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no restart after idle")
	}
	if logs.Load() != 1 {
		t.Fatalf("update logged %d times, want once", logs.Load())
	}
}

func TestWatchIgnoresUnchangedAndStopsOnCancel(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bin")
	_ = os.WriteFile(p, []byte("v1"), 0o700)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Watch(ctx, p, 10*time.Millisecond, func() bool { return true }, func(string) {}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat("/nonexistent/bin"); err == nil {
		t.Fatal("precondition")
	}
	if err := Watch(context.Background(), "/nonexistent/bin", time.Millisecond, nil, nil); err == nil {
		t.Fatal("missing binary must fail fast")
	}
}
