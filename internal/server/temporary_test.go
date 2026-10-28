package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Temporary servers and password-only operators (issues #866 and #875,
// ADR-011, ADR-012): the five places of ADR-011 item 2.1, each on a server
// that requires the authenticator and on both kinds that allow password-only
// (temporary, and normal with CONCHD_OPERATOR_TOTP=optional), for an
// operator with a password alone and for one with an authenticator, and the
// banner fields of ADR-011 item 2.3, which only temporary mode sets.

// serverSettings are a server's two start-time settings: its mode and its
// operator TOTP setting, empty for the mode's default (ADR-012 item 2).
type serverSettings struct {
	mode store.Mode
	totp store.OperatorTOTP
}

var (
	normalRequired = serverSettings{store.ModeNormal, ""}
	temporary      = serverSettings{store.ModeTemporary, ""}
	normalOptional = serverSettings{store.ModeNormal, store.OperatorTOTPOptional}
)

func (s serverSettings) resolved(t *testing.T) store.OperatorTOTP {
	t.Helper()
	totp, err := store.ParseOperatorTOTP(string(s.totp), s.mode)
	if err != nil {
		t.Fatal(err)
	}
	return totp
}

func (s serverSettings) allowed(t *testing.T) bool {
	t.Helper()
	return store.PasswordOnlyAllowed(s.mode, s.resolved(t))
}

// newModeFixture is the operator fixture on a server started with settings,
// recorded in the store as conchd records them at start (store.StartMode),
// since the store's checks read the recorded flag.
func newModeFixture(t *testing.T, settings serverSettings) *operatorFixture {
	t.Helper()
	totp := settings.resolved(t)
	f := &operatorFixture{newLoginFixture(t, Config{TOTPKey: testTOTPKey(t), Mode: settings.mode, OperatorTOTP: totp})}
	if _, err := f.srv.store.StartMode(context.Background(), settings.mode, store.PasswordOnlyAllowed(settings.mode, totp), f.srv.now()); err != nil {
		t.Fatal(err)
	}
	return f
}

// modeCases are the six rows every place is held to.
var modeCases = []struct {
	name     string
	mode     serverSettings
	enrolled bool
	// passwordOnly is what ADR-011 item 2.1 allows: no code is asked for.
	passwordOnly bool
}{
	{"normal, password only", normalRequired, false, false},
	{"normal, enrolled", normalRequired, true, false},
	{"temporary, password only", temporary, false, true},
	{"temporary, enrolled", temporary, true, false},
	{"normal+optional, password only", normalOptional, false, true},
	{"normal+optional, enrolled", normalOptional, true, false},
}

// rootSession gives root a browser session for the case and returns its
// token and, when enrolled, the authenticator's secret. A password-only
// operator on a normal server has no way to one, so it is made as an
// enrolled one's and the authenticator removed underneath, as the state a
// server could be in only if the enrolment were lost.
func rootSession(t *testing.T, f *operatorFixture, settings serverSettings, enrolled bool) (string, []byte) {
	t.Helper()
	if !enrolled && settings.allowed(t) {
		rec := f.login(t, "root", rootPassword, "")
		resp := decodeBody[schema.LoginResponseV1](t, rec)
		if rec.Code != http.StatusOK || resp.Next != schema.LoginNextSignedIn {
			t.Fatalf("password-only login = %d %s", rec.Code, rec.Body)
		}
		return resp.Token, nil
	}
	secret := f.enrolOperator(t, f.root.ID)
	_, tok := f.operatorSession(t, secret)
	if !enrolled {
		f.exec(t, "DELETE FROM totp WHERE principal_id = ?", f.root.ID)
		secret = nil
	}
	return tok, secret
}

// Place 1, browser sign-in: a password-only operator on a temporary server
// gets the operator's browser session straight away, as a member gets one;
// on a normal server they are refused as setup incomplete; an enrolled
// operator gets the challenge in either mode.
func TestTemporaryLogin(t *testing.T) {
	for _, tt := range modeCases {
		t.Run(tt.name, func(t *testing.T) {
			f := newModeFixture(t, tt.mode)
			if tt.enrolled {
				f.enrollTOTP(t, f.root.ID)
			}
			before := f.credentialCount(t, f.root.ID)
			rec := f.login(t, "root", rootPassword, "")
			switch {
			case tt.passwordOnly:
				resp := decodeBody[schema.LoginResponseV1](t, rec)
				if rec.Code != http.StatusOK || resp.Validate() != nil || resp.Next != schema.LoginNextSignedIn || resp.Token == "" {
					t.Fatalf("login = %d %s, want signed in", rec.Code, rec.Body)
				}
				if rec.Header().Get("Cache-Control") != "no-store" {
					t.Error("the session was sent without no-store")
				}
				// The operator's browser session: twelve hours, an idle
				// bound, no renewal, and the label the code's session has.
				c := resp.Credential
				if c.Label != store.OperatorSessionLabel || c.IdleExpiresAt == nil || c.ExpiresAt == nil || c.Renews {
					t.Errorf("credential %+v, want the operator's browser session", c)
				}
				if got := f.whoamiAuth(t, resp.Token); got == nil || !got.SetupComplete || got.TOTPEnrolled || got.Scope != schema.CredentialScopeFull {
					t.Errorf("whoami of the session = %+v", got)
				}
				if n := len(auditRows(t, f.authFixture, store.AuditLogin)); n != 1 {
					t.Errorf("%d login rows, want one", n)
				}
			case tt.enrolled:
				resp := decodeBody[schema.LoginResponseV1](t, rec)
				if rec.Code != http.StatusOK || resp.Next != schema.LoginNextTOTP || resp.Token != "" {
					t.Fatalf("login = %d %s, want a challenge", rec.Code, rec.Body)
				}
			default:
				assertErrorBody(t, rec, http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
				rows := loginFailedRows(t, f.authFixture)
				if len(rows) != 1 || !strings.Contains(rows[0], "reason="+loginReasonSetupIncomplete) {
					t.Errorf("login_failed rows %q, want one setup_incomplete", rows)
				}
			}
			if want := before + map[bool]int{true: 1}[tt.passwordOnly]; f.credentialCount(t, f.root.ID) != want {
				t.Errorf("root holds %d credentials, want %d", f.credentialCount(t, f.root.ID), want)
			}
		})
	}
}

// The review's NOTE 1: where password-only is allowed (temporary, or
// normal with optional), an owner whose one credential is a password-only
// browser session signs out of it (204), since the password signs them in
// again; the lock-out guard counts that (ADR-011 item 2.1, ADR-012 item 3).
func TestTemporaryOwnerSignsOutLastSession(t *testing.T) {
	for name, settings := range map[string]serverSettings{"temporary": temporary, "normal+optional": normalOptional} {
		t.Run(name, func(t *testing.T) {
			f := newModeFixture(t, settings)
			tok, _ := rootSession(t, f, settings, false)
			// The fixture's script credential would count on its own; without
			// it the session is root's only way in but the password.
			f.exec(t, "UPDATE credentials SET revoked_at = 1 WHERE principal_id = ? AND label <> ?", f.root.ID, store.OperatorSessionLabel)
			if rec := f.send(t, "DELETE", "/v1/credentials/current", tok, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("signing out of the last session = %d %s, want 204", rec.Code, rec.Body)
			}
			if code := f.status(t, tok); code != http.StatusUnauthorized {
				t.Errorf("the signed-out session = %d, want 401", code)
			}
			if rec := f.login(t, "root", rootPassword, ""); rec.Code != http.StatusOK {
				t.Errorf("signing in again = %d %s", rec.Code, rec.Body)
			}
		})
	}
}

// A member signs in on a temporary server exactly as on a normal one.
func TestTemporaryLoginMember(t *testing.T) {
	f := newModeFixture(t, temporary)
	rec := f.login(t, "alice", alicePassword, "")
	resp := decodeBody[schema.LoginResponseV1](t, rec)
	if rec.Code != http.StatusOK || resp.Next != schema.LoginNextSignedIn || resp.Credential.Label != "password sign-in" || !resp.Credential.Renews {
		t.Fatalf("member login = %d %s", rec.Code, rec.Body)
	}
}

// The server and its store must agree: a Config that allows password-only
// (temporary, or optional) over a store that recorded it not allowed still
// asks for the code at the sign-in (ADR-012 item 3: the store reads its own
// flag).
func TestTemporaryConfigOverNormalStoreFailsClosed(t *testing.T) {
	for _, cfg := range []Config{
		{Mode: store.ModeTemporary, OperatorTOTP: store.OperatorTOTPOptional},
		{Mode: store.ModeNormal, OperatorTOTP: store.OperatorTOTPOptional},
	} {
		t.Run(string(cfg.Mode), func(t *testing.T) {
			cfg.TOTPKey = testTOTPKey(t)
			f := &operatorFixture{newLoginFixture(t, cfg)}
			if _, err := f.srv.store.StartMode(context.Background(), store.ModeNormal, false, f.srv.now()); err != nil {
				t.Fatal(err)
			}
			assertErrorBody(t, f.login(t, "root", rootPassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			if rows := loginFailedRows(t, f.authFixture); len(rows) != 1 || !strings.Contains(rows[0], "reason="+loginReasonStale) {
				t.Errorf("login_failed rows %q, want one stale", rows)
			}
		})
	}
}

// Place 2, re-authentication (and so ADR-010's role route and owner mark):
// a password-only operator on a temporary server proves the password alone,
// and the proof opens a route of the re-authentication class; everywhere
// else the code is asked for, and an enrolled operator's right code works.
func TestTemporaryReauth(t *testing.T) {
	for _, tt := range modeCases {
		t.Run(tt.name, func(t *testing.T) {
			f := newModeFixture(t, tt.mode)
			tok, secret := rootSession(t, f, tt.mode, tt.enrolled)
			makeCredential := func() int {
				return f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials", f.root.ID), tok,
					inviteJSON(t, schema.CreateCredentialRequestV1{Label: "cli"}), "").Code
			}
			if code := makeCredential(); code != http.StatusForbidden {
				t.Fatalf("a re-authentication route before any proof = %d, want 403", code)
			}
			rec := f.reauthOp(t, tok, rootPassword, "")
			if !tt.passwordOnly {
				assertErrorBody(t, rec, http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
				if f.whoamiAuth(t, tok).RecentAuthUntil != nil {
					t.Error("a proof stands with no code")
				}
				if !tt.enrolled {
					return
				}
				rec = f.reauthOp(t, tok, rootPassword, f.code(secret))
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("reauth = %d %s, want 200", rec.Code, rec.Body)
			}
			if f.whoamiAuth(t, tok).RecentAuthUntil == nil {
				t.Error("no proof stands after the re-authentication")
			}
			if code := makeCredential(); code != http.StatusCreated {
				t.Errorf("a re-authentication route after the proof = %d, want 201", code)
			}
			if n := len(auditRows(t, f.authFixture, store.AuditReauth)); n != 1 {
				t.Errorf("%d reauth rows, want one", n)
			}
		})
	}
}

// A code sent by a password-only operator on a temporary server is not
// looked at: there is nothing to check it against.
func TestTemporaryReauthIgnoresAStrayCode(t *testing.T) {
	f := newModeFixture(t, temporary)
	tok, _ := rootSession(t, f, temporary, false)
	if rec := f.reauthOp(t, tok, rootPassword, "000000"); rec.Code != http.StatusOK {
		t.Fatalf("reauth with a stray code = %d %s", rec.Code, rec.Body)
	}
	// A wrong password is still a wrong password.
	assertErrorBody(t, f.reauthOp(t, tok, "not the operator's passphrase at all", ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
}

// Place 3, password change: the same rule as re-authentication, and the
// store's own check agrees.
func TestTemporaryChangePassword(t *testing.T) {
	const newPassword = "the operator's new and private passphrase"
	for _, tt := range modeCases {
		t.Run(tt.name, func(t *testing.T) {
			f := newModeFixture(t, tt.mode)
			tok, secret := rootSession(t, f, tt.mode, tt.enrolled)
			change := func(code string) *httptest.ResponseRecorder {
				return f.send(t, "PUT", "/v1/password/mine", tok,
					inviteJSON(t, schema.ChangePasswordRequestV1{Current: rootPassword, Password: newPassword, Code: code}), "")
			}
			rec := change("")
			if !tt.passwordOnly {
				assertErrorBody(t, rec, http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
				if !tt.enrolled {
					return
				}
				rec = change(f.code(secret))
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("change = %d %s, want 200", rec.Code, rec.Body)
			}
			resp := decodeBody[schema.PasswordUpdatedResponseV1](t, rec)
			if resp.Token == "" || f.status(t, tok) != http.StatusUnauthorized || f.status(t, resp.Token) != http.StatusOK {
				t.Errorf("change answered %+v; want the session rotated", resp)
			}
			if n := len(auditRows(t, f.authFixture, store.AuditPasswordChanged)); n != 1 {
				t.Errorf("%d password_changed rows, want one", n)
			}
		})
	}
}

// Place 4, whoami's setup_complete: a password completes an operator's setup
// on a temporary server, both factors on a normal one; and whoami and
// /healthz carry server_mode only in temporary mode (ADR-011 item 2.3).
func TestTemporaryWhoAmI(t *testing.T) {
	for _, tt := range modeCases {
		t.Run(tt.name, func(t *testing.T) {
			f := newModeFixture(t, tt.mode)
			if tt.enrolled {
				f.enrollTOTP(t, f.root.ID)
			}
			rec := f.send(t, "GET", "/v1/whoami", f.rootTok, "", "")
			who := decodeBody[schema.WhoAmIResponseV1](t, rec)
			if err := who.Validate(); err != nil {
				t.Errorf("whoami does not validate: %v", err)
			}
			wantComplete := tt.enrolled || tt.mode.allowed(t)
			if who.Auth == nil || !who.Auth.PasswordSet || who.Auth.TOTPEnrolled != tt.enrolled || who.Auth.SetupComplete != wantComplete {
				t.Errorf("auth = %+v, want setup_complete %v", who.Auth, wantComplete)
			}
			// A member is unchanged.
			if a := f.whoamiAuth(t, f.aliceTok); a == nil || !a.SetupComplete {
				t.Errorf("alice = %+v", a)
			}
			wantMode := schema.ServerMode("")
			// The banner is temporary mode's alone: optional sets nothing.
			if tt.mode.mode == store.ModeTemporary {
				wantMode = schema.ServerModeTemporary
			}
			if who.ServerMode != wantMode || strings.Contains(rec.Body.String(), "server_mode") != (wantMode != "") {
				t.Errorf("whoami server_mode = %q in %s, want %q", who.ServerMode, rec.Body, wantMode)
			}
			hrec := f.send(t, "GET", "/healthz", "", "", "")
			var h schema.Health
			if err := json.Unmarshal(hrec.Body.Bytes(), &h); err != nil || h.Validate() != nil {
				t.Fatalf("healthz %s: %v", hrec.Body, err)
			}
			if h.ServerMode != wantMode || strings.Contains(hrec.Body.String(), "server_mode") != (wantMode != "") {
				t.Errorf("healthz server_mode = %q in %s, want %q", h.ServerMode, hrec.Body, wantMode)
			}
		})
	}
}

// Place 5, the setup credential's end: on a temporary server, a password
// completes setup and the setup credential keeps only whoami, the
// authenticator routes and its own sign-out (a second password is refused);
// a confirmed enrolment or Skip ends it. On a normal server it keeps setting
// passwords until setup is complete, and Skip ends it too. With an
// authenticator already, the password ends it in either mode.
func TestTemporarySetupCredential(t *testing.T) {
	const password = "the operator's chosen and private passphrase"
	for _, tt := range modeCases {
		for _, end := range []string{"skip", "enrol"} {
			t.Run(tt.name+"/"+end, func(t *testing.T) {
				f := newModeFixture(t, tt.mode)
				if tt.enrolled {
					// An authenticator and no password: the setup the host
					// can still give a token for.
					f.enrollTOTP(t, f.root.ID)
					f.exec(t, "DELETE FROM passwords WHERE principal_id = ?", f.root.ID)
				}
				setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
				setPassword := func(p string) *httptest.ResponseRecorder {
					return f.send(t, "POST", "/v1/password/mine", setup.Token, inviteJSON(t, schema.SetPasswordRequestV1{Password: p}), "")
				}
				if rec := setPassword(password); rec.Code != http.StatusOK {
					t.Fatalf("set password = %d %s", rec.Code, rec.Body)
				}
				if tt.enrolled {
					if code := f.status(t, setup.Token); code != http.StatusUnauthorized {
						t.Errorf("the setup credential after the password = %d, want ended", code)
					}
					return
				}
				auth := f.whoamiAuth(t, setup.Token)
				if auth.Scope != schema.CredentialScopeSetup || auth.SetupComplete != tt.passwordOnly {
					t.Errorf("whoami of the setup credential = %+v, want setup_complete %v", auth, tt.passwordOnly)
				}
				rec := setPassword("a second and private passphrase")
				if tt.passwordOnly {
					assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeSetupIncomplete)
				} else if rec.Code != http.StatusOK {
					t.Errorf("a second password on a normal server = %d %s, want it set", rec.Code, rec.Body)
				}
				assertErrorBody(t, f.send(t, "GET", "/v1/channels", setup.Token, "", ""), http.StatusForbidden, schema.ErrorCodeSetupIncomplete)
				switch end {
				case "skip":
					if rec := f.send(t, "DELETE", "/v1/credentials/current", setup.Token, "", ""); rec.Code != http.StatusNoContent {
						t.Fatalf("skip = %d %s", rec.Code, rec.Body)
					}
				case "enrol":
					secret := f.enroll(t, setup.Token)
					if rec := f.confirm(t, setup.Token, f.code(secret)); rec.Code != http.StatusNoContent {
						t.Fatalf("confirm = %d %s", rec.Code, rec.Body)
					}
				}
				if code := f.status(t, setup.Token); code != http.StatusUnauthorized {
					t.Errorf("the setup credential after %s = %d, want ended", end, code)
				}
				// The password it set is the one that signs in.
				want := password
				if !tt.passwordOnly {
					want = "a second and private passphrase"
				}
				rec = f.login(t, "root", want, "")
				resp := decodeBody[schema.LoginResponseV1](t, rec)
				switch {
				case end == "enrol" && (rec.Code != http.StatusOK || resp.Next != schema.LoginNextTOTP):
					t.Errorf("login after enrolling = %d %s, want a challenge", rec.Code, rec.Body)
				case end == "skip" && tt.passwordOnly && (rec.Code != http.StatusOK || resp.Next != schema.LoginNextSignedIn):
					t.Errorf("login after skipping = %d %s, want signed in", rec.Code, rec.Body)
				case end == "skip" && !tt.passwordOnly && rec.Code != http.StatusUnauthorized:
					t.Errorf("login after skipping on a normal server = %d %s, want refused", rec.Code, rec.Body)
				}
			})
		}
	}
}

// A setup credential that set the password on a temporary server reaches
// whoami, the authenticator routes and its own sign-out, and every other
// route of the table, the password enrolment included, answers 403
// setup_incomplete (ADR-011 item 2.1), as an unrestricted one is answered
// outside setupScopeRoutes.
func TestSetupTOTPOnlyReachesOnlyItsRoutes(t *testing.T) {
	f := newModeFixture(t, temporary)
	setup := f.redeemSetup(t, f.hostSetupToken(t, f.root.ID))
	if rec := f.send(t, "POST", "/v1/password/mine", setup.Token,
		inviteJSON(t, schema.SetPasswordRequestV1{Password: "a replacement and private passphrase"}), ""); rec.Code != http.StatusOK {
		t.Fatalf("set password = %d %s", rec.Code, rec.Body)
	}
	for _, rt := range f.srv.routes {
		if rt.access == accessExempt {
			continue // the middleware reads no credential there
		}
		exp, ok := routeExpectations[rt.pattern]
		if !ok {
			t.Fatalf("no expectation for %s", rt.pattern)
		}
		if rt.pattern == "POST /v1/totp/enroll" || rt.pattern == "DELETE /v1/credentials/current" {
			continue // these write; they are run on their own below
		}
		rec := f.send(t, routeMethod(rt.pattern), exp.path, setup.Token, exp.body, "")
		code := ""
		if rec.Code == http.StatusForbidden {
			code = decodeBody[schema.Error](t, rec).Code
		}
		if setupTOTPOnlyRoutes[rt.pattern] {
			if !setupScopeRoutes[rt.pattern] {
				t.Errorf("%s is in setupTOTPOnlyRoutes but not setupScopeRoutes", rt.pattern)
			}
			if rec.Code == http.StatusUnauthorized || code == schema.ErrorCodeSetupIncomplete {
				t.Errorf("%s refused the restricted setup credential: %d %s", rt.pattern, rec.Code, rec.Body)
			}
			continue
		}
		if rec.Code != http.StatusForbidden || code != schema.ErrorCodeSetupIncomplete {
			t.Errorf("%s answered the restricted setup credential %d %s, want 403 setup_incomplete", rt.pattern, rec.Code, rec.Body)
		}
	}
	if rec := f.send(t, "POST", "/v1/totp/enroll", setup.Token, "", ""); rec.Code != http.StatusOK {
		t.Errorf("enroll = %d %s", rec.Code, rec.Body)
	}
	if rec := f.send(t, "DELETE", "/v1/credentials/current", setup.Token, "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("skip = %d %s", rec.Code, rec.Body)
	}
	if code := f.status(t, setup.Token); code != http.StatusUnauthorized {
		t.Errorf("whoami after skip = %d, want 401", code)
	}
}
