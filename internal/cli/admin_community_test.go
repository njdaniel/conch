package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The community model's admin commands (ADR-013, issue #994) against a real
// conchd handler, and against a fake server that refuses.

// makeDefaultChannel makes channel ops a public default channel, as an
// operator, through the commands under test's own route.
func (e *adminEnv) makeDefaultChannel(t *testing.T) {
	const name = "ops"
	t.Helper()
	for _, args := range [][]string{
		{"channel", "create", name},
		{"channel", "settings", name, "--public"},
		{"channel", "settings", name, "--default", "on"},
	} {
		if _, _, err := e.admin(t, e.operatorToken, "", args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
}

func TestAdminChannelSettings(t *testing.T) {
	e := newAdminEnv(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "channel", "create", "ops"); err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		name       string
		args       []string
		wantStdout string
		// setup is a step whose output is not the test's concern.
		setup bool
	}{
		{"public", []string{"channel", "settings", "ops", "--public"}, "channel ops is now public\n", false},
		{"default", []string{"channel", "settings", "ops", "--default", "on"}, "channel ops is now a default channel: new members start in it\n", false},
		{"flags may come first", []string{"channel", "settings", "--default", "off", "ops"}, "channel ops is no longer a default channel\n", false},
		{"private keeps its members", []string{"channel", "settings", "ops", "--private"}, "channel ops is now private; its members were kept and still see it\n", false},
		{"both at once", []string{"channel", "settings", "ops", "--public", "--default", "on"}, "channel ops is now public\nchannel ops is now a default channel: new members start in it\n", false},
		{"open sign-up on", []string{"join", "set", "--open-signup", "on"}, "", true},
		{"the last default goes", []string{"channel", "settings", "ops", "--default", "off"}, "channel ops is no longer a default channel\nopen sign-up is now off: no channel is a default channel\n", false},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			stdout, stderr, err := e.admin(t, e.operatorToken, "", st.args...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if st.setup {
				return
			}
			if stdout != st.wantStdout || stderr != "" {
				t.Errorf("stdout = %q, stderr = %q; want %q and nothing", stdout, stderr, st.wantStdout)
			}
		})
	}
}

// `channel settings --archive|--restore` (issue #844) against a real handler:
// the answer says what happened, `channels list` marks the archived channel
// (for the operator's --all listing and the member's own), the default
// channel refuses, and restoring puts it back.
func TestAdminChannelArchiveRestore(t *testing.T) {
	e := newAdminEnv(t)
	for _, args := range [][]string{{"channel", "create", "old"}, {"channel", "settings", "old", "--public"}} {
		if _, _, err := e.admin(t, e.operatorToken, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	e.makeDefaultChannel(t)
	steps := []struct {
		name       string
		token      string
		args       []string
		wantStdout string
		wantCode   string
	}{
		{"archive", e.operatorToken, []string{"channel", "settings", "old", "--archive"}, "channel old is now archived: it keeps its history and members and refuses anything new until it is restored\n", ""},
		{"archived again changes nothing", e.operatorToken, []string{"channel", "settings", "old", "--archive"}, "channel old is now archived: it keeps its history and members and refuses anything new until it is restored\n", ""},
		{"the default channel refuses", e.operatorToken, []string{"channel", "settings", "ops", "--archive"}, "", schema.ErrorCodeChannelArchiveDefault},
		{"archive and default together refuse", e.operatorToken, []string{"channel", "settings", "old", "--default", "on"}, "", schema.ErrorCodeChannelArchiveDefault},
		{"a member may not", e.memberToken, []string{"channel", "settings", "old", "--restore"}, "", "forbidden"},
		{"list marks it", e.operatorToken, []string{"channels", "list", "--all"}, "", ""},
		{"restore", e.operatorToken, []string{"channel", "settings", "old", "--restore"}, "channel old is restored: posting, joining and voice work again\n", ""},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			run := e.admin
			if st.args[0] == "channels" {
				run = func(t *testing.T, token, _ string, args ...string) (string, string, error) {
					return e.cli(t, token, args...)
				}
			}
			stdout, _, err := run(t, st.token, "", st.args...)
			if st.wantCode != "" {
				if !IsServerCode(err, st.wantCode) {
					t.Fatalf("error = %v, want the server's %s", err, st.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if st.name == "list marks it" {
				for _, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
					fields := strings.Fields(line)
					if len(fields) < 7 {
						t.Fatalf("line %q has %d fields, want 7", line, len(fields))
					}
					if archived := fields[6] == "old"; (fields[5] == "archived") != archived {
						t.Errorf("line %q: archived column = %q", line, fields[5])
					}
				}
				return
			}
			if stdout != st.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout, st.wantStdout)
			}
		})
	}
	stdout, _, err := e.cli(t, e.operatorToken, "channels", "list", "--all")
	if err != nil || strings.Contains(stdout, "archived") {
		t.Errorf("after restore, channels list --all = %q, %v; want no archived mark", stdout, err)
	}
}

