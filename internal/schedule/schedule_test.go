package schedule

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// A fixed "now": Wednesday 2026-10-14, 12:00 in Nairobi.
var now = time.Date(2026, 10, 14, 12, 0, 0, 0, Location())

func at(date string, hour, minute int) time.Time {
	d, ok := parseDate(date)
	if !ok {
		panic("bad date " + date)
	}
	return d.Add(time.Duration(hour*60+minute) * time.Minute)
}

func labels(slots []Slot) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.Label()
	}
	return out
}

// The hours the venue asked for: weekdays 07:00-09:00 and 15:00-01:00,
// weekends and public holidays 07:00-01:00.
func TestSlotsForEachKindOfDay(t *testing.T) {
	weekday1h := []string{
		"07:00 – 08:00", "08:00 – 09:00",
		"15:00 – 16:00", "16:00 – 17:00", "17:00 – 18:00", "18:00 – 19:00", "19:00 – 20:00",
		"20:00 – 21:00", "21:00 – 22:00", "22:00 – 23:00", "23:00 – 00:00", "00:00 – 01:00",
	}
	weekday2h := []string{
		"07:00 – 09:00",
		"15:00 – 17:00", "17:00 – 19:00", "19:00 – 21:00", "21:00 – 23:00", "23:00 – 01:00",
	}
	weekend2h := []string{
		"07:00 – 09:00", "09:00 – 11:00", "11:00 – 13:00", "13:00 – 15:00", "15:00 – 17:00",
		"17:00 – 19:00", "19:00 – 21:00", "21:00 – 23:00", "23:00 – 01:00",
	}
	var weekend1h []string
	for h := 7; h < 25; h++ {
		weekend1h = append(weekend1h, FormatRange(h*60, (h+1)*60))
	}

	if got := labels(Slots(KindWeekday, 60)); !reflect.DeepEqual(got, weekday1h) {
		t.Errorf("weekday 1hr = %v", got)
	}
	if got := labels(Slots(KindWeekday, 120)); !reflect.DeepEqual(got, weekday2h) {
		t.Errorf("weekday 2hr = %v", got)
	}
	if got := labels(Slots(KindWeekend, 120)); !reflect.DeepEqual(got, weekend2h) {
		t.Errorf("weekend 2hr = %v", got)
	}
	if got := labels(Slots(KindWeekend, 60)); !reflect.DeepEqual(got, weekend1h) || len(got) != 18 {
		t.Errorf("weekend 1hr = %v (%d)", got, len(got))
	}
	// A public holiday is a weekend for opening hours.
	for _, minutes := range []int{60, 120} {
		if !reflect.DeepEqual(Slots(KindHoliday, minutes), Slots(KindWeekend, minutes)) {
			t.Errorf("a holiday must offer what a weekend offers (%d min)", minutes)
		}
	}
	if Slots(KindWeekday, 0) != nil || Slots("nonsense", 60) != nil {
		t.Error("no sessions for a zero length or an unknown kind")
	}
}

func TestOpeningHoursAreConsistent(t *testing.T) {
	for _, kind := range Kinds() {
		windows := Windows(kind)
		if len(windows) == 0 {
			t.Fatalf("%s has no windows", kind)
		}
		for i, w := range windows {
			if w.Open < dayRollover {
				t.Errorf("%s window %v opens before %d: a start that early is read as after midnight", kind, w, dayRollover)
			}
			if w.Close <= w.Open {
				t.Errorf("%s window %v is empty", kind, w)
			}
			if w.Close-minutesPerDay >= dayRollover {
				t.Errorf("%s window %v closes so late that its end would be read as the same day", kind, w)
			}
			if i > 0 && w.Open < windows[i-1].Close {
				t.Errorf("%s windows overlap: %v", kind, windows)
			}
		}
	}

	// Sessions are recorded per operating day, and bookings are locked and
	// checked per date. That is only sound if one day's sessions are over before
	// the next day's begin.
	for _, a := range Kinds() {
		latest := 0
		for _, w := range openingHours[a] {
			latest = max(latest, w.Close)
		}
		for _, b := range Kinds() {
			earliest := openingHours[b][0].Open
			if latest-minutesPerDay > earliest {
				t.Errorf("a %s day runs until %d minutes, past the opening of a following %s day at %d", a, latest-minutesPerDay, b, earliest)
			}
		}
	}
	if closeSpill != 60 {
		t.Errorf("closeSpill = %d, want 60 (01:00)", closeSpill)
	}
}

