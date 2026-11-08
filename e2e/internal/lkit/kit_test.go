package lkit

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// lkArchive is a tar.gz holding a file called lk.
func lkArchive(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("#!/bin/sh\n")
	if err := tw.WriteHeader(&tar.Header{Name: "lk", Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchLKRetry(t *testing.T) {
	good := lkArchive(t)
	sum := sha256.Sum256(good)
	goodSum := hex.EncodeToString(sum[:])

	tests := []struct {
		name      string
		answers   []int // status per request; the last repeats
		pin       string
		wantErr   bool
		unreach   bool
		checksum  bool
		wantCalls int32
	}{
		{name: "503 then 200", answers: []int{503, 200}, pin: goodSum, wantCalls: 2},
		{name: "three 5xx then 200", answers: []int{502, 500, 503, 200}, pin: goodSum, wantCalls: 4},
		{name: "four 5xx in a row exhaust", answers: []int{503}, pin: goodSum, wantErr: true, unreach: true, wantCalls: 4},
		{name: "404 is final", answers: []int{404}, pin: goodSum, wantErr: true, wantCalls: 1},
		{name: "bad checksum is final", answers: []int{200}, pin: "00", wantErr: true, checksum: true, wantCalls: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				i := int(calls.Add(1)) - 1
				if i >= len(tc.answers) {
					i = len(tc.answers) - 1
				}
				if tc.answers[i] != 200 {
					w.WriteHeader(tc.answers[i])
					return
				}
				_, _ = w.Write(good)
			}))
			defer srv.Close()
			stubFetch(t, srv.URL, tc.pin, []time.Duration{0, 0, 0})

			path, err := FetchLK(context.Background(), t.TempDir())
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("requests = %d, want %d", got, tc.wantCalls)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("FetchLK: %v", err)
				}
				if _, err := os.Stat(path); err != nil {
					t.Errorf("binary missing: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("FetchLK succeeded, want an error")
			}
			if got := errors.Is(err, ErrUnreachable); got != tc.unreach {
				t.Errorf("errors.Is(ErrUnreachable) = %v, want %v (%v)", got, tc.unreach, err)
			}
			var ce *ChecksumError
			if got := errors.As(err, &ce); got != tc.checksum {
				t.Errorf("is ChecksumError = %v, want %v (%v)", got, tc.checksum, err)
			}
		})
	}
}

func TestFetchLKCancelledDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	stubFetch(t, srv.URL, "00", []time.Duration{time.Hour, time.Hour, time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for calls.Load() == 0 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	done := make(chan error, 1)
	go func() { _, err := FetchLK(ctx, t.TempDir()); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FetchLK did not stop on cancellation")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

// stubFetch points FetchLK at base with the given pin and backoff, until the
// test ends.
func stubFetch(t *testing.T, base, pin string, backoff []time.Duration) {
	t.Helper()
	key := runtime.GOOS + "_" + runtime.GOARCH
	oldURL, oldBackoff, oldSum, had := lkURL, lkBackoff, lkSHA256[key], false
	_, had = lkSHA256[key]
	lkURL = func(string) string { return base }
	lkBackoff = backoff
	lkSHA256[key] = pin
	t.Cleanup(func() {
		lkURL, lkBackoff = oldURL, oldBackoff
		if had {
			lkSHA256[key] = oldSum
		} else {
			delete(lkSHA256, key)
		}
	})
}
