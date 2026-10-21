package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func req(remote, xff string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name string
		r    *http.Request
		hops int
		want string
	}{
		{"direct, header ignored when hops=0", req("203.0.113.9:5000", "1.2.3.4"), 0, "203.0.113.9"},
		{"auto: public peer cannot spoof", req("203.0.113.9:5000", "1.2.3.4"), AutoProxyHops, "203.0.113.9"},
		{"auto: private peer, one proxy", req("10.0.0.5:5000", "198.51.100.7"), AutoProxyHops, "198.51.100.7"},
		{"auto: client-forged prefix is ignored", req("10.0.0.5:5000", "6.6.6.6, 198.51.100.7"), AutoProxyHops, "198.51.100.7"},
		{"auto: skips internal hops from the right", req("10.0.0.5:5000", "198.51.100.7, 10.1.1.1"), AutoProxyHops, "198.51.100.7"},
		{"auto: loopback nginx", req("127.0.0.1:5000", "198.51.100.7"), AutoProxyHops, "198.51.100.7"},
		{"auto: cgnat peer", req("100.64.1.1:5000", "198.51.100.7"), AutoProxyHops, "198.51.100.7"},
		{"auto: no header falls back to peer", req("10.0.0.5:5000", ""), AutoProxyHops, "10.0.0.5"},
		{"auto: malformed entry falls back to peer", req("10.0.0.5:5000", "not-an-ip"), AutoProxyHops, "10.0.0.5"},
		{"hops=1 trusts last entry only", req("203.0.113.9:5000", "6.6.6.6, 198.51.100.7"), 1, "198.51.100.7"},
		{"hops=2 (CDN + LB)", req("203.0.113.9:5000", "198.51.100.7, 172.67.0.1"), 2, "198.51.100.7"},
		{"hops larger than chain clamps", req("203.0.113.9:5000", "198.51.100.7"), 3, "198.51.100.7"},
		{"ipv4-mapped ipv6 is unmapped", req("[::ffff:10.0.0.5]:5000", "::ffff:198.51.100.7"), AutoProxyHops, "198.51.100.7"},
	}
	for _, c := range cases {
		if got := ClientIP(c.r, c.hops); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter(60, 3) // 1 token/sec, burst 3
	clock := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return clock }

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d within burst should pass", i+1)
		}
	}
	ok, wait := l.Allow("a")
	if ok {
		t.Fatal("4th request should be limited")
	}
	if wait <= 0 || wait > time.Second {
		t.Errorf("retryAfter = %v, want within (0,1s]", wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Error("a different key has its own bucket")
	}

	clock = clock.Add(time.Second)
	if ok, _ := l.Allow("a"); !ok {
		t.Error("a token should have refilled after 1s")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Error("only one token should have refilled")
	}

	clock = clock.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatal("bucket should be full again, and never exceed burst")
		}
	}
	if ok, _ := l.Allow("a"); ok {
		t.Error("refill must be capped at burst")
	}
}

func TestLimiterRefund(t *testing.T) {
	l := NewLimiter(0.001, 2) // effectively no refill during the test
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("attempt %d should be allowed", i)
		}
	}
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("third attempt should be refused")
	}
	l.Refund("k")
	if ok, _ := l.Allow("k"); !ok {
		t.Error("a refunded token should allow one more attempt")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Error("a refund gives back one token, not more")
	}
	// Refunding more than was taken never lifts the bucket above its burst.
	for i := 0; i < 5; i++ {
		l.Refund("k")
	}
	allowed := 0
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("k"); ok {
			allowed++
		}
	}
	if allowed != 2 {
		t.Errorf("after over-refunding, %d attempts allowed; want the burst of 2", allowed)
	}
	l.Refund("never-seen") // must not panic or create state
}

func TestLimiterMemoryIsBounded(t *testing.T) {
	l := NewLimiter(1, 1)
	l.maxKeys = 100
	clock := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return clock }
	for i := 0; i < 10_000; i++ {
		l.Allow(string(rune('a'+i%26)) + time.Duration(i).String())
		if len(l.buckets) > l.maxKeys {
			t.Fatalf("bucket map grew to %d, cap is %d", len(l.buckets), l.maxKeys)
		}
	}
}

func TestByIPReturns429WithRetryAfter(t *testing.T) {
	l := NewLimiter(1, 1)
	h := l.ByIP(0, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req("203.0.113.9:1", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("first request = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req("203.0.113.9:2", ""))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("second request = %d, Retry-After=%q", rec.Code, rec.Header().Get("Retry-After"))
	}
}

func TestCORS(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := CORS([]string{"https://karena.example.com/"}, next)

	do := func(method, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	if rec := do("OPTIONS", "https://karena.example.com"); rec.Code != http.StatusNoContent ||
		rec.Header().Get("Access-Control-Allow-Origin") != "https://karena.example.com" {
		t.Errorf("allowed preflight: %d %v", rec.Code, rec.Header())
	}
	if rec := do("OPTIONS", "https://evil.example"); rec.Code != http.StatusForbidden ||
		rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("foreign preflight: %d %v", rec.Code, rec.Header())
	}
	rec := do("GET", "https://evil.example")
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("foreign origin must not receive an Allow-Origin header")
	}
	if rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Error("credentials must never be allowed")
	}
}

func TestRecover(t *testing.T) {
	h := Recover(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d", rec.Code)
	}
	if body := rec.Body.String(); len(body) > 40 {
		t.Errorf("panic detail leaked to client: %q", body)
	}
}
