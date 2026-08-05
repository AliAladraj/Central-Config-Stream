package obs

import (
	"expvar"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The metrics tier is deliberately standard library only: expvar holds the
// values, and the registry below renders them in Prometheus text format on
// GET /metrics. Prometheus is what an operator already scrapes; expvar's own
// JSON is not something anything alerts on.

// collector is one rendered metric family.
type collector interface {
	writeTo(b *strings.Builder)
}

var registry struct {
	mu   sync.Mutex
	list []collector
}

func register(c collector) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.list = append(registry.list, c)
}

// Handler serves the registry in Prometheus text exposition format.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		registry.mu.Lock()
		for _, c := range registry.list {
			c.writeTo(&b)
		}
		registry.mu.Unlock()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}

// ---- counters ----

// Counter is a monotonic counter, optionally broken down by labels. Values live
// in an expvar.Map keyed by the rendered label set, which keeps the increment
// path lock-free per series.
type Counter struct {
	name, help string
	vals       *expvar.Map
}

func NewCounter(name, help string) *Counter {
	c := &Counter{name: name, help: help, vals: new(expvar.Map)}
	register(c)
	return c
}

// Add increments the series identified by the given label key/value pairs.
func (c *Counter) Add(n int64, labels ...string) {
	c.vals.Add(labelKey(labels), n)
}

// Inc adds one.
func (c *Counter) Inc(labels ...string) { c.Add(1, labels...) }

// Value reports a series' current total. Only tests need it.
func (c *Counter) Value(labels ...string) int64 {
	v, _ := c.vals.Get(labelKey(labels)).(*expvar.Int)
	if v == nil {
		return 0
	}
	return v.Value()
}

func (c *Counter) writeTo(b *strings.Builder) {
	writeHeader(b, c.name, c.help, "counter")
	c.vals.Do(func(kv expvar.KeyValue) {
		fmt.Fprintf(b, "%s%s %s\n", c.name, kv.Key, kv.Value.String())
	})
}

// ---- gauges ----

// Gauge is a single value that goes up and down (database reachable, time of
// the last successful reconcile).
type Gauge struct {
	name, help string
	v          expvar.Float
}

func NewGauge(name, help string) *Gauge {
	g := &Gauge{name: name, help: help}
	register(g)
	return g
}

func (g *Gauge) Set(f float64) { g.v.Set(f) }

// Value reports the current value. Only tests need it.
func (g *Gauge) Value() float64 { return g.v.Value() }

// SetBool records a state flag as the 1/0 an alert expression can compare.
func (g *Gauge) SetBool(ok bool) {
	if ok {
		g.Set(1)
		return
	}
	g.Set(0)
}

func (g *Gauge) writeTo(b *strings.Builder) {
	writeHeader(b, g.name, g.help, "gauge")
	fmt.Fprintf(b, "%s %s\n", g.name, g.v.String())
}

// ---- histograms ----

// defaultBuckets spans a database round trip to a stalled reconcile sweep.
var defaultBuckets = []float64{0.005, 0.025, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60}

// Histogram is a Prometheus histogram: bucket counts plus sum and count, so
// quantiles can be computed at query time.
type Histogram struct {
	name, help string
	bounds     []float64

	mu     sync.Mutex
	series map[string]*histSeries
}

type histSeries struct {
	counts []uint64
	sum    float64
	count  uint64
}

func NewHistogram(name, help string) *Histogram {
	h := &Histogram{name: name, help: help, bounds: defaultBuckets, series: map[string]*histSeries{}}
	register(h)
	return h
}

// Observe records one measurement, in seconds.
func (h *Histogram) Observe(seconds float64, labels ...string) {
	key := labelKey(labels)

	h.mu.Lock()
	defer h.mu.Unlock()

	s := h.series[key]
	if s == nil {
		s = &histSeries{counts: make([]uint64, len(h.bounds))}
		h.series[key] = s
	}
	s.sum += seconds
	s.count++
	for i, upper := range h.bounds {
		if seconds <= upper {
			s.counts[i]++
		}
	}
}

func (h *Histogram) writeTo(b *strings.Builder) {
	writeHeader(b, h.name, h.help, "histogram")

	h.mu.Lock()
	defer h.mu.Unlock()

	for _, key := range sortedKeys(h.series) {
		s := h.series[key]
		for i, upper := range h.bounds {
			fmt.Fprintf(b, "%s_bucket%s %d\n", h.name, withLabel(key, "le", formatFloat(upper)), s.counts[i])
		}
		fmt.Fprintf(b, "%s_bucket%s %d\n", h.name, withLabel(key, "le", "+Inf"), s.count)
		fmt.Fprintf(b, "%s_sum%s %s\n", h.name, key, formatFloat(s.sum))
		fmt.Fprintf(b, "%s_count%s %d\n", h.name, key, s.count)
	}
}

func sortedKeys(m map[string]*histSeries) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- label rendering ----

// labelKey renders key/value pairs into the `{a="1",b="2"}` suffix a series is
// identified by. Storing the rendered form means nothing has to be parsed back
// out at scrape time. An odd number of arguments is a programming error and is
// rendered rather than taking the process down.
func labelKey(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(labels); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(labels[i])
		b.WriteString(`="`)
		b.WriteString(escapeLabel(labels[i+1]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}

// withLabel appends one more label to an already-rendered label set, which is
// how the histogram adds its `le` bound.
func withLabel(key, name, value string) string {
	if key == "" {
		return fmt.Sprintf(`{%s="%s"}`, name, value)
	}
	return key[:len(key)-1] + fmt.Sprintf(`,%s="%s"}`, name, value)
}

// escapeLabel applies the exposition format's escaping. Label values here are
// bucket names, HTTP methods and route patterns, but a value is never trusted
// to be free of a quote.
func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func writeHeader(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

func formatFloat(f float64) string {
	if math.IsInf(f, 1) {
		return "+Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ---- instruments ----
//
// The rest of the catalogue lives in instruments.go; these two belong with it
// and are declared here only because that file is outside this change's reach.

var (
	// NATSUp separates "the distribution plane is down" from "the process is
	// down". A NATS outage no longer stops the service booting, so without a
	// gauge the degraded start is visible only in a log line at startup.
	NATSUp = NewGauge("centralconfig_nats_up",
		"1 when the KV buckets are provisioned and NATS is connected, 0 otherwise.")

	// ReconcilePruneRefused fires when a sweep proposed deleting more of a
	// bucket than the ceiling allows. It means KV and the database disagree
	// wholesale, which is worth an alert on the first occurrence.
	ReconcilePruneRefused = NewCounter("centralconfig_reconcile_prune_refused_total",
		"Prune passes refused because they would have deleted too much of a bucket, by bucket.")
)