// The default mark can be refused with a warning raised; the warning is shown
// in the permissions command's words.
func TestAdminChannelSettingsRefusals(t *testing.T) {
	e := newAdminEnv(t)
	for _, args := range [][]string{{"channel", "create", "ops"}, {"channel", "create", "lounge"}, {"channel", "settings", "lounge", "--public"}} {
		if _, _, err := e.admin(t, e.operatorToken, "", args...); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name  string
		token string
		args  []string
		code  string
		line  string
	}{
		{"a private channel cannot be default", e.operatorToken, []string{"channel", "settings", "ops", "--default", "on"}, schema.ErrorCodeChannelSettingsConflict,
			"a private channel cannot be a default channel"},
		{"a member is refused", e.memberToken, []string{"channel", "settings", "lounge", "--private"}, "forbidden", "only an operator may do this"},
		{"no such channel", e.operatorToken, []string{"channel", "settings", "nowhere", "--private"}, "", "no such channel, or you cannot see it"},
	}
	// The lookup miss reads as the server's own 404 does.
	_, _, missErr := e.admin(t, e.operatorToken, "", "channel", "settings", "nowhere", "--private")
	var miss *refusalError
	if !errors.As(missErr, &miss) || miss.line != refusalLine(&ServerError{Status: 404, Code: "channel_not_found"}, channelSettingsRefusals) {
		t.Errorf("a lookup miss = %v, want the refusal line of a 404 channel_not_found", missErr)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := e.admin(t, tt.token, "", tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.line) {
				t.Fatalf("error = %v, want a line with %q", err, tt.line)
			}
			if tt.code != "" && !IsServerCode(err, tt.code) {
				t.Errorf("error = %v, want the server's %s underneath", err, tt.code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
		})
	}
}

func TestAdminChannelSettingsUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"nothing to change", []string{"channel", "settings", "ops"}, "give at least one of --public, --private, --default, --archive and --restore"},
		{"both visibilities", []string{"channel", "settings", "ops", "--public", "--private"}, "--public or --private, not both"},
		{"default needs on or off", []string{"channel", "settings", "ops", "--default", "yes"}, "--default must be on or off"},
		{"both archive and restore", []string{"channel", "settings", "ops", "--archive", "--restore"}, "--archive or --restore, not both"},
		{"no channel", []string{"channel", "settings", "--public"}, "expected <channel>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer fake.Close()
			_, _, err := runCLI(t, "", append(append([]string{"admin"}, tt.args...), "--server", fake.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if requests.Load() != 0 {
				t.Errorf("%d requests were sent, want none", requests.Load())
			}
		})
	}
}

func TestAdminPrincipalVoice(t *testing.T) {
	e := newAdminEnv(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "principal", "create", "--kind", "agent", "bot"); err != nil {
		t.Fatal(err)
	}
	// The member is principal 2, the agent 3.
	steps := []struct {
		name       string
		args       []string
		wantStdout string
		wantCode   string
		wantLine   string
	}{
		{name: "off", args: []string{"principal", "voice", "2", "off"}, wantStdout: "voice is now off for 2\n"},
		{name: "off again", args: []string{"principal", "voice", "2", "off"}, wantStdout: "voice was already off for 2; nothing changed\n"},
		{name: "on", args: []string{"principal", "voice", "2", "on"}, wantStdout: "voice is now on for 2\n"},
		{name: "an agent", args: []string{"principal", "voice", "3", "on"}, wantCode: schema.ErrorCodeNotAPerson, wantLine: "agents do not use voice"},
		{name: "nobody", args: []string{"principal", "voice", "99", "on"}, wantCode: "principal_not_found", wantLine: "no such person"},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			stdout, _, err := e.admin(t, e.operatorToken, "", st.args...)
			if st.wantCode != "" {
				if !IsServerCode(err, st.wantCode) || !strings.Contains(err.Error(), st.wantLine) {
					t.Fatalf("error = %v, want %s in the client's words %q", err, st.wantCode, st.wantLine)
				}
				return
			}
			if err != nil || stdout != st.wantStdout {
				t.Errorf("stdout = %q, err = %v; want %q", stdout, err, st.wantStdout)
			}
		})
	}
	// The list shows the switch, and a member may not turn it.
	stdout, _, err := e.admin(t, e.operatorToken, "", "principal", "list", "--kind", "human")
	if err != nil || !strings.Contains(stdout, "voice on may-invite ") {
		t.Errorf("principal list = %q, %v; want the voice and may-invite columns", stdout, err)
	}
	if _, _, err := e.admin(t, e.memberToken, "", "principal", "voice", "2", "off"); !IsServerCode(err, "forbidden") {
		t.Errorf("a member turning voice off = %v, want the server's forbidden", err)
	}
	if _, _, err := e.admin(t, e.operatorToken, "", "principal", "voice", "2", "maybe"); err == nil || !strings.Contains(err.Error(), "must be on or off") {
		t.Errorf("voice maybe = %v", err)
	}
}

