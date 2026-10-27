package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"
	"turf-booking-backend/internal/ws"
)

const (
	memberPhone  = "254712345678"
	member2Phone = "254733000222"
	plainPhone   = "254722000111" // a customer who is not a member
	testAdminKey = "0123456789abcdef0123456789abcdef"
)

// A Sunday morning, so every date from tomorrow to a month ahead can be booked.
var flowNow = time.Date(2026, 11, 1, 10, 0, 0, 0, schedule.Location())

// nov is a day of November 2026. None of them is a public holiday, and a
// 21:00 - 23:00 session is on the grid on every one.
func nov(day int) string { return fmt.Sprintf("2026-11-%02d", day) }

// flow is the whole booking flow on in-memory stores, with a fixed clock and a
// payment gateway that only records what it is asked for.
type flow struct {
	h        *BookingHandler
	bookings *fakeBookings
	members  *fakeMembers
	gateway  *fakePayments
}

func newFlow(memberPhones ...string) *flow {
	f := &flow{bookings: &fakeBookings{}, members: newFakeMembers(memberPhones...), gateway: &fakePayments{}}
	h := NewBookingHandler(nil, &config.Config{AdminAPIKey: testAdminKey}, ws.NewPaymentHub())
	h.store = f.bookings
	h.members = f.members
	h.pay = f.gateway.pay
	h.now = func() time.Time { return flowNow }
	h.phoneLimiter = middleware.NewLimiter(6_000_000, 1_000_000) // out of the way unless a test wants it
	f.h = h
	return f
}

func eightASideTwoHours(over body) body {
	b := body{
		"userName":    "Wanjiru Njoroge",
		"email":       "wanjiru@example.com",
		"phoneNumber": memberPhone,
		"date":        nov(3),
		"timeRange":   "21:00 – 23:00",
		"pitchType":   "8Aside",
	}
	for k, v := range over {
		b[k] = v
	}
	return b
}

