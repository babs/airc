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

## 2026-09-03 Live status is polled, not posted

Feedback while testing: nothing tells the operator that an agent woke up until its reply lands, sometimes minutes later. Two ways to fix it: post "waking @x" system lines on the bus, or expose the driver's in-memory state. Bus lines would land in every agent's next prompt (system messages are part of the delta) and clutter the log for good. So `GET /status` exposes running/idle/queued per agent and the UI polls it every 2 s; state transitions are shown as ephemeral lines in the message list, client-side only. `!status` reads the same source and shows elapsed time and the trigger id.

## 2026-09-03 Spawn conveniences

`!spawn` accepts `--home` (per-agent `CLAUDE_CONFIG_DIR` / `CODEX_HOME`), `--session` (attach an existing Claude session or Codex thread), a nick-derived provider, and `~` in paths. All came from the first hands-on session: the operator typed what a shell would accept. Usage strings are wrapped in backticks because the UI renders markdown and DOMPurify strips bare `<nick>` as an HTML tag.

## 2026-09-03 Review by codex-review over the bus: in-flight turns and bounded reads

First review done by a peer agent through airc itself. Accepted and fixed:
- Delta and reset prompts used the oldest N rows; past `ContextLimit` the trigger fell off the end while the cursor moved past it. `store.Latest` keeps the newest N. Rescan pages through the channel instead of stopping at 1000 rows.
- A crash after the turn claim lost the wake for good (rollback only ran on graceful shutdown). The agent row now keeps `inflight_prev`, the cursor before the running turn; startup rolls back every row still in flight and parks the agent until `!release` (the provider may have finished the work before the crash). Rollback is conditional on the in-flight flag, so a stale rollback after a reset `!spawn` cannot move the cursor, and finishing a turn only writes the session id if the operator did not attach another one meanwhile.
- A full per-agent queue parked nothing; the overflow is parked like a guard pause and `!release` replays it.
- The README overstated the restart guarantee; reworded once it became true.

Rejected: "live status must go through the bus". Status is ephemeral by design, see the entry above; AGENTS.md now says so explicitly.

## 2026-09-03 Claude rw agents run in `auto` permission mode

`acceptEdits` covered edits but every Bash call outside the `--tools` list was denied, and the first hands-on session produced a denial per turn (heredoc python, ad-hoc shell). The operator asked for `auto` by default: Claude Code's classifier approves routine commands and still refuses dangerous ones, which is what an unattended agent needs. `bypassPermissions` stays excluded. Override with `AIRC_CLAUDE_MODE=acceptEdits` for a stricter agent; `ro` agents keep `default` (no edits, no prompt).

## 2026-09-03 Codex rw agents use the automatic approval reviewer

Same request as for Claude. `codex exec --approve-for-me` routes approval requests through Codex's own reviewer instead of failing them; the flag is absent on `exec resume`, so the driver sets what it configures: `approval_policy="on-request"`, `approvals_reviewer="auto_review"` (keys read from the binary, accepted under `--strict-config`), plus `sandbox_workspace_write.network_access=true` so `say` (curl to localhost) works. `ro` agents keep `never` and no network. `AIRC_CODEX_STRICT=1` restores the old behaviour.

## 2026-09-03 Turn claim, finish and crash recovery (second Codex review pass)

Three windows in the recovery state machine, all accepted:
- The claim was an unconditional `UPDATE ... WHERE nick`. A `!stop` or a respawn to another channel between the agent read and the claim let a stale trigger advance the new configuration's cursor. `ClaimTurn` is now one conditional update (enabled, same channel, idle, cursor behind the trigger) and a failed claim drops the trigger. Store-level test for each refusal, driver-level test for the respawn-while-queued case.
- Finishing the turn and storing the reply were two writes. A crash in between consumed the wake without a reply; the reverse order would rerun a posted turn. `FinishTurn` now does both in one transaction and the bus only broadcasts afterwards.
- Automatic replay of an in-flight turn after a crash could rerun a turn whose provider had already completed (edits, commits). Recovery now rolls the cursor back and parks the wake with a `[waiting for human]` notice; `!release` reruns it. Restart robustness is kept, the decision to rerun is the operator's.

## 2026-09-03 Auto-restart on update

Every rebuild during the first hands-on session needed a manual restart, timed to not kill a running turn. With `AIRC_AUTO_RESTART=1` the process stats its own executable every 2 s; a change that stays stable for one more tick (build finished) and an idle driver (no running turn, empty queues) end the servers cleanly and `syscall.Exec` the new binary in place: same pid, environment and arguments, so the token and database follow. Every response carries an `X-Airc-Boot` id; the UI reloads when it changes. Wakes parked in memory are lost by the restart, but their cursors were never advanced, so the startup rescan re-delivers them.

## 2026-09-03 Second review round over the bus: claim generation and a persisted recovery block

- A respawn on the same channel (new provider, cwd, sandbox, tools) or a `--session` attach between the agent read and the turn claim ran the turn with the configuration read before. The agent row carries `gen`, bumped by every operator mutation; `ClaimTurn` requires the generation it read. `FinishTurn` still stores the reply of a turn that ran during a reset: the work happened and hiding its output would be worse than showing it.
- The crash-recovery block was an in-memory flag consumed by the first replayed wake; a second persisted wake, or a live one, ran the agent past the parked trigger with the ambiguous work inside its delta. The block is now a persisted setting checked by `turn()` on every wake; `!release` clears it, a reset `!spawn` clears it, a second crash keeps it. Test: two pre-crash addresses plus one live address, all parked, three turns after release.
- README and the previous entry promised an automatic replay after a crash; reworded to the parked behaviour.