func TestAdminDirectoryMembers(t *testing.T) {
	e := newAdminEnv(t)
	stdout, _, err := e.admin(t, e.operatorToken, "", "directory", "members")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "1 human operator owner created ") || !strings.HasSuffix(lines[0], " root") ||
		!strings.HasPrefix(lines[1], "2 human member - created ") || !strings.HasSuffix(lines[1], " mallory") {
		t.Fatalf("operator's directory = %q", stdout)
	}
	// An operator also sees the operator fields; a member sees none.
	if !strings.Contains(lines[1], " voice on may-invite ") {
		t.Errorf("operator line %q has no voice column", lines[1])
	}
	stdout, _, err = e.admin(t, e.memberToken, "", "directory", "members")
	if err != nil || !strings.Contains(stdout, " voice - may-invite - invited-by - ") {
		t.Errorf("member's directory = %q, %v; want no operator fields", stdout, err)
	}
	stdout, _, err = e.admin(t, e.operatorToken, "", "directory", "members", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc schema.ServerMembersResponseV1
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil || len(doc.Members) != 2 {
		t.Errorf("--json = %q, %v", stdout, err)
	}
}

// refusalServer answers every request with one refusal.
func refusalServer(status int, code string, retryAfter string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(schema.Error{Code: code, Message: "raw " + hostile})
	}))
}

