package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// Message search (issue #22) is a second way to read messages, so these tests
// are about disclosure first: a caller gets from search exactly what listing
// gives them, and nothing about a message they cannot see.

func (f *scopedFixture) search(t *testing.T, who, query string) wireResult {
	t.Helper()
	tok := ""
	if who != "" {
		tok = f.p(who).token
	}
	return f.callREST(t, "GET", "/v2/channels/ops/messages/search?"+query, tok, "")
}

func searchQ(words string, extra ...string) string {
	q := "q=" + url.QueryEscape(words)
	for _, e := range extra {
		q += "&" + e
	}
	return q
}

// postBody is the JSON of a post by the fixture's people: channel-wide
// ("wide"), to net1 ("net") or a whisper to olga ("whisper").
func (f *scopedFixture) postBody(kind, text string) string {
	switch kind {
	case "net":
		return fmt.Sprintf(`{"body":%q,%s}`, text, netAudience(f.net1.ID))
	case "whisper":
		return fmt.Sprintf(`{"body":%q,%s}`, text, whisperAudience(f.p("olga").ID))
	}
	return fmt.Sprintf(`{"body":%q}`, text)
}

type searchPage struct {
	ids        []int64
	nextBefore int64
}

func decodeSearch(t *testing.T, body string) searchPage {
	t.Helper()
	var resp schema.SearchMessagesResponseV2
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("decode search response %q: %v", body, err)
	}
	p := searchPage{ids: []int64{}, nextBefore: resp.NextBefore}
	for _, m := range resp.Messages {
		if err := m.Validate(); err != nil {
			t.Errorf("search returned an invalid message: %v", err)
		}
		p.ids = append(p.ids, m.ID)
	}
	return p
}

// The visibility matrix: for a channel-wide message, a net message and a
// whisper, every kind of caller gets from search exactly the messages (and the
// refusal) that listing gives them.
func TestSearchMatchesListForEveryCaller(t *testing.T) {
	f := newScopedFixture(t)
	wide := f.mustPost(t, "olga", f.postBody("wide", "needle wide"))
	net := f.mustPost(t, "ann", f.postBody("net", "needle net"))
	whisper := f.mustPost(t, "ann", f.postBody("whisper", "needle whisper"))
	f.mustPost(t, "olga", f.postBody("wide", "hay"))
	matching := map[int64]bool{wide: true, net: true, whisper: true}

	tests := []struct {
		name string
		who  string
		want []int64 // newest first; nil means the list refuses and so must search
	}{
		{"whisper author and net member", "ann", []int64{whisper, net, wide}},
		{"net member, not a recipient of the whisper", "nina", []int64{net, wide}},
		{"whisper recipient, not on the net", "olga", []int64{whisper, wide}},
		{"net monitor", "mona", []int64{net, wide}},
		{"channel member in no audience", "wes", []int64{wide}},
		{"operator has no exemption", "root", []int64{wide}},
		{"agent with the grant, on the net", "aria", []int64{net, wide}},
		{"agent in the channel without read", "arlo", nil},
		{"agent outside the channel", "nomad", nil},
		{"non-member", "nora", nil},
		{"no credential", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok := ""
			if tt.who != "" {
				tok = f.p(tt.who).token
			}
			list := f.callREST(t, "GET", "/v2/channels/ops/messages", tok, "")
			got := f.search(t, tt.who, searchQ("needle"))
			if tt.want == nil {
				if list.status == http.StatusOK {
					t.Fatalf("test table is wrong: list answered 200 for %q", tt.who)
				}
				if got.status != list.status || got.body != list.body {
					t.Errorf("search = %d %s, list = %d %s; a refusal must be the same", got.status, got.body, list.status, list.body)
				}
				return
			}
			if got.status != http.StatusOK || got.contentType != "application/json" {
				t.Fatalf("search = %d %s (%s)", got.status, got.body, got.contentType)
			}
			page := decodeSearch(t, got.body)
			if !reflect.DeepEqual(page.ids, tt.want) {
				t.Errorf("search = %v, want %v", page.ids, tt.want)
			}
			// And it is what listing shows, restricted to the matches, newest first.
			listed, _ := listIDs(t, list.body)
			fromList := []int64{}
			for i := len(listed) - 1; i >= 0; i-- {
				if matching[listed[i]] {
					fromList = append(fromList, listed[i])
				}
			}
			if !reflect.DeepEqual(page.ids, fromList) {
				t.Errorf("search = %v, but listing shows %v", page.ids, fromList)
			}
			if page.nextBefore != 0 {
				t.Errorf("next_before = %d on a page that holds every match", page.nextBefore)
			}
		})
	}
}

