# central-config — Production Readiness & Gap Analysis

**Purpose:** an honest picture of what exists, what is still missing, and what
each gap would cost you in production.
**Scope:** three config domains — **feature flags**, **appsettings**, and
**localization** — plus the JetStream KV distribution layer that carries them to
consumers.
**Last updated:** 2026-08-05

---

## 1. TL;DR

The full path works end to end: admin write → Oracle → KV write-through →
consumer memory, with a reconciler healing drift and a consumer library shipped.
All three domains are implemented, and the whole path is covered by tests that
run a real embedded JetStream server.

What is genuinely not done: the Oracle repositories are exercised only by
compilation, the dual write is non-transactional, and there is no CI. None of
those stop the service working, and none of them are painful while you are
evaluating it on a laptop or a staging environment. All three are worth closing
before you put anything you care about behind it — §3 says what each one costs
and what the fix is.

---

## 2. What exists

### 2.1 Domains

| Domain | Package | Status |
|---|---|---|
| Feature flags | `internal/flagsconfig` | CRUD over HTTP, Oracle-backed, KV write-through |
| Appsettings | `internal/microconfig` | CRUD over HTTP, Oracle-backed, KV write-through |
| Localization | `internal/localization` | CRUD over HTTP, Oracle-backed, KV write-through |

### 2.2 Distribution

- `internal/messaging` — NATS client, idempotent bucket provisioning
  (`FLAGS` / `MICROCONFIG` / `LOCALIZATION`, history depth 5), key builders,
  publisher (plus a no-op for tests and for running with distribution off),
  and the reconciler.
- Write-through is Oracle first, KV second. A KV failure is logged and counted,
  not returned to the caller.
- The reconciler does an incremental sweep by `UPDATED_AT` window each cycle and
  a full sweep every 12 cycles, republishing missing or stale keys and pruning
  keys whose rows are gone.
- `pkg/configclient` — consumer library: warm-up, watch, in-memory cache,
  optional one-shot HTTP hydration for a cold start with NATS unreachable
  (`httpfallback.go`, off by default, never on the read path). Scoping via
  `Options.MicroserviceID` so a consumer watches only its own keys.

### 2.3 Hardening in place

| Area | State |
|---|---|
| Auth | Bearer tokens on every write. Named tokens with per-environment scope (`ADMIN_TOKENS`); a single shared `ADMIN_TOKEN` is also accepted, as a full-scope actor named `shared`. Fail-closed when configured, loud warning when not. |
| Rate limiting | Per-caller write limit, `WRITE_RATE_LIMIT_PER_MINUTE`, default 120/min. |
| TLS | In-process HTTPS when `TLS_CERT_FILE` and `TLS_KEY_FILE` are both set; plain HTTP with a startup warning otherwise. |
| Audit | Every write recorded with actor, method, path, target, status, remote address, and a redacted request body (`CONFIG_AUDIT_LOG`, `GET /audit`, token required). |
| Observability | ECS JSON logs; Prometheus `/metrics` covering publish attempts/success/failure/skips, reconcile cycles, keys republished and pruned, HTTP requests and panics, DB ping failures. `/health` reports NATS connectivity and returns 503 when it is down. |
| Server | Read/read-header/write/idle timeouts set; graceful shutdown drains NATS before closing the DB. |
| Packaging | Multi-stage distroless Dockerfile, non-root, one target per binary. k8s manifests and a deploy guide in `deploy/k8s/` and `docs/DEPLOY_JETSTREAM_K8S.md`. |
| Tests | Keys, reconciler (including prune), flag write-through, auth middleware, rate limit, audit store and redaction, observability, configclient scoping and integration, and an end-to-end stack test running embedded JetStream. |

---

## 3. Open gaps

### 3.1 Oracle repositories are untested

The Oracle SQL is compile-verified only. The end-to-end and repository tests run
against the SQLite backend, whose schema mirrors the Oracle one but whose SQL
differs in bind-parameter syntax. The reconciler's `ListAll*` queries are the
riskiest of these: they are only ever exercised on the SQLite path.

**Cost:** a syntax or bind error in an Oracle-only query surfaces at runtime,
and for the reconciler that means silent drift rather than a failed request.
**Fix:** repository tests against a real or containerised Oracle in CI.

### 3.2 The dual write is not transactional

Oracle is committed, then KV is written. A crash in between leaves KV stale.

**Cost:** bounded staleness — the reconciler republishes on its next sweep, so
the exposure is one `RECONCILE_INTERVAL` (default 5m), not permanent.
**Fix:** a transactional outbox — write the KV intent into an Oracle table in
the same transaction, publish from a relay. Deliberately deferred: the
reconciler already bounds the exposure, and the outbox adds a table, a relay
loop and its own failure modes.

### 3.3 KV has no per-key access control

Buckets are readable in full by anyone holding NATS credentials for the account.
The environment prefix scopes what a consumer *watches*; it is not an authorisation
boundary.

**Cost:** in the default single-credential deployment, any consumer that holds
NATS credentials can read every environment's configuration, and every consumer
caches what it reads in plaintext memory. Any deployment larger than a single
machine has to narrow that.
**Mitigation in place:** the `env:VAR_NAME` marker convention keeps secret values
out of KV entirely — see `SECURITY.md`. That convention is the control; nothing
enforces it, so a reviewer has to.
**Fix if stronger isolation is needed:** per-service, per-environment NATS
credentials scoped to the key layout, or separate accounts per environment each
with its own buckets and credentials. `SECURITY.md` spells out the permission
model.

### 3.4 Clock skew in the incremental reconcile

Between full sweeps the reconciler selects rows by `UPDATED_AT` against this
process's clock, not the database's. A large skew can push a recently changed
row outside the window.

**Cost:** a change missed until the next full sweep (12 cycles).
**Mitigation in place:** a one-minute overlap on each window absorbs small skew,
and the periodic full sweep bounds the damage.

### 3.5 No CI

Build, vet, and test are run by hand. Nothing gates a merge.

### 3.6 Smaller items

- No alert rules shipped with the metrics; `OBSERVABILITY.md` says what to alert
  on but the rules live wherever Prometheus is configured.
- Localization has no size guard on a bundle. A very large bundle is accepted by
  the API and then rejected by KV's value limit at publish time, which surfaces
  as a publish failure rather than a 400.
- No migration runner: `migrations/` is DDL, applied by hand or by whatever
  migration tool you already run.

---

## 4. Checklist for operators

If you are deciding whether to run this, the split below is the useful one:
what the repository already gives you, and what it deliberately leaves to your
environment.

**Already here, nothing for you to do:**

- All three domains read and write over HTTP against the source of truth.
- Every write propagates to KV, and the reconciler heals drift.
- `configclient` is scoped per service and covered by an end-to-end test.
- Bearer auth with per-environment token scoping, write rate limiting, and an
  audit trail with redacted bodies.
- TLS in-process, or terminated at an ingress if you prefer.
- Metrics and `/health` report distribution status, not just process liveness.
- The container image builds and runs non-root.

**Yours to add before it carries traffic you care about:**

- Repository tests against your own Oracle instance (§3.1). The SQL is
  compile-verified only.
- A CI gate on build, vet and test (§3.5).
- NATS authentication and per-service credentials — see `SECURITY.md`. The
  shipped compose stack has neither, on purpose.
- Alert rules for the metrics; `OBSERVABILITY.md` §3 says which three are worth
  having and why.
- A decision on the dual write (§3.2): accept the bounded staleness the
  reconciler gives you, or build the transactional outbox. Either is defensible;
  drifting into one by accident is not.