func jsonRequest(method, path string, v any) *http.Request {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func (f *flow) initiate(over body) (*httptest.ResponseRecorder, initiateResponse) {
	rec := httptest.NewRecorder()
	f.h.InitiateBooking(rec, jsonRequest(http.MethodPost, "/api/v1/initiate/paystack/push-stk", eightASideTwoHours(over)))
	var resp initiateResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

// book makes a booking that must succeed and returns what the customer was told.
func (f *flow) book(t *testing.T, over body) initiateResponse {
	t.Helper()
	rec, resp := f.initiate(over)
	if rec.Code != http.StatusCreated {
		t.Fatalf("booking %v = %d %q", over, rec.Code, rec.Body.String())
	}
	return resp
}

type quoted struct {
	Amount    int `json:"amount"`
	ListPrice int `json:"listPrice"`
	Member    *struct {
		Percent int    `json:"percent"`
		Applied bool   `json:"applied"`
		Month   string `json:"month"`
		Used    int    `json:"used"`
		Limit   int    `json:"limit"`
	} `json:"member"`
}

func (f *flow) quote(over body) (*httptest.ResponseRecorder, quoted) {
	b := eightASideTwoHours(over)
	delete(b, "userName")
	delete(b, "email")
	rec := httptest.NewRecorder()
	f.h.Quote(rec, jsonRequest(http.MethodPost, "/api/v1/quote", b))
	var q quoted
	_ = json.Unmarshal(rec.Body.Bytes(), &q)
	return rec, q
}

func (f *flow) discounted() int {
	n := 0
	for _, b := range f.bookings.all() {
		if b.Transaction.Discount == models.DiscountMember {
			n++
		}
	}
	return n
}

func (f *flow) setStatus(reference, status string) {
	f.bookings.mu.Lock()
	defer f.bookings.mu.Unlock()
	for i := range f.bookings.rows {
		if f.bookings.rows[i].Transaction.Reference == reference {
			f.bookings.rows[i].BookingDetails.BookingStatus = status
		}
	}
}

// ---- who pays what ------------------------------------------------------------

func TestACustomerWhoIsNotAMemberPaysTheListPrice(t *testing.T) {
	f := newFlow(memberPhone)
	resp := f.book(t, body{"phoneNumber": plainPhone})

	if resp.Amount != 10_000 || resp.Discount != "" {
		t.Errorf("response = %+v, want Ksh 10,000 and no discount", resp)
	}
	calls := f.gateway.all()
	if len(calls) != 1 || calls[0].cents != 1_000_000 || calls[0].phone != plainPhone {
		t.Fatalf("payment requests = %+v, want one of Ksh 10,000 (1,000,000 cents)", calls)
	}
	stored := f.bookings.all()
	if len(stored) != 1 || stored[0].Transaction.Amount != 10_000 || stored[0].Transaction.Discount != "" || stored[0].Transaction.ListAmount != 0 {
		t.Errorf("stored booking = %+v", stored)
	}
}

func TestAMemberPaysTwentyPercentLessOnTheEightASideTwoHourSession(t *testing.T) {
	f := newFlow(memberPhone)
	resp := f.book(t, nil)

	if resp.Amount != 8_000 || resp.Discount != models.DiscountMember {
		t.Errorf("response = %+v, want Ksh 8,000 at the member price", resp)
	}
	calls := f.gateway.all()
	if len(calls) != 1 || calls[0].cents != 800_000 || calls[0].ref != resp.Reference {
		t.Fatalf("payment requests = %+v, want one of Ksh 8,000 (800,000 cents) for %s", calls, resp.Reference)
	}
	stored := f.bookings.all()
	if len(stored) != 1 {
		t.Fatalf("stored %d bookings", len(stored))
	}
	tx := stored[0].Transaction
	if tx.Amount != 8_000 || tx.ListAmount != 10_000 || tx.Discount != models.DiscountMember {
		t.Errorf("stored transaction = %+v, want 8000 charged against a list price of 10000", tx)
	}
	if stored[0].BookingDetails.BookingStatus != statusPending {
		t.Errorf("status = %q, want Pending until the payment lands", stored[0].BookingDetails.BookingStatus)
	}
}

// The discount is for the 8-a-side 2 hour session only. A member who books
// anything else pays the standard price, and that booking does not use up one
// of their discounted sessions.
func TestAMemberPaysTheListPriceOnEveryOtherSession(t *testing.T) {
	f := newFlow(memberPhone)
	for i, c := range []struct {
		pitch, time string
		want        int
	}{
		{"5Aside", "21:00 – 23:00", 6_000},
		{"8Aside", "21:00 – 22:00", 5_000},
		{"5Aside", "21:00 – 22:00", 3_000},
	} {
		resp := f.book(t, body{"date": nov(3 + i), "timeRange": c.time, "pitchType": c.pitch})
		if resp.Amount != c.want || resp.Discount != "" {
			t.Errorf("%s %s = %+v, want the list price Ksh %d", c.pitch, c.time, resp, c.want)
		}
	}
	if n := f.discounted(); n != 0 {
		t.Errorf("%d bookings marked as discounted", n)
	}
	// ...so all four discounted sessions are still there.
	for i := 0; i < 4; i++ {
		if resp := f.book(t, body{"date": nov(10 + i)}); resp.Amount != 8_000 {
			t.Errorf("discounted session %d cost %d", i+1, resp.Amount)
		}
	}
}

func TestAMemberGetsFourDiscountedSessionsAMonthAndTheFifthIsFullPrice(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 4; i++ {
		resp := f.book(t, body{"date": nov(3 + i)})
		if resp.Amount != 8_000 || resp.Discount != models.DiscountMember {
			t.Fatalf("session %d = %+v, want the member price", i+1, resp)
		}
	}
	fifth := f.book(t, body{"date": nov(7)})
	if fifth.Amount != 10_000 || fifth.Discount != "" {
		t.Errorf("fifth session = %+v, want the full price", fifth)
	}
	// The month's allowance is not carried over or borrowed from: December has its own.
	december := f.book(t, body{"date": "2026-12-01"})
	if december.Amount != 8_000 {
		t.Errorf("first session of December = %+v, want the member price", december)
	}
	if n := f.discounted(); n != 5 {
		t.Errorf("%d discounted bookings stored, want 4 in November and 1 in December", n)
	}
}

// A session counts towards the month its date is in, not the month it was
// booked in: booking early does not use up this month's allowance.
func TestASessionCountsTowardsTheMonthItIsPlayedIn(t *testing.T) {
	f := newFlow(memberPhone)
	f.book(t, body{"date": "2026-12-01"}) // booked in November, played in December
	for i := 0; i < 4; i++ {
		if resp := f.book(t, body{"date": nov(3 + i)}); resp.Amount != 8_000 {
			t.Fatalf("November session %d cost %d: the December booking must not count against November", i+1, resp.Amount)
		}
	}
	if resp := f.book(t, body{"date": nov(30)}); resp.Amount != 10_000 {
		t.Errorf("fifth November session cost %d, want the full price", resp.Amount)
	}
	// 30 November was the fifth for November; 1 December is only the second for December.
	if resp := f.book(t, body{"date": "2026-12-02"}); resp.Amount != 8_000 {
		t.Errorf("second December session cost %d, want the member price", resp.Amount)
	}
}

func TestOnlyBookingsThatHoldAPlaceCountTowardsTheMonth(t *testing.T) {
	f := newFlow(memberPhone)
	var refs []string
	for i := 0; i < 4; i++ {
		refs = append(refs, f.book(t, body{"date": nov(3 + i)}).Reference)
	}
	// Pending (an unpaid hold), Booked (paid) and Confirmed (older rows) all count.
	f.setStatus(refs[1], statusBooked)
	f.setStatus(refs[2], statusConfirmed)
	if resp := f.book(t, body{"date": nov(7)}); resp.Amount != 10_000 {
		t.Fatalf("with four held sessions the fifth cost %d, want the full price", resp.Amount)
	}

	// A payment that failed no longer holds a place, so its session is given back.
	f.setStatus(refs[0], "Failed")
	if resp := f.book(t, body{"date": nov(8)}); resp.Amount != 8_000 {
		t.Errorf("after a failed payment the next session cost %d, want the member price", resp.Amount)
	}
	if resp := f.book(t, body{"date": nov(9)}); resp.Amount != 10_000 {
		t.Errorf("but only one session was given back; the next cost %d, want the full price", resp.Amount)
	}
}

func TestTakingAMemberOffTheListEndsTheDiscountButNotTheSessionsAlreadyBooked(t *testing.T) {
	f := newFlow(memberPhone)
	f.book(t, body{"date": nov(3)})

	if removed, err := f.members.Remove(context.Background(), memberPhone); !removed || err != nil {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if resp := f.book(t, body{"date": nov(4)}); resp.Amount != 10_000 {
		t.Errorf("a former member paid %d, want the full price", resp.Amount)
	}
	if n := f.discounted(); n != 1 {
		t.Errorf("the booking made as a member must stay as it was; %d discounted bookings", n)
	}

	// Back on the list, the one session already used this month still counts.
	if err := f.members.Add(context.Background(), models.Member{PhoneNumber: memberPhone, FullName: "Wanjiru Njoroge"}); err != nil {
		t.Fatal(err)
	}
	_, q := f.quote(body{"date": nov(5)})
	if q.Member == nil || q.Member.Used != 1 || !q.Member.Applied || q.Amount != 8_000 {
		t.Errorf("quote after re-adding = %+v", q)
	}
}

// Every price the customer is told, the gateway is asked for and the database
// records are the same figure.
func TestThePriceToldAskedForAndStoredIsOneFigure(t *testing.T) {
	f := newFlow(memberPhone)
	cases := []struct {
		phone, pitch, time string
		want               int
	}{
		{memberPhone, "8Aside", "21:00 – 23:00", 8_000},
		{memberPhone, "8Aside", "21:00 – 22:00", 5_000},
		{memberPhone, "5Aside", "21:00 – 23:00", 6_000},
		{memberPhone, "5Aside", "21:00 – 22:00", 3_000},
		{plainPhone, "8Aside", "21:00 – 23:00", 10_000},
		{plainPhone, "8Aside", "21:00 – 22:00", 5_000},
		{plainPhone, "5Aside", "21:00 – 23:00", 6_000},
		{plainPhone, "5Aside", "21:00 – 22:00", 3_000},
	}
	for i, c := range cases {
		resp := f.book(t, body{"phoneNumber": c.phone, "pitchType": c.pitch, "timeRange": c.time, "date": nov(3 + i)})
		calls := f.gateway.all()
		last := calls[len(calls)-1]
		stored := f.bookings.all()[len(calls)-1]
		if resp.Amount != c.want || last.cents != c.want*100 || stored.Transaction.Amount != c.want {
			t.Errorf("%s %s %s: told %d, asked for %d cents, stored %d; want %d", c.phone, c.pitch, c.time, resp.Amount, last.cents, stored.Transaction.Amount, c.want)
		}
	}
}

// ---- the price the customer was shown -----------------------------------------

func TestABookingIsRefusedIfThePriceIsNotTheOneTheCustomerWasShown(t *testing.T) {
	f := newFlow(memberPhone)

	// The member was shown Ksh 10,000 (a stale page), but the member price applies.
	rec, _ := f.initiate(body{"expectedAmount": 10_000})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "Ksh 8,000") || !strings.Contains(msg, "Ksh 10,000") {
		t.Errorf("message %q should name both prices", msg)
	}
	if rec.Header().Get(middleware.UserMessageHeader) != "1" {
		t.Error("the message is written for the customer, so it must be marked as readable")
	}
	if len(f.bookings.all()) != 0 || len(f.gateway.all()) != 0 {
		t.Fatalf("a refused booking recorded %d bookings and sent %d payment requests", len(f.bookings.all()), len(f.gateway.all()))
	}

	// The right amount goes through, and so does a request that names no amount.
	if rec, _ := f.initiate(body{"expectedAmount": 8_000}); rec.Code != http.StatusCreated {
		t.Errorf("the shown price should be accepted, got %d %s", rec.Code, rec.Body.String())
	}
	if rec, _ := f.initiate(body{"date": nov(4)}); rec.Code != http.StatusCreated {
		t.Errorf("a request with no expectedAmount should be accepted, got %d", rec.Code)
	}

	// A non-member shown the member price is refused too: the discount is not
	// something the browser can ask for.
	rec, _ = f.initiate(body{"phoneNumber": plainPhone, "date": nov(5), "expectedAmount": 8_000})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Ksh 10,000") {
		t.Errorf("a non-member asking for Ksh 8,000 got %d %q", rec.Code, rec.Body.String())
	}
	if rec, _ := f.initiate(body{"phoneNumber": plainPhone, "date": nov(5), "expectedAmount": 10_000}); rec.Code != http.StatusCreated {
		t.Errorf("a non-member shown Ksh 10,000 got %d", rec.Code)
	}
}

// The discounted sessions can run out between the customer seeing a price and
// paying it, for instance in a second browser tab.
func TestAPriceThatChangesBecauseTheMonthRanOutIsRefusedNotCharged(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 4; i++ {
		f.book(t, body{"date": nov(3 + i), "expectedAmount": 8_000})
	}
	rec, _ := f.initiate(body{"date": nov(7), "expectedAmount": 8_000})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "now Ksh 10,000") {
		t.Fatalf("got %d %q, want a 409 saying the price is now Ksh 10,000", rec.Code, rec.Body.String())
	}
	if len(f.bookings.all()) != 4 || len(f.gateway.all()) != 4 {
		t.Errorf("%d bookings and %d payment requests, want 4 of each", len(f.bookings.all()), len(f.gateway.all()))
	}
}

