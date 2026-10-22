// Package validate checks and normalises the free-text fields of a booking
// request. These values end up in the database, in Paystack requests and in
// SMS bodies, so they are constrained at the door.
package validate

import (
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var kenyanMobile = regexp.MustCompile(`^254[17][0-9]{8}$`)

// maxMarksInARow is how many combining marks may follow one base character.
// Real names need one (sometimes two); a long run is a "Zalgo" name that piles
// up marks to overflow its box in the admin list.
const maxMarksInARow = 2

// apostropheLookalikes are what phone keyboards produce when you type '.
var apostropheLookalikes = strings.NewReplacer("\u2018", "'", "\u2019", "'", "\u02BC", "'", "\u0060", "'", "\u00B4", "'")

// invisibleFillers are letters (so IsLetter accepts them) that draw as blank
// space, which would let a name that looks empty pass.
func invisibleFiller(r rune) bool {
	switch r {
	case 0x115F, 0x1160, 0x3164, 0xFFA0, 0x17B4, 0x17B5:
		return true
	}
	return false
}

// Name tidies a person's name and restricts it to characters that are safe to
// echo into an SMS, an admin table and a spreadsheet. Curly apostrophes become
// plain ones, text is Unicode-normalised (so the same name always compares
// equal), and names made of blanks or stacked combining marks are refused.
func Name(s string) (string, error) {
	s = apostropheLookalikes.Replace(s)
	s = norm.NFC.String(s)
	s = strings.Join(strings.Fields(s), " ")
	n := utf8.RuneCountInString(s)
	if n < 2 || n > 60 {
		return "", errors.New("name must be between 2 and 60 characters")
	}
	substantial, marks := 0, 0
	for _, r := range s {
		switch {
		case invisibleFiller(r):
			return "", errors.New("name contains unsupported characters")
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			substantial++
			marks = 0
		case unicode.IsMark(r):
			if marks++; marks > maxMarksInARow {
				return "", errors.New("name contains unsupported characters")
			}
		case strings.ContainsRune(" '.-&", r):
			marks = 0
		default:
			return "", errors.New("name contains unsupported characters")
		}
	}
	if substantial < 2 {
		return "", errors.New("name must include at least two letters or numbers")
	}
	return s, nil
}

// Email accepts a single plain address (no display name, no comments).
func Email(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 254 {
		return "", errors.New("a valid email is required")
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s {
		return "", errors.New("a valid email is required")
	}
	at := strings.LastIndexByte(s, '@')
	if !strings.Contains(s[at+1:], ".") {
		return "", errors.New("a valid email is required")
	}
	return strings.ToLower(s), nil
}

// KenyanMobile normalises 0712345678, 712345678, +254712345678 and
// 254712345678 to 254712345678, rejecting anything that is not a Safaricom /
// Airtel style mobile number.
func KenyanMobile(s string) (string, error) {
	s = strings.NewReplacer(" ", "", "-", "", "+", "").Replace(s)
	switch {
	case strings.HasPrefix(s, "0") && len(s) == 10:
		s = "254" + s[1:]
	case len(s) == 9:
		s = "254" + s
	}
	if !kenyanMobile.MatchString(s) {
		return "", errors.New("enter a valid Kenyan mobile number, e.g. 712345678")
	}
	return s, nil
}
