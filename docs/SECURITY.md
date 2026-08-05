# central-config — security posture

central-config is a control plane for a fleet of services. A relational database
is the source of truth, and every admin write is mirrored into NATS JetStream KV
buckets that consumers watch and cache in memory. A single successful write here
changes the runtime behaviour of every consuming service within milliseconds,
with no deploy and no review step in between. That is the threat model: the
admin API is a change channel for running systems, not a CRUD app.

This document describes what the service enforces itself, and what you have to
enforce around it — the parts of the model that live in deployment configuration
rather than in Go.

## What the API enforces

### Named admin tokens with environment scope

Write endpoints require a bearer token. Tokens are configured by environment
variable — no database, no user store — and each carries an operator name and an
environment scope:

```
ADMIN_TOKENS=admin:*:REPLACE_ME,ci:1|2:REPLACE_ME,release:3:REPLACE_ME
```

* Entries are comma separated.
* Each entry is `name:scope:secret`. The secret is everything after the second
  colon, so it may itself contain colons.
* Scope is `*` (every environment) or a `|`-separated list of
  `CONFIG_ENVIRONMENTS` ids — `1|2` is dev+staging, `3` is production.
* The name is what appears in the audit trail. Give one per person or per
  pipeline; that is the only thing that makes a change attributable.

A single `ADMIN_TOKEN` is also accepted, and is treated as a full-scope token
named `shared`. It is the smallest thing that works, which is why the bundled
console and the compose stack use it — but every change it makes is attributed
to `shared`, which is exactly the problem named tokens exist to solve. Use
`ADMIN_TOKENS` anywhere the audit trail has to mean something.

Secrets are compared with `crypto/subtle.ConstantTimeCompare`, against every
configured token without an early exit, so neither the secret nor a token's
position in the list is observable through response timing.

If neither variable is set, write auth is **disabled** and a loud warning is
logged at startup. That mode is for local development only.

### How the target environment is determined

A scope check is only as good as its answer to "which environment does this
write touch?". The routes fall into four cases, declared where each route is
registered (`internal/app/config.go`):

| Case | Routes | How the environment is found |
| --- | --- | --- |
| Body is authoritative | `POST /flags/values`, `POST /configs/values`, `POST /localization` | `environmentId` in the body — it is the value being inserted |
| Path is the environment | `DELETE /environments/{id}` | `{id}` *is* a `CONFIG_ENVIRONMENTS` id |
| Row lookup | `PUT /flags/values`, `PUT /configs/values`, `PUT /localization/values`, `DELETE /flags/values/{id}`, `DELETE /configs/values/{id}`, `DELETE /localization/{id}` | `SELECT ENVIRONMENT_ID` for the addressed row |
| No environment / every environment | `POST /flags`, `POST /microservices` (neutral); `DELETE /flags/{id}`, `DELETE /microservices/{id}`, `POST /environments` (global) | not applicable |

The row-lookup case matters. The update handlers key off the row id and ignore
any `environmentId` in the body, so trusting the body there would let a
dev-scoped token send `{"id": <prod row>, "environmentId": 1}` and change
production. The environment is therefore read back from the database before the
scope check, and a body that disagrees does not win. The microconfig and
localization updates additionally *rewrite* `ENVIRONMENT_ID`, so those writes are
checked against both the environment the row is in and the one it would move to.

The last row splits two ways:

* **Neutral** — creating a flag or microservice definition inserts a row that
  belongs to no environment and publishes nothing to KV. Any valid token may do
  it.
* **Global** — deleting a flag removes its values in *every* environment,
  including production; deleting a microservice and adding an environment change
  the reference set every scope is expressed in. These require a full-scope
  token. The same fail-closed treatment applies whenever the target cannot be
  determined at all (an unparseable id, a row that no longer exists).

### Audit trail

Every mutating request (`POST`/`PUT`/`PATCH`/`DELETE`) is recorded in
`CONFIG_AUDIT_LOG` (Oracle DDL in `migrations/008_config_audit_log.sql`, mirrored
in `internal/database/sqlite.go`). Reads are not audited.

Columns: `OCCURRED_AT`, `ACTOR` (token name), `METHOD`, `PATH`, `TARGET_DOMAIN`,
`TARGET_ID`, `ENVIRONMENT_ID`, `STATUS_CODE`, `REMOTE_ADDR`, `REQUEST_BODY`.

* Recording wraps the whole mux, so writes that never reach a handler are
  captured too — a 401 from an unknown token and a 403 from a dev token aimed at
  production both leave a row. That is the trail that shows someone probing.
* `REQUEST_BODY` is stored redacted: any JSON field whose name contains
  `password`, `secret`, `token`, `apikey`, `credential`, … has its value replaced
  with `[redacted]`, at any nesting depth. Bodies are truncated to fit the
  column, and a non-JSON body is dropped rather than stored verbatim.
* **An audit write never fails the request.** It runs after the change has been
  committed to Oracle and published to KV; failing then would report an error for
  a change that did happen. Failures are logged, the same philosophy as the KV
  publish path, which logs and lets the reconciler heal.

Read it back with `GET /audit?actor=&from=&to=&limit=&offset=`. `from`/`to`
accept `2026-01-01` or a full RFC 3339 instant; paging follows the same
`limit`/`offset` conventions as the other list endpoints. Unlike the config
reads, this endpoint requires a valid token — it carries request bodies.

### Request hardening

* **Body size** — every write handler decodes through `web.DecodeJSON`, which
  caps the body at 1 MiB via `http.MaxBytesReader`. The audit/scope middleware
  buffers the body under the same bound and puts it back, so the cap still
  applies downstream.
