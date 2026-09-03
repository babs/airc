// Package restart re-executes the process when its binary changes and the driver is idle.
package restart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrUpdated is returned by Watch when the binary changed and the process should re-exec.
var ErrUpdated = errors.New("binary updated")

// Watch polls path every interval. Once its size or mtime differ from the initial values and
// stay stable for one more tick (the build finished), it waits for idle() and returns ErrUpdated.
func Watch(ctx context.Context, path string, interval time.Duration, idle func() bool, log func(string)) error {
	base, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}
	var pending os.FileInfo
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		cur, err := os.Stat(path)
		if err != nil {
			// Mid-rebuild the file may be missing for an instant.
			continue
		}
		if same(cur, base) {
			pending = nil
			continue
		}
		if pending == nil || !same(cur, pending) {
			pending = cur
			log("update_detected")
			continue
		}
		if !idle() {
			continue
		}
		return ErrUpdated
	}
}

func same(a, b os.FileInfo) bool {
	return a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
