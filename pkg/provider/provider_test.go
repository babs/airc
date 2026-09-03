package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseClaude(t *testing.T) {
	r, err := parseClaude([]byte(`{"session_id":"s1","result":"pong\n\n[done]","is_error":false,"total_cost_usd":0.5,
		"num_turns":2,"usage":{"input_tokens":10,"output_tokens":3},
		"permission_denials":[{"tool_name":"Bash","tool_input":{"command":"touch x"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionID != "s1" || r.Text != "pong\n\n[done]" || r.CostUSD != 0.5 || len(r.Denials) != 1 ||
		!strings.Contains(r.Denials[0], "touch x") || r.Usage != "in=10 out=3 turns=2" {
		t.Fatalf("%+v", r)
	}
	if _, err := parseClaude([]byte(`{"is_error":true,"result":"boom"}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("is_error: %v", err)
	}
	if _, err := parseClaude([]byte(`not json`)); err == nil {
		t.Fatal("garbage must fail")
	}
}

func TestParseCodex(t *testing.T) {
	out := `{"type":"thread.started","thread_id":"t1"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"ls"}}
{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":"first"}}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"pong [done]"}}
{"type":"turn.completed","usage":{"input_tokens":7,"output_tokens":2}}
`
	r, err := parseCodex([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionID != "t1" || r.Text != "pong [done]" || r.Usage != "in=7 out=2" {
		t.Fatalf("%+v", r)
	}
	_, err = parseCodex([]byte(`{"type":"thread.started","thread_id":"t1"}` + "\n" + `{"type":"error","message":"quota"}`))
	if err == nil || !strings.Contains(err.Error(), "quota") {
		t.Fatalf("error event: %v", err)
	}
	if _, err := parseCodex([]byte("")); err == nil {
		t.Fatal("empty must fail")
	}
}

// fakeBin writes a shell script standing in for the CLI. It dumps argv to $ARGS_FILE, stdin to
// $ARGS_FILE.in and the environment to $ARGS_FILE.env.
func fakeBin(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGS_FILE\"\ncat > \"$ARGS_FILE.in\"\nenv > \"$ARGS_FILE.env\"\n" + body
	if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeRunArgsAndSessionLost(t *testing.T) {
	t.Setenv("AIRC_TOKEN", "admin-secret")
	t.Setenv("CLAUDECODE", "1")
	args := filepath.Join(t.TempDir(), "args")
	bin := fakeBin(t, `case "$*" in *--resume*) echo "No conversation found with session ID" >&2; exit 1;; esac
echo '{"session_id":"","result":"ok"}'`)
	env := []string{"ARGS_FILE=" + args}
	c := Claude{Bin: bin}
	r, err := c.Run(context.Background(), Turn{Cwd: t.TempDir(), Sandbox: "rw", Model: "claude-sonnet-5", Tools: []string{"Bash(git:*)"}, Protocol: "P", Prompt: "hi", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if r.SessionID == "" || r.Text != "ok" {
		t.Fatalf("%+v", r)
	}
	got, _ := os.ReadFile(args)
	for _, want := range []string{"--session-id\n" + r.SessionID, "--permission-mode\nauto", "--allowedTools\nBash(git:*)", "--append-system-prompt\nP", "--model\nclaude-sonnet-5"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("args missing %q in:\n%s", want, got)
		}
	}
	if in, _ := os.ReadFile(args + ".in"); string(in) != "hi" {
		t.Errorf("prompt must travel on stdin, got %q", in)
	}
	if envDump, _ := os.ReadFile(args + ".env"); strings.Contains(string(envDump), "admin-secret") || strings.Contains(string(envDump), "CLAUDECODE=") {
		t.Errorf("server env leaked to the agent:\n%s", envDump)
	}
	_, err = c.Run(context.Background(), Turn{SessionID: r.SessionID, Cwd: t.TempDir(), Sandbox: "ro", Prompt: "again", Env: env})
	if !errors.Is(err, ErrSessionLost) {
		t.Fatalf("want ErrSessionLost, got %v", err)
	}
	got, _ = os.ReadFile(args)
	if !strings.Contains(string(got), "--permission-mode\ndefault") {
		t.Errorf("ro must map to default mode:\n%s", got)
	}
}

func TestCodexRunArgs(t *testing.T) {
	args := filepath.Join(t.TempDir(), "args")
	bin := fakeBin(t, `echo '{"type":"thread.started","thread_id":"t9"}'; echo '{"type":"item.completed","item":{"type":"agent_message","text":"yo"}}'`)
	env := []string{"ARGS_FILE=" + args}
	c := Codex{Bin: bin}
	r, err := c.Run(context.Background(), Turn{Cwd: t.TempDir(), Sandbox: "ro", Model: "gpt-5-codex", Protocol: "P", Prompt: "hi", Env: env})
	if err != nil || r.SessionID != "t9" || r.Text != "yo" {
		t.Fatalf("%v %+v", err, r)
	}
	got, _ := os.ReadFile(args)
	for _, want := range []string{"exec\n-C\n", "-s\nread-only", "--json", `approval_policy="never"`, "\n-\n", `model="gpt-5-codex"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("first turn args missing %q in:\n%s", want, got)
		}
	}
	if in, _ := os.ReadFile(args + ".in"); string(in) != "P\n\n---\n\nhi" {
		t.Errorf("protocol+prompt must travel on stdin, got %q", in)
	}
	_, err = c.Run(context.Background(), Turn{SessionID: "t9", Cwd: t.TempDir(), Sandbox: "rw", Prompt: "x", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(args)
	for _, want := range []string{"exec\nresume\nt9\n", `sandbox_mode="workspace-write"`, `approvals_reviewer="auto_review"`, `approval_policy="on-request"`, "network_access=true"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("rw resume args missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(string(got), "\n-s\n") {
		t.Errorf("resume must not pass -s:\n%s", got)
	}
	_, err = Codex{Bin: bin, Strict: true}.Run(context.Background(), Turn{SessionID: "t9", Cwd: t.TempDir(), Sandbox: "rw", Prompt: "x", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(args)
	if !strings.Contains(string(got), `approval_policy="never"`) || strings.Contains(string(got), "auto_review") {
		t.Errorf("strict rw args:\n%s", got)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	bin := fakeBin(t, "sleep 30 & wait")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := run(ctx, bin, nil, t.TempDir(), "", []string{"ARGS_FILE=/dev/null"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("child sleep kept the parent waiting: process group not killed")
	}
}
