# central-config — Production Readiness & Gap Analysis

**Purpose:** an honest picture of what exists, what is still missing, and what
each gap would cost you in production.
**Scope:** three config domains — **feature flags**, **appsettings**, and
**localization** — plus the JetStream KV distribution layer that carries them to
consumers.
**Last updated:** 2026-08-06

---

## 1. TL;DR

The full path works end to end: admin write → database → KV write-through →
consumer memory, with a reconciler healing drift and a consumer library shipped.
All three domains are implemented, and the whole path is covered by tests that
run a real embedded JetStream server.

What is genuinely not done, in the order it would hurt you: **the Oracle
repository code paths have never executed against a real Oracle instance**;
there is **no migration runner**, and the migration files are not in dependency
order; and the schema is **defined twice**, once in `migrations/` and once by
hand in the SQLite backend. The dual write is also still non-transactional,
which is a deliberate choice rather than an oversight — §3.2 says why.

None of those stop the service working, and none are painful while you are
evaluating it on a laptop or in staging. §3 says what each one costs and what
the fix is.

---

## 2. What exists

### 2.1 Domains

| Domain | Package | Status |
|---|---|---|
| Feature flags | `internal/flagsconfig` | CRUD over HTTP, Oracle-backed, KV write-through |
| Appsettings | `internal/microconfig` | CRUD over HTTP, Oracle-backed, KV write-through |
| Localization | `internal/localization` | CRUD over HTTP, Oracle-backed, KV write-through |

Updates run the same validation and referential checks as creates. That matters
because an update rewrites the natural key: without those checks a `PUT` could
point a row at a microservice or environment that does not exist, or collide
with another row and surface as a `500` instead of a `409`. An update that moves
a row to a different `(environment, microservice, locale)` also purges the KV
key it moved away from, so consumers of the old identity stop being served a
value no row backs.

### 2.2 Distribution

- `internal/messaging` — NATS client, idempotent bucket provisioning
  (`FLAGS` / `MICROCONFIG` / `LOCALIZATION`, history depth 5), key builders,
  publisher (plus a no-op for tests and for running with distribution off),
  and the reconciler.
- Write-through is database first, KV second. A KV failure is logged and
  counted, not returned to the caller.
- The KV write is **revision-checked**: the publisher reads the current value,
  builds the new one from it, and conditions the write on the revision it read.
  A plain `Put` would let two admins updating the same key leave KV holding the
  older of the two writes while the database holds the newer, with both requests
  answering `200`. A conflict is re-resolved against whatever KV actually ended
  up with, three attempts before it is left to the reconciler.
- A byte-identical value is skipped rather than written, so a full sweep does
  not fan a no-op update out to the whole fleet or burn the bucket's history
  window on identical revisions.
- Buckets carry an explicit **512 KiB value ceiling**, and the appsettings and
  localization services refuse anything larger *before* the database write, with
  a `400`. Both numbers are the same constant. Left unset a bucket inherits the
  server's `max_payload`, which is how a payload accepted with a `201` becomes
  drift the reconciler can never heal — every sweep failing on the same row.
- The reconciler does an incremental sweep by `UPDATED_AT` window each cycle and
  a full sweep every 12 cycles, republishing missing or stale keys and pruning
  keys whose rows are gone. It is fault-tolerant per key: a row that will not
  publish is recorded on the result and the sweep carries on, so one bad row no
  longer strands every row behind it — which previously also suppressed pruning
  and stalled the incremental window, repeating the same failure every cycle.
- Pruning is guarded twice. It compares against a **snapshot of the bucket taken
  before the sweep read the database**, and only deletes a key still sitting at
  the revision it had then, so a key written mid-sweep is kept rather than
  purged — which also stops two replicas each running a reconciler from pruning
  each other's writes. And it **refuses to prune beyond a bounded fraction of a
  bucket** (`RECONCILE_PRUNE_MAX_FRACTION`, default `0.2`, with an absolute
  floor of 5 keys); a sweep that read no rows at all prunes nothing. A refusal
  increments `centralconfig_reconcile_prune_refused_total` and logs at error.
