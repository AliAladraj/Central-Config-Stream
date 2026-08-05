package app

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// idleBucketTTL is how long a caller's bucket is kept after its last write.
// Without it the map grows once per distinct client address forever.
const idleBucketTTL = 10 * time.Minute

// rateLimiter is a per-caller token bucket over the write endpoints. Reads are
// not limited: they are cheap and already open.
//
// A nil limiter allows everything, which is what a non-positive configured rate
// means — the limiter is a safety net, not something that should be able to
// lock an operator out of production by misconfiguration.
type rateLimiter struct {
	capacity float64 // burst
	refill   float64 // tokens per second

	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time // swapped in tests
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

// allow spends one token for key. When the bucket is empty it reports the whole
// seconds a caller should wait, for the Retry-After header.
func (l *rateLimiter) allow(key string) (retryAfter int, ok bool) {
	if l == nil {
		return 0, true
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, seen := l.buckets[key]
	if !seen {
		l.sweep(now)
		b = &bucket{tokens: l.capacity, last: now}
		l.buckets[key] = b
	}

	b.tokens = math.Min(l.capacity, b.tokens+now.Sub(b.last).Seconds()*l.refill)
	b.last = now

	if b.tokens < 1 {
		wait := (1 - b.tokens) / l.refill
		return int(math.Ceil(wait)), false
	}
	b.tokens--
	return 0, true
}

// sweep drops buckets nobody has used recently. It runs only when a new caller
// appears, so a steady set of callers costs nothing.
func (l *rateLimiter) sweep(now time.Time) {
	for key, b := range l.buckets {
		if now.Sub(b.last) > idleBucketTTL {
			delete(l.buckets, key)
		}
	}
}

// rateLimitKey identifies the caller. The bearer token is preferred so one
// operator's script cannot spend another's budget, and it is hashed so the
// limiter never holds a credential in a long-lived map. Callers with no
// credential fall back to their address, which is what keeps an unauthenticated
// flood from reaching the token check at all.
func rateLimitKey(r *http.Request) string {
	if secret, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok && secret != "" {
		sum := sha256.Sum256([]byte(secret))
		return "t:" + hex.EncodeToString(sum[:8])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip:" + host
}
