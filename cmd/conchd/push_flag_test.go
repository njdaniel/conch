package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/push"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// TestParseServeArgsPush: push is off unless asked for, by flag or
// environment; the relaxing flag given alone is a mistake, as
// --metrics-allow-remote is; an unreadable switch is an error, not unset.
func TestParseServeArgsPush(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		wantWebPush bool
		wantPrivate bool
		wantErr     string
	}{
		{name: "off by default"},
		{name: "flag", args: []string{"--push-webpush"}, wantWebPush: true},
		{name: "env", env: map[string]string{"CONCHD_PUSH_WEBPUSH": "1"}, wantWebPush: true},
		{name: "env off", env: map[string]string{"CONCHD_PUSH_WEBPUSH": "0"}},
		{name: "flag wins over env", args: []string{"--push-webpush=false"}, env: map[string]string{"CONCHD_PUSH_WEBPUSH": "1"}},
		{name: "allow private", args: []string{"--push-webpush", "--push-webpush-allow-private"}, wantWebPush: true, wantPrivate: true},
		{name: "allow private by env", args: []string{"--push-webpush"}, env: map[string]string{"CONCHD_PUSH_WEBPUSH_ALLOW_PRIVATE": "1"}, wantWebPush: true, wantPrivate: true},
		{name: "both by env", env: map[string]string{"CONCHD_PUSH_WEBPUSH": "1", "CONCHD_PUSH_WEBPUSH_ALLOW_PRIVATE": "1"}, wantWebPush: true, wantPrivate: true},
		{name: "allow private alone", args: []string{"--push-webpush-allow-private"}, wantErr: "needs --push-webpush"},
		{name: "allow private alone by env", env: map[string]string{"CONCHD_PUSH_WEBPUSH_ALLOW_PRIVATE": "1"}, wantErr: "needs --push-webpush"},
		{name: "webpush env unreadable", env: map[string]string{"CONCHD_PUSH_WEBPUSH": "maybe"}, wantErr: "CONCHD_PUSH_WEBPUSH"},
		{name: "allow private env unreadable", env: map[string]string{"CONCHD_PUSH_WEBPUSH_ALLOW_PRIVATE": "maybe"}, wantErr: "CONCHD_PUSH_WEBPUSH_ALLOW_PRIVATE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONCHD_PUSH_WEBPUSH", "")
			t.Setenv("CONCHD_PUSH_WEBPUSH_ALLOW_PRIVATE", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts.pushWebPush != tt.wantWebPush || opts.pushAllowPrivate != tt.wantPrivate {
				t.Errorf("push = %v private %v, want %v private %v", opts.pushWebPush, opts.pushAllowPrivate, tt.wantWebPush, tt.wantPrivate)
			}
		})
	}
}

