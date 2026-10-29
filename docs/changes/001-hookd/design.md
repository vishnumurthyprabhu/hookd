# Design: Hookd 1/6 — Signed event delivery

This design covers piece 1 and the admin UI, with its built-in test receiver and the settings that can be changed while Hookd runs. It is shaped so that the later pieces (retries, replay, rotation, isolation, network safety) can be added without restructuring. It does not build anything for them yet.

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
- `github.com/standard-webhooks/standard-webhooks/libraries/go`, **for verifying only**: in tests, `cmd/testreceiver` and the admin UI's built-in test receiver, to prove R15. Production signing is about 20 lines of our own code, so the check stays independent of Hookd's own signer.
- `github.com/oklog/ulid/v2` for IDs

**Admin UI.** The options were server-rendered pages (`html/template` and plain HTML forms, embedded with `embed`), a single-page app (React + Vite) on a separate admin JSON API, and server-rendered pages plus htmx. **Chosen: server-rendered pages.** They need nothing beyond Go and no build step (N6), work in every browser (N7), and each form posts to a handler that uses the same checks and Store as the API (N8). The SPA would need Node.js and a second API, and nothing requires htmx's partial updates. The one dynamic need, refreshing an event page while deliveries are pending (R34), is met with `<meta http-equiv="refresh">` and a "Refresh" link. Confirmations (R27, R31) are small confirm pages with a POST button, so the UI works without JavaScript; JavaScript only adds the "copy" buttons.

Cross-site request forgery (N9) is handled by Go's standard `http.CrossOriginProtection`. It rejects non-GET requests whose `Sec-Fetch-Site` or `Origin` header shows another site, and needs no tokens or sessions, which suits a UI without sign-in. Requests without those headers, such as the delivery workers' POSTs to the built-in test receiver, pass.

## Architecture

```
  Developer's app            Browser (admin)                      Receiver
       │                          │                                  ▲
       │ HTTPS + Bearer API key   │ GET pages / POST forms           │ POST JSON + webhook-* headers
       ▼                          ▼                                  │
 ┌──────────────────────────────── hookd serve ──────────────────────┼─────────┐
 │  api.Server root handler (logging, panic recovery)                │         │
 │    ├── /v1/*, /healthz ──▶ HTTP API ──▶ Auth (API key → account)  │         │
 │    └── /, /admin/* ─────▶ admin.Handler (CrossOriginProtection)   │         │
 │                              ├── pages and forms                  │         │
 │                              └── POST /admin/test-receiver/{id} ◀─┼─┐       │
 │                                                                   │ │       │
 │   validate (shared checks) ◀── used by both API and admin         │ │       │
 │                                                                   │ │       │
 │   API and admin ── wake signal ──▶ ┌──────────────────┐ ──────────┘ │       │
 │                                    │ Delivery workers │ ────────────┘       │
 │                                    │ (N goroutines)   │  test endpoints     │
 │                                    └────────┬─────────┘  (loopback)         │
 │                                             │ sign ──▶ Signer               │
 │                                                                             │
 │   settings.Manager ── effective Config = env + DB overrides, reload every 5 s
 │        └── Current() ──▶ workers, API, admin, Store, slog.LevelVar          │
 │                                                                             │
 │  ┌──────────────────────────── Store (interface) ─────────────────────────┐ │
 │  │ accounts · api_keys · endpoints · events · deliveries                  │ │
 │  │ settings · test_receivers · test_requests                              │ │
 │  └──────────────────────────────────┬─────────────────────────────────────┘ │
 └─────────────────────────────────────┼───────────────────────────────────────┘
                                       ▼
                         SQLite file (local)  │  PostgreSQL (production)

  hookd account create ──▶ Store   (operator CLI, same binary)
  hookd apikey …       ──▶ Store
  hookd migrate        ──▶ Store
```

