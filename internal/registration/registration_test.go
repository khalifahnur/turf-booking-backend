package registration

import (
	"net/mail"
	"reflect"
	"strings"
	"testing"
	"time"

	"turf-booking-backend/internal/schedule"
)

func TestFeesAndPlacesAreWhatTheAcademyAnnounced(t *testing.T) {
	// If any of these change, the rules page and the SMS wording must change too.
	if TournamentFeePerCategoryKES != 2_000 || AcademyFeeKES != 5_000 {
		t.Errorf("fees %d/%d", TournamentFeePerCategoryKES, AcademyFeeKES)
	}
	type shape struct {
		id, group      string
		capacity, open int
	}
	var got []shape
	for _, c := range Categories() {
		got = append(got, shape{c.ID, c.Group, c.Capacity, c.Open()})
	}
	// Youth: 4 teams in each of U10 and U15, one place in each already taken by
	// St. Sebastian Sports Academy, so 3 are open. Adults: 6 teams, one of which
	// has booked directly, so 5 are open.
	want := []shape{{"u10", "youth", 4, 3}, {"u15", "youth", 4, 3}, {"adults", "adults", 6, 5}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("categories = %+v, want %+v", got, want)
	}
	if u10, _ := LookupCategory("u10"); u10.Reserved != 1 {
		t.Errorf("the academy holds one place in U10, got %d", u10.Reserved)
	}
	if adults, _ := LookupCategory("adults"); adults.Reserved != 1 {
		t.Errorf("one adults team has booked directly, got %d reserved", adults.Reserved)
	}
}

func TestNewEntry(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"one category", []string{"u15"}, []string{"u15"}},
		{"both youth, in any order", []string{"u15", "u10"}, []string{"u10", "u15"}},
		{"youth and adults", []string{"adults", "u10"}, []string{"u10", "adults"}},
		{"adults only", []string{"adults"}, []string{"adults"}},
		{"repeats collapse to one place", []string{"u10", "u10", "u15", "u10"}, []string{"u10", "u15"}},
		{"all three", []string{"adults", "u15", "u10"}, []string{"u10", "u15", "adults"}},
	}
	for _, c := range cases {
		got, err := NewEntry(c.in)
		if err != nil || !reflect.DeepEqual(got.Categories, c.want) {
			t.Errorf("%s: NewEntry(%v) = %v, %v; want %v", c.name, c.in, got.Categories, err, c.want)
		}
		// Ksh 2,000 for each category the registration covers, and a repeated one is not charged twice.
		if err == nil && got.FeeKES != 2_000*len(c.want) {
			t.Errorf("%s: fee = %d, want Ksh %d (2,000 for each of %d categories)", c.name, got.FeeKES, 2_000*len(c.want), len(c.want))
		}
	}
	for name, in := range map[string][]string{"none": nil, "empty": {}, "unknown": {"u12"}, "singular": {"adult"}, "one good one bad": {"u10", "senior"}, "wrong case": {"U10"}} {
		if got, err := NewEntry(in); err == nil {
			t.Errorf("%s: NewEntry(%v) = %v, want an error", name, in, got)
		}
	}
}

func TestValidatePlayers(t *testing.T) {
	for n, ok := range map[int]bool{-1: false, 0: false, 4: false, 5: true, 18: true, 40: true, 41: false, 1_000_000: false} {
		if err := ValidatePlayers(n); (err == nil) != ok {
			t.Errorf("ValidatePlayers(%d) = %v, want ok=%v", n, err, ok)
		}
	}
	if err := ValidatePlayers(2); err == nil || !strings.Contains(err.Error(), "between 5 and 40") {
		t.Errorf("the message must say the limits: %v", err)
	}
}

func TestPlacesLeft(t *testing.T) {
	for _, c := range []struct {
		id         string
		paid, held int
		left       int
	}{
		{"u10", 0, 0, 3}, {"u10", 1, 1, 1}, {"u10", 2, 0, 1}, {"u10", 2, 1, 0}, {"u10", 3, 0, 0}, {"u10", 5, 5, 0},
		{"u15", 0, 0, 3}, {"u15", 0, 3, 0},
		{"adults", 0, 0, 5}, {"adults", 3, 2, 0}, {"adults", 4, 0, 1}, {"adults", 5, 1, 0},
		{"u12", 0, 0, 0}, // not a category: nothing to take
	} {
		s := Slots{Paid: c.paid, Held: c.held}
		if got := Left(c.id, s); got != c.left || Full(c.id, s) != (c.left == 0) {
			t.Errorf("%s %+v: left %d full %v, want %d", c.id, s, got, Full(c.id, s), c.left)
		}
	}
	got := Unavailable([]string{"u10", "u15", "adults"}, map[string]Slots{"u10": {Paid: 3}, "u15": {Paid: 1}, "adults": {Paid: 6}})
	if !reflect.DeepEqual(got, []string{"U10", "Adults"}) {
		t.Errorf("Unavailable = %v, want [U10 Adults]", got)
	}
	if got := Unavailable([]string{"u10", "u15"}, nil); len(got) != 0 {
		t.Errorf("with nobody registered nothing is unavailable, got %v", got)
	}
}

