# Requirements: Hookd 1/6 — Signed event delivery

## Summary

Hookd lets a software company (a "Hookd customer") send webhooks to its own customers without building delivery infrastructure. In this first piece, a developer can register their customers' endpoints over an HTTP API, choose which event types each endpoint receives, and send events. Hookd delivers each event to every subscribed endpoint and signs it under the Standard Webhooks spec, so receivers can verify it with existing libraries. A failed delivery is retried automatically on a backoff schedule for about a day. Every attempt is recorded with its time, response status, latency and error, and can be listed per event and per endpoint. Events, with their deliveries and attempts, are kept for 30 days and then deleted.

Hookd also has a web admin UI, served by `hookd serve` itself. Everything the CLI, the API and the separate test receiver do for setting up and testing can be done from a browser: create accounts and API keys, register endpoints, send events, check deliveries and their attempts, receive and verify test webhooks with a built-in receiver, and change some settings while Hookd runs. The admin does not need a shell on the host.

Retries, the attempt history and retention came in piece 2/6 and are described here. Later pieces add replay, resending missed events and auto-pause (3/6), secret rotation (4/6), per-endpoint isolation (5/6) and network safety (6/6). They are not part yet.

## Users

- **Developer**: an engineer at a Hookd customer. Calls the Hookd API from their own application to manage endpoints and send events, and looks up attempts to find out why a customer did not get a webhook.
- **Receiver**: the owner of an endpoint, usually the developer's own customer. Receives the webhook HTTP requests, including retries, and verifies their signatures. Uses `webhook-id` to drop duplicates. Receivers never call Hookd.
- **Operator**: whoever runs Hookd. Sets it up, gives out API keys, and sets the retry schedule and retention.
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
- R7. A disabled or deleted endpoint receives no new deliveries, and no retries of earlier ones (R47).
- R8. Event type names are free-form strings made of letters, digits, `.`, `_` and `-` (for example `order.paid`), up to 128 characters. Event types do not have to be declared before they are used.

### Sending events

- R9. A developer can send an event with an event type and a JSON payload (up to 256 KB). Hookd answers right away with the event's ID. Delivery happens in the background, not during the request.
- R10. The developer can give an idempotency key with an event. Sending the same key again within 24 hours returns the original event and does not create a new one or deliver it again.
- R11. An event is delivered to every endpoint of the account that is enabled when the event is accepted and subscribes to its type (or to all types).
- R12. A developer can get an event by ID: its type, payload, time and, for each target endpoint, whether delivery is pending, succeeded or failed, with the HTTP status and error of the last attempt if there was one, the number of attempts made so far, the time of the last attempt and, while a retry is waiting, the time of the next one. A delivery waiting for a retry is `pending`, as it is while waiting for its first attempt. Each attempt can also be looked up on its own (R52–R56).

### Delivery and signing

- R13. Each delivery is an HTTP `POST` of the event payload as JSON, with `Content-Type: application/json`.
- R14. Each delivery carries the Standard Webhooks headers:
  - `webhook-id`: the event's ID, the same for every endpoint, for every retry and for any later redelivery,
  - `webhook-timestamp`: the send time of this attempt in Unix seconds,
  - `webhook-signature`: `v1,<base64 HMAC-SHA256>` over `"{id}.{timestamp}.{body}"`, using the endpoint's current secret.

  A retry sends the same body with the same `webhook-id`. `webhook-timestamp` and `webhook-signature` are made new for each attempt.
- R15. A signature produced by Hookd must verify with at least one official Standard Webhooks library (for example the JavaScript or Python one) without changes.
- R16. A delivery attempt succeeds if the endpoint answers with any 2xx status within 15 seconds. Anything else counts as a failure: another status, a timeout, or a connection or TLS error.
- R17. A failed attempt is retried automatically (R46–R51). A delivery that succeeds on any attempt is `succeeded`, and a delivery whose last scheduled attempt fails is `failed`. After either, nothing more is sent.
- R18. Redirects are not followed. A 3xx response counts as a failure.

### Retries

- R46. The default schedule, counted from the previous attempt, is: right away, then after 5 seconds, 5 minutes, 30 minutes, 2 hours, 5 hours, 10 hours and 10 hours. That is 8 attempts over about 27½ hours. Each wait is varied at random by up to 10%, so retries to many endpoints do not all fire at the same moment.
- R47. Every kind of failure (R16, R18) is retried. If the endpoint is disabled or deleted while a delivery is waiting for a retry, the delivery is not sent again. It ends as `failed` with the error `endpoint_disabled`. Enabling the endpoint again does not bring it back.
- R48. Retries waiting for their time survive a restart: a retry due while Hookd was down is sent soon after it starts again. They are shared between instances like any other delivery, and a retry is never sent by two instances at once (apart from the at-least-once case after a crash in the middle of an attempt).
- R49. A retry is sent within a few seconds of its due time under normal load. Waiting retries do not hold workers or delay new events.
- R50. The schedule is a setting, `HOOKD_RETRY_SCHEDULE`: a list of waits, for example `5s,5m,30m,2h,5h,10h,10h`, at most 20. The value `none` means no retries (one attempt). The admin can change it in the admin UI (R41), and a change applies to the next wait that is scheduled; waits already scheduled keep their time.
- R51. The schedule's values are checked when they are saved and at start-up, by the same rules (R43).

