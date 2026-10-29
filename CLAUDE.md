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

- `internal/api`: routes on `http.ServeMux` (including the attempt routes `GET /v1/events/{id}/attempts`, `GET /v1/endpoints/{id}/attempts`, `GET /v1/attempts/{id}`), bearer API key auth (key hash → account; every query is scoped by account, other accounts' objects answer 404), JSON errors `{"error":{"code","message"}}`. `api.WithAdmin` routes `/` and `/admin/…` to the admin UI.
- `internal/admin`: the admin UI under `/admin`, mounted only when `HOOKD_ADMIN_UI` is on. Server-rendered `html/template` pages and plain forms, embedded templates and static files, `http.CrossOriginProtection` on every route, no sign-in (a notice on every page). It calls the Store directly with the same checks as the API. Also the built-in test receiver, `POST /admin/test-receiver/{trcv_id}`, which verifies with the official library and stores requests in the database.
- `internal/settings`: the run-time settings manager. Effective config = environment + overrides from the `settings` table (eight editable settings, `HOOKD_RETRY_SCHEDULE` included; `HOOKD_RETENTION` is environment-only), checked by `config.FromLookup`, reloaded every 5 s; consumers call `Current()` and never keep a copy. Owns the `slog.LevelVar`.
- `internal/validate`: the URL, description, event type, idempotency key and payload checks shared by `internal/api` and `internal/admin`.
- `internal/store`: one `Store` interface, one SQL implementation with a small dialect layer for SQLite (local) and PostgreSQL (production); embedded migrations per dialect (`0001_init`, `0002_admin`, `0003_retries`). The `deliveries` table is the work queue: workers claim due rows (`next_attempt_at <= now`) with a lease (`FOR UPDATE SKIP LOCKED` on PostgreSQL). The `attempts` table holds one row per attempt; `RecordAttempt` writes it and the delivery's next step in one transaction. Disabling or deleting an endpoint makes its waiting deliveries due at once.
- `internal/delivery`: worker pool woken by the API and the admin UI after each event and polling as a fallback; it reads worker count, poll interval, timeout (default 15 s), lease and retry schedule from the settings manager on the fly. No redirects. A failed attempt n is retried after `RetrySchedule[n-1]` ±10% jitter (default `5s,5m,30m,2h,5h,10h,10h`, 8 attempts); a waiting retry is a `pending` row with a later `next_attempt_at`, not a goroutine. A disabled or deleted endpoint ends it with `endpoint_disabled`.
- `internal/retention`: the retention runner started by `serve`: one minute after start, then hourly, `Store.DeleteExpiredEvents` in batches of 500 removes events older than `HOOKD_RETENTION` (default 30 days) with their deliveries and attempts, skipping events with unfinished deliveries; safe on several instances (`SKIP LOCKED`).
- `internal/signing`: the Standard Webhooks signer. `internal/ids`: prefixed ULIDs (`att_` for attempts), secrets, API keys. `internal/config`: every `HOOKD_*` setting and its default, including `HOOKD_ADMIN_UI`, `HOOKD_ADMIN_RECEIVER_URL`, `HOOKD_RETRY_SCHEDULE` and `HOOKD_RETENTION` (which must exceed the idempotency window and the schedule's total plus 10%).
- `cmd/testreceiver`: dev-only receiver that verifies with the official library (the admin UI's built-in receiver does the same job in-process).

User docs: `README.md` (local setup, in the admin UI and with the API), `docs/overview.md`, `docs/configuration.md`, `docs/deployment.md`, `docs/testing.md`, `docs/api.md`. Requirements and design for each change are under `docs/changes/`.

## Conventions & Patterns

_Add your project-specific conventions here_
