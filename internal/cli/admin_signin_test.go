package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin principal sign-in-link` (issue #493), against a real conchd
// handler.

var signInLinkPattern = regexp.MustCompile(`^(http://127\.0\.0\.1:\d+)/app/#/signin/(conch_sil_[A-Za-z0-9_-]{43})\n$`)

// useSignInLink uses a sign-in link over HTTP, as a browser would, and
// returns the status and, on success, the answer.
func (e *adminEnv) useSignInLink(t *testing.T, code string) (int, schema.RedeemSignInLinkResponseV1) {
	t.Helper()
	body, err := json.Marshal(schema.RedeemSignInLinkRequestV1{Code: code})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(e.url+"/v1/sign-in-links/redeem", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out schema.RedeemSignInLinkResponseV1
	if resp.StatusCode == http.StatusCreated {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, out
}

// The link is printed alone on standard output, its code nowhere else, and
// using it signs the same member in again, once.
func TestAdminPrincipalSignInLink(t *testing.T) {
	e := newAdminEnv(t)
	id := strconv.FormatInt(e.memberID, 10)
	stdout, stderr, err := e.admin(t, e.operatorToken, "", "principal", "sign-in-link", id)
	if err != nil {
		t.Fatal(err)
	}
	m := signInLinkPattern.FindStringSubmatch(stdout)
	if m == nil || m[1] != e.url {
		t.Fatalf("stdout = %q, want the link alone on one line", stdout)
	}
	code := m[2]
	if strings.Contains(stderr, code) || !strings.HasPrefix(stderr, "sign-in link 1 for principal "+id+", one use, expires ") {
		t.Errorf("stderr = %q, want the link's id, person and expiry, and not its code", stderr)
	}
	status, got := e.useSignInLink(t, code)
	if status != http.StatusCreated || got.Principal.ID != e.memberID || e.whoamiStatus(t, got.Token) != http.StatusOK {
		t.Fatalf("use = %d %+v, want the member signed in", status, got.Principal)
	}
	if status, _ := e.useSignInLink(t, code); status != http.StatusNotFound {
		t.Errorf("second use = %d, want 404", status)
	}

	// --expires sets how long it lasts; --json prints the server's document.
	stdout, _, err = e.admin(t, e.operatorToken, "", "principal", "sign-in-link", id, "--expires", "2h", "--json")
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	var resp schema.CreateSignInLinkResponseV1
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode %q: %v", stdout, err)
	}
	if left := time.Until(resp.SignInLink.ExpiresAt.Time()); left < time.Hour+50*time.Minute || left > 2*time.Hour {
		t.Errorf("expires in %v, want about 2h", left)
	}
	if schema.ValidateSignInLinkCode(resp.Code) != nil || resp.SignInLink.PrincipalID != e.memberID {
		t.Errorf("response = %+v", resp.SignInLink)
	}
}

// The CLI decides nothing about who may: the server refuses a member, and a
// link for an operator or for nobody, with its code, and nothing is printed.
func TestAdminPrincipalSignInLinkRefused(t *testing.T) {
	e := newAdminEnv(t)
	tests := []struct {
		name, token, id, code string
	}{
		{"a member asks", e.memberToken, strconv.FormatInt(e.memberID, 10), "forbidden"},
		{"for the operator", e.operatorToken, "1", "invalid_request"},
		{"for nobody", e.operatorToken, "99", "principal_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := e.admin(t, tt.token, "", "principal", "sign-in-link", tt.id)
			if err == nil || !strings.Contains(err.Error(), tt.code) {
				t.Fatalf("error = %v, want %s", err, tt.code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

// Mistakes the CLI can see are refused before any request.
func TestAdminPrincipalSignInLinkUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no id", []string{"principal", "sign-in-link"}, "expected <id>"},
		{"bad id", []string{"principal", "sign-in-link", "x"}, "id must be a positive integer"},
		{"bad expiry", []string{"principal", "sign-in-link", "2", "--expires", "soon"}, "--expires must be a duration"},
		{"too long", []string{"principal", "sign-in-link", "2", "--expires", "8d"}, "expires_at must be within"},
		{"past", []string{"principal", "sign-in-link", "2", "--expires", "2001-01-01T00:00:00Z"}, "must be in the future"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer fake.Close()
			stdout, stderr, err := runCLI(t, "", append(append([]string{"admin"}, tt.args...), "--server", fake.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if stdout != "" || stderr != "" || requests.Load() != 0 {
				t.Errorf("stdout %q, stderr %q, %d requests; want none", stdout, stderr, requests.Load())
			}
		})
	}
}

// A code that is not shaped like one is never printed, and the error does not
// repeat it.
func TestAdminPrincipalSignInLinkRefusesMalformedCode(t *testing.T) {
	good := schema.SignInLinkCodePrefix + strings.Repeat("A", 43)
	tests := []struct {
		name, code string
		wantOK     bool
	}{
		{"well formed", good, true},
		{"an invite code", schema.InviteCodePrefix + strings.Repeat("A", 43), false},
		{"escape sequence", good[:20] + "\x1b[2J" + good[24:], false},
		{"too long", good + "A", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_ = json.NewEncoder(w).Encode(schema.CreateSignInLinkResponseV1{SignInLink: schema.SignInLinkV1{ID: 1, PrincipalID: 2}, Code: tt.code})
			}))
			defer fake.Close()
			stdout, stderr, err := runCLI(t, "", "admin", "principal", "sign-in-link", "2", "--server", fake.URL)
			if tt.wantOK {
				if err != nil || stdout != fake.URL+"/app/#/signin/"+good+"\n" {
					t.Errorf("err = %v, stdout = %q; want the link", err, stdout)
				}
				return
			}
			if err == nil || stdout != "" {
				t.Fatalf("err = %v, stdout = %q; want a refusal and no output", err, stdout)
			}
			if tt.code != "" && strings.Contains(err.Error()+stderr, tt.code) {
				t.Errorf("the refusal repeats the code")
			}
		})
	}
}

// `conch admin sign-in-link close <id>` stops a link that leaked: it no
// longer works, closing it again says the same, and the server's refusals
// come through with their codes.
func TestAdminSignInLinkClose(t *testing.T) {
	e := newAdminEnv(t)
	stdout, _, err := e.admin(t, e.operatorToken, "", "principal", "sign-in-link", strconv.FormatInt(e.memberID, 10))
	if err != nil {
		t.Fatal(err)
	}
	m := signInLinkPattern.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("stdout = %q", stdout)
	}
	for range 2 {
		out, _, err := e.admin(t, e.operatorToken, "", "sign-in-link", "close", "1")
		if err != nil || out != "closed sign-in link 1\n" {
			t.Fatalf("close = %q, %v", out, err)
		}
	}
	if status, _ := e.useSignInLink(t, m[2]); status != http.StatusNotFound {
		t.Errorf("use after close = %d, want 404", status)
	}
	if out, _, err := e.admin(t, e.operatorToken, "", "sign-in-link", "close", "1", "--json"); err != nil || out != "" {
		t.Errorf("--json close = %q, %v; want no document", out, err)
	}
	for _, c := range []struct{ name, token, id, code string }{
		{"a member", e.memberToken, "1", "forbidden"},
		{"unknown", e.operatorToken, "99", schema.ErrorCodeSignInLinkNotFound},
	} {
		out, _, err := e.admin(t, c.token, "", "sign-in-link", "close", c.id)
		if err == nil || !strings.Contains(err.Error(), c.code) || out != "" {
			t.Errorf("%s: %q, %v; want %s and no output", c.name, out, err, c.code)
		}
	}
}

func TestAdminSignInLinkCloseUsageErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"no id", []string{"sign-in-link", "close"}, "expected <id>"},
		{"bad id", []string{"sign-in-link", "close", "x"}, "id must be a positive integer"},
		{"unknown verb", []string{"sign-in-link", "open", "1"}, "conch admin sign-in-link <close>"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer fake.Close()
			_, _, err := runCLI(t, "", append(append([]string{"admin"}, tt.args...), "--server", fake.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if requests.Load() != 0 {
				t.Errorf("%d requests sent, want none", requests.Load())
			}
		})
	}
}