### Attempt history

- R52. Every attempt is recorded, including the one that found the endpoint disabled: the event, the endpoint, the attempt number (1, 2, …), the time it started, the outcome (`succeeded` or `failed`), the HTTP status if there was one, the latency in milliseconds, and the error text if it failed.
- R53. A developer can list the attempts of one event, across all its endpoints, oldest first. The list can be filtered to one endpoint.
- R54. A developer can list the attempts made to one endpoint, newest first, across all events. The list can be filtered by outcome (`succeeded` or `failed`) and is paged.
- R55. A developer can get one attempt by its ID.
- R56. Attempts are scoped by account like everything else: an event, endpoint or attempt of another account answers 404. The attempts of a deleted endpoint can still be listed until they expire (R57).

### Retention

- R57. Events are kept for 30 days after they were accepted. After that, Hookd deletes the event together with its deliveries and attempts, without the operator having to do anything. Expired events then answer 404 in the API and "not found" in the admin UI.
- R58. The retention period is a setting, `HOOKD_RETENTION`, default `720h` (30 days). It must be longer than both the idempotency window and the total of the retry schedule with its jitter, so that no event is deleted while its key is still honoured or a retry is still waiting. It is set with an environment variable only.
- R59. Expired events are removed in small batches in the background, at least once an hour, on any instance. Deleting them does not lock the tables for long or slow down delivery, and it is safe with several instances running.

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
- R33. The admin can list the account's events, newest first, with their type, time and a summary of delivery results (for example "2 succeeded, 1 failed, 0 pending"). Deliveries waiting for a retry count as pending. The list can be filtered by event type and is paged.
- R34. The admin can open an event and see its type, idempotency key, time, payload (formatted), and for each target endpoint: the URL, the delivery status, the HTTP status if there was one, the error text, the number of attempts, when the next retry is due if one is waiting, and the list of its attempts with their time, outcome, HTTP status, latency and error. While any delivery is pending or in progress, including while a retry is waiting, the page updates on its own (more slowly while a retry is still far off), or with one click, without sending the event again.
- R60. The endpoint page shows the endpoint's recent attempts across events, newest first and paged, filterable by outcome, with a link to each attempt's event.

### Built-in test receiver

- R35. The admin can create a test endpoint for the chosen account in one step. It is an ordinary endpoint of the account whose URL points at a receiver built into Hookd, so no separate `cmd/testreceiver` is needed. The admin chooses its event types like for any endpoint.
- R36. The built-in receiver checks each request's Standard Webhooks signature with the test endpoint's secret and records the time, the `webhook-id`, the event type, the headers, the body, whether the signature verified (and why not if it did not), and the status it answered with.
- R37. For each test endpoint, the admin can set the HTTP status the receiver answers with (default 200, for example 500 to see failures), like `--status` on `cmd/testreceiver`.
- R38. The admin can see a test endpoint's received requests, newest first, and clear them. At least the last 100 requests of each test endpoint are kept, and older ones may be dropped. Deleting the test endpoint deletes its requests.
- R39. Requests that a test endpoint received are visible no matter which Hookd instance received them, and they are still there after a restart.

### Settings and health

- R40. The admin can see every `HOOKD_*` setting with its current value and where the value comes from: the default, the environment, or the UI. The database password is never shown, only whether it is set.
- R41. The admin can change these settings from the UI: `HOOKD_WORKERS`, `HOOKD_POLL_INTERVAL`, `HOOKD_DELIVERY_TIMEOUT`, `HOOKD_DELIVERY_LEASE`, `HOOKD_MAX_PAYLOAD_BYTES`, `HOOKD_IDEMPOTENCY_WINDOW`, `HOOKD_LOG_LEVEL` and `HOOKD_RETRY_SCHEDULE`. A value saved in the UI overrides the environment variable. The admin can also reset a setting to its environment or default value.
- R42. Changed settings take effect without a restart, within a few seconds, on every running instance.
- R43. Values are checked by the same rules as at start-up, including that the delivery lease is longer than the delivery timeout and that the retention period stays longer than the idempotency window and the retry schedule's total (R58). An invalid value is refused with the reason, and nothing is saved.
- R44. The settings that Hookd needs before it can read the database, or that only apply at start-up, are shown read-only and marked "set with environment variables": the listen address, every `HOOKD_DB_*` setting, `HOOKD_AUTO_MIGRATE`, `HOOKD_LOG_FORMAT`, `HOOKD_ADMIN_UI` and `HOOKD_ADMIN_RECEIVER_URL`. So is `HOOKD_RETENTION`, because shortening it by mistake deletes data.
- R45. A health section shows whether the database can be reached, which database is in use (driver and host or file, never the password), whether migrations are up to date, how many deliveries are pending or in progress right now, and how many are waiting for a retry.

