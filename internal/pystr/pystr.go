// Package pystr reproduces a few Python string behaviors exactly, so the Go
// version of the app produces the same text as the Python one did.
package pystr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// IsSpace matches Python's str.isspace(), which also counts the ASCII
// file/group/record/unit separators (0x1C-0x1F).
func IsSpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// Strip is Python's str.strip() with no arguments.
func Strip(s string) string { return strings.TrimFunc(s, IsSpace) }

// IsWord matches Python's \w for str patterns: letters, digits, underscore (Unicode-aware).
func IsWord(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.In(r, unicode.Mn, unicode.Nd, unicode.Nl, unicode.No)
}

// HTMLEscape is Python's html.escape(s, quote=True).
func HTMLEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#x27;").Replace(s)
}

// ISOFormat is Python's datetime.isoformat(): microseconds only when non-zero.
func ISOFormat(t time.Time) string {
	layout := "2006-01-02T15:04:05"
	if t.Nanosecond()/1000 != 0 {
		layout += ".000000"
	}
	return t.Format(layout + "-07:00")
}

// ISOSeconds is isoformat(timespec="seconds").
func ISOSeconds(t time.Time) string { return t.Format("2006-01-02T15:04:05-07:00") }

// ParseISO reads what ISOFormat writes (and a plain date or date-time without an offset, in local time).
func ParseISO(s string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02T15:04", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid isoformat string: %q", s)
}

// Thousands formats like Python's f"{n:,}".
func Thousands(n int) string {
	s := strconv.Itoa(n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// Prefix returns the first n characters (not bytes), like s[:n] in Python.
func Prefix(s string, n int) string {
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// Len counts characters, like len(s) in Python.
func Len(s string) int { return len([]rune(s)) }
