# Requirements: Hookd 1/6 — Signed event delivery

## Summary

Hookd lets a software company (a "Hookd customer") send webhooks to its own customers without building delivery infrastructure. In this first piece, a developer can register their customers' endpoints over an HTTP API, choose which event types each endpoint receives, and send events. Hookd delivers each event once to every subscribed endpoint and signs it under the Standard Webhooks spec, so receivers can verify it with existing libraries.

Hookd also has a web admin UI, served by `hookd serve` itself. Everything the CLI, the API and the separate test receiver do for setting up and testing can be done from a browser: create accounts and API keys, register endpoints, send events, check deliveries, receive and verify test webhooks with a built-in receiver, and change some settings while Hookd runs. The admin does not need a shell on the host.

Later pieces add retries and delivery history (2/6), replay and auto-pause (3/6), secret rotation (4/6), per-endpoint isolation (5/6) and network safety (6/6). They are not part of this change.

## Users

- **Developer**: an engineer at a Hookd customer. Calls the Hookd API from their own application to manage endpoints and send events.
- **Receiver**: the owner of an endpoint, usually the developer's own customer. Receives the webhook HTTP requests and verifies their signatures. Receivers never call Hookd.
- **Operator**: whoever runs Hookd. Sets it up and gives out API keys.
- **Admin**: the operator using the admin UI, for now the project owner alone. Uses it to set up accounts, configure endpoints and settings, and test delivery end to end. There is one kind of admin user, and the UI has no sign-in (see Assumptions).

## Functional requirements

### Accounts and authentication

- R1. Hookd supports several separate accounts (one per Hookd customer). Each account sees only its own endpoints and events.
- R2. Every API request is authenticated with an API key sent as a bearer token. Requests without a valid key are rejected with 401.
- R3. The operator can create an account and its API key with a command-line command, or in the admin UI (R25). A self-service sign-up is not needed.

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

### Admin UI: general

- R19. `hookd serve` serves the admin UI on its normal listen address under `/admin`. Opening `/` in a browser leads to it. No other process is needed.
- R20. The UI has no sign-in. Anyone who can reach `/admin` can use every page.
- R21. A setting turns the UI on or off. When it is off, `/admin` and the built-in test receiver answer 404. The API and `/healthz` are not affected.
- R22. Every page shows a visible notice that the UI has no sign-in and must only be reachable by trusted people.
- R23. Errors (invalid input, something not found, the database unavailable) are shown on the page in plain words, and the input already entered is kept.

### Admin UI: accounts and API keys

- R24. The admin can list all accounts with their name, ID, creation time, and number of endpoints and API keys.
- R25. The admin can create an account by giving a name. Like `hookd account create`, this also creates the account's first API key.
- R26. The admin can create more API keys for an account. A new key is shown in full exactly once, right after it is created, with a way to copy it. After that only its prefix is ever shown.
- R27. The admin can list an account's API keys (prefix and creation time) and revoke one. A revoked key stops working with the API at once. The UI asks for confirmation before revoking.
- R28. All the other pages work on one account at a time. The admin picks the account, and the pages show only that account's endpoints and events.

### Admin UI: endpoints

- R29. For the chosen account, the admin can list endpoints (URL, description, event types, enabled or not) and create, edit, enable, disable and delete them. The rules are the same as the API's (R4–R8): the URL checks, the event type pattern, and an empty list meaning all event types.
- R30. The admin can reveal an endpoint's signing secret on demand and copy it. Secrets are hidden until revealed.
- R31. Deleting an endpoint asks for confirmation first.

### Admin UI: sending events and checking deliveries

