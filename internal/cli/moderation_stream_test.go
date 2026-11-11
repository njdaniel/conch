package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestTailPrintsOnlyReceivedTombstonesWithoutRewriting(t *testing.T) {
	ts := schema.NewTimestamp(time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC))
	original := schema.MessageV2{Schema: schema.MessageSchemaV2, ID: 42, AuthorID: 7, CreatedAt: ts, Body: "original secret"}
	removed := original
	removed.Body = schema.MessageRemovedBody
	removed.Removed = true
	fake := newNetsFake(t)
	fake.stream = []schema.MessageV2{original, removed, removed}
	isolateConfig(t)
	stdout, stderr, err := runCLI(t, "", "tail", "--server", fake.URL, "general")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(stdout, "original secret"); got != 1 {
		t.Fatalf("original occurrences = %d, output %q", got, stdout)
	}
	if got := strings.Count(stdout, schema.MessageRemovedBody); got != 2 {
		t.Fatalf("tombstone occurrences = %d, output %q", got, stdout)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], "original secret") || lines[1] != lines[2] || !strings.HasSuffix(lines[1], schema.MessageRemovedBody) {
		t.Fatalf("stream lines = %#v", lines)
	}
	if strings.Contains(strings.ToLower(stdout), "rewritten") || strings.Contains(strings.ToLower(stdout), "retracted") {
		t.Fatalf("stream claims to rewrite earlier output: %q", stdout)
	}
	if !strings.Contains(stderr, "server shutting down") {
		t.Fatalf("stderr = %q", stderr)
	}
}
