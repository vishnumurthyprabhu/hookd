# Design: Hookd 2/6 — Retries and delivery history

This design adds retries, the attempt history and 30-day retention to Hookd as it stands after Hookd 1/6 and the admin UI (see `docs/changes/001-hookd/design.md`). The stack is unchanged: one Go binary, the SQL database as the only outside component and as the work queue, SQLite locally and PostgreSQL in production. Requirement numbers (R…, N…) refer to this change's `requirements.md`.

## Options considered

How to keep a failed delivery waiting for its retry, and where to keep the attempts:

| Option | Fit to requirements | Complexity | Cost to run and maintain |
|---|---|---|---|
| **A. Retry time on the delivery row, attempts in a new table.** A failed delivery goes back to `pending` with `next_attempt_at`; the claim query only takes rows that are due. Each attempt is one row in `attempts`. | Good. Waiting retries are rows, not goroutines, so they survive restarts, are shared by instances and hold no worker (R6, R7). One transaction records the attempt and the next step (N1). | Low: two columns, one table, a changed claim query | Nothing new to run |
| B. One queue row per attempt: a failed attempt inserts a new `deliveries` row for the next one | Fits, but the delivery's status (R9) has to be pieced together from several rows, and `(event_id, endpoint_id)` stops being unique | Medium: every reader of `deliveries` changes | Nothing new to run, but more rows and harder queries |
| C. In-process timers (`time.AfterFunc`) that re-queue the delivery when due | Poor. A restart loses waiting retries unless they are also stored, which brings back A. Several instances would each need to own their timers. | Medium | Nothing new to run, but fragile |

**Chosen: A.** It extends the queue Hookd already has with the least change: the `deliveries` row stays the one place that says where a delivery stands, and the claim query, already safe with `SKIP LOCKED`, only gains a "due" condition.

For retention, the options were a periodic batched delete and, on PostgreSQL, tables partitioned by day and dropped whole. **Chosen: the batched delete.** It works the same on SQLite and PostgreSQL, and at the scale in the requirements (100,000 events per account) a delete of 500 events at a time is short. Partitioning can come later if volume needs it.

No new library is needed. The jitter uses `math/rand/v2`.

## Architecture

```
  Developer's app              Browser (admin)                     Receiver
       │                            │                                 ▲
       │ GET /v1/events/{id}        │ event page, endpoint page,      │ POST (first attempt
       │ GET …/attempts             │ settings (HOOKD_RETRY_SCHEDULE) │  and each retry)
       ▼                            ▼                                 │
 ┌──────────────────────────────── hookd serve ───────────────────────┼──────────┐
 │  HTTP API (+ attempt routes)      admin.Handler (+ attempt lists)  │          │
 │        │                                 │                         │          │
 │        └──────────────┬──────────────────┘                         │          │
 │                       ▼                                            │          │
 │   Delivery workers ── claim due delivery ──▶ send ─────────────────┘          │
 │        │   attempt n failed and n ≤ len(schedule):                            │
 │        │      next_attempt_at = now + schedule[n-1] ± 10%  → back to pending  │
 │        │   else: succeeded / failed (final)                                   │
 │        └── RecordAttempt (one transaction: attempts row + deliveries row)     │
 │                                                                              │
 │   Retention runner ── every hour ──▶ DeleteExpiredEvents (batches of 500)     │
 │                                                                              │
 │   settings.Manager ── Current() ──▶ RetrySchedule (workers), Retention        │
 │                                                                              │
 │  ┌──────────────────────────── Store (interface) ──────────────────────────┐ │
 │  │ events · deliveries (+ attempts, next_attempt_at) · attempts (new) · …   │ │
 │  └───────────────────────────────────┬─────────────────────────────────────┘ │
 └──────────────────────────────────────┼───────────────────────────────────────┘
                                        ▼
                          SQLite file (local)  │  PostgreSQL (production)
```

