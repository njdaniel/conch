package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Restoring a backup (issue #716, docs/design/guests-and-accounts.md §4.2,
// §7.3). The store's tests cover each thing the restore invalidates; these
// run the whole of it against the API: a backup taken while people were
// signed in, restored after they signed out, is served only after conchd
// restore's transaction, and then nothing in it signs anyone in.

// backupOf is the fixture's database as it is now, in a file of its own,
// as an online backup takes it.
func backupOf(t *testing.T, f *loginFixture) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "conch.db")
	f.exec(t, "VACUUM INTO ?", path)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// serveRestored does what a restore does to the backup at path before the
// server takes traffic, conchd restore and then the first start, and puts
// a server on it in the fixture's place, with the fixture's configuration
// and clock.
func serveRestored(t *testing.T, f *loginFixture, path string) store.RestoreResult {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	res, err := st.InvalidateRestored(ctx, f.srv.now())
	if err != nil {
		t.Fatalf("InvalidateRestored: %v", err)
	}
	if _, restored, err := st.StartRestored(ctx, f.srv.now()); err != nil || !restored {
		t.Fatalf("StartRestored = %v, %v", restored, err)
	}
	srv := New(f.srv.cfg, st)
	srv.now = f.srv.now
	srv.setInviteLimits(looseInviteLimits())
	srv.setLoginLimits(looseLoginLimits())
	testDBPaths.Store(srv, path)
	f.srv = srv
	return res
}