func TestOverCapacity(t *testing.T) {
	// Three other teams have paid in U10 (all the places the website has), two in
	// U15 and four in the adults (one place short of the five it has): this payment
	// is one too many only for U10.
	got := OverCapacity([]string{"u10", "u15", "adults"}, map[string]int{"u10": 3, "u15": 2, "adults": 4})
	if !reflect.DeepEqual(got, []string{"u10"}) {
		t.Errorf("OverCapacity = %v, want [u10]", got)
	}
	if got := OverCapacity([]string{"adults"}, map[string]int{"adults": 5}); !reflect.DeepEqual(got, []string{"adults"}) {
		t.Errorf("a sixth paying adult team is over capacity (one place is taken directly), got %v", got)
	}
	if got := OverCapacity([]string{"u10"}, map[string]int{"u10": 2}); len(got) != 0 {
		t.Errorf("the third paying youth team is within capacity, got %v", got)
	}
	if got := OverCapacity([]string{"u12"}, map[string]int{"u12": 99}); len(got) != 0 {
		t.Errorf("an unknown category is not flagged: %v", got)
	}
}

func TestLabels(t *testing.T) {
	for in, want := range map[string]string{"": "", "u10": "U10", "u10,u15": "U10 and U15", "u10,adults": "U10 and Adults", "u10,u15,u12": "U10, U15 and U12"} {
		var ids []string
		if in != "" {
			ids = strings.Split(in, ",")
		}
		if got := Labels(ids); got != want {
			t.Errorf("Labels(%v) = %q, want %q", ids, got, want)
		}
	}
}

func TestParseBirthDate(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, schedule.Location())
	good := []string{
		"2020-11-25", // turned 6 today
		"2023-11-25", // exactly 3
		"2005-11-26", // 20, turns 21 tomorrow
		"2015-01-01",
	}
	for _, in := range good {
		if got, err := ParseBirthDate(in, now); err != nil || got != in {
			t.Errorf("ParseBirthDate(%q) = %q, %v; want it accepted", in, got, err)
		}
	}
	bad := map[string]string{
		"2023-11-26": "between 3 and 21", // a day short of 3
		"2026-11-26": "future",
		"1999-01-01": "between 3 and 21",
		"not a date": "date of birth",
		"":           "date of birth",
		"2015-02-30": "date of birth", // a day that does not exist
		"15/01/2015": "date of birth",
		"2015-1-5":   "date of birth",
	}
	for in, want := range bad {
		if _, err := ParseBirthDate(in, now); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ParseBirthDate(%q) error = %v, want one containing %q", in, err, want)
		}
	}
	if _, err := ParseBirthDate("2005-11-25", now); err != nil {
		t.Errorf("a 21 year old is within the limit: %v", err)
	}
	if _, err := ParseBirthDate("2004-11-25", now); err == nil {
		t.Error("a 22 year old is over the limit")
	}
}

func TestReferences(t *testing.T) {
	for ref, want := range map[string]bool{
		"tourney-" + strings.Repeat("a", 32): true,
		"academy-" + strings.Repeat("a", 32): true,
		"camp-" + strings.Repeat("a", 32):    false, // the camp has its own
		strings.Repeat("a", 32):              false, // a pitch booking
		"":                                   false,
	} {
		if IsReference(ref) != want {
			t.Errorf("IsReference(%q) = %v, want %v", ref, !want, want)
		}
	}
	if got := TrimReferencePrefix("tourney-abc"); got != "abc" {
		t.Errorf("TrimReferencePrefix = %q", got)
	}
	if got := TrimReferencePrefix("academy-abc"); got != "abc" {
		t.Errorf("TrimReferencePrefix = %q", got)
	}
	if got := TrimReferencePrefix("abc"); got != "abc" {
		t.Errorf("TrimReferencePrefix = %q", got)
	}
}

func TestPayerEmailIsAValidAddressThatIsNotAnybodysInbox(t *testing.T) {
	got := PayerEmail("254712345678")
	addr, err := mail.ParseAddress(got)
	if err != nil || addr.Address != got {
		t.Fatalf("PayerEmail = %q, which Paystack would refuse: %v", got, err)
	}
	if !strings.HasSuffix(got, "@pay.karena.stsebastiansportsacademy.com") {
		t.Errorf("PayerEmail = %q, want an address on the academy's own domain", got)
	}
}

func TestSortedIDs(t *testing.T) {
	if got := SortedIDs([]string{"u15", "u10"}); !reflect.DeepEqual(got, []string{"u10", "u15"}) {
		t.Errorf("SortedIDs = %v", got)
	}
}