// TestParseServeArgsPushSending: the sender's three settings have defaults,
// environment twins, flags that win over them, and are refused when they are
// out of range or unreadable, whether or not push is on.
func TestParseServeArgsPushSending(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		env         map[string]string
		wantSubject string
		wantMax     int
		wantExpire  time.Duration
		wantErr     string
	}{
		{name: "defaults", wantMax: 600, wantExpire: 24 * time.Hour},
		{name: "flags", args: []string{"--push-vapid-subject", "mailto:ops@example.org", "--push-max-per-minute", "60", "--push-expire", "2h"},
			wantSubject: "mailto:ops@example.org", wantMax: 60, wantExpire: 2 * time.Hour},
		{name: "https subject", args: []string{"--push-vapid-subject=https://example.org/c"}, wantSubject: "https://example.org/c", wantMax: 600, wantExpire: 24 * time.Hour},
		{name: "env twins", env: map[string]string{"CONCHD_PUSH_VAPID_SUBJECT": "mailto:a@b.c", "CONCHD_PUSH_MAX_PER_MINUTE": "5", "CONCHD_PUSH_EXPIRE": "672h"},
			wantSubject: "mailto:a@b.c", wantMax: 5, wantExpire: 672 * time.Hour},
		{name: "flags win", args: []string{"--push-max-per-minute=7", "--push-expire=1h"}, env: map[string]string{"CONCHD_PUSH_MAX_PER_MINUTE": "5", "CONCHD_PUSH_EXPIRE": "672h"},
			wantMax: 7, wantExpire: time.Hour},
		{name: "expire past 28 days", args: []string{"--push-expire", "673h"}, wantErr: "--push-expire"},
		{name: "expire under a second", args: []string{"--push-expire", "500ms"}, wantErr: "--push-expire"},
		{name: "expire one second", args: []string{"--push-expire", "1s"}, wantMax: 600, wantExpire: time.Second},
		{name: "expire zero", args: []string{"--push-expire", "0"}, wantErr: "--push-expire"},
		{name: "expire negative", args: []string{"--push-expire=-1h"}, wantErr: "--push-expire"},
		{name: "expire env unreadable", env: map[string]string{"CONCHD_PUSH_EXPIRE": "soon"}, wantErr: "CONCHD_PUSH_EXPIRE"},
		{name: "budget zero", args: []string{"--push-max-per-minute", "0"}, wantErr: "--push-max-per-minute"},
		{name: "budget negative", args: []string{"--push-max-per-minute=-3"}, wantErr: "--push-max-per-minute"},
		{name: "budget env unreadable", env: map[string]string{"CONCHD_PUSH_MAX_PER_MINUTE": "many"}, wantErr: "CONCHD_PUSH_MAX_PER_MINUTE"},
		{name: "subject http", args: []string{"--push-vapid-subject", "http://example.org"}, wantErr: "--push-vapid-subject"},
		{name: "subject bare address", args: []string{"--push-vapid-subject", "ops@example.org"}, wantErr: "--push-vapid-subject"},
		{name: "subject with a space", args: []string{"--push-vapid-subject", "mailto:a b@c.d"}, wantErr: "--push-vapid-subject"},
		{name: "subject env bad", env: map[string]string{"CONCHD_PUSH_VAPID_SUBJECT": "tel:123"}, wantErr: "--push-vapid-subject"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, k := range []string{"CONCHD_PUSH_VAPID_SUBJECT", "CONCHD_PUSH_MAX_PER_MINUTE", "CONCHD_PUSH_EXPIRE", "CONCHD_PUSH_WEBPUSH", "CONCHD_PUSH_WEBPUSH_ALLOW_PRIVATE"} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			opts, err := parseServeArgs(append([]string{"--data", t.TempDir()}, tt.args...))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if opts.pushSubject != tt.wantSubject || opts.pushMaxPerMinute != tt.wantMax || opts.pushExpire != tt.wantExpire {
				t.Errorf("subject %q max %d expire %s, want %q %d %s", opts.pushSubject, opts.pushMaxPerMinute, opts.pushExpire, tt.wantSubject, tt.wantMax, tt.wantExpire)
			}
			cfg, err := startPush(context.Background(), openPushStore(t), serveOptions{dataDir: t.TempDir(), pushWebPush: true,
				pushSubject: opts.pushSubject, pushMaxPerMinute: opts.pushMaxPerMinute, pushExpire: opts.pushExpire}, mustKey(t), time.Now())
			if err != nil || cfg.VAPIDSubject != tt.wantSubject || cfg.MaxPerMinute != tt.wantMax || cfg.Expire != tt.wantExpire {
				t.Errorf("config = %+v, %v", cfg, err)
			}
		})
	}
}

func mustKey(t *testing.T) *push.VAPIDKey {
	t.Helper()
	key, err := push.LoadOrCreateVAPIDKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// The usage text lists both flags and both environment variables.
func TestUsageNamesPushFlags(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "usage")
	if err != nil {
		t.Fatal(err)
	}
	usage(f)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(f); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--push-webpush", "CONCHD_PUSH_WEBPUSH=1", "--push-webpush-allow-private", "CONCHD_PUSH_WEBPUSH_ALLOW_PRIVATE=1", "push-vapid-key.pem",
		"--push-vapid-subject", "CONCHD_PUSH_VAPID_SUBJECT", "--push-max-per-minute", "CONCHD_PUSH_MAX_PER_MINUTE", "--push-expire", "CONCHD_PUSH_EXPIRE"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("usage does not mention %q", want)
		}
	}
}

func openPushStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestStartPush: with push off no key is made and nothing is touched; with it
// on the key is made once; a replaced key ends the webpush registrations as
// stale, audited.
func TestStartPush(t *testing.T) {
	ctx := context.Background()
	t.Run("off creates no key file", func(t *testing.T) {
		dir := t.TempDir()
		cfg, err := startPush(ctx, openPushStore(t), serveOptions{dataDir: dir}, nil, time.Now())
		if err != nil || cfg.WebPush || cfg.VAPID != nil {
			t.Fatalf("cfg = %+v, %v", cfg, err)
		}
		if _, err := os.Stat(filepath.Join(dir, push.KeyFileName)); !os.IsNotExist(err) {
			t.Errorf("a key file exists with push off: %v", err)
		}
	})
	t.Run("a replaced key expires the webpush registrations", func(t *testing.T) {
		st := openPushStore(t)
		dir := t.TempDir()
		opts := serveOptions{dataDir: dir, pushWebPush: true, pushAllowPrivate: true}
		key, err := push.LoadOrCreateVAPIDKey(dir)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := startPush(ctx, st, opts, key, time.Now())
		if err != nil || !cfg.WebPush || !cfg.AllowPrivate || cfg.VAPID != key {
			t.Fatalf("cfg = %+v, %v", cfg, err)
		}
		human, err := st.CreatePrincipal(ctx, store.PrincipalHuman, "nick")
		if err != nil {
			t.Fatal(err)
		}
		cred, _, err := st.CreateCredential(ctx, "system", human.ID, "phone", nil)
		if err != nil {
			t.Fatal(err)
		}
		reg, err := st.CreatePushRegistration(ctx, cred.ID, store.PushTarget{Transport: schema.PushTransportWebPush,
			Platform: schema.PushPlatformWeb, App: "a", Endpoint: "https://x.example/1", P256DH: "p", Auth: "a"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		// The same key again: a restart changes nothing.
		if _, err := startPush(ctx, st, opts, key, time.Now()); err != nil {
			t.Fatal(err)
		}
		if regs, _ := st.ListOwnPushRegistrations(ctx, cred.ID); len(regs) != 1 {
			t.Fatalf("a restart ended the registration")
		}
		// A new key file: the registration is dead.
		if err := os.Remove(filepath.Join(dir, push.KeyFileName)); err != nil {
			t.Fatal(err)
		}
		fresh, err := push.LoadOrCreateVAPIDKey(dir)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Fingerprint() == key.Fingerprint() {
			t.Fatal("the new key is the old one")
		}
		if _, err := startPush(ctx, st, opts, fresh, time.Now()); err != nil {
			t.Fatal(err)
		}
		if regs, _ := st.ListOwnPushRegistrations(ctx, cred.ID); len(regs) != 0 {
			t.Errorf("the registration survived a replaced key: %+v", regs)
		}
		found := false
		events, _ := st.ListAuditEvents(ctx, 0, 100)
		for _, e := range events {
			if e.Action == schema.AuditActionPushRegistrationEnded && strings.Contains(e.Detail, reg.ID) && strings.HasSuffix(e.Detail, "reason=stale") {
				found = true
			}
		}
		if !found {
			t.Error("no push_registration_ended reason=stale row")
		}
	})
}

// TestServeRefusesAnUnusableKey: a key file anyone can read stops serve with
// one error that names the file, before anything is opened or served.
func TestServeRefusesAnUnusableKey(t *testing.T) {
	t.Setenv("CONCHD_PUSH_WEBPUSH", "")
	t.Setenv("CONCHD_PUSH_WEBPUSH_ALLOW_PRIVATE", "")
	dir := t.TempDir()
	path := filepath.Join(dir, push.KeyFileName)
	if err := os.WriteFile(path, []byte("secret-contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // #nosec G302 -- the test makes the file too open on purpose
		t.Fatal(err)
	}
	err := runServe([]string{"--data", dir, "--listen", "127.0.0.1:0", "--push-webpush"})
	if err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "secret-contents") {
		t.Fatalf("error = %v, want one naming %s and none of its contents", err, path)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "conch.db")); !os.IsNotExist(statErr) {
		t.Errorf("the database was opened before the key was refused: %v", statErr)
	}
}

// --push-webpush-allow-private is warned about at start, as
// --metrics-allow-remote is; without it there is no such warning.
func TestStartPushWarnsOfAllowPrivate(t *testing.T) {
	for _, private := range []bool{false, true} {
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
		dir := t.TempDir()
		key, err := push.LoadOrCreateVAPIDKey(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = startPush(context.Background(), openPushStore(t), serveOptions{dataDir: dir, pushWebPush: true, pushAllowPrivate: private}, key, time.Now())
		slog.SetDefault(prev)
		if err != nil {
			t.Fatal(err)
		}
		warned := strings.Contains(buf.String(), "level=WARN") && strings.Contains(buf.String(), "allow-private")
		if warned != private {
			t.Errorf("allow-private %v: warned %v in %q", private, warned, buf.String())
		}
	}
}
