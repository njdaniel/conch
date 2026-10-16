// Package names decides when two principal names are the same name (issue
// #441, docs/design/private-alpha.md O3). Names are unique per server, and
// with invites people choose their own, so "unique" has to mean "cannot be
// mistaken for another": "Alice" and "alice" are one name, and so are
// "paypal" and the same word spelt with a Cyrillic a, "Alice" and "AIice"
// (a capital I for the l), "bob" and "b0b" (a zero for the o).
// A name is stored and shown as typed; its Keys are what is compared.
package names

import (
	"sort"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

//go:generate go run ./gen -out confusables_table.go

// fold is Unicode full case folding, without the Turkic special cases: "I"
// folds to "i" whatever the person's language. It is stateless and safe for
// concurrent use.
var fold = cases.Fold()

// Keys are the two keys of a name. Two names clash when their Folded keys are
// equal or their Skeletal keys are equal. A Folded key is never compared with
// a Skeletal one: each is one normal form, applied the same way to both
// names, and mixing them would compare two different orders of folding and
// mapping.
//
// Two keys, because neither order alone is enough. Folding first makes
// "Alice" and "alice" one name, but has already turned the capital I of
// "AIice" into an i and the 0 of "b0b" stays a digit, so neither looks like
// "Alice" or "bob" any more by the time look-alikes are mapped. Mapping first
// catches those (confusables.txt maps a capital I to l and 0 to O, then the
// fold makes O an o), but alone it would leave "Ian" and "ian" apart.
//
// Each skeleton is UTS #39's internalSkeleton (NFD, drop every
// Default_Ignorable_Code_Point, replace each character by its confusables.txt
// prototype, NFD again), from the tables in confusables_table.go. Dropping
// the default ignorables means the joiners (ZWJ, ZWNJ) and the directional
// marks, which a valid name may contain, never make a name different from the
// same name without them: an Indic name written with a joiner clashes with
// its plain spelling, as UTS #39 says it should. Last, runs of whitespace are
// collapsed to one space and trimmed, so a doubled space or a blank that maps
// to a space (U+2800) makes no new name.
//
// A key is an intermediate form, like a hash: never shown, and only
// comparable with keys computed from the same tables. It is stable for a
// given Unicode version of those tables and of golang.org/x/text; a change of
// either can change some keys, and the stored ones then need recomputing.
type Keys struct {
	// Folded is compact(skeleton(fold(NFC(name)))).
	Folded string
	// Skeletal is compact(skeleton(fold(skeleton(NFC(name))))).
	Skeletal string
}

// KeysOf returns the keys of name.
func KeysOf(name string) Keys {
	n := norm.NFC.String(name)
	return Keys{
		Folded:   compact(skeleton(fold.String(n))),
		Skeletal: compact(skeleton(fold.String(skeleton(n)))),
	}
}

// Empty reports whether either key is empty: the name is nothing but
// characters a reader cannot see (a Hangul filler, say), so it cannot be told
// from any other such name and must be refused.
func (k Keys) Empty() bool { return k.Folded == "" || k.Skeletal == "" }

// compact collapses every run of whitespace to one space and trims both ends.
func compact(s string) string { return strings.Join(strings.Fields(s), " ") }

func skeleton(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range norm.NFD.String(s) {
		if isDefaultIgnorable(r) {
			continue
		}
		if to, ok := prototypeOf(r); ok {
			b.WriteString(to)
			continue
		}
		b.WriteRune(r)
	}
	return norm.NFD.String(b.String())
}

// prototype is one row of confusables.txt: a character and what it looks like.
type prototype struct {
	from rune
	to   string
}

// runeRange is an inclusive range of code points.
type runeRange struct{ lo, hi rune }

func prototypeOf(r rune) (string, bool) {
	i := sort.Search(len(prototypes), func(i int) bool { return prototypes[i].from >= r })
	if i < len(prototypes) && prototypes[i].from == r {
		return prototypes[i].to, true
	}
	return "", false
}

func isDefaultIgnorable(r rune) bool {
	i := sort.Search(len(defaultIgnorable), func(i int) bool { return defaultIgnorable[i].hi >= r })
	return i < len(defaultIgnorable) && defaultIgnorable[i].lo <= r
}
