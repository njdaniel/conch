package schema

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateDisplayName reports whether name is acceptable as an operator-chosen
// display name or label — a principal or channel name, a credential or hook
// label: valid UTF-8, at most max characters, no leading or trailing
// whitespace, and no character that can rewrite what a reader sees — C0, DEL
// and C1 control characters, the Unicode line and paragraph separators, the
// zero-width space, and the strong bidi controls. Such a name ends up in log
// lines, audit details, notification text and other people's terminals, where
// an escape sequence or a bidi override in it would let one entry imitate
// another.
//
// The bidi refusal is the strong controls only — the overrides U+202A–U+202E
// and the isolates U+2066–U+2069, the characters that reorder what a terminal
// shows. The zero-width directional marks (LRM U+200E, RLM U+200F, ALM
// U+061C) cannot reorder text and legitimately appear in mixed-script names,
// so they pass, as do the joiners ZWJ/ZWNJ that Indic and Arabic names need.
// Letters of every script, digits, interior spaces and ordinary punctuation
// are fine ("Zoë Müller", "山田 太郎", "o'brien-smith"). Whether the name may
// be empty is the caller's rule, not this one's. Errors name the rule and the
// field but never echo the input: it may be hostile.
func ValidateDisplayName(field, name string, max int) error {
	if !utf8.ValidString(name) {
		return fmt.Errorf("schema: %s must be valid UTF-8", field)
	}
	if n := utf8.RuneCountInString(name); n > max {
		return fmt.Errorf("schema: %s must be at most %d characters, got %d", field, max, n)
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("schema: %s must not begin or end with whitespace", field)
	}
	for _, c := range name {
		if unicode.IsControl(c) || unicode.Is(unicode.Zl, c) || unicode.Is(unicode.Zp, c) || isStrongBidiControl(c) {
			return fmt.Errorf("schema: %s must not contain control characters, line or paragraph separators, or bidi controls", field)
		}
		if c == '\u200b' {
			return fmt.Errorf("schema: %s must not contain zero-width spaces", field)
		}
	}
	return nil
}

// isStrongBidiControl reports whether c is one of the explicit bidi
// formatting controls that push, override or pop the direction stack: the
// embeddings and overrides U+202A–U+202E and the isolates U+2066–U+2069. The
// zero-width marks of the Unicode Bidi_Control property (U+061C, U+200E,
// U+200F) are deliberately not included; see ValidateDisplayName.
func isStrongBidiControl(c rune) bool {
	return c >= '\u202a' && c <= '\u202e' || c >= '\u2066' && c <= '\u2069'
}
