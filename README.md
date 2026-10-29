# Hookd

Hookd sends webhooks for you. You register your customers' endpoints over an HTTP API, choose which event types each endpoint receives, and send events. Hookd stores each event, delivers it in the background to every subscribed endpoint, and signs it under the [Standard Webhooks](https://www.standardwebhooks.com/) spec, so receivers can check it with an existing library.

Hookd is one Go binary. Its only outside component is the database: a SQLite file locally (nothing to install), PostgreSQL in production. The same binary serves a browser admin UI at `/admin`, where you can do everything below without a shell: create accounts and API keys, register endpoints, send events, check deliveries, receive test webhooks and change settings.

A failed delivery is retried on a backoff schedule for about a day (8 attempts by default), and every attempt is recorded with its time, HTTP status, latency and error, for the API and the admin UI to show. Events, with their deliveries and attempts, are kept for 30 days and then deleted.

## Guides

- [Set up and run from scratch](#set-up-and-run-from-scratch) (below)
- [Configuration](docs/configuration.md): every `HOOKD_*` setting, and which ones the admin UI can change
- [Deploy to production](docs/deployment.md): PostgreSQL, migrations, several instances behind a load balancer, keeping the admin UI private
- [Test locally](docs/testing.md): `go test ./...`, the PostgreSQL suite, and testing by hand
- [API reference](docs/api.md): every route with `curl` examples, including the attempt history, and how receivers verify signatures

## Set up and run from scratch

This walkthrough installs Go, starts Hookd and uses the admin UI in a browser to create an account, register a test endpoint, send it a signed event, check the delivery and the request it received, watch a failed delivery being retried, and change a setting. It takes about ten minutes and needs one terminal and a browser. The same steps [with `curl` instead of the UI](#the-same-with-the-api) follow.

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
level=INFO msg="applied migration" version=0003_retries
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

The event's page opens with "Event sent". Under **Deliveries**, the delivery to the test endpoint shows `pending` for a moment and then `succeeded`, with 1 attempt and HTTP status `200`. Under **Attempts**, the attempt is listed with its number, start time, outcome, HTTP status, latency and error (none here). The page refreshes by itself every 2 seconds while a delivery is unfinished, and **Refresh** reloads it by hand.

Click the endpoint's URL in the delivery row. Under **Received requests**, the request is listed with its `webhook-id` (the event's `msg_...` ID), the event type `order.paid`, **signature verified**, and the status it answered, 200. **Headers** and **Body** expand to show what Hookd sent. Further down, **Recent attempts** lists every attempt made to this endpoint, newest first, with a link to each attempt's event.

An event of a type the endpoint is not subscribed to, such as `user.created`, creates no delivery.

### 8. Watch a retry

A failed delivery is retried. With the default schedule the first retry comes 5 seconds after the failure and the next one 5 minutes later, so for this walkthrough set a short schedule first:

1. Click **Settings** at the top of any page. In the **Settings** section, set `HOOKD_RETRY_SCHEDULE` to `5s,10s,20s` and click **Save**. Its source becomes `UI`, and it applies at once, without a restart. This means: retry 5 seconds after the first failure, 10 seconds after the second, 20 seconds after the third, then give up, so 4 attempts in all. (Each wait varies at random by up to 10%.)
2. Open the test endpoint's page again (account, **Endpoints**, then the endpoint). Set **Answer status** to `500` and click **Save**.
3. Send another `order.paid` event, as in step 6.
4. On the event's page, the first attempt fails at once with HTTP status `500` and the error `non-2xx response: 500 Internal Server Error`. The delivery stays `pending`, with "next retry at …" and the time. About 5 seconds later a second attempt fails the same way, and the next retry is about 10 seconds off. (While a retry is more than 10 seconds away the page refreshes only every 30 seconds; click **Refresh** to see it sooner.)
5. Before that retry, go to the test endpoint's page in another tab, set **Answer status** back to `200` and click **Save**.
6. Back on the event's page, the third attempt succeeds: the delivery shows `succeeded` after 3 attempts, and no further attempt is made. The page stops refreshing once every delivery is finished.

On the test endpoint's page, **Received requests** shows three requests with the same `webhook-id`, each **signature verified**: a retry sends the same event, signed again with a new timestamp. The 500 answers were the receiver's choice, not a signature problem. **Recent attempts** shows the same three attempts, newest first; choose `failed` under **Outcome** and click **Filter** to see only the two failed ones.

If the endpoint keeps answering 500, the delivery ends `failed` after the fourth attempt. If you disable or delete the endpoint while a retry is waiting, the delivery ends `failed` with the error `endpoint_disabled` at once, and nothing more is sent.

### 9. Change a setting

The **Health** section of **Settings** shows that the database is reachable, which database file is in use, that migrations are up to date, and how many deliveries are pending, in progress and waiting for a retry.

The **Settings** section lists every `HOOKD_*` setting with its value and where it comes from: `default`, `environment` or `UI`. You already changed `HOOKD_RETRY_SCHEDULE` in step 8. Change `HOOKD_DELIVERY_TIMEOUT` to `20s` and click **Save**. Its source becomes `UI`, and the next delivery uses it, without a restart. Values are checked as at start-up: `90s` is refused, because the timeout must be shorter than `HOOKD_DELIVERY_LEASE` (`60s`), and nothing is saved. **Reset** goes back to the environment or default value; reset `HOOKD_RETRY_SCHEDULE` too when you are done, to get the default schedule back. The other settings, which Hookd needs at start-up (such as the database, the listen address and `HOOKD_RETENTION`), are marked "set with environment variables" and cannot be changed here. [Configuration](docs/configuration.md) has the details.

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
{"id":"msg_01M636H35KBNCA9WXZ8KEEVG0E","event_type":"order.paid","payload":{"type":"order.paid","order_id":"1001","amount":4200},"idempotency_key":"order-1001-paid","created_at":"2026-10-29T06:46:01.13945Z","deliveries":[{"endpoint_id":"ep_01M636GV9ZGBJMW8ECDR20804R","status":"pending","response_status":null,"attempts":0,"last_attempt_at":null,"next_attempt_at":null}]}
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

The delivery now shows as `succeeded` after one attempt, with the receiver's status:

```json
{"id":"msg_01M636H35KBNCA9WXZ8KEEVG0E", ..., "deliveries":[{"endpoint_id":"ep_01M636GV9ZGBJMW8ECDR20804R","status":"succeeded","response_status":200,"attempts":1,"last_attempt_at":"2026-10-29T06:46:01.143112Z","next_attempt_at":null}]}
```

The event, the endpoint and the account also show up in the admin UI, since the UI and the API work on the same data.

### 6. Watch a retry (terminals 1, 2 and 3)

1. Stop Hookd in terminal 1 with Ctrl-C and start it again with a short retry schedule, so the retries come within seconds (retry 5 s after the first failure, 10 s after the second, 20 s after the third, then give up):

   ```bash
   HOOKD_RETRY_SCHEDULE=5s,10s,20s go run ./cmd/hookd serve
   ```

   (A value saved in the admin UI's Settings page overrides the environment; if you saved one in the UI walkthrough, **Reset** it there first, or set the schedule there instead.)

2. Stop the test receiver in terminal 3 and start it again answering 500:

   ```bash
   go run ./cmd/testreceiver --secret whsec_... --status 500
   ```

3. Send an event as in step 4, with no idempotency key (or a new one), and note its `id`. Within about 7 seconds, fetch it as in step 5: the delivery is still `pending`, after 2 failed attempts, with the time of the next retry:

   ```json
   {"endpoint_id":"ep_01M636GV9ZGBJMW8ECDR20804R","status":"pending","response_status":500,"error":"non-2xx response: 500 Internal Server Error","attempts":2,"last_attempt_at":"2026-10-29T06:47:05.774941Z","next_attempt_at":"2026-10-29T06:47:15.5305Z"}
   ```

   Terminal 1 logs each attempt with its number and, after a failure, when the next one is due:

   ```
   level=INFO msg="delivery attempt" delivery_id=dlv_... event_id=msg_... endpoint_id=ep_... attempt=2 outcome=failed response_status=500 error="non-2xx response: 500 Internal Server Error" latency_ms=1 next_attempt_at=2026-10-29T06:47:15.530Z
   ```

4. Before `next_attempt_at`, stop the test receiver and start it answering 200 (`go run ./cmd/testreceiver --secret whsec_...`). The third attempt succeeds: fetching the event shows `"status":"succeeded","response_status":200,"attempts":3,"next_attempt_at":null`. The test receiver logged `verified` three times with the same `id`: each retry is the same event, signed again.

5. List the event's attempts, oldest first:

   ```bash
   curl -s http://localhost:8080/v1/events/msg_.../attempts \
     -H "Authorization: Bearer $HOOKD_API_KEY"
   ```

   ```json
   {"data":[{"id":"att_01M64FB9ME99Q5TX92DAG5VJKW","event_id":"msg_...","endpoint_id":"ep_...","number":1,"started_at":"2026-10-29T06:47:00.764954Z","outcome":"failed","response_status":500,"latency_ms":1,"error":"non-2xx response: 500 Internal Server Error"},{"id":"att_...","number":2, ...},{"id":"att_...","number":3,"outcome":"succeeded","response_status":200,"latency_ms":0, ...}],"has_more":false}
   ```

   The endpoint's attempts, across all its events, newest first, are at `/v1/endpoints/ep_.../attempts`; add `?outcome=failed` to see only the failed ones. [API reference](docs/api.md#attempts) has the details.

### Next steps

- Keep the test receiver answering 500 and send another event: with the schedule above, it ends `failed` after 4 attempts, and no fifth is made. Its `error` is that of the last attempt.
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

Settings come from `HOOKD_*` environment variables, and eight of them can also be changed in the admin UI while Hookd runs; see [Configuration](docs/configuration.md). `cmd/testreceiver` is a development tool and is not deployed.

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
internal/delivery/    worker pool, HTTP sender, retry schedule
internal/retention/   background deletion of events older than HOOKD_RETENTION
internal/signing/     Standard Webhooks signer
internal/ids/         prefixed ULIDs, secret and API key generation
docs/changes/         requirements, design and plan of each change
```
