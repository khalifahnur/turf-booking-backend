package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/schedule"

	"go.mongodb.org/mongo-driver/bson"
)

// adminMux routes the member endpoints the way main does.
func (f *flow) adminMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/admin/members", f.h.AdminOnly(f.h.ListMembers))
	mux.Handle("POST /api/v1/admin/members", f.h.AdminOnly(f.h.AddMember))
	mux.Handle("DELETE /api/v1/admin/members/{phone}", f.h.AdminOnly(f.h.RemoveMember))
	return mux
}

func (f *flow) admin(method, path string, payload any) *httptest.ResponseRecorder {
	var r *http.Request
	if payload == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = jsonRequest(method, path, payload)
	}
	r.Header.Set("Authorization", "Bearer "+testAdminKey)
	rec := httptest.NewRecorder()
	f.adminMux().ServeHTTP(rec, r)
	return rec
}

type memberJSON struct {
	ID          string    `json:"id"`
	FullName    string    `json:"fullName"`
	PhoneNumber string    `json:"phoneNumber"`
	CreatedAt   time.Time `json:"createdAt"`
	// Sessions are the 8-a-side 2 hour sessions paid for this month; DiscountUsed
	// says whether the month's discount has been given.
	Sessions     int  `json:"sessions"`
	DiscountUsed bool `json:"discountUsed"`
}

type memberListJSON struct {
	Month           string       `json:"month"`
	Percent         int          `json:"percent"`
	DiscountSession int          `json:"discountSession"`
	Members         []memberJSON `json:"members"`
}

func (f *flow) list(t *testing.T) memberListJSON {
	t.Helper()
	rec := f.admin(http.MethodGet, "/api/v1/admin/members", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	var out memberListJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("list body %q: %v", rec.Body.String(), err)
	}
	return out
}

func TestMemberAdminEndpointsNeedTheAdminKey(t *testing.T) {
	f := newFlow(memberPhone)
	mux := f.adminMux()
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/admin/members", ""},
		{http.MethodPost, "/api/v1/admin/members", `{"fullName":"Amina Otieno","phoneNumber":"254700111222"}`},
		{http.MethodDelete, "/api/v1/admin/members/" + memberPhone, ""},
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
	// Nothing was changed by any of them.
	if got := f.list(t); len(got.Members) != 1 {
		t.Errorf("the member list changed: %+v", got.Members)
	}

	// With no ADMIN_API_KEY configured the endpoints are switched off.
	f.h.Config.AdminAPIKey = ""
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/members", nil)
	r.Header.Set("Authorization", "Bearer anything")
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("with no key configured = %d, want 503", rec.Code)
	}
}

