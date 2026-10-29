# Requirements: Hookd 1/6 — Signed event delivery

## Summary

Hookd lets a software company (a "Hookd customer") send webhooks to its own customers without building delivery infrastructure. In this first piece, a developer can register their customers' endpoints over an HTTP API, choose which event types each endpoint receives, and send events. Hookd delivers each event once to every subscribed endpoint and signs it under the Standard Webhooks spec, so receivers can verify it with existing libraries.

Later pieces add retries and delivery history (2/6), replay and auto-pause (3/6), secret rotation (4/6), per-endpoint isolation (5/6) and network safety (6/6). They are not part of this change.

## Users

- **Developer**: an engineer at a Hookd customer. Calls the Hookd API from their own application to manage endpoints and send events.
- **Receiver**: the owner of an endpoint, usually the developer's own customer. Receives the webhook HTTP requests and verifies their signatures. Receivers never call Hookd.
- **Operator**: whoever runs Hookd. Sets it up and gives out API keys.

## Functional requirements

### Accounts and authentication

- R1. Hookd supports several separate accounts (one per Hookd customer). Each account sees only its own endpoints and events.
- R2. Every API request is authenticated with an API key sent as a bearer token. Requests without a valid key are rejected with 401.
- R3. The operator can create an account and its API key with a command-line command. A self-service sign-up or dashboard is not needed.

### Endpoints

- R4. A developer can create an endpoint with:
  - a URL (required; `https://`, or `http://` so that local testing works),
  - an optional description,
  - a list of event types it subscribes to. An empty list, or leaving it out, means "all event types".
- R5. When an endpoint is created, Hookd generates a signing secret for it in the Standard Webhooks format (`whsec_` followed by base64 of at least 24 random bytes). The secret can be fetched later through the API.
- R6. A developer can list their endpoints, get one, update its URL, description, event types and enabled/disabled state, and delete it.
- R7. A disabled or deleted endpoint receives no new deliveries.
- R8. Event type names are free-form strings made of letters, digits, `.`, `_` and `-` (for example `order.paid`), up to 128 characters. Event types do not have to be declared before they are used.

### Sending events

- R9. A developer can send an event with an event type and a JSON payload (up to 256 KB). Hookd answers right away with the event's ID. Delivery happens in the background, not during the request.
- R10. The developer can give an idempotency key with an event. Sending the same key again within 24 hours returns the original event and does not create a new one or deliver it again.
- R11. An event is delivered to every endpoint of the account that is enabled when the event is accepted and subscribes to its type (or to all types).
- R12. A developer can get an event by ID: its type, payload, time and, for each target endpoint, whether delivery is pending, succeeded or failed (with the HTTP status if there was one). The full attempt history comes in piece 2.

### Delivery and signing

- R13. Each delivery is an HTTP `POST` of the event payload as JSON, with `Content-Type: application/json`.
- R14. Each delivery carries the Standard Webhooks headers:
  - `webhook-id`: the event's ID, the same for every endpoint and for any later redelivery,
  - `webhook-timestamp`: the send time in Unix seconds,
  - `webhook-signature`: `v1,<base64 HMAC-SHA256>` over `"{id}.{timestamp}.{body}"`, using the endpoint's secret.
- R15. A signature produced by Hookd must verify with at least one official Standard Webhooks library (for example the JavaScript or Python one) without changes.
- R16. A delivery succeeds if the endpoint answers with any 2xx status within 15 seconds. Anything else counts as a failure: another status, a timeout, or a connection or TLS error.
- R17. In this piece each delivery is attempted once. A failure is recorded (R12) and is not retried. Retries come in piece 2.
- R18. Redirects are not followed. A 3xx response counts as a failure.

## Non-functional requirements

- N1. Hookd runs as a single deployable service that can be started locally with one documented command, together with any database it needs.
- N2. An accepted event is stored durably before the send-event request returns. If the service restarts, events that are accepted but not delivered yet are still delivered.
- N3. Under normal conditions a delivery starts within a few seconds of the event being accepted.
- N4. Secrets and API keys are never written to logs.
- N5. The API is JSON over HTTP, with clear error responses: a machine-readable code and a message.

## Acceptance

This piece is done when, starting from a fresh local setup:

1. The operator creates an account and gets an API key.
2. Using that key, the developer registers two endpoints on a local test receiver: one subscribed to `order.paid`, and one subscribed to `user.created`.
3. The developer sends an `order.paid` event. Only the first endpoint receives it.
4. The receiver checks the request with an official Standard Webhooks library and the endpoint's secret, and the check passes.
5. Fetching the event shows delivery succeeded for the first endpoint.
6. An event sent to an endpoint whose receiver returns 500 shows as failed, with status 500.
7. A request with another account's key cannot see or change the first account's endpoints or events.

## Out of scope for this piece

- Retries and the attempt history API (piece 2), and the 30-day retention of event history (piece 2)
- Replaying events, resending missed events, auto-pausing endpoints (piece 3)
- Rotating secrets (piece 4)
- Guaranteeing that a slow endpoint cannot delay the others (piece 5)
- Blocking internal or private network URLs, and fixed egress IPs (piece 6)
- Web dashboard, self-service sign-up, billing, receiver-facing portal

## Assumptions

- Hookd is multi-tenant: one installation serves several Hookd customers, each with its own API key(s).
- The API is used directly by developers. No SDKs are needed in this piece.
- Payloads are JSON objects. Other content types are not supported.
- The event ID doubles as the `webhook-id`, so receivers can use it to drop duplicates.
- Scale for this piece is modest (tens of events per second on one instance). High-throughput tuning comes later.
