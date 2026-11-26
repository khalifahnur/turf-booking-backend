package handlers

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"turf-booking-backend/internal/config"
	"turf-booking-backend/internal/middleware"
	"turf-booking-backend/internal/models"
	"turf-booking-backend/internal/registration"
	"turf-booking-backend/internal/services"
	"turf-booking-backend/internal/ws"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type (
	bsonM       = bson.M
	primitive_A = bson.A
)

// servicesUncertain is the error a payment gateway returns when a prompt may
// have been sent although it did not hear back.
func servicesUncertain() error { return services.ErrPaymentUncertain }

// regFlow is tournament and academy registration on an in-memory store, with a
// clock the test moves and a payment gateway that only records what it is asked.
type regFlow struct {
	h        *RegistrationHandler
	store    *fakeRegistrations
	gateway  *fakePayments
	clock    time.Time
	mu       sync.Mutex
	texts    chan models.Registration
	notified []string
}

func newRegFlow() *regFlow {
	f := &regFlow{
		store:   &fakeRegistrations{},
		gateway: &fakePayments{},
		clock:   flowNow,
		texts:   make(chan models.Registration, 32),
	}
	h := NewRegistrationHandler(f.store, &config.Config{AdminAPIKey: testAdminKey}, ws.NewPaymentHub())
	h.pay = f.gateway.pay
	h.now = func() time.Time { f.mu.Lock(); defer f.mu.Unlock(); return f.clock }
	h.phoneLimiter = middleware.NewLimiter(6_000_000, 1_000_000) // out of the way unless a test wants it
	h.sendSMS = func(_ *config.Config, reg models.Registration) error { f.texts <- reg; return nil }
	h.notify = func(ref, status string) { f.mu.Lock(); f.notified = append(f.notified, ref+" "+status); f.mu.Unlock() }
	f.h = h
	return f
}

func (f *regFlow) advance(d time.Duration) { f.mu.Lock(); f.clock = f.clock.Add(d); f.mu.Unlock() }

func (f *regFlow) notifications() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notified...)
}

// phoneN gives each team its own number.
func phoneN(n int) string { return fmt.Sprintf("2547000%05d", n) }

func teamBody(over body) body {
	b := body{
		"academyName":   "Kibera Stars",
		"players":       14,
		"categories":    []string{"u10"},
		"phoneNumber":   phoneN(1),
		"acceptedRules": true,
		"rulesVersion":  registration.RulesVersion,
	}
	for k, v := range over {
		b[k] = v
	}
	return b
}

func playerBody(over body) body {
	b := body{
		"playerName":   "Amani Otieno",
		"dateOfBirth":  "2016-03-14",
		"guardianName": "Grace Otieno",
		"phoneNumber":  phoneN(1),
	}
	for k, v := range over {
		b[k] = v
	}
	return b
}

type registered struct {
	Message    string   `json:"message"`
	Reference  string   `json:"reference"`
	Total      int      `json:"total"`
	Categories []string `json:"categories"`
}

func (f *regFlow) post(h http.HandlerFunc, payload any) (*httptest.ResponseRecorder, registered) {
	rec := httptest.NewRecorder()
	h(rec, jsonRequest(http.MethodPost, "/register", payload))
	var out registered
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func (f *regFlow) team(over body) (*httptest.ResponseRecorder, registered) {
	return f.post(f.h.RegisterTournament, teamBody(over))
}

func (f *regFlow) player(over body) (*httptest.ResponseRecorder, registered) {
	return f.post(f.h.RegisterAcademy, playerBody(over))
}

// mustTeam registers a team that must be accepted.
func (f *regFlow) mustTeam(t *testing.T, over body) registered {
	t.Helper()
	rec, out := f.team(over)
	if rec.Code != http.StatusCreated {
		t.Fatalf("registering %v = %d %q", over, rec.Code, rec.Body.String())
	}
	return out
}

// pay is the customer entering their PIN: Paystack's webhook for the reference.
func (f *regFlow) pay(reference string, cents int64) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.HandlePaystackEvent(rec, httptest.NewRequest(http.MethodPost, "/webhook", nil), paystackEvent{
		Event: "charge.success", Reference: reference, Status: "success", Currency: "KES", AmountCents: cents,
	})
	return rec
}

func (f *regFlow) event(reference, event, status string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	f.h.HandlePaystackEvent(rec, httptest.NewRequest(http.MethodPost, "/webhook", nil), paystackEvent{
		Event: event, Reference: reference, Status: status, Currency: "KES",
	})
	return rec
}

func (f *regFlow) status(t *testing.T, reference string) string {
	t.Helper()
	got, err := f.store.Status(nil, reference)
	if err != nil {
		t.Fatalf("status of %s: %v", reference, err)
	}
	return got
}

type infoJSON struct {
	FeePerCategory int        `json:"feePerCategory"`
	MinPlayers     int        `json:"minPlayers"`
	MaxPlayers     int        `json:"maxPlayers"`
	RulesVersion   string     `json:"rulesVersion"`
	HoldMinutes    int        `json:"holdMinutes"`
	Categories     []slotView `json:"categories"`
}

