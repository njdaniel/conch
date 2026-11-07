package server

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestPreviewableRoutesAreReads is the structural half of preview's safety
// argument (design §4.3, §12): the allowlist is what makes preview read-only,
// so it is closed. A previewable route must be a GET, must not be a socket,
// and must be one of the five this test names, so adding one is a deliberate,
// reviewed act.
func TestPreviewableRoutesAreReads(t *testing.T) {
	// The test's own list, not routePreviewable's: two statements of it.
	allowed := []string{
		"GET /v1/preview",
		"GET /v1/channels",
		"GET /v1/directory/channels",
		"GET /v1/directory/members",
		"GET /v1/channels/{channel}/members",
	}
	f := newAuthFixture(t, AuthRequired)
	var got []string
	for _, rt := range f.srv.routes {
		if !rt.previewable {
			continue
		}
		got = append(got, rt.pattern)
		if !strings.HasPrefix(rt.pattern, "GET ") {
			t.Errorf("previewable route %q is not a GET", rt.pattern)
		}
		if strings.HasSuffix(rt.pattern, "/ws") {
			t.Errorf("previewable route %q is a socket", rt.pattern)
		}
		if rt.acceptsTicket {
			t.Errorf("previewable route %q accepts a ticket", rt.pattern)
		}
		if rt.access == accessExempt {
			t.Errorf("previewable route %q is exempt from authentication", rt.pattern)
		}
		if !slices.Contains(allowed, rt.pattern) {
			t.Errorf("previewable route %q is not in this test's list: adding one is a design change (permission-preview.md §4.3)", rt.pattern)
		}
	}
	sort.Strings(got)
	sort.Strings(allowed)
	if !slices.Equal(got, allowed) {
		t.Errorf("previewable routes = %v, want %v", got, allowed)
	}
}
