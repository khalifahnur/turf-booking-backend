// Package schedule is the single source of truth for which sessions can be
// booked, what they cost, and whether a requested session still fits on the
// pitch. The client is never trusted for any of these: it only names a pitch
// type, a date and a time range, and the server derives everything else.
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

	// MaxAdvanceDays is how far ahead a session can be booked. The UI offers 30
	// days; one extra day absorbs timezone differences between client and venue.
	MaxAdvanceDays = 31

	// MinRemaining mirrors the long-standing rule that a session can still be
	// booked until fewer than 30 minutes of it are left.
	MinRemaining = 30 * time.Minute
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

// Today returns the venue's current date as YYYY-MM-DD.
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

// openingHours returns the first start and the last end of the day, in minutes
// after midnight.
func openingHours(day time.Weekday) (open, close int) {
	if day == time.Saturday || day == time.Sunday {
		return 8 * 60, 22 * 60
	}
	return 16 * 60, 22 * 60
}

// Session is a validated bookable window on a single date.
type Session struct {
	Date  string
	Start int // minutes after midnight
	End   int
	end   time.Time
}

// Minutes is the session length.
func (s Session) Minutes() int { return s.End - s.Start }

// Label is the canonical form stored in the database and shown in the UI:
// "16:00 – 18:00" with an en dash.
func (s Session) Label() string { return FormatRange(s.Start, s.End) }

// FormatRange renders minutes-after-midnight as the canonical range label.
func FormatRange(start, end int) string {
	return fmt.Sprintf("%02d:%02d – %02d:%02d", start/60, start%60, end/60, end%60)
}

// ParseSession validates a client-supplied date and time range against the
// opening hours and session grid, and the booking window relative to now.
// Every returned error is safe to show to the user.
func ParseSession(date, timeRange string, now time.Time) (Session, error) {
	if len(date) != len("2006-01-02") {
		return Session{}, errors.New("invalid date")
	}
	day, err := time.ParseInLocation("2006-01-02", date, location)
	if err != nil || day.Format("2006-01-02") != date {
		return Session{}, errors.New("invalid date")
	}

	nowLocal := now.In(location)
	today := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), 0, 0, 0, 0, location)
	if day.Before(today) {
		return Session{}, errors.New("booking closed: this date has already passed")
	}
	if day.After(today.AddDate(0, 0, MaxAdvanceDays)) {
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
	open, closing := openingHours(day.Weekday())
	if start < open || end > closing || (start-open)%minutes != 0 {
		return Session{}, errors.New("that time is not an available session")
	}

	endsAt := day.Add(time.Duration(end) * time.Minute)
	if endsAt.Sub(nowLocal) < MinRemaining {
		return Session{}, errors.New("booking closed: less than 30 minutes remaining in this slot")
	}

	return Session{Date: date, Start: start, End: end, end: endsAt}, nil
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
	start, end = a.Hour()*60+a.Minute(), b.Hour()*60+b.Minute()
	return start, end, end > start
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
	start, ok1 := parseClock(strings.TrimSpace(parts[0]))
	end, ok2 := parseClock(strings.TrimSpace(parts[1]))
	if !ok1 || !ok2 || end <= start {
		return 0, 0, invalid
	}
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

// Occupancy is an existing booking on the same date.
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
