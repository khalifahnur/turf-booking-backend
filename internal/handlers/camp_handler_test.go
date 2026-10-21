package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"turf-booking-backend/internal/camp"
	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"
	"turf-booking-backend/internal/services"
	"turf-booking-backend/internal/ws"
)

// memCampStore is an in-memory CampStore that follows the same rules as the
// MongoDB one, so the handler's business logic can be tested without a database.
type memCampStore struct {
	mu   sync.Mutex
	regs []models.CampRegistration

	// latency imitates a database round trip. With none, the gap between the
	// duplicate check and the insert is microseconds wide and a missing lock
	// would go unnoticed.
	latency time.Duration
}

func (m *memCampStore) Conflicts(_ context.Context, childKey, phone string, weeks []string, pendingAfter time.Time) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var match []models.CampRegistration
	for _, r := range m.regs {
		if r.Child.Key != childKey || r.Parent.PhoneNumber != phone {
			continue
		}
		if r.Status == camp.StatusPaid || (r.Status == camp.StatusPending && !r.CreatedAt.Before(pendingAfter)) {
			match = append(match, r)
		}
	}
	out := overlap(weeks, match)
	m.mu.Unlock()
	time.Sleep(m.latency)
	m.mu.Lock() // re-taken for the deferred Unlock
	return out, nil
}

func (m *memCampStore) Insert(_ context.Context, reg models.CampRegistration) error {
	time.Sleep(m.latency)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.regs = append(m.regs, reg)
	return nil
}

func (m *memCampStore) MarkPaid(_ context.Context, ref string, cents int64, now time.Time) (*models.CampRegistration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.regs {
		if m.regs[i].Payment.Reference == ref && m.regs[i].Status != camp.StatusPaid {
			if cents > 0 && cents < int64(m.regs[i].AmountKES)*100 {
				return nil, nil // underpaid: the same guard as markPaidFilter's $expr
			}
			m.regs[i].Status = camp.StatusPaid
			m.regs[i].Payment.PaidAmountCents = cents
			m.regs[i].Payment.PaidAt = &now
			cp := m.regs[i]
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *memCampStore) MarkFailed(_ context.Context, ref string, _ time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.regs {
		if m.regs[i].Payment.Reference == ref && m.regs[i].Status == camp.StatusPending {
			m.regs[i].Status = camp.StatusFailed
			return true, nil
		}
	}
	return false, nil
}

func (m *memCampStore) FlagDuplicates(_ context.Context, childKey, phone string, weeks []string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var idx []int
	for i, r := range m.regs {
		if r.Child.Key == childKey && r.Parent.PhoneNumber == phone && r.Status == camp.StatusPaid && len(overlap(weeks, []models.CampRegistration{r})) > 0 {
			idx = append(idx, i)
		}
	}
	if len(idx) >= 2 {
		for _, i := range idx {
			m.regs[i].Duplicate = true
		}
	}
	return len(idx), nil
}

func (m *memCampStore) Status(_ context.Context, ref string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.regs {
		if r.Payment.Reference == ref {
			return r.Status, nil
		}
	}
	return "", errUnknownReference
}

func (m *memCampStore) ListForAdmin(context.Context) ([]models.CampRegistration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []models.CampRegistration
	for _, r := range m.regs {
		if r.Status == camp.StatusPaid || r.Status == camp.StatusPending {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memCampStore) all() []models.CampRegistration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]models.CampRegistration(nil), m.regs...)
}

type payCall struct {
	phone, email string
	cents        int
	ref          string
}

type campRig struct {
	h     *CampHandler
	store *memCampStore
	hub   *ws.PaymentHub

	mu       sync.Mutex
	payCalls []payCall
	payErr   error
	sms      chan models.CampRegistration
	told     []string
	now      time.Time
}

// A Wednesday before the camp starts.
var campNow = time.Date(2026, 10, 21, 12, 0, 0, 0, schedule.Location())

func newCampRig() *campRig {
	rig := &campRig{store: &memCampStore{}, hub: ws.NewPaymentHub(), sms: make(chan models.CampRegistration, 16), now: campNow}
	h := NewCampHandler(rig.store, &config.Config{AdminAPIKey: strings.Repeat("k", 32)}, rig.hub)
	h.now = func() time.Time { return rig.now }
	h.pay = func(_ *config.Config, phone, email string, cents int, ref string) (string, error) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.payCalls = append(rig.payCalls, payCall{phone, email, cents, ref})
		return "ok", rig.payErr
	}
	h.sendSMS = func(_ *config.Config, reg models.CampRegistration) error {
		rig.sms <- reg
		return nil
	}
	h.notify = func(ref, status string) {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		rig.told = append(rig.told, ref+"="+status)
	}
	h.phoneLimiter = middleware.NewLimiter(6000, 1000) // out of the way unless a test wants it
	rig.h = h
	return rig
}

func (r *campRig) calls() []payCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]payCall(nil), r.payCalls...)
}

type body map[string]any

func goodBody(over body) body {
	b := body{
		"childName":     "Amani Otieno",
		"parentName":    "Grace Otieno",
		"email":         "grace@example.com",
		"phoneNumber":   "254712345678",
		"program":       "fullDay",
		"lunch":         false,
		"weeks":         []string{"2026-11-02"},
		"acceptedTerms": true,
		"termsVersion":  camp.TermsVersion,
	}
	for k, v := range over {
		b[k] = v
	}
	return b
}

func (r *campRig) register(b body) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(b)
	return r.registerRaw("application/json", string(raw))
}

func (r *campRig) registerRaw(contentType, raw string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/camp/register", strings.NewReader(raw))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	r.h.Register(rec, req)
	return rec
}

