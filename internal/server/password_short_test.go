package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Short passwords (issue #975): the owner removed the 15-character minimum
// on 2026-10-30. Every route that sets a password takes a short one that
// passes the unchanged policy (not common, not one unit repeated, no name in
// it), and the person then signs in and re-authenticates with it. Members
// and operators, normal and temporary servers, are held to the same rule:
// non-empty and within the schema's upper bounds, nothing more.

// shortPassword is four letters: on no list, no unit repeated, and in no
// name the fixtures use.
const shortPassword = "frog"

// A short password set through each member route signs that member in, and
// re-authenticates the session it gives; a wrong one still does neither.
func TestShortPasswordThroughEveryMemberRoute(t *testing.T) {
	tests := []struct {
		name string
		// set sets shortPassword through the route and returns the fixture
		// and the name the account signs in with.
		set func(t *testing.T) (*loginFixture, string)
	}{
		{"join an account from the bare address", func(t *testing.T) (*loginFixture, string) {
			f := newLoginFixture(t, Config{})
			f.mustPutJoin(t, openAccounts("red"))
			rec := f.send(t, "POST", "/v1/join/account", "", inviteJSON(t, schema.JoinAccountRequestV1{Name: "newcomer", Password: shortPassword}), "")
			mustAccount(t, rec)
			return f, "newcomer"
		}},
		{"redeem an invite", func(t *testing.T) (*loginFixture, string) {
			f := newLoginFixture(t, Config{})
			code := f.createInvite(t, f.rootTok, openLink("general")).Code
			rec := f.send(t, "POST", "/v2/invites/redeem", "", inviteJSON(t, schema.RedeemInviteRequestV2{Code: code, Name: "river", Password: shortPassword}), "")
			if rec.Code != http.StatusCreated {
				t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
			}
			return f, "river"
		}},
		{"convert a guest visit", func(t *testing.T) (*loginFixture, string) {
			f := newLoginFixture(t, Config{})
			f.mustPutJoin(t, openAccounts("red"))
			g := f.seedGuest(t, "Pat")
			mustConverted(t, f.send(t, "POST", "/v1/guest/convert", g.Token,
				inviteJSON(t, schema.ConvertGuestRequestV1{Name: "member", Password: shortPassword}), ""))
			return f, "member"
		}},
		{"enrol a first password with an enrollment link", func(t *testing.T) (*loginFixture, string) {
			f := newRecoveryFixture(t)
			if rec := f.enroll(t, f.wrenTok, shortPassword, f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID).Code); rec.Code != http.StatusOK {
				t.Fatalf("enroll = %d %s", rec.Code, rec.Body)
			}
			return f.loginFixture, "wren"
		}},
		{"change a password", func(t *testing.T) (*loginFixture, string) {
			f := newRecoveryFixture(t)
			if rec := f.change(t, f.aliceTok, alicePassword, shortPassword); rec.Code != http.StatusOK {
				t.Fatalf("change = %d %s", rec.Code, rec.Body)
			}
			return f.loginFixture, "alice"
		}},
		{"reset a password with a reset link", func(t *testing.T) (*loginFixture, string) {
			f := newRecoveryFixture(t)
			if rec := f.reset(t, f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code, shortPassword); rec.Code != http.StatusOK {
				t.Fatalf("reset = %d %s", rec.Code, rec.Body)
			}
			return f.loginFixture, "alice"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, name := tt.set(t)
			assertErrorBody(t, f.login(t, name, shortPassword+"s", ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			rec := f.login(t, name, shortPassword, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("login with the short password = %d %s", rec.Code, rec.Body)
			}
			resp := decodeBody[schema.LoginResponseV1](t, rec)
			if resp.Next != schema.LoginNextSignedIn || resp.Token == "" {
				t.Fatalf("login answered %+v, want signed in", resp)
			}
			reauth := func(password string) *httptest.ResponseRecorder {
				return f.send(t, "POST", "/v1/reauth", resp.Token, inviteJSON(t, schema.ReauthRequestV1{Password: password}), "")
			}
			assertErrorBody(t, reauth("toad"), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			if rec := reauth(shortPassword); rec.Code != http.StatusOK {
				t.Fatalf("reauth with the short password = %d %s", rec.Code, rec.Body)
			}
		})
	}
}

// An operator's setup takes a short password too, on a server that requires
// the authenticator and on a temporary one that allows the password alone:
// the password then opens the sign-in (a challenge, or the session), and
// re-authenticates with the code where one is enrolled.
func TestShortPasswordThroughOperatorSetup(t *testing.T) {
	for _, settings := range []struct {
		name string
		mode serverSettings
	}{
		{"normal, authenticator required", normalRequired},
		{"temporary, password only", temporary},
	} {
		t.Run(settings.name, func(t *testing.T) {
			f := newModeFixture(t, settings.mode)
			setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
			if rec := f.send(t, "POST", "/v1/password/mine", setup.Token, inviteJSON(t, schema.SetPasswordRequestV1{Password: shortPassword}), ""); rec.Code != http.StatusOK {
				t.Fatalf("set the operator's password = %d %s", rec.Code, rec.Body)
			}
			assertErrorBody(t, f.login(t, "root", rootPassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			if settings.mode.allowed(t) {
				rec := f.login(t, "root", shortPassword, "")
				resp := decodeBody[schema.LoginResponseV1](t, rec)
				if rec.Code != http.StatusOK || resp.Next != schema.LoginNextSignedIn || resp.Token == "" {
					t.Fatalf("password-only login = %d %s", rec.Code, rec.Body)
				}
				if rec := f.reauthOp(t, resp.Token, shortPassword, ""); rec.Code != http.StatusOK {
					t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
				}
				return
			}
			secret := f.enroll(t, setup.Token)
			if rec := f.confirm(t, setup.Token, f.code(secret)); rec.Code != http.StatusNoContent {
				t.Fatalf("confirm = %d %s", rec.Code, rec.Body)
			}
			f.nextStep()
			rec := f.completeLogin(t, f.rootChallenge(t, shortPassword, ""), f.code(secret), "")
			if rec.Code != http.StatusOK {
				t.Fatalf("login/totp = %d %s", rec.Code, rec.Body)
			}
			tok := decodeBody[schema.LoginResponseV1](t, rec).Token
			f.nextStep()
			if rec := f.reauthOp(t, tok, shortPassword, f.code(secret)); rec.Code != http.StatusOK {
				t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
			}
		})
	}
}

// legacyRecord is the Argon2id record of legacyPassword, made by the
// fixture's parameters before the minimum was removed (main at 37f88da). It
// verifies unchanged: no migration, reset or re-enrolment.
const (
	legacyPassword = "a long passphrase set before #975" // #nosec G101 -- a test password
	legacyRecord   = "$argon2id$v=19$m=64,t=1,p=1$0/zXJr0+cwx1ldVBlPFduw$FJtAP8aUF3QV9TMy7SnAuAyEeVTZfW+iUewdT3EjAKs"
)

func TestExistingLongPasswordStillVerifies(t *testing.T) {
	f := newLoginFixture(t, Config{})
	if _, err := f.srv.store.SetPassword(context.Background(), "system", f.alice.ID, legacyRecord, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := f.login(t, "alice", legacyPassword, "")
	if rec.Code != http.StatusOK || decodeBody[schema.LoginResponseV1](t, rec).Next != schema.LoginNextSignedIn {
		t.Fatalf("login with a password set before #975 = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.login(t, "alice", shortPassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
}