- **Delivery workers** (`internal/delivery`, changed): claim only deliveries that are due, number each attempt, decide from the current retry schedule whether a failure is retried and when, and record the attempt and the outcome together.
- **Store** (`internal/store`, changed): the new `attempts` table, the retry columns on `deliveries`, the attempt queries, and the retention delete.
- **HTTP API** (`internal/api`, changed): the delivery fields on `GET /v1/events/{id}` and three attempt routes.
- **Admin UI** (`internal/admin`, changed): attempts on the event and endpoint pages, the waiting-retry count in health, and `HOOKD_RETRY_SCHEDULE` as an editable setting.
- **Retention runner** (`internal/retention`, new): a goroutine started by `serve` that deletes expired events in batches.
- **Config and settings manager** (`internal/config`, `internal/settings`, changed): parse and check `HOOKD_RETRY_SCHEDULE` and `HOOKD_RETENTION`; the schedule becomes the eighth editable setting.

## Data model

New migration `0003_retries`, one file per dialect.

**`deliveries`, changed**

| Column | Change | Notes |
|---|---|---|
| `attempts` | new, `INTEGER NOT NULL DEFAULT 0` | Attempts recorded so far |
| `next_attempt_at` | new, timestamp, `NOT NULL` | When the delivery may be claimed. Set to the creation time for a new delivery, to the retry time after a failed attempt. Kept, but not used, once the delivery is final. |
| `status` | unchanged values | `pending` now also means "waiting for a retry" (`attempts > 0`) |
| `response_status`, `error`, `latency_ms`, `attempted_at` | unchanged | Now those of the last attempt |

New indexes: `(status, next_attempt_at)` for the claim, `(endpoint_id, status)` for disabling an endpoint. The old `(status, locked_until)` index stays for reclaiming expired leases.

**`attempts`, new**

| Column | Notes |
|---|---|
| `id` (`att_…`) | ULID, so ID order is time order |
| `account_id` | Copied from the event, so `GetAttempt` is scoped without a join |
| `delivery_id`, `event_id`, `endpoint_id` | |
| `number` | 1, 2, … within the delivery |
| `started_at` | |
| `outcome` | `succeeded` or `failed` |
| `response_status` | NULL if there was none |
| `latency_ms` | |
| `error` | NULL on success |

Unique on (`delivery_id`, `number`). Indexes: (`event_id`, `id`) for the event's list; (`endpoint_id`, `id`) and (`endpoint_id`, `outcome`, `id`) for the endpoint's list, with and without the outcome filter (N2).

**`events`**: a new index on `created_at` for the retention delete.

**Migrating existing data (N3)**, in the same migration:
- every delivery gets `next_attempt_at = created_at`;
- every `succeeded` or `failed` delivery with an `attempted_at` gets `attempts = 1` and one `attempts` row built from its recorded outcome, with the ID `'att_' || substr(id, 5)` (the delivery's ULID under the attempt prefix, so it is unique and sorts by the delivery's time);
- `pending` and `in_progress` deliveries keep `attempts = 0`.

SQLite adds the columns with `ALTER TABLE … ADD COLUMN` (with a default of `''` for `next_attempt_at`, filled in right after); PostgreSQL adds them and then sets `NOT NULL`.

## Key flows

**Claim (changed)**: a delivery can be claimed when it is `pending` and `next_attempt_at <= now`, or `in_progress` with an expired lease, as before. The oldest due one is taken first (`ORDER BY next_attempt_at, id`). `ClaimDelivery` also returns the number of attempts made so far. Waiting retries are only rows, so they hold no worker and do not delay new events, which are due at once (R7). Idle workers poll every `HOOKD_POLL_INTERVAL` (1 s), so a retry starts within about a second of its due time; no extra wake-up is needed.

**Delivery worker (changed)**
1. Claim a due delivery. This attempt's number is `attempts + 1`.
2. Load the job. If the endpoint is disabled or deleted, the outcome is `failed` with `endpoint_disabled`, and it is final (R4).
3. Otherwise sign with a new timestamp and the endpoint's current secret, and send (R5). Success and failure are judged as before (R1).
4. If it failed, it was not `endpoint_disabled`, and `number <= len(schedule)`, the next attempt is due at `now + schedule[number-1] × r`, with `r` drawn uniformly from 0.9 to 1.1 (R2). The schedule is read from the settings manager at this moment, so a changed schedule applies to the next wait scheduled (R8).
5. `Store.RecordAttempt` stores, in one transaction (N1): the `attempts` row, and the delivery's `attempts`, last-attempt fields, cleared lease and either its final status (`succeeded`/`failed`) or `pending` with the new `next_attempt_at`. The update only applies `WHERE status = 'in_progress' AND attempts = number - 1`, so an attempt reclaimed by another worker after a lease expired is recorded once.
6. Log `delivery attempt` with the delivery, event and endpoint IDs, the attempt number, the outcome and, if retried, `next_attempt_at` (N4).