- **HTTP API** (`internal/api`): routes on the standard `http.ServeMux`. Checks input, calls the Store, and returns JSON or errors (N5). The option `WithAdmin(h)` routes `/` and `/admin/…` to the admin handler, inside the same request logging and panic recovery. Every other unknown path gets the JSON 404. The max payload is read from the settings manager on each request.
- **Auth**: middleware that hashes the bearer token, looks it up in `api_keys`, and puts the account ID in the request context. Every Store query is scoped by account ID (R1, R2).
- **validate** (`internal/validate`): the URL, description, event type, idempotency key and payload checks, shared by the API and the admin UI so both apply the same rules and codes (N8).
- **Admin UI** (`internal/admin`): serves the pages and form actions under `/admin` and redirects `/` to `/admin/` (R19). It renders embedded `html/template` files with one layout that carries the no-sign-in notice (R22) and the embedded CSS and copy-button script. It calls the Store directly with the shared checks. Every route is wrapped in `http.CrossOriginProtection` (N9) and sends headers that forbid framing and limit scripts and styles to its own (`Content-Security-Policy`, `X-Frame-Options`). It is mounted only when `HOOKD_ADMIN_UI` is on (R21).
- **Built-in test receiver** (part of `internal/admin`): `POST /admin/test-receiver/{trcv_id}` verifies each request with the official library, stores it, and answers with the receiver's configured status (R35–R39).
- **Settings manager** (`internal/settings`): builds the effective configuration from the environment plus the overrides saved in the `settings` table, checks it with the start-up parser, and gives the current value to everything that uses it (R40–R44). It owns the `slog.LevelVar` the logger uses.
- **Store**: one Go interface with a single SQL implementation that takes a small dialect setting for SQLite or PostgreSQL. It holds all state, and its `deliveries` table is the work queue. It reads the idempotency window from a function, so a changed setting applies to the next event.
- **Delivery workers**: a pool of goroutines that claim pending deliveries, sign them, POST them once, and record the result (R13–R18). The API and the admin UI send a non-blocking wake signal after each accepted event, so delivery starts within milliseconds. Workers also poll on an interval to catch work after a restart. The pool reads worker count, poll interval, timeout and lease from the settings manager each time it uses them; a supervisor compares the worker count with the setting every second and starts or stops workers, and a stopped worker finishes its current delivery first.
- **Signer**: a pure function that builds the Standard Webhooks headers from (secret, message ID, timestamp, body) (R14).
- **Operator CLI**: subcommands of the same binary (`serve`, `migrate`, `account create`, `apikey create|list|revoke`) (R3).
- **Test receiver** (`cmd/testreceiver`, not deployed): a small HTTP server that checks each request with the official Standard Webhooks Go library, logs whether it verified, and answers with a status it is configured to return. It is used for the acceptance steps of the API and for manual checks. The admin UI's built-in receiver does the same job in-process.

## Data model

| Table | Columns | Notes |
|---|---|---|
| `accounts` | `id` (`acct_…`), `name`, `created_at` | |
| `api_keys` | `id`, `account_id`, `key_hash` (SHA-256, unique), `key_prefix`, `created_at` | The key is shown once, at creation. Only its hash is stored. Revoking a key deletes its row (R27). |
| `endpoints` | `id` (`ep_…`), `account_id`, `url`, `description`, `event_types` (JSON array text; empty = all), `enabled`, `secret`, `created_at`, `updated_at`, `deleted_at` | Soft delete, so event status can still name the endpoint (R12). The secret is stored as-is because signing needs it. |
| `events` | `id` (`msg_…`), `account_id`, `event_type`, `payload` (text), `idempotency_key`, `created_at` | Unique on (`account_id`, `idempotency_key`) when the key is not null |
| `deliveries` | `id`, `event_id`, `endpoint_id`, `status` (`pending`/`in_progress`/`succeeded`/`failed`), `response_status`, `error`, `latency_ms`, `attempted_at`, `locked_until`, `created_at` | One row per (event, endpoint). Indexed on (`status`, `locked_until`). |
| `settings` | `name` (PK), `value`, `updated_at` | Only UI overrides of the seven editable settings. No row means environment or default. |
| `test_receivers` | `id` (`trcv_…`), `account_id`, `endpoint_id` (unique), `response_status` (default 200), `created_at` | Links a test endpoint to its built-in receiver |
| `test_requests` | `id` (`treq_…`), `receiver_id`, `received_at`, `webhook_id`, `event_type`, `headers` (JSON text), `body` (text), `verified`, `verify_error`, `response_status` | Indexed on (`receiver_id`, `id`). At most 100 per receiver. |

