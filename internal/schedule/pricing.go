package schedule

import (
	"fmt"
	"strconv"
	"time"
)

// The member discount. A member is a customer on the list the admin keeps (see
// the handlers package). In each calendar month a member pays the standard price
// for their first three 8-a-side 2 hour sessions and gets 80% off the fourth:
// Ksh 10,000, Ksh 10,000, Ksh 10,000 and then Ksh 2,000. This file is only the
// rule, so what it covers and what it costs live in one place beside the
// standard prices.
const (
	// MemberDiscountPercent is taken off the price of the session the discount
	// is given on.
	MemberDiscountPercent = 80

	// MemberDiscountSession is the session of the month the discount is given on:
	// the fourth, after three at the standard price. Only 8-a-side 2 hour
	// sessions that have been paid for count towards it, and a session counts
	// towards the month its date is in, which is the operating day it starts on
	// (see the package comment).
	MemberDiscountSession = 4
)

// MemberEligible reports whether the member discount can apply to a session:
// only the 8-a-side 2 hour session, which is the one that costs Ksh 10,000.
func MemberEligible(pitchType string, minutes int) bool {
	return pitchType == Pitch8Aside && minutes == 120
}

// MemberPriceKES is the price in whole shillings a member pays for the session
// the discount is given on, and false for a session the discount does not
// cover.
func MemberPriceKES(pitchType string, minutes int) (int, bool) {
	if !MemberEligible(pitchType, minutes) {
		return 0, false
	}
	list, ok := PriceKES(pitchType, minutes)
	if !ok {
		return 0, false
	}
	return list - list*MemberDiscountPercent/100, true
}

// MonthOf is the month a session date counts towards, as "YYYY-MM". date must
// be a valid YYYY-MM-DD, which ParseSession guarantees.
func MonthOf(date string) string {
	if len(date) < len("2006-01") {
		return ""
	}
	return date[:len("2006-01")]
}

// MonthBounds returns the first date of a month and of the month after it, so
// that a booking is in the month when from <= its date < to. Dates are ISO
// strings, which compare in date order.
func MonthBounds(month string) (from, to string, err error) {
	first, err := time.ParseInLocation("2006-01", month, location)
	if err != nil || first.Format("2006-01") != month {
		return "", "", fmt.Errorf("invalid month %q", month)
	}
	return first.Format("2006-01-02"), first.AddDate(0, 1, 0).Format("2006-01-02"), nil
}

// MonthName is a month as a customer reads it ("November").
func MonthName(month string) string {
	first, err := time.ParseInLocation("2006-01", month, location)
	if err != nil {
		return month
	}
	return first.Month().String()
}

// FormatKES writes an amount of shillings the way the site does: "Ksh 8,000".
func FormatKES(amount int) string {
	digits := strconv.Itoa(amount)
	negative := false
	if amount < 0 {
		negative, digits = true, digits[1:]
	}
	var out []byte
	for i := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, digits[i])
	}
	if negative {
		return "Ksh -" + string(out)
	}
	return "Ksh " + string(out)
}
