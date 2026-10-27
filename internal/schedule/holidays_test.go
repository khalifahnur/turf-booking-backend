package schedule

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// withExtraHolidays sets the configured holidays for one test.
func withExtraHolidays(t *testing.T, extra map[string]string) {
	t.Helper()
	SetExtraHolidays(extra)
	t.Cleanup(func() { SetExtraHolidays(nil) })
}

func TestEasterSunday(t *testing.T) {
	for year, want := range map[int]string{
		2000: "2000-04-23", 2019: "2019-04-21", 2024: "2024-03-31", 2025: "2025-04-20",
		2026: "2026-04-05", 2027: "2027-03-28", 2028: "2028-04-16", 2029: "2029-04-01",
		2030: "2030-04-21", 2038: "2038-04-25",
	} {
		if got := easterSunday(year).Format("2006-01-02"); got != want {
			t.Errorf("Easter %d = %s, want %s", year, got, want)
		}
	}
}

func holidaysOf(year int) map[string]string { return statutoryHolidays(year) }

// 2026, the year this was written for: nothing falls on a Sunday, so there is
// no shifting, and the three December holidays include two Saturdays.
func TestStatutoryHolidays2026(t *testing.T) {
	want := map[string]string{
		"2026-01-01": "New Year's Day",
		"2026-04-03": "Good Friday",
		"2026-04-06": "Easter Monday",
		"2026-05-01": "Labour Day",
		"2026-06-01": "Madaraka Day",
		"2026-10-10": "Mazingira Day",
		"2026-10-20": "Mashujaa Day",
		"2026-12-12": "Jamhuri Day",
		"2026-12-25": "Christmas Day",
		"2026-12-26": "Boxing Day",
	}
	if got := holidaysOf(2026); !reflect.DeepEqual(got, want) {
		t.Errorf("2026 holidays = %v\nwant %v", got, want)
	}
}

// Section 4 of the Public Holidays Act, with the occasions it is known to have
// been applied.
func TestSundayHolidaysMoveToTheNextDayThatIsFree(t *testing.T) {
	for _, c := range []struct {
		date, name string
		note       string
	}{
		{"2014-06-02", "Madaraka Day (observed)", "Madaraka Day was a Sunday in 2014"},
		{"2021-10-11", "Mazingira Day (observed)", "10 October 2021 was a Sunday"},
		{"2021-12-13", "Jamhuri Day (observed)", "12 December 2021 was a Sunday"},
		{"2021-12-27", "Boxing Day (observed)", "Boxing Day 2021 was a Sunday"},
		{"2022-05-02", "Labour Day (observed)", "1 May 2022 was a Sunday"},
		{"2022-12-27", "Christmas Day (observed)", "Christmas 2022 was a Sunday and Boxing Day the Monday, so the Tuesday"},
		{"2023-01-02", "New Year's Day (observed)", "1 January 2023 was a Sunday"},
		{"2024-10-21", "Mashujaa Day (observed)", "20 October 2024 was a Sunday"},
		{"2025-06-02", "Madaraka Day (observed)", "1 June 2025 was a Sunday"},
		{"2027-10-11", "Mazingira Day (observed)", "10 October 2027 is a Sunday"},
		{"2027-12-13", "Jamhuri Day (observed)", "12 December 2027 is a Sunday"},
		{"2027-12-27", "Boxing Day (observed)", "Boxing Day 2027 is a Sunday"},
	} {
		year, err := strconv.Atoi(c.date[:4])
		if err != nil {
			t.Fatal(err)
		}
		got := holidaysOf(year)
		if got[c.date] != c.name {
			t.Errorf("%s: %s should be %q, got %q", c.note, c.date, c.name, got[c.date])
		}
	}

	// The Sunday itself ceases to be a public holiday, so it is just a Sunday.
	for _, sunday := range []string{"2025-06-01", "2024-10-20", "2022-12-25", "2027-10-10"} {
		kind, name, err := KindOf(sunday)
		if err != nil || kind != KindWeekend || name != "" {
			t.Errorf("%s = %s %q (%v), want an ordinary Sunday", sunday, kind, name, err)
		}
	}
	// A Saturday is not moved.
	for _, saturday := range []string{"2026-10-10", "2026-12-12", "2026-12-26"} {
		kind, name, _ := KindOf(saturday)
		if kind != KindHoliday || name == "" {
			t.Errorf("%s = %s %q, want a holiday on the Saturday itself", saturday, kind, name)
		}
	}
	if kind, _, _ := KindOf("2026-10-12"); kind != KindWeekday {
		t.Errorf("the Monday after a Saturday holiday stays a weekday, got %s", kind)
	}
}

// Whatever year it is, there are ten holidays under the Act, none of them
// falls on a Sunday, and Easter Sunday itself is not one.
func TestStatutoryHolidaysEveryYear(t *testing.T) {
	for year := 2000; year <= 2100; year++ {
		days := holidaysOf(year)
		if len(days) != 10 {
			t.Errorf("%d has %d holidays: %v", year, len(days), days)
		}
		for date, name := range days {
			day, ok := parseDate(date)
			if !ok || day.Year() != year {
				t.Errorf("%d: bad holiday date %q (%s)", year, date, name)
				continue
			}
			if day.Weekday() == time.Sunday {
				t.Errorf("%s (%s) is a Sunday", date, name)
			}
		}
		if _, ok := days[easterSunday(year).Format("2006-01-02")]; ok {
			t.Errorf("%d: Easter Sunday is listed", year)
		}
		easter := easterSunday(year)
		if days[easter.AddDate(0, 0, -2).Format("2006-01-02")] != "Good Friday" || days[easter.AddDate(0, 0, 1).Format("2006-01-02")] != "Easter Monday" {
			t.Errorf("%d: Good Friday or Easter Monday missing", year)
		}
	}
}