`events` also has an index on (`account_id`, `event_type`, `id`) for the admin UI's filtered event list (N10).

IDs are ULIDs with a type prefix, so they sort by time. The event ID is the `webhook-id` (R14).

## Key flows

**Send event (`POST /v1/events`)**
1. Check the event type pattern (R8) and payload size and JSON validity (R9), with `internal/validate` and the current `HOOKD_MAX_PAYLOAD_BYTES`.
2. If an `Idempotency-Key` header is present and an event with that key exists within the current idempotency window (24 hours by default), return that event with 200 (R10). An older match has its key cleared first, so the unique index allows the new event.
3. In one transaction, insert the event, load the account's enabled, non-deleted endpoints, filter them by event type in Go, and insert one `pending` delivery for each match (R11, N2).
4. Commit, signal the workers, and return 202 with the event (R9).

The admin UI's send-event form goes through the same steps: the shared checks, `Store.CreateEvent`, and the wake signal. An idempotency hit shows "existing event returned" with a link (R32).

**Delivery worker loop**
1. Claim one delivery: a row that is `pending`, or `in_progress` with `locked_until` in the past. Set it to `in_progress` and `locked_until = now + lease`. On PostgreSQL this uses `FOR UPDATE SKIP LOCKED`, so several instances can share the queue. On SQLite it is a single `UPDATE … RETURNING`, since SQLite allows only one writer.
2. Load the event and the endpoint. If the endpoint has been disabled or deleted since then, mark the delivery `failed` with the error `endpoint_disabled` (R7).
3. Sign the request and POST it with a context deadline taken from the current `HOOKD_DELIVERY_TIMEOUT` (15 s by default). Redirects are not followed. The response body is read up to 64 KB and thrown away.
4. Record `succeeded` (2xx) or `failed`, with the status, error text and latency. Nothing is retried (R17).

If the process crashes while a delivery is `in_progress`, its lease expires and another worker sends it again. Delivery is therefore at-least-once, and receivers drop duplicates using `webhook-id`.

**Signing**: `content = id + "." + timestamp + "." + body`. The key is the base64-decoded secret after removing `whsec_`. Header: `webhook-signature: v1,` + base64(HMAC-SHA256(key, content)). The body sent is exactly the stored payload bytes.

**Built-in test receiver**
- *Create (R35)*: in one Store transaction, a new `trcv_…` ID is made, the endpoint is created with the URL `{receiver base URL}/admin/test-receiver/{trcv_id}` and a new secret, and the `test_receivers` row links the two with an answer status (default 200, settable from 100 to 599, R37). The endpoint is an ordinary endpoint, so delivery, signing and the API treat it like any other.
- *Receiver base URL*: `HOOKD_ADMIN_RECEIVER_URL` without a trailing `/`. When it is empty, it is derived from the address `serve` actually listens on: `http://127.0.0.1:<port>` when the host is empty, `0.0.0.0` or `::`, and `http://<host>:<port>` otherwise. Every instance's workers therefore deliver to their own instance over loopback, and because requests are stored in the database it does not matter which instance received one (R39). The URL is fixed when the test endpoint is created.
- *Receive (R36)*: the handler loads the receiver and its endpoint's secret, reads the body (up to the current max payload plus 64 KB), and checks the request with `standardwebhooks.NewWebhook(secret).Verify(body, headers)`. It stores one `test_requests` row: time, `webhook-id`, the event type (looked up from the account's event with that ID, if any), the headers, the body, whether it verified, the verify error, and the status answered. Then it answers with the configured status and a short JSON body. If the receiver is unknown or its endpoint is deleted, it answers 404 and stores nothing.
- *Keep the last 100 (R38)*: the insert and a delete of that receiver's rows beyond the newest 100 run in the same transaction. Deleting the endpoint deletes its `test_requests` rows, and clearing deletes them too.
- Recorded headers and bodies are stored but never logged (N4). Request logging logs only the method, path and status.

