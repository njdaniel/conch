package main

import (
	"bytes"
	"context"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// lockedBuffer is a buffer the log package and a test may share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// installRequestLogging keeps the log's format (a timestamp, the level, the
// message, its attributes: what scripts that read conchd's log search for),
// adds request_id to a request's lines, does not loop through the log
// package, and does not wrap twice.
func TestInstallRequestLogging(t *testing.T) {
	prevSlog, prevOut, prevFlags := slog.Default(), log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(prevSlog)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	var out lockedBuffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		installRequestLogging(&out)
		installRequestLogging(&out)
		slog.Info("plain line", "k", "v")
		log.Print("from the log package")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging deadlocked")
	}

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Config{AuthMode: server.AuthOff, Version: "v0", Listen: "127.0.0.1:0"}, st)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	id := rec.Header().Get(schema.RequestIDHeader)

	got := out.String()
	stamp := `\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} `
	for _, want := range []string{
		stamp + `INFO plain line k=v\n`,
		stamp + `from the log package\n`,
		stamp + `ERROR healthz: store ping failed .*request_id=` + id + `\n`,
	} {
		if !regexp.MustCompile(want).MatchString(got) {
			t.Errorf("the log lacks a line like %q:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "request_id="); n != 1 {
		t.Errorf("request_id appears %d times, want once:\n%s", n, got)
	}
}
