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

The gap that led this document for the project's whole life — the production
storage path being compile-verified only — is **closed**. The PostgreSQL
repositories and the audit store now run against a live server on every CI run
(§3.1, §3.6). What replaces it is smaller and honest: that coverage stops at the
repository layer, and **PostgreSQL is newly adopted here — no deployment has yet
run this schema in anger.** An integration suite is evidence, not production.

The rest, in the order it would hurt you: there is **no migration runner**, so
applying `migrations/` is yours to do and to track (§3.4); the schema is
**defined twice**, once in `migrations/` and once by hand in the SQLite backend,
and a *diverging column* between them is still invisible (§3.5). The dual write
is also still non-transactional, which is a deliberate choice rather than an
oversight — §3.2 says why.

None of those stop the service working, and none are painful while you are
evaluating it on a laptop or in staging. §3 says what each one costs and what
the fix is.

---

## 2. What exists

### 2.1 Domains

| Domain | Package | Status |
|---|---|---|
| Feature flags | `internal/flagsconfig` | CRUD over HTTP, PostgreSQL-backed, KV write-through |
| Appsettings | `internal/microconfig` | CRUD over HTTP, PostgreSQL-backed, KV write-through |
| Localization | `internal/localization` | CRUD over HTTP, PostgreSQL-backed, KV write-through |

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
| Database | PostgreSQL through `jackc/pgx/v5` over `database/sql`. Connection pool bounded and configurable (`DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`, `DB_CONN_MAX_LIFETIME`, `DB_CONN_MAX_IDLE_TIME`) — a bound that matters more against Postgres, where every connection is a backend process. Documents are `TEXT` and bind as plain Go strings, deliberately not `jsonb`: `jsonb` re-emits with its own key order, which would defeat the publisher's byte-identical skip and republish every tree to the fleet on every sweep. `UPDATED_AT` is `TIMESTAMPTZ`, so the reconcile window is a plain instant comparison. Unique-constraint violations (SQLSTATE `23505`) mapped to `409` rather than `500`. |
| Server | Read/read-header/write/idle timeouts set; graceful shutdown waits for the running reconcile cycle, drains NATS, then closes the database. |
| Packaging | Multi-stage distroless Dockerfile, non-root, one target per binary. k8s manifests and a deploy guide in `deploy/k8s/` and `DEPLOY_JETSTREAM_K8S.md`. |
| Tests | Keys, reconciler (partial sweeps, prune, prune refusal, mid-sweep writes), flag write-through, auth middleware, read scoping, rate limit, audit store and redaction, liveness, observability, configclient scoping and integration, and an end-to-end stack test running embedded JetStream — all on SQLite. Plus `internal/pgintegration`: 25 tests running the real repositories and the audit store against a live PostgreSQL, each in a schema of its own that the test migrates and seeds itself. |

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

### 3.1 Only the repository layer runs against PostgreSQL

**The gap this section used to describe is closed, not narrowed.** For the whole
of the project's life until now, the production storage path was compile-verified
only: every test ran against SQLite, whose SQL differs from the real backend in
bind syntax, pagination clause and type handling. `internal/pgintegration` now
runs the real repositories and the audit store against a live PostgreSQL, and
CI runs it on every push and pull request (§3.6).

Both of the examples this section used to lead with are gone rather than
verified, which is the better outcome:

- **CLOB binding no longer exists.** `SETTINGS_JSON` and `BUNDLE_JSON` are
  `TEXT`, and Postgres takes a plain Go string straight into them. There is no
  wrapper type to get wrong, so `database.CLOB` was deleted rather than tested.
  What is tested is the property that mattered underneath it: a large document
  round-trips **byte-identically**, which is what the publisher's skip depends
  on.
- **The timezone-safe reconcile window is now a plain comparison.**
  `UPDATED_AT` is `TIMESTAMPTZ`, so the incremental sweep is
  `WHERE UPDATED_AT >= $1` against a bound instant, and the
  `FROM_TZ … SESSIONTIMEZONE` conversion the old column needed is deleted. That
  removed a class of bug rather than an instance of one, and it is pinned:
  `reconcile_timezone_test.go` runs the window on sessions at UTC+14
  (`Pacific/Kiritimati`) and UTC−11 (`Pacific/Pago_Pago`) as well as UTC,
  because those are the two signs the old error came in and a UTC-only CI runner
  would have gone green against the broken column too.

**What remains open is the boundary of that coverage.** The suite reaches the
repositories and the audit store and stops there. `internal/app`'s end-to-end
and handler tests — routing, token scoping, the write-through publish, the
reconciler driving real sources — still run on SQLite alone. So the SQL is
exercised against the real backend; the wiring above it is exercised against the
mirror schema.

And the coverage is not deployment experience. PostgreSQL is newly adopted in
this repository. No production instance has yet carried this schema, and the
first one to do so will find whatever an integration suite on a throwaway
container cannot: connection handling through a real pooler, a DBA's session
settings, `max_connections` under real replica counts.

