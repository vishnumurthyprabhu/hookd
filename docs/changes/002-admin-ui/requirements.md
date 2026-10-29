# Requirements: Admin UI

## Summary

Hookd gets a web admin UI, served by `hookd serve` itself, so that everything the README walk-through does with the CLI, curl and the separate test receiver can be done from a browser: create accounts and API keys, register endpoints, send events, check deliveries, receive and verify test webhooks, and change settings. People who use it do not need a shell on the host.

This change adds the UI and what it needs to work. The JSON API under `/v1` and the CLI commands keep working as they do now.

## Users

- **Admin**: whoever runs Hookd, for now the project owner alone. Uses the UI to set up accounts, configure endpoints and settings, and test delivery end to end. There is one kind of user, and the UI has no sign-in (see Assumptions).

## Functional requirements

### General

- R1. `hookd serve` serves the admin UI on its normal listen address under `/admin`. Opening `/` in a browser leads to it. No other process is needed.
- R2. The UI has no sign-in in this change. Anyone who can reach `/admin` can use every page.
- R3. A new setting turns the UI on or off. When it is off, `/admin` and the built-in test receiver answer 404. The API and `/healthz` are not affected.
- R4. Every page shows a visible notice that the UI has no sign-in and must only be reachable by trusted people.
- R5. Errors (invalid input, something not found, the database unavailable) are shown on the page in plain words, and the input already entered is kept.

### Accounts and API keys

- R6. The admin can list all accounts with their name, ID, creation time, and number of endpoints and API keys.
- R7. The admin can create an account by giving a name. Like `hookd account create`, this also creates the account's first API key.
- R8. The admin can create more API keys for an account. A new key is shown in full exactly once, right after it is created, with a way to copy it. After that only its prefix is ever shown.
- R9. The admin can list an account's API keys (prefix and creation time) and revoke one. A revoked key stops working with the API at once. The UI asks for confirmation before revoking.
- R10. All the other pages work on one account at a time. The admin picks the account, and the pages show only that account's endpoints and events.

### Endpoints

- R11. For the chosen account, the admin can list endpoints (URL, description, event types, enabled or not) and create, edit, enable, disable and delete them. The rules are the same as the API's (Hookd 1/6 R4–R8): the URL checks, the event type pattern, and an empty list meaning all event types.
- R12. The admin can reveal an endpoint's signing secret on demand and copy it. Secrets are hidden until revealed.
- R13. Deleting an endpoint asks for confirmation first.

### Sending events and checking deliveries

- R14. For the chosen account, the admin can send an event from a form with an event type, a JSON payload and an optional idempotency key. Invalid JSON, a payload over the size limit or a bad event type is reported without sending. After sending, the page shows the new event's ID, or says that an existing event was returned because of the idempotency key.
- R15. The admin can list the account's events, newest first, with their type, time and a summary of delivery results (for example "2 succeeded, 1 failed, 0 pending"). The list can be filtered by event type and is paged.
- R16. The admin can open an event and see its type, idempotency key, time, payload (formatted), and for each target endpoint: the URL, the delivery status, the HTTP status if there was one, and the error text. While any delivery is pending or in progress, the page updates on its own, or with one click, without sending the event again.

### Built-in test receiver

- R17. The admin can create a test endpoint for the chosen account in one step. It is an ordinary endpoint of the account whose URL points at a receiver built into Hookd, so no separate `cmd/testreceiver` is needed. The admin chooses its event types like for any endpoint.
- R18. The built-in receiver checks each request's Standard Webhooks signature with the test endpoint's secret and records the time, the `webhook-id`, the event type, the headers, the body, whether the signature verified (and why not if it did not), and the status it answered with.
- R19. For each test endpoint, the admin can set the HTTP status the receiver answers with (default 200, for example 500 to see failures), like `--status` on `cmd/testreceiver`.
- R20. The admin can see a test endpoint's received requests, newest first, and clear them. At least the last 100 requests of each test endpoint are kept, and older ones may be dropped. Deleting the test endpoint deletes its requests.
- R21. Requests that a test endpoint received are visible no matter which Hookd instance received them, and they are still there after a restart.

### Settings and health

