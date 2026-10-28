package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Setup's optional authenticator follows the post-password auth state, not
// the server's banner or the state returned when the setup token was redeemed.
func TestSetupOptionalTOTP(t *testing.T) {
	optional := &schema.AuthStateV1{Scope: schema.CredentialScopeSetup, PasswordSet: true, SetupComplete: true}
	required := &schema.AuthStateV1{Scope: schema.CredentialScopeSetup, PasswordSet: true}
	tests := []struct {
		name        string
		auth        *schema.AuthStateV1
		mode        schema.ServerMode
		interactive bool
		answer      string
		wantPrompt  bool
		wantEnroll  bool
		failRoute   string
	}{
		{name: "no", auth: optional, interactive: true, answer: "n", wantPrompt: true},
		{name: "empty defaults to no", auth: optional, interactive: true, wantPrompt: true},
		{name: "yes", auth: optional, interactive: true, answer: "y", wantPrompt: true, wantEnroll: true},
		{name: "non-interactive defaults to no", auth: optional},
		{name: "non-interactive ignores a yes", auth: optional, answer: "yes"},
		{name: "normal server requires enrollment", auth: required, interactive: true, wantEnroll: true},
		{name: "required enrollment from a pipe", auth: required, wantEnroll: true},
		{name: "temporary banner does not permit skipping", auth: required, mode: schema.ServerModeTemporary, interactive: true, wantEnroll: true},
		{name: "temporary optional setup", auth: optional, mode: schema.ServerModeTemporary, interactive: true, wantPrompt: true},
		{name: "missing auth does not permit skipping", interactive: true, wantEnroll: true},
		{name: "full scope does not permit skipping", auth: &schema.AuthStateV1{Scope: schema.CredentialScopeFull, PasswordSet: true, SetupComplete: true}, interactive: true, wantEnroll: true},
		{name: "enrolled authenticator does not permit skipping", auth: &schema.AuthStateV1{Scope: schema.CredentialScopeSetup, PasswordSet: true, SetupComplete: true, TOTPEnrolled: true}, interactive: true, wantEnroll: true},
		{name: "whoami failure stops setup", auth: optional, failRoute: "GET /v1/whoami"},
		{name: "skip failure is reported", auth: optional, failRoute: "DELETE /v1/credentials/current"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", fakeToken)
			setupToken := schema.SetupTokenPrefix + strings.Repeat("t", 43)
			credential := schema.CredentialTokenPrefix + strings.Repeat("s", 43)
			calls := make(chan string, 16)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				route := r.Method + " " + r.URL.Path
				calls <- route
				if route != "POST /v1/setup/redeem" && r.Header.Get("Authorization") != "Bearer "+credential {
					t.Error("setup request did not use the explicit setup credential")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if route == tt.failRoute {
					writeAPIError(w, http.StatusInternalServerError, "internal_error")
					return
				}
				switch route {
				case "POST /v1/setup/redeem":
					var req schema.RedeemSetupTokenRequestV1
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Token != setupToken {
						t.Error("wrong setup token request")
					}
					_ = json.NewEncoder(w).Encode(schema.RedeemSetupTokenResponseV1{
						Principal: schema.PrincipalV0{ID: 1, Name: "root", Kind: schema.PrincipalHuman},
						Token:     credential, Auth: schema.AuthStateV1{Scope: schema.CredentialScopeSetup},
					})
				case "POST /v1/password/mine":
					var req schema.SetPasswordRequestV1
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password != operatorPassword {
						t.Error("wrong password enrollment request")
					}
					_ = json.NewEncoder(w).Encode(schema.PasswordUpdatedResponseV1{})
				case "GET /v1/whoami":
					_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 1, Name: "root", Kind: schema.PrincipalHuman,
						Role: schema.RoleOperator, IdentityClass: schema.IdentityOperator, Auth: tt.auth, ServerMode: tt.mode})
				case "DELETE /v1/credentials/current":
					w.WriteHeader(http.StatusNoContent)
				case "POST /v1/totp/enroll":
					_ = json.NewEncoder(w).Encode(schema.TOTPEnrollmentV1{ //nolint:gosec // a fake enrolment from a test server, not a real secret
						Account: "root", Issuer: "conch", Secret: "MANUALENTRYSECRET", Algorithm: "SHA1", Digits: 6, PeriodSeconds: 30,
					})
				case "POST /v1/totp/confirm":
					var req schema.ConfirmTOTPRequestV1
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code != "123456" {
						t.Error("wrong authenticator confirmation")
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request: %s", route)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(ts.Close)

			var stdin io.Reader
			var reads *[]string
			if tt.interactive {
				stdin, reads = fakeTerminalInput(t, []string{setupToken, operatorPassword, operatorPassword, "123456"}, []string{tt.answer})
			} else {
				answers := setupToken + "\n" + operatorPassword + "\n"
				if tt.wantEnroll {
					answers += "123456\n"
				} else if tt.answer != "" {
					answers += tt.answer + "\n"
				}
				stdin = strings.NewReader(answers)
			}
			var stdout, stderr bytes.Buffer
			err := RunWithStdin(context.Background(), []string{"setup", "--server", ts.URL}, stdin, &stdout, &stderr, "vtest")
			if (err != nil) != (tt.failRoute != "") {
				t.Fatalf("setup error = %v; expected failure on %q", err, tt.failRoute)
			}
			wantCalls := []string{"POST /v1/setup/redeem", "POST /v1/password/mine", "GET /v1/whoami"}
			if tt.failRoute != "GET /v1/whoami" {
				if tt.wantEnroll {
					wantCalls = append(wantCalls, "POST /v1/totp/enroll", "POST /v1/totp/confirm")
				} else {
					wantCalls = append(wantCalls, "DELETE /v1/credentials/current")
				}
			}
			var gotCalls []string
			for len(calls) > 0 {
				gotCalls = append(gotCalls, <-calls)
			}
			if !reflect.DeepEqual(gotCalls, wantCalls) {
				t.Errorf("requests = %v, want %v", gotCalls, wantCalls)
			}
			const prompt = "Set up an authenticator now? [y/N] "
			if strings.Contains(stderr.String(), prompt) != tt.wantPrompt {
				t.Errorf("stderr = %q; want optional prompt %v", stderr.String(), tt.wantPrompt)
			}
			if reads != nil {
				wantReads := "secret secret secret"
				if tt.wantPrompt {
					wantReads += " line"
				}
				if tt.wantEnroll {
					wantReads += " secret"
				}
				if strings.Join(*reads, " ") != wantReads {
					t.Errorf("reads = %v, want %s", *reads, wantReads)
				}
			}
			if tt.failRoute == "" {
				want := "setup complete for root; sign in with: conch login --password --name 'root' --server " + ts.URL + "\n"
				if !strings.HasSuffix(stdout.String(), want) {
					t.Errorf("stdout = %q, want completion %q", stdout.String(), want)
				}
			} else if strings.Contains(stdout.String(), "setup complete") {
				t.Errorf("failed setup reported success: %q", stdout.String())
			}
			if strings.Contains(stdout.String(), "manual entry") != tt.wantEnroll {
				t.Errorf("stdout = %q; want enrollment %v", stdout.String(), tt.wantEnroll)
			}
			if stored, err := readCredentials(); err != nil || stored[ts.URL].Token != "" {
				t.Errorf("setup stored a credential: %v", err)
			}
			for _, secret := range []string{setupToken, credential, operatorPassword, "123456"} {
				if strings.Contains(stdout.String()+stderr.String(), secret) {
					t.Error("setup printed a token, password or confirmation code")
				}
			}
		})
	}
}

