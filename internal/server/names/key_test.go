package names

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/njdaniel/conch/pkg/schema"
)

// Pairs of names that must clash, and pairs that must not. Every name is one
// a person may choose: each passes the create rule, so the key is never the
// only thing keeping a name out.
func TestKey(t *testing.T) {
	tests := []struct {
		name  string
		a, b  string
		clash bool
	}{
		// Case.
		{"case: capitalised", "Alice", "alice", true},
		{"case: upper", "ALICE", "alice", true},
		{"case: sharp s folds to ss", "Straße", "STRASSE", true},
		{"case: a Greek final sigma is a sigma", "ΟΔΥΣΣΕΥΣ", "οδυσσευς", true},

		// The Turkish dotless i. Folding is not Turkic, so "I" is "i"; the
		// dotless "ı" looks like an "i" without its dot, and the data says so.
		{"turkish: dotless i is i", "ılker", "ilker", true},
		{"turkish: dotless i is capital I", "ılker", "Ilker", true},
		{"turkish: capital dotted I keeps its dot", "İlker", "ilker", false},

		// Ligatures and compatibility forms.
		{"ligature: fi", "ﬁona", "fiona", true},
		{"ligature: ffl", "waﬄe", "waffle", true},
		{"fullwidth letters", "ａｌｉｃｅ", "alice", true},
		{"rn looks like m", "rnary", "mary", true},

		// One script's letters standing in for another's.
		{"cyrillic a in a latin name", "pаypal", "paypal", true},
		{"cyrillic whole-word look-alike", "аре", "ape", true},
		{"cyrillic capital A", "Аlice", "alice", true},
		{"greek omicron", "οmega", "omega", true},
		{"greek capital omicron", "Οmega", "omega", true},
		{"mixed scripts both ways", "pаypаl", "раураl", true},

		// Invisible marks a valid name may carry change nothing.
		{"a left-to-right mark", "alice\u200e", "alice", true},
		{"a right-to-left mark", "\u200fdana", "dana", true},

		// Indic names with joiners. ZWJ and ZWNJ are valid in a name but are
		// default-ignorable, so UTS #39 drops them: a name with a joiner is the
		// same name without it.
		{"devanagari ZWJ: same name without it", "अर्\u200dजुन", "अर्जुन", true},
		{"devanagari ZWNJ: same name without it", "क्\u200cष", "क्ष", true},
		{"malayalam ZWJ chillu and atomic chillu", "ന്\u200d", "ൻ", true},
		{"devanagari ZWJ name: a different name", "अर्\u200dजुन", "अर्पण", false},
		{"devanagari: other letter order", "क्\u200dष", "ष्क", false},

		// Names that must not clash.
		{"different names", "alice", "alicia", false},
		{"a diaeresis is not nothing", "Zoë", "Zoe", false},
		{"precomposed and decomposed are one", "Zoë", "Zoe\u0308", true},
		{"han characters", "山田 太郎", "山田 次郎", false},
		{"a space is not nothing", "ann lee", "annlee", false},
		{"punctuation", "o'brien", "obrien", false},
		{"hidden names stay apart", "hidden-12", "hidden-13", false},
		{"hidden name is not the visible one", "hidden-12", "hidden-21", false},

		// Latin look-alikes that folding first would hide: the capital I
		// that reads as l, the zero that reads as O. The skeletal key
		// catches them.
		{"capital I for l", "Alice", "AIice", true},
		{"capital I for l, twice", "Bill", "BiII", true},
		{"zero for o", "bob", "b0b", true},
		{"zero for O", "BOB", "b0b", true},
		{"capital I for l at the start", "Ian", "lan", true},
		{"one for l", "lily", "1ily", true},
		// ...but small letters that only resemble each other stay apart.
		{"l and i are different letters", "lily", "iiiy", false},
		{"small i and small l", "ian", "lan", false},
		{"o and a", "bob", "bab", false},

		// Whitespace: a doubled space, or a blank that maps to a space, makes
		// no new name; a missing space does.
		{"a doubled space", "Alice  Smith", "Alice Smith", true},
		{"an ideographic space", "Alice\u3000Smith", "alice smith", true},
		{"a braille blank at the end", "Alice\u2800", "alice", true},
		{"a braille blank inside", "Alice\u2800Smith", "alice smith", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, n := range []string{tt.a, tt.b} {
				if err := (schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: n}).Validate(); err != nil {
					t.Fatalf("%q is not a valid name: %v", n, err)
				}
			}
			ka, kb := KeysOf(tt.a), KeysOf(tt.b)
			if got := ka.Folded == kb.Folded || ka.Skeletal == kb.Skeletal; got != tt.clash {
				t.Errorf("KeysOf(%q) = %+q, KeysOf(%q) = %+q; clash = %v, want %v", tt.a, ka, tt.b, kb, got, tt.clash)
			}
		})
	}
}

