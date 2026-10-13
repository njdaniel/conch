package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const auditFakeLines = `{"schema":"conch.audit_event.v1","id":1,"actor":"system","action":"a","subject":"","detail":"","created_at":"2026-10-12T18:00:00.000Z"}
{"schema":"conch.audit_event.v1","id":2,"actor":"principal:3","action":"b","subject":"s","detail":"d  with  spaces","created_at":"2026-10-12T18:00:01.000Z"}
`

func TestAuditExportCommand(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		status    int
		body      string
		wantQuery string
		wantOut   string
		wantErr   string // substring of the returned error; "" means success
		noRequest bool
	}{
		{name: "lines pass through unchanged", args: nil, status: 200, body: auditFakeLines, wantQuery: "", wantOut: auditFakeLines},
		{name: "after and limit are sent", args: []string{"--after", "7", "--limit", "20"}, status: 200, body: auditFakeLines,
			wantQuery: "after=7&limit=20", wantOut: auditFakeLines},
		{name: "empty export is empty output", args: []string{"--after", "9"}, status: 200, body: "", wantQuery: "after=9", wantOut: ""},
		{name: "refusal", status: 403, body: `{"code":"forbidden","message":"operator role required"}`,
			wantErr: "forbidden"},
		{name: "unauthenticated", status: 401, body: `{"code":"unauthenticated","message":"x"}`, wantErr: "conch login"},
		{name: "server text cannot start another line", status: 400,
			body: `{"code":"invalid_request","message":"bad\nsecond line\u001b[31m"}`, wantErr: "bad\\nsecond line"},
		{name: "negative after", args: []string{"--after", "-1"}, wantErr: "--after", noRequest: true},
		{name: "negative limit", args: []string{"--limit", "-2"}, wantErr: "--limit", noRequest: true},
		{name: "explicit zero limit", args: []string{"--limit", "0"}, wantErr: "--limit", noRequest: true},
		{name: "positional argument", args: []string{"extra"}, wantErr: "no positional", noRequest: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotQuery, gotAuth string
			var requests int
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				gotQuery, gotAuth = r.URL.RawQuery, r.Header.Get("Authorization")
				if r.URL.Path != "/v1/audit" || r.Method != http.MethodGet {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if tt.status >= 400 {
					w.Header().Set("Content-Type", "application/json")
				} else {
					w.Header().Set("Content-Type", "application/x-ndjson")
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer fake.Close()
			t.Setenv("CONCH_TOKEN", "cnch_test-token")
			var stdout, stderr bytes.Buffer
			err := Run(context.Background(), append([]string{"audit", "export", "--server", fake.URL}, tt.args...), &stdout, &stderr, "test")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "\n") || strings.Contains(err.Error(), "\x1b") {
					t.Errorf("error is not one safe line: %q", err)
				}
				if stdout.Len() != 0 {
					t.Errorf("a failed export wrote to stdout: %q", stdout.String())
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if tt.noRequest && requests != 0 {
				t.Errorf("a usage error sent %d requests", requests)
			}
			if !tt.noRequest {
				if gotQuery != tt.wantQuery {
					t.Errorf("query = %q, want %q", gotQuery, tt.wantQuery)
				}
				if gotAuth != "Bearer cnch_test-token" {
					t.Errorf("Authorization = %q", gotAuth)
				}
			}
			if tt.wantErr == "" && stdout.String() != tt.wantOut {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantOut)
			}
		})
	}
}

func TestAuditSubcommandErrors(t *testing.T) {
	for _, args := range [][]string{{"audit"}, {"audit", "nope"}} {
		var out, errOut bytes.Buffer
		if err := Run(context.Background(), args, &out, &errOut, "test"); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}

func TestUsageMentionsAuditExport(t *testing.T) {
	var out bytes.Buffer
	Usage(&out)
	for _, want := range []string{"conch audit export", "--after", "--limit", "audit_exported"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("usage lacks %q", want)
		}
	}
}