// The Idd holidays are gazetted, not calculated: those announced are in the
// calendar, and the days around them are not.
func TestDeclaredHolidays(t *testing.T) {
	for date, name := range map[string]string{"2026-03-20": "Eid-ul-Fitr", "2026-05-27": "Eid-ul-Adha"} {
		kind, got, err := KindOf(date)
		if err != nil || kind != KindHoliday || got != name {
			t.Errorf("%s = %s %q (%v), want a %s holiday", date, kind, got, err, name)
		}
	}
	if kind, _, _ := KindOf("2026-03-19"); kind != KindWeekday {
		t.Errorf("the day before Eid is an ordinary day, got %s", kind)
	}
	// A year whose Eid has not been announced has none: it fails towards the
	// shorter weekday hours rather than guessing.
	if kind, _, _ := KindOf("2027-03-10"); kind != KindWeekday {
		t.Errorf("an unannounced Eid must not be a holiday yet, got %s", kind)
	}
}

func TestConfiguredHolidays(t *testing.T) {
	// 2027-03-10 is an ordinary Wednesday until Eid is added.
	if kind, _, _ := KindOf("2027-03-10"); kind != KindWeekday {
		t.Fatalf("precondition: %s", kind)
	}
	withExtraHolidays(t, map[string]string{
		"2027-03-10": "Eid-ul-Fitr",
		"2026-12-25": "Christmas (renamed)",
	})
	kind, name, _ := KindOf("2027-03-10")
	if kind != KindHoliday || name != "Eid-ul-Fitr" {
		t.Errorf("configured holiday = %s %q", kind, name)
	}
	if _, name, _ := KindOf("2026-12-25"); name != "Christmas (renamed)" {
		t.Errorf("a configured name should win over the built-in one, got %q", name)
	}
	// And a configured day takes part in the Sunday rule: with the Monday after
	// 10 October 2027 (a Sunday) already a holiday, the Tuesday is the one.
	withExtraHolidays(t, map[string]string{"2027-10-11": "Special day"})
	if _, name, _ := KindOf("2027-10-12"); name != "Mazingira Day (observed)" {
		t.Errorf("2027-10-12 = %q", name)
	}
	if _, name, _ := KindOf("2027-10-11"); name != "Special day" {
		t.Errorf("2027-10-11 = %q", name)
	}

	SetExtraHolidays(nil)
	if kind, _, _ := KindOf("2027-03-10"); kind != KindWeekday {
		t.Errorf("clearing the configured holidays must remove them, got %s", kind)
	}
}

func TestHolidaysBetweenAndLastDeclared(t *testing.T) {
	got := HolidaysBetween(at("2026-10-27", 12, 0), at("2027-01-02", 0, 0))
	want := []Holiday{
		{"2026-12-12", "Jamhuri Day"},
		{"2026-12-25", "Christmas Day"},
		{"2026-12-26", "Boxing Day"},
		{"2027-01-01", "New Year's Day"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("holidays = %v\nwant %v", got, want)
	}
	if got := HolidaysBetween(at("2026-10-27", 0, 0), at("2026-10-27", 0, 0)); len(got) != 0 {
		t.Errorf("a single ordinary day: %v", got)
	}
	if got := HolidaysBetween(at("2026-12-25", 23, 59), at("2026-12-25", 0, 0)); len(got) != 1 {
		t.Errorf("one holiday day, either end of it: %v", got)
	}

	if got := LastDeclared(); got != "2026-05-27" {
		t.Errorf("LastDeclared = %q", got)
	}
	withExtraHolidays(t, map[string]string{"2027-05-16": "Eid-ul-Adha"})
	if got := LastDeclared(); got != "2027-05-16" {
		t.Errorf("LastDeclared with extras = %q", got)
	}
}

func TestParseHolidays(t *testing.T) {
	got, problems := ParseHolidays(" 2027-03-09=Eid-ul-Fitr , 2027-05-16 ,, 2027-12-31=New Year's Eve (observed) ")
	want := map[string]string{
		"2027-03-09": "Eid-ul-Fitr",
		"2027-05-16": "Public holiday",
		"2027-12-31": "New Year's Eve (observed)",
	}
	if !reflect.DeepEqual(got, want) || len(problems) != 0 {
		t.Errorf("got %v, problems %v", got, problems)
	}

	got, problems = ParseHolidays("2027-3-9=Eid, 2027-02-30, tomorrow, 2027-04-01=<b>x</b>, 2027-04-02=ok, 2027-04-03=" + strings.Repeat("n", 61) + ", 2027-04-04")
	wantKept := []string{"2027-04-02", "2027-04-04"}
	var kept []string
	for date := range got {
		kept = append(kept, date)
	}
	sort.Strings(kept)
	if !reflect.DeepEqual(kept, wantKept) {
		t.Errorf("kept %v, want %v", kept, wantKept)
	}
	if len(problems) != 5 {
		t.Errorf("want 5 problems, got %d: %v", len(problems), problems)
	}

	if got, problems := ParseHolidays(""); len(got) != 0 || len(problems) != 0 {
		t.Errorf("empty setting: %v %v", got, problems)
	}
}