// Every label the page can offer must be read back as the same session, and
// the three late-night shapes must keep their meaning.
func TestEverySlotRoundTripsThroughTheParsers(t *testing.T) {
	for _, kind := range Kinds() {
		for _, minutes := range []int{60, 120} {
			for _, s := range Slots(kind, minutes) {
				start, end, err := parseRangeStrict(s.Label())
				if err != nil || start != s.Start || end != s.End {
					t.Errorf("%s %q: strict = %d-%d (%v), want %d-%d", kind, s.Label(), start, end, err, s.Start, s.End)
				}
				start, end, ok := ParseStoredRange(s.Label())
				if !ok || start != s.Start || end != s.End {
					t.Errorf("%s %q: stored = %d-%d (ok=%v), want %d-%d", kind, s.Label(), start, end, ok, s.Start, s.End)
				}
			}
		}
	}
	for _, c := range []struct {
		label      string
		start, end int
	}{
		{"23:00 – 01:00", 23 * 60, 25 * 60},
		{"23:00 – 00:00", 23 * 60, 24 * 60},
		{"00:00 – 01:00", 24 * 60, 25 * 60},
		{"07:00 – 09:00", 7 * 60, 9 * 60},
	} {
		start, end, err := parseRangeStrict(c.label)
		if err != nil || start != c.start || end != c.end {
			t.Errorf("%q = %d-%d (%v), want %d-%d", c.label, start, end, err, c.start, c.end)
		}
	}
}

func TestFormatting(t *testing.T) {
	for _, c := range []struct {
		start, end int
		want       string
	}{
		{16 * 60, 18 * 60, "16:00 – 18:00"},
		{23 * 60, 25 * 60, "23:00 – 01:00"},
		{23 * 60, 24 * 60, "23:00 – 00:00"},
		{24 * 60, 25 * 60, "00:00 – 01:00"},
		{7*60 + 30, 8*60 + 45, "07:30 – 08:45"},
	} {
		if got := FormatRange(c.start, c.end); got != c.want {
			t.Errorf("FormatRange(%d, %d) = %q, want %q", c.start, c.end, got, c.want)
		}
	}
	if !strings.Contains(Session{Start: 1380, End: 1500}.Label(), " – ") {
		t.Error("label is not canonical")
	}
}

func TestParseSessionAcceptsOfferedSessions(t *testing.T) {
	cases := []struct{ name, date, rng string }{
		{"weekday morning 2hr", "2026-10-15", "07:00 – 09:00"},
		{"weekday morning 1hr", "2026-10-15", "08:00 – 09:00"},
		{"weekday afternoon 2hr", "2026-10-15", "15:00 – 17:00"},
		{"weekday evening 2hr", "2026-10-15", "21:00 – 23:00"},
		{"weekday late 2hr crosses midnight", "2026-10-15", "23:00 – 01:00"},
		{"weekday 1hr until midnight", "2026-10-15", "23:00 – 00:00"},
		{"weekday last hour, after midnight", "2026-10-15", "00:00 – 01:00"},
		{"weekday 1hr", "2026-10-15", "15:00 – 16:00"},
		{"Friday night into Saturday", "2026-10-16", "23:00 – 01:00"},
		{"Saturday morning 2hr", "2026-10-17", "07:00 – 09:00"},
		{"Saturday midday 2hr", "2026-10-17", "11:00 – 13:00"},
		{"Saturday 1hr", "2026-10-17", "12:00 – 13:00"},
		{"Sunday late 2hr", "2026-10-18", "23:00 – 01:00"},
		{"Sunday last hour", "2026-10-18", "00:00 – 01:00"},
		{"holiday weekday midday (Mashujaa Day is a Tuesday)", "2026-10-20", "11:00 – 13:00"},
		{"holiday weekday 1hr", "2026-10-20", "12:00 – 13:00"},
		{"ascii hyphen is canonicalised", "2026-10-15", "15:00 - 17:00"},
		{"no spaces", "2026-10-15", "15:00-17:00"},
	}
	for _, c := range cases {
		s, err := ParseSession(c.date, c.rng, now)
		if err != nil {
			t.Errorf("%s: %s %q: unexpected error: %v", c.name, c.date, c.rng, err)
			continue
		}
		if !strings.Contains(s.Label(), " – ") {
			t.Errorf("label %q is not canonical", s.Label())
		}
	}
}

