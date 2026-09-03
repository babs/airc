package proto

import (
	"reflect"
	"testing"
)

func TestAddress(t *testing.T) {
	cases := map[string]string{
		"@codex-review diff ready [ask]": "codex-review",
		"  \n@claude-impl: go":           "claude-impl",
		"hello @codex-review":            "",
		"summary line\n\ndetails\n\n@codex-review please review [ask]": "codex-review",
		"@claude-impl first\n@codex-review second":                     "claude-impl",
		"see `@codex-review` inline\n  @codex-review indented":         "codex-review",
		"email me@example.com\nno address":                             "",
		"@Bad_Nick x":                                                  "",
		"@a":                                                           "a",
		"":                                                             "",
		"@codex-review\nsecond line":                                   "codex-review",
	}
	for in, want := range cases {
		if got := Address(in); got != want {
			t.Errorf("Address(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTags(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"done [done]", []string{"done"}},
		{"x [ASK] y [Waiting For Human] [ask]", []string{"ask", "waiting for human"}},
		{"[blocked]", []string{"blocked"}},
		{"[unknown] none", nil},
		{"[done ]", nil},
	}
	for _, c := range cases {
		if got := Tags(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("Tags(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestParseCommand(t *testing.T) {
	c, err := ParseCommand("!spawn claude-impl claude task-1 /wt/x --sandbox rw --tools=Bash(git:*),Bash(curl:*) --dry\nignored")
	if err != nil {
		t.Fatal(err)
	}
	want := &Command{Verb: "spawn", Args: []string{"claude-impl", "claude", "task-1", "/wt/x"},
		Flags: map[string]string{"sandbox": "rw", "tools": "Bash(git:*),Bash(curl:*)", "dry": "true"}}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v", c)
	}
	if _, err := ParseCommand("plain text"); err != ErrNotCommand {
		t.Fatalf("want ErrNotCommand, got %v", err)
	}
	if _, err := ParseCommand("!"); err == nil {
		t.Fatal("empty command must fail")
	}
	if !ValidNick("claude-impl-2") || ValidNick("-x") || ValidNick("Up") || ValidNick("") {
		t.Fatal("ValidNick")
	}
}
