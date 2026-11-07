package cli

import (
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

func sharingPerson(id int64, startedMinute int, quality *schema.VoiceShareQuality, sound bool) schema.VoiceParticipantV2 {
	p := cameraPerson(id, false, false)
	started := schema.NewTimestamp(time.Date(2026, time.October, 9, 8, startedMinute, 0, 0, time.UTC))
	p.Sharing, p.SharingAudio, p.ShareStartedAt, p.ShareQuality = true, sound, &started, quality
	return p
}

func TestVoicePresenceShowsShares(t *testing.T) {
	const joined = "2026-10-09T08:30:00Z"
	hd := &schema.VoiceShareQuality{Resolution: schema.VoiceShareResolution1080p, FrameRate: 60}
	sd := &schema.VoiceShareQuality{Resolution: schema.VoiceShareResolution720p, FrameRate: 30}
	tests := []struct {
		name  string
		rooms []schema.VoicePresenceRoomV2
		want  string
	}{
		{
			name:  "no sharer: lines are as before",
			rooms: []schema.VoicePresenceRoomV2{{Participants: []schema.VoiceParticipantV2{cameraPerson(3, true, false)}}},
			want:  "3 - talking " + joined + "\n",
		},
		{
			name:  "one sharer, nothing declared",
			rooms: []schema.VoicePresenceRoomV2{{Participants: []schema.VoiceParticipantV2{sharingPerson(3, 35, nil, false)}}},
			want:  "3 - quiet+share " + joined + " share_since=2026-10-09T08:35:00Z\n",
		},
		{
			name:  "one sharer with quality",
			rooms: []schema.VoicePresenceRoomV2{{Participants: []schema.VoiceParticipantV2{sharingPerson(3, 35, hd, false)}}},
			want:  "3 - quiet+share " + joined + " share_since=2026-10-09T08:35:00Z quality=1080p@60\n",
		},
		{
			name: "several sharers, one with sound, in id order",
			rooms: []schema.VoicePresenceRoomV2{{Participants: []schema.VoiceParticipantV2{
				sharingPerson(9, 31, sd, true), sharingPerson(3, 35, hd, false), cameraPerson(5, false, false),
			}}},
			want: "3 - quiet+share " + joined + " share_since=2026-10-09T08:35:00Z quality=1080p@60\n" +
				"5 - quiet " + joined + "\n" +
				"9 - quiet+share " + joined + " share_since=2026-10-09T08:31:00Z quality=720p@30 sound\n",
		},
		{
			name: "share and camera together",
			rooms: []schema.VoicePresenceRoomV2{{Participants: func() []schema.VoiceParticipantV2 {
				p := sharingPerson(3, 35, nil, true)
				p.Camera = true
				p.Transmitting = true
				return []schema.VoiceParticipantV2{p}
			}()}},
			want: "3 - talking+camera+share " + joined + " share_since=2026-10-09T08:35:00Z sound\n",
		},
		{
			name: "a hostile server's share in a scoped room is not shown",
			rooms: []schema.VoicePresenceRoomV2{
				{Participants: []schema.VoiceParticipantV2{cameraPerson(3, false, false)}},
				{Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}, Participants: []schema.VoiceParticipantV2{sharingPerson(7, 35, hd, true)}},
			},
			want: "3 - quiet " + joined + "\n7 - quiet " + joined + " net:4\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := schema.VoicePresenceV2{Schema: schema.VoicePresenceSchemaV2, ChannelID: 1, Configured: true, Available: true, Rooms: tt.rooms}
			var out strings.Builder
			if err := writeVoicePresence(&out, "general", doc, nil); err != nil {
				t.Fatal(err)
			}
			if out.String() != tt.want {
				t.Errorf("output =\n%s\nwant\n%s", out.String(), tt.want)
			}
		})
	}
}

