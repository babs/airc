# airc

A conversation bus for Claude Code and Codex CLI sessions, driven by a human. One Go binary: SQLite store, HTTP + SSE API, a web UI, and the driver that turns an addressed message into one CLI turn.

Design and rationale: `intent.md` (plan) and `DECISIONS.md` (why things are the way they are).

## Run

```sh
make build
AIRC_TOKEN=change-me ./bin/airc
```

Open `http://127.0.0.1:7667/`, paste the token, pick a channel. Without `AIRC_TOKEN` a token is generated on first start, logged once, and kept in the database.

Env: `AIRC_ADDR` (127.0.0.1:7667), `AIRC_URL` (http://AIRC_ADDR, what agents are told to post to), `AIRC_DB` (airc.db), `AIRC_BOT_CAP` (4), `AIRC_MAX_TURNS_AGENT` (50), `AIRC_MAX_TURNS_CHANNEL` (200), `AIRC_TURN_TIMEOUT` (15m), `AIRC_CONTEXT_LIMIT` (200), `AIRC_NTFY_URL` (off), `AIRC_CLAUDE_BIN`, `AIRC_CODEX_BIN`, `LOG_LEVEL`, `LOG_FORMAT`. Metrics on `AIRC_METRICS_ADDR` (127.0.0.1:7668).

## Use

Everything is a message. Commands start with `!`, addressing starts a line with `@nick`. Only an addressed message wakes an agent.

```
!spawn claude-impl claude task-1 /path/to/worktree --sandbox rw --tools Bash(git:*),Bash(python3:*),Bash(curl:*)
!spawn codex-review codex task-1 /path/to/worktree --sandbox ro
@claude-impl implement X, then ask @codex-review to review before you commit.
```

Tool patterns in `--tools` are comma-separated and cannot contain spaces. Sandbox `ro` maps to Claude's default permission mode (no edits) and Codex `read-only`; `rw` to `acceptEdits` and `workspace-write`.

Agents end each turn with a tag: `[done]`, `[ask]`, `[blocked]`, `[waiting for human]`. An agent asking a peer ends its turn with `@peer question [ask]`; the reply wakes it.

Guards: after `AIRC_BOT_CAP` consecutive agent messages the channel pauses with `[waiting for human]`. `!release <n>` allows n more turns and resumes; `!lock` / `!unlock` freezes a channel; `!stop <nick>` disables an agent (session kept); `!kill <nick>` aborts its running turn; `!status` lists everything.

Do not point a worktree under a `.claude/` directory: Claude Code refuses edits there.

## API

Bearer token in `Authorization`, or `?token=` for `GET /events`.

- `POST /msg` `{channel, from, to?, body}`. An agent token pins `from` and `channel`.
- `GET /msgs?channel=&since=&limit=`
- `GET /events?channel=&since=` server-sent events, replay then live
- `GET /agents`, `GET /channels`, `GET /healthz`

Agents get `AIRC_URL`, `AIRC_TOKEN`, `AIRC_NICK`, `AIRC_CHANNEL` in their environment and can post mid-turn with `curl` (Claude agents are told so only when a `Bash(curl…)` tool is allowed). The rest of the operator's environment is inherited as is, minus `AIRC_*` and `CLAUDECODE*`.

Guards on failure: a turn that errors or times out is rolled back and parked; `!release` retries it. A turn interrupted by a server restart is replayed at startup.

## Develop

`make test`, `make lint`, `make cover`. Tests never call the real CLIs.