- `pkg/configclient` — consumer library: warm-up, watch, in-memory cache,
  optional one-shot HTTP hydration for a cold start with NATS unreachable
  (`httpfallback.go`, off by default, never on the read path). Scoping via
  `Options.MicroserviceID` so a consumer watches only its own keys.

### 2.3 Hardening in place

| Area | State |
|---|---|
| Auth | Bearer tokens on **every route except `/health`, `/livez` and `/metrics`** — reads as well as writes. Named tokens with per-environment scope (`ADMIN_TOKENS`); a single shared `ADMIN_TOKEN` is also accepted, as a full-scope actor named `shared`. A malformed token configuration is a fatal startup error. Fail-closed when configured, loud warning when not. |
| Read scoping | A token's scope narrows what it can see, not just what it can write: listings, `/inventory` and `/audit` are filtered to its environments, and an out-of-scope row fetched by id answers `404` rather than `403`. |
| Rate limiting | Two write buckets, `WRITE_RATE_LIMIT_PER_MINUTE` (default 120/min): address-keyed at the edge ahead of the token check and the audit insert, credential-keyed after it. |
| TLS | In-process HTTPS with a **TLS 1.3 floor** when `TLS_CERT_FILE` and `TLS_KEY_FILE` are both set; plain HTTP with a startup warning otherwise. `X-Content-Type-Options: nosniff` on every response, HSTS on a real TLS connection. |
| Audit | Every routed write recorded with actor, method, path, target, status, remote address, and a redacted request body (`CONFIG_AUDIT_LOG`, `GET /audit`, token required). Writes to unrouted paths are not recorded, and not buffered. |
| Observability | ECS JSON logs; Prometheus `/metrics` covering publish attempts/success/failure/skips, reconcile cycles, keys republished, pruned and prune refusals, HTTP requests and panics, database and NATS reachability. |
| Probes | `/livez` is static process liveness; `/health` is readiness and reports the database and NATS, returning `503` when either is down. |
| Database | Connection pool bounded and configurable (`DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`, `DB_CONN_MAX_LIFETIME`, `DB_CONN_MAX_IDLE_TIME`); CLOB columns bound as CLOBs; unique-constraint violations mapped to `409` rather than `500`. |
| Server | Read/read-header/write/idle timeouts set; graceful shutdown waits for the running reconcile cycle, drains NATS, then closes the database. |
| Packaging | Multi-stage distroless Dockerfile, non-root, one target per binary. k8s manifests and a deploy guide in `deploy/k8s/` and `DEPLOY_JETSTREAM_K8S.md`. |
| Tests | Keys, reconciler (partial sweeps, prune, prune refusal, mid-sweep writes), flag write-through, auth middleware, read scoping, rate limit, audit store and redaction, liveness, observability, configclient scoping and integration, and an end-to-end stack test running embedded JetStream. |

### 2.4 A NATS outage no longer stops the service

The service boots and serves reads with NATS down. Bucket provisioning failing
at startup is logged and the process carries on with unprovisioned handles;
every publish then fails cleanly while the database-backed read paths keep
working, and the reconciler re-provisions on its next cycle. This is deliberate:
the read paths need nothing from KV, and turning a flag off is exactly what an
operator reaches for while the distribution plane is down. Crash-looping every
pod removes that option at the worst moment — which is also why liveness points
at `/livez` and only readiness at `/health`.

---

## 3. Open gaps

### 3.1 The Oracle repositories have never run against Oracle

This is the largest gap in the repository and it has not moved.

The Oracle SQL is **compile-verified only**. Every test — end to end, repository,
reconciler — runs against the SQLite backend, whose schema mirrors the Oracle one
but whose SQL differs in bind syntax, pagination clause and type handling. Two
recent pieces of work are the sharpest examples, because both exist *only* on
the Oracle path and neither has ever executed:

- **CLOB binding.** `SETTINGS_JSON` and `BUNDLE_JSON` are CLOBs, and go-ora
  sends a plain Go string as `VARCHAR2`, which an ordinary appsettings tree
  overflows (ORA-01461/ORA-01704) on any database without
  `MAX_STRING_SIZE=EXTENDED`. The bind now goes through `database.CLOB`. Nothing
  has confirmed that against a real driver.
- **The timezone-safe reconcile window.** The incremental sweep binds a UTC
  instant and converts it into the session time zone
  (`CAST(FROM_TZ(CAST(:1 AS TIMESTAMP), 'UTC') AT TIME ZONE SESSIONTIMEZONE AS TIMESTAMP)`)
  so that comparing it against an `UPDATED_AT` written by `CURRENT_TIMESTAMP` is
  meaningful. On a non-UTC session the old comparison was silently offset by the
  session's whole UTC offset. The new expression is untested against Oracle.

The reconciler's `ListAllForReconcile` queries are the riskiest place for this,
because a failure there is silent: a broken query means drift rather than a
failed request.

**Cost:** a syntax, bind or type error in an Oracle-only path surfaces at
runtime in production, and for the reconciler it surfaces as configuration that
quietly stops converging.
**Fix:** repository tests against a real or containerised Oracle in CI. Until
then, treat the first deployment against a real Oracle instance as the test, and
watch `centralconfig_reconcile_last_success_timestamp_seconds` while you do it.

### 3.2 The dual write is not transactional

The database is committed, then KV is written. A crash in between leaves KV
stale.

**Cost:** bounded staleness — the reconciler republishes on its next sweep, so
the exposure is one `RECONCILE_INTERVAL` (default 5m), not permanent.
**Fix:** a transactional outbox — write the KV intent into a table in the same
transaction, publish from a relay. Deliberately deferred: the reconciler already
bounds the exposure, and the outbox adds a table, a relay loop and its own
failure modes. Decide this explicitly rather than inheriting it.

### 3.3 KV has no per-key access control

Buckets are readable in full by anyone holding NATS credentials for the account.
The environment prefix scopes what a consumer *watches*; it is not an
authorisation boundary. Note that this is now the weaker half of the model: the
admin API scopes reads per token, and the data plane does not.

**Cost:** in the default single-credential deployment, any consumer that holds
NATS credentials can read every environment's configuration, and every consumer
caches what it reads in plaintext memory.
**Mitigation in place:** the `env:VAR_NAME` marker convention keeps secret values
out of KV entirely — see [`docs/SECURITY.md`](SECURITY.md). That convention is
the control; nothing
enforces it, so a reviewer has to.
**Fix if stronger isolation is needed:** per-service, per-environment NATS
credentials scoped to the key layout, or separate accounts per environment each
with its own buckets and credentials. [`docs/SECURITY.md`](SECURITY.md) §2 spells
out the permission model, and
[`DEPLOY_JETSTREAM_K8S.md`](DEPLOY_JETSTREAM_K8S.md) §4 is where you apply it.

### 3.4 No migration runner, and the migrations are not in dependency order

`migrations/` is DDL, applied by hand or by whatever migration tool you already
run. Nothing records which files have been applied, so nothing stops a partial
or repeated application, and there is no down path.

Worse, **the file numbers are not the apply order**. `001_config_localization.sql`
has foreign keys into `CONFIG_MICROSERVICES` and `CONFIG_ENVIRONMENTS`, which are
created by `003` and `002`. On a fresh database you must apply 002 and 003 before
001. That is recorded only in a comment inside `002_config_environments.sql`.

