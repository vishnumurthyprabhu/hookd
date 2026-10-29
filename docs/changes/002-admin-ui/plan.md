# Plan

This plan builds the admin UI described in `docs/changes/002-admin-ui/requirements.md` and `design.md`. R-numbers refer to those requirements. Every task keeps `go build ./...`, `go vet ./...` and `go test ./...` passing. Store work is tested in the shared store suite, so it also runs on PostgreSQL with `HOOKD_TEST_PG=1`. The `/v1` API and the CLI must behave exactly as before throughout.

## S1: Shared checks, settings and store

This stage prepares what the UI is built on: the shared input checks, the two new settings, and the new tables and Store methods. Nothing is visible in a browser yet. Each task is checked by its own tests and by the existing tests still passing.

### T1.1: Move input checks to internal/validate

Depends on: none

Create `internal/validate` holding the URL, description, event type list, event type pattern, idempotency key length and event payload checks (JSON object, size limit passed in) that live in `internal/api` today. Change `internal/api` to call them. The checks return a code and a message as now, so the API's error codes and messages do not change.

Done when `internal/validate` has table tests for each check, every existing `internal/api` test passes unchanged, and `internal/api` no longer has its own copies of these checks.

### T1.2: Admin settings in config

Depends on: none

Add `HOOKD_ADMIN_UI` (boolean, default `true`) and `HOOKD_ADMIN_RECEIVER_URL` (string, default empty) to `config.Config` and `FromLookup`. Add `Config.ReceiverBaseURL()`, which returns `HOOKD_ADMIN_RECEIVER_URL` when it is set. Otherwise it derives the URL from `HOOKD_LISTEN_ADDR`: `http://127.0.0.1:<port>` when the host is empty, `0.0.0.0` or `::`, and `http://<host>:<port>` otherwise. A trailing `/` is removed. A set receiver URL that is not an absolute `http` or `https` URL is a configuration error.

Done when config tests cover the defaults, parsing, invalid values and each case of the derived URL.

### T1.3: Migration 0002, settings table and health queries

Depends on: none

Add `0002_admin.sql` for SQLite and PostgreSQL. It creates `settings`, `test_receivers` and `test_requests` as in the design's data model, and the index `events (account_id, event_type, id)`. Add the Store methods `ListSettings`, `SetSetting` (upsert), `DeleteSetting`, `PendingMigrations` (versions not yet applied) and `CountDeliveriesByStatus`.

Done when migrating a fresh database and a database at 0001 both succeed on SQLite (and on PostgreSQL with `HOOKD_TEST_PG=1`), and the store suite covers the new methods. The suite must show that `PendingMigrations` returns 0002 before migrating and nothing after.

### T1.4: Account and event listing in the store

Depends on: T1.3

Add `GetAccount`, `ListAccounts(limit, after)`, which returns each account with its count of non-deleted endpoints and its count of API keys, and `ListEvents(accountID, filter)`, which returns events newest first with an optional exact event type, a `Before` cursor and a limit. Each listed event carries the counts of its deliveries by status, without the payload.

Done when the store suite covers paging in both lists, the event type filter, the delivery counts, and that `ListEvents` never returns another account's events.

### T1.5: Test receivers in the store

Depends on: T1.3

Add `CreateTestEndpoint`, which in one transaction makes a `trcv_` ID, creates the endpoint with the URL `<BaseURL>/admin/test-receiver/<trcv_id>` and a new secret, and inserts the `test_receivers` row (status default 200). Add `GetTestReceiver(id)`, which returns the receiver with its account, endpoint, secret and whether the endpoint is deleted. Also add `ListTestReceivers(accountID)`, `SetTestReceiverStatus`, `RecordTestRequest` (insert, then trim to the newest 100 for that receiver in the same transaction), `ListTestRequests` (newest first) and `ClearTestRequests`. Change `DeleteEndpoint` to also delete the endpoint's test requests. Add the `trcv_` and `treq_` prefixes to `internal/ids`.

Done when the store suite covers: creation and the URL it builds; account scoping on every method; keeping only the newest 100 requests; clearing them; that deleting the endpoint removes its requests; and that a test endpoint is listed by `ListEndpoints` like any other.

## S2: Settings that change while Hookd runs

This stage makes the seven editable settings take effect without a restart. Once it is done, a value written to the `settings` table changes Hookd's behaviour within 5 seconds, which tests can show before the UI exists.

### T2.1: Settings manager

Depends on: T1.2, T1.3

Create `internal/settings` with a `Manager` that does the following:
- builds the effective `config.Config` by calling `config.FromLookup` with a lookup that prefers the stored override and falls back to the environment;
- records each setting's source (default, environment or UI);
- gives `Current()` and a list of settings with their value, source and whether they are editable (only the seven in the design);
- shows `HOOKD_DB_PASSWORD` as set or not set only;
- has `Set(name, value)`, which checks the whole configuration with the new value, saves it only if it is valid and returns the parser's message if not, and `Reset(name)`;
- has `Run(ctx)`, which reloads every 5 seconds and keeps the last valid configuration (with the error available for the settings page) when the stored overrides become invalid;
- owns an `slog.LevelVar` that follows `HOOKD_LOG_LEVEL`.

