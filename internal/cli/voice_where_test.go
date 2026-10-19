package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func occupancyDoc(configured, available bool, rows ...schema.VoiceChannelOccupancy) schema.VoiceOccupancyV1 {
	if rows == nil {
		rows = []schema.VoiceChannelOccupancy{}
	}
	return schema.VoiceOccupancyV1{Schema: schema.VoiceOccupancySchemaV1, Configured: configured, Available: available, Channels: rows}
}

func TestVoiceWhere(t *testing.T) {
	apiError := func(code string) schema.Error { return schema.Error{Code: code, Message: "refused by test"} }
	channels := []schema.ChannelV0{{ID: 5, Name: "x\n9 \x1b[31m"}, {ID: 7, Name: "game-night"}, {ID: 9, Name: "ops"}, {ID: 11, Name: "Two Words"}}
	tests := []struct {
		name         string
		status       int
		body         any
		channelsDown bool
		wantOut      string
		wantErr      string
		wantChannels bool // whether the channel list is read
	}{
		{
			name:    "people in one channel",
			body:    occupancyDoc(true, true, schema.VoiceChannelOccupancy{ChannelID: 7, InVoice: 3}, schema.VoiceChannelOccupancy{ChannelID: 9}),
			wantOut: "game-night 3\n", wantChannels: true,
		},
		{
			name: "several channels, zeros left out, odd name quoted",
			body: occupancyDoc(true, true,
				schema.VoiceChannelOccupancy{ChannelID: 7, InVoice: 1},
				schema.VoiceChannelOccupancy{ChannelID: 9},
				schema.VoiceChannelOccupancy{ChannelID: 11, InVoice: 2}),
			wantOut: "game-night 1\n\"Two Words\" 2\n", wantChannels: true,
		},
		{
			name: "channel list unavailable shows ids", channelsDown: true,
			body:    occupancyDoc(true, true, schema.VoiceChannelOccupancy{ChannelID: 7, InVoice: 3}),
			wantOut: "#7 3\n", wantChannels: true,
		},
		{
			name:    "hostile channel name stays on one line",
			body:    occupancyDoc(true, true, schema.VoiceChannelOccupancy{ChannelID: 5, InVoice: 1}),
			wantOut: "\"x\\n9 \\x1b[31m\" 1\n", wantChannels: true,
		},
		{name: "nobody in voice", body: occupancyDoc(true, true, schema.VoiceChannelOccupancy{ChannelID: 7}), wantOut: "nobody is in voice in any of your channels\n"},
		{name: "no channels at all", body: occupancyDoc(true, true), wantOut: "nobody is in voice in any of your channels\n"},
		{name: "not configured", body: occupancyDoc(false, false), wantErr: "voice is not configured on this server"},
		{name: "unavailable", body: occupancyDoc(true, false), wantErr: "voice is unavailable"},
		{name: "person without voice", status: http.StatusForbidden, body: apiError(schema.ErrorCodeVoiceNotAllowed), wantErr: schema.ErrorCodeVoiceNotAllowed},
		{name: "agent credential", status: http.StatusForbidden, body: apiError("forbidden"), wantErr: "forbidden"},
		{name: "auth off", status: http.StatusBadRequest, body: apiError(schema.ErrorCodeVoiceRequiresAuth), wantErr: schema.ErrorCodeVoiceRequiresAuth},
		{name: "not logged in", status: http.StatusUnauthorized, body: apiError("unauthenticated"), wantErr: "not logged in"},
		{
			name: "server text cannot break the error line", status: http.StatusForbidden,
			body: schema.Error{Code: "forbidden", Message: "no\n\x1b[2Jfake line"}, wantErr: `no\n\x1b[2Jfake line`,
		},
		{name: "invalid document is not printed", body: schema.VoiceOccupancyV1{Schema: "conch.voice_occupancy.v9", Channels: []schema.VoiceChannelOccupancy{}}, wantErr: "invalid voice occupancy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				switch r.Method + " " + r.URL.Path {
				case "GET /v1/voice/occupancy":
					if tt.status != 0 {
						w.WriteHeader(tt.status)
					}
					_ = json.NewEncoder(w).Encode(tt.body)
				case "GET /v1/channels":
					if tt.channelsDown {
						w.WriteHeader(http.StatusInternalServerError)
						return
					}
					_ = json.NewEncoder(w).Encode(schema.ListChannelsResponse{Channels: channels})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			stdout, stderr, err := runCLI(t, "", "voice", "where", "--server", srv.URL)
			if stdout != tt.wantOut {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantOut)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want none", stderr)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want exit 0", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				if strings.ContainsAny(err.Error(), "\n\x1b") {
					t.Errorf("error is not one clean line: %q", err.Error())
				}
			}
			read := false
			for _, line := range requests {
				read = read || line == "GET /v1/channels"
			}
			if read != tt.wantChannels {
				t.Errorf("channel list read = %v, want %v (requests %v)", read, tt.wantChannels, requests)
			}
		})
	}
}

// --json prints the document the server sent, decodable into the schema type,
// whatever state it reports, and does not look up channel names.
func TestVoiceWhereJSON(t *testing.T) {
	tests := []struct {
		name string
		doc  schema.VoiceOccupancyV1
	}{
		{"people in voice", occupancyDoc(true, true, schema.VoiceChannelOccupancy{ChannelID: 7, InVoice: 3}, schema.VoiceChannelOccupancy{ChannelID: 9})},
		{"nobody", occupancyDoc(true, true)},
		{"unavailable", occupancyDoc(true, false)},
		{"not configured", occupancyDoc(false, false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.URL.Path)
				_ = json.NewEncoder(w).Encode(tt.doc)
			}))
			t.Cleanup(srv.Close)
			stdout, stderr, err := runCLI(t, "", "voice", "where", "--json", "--server", srv.URL)
			if err != nil {
				t.Fatalf("err = %v, want exit 0", err)
			}
			if stderr != "" {
				t.Errorf("stderr = %q", stderr)
			}
			dec := json.NewDecoder(strings.NewReader(stdout))
			dec.DisallowUnknownFields()
			var got schema.VoiceOccupancyV1
			if err := dec.Decode(&got); err != nil {
				t.Fatalf("stdout is not a voice occupancy document: %v", err)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("document invalid: %v", err)
			}
			want, err := json.Marshal(tt.doc)
			if err != nil {
				t.Fatal(err)
			}
			if stdout != string(want)+"\n" {
				t.Errorf("stdout = %q, want %q", stdout, string(want)+"\n")
			}
			if len(requests) != 1 || requests[0] != "/v1/voice/occupancy" {
				t.Errorf("requests = %v, want only the occupancy read", requests)
			}
		})
	}
}

func TestVoiceWhereUsageErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"extra argument", []string{"voice", "where", "general"}, "takes no arguments"},
		{"unknown flag", []string{"voice", "where", "--loud"}, "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			fake := newVoiceFake(t)
			stdout, _, err := runCLI(t, "", append(tt.args, "--server", fake.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if stdout != "" || len(fake.seen()) != 0 {
				t.Errorf("stdout = %q, requests = %v", stdout, fake.seen())
			}
		})
	}
}

func TestUsageMentionsVoiceWhere(t *testing.T) {
	var out bytes.Buffer
	Usage(&out)
	for _, want := range []string{"conch voice where", "--json", "nobody is in voice"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage lacks %q", want)
		}
	}
}
