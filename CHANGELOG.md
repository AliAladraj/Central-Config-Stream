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

### Changed — breaking

One concept was called four things: `MICROCONFIG` in KV, `microconfig` in the Go
packages, `MicroSettings()` on the client, and appsettings in prose. It is
**service settings** now, and the rename reaches the wire rather than stopping
at the source, because a name that is only consistent internally has not been
made consistent for anyone who has to integrate against it.

This breaks consumers on both axes, at compile time and at run time. Read the
upgrade note below before taking it.

- **The KV bucket `MICROCONFIG` is now `SERVICESETTINGS`.** A consumer watching
  the old bucket keeps whatever it had cached and receives nothing further — the
  bucket it is watching stops being written to, which is the quiet failure this
  project exists to prevent, so it is stated plainly here rather than left to be
  discovered. The key layout inside the bucket is unchanged:
  `{environmentId}.{microserviceId}`, one whole settings tree per value.
- **`pkg/configclient` renames what it exposes.** `Client.MicroSettings()` is
  `Client.ServiceSettings()`. On `Snapshot`, the `Micro` field is
  `ServiceSettings` and serialises as `serviceSettings` rather than `micro`, and
  the same rename applies to the `Counts` struct. Behaviour is identical; only
  the names moved.
- **`GET /inventory` renames one field.** `microConfigs` is `serviceSettings`.
  Every other route, request body and response shape is untouched — in
  particular `settingsJson` keeps its name, and `/configs` keeps its path.

**Upgrading.** The database is the source of truth and needs no migration; no
table or column carried the old name. On first start after the upgrade the
service creates `SERVICESETTINGS` and the reconciler republishes every settings
row into it from the database, so the new bucket is correct without operator
action — verified on the local stack, where it repopulated within one reconcile
interval. The old `MICROCONFIG` bucket is *not* removed, because deleting data
on a version bump is not a thing this should do quietly. It is orphaned once
every consumer is upgraded, and you delete it yourself:

```bash
nats kv del MICROCONFIG --force
```

Upgrade consumers before deleting it, not after: a consumer still on the old
client is reading that bucket, and it is the only thing still feeding it.

### Added

- `GET /audit` accepts `environmentId`, so the audit log can be narrowed to one
  environment. It only ever narrows — a scoped token asking for an environment
  outside its scope gets an empty array rather than the rows, and rather than a
  403 that would confirm the environment exists.

## [0.1.1] — 2026-08-07

### Fixed

- The published container image is now built for `linux/arm64` as well as
  `linux/amd64` and pushed as one manifest list, so `docker pull` resolves the
  right image on an ARM host instead of failing outright. The build
  cross-compiles rather than emulating: the build stage is pinned to the
  machine running it and Go is told its target, so the second architecture
  costs a compile rather than minutes under QEMU.

## [0.1.0] — 2026-08-07

The first tagged release. Everything in the repository at the tag is listed
under it, and `## [Unreleased]` above is deliberately empty.

There is no earlier version to have fixed anything from, so there is no
**Fixed** section: the hardening that happened during development —
authenticating reads, making reconciliation fault-tolerant, splitting liveness
from readiness — is part of what 0.1.0 *is*, not a change to something anyone
ran. It is described below as the shipped behaviour rather than as a repair.

### Added

- **Three configuration domains over one admin API.** Feature flags, per-service
  appsettings and localization bundles, each with full CRUD over HTTP and a
  relational database as the source of truth. Appsettings and localization
  updates run the same validation and referential checks as creates, because
  those updates rewrite the natural key: without them a `PUT` could point a row
  at an environment that does not exist, or collide with another row and surface
  as a `500` rather than a `409`. A flag-value update addresses its row by id and
  rewrites only the value and the enabled flag, so it has no references to check
  and cannot collide — but it runs the same *input* validation, which is what
  matters there.

- **Flag values are validated on the way in.** `value` must be non-empty and at
  most 4000 characters — runes, because the `VARCHAR(4000)` column counts
  characters and `len()` would refuse a value of 4000 accented ones the column
  would take — and `enabled`, like `isActive` on a flag definition, must be
  exactly `0` or `1`. Only those two mean anything: the KV payload collapses
  everything non-zero to `true`, so a stored `7` is a row the API reads back as
  `7` while every consumer sees the same `true` a `1` would have given them.
  Each of these was a driver error mapped to a `500` before, except the empty
  value, which succeeded and published an empty string to every consumer in the
  environment — indistinguishable at the far end from a parse bug.

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

