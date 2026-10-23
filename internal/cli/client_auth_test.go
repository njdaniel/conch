package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The client's side of ADR-009 (issue #715): it fails closed on an identity
// it cannot read, tells a sign-in failure from a signed-out credential,
// forgets a guest visit the server ended, and repeats a request once after a
// fresh proof of the password.

func TestCheckIdentity(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	handle := schema.GuestNamePrefix + "a7k4m2q9a7k4m2q9"
	visit := schema.GuestV1{State: schema.GuestStateActive, Origin: schema.GuestOriginAddress, Handle: handle, Tag: schema.GuestTag(handle),
		StartedAt: schema.NewTimestamp(now), ExpiresAt: schema.NewTimestamp(now.Add(time.Hour)), IdleExpiresAt: schema.NewTimestamp(now.Add(time.Minute))}
	bad := visit
	bad.Tag = "zzzz"
	tests := []struct {
		name string
		who  schema.WhoAmIResponseV1
		ok   bool
	}{
		{"a server that predates guests", schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Role: schema.RoleMember}, true},
		{"a member", schema.WhoAmIResponseV1{IdentityClass: schema.IdentityMember, Auth: &schema.AuthStateV1{Scope: schema.CredentialScopeFull}}, true},
		{"a guest with its visit", schema.WhoAmIResponseV1{IdentityClass: schema.IdentityGuest, Guest: &visit}, true},
		{"an unknown class", schema.WhoAmIResponseV1{IdentityClass: "visitor"}, false},
		{"a guest without its visit", schema.WhoAmIResponseV1{IdentityClass: schema.IdentityGuest}, false},
		{"a visit for a member", schema.WhoAmIResponseV1{IdentityClass: schema.IdentityMember, Guest: &visit}, false},
		{"a visit with no class", schema.WhoAmIResponseV1{Guest: &visit}, false},
		{"a malformed visit", schema.WhoAmIResponseV1{IdentityClass: schema.IdentityGuest, Guest: &bad}, false},
		{"an unknown scope", schema.WhoAmIResponseV1{IdentityClass: schema.IdentityMember, Auth: &schema.AuthStateV1{Scope: "root"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkIdentity(tt.who)
			if (err == nil) != tt.ok {
				t.Fatalf("checkIdentity = %v, want ok %v", err, tt.ok)
			}
			if err != nil && !strings.Contains(err.Error(), "update conch") {
				t.Errorf("err = %v, want it to say what to do", err)
			}
		})
	}
}

// writeAPIError answers with the server's error document.
func writeAPIError(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(schema.Error{Code: code, Message: "refused"})
}

func TestDecodeUnauthorized(t *testing.T) {
	tests := []struct {
		name     string
		code     string
		guest    bool
		wantCode string
		wantIs   error
	}{
		{name: "a sign-in failure is the server's answer", code: schema.ErrorCodeAuthFailed, wantCode: schema.ErrorCodeAuthFailed},
		{name: "a refused credential", code: "unauthenticated", wantIs: ErrUnauthenticated},
		{name: "a refused guest visit", code: "unauthenticated", guest: true, wantIs: ErrGuestVisitEnded},
		{name: "a guest's own sign-in failure leaves the visit", code: schema.ErrorCodeAuthFailed, guest: true, wantCode: schema.ErrorCodeAuthFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeAPIError(w, http.StatusUnauthorized, tt.code) }))
			defer ts.Close()
			client, err := NewClient(ts.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			if tt.guest {
				if err := saveGuestVisit(ts.URL, storedGuestVisit{Token: "guest-token", Handle: "guest-x", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				client.guestServer = client.Server()
			}
			_, err = client.Reauth(context.Background(), schema.ReauthRequestV1{Password: "x"})
			if tt.wantCode != "" && !IsServerCode(err, tt.wantCode) {
				t.Errorf("err = %v, want the server's %s", err, tt.wantCode)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("err = %v, want %v", err, tt.wantIs)
			}
			if tt.guest {
				token, _ := LoadToken(ts.URL)
				if ended := errors.Is(tt.wantIs, ErrGuestVisitEnded); ended != (token == "") {
					t.Errorf("stored visit token = %q after %v", token, err)
				}
			}
		})
	}
}

// A request refused with reauth_required is sent once more after the proof,
// with the same body; a second refusal is the answer.
func TestRoundTripReauthOnce(t *testing.T) {
	for _, refusals := range []int{1, 2} {
		t.Run(either(refusals == 1, "proved", "still refused"), func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(b))
				n := len(bodies)
				mu.Unlock()
				if n <= refusals {
					writeAPIError(w, http.StatusForbidden, schema.ErrorCodeReauthRequired)
					return
				}
				_ = json.NewEncoder(w).Encode(schema.JoinSettingsResponseV1{ServerName: "conch"})
			}))
			defer ts.Close()
			client, err := NewClient(ts.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			proofs := 0
			client.reauth = func(context.Context) error { proofs++; return nil }
			on := true
			_, err = client.PutJoinSettings(context.Background(), schema.PutJoinSettingsRequestV1{GuestsEnabled: &on})
			if refusals == 1 && err != nil {
				t.Fatalf("after the proof: %v", err)
			}
			if refusals == 2 && !IsServerCode(err, schema.ErrorCodeReauthRequired) {
				t.Fatalf("a second refusal = %v, want it as the answer", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if proofs != 1 || len(bodies) != 2 || bodies[0] != bodies[1] || bodies[0] == "" {
				t.Errorf("proofs %d, bodies %q; want one proof and the same body twice", proofs, bodies)
			}
		})
	}
}