// Every new command, refused each way the routes refuse: the line is the
// client's own, chosen by the code, and nothing of the server's message or an
// unknown code reaches the output.
func TestCommunityCommandsRefusals(t *testing.T) {
	const (
		neutral = "the server does not allow you to do this"
	)
	commands := []struct {
		name string
		args []string
		// forbidden is the command's line for a 403 forbidden: operator-only
		// where the route is, agents and outsiders where it is theirs, and
		// the cause-neutral line where the cause could be either.
		forbidden string
	}{
		{"channels list", []string{"channels", "list"}, communityOnly},
		{"channels list --all", []string{"channels", "list", "--all"}, operatorOnly},
		{"channels join", []string{"channels", "join", "general"}, neutral},
		{"channels leave", []string{"channels", "leave", "general"}, communityOnly},
		{"channels create", []string{"channels", "create", "general"}, communityOnly},
		{"channel settings", []string{"admin", "channel", "settings", "general", "--private"}, operatorOnly},
		{"channel settings --archive", []string{"admin", "channel", "settings", "general", "--archive"}, operatorOnly},
		{"channel settings --restore", []string{"admin", "channel", "settings", "general", "--restore"}, operatorOnly},
		{"principal voice", []string{"admin", "principal", "voice", "9", "off"}, voiceForbidden},
		{"directory members", []string{"admin", "directory", "members"}, communityOnly},
		{"invite create", []string{"admin", "invite", "create"}, neutral},
		{"invite list", []string{"admin", "invite", "list"}, neutral},
		{"invite joiners", []string{"admin", "invite", "joiners", "9"}, neutral},
		{"invite close", []string{"admin", "invite", "close", "9"}, neutral},
		{"join status", []string{"admin", "join", "status"}, operatorOnly},
		{"join set", []string{"admin", "join", "set", "--open-signup", "on"}, operatorOnly},
	}
	refusals := []struct {
		name       string
		status     int
		code       string
		retryAfter string
		want       string
	}{
		{"403 forbidden", 403, "forbidden", "", "\x00forbidden"},
		{"403 permission", 403, schema.ErrorCodePermissionDenied, "", "you do not have the permission this needs"},
		{"404 channel", 404, "channel_not_found", "", "no such channel, or you cannot see it"},
		{"409 unknown code", 409, "something_new", "", "the server refused this (HTTP 409)"},
		{"429 with Retry-After", 429, schema.ErrorCodeRateLimited, "90", "too many tries: wait 90s and try again"},
		{"429 without it", 429, schema.ErrorCodeRateLimited, "", "too many tries: wait a while and try again"},
		{"429 with a hostile wait", 429, schema.ErrorCodeRateLimited, "99999999999", "too many tries: wait 24h and try again"},
		{"429 with a word", 429, schema.ErrorCodeRateLimited, "soon", "too many tries: wait a while and try again"},
		{"reauth", 403, schema.ErrorCodeReauthRequired, "", "this needs your password first"},
	}
	for _, c := range commands {
		for _, rf := range refusals {
			t.Run(c.name+"/"+rf.name, func(t *testing.T) {
				isolateConfig(t)
				t.Setenv("CONCH_TOKEN", "tok")
				srv := refusalServer(rf.status, rf.code, rf.retryAfter)
				defer srv.Close()
				var stdout, stderr bytes.Buffer
				err := RunWithStdin(context.Background(), append(append([]string(nil), c.args...), "--server", srv.URL), strings.NewReader(""), &stdout, &stderr, "vtest")
				if err == nil {
					t.Fatalf("a refused command succeeded; stdout = %q", stdout.String())
				}
				want := rf.want
				if want == "\x00forbidden" {
					want = c.forbidden
				}
				if want != "" && !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want %q", err.Error(), want)
				}
				all := stdout.String() + stderr.String() + err.Error()
				if strings.Contains(all, "raw") || strings.Contains(all, "H"+esc) || strings.Contains(all, rf.code) && rf.code == "something_new" {
					t.Errorf("the server's own words reached the output: %q", all)
				}
				if bad := firstUnsafe(all); bad != "" {
					t.Errorf("%s in %q", bad, all)
				}
			})
		}
	}
}

// The 409s the routes document, each in its command's words.
func TestCommunityCommandsNamedConflicts(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		status int
		code   string
		want   string
	}{
		{"create, any collision", []string{"channels", "create", "general"}, 409, "channel_exists", "there is already a channel with that name, perhaps one you are not in"},
		{"join, not joinable", []string{"channels", "join", "general"}, 403, schema.ErrorCodeChannelNotJoinable, "you cannot join this channel"},
		{"leave, last member", []string{"channels", "leave", "general"}, 409, schema.ErrorCodeChannelLastMember, "you are the last member of this private channel"},
		{"settings conflict", []string{"admin", "channel", "settings", "general", "--private"}, 409, schema.ErrorCodeChannelSettingsConflict, "a default channel must be public"},
		{"archive, a default channel", []string{"admin", "channel", "settings", "general", "--archive"}, 409, schema.ErrorCodeChannelArchiveDefault, "a default channel cannot be archived"},
		{"voice for an agent", []string{"admin", "principal", "voice", "9", "on"}, 409, schema.ErrorCodeNotAPerson, "agents do not use voice"},
		{"landing channel beyond the maker", []string{"admin", "invite", "create", "--landing-channel", "general"}, 403, schema.ErrorCodeInviteGrantExceedsInviter, "that link would give more than you have"},
		{"retired invite", []string{"admin", "invite", "create"}, 409, schema.ErrorCodeInviteKindRetired, "per-channel invites are retired"},
		{"join, guests", []string{"admin", "join", "set", "--open-joining", "guest"}, 409, schema.ErrorCodeJoinPolicyConflict, "guests are turned off for now"},
		{"join, voice off", []string{"admin", "join", "set", "--voice", "off"}, 409, schema.ErrorCodeJoinPolicyConflict, "voice for joiners follows each member's own switch"},
		{"join, private open channel", []string{"admin", "join", "set", "--open-channel", "general"}, 409, schema.ErrorCodeChannelSettingsConflict, "a default channel must be public"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			srv := refusalServer(tt.status, tt.code, "")
			defer srv.Close()
			var stdout, stderr bytes.Buffer
			err := RunWithStdin(context.Background(), append(append([]string(nil), tt.args...), "--server", srv.URL), strings.NewReader(""), &stdout, &stderr, "vtest")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if !IsServerCode(err, tt.code) {
				t.Errorf("error = %v, want %s underneath", err, tt.code)
			}
		})
	}
}