// Nothing is sent to the customer's phone when the price is refused, so it does
// not count as one of the few attempts a number gets.
func TestARefusedPriceDoesNotCostTheCustomerAnAttempt(t *testing.T) {
	f := newFlow(memberPhone)
	f.h.phoneLimiter = middleware.NewLimiter(0.3, 3) // as in production
	for i := 0; i < 6; i++ {
		if rec, _ := f.initiate(body{"expectedAmount": 10_000}); rec.Code != http.StatusConflict {
			t.Fatalf("refusal %d = %d, want 409 every time and never 429", i+1, rec.Code)
		}
	}
	if rec, _ := f.initiate(body{"expectedAmount": 8_000}); rec.Code != http.StatusCreated {
		t.Errorf("the corrected request = %d, want it accepted", rec.Code)
	}
}

// ---- concurrency ----------------------------------------------------------------

// A member's discounted sessions are counted and then one more is booked. Sent
// together, the requests must not all see "0 used": exactly four of twelve get
// the discount. Run with -race.
func TestTwelveBookingsSentTogetherGetExactlyFourDiscounts(t *testing.T) {
	f := newFlow(memberPhone)
	f.bookings.widen = 5 * time.Millisecond // time for an unserialised request to read the same count
	f.gateway.delay = 2 * time.Millisecond

	amounts := make([]int, 12)
	codes := make([]int, 12)
	var wg sync.WaitGroup
	for i := range amounts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, resp := f.initiate(body{"date": nov(2 + i)})
			codes[i], amounts[i] = rec.Code, resp.Amount
		}()
	}
	wg.Wait()

	discounted, full := 0, 0
	for i, a := range amounts {
		if codes[i] != http.StatusCreated {
			t.Fatalf("request %d = %d", i, codes[i])
		}
		switch a {
		case 8_000:
			discounted++
		case 10_000:
			full++
		default:
			t.Errorf("request %d cost %d", i, a)
		}
	}
	if discounted != 4 || full != 8 {
		t.Errorf("%d discounted and %d full price, want 4 and 8", discounted, full)
	}
	if n := f.discounted(); n != 4 {
		t.Errorf("%d discounted bookings stored, want 4", n)
	}
}

