# Summary: Admin UI

## What was delivered

- `hookd serve` now serves a browser admin UI at `/admin` (`/` leads there), with nothing extra to build or install.
- Create accounts and API keys (each key shown once), revoke keys, and manage endpoints, under the same rules as the API.
- Send events from a form and watch each delivery's result on a self-refreshing event page.
- A built-in test receiver: a test endpoint verifies each request's signature, records it, and answers with a status you choose.
- A settings page shows every `HOOKD_*` setting and its source. Seven can be changed while Hookd runs. A health section is included.
- `HOOKD_ADMIN_UI=false` turns the UI off.

## What it looks like

I ran it locally against a fresh SQLite database:

```
GET  /                      -> 302 Location: /admin/
POST /admin/accounts  name=acme   -> 200 "The account acme (acct_01M63V5P…) was created with its first API key."
GET  /v1/endpoints  (with that key) -> 200 {"data":[],"has_more":false}
```

An `order.paid` event sent to a test endpoint showed `succeeded 200`, and the request was recorded as `signature verified`. A `90s` delivery timeout was refused (422): `HOOKD_DELIVERY_LEASE="1m0s": must be longer than HOOKD_DELIVERY_TIMEOUT="1m30s"`, while `20s` was saved with source `UI`. A cross-site post got 403. With the UI off, `/admin/` answered 404 and `/healthz` 200.

## How to try it

```bash
go run ./cmd/hookd serve
```

Then open <http://localhost:8080/> and follow steps 3–8 of the README.

## Not done or worth knowing

- No sign-in, as asked, and on by default: keep it on a trusted network or turn it off (`docs/deployment.md`).
- Retries, replay and account rename/delete were out of scope.
- No follow-up issues are open. A flaky test found along the way was fixed (hookd-52q).