// The whole chain over REST. Before the backup: the operator is enrolled and
// signed in, a member is signed in, a guest is in a channel and has posted,
// and a reset link is open. After it, on the live server: the guest ends its
// visit, the member's session is revoked, the link is used and the operator
// signs in with a later code. The backup is restored: every one of those
// credentials and the link fail, and no code that may have been used is
// accepted; the member signs in again with the password the backup holds
// and reads the guest's post under its guest attribution; the operator
// signs in with the next code.
func TestRestoreInvalidatesOverREST(t *testing.T) {
	f := newOperatorFixture(t)
	ctx := context.Background()
	secret := f.enrolOperator(t, f.root.ID)
	_, opSession := f.operatorSession(t, secret)

	rec := f.login(t, "alice", alicePassword, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's sign-in = %d %s", rec.Code, rec.Body)
	}
	aliceSession := decodeBody[schema.LoginResponseV1](t, rec)
	g := newGuest(t, f.srv, "Pat", time.Hour, 24*time.Hour, f.red.ID)
	if rec := f.send(t, "POST", "/v2/channels/red/messages", g.Token, `{"body":"a guest was here"}`, ""); rec.Code != http.StatusCreated {
		t.Fatalf("the guest's post = %d %s", rec.Code, rec.Body)
	}
	link, err := f.srv.store.CreateSignInLink(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, f.alice.ID,
		schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurposeReset}, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}

	path := backupOf(t, f.loginFixture)

	// After the backup, on the live server.
	if rec := f.send(t, "POST", "/v1/guest/end", g.Token, "", ""); rec.Code != http.StatusOK {
		t.Fatalf("end visit = %d %s", rec.Code, rec.Body)
	}
	if err := f.srv.store.RevokeCredential(ctx, "system", aliceSession.Credential.ID); err != nil {
		t.Fatal(err)
	}
	const later = "a fresh passphrase chosen after the backup"
	if rec := f.send(t, "POST", "/v1/password/reset", "", inviteJSON(t, schema.ResetPasswordRequestV1{Code: link.Code, Password: later}), ""); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body)
	}
	usedCode := f.code(secret) // the code the live server is about to take
	if rec := f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), usedCode, ""); rec.Code != http.StatusOK {
		t.Fatalf("the operator's later sign-in = %d %s", rec.Code, rec.Body)
	}

	res := serveRestored(t, f.loginFixture, path)
	if len(res.GuestsEnded) != 1 || res.GuestsEnded[0].PrincipalID != g.Principal.ID {
		t.Errorf("the restore ended %+v, want the guest", res.GuestsEnded)
	}

	for what, tok := range map[string]string{
		"the operator's script credential": f.rootTok,
		"the operator's browser session":   opSession,
		"the member's session":             aliceSession.Token,
		"the member's script credential":   f.aliceTok,
		"the agent's credential":           f.botTok,
		"the guest's credential":           g.Token,
	} {
		if got := f.status(t, tok); got != http.StatusUnauthorized {
			t.Errorf("%s = %d on the restored server, want 401", what, got)
		}
	}
	if rec := f.send(t, "POST", "/v1/password/reset", "", inviteJSON(t, schema.ResetPasswordRequestV1{Code: link.Code, Password: later}), ""); rec.Code == http.StatusOK {
		t.Errorf("the reset link worked on the restored server: %s", rec.Body)
	}
	v, err := f.srv.store.GuestVisitOf(ctx, g.Principal.ID)
	if err != nil || v.State != schema.GuestStateEnded || v.EndReason != schema.GuestEndRestore {
		t.Errorf("the visit = %+v, %v; want ended, reason restore", v, err)
	}

	// The member signs in with the password the backup holds, as on a new
	// device, and reads the guest's post as it was attributed.
	if rec := f.login(t, "alice", later, ""); rec.Code == http.StatusOK {
		t.Error("a password set after the backup signs in on the restored server")
	}
	rec = f.login(t, "alice", alicePassword, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("alice's sign-in on the restored server = %d %s", rec.Code, rec.Body)
	}
	back := decodeBody[schema.LoginResponseV1](t, rec)
	rec = f.send(t, "GET", "/v2/channels/red/messages", back.Token, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("messages = %d %s", rec.Code, rec.Body)
	}
	var found bool
	for _, m := range decodeBody[schema.ListMessagesResponseV2](t, rec).Messages {
		if m.Body == "a guest was here" {
			found = true
			if m.AuthorID != g.Principal.ID || m.GuestAuthor == nil || m.GuestAuthor.Handle != g.Visit.Handle || m.GuestAuthor.Label != "Pat" {
				t.Errorf("the guest's post = %+v, want its guest attribution", m)
			}
		}
	}
	if !found {
		t.Errorf("the guest's post is not on the restored server: %s", rec.Body)
	}

	// The operator's password and authenticator are kept, but the code the
	// live server took after the backup, and every code up to now and the
	// window after it, is refused; two steps on, the current one works.
	for _, code := range []string{usedCode, f.code(secret), totpCodeAt(secret, totpStep(f.srv.now())+schema.TOTPWindowSteps)} {
		assertErrorBody(t, f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), code, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	}
	f.advance(2 * schema.TOTPPeriodSeconds * time.Second)
	if rec := f.completeLogin(t, f.rootChallenge(t, rootPassword, ""), f.code(secret), ""); rec.Code != http.StatusOK {
		t.Errorf("the operator's sign-in two steps after the start = %d %s", rec.Code, rec.Body)
	}
}