* **Rate limiting** — a per-caller token bucket over write endpoints, standard
  library only. `WRITE_RATE_LIMIT_PER_MINUTE` (default 120, `0` disables) sets
  both the sustained rate and the burst. Exhausted callers get `429` with a
  `Retry-After` header. The bucket is keyed by a hash of the bearer token when
  one is present — so one operator's script cannot spend another's budget — and
  by client address otherwise, which is what bounds an unauthenticated flood
  before it reaches the token check.
* **Server timeouts** — `ReadHeaderTimeout` 10s, `ReadTimeout` 30s,
  `WriteTimeout` 60s, `IdleTimeout` 120s. All four matter, and a header timeout
  alone is the easy mistake: it bounds how long a client may take to send its
  headers, but nothing after that. Without `ReadTimeout`, a client that sends
  valid headers and then trickles one byte of body per minute holds a connection
  open indefinitely, and enough such clients exhaust the server without ever
  looking like an attack.
* **Error responses** — handlers return domain sentinel messages or a flat
  `internal server error`; driver errors, SQL and file paths are logged
  server-side only.

### TLS

Set `TLS_CERT_FILE` and `TLS_KEY_FILE` and the admin API serves HTTPS with a TLS
1.2 floor. Leave them unset and it serves plain HTTP — which is what the compose
stack and the local two-terminal flow do — with a startup warning that admin
tokens are crossing the wire in clear text. In any shared environment, terminate
TLS here
or at a proxy that is not reachable except over TLS: the bearer token in the
`Authorization` header is a production-change credential.

## What you must harden before deploying this

The service secures its own admin API. It does not — and cannot — secure the
NATS side for you: that lives entirely in how you configure the cluster and issue
credentials. Two things need deciding before this carries anything you care
about.

### 1. Authenticate NATS

The shipped compose stack (`deploy/compose/docker-compose.yml`) starts `nats`
with `--jetstream` and nothing else. That is deliberate for a laptop, and wrong
anywhere else: anyone who can reach port 4222 can read every KV bucket, and —
because JetStream KV is just a stream — can also **write** them.

A forged KV entry is indistinguishable to a consumer from one central-config
published; it lands in that consumer's in-memory cache immediately. The
reconciler heals it, but "heals" means one reconcile interval (default 5 minutes)
of wrong behaviour, and a key with no corresponding database row survives until
the next full sweep.

Treat the compose file as a demonstration of the data path, not as a manifest
shape to promote. Any deployment larger than a single machine needs
authentication on the cluster before anything else on this page matters.

### 2. Scope consumer credentials per service, not per fleet

Authentication alone is not enough. A single shared credential handed to every
consumer leaves each of them able to read the whole config estate — one service's
credential reads another service's appsettings.

That matters because appsettings trees are where connection details and
integration endpoints naturally accumulate. The `env:VAR_NAME` marker convention
(see the seed data in `internal/database/sqlite.go`) keeps actual secret values
out of KV, and it is load-bearing precisely because a shared credential gives no
per-service isolation. Convention is a weak control; the permission model below
is the strong one, and it makes the convention a second line of defence rather
than the only one.

### The account and permission model that closes both

NATS accounts are the isolation boundary; within an account, user permissions
narrow subjects. KV bucket `X` is stream `KV_X` with subjects `$KV.X.>`.

1. **Two accounts.** `CONFIG` owns the three KV buckets (`FLAGS`,
   `MICROCONFIG`, `LOCALIZATION`) and the JetStream domain. Consumers live in a
   separate account and reach the buckets through explicit exports/imports, so
   the account boundary — not a subject string — is the default deny.

2. **One writer.** central-config gets the only credential in `CONFIG` with
   publish rights on `$KV.FLAGS.>`, `$KV.MICROCONFIG.>` and
   `$KV.LOCALIZATION.>`. Nothing else in the system may publish to them.

3. **Per-service consumer users, scoped by the key layout.** The keys are already
   shaped for this — `{environmentId}.{flagKey}`,
   `{environmentId}.{microserviceId}`, `{environmentId}.{microserviceId}.{locale}` —
   so a user for `cart-api` (id 2) in environment 3 gets subscribe permission on
   `$KV.MICROCONFIG.3.2`, `$KV.LOCALIZATION.3.2.*` and `$KV.FLAGS.3.>`, and
   publish permission on nothing. Flags stay environment-wide because they are
   not per-service; appsettings and localization become per-service, which is
   what closes the leak.

   ```
   # illustrative user permissions for cart-api (id 2) in environment 3
   permissions: {
     publish:   { deny: [">"] }
     subscribe: { allow: ["$KV.FLAGS.3.>",
                          "$KV.MICROCONFIG.3.2",
                          "$KV.LOCALIZATION.3.2.*",
                          "_INBOX.>"] }
   }
   ```

4. **Credentials per environment, not per fleet.** A dev consumer credential must
   not carry `3.` subjects. This is the NATS-side mirror of the token scoping the
   API now enforces; without it, a leaked dev credential still reads production
   config.

5. **NKey/JWT credentials over TLS**, issued per service and per environment,
   distributed through whatever secret store the deployment already uses, and
   rotatable without a redeploy of central-config. `NATS_CREDS` is already wired
   for the writer side (`internal/app/config.go`).

Until points 1–3 are in place, treat every KV value as readable by anything on
the NATS network and keep secrets out of appsettings trees — the `env:VAR_NAME`
marker convention is mandatory, and stays a good idea afterwards.