// The payment request goes to a gateway over the internet and can take seconds.
// A member's other bookings must not wait for it: the lock is only for counting
// and recording.
func TestAMembersLockIsNotHeldWhileThePaymentIsRequested(t *testing.T) {
	f := newFlow(memberPhone)
	entered, release := make(chan struct{}), make(chan struct{})
	f.gateway.gate = func(n int) {
		if n == 0 {
			close(entered)
			<-release
		}
	}
	first := make(chan int, 1)
	go func() {
		rec, _ := f.initiate(body{"date": nov(3)})
		first <- rec.Code
	}()
	<-entered // the first booking is recorded and its payment request is in flight

	second := make(chan int, 1)
	go func() {
		rec, _ := f.initiate(body{"date": nov(4)})
		second <- rec.Code
	}()
	select {
	case code := <-second:
		if code != http.StatusCreated {
			t.Errorf("second booking = %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Error("a second booking waited for the first one's payment request to finish")
	}
	close(release)
	if code := <-first; code != http.StatusCreated {
		t.Errorf("first booking = %d", code)
	}
}

// Members, non-members and clashing bookings for the same few dates, all at
// once: every request finishes (the member lock and the date lock never wait on
// each other) and no member gets more than four discounts.
func TestManyMembersAndManyDatesAtOnceNeitherDeadlockNorOverDiscount(t *testing.T) {
	phones := []string{memberPhone, member2Phone, plainPhone}
	f := newFlow(memberPhone, member2Phone)
	f.bookings.widen = time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for _, phone := range phones {
			for day := 2; day <= 9; day++ { // 8 dates, shared by all three numbers
				wg.Add(1)
				go func() {
					defer wg.Done()
					f.initiate(body{"phoneNumber": phone, "date": nov(day)})
				}()
			}
		}
		wg.Wait()
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the bookings did not finish: the locks deadlocked")
	}

	perPhone := map[string]int{}
	for _, b := range f.bookings.all() {
		if b.Transaction.Discount == models.DiscountMember {
			perPhone[b.Team.PhoneNumber]++
		}
	}
	for phone, n := range perPhone {
		if n > schedule.MemberSessionsPerMonth {
			t.Errorf("%s got %d discounts", phone, n)
		}
	}
	if perPhone[plainPhone] != 0 {
		t.Errorf("a non-member got %d discounts", perPhone[plainPhone])
	}
}

// ---- what does not use up a discounted session ------------------------------------

func TestABookingThatClashesDoesNotUseUpADiscountedSession(t *testing.T) {
	f := newFlow(memberPhone, member2Phone)
	f.book(t, body{"date": nov(3)}) // takes the whole pitch

	rec, _ := f.initiate(body{"phoneNumber": member2Phone, "date": nov(3)})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a second 8-a-side session at the same time = %d, want 409", rec.Code)
	}
	_, q := f.quote(body{"phoneNumber": member2Phone, "date": nov(4)})
	if q.Member == nil || q.Member.Used != 0 {
		t.Errorf("the refused booking used up a session: %+v", q.Member)
	}
	if resp := f.book(t, body{"phoneNumber": member2Phone, "date": nov(4)}); resp.Amount != 8_000 {
		t.Errorf("member 2's first real booking cost %d", resp.Amount)
	}
}

