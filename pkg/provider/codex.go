package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Codex drives `codex exec`.
type Codex struct {
	// Bin is the executable; default "codex".
	Bin string
}

// Name implements Provider.
func (Codex) Name() string { return "codex" }

// Not verified against a real lost thread: codex's wording is a guess, kept narrow so an
// unrelated "file not found" on stderr does not trigger a costly session restart.
var codexLost = regexp.MustCompile(`(?i)(session|thread|rollout|conversation)\b.{0,40}\bnot found|no (session|rollout|thread) found`)

// Run implements Provider.
func (c Codex) Run(ctx context.Context, t Turn) (Result, error) {
	bin := c.Bin
	if bin == "" {
		bin = "codex"
	}
	sandbox := "read-only"
	if t.Sandbox == "rw" {
		sandbox = "workspace-write"
	}
	// exec is non-interactive: approval_policy=never or the turn fails on the first approval.
	common := []string{"--json", "--skip-git-repo-check", "-c", `approval_policy="never"`}
	var args []string
	if t.SessionID == "" {
		args = append([]string{"exec", "-C", t.Cwd, "-s", sandbox}, common...)
	} else {
		// resume has no -s/-C; the sandbox goes through config overrides.
		args = append([]string{"exec", "resume", t.SessionID}, common...)
		args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", sandbox))
	}
	// No system-prompt flag on exec: the protocol travels as a prompt prefix.
	prompt := t.Prompt
	if t.Protocol != "" {
		prompt = t.Protocol + "\n\n---\n\n" + t.Prompt
	}
	args = append(args, "-")

	out, stderr, err := run(ctx, bin, args, t.Cwd, prompt, t.Env)
	if err != nil {
		if t.SessionID != "" && codexLost.MatchString(stderr) {
			return Result{}, fmt.Errorf("%w: %v", ErrSessionLost, err)
		}
		return Result{}, err
	}
	res, err := parseCodex(out)
	if err != nil {
		return Result{}, err
	}
	if res.SessionID == "" {
		res.SessionID = t.SessionID
	}
	return res, nil
}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Item struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Usage struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
}

// parseCodex reads the JSONL event stream. The reply is the last agent_message of the turn.
func parseCodex(out []byte) (Result, error) {
	var r Result
	var errs []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			r.SessionID = ev.ThreadID
		case "item.completed":
			if ev.Item.Type == "agent_message" {
				r.Text = ev.Item.Text
			}
		case "turn.completed":
			r.Usage = fmt.Sprintf("in=%d out=%d", ev.Usage.Input, ev.Usage.Output)
		case "turn.failed":
			if ev.Error != nil {
				errs = append(errs, ev.Error.Message)
			}
		case "error":
			errs = append(errs, ev.Message)
		}
	}
	if err := sc.Err(); err != nil {
		return Result{}, fmt.Errorf("read codex output: %w", err)
	}
	if r.Text == "" && len(errs) > 0 {
		return Result{}, fmt.Errorf("codex reported an error: %s", strings.Join(errs, "; "))
	}
	if r.Text == "" && r.SessionID == "" {
		return Result{}, fmt.Errorf("parse codex output: no events (%s)", head(out))
	}
	return r, nil
}
