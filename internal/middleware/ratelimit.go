package middleware

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Limiter is an in-memory token bucket per key. State is per process, which is
// enough for a single instance; run a shared limiter (e.g. at the proxy) if the
// API is ever scaled out.
type Limiter struct {
	mu        sync.Mutex
	rate      float64 // tokens added per second
	burst     float64
	maxKeys   int
	buckets   map[string]*bucket
	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter allows `burst` requests at once, refilling `perMinute` per minute.
func NewLimiter(perMinute float64, burst int) *Limiter {
	return &Limiter{
		rate:    perMinute / 60,
		burst:   float64(burst),
		maxKeys: 50_000,
		buckets: make(map[string]*bucket),
		now:     time.Now,
	}
}

// Allow takes one token for key. When it refuses, retryAfter says when the
// next token will exist.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if len(l.buckets) >= l.maxKeys || now.Sub(l.lastSweep) > time.Minute {
		l.sweep(now)
	}

	b, exists := l.buckets[key]
	if !exists {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	return false, wait
}

// Refund gives back a token taken by Allow, for work that failed before it had
// any effect on the caller (so the failure should not count against them).
func (l *Limiter) Refund(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		b.tokens = math.Min(l.burst, b.tokens+1)
	}
}

// sweep drops buckets that have refilled completely: recreating one later is
// indistinguishable from keeping it. Caller holds l.mu.
func (l *Limiter) sweep(now time.Time) {
	l.lastSweep = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, k)
		}
	}
	// Still full of actively throttled keys: make room rather than grow.
	for k := range l.buckets {
		if len(l.buckets) < l.maxKeys {
			break
		}
		delete(l.buckets, k)
	}
}

// ByIP rejects requests over the per-client-IP limit with 429.
func (l *Limiter) ByIP(proxyHops int, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.Allow(ClientIP(r, proxyHops)); !ok {
			TooManyRequests(w, wait)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TooManyRequests writes a 429 with Retry-After.
func TooManyRequests(w http.ResponseWriter, wait time.Duration) {
	TooManyRequestsMessage(w, wait, "Too many requests, please slow down")
}

// TooManyRequestsMessage is TooManyRequests with wording suited to the caller,
// for endpoints whose users are parents rather than programs.
func TooManyRequestsMessage(w http.ResponseWriter, wait time.Duration, message string) {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, message, http.StatusTooManyRequests)
}
