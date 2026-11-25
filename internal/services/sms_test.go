package services

import (
	"strings"
	"testing"
)

func TestCampConfirmationMessage(t *testing.T) {
	got := campConfirmationMessage("Amani Otieno", "Full day 8:30am - 4:00pm + lunch", "2 Nov - 13 Nov", 6000)
	want := "Confirmed! Amani Otieno is registered for the K-Arena Holiday Football Camp: Full day 8:30am - 4:00pm + lunch, 2 Nov - 13 Nov. Paid Ksh 6,000. Directions: " + directionsLink
	if got != want {
		t.Errorf("message = %q\nwant      %q", got, want)
	}
	// One SMS segment is 160 characters; a long name and several date ranges should
	// still fit in two.
	long := campConfirmationMessage(strings.Repeat("a", 60), "Half day 8:30am - 12:30pm + lunch", "2 Nov - 13 Nov, 23-27 Nov, 7-18 Dec", 12000)
	if len(long) > 320 {
		t.Errorf("worst-case message is %d characters", len(long))
	}
}

func TestTournamentConfirmationMessage(t *testing.T) {
	got := tournamentConfirmationMessage("Kibera Stars", "U10 and U15", 2000)
	want := "Confirmed! Kibera Stars is registered for the K-Arena Tournament: U10 and U15. Paid Ksh 2,000. Directions: " + directionsLink
	if got != want {
		t.Errorf("message = %q\nwant      %q", got, want)
	}
	if long := tournamentConfirmationMessage(strings.Repeat("a", 60), "U10 and U15", 2000); len(long) > 320 {
		t.Errorf("worst-case message is %d characters", len(long))
	}
}

func TestAcademyConfirmationMessage(t *testing.T) {
	got := academyConfirmationMessage("Amani Otieno", 5000)
	want := "Confirmed! Amani Otieno is registered at St. Sebastian Sports Academy. Paid Ksh 5,000. Thank you."
	if got != want {
		t.Errorf("message = %q\nwant      %q", got, want)
	}
}
