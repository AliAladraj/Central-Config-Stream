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

> Reporting a vulnerability is a different document: [`SECURITY.md` at the
> repository root](../SECURITY.md) is the disclosure policy. Do not open a
> public issue for a security problem.

## What the API enforces

### Everything except the probes needs a token

`GET /health`, `GET /livez` and `GET /metrics` are open: they carry process
state and counters, never configuration values. **Every other route answers
`401` without a valid bearer token — the reads as well as the writes.** A read
is not a lesser operation here. One anonymous `GET /configs/values` would
otherwise hand over the whole configuration estate: every appsettings tree,
every bundle and every flag value, production as readily as dev.

### Named admin tokens with environment scope

Tokens are configured by environment variable — no database, no user store — and
each carries an operator name and an environment scope:

```
ADMIN_TOKENS=admin:*:REPLACE_ME,ci:1|2:REPLACE_ME,release:3:REPLACE_ME
```

* Entries are comma separated.
* Each entry is `name:scope:secret`. The secret is everything after the second
  colon, so it may itself contain colons — but **not a comma**. The variable is
  split on commas before it is split on colons, so a comma inside a secret
  quietly starts an entry, and possibly a token, of its own.
* Scope is `*` (every environment) or a `|`-separated list of
  `CONFIG_ENVIRONMENTS` ids — `1|2` is dev+staging, `3` is production.
* The name is what appears in the audit trail. Give one per person or per
  pipeline; that is the only thing that makes a change attributable. It is
  constrained to letters, digits, dot, dash and underscore, up to 100
  characters — the width of the `ACTOR` column it is recorded in. Checking it
  rather than trusting it is also what catches the comma case above: the
  fragment after a stray comma is almost never a usable name.

Any of these violations is a **fatal startup error**. Credentials are parsed
before the database is opened and before the listener binds, so a malformed
`ADMIN_TOKENS` stops the process rather than silently leaving writes less
protected than the operator believes. The error message carries the entry
number, never the entry: startup logs are not a place to print secrets.

A single `ADMIN_TOKEN` is also accepted, and is treated as a full-scope token
named `shared`. It is the smallest thing that works, which is why the bundled
console and the compose stack use it — but every change it makes is attributed
to `shared`, which is exactly the problem named tokens exist to solve. Use
`ADMIN_TOKENS` anywhere the audit trail has to mean something.

Secrets are compared with `crypto/subtle.ConstantTimeCompare`, against every
configured token without an early exit, so neither the secret nor a token's
position in the list is observable through response timing.

If neither variable is set, auth is **disabled entirely** — reads and writes
both — and a loud warning is logged at startup. Every request is then treated as
a full-scope caller with no name, so nothing is scoped and the audit trail has
no actor to record. That mode is for local development only.

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

### The same scope narrows reads

A token's environment scope is not only a write permission. A `ci:1|2:…` token
sees environments 1 and 2 and nothing else, everywhere it can read:

| Route shape | How the response is narrowed |
| --- | --- |
| Listings that name their environment — `GET /flags/values`, `GET /configs/values`, `GET /localization`, `GET /localization/lookup/…` | rows whose `environmentId` is outside the scope are dropped |
| `GET /environments` | narrowed on the rows' own `id` |
| `GET /inventory` | the handler filters and pages against the scope directly |
| `GET /audit` | filtered in SQL, `ENVIRONMENT_ID IN (…)` |
| Definitions that belong to no environment — `GET /flags`, `GET /flags/{id}`, `GET /microservices`, `GET /configs/{id}` | authenticated, not narrowed; a flag or microservice definition names no environment and reveals no environment's values |

Two consequences are deliberate and worth knowing before you read a response:

* **An out-of-scope row fetched by id returns `404`, not `403`.** A `403` would
  confirm the row exists, which is the one fact the scope is meant to withhold.
  A row you may not see is indistinguishable from a row that is not there.
* **A scoped caller can get back fewer than `?limit` rows on a page.** The
  domain handlers page in the database and the narrowing runs after, so pages
  are ragged rather than short-circuited. Keep paging until a page comes back
  empty; do not treat a short page as the end of the list.

