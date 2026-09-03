// Package provider runs one turn of an agent CLI (Claude Code, Codex) and parses its output.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Turn is everything a provider needs to run one exchange.
type Turn struct {
	// SessionID is empty on the first turn; the provider then creates one and returns it.
	SessionID string
	Cwd       string
	// Sandbox is "ro" or "rw".
	Sandbox string
	// Model overrides the CLI's default model when set.
	Model string
	// Tools are extra allowed tools (Claude syntax, e.g. "Bash(git:*)"). Ignored by Codex.
	Tools []string
	// Protocol is the bus conventions text, injected as system prompt or prompt prefix.
	Protocol string
	Prompt   string
	// Env is appended to the subprocess environment (AIRC_URL, AIRC_TOKEN, ...).
	Env []string
}

// Result is the parsed outcome of a turn.
type Result struct {
	SessionID string
	Text      string
	// Denials lists tool calls the CLI refused (Claude only).
	Denials []string
	CostUSD float64
	// Usage is a short free-form usage summary for logs.
	Usage string
}

// Provider drives one CLI.
type Provider interface {
	Name() string
	Run(ctx context.Context, t Turn) (Result, error)
}

// ErrSessionLost means the CLI no longer knows the session; the caller may start a fresh one.
var ErrSessionLost = errors.New("session lost")

// ExitError carries the CLI's stderr for diagnostics.
type ExitError struct {
	Cmd    string
	Err    error
	Stderr string
}

func (e *ExitError) Error() string {
	s := strings.TrimSpace(e.Stderr)
	if len(s) > 500 {
		s = s[len(s)-500:]
	}
	return fmt.Sprintf("%s: %v: %s", e.Cmd, e.Err, s)
}

func (e *ExitError) Unwrap() error { return e.Err }

// run executes bin with args in cwd, feeding stdin, and returns stdout. The prompt travels on
// stdin: a single argv entry is capped at 128 KiB on Linux and a channel log can exceed that.
// The process gets its own group so a timeout kills the whole tree (agents spawn shells).
func run(ctx context.Context, bin string, args []string, cwd, stdin string, env []string) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // bin is operator config (AIRC_*_BIN)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = append(cleanEnv(os.Environ()), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return stdout.Bytes(), stderr.String(), &ExitError{Cmd: bin, Err: err, Stderr: stderr.String()}
	}
	return stdout.Bytes(), stderr.String(), nil
}

// cleanEnv drops variables that make a nested CLI refuse to start, and every AIRC_* one: the
// server's own AIRC_TOKEN is the admin token and must never reach an agent.
func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDECODE" || k == "CLAUDE_CODE_ENTRYPOINT" || strings.HasPrefix(k, "AIRC_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Lookup returns the provider registered under name.
func Lookup(providers []Provider, name string) (Provider, bool) {
	for _, p := range providers {
		if p.Name() == name {
			return p, true
		}
	}
	return nil, false
}