func TestParseSessionRejectsOffGridAndAbusiveRanges(t *testing.T) {
	cases := []struct{ name, date, rng string }{
		{"all day", "2026-10-15", "00:00 – 23:59"},
		{"weekday mid-day", "2026-10-15", "12:00 – 14:00"},
		{"weekday mid-day 1hr", "2026-10-15", "12:00 – 13:00"},
		{"weekday after the morning window", "2026-10-15", "09:00 – 10:00"},
		{"weekday morning 2hr off the grid", "2026-10-15", "08:00 – 10:00"},
		{"weekday just before the afternoon", "2026-10-15", "14:00 – 15:00"},
		{"weekday 2hr before opening", "2026-10-15", "14:00 – 16:00"},
		{"the old weekday grid", "2026-10-15", "16:00 – 18:00"},
		{"after closing", "2026-10-15", "01:00 – 02:00"},
		{"past closing", "2026-10-15", "00:00 – 02:00"},
		{"small hours", "2026-10-15", "03:00 – 04:00"},
		{"before opening", "2026-10-17", "06:00 – 07:00"},
		{"before opening, small hours", "2026-10-17", "05:00 – 07:00"},
		{"misaligned weekday evening 2hr", "2026-10-15", "22:00 – 00:00"},
		{"misaligned weekend 2hr", "2026-10-17", "08:00 – 10:00"},
		{"the old weekend grid", "2026-10-17", "10:00 – 12:00"},
		{"misaligned holiday 2hr", "2026-10-20", "12:00 – 14:00"},
		{"90 minutes", "2026-10-15", "15:00 – 16:30"},
		{"3 hours", "2026-10-15", "15:00 – 18:00"},
		{"22 hours (reversed)", "2026-10-15", "18:00 – 16:00"},
		{"empty", "2026-10-15", ""},
		{"garbage", "2026-10-15", "soon"},
		{"three parts", "2026-10-15", "15:00 – 16:00 – 17:00"},
		{"single digit hour", "2026-10-15", "7:00 – 9:00"},
		{"hour 24", "2026-10-15", "24:00 – 01:00"},
		{"hour 25", "2026-10-15", "23:00 – 25:00"},
		{"oversized", "2026-10-15", strings.Repeat("1", 500)},
		{"past date", "2026-10-13", "15:00 – 17:00"},
		{"too far ahead", "2026-12-25", "15:00 – 17:00"},
		{"bad date", "2026-13-40", "15:00 – 17:00"},
		{"unpadded date", "2026-1-5", "15:00 – 17:00"},
		{"date with junk", "2026-10-15'; drop", "15:00 – 17:00"},
	}
	for _, c := range cases {
		if _, err := ParseSession(c.date, c.rng, now); err == nil {
			t.Errorf("%s: %s %q should have been rejected", c.name, c.date, c.rng)
		}
	}
}

// The same clock time is open or shut depending on the kind of day, and a
// public holiday on a weekday is open all day.
func TestParseSessionDependsOnTheKindOfDay(t *testing.T) {
	const midday = "11:00 – 13:00"
	for date, want := range map[string]bool{
		"2026-10-15": false, // Thursday
		"2026-10-17": true,  // Saturday
		"2026-10-18": true,  // Sunday
		"2026-10-20": true,  // Tuesday, but Mashujaa Day
		"2026-10-21": false, // Wednesday again
	} {
		_, err := ParseSession(date, midday, now)
		if (err == nil) != want {
			t.Errorf("%s %s: accepted=%v, want %v (%v)", date, midday, err == nil, want, err)
		}
	}
}