`GET /audit` is narrowed the same way, and there the rule bites in a direction
worth stating outright: **a row carrying no environment is outside every
narrowed scope.** `NULL` never satisfies `IN`, so the 401 and 404 envelopes, and
the writes that belong to no environment at all, are invisible to a scoped
token. Only a full-scope token sees the whole trail. That is the conservative
answer — the recorded body is the one thing about such a row that is not already
knowable from the request — but it means a scoped operator cannot use `/audit`
to investigate probing against the API as a whole.

### Audit trail

Every mutating request (`POST`/`PUT`/`PATCH`/`DELETE`) **that is addressed at a
real route** is recorded in `CONFIG_AUDIT_LOG` (PostgreSQL DDL in
`migrations/008_config_audit_log.sql`, mirrored in
`internal/database/sqlite.go`). Reads are not audited.

A write matching no route is the exception, and it is not recorded at all: no
body buffering, no JSON walk, no insert. Such a request changes nothing, so the
row would say nothing — while the machinery to produce it is a megabyte of body
read and a synchronous insert per request, reachable without a credential. That
is a way to flood the audit table, exhaust the connection pool and bury the real
trail in noise. Path scanners are therefore visible in the access log and in
`centralconfig_http_requests_total{route="other"}`, not in the audit trail.

Columns: `OCCURRED_AT`, `ACTOR` (token name), `METHOD`, `PATH`, `TARGET_DOMAIN`,
`TARGET_ID`, `ENVIRONMENT_ID`, `STATUS_CODE`, `REMOTE_ADDR`, `REQUEST_BODY`.

* Recording wraps the whole mux, so a write that never reaches a handler is
  captured too — a 401 from an unknown token and a 403 from a dev token aimed at
  production both leave a row, as long as the path is one the API serves. That
  is the trail that shows someone probing with the wrong credential.
* `REQUEST_BODY` is stored redacted: any JSON field whose name *contains*, case
  insensitively, one of

  ```
  password  passwd  pwd  secret  token  apikey  api_key  credential
  privatekey  private_key  authorization  connectionstring
  connection_string  connstring  conn_string  dsn  signingkey
  signing_key  cert  pem  sessionid  session_id
  ```

  has its value replaced with `[redacted]`, at any nesting depth. The match is a
  substring, which is why both the camelCase and the snake_case spelling of a
  compound name is listed: `connectionString` contains `connectionstring` but
  `connection_string` does not. Bodies are truncated to 3800 bytes to fit the
  column, and a non-JSON body is dropped rather than stored verbatim.

  This stays a deny list rather than an allow list of storable fields, and that
  is a deliberate trade. The bodies recorded here are appsettings trees and
  translation bundles whose keys are defined by the consuming services, not by
  this schema; an allow list would have nothing to enumerate and would reduce
  every recorded body to its envelope — the part the other columns already
  carry. The trail exists to show what an operator actually sent. The cost is
  that a secret in a field named nothing like a secret is stored in full, which
  is one more reason for the `env:VAR_NAME` convention below.
* **An audit write never fails the request.** It runs after the change has been
  committed to the database and published to KV; failing then would report an
  error for a change that did happen. Failures are logged, the same philosophy
  as the KV publish path, which logs and lets the reconciler heal.

Read it back with `GET /audit?actor=&from=&to=&limit=&offset=`. `from`/`to`
accept `2026-01-01` or a full RFC 3339 instant; paging follows the same
`limit`/`offset` conventions as the other list endpoints. The listing is
narrowed to the caller's environment scope in SQL — see above for what that
hides.

### Request hardening

* **Body size** — every write handler decodes through `web.DecodeJSON`, which
  caps the body at 1 MiB via `http.MaxBytesReader`. The audit/scope middleware
  buffers the body under the same bound and puts it back, so the cap still
  applies downstream.
