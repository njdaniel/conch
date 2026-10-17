package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin invite …` and the principal commands that administer invites
// (issue #438), against a real conchd handler.

var inviteLinkPattern = regexp.MustCompile(`^(http://127\.0\.0\.1:\d+)/app/#/invite/(conch_inv_[A-Za-z0-9_-]{43})\n$`)

// redeemCode uses an invite over HTTP, as a browser would, and returns the
// new member's token.
func (e *adminEnv) redeemCode(t *testing.T, code, name string) string {
	t.Helper()
	body, err := json.Marshal(schema.RedeemInviteRequestV1{Code: code, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(e.url+"/v1/invites/redeem", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeem %s = %d", name, resp.StatusCode)
	}
	var out schema.RedeemInviteResponseV1
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Token
}

func (e *adminEnv) whoamiStatus(t *testing.T, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.url+"/v1/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestAdminInviteCommands walks a leaked link through every invite command.
// The ids are a fresh instance's: the operator is 1, the member 2, the
// channel 1 and the open link invite 1; its joiners are 3 and 4.
func TestAdminInviteCommands(t *testing.T) {
	e := newAdminEnv(t)
	run := func(args ...string) (string, string) {
		t.Helper()
		stdout, stderr, err := e.admin(t, e.operatorToken, "", args...)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return stdout, stderr
	}
	run("channel", "create", "ops")

	stdout, stderr := run("invite", "create", "--open", "--channel", "ops", "--expires", "24h", "--max-uses", "5", "--voice")
	m := inviteLinkPattern.FindStringSubmatch(stdout)
	if m == nil || m[1] != e.url {
		t.Fatalf("stdout = %q, want the link alone on one line", stdout)
	}
	code := m[2]
	if strings.Contains(stderr, code) || !strings.HasPrefix(stderr, "invite 1 open uses 0/5 expires ") {
		t.Errorf("stderr = %q, want the invite's id and expiry and not its code", stderr)
	}
	wren := e.redeemCode(t, code, "wren")
	piper := e.redeemCode(t, code, "piper")

	steps := []struct {
		name string
		args []string
		want string // a regular expression for the whole of stdout
	}{
		{"list", []string{"invite", "list"}, `^1 open open uses 2/5 expires \S+ by 1 channels ops\n$`},
		{"list, filtered", []string{"invite", "list", "--kind", "personal"}, `^$`},
		{"joiners", []string{"invite", "joiners", "1"}, `^3 enabled \S+ wren\n4 enabled \S+ piper\n$`},
		{"dry run", []string{"invite", "disable-joiners", "1"}, `^would disable 2 principal\(s\): 3,4; run again with --yes to do it and close invite 1\n$`},
		{"still open", []string{"invite", "list", "--state", "open"}, `^1 open open `},
		{"disable them", []string{"invite", "disable-joiners", "1", "--yes"}, `^disabled 2 principal\(s\): 3,4\nclosed invite 1\n$`},
		{"closed", []string{"invite", "list", "--state", "closed", "--created-by", "1"}, `^1 open closed uses 2/5 `},
		{"joiners disabled", []string{"invite", "joiners", "1"}, `^3 disabled \S+ wren\n4 disabled \S+ piper\n$`},
		{"pause", []string{"invite", "pause"}, `^invites are paused\n$`},
		{"status", []string{"invite", "status"}, `^invites are paused\n$`},
		{"resume", []string{"invite", "resume"}, `^invites are open\n$`},
		{"grant", []string{"principal", "invite-permission", "2", "on"}, `^2 may invite\n$`},
		{"revoke", []string{"principal", "invite-permission", "2", "off"}, `^2 may no longer invite; their open personal invites are closed\n$`},
		{"invitees, dry run", []string{"principal", "disable-invitees", "1", "--all"}, `^would disable 0 principal\(s\): -; run again with --yes to do it\n$`},
		{"invitees", []string{"principal", "disable-invitees", "1", "--yes"}, `^disabled 0 principal\(s\): -\n$`},
	}
	for _, st := range steps {
		stdout, _ := run(st.args...)
		if !regexp.MustCompile(st.want).MatchString(stdout) {
			t.Errorf("%s: stdout = %q, want %s", st.name, stdout, st.want)
		}
	}
	for name, tok := range map[string]string{"wren": wren, "piper": piper} {
		if got := e.whoamiStatus(t, tok); got != http.StatusUnauthorized {
			t.Errorf("%s whoami after disable-joiners = %d, want 401", name, got)
		}
	}

	// A personal invite, then closed by id.
	stdout, _ = run("invite", "create", "--personal", "--channel", "ops", "--expires", "7d")
	if !inviteLinkPattern.MatchString(stdout) {
		t.Fatalf("personal create stdout = %q", stdout)
	}
	if out, _ := run("invite", "close", "2"); out != "closed invite 2\n" {
		t.Errorf("close = %q", out)
	}
}

// Pausing refuses redemption until resumed.
func TestAdminInvitePauseRefusesRedeem(t *testing.T) {
	e := newAdminEnv(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "channel", "create", "ops"); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := e.admin(t, e.operatorToken, "", "invite", "create", "--open", "--channel", "ops", "--expires", "1h")
	if err != nil {
		t.Fatal(err)
	}
	code := inviteLinkPattern.FindStringSubmatch(stdout)[2]
	if _, _, err := e.admin(t, e.operatorToken, "", "invite", "pause"); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(schema.RedeemInviteRequestV1{Code: code, Name: "late"})
	resp, err := http.Post(e.url+"/v1/invites/redeem", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("redeem while paused = %d, want 503", resp.StatusCode)
	}
	if _, _, err := e.admin(t, e.operatorToken, "", "invite", "create", "--open", "--channel", "ops", "--expires", "1h"); err == nil || !strings.Contains(err.Error(), schema.ErrorCodeInvitesPaused) {
		t.Errorf("create while paused = %v, want invites_paused", err)
	}
	if _, _, err := e.admin(t, e.operatorToken, "", "invite", "resume"); err != nil {
		t.Fatal(err)
	}
	e.redeemCode(t, code, "late")
}

// The code is printed once, inside the link on standard output, and appears
// in no other output of any invite command, nor in any error.
func TestAdminInviteCodePrintedOnce(t *testing.T) {
	e := newAdminEnv(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "channel", "create", "ops"); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := e.admin(t, e.operatorToken, "", "invite", "create", "--open", "--channel", "ops", "--expires", "1h")
	if err != nil {
		t.Fatal(err)
	}
	m := inviteLinkPattern.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("stdout = %q", stdout)
	}
	code := m[2]
	all := []string{stderr}
	for _, args := range [][]string{
		{"invite", "list"}, {"invite", "list", "--json"}, {"invite", "joiners", "1"}, {"invite", "joiners", "1", "--json"},
		{"invite", "disable-joiners", "1"}, {"invite", "status"}, {"invite", "close", "1"},
		{"invite", "close", "99"}, {"invite", "joiners", "99"}, {"invite", "list", "--state", "nonsense"},
	} {
		out, errOut, err := e.admin(t, e.operatorToken, "", args...)
		all = append(all, out, errOut)
		if err != nil {
			all = append(all, err.Error())
		}
	}
	// A member's refusals too.
	for _, args := range [][]string{{"invite", "joiners", "1"}, {"invite", "create", "--open", "--channel", "ops", "--expires", "1h"}} {
		out, errOut, err := e.admin(t, e.memberToken, "", args...)
		all = append(all, out, errOut)
		if err != nil {
			all = append(all, err.Error())
		}
	}
	for _, text := range all {
		if strings.Contains(text, code) {
			t.Errorf("the code appears in %q", text)
		}
	}
	if strings.Count(stdout, code) != 1 {
		t.Errorf("the link carries the code %d times", strings.Count(stdout, code))
	}
}

// A code that is not shaped like one is not printed, whatever it holds, and
// the error does not repeat it; a malformed response does not carry it into
// the error either.
func TestAdminInviteCreateRefusesMalformedCode(t *testing.T) {
	good := schema.InviteCodePrefix + strings.Repeat("A", 43)
	tests := []struct {
		name   string
		body   func() []byte
		secret string
		wantOK bool
	}{
		{"well formed", func() []byte { return createResponse(t, good) }, good, true},
		{"empty", func() []byte { return createResponse(t, "") }, "", false},
		{"too long", func() []byte { return createResponse(t, good+"A") }, good + "A", false},
		{"escape sequence", func() []byte { return createResponse(t, good[:20]+"\x1b[2J"+good[24:]) }, good[:20] + "\x1b[2J" + good[24:], false},
		{"a credential token", func() []byte { return createResponse(t, schema.CredentialTokenPrefix+strings.Repeat("A", 43)) }, schema.CredentialTokenPrefix + strings.Repeat("A", 43), false},
		{"truncated response", func() []byte { return []byte(`{"code":"` + good + `"`) }, good, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write(tt.body())
			}))
			defer fake.Close()
			var out, errOut bytes.Buffer
			err := RunWithStdin(context.Background(), []string{"admin", "invite", "create", "--open", "--channel", "ops", "--expires", "1h", "--server", fake.URL},
				strings.NewReader(""), &out, &errOut, "vtest")
			if tt.wantOK {
				if err != nil || out.String() != fake.URL+"/app/#/invite/"+good+"\n" {
					t.Errorf("err = %v, stdout = %q; want the link", err, out.String())
				}
				return
			}
			if err == nil || out.Len() != 0 {
				t.Fatalf("err = %v, stdout = %q; want a refusal and no output", err, out.String())
			}
			if tt.secret != "" && strings.Contains(err.Error()+errOut.String(), tt.secret) {
				t.Errorf("the refusal repeats the code")
			}
		})
	}
}

