package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

func cameraPerson(id int64, talking, camera bool) schema.VoiceParticipant {
	return schema.VoiceParticipant{
		PrincipalID: id, CanPublish: true, Transmitting: talking, Camera: camera,
		JoinedAt: schema.NewTimestamp(time.Date(2026, time.October, 9, 8, 30, 0, 0, time.UTC)),
	}
}

func TestVoicePresenceShowsCamera(t *testing.T) {
	const joined = "2026-10-09T08:30:00Z"
	tests := []struct {
		name  string
		rooms []schema.VoicePresenceRoom
		want  string
	}{
		{
			name:  "no camera: lines are as before",
			rooms: []schema.VoicePresenceRoom{{Participants: []schema.VoiceParticipant{cameraPerson(3, true, false), cameraPerson(9, false, false)}}},
			want:  "3 - talking " + joined + "\n9 - quiet " + joined + "\n",
		},
		{
			name:  "one camera",
			rooms: []schema.VoicePresenceRoom{{Participants: []schema.VoiceParticipant{cameraPerson(3, false, false), cameraPerson(9, false, true)}}},
			want:  "3 - quiet " + joined + "\n9 - quiet+camera " + joined + "\n",
		},
		{
			name:  "several cameras",
			rooms: []schema.VoicePresenceRoom{{Participants: []schema.VoiceParticipant{cameraPerson(9, false, true), cameraPerson(3, false, true), cameraPerson(5, false, false)}}},
			want:  "3 - quiet+camera " + joined + "\n5 - quiet " + joined + "\n9 - quiet+camera " + joined + "\n",
		},
		{
			name:  "a camera with talking",
			rooms: []schema.VoicePresenceRoom{{Participants: []schema.VoiceParticipant{cameraPerson(3, true, true)}}},
			want:  "3 - talking+camera " + joined + "\n",
		},
		{
			name: "a hostile server's camera in a scoped room is not shown and the scope stays",
			rooms: []schema.VoicePresenceRoom{
				{Participants: []schema.VoiceParticipant{cameraPerson(3, false, true)}},
				{Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}, Participants: []schema.VoiceParticipant{cameraPerson(7, true, true)}},
			},
			want: "3 - quiet+camera " + joined + "\n7 - talking " + joined + " net:4\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := schema.VoicePresenceV1{Schema: schema.VoicePresenceSchemaV1, ChannelID: 1, Configured: true, Available: true, Rooms: tt.rooms}
			var out strings.Builder
			if err := writeVoicePresence(&out, "general", doc, nil); err != nil {
				t.Fatal(err)
			}
			if out.String() != tt.want {
				t.Errorf("output =\n%s\nwant\n%s", out.String(), tt.want)
			}
			for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
				if n := len(strings.Fields(line)); n != 4 && n != 5 {
					t.Errorf("line %q has %d fields", line, n)
				}
			}
		})
	}
}

func TestVoiceStatusWatchShowsCamera(t *testing.T) {
	isolateConfig(t)
	fake := newVoiceFake(t)
	fake.socket = func(ctx context.Context, conn *websocket.Conn) {
		doc := func(ps ...schema.VoiceParticipant) schema.VoicePresenceV1 {
			return schema.VoicePresenceV1{Schema: schema.VoicePresenceSchemaV1, ChannelID: 1, Configured: true, Available: true,
				Rooms: []schema.VoicePresenceRoom{{Participants: ps}}}
		}
		_ = wsjson.Write(ctx, conn, doc(cameraPerson(3, false, false)))
		_ = wsjson.Write(ctx, conn, doc(cameraPerson(3, false, true)))
		_ = wsjson.Write(ctx, conn, doc(cameraPerson(3, true, true)))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}
	voiceNow = func() time.Time { return time.Date(2026, time.October, 9, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { voiceNow = time.Now })
	stdout, _, err := runCLI(t, "", "voice", "status", "--watch", "--server", fake.URL, "general")
	if err != nil {
		t.Fatal(err)
	}
	const stamp = "--- 2026-10-09T09:00:00Z\n"
	const joined = "2026-10-09T08:30:00Z"
	want := stamp + "3 - quiet " + joined + "\n" + stamp + "3 - quiet+camera " + joined + "\n" + stamp + "3 - talking+camera " + joined + "\n"
	if stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", stdout, want)
	}
}

