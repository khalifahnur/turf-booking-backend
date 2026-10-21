package camp

import (
	"strings"
	"testing"
	"time"

	"turf-booking-backend/internal/schedule"
)

// Wednesday 21 Oct 2026, before the camp starts.
var before = time.Date(2026, 10, 21, 12, 0, 0, 0, schedule.Location())

func TestCampCalendar(t *testing.T) {
	weeks := Weeks()
	if len(weeks) != 7 {
		t.Fatalf("camp 2 Nov - 18 Dec is 7 weeks, got %d", len(weeks))
	}
	if weeks[0].Start != "2026-11-02" || weeks[0].End != "2026-11-06" {
		t.Errorf("week 1 = %+v", weeks[0])
	}
	if weeks[6].Start != "2026-12-14" || weeks[6].End != "2026-12-18" {
		t.Errorf("week 7 = %+v", weeks[6])
	}
	for _, w := range weeks {
		start, _ := time.ParseInLocation("2006-01-02", w.Start, schedule.Location())
		end, _ := time.ParseInLocation("2006-01-02", w.End, schedule.Location())
		if start.Weekday() != time.Monday || end.Weekday() != time.Friday {
			t.Errorf("week %d is %s-%s, want Monday-Friday", w.Index, start.Weekday(), end.Weekday())
		}
	}
}

func TestRangeLabel(t *testing.T) {
	w := Weeks()
	if got := RangeLabel(w[0]); got != "2-6 Nov" {
		t.Errorf("week 1 label = %q", got)
	}
	if got := RangeLabel(w[4]); got != "30 Nov - 4 Dec" { // spans two months
		t.Errorf("week 5 label = %q", got)
	}
}

func TestQuotePricing(t *testing.T) {
	cases := []struct {
		name    string
		program string
		lunch   bool
		weeks   []string
		total   int
		perWeek int
	}{
		{"full day, 1 week", "fullDay", false, []string{"2026-11-02"}, 2_500, 2_500},
		{"half day, 1 week", "halfDay", false, []string{"2026-11-02"}, 1_500, 1_500},
		{"full day + lunch, 1 week", "fullDay", true, []string{"2026-11-02"}, 3_000, 3_000},
		{"half day + lunch, 1 week", "halfDay", true, []string{"2026-11-02"}, 2_000, 2_000},
		{"full day, 2 weeks", "fullDay", false, []string{"2026-11-02", "2026-11-09"}, 5_000, 2_500},
		{"full day + lunch, 3 weeks", "fullDay", true, []string{"2026-11-02", "2026-11-09", "2026-11-16"}, 9_000, 3_000},
		{"half day, whole camp", "halfDay", false, []string{"2026-11-02", "2026-11-09", "2026-11-16", "2026-11-23", "2026-11-30", "2026-12-07", "2026-12-14"}, 10_500, 1_500},
		{"full day + lunch, whole camp", "fullDay", true, []string{"2026-11-02", "2026-11-09", "2026-11-16", "2026-11-23", "2026-11-30", "2026-12-07", "2026-12-14"}, 21_000, 3_000},
	}
	for _, c := range cases {
		q, err := NewQuote(c.program, c.lunch, c.weeks, before)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if q.TotalKES != c.total || q.PerWeekKES != c.perWeek {
			t.Errorf("%s: total=%d perWeek=%d, want %d/%d", c.name, q.TotalKES, q.PerWeekKES, c.total, c.perWeek)
		}
	}
}

func TestQuoteLunchIsPerDay(t *testing.T) {
	q, err := NewQuote("halfDay", true, []string{"2026-11-02"}, before)
	if err != nil {
		t.Fatal(err)
	}
	if q.LunchWeekKES != 500 {
		t.Errorf("lunch for one week = %d, want 5 days x Ksh 100 = 500", q.LunchWeekKES)
	}
}

