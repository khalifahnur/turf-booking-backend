package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The booking tests' clock is Sunday 1 November 2026, 10:00. 21:00 – 23:00 and
// 19:00 – 21:00 are sessions on every kind of day.

const (
	evening  = "21:00 – 23:00"
	teatime  = "19:00 – 21:00"
	hourSlot = "21:00 – 22:00"
)

type blockResult struct {
	Created []blockView    `json:"created"`
	Skipped []skippedBlock `json:"skipped"`
}

func (f *flow) createBlocks(over body) (*httptest.ResponseRecorder, blockResult) {
	b := body{"reason": "Paying first: Brian", "pitchType": "8Aside", "timeRange": evening, "dates": []string{nov(5)}}
	for k, v := range over {
		b[k] = v
	}
	rec := f.admin(http.MethodPost, "/api/v1/admin/blocks", b)
	var out blockResult
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// block blocks sessions that must all succeed.
func (f *flow) block(t *testing.T, over body) blockResult {
	t.Helper()
	rec, out := f.createBlocks(over)
	if rec.Code != http.StatusCreated {
		t.Fatalf("block %v = %d %q", over, rec.Code, rec.Body.String())
	}
	return out
}

func (f *flow) listBlocks(t *testing.T) []blockView {
	t.Helper()
	rec := f.admin(http.MethodGet, "/api/v1/admin/blocks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list blocks = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Blocks []blockView `json:"blocks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list body %q: %v", rec.Body.String(), err)
	}
	return out.Blocks
}

func TestBlockEndpointsNeedTheAdminKey(t *testing.T) {
	f := newFlow()
	f.block(t, nil)
	id := f.blocks.all()[0].ID.Hex()
	mux := f.adminMux()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/blocks", ""},
		{http.MethodPost, "/api/v1/admin/blocks", `{"reason":"Brian","pitchType":"8Aside","timeRange":"21:00 – 23:00","dates":["2026-11-06"]}`},
		{http.MethodDelete, "/api/v1/admin/blocks/" + id, ""},
		{http.MethodDelete, "/api/v1/admin/block-series/" + strings.Repeat("a", 32), ""},
	} {
		for name, auth := range map[string]string{"no key": "", "wrong key": "Bearer " + strings.Repeat("x", 32), "not a bearer token": testAdminKey} {
			r := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			if auth != "" {
				r.Header.Set("Authorization", auth)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with %s = %d, want 401", c.method, c.path, name, rec.Code)
			}
		}
	}
	if got := f.blocks.all(); len(got) != 1 {
		t.Errorf("blocks changed without the key: %+v", got)
	}
}

func TestABlockedSessionCannotBeBooked(t *testing.T) {
	f := newFlow()
	out := f.block(t, body{"dates": []string{nov(5)}})
	if len(out.Created) != 1 || len(out.Skipped) != 0 {
		t.Fatalf("result = %+v", out)
	}
	got := out.Created[0]
	if got.ID == "" || got.SeriesID == "" || got.Date != nov(5) || got.Time != evening || got.PitchType != "8Aside" || got.Reason != "Paying first: Brian" || !got.CreatedAt.Equal(flowNow) {
		t.Errorf("created block = %+v", got)
	}

	// The same session is refused for either pitch size, however it is paid for.
	for _, pitch := range []string{"8Aside", "5Aside"} {
		rec, _ := f.initiate(body{"date": nov(5), "pitchType": pitch})
		if rec.Code != http.StatusConflict {
			t.Errorf("booking the blocked session as %s = %d %q, want 409", pitch, rec.Code, rec.Body.String())
		}
	}
	if len(f.gateway.all()) != 0 || len(f.bookings.all()) != 0 {
		t.Error("a blocked session must never reach the payment gateway or the bookings")
	}

	// Everything around it is untouched: the day before, the same day an hour
	// earlier (the block starts at 21:00) and the session right after.
	f.book(t, body{"date": nov(4)})
	f.book(t, body{"date": nov(5), "timeRange": teatime})
	f.book(t, body{"date": nov(6)})
}

