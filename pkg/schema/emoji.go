package schema

import (
	"slices"
	"strings"
)

// The emoji catalog (issue #953, docs/design/message-reactions.md §3). The
// data is the Unicode Consortium's emoji-test.txt for one pinned Unicode
// Emoji version, vendored under third_party/unicode and generated into
// emoji_catalog_gen.go by cmd/emoji-gen; EmojiVersion says which. The same
// run generates the browser's catalog, so every client and the server agree
// on which sequences are emoji. The table is read-only after init.

// EmojiEntry is one emoji of the catalog: a fully-qualified sequence as
// emoji-test.txt lists it, which is the canonical reaction key for it
// (CanonicalReactionKey), with its CLDR short name and the group and
// subgroup the file places it in. The name is for search and for a terminal
// that cannot render the glyph; it never travels on the wire.
type EmojiEntry struct {
	Key      string
	Name     string
	Group    string
	Subgroup string
}

// EmojiCatalog returns every emoji of EmojiVersion in the file's (CLDR)
// order, which is the order a picker shows them. It is a copy.
func EmojiCatalog() []EmojiEntry {
	return slices.Clone(emojiCatalog[:])
}

// emojiByStripped maps every catalog key with its presentation selectors
// removed to the key. Every listed form of an emoji (fully-, minimally- and
// unqualified) strips to the same remainder, so one map serves them all; the
// generator has refused a data file in which two keys would share one.
var emojiByStripped = func() map[string]string {
	m := make(map[string]string, len(emojiCatalog))
	for _, e := range emojiCatalog {
		m[stripPresentationSelectors(e.Key)] = e.Key
	}
	return m
}()

// stripPresentationSelectors removes every U+FE0F (emoji presentation) and
// U+FE0E (text presentation) from s, wherever it occurs.
func stripPresentationSelectors(s string) string {
	return strings.Map(func(r rune) rune {
		if r == 0xFE0F || r == 0xFE0E {
			return -1
		}
		return r
	}, s)
}