func TestParseSessionCutoff(t *testing.T) {
	// 2hr session 15:00-17:00 ends at 17:00.
	cases := []struct {
		at      time.Time
		allowed bool
	}{
		{time.Date(2026, 10, 14, 14, 0, 0, 0, Location()), true},
		{time.Date(2026, 10, 14, 16, 29, 0, 0, Location()), true},
		{time.Date(2026, 10, 14, 16, 30, 0, 0, Location()), true}, // exactly 30m left
		{time.Date(2026, 10, 14, 16, 31, 0, 0, Location()), false},
		{time.Date(2026, 10, 14, 18, 0, 0, 0, Location()), false},
	}
	for _, c := range cases {
		_, err := ParseSession("2026-10-14", "15:00 – 17:00", c.at)
		if (err == nil) != c.allowed {
			t.Errorf("at %s: allowed=%v, err=%v", c.at.Format("15:04"), c.allowed, err)
		}
	}
}

// A session that runs past midnight ends on the next calendar date, so its
// cut-off is on the next date too.
func TestParseSessionCutoffAcrossMidnight(t *testing.T) {
	cases := []struct {
		date, rng string
		at        time.Time
		allowed   bool
	}{
		{"2026-10-14", "23:00 – 01:00", at("2026-10-14", 23, 30), true},
		{"2026-10-14", "23:00 – 01:00", at("2026-10-15", 0, 30), true},  // exactly 30m left
		{"2026-10-14", "23:00 – 01:00", at("2026-10-15", 0, 31), false}, // 29m left
		{"2026-10-14", "00:00 – 01:00", at("2026-10-14", 23, 0), true},  // yet to start
		{"2026-10-14", "00:00 – 01:00", at("2026-10-15", 0, 29), true},
		{"2026-10-14", "00:00 – 01:00", at("2026-10-15", 0, 31), false},
		{"2026-10-14", "23:00 – 00:00", at("2026-10-14", 23, 30), true},
		{"2026-10-14", "23:00 – 00:00", at("2026-10-14", 23, 31), false},
	}
	for _, c := range cases {
		_, err := ParseSession(c.date, c.rng, c.at)
		if (err == nil) != c.allowed {
			t.Errorf("%s %q at %s: allowed=%v, err=%v", c.date, c.rng, c.at.Format("Mon 15:04"), err == nil, err)
		}
	}
}

// Until the venue closes at 01:00 the previous date is still the operating day:
// its last sessions can be booked, and nothing older can.
func TestOperatingDay(t *testing.T) {
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{at("2026-10-16", 0, 0), "2026-10-15"},
		{at("2026-10-16", 0, 59), "2026-10-15"},
		{at("2026-10-16", 1, 0), "2026-10-16"},
		{at("2026-10-16", 7, 0), "2026-10-16"},
		{at("2026-10-16", 23, 59), "2026-10-16"},
	} {
		if got := OperatingDate(c.at); got != c.want {
			t.Errorf("OperatingDate(%s) = %s, want %s", c.at.Format("Mon 15:04"), got, c.want)
		}
	}
	// Today is the calendar date; it does not change at 01:00.
	if got := Today(at("2026-10-16", 0, 30)); got != "2026-10-16" {
		t.Errorf("Today = %s", got)
	}

	// 00:10 on Friday 16th: Thursday's 00:00-01:00 session has 50 minutes left.
	early := at("2026-10-16", 0, 10)
	if _, err := ParseSession("2026-10-15", "00:00 – 01:00", early); err != nil {
		t.Errorf("yesterday's last hour should still be bookable at 00:10: %v", err)
	}
	if _, err := ParseSession("2026-10-15", "23:00 – 01:00", early); err != nil {
		t.Errorf("yesterday's 23:00 - 01:00 should still be bookable at 00:10: %v", err)
	}
	if _, err := ParseSession("2026-10-15", "21:00 – 23:00", early); err == nil {
		t.Error("a session that has ended must not be bookable")
	}
	if _, err := ParseSession("2026-10-14", "00:00 – 01:00", early); err == nil {
		t.Error("the day before yesterday must be closed")
	}
	if _, err := ParseSession("2026-10-15", "00:00 – 01:00", at("2026-10-16", 0, 45)); err == nil {
		t.Error("with 15 minutes left, the last hour is closed")
	}
	// From 01:00 yesterday is over.
	if _, err := ParseSession("2026-10-15", "00:00 – 01:00", at("2026-10-16", 1, 0)); err == nil {
		t.Error("yesterday must be closed from 01:00")
	}
	if _, err := ParseSession("2026-10-16", "07:00 – 09:00", at("2026-10-16", 1, 0)); err != nil {
		t.Errorf("today's morning session should be bookable at 01:00: %v", err)
	}
}

