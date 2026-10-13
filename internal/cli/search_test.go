package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func searchMessage(id int64, body string, audience *schema.Audience) schema.MessageV2 {
	return schema.MessageV2{Schema: schema.MessageSchemaV2, ID: id, ChannelID: 1, AuthorID: 7, Body: body, Audience: audience,
		CreatedAt: schema.NewTimestamp(time.Date(2026, 10, 13, 12, 0, int(id), 0, time.UTC))}
}

func TestRunSearch(t *testing.T) {
	t.Setenv("CONCH_TOKEN", "tok")
	net := &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3}
	tests := []struct {
		name       string
		args       []string
		response   schema.SearchMessagesResponseV2
		wantQuery  map[string]string
		wantOut    []string // each line must be contained, in order
		wantNoOut  bool
		wantStderr string
		wantErr    string
	}{
		{
			name:      "words are joined into one q, matches print like tail, newest first",
			args:      []string{"ops", "red", "falcon"},
			response:  schema.SearchMessagesResponseV2{Messages: []schema.MessageV2{searchMessage(9, "red falcon", nil), searchMessage(4, "[x] falcon red", nil)}},
			wantQuery: map[string]string{"q": "red falcon", "limit": "", "before": ""},
			wantOut:   []string{"2026-10-13T12:00:09Z 7 red falcon", `2026-10-13T12:00:04Z 7 \[x] falcon red`},
		},
		{
			name:      "scoped messages carry their marker",
			args:      []string{"ops", "hold"},
			response:  schema.SearchMessagesResponseV2{Messages: []schema.MessageV2{searchMessage(9, "hold", net)}},
			wantQuery: map[string]string{"q": "hold"},
			wantOut:   []string{"7 [net:3] hold"},
		},
		{
			name:       "limit and before are sent; a cursor is reported on stderr",
			args:       []string{"--limit", "2", "--before", "50", "ops", "x"},
			response:   schema.SearchMessagesResponseV2{Messages: []schema.MessageV2{searchMessage(9, "x", nil)}, NextBefore: 9},
			wantQuery:  map[string]string{"q": "x", "limit": "2", "before": "50"},
			wantOut:    []string{"7 x"},
			wantStderr: "--before 9",
		},
		{
			name:      "no match prints nothing and succeeds",
			args:      []string{"ops", "zebra"},
			response:  schema.SearchMessagesResponseV2{Messages: []schema.MessageV2{}},
			wantQuery: map[string]string{"q": "zebra"},
			wantNoOut: true,
		},
		{
			name:      "operators in the words are sent as plain text",
			args:      []string{"ops", `"a`, "OR", "-b*"},
			response:  schema.SearchMessagesResponseV2{Messages: []schema.MessageV2{}},
			wantQuery: map[string]string{"q": `"a OR -b*`},
			wantNoOut: true,
		},
		{name: "no words", args: []string{"ops"}, wantErr: "expected <channel> <words>"},
		{name: "blank words", args: []string{"ops", " "}, wantErr: "expected <channel> <words>"},
		{name: "negative limit", args: []string{"--limit", "-1", "ops", "x"}, wantErr: "must not be negative"},
		{name: "unknown flag", args: []string{"--nope", "ops", "x"}, wantErr: "flag provided but not defined"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotQuery map[string][]string
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/channels/ops/messages/search":
					gotPath, gotQuery = r.URL.Path, r.URL.Query()
					_ = json.NewEncoder(w).Encode(tt.response)
				case "/v1/channels/ops/nets", "/v2/channels/ops/nets":
					_ = json.NewEncoder(w).Encode(schema.ListNetsResponseV1{Nets: []schema.NetV1{}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			var out, errOut bytes.Buffer
			err := RunWithStdin(context.Background(), append([]string{"search", "--server", srv.URL}, tt.args...), strings.NewReader(""), &out, &errOut, "test")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error()+errOut.String(), tt.wantErr) {
					t.Fatalf("err = %v (stderr %q), want %q", err, errOut.String(), tt.wantErr)
				}
				if gotPath != "" {
					t.Errorf("a refused command still called the server")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			for k, want := range tt.wantQuery {
				if got := strings.Join(gotQuery[k], ","); got != want {
					t.Errorf("query %s = %q, want %q", k, got, want)
				}
			}
			if tt.wantNoOut && out.Len() != 0 {
				t.Errorf("stdout = %q, want none", out.String())
			}
			rest := out.String()
			for _, line := range tt.wantOut {
				i := strings.Index(rest, line)
				if i < 0 {
					t.Fatalf("stdout %q does not contain %q in order", out.String(), line)
				}
				rest = rest[i+len(line):]
			}
			if n := strings.Count(out.String(), "\n"); n != len(tt.wantOut) {
				t.Errorf("stdout has %d lines, want %d: %q", n, len(tt.wantOut), out.String())
			}
			if tt.wantStderr != "" && !strings.Contains(errOut.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want %q", errOut.String(), tt.wantStderr)
			}
			if tt.wantStderr == "" && errOut.Len() != 0 {
				t.Errorf("stderr = %q, want none", errOut.String())
			}
		})
	}
}

func TestRunSearchServerRefusal(t *testing.T) {
	t.Setenv("CONCH_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(schema.Error{Code: "channel_not_found", Message: "channel not found"})
	}))
	defer srv.Close()
	var out, errOut bytes.Buffer
	err := Run(context.Background(), []string{"search", "--server", srv.URL, "nowhere", "x"}, &out, &errOut, "test")
	if err == nil || !strings.Contains(err.Error(), "channel not found") {
		t.Fatalf("err = %v, want the server's refusal", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q on a refusal", out.String())
	}
}

func TestUsageNamesSearch(t *testing.T) {
	var b bytes.Buffer
	Usage(&b)
	if !strings.Contains(b.String(), "conch search ") {
		t.Error("help text does not mention conch search")
	}
}
