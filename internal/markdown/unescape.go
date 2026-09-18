package markdown

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/util"
)

// unescapeText resolves backslash escapes and HTML entity references in raw
// Markdown text the way CommonMark specifies, in a single pass so that an
// escaped ampersand never starts an entity. NUL bytes become U+FFFD.
func unescapeText(src []byte) string {
	if !needsUnescape(src) {
		return string(src)
	}
	var b strings.Builder
	b.Grow(len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\\' && i+1 < len(src) && util.IsPunct(src[i+1]):
			b.WriteByte(src[i+1])
			i++
		case c == '&':
			if r, n := decodeEntity(src[i:]); n > 0 {
				b.WriteString(r)
				i += n - 1
			} else {
				b.WriteByte(c)
			}
		case c == 0:
			b.WriteRune(utf8.RuneError)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func needsUnescape(src []byte) bool {
	for _, c := range src {
		if c == '\\' || c == '&' || c == 0 {
			return true
		}
	}
	return false
}

// decodeEntity decodes an entity or numeric character reference at the start
// of src (which begins with '&'). It returns the replacement and the number of
// bytes consumed, or 0 when src does not start with a valid reference.
func decodeEntity(src []byte) (string, int) {
	if len(src) < 3 || src[0] != '&' {
		return "", 0
	}
	if src[1] == '#' {
		i := 2
		base := 10
		if i < len(src) && (src[i] == 'x' || src[i] == 'X') {
			base = 16
			i++
		}
		start := i
		for i < len(src) && ((base == 10 && util.IsNumeric(src[i])) || (base == 16 && util.IsHexDecimal(src[i]))) {
			i++
		}
		digits := i - start
		if digits == 0 || i >= len(src) || src[i] != ';' {
			return "", 0
		}
		if (base == 10 && digits > 7) || (base == 16 && digits > 6) {
			return "", 0
		}
		v, err := strconv.ParseUint(string(src[start:i]), base, 32)
		if err != nil {
			return "", 0
		}
		r := util.ToValidRune(rune(v))
		return string(r), i + 1
	}
	i := 1
	for i < len(src) && util.IsAlphaNumeric(src[i]) {
		i++
	}
	if i == 1 || i >= len(src) || src[i] != ';' {
		return "", 0
	}
	ent, ok := util.LookUpHTML5EntityByName(string(src[1:i]))
	if !ok {
		return "", 0
	}
	return string(ent.Characters), i + 1
}

// entityLike reports whether s starts with something the parser would read
// as an entity or numeric character reference.
func entityLike(s string) bool {
	_, n := decodeEntity([]byte(s))
	if n > 0 {
		return true
	}
	// Unknown names are left alone by the parser, but keep them escaped too
	// so the serializer never depends on the entity table.
	if len(s) < 3 || s[0] != '&' {
		return false
	}
	i := 1
	if s[1] == '#' {
		i = 2
		if i < len(s) && (s[i] == 'x' || s[i] == 'X') {
			i++
		}
		start := i
		for i < len(s) && (util.IsNumeric(s[i]) || util.IsHexDecimal(s[i])) {
			i++
		}
		return i > start && i < len(s) && s[i] == ';'
	}
	for i < len(s) && util.IsAlphaNumeric(s[i]) {
		i++
	}
	return i > 1 && i < len(s) && s[i] == ';'
}