An attempt cut short by a crash is not recorded. Its lease expires, and the attempt is made again with the same number.

**Disabling or deleting an endpoint (changed)**: `UpdateEndpoint` (with `enabled = false`) and `DeleteEndpoint` set, in the same transaction, `next_attempt_at = now` on that endpoint's `pending` deliveries. The workers then claim them at once and record the `endpoint_disabled` attempt, instead of leaving them waiting for hours (R4, R10). Enabling the endpoint again does nothing to them.

**Delivery status (R9)**: `GetEvent` also reads `attempts`, `attempted_at` and `next_attempt_at`. `next_attempt_at` is reported only while the delivery is `pending` and `attempts > 0`, so a new delivery has none.

**Attempt lists (R11–R14)**
- *Event's attempts*: check the event belongs to the account (else `ErrNotFound`), then `SELECT … FROM attempts WHERE event_id = ? [AND endpoint_id = ?] [AND id > ?] ORDER BY id LIMIT ?`.
- *Endpoint's attempts*: check the endpoint belongs to the account, including deleted endpoints (R14), then `WHERE endpoint_id = ? [AND outcome = ?] [AND id < ?] ORDER BY id DESC LIMIT ?`.
- *One attempt*: `WHERE id = ? AND account_id = ?`.

**Retention (R17–R19)**: the retention runner waits one minute after start, then runs once an hour. Each run computes `cutoff = now - HOOKD_RETENTION` and calls `Store.DeleteExpiredEvents(cutoff, 500)` until it deletes fewer than 500. Each call is one short transaction:
1. select up to 500 event IDs with `created_at < cutoff` and no delivery that is `pending` or `in_progress` (a safeguard for an event whose retries were held up by downtime), oldest first; on PostgreSQL with `FOR UPDATE SKIP LOCKED`, so two instances running at once take different events (R19);
2. delete their `attempts`, then their `deliveries`, then the events.

Expired events then answer 404 and "not found" with no special case (R17). Because `HOOKD_RETENTION` must be longer than the idempotency window, a deleted event's key is no longer honoured anyway. Test requests keep their own rule (the last 100) and are not touched.

## API

Added to what Hookd 1/6 has. Errors and auth are unchanged. Paged lists answer `{"data": [...], "has_more": bool}` like `GET /v1/endpoints`, with `limit` (default and maximum as for endpoints).

| Method & path | Purpose |
|---|---|
| `GET /v1/events/{id}/attempts` | The event's attempts, oldest first (R11). Query: `endpoint_id`, `limit`, `after` (attempt ID). |
| `GET /v1/endpoints/{id}/attempts` | The endpoint's attempts, newest first, also for a deleted endpoint (R12, R14). Query: `outcome` (`succeeded` or `failed`), `limit`, `before` (attempt ID). |
| `GET /v1/attempts/{id}` | One attempt (R13) |

An attempt:

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

Each entry of `deliveries` in `GET /v1/events/{id}` (and in the `POST /v1/events` answer) gains three fields (R9, N5):

| Field | Meaning |
|---|---|
| `attempts` | Attempts made so far |
| `last_attempt_at` | Start of the last attempt, or `null` |
| `next_attempt_at` | When the next retry is due, or `null` when none is waiting |

## Admin UI

