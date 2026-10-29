# Test locally

## Run the tests

You need Go 1.26 or later and nothing else. From the repository folder:

```bash
go test ./...
```

The times differ from run to run:

```
ok  	hookd/cmd/hookd	1.937s
ok  	hookd/cmd/testreceiver	0.007s
ok  	hookd/internal/admin	7.671s
ok  	hookd/internal/api	2.790s
ok  	hookd/internal/config	0.005s
ok  	hookd/internal/delivery	3.430s
ok  	hookd/internal/ids	0.034s
ok  	hookd/internal/retention	0.154s
ok  	hookd/internal/settings	0.477s
ok  	hookd/internal/signing	0.005s
ok  	hookd/internal/store	5.525s
?   	hookd/internal/store/storetest	[no test files]
ok  	hookd/internal/validate	0.003s
```

Every test opens a new SQLite database in its own temporary folder and migrates it, so tests never see each other's data and leave nothing behind. No service has to be running. Retries are tested with schedules of a few milliseconds, and retention with a fake clock, so nothing waits for real minutes or days.

What is covered:

| Package | Tests |
|---|---|
| `cmd/hookd` | The commands, and two end-to-end acceptance tests. `TestAcceptance`: an in-process server with local receivers covering the acceptance steps of the API's requirements (two accounts, event type filtering, signatures checked with the official Standard Webhooks library, a 500 receiver counted as a failure, isolation between accounts). `TestRetriesAcceptance`: the acceptance steps of retries and delivery history, on an in-process server with the admin UI's built-in test receiver and the retry schedule `5ms,10ms,20ms` saved through the Settings page. A test endpoint answers 500 twice and is switched to 200, so the delivery succeeds on attempt 3, with three received requests sharing one `webhook-id`, all verified; the event's and the endpoint's attempt lists, with the failed-only filter and `GET /v1/attempts/{id}`; an endpoint that keeps answering 500 ending `failed` after 4 attempts; disabling an endpoint while a retry waits, ending `failed` with `endpoint_disabled` and no further request; a restart (a new server on the same database) sending a retry that came due while it was stopped; the retention runner deleting an old event with its deliveries and attempts; and another account getting 404 from every attempt route. Also that `serve` mounts the admin UI only when `HOOKD_ADMIN_UI` is on (`TestServeAdminUI`, with `/healthz` and `/v1` working either way), and that a setting saved in the database before `serve` starts is in effect from the start (`TestServeAppliesSavedSettings`). |
| `internal/admin` | Every admin UI route over HTTP with `httptest`: the no-sign-in notice on every page; form errors keeping the input; an API key shown once and only its prefix afterwards, and a revoked key getting 401 from `/v1`; the same invalid endpoint and event input refused as by the API; secrets only on the secret page; another account's IDs answering not found; cross-site form posts refused; the event list's filter and paging; the event page showing each delivery's attempts and next retry, with a "more" link beyond 200 attempts, and refreshing every 2 s while a delivery is in progress or a retry is due within 10 s, every 30 s while a retry waits further off, and not at all once every delivery is final; the endpoint page's recent attempts, newest first, with the outcome filter and paging; the built-in test receiver storing correctly and wrongly signed requests, answering the set status and keeping the last 100; saving, refusing and resetting settings, including the retry schedule (refused when it would break the retention rule) and `HOOKD_RETENTION` shown read-only; and the health section with its count of deliveries waiting for a retry. `TestSendEventToTestEndpoint` goes end to end with real workers: an event sent from the UI is delivered to a test endpoint, ends up succeeded, and its request shows on the endpoint page as verified, with none of its headers or body in the logs. |
| `cmd/testreceiver` | The verifying test receiver. |
| `internal/api` | Every route, auth failures (401), isolation between accounts (404) and error responses. The delivery fields `attempts`, `last_attempt_at` and `next_attempt_at`, and the three attempt routes with their filters and paging, 400 for a bad `outcome` or `limit`, and 404 for another account's event, endpoint and attempt. |
| `internal/config` | Parsing and checking every `HOOKD_*` setting, including the retry schedule (the default, `none`, bad durations, more than 20 waits) and both retention checks. |
| `internal/delivery` | The worker pool and HTTP sender: success on 2xx, failure on other statuses (including 302, which is not followed), timeouts and refused connections, disabled endpoints, reclaiming after an expired lease, finishing in-flight deliveries at shutdown, the worker count growing and shrinking with `HOOKD_WORKERS`, and a changed timeout applying to the next send. Retries, with a millisecond schedule and an `httptest` receiver (`retry_test.go`): a failure retried until the first success, `failed` after the last wait, `none` meaning one attempt, jitter within ±10%, a disabled endpoint stopping the retries with `endpoint_disabled`, and a schedule changed while running applying to the next wait. |
| `internal/retention` | The retention runner, with a fake clock and a small batch: deleting in several batches until done, reading `HOOKD_RETENTION` at each run, and stopping when its context ends. |
| `internal/signing` | The signer, checked against the official Go library and a test vector from the spec. |
| `internal/settings` | The settings that change while Hookd runs: the order of UI value, environment and default, and their sources; refusing a non-editable name (such as `HOOKD_RETENTION`), a bad value, a lease not longer than the timeout and a retry schedule that breaks the retention rule, without saving; a retry schedule saved from the UI being applied; reset; picking up a change made in the database on reload; and keeping the last valid configuration when saved values become invalid. |
| `internal/store` | Storage, account scoping, idempotency (including a changed window), fan-out filtering, the claim/lease queue, and what the admin UI needs: all three migrations (including migrating a database holding 0002-era deliveries, which get one attempt each), saved settings, account and event lists with paging and filtering, test receivers and their requests (only the newest 100 kept, deleted with the endpoint), and the health queries. For retries and history: a delivery not yet due is not claimed and is claimed once due; `RecordAttempt` storing the attempt and the next step together, and refusing a second record of the same number; disabling or deleting an endpoint making its waiting retries due at once (and enabling it again changing nothing); the attempt lists with their filters, cursors and account scoping, including a deleted endpoint's; and `DeleteExpiredEvents` removing old events with all their rows, in batches, while keeping newer ones and those with unfinished deliveries. Runs on SQLite, and also on PostgreSQL when asked (below). |
| `internal/validate` | The input checks shared by the API and the admin UI: URL, description, event types, idempotency key and payload. |