func (f *regFlow) info(t *testing.T) infoJSON {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.TournamentInfo(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tournament", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("info = %d %s", rec.Code, rec.Body.String())
	}
	var out infoJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *regFlow) slot(t *testing.T, id string) slotView {
	t.Helper()
	for _, s := range f.info(t).Categories {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no category %q in the info", id)
	return slotView{}
}

// ---- the tournament ---------------------------------------------------------------------

func TestATournamentRegistrationChargesTheFeeForEachCategoryAndHoldsAPlaceInEach(t *testing.T) {
	f := newRegFlow()
	rec, out := f.team(body{"academyName": "  kibera   Stars ", "categories": []string{"u15", "u10"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d %q", rec.Code, rec.Body.String())
	}
	if out.Total != 4_000 || !strings.HasPrefix(out.Reference, "tourney-") || len(out.Reference) != len("tourney-")+32 || out.Message != "Awaiting Payment" {
		t.Errorf("response = %+v, want Ksh 4,000 and a tourney- reference", out)
	}
	if !reflect.DeepEqual(out.Categories, []string{"u10", "u15"}) {
		t.Errorf("categories = %v, want them in order", out.Categories)
	}

	calls := f.gateway.all()
	if len(calls) != 1 || calls[0].cents != 400_000 || calls[0].phone != phoneN(1) || calls[0].ref != out.Reference ||
		calls[0].email != registration.PayerEmail(phoneN(1)) {
		t.Fatalf("payment requests = %+v, want one of Ksh 4,000 (2,000 for each of two categories) to the team's number", calls)
	}

	rows := f.store.all()
	if len(rows) != 1 {
		t.Fatalf("stored %d registrations", len(rows))
	}
	r := rows[0]
	if r.Kind != "tournament" || r.TeamName != "kibera Stars" || r.TeamKey != "kibera stars" || r.Players != 14 || r.PhoneNumber != phoneN(1) ||
		!reflect.DeepEqual(r.Categories, []string{"u10", "u15"}) || r.AmountKES != 4_000 || r.Status != "Pending" ||
		r.RulesVersion != registration.RulesVersion || !r.RulesAcceptedAt.Equal(flowNow) || r.Payment.Reference != out.Reference {
		t.Errorf("stored = %+v", r)
	}
	// It holds one place in each category it asked for.
	for _, id := range []string{"u10", "u15"} {
		if s := f.slot(t, id); s.Held != 1 || s.Paid != 0 || s.Left != 2 {
			t.Errorf("%s = %+v, want one place held", id, s)
		}
	}
}

func TestTheFeeIsNotSomethingTheBrowserCanChoose(t *testing.T) {
	f := newRegFlow()
	rec, out := f.team(body{"amount": 1, "fee": 1, "total": 1})
	if rec.Code != http.StatusCreated || out.Total != 2_000 {
		t.Fatalf("register = %d total %d", rec.Code, out.Total)
	}
	if calls := f.gateway.all(); len(calls) != 1 || calls[0].cents != 200_000 {
		t.Errorf("charged %+v, want Ksh 2,000 whatever the request said", calls)
	}
}

// Youth: 4 teams in each of U10 and U15, one place in each already taken by
// St. Sebastian Sports Academy, so 3 are open. Adults: 6.
func TestFirstComeFirstSlotTheYouthCategoriesTakeThreeMoreTeamsAndTheAdultsSix(t *testing.T) {
	for _, c := range []struct {
		id, label string
		open      int
	}{{"u10", "U10", 3}, {"u15", "U15", 3}, {"adults", "Adults", 6}} {
		f := newRegFlow()
		for i := 1; i <= c.open; i++ {
			f.mustTeam(t, body{"academyName": fmt.Sprintf("Team %d", i), "phoneNumber": phoneN(i), "categories": []string{c.id}})
		}
		if s := f.slot(t, c.id); s.Left != 0 || s.Held != c.open {
			t.Fatalf("%s = %+v, want every open place held", c.id, s)
		}

		// The next team is turned away, told why, and is not sent a prompt.
		before := len(f.gateway.all())
		rec, _ := f.team(body{"academyName": "One More", "phoneNumber": phoneN(20), "categories": []string{c.id}})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), c.label+" is full: all its places are taken") {
			t.Errorf("%s: team %d = %d %q, want 409 %s is full", c.id, c.open+1, rec.Code, rec.Body.String(), c.label)
		}
		if rec.Header().Get(middleware.UserMessageHeader) != "1" {
			t.Error("the refusal is for the team to read, so it must be marked")
		}
		if !strings.Contains(rec.Body.String(), "still paying") {
			t.Errorf("places are held by teams that have not paid yet, and the message should say they may be released: %q", rec.Body.String())
		}
		if len(f.gateway.all()) != before || len(f.store.all()) != c.open {
			t.Error("a team turned away must not be charged or recorded")
		}

		// The other categories are separate.
		other := map[string]string{"u10": "adults", "u15": "adults", "adults": "u10"}[c.id]
		f.mustTeam(t, body{"academyName": "Elsewhere", "phoneNumber": phoneN(21), "categories": []string{other}})
	}
}

func TestTheAcademysOwnPlacesAreShownAsTakenButAreNotAnyTeamsRegistration(t *testing.T) {
	f := newRegFlow()
	info := f.info(t)
	want := map[string]slotView{
		"u10":    {ID: "u10", Label: "U10", Group: "youth", Capacity: 4, Reserved: 1, Left: 3},
		"u15":    {ID: "u15", Label: "U15", Group: "youth", Capacity: 4, Reserved: 1, Left: 3},
		"adults": {ID: "adults", Label: "Adults", Group: "adults", Capacity: 6, Reserved: 0, Left: 6},
	}
	if len(info.Categories) != 3 {
		t.Fatalf("categories = %+v", info.Categories)
	}
	for _, got := range info.Categories {
		if !reflect.DeepEqual(got, want[got.ID]) {
			t.Errorf("%s = %+v, want %+v", got.ID, got, want[got.ID])
		}
	}
	if len(f.store.all()) != 0 {
		t.Error("the academy's places must not need a registration row")
	}
}

func TestATeamInAllCategoriesTakesAPlaceInEach(t *testing.T) {
	f := newRegFlow()
	all := []string{"u10", "u15", "adults"}
	for i := 1; i <= 3; i++ {
		f.mustTeam(t, body{"academyName": fmt.Sprintf("Everything %d", i), "phoneNumber": phoneN(i), "categories": all})
	}
	for id, left := range map[string]int{"u10": 0, "u15": 0, "adults": 3} {
		if s := f.slot(t, id); s.Left != left {
			t.Errorf("%s = %+v, want %d left", id, s, left)
		}
	}
	rec, _ := f.team(body{"academyName": "Late", "phoneNumber": phoneN(9), "categories": []string{"u10", "u15"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "U10 and U15 are full") {
		t.Errorf("a team wanting both youth categories = %d %q", rec.Code, rec.Body.String())
	}
	// The adults still have room, and only they do.
	rec, _ = f.team(body{"academyName": "Late", "phoneNumber": phoneN(9), "categories": all})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "You can still register for Adults on its own") {
		t.Errorf("a team wanting all three = %d %q", rec.Code, rec.Body.String())
	}
	f.mustTeam(t, body{"academyName": "Late", "phoneNumber": phoneN(9), "categories": []string{"adults"}})
}

func TestOneFullCategoryRefusesTheWholeRegistrationAndSaysWhatIsStillOpen(t *testing.T) {
	f := newRegFlow()
	for i := 1; i <= 3; i++ {
		f.mustTeam(t, body{"academyName": fmt.Sprintf("Team %d", i), "phoneNumber": phoneN(i), "categories": []string{"u15"}})
	}
	rec, _ := f.team(body{"academyName": "Wants Both", "phoneNumber": phoneN(8), "categories": []string{"u10", "u15"}})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "U15 is full") || !strings.Contains(rec.Body.String(), "You can still register for U10 on its own") {
		t.Errorf("= %d %q", rec.Code, rec.Body.String())
	}
	// Refusing it took nothing from the category that was open.
	if s := f.slot(t, "u10"); s.Held != 0 || s.Left != 3 {
		t.Errorf("u10 = %+v, want untouched", s)
	}
	if len(f.store.all()) != 3 {
		t.Errorf("%d registrations stored", len(f.store.all()))
	}
}

func TestAnUnpaidPlaceGoesBackAfterTheHoldWindowButAPaidOneNeverDoes(t *testing.T) {
	f := newRegFlow()
	paid := f.mustTeam(t, body{"academyName": "Paid Team", "phoneNumber": phoneN(1)})
	f.pay(paid.Reference, 200_000)
	for i := 2; i <= 3; i++ {
		f.mustTeam(t, body{"academyName": fmt.Sprintf("Slow %d", i), "phoneNumber": phoneN(i)})
	}
	if rec, _ := f.team(body{"academyName": "Next", "phoneNumber": phoneN(7)}); rec.Code != http.StatusConflict {
		t.Fatalf("setup: the category should be full, got %d", rec.Code)
	}

	f.advance(registration.HoldWindow - time.Second)
	if rec, _ := f.team(body{"academyName": "Next", "phoneNumber": phoneN(7)}); rec.Code != http.StatusConflict {
		t.Errorf("just before the hold lapses = %d, want it still held", rec.Code)
	}

	f.advance(2 * time.Second)
	s := f.slot(t, "u10")
	if s.Paid != 1 || s.Held != 0 || s.Left != 2 {
		t.Fatalf("after the window u10 = %+v, want the two unpaid places released and the paid one kept", s)
	}
	f.advance(30 * 24 * time.Hour)
	if s := f.slot(t, "u10"); s.Paid != 1 {
		t.Errorf("a paid place must never lapse, got %+v", s)
	}
	// The two released places are really available.
	for i := 7; i <= 8; i++ {
		f.mustTeam(t, body{"academyName": fmt.Sprintf("Next %d", i), "phoneNumber": phoneN(i)})
	}
	if rec, _ := f.team(body{"academyName": "One Too Many", "phoneNumber": phoneN(12)}); rec.Code != http.StatusConflict {
		t.Errorf("the place after the release = %d, want full", rec.Code)
	}
}

func TestAFailedPromptFreesThePlaceAtOnce(t *testing.T) {
	f := newRegFlow()
	f.gateway.err = errors.New("paystack error (Status 400): invalid phone")
	rec, _ := f.team(nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "You have not been charged") {
		t.Fatalf("refused prompt = %d %q", rec.Code, rec.Body.String())
	}
	if rows := f.store.all(); len(rows) != 1 || rows[0].Status != "Failed" {
		t.Fatalf("stored = %+v, want it marked Failed", rows)
	}
	if s := f.slot(t, "u10"); s.Left != 3 {
		t.Errorf("u10 = %+v, want the place back", s)
	}
	f.gateway.err = nil
	f.mustTeam(t, nil) // the same team can try again straight away
}

func TestAnUncertainPromptKeepsThePlaceUntilItIsKnown(t *testing.T) {
	f := newRegFlow()
	f.gateway.err = fmt.Errorf("%w: timeout", servicesUncertain())
	rec, _ := f.team(nil)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "check your phone") || !strings.Contains(rec.Body.String(), "wait about 3 minutes") {
		t.Fatalf("uncertain prompt = %d %q, want it to say how long to wait (the hold)", rec.Code, rec.Body.String())
	}
	rows := f.store.all()
	if len(rows) != 1 || rows[0].Status != "Pending" {
		t.Fatalf("stored = %+v, want it left Pending: a prompt may have been sent", rows)
	}
	// An immediate retry from the same team is refused rather than sending a second prompt.
	f.gateway.err = nil
	if rec, _ := f.team(nil); rec.Code != http.StatusConflict {
		t.Errorf("immediate retry = %d, want 409", rec.Code)
	}
	// If the payment does arrive, it settles the registration.
	if rec := f.pay(rows[0].Payment.Reference, 200_000); rec.Code != http.StatusOK || f.status(t, rows[0].Payment.Reference) != "Paid" {
		t.Errorf("late settle = %d, status %s", rec.Code, f.status(t, rows[0].Payment.Reference))
	}
}

