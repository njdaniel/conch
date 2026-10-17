package web

import (
	"io/fs"
	"testing"
)

// embedded is true in a build with -tags conchweb (embed_test.go).
func TestFiles(t *testing.T) {
	fsys, ok := Files()
	if ok != embedded {
		t.Fatalf("Files() ok = %v, want %v", ok, embedded)
	}
	if !ok {
		if fsys != nil {
			t.Error("Files() returned a file system with ok false")
		}
		return
	}
	fi, err := fs.Stat(fsys, "index.html")
	if err != nil {
		t.Fatalf("the embedded client has no index.html at its root: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("index.html is %v", fi.Mode())
	}
}