func TestParseSessionUsesVenueTimezone(t *testing.T) {
	// 22:00 UTC on the 14th is already 01:00 on the 15th in Nairobi: the venue has
	// closed, so the 14th has passed.
	lateUTC := time.Date(2026, 10, 14, 22, 0, 0, 0, time.UTC)
	if _, err := ParseSession("2026-10-14", "15:00 – 17:00", lateUTC); err == nil {
		t.Error("expected the 14th to be closed once it is 01:00 on the 15th in Nairobi")
	}
	// 21:10 UTC is 00:10 in Nairobi: the 14th's last hour is still on offer.
	if _, err := ParseSession("2026-10-14", "00:00 – 01:00", time.Date(2026, 10, 14, 21, 10, 0, 0, time.UTC)); err != nil {
		t.Errorf("the 14th's last hour is open at 00:10 Nairobi time: %v", err)
	}
}

func TestPricing(t *testing.T) {
	cases := []struct {
		pitch string
		mins  int
		price int
	}{
		{Pitch5Aside, 60, 3_000},
		{Pitch8Aside, 60, 5_000},
		{Pitch5Aside, 120, 6_000},
		{Pitch8Aside, 120, 10_000},
	}
	for _, c := range cases {
		got, ok := PriceKES(c.pitch, c.mins)
		if !ok || got != c.price {
			t.Errorf("%s %dmin = %d (ok=%v), want %d", c.pitch, c.mins, got, ok, c.price)
		}
	}
	if _, ok := PriceKES("9Aside", 60); ok {
		t.Error("unknown pitch type must not have a price")
	}
	if _, ok := PriceKES(Pitch5Aside, 90); ok {
		t.Error("unknown duration must not have a price")
	}
}

