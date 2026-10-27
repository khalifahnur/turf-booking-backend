package schedule

import "testing"

// The member discount covers the 8-a-side 2 hour session and nothing else: that
// is the Ksh 10,000 session the owner named, which becomes Ksh 8,000.
func TestMemberEligibleIsOnlyTheEightASideTwoHourSession(t *testing.T) {
	for _, pitch := range []string{Pitch5Aside, Pitch8Aside, "", "11Aside", "8aside"} {
		for _, minutes := range []int{0, 30, 60, 90, 120, 180, 240} {
			want := pitch == Pitch8Aside && minutes == 120
			if got := MemberEligible(pitch, minutes); got != want {
				t.Errorf("MemberEligible(%q, %d) = %v, want %v", pitch, minutes, got, want)
			}
		}
	}
}

func TestMemberPrice(t *testing.T) {
	// 80% off Ksh 10,000 leaves Ksh 2,000.
	if got, ok := MemberPriceKES(Pitch8Aside, 120); !ok || got != 2_000 {
		t.Errorf("8-a-side 2 hr member price = %d, %v; want 2000", got, ok)
	}
	for _, c := range []struct {
		pitch   string
		minutes int
	}{
		{Pitch8Aside, 60}, {Pitch5Aside, 60}, {Pitch5Aside, 120}, {"nope", 120}, {Pitch8Aside, 90},
	} {
		if got, ok := MemberPriceKES(c.pitch, c.minutes); ok || got != 0 {
			t.Errorf("MemberPriceKES(%q, %d) = %d, %v; the discount does not cover it", c.pitch, c.minutes, got, ok)
		}
	}
}

// The rule is 80% off the fourth session, to the shilling: Ksh 10,000, 10,000,
// 10,000 and then Ksh 2,000, which is the same Ksh 32,000 for the month as four
// sessions at Ksh 8,000. If a price is ever changed so that 80% of it is not a
// whole number of shillings this fails rather than quietly rounding someone's bill.
func TestMemberPriceIsExactlyTheDiscountedListPrice(t *testing.T) {
	if MemberDiscountPercent != 80 || MemberDiscountSession != 4 {
		t.Fatalf("the member rule is 80%% off the 4th session; got %d%%, session %d", MemberDiscountPercent, MemberDiscountSession)
	}
	for pitch := range pricesKES {
		for minutes := range pricesKES[pitch] {
			if !MemberEligible(pitch, minutes) {
				continue
			}
			list, _ := PriceKES(pitch, minutes)
			if list*MemberDiscountPercent%100 != 0 {
				t.Errorf("%d%% of %d is not a whole number of shillings", MemberDiscountPercent, list)
			}
			got, ok := MemberPriceKES(pitch, minutes)
			if !ok || got != list*(100-MemberDiscountPercent)/100 || got >= list || got <= 0 {
				t.Errorf("member price of %s/%d = %d, %v (list %d)", pitch, minutes, got, ok, list)
			}
			if month := 3*list + got; month != 32_000 {
				t.Errorf("three sessions and the discounted fourth cost %d, want Ksh 32,000", month)
			}
		}
	}
}

func TestMonthOfAndBounds(t *testing.T) {
	if got := MonthOf("2026-11-07"); got != "2026-11" {
		t.Errorf("MonthOf = %q", got)
	}
	if got := MonthOf("bad"); got != "" {
		t.Errorf("MonthOf of a short string = %q", got)
	}
	for month, want := range map[string][2]string{
		"2026-11": {"2026-11-01", "2026-12-01"},
		"2026-12": {"2026-12-01", "2027-01-01"}, // the year turns over
		"2028-02": {"2028-02-01", "2028-03-01"}, // a leap year
		"2027-01": {"2027-01-01", "2027-02-01"},
	} {
		from, to, err := MonthBounds(month)
		if err != nil || from != want[0] || to != want[1] {
			t.Errorf("MonthBounds(%q) = %q, %q, %v; want %v", month, from, to, err, want)
		}
	}
	for _, bad := range []string{"", "2026", "2026-13", "2026-00", "2026-1", "26-11", "2026-11-01", "abcd-ef"} {
		if _, _, err := MonthBounds(bad); err == nil {
			t.Errorf("MonthBounds(%q) should be an error", bad)
		}
	}

	// Every date lies in exactly the month MonthOf says, by plain string order.
	for _, date := range []string{"2026-11-01", "2026-11-30", "2026-12-01", "2026-12-31", "2027-01-01", "2028-02-29"} {
		from, to, err := MonthBounds(MonthOf(date))
		if err != nil || !(from <= date && date < to) {
			t.Errorf("%s is not within its own month %s..%s (%v)", date, from, to, err)
		}
	}
	// And the day after a month ends is not in it.
	if _, to, _ := MonthBounds("2026-11"); "2026-12-01" < to {
		t.Error("1 December must be outside November")
	}
}

func TestMonthName(t *testing.T) {
	if got := MonthName("2026-11"); got != "November" {
		t.Errorf("MonthName = %q", got)
	}
	if got := MonthName("nonsense"); got != "nonsense" {
		t.Errorf("an unreadable month is returned as it is, got %q", got)
	}
}

func TestFormatKES(t *testing.T) {
	for amount, want := range map[int]string{
		0: "Ksh 0", 5: "Ksh 5", 800: "Ksh 800", 8_000: "Ksh 8,000", 10_000: "Ksh 10,000",
		123_456: "Ksh 123,456", 1_234_567: "Ksh 1,234,567", -4_800: "Ksh -4,800",
	} {
		if got := FormatKES(amount); got != want {
			t.Errorf("FormatKES(%d) = %q, want %q", amount, got, want)
		}
	}
}
