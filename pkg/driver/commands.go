package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"airc/pkg/proto"
	"airc/pkg/provider"
	"airc/pkg/store"
)

// Angle brackets live inside backticks: the UI renders markdown and would strip them as HTML.
const spawnUsage = "`!spawn <nick> [claude|codex] [channel] [cwd] [--sandbox ro|rw] [--tools a,b] [--home <config-dir>] [--session <id>] [--model <name>]` (provider defaults to the nick prefix, channel to the current one, cwd to the channel's `!cwd`; --session resumes an existing Claude session / Codex thread)"

const help = "Commands: " + spawnUsage + ", `!cwd [path]` (channel workspace), `!topic [title]`, `!rename <new-name>`, `!close` / `!open <channel>`, `!stop <nick>`, `!kill <nick>`, `!status`, `!release [n]`, `!lock`, `!unlock`, `!help`"

// errNoReply means the command answered elsewhere (a renamed channel).
var errNoReply = errors.New("no reply")

func (d *Driver) command(ctx context.Context, m store.Message) {
	cmd, err := proto.ParseCommand(m.Body)
	if err != nil {
		d.system(ctx, m.Channel, "bad command: "+err.Error()+". "+help)
		return
	}
	reply, err := d.exec(ctx, m.Channel, cmd)
	if errors.Is(err, errNoReply) {
		return
	}
	if err != nil {
		reply = "error: " + err.Error()
	}
	d.system(ctx, m.Channel, reply)
}

func (d *Driver) exec(ctx context.Context, channel string, c *proto.Command) (string, error) {
	switch c.Verb {
	case "spawn":
		return d.spawn(ctx, channel, c)
	case "rename":
		if len(c.Args) != 1 || !proto.ValidNick(c.Args[0]) {
			return "", errors.New("usage: `!rename <new-name>` (lowercase, digits, dashes)")
		}
		if err := d.bus.Store.RenameChannel(ctx, channel, c.Args[0]); err != nil {
			return "", err
		}
		// Parked wakes are keyed by channel in memory: follow the rename or !release loses them.
		d.mu.Lock()
		for i := range d.paused[channel] {
			d.paused[channel][i].Channel = c.Args[0]
		}
		d.paused[c.Args[0]] = append(d.paused[c.Args[0]], d.paused[channel]...)
		delete(d.paused, channel)
		d.mu.Unlock()
		// The reply goes to the new name: the old one no longer exists.
		d.system(ctx, c.Args[0], fmt.Sprintf("channel %s renamed to %s.", channel, c.Args[0]))
		return "", errNoReply
	case "close":
		agents, err := d.bus.Store.Agents(ctx)
		if err != nil {
			return "", err
		}
		n := 0
		for _, a := range agents {
			if a.Channel == channel && a.Enabled {
				_ = d.bus.Store.SetEnabled(ctx, a.Nick, false)
				d.cancelRunning(a.Nick) // like !stop: no point finishing a turn whose reply is discarded
				n++
			}
		}
		if err := d.bus.Store.SetSetting(ctx, "archived:"+channel, "1"); err != nil {
			return "", err
		}
		return fmt.Sprintf("channel %s closed: hidden from the list, %d agent(s) stopped. `!open %s` from anywhere reopens it.", channel, n, channel), nil
	case "open":
		if len(c.Args) != 1 || !proto.ValidNick(c.Args[0]) {
			return "", errors.New("usage: `!open <channel>`")
		}
		if err := d.bus.Store.SetSetting(ctx, "archived:"+c.Args[0], "0"); err != nil {
			return "", err
		}
		return fmt.Sprintf("channel %s reopened (its agents stay stopped; `!spawn` them again).", c.Args[0]), nil
	case "topic":
		if len(c.Args) == 0 {
			t, _ := d.bus.Store.Setting(ctx, "topic:"+channel)
			if t == "" {
				return "no explicit topic for " + channel + " (the first message is used). `!topic <title>` to set one.", nil
			}
			return "topic of " + channel + ": " + t, nil
		}
		t := strings.Join(c.Args, " ")
		if err := d.bus.Store.SetSetting(ctx, "topic:"+channel, t); err != nil {
			return "", err
		}
		return "topic of " + channel + " set to: " + t, nil
	case "cwd":
		if len(c.Args) == 0 {
			cwd, _ := d.bus.Store.Setting(ctx, "cwd:"+channel)
			if cwd == "" {
				return "no workspace set for " + channel + ". `!cwd <path>` to set one.", nil
			}
			return "workspace of " + channel + ": " + cwd, nil
		}
		if len(c.Args) != 1 {
			return "", errors.New("usage: `!cwd [path]`")
		}
		cwd, err := absPath(c.Args[0])
		if err != nil {
			return "", err
		}
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("%q is not a directory", cwd)
		}
		if err := d.bus.Store.SetSetting(ctx, "cwd:"+channel, cwd); err != nil {
			return "", err
		}
		return "workspace of " + channel + " set to " + cwd + ". `!spawn <nick>` now defaults to it.", nil
	case "stop":
		return d.setEnabled(ctx, c, false)
	case "kill":
		if len(c.Args) != 1 {
			return "", errors.New("usage: `!kill <nick>`")
		}
		d.mu.Lock()
		r, ok := d.running[c.Args[0]]
		d.mu.Unlock()
		if !ok {
			return fmt.Sprintf("@%s has no running turn.", c.Args[0]), nil
		}
		r.cancel()
		return fmt.Sprintf("killing the running turn of @%s.", c.Args[0]), nil
	case "status":
		return d.status(ctx, channel)
	case "release":
		n := d.cfg.BotCap
		if len(c.Args) == 1 {
			v, err := strconv.Atoi(c.Args[0])
			if err != nil || v <= 0 {
				return "", errors.New("usage: `!release [n>0]`")
			}
			n = v
		}
		replayed, err := d.release(ctx, channel, n)
		if err != nil {
			return "", err
		}
		if replayed == 0 {
			return fmt.Sprintf("released %d agent turns in %s. Address an agent to resume.", n, channel), nil
		}
		return fmt.Sprintf("released %d agent turns in %s, resuming %d paused wake(s).", n, channel, replayed), nil
	case "lock":
		if err := d.bus.Store.SetSetting(ctx, "lock:"+channel, "1"); err != nil {
			return "", err
		}
		return "[waiting for human] channel locked: no agent will be woken until `!unlock`.", nil
	case "unlock":
		if err := d.bus.Store.SetSetting(ctx, "lock:"+channel, "0"); err != nil {
			return "", err
		}
		if n := d.replayPaused(ctx, channel); n > 0 {
			return fmt.Sprintf("channel unlocked, resuming %d paused wake(s).", n), nil
		}
		return "channel unlocked.", nil
	case "help":
		return help, nil
	}
	return "", fmt.Errorf("unknown command %q. %s", c.Verb, help)
}

