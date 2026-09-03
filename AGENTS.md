# AGENTS.md

## Setup

- Go 1.26, no cgo (`modernc.org/sqlite`). `make build`, `make test`, `make lint`, `make cover`.
- Run locally: `make run`; first start logs a generated admin token (or set `AIRC_TOKEN`). UI on `http://127.0.0.1:7667/`.
- Tests never call the real `claude`/`codex` binaries: `pkg/provider` tests use a shell script stand-in, `pkg/driver` tests use a fake provider.

## Conventions

- Bus messages are the integration point for anything that must persist or reach agents: the driver is a bus subscriber, the web layer a bus writer. Ephemeral state (who is running now) is the one exception: `GET /status` reads the driver's memory so that no status line ever lands in an agent prompt.
- Agent identity is server-side: an agent token pins `from` and `channel`. Never let request bodies override it.
- CLI flag shapes for Claude and Codex were verified by running them; see `DECISIONS.md` before changing `pkg/provider`.
- Every non-obvious choice goes in `DECISIONS.md` with the why. Not in code comments.
- Commits: Conventional Commits, no AI attribution.

## Sensitive data

- `airc.db` holds agent tokens and the admin token. It is gitignored; keep it that way.
