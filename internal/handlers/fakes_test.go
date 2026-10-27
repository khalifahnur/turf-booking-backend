package handlers

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// ---- a tiny MongoDB filter evaluator ---------------------------------------
//
// The in-memory stores below answer questions by running the very filters the
// MongoDB stores send (occupancyFilter, memberSessionsFilter) against the
// bookings as BSON documents. A misspelt field name or a wrong operator in a
// filter therefore fails these tests instead of silently matching nothing.
// It understands only what those filters use: equality, $gte, $lt and $in.

func docOf(v any) bson.M {
	raw, err := bson.Marshal(v)
	if err != nil {
		panic(err)
	}
	var doc bson.M
	if err := bson.Unmarshal(raw, &doc); err != nil {
		panic(err)
	}
	return doc
}

func field(doc bson.M, path string) (any, bool) {
	var cur any = doc
	for _, part := range strings.Split(path, ".") {
		switch m := cur.(type) {
		case bson.M:
			v, ok := m[part]
			if !ok {
				return nil, false
			}
			cur = v
		case map[string]any:
			v, ok := m[part]
			if !ok {
				return nil, false
			}
			cur = v
		default:
			return nil, false
		}
	}
	return cur, true
}

func inList(list any, got any) bool {
	rv := reflect.ValueOf(list)
	for i := 0; i < rv.Len(); i++ {
		if reflect.DeepEqual(rv.Index(i).Interface(), got) {
			return true
		}
	}
	return false
}

func matches(doc, filter bson.M) bool {
	for path, want := range filter {
		got, present := field(doc, path)
		ops, isOps := want.(bson.M)
		if !isOps {
			if !present || !reflect.DeepEqual(got, want) {
				return false
			}
			continue
		}
		for op, arg := range ops {
			if !present {
				return false
			}
			switch op {
			case "$in":
				if !inList(arg, got) {
					return false
				}
			case "$gte", "$lt":
				g, ok1 := got.(string)
				a, ok2 := arg.(string)
				if !ok1 || !ok2 {
					panic(fmt.Sprintf("the test evaluator compares strings only: %T %T", got, arg))
				}
				if (op == "$gte" && g < a) || (op == "$lt" && g >= a) {
					return false
				}
			default:
				panic("operator not supported by the test evaluator: " + op)
			}
		}
	}
	return true
}

// ---- in-memory bookings -----------------------------------------------------

type fakeBookings struct {
	mu   sync.Mutex
	rows []models.Booking
	// widen makes looking up a member's standing take a moment, so two requests
	// that are not serialised have time to both read the same standing.
	widen time.Duration
	// failures makes a method fail: "Occupancy", "Insert", "MemberMonth", ...
	failures map[string]error
}

func (f *fakeBookings) fail(method string) error {
	if err, ok := f.failures[method]; ok {
		return err
	}
	return nil
}

func (f *fakeBookings) all() []models.Booking {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]models.Booking(nil), f.rows...)
}

func (f *fakeBookings) Occupancy(_ context.Context, date string) ([]schedule.Occupancy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Occupancy"); err != nil {
		return nil, err
	}
	var hit []models.Booking
	filter := occupancyFilter(date)
	for _, b := range f.rows {
		if matches(docOf(b), filter) {
			hit = append(hit, b)
		}
	}
	return toOccupancy(hit, date), nil
}

func (f *fakeBookings) Insert(_ context.Context, b models.Booking) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("Insert"); err != nil {
		return err
	}
	f.rows = append(f.rows, b)
	return nil
}

func (f *fakeBookings) MarkFailed(_ context.Context, reference string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.rows {
		if matches(docOf(f.rows[i]), failBookingFilter(reference)) {
			f.rows[i].Transaction.PaymentStatus = "Failed"
			f.rows[i].BookingDetails.BookingStatus = "Failed"
		}
	}
	return nil
}

func (f *fakeBookings) MemberMonth(_ context.Context, phone, month string) (MemberMonth, error) {
	months, err := f.memberMonths(phone, month)
	// The standing is out of date as soon as it is read. Pausing here, after the
	// read, gives every request that is not serialised time to read the same
	// standing, however many CPUs the test runs on.
	if f.widen > 0 {
		time.Sleep(f.widen)
	}
	return months[phone], err
}

func (f *fakeBookings) MemberMonths(_ context.Context, month string) (map[string]MemberMonth, error) {
	return f.memberMonths("", month)
}

// memberMonths runs the filter the MongoDB store sends over the bookings, and
// summarises what it matches with the same function the store uses.
func (f *fakeBookings) memberMonths(phone, month string) (map[string]MemberMonth, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("MemberMonth"); err != nil {
		return nil, err
	}
	from, to, err := schedule.MonthBounds(month)
	if err != nil {
		return nil, err
	}
	var hit []models.Booking
	filter := memberMonthFilter(phone, from, to)
	for _, b := range f.rows {
		if matches(docOf(b), filter) {
			hit = append(hit, b)
		}
	}
	return summariseMemberMonths(hit), nil
}

// ---- in-memory members ------------------------------------------------------

type fakeMembers struct {
	mu       sync.Mutex
	byPhone  map[string]models.Member
	failures map[string]error
}

func newFakeMembers(phones ...string) *fakeMembers {
	f := &fakeMembers{byPhone: map[string]models.Member{}}
	for _, p := range phones {
		f.byPhone[p] = models.Member{
			ID: primitive.NewObjectID(), PhoneNumber: p, FullName: "Member " + p[len(p)-3:],
			CreatedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		}
	}
	return f
}

func (f *fakeMembers) Find(_ context.Context, phone string) (*models.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["Find"]; err != nil {
		return nil, err
	}
	if m, ok := f.byPhone[phone]; ok {
		return &m, nil
	}
	return nil, nil
}

func (f *fakeMembers) List(context.Context) ([]models.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["List"]; err != nil {
		return nil, err
	}
	out := make([]models.Member, 0, len(f.byPhone))
	for _, m := range f.byPhone {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PhoneNumber < out[j].PhoneNumber })
	return out, nil
}

func (f *fakeMembers) Add(_ context.Context, m models.Member) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["Add"]; err != nil {
		return err
	}
	if _, ok := f.byPhone[m.PhoneNumber]; ok {
		return errMemberExists
	}
	m.ID = primitive.NewObjectID()
	f.byPhone[m.PhoneNumber] = m
	return nil
}

func (f *fakeMembers) Remove(_ context.Context, phone string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["Remove"]; err != nil {
		return false, err
	}
	_, ok := f.byPhone[phone]
	delete(f.byPhone, phone)
	return ok, nil
}

// ---- the payment gateway ------------------------------------------------------

// fakePayments records what the customer's phone would be asked for.
type fakePayments struct {
	mu    sync.Mutex
	calls []payCall
	n     int
	err   error
	delay time.Duration
	// gate, if set, runs at the start of every payment request with the request's
	// number (from 0), and may block it.
	gate func(n int)
}

func (p *fakePayments) pay(_ *config.Config, phone, email string, cents int, reference string) (string, error) {
	p.mu.Lock()
	n := p.n
	p.n++
	p.mu.Unlock()
	if p.gate != nil {
		p.gate(n)
	}
	if p.delay > 0 {
		time.Sleep(p.delay)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return "", p.err
	}
	p.calls = append(p.calls, payCall{phone, email, cents, reference})
	return "ok", nil
}

func (p *fakePayments) all() []payCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]payCall(nil), p.calls...)
}