func TestFits(t *testing.T) {
	h := func(hour int) int { return hour * 60 }
	occ := func(from, to, units int) Occupancy { return Occupancy{h(from), h(to), units} }

	cases := []struct {
		name     string
		existing []Occupancy
		start    int
		end      int
		units    int
		want     bool
	}{
		{"empty pitch, 8-aside", nil, h(18), h(20), 2, true},
		{"one 5-aside leaves room for another 5-aside", []Occupancy{occ(18, 20, 1)}, h(18), h(20), 1, true},
		{"one 5-aside blocks an 8-aside", []Occupancy{occ(18, 20, 1)}, h(18), h(20), 2, false},
		{"two 5-asides fill it", []Occupancy{occ(18, 20, 1), occ(18, 20, 1)}, h(18), h(20), 1, false},
		{"8-aside fills it", []Occupancy{occ(18, 20, 2)}, h(18), h(20), 1, false},
		{"back-to-back sessions do not clash", []Occupancy{occ(18, 20, 2)}, h(20), h(22), 2, true},
		{"1hr 8-aside blocks the overlapping 2hr", []Occupancy{occ(18, 19, 2)}, h(18), h(20), 1, false},
		{"1hr in the second hour blocks the 2hr 8-aside", []Occupancy{occ(19, 20, 1)}, h(18), h(20), 2, false},
		{"1hr 8-aside blocks only its own hour", []Occupancy{occ(18, 19, 2)}, h(19), h(20), 2, true},
		{"2hr 8-aside blocks both 1hr halves", []Occupancy{occ(18, 20, 2)}, h(19), h(20), 1, false},
		{"2hr 5-aside leaves 1hr 5-aside", []Occupancy{occ(18, 20, 1)}, h(19), h(20), 1, true},
		{"2hr 5-aside blocks 1hr 8-aside", []Occupancy{occ(18, 20, 1)}, h(19), h(20), 2, false},
		{"staggered 5-asides peak at 2 in the middle",
			[]Occupancy{occ(18, 19, 1), occ(19, 20, 1)}, h(18), h(20), 1, true},
		{"overlap peak is found even when it starts mid-window",
			[]Occupancy{occ(19, 20, 1), occ(19, 20, 1)}, h(18), h(20), 1, false},

		// Sessions that run into the next morning share the pitch with the
		// sessions around midnight, on the operating day's own timeline.
		{"23-01 8-aside blocks the 00-01 hour", []Occupancy{occ(23, 25, 2)}, h(24), h(25), 1, false},
		{"23-01 8-aside blocks the 23-00 hour", []Occupancy{occ(23, 25, 2)}, h(23), h(24), 1, false},
		{"23-01 8-aside leaves the hour before", []Occupancy{occ(23, 25, 2)}, h(22), h(23), 2, true},
		{"00-01 8-aside blocks a 23-01 session", []Occupancy{occ(24, 25, 2)}, h(23), h(25), 1, false},
		{"00-01 5-aside lets a 23-01 5-aside in", []Occupancy{occ(24, 25, 1)}, h(23), h(25), 1, true},
		{"00-01 5-aside blocks a 23-01 8-aside", []Occupancy{occ(24, 25, 1)}, h(23), h(25), 2, false},
		{"23-00 8-aside leaves the 00-01 hour", []Occupancy{occ(23, 24, 2)}, h(24), h(25), 2, true},
		{"a legacy 20-22 booking blocks the 21-23 session", []Occupancy{occ(20, 22, 2)}, h(21), h(23), 1, false},
		{"a legacy 16-18 booking blocks 15-17 and 17-19 but not 19-21",
			[]Occupancy{occ(16, 18, 2)}, h(19), h(21), 2, true},
	}
	for _, c := range cases {
		if got := Fits(c.existing, c.start, c.end, c.units); got != c.want {
			t.Errorf("%s: Fits = %v, want %v", c.name, got, c.want)
		}
	}
	// The old rows overlap the new grid on both sides.
	legacy := []Occupancy{occ(16, 18, 2)}
	for _, s := range Slots(KindWeekday, 120) {
		overlaps := s.Start < h(18) && s.End > h(16)
		if got := Fits(legacy, s.Start, s.End, 1); got == overlaps {
			t.Errorf("legacy 16-18 vs new %s: fits=%v, overlaps=%v", s.Label(), got, overlaps)
		}
	}
}

func TestParseStoredRange(t *testing.T) {
	for _, c := range []struct {
		in         string
		start, end int
	}{
		{"16:00 – 18:00", 960, 1080},
		{"16:00 - 18:00", 960, 1080},
		{"16:00-18:00", 960, 1080},
		{"9:00 – 11:00", 540, 660},
		{"08:00 – 22:00", 480, 1320}, // an old, longer row is still read as written
		{"23:00 – 01:00", 1380, 1500},
		{"00:00 – 01:00", 1440, 1500},
		{"23:00 – 00:00", 1380, 1440},
	} {
		start, end, ok := ParseStoredRange(c.in)
		if !ok || start != c.start || end != c.end {
			t.Errorf("%q = %d-%d (ok=%v), want %d-%d", c.in, start, end, ok, c.start, c.end)
		}
	}
	for _, in := range []string{"", "x", "16:00", "18:00 – 16:00", "20:00 – 20:00", "10:00 – 09:00", "25:00 – 26:00"} {
		if _, _, ok := ParseStoredRange(in); ok {
			t.Errorf("%q should not parse", in)
		}
	}
}

