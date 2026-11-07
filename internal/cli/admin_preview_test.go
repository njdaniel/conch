package cli

import (
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

type previewRequest struct {
	method    string
	path      string
	query     string
	previewAs string
}

type previewRecorder struct {
	mu       sync.Mutex
	requests []previewRequest
}

func (r *previewRecorder) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, previewRequest{
		method: req.Method, path: req.URL.Path, query: req.URL.RawQuery,
		previewAs: req.Header.Get("Conch-Preview-As"),
	})
}

func (r *previewRecorder) got() []previewRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

func previewTestServer(t *testing.T, handler func(http.ResponseWriter, *http.Request)) (*httptest.Server, *previewRecorder) {
	t.Helper()
	recorder := &previewRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder.record(r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, recorder
}

func runPreviewTest(t *testing.T, srv *httptest.Server, args ...string) (string, string, error) {
	t.Helper()
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", fakeToken)
	return runCLI(t, "", append(args, "--server", srv.URL)...)
}

func previewFixture() schema.PreviewResponseV1 {
	return schema.PreviewResponseV1{
		ID: 7, Kind: schema.PrincipalHuman, Name: "alice", Role: schema.RoleMember,
		Permissions: []schema.Permission{schema.PermUseVoice, schema.PermUseCamera}, PermissionsRevision: 3,
		MayInvite: true, VoiceAllowed: false,
	}
}

func channelPreviewFixture() schema.ListChannelsResponse {
	return schema.ListChannelsResponse{Channels: []schema.ChannelV0{{
		ID: 2, Name: "ops", Manages: true, Visibility: schema.ChannelPrivate, Default: true,
	}}}
}

func directoryPreviewFixture() schema.ServerMembersResponseV1 {
	return schema.ServerMembersResponseV1{
		Scope: schema.DirectoryScopeSharedChannels,
		Members: []schema.ServerMemberV1{{
			ID: 7, Kind: schema.PrincipalHuman, Name: "alice", Role: schema.RoleMember,
			CreatedAt: schema.NewTimestamp(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)),
		}},
	}
}

func writePreviewJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestAdminPreview(t *testing.T) {
	for _, target := range []string{"alice", "7"} {
		t.Run("target "+target, func(t *testing.T) {
			srv, recorder := previewTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/directory/members":
					if r.Header.Get("Conch-Preview-As") == "" {
						writePreviewJSON(t, w, schema.ServerMembersResponseV1{Members: []schema.ServerMemberV1{{ID: 7, Kind: schema.PrincipalHuman, Name: "alice", Role: schema.RoleMember}}})
						return
					}
					writePreviewJSON(t, w, directoryPreviewFixture())
				case "/v1/preview":
					writePreviewJSON(t, w, previewFixture())
				case "/v1/channels":
					writePreviewJSON(t, w, channelPreviewFixture())
				default:
					http.NotFound(w, r)
				}
			})
			stdout, stderr, err := runPreviewTest(t, srv, "admin", "preview", target)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			want := "member: alice (id 7)\n" +
				"role: member\n" +
				"owner: false\n" +
				"permissions: use_voice, use_camera\n" +
				"channels:\n" +
				"2 private manages default ops\n" +
				"directory: shared_channels\n" +
				"members:\n" +
				"7 human member - created 2026-11-07T12:00:00.000Z voice - may-invite - invited-by - alice\n"
			if stdout != want || stderr != "" {
				t.Errorf("stdout = %q, stderr = %q; want %q", stdout, stderr, want)
			}
			wantRequests := []previewRequest{
				{method: http.MethodGet, path: "/v1/preview", previewAs: "7"},
				{method: http.MethodGet, path: "/v1/channels", previewAs: "7"},
				{method: http.MethodGet, path: "/v1/directory/members", query: "limit=200", previewAs: "7"},
			}
			if target == "alice" {
				wantRequests = append([]previewRequest{{method: http.MethodGet, path: "/v1/directory/members", query: "limit=200"}}, wantRequests...)
			}
			if got := recorder.got(); !slices.Equal(got, wantRequests) {
				t.Errorf("requests = %+v, want %+v", got, wantRequests)
			}
		})
	}
}

func TestAdminPreviewJSON(t *testing.T) {
	srv, recorder := previewTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/preview":
			writePreviewJSON(t, w, previewFixture())
		case "/v1/channels":
			writePreviewJSON(t, w, channelPreviewFixture())
		case "/v1/directory/members":
			writePreviewJSON(t, w, directoryPreviewFixture())
		default:
			http.NotFound(w, r)
		}
	})
	stdout, _, err := runPreviewTest(t, srv, "admin", "preview", "7", "--json")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("stdout = %q, want three JSON documents", stdout)
	}
	var preview schema.PreviewResponseV1
	var channels schema.ListChannelsResponse
	var directory schema.ServerMembersResponseV1
	for i, into := range []any{&preview, &channels, &directory} {
		if err := json.Unmarshal([]byte(lines[i]), into); err != nil {
			t.Fatalf("document %d: %v", i, err)
		}
	}
	if preview.ID != 7 || len(channels.Channels) != 1 || directory.Scope != schema.DirectoryScopeSharedChannels {
		t.Errorf("documents = %+v, %+v, %+v", preview, channels, directory)
	}
	for _, request := range recorder.got() {
		if request.previewAs != "7" {
			t.Errorf("request %+v has no preview header", request)
		}
	}
}