func createResponse(t *testing.T, code string) []byte {
	t.Helper()
	b, err := json.Marshal(schema.CreateInviteResponseV1{
		Invite: schema.InviteV1{ID: 1, Kind: schema.InviteKindOpen, Channels: []schema.InviteChannelV1{}},
		Code:   code,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAdminInviteJSON decodes each --json output strictly into the schema
// type the route documents.
func TestAdminInviteJSON(t *testing.T) {
	e := newAdminEnv(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "channel", "create", "ops"); err != nil {
		t.Fatal(err)
	}
	strict := func(t *testing.T, raw string, into any) {
		t.Helper()
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(into); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if dec.More() {
			t.Fatalf("extra data after the document in %q", raw)
		}
	}
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, out string)
	}{
		{"create", []string{"invite", "create", "--open", "--channel", "ops", "--expires", "1h"}, func(t *testing.T, out string) {
			var r schema.CreateInviteResponseV1
			strict(t, out, &r)
			if r.Invite.ID != 1 || schema.ValidateInviteCode(r.Code) != nil {
				t.Errorf("response = %+v", r.Invite)
			}
		}},
		{"list", []string{"invite", "list"}, func(t *testing.T, out string) {
			var r schema.ListInvitesResponseV1
			strict(t, out, &r)
			if len(r.Invites) != 1 {
				t.Errorf("invites = %+v", r.Invites)
			}
		}},
		{"joiners", []string{"invite", "joiners", "1"}, func(t *testing.T, out string) {
			var r schema.InviteJoinersResponseV1
			strict(t, out, &r)
			if r.Joiners == nil || len(r.Joiners) != 0 {
				t.Errorf("joiners = %+v, want []", r.Joiners)
			}
		}},
		{"disable-joiners", []string{"invite", "disable-joiners", "1"}, func(t *testing.T, out string) {
			var r schema.DisableJoinersResponseV1
			strict(t, out, &r)
			if !r.DryRun || r.Count != 0 || r.PrincipalIDs == nil {
				t.Errorf("response = %+v", r)
			}
		}},
		{"disable-invitees", []string{"principal", "disable-invitees", "1", "--all"}, func(t *testing.T, out string) {
			var r schema.DisableJoinersResponseV1
			strict(t, out, &r)
			if !r.DryRun {
				t.Errorf("response = %+v", r)
			}
		}},
		{"status", []string{"invite", "status"}, func(t *testing.T, out string) {
			var r schema.InviteSettingsV1
			strict(t, out, &r)
		}},
		{"pause", []string{"invite", "pause"}, func(t *testing.T, out string) {
			var r schema.InviteSettingsV1
			strict(t, out, &r)
			if !r.Paused {
				t.Error("not paused")
			}
		}},
		{"invite-permission has no document", []string{"principal", "invite-permission", "2", "on"}, func(t *testing.T, out string) {
			if out != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := e.admin(t, e.operatorToken, "", append(tt.args, "--json")...)
			if err != nil {
				t.Fatalf("%v: %v", tt.args, err)
			}
			tt.check(t, stdout)
		})
	}
}

// TestAdminInviteMemberRefused: the CLI decides nothing; the server refuses a
// member every operator route, with its code.
func TestAdminInviteMemberRefused(t *testing.T) {
	e := newAdminEnv(t)
	tests := []struct {
		name string
		args []string
		code string
	}{
		{"joiners", []string{"invite", "joiners", "1"}, "forbidden"},
		{"disable-joiners", []string{"invite", "disable-joiners", "1", "--yes"}, "forbidden"},
		{"pause", []string{"invite", "pause"}, "forbidden"},
		{"resume", []string{"invite", "resume"}, "forbidden"},
		{"status", []string{"invite", "status"}, "forbidden"},
		{"invite-permission", []string{"principal", "invite-permission", "2", "on"}, "forbidden"},
		{"disable-invitees", []string{"principal", "disable-invitees", "1", "--yes"}, "forbidden"},
		// Making an invite is a person's route; this member may not.
		{"create", []string{"invite", "create", "--personal", "--channel", "ops", "--expires", "1h"}, schema.ErrorCodeInviteNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := e.admin(t, e.memberToken, "", tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.code) {
				t.Fatalf("error = %v, want %s", err, tt.code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
	// A member lists their own invites: none.
	stdout, _, err := e.admin(t, e.memberToken, "", "invite", "list")
	if err != nil || stdout != "" {
		t.Errorf("member list = %q, %v; want nothing", stdout, err)
	}
}

// Mistakes the CLI can see are refused before any request.
func TestAdminInviteUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no kind", []string{"invite", "create", "--channel", "ops", "--expires", "1h"}, "exactly one of --open and --personal"},
		{"both kinds", []string{"invite", "create", "--open", "--personal", "--channel", "ops", "--expires", "1h"}, "exactly one of --open and --personal"},
		{"no channel", []string{"invite", "create", "--open", "--expires", "1h"}, "--channel is required"},
		{"no expiry", []string{"invite", "create", "--open", "--channel", "ops"}, "--expires is required"},
		{"bad expiry", []string{"invite", "create", "--open", "--channel", "ops", "--expires", "soon"}, "--expires must be a duration"},
		{"zero days", []string{"invite", "create", "--open", "--channel", "ops", "--expires", "0d"}, "--expires must be a duration"},
		{"past expiry", []string{"invite", "create", "--open", "--channel", "ops", "--expires", "2001-01-01T00:00:00Z"}, "must be in the future"},
		{"open link too long", []string{"invite", "create", "--open", "--channel", "ops", "--expires", "8d"}, "expires_at must be within"},
		{"personal with a cap", []string{"invite", "create", "--personal", "--channel", "ops", "--expires", "1h", "--max-uses", "3"}, "only an open invite takes max_uses"},
		{"zero cap", []string{"invite", "create", "--open", "--channel", "ops", "--expires", "1h", "--max-uses", "0"}, "max_uses must be between"},
		{"credential days", []string{"invite", "create", "--open", "--channel", "ops", "--expires", "1h", "--credential-days", "400"}, "--credential-days must be between"},
		{"same channel twice", []string{"invite", "create", "--open", "--channel", "ops", "--channel", "ops", "--expires", "1h"}, "names a channel twice"},
		{"close without id", []string{"invite", "close"}, "expected <id>"},
		{"joiners bad id", []string{"invite", "joiners", "x"}, "id must be a positive integer"},
		{"pause takes nothing", []string{"invite", "pause", "now"}, "expected <>"},
		{"permission without a value", []string{"principal", "invite-permission", "2"}, "expected <id> <on|off>"},
		{"permission yes", []string{"principal", "invite-permission", "2", "yes"}, "must be on or off"},
		{"list negative maker", []string{"invite", "list", "--created-by", "-1"}, "--created-by must be a positive integer"},
		{"unknown verb", []string{"invite", "frob"}, "conch admin invite <create|list|close|joiners|disable-joiners|pause|resume|status>"},
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
			if stdout != "" || stderr != "" {
				t.Errorf("stdout %q, stderr %q; want both empty", stdout, stderr)
			}
			if n := requests.Load(); n != 0 {
				t.Errorf("%d requests were sent, want none", n)
			}
		})
	}
}

func TestParseInviteExpiry(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in      string
		want    time.Time
		wantErr bool
	}{
		{"24h", now.Add(24 * time.Hour), false},
		{"90m", now.Add(90 * time.Minute), false},
		{"7d", now.Add(7 * 24 * time.Hour), false},
		{"2026-10-20T00:00:00Z", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), false},
		{"", time.Time{}, true},
		{"0d", time.Time{}, true},
		{"-1h", time.Time{}, true},
		{"d", time.Time{}, true},
		{"tomorrow", time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseInviteExpiry(tt.in, now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !got.Equal(tt.want) {
				t.Errorf("= %v, want %v", got, tt.want)
			}
			if err != nil && len(tt.in) > 3 && strings.Contains(err.Error(), tt.in) {
				t.Errorf("the error repeats the input: %v", err)
			}
		})
	}
}

func TestInviteState(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) schema.Timestamp { return schema.NewTimestamp(now.Add(d)) }
	one := int64(1)
	closedAt := ts(-time.Minute)
	tests := []struct {
		name string
		inv  schema.InviteV1
		want string
	}{
		{"open", schema.InviteV1{ExpiresAt: ts(time.Hour)}, "open"},
		{"open with a cap left", schema.InviteV1{ExpiresAt: ts(time.Hour), MaxUses: &one}, "open"},
		{"used", schema.InviteV1{ExpiresAt: ts(time.Hour), MaxUses: &one, Uses: 1}, "used"},
		{"expired", schema.InviteV1{ExpiresAt: ts(-time.Hour)}, "expired"},
		{"closed wins", schema.InviteV1{ExpiresAt: ts(-time.Hour), MaxUses: &one, Uses: 1, ClosedAt: &closedAt}, "closed"},
		{"used wins over expired", schema.InviteV1{ExpiresAt: ts(-time.Hour), MaxUses: &one, Uses: 1}, "used"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inviteState(tt.inv, now); got != tt.want {
				t.Errorf("state = %s, want %s", got, tt.want)
			}
		})
	}
}
