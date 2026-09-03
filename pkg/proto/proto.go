// Package proto holds the wire conventions shared by humans and agents: addressing, tags, commands.
package proto

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Tags an agent can close a turn with.
const (
	TagDone    = "done"
	TagAsk     = "ask"
	TagBlocked = "blocked"
	TagWaiting = "waiting for human"
)

// SystemNick is the driver's own voice on the bus.
const SystemNick = "airc"

var (
	nickRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	addressRe = regexp.MustCompile(`(?m)^[ \t]*@([a-z0-9][a-z0-9-]{0,31})\b`)
	tagRe     = regexp.MustCompile(`(?i)\[(done|ask|blocked|waiting for human)\]`)
)

// ValidNick reports whether s is an acceptable nick or channel name.
func ValidNick(s string) bool { return nickRe.MatchString(s) }

// Address returns the nick a body addresses: the first line that starts with "@nick". Agents put
// a summary first and the addressee at the end, so any line counts, not only the first.
func Address(body string) string {
	m := addressRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// Tags extracts the end-of-turn tags present anywhere in body, lower-cased, deduplicated, in order.
func Tags(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range tagRe.FindAllStringSubmatch(body, -1) {
		t := strings.ToLower(m[1])
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// HasTag reports whether tags contains tag.
func HasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Command is a parsed "!verb args" line.
type Command struct {
	Verb string
	Args []string
	// Flags holds --key value pairs; bare --key is "true".
	Flags map[string]string
}

// ErrNotCommand means the body does not start with "!".
var ErrNotCommand = errors.New("not a command")

// ParseCommand parses "!verb a b --k v". Only the first line is read.
func ParseCommand(body string) (*Command, error) {
	line, _, _ := strings.Cut(strings.TrimSpace(body), "\n")
	if !strings.HasPrefix(line, "!") {
		return nil, ErrNotCommand
	}
	fields := strings.Fields(line[1:])
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	c := &Command{Verb: strings.ToLower(fields[0]), Flags: map[string]string{}}
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		if !strings.HasPrefix(f, "--") {
			c.Args = append(c.Args, f)
			continue
		}
		key := strings.TrimPrefix(f, "--")
		if k, v, ok := strings.Cut(key, "="); ok {
			c.Flags[k] = v
			continue
		}
		if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
			c.Flags[key] = fields[i+1]
			i++
		} else {
			c.Flags[key] = "true"
		}
	}
	return c, nil
}
