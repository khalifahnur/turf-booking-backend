// Package registration holds the business rules of the two things people pay
// the academy to join by M-Pesa: a place in the tournament and a place at the
// St. Sebastian Sports Academy. The browser only names who is registering and
// which categories a team wants; the fee, the number of places and who gets
// them are all decided here, so nothing a client sends can change what it is
// charged or take more than its share of places.
package registration

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"turf-booking-backend/internal/schedule"
)

// The two kinds of registration.
const (
	KindTournament = "tournament"
	KindAcademy    = "academy"
)

// Reference prefixes mark a Paystack reference as one of these payments, which is
// how the shared webhook and payment WebSocket tell them from pitch bookings and
// camp registrations.
const (
	TournamentReferencePrefix = "tourney-"
	AcademyReferencePrefix    = "academy-"
)

// Fees, in whole shillings.
const (
	// TournamentFeePerCategoryKES is charged for each category a team enters, so a
	// registration for U10 and U15 costs twice this and one for the Adults once.
	TournamentFeePerCategoryKES = 2_000
	AcademyFeeKES               = 5_000
)

const (
	// MinPlayers and MaxPlayers bound the squad size a team states. They are
	// sanity limits against typos, not the tournament's squad rules.
	MinPlayers = 5
	MaxPlayers = 40

	// RulesVersion identifies the tournament rules a team ticked. The form sends
	// the version it displayed and registration is refused if it is not this
	// one, so a stale browser tab cannot record agreement to wording that has
	// since changed. Bump it together with the rules in the frontend
	// (lib/tournament.ts) whenever they change.
	RulesVersion = "tournament-2026-v3"
)

// Statuses stored on a registration.
const (
	StatusPending = "Pending"
	StatusPaid    = "Paid"
	StatusFailed  = "Failed"
	// StatusExpired is a pending registration whose payment prompt went
	// unanswered. It is kept for a while so a payment that lands late can still
	// complete it, rather than taking the payer's money and giving nothing.
	StatusExpired = "Expired"
)

// HoldWindow is how long an unpaid tournament registration keeps its place. An
// M-Pesa prompt stays answerable for about a minute, so this is a little longer
// than that: after it the place goes back to whoever is next, and the team that
// let it lapse can register again straight away. A payment that still arrives
// later settles as usual, unless the place was given away meanwhile, in which
// case it is flagged for a refund (see OverCapacity).
const HoldWindow = 3 * time.Minute

// Category is a division of the tournament. Places in it go to whoever
// registers first. Capacity is how many teams it takes in all; Reserved is how
// many of those places are already taken by the academy's own teams, which are
// not sold on the website, so only Capacity - Reserved are open to everyone else.
type Category struct {
	ID       string
	Label    string
	Group    string // "youth" or "adults", for how the pages group them
	Capacity int
	Reserved int
}

// Open is how many places of the category the website can give out.
func (c Category) Open() int { return max(0, c.Capacity-c.Reserved) }

// The youth tournament takes 4 teams in each of U10 and U15 (St. Sebastian
// Sports Academy has one place in each already), and the adults take 6.
var categories = []Category{
	{ID: "u10", Label: "U10", Group: "youth", Capacity: 4, Reserved: 1},
	{ID: "u15", Label: "U15", Group: "youth", Capacity: 4, Reserved: 1},
	{ID: "adults", Label: "Adults", Group: "adults", Capacity: 6, Reserved: 0},
}

// Categories lists the age groups in the order they are shown.
func Categories() []Category { return append([]Category(nil), categories...) }

// LookupCategory finds a category by its id.
func LookupCategory(id string) (Category, bool) {
	for _, c := range categories {
		if c.ID == id {
			return c, true
		}
	}
	return Category{}, false
}

// Entry is a validated tournament registration, ready to be priced and placed.
type Entry struct {
	Categories []string // ids, in the order Categories lists them, no repeats
	FeeKES     int
}

// NewEntry checks the categories a team asked for and prices the registration.
// Every error is safe to show to the person registering.
func NewEntry(ids []string) (Entry, error) {
	if len(ids) == 0 {
		return Entry{}, errors.New("please choose at least one category")
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := LookupCategory(id); !ok {
			return Entry{}, errors.New("one of the chosen categories does not exist")
		}
		wanted[id] = struct{}{}
	}
	out := make([]string, 0, len(wanted))
	for _, c := range categories {
		if _, ok := wanted[c.ID]; ok {
			out = append(out, c.ID)
		}
	}
	return Entry{Categories: out, FeeKES: TournamentFeePerCategoryKES * len(out)}, nil
}

