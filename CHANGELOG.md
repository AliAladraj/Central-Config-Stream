# Changelog

What changed, stated as what it does for whoever runs this. A control plane has
an indirect readership — a consumer never calls this service, it watches the KV
keys this service writes — so the things worth recording here are the ones a
downstream process can feel: a value shape, a token's reach, a key layout, a
guarantee about what happens when a dependency is down. Commit subjects are in
`git log` and belong there.

The format is [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the
versions are [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Where
something is implemented but unverified, this file says so, in the same register
as [`docs/PRODUCTION_READINESS.md`](docs/PRODUCTION_READINESS.md) — a changelog
that only lists wins is a changelog you have to check the code against anyway.

## [Unreleased]

Nothing yet. 0.1.0 has not been tagged, so everything currently in the
repository is listed under it.

## [0.1.0] — UNRELEASED — tag pending

The first release. There is no earlier version to have fixed anything from, so
there is no **Fixed** section: the hardening that happened during development —
authenticating reads, making reconciliation fault-tolerant, splitting liveness
from readiness — is part of what 0.1.0 *is*, not a change to something anyone
ran. It is described below as the shipped behaviour rather than as a repair.

### Added

- **Three configuration domains over one admin API.** Feature flags, per-service
  appsettings and localization bundles, each with full CRUD over HTTP and a
  relational database as the source of truth. Updates run the same validation
  and referential checks as creates, because an update rewrites the natural key:
  without them a `PUT` could point a row at an environment that does not exist,
  or collide with another row and surface as a `500` rather than a `409`.

- **Write-through distribution over NATS JetStream KV.** Every admin write also
  lands in one of three buckets — `FLAGS`, `MICROCONFIG`, `LOCALIZATION` — under
  environment-scoped keys, so a consumer watches one prefix and receives only
  what belongs to its environment. Buckets keep five historical values per key,
  which is the rollback depth. Consumers never call this service on the request
  path and never query the database; a config read in a consumer is a field
  access.

- **`pkg/configclient`, a Go consumer library.** `New` blocks until the initial
  values for the client's scope are loaded, so a service cannot begin serving on
  an empty cache — that is how a "feature disabled everywhere" incident happens.
  `Options.MicroserviceID` narrows the watch to one service's own keys; without
  it the process caches the whole fleet's configuration and `Status().FleetWide`
  reports that it did. `Options.HTTPFallback` is an opt-in cold-start path that
  hydrates over the admin API when JetStream is unreachable at boot; it runs
  only from `New`, never from a read.

- **A reconciler that bounds drift rather than assuming it away.** The dual
  write is not transactional — the database is committed first and a KV failure
  is logged, counted and not returned to the caller — so an in-process
  reconciler sweeps the database against KV on `RECONCILE_INTERVAL` (default
  5m), republishing anything missing or stale and pruning keys whose rows are
  gone. Incremental by `UPDATED_AT` each cycle, full every twelfth. It is
  fault-tolerant per key: a row that will not publish is recorded and the sweep
  carries on, so one bad row cannot strand every row behind it.

- **Two guards on pruning**, because the failure mode is deleting the fleet's
  configuration. A prune compares against a snapshot of the bucket taken before
  the sweep read the database and only removes a key still at the revision it
  had then, so a key written mid-sweep survives and two replicas each running a
  reconciler do not purge each other's writes. And no cycle may prune beyond
  `RECONCILE_PRUNE_MAX_FRACTION` of a bucket (default `0.2`, absolute floor 5
  keys); a sweep that read no rows at all prunes nothing. A refusal is logged at
  error and counted.

- **Revision-checked KV writes.** The publisher reads the current value, builds
  the new one from it and conditions the write on the revision it read, three
  attempts before the row is left to the reconciler. A plain `Put` would let two
  admins updating the same key leave KV holding the older of the two writes
  while the database holds the newer, both requests answering `200`. A
  byte-identical value is skipped rather than written, so a full sweep does not
  fan a no-op out to the fleet or burn the bucket's history window.

- **A 512 KiB value ceiling, enforced before the database write.** The buckets
  carry it explicitly and the appsettings and localization services refuse
  anything larger with a `400`. Both numbers are the same constant. Left to
  JetStream, an oversized payload is accepted with a `201` and then becomes
  drift no sweep can ever heal.

- **Optimistic concurrency on every `PUT`.** An optional `expectedUpdatedAt`
  applies the write only while the stored row still carries that timestamp, and
  answers `409` otherwise. Absent, the original last-write-wins behaviour
  stands, so nothing existing had to change to adopt it. Implemented and tested
  in all three domains against both repository implementations.

- **An audit trail for writes.** Every routed write records the actor, method,
  path, target, status, remote address and a redacted request body, readable at
  `GET /audit` with a token. Writes to unrouted paths are not recorded and their
  bodies are not buffered.

- **Liveness and readiness as separate questions.** `GET /livez` is static and
  touches no dependency; `GET /health` pings the database and reports NATS,
  answering `503` when either is down. The service boots and serves reads with
  NATS unreachable, retrying bucket provisioning on each reconcile cycle,
  because turning a flag off is exactly what an operator reaches for while the
  distribution plane is down — and crash-looping every pod removes that option
  at the worst moment.

- **Prometheus metrics and ECS-shaped JSON logs.** `/metrics` covers publish
  attempts, successes, failures and skips; reconcile cycles, duration, keys
  republished and pruned, prune refusals and last success timestamp; HTTP
  requests, duration and panics; and database and NATS reachability.
  `centralconfig_reconcile_last_success_timestamp_seconds` is the one to watch
  on a first deployment.

- **A bounded database session pool** (`DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`,
  `DB_CONN_MAX_LIFETIME`, `DB_CONN_MAX_IDLE_TIME`), CLOB columns bound as CLOBs
  rather than as strings, a timezone-safe reconcile window, and
  unique-constraint violations mapped to `409` instead of `500`. **The Oracle
  paths are compile-verified only** — every test in this repository runs against
  the SQLite backend, so none of the four has ever executed against a real
  driver. See [`docs/PRODUCTION_READINESS.md`](docs/PRODUCTION_READINESS.md)
  §3.1.

- **A local stack that exercises the whole path on one machine.** A compose
  file, a distroless multi-stage image (non-root, one target per binary) and
  Kubernetes manifests, plus `DB_DRIVER=sqlite` and an embedded JetStream server
  so the write → database → KV → consumer path runs with no Oracle and no NATS
  installation.

- **A React console that plays a consuming microservice.** Seven views over a
  live `configclient` cache, an environment switcher, SSE push of every KV
  update the consumer receives, a drift panel that compares the database against
  what a consumer is actually serving, and a measured propagation latency. It is
  the only place anything checks the two planes against each other. It proxies
  its admin calls so the browser never holds a token, binds to `127.0.0.1` by
  default and warns loudly when it does not.

- **`--version` on both binaries.** `central-config --version` and
  `testconsole --version` print the version, the short commit and the UTC build
  date, stamped at link time by the Makefile and threaded through the Dockerfile
  as build arguments that are repeated as OCI image labels. Answered before any
  configuration is read and before anything is dialled, so it works on a machine
  that can reach neither the database nor NATS — which is where somebody
  untangling a rollout usually is. An unstamped `go build` reports `dev`, so a
  hand-built binary never claims a version it does not have.

- **An OpenAPI 3.1 description of the admin API**
  ([`docs/openapi.yaml`](docs/openapi.yaml)), written from the handlers rather
  than from the prose, so it records the quirks as implemented: `enabled` is an
  integer here and a boolean in KV, updates carry their id in the body rather
  than the path, and a read outside a token's scope answers `404`.

- **A CI gate** on `gofmt`, build, vet, race tests, module tidiness,
  `golangci-lint` and the web UI's lint, test and build — deliberately one job,
  so a contributor sees a single tick rather than six to correlate. It stands up
  no database: the Oracle path is the one path no gate exercises
  ([`docs/PRODUCTION_READINESS.md`](docs/PRODUCTION_READINESS.md) §3.6).

- **Documentation for each audience**: a compose walkthrough, a consumer
  contract specified well enough to implement a client in any language, a
  security model, a Kubernetes deployment guide, an observability guide, a
  contributing guide listing the ~45 sites a new config domain touches, and a
  production-readiness document that leads with what is missing.

- **Community health files**: a code of conduct routed to the private advisory
  channel (there is no shared inbox behind this project), issue forms that ask
  for what actually narrows a bug here — the commit, the driver, the NATS
  version, and above all whether a value reached the database but not the key
  store — a pull-request template, and `CODEOWNERS` so review is requested
  rather than remembered.

- **Handler and repository test coverage for appsettings and localization**,
  which previously had service-level tests only. That left the create/update
  split, the id-in-body update shape, the object-only rule for `settingsJson`
  and `bundleJson`, the value ceiling and SQLite paging unexercised.

### Security

- **Reads are authenticated.** Every route except `GET /health`, `GET /livez`
  and `GET /metrics` requires a bearer token. Reads were anonymous during
  development; a single unauthenticated `GET` was enough to walk off with every
  appsettings tree and bundle in the estate.

- **Token scope narrows reads as well as writes.** `ADMIN_TOKENS` gives a token
  a set of environments, and that scope applies in both directions: writes
  elsewhere answer `403`, listings and `GET /inventory` and `GET /audit` return
  only in-scope rows, and **a single row fetched by id from outside the scope
  answers `404`, not `403`** — any other answer would confirm it exists. Audit
  rows carrying no environment are invisible to a scoped token. A malformed
  token configuration is a fatal startup error; with no tokens configured at all
  the service runs unauthenticated and says so loudly, which is a dev-only mode.

- **`GET /inventory` is paged** (`?limit`, default 100, max 500) rather than
  returning the whole estate in one response.

- **Write rate limiting that cannot be walked around**
  (`WRITE_RATE_LIMIT_PER_MINUTE`, default 120). Two buckets: address-keyed at
  the edge, ahead of the token check and the audit insert, and credential-keyed
  after it. A flood of distinct bearer values therefore neither bypasses the
  limit nor grows the bucket map without bound.

- **TLS 1.3 as the floor** when `TLS_CERT_FILE` and `TLS_KEY_FILE` are both set
  — nothing in the stack needs 1.2 kept open. `X-Content-Type-Options: nosniff`
  on every response and HSTS on a genuine TLS connection. TLS is opt-in; without
  those variables the API serves plain HTTP and warns at startup, on the
  assumption that TLS terminates at an ingress.

- **Redaction in the audit body** covering authorization, connection string,
  DSN, signing key, certificate and session field names.

- **The `configclient` HTTP fallback requires a credential.** It is validated
  when the client is constructed rather than on the day the fallback actually
  runs, `401`, `403` and `404` produce distinct errors instead of a quietly
  half-populated cache, and running the fallback unauthenticated has to be opted
  into explicitly.

- **The console does not hand the browser a token.** It proxies admin calls,
  checks `Origin`, forwards only `application/json` to a named list of API
  paths, and binds to loopback unless told otherwise — it attaches a full-scope
  admin token to everything it forwards.

- **Scheduled scanning, kept out of the contributor gate.** `govulncheck` and
  `npm audit` weekly and on pull requests that move a dependency manifest, and
  CodeQL over Go and JavaScript weekly and on every pull request. None of them
  gates an unrelated change: a scanner wired into CI turns an advisory published
  overnight into a red tick on somebody's typo fix, which is how a project
  teaches people that red means nothing.

- **No secrets in KV, by convention.** Anyone with NATS credentials can read
  every key and every consumer holds it in plaintext memory, so secret-shaped
  fields carry a marker (`"accessKeyId": "env:STORAGE_ACCESS_KEY_ID"`) the
  consumer resolves from its own secret store at bind time. Nothing enforces
  this — a reviewer has to.

### Known limitations at this release

Listed here because a first release is exactly where they are easiest to miss;
[`docs/PRODUCTION_READINESS.md`](docs/PRODUCTION_READINESS.md) §3 gives each one
a cost and a fix.

- **The Oracle repositories have never run against Oracle.** The SQL is
  compile-verified; every test runs against SQLite, whose bind syntax,
  pagination clause and type handling all differ. Treat the first deployment
  against a real instance as the test.
- **There is no migration runner, and the migration files are not in dependency
  order.** `001` has foreign keys into tables created by `002` and `003`, so a
  fresh database needs 002 and 003 applied first. Nothing records what has been
  applied, and there is no down path.
- **The schema is defined twice** — once as Oracle DDL in `migrations/`, once by
  hand in `internal/database/sqlite.go` — and kept in step by nothing but
  attention. Because every test runs on SQLite, a divergence shows up as a green
  build, which is the expensive kind.
- **The dual write is not transactional.** A crash between the database commit
  and the KV write leaves KV stale until the next reconcile. This is a decision,
  not an oversight: the reconciler bounds the exposure to one
  `RECONCILE_INTERVAL` and a transactional outbox brings its own failure modes.
- **KV has no per-key access control.** The environment prefix scopes what a
  consumer watches; it is not an authorisation boundary. With a single shared
  NATS credential, any consumer holding it can read every environment.
- **The HTTP cold-start fallback cannot rehydrate flags on its own.** There is
  no "list flag values for environment X" route, so `HTTPFallback.FlagValueIDs`
  has to name the row ids by hand.

[Unreleased]: https://github.com/ErasedKyte/Central-Config-Stream/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ErasedKyte/Central-Config-Stream/releases/tag/v0.1.0