func TestAFailedPaymentGivesTheSessionBack(t *testing.T) {
	f := newFlow(memberPhone)
	f.gateway.err = errors.New("gateway down")
	for i := 0; i < 5; i++ {
		rec, _ := f.initiate(body{"date": nov(3 + i)})
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d = %d, want 502", i+1, rec.Code)
		}
	}
	for _, b := range f.bookings.all() {
		if b.BookingDetails.BookingStatus != "Failed" || b.Transaction.PaymentStatus != "Failed" {
			t.Errorf("a booking whose payment could not start is %q/%q, want Failed", b.BookingDetails.BookingStatus, b.Transaction.PaymentStatus)
		}
	}

	f.gateway.err = nil
	if resp := f.book(t, body{"date": nov(10)}); resp.Amount != 8_000 {
		t.Errorf("after five failed attempts the first real booking cost %d, want the member price", resp.Amount)
	}
}

func TestADatabaseErrorWhilePricingRefusesTheBooking(t *testing.T) {
	f := newFlow(memberPhone)
	f.bookings.failures = map[string]error{"MemberSessions": errors.New("down")}
	rec, _ := f.initiate(nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500: a member must not be charged a price that could not be worked out", rec.Code)
	}
	if len(f.gateway.all()) != 0 || len(f.bookings.all()) != 0 {
		t.Error("nothing may be recorded or charged")
	}

	f.bookings.failures = nil
	f.members.failures = map[string]error{"Find": errors.New("down")}
	if rec, _ := f.initiate(nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing member lookup = %d, want 500", rec.Code)
	}
	// A booking the discount cannot touch never needs the member list.
	if rec, _ := f.initiate(body{"pitchType": "5Aside", "date": nov(4)}); rec.Code != http.StatusCreated {
		t.Errorf("a 5-a-side booking = %d, want it unaffected by the member list", rec.Code)
	}
}

