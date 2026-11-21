package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// provenanceOf reads credential id's provenance both ways the server does:
// from the store by id (a ticket-authenticated socket) and, when the token
// is given, from the request resolution the middleware makes. The two must
// agree.
func provenanceOf(t *testing.T, s *Store, id int64, token string) bool {
	t.Helper()
	ctx := context.Background()
	self, err := s.CredentialSelfMade(ctx, id)
	if err != nil {
		t.Fatalf("CredentialSelfMade(%d) = %v", id, err)
	}
	if token != "" {
		_, cred, err := s.ResolveRequestCredential(ctx, token)
		if err != nil {
			t.Fatalf("ResolveRequestCredential = %v", err)
		}
		if cred.ID != id || cred.SelfMade != self {
			t.Fatalf("resolved credential %d self-made %v; the row %d says %v", cred.ID, cred.SelfMade, id, self)
		}
	}
	return self
}

// withRecentAuth gives credential id a recent-auth proof, as POST
// /v1/reauth would, so a device link can be made on it.
func withRecentAuth(t *testing.T, w *modeWorld, id int64) {
	t.Helper()
	w.exec(t, "UPDATE credentials SET recent_auth_until = ? WHERE id = ?", w.now.Add(time.Hour).UnixMilli(), id)
}

// deviceLinkCredential makes the member a device link on credential cred
// and redeems it, returning the credential the redemption issued.
func deviceLinkCredential(t *testing.T, w *modeWorld, cred int64) (int64, string) {
	t.Helper()
	ctx := context.Background()
	withRecentAuth(t, w, cred)
	link, err := w.s.CreateDeviceLink(ctx, principalActor(w.member.ID), w.member.ID, cred, schema.CreateDeviceLinkRequestV1{Label: "phone"}, w.now)
	if err != nil {
		t.Fatalf("CreateDeviceLink = %v", err)
	}
	r, err := w.s.RedeemSignInLink(ctx, HashSignInLinkCode(link.Code), w.now)
	if err != nil {
		t.Fatalf("RedeemSignInLink(device) = %v", err)
	}
	return r.Credential.ID, r.Token
}