- **PostgreSQL as the source of truth**, through `jackc/pgx/v5` driven by
  `database/sql`. `DB_DRIVER=postgres` is the default and `CONN_STRING` takes a
  Postgres DSN; `DB_DRIVER=sqlite` still selects the local test stack. The
  driver is pure Go, so `CGO_ENABLED=0` static builds and the four-platform
  cross-compile are unaffected. Two type choices are load-bearing rather than
  incidental: the JSON documents live in `TEXT` columns and bind as plain Go
  strings — deliberately **not** `jsonb`, which re-emits with its own key order
  and would defeat the publisher's byte-identical skip, republishing every tree
  to the whole fleet on every sweep — and `UPDATED_AT` is `TIMESTAMPTZ`, which
  makes the incremental reconcile window a plain instant comparison and removes
  a class of timezone bug rather than an instance of one. Also a bounded
  connection pool (`DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`,
  `DB_CONN_MAX_LIFETIME`, `DB_CONN_MAX_IDLE_TIME`), which matters more against
  Postgres because every connection there is a backend process, and
  unique-constraint violations (SQLSTATE `23505`) mapped to `409` instead of
  `500`.

- **The production storage path is covered by tests.**
  `internal/pgintegration` runs against a live PostgreSQL — 29 tests, each in a
  fresh schema it creates, migrates from `migrations/*.sql` and seeds itself, so
  applying the shipped DDL is exercised several dozen times on every run. The
  reconcile window is pinned on sessions at UTC+14 and UTC−11 as well as UTC,
  because those are the two signs the timezone bug came in and a UTC-only runner
  would miss both. Large documents are asserted to round-trip byte-identically,
  which is what the publish skip depends on. The suite skips cleanly when
  `TEST_POSTGRES_DSN` is unset, so `go test ./...` stays green without a
  database; `make test-postgres` starts a throwaway one.

  Twenty-five of those drive the repositories and the audit store. The other
  four drive the **whole service** — router, middleware chain, audit store —
  over the real database, because the scope middleware picks its bind
  placeholder from the same driver flag every other end-to-end test sets to
  `sqlite`, so the branch a deployment runs was reachable from no test at all.
  Everything else above the repositories still tests on SQLite alone
  ([`docs/PRODUCTION_READINESS.md`](docs/PRODUCTION_READINESS.md) §3.1).

- **A local stack that exercises the whole path on one machine.** A compose
  file, a distroless multi-stage image (non-root, one target per binary) and
  Kubernetes manifests, plus `DB_DRIVER=sqlite` and an embedded JetStream server
  so the write → database → KV → consumer path runs with no PostgreSQL and no
  NATS installation.

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
  a `postgres:17-alpine` service container so the integration suite above runs
  there rather than skipping, and because that suite fails *open* by design, the
  workflow re-runs it verbosely and fails the step if any test skipped. A DSN
  typo would otherwise leave the gate green while testing nothing
  ([`docs/PRODUCTION_READINESS.md`](docs/PRODUCTION_READINESS.md) §3.6). The
  release workflow's own gate stands up the same container with the same skip
  check, because that is the run which decides whether a version number gets
  spent.

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
  rows carrying no environment are invisible to a scoped token. A narrowed
  listing whose page keeps no rows answers `[]` with `200`, so a scoped caller
  paging to the end of a collection sees an ordinary empty page rather than a
  server error.

- **A token configuration that authenticates nobody stops the process.** A
  malformed `ADMIN_TOKENS` was already fatal; a value that is *set* but yields
  no entry now is too. `" , , "` — what a Helm value referencing an empty secret
  renders to — parsed to the empty token set, and the empty token set is how
  "authentication is off" is represented, so the process came up announcing that
  neither variable was set and served unauthenticated deletes. A whitespace-only
  `ADMIN_TOKEN` is fatal for the opposite reason: it is a live full-scope
  credential whose secret is a space. A **blank** `ADMIN_TOKENS` stays the
  documented development path — it cannot be distinguished from the variable
  being absent — and still announces itself loudly at startup.