// The one collision line is the same whatever the cause, so it cannot tell a
// name in use from one in a private channel the caller cannot see.
func TestChannelExistsIsOneLine(t *testing.T) {
	var lines []string
	for _, msg := range []string{"a channel with that name exists", "a name that reads the same exists", "private " + hostile} {
		isolateConfig(t)
		t.Setenv("CONCH_TOKEN", "tok")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(schema.Error{Code: "channel_exists", Message: msg})
		}))
		var stdout, stderr bytes.Buffer
		err := RunWithStdin(context.Background(), []string{"channels", "create", "general", "--server", srv.URL}, strings.NewReader(""), &stdout, &stderr, "vtest")
		srv.Close()
		if err == nil {
			t.Fatal("collision succeeded")
		}
		lines = append(lines, err.Error()+"|"+stdout.String()+"|"+stderr.String())
	}
	if lines[0] != lines[1] || lines[1] != lines[2] {
		t.Errorf("collisions print different lines: %q", lines)
	}
}

// A server that asks for a recent proof gets one, from a person; the request
// is then sent again and acts.
func TestAdminPrincipalVoiceAsksToReauthenticate(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	var puts, reauths atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "PUT /v1/principals/9/voice-permission":
			if puts.Add(1) == 1 {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(schema.Error{Code: schema.ErrorCodeReauthRequired, Message: "m"})
				return
			}
			_ = json.NewEncoder(w).Encode(schema.VoicePermissionResponseV1{PrincipalID: 9, VoiceAllowed: false, Changed: true})
		case "GET /v1/whoami":
			_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: "root", Role: schema.RoleOperator})
		case "POST /v1/reauth":
			reauths.Add(1)
			_ = json.NewEncoder(w).Encode(schema.ReauthResponseV1{Until: schema.NewTimestamp(time.Now())})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	var stdout, stderr bytes.Buffer
	err := RunWithStdin(context.Background(), []string{"admin", "principal", "voice", "9", "off", "--server", srv.URL}, strings.NewReader("a passphrase long enough to pass\n"), &stdout, &stderr, "vtest")
	if err != nil {
		t.Fatalf("run: %v (stderr %q)", err, stderr.String())
	}
	if stdout.String() != "voice is now off for 9\n" || puts.Load() != 2 || reauths.Load() != 1 {
		t.Errorf("stdout = %q, puts = %d, reauths = %d", stdout.String(), puts.Load(), reauths.Load())
	}
}

// admin join, under Option B.
func TestAdminJoinOptionB(t *testing.T) {
	e := newAdminEnv(t)
	status := func(t *testing.T) string {
		t.Helper()
		stdout, _, err := e.admin(t, e.operatorToken, "", "join", "status")
		if err != nil {
			t.Fatal(err)
		}
		return stdout
	}
	before := status(t)
	for _, want := range []string{"open sign-up: off\n", "member link expiry: 168h\n", "default channels: -\n", "warning: no channel is a default channel"} {
		if !strings.Contains(before, want) {
			t.Errorf("status = %q, want it to contain %q", before, want)
		}
	}
	e.makeDefaultChannel(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "join", "set", "--open-signup", "on", "--link-expiry", "48h"); err != nil {
		t.Fatal(err)
	}
	after := status(t)
	for _, want := range []string{"open sign-up: on\n", "member link expiry: 48h\n", "default channels: ops\n"} {
		if !strings.Contains(after, want) {
			t.Errorf("status = %q, want it to contain %q", after, want)
		}
	}
	if strings.Contains(after, "warning:") {
		t.Errorf("status = %q, want no warning once a default channel exists", after)
	}
	if _, _, err := e.admin(t, e.operatorToken, "", "join", "set", "--link-expiry", "0"); err != nil {
		t.Fatal(err)
	}
	if got := status(t); !strings.Contains(got, "member link expiry: none\n") {
		t.Errorf("status = %q, want no default expiry", got)
	}
	// Guest joining and a joiners-voice switch off are the server's 409, in the
	// client's words.
	for _, args := range [][]string{{"join", "set", "--open-joining", "guest"}, {"join", "set", "--open-joining", "both"}, {"join", "set", "--voice", "off"}} {
		_, _, err := e.admin(t, e.operatorToken, "", args...)
		if !IsServerCode(err, schema.ErrorCodeJoinPolicyConflict) || !strings.Contains(err.Error(), "guests are turned off for now") {
			t.Errorf("%v = %v, want the join_policy_conflict line", args, err)
		}
	}
	// --open-channel still works, and says it is deprecated on standard error.
	stdout, stderr, err := e.admin(t, e.operatorToken, "", "join", "set", "--open-channel", "ops")
	if err != nil || !strings.Contains(stderr, "deprecated") || !strings.Contains(stderr, "conch admin channel settings") || strings.Contains(stdout, "deprecated") {
		t.Errorf("--open-channel: stdout = %q, stderr = %q, err = %v", stdout, stderr, err)
	}
	// Turning open sign-up on with no default channel is refused by the server.
	if _, _, err := e.admin(t, e.operatorToken, "", "join", "set", "--no-open-channels", "--open-signup", "on"); !IsServerCode(err, schema.ErrorCodeJoinPolicyConflict) {
		t.Errorf("open sign-up with no default channel = %v, want join_policy_conflict", err)
	}
}

