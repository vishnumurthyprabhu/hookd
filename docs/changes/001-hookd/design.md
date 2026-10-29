# Design: Hookd 1/6 — Signed event delivery

This design covers piece 1 and the admin UI, with its built-in test receiver and the settings that can be changed while Hookd runs, and piece 2: retries, the attempt history and retention. It is shaped so that the later pieces (replay, rotation, isolation, network safety) can be added without restructuring. It does not build anything for them yet.

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

The retry jitter uses the standard `math/rand/v2`.

**Retries.** The options were a retry time on the delivery row with attempts in their own table, one queue row per attempt, and in-process timers (`time.AfterFunc`) that re-queue a delivery when due. **Chosen: the retry time on the delivery row.** A failed delivery goes back to `pending` with a later `next_attempt_at`, and the claim only takes rows that are due. Waiting retries are rows, not goroutines, so they survive restarts, are shared by instances and hold no worker (R48, R49), and one transaction records the attempt and the next step (N12). One row per attempt would make the delivery's status a sum of several rows, and timers are lost on a restart unless they are stored anyway.

**Retention.** The options were a periodic batched delete and, on PostgreSQL, tables partitioned by day and dropped whole. **Chosen: the batched delete.** It works the same on both databases, and at the scale in the requirements a delete of 500 events at a time is short. Partitioning can come later if volume needs it.

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
 │                                             │ failed, wait left in schedule:│
 │                                             │   back to pending, due later  │
 │                                             └── RecordAttempt (one tx)      │
 │                                                                             │
 │   Retention runner ── hourly ──▶ DeleteExpiredEvents (batches of 500)       │
 │                                                                             │
 │   settings.Manager ── effective Config = env + DB overrides, reload every 5 s
 │        └── Current() ──▶ workers, API, admin, Store, retention, slog.LevelVar
 │                                                                             │
 │  ┌──────────────────────────── Store (interface) ─────────────────────────┐ │
 │  │ accounts · api_keys · endpoints · events · deliveries · attempts       │ │
 │  │ settings · test_receivers · test_requests                              │ │
 │  └──────────────────────────────────┬─────────────────────────────────────┘ │
 └─────────────────────────────────────┼───────────────────────────────────────┘
                                       ▼
                         SQLite file (local)  │  PostgreSQL (production)

  hookd account create ──▶ Store   (operator CLI, same binary)
  hookd apikey …       ──▶ Store
  hookd migrate        ──▶ Store