- R32. For the chosen account, the admin can send an event from a form with an event type, a JSON payload and an optional idempotency key. Invalid JSON, a payload over the size limit or a bad event type is reported without sending. After sending, the page shows the new event's ID, or says that an existing event was returned because of the idempotency key.
- R33. The admin can list the account's events, newest first, with their type, time and a summary of delivery results (for example "2 succeeded, 1 failed, 0 pending"). The list can be filtered by event type and is paged.
- R34. The admin can open an event and see its type, idempotency key, time, payload (formatted), and for each target endpoint: the URL, the delivery status, the HTTP status if there was one, and the error text. While any delivery is pending or in progress, the page updates on its own, or with one click, without sending the event again.

### Built-in test receiver

- R35. The admin can create a test endpoint for the chosen account in one step. It is an ordinary endpoint of the account whose URL points at a receiver built into Hookd, so no separate `cmd/testreceiver` is needed. The admin chooses its event types like for any endpoint.
- R36. The built-in receiver checks each request's Standard Webhooks signature with the test endpoint's secret and records the time, the `webhook-id`, the event type, the headers, the body, whether the signature verified (and why not if it did not), and the status it answered with.
- R37. For each test endpoint, the admin can set the HTTP status the receiver answers with (default 200, for example 500 to see failures), like `--status` on `cmd/testreceiver`.
- R38. The admin can see a test endpoint's received requests, newest first, and clear them. At least the last 100 requests of each test endpoint are kept, and older ones may be dropped. Deleting the test endpoint deletes its requests.
- R39. Requests that a test endpoint received are visible no matter which Hookd instance received them, and they are still there after a restart.

### Settings and health

- R40. The admin can see every `HOOKD_*` setting with its current value and where the value comes from: the default, the environment, or the UI. The database password is never shown, only whether it is set.
- R41. The admin can change these settings from the UI: `HOOKD_WORKERS`, `HOOKD_POLL_INTERVAL`, `HOOKD_DELIVERY_TIMEOUT`, `HOOKD_DELIVERY_LEASE`, `HOOKD_MAX_PAYLOAD_BYTES`, `HOOKD_IDEMPOTENCY_WINDOW` and `HOOKD_LOG_LEVEL`. A value saved in the UI overrides the environment variable. The admin can also reset a setting to its environment or default value.
- R42. Changed settings take effect without a restart, within a few seconds, on every running instance.
- R43. Values are checked by the same rules as at start-up, including that the delivery lease is longer than the delivery timeout. An invalid value is refused with the reason, and nothing is saved.
- R44. The settings that Hookd needs before it can read the database, or that only apply at start-up, are shown read-only and marked "set with environment variables": the listen address, every `HOOKD_DB_*` setting, `HOOKD_AUTO_MIGRATE`, `HOOKD_LOG_FORMAT`, `HOOKD_ADMIN_UI` and `HOOKD_ADMIN_RECEIVER_URL`.
- R45. A health section shows whether the database can be reached, which database is in use (driver and host or file, never the password), whether migrations are up to date, and how many deliveries are pending or in progress right now.

## Non-functional requirements

- N1. Hookd runs as a single deployable service that can be started locally with one documented command, together with any database it needs.
- N2. An accepted event is stored durably before the send-event request returns. If the service restarts, events that are accepted but not delivered yet are still delivered.
- N3. Under normal conditions a delivery starts within a few seconds of the event being accepted.
- N4. Secrets and API keys are never written to logs. This includes the bodies and headers that the built-in test receiver records, which are kept only in the database.
- N5. The API is JSON over HTTP, with clear error responses: a machine-readable code and a message.
- N6. The admin UI is part of the same binary and is built with nothing beyond Go. Running it locally still takes one command, `go run ./cmd/hookd serve`, with no Node.js or other build step and no files outside the binary.
- N7. The admin UI works in current desktop versions of Chrome, Firefox, Safari and Edge, and stays usable on a narrow window.
- N8. Changes made in the admin UI keep every rule the API enforces: account scoping, the event type and URL checks, the payload limit, idempotency and soft delete. What the UI creates behaves exactly like what the API creates.
- N9. Admin UI forms that change data are protected against being sent from another website (cross-site request forgery), even though there is no sign-in.
- N10. Admin UI pages load in under a second with up to 1,000 accounts, 1,000 endpoints per account and 100,000 events per account, because the lists are paged.
- N11. The admin UI works with both SQLite and PostgreSQL, and with several instances behind a load balancer.