## Non-functional requirements

- N1. Hookd runs as a single deployable service that can be started locally with one documented command, together with any database it needs.
- N2. An accepted event is stored durably before the send-event request returns. If the service restarts, events that are accepted but not delivered yet are still delivered.
- N3. Under normal conditions a delivery starts within a few seconds of the event being accepted.
- N4. Secrets and API keys are never written to logs. This includes the bodies and headers that the built-in test receiver records, which are kept only in the database. Each attempt, first or retry, is logged with its attempt number.
- N5. The API is JSON over HTTP, with clear error responses: a machine-readable code and a message.
- N6. The admin UI is part of the same binary and is built with nothing beyond Go. Running it locally still takes one command, `go run ./cmd/hookd serve`, with no Node.js or other build step and no files outside the binary.
- N7. The admin UI works in current desktop versions of Chrome, Firefox, Safari and Edge, and stays usable on a narrow window.
- N8. Changes made in the admin UI keep every rule the API enforces: account scoping, the event type and URL checks, the payload limit, idempotency and soft delete. What the UI creates behaves exactly like what the API creates.
- N9. Admin UI forms that change data are protected against being sent from another website (cross-site request forgery), even though there is no sign-in.
- N10. Admin UI pages load in under a second with up to 1,000 accounts, 1,000 endpoints per account and 100,000 events per account, because the lists are paged.
- N11. The admin UI works with both SQLite and PostgreSQL, and with several instances behind a load balancer.
- N12. An attempt's record and the scheduling of its next retry are stored together, so a crash never leaves a delivery with a recorded failure and no next step, or with a next step and no record.
- N13. The attempt lists answer in under a second with 100,000 events per account and 8 attempts per delivery, because they are paged and indexed.
- N14. Existing databases are migrated without losing data: deliveries made before retries existed get one attempt each from their recorded outcome.

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

Retries and the attempt history are done when, starting from a fresh local setup with `go run ./cmd/hookd serve`:

1. The admin sets `HOOKD_RETRY_SCHEDULE` to `5s,10s,20s` in the admin UI.
2. A test endpoint answering 500 gets an event. The event page shows the first attempt failed with 500, a second attempt about 5 seconds later, also failed, and the time of the next retry. `GET /v1/events/{id}` shows the delivery as `pending` with 2 attempts and the next attempt time.
3. The admin sets the test endpoint to answer 200 before the next retry. The next attempt succeeds, the delivery shows `succeeded` after 3 attempts, and no further attempt is made. The test endpoint's received requests show three requests with the same `webhook-id`, each with "signature verified".
4. The event's attempt list in the API shows the three attempts with their status, latency and error, and the endpoint's attempt list shows the same three, newest first, and can be filtered to the two failed ones.
5. An event to an endpoint that keeps answering 500 ends `failed` after 4 attempts, and no fifth is made.
6. Disabling an endpoint while a retry is waiting makes the delivery end `failed` with `endpoint_disabled`, and no further request reaches the receiver.
7. Stopping Hookd while a retry is waiting and starting it again after the due time sends the retry soon after start.
8. Events older than `HOOKD_RETENTION` are deleted with their deliveries and attempts, and newer ones are kept (tested in the Store tests with a fake clock).
9. Another account's key gets 404 for the attempt routes of the first account's events, endpoints and attempts.

## Out of scope

- Replaying events, resending missed events, auto-pausing endpoints (piece 3)
- Retrying a delivery by hand, or cancelling a waiting retry other than by disabling the endpoint
- Storing the endpoint's response body or response headers with an attempt
- A retry schedule per account or per endpoint, or honouring a `Retry-After` header
- Per-account retention periods, and exporting history before it expires
- Alerting or notifying anyone when deliveries fail
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
- The retry schedule follows the one common in webhook services: a few quick retries for brief blips, then longer waits, ending a little over a day after the event. Every failure is retried, including 4xx answers, because a receiver's 4xx is often a passing misconfiguration.
- The API keeps three delivery statuses (`pending`, `succeeded`, `failed`). Waiting for a retry is shown with the next-attempt time rather than a new status, so existing clients keep working.
- Retention applies to events, deliveries and attempts. Accounts, API keys, endpoints, settings and test receivers are not expired, and a test receiver's stored requests keep their own rule (the last 100).
- An attempt cut short by a crash is not recorded. Its delivery is claimed again after the lease, and that attempt is recorded normally.
