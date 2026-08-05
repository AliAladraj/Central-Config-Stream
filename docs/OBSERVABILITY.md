# Observability

central-config emits ECS-shaped JSON logs on stdout and Prometheus metrics on
`GET /metrics`. Both are standard library only — there is no logging or metrics
dependency in `go.mod`, and adding one is not required to ship this to Elastic.

---

## 1. Logging

`log/slog` with a JSON handler whose built-in keys are remapped to their ECS
names (`internal/obs/log.go`). Every line goes to **stdout**, one JSON document
per line, which is what a container log shipper expects.

### Schema

| Field | Meaning |
| --- | --- |
| `@timestamp` | RFC 3339 event time |
| `log.level` | `debug` / `info` / `warn` / `error` |
| `message` | the event, as a short constant string — values live in fields, not in here |
| `service.name` | `SERVICE_NAME`, default `central-config` |
| `service.version` | `SERVICE_VERSION`, default `dev` |
| `event.dataset` | `central-config.<component>` — `http`, `app`, `startup`, `reconciler`, `flagsconfig`, `microconfig`, `localization`, `audit`, `security`, `health`, `inventory` |
| `error.message` | present on every failure line |
| `error.stack_trace` | present on a recovered handler panic |
| `http.request.id` | correlation id, on every line emitted while serving a request |

Access lines (`message: "http request"`, one per request) add
`http.request.method`, `url.path`, `http.route` (the matched route pattern),
`http.response.status_code`, `event.duration` (nanoseconds, per ECS),
`client.ip`, and `user.name` when the caller authenticated.

Domain lines add what the event is about: `flag.key`, `kv.key`, `bucket`,
`source`, `environment.id`, `microservice.id`, `locale`, `count`, `sweep`.

### Example lines

Sample output from `DB_DRIVER=sqlite PUBLISH_ENABLED=false go run ./cmd/central-config`:

```json
{"@timestamp":"2026-01-15T16:27:22.874Z","log.level":"info","message":"http request","service.name":"central-config","service.version":"dev","http.request.id":"demo-put-1","event.dataset":"central-config.http","http.request.method":"PUT","url.path":"/flags/values","http.route":"PUT /flags/values","http.response.status_code":200,"event.duration":8918200,"client.ip":"127.0.0.1","user.name":"shared"}
{"@timestamp":"2026-01-15T16:27:09.620Z","log.level":"warn","message":"running without KV distribution (dev mode)","service.name":"central-config","service.version":"dev","event.dataset":"central-config.app","publish.enabled":false}
```

A failure carries the error as a field rather than in the message, so failures
can be grouped:

```json
{"@timestamp":"...","log.level":"error","message":"publish flag failed (will reconcile)","event.dataset":"central-config.flagsconfig","http.request.id":"demo-put-1","flag.key":"search_v2","environment.id":1,"error.message":"messaging: kv put flag 1.search_v2: nats: no responders available for request"}
```

### Correlation

`observe` (`internal/app/observe.go`) is the outermost middleware. It honours an
inbound `X-Request-Id` (rejecting anything that is not a plain ≤64-character
token, so a header cannot forge a log record), generates one otherwise, echoes
it in the `X-Request-Id` response header, and puts a logger carrying it into the
request context. Handlers and services log through `obs.FromContext(ctx, …)`, so
every line for one request shares `http.request.id` — in Kibana, filter on it to
get the request's whole story including its audit failure or KV publish failure.

It also recovers a panicking handler into a `500` plus an `error.stack_trace`
line. Without that recovery, `net/http` drops the connection mid-response and
logs nothing you can act on.

### Volume

The reconciler can republish thousands of keys in a sweep, so per-key lines are
`debug` and the `info` lines carry counts (`source republished` with `count`,
`pruned stale keys` with `count`). `GET /health` and `GET /metrics` access lines
are `debug` too — an orchestrator probe every few seconds would otherwise be
most of the log volume.

### Environment variables

| Variable | Default | Effect |
| --- | --- | --- |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `json` | `json` = ECS for Elasticsearch; `text` = readable key=value for a terminal |
| `SERVICE_NAME` | `central-config` | `service.name` |
| `SERVICE_VERSION` | `dev` | `service.version`; set it to the image tag |

`deploy/compose/docker-compose.yml` sets `LOG_FORMAT=text` so `docker compose
logs` stays readable; `deploy/k8s/central-config.yaml` sets `LOG_FORMAT=json`.

### Shipping to Elastic

Nothing in the service talks to Elasticsearch — it writes to stdout and a
shipper does the rest.

**Filebeat** (`filebeat.yml`), reading the container's stdout:

```yaml
filebeat.inputs:
  - type: container
    paths:
      - /var/lib/docker/containers/*/*.log
    parsers:
      - ndjson:
          target: ""           # fields land at the document root, already ECS
          overwrite_keys: true
          add_error_key: true
processors:
  - add_kubernetes_metadata: ~
output.elasticsearch:
  hosts: ["https://elastic:9200"]
```

**Elastic Agent**: add the *Custom Logs* (or *Kubernetes*) integration, set the
dataset to `central-config`, and enable JSON parsing with the same
`target: ""` / `overwrite_keys: true` settings. Because the documents already
carry ECS field names, no ingest pipeline or field mapping is needed; `log.level`
and `@timestamp` are picked up as-is and `event.dataset` splits the streams.