```

- **HTTP API** (`internal/api`): routes on the standard `http.ServeMux`, including the three attempt routes. Checks input, calls the Store, and returns JSON or errors (N5). The option `WithAdmin(h)` routes `/` and `/admin/…` to the admin handler, inside the same request logging and panic recovery. Every other unknown path gets the JSON 404. The max payload is read from the settings manager on each request.
- **Auth**: middleware that hashes the bearer token, looks it up in `api_keys`, and puts the account ID in the request context. Every Store query is scoped by account ID (R1, R2).
- **validate** (`internal/validate`): the URL, description, event type, idempotency key and payload checks, shared by the API and the admin UI so both apply the same rules and codes (N8).
- **Admin UI** (`internal/admin`): serves the pages and form actions under `/admin` and redirects `/` to `/admin/` (R19). It renders embedded `html/template` files with one layout that carries the no-sign-in notice (R22) and the embedded CSS and copy-button script. It calls the Store directly with the shared checks. Every route is wrapped in `http.CrossOriginProtection` (N9) and sends headers that forbid framing and limit scripts and styles to its own (`Content-Security-Policy`, `X-Frame-Options`). It is mounted only when `HOOKD_ADMIN_UI` is on (R21).
- **Built-in test receiver** (part of `internal/admin`): `POST /admin/test-receiver/{trcv_id}` verifies each request with the official library, stores it, and answers with the receiver's configured status (R35–R39).
- **Settings manager** (`internal/settings`): builds the effective configuration from the environment plus the overrides saved in the `settings` table, checks it with the start-up parser, and gives the current value to everything that uses it (R40–R44). It owns the `slog.LevelVar` the logger uses.
- **Store**: one Go interface with a single SQL implementation that takes a small dialect setting for SQLite or PostgreSQL. It holds all state, its `deliveries` table is the work queue, and its `attempts` table the attempt history. It reads the idempotency window from a function, so a changed setting applies to the next event. Its clock can be replaced in tests, so due times and retention are tested without sleeping.
- **Delivery workers** (`internal/delivery`): a pool of goroutines that claim due deliveries, sign them, POST them, and record each attempt with the delivery's next step: final, or `pending` again until a retry is due by the current retry schedule (R13–R18, R46–R51). The API and the admin UI send a non-blocking wake signal after each accepted event, so delivery starts within milliseconds. Workers also poll on an interval to catch work after a restart. The pool reads worker count, poll interval, timeout, lease and retry schedule from the settings manager each time it uses them; a supervisor compares the worker count with the setting every second and starts or stops workers, and a stopped worker finishes its current delivery first.
- **Retention runner** (`internal/retention`): a goroutine started by `serve` that deletes expired events in batches (R57–R59).
- **Signer**: a pure function that builds the Standard Webhooks headers from (secret, message ID, timestamp, body) (R14).
- **Operator CLI**: subcommands of the same binary (`serve`, `migrate`, `account create`, `apikey create|list|revoke`) (R3).
- **Test receiver** (`cmd/testreceiver`, not deployed): a small HTTP server that checks each request with the official Standard Webhooks Go library, logs whether it verified, and answers with a status it is configured to return. It is used for the acceptance steps of the API and for manual checks. The admin UI's built-in receiver does the same job in-process.

## Data model

| Table | Columns | Notes |
|---|---|---|
| `accounts` | `id` (`acct_…`), `name`, `created_at` | |
| `api_keys` | `id`, `account_id`, `key_hash` (SHA-256, unique), `key_prefix`, `created_at` | The key is shown once, at creation. Only its hash is stored. Revoking a key deletes its row (R27). |
| `endpoints` | `id` (`ep_…`), `account_id`, `url`, `description`, `event_types` (JSON array text; empty = all), `enabled`, `secret`, `created_at`, `updated_at`, `deleted_at` | Soft delete, so event status can still name the endpoint (R12). The secret is stored as-is because signing needs it. |
| `events` | `id` (`msg_…`), `account_id`, `event_type`, `payload` (text), `idempotency_key`, `created_at` | Unique on (`account_id`, `idempotency_key`) when the key is not null. Indexed on `created_at` for the retention delete. |
| `deliveries` | `id`, `event_id`, `endpoint_id`, `status` (`pending`/`in_progress`/`succeeded`/`failed`), `response_status`, `error`, `latency_ms`, `attempted_at`, `locked_until`, `attempts`, `next_attempt_at`, `created_at` | One row per (event, endpoint). `pending` also means "waiting for a retry" (`attempts > 0`). `response_status`, `error`, `latency_ms` and `attempted_at` are those of the last attempt. `next_attempt_at` is when the delivery may be claimed: the creation time for a new delivery, the retry time after a failed attempt; it is kept, but not used, once the delivery is final. Indexed on (`status`, `locked_until`) for reclaiming expired leases, (`status`, `next_attempt_at`) for the claim, and (`endpoint_id`, `status`) for disabling an endpoint. |
| `attempts` | `id` (`att_…`), `account_id`, `delivery_id`, `event_id`, `endpoint_id`, `number`, `started_at`, `outcome` (`succeeded`/`failed`), `response_status` (NULL if none), `latency_ms`, `error` (NULL on success) | One row per attempt (R52). `account_id` is copied from the event, so one attempt is fetched by account without a join. Unique on (`delivery_id`, `number`). Indexed on (`event_id`, `id`), (`endpoint_id`, `id`) and (`endpoint_id`, `outcome`, `id`) for the lists (N13). |
| `settings` | `name` (PK), `value`, `updated_at` | Only UI overrides of the eight editable settings. No row means environment or default. |
| `test_receivers` | `id` (`trcv_…`), `account_id`, `endpoint_id` (unique), `response_status` (default 200), `created_at` | Links a test endpoint to its built-in receiver |
| `test_requests` | `id` (`treq_…`), `receiver_id`, `received_at`, `webhook_id`, `event_type`, `headers` (JSON text), `body` (text), `verified`, `verify_error`, `response_status` | Indexed on (`receiver_id`, `id`). At most 100 per receiver. |

`events` also has an index on (`account_id`, `event_type`, `id`) for the admin UI's filtered event list (N10).

IDs are ULIDs with a type prefix, so they sort by time. The event ID is the `webhook-id` (R14).

Migration `0003_retries` added the retry columns, the `attempts` table and their indexes, and filled them in for existing data (N14): every delivery got `next_attempt_at = created_at`, and every `succeeded` or `failed` delivery with an `attempted_at` got `attempts = 1` and one `attempts` row built from its recorded outcome, with the ID `'att_' || substr(id, 5)` (the delivery's ULID under the attempt prefix). SQLite adds `next_attempt_at` with a default of `''` and fills it right after.

## Key flows

**Send event (`POST /v1/events`)**
1. Check the event type pattern (R8) and payload size and JSON validity (R9), with `internal/validate` and the current `HOOKD_MAX_PAYLOAD_BYTES`.
2. If an `Idempotency-Key` header is present and an event with that key exists within the current idempotency window (24 hours by default), return that event with 200 (R10). An older match has its key cleared first, so the unique index allows the new event.
3. In one transaction, insert the event, load the account's enabled, non-deleted endpoints, filter them by event type in Go, and insert one `pending` delivery for each match, due at once (R11, N2).
4. Commit, signal the workers, and return 202 with the event (R9).

The admin UI's send-event form goes through the same steps: the shared checks, `Store.CreateEvent`, and the wake signal. An idempotency hit shows "existing event returned" with a link (R32).

**Delivery worker loop**
1. Claim one due delivery: a row that is `pending` with `next_attempt_at <= now`, or `in_progress` with `locked_until` in the past, the oldest due first (`ORDER BY next_attempt_at, id`). Set it to `in_progress` and `locked_until = now + lease`. On PostgreSQL this uses `FOR UPDATE SKIP LOCKED`, so several instances can share the queue. On SQLite it is a single `UPDATE … RETURNING`, since SQLite allows only one writer. The claim returns the attempts made so far; this attempt's number is one more.
2. Load the event and the endpoint. If the endpoint has been disabled or deleted since then, the outcome is `failed` with the error `endpoint_disabled`, and it is final (R7, R47).
3. Otherwise sign the request with a new timestamp and the endpoint's current secret, and POST it with a context deadline taken from the current `HOOKD_DELIVERY_TIMEOUT` (15 s by default). Redirects are not followed. The response body is read up to 64 KB and thrown away. A 2xx is `succeeded`; anything else is `failed`, with an error such as `non-2xx response: 500 Internal Server Error` or `redirect not followed: 302 Found`.
4. If it failed, was not `endpoint_disabled`, and `number <= len(schedule)`, the next attempt is due at `now + schedule[number-1] × r`, with `r` drawn uniformly from 0.9 to 1.1 (R46). The schedule is read from the settings manager at this moment, so a changed schedule applies to the next wait scheduled (R50).
5. `Store.RecordAttempt` stores, in one transaction (N12): the `attempts` row, and the delivery's attempt count, last-attempt fields, cleared lease and either its final status or `pending` with the new `next_attempt_at`. The update only applies `WHERE status = 'in_progress' AND attempts = number - 1`, so an attempt reclaimed by another worker after a lease expired is recorded once.
6. Log `delivery attempt` with the delivery, event and endpoint IDs, the attempt number, the outcome and, if retried, `next_attempt_at` (N4).

Waiting retries are only rows, so they hold no worker and do not delay new events, which are due at once (R49). Idle workers poll every `HOOKD_POLL_INTERVAL`, so a retry starts within about a second of its due time, also after a restart (R48).

If the process crashes while a delivery is `in_progress`, its lease expires and another worker sends it again, as an attempt with the same number; the cut-short attempt is not recorded. Delivery is therefore at-least-once, and receivers drop duplicates using `webhook-id`.

**Disabling or deleting an endpoint**: `UpdateEndpoint` (with `enabled = false`) and `DeleteEndpoint` set, in the same transaction, `next_attempt_at = now` on that endpoint's `pending` deliveries that are not yet due. The workers then claim them at once and record the `endpoint_disabled` attempt, instead of leaving them waiting for hours (R47, R52). Enabling the endpoint again does nothing to them.

**Delivery status (R12)**: `GetEvent` also reads each delivery's `attempts`, `attempted_at` and `next_attempt_at`. `next_attempt_at` is reported only while the delivery is `pending` and `attempts > 0`, so a new delivery has none.

**Attempt lists (R53–R56)**
- *Event's attempts*: check the event belongs to the account (else `ErrNotFound`), then `SELECT … FROM attempts WHERE event_id = ? [AND endpoint_id = ?] [AND id > ?] ORDER BY id LIMIT ?`.
- *Endpoint's attempts*: check the endpoint belongs to the account, deleted endpoints included, then `WHERE endpoint_id = ? [AND outcome = ?] [AND id < ?] ORDER BY id DESC LIMIT ?`.
- *One attempt*: `WHERE id = ? AND account_id = ?`.

Attempt IDs are ULIDs, so ID order is time order.

**Retention (R57–R59)**: the retention runner waits one minute after start, then runs once an hour. Each run computes `cutoff = now - HOOKD_RETENTION` and calls `Store.DeleteExpiredEvents(cutoff, 500)` until it deletes fewer than 500; errors are logged and the next run tries again. Each call is one short transaction:
1. select up to 500 event IDs with `created_at < cutoff` and no delivery that is `pending` or `in_progress` (a safeguard for an event whose retries were held up by downtime), oldest first; on PostgreSQL with `FOR UPDATE SKIP LOCKED`, so two instances running at once take different events;
2. delete their `attempts`, then their `deliveries`, then the events.

Expired events then answer 404 and "not found" with no special case. Because `HOOKD_RETENTION` must be longer than the idempotency window, a deleted event's key is no longer honoured anyway. Test requests keep their own rule (the last 100) and are not touched. The runner's interval and batch size are constants.

**Signing**: `content = id + "." + timestamp + "." + body`. The key is the base64-decoded secret after removing `whsec_`. Header: `webhook-signature: v1,` + base64(HMAC-SHA256(key, content)). The body sent is exactly the stored payload bytes.

**Built-in test receiver**
- *Create (R35)*: in one Store transaction, a new `trcv_…` ID is made, the endpoint is created with the URL `{receiver base URL}/admin/test-receiver/{trcv_id}` and a new secret, and the `test_receivers` row links the two with an answer status (default 200, settable from 100 to 599, R37). The endpoint is an ordinary endpoint, so delivery, signing and the API treat it like any other.
- *Receiver base URL*: `HOOKD_ADMIN_RECEIVER_URL` without a trailing `/`. When it is empty, it is derived from the address `serve` actually listens on: `http://127.0.0.1:<port>` when the host is empty, `0.0.0.0` or `::`, and `http://<host>:<port>` otherwise. Every instance's workers therefore deliver to their own instance over loopback, and because requests are stored in the database it does not matter which instance received one (R39). The URL is fixed when the test endpoint is created.
- *Receive (R36)*: the handler loads the receiver and its endpoint's secret, reads the body (up to the current max payload plus 64 KB), and checks the request with `standardwebhooks.NewWebhook(secret).Verify(body, headers)`. It stores one `test_requests` row: time, `webhook-id`, the event type (looked up from the account's event with that ID, if any), the headers, the body, whether it verified, the verify error, and the status answered. Then it answers with the configured status and a short JSON body. If the receiver is unknown or its endpoint is deleted, it answers 404 and stores nothing.
- *Keep the last 100 (R38)*: the insert and a delete of that receiver's rows beyond the newest 100 run in the same transaction. Deleting the endpoint deletes its `test_requests` rows, and clearing deletes them too.
- Recorded headers and bodies are stored but never logged (N4). Request logging logs only the method, path and status.