func TestPreviewAsReadCommands(t *testing.T) {
	channelDirectory := schema.ChannelDirectoryResponseV1{Channels: []schema.ChannelListingV1{{
		ID: 2, Name: "den", Visibility: schema.ChannelPrivate, Default: true, Joined: true,
	}}}
	tests := []struct {
		name         string
		args         []string
		path         string
		response     any
		wantStdout   string
		wantRequests []previewRequest
	}{
		{
			name: "channels by name", args: []string{"channels", "list", "--preview-as", "alice"},
			path: "/v1/directory/channels", response: channelDirectory, wantStdout: "2 private default joined - - den\n",
			wantRequests: []previewRequest{
				{method: http.MethodGet, path: "/v1/directory/members", query: "limit=200"},
				{method: http.MethodGet, path: "/v1/directory/channels", previewAs: "7"},
			},
		},
		{
			name: "channels all by id", args: []string{"channels", "list", "--all", "--preview-as", "7"},
			path: "/v1/directory/channels", response: channelDirectory, wantStdout: "2 private default joined - - den\n",
			wantRequests: []previewRequest{{method: http.MethodGet, path: "/v1/directory/channels", query: "scope=all", previewAs: "7"}},
		},
		{
			name: "channels JSON", args: []string{"channels", "list", "--preview-as", "7", "--json"},
			path: "/v1/directory/channels", response: channelDirectory, wantStdout: "{\"channels\":[{\"id\":2,\"name\":\"den\",\"created_at\":\"0001-01-01T00:00:00.000Z\",\"visibility\":\"private\",\"default\":true,\"joined\":true}]}\n",
			wantRequests: []previewRequest{{method: http.MethodGet, path: "/v1/directory/channels", previewAs: "7"}},
		},
		{
			name: "directory by name", args: []string{"admin", "directory", "members", "--preview-as", "alice"},
			path: "/v1/directory/members", response: directoryPreviewFixture(),
			wantStdout: "7 human member - created 2026-11-07T12:00:00.000Z voice - may-invite - invited-by - alice\n",
			wantRequests: []previewRequest{
				{method: http.MethodGet, path: "/v1/directory/members", query: "limit=200"},
				{method: http.MethodGet, path: "/v1/directory/members", query: "limit=200", previewAs: "7"},
			},
		},
		{
			name: "directory JSON", args: []string{"admin", "directory", "members", "--preview-as", "7", "--json"},
			path: "/v1/directory/members", response: directoryPreviewFixture(),
			wantStdout:   "{\"members\":[{\"id\":7,\"kind\":\"human\",\"name\":\"alice\",\"role\":\"member\",\"created_at\":\"2026-11-07T12:00:00.000Z\"}],\"scope\":\"shared_channels\"}\n",
			wantRequests: []previewRequest{{method: http.MethodGet, path: "/v1/directory/members", query: "limit=200", previewAs: "7"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, recorder := previewTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/directory/members" && r.Header.Get("Conch-Preview-As") == "" {
					writePreviewJSON(t, w, schema.ServerMembersResponseV1{Members: []schema.ServerMemberV1{{ID: 7, Kind: schema.PrincipalHuman, Name: "alice", Role: schema.RoleMember}}})
					return
				}
				if r.URL.Path != tt.path {
					http.NotFound(w, r)
					return
				}
				writePreviewJSON(t, w, tt.response)
			})
			stdout, stderr, err := runPreviewTest(t, srv, tt.args...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if stdout != tt.wantStdout || stderr != "" {
				t.Errorf("stdout = %q, stderr = %q; want %q", stdout, stderr, tt.wantStdout)
			}
			if got := recorder.got(); !slices.Equal(got, tt.wantRequests) {
				t.Errorf("requests = %+v, want %+v", got, tt.wantRequests)
			}
		})
	}
}

func TestPreviewRefusalsIncludeServerFields(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"preview", []string{"admin", "preview", "7"}},
		{"channels", []string{"channels", "list", "--preview-as", "7"}},
		{"directory", []string{"admin", "directory", "members", "--preview-as", "7"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := previewTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusForbidden)
				writePreviewJSON(t, w, schema.Error{Code: schema.ErrorCodePermissionDenied, Message: "preview refused", Permission: schema.PermManageChannels})
			})
			stdout, _, err := runPreviewTest(t, srv, tt.args...)
			if err == nil {
				t.Fatal("refusal succeeded")
			}
			for _, want := range []string{schema.ErrorCodePermissionDenied, "preview refused", string(schema.PermManageChannels)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want %q", err, want)
				}
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want none", stdout)
			}
		})
	}
}