// Under Option B the open channels are the default channels, and are printed
// once, as such; and the usage line of join set offers only what the server
// takes.
func TestJoinStatusNamesDefaultChannelsOnce(t *testing.T) {
	e := newAdminEnv(t)
	e.makeDefaultChannel(t)
	out, _, err := e.admin(t, e.operatorToken, "", "join", "status")
	if err != nil || strings.Contains(out, "open channels:") || strings.Count(out, "default channels: ops\n") != 1 {
		t.Errorf("status = %q, %v", out, err)
	}
}

func TestJoinSetUsageMatchesHelp(t *testing.T) {
	var join adminCommand
	for _, c := range adminCommands {
		if c.group == "join" && c.verb == "set" {
			join = c
		}
	}
	for _, bad := range []string{"guest|account|both", "--guests on|off", "--voice on|off"} {
		if strings.Contains(join.usage(), bad) {
			t.Errorf("the usage line offers %q, which the server refuses", bad)
		}
	}
	var buf bytes.Buffer
	Usage(&buf)
	if strings.Contains(buf.String(), "--voice on|off") || strings.Contains(buf.String(), "--open-joining off|guest") {
		t.Errorf("the usage text offers what the server refuses")
	}
}

func TestLinkExpiry(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		given   bool
		wantErr bool
	}{
		{"", 0, false, false},
		{"0", 0, true, false},
		{"48h", 48 * 3600, true, false},
		{"7d", 7 * 86400, true, false},
		{"365d", 365 * 86400, true, false},
		{"366d", 0, false, true},
		{"8761h", 0, false, true},
		{"-1h", 0, false, true},
		{"0d", 0, false, true},
		{"1500ms", 0, false, true},
		{"soon", 0, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := linkExpiry(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.given != (got != nil) || (got != nil && *got != tt.want) {
				t.Errorf("= %v, want %d (given %v)", got, tt.want, tt.given)
			}
		})
	}
}

// A hostile server's pages cannot keep the directory reading for ever or send
// it backwards.
func TestDirectoryMembersPagesMustAdvance(t *testing.T) {
	tests := []struct {
		name  string
		after func(call int) int64
		want  string
	}{
		{"the cursor does not move", func(int) int64 { return 5 }, "do not move forward"},
		{"the cursor goes back", func(call int) int64 { return int64(10 - call) }, "do not move forward"},
		{"the cursor never ends", func(call int) int64 { return int64(call + 1) }, "do not end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				call := int(calls.Add(1))
				after := tt.after(call)
				_ = json.NewEncoder(w).Encode(schema.ServerMembersResponseV1{
					Members: []schema.ServerMemberV1{{ID: after, Kind: schema.PrincipalHuman, Name: "n" + strconv.Itoa(call)}}, After: after,
				})
			}))
			defer srv.Close()
			var stdout, stderr bytes.Buffer
			err := RunWithStdin(context.Background(), []string{"admin", "directory", "members", "--server", srv.URL}, strings.NewReader(""), &stdout, &stderr, "vtest")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want nothing printed from a broken listing", stdout.String())
			}
		})
	}
}