func TestQuoteNormalisesWeeks(t *testing.T) {
	q, err := NewQuote("fullDay", false, []string{"2026-11-16", "2026-11-02", "2026-11-16", "2026-11-02"}, before)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(q.Weeks, ","); got != "2026-11-02,2026-11-16" {
		t.Errorf("weeks = %s, want sorted and de-duplicated", got)
	}
	if q.TotalKES != 5_000 {
		t.Errorf("a repeated week must not be charged twice: total = %d, want 5000", q.TotalKES)
	}
}

func TestQuoteRejectsBadInput(t *testing.T) {
	cases := []struct {
		name    string
		program string
		weeks   []string
	}{
		{"unknown programme", "overnight", []string{"2026-11-02"}},
		{"empty programme", "", []string{"2026-11-02"}},
		{"no weeks", "fullDay", nil},
		{"empty weeks", "fullDay", []string{}},
		{"a Tuesday, not a week start", "fullDay", []string{"2026-11-03"}},
		{"before the camp", "fullDay", []string{"2026-10-26"}},
		{"after the camp", "fullDay", []string{"2026-12-21"}},
		{"not a date", "fullDay", []string{"next week"}},
		{"injection attempt", "fullDay", []string{`{"$ne":""}`}},
		{"one good one bad", "fullDay", []string{"2026-11-02", "2026-11-04"}},
		{"too many entries", "fullDay", strings.Split(strings.Repeat("2026-11-02,", 11), ",")},
	}
	for _, c := range cases {
		if _, err := NewQuote(c.program, false, c.weeks, before); err == nil {
			t.Errorf("%s: should have been rejected", c.name)
		}
	}
}

func TestQuoteClosesWeeksThatHaveEnded(t *testing.T) {
	// Saturday 7 Nov: week 1 (2-6 Nov) is over, week 2 starts Monday.
	sat := time.Date(2026, 11, 7, 9, 0, 0, 0, schedule.Location())
	if _, err := NewQuote("fullDay", false, []string{"2026-11-02"}, sat); err == nil {
		t.Error("a week that has already ended must not be sold")
	}
	if _, err := NewQuote("fullDay", false, []string{"2026-11-09"}, sat); err != nil {
		t.Errorf("the next week is still open: %v", err)
	}
	// Wednesday of week 1: still bookable (a late joiner), through to Friday.
	wed := time.Date(2026, 11, 4, 9, 0, 0, 0, schedule.Location())
	if _, err := NewQuote("fullDay", false, []string{"2026-11-02"}, wed); err != nil {
		t.Errorf("a week in progress stays open to late joiners: %v", err)
	}
	fri := time.Date(2026, 11, 6, 23, 0, 0, 0, schedule.Location())
	if _, err := NewQuote("fullDay", false, []string{"2026-11-02"}, fri); err != nil {
		t.Errorf("the last day of a week is still open: %v", err)
	}
}

func TestQuoteUsesVenueTimezone(t *testing.T) {
	// 21:30 UTC on Fri 6 Nov is 00:30 Sat 7 Nov in Nairobi: week 1 has ended.
	lateUTC := time.Date(2026, 11, 6, 21, 30, 0, 0, time.UTC)
	if _, err := NewQuote("fullDay", false, []string{"2026-11-02"}, lateUTC); err == nil {
		t.Error("week 1 is over once it is Saturday in Nairobi")
	}
}

func TestReferenceAndLabels(t *testing.T) {
	if !IsReference("camp-0123456789abcdef") || IsReference("0123456789abcdef01234567") || IsReference("") {
		t.Error("IsReference must only match camp- references")
	}
	if got := WeeksLabel([]string{"2026-11-02", "2026-11-09"}); got != "w/c 2 Nov, 9 Nov" {
		t.Errorf("WeeksLabel = %q", got)
	}
	if got := WeeksLabel([]string{"2026-11-02", "2026-11-09", "2026-11-16", "2026-11-23"}); got != "4 weeks" {
		t.Errorf("WeeksLabel for 4 = %q", got)
	}
}