**Settings at run time**
- *Effective configuration*: `config.FromLookup` gets a lookup that returns the DB override for a name if there is one, and the environment variable otherwise. The start-up parser is therefore also the checker for UI values, including the lease > timeout rule (R43). The manager also records the source of each value: default, environment or UI (R40).
- *Editable (R41)*: `HOOKD_WORKERS`, `HOOKD_POLL_INTERVAL`, `HOOKD_DELIVERY_TIMEOUT`, `HOOKD_DELIVERY_LEASE`, `HOOKD_MAX_PAYLOAD_BYTES`, `HOOKD_IDEMPOTENCY_WINDOW`, `HOOKD_LOG_LEVEL`, `HOOKD_RETRY_SCHEDULE`. Every other setting, `HOOKD_RETENTION` included, is shown read-only, with "set with environment variables" (R44). `HOOKD_DB_PASSWORD` is shown as "set" or "not set" only.
- *Save*: the manager builds the configuration from the saved overrides with the new value in place. If parsing fails, the form shows the parser's message and nothing is saved (R43). Otherwise it upserts the `settings` row and applies the change at once. *Reset* deletes the row, with the same check.
- *Spread (R42)*: each instance reloads the `settings` table every 5 seconds. If the stored overrides together with that instance's environment are invalid (for example, a different environment on another machine), the instance keeps its last valid configuration, logs an error, and the settings page shows the problem.
- *Applying*: consumers call `manager.Current()` when they need a value, never storing a copy. The log level is an `slog.LevelVar` that the manager sets. `serve` loads the overrides once after migrating and before the workers and the server start, so a restart keeps them. If they cannot be applied, it starts with the environment's values and keeps trying.
- `hookd migrate`, `account create` and the `apikey` commands read the environment only.

