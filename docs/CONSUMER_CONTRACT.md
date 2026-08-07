# Consumer contract

How to write a service that reads its configuration from central-config, in any
language that has a NATS client.

This document is self-contained. It assumes no knowledge of the central-config
Go codebase — everything the consumer side needs is here. If your service is
written in Go, `pkg/configclient` already implements all of it and you can read
this as a description of what that library does.

---

## 1. What you are integrating with

`central-config` distributes feature flags, per-service settings, and
localization bundles to services. It is split into two planes, and that split
governs the whole design:

- **Control plane** — `central-config`, an HTTP admin API backed by a relational
  database. Admins and deploy pipelines write here. **Your service never calls
  it.**
- **Data plane** — NATS JetStream KV. Your service watches it, caches values in
  memory, and serves every config read from that cache. No polling, no HTTP, no
  database on the request path.

The consequences are worth being explicit about, because they are the reason to
build it this way:

- If central-config or its database is down, your service keeps running on
  cached values.
- If NATS goes down *after* your service has started, the same — values freeze
  at last known good rather than erroring.
- If NATS is unreachable at *cold start*, you have nothing. §4 covers what to do
  about that.

## 2. The data you will read

Three KV buckets. Keys are environment-scoped, so a consumer can watch one
prefix and receive only what belongs to its environment.

| bucket | key format | example |
|---|---|---|
| `FLAGS` | `{environmentId}.{flagKey}` | `1.search_v2` |
| `SERVICESETTINGS` | `{environmentId}.{microserviceId}` | `1.1` |
| `LOCALIZATION` | `{environmentId}.{microserviceId}.{locale}` | `1.1.en-US` |

`environmentId` and `microserviceId` are row ids from the control plane's
`CONFIG_ENVIRONMENTS` and `CONFIG_MICROSERVICES` tables. `GET /inventory` on the
admin API lists every editable row with its id, which is how you find the two
numbers your service needs; pass them in as deployment configuration rather than
hardcoding them.

**Watch the narrowest prefix you need**, one watcher per bucket:

```
FLAGS            "{env}.>"                  flags are shared across services
SERVICESETTINGS  "{env}.{microserviceId}"   exactly one key — your own settings
LOCALIZATION     "{env}.{microserviceId}.>"
```

Do not watch `{env}.>` on `SERVICESETTINGS`. That caches every other service's
configuration in your process, for no benefit — and see §5 on why holding other
services' settings is something to avoid deliberately.

### Value shapes

`FLAGS`:

```json
{ "enabled": true, "value": "0.25", "updatedAt": "2026-01-15T10:07:59Z" }
```

`enabled` is the on/off answer. `value` is a free-form string used for things
like rollout percentages or variant names; parse it yourself when a flag carries
more than on/off. `updatedAt` is RFC 3339 and is useful for the ordering guard
in §3.

`SERVICESETTINGS` is **the entire service settings tree as one JSON document** — not one
key per setting. One push replaces the whole document atomically, so a consumer
can never observe half an update. The shape of that tree is entirely yours; the
control plane treats it as opaque JSON.

`LOCALIZATION` is one flat JSON object per locale:

```json
{ "catalog.title": "Catalog", "catalog.search": "Search" }
```

Buckets keep five historical values per key, which is the available rollback
depth on the KV side.

### A value is at most 512 KiB

The buckets are provisioned with an explicit 512 KiB ceiling per value, and the
control plane refuses an oversized service settings tree or translation bundle with a
`400` before the row is written. The two limits are the same constant on
purpose: a payload accepted by the admin API but refused by JetStream would be
drift no reconcile sweep could ever heal, failing on the same row every cycle.

What this means for you:

- **Do not design a consumer around one enormous document.** An service settings tree
  is one key and is replaced whole; if yours is approaching a few hundred
  kilobytes, split the service or move the bulk out of configuration.
- **Translation bundles are per locale**, not per service, so the ceiling
  applies to each locale separately. That is usually the constraint that saves
  you.
- If a bucket was provisioned before this ceiling existed, it inherits the
  server's `max_payload` instead. Check with `nats kv info <BUCKET>`;
  central-config re-applies the configuration on every startup and on every
  reconcile cycle, so restarting the control plane is enough to correct it.

## 3. What to build