func TestCampRegisterRejectsBadRequests(t *testing.T) {
	cases := []struct {
		name string
		over body
		want int
	}{
		{"child name missing", body{"childName": ""}, 400},
		{"child name has markup", body{"childName": "<b>x</b>"}, 400},
		{"parent name too long", body{"parentName": strings.Repeat("a", 80)}, 400},
		{"sms injection via child name", body{"childName": "Amani http://evil.example/pay"}, 400},
		{"bad email", body{"email": "nope"}, 400},
		{"foreign phone", body{"phoneNumber": "14155550123"}, 400},
		{"short phone", body{"phoneNumber": "712345"}, 400},
		{"unknown programme", body{"program": "boarding"}, 400},
		{"no weeks", body{"weeks": []string{}}, 400},
		{"a day that is not a week start", body{"weeks": []string{"2026-11-03"}}, 400},
		{"a week outside the camp", body{"weeks": []string{"2027-01-04"}}, 400},
		{"terms not accepted", body{"acceptedTerms": false}, 400},
		{"stale terms version", body{"termsVersion": "camp-2025-v0"}, 409},
		{"missing terms version", body{"termsVersion": ""}, 409},
	}
	for _, c := range cases {
		rig := newCampRig()
		rec := rig.register(goodBody(c.over))
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d (%q)", c.name, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
		}
		if n := len(rig.store.all()); n != 0 {
			t.Errorf("%s: %d registration(s) stored, want none", c.name, n)
		}
		if n := len(rig.calls()); n != 0 {
			t.Errorf("%s: payment prompt sent for a rejected request", c.name)
		}
	}

	rig := newCampRig()
	if rec := rig.registerRaw("", `{}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("no content type = %d", rec.Code)
	}
	if rec := rig.registerRaw("text/plain", `{}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d", rec.Code)
	}
	if rec := rig.registerRaw("application/json", `{nope`); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d", rec.Code)
	}
	if rec := rig.registerRaw("application/json", `{"childName":"`+strings.Repeat("a", 20_000)+`"}`); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized = %d", rec.Code)
	}
	raw, _ := json.Marshal(goodBody(nil))
	if rec := rig.registerRaw("application/json", string(raw)+string(raw)); rec.Code != http.StatusBadRequest {
		t.Errorf("trailing JSON = %d", rec.Code)
	}
}

