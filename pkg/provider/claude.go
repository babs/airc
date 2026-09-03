package provider

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Claude drives `claude -p`.
type Claude struct {
	// Bin is the executable; default "claude".
	Bin string
	// Mode is the permission mode for rw agents; default "auto". Never bypassPermissions.
	Mode string
}

// Name implements Provider.
func (Claude) Name() string { return "claude" }

var claudeLost = regexp.MustCompile(`(?i)no conversation found|session.*not found`)

// Run implements Provider.
func (c Claude) Run(ctx context.Context, t Turn) (Result, error) {
	bin := c.Bin
	if bin == "" {
		bin = "claude"
	}
	sid, first := t.SessionID, false
	if sid == "" {
		sid, first = newUUID(), true
	}
	args := []string{"-p", "--output-format", "json"}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	if first {
		args = append(args, "--session-id", sid)
	} else {
		args = append(args, "--resume", sid)
	}
	// "ro": default mode denies edits in -p (no prompt possible), so the agent can only read.
	mode := "default"
	if t.Sandbox == "rw" {
		mode = c.Mode
		if mode == "" {
			mode = "auto"
		}
	}
	args = append(args, "--permission-mode", mode)
	if len(t.Tools) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, t.Tools...)
	}
	if t.Protocol != "" {
		args = append(args, "--append-system-prompt", t.Protocol)
	}
	out, stderr, err := run(ctx, bin, args, t.Cwd, t.Prompt, t.Env)
	if err != nil {
		if !first && claudeLost.MatchString(stderr+string(out)) {
			return Result{}, fmt.Errorf("%w: %v", ErrSessionLost, err)
		}
		return Result{}, err
	}
	res, err := parseClaude(out)
	if err != nil {
		return Result{}, err
	}
	if res.SessionID == "" {
		res.SessionID = sid
	}
	return res, nil
}

type claudeOut struct {
	SessionID string  `json:"session_id"`
	Result    string  `json:"result"`
	IsError   bool    `json:"is_error"`
	CostUSD   float64 `json:"total_cost_usd"`
	NumTurns  int     `json:"num_turns"`
	Usage     struct {
		Input  int `json:"input_tokens"`
		Output int `json:"output_tokens"`
	} `json:"usage"`
	Denials []struct {
		Tool  string          `json:"tool_name"`
		Input json.RawMessage `json:"tool_input"`
	} `json:"permission_denials"`
}

func parseClaude(out []byte) (Result, error) {
	var o claudeOut
	if err := json.Unmarshal(out, &o); err != nil {
		return Result{}, fmt.Errorf("parse claude output: %w (%s)", err, head(out))
	}
	if o.IsError {
		return Result{}, fmt.Errorf("claude reported an error: %s", head([]byte(o.Result)))
	}
	r := Result{SessionID: o.SessionID, Text: o.Result, CostUSD: o.CostUSD,
		Usage: fmt.Sprintf("in=%d out=%d turns=%d", o.Usage.Input, o.Usage.Output, o.NumTurns)}
	for _, d := range o.Denials {
		r.Denials = append(r.Denials, d.Tool+" "+head(d.Input))
	}
	return r, nil
}

func head(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