## Acceptance

This piece is done when, starting from a fresh local setup:

1. The operator creates an account and gets an API key.
2. Using that key, the developer registers two endpoints on a local test receiver: one subscribed to `order.paid`, and one subscribed to `user.created`.
3. The developer sends an `order.paid` event. Only the first endpoint receives it.
4. The receiver checks the request with an official Standard Webhooks library and the endpoint's secret, and the check passes.
5. Fetching the event shows delivery succeeded for the first endpoint.
6. An event sent to an endpoint whose receiver returns 500 shows as failed, with status 500.
7. A request with another account's key cannot see or change the first account's endpoints or events.

The admin UI is done when, starting from a fresh local setup with only `go run ./cmd/hookd serve` running and no other terminal used:

1. Opening http://localhost:8080/ in a browser shows the admin UI with the no-sign-in notice.
2. The admin creates the account "acme" and copies its API key. The key works with `curl … /v1/endpoints`, and the UI no longer shows it in full.
3. The admin creates a test endpoint subscribed to `order.paid` and a second, ordinary endpoint subscribed to `user.created`, and reveals the test endpoint's secret.
4. The admin sends an `order.paid` event from the UI. The event page shows the delivery to the test endpoint as succeeded, and none to the second endpoint. The test endpoint's received requests show the request with "signature verified".
5. The admin sets the test endpoint to answer 500 and sends another event. The event page shows it as failed with status 500.
6. The admin changes `HOOKD_DELIVERY_TIMEOUT` in the UI. The settings page shows the new value with its source "UI", and the next delivery uses it without a restart. Setting a timeout longer than the lease is refused.
7. The admin creates a second API key, revokes the first one, and the first key then gets 401 from the API.
8. With the UI turned off by its setting, `/admin` answers 404 and the API still works.

## Out of scope for this piece

- Retries and the attempt history API (piece 2), and the 30-day retention of event history (piece 2)
- Replaying events, resending missed events, auto-pausing endpoints (piece 3)
- Rotating secrets (piece 4)
- Guaranteeing that a slow endpoint cannot delay the others (piece 5)
- Blocking internal or private network URLs, and fixed egress IPs (piece 6)
- Self-service sign-up, billing, receiver-facing portal
- In the admin UI: signing in, users, roles, and an audit log of who changed what; renaming or deleting accounts; editing database, listen address and other start-up settings
- Removing `cmd/testreceiver` or any CLI command

## Assumptions

- Hookd is multi-tenant: one installation serves several Hookd customers, each with its own API key(s).
- The API is used directly by developers. No SDKs are needed in this piece.
- Payloads are JSON objects. Other content types are not supported.
- The event ID doubles as the `webhook-id`, so receivers can use it to drop duplicates.
- Scale for this piece is modest (tens of events per second on one instance). High-throughput tuning comes later.
- The admin alone uses the admin UI for now, so it has no sign-in. Because of that, it should only be reachable on a trusted network. The on/off setting (R21) and the notice (R22) are there so it can be shut off where that is not the case. The setting is on by default.
- "Settings" means Hookd's own `HOOKD_*` settings. Settings needed before the database can be read cannot be stored in it, so they stay environment-only (R44).
- The built-in receiver lives in the same service, so a test endpoint's URL points back at Hookd itself. Delivering to it goes through the normal workers, the same way a real endpoint is delivered to.
- Test endpoints count as normal endpoints in the API (they are listed by `GET /v1/endpoints`), and they are marked as test endpoints in the UI.
- The admin UI does not need to look polished. It should be plain, clear and work without JavaScript frameworks.