**Cost:** the first `sqlplus @001` on a new database fails on a missing table,
and the operator has to work out why. Any tool that applies the directory in
lexical order fails the same way.
**Fix:** renumber so the order is the dependency order, and adopt a runner that
tracks applied versions. Renumbering is only safe while no deployment has
applied the current names, so it gets harder from here.

### 3.5 The schema is defined twice

`migrations/*.sql` is the Oracle DDL. `internal/database/sqlite.go` declares the
same tables again, by hand, for the local stack. They are kept in step by
nothing but attention.

**Cost:** a column added to one and not the other produces a local stack that
disagrees with production. Because every test runs on SQLite, the divergence
shows up as tests that pass against a schema production does not have — the
failure mode is a green build, which is the expensive kind.
**Fix:** generate one from the other, or drive the SQLite stack from the same
DDL with a translation step. Neither is free; in the meantime, treat a
`migrations/` change and a `sqlite.go` change as a single unit of work and say
so in the pull request.

### 3.6 No CI

Build, vet, and test are run by hand. Nothing gates a merge. There is no
`.github/workflows/` directory in the repository as it stands.

**Cost:** every gap above is one an unreviewed commit can widen silently, and
§3.1 in particular cannot be closed without somewhere to run an Oracle
container.
**Fix:** a workflow running `go build ./...`, `go vet ./...`, `go test ./...`
and the web UI's `npm ci && npm run lint && npm test`, on pull requests to
`master`.

### 3.7 Clock skew in the incremental reconcile

Distinct from §3.1's timezone bug, and not fixed by it. Between full sweeps the
reconciler selects rows by `UPDATED_AT` against *this process's* clock, not the
database's. A large skew between the two can push a recently changed row outside
the window.

**Cost:** a change missed until the next full sweep (12 cycles).
**Mitigation in place:** a one-minute overlap on each window absorbs small skew,
the window only advances after a clean cycle, and the periodic full sweep bounds
the damage.

### 3.8 Smaller items

- No alert rules shipped with the metrics; `OBSERVABILITY.md` §3 says what to
  alert on but the rules live wherever Prometheus is configured.
- `centralconfig_db_up` only refreshes when `/health` is called. In Kubernetes
  that is the readiness probe, so it stays current; anywhere nothing probes the
  service, it does not.
- The prune ceiling protects against deleting too much of a bucket, not against
  deleting the wrong key within the allowance. The revision check is what covers
  that, and it only covers keys the sweep actually saw.

---

## 4. Checklist for operators

If you are deciding whether to run this, the split below is the useful one:
what the repository already gives you, and what it deliberately leaves to your
environment.

**Already here, nothing for you to do:**

- All three domains read and write over HTTP against the source of truth.
- Every write propagates to KV, and the reconciler heals drift — per key, with
  a bounded prune.
- `configclient` is scoped per service and covered by an end-to-end test.
- Bearer auth on reads and writes, per-environment token scoping, write rate
  limiting, and an audit trail with redacted bodies.
- TLS in-process at a 1.3 floor, or terminated at an ingress if you prefer.
- Separate liveness and readiness probes, so a NATS outage costs you traffic
  rather than every pod.
- Metrics that report distribution status, not just process liveness.
- The container image builds and runs non-root.

**Yours to add before it carries traffic you care about:**

- Repository tests against your own Oracle instance (§3.1). The SQL is
  compile-verified only, and that is the gap most likely to bite first.
- A migration order you can apply unattended (§3.4), and a habit of changing
  `migrations/` and `internal/database/sqlite.go` together (§3.5).
- A CI gate on build, vet and test (§3.6).
- NATS authentication and per-service credentials — see
  [`docs/SECURITY.md`](SECURITY.md). The
  shipped compose stack has neither, on purpose.
- Alert rules for the metrics; `OBSERVABILITY.md` §3 says which are worth having
  and why.
- A decision on the dual write (§3.2): accept the bounded staleness the
  reconciler gives you, or build the transactional outbox. Either is defensible;
  drifting into one by accident is not.