var createdAt = regexp.MustCompile(`"created_at":"[^"]*"`)

// A caller outside a scoped message's audience cannot tell it is there: with
// the message present, every response a search can give them (body and
// next_before, every page size, every page) equals the response when a message
// that does not match took its place. That includes the message being the
// newest of a full page, and the oldest.
func TestSearchIsIndistinguishableFromAbsence(t *testing.T) {
	// outsiders cannot see the scoped message of the kind in question.
	for _, tt := range []struct {
		kind      string
		outsiders []string
	}{
		{"net", []string{"wes", "olga"}},
		{"whisper", []string{"wes", "nina", "mona"}},
	} {
		for _, position := range []string{"newest", "middle", "oldest"} {
			t.Run(tt.kind+" "+position, func(t *testing.T) {
				run := func(present bool) map[string]string {
					f := newScopedFixture(t)
					texts := []string{"needle a", "needle b", "needle c"}
					at := map[string]int{"oldest": 0, "middle": 2, "newest": 3}[position]
					seq := append(append(append([]string{}, texts[:at]...), "@"), texts[at:]...)
					for _, text := range seq {
						switch {
						case text != "@":
							f.mustPost(t, "ann", f.postBody("wide", text))
						case present:
							f.mustPost(t, "ann", f.postBody(tt.kind, "needle hidden"))
						default:
							f.mustPost(t, "ann", f.postBody("wide", "nothing to see"))
						}
					}
					responses := map[string]string{}
					for _, who := range tt.outsiders {
						if f.people[who] == nil {
							t.Fatalf("outsider %q is not in the fixture; the case would compare nothing for them", who)
						}
						for limit := 1; limit <= 4; limit++ {
							before := int64(0)
							for page := 0; page < 6; page++ {
								q := searchQ("needle", fmt.Sprintf("limit=%d", limit))
								if before != 0 {
									q += fmt.Sprintf("&before=%d", before)
								}
								res := f.search(t, who, q)
								responses[fmt.Sprintf("%s/limit=%d/page=%d", who, limit, page)] =
									fmt.Sprintf("%d %s", res.status, createdAt.ReplaceAllString(res.body, `"created_at":"-"`))
								if res.status != http.StatusOK {
									break
								}
								p := decodeSearch(t, res.body)
								if p.nextBefore == 0 {
									break
								}
								before = p.nextBefore
							}
						}
					}
					return responses
				}
				present, absent := run(true), run(false)
				if len(present) == 0 {
					t.Fatal("no responses were compared")
				}
				if !reflect.DeepEqual(present, absent) {
					for k := range present {
						if present[k] != absent[k] {
							t.Errorf("%s differs:\n with the scoped message: %s\n without it:            %s", k, present[k], absent[k])
						}
					}
				}
			})
		}
	}
}

