// Package camp holds the business rules of the holiday football camp: which
// weeks it runs, what each programme costs, and how a registration is priced.
// The browser only names a programme, a lunch choice and the weeks it wants;
// every amount is worked out here, so nothing a client sends can change what it
// is charged.
package camp

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"turf-booking-backend/internal/schedule"
)

const (
	// TermsVersion identifies the terms and conditions text a parent ticked.
	// The form sends the version it displayed and registration is refused if it
	// is not this one, so a stale browser tab cannot record agreement to
	// wording that has since changed. Bump it together with the text in the
	// frontend (lib/camp.ts) whenever the terms change.
	TermsVersion = "camp-2026-v1"

	// ReferencePrefix marks a Paystack reference as a camp payment, which is how
	// the shared webhook and payment WebSocket tell camp payments from pitch
	// bookings.
	ReferencePrefix = "camp-"

	// LunchPerDayKES is charged for every camp day when lunch is added.
	LunchPerDayKES = 100
	// DaysPerWeek is the length of a camp week (Monday to Friday).
	DaysPerWeek = 5

	// firstMonday and lastFriday bound this year's camp (Africa/Nairobi dates).
	firstMonday = "2026-11-02"
	lastFriday  = "2026-12-18"

	// MaxWeeks caps a single registration; the camp is shorter than this.
	MaxWeeks = 10
)

// Programme statuses stored on a registration.
const (
	StatusPending = "Pending"
	StatusPaid    = "Paid"
	StatusFailed  = "Failed"
	// StatusExpired is a pending registration whose payment prompt went
	// unanswered. It is kept for a while so a payment that lands late can still
	// complete it, rather than taking the parent's money and giving nothing.
	StatusExpired = "Expired"
)

// Program is a camp day-length option.
type Program struct {
	ID       string
	Label    string
	Hours    string
	PriceKES int // per week
}

var programs = []Program{
	{ID: "fullDay", Label: "Full day", Hours: "8:30am - 4:00pm", PriceKES: 2_500},
	{ID: "halfDay", Label: "Half day", Hours: "8:30am - 12:30pm", PriceKES: 1_500},
}

// Programs lists the options a parent can choose from.
func Programs() []Program { return append([]Program(nil), programs...) }

// LookupProgram finds a programme by its id.
func LookupProgram(id string) (Program, bool) {
	for _, p := range programs {
		if p.ID == id {
			return p, true
		}
	}
	return Program{}, false
}

// Week is one Monday-to-Friday block of the camp.
type Week struct {
	Index int    // 1-based
	Start string // Monday, YYYY-MM-DD
	End   string // Friday, YYYY-MM-DD
}

// Weeks lists every week of the camp in order.
func Weeks() []Week {
	loc := schedule.Location()
	start, err1 := time.ParseInLocation("2006-01-02", firstMonday, loc)
	end, err2 := time.ParseInLocation("2006-01-02", lastFriday, loc)
	if err1 != nil || err2 != nil {
		panic("camp: invalid camp dates") // constants above; caught by the tests
	}
	var out []Week
	for monday, i := start, 1; !monday.After(end); monday, i = monday.AddDate(0, 0, 7), i+1 {
		out = append(out, Week{
			Index: i,
			Start: monday.Format("2006-01-02"),
			End:   monday.AddDate(0, 0, 4).Format("2006-01-02"),
		})
	}
	return out
}

// Quote is a priced registration.
type Quote struct {
	Program      Program
	Lunch        bool
	Weeks        []string // Mondays, ascending, no duplicates
	PerWeekKES   int      // programme plus lunch, for one week
	LunchWeekKES int      // lunch part of PerWeekKES, 0 when not added
	TotalKES     int
}

// NewQuote validates a programme, lunch choice and set of weeks against the
// camp calendar as of `now`, and prices them. Every error is safe to show to
// the parent.
func NewQuote(programID string, lunch bool, weekStarts []string, now time.Time) (Quote, error) {
	prog, ok := LookupProgram(programID)
	if !ok {
		return Quote{}, errors.New("please choose a full day or half day programme")
	}
	if len(weekStarts) == 0 {
		return Quote{}, errors.New("please choose at least one week")
	}
	if len(weekStarts) > MaxWeeks {
		return Quote{}, errors.New("too many weeks selected")
	}

	byStart := make(map[string]Week)
	for _, w := range Weeks() {
		byStart[w.Start] = w
	}
	today := schedule.Today(now)

	seen := make(map[string]struct{}, len(weekStarts))
	chosen := make([]string, 0, len(weekStarts))
	for _, s := range weekStarts {
		w, valid := byStart[s]
		if !valid {
			return Quote{}, errors.New("one of the selected weeks is not part of the camp")
		}
		if w.End < today { // YYYY-MM-DD compares chronologically
			return Quote{}, fmt.Errorf("week %d (%s) has already ended", w.Index, RangeLabel(w))
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		chosen = append(chosen, s)
	}
	sort.Strings(chosen)

	lunchWeek := 0
	if lunch {
		lunchWeek = LunchPerDayKES * DaysPerWeek
	}
	perWeek := prog.PriceKES + lunchWeek
	return Quote{
		Program:      prog,
		Lunch:        lunch,
		Weeks:        chosen,
		PerWeekKES:   perWeek,
		LunchWeekKES: lunchWeek,
		TotalKES:     perWeek * len(chosen),
	}, nil
}

// IsReference reports whether a payment reference belongs to the camp.
func IsReference(ref string) bool { return strings.HasPrefix(ref, ReferencePrefix) }

// RangeLabel renders a week as "2-6 Nov" (or "30 Nov - 4 Dec" across months).
func RangeLabel(w Week) string {
	loc := schedule.Location()
	s, e1 := time.ParseInLocation("2006-01-02", w.Start, loc)
	e, e2 := time.ParseInLocation("2006-01-02", w.End, loc)
	if e1 != nil || e2 != nil {
		return w.Start + " - " + w.End
	}
	if s.Month() == e.Month() {
		return fmt.Sprintf("%d-%d %s", s.Day(), e.Day(), s.Format("Jan"))
	}
	return fmt.Sprintf("%d %s - %d %s", s.Day(), s.Format("Jan"), e.Day(), e.Format("Jan"))
}

// WeeksLabel summarises chosen weeks for a text message: up to three weeks are
// listed by their Monday ("w/c 2 Nov, 9 Nov"), more are counted.
func WeeksLabel(weekStarts []string) string {
	if len(weekStarts) == 0 {
		return ""
	}
	if len(weekStarts) > 3 {
		return fmt.Sprintf("%d weeks", len(weekStarts))
	}
	loc := schedule.Location()
	parts := make([]string, 0, len(weekStarts))
	for _, s := range weekStarts {
		t, err := time.ParseInLocation("2006-01-02", s, loc)
		if err != nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", t.Day(), t.Format("Jan")))
	}
	return "w/c " + strings.Join(parts, ", ")
}
