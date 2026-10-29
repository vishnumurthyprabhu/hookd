<!-- Synced by Software Factory from commit d5c3b034e8e0efb51a1436f012108efe50c332eb, 2026-10-29T18:53:28.407Z. Do not edit this line. -->
# Hookd documentation

## Prerequisites

- Go 1.26 or later (<https://go.dev/dl/>). Check it with `go version`.
- `git` and `curl`.
- Nothing else locally: the SQLite driver is pure Go and the database is a local file.
- For production: Linux hosts, PostgreSQL 13 or later on its own node, and a load balancer or reverse proxy that terminates TLS.
- For the AWS deployment in `deploy/aws`: Node.js with npm, and an AWS account set up as a CLI profile (the CDK takes the account and region from it).

## Quick local start

Use three terminals, all in the repository folder.

1. Get the code:

   ```bash
   git clone <repository-url> hookd
   cd hookd
   ```

2. Start Hookd (terminal 1). It creates `./hookd.db`, applies its migrations and listens on port 8080:

   ```bash
   go run ./cmd/hookd serve
   ```

   Check it from terminal 2:

   ```bash
   curl -s http://localhost:8080/healthz
   ```

   ```json
   {"status":"ok"}
   ```

   Or open <http://localhost:8080/> in a browser: it leads to the admin UI at `/admin/`, where you can do the steps below without a shell. There, **New test endpoint** creates an endpoint that delivers to the receiver built into Hookd, so `cmd/testreceiver` isn't needed. `README.md` goes through it step by step.

3. Create an account (terminal 2). Hookd prints an `account_id` and an `api_key`, and shows the key only once:

   ```bash
   go run ./cmd/hookd account create --name acme
   export HOOKD_API_KEY=hk_...
   ```

4. Register an endpoint that points at the test receiver. The answer includes the endpoint's `secret` (`whsec_...`):

   ```bash
   curl -s -X POST http://localhost:8080/v1/endpoints \
     -H "Authorization: Bearer $HOOKD_API_KEY" \
     -H "Content-Type: application/json" \
     -d '{"url": "http://127.0.0.1:9000", "description": "local test receiver", "event_types": ["order.paid"]}'
   ```

5. Start the test receiver with that secret (terminal 3). It listens on `127.0.0.1:9000` and logs whether each signature verified:

   ```bash
   go run ./cmd/testreceiver --secret whsec_...
   ```

   Flags: `--addr` (default `127.0.0.1:9000`) and `--status` (default `200`). For example, `--status 500` makes it answer with a failure.

6. Send an event (terminal 2). Hookd answers `202` straight away, and terminal 3 logs `verified=true`:

   ```bash
   curl -s -X POST http://localhost:8080/v1/events \
     -H "Authorization: Bearer $HOOKD_API_KEY" \
     -H "Content-Type: application/json" \
     -H "Idempotency-Key: order-1001-paid" \
     -d '{"event_type": "order.paid", "payload": {"type": "order.paid", "order_id": "1001", "amount": 4200}}'
   ```

7. Fetch the event by its `msg_...` ID. The delivery now shows `"status":"succeeded"` with `"attempts":1`, and `/attempts` lists that attempt with its time, outcome, HTTP status and latency:

   ```bash
   curl -s http://localhost:8080/v1/events/msg_... \
     -H "Authorization: Bearer $HOOKD_API_KEY"
   curl -s http://localhost:8080/v1/events/msg_.../attempts \
     -H "Authorization: Bearer $HOOKD_API_KEY"
   ```

8. To watch a retry, restart Hookd with a short schedule, `HOOKD_RETRY_SCHEDULE=5s,10s,20s go run ./cmd/hookd serve` (retry 5 s, 10 s and 20 s after each failure, so 4 attempts), and the test receiver with `--status 500`, then send an event without an idempotency key. The delivery stays `pending` with a `next_attempt_at` while each retry waits. Restart the receiver answering 200 before the next retry and the delivery ends `succeeded`; left at 500, it ends `failed` after the fourth attempt. `README.md` has this step by step, in the admin UI and with the API.

To start again with an empty database, stop Hookd and delete `hookd.db`.

To run Hookd locally on PostgreSQL instead, start a throwaway container and point Hookd at it. The other `HOOKD_DB_*` defaults match the container. Run `account create` with the same two settings.

```bash
docker run --rm -d --name hookd-pg -p 5432:5432 \
  -e POSTGRES_USER=hookd -e POSTGRES_PASSWORD=hookd -e POSTGRES_DB=hookd \
  postgres:17
HOOKD_DB_DRIVER=postgres HOOKD_DB_PASSWORD=hookd go run ./cmd/hookd serve
```

### Tests

```bash
go test ./...                                # all tests, on SQLite
go test -count=1 ./...                       # ignore cached results
go test -v -run TestAcceptance ./cmd/hookd   # one test, with its output
go test -v -run TestRetriesAcceptance ./cmd/hookd   # retries, history and retention end to end
go vet ./...                                 # static checks
```

Retries are tested with schedules of a few milliseconds and retention with a fake clock, so nothing waits for real minutes or days. The store suite, with all three migrations, the `SKIP LOCKED` claim, the attempt queries and the retention delete, can also run on PostgreSQL:

```bash
HOOKD_TEST_PG=1 \
HOOKD_DB_HOST=localhost HOOKD_DB_PORT=5432 \
HOOKD_DB_NAME=hookd HOOKD_DB_USER=hookd HOOKD_DB_PASSWORD=hookd \
HOOKD_DB_SSLMODE=disable \
go test -count=1 -v ./internal/store/ 2>&1 | grep -- '--- .*/postgres'
```

## Production deployment

In production, one or more identical `hookd serve` processes run behind a load balancer and share one PostgreSQL database. Don't run more than one instance on SQLite.

1. **Build** one static binary with the migrations built in, then copy it to each host, for example to `/usr/local/bin/hookd`. For ARM hosts, use `GOARCH=arm64`.

   ```bash
   CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o hookd ./cmd/hookd
   hookd help
   ```

2. **Prepare PostgreSQL** as a superuser (`sudo -u postgres psql`):

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

3. **Allow the application hosts to connect over TLS.**
   - In `postgresql.conf`, set `listen_addresses = '*'` (or the node's private address), and also `ssl = on`, `ssl_cert_file` and `ssl_key_file`.
   - In `pg_hba.conf`, add a line such as `hostssl hookd hookd 10.0.1.0/24 scram-sha-256`.
   - Restart with `sudo systemctl restart postgresql`.
   - Make sure `max_connections` covers `HOOKD_DB_MAX_CONNS` (10 by default) multiplied by the number of instances.
   - If your own CA signed the database certificate, set `PGSSLROOTCERT=/etc/hookd/db-ca.crt`.

4. **Settings.** Put them in `/etc/hookd/hookd.env` and run `sudo chmod 600 /etc/hookd/hookd.env`:

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

   The admin UI has no sign-in, so `HOOKD_ADMIN_UI=false` turns it off. Leave it on only where `/admin` is restricted at the load balancer to the admins' network (step 7).

   To load the settings into a shell before running `hookd` commands, use `set -a; . /etc/hookd/hookd.env; set +a`.

5. **Migrate** from one host, at every deploy, before starting the new version:

   ```bash
   hookd migrate
   ```

6. **Run** `hookd serve` on each host under systemd, with the unit file `/etc/systemd/system/hookd.service`:

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

7. **Load balancer.** Hookd serves plain HTTP, so terminate TLS in front of it. Sticky sessions aren't needed. Keep the proxy's body size limit above `HOOKD_MAX_PAYLOAD_BYTES` plus a few KB (for nginx, `client_max_body_size 1m;`). Point the health check at `GET /healthz`, which answers `200` when the database is reachable and `503` when it isn't. If the admin UI is on, let `/admin` through only from the admins' network, and keep `proxy_set_header Host $host` (the UI checks a form's `Origin` against `Host`).

8. **Upgrade:**
   1. Build the new binary and copy it to each host.
   2. Run `hookd migrate` once. From the version without the admin UI, it applies `0002_admin`. Since the new version serves the UI by default, set `HOOKD_ADMIN_UI=false` or restrict `/admin` before restarting.
   3. Restart the instances one at a time with `sudo systemctl restart hookd`, checking `/healthz` after each.

   Upgrading to the version with retries (migration `0003_retries`, which adds the `attempts` table and the retry columns of `deliveries`) is not a rolling upgrade: once it is applied, instances of the earlier version answer `500` to `POST /v1/events`. Stop every instance (`sudo systemctl stop hookd`), run `hookd migrate` from one host (it prints `applied 0003_retries`; on a large `deliveries` table it takes a while and locks it), then start every instance and check `/healthz`. Existing data is kept: each finished delivery gets one attempt row from its recorded outcome. From then on failed deliveries are retried, and events older than 30 days are deleted from about a minute after the first instance starts, so set `HOOKD_RETENTION` first if you need to keep them longer.

9. **Back up** the PostgreSQL database, for example with `pg_dump`. It holds all of Hookd's state, signing secrets and attempt history included, so protect the backups as secrets. Events older than `HOOKD_RETENTION` are deleted from the database and so from later backups; keep older backups if you need history beyond that. With retries on, each delivery stores up to 8 attempt rows, and the database holds about 30 days of traffic; size the disk for that.

Waiting retries need nothing set up with several instances: each is a row in `deliveries` with its due time, claimed with `SKIP LOCKED` by whichever instance is free, and it survives restarts and deploys. Every instance runs the hourly retention deletion and they share the work. Set `HOOKD_RETENTION` to the same value on every instance.

The full guide, with an nginx example, a `sslmode` table, and when to set `HOOKD_ADMIN_RECEIVER_URL` for the built-in test receiver, is in `docs/deployment.md`.

### AWS test box (`deploy/aws`)

`deploy/aws` is an AWS CDK app in TypeScript (stack `hookd`, set in `cdk.json`) that deploys Hookd to one EC2 instance:

- One VPC with a single public subnet and no NAT gateway, and a `t3.small` Amazon Linux 2023 instance with an encrypted 20 GB volume and IMDSv2 only.
- The repository is uploaded as a zip asset. At first boot the instance installs Go, builds `hookd` with `CGO_ENABLED=0`, and runs `hookd serve` under systemd as user `hookd`, on SQLite at `/data/hookd.db`, listening on `127.0.0.1:8080` with `HOOKD_AUTO_MIGRATE=true` and `HOOKD_LOG_FORMAT=json`.
- nginx on the instance terminates HTTPS on 443 with a self-signed certificate, so call it with `curl -k`. Port 80 serves only `/healthz` and redirects everything else to HTTPS. An Elastic IP keeps the address when the instance is replaced, and new source replaces the instance.
- Logs go to CloudWatch Logs. Operator commands run in a Session Manager shell on the instance, for example `sudo -u hookd bash -c 'set -a; . /etc/hookd/hookd.env; hookd account create --name NAME'`.

The stack doesn't set `HOOKD_ADMIN_UI`, so the admin UI is on. nginx lets `/admin` and `/` through on 443 only from one address, `adminCidr` in `deploy/aws/lib/hookd-stack.ts`, and answers `403` to everyone else; set it to your own public IP before deploying. The UI is also reachable on the instance's loopback through a Session Manager port forward. The API on 443 is open to anyone. Use this box only for testing.

### Settings

All settings are environment variables. Each one has a default that works locally, and the operator commands need the same `HOOKD_DB_*` values as `serve`. Eight of them (`HOOKD_WORKERS`, `HOOKD_POLL_INTERVAL`, `HOOKD_DELIVERY_TIMEOUT`, `HOOKD_DELIVERY_LEASE`, `HOOKD_MAX_PAYLOAD_BYTES`, `HOOKD_IDEMPOTENCY_WINDOW`, `HOOKD_LOG_LEVEL`, `HOOKD_RETRY_SCHEDULE`) can also be changed on the admin UI's Settings page. A saved value is stored in the database, overrides the environment on every instance within 5 seconds without a restart, and is used only by `serve`. **Reset** goes back to the environment or default value.

| Variable | Default |
|---|---|
| `HOOKD_LISTEN_ADDR` | `:8080` |
| `HOOKD_DB_DRIVER` (`sqlite` or `postgres`) | `sqlite` |
| `HOOKD_DB_PATH` (SQLite only, `:memory:` allowed) | `./hookd.db` |
| `HOOKD_DB_HOST` / `HOOKD_DB_PORT` | `localhost` / `5432` |
| `HOOKD_DB_NAME` / `HOOKD_DB_USER` / `HOOKD_DB_PASSWORD` | `hookd` / `hookd` / empty |
| `HOOKD_DB_SSLMODE` | `disable` |
| `HOOKD_DB_MAX_CONNS` | `10` |
| `HOOKD_WORKERS` | `8` |
| `HOOKD_POLL_INTERVAL` | `1s` |
| `HOOKD_DELIVERY_TIMEOUT` | `15s` |
| `HOOKD_DELIVERY_LEASE` (must be longer than the timeout) | `60s` |
| `HOOKD_MAX_PAYLOAD_BYTES` | `262144` |
| `HOOKD_IDEMPOTENCY_WINDOW` | `24h` |
| `HOOKD_AUTO_MIGRATE` | `true` |
| `HOOKD_LOG_LEVEL` (`debug`, `info`, `warn`, `error`) | `info` |
| `HOOKD_LOG_FORMAT` (`text` or `json`) | `text` |
| `HOOKD_RETRY_SCHEDULE` (waits before each retry, counted from the previous attempt, each varied by up to 10%; at most 20; `none` for no retries) | `5s,5m,30m,2h,5h,10h,10h` (8 attempts over about 27½ hours) |
| `HOOKD_RETENTION` (how long events, with their deliveries and attempts, are kept; environment only; must be longer than `HOOKD_IDEMPOTENCY_WINDOW` and than the retry schedule's total plus 10%) | `720h` (30 days) |
| `HOOKD_ADMIN_UI` (serve the admin UI and built-in test receiver under `/admin`) | `true` |
| `HOOKD_ADMIN_RECEIVER_URL` (base URL the workers use to reach the built-in test receiver) | empty (`http://127.0.0.1:8080` by default) |

If a setting is invalid, Hookd names it and exits without doing anything. The Settings page refuses a value that would make the configuration invalid, such as a retry schedule too long for `HOOKD_RETENTION`. `docs/configuration.md` describes each setting.

## How to use it

### Operator commands

```
hookd serve                        Run the API server, the admin UI and the delivery workers
hookd migrate                      Apply database migrations
hookd account create --name NAME   Create an account and print its API key
hookd apikey create --account ID   Create another API key for an account and print it
hookd apikey list --account ID     List an account's API keys
hookd apikey revoke --id KEY_ID    Revoke an API key so it no longer works
```

Hookd stores only a hash of each key, so a lost key can't be shown again. To rotate a key:

1. Create a new key with `apikey create`.
2. Give it to the customer.
3. Once they have switched, find the old key's ID with `apikey list` and revoke it with `apikey revoke`.

A revoked key gets `401`.

### Admin UI

Open `http://<host>:8080/`, which leads to `/admin/`. Every page has a "No sign-in" notice.

- **Accounts:** create an account. Its first API key is shown once, with a **Copy** button. An account's page lists its keys and has **Create API key** and **Revoke**.
- **Endpoints:** create, edit, disable, enable and delete endpoints. **Show the signing secret** shows the secret on request.
- **New test endpoint:** creates an endpoint at `/admin/test-receiver/trcv_...`, the built-in receiver. It checks each request's signature and answers with the **Answer status** you set (for example `500` to see failures). Its page lists the last 100 **Received requests**, with headers and body.
- **Endpoint page:** **Recent attempts** lists every attempt made to the endpoint, newest first, with a link to each attempt's event and an **Outcome** filter (`failed` shows only the failures).
- **Events:** **Send an event** with an event type, a JSON payload and an optional idempotency key. The event's page shows each delivery's status, attempt count and, while a retry waits, "next retry at …", and under **Attempts** each attempt with its number, start time, outcome, HTTP status, latency and error. It refreshes itself every 2 seconds while a delivery is being sent or a retry is due within 10 seconds, every 30 seconds while a retry waits further off, and stops once every delivery is finished.
- **Settings:** a Health section (database, migrations, and deliveries pending, in progress and waiting for a retry) and every `HOOKD_*` setting with its source (`default`, `environment` or `UI`). The eight editable ones, `HOOKD_RETRY_SCHEDULE` included, have **Save** and **Reset**. `HOOKD_RETENTION` is read-only, because shortening it by mistake deletes data.

### API

Send `Authorization: Bearer hk_...` on every `/v1` route. An object that belongs to another account answers `404`.

| Method & path | What it does |
|---|---|
| `POST /v1/endpoints` | Create an endpoint (`url`, `description`, `event_types`). The answer includes its `secret`. |
| `GET /v1/endpoints?limit=&after=` | List endpoints, oldest first. `limit` is 1 to 100, default 50. To get the next page, pass the last `id` as `after`. |
| `GET /v1/endpoints/{id}` | Get one endpoint, without its secret. |
| `PATCH /v1/endpoints/{id}` | Change `url`, `description`, `event_types` or `enabled`. |
| `DELETE /v1/endpoints/{id}` | Delete an endpoint (`204`). |
| `GET /v1/endpoints/{id}/secret` | Get the signing secret. |
| `POST /v1/events` | Send `event_type` and a JSON object `payload`, plus an optional `Idempotency-Key` header. Answers `202`, or `200` with `Idempotent-Replayed: true` for a key repeated within the window. |
| `GET /v1/events/{id}` | Get the event and each delivery's `status`, `response_status`, `error`, `attempts` (count), `last_attempt_at` and `next_attempt_at` (while a retry waits, otherwise `null`). |
| `GET /v1/events/{id}/attempts?endpoint_id=&limit=&after=` | List the event's attempts to all its endpoints, oldest first. `endpoint_id` keeps one endpoint's. To get the next page, pass the last `id` as `after`. |
| `GET /v1/endpoints/{id}/attempts?outcome=&limit=&before=` | List the attempts made to an endpoint across all events, newest first, also for a deleted endpoint. `outcome` is `succeeded` or `failed`. To get the next page, pass the last `id` as `before`. |
| `GET /v1/attempts/{id}` | Get one attempt. |
| `GET /healthz` | Service and database check. Needs no key. |

Rules:

- An empty `event_types` list subscribes the endpoint to every type.
- Event types are 1 to 128 letters, digits, `.`, `_` or `-`.
- Changes to an endpoint apply to events sent afterwards.
- A delivery that hasn't been sent yet, or is waiting for a retry, when its endpoint is disabled or deleted fails with `endpoint_disabled` within moments. Enabling the endpoint again doesn't bring it back.
- An attempt succeeds only on a 2xx answer within the timeout. Anything else is a failure and is retried, 4xx included; a 3xx answer counts as a failure, because redirects aren't followed. The first success makes the delivery `succeeded`; if the last attempt fails, it is `failed`. While a retry waits, the delivery is `pending` with a `next_attempt_at`.
- An attempt has `id` (`att_...`), `event_id`, `endpoint_id`, `number` (1 for the first, 2 for the first retry), `started_at`, `outcome` (`succeeded` or `failed`), `response_status` (`null` if there was none), `latency_ms` and, on failure, `error`. The attempt lists are paged as `{"data": [...], "has_more": bool}`, `limit` 1 to 100, default 50.
- Events are kept for 30 days (`HOOKD_RETENTION`), then deleted with their deliveries and attempts; after that the event and its attempt routes answer `404`.

Errors look like `{"error":{"code":"...","message":"..."}}`. The codes are `invalid_json`, `invalid_request`, `invalid_url`, `invalid_event_type`, `invalid_payload`, `invalid_idempotency_key`, `unauthorized`, `not_found`, `method_not_allowed`, `payload_too_large`, `internal_error` and `unavailable`.

### Receiving deliveries

Each delivery is a `POST` of the payload bytes, exactly as they were sent. It carries these headers:

- `Content-Type: application/json`
- `webhook-id` (the event ID)
- `webhook-timestamp`
- `webhook-signature` (`v1,<base64 HMAC-SHA256>`)

Verify each delivery against the raw body using any Standard Webhooks library and the endpoint's secret. Delivery is at least once: a failed delivery is retried, and a delivery cut off by a crash is sent again, both with the same `webhook-id` and body, so drop repeats by that header. Each retry has a new `webhook-timestamp` and signature, so it passes the libraries' five-minute timestamp check however late it comes. Answering 2xx stops the retries. `docs/api.md` has complete receivers in Node.js (`npm install standardwebhooks`) and Go.
