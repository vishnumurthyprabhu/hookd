# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:1105d646 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/core-concepts/sync-concepts.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->


## Build & Test

Go 1.26 or later. No other setup: the SQLite driver is pure Go and tests need no running service.

```bash
go build ./...                       # build everything
go vet ./...                         # static checks
go test ./...                        # all tests, on SQLite
go run ./cmd/hookd serve             # run locally on :8080 with ./hookd.db
go run ./cmd/hookd account create --name acme   # prints an API key
go run ./cmd/testreceiver --secret whsec_...    # verifying receiver on 127.0.0.1:9000
```

The store suite also runs on PostgreSQL with `HOOKD_TEST_PG=1` plus the `HOOKD_DB_*` settings; see `docs/testing.md`. Without it those subtests are skipped.

## Architecture Overview

Hookd is one Go binary (`cmd/hookd`) that takes webhook events over a JSON HTTP API and delivers them, signed under the Standard Webhooks spec, to registered endpoints. The database is the only outside component and doubles as the delivery queue.

- `internal/api`: routes on `http.ServeMux`, bearer API key auth (key hash → account; every query is scoped by account, other accounts' objects answer 404), JSON errors `{"error":{"code","message"}}`.
- `internal/store`: one `Store` interface, one SQL implementation with a small dialect layer for SQLite (local) and PostgreSQL (production); embedded migrations per dialect. The `deliveries` table is the work queue: workers claim rows with a lease (`FOR UPDATE SKIP LOCKED` on PostgreSQL).
- `internal/delivery`: worker pool woken by the API after each event and polling as a fallback; one attempt per delivery, 15 s timeout, no redirects, no retries (piece 1).
- `internal/signing`: the Standard Webhooks signer. `internal/ids`: prefixed ULIDs, secrets, API keys. `internal/config`: every `HOOKD_*` setting and its default.
- `cmd/testreceiver`: dev-only receiver that verifies with the official library.

User docs: `README.md` (local setup), `docs/configuration.md`, `docs/deployment.md`, `docs/testing.md`, `docs/api.md`. Requirements and design for each change are under `docs/changes/`.

## Conventions & Patterns

_Add your project-specific conventions here_