func TestTheMemberListShowsWhereEachMemberStandsThisMonth(t *testing.T) {
	f := newFlow()
	for _, m := range []struct{ name, phone string }{
		// Not in the order of their numbers, so a list sorted by number would be caught.
		{"zainab Hassan", "254700111222"}, {"amina Otieno", "254733000222"}, {"Brian Kamau", memberPhone},
	} {
		if rec := f.admin(http.MethodPost, "/api/v1/admin/members", body{"fullName": m.name, "phoneNumber": m.phone}); rec.Code != http.StatusCreated {
			t.Fatalf("add %s = %d %s", m.name, rec.Code, rec.Body.String())
		}
	}
	f.bookPaid(t, body{"phoneNumber": memberPhone, "date": nov(3)})
	f.bookPaid(t, body{"phoneNumber": memberPhone, "date": nov(4)})
	f.book(t, body{"phoneNumber": memberPhone, "date": nov(5)}) // not paid yet: does not count
	f.bookPaid(t, body{"phoneNumber": "254700111222", "date": nov(6)})
	f.bookPaid(t, body{"phoneNumber": memberPhone, "date": "2026-12-01"}) // next month: not this month's
	f.bookPaid(t, body{"phoneNumber": plainPhone, "date": nov(7)})        // not a member: not in the list
	f.bookPaid(t, body{"phoneNumber": "254733000222", "date": nov(8)})    // amina: three paid, then the discounted fourth
	f.bookPaid(t, body{"phoneNumber": "254733000222", "date": nov(9)})
	f.bookPaid(t, body{"phoneNumber": "254733000222", "date": nov(10)})
	if resp := f.book(t, body{"phoneNumber": "254733000222", "date": nov(11)}); resp.Amount != 2_000 {
		t.Fatalf("setup: amina's fourth cost %d", resp.Amount)
	}

	got := f.list(t)
	if got.Month != "2026-11" || got.Percent != 80 || got.DiscountSession != 4 {
		t.Errorf("rule = %q %d%% session %d", got.Month, got.Percent, got.DiscountSession)
	}
	var names []string
	byPhone := map[string]memberJSON{}
	for _, m := range got.Members {
		names = append(names, m.FullName)
		byPhone[m.PhoneNumber] = m
		if m.ID == "" || m.CreatedAt.IsZero() {
			t.Errorf("member %+v is missing its id or creation time", m)
		}
	}
	if want := []string{"amina Otieno", "Brian Kamau", "zainab Hassan"}; !reflect.DeepEqual(names, want) {
		t.Errorf("order = %v, want by name without regard to case: %v", names, want)
	}
	// The discounted fourth is still waiting for its payment, so amina has paid
	// for three; the discount has been given.
	if m := byPhone[memberPhone]; m.Sessions != 2 || m.DiscountUsed {
		t.Errorf("Brian = %+v, want 2 paid sessions and no discount yet", m)
	}
	if m := byPhone["254700111222"]; m.Sessions != 1 || m.DiscountUsed {
		t.Errorf("zainab = %+v, want 1 paid session", m)
	}
	if m := byPhone["254733000222"]; m.Sessions != 3 || !m.DiscountUsed {
		t.Errorf("amina = %+v, want 3 paid sessions and the discount given", m)
	}
}

// "This month" is the venue's month by its operating day: at half past midnight
// on 1 December the venue is still running 30 November's evening.
func TestTheMemberListMonthFollowsTheOperatingDay(t *testing.T) {
	f := newFlow(memberPhone)
	f.bookPaid(t, body{"date": nov(30)})
	f.bookPaid(t, body{"date": "2026-12-01"})
	f.bookPaid(t, body{"date": "2026-12-02"})

	for _, c := range []struct {
		now          time.Time
		wantMonth    string
		wantSessions int
	}{
		{time.Date(2026, 11, 20, 12, 0, 0, 0, schedule.Location()), "2026-11", 1},
		{time.Date(2026, 12, 1, 0, 30, 0, 0, schedule.Location()), "2026-11", 1},
		{time.Date(2026, 12, 1, 1, 0, 0, 0, schedule.Location()), "2026-12", 2},
		{time.Date(2026, 12, 20, 12, 0, 0, 0, schedule.Location()), "2026-12", 2},
		{time.Date(2027, 1, 5, 12, 0, 0, 0, schedule.Location()), "2027-01", 0},
	} {
		f.h.now = func() time.Time { return c.now }
		got := f.list(t)
		if got.Month != c.wantMonth || len(got.Members) != 1 || got.Members[0].Sessions != c.wantSessions {
			t.Errorf("at %s: month %q members %v, want %q with %d paid sessions", c.now.Format(time.RFC3339), got.Month, got.Members, c.wantMonth, c.wantSessions)
		}
	}
}

