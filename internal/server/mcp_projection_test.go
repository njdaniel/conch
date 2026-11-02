package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// The MCP outputs map 1:1 onto pkg/schema (ADR-001). They are projected by
// JSON round trip, which drops any field the MCP struct does not declare, so
// a field added to a schema type is lost over MCP without a compile error.
// These guards feed every golden fixture of the projected types through the
// projection and fail if the result is not the fixture, field for field.

func readFixture(t *testing.T, path string) any {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a golden fixture under pkg/schema/testdata
	if err != nil {
		t.Fatal(err)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return doc
}

func asJSONValue(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func schemaFixtures(t *testing.T, glob string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "pkg", "schema", "testdata", glob))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures match %s (%v)", glob, err)
	}
	return files
}

func TestMCPMessageProjectionLosesNoField(t *testing.T) {
	for _, file := range schemaFixtures(t, "message-v2*.json") {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file) //nolint:gosec // a golden fixture under pkg/schema/testdata
			if err != nil {
				t.Fatal(err)
			}
			var message schema.MessageV2
			if err := json.Unmarshal(raw, &message); err != nil {
				t.Fatal(err)
			}
			out, err := mcpMessageFromSchema(message)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := asJSONValue(t, out), readFixture(t, file); !reflect.DeepEqual(got, want) {
				t.Errorf("MCP projection differs from the fixture:\n got  %v\n want %v", got, want)
			}
		})
	}
}

func TestMCPReactionProjectionsLoseNoField(t *testing.T) {
	tests := []struct {
		glob    string
		project func(raw []byte) (any, error)
	}{
		{"reaction-summary-v1*.json", func(raw []byte) (any, error) {
			var doc schema.ReactionSummaryV1
			if err := json.Unmarshal(raw, &doc); err != nil {
				return nil, err
			}
			return projectMCP[mcpReactionSummary](doc)
		}},
		{"reaction-summaries-response-v1*.json", func(raw []byte) (any, error) {
			var doc schema.ReactionSummariesResponseV1
			if err := json.Unmarshal(raw, &doc); err != nil {
				return nil, err
			}
			return projectMCP[mcpReactionSummaries](doc)
		}},
		{"reaction-v1*.json", func(raw []byte) (any, error) {
			var doc schema.ReactionV1
			if err := json.Unmarshal(raw, &doc); err != nil {
				return nil, err
			}
			return projectMCP[mcpReactor](doc)
		}},
		{"list-reactors-response-v1*.json", func(raw []byte) (any, error) {
			var doc schema.ListReactorsResponseV1
			if err := json.Unmarshal(raw, &doc); err != nil {
				return nil, err
			}
			return projectMCP[mcpReactorList](doc)
		}},
	}
	for _, tt := range tests {
		for _, file := range schemaFixtures(t, tt.glob) {
			t.Run(filepath.Base(file), func(t *testing.T) {
				raw, err := os.ReadFile(file) //nolint:gosec // a golden fixture under pkg/schema/testdata
				if err != nil {
					t.Fatal(err)
				}
				out, err := tt.project(raw)
				if err != nil {
					t.Fatal(err)
				}
				if got, want := asJSONValue(t, out), readFixture(t, file); !reflect.DeepEqual(got, want) {
					t.Errorf("MCP projection differs from the fixture:\n got  %v\n want %v", got, want)
				}
			})
		}
	}
}
