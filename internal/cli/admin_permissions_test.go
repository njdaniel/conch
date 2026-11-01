package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin permissions` and permissions in `conch whoami` (ADR-014, issue
// #1022), against a real conchd handler. The owner is "root" (id 1), the
// member "mallory" (id 2); the tests add operator-1 (id 3).

// permsEnv is an adminEnv with a second operator, who is not an owner.
type permsEnv struct {
	*adminEnv
	operatorTwoToken string // operator-1, id 3
}

func newPermsEnv(t *testing.T) *permsEnv {
	t.Helper()
	e := &permsEnv{adminEnv: newAdminEnv(t)}
	for _, args := range [][]string{
		{"principal", "create", "--kind", "human", "operator-1"},
		{"principal", "role", "3", "operator"},
	} {
		if _, _, err := e.admin(t, e.operatorToken, "", args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	out, _, err := e.admin(t, e.operatorToken, "", "credential", "issue", "--label", "test", "3")
	if err != nil {
		t.Fatal(err)
	}
	e.operatorTwoToken = strings.TrimSpace(out)
	return e
}

func (e *permsEnv) token(who string) string {
	switch who {
	case "owner":
		return e.operatorToken
	case "operator-1":
		return e.operatorTwoToken
	default:
		return e.memberToken
	}
}

// showOwner is `permissions show` as the owner on a fresh server.
const showFresh = `revision: 1
PERMISSION               MEMBERS  OPERATORS  FIXED
create_public_channels   on       on         -
create_private_channels  on       on         -
make_member_links        on       on         -
see_member_directory     on       on         operators
use_voice                on       on         -
share_screen             on       on         -
manage_members           off      on         members
act_as_members           off      on         members
manage_channels          off      on         members
change_server_settings   off      off        members
read_audit_log           off      off        members
act_as_members: can sign in as, and post as, any member who isn't an owner
read_audit_log: sees every member and every private channel's name in the audit trail
preset: group
you may change: member, operator
`

// TestAdminPermissionsCommands walks the three commands through one
// server's life. Each step names who runs it; the steps depend on the ones
// before.
func TestAdminPermissionsCommands(t *testing.T) {
	e := newPermsEnv(t)
	steps := []struct {
		name  string
		who   string
		stdin string
		args  []string
		// wantOut is the whole of stdout when set; wantOutHas are fragments.
		wantOut    string
		wantOutHas []string
		wantOutNot []string
		wantErr    string // a fragment of the error; empty means success
	}{
		{name: "show as owner", who: "owner", args: []string{"permissions", "show"}, wantOut: showFresh},
		{name: "show as an operator without change_server_settings", who: "operator-1", args: []string{"permissions", "show"},
			wantOutHas: []string{"revision: 1\n", "you may change: nothing\n", "preset: group\n"}},
		{name: "a member is refused", who: "member", args: []string{"permissions", "show"}, wantErr: "forbidden"},

		// What the client refuses before sending anything.
		{name: "unknown permission", who: "owner", args: []string{"permissions", "set", "--members", "on", "teleport"}, wantErr: "teleport is not a permission this conch knows"},
		{name: "no cell given", who: "owner", args: []string{"permissions", "set", "use_voice"}, wantErr: "give --members or --operators"},
		{name: "bad value", who: "owner", args: []string{"permissions", "set", "--members", "maybe", "use_voice"}, wantErr: "--members must be on or off"},
		{name: "a fixed cell", who: "owner", args: []string{"permissions", "set", "--members", "on", "manage_members"}, wantErr: "operator-only"},
		{name: "unknown preset", who: "owner", args: []string{"permissions", "preset", "--yes", "bogus"}, wantErr: "bogus is not a preset this conch knows"},

		// An operator without change_server_settings is refused, naming it.
		{name: "operator without the permission", who: "operator-1", args: []string{"permissions", "set", "--members", "off", "make_member_links"},
			wantErr: "permission_denied"},
		{name: "operator without the permission names it", who: "operator-1", args: []string{"permissions", "set", "--members", "off", "make_member_links"},
			wantErr: "(permission: change_server_settings)"},
		{name: "preset without the permission", who: "operator-1", args: []string{"permissions", "preset", "--yes", "personal"},
			wantErr: "(permission: change_server_settings)"},

		// The owner grants it to operators; the operator then changes members.
		{name: "owner grants the permission to operators", who: "owner", args: []string{"permissions", "set", "--operators", "on", "change_server_settings"},
			wantOut: "change_server_settings: members off, operators on (revision 2)\n"},
		{name: "operator changes a member cell", who: "operator-1", args: []string{"permissions", "set", "--members", "off", "make_member_links"},
			wantOut: "make_member_links: members off, operators on (revision 3)\n"},
		{name: "operator may change members only", who: "operator-1", args: []string{"permissions", "show"},
			wantOutHas: []string{"you may change: member\n", "preset: none (the member column matches no preset)\n"}},
		{name: "an operator-column change by an operator", who: "operator-1", args: []string{"permissions", "set", "--operators", "off", "manage_channels"},
			wantErr: "owner required"},
		{name: "operators below members is a conflict", who: "owner", args: []string{"permissions", "set", "--operators", "off", "create_public_channels"},
			wantErr: "permissions_conflict"},
		{name: "both cells", who: "owner", args: []string{"permissions", "set", "--members", "off", "--operators", "off", "share_screen"},
			wantOut: "share_screen: members off, operators off (revision 4)\n"},
		{name: "both cells back", who: "owner", args: []string{"permissions", "set", "--operators", "on", "--members", "on", "share_screen"},
			wantOut: "share_screen: members on, operators on (revision 5)\n"},

		// Presets.
		{name: "preset declined", who: "owner", stdin: "n\n", args: []string{"permissions", "preset", "personal"},
			wantOutHas: []string{"the personal preset changes the member column:\n", "create_public_channels: members on -> off\n", "see_member_directory: members on -> off\n"},
			wantOutNot: []string{"applied"}, wantErr: "not applied"},
		{name: "preset with nothing to answer is declined", who: "owner", args: []string{"permissions", "preset", "personal"},
			wantErr: "not applied"},
		{name: "preset confirmed", who: "owner", stdin: "y\n", args: []string{"permissions", "preset", "personal"},
			wantOutHas: []string{"applied the personal preset (revision 6)\n"}},
		{name: "show after the preset", who: "owner", args: []string{"permissions", "show"},
			wantOutHas: []string{"revision: 6\n", "preset: personal\n", "use_voice                on       on", "create_public_channels   off      on"}},
		{name: "preset again changes nothing", who: "owner", args: []string{"permissions", "preset", "personal"},
			wantOut: "the member column already matches the personal preset; nothing to change\n"},
		{name: "preset with --yes", who: "owner", args: []string{"permissions", "preset", "--yes", "group"},
			wantOutHas: []string{"applied the group preset (revision 7)\n"}},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			stdout, _, err := e.admin(t, e.token(st.who), st.stdin, st.args...)
			if st.wantErr == "" {
				if err != nil {
					t.Fatalf("run: %v", err)
				}
			} else if err == nil || !strings.Contains(ErrorLine(err), st.wantErr) {
				t.Fatalf("error = %v, want it to contain %q", err, st.wantErr)
			}
			if st.wantOut != "" && stdout != st.wantOut {
				t.Errorf("stdout = %q\nwant     %q", stdout, st.wantOut)
			}
			for _, frag := range st.wantOutHas {
				if !strings.Contains(stdout, frag) {
					t.Errorf("stdout = %q, want it to contain %q", stdout, frag)
				}
			}
			for _, frag := range st.wantOutNot {
				if strings.Contains(stdout, frag) {
					t.Errorf("stdout = %q, must not contain %q", stdout, frag)
				}
			}
		})
	}

	t.Run("show --json is the server's document", func(t *testing.T) {
		stdout, _, err := e.admin(t, e.operatorToken, "", "permissions", "show", "--json")
		if err != nil {
			t.Fatal(err)
		}
		var doc schema.PermissionsResponseV1
		if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
			t.Fatalf("decode %q: %v", stdout, err)
		}
		if doc.Revision != 7 || doc.Preset != schema.PresetGroup || len(doc.Rows) != len(schema.Permissions) {
			t.Errorf("document = revision %d preset %q rows %d", doc.Revision, doc.Preset, len(doc.Rows))
		}
	})
}

// TestAdminPermissionsDeclinedChangesNothing: a declined preset, and one the
// client refused, leave the revision where it was.
func TestAdminPermissionsDeclinedChangesNothing(t *testing.T) {
	e := newPermsEnv(t)
	_, _, _ = e.admin(t, e.operatorToken, "n\n", "permissions", "preset", "personal")
	_, _, _ = e.admin(t, e.operatorToken, "", "permissions", "set", "--members", "on", "teleport")
	out, _, err := e.admin(t, e.operatorToken, "", "permissions", "show")
	if err != nil || !strings.HasPrefix(out, "revision: 1\n") {
		t.Fatalf("show = %q, %v; want revision 1", out, err)
	}
}

// TestAdminPermissionsIssuedToOthers: turning act_as_members off for
// operators prints what operators issued to other people, with the command
// that revokes each; the printed command works.
func TestAdminPermissionsIssuedToOthers(t *testing.T) {
	e := newPermsEnv(t)
	out, _, err := e.admin(t, e.operatorTwoToken, "", "credential", "issue", "--label", "for-member-1", "2")
	if err != nil || !tokenPattern.MatchString(strings.TrimSpace(out)) {
		t.Fatalf("operator-1 issues a credential for the member: %q, %v", out, err)
	}
	issued := strings.TrimSpace(out)

	stdout, _, err := e.admin(t, e.operatorToken, "", "permissions", "set", "--operators", "off", "act_as_members")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"act_as_members: members off, operators off (revision 2)\n",
		"operators issued these to other people; turning act_as_members off does not end them. Revoke each you do not want:\n",
		"  credential 4 (principal 2, issued by 3): conch admin credential revoke 4\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	if got := e.whoamiStatus(t, issued); got != http.StatusOK {
		t.Fatalf("the issued credential before the revoke = %d, want it still live", got)
	}
	if _, _, err := e.admin(t, e.operatorToken, "", "credential", "revoke", "4"); err != nil {
		t.Fatalf("the printed revoke command: %v", err)
	}
	if got := e.whoamiStatus(t, issued); got != http.StatusUnauthorized {
		t.Errorf("the issued credential after the revoke = %d, want 401", got)
	}
	// --json keeps the document alone.
	stdout, _, err = e.admin(t, e.operatorToken, "", "permissions", "set", "--json", "--operators", "on", "act_as_members")
	if err != nil || !json.Valid([]byte(stdout)) {
		t.Errorf("set --json = %q, %v", stdout, err)
	}
}

// A stale revision is reported, and what was asked is not applied: another
// owner's change lands between the read and the write.
func TestAdminPermissionsStale(t *testing.T) {
	e := newPermsEnv(t)
	target, err := url.Parse(e.url)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var once sync.Once
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Path == "/v1/permissions" {
			once.Do(func() {
				c, err := NewClient(e.url, nil)
				if err != nil {
					t.Error(err)
					return
				}
				c = c.WithToken(e.operatorToken)
				doc, err := c.GetPermissions(r.Context())
				if err != nil {
					t.Error(err)
					return
				}
				_, err = c.PutPermissions(r.Context(), schema.PutPermissionsRequestV1{Revision: doc.Revision,
					Changes: []schema.PermissionChangeV1{{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false}}})
				if err != nil {
					t.Error(err)
				}
			})
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()

	t.Setenv("CONCH_TOKEN", e.operatorToken)
	stdout, _, err := runCLI(t, "", "admin", "permissions", "set", "--members", "off", "--server", front.URL, "make_member_links")
	if err == nil || !strings.Contains(ErrorLine(err), "the permissions changed since they were read (revision 1, now 2); nothing was changed") {
		t.Fatalf("set = %q, %v; want the stale report", stdout, err)
	}
	out, _, err := e.admin(t, e.operatorToken, "", "permissions", "show")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "revision: 2\n") || !strings.Contains(out, "make_member_links        on       on") {
		t.Errorf("show = %q; the stale request must not have been applied", out)
	}
}

// reauthServer is a server whose PUT wants a proof first: it answers
// reauth_required until /v1/reauth has been given the password.
func reauthServer(t *testing.T) (*httptest.Server, func() (puts, proofs int)) {
	t.Helper()
	var mu sync.Mutex
	var puts, proofs int
	proved := false
	ts := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/whoami":
			_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: "owner", Role: schema.RoleOperator, Owner: true})
		case "GET /v1/permissions":
			_ = json.NewEncoder(w).Encode(permissionsFixture(1))
		case "POST /v1/reauth":
			proofs++
			proved = true
			_ = json.NewEncoder(w).Encode(schema.ReauthResponseV1{Until: schema.NewTimestamp(ts.Add(time.Hour))})
		case "PUT /v1/permissions":
			puts++
			if !proved {
				writeAPIError(w, http.StatusForbidden, schema.ErrorCodeReauthRequired)
				return
			}
			_ = json.NewEncoder(w).Encode(permissionsFixture(2))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return puts, proofs
	}
}

// permissionsFixture is a document of two rows at a revision.
func permissionsFixture(revision int64) schema.PermissionsResponseV1 {
	return schema.PermissionsResponseV1{
		Revision: revision,
		Rows: []schema.PermissionRowV1{
			{Permission: schema.PermMakeMemberLinks, Member: revision == 1, Operator: true},
			{Permission: schema.PermUseVoice, Member: true, Operator: true},
		},
		Presets:  []schema.PermissionPresetV1{{Preset: schema.PresetPersonal, Member: []schema.Permission{schema.PermUseVoice}}},
		Editable: []schema.PermissionRole{schema.PermissionRoleMember, schema.PermissionRoleOperator},
	}
}

// The PUT wants a recent proof: the password is asked for, the request is
// sent again, and with no password to give the refusal is the answer.
func TestAdminPermissionsReauth(t *testing.T) {
	tests := []struct {
		name       string
		stdin      string
		wantErr    string
		wantOut    string
		wantStderr string
		wantPuts   int
		wantProofs int
	}{
		{name: "proved", stdin: walkPassword + "\n", wantOut: "make_member_links: members off, operators on (revision 2)\n",
			wantStderr: "This needs a recent proof that it is you: your password.", wantPuts: 2, wantProofs: 1},
		{name: "no password to give", stdin: "", wantErr: "no password given", wantPuts: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			srv, counts := reauthServer(t)
			t.Setenv("CONCH_TOKEN", "tok")
			stdout, stderr, err := runCLI(t, tt.stdin, "admin", "permissions", "set", "--members", "off", "--server", srv.URL, "make_member_links")
			if tt.wantErr == "" && err != nil {
				t.Fatalf("run: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(ErrorLine(err), tt.wantErr)) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if stdout != tt.wantOut {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantOut)
			}
			if !strings.Contains(stderr, tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", stderr, tt.wantStderr)
			}
			if puts, proofs := counts(); puts != tt.wantPuts || proofs != tt.wantProofs {
				t.Errorf("puts %d, proofs %d; want %d and %d", puts, proofs, tt.wantPuts, tt.wantProofs)
			}
		})
	}
}

// Every refusal that names a permission prints it, whatever its code, and a
// permission this client has never heard of is printed by name.
func TestRefusalsNameTheirPermission(t *testing.T) {
	tests := []struct {
		code, permission string
		wantSuffix       string
	}{
		{schema.ErrorCodePermissionDenied, "change_server_settings", "(permission: change_server_settings)"},
		{"forbidden", "manage_members", "(permission: manage_members)"},
		{"voice_not_allowed", "use_voice", "(permission: use_voice)"},
		{"invite_not_allowed", "make_member_links", "(permission: make_member_links)"},
		{schema.ErrorCodePermissionDenied, "a_later_permission", "(permission: a_later_permission)"},
		{"forbidden", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.code+"/"+tt.permission, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(schema.Error{Code: tt.code, Message: "refused", Permission: schema.Permission(tt.permission)})
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.GetPermissions(context.Background())
			want := "cli: server error " + tt.code + ": refused"
			if tt.wantSuffix != "" {
				want += " " + tt.wantSuffix
			}
			if err == nil || err.Error() != want {
				t.Errorf("error = %v, want %q", err, want)
			}
		})
	}
}

// whoami prints what the caller holds.
func TestWhoAmIPermissions(t *testing.T) {
	e := newPermsEnv(t)
	if _, _, err := e.admin(t, e.operatorToken, "", "permissions", "preset", "--yes", "personal"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		who  string
		want string
	}{
		{"owner", "permissions: create_public_channels, create_private_channels, make_member_links, see_member_directory, use_voice, share_screen, manage_members, act_as_members, manage_channels, change_server_settings, read_audit_log\n"},
		{"operator-1", "permissions: create_public_channels, create_private_channels, make_member_links, see_member_directory, use_voice, share_screen, manage_members, act_as_members, manage_channels\n"},
		{"member", "permissions: use_voice, share_screen\n"},
	}
	for _, tt := range tests {
		t.Run(tt.who, func(t *testing.T) {
			t.Setenv("CONCH_TOKEN", e.token(tt.who))
			stdout, _, err := runCLI(t, "", "whoami", "--server", e.url)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stdout, tt.want) {
				t.Errorf("whoami = %q, want it to contain %q", stdout, tt.want)
			}
		})
	}
}

func TestWriteWhoAmIPermissions(t *testing.T) {
	tests := []struct {
		name string
		who  schema.WhoAmIResponseV1
		want string
	}{
		{"a server that predates permissions", schema.WhoAmIResponseV1{}, ""},
		{"holds none", schema.WhoAmIResponseV1{PermissionsRevision: 3}, "permissions: none\n"},
		{"some", schema.WhoAmIResponseV1{PermissionsRevision: 3, Permissions: []schema.Permission{schema.PermUseVoice, "later"}}, "permissions: use_voice, later\n"},
		{"hostile", schema.WhoAmIResponseV1{PermissionsRevision: 3, Permissions: []schema.Permission{schema.Permission(hostile)}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sb strings.Builder
			if err := writeWhoAmIPermissions(&sb, tt.who); err != nil {
				t.Fatal(err)
			}
			got := sb.String()
			if tt.name == "hostile" {
				if bad := firstUnsafe(got); bad != "" || !strings.Contains(got, `\u202e`) {
					t.Errorf("output %q: unsafe %q or no escape", got, bad)
				}
				return
			}
			if got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}
}

// hostilePermissionsServer answers GET and PUT /v1/permissions with doc, or
// with a body of raw bytes.
func hostilePermissionsServer(t *testing.T, doc schema.PermissionsResponseV1, raw string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/permissions" {
			http.NotFound(w, r)
			return
		}
		if raw != "" {
			_, _ = w.Write([]byte(raw))
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// What a hostile server sends prints safely, and the commands exit nonzero
// where they should.
func TestAdminPermissionsHostileServer(t *testing.T) {
	ts := schema.NewTimestamp(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	hostileDoc := hostilePermissions(ts)
	oversized := `{"revision":1,"rows":[],"presets":[],"editable":[],"warnings":[],"updated_at":"2026-11-01T12:00:00Z","pad":"` + strings.Repeat("x", maxPermissionsBody) + `"}`
	tests := []struct {
		name     string
		doc      schema.PermissionsResponseV1
		raw      string
		args     []string
		wantErr  string
		wantOut  []string // fragments of stdout
		wantNot  []string
		wantSafe bool
	}{
		{name: "show: unknown names print by name", doc: hostileDoc, args: []string{"show"}, wantSafe: true,
			wantOut: []string{`\u202e`, "act_as_members: can sign in as, and post as, any member who isn't an owner\n", "warning: open_signup_on: members cannot make member links"}},
		{name: "show: an oversized document", raw: oversized, args: []string{"show"}, wantErr: "larger than"},
		{name: "show: not a document", raw: `["x"]`, args: []string{"show"}, wantErr: "decode read permissions response"},
		{name: "set: unknown values in the answer", doc: hostileDoc, args: []string{"set", "--members", "off", "use_voice"}, wantSafe: true,
			wantOut: []string{"use_voice: members on, operators on (revision 3)", `\u202e`},
			// The bogus items get no revoke command.
			wantNot: []string{"revoke -1", "revoke 0", "/v1/hooks/0", "credential revoke 7"}},
		{name: "set: issued items with bogus ids and kinds", doc: hostileDoc, args: []string{"set", "--members", "off", "use_voice"}, wantSafe: true,
			wantOut: []string{
				"no valid id to revoke", "does not know how to revoke it", "conch admin credential revoke 8\n"}},
		{name: "preset: the server lacks the preset", doc: func() schema.PermissionsResponseV1 { d := hostileDoc; d.Presets = nil; return d }(),
			args: []string{"preset", "--yes", "personal"}, wantErr: "the server does not define the personal preset"},
		{name: "preset: unknown permission in the diff", doc: hostileDoc, args: []string{"preset", "--yes", "personal"}, wantSafe: true,
			wantOut: []string{`\u202e`, "applied the personal preset (revision 3)"}},
		{name: "set: an oversized answer", raw: oversized, args: []string{"set", "--members", "off", "use_voice"}, wantErr: "larger than"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			srv := hostilePermissionsServer(t, tt.doc, tt.raw)
			args := append([]string{"admin", "permissions"}, tt.args...)
			stdout, stderr, err := runCLI(t, "", append(args, "--server", srv.URL)...)
			all := stdout + stderr
			if err != nil {
				all += ErrorLine(err)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("run: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(ErrorLine(err), tt.wantErr)):
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if tt.wantSafe {
				if bad := firstUnsafe(all); bad != "" {
					t.Errorf("%s in output %q", bad, all)
				}
			}
			for _, frag := range tt.wantOut {
				if !strings.Contains(all, frag) {
					t.Errorf("output = %q, want it to contain %q", all, frag)
				}
			}
			for _, frag := range tt.wantNot {
				if strings.Contains(all, frag) {
					t.Errorf("output = %q, must not contain %q", all, frag)
				}
			}
		})
	}
}