- R22. The admin can see every `HOOKD_*` setting with its current value and where the value comes from: the default, the environment, or the UI. The database password is never shown, only whether it is set.
- R23. The admin can change these settings from the UI: `HOOKD_WORKERS`, `HOOKD_POLL_INTERVAL`, `HOOKD_DELIVERY_TIMEOUT`, `HOOKD_DELIVERY_LEASE`, `HOOKD_MAX_PAYLOAD_BYTES`, `HOOKD_IDEMPOTENCY_WINDOW` and `HOOKD_LOG_LEVEL`. A value saved in the UI overrides the environment variable. The admin can also reset a setting to its environment or default value.
- R24. Changed settings take effect without a restart, within a few seconds, on every running instance.
- R25. Values are checked by the same rules as at start-up, including that the delivery lease is longer than the delivery timeout. An invalid value is refused with the reason, and nothing is saved.
- R26. The settings that Hookd needs before it can read the database, or that only apply at start-up, are shown read-only and marked "set with environment variables": the listen address, every `HOOKD_DB_*` setting, `HOOKD_AUTO_MIGRATE`, `HOOKD_LOG_FORMAT` and the new UI on/off setting.
- R27. A health section shows whether the database can be reached, which database is in use (driver and host or file, never the password), whether migrations are up to date, and how many deliveries are pending or in progress right now.

## Non-functional requirements

- N1. The UI is part of the same binary and is built with nothing beyond Go. Running it locally still takes one command, `go run ./cmd/hookd serve`, with no Node.js or other build step and no files outside the binary.
- N2. The UI works in current desktop versions of Chrome, Firefox, Safari and Edge, and stays usable on a narrow window.
- N3. Secrets and API keys are never written to logs (as in Hookd 1/6 N4). This includes the bodies and headers that the test receiver records, which are kept only in the database.
- N4. Changes made in the UI keep every rule the API enforces: account scoping, the event type and URL checks, the payload limit, idempotency and soft delete. What the UI creates behaves exactly like what the API creates.
- N5. Forms that change data are protected against being sent from another website (cross-site request forgery), even though there is no sign-in.
- N6. Pages load in under a second with up to 1,000 accounts, 1,000 endpoints per account and 100,000 events per account, because the lists are paged.
- N7. The UI works with both SQLite and PostgreSQL, and with several instances behind a load balancer.

## Acceptance

This change is done when, starting from a fresh local setup with only `go run ./cmd/hookd serve` running and no other terminal used:

1. Opening http://localhost:8080/ in a browser shows the admin UI with the no-sign-in notice.
2. The admin creates the account "acme" and copies its API key. The key works with `curl … /v1/endpoints`, and the UI no longer shows it in full.
3. The admin creates a test endpoint subscribed to `order.paid` and a second, ordinary endpoint subscribed to `user.created`, and reveals the test endpoint's secret.
4. The admin sends an `order.paid` event from the UI. The event page shows the delivery to the test endpoint as succeeded, and none to the second endpoint. The test endpoint's received requests show the request with "signature verified".
5. The admin sets the test endpoint to answer 500 and sends another event. The event page shows it as failed with status 500.
6. The admin changes `HOOKD_DELIVERY_TIMEOUT` in the UI. The settings page shows the new value with its source "UI", and the next delivery uses it without a restart. Setting a timeout longer than the lease is refused.
7. The admin creates a second API key, revokes the first one, and the first key then gets 401 from the API.
8. With the UI turned off by its setting, `/admin` answers 404 and the API still works.

## Out of scope for this change

- Signing in, users, roles, and an audit log of who changed what
- Renaming or deleting accounts
- Editing database, listen address and other start-up settings from the UI
- Retries, attempt history, replay and the other features of later Hookd pieces
- A receiver-facing portal for Hookd customers' own customers
- Removing `cmd/testreceiver` or any CLI command

## Assumptions

- The admin alone uses the UI for now, so it has no sign-in, as asked. Because of that, it should only be reachable on a trusted network. The on/off setting (R3) and the notice (R4) are there so it can be shut off where that is not the case. The setting is on by default.
- "Settings" means Hookd's own `HOOKD_*` settings. Settings needed before the database can be read cannot be stored in it, so they stay environment-only (R26).
- The built-in receiver lives in the same service, so a test endpoint's URL points back at Hookd itself. Delivering to it goes through the normal workers, the same way a real endpoint is delivered to.
- Test endpoints count as normal endpoints in the API (they are listed by `GET /v1/endpoints`), and they are marked as test endpoints in the UI.
- The UI does not need to look polished. It should be plain, clear and work without JavaScript frameworks.
