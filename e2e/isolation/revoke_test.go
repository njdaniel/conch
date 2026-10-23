package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestDurationConsts: the bounds the run holds the server to are read from
// its source, in the two forms it writes them.
func TestDurationConsts(t *testing.T) {
	src := `package x

import "time"

const (
	a = 1500 * time.Millisecond
	b = time.Second
	c = 30 * time.Second
	d = "not a duration"
	e = 3 * other.Second
)
`
	got, err := durationConsts(src)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]time.Duration{"a": 1500 * time.Millisecond, "b": time.Second, "c": 30 * time.Second} {
		if got[name] != want {
			t.Errorf("%s = %s, want %s", name, got[name], want)
		}
	}
	for _, name := range []string{"d", "e"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s was read as a duration", name)
		}
	}
}

// TestServerBoundsReadable: the bounds the run reads from this repository's
// server are there and positive, so a rename fails the unit tests as well as
// the program.
func TestServerBoundsReadable(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if b, err := readVoiceBounds(repo); err != nil || b.total() <= 0 {
		t.Errorf("voice bounds %s, %v", b, err)
	}
	if d, err := readSweepFallback(repo); err != nil || d <= 0 {
		t.Errorf("sweeper fallback %s, %v", d, err)
	}
	if d, err := recheckBound(repo); err != nil || d <= 0 {
		t.Errorf("socket re-check bound %s, %v", d, err)
	}
}