Two things to keep in mind:
- `error.stack_trace` is multi-line *inside a JSON string*, so no multiline
  parser is needed. Do not enable one.
- `event.duration` is nanoseconds. Divide by 1e6 in Lens for milliseconds.

---

## 2. Metrics

`GET /metrics`, Prometheus text exposition format, unauthenticated like
`/health` (it carries counters, never configuration values). Backed by `expvar`
with a small renderer in `internal/obs/metrics.go`; the metric definitions are
in `internal/obs/instruments.go`.

### KV distribution

| Metric | Labels | Meaning |
| --- | --- | --- |
| `centralconfig_kv_publish_attempts_total` | `bucket` | write-through publishes attempted |
| `centralconfig_kv_publish_success_total` | `bucket` | publishes that reached JetStream |
| `centralconfig_kv_publish_failures_total` | `bucket` | publishes that failed — the admin write still succeeded, so this is invisible to the caller and to Oracle |
| `centralconfig_kv_publish_skipped_total` | `bucket` | publishes skipped because KV already held a byte-identical value — in steady state a full sweep should be almost entirely skips, since every real write pushes to every consumer |
| `centralconfig_kv_delete_failures_total` | `bucket` | key deletions that failed, i.e. a deleted row still being served to consumers |

### Reconciler

| Metric | Labels | Meaning |
| --- | --- | --- |
| `centralconfig_reconcile_cycles_total` | `sweep` (`full`/`incremental`), `result` (`ok`/`failed`) | cycles completed |
| `centralconfig_reconcile_duration_seconds` | `sweep` | histogram of cycle wall time |
| `centralconfig_reconcile_keys_republished_total` | `source` | keys pushed back to KV |
| `centralconfig_reconcile_keys_pruned_total` | `bucket` | KV keys deleted because their database row is gone — this is measured drift |
| `centralconfig_reconcile_source_failures_total` | `source` | a source could not be read or republished |
| `centralconfig_reconcile_last_success_timestamp_seconds` | — | unix time of the last cycle where every source succeeded; stays `0` when `PUBLISH_ENABLED=false`, because no reconciler runs |

### HTTP

| Metric | Labels | Meaning |
| --- | --- | --- |
| `centralconfig_http_requests_total` | `method`, `route`, `status` | `route` is the matched `ServeMux` pattern (`PUT /flags/values`), or `other` for an unmatched path, so a path scanner cannot blow up cardinality |
| `centralconfig_http_request_duration_seconds` | `method`, `route` | histogram, seconds |
| `centralconfig_http_panics_total` | `route` | recovered handler panics |

### Database

| Metric | Meaning |
| --- | --- |
| `centralconfig_db_up` | 1/0 from the last `/health` ping (Oracle in production, SQLite in the local stack) |
| `centralconfig_db_ping_failures_total` | failed health-check pings |

Note that `centralconfig_db_up` only moves when `/health` is called, which in
Kubernetes is the liveness probe. If nothing probes the service, it does not
refresh.

### Scraping

Kubernetes: `deploy/k8s/central-config.yaml` carries the
`prometheus.io/scrape`, `/port` and `/path` pod annotations. Anything else:
point the scraper at `http://<host>:8080/metrics`.

---

## 3. Alerts worth having

Three, in order of what they would actually catch.

**1. KV publishes are failing — consumers are drifting from Oracle.**

```promql
sum(rate(centralconfig_kv_publish_failures_total[5m]))
  / clamp_min(sum(rate(centralconfig_kv_publish_attempts_total[5m])), 1) > 0.05
for: 10m
```

This is the failure mode the reconciler exists to paper over, which is exactly
why it needs an alert: the admin write returns `200`, the audit trail records a
success, and consumers keep serving the old value. Without this, the first
symptom is a flag that "didn't take".

**2. The reconciler has stopped converging.**

```promql
time() - centralconfig_reconcile_last_success_timestamp_seconds
  > 3 * <RECONCILE_INTERVAL seconds>
for: 5m
```

The reconciler is the only thing that repairs alert 1. If both fire, KV is
frozen at whatever it last held. Scope this alert to instances with
`PUBLISH_ENABLED=true` — the gauge is legitimately `0` when publishing is off.

**3. Sustained drift pruning.**

```promql
sum(increase(centralconfig_reconcile_keys_pruned_total[1h])) > 0
```

A prune means a KV key outlived its database row: the delete-time purge did not
happen. One after a genuine deletion outage is fine; a steady trickle means the
write-through delete path is broken and consumers are being served rows that no
longer exist. Warning severity, not a page.

Worth graphing but not alerting: `centralconfig_http_request_duration_seconds`
p95 by route (Oracle latency shows up here first), and
`centralconfig_http_requests_total{status="401"}` (a rising 401 rate is either a
rotated token or somebody probing the admin API).

---

## 4. What is deliberately not instrumented

- **`pkg/configclient`** is imported by consuming services. It logs nothing
  by default; set `Options.Logger` to an `*slog.Logger` to get watch failures,
  malformed payloads and applied-update debug lines in the consumer's own log
  stream. Its `Status()` still reports the same information without a logger.
- **`cmd/testconsole`** is a laptop-only harness whose output is read in a
  terminal by the person who started it. It stays on the standard `log` package.
- **Repositories** do not log; they wrap errors with `fmt.Errorf("…: %w", err)`
  and the handler or service that decides what the failure means logs it once.