**Health (R45)**: the settings page shows the result of `Store.Ping`; the driver and either the SQLite path or the PostgreSQL host, port and database name (never the password); the migrations not yet applied, from `Store.PendingMigrations`; the counts of `pending` and `in_progress` deliveries, from `Store.CountDeliveriesByStatus`; and the count of deliveries waiting for a retry (`pending` with `attempts > 0`), from `Store.CountWaitingRetries`.

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
| `GET /v1/events/{id}/attempts` | The event's attempts, oldest first (R53). Query: `endpoint_id`, `limit`, `after` (attempt ID). |
| `GET /v1/endpoints/{id}/attempts` | The endpoint's attempts, newest first, also for a deleted endpoint (R54, R56). Query: `outcome` (`succeeded` or `failed`), `limit`, `before` (attempt ID). |
| `GET /v1/attempts/{id}` | One attempt (R55) |
| `GET /healthz` | Liveness and database check. No auth. |

Each entry of `deliveries` in `GET /v1/events/{id}` (and in the `POST /v1/events` answer) has, besides its status, `response_status` and `error`, the fields `attempts` (attempts made so far), `last_attempt_at` (start of the last attempt, or `null`) and `next_attempt_at` (when the next retry is due, or `null` when none is waiting).

The attempt lists answer `{"data": [...], "has_more": bool}` like `GET /v1/endpoints`, with the same `limit`. An attempt:

```json
{
  "id": "att_01J…",
  "event_id": "msg_01J…",
  "endpoint_id": "ep_01J…",
  "number": 2,
  "started_at": "2026-10-29T10:00:05.123Z",
  "outcome": "failed",
  "response_status": 500,
  "latency_ms": 42,
  "error": "non-2xx response: 500 Internal Server Error"
}
```

`response_status` is `null` when there was none, and `error` is left out on success. An unknown `outcome` or a bad `limit` answers 400 `invalid_request`.

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
| `GET /admin/accounts/{acct}/endpoints/{ep}` | Edit form and "Recent attempts", newest first, 50 per page (`?attempts_before=`), with an `?outcome=` filter and a link from each row to its event (R60). For a test endpoint, also its answer status and received requests (R38). A deleted endpoint has no page; its attempts still show on each event's page. |
| `POST /admin/accounts/{acct}/endpoints/{ep}` | Update URL, description, event types (R29) |
| `POST /admin/accounts/{acct}/endpoints/{ep}/enable` and `/disable` | Enable or disable (R29) |
| `GET`/`POST /admin/accounts/{acct}/endpoints/{ep}/delete` | Confirm page, then soft delete (R31) |
| `GET /admin/accounts/{acct}/endpoints/{ep}/secret` | Page showing the secret, with a copy button. The secret is on no other page (R30). |
| `POST /admin/accounts/{acct}/endpoints/{ep}/test-status` | Set the receiver's answer status, 100–599 (R37) |
| `POST /admin/accounts/{acct}/endpoints/{ep}/test-requests/clear` | Clear the received requests (R38) |
| `GET /admin/accounts/{acct}/events` | Event list, newest first, `?type=` filter, `?before=` cursor. Waiting retries are `pending` and count as pending (R33). |
| `GET`/`POST /admin/accounts/{acct}/events/new` | Send-event form (R32) |
| `GET /admin/accounts/{acct}/events/{msg}` | Event detail. For each delivery: the number of attempts, "next retry at …" when one is waiting, and a table of its attempts (number, time, outcome, HTTP status, latency, error), up to 200 with a "more" link (`?after=`) beyond that. It refreshes every 2 s while any delivery is in progress, waits for its first attempt or has a retry due within 10 s; every 30 s while the only unfinished deliveries wait for a retry further off; and not at all once every delivery is final (R34) |
| `GET /admin/settings` | Settings with value and source, edit forms for editable ones, and health (R40–R45) |
| `POST /admin/settings/{name}` and `/admin/settings/{name}/reset` | Save or reset a setting (R41, R43) |
| `POST /admin/test-receiver/{trcv_id}` | Built-in test receiver (R35–R37) |
| `GET /admin/static/…` | Embedded CSS and the copy-button script |

