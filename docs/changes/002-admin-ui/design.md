# Design: Admin UI

This design adds a browser UI to `hookd serve`, a built-in test receiver, and settings that can be changed while Hookd runs. It keeps the stack of Hookd 1/6: one Go binary, the standard library's `net/http`, and the SQL store on SQLite or PostgreSQL. The `/v1` API and the CLI do not change.

## Options considered

| Option | Fit to requirements | Complexity | Cost to run and maintain |
|---|---|---|---|
| **A. Server-rendered pages**: `html/template` plus plain HTML forms, embedded in the binary with `embed`, and a few lines of plain JavaScript for "copy" buttons only | Good. Nothing beyond Go (N1). Works in every browser (N2). Each form posts to a handler that calls the same checks and Store as the API (N4). | Low: one new package, no new dependency, no build step | None beyond the binary. Templates are in the same repo and tested with `httptest`. |
| B. Single-page app (React + Vite) on a new admin JSON API, with the built files embedded | Works, but needs Node.js and a build step before `go build`, which breaks N1. It also needs a second, admin-only JSON API. | High: two languages, a bundler, an extra API layer | Higher: a JS dependency tree to keep up to date |
| C. Server-rendered pages plus htmx (vendored JS file) for partial updates | Good. It refreshes parts of pages without reloads. | Medium: a third-party script and its attribute conventions | Low, but nothing in the requirements needs partial updates |

**Chosen: A.** It meets every requirement with no new dependency. The one dynamic need, refreshing an event page while deliveries are pending (R16), is met with `<meta http-equiv="refresh">` and a "Refresh" link. Confirmations (R9, R13) are small confirm pages with a POST button, so the UI works without JavaScript. JavaScript only adds the "copy" buttons.

Cross-site request forgery (N5) is handled by Go's standard `http.CrossOriginProtection` (`net/http`, Go 1.25+). It rejects non-GET requests whose `Sec-Fetch-Site` or `Origin` header shows another site, and needs no tokens or sessions, which suits a UI without sign-in. Requests without those headers, such as the delivery workers' POSTs to the test receiver, pass.

The built-in receiver verifies signatures with the official Standard Webhooks Go library. Hookd 1/6 already uses it in tests and `cmd/testreceiver`, so the check stays independent of Hookd's own signer (as Hookd 1/6 R15 intended). This moves the library from test-only to the production binary, which is a small, pure-Go dependency.

## Architecture

```
  Browser (admin)                              Delivery workers (same process)
     │ GET pages / POST forms                        │ signed POST to a test endpoint URL
     ▼                                               ▼
 ┌──────────────────────────────── hookd serve ──────────────────────────────────┐
 │  api.Server root handler (logging, panic recovery)                             │
 │    ├── /v1/*, /healthz ──▶ existing JSON API (bearer auth)                     │
 │    └── /, /admin/* ─────▶ admin.Handler (CrossOriginProtection)                │
 │                              ├── pages: accounts, keys, endpoints, events,     │
 │                              │          test endpoints, settings & health      │
 │                              └── POST /admin/test-receiver/{id} ── verify ──┐  │
 │                                                                              │  │
 │   validate (shared checks) ◀── used by both api and admin                    │  │
 │                                                                              │  │
 │   settings.Manager ── effective Config = env + DB overrides, reload every 5 s │  │
 │        │ Current()                                                           │  │
 │        ├──▶ delivery.Pool (worker count, poll interval, timeout, lease)      │  │
 │        ├──▶ api / admin (max payload)                                        │  │
 │        ├──▶ store (idempotency window)                                       │  │
 │        └──▶ slog.LevelVar (log level)                                        │  │
 │                                                                              ▼  │
 │  ┌───────────────────────────── Store (interface) ──────────────────────────┐  │
 │  │ accounts · api_keys · endpoints · events · deliveries                    │  │
 │  │ + settings · test_receivers · test_requests                              │  │
 │  └──────────────────────────────────┬───────────────────────────────────────┘  │
 └─────────────────────────────────────┼──────────────────────────────────────────┘
                                       ▼
                         SQLite file (local) │ PostgreSQL (production)
```

