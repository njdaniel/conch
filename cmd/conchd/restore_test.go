package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// restoredDir is a data directory as the restore procedure leaves it before
// conchd restore runs (issue #716): the backup unpacked, with the operator's
// script credential and a guest's visit live in it, and restorePendingFile
// put there first.
func restoredDir(t *testing.T) (dir, opTok, guestTok string) {
	t.Helper()
	t.Setenv("CONCHD_DATA", "")
	dir = t.TempDir()
	var out, errOut bytes.Buffer
	if err := runBootstrapOperator([]string{"--data", dir, "--name", "root"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	opTok = strings.TrimSpace(out.String())
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	g, err := st.CreateGuest(ctx, "system", store.NewGuest{Origin: schema.GuestOriginAddress, Idle: time.Hour, Absolute: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, restorePendingFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, opTok, g.Token
}

func runRestoreCmd(args ...string) (string, string, error) {
	var out, errOut bytes.Buffer
	err := runRestore(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

func liveIn(t *testing.T, dir, token string) bool {
	t.Helper()
	st := openData(t, dir)
	_, err := st.ResolveCredential(context.Background(), token)
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrCredentialInvalid):
		return false
	default:
		t.Fatal(err)
		return false
	}
}

// serve will not start on a directory a restore has not finished with, and
// says what to run; conchd restore makes the backup safe, says what it did
// without a secret, removes the file, and after it serve's check passes.
// Running it again is harmless.
func TestRestoreCommand(t *testing.T) {
	dir, opTok, guestTok := restoredDir(t)
	err := runServe([]string{"--data", dir, "--listen", "127.0.0.1:0", "--no-web"})
	if err == nil || !strings.Contains(err.Error(), "conchd restore --data "+dir) {
		t.Fatalf("serve on an unfinished restore = %v, want a refusal naming conchd restore", err)
	}
	if !liveIn(t, dir, opTok) || !liveIn(t, dir, guestTok) {
		t.Fatal("the backup's credentials are not live before the restore; the test proves nothing")
	}

	out, errOut, err := runRestoreCmd("--data", dir)
	if err != nil {
		t.Fatalf("restore = %v (%s)", err, errOut)
	}
	for _, want := range []string{"signed out 2 principal(s)", "ended 1 guest visit(s)", "deleted 0 setup token(s)", "sign in again",
		"invites are paused: unpause after review", "guest joining is paused: unpause after review", "0 webhook hook(s) are live"} {
		if !strings.Contains(out, want) {
			t.Errorf("restore printed %q, want %q in it", out, want)
		}
	}
	for _, secret := range []string{opTok, guestTok} {
		if strings.Contains(out+errOut, secret) {
			t.Error("restore printed a token")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, restorePendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s is still there after the restore: %v", restorePendingFile, err)
	}
	if err := refuseUnfinishedRestore("serve", dir); err != nil {
		t.Errorf("serve's check after the restore = %v", err)
	}
	if liveIn(t, dir, opTok) || liveIn(t, dir, guestTok) {
		t.Error("a credential from the backup works after the restore")
	}

	// Again, as after an interruption nobody saw the end of: the file is
	// gone, so it takes --force.
	if _, _, err := runRestoreCmd("--data", dir); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Errorf("a second restore without restore-pending = %v, want a refusal naming --force", err)
	}
	if out, _, err := runRestoreCmd("--data", dir, "--force"); err != nil || !strings.Contains(out, "ended 0 guest visit(s)") {
		t.Errorf("the second restore = %v, %q", err, out)
	}
}

// A restore that fails leaves the directory unservable: nothing in the
// database changed, the file is still there and serve still refuses. Run
// again once the cause is gone, it finishes.
func TestRestoreCommandFailureLeavesItUnservable(t *testing.T) {
	dir, opTok, _ := restoredDir(t)
	exec := func(query string) {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "conch.db")+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TRIGGER fail_restore BEFORE INSERT ON audit_events WHEN NEW.action = 'restore_invalidated'
		BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	_, _, err := runRestoreCmd("--data", dir)
	if err == nil || !strings.Contains(err.Error(), "must not be served") {
		t.Fatalf("a failing restore = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, restorePendingFile)); err != nil {
		t.Errorf("%s is gone after a failed restore: %v", restorePendingFile, err)
	}
	if err := refuseUnfinishedRestore("serve", dir); err == nil {
		t.Error("serve's check passes after a failed restore")
	}
	if !liveIn(t, dir, opTok) {
		t.Error("a failed restore changed the database")
	}
	exec("DROP TRIGGER fail_restore")
	if _, _, err := runRestoreCmd("--data", dir); err != nil {
		t.Fatalf("the restore run again = %v", err)
	}
	if err := refuseUnfinishedRestore("serve", dir); err != nil || liveIn(t, dir, opTok) {
		t.Errorf("after the second run: serve's check %v, the old credential live %v", err, liveIn(t, dir, opTok))
	}
}

// The command refuses what it cannot be meant for, and creates nothing.
func TestRestoreCommandRefusals(t *testing.T) {
	t.Setenv("CONCHD_DATA", "")
	empty := t.TempDir()
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no --data", nil, "--data (or CONCHD_DATA) is required"},
		{"no restore-pending: a live directory, likely", []string{"--data", empty}, "add --force"},
		{"no database there", []string{"--data", empty, "--force"}, "no database at"},
		{"an argument", []string{"--data", empty, "extra"}, "unexpected argument"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := runRestoreCmd(tt.args...); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("restore %v = %v, want %q", tt.args, err, tt.want)
			}
		})
	}
	if entries, err := os.ReadDir(empty); err != nil || len(entries) != 0 {
		t.Errorf("a refused restore left %v in the directory (%v)", entries, err)
	}
}

// An unreadable marker check fails closed: serve does not start when it
// cannot tell whether a restore is unfinished.
func TestRestorePendingFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // #nosec G302 -- a directory, so TempDir can remove it
	if err := refuseUnfinishedRestore("serve", locked); err == nil {
		t.Error("serve's check passed on a directory it cannot read")
	}
}

// Run by mistake on a live data directory, which holds no restore-pending,
// conchd restore changes nothing; with --force it signs everyone out of it.
func TestRestoreCommandRefusesALiveDirectory(t *testing.T) {
	dir, opTok, _ := restoredDir(t)
	if err := os.Remove(filepath.Join(dir, restorePendingFile)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runRestoreCmd("--data", dir); err == nil || !strings.Contains(err.Error(), "nothing changed") {
		t.Fatalf("restore on a live directory = %v, want a refusal", err)
	}
	if !liveIn(t, dir, opTok) {
		t.Fatal("a refused restore signed someone out")
	}
	if _, _, err := runRestoreCmd("--data", dir, "--force"); err != nil {
		t.Fatalf("restore --force = %v", err)
	}
	if liveIn(t, dir, opTok) {
		t.Error("restore --force left a credential live")
	}
}

// The host commands that make an operator or a setup token refuse a
// directory whose restore has not finished: conchd restore would delete the
// token, and the database is not to be used before it.
func TestOperatorCommandsRefuseAnUnfinishedRestore(t *testing.T) {
	dir, _, _ := restoredDir(t)
	for _, tt := range []struct {
		name string
		run  func() error
	}{
		{"bootstrap-operator", func() error {
			return runBootstrapOperator([]string{"--data", dir, "--name", "other"}, &bytes.Buffer{}, &bytes.Buffer{})
		}},
		{"operator bootstrap", func() error { _, _, err := runOp(t, "bootstrap", "--data", dir, "other"); return err }},
		{"operator setup", func() error { _, _, err := runOp(t, "setup", "--data", dir, "1"); return err }},
		{"operator recover", func() error { _, _, err := runOp(t, "recover", "--data", dir, "1"); return err }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.run(); err == nil || !strings.Contains(err.Error(), "conchd restore --data "+dir) {
				t.Errorf("%s = %v, want a refusal naming conchd restore", tt.name, err)
			}
		})
	}
	var tokens int
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.QueryRow("SELECT COUNT(*) FROM setup_tokens").Scan(&tokens); err != nil || tokens != 0 {
		t.Errorf("%d setup tokens made on an unfinished restore (%v)", tokens, err)
	}
}