* **Rate limiting** — two token buckets over the write endpoints, standard
  library only. `WRITE_RATE_LIMIT_PER_MINUTE` (default 120, `0` disables the
  limiter entirely) sets both the sustained rate and the burst for each of them,
  and an authenticated write spends one token from each:

  * **At the edge, keyed by client address.** This one runs before
    authentication and before the audit middleware, because at that point
    nothing about the caller has been checked. Without it, an unauthenticated
    `POST` to any path buys a body read, a JSON walk and a synchronous database
    insert — a write primitive with no credential in front of it. It keys on
    `RemoteAddr` and deliberately not on a forwarded-for header: that header is
    caller input, and keying on it hands every request that invents a new value
    a fresh full bucket, which is no limit at all.

    **Behind a proxy or an ingress, `RemoteAddr` is the proxy.** The edge budget
    is then shared by everything arriving through it, so size
    `WRITE_RATE_LIMIT_PER_MINUTE` for the whole fleet of operators, pipelines
    and consoles that come in that way — not for one of them. Set it to one
    person's comfortable rate and a busy deploy window locks out everybody at
    once. The separation between callers in that topology comes from the second
    bucket, not this one.

  * **After authentication, keyed by the token name.** This is what stops one
    operator's script spending another's budget. The key is the configured
    label rather than the secret, so nothing sensitive ends up in a long-lived
    map.

  Exhausted callers get `429` with a `Retry-After` header giving whole seconds.
  Only writes are limited; reads answer from a bounded page and change nothing.
  The bucket map is swept for idle entries once a minute and capped at 8192
  keys, past which new callers share a single overflow bucket — so the
  limiter's memory is bounded by configuration rather than by how many distinct
  addresses somebody can present.
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

### TLS and response headers

Set `TLS_CERT_FILE` and `TLS_KEY_FILE` and the admin API serves HTTPS with a
**TLS 1.3 floor**, so a downgrade cannot get an admin token onto a legacy
cipher. Everything that talks to this API is either a browser or a Go client —
the console proxy, the `configclient` HTTP fallback, Prometheus — and all of
them have spoken 1.3 for years, so nothing in the stack needs 1.2 kept open.

Leave the two unset and it serves plain HTTP — which is what the compose stack
and the local two-terminal flow do — with a startup warning that admin tokens
are crossing the wire in clear text. In any shared environment, terminate TLS
here or at a proxy that is not reachable except over TLS: the bearer token in
the `Authorization` header is a production-change credential.

Two headers go out regardless of route, from the one middleware every response
passes through:

* `X-Content-Type-Options: nosniff`, always.
* `Strict-Transport-Security: max-age=31536000; includeSubDomains`, but only on
  a connection that is really TLS. Over plain HTTP the header means nothing, and
  a deployment terminating TLS at an ingress sets it there — where the
  certificate and the hostname actually live, and where it can be set for the
  whole domain rather than for this one service.

## The bundled test console is never safe to expose

`cmd/testconsole` plays a consuming microservice and serves a browser UI that
shows a live cache updating. To let the browser make admin writes without ever
holding a credential, it proxies them and attaches an admin token itself — the
compose stack gives it a full-scope one. **The console is therefore exactly as
powerful as that token, with no authentication of its own.** Anyone who can
reach its port can change configuration for every service watching the fleet.

It is built to be hard to expose by accident, not to be safe when exposed:

* It binds to `127.0.0.1` by default. `PORT` is a bare port number and
  `BIND_ADDR` supplies the host, so widening it is an explicit act; a value
  containing a colon is honoured verbatim, which is how the compose stack asks
  for `:8090`. Binding to anything that is not loopback logs a boxed warning at
  startup.
* It refuses cross-origin requests. A browser sends `Origin` on every
  cross-origin request and on same-origin writes, so a mismatch is the CSRF
  signal. Absence is allowed — a plain same-origin `GET` omits it, and so does
  `curl` — and one relaxation exists: a loopback page talking to a loopback
  console, which is what `npm run dev` on `:5173` needs. Bind it anywhere else
  and the comparison is exact.
* It forwards `application/json` only. A cross-origin form post can declare
  only urlencoded, multipart or `text/plain`, none of which get through — the
  second half of the CSRF defence, independent of the `Origin` header being
  present. The upstream `Content-Type` is passed through rather than rewritten,
  because rewriting it is what would turn such a post into an authenticated
  admin write.
* It forwards only an explicit allowlist of central-config path prefixes, after
  cleaning the path, so a traversal attempt is judged as the path it resolves
  to and nothing outside `/api/admin` reaches upstream wearing the token.
* Request bodies are capped at 1 MiB.

None of that makes it a supported deployment target, and it is not covered by
the threat model above. It is a development tool: run it on the machine you are
sitting at, against a stack you do not mind changing. It is not part of the
service binary, and `deploy/k8s/` does not deploy it.

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
