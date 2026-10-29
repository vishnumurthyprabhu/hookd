# Test locally

## Run the tests

You need Go 1.26 or later and nothing else. From the repository folder:

```bash
go test ./...
```

The times differ from run to run:

```
ok  	hookd/cmd/hookd	0.395s
ok  	hookd/cmd/testreceiver	0.012s
ok  	hookd/internal/api	0.322s
ok  	hookd/internal/config	0.004s
ok  	hookd/internal/delivery	1.402s
ok  	hookd/internal/ids	0.006s
ok  	hookd/internal/signing	0.005s
ok  	hookd/internal/store	0.871s
?   	hookd/internal/store/storetest	[no test files]
```

Every test opens a new SQLite database in its own temporary folder and migrates it, so tests never see each other's data and leave nothing behind. No service has to be running.

What is covered:

| Package | Tests |
|---|---|
| `cmd/hookd` | The commands, and an end-to-end acceptance test: an in-process server with local receivers covering the acceptance steps of the requirements (two accounts, event type filtering, signatures checked with the official Standard Webhooks library, a 500 receiver counted as a failure, isolation between accounts). |
| `cmd/testreceiver` | The verifying test receiver. |
| `internal/api` | Every route, auth failures (401), isolation between accounts (404) and error responses. |
| `internal/delivery` | The worker pool and HTTP sender: success on 2xx, failure on other statuses (including 302, which is not followed), timeouts and refused connections, disabled endpoints, reclaiming after an expired lease, and finishing in-flight deliveries at shutdown. |
| `internal/signing` | The signer, checked against the official Go library and a test vector from the spec. |
| `internal/store` | Storage, account scoping, idempotency, fan-out filtering and the claim/lease queue. Runs on SQLite, and also on PostgreSQL when asked (below). |

Useful flags:

```bash
go test -count=1 ./...                       # ignore cached results
go test -v -run TestAcceptance ./cmd/hookd   # one test, with its output
go vet ./...                                 # static checks
```

## Run the store suite on PostgreSQL

The `internal/store` tests can also run on PostgreSQL, to check the PostgreSQL SQL and the `SKIP LOCKED` claim query. Without `HOOKD_TEST_PG=1` these tests are skipped (they show as `SKIP: .../postgres` with `-v`).

### 1. Get a PostgreSQL server

Any PostgreSQL 13 or later works. The quickest is a throwaway container:

```bash
docker run --rm -d --name hookd-pg -p 5432:5432 \
  -e POSTGRES_USER=hookd -e POSTGRES_PASSWORD=hookd -e POSTGRES_DB=hookd \
  postgres:17
```

Wait a few seconds for it to start (`docker logs hookd-pg` shows `database system is ready to accept connections`).

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

To see Hookd deliver to a real receiver, follow [Set up and run from scratch](../README.md#set-up-and-run-from-scratch). To try the whole service on PostgreSQL rather than SQLite, start the container above and run:

```bash
HOOKD_DB_DRIVER=postgres HOOKD_DB_PASSWORD=hookd go run ./cmd/hookd serve
```

The other `HOOKD_DB_*` defaults (`localhost`, `5432`, `hookd`, `hookd`, `sslmode=disable`) match the container. Run `account create` with the same two settings.
