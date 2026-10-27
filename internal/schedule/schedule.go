// Package schedule is the single source of truth for which sessions can be
// booked, what they cost, and whether a requested session still fits on the
// pitch. The client is never trusted for any of these: it only names a pitch
// type, a date and a time range, and the server derives everything else.
//
// The venue stays open past midnight, so a session belongs to the *operating
// day* it starts on, not to the calendar date it ends on. Friday's evening
// window runs from 15:00 to 01:00, and the 23:00 - 01:00 and 00:00 - 01:00
// sessions are Friday's, even though they finish on Saturday. Times within an
// operating day are minutes after that day's midnight and can therefore pass
// 1440; two sessions of the same operating day overlap in time exactly when
// their minute ranges overlap, which is what lets Fits work unchanged.
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	Pitch5Aside = "5Aside"
	Pitch8Aside = "8Aside"

	// PitchCapacity is measured in half-pitch units: a 5-a-side game uses one
	// unit, an 8-a-side game uses the whole pitch (two units).
	PitchCapacity = 2

	// BookableDays is how many days the booking calendar offers, starting with
	// the current operating day.
	BookableDays = 30

	// MaxAdvanceDays is how far ahead a session can be booked. The calendar
	// offers BookableDays; one extra day absorbs a client whose clock disagrees
	// with the venue's.
	MaxAdvanceDays = 31

	// MinRemaining mirrors the long-standing rule that a session can still be
	// booked until fewer than 30 minutes of it are left.
	MinRemaining = 30 * time.Minute

	minutesPerDay = 24 * 60

	// DayEnd is past the end of every possible session. A stored booking whose
	// time cannot be read is treated as lasting until here, so it blocks the
	// whole operating day, late-night sessions included.
	DayEnd = 2 * minutesPerDay

	// dayRollover separates the two ways a clock time can be read. The venue is
	// never open between its late closing and its morning opening, so a time
	// before this can only be in the small hours after midnight at the end of the
	// operating day being booked ("00:00 - 01:00" on Friday is Saturday's first
	// hour), and a time from here on is on the operating day itself.
	dayRollover = 5 * 60

	// maxWrapped is the longest range a stored row may have if it runs past
	// midnight: only 1 and 2 hour sessions can, so anything longer is bad data.
	maxWrapped = 4 * 60
)

var location = loadLocation()

// loadLocation never falls back to UTC: that would silently shift every
// session by three hours. East Africa Time has no DST, so a fixed offset is
// exact when the tz database is unavailable.
func loadLocation() *time.Location {
	if l, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return l
	}
	return time.FixedZone("EAT", 3*60*60)
}

// Location is the venue's timezone.
func Location() *time.Location { return location }

// Today returns the venue's current calendar date as YYYY-MM-DD. For deciding
// which day's bookings are still live use OperatingDate, which is the previous
// date until the venue closes in the early hours.
func Today(now time.Time) string { return now.In(location).Format("2006-01-02") }

// pricesKES is the only place prices live, keyed by pitch then session length
// in minutes.
var pricesKES = map[string]map[int]int{
	Pitch5Aside: {60: 3_000, 120: 6_000},
	Pitch8Aside: {60: 5_000, 120: 10_000},
}

// Units returns how much of the pitch a game of this type occupies.
func Units(pitchType string) (int, bool) {
	switch pitchType {
	case Pitch5Aside:
		return 1, true
	case Pitch8Aside:
		return 2, true
	}
	return 0, false
}

// PriceKES returns the price in whole shillings for a pitch type and session
// length in minutes.
func PriceKES(pitchType string, minutes int) (int, bool) {
	p, ok := pricesKES[pitchType][minutes]
	return p, ok
}

// DayKind says which opening hours apply to a date.
type DayKind string

const (
	// KindWeekday is an ordinary Monday to Friday.
	KindWeekday DayKind = "weekday"
	// KindWeekend is a Saturday or Sunday.
	KindWeekend DayKind = "weekend"
	// KindHoliday is a Kenyan public holiday, which gets the weekend hours
	// whatever day of the week it falls on.
	KindHoliday DayKind = "holiday"
)

// Window is a stretch of an operating day in which sessions can be booked, in
// minutes after that day's midnight. Close may pass 1440.
type Window struct{ Open, Close int }

func hm(hour, minute int) int { return hour*60 + minute }

// openingHours is the only place opening hours live. Weekdays have a short
// morning window and then the afternoon and evening, until 1am; weekends and
// public holidays are open from 7am until 1am the next morning.
var openingHours = map[DayKind][]Window{
	KindWeekday: {{hm(7, 0), hm(9, 0)}, {hm(15, 0), hm(25, 0)}},
	KindWeekend: {{hm(7, 0), hm(25, 0)}},
	KindHoliday: {{hm(7, 0), hm(25, 0)}},
}

