package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// toolServer answers every tools/call with the given result body and records
// the last request's arguments.
func toolServer(t *testing.T, result string, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		*got = req.Params.Arguments
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":`+result+`}`)
	}))
}

func TestPostMessageToSendsAudienceAsGiven(t *testing.T) {
	ok := `{"structuredContent":{"message":{"schema":"conch.message.v2","id":1,"channel_id":1,"author_id":2,"body":"hi"}}}`
	whisper := &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{3, 1}}
	tests := []struct {
		name     string
		audience *schema.Audience
		want     map[string]any
	}{
		{"channel-wide has no audience argument", nil, map[string]any{"channel": "ops", "body": "hi"}},
		{"net", &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}, map[string]any{"channel": "ops", "body": "hi", "audience": map[string]any{"kind": "net", "net_id": float64(4)}}},
		{"whisper keeps its list as given", whisper, map[string]any{"channel": "ops", "body": "hi", "audience": map[string]any{"kind": "principals", "principal_ids": []any{float64(3), float64(1)}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got map[string]any
			srv := toolServer(t, ok, &got)
			defer srv.Close()
			if _, err := New(srv.URL, "t").PostMessageTo(context.Background(), "ops", "hi", tt.audience); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("arguments = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToolErrorCarriesTheCode(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		wantCode string
		wantText string
	}{
		{"schema error", `{"code":"net_not_found","message":"no such net"}`, "net_not_found", `mcp tools/call tool error: {"code":"net_not_found","message":"no such net"}`},
		{"plain text has no code", "boom", "", "mcp tools/call tool error: boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got map[string]any
			text, _ := json.Marshal(tt.text)
			srv := toolServer(t, `{"isError":true,"content":[{"type":"text","text":`+string(text)+`}]}`, &got)
			defer srv.Close()
			_, err := New(srv.URL, "t").PostMessageTo(context.Background(), "ops", "hi", nil)
			var toolErr *ToolError
			if !errors.As(err, &toolErr) {
				t.Fatalf("error = %v, want a *ToolError", err)
			}
			if toolErr.Code != tt.wantCode {
				t.Errorf("code = %q, want %q", toolErr.Code, tt.wantCode)
			}
			// The message text is unchanged from before ToolError existed.
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want %q", err.Error(), tt.wantText)
			}
		})
	}
}

// Errors from this client end up in its callers' logs. A tool result can hold
// message bodies, including ones addressed to a net or whispered, so an error
// about a result that could not be read must not quote it.
func TestErrorsDoNotQuoteToolResults(t *testing.T) {
	const secret = "SECRET-whispered-body"
	tests := []struct {
		name    string
		status  int
		body    string
		wantHas string
	}{
		{"truncated JSON holding a message", http.StatusOK,
			`{"jsonrpc":"2.0","id":1,"result":{"structuredContent":{"messages":[{"id":1,"body":"` + secret, "decode mcp tools/call response"},
		{"not JSON at all", http.StatusOK, secret + " <html>", "decode mcp tools/call response"},
		{"a long error page", http.StatusBadGateway, "upstream said no\n" + strings.Repeat("x", 500) + secret, "status 502: upstream said no"},
		{"a one-line refusal", http.StatusUnauthorized, "Unauthorized\n", "status 401: Unauthorized"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			_, err := New(srv.URL, "token-not-real").ReadChannel(context.Background(), "ops", 0, 10)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("the error quotes the response: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantHas) {
				t.Errorf("err = %v, want it to contain %q", err, tt.wantHas)
			}
			if len(err.Error()) > 300 {
				t.Errorf("the error is %d bytes long", len(err.Error()))
			}
		})
	}
}