func TestATeamCannotTakeTheSamePlaceTwice(t *testing.T) {
	f := newRegFlow()
	first := f.mustTeam(t, nil)

	for _, name := range []string{"Kibera Stars", "kibera stars", "  KIBERA   STARS "} {
		rec, _ := f.team(body{"academyName": name, "phoneNumber": phoneN(2)})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already registered") || !strings.Contains(rec.Body.String(), "U10") {
			t.Errorf("%q again = %d %q, want 409", name, rec.Code, rec.Body.String())
		}
	}
	// A place in the other category is a different place.
	f.mustTeam(t, body{"categories": []string{"u15"}, "phoneNumber": phoneN(2)})
	// But not one that includes a category they already hold.
	if rec, _ := f.team(body{"categories": []string{"u10", "u15"}, "phoneNumber": phoneN(3)}); rec.Code != http.StatusConflict {
		t.Errorf("both categories when already in both = %d, want 409", rec.Code)
	}
	if got := len(f.store.all()); got != 2 {
		t.Errorf("%d registrations stored, want 2", got)
	}

	// Once the first one has failed, the team may register again.
	f.event(first.Reference, "charge.failed", "failed")
	f.mustTeam(t, body{"phoneNumber": phoneN(4)})
}

func TestATournamentRegistrationRefusesBadDetails(t *testing.T) {
	f := newRegFlow()
	for name, c := range map[string]struct {
		over body
		want int
		text string
	}{
		"no name":             {body{"academyName": ""}, 400, "Academy name must be"},
		"one letter":          {body{"academyName": "A"}, 400, "Academy name must be"},
		"markup in the name":  {body{"academyName": "<b>Stars</b>"}, 400, "Academy name"},
		"a link in the name":  {body{"academyName": "Stars http://evil.example"}, 400, "Academy name"},
		"too few players":     {body{"players": 4}, 400, "between 5 and 40"},
		"too many players":    {body{"players": 41}, 400, "between 5 and 40"},
		"no players":          {body{"players": 0}, 400, "between 5 and 40"},
		"negative players":    {body{"players": -3}, 400, "between 5 and 40"},
		"players as text":     {body{"players": "twelve"}, 400, "Invalid JSON"},
		"no category":         {body{"categories": []string{}}, 400, "at least one category"},
		"unknown category":    {body{"categories": []string{"u12"}}, 400, "does not exist"},
		"no phone":            {body{"phoneNumber": ""}, 400, "valid Kenyan mobile"},
		"foreign phone":       {body{"phoneNumber": "14155550123"}, 400, "valid Kenyan mobile"},
		"rules not accepted":  {body{"acceptedRules": false}, 400, "accept the tournament rules"},
		"rules not mentioned": {body{"acceptedRules": nil}, 400, "accept the tournament rules"},
		"old rules version":   {body{"rulesVersion": "tournament-2025-v0"}, 409, "rules have been updated"},
		"no rules version":    {body{"rulesVersion": ""}, 409, "rules have been updated"},
		"a huge body":         {body{"academyName": strings.Repeat("a", 10_000)}, 413, "too large"},
	} {
		rec, _ := f.team(c.over)
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.text) {
			t.Errorf("%s = %d %q, want %d containing %q", name, rec.Code, rec.Body.String(), c.want, c.text)
		}
		if rec.Header().Get(middleware.UserMessageHeader) != "1" {
			t.Errorf("%s: the refusal is for the team to read, so it must be marked", name)
		}
	}
	if len(f.store.all()) != 0 || len(f.gateway.all()) != 0 {
		t.Error("a refused registration must record nothing and send no prompt")
	}

	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	f.h.RegisterTournament(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain = %d, want 415", rec.Code)
	}
	for name, raw := range map[string]string{"not json": "{nope", "two objects": `{}{}`} {
		r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		f.h.RegisterTournament(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, rec.Code)
		}
	}
}