// Hostile input is only ever words: each of these is a 200 and writes no
// error line, because nothing a caller types reaches FTS5's query language.
func TestSearchHostileQueries(t *testing.T) {
	f := newScopedFixture(t)
	f.mustPost(t, "olga", f.postBody("wide", "alpha beta body x NEAR a b OR plain"))
	f.mustPost(t, "olga", f.postBody("wide", "héllo wörld Ünïcode 日本語 text"))
	logs := captureLogsLocked(t)
	tests := []struct {
		name  string
		query string // the raw, already-escaped query string
	}{
		{"quote", "q=%22"},
		{"empty phrase", "q=%22%22"},
		{"star", "q=*"},
		{"minus word", "q=-word"},
		{"NEAR", "q=NEAR(a%20b)"},
		{"column filter", "q=body:x"},
		{"OR", "q=a%20OR%20b"},
		{"open paren", "q=("},
		{"close paren", "q=)"},
		{"NUL byte", "q=a%00b"},
		{"only NUL", "q=%00"},
		{"256-byte word", "q=" + strings.Repeat("a", 256)},
		{"non-ASCII", "q=" + url.QueryEscape("héllo 日本語")},
		{"emoji", "q=" + url.QueryEscape("😀")},
		{"quotes inside", "q=a%22b%22%20%22c"},
		{"caret and plus", "q=%5Ea+b"},
		{"colon", "q=:"},
		{"AND NOT", "q=alpha%20AND%20NOT%20beta"},
		{"column filter on a missing column", "q=nope:alpha"},
		{"braces", "q=%7Bbody%7D:alpha"},
		{"control characters", "q=%01%02%1f"},
		{"invalid UTF-8", "q=%ff%fe"},
		{"semicolon and SQL", "q=%27%3BDROP%20TABLE%20messages%3B--"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, who := range []string{"wes", "ann"} {
				res := f.callREST(t, "GET", "/v2/channels/ops/messages/search?"+tt.query, f.p(who).token, "")
				if res.status != http.StatusOK {
					t.Fatalf("%s: %d %s", who, res.status, res.body)
				}
				decodeSearch(t, res.body)
			}
		})
	}
	if out := logs.String(); strings.Contains(out, "level=ERROR") || strings.Contains(out, "level=WARN") {
		t.Errorf("a hostile query logged:\n%s", out)
	}
	// The words that are in the data still match as plain words, and the
	// operators among them did nothing: "alpha OR zebra" needs all three
	// words, and "body:zzz" is two words, not a column filter.
	for q, want := range map[string]int{"near": 1, "OR": 1, "x": 1, "ÜNÏCODE": 1, "日本語": 1, "alpha OR zebra": 0, "body:zzz": 0, "body:x": 1} {
		if got := len(decodeSearch(t, f.search(t, "wes", searchQ(q)).body).ids); got != want {
			t.Errorf("q=%q matched %d messages, want %d", q, got, want)
		}
	}
}

func TestSearchParameterRules(t *testing.T) {
	f := newScopedFixture(t)
	f.mustPost(t, "olga", f.postBody("wide", "hello"))
	tests := []struct {
		name   string
		query  string
		status int
		code   string
	}{
		{"q missing", "", 400, "invalid_request"},
		{"q empty", "q=", 400, "invalid_request"},
		{"q only whitespace", "q=%20%20%09", 400, "invalid_request"},
		{"q 257 bytes", "q=" + strings.Repeat("a", 257), 400, "invalid_request"},
		{"q 256 bytes", "q=" + strings.Repeat("a", 256), 200, ""},
		{"q 257 bytes in two words", "q=" + strings.Repeat("a", 128) + "%20" + strings.Repeat("a", 128), 400, "invalid_request"},
		{"limit 0", "q=hello&limit=0", 400, "invalid_request"},
		{"limit over the cap", fmt.Sprintf("q=hello&limit=%d", maxMessageLimit+1), 400, "invalid_request"},
		{"limit at the cap", fmt.Sprintf("q=hello&limit=%d", maxMessageLimit), 200, ""},
		{"limit not a number", "q=hello&limit=x", 400, "invalid_request"},
		{"before negative", "q=hello&before=-1", 400, "invalid_request"},
		{"before not a number", "q=hello&before=x", 400, "invalid_request"},
		{"before zero", "q=hello&before=0", 200, ""},
		{"before below everything", "q=hello&before=1", 200, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.search(t, "wes", tt.query)
			if res.status != tt.status {
				t.Fatalf("status = %d %s, want %d", res.status, res.body, tt.status)
			}
			if tt.code != "" {
				var e schema.Error
				if err := json.Unmarshal([]byte(res.body), &e); err != nil || e.Code != tt.code || e.Message == "" {
					t.Errorf("error body = %s (%v), want code %q", res.body, err, tt.code)
				}
			}
		})
	}
}

