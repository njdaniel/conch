package voicehelper

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// diagLines is how many of the helper's last log lines are kept.
	diagLines = 16
	// diagLineBytes is the most of each that is kept.
	diagLineBytes = 200
	// redacted stands in for the token, should a log line ever hold it.
	redacted = "[redacted]"
)

// sanitizeDiag makes one line of the helper's log safe to show: invalid
// UTF-8, control characters and invisible formatting characters (which can
// reorder what a terminal shows) removed, the token replaced, and the rest cut
// to diagLineBytes. The helper scrubs its own log; this does not rely on it.
func sanitizeDiag(line []byte, token string) string {
	s := printable(line)
	if token != "" {
		// Both forms: a token split by a control character is whole again
		// once the character is gone.
		s = strings.ReplaceAll(s, token, redacted)
		if clean := printable([]byte(token)); clean != "" {
			s = strings.ReplaceAll(s, clean, redacted)
		}
	}
	if len(s) > diagLineBytes {
		cut := diagLineBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut]
	}
	return s
}

// printable is b without invalid UTF-8, control characters or formatting
// characters.
func printable(b []byte) string {
	var out strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		b = b[size:]
		if (r == utf8.RuneError && size == 1) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

// quote writes a path for a message: quoted, with anything unprintable
// escaped, since CONCH_VOICE_HELPER may hold anything.
func quote(s string) string { return strconv.Quote(s) }