func TestOnePhoneCannotBeUsedToFloodAHandsetWithPrompts(t *testing.T) {
	f := newRegFlow()
	f.h.phoneLimiter = middleware.NewLimiter(0.3, 3)
	for i := 1; i <= 3; i++ {
		f.mustTeam(t, body{"academyName": fmt.Sprintf("Team %d", i), "categories": []string{"u10"}})
		f.store.rows[len(f.store.rows)-1].Status = "Failed" // let the next try pass the duplicate check
	}
	rec, _ := f.team(body{"academyName": "Team 4"})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Errorf("fourth attempt = %d, want 429 with Retry-After", rec.Code)
	}
}

// Twelve teams ask for the three open places of a youth category in the same
// instant. Exactly three may be given a place, however the requests interleave.
func TestTheLastPlacesAreNeverGivenOutTwice(t *testing.T) {
	f := newRegFlow()
	f.h.pay = func(*config.Config, string, string, int, string) (string, error) {
		time.Sleep(2 * time.Millisecond) // a slow gateway must not hold up the next team's place
		return "ok", nil
	}
	const teams = 12
	codes := make([]int, teams)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < teams; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec, _ := f.team(body{"academyName": fmt.Sprintf("Racer %d", i), "phoneNumber": phoneN(i + 1)})
			codes[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	accepted, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			accepted++
		case http.StatusConflict:
			refused++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if accepted != 3 || refused != 9 {
		t.Errorf("%d accepted, %d refused; want exactly 3 and 9", accepted, refused)
	}
	if s := f.slot(t, "u10"); s.Held != 3 || s.Left != 0 {
		t.Errorf("u10 = %+v", s)
	}
}

// ---- paying -------------------------------------------------------------------------------------

func TestPayingSettlesTheRegistrationAndTextsTheTeam(t *testing.T) {
	f := newRegFlow()
	out := f.mustTeam(t, body{"categories": []string{"u10", "u15"}})

	if rec := f.pay(out.Reference, 400_000); rec.Code != http.StatusOK {
		t.Fatalf("webhook = %d", rec.Code)
	}
	r := f.store.all()[0]
	if r.Status != "Paid" || r.Payment.PaidAt == nil || r.Payment.PaidAmountCents != 400_000 || len(r.Overflow) != 0 {
		t.Errorf("settled = %+v", r)
	}
	select {
	case sent := <-f.texts:
		if sent.TeamName != "Kibera Stars" || sent.PhoneNumber != phoneN(1) || sent.AmountKES != 4_000 {
			t.Errorf("text for %+v", sent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no confirmation text was sent")
	}
	if got := f.notifications(); !reflect.DeepEqual(got, []string{out.Reference + " Completed"}) {
		t.Errorf("the payment page was told %v", got)
	}

	// Paystack repeats webhooks. The repeat changes nothing and texts nobody again.
	if rec := f.pay(out.Reference, 400_000); rec.Code != http.StatusOK {
		t.Errorf("repeat webhook = %d", rec.Code)
	}
	select {
	case <-f.texts:
		t.Error("a repeated webhook sent a second text")
	case <-time.After(100 * time.Millisecond):
	}
	if s := f.slot(t, "u10"); s.Paid != 1 || s.Held != 0 {
		t.Errorf("u10 = %+v, want one paid place", s)
	}
}

func TestAPaymentBelowTheFeeDoesNotSettle(t *testing.T) {
	f := newRegFlow()
	out := f.mustTeam(t, nil)
	if rec := f.pay(out.Reference, 100_000); rec.Code != http.StatusOK {
		t.Fatalf("webhook = %d", rec.Code)
	}
	if got := f.status(t, out.Reference); got != "Pending" {
		t.Errorf("status = %s; Ksh 1,000 is not the Ksh 2,000 fee", got)
	}
	// An amount Paystack did not send is accepted: refusing a confirmed payment is worse.
	if rec := f.pay(out.Reference, 0); rec.Code != http.StatusOK || f.status(t, out.Reference) != "Paid" {
		t.Errorf("unknown amount: %d %s", rec.Code, f.status(t, out.Reference))
	}
}

// Paying for one category does not settle a registration for two: the fee is
// 2,000 for each, and the check against it is made in the update itself.
func TestAPaymentForOneCategoryDoesNotSettleARegistrationForTwo(t *testing.T) {
	f := newRegFlow()
	out := f.mustTeam(t, body{"categories": []string{"u10", "adults"}})
	if out.Total != 4_000 {
		t.Fatalf("total = %d, want Ksh 4,000", out.Total)
	}
	f.pay(out.Reference, 200_000)
	if got := f.status(t, out.Reference); got != "Pending" {
		t.Errorf("status = %s; Ksh 2,000 is not the Ksh 4,000 fee for two categories", got)
	}
	f.pay(out.Reference, 400_000)
	if got := f.status(t, out.Reference); got != "Paid" {
		t.Errorf("status = %s after the full Ksh 4,000", got)
	}
}

// Each category is priced on its own: the Adults alone cost 2,000, all three 6,000.
func TestTheFeeFollowsTheNumberOfCategories(t *testing.T) {
	for _, c := range []struct {
		cats  []string
		total int
	}{{[]string{"adults"}, 2_000}, {[]string{"u15"}, 2_000}, {[]string{"u10", "u15"}, 4_000}, {[]string{"u10", "u15", "adults"}, 6_000}} {
		f := newRegFlow()
		out := f.mustTeam(t, body{"categories": c.cats})
		calls := f.gateway.all()
		if out.Total != c.total || len(calls) != 1 || calls[0].cents != c.total*100 {
			t.Errorf("%v: total %d, prompts %+v, want Ksh %d", c.cats, out.Total, calls, c.total)
		}
	}
}

func TestWebhookEventsThatAreNotAChargeOutcomeChangeNothing(t *testing.T) {
	f := newRegFlow()
	out := f.mustTeam(t, nil)
	for _, c := range []struct{ event, status string }{{"transfer.success", "success"}, {"refund.processed", "processed"}} {
		if rec := f.event(out.Reference, c.event, c.status); rec.Code != http.StatusOK {
			t.Errorf("%s = %d", c.event, rec.Code)
		}
	}
	// A charge.success in another currency cannot be compared with the fee.
	rec := httptest.NewRecorder()
	f.h.HandlePaystackEvent(rec, httptest.NewRequest(http.MethodPost, "/w", nil), paystackEvent{Event: "charge.success", Reference: out.Reference, Status: "success", Currency: "USD", AmountCents: 200_000})
	if got := f.status(t, out.Reference); got != "Pending" || len(f.notifications()) != 0 {
		t.Errorf("status %s, notified %v; nothing should have changed", got, f.notifications())
	}
}

func TestAFailedPaymentIsReportedButNeverUndoesAPaidOne(t *testing.T) {
	f := newRegFlow()
	a := f.mustTeam(t, body{"academyName": "Team A", "phoneNumber": phoneN(1)})
	b := f.mustTeam(t, body{"academyName": "Team B", "phoneNumber": phoneN(2)})

	f.event(a.Reference, "charge.failed", "failed")
	if f.status(t, a.Reference) != "Failed" || !contains(f.notifications(), a.Reference+" Failed") {
		t.Errorf("a: %s, notified %v", f.status(t, a.Reference), f.notifications())
	}
	if s := f.slot(t, "u10"); s.Held != 1 {
		t.Errorf("u10 = %+v, want only B's place held", s)
	}

	f.pay(b.Reference, 200_000)
	f.event(b.Reference, "charge.failed", "failed") // a stray event
	if got := f.status(t, b.Reference); got != "Paid" {
		t.Errorf("a paid registration became %s", got)
	}
	if contains(f.notifications(), b.Reference+" Failed") {
		t.Error("the payer of a paid registration was told it failed")
	}

	// A payment that lands after the registration was failed still settles it.
	f.pay(a.Reference, 200_000)
	if got := f.status(t, a.Reference); got != "Paid" {
		t.Errorf("a late payment left the registration %s", got)
	}
}

// The prompt went unanswered, the hold ran out and three other teams took the
// places; then the first team's PIN goes through. The money is real, so the
// registration is paid, but it is flagged: the admin has to refund it.
func TestAPaymentThatArrivesAfterItsPlaceWasGivenAwayIsFlaggedForARefund(t *testing.T) {
	f := newRegFlow()
	late := f.mustTeam(t, body{"academyName": "Late Payers", "phoneNumber": phoneN(1)})
	f.advance(registration.HoldWindow + time.Minute)

	var others []registered
	for i := 2; i <= 4; i++ {
		others = append(others, f.mustTeam(t, body{"academyName": fmt.Sprintf("Team %d", i), "phoneNumber": phoneN(i)}))
	}
	for _, o := range others {
		f.pay(o.Reference, 200_000)
	}
	for _, r := range f.store.all() {
		if len(r.Overflow) != 0 {
			t.Fatalf("setup: %s was flagged but is within the three: %+v", r.TeamName, r)
		}
	}

	f.pay(late.Reference, 200_000)
	var got models.Registration
	for _, r := range f.store.all() {
		if r.TeamName == "Late Payers" {
			got = r
		}
	}
	if got.Status != "Paid" || !reflect.DeepEqual(got.Overflow, []string{"u10"}) {
		t.Errorf("late payer = %+v, want Paid and flagged for u10", got)
	}
	// The teams that were in time are not touched.
	if s := f.slot(t, "u10"); s.Paid != 4 {
		t.Errorf("u10 = %+v", s)
	}
	_, _, view := f.adminTournament(t)
	flagged := 0
	for _, v := range view {
		if len(v.Overflow) > 0 {
			flagged++
			if v.AcademyName != "Late Payers" {
				t.Errorf("%s flagged", v.AcademyName)
			}
		}
	}
	if flagged != 1 {
		t.Errorf("%d registrations flagged in the admin list, want 1", flagged)
	}
}

func TestTheLastPayingTeamOfACategoryIsNotFlagged(t *testing.T) {
	for id, open := range map[string]int{"u10": 3, "u15": 3, "adults": 6} {
		f := newRegFlow()
		var refs []string
		for i := 1; i <= open; i++ {
			refs = append(refs, f.mustTeam(t, body{"academyName": fmt.Sprintf("Team %d", i), "phoneNumber": phoneN(i), "categories": []string{id}}).Reference)
		}
		for _, r := range refs {
			f.pay(r, 200_000)
		}
		for _, r := range f.store.all() {
			if r.Status != "Paid" || len(r.Overflow) != 0 {
				t.Errorf("%s: %s = %s overflow %v, want every open place paid and unflagged", id, r.TeamName, r.Status, r.Overflow)
			}
		}
	}
}

func TestTheWebhookFailsSoPaystackRetriesWhenTheDatabaseDoes(t *testing.T) {
	f := newRegFlow()
	out := f.mustTeam(t, nil)
	f.store.failures = map[string]error{"MarkPaid": errors.New("down")}
	if rec := f.pay(out.Reference, 200_000); rec.Code != http.StatusInternalServerError {
		t.Errorf("webhook with the database down = %d, want 500 so Paystack retries", rec.Code)
	}
	f.store.failures = map[string]error{"FlagOverflow": errors.New("down"), "TournamentSlots": errors.New("down")}
	if rec := f.pay(out.Reference, 200_000); rec.Code != http.StatusInternalServerError {
		t.Errorf("webhook that cannot check the places = %d, want 500", rec.Code)
	}
}

func TestThePaymentSocketSeesRegistrationStatuses(t *testing.T) {
	f := newRegFlow()
	out := f.mustTeam(t, nil)
	check := func(want string) {
		t.Helper()
		got, err := f.h.PaymentStatus(nil, out.Reference)
		if err != nil || got != want {
			t.Errorf("PaymentStatus = %q, %v; want %q", got, err, want)
		}
	}
	check("Pending")
	f.pay(out.Reference, 200_000)
	check("Completed")
	if _, err := f.h.PaymentStatus(nil, "tourney-"+strings.Repeat("0", 32)); !errors.Is(err, errUnknownReference) {
		t.Errorf("unknown reference: %v", err)
	}
	f.store.rows[0].Status = "Expired"
	check("Failed")
	f.store.rows[0].Status = "Failed"
	check("Failed")
}

// The same Paystack webhook serves pitch bookings, camp registrations and these:
// a registration payment must reach this handler and no other.
func TestTheSharedWebhookAndSocketRouteRegistrationPayments(t *testing.T) {
	f := newRegFlow()
	bh := NewBookingHandler(nil, &config.Config{PaystackSK: "sk_test_secret", AdminAPIKey: testAdminKey}, ws.NewPaymentHub())
	bh.AttachRegistrations(f.h)

	tour := f.mustTeam(t, nil)
	acad := func() registered { _, o := f.player(nil); return o }()

	send := func(ref string, amount int64) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"event": "charge.success", "data": map[string]any{
			"reference": ref, "status": "success", "currency": "KES", "amount": amount,
		}})
		mac := hmac.New(sha512.New, []byte("sk_test_secret"))
		mac.Write(raw)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/paystack", strings.NewReader(string(raw)))
		r.Header.Set("x-paystack-signature", hex.EncodeToString(mac.Sum(nil)))
		rec := httptest.NewRecorder()
		bh.PaystackWebhook(rec, r)
		return rec
	}
	if rec := send(tour.Reference, 200_000); rec.Code != http.StatusOK || f.status(t, tour.Reference) != "Paid" {
		t.Errorf("tournament webhook = %d, status %s", rec.Code, f.status(t, tour.Reference))
	}
	if rec := send(acad.Reference, 500_000); rec.Code != http.StatusOK || f.status(t, acad.Reference) != "Paid" {
		t.Errorf("academy webhook = %d, status %s", rec.Code, f.status(t, acad.Reference))
	}

	// A forged webhook is refused before it can settle anything.
	forged := httptest.NewRequest(http.MethodPost, "/api/v1/webhook/paystack", strings.NewReader(`{}`))
	forged.Header.Set("x-paystack-signature", strings.Repeat("ab", 64))
	if rec := httptest.NewRecorder(); func() int { bh.PaystackWebhook(rec, forged); return rec.Code }() != http.StatusUnauthorized {
		t.Error("an unsigned webhook must be refused")
	}

	if got, err := bh.paymentStatus(tour.Reference); err != nil || got != "Completed" {
		t.Errorf("socket status for a tournament payment = %q, %v", got, err)
	}
	if !validReference(tour.Reference) || !validReference(acad.Reference) {
		t.Error("the payment socket must accept registration references")
	}

	// With no registration handler attached the webhook acknowledges and ignores them.
	bh.AttachRegistrations(nil)
	if rec := send(tour.Reference, 200_000); rec.Code != http.StatusOK {
		t.Errorf("unattached = %d, want 200", rec.Code)
	}
	if _, err := bh.paymentStatus(tour.Reference); !errors.Is(err, errUnknownReference) {
		t.Errorf("unattached socket status: %v", err)
	}
}

