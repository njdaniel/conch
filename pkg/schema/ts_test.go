package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestTSTypesFixtures checks TSTypes against the golden fixtures: each Family
// names at least one, each of its fixtures decodes into the row's type with no
// unknown field, and each carries the row's Schema when it has one. The web
// test runs the generated validators over the same files; this finds a wrong
// row with Go alone.
func TestTSTypesFixtures(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range TSTypes {
		if row.Family == "" {
			continue
		}
		t.Run(row.Family, func(t *testing.T) {
			var family []string
			for _, file := range files {
				base := filepath.Base(file)
				if base == row.Family+".json" || strings.HasPrefix(base, row.Family+"-") {
					family = append(family, file)
				}
			}
			if len(family) == 0 {
				t.Fatalf("no fixture testdata/%s.json or testdata/%s-*.json", row.Family, row.Family)
			}
			for _, file := range family {
				data, err := os.ReadFile(file) // #nosec G304 -- a fixture path this test globbed from its own testdata
				if err != nil {
					t.Fatal(err)
				}
				dec := json.NewDecoder(bytes.NewReader(data))
				dec.DisallowUnknownFields()
				if err := dec.Decode(reflect.New(reflect.TypeOf(row.Value)).Interface()); err != nil {
					t.Errorf("%s does not decode into %T: %v", file, row.Value, err)
				}
				if row.Schema == "" {
					continue
				}
				var doc struct {
					Schema string `json:"schema"`
				}
				if err := json.Unmarshal(data, &doc); err != nil {
					t.Fatal(err)
				}
				if doc.Schema != row.Schema {
					t.Errorf("%s has schema %q, the row says %q", file, doc.Schema, row.Schema)
				}
			}
		})
	}
}

// TestTSVocabularyModes checks the modes table against Vocabularies: every
// vocabulary has a mode and nothing else does, each mode is one of the two,
// the ones the issue names are tolerant, and no union's tag is (issue
// #1051). cmd/schema-ts refuses the same table faults; this finds them with
// Go alone.
func TestTSVocabularyModes(t *testing.T) {
	var want, got []string
	for name := range Vocabularies {
		want = append(want, name)
	}
	for name, mode := range TSVocabularyModes {
		got = append(got, name)
		if mode != TSStrict && mode != TSTolerant {
			t.Errorf("TSVocabularyModes[%q] = %d, want TSStrict or TSTolerant", name, mode)
		}
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TSVocabularyModes lists %v,\nVocabularies lists %v", got, want)
	}
	for _, name := range []string{"Permission", "PermissionsWarning", "JoinWarning"} {
		if TSVocabularyModes[name] != TSTolerant {
			t.Errorf("TSVocabularyModes[%q] is strict; the type's contract says a client shows a value it does not know by its name", name)
		}
	}
	for _, u := range TSUnions {
		typ := reflect.TypeOf(u.Value)
		for i := range typ.NumField() {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name != u.Tag {
				continue
			}
			if TSVocabularyModes[f.Type.Name()] == TSTolerant {
				t.Errorf("%s is tolerant, and is the tag of the union %s; a tagged validator has no variant for a value it does not know", f.Type.Name(), typ.Name())
			}
		}
	}
}
