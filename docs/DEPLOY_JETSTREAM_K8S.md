# Deploying NATS JetStream + central-config on Kubernetes

This guide stands up a production-shaped NATS JetStream cluster on Kubernetes and
deploys `central-config` against it. JetStream KV is the distribution layer;
Oracle stays the source of truth.

Sections:
1. Prerequisites
2. Install the NATS JetStream cluster (Helm)
3. Verify JetStream
4. Security: accounts & credentials (write vs read-only)
5. Deploy central-config
6. Buckets (auto-created on startup)
7. Wire up consuming services
8. Observability & operations
9. Troubleshooting

---

## 1. Prerequisites

- A Kubernetes cluster (1.25+) and `kubectl` context set.
- [Helm 3](https://helm.sh/).
- The NATS CLI for verification: `nats` ([install](https://github.com/nats-io/natscli)).
- A namespace:

```bash
kubectl create namespace messaging
kubectl create namespace central-config
```

---

## 2. Install the NATS JetStream cluster (Helm)

Add the repo:

```bash
helm repo add nats https://nats-io.github.io/k8s/helm/charts/
helm repo update
```

Use the values file at [`deploy/k8s/nats-values.yaml`](../deploy/k8s/nats-values.yaml):
a 3-node cluster, JetStream enabled with **file** storage and a persistent
volume per node. Install:

```bash
helm upgrade --install nats nats/nats \
  --namespace messaging \
  -f deploy/k8s/nats-values.yaml
```

Wait for all 3 pods:

```bash
kubectl -n messaging rollout status statefulset/nats
kubectl -n messaging get pods -l app.kubernetes.io/name=nats
```

The in-cluster URL will be:

```
nats://nats.messaging.svc.cluster.local:4222
```

> **Sizing.** Config volume is tiny; JetStream file storage with a few GiB per
> node is ample. Replicas=3 gives quorum so a single node loss doesn't lose the
> KV. History depth (per-key rollback) is the main sizing knob.

---

## 3. Verify JetStream

Port-forward and check that JetStream is enabled and clustered:

```bash
kubectl -n messaging port-forward svc/nats 4222:4222 &
nats --server nats://localhost:4222 account info
nats --server nats://localhost:4222 server report jetstream
```

You should see JetStream `enabled` and 3 servers in the cluster.

---

## 4. Security: accounts & credentials

> [`docs/SECURITY.md`](SECURITY.md) §2 is the authority on this. What follows
> is how to apply it on Kubernetes; if the two ever disagree, that document is
> right.

Give `central-config` **write** on the config buckets and consumers **read/watch
only**. With JetStream KV, a bucket `FLAGS` is backed by stream `KV_FLAGS` and
subjects `$KV.FLAGS.>`.

The writer is straightforward — one credential, and nothing else in the system
publishes to those subjects:

```
# central-config (writer) — one credential, and only this one
publish:   "$KV.FLAGS.>", "$KV.MICROCONFIG.>", "$KV.LOCALIZATION.>",
           "$JS.API.>"
subscribe: "_INBOX.>"
```

### Consumers: per service and per environment, not per fleet

**Do not issue consumers a credential that subscribes to `$KV.FLAGS.>` and the
rest fleet-wide.** It is the obvious shape and it is wrong: it lets any service
holding it read every other service's appsettings, in every environment, and
appsettings trees are exactly where connection details and integration endpoints
accumulate. The `env:VAR_NAME` marker convention keeps real secret values out of
KV, but a convention is a weak control and nothing enforces it.

The key layout is already shaped for the strong control. Keys are
`{env}.{flagKey}`, `{env}.{microserviceId}` and `{env}.{microserviceId}.{locale}`,
so a subject filter narrows a consumer to its own configuration:

```
# consumer: cart-api (microservice id 2), environment 3 (prod)
publish:   "$JS.API.STREAM.INFO.*", "$JS.API.CONSUMER.CREATE.*",
           "$JS.API.DIRECT.GET.*"
subscribe: "$KV.FLAGS.3.>",
           "$KV.MICROCONFIG.3.2",
           "$KV.LOCALIZATION.3.2.*",
           "_INBOX.>"
```

Flags stay environment-wide because flags are not per-service. Appsettings and
localization become per-service, which is what closes the leak. The environment
number is in every subject, so a dev credential cannot carry `3.` subjects —
without that, a leaked dev credential still reads production, and the API's own
per-environment token scoping is undone at the data plane.

That is one credential per (service, environment) pair. It is more credentials
to issue and rotate, and that is the actual cost of the model; `nsc` and a
templated issuing step make it routine. Consumers should live in a **separate
account** from `CONFIG`, reaching the buckets through explicit exports and
imports, so the account boundary rather than a subject string is the default
deny — see [`docs/SECURITY.md`](SECURITY.md) §2 for the account layout.

A fleet-wide `$KV.*.>` subscription is the shape the local compose stack and the
test console use, because there is one machine, one operator and nothing to
isolate. Treat it as laptop-shaped and do not promote it.

Generate NKEY/JWT creds with `nsc` (recommended) or issue static creds, then
store them as secrets:

```bash
kubectl -n central-config create secret generic nats-creds \
  --from-file=nats.creds=./central-config.creds

# in each consumer's namespace — one per (service, environment)
kubectl create secret generic nats-consumer-creds \
  --from-file=nats.creds=./cart-api-prod.creds
```

Enable **TLS** to the cluster in `nats-values.yaml` (`tls` block) and mount the
CA into clients. For local/dev you can skip creds and TLS entirely.

---

## 5. Deploy central-config

Create the secrets it needs:

```bash
kubectl -n central-config create secret generic central-config-secrets \
  --from-literal=CONN_STRING='oracle://user:pass@oracle-host:1521/svc' \
  --from-literal=ADMIN_TOKEN='<a-long-random-token>'
```

`ADMIN_TOKEN` is the smallest thing that works and every change it makes is
attributed to a single actor named `shared`. For anything where the audit trail
has to mean something, use `ADMIN_TOKENS` instead — one named, environment-scoped
credential per person or pipeline:

```bash
kubectl -n central-config create secret generic central-config-secrets \
  --from-literal=CONN_STRING='oracle://user:pass@oracle-host:1521/svc' \
  --from-literal=ADMIN_TOKENS='alice:*:<secret>,ci-dev:1|2:<secret>,release:3:<secret>'
```

Mind two constraints, both of which are fatal at startup rather than silent: a
secret may contain colons but **not a comma**, and a name is limited to letters,
digits, dot, dash and underscore. [`docs/SECURITY.md`](SECURITY.md) explains why
both are checked.

Apply the Deployment + Service ([`deploy/k8s/central-config.yaml`](../deploy/k8s/central-config.yaml)):

```bash
kubectl -n central-config apply -f deploy/k8s/central-config.yaml
kubectl -n central-config rollout status deployment/central-config
```

Key env the manifest sets:

| Env | Value | Meaning |
|-----|-------|---------|
| `NATS_URL` | `nats://nats.messaging.svc.cluster.local:4222` | cluster address |
| `NATS_CREDS` | `/etc/nats/nats.creds` | mounted from `nats-creds` secret |
| `PUBLISH_ENABLED` | `true` | write-through to KV + run reconciler |
| `NATS_REPLICAS` | `3` | KV buckets created with quorum |
| `RECONCILE_INTERVAL` | `5m` | Oracle→KV drift heal cadence |
| `ADMIN_TOKEN` | from secret | bearer token — required on every route except `/health`, `/livez` and `/metrics` |

Worth setting explicitly for a real deployment, though the manifest leaves them
at their defaults: `RECONCILE_PRUNE_MAX_FRACTION` (default `0.2`) and the
connection-pool bounds `DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`,
`DB_CONN_MAX_LIFETIME`, `DB_CONN_MAX_IDLE_TIME`. The pool defaults (20/5/30m/5m)
are per replica, so multiply by `replicas` before comparing them against what
your Oracle instance will tolerate — this service is usually not the only thing
on that database.

### Probes

The manifest points **liveness at `GET /livez`** and **readiness at
`GET /health`**, and the split is load-bearing:

- `/livez` is static. It answers `{"status":"alive"}` for the process alone and
  touches neither the database nor NATS.
- `/health` pings the database and reports NATS connectivity, returning `503`
  when either is down.

Pointing liveness at `/health` — the intuitive thing to do, since it is the
endpoint that knows the most — turns a NATS or Oracle outage into a crash loop
across every replica. That takes the admin API's *read* paths down with it, at
exactly the moment an operator wants to look at configuration or turn a flag
off. A dependency outage should cost you traffic, not your pods.

---

## 6. Buckets (auto-created on startup)

`central-config` calls `CreateOrUpdateKeyValue` on boot, so `FLAGS`,
`MICROCONFIG` and `LOCALIZATION` are created idempotently with
`History=5`, file storage, `Replicas=NATS_REPLICAS` and a **512 KiB
`MaxValueSize`**. No manual step.

Two consequences worth knowing:

- **NATS being unreachable at startup is not fatal.** The service boots, serves
  reads from Oracle, publishes nothing, and sets `centralconfig_nats_up` to `0`;
  the reconciler re-provisions the buckets on its next cycle. The same call runs
  every cycle, so a NATS restart with an empty store heals itself rather than
  leaving every publish failing until the pods are restarted.
- **The value ceiling is applied on every startup and every cycle**, so a bucket
  provisioned by hand without it is corrected by the running service. Left
  unset, a bucket inherits the server's `max_payload` and an oversized payload
  becomes drift the reconciler can never heal.

Verify:

```bash
nats --server nats://localhost:4222 kv ls
nats --server nats://localhost:4222 kv info FLAGS
```

To create them manually (e.g. to pre-provision before first deploy):

```bash
nats kv add FLAGS        --history=5 --storage=file --replicas=3 --max-value-size=524288
nats kv add MICROCONFIG  --history=5 --storage=file --replicas=3 --max-value-size=524288
nats kv add LOCALIZATION --history=5 --storage=file --replicas=3 --max-value-size=524288
```

---

## 7. Wire up consuming services

Consumers import `pkg/configclient`. It warms an in-memory cache from KV and
keeps it live with a watch:

```go
cc, err := configclient.New(ctx, configclient.Options{
    NATSURL:        "nats://nats.messaging.svc.cluster.local:4222",
    NATSCreds:      "/etc/nats/nats.creds", // read-only creds
    EnvironmentID:  3,
    MicroserviceID: 1, // scopes the watch to this service's own keys
})
if err != nil { log.Fatal(err) }
defer cc.Close()

if cc.FlagEnabled("search_v2") { /* ... */ }
settings, _ := cc.MicroSettings(1)
title, _ := cc.Translate(1, "pt-BR", "catalog.title")
```

Mount the consumer's read-only creds secret — the one scoped to this service and
this environment, per §4 — and pass the environment and microservice ids as
deployment configuration rather than hardcoding them. `GET /inventory` on the
admin API lists every row with its id. Reads are served from memory (no per-read
network call) and survive NATS blips.

If you also configure `Options.HTTPFallback` so a cold start survives NATS being
down, note that it needs a bearer token of its own: the admin API authenticates
every route except `/health`, `/livez` and `/metrics`, and a token's environment
scope narrows reads as well as writes. Give the consumer a token scoped to its
environment and nothing more. `configclient` validates it in `New`, so a missing
one fails the deployment rather than surfacing as a `401` on the day JetStream
is already down.

For a consumer in any other language there is no shipped client, but the whole
contract is specified in [`CONSUMER_CONTRACT.md`](CONSUMER_CONTRACT.md).

---

## 8. Observability & operations

- **Health:** `GET /health` → `{"status","database","nats"}`; returns `503` if
  the database ping fails or NATS is disconnected. Readiness and alerting, not
  liveness — see §5.
- **Liveness:** `GET /livez` → `{"status":"alive"}`, always `200` while the
  process is up.
- **Metrics:** `GET /metrics`. The two to watch on a deployment day are
  `centralconfig_nats_up` and
  `centralconfig_reconcile_last_success_timestamp_seconds` — between them they
  say whether the distribution plane is up and whether it is converging.
  [`OBSERVABILITY.md`](OBSERVABILITY.md) §3 has the alert expressions.
- **Logs:** every failed publish logs `... (will reconcile)`; the reconciler logs
  how many keys it republishes each sweep, and names any key it could not
  publish.
- **JetStream monitoring:** `nats server report jetstream`, and scrape the NATS
  Prometheus exporter (enable `exporter` in the Helm values) for stream/consumer
  and storage metrics.
- **Backups:** Oracle is the system of record — standard Oracle backups cover
  correctness. KV can always be rebuilt by the reconciler. Optionally snapshot
  streams with `nats stream backup`.

---

## 9. Troubleshooting

| Symptom | Likely cause | Fix |
|---------|--------------|-----|
| `/health` returns 503, pods stay up | NATS unreachable / creds wrong / Oracle unreachable | check `NATS_URL`, creds secret, cluster pods, `CONN_STRING`. Pods staying up is correct — reads still work |
| Buckets not created | JetStream disabled or no write perms | verify `jetstream.enabled: true`; check writer perms on `$JS.API.>`. The reconciler retries every cycle, so fix it and wait rather than restarting |
| Consumer sees no values | wrong `EnvironmentID` / subject perms too narrow | confirm the ids against `GET /inventory`; check the consumer's subjects against §4 — a per-service credential that is one id off subscribes to nothing and reports no error |
| Every API call returns 401 | no token, or a token the deployment does not know | reads need one too; check `ADMIN_TOKEN`/`ADMIN_TOKENS` and that the client sends `Authorization: Bearer …` |
| A read returns 404 for a row that exists | the token is not scoped to that environment | out-of-scope reads answer `404` by design; widen the scope or use a token that covers it |
| Pod will not start, logs `parse admin tokens` | malformed `ADMIN_TOKENS` | a comma inside a secret, or a name with characters outside `A-Za-z0-9._-`. The message names the entry number, not the entry |
| `centralconfig_reconcile_prune_refused_total` climbing | a sweep proposed deleting more than `RECONCILE_PRUNE_MAX_FRACTION` of a bucket | nothing was deleted; find out why the database looked empty to that sweep before touching the fraction |
| Values stale after a NATS outage | publish dropped mid-outage | reconciler heals on next sweep; force restart to resync immediately |
| Publish errors in logs but writes succeed | KV write-through failing | expected to be self-healing; investigate if drift persists past one interval |
| A write returns 400 “exceeds the maximum value size” | payload over 512 KiB | correct — the ceiling matches the bucket's. Split the document; do not raise one side only |

---

## Appendix — teardown

```bash
kubectl -n central-config delete -f deploy/k8s/central-config.yaml
helm -n messaging uninstall nats
kubectl -n messaging delete pvc -l app.kubernetes.io/name=nats   # deletes JetStream data
```
