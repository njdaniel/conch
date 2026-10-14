//go:build unix

package server

import (
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestStaticServesOnlyRegularFiles: a FIFO in the directory would block the
// request until something wrote to it. It is a 404, at once, and so is a
// symlink to one.
func TestStaticServesOnlyRegularFiles(t *testing.T) {
	dist := webFixture(t)
	if err := syscall.Mkfifo(filepath.Join(dist, "pipe.js"), 0o600); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	if err := os.Symlink("pipe.js", filepath.Join(dist, "pipelink.js")); err != nil {
		t.Fatal(err)
	}
	ts := webServer(t, AuthRequired, dist)
	// If the handler is ever blocked on the FIFO, opening the other end
	// releases it, so a failure here ends the test and does not hang the
	// server's shutdown. Registered after the server, so it runs first.
	t.Cleanup(func() {
		if w, err := os.OpenFile(filepath.Join(dist, "pipe.js"), os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil { // #nosec G304 -- a path under the test's own temp directory
			_ = w.Close()
		}
	})
	for _, p := range []string{"/app/pipe.js", "/app/pipelink.js"} {
		t.Run(p, func(t *testing.T) {
			done := make(chan int, 1)
			go func() {
				resp, _ := fetch(t, "GET", ts.URL+p)
				done <- resp.StatusCode
			}()
			select {
			case code := <-done:
				if code != http.StatusNotFound {
					t.Errorf("status = %d, want 404", code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the request is blocked on the FIFO")
			}
		})
	}
}
