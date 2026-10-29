# Test locally

## Run the tests

You need Go 1.26 or later and nothing else. From the repository folder:

```bash
go test ./...
```

The times differ from run to run:

```
ok  	hookd/cmd/hookd	1.038s
ok  	hookd/cmd/testreceiver	0.008s
ok  	hookd/internal/admin	4.189s
ok  	hookd/internal/api	1.874s
ok  	hookd/internal/config	0.004s
ok  	hookd/internal/delivery	2.571s
ok  	hookd/internal/ids	0.030s
ok  	hookd/internal/settings	0.387s
ok  	hookd/internal/signing	0.006s
ok  	hookd/internal/store	3.727s
?   	hookd/internal/store/storetest	[no test files]
ok  	hookd/internal/validate	0.005s
```

Every test opens a new SQLite database in its own temporary folder and migrates it, so tests never see each other's data and leave nothing behind. No service has to be running.

What is covered:

| Package | Tests |
|---|---|
| `cmd/hookd` | The commands, and the end-to-end acceptance test `TestAcceptance`: an in-process server with local receivers covering the acceptance steps of the API's requirements (two accounts, event type filtering, signatures checked with the official Standard Webhooks library, a 500 receiver counted as a failure, isolation between accounts). Also that `serve` mounts the admin UI only when `HOOKD_ADMIN_UI` is on (`TestServeAdminUI`, with `/healthz` and `/v1` working either way), and that a setting saved in the database before `serve` starts is in effect from the start (`TestServeAppliesSavedSettings`). |
| `internal/admin` | Every admin UI route over HTTP with `httptest`: the no-sign-in notice on every page; form errors keeping the input; an API key shown once and only its prefix afterwards, and a revoked key getting 401 from `/v1`; the same invalid endpoint and event input refused as by the API; secrets only on the secret page; another account's IDs answering not found; cross-site form posts refused; the event list's filter and paging, and the event page refreshing only while deliveries are pending; the built-in test receiver storing correctly and wrongly signed requests, answering the set status and keeping the last 100; saving, refusing and resetting settings; and the health section. `TestSendEventToTestEndpoint` goes end to end with real workers: an event sent from the UI is delivered to a test endpoint, ends up succeeded, and its request shows on the endpoint page as verified, with none of its headers or body in the logs. |
| `cmd/testreceiver` | The verifying test receiver. |
| `internal/api` | Every route, auth failures (401), isolation between accounts (404) and error responses. |
| `internal/delivery` | The worker pool and HTTP sender: success on 2xx, failure on other statuses (including 302, which is not followed), timeouts and refused connections, disabled endpoints, reclaiming after an expired lease, finishing in-flight deliveries at shutdown, the worker count growing and shrinking with `HOOKD_WORKERS`, and a changed timeout applying to the next send. |
| `internal/signing` | The signer, checked against the official Go library and a test vector from the spec. |
| `internal/settings` | The settings that change while Hookd runs: the order of UI value, environment and default, and their sources; refusing a non-editable name, a bad value and a lease not longer than the timeout, without saving; reset; picking up a change made in the database on reload; and keeping the last valid configuration when saved values become invalid. |
| `internal/store` | Storage, account scoping, idempotency (including a changed window), fan-out filtering, the claim/lease queue, and what the admin UI needs: both migrations, saved settings, account and event lists with paging and filtering, test receivers and their requests (only the newest 100 kept, deleted with the endpoint), and the health queries. Runs on SQLite, and also on PostgreSQL when asked (below). |
| `internal/validate` | The input checks shared by the API and the admin UI: URL, description, event types, idempotency key and payload. |

Useful flags:

```bash
go test -count=1 ./...                       # ignore cached results
go test -v -run TestAcceptance ./cmd/hookd   # one test, with its output
go test -v -run TestSendEventToTestEndpoint ./internal/admin   # the admin UI end to end
go vet ./...                                 # static checks
```

## Run the store suite on PostgreSQL

The `internal/store` tests can also run on PostgreSQL, to check the PostgreSQL SQL (both migrations, `0001_init` and `0002_admin`, included) and the `SKIP LOCKED` claim query. Without `HOOKD_TEST_PG=1` these tests are skipped (they show as `SKIP: .../postgres` with `-v`).

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

Open <http://localhost:8080/> and follow steps 3 to 8 of [Set up and run from scratch](../README.md#set-up-and-run-from-scratch). In short, these are the checks of the admin UI's acceptance steps:

1. The admin UI opens, with the **No sign-in** notice at the top.
2. Create the account `acme` and copy its API key. It works with the API, and the account page then shows only its first characters:

   ```bash
   curl -s http://localhost:8080/v1/endpoints -H "Authorization: Bearer hk_..."
   ```

3. Under **Endpoints**, create a test endpoint (**New test endpoint**) for `order.paid`, and an ordinary one (**New endpoint**) for `user.created`, with any URL such as `https://example.com/hooks`. **Show the signing secret** on the test endpoint shows its secret. `GET /v1/endpoints` lists both: a test endpoint is an ordinary endpoint.
4. Under **Events**, **Send an event** of type `order.paid` with the payload `{"order_id": "1001"}`. The event page shows one delivery, to the test endpoint, `succeeded` with status 200, and none to the `user.created` endpoint. The test endpoint's page lists the request under **Received requests** as **signature verified**.
5. On the test endpoint's page, set **Answer status** to `500` and send another event: it shows `failed` with HTTP status 500.
6. Under **Settings**, save `HOOKD_DELIVERY_TIMEOUT` as `20s`: its source becomes `UI`, and the next delivery uses it. Saving `90s` is refused, because it is not shorter than `HOOKD_DELIVERY_LEASE` (`60s`). **Reset** brings back `15s`.
7. On the account page, **Create API key**, then **Revoke** the first key and confirm with **Revoke key**: the `curl` above now answers 401, and works with the new key.
8. Stop Hookd and start it with the UI off. `/admin/` answers 404, and the API still works:

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