// Every path that creates a credential records who made it (issue #2042):
// self for a sign-in or entry that proves the person is the one asking (a
// password sign-in, an operator's password-and-code sign-in, the owner's
// bootstrap, an invite redemption, a guest's own visit, a device link made
// on a self-made credential, and a rotation of one's own self-made session),
// and another for everything an act_as_members holder can make (an issued
// or rotated credential, a sign-in link, a device link made on such a
// credential, a password change on such a credential) and for the host-made
// setup credential.
func TestCredentialProvenanceByPath(t *testing.T) {
	ctx := context.Background()
	login := func(t *testing.T, w *modeWorld) (int64, string) {
		t.Helper()
		return mustMemberLogin(t, w)
	}
	actAs := func(t *testing.T, w *modeWorld) (int64, string) {
		t.Helper()
		c, tok, err := w.s.CreateCredential(ctx, principalActor(w.op.ID), w.member.ID, "act as", nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID, tok
	}
	changePassword := func(t *testing.T, w *modeWorld, cred int64) (int64, string) {
		t.Helper()
		issue := w.issue(t, w.member)
		up, err := w.s.ChangePassword(ctx, principalActor(w.member.ID), PasswordChange{PrincipalID: w.member.ID, CredentialID: cred,
			AuthGeneration: issue.AuthGeneration, Record: issue.Record, NewRecord: testRecord("wren-new")}, w.now)
		if err != nil {
			t.Fatalf("ChangePassword = %v", err)
		}
		if up.Token == "" {
			t.Fatal("ChangePassword rotated no session")
		}
		return up.Credential.ID, up.Token
	}
	// openJoining lets the bare address make accounts and guests, as a
	// server from before #1005 could.
	openJoining := func(t *testing.T, w *modeWorld) {
		t.Helper()
		if _, err := w.s.CreateChannel(ctx, "general"); err != nil {
			t.Fatal(err)
		}
		legacyJoinSettings(t, w.s, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolRef(true),
			OpenJoining: ptrMode(schema.OpenJoiningBoth), OpenChannels: &[]string{"general"}})
	}
	for _, tt := range []struct {
		name string
		make func(t *testing.T, w *modeWorld) (int64, string)
		self bool
	}{
		{"password sign-in", login, true},
		{"operator sign-in with password and code", func(t *testing.T, w *modeWorld) (int64, string) {
			session, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: w.challenge(t), AddressMatches: true, Check: stepCheck(10)}, w.now)
			if err != nil {
				t.Fatal(err)
			}
			return session.Credential.ID, session.Token
		}, true},
		{"owner's bootstrap", func(t *testing.T, w *modeWorld) (int64, string) {
			s := openTestStore(t)
			_, cred, tok, err := s.BootstrapOperator(ctx, "owner")
			if err != nil {
				t.Fatal(err)
			}
			w.s = s // read it where it was made
			return cred.ID, tok
		}, true},
		{"invite redemption", func(t *testing.T, w *modeWorld) (int64, string) {
			if _, err := w.s.CreateChannel(ctx, "general"); err != nil {
				t.Fatal(err)
			}
			inv, err := w.s.CreateInvite(ctx, principalActor(w.op.ID), w.op.ID,
				schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}, InviteOptions{}, w.now)
			if err != nil {
				t.Fatal(err)
			}
			r, err := w.s.RedeemInvite(ctx, HashInviteCode(inv.Code), "newcomer", w.now)
			if err != nil {
				t.Fatal(err)
			}
			return r.Credential.ID, r.Token
		}, true},
		{"guest's own visit", func(t *testing.T, w *modeWorld) (int64, string) {
			g, err := w.s.CreateGuest(ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress, Idle: time.Hour, Absolute: 2 * time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			return g.Credential.ID, g.Token
		}, true},
		{"joining as an account with a password", func(t *testing.T, w *modeWorld) (int64, string) {
			openJoining(t, w)
			a, err := w.s.JoinAsAccount(ctx, "newcomer", testRecord("n"), w.now)
			if err != nil {
				t.Fatal(err)
			}
			return a.Credential.ID, a.Token
		}, true},
		{"guest's conversion to an account", func(t *testing.T, w *modeWorld) (int64, string) {
			openJoining(t, w)
			g, err := w.s.CreateGuest(ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress, Idle: time.Hour, Absolute: 2 * time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			c, err := w.s.ConvertGuest(ctx, principalActor(g.Principal.ID), GuestConversion{PrincipalID: g.Principal.ID, Name: "pat", Record: testRecord("p")})
			if err != nil {
				t.Fatal(err)
			}
			return c.Credential.ID, c.Token
		}, true},
		{"device link made on a password sign-in", func(t *testing.T, w *modeWorld) (int64, string) {
			cred, _ := login(t, w)
			return deviceLinkCredential(t, w, cred)
		}, true},
		{"own session rotated by a password change", func(t *testing.T, w *modeWorld) (int64, string) {
			cred, _ := login(t, w)
			return changePassword(t, w, cred)
		}, true},

		{"act-as credential", actAs, false},
		{"act-as credential, renewable", func(t *testing.T, w *modeWorld) (int64, string) {
			c, tok, err := w.s.CreateRenewableCredential(ctx, principalActor(w.op.ID), w.member.ID, "act as", 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			return c.ID, tok
		}, false},
		{"credential an operator issues to themselves", func(t *testing.T, w *modeWorld) (int64, string) {
			c, tok, err := w.s.CreateCredential(ctx, principalActor(w.olga.ID), w.olga.ID, "mine", nil)
			if err != nil {
				t.Fatal(err)
			}
			return c.ID, tok
		}, false},
		{"operator rotates the member's own sign-in", func(t *testing.T, w *modeWorld) (int64, string) {
			cred, _ := login(t, w)
			c, tok, err := w.s.RotateCredential(ctx, principalActor(w.op.ID), cred)
			if err != nil {
				t.Fatal(err)
			}
			return c.ID, tok
		}, false},
		{"someone else's sign-in link", func(t *testing.T, w *modeWorld) (int64, string) {
			link, err := w.s.CreateSignInLink(ctx, principalActor(w.op.ID), w.op.ID, w.member.ID, schema.CreateSignInLinkRequestV1{}, w.now)
			if err != nil {
				t.Fatal(err)
			}
			r, err := w.s.RedeemSignInLink(ctx, HashSignInLinkCode(link.Code), w.now)
			if err != nil {
				t.Fatal(err)
			}
			return r.Credential.ID, r.Token
		}, false},
		{"device link made on an act-as credential", func(t *testing.T, w *modeWorld) (int64, string) {
			cred, _ := actAs(t, w)
			return deviceLinkCredential(t, w, cred)
		}, false},
		{"act-as session rotated by a password change", func(t *testing.T, w *modeWorld) (int64, string) {
			cred, _ := actAs(t, w)
			return changePassword(t, w, cred)
		}, false},
		{"host-made setup credential", func(t *testing.T, w *modeWorld) (int64, string) {
			tok, err := w.s.IssueSetupToken(ctx, w.olga.ID, w.now)
			if err != nil {
				t.Fatal(err)
			}
			setup, err := w.s.RedeemSetupToken(ctx, HashSetupToken(tok.Token), w.now)
			if err != nil {
				t.Fatal(err)
			}
			return setup.Credential.ID, setup.Token
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newModeWorld(t)
			id, token := tt.make(t, w)
			if got := provenanceOf(t, w.s, id, token); got != tt.self {
				t.Errorf("self-made = %v, want %v", got, tt.self)
			}
		})
	}
}

// A credential id the store does not know has no provenance.
func TestCredentialSelfMadeUnknown(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CredentialSelfMade(context.Background(), 404); !errors.Is(err, ErrCredentialNotFound) {
		t.Errorf("CredentialSelfMade(unknown) = %v, want ErrCredentialNotFound", err)
	}
}

// The provenance migration marks every existing credential and sign-in link
// made by another, whatever it looks like: a password sign-in of the
// person's own, an operator's credential, an operator-made sign-in link and
// a device link the person made for themselves alike, since which path made
// an old row is not on record (issue #2042). A device link from before the
// migration, redeemed after it, issues a credential made by another.
// Reopening changes nothing.
func TestCredentialProvenanceMigration(t *testing.T) {
	ctx := context.Background()
	far := time.Now().Add(time.Hour).UnixMilli()
	s, path := migrateTo(t, credentialProvenanceMigration-1, []string{
		`INSERT INTO principals (id, kind, name, name_key, name_skel, role, owner, voice_allowed, may_invite, created_at) VALUES
			(1, 'human', 'owner', 'owner', 'owner', 'operator', 1, 1, 0, 1),
			(2, 'human', 'wren', 'wren', 'wren', 'member', 0, 1, 1, 2)`,
		`INSERT INTO credentials (id, principal_id, label, token_hash, created_at, issued_by) VALUES
			(1, 2, 'password sign-in', 'h1', 3, 2),
			(2, 2, 'act as', 'h2', 4, 1),
			(3, 1, 'bootstrap', 'h3', 5, NULL)`,
		fmt.Sprintf(`INSERT INTO sign_in_links (id, code_hash, principal_id, created_by, created_at, expires_at, purpose, label) VALUES
			(1, 'l1', 2, 1, 6, %d, 'sign_in', NULL),
			(2, '%s', 2, 2, 7, %d, 'device', 'phone')`, far, HashSignInLinkCode("old-device-link"), far),
	})
	check := func(s *Store) {
		t.Helper()
		var n, self int
		if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(self_made), 0) FROM credentials WHERE id <= 3`).Scan(&n, &self); err != nil {
			t.Fatal(err)
		}
		if n != 3 || self != 0 {
			t.Errorf("%d migrated credentials, %d self-made; want 3, none", n, self)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(self_made), 0) FROM sign_in_links WHERE id <= 2`).Scan(&n, &self); err != nil {
			t.Fatal(err)
		}
		if n != 2 || self != 0 {
			t.Errorf("%d migrated sign-in links, %d self-made; want 2, none", n, self)
		}
	}
	check(s)
	r, err := s.RedeemSignInLink(ctx, HashSignInLinkCode("old-device-link"), time.Now())
	if err != nil {
		t.Fatalf("RedeemSignInLink(pre-migration device link) = %v", err)
	}
	if provenanceOf(t, s, r.Credential.ID, r.Token) {
		t.Error("a device link from before the migration issued a self-made credential")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	check(openTestStoreAt(t, path))
}

// The column refuses anything but 0 and 1, and a row written without it is
// made by another.
func TestCredentialProvenanceColumn(t *testing.T) {
	s := openTestStore(t)
	p, err := s.CreatePrincipal(context.Background(), PrincipalHuman, "wren")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO credentials (principal_id, label, token_hash, created_at, self_made) VALUES (?, 'x', 'h-bad', 1, 2)`, p.ID); err == nil {
		t.Error("self_made took 2")
	}
	res, err := s.db.Exec(`INSERT INTO credentials (principal_id, label, token_hash, created_at) VALUES (?, 'x', 'h-default', 1)`, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if provenanceOf(t, s, id, "") {
		t.Error("a credential written without provenance is self-made")
	}
}