// Run on a server that is serving, the restore closes what is open at once:
// every socket of every principal, the guest's and the members' alike,
// within the bound of any other revocation, although the server would not
// re-check those credentials for an hour.
func TestRestoreOnARunningServerClosesSockets(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.srv.credRecheckInterval = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	g := newGuest(t, f.srv, "", time.Hour, time.Hour, f.ops.ID)
	base := wsTestServer(t, f.srv)
	type open struct {
		who, path string
		log       *frameLog
	}
	var sockets []open
	for _, s := range []struct{ who, token, path string }{
		{"guest", g.Token, "/v2/ws?channel=ops"},
		{"guest", g.Token, "/v1/events/ws?channel=ops"},
		{"ann", f.tokens["ann"], "/v2/ws?channel=ops"},
		{"ann", f.tokens["ann"], "/v1/events/ws?channel=ops"},
		{"robo", f.tokens["robo"], "/v2/ws?channel=ops"},
	} {
		sockets = append(sockets, open{s.who, s.path, recordFrames(dialAs(t, ctx, base, s.path, s.token))})
	}
	start := time.Now()
	if _, err := f.srv.store.InvalidateRestored(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, s := range sockets {
		select {
		case <-s.log.done:
		case <-time.After(guestSocketBound + time.Second):
			t.Fatalf("%s's %s stayed open %v after the restore", s.who, s.path, time.Since(start))
		}
	}
	if took := time.Since(start); took > guestSocketBound {
		t.Errorf("the sockets closed %v after the restore, bound %v", took, guestSocketBound)
	}
	for who, tok := range map[string]string{"guest": g.Token, "ann": f.tokens["ann"], "root": f.rootTok} {
		if rec := f.do(t, "GET", "/v1/whoami", tok, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s's credential = %d after the restore, want 401", who, rec.Code)
		}
	}
	if v, err := f.srv.store.GuestVisitOf(context.Background(), g.Principal.ID); err != nil || v.EndReason != schema.GuestEndRestore {
		t.Errorf("the visit = %+v, %v", v, err)
	}
}

// roomOf is a room name of the shape store.IsVoiceRoomName accepts for
// prefix: the prefix and 26 characters of lower-case base32, which c fills.
func roomOf(prefix string, c byte) string { return prefix + strings.Repeat(string(c), 26) }

// The first start on restored data ends every media session LiveKit holds
// for this instance (ClearVoiceRooms): each room of the exact shape this
// instance names its rooms is deleted. A room of another instance on the
// same LiveKit is not, even one whose prefix begins with this instance's
// ("alpha-2-" beside "alpha-"), nor a name with this prefix and any other
// shape. A LiveKit that cannot list its rooms, or refuses a delete, is an
// error the caller sees, and with voice off there is nothing to do.
func TestClearVoiceRooms(t *testing.T) {
	ctx := context.Background()
	t.Run("deletes this instance's rooms only", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		if err := f.srv.store.SetVoiceRoomPrefix("alpha-"); err != nil {
			t.Fatal(err)
		}
		mine := []string{roomOf("alpha-", 'a'), roomOf("alpha-", '7')}
		others := []string{
			roomOf("alpha-2-", 'a'),            // a nested prefix's room
			roomOf("beta-", 'a'),               // another instance's room
			"alpha-" + strings.Repeat("a", 25), // too short
			"alpha-" + strings.Repeat("A", 26), // not lower-case base32
			"alpha-" + strings.Repeat("1", 26), // 1 is not base32
		}
		for i, r := range append(append([]string{}, mine...), others...) {
			f.lk.setRoom(r, fakeParticipant{identity: fmt.Sprintf("p%d", i)})
		}
		n, err := f.srv.ClearVoiceRooms(ctx)
		if err != nil || n != len(mine) {
			t.Fatalf("ClearVoiceRooms = %d, %v; want %d", n, err, len(mine))
		}
		for _, r := range mine {
			if f.lk.has(r) {
				t.Errorf("%s is still in LiveKit", r)
			}
		}
		for _, r := range others {
			if !f.lk.has(r) || f.lk.callsFor("DeleteRoom", r) != 0 {
				t.Errorf("%s, not this instance's room, was deleted", r)
			}
		}
	})
	t.Run("LiveKit cannot list", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		f.lk.setRoom(roomOf(f.srv.store.VoiceRoomPrefix(), 'b'), fakeParticipant{identity: "p1"})
		f.lk.setListStatus(http.StatusServiceUnavailable)
		if n, err := f.srv.ClearVoiceRooms(ctx); err == nil || n != 0 {
			t.Errorf("ClearVoiceRooms with LiveKit failing = %d, %v; want an error", n, err)
		}
	})
	t.Run("a delete refused", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		f.lk.setRoom(roomOf(f.srv.store.VoiceRoomPrefix(), 'c'), fakeParticipant{identity: "p1"})
		f.lk.setDeleteStatus(http.StatusInternalServerError)
		if _, err := f.srv.ClearVoiceRooms(ctx); err == nil {
			t.Error("a refused delete was not reported")
		}
	})
	t.Run("voice not configured", func(t *testing.T) {
		srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired})
		if n, err := srv.ClearVoiceRooms(ctx); err != nil || n != 0 {
			t.Errorf("ClearVoiceRooms without voice = %d, %v", n, err)
		}
	})
}