Useful flags:

```bash
go test -count=1 ./...                       # ignore cached results
go test -v -run TestAcceptance ./cmd/hookd   # one test, with its output
go test -v -run TestRetriesAcceptance ./cmd/hookd   # retries, history and retention end to end
go test -v -run TestSendEventToTestEndpoint ./internal/admin   # the admin UI end to end
go vet ./...                                 # static checks
```

## Run the store suite on PostgreSQL

The `internal/store` tests can also run on PostgreSQL, to check the PostgreSQL SQL (all three migrations, `0001_init`, `0002_admin` and `0003_retries`, included), the `SKIP LOCKED` claim query, the attempt queries and the retention delete. The end-to-end tests in `cmd/hookd` and the other packages always use SQLite. Without `HOOKD_TEST_PG=1` these tests are skipped (they show as `SKIP: .../postgres` with `-v`).

### 1. Get a PostgreSQL server

Any PostgreSQL 13 or later works. The quickest is a throwaway container:

```bash
docker run --rm -d --name hookd-pg -p 5432:5432 \
  -e POSTGRES_USER=hookd -e POSTGRES_PASSWORD=hookd -e POSTGRES_DB=hookd \
  postgres:17
```

Wait a few seconds for it to start. `docker logs hookd-pg` shows `database system is ready to accept connections` twice: the first time is a temporary server that sets up the user and database and does not take connections on port 5432. The container is ready after `PostgreSQL init process complete; ready for start up.` and the second `ready to accept connections`.

To use a PostgreSQL server you already have instead, create a user and a database it owns, as a superuser in `psql`:

```sql
CREATE USER hookd WITH PASSWORD 'hookd';
CREATE DATABASE hookd OWNER hookd;
```

The user needs the right to create schemas in the database, which the owner has.

### 2. Run the tests

Point the `HOOKD_DB_*` settings at the database and set `HOOKD_TEST_PG=1`:

```bash
HOOKD_TEST_PG=1 \
HOOKD_DB_HOST=localhost HOOKD_DB_PORT=5432 \
HOOKD_DB_NAME=hookd HOOKD_DB_USER=hookd HOOKD_DB_PASSWORD=hookd \
HOOKD_DB_SSLMODE=disable \
go test -count=1 -v ./internal/store/ 2>&1 | grep -- '--- .*/postgres'
```

Every store test now also runs as a `/postgres` subtest, and each line should say `PASS`:

```
    --- PASS: TestClaimDeliveryLease/postgres (0.05s)
    ...
```

Drop the `| grep ...` to see the whole run, or use `go test ./...` to run everything else as well. Each PostgreSQL test creates its own schema, named `hookd_test_<random>`, and drops it when it ends, so the database is left as it was and runs can share it. If a run is killed part way, left-over `hookd_test_*` schemas can be dropped by hand.

### 3. Clean up

```bash
docker stop hookd-pg
```

## Try it by hand

### With the admin UI and its built-in receiver

