package handlers

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"
	"turf-booking-backend/internal/ws"
)

// Every test here stops before touching MongoDB (the collection is nil), which
// is exactly the point: these are the checks that must reject a request before
// any state is read or written.
func newTestHandler(cfg *config.Config) *BookingHandler {
	return NewBookingHandler(nil, cfg, ws.NewPaymentHub())
}

func validBody(overrides map[string]string) string {
	fields := map[string]string{
		"userName":    "Wanjiru Njoroge",
		"email":       "wanjiru@example.com",
		"phoneNumber": "254712345678",
		"date":        schedule.Today(time.Now().AddDate(0, 0, 2)),
		"timeRange":   "20:00 – 22:00",
		"pitchType":   "5Aside",
	}
	for k, v := range overrides {
		fields[k] = v
	}
	var sb strings.Builder
	sb.WriteString("{")
	first := true
	for k, v := range fields {
		if !first {
			sb.WriteString(",")
		}
		first = false
		sb.WriteString(`"` + k + `":"` + v + `"`)
	}
	sb.WriteString("}")
	return sb.String()
}

func post(h http.HandlerFunc, contentType, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/initiate/paystack/push-stk", strings.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

func TestInitiateBookingRejectsBadRequestsBeforeTouchingTheDatabase(t *testing.T) {
	h := newTestHandler(&config.Config{})

	cases := []struct {
		name        string
		contentType string
		body        string
		want        int
	}{
		{"no content type", "", validBody(nil), http.StatusUnsupportedMediaType},
		{"text/plain (cross-site form trick)", "text/plain", validBody(nil), http.StatusUnsupportedMediaType},
		{"not json", "application/json", "{nope", http.StatusBadRequest},
		{"trailing second object", "application/json", validBody(nil) + validBody(nil), http.StatusBadRequest},
		{"oversized body", "application/json", `{"userName":"` + strings.Repeat("a", 10_000) + `"}`, http.StatusRequestEntityTooLarge},
		{"bad phone", "application/json", validBody(map[string]string{"phoneNumber": "12345"}), http.StatusBadRequest},
		{"foreign phone", "application/json", validBody(map[string]string{"phoneNumber": "14155550123"}), http.StatusBadRequest},
		{"bad email", "application/json", validBody(map[string]string{"email": "nope"}), http.StatusBadRequest},
		{"sms injection in name", "application/json", validBody(map[string]string{"userName": "Bob http://evil.example/pay"}), http.StatusBadRequest},
		{"unknown pitch", "application/json", validBody(map[string]string{"pitchType": "11Aside"}), http.StatusBadRequest},
		{"all-day booking at a 1hr price", "application/json", validBody(map[string]string{"timeRange": "00:00 – 23:59"}), http.StatusBadRequest},
		{"3 hour booking", "application/json", validBody(map[string]string{"timeRange": "16:00 – 19:00"}), http.StatusBadRequest},
		{"past date", "application/json", validBody(map[string]string{"date": "2020-01-01"}), http.StatusBadRequest},
		{"far-future date", "application/json", validBody(map[string]string{"date": "2999-01-01"}), http.StatusBadRequest},
		{"operator injection as date", "application/json", validBody(map[string]string{"date": `{"$gt":""}`}), http.StatusBadRequest},
	}
	for _, c := range cases {
		rec := post(h.InitiateBooking, c.contentType, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d (body %q)", c.name, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
		}
	}
}

func TestAdminOnly(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef"
	reached := false
	next := func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(http.StatusOK) }

	call := func(cfg *config.Config, header string) *httptest.ResponseRecorder {
		reached = false
		h := newTestHandler(cfg)
		r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/bookings", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		h.AdminOnly(next)(rec, r)
		return rec
	}

	enabled := &config.Config{AdminAPIKey: key}

	if rec := call(enabled, ""); rec.Code != http.StatusUnauthorized || reached {
		t.Errorf("no credentials: %d reached=%v", rec.Code, reached)
	}
	if rec := call(enabled, "Bearer wrong"); rec.Code != http.StatusUnauthorized || reached {
		t.Errorf("wrong key: %d reached=%v", rec.Code, reached)
	}
	if rec := call(enabled, key); rec.Code != http.StatusUnauthorized || reached {
		t.Errorf("key without Bearer scheme: %d reached=%v", rec.Code, reached)
	}
	if rec := call(enabled, "Bearer "); rec.Code != http.StatusUnauthorized || reached {
		t.Errorf("empty bearer: %d reached=%v", rec.Code, reached)
	}
	if rec := call(enabled, "Bearer "+key); rec.Code != http.StatusOK || !reached {
		t.Errorf("correct key: %d reached=%v", rec.Code, reached)
	}
	// An unset key must close the endpoint, not leave it open or accept "".
	if rec := call(&config.Config{}, "Bearer "); rec.Code != http.StatusServiceUnavailable || reached {
		t.Errorf("admin disabled + empty bearer: %d reached=%v", rec.Code, reached)
	}
	if rec := call(&config.Config{}, ""); rec.Code != http.StatusServiceUnavailable || reached {
		t.Errorf("admin disabled: %d reached=%v", rec.Code, reached)
	}
}

