package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDurationConst(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		want    time.Duration
		wantErr bool
	}{
		{"milliseconds", "const (\n\tvoicePassGapMax  = 1500 * time.Millisecond\n)\n", 1500 * time.Millisecond, false},
		{"seconds", "const voicePassGapMax = 15 * time.Second\n", 15 * time.Second, false},
		{"another name only", "const voicePassGapMaxOther = 3 * time.Second\n", 0, true},
		{"not a product", "const voicePassGapMax = time.Second\n", 0, true},
		{"absent", "package server\n", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			dir := filepath.Join(repo, "internal", "server")
			if err := os.MkdirAll(dir, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "voice.go"), []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := durationConst(repo, "voice.go", "voicePassGapMax")
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("durationConst = %v, %v; want %v (error %v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

// TestReadVoiceBound reads the server's real constants: the bound is the
// poller's longest gap plus a join token's lifetime, and a rename or reshape
// of either fails here, not in the scenario's last minute.
func TestReadVoiceBound(t *testing.T) {
	got, err := readVoiceBound(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if got <= 15*time.Second {
		t.Fatalf("bound = %s, want more than the token lifetime alone", got)
	}
}