func TestVoiceStatusAsksForV2(t *testing.T) {
	isolateConfig(t)
	var snapshotQuery, wsQuery string
	hd := &schema.VoiceShareQuality{Resolution: schema.VoiceShareResolution1080p, FrameRate: 60}
	doc := schema.VoicePresenceV2{Schema: schema.VoicePresenceSchemaV2, ChannelID: 1, Configured: true, Available: true,
		Rooms: []schema.VoicePresenceRoomV2{{Participants: []schema.VoiceParticipantV2{sharingPerson(3, 35, hd, true), sharingPerson(4, 36, nil, false)}}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/channels/general/voice":
			snapshotQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(doc)
		case "/v1/voice/ws":
			wsQuery = r.URL.RawQuery
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			_ = wsjson.Write(r.Context(), conn, doc)
			_ = conn.Close(websocket.StatusNormalClosure, "")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	want := "3 - quiet+share 2026-10-09T08:30:00Z share_since=2026-10-09T08:35:00Z quality=1080p@60 sound\n" +
		"4 - quiet+share 2026-10-09T08:30:00Z share_since=2026-10-09T08:36:00Z\n"
	stdout, _, err := runCLI(t, "", "voice", "status", "--server", server.URL, "general")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != want {
		t.Errorf("stdout =\n%s\nwant\n%s", stdout, want)
	}
	if !strings.Contains(snapshotQuery, "schema="+schema.VoicePresenceSchemaV2) {
		t.Errorf("snapshot query = %q, want schema=%s", snapshotQuery, schema.VoicePresenceSchemaV2)
	}
	voiceNow = func() time.Time { return time.Date(2026, time.October, 9, 9, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { voiceNow = time.Now })
	stdout, _, err = runCLI(t, "", "voice", "status", "--watch", "--server", server.URL, "general")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "--- 2026-10-09T09:00:00Z\n"+want {
		t.Errorf("watch stdout =\n%s", stdout)
	}
	if !strings.Contains(wsQuery, "schema="+schema.VoicePresenceSchemaV2) || !strings.Contains(wsQuery, "channel=general") {
		t.Errorf("socket query = %q, want channel and schema", wsQuery)
	}
}

// A v1 document where v2 was asked for is a server that did not understand:
// it is refused, never read as v2.
func TestVoiceStatusRefusesV1Document(t *testing.T) {
	isolateConfig(t)
	fake := newVoiceFake(t)
	fake.snapshot = schema.VoicePresenceV1{Schema: schema.VoicePresenceSchemaV1, ChannelID: 1, Configured: true, Available: true}
	stdout, _, err := runCLI(t, "", "voice", "status", "--server", fake.URL, "general")
	if err == nil || stdout != "" {
		t.Fatalf("err = %v, stdout = %q; want a refusal", err, stdout)
	}
}

func TestVoiceShareStop(t *testing.T) {
	const req = "DELETE /v1/channels/general/voice/share?principal=7"
	apiError := func(code string, perm schema.Permission) schema.Error {
		return schema.Error{Code: code, Message: "hostile words\n\x1b[31mpwned\u202e", Permission: perm}
	}
	tests := []struct {
		name    string
		args    []string // "@" is replaced by --server <url>
		status  int
		body    any
		raw     string
		wantReq string
		wantErr string // substring; empty means exit 0 and nothing printed
	}{
		{name: "moderator stops a share", args: []string{"@", "general", "7"}, status: http.StatusNoContent, wantReq: req},
		{name: "flag after the positionals", args: []string{"general", "7", "@"}, status: http.StatusNoContent, wantReq: req},
		{name: "a plain member is refused", args: []string{"@", "general", "7"}, status: http.StatusForbidden,
			body: apiError("forbidden", ""), wantReq: req, wantErr: "only a manager of the channel, or an operator, may stop someone else's share"},
		{name: "operator without manage_channels", args: []string{"@", "general", "7"}, status: http.StatusForbidden,
			body: apiError(schema.ErrorCodePermissionDenied, schema.PermManageChannels), wantReq: req, wantErr: "only a manager of the channel, or an operator"},
		{name: "not a member, or no such channel", args: []string{"@", "general", "7"}, status: http.StatusNotFound,
			body: apiError("channel_not_found", ""), wantReq: req, wantErr: "no such channel, or you cannot see it"},
		{name: "404 with a code this client does not know", args: []string{"@", "general", "7"}, status: http.StatusNotFound,
			body: apiError("evil\x1b[31m", ""), wantReq: req, wantErr: "HTTP 404"},
		{name: "voice not configured", args: []string{"@", "general", "7"}, status: http.StatusServiceUnavailable,
			body: apiError(schema.ErrorCodeVoiceNotConfigured, ""), wantReq: req, wantErr: "voice is not configured on this server"},
		{name: "voice unavailable", args: []string{"@", "general", "7"}, status: http.StatusServiceUnavailable,
			body: apiError(schema.ErrorCodeVoiceUnavailable, ""), wantReq: req, wantErr: "voice is unavailable"},
		{name: "bad request", args: []string{"@", "general", "7"}, status: http.StatusBadRequest,
			body: apiError("invalid_request", ""), wantReq: req, wantErr: "the server did not accept the request"},
		{name: "wrong shape of refusal", args: []string{"@", "general", "7"}, status: http.StatusForbidden, raw: `["not","an","error"]`,
			wantReq: req, wantErr: "server returned"},
		{name: "html where a refusal should be", args: []string{"@", "general", "7"}, status: http.StatusNotFound, raw: "<html>\x1b[31mnope</html>",
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
			args := []string{"voice", "share", "stop"}
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

func TestVoiceShareStopRefusedConnection(t *testing.T) {
	isolateConfig(t)
	stdout, _, err := runCLI(t, "", "voice", "share", "stop", "--server", "http://127.0.0.1:1", "general", "7")
	if err == nil || stdout != "" {
		t.Fatalf("err = %v, stdout = %q; want an error and nothing printed", err, stdout)
	}
	if strings.Contains(ErrorLine(err), "\n") {
		t.Errorf("error spans lines: %q", ErrorLine(err))
	}
}

func TestVoiceShareSubcommands(t *testing.T) {
	tests := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"voice", "share"}, "voice share requires a subcommand (stop)"},
		{[]string{"voice", "share", "start", "general"}, `unknown voice share subcommand "start"`},
		{[]string{"voice"}, "share stop"},
	}
	for _, tt := range tests {
		_, _, err := runCLI(t, "", tt.args...)
		if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
			t.Errorf("%v: err = %v, want %q", tt.args, err, tt.wantErr)
		}
	}
}