// ---- the academy ------------------------------------------------------------------------------

func TestAnAcademyRegistrationChargesFiveThousand(t *testing.T) {
	f := newRegFlow()
	rec, out := f.player(body{"playerName": "  amani   Otieno ", "guardianName": "Grace Otieno"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d %q", rec.Code, rec.Body.String())
	}
	if out.Total != 5_000 || !strings.HasPrefix(out.Reference, "academy-") || out.Message != "Awaiting Payment" {
		t.Errorf("response = %+v", out)
	}
	calls := f.gateway.all()
	if len(calls) != 1 || calls[0].cents != 500_000 || calls[0].phone != phoneN(1) {
		t.Fatalf("payment requests = %+v, want one of Ksh 5,000", calls)
	}
	r := f.store.all()[0]
	if r.Kind != "academy" || r.PlayerName != "amani Otieno" || r.PlayerKey != "amani otieno" || r.DateOfBirth != "2016-03-14" ||
		r.GuardianName != "Grace Otieno" || r.AmountKES != 5_000 || r.Status != "Pending" || len(r.Categories) != 0 || r.TeamName != "" {
		t.Errorf("stored = %+v", r)
	}

	f.pay(out.Reference, 500_000)
	select {
	case sent := <-f.texts:
		if sent.Kind != "academy" || sent.PlayerName != "amani Otieno" {
			t.Errorf("text for %+v", sent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no confirmation text was sent")
	}
	// A tournament-sized payment does not settle an academy registration.
	_, other := f.player(body{"playerName": "Brian Kamau", "phoneNumber": phoneN(2)})
	f.pay(other.Reference, 200_000)
	if got := f.status(t, other.Reference); got != "Pending" {
		t.Errorf("Ksh 2,000 settled a Ksh 5,000 registration: %s", got)
	}
}

func TestAnAcademyRegistrationRefusesBadDetails(t *testing.T) {
	f := newRegFlow()
	for name, c := range map[string]struct {
		over body
		want int
		text string
	}{
		"no player name":          {body{"playerName": ""}, 400, "Player's name must be"},
		"markup in the name":      {body{"playerName": "<i>Amani</i>"}, 400, "Player's name"},
		"no parent name":          {body{"guardianName": ""}, 400, "Parent's name must be"},
		"no birth date":           {body{"dateOfBirth": ""}, 400, "date of birth"},
		"a birth date in 2030":    {body{"dateOfBirth": "2030-01-01"}, 400, "future"},
		"too young":               {body{"dateOfBirth": "2025-01-01"}, 400, "between 3 and 21"},
		"too old":                 {body{"dateOfBirth": "1990-01-01"}, 400, "between 3 and 21"},
		"day that does not exist": {body{"dateOfBirth": "2016-02-30"}, 400, "date of birth"},
		"bad phone":               {body{"phoneNumber": "123"}, 400, "valid Kenyan mobile"},
		"a huge body":             {body{"playerName": strings.Repeat("a", 10_000)}, 413, "too large"},
	} {
		rec, _ := f.player(c.over)
		if rec.Code != c.want || !strings.Contains(rec.Body.String(), c.text) {
			t.Errorf("%s = %d %q, want %d containing %q", name, rec.Code, rec.Body.String(), c.want, c.text)
		}
	}
	if len(f.store.all()) != 0 || len(f.gateway.all()) != 0 {
		t.Error("a refused registration must record nothing and send no prompt")
	}
}

func TestAPlayerIsNotRegisteredOrPromptedTwice(t *testing.T) {
	f := newRegFlow()
	first := func() registered { _, o := f.player(nil); return o }()

	// A double-click while the prompt may be live.
	rec, _ := f.player(nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already registered, or has a payment in progress") {
		t.Errorf("immediate repeat = %d %q", rec.Code, rec.Body.String())
	}
	if len(f.gateway.all()) != 1 {
		t.Errorf("%d prompts sent, want 1", len(f.gateway.all()))
	}

	// After the prompt can no longer be answered, the parent may try again.
	f.advance(academyPendingWindow + time.Second)
	if rec, _ := f.player(nil); rec.Code != http.StatusCreated {
		t.Errorf("retry after the window = %d %q", rec.Code, rec.Body.String())
	}

	// Once paid, the player is registered for good: no later attempt is charged.
	f.pay(first.Reference, 500_000)
	f.advance(90 * 24 * time.Hour)
	if rec, _ := f.player(nil); rec.Code != http.StatusConflict {
		t.Errorf("registering a paid player again = %d, want 409", rec.Code)
	}
	// Another child of the same parent, or the same name from another number, is not the same player.
	if rec, _ := f.player(body{"playerName": "Brian Otieno"}); rec.Code != http.StatusCreated {
		t.Errorf("a sibling = %d", rec.Code)
	}
	if rec, _ := f.player(body{"phoneNumber": phoneN(2)}); rec.Code != http.StatusCreated {
		t.Errorf("same name from another number = %d", rec.Code)
	}
}

// ---- information for the pages ------------------------------------------------------------------

func TestThePublicTournamentInfoStatesTheRulesOfTheFormAndNamesNobody(t *testing.T) {
	f := newRegFlow()
	f.mustTeam(t, body{"academyName": "Secret Name FC", "phoneNumber": "254799123456"})
	rec := httptest.NewRecorder()
	f.h.TournamentInfo(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tournament", nil))
	if strings.Contains(rec.Body.String(), "Secret") || strings.Contains(rec.Body.String(), "254799123456") {
		t.Errorf("the public info leaked a team: %s", rec.Body.String())
	}
	info := f.info(t)
	if info.FeePerCategory != 2_000 || info.MinPlayers != 5 || info.MaxPlayers != 40 || info.RulesVersion != registration.RulesVersion || info.HoldMinutes != 3 {
		t.Errorf("info = %+v", info)
	}
	if len(info.Categories) != 3 || info.Categories[0].ID != "u10" || info.Categories[0].Label != "U10" || info.Categories[1].ID != "u15" || info.Categories[2].ID != "adults" {
		t.Errorf("categories = %+v, want U10, U15 then Adults", info.Categories)
	}
	if info.Categories[0].Held != 1 || info.Categories[0].Left != 2 || info.Categories[1].Left != 3 || info.Categories[2].Left != 6 {
		t.Errorf("slots = %+v", info.Categories)
	}

	rec = httptest.NewRecorder()
	f.h.AcademyInfo(rec, httptest.NewRequest(http.MethodGet, "/api/v1/academy", nil))
	if strings.TrimSpace(rec.Body.String()) != `{"fee":5000}` {
		t.Errorf("academy info = %s", rec.Body.String())
	}

	f.store.failures = map[string]error{"TournamentSlots": errors.New("down")}
	rec = httptest.NewRecorder()
	f.h.TournamentInfo(rec, httptest.NewRequest(http.MethodGet, "/api/v1/tournament", nil))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "down") {
		t.Errorf("info with the database down = %d %q", rec.Code, rec.Body.String())
	}
}

// ---- the admin dashboard --------------------------------------------------------------------------

func (f *regFlow) adminMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/admin/tournament/registrations", f.h.AdminOnly(f.h.ListTournamentAdmin))
	mux.Handle("GET /api/v1/admin/academy/registrations", f.h.AdminOnly(f.h.ListAcademyAdmin))
	return mux
}

func (f *regFlow) admin(path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set("Authorization", "Bearer "+testAdminKey)
	rec := httptest.NewRecorder()
	f.adminMux().ServeHTTP(rec, r)
	return rec
}

func (f *regFlow) adminTournament(t *testing.T) (int, []slotView, []tournamentRegistrationView) {
	t.Helper()
	rec := f.admin("/api/v1/admin/tournament/registrations")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin tournament = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Fee           int                          `json:"feePerCategory"`
		Categories    []slotView                   `json:"categories"`
		Registrations []tournamentRegistrationView `json:"registrations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Fee, out.Categories, out.Registrations
}

func TestTheAdminSeesEveryTournamentRegistrationAndTheSlotsTaken(t *testing.T) {
	f := newRegFlow()
	a := f.mustTeam(t, body{"academyName": "Team A", "phoneNumber": phoneN(1), "players": 12, "categories": []string{"u10"}})
	f.advance(time.Minute)
	b := f.mustTeam(t, body{"academyName": "Team B", "phoneNumber": phoneN(2), "players": 20, "categories": []string{"u10", "u15"}})
	f.advance(time.Minute)
	c := f.mustTeam(t, body{"academyName": "Team C", "phoneNumber": phoneN(3)})
	f.pay(a.Reference, 200_000)
	f.event(c.Reference, "charge.failed", "failed") // a failed one is not listed
	_ = b

	_, slots, regs := f.adminTournament(t)
	var names []string
	byName := map[string]tournamentRegistrationView{}
	for _, r := range regs {
		names = append(names, r.AcademyName)
		byName[r.AcademyName] = r
	}
	if want := []string{"Team B", "Team A"}; !reflect.DeepEqual(names, want) {
		t.Errorf("registrations = %v, want the newest first and without the failed one: %v", names, want)
	}
	if v := byName["Team A"]; v.Players != 12 || v.PhoneNumber != phoneN(1) || v.Status != "Paid" || v.PaidAt == nil || v.AmountKES != 2_000 ||
		!reflect.DeepEqual(v.Categories, []string{"u10"}) || v.Overflow == nil || v.ID == "" || v.RulesVersion != registration.RulesVersion {
		t.Errorf("Team A = %+v", v)
	}
	if v := byName["Team B"]; v.Status != "Pending" || v.PaidAt != nil || !reflect.DeepEqual(v.Categories, []string{"u10", "u15"}) || v.Players != 20 || v.AmountKES != 4_000 {
		t.Errorf("Team B = %+v", v)
	}
	if len(slots) != 3 || slots[0].Paid != 1 || slots[0].Held != 1 || slots[0].Left != 1 || slots[0].Reserved != 1 || slots[1].Held != 1 || slots[1].Left != 2 || slots[2].Left != 6 {
		t.Errorf("slots = %+v", slots)
	}
}

func TestTheAdminSeesEveryAcademyRegistration(t *testing.T) {
	f := newRegFlow()
	_, a := f.player(body{"playerName": "Amani Otieno", "phoneNumber": phoneN(1)})
	f.advance(time.Minute)
	f.player(body{"playerName": "Brian Kamau", "guardianName": "Jane Kamau", "dateOfBirth": "2012-07-01", "phoneNumber": phoneN(2)})
	f.pay(a.Reference, 500_000)

	rec := f.admin("/api/v1/admin/academy/registrations")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin academy = %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Fee           int                       `json:"fee"`
		Registrations []academyRegistrationView `json:"registrations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Fee != 5_000 || len(out.Registrations) != 2 {
		t.Fatalf("out = %+v", out)
	}
	if v := out.Registrations[0]; v.PlayerName != "Brian Kamau" || v.GuardianName != "Jane Kamau" || v.DateOfBirth != "2012-07-01" || v.PhoneNumber != phoneN(2) || v.Status != "Pending" || v.AmountKES != 5_000 {
		t.Errorf("newest = %+v", v)
	}
	if v := out.Registrations[1]; v.PlayerName != "Amani Otieno" || v.Status != "Paid" || v.PaidAt == nil {
		t.Errorf("older = %+v", v)
	}
	// A tournament team is not in the academy list, nor the other way round.
	f.mustTeam(t, body{"phoneNumber": phoneN(3)})
	rec = f.admin("/api/v1/admin/academy/registrations")
	if strings.Contains(rec.Body.String(), "Kibera Stars") {
		t.Error("a tournament team appeared in the academy list")
	}
	if _, _, regs := f.adminTournament(t); len(regs) != 1 || regs[0].AcademyName != "Kibera Stars" {
		t.Errorf("tournament list = %+v", regs)
	}
}

func TestTheRegistrationAdminEndpointsNeedTheAdminKey(t *testing.T) {
	f := newRegFlow()
	f.mustTeam(t, nil)
	mux := f.adminMux()
	for _, path := range []string{"/api/v1/admin/tournament/registrations", "/api/v1/admin/academy/registrations"} {
		for name, auth := range map[string]string{"no key": "", "wrong key": "Bearer " + strings.Repeat("x", 32), "not a bearer token": testAdminKey} {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			if auth != "" {
				r.Header.Set("Authorization", auth)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, r)
			if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Body.String(), "Kibera") {
				t.Errorf("%s with %s = %d %q, want 401", path, name, rec.Code, rec.Body.String())
			}
		}
	}
	f.h.Config.AdminAPIKey = ""
	r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/tournament/registrations", nil)
	r.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("with no key configured = %d, want 503", rec.Code)
	}
}

