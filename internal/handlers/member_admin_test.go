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
	Used        int       `json:"used"`
}

type memberListJSON struct {
	Month   string       `json:"month"`
	Percent int          `json:"percent"`
	Limit   int          `json:"limit"`
	Members []memberJSON `json:"members"`
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

func TestTheMemberListShowsEachMembersSessionsThisMonth(t *testing.T) {
	f := newFlow()
	for _, m := range []struct{ name, phone string }{
		// Not in the order of their numbers, so a list sorted by number would be caught.
		{"zainab Hassan", "254700111222"}, {"amina Otieno", "254733000222"}, {"Brian Kamau", memberPhone},
	} {
		if rec := f.admin(http.MethodPost, "/api/v1/admin/members", body{"fullName": m.name, "phoneNumber": m.phone}); rec.Code != http.StatusCreated {
			t.Fatalf("add %s = %d %s", m.name, rec.Code, rec.Body.String())
		}
	}
	f.book(t, body{"phoneNumber": memberPhone, "date": nov(3)})
	f.book(t, body{"phoneNumber": memberPhone, "date": nov(4)})
	f.book(t, body{"phoneNumber": "254700111222", "date": nov(5)})
	f.book(t, body{"phoneNumber": memberPhone, "date": "2026-12-01"}) // next month: not this month's
	f.book(t, body{"phoneNumber": plainPhone, "date": nov(6)})        // not a member

	got := f.list(t)
	if got.Month != "2026-11" || got.Percent != 20 || got.Limit != 4 {
		t.Errorf("rule = %q %d%% %d", got.Month, got.Percent, got.Limit)
	}
	var names []string
	used := map[string]int{}
	for _, m := range got.Members {
		names = append(names, m.FullName)
		used[m.PhoneNumber] = m.Used
		if m.ID == "" || m.CreatedAt.IsZero() {
			t.Errorf("member %+v is missing its id or creation time", m)
		}
	}
	if want := []string{"amina Otieno", "Brian Kamau", "zainab Hassan"}; !reflect.DeepEqual(names, want) {
		t.Errorf("order = %v, want by name without regard to case: %v", names, want)
	}
	if used[memberPhone] != 2 || used["254700111222"] != 1 || used["254733000222"] != 0 {
		t.Errorf("sessions used this month = %v, want 2, 1 and 0", used)
	}
}

// "This month" is the venue's month by its operating day: at half past midnight
// on 1 December the venue is still running 30 November's evening.
func TestTheMemberListMonthFollowsTheOperatingDay(t *testing.T) {
	f := newFlow(memberPhone)
	f.book(t, body{"date": nov(30)})
	f.book(t, body{"date": "2026-12-01"})

	for _, c := range []struct {
		now       time.Time
		wantMonth string
		wantUsed  int
	}{
		{time.Date(2026, 11, 20, 12, 0, 0, 0, schedule.Location()), "2026-11", 1},
		{time.Date(2026, 12, 1, 0, 30, 0, 0, schedule.Location()), "2026-11", 1},
		{time.Date(2026, 12, 1, 1, 0, 0, 0, schedule.Location()), "2026-12", 1},
		{time.Date(2026, 12, 20, 12, 0, 0, 0, schedule.Location()), "2026-12", 1},
		{time.Date(2027, 1, 5, 12, 0, 0, 0, schedule.Location()), "2027-01", 0},
	} {
		f.h.now = func() time.Time { return c.now }
		got := f.list(t)
		if got.Month != c.wantMonth || len(got.Members) != 1 || got.Members[0].Used != c.wantUsed {
			t.Errorf("at %s: month %q used %v, want %q and %d", c.now.Format(time.RFC3339), got.Month, got.Members, c.wantMonth, c.wantUsed)
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
	if m.FullName != "Amina Otieno" || m.PhoneNumber != "254712345678" || m.ID == "" || m.Used != 0 {
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

	// And they get the member price straight away, whichever way they type their number.
	f.h.phoneLimiter = middleware.NewLimiter(6_000_000, 1_000_000)
	if resp := f.book(t, body{"phoneNumber": "+254 712 345 678"}); resp.Amount != 8_000 {
		t.Errorf("the new member's first booking cost %d", resp.Amount)
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
	if resp := f.book(t, body{"phoneNumber": memberPhone}); resp.Amount != 10_000 {
		t.Errorf("the removed member's next booking cost %d, want the full price", resp.Amount)
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
	f.bookings.failures = map[string]error{"MemberSessionsByPhone": errors.New("down")}
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
	var pipelineStages []bson.M
	for _, stage := range memberSessionsByPhonePipeline(from, to) {
		raw, err := bson.Marshal(stage)
		if err != nil {
			t.Fatal(err)
		}
		var m bson.M
		if err := bson.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		pipelineStages = append(pipelineStages, m)
	}
	match, ok := pipelineStages[0]["$match"].(bson.M)
	if !ok {
		t.Fatalf("the first stage must be a $match: %v", pipelineStages[0])
	}
	group, ok := pipelineStages[1]["$group"].(bson.M)
	if !ok || group["_id"] != "$team.phoneNumber" {
		t.Fatalf("the second stage must group by the phone number: %v", pipelineStages[1])
	}
	if !paths["team.phoneNumber"] {
		t.Error("team.phoneNumber is not a bson field of Booking")
	}

	queries := map[string]any{
		"occupancyFilter":                occupancyFilter("2026-11-03"),
		"memberSessionsFilter":           memberSessionsFilter(memberPhone, from, to),
		"memberSessionsFilter(everyone)": memberSessionsFilter("", from, to),
		"pipeline $match":                match,
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

// What counts towards the month, checked against the filter itself: member
// price only, this number only, a date in the month, a status that holds a place.
func TestMemberSessionsFilterMatchesExactlyTheSessionsThatCount(t *testing.T) {
	booking := func(mut func(*models.Booking)) models.Booking {
		b := models.Booking{
			Team:           models.Team{PhoneNumber: memberPhone},
			BookingDetails: models.BookingDetails{Date: "2026-11-15", BookingStatus: "Pending"},
			Transaction:    models.Transaction{Discount: models.DiscountMember},
		}
		if mut != nil {
			mut(&b)
		}
		return b
	}
	filter := memberSessionsFilter(memberPhone, "2026-11-01", "2026-12-01")
	for name, c := range map[string]struct {
		b    models.Booking
		want bool
	}{
		"a pending member session":    {booking(nil), true},
		"a paid member session":       {booking(func(b *models.Booking) { b.BookingDetails.BookingStatus = "Booked" }), true},
		"an older confirmed row":      {booking(func(b *models.Booking) { b.BookingDetails.BookingStatus = "Confirmed" }), true},
		"a failed payment":            {booking(func(b *models.Booking) { b.BookingDetails.BookingStatus = "Failed" }), false},
		"a full-price booking":        {booking(func(b *models.Booking) { b.Transaction.Discount = "" }), false},
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
	if !matches(docOf(booking(func(b *models.Booking) { b.Team.PhoneNumber = member2Phone })), memberSessionsFilter("", "2026-11-01", "2026-12-01")) {
		t.Error("the filter for everyone must match any number")
	}
}
