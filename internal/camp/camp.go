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
	"strconv"
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
	TermsVersion = "camp-2026-v3"

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

// FormatKES renders shillings the way the site and text messages show them:
// "Ksh 3,000".
func FormatKES(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	digits := strconv.Itoa(n)
	var b strings.Builder
	for i, d := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	if neg {
		return "Ksh -" + b.String()
	}
	return "Ksh " + b.String()
}

// ProgramSummary describes what was bought: "Full day 8:30am - 4:00pm + lunch".
func ProgramSummary(p Program, lunch bool) string {
	s := strings.TrimSpace(p.Label + " " + p.Hours)
	if lunch {
		s += " + lunch"
	}
	return s
}

// WeeksLabel writes the chosen weeks as exact dates for a text message.
// Consecutive weeks are joined into one range, so the whole camp reads
// "2 Nov - 18 Dec" and weeks 1, 2 and 4 read "2 Nov - 13 Nov, 23-27 Nov".
func WeeksLabel(weekStarts []string) string {
	runs := weekRuns(weekStarts)
	labels := make([]string, len(runs))
	for i, r := range runs {
		labels[i] = r.label
	}
	return strings.Join(labels, ", ")
}

// WeekRunsLabel names the weeks for an error message, joining consecutive ones so
// that seven weeks read "weeks 1-7 (2 Nov - 18 Dec)" rather than seven entries:
// "week 1 (2-6 Nov), weeks 3-4 (16 Nov - 27 Nov)".
func WeekRunsLabel(weekStarts []string) string {
	runs := weekRuns(weekStarts)
	parts := make([]string, len(runs))
	for i, r := range runs {
		if r.first == r.last {
			parts[i] = fmt.Sprintf("week %d (%s)", r.first, r.label)
		} else {
			parts[i] = fmt.Sprintf("weeks %d-%d (%s)", r.first, r.last, r.label)
		}
	}
	return strings.Join(parts, ", ")
}

type weekRun struct {
	first, last int // week numbers
	label       string
}

// weekRuns sorts the known weeks among weekStarts and joins consecutive ones.
// Dates that are not camp weeks are ignored.
func weekRuns(weekStarts []string) []weekRun {
	index := make(map[string]Week)
	for _, w := range Weeks() {
		index[w.Start] = w
	}
	var chosen []Week
	for _, s := range weekStarts {
		if w, ok := index[s]; ok {
			chosen = append(chosen, w)
		}
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].Index < chosen[j].Index })

	var runs []weekRun
	for i := 0; i < len(chosen); {
		j := i
		for j+1 < len(chosen) && chosen[j+1].Index == chosen[j].Index+1 {
			j++
		}
		r := weekRun{first: chosen[i].Index, last: chosen[j].Index}
		if i == j {
			r.label = RangeLabel(chosen[i])
		} else {
			r.label = dayMonth(chosen[i].Start) + " - " + dayMonth(chosen[j].End)
		}
		runs = append(runs, r)
		i = j + 1
	}
	return runs
}

func dayMonth(date string) string {
	t, err := time.ParseInLocation("2006-01-02", date, schedule.Location())
	if err != nil {
		return date
	}
	return fmt.Sprintf("%d %s", t.Day(), t.Format("Jan"))
}