func TestAddingAMember(t *testing.T) {
	f := newFlow()
	rec := f.admin(http.MethodPost, "/api/v1/admin/members", body{"fullName": "  Amina   Otieno ", "phoneNumber": "0712 345 678"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", rec.Code, rec.Body.String())
	}
	var m memberJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.FullName != "Amina Otieno" || m.PhoneNumber != "254712345678" || m.ID == "" || m.Sessions != 0 || m.DiscountUsed {
		t.Errorf("added member = %+v, want a tidied name and the number as 254712345678", m)
	}
	if !m.CreatedAt.Equal(flowNow) {
		t.Errorf("created at %v, want the time of the request %v", m.CreatedAt, flowNow)
	}

	// The same number in any other spelling is the same member.
	for _, again := range []string{"254712345678", "+254712345678", "0712345678", "712345678", "0712-345-678"} {
		rec := f.admin(http.MethodPost, "/api/v1/admin/members", body{"fullName": "Someone Else", "phoneNumber": again})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already on the member list") {
			t.Errorf("adding %q again = %d %q, want 409", again, rec.Code, rec.Body.String())
		}
		if rec.Header().Get(middleware.UserMessageHeader) != "1" {
			t.Errorf("the 409 for %q is for the admin to read, so it must be marked", again)
		}
	}
	if got := f.list(t); len(got.Members) != 1 || got.Members[0].FullName != "Amina Otieno" {
		t.Errorf("members = %+v", got.Members)
	}

	// Whichever way they type their number it is the same member: once the
	// sessions already paid for this month reach three, the next one is discounted.
	f.h.phoneLimiter = middleware.NewLimiter(6_000_000, 1_000_000)
	for i, typed := range []string{"+254 712 345 678", "0712345678", "712345678"} {
		if resp := f.bookPaid(t, body{"phoneNumber": typed, "date": nov(3 + i)}); resp.Amount != 10_000 {
			t.Errorf("session %d cost %d", i+1, resp.Amount)
		}
	}
	if resp := f.book(t, body{"phoneNumber": "254712345678", "date": nov(6)}); resp.Amount != 2_000 {
		t.Errorf("the new member's fourth session cost %d, want Ksh 2,000", resp.Amount)
	}
}

// A number that has already paid for sessions this month starts the list where
// those sessions put it: they count whether or not it was a member when it booked.
func TestAddingAMemberShowsTheSessionsAlreadyPaidForThisMonth(t *testing.T) {
	f := newFlow()
	for i := 0; i < 3; i++ {
		f.seedPaid(memberPhone, nov(10+i), "21:00 – 23:00", "8Aside")
	}
	f.seedPaid(memberPhone, "2026-12-01", "21:00 – 23:00", "8Aside") // next month: not counted
	f.seedPaid(memberPhone, nov(13), "21:00 – 22:00", "8Aside")      // 1 hour: not counted

	rec := f.admin(http.MethodPost, "/api/v1/admin/members", body{"fullName": "Amina Otieno", "phoneNumber": memberPhone})
	if rec.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", rec.Code, rec.Body.String())
	}
	var m memberJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Sessions != 3 || m.DiscountUsed {
		t.Errorf("added member = %+v, want 3 paid sessions and no discount given yet", m)
	}
}

