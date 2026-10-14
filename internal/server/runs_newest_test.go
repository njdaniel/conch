package server

import (
	"fmt"
	"net/http"
	"testing"
)

// The list newest first (issue #317): order=desc with no before is the newest
// page, before pages back from the smallest id held, a run the reader may not
// see neither fills a page nor moves the cursor, session= narrows, and no
// response field is added.
func TestListRunsNewestFirst(t *testing.T) {
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	other := f.session(t, f.botTok)
	var bobs []int64 // ascending
	for i := 0; i < 4; i++ {
		bobs = append(bobs, f.start(t, f.botTok, sess, "general", 0, fmt.Sprintf("open %d", i)).ID)
		f.start(t, f.botTok, sess, "general", f.whisper, "hidden from bob")
	}
	onOther := f.start(t, f.botTok, other, "general", 0, "on the other session").ID
	bobs = append(bobs, onOther)
	newest := make([]int64, len(bobs))
	for i, id := range bobs {
		newest[len(bobs)-1-i] = id
	}

	var got []int64
	query := "/v1/runs?channel=general&order=desc&limit=2"
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("paging did not end")
		}
		rec := f.do(t, "GET", query, f.bobTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body)
		}
		page := validRunList(t, rec)
		if page.NextAfter != 0 {
			t.Errorf("a newest-first page advertises next_after %d", page.NextAfter)
		}
		if len(page.Runs) == 0 {
			break
		}
		for _, r := range page.Runs {
			got = append(got, r.ID)
		}
		query = fmt.Sprintf("/v1/runs?channel=general&order=desc&limit=2&before=%d", page.Runs[len(page.Runs)-1].ID)
	}
	if fmt.Sprint(got) != fmt.Sprint(newest) {
		t.Errorf("bob paged %v, want %v", got, newest)
	}

	// The newest page of one session, and the same read with empty cursors.
	page := validRunList(t, f.do(t, "GET", fmt.Sprintf("/v1/runs?channel=general&session=%d&order=desc&limit=10", other), f.bobTok, ""))
	if len(page.Runs) != 1 || page.Runs[0].ID != onOther {
		t.Errorf("session filter = %+v", page)
	}
	page = validRunList(t, f.do(t, "GET", "/v1/runs?channel=general&order=desc&before=&after=&limit=3", f.bobTok, ""))
	if len(page.Runs) != 3 || page.Runs[0].ID != newest[0] {
		t.Errorf("empty cursors listed %v, want the 3 newest from %d", page.Runs, newest[0])
	}
	// order=asc says what no order says.
	page = validRunList(t, f.do(t, "GET", "/v1/runs?channel=general&order=asc&after=0&limit=2", f.bobTok, ""))
	if len(page.Runs) != 2 || page.Runs[0].ID != bobs[0] || page.NextAfter != bobs[1] {
		t.Errorf("order=asc = %+v, want %v from the start", page, bobs[:2])
	}
	// The whispers bob is not in never show, whatever the page.
	for _, r := range validRunList(t, f.do(t, "GET", "/v1/runs?channel=general&order=desc", f.bobTok, "")).Runs {
		if r.Task == "hidden from bob" {
			t.Errorf("bob was shown run %d: %q", r.ID, r.Task)
		}
	}

	for _, c := range []struct{ name, query string }{
		{"after and before", "channel=general&order=desc&after=1&before=9"},
		{"after and before without an order", "channel=general&after=1&before=9"},
		{"before with asc", "channel=general&before=9"},
		{"before with order=asc", "channel=general&order=asc&before=9"},
		{"after with desc", "channel=general&order=desc&after=1"},
		{"after=0 with desc", "channel=general&order=desc&after=0"},
		{"an unknown order", "channel=general&order=up"},
		{"a negative before", "channel=general&order=desc&before=-1"},
		{"a before that is not a number", "channel=general&order=desc&before=x"},
		{"a limit of 0", "channel=general&order=desc&limit=0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertErrorBody(t, f.do(t, "GET", "/v1/runs?"+c.query, f.bobTok, ""), 400, "invalid_request")
		})
	}
}
