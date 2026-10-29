# Design: Hookd 1/6 — Signed event delivery

This design covers piece 1 only. It is shaped so that the later pieces (retries, replay, rotation, isolation, network safety) can be added without restructuring. It does not build anything for them yet.

## Options considered

| Option | Fit to requirements | Complexity | Cost to run and maintain |
|---|---|---|---|
| **A. Go single binary, SQL database as the queue** (SQLite locally, PostgreSQL in production) | Good. Durable delivery (N2) comes from the same table that records status (R12). Standard library HTTP client gives timeouts and no redirects (R16, R18). | Low: one process, one store, no framework | One binary plus a database. Cheap. |
| B. Python (FastAPI) + PostgreSQL + Redis/Celery for delivery jobs | Good, but delivery state is split between Redis and the database, and keeping them consistent takes extra work for N2 | Medium: three services, two of them required even locally | Higher: Redis to run, Celery workers to tune |
| C. Node.js/TypeScript + PostgreSQL + BullMQ (Redis) | Good, with the same split-state issue as B | Medium | Higher: Redis to run, and a larger dependency tree |

**Chosen: A.** It is the simplest option that meets every requirement. The database already has to store events and delivery status, so using it as the work queue as well gives durability across restarts without a separate broker. Go's standard library covers the HTTP server, the HTTP client, HMAC and base64, so no web framework is needed. The SQLite driver is pure Go (no cgo), so local runs need nothing installed except Go.

Libraries:
- `modernc.org/sqlite` (SQLite driver, pure Go)
- `github.com/jackc/pgx/v5/stdlib` (PostgreSQL driver)
- `github.com/standard-webhooks/standard-webhooks/libraries/go`, **in tests and the test receiver only**, to prove R15. Production signing is about 20 lines of our own code.
- `github.com/oklog/ulid/v2` for IDs

## Architecture

```
  Developer's app                                         Receiver
       │                                                     ▲
       │ HTTPS + Bearer API key                              │ POST JSON + webhook-* headers
       ▼                                                     │
 ┌───────────────────────────── hookd serve ─────────────────┼──────────┐
 │                                                          │          │
 │  ┌────────────┐   ┌────────────┐                ┌─────────┴───────┐  │
 │  │ HTTP API   │──▶│ Auth       │                │ Delivery workers │  │
 │  │ (net/http) │   │ (API key → │                │ (N goroutines)   │  │
 │  └─────┬──────┘   │  account)  │                └──┬──────────▲────┘  │
 │        │          └────────────┘   wake signal     │ sign     │       │
 │        │ ───────────────────────────────────────▶  ▼          │       │
 │        │                                     ┌──────────┐     │       │
 │        │                                     │ Signer   │     │       │
 │        │                                     └──────────┘     │       │
 │        ▼                                                      │       │
 │  ┌──────────────────────── Store (interface) ────────────────┴────┐  │
 │  │ accounts · api_keys · endpoints · events · deliveries           │  │
 │  └───────────────────────────────┬──────────────────────────────────┘  │
 └──────────────────────────────────┼───────────────────────────────────┘
                                    ▼
                     SQLite file (local)  │  PostgreSQL (production)

  hookd account create ──▶ Store   (operator CLI, same binary)
  hookd migrate        ──▶ Store
```

- **HTTP API**: routes on the standard `http.ServeMux`. Checks input, calls the Store, and returns JSON or errors (N5).
- **Auth**: middleware that hashes the bearer token, looks it up in `api_keys`, and puts the account ID in the request context. Every Store query is scoped by account ID (R1, R2).
- **Store**: one Go interface with a single SQL implementation that takes a small dialect setting for SQLite or PostgreSQL. It holds all state, and its `deliveries` table is the work queue.
- **Delivery workers**: a pool of goroutines that claim pending deliveries, sign them, POST them once, and record the result (R13–R18). The API sends a non-blocking wake signal after each accepted event, so delivery starts within milliseconds. Workers also poll on an interval to catch work after a restart.
- **Signer**: a pure function that builds the Standard Webhooks headers from (secret, message ID, timestamp, body) (R14).
- **Operator CLI**: subcommands of the same binary (`serve`, `migrate`, `account create`) (R3).
- **Test receiver** (`cmd/testreceiver`, not deployed): a small HTTP server that checks each request with the official Standard Webhooks Go library, logs whether it verified, and answers with a status it is configured to return. It is used for the acceptance steps and for manual checks.

