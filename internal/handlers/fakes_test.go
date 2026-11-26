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
	"turf-booking-backend/internal/registration"
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
// It understands only what those filters use: equality, $ne, $gte, $lt, $in and $or.

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
	// A field that holds an array matches $in when any of its elements is listed.
	if arr, ok := got.(primitive.A); ok {
		for _, el := range arr {
			if inList(list, el) {
				return true
			}
		}
		return false
	}
	for i := 0; i < rv.Len(); i++ {
		if reflect.DeepEqual(rv.Index(i).Interface(), got) {
			return true
		}
	}
	return false
}

// asTime reads a stored date (the driver gives back primitive.DateTime) or a
// filter's time.Time.
func asTime(v any) (time.Time, bool) {
	switch x := v.(type) {
	case time.Time:
		return x, true
	case primitive.DateTime:
		return x.Time(), true
	}
	return time.Time{}, false
}

func matches(doc, filter bson.M) bool {
	for path, want := range filter {
		if path == "$or" {
			any := false
			for _, clause := range want.(bson.A) {
				if matches(doc, clause.(bson.M)) {
					any = true
					break
				}
			}
			if !any {
				return false
			}
			continue
		}
		got, present := field(doc, path)
		ops, isOps := want.(bson.M)
		if !isOps {
			if !present || !reflect.DeepEqual(got, want) {
				return false
			}
			continue
		}
		for op, arg := range ops {
			if op == "$ne" {
				if present && reflect.DeepEqual(got, arg) {
					return false
				}
				continue
			}
			if !present {
				return false
			}
			switch op {
			case "$in":
				if !inList(arg, got) {
					return false
				}
			case "$gte", "$lt":
				if gt, ok := asTime(got); ok {
					at, ok2 := asTime(arg)
					if !ok2 {
						panic(fmt.Sprintf("the test evaluator cannot compare %T with %T", got, arg))
					}
					if (op == "$gte" && gt.Before(at)) || (op == "$lt" && !gt.Before(at)) {
						return false
					}
					continue
				}
				g, ok1 := got.(string)
				a, ok2 := arg.(string)
				if !ok1 || !ok2 {
					panic(fmt.Sprintf("the test evaluator compares strings and dates only: %T %T", got, arg))
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

// ---- in-memory blocks ---------------------------------------------------------

// fakeBlocks runs the same filters the MongoDB block store sends.
type fakeBlocks struct {
	mu       sync.Mutex
	rows     []models.Block
	failures map[string]error
}

func (f *fakeBlocks) pick(filter bson.M) []models.Block {
	var hit []models.Block
	for _, b := range f.rows {
		if matches(docOf(b), filter) {
			hit = append(hit, b)
		}
	}
	return hit
}

func (f *fakeBlocks) all() []models.Block {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]models.Block(nil), f.rows...)
}

func (f *fakeBlocks) OnDate(_ context.Context, date string) ([]models.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["OnDate"]; err != nil {
		return nil, err
	}
	return f.pick(blockDateFilter(date)), nil
}

func (f *fakeBlocks) From(_ context.Context, date string) ([]models.Block, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["From"]; err != nil {
		return nil, err
	}
	return f.pick(blockFromFilter(date)), nil
}

func (f *fakeBlocks) Insert(_ context.Context, b models.Block) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["Insert"]; err != nil {
		return err
	}
	f.rows = append(f.rows, b)
	return nil
}

func (f *fakeBlocks) Remove(_ context.Context, id primitive.ObjectID) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["Remove"]; err != nil {
		return false, err
	}
	kept, removed := f.rows[:0], false
	for _, b := range f.rows {
		if matches(docOf(b), blockIDFilter(id)) {
			removed = true
			continue
		}
		kept = append(kept, b)
	}
	f.rows = kept
	return removed, nil
}

func (f *fakeBlocks) RemoveSeries(_ context.Context, seriesID, fromDate string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["RemoveSeries"]; err != nil {
		return 0, err
	}
	kept, n := f.rows[:0], 0
	for _, b := range f.rows {
		if matches(docOf(b), blockSeriesFilter(seriesID, fromDate)) {
			n++
			continue
		}
		kept = append(kept, b)
	}
	f.rows = kept
	return n, nil
}

// ---- in-memory registrations --------------------------------------------------

type fakeRegistrations struct {
	mu       sync.Mutex
	rows     []models.Registration
	failures map[string]error
}

func (f *fakeRegistrations) all() []models.Registration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]models.Registration(nil), f.rows...)
}