One component that owns the NATS connection and exposes a typed, always-current
snapshot to the rest of the application.

1. **Connect with unlimited reconnect.** Whatever your client calls it, set the
   reconnect attempts to infinite. A NATS blip must never take the service down,
   and a client that gives up after N attempts turns a transient outage into a
   permanent one that only a restart fixes.

2. **Watch the prefixes from §2**, one watcher per bucket you actually use.

3. **Deserialise on push, not on read.** Parse inside the watch callback, build
   whatever immutable structure your language offers, and swap a single
   reference to it. A read from application code should be a field access, never
   a JSON parse. If your language has no atomic reference swap, use the smallest
   lock you can and never hold it across I/O.

4. **Gate startup on the first snapshot.** A KV watch delivers the current value
   of every matching key first, then signals that the initial replay is done
   (most clients expose this as an end-of-initial-values marker, a `null`/
   sentinel entry, or a `delta == 0` on the last entry). Block startup until
   that signal arrives, with a timeout.

   This step is not optional. **A service that starts serving on an empty cache
   sees every flag as `false` and every setting as missing** — that is exactly
   how a "feature disabled everywhere" incident happens, and it will look like a
   bug in your application rather than in your config bootstrap.

5. **Handle deletes.** An entry whose operation is a delete or a purge means the
   key is gone. Remove it from the cache rather than storing an empty value —
   otherwise a deleted flag becomes a permanently-false flag rather than
   falling back to your default.

6. **Keep checked-in defaults as the floor.** Ship a static config file with
   sane defaults and treat KV as the override layer on top of it. This is what
   makes a cold start with NATS unreachable survivable, and it gives you
   something to diff against when a value looks wrong.

### Updates are at-least-once — the handler must be idempotent

A delivery does not mean the value changed. You will be handed the same value
more than once:

- every reconnect replays the current value of every watched key;
- the control plane's reconciler republishes on its own schedule to heal drift.

There is no deduplication on the consumer side, and none on the wire.

So whatever runs inside the watch callback must be safe to run repeatedly with
an unchanged value. Idempotent assignments are fine — updating a log-level
switch, re-reading a timeout, replacing a snapshot reference. What is *not* fine
is anything with a per-delivery side effect:

- recreating an HTTP client or connection pool (socket exhaustion);
- flushing or invalidating a cache;
- emitting a "config changed" notification, webhook or audit event;
- restarting a background worker or rebalancing a consumer group.

Do those only after comparing the incoming value with the one you already hold,
and treat an equal value as a no-op.

The control plane skips writing a value that KV already holds byte-for-byte, so
redundant republishes are rare in practice — but that is an optimisation, not a
guarantee. Reconnect replay is unconditional.

### Ordering

Each KV entry carries a monotonically increasing revision. During a reconnect
you can in principle be handed an older value after a newer one. Guard against
it by ignoring any update whose `updatedAt` (or revision) is older than what you
already hold for that key.

## 4. Behaviour when KV is unreachable

Two cases, and they deserve different answers:

| when | what you hold | what to do |
|---|---|---|
| **After start** | a warm cache | Serve it. Values freeze at last known good; log a warning, expose it in your health output, do not fail reads. The watch will re-warm the cache automatically on reconnect. |
| **At cold start** | nothing | Do not serve traffic on an empty cache. Fail the readiness probe until the first snapshot arrives, or start from checked-in defaults if — and only if — those defaults are safe to serve. |

The second row is the one that bites. The safest default is to block startup and
let the orchestrator retry; a service that comes up "healthy" with no config is
worse than one that visibly refuses to start.

If you need a cold start to survive a NATS outage and defaults are not enough,
the control plane's read endpoints can hydrate the cache once at startup over
HTTP. Treat that strictly as a bootstrap path: one pass, at start, never on the
request path. `pkg/configclient` implements exactly this and keeps it off by
default.

**The bootstrap path needs a bearer token.** The admin API authenticates every
route except `GET /health`, `GET /livez` and `GET /metrics`, reads included, and
a token's environment scope narrows what it may read as well as what it may
write. So a consumer that wants this fallback needs a credential of its own,
scoped to its environment — which is a real cost, and the reason the fallback
stays optional rather than becoming the default answer to a cold start.