func TestRegistrationFailsCleanlyWhenTheDatabaseDoes(t *testing.T) {
	for method, post := range map[string]func(f *regFlow) *httptest.ResponseRecorder{
		"TeamHolds":       func(f *regFlow) *httptest.ResponseRecorder { r, _ := f.team(nil); return r },
		"TournamentSlots": func(f *regFlow) *httptest.ResponseRecorder { r, _ := f.team(nil); return r },
		"Insert":          func(f *regFlow) *httptest.ResponseRecorder { r, _ := f.team(nil); return r },
		"PlayerHolds":     func(f *regFlow) *httptest.ResponseRecorder { r, _ := f.player(nil); return r },
	} {
		f := newRegFlow()
		f.store.failures = map[string]error{method: errors.New("down")}
		rec := post(f)
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "down") || !strings.Contains(rec.Body.String(), "You have not been charged") {
			t.Errorf("%s down: %d %q, want a 500 that says nobody was charged", method, rec.Code, rec.Body.String())
		}
		if len(f.gateway.all()) != 0 {
			t.Errorf("%s down: a prompt was sent", method)
		}
	}

	f := newRegFlow()
	f.store.failures = map[string]error{"ListForAdmin": errors.New("down")}
	for _, p := range []string{"/api/v1/admin/tournament/registrations", "/api/v1/admin/academy/registrations"} {
		if rec := f.admin(p); rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "down") {
			t.Errorf("%s with the database down = %d %q", p, rec.Code, rec.Body.String())
		}
	}
	f.store.failures = map[string]error{"TournamentSlots": errors.New("down")}
	if rec := f.admin("/api/v1/admin/tournament/registrations"); rec.Code != http.StatusInternalServerError {
		t.Errorf("admin list that cannot count the places = %d", rec.Code)
	}
}

