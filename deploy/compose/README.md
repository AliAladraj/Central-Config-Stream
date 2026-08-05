# Local test stack

Runs the whole distribution path on one machine: an admin write hits the HTTP
API, lands in the database, is written through to a JetStream KV bucket, and is
pushed to a consuming service that serves it from memory.

The database here is **SQLite, not Oracle** (`DB_DRIVER=sqlite`). It exists so
the JetStream path can be exercised without an Oracle instance; the schema is
the same shape, but running this stack does not exercise the Oracle
repositories at all.

## With Docker

```bash
docker compose -f deploy/compose/docker-compose.yml up --build
```

Then open <http://localhost:8090>.

- `nats` — JetStream, file storage, monitoring on <http://localhost:8222>
- `central-config` — the service, `:8080`, `ADMIN_TOKEN=local-dev-token`
- `testconsole` — consumer + UI, `:8090`

## Without Docker

The console can host an embedded JetStream server, so two terminals are enough:

```bash
# terminal 1 — consumer UI + embedded NATS on :4222
NATS_EMBEDDED=true NATS_URL=nats://127.0.0.1:4222 ENVIRONMENT_ID=1 \
CENTRAL_CONFIG_URL=http://127.0.0.1:8080 ADMIN_TOKEN=local-dev-token \
PORT=:8090 WEB_DIR=web go run ./cmd/testconsole

# terminal 2 — the service
DB_DRIVER=sqlite CONN_STRING="file:./central-config.db" PORT=:8080 \
NATS_URL=nats://127.0.0.1:4222 PUBLISH_ENABLED=true NATS_REPLICAS=1 \
RECONCILE_INTERVAL=60s ADMIN_TOKEN=local-dev-token go run ./cmd/central-config
```

The console waits for `central-config` to create the KV buckets, so start order
does not matter.

## What to try

Seeded environment 1 (`dev`):

| what | id | notes |
|---|---|---|
| flag values | 100 `search_v2`, 101 `dark_mode`, 102 `new_pricing` | 103 is `search_v2` in env 3 (`prod`) — invisible to this console, which watches env 1 |
| appsettings | 200 (service 1, `catalog-api`), 201 (service 2, `cart-api`), 202 (service 3, `storefront-api`) | 200 is the full example appsettings tree used by `docs/CONSUMER_CONTRACT.md`, at KV key `1.1` |
| localization | 300 (`en-US`), 301 (`pt-BR`) | service 1 (`catalog-api`) |

1. Change flag 100 and watch the right-hand panel update, with the measured
   delay between the HTTP response and the KV push.
2. Update appsettings 200 or 201 — both are env 1, so both land in the console
   under `MICROCONFIG` keys `1.1` and `1.2`.
3. Update flag 103 (env 3): the write succeeds but the console shows nothing,
   which is the environment-prefix watch doing its job.
4. Stop `central-config` and restart it: the startup reconcile republishes
   everything, and the console's cache is rebuilt from KV.

## The UI

React (Vite) in `webui/`, built to `web/`, which the Go console serves as static
files. The Docker image builds it in a node stage; locally:

```bash
cd webui && npm install && npm run build     # writes ../web
cd webui && npm run dev                      # or: hot reload on :5173, /api proxied to :8090
```

## Endpoints on the console

- `GET /api/state` — the consumer's in-memory cache
- `GET /api/events` — SSE stream of KV pushes
- `GET|POST|PUT|DELETE /api/admin/*` — proxied to `central-config` with the
  admin token

## Note on the incremental reconciler

Between full sweeps the reconciler only reads rows whose `UPDATED_AT` is inside
the last window, using this process's clock. A large clock skew between the app
and the database could push a row outside the window; the one-minute overlap
absorbs small skew, and a full sweep every 12 cycles bounds the damage.