In `pkg/configclient` that is `HTTPFallback.Token`. It is validated in `New`,
whether or not the fallback is ever reached, so a missing credential fails at
boot in dev and in CI rather than months later with KV already down and a `401`
as the only clue. `AllowUnauthenticated` exists for a control plane running with
auth switched off, which is a dev-only mode it warns about at startup; setting
it anywhere else just converts a boot-time configuration error into a runtime
one on the worst possible day.

Three status codes are worth telling apart if you are writing your own client,
because none of them means "there is no configuration":

| status | what it means | what to do |
|---|---|---|
| `401` | the control plane does not accept this token | fix the credential |
| `403` | the token is accepted but not permitted here | get one whose scope lists your environment |
| `404` | **either** no such row **or** the token is not scoped to your environment | check the scope before concluding the row is gone |

That `404` is deliberate: answering `403` for an out-of-scope row would confirm
the row exists. It is also the one that will waste your afternoon, so handle a
`404` during bootstrap by reporting both possibilities.

A credential refused on any endpoint should fail the whole hydration pass rather
than being retried — it is not the kind of failure that gets better while the
process runs. A single endpoint being unwell should not; a partial cold start is
better than none. `configclient` splits it exactly that way.

Which endpoints a bootstrap can use is a question about their keys. KV is keyed
the way a consumer thinks — flag key, microservice, locale — and the
fetch-by-id endpoints are not:

```
GET /localization/lookup/{msId}/{envId}/{locale}   reachable from what you know
GET /configs/values/{id}    {id} is the service settings row id, not the service id
GET /flags/values/{id}      {id} is the flag-value row id, not the flag key
```

`configclient`'s fallback uses exactly those three, which is why it has to be
handed the row ids as deployment configuration — `GET /inventory` is where an
operator finds them.

**If you are writing your own client, do not copy that.** The listing routes are
keyed the way you already think and answer in one request:

```
GET /flags/values?environmentId={envId}      every flag value in the environment,
                                             each row carrying its flagKey
GET /configs/values?microserviceId={msId}&environmentId={envId}
GET /localization?microserviceId={msId}&environmentId={envId}
```

All three are paged (`?limit`, default 100, max 500, `&offset`) and narrowed to
your token's scope, so page until you get back `[]`. The flags listing in
particular removes the whole row-id problem for a cold start: the environment is
something the consumer was configured with anyway. That `configclient` does not
use it yet is a limitation of the shipped client, not of the control plane.

## 5. Secrets — the `env:VAR_NAME` convention

**Never put a real credential in a KV value.**

Anyone holding NATS credentials can read every key they are permitted to
subscribe to, and every consumer caches what it reads in plaintext process
memory. KV has no field-level encryption. Unless you have configured per-service
NATS permissions (see [`docs/SECURITY.md`](SECURITY.md)), a single shared
credential means every
consumer can read every other service's settings.

The convention, which the seeded example data demonstrates: secret-shaped fields
carry a marker instead of a value.

```json
"storage": {
  "endpoint": "https://objects.example.com",
  "bucket": "catalog-media",
  "accessKeyId": "env:STORAGE_ACCESS_KEY_ID",
  "secretAccessKey": "env:STORAGE_SECRET_ACCESS_KEY"
}
```

Your consumer resolves `env:NAME` at bind time by looking `NAME` up in its own
environment, or in whatever secret store it already uses. Put that behind a
one-method interface — "given a marker, give me the value" — so the backing
store can change from environment variables to a managed secret store without
touching the binding code.

Anything matching a password / secret / key / token / connection-string shape
belongs in the deployment secret store and gets a marker in KV, not a value.
Nothing in the control plane enforces this; it is a review discipline, and it is
the reason config trees can be distributed this freely at all.

Two related habits worth adopting:

- **Never log a resolved config tree.** Log the keys that changed, not the
  document. The control plane redacts secret-shaped fields in its own audit
  trail; your logs are not covered by that.
- **Think about kill switches.** A setting like "skip TLS certificate
  validation" is legitimate in a config tree, but once it is in KV, anyone with
  a write token can flip it on a running system with no deploy and no review.
  Decide deliberately, per setting, whether it is live-updatable or pinned in
  your checked-in defaults for sensitive environments.

## 6. Integration traps

Five that are easy to get wrong regardless of the language you are writing in.