With `HOOKD_ADMIN_UI=false` the handler is not mounted, so every one of these answers the API's JSON 404 (R21).

## Configuration

All settings come from environment variables. Each has a default that works locally. The eight listed under "Settings at run time" (marked *) can also be overridden from the admin UI; those overrides are stored in the database and win over the environment. Every other setting is taken only from the environment and applies at start-up.

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
| `HOOKD_RETRY_SCHEDULE` * | Waits before each retry, comma-separated Go durations, at most 20 (R50). `none` means no retries. Each wait gets ±10% jitter. | `5s,5m,30m,2h,5h,10h,10h` |
| `HOOKD_RETENTION` | How long events, deliveries and attempts are kept (R58) | `720h` (30 days) |

Besides lease > timeout, `config.FromLookup` checks that every wait in the schedule is a positive duration and there are at most 20, and that `HOOKD_RETENTION` is longer than `HOOKD_IDEMPOTENCY_WINDOW` and longer than the sum of the schedule plus 10%. These checks apply at start-up and to values saved in the UI (R43, R51). An unset or empty variable keeps its default, as for every setting, which is why "no retries" is written `none` rather than an empty value.

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
| Waiting retries | rows in SQLite, sent by the one instance | rows in PostgreSQL, claimed by any instance with `SKIP LOCKED` |
| Retention | runs in the one instance | runs in every instance; `SKIP LOCKED` keeps them on different events |

