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

Give `central-config` **write** on the config buckets and consumers **read/watch
only**. With JetStream KV, a bucket `FLAGS` is backed by stream `KV_FLAGS` and
subjects `$KV.FLAGS.>`.

Minimal split using two users in the same account (for full isolation use
separate accounts + `nsc`). Example permissions:

```
# central-config (writer)
publish:   "$KV.FLAGS.>", "$KV.MICROCONFIG.>", "$KV.LOCALIZATION.>",
           "$JS.API.>"
subscribe: "_INBOX.>"

# consumers (reader/watcher)
publish:   "$JS.API.STREAM.INFO.*", "$JS.API.CONSUMER.CREATE.*",
           "$JS.API.DIRECT.GET.*"
subscribe: "$KV.FLAGS.>", "$KV.MICROCONFIG.>", "$KV.LOCALIZATION.>", "_INBOX.>"
```

Generate NKEY/JWT creds with `nsc` (recommended) or issue static creds, then
store them as secrets:

```bash
kubectl -n central-config create secret generic nats-creds \
  --from-file=nats.creds=./central-config.creds

# in each consumer's namespace:
kubectl create secret generic nats-consumer-creds \
  --from-file=nats.creds=./consumer.creds
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
| `ADMIN_TOKEN` | from secret | bearer token for write endpoints |

Liveness/readiness use `GET /health`, which reports NATS connectivity
(`503` when NATS is down).

---

## 6. Buckets (auto-created on startup)

`central-config` calls `CreateOrUpdateKeyValue` on boot, so `FLAGS`,
`MICROCONFIG` and `LOCALIZATION` are created idempotently with
`History=5`, file storage, `Replicas=NATS_REPLICAS`. No manual step. Verify:

```bash
nats --server nats://localhost:4222 kv ls
nats --server nats://localhost:4222 kv info FLAGS
```

To create them manually (e.g. to pre-provision before first deploy):

```bash
nats kv add FLAGS        --history=5 --storage=file --replicas=3
nats kv add MICROCONFIG  --history=5 --storage=file --replicas=3
nats kv add LOCALIZATION --history=5 --storage=file --replicas=3
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

Mount the consumer's read-only creds secret and pass the environment and
microservice ids as deployment configuration rather than hardcoding them —
`GET /inventory` on the admin API lists every row with its id. Reads are served
from memory (no per-read network call) and survive NATS blips.

For a consumer in any other language there is no shipped client, but the whole
contract is specified in [`CONSUMER_CONTRACT.md`](CONSUMER_CONTRACT.md).

---

## 8. Observability & operations

- **Health:** `GET /health` → `{"status","database","nats"}`; returns `503` if
  NATS is disconnected. Use it for k8s probes and alerting.
- **Logs:** every failed publish logs `... (will reconcile)`; the reconciler logs
  how many keys it republishes each sweep.
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
| `/health` returns 503 | NATS unreachable / creds wrong | check `NATS_URL`, creds secret, cluster pods |
| Buckets not created | JetStream disabled or no write perms | verify `jetstream.enabled: true`; check writer perms on `$JS.API.>` |
| Consumer sees no values | wrong `EnvironmentID` / read perms | confirm it watches `{env}.>`; check reader perms on `$KV.*.>` |
| Values stale after a NATS outage | publish dropped mid-outage | reconciler heals on next sweep; force restart to resync immediately |
| Publish errors in logs but writes succeed | KV write-through failing | expected to be self-healing; investigate if drift persists past one interval |

---

## Appendix — teardown

```bash
kubectl -n central-config delete -f deploy/k8s/central-config.yaml
helm -n messaging uninstall nats
kubectl -n messaging delete pvc -l app.kubernetes.io/name=nats   # deletes JetStream data
```
