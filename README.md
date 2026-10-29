# Hookd

Hookd sends webhooks for you. You register your customers' endpoints over an HTTP API, choose which event types each endpoint receives, and send events. Hookd stores each event, delivers it in the background to every subscribed endpoint, and signs it under the [Standard Webhooks](https://www.standardwebhooks.com/) spec, so receivers can check it with an existing library.

Hookd is one Go binary. Its only outside component is the database: a SQLite file locally (nothing to install), PostgreSQL in production.

In this version each delivery is attempted once and failures are recorded, not retried.

## Guides

- [Set up and run from scratch](#set-up-and-run-from-scratch) (below)
- [Configuration](docs/configuration.md): every `HOOKD_*` setting
- [Deploy to production](docs/deployment.md): PostgreSQL, migrations, several instances behind a load balancer
- [Test locally](docs/testing.md): `go test ./...` and the PostgreSQL suite
- [API reference](docs/api.md): every route with `curl` examples, and how receivers verify signatures

## Set up and run from scratch

This walkthrough starts Hookd, creates an account, registers an endpoint that points at a local test receiver, and sends it a signed event. It takes about five minutes. You need three terminals, all in the repository folder.

### 1. Install Go and get the code

Install Go 1.26 or later from <https://go.dev/dl/> and check it:

```bash
go version
```

You also need `git` and `curl`. Then clone the repository:

```bash
git clone <repository-url> hookd
cd hookd
```

### 2. Start Hookd (terminal 1)

```bash
go run ./cmd/hookd serve
```

The first run downloads the Go modules, which can take a minute. Hookd then creates the SQLite database `./hookd.db`, applies its migrations and listens on port 8080:

```
level=INFO msg="applied migration" version=0001_init
level=INFO msg=listening addr=[::]:8080
```

Leave it running. In terminal 2, check that it is up:

```bash
curl -s http://localhost:8080/healthz
```

```json
{"status":"ok"}
```

### 3. Create an account (terminal 2)

```bash
go run ./cmd/hookd account create --name acme
```

```
account_id: acct_01M636GV96GQFMA7V1G5N17DPQ
api_key:    hk_1r41QRqtUtyhEHFF6nnQZin__SkqHv-QP20dGALqfvE
Keep the API key safe: it is shown only once.
```

Put your API key (not the one above) in a variable so the next commands can use it:

```bash
export HOOKD_API_KEY=hk_...
```

### 4. Register an endpoint (terminal 2)

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

### 5. Start the test receiver (terminal 3)

`cmd/testreceiver` is a small webhook receiver for development. It checks each request with the official Standard Webhooks Go library and logs whether the signature verified. Start it with your endpoint's secret:

```bash
go run ./cmd/testreceiver --secret whsec_...
```

```
level=INFO msg=listening addr=127.0.0.1:9000 status=200
```

It listens on `127.0.0.1:9000` and answers every request with 200. Use `--addr` to change the address and `--status 500` to make it answer with a failure instead.

### 6. Send an event (terminal 2)

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

### 7. Check the delivery (terminal 2)

Fetch the event by the `id` from step 6:

```bash
curl -s http://localhost:8080/v1/events/msg_... \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

The delivery now shows as `succeeded`, with the receiver's status:

```json
{"id":"msg_01M636H35KBNCA9WXZ8KEEVG0E", ..., "deliveries":[{"endpoint_id":"ep_01M636GV9ZGBJMW8ECDR20804R","status":"succeeded","response_status":200}]}
```

### Next steps

- Stop the test receiver, restart it with `--status 500`, send another event, and fetch it: its delivery shows `"status":"failed","response_status":500,"error":"non-2xx response: 500 Internal Server Error"`. (The test receiver still logs `verified`: the signature was fine, it just answered 500.)
- Send an event of another type, such as `user.created`: it creates no delivery, since the endpoint is subscribed to `order.paid` only.
- Stop Hookd with Ctrl-C. To start again from an empty database, delete `hookd.db` (Hookd recreates it at the next start).

## Commands

```
hookd serve                        Run the API server and delivery workers
hookd migrate                      Apply database migrations
hookd account create --name NAME   Create an account and print its API key
hookd apikey create --account ID   Create another API key for an account and print it
```

All settings come from `HOOKD_*` environment variables; see [Configuration](docs/configuration.md). `cmd/testreceiver` is a development tool and is not deployed.

## Project layout

```
cmd/hookd/            main: serve, migrate, account create, apikey create
cmd/testreceiver/     verifying test receiver (dev/test only)
internal/config/      env parsing and defaults
internal/store/       Store interface, SQL implementation, dialects, migrations
internal/api/         handlers, auth middleware, errors
internal/delivery/    worker pool, HTTP sender
internal/signing/     Standard Webhooks signer
internal/ids/         prefixed ULIDs, secret and API key generation
docs/changes/         requirements, design and plan of each change
```