func TestAddingAMemberRefusesBadDetails(t *testing.T) {
	f := newFlow()
	for name, c := range map[string]struct {
		payload body
		want    int
	}{
		"no number":      {body{"fullName": "Amina Otieno"}, http.StatusBadRequest},
		"short number":   {body{"fullName": "Amina Otieno", "phoneNumber": "12345"}, http.StatusBadRequest},
		"foreign number": {body{"fullName": "Amina Otieno", "phoneNumber": "14155550123"}, http.StatusBadRequest},
		"letters":        {body{"fullName": "Amina Otieno", "phoneNumber": "07abcdefgh"}, http.StatusBadRequest},
		"no name":        {body{"phoneNumber": "254712345678"}, http.StatusBadRequest},
		"one letter":     {body{"fullName": "A", "phoneNumber": "254712345678"}, http.StatusBadRequest},
		"markup in name": {body{"fullName": "<b>Amina</b>", "phoneNumber": "254712345678"}, http.StatusBadRequest},
		"link in name":   {body{"fullName": "Amina http://evil.example", "phoneNumber": "254712345678"}, http.StatusBadRequest},
		"very long name": {body{"fullName": strings.Repeat("a", 61), "phoneNumber": "254712345678"}, http.StatusBadRequest},
		"oversized body": {body{"fullName": strings.Repeat("a", 10_000), "phoneNumber": "254712345678"}, http.StatusRequestEntityTooLarge},
	} {
		if rec := f.admin(http.MethodPost, "/api/v1/admin/members", c.payload); rec.Code != c.want {
			t.Errorf("%s = %d %q, want %d", name, rec.Code, rec.Body.String(), c.want)
		}
	}

	r := httptest.NewRequest(http.MethodPost, "/api/v1/admin/members", strings.NewReader(`{"fullName":"Amina Otieno","phoneNumber":"254712345678"}`))
	r.Header.Set("Authorization", "Bearer "+testAdminKey)
	r.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	f.adminMux().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d, want 415", rec.Code)
	}

	if got := f.list(t); len(got.Members) != 0 {
		t.Errorf("a refused request added %+v", got.Members)
	}
}

func TestRemovingAMember(t *testing.T) {
	f := newFlow(memberPhone, member2Phone)
	for _, c := range []struct {
		path string
		want int
	}{
		{"/api/v1/admin/members/0712345678", http.StatusNoContent},  // any spelling of the number
		{"/api/v1/admin/members/254712345678", http.StatusNotFound}, // already gone
		{"/api/v1/admin/members/254799999999", http.StatusNotFound}, // never a member
		{"/api/v1/admin/members/12345", http.StatusBadRequest},
		{"/api/v1/admin/members/not-a-number", http.StatusBadRequest},
	} {
		if rec := f.admin(http.MethodDelete, c.path, nil); rec.Code != c.want {
			t.Errorf("DELETE %s = %d %q, want %d", c.path, rec.Code, rec.Body.String(), c.want)
		}
	}
	got := f.list(t)
	if len(got.Members) != 1 || got.Members[0].PhoneNumber != member2Phone {
		t.Errorf("members = %+v, want only the second member left", got.Members)
	}
	for i := 0; i < 3; i++ {
		f.seedPaid(memberPhone, nov(20+i), "21:00 – 23:00", "8Aside")
	}
	if resp := f.book(t, body{"phoneNumber": memberPhone}); resp.Amount != 10_000 {
		t.Errorf("the removed member's fourth session cost %d, want the standard price", resp.Amount)
	}
}

func TestMemberAdminFailsCleanlyWhenTheDatabaseDoes(t *testing.T) {
	f := newFlow(memberPhone)
	f.members.failures = map[string]error{"List": errors.New("down"), "Add": errors.New("down"), "Remove": errors.New("down")}
	for _, rec := range []*httptest.ResponseRecorder{
		f.admin(http.MethodGet, "/api/v1/admin/members", nil),
		f.admin(http.MethodPost, "/api/v1/admin/members", body{"fullName": "Amina Otieno", "phoneNumber": "254700111222"}),
		f.admin(http.MethodDelete, "/api/v1/admin/members/"+memberPhone, nil),
	} {
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("status %d %q, want 500", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "down") {
			t.Errorf("the database's error text leaked to the client: %q", rec.Body.String())
		}
	}

	f.members.failures = nil
	f.bookings.failures = map[string]error{"MemberMonth": errors.New("down")}
	if rec := f.admin(http.MethodGet, "/api/v1/admin/members", nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("a failing count = %d, want 500", rec.Code)
	}
}

