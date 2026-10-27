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

	"go.mongodb.org/mongo-driver/bson/primitive"
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

// bookPaid is book followed by the customer paying, as the payment webhook
// records it.
func (f *flow) bookPaid(t *testing.T, over body) initiateResponse {
	t.Helper()
	resp := f.book(t, over)
	f.setStatus(resp.Reference, statusBooked)
	return resp
}

// seedPaid puts a paid booking straight into the store, as if it had been made
// earlier.
func (f *flow) seedPaid(phone, date, timeRange, pitch string) {
	f.bookings.mu.Lock()
	defer f.bookings.mu.Unlock()
	f.bookings.rows = append(f.bookings.rows, models.Booking{
		ID:             primitive.NewObjectID(),
		Team:           models.Team{FullName: "Earlier Booking", PhoneNumber: phone},
		BookingDetails: models.BookingDetails{Date: date, Time: timeRange, PitchType: pitch, BookingStatus: statusBooked},
		Transaction:    models.Transaction{Amount: 10_000, PaymentStatus: "Completed", Reference: fmt.Sprintf("seed-%d", len(f.bookings.rows))},
	})
}

type quoted struct {
	Amount    int `json:"amount"`
	ListPrice int `json:"listPrice"`
	Member    *struct {
		Percent         int    `json:"percent"`
		Applied         bool   `json:"applied"`
		Month           string `json:"month"`
		Sessions        int    `json:"sessions"`
		DiscountSession int    `json:"discountSession"`
		DiscountUsed    bool   `json:"discountUsed"`
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

// expire removes a booking, as the cleanup worker does to one that was never paid.
func (f *flow) expire(reference string) {
	f.bookings.mu.Lock()
	defer f.bookings.mu.Unlock()
	kept := f.bookings.rows[:0]
	for _, b := range f.bookings.rows {
		if b.Transaction.Reference != reference {
			kept = append(kept, b)
		}
	}
	f.bookings.rows = kept
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

// The rule: Ksh 10,000, 10,000, 10,000 and then, on the fourth, 80% off (Ksh 2,000).
func TestAMemberPaysTheStandardPriceForThreeSessionsAndEightyPercentLessForTheFourth(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 3; i++ {
		_, q := f.quote(body{"date": nov(3 + i)})
		if q.Amount != 10_000 || q.Member == nil || q.Member.Applied || q.Member.Sessions != i || q.Member.DiscountSession != 4 || q.Member.DiscountUsed {
			t.Fatalf("quote for session %d = %+v %+v", i+1, q, q.Member)
		}
		resp := f.bookPaid(t, body{"date": nov(3 + i)})
		if resp.Amount != 10_000 || resp.Discount != "" {
			t.Fatalf("session %d = %+v, want the standard price", i+1, resp)
		}
	}

	_, q := f.quote(body{"date": nov(6)})
	if q.Amount != 2_000 || q.ListPrice != 10_000 || q.Member == nil || !q.Member.Applied || q.Member.Sessions != 3 || q.Member.Percent != 80 {
		t.Fatalf("quote for the fourth session = %+v %+v", q, q.Member)
	}
	fourth := f.bookPaid(t, body{"date": nov(6)})
	if fourth.Amount != 2_000 || fourth.Discount != models.DiscountMember {
		t.Fatalf("fourth session = %+v, want Ksh 2,000", fourth)
	}
	calls := f.gateway.all()
	if last := calls[len(calls)-1]; last.cents != 200_000 || last.ref != fourth.Reference {
		t.Errorf("M-Pesa was asked for %+v, want 200,000 cents", last)
	}
	tx := f.bookings.all()[3].Transaction
	if tx.Amount != 2_000 || tx.ListAmount != 10_000 || tx.Discount != models.DiscountMember {
		t.Errorf("stored transaction = %+v, want 2000 charged against a list price of 10000", tx)
	}

	// Once a month: the fifth and sixth are at the standard price again.
	for i, day := range []int{7, 8} {
		_, q := f.quote(body{"date": nov(day)})
		if q.Amount != 10_000 || q.Member == nil || q.Member.Applied || !q.Member.DiscountUsed {
			t.Fatalf("quote for session %d = %+v %+v", 5+i, q, q.Member)
		}
		if resp := f.bookPaid(t, body{"date": nov(day)}); resp.Amount != 10_000 || resp.Discount != "" {
			t.Errorf("session %d = %+v, want the standard price", 5+i, resp)
		}
	}
	if n := f.discounted(); n != 1 {
		t.Errorf("%d discounted bookings in the month, want exactly 1", n)
	}
}

// Every month has its own fourth session, and a session counts towards the
// month its date is in, not the month it was booked in.
func TestEachMonthHasItsOwnFourthSession(t *testing.T) {
	f := newFlow(memberPhone)
	f.bookPaid(t, body{"date": "2026-12-01"}) // booked in November, played in December

	for i := 0; i < 3; i++ {
		if resp := f.bookPaid(t, body{"date": nov(3 + i)}); resp.Amount != 10_000 {
			t.Fatalf("November session %d cost %d: the December booking must not count towards November", i+1, resp.Amount)
		}
	}
	if resp := f.bookPaid(t, body{"date": nov(30)}); resp.Amount != 2_000 {
		t.Fatalf("November's fourth session cost %d, want Ksh 2,000", resp.Amount)
	}
	// 1 December was December's first session, so 2 December is its second.
	if resp := f.bookPaid(t, body{"date": "2026-12-02"}); resp.Amount != 10_000 {
		t.Errorf("December's second session cost %d", resp.Amount)
	}
}

// A discount reached by starting bookings and not paying for them would be free
// money: only sessions that have been paid for count towards the fourth.
func TestOnlyPaidSessionsCountTowardsTheFourth(t *testing.T) {
	f := newFlow(memberPhone)
	var pending []string
	for i := 0; i < 3; i++ {
		pending = append(pending, f.book(t, body{"date": nov(3 + i)}).Reference) // never paid
	}
	_, q := f.quote(body{"date": nov(6)})
	if q.Amount != 10_000 || q.Member == nil || q.Member.Applied || q.Member.Sessions != 0 {
		t.Fatalf("three unpaid bookings must not count: quote = %+v %+v", q, q.Member)
	}
	if resp := f.book(t, body{"date": nov(6)}); resp.Amount != 10_000 {
		t.Fatalf("fourth booking after three unpaid ones cost %d, want the standard price", resp.Amount)
	}

	// The unpaid ones lapse; nothing was gained.
	for _, ref := range pending {
		f.expire(ref)
	}
	if resp := f.book(t, body{"date": nov(7)}); resp.Amount != 10_000 {
		t.Errorf("after they lapsed the next booking cost %d, want the standard price", resp.Amount)
	}

	// Paid ones do count: pay the three and the next session is the fourth.
	f2 := newFlow(memberPhone)
	var refs []string
	for i := 0; i < 3; i++ {
		refs = append(refs, f2.book(t, body{"date": nov(3 + i)}).Reference)
	}
	for _, ref := range refs {
		f2.setStatus(ref, statusBooked)
	}
	if resp := f2.book(t, body{"date": nov(6)}); resp.Amount != 2_000 {
		t.Errorf("after the three were paid the next cost %d, want Ksh 2,000", resp.Amount)
	}
}

// "Confirmed" is what older rows say instead of "Booked"; both are paid.
func TestOlderConfirmedBookingsCountAsPaid(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 3; i++ {
		resp := f.book(t, body{"date": nov(3 + i)})
		f.setStatus(resp.Reference, statusConfirmed)
	}
	if resp := f.book(t, body{"date": nov(6)}); resp.Amount != 2_000 {
		t.Errorf("fourth session cost %d, want Ksh 2,000", resp.Amount)
	}
}

// The discount is given once, to one booking, even while that booking is still
// waiting for its payment, and is given back if the payment does not happen.
func TestTheDiscountIsHeldWhileItsBookingWaitsForPaymentAndReleasedIfItFails(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 3; i++ {
		f.bookPaid(t, body{"date": nov(3 + i)})
	}
	first := f.book(t, body{"date": nov(6)}) // the fourth, not yet paid
	if first.Amount != 2_000 {
		t.Fatalf("fourth session = %+v", first)
	}
	_, q := f.quote(body{"date": nov(7)})
	if q.Amount != 10_000 || q.Member == nil || q.Member.Applied || !q.Member.DiscountUsed {
		t.Errorf("while the discounted booking waits for payment another one must be at the standard price: %+v %+v", q, q.Member)
	}
	if resp := f.book(t, body{"date": nov(7)}); resp.Amount != 10_000 {
		t.Errorf("a second booking cost %d while the first is unpaid", resp.Amount)
	}

	// The payment fails: the discount is available again.
	f.setStatus(first.Reference, "Failed")
	f.expire(f.bookings.all()[4].Transaction.Reference) // and the other unpaid booking lapses
	if resp := f.book(t, body{"date": nov(8)}); resp.Amount != 2_000 {
		t.Errorf("after the failed payment the next booking cost %d, want Ksh 2,000", resp.Amount)
	}
}

// What counts is what the number has paid for, however it was booked: sessions
// booked before the number was added to the list count, and a customer who is
// not a member never gets the discount.
func TestSessionsPaidForBeforeJoiningCountButNonMembersGetNothing(t *testing.T) {
	f := newFlow() // nobody is a member yet
	for i := 0; i < 3; i++ {
		f.seedPaid(memberPhone, nov(10+i), "21:00 – 23:00", "8Aside")
	}
	if resp := f.book(t, body{"date": nov(3)}); resp.Amount != 10_000 {
		t.Fatalf("a customer who is not a member cost %d on their fourth session, want the standard price", resp.Amount)
	}
	f.expire(f.bookings.all()[3].Transaction.Reference)

	if err := f.members.Add(context.Background(), models.Member{PhoneNumber: memberPhone, FullName: "Wanjiru Njoroge"}); err != nil {
		t.Fatal(err)
	}
	if resp := f.book(t, body{"date": nov(3)}); resp.Amount != 2_000 {
		t.Errorf("once on the list, the next session cost %d, want Ksh 2,000", resp.Amount)
	}
}

// Only the 8-a-side 2 hour session counts, and only it is discounted.
func TestOnlyEightASideTwoHourSessionsCountAndOnlyTheyAreDiscounted(t *testing.T) {
	f := newFlow(memberPhone)
	for i, c := range []struct{ pitch, time string }{
		{"5Aside", "21:00 – 23:00"}, {"5Aside", "21:00 – 23:00"}, {"5Aside", "21:00 – 23:00"},
		{"8Aside", "21:00 – 22:00"}, {"8Aside", "21:00 – 22:00"}, {"8Aside", "21:00 – 22:00"},
		{"5Aside", "21:00 – 22:00"},
	} {
		want := map[string]int{"5Aside/21:00 – 23:00": 6_000, "8Aside/21:00 – 22:00": 5_000, "5Aside/21:00 – 22:00": 3_000}[c.pitch+"/"+c.time]
		resp := f.bookPaid(t, body{"date": nov(3 + i), "pitchType": c.pitch, "timeRange": c.time})
		if resp.Amount != want || resp.Discount != "" {
			t.Errorf("%s %s = %+v, want the list price Ksh %d", c.pitch, c.time, resp, want)
		}
		if _, q := f.quote(body{"date": nov(3 + i), "pitchType": c.pitch, "timeRange": c.time}); q.Member != nil {
			t.Errorf("%s %s: a session the discount does not cover must say nothing about it: %+v", c.pitch, c.time, q.Member)
		}
	}
	// Seven paid sessions of other kinds have not moved a member towards the fourth.
	if _, q := f.quote(body{"date": nov(20)}); q.Amount != 10_000 || q.Member == nil || q.Member.Sessions != 0 || q.Member.Applied {
		t.Fatalf("quote = %+v %+v", q, q.Member)
	}
	for i := 0; i < 3; i++ {
		if resp := f.bookPaid(t, body{"date": nov(20 + i)}); resp.Amount != 10_000 {
			t.Fatalf("eligible session %d cost %d", i+1, resp.Amount)
		}
	}
	if resp := f.bookPaid(t, body{"date": nov(23)}); resp.Amount != 2_000 {
		t.Errorf("the fourth 8-a-side 2 hour session cost %d, want Ksh 2,000", resp.Amount)
	}
}

func TestTakingAMemberOffTheListEndsTheDiscountButTheSessionsStayCounted(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 3; i++ {
		f.bookPaid(t, body{"date": nov(3 + i)})
	}
	if removed, err := f.members.Remove(context.Background(), memberPhone); !removed || err != nil {
		t.Fatalf("remove = %v, %v", removed, err)
	}
	if resp := f.book(t, body{"date": nov(6)}); resp.Amount != 10_000 {
		t.Errorf("a former member paid %d, want the standard price", resp.Amount)
	}
	f.expire(f.bookings.all()[3].Transaction.Reference)

	if err := f.members.Add(context.Background(), models.Member{PhoneNumber: memberPhone, FullName: "Wanjiru Njoroge"}); err != nil {
		t.Fatal(err)
	}
	if _, q := f.quote(body{"date": nov(6)}); q.Amount != 2_000 || q.Member == nil || q.Member.Sessions != 3 {
		t.Errorf("quote after re-adding = %+v %+v", q, q.Member)
	}
}

// Every price the customer is told, the gateway is asked for and the database
// records are the same figure.
func TestThePriceToldAskedForAndStoredIsOneFigure(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 3; i++ {
		f.bookPaid(t, body{"date": nov(3 + i)})
	}
	cases := []struct {
		phone, pitch, time string
		want               int
	}{
		{memberPhone, "8Aside", "21:00 – 22:00", 5_000},
		{memberPhone, "5Aside", "21:00 – 23:00", 6_000},
		{memberPhone, "5Aside", "21:00 – 22:00", 3_000},
		{plainPhone, "8Aside", "21:00 – 23:00", 10_000},
		{plainPhone, "8Aside", "21:00 – 22:00", 5_000},
		{memberPhone, "8Aside", "21:00 – 23:00", 2_000}, // last: it takes the discount
	}
	for i, c := range cases {
		before := len(f.gateway.all())
		resp := f.book(t, body{"phoneNumber": c.phone, "pitchType": c.pitch, "timeRange": c.time, "date": nov(10 + i)})
		calls := f.gateway.all()
		last := calls[len(calls)-1]
		stored := f.bookings.all()[len(f.bookings.all())-1]
		if len(calls) != before+1 || resp.Amount != c.want || last.cents != c.want*100 || stored.Transaction.Amount != c.want {
			t.Errorf("%s %s %s: told %d, asked for %d cents, stored %d; want %d", c.phone, c.pitch, c.time, resp.Amount, last.cents, stored.Transaction.Amount, c.want)
		}
	}
}

// ---- the price the customer was shown -----------------------------------------

func TestABookingIsRefusedIfThePriceIsNotTheOneTheCustomerWasShown(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 3; i++ {
		f.bookPaid(t, body{"date": nov(3 + i)})
	}
	before := len(f.bookings.all())

	// The member was shown the standard price (a stale page), but this is the discounted session.
	rec, _ := f.initiate(body{"date": nov(6), "expectedAmount": 10_000})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if msg := rec.Body.String(); !strings.Contains(msg, "Ksh 2,000") || !strings.Contains(msg, "Ksh 10,000") {
		t.Errorf("message %q should name both prices", msg)
	}
	if rec.Header().Get(middleware.UserMessageHeader) != "1" {
		t.Error("the message is written for the customer, so it must be marked as readable")
	}
	if len(f.bookings.all()) != before || len(f.gateway.all()) != 3 {
		t.Fatalf("a refused booking recorded %d bookings and sent %d payment requests", len(f.bookings.all())-before, len(f.gateway.all())-3)
	}

	// The right amount goes through.
	if rec, resp := f.initiate(body{"date": nov(6), "expectedAmount": 2_000}); rec.Code != http.StatusCreated || resp.Amount != 2_000 {
		t.Errorf("the shown price should be accepted, got %d %s", rec.Code, rec.Body.String())
	}

	// A non-member shown the discounted price is refused: the discount is not
	// something the browser can ask for.
	rec, _ = f.initiate(body{"phoneNumber": plainPhone, "date": nov(7), "expectedAmount": 2_000})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Ksh 10,000") {
		t.Errorf("a non-member asking for Ksh 2,000 got %d %q", rec.Code, rec.Body.String())
	}
	if rec, _ := f.initiate(body{"phoneNumber": plainPhone, "date": nov(7), "expectedAmount": 10_000}); rec.Code != http.StatusCreated {
		t.Errorf("a non-member shown Ksh 10,000 got %d", rec.Code)
	}
	// And a request that names no amount is accepted at the price the server works out.
	if rec, resp := f.initiate(body{"date": nov(8)}); rec.Code != http.StatusCreated || resp.Amount != 10_000 {
		t.Errorf("a request with no expectedAmount = %d %+v", rec.Code, resp)
	}
}

// The discount can go to another booking between the customer seeing a price
// and paying it, for instance from a second browser tab.
func TestAPriceThatChangesBecauseTheDiscountWasGivenElsewhereIsRefusedNotCharged(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 3; i++ {
		f.bookPaid(t, body{"date": nov(3 + i)})
	}
	_, q := f.quote(body{"date": nov(7)})
	if q.Amount != 2_000 {
		t.Fatalf("the page would show %d", q.Amount)
	}
	f.book(t, body{"date": nov(6)}) // another tab takes the discount

	rec, _ := f.initiate(body{"date": nov(7), "expectedAmount": q.Amount})
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
	for i := 0; i < 3; i++ {
		f.bookPaid(t, body{"date": nov(3 + i)})
	}
	f.h.phoneLimiter = middleware.NewLimiter(0.3, 3) // as in production
	for i := 0; i < 6; i++ {
		if rec, _ := f.initiate(body{"date": nov(6), "expectedAmount": 10_000}); rec.Code != http.StatusConflict {
			t.Fatalf("refusal %d = %d, want 409 every time and never 429", i+1, rec.Code)
		}
	}
	if rec, _ := f.initiate(body{"date": nov(6), "expectedAmount": 2_000}); rec.Code != http.StatusCreated {
		t.Errorf("the corrected request = %d, want it accepted", rec.Code)
	}
}

// ---- concurrency ----------------------------------------------------------------

// Where a member stands is looked up and then one more session is booked. Sent
// together, the requests must not all see "three paid, no discount yet": exactly
// one of eight gets the discount. Run with -race.
func TestEightBookingsSentTogetherGetExactlyOneDiscount(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 3; i++ {
		f.seedPaid(memberPhone, nov(20+i), "21:00 – 23:00", "8Aside")
	}
	f.bookings.widen = 5 * time.Millisecond // time for an unserialised request to read the same standing
	f.gateway.delay = 2 * time.Millisecond

	amounts := make([]int, 8)
	codes := make([]int, 8)
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
		case 2_000:
			discounted++
		case 10_000:
			full++
		default:
			t.Errorf("request %d cost %d", i, a)
		}
	}
	if discounted != 1 || full != 7 {
		t.Errorf("%d discounted and %d full price, want 1 and 7", discounted, full)
	}
	if n := f.discounted(); n != 1 {
		t.Errorf("%d discounted bookings stored, want 1", n)
	}
}