## Data model

| Table | Columns | Notes |
|---|---|---|
| `accounts` | `id` (`acct_…`), `name`, `created_at` | |
| `api_keys` | `id`, `account_id`, `key_hash` (SHA-256, unique), `key_prefix`, `created_at` | The key is shown once, at creation. Only its hash is stored. |
| `endpoints` | `id` (`ep_…`), `account_id`, `url`, `description`, `event_types` (JSON array text; empty = all), `enabled`, `secret`, `created_at`, `updated_at`, `deleted_at` | Soft delete, so event status can still name the endpoint (R12). The secret is stored as-is because signing needs it. |
| `events` | `id` (`msg_…`), `account_id`, `event_type`, `payload` (text), `idempotency_key`, `created_at` | Unique on (`account_id`, `idempotency_key`) when the key is not null |
| `deliveries` | `id`, `event_id`, `endpoint_id`, `status` (`pending`/`in_progress`/`succeeded`/`failed`), `response_status`, `error`, `latency_ms`, `attempted_at`, `locked_until`, `created_at` | One row per (event, endpoint). Indexed on (`status`, `locked_until`). |

IDs are ULIDs with a type prefix, so they sort by time. The event ID is the `webhook-id` (R14).

## Key flows

**Send event (`POST /v1/events`)**
1. Check the event type pattern (R8) and payload size and JSON validity (R9).
2. If an `Idempotency-Key` header is present and an event with that key exists from the last 24 hours, return that event with 200 (R10). An older match has its key cleared first, so the unique index allows the new event.
3. In one transaction, insert the event, load the account's enabled, non-deleted endpoints, filter them by event type in Go, and insert one `pending` delivery for each match (R11, N2).
4. Commit, signal the workers, and return 202 with the event (R9).

**Delivery worker loop**
1. Claim one delivery: a row that is `pending`, or `in_progress` with `locked_until` in the past. Set it to `in_progress` and `locked_until = now + lease`. On PostgreSQL this uses `FOR UPDATE SKIP LOCKED`, so several instances can share the queue. On SQLite it is a single `UPDATE … RETURNING`, since SQLite allows only one writer.
2. Load the event and the endpoint. If the endpoint has been disabled or deleted since then, mark the delivery `failed` with the error `endpoint_disabled` (R7).
3. Sign the request and POST it with a 15 s timeout. Redirects are not followed. The response body is read up to 64 KB and thrown away.
4. Record `succeeded` (2xx) or `failed`, with the status, error text and latency. Nothing is retried (R17).

If the process crashes while a delivery is `in_progress`, its lease expires and another worker sends it again. Delivery is therefore at-least-once, and receivers drop duplicates using `webhook-id`.

**Signing**: `content = id + "." + timestamp + "." + body`. The key is the base64-decoded secret after removing `whsec_`. Header: `webhook-signature: v1,` + base64(HMAC-SHA256(key, content)). The body sent is exactly the stored payload bytes.

## API

All routes are under `/v1`, take JSON, and require `Authorization: Bearer <key>`. Errors look like `{"error": {"code": "not_found", "message": "…"}}`.

| Method & path | Purpose |
|---|---|
| `POST /v1/endpoints` | Create an endpoint (`url`, `description`, `event_types`). Returns it, including `secret`. |
| `GET /v1/endpoints` | List endpoints (`limit`, `after` cursor) |
| `GET /v1/endpoints/{id}` | Get one endpoint (without the secret) |
| `PATCH /v1/endpoints/{id}` | Update `url`, `description`, `event_types`, `enabled` |
| `DELETE /v1/endpoints/{id}` | Soft delete |
| `GET /v1/endpoints/{id}/secret` | Get the signing secret (R5) |
| `POST /v1/events` | Send an event (`event_type`, `payload`; optional `Idempotency-Key` header) |
| `GET /v1/events/{id}` | Get the event and its delivery status for each endpoint (R12) |
| `GET /healthz` | Liveness and database check. No auth. |

URLs must parse as absolute `http` or `https` URLs with a host. Anything belonging to another account returns 404, never 403, so IDs from other accounts are not revealed.

## Configuration

All settings come from environment variables. Each has a default that works locally.