func TestABlockOverlappingASessionOfAnotherLengthBlocksIt(t *testing.T) {
	f := newFlow()
	f.block(t, body{"timeRange": evening, "dates": []string{nov(5)}})
	for _, hour := range []string{"21:00 – 22:00", "22:00 – 23:00"} {
		if rec, _ := f.initiate(body{"date": nov(5), "timeRange": hour, "pitchType": "5Aside"}); rec.Code != http.StatusConflict {
			t.Errorf("booking %s inside a blocked 2 hour session = %d, want 409", hour, rec.Code)
		}
	}
	if rec, _ := f.initiate(body{"date": nov(5), "timeRange": "23:00 – 00:00", "pitchType": "5Aside"}); rec.Code != http.StatusCreated {
		t.Errorf("the hour after the block = %d, want it bookable", rec.Code)
	}
}

func TestBlockingHalfThePitchLeavesTheOtherHalf(t *testing.T) {
	f := newFlow()
	f.block(t, body{"pitchType": "5Aside", "dates": []string{nov(5)}})

	if rec, _ := f.initiate(body{"date": nov(5), "pitchType": "8Aside"}); rec.Code != http.StatusConflict {
		t.Errorf("a whole-pitch booking next to a half block = %d, want 409", rec.Code)
	}
	f.book(t, body{"date": nov(5), "pitchType": "5Aside", "phoneNumber": plainPhone})
	if rec, _ := f.initiate(body{"date": nov(5), "pitchType": "5Aside", "phoneNumber": "254733000999"}); rec.Code != http.StatusConflict {
		t.Errorf("a second half booking = %d, want 409: one half is blocked, one is booked", rec.Code)
	}
}