func sign(secret string, body string) string {
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestWebhookSignature(t *testing.T) {
	const secret = "sk_test_secret"
	h := newTestHandler(&config.Config{PaystackSK: secret})
	// An event that is not charge.success is handled without the database.
	body := `{"event":"refund.processed","data":{"reference":"abc","status":"x"}}`

	do := func(sig string, payload string) int {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/paystack", strings.NewReader(payload))
		if sig != "" {
			r.Header.Set("x-paystack-signature", sig)
		}
		rec := httptest.NewRecorder()
		h.PaystackWebhook(rec, r)
		return rec.Code
	}

	if code := do(sign(secret, body), body); code != http.StatusOK {
		t.Errorf("valid signature: %d", code)
	}
	if code := do("", body); code != http.StatusUnauthorized {
		t.Errorf("missing signature: %d", code)
	}
	if code := do("not-hex", body); code != http.StatusUnauthorized {
		t.Errorf("non-hex signature: %d", code)
	}
	if code := do(sign("another-secret", body), body); code != http.StatusUnauthorized {
		t.Errorf("signature from the wrong key: %d", code)
	}
	if code := do(sign(secret, body), body+" "); code != http.StatusUnauthorized {
		t.Errorf("signature over a different body: %d", code)
	}
	// The classic misconfiguration: anyone can sign with an empty key.
	if code := do(sign("", body), body); code != http.StatusUnauthorized {
		t.Errorf("signature made with an empty key must not verify against a real key: %d", code)
	}
}

func TestConfigValidateRefusesUnsafeSetups(t *testing.T) {
	if err := (&config.Config{}).Validate(); err == nil {
		t.Error("a missing PAYSTACK_SK must stop startup: an empty HMAC key can be forged by anyone")
	}
	if err := (&config.Config{PaystackSK: "sk", AdminAPIKey: "short"}).Validate(); err == nil {
		t.Error("a weak admin key must be refused")
	}
	if err := (&config.Config{PaystackSK: "sk", AllowedOrigins: []string{"http://evil.example"}}).Validate(); err == nil {
		t.Error("a plain-http non-local origin must be refused")
	}
	ok := &config.Config{
		PaystackSK:     "sk",
		AdminAPIKey:    strings.Repeat("k", 32),
		AllowedOrigins: []string{"https://karena.example.com", "http://localhost:3000"},
	}
	if err := ok.Validate(); err != nil {
		t.Errorf("a sound config was refused: %v", err)
	}
}

func TestValidReference(t *testing.T) {
	for _, s := range []string{strings.Repeat("a", 32), "507f1f77bcf86cd799439011"} {
		if !validReference(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []string{"", "abc", strings.Repeat("g", 32), strings.Repeat("a", 33), "../../etc/passwd", `{"$ne":""}`} {
		if validReference(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func TestNewReferenceIsUnguessable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		ref, err := newReference()
		if err != nil || !validReference(ref) || len(ref) != 32 {
			t.Fatalf("bad reference %q (%v)", ref, err)
		}
		if seen[ref] {
			t.Fatalf("duplicate reference %q", ref)
		}
		seen[ref] = true
	}
}

func TestToOccupancyFailsClosed(t *testing.T) {
	b := func(pitch, rng string) models.Booking {
		return models.Booking{BookingDetails: models.BookingDetails{PitchType: pitch, Time: rng}}
	}
	got := toOccupancy([]models.Booking{
		b("5Aside", "18:00 – 19:00"),
		b("8Aside", "18:00 – 20:00"),
		b("???", "20:00 – 21:00"), // unknown pitch: assume the whole pitch
		b("5Aside", "garbage"),    // unreadable time: assume the whole day
	}, "2026-10-14")

	want := []schedule.Occupancy{
		{Start: 18 * 60, End: 19 * 60, Units: 1},
		{Start: 18 * 60, End: 20 * 60, Units: 2},
		{Start: 20 * 60, End: 21 * 60, Units: 2},
		{Start: 0, End: 24 * 60, Units: 1},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if schedule.Fits(toOccupancy([]models.Booking{b("5Aside", "garbage")}, "d"), 20*60, 22*60, 2) {
		t.Error("an unreadable row must block an 8-aside on that date")
	}
}

func TestBookingsCache(t *testing.T) {
	var c bookingsCache
	calls := 0
	fill := func() ([]byte, error) { calls++; return []byte("[]"), nil }

	for i := 0; i < 5; i++ {
		if _, err := c.load(fill); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("5 reads made %d queries, want 1", calls)
	}
	c.invalidate()
	c.load(fill)
	if calls != 2 {
		t.Errorf("after a write the next read must hit the database; queries = %d", calls)
	}
}
