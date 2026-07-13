package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// assertGoldenFixture decodes the named fixture from testdata into a fresh
// value (built by new), re-encodes it, and fails if the result doesn't
// byte-for-byte match the fixture. It also rejects unknown fields, so a
// fixture referencing a field the type doesn't declare fails loudly instead
// of silently dropping data.
func assertGoldenFixture(t *testing.T, file string, new func() any) {
	t.Helper()
	golden, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name comes from this package's own test table, not external input
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	value := new()
	decoder := json.NewDecoder(bytes.NewReader(golden))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	got, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatalf("marshal fixture value: %v", err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, golden) {
		t.Errorf("fixture does not match schema serialization\ngot:\n%s\nwant:\n%s", got, golden)
	}
}