func TestABlockCannotLandOnABookingOrAnotherBlock(t *testing.T) {
	f := newFlow()
	f.book(t, body{"date": nov(3)})                        // waiting for payment: still holds the pitch
	f.bookPaid(t, body{"date": nov(4)})                    // paid
	f.block(t, body{"dates": []string{nov(5)}})            // already blocked
	f.book(t, body{"date": nov(6), "pitchType": "5Aside"}) // one half taken

	rec, out := f.createBlocks(body{"dates": []string{nov(3), nov(4), nov(5), nov(6), nov(7)}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("block = %d %q", rec.Code, rec.Body.String())
	}
	if len(out.Created) != 1 || out.Created[0].Date != nov(7) {
		t.Errorf("created = %+v, want only the free date", out.Created)
	}
	var skipped []string
	for _, s := range out.Skipped {
		skipped = append(skipped, s.Date)
		if !strings.Contains(s.Reason, "already booked or blocked") {
			t.Errorf("skipped %s because %q", s.Date, s.Reason)
		}
	}
	if want := []string{nov(3), nov(4), nov(5), nov(6)}; !reflect.DeepEqual(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
	// Nothing that was booked was touched.
	if got := len(f.bookings.all()); got != 3 {
		t.Errorf("%d bookings, want the 3 that were made", got)
	}

	// A free half can still be blocked next to a half booking.
	if out := f.block(t, body{"pitchType": "5Aside", "dates": []string{nov(6)}}); len(out.Created) != 1 {
		t.Errorf("blocking the free half = %+v", out)
	}
}

func TestNothingBlockedIsRefusedWithTheReason(t *testing.T) {
	f := newFlow()
	f.bookPaid(t, body{"date": nov(4)})

	rec, _ := f.createBlocks(body{"dates": []string{nov(4)}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "Nothing was blocked: already booked or blocked at that time") {
		t.Errorf("one taken date = %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get(middleware.UserMessageHeader) != "1" {
		t.Error("the refusal is for the admin to read, so it must be marked")
	}
	rec, _ = f.createBlocks(body{"dates": []string{nov(4), "2026-10-01"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "None of the 2 sessions could be blocked") {
		t.Errorf("two refused dates = %d %q", rec.Code, rec.Body.String())
	}
	if len(f.blocks.all()) != 0 {
		t.Error("a refused request blocked something")
	}
}

func TestABlockedSeriesSkipsDatesThatDoNotQualify(t *testing.T) {
	f := newFlow()
	// Mondays to Thursdays for three weeks; 2026-10-30 is in the past, and
	// 2027-06-01 is further out than a session can be blocked.
	dates := []string{nov(2), nov(3), nov(4), nov(5), nov(9), nov(10), nov(11), nov(12), nov(16), nov(17), nov(18), nov(19), "2026-10-30", "2027-06-01", nov(2), "not-a-date"}
	out := f.block(t, body{"reason": "Kibera Utd weekly", "dates": dates})

	if len(out.Created) != 12 {
		t.Fatalf("created %d, want 12", len(out.Created))
	}
	series := out.Created[0].SeriesID
	for i, b := range out.Created {
		if b.SeriesID != series {
			t.Errorf("block %d is in series %q, want them all in %q", i, b.SeriesID, series)
		}
		if i > 0 && out.Created[i-1].Date >= b.Date {
			t.Errorf("blocks are not in date order, with the date repeated once: %v then %v", out.Created[i-1].Date, b.Date)
		}
	}
	reasons := map[string]string{}
	for _, s := range out.Skipped {
		reasons[s.Date] = s.Reason
	}
	if len(out.Skipped) != 3 || !strings.Contains(reasons["2026-10-30"], "already passed") || !strings.Contains(reasons["2027-06-01"], "ahead") || reasons["not-a-date"] != "invalid date" {
		t.Errorf("skipped = %+v", out.Skipped)
	}
}

func TestBlocksAreListedInTheOrderTheVenueRunsThem(t *testing.T) {
	f := newFlow()
	f.block(t, body{"reason": "Late game", "timeRange": "00:00 – 01:00", "dates": []string{nov(5)}})
	f.block(t, body{"reason": "Evening", "timeRange": evening, "dates": []string{nov(5)}, "pitchType": "5Aside"})
	f.block(t, body{"reason": "Earlier day", "dates": []string{nov(4)}})
	// Yesterday's blocks are past and no longer listed.
	f.blocks.rows = append(f.blocks.rows, models.Block{ID: primitive.NewObjectID(), Date: "2026-10-31", Time: evening, PitchType: "8Aside", Reason: "Old"})

	var got []string
	for _, b := range f.listBlocks(t) {
		got = append(got, b.Reason)
	}
	// 00:00 – 01:00 on the 5th is the early hours of the 6th: the last session of that evening.
	if want := []string{"Earlier day", "Evening", "Late game"}; !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
	if empty := newFlow().listBlocks(t); empty == nil || len(empty) != 0 {
		t.Errorf("no blocks must list as an empty array, got %#v", empty)
	}
}

func TestBlockedSessionsShowOnThePublicCalendarWithoutTheNote(t *testing.T) {
	f := newFlow()
	f.block(t, body{"reason": "Secret: Brian 0712 345 678", "pitchType": "5Aside", "dates": []string{nov(5)}})
	f.blocks.rows = append(f.blocks.rows, models.Block{ID: primitive.NewObjectID(), Date: "2026-10-31", Time: evening, PitchType: "8Aside"})

	slots, err := f.h.blockedSlots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []map[string]string{{"date": nov(5), "time": evening, "status": "Blocked", "pitchType": "5Aside"}}
	if !reflect.DeepEqual(slots, want) {
		t.Errorf("public slots = %v, want %v", slots, want)
	}
	if raw, _ := json.Marshal(slots); strings.Contains(string(raw), "Brian") {
		t.Errorf("the admin's note reached the public list: %s", raw)
	}
	f.h.blocks = nil
	if slots, err := f.h.blockedSlots(context.Background()); err != nil || len(slots) != 0 {
		t.Errorf("without a block list = %v, %v", slots, err)
	}
}

func TestLiftingABlockFreesTheSession(t *testing.T) {
	f := newFlow()
	f.block(t, body{"dates": []string{nov(5)}})
	id := f.blocks.all()[0].ID.Hex()
	if rec, _ := f.initiate(body{"date": nov(5)}); rec.Code != http.StatusConflict {
		t.Fatalf("setup: the blocked session = %d", rec.Code)
	}

	for _, c := range []struct {
		path string
		want int
	}{
		{"/api/v1/admin/blocks/" + id, http.StatusNoContent},
		{"/api/v1/admin/blocks/" + id, http.StatusNotFound}, // already gone
		{"/api/v1/admin/blocks/" + primitive.NewObjectID().Hex(), http.StatusNotFound},
		{"/api/v1/admin/blocks/12345", http.StatusBadRequest},
		{"/api/v1/admin/blocks/not-an-id", http.StatusBadRequest},
	} {
		rec := f.admin(http.MethodDelete, c.path, nil)
		if rec.Code != c.want {
			t.Errorf("DELETE %s = %d %q, want %d", c.path, rec.Code, rec.Body.String(), c.want)
		}
		if c.want >= 400 && rec.Header().Get(middleware.UserMessageHeader) != "1" {
			t.Errorf("DELETE %s: the refusal is for the admin to read, so it must be marked", c.path)
		}
	}
	f.book(t, body{"date": nov(5)})
}

func TestLiftingASeriesKeepsThePastAndOtherSeries(t *testing.T) {
	f := newFlow()
	team := f.block(t, body{"reason": "Team A", "dates": []string{nov(2), nov(9), nov(16)}}).Created[0].SeriesID
	other := f.block(t, body{"reason": "Team B", "timeRange": teatime, "dates": []string{nov(2), nov(9)}}).Created[0].SeriesID
	// Part of Team A's series that has already been played.
	f.blocks.rows = append(f.blocks.rows, models.Block{ID: primitive.NewObjectID(), Date: "2026-10-26", Time: evening, PitchType: "8Aside", Reason: "Team A", SeriesID: team})

	rec := f.admin(http.MethodDelete, "/api/v1/admin/block-series/"+team, nil)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"removed":3}` {
		t.Fatalf("lift series = %d %q, want 3 removed", rec.Code, rec.Body.String())
	}
	var left []string
	for _, b := range f.blocks.all() {
		left = append(left, b.Reason+" "+b.Date)
	}
	if want := []string{"Team B 2026-11-02", "Team B 2026-11-09", "Team A 2026-10-26"}; !reflect.DeepEqual(left, want) {
		t.Errorf("left = %v, want %v", left, want)
	}
	// Team A's sessions are free; Team B's are not.
	f.book(t, body{"date": nov(2)})
	if rec, _ := f.initiate(body{"date": nov(2), "timeRange": teatime}); rec.Code != http.StatusConflict {
		t.Errorf("Team B's session = %d, want it still blocked", rec.Code)
	}

	for _, c := range []struct {
		series string
		want   int
	}{
		{team, http.StatusNotFound}, // nothing of it left to lift
		{strings.Repeat("b", 32), http.StatusNotFound},
		{"nothex", http.StatusBadRequest},
		{strings.Repeat("a", 31), http.StatusBadRequest},
		{strings.Repeat("a", 34), http.StatusBadRequest},
	} {
		if rec := f.admin(http.MethodDelete, "/api/v1/admin/block-series/"+c.series, nil); rec.Code != c.want {
			t.Errorf("lift series %q = %d %q, want %d", c.series, rec.Code, rec.Body.String(), c.want)
		}
	}
	_ = other
}

func TestBlockingRefusesBadRequests(t *testing.T) {
	f := newFlow()
	many := make([]string, 0, maxBlockDates+1)
	for i := 0; i <= maxBlockDates; i++ {
		many = append(many, schedule.Today(flowNow.AddDate(0, 0, i)))
	}
	for name, c := range map[string]struct {
		over body
		want int
	}{
		"no note":               {body{"reason": ""}, http.StatusBadRequest},
		"one letter note":       {body{"reason": "B"}, http.StatusBadRequest},
		"markup in the note":    {body{"reason": "<script>x</script>"}, http.StatusBadRequest},
		"very long note":        {body{"reason": strings.Repeat("a", 81)}, http.StatusBadRequest},
		"a made-up pitch":       {body{"pitchType": "11Aside"}, http.StatusBadRequest},
		"no pitch":              {body{"pitchType": ""}, http.StatusBadRequest},
		"no dates":              {body{"dates": []string{}}, http.StatusBadRequest},
		"too many dates":        {body{"dates": many}, http.StatusBadRequest},
		"a time off the grid":   {body{"timeRange": "21:30 – 22:30"}, http.StatusConflict},
		"a 3 hour session":      {body{"timeRange": "21:00 – 00:00"}, http.StatusConflict},
		"not a time":            {body{"timeRange": "evening"}, http.StatusConflict},
		"oversized body":        {body{"reason": strings.Repeat("a", 10_000)}, http.StatusRequestEntityTooLarge},
		"unknown field is fine": {body{"extra": true}, http.StatusCreated},
	} {
		if rec, _ := f.createBlocks(c.over); rec.Code != c.want {
			t.Errorf("%s = %d %q, want %d", name, rec.Code, rec.Body.String(), c.want)
		}
	}
	// Only the one request that was fine blocked anything.
	if got := len(f.blocks.all()); got != 1 {
		t.Errorf("%d blocks, want 1", got)
	}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/blocks", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+testAdminKey)
	r.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	f.adminMux().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d, want 415", rec.Code)
	}
}

// A session that is about to start, or has started, can still be held, which a
// customer could no longer book; one that has finished cannot.
func TestBlockingHasNoCutOffButRefusesFinishedSessions(t *testing.T) {
	f := newFlow()
	at := func(h, m int) {
		f.h.now = func() time.Time { return time.Date(2026, 11, 3, h, m, 0, 0, schedule.Location()) }
	}

	at(21, 15) // the 21:00 – 23:00 session is under way
	if rec, _ := f.createBlocks(body{"dates": []string{nov(3)}}); rec.Code != http.StatusCreated {
		t.Errorf("blocking a session under way = %d %q", rec.Code, rec.Body.String())
	}
	at(22, 50) // 10 minutes left: too late to book, fine to hold
	if rec, _ := f.createBlocks(body{"timeRange": teatime, "dates": []string{nov(3)}}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already finished") {
		t.Errorf("blocking a finished session = %d %q, want 409 already finished", rec.Code, rec.Body.String())
	}
}

func TestBlockingFailsCleanlyWhenTheDatabaseDoes(t *testing.T) {
	for name, failure := range map[string]func(f *flow){
		"the block list":  func(f *flow) { f.blocks.failures = map[string]error{"OnDate": errors.New("down")} },
		"the bookings":    func(f *flow) { f.bookings.failures = map[string]error{"Occupancy": errors.New("down")} },
		"writing a block": func(f *flow) { f.blocks.failures = map[string]error{"Insert": errors.New("down")} },
	} {
		f := newFlow()
		failure(f)
		rec, _ := f.createBlocks(nil)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s down: %d %q, want 500", name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "down") {
			t.Errorf("%s: the database's error text leaked: %q", name, rec.Body.String())
		}
	}

	f := newFlow()
	f.block(t, nil)
	id := f.blocks.all()[0].ID.Hex()
	f.blocks.failures = map[string]error{"From": errors.New("down"), "Remove": errors.New("down"), "RemoveSeries": errors.New("down")}
	for _, rec := range []*httptest.ResponseRecorder{
		f.admin(http.MethodGet, "/api/v1/admin/blocks", nil),
		f.admin(http.MethodDelete, "/api/v1/admin/blocks/"+id, nil),
		f.admin(http.MethodDelete, "/api/v1/admin/block-series/"+strings.Repeat("a", 32), nil),
	} {
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "down") {
			t.Errorf("status %d %q, want a plain 500", rec.Code, rec.Body.String())
		}
	}
	if _, err := f.h.blockedSlots(context.Background()); err == nil {
		t.Error("the public calendar must fail rather than show a pitch as free when the block list is unreadable")
	}
}

// If the block list cannot be read, a booking must not go through: the pitch
// might be held.
func TestBookingFailsClosedWhenTheBlockListIsUnreadable(t *testing.T) {
	f := newFlow()
	f.blocks.failures = map[string]error{"OnDate": errors.New("down")}
	rec, _ := f.initiate(nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("booking with the block list down = %d, want 500", rec.Code)
	}
	if len(f.bookings.all()) != 0 || len(f.gateway.all()) != 0 {
		t.Error("nothing may be booked or charged")
	}
}

func TestBlockingIsSwitchedOffWithoutABlockList(t *testing.T) {
	f := newFlow()
	f.h.blocks = nil
	for _, rec := range []*httptest.ResponseRecorder{
		f.admin(http.MethodGet, "/api/v1/admin/blocks", nil),
		f.admin(http.MethodPost, "/api/v1/admin/blocks", body{"reason": "Brian", "pitchType": "8Aside", "timeRange": evening, "dates": []string{nov(5)}}),
		f.admin(http.MethodDelete, "/api/v1/admin/blocks/"+primitive.NewObjectID().Hex(), nil),
		f.admin(http.MethodDelete, "/api/v1/admin/block-series/"+strings.Repeat("a", 32), nil),
	} {
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503", rec.Code)
		}
	}
	// Booking itself is unaffected.
	f.book(t, nil)
}

// A customer paying for a session at the moment the admin blocks it: exactly one
// of them gets it, and the other is told it is taken.
func TestABlockAndABookingForTheSameSessionCannotBothSucceed(t *testing.T) {
	f := newFlow()
	f.h.phoneLimiter = middleware.NewLimiter(6_000_000, 1_000_000)
	for round := 0; round < 15; round++ {
		date := schedule.Today(flowNow.AddDate(0, 0, 1+round))
		var wg sync.WaitGroup
		var bookCode, blockCode int
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			rec, _ := f.initiate(body{"date": date})
			bookCode = rec.Code
		}()
		go func() {
			defer wg.Done()
			<-start
			rec, _ := f.createBlocks(body{"dates": []string{date}})
			blockCode = rec.Code
		}()
		close(start)
		wg.Wait()

		won := 0
		if bookCode == http.StatusCreated {
			won++
		}
		if blockCode == http.StatusCreated {
			won++
		}
		if won != 1 {
			t.Fatalf("%s: booking = %d, block = %d; exactly one must win", date, bookCode, blockCode)
		}
	}
}

// ---- the Mongo queries and the rules around them -------------------------------------

func TestBlockQueriesUseRealFieldNames(t *testing.T) {
	paths := map[string]bool{}
	bsonPaths(reflect.TypeOf(models.Block{}), "", paths)
	for name, q := range map[string]any{
		"blockDateFilter":   blockDateFilter("2026-11-03"),
		"blockFromFilter":   blockFromFilter("2026-11-03"),
		"blockIDFilter":     blockIDFilter(primitive.NewObjectID()),
		"blockSeriesFilter": blockSeriesFilter("abc", "2026-11-03"),
	} {
		var refs []string
		fieldRefs(q, &refs)
		if len(refs) == 0 {
			t.Errorf("%s mentions no fields; the walker is broken", name)
		}
		for _, ref := range refs {
			if !paths[ref] {
				t.Errorf("%s uses field %q, which is not a bson field of Block", name, ref)
			}
		}
	}
}

func TestBlockOccupancyTreatsUnreadableDataAsTheWholeDay(t *testing.T) {
	got := blockOccupancy([]models.Block{
		{Time: evening, PitchType: "5Aside"},
		{Time: "garbage", PitchType: "8Aside"},
		{Time: evening, PitchType: "mystery"},
	}, nov(5))
	want := []schedule.Occupancy{
		{Start: 21 * 60, End: 23 * 60, Units: 1},
		{Start: 0, End: schedule.DayEnd, Units: 2},
		{Start: 21 * 60, End: 23 * 60, Units: schedule.PitchCapacity},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("occupancy = %+v, want %+v", got, want)
	}
}
