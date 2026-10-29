# Hookd

Hookd sends webhooks for you. You register your customers' endpoints over an HTTP API, choose which event types each endpoint receives, and send events. Hookd stores each event, delivers it in the background to every subscribed endpoint, and signs it under the [Standard Webhooks](https://www.standardwebhooks.com/) spec, so receivers can check it with an existing library.

Hookd is one Go binary. Its only outside component is the database: a SQLite file locally (nothing to install), PostgreSQL in production. The same binary serves a browser admin UI at `/admin`, where you can do everything below without a shell: create accounts and API keys, register endpoints, send events, check deliveries, receive test webhooks and change settings.

In this version each delivery is attempted once and failures are recorded, not retried.

## Guides

- [Set up and run from scratch](#set-up-and-run-from-scratch) (below)
- [Configuration](docs/configuration.md): every `HOOKD_*` setting, and which ones the admin UI can change
- [Deploy to production](docs/deployment.md): PostgreSQL, migrations, several instances behind a load balancer, keeping the admin UI private
- [Test locally](docs/testing.md): `go test ./...`, the PostgreSQL suite, and testing by hand
- [API reference](docs/api.md): every route with `curl` examples, and how receivers verify signatures

## Set up and run from scratch

This walkthrough installs Go, starts Hookd and uses the admin UI in a browser to create an account, register a test endpoint, send it a signed event, check the delivery and the request it received, and change a setting. It takes about five minutes and needs one terminal and a browser. The same steps [with `curl` instead of the UI](#the-same-with-the-api) follow.

### 1. Install Go and get the code

Install Go 1.26 or later from <https://go.dev/dl/> and check it:

```bash
go version
```

You also need `git`. Then clone the repository:

```bash
git clone <repository-url> hookd
cd hookd
```

### 2. Start Hookd

```bash
go run ./cmd/hookd serve
```

The first run downloads the Go modules, which can take a minute. Hookd then creates the SQLite database `./hookd.db`, applies its migrations and listens on port 8080:

```
level=INFO msg="applied migration" version=0001_init
level=INFO msg="applied migration" version=0002_admin
level=INFO msg=listening addr=[::]:8080
```

Leave it running. Stop it with Ctrl-C when you are done. To start again from an empty database, delete `hookd.db` (Hookd recreates it at the next start).

### 3. Open the admin UI

Open <http://localhost:8080/> in a browser. It takes you to the admin UI at `/admin/`, which starts on the **Accounts** page. Every page shows a **No sign-in** notice: anyone who can open the UI can use every page, so keep it reachable only by trusted people (see [Deploy to production](docs/deployment.md#keep-the-admin-ui-private)).

### 4. Create an account

Under **New account**, enter the name `acme` and click **Create account**. The next page shows the account's first API key (`hk_...`) with a **Copy** button. Copy it now: it is shown only this once, and afterwards the UI shows only its first characters. You need it only to call the API, as in [the same with the API](#the-same-with-the-api).

Click **Go to the account**. The account page lists its API keys, has **Create API key** to make more, and **Revoke** next to each key. It also links to the account's **Endpoints** and **Events**.

### 5. Create a test endpoint

A test endpoint is an ordinary endpoint whose URL points at a receiver built into Hookd. The receiver checks each request's signature and records it, so you need no separate receiver.

1. On the account page, click **Endpoints**, then **New test endpoint**.
2. In **Event types**, enter `order.paid`. Leave **Answer status** at `200`.
3. Click **Create test endpoint**.

The endpoint's page opens. Its URL looks like `http://127.0.0.1:8080/admin/test-receiver/trcv_...`. **Show the signing secret** shows the `whsec_...` secret that Hookd signs this endpoint's deliveries with. **Received requests** is empty for now.

### 6. Send an event

1. Go back to the account (the account name at the top of the page links to it) and click **Events**, then **Send an event**.
2. Enter the event type `order.paid` and this payload:

   ```json
   {"order_id": "1001", "amount": 4200}
   ```

3. Optionally enter an idempotency key, such as `order-1001-paid`. Sending again with the same key returns the first event instead of a new one.
4. Click **Send event**.

### 7. Check the delivery and the received request

The event's page opens with "Event sent". Under **Deliveries**, the delivery to the test endpoint shows `pending` for a moment and then `succeeded`, with HTTP status `200`. The page refreshes by itself every 2 seconds while a delivery is unfinished, and **Refresh** reloads it by hand.

Click the endpoint's URL in the delivery row. Under **Received requests**, the request is listed with its `webhook-id` (the event's `msg_...` ID), the event type `order.paid`, **signature verified**, and the status it answered, 200. **Headers** and **Body** expand to show what Hookd sent.

To see a failure, set **Answer status** on the same page to `500`, click **Save**, and send another event: its delivery shows `failed` with HTTP status `500` and the error `non-2xx response: 500 Internal Server Error`. The received request still says **signature verified**: the signature was fine, the receiver just answered 500. An event of a type the endpoint is not subscribed to, such as `user.created`, creates no delivery.

### 8. Change a setting

Click **Settings** at the top of any page. The **Health** section shows that the database is reachable, which database file is in use, that migrations are up to date, and how many deliveries are pending or in progress.

The **Settings** section lists every `HOOKD_*` setting with its value and where it comes from: `default`, `environment` or `UI`. Change `HOOKD_DELIVERY_TIMEOUT` to `20s` and click **Save**. Its source becomes `UI`, and the next delivery uses it, without a restart. Values are checked as at start-up: `90s` is refused, because the timeout must be shorter than `HOOKD_DELIVERY_LEASE` (`60s`), and nothing is saved. **Reset** goes back to the environment or default value. The other settings, which Hookd needs at start-up (such as the database and the listen address), are marked "set with environment variables" and cannot be changed here. [Configuration](docs/configuration.md) has the details.

## The same with the API

Everything above can also be done with the command line and the JSON API, with `cmd/testreceiver` as the receiver instead of the built-in one. This uses three terminals, all in the repository folder, and `curl`. Start Hookd as in step 2 (terminal 1), then check that it is up in terminal 2:

```bash
curl -s http://localhost:8080/healthz
```

```json
{"status":"ok"}
```

### 1. Create an account (terminal 2)

```bash
go run ./cmd/hookd account create --name acme
```

```
account_id: acct_01M636GV96GQFMA7V1G5N17DPQ
api_key:    hk_1r41QRqtUtyhEHFF6nnQZin__SkqHv-QP20dGALqfvE
Keep the API key safe: it is shown only once.
```

Put your API key (not the one above) in a variable so the next commands can use it. A key copied from the admin UI works the same way:

```bash
export HOOKD_API_KEY=hk_...
```

### 2. Register an endpoint (terminal 2)

Register an endpoint at `http://127.0.0.1:9000`, where the test receiver will listen, subscribed to `order.paid`:

```bash
curl -s -X POST http://localhost:8080/v1/endpoints \
  -H "Authorization: Bearer $HOOKD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"url": "http://127.0.0.1:9000", "description": "local test receiver", "event_types": ["order.paid"]}'
```

```json
{"id":"ep_01M636GV9ZGBJMW8ECDR20804R","url":"http://127.0.0.1:9000","description":"local test receiver","event_types":["order.paid"],"enabled":true,"created_at":"2026-10-29T06:45:53.087516Z","updated_at":"2026-10-29T06:45:53.087516Z","secret":"whsec_hPvrVhFTx70CvazjKb2goHGk8fuFZRXq"}
```

Note the `secret` (`whsec_...`). Hookd signs every delivery to this endpoint with it. You can fetch it again later with `GET /v1/endpoints/{id}/secret`.

### 3. Start the test receiver (terminal 3)

`cmd/testreceiver` is a small webhook receiver for development. It checks each request with the official Standard Webhooks Go library and logs whether the signature verified. Start it with your endpoint's secret:

```bash
go run ./cmd/testreceiver --secret whsec_...
```

```
level=INFO msg=listening addr=127.0.0.1:9000 status=200
```

It listens on `127.0.0.1:9000` and answers every request with 200. Use `--addr` to change the address and `--status 500` to make it answer with a failure instead.

### 4. Send an event (terminal 2)

```bash
curl -s -X POST http://localhost:8080/v1/events \
  -H "Authorization: Bearer $HOOKD_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-1001-paid" \
  -d '{"event_type": "order.paid", "payload": {"type": "order.paid", "order_id": "1001", "amount": 4200}}'
```

Hookd answers `202 Accepted` at once, before delivering:

```json
{"id":"msg_01M636H35KBNCA9WXZ8KEEVG0E","event_type":"order.paid","payload":{"type":"order.paid","order_id":"1001","amount":4200},"idempotency_key":"order-1001-paid","created_at":"2026-10-29T06:46:01.13945Z","deliveries":[{"endpoint_id":"ep_01M636GV9ZGBJMW8ECDR20804R","status":"pending","response_status":null}]}
```

Within a moment, the test receiver in terminal 3 logs that the signature verified:

```
level=INFO msg=verified id=msg_01M636H35KBNCA9WXZ8KEEVG0E type=order.paid body="{\"type\": \"order.paid\", \"order_id\": \"1001\", \"amount\": 4200}" verified=true
```

### 5. Check the delivery (terminal 2)

Fetch the event by the `id` from step 4:

```bash
curl -s http://localhost:8080/v1/events/msg_... \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

The delivery now shows as `succeeded`, with the receiver's status:

```json
{"id":"msg_01M636H35KBNCA9WXZ8KEEVG0E", ..., "deliveries":[{"endpoint_id":"ep_01M636GV9ZGBJMW8ECDR20804R","status":"succeeded","response_status":200}]}
```

The event, the endpoint and the account also show up in the admin UI, since the UI and the API work on the same data.

### Next steps

- Stop the test receiver, restart it with `--status 500`, send another event, and fetch it: its delivery shows `"status":"failed","response_status":500,"error":"non-2xx response: 500 Internal Server Error"`. (The test receiver still logs `verified`: the signature was fine, it just answered 500.)
- Send an event of another type, such as `user.created`: it creates no delivery, since the endpoint is subscribed to `order.paid` only.
- Settings are changed with `HOOKD_*` environment variables, for example `HOOKD_DELIVERY_TIMEOUT=20s go run ./cmd/hookd serve`; see [Configuration](docs/configuration.md).

## Commands

```
hookd serve                        Run the API server, the admin UI and the delivery workers
hookd migrate                      Apply database migrations
hookd account create --name NAME   Create an account and print its API key
hookd apikey create --account ID   Create another API key for an account and print it
hookd apikey list --account ID     List an account's API keys
hookd apikey revoke --id KEY_ID    Revoke an API key so it no longer works
```

Settings come from `HOOKD_*` environment variables, and seven of them can also be changed in the admin UI while Hookd runs; see [Configuration](docs/configuration.md). `cmd/testreceiver` is a development tool and is not deployed.

## Project layout

```
cmd/hookd/            main: serve, migrate, account create, apikey create/list/revoke
cmd/testreceiver/     verifying test receiver (dev/test only)
internal/config/      env parsing and defaults
internal/settings/    settings that change while Hookd runs: environment plus UI overrides
internal/store/       Store interface, SQL implementation, dialects, migrations
internal/api/         handlers, auth middleware, errors
internal/admin/       admin UI: pages, forms, built-in test receiver, embedded templates and static files
internal/validate/    input checks shared by the API and the admin UI
internal/delivery/    worker pool, HTTP sender
internal/signing/     Standard Webhooks signer
internal/ids/         prefixed ULIDs, secret and API key generation
docs/changes/         requirements, design and plan of each change
```
