# API reference

The API is JSON over HTTP. Every route under `/v1` needs an API key, sent as a bearer token:

```
Authorization: Bearer hk_...
```

The operator creates keys with `hookd account create` and `hookd apikey create`, or in the admin UI, and revokes them with `hookd apikey revoke` or in the admin UI (see [Deploy to production](deployment.md#9-create-accounts)). A key created in the admin UI works exactly like one from the command line. Each key belongs to one account, and an account sees only its own endpoints and events. Anything that belongs to another account answers 404, as if it did not exist.

The examples assume Hookd at `http://localhost:8080` and the key in `$HOOKD_API_KEY`, as in the [local setup](../README.md#set-up-and-run-from-scratch).

## Routes

| Method & path | What it does | Success |
|---|---|---|
| `POST /v1/endpoints` | Create an endpoint. The answer includes its signing secret. | 201 |
| `GET /v1/endpoints` | List endpoints, oldest first, a page at a time. | 200 |
| `GET /v1/endpoints/{id}` | Get one endpoint, without its secret. | 200 |
| `PATCH /v1/endpoints/{id}` | Change an endpoint's `url`, `description`, `event_types` or `enabled`. | 200 |
| `DELETE /v1/endpoints/{id}` | Delete an endpoint. | 204 |
| `GET /v1/endpoints/{id}/secret` | Get an endpoint's signing secret. | 200 |
| `GET /v1/endpoints/{id}/attempts` | List the attempts made to an endpoint, newest first, a page at a time. | 200 |
| `POST /v1/events` | Send an event. Delivery happens in the background. | 202, or 200 for a repeated idempotency key |
| `GET /v1/events/{id}` | Get an event and the delivery status for each endpoint. | 200 |
| `GET /v1/events/{id}/attempts` | List an event's attempts, oldest first, a page at a time. | 200 |
| `GET /v1/attempts/{id}` | Get one attempt. | 200 |
| `GET /healthz` | Check the service and its database. No API key needed. | 200, or 503 |

## Errors

Every error has the same shape: a machine-readable `code` and a message for people.

```json
{"error":{"code":"invalid_url","message":"url must be an absolute http or https URL with a host"}}
```

| Status | Code | When |
|---|---|---|
| 400 | `invalid_json` | The body is not valid JSON, or holds more than one JSON value. |
| 400 | `invalid_request` | A field is unknown or has the wrong type, or a parameter is out of range. |
| 400 | `invalid_url` | The endpoint URL is missing, too long (over 2048 bytes), or not an absolute `http` or `https` URL with a host. |
| 400 | `invalid_event_type` | An event type is not 1 to 128 letters, digits, `.`, `_` or `-`. |
| 400 | `invalid_payload` | The event payload is missing or not a JSON object. |
| 400 | `invalid_idempotency_key` | The `Idempotency-Key` header is over 255 bytes. |
| 401 | `unauthorized` | The API key is missing, malformed or unknown. |
| 404 | `not_found` | No such route, or no such endpoint, event or attempt in this account. Events older than the retention period (30 days by default) have been deleted and answer 404 too. |
| 405 | `method_not_allowed` | The route exists but not with this method. |
| 413 | `payload_too_large` | The event payload is over `HOOKD_MAX_PAYLOAD_BYTES` (256 KB by default). |
| 500 | `internal_error` | Something went wrong in Hookd. Details are in its log. |
| 503 | `unavailable` | `/healthz` only: the database cannot be reached. |

## Endpoints

An endpoint is a URL that receives events. It has:

| Field | Type | Notes |
|---|---|---|
| `id` | string | `ep_...`, set by Hookd. |
| `url` | string | Required. An absolute `https://` or `http://` URL. |
| `description` | string | Optional, up to 1024 bytes. |
| `event_types` | array of strings | The event types it receives, up to 100. Empty or left out means every type. |
| `enabled` | boolean | `true` when created. A disabled endpoint receives no new deliveries. |
| `created_at`, `updated_at` | string | RFC 3339 times in UTC. |
| `secret` | string | `whsec_...`. Only in the answer to `POST /v1/endpoints`, and from `GET /v1/endpoints/{id}/secret`. |

Endpoints created in the admin UI are ordinary endpoints of the account. **Test endpoints** are too: a test endpoint is an endpoint whose `url` points at Hookd's built-in test receiver, such as `http://127.0.0.1:8080/admin/test-receiver/trcv_01M63TNJFB7E4H67PBC23F27CY`. The API lists, gets, updates, disables and deletes it like any other endpoint, events are delivered and signed to it the same way, and nothing in its JSON marks it as a test endpoint. Only the admin UI marks it, and shows the requests its receiver got. If its `url` is changed through the API, deliveries go to the new URL and the built-in receiver no longer sees them. The built-in receiver is not part of the `/v1` API and needs no API key; it answers 404 when the admin UI is off.

### Create an endpoint

```bash
curl -s -X POST http://localhost:8080/v1/endpoints \
  -H "Authorization: Bearer $HOOKD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/webhooks", "description": "Acme production", "event_types": ["order.paid", "order.refunded"]}'
```

```json
{"id":"ep_01M638W1FQ3V5Z8C7D2W0N0X1A","url":"https://example.com/webhooks","description":"Acme production","event_types":["order.paid","order.refunded"],"enabled":true,"created_at":"2026-10-29T07:00:00.000000Z","updated_at":"2026-10-29T07:00:00.000000Z","secret":"whsec_hPvrVhFTx70CvazjKb2goHGk8fuFZRXq"}
```

Give the `secret` to whoever runs the receiver, so they can check signatures.

### List endpoints

```bash
curl -s "http://localhost:8080/v1/endpoints?limit=10" \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

```json
{"data":[{"id":"ep_01M638W1FQ3V5Z8C7D2W0N0X1A", ...}],"has_more":false}
```

`limit` is 1 to 100 (default 50). When `has_more` is `true`, get the next page by passing the last `id` of this page as `after`:

```bash
curl -s "http://localhost:8080/v1/endpoints?limit=10&after=ep_01M638W1FQ3V5Z8C7D2W0N0X1A" \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

### Get an endpoint

```bash
curl -s http://localhost:8080/v1/endpoints/ep_01M638W1FQ3V5Z8C7D2W0N0X1A \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

### Update an endpoint

Send only the fields to change. This one disables the endpoint and subscribes it to every event type:

```bash
curl -s -X PATCH http://localhost:8080/v1/endpoints/ep_01M638W1FQ3V5Z8C7D2W0N0X1A \
  -H "Authorization: Bearer $HOOKD_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"enabled": false, "event_types": []}'
```

The answer is the updated endpoint. Changes apply to events sent afterwards. A delivery that has not been sent yet, or is waiting for a retry, when its endpoint is disabled or deleted is not sent again: it fails with the error `endpoint_disabled` within moments. Enabling the endpoint again does not bring such deliveries back.

### Get the signing secret

```bash
curl -s http://localhost:8080/v1/endpoints/ep_01M638W1FQ3V5Z8C7D2W0N0X1A/secret \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

```json
{"secret":"whsec_hPvrVhFTx70CvazjKb2goHGk8fuFZRXq"}
```

### Delete an endpoint

```bash
curl -s -X DELETE http://localhost:8080/v1/endpoints/ep_01M638W1FQ3V5Z8C7D2W0N0X1A \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

The answer is `204 No Content`. The endpoint receives nothing more and is no longer listed, but events sent to it before still show its delivery status, and its attempts can still be listed with `GET /v1/endpoints/{id}/attempts` until they expire with their events.

## Events

### Send an event

```bash
curl -s -X POST http://localhost:8080/v1/events \
  -H "Authorization: Bearer $HOOKD_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: order-1001-paid" \
  -d '{"event_type": "order.paid", "payload": {"type": "order.paid", "order_id": "1001", "amount": 4200}}'
```

```json
{"id":"msg_01M636H35KBNCA9WXZ8KEEVG0E","event_type":"order.paid","payload":{"type":"order.paid","order_id":"1001","amount":4200},"idempotency_key":"order-1001-paid","created_at":"2026-10-29T06:46:01.13945Z","deliveries":[{"endpoint_id":"ep_01M636GV9ZGBJMW8ECDR20804R","status":"pending","response_status":null,"attempts":0,"last_attempt_at":null,"next_attempt_at":null}]}
```

- `event_type` is required: 1 to 128 letters, digits, `.`, `_` or `-`. Types need not be declared first.
- `payload` is required and must be a JSON object, up to 256 KB. It is sent to receivers byte for byte as given. Hookd does not add the event type to it, so put it in the payload if receivers need it.
- `Idempotency-Key` is optional. Sending the same key again within 24 hours answers `200` with the original event (and the header `Idempotent-Replayed: true`) instead of creating and delivering a new one. Keys are per account.

Hookd stores the event before it answers `202`. It creates one delivery for each endpoint of the account that is enabled and subscribed to the event type at that moment; with no matching endpoint, `deliveries` is empty. Delivery starts within moments, in the background.

### Get an event

```bash
curl -s http://localhost:8080/v1/events/msg_01M636H35KBNCA9WXZ8KEEVG0E \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

```json
{"id":"msg_01M636H35KBNCA9WXZ8KEEVG0E","event_type":"order.paid","payload":{...},"idempotency_key":"order-1001-paid","created_at":"2026-10-29T06:46:01.13945Z","deliveries":[{"endpoint_id":"ep_01M636GV9ZGBJMW8ECDR20804R","status":"succeeded","response_status":200,"attempts":1,"last_attempt_at":"2026-10-29T06:46:01.143112Z","next_attempt_at":null}]}
```

Each delivery has:

| Field | Notes |
|---|---|
| `endpoint_id` | The endpoint it goes to. |
| `status` | `pending` (not sent yet, being sent, or waiting for a retry), `succeeded` or `failed`. `succeeded` and `failed` are final. |
| `response_status` | The endpoint's HTTP status in the last attempt, or `null` if there was none (not sent yet, timeout, connection error). |
| `error` | Why the last attempt failed, when it did. For example `non-2xx response: 500 Internal Server Error`, `redirect not followed: 302 Found`, a timeout or connection error, or `endpoint_disabled`. Left out when there is none. |
| `attempts` | How many attempts have been made so far. `0` before the first. |
| `last_attempt_at` | When the last attempt started, or `null` before the first. |
| `next_attempt_at` | When the next retry is due, while one is waiting; otherwise `null` (before the first attempt, while an attempt is being sent, and once the delivery is final). |

An attempt succeeds if the endpoint answers any 2xx status within 15 seconds (`HOOKD_DELIVERY_TIMEOUT`); anything else is a failure, including 3xx (redirects are not followed) and 4xx. A failed attempt is retried on the retry schedule: by default 5 seconds, 5 minutes, 30 minutes, 2 hours, 5 hours, 10 hours and 10 hours after the previous attempt, each varied by up to 10%, so 8 attempts over about 27½ hours. The first success makes the delivery `succeeded`; if the last attempt fails, it is `failed`. Nothing more is sent after either. The operator can change the schedule or turn retries off with `HOOKD_RETRY_SCHEDULE` (see [Configuration](configuration.md#retries)).

A delivery waiting for its second attempt, after the first failed with 500:

```json
{"endpoint_id":"ep_01M636GV9ZGBJMW8ECDR20804R","status":"pending","response_status":500,"error":"non-2xx response: 500 Internal Server Error","attempts":1,"last_attempt_at":"2026-10-29T06:46:01.143112Z","next_attempt_at":"2026-10-29T06:46:06.271403Z"}
```

Events are kept for 30 days by default (`HOOKD_RETENTION`), then deleted with their deliveries and attempts. After that `GET /v1/events/{id}` and the event's attempt routes answer 404.

## Attempts

Every attempt to send a delivery is recorded, including one that found the endpoint disabled. An attempt has:

| Field | Type | Notes |
|---|---|---|
| `id` | string | `att_...`, set by Hookd. IDs sort in the order the attempts were made. |
| `event_id` | string | The event that was sent. |
| `endpoint_id` | string | The endpoint it was sent to. |
| `number` | number | 1 for the first attempt of a delivery, 2 for the first retry, and so on. |
| `started_at` | string | When the attempt started, RFC 3339 in UTC. |
| `outcome` | string | `succeeded` or `failed`. |
| `response_status` | number or null | The endpoint's HTTP status, or `null` if there was none (timeout, connection error, disabled endpoint). |
| `latency_ms` | number | How long the attempt took, in milliseconds. |
| `error` | string | Why it failed, as for a delivery's `error`. Left out on success. |

An attempt cut short by a crash of Hookd is not recorded; the delivery is sent again once its lease runs out, and that attempt is recorded with the same number.

The two lists are paged like `GET /v1/endpoints`: they answer `{"data": [...], "has_more": bool}`, and `limit` is 1 to 100 (default 50). When `has_more` is `true`, pass the last `id` of the page as `after` (event list) or `before` (endpoint list) to get the next page.

### List an event's attempts

All the attempts of one event, to all its endpoints, oldest first:

```bash
curl -s http://localhost:8080/v1/events/msg_01M64FB9M72S04FCYTQWQT6QPZ/attempts \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

```json
{"data":[{"id":"att_01M64FB9ME99Q5TX92DAG5VJKW","event_id":"msg_01M64FB9M72S04FCYTQWQT6QPZ","endpoint_id":"ep_01M64FB9455QB7T0KRSB5G6RJZ","number":1,"started_at":"2026-10-29T18:39:22.764954Z","outcome":"failed","response_status":500,"latency_ms":1,"error":"non-2xx response: 500 Internal Server Error"},{"id":"att_01M64FBEH0A0CGAKYTRXRE1ZHJ","event_id":"msg_01M64FB9M72S04FCYTQWQT6QPZ","endpoint_id":"ep_01M64FB9455QB7T0KRSB5G6RJZ","number":2,"started_at":"2026-10-29T18:39:27.774941Z","outcome":"failed","response_status":500,"latency_ms":1,"error":"non-2xx response: 500 Internal Server Error"},{"id":"att_01M64FBR9XBW92N6JTBNEY5S2V","event_id":"msg_01M64FB9M72S04FCYTQWQT6QPZ","endpoint_id":"ep_01M64FB9455QB7T0KRSB5G6RJZ","number":3,"started_at":"2026-10-29T18:39:37.788394Z","outcome":"succeeded","response_status":200,"latency_ms":0}],"has_more":false}
```

Query parameters:

| Parameter | Notes |
|---|---|
| `endpoint_id` | Only the attempts to this endpoint. |
| `limit` | 1 to 100, default 50. |
| `after` | An attempt ID: only the attempts after it, for the next page. |

An event of another account, or one that has expired, answers 404.

### List an endpoint's attempts

The attempts made to one endpoint, across all events, newest first. This also works for an endpoint that has been deleted.

```bash
curl -s "http://localhost:8080/v1/endpoints/ep_01M64FB9455QB7T0KRSB5G6RJZ/attempts?outcome=failed&limit=1" \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

```json
{"data":[{"id":"att_01M64FBEH0A0CGAKYTRXRE1ZHJ","event_id":"msg_01M64FB9M72S04FCYTQWQT6QPZ","endpoint_id":"ep_01M64FB9455QB7T0KRSB5G6RJZ","number":2,"started_at":"2026-10-29T18:39:27.774941Z","outcome":"failed","response_status":500,"latency_ms":1,"error":"non-2xx response: 500 Internal Server Error"}],"has_more":true}
```

The next page:

```bash
curl -s "http://localhost:8080/v1/endpoints/ep_01M64FB9455QB7T0KRSB5G6RJZ/attempts?outcome=failed&limit=1&before=att_01M64FBEH0A0CGAKYTRXRE1ZHJ" \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

Query parameters:

| Parameter | Notes |
|---|---|
| `outcome` | `succeeded` or `failed`: only the attempts with this outcome. Anything else answers 400 `invalid_request`. |
| `limit` | 1 to 100, default 50. |
| `before` | An attempt ID: only the attempts before it, for the next page. |

An endpoint of another account answers 404.

### Get an attempt

```bash
curl -s http://localhost:8080/v1/attempts/att_01M64FB9ME99Q5TX92DAG5VJKW \
  -H "Authorization: Bearer $HOOKD_API_KEY"
```

```json
{"id":"att_01M64FB9ME99Q5TX92DAG5VJKW","event_id":"msg_01M64FB9M72S04FCYTQWQT6QPZ","endpoint_id":"ep_01M64FB9455QB7T0KRSB5G6RJZ","number":1,"started_at":"2026-10-29T18:39:22.764954Z","outcome":"failed","response_status":500,"latency_ms":1,"error":"non-2xx response: 500 Internal Server Error"}
```

An attempt of another account answers 404.

## Health

### Check health

```bash
curl -s http://localhost:8080/healthz
```

```json
{"status":"ok"}
```

## What receivers get

Each delivery is an HTTP `POST` of the event payload with these headers:

| Header | Value |
|---|---|
| `Content-Type` | `application/json` |
| `webhook-id` | The event ID (`msg_...`). The same for every endpoint, every retry and any resend, so receivers can drop duplicates by it. |
| `webhook-timestamp` | The time of this attempt, in Unix seconds. Each retry has a new one, and a new signature. |
| `webhook-signature` | `v1,` followed by the base64 HMAC-SHA256 of `{webhook-id}.{webhook-timestamp}.{body}`, keyed with the base64-decoded part of the secret after `whsec_`. |

This is the [Standard Webhooks](https://www.standardwebhooks.com/) format, so receivers check deliveries with any Standard Webhooks library and the endpoint's secret. The library also rejects timestamps more than five minutes away from now, which stops old deliveries being replayed; since every retry is signed afresh, retries pass this check however late they come. Delivery is at least once: a failed delivery is retried, and a delivery cut off by a crash is sent again, both with the same `webhook-id` and body. A receiver that answers 2xx to an event it has already processed stops the retries.

Verify against the raw request body, exactly as received. Parsing the JSON and serializing it again changes the bytes, and the signature no longer matches.

### Example: verify in Node.js

With the official [`standardwebhooks`](https://www.npmjs.com/package/standardwebhooks) package:

```bash
mkdir receiver && cd receiver
npm init -y
npm install standardwebhooks
```

Save this as `receiver.js`:

```js
const http = require("node:http");
const { Webhook } = require("standardwebhooks");

const wh = new Webhook(process.env.WEBHOOK_SECRET); // whsec_...

http
  .createServer((req, res) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const body = Buffer.concat(chunks).toString("utf8"); // the raw body
      try {
        const payload = wh.verify(body, req.headers);
        console.log("verified", req.headers["webhook-id"], payload);
        res.writeHead(204).end();
      } catch (err) {
        console.log("not verified:", err.message);
        res.writeHead(400).end();
      }
    });
  })
  .listen(9000, "127.0.0.1", () => console.log("listening on 127.0.0.1:9000"));
```

Run it with the endpoint's secret:

```bash
WEBHOOK_SECRET=whsec_... node receiver.js
```

It is a drop-in replacement for `cmd/testreceiver` in the [local setup](../README.md#set-up-and-run-from-scratch): send an event and it prints `verified` with the event ID and payload.

### Example: verify in Go

With the official Go library, which `cmd/testreceiver` also uses:

```go
import standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"

wh, err := standardwebhooks.NewWebhook(secret) // whsec_...
if err != nil {
	return err
}

http.HandleFunc("POST /webhooks", func(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	if err := wh.Verify(body, r.Header); err != nil {
		http.Error(w, "invalid signature", http.StatusBadRequest)
		return
	}
	// body is a verified event payload.
	w.WriteHeader(http.StatusNoContent)
})
```

See [`cmd/testreceiver/main.go`](../cmd/testreceiver/main.go) for a complete program.

Libraries for other languages, including Python, Ruby, PHP, Java and Rust, are listed at <https://github.com/standard-webhooks/standard-webhooks>.
