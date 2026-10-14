package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func (f *scopedFixture) listIDs(t *testing.T, who, path string) ([]int64, schema.ListMessagesResponseV2) {
	t.Helper()
	res := f.callREST(t, "GET", path, f.p(who).token, "")
	if res.status != http.StatusOK {
		t.Fatalf("%s %s = %d %s", who, path, res.status, res.body)
	}
	var resp schema.ListMessagesResponseV2
	if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	ids := []int64{}
	for _, m := range resp.Messages {
		ids = append(ids, m.ID)
	}
	return ids, resp
}

// The message list newest first (issue #334): order=desc with no before is the
// newest page, before pages back, a message the reader may not see neither
// fills a page nor moves the cursor, next_after is never set, and the
// ascending read is as it was.
func TestListMessagesNewestFirst(t *testing.T) {
	f := newScopedFixture(t)
	// olga is on no net and is the whisper's recipient; nina is on net1 and
	// is not. So olga is shown the wide and whisper messages, nina the wide
	// and net ones.
	seen := map[string][]int64{}
	for i := 0; i < 4; i++ {
		for _, kind := range []string{"wide", "net", "whisper"} {
			id := f.mustPost(t, "ann", f.postBody(kind, fmt.Sprintf("%s %d", kind, i)))
			if kind != "net" {
				seen["olga"] = append(seen["olga"], id)
			}
			if kind != "whisper" {
				seen["nina"] = append(seen["nina"], id)
			}
		}
	}
	reverse := func(ids []int64) []int64 {
		out := make([]int64, len(ids))
		for i, id := range ids {
			out[len(ids)-1-i] = id
		}
		return out
	}

	for _, who := range []string{"olga", "nina"} {
		want := reverse(seen[who])
		var got []int64
		path := "/v2/channels/ops/messages?order=desc&limit=3"
		for pages := 0; ; pages++ {
			if pages > 20 {
				t.Fatal("paging did not end")
			}
			ids, resp := f.listIDs(t, who, path)
			if resp.NextAfter != 0 {
				t.Errorf("%s: a newest-first page advertises next_after %d", who, resp.NextAfter)
			}
			if len(ids) == 0 {
				break
			}
			got = append(got, ids...)
			path = fmt.Sprintf("/v2/channels/ops/messages?order=desc&limit=3&before=%d", ids[len(ids)-1])
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s paged %v, want %v", who, got, want)
		}

		// The newest page alone, with and without empty cursors.
		for _, q := range []string{"order=desc&limit=2", "order=desc&before=&after=&limit=2"} {
			if ids, _ := f.listIDs(t, who, "/v2/channels/ops/messages?"+q); !reflect.DeepEqual(ids, want[:2]) {
				t.Errorf("%s %s = %v, want %v", who, q, ids, want[:2])
			}
		}
		// order=asc says what no order says, and is unchanged.
		ids, resp := f.listIDs(t, who, "/v2/channels/ops/messages?order=asc&after=0&limit=2")
		if wantAsc := reverse(want)[:2]; !reflect.DeepEqual(ids, wantAsc) || resp.NextAfter != wantAsc[1] {
			t.Errorf("%s order=asc = %v next_after %d, want %v", who, ids, resp.NextAfter, wantAsc)
		}
		ids, _ = f.listIDs(t, who, "/v2/channels/ops/messages")
		if !reflect.DeepEqual(ids, reverse(want)) {
			t.Errorf("%s default read = %v, want %v", who, ids, reverse(want))
		}
	}

	// v1 shares the handler and so the order; it never carries a scoped message.
	res := f.callREST(t, "GET", "/v1/channels/ops/messages?order=desc&limit=2", f.p("nina").token, "")
	var v1 schema.ListMessagesResponseV1
	if err := json.Unmarshal([]byte(res.body), &v1); err != nil || res.status != 200 || len(v1.Messages) != 2 ||
		v1.Messages[0].ID <= v1.Messages[1].ID || v1.NextAfter != 0 {
		t.Errorf("v1 order=desc = %d %s (%v)", res.status, res.body, err)
	}
	// v0 is untouched: it has no order and still ignores what it does not know.
	res = f.callREST(t, "GET", "/v0/channels/ops/messages?order=desc&before=9&limit=2", f.p("nina").token, "")
	var v0 schema.ListMessagesResponse
	if err := json.Unmarshal([]byte(res.body), &v0); err != nil || res.status != 200 || len(v0.Messages) != 2 ||
		v0.Messages[0].ID >= v0.Messages[1].ID {
		t.Errorf("v0 with order=desc = %d %s (%v)", res.status, res.body, err)
	}

	for _, c := range []struct{ name, query string }{
		{"after and before", "order=desc&after=1&before=9"},
		{"after and before without an order", "after=1&before=9"},
		{"before with asc", "before=9"},
		{"before with order=asc", "order=asc&before=9"},
		{"after with desc", "order=desc&after=1"},
		{"after=0 with desc", "order=desc&after=0"},
		{"an unknown order", "order=up"},
		{"a negative before", "order=desc&before=-1"},
		{"a before that is not a number", "order=desc&before=x"},
		{"a negative after", "after=-1"},
		{"a limit of 0", "order=desc&limit=0"},
		{"a limit over 100", "order=desc&limit=101"},
		{"a thread read newest first (v2 only: v1 has no threads)", "order=desc&thread=1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, version := range []string{"v1", "v2"} {
				if version == "v1" && c.query == "order=desc&thread=1" {
					continue
				}
				res := f.callREST(t, "GET", "/"+version+"/channels/ops/messages?"+c.query, f.p("nina").token, "")
				if res.status != http.StatusBadRequest {
					t.Errorf("%s: %s = %d %s, want 400", version, c.query, res.status, res.body)
				}
			}
		})
	}
}