func TestPreviewAsRefusedLocallyElsewhere(t *testing.T) {
	for _, command := range adminCommands {
		t.Run("admin "+command.group+" "+command.verb, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			if command.bind != nil {
				command.bind(fs, &adminOpts{})
			}
			got := fs.Lookup("preview-as") != nil
			want := command.group == "directory" && command.verb == "members"
			if got != want {
				t.Errorf("accepts --preview-as = %t, want %t", got, want)
			}
		})
	}

	tests := []struct {
		name string
		args []string
	}{
		{"channel write", []string{"channels", "create", "--preview-as", "7", "ops"}},
		{"channel join", []string{"channels", "join", "--preview-as", "7", "ops"}},
		{"admin write", []string{"admin", "channel", "create", "--preview-as", "7", "ops"}},
		{"admin read", []string{"admin", "permissions", "show", "--preview-as", "7"}},
		{"whoami", []string{"whoami", "--preview-as", "7"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, recorder := previewTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				writePreviewJSON(t, w, schema.Error{Code: schema.ErrorCodePreviewReadOnly, Message: "this route cannot be read in preview"})
			})
			_, _, err := runPreviewTest(t, srv, tt.args...)
			if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -preview-as") {
				t.Errorf("error = %v, want a local unknown-flag refusal", err)
			}
			if got := recorder.got(); len(got) != 0 {
				t.Errorf("requests = %+v, want none", got)
			}
		})
	}
}

func TestAdminPreviewMemberResolutionFailure(t *testing.T) {
	srv, recorder := previewTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writePreviewJSON(t, w, schema.ServerMembersResponseV1{Members: []schema.ServerMemberV1{}})
	})
	stdout, _, err := runPreviewTest(t, srv, "admin", "preview", "nobody")
	if err == nil || !strings.Contains(err.Error(), `no member "nobody"`) {
		t.Errorf("error = %v, want no member", err)
	}
	if stdout != "" || len(recorder.got()) != 1 {
		t.Errorf("stdout = %q, requests = %+v; want no output and only resolution", stdout, recorder.got())
	}
}

func TestAdminPreviewHostileResponses(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "malformed JSON", body: `{"id":`, want: "decode preview response"},
		{name: "malformed document", body: `{"id":7,"kind":"robot","name":"alice","role":"member","owner":false,"permissions":[],"permissions_revision":3,"may_invite":true,"voice_allowed":false}`, want: "invalid preview"},
		{name: "wrong member", body: `{"id":8,"kind":"human","name":"bob","role":"member","owner":false,"permissions":[],"permissions_revision":3,"may_invite":true,"voice_allowed":false}`, want: "invalid preview"},
		{name: "role simulation", body: `{"id":0,"kind":"human","name":"","role":"member","owner":false,"permissions":[],"permissions_revision":3,"may_invite":true,"voice_allowed":false,"simulation":"role"}`, want: "invalid preview"},
		{name: "oversized document", body: `{"id":7,"kind":"human","name":"` + strings.Repeat("x", 128<<10) + `","role":"member","owner":false,"permissions":[],"permissions_revision":3,"may_invite":true,"voice_allowed":false}`, want: "larger than 65536 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, recorder := previewTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.body)
			})
			stdout, _, err := runPreviewTest(t, srv, "admin", "preview", "7")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to contain %q", err, tt.want)
			}
			if stdout != "" || len(recorder.got()) != 1 {
				t.Errorf("stdout = %q, requests = %+v; want no output and one request", stdout, recorder.got())
			}
		})
	}
}

func TestAdminPreviewRealServer(t *testing.T) {
	e := newAdminEnv(t)
	stdout, stderr, err := e.admin(t, e.operatorToken, "", "preview", "mallory")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"member: mallory (id 2)\n", "role: member\n", "owner: false\n", "channels:\n", "directory: everyone\n", "members:\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout = %q, want it to contain %q", stdout, want)
		}
	}
	if stderr != "" {
		t.Errorf("stderr = %q", stderr)
	}
	stdout, _, err = e.cli(t, e.operatorToken, "channels", "list", "--preview-as", "mallory")
	if err != nil || stdout != "" {
		t.Errorf("previewed channels = %q, %v; want the member's empty list", stdout, err)
	}
	stdout, _, err = e.admin(t, e.operatorToken, "", "directory", "members", "--preview-as", "2")
	if err != nil || !strings.Contains(stdout, "2 human member - created ") || !strings.Contains(stdout, " voice - may-invite - invited-by - mallory") {
		t.Errorf("previewed directory = %q, %v; want the member's fields", stdout, err)
	}
}
