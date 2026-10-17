package schedule

import (
	"strings"
	"testing"
	"time"
)

// A fixed "now": Wednesday 2026-10-14, 12:00 in Nairobi.
var now = time.Date(2026, 10, 14, 12, 0, 0, 0, Location())

func TestParseSessionAcceptsOfferedSessions(t *testing.T) {
	cases := []struct{ date, rng string }{
		{"2026-10-14", "16:00 – 18:00"}, // weekday 2hr
		{"2026-10-14", "20:00 – 22:00"},
		{"2026-10-14", "16:00 – 17:00"}, // weekday 1hr
		{"2026-10-14", "21:00 – 22:00"},
		{"2026-10-17", "08:00 – 10:00"}, // Saturday 2hr
		{"2026-10-17", "08:00 – 09:00"}, // Saturday 1hr
		{"2026-10-18", "13:00 – 14:00"}, // Sunday 1hr
		{"2026-10-18", "12:00 – 14:00"},
		{"2026-10-14", "16:00 - 18:00"}, // ascii hyphen is canonicalised
		{"2026-10-14", "16:00-18:00"},
	}
	for _, c := range cases {
		s, err := ParseSession(c.date, c.rng, now)
		if err != nil {
			t.Errorf("%s %q: unexpected error: %v", c.date, c.rng, err)
			continue
		}
		if !strings.Contains(s.Label(), " – ") {
			t.Errorf("label %q is not canonical", s.Label())
		}
	}
}

func TestParseSessionRejectsOffGridAndAbusiveRanges(t *testing.T) {
	cases := []struct{ name, date, rng string }{
		{"all day", "2026-10-14", "00:00 – 23:59"},
		{"before weekday opening", "2026-10-14", "14:00 – 16:00"},
		{"weekday morning", "2026-10-14", "08:00 – 10:00"},
		{"after closing", "2026-10-14", "21:00 – 23:00"},
		{"misaligned 2hr", "2026-10-14", "17:00 – 19:00"},
		{"misaligned weekend 2hr", "2026-10-17", "09:00 – 11:00"},
		{"90 minutes", "2026-10-14", "16:00 – 17:30"},
		{"3 hours", "2026-10-14", "16:00 – 19:00"},
		{"reversed", "2026-10-14", "18:00 – 16:00"},
		{"empty", "2026-10-14", ""},
		{"garbage", "2026-10-14", "soon"},
		{"three parts", "2026-10-14", "16:00 – 17:00 – 18:00"},
		{"single digit hour", "2026-10-14", "6:00 – 8:00"},
		{"oversized", "2026-10-14", strings.Repeat("1", 500)},
		{"past date", "2026-10-13", "16:00 – 18:00"},
		{"too far ahead", "2026-12-25", "16:00 – 18:00"},
		{"bad date", "2026-13-40", "16:00 – 18:00"},
		{"unpadded date", "2026-1-5", "16:00 – 18:00"},
		{"date with junk", "2026-10-14'; drop", "16:00 – 18:00"},
	}
	for _, c := range cases {
		if _, err := ParseSession(c.date, c.rng, now); err == nil {
			t.Errorf("%s: %s %q should have been rejected", c.name, c.date, c.rng)
		}
	}
}

func TestParseSessionCutoff(t *testing.T) {
	// 2hr session 16:00-18:00 ends at 18:00.
	cases := []struct {
		at      time.Time
		allowed bool
	}{
		{time.Date(2026, 10, 14, 15, 0, 0, 0, Location()), true},
		{time.Date(2026, 10, 14, 17, 29, 0, 0, Location()), true},
		{time.Date(2026, 10, 14, 17, 30, 0, 0, Location()), true}, // exactly 30m left
		{time.Date(2026, 10, 14, 17, 31, 0, 0, Location()), false},
		{time.Date(2026, 10, 14, 19, 0, 0, 0, Location()), false},
	}
	for _, c := range cases {
		_, err := ParseSession("2026-10-14", "16:00 – 18:00", c.at)
		if (err == nil) != c.allowed {
			t.Errorf("at %s: allowed=%v, err=%v", c.at.Format("15:04"), c.allowed, err)
		}
	}
}

func TestParseSessionUsesVenueTimezone(t *testing.T) {
	// 22:00 UTC on the 14th is already 01:00 on the 15th in Nairobi, so the
	// 14th has passed.
	lateUTC := time.Date(2026, 10, 14, 22, 0, 0, 0, time.UTC)
	if _, err := ParseSession("2026-10-14", "16:00 – 18:00", lateUTC); err == nil {
		t.Error("expected the 14th to be closed once it is the 15th in Nairobi")
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
	}
	for _, c := range cases {
		if got := Fits(c.existing, c.start, c.end, c.units); got != c.want {
			t.Errorf("%s: Fits = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseStoredRange(t *testing.T) {
	for _, in := range []string{"16:00 – 18:00", "16:00 - 18:00", "16:00-18:00", "9:00 – 11:00"} {
		if _, _, ok := ParseStoredRange(in); !ok {
			t.Errorf("%q should parse", in)
		}
	}
	for _, in := range []string{"", "x", "18:00 – 16:00", "16:00"} {
		if _, _, ok := ParseStoredRange(in); ok {
			t.Errorf("%q should not parse", in)
		}
	}
}
