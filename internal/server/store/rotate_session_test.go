package store

import (
	"context"
	"testing"
)

// Rotating a credential keeps what kind it is (issue #866 review): an
// operator's browser session stays a session with its idle bound, so the
// recent-auth gate and the end of password-only still reach it; a member's
// renewable sign-in stays renewable; a script credential stays a script
// credential. None carries a recent-auth proof over.
func TestRotateCredentialKeepsTheSessionClass(t *testing.T) {
	ctx := context.Background()
	w := newModeWorld(t)
	sessionID, _ := w.session(t, w.op.ID)
	memberID, _ := mustMemberLogin(t, w)
	script, _, err := w.s.CreateCredential(ctx, "system", w.op.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name                     string
		id                       int64
		session, idle, renewable bool
	}{
		{"an operator's browser session", sessionID, true, true, false},
		{"a member's password sign-in", memberID, true, false, true},
		{"a script credential", script.ID, false, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rotated, token, err := w.s.RotateCredential(ctx, "system", tt.id)
			if err != nil {
				t.Fatalf("RotateCredential = %v", err)
			}
			if (rotated.IdleExpiresAt != nil) != tt.idle || rotated.Renews != tt.renewable {
				t.Errorf("rotated = %+v, want idle bound %v, renews %v", rotated, tt.idle, tt.renewable)
			}
			_, cred, err := w.s.ResolveRequestCredential(ctx, token)
			if err != nil {
				t.Fatal(err)
			}
			if cred.Session != tt.session || cred.IdleBounded != tt.idle || cred.RecentAuthUntil != nil {
				t.Errorf("the rotated credential resolves as %+v, want session %v, idle-bounded %v, no proof", cred, tt.session, tt.idle)
			}
			if tt.idle && rotated.ExpiresAt != nil && rotated.IdleExpiresAt.Time().After(rotated.ExpiresAt.Time()) {
				t.Errorf("idle bound %v is past the expiry %v", rotated.IdleExpiresAt, rotated.ExpiresAt)
			}
		})
	}
}

// The regression behind it: a password-only operator signs in, proves the
// password, rotates the session; when password-only stops being allowed, the
// rotated session is revoked like any other, and a real script credential is
// not.
func TestRotatedSessionEndsWithPasswordOnly(t *testing.T) {
	ctx := context.Background()
	w := newModeWorld(t)
	w.start(t, normalOptional)
	olga, err := w.s.IssuePasswordOnlyOperatorSession(ctx, w.issue(t, w.olga), w.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.RecordPasswordOnlyOperatorRecentAuth(ctx, w.issue(t, w.olga), olga.Credential.ID, w.now); err != nil {
		t.Fatal(err)
	}
	_, rotatedTok, err := w.s.RotateCredential(ctx, "system", olga.Credential.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, scriptTok, err := w.s.CreateCredential(ctx, "system", w.olga.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res := w.start(t, normalRequired); res.SessionsRevoked != 1 {
		t.Errorf("StartMode revoked %d sessions, want the rotated one", res.SessionsRevoked)
	}
	if w.live(t, rotatedTok) {
		t.Error("the rotated session survived the end of password-only")
	}
	if !w.live(t, scriptTok) {
		t.Error("the script credential did not survive the end of password-only")
	}
}