// A name made only of characters nobody can see has empty keys, and the
// store refuses it; a name with one visible character does not.
func TestKeysEmpty(t *testing.T) {
	tests := []struct {
		name  string
		empty bool
	}{
		{"\u3164", true},             // HANGUL FILLER: default-ignorable
		{"\u3164\u3164", true},       // twice
		{"\u2800", true},             // BRAILLE PATTERN BLANK: maps to a space, trimmed
		{"\u3164\u2800\u3164", true}, // both
		{"\u115f\u1160", true},       // the Hangul choseong and jungseong fillers
		{"a\u3164", false},           // one letter is enough
		{"\u2800x\u2800", false},
	}
	for _, tt := range tests {
		if got := KeysOf(tt.name).Empty(); got != tt.empty {
			t.Errorf("KeysOf(%+q).Empty() = %v, want %v (keys %+q)", tt.name, got, tt.empty, KeysOf(tt.name))
		}
	}
}

// Keys are stored, so they must not change under the database's feet. The
// Unicode versions of every table they come from are pinned here, and so are
// the keys of a few names: an upgrade of golang.org/x/text (or of Go, which
// picks x/text's tables) or a regenerated confusables table that changes a
// key fails this test, and must ship with a migration that recomputes the
// stored keys.
func TestKeyStable(t *testing.T) {
	for _, v := range []struct{ what, got, want string }{
		{"golang.org/x/text/unicode/norm.Version", norm.Version, "15.0.0"},
		{"golang.org/x/text/cases.UnicodeVersion", cases.UnicodeVersion, "15.0.0"},
		{"the confusables table's Unicode version", tableUnicodeVersion, "18.0.0"},
	} {
		if v.got != v.want {
			t.Errorf("%s = %s, pinned %s: stored name keys may have changed; recompute them in a migration, then move the pin", v.what, v.got, v.want)
		}
	}
	golden := []struct {
		name string
		want Keys
	}{
		{"Alice", Keys{"alice", "alice"}},
		{"AIice", Keys{"aiice", "alice"}},
		{"b0b", Keys{"bOb", "bob"}},
		{"hidden-12", Keys{"hidden-l2", "hidden-l2"}},
		{"\ufb01ona", Keys{"fiona", "fiona"}},
		{"p\u0430ypal", Keys{"paypal", "paypal"}},
		{"\u0131lker", Keys{"ilker", "ilker"}},
		{"Ann  Lee\u2800", Keys{"ann lee", "ann lee"}},
		{"\u0905\u0930\u094d\u200d\u091c\u0941\u0928", Keys{"\u0905\u0930\u094d\u091c\u0941\u0928", "\u0905\u0930\u094d\u091c\u0941\u0928"}},
	}
	for _, g := range golden {
		if got := KeysOf(g.name); got != g.want {
			t.Errorf("KeysOf(%+q) = %+q, pinned %+q", g.name, got, g.want)
		}
	}
	for _, n := range []string{"Alice", "p\u0430ypal", "\ufb01ona", "\u0905\u0930\u094d\u200d\u091c\u0941\u0928", "hidden-7", "\u5c71\u7530 \u592a\u90ce", "\u0131lker", "\u216b", "B0B", "BiII"} {
		k := KeysOf(n)
		if k.Empty() {
			t.Errorf("KeysOf(%+q) is empty", n)
		}
		if again := KeysOf(n); again != k {
			t.Errorf("KeysOf(%+q) = %+q then %+q", n, k, again)
		}
	}
}

// The table in confusables_table.go records the Unicode version and the
// hashes of the files it came from. They must be the generator's pins: a
// table generated from other data, or a pin changed without regenerating,
// fails here. The generator refuses any file whose hash is not its pin, so
// this ties the committed table to the pinned bytes without the network.
func TestTablePins(t *testing.T) {
	pins := generatorPins(t)
	for _, c := range []struct{ pin, recorded string }{
		{"unicodeVersion", tableUnicodeVersion},
		{"confusablesSHA256", tableConfusablesSHA256},
		{"derivedSHA256", tableDerivedSHA256},
	} {
		want, ok := pins[c.pin]
		if !ok {
			t.Fatalf("gen/main.go has no constant %s", c.pin)
		}
		if c.recorded != want {
			t.Errorf("the table records %s = %q, the generator pins %q: run go generate ./internal/server/names", c.pin, c.recorded, want)
		}
	}
	if len(prototypes) == 0 || len(defaultIgnorable) == 0 {
		t.Fatal("the table is empty")
	}
	for i := 1; i < len(prototypes); i++ {
		if prototypes[i-1].from >= prototypes[i].from {
			t.Fatalf("prototypes is not sorted at %d", i)
		}
	}
	for i := 1; i < len(defaultIgnorable); i++ {
		if defaultIgnorable[i-1].hi+1 >= defaultIgnorable[i].lo {
			t.Fatalf("defaultIgnorable is not sorted and merged at %d", i)
		}
	}
}

// generatorPins reads the string constants of gen/main.go.
func generatorPins(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join("gen", "main.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				pins[name.Name] = v
			}
		}
	}
	return pins
}
