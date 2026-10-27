package schedule

import (
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

// Kenya's public holidays, which the venue treats like a weekend: open from
// 07:00 until 01:00 the next morning.
//
// They come from three places:
//
//   - the days the Public Holidays Act (Cap 110, Part I) fixes by date or by
//     Easter, worked out here for any year;
//   - the Idd holidays, which the Act leaves to a Gazette notice from the
//     Interior Cabinet Secretary a day or two beforehand, once the moon has been
//     sighted. They cannot be calculated, so each year's dates are added to
//     declaredHolidays below, or to the EXTRA_HOLIDAYS setting, when announced;
//   - EXTRA_HOLIDAYS itself, for any other day the Cabinet Secretary declares.
//
// A day that is not listed is an ordinary day, which fails safe: it keeps the
// shorter weekday hours.

type fixedHoliday struct {
	month time.Month
	day   int
	name  string
}

// fixedHolidays fall on the same date every year. Mazingira Day (10 October)
// replaced Utamaduni Day in the Act in 2024.
var fixedHolidays = []fixedHoliday{
	{time.January, 1, "New Year's Day"},
	{time.May, 1, "Labour Day"},
	{time.June, 1, "Madaraka Day"},
	{time.October, 10, "Mazingira Day"},
	{time.October, 20, "Mashujaa Day"},
	{time.December, 12, "Jamhuri Day"},
	{time.December, 25, "Christmas Day"},
	{time.December, 26, "Boxing Day"},
}

// declaredHolidays are the Idd holidays gazetted so far, as YYYY-MM-DD. Add
// each one when the Gazette notice is out (or set EXTRA_HOLIDAYS to do it
// without a code change). Gazette Notice dated 18 March 2026: Friday 20 March;
// special Gazette dated 25 May 2026: Wednesday 27 May.
var declaredHolidays = map[string]string{
	"2026-03-20": "Eid-ul-Fitr",
	"2026-05-27": "Eid-ul-Adha",
}

// extraHolidays are the dates configured with EXTRA_HOLIDAYS. It is replaced
// as a whole, so readers never see it half way through an update.
var extraHolidays atomic.Pointer[map[string]string]

// SetExtraHolidays replaces the configured holidays (date to name). Call it at
// start-up, before serving requests.
func SetExtraHolidays(extra map[string]string) {
	copied := make(map[string]string, len(extra))
	for date, name := range extra {
		copied[date] = name
	}
	extraHolidays.Store(&copied)
}

func extraHoliday(date string) (string, bool) {
	if m := extraHolidays.Load(); m != nil {
		name, ok := (*m)[date]
		return name, ok
	}
	return "", false
}

// declaredHoliday looks in the gazetted dates and then in the configured ones.
func declaredHoliday(date string) (string, bool) {
	if name, ok := extraHoliday(date); ok {
		return name, true
	}
	name, ok := declaredHolidays[date]
	return name, ok
}

// holidayName reports whether a date is a public holiday and what it is called.
// A configured name wins over a built-in one.
func holidayName(day time.Time) (string, bool) {
	date := day.Format("2006-01-02")
	if name, ok := declaredHoliday(date); ok {
		return name, true
	}
	name, ok := statutoryHolidays(day.Year())[date]
	return name, ok
}

// statutoryHolidays returns the holidays the Act itself sets for a year, keyed
// by date.
//
// Section 4 of the Act: where a day in Part I of the Schedule falls on a Sunday,
// the first succeeding day that is not itself a public holiday is a public
// holiday, and the Sunday ceases to be one. So 1 June 2025 (a Sunday) became
// Monday 2 June, and Christmas on a Sunday with Boxing Day on the Monday made
// the Tuesday a holiday, as in 2022.
func statutoryHolidays(year int) map[string]string {
	days := make(map[string]string, len(fixedHolidays)+2)
	for _, h := range fixedHolidays {
		days[time.Date(year, h.month, h.day, 0, 0, 0, 0, location).Format("2006-01-02")] = h.name
	}
	easter := easterSunday(year)
	days[easter.AddDate(0, 0, -2).Format("2006-01-02")] = "Good Friday"
	days[easter.AddDate(0, 0, 1).Format("2006-01-02")] = "Easter Monday"

	dates := make([]string, 0, len(days))
	for date := range days {
		dates = append(dates, date)
	}
	sort.Strings(dates) // earliest first, so a shifted holiday can push a later one on
	for _, date := range dates {
		day, _ := parseDate(date)
		if day.Weekday() != time.Sunday {
			continue
		}
		name := days[date]
		delete(days, date)
		next := day.AddDate(0, 0, 1)
		for taken(days, next) {
			next = next.AddDate(0, 0, 1)
		}
		days[next.Format("2006-01-02")] = name + " (observed)"
	}
	return days
}

func taken(days map[string]string, day time.Time) bool {
	date := day.Format("2006-01-02")
	if _, ok := days[date]; ok {
		return true
	}
	_, ok := declaredHoliday(date)
	return ok
}

// easterSunday is Easter Sunday in the Gregorian calendar (the "anonymous"
// algorithm of Meeus, Jones and Butcher).
func easterSunday(year int) time.Time {
	a := year % 19
	b, c := year/100, year%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(year, time.Month(month), day, 0, 0, 0, 0, location)
}

// Holiday is a public holiday on a date.
type Holiday struct {
	Date string
	Name string
}

// HolidaysBetween lists the public holidays from one date to another, both
// included, in date order.
func HolidaysBetween(from, to time.Time) []Holiday {
	var out []Holiday
	for day := dateOf(from); !day.After(dateOf(to)); day = day.AddDate(0, 0, 1) {
		if name, ok := holidayName(day); ok {
			out = append(out, Holiday{Date: day.Format("2006-01-02"), Name: name})
		}
	}
	return out
}

func dateOf(t time.Time) time.Time {
	t = t.In(location)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, location)
}

// LastDeclared is the latest date for which an Idd holiday (or any configured
// extra holiday) is known, or "" if there is none. Past it, Eid is not yet in
// the calendar and has to be added when it is gazetted.
func LastDeclared() string {
	last := ""
	for date := range declaredHolidays {
		last = max(last, date)
	}
	if m := extraHolidays.Load(); m != nil {
		for date := range *m {
			last = max(last, date)
		}
	}
	return last
}

// maxHolidayName bounds a configured holiday's name.
const maxHolidayName = 60

// ParseHolidays reads the EXTRA_HOLIDAYS setting: comma-separated entries that
// are a date, YYYY-MM-DD, optionally followed by "=" and a name, e.g.
// "2027-03-09=Eid-ul-Fitr,2027-05-16=Eid-ul-Adha". It returns the entries it
// understood, and a message for each one it could not use.
func ParseHolidays(raw string) (holidays map[string]string, problems []string) {
	holidays = map[string]string{}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		date, name, _ := strings.Cut(entry, "=")
		date, name = strings.TrimSpace(date), strings.TrimSpace(name)
		if _, ok := parseDate(date); !ok {
			problems = append(problems, fmt.Sprintf("%q: the date must be YYYY-MM-DD", entry))
			continue
		}
		if name == "" {
			name = "Public holiday"
		}
		if !validHolidayName(name) {
			problems = append(problems, fmt.Sprintf("%q: the name must be at most %d letters, digits, spaces or - . ' & ( )", entry, maxHolidayName))
			continue
		}
		holidays[date] = name
	}
	return holidays, problems
}

func validHolidayName(name string) bool {
	if len([]rune(name)) > maxHolidayName {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(" -.'&()", r) {
			return false
		}
	}
	return true
}