// spawn accepts its positional arguments in any order after the nick: a known provider name, a
// path (contains "/" or starts with "~" or "."), anything else is a channel name. On an existing
// agent only what is given changes, so `!spawn nick --sandbox ro` flips one setting and keeps
// the session, tools and model.
func (d *Driver) spawn(ctx context.Context, current string, c *proto.Command) (string, error) {
	if len(c.Args) < 1 || len(c.Args) > 4 {
		return "", errors.New("usage: " + spawnUsage)
	}
	nick := c.Args[0]
	if !proto.ValidNick(nick) || nick == proto.SystemNick {
		return "", fmt.Errorf("invalid nick %q", nick)
	}
	ag, err := d.bus.Store.Agent(ctx, nick)
	created := errors.Is(err, store.ErrNotFound)
	if err != nil && !created {
		return "", err
	}
	if created {
		// Defaults for a new agent: provider from the nick prefix, this channel, rw (Claude auto,
		// Codex automatic review). Reviewers get --sandbox ro explicitly.
		prov, _, _ := strings.Cut(nick, "-")
		ag = &store.Agent{Nick: nick, Token: newToken(), Provider: prov, Channel: current, Sandbox: "rw"}
	}
	prev := *ag
	var cwd string
	// --channel is the unambiguous form: a channel named like a provider cannot be given positionally.
	if ch, ok := c.Flags["channel"]; ok {
		ag.Channel = ch
	}
	for _, a := range c.Args[1:] {
		switch {
		case strings.ContainsAny(a, "/") || strings.HasPrefix(a, "~") || strings.HasPrefix(a, "."):
			cwd = a
		default:
			if _, ok := provider.Lookup(d.providers, a); ok {
				ag.Provider = a
			} else {
				ag.Channel = a
			}
		}
	}
	if !proto.ValidNick(ag.Channel) {
		return "", fmt.Errorf("invalid channel %q", ag.Channel)
	}
	if _, ok := provider.Lookup(d.providers, ag.Provider); !ok {
		return "", fmt.Errorf("unknown provider %q", ag.Provider)
	}
	if cwd == "" && (created || ag.Channel != prev.Channel) {
		if cwd, _ = d.bus.Store.Setting(ctx, "cwd:"+ag.Channel); cwd == "" {
			return "", fmt.Errorf("no cwd given and no workspace set for %s (`!cwd <path>`)", ag.Channel)
		}
	}
	if cwd != "" {
		if cwd, err = absPath(cwd); err != nil {
			return "", err
		}
		if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
			return "", fmt.Errorf("cwd %q is not a directory", cwd)
		}
		ag.Cwd = cwd
	}
	if home, ok := c.Flags["home"]; ok {
		if home != "" {
			if home, err = absPath(home); err != nil {
				return "", err
			}
			if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
				return "", fmt.Errorf("home %q is not a directory", home)
			}
		}
		ag.Home = home
	}
	if sandbox, ok := c.Flags["sandbox"]; ok {
		if sandbox != "ro" && sandbox != "rw" {
			return "", fmt.Errorf("sandbox must be ro or rw, got %q", sandbox)
		}
		ag.Sandbox = sandbox
	}
	if t, ok := c.Flags["tools"]; ok {
		ag.Tools = nil
		if t != "" && t != "none" {
			ag.Tools = strings.Split(t, ",")
			for _, tool := range ag.Tools {
				// A leading dash would land in the CLI argv as an option (--dangerously-skip-permissions).
				if tool == "" || strings.HasPrefix(tool, "-") {
					return "", fmt.Errorf("invalid tool pattern %q", tool)
				}
			}
		}
	}
	if model, ok := c.Flags["model"]; ok {
		if strings.HasPrefix(model, "-") || strings.ContainsAny(model, " \"'") {
			return "", fmt.Errorf("invalid model name %q", model)
		}
		ag.Model = model
	}
	// A session belongs to one provider and one channel: switching either starts over.
	reset := !created && (ag.Provider != prev.Provider || ag.Channel != prev.Channel)
	ag.Enabled = true
	prov, channel, home, sandbox, model := ag.Provider, ag.Channel, ag.Home, ag.Sandbox, ag.Model
	cwd = ag.Cwd
	var err2 error
	if created {
		err2 = d.bus.Store.PutAgent(ctx, ag)
	} else {
		err2 = d.bus.Store.UpdateAgentConfig(ctx, ag, reset)
	}
	if err2 != nil {
		return "", err2
	}
	if reset {
		// A fresh session has nothing unaccounted for.
		if err := d.bus.Store.SetRecovering(ctx, nick, false); err != nil {
			return "", err
		}
	}
	if sid := c.Flags["session"]; sid != "" {
		if err := d.bus.Store.SetSession(ctx, nick, sid); err != nil {
			return "", err
		}
	}
	verb := "spawned"
	if !created {
		verb = "updated"
	}
	homeNote := ""
	if home != "" {
		homeNote = ", home " + home
	}
	if sid := c.Flags["session"]; sid != "" {
		homeNote += ", resuming session " + short(sid)
	}
	if model != "" {
		homeNote += ", model " + model
	}
	return fmt.Sprintf("%s @%s (%s, %s, cwd %s%s) in %s. Address it with `@%s ...`.", verb, nick, prov, sandbox, cwd, homeNote, channel, nick), nil
}