Both databases sit behind the same `Store` interface. The SQL is shared, and the few differences (placeholders, the claim query, `FOR UPDATE SKIP LOCKED` for the retention select, JSON and timestamp types) live in a small dialect layer. Migrations are embedded SQL files, with one set per dialect: `0001_init`, `0002_admin` (the admin UI's tables and index) and `0003_retries` (the retry columns and the `attempts` table).

**Commands**
- Run locally: `go run ./cmd/hookd serve`
- Create an account: `go run ./cmd/hookd account create --name acme` (prints the account ID and API key)
- Test: `go test ./...`
- Watch retries locally without waiting hours: set `HOOKD_RETRY_SCHEDULE=5s,10s,20s` in the environment or in the admin UI

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
internal/delivery/         worker pool, retry scheduling, HTTP sender
internal/retention/        retention runner
internal/signing/          Standard Webhooks signer
internal/ids/              prefixed ULIDs (including trcv_, treq_, att_), secret and API key generation
```

## Testing

- **Signer**: signs fixed inputs and checks the result with the official Go library, plus a known test vector from the spec (R15).
- **validate**: table tests for each check.
- **Store**: CRUD, account scoping, idempotency window (including a changed window applying to the next event), fan-out filtering, claim/lease behaviour and reclaiming after an expired lease. Claims skip deliveries that are not due and take them once due; `RecordAttempt` writes the attempt and the next step together and refuses a second record of the same number; the `0003_retries` migration fills attempts for existing deliveries; disabling or deleting an endpoint makes its waiting deliveries due; the attempt lists with filters, cursors and account scoping, including a deleted endpoint; `DeleteExpiredEvents` removes old events with their rows in batches and keeps newer ones and those with unfinished deliveries. Also the admin methods: account and event list paging and filtering, settings, pending migrations, keeping only the last 100 test requests and deleting them with the endpoint.
- **config / settings**: the lookup order and sources, refusing invalid values (including lease ≤ timeout), reset, reload from the database, and keeping the last valid configuration when stored overrides become invalid. Parsing the retry schedule (`none`, bad durations, more than 20) and the retention checks, including a schedule saved from the UI refused when it would break them.
- **delivery**: besides sending, the worker count grows and shrinks with the setting, and a changed timeout applies to the next send. With a schedule of a few milliseconds: retry after failure, stop at success, final `failed` after the last wait, `none` meaning one attempt, jitter within ±10% (with an injected random source), the attempt number in logs, and a changed schedule applying to the next wait.
- **retention**: the runner deletes in batches until done, with a fake clock.
- **API**: `httptest` tests for every route, auth failures (401), isolation between accounts (404), and a lowered max payload refused without a restart. For the attempt routes: filters, paging, 400 for a bad `outcome`, and the new delivery fields.
- **admin**: `httptest` tests for every route: the no-sign-in notice on every page; the event page showing attempts and the next retry, the endpoint page listing attempts, the settings page editing the retry schedule; form errors keeping the input; a key shown once and only its prefix afterwards; secrets absent from pages other than the secret page; another account's IDs answering not found; a cross-site POST rejected (N9); and a 404 for `/admin` when the UI is off. The built-in receiver stores a correctly signed request as verified, one signed with a wrong secret as not verified with the reason, and answers the configured status.
- **End to end**: an in-process server with `httptest` receivers covers the seven API acceptance steps from the requirements, including a 500 receiver, a timeout, and a 3xx answer counted as a failure. A second test goes through the eight admin UI acceptance steps by driving the admin routes over HTTP, with the workers delivering to the built-in receiver. A third walks the retry acceptance steps with schedule `5ms,10ms,20ms` and the built-in receiver: fail, retry, switch the receiver to 200, succeed on attempt 3 with the same `webhook-id`; 4 attempts then `failed`; disabling while a retry waits; and a restart (a new pool on the same database) sending a retry that came due while it was stopped.
