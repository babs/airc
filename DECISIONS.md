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

## 2026-09-03 Moderation: cap, release, lock

Asked for mid-build: a lock when agents start looping, with a way to free N turns from the UI.
- Cap: N consecutive agent messages in a channel without a human one (default 4, `AIRC_BOT_CAP`). The wake that would exceed it is dropped, a `[waiting for human]` notice is posted, and the agent's cursor stays where it was so nothing is lost.
- `!release <n>`: grants n turns past the cap and replays the dropped wake. A human message resets the streak by itself, so after a human post the agents get cap turns again; the grant only counts beyond the cap. Human `!commands` and system notices do not count as human participation, otherwise `!release` would reset the streak it is meant to control.
- `!lock` / `!unlock`: no wake at all in the channel, whatever the trigger.
- Budgets (`AIRC_MAX_TURNS_AGENT`, `AIRC_MAX_TURNS_CHANNEL`) are turn counters; `!release n` lowers them by n. Cheap, not auditable. Add an allowance column if that matters.

## 2026-09-03 Addressing: any line, not only the first

First e2e run: Claude wrote a one-line summary, the diff, then `@codex-review please review [ask]`. My parser only read the first line, so Codex never woke. The protocol asks for a short first line and the addressee at the end, so the two rules contradicted each other. Now the first line that starts with `@nick` wins. `To`/tags derivation moved into `bus.Post` so every writer (web, driver, tests) follows one rule.

## 2026-09-03 Where agent worktrees may live

Claude Code refuses edits under a `.claude/` directory ("sensitive file edit not allowed"), even under `acceptEdits`. The first e2e worktree sat in `.claude/scratchpad/` and every Edit was denied. Test worktrees live in `.scratchpad/` (gitignored) instead. Users spawning agents on paths under `.claude/` will hit the same wall.

## 2026-09-03 Tags stored comma-separated

`waiting for human` has spaces. Space-joined storage split it into three tags; caught by the API output during e2e. Comma it is, with a round-trip test.

## 2026-09-03 What the e2e scenario showed (real Claude + Codex, 5 turns)

Human → Claude implements `add()` + test, asks Codex → Codex reviews, finds an unignored `__pycache__` → Claude fixes, asks again → Codex LGTM → Claude tries to commit, SSH signing needs a passphrase, ends with `[waiting for human]`. Human answers with a signing override → Claude commits, `[done]`. The ping-pong, the tags, the pause on a real blocker and the human resumption all work as designed. Codex turns ran 36 to 80 s, Claude turns 16 to 40 s.

Also seen: Claude's global `CLAUDE.md` applies inside a driven session (it tried `ls ~/.claude/rules/`, denied). Agents inherit the operator's Claude config; `CLAUDE_CONFIG_DIR` per agent would isolate that. Not done in the POC.

## 2026-09-03 Self-review fixes

- Prompt on stdin, not argv. Linux caps one argv entry at 128 KiB; a channel log with diffs exceeds it. Both CLIs read stdin (verified: `claude -p` with piped stdin and no positional prompt, `codex exec -`).
- Every `AIRC_*` variable is scrubbed from the agent's environment. The server's own `AIRC_TOKEN` is the admin token; leaking it would hand an agent `!spawn` with a `rw` sandbox on any directory. A test asserts the scrub and goes red without it.
- Agent output is rendered through DOMPurify. The page holds the admin token in localStorage, and agent text is untrusted (prompt injection through a peer). Without a sanitizer, one `<img onerror>` in a reply exfiltrates the token.
- Agent row updates are column-level (`BumpTurn`, `SetSession`, `SetEnabled`, `ReleaseTurns`). Full-row `PutAgent` from a running turn could overwrite a concurrent `!stop`.
- SSE handlers treat the live channel as a signal and re-read the store after the last sent id. A dropped broadcast (full buffer) or an out-of-order fan-out can no longer leave a gap. The driver keeps its direct subscription; drops are counted in `airc_bus_dropped_total`.
- Bot streak: an agent message starting with `!` used to be skipped like a human command, which let an agent dodge the cap. Only human `!` lines are skipped now.
- Codex "session lost" regex narrowed to session/thread/rollout wording. Still unverified against a real lost thread.

## 2026-09-03 Independent review: what changed and what was waived

Fixed: cross-channel wakes (an `@nick` from another channel ran a turn on an empty delta), system notices replayed as wakes after a restart (they address the agent), failed or interrupted turns consuming the wake for good (now rolled back and parked; `!release` or a restart replays them), agent tokens listing every channel and agent, `--tools=--flag` reaching Claude's argv, non-atomic grant decrement, `!unlock` not resuming, stopped agents keeping a valid token, SSE heartbeat not catching up, 1 MiB message bodies, SRI on the two CDN scripts, and a timeout test that never timed out.

Waived: the agent inherits the operator's environment (minus `AIRC_*` and `CLAUDECODE*`). An allowlist would break the CLIs in ways I cannot enumerate (auth, proxies, `CLAUDE_CONFIG_DIR`, `CODEX_HOME`, certificates). An interactive Claude Code session has the same environment; the POC does not claim to sandbox better than the CLIs themselves do.