**Settings at run time**
- *Effective configuration*: `config.FromLookup` gets a lookup that returns the DB override for a name if there is one, and the environment variable otherwise. The start-up parser is therefore also the checker for UI values, including the lease > timeout rule (R43). The manager also records the source of each value: default, environment or UI (R40).
- *Editable (R41)*: `HOOKD_WORKERS`, `HOOKD_POLL_INTERVAL`, `HOOKD_DELIVERY_TIMEOUT`, `HOOKD_DELIVERY_LEASE`, `HOOKD_MAX_PAYLOAD_BYTES`, `HOOKD_IDEMPOTENCY_WINDOW`, `HOOKD_LOG_LEVEL`. Every other setting is shown read-only, with "set with environment variables" (R44). `HOOKD_DB_PASSWORD` is shown as "set" or "not set" only.
- *Save*: the manager builds the configuration from the saved overrides with the new value in place. If parsing fails, the form shows the parser's message and nothing is saved (R43). Otherwise it upserts the `settings` row and applies the change at once. *Reset* deletes the row, with the same check.
- *Spread (R42)*: each instance reloads the `settings` table every 5 seconds. If the stored overrides together with that instance's environment are invalid (for example, a different environment on another machine), the instance keeps its last valid configuration, logs an error, and the settings page shows the problem.
- *Applying*: consumers call `manager.Current()` when they need a value, never storing a copy. The log level is an `slog.LevelVar` that the manager sets. `serve` loads the overrides once after migrating and before the workers and the server start, so a restart keeps them. If they cannot be applied, it starts with the environment's values and keeps trying.
- `hookd migrate`, `account create` and the `apikey` commands read the environment only.

**Health (R45)**: the settings page shows the result of `Store.Ping`; the driver and either the SQLite path or the PostgreSQL host, port and database name (never the password); the migrations not yet applied, from `Store.PendingMigrations`; and the counts of `pending` and `in_progress` deliveries, from `Store.CountDeliveriesByStatus`.

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

URLs must parse as absolute `http` or `https` URLs with a host. Anything belonging to another account returns 404, never 403, so IDs from other accounts are not revealed. Test endpoints made in the admin UI are ordinary endpoints here.

## Admin UI routes

All under `/admin`, with no auth. Pages are GET. Actions are POST forms that redirect to a page afterwards (post/redirect/get). When a form has errors, the form is shown again with the input kept and the messages above it (R23). The account is part of the URL, so every page below `/admin/accounts/{acct}` is scoped to that account (R28), and an ID from another account shows the "not found" page. Lists show 50 rows per page.