No second terminal or separate receiver is needed. Start Hookd on a fresh database:

```bash
rm -f hookd.db hookd.db-wal hookd.db-shm
go run ./cmd/hookd serve
```

Open <http://localhost:8080/> and follow steps 3 to 9 of [Set up and run from scratch](../README.md#set-up-and-run-from-scratch). In short, these are the checks of the admin UI's acceptance steps and of retries and delivery history (retention is checked only by the tests, since it takes 30 days):

1. The admin UI opens, with the **No sign-in** notice at the top.
2. Create the account `acme` and copy its API key. It works with the API, and the account page then shows only its first characters:

   ```bash
   curl -s http://localhost:8080/v1/endpoints -H "Authorization: Bearer hk_..."
   ```

3. Under **Endpoints**, create a test endpoint (**New test endpoint**) for `order.paid`, and an ordinary one (**New endpoint**) for `user.created`, with any URL such as `https://example.com/hooks`. **Show the signing secret** on the test endpoint shows its secret. `GET /v1/endpoints` lists both: a test endpoint is an ordinary endpoint.
4. Under **Events**, **Send an event** of type `order.paid` with the payload `{"order_id": "1001"}`. The event page shows one delivery, to the test endpoint, `succeeded` with status 200, and none to the `user.created` endpoint. The test endpoint's page lists the request under **Received requests** as **signature verified**.
5. Under **Settings**, save `HOOKD_RETRY_SCHEDULE` as `5s,10s,20s`. On the test endpoint's page, set **Answer status** to `500` and send another event. The event page shows the first attempt failed with 500, then a second about 5 seconds later, also failed, and "next retry at …". `GET /v1/events/{id}` shows the delivery `pending` with `"attempts":2` and a `next_attempt_at`.
6. Before that retry, set **Answer status** back to `200`. The third attempt succeeds, the delivery shows `succeeded` after 3 attempts, and nothing more is sent. **Received requests** shows three requests with the same `webhook-id`, each **signature verified**. `GET /v1/events/{id}/attempts` lists the three attempts, and `GET /v1/endpoints/{id}/attempts?outcome=failed` the two failed ones, newest first. With the endpoint answering 500 throughout, another event ends `failed` after 4 attempts. Disabling the endpoint while a retry waits (set the schedule to `1h` to have time) ends the delivery `failed` with `endpoint_disabled` at once.
7. Under **Settings**, save `HOOKD_DELIVERY_TIMEOUT` as `20s`: its source becomes `UI`, and the next delivery uses it. Saving `90s` is refused, because it is not shorter than `HOOKD_DELIVERY_LEASE` (`60s`). **Reset** brings back `15s`. Saving `HOOKD_RETRY_SCHEDULE` as `400h,400h` is refused, because its total plus 10% (880h) is not shorter than `HOOKD_RETENTION` (`720h`). **Reset** it too, to get the default schedule back.
8. On the account page, **Create API key**, then **Revoke** the first key and confirm with **Revoke key**: the `curl` above now answers 401, and works with the new key.
9. Save `HOOKD_RETRY_SCHEDULE` as `1m`, set the test endpoint to answer 500, send an event, stop Hookd with Ctrl-C once its first attempt has failed, wait more than a minute and start it again: the retry is sent within a few seconds of the start.
10. Stop Hookd and start it with the UI off. `/admin/` answers 404, and the API still works:

   ```bash
   HOOKD_ADMIN_UI=false go run ./cmd/hookd serve
   curl -s http://localhost:8080/admin/            # {"error":{"code":"not_found","message":"not found"}}
   curl -s http://localhost:8080/v1/endpoints -H "Authorization: Bearer hk_..."
   ```

The test endpoint's URL is `http://127.0.0.1:8080/admin/test-receiver/trcv_...`. If you run Hookd on another port, with `HOOKD_LISTEN_ADDR=:8081` for example, new test endpoints use that port. Test endpoints created before keep their URL.

### With the API and cmd/testreceiver

To try the API with a separate receiver instead, follow [The same with the API](../README.md#the-same-with-the-api). `cmd/testreceiver` logs each request and whether it verified; the built-in receiver records the same in the database and shows it in the UI.

### On PostgreSQL

To try the whole service on PostgreSQL rather than SQLite, start the container above and run:

```bash
HOOKD_DB_DRIVER=postgres HOOKD_DB_PASSWORD=hookd go run ./cmd/hookd serve
```

The other `HOOKD_DB_*` defaults (`localhost`, `5432`, `hookd`, `hookd`, `sslmode=disable`) match the container. Run `account create` with the same two settings. The admin UI's Settings page then shows the PostgreSQL host, port and database under Health.
