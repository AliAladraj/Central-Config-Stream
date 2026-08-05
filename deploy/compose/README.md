# Local test stack

Runs the whole distribution path on one machine: an admin write hits the HTTP
API, lands in the database, is written through to a JetStream KV bucket, and is
pushed to a consuming service that serves it from memory.

The database here is **SQLite, not Oracle** (`DB_DRIVER=sqlite`). It exists so
the JetStream path can be exercised without an Oracle instance; the schema is
the same shape, but running this stack does not exercise the Oracle
repositories at all.

Everything is seeded into **environment 1 (`dev`)**. Environments 2 (`staging`)
and 3 (`prod`) exist as rows, but env 3 holds exactly one row — a *disabled*
`search_v2` — and env 2 holds none.

## With Docker

```bash
docker compose -f deploy/compose/docker-compose.yml up --build
```

The image builds the React bundle in a Node stage, so there is nothing to build
by hand. When it is up, open <http://localhost:8090>.

- `nats` — JetStream, file storage, monitoring on <http://localhost:8222>
- `central-config` — the service, `:8080`, `ADMIN_TOKEN=local-dev-token`
- `testconsole` — consumer + UI, `:8090`

## Without Docker

The console can host an embedded JetStream server, so two terminals are enough.
**Build the UI first** — `web/` is gitignored, so a fresh clone has none, and
the console has nothing to serve until it exists:

```bash
# once, before anything else
cd webui && npm ci && npm run build     # writes ../web
```

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

Both commands run from the repository root. The console waits for
`central-config` to create the KV buckets, so start order does not matter.

If you skip the build step the console still starts and answers on `:8090`, but
the page says the UI has not been built rather than 404-ing. Building it in
another terminal is picked up by a reload — the console re-checks per request
and does not need restarting.

The console does **not** auto-load a `.env` file; only the service does. Set its
variables on the command line as above.

Two console defaults worth knowing: it binds to `127.0.0.1` unless `PORT`
already carries a colon-form address or `BIND_ADDR` says otherwise, and it
refuses cross-origin requests. It proxies a full-scope admin token, so anyone
who can reach the port can change configuration for every service watching it.

## Seeded data

Environments: 1 `dev`, 2 `staging`, 3 `prod`.
Microservices: 1 `catalog-api`, 2 `cart-api`, 3 `storefront-api`.
Flag definitions: 7 `search_v2`, 8 `dark_mode`, 9 `new_pricing`.

| what | id | env | notes |
|---|---|---|---|
| flag value | 100 | 1 | `search_v2`, enabled, value `on` |
| flag value | 101 | 1 | `dark_mode`, disabled, value `off` |
| flag value | 102 | 1 | `new_pricing`, enabled, value `0.25` |
| flag value | 103 | **3** | `search_v2`, disabled — invisible to this console, which watches env 1 |
| appsettings | 200 | 1 | service 1 `catalog-api`. The full example tree used by [`docs/CONSUMER_CONTRACT.md`](../../docs/CONSUMER_CONTRACT.md), including the `env:` secret markers, at KV key `1.1` |
| appsettings | 201 | 1 | service 2 `cart-api`, at KV key `1.2` |
| appsettings | 202 | 1 | service 3 `storefront-api`, at KV key `1.3` |
| localization | 300 | 1 | service 1, `en-US`, at KV key `1.1.en-US` |
| localization | 301 | 1 | service 1, `pt-BR`, at KV key `1.1.pt-BR` |

The seed is idempotent (`INSERT OR IGNORE` with explicit ids), so a restart
keeps whatever you edited through the API. Source:
[`internal/database/sqlite.go`](../../internal/database/sqlite.go).

## What to try

Every request needs a bearer token — reads included. The stack's is
`local-dev-token`.

**1. Watch one write cross the whole path.** Turn `dark_mode` on and watch the
right-hand panel of the console update, with the measured delay between the HTTP
response and the KV push:

```bash
curl -s -X PUT http://localhost:8080/flags/values \
  -H "Authorization: Bearer local-dev-token" \
  -H "Content-Type: application/json" \
  -d '{"id":101,"enabled":1,"value":"on"}'
```

```json
{"id":101,"environmentId":1,"flagId":8,"value":"on","enabled":1,"updatedAt":"..."}
```

The consumer's cache has it before you can reload:

```bash
curl -s http://localhost:8090/api/state | jq '.flags.dark_mode'
# {"enabled": true, "value": "on", "updatedAt": "..."}
```

Note the type change — `"enabled": 1` on the API, `"enabled": true` in KV.

**2. Update appsettings.** Rows 200 and 201 are both env 1, so both land in the
console under `MICROCONFIG` keys `1.1` and `1.2`. Edit them in the Appsettings
view, or:

```bash
curl -s -X PUT http://localhost:8080/configs/values \
  -H "Authorization: Bearer local-dev-token" \
  -H "Content-Type: application/json" \
  -d '{"id":201,"microserviceId":2,"environmentId":1,"settingsJson":{"service":{"displayName":"Cart"},"http":{"timeoutMs":900}}}'
```

**3. Write to another environment and see nothing happen.** Flag value 103 is
env 3. The write succeeds and the console shows nothing, which is the
environment-prefix watch doing its job:

```bash
curl -s -X PUT http://localhost:8080/flags/values \
  -H "Authorization: Bearer local-dev-token" \
  -H "Content-Type: application/json" \
  -d '{"id":103,"enabled":1,"value":"on"}'
```

**4. Lose a race on purpose.** Read a row, then update it with a stale
`expectedUpdatedAt`; the write is refused with 409 and changes nothing:

```bash
curl -s -o /dev/null -w '%{http_code}\n' -X PUT http://localhost:8080/flags/values \
  -H "Authorization: Bearer local-dev-token" \
  -H "Content-Type: application/json" \
  -d '{"id":100,"enabled":0,"value":"off","expectedUpdatedAt":"2020-01-01T00:00:00Z"}'
# 409
```

**5. Restart the service.** Stop `central-config` and start it again: the
startup reconcile republishes everything and the console's cache is rebuilt
from KV.

**6. See a scoped token in action.** Restart the service with
`ADMIN_TOKENS='ci-dev:1|2:ci-secret'` instead of `ADMIN_TOKEN`. That token sees
environments 1 and 2 only — `GET /environments` returns two rows, `/inventory`
drops every env-3 row, `GET /flags/values/103` answers **404** rather than 403,
and a write aimed at env 3 answers **403**.

## The UI

React (Vite) in `webui/`, built to `web/`, which the Go console serves as static
files:

```bash
cd webui && npm ci && npm run build     # writes ../web
cd webui && npm run dev                 # or: hot reload on :5173, /api proxied to :8090
cd webui && npm test                    # vitest — the drift comparison has its own suite
```

Seven views: Overview (health, drift, recent pushes), Audit log, System, Flags,
Appsettings, Localization, and Environments & services — with an environment
switcher in the header that narrows all of them.

## Endpoints on the console

- `GET /api/state` — the consumer's in-memory cache
- `GET /api/events` — SSE stream of KV pushes
- `GET|POST|PUT|DELETE /api/admin/*` — proxied to `central-config` with the
  admin token attached, for a named list of API path prefixes only

## Note on the incremental reconciler

Between full sweeps the reconciler only reads rows whose `UPDATED_AT` is inside
the last window, using this process's clock. A large clock skew between the app
and the database could push a row outside the window; the one-minute overlap
absorbs small skew, and a full sweep every 12 cycles bounds the damage.