func TestPasswordCommandsTOTPEnrollment(t *testing.T) {
	identities := []struct {
		name     string
		class    schema.IdentityClass
		role     schema.PrincipalRole
		auth     *schema.AuthStateV1
		wantCode bool
	}{
		{"password-only operator", schema.IdentityOperator, schema.RoleOperator, &schema.AuthStateV1{Scope: schema.CredentialScopeFull, PasswordSet: true, SetupComplete: true}, false},
		{"enrolled operator", schema.IdentityOperator, schema.RoleOperator, &schema.AuthStateV1{Scope: schema.CredentialScopeFull, PasswordSet: true, SetupComplete: true, TOTPEnrolled: true}, true},
		{"member", schema.IdentityMember, schema.RoleMember, &schema.AuthStateV1{Scope: schema.CredentialScopeFull, PasswordSet: true, SetupComplete: true}, false},
		{"operator without auth state", schema.IdentityOperator, schema.RoleOperator, nil, false},
		{"legacy enrolled operator", "", schema.RoleOperator, &schema.AuthStateV1{Scope: schema.CredentialScopeFull, TOTPEnrolled: true}, true},
	}
	for _, command := range []string{"password change", "reauth"} {
		for _, tt := range identities {
			t.Run(command+"/"+tt.name, func(t *testing.T) {
				isolateConfig(t)
				t.Setenv("CONCH_TOKEN", fakeToken)
				codes := make(chan string, 2)
				ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer "+fakeToken {
						t.Error("request did not use the caller's credential")
					}
					switch r.Method + " " + r.URL.Path {
					case "GET /v1/whoami":
						_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: "root", Role: tt.role, IdentityClass: tt.class, Auth: tt.auth})
					case "PUT /v1/password/mine":
						var req schema.ChangePasswordRequestV1
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Current != operatorPassword || req.Password != memberPassword {
							t.Error("wrong password change request")
						}
						codes <- req.Code
						_ = json.NewEncoder(w).Encode(schema.PasswordUpdatedResponseV1{})
					case "POST /v1/reauth":
						var req schema.ReauthRequestV1
						if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password != operatorPassword {
							t.Error("wrong reauth request")
						}
						codes <- req.Code
						_ = json.NewEncoder(w).Encode(schema.ReauthResponseV1{Until: schema.NewTimestamp(time.Now().Add(time.Minute))})
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				t.Cleanup(ts.Close)
				secrets := []string{operatorPassword}
				if command == "password change" {
					secrets = append(secrets, memberPassword, memberPassword)
				}
				wantReads := len(secrets)
				wantCode := ""
				if tt.wantCode {
					wantCode = "123456"
					secrets = append(secrets, wantCode)
					wantReads++
				}
				stdin, reads := fakeTerminalInput(t, secrets, nil)
				var stdout, stderr bytes.Buffer
				args := append(strings.Fields(command), "--server", ts.URL)
				if err := RunWithStdin(context.Background(), args, stdin, &stdout, &stderr, "vtest"); err != nil {
					t.Fatalf("%s: %v", command, err)
				}
				if len(codes) != 1 {
					t.Fatalf("got %d password requests, want one", len(codes))
				}
				if got := <-codes; got != wantCode {
					t.Errorf("sent code = %q, want %q", got, wantCode)
				}
				if len(*reads) != wantReads || strings.Contains(stderr.String(), "Authenticator code: ") != tt.wantCode {
					t.Errorf("reads = %v, stderr = %q; want %d reads, code prompt %v", *reads, stderr.String(), wantReads, tt.wantCode)
				}
				if command == "reauth" && strings.Contains(stderr.String(), "and an authenticator code") != tt.wantCode {
					t.Errorf("reauth explanation does not match enrollment: %q", stderr.String())
				}
				for _, secret := range secrets {
					if strings.Contains(stdout.String()+stderr.String(), secret) {
						t.Error("password command printed a secret")
					}
				}
			})
		}
	}
}