// Kinds lists every day kind in a stable order.
func Kinds() []DayKind { return []DayKind{KindWeekday, KindWeekend, KindHoliday} }

// Windows returns the booking windows of a kind of day.
func Windows(kind DayKind) []Window {
	return append([]Window(nil), openingHours[kind]...)
}

// closeSpill is how far past midnight the latest closing time reaches. Until
// then the previous date is still the venue's current operating day.
var closeSpill = func() int {
	latest := 0
	for _, windows := range openingHours {
		for _, w := range windows {
			latest = max(latest, w.Close)
		}
	}
	return max(0, latest-minutesPerDay)
}()

// OperatingDate returns the date of the operating day that is under way at
// `now`, as YYYY-MM-DD: today, except in the early hours after midnight while
// yesterday's sessions are still running.
func OperatingDate(now time.Time) string {
	return operatingMidnight(now.In(location)).Format("2006-01-02")
}

func operatingMidnight(local time.Time) time.Time {
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	if local.Sub(midnight) < time.Duration(closeSpill)*time.Minute {
		midnight = midnight.AddDate(0, 0, -1)
	}
	return midnight
}

func parseDate(date string) (time.Time, bool) {
	if len(date) != len("2006-01-02") {
		return time.Time{}, false
	}
	day, err := time.ParseInLocation("2006-01-02", date, location)
	if err != nil || day.Format("2006-01-02") != date {
		return time.Time{}, false
	}
	return day, true
}

// KindOf says which opening hours apply on a date (YYYY-MM-DD). For a public
// holiday it also returns the holiday's name.
func KindOf(date string) (kind DayKind, holiday string, err error) {
	day, ok := parseDate(date)
	if !ok {
		return "", "", errors.New("invalid date")
	}
	kind, holiday = kindOfDay(day)
	return kind, holiday, nil
}

func kindOfDay(day time.Time) (DayKind, string) {
	if name, ok := holidayName(day); ok {
		return KindHoliday, name
	}
	if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return KindWeekend, ""
	}
	return KindWeekday, ""
}

// Slot is one bookable session on an operating day's own clock.
type Slot struct {
	Start int // minutes after the operating day's midnight
	End   int
}

// Minutes is the session length.
func (s Slot) Minutes() int { return s.End - s.Start }

// Label is the canonical form stored in the database and shown in the UI.
func (s Slot) Label() string { return FormatRange(s.Start, s.End) }

// Slots lists the sessions of the given length that a kind of day offers, in
// time order. A session has to fit inside one window, and starts on that
// window's grid, so a 2 hour session in a window that opens at 15:00 starts at
// 15:00, 17:00, 19:00 and so on.
func Slots(kind DayKind, minutes int) []Slot {
	if minutes <= 0 {
		return nil
	}
	var out []Slot
	for _, w := range openingHours[kind] {
		for start := w.Open; start+minutes <= w.Close; start += minutes {
			out = append(out, Slot{Start: start, End: start + minutes})
		}
	}
	return out
}

// offered reports whether a session of a kind of day is one that Slots lists.
func offered(kind DayKind, start, end int) bool {
	minutes := end - start
	for _, w := range openingHours[kind] {
		if start >= w.Open && end <= w.Close && (start-w.Open)%minutes == 0 {
			return true
		}
	}
	return false
}

// Session is a validated bookable window on one operating day.
type Session struct {
	Date  string
	Start int // minutes after midnight of Date; past 1440 for the small hours of the next morning
	End   int
	end   time.Time
}

// Minutes is the session length.
func (s Session) Minutes() int { return s.End - s.Start }

// Label is the canonical form stored in the database and shown in the UI:
// "16:00 – 18:00" with an en dash. A session that runs past midnight wraps
// ("23:00 – 01:00"); the date it is stored against is the day it starts on.
func (s Session) Label() string { return FormatRange(s.Start, s.End) }

// FormatClock renders minutes after midnight as HH:MM, wrapping past 24:00.
func FormatClock(minutes int) string {
	minutes %= minutesPerDay
	return fmt.Sprintf("%02d:%02d", minutes/60, minutes%60)
}

// FormatRange renders minutes-after-midnight as the canonical range label.
func FormatRange(start, end int) string {
	return FormatClock(start) + " – " + FormatClock(end)
}

