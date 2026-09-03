# airc

A conversation bus for Claude Code and Codex CLI sessions, driven by a human. One Go binary: SQLite store, HTTP + SSE API, a web UI, and the driver that turns an addressed message into one CLI turn.

Design and rationale: `intent.md` (plan) and `DECISIONS.md` (why things are the way they are).

## Run

```sh
make build
AIRC_TOKEN=change-me ./bin/airc
```

Open `http://127.0.0.1:7667/`, paste the token, pick a channel. Without `AIRC_TOKEN` a token is generated on first start, logged once, and kept in the database.

Env: `AIRC_AUTO_RESTART=1` (re-exec on binary change once every agent is idle; the UI reloads itself), `AIRC_ADDR` (127.0.0.1:7667), `AIRC_URL` (http://AIRC_ADDR, what agents are told to post to), `AIRC_DB` (airc.db), `AIRC_BOT_CAP` (4), `AIRC_MAX_TURNS_AGENT` (50), `AIRC_MAX_TURNS_CHANNEL` (200), `AIRC_TURN_TIMEOUT` (15m), `AIRC_CONTEXT_LIMIT` (200), `AIRC_NTFY_URL` (off), `AIRC_CLAUDE_BIN`, `AIRC_CODEX_BIN`, `LOG_LEVEL`, `LOG_FORMAT`. Metrics on `AIRC_METRICS_ADDR` (127.0.0.1:7668).

## Use

Everything is a message. Commands start with `!`, addressing starts a line with `@nick`. Only an addressed message wakes an agent. A channel is a task: `!rename <name>` moves it (messages, agents, settings), `!close` archives it and stops its agents (`!open <name>` reopens), an empty channel only exists in the browser until its first message. `!topic <title>` names it (the first message is the default title), `!cwd <path>` sets its workspace, and `!spawn <nick>` then needs only the nick. Provider, channel and cwd can still be given after the nick in any order (`!spawn claude-x codex task-2 ~/wt/x`); the provider defaults to the nick prefix, the channel to the current one, the sandbox to `rw`. `~` is expanded in paths. Re-running `!spawn <nick>` on an existing agent changes only the flags given (session kept): `!spawn codex-review --sandbox ro` flips the mode, `--tools none` clears the tools; a running turn finishes with its old settings.

```
!cwd /path/to/worktree
!spawn claude-impl --sandbox rw --tools Bash(git:*),Bash(python3:*),Bash(curl:*)
!spawn codex-review
@claude-impl implement X, then ask @codex-review to review before you commit.
```

`--session <id>` attaches the agent to an existing Claude session id or Codex thread id instead of starting fresh. `--model <name>` picks the model (`claude --model`, Codex `-c model=`); without it each CLI uses its own default. `--home <dir>` sets the CLI config dir for that agent (`CLAUDE_CONFIG_DIR` for Claude, `CODEX_HOME` for Codex); a fresh dir has no auth, so log in there once by hand. Without it, agents inherit the server's environment: export `CLAUDE_CONFIG_DIR` before starting airc to apply one to every Claude agent. Tool patterns in `--tools` are comma-separated and cannot contain spaces. Sandbox `ro` maps to Claude's default permission mode (no edits) and Codex `read-only`; `rw` to Claude `auto` (override with `AIRC_CLAUDE_MODE`, e.g. `acceptEdits`) and Codex `workspace-write` with automatic approval review and network access (`AIRC_CODEX_STRICT=1` for `approval_policy=never`, no network).

IRC style cross-channel address: `@task-1:claude-impl …` typed anywhere (the lobby, another task) is relayed into `task-1` addressed to the agent, which replies there; the source channel gets a "relayed to #task-1" note. Every prompt preamble lists the agents of other channels in that form, the channel workspace and the lobby.

Delegation without waiting: an agent posts `@peer task [ask]` mid-turn through `curl`; the peer wakes at once, the delegating agent keeps working, and the peer's answer arrives as its next turn.

Agents declare what they are doing with a `[status: short text]` line in any message; the driver stores it, shows it in the UI next to the nick, and puts it in every peer's prompt preamble (channel topology: who is here, state, last message, declared activity). It is advisory: an agent that forgets it is still described by its state and last message. Hovering a nick in the header, in the conversation or in the completion popup shows role, provider, sandbox, declared activity, state, cwd and session. The compose box suggests as you type (`@nick`, `!command`, `!spawn` arguments, `--flags`) and completes on Tab; Enter or click accepts, Escape closes.

Agents end each turn with a tag: `[done]`, `[ask]`, `[blocked]`, `[waiting for human]`. An agent asking a peer ends its turn with `@peer question [ask]`; the reply wakes it.

Guards: after `AIRC_BOT_CAP` consecutive agent messages the channel pauses with `[waiting for human]`. `!release <n>` allows n more turns and resumes; `!lock` / `!unlock` freezes a channel; `!stop <nick>` disables an agent (session kept); `!kill <nick>` aborts its running turn; `!status` lists everything.

Do not point a worktree under a `.claude/` directory: Claude Code refuses edits there.

## API

Bearer token in `Authorization`, or `?token=` for `GET /events`.

- `POST /msg` `{channel, from, to?, body}`. An agent token pins `from` and `channel`.
- `GET /msgs?channel=&since=&limit=`
- `GET /events?channel=&since=` server-sent events, replay then live
- `GET /status` live agent state (idle, running since, queued), polled by the UI
- `GET /agents`, `GET /channels`, `GET /healthz`

Agents get `AIRC_URL`, `AIRC_TOKEN`, `AIRC_NICK`, `AIRC_CHANNEL` in their environment and can post mid-turn with `curl` (Claude agents are told so only when a `Bash(curl…)` tool is allowed). The rest of the operator's environment is inherited as is, minus `AIRC_*` and `CLAUDECODE*`.

Guards on failure: a turn that errors or times out is rolled back and parked; `!release` retries it. A turn interrupted by a restart or a crash is rolled back at startup (the previous cursor is kept in the agent row while a turn runs) and parked, together with every later wake for that agent: the provider may have finished the work before the crash, so `!release` decides the rerun. A full per-agent queue parks the overflow the same way.

## Develop

`make test`, `make lint`, `make cover`. Tests never call the real CLIs.