func (f *fakeRegistrations) pick(filter bson.M) []models.Registration {
	var hit []models.Registration
	for _, r := range f.rows {
		if matches(docOf(r), filter) {
			hit = append(hit, r)
		}
	}
	return hit
}

func (f *fakeRegistrations) Insert(_ context.Context, reg models.Registration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["Insert"]; err != nil {
		return err
	}
	f.rows = append(f.rows, reg)
	return nil
}

func (f *fakeRegistrations) TournamentSlots(_ context.Context, pendingAfter time.Time, excluding primitive.ObjectID) (map[string]registration.Slots, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["TournamentSlots"]; err != nil {
		return nil, err
	}
	return countSlots(f.pick(tournamentSlotsFilter(pendingAfter, excluding))), nil
}

func (f *fakeRegistrations) TeamHolds(_ context.Context, teamKey string, wanted []string, pendingAfter time.Time) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["TeamHolds"]; err != nil {
		return nil, err
	}
	return held(wanted, f.pick(teamHoldsFilter(teamKey, wanted, pendingAfter))), nil
}

func (f *fakeRegistrations) PlayerHolds(_ context.Context, playerKey, phone string, pendingAfter time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["PlayerHolds"]; err != nil {
		return false, err
	}
	return len(f.pick(playerHoldsFilter(playerKey, phone, pendingAfter))) > 0, nil
}

func (f *fakeRegistrations) MarkPaid(_ context.Context, reference string, paidCents int64, now time.Time) (*models.Registration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["MarkPaid"]; err != nil {
		return nil, err
	}
	// The evaluator cannot run the $expr that compares the amount, so that part of
	// the filter is applied by hand (its shape is checked in the query tests).
	filter := registrationPaidFilter(reference, paidCents)
	delete(filter, "$expr")
	for i := range f.rows {
		r := &f.rows[i]
		if !matches(docOf(*r), filter) {
			continue
		}
		if paidCents > 0 && paidCents < int64(r.AmountKES)*100 {
			return nil, nil
		}
		r.Status = registration.StatusPaid
		r.Payment.PaidAmountCents = paidCents
		r.Payment.PaidAt = &now
		r.UpdatedAt = now
		out := *r
		return &out, nil
	}
	return nil, nil
}

func (f *fakeRegistrations) MarkFailed(_ context.Context, reference string, now time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["MarkFailed"]; err != nil {
		return false, err
	}
	for i := range f.rows {
		if matches(docOf(f.rows[i]), registrationFailedFilter(reference)) {
			f.rows[i].Status = registration.StatusFailed
			f.rows[i].UpdatedAt = now
			return true, nil
		}
	}
	return false, nil
}

func (f *fakeRegistrations) Status(_ context.Context, reference string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["Status"]; err != nil {
		return "", err
	}
	if hit := f.pick(registrationReferenceFilter(reference)); len(hit) > 0 {
		return hit[0].Status, nil
	}
	return "", errUnknownReference
}

func (f *fakeRegistrations) FlagOverflow(_ context.Context, id primitive.ObjectID, categories []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["FlagOverflow"]; err != nil {
		return err
	}
	for i := range f.rows {
		if matches(docOf(f.rows[i]), registrationIDFilter(id)) {
			f.rows[i].Overflow = categories
		}
	}
	return nil
}

func (f *fakeRegistrations) ListForAdmin(_ context.Context, kind string) ([]models.Registration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failures["ListForAdmin"]; err != nil {
		return nil, err
	}
	out := f.pick(registrationAdminFilter(kind))
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
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