func TestWithNoMemberListEveryoneSimplyPaysTheListPrice(t *testing.T) {
	f := newFlow(memberPhone)
	f.h.members = nil
	if resp := f.book(t, nil); resp.Amount != 10_000 || resp.Discount != "" {
		t.Errorf("response = %+v", resp)
	}
}

// ---- the quote -----------------------------------------------------------------------

func TestQuoteForACustomerWhoIsNotAMember(t *testing.T) {
	f := newFlow(memberPhone)
	rec, q := f.quote(body{"phoneNumber": plainPhone})
	if rec.Code != http.StatusOK || q.Amount != 10_000 || q.ListPrice != 10_000 || q.Member != nil {
		t.Errorf("quote = %d %+v %s", rec.Code, q, rec.Body.String())
	}
	// Nothing in the answer hints at the member list.
	if strings.Contains(rec.Body.String(), "member") {
		t.Errorf("a non-member's quote mentions members: %s", rec.Body.String())
	}
}

func TestQuoteForAMemberShowsThePriceAndWhereTheyAreInTheMonth(t *testing.T) {
	f := newFlow(memberPhone)
	rec, q := f.quote(nil)
	if rec.Code != http.StatusOK || q.Amount != 8_000 || q.ListPrice != 10_000 || q.Member == nil {
		t.Fatalf("quote = %d %s", rec.Code, rec.Body.String())
	}
	if m := *q.Member; m.Percent != 20 || !m.Applied || m.Month != "2026-11" || m.Used != 0 || m.Limit != 4 {
		t.Errorf("member part = %+v", m)
	}

	for i := 0; i < 4; i++ {
		f.book(t, body{"date": nov(3 + i)})
		_, q = f.quote(body{"date": nov(10)})
		wantUsed := i + 1
		wantApplied := wantUsed < 4
		wantAmount := 8_000
		if !wantApplied {
			wantAmount = 10_000
		}
		if q.Member == nil || q.Member.Used != wantUsed || q.Member.Applied != wantApplied || q.Amount != wantAmount {
			t.Errorf("after %d sessions: %+v %+v", wantUsed, q, q.Member)
		}
	}
	// The next month starts afresh.
	if _, q = f.quote(body{"date": "2026-12-01"}); q.Amount != 8_000 || q.Member == nil || q.Member.Used != 0 || q.Member.Month != "2026-12" {
		t.Errorf("December quote = %+v %+v", q, q.Member)
	}
}

