package schema

import (
	"unicode"
	"unicode/utf8"
)

// The shared mention rule (issue #1712, Mobile M11-S;
// docs/design/mobile-notifications.md §6.2 and §9.1, S6). It is a rule, not
// a wire shape: it is here so that "mentions" means one thing to the server,
// which applies a credential's NotificationPolicyMentions, and to the web
// client, which applies its own (as ValidateDisplayName is the one rule for
// names).
//
// The rule is the browser's, as mentions() in web/src/useNotifications.ts
// has it:
//
//	(?:^|[\s.,!?;:'"()[\]{}<>])@<name>(?=$|[\s.,!?;:'"()[\]{}<>])
//
// with the flags i and u, the name taken literally, and an empty name never
// matching. testdata/rules/mentions.json is the golden table of it. Go's
// test and the web client's both read that file, so the two cannot drift
// apart unseen.
//
// It is written out here, and not as a regexp, because Go's regexp cannot
// say it: RE2 has no lookahead, and Go's \s is ASCII only where
// JavaScript's is a list of Unicode spaces. unicode.IsSpace is not that list
// either: it has U+0085, which JavaScript's \s has not, and lacks U+FEFF,
// which JavaScript's has.

// Mentions reports whether body mentions name: "@" and then name, with the
// start of the text or a boundary character before the "@", and the end of
// the text or a boundary character after the name. A boundary character is
// a JavaScript space (mentionBoundary lists them) or one of
//
//	. , ! ? ; : ' " ( ) [ ] { } < >
//
// Letters, digits, "@", "_", "-" and everything else are not boundaries, so
// "a@ada", "@adam" and "@ada@ada" do not mention ada.
//
// The name is compared code point by code point under Unicode simple case
// folding, which is what JavaScript's i and u flags do: "ZOË" matches
// "zoë", and "Σ", "σ" and "ς" match each other, while the Turkish "İ" and
// "ı" match only themselves and "ß" does not match "ss". Nothing is
// normalized: a precomposed letter does not match its decomposed form.
//
// An empty name never matches. The caller passes the reader's current
// display name.
//
// The folding is the Unicode version of this Go's tables (15.0 in Go 1.25).
// A browser with newer tables folds the pairs that a later Unicode created,
// and Go does not until its tables catch up. Every pair that differs
// involves at least one code point added in Unicode 16 or later: Garay,
// Latin U+A7CB to U+A7DC (which gave capitals to old letters such as U+019B
// and U+0264), Cyrillic U+1C89 and U+1C8A, and Beria Erfe in 17.
func Mentions(body, name string) bool {
	if name == "" {
		return false
	}
	// prev is the code point before i; boundary before the start of text.
	atStart := true
	var prev rune
	for i := 0; i < len(body); {
		r, size := utf8.DecodeRuneInString(body[i:])
		if r == '@' && (atStart || mentionBoundary(prev)) {
			if rest, ok := mentionName(body[i+size:], name); ok {
				if rest == "" {
					return true
				}
				if next, _ := utf8.DecodeRuneInString(rest); mentionBoundary(next) {
					return true
				}
			}
		}
		atStart, prev = false, r
		i += size
	}
	return false
}

// mentionName reports whether text starts with name under simple case
// folding, and returns what follows it.
func mentionName(text, name string) (string, bool) {
	for name != "" {
		if text == "" {
			return "", false
		}
		want, wsize := utf8.DecodeRuneInString(name)
		got, gsize := utf8.DecodeRuneInString(text)
		if want != got && mentionFold(want) != mentionFold(got) {
			return "", false
		}
		name, text = name[wsize:], text[gsize:]
	}
	return text, true
}

// mentionFold returns one code point for every code point that is equal to
// r under Unicode simple case folding: the smallest of them. Two code points
// match without regard to case exactly when this is the same for both.
func mentionFold(r rune) rune {
	least := r
	// unicode.SimpleFold walks the code points equal to r, in a cycle that
	// comes back to r. The longest cycle has four members; the count is a
	// guard, not a limit anything reaches.
	for f, n := unicode.SimpleFold(r), 0; f != r && n < 8; f, n = unicode.SimpleFold(f), n+1 {
		if f < least {
			least = f
		}
	}
	return least
}

// mentionBoundary reports whether r may stand before the "@" of a mention or
// after its name: JavaScript's \s under the u flag, spelled out, and the
// rule's punctuation.
func mentionBoundary(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00a0, // no-break space
		0x1680, // ogham space mark
		0x2028, // line separator
		0x2029, // paragraph separator
		0x202f, // narrow no-break space
		0x205f, // medium mathematical space
		0x3000, // ideographic space
		0xfeff: // zero width no-break space, the byte order mark
		return true
	case '.', ',', '!', '?', ';', ':', '\'', '"', '(', ')', '[', ']', '{', '}', '<', '>':
		return true
	}
	// En quad to hair space.
	return r >= 0x2000 && r <= 0x200a
}