// ParseSession validates a client-supplied date and time range against the
// opening hours and session grid, and the booking window relative to now.
// Every returned error is safe to show to the user.
func ParseSession(date, timeRange string, now time.Time) (Session, error) {
	day, ok := parseDate(date)
	if !ok {
		return Session{}, errors.New("invalid date")
	}

	nowLocal := now.In(location)
	current := operatingMidnight(nowLocal)
	if day.Before(current) {
		return Session{}, errors.New("booking closed: this date has already passed")
	}
	if day.After(current.AddDate(0, 0, MaxAdvanceDays)) {
		return Session{}, errors.New("bookings are only open up to a month ahead")
	}

	start, end, err := parseRangeStrict(timeRange)
	if err != nil {
		return Session{}, err
	}

	minutes := end - start
	if minutes != 60 && minutes != 120 {
		return Session{}, errors.New("sessions are either 1 hour or 2 hours long")
	}
	kind, _ := kindOfDay(day)
	if !offered(kind, start, end) {
		return Session{}, errors.New("that time is not an available session")
	}

	endsAt := day.Add(time.Duration(end) * time.Minute)
	if endsAt.Sub(nowLocal) < MinRemaining {
		return Session{}, errors.New("booking closed: less than 30 minutes remaining in this slot")
	}

	return Session{Date: date, Start: start, End: end, end: endsAt}, nil
}

// unwrap puts two clock times (minutes after midnight, 0-1439) on the
// operating day's own timeline: a start in the small hours is after midnight,
// and an end at or before the start is on the following morning.
func unwrap(from, to int) (start, end int) {
	start, end = from, to
	if start < dayRollover {
		start += minutesPerDay
	}
	for end <= start {
		end += minutesPerDay
	}
	return start, end
}

// ParseStoredRange reads a time range that is already in the database. It is
// deliberately more forgiving than the strict parser applied to new requests,
// because older rows predate the canonical format.
func ParseStoredRange(s string) (start, end int, ok bool) {
	parts := splitRange(s)
	if len(parts) != 2 {
		return 0, 0, false
	}
	a, errA := time.Parse("15:04", strings.TrimSpace(parts[0]))
	b, errB := time.Parse("15:04", strings.TrimSpace(parts[1]))
	if errA != nil || errB != nil {
		return 0, 0, false
	}
	from, to := a.Hour()*60+a.Minute(), b.Hour()*60+b.Minute()
	if to > from && from >= dayRollover {
		// An ordinary range within the day, which is every row written before the
		// venue opened late.
		return from, to, true
	}
	start, end = unwrap(from, to)
	return start, end, end-start <= maxWrapped
}

func splitRange(s string) []string {
	for _, dash := range []string{"–", "—", "−"} {
		s = strings.ReplaceAll(s, dash, "-")
	}
	return strings.Split(s, "-")
}

func parseRangeStrict(s string) (start, end int, err error) {
	invalid := errors.New("invalid time range")
	if len(s) > 32 {
		return 0, 0, invalid
	}
	parts := splitRange(s)
	if len(parts) != 2 {
		return 0, 0, invalid
	}
	from, ok1 := parseClock(strings.TrimSpace(parts[0]))
	to, ok2 := parseClock(strings.TrimSpace(parts[1]))
	if !ok1 || !ok2 {
		return 0, 0, invalid
	}
	start, end = unwrap(from, to)
	return start, end, nil
}

// parseClock accepts exactly HH:MM.
func parseClock(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	for _, i := range []int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	m := int(s[3]-'0')*10 + int(s[4]-'0')
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// SMSWhen words a stored booking's date and time for a text message. A session
// that starts after midnight is named by the date it really happens on, with
// the night it belongs to, so that nobody turns up a day early; every other
// session reads as it is stored.
func SMSWhen(date, label string) (dateText, timeText string) {
	day, ok := parseDate(date)
	start, _, parsed := ParseStoredRange(label)
	if !ok || !parsed || start < minutesPerDay {
		return date, label
	}
	actual := day.AddDate(0, 0, 1)
	return fmt.Sprintf("%s (%s night)", actual.Format("2006-01-02"), day.Weekday().String()[:3]), label
}

// Occupancy is an existing booking on the same operating day.
type Occupancy struct {
	Start, End int
	Units      int
}

// Fits reports whether a new booking of the given size can share the pitch
// with the existing bookings for its whole duration. Sessions of different
// lengths overlap (a 2 hour game spans two 1 hour games), so this checks the
// busiest moment inside the new window rather than comparing time labels.
func Fits(existing []Occupancy, start, end, units int) bool {
	// Load only changes where a booking starts, so those are the only moments
	// worth checking.
	moments := []int{start}
	for _, o := range existing {
		if o.Start > start && o.Start < end {
			moments = append(moments, o.Start)
		}
	}
	for _, t := range moments {
		load := units
		for _, o := range existing {
			if o.Start <= t && t < o.End {
				load += o.Units
			}
		}
		if load > PitchCapacity {
			return false
		}
	}
	return true
}
