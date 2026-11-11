package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"unicode"
)

// mentionRow is one row of testdata/rules/mentions.json.
type mentionRow struct {
	Body string `json:"body"`
	Name string `json:"name"`
	Want bool   `json:"want"`
}

// readMentionTable reads the golden table of the mention rule. The web
// client's test (web/src/mentions.golden.test.ts) reads the same file.
func readMentionTable(t *testing.T) []mentionRow {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "rules", "mentions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []mentionRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	return rows
}

// TestMentionsGoldenTable checks Mentions against every row of the golden
// table, which records the browser's rule (issue #1712).
func TestMentionsGoldenTable(t *testing.T) {
	rows := readMentionTable(t)
	if len(rows) < 40 {
		t.Fatalf("the table has %d rows, want at least 40", len(rows))
	}
	for i, row := range rows {
		if got := Mentions(row.Body, row.Name); got != row.Want {
			t.Errorf("row %d: Mentions(%+q, %+q) = %v, want %v", i, row.Body, row.Name, got, row.Want)
		}
	}
}

// TestMentionsGoldenTableCovers fails when the table loses a case the issue
// requires, so a row cannot be dropped to make a disagreement go away.
func TestMentionsGoldenTableCovers(t *testing.T) {
	rows := readMentionTable(t)
	has := func(body, name string, want bool) bool {
		for _, row := range rows {
			if row.Body == body && row.Name == name && row.Want == want {
				return true
			}
		}
		return false
	}
	space := func(r rune) string { return "x" + string(r) + "@ada" + string(r) + "y" }
	required := []mentionRow{
		{"@ada", "", false},
		{"@ada", "ada", true},
		{"hi @ada", "ada", true},
		{"a@ada", "ada", false},
		{"@adam", "ada", false},
		{"@ada@ada", "ada", false},
		{"x@ada then @ada", "ada", true},
		{"@ADA", "ada", true},
		{"@zo" + string(rune(0xeb)), "ZO" + string(rune(0xcb)), true},
		{"@" + string(rune(0x3c2)), string(rune(0x3a3)), true},
		{"@" + string(rune(0x3c3)), string(rune(0x3a3)), true},
		{"@i", string(rune(0x130)), false},
		{"@I", string(rune(0x131)), false},
		{"@a.b", "a.b", true},
		{"@axb", "a.b", false},
		{"hey @Ada L, hi", "Ada L", true},
		{space(0xa0), "ada", true},
		{space(0x2028), "ada", true},
		{space(0x3000), "ada", true},
		{space(0xfeff), "ada", true},
		{"x" + string(rune(0x85)) + "@ada", "ada", false},
		{"@ada" + string(rune(0x85)), "ada", false},
		{"line one\n@ada\nline three", "ada", true},
		{"\t@ada\t", "ada", true},
	}
	for _, c := range ".,!?;:'\"()[]{}<>" {
		required = append(required,
			mentionRow{string(c) + "@ada", "ada", true},
			mentionRow{"@ada" + string(c), "ada", true})
	}
	for _, row := range required {
		if !has(row.Body, row.Name, row.Want) {
			t.Errorf("the table has no row {%+q, %+q, %v}", row.Body, row.Name, row.Want)
		}
	}
}

// The boundary set is JavaScript's \s under the u flag and the rule's
// punctuation, and nothing else. This walks every code point, so a space
// added to one side and not the other fails here and not in a message.
func TestMentionBoundaryIsJavaScriptSpaceAndPunctuation(t *testing.T) {
	want := map[rune]bool{}
	for _, r := range "\t\n\v\f\r .,!?;:'\"()[]{}<>" {
		want[r] = true
	}
	for _, r := range []rune{0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff} {
		want[r] = true
	}
	for r := rune(0x2000); r <= 0x200a; r++ {
		want[r] = true
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if got := mentionBoundary(r); got != want[r] {
			t.Errorf("mentionBoundary(%U) = %v, want %v", r, got, want[r])
		}
	}
	// The two places unicode.IsSpace and JavaScript's \s part.
	if mentionBoundary(0x85) || !unicode.IsSpace(0x85) {
		t.Error("U+0085 is a Go space and not a JavaScript one; it must not be a boundary")
	}
	if !mentionBoundary(0xfeff) || unicode.IsSpace(0xfeff) {
		t.Error("U+FEFF is a JavaScript space and not a Go one; it must be a boundary")
	}
}

// mentionFold picks one member of each simple-folding class: it is the same
// for every member of a class, and unicode.SimpleFold's cycle always closes
// within the guard.
func TestMentionFold(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		least, steps := r, 0
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < least {
				least = f
			}
			if steps++; steps > 4 {
				t.Fatalf("unicode.SimpleFold(%U) has a cycle longer than four", r)
			}
		}
		if got := mentionFold(r); got != least {
			t.Errorf("mentionFold(%U) = %U, want %U", r, got, least)
		}
		if got := mentionFold(unicode.SimpleFold(r)); got != least {
			t.Errorf("mentionFold(SimpleFold(%U)) = %U, want %U", r, got, least)
		}
	}
	tests := []struct {
		a, b rune
		same bool
	}{
		{'a', 'A', true},
		{'k', 0x212a, true},      // the Kelvin sign
		{0x3a3, 0x3c2, true},     // capital and final sigma
		{0xdf, 0x1e9e, true},     // small and capital sharp s
		{'i', 0x130, false},      // Turkish dotted capital I
		{'I', 0x131, false},      // Turkish dotless small i
		{0x130, 0x131, false},    // and not each other
		{0x10400, 0x10428, true}, // Deseret long I
		{'a', 'b', false},
	}
	for _, tt := range tests {
		if got := mentionFold(tt.a) == mentionFold(tt.b); got != tt.same {
			t.Errorf("%U and %U fold alike = %v, want %v", tt.a, tt.b, got, tt.same)
		}
	}
}

// Rows that are easier to read here than in the table, and text that JSON
// cannot hold: a body that is not valid UTF-8 neither matches by accident
// nor panics.
func TestMentions(t *testing.T) {
	tests := []struct {
		name string
		body string
		who  string
		want bool
	}{
		{"a mention alone", "@ada", "ada", true},
		{"a name longer than the text", "@ad", "ada", false},
		{"the name at the very end, after an unbounded one", "@adam @ada", "ada", true},
		{"invalid UTF-8 before the mention", "\xff @ada", "ada", true},
		{"invalid UTF-8 as the boundary", "\xff@ada", "ada", false},
		{"invalid UTF-8 after the name", "@ada\xff", "ada", false},
		{"a body cut inside a code point", "@ad\xc3", "ad" + string(rune(0xe9)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Mentions(tt.body, tt.who); got != tt.want {
				t.Errorf("Mentions(%+q, %+q) = %v, want %v", tt.body, tt.who, got, tt.want)
			}
		})
	}
}