**Cost:** a defect in the layer above the repositories, or in an interaction the
suite does not stage, still reaches a real deployment first. For the reconciler
that is the expensive direction, because a failure there is silent —
`ListAllForReconcile` returning nothing wrong-looking means drift rather than a
failed request.
**Fix:** run `internal/app`'s stack test against Postgres too — a larger job than
it sounds, because those tests hard-code `DBDriver: "sqlite"` and open the
database themselves rather than taking one they are handed.
Until then, treat the first real deployment as the remaining test, and watch
`centralconfig_reconcile_last_success_timestamp_seconds` while you do it.

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

### 3.4 No migration runner

`migrations/` is DDL, applied by hand or by whatever migration tool you already
run. Nothing records which files have been applied, so nothing stops a partial
or repeated application, and there is no down path.

The dependency-order half of this gap is closed. **The file numbers are now the
apply order** — `CONFIG_ENVIRONMENTS` is `001`, `CONFIG_MICROSERVICES` is `002`,
localization is `003` — so `psql -f` over the directory in lexical order works on
an empty database. The renumbering happened during the port to Postgres, which
was the last moment it was free: no deployment had applied the old names. And it
is not merely asserted — `internal/pgintegration` applies `migrations/*.sql` in
lexical order for *every one of its tests*, so a file that no longer applies, or
one numbered ahead of something it references, is a red build rather than a
discovery made during a deploy window.

**Cost:** what is left is bookkeeping. Nobody can tell you what a given database
has had applied, an interrupted apply leaves no record of where it stopped, and
rolling one back is a hand-written `DROP`.
**Fix:** adopt a runner that tracks applied versions. The numbering it would
need is already in place, so this is now a small job rather than a renumbering
exercise.

### 3.5 The schema is defined twice

`migrations/*.sql` is the PostgreSQL DDL. `internal/database/sqlite.go` declares
the same tables again, by hand, for the local stack. They are kept in step by
nothing but attention.

The failure mode is narrower than it was, and worth stating precisely, because
the half that remains is the half that hides. **A missing table is now caught.**
`internal/pgintegration` applies `migrations/` and then drives the real
repositories against it, so a table that exists only in `sqlite.go` fails there
immediately — and a table that exists only in `migrations/` has always failed
loudly in the SQLite tests. **A diverging column is not caught.** Nothing
compares the two declarations, so a column added to one and not the other, or
declared with a different type, width or nullability, still produces a local
stack that disagrees with production.

**Cost:** the tests that run on SQLite — which is still everything above the
repositories — pass against a schema production does not have. The failure mode
is a green build, which is the expensive kind.
**Fix:** generate one from the other, or drive the SQLite stack from the same
DDL with a translation step. Neither is free; a cheaper interim step is a test
that reads both schemas and compares them column by column. In the meantime,
treat a `migrations/` change and a `sqlite.go` change as a single unit of work
and say so in the pull request.

### 3.6 CI stands up a database — closed

[`.github/workflows/ci.yml`](../.github/workflows/ci.yml) gates every push and
pull request on `gofmt`, `go build`, `go vet`, `go test -race`, `go mod tidy`,
`golangci-lint`, and the web UI's lint, test and build. It **also** runs a
`postgres:17-alpine` service container and points `TEST_POSTGRES_DSN` at it, so
`internal/pgintegration` runs there instead of skipping the way it does on a
laptop with no server. The production SQL is now gated on every push.

The interesting part is the guard on top of it. The suite fails *open* by
design — with no database configured every test skips and the package reports
`ok`, which is what keeps `go test ./...` green for a contributor and would be
useless in a gate. A DSN typo, a renamed service or a container that never
became healthy would all leave the workflow green while testing nothing. So the
suite is run once more verbosely, `--- SKIP` is grepped for, and any skip fails
the step. That is the difference between "CI covers Postgres" being a fact and
being a claim, and it costs a few seconds.

**What this does not gate** is anything above the repository layer — see §3.1,
which is where the residual lives. The workflow still runs no NATS container:
the messaging tests start an embedded JetStream server in-process, which is
genuine coverage rather than a substitute for one.

### 3.7 Clock skew in the incremental reconcile

Distinct from the session-timezone bug §3.1 records as removed, and untouched by
removing it — `TIMESTAMPTZ` settles which *instant* a stored time means, not
whose clock produced it. Between full sweeps the
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
- The PostgreSQL repositories and the audit store are exercised against a real
  server on every CI run, and the migrations are applied — in the order their
  file numbers give — several dozen times while that happens.

**Yours to add before it carries traffic you care about:**

- A first rollout treated as the last test (§3.1). The SQL is exercised against
  a real PostgreSQL in CI, but no deployment has yet run this schema, and
  nothing above the repository layer is covered against Postgres at all. Watch
  `centralconfig_reconcile_last_success_timestamp_seconds` while you go.
- A way of applying `migrations/` and recording what you applied (§3.4). The
  file numbers are the apply order, so the directory in lexical order is
  correct on an empty database; there is no runner and no down path. Plus the
  habit of changing `migrations/` and `internal/database/sqlite.go` together,
  because a column that diverges between them is still invisible (§3.5).
- NATS authentication and per-service credentials — see
  [`docs/SECURITY.md`](SECURITY.md). The
  shipped compose stack has neither, on purpose.
- Alert rules for the metrics; `OBSERVABILITY.md` §3 says which are worth having
  and why.
- A decision on the dual write (§3.2): accept the bounded staleness the
  reconciler gives you, or build the transactional outbox. Either is defensible;
  drifting into one by accident is not.