// A refused Insert must not leave the number's attempt counted, and a team that
// could not be saved can simply try again.
func TestAFailedSaveDoesNotUseUpAnAttempt(t *testing.T) {
	f := newRegFlow()
	f.h.phoneLimiter = middleware.NewLimiter(0.3, 1)
	f.store.failures = map[string]error{"Insert": errors.New("down")}
	if rec, _ := f.team(nil); rec.Code != http.StatusInternalServerError {
		t.Fatalf("= %d", rec.Code)
	}
	f.store.failures = nil
	if rec, _ := f.team(nil); rec.Code != http.StatusCreated {
		t.Errorf("retry after a failed save = %d, want it allowed", rec.Code)
	}
}

// ---- the Mongo queries --------------------------------------------------------------------------------

func TestRegistrationQueriesUseRealFieldNames(t *testing.T) {
	paths := map[string]bool{}
	bsonPaths(reflect.TypeOf(models.Registration{}), "", paths)
	now := time.Now()
	id := primitive.NewObjectID()
	for name, q := range map[string]any{
		"tournamentSlotsFilter":       tournamentSlotsFilter(now, id),
		"teamHoldsFilter":             teamHoldsFilter("k", []string{"u10"}, now),
		"playerHoldsFilter":           playerHoldsFilter("k", "254712345678", now),
		"registrationPaidFilter":      registrationPaidFilter("tourney-x", 200_000),
		"registrationPaidFilter(0)":   registrationPaidFilter("tourney-x", 0),
		"registrationPaidUpdate":      registrationPaidUpdate(200_000, now),
		"registrationFailedFilter":    registrationFailedFilter("tourney-x"),
		"registrationFailedUpdate":    registrationFailedUpdate(now),
		"registrationReferenceFilter": registrationReferenceFilter("tourney-x"),
		"registrationIDFilter":        registrationIDFilter(id),
		"registrationOverflowUpdate":  registrationOverflowUpdate([]string{"u10"}),
		"registrationAdminFilter":     registrationAdminFilter("tournament"),
	} {
		var refs []string
		fieldRefs(q, &refs)
		if len(refs) == 0 {
			t.Errorf("%s mentions no fields; the walker is broken", name)
		}
		for _, ref := range refs {
			if !paths[ref] {
				t.Errorf("%s uses field %q, which is not a bson field of Registration", name, ref)
			}
		}
	}
}