- **Event page** (R15): for each delivery, the number of attempts, "next retry at …" when one is waiting, and a table of its attempts (number, time, outcome, HTTP status, latency, error) from the event's attempt list, up to 200 attempts with a "more" link (`?after=`) beyond that. The page refreshes every 2 s while any delivery is in progress or a retry is due within 10 s, every 30 s while a retry is waiting further off, and not at all once every delivery is final. The "Refresh" link stays.
- **Endpoint page** (R16): a "Recent attempts" section, newest first, 50 per page (`?attempts_before=`), with an outcome filter and a link from each row to its event. A deleted endpoint has no page, as before; its attempts still show on each event's page.
- **Event list**: unchanged; waiting retries are `pending` and count as pending (R16).
- **Settings**: `HOOKD_RETRY_SCHEDULE` is editable, `HOOKD_RETENTION` is shown read-only with "set with environment variables".
- **Health**: adds "waiting for a retry", from `Store.CountWaitingRetries` (pending deliveries with `attempts > 0`).

## Store interface changes

- `ClaimDelivery`: claims only due deliveries; `ClaimedDelivery` gains `Attempts`.
- `RecordDeliveryResult` is replaced by `RecordAttempt(ctx, deliveryID, AttemptResult) (Attempt, error)`. `AttemptResult` has `Number`, `Outcome`, `ResponseStatus`, `Error`, `Latency`, `StartedAt` and `NextAttemptAt *time.Time` (nil means final). It returns `ErrNotFound` if the delivery is not in progress with `Number - 1` attempts.
- `DeliveryStatus` gains `Attempts`, `LastAttemptAt` and `NextAttemptAt`.
- New: `ListEventAttempts(ctx, accountID, eventID, AttemptFilter)`, `ListEndpointAttempts(ctx, accountID, endpointID, AttemptFilter)`, `GetAttempt(ctx, accountID, id)`, `DeleteExpiredEvents(ctx, cutoff, limit) (int, error)`, `CountWaitingRetries(ctx) (int, error)`. `AttemptFilter` holds `EndpointID`, `Outcome`, `After`, `Before` and `Limit`.
- `UpdateEndpoint` and `DeleteEndpoint` make the endpoint's pending deliveries due at once when it is disabled or deleted.
- The dialect gains `skipLocked` (`" FOR UPDATE SKIP LOCKED"` on PostgreSQL, empty on SQLite) for the retention select.

## Configuration

Every setting still comes from an environment variable with a default that works locally; the editable ones (*) can be overridden in the admin UI. Two are new; the others are unchanged from Hookd 1/6.

| Variable | What it does | Default |
|---|---|---|
| `HOOKD_RETRY_SCHEDULE` * (new) | Waits before each retry, comma-separated Go durations, at most 20 (R8). `none` means no retries. Each wait gets ±10% jitter. | `5s,5m,30m,2h,5h,10h,10h` |
| `HOOKD_RETENTION` (new) | How long events, deliveries and attempts are kept (R18) | `720h` (30 days) |
| `HOOKD_LISTEN_ADDR` | Address the API listens on | `:8080` |
| `HOOKD_DB_DRIVER` | `sqlite` or `postgres` | `sqlite` |
| `HOOKD_DB_PATH` | SQLite file path | `./hookd.db` |
| `HOOKD_DB_HOST`, `HOOKD_DB_PORT`, `HOOKD_DB_NAME`, `HOOKD_DB_USER`, `HOOKD_DB_PASSWORD`, `HOOKD_DB_SSLMODE` | PostgreSQL connection | `localhost`, `5432`, `hookd`, `hookd`, empty, `disable` |
| `HOOKD_DB_MAX_CONNS` | Connection pool size | `10` |
| `HOOKD_WORKERS` * | Delivery goroutines | `8` |
| `HOOKD_POLL_INTERVAL` * | How often idle workers look for due work, which bounds how late a retry starts | `1s` |
| `HOOKD_DELIVERY_TIMEOUT` * | Per-attempt HTTP timeout | `15s` |
| `HOOKD_DELIVERY_LEASE` * | Claim lease, longer than the timeout | `60s` |
| `HOOKD_MAX_PAYLOAD_BYTES` * | Maximum payload size | `262144` |
| `HOOKD_IDEMPOTENCY_WINDOW` * | How long idempotency keys are honoured | `24h` |
| `HOOKD_AUTO_MIGRATE` | Migrate at start | `true` |
| `HOOKD_LOG_LEVEL` * | `debug`, `info`, `warn`, `error` | `info` |
| `HOOKD_LOG_FORMAT` | `text` or `json` | `text` |
| `HOOKD_ADMIN_UI` | Serve the admin UI | `true` |
| `HOOKD_ADMIN_RECEIVER_URL` | Base URL of the built-in test receiver | empty (derived) |

