package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestMCPAcceptsProxiedHost (issue #1479): a deployed conchd listens on
// loopback behind a reverse proxy that keeps the public Host. The MCP
// library's own DNS-rebinding check would refuse every such request with 403
// "invalid Host header"; /mcp is authenticated by a bearer credential in
// every mode, which is what defeats rebinding, so the check is off. The
// request goes over a real loopback listener (httptest.NewServer), because
// the library looks at the connection's local address, which a recorder
// does not have.
func TestMCPAcceptsProxiedHost(t *testing.T) {
	for _, mode := range []AuthMode{AuthRequired, AuthOff} {
		f := newMCPAuthFixture(t, Config{AuthMode: mode})
		_, token := f.issue(t, f.agent.ID, nil)
		httpSrv := httptest.NewServer(f.srv.Handler())
		t.Cleanup(httpSrv.Close)
		tests := []struct {
			name          string
			host          string
			authorization string
			want          int
		}{
			{"proxied host with a credential", "conch.example.org", "Bearer " + token, http.StatusOK},
			{"proxied host with no credential", "conch.example.org", "", http.StatusUnauthorized},
			{"proxied host with a bad credential", "conch.example.org", "Bearer conch_nope", http.StatusUnauthorized},
			{"loopback host with a credential", "", "Bearer " + token, http.StatusOK},
			{"loopback host with no credential", "", "", http.StatusUnauthorized},
		}
		for _, tt := range tests {
			t.Run(string(mode)+"/"+tt.name, func(t *testing.T) {
				body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
					"name": "post_message", "arguments": map[string]any{"channel": "general", "body": "through the proxy"},
				}})
				if err != nil {
					t.Fatal(err)
				}
				req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/mcp", bytes.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				if tt.host != "" {
					req.Host = tt.host
				}
				if tt.authorization != "" {
					req.Header.Set("Authorization", tt.authorization)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				got, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != tt.want {
					t.Fatalf("status = %d (%s), want %d", resp.StatusCode, strings.TrimSpace(string(got)), tt.want)
				}
				switch tt.want {
				case http.StatusOK:
					if strings.Contains(string(got), `"isError":true`) || !strings.Contains(string(got), `"result"`) {
						t.Errorf("the tool call did not succeed: %s", got)
					}
				case http.StatusUnauthorized:
					// conchd's own refusal, not the library's.
					if resp.Header.Get("WWW-Authenticate") != `Bearer realm="conch-mcp"` {
						t.Errorf("WWW-Authenticate = %q, want conchd's bearer challenge", resp.Header.Get("WWW-Authenticate"))
					}
				}
			})
		}
	}
}
