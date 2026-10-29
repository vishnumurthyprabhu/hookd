# Plan

This plan builds Hookd 2/6 (retries, delivery history and retention) as described in `requirements.md` and `design.md` in this folder. Every task leaves `go build ./...`, `go vet ./...` and `go test ./...` passing on SQLite. Store tasks also pass with `HOOKD_TEST_PG=1` on PostgreSQL (see `docs/testing.md`).

## S1: Settings and schema

The new settings and the database changes that everything else builds on.

### T1.1: Retry schedule and retention settings

Depends on: none

In `internal/config`, add `RetrySchedule []time.Duration` (`HOOKD_RETRY_SCHEDULE`, default `5s,5m,30m,2h,5h,10h,10h`, `none` meaning an empty list, at most 20 positive durations) and `Retention time.Duration` (`HOOKD_RETENTION`, default `720h`). Add the checks to `FromLookup`: retention longer than `HOOKD_IDEMPOTENCY_WINDOW`, and longer than the sum of the schedule plus 10%. In `internal/settings`, add both definitions: `HOOKD_RETRY_SCHEDULE` editable, shown as comma-separated durations or `none`, and `HOOKD_RETENTION` read-only.

Done when config tests cover the default, `none`, bad durations, more than 20 waits and both retention checks, and settings tests show that a schedule saved from the UI is applied, that one breaking the retention rule is refused with nothing saved, and that `HOOKD_RETENTION` cannot be edited.

### T1.2: Migration 0003_retries

Depends on: none

Add `PrefixAttempt = "att_"` and `NewAttemptID` to `internal/ids`. Write `0003_retries.sql` for SQLite and PostgreSQL as in the design's data model:
- `deliveries.attempts` and `deliveries.next_attempt_at`;
- the `attempts` table, with its unique key and indexes;
- the new indexes on `deliveries` and on `events.created_at`;
- the backfill: `next_attempt_at = created_at`, and one attempt per finished delivery with an ID `'att_' || substr(id, 5)`.

Done when a migration test builds a database at 0002, inserts pending, in-progress, succeeded and failed deliveries, migrates it, and finds the expected columns, attempt rows and counts, on both dialects. `PendingMigrations` is empty afterwards.

## S2: Store

The Store methods for the retry queue, the attempt history and retention.

### T2.1: Claim due deliveries and record attempts

Depends on: T1.2

Change `ClaimDelivery` (both dialects) to take only `pending` deliveries with `next_attempt_at <= now`, or expired leases, ordered by `next_attempt_at, id`, and to return `Attempts`. New deliveries in `CreateEvent` get `next_attempt_at = now`. Replace `RecordDeliveryResult` with `RecordAttempt(ctx, deliveryID, AttemptResult) (Attempt, error)`. In one transaction it inserts the `attempts` row and updates the delivery: attempt count, last-attempt fields, lease cleared, and either a final status or `pending` with `NextAttemptAt`. The update is guarded by `status = 'in_progress' AND attempts = number - 1`. Add `Attempts`, `LastAttemptAt` and `NextAttemptAt` to `DeliveryStatus` in `GetEvent` (the next time only while pending with attempts > 0), and add `CountWaitingRetries`. Update `internal/delivery` to call `RecordAttempt` with the attempt number and no next attempt, so behaviour stays one attempt for now.

Done when Store tests show: a delivery not yet due is not claimed and is claimed once due; a recorded failure with a next time comes back as pending; a second record of the same number gets `ErrNotFound`; `GetEvent` reports the new fields; and `CountWaitingRetries` counts the right rows. Existing delivery tests still pass.

### T2.2: Disabled or deleted endpoints make waiting deliveries due

Depends on: T2.1

In `UpdateEndpoint` (when `enabled` becomes false) and `DeleteEndpoint`, set `next_attempt_at = now` on that endpoint's `pending` deliveries, in the same transaction.

Done when Store tests show that a delivery waiting hours for a retry becomes claimable at once after its endpoint is disabled, and after it is deleted, and that enabling the endpoint again changes nothing.

### T2.3: Attempt queries

Depends on: T2.1

Add `AttemptFilter`, `ListEventAttempts` (oldest first, `EndpointID`, `After`, `Limit`), `ListEndpointAttempts` (newest first, `Outcome`, `Before`, `Limit`, also for a deleted endpoint of the account) and `GetAttempt`, all scoped by account as the design describes.

Done when Store tests cover each filter and cursor, paging, a deleted endpoint's list, and `ErrNotFound` for another account's event, endpoint and attempt, on both dialects.

### T2.4: Delete expired events

Depends on: T2.1

Add the dialect field `skipLocked` and `DeleteExpiredEvents(ctx, cutoff, limit) (int, error)`. In one transaction it selects up to `limit` events older than `cutoff` that have no `pending` or `in_progress` delivery, oldest first, using `skipLocked`, then deletes their attempts, deliveries and the events.

Done when Store tests, using the injectable clock, show that old events go with all their rows, newer events stay, an old event with an unfinished delivery stays, and the limit is respected across repeated calls.

## S3: Retries and retention at run time

The worker behaviour and the background cleanup.

### T3.1: Retries in the delivery pool

Depends on: T1.1, T2.1, T2.2

Add `RetrySchedule` to `delivery.Config` and pass it from the settings manager in `serve`. In `handle`:
- this attempt's number is the claimed `Attempts + 1`;
- a failure that is not `endpoint_disabled`, with `number <= len(schedule)`, gets `NextAttemptAt = now + schedule[number-1] × r`, where `r` is uniform in [0.9, 1.1] from an injectable random source;
- anything else is final.