func TestMemberAdminIsSwitchedOffWithoutAMemberList(t *testing.T) {
	f := newFlow()
	f.h.members = nil
	for _, rec := range []*httptest.ResponseRecorder{
		f.admin(http.MethodGet, "/api/v1/admin/members", nil),
		f.admin(http.MethodPost, "/api/v1/admin/members", body{"fullName": "Amina Otieno", "phoneNumber": "254700111222"}),
		f.admin(http.MethodDelete, "/api/v1/admin/members/"+memberPhone, nil),
	} {
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503", rec.Code)
		}
	}
}

// ---- the Mongo queries -----------------------------------------------------------------

func bookingFieldPaths() map[string]bool {
	paths := map[string]bool{}
	bsonPaths(reflect.TypeOf(models.Booking{}), "", paths)
	return paths
}

func TestBookingQueriesUseRealFieldNames(t *testing.T) {
	paths := bookingFieldPaths()
	from, to := "2026-11-01", "2026-12-01"
	projection := map[string]any{}
	for k, v := range memberMonthFields {
		projection[k] = v
	}
	queries := map[string]any{
		"occupancyFilter":                occupancyFilter("2026-11-03"),
		"memberMonthFilter":              memberMonthFilter(memberPhone, from, to),
		"memberMonthFilter(everyone)":    memberMonthFilter("", from, to),
		"memberMonthFields (projection)": bson.M(projection),
		"failBookingFilter":              failBookingFilter("abc"),
		"failBookingUpdate":              failBookingUpdate(),
	}
	for name, q := range queries {
		var refs []string
		fieldRefs(q, &refs)
		if len(refs) == 0 {
			t.Errorf("%s mentions no fields; the walker is broken", name)
		}
		for _, ref := range refs {
			if !paths[ref] {
				t.Errorf("%s uses field %q, which is not a bson field of Booking", name, ref)
			}
		}
	}
}

func TestMemberQueriesUseRealFieldNames(t *testing.T) {
	paths := map[string]bool{}
	bsonPaths(reflect.TypeOf(models.Member{}), "", paths)
	m := models.Member{PhoneNumber: memberPhone, FullName: "Amina Otieno", CreatedAt: flowNow}
	for name, q := range map[string]any{
		"memberFilter":       memberFilter(memberPhone),
		"memberInsertUpdate": memberInsertUpdate(m)["$setOnInsert"],
	} {
		var refs []string
		fieldRefs(q, &refs)
		if len(refs) == 0 {
			t.Errorf("%s mentions no fields", name)
		}
		for _, ref := range refs {
			if !paths[ref] {
				t.Errorf("%s uses field %q, which is not a bson field of Member", name, ref)
			}
		}
	}
	// The upsert writes the whole member, and only when there is none.
	set, _ := memberInsertUpdate(m)["$setOnInsert"].(bson.M)
	if len(set) != 3 {
		t.Errorf("$setOnInsert = %v, want the number, the name and the creation time", set)
	}
	if _, ok := memberInsertUpdate(m)["$set"]; ok {
		t.Error("an existing member must never be overwritten")
	}
}