// A server without the new routes is an older one, said so, and nothing is
// assumed about what it would have done.
func TestOlderServer(t *testing.T) {
	isolateConfig(t)
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()
	for _, args := range [][]string{{"join", "info"}, {"join", "guest"}} {
		_, _, err := runCLI(t, "", append(args, "--server", ts.URL)...)
		if err == nil || !strings.Contains(err.Error(), "it is older than this conch") {
			t.Errorf("%v = %v", args, err)
		}
	}
	if _, err := LoadToken(ts.URL); err != nil {
		t.Fatal(err)
	}
}

// Ending a visit when no answer comes is tried again, and when it is never
// confirmed the visit stays stored, and the person is told.
func TestEndVisitUnconfirmed(t *testing.T) {
	isolateConfig(t)
	oldPause := endVisitPause
	endVisitPause = time.Millisecond
	t.Cleanup(func() { endVisitPause = oldPause })
	var mu sync.Mutex
	attempts := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		writeAPIError(w, http.StatusServiceUnavailable, "unavailable")
	}))
	defer ts.Close()
	if err := saveGuestVisit(ts.URL, storedGuestVisit{Token: fakeToken, Handle: "guest-x", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runCLI(t, "", "end-visit", "--server", ts.URL)
	if err == nil || !strings.Contains(err.Error(), "not confirmed") || !strings.Contains(stderr, "still stored here") {
		t.Errorf("end-visit = %v, stderr %q", err, stderr)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != endVisitAttempts {
		t.Errorf("%d attempts, want %d", attempts, endVisitAttempts)
	}
	if token, _ := LoadToken(ts.URL); token != fakeToken {
		t.Errorf("an unconfirmed end dropped the visit (token %q)", token)
	}
}

// redirectTransport sends every request to target, whatever host its URL
// names, so a client can be pointed at an address that is not loopback.
type redirectTransport struct{ target string }

func (rt redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = "http", strings.TrimPrefix(rt.target, "http://")
	return http.DefaultTransport.RoundTrip(r)
}

// reauthFake answers every route with reauth_required but whoami, and
// counts the proofs it is sent.
func reauthFake(t *testing.T) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	proofs := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/whoami":
			_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: "root", Role: schema.RoleMember})
			return
		case "/v1/reauth":
			mu.Lock()
			proofs++
			mu.Unlock()
		}
		writeAPIError(w, http.StatusForbidden, schema.ErrorCodeReauthRequired)
	}))
	t.Cleanup(ts.Close)
	return ts, func() int {
		mu.Lock()
		defer mu.Unlock()
		return proofs
	}
}

// A reauth_required from a plain-http address that is not this machine is
// never answered with a password: anyone there could have forged it.
func TestReauthNeverOverPlainHTTP(t *testing.T) {
	ts, proofs := reauthFake(t)
	client, err := NewClient("http://conch.example:8080", &http.Client{Transport: redirectTransport{ts.URL}})
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	stdin := strings.NewReader(memberPassword + "\n")
	askToReauth(client, newPrompter(context.Background(), stdin, &stderr), &stderr)
	on := true
	_, err = client.PutJoinSettings(context.Background(), schema.PutJoinSettingsRequestV1{GuestsEnabled: &on})
	if err == nil || !strings.Contains(err.Error(), "will not send a password, code or sign-in token over plain http to conch.example:8080") {
		t.Fatalf("err = %v, want the https refusal", err)
	}
	if proofs() != 0 || stderr.Len() != 0 || stdin.Len() == 0 {
		t.Errorf("%d proofs sent, stderr %q, stdin read %v; want nothing asked or sent", proofs(), stderr.String(), stdin.Len() == 0)
	}
}

// A server that answers the proof itself with reauth_required gets one
// question and one proof, then the error.
func TestReauthDoesNotRecurse(t *testing.T) {
	ts, proofs := reauthFake(t)
	client, err := NewClient(ts.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	answers := strings.Repeat(memberPassword+"\n", 5)
	askToReauth(client, newPrompter(context.Background(), strings.NewReader(answers), &stderr), &stderr)
	on := true
	_, err = client.PutJoinSettings(context.Background(), schema.PutJoinSettingsRequestV1{GuestsEnabled: &on})
	if err == nil {
		t.Fatal("a proof the server keeps refusing succeeded")
	}
	if proofs() != 1 || strings.Count(stderr.String(), "This needs a recent proof") != 1 {
		t.Errorf("%d proofs, stderr %q; want one question and one proof", proofs(), stderr.String())
	}
}

// A secret typed where a subcommand goes is not repeated in the refusal.
func TestUnknownSubcommandNotRepeated(t *testing.T) {
	const secret = "hunter2hunter2hunter2"
	for _, cmd := range []string{"password", "device-link", "join", "account"} {
		t.Run(cmd, func(t *testing.T) {
			isolateConfig(t)
			stdout, stderr, err := runCLI(t, "", cmd, secret)
			if err == nil {
				t.Fatal("an unknown subcommand was taken")
			}
			if strings.Contains(err.Error()+stdout+stderr, secret) {
				t.Errorf("the refusal repeats it: %v", err)
			}
		})
	}
}

// A sign-in suggestion quotes the name for a shell, and gives no command for
// a name no principal may have.
func TestSignInHint(t *testing.T) {
	tests := []struct{ name, want string }{
		{"bob", "sign in with: conch login --password --name 'bob'"},
		{"o'brien", `sign in with: conch login --password --name 'o'\''brien'`},
		{"$(rm -rf ~); `x`", "sign in with: conch login --password --name '$(rm -rf ~); `x`'"},
		{"evil\nname", "sign in with your new name: conch login --password"},
		{"esc\x1b[31m", "sign in with your new name: conch login --password"},
		{"", "sign in with your new name: conch login --password"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := signInHint(tt.name); got != tt.want {
				t.Errorf("signInHint(%q) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}
