# Contributing to central-config

This is a control plane. A change here reaches every consuming service in the
fleet within milliseconds of an admin write, with no deploy and no review step
between the two. That shapes what this document asks of you: not ceremony, but
knowing which of the many places a change has to land, and being honest in the
pull request about what you did and did not verify.

If you are adding a new configuration domain alongside flags, appsettings and
localization, skip to [Adding a config domain](#adding-a-config-domain). That
list is the single most useful thing in this file, because roughly half of the
places it names fail at *runtime* rather than at compile time — you can ship a
domain that builds, passes review and quietly never reaches a consumer.

Taking part at all — issues, pull requests, review — means
[`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md) applies. It is the Contributor
Covenant, and it names a private channel for reporting a problem, because the
only other one this project has is a world-readable issue tracker.

---

## Prerequisites

| Tool | Version | Where that comes from |
|---|---|---|
| Go | **1.26 or newer** | the `go` directive in `go.mod`; the image builds on `golang:1.26-alpine` |
| Node | **20.19+ or 22.12+** | `engines.node` in `webui/package.json`; the image builds on `node:20-alpine` |
| Docker | any current version | the compose stack and `make test-postgres` — everything else runs without it |
| `nats` CLI | any current version | optional, for inspecting KV buckets |

No PostgreSQL client library is needed. The Postgres driver is pure Go
(`github.com/jackc/pgx/v5`, driven through `database/sql` by its `stdlib` shim),
and SQLite comes from `modernc.org/sqlite`, so `CGO_ENABLED=0` builds work
everywhere.

You do not need a Postgres server to build or to run `go test ./...`: the
integration suite in `internal/pgintegration` skips every test when
`TEST_POSTGRES_DSN` is unset. You do need one to have actually exercised the
production storage path, which is what `make test-postgres` is for — it starts a
throwaway container, runs the suite against it and removes it. CI runs the same
suite against a service container and fails if it skipped.

---

## Build, vet, test

`make help` lists every target, and CI runs the same checks on every pull
request. Run them locally before opening one — `make build`, `make test` and
`make lint` cover it, and the commands below are what they invoke.

```bash
# make build
go build ./...      # plus two -ldflags binary builds; see Version stamping

# make test
go test -race -count=1 ./...   # the reconciler, the rate limiter and configclient
                               # are all concurrent; -count=1 defeats the cache

# make lint — all four, in this order, and it stops at the first failure
gofmt -l .                     # must print nothing (node_modules excluded)
go vet ./...
golangci-lint run              # configured in .golangci.yml; v2 required
cd webui && npm run lint       # eslint, config at webui/eslint.config.js
```

`make lint` is the one people are surprised by: `golangci-lint` is the
centrepiece of it, and the last step runs the web UI's ESLint, so the target
needs `webui/node_modules` present — run `npm ci` in `webui/` once. It also
checks that the `golangci-lint` on your `PATH` is a v2, because `.golangci.yml`
is a v2 schema and a v1 binary fails in a way that reads like a code problem.
`make tools` installs the pinned version.

The web UI has two more of its own, which `make lint` does not run:

```bash
cd webui
npm test            # vitest
npm run build       # writes ../web
```

CI runs all of the above and three checks that have no `make` target: the
`--- SKIP` guard on the Postgres suite (below), `go mod tidy` leaving `go.mod`
and `go.sum` unchanged, and a clean working tree afterwards, so no build
artefact escaped `.gitignore`. It re-implements the recipes inline rather than
calling `make`, so the `Makefile` and `.github/workflows/ci.yml` have to be
changed together.

`go test ./...` starts a real embedded JetStream server for the messaging and
end-to-end tests, so it needs a free loopback port and takes appreciably longer
than a unit suite. It does not need Docker.

```bash
make test-postgres   # the same suite against a throwaway PostgreSQL container
```

That is the one part of `go test ./...` which is *not* run by default:
`internal/pgintegration` skips unless `TEST_POSTGRES_DSN` names a server, and it
is the only coverage the production SQL has. Run it before opening a pull
request that touches a `repository.go`, a migration, or anything about how a
value is bound or compared. CI runs it too, and fails the step if it skipped —
so a change that breaks the Postgres path is red there rather than discovered in
a deploy window.

### Version stamping

`make build` is not quite `go build`: it passes `-ldflags -X` to write the
version, the short commit and a UTC build date into `internal/buildinfo`, which
is what `./central-config --version` and `./testconsole --version` print. A
plain `go build` leaves the defaults — `dev`, `none`, `unknown` — and that is
fine: it is how a laptop binary tells you what it is. `make version` prints what
the stamp would be, and `make build VERSION=v1.2.3` overrides it for a release
pipeline that knows the version it is cutting better than `git describe` does.
The Dockerfile takes the same three values as build arguments and repeats them
as OCI image labels.

`--version` is answered before any configuration is read and before the database
or NATS is dialled, so it works on a machine that has neither.

### Formatting and linting

`gofmt` is the whole formatting standard — no `goimports` grouping convention
beyond what `gofmt` enforces. `go vet` and `golangci-lint` (configured in
[`.golangci.yml`](.golangci.yml), run by `make lint`) are the lint bar for Go. The web UI has an ESLint config at
`webui/eslint.config.js`; `npm run lint` must be clean.

Two conventions the tooling cannot check but the codebase holds to:

- **Comments say why, not what.** The code says what it does. Nearly every
  non-obvious block here carries a comment explaining the failure it is there to
  prevent, and that is the house style — if you remove a guard, you are removing
  a comment that says what happens without it, so read it first.
- **Errors are wrapped with context and never leak driver detail to a caller.**
  Repositories wrap with `fmt.Errorf("…: %w", err)`; handlers return a domain
  sentinel or a flat `internal server error`, and log the real one. SQL, table
  names and file paths are for the log, not the response body.

---

## The local development loop

The compose stack runs everything in Docker:

```bash
docker compose -f deploy/compose/docker-compose.yml up --build
# console: http://localhost:8090   admin API: :8080   NATS: nats://localhost:4222
```

For actual development that rebuild loop is too slow. Use the two-terminal setup
instead — the console can host an embedded JetStream server, so no Docker is
needed at all. **Build the UI first**; `web/` is gitignored, so a fresh clone has
none and the console has nothing to serve:

```bash
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

Both run from the repository root. Start order does not matter: the console
waits for `central-config` to create the KV buckets, and reports why it is
waiting while it does.

For UI work, `cd webui && npm run dev` serves the UI on `:5173` with hot reload
and proxies `/api` to the console on `:8090` — the console's `Origin` check
allows that specific case because both ends are loopback.

`deploy/compose/README.md` has the seeded data and a set of things to try
against the running stack.

### About the console

`cmd/testconsole` plays a consuming microservice: it holds a live `configclient`
cache and serves a React UI that shows that cache updating, with the measured
latency of every KV push. Admin writes typed into the UI are proxied to
central-config with an admin token attached by the console process, so the
browser never holds one.

It is a development tool and **not a deployment target**. It is as powerful as
the token it carries, which is why it binds to loopback, refuses a `Host` it was
not configured to answer to, refuses cross-origin requests and forwards only
JSON to an allowlisted set of paths. Do not add capability to it that would be
dangerous if it were exposed, on the assumption that it never will be. See
[`docs/SECURITY.md`](docs/SECURITY.md).

Two of those bite during development. `PORT=:8090` above binds **loopback**, not
every interface — the console reads `PORT` against `BIND_ADDR` and only a `PORT`
naming a host widens it — so reaching the console from another machine takes
`BIND_ADDR` *and* an `ALLOWED_HOSTS` entry naming it, or the `Host` check
answers 403. `/api/state` and `/api/events` are behind that check too.

---

## Commits

This repository uses [Conventional Commits](https://www.conventionalcommits.org/).
Real examples from its history:

```
feat(messaging): JetStream KV write-through distribution
fix(security): authenticate and scope reads, and harden the rate limiter
fix(reliability): make reconciliation fault-tolerant and decouple liveness from NATS
docs: add code of conduct, issue forms and contribution templates
ci: release binaries and a container image from a version tag
test: end-to-end coverage of the write-through path
chore: initialize Go module and project layout
```

Types in use, in descending order of how often: `chore`, `feat`, `docs`, `fix`,
`test`, `ci`. Scopes in use: `deps` and `deps-dev` (Dependabot's, and between
them the most common), then `security`, `db`, `messaging`, `reliability`, `api`,
`config`, `obs`, `deploy`, `webui`, `configclient`. Add a scope when the change
is confined to one of those; leave it off when it is not.

Those two lists are what the history actually contains rather than what would
look tidy — `refactor` is a perfectly good Conventional Commits type and this
repository has never used one, so it is not listed. If you write the first, add
it here.

The subject is a lowercase imperative phrase with no trailing full stop. Keep
the body for *why*, especially when the change closes a failure mode — that
reasoning is often the only record of what the code is defending against.

---

## What a pull request should include

- **A test for every behavioural change.** Not for a rename or a comment; for
  anything that changes what the service does with a request, a row or a key.
- **A note on what you could not verify.** If you changed Postgres-only SQL and
  did not run `make test-postgres`, say so — CI will run it, but the pull request
  should not imply you watched it pass. And nothing here has run in a production
  deployment yet (`docs/PRODUCTION_READINESS.md` §3.1), so an integration suite
  passing is evidence, not a guarantee. That is an acceptable state to be in and
  a bad thing to leave implicit.
- **`migrations/` and `internal/database/sqlite.go` changed together.** The
  schema is defined twice and nothing keeps the two in step. A missing *table*
  is now caught, because `internal/pgintegration` applies `migrations/` itself
  and the repositories would fail against it; a column that merely *differs*
  between the two is not, and shows up as a green build against a schema
  production does not have.
- **Documentation changed with the behaviour.** A new environment variable goes
  in `.env.example`; a new metric in `docs/OBSERVABILITY.md`; a change to what
  a token can reach in `docs/SECURITY.md`; a change to what consumers observe in
  `docs/CONSUMER_CONTRACT.md`. These documents are read by people who will not
  read the code.

### On test coverage

The three config domains carry the same three test files each, and — now — a
comparable amount in them, so there is no thinner one to match by accident:

| Package | What exists |
|---|---|
| `internal/flagsconfig` | service, handler and SQLite repository tests |
| `internal/servicesettings` | service, handler and SQLite repository tests |
| `internal/localization` | service, handler and SQLite repository tests |
| `internal/pgintegration` | all three PostgreSQL repositories and the audit store against a live server, plus one end-to-end case driving the whole service over it |
| `internal/app` | auth and scope middleware, read scoping, rate limiting, audit, liveness, observability, reconcile partial/prune, end-to-end stack |
| `internal/messaging` | keys, publisher (including skip and revision conflict), reconciler (prune, refusal, mid-sweep writes) |
| `pkg/configclient` | scoping and a full integration test against embedded JetStream |

"Same three files" is the shape, not the measure, and it is worth saying that
the sentence above was untrue until recently: `flagsconfig/handler_test.go` had
two test functions against the other two domains' eleven each, so the file
existed and covered almost nothing, and this document pointed newcomers at it as
the example to copy. It is now level with them. If you find yourself matching a
neighbouring domain's file *list*, count the cases too.

`flagsconfig` is the one to read first, because the split is the same in all
three: `service_test.go` for validation and publish behaviour,
`handler_test.go` for status-code mapping, `repository_sqlite_test.go` for the
SQL. A fourth domain gets the same three files, **plus a case in
`internal/pgintegration`** — the Postgres repositories are the ones a deployment
runs, and a domain that exists only in the SQLite tests is a domain whose
production SQL nothing has executed.

`internal/pgintegration` is one package rather than a file per domain because
its harness — schema creation, applying `migrations/`, seeding, teardown — would
otherwise be copied four times, and Go cannot share a helper that lives in a
`_test.go` file. Each test gets a fresh schema of its own, so tests do not have
to coordinate over identity sequences or the reference-table delete guards.

**What that suite barely covers is anything above the repositories.** The one
exception is `app_scope_test.go`, which builds the whole service through
`app.NewApp` over the real database and drives token scoping, the
row-environment lookup and the audit insert through HTTP. It exists because that
lookup picks its bind placeholder from the same `DBDriver` flag every
`internal/app` test sets to `"sqlite"`, so the branch a deployment runs was
reachable from no test at all — and what was in it was `:1` where pgx wants
`$1`, which Postgres rejects outright, which made the guard refuse every scoped
token every row-addressed write. Everything else in `internal/app` still runs on SQLite
alone, so the rest of the routing and publish wiring is verified against the
mirror schema rather than the real one (`docs/PRODUCTION_READINESS.md` §3.1).
If you add a branch that keys off `DBDriver`, assume nothing else will execute
the Postgres side of it.

---

## Adding a config domain

Flags, appsettings and localization are three instances of one shape, and the
shape is spread across the codebase rather than abstracted. Adding a fourth —
say routing rules, or per-service rate limits — touches **around 45 sites in 12
existing files** (§3–§13), plus a new package of its own, a new migration, five
documents, a case in the PostgreSQL integration suite, and the web console.

Roughly half of those sites do not fail to compile if you miss them. They fail
at runtime, and several fail *silently*: the write returns `200`, the row is in
the database, and nothing ever reaches a consumer. Those are marked
**⚠ silent** below. Work through the list in order; it is ordered so that each
step compiles once the previous one is done.

Trace `flagsconfig` while you read this — everything below has a working example
under that name.

### 1. The new package — `internal/<domain>/`

Six files, following the existing three packages exactly:

| File | What goes in it |
|---|---|
| `model.go` | the row struct with its JSON tags, `Page`, and the filter types |
| `errors.go` | sentinel errors: invalid-input, not-found, `…Exists` for each natural unique key, and `ErrConflict` for a lost optimistic-concurrency race |
| `repository.go` | the `Repository` interface, plus the PostgreSQL implementation and `ListAllForReconcile` |
| `repository_sqlite.go` | the SQLite mirror — `?` binds instead of `$1`; the `LIMIT/OFFSET` paging is the same on both |
| `service.go` | validation, `normalizePage`, the write-through publish, the delete purge, and the size check |
| `handler.go` | HTTP handlers plus a single `writeErr` that maps every sentinel to a status |

Points that are easy to get wrong, all of them learned from the existing three:

- **Validate the key components against what a KV key may hold.** Keys are
  joined with `.`, so any component that ends up in a key must not contain one —
  see `validFlagKey` and `validLocale`. A value that only a `PUT` let through
  still becomes a KV key, and one a consumer cannot parse is republished by
  every sweep from then on.
- **Run the same validation and reference checks on update as on create.** An
  update rewrites the natural key; without them a `PUT` can point at rows that
  do not exist or collide and surface as a `500`.
- **Check the payload size against `messaging.MaxValueSize` before the database
  write**, if the domain carries a document. Accepting it and failing at publish
  produces a `201` for a row that can never be distributed.
- **Give a document column `TEXT`, and bind it as a plain Go string.** There is
  no wrapper type to remember: Postgres takes a string straight into `TEXT`.
  Resist `jsonb` — the document is republished to KV verbatim and the publisher
  skips a write whose bytes already match, so a column that re-emits with its
  own key ordering would make every sweep decide every row had changed and
  republish the whole tree to the fleet on every cycle.
- **Map `database.IsUniqueViolation` to your `…Exists` error**, so a race
  between two creates is a `409` rather than a `500`.
- **Read the previous row before an update that can move it**, so the service
  can purge the KV key it moved away from. Afterwards there is nothing left to
  derive the old key from.

### 2. `migrations/NNN_config_<domain>.sql`

PostgreSQL DDL: table, constraints, indexes. **The file numbers are the apply
order**, so take the next number after everything your foreign keys reference —
`CONFIG_ENVIRONMENTS` is `001` and `CONFIG_MICROSERVICES` is `002`. That
property is exercised on every CI run, because `internal/pgintegration` applies
the directory in lexical order for each of its tests; a number out of dependency
order is a red build rather than a puzzle for whoever runs the DDL first.
`migrations/001` states the conventions the directory relies on — unquoted
UPPER CASE identifiers, `GENERATED BY DEFAULT AS IDENTITY`, `BIGINT` ids and
`TIMESTAMPTZ` timestamps — read them before adding a file.

### 3. `internal/database/sqlite.go` — ⚠ silent

The same table again, by hand, plus seed rows. Miss it and the local stack and
every test fail at runtime on a missing table, which at least is loud; get a
column subtly *different* and the tests pass against a schema production does not
have, which is not.

### 4. `internal/messaging/keys.go`

Two additions: the bucket-name constant, and the key builder. Document the key
shape in the builder's comment — consumers in other languages construct these by
hand from `docs/CONSUMER_CONTRACT.md`.

### 5. `internal/messaging/kv.go` — four sites, two ⚠ silent

1. A field on the `Buckets` struct.
2. An `ensureBucket` call in `EnsureBuckets`, **and** the field in the struct
   literal it returns.
3. The handle-swap line in `Ensure`. **⚠ silent** — miss it and the bucket is
   provisioned but the handle stays `nil`, so every publish fails with
   `ErrNotProvisioned` after any re-provision.
4. A case in the `byName` switch. **⚠ silent, and the classic one** — the
   default arm returns `messaging: unknown bucket "…"` at *runtime*. Nothing
   fails to compile. The admin write still returns `200`, the row is committed,
   the publish fails, and the reconciler fails the same way on every sweep
   forever.

### 6. `internal/messaging/publisher.go` and `noop.go`

Add `Publish<Domain>` to the `ConfigPublisher` interface and implement it on
`Publisher` and on `NoopPublisher`. This one is *compile-enforced* — the
interface breaks every implementation, including the fakes in the test files,
which is exactly why it is the safest part of the whole job. Use `raw()` for a
verbatim document; only flags need a custom `buildValue`, because their payload
carries a timestamp that must not count as a change on its own.

### 7. `internal/app/app.go` — the assembly point, ~11 sites

The import; the `<domain>Repository` interface pairing the domain `Repository`
with the lister; the `newRepositories` signature and **both** of its return arms
(PostgreSQL and SQLite); the service and handler construction in `NewApp`; the
argument to `NewRouter`; the field on the `inventoryHandler` literal; the
`<domain>Lister` interface; the `<domain>ReconcileSource` type with its `Name`,
`Bucket` and `Resync` methods; and — **⚠ silent** — registering that source in
the `messaging.NewReconciler(...)` call. Forget the registration and everything
works until a publish is dropped, at which point nothing ever heals it and no
prune ever removes a deleted row's key.

In `Resync`, record each row's outcome with `res.Publish(...)` rather than
returning early on the first error. A source that aborts strands every row
behind it, and because the sweep is then partial it neither prunes nor advances
its window — so it does the same thing again on every cycle.

### 8. `internal/app/config.go`

The `NewRouter` parameter, and the route registrations. Every route goes through
`sec.guard` (writes) or `sec.read` (reads); there is no third option, and adding
one to the mux directly is how a route ends up unauthenticated. Choose the
`targetClass` and `readClass` deliberately — see the next section, which is
where the real hazard is.

### 9. `internal/app/middleware.go` — five sites, **all ⚠ silent**

This is the file to be careful in. Nothing here fails to compile, and two of the
five failures are security holes rather than outages.

1. **A `classRow<Domain>` constant** in the `targetClass` block, if the domain
   has routes that address an existing row by id.
2. **A case in `targetClass.table()`.** The default arm returns `""`. That makes
   `lookupRowEnvironment` report failure, which sets `target.global = true`,
   which means **only a full-scope token can write to your domain at all**. It
   fails closed, so it is the mildest of these — but it is invisible until a
   scoped operator files a bug about a `403` nobody can explain.
3. **`targetClass.movesEnvironment()`**, if your update rewrites
   `ENVIRONMENT_ID` from the body. Miss it and the write is checked only against
   the environment the row is *in*, never the one it is moving *to* — a
   dev-scoped token can move a row into production. **This is a scope bypass,
   and it is silent.**
4. **A `classRead<Domain>` constant** in the `readClass` block, if the shape of
   your responses does not match an existing class.
5. **A case in `readClass.scopeField()`.** The default returns `""`, which means
   the response is **not narrowed at all** — a token scoped to dev gets back
   every environment's rows, including production's. **This is a scope bypass in
   the other direction, and it is also silent.** If your rows name their
   environment in `environmentId`, `classReadEnvRows` already covers you; make
   that the deliberate choice rather than the accident.

Write a middleware test for both directions. `middleware_test.go` and
`read_scope_test.go` have the pattern: a scoped token, an in-scope row, an
out-of-scope row, and the assertion that the second answers `404` and not `403`.

### 10. `internal/app/inventory.go` — four sites, ⚠ silent

The collection field on `Inventory`, the row struct, the field on
`inventoryHandler`, and the list-and-filter block. That block must call
`inScope(r, row.EnvironmentID)` and `page.take()` in that order, exactly as the
existing three do. Miss `inScope` and `/inventory` hands a scoped token the
whole estate — the same leak as §9, in a handler that narrows its own rows
instead of being narrowed on the way out.

### 11. `pkg/configclient/configclient.go` — ~8 sites, ⚠ silent

The bucket constant; the cache map on `Client`; its initialisation in `New`; the
watch prefix and the `startWatch` call in `connectAndWatch`; the `apply<Domain>`
function that parses a key and applies or deletes; the public reader method; the
`Snapshot` struct field and its copy loop; and the `Status.Counts` field.

Decide explicitly whether the domain is environment-wide (like flags) or
per-service (like appsettings and localization), because that decides the watch
prefix and therefore how much of the fleet's configuration every consumer holds
in memory. Miss the `startWatch` call and the whole feature is invisible to
consumers while every server-side test still passes.

### 12. `pkg/configclient/httpfallback.go` — ~5 sites

The response struct mirroring your `GET` endpoint, the `HTTPFallback` fields
identifying what to fetch, the `missingOwnKeys` check, the `hydrate` call, and
the `fetch<Domain>` function. Reuse `f.get(...)`: it attaches the bearer token
and turns a non-`200` into a message that distinguishes a rejected credential
from an out-of-scope read. If your endpoint is keyed by row id rather than by
something the consumer already knows, say so in the field comment — that
limitation is why the fallback needs configuring at all.

### 13. `cmd/testconsole/main.go` — ⚠ silent

One line: the path prefix in `proxiedPaths`. Miss it and the console answers
`404 not a proxied central-config path` for your whole domain, which reads like
a bug in the UI.

### 14. The web console — `webui/src/`

`api.js` (read and write wrappers), `App.jsx` (the `NAV` entry and the `VIEWS`
map), a new `views/<Domain>.jsx`, `useKvStream.js` (the `e.bucket === '…'`
branch), `drift.js` (the database-versus-cache comparison), and `Panels.jsx`
(the live consumer-cache panel). None of these break the build if omitted; the
domain is simply absent from the UI, and `drift.js` will report nothing about it
— which looks like "no drift" rather than "not checked".

### 15. Documentation

`docs/CONSUMER_CONTRACT.md` (the bucket table and the value shape — consumers in
other languages have nothing else to go on), `docs/SECURITY.md` (the
target-environment table and the read-scoping table), `docs/OBSERVABILITY.md`
(the `event.dataset` list), `README.md` (the key layout), and `.env.example` if
you added a variable.

### 16. `internal/pgintegration/<domain>_test.go`

Not optional, and not covered by the SQLite repository test from §1. The
Postgres repository is the one a deployment runs, and until there is a case here
its SQL has been checked by the compiler and nothing else. Follow
`flags_test.go`: create, read back, list and page, the guarded update that has
to answer a conflict on a stale timestamp, and — via `installRacingWriter` — the
create that loses a race and must come back as your `…Exists` sentinel rather
than a `500`. The harness hands you a freshly migrated, freshly seeded schema of
your own, so a test may write whatever it likes and drops it afterwards.

If the domain carries a document, add it to the large-document round trip too:
that is the test which says the bytes an admin sent are the bytes KV will be
handed, and the byte-identical publish skip depends on it.

### Before you call it done

```bash
go test ./... && go test -race ./...
make test-postgres            # §16 — otherwise that suite silently skips
```

Then, against the two-terminal stack: create a row through the API, confirm the
console's consumer cache shows it within milliseconds, delete it, and confirm
the key disappears rather than being blanked. Then stop NATS, make a write —
which must still succeed — start NATS again, and confirm the reconciler
republishes it without a restart. That last step is the one that catches a
missed `byName` case, a missed reconciler registration, and a missed `Ensure`
swap, and it is the one people skip.