func TestQuoteForAMemberOnASessionTheDiscountDoesNotCover(t *testing.T) {
	f := newFlow(memberPhone)
	for _, c := range []struct {
		pitch, time string
		want        int
	}{{"5Aside", "21:00 – 23:00", 6_000}, {"8Aside", "21:00 – 22:00", 5_000}, {"5Aside", "21:00 – 22:00", 3_000}} {
		_, q := f.quote(body{"pitchType": c.pitch, "timeRange": c.time})
		if q.Amount != c.want || q.ListPrice != c.want || q.Member != nil {
			t.Errorf("%s %s quote = %+v", c.pitch, c.time, q)
		}
	}
}

// The quote and the booking agree at every step, because they share one rule.
func TestTheQuoteIsAlwaysThePriceTheBookingThenCharges(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 7; i++ {
		date := nov(3 + i)
		phone := memberPhone
		if i%3 == 2 {
			phone = plainPhone
		}
		_, q := f.quote(body{"phoneNumber": phone, "date": date})
		resp := f.book(t, body{"phoneNumber": phone, "date": date, "expectedAmount": q.Amount})
		if resp.Amount != q.Amount {
			t.Errorf("booking %d: quoted %d, charged %d", i+1, q.Amount, resp.Amount)
		}
	}
}

func TestQuoteChangesNothing(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 10; i++ {
		f.quote(nil)
	}
	if len(f.bookings.all()) != 0 || len(f.gateway.all()) != 0 {
		t.Error("a quote must not record a booking or ask for a payment")
	}
}

func TestQuoteRefusesWhatABookingWouldRefuse(t *testing.T) {
	f := newFlow(memberPhone)
	for name, over := range map[string]body{
		"bad phone":        {"phoneNumber": "12345"},
		"foreign phone":    {"phoneNumber": "14155550123"},
		"bad pitch":        {"pitchType": "11Aside"},
		"past date":        {"date": "2026-10-20"},
		"too far ahead":    {"date": "2027-03-03"},
		"off the grid":     {"timeRange": "21:30 – 23:30"},
		"three hours":      {"timeRange": "20:00 – 23:00"},
		"not a date":       {"date": "tomorrow"},
		"empty time range": {"timeRange": ""},
	} {
		if rec, _ := f.quote(over); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/quote", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/plain")
	f.h.Quote(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d, want 415 (a cross-site form must not be able to ask)", rec.Code)
	}

	rec = httptest.NewRecorder()
	f.h.Quote(rec, jsonRequest(http.MethodPost, "/api/v1/quote", body{"phoneNumber": strings.Repeat("7", 10_000)}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body = %d, want 413", rec.Code)
	}

	f.bookings.failures = map[string]error{"MemberSessions": errors.New("down")}
	if rec, _ := f.quote(nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("a database error = %d, want 500", rec.Code)
	}
}

// ---- what the admin sees --------------------------------------------------------------

func TestTheAdminBookingListShowsWhatWasCharged(t *testing.T) {
	member := models.Booking{
		Team:           models.Team{FullName: "Wanjiru Njoroge", PhoneNumber: memberPhone},
		BookingDetails: models.BookingDetails{Date: nov(3), Time: "21:00 – 23:00", PitchType: "8Aside", BookingStatus: "Booked"},
		Transaction:    models.Transaction{Amount: 8_000, ListAmount: 10_000, Discount: models.DiscountMember},
	}
	v := adminBookingView(member)
	if v["amount"] != "8000" || v["discount"] != "member" || v["listAmount"] != "10000" {
		t.Errorf("member view = %v", v)
	}

	plain := member
	plain.Transaction = models.Transaction{Amount: 10_000}
	v = adminBookingView(plain)
	if v["amount"] != "10000" {
		t.Errorf("plain view = %v", v)
	}
	if _, ok := v["discount"]; ok {
		t.Errorf("a full-price booking must not carry a discount: %v", v)
	}

	// Rows from before prices were recorded have no amount at all.
	old := member
	old.Transaction = models.Transaction{}
	v = adminBookingView(old)
	if _, ok := v["amount"]; ok {
		t.Errorf("an old booking has no amount to show: %v", v)
	}
	if v["date"] != nov(3) || v["pitchType"] != "8Aside" || v["phoneNumber"] != memberPhone {
		t.Errorf("the existing fields must be unchanged: %v", v)
	}
}