// The payment request goes to a gateway over the internet and can take seconds.
// A member's other bookings must not wait for it: the lock is only for looking
// up and recording.
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
// each other) and no member gets more than the one discount.
func TestManyMembersAndManyDatesAtOnceNeitherDeadlockNorOverDiscount(t *testing.T) {
	phones := []string{memberPhone, member2Phone, plainPhone}
	f := newFlow(memberPhone, member2Phone)
	for _, phone := range phones {
		for i := 0; i < 3; i++ {
			f.seedPaid(phone, nov(20+i), "21:00 – 23:00", "8Aside")
		}
	}
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
		if n > 1 {
			t.Errorf("%s got %d discounts in one month", phone, n)
		}
	}
	if perPhone[plainPhone] != 0 {
		t.Errorf("a non-member got %d discounts", perPhone[plainPhone])
	}
}

// ---- what does not use up the discount -------------------------------------------

func TestABookingThatClashesDoesNotUseUpTheDiscount(t *testing.T) {
	f := newFlow(memberPhone, member2Phone)
	for i := 0; i < 3; i++ {
		f.seedPaid(member2Phone, nov(20+i), "21:00 – 23:00", "8Aside")
	}
	f.book(t, body{"date": nov(3)}) // member 1 takes the whole pitch on 3 November

	rec, _ := f.initiate(body{"phoneNumber": member2Phone, "date": nov(3)})
	if rec.Code != http.StatusConflict {
		t.Fatalf("a second 8-a-side session at the same time = %d, want 409", rec.Code)
	}
	if _, q := f.quote(body{"phoneNumber": member2Phone, "date": nov(4)}); q.Member == nil || q.Member.DiscountUsed || q.Amount != 2_000 {
		t.Errorf("the refused booking used up the discount: %+v %+v", q, q.Member)
	}
	if resp := f.book(t, body{"phoneNumber": member2Phone, "date": nov(4)}); resp.Amount != 2_000 {
		t.Errorf("member 2's next booking cost %d, want Ksh 2,000", resp.Amount)
	}
}

