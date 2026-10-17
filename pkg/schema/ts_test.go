package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