| Variable | What it does | Default |
|---|---|---|
| `HOOKD_LISTEN_ADDR` | Address the API listens on | `:8080` |
| `HOOKD_DB_DRIVER` | `sqlite` or `postgres` | `sqlite` |
| `HOOKD_DB_PATH` | SQLite file path (`:memory:` allowed) | `./hookd.db` |
| `HOOKD_DB_HOST` | PostgreSQL host | `localhost` |
| `HOOKD_DB_PORT` | PostgreSQL port | `5432` |
| `HOOKD_DB_NAME` | PostgreSQL database name | `hookd` |
| `HOOKD_DB_USER` | PostgreSQL user | `hookd` |
| `HOOKD_DB_PASSWORD` | PostgreSQL password | empty |
| `HOOKD_DB_SSLMODE` | PostgreSQL `sslmode` | `disable` |
| `HOOKD_DB_MAX_CONNS` | Database connection pool size | `10` |
| `HOOKD_WORKERS` | Number of delivery goroutines | `8` |
| `HOOKD_POLL_INTERVAL` | How often idle workers check for work | `1s` |
| `HOOKD_DELIVERY_TIMEOUT` | Per-delivery HTTP timeout (R16) | `15s` |
| `HOOKD_DELIVERY_LEASE` | How long a claimed delivery is locked before it can be reclaimed | `60s` |
| `HOOKD_MAX_PAYLOAD_BYTES` | Maximum event payload size (R9) | `262144` |
| `HOOKD_IDEMPOTENCY_WINDOW` | How long idempotency keys are honoured (R10) | `24h` |
| `HOOKD_AUTO_MIGRATE` | Apply database migrations when `serve` starts | `true` |
| `HOOKD_LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` |
| `HOOKD_LOG_FORMAT` | `text` or `json` | `text` |

Logging uses `log/slog`. Secrets, API keys and `Authorization` headers are never logged (N4).

## Local and production

The only outside component is the **database**. There is no cache, broker or file store.

| | Local | Production |
|---|---|---|
| Database | SQLite file `./hookd.db` (pure Go driver, nothing to install) | PostgreSQL on its own node |
| Settings | none (defaults) | `HOOKD_DB_DRIVER=postgres`, `HOOKD_DB_HOST`, `HOOKD_DB_PORT`, `HOOKD_DB_NAME`, `HOOKD_DB_USER`, `HOOKD_DB_PASSWORD`, `HOOKD_DB_SSLMODE`, `HOOKD_DB_MAX_CONNS` |
| Instances | one | one or more behind a load balancer. `SKIP LOCKED` keeps workers from claiming the same delivery twice. |
| Migrations | applied automatically at start | `hookd migrate` as a deploy step, or auto-migrate |

Both databases sit behind the same `Store` interface. The SQL is shared, and the few differences (placeholders, the claim query, JSON and timestamp types) live in a small dialect layer. Migrations are embedded SQL files, with one set per dialect.

**Commands**
- Run locally: `go run ./cmd/hookd serve`
- Create an account: `go run ./cmd/hookd account create --name acme` (prints the account ID and API key)
- Test: `go test ./...`

**Clean store in tests**: each test opens a new SQLite database in `t.TempDir()` and runs migrations, so no test sees another's data. The Store test suite can also be run against PostgreSQL by setting `HOOKD_TEST_PG=1` along with the `HOOKD_DB_*` settings. Each run then creates and drops its own schema. Without `HOOKD_TEST_PG`, these tests are skipped, so the default `go test ./...` needs no outside service.

## Project layout

```
cmd/hookd/            main: serve, migrate, account create
cmd/testreceiver/     verifying test receiver (dev/test only)
internal/config/      env parsing and defaults
internal/store/       Store interface, SQL implementation, dialects, migrations
internal/api/         handlers, auth middleware, errors
internal/delivery/    worker pool, HTTP sender
internal/signing/     Standard Webhooks signer
internal/ids/         prefixed ULIDs, secret and API key generation
```

## Testing

- **Signer**: signs fixed inputs and checks the result with the official Go library, plus a known test vector from the spec (R15).
- **Store**: CRUD, account scoping, idempotency window, fan-out filtering, claim/lease behaviour and reclaiming after an expired lease.
- **API**: `httptest` tests for every route, auth failures (401), and isolation between accounts (404).
- **End to end**: an in-process server with `httptest` receivers covers the seven acceptance steps from the requirements, including a 500 receiver, a timeout, and a 3xx answer counted as a failure.
