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

func TestNameNormalisesAndRejectsTricks(t *testing.T) {
	// A phone's curly apostrophe is accepted and stored as a plain one.
	for in, want := range map[string]string{
		"Ng\u2019ang\u2019a":     "Ng'ang'a",
		"O\u2018Brien":           "O'Brien",
		"Wang\u02BCombe":         "Wang'ombe",
		"Jose\u0301 Mun\u0303oz": "José Muñoz", // decomposed accents are composed
	} {
		got, err := Name(in)
		if err != nil || got != want {
			t.Errorf("Name(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// Same name typed two ways must compare equal after normalising.
	a, _ := Name("Jose\u0301")
	b, _ := Name("José")
	if a != b {
		t.Errorf("composed and decomposed forms differ: %q vs %q", a, b)
	}

	// Every blank-drawing filler is refused on its own, not just two of them.
	for _, filler := range []rune{0x115F, 0x1160, 0x3164, 0xFFA0, 0x17B4, 0x17B5} {
		in := "Amani " + string(filler) + string(filler)
		if got, err := Name(in); err == nil {
			t.Errorf("filler U+%04X: Name(%q) = %q should have been rejected", filler, in, got)
		}
	}

	// Real names keep the combining marks that survive composition: one or two
	// are fine, a pile of them is not.
	for in, want := range map[string]string{
		"\u1ECC\u0301b\u00E0":  "\u1ECC\u0301b\u00E0", // Yoruba: letter + one mark left after NFC
		"Ax\u0300\u0301\u0302": "",                    // x has no precomposed form: three marks stay three, rejected
		"`Amani":               "'Amani",              // backtick typed for an apostrophe
		"N\u00B4gang\u00B4a":   "N'gang'a",            // acute accent typed for an apostrophe
		"Ng\u02BCang\u02BCa":   "Ng'ang'a",
	} {
		got, err := Name(in)
		if want == "" {
			if err == nil {
				t.Errorf("Name(%q) = %q should have been rejected", in, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("Name(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	// The threshold is exactly two marks in a row.
	if _, err := Name("Ax\u0300\u0301"); err != nil {
		t.Errorf("two combining marks should be accepted: %v", err)
	}
	if _, err := Name("Ax\u0300\u0301\u0302"); err == nil {
		t.Error("three combining marks in a row should be rejected")
	}

	for name, in := range map[string]string{
		"hangul fillers (blank)":     "\u3164\u3164",
		"hangul choseong filler":     "A\u115F",
		"marks only":                 "\u0301\u0301\u0301",
		"zalgo":                      "A" + strings.Repeat("\u0301", 10),
		"punctuation only":           "..",
		"hyphens only":               "--",
		"one letter and punctuation": "A.",
		"zero width joiner":          "Ama\u200Dni",
		"right-to-left override":     "Amani\u202Etxt",
		"emoji":                      "Amani \U0001F600",
	} {
		if got, err := Name(in); err == nil {
			t.Errorf("%s: Name(%q) = %q should have been rejected", name, in, got)
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