- **admin.Handler** (`internal/admin`, new): serves the pages and form actions under `/admin` and redirects `/` to `/admin/`. It renders embedded `html/template` files with one layout that carries the no-sign-in notice (R4) and the static CSS/JS. It calls the Store directly, using the shared checks. It is mounted only when `HOOKD_ADMIN_UI` is on (R3).
- **Test receiver** (part of `internal/admin`): `POST /admin/test-receiver/{trcv_id}` loads the receiver and its endpoint's secret, verifies the request with the official library, stores the request, and answers with the receiver's configured status (R17–R21).
- **validate** (`internal/validate`, new): the URL, description, event type, idempotency key and payload checks, moved out of `internal/api` so the API and the UI apply the same rules (N4). The API's behaviour and error codes do not change.
- **settings.Manager** (`internal/settings`, new): builds the effective configuration from the environment plus the overrides saved in the `settings` table, checks it with the same parser as start-up, and gives the current value to everything that uses it (R22–R26).
- **api.Server** (changed): a new option `WithAdmin(h)` routes `/` and `/admin/…` to the admin handler, inside the existing logging and panic recovery. Every other path keeps the JSON 404. Max payload comes from the settings manager instead of a fixed value.
- **delivery.Pool** (changed): reads its settings from the manager. A supervisor goroutine starts or stops workers when `HOOKD_WORKERS` changes. Each send uses a context timeout taken from the current `HOOKD_DELIVERY_TIMEOUT` instead of a fixed client timeout.
- **Store** (changed): new methods and three new tables, listed below. It takes the idempotency window as a function, so a change applies to the next event.

## Pages and routes

All under `/admin`. Pages are GET. Actions are POST forms that redirect to a page afterwards (post/redirect/get). When a form has errors, the form is shown again with the input kept and the messages above it (R5). The account is part of the URL, so every page below `/admin/accounts/{acct}` is scoped to that account (R10), and an ID from another account answers the "not found" page.

| Route | What it does |
|---|---|
| `GET /` | Redirect to `/admin/` (R1) |
| `GET /admin/` | Account list, paged, with endpoint and key counts, and a "new account" form (R6) |
| `POST /admin/accounts` | Create the account and its first key, then show the key once (R7, R8) |
| `GET /admin/accounts/{acct}` | Account overview: API keys and links to endpoints and events |
| `POST /admin/accounts/{acct}/keys` | Create a key and show it once (R8) |
| `GET`/`POST /admin/accounts/{acct}/keys/{key}/revoke` | Confirm page, then revoke (R9) |
| `GET /admin/accounts/{acct}/endpoints` | Endpoint list, paged, with test endpoints marked (R11) |
| `GET`/`POST /admin/accounts/{acct}/endpoints/new` | Create an endpoint (R11) |
| `GET`/`POST /admin/accounts/{acct}/test-endpoints/new` | Create a test endpoint: event types, description and answer status (R17) |
| `GET /admin/accounts/{acct}/endpoints/{ep}` | Edit form. For a test endpoint, also its answer status and received requests (R20) |
| `POST /admin/accounts/{acct}/endpoints/{ep}` | Update URL, description, event types (R11) |
| `POST /admin/accounts/{acct}/endpoints/{ep}/enable` and `/disable` | Enable or disable (R11) |
| `GET`/`POST /admin/accounts/{acct}/endpoints/{ep}/delete` | Confirm page, then soft delete (R13) |
| `GET /admin/accounts/{acct}/endpoints/{ep}/secret` | Page showing the secret, with a copy button. The secret is never in other pages (R12). |
| `POST /admin/accounts/{acct}/endpoints/{ep}/test-status` | Set the receiver's answer status, 100–599 (R19) |
| `POST /admin/accounts/{acct}/endpoints/{ep}/test-requests/clear` | Clear the received requests (R20) |
| `GET /admin/accounts/{acct}/events` | Event list, newest first, `?type=` filter, `?before=` cursor, 50 per page (R15) |
| `GET`/`POST /admin/accounts/{acct}/events/new` | Send-event form (R14) |
| `GET /admin/accounts/{acct}/events/{msg}` | Event detail. It refreshes every 2 s while any delivery is pending or in progress (R16) |
| `GET /admin/settings` | Settings with value and source, edit forms for editable ones, and health (R22–R27) |
| `POST /admin/settings/{name}` and `/admin/settings/{name}/reset` | Save or reset a setting (R23, R25) |
| `POST /admin/test-receiver/{trcv_id}` | Built-in receiver (R17–R19) |
| `GET /admin/static/…` | Embedded CSS and the copy-button script |

With `HOOKD_ADMIN_UI=false` the handler is not mounted, so every one of these answers the API's JSON 404 (R3).

