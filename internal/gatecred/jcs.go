package gatecred

import (
	"sort"
	"strconv"
)

// member is one member of the flat credential object: a string or an integer.
type member struct {
	key   string
	str   string
	num   int64
	isNum bool
}

// encodeJCS writes the RFC 8785 (JSON Canonicalization Scheme) form of a flat
// object of string and integer members. Keys are sorted by UTF-16 code units;
// every key here is ASCII, so that is a plain byte sort. Integers are below
// 2^53 and written in plain decimal, which is what ECMAScript's Number
// serialisation gives for them. Strings are escaped as ECMAScript's
// JSON.stringify does: the quote, the backslash, the short forms
// \b \t \n \f \r, other controls as \u00xx in lower case, and everything else
// as itself in UTF-8 (U+2028, U+2029 and characters above U+FFFF included).
func encodeJCS(ms []member) []byte {
	sorted := append([]member(nil), ms...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].key < sorted[j].key })
	b := []byte{'{'}
	for i, m := range sorted {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendJCSString(b, m.key)
		b = append(b, ':')
		if m.isNum {
			b = strconv.AppendInt(b, m.num, 10)
		} else {
			b = appendJCSString(b, m.str)
		}
	}
	return append(b, '}')
}

func appendJCSString(b []byte, s string) []byte {
	const hexDigits = "0123456789abcdef"
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\b':
			b = append(b, '\\', 'b')
		case '\f':
			b = append(b, '\\', 'f')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			if c < 0x20 {
				b = append(b, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			} else {
				b = append(b, c) // bytes >= 0x20 pass through: UTF-8 stays as it is
			}
		}
	}
	return append(b, '"')
}