Done when unit tests on a SQLite store cover the lookup order and sources, refusing a non-editable name, refusing a bad value and a lease ≤ timeout without saving, reset, a change made in the database being picked up by reload, and the last valid configuration being kept.

### T2.2: Use live settings in serve, workers, API and store

Depends on: T2.1

- **`delivery.Pool`**: change it to read `Workers`, `PollInterval`, `Timeout` and `Lease` from a function instead of a fixed `Config`. Add a supervisor that starts or stops workers to match `Workers`, where a stopped worker finishes its current delivery. Apply the timeout per send with a context deadline.
- **`api`**: give it an option that reads the max payload from a function.
- **`store`**: give it an option that reads the idempotency window from a function.
- **`serve`**: build the logger on the manager's `LevelVar`, load the overrides once after migrating and before starting the workers and the server, then run the manager in the background.
- **CLI**: `migrate`, `account create` and the `apikey` commands are unchanged.

Done when:
- pool tests show the worker count growing and shrinking and a changed timeout applying to the next send;
- an API test shows a lowered max payload refused without restarting;
- a store test shows a changed idempotency window applying to the next event;
- a `cmd/hookd` test shows an override saved in the database before `serve` starts being in effect;
- all existing tests pass.

## S3: Admin UI pages

This stage builds the UI itself. After T3.1, `go run ./cmd/hookd serve` shows the UI at http://localhost:8080/. Each later task adds pages you can open and use. All pages are server-rendered `html/template` files embedded in the binary, need no JavaScript to work, and are tested with `httptest`.

### T3.1: Admin skeleton and account list

Depends on: T1.2, T1.4

Create `internal/admin` with:
- the handler, embedded templates and static files;
- one layout carrying the no-sign-in notice (R4), simple CSS that stays usable in a narrow window, and `copy.js` for copy buttons;
- a shared error/not-found page;
- `http.CrossOriginProtection` around all its routes.

Add `api.WithAdmin(h)` so that `/` redirects to `/admin/` and `/admin/…` goes to the admin handler, inside the existing logging and panic recovery. Every other path keeps the JSON 404. In `serve`, mount the handler only when `HOOKD_ADMIN_UI` is true. Build `GET /admin/` (the paged account list with endpoint and key counts) and `POST /admin/accounts`, which creates the account and its first key and shows the key once with a copy button (R6, R7). An empty name re-renders the form with the error (R5).

Done when `httptest` tests show:
- `/` redirects;
- the notice is on every page;
- creating an account shows its key once, and the key works on `/v1/endpoints`;
- an empty name keeps the form and shows the error;
- a POST with `Sec-Fetch-Site: cross-site` is rejected;
- `/admin/` answers the JSON 404 when `HOOKD_ADMIN_UI=false`, while `/healthz` and `/v1` still work;
- running `serve` locally shows the page in a browser.

### T3.2: Account page and API keys

Depends on: T3.1

Build `GET /admin/accounts/{acct}`, which shows the account, its API keys (prefix and creation time) and links to its endpoints and events, and is reached from the account list (R10). Add `POST …/keys`, which creates a key and shows it once (R8), and `GET`/`POST …/keys/{key}/revoke`, a confirm page and then the revoke (R9). An unknown account or key, or a key belonging to another account, shows the not-found page.

Done when tests show a new key shown in full once and only by prefix afterwards, a revoked key getting 401 from `/v1`, the confirm page not revoking on GET, and another account's key ID being not found.

### T3.3: Endpoint pages

Depends on: T1.1, T3.2

Build the endpoint list (paged, test endpoints marked using `ListTestReceivers`), plus the create and edit forms using `internal/validate`, with event types entered as a comma- or line-separated list and empty meaning all. Add enable and disable, delete with a confirm page, and the secret page with a copy button (R11–R13). The secret appears on no other page. Form errors keep the input (R5).

Done when tests cover each action. They must show that the same invalid inputs the API refuses are refused here with a message, that the secret is absent from list and edit pages, that delete only happens on POST after the confirm page, and that another account's endpoint is not found.

### T3.4: Send events and event pages

Depends on: T1.1, T1.4, T3.2

Give the admin handler the workers' wake channel and the current max payload. Build the send-event form: event type, a JSON payload textarea and an optional idempotency key. It is checked with `internal/validate`, then `Store.CreateEvent` stores the event and the workers are woken. The page then shows the new event, or "existing event returned" on an idempotency hit (R14). Build the event list, newest first, 50 per page, with a `?type=` filter, a `?before=` cursor and the delivery counts for each event (R15). Build the event page: type, idempotency key, time, indented payload, and for each target endpoint its URL, status, HTTP status and error. The page refreshes every 2 seconds while any delivery is pending or in progress, and has a "Refresh" link (R16).

Done when tests show:
- an event sent from the form is delivered to an `httptest` receiver, and its page then shows succeeded;
- invalid JSON, a non-object payload, an oversized payload and a bad event type are refused without creating an event;
- a repeated idempotency key returns the existing event;
- the filter and paging work;
- the refresh tag is present only while deliveries are pending.