- **`GET /inventory` is paged** (`?limit`, default 100, max 500) rather than
  returning the whole estate in one response.

- **Write rate limiting that cannot be walked around**
  (`WRITE_RATE_LIMIT_PER_MINUTE`, default 120), over four key namespaces:
  `ip:<addr>` at the edge for a write arriving with a configured credential,
  `anon:<addr>` at the edge for one arriving without — charged four tokens, so a
  quarter of the rate, and kept a separate namespace so a flood of
  credential-less writes cannot spend the budget an authenticated caller behind
  the same ingress IP is about to need — `t:<name>` per credential after
  authentication, and `inv:t:<name>` for `GET /inventory`. That last is the only
  rate-limited read, because it is the only one not bounded by a page in SQL: it
  reads all three domains whole and pages in Go, so any valid token could
  otherwise loop it for three full table scans per request. A flood of distinct
  bearer values neither bypasses the limit nor grows the bucket map without
  bound.

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
  forwards only `application/json` to a named list of API paths, and binds to
  loopback unless told otherwise — it attaches a full-scope admin token to
  everything it forwards. `PORT` is read against `BIND_ADDR` and only a `PORT`
  naming a host widens the bind, so neither `8090` nor `:8090` reaches beyond
  the machine on its own; the embedded JetStream server follows the same
  variable rather than sitting on `0.0.0.0` regardless.

- **The console checks `Host` before it checks `Origin`.** Comparing `Origin`
  against `Host` defends nothing alone, because a page sets both: `evil.example`
  resolving to `127.0.0.1` reaches the console with the two agreeing, and no
  preflight applies to a JSON `GET` or to a `POST` the page can shape. Only
  `Host` gives DNS rebinding away, since a browser fills it in from the address
  it was pointed at. The console answers only to loopback, the address it is
  bound to, and whatever `ALLOWED_ORIGINS` and the new `ALLOWED_HOSTS` name;
  anything else is `403` before the proxy builds a request. `/api/state` and
  `/api/events` are behind the same guard, because they hand over the consumer's
  whole cache.

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

- **PostgreSQL has never run a deployment.** The repositories, the audit store
  and one end-to-end slice of the service are exercised against a real server on
  every CI run, which is a great deal more than compilation, but no production
  instance has yet carried this schema — and the coverage stops just above the
  repository layer, so the rest of the end-to-end and handler tests still run on
  SQLite alone. Treat the first deployment as the remaining test.
- **There is no migration runner.** `migrations/*.sql` is DDL you apply
  yourself. The file numbers *are* the apply order, so the directory in lexical
  order is correct on an empty database, but nothing records what has been
  applied and there is no down path.
- **The schema is defined twice** — once as PostgreSQL DDL in `migrations/`,
  once by hand in `internal/database/sqlite.go` — and kept in step by nothing
  but attention. A missing table is caught now that the integration suite
  applies the migrations; a column that merely *differs* is not, and shows up as
  a green build against a schema production does not have.
- **The dual write is not transactional.** A crash between the database commit
  and the KV write leaves KV stale until the next reconcile. This is a decision,
  not an oversight: the reconciler bounds the exposure to one
  `RECONCILE_INTERVAL` and a transactional outbox brings its own failure modes.
- **KV has no per-key access control.** The environment prefix scopes what a
  consumer watches; it is not an authorisation boundary. With a single shared
  NATS credential, any consumer holding it can read every environment.
- **The HTTP cold-start fallback fetches flags by row id.**
  `GET /flags/values?environmentId=` exists, but `configclient`'s fallback does
  not use it — it fetches `GET /flags/values/{id}` one row at a time, so
  `HTTPFallback.FlagValueIDs` has to name the row ids by hand. Closing this is a
  change to `pkg/configclient/httpfallback.go`, not a new server route.

[Unreleased]: https://github.com/AliAladraj/Central-Config-Stream/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/AliAladraj/Central-Config-Stream/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/AliAladraj/Central-Config-Stream/releases/tag/v0.1.0