| Route | What it does |
|---|---|
| `GET /` | Redirect to `/admin/` (R19) |
| `GET /admin/` | Account list, paged, with endpoint and key counts, and a "new account" form (R24) |
| `POST /admin/accounts` | Create the account and its first key, then show the key once (R25, R26) |
| `GET /admin/accounts/{acct}` | Account overview: API keys and links to endpoints and events |
| `POST /admin/accounts/{acct}/keys` | Create a key and show it once (R26) |
| `GET`/`POST /admin/accounts/{acct}/keys/{key}/revoke` | Confirm page, then revoke (R27) |
| `GET /admin/accounts/{acct}/endpoints` | Endpoint list, paged, with test endpoints marked (R29) |
| `GET`/`POST /admin/accounts/{acct}/endpoints/new` | Create an endpoint (R29) |
| `GET`/`POST /admin/accounts/{acct}/test-endpoints/new` | Create a test endpoint: event types, description and answer status (R35) |
| `GET /admin/accounts/{acct}/endpoints/{ep}` | Edit form. For a test endpoint, also its answer status and received requests (R38) |
| `POST /admin/accounts/{acct}/endpoints/{ep}` | Update URL, description, event types (R29) |
| `POST /admin/accounts/{acct}/endpoints/{ep}/enable` and `/disable` | Enable or disable (R29) |
| `GET`/`POST /admin/accounts/{acct}/endpoints/{ep}/delete` | Confirm page, then soft delete (R31) |
| `GET /admin/accounts/{acct}/endpoints/{ep}/secret` | Page showing the secret, with a copy button. The secret is on no other page (R30). |
| `POST /admin/accounts/{acct}/endpoints/{ep}/test-status` | Set the receiver's answer status, 100–599 (R37) |
| `POST /admin/accounts/{acct}/endpoints/{ep}/test-requests/clear` | Clear the received requests (R38) |
| `GET /admin/accounts/{acct}/events` | Event list, newest first, `?type=` filter, `?before=` cursor (R33) |
| `GET`/`POST /admin/accounts/{acct}/events/new` | Send-event form (R32) |
| `GET /admin/accounts/{acct}/events/{msg}` | Event detail. It refreshes every 2 s while any delivery is pending or in progress (R34) |
| `GET /admin/settings` | Settings with value and source, edit forms for editable ones, and health (R40–R45) |
| `POST /admin/settings/{name}` and `/admin/settings/{name}/reset` | Save or reset a setting (R41, R43) |
| `POST /admin/test-receiver/{trcv_id}` | Built-in test receiver (R35–R37) |
| `GET /admin/static/…` | Embedded CSS and the copy-button script |

With `HOOKD_ADMIN_UI=false` the handler is not mounted, so every one of these answers the API's JSON 404 (R21).

## Configuration

All settings come from environment variables. Each has a default that works locally. The seven listed under "Settings at run time" (marked *) can also be overridden from the admin UI; those overrides are stored in the database and win over the environment. Every other setting is taken only from the environment and applies at start-up.

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
| `HOOKD_WORKERS` * | Number of delivery goroutines | `8` |
| `HOOKD_POLL_INTERVAL` * | How often idle workers check for work | `1s` |
| `HOOKD_DELIVERY_TIMEOUT` * | Per-delivery HTTP timeout (R16) | `15s` |
| `HOOKD_DELIVERY_LEASE` * | How long a claimed delivery is locked before it can be reclaimed. Must be longer than the timeout. | `60s` |
| `HOOKD_MAX_PAYLOAD_BYTES` * | Maximum event payload size (R9) | `262144` |
| `HOOKD_IDEMPOTENCY_WINDOW` * | How long idempotency keys are honoured (R10) | `24h` |
| `HOOKD_AUTO_MIGRATE` | Apply database migrations when `serve` starts | `true` |
| `HOOKD_LOG_LEVEL` * | `debug`, `info`, `warn`, `error` | `info` |
| `HOOKD_LOG_FORMAT` | `text` or `json` | `text` |
| `HOOKD_ADMIN_UI` | Serve the admin UI and the built-in test receiver under `/admin` (R21) | `true` |
| `HOOKD_ADMIN_RECEIVER_URL` | Base URL, absolute `http` or `https`, that the workers use to reach the built-in test receiver. It is put in new test endpoints' URLs. | empty, meaning derived from the listen address (see "Built-in test receiver") |

Logging uses `log/slog`. Secrets, API keys and `Authorization` headers are never logged (N4).

## Local and production

The only outside component is the **database**. There is no cache, broker or file store.

