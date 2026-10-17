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
)

var kenyanMobile = regexp.MustCompile(`^254[17][0-9]{8}$`)

// Name trims, collapses whitespace and restricts the representative name to
// characters that are safe to echo into an SMS.
func Name(s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	n := utf8.RuneCountInString(s)
	if n < 2 || n > 60 {
		return "", errors.New("name must be between 2 and 60 characters")
	}
	for _, r := range s {
		if !(unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || strings.ContainsRune(" '.-&", r)) {
			return "", errors.New("name contains unsupported characters")
		}
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