// A camera_ended reason this client does not know is refused by the schema
// check; the command prints one safe line and the reason never reaches the
// terminal raw.
func TestVoiceStatusUnknownCameraEndedReason(t *testing.T) {
	isolateConfig(t)
	fake := newVoiceFake(t)
	at := schema.NewTimestamp(time.Date(2026, time.October, 9, 8, 30, 0, 0, time.UTC))
	fake.snapshot = schema.VoicePresenceV1{Schema: schema.VoicePresenceSchemaV1, ChannelID: 1, Configured: true, Available: true,
		Rooms:       []schema.VoicePresenceRoom{{Participants: []schema.VoiceParticipant{cameraPerson(3, false, true)}}},
		CameraEnded: &schema.VoiceCameraEnded{Reason: "x\n9 root \x1b[31m\u202e", StartedAt: at, EndedAt: at}}
	stdout, _, err := runCLI(t, "", "voice", "status", "--server", fake.URL, "general")
	if err == nil {
		t.Fatalf("an unknown reason was accepted; stdout=%q", stdout)
	}
	if line := ErrorLine(err); strings.ContainsAny(line, "\n\x1b\u202e") || stdout != "" {
		t.Errorf("unsafe output: stdout=%q error=%q", stdout, line)
	}
}

func TestVoiceStatusHugeParticipantList(t *testing.T) {
	isolateConfig(t)
	fake := newVoiceFake(t)
	var people []schema.VoiceParticipant
	for i := int64(1); i <= 5000; i++ {
		people = append(people, cameraPerson(i, i%2 == 0, i%3 == 0))
	}
	fake.snapshot = schema.VoicePresenceV1{Schema: schema.VoicePresenceSchemaV1, ChannelID: 1, Configured: true, Available: true,
		Rooms: []schema.VoicePresenceRoom{{Participants: people}}}
	stdout, _, err := runCLI(t, "", "voice", "status", "--server", fake.URL, "general")
	if err != nil {
		// The document is well under the client's body cap: it must be read.
		t.Fatalf("5000 people: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != len(people) {
		t.Fatalf("%d lines for %d people", len(lines), len(people))
	}
	for _, line := range lines {
		if n := len(strings.Fields(line)); n != 4 {
			t.Fatalf("line %q has %d fields", line, n)
		}
	}
}

func TestVoiceCameraStop(t *testing.T) {
	const req = "DELETE /v1/channels/general/voice/camera?principal=7"
	apiError := func(code string, perm schema.Permission) schema.Error {
		return schema.Error{Code: code, Message: "hostile words\n\x1b[31mpwned\u202e", Permission: perm}
	}
	tests := []struct {
		name    string
		args    []string // "@" is replaced by --server <url>
		status  int
		body    any
		raw     string
		wantReq string // method, path and raw query; empty means no request
		wantErr string // substring; empty means exit 0 and nothing printed
	}{
		{name: "owner stops a camera", args: []string{"@", "general", "7"}, status: http.StatusNoContent, wantReq: req},
		{name: "flag after the positionals", args: []string{"general", "7", "@"}, status: http.StatusNoContent, wantReq: req},
		{name: "operator without manage_channels", args: []string{"@", "general", "7"}, status: http.StatusForbidden,
			body: apiError(schema.ErrorCodePermissionDenied, schema.PermManageChannels), wantReq: req, wantErr: "only an owner, or an operator holding manage_channels"},
		{name: "permission this client does not know", args: []string{"@", "general", "7"}, status: http.StatusForbidden,
			body: apiError(schema.ErrorCodePermissionDenied, "evil\x1b[31m"), wantReq: req, wantErr: "only an owner, or an operator holding manage_channels"},
		{name: "unknown channel", args: []string{"@", "general", "7"}, status: http.StatusNotFound,
			body: apiError("channel_not_found", ""), wantReq: req, wantErr: "no such channel, or you cannot see it"},
		{name: "bad request", args: []string{"@", "general", "7"}, status: http.StatusBadRequest,
			body: apiError("invalid_request", ""), wantReq: req, wantErr: "the server did not accept the request"},
		{name: "voice not configured", args: []string{"@", "general", "7"}, status: http.StatusServiceUnavailable,
			body: apiError(schema.ErrorCodeVoiceNotConfigured, ""), wantReq: req, wantErr: "voice is not configured on this server"},
		{name: "unknown code", args: []string{"@", "general", "7"}, status: http.StatusConflict,
			body: apiError("code\x1b[31m", ""), wantReq: req, wantErr: "the server refused this (HTTP 409)"},
		{name: "wrong shape of refusal", args: []string{"@", "general", "7"}, status: http.StatusBadGateway, raw: `["not","an","error"]`,
			wantReq: req, wantErr: "server returned"},
		{name: "oversize refusal body", args: []string{"@", "general", "7"}, status: http.StatusInternalServerError, raw: strings.Repeat("x", 4<<20),
			wantReq: req, wantErr: "server returned"},
		{name: "person is required", args: []string{"@", "general"}, wantErr: "expected <channel> <person-id>"},
		{name: "no arguments", args: []string{"@"}, wantErr: "expected <channel> <person-id>"},
		{name: "too many", args: []string{"@", "general", "7", "8"}, wantErr: "expected <channel> <person-id>"},
		{name: "name is not an id", args: []string{"@", "general", "nick"}, wantErr: "must be a principal id"},
		{name: "zero is not an id", args: []string{"@", "general", "0"}, wantErr: "must be a principal id"},
		{name: "signed id", args: []string{"@", "general", "+7"}, wantErr: "must be a principal id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var got []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if len(body) != 0 {
					t.Errorf("request body %q, want none", body)
				}
				got = append(got, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
				w.WriteHeader(tt.status)
				switch {
				case tt.raw != "":
					_, _ = w.Write([]byte(tt.raw))
				case tt.body != nil:
					_ = json.NewEncoder(w).Encode(tt.body)
				}
			}))
			defer server.Close()
			args := []string{"voice", "camera", "stop"}
			for _, a := range tt.args {
				if a == "@" {
					args = append(args, "--server", server.URL)
					continue
				}
				args = append(args, a)
			}
			stdout, _, err := runCLI(t, "", args...)
			switch {
			case tt.wantReq == "" && len(got) != 0:
				t.Errorf("sent %v, want no request", got)
			case tt.wantReq != "" && (len(got) != 1 || got[0] != tt.wantReq):
				t.Errorf("requests = %v, want [%s]", got, tt.wantReq)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want nothing", stdout)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want success", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("succeeded, want %q", tt.wantErr)
			}
			line := ErrorLine(err)
			if !strings.Contains(line, tt.wantErr) {
				t.Errorf("error %q lacks %q", line, tt.wantErr)
			}
			if strings.ContainsAny(line, "\n\x1b\u202e") || !utf8.ValidString(line) || strings.Contains(line, "pwned") || strings.Contains(line, "hostile words") {
				t.Errorf("unsafe or server-worded error %q", line)
			}
		})
	}
}

func TestVoiceCameraStopRefusedConnection(t *testing.T) {
	isolateConfig(t)
	stdout, _, err := runCLI(t, "", "voice", "camera", "stop", "--server", "http://127.0.0.1:1", "general", "7")
	if err == nil || stdout != "" {
		t.Fatalf("err = %v, stdout = %q; want an error and nothing printed", err, stdout)
	}
	if strings.Contains(ErrorLine(err), "\n") {
		t.Errorf("error spans lines: %q", ErrorLine(err))
	}
}

func TestVoiceCameraSubcommands(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"voice", "camera"}, "voice camera requires a subcommand (stop)"},
		{[]string{"voice", "camera", "start", "general"}, `unknown voice camera subcommand "start"`},
		{[]string{"voice"}, "camera stop"},
	}
	for _, tt := range tests {
		_, _, err := runCLI(t, "", tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%v: err = %v, want %q", tt.args, err, tt.wantErr)
		}
	}
}