| | Local | Production |
|---|---|---|
| Database | SQLite file `./hookd.db` (pure Go driver, nothing to install) | PostgreSQL on its own node |
| Settings | none (defaults) | `HOOKD_DB_DRIVER=postgres`, `HOOKD_DB_HOST`, `HOOKD_DB_PORT`, `HOOKD_DB_NAME`, `HOOKD_DB_USER`, `HOOKD_DB_PASSWORD`, `HOOKD_DB_SSLMODE`, `HOOKD_DB_MAX_CONNS` |
| Instances | one | one or more behind a load balancer. `SKIP LOCKED` keeps workers from claiming the same delivery twice. |
| Migrations | applied automatically at start | `hookd migrate` as a deploy step, or auto-migrate |
| Admin UI | on, at http://localhost:8080/ | on by default. Set `HOOKD_ADMIN_UI=false` on instances reachable from untrusted networks, or limit `/admin` at the load balancer. The UI has no sign-in. |
| Built-in test receiver | workers reach `http://127.0.0.1:8080` | each instance reaches itself over loopback. Requests are stored in the database, so every instance shows them. |
| UI settings overrides | stored in SQLite | stored in PostgreSQL and picked up by every instance within 5 s |

Both databases sit behind the same `Store` interface. The SQL is shared, and the few differences (placeholders, the claim query, JSON and timestamp types) live in a small dialect layer. Migrations are embedded SQL files, with one set per dialect: `0001_init` and `0002_admin` (the admin UI's tables and index).

**Commands**
- Run locally: `go run ./cmd/hookd serve`
- Create an account: `go run ./cmd/hookd account create --name acme` (prints the account ID and API key)
- Test: `go test ./...`

**Clean store in tests**: each test opens a new SQLite database in `t.TempDir()` and runs migrations, so no test sees another's data. The Store test suite can also be run against PostgreSQL by setting `HOOKD_TEST_PG=1` along with the `HOOKD_DB_*` settings. Each run then creates and drops its own schema. Without `HOOKD_TEST_PG`, these tests are skipped, so the default `go test ./...` needs no outside service.

## Project layout

```
cmd/hookd/                 main: serve, migrate, account create, apikey
cmd/testreceiver/          verifying test receiver (dev/test only)
internal/config/           env parsing and defaults
internal/settings/         run-time settings manager
internal/store/            Store interface, SQL implementation, dialects, migrations
internal/api/              handlers, auth middleware, errors
internal/validate/         input checks shared by api and admin
internal/admin/            admin UI handler, pages, built-in test receiver
internal/admin/templates/  html/template files (embedded)
internal/admin/static/     style.css, copy.js (embedded)
internal/delivery/         worker pool, HTTP sender
internal/signing/          Standard Webhooks signer
internal/ids/              prefixed ULIDs (including trcv_, treq_), secret and API key generation
```

## Testing

- **Signer**: signs fixed inputs and checks the result with the official Go library, plus a known test vector from the spec (R15).
- **validate**: table tests for each check.
- **Store**: CRUD, account scoping, idempotency window (including a changed window applying to the next event), fan-out filtering, claim/lease behaviour and reclaiming after an expired lease. Also the admin methods: account and event list paging and filtering, settings, pending migrations, keeping only the last 100 test requests and deleting them with the endpoint.
- **settings**: the lookup order and sources, refusing invalid values (including lease ≤ timeout), reset, reload from the database, and keeping the last valid configuration when stored overrides become invalid.
- **delivery**: besides sending, the worker count grows and shrinks with the setting, and a changed timeout applies to the next send.
- **API**: `httptest` tests for every route, auth failures (401), isolation between accounts (404), and a lowered max payload refused without a restart.
- **admin**: `httptest` tests for every route: the no-sign-in notice on every page; form errors keeping the input; a key shown once and only its prefix afterwards; secrets absent from pages other than the secret page; another account's IDs answering not found; a cross-site POST rejected (N9); and a 404 for `/admin` when the UI is off. The built-in receiver stores a correctly signed request as verified, one signed with a wrong secret as not verified with the reason, and answers the configured status.
- **End to end**: an in-process server with `httptest` receivers covers the seven API acceptance steps from the requirements, including a 500 receiver, a timeout, and a 3xx answer counted as a failure. A second test goes through the eight admin UI acceptance steps by driving the admin routes over HTTP, with the workers delivering to the built-in receiver.