- **Never rebuild a connection or client per read — or per delivery.** Whatever
  your runtime calls it, an HTTP client, connection pool or database handle is
  built once and kept for the process lifetime. A configuration value that
  changes how such a client behaves is tempting to implement by throwing the
  client away and building a new one in the watch callback; do not. That is the
  fastest route to socket exhaustion, and §3 explains why the callback fires far
  more often than the value changes. Prefer applying the value *per call* — a
  per-request deadline, a per-request retry budget — over rebuilding anything.
  If you genuinely must rebuild, gate it on the value having actually changed.

- **Honour cancellation.** Every blocking step in the config path — waiting for
  the first snapshot, the watch loop itself, an HTTP hydration fallback — should
  take whatever cancellation or deadline mechanism your language offers and
  return promptly when it fires. Skip this and two things go wrong: a service
  that cannot reach NATS hangs in startup instead of failing its readiness probe
  and letting the orchestrator retry, and shutdown blocks on a watch that never
  returns.

- **Swap cached state atomically, and read it once per request.** Build the new
  snapshot completely, then publish it with a single assignment; never mutate a
  structure that readers are already holding. On the read side, take one
  reference at the top of a request and use that same reference throughout.
  Reading the shared location repeatedly can straddle a push and mix old and new
  values within a single request — a class of bug that is essentially impossible
  to reproduce from a report.

- **Release what you replace, and only after you have replaced it.** If your
  language makes you manage the lifetime of parsed documents, buffers or
  clients, free the old one *after* the new reference is visible to readers,
  never before: an in-flight reader is still using it. Under a garbage collector
  there is nothing to do here beyond not retaining references you have finished
  with.

- **Pin your NATS client version and check the API against that version.**
  JetStream KV is younger than core NATS, and the watch and serialisation
  surfaces have changed between minor releases in several client libraries. Pin
  the version, read that version's own documentation, and do not trust a sample
  written against an older one.

## 7. Testing against a real stack

central-config ships a local stack: NATS JetStream, the service on SQLite, and a
React console that shows what a consumer holds and logs every KV push with its
measured latency.

```bash
# in the central-config repo
docker compose -f deploy/compose/docker-compose.yml up --build
# console: http://localhost:8090   admin API: :8080   NATS: nats://localhost:4222
```

Point your service at `nats://localhost:4222` with environment id `1` (`dev`).
The stack's admin token is `local-dev-token`, full scope — that is what to give
a bootstrap fallback you want to exercise locally.
Seeded there: flags `search_v2`, `dark_mode` and `new_pricing`; service settings for
three services, ids `1`, `2` and `3`; and `en-US` / `pt-BR` localization bundles
for service `1`. Service `1` (`catalog-api`) carries the full example service settings
tree, including the `env:` secret markers, at KV key `1.1`.

The loop that proves the integration works:

1. Start your service and confirm it logs a warm cache **before** it reports
   ready. If it serves anything before the first snapshot, fix that first.
2. Change a value in the console UI. Your service must observe it in
   milliseconds, with no restart and no polling.
3. Stop the `nats` container. Reads must keep working, on frozen values.
4. Change a value while NATS is down — the admin write still succeeds, since the
   database is the source of truth.
5. Start `nats` again. Your service must reconnect on its own and pick up the
   change made while it was down, without a restart.
6. Delete a key through the admin API. Your service must fall back to its
   checked-in default, not to a null or an empty string.

Steps 3–5 are the ones people skip and the ones that fail. A consumer that
passes 1, 2 and 6 but not 5 will look perfect until the first NATS restart.

## 8. Checklist

- No HTTP call to central-config on the request path.
- A config read is a memory access — no parsing, no I/O, no lock held across I/O.
- The service does not serve traffic until the first snapshot is applied.
- A KV push is reflected without a restart, within milliseconds.
- NATS down after startup degrades to frozen values, not errors.
- The watch callback is idempotent; side effects are gated on a value change.
- Deletes remove keys rather than blanking them.
- No secret value appears in any KV key, log line, or exception message.
- Only this service's own `SERVICESETTINGS` key is watched, not the whole bucket.
- No service settings tree or bundle is anywhere near 512 KiB.
- If an HTTP bootstrap fallback is configured, it carries a bearer token scoped
  to this environment, is validated at start, and runs once — never on a read.
