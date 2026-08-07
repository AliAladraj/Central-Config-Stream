package app

import (
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// idleBucketTTL is how long a caller's bucket is kept after its last write.
	// Without it the map grows once per distinct client address forever.
	idleBucketTTL = 10 * time.Minute

	// sweepInterval bounds how often the expiry scan runs. Sweeping on every
	// unseen key instead makes a flood of distinct keys pay for a full scan of
	// the map per request, under the one lock, while the map it scans grows
	// with the flood — the cost rises with exactly the traffic the limiter is
	// there to absorb.
	sweepInterval = time.Minute

	// maxBuckets caps the map. Past it every new caller shares one bucket
	// rather than allocating another, so the limiter's memory is bounded by
	// configuration rather than by how many distinct callers an attacker can
	// present.
	maxBuckets = 8192
)

// overflowKey is the shared bucket used once the map is full. Pooling those
// callers' budgets is a real cost — a legitimate new caller arriving during a
// flood is charged against it too — and it is the right one: the alternative is
// a map that grows with the attack.
const overflowKey = "\x00overflow"

// rateLimiter is a per-caller token bucket over the write endpoints. Reads are
// not limited: they answer from a bounded page and change nothing.
//
// A nil limiter allows everything, which is what a non-positive configured rate
// means — the limiter is a safety net, not something that should be able to
// lock an operator out of production by misconfiguration.
type rateLimiter struct {
	capacity float64 // burst
	refill   float64 // tokens per second

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
	now       func() time.Time // swapped in tests
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter builds a limiter allowing perMinute writes per caller per
// minute, bursting up to a minute's worth. A non-positive rate disables it.
func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute <= 0 {
		return nil
	}
	return &rateLimiter{
		capacity: float64(perMinute),
		refill:   float64(perMinute) / 60,
		buckets:  make(map[string]*bucket),
		now:      time.Now,
	}
}

// anonWriteCost is what one credential-less write costs against its bucket. It
// is a divisor rather than a second limiter on purpose: the anonymous share
// then lives in the same map, under the same sweep and the same maxBuckets
// ceiling, so bounding what an unauthenticated flood can allocate did not have
// to be solved twice. Four means a credential-less caller gets a quarter of
// WRITE_RATE_LIMIT_PER_MINUTE, on top of rather than out of what an
// authenticated one at the same address may spend — so the worst an address can
// now extract from the edge is a quarter more work than before, in exchange for
// no amount of it being able to lock out a caller that holds a token. A quarter
// is far more than the zero legitimate writes a caller with no credential
// makes, and far less than enough to matter to the database.
const anonWriteCost = 4

// allow spends one token for key. When the bucket is empty it reports the whole
// seconds a caller should wait, for the Retry-After header.
func (l *rateLimiter) allow(key string) (retryAfter int, ok bool) {
	return l.allowCost(key, 1)
}

// allowAnonymous spends a write from a caller that presented no usable
// credential. The cost is capped at the whole budget so that a deployment
// configured down to a handful of writes a minute still admits one — a bucket
// nothing can ever afford is a 429 for every unauthenticated request, which is
// a different failure from the one this is defending against.
func (l *rateLimiter) allowAnonymous(key string) (retryAfter int, ok bool) {
	if l == nil {
		return 0, true
	}
	return l.allowCost(key, math.Min(anonWriteCost, l.capacity))
}

func (l *rateLimiter) allowCost(key string, cost float64) (retryAfter int, ok bool) {
	if l == nil {
		return 0, true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b := l.buckets[key]
	if b == nil {
		if now.Sub(l.lastSweep) >= sweepInterval {
			l.sweep(now)
			l.lastSweep = now
		}
		if len(l.buckets) >= maxBuckets {
			key = overflowKey
			b = l.buckets[key]
		}
		if b == nil {
			b = &bucket{tokens: l.capacity, last: now}
			l.buckets[key] = b
		}
	}

	b.tokens = math.Min(l.capacity, b.tokens+now.Sub(b.last).Seconds()*l.refill)
	b.last = now

	if b.tokens < cost {
		wait := (cost - b.tokens) / l.refill
		return int(math.Ceil(wait)), false
	}
	b.tokens -= cost
	return 0, true
}

// sweep drops buckets nobody has used recently. It runs at most once per
// sweepInterval, so a steady set of callers costs nothing and a flood of
// one-off keys costs one scan a minute rather than one per request.
func (l *rateLimiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if now.Sub(b.last) > idleBucketTTL {
			delete(l.buckets, key)
		}
	}
}

// addressKey identifies a caller that has not been authenticated yet. It is
// deliberately not the bearer token: the limiter runs before the token check,
// so keying on an unvalidated header hands every request that invents a new
// Authorization value a fresh full bucket — which is no limit at all, and a map
// entry per request besides.
//
// It is the peer address and not a forwarded-for header, which is caller input
// and would put the spoofable key straight back. Behind a proxy that means one
// bucket for everything arriving through it, so WRITE_RATE_LIMIT_PER_MINUTE is
// the fleet's shared edge budget there rather than one operator's; the
// per-credential budget the guard charges is what separates operators.
func addressKey(r *http.Request) string {
	return "ip:" + peerHost(r)
}

func peerHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// anonymousKey identifies an address that has presented no usable credential.
// It is a separate namespace from addressKey rather than the same one, so that
// a flood of credential-less writes cannot spend the budget an authenticated
// caller at the same address is about to need — which behind an ingress, where
// RemoteAddr is the proxy for every operator at once, is the whole point.
func anonymousKey(r *http.Request) string {
	return "anon:" + peerHost(r)
}

// tokenKey identifies a caller whose credential has already been matched, so
// one operator's script cannot spend another's budget. The name is a configured
// label rather than the secret, so nothing sensitive lands in a long-lived map.
func tokenKey(name string) string {
	return "t:" + name
}

// inventoryKey meters the one read that is not bounded by a page. It is its own
// namespace so that polling /inventory does not eat the credential's write
// budget — an operator locked out of a flag flip by a dashboard refreshing in
// the background would be a worse failure than the one this bounds. The address
// is the fallback for a deployment running with auth disabled, where every
// caller is nameless.
func inventoryKey(name string, r *http.Request) string {
	if name == "" {
		return "inv:" + peerHost(r)
	}
	return "inv:t:" + name
}