// ValidatePlayers checks the squad size a team stated.
func ValidatePlayers(n int) error {
	if n < MinPlayers || n > MaxPlayers {
		return fmt.Errorf("enter the total number of players, between %d and %d", MinPlayers, MaxPlayers)
	}
	return nil
}

// Slots is how many places of a category the website has given out.
type Slots struct {
	Paid int // registrations that have paid
	Held int // registrations whose prompt may still be answered
}

// Left is how many places of the category are still free for a new
// registration. An unknown category has none.
func Left(id string, s Slots) int {
	c, ok := LookupCategory(id)
	if !ok {
		return 0
	}
	return max(0, c.Open()-s.Paid-s.Held)
}

// Full reports whether the category has no place left for a new registration.
func Full(id string, s Slots) bool { return Left(id, s) == 0 }

// Unavailable lists the labels of the wanted categories that have no place left,
// in the order the categories are shown.
func Unavailable(wanted []string, slots map[string]Slots) []string {
	var out []string
	for _, id := range wanted {
		if Full(id, slots[id]) {
			c, _ := LookupCategory(id)
			out = append(out, c.Label)
		}
	}
	return out
}

// OverCapacity lists the wanted categories that already have as many paid
// registrations, other than this one, as the website can give places for. A
// registration that settles into one is money taken for a place that no longer
// exists, which the admin must refund.
func OverCapacity(wanted []string, paidByOthers map[string]int) []string {
	var out []string
	for _, id := range wanted {
		c, ok := LookupCategory(id)
		if ok && paidByOthers[id] >= c.Open() {
			out = append(out, id)
		}
	}
	return out
}

// Labels turns category ids into the labels people read: "U10 and U15".
func Labels(ids []string) string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		c, ok := LookupCategory(id)
		if !ok {
			c.Label = strings.ToUpper(id)
		}
		labels = append(labels, c.Label)
	}
	switch len(labels) {
	case 0:
		return ""
	case 1:
		return labels[0]
	}
	return strings.Join(labels[:len(labels)-1], ", ") + " and " + labels[len(labels)-1]
}

// ---- the academy ------------------------------------------------------------

const (
	// MinAcademyAge and MaxAcademyAge bound a player's age at registration. They
	// catch a mistyped birth year; they are not the academy's admission rules.
	MinAcademyAge = 3
	MaxAcademyAge = 21
)

// ParseBirthDate reads a player's date of birth (YYYY-MM-DD) and checks it gives
// a plausible age on `now`. Every error is safe to show to a parent.
func ParseBirthDate(s string, now time.Time) (string, error) {
	born, err := time.ParseInLocation("2006-01-02", s, schedule.Location())
	if err != nil || born.Format("2006-01-02") != s {
		return "", errors.New("enter the player's date of birth")
	}
	today := now.In(schedule.Location())
	if born.After(today) {
		return "", errors.New("the date of birth cannot be in the future")
	}
	age := today.Year() - born.Year()
	if today.Month() < born.Month() || (today.Month() == born.Month() && today.Day() < born.Day()) {
		age--
	}
	if age < MinAcademyAge || age > MaxAcademyAge {
		return "", fmt.Errorf("the player must be between %d and %d years old", MinAcademyAge, MaxAcademyAge)
	}
	return s, nil
}

// ---- references and money ---------------------------------------------------

// IsReference reports whether a payment reference belongs to a registration.
func IsReference(ref string) bool {
	return strings.HasPrefix(ref, TournamentReferencePrefix) || strings.HasPrefix(ref, AcademyReferencePrefix)
}

// TrimReferencePrefix removes whichever registration prefix a reference carries.
func TrimReferencePrefix(ref string) string {
	if s, ok := strings.CutPrefix(ref, TournamentReferencePrefix); ok {
		return s
	}
	return strings.TrimPrefix(ref, AcademyReferencePrefix)
}

// PayerEmail is the customer address Paystack requires on every charge. The forms
// ask for none, so one is made from the paying number, on the academy's own
// domain so it is never somebody else's inbox.
func PayerEmail(phone string) string {
	return phone + "@pay.karena.stsebastiansportsacademy.com"
}

// SortedIDs returns ids in the order Categories lists them, for stable output.
func SortedIDs(ids []string) []string {
	order := map[string]int{}
	for i, c := range categories {
		order[c.ID] = i
	}
	out := append([]string(nil), ids...)
	sort.SliceStable(out, func(i, j int) bool { return order[out[i]] < order[out[j]] })
	return out
}