Sending an event from the UI goes through the same steps as `POST /v1/events`: the shared checks, `Store.CreateEvent`, and the wake signal to the workers. The admin handler gets the same wake channel. An idempotency hit shows "existing event returned" with a link (R14).

## Built-in test receiver

- **Create (R17)**: in one Store transaction, a new `trcv_…` ID is made, the endpoint is created with the URL `{receiver base URL}/admin/test-receiver/{trcv_id}`, and the `test_receivers` row links the two with an answer status (default 200). The endpoint is an ordinary endpoint with an ordinary secret, so delivery, signing and the API treat it like any other.
- **Receiver base URL**: `HOOKD_ADMIN_RECEIVER_URL`. When it is empty, it is derived from `HOOKD_LISTEN_ADDR`: `http://127.0.0.1:<port>`, or `http://<host>:<port>` when the listen address names a specific host. Every instance's workers therefore deliver to their own instance over loopback. Because requests are stored in the database, it does not matter which instance received one (R21). The URL is fixed when the test endpoint is created.
- **Receive (R18)**: the handler reads the body (up to the current max payload plus 64 KB), loads the receiver and its endpoint's secret, and checks the request with `standardwebhooks.NewWebhook(secret).Verify(body, headers)`. It then stores one `test_requests` row: time, `webhook-id`, the event type (looked up from the account's event with that ID, if any), the headers, the body, whether it verified, the verify error, and the status answered. Then it answers with the configured status and a short JSON body. If the receiver is unknown or its endpoint is deleted, it answers 404 and stores nothing.
- **Keep the last 100 (R20)**: the insert and a delete of that receiver's rows beyond the newest 100 run in the same transaction. Deleting the endpoint deletes its `test_requests` rows. Clearing deletes them too.
- Recorded headers and bodies are stored but never logged (N3). Request logging already logs only the method, path and status.

## Settings at run time

- **Effective configuration**: `config.FromLookup` gets a lookup that returns the DB override for a name if there is one, and the environment variable otherwise. The start-up parser is therefore also the checker for UI values, including the lease > timeout rule (R25). The manager also records the source of each value: default, environment or UI (R22).
- **Editable (R23)**: `HOOKD_WORKERS`, `HOOKD_POLL_INTERVAL`, `HOOKD_DELIVERY_TIMEOUT`, `HOOKD_DELIVERY_LEASE`, `HOOKD_MAX_PAYLOAD_BYTES`, `HOOKD_IDEMPOTENCY_WINDOW`, `HOOKD_LOG_LEVEL`. Every other setting is shown read-only, with "set with environment variables" (R26). `HOOKD_DB_PASSWORD` is shown as "set" or "not set" only.
- **Save**: the manager builds the configuration with the new value in place. If parsing fails, the form shows the parser's message and nothing is saved (R25). Otherwise it upserts the `settings` row and applies the change at once. **Reset** deletes the row.
- **Spread (R24)**: each instance reloads the `settings` table every 5 seconds (a constant). If the stored overrides together with that instance's environment are invalid (for example, a different environment on another machine), the instance keeps its last valid configuration, logs an error, and the settings page shows the problem.
- **Applying**: consumers call `manager.Current()` when they need a value, never storing a copy. The pool's supervisor resizes the worker set when the count changes. A worker that is stopped finishes its current delivery first. The log level is an `slog.LevelVar` that the manager sets. The overrides are loaded once before the workers and the server start, so a restart keeps them.
- `hookd migrate`, `account create` and the `apikey` commands keep reading the environment only. They do not use the settings that change at run time.

## Health (R27)

The settings page shows:
- the result of `Store.Ping`,
- the driver and either the SQLite path or the PostgreSQL host, port and database name (never the password),
- the migrations not yet applied, from a new `Store.PendingMigrations`,
- the counts of `pending` and `in_progress` deliveries, from `Store.CountDeliveriesByStatus`.

## Data model changes

New migration `0002_admin` for each dialect:

| Table | Columns | Notes |
|---|---|---|
| `settings` | `name` (PK), `value`, `updated_at` | Only UI overrides. No row means environment or default. |
| `test_receivers` | `id` (`trcv_…`, PK), `account_id`, `endpoint_id` (unique), `response_status` (default 200), `created_at` | Links a test endpoint to its receiver |
| `test_requests` | `id` (`treq_…`, PK), `receiver_id`, `received_at`, `webhook_id`, `event_type`, `headers` (JSON text), `body` (text), `verified`, `verify_error`, `response_status` | Indexed on (`receiver_id`, `id`). At most 100 per receiver. |

