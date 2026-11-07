package main

import (
	"slices"
	"strings"
	"testing"
)

// TestPreviewCoverage: the preview phase has a refusal row for every route of
// the table but the five previewable ones and /mcp, each row's request is one
// its route serves, and a list entry the table lacks is stale.
func TestPreviewCoverage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		table   []string
		wantErr string
	}{
		{"every route has a row", append([]string{"POST /v1/hooks/{token}", "GET /v2/ws", "POST /v1/ws-tickets", "/mcp"}, previewable...), ""},
		{"a method-less route other than /mcp fails coverage", []string{"/other", "/mcp"}, "/other takes any method"},
		{"a previewable route the table lost", []string{"GET /v1/channels", "POST /v1/ws-tickets"}, "GET /v1/preview is not in the table"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := previewCoverage(table(t, tc.table...))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("previewCoverage = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("previewCoverage = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

// TestPreviewRefusalRows: the rows name the hook's token and a thing that
// never existed for every other segment, and leave out the previewable
// routes and /mcp.
func TestPreviewRefusalRows(t *testing.T) {
	rows := previewRefusalRows(table(t, "POST /v1/hooks/{token}", "PUT /v1/channels/{id}/name", "GET /v1/channels", "/mcp"), "tok")
	got := map[string]string{}
	for _, r := range rows {
		got[r.route] = r.method + " " + r.path
	}
	want := map[string]string{ //nolint:gosec // route patterns, not credentials
		"POST /v1/hooks/{token}": "POST /v1/hooks/tok", "PUT /v1/channels/{id}/name": "PUT /v1/channels/987654321/name"}
	if len(got) != len(want) {
		t.Fatalf("rows = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("row %s = %q, want %q", k, got[k], v)
		}
	}
}

// TestCheckExempt: the exempt list is cross-checked against the table.
func TestCheckExempt(t *testing.T) {
	const src = `
		rt("GET /healthz", accessExempt, hf(s.h)),
		{pattern: "POST /v1/login", access: accessExempt, handler: h},
		rt("/mcp", accessExempt, s.mcpHandler()), // accessExempt in a comment
`
	for _, tc := range []struct {
		name    string
		src     string
		table   []string
		want    []string
		wantErr string
	}{
		{"agrees with the table", src, []string{"GET /healthz", "POST /v1/login", "/mcp"}, []string{"GET /healthz", "POST /v1/login"}, ""},
		{"an exempt route the table lacks", src, []string{"GET /healthz", "/mcp"}, nil, "POST /v1/login is read as exempt but is not in the route table"},
		{"a previewable route read as exempt", "rt(\"GET /v1/channels\", accessExempt, h),", []string{"GET /v1/channels"}, nil, "is previewable"},
		{"an exempt use the pattern missed", src + "\trt(\"GET\"+x, accessExempt, h),\n", []string{"GET /healthz", "POST /v1/login", "/mcp"}, nil, "a route of the exempt class was missed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := checkExempt(tc.src, table(t, tc.table...))
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("checkExempt = %v, want nil", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("checkExempt = %v, want %q", err, tc.wantErr)
			case tc.wantErr == "" && !slices.Equal(got, tc.want):
				t.Errorf("checkExempt = %v, want %v", got, tc.want)
			}
		})
	}
}
