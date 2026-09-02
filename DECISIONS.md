# Decisions log

Why things are the way they are. Newest at the bottom. Verified facts are marked as such; the rest is a choice that can be revisited.

## 2026-09-03 Transport: HTTP + SQLite in one binary, not IRC

IRC brought ready-made clients and nothing else the v1 needs. It cost 512-byte lines, flood limits, SASL, one connection per nick, and a paste service for long outputs. All of that disappears with a single Go process holding the bus (SQLite + SSE) and the driver. See `intent.md` §1.

## 2026-09-03 Phase 0: what the CLIs actually do (verified by running them)

Claude Code 2.1.259:
- `claude -p --output-format json --session-id <uuid>` creates the session with that id; `--resume <uuid>` continues it. JSON carries `session_id`, `result`, `permission_denials`, `usage`, `total_cost_usd`.
- `--append-system-prompt` is applied again on every resumed turn (a `[done]` tag rule was honored on turn 2 and 3).
- `--allowedTools` is additive to the allow list in `~/.claude/settings.json`. A global `Bash(ls:*)` leaked into a turn that only allowed `Bash(echo:*)`. The driver cannot fully isolate an agent from the operator's own permission config. Accepted for the POC; `CLAUDE_CONFIG_DIR` would be the fix.
- A tool outside the list is denied silently and reported in `permission_denials`, never prompted.
- `CLAUDECODE` must be removed from the environment when spawning from inside a Claude Code session.

Codex CLI 0.152.1:
- First turn: `codex exec --json -C <dir> -s read-only|workspace-write --skip-git-repo-check -c approval_policy="never" <prompt>`. The thread id arrives in the `thread.started` event.
- Later turns: `codex exec resume <thread_id> --json -c sandbox_mode="..." -c approval_policy="never" <prompt>`. `resume` rejects `-s` and `-C`, so sandbox goes through `-c`.
- The final reply is the last `agent_message` item; a turn can emit several.
- stdin must be `/dev/null`, otherwise codex waits on it ("Reading additional input from stdin...").
- Read-only sandbox is enforced (file creation failed with "read-only file system").

## 2026-09-03 Protocol injection

Claude gets the protocol through `--append-system-prompt`. Codex has no equivalent flag on `exec`, so the protocol is prefixed to the prompt. Writing an `AGENTS.md` into the agent's worktree was rejected: it pollutes a tree the user owns.

## 2026-09-03 Auth: one admin token, one token per agent

A shared token would let any agent post as any nick. Each `!spawn` generates an agent token that pins `from` and `channel` server-side. The admin token (`AIRC_TOKEN`) can post as anyone and run `!` commands. Agents receive their token in the environment of their subprocess only.

## 2026-09-03 Addressing

`to` is an explicit field, or derived from a body whose first line starts with `@nick`. One rule for humans (UI) and agents (their reply text). Only an addressed message wakes an agent.

## 2026-09-03 No Dockerfile

The binary has to run next to the `claude` and `codex` CLIs, their auth, and the worktrees. A container would ship none of that. Waived, not forgotten.
