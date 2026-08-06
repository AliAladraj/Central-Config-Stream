package obs

// The metrics this service exposes, defined in one place so the names an
// operator alerts on are visible together. See docs/OBSERVABILITY.md for what
// each one is for and which alerts are worth having.

// KV distribution. A publish failure is the drift the reconciler exists to
// heal; it is silent from the caller's side, so it has to be countable.
var (
	// NATSUp separates "the distribution plane is down" from "the process is
	// down". A NATS outage no longer stops the service booting, so without a
	// gauge the degraded start is visible only in a log line at startup.
	NATSUp = NewGauge("centralconfig_nats_up",
		"1 when the KV buckets are provisioned and NATS is connected, 0 otherwise.")
	KVPublishAttempts = NewCounter("centralconfig_kv_publish_attempts_total",
		"KV publish attempts, by bucket.")
	KVPublishSuccess = NewCounter("centralconfig_kv_publish_success_total",
		"KV publishes that reached JetStream, by bucket.")
	KVPublishFailures = NewCounter("centralconfig_kv_publish_failures_total",
		"KV publishes that failed and were left to the reconciler, by bucket.")
	KVPublishSkipped = NewCounter("centralconfig_kv_publish_skipped_total",
		"KV publishes skipped because the stored value was already identical, by bucket.")
	KVDeleteFailures = NewCounter("centralconfig_kv_delete_failures_total",
		"KV key deletions that failed, by bucket.")
)

// Reconciler. Its whole job is to converge KV on the database, so what matters is
// that cycles keep completing and how much drift each one had to repair.
var (
	ReconcileCycles = NewCounter("centralconfig_reconcile_cycles_total",
		"Reconcile cycles completed, by sweep (full/incremental) and result.")
	ReconcileDuration = NewHistogram("centralconfig_reconcile_duration_seconds",
		"Wall time of a reconcile cycle, by sweep.")
	ReconcileKeysRepublished = NewCounter("centralconfig_reconcile_keys_republished_total",
		"Keys republished from the database to KV, by source.")
	ReconcileKeysPruned = NewCounter("centralconfig_reconcile_keys_pruned_total",
		"KV keys deleted because their database row is gone, by bucket.")
	// ReconcilePruneRefused fires when a sweep proposed deleting more of a
	// bucket than the ceiling allows. It means KV and the database disagree
	// wholesale, which is worth an alert on the first occurrence.
	ReconcilePruneRefused = NewCounter("centralconfig_reconcile_prune_refused_total",
		"Prune passes refused because they would have deleted too much of a bucket, by bucket.")
	ReconcileSourceFailures = NewCounter("centralconfig_reconcile_source_failures_total",
		"Reconcile sources that failed to resync, by source.")
	ReconcileLastSuccess = NewGauge("centralconfig_reconcile_last_success_timestamp_seconds",
		"Unix time of the last reconcile cycle in which every source succeeded.")
)

// HTTP admin API.
var (
	HTTPRequests = NewCounter("centralconfig_http_requests_total",
		"HTTP requests, by method, matched route and status code.")
	HTTPDuration = NewHistogram("centralconfig_http_request_duration_seconds",
		"HTTP request duration, by method and matched route.")
	HTTPPanics = NewCounter("centralconfig_http_panics_total",
		"Requests whose handler panicked.")
)

// Database — PostgreSQL in production, SQLite in the local stack.
var (
	DBUp = NewGauge("centralconfig_db_up",
		"1 when the last health-check ping succeeded, 0 otherwise.")
	DBPingFailures = NewCounter("centralconfig_db_ping_failures_total",
		"Health-check database pings that failed.")
)