// Which bookings the month's filter picks up, checked against the filter itself:
// 8-a-side only, a date in the month, a status that holds a place, and the number.
func TestMemberMonthFilterMatchesExactlyTheBookingsThatCanCount(t *testing.T) {
	booking := func(mut func(*models.Booking)) models.Booking {
		b := models.Booking{
			Team:           models.Team{PhoneNumber: memberPhone},
			BookingDetails: models.BookingDetails{Date: "2026-11-15", PitchType: "8Aside", Time: "21:00 – 23:00", BookingStatus: "Booked"},
		}
		if mut != nil {
			mut(&b)
		}
		return b
	}
	filter := memberMonthFilter(memberPhone, "2026-11-01", "2026-12-01")
	for name, c := range map[string]struct {
		b    models.Booking
		want bool
	}{
		"a paid 8-a-side session":     {booking(nil), true},
		"a pending 8-a-side session":  {booking(func(b *models.Booking) { b.BookingDetails.BookingStatus = "Pending" }), true},
		"an older confirmed row":      {booking(func(b *models.Booking) { b.BookingDetails.BookingStatus = "Confirmed" }), true},
		"a failed payment":            {booking(func(b *models.Booking) { b.BookingDetails.BookingStatus = "Failed" }), false},
		"a 5-a-side session":          {booking(func(b *models.Booking) { b.BookingDetails.PitchType = "5Aside" }), false},
		"another number":              {booking(func(b *models.Booking) { b.Team.PhoneNumber = member2Phone }), false},
		"the first day of the month":  {booking(func(b *models.Booking) { b.BookingDetails.Date = "2026-11-01" }), true},
		"the last day of the month":   {booking(func(b *models.Booking) { b.BookingDetails.Date = "2026-11-30" }), true},
		"the day before":              {booking(func(b *models.Booking) { b.BookingDetails.Date = "2026-10-31" }), false},
		"the first day of next month": {booking(func(b *models.Booking) { b.BookingDetails.Date = "2026-12-01" }), false},
		"the same month a year later": {booking(func(b *models.Booking) { b.BookingDetails.Date = "2027-11-15" }), false},
	} {
		if got := matches(docOf(c.b), filter); got != c.want {
			t.Errorf("%s: matches = %v, want %v", name, got, c.want)
		}
	}
	// Without a number it matches every member's sessions.
	if !matches(docOf(booking(func(b *models.Booking) { b.Team.PhoneNumber = member2Phone })), memberMonthFilter("", "2026-11-01", "2026-12-01")) {
		t.Error("the filter for everyone must match any number")
	}
}

// What the bookings the filter picked up add up to: only 2 hour sessions count,
// only paid ones are progress, and a discounted one marks the discount as given
// whether it is paid or still waiting.
func TestSummariseMemberMonths(t *testing.T) {
	row := func(phone, timeRange, pitch, status, discount string) models.Booking {
		return models.Booking{
			Team:           models.Team{PhoneNumber: phone},
			BookingDetails: models.BookingDetails{Time: timeRange, PitchType: pitch, BookingStatus: status},
			Transaction:    models.Transaction{Discount: discount},
		}
	}
	got := summariseMemberMonths([]models.Booking{
		row(memberPhone, "21:00 – 23:00", "8Aside", "Booked", ""),
		row(memberPhone, "19:00 – 21:00", "8Aside", "Confirmed", ""),
		row(memberPhone, "17:00 – 19:00", "8Aside", "Pending", ""),        // waiting: not progress
		row(memberPhone, "15:00 – 16:00", "8Aside", "Booked", ""),         // 1 hour: not eligible
		row(memberPhone, "21:00 – 23:00", "5Aside", "Booked", ""),         // 5-a-side: not eligible
		row(memberPhone, "garbage", "8Aside", "Booked", ""),               // unreadable: not counted
		row(memberPhone, "23:00 – 01:00", "8Aside", "Booked", ""),         // runs past midnight: still 2 hours
		row(member2Phone, "21:00 – 23:00", "8Aside", "Pending", "member"), // the discounted one, unpaid
		row(plainPhone, "21:00 – 23:00", "8Aside", "Booked", ""),
	})
	if m := got[memberPhone]; m.Paid != 3 || m.DiscountHeld {
		t.Errorf("member = %+v, want 3 paid (a paid, a confirmed and one past midnight) and no discount", m)
	}
	if m := got[member2Phone]; m.Paid != 0 || !m.DiscountHeld {
		t.Errorf("member 2 = %+v, want the discount held while its booking waits for payment", m)
	}
	if m := got[plainPhone]; m.Paid != 1 || m.DiscountHeld {
		t.Errorf("plain = %+v", m)
	}
	if _, ok := got["254799999999"]; ok || len(got) != 3 {
		t.Errorf("numbers with nothing to count must not appear: %v", got)
	}
	if got := summariseMemberMonths(nil); len(got) != 0 {
		t.Errorf("no bookings = %v", got)
	}
}
