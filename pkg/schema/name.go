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
// strong bidi controls, and any other invisible formatting character. Such a
// name ends up in log lines, audit details, notification text and other
// people's terminals, where an escape sequence, a bidi override or a
// zero-width character in it would let one entry imitate another.
//
// The bidi refusal is the strong controls only — the overrides U+202A–U+202E
// and the isolates U+2066–U+2069, the characters that reorder what a terminal
// shows. The zero-width directional marks (LRM U+200E, RLM U+200F, ALM
// U+061C) cannot reorder text and legitimately appear in mixed-script names,
// so they pass, as do the joiners ZWJ/ZWNJ that Indic and Arabic names need.
// Every other Unicode format character (Cf) is refused: the zero-width space,
// the word joiner, the BOM, the tag characters and their kin are invisible
// and have no place in a name, where they make two different names look
// alike. Letters of every script, digits, interior spaces and ordinary
// punctuation are fine ("Zoë Müller", "山田 太郎", "o'brien-smith"). Whether
// the name may be empty is the caller's rule, not this one's. Errors name the
// rule and the field but never echo the input: it may be hostile.
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
		switch {
		case unicode.IsControl(c) || unicode.Is(unicode.Zl, c) || unicode.Is(unicode.Zp, c):
			return fmt.Errorf("schema: %s must not contain control characters, line or paragraph separators, or bidi controls", field)
		case isStrongBidiControl(c):
			return fmt.Errorf("schema: %s must not contain control characters, line or paragraph separators, or bidi controls", field)
		case unicode.Is(unicode.Cf, c) && !isAllowedFormatMark(c):
			return fmt.Errorf("schema: %s must not contain invisible formatting characters", field)
		}
	}
	return nil
}

// isStrongBidiControl reports whether c is one of the explicit bidi
// formatting controls that push, override or pop the direction stack: the
// embeddings and overrides U+202A–U+202E and the isolates U+2066–U+2069.
func isStrongBidiControl(c rune) bool {
	return c >= '\u202a' && c <= '\u202e' || c >= '\u2066' && c <= '\u2069'
}

// isAllowedFormatMark reports whether c is a Unicode format character (Cf)
// that a name may legitimately contain: the zero-width joiner and non-joiner
// that Indic and Arabic scripts need, and the directional marks LRM, RLM and
// ALM, which fix punctuation placement in mixed-script text and cannot
// reorder what is shown.
func isAllowedFormatMark(c rune) bool {
	switch c {
	case '\u200c', '\u200d', '\u200e', '\u200f', '\u061c':
		return true
	}
	return false
}
