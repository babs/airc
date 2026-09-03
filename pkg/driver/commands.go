package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"airc/pkg/proto"
	"airc/pkg/provider"
	"airc/pkg/store"
)

const help = "Commands: `!spawn <nick> <claude|codex> <channel> <cwd> [--sandbox ro|rw] [--tools a,b]`, `!stop <nick>`, `!kill <nick>`, `!status`, `!release [n]`, `!lock`, `!unlock`, `!help`"

func (d *Driver) command(ctx context.Context, m store.Message) {
	cmd, err := proto.ParseCommand(m.Body)
	if err != nil {
		d.system(ctx, m.Channel, "bad command: "+err.Error()+". "+help)
		return
	}
	reply, err := d.exec(ctx, m.Channel, cmd)
	if err != nil {
		reply = "error: " + err.Error()
	}
	d.system(ctx, m.Channel, reply)
}

func (d *Driver) exec(ctx context.Context, channel string, c *proto.Command) (string, error) {
	switch c.Verb {
	case "spawn":
		return d.spawn(ctx, c)
	case "stop":
		return d.setEnabled(ctx, c, false)
	case "kill":
		if len(c.Args) != 1 {
			return "", errors.New("usage: !kill <nick>")
		}
		d.mu.Lock()
		cancel, ok := d.running[c.Args[0]]
		d.mu.Unlock()
		if !ok {
			return fmt.Sprintf("@%s has no running turn.", c.Args[0]), nil
		}
		cancel()
		return fmt.Sprintf("killing the running turn of @%s.", c.Args[0]), nil
	case "status":
		return d.status(ctx, channel)
	case "release":
		n := d.cfg.BotCap
		if len(c.Args) == 1 {
			v, err := strconv.Atoi(c.Args[0])
			if err != nil || v <= 0 {
				return "", errors.New("usage: !release [n>0]")
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

func (d *Driver) spawn(ctx context.Context, c *proto.Command) (string, error) {
	if len(c.Args) != 4 {
		return "", errors.New("usage: !spawn <nick> <provider> <channel> <cwd> [--sandbox ro|rw] [--tools a,b]")
	}
	nick, prov, channel, cwd := c.Args[0], c.Args[1], c.Args[2], c.Args[3]
	if !proto.ValidNick(nick) || nick == proto.SystemNick {
		return "", fmt.Errorf("invalid nick %q", nick)
	}
	if !proto.ValidNick(channel) {
		return "", fmt.Errorf("invalid channel %q", channel)
	}
	if _, ok := provider.Lookup(d.providers, prov); !ok {
		return "", fmt.Errorf("unknown provider %q", prov)
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(cwd); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	sandbox := c.Flags["sandbox"]
	if sandbox == "" {
		sandbox = "ro"
	}
	if sandbox != "ro" && sandbox != "rw" {
		return "", fmt.Errorf("sandbox must be ro or rw, got %q", sandbox)
	}
	var tools []string
	if t := c.Flags["tools"]; t != "" {
		tools = strings.Split(t, ",")
		for _, tool := range tools {
			// A leading dash would land in the CLI argv as an option (--dangerously-skip-permissions).
			if tool == "" || strings.HasPrefix(tool, "-") {
				return "", fmt.Errorf("invalid tool pattern %q", tool)
			}
		}
	}

	ag, err := d.bus.Store.Agent(ctx, nick)
	created := errors.Is(err, store.ErrNotFound)
	if err != nil && !created {
		return "", err
	}
	// A session belongs to one provider and one channel: switching either starts over.
	reset := !created && (ag.Provider != prov || ag.Channel != channel)
	if created {
		ag = &store.Agent{Nick: nick, Token: newToken()}
	}
	ag.Provider, ag.Channel, ag.Cwd, ag.Sandbox, ag.Tools, ag.Enabled = prov, channel, cwd, sandbox, tools, true
	var err2 error
	if created {
		err2 = d.bus.Store.PutAgent(ctx, ag)
	} else {
		err2 = d.bus.Store.UpdateAgentConfig(ctx, ag, reset)
	}
	if err2 != nil {
		return "", err2
	}
	verb := "spawned"
	if !created {
		verb = "updated"
	}
	return fmt.Sprintf("%s @%s (%s, %s, cwd %s) in %s. Address it with `@%s ...`.", verb, nick, prov, sandbox, cwd, channel, nick), nil
}

func (d *Driver) setEnabled(ctx context.Context, c *proto.Command, enabled bool) (string, error) {
	if len(c.Args) != 1 {
		return "", errors.New("usage: !stop <nick>")
	}
	ag, err := d.bus.Store.Agent(ctx, c.Args[0])
	if err != nil {
		return "", fmt.Errorf("agent %q: %w", c.Args[0], err)
	}
	if err := d.bus.Store.SetEnabled(ctx, ag.Nick, enabled); err != nil {
		return "", err
	}
	d.mu.Lock()
	cancel, running := d.running[ag.Nick]
	d.mu.Unlock()
	if running {
		cancel()
	}
	return fmt.Sprintf("@%s stopped (session kept; `!spawn` again to re-enable).", ag.Nick), nil
}

func (d *Driver) status(ctx context.Context, channel string) (string, error) {
	agents, err := d.bus.Store.Agents(ctx)
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
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range agents {
		state := "idle"
		if !a.Enabled {
			state = "stopped"
		} else if _, ok := d.running[a.Nick]; ok {
			state = "running"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %s | %s |\n", a.Nick, a.Provider, a.Channel, a.Sandbox, a.Turns, state, short(a.SessionID))
	}
	return b.String(), nil
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