func TestPasswordOnlyLogin(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(either(interactive, "terminal", "pipe"), func(t *testing.T) {
			isolateConfig(t)
			token := schema.CredentialTokenPrefix + strings.Repeat("l", 43)
			calls := make(chan string, 4)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls <- r.Method + " " + r.URL.Path
				switch r.Method + " " + r.URL.Path {
				case "POST /v1/login":
					var req schema.LoginRequestV1
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name != "root" || req.Password != operatorPassword {
						t.Error("wrong login request")
					}
					_ = json.NewEncoder(w).Encode(schema.LoginResponseV1{Next: schema.LoginNextSignedIn, Token: token,
						Principal:  &schema.PrincipalV0{ID: 1, Kind: schema.PrincipalHuman, Name: "root"},
						Credential: &schema.CredentialV1{ID: 1, PrincipalID: 1, Scope: schema.CredentialScopeFull}})
				case "GET /v1/whoami":
					if r.Header.Get("Authorization") != "Bearer "+token {
						t.Error("whoami did not use the new login")
					}
					_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: "root", Role: schema.RoleOperator,
						IdentityClass: schema.IdentityOperator, Auth: &schema.AuthStateV1{Scope: schema.CredentialScopeFull, PasswordSet: true, SetupComplete: true}})
				default:
					t.Errorf("unexpected login request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(ts.Close)
			var stdin io.Reader = strings.NewReader(operatorPassword + "\n")
			var reads *[]string
			if interactive {
				stdin, reads = fakeTerminalInput(t, []string{operatorPassword}, nil)
			}
			var stdout, stderr bytes.Buffer
			if err := RunWithStdin(context.Background(), []string{"login", "--password", "--name", "root", "--server", ts.URL}, stdin, &stdout, &stderr, "vtest"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout.String(), "as root (operator)") || strings.Contains(stderr.String(), "Authenticator") {
				t.Errorf("stdout = %q, stderr = %q; want signed in without an authenticator prompt", stdout.String(), stderr.String())
			}
			if reads != nil && strings.Join(*reads, " ") != "secret" {
				t.Errorf("reads = %v, want only the password", *reads)
			}
			var gotCalls []string
			for len(calls) > 0 {
				gotCalls = append(gotCalls, <-calls)
			}
			if want := []string{"POST /v1/login", "GET /v1/whoami"}; !reflect.DeepEqual(gotCalls, want) {
				t.Errorf("requests = %v, want %v", gotCalls, want)
			}
			if got, err := LoadToken(ts.URL); err != nil || got != token {
				t.Errorf("login did not store the session: %v", err)
			}
			if strings.Contains(stdout.String()+stderr.String(), operatorPassword) || strings.Contains(stdout.String()+stderr.String(), token) {
				t.Error("login printed the password or session token")
			}
		})
	}
}