New index: `events (account_id, event_type, id)` for the filtered event list (N6).

New Store methods. Every method that is about an account's data takes the account ID, as now:
- `ListAccounts(limit, after)` returns accounts with endpoint and key counts. `GetAccount(id)`.
- `ListEvents(accountID, filter{EventType, Before, Limit})` returns events newest first, each with counts of deliveries by status.
- `CreateTestEndpoint(accountID, NewTestEndpoint{Description, EventTypes, ResponseStatus, BaseURL})`, `GetTestReceiver(id)` (with the endpoint's secret and whether it is deleted), `ListTestReceivers(accountID)`, `SetTestReceiverStatus(accountID, endpointID, status)`, `RecordTestRequest`, `ListTestRequests(accountID, endpointID, limit)`, `ClearTestRequests(accountID, endpointID)`.
- `DeleteEndpoint` also deletes the endpoint's test requests.
- `ListSettings()`, `SetSetting(name, value)`, `DeleteSetting(name)`.
- `PendingMigrations()`, `CountDeliveriesByStatus()`.

## Configuration

New settings, added to `internal/config` and `docs/configuration.md`. Both are start-up only.

| Variable | What it does | Default |
|---|---|---|
| `HOOKD_ADMIN_UI` | Serve the admin UI and the built-in test receiver under `/admin` | `true` |
| `HOOKD_ADMIN_RECEIVER_URL` | Base URL that the workers use to reach the built-in test receiver. It is put in new test endpoints' URLs. | empty, meaning `http://127.0.0.1:<port of HOOKD_LISTEN_ADDR>` |

The existing `HOOKD_*` settings keep their names and defaults. The seven listed under "Settings at run time" can also be overridden from the UI, and those overrides are stored in the database. Every other setting is still taken only from the environment.

## Local and production

The only outside component is still the **database**. This change adds no service. The new tables live in the same database and follow the same SQLite/PostgreSQL dialect layer and embedded migrations.

| | Local | Production |
|---|---|---|
| Database | SQLite `./hookd.db`, migrated automatically | PostgreSQL, through the existing `HOOKD_DB_*` settings |
| Admin UI | on, at http://localhost:8080/ | on by default. Set `HOOKD_ADMIN_UI=false` on instances reachable from untrusted networks, or limit `/admin` at the load balancer. The UI has no sign-in. |
| Test receiver | workers reach `http://127.0.0.1:8080` | each instance reaches itself over loopback. Requests are stored in PostgreSQL, so every instance shows them. |
| UI settings overrides | stored in SQLite | stored in PostgreSQL and picked up by every instance within 5 s |

Commands are unchanged: `go run ./cmd/hookd serve` to run and `go test ./...` to test.

**Clean store in tests**: as in Hookd 1/6, each test opens a new SQLite database in `t.TempDir()` and migrates it. The new Store methods join the shared store suite, so they also run on PostgreSQL with `HOOKD_TEST_PG=1`, each run in its own schema.

## Project layout changes

```
internal/admin/            handler, pages, test receiver
internal/admin/templates/  html/template files (embedded)
internal/admin/static/     style.css, copy.js (embedded)
internal/settings/         run-time settings manager
internal/validate/         checks shared by api and admin
internal/store/migrations/{sqlite,postgres}/0002_admin.sql
```

## Testing

- **validate**: table tests for each check. The existing API tests must still pass unchanged, which shows the API's behaviour did not move.
- **settings**: the lookup order and sources, refusing invalid values (including lease ≤ timeout), reset, reload from the database, and keeping the last valid configuration when stored overrides become invalid.
- **delivery**: the worker count grows and shrinks with the setting, and a changed timeout applies to the next send.
- **Store**: the new methods in the shared suite, including account scoping, keeping only the last 100 test requests, deleting them with the endpoint, and event list paging and filtering.
- **admin**: `httptest` tests for every route. They cover: the no-sign-in notice on every page; form errors keeping the input; a key shown once and only its prefix afterwards; secrets absent from pages other than the secret page; another account's IDs answering not found; a cross-site POST rejected (N5); and a 404 for `/admin` when the UI is off.
- **Test receiver**: a correctly signed request is stored as verified, a request signed with a wrong secret is stored as not verified with the reason, and the configured status is answered.
- **End to end**: an in-process server goes through the eight acceptance steps by driving the admin routes over HTTP, with the workers delivering to the built-in receiver.