func TestRegistrationQueriesCarryTheBusinessRules(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	after := now.Add(-registration.HoldWindow)

	// Only paid registrations, and unpaid ones young enough to be answered, hold a place.
	or := tournamentSlotsFilter(after, primitive.NilObjectID)["$or"].(primitive_A)
	if len(or) != 2 {
		t.Fatalf("$or = %v", or)
	}
	if or[0].(bsonM)["status"] != registration.StatusPaid {
		t.Errorf("first clause = %v, want the paid ones", or[0])
	}
	if p := or[1].(bsonM); p["status"] != registration.StatusPending || !reflect.DeepEqual(p["createdAt"], bsonM{"$gte": after}) {
		t.Errorf("second clause = %v, want Pending and recent", p)
	}
	if tournamentSlotsFilter(after, primitive.NilObjectID)["kind"] != registration.KindTournament {
		t.Error("slots must only count tournament registrations")
	}
	if playerHoldsFilter("k", "p", after)["kind"] != registration.KindAcademy {
		t.Error("a player check must only look at academy registrations")
	}

	// The fee check runs inside the same atomic update as the status change.
	if _, ok := registrationPaidFilter("r", 1)["$expr"]; !ok {
		t.Error("a payment with an amount must be checked against the fee in the update itself")
	}
	if _, ok := registrationPaidFilter("r", 0)["$expr"]; ok {
		t.Error("an unknown amount must be accepted")
	}
	if st := registrationPaidFilter("r", 1)["status"]; !reflect.DeepEqual(st, bsonM{"$ne": registration.StatusPaid}) {
		t.Errorf("already-paid registrations must not match again, got %v", st)
	}
	if registrationFailedFilter("r")["status"] != registration.StatusPending {
		t.Error("only a Pending registration may be failed: a paid one is never touched")
	}
	if got := registrationAdminFilter("academy")["status"]; !reflect.DeepEqual(got, bsonM{"$in": []string{registration.StatusPaid, registration.StatusPending}}) {
		t.Errorf("admin list statuses = %v", got)
	}
}

func TestCountSlotsAndHeld(t *testing.T) {
	regs := []models.Registration{
		{Categories: []string{"u10", "u15"}, Status: registration.StatusPaid},
		{Categories: []string{"u10"}, Status: registration.StatusPending},
		{Categories: []string{"u15"}, Status: registration.StatusPending},
		{Categories: []string{"u10"}, Status: registration.StatusPaid},
	}
	got := countSlots(regs)
	if got["u10"] != (registration.Slots{Paid: 2, Held: 1}) || got["u15"] != (registration.Slots{Paid: 1, Held: 1}) {
		t.Errorf("countSlots = %+v", got)
	}
	if got := held([]string{"u15", "u10"}, regs[:1]); !reflect.DeepEqual(got, []string{"u15", "u10"}) {
		t.Errorf("held = %v, want the wanted order", got)
	}
	if got := held([]string{"u10"}, nil); len(got) != 0 {
		t.Errorf("held with nothing = %v", got)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