func TestSMSWhen(t *testing.T) {
	for _, c := range []struct {
		date, label, wantDate, wantTime string
	}{
		// Ordinary sessions read as they are stored.
		{"2026-10-16", "16:00 – 18:00", "2026-10-16", "16:00 – 18:00"},
		{"2026-10-16", "23:00 – 01:00", "2026-10-16", "23:00 – 01:00"},
		{"2026-10-16", "23:00 – 00:00", "2026-10-16", "23:00 – 00:00"},
		// A session after midnight is named by the date it happens on.
		{"2026-10-16", "00:00 – 01:00", "2026-10-17 (Fri night)", "00:00 – 01:00"},
		{"2026-10-18", "00:00 – 01:00", "2026-10-19 (Sun night)", "00:00 – 01:00"},
		{"2026-12-31", "00:00 – 01:00", "2027-01-01 (Thu night)", "00:00 – 01:00"},
		// What cannot be understood is passed through untouched.
		{"2026-10-16", "garbage", "2026-10-16", "garbage"},
		{"not a date", "00:00 – 01:00", "not a date", "00:00 – 01:00"},
	} {
		d, tm := SMSWhen(c.date, c.label)
		if d != c.wantDate || tm != c.wantTime {
			t.Errorf("SMSWhen(%q, %q) = %q, %q; want %q, %q", c.date, c.label, d, tm, c.wantDate, c.wantTime)
		}
	}
}

// Everything the calendar offers must be accepted by the booking check, for
// the day it is offered on, and nothing else on those days may be.
func TestSnapshotMatchesTheBookingCheck(t *testing.T) {
	// 01:00 on the 14th: the 14th's operating day has just begun, so none of its
	// sessions has started or is about to close.
	start := at("2026-10-14", 1, 0)
	snap := NewSnapshot(start)

	if snap.Today != "2026-10-14" || len(snap.Days) != BookableDays || snap.Days[0].Date != "2026-10-14" {
		t.Fatalf("snapshot starts %s with %d days (first %s)", snap.Today, len(snap.Days), snap.Days[0].Date)
	}
	if snap.Timezone != "Africa/Nairobi" || snap.MinRemainingMinutes != 30 {
		t.Errorf("timezone %q, min remaining %d", snap.Timezone, snap.MinRemainingMinutes)
	}
	for _, kind := range Kinds() {
		k, ok := snap.Kinds[kind]
		if !ok || len(k.Windows) == 0 || len(k.Sessions) == 0 {
			t.Fatalf("snapshot has nothing for %s", kind)
		}
	}

	for i, d := range snap.Days {
		kind, holiday, err := KindOf(d.Date)
		if err != nil || d.Kind != kind || d.Holiday != holiday {
			t.Errorf("day %s = %s %q, KindOf says %s %q", d.Date, d.Kind, d.Holiday, kind, holiday)
		}
		if i > 0 && d.Date <= snap.Days[i-1].Date {
			t.Errorf("days are not in order at %s", d.Date)
		}
		offered := map[string]bool{}
		for _, s := range snap.Kinds[d.Kind].Sessions {
			offered[s.Label] = true
			if _, err := ParseSession(d.Date, s.Label, start); err != nil {
				t.Errorf("%s offers %s %q (%d min) but the booking check refuses it: %v", d.Kind, d.Date, s.Label, s.Minutes, err)
			}
			if got := FormatRange(s.From, s.To); got != s.Label || FormatClock(s.From) != s.Start || FormatClock(s.To) != s.End || s.To-s.From != s.Minutes {
				t.Errorf("inconsistent session %+v", s)
			}
		}
		// Every other quarter hour of the day, at both lengths, must be refused.
		for from := 0; from < 26*60; from += 15 {
			for _, minutes := range []int{60, 120} {
				label := FormatRange(from, from+minutes)
				if offered[label] {
					continue
				}
				if _, err := ParseSession(d.Date, label, start); err == nil {
					t.Errorf("%s %s: %q is accepted but the schedule does not offer it", d.Kind, d.Date, label)
				}
			}
		}
	}
}

func TestSnapshotStartsOnTheOperatingDay(t *testing.T) {
	snap := NewSnapshot(at("2026-10-16", 0, 30))
	if snap.Today != "2026-10-15" || snap.Days[0].Date != "2026-10-15" {
		t.Errorf("at 00:30 the calendar should start on the 15th, got %s / %s", snap.Today, snap.Days[0].Date)
	}
	snap = NewSnapshot(at("2026-10-16", 1, 0))
	if snap.Today != "2026-10-16" {
		t.Errorf("from 01:00 the calendar should start on the 16th, got %s", snap.Today)
	}
}