func TestAFailedPaymentGivesTheDiscountBack(t *testing.T) {
	f := newFlow(memberPhone)
	for i := 0; i < 3; i++ {
		f.seedPaid(memberPhone, nov(20+i), "21:00 – 23:00", "8Aside")
	}
	f.gateway.err = errors.New("gateway down")
	for i := 0; i < 3; i++ {
		rec, _ := f.initiate(body{"date": nov(3 + i)})
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("attempt %d = %d, want 502", i+1, rec.Code)
		}
	}
	for _, b := range f.bookings.all() {
		if b.Transaction.Discount != "" && (b.BookingDetails.BookingStatus != "Failed" || b.Transaction.PaymentStatus != "Failed") {
			t.Errorf("a booking whose payment could not start is %q/%q, want Failed", b.BookingDetails.BookingStatus, b.Transaction.PaymentStatus)
		}
	}

	f.gateway.err = nil
	if resp := f.book(t, body{"date": nov(10)}); resp.Amount != 2_000 {
		t.Errorf("after failed attempts the next booking cost %d, want Ksh 2,000", resp.Amount)
	}
}

func TestADatabaseErrorWhilePricingRefusesTheBooking(t *testing.T) {
	f := newFlow(memberPhone)
	f.bookings.failures = map[string]error{"MemberMonth": errors.New("down")}
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
	for i := 0; i < 3; i++ {
		f.seedPaid(memberPhone, nov(20+i), "21:00 – 23:00", "8Aside")
	}
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
	if rec.Code != http.StatusOK || q.Amount != 10_000 || q.ListPrice != 10_000 || q.Member == nil {
		t.Fatalf("quote = %d %s", rec.Code, rec.Body.String())
	}
	if m := *q.Member; m.Percent != 80 || m.Applied || m.Month != "2026-11" || m.Sessions != 0 || m.DiscountSession != 4 || m.DiscountUsed {
		t.Errorf("member part = %+v", m)
	}

	for i := 0; i < 3; i++ {
		f.bookPaid(t, body{"date": nov(3 + i)})
	}
	_, q = f.quote(body{"date": nov(10)})
	if q.Amount != 2_000 || q.ListPrice != 10_000 || q.Member == nil || !q.Member.Applied || q.Member.Sessions != 3 {
		t.Errorf("after three paid sessions: %+v %+v", q, q.Member)
	}

	// The same session in December is the first of that month.
	if _, q = f.quote(body{"date": "2026-12-01"}); q.Amount != 10_000 || q.Member == nil || q.Member.Sessions != 0 || q.Member.Month != "2026-12" || q.Member.Applied {
		t.Errorf("December quote = %+v %+v", q, q.Member)
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
		resp := f.bookPaid(t, body{"phoneNumber": phone, "date": date, "expectedAmount": q.Amount})
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

	f.bookings.failures = map[string]error{"MemberMonth": errors.New("down")}
	if rec, _ := f.quote(nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("a database error = %d, want 500", rec.Code)
	}
}

// ---- what the admin sees --------------------------------------------------------------

func TestTheAdminBookingListShowsWhatWasCharged(t *testing.T) {
	member := models.Booking{
		Team:           models.Team{FullName: "Wanjiru Njoroge", PhoneNumber: memberPhone},
		BookingDetails: models.BookingDetails{Date: nov(3), Time: "21:00 – 23:00", PitchType: "8Aside", BookingStatus: "Booked"},
		Transaction:    models.Transaction{Amount: 2_000, ListAmount: 10_000, Discount: models.DiscountMember},
	}
	v := adminBookingView(member)
	if v["amount"] != "2000" || v["discount"] != "member" || v["listAmount"] != "10000" {
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
