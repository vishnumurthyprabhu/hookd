# Deploy to production

In production Hookd runs as one or more identical processes behind a load balancer, all sharing one PostgreSQL database. The database is also the delivery queue: instances claim deliveries with `FOR UPDATE SKIP LOCKED`, so they never send the same delivery at the same time, and nothing else (no cache, no broker) is needed.

```
                    ┌──────────────┐
  API clients ────▶ │ load balancer│ ──┬──▶ hookd serve ──┐
                    └──────────────┘   └──▶ hookd serve ──┤──▶ PostgreSQL
                                                          │
  receivers ◀──── deliveries from every instance ◀────────┘
```

Do not run more than one instance on SQLite. SQLite is for local use.

The steps below assume Linux hosts, PostgreSQL 13 or later on its own node, and Go 1.26 or later on the machine that builds Hookd.

## 1. Build the binary

On the build machine, in the repository folder:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o hookd ./cmd/hookd
```

This makes one static file, `hookd`, with the migrations built in. Use `GOARCH=arm64` for ARM hosts. Copy it to each application host, for example to `/usr/local/bin/hookd`, and check it runs:

```bash
hookd help
```

## 2. Prepare the PostgreSQL node

On the database node, open `psql` as a superuser:

```bash
sudo -u postgres psql
```

Create a user and a database owned by it. Choose a long random password:

```sql
CREATE USER hookd WITH PASSWORD 'change-me';
CREATE DATABASE hookd OWNER hookd;
REVOKE ALL ON DATABASE hookd FROM PUBLIC;
GRANT CONNECT, CREATE, TEMPORARY ON DATABASE hookd TO hookd;
\c hookd
GRANT USAGE, CREATE ON SCHEMA public TO hookd;
\q
```

Hookd's tables go in the `public` schema. As the owner of the database, `hookd` can already do all of this on PostgreSQL 15 and later; the grants make it explicit and cover older versions, where `public` belongs to `postgres`.

## 3. Let the application hosts connect

PostgreSQL must listen on the network and accept the `hookd` user from the application hosts.

In `postgresql.conf` (`SHOW config_file;` in `psql` shows where it is), set:

```
listen_addresses = '*'
```

or the node's private address instead of `*`. In `pg_hba.conf` (`SHOW hba_file;`), allow the application hosts' network, over TLS only:

```
# TYPE    DATABASE  USER   ADDRESS        METHOD
hostssl   hookd     hookd  10.0.1.0/24    scram-sha-256
```

Replace `10.0.1.0/24` with your application hosts' network. Each instance opens up to `HOOKD_DB_MAX_CONNS` connections (10 by default), so make sure `max_connections` covers every instance with room to spare.

## 4. Set up TLS to the database

Turn on TLS on the server in `postgresql.conf`, with a certificate whose name matches the host name Hookd connects to:

```
ssl = on
ssl_cert_file = '/etc/postgresql/tls/server.crt'
ssl_key_file  = '/etc/postgresql/tls/server.key'
```

The key file must belong to the `postgres` user and have mode `0600`. Restart PostgreSQL to apply steps 3 and 4:

```bash
sudo systemctl restart postgresql
```

On the Hookd side, `HOOKD_DB_SSLMODE` chooses how strict the connection is:

| `sslmode` | Encrypted | Server certificate checked |
|---|---|---|
| `disable` | no | no |
| `require` | yes | no |
| `verify-ca` | yes | signed by a trusted CA |
| `verify-full` | yes | signed by a trusted CA and matches `HOOKD_DB_HOST` |

Use `verify-full`. Hookd trusts the host's system CA certificates. If the database certificate is signed by your own CA, point Hookd at that CA's certificate with the standard PostgreSQL variable `PGSSLROOTCERT`:

```bash
export PGSSLROOTCERT=/etc/hookd/db-ca.crt
```

## 5. Set the database settings

On each application host, put the settings in a file only root can read, for example `/etc/hookd/hookd.env`:

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
```