func (d *Driver) setEnabled(ctx context.Context, c *proto.Command, enabled bool) (string, error) {
	if len(c.Args) != 1 {
		return "", errors.New("usage: `!stop <nick>`")
	}
	ag, err := d.bus.Store.Agent(ctx, c.Args[0])
	if err != nil {
		return "", fmt.Errorf("agent %q: %w", c.Args[0], err)
	}
	if err := d.bus.Store.SetEnabled(ctx, ag.Nick, enabled); err != nil {
		return "", err
	}
	d.cancelRunning(ag.Nick)
	return fmt.Sprintf("@%s stopped (session kept; `!spawn` again to re-enable).", ag.Nick), nil
}

func (d *Driver) status(ctx context.Context, channel string) (string, error) {
	agents, err := d.bus.Store.Agents(ctx)
	if err != nil {
		return "", err
	}
	live, err := d.Status(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	lock, _ := d.bus.Store.Setting(ctx, "lock:"+channel)
	grant, _ := d.bus.Store.Setting(ctx, "grant:"+channel)
	fmt.Fprintf(&b, "channel %s: locked=%v released=%s cap=%d\n\n", channel, lock == "1", orZero(grant), d.cfg.BotCap)
	if len(agents) == 0 {
		b.WriteString("no agents. " + help)
		return b.String(), nil
	}
	b.WriteString("| nick | provider | channel | sandbox | turns | state | session |\n|---|---|---|---|---|---|---|\n")
	for i, a := range agents {
		state := live[i].State
		if state == "running" {
			state = fmt.Sprintf("running %s on #%d", time.Since(live[i].Since).Round(time.Second), live[i].Trigger)
		}
		if live[i].Queued > 0 {
			state += fmt.Sprintf(", %d queued", live[i].Queued)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %s | %s |\n", a.Nick, a.Provider, a.Channel, a.Sandbox, a.Turns, state, short(a.SessionID))
	}
	return b.String(), nil
}

// absPath expands a leading "~" (the shell is not in the loop here) and makes the path absolute.
func absPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = home + p[1:]
	}
	return filepath.Abs(p)
}

func short(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	if s == "" {
		return "-"
	}
	return s
}

func orZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}