Checks in `config.FromLookup`, so they apply at start-up and to values saved in the UI (R8, R18):
- every wait in the schedule is a positive duration, and there are at most 20;
- `HOOKD_RETENTION` is longer than `HOOKD_IDEMPOTENCY_WINDOW`, and longer than the sum of the schedule plus 10% (its jitter).

An unset or empty variable keeps its default, as for every setting, which is why "no retries" is written `none` rather than an empty value. The retention runner's interval (1 hour) and batch size (500) are constants, since they do not differ between environments.

## Local and production

Nothing new to run. The database is still the only outside component, and the new table, columns and retention delete work on both dialects behind the same `Store` interface.

| | Local | Production |
|---|---|---|
| Database | SQLite file `./hookd.db` | PostgreSQL, set with the `HOOKD_DB_*` settings above |
| Waiting retries | rows in SQLite, sent by the one instance | rows in PostgreSQL, claimed by any instance with `SKIP LOCKED` |
| Retention | runs in the one instance | runs in every instance; `SKIP LOCKED` keeps them on different events |
| Migration | `0003_retries` applied at start | `hookd migrate` as a deploy step, or auto-migrate |

**Commands** are unchanged: `go run ./cmd/hookd serve` to run, `go test ./...` to test. To watch retries locally without waiting hours, set `HOOKD_RETRY_SCHEDULE=5s,10s,20s` in the environment or in the admin UI.

**Clean store in tests**: as before, each test opens a new SQLite database in `t.TempDir()` and migrates it, and the Store suite also runs on PostgreSQL with `HOOKD_TEST_PG=1`, in its own schema. The store's clock is injectable, so retention and due times are tested without sleeping.

## Project layout

```
internal/retention/                 new: the retention runner
internal/store/migrations/*/0003_retries.sql
internal/store/attempts.go          new: RecordAttempt, attempt lists, DeleteExpiredEvents
internal/api/attempts.go            new: the three attempt routes
internal/admin/templates/           event.html and endpoint.html show attempts
internal/ids/                       PrefixAttempt = "att_"
```

## Testing

- **config / settings**: parsing the schedule (`none`, bad durations, more than 20), the retention checks, the schedule saved from the UI and refused when it would break the retention rule.
- **Store**: claims skip deliveries that are not due and take them once due; `RecordAttempt` writes the attempt and the next step together, and refuses a second record of the same number; the migration fills attempts for existing deliveries (a test migrates a database holding 0002-era rows); disabling or deleting an endpoint makes its waiting deliveries due; the attempt lists with filters, cursors and account scoping, including a deleted endpoint; `DeleteExpiredEvents` removes old events with their rows in batches, keeps newer ones and those with unfinished deliveries. All on SQLite, and on PostgreSQL with `HOOKD_TEST_PG=1`.
- **delivery**: with a schedule of a few milliseconds and an `httptest` receiver: retry after failure, stop at success, final `failed` after the last wait, `none` meaning one attempt, jitter within ±10% (with an injected random source), the attempt number in logs, and a changed schedule applying to the next wait.
- **retention**: the runner deletes in batches until done, with a fake clock.
- **API**: the three routes, filters, paging, 400 for a bad `outcome`, 404 across accounts, and the new delivery fields.
- **admin**: the event page shows attempts and the next retry, the endpoint page lists attempts, the settings page edits the schedule.
- **End to end**: an in-process server with the built-in test receiver and schedule `5ms,10ms,20ms` walks the acceptance steps: fail, retry, switch the receiver to 200, succeed on attempt 3 with the same `webhook-id`; 4 attempts then `failed`; disabling while a retry waits; and a restart (a new pool on the same database) sending a retry that came due while it was stopped.