// The default and the cap are the list route's, and following next_before to
// the end returns every visible match once, newest first.
func TestSearchPagesToTheEnd(t *testing.T) {
	f := newScopedFixture(t)
	var want []int64
	for i := 0; i < 7; i++ {
		want = append([]int64{f.mustPost(t, "olga", f.postBody("wide", "needle"))}, want...)
		f.mustPost(t, "ann", f.postBody("net", "needle hidden"))
	}
	var got []int64
	before := int64(0)
	pages := 0
	for {
		q := searchQ("needle", "limit=3")
		if before != 0 {
			q += fmt.Sprintf("&before=%d", before)
		}
		res := f.search(t, "wes", q)
		if res.status != http.StatusOK {
			t.Fatalf("%d %s", res.status, res.body)
		}
		p := decodeSearch(t, res.body)
		got = append(got, p.ids...)
		pages++
		if p.nextBefore == 0 {
			break
		}
		if p.nextBefore != p.ids[len(p.ids)-1] {
			t.Fatalf("next_before = %d, want the oldest id on the page, %d", p.nextBefore, p.ids[len(p.ids)-1])
		}
		before = p.nextBefore
		if pages > 10 {
			t.Fatal("paging did not end")
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("paged %v, want %v", got, want)
	}
	if pages != 3 {
		t.Errorf("pages = %d, want 3 (3+3+1)", pages)
	}
	// The default page size is the list route's.
	for i := 0; i < defaultMessageLimit; i++ {
		f.mustPost(t, "olga", f.postBody("wide", "bulk"))
	}
	if p := decodeSearch(t, f.search(t, "wes", searchQ("bulk")).body); len(p.ids) != defaultMessageLimit || p.nextBefore != 0 {
		t.Errorf("default page: %d messages, next_before %d; want %d and none", len(p.ids), p.nextBefore, defaultMessageLimit)
	}
	f.mustPost(t, "olga", f.postBody("wide", "bulk"))
	if p := decodeSearch(t, f.search(t, "wes", searchQ("bulk")).body); len(p.ids) != defaultMessageLimit || p.nextBefore == 0 {
		t.Errorf("default page with one more: %d messages, next_before %d; want a full page and a cursor", len(p.ids), p.nextBefore)
	}
}

// A non-member gets the answer for an unknown channel, and v0 and v1 get no
// search at all.
func TestSearchAuthorisation(t *testing.T) {
	f := newScopedFixture(t)
	f.mustPost(t, "olga", f.postBody("wide", "hello"))
	unknown := f.callREST(t, "GET", "/v2/channels/nowhere/messages/search?q=hello", f.p("nora").token, "")
	member := f.search(t, "nora", "q=hello")
	if member.status != http.StatusNotFound || member.body != unknown.body {
		t.Errorf("non-member = %d %s, unknown channel = %d %s; they must be the same", member.status, member.body, unknown.status, unknown.body)
	}
	if res := f.search(t, "", "q=hello"); res.status != http.StatusUnauthorized {
		t.Errorf("no credential = %d, want 401", res.status)
	}
	// The refusal comes before the parameters are looked at.
	if res := f.search(t, "nora", ""); res.status != http.StatusNotFound {
		t.Errorf("non-member without q = %d, want 404", res.status)
	}
	for _, v := range []string{"v0", "v1"} {
		res := f.callREST(t, "GET", "/"+v+"/channels/ops/messages/search?q=hello", f.p("olga").token, "")
		if res.status != http.StatusNotFound && res.status != http.StatusMethodNotAllowed {
			t.Errorf("%s search = %d, want no such route", v, res.status)
		}
	}
	// Removing a recipient from the channel hides what they were sent, and the
	// channel itself.
	netMsg := f.mustPost(t, "ann", f.postBody("net", "needle net"))
	if got := decodeSearch(t, f.search(t, "nina", searchQ("needle")).body).ids; !reflect.DeepEqual(got, []int64{netMsg}) {
		t.Fatalf("nina before removal = %v", got)
	}
	if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.p("nina").ID); err != nil {
		t.Fatal(err)
	}
	if res := f.search(t, "nina", searchQ("needle")); res.status != http.StatusNotFound {
		t.Errorf("removed member = %d %s, want 404", res.status, res.body)
	}
}

// A message is findable as soon as its post returns, whoever may see it.
func TestSearchFindsAPostAtOnce(t *testing.T) {
	f := newScopedFixture(t)
	for _, tt := range []struct{ kind, who string }{{"wide", "wes"}, {"net", "nina"}, {"whisper", "olga"}} {
		t.Run(tt.kind, func(t *testing.T) {
			id := f.mustPost(t, "ann", f.postBody(tt.kind, "fresh "+tt.kind))
			got := decodeSearch(t, f.search(t, tt.who, searchQ("fresh "+tt.kind)).body).ids
			if len(got) == 0 || got[0] != id {
				t.Errorf("after posting %d, search = %v", id, got)
			}
		})
	}
}
