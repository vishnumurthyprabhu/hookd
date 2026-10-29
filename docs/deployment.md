<!-- Synced by Software Factory from commit a2e6af58e55011b0d8aa5fd8623257669503f524, 2026-10-29T07:27:02.822Z. Do not edit this line. -->
# Deploy to production

## What it is

Hookd sends webhooks on behalf of an application. Its users register their customers' endpoints (URLs) over a JSON HTTP API, choose which event types each endpoint receives, and send events. Hookd stores each event, delivers it in the background to every subscribed endpoint with an HTTP POST, and signs it under the Standard Webhooks spec (an open specification for signed webhooks: `webhook-id`, `webhook-timestamp` and `webhook-signature` headers, HMAC-SHA256), so receivers can check it with an existing library. In this version each delivery is attempted once, with a 15 second timeout and no redirects followed; failures are recorded, not retried.

What is deployed is one program, the `hookd` binary, run as `hookd serve`: it is the HTTP API, a browser admin UI under `/admin` and the delivery workers in one process. The admin UI has no sign-in, so in production it must be turned off or kept reachable only by trusted people (see [Keep the admin UI private](#keep-the-admin-ui-private)). The same binary also has the operator commands (`migrate`, `account create`, `apikey create`, `apikey list`, `apikey revoke`). Its only outside component is the database: a SQLite file locally (nothing to install), PostgreSQL in production. The database is also the delivery queue: instances claim deliveries with `FOR UPDATE SKIP LOCKED`, so they never send the same delivery at the same time, and nothing else (no cache, no broker, no file store) is needed.

## Components

- **hookd serve** (the project's own code, Go): the HTTP API on port 8080 (bearer API key authentication; every account sees only its own objects) and a pool of delivery workers in the same process. The API wakes the workers after each accepted event; idle workers also poll the database every second to pick up work left after a restart or by another instance. Workers claim a delivery with a lease, sign it, POST it once and record the result.
- **Admin UI** (the project's own code, Go, part of `hookd serve`): server-rendered pages under `/admin` on the same port, with no JavaScript framework and no files outside the binary. It creates accounts and API keys, manages endpoints, sends events, shows deliveries, and changes seven settings while Hookd runs. It has no sign-in. On by default; `HOOKD_ADMIN_UI=false` turns it off.
- **Built-in test receiver** (the project's own code, Go, part of the admin UI): `POST /admin/test-receiver/trcv_...`, the URL of each test endpoint created in the UI. It checks each request's signature with the official Standard Webhooks Go library, records the request in the database and answers with the status set in the UI. The delivery workers reach it over loopback on their own instance.
- **hookd operator commands** (the project's own code, Go, same binary): `hookd migrate` applies the database migrations, which are built into the binary; `hookd account create`, `hookd apikey create`, `hookd apikey list` and `hookd apikey revoke` manage accounts and their API keys. They are run by hand from a shell with the same settings as `serve`; they are not services.
- **PostgreSQL** (used, production): holds all state and is the delivery queue. PostgreSQL 13 or later.
- **SQLite** (used, local only): a single database file, `./hookd.db` by default, opened through a pure Go driver built into the binary, so nothing is installed. For one instance only.
- **Load balancer or reverse proxy** (used, production): terminates TLS in front of the instances, since Hookd serves plain HTTP. Any HTTP load balancer works; nginx is the example below.
- **testreceiver** (the project's own code, Go, development only, not deployed): a small separate webhook receiver that checks each request's signature with the official Standard Webhooks Go library and logs whether it verified. It listens on `127.0.0.1:9000`. The built-in test receiver does the same job from the admin UI.

## Deployment diagram

```text
                                 HTTPS :443
  API clients  ──────────────▶ ┌──────────────────────┐
  (Bearer API key)             │ load balancer / TLS  │
  admin's browser ── /admin ─▶ │ reverse proxy        │
  (trusted network only)       │ (/admin restricted)  │
                               └──────────┬───────────┘
                                          │ HTTP :8080
                          ┌───────────────┴───────────────┐
                          ▼                               ▼
               ┌─────────────────────┐         ┌─────────────────────┐
               │ hookd serve         │         │ hookd serve         │
               │  HTTP API  :8080    │   ...   │  HTTP API  :8080    │
               │  admin UI  /admin   │         │  admin UI  /admin   │
               │  delivery workers   │         │  delivery workers   │
               └───┬────────────┬────┘         └────┬───────────┬────┘
                   │            │  TCP :5432 (TLS)  │           │
                   │            ▼                   ▼           │
                   │       ┌─────────────────────────────┐      │
                   │       │ PostgreSQL 13+  db "hookd"  │      │
                   │       │ (all state + delivery queue)│      │
                   │       └─────────────────────────────┘      │
                   │                                            │
                   ▼  HTTP/HTTPS POST, signed                   ▼
          ┌──────────────────────────────────────────────────────────┐
          │ receivers: customers' webhook endpoints (outside, any URL)│
          └──────────────────────────────────────────────────────────┘

  operator shell: hookd migrate / account create / apikey ... ──▶ PostgreSQL :5432
```

- Load balancer → hookd serve: plain HTTP to port 8080 on each instance; no sticky sessions, for the API and the admin UI alike; health check on `GET /healthz`. The load balancer should let `/admin` through only from the admins' network, or not at all.
- hookd serve → itself: deliveries to test endpoints created in the admin UI go to the built-in test receiver over loopback, `http://127.0.0.1:8080/admin/test-receiver/...` by default (`HOOKD_ADMIN_RECEIVER_URL` changes it). They do not pass through the load balancer.
- hookd serve → receivers: outbound connections that each instance opens to whatever URL a customer registered, on the port in that URL; one POST per delivery, 15 s timeout, redirects not followed.
- Operator shell → PostgreSQL: the operator commands connect straight to the database with the same `HOOKD_DB_*` settings; they do not go through the API.

## Tech stack

- **Go 1.26** (`go 1.26.0` in the module file; 1.26 or later to build): the language and toolchain; Hookd's HTTP server, HTTP client, HMAC signing and logging (`log/slog`) all come from its standard library, with no web framework.
- **PostgreSQL 13 or later**: the production database and delivery queue.
- **SQLite** through `modernc.org/sqlite` v1.61.0: a pure Go SQLite driver (no cgo, no C compiler), used for the local database file.
- **pgx** `github.com/jackc/pgx/v5` v5.11.0: the PostgreSQL driver.
- **ULID** `github.com/oklog/ulid/v2` v2.1.2: time-sortable IDs, prefixed by type (`acct_`, `ep_`, `msg_`, `key_`, and `trcv_` and `treq_` for test receivers and their requests).
- **Standard Webhooks Go library** `github.com/standard-webhooks/standard-webhooks/libraries` v0.0.1: verifies signatures in the built-in test receiver, the tests and `cmd/testreceiver`. Production signing is Hookd's own code, so the check stays independent of it.
- **Admin UI**: Go's `html/template` and `embed`, with plain HTML forms, one CSS file and a small script for the copy buttons, all built into the binary. Cross-site form posts are refused with the standard library's `http.CrossOriginProtection`. No Node.js and no build step.

## Dependencies

To build:
- Go 1.26 or later. The Go modules are downloaded on the first build, so the build machine needs to reach the Go module proxy (or have the modules already).
- `git`, to get the code.

To run:
- Nothing for the binary itself: it is one static file with the migrations built in.
- Locally: nothing else (SQLite is built in).
- In production: a PostgreSQL 13 or later server reachable from every instance; a load balancer or reverse proxy that terminates TLS; outbound network access from every instance to the receivers' URLs.
- `curl`, to call the API and the health check by hand.

## Build

On the build machine, in the repository folder:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o hookd ./cmd/hookd
```

This makes one static file, `hookd`, with the migrations built in. Use `GOARCH=arm64` for ARM hosts. Copy it to each application host, for example to `/usr/local/bin/hookd`, and check it runs:

```bash
hookd help
```

To build everything in the repository (including the test receiver) and check it:

```bash
go build ./...
go vet ./...
go test ./...
```

## Run

**hookd serve**

```bash
hookd serve
```

or, from the repository folder without building first:

```bash
go run ./cmd/hookd serve
```

- Port: 8080 on every interface (`HOOKD_LISTEN_ADDR`, default `:8080`).
- Admin UI: `http://<host>:8080/` in a browser, which leads to `/admin/`. No sign-in. Off with `HOOKD_ADMIN_UI=false`, and then `/` and `/admin` answer 404.
- Health check: `GET http://<host>:8080/healthz`, no API key. It answers `200 {"status":"ok"}` when the instance can reach the database, and `503` with code `unavailable` when it cannot.

```bash
curl -s http://localhost:8080/healthz
```

On SIGTERM (or Ctrl-C), Hookd stops accepting requests, gives requests in flight up to 10 seconds to finish, and lets each worker finish the delivery it is sending (up to `HOOKD_DELIVERY_TIMEOUT`).

**Operator commands** (run, print and exit; no port):

```
hookd migrate                      Apply database migrations
hookd account create --name NAME   Create an account and print its API key
hookd apikey create --account ID   Create another API key for an account and print it
hookd apikey list --account ID     List an account's API keys
hookd apikey revoke --id KEY_ID    Revoke an API key so it no longer works
```

**testreceiver** (development only):

```bash
go run ./cmd/testreceiver --secret whsec_...
```

- Port: `127.0.0.1:9000` (`--addr` changes it). No health check. It answers every request with 200, or with the status given by `--status`, such as `--status 500`.

## Configuration

Hookd reads its settings from environment variables. Every setting has a default that works locally, so `hookd serve` runs with none set. A variable that is unset or empty keeps its default. The settings apply to every command (`serve`, `migrate`, `account create`, `apikey create`, `apikey list`, `apikey revoke`), so the operator commands must be run with the same `HOOKD_DB_*` settings as `serve`.

Seven settings, marked "Also in the UI" in the table below, can also be changed on the admin UI's Settings page while Hookd runs. A value saved there is stored in the database, overrides the environment variable on every instance, and reaches every instance within 5 seconds; **Reset** on that page goes back to the environment or default value. Only `serve` uses these saved values. Every other setting is environment-only and needs a restart to change. [Configuration](configuration.md#settings-changed-in-the-admin-ui) has the details.

Hookd checks every setting when it starts. If any is invalid it names each bad one and exits without doing anything:

```
hookd: invalid configuration:
  HOOKD_WORKERS="0": must be a whole number greater than 0
```

| Name | Purpose | Required | Default | Secret |
|---|---|---|---|---|
| `HOOKD_LISTEN_ADDR` | Address the API and the admin UI listen on, as `host:port`. `:8080` listens on every interface. | no | `:8080` | no |
| `HOOKD_DB_DRIVER` | Database to use: `sqlite` or `postgres`. | in production (`postgres`) | `sqlite` | no |
| `HOOKD_DB_PATH` | SQLite file path. `:memory:` keeps everything in memory and loses it on exit. SQLite only. | no | `./hookd.db` | no |
| `HOOKD_DB_HOST` | PostgreSQL host name or IP address. | in production | `localhost` | no |
| `HOOKD_DB_PORT` | PostgreSQL port, 1 to 65535. | no | `5432` | no |
| `HOOKD_DB_NAME` | PostgreSQL database name. | no | `hookd` | no |
| `HOOKD_DB_USER` | PostgreSQL user. | no | `hookd` | no |
| `HOOKD_DB_PASSWORD` | PostgreSQL password. | in production | empty | yes |
| `HOOKD_DB_SSLMODE` | PostgreSQL `sslmode`: `disable`, `allow`, `prefer`, `require`, `verify-ca` or `verify-full`. Use `verify-full` in production. | in production (`verify-full`) | `disable` | no |
| `HOOKD_DB_MAX_CONNS` | Largest number of open database connections, for each Hookd process. Applies to both drivers. | no | `10` | no |
| `HOOKD_WORKERS` | Number of delivery workers, so the most deliveries one process sends at once. Also in the UI. | no | `8` | no |
| `HOOKD_POLL_INTERVAL` | How often idle workers look for work. New events wake the workers at once; polling picks up work left after a restart or by another instance. Also in the UI. | no | `1s` | no |
| `HOOKD_DELIVERY_TIMEOUT` | How long an endpoint has to answer one delivery. A slower answer counts as a failure. Also in the UI. | no | `15s` | no |
| `HOOKD_DELIVERY_LEASE` | How long a claimed delivery stays locked to its worker. If the process dies while sending, another worker sends the delivery again once the lease has run out. Must be longer than `HOOKD_DELIVERY_TIMEOUT`; Hookd refuses to start otherwise. Also in the UI. | no | `60s` | no |
| `HOOKD_MAX_PAYLOAD_BYTES` | Largest event payload accepted, in bytes. Larger events are rejected with 413. Also in the UI. | no | `262144` (256 KB) | no |
| `HOOKD_IDEMPOTENCY_WINDOW` | How long an `Idempotency-Key` is remembered. Sending the same key again within this time returns the original event. Also in the UI. | no | `24h` | no |
| `HOOKD_AUTO_MIGRATE` | Apply database migrations when `serve` starts: `true` or `false`. | no | `true` | no |
| `HOOKD_LOG_LEVEL` | Lowest level logged: `debug`, `info`, `warn` or `error`. Also in the UI. | no | `info` | no |
| `HOOKD_LOG_FORMAT` | Log format: `text` or `json`. | no | `text` | no |
| `HOOKD_ADMIN_UI` | Serve the admin UI and the built-in test receiver under `/admin`: `true` or `false`. When `false`, `/` and `/admin` answer 404; the API and `/healthz` are not affected. The UI has no sign-in. | `false` on instances reachable from untrusted networks, unless `/admin` is restricted at the load balancer | `true` | no |
| `HOOKD_ADMIN_RECEIVER_URL` | Base URL that the delivery workers use to reach the built-in test receiver, put in new test endpoints' URLs. Empty means `http://127.0.0.1:<port>` when `HOOKD_LISTEN_ADDR` listens on every interface, or `http://<host>:<port>` when it names a host. See [When to set the receiver URL](#when-to-set-the-receiver-url). | in some setups | empty | no |
| `PGSSLROOTCERT` | Standard PostgreSQL variable, read by the PostgreSQL driver: the CA certificate file to trust for the database's certificate, when it is signed by your own CA. Without it the host's system CA certificates are trusted. | only with a private CA | unset | no |

Durations are Go durations, such as `500ms`, `15s`, `5m` or `24h`, and must be greater than zero. Choices such as `postgres` or `json` may be written in any case. Booleans accept `true`, `false`, `1` and `0`.

Hookd logs to standard error: one line per API or admin UI request (method, path and status), one per delivery, and one per startup step. API keys, signing secrets, `Authorization` headers, and the headers and bodies that the built-in test receiver records are never logged. Use `HOOKD_LOG_FORMAT=json` when the logs go to a log collector.

The test receiver takes flags instead of environment variables: `--secret` (endpoint signing secret, `whsec_...`, required, a secret), `--addr` (default `127.0.0.1:9000`) and `--status` (default `200`).

## Data

All of Hookd's state is in the database: accounts, API key hashes, endpoints with their signing secrets, events, delivery status, the settings saved in the admin UI, and the requests received by the built-in test receiver. There is no other file or store.

Tables, created by the built-in migrations (`0001_init`, and `0002_admin` for the last three before `schema_migrations`):

| Table | What it holds |
|---|---|
| `accounts` | ID (`acct_…`), name, creation time |
| `api_keys` | ID (`key_…`), account, SHA-256 hash of the key, its first characters, creation time. The key itself is shown once, at creation, and not stored. |
| `endpoints` | ID (`ep_…`), account, URL, description, event types (JSON array as text), enabled flag, signing secret (stored as is, because signing needs it), timestamps, soft-delete time |
| `events` | ID (`msg_…`), account, event type, payload (text, exactly as received), idempotency key, creation time |
| `deliveries` | one row per event and endpoint: status (`pending`, `in_progress`, `succeeded`, `failed`), response status, error, latency, attempt time, lease time. This table is the work queue. |
| `settings` | the setting values saved in the admin UI: name, value, update time. No row means the environment or the default applies. |
| `test_receivers` | ID (`trcv_…`), account, the test endpoint it belongs to, the status it answers with |
| `test_requests` | ID (`treq_…`), receiver, time, `webhook-id`, event type, the request's headers and body, whether the signature verified and why not, the status answered. Only the newest 100 per test endpoint are kept, and they are deleted with the endpoint. |
| `schema_migrations` | which migrations have been applied |

- **In production (PostgreSQL)**: these tables in the `public` schema of the `hookd` database.
- **Locally (SQLite)**: one database file at `HOOKD_DB_PATH`, `./hookd.db` by default, relative to the folder Hookd is started in. SQLite runs in WAL mode, so next to it there are `hookd.db-wal` and `hookd.db-shm` while it is open; they belong to the database and go with it. Hookd creates the file if it is missing; deleting it starts again from an empty database.

## Local deployment

1. Install Go 1.26 or later from <https://go.dev/dl/> and check it:

   ```bash
   go version
   ```

2. Install `git` and `curl`, then clone the repository:

   ```bash
   git clone <repository-url> hookd
   cd hookd
   ```

3. Start Hookd (terminal 1):

   ```bash
   go run ./cmd/hookd serve
   ```

   The first run downloads the Go modules, which can take a minute. Hookd then creates the SQLite database `./hookd.db`, applies its migrations and listens on port 8080:

   ```
   level=INFO msg="applied migration" version=0001_init
   level=INFO msg="applied migration" version=0002_admin
   level=INFO msg=listening addr=[::]:8080
   ```

4. Open <http://localhost:8080/> in a browser. It leads to the admin UI, where you can create an account and its API key, create a test endpoint that delivers to Hookd's built-in receiver, send an event, and watch its delivery and the request received. [Set up and run from scratch](../README.md#set-up-and-run-from-scratch) goes through it step by step.

5. Or use the API instead. Check that Hookd is up (terminal 2):

   ```bash
   curl -s http://localhost:8080/healthz
   ```

   ```json
   {"status":"ok"}
   ```

6. Create an account (terminal 2), and put the API key it prints in a variable:

   ```bash
   go run ./cmd/hookd account create --name acme
   export HOOKD_API_KEY=hk_...
   ```

7. Register an endpoint at `http://127.0.0.1:9000`, where the test receiver will listen, subscribed to `order.paid`, and note the `secret` (`whsec_...`) in the answer:

   ```bash
   curl -s -X POST http://localhost:8080/v1/endpoints \
     -H "Authorization: Bearer $HOOKD_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"url": "http://127.0.0.1:9000", "description": "local test receiver", "event_types": ["order.paid"]}'
   ```

8. Start the test receiver with that secret (terminal 3):

   ```bash
   go run ./cmd/testreceiver --secret whsec_...
   ```

9. Send an event (terminal 2). Hookd answers `202 Accepted`, and within a moment the test receiver logs `verified=true`:

   ```bash
   curl -s -X POST http://localhost:8080/v1/events \
     -H "Authorization: Bearer $HOOKD_API_KEY" \
     -H "Content-Type: application/json" \
     -H "Idempotency-Key: order-1001-paid" \
     -d '{"event_type": "order.paid", "payload": {"type": "order.paid", "order_id": "1001", "amount": 4200}}'
   ```

Stop Hookd with Ctrl-C. To start again from an empty database, delete `hookd.db` (Hookd recreates it at the next start).

## Local and production

| Component | Local | Production | Setting that switches it |
|---|---|---|---|
| Database | SQLite file `./hookd.db`, built in | PostgreSQL 13 or later on its own node | `HOOKD_DB_DRIVER=postgres` with `HOOKD_DB_HOST`, `HOOKD_DB_PORT`, `HOOKD_DB_NAME`, `HOOKD_DB_USER`, `HOOKD_DB_PASSWORD` |
| Database connection security | no TLS | TLS, certificate checked | `HOOKD_DB_SSLMODE=verify-full` (and `PGSSLROOTCERT` for a private CA) |
| Instances of `hookd serve` | one | one or more behind a load balancer, sharing one PostgreSQL database | none; needs `HOOKD_DB_DRIVER=postgres` |
| Migrations | applied automatically at start | `hookd migrate` as a deploy step, or auto-migrate | `HOOKD_AUTO_MIGRATE=false` |
| Logs | text on standard error | JSON on standard error, for a log collector | `HOOKD_LOG_FORMAT=json` |
| HTTPS for the API | plain HTTP on 8080 | Hookd still serves plain HTTP; production must put a TLS-terminating load balancer or reverse proxy in front | none |
| Process supervision | a terminal | a service manager that restarts it (systemd unit below) | none |
| Admin UI | on, at http://localhost:8080/ | off on instances reachable from untrusted networks, or `/admin` restricted at the load balancer; it has no sign-in | `HOOKD_ADMIN_UI=false` |
| Test receiver | the built-in one (test endpoints in the admin UI), reached at `http://127.0.0.1:8080`; or `go run ./cmd/testreceiver` | the built-in one, if the admin UI is on: each instance reaches itself over loopback, and the received requests are stored in PostgreSQL, so every instance shows them. `cmd/testreceiver` is not deployed. | `HOOKD_ADMIN_RECEIVER_URL` |
| Settings saved in the admin UI | stored in SQLite | stored in PostgreSQL and picked up by every instance within 5 s | none |

## Production needs

**Persistence.** Only the PostgreSQL database must survive a restart: the `hookd` database with all the tables under Data. The application hosts keep nothing; an instance can be replaced at any time. (SQLite's `./hookd.db` with its `hookd.db-wal` and `hookd.db-shm` is for local use only; anything that runs on it must keep that folder on a disk kept between restarts.)

**More than one copy.** Yes, on PostgreSQL: run as many identical instances as needed behind any HTTP load balancer, with no sticky sessions. Do not run more than one instance on SQLite. SQLite is for local use. Each instance opens up to `HOOKD_DB_MAX_CONNS` connections (10 by default), so make sure PostgreSQL's `max_connections` covers every instance with room to spare. Migrations take a database lock, so two runs at once cannot both apply the same migration.

**Delivery guarantee.** A delivery cut off by a crash is sent again by another worker once its lease (`HOOKD_DELIVERY_LEASE`) runs out, so receivers can get a duplicate and should drop repeats by `webhook-id`.

**Secrets to supply.** `HOOKD_DB_PASSWORD`. The database itself holds secrets (endpoint signing secrets and API key hashes), and API keys printed by `hookd account create` and `hookd apikey create`, or shown once in the admin UI, must be handed to customers over a secure channel.

**Outside services to reach.** The PostgreSQL server, from every application host and from wherever the operator commands are run; and the customers' webhook endpoints, outbound over HTTP or HTTPS, from every application host. Endpoint URLs only have to be absolute `http` or `https` URLs with a host; this version does not restrict which addresses they point at.

**Admin UI.** It has no sign-in: anyone who reaches `/admin` can create API keys, read every endpoint's signing secret and change settings. Turn it off with `HOOKD_ADMIN_UI=false`, or let only the admins' network reach `/admin`; see [Keep the admin UI private](#keep-the-admin-ui-private).

**TLS.** Hookd serves plain HTTP. Put the instances behind a load balancer or reverse proxy that terminates TLS, so API keys and the admin UI's pages never cross the network unencrypted. Connect to the database over TLS with `verify-full`.

**Health check.** `GET /healthz` on port 8080, no API key; `200` when the database is reachable, `503` when not.

**Shutdown.** Stop with SIGTERM and allow more than 10 seconds plus `HOOKD_DELIVERY_TIMEOUT` (15 s by default) before a forced kill.

The steps below assume Linux hosts, PostgreSQL 13 or later on its own node, and Go 1.26 or later on the machine that builds Hookd.

### 1. Build the binary

Build it as under Build above and copy `hookd` to each application host, for example to `/usr/local/bin/hookd`.

### 2. Prepare the PostgreSQL node

On the database node, open `psql` as a superuser:

```bash
sudo -u postgres psql
```

Create a user and a database owned by it. Choose a long random password:

```sql
SET password_encryption = 'scram-sha-256';
CREATE USER hookd WITH PASSWORD 'change-me';
CREATE DATABASE hookd OWNER hookd;
REVOKE ALL ON DATABASE hookd FROM PUBLIC;
GRANT CONNECT, CREATE, TEMPORARY ON DATABASE hookd TO hookd;
\c hookd
GRANT USAGE, CREATE ON SCHEMA public TO hookd;
\q
```

Hookd's tables go in the `public` schema. As the owner of the database, `hookd` can already do all of this on PostgreSQL 15 and later; the grants make it explicit and cover older versions, where `public` belongs to `postgres`. The `SET` line makes the password usable with the `scram-sha-256` login of step 3: PostgreSQL 13 stores new passwords as `md5` by default, and a user created that way cannot log in through a `scram-sha-256` line. It changes nothing on PostgreSQL 14 and later, where it is the default.

### 3. Let the application hosts connect

PostgreSQL must listen on the network and accept the `hookd` user from the application hosts.

In `postgresql.conf` (`SHOW config_file;` in `psql` shows where it is), set:

```
listen_addresses = '*'
```

or the node's private address instead of `*`. In `pg_hba.conf` (`SHOW hba_file;`), allow the application hosts' network, over TLS only:

```
# TYPE    DATABASE  USER   ADDRESS        METHOD
hostssl   hookd     hookd  10.0.1.0/24    scram-sha-256
```

Replace `10.0.1.0/24` with your application hosts' network. Each instance opens up to `HOOKD_DB_MAX_CONNS` connections (10 by default), so make sure `max_connections` covers every instance with room to spare.

### 4. Set up TLS to the database

Turn on TLS on the server in `postgresql.conf`, with a certificate whose name matches the host name Hookd connects to:

```
ssl = on
ssl_cert_file = '/etc/postgresql/tls/server.crt'
ssl_key_file  = '/etc/postgresql/tls/server.key'
```

The key file must belong to the `postgres` user and have mode `0600`. Restart PostgreSQL to apply steps 3 and 4:

```bash
sudo systemctl restart postgresql
```

On the Hookd side, `HOOKD_DB_SSLMODE` chooses how strict the connection is:

| `sslmode` | Encrypted | Server certificate checked |
|---|---|---|
| `disable` | no | no |
| `require` | yes | no |
| `verify-ca` | yes | signed by a trusted CA |
| `verify-full` | yes | signed by a trusted CA and matches `HOOKD_DB_HOST` |

Use `verify-full`. Hookd trusts the host's system CA certificates. If the database certificate is signed by your own CA, point Hookd at that CA's certificate with the standard PostgreSQL variable `PGSSLROOTCERT`:

```bash
export PGSSLROOTCERT=/etc/hookd/db-ca.crt
```

### 5. Set the database settings

On each application host, put the settings in a file only root can read, for example `/etc/hookd/hookd.env`:

```bash
HOOKD_DB_DRIVER=postgres
HOOKD_DB_HOST=db.internal.example.com
HOOKD_DB_PORT=5432
HOOKD_DB_NAME=hookd
HOOKD_DB_USER=hookd
HOOKD_DB_PASSWORD=change-me
HOOKD_DB_SSLMODE=verify-full
HOOKD_DB_MAX_CONNS=10
HOOKD_AUTO_MIGRATE=false
HOOKD_LOG_FORMAT=json
HOOKD_ADMIN_UI=false
```

```bash
sudo chmod 600 /etc/hookd/hookd.env
```

Add `PGSSLROOTCERT=/etc/hookd/db-ca.crt` if you need it (step 4). Every other setting is in the Configuration table above. `HOOKD_AUTO_MIGRATE=false` leaves migrations to the deploy step below. `HOOKD_ADMIN_UI=false` turns the admin UI off, which is the safe choice; leave it out (or set `true`) only on instances whose `/admin` is restricted as in [Keep the admin UI private](#keep-the-admin-ui-private).

To run the `hookd` commands below from a shell with these settings, load the file first:

```bash
set -a; . /etc/hookd/hookd.env; set +a
```

### 6. Apply the migrations

From one application host, with the settings loaded:

```bash
hookd migrate
```

```
applied 0001_init
applied 0002_admin
```

`0001_init` creates the API's tables and `0002_admin` the admin UI's (`settings`, `test_receivers`, `test_requests`) and an index on `events`. On a database of an earlier Hookd version, which has only `0001_init`, it prints just `applied 0002_admin`. Run it again at every deploy, before starting the new version. When there is nothing to do it says `database is up to date`. Migrations take a database lock, so two runs at once cannot both apply the same migration. If you would rather not have a separate step, leave `HOOKD_AUTO_MIGRATE=true` and each instance applies them, `0002_admin` included, when it starts. With `HOOKD_AUTO_MIGRATE=false` and a migration not yet applied, the admin UI's Settings page lists it under Health as not applied.

### 7. Run the instances

Run `hookd serve` on each application host, under a service manager that restarts it. A systemd unit, `/etc/systemd/system/hookd.service`:

```ini
[Unit]
Description=Hookd webhook delivery
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/hookd/hookd.env
ExecStart=/usr/local/bin/hookd serve
Restart=always
RestartSec=2
KillSignal=SIGTERM
TimeoutStopSec=30
DynamicUser=yes
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now hookd
sudo journalctl -u hookd -f
```

On SIGTERM, Hookd stops accepting requests, gives requests in flight up to 10 seconds to finish, and lets each worker finish the delivery it is sending (up to `HOOKD_DELIVERY_TIMEOUT`). Keep `TimeoutStopSec` above both. A delivery cut off by a crash is sent again by another worker once its lease (`HOOKD_DELIVERY_LEASE`) runs out, so receivers can get a duplicate and should drop repeats by `webhook-id`.

Hookd serves plain HTTP. Put the instances behind a load balancer or reverse proxy that terminates TLS, so API keys never cross the network unencrypted. Any HTTP load balancer works; there are no sticky sessions. For example, with nginx:

```nginx
upstream hookd {
    server 10.0.1.11:8080;
    server 10.0.1.12:8080;
}

server {
    listen 443 ssl;
    server_name hooks.example.com;
    ssl_certificate     /etc/nginx/tls/hooks.example.com.crt;
    ssl_certificate_key /etc/nginx/tls/hooks.example.com.key;

    client_max_body_size 1m;

    location / {
        proxy_pass http://hookd;
        proxy_set_header Host $host;
    }
}
```

Keep the load balancer's body size limit above `HOOKD_MAX_PAYLOAD_BYTES` plus a few KB, so Hookd rather than the proxy answers oversized events.

### Keep the admin UI private

The admin UI at `/admin` has no sign-in. Anyone who can reach it can use every page: create and revoke API keys, read every endpoint's signing secret, send events and change settings. Every page says so. Its forms are protected against being sent from other websites, but not against someone who can open the pages. Choose one of these:

- **Turn it off** on every instance that does not need it, with `HOOKD_ADMIN_UI=false` in `/etc/hookd/hookd.env` (step 5). Then `/` and everything under `/admin`, including the built-in test receiver, answer 404, while the API and `/healthz` work as before. Accounts and keys are then managed with the operator commands (step 9).
- **Restrict `/admin` at the load balancer**, so only the admins' network reaches it. With the nginx example above, add these `location` blocks to the `server` block, next to `location /`:

  ```nginx
      # Admin UI: only from the admins' network. It has no sign-in.
      location /admin {
          allow 10.0.9.0/24;
          deny  all;
          proxy_pass http://hookd;
          proxy_set_header Host $host;
      }

      # / only redirects to /admin/.
      location = / {
          return 404;
      }
  ```

  Replace `10.0.9.0/24` with the admins' network, or use `deny all;` alone to block `/admin` from the outside completely. Keep `proxy_set_header Host $host`: the UI compares a form's `Origin` with the `Host` header when the browser does not send `Sec-Fetch-Site`. The instances themselves must still not be reachable on port 8080 from untrusted networks, since that would bypass nginx; only the load balancer and the application hosts should reach it.

Turning the UI off changes nothing for the API: what the UI created (accounts, keys, endpoints, events, saved settings) stays in the database and keeps working. Saved settings still apply with the UI off.

### When to set the receiver URL

A test endpoint created in the admin UI has a URL that points at Hookd's built-in receiver, `<receiver base URL>/admin/test-receiver/trcv_...`. Every instance's workers deliver to that URL, like to any endpoint. The base URL is taken from `HOOKD_ADMIN_RECEIVER_URL` on the instance where the test endpoint is created, and is fixed in the endpoint's URL from then on.

When `HOOKD_ADMIN_RECEIVER_URL` is empty, it is `http://127.0.0.1:8080` with the default `HOOKD_LISTEN_ADDR=:8080`, so each instance delivers to itself over loopback, without going through nginx. Because the received requests are stored in PostgreSQL, it does not matter which instance received one: every instance's UI shows it. This works with no setting as long as every instance runs with the admin UI on and listens on the same port.

Set `HOOKD_ADMIN_RECEIVER_URL` (on every instance with the UI on) when that is not so:

- **Some instances run with `HOOKD_ADMIN_UI=false`** but share the database. Their workers would deliver to their own loopback, where the receiver answers 404, so those deliveries fail. Set it to an address that every application host can reach and that ends at an instance with the UI on, such as `http://10.0.1.11:8080` or an internal load balancer that sends `/admin` only to such instances.
- **The instances listen on different ports**, or `HOOKD_LISTEN_ADDR` names one host's own address (such as `10.0.1.11:8080`): the derived URL would point at one instance, or at a port the others do not use. Set a stable address that all of them can reach.
- **The workers cannot reach the instance at the listen address**, for example in containers where the port is published under another address. Set the address that works from inside the instance.

It must be an absolute `http` or `https` URL, such as `http://10.0.1.11:8080`. If it goes through nginx, `/admin` must be allowed from the application hosts too. Test endpoints created before the setting changed keep their old URL; delete them and create new ones.

### Settings changed in the admin UI

Values saved on the admin UI's Settings page (the seven marked "Also in the UI" under Configuration) are stored in the `settings` table and override the environment on every instance. The instance where a value is saved applies it at once, and every other instance reads the table every 5 seconds, so a change reaches all of them within 5 s, with no restart. If a saved value is invalid together with one instance's own environment (for example a saved `HOOKD_DELIVERY_TIMEOUT` longer than that instance's `HOOKD_DELIVERY_LEASE`), that instance keeps its last valid configuration, logs an error, and shows the problem on its Settings page. Keep the environment files of all instances the same to avoid this.

The values stay in the database across restarts and deploys. To go back to the environment file's value, click **Reset** on the Settings page. With the UI off, saved values still apply; to remove one without the UI, delete its row: `DELETE FROM settings WHERE name = 'HOOKD_WORKERS';`.

### 8. Check health

`GET /healthz` needs no API key. It answers `200 {"status":"ok"}` when the instance can reach the database, and `503` with code `unavailable` when it cannot. Point the load balancer's health check at it, and check each instance directly after a deploy:

```bash
curl -s http://10.0.1.11:8080/healthz
```

```json
{"status":"ok"}
```

### 9. Create accounts

Each Hookd customer gets an account and an API key. If the admin UI is on and reachable (see [Keep the admin UI private](#keep-the-admin-ui-private)), you can do all of this on its pages instead: **Create account** on the Accounts page, and **Create API key** and **Revoke** on an account's page. Otherwise, on any application host, with the settings loaded (step 5):

```bash
hookd account create --name acme
```

```
account_id: acct_01M636GV96GQFMA7V1G5N17DPQ
api_key:    hk_...
Keep the API key safe: it is shown only once.
```

Give the key to the customer over a secure channel. The customer can now call the API:

```bash
curl -s https://hooks.example.com/v1/endpoints \
  -H "Authorization: Bearer hk_..."
```

Hookd stores only a hash of the key, so a lost key cannot be shown again. Issue the account another key instead; its endpoints and events stay as they are:

```bash
hookd apikey create --account acct_01M636GV96GQFMA7V1G5N17DPQ
```

An account can have several keys, and each one keeps working when another is created. To retire a lost or leaked key, list the account's keys. Only each key's ID, its first characters and its creation time are shown, oldest first:

```bash
hookd apikey list --account acct_01M636GV96GQFMA7V1G5N17DPQ
```

```
ID                              PREFIX       CREATED
key_01M636GV9BW7N4XKQ2T8ZJ3RYD  hk_1r41QRqt  2026-10-29T07:02:11Z
key_01M637MFSVMW25MCX5BR3DGXNR  hk_Zq0b1Cq7  2026-10-29T07:05:20Z
```

Then revoke the old one by its ID:

```bash
hookd apikey revoke --id key_01M636GV9BW7N4XKQ2T8ZJ3RYD
```

```
revoked key_01M636GV9BW7N4XKQ2T8ZJ3RYD (hk_1r41QRqt...) of account acct_01M636GV96GQFMA7V1G5N17DPQ
```

From then on, requests with that key get `401`. The account's other keys, endpoints and events are not touched. To rotate a key, create the new one, give it to the customer, and revoke the old one once they have switched. Revoking an account's last key leaves it with none until `hookd apikey create` issues another.

### Upgrading

1. Build the new binary and copy it to each host.
2. Run `hookd migrate` once. Upgrading from the version without the admin UI, it applies `0002_admin`. Older instances keep working with it while they are replaced. Before restarting, add `HOOKD_ADMIN_UI=false` to the environment file, or restrict `/admin` at the load balancer, since the new version serves the UI by default.
3. Restart the instances one at a time (`sudo systemctl restart hookd`), checking `/healthz` after each.

### Backups

All of Hookd's state is in the PostgreSQL database: accounts, API key hashes, endpoints with their signing secrets, events, delivery status, the settings saved in the admin UI and the test receiver's requests. Back it up like any PostgreSQL database (for example with `pg_dump` or continuous archiving), and protect the backups as secrets, because they contain the signing secrets.