func TestCampRegisterHappyPath(t *testing.T) {
	rig := newCampRig()
	rec := rig.register(goodBody(body{
		"childName":   "  amani   OTIENO ",
		"phoneNumber": "0712 345 678",
		"program":     "halfDay",
		"lunch":       true,
		"weeks":       []string{"2026-11-09", "2026-11-02", "2026-11-09"},
	}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Reference string `json:"reference"`
		Total     int    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	// Half day Ksh 1,500 + lunch Ksh 500 = 2,000 a week, two distinct weeks.
	if resp.Total != 4_000 {
		t.Errorf("total = %d, want 4000", resp.Total)
	}
	if !camp.IsReference(resp.Reference) || !validReference(resp.Reference) {
		t.Errorf("reference %q is not a valid camp reference", resp.Reference)
	}

	regs := rig.store.all()
	if len(regs) != 1 {
		t.Fatalf("%d registrations stored", len(regs))
	}
	r := regs[0]
	if r.Status != camp.StatusPending || r.AmountKES != 4_000 || r.PerWeekKES != 2_000 || !r.Lunch || r.Program != "halfDay" {
		t.Errorf("stored registration = %+v", r)
	}
	if strings.Join(r.Weeks, ",") != "2026-11-02,2026-11-09" {
		t.Errorf("weeks = %v", r.Weeks)
	}
	if r.Child.FullName != "amani OTIENO" || r.Child.Key != "amani otieno" {
		t.Errorf("child = %+v", r.Child)
	}
	if r.Parent.PhoneNumber != "254712345678" {
		t.Errorf("phone not normalised: %q", r.Parent.PhoneNumber)
	}
	if r.Consent.TermsVersion != camp.TermsVersion || !r.Consent.AcceptedAt.Equal(campNow) {
		t.Errorf("consent not recorded: %+v", r.Consent)
	}
	if r.Payment.Reference != resp.Reference {
		t.Error("stored reference differs from the one returned")
	}

	calls := rig.calls()
	if len(calls) != 1 || calls[0].cents != 400_000 || calls[0].ref != resp.Reference || calls[0].phone != "254712345678" {
		t.Errorf("payment prompt = %+v", calls)
	}
}

// The browser must not be able to influence what it is charged.
func TestCampRegisterIgnoresClientSuppliedPrices(t *testing.T) {
	rig := newCampRig()
	rec := rig.register(goodBody(body{"amount": 1, "total": 1, "amountKes": 1, "perWeekKes": 1, "price": 1, "status": "Paid"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d", rec.Code)
	}
	r := rig.store.all()[0]
	if r.AmountKES != 2_500 || r.Status != camp.StatusPending {
		t.Errorf("client-supplied fields leaked into the registration: %+v", r)
	}
	if c := rig.calls(); len(c) != 1 || c[0].cents != 250_000 {
		t.Errorf("charged %+v, want 250000 cents", c)
	}
}

func TestCampRegisterDuplicateProtection(t *testing.T) {
	rig := newCampRig()
	first := rig.register(goodBody(nil))
	if first.Code != http.StatusCreated {
		t.Fatalf("first = %d", first.Code)
	}

	// Double-click while the prompt may still be live.
	if rec := rig.register(goodBody(nil)); rec.Code != http.StatusConflict {
		t.Errorf("immediate repeat = %d, want 409", rec.Code)
	}
	if n := len(rig.calls()); n != 1 {
		t.Errorf("%d prompts sent, want 1", n)
	}

	// After the prompt window a parent whose payment did not complete can retry.
	rig.now = campNow.Add(campPendingWindow + time.Second)
	if rec := rig.register(goodBody(nil)); rec.Code != http.StatusCreated {
		t.Errorf("retry after the window = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	// Once paid, the same child cannot be bought again for that week...
	var resp struct{ Reference string }
	json.Unmarshal(first.Body.Bytes(), &resp)
	if reg, _ := rig.store.MarkPaid(context.Background(), resp.Reference, 250_000, rig.now); reg == nil {
		t.Fatal("could not mark paid")
	}
	rig.now = campNow.Add(24 * time.Hour)
	if rec := rig.register(goodBody(nil)); rec.Code != http.StatusConflict {
		t.Errorf("paid week bought again = %d, want 409", rec.Code)
	}
	if rec := rig.register(goodBody(body{"weeks": []string{"2026-11-02", "2026-11-09"}})); rec.Code != http.StatusConflict {
		t.Errorf("overlapping weeks = %d, want 409", rec.Code)
	} else if !strings.Contains(rec.Body.String(), "week 1") {
		t.Errorf("conflict message should name the week: %q", rec.Body.String())
	}
	// ...but other weeks, other children and other parents are fine.
	if rec := rig.register(goodBody(body{"weeks": []string{"2026-11-09"}})); rec.Code != http.StatusCreated {
		t.Errorf("a different week = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := rig.register(goodBody(body{"childName": "Baraka Otieno"})); rec.Code != http.StatusCreated {
		t.Errorf("a sibling = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := rig.register(goodBody(body{"phoneNumber": "254733000111"})); rec.Code != http.StatusCreated {
		t.Errorf("a different parent phone = %d: %s", rec.Code, rec.Body.String())
	}
	// The child's name is matched without regard to case.
	if rec := rig.register(goodBody(body{"childName": "AMANI otieno"})); rec.Code != http.StatusConflict {
		t.Errorf("same child, different capitals = %d, want 409", rec.Code)
	}
}

// Many identical submissions at once must produce exactly one registration.
func TestCampRegisterConcurrentDuplicatesCreateOne(t *testing.T) {
	rig := newCampRig()
	rig.store.latency = 5 * time.Millisecond
	const n = 25
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = rig.register(goodBody(nil)).Code
		}(i)
	}
	wg.Wait()

	created, conflicts := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusConflict:
			conflicts++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if created != 1 || conflicts != n-1 {
		t.Errorf("created=%d conflicts=%d, want 1 and %d", created, conflicts, n-1)
	}
	if got := len(rig.store.all()); got != 1 {
		t.Errorf("%d registrations stored, want 1", got)
	}
	if got := len(rig.calls()); got != 1 {
		t.Errorf("%d payment prompts sent, want 1", got)
	}
}

func TestCampRegisterPaymentGatewayFailure(t *testing.T) {
	rig := newCampRig()
	rig.payErr = errors.New("paystack down")
	rec := rig.register(goodBody(nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "paystack down") {
		t.Error("gateway error detail leaked to the client")
	}
	if r := rig.store.all()[0]; r.Status != camp.StatusFailed {
		t.Errorf("status = %s, want Failed", r.Status)
	}
	// A failed attempt must not block an immediate retry.
	rig.payErr = nil
	if rec := rig.register(goodBody(nil)); rec.Code != http.StatusCreated {
		t.Errorf("retry after a gateway failure = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestCampRegisterPerPhoneRateLimit(t *testing.T) {
	rig := newCampRig()
	rig.h.phoneLimiter = middleware.NewLimiter(0.3, 3)
	names := []string{"Amani Otieno", "Baraka Otieno", "Chebet Otieno", "Daudi Otieno"}
	var codes []int
	for _, n := range names {
		codes = append(codes, rig.register(goodBody(body{"childName": n})).Code)
	}
	if codes[0] != 201 || codes[1] != 201 || codes[2] != 201 || codes[3] != http.StatusTooManyRequests {
		t.Errorf("codes = %v, want three 201s then 429", codes)
	}
	rec := rig.register(goodBody(body{"childName": "Daudi Otieno"}))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "payment attempts") {
		t.Errorf("limit response = %d %q (Retry-After %q): want a plain-language 429 with Retry-After",
			rec.Code, rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	// A different phone has its own allowance.
	if rec := rig.register(goodBody(body{"childName": "Ema Wanjiru", "phoneNumber": "254799000111"})); rec.Code != 201 {
		t.Errorf("another phone = %d", rec.Code)
	}
}

func (r *campRig) webhook(event, ref string, cents int64) *httptest.ResponseRecorder {
	return r.webhookEvent(paystackEvent{Event: event, Reference: ref, Status: "success", Currency: "KES", AmountCents: cents})
}

func (r *campRig) webhookEvent(ev paystackEvent) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.h.HandlePaystackEvent(rec, httptest.NewRequest(http.MethodPost, "/webhook", nil), ev)
	return rec
}

// notified returns what the parent's open status socket was told, in order.
func (r *campRig) notified() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.told...)
}

func TestCampPaymentSettlement(t *testing.T) {
	rig := newCampRig()
	var resp struct{ Reference string }
	json.Unmarshal(rig.register(goodBody(body{"lunch": true})).Body.Bytes(), &resp)

	if status, err := rig.h.PaymentStatus(context.Background(), resp.Reference); err != nil || status != "Pending" {
		t.Errorf("before payment: %q, %v", status, err)
	}

	if rec := rig.webhook("charge.success", resp.Reference, 300_000); rec.Code != http.StatusOK {
		t.Fatalf("webhook = %d", rec.Code)
	}
	r := rig.store.all()[0]
	if r.Status != camp.StatusPaid || r.Payment.PaidAmountCents != 300_000 || r.Payment.PaidAt == nil {
		t.Errorf("not settled: %+v", r)
	}
	select {
	case got := <-rig.sms:
		if got.Child.FullName != "Amani Otieno" || got.AmountKES != 3_000 {
			t.Errorf("sms for %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no confirmation SMS sent")
	}
	if status, _ := rig.h.PaymentStatus(context.Background(), resp.Reference); status != "Completed" {
		t.Errorf("after payment: %q", status)
	}

	// Paystack redelivers webhooks; the parent must not get a second SMS.
	if rec := rig.webhook("charge.success", resp.Reference, 300_000); rec.Code != http.StatusOK {
		t.Errorf("redelivery = %d", rec.Code)
	}
	select {
	case <-rig.sms:
		t.Error("a redelivered webhook sent a second SMS")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestCampWebhookEdgeCases(t *testing.T) {
	rig := newCampRig()
	var resp struct{ Reference string }
	json.Unmarshal(rig.register(goodBody(nil)).Body.Bytes(), &resp)

	if rec := rig.webhook("charge.success", "camp-"+strings.Repeat("0", 32), 250_000); rec.Code != http.StatusOK {
		t.Errorf("unknown reference = %d, want 200 so Paystack stops retrying", rec.Code)
	}
	if rec := rig.webhook("refund.processed", resp.Reference, 0); rec.Code != http.StatusOK {
		t.Errorf("other event = %d", rec.Code)
	}
	if r := rig.store.all()[0]; r.Status != camp.StatusPending {
		t.Errorf("a non-success event changed the status to %s", r.Status)
	}
	select {
	case <-rig.sms:
		t.Error("SMS sent without a successful charge")
	case <-time.After(100 * time.Millisecond):
	}

	// A payment that lands after the prompt was marked expired still counts.
	rig.store.mu.Lock()
	rig.store.regs[0].Status = camp.StatusExpired
	rig.store.mu.Unlock()
	if rec := rig.webhook("charge.success", resp.Reference, 250_000); rec.Code != http.StatusOK {
		t.Fatalf("late payment = %d", rec.Code)
	}
	if r := rig.store.all()[0]; r.Status != camp.StatusPaid {
		t.Errorf("a late payment was lost: status %s", r.Status)
	}
}

func TestCampAdminList(t *testing.T) {
	rig := newCampRig()
	var resp struct{ Reference string }
	json.Unmarshal(rig.register(goodBody(body{"lunch": true, "weeks": []string{"2026-11-02", "2026-11-09"}})).Body.Bytes(), &resp)
	rig.webhook("charge.success", resp.Reference, 600_000)
	<-rig.sms
	rig.register(goodBody(body{"childName": "Baraka Otieno"})) // stays pending

	call := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/camp/registrations", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		rig.h.AdminOnly(rig.h.ListAdmin)(rec, req)
		return rec
	}

	for _, auth := range []string{"", "Bearer wrong", strings.Repeat("k", 32)} {
		if rec := call(auth); rec.Code != http.StatusUnauthorized {
			t.Errorf("auth %q = %d, want 401", auth, rec.Code)
		}
	}

	rec := call("Bearer " + strings.Repeat("k", 32))
	if rec.Code != http.StatusOK {
		t.Fatalf("authorised = %d", rec.Code)
	}
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("%d rows, want 2", len(rows))
	}
	var paid map[string]any
	for _, r := range rows {
		if r["status"] == camp.StatusPaid {
			paid = r
		}
	}
	if paid == nil {
		t.Fatal("no paid row")
	}
	if paid["childName"] != "Amani Otieno" || paid["parentName"] != "Grace Otieno" || paid["phoneNumber"] != "254712345678" ||
		paid["email"] != "grace@example.com" || paid["programLabel"] != "Full day" || paid["lunch"] != true ||
		paid["amount"] != float64(6_000) || paid["paidAt"] == nil || paid["termsVersion"] != camp.TermsVersion {
		t.Errorf("paid row = %v", paid)
	}
	if ws, _ := paid["weeks"].([]any); len(ws) != 2 {
		t.Errorf("weeks = %v", paid["weeks"])
	}
	// The payment reference is a capability for the status socket: it must not
	// be handed out, even to the dashboard.
	if strings.Contains(rec.Body.String(), resp.Reference) || strings.Contains(rec.Body.String(), `"reference"`) {
		t.Error("the payment reference leaked into the admin listing")
	}

	empty := newCampRig()
	rec = httptest.NewRecorder()
	empty.h.ListAdmin(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("empty list should be [] not %q", rec.Body.String())
	}
}

// ---- the shared webhook and payment socket ----

func sharedHandlers(rig *campRig) *BookingHandler {
	// A nil bookings collection: any attempt to touch it would panic, which is
	// the point of these tests.
	b := NewBookingHandler(nil, &config.Config{PaystackSK: "sk_test_secret"}, rig.hub)
	b.AttachCamp(rig.h)
	return b
}

func TestSharedWebhookRoutesCampPaymentsAwayFromBookings(t *testing.T) {
	rig := newCampRig()
	var resp struct{ Reference string }
	json.Unmarshal(rig.register(goodBody(nil)).Body.Bytes(), &resp)
	b := sharedHandlers(rig)

	payload := `{"event":"charge.success","data":{"reference":"` + resp.Reference + `","status":"success","amount":250000}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/paystack", strings.NewReader(payload))
	req.Header.Set("x-paystack-signature", sign("sk_test_secret", payload))
	rec := httptest.NewRecorder()
	b.PaystackWebhook(rec, req) // would panic on the nil bookings collection if misrouted
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook = %d", rec.Code)
	}
	if r := rig.store.all()[0]; r.Status != camp.StatusPaid || r.Payment.PaidAmountCents != 250_000 {
		t.Errorf("registration after webhook: %+v", r)
	}

	// Unsigned or wrongly signed events must not settle anything.
	rig2 := newCampRig()
	var resp2 struct{ Reference string }
	json.Unmarshal(rig2.register(goodBody(nil)).Body.Bytes(), &resp2)
	b2 := sharedHandlers(rig2)
	payload2 := `{"event":"charge.success","data":{"reference":"` + resp2.Reference + `","amount":250000}}`
	for _, sig := range []string{"", "zz", sign("another-secret", payload2)} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/paystack", strings.NewReader(payload2))
		if sig != "" {
			req.Header.Set("x-paystack-signature", sig)
		}
		rec := httptest.NewRecorder()
		b2.PaystackWebhook(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("signature %q = %d, want 401", sig, rec.Code)
		}
	}
	if r := rig2.store.all()[0]; r.Status != camp.StatusPending {
		t.Errorf("a forged webhook settled the registration: %s", r.Status)
	}
}

func TestSharedWebhookWithoutCampHandlerDoesNotTouchBookings(t *testing.T) {
	b := NewBookingHandler(nil, &config.Config{PaystackSK: "sk_test_secret"}, ws.NewPaymentHub())
	payload := `{"event":"charge.success","data":{"reference":"camp-` + strings.Repeat("a", 32) + `","amount":250000}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/paystack", strings.NewReader(payload))
	req.Header.Set("x-paystack-signature", sign("sk_test_secret", payload))
	rec := httptest.NewRecorder()
	b.PaystackWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("= %d", rec.Code)
	}
}

func TestPaymentStatusDispatchesToCamp(t *testing.T) {
	rig := newCampRig()
	var resp struct{ Reference string }
	json.Unmarshal(rig.register(goodBody(nil)).Body.Bytes(), &resp)
	b := sharedHandlers(rig)

	if status, err := b.paymentStatus(resp.Reference); err != nil || status != "Pending" {
		t.Errorf("known camp reference: %q, %v", status, err)
	}
	if _, err := b.paymentStatus("camp-" + strings.Repeat("0", 32)); !errors.Is(err, errUnknownReference) {
		t.Errorf("unknown camp reference: %v", err)
	}
	bare := NewBookingHandler(nil, &config.Config{}, ws.NewPaymentHub())
	if _, err := bare.paymentStatus(resp.Reference); !errors.Is(err, errUnknownReference) {
		t.Errorf("camp reference with no camp handler: %v", err)
	}
}

func TestCampReferencesAreValidForTheStatusSocket(t *testing.T) {
	ref, err := newCampReference()
	if err != nil || !validReference(ref) || !camp.IsReference(ref) {
		t.Fatalf("bad camp reference %q (%v)", ref, err)
	}
	for _, bad := range []string{"camp-", "camp-xyz", "camp-" + strings.Repeat("g", 32), "camp-camp-" + strings.Repeat("a", 32), "CAMP-" + strings.Repeat("a", 32)} {
		if validReference(bad) {
			t.Errorf("%q should not be a valid reference", bad)
		}
	}
}

// A parent who is turned away (already registered, or a typo) has not tried to
// pay, so those attempts must not use up the per-phone allowance.
func TestCampRefusedAttemptsDoNotUseTheRateLimit(t *testing.T) {
	rig := newCampRig()
	rig.h.phoneLimiter = middleware.NewLimiter(0.3, 2)
	if rec := rig.register(goodBody(nil)); rec.Code != http.StatusCreated {
		t.Fatalf("first = %d", rec.Code)
	}
	for i := 0; i < 10; i++ {
		if rec := rig.register(goodBody(nil)); rec.Code != http.StatusConflict {
			t.Fatalf("repeat %d = %d, want 409 (not 429)", i, rec.Code)
		}
		if rec := rig.register(goodBody(body{"email": "nope"})); rec.Code != http.StatusBadRequest {
			t.Fatalf("typo %d = %d, want 400", i, rec.Code)
		}
	}
	if rec := rig.register(goodBody(body{"childName": "Baraka Otieno"})); rec.Code != http.StatusCreated {
		t.Errorf("second real attempt = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// The two minutes are the whole point of the window, so the boundary is checked
// against literal durations rather than the constant it is derived from.
func TestCampPendingWindowBoundary(t *testing.T) {
	rig := newCampRig()
	if rec := rig.register(goodBody(nil)); rec.Code != http.StatusCreated {
		t.Fatalf("first = %d", rec.Code)
	}
	rig.now = campNow.Add(119 * time.Second)
	if rec := rig.register(goodBody(nil)); rec.Code != http.StatusConflict {
		t.Errorf("at 119s the prompt may still be live: got %d, want 409", rec.Code)
	}
	rig.now = campNow.Add(121 * time.Second)
	if rec := rig.register(goodBody(nil)); rec.Code != http.StatusCreated {
		t.Errorf("at 121s a retry is allowed: got %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

func TestCampErrorMessagesAreSentences(t *testing.T) {
	for _, over := range []body{
		{"childName": ""}, {"parentName": ""}, {"email": "nope"}, {"phoneNumber": "123"},
		{"program": "x"}, {"weeks": []string{}}, {"acceptedTerms": false}, {"termsVersion": "old"},
	} {
		rec := newCampRig().register(goodBody(over))
		msg := strings.TrimSpace(rec.Body.String())
		if msg == "" || msg[0] < 'A' || msg[0] > 'Z' {
			t.Errorf("%v: message %q should start with a capital letter", over, msg)
		}
	}
}

func TestCampServerErrorsReassureAboutCharges(t *testing.T) {
	rig := newCampRig()
	rig.payErr = errors.New("down")
	rec := rig.register(goodBody(nil))
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "not been charged") {
		t.Errorf("gateway failure = %d %q", rec.Code, rec.Body.String())
	}
}

func (r *campRig) registerAndGetRef(t *testing.T, over body) string {
	t.Helper()
	rec := r.register(goodBody(over))
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct{ Reference string }
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Reference
}

// Paystack reports what it collected. Less than the price must not buy a place.
func TestCampUnderpaymentDoesNotSettle(t *testing.T) {
	rig := newCampRig()
	ref := rig.registerAndGetRef(t, nil)

	if rec := rig.webhook("charge.success", ref, 100_000); rec.Code != http.StatusOK {
		t.Fatalf("underpaid webhook = %d, want 200 so Paystack stops retrying", rec.Code)
	}
	if r := rig.store.all()[0]; r.Status != camp.StatusPending || r.Payment.PaidAt != nil {
		t.Errorf("an underpayment settled the registration: %+v", r)
	}
	select {
	case <-rig.sms:
		t.Error("confirmation SMS sent for an underpayment")
	case <-time.After(100 * time.Millisecond):
	}
	for _, n := range rig.notified() {
		if strings.HasSuffix(n, "=Completed") {
			t.Errorf("parent told the payment completed: %v", rig.notified())
		}
	}

	// Exactly the price, an overpayment, and an unknown amount all settle.
	for _, cents := range []int64{250_000, 300_000, 0} {
		rig := newCampRig()
		ref := rig.registerAndGetRef(t, nil)
		rig.webhook("charge.success", ref, cents)
		if r := rig.store.all()[0]; r.Status != camp.StatusPaid {
			t.Errorf("paying %d cents left the registration %s", cents, r.Status)
		}
	}
}

func TestCampWebhookIgnoresWrongCurrencyAndStatus(t *testing.T) {
	for name, ev := range map[string]paystackEvent{
		"USD":                             {Event: "charge.success", Status: "success", Currency: "USD", AmountCents: 250_000},
		"success event but failed status": {Event: "charge.success", Status: "failed", Currency: "KES", AmountCents: 250_000},
	} {
		rig := newCampRig()
		ev.Reference = rig.registerAndGetRef(t, nil)
		if rec := rig.webhookEvent(ev); rec.Code != http.StatusOK {
			t.Errorf("%s: webhook = %d", name, rec.Code)
		}
		if r := rig.store.all()[0]; r.Status != camp.StatusPending {
			t.Errorf("%s: registration became %s", name, r.Status)
		}
	}
	// Currency and status are optional in the payload; their absence is fine.
	rig := newCampRig()
	ref := rig.registerAndGetRef(t, nil)
	rig.webhookEvent(paystackEvent{Event: "charge.success", Reference: ref, AmountCents: 250_000})
	if r := rig.store.all()[0]; r.Status != camp.StatusPaid {
		t.Errorf("a charge.success without currency/status = %s, want Paid", r.Status)
	}
}

func TestCampFailureEvents(t *testing.T) {
	rig := newCampRig()
	ref := rig.registerAndGetRef(t, nil)

	// Events that are not about the charge must not tell the parent it failed.
	for _, e := range []string{"refund.processed", "transfer.success", "customeridentification.success"} {
		if rec := rig.webhookEvent(paystackEvent{Event: e, Reference: ref}); rec.Code != http.StatusOK {
			t.Errorf("%s = %d", e, rec.Code)
		}
	}
	if len(rig.notified()) != 0 || rig.store.all()[0].Status != camp.StatusPending {
		t.Fatalf("unrelated events changed state: told %v, status %s", rig.notified(), rig.store.all()[0].Status)
	}

	// A declined charge fails the registration and tells the parent once.
	if rec := rig.webhookEvent(paystackEvent{Event: "charge.failed", Reference: ref}); rec.Code != http.StatusOK {
		t.Fatalf("charge.failed = %d", rec.Code)
	}
	if rig.store.all()[0].Status != camp.StatusFailed {
		t.Errorf("status = %s, want Failed", rig.store.all()[0].Status)
	}
	if got := rig.notified(); len(got) != 1 || got[0] != ref+"=Failed" {
		t.Errorf("told %v", got)
	}
	if status, _ := rig.h.PaymentStatus(context.Background(), ref); status != "Failed" {
		t.Errorf("PaymentStatus = %q", status)
	}
	// A redelivery repeats the message in case the socket missed it.
	rig.webhookEvent(paystackEvent{Event: "charge.failed", Reference: ref})
	if got := rig.notified(); len(got) != 2 {
		t.Errorf("redelivered failure told %v", got)
	}

	// A late success still settles a failed registration.
	rig.webhook("charge.success", ref, 250_000)
	if rig.store.all()[0].Status != camp.StatusPaid {
		t.Errorf("a late success after a failure left %s", rig.store.all()[0].Status)
	}

	// ...and a stray failure event can never undo a paid registration or alarm the parent.
	before := len(rig.notified())
	rig.webhookEvent(paystackEvent{Event: "charge.failed", Reference: ref})
	if rig.store.all()[0].Status != camp.StatusPaid {
		t.Errorf("a failure event changed a paid registration to %s", rig.store.all()[0].Status)
	}
	if len(rig.notified()) != before {
		t.Errorf("a paid registration was announced as failed: %v", rig.notified())
	}
}

func TestCampPaymentStatusMapping(t *testing.T) {
	rig := newCampRig()
	ref := rig.registerAndGetRef(t, nil)
	for status, want := range map[string]string{
		camp.StatusPending: "Pending",
		camp.StatusPaid:    "Completed",
		camp.StatusFailed:  "Failed",
		camp.StatusExpired: "Failed",
	} {
		rig.store.mu.Lock()
		rig.store.regs[0].Status = status
		rig.store.mu.Unlock()
		if got, err := rig.h.PaymentStatus(context.Background(), ref); err != nil || got != want {
			t.Errorf("%s -> %q (%v), want %q", status, got, err, want)
		}
	}
}

// A parent who retries while the first prompt is still open can end up paying
// twice. Both payments are real money; the owner has to see it.
func TestCampDuplicatePaymentIsFlagged(t *testing.T) {
	rig := newCampRig()
	first := rig.registerAndGetRef(t, nil)
	rig.now = campNow.Add(3 * time.Minute) // past the window: the retry is allowed
	second := rig.registerAndGetRef(t, nil)

	rig.webhook("charge.success", first, 250_000)
	if r := rig.store.all()[0]; r.Duplicate {
		t.Error("a single payment was flagged as a duplicate")
	}
	rig.webhook("charge.success", second, 250_000)
	for i, r := range rig.store.all() {
		if r.Status != camp.StatusPaid || !r.Duplicate {
			t.Errorf("registration %d = status %s duplicate %v, want both paid and flagged", i, r.Status, r.Duplicate)
		}
	}

	rec := httptest.NewRecorder()
	rig.h.ListAdmin(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var rows []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row["duplicate"] != true {
			t.Errorf("admin row not flagged: %v", row)
		}
	}

	// A sibling paying for the same week is not a duplicate.
	other := newCampRig()
	a := other.registerAndGetRef(t, nil)
	b := other.registerAndGetRef(t, body{"childName": "Baraka Otieno"})
	other.webhook("charge.success", a, 250_000)
	other.webhook("charge.success", b, 250_000)
	for _, r := range other.store.all() {
		if r.Duplicate {
			t.Errorf("sibling flagged as duplicate: %+v", r)
		}
	}
}

// roundTripFunc stands in for the network, so the production SMS closure can be
// driven all the way to the request Africa's Talking would receive.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The SMS the parent receives must say what they bought, for when, and what they
// paid. This goes through NewCampHandler's own closure; the handler tests above
// replace it, so a mistake in its wiring would otherwise go unnoticed.
func TestCampConfirmationSMSAsSent(t *testing.T) {
	var sent url.Values
	prev := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		sent, _ = url.ParseQuery(string(raw))
		return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})
	defer func() { http.DefaultTransport = prev }()

	h := NewCampHandler(&memCampStore{}, &config.Config{ATUsername: "sandbox", ATAPIKey: "k"}, ws.NewPaymentHub())
	reg := models.CampRegistration{
		Child:     models.CampChild{FullName: "Amani Otieno"},
		Parent:    models.CampParent{PhoneNumber: "254712345678"},
		Program:   "fullDay",
		Lunch:     true,
		Weeks:     []string{"2026-11-09", "2026-11-02"},
		AmountKES: 6000,
	}
	if err := h.sendSMS(h.Config, reg); err != nil {
		t.Fatal(err)
	}
	msg := sent.Get("message")
	for _, want := range []string{"Amani Otieno", "Full day 8:30am - 4:00pm + lunch", "2 Nov - 13 Nov", "Paid Ksh 6,000", "https://maps.app.goo.gl"} {
		if !strings.Contains(msg, want) {
			t.Errorf("SMS %q is missing %q", msg, want)
		}
	}
	if sent.Get("to") != "+254712345678" {
		t.Errorf("sent to %q", sent.Get("to"))
	}

	reg.Lunch = false
	reg.Program = "halfDay"
	h.sendSMS(h.Config, reg)
	if msg := sent.Get("message"); !strings.Contains(msg, "Half day 8:30am - 12:30pm,") || strings.Contains(msg, "lunch") {
		t.Errorf("half day without lunch: %q", msg)
	}
	// A programme this build does not know must not panic or send an empty blurb.
	reg.Program = "retired"
	h.sendSMS(h.Config, reg)
	if msg := sent.Get("message"); !strings.Contains(msg, "retired") {
		t.Errorf("unknown programme: %q", msg)
	}
}

// When Paystack may have sent the prompt but we did not hear back, the parent
// must not be told they were not charged, and must not be able to send a second
// prompt straight away.
func TestCampUncertainPaymentKeepsTheRegistrationPending(t *testing.T) {
	rig := newCampRig()
	rig.payErr = fmt.Errorf("%w: api request failed: timeout", services.ErrPaymentUncertain)
	rec := rig.register(goodBody(nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "not been charged") || strings.Contains(body, "timeout") {
		t.Errorf("message %q must neither promise no charge nor leak the cause", body)
	}
	if !strings.Contains(body, "if a prompt arrived") || !strings.Contains(body, "2 minutes") {
		t.Errorf("message %q should say what to check and how long to wait", body)
	}
	if r := rig.store.all()[0]; r.Status != camp.StatusPending {
		t.Errorf("status = %s; an uncertain payment must stay Pending", r.Status)
	}

	// The prompt may be live: no second one inside the window...
	rig.payErr = nil
	if rec := rig.register(goodBody(nil)); rec.Code != http.StatusConflict {
		t.Errorf("immediate retry = %d, want 409", rec.Code)
	}
	if n := len(rig.calls()); n != 1 {
		t.Errorf("%d prompts sent, want 1", n)
	}
	// ...and if the parent did pay, the pending registration still settles.
	ref := rig.store.all()[0].Payment.Reference
	rig.webhook("charge.success", ref, 250_000)
	if r := rig.store.all()[0]; r.Status != camp.StatusPaid {
		t.Errorf("a payment after an uncertain failure left %s", r.Status)
	}

	// After the window, a parent who got nothing can try again.
	rig2 := newCampRig()
	rig2.payErr = fmt.Errorf("%w: timeout", services.ErrPaymentUncertain)
	rig2.register(goodBody(nil))
	rig2.payErr = nil
	rig2.now = campNow.Add(121 * time.Second)
	if rec := rig2.register(goodBody(nil)); rec.Code != http.StatusCreated {
		t.Errorf("retry after the window = %d: %s", rec.Code, rec.Body.String())
	}
}

// Failures that happen before anything reaches the parent's phone must not use
// up the per-phone allowance, or a short outage locks families out.
func TestCampFailedAttemptsGiveBackTheRateLimitToken(t *testing.T) {
	rig := newCampRig()
	rig.h.phoneLimiter = middleware.NewLimiter(0.001, 2)
	rig.payErr = errors.New("paystack refused")
	for i := 0; i < 6; i++ {
		if rec := rig.register(goodBody(nil)); rec.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d = %d, want 502 (never 429)", i, rec.Code)
		}
	}

	// An unsure failure may have sent a prompt, so it keeps counting.
	rig = newCampRig()
	rig.h.phoneLimiter = middleware.NewLimiter(0.001, 2)
	rig.payErr = fmt.Errorf("%w: timeout", services.ErrPaymentUncertain)
	var codes []int
	for i := 0; i < 3; i++ {
		rig.now = rig.now.Add(3 * time.Minute) // outside the pending window each time
		codes = append(codes, rig.register(goodBody(nil)).Code)
	}
	if codes[0] != 502 || codes[1] != 502 || codes[2] != http.StatusTooManyRequests {
		t.Errorf("uncertain attempts = %v, want 502, 502, 429", codes)
	}

	// A database failure on insert is refunded too.
	rig = newCampRig()
	rig.h.phoneLimiter = middleware.NewLimiter(0.001, 1)
	rig.h.Store = &failingInsertStore{memCampStore: rig.store}
	for i := 0; i < 3; i++ {
		if rec := rig.register(goodBody(nil)); rec.Code != http.StatusInternalServerError {
			t.Fatalf("insert failure %d = %d, want 500 (never 429)", i, rec.Code)
		}
	}
}

type failingConflictsStore struct{ *memCampStore }

func (failingConflictsStore) Conflicts(context.Context, string, string, []string, time.Time) ([]string, error) {
	return nil, errors.New("db down")
}

type failingInsertStore struct{ *memCampStore }

func (failingInsertStore) Insert(context.Context, models.CampRegistration) error {
	return errors.New("disk full")
}

func TestCampSuccessAnnouncesCompletedAndFailureVariants(t *testing.T) {
	rig := newCampRig()
	ref := rig.registerAndGetRef(t, nil)
	rig.webhook("charge.success", ref, 250_000)
	if got := rig.notified(); len(got) != 1 || got[0] != ref+"=Completed" {
		t.Errorf("a successful payment told the parent %v", got)
	}
	<-rig.sms

	// Failure events are recognised by their status as well as by their name.
	for name, ev := range map[string]paystackEvent{
		"status failed":    {Event: "paymentrequest.error", Status: "failed"},
		"status abandoned": {Event: "charge.timeout", Status: "abandoned"},
		"status reversed":  {Event: "charge.reversed", Status: "Reversed"},
		"name only":        {Event: "charge.failed"},
	} {
		rig := newCampRig()
		ev.Reference = rig.registerAndGetRef(t, nil)
		rig.webhookEvent(ev)
		if rig.store.all()[0].Status != camp.StatusFailed {
			t.Errorf("%s: status = %s, want Failed", name, rig.store.all()[0].Status)
		}
	}

	// A registration whose prompt expired is still told about a later failure.
	rig = newCampRig()
	ref = rig.registerAndGetRef(t, nil)
	rig.store.mu.Lock()
	rig.store.regs[0].Status = camp.StatusExpired
	rig.store.mu.Unlock()
	rig.webhookEvent(paystackEvent{Event: "charge.failed", Reference: ref})
	if got := rig.notified(); len(got) != 1 || got[0] != ref+"=Failed" {
		t.Errorf("an expired registration's failure told the parent %v", got)
	}
	if rig.store.all()[0].Status != camp.StatusExpired {
		t.Error("a failure event must not rewrite an expired registration")
	}
}

// The whole signed payload, as Paystack sends it, through the shared webhook.
func TestSharedWebhookCarriesStatusAndCurrency(t *testing.T) {
	post := func(rig *campRig, payload string) {
		b := sharedHandlers(rig)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/paystack", strings.NewReader(payload))
		req.Header.Set("x-paystack-signature", sign("sk_test_secret", payload))
		rec := httptest.NewRecorder()
		b.PaystackWebhook(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook = %d", rec.Code)
		}
	}
	rig := newCampRig()
	ref := rig.registerAndGetRef(t, nil)
	post(rig, `{"event":"charge.success","data":{"reference":"`+ref+`","status":"success","currency":"USD","amount":250000}}`)
	if rig.store.all()[0].Status != camp.StatusPending {
		t.Error("a payment in the wrong currency settled the registration")
	}
	post(rig, `{"event":"charge.success","data":{"reference":"`+ref+`","status":"success","currency":"KES","amount":100000}}`)
	if rig.store.all()[0].Status != camp.StatusPending {
		t.Error("an underpayment through the real webhook settled the registration")
	}
	post(rig, `{"event":"charge.failed","data":{"reference":"`+ref+`","status":"failed","currency":"KES","amount":250000}}`)
	if rig.store.all()[0].Status != camp.StatusFailed {
		t.Errorf("a failure through the real webhook left %s", rig.store.all()[0].Status)
	}
	post(rig, `{"event":"charge.success","data":{"reference":"`+ref+`","status":"success","currency":"KES","amount":250000}}`)
	if r := rig.store.all()[0]; r.Status != camp.StatusPaid || r.Payment.PaidAmountCents != 250000 {
		t.Errorf("the full price through the real webhook = %+v", r)
	}
}

// Every refusal Register writes is meant for a parent, so each carries the marker
// that lets the frontend show it as it is (a proxy's error page never does).
func TestCampRegisterErrorsAreMarkedForParents(t *testing.T) {
	rig := newCampRig()
	rig.h.phoneLimiter = middleware.NewLimiter(0.001, 1)
	check := func(name string, rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		if rec.Code != want {
			t.Errorf("%s: status = %d, want %d", name, rec.Code, want)
		}
		if rec.Header().Get(middleware.UserMessageHeader) != "1" {
			t.Errorf("%s: response carries no %s marker (%q)", name, middleware.UserMessageHeader, rec.Body.String())
		}
	}
	check("validation", rig.register(goodBody(body{"email": "nope"})), 400)
	check("terms", rig.register(goodBody(body{"acceptedTerms": false})), 400)
	check("stale terms", rig.register(goodBody(body{"termsVersion": "old"})), 409)
	check("media type", rig.registerRaw("text/plain", "{}"), 415)
	check("bad json", rig.registerRaw("application/json", "{"), 400)
	check("too large", rig.registerRaw("application/json", `{"childName":"`+strings.Repeat("a", 20_000)+`"}`), 413)
	if rec := rig.register(goodBody(nil)); rec.Code != 201 {
		t.Fatalf("first = %d", rec.Code)
	}
	check("duplicate", rig.register(goodBody(nil)), 409)
	check("rate limit", rig.register(goodBody(body{"childName": "Baraka Otieno"})), 429)

	rig = newCampRig()
	rig.payErr = errors.New("refused")
	check("definite pay failure", rig.register(goodBody(nil)), 502)
	rig = newCampRig()
	rig.payErr = fmt.Errorf("%w: timeout", services.ErrPaymentUncertain)
	check("uncertain pay failure", rig.register(goodBody(nil)), 502)
	rig = newCampRig()
	rig.h.Store = &failingInsertStore{memCampStore: rig.store}
	check("insert failure", rig.register(goodBody(nil)), 500)
	rig = newCampRig()
	rig.h.Store = &failingConflictsStore{memCampStore: rig.store}
	check("duplicate-check failure", rig.register(goodBody(nil)), 500)

	// A successful response is not an error and carries no marker.
	if rec := newCampRig().register(goodBody(nil)); rec.Header().Get(middleware.UserMessageHeader) != "" {
		t.Error("a 201 must not carry the user-message marker")
	}
}

// The frontend shows these messages in a small banner; the longest one must stay
// within the length it is willing to show (400 characters).
func TestCampLongestConflictMessageFitsTheBanner(t *testing.T) {
	rig := newCampRig()
	every := make([]string, 0, 7)
	for _, w := range camp.Weeks() {
		every = append(every, w.Start)
	}
	name := strings.Repeat("Abcdefghi ", 6)[:59] + "z" // exactly 60 characters
	if rec := rig.register(goodBody(body{"childName": name, "weeks": every})); rec.Code != 201 {
		t.Fatalf("first = %d: %s", rec.Code, rec.Body.String())
	}
	rec := rig.register(goodBody(body{"childName": name, "weeks": every}))
	if rec.Code != http.StatusConflict {
		t.Fatalf("repeat = %d", rec.Code)
	}
	msg := strings.TrimSpace(rec.Body.String())
	if len(msg) > 400 {
		t.Errorf("the longest conflict message is %d characters: %q", len(msg), msg)
	}
	if !strings.Contains(msg, "weeks 1-7 (2 Nov - 18 Dec)") {
		t.Errorf("seven consecutive weeks should read as one range: %q", msg)
	}

	// The worst case for length is every other week: nothing joins up.
	rig = newCampRig()
	alternate := []string{every[0], every[2], every[4], every[6]}
	rig.register(goodBody(body{"childName": name, "weeks": alternate}))
	rec = rig.register(goodBody(body{"childName": name, "weeks": alternate}))
	msg = strings.TrimSpace(rec.Body.String())
	if rec.Code != http.StatusConflict || len(msg) > 300 {
		t.Errorf("alternate weeks: %d, %d characters: %q", rec.Code, len(msg), msg)
	}
	for _, want := range []string{"week 1 (2-6 Nov)", "week 3 (16-20 Nov)", "week 5 (30 Nov - 4 Dec)", "week 7 (14-18 Dec)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("it should name %q: %q", want, msg)
		}
	}
}

// Three real registrations use the burst; a failed attempt is refunded exactly
// once, so exactly one more is allowed afterwards, not two.
func TestCampDefiniteFailureRefundsExactlyOneToken(t *testing.T) {
	rig := newCampRig()
	rig.h.phoneLimiter = middleware.NewLimiter(0.001, 3)
	for _, n := range []string{"Amani Otieno", "Baraka Otieno"} {
		if rec := rig.register(goodBody(body{"childName": n})); rec.Code != 201 {
			t.Fatalf("%s = %d", n, rec.Code)
		}
	}
	rig.payErr = errors.New("refused")
	if rec := rig.register(goodBody(body{"childName": "Chebet Otieno"})); rec.Code != http.StatusBadGateway {
		t.Fatalf("failing attempt = %d", rec.Code)
	}
	rig.payErr = nil
	if rec := rig.register(goodBody(body{"childName": "Daudi Otieno"})); rec.Code != 201 {
		t.Errorf("the refunded attempt should allow one more registration, got %d", rec.Code)
	}
	if rec := rig.register(goodBody(body{"childName": "Ema Otieno"})); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a failure must be refunded once, not twice: got %d, want 429", rec.Code)
	}
}

func TestCampSMSDetailsForAnUnknownProgramme(t *testing.T) {
	what, when := campSMSDetails(models.CampRegistration{Program: "retired", Lunch: true, Weeks: []string{"2026-11-02"}})
	if what != "retired + lunch" || when != "2-6 Nov" {
		t.Errorf("what, when = %q, %q", what, when)
	}
}
