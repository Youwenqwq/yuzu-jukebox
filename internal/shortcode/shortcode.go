package shortcode

import (
	"encoding/base32"
	"strings"
	"unicode"
)

const (
	Length   = 12
	Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
)

var encoding = base32.NewEncoding(Alphabet).WithPadding(base32.NoPadding)

// Encode converts entropy bytes into the canonical 12-character code.
func Encode(raw []byte) (string, bool) {
	encoded := encoding.EncodeToString(raw)
	if len(encoded) < Length {
		return "", false
	}
	return encoded[:Length], true
}

// Normalize accepts formatted or unformatted codes, ignoring case, whitespace,
// and hyphens. It returns the canonical 12-character representation.
func Normalize(code string) (string, bool) {
	canonical := strings.Map(func(r rune) rune {
		if r == '-' || unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToUpper(r)
	}, code)
	if len(canonical) != Length {
		return "", false
	}
	for _, r := range canonical {
		if !strings.ContainsRune(Alphabet, r) {
			return "", false
		}
	}
	return canonical, true
}

// Format renders a canonical code as XXXX-XXXX-XXXX.
func Format(canonical string) (string, bool) {
	canonical, ok := Normalize(canonical)
	if !ok {
		return "", false
	}
	return canonical[:4] + "-" + canonical[4:8] + "-" + canonical[8:], true
}