## 2026-09-03 Third round over the bus: stale finishes, atomic recovery, fail-closed block

- My `FinishTurn` rejection did not hold. On a first turn the old session is empty; a reset respawn during the turn leaves the row's session empty too, so a CASE on the old value matched and wrote the previous provider's session into the replacement row. `FinishTurn` is now conditional on the claim generation and the in-flight flag. A stale turn is rolled back and its reply is stored as a system audit line addressing nobody: the work stays visible, nothing is written to the replacement configuration, no peer is woken. Store test covers the empty-session reset; the driver test for a respawn to another channel now expects the audit line.
- `RecoverInflight` wrote the rollback and the block as two statements; a crash between them left an ordinary pending wake that the next start would rerun. One transaction per agent now. Atomicity is by construction, not by a failure-injection test: the store has no hook to fail between two statements and adding one for the test alone was not worth it.
- `turn()` ignored an error reading the recovery block and ran. It parks the wake instead, with a `[blocked]` notice naming the error. No test: the store cannot be made to fail on demand.

## 2026-09-03 Fourth round: a stale success consumes its trigger

Rolling back a stale but successful turn put the completed request back into the next delta; with an `rw` agent that duplicates edits or commits. A stale finish now only clears the in-flight marker (or leaves a reset row untouched) and keeps the audit line. Store test checks the next delta holds only the audit line; driver test bumps the generation mid-turn (same channel, new sandbox) and checks the next prompt excludes the completed request and uses the new sandbox.

## 2026-09-03 Channel topology and self-declared activity in the prompt preamble

Each turn's protocol now opens with who is in the channel: every peer with provider, sandbox, live state, turn count, the first line of its last message, and the humans seen. Agents declare their own activity with a `[status: ...]` line in any message (final reply or mid-turn `say`), parsed by the driver into the agent row; a convention rather than an endpoint so read-only Codex agents without network can declare too. Shown as `doing: "..."` in the topology and next to the nick in the UI. Advisory: an agent may ignore it; state and last message are the floor.

## 2026-09-03 Two writers on one tree

While the operator tested, the `claude-airc` agent (a resumed copy of the main Claude session, `rw` on this repo) ran its own review/fix loop with `codex-review` and landed a transactional turn claim (`ClaimTurn`/`FinishTurn`, `inflight_prev`, `gen`) in the same uncommitted tree the main session was editing. Nothing was lost, but only because the edits touched different lines. Rule from now on: one writer per worktree; a driven agent gets its own worktree, never the checkout a human session is editing.

## 2026-09-03 A syntax gate for the UI file

A `let boot` added for the reload-on-restart feature collided with the existing `boot()` function; the whole inline script died and the page went blank, and my check had only exercised the header with curl. `make test` now parses every inline `<script>` with node (`make ui-check`), fails the build on a syntax error, and the gate was proven red on the duplicate declaration before being kept.

## 2026-09-03 A channel is a task

`!cwd <path>` and `!topic <title>` are per-channel settings. `!spawn <nick>` then needs only the nick: provider from the nick prefix, channel from where the command is typed, cwd from the channel. The remaining positional arguments are recognised by shape (a known provider, a path, else a channel name), so nothing has to be remembered in order. The sidebar shows, per channel, its title (explicit topic, else the first non-command message), the last speaker with the start of the message, and a blinking marker when an agent is running there. Width is draggable and remembered per browser.

## 2026-09-03 Cross-channel addressing, IRC style

`@channel:nick` from any channel relays the message into that channel, addressed to the bare nick, with a "(relayed from X)" header and a note back in the source. The agent is woken where it lives, so its delta contains the message and its reply lands next to its context; the earlier rule (an agent is woken only from its own channel) is kept, the relay is what crosses. Agents may use it too, through `say`, for cross-task delegation. Prompt preambles now list agents of other channels in that form, plus the workspace and the lobby.

## 2026-09-03 UI layout

Operator's control room, not a consumer chat: a graphite palette (no tinted black), one type family, a sidebar with brand, channels (title, last speaker and snippet, running marker) and identity at the bottom; a channel header with agent chips whose dot blinks while a turn runs and shows the declared activity; a timeline with a colour rail per role (human, claude, codex, driver) and consecutive messages grouped, broken by an address, a tag, a command or a 5 min gap. Enter sends, Shift+Enter breaks a line.

## 2026-09-03 No JS framework

Asked after a hover-card bug (the card anchored at 0,0). The cause was a detached DOM node after a re-render, a bug I would have written in any framework's escape hatch too. One hand-written file, no build step, no dependency churn, and a syntax gate in `make test` is the right size for this tool. If the UI ever grows real state (several views, forms, undo), the step is a small runtime library without a bundler, not React and a toolchain.

## 2026-09-03 Re-spawn merges, rw by default

`!spawn` on an existing nick used to rebuild the whole configuration from the flags given, so `!spawn x --sandbox ro` silently dropped the tools and model. Now only what is given changes; `--tools none` clears the list. New agents default to `rw` (Claude `auto`, Codex automatic review) because that is what every hands-on spawn asked for; reviewers get `--sandbox ro`. A running turn keeps the settings it started with; the next one picks up the change.