Each attempt is signed with a new timestamp. Log `delivery attempt` with the attempt number, the outcome and `next_attempt_at`. Update the package comments that say nothing is retried.

Done when pool tests with a millisecond schedule and an `httptest` receiver show:
- a failure is retried and stops at the first success;
- the delivery ends `failed` after the last wait;
- `none` means a single attempt;
- jitter stays within ±10%;
- a disabled endpoint stops the retries with `endpoint_disabled`;
- a schedule changed while running applies to the next wait.

### T3.2: Retention runner

Depends on: T1.1, T2.4

Add `internal/retention`. Its runner waits one minute, then runs once an hour. It reads `Retention` from the settings manager and calls `DeleteExpiredEvents` with batches of 500 until fewer than 500 come back, logging how many were deleted. The first delay, the interval and the clock are injectable for tests. Start it from `serve` next to the pool, and stop it on shutdown.

Done when retention tests with a fake clock and a small batch show the runner deleting in several batches until done and stopping on context cancel, and `go run ./cmd/hookd serve` starts and stops cleanly with it.

## S4: API and admin UI

What developers and the admin see.

### T4.1: API delivery fields and attempt routes

Depends on: T2.3

Add `attempts`, `last_attempt_at` and `next_attempt_at` to `deliveryResponse`. Add `internal/api/attempts.go` with these routes and the attempt JSON shape from the design:
- `GET /v1/events/{id}/attempts` (`endpoint_id`, `limit`, `after`);
- `GET /v1/endpoints/{id}/attempts` (`outcome`, `limit`, `before`);
- `GET /v1/attempts/{id}`.

Done when `httptest` tests cover every route with its filters and paging, 400 `invalid_request` for a bad `outcome` or `limit`, 401 without a key, 404 for another account's event, endpoint and attempt, and the new fields in `GET /v1/events/{id}`.

### T4.2: Admin event page with attempts

Depends on: T2.3

On the event page, show for each delivery:
- the attempt count;
- "next retry at …" when a retry is waiting;
- a table of its attempts (number, time, outcome, HTTP status, latency, error), up to 200 with a "more" link (`?after=`).

Set the refresh as in the design: every 2 s while a delivery is in progress or a retry is due within 10 s, every 30 s while a retry waits further off, and none once all deliveries are final.

Done when admin tests show the attempts and next retry for a retried delivery, the refresh interval in each of the three cases, and the "more" link when there are more than 200 attempts.

### T4.3: Admin endpoint attempts, health and settings

Depends on: T1.1, T2.3

On the endpoint page, add "Recent attempts": newest first, 50 per page with `?attempts_before=`, an outcome filter, and a link from each row to its event. In health, show "waiting for a retry" from `CountWaitingRetries`. On the settings page, `HOOKD_RETRY_SCHEDULE` gets an edit form and `HOOKD_RETENTION` is shown read-only.

Done when admin tests show the endpoint's attempts with paging and the filter, the health count, saving and resetting the schedule from the settings page, and a schedule that breaks the retention rule refused with the reason.

## S5: Acceptance and documentation

### T5.1: End-to-end acceptance test

Depends on: T3.1, T3.2, T4.1, T4.2, T4.3

Add an end-to-end test with an in-process server, workers and the built-in test receiver, using the schedule `5ms,10ms,20ms`, that walks the acceptance steps of `requirements.md`:
- fail, retry, switch the receiver to 200, and succeed on attempt 3, with three received requests that share one `webhook-id` and all verify;
- the event and endpoint attempt lists, with the failed-only filter;
- 4 attempts, then `failed`;
- disabling the endpoint while a retry waits;
- a restart (a new pool on the same database) that sends a retry which came due while it was stopped;
- retention removing an old event;
- 404 for another account.

Done when the test passes with `go test ./...`.

### T5.2: Documentation for retries, history and retention

Depends on: T1.1, T1.2, T2.1, T2.2, T2.3, T2.4, T3.1, T3.2, T4.1, T4.2, T4.3, T5.1

Update the project's documentation for this change:
- `README.md`: setting up and running from scratch on a new machine (Go 1.26, `go run ./cmd/hookd serve`), now with watching a retry locally by setting `HOOKD_RETRY_SCHEDULE=5s,10s,20s` and a test endpoint answering 500 and then 200.
- `docs/configuration.md`: every `HOOKD_*` setting with its default, including `HOOKD_RETRY_SCHEDULE` (format, `none`, jitter, editable in the UI) and `HOOKD_RETENTION` (environment only, its checks).
- `docs/deployment.md`: deploying to production with the PostgreSQL node and its `HOOKD_DB_*` settings, running `hookd migrate` for `0003_retries` on an existing database, and how retries and retention behave with several instances.
- `docs/testing.md`: running the tests locally, on SQLite and on PostgreSQL with `HOOKD_TEST_PG=1`, including the new end-to-end test.
- `docs/api.md`: the new delivery fields and the three attempt routes, with examples.
- `docs/overview.md`: retries, the attempt history and retention.
- `CLAUDE.md`: the architecture lines for `internal/delivery` (no longer "no retries") and the new `internal/retention`.

Done when someone new to the project can follow each of the three from start to finish: setting up and running it locally, deploying it with PostgreSQL, and running the tests. Every setting in `internal/config` appears in `docs/configuration.md` with its default.
