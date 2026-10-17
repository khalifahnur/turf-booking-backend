package validate

import (
	"strings"
	"testing"
)

func TestKenyanMobile(t *testing.T) {
	good := map[string]string{
		"254712345678":    "254712345678",
		"254112345678":    "254112345678",
		"+254 712-345678": "254712345678",
		"0712345678":      "254712345678",
		"712345678":       "254712345678",
	}
	for in, want := range good {
		got, err := KenyanMobile(in)
		if err != nil || got != want {
			t.Errorf("KenyanMobile(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "25471234567", "2547123456789", "254212345678", "abcdefghi", "254712345678; DROP", "+1 555 123 4567", "0212345678"} {
		if _, err := KenyanMobile(in); err == nil {
			t.Errorf("KenyanMobile(%q) should fail", in)
		}
	}
}

func TestName(t *testing.T) {
	for in, want := range map[string]string{
		"  Khalifah   Nur ": "Khalifah Nur",
		"O'Brien-Smith Jr.": "O'Brien-Smith Jr.",
		"FC Spartans & Co":  "FC Spartans & Co",
		"Wanjiru Njoroge":   "Wanjiru Njoroge",
		"José Muñoz":        "José Muñoz",
		"Team 7":            "Team 7",
	} {
		got, err := Name(in)
		if err != nil || got != want {
			t.Errorf("Name(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "A", strings.Repeat("a", 61), "Bob\nClick http://evil.example", "<script>", "a\x00b", "Bob; Free money: http://x.co/y?z=1"} {
		if _, err := Name(in); err == nil {
			t.Errorf("Name(%q) should fail", in)
		}
	}
}

func TestEmail(t *testing.T) {
	for in, want := range map[string]string{
		"abc@gmail.com":           "abc@gmail.com",
		"  Team.Lead@Club.CO.KE ": "team.lead@club.co.ke",
	} {
		got, err := Email(in)
		if err != nil || got != want {
			t.Errorf("Email(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "nope", "a@b", "Bob <bob@x.com>", "a@b.com, c@d.com", strings.Repeat("a", 250) + "@x.com", "a b@x.com"} {
		if _, err := Email(in); err == nil {
			t.Errorf("Email(%q) should fail", in)
		}
	}
}
