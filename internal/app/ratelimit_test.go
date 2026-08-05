package app

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The limiter used to key on the bearer token as sent, before anything had
// checked it. A caller inventing a new Authorization value per request drew a
// fresh full bucket every time — no limit at all — and left a map entry behind
// on the way past, each one costing a full scan of the map under one lock.
func TestDistinctBearerTokensDoNotEscapeTheLimiter(t *testing.T) {
	sec := &security{
		tokens:  mustTokens(t, "", "secret"),
		limiter: newRateLimiter(5),
	}
	handler := sec.limitWrites(http.HandlerFunc(okHandler))

	var limited, retryAfter int
	for i := 0; i < 40; i++ {
		req := httptest.NewRequest(http.MethodPost, "/flags", strings.NewReader("{}"))
		req.RemoteAddr = "203.0.113.7:40000"
		req.Header.Set("Authorization", fmt.Sprintf("Bearer invented-%d", i))

		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited++
			if rec.Header().Get("Retry-After") == "" {
				t.Fatal("429 without a Retry-After header")
			}
			retryAfter++
		}
	}

	if limited == 0 {
		t.Fatal("40 writes under 40 distinct invented tokens were all allowed")
	}
	if limited < 30 {
		t.Errorf("only %d of 40 writes were limited; the budget is 5 a minute", limited)
	}
	if retryAfter != limited {
		t.Errorf("%d of %d rejections carried a Retry-After", retryAfter, limited)
	}

	// A caller at another address still has its own budget.
	req := httptest.NewRequest(http.MethodPost, "/flags", strings.NewReader("{}"))
	req.RemoteAddr = "198.51.100.9:40000"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("a second address was charged the first one's budget: %d", rec.Code)
	}
}

// Reads change nothing and answer from a bounded page, so the write budget is
// not spent on them.
func TestEdgeLimiterLeavesReadsAlone(t *testing.T) {
	sec := &security{tokens: mustTokens(t, "", "secret"), limiter: newRateLimiter(1)}
	handler := sec.limitWrites(http.HandlerFunc(okHandler))

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/flags", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("read %d was rate limited: %d", i, rec.Code)
		}
	}
}

// The bucket map is what an attacker grows, so it has a ceiling: past it every
// new caller shares one bucket rather than allocating another.
func TestLimiterBucketMapIsBounded(t *testing.T) {
	l := newRateLimiter(120)
	for i := 0; i < maxBuckets*2; i++ {
		l.allow(fmt.Sprintf("ip:10.0.%d.%d", i/256, i%256))
	}

	l.mu.Lock()
	size := len(l.buckets)
	_, pooled := l.buckets[overflowKey]
	l.mu.Unlock()

	if size > maxBuckets+1 {
		t.Errorf("the map holds %d buckets, over the %d ceiling", size, maxBuckets)
	}
	if !pooled {
		t.Error("callers past the ceiling did not fall back to the shared bucket")
	}
}

// Sweeping on every unseen key made a flood pay for a full scan of a growing
// map per request. It runs on a schedule now, and still expires what is idle.
func TestLimiterSweepsOnASchedule(t *testing.T) {
	now := time.Now()
	l := newRateLimiter(120)
	l.now = func() time.Time { return now }

	l.allow("ip:10.0.0.1")

	// A second caller inside the interval must not trigger a scan.
	now = now.Add(time.Second)
	l.allow("ip:10.0.0.2")
	l.mu.Lock()
	swept := l.lastSweep
	l.mu.Unlock()

	// Past the idle TTL, the next new caller expires both of them.
	now = now.Add(idleBucketTTL + time.Minute)
	l.allow("ip:10.0.0.3")

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.lastSweep.Equal(swept) {
		t.Error("the scheduled sweep never ran")
	}
	if _, ok := l.buckets["ip:10.0.0.1"]; ok {
		t.Error("an idle bucket outlived its TTL")
	}
	if _, ok := l.buckets["ip:10.0.0.3"]; !ok {
		t.Error("the sweep took the caller that triggered it")
	}
}

// Once a credential is known to be one, the budget is charged to it, so one
// operator's script cannot spend another's.
func TestWriteBudgetIsPerCredentialAfterAuth(t *testing.T) {
	limiter := newRateLimiter(1)
	sec := &security{tokens: mustTokens(t, "alice:*:alpha,bob:*:beta", ""), limiter: limiter}
	h := sec.guard(classNeutral, "flags", okHandler)

	if got := call(t, h, http.MethodPost, "/flags", "Bearer alpha", "").Code; got != http.StatusOK {
		t.Fatalf("alice's first write rejected: %d", got)
	}
	if got := call(t, h, http.MethodPost, "/flags", "Bearer alpha", "").Code; got != http.StatusTooManyRequests {
		t.Errorf("alice's second write got %d, want 429", got)
	}
	if got := call(t, h, http.MethodPost, "/flags", "Bearer beta", "").Code; got != http.StatusOK {
		t.Errorf("bob was charged alice's budget: %d", got)
	}

	// An unknown credential is answered before any budget is charged to a name.
	if got := call(t, h, http.MethodPost, "/flags", "Bearer nope", "").Code; got != http.StatusUnauthorized {
		t.Errorf("an unknown token got %d, want 401", got)
	}
}
