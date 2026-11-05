package domain

import (
	"strings"
	"time"
	"unicode"
)

// accentFold maps accented vowels and the letter enye to their ASCII base. It is
// intentionally explicit: it avoids external Unicode normalization tables
// and covers the alphabets used on Spanish and English labels.
var accentFold = map[rune]rune{
	'Á': 'A', 'À': 'A', 'Ä': 'A', 'Â': 'A', 'á': 'a', 'à': 'a', 'ä': 'a', 'â': 'a',
	'É': 'E', 'È': 'E', 'Ë': 'E', 'Ê': 'E', 'é': 'e', 'è': 'e', 'ë': 'e', 'ê': 'e',
	'Í': 'I', 'Ì': 'I', 'Ï': 'I', 'Î': 'I', 'í': 'i', 'ì': 'i', 'ï': 'i', 'î': 'i',
	'Ó': 'O', 'Ò': 'O', 'Ö': 'O', 'Ô': 'O', 'ó': 'o', 'ò': 'o', 'ö': 'o', 'ô': 'o',
	'Ú': 'U', 'Ù': 'U', 'Ü': 'U', 'Û': 'U', 'ú': 'u', 'ù': 'u', 'ü': 'u', 'û': 'u',
	'Ñ': 'N', 'ñ': 'n', 'Ç': 'C', 'ç': 'c',
}

// FoldAccents returns the text without accents, preserving letter case.
func FoldAccents(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if f, ok := accentFold[r]; ok {
			r = f
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NormalizeText produces the canonical form for comparing product text:
// accent-free, uppercase, letters/digits/spaces only, with whitespace runs
// collapsed to one. Two labels of the same product from different suppliers
// must produce the same canonical form.
func NormalizeText(s string) string {
	s = strings.ToUpper(FoldAccents(s))
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true // trims leading spaces
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastSpace = false
		case unicode.IsSpace(r):
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		default:
			// Punctuation and symbols are not significant in the name.
		}
	}
	return strings.TrimSpace(b.String())
}

// NormalizeCode produces the canonical form for codes (SKU, lot, component):
// no spaces, uppercase, keeping hyphens.
func NormalizeCode(s string) string {
	s = strings.ToUpper(FoldAccents(strings.TrimSpace(s)))
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
}

// OnlyDigits extracts the ASCII digits of a text (e.g. a GTIN read by OCR
// with spaces or hyphens).
func OnlyDigits(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// GTINCheckDigit computes the GTIN check digit for the first 12 digits.
// It returns -1 when the input is not exactly 12 digits.
func GTINCheckDigit(first12 string) int {
	d := OnlyDigits(first12)
	if len(d) != 12 {
		return -1
	}
	sum := 0
	// Positions from the right, excluding the check digit: odd ones x3.
	for i := 0; i < 12; i++ {
		n := int(d[11-i] - '0')
		if i%2 == 0 {
			sum += n * 3
		} else {
			sum += n
		}
	}
	return (10 - (sum % 10)) % 10
}

// ValidGTIN13 checks format (13 digits) and check digit.
func ValidGTIN13(gtin string) bool {
	d := OnlyDigits(gtin)
	if len(d) != 13 {
		return false
	}
	return GTINCheckDigit(d[:12]) == int(d[12]-'0')
}

// WithCheckDigit completes a GTIN-13 from its first 12 digits. It returns an
// empty string for invalid input. Order seeding and tests use it to generate
// always-valid codes.
func WithCheckDigit(first12 string) string {
	c := GTINCheckDigit(first12)
	if c < 0 || c > 9 { // explicit bound: the digit conversion is safe
		return ""
	}
	return OnlyDigits(first12) + string(rune('0'+c))
}

// ValidLot checks the accepted lot format on the value as given (only edges
// are trimmed and accents/case are folded): 4-20 alphanumeric characters or
// hyphens, starting with an alphanumeric. An inner space invalidates the
// format; space tolerance for an OCR reading lives in NormalizeCode at
// comparison time, not here.
func ValidLot(lot string) bool {
	l := strings.ToUpper(FoldAccents(strings.TrimSpace(lot)))
	if len(l) < 4 || len(l) > 20 {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		isAlnum := (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if i == 0 && !isAlnum {
			return false
		}
		if !isAlnum && c != '-' {
			return false
		}
	}
	return true
}

// DateOnly formats a date as 2006-01-02 in UTC.
func DateOnly(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// TruncateToDay returns the date at UTC midnight.
func TruncateToDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// expiryLayouts are the accepted label date formats, most to least specific.
// Month-year forms are handled apart (last day of the month).
var expiryLayouts = []string{
	"2006-01-02",
	"02/01/2006",
	"02-01-2006",
	"20060102",
}

var expiryMonthLayouts = []string{
	"01/2006",
	"2006-01",
}

// ParseExpiry interprets the expiry text read from the label. It returns
// ok=false when no accepted format recognizes it (unreadable expiry).
func ParseExpiry(text string) (time.Time, bool) {
	s := strings.TrimSpace(strings.ToUpper(text))
	// Common prefixes printed next to the date (English and Spanish labels).
	for _, p := range []string{"EXP:", "EXP", "CAD:", "CAD", "VENCE:", "VENCE"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimSpace(strings.TrimPrefix(s, p))
			break
		}
	}
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range expiryLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return TruncateToDay(t), true
		}
	}
	for _, layout := range expiryMonthLayouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			// Last day of the given month.
			first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
			return first.AddDate(0, 1, -1), true
		}
	}
	return time.Time{}, false
}