### T3.5: Built-in test receiver and test endpoint pages

Depends on: T1.5, T3.3

Build `POST /admin/test-receiver/{trcv_id}`. It reads the body (up to the current max payload plus 64 KB) and checks it with the official Standard Webhooks Go library and the endpoint's secret. It records the request with the event type looked up from the `webhook-id`, then answers with the configured status. An unknown receiver, or one whose endpoint is deleted, gets 404 and nothing is stored (R17–R19). Move the library from test-only use to a normal dependency in `go.mod`.

Also build:
- the "new test endpoint" form (description, event types, answer status), which uses `Config.ReceiverBaseURL()`;
- on a test endpoint's page, the answer status form (100–599) and the received requests, newest first, each showing time, `webhook-id`, event type, verified or the reason it failed, the status answered, and the headers and body, which can be expanded;
- a "clear" button (R20).

Done when tests show:
- a request signed with the endpoint's secret is stored as verified;
- a request signed with another secret is stored as not verified with the reason;
- the configured status (for example 500) is answered;
- only 100 requests are kept;
- an event sent from the UI to a test endpoint ends up succeeded, and its request appears on the endpoint page, with real workers in the test;
- nothing from the recorded headers or body appears in the logs.

### T3.6: Settings and health page

Depends on: T1.3, T2.2, T3.1

Build `GET /admin/settings`. It lists every `HOOKD_*` setting with its value and source. The seven editable ones have edit and reset forms, and the others are marked "set with environment variables", with the database password shown as set or not set (R22, R23, R26). It shows the manager's last reload error if there is one. Add `POST /admin/settings/{name}` and `…/reset`, which save through the manager and show the parser's message on an invalid value (R25). Add the health section: database reachable, the driver and the SQLite path or the PostgreSQL host, port and database name, pending migrations, and counts of pending and in-progress deliveries (R27).

Done when tests show:
- a saved value appears with the source "UI" and is in effect at once;
- a delivery timeout longer than the lease is refused with the message and not saved;
- a read-only setting cannot be posted;
- reset brings back the environment value;
- the password value never appears in the page;
- the health section shows the database and the delivery counts.

## S4: Acceptance and documentation

### T4.1: End-to-end acceptance test

Depends on: T2.2, T3.1, T3.2, T3.3, T3.4, T3.5, T3.6

Add an acceptance test in `cmd/hookd`, next to the existing one, that starts the real server and workers in-process on a fresh SQLite database. It goes through the eight acceptance steps of the requirements using only HTTP requests to the admin routes, plus `/v1` where a step checks an API key. The workers deliver to the built-in test receiver over loopback.

Done when the test passes with `go test ./...` and also with `HOOKD_TEST_PG=1` against PostgreSQL. A manual run of the same steps in a browser with `go run ./cmd/hookd serve` must also work.

### T4.2: Documentation for the admin UI

Depends on: T1.1, T1.2, T1.3, T1.4, T1.5, T2.1, T2.2, T3.1, T3.2, T3.3, T3.4, T3.5, T3.6, T4.1

Update the existing documents:

- **`README.md`** (setup from scratch on a new machine):
  - installing Go;
  - getting the code;
  - `go run ./cmd/hookd serve`;
  - opening http://localhost:8080/;
  - the walk-through done in the UI: create an account, a test endpoint, send an event, check the delivery and the received request, change a setting;
  - the curl and three-terminal walk-through kept as the API alternative.
- **`docs/configuration.md`**:
  - every setting with its default, including `HOOKD_ADMIN_UI` and `HOOKD_ADMIN_RECEIVER_URL`;
  - which seven settings can be changed in the UI;
  - that a UI value overrides the environment, and how to reset it;
  - that the others are environment-only.
- **`docs/deployment.md`** (deploying to production):
  - configuring the PostgreSQL node with the `HOOKD_DB_*` settings, as now;
  - that migration 0002 is applied by `hookd migrate` or auto-migrate;
  - that the UI has no sign-in, with how to turn it off with `HOOKD_ADMIN_UI=false` or restrict `/admin` at nginx (with an example `location` block);
  - when `HOOKD_ADMIN_RECEIVER_URL` must be set;
  - that UI setting changes reach every instance within 5 s.
- **`docs/testing.md`** (testing locally):
  - running `go test ./...`, and the store suite on PostgreSQL with `HOOKD_TEST_PG=1`;
  - the new acceptance test;
  - testing by hand with the built-in receiver instead of `cmd/testreceiver`.
- **`docs/api.md`**: a note that test endpoints are ordinary endpoints.
- **`docs/overview.md`**: mention the admin UI.
- **The architecture section of `CLAUDE.md`**: add `internal/admin`, `internal/settings` and `internal/validate`.

Done when someone who has never seen the project can follow each of the three from start to finish without other help: setting it up and running it on a new machine, deploying it to production with a separate PostgreSQL node, and testing it locally. Every command and setting in the documents must match the code.
