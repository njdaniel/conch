package server

import (
	"fmt"
	"net/http"
	"testing"
)

// TestPermissionsPolicyOutsideApp pins N8 (ADR-015, design §6.4): every answer
// outside the web client carries the deny-all Permissions-Policy, a refusal,
// a redirect and a 404 included; the web client's own answers carry its
// policy and nothing else, once.
func TestPermissionsPolicyOutsideApp(t *testing.T) {
	ts := webServer(t, AuthRequired, webFixture(t))
	for _, tt := range []struct {
		name   string
		path   string
		status int
		want   string
	}{
		{"an API read", "/healthz", http.StatusOK, denyPermissionsPolicy},
		{"an API refusal", "/v1/whoami", http.StatusUnauthorized, denyPermissionsPolicy},
		{"the redirect to the client", "/", http.StatusFound, denyPermissionsPolicy},
		{"the client's path without its slash", "/app", http.StatusMovedPermanently, denyPermissionsPolicy},
		{"a path nothing serves, refused unauthenticated", "/no-such-path", http.StatusUnauthorized, denyPermissionsPolicy},
		{"the client", "/app/", http.StatusOK, webPermissionsPolicy},
		{"a file of the client", "/app/assets/x.js", http.StatusOK, webPermissionsPolicy},
		{"a 404 of the client", "/app/no-such-file.js", http.StatusNotFound, webPermissionsPolicy},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resp, _ := fetch(t, "GET", ts.URL+tt.path)
			if resp.StatusCode != tt.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.status)
			}
			if got := resp.Header.Values("Permissions-Policy"); len(got) != 1 || got[0] != tt.want {
				t.Errorf("Permissions-Policy = %q, want exactly [%q]", got, tt.want)
			}
		})
	}
}

// TestPermissionsPolicyOnAttachmentsAndReads: an authenticated API read, an
// attachment download and a 404 carry the deny-all policy too.
func TestPermissionsPolicyOnAttachmentsAndReads(t *testing.T) {
	f := newFileFixture(t)
	meta := f.mustUpload(t, "ann", "plan.txt", []byte("an attachment"))
	for _, tt := range []struct {
		name   string
		path   string
		status int
	}{
		{"an API read", "/v1/whoami", http.StatusOK},
		{"an attachment download", fmt.Sprintf("/v2/files/%d", meta.ID), http.StatusOK},
		{"an attachment's metadata", fmt.Sprintf("/v2/files/%d/meta", meta.ID), http.StatusOK},
		{"an attachment nobody has", "/v2/files/999999", http.StatusNotFound},
		{"a path nothing serves", "/no-such-path", http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res := f.do(t, "GET", tt.path, "ann", "", nil)
			if res.status != tt.status {
				t.Fatalf("status = %s, want %d", res, tt.status)
			}
			if got := res.header.Values("Permissions-Policy"); len(got) != 1 || got[0] != denyPermissionsPolicy {
				t.Errorf("Permissions-Policy = %q, want exactly [%q]", got, denyPermissionsPolicy)
			}
		})
	}
}