```bash
sudo chmod 600 /etc/hookd/hookd.env
```

Add `PGSSLROOTCERT=/etc/hookd/db-ca.crt` if you need it (step 4). See [Configuration](configuration.md) for every other setting. `HOOKD_AUTO_MIGRATE=false` leaves migrations to the deploy step below.

To run the `hookd` commands below from a shell with these settings, load the file first:

```bash
set -a; . /etc/hookd/hookd.env; set +a
```

## 6. Apply the migrations

From one application host, with the settings loaded:

```bash
hookd migrate
```

```
applied 0001_init
```

Run it again at every deploy, before starting the new version. When there is nothing to do it says `database is up to date`. Migrations take a database lock, so two runs at once cannot both apply the same migration. If you would rather not have a separate step, leave `HOOKD_AUTO_MIGRATE=true` and each instance applies them when it starts.

## 7. Run the instances

Run `hookd serve` on each application host, under a service manager that restarts it. A systemd unit, `/etc/systemd/system/hookd.service`:

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

On SIGTERM, Hookd stops accepting requests, gives requests in flight up to 10 seconds to finish, and lets each worker finish the delivery it is sending (up to `HOOKD_DELIVERY_TIMEOUT`). Keep `TimeoutStopSec` above both. A delivery cut off by a crash is sent again by another worker once its lease (`HOOKD_DELIVERY_LEASE`) runs out, so receivers can get a duplicate and should drop repeats by `webhook-id`.

Hookd serves plain HTTP. Put the instances behind a load balancer or reverse proxy that terminates TLS, so API keys never cross the network unencrypted. Any HTTP load balancer works; there are no sticky sessions. For example, with nginx:

```nginx
upstream hookd {
    server 10.0.1.11:8080;
    server 10.0.1.12:8080;
}

server {
    listen 443 ssl;
    server_name hooks.example.com;
    ssl_certificate     /etc/nginx/tls/hooks.example.com.crt;
    ssl_certificate_key /etc/nginx/tls/hooks.example.com.key;

    client_max_body_size 1m;

    location / {
        proxy_pass http://hookd;
        proxy_set_header Host $host;
    }
}
```

Keep the load balancer's body size limit above `HOOKD_MAX_PAYLOAD_BYTES` plus a few KB, so Hookd rather than the proxy answers oversized events.

## 8. Check health

`GET /healthz` needs no API key. It answers `200 {"status":"ok"}` when the instance can reach the database, and `503` with code `unavailable` when it cannot. Point the load balancer's health check at it, and check each instance directly after a deploy:

```bash
curl -s http://10.0.1.11:8080/healthz
```

```json
{"status":"ok"}
```

## 9. Create accounts

Each Hookd customer gets an account and an API key. On any application host, with the settings loaded (step 5):

```bash
hookd account create --name acme
```

```
account_id: acct_01M636GV96GQFMA7V1G5N17DPQ
api_key:    hk_1r41QRqtUtyhEHFF6nnQZin__SkqHv-QP20dGALqfvE
Keep the API key safe: it is shown only once.
```

Give the key to the customer over a secure channel. Hookd stores only a hash of it, so a lost key cannot be shown again; create a new account instead. The customer can now call the API, as in the [API reference](api.md):

```bash
curl -s https://hooks.example.com/v1/endpoints \
  -H "Authorization: Bearer hk_..."
```

## Upgrading

1. Build the new binary and copy it to each host.
2. Run `hookd migrate` once.
3. Restart the instances one at a time (`sudo systemctl restart hookd`), checking `/healthz` after each.

## Backups

All of Hookd's state is in the PostgreSQL database: accounts, API key hashes, endpoints with their signing secrets, events and delivery status. Back it up like any PostgreSQL database (for example with `pg_dump` or continuous archiving), and protect the backups as secrets, because they contain the signing secrets.
