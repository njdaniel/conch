package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// HTTP tests of the DM open, resume, list and state routes (issue #2019).
// Every member here holds a self-made credential (an invite redemption), as
// the routes require; the act-as refusal of the same routes is driven by
// TestSelfMadeCredentialRoutesRefuseOthers from selfMadeCredentialRoutes.

type dmMember struct {
	p   store.Principal
	tok string
}

type dmFixture struct {
	*authFixture
	n int
}

func newDMFixture(t *testing.T) *dmFixture {
	t.Helper()
	return &dmFixture{authFixture: newAuthFixture(t, AuthRequired)}
}

// join makes a member through an open invite: their credential is their own.
func (f *dmFixture) join(t *testing.T) dmMember {
	t.Helper()
	f.n++
	ctx := context.Background()
	now := time.Now()
	inv, err := f.srv.store.CreateInvite(ctx, "principal:"+strconv.FormatInt(f.root.ID, 10), f.root.ID,
		schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}, store.InviteOptions{}, now)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	r, err := f.srv.store.RedeemInvite(ctx, store.HashInviteCode(inv.Code), fmt.Sprintf("dmuser%d", f.n), now)
	if err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}
	return dmMember{p: r.Principal, tok: r.Token}
}

func (f *dmFixture) open(t *testing.T, who dmMember, peer int64) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodPost, "/v1/dms", who.tok, fmt.Sprintf(`{"peer_id":%d}`, peer))
}

func (f *dmFixture) mustOpen(t *testing.T, who dmMember, peer int64) schema.DMConversationV1 {
	t.Helper()
	rec := f.open(t, who, peer)
	if rec.Code != http.StatusOK {
		t.Fatalf("open: %d %s", rec.Code, rec.Body)
	}
	var resp schema.OpenDMResponseV1
	dmDecode(t, rec, &resp)
	if err := resp.Validate(); err != nil {
		t.Fatalf("open response: %v", err)
	}
	return resp.Conversation
}

func dmDecode(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("decode %q: %v", rec.Body, err)
	}
}

func (f *dmFixture) list(t *testing.T, who dmMember, query string) schema.ListDMConversationsResponseV1 {
	t.Helper()
	rec := f.do(t, http.MethodGet, "/v1/dms"+query, who.tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list %s: %d %s", query, rec.Code, rec.Body)
	}
	var resp schema.ListDMConversationsResponseV1
	dmDecode(t, rec, &resp)
	if err := resp.Validate(); err != nil {
		t.Fatalf("list response: %v", err)
	}
	return resp
}

func (f *dmFixture) wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status %d %s, want %d %s", rec.Code, rec.Body, status, code)
	}
	if got := answerCode(t, rec); got != code {
		t.Fatalf("code %q (%s), want %q", got, rec.Body, code)
	}
}

func (f *dmFixture) send(t *testing.T, conv, author int64, at time.Time) {
	t.Helper()
	_, _, err := f.srv.store.InsertDMMessage(context.Background(), store.DMPost{
		ConversationID: conv, AuthorID: author, Body: "hi", ClientNonce: fmt.Sprintf("n%d-%d", conv, at.UnixNano()),
	}, at)
	if err != nil {
		t.Fatalf("InsertDMMessage: %v", err)
	}
}

func TestDMOpenBothOrders(t *testing.T) {
	f := newDMFixture(t)
	a, b := f.join(t), f.join(t)
	first := f.mustOpen(t, a, b.p.ID)
	if first.PeerID != b.p.ID || first.PeerName != b.p.Name || first.State != schema.DMStateOpen ||
		first.CreatedAt != nil || first.LastMessageID != 0 || first.Revision != 0 {
		t.Errorf("a's first open = %+v", first)
	}
	if want := []int64{min(a.p.ID, b.p.ID), max(a.p.ID, b.p.ID)}; first.ParticipantIDs[0] != want[0] || first.ParticipantIDs[1] != want[1] {
		t.Errorf("participants %v, want %v", first.ParticipantIDs, want)
	}
	// The peer's own open resumes the same conversation, in their own view,
	// with no sign that the other opened first.
	second := f.mustOpen(t, b, a.p.ID)
	if second.ID != first.ID || second.PeerID != a.p.ID || second.PeerName != a.p.Name || second.CreatedAt != nil {
		t.Errorf("b's open = %+v, want conversation %d with peer a", second, first.ID)
	}
	// A repeat in either order answers 200 with the same id.
	if again := f.mustOpen(t, a, b.p.ID); again.ID != first.ID {
		t.Errorf("a's repeat open = %d, want %d", again.ID, first.ID)
	}
	// Both lists now hold it; each in the viewer's own view.
	for _, who := range []dmMember{a, b} {
		got := f.list(t, who, "")
		if len(got.Conversations) != 1 || got.Conversations[0].ID != first.ID || got.NextCursor != "" {
			t.Errorf("%s's list = %+v", who.p.Name, got)
		}
	}
	// The conversation id is no principal id and the same either way round:
	// a fresh pair opened in the other order gets an independent id.
	c := f.join(t)
	x := f.mustOpen(t, c, a.p.ID)
	if x.ID == first.ID {
		t.Errorf("two pairs share conversation id %d", x.ID)
	}
}

func TestDMConcurrentOpens(t *testing.T) {
	f := newDMFixture(t)
	a, b := f.join(t), f.join(t)
	f.srv.setDMNewLimit(inviteLimit{name: "relaxed", burst: testRelaxedBurst, window: time.Hour})
	const n = 12
	ids := make([]int64, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			who, peer := a, b
			if i%2 == 1 {
				who, peer = b, a
			}
			rec := f.open(t, who, peer.p.ID)
			var resp schema.OpenDMResponseV1
			if rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &resp) == nil {
				ids[i] = resp.Conversation.ID
			}
		}()
	}
	wg.Wait()
	for i, id := range ids {
		if id == 0 || id != ids[0] {
			t.Fatalf("open %d answered conversation %d, want %d for every open: %v", i, id, ids[0], ids)
		}
	}
}

// An empty open is private to its opener: nothing for the peer, in their
// list, by id, or on any channel surface.
func TestDMEmptyOpenIsSilentForThePeer(t *testing.T) {
	f := newDMFixture(t)
	a, b := f.join(t), f.join(t)
	snap := func(who dmMember) string {
		var sb strings.Builder
		for _, p := range []string{"/v1/channels", "/v1/directory/channels", "/v1/directory/members", "/v1/notifications/pending"} {
			rec := f.do(t, http.MethodGet, p, who.tok, "")
			fmt.Fprintf(&sb, "%s %d %s\n", p, rec.Code, rec.Body)
		}
		return sb.String()
	}
	beforeA, beforeB := snap(a), snap(b)
	conv := f.mustOpen(t, a, b.p.ID)
	if got := snap(b); got != beforeB {
		t.Errorf("b's channel surfaces changed after a's empty open:\nbefore %s\nafter %s", beforeB, got)
	}
	if got := snap(a); got != beforeA {
		t.Errorf("a's channel surfaces changed after their own empty open:\nbefore %s\nafter %s", beforeA, got)
	}
	if got := f.list(t, b, ""); len(got.Conversations) != 0 || got.Conversations == nil {
		t.Errorf("b's open list = %+v, want empty (never null)", got)
	}
	if got := f.list(t, b, "?state=archived"); len(got.Conversations) != 0 {
		t.Errorf("b's archived list = %+v, want empty", got)
	}
	notFound := f.do(t, http.MethodGet, fmt.Sprintf("/v1/dms/%d", conv.ID), b.tok, "")
	f.wantError(t, notFound, http.StatusNotFound, schema.ErrorCodeDMNotFound)
	// Byte for byte the answer to an id that does not exist.
	missing := f.do(t, http.MethodGet, "/v1/dms/4242424242", b.tok, "")
	if notFound.Body.String() != missing.Body.String() {
		t.Errorf("empty-open 404 %q differs from missing-id 404 %q", notFound.Body, missing.Body)
	}
	state := f.do(t, http.MethodPut, fmt.Sprintf("/v1/dms/%d/state", conv.ID), b.tok, `{"state":"hidden"}`)
	f.wantError(t, state, http.StatusNotFound, schema.ErrorCodeDMNotFound)
	// The opener still sees their own, and a first message surfaces it for b.
	if got := f.list(t, a, ""); len(got.Conversations) != 1 {
		t.Errorf("a's list = %+v, want the empty conversation", got)
	}
	f.send(t, conv.ID, a.p.ID, time.Now())
	got := f.list(t, b, "")
	if len(got.Conversations) != 1 || got.Conversations[0].ID != conv.ID || got.Conversations[0].CreatedAt == nil || got.Conversations[0].Unread != 1 {
		t.Errorf("b's list after a's message = %+v", got)
	}
	if rec := f.do(t, http.MethodGet, fmt.Sprintf("/v1/dms/%d", conv.ID), b.tok, ""); rec.Code != http.StatusOK {
		t.Errorf("b reads the surfaced conversation: %d %s", rec.Code, rec.Body)
	}
}

func TestDMListPaginationTiesAndOrder(t *testing.T) {
	f := newDMFixture(t)
	me := f.join(t)
	f.srv.setDMNewLimit(inviteLimit{name: "relaxed", burst: testRelaxedBurst, window: time.Hour})
	tie := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	var withMsg, empty int
	for i := range 9 {
		peer := f.join(t)
		conv := f.mustOpen(t, me, peer.p.ID)
		switch {
		case i < 4: // the same instant: ties are broken by the conversation id
			f.send(t, conv.ID, me.p.ID, tie)
			withMsg++
		case i < 6:
			f.send(t, conv.ID, me.p.ID, tie.Add(time.Duration(i)*time.Minute))
			withMsg++
		default:
			empty++
		}
	}
	all := f.list(t, me, "")
	if len(all.Conversations) != 9 || all.NextCursor != "" {
		t.Fatalf("full list = %d rows, cursor %q", len(all.Conversations), all.NextCursor)
	}
	// Messages first, newest first with ties by id descending; then the empty
	// ones by id descending.
	for i := 1; i < len(all.Conversations); i++ {
		p, c := all.Conversations[i-1], all.Conversations[i]
		switch {
		case p.LastMessageAt == nil && c.LastMessageAt != nil:
			t.Fatalf("an empty conversation precedes one with a message at %d", i)
		case p.LastMessageAt != nil && c.LastMessageAt != nil && p.LastMessageAt.Time().Before(c.LastMessageAt.Time()):
			t.Fatalf("row %d is newer than row %d", i, i-1)
		case (p.LastMessageAt == nil) == (c.LastMessageAt == nil) &&
			(p.LastMessageAt == nil || p.LastMessageAt.Time().Equal(c.LastMessageAt.Time())) && p.ID < c.ID:
			t.Fatalf("tie at %d is not by id descending: %d then %d", i, p.ID, c.ID)
		}
	}
	for _, limit := range []int{1, 2, 3, 4} {
		var got []int64
		cursor := ""
		for pages := 0; pages < 20; pages++ {
			q := fmt.Sprintf("?limit=%d", limit)
			if cursor != "" {
				q += "&cursor=" + cursor
			}
			page := f.list(t, me, q)
			if len(page.Conversations) > limit {
				t.Fatalf("limit %d page has %d rows", limit, len(page.Conversations))
			}
			for _, c := range page.Conversations {
				got = append(got, c.ID)
			}
			if cursor = page.NextCursor; cursor == "" {
				break
			}
		}
		if len(got) != len(all.Conversations) {
			t.Fatalf("limit %d paged %d rows, want %d: %v", limit, len(got), len(all.Conversations), got)
		}
		for i, c := range all.Conversations {
			if got[i] != c.ID {
				t.Fatalf("limit %d: row %d = %d, want %d (no repeat or skip across a tie)", limit, i, got[i], c.ID)
			}
		}
	}
	// A page that ends exactly on the last row has no cursor.
	if page := f.list(t, me, "?limit=9"); page.NextCursor != "" || len(page.Conversations) != 9 {
		t.Errorf("exact page = %d rows, cursor %q", len(page.Conversations), page.NextCursor)
	}
	// A limit above the maximum is taken as the maximum.
	if page := f.list(t, me, "?limit=100000"); len(page.Conversations) != 9 {
		t.Errorf("large limit page = %d rows", len(page.Conversations))
	}
}

func TestDMListRejectsBadQueries(t *testing.T) {
	f := newDMFixture(t)
	a := f.join(t)
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, q := range map[string]string{
		"hidden state":   "?state=hidden",
		"unknown state":  "?state=all",
		"zero limit":     "?limit=0",
		"negative limit": "?limit=-1",
		"text limit":     "?limit=x",
		"garbage cursor": "?cursor=not-a-cursor",
		"not a token":    "?cursor=" + strings.Repeat("a", schema.MaxDMCursorBytes+1),
		"non-canonical":  "?cursor=" + enc("M:0005:7"),
		"wrong shape":    "?cursor=" + enc("M:5"),
		"zero id":        "?cursor=" + enc("E:0"),
		"negative":       "?cursor=" + enc("M:-1:4"),
		"unpadded junk":  "?cursor=%00",
	} {
		t.Run(name, func(t *testing.T) {
			f.wantError(t, f.do(t, http.MethodGet, "/v1/dms"+q, a.tok, ""), http.StatusBadRequest, "invalid_request")
		})
	}
	if _, err := decodeDMCursor(encodeDMCursor(store.DMListCursor{At: 1234, ID: 9})); err != nil {
		t.Errorf("a cursor the server issued does not decode: %v", err)
	}
}

func TestDMPerViewerHideArchiveReopen(t *testing.T) {
	f := newDMFixture(t)
	a, b := f.join(t), f.join(t)
	conv := f.mustOpen(t, a, b.p.ID)
	f.send(t, conv.ID, a.p.ID, time.Now())
	set := func(who dmMember, state string) *httptest.ResponseRecorder {
		return f.do(t, http.MethodPut, fmt.Sprintf("/v1/dms/%d/state", conv.ID), who.tok, fmt.Sprintf(`{"state":%q}`, state))
	}
	get := func(who dmMember) schema.DMConversationV1 {
		rec := f.do(t, http.MethodGet, fmt.Sprintf("/v1/dms/%d", conv.ID), who.tok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("get: %d %s", rec.Code, rec.Body)
		}
		var c schema.DMConversationV1
		dmDecode(t, rec, &c)
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	counts := func(who dmMember) (open, archived int) {
		return len(f.list(t, who, "").Conversations), len(f.list(t, who, "?state=archived").Conversations)
	}
	if rec := set(a, "archived"); rec.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body)
	}
	if o, ar := counts(a); o != 0 || ar != 1 {
		t.Errorf("a after archive: open %d archived %d", o, ar)
	}
	if o, ar := counts(b); o != 1 || ar != 0 {
		t.Errorf("b after a archives: open %d archived %d (a per-viewer state)", o, ar)
	}
	if rec := set(a, "hidden"); rec.Code != http.StatusOK {
		t.Fatalf("hide: %d %s", rec.Code, rec.Body)
	}
	if o, ar := counts(a); o != 0 || ar != 0 {
		t.Errorf("a after hide: open %d archived %d", o, ar)
	}
	if o, _ := counts(b); o != 1 {
		t.Errorf("b after a hides: open %d", o)
	}
	// Hidden is not gone: the id still reads, with the state, and the history
	// is kept.
	if c := get(a); c.State != schema.DMStateHidden || c.LastMessageID == 0 {
		t.Errorf("a's hidden conversation = %+v", c)
	}
	// Opening with the same person resumes it and sets it open again.
	if c := f.mustOpen(t, a, b.p.ID); c.ID != conv.ID || c.State != schema.DMStateOpen || c.LastMessageID == 0 {
		t.Errorf("reopen = %+v", c)
	}
	if o, _ := counts(a); o != 1 {
		t.Errorf("a after reopen: open %d", o)
	}
	// An explicit reopen of an archived one, and a state of a stranger's.
	set(a, "archived")
	if rec := set(a, "open"); rec.Code != http.StatusOK || get(a).State != schema.DMStateOpen {
		t.Errorf("explicit reopen: %d %s", rec.Code, rec.Body)
	}
	// Archived stays archived on resume.
	set(a, "archived")
	if c := f.mustOpen(t, a, b.p.ID); c.State != schema.DMStateArchived {
		t.Errorf("resume of an archived conversation = %v, want it to stay archived", c.State)
	}
	f.wantError(t, set(a, "bogus"), http.StatusBadRequest, "invalid_request")
	f.wantError(t, f.do(t, http.MethodPut, fmt.Sprintf("/v1/dms/%d/state", conv.ID), a.tok, `{"state":"open","extra":1}`), http.StatusBadRequest, "invalid_request")
	c := f.join(t)
	f.wantError(t, set(c, "hidden"), http.StatusNotFound, schema.ErrorCodeDMNotFound)
}

// A person who is in no pair, whatever their role, finds nothing: one answer
// for the id of a stranger's conversation and for one that never existed.
func TestDMHiddenIDLookupAndUnrelatedOperator(t *testing.T) {
	f := newDMFixture(t)
	a, b, c := f.join(t), f.join(t), f.join(t)
	conv := f.mustOpen(t, a, b.p.ID)
	f.send(t, conv.ID, a.p.ID, time.Now())
	root := dmMember{p: f.root, tok: f.rootTok}
	missing := f.do(t, http.MethodGet, "/v1/dms/5151515151", c.tok, "")
	f.wantError(t, missing, http.StatusNotFound, schema.ErrorCodeDMNotFound)
	for name, who := range map[string]dmMember{"outsider": c, "operator": root} {
		rec := f.do(t, http.MethodGet, fmt.Sprintf("/v1/dms/%d", conv.ID), who.tok, "")
		f.wantError(t, rec, http.StatusNotFound, schema.ErrorCodeDMNotFound)
		if rec.Body.String() != missing.Body.String() {
			t.Errorf("%s: 404 body %q differs from a missing id's %q", name, rec.Body, missing.Body)
		}
		f.wantError(t, f.do(t, http.MethodPut, fmt.Sprintf("/v1/dms/%d/state", conv.ID), who.tok, `{"state":"hidden"}`), http.StatusNotFound, schema.ErrorCodeDMNotFound)
		if got := f.list(t, who, ""); len(got.Conversations) != 0 {
			t.Errorf("%s's inbox = %+v, want none of a stranger's", name, got)
		}
	}
	// The pair is unaffected by the strangers' attempts.
	if got := f.list(t, a, ""); len(got.Conversations) != 1 || got.Conversations[0].State != schema.DMStateOpen {
		t.Errorf("a's inbox after a stranger's probes = %+v", got)
	}
	// Malformed ids are a plain 400, with no lookup.
	f.wantError(t, f.do(t, http.MethodGet, "/v1/dms/0", c.tok, ""), http.StatusBadRequest, "invalid_request")
	f.wantError(t, f.do(t, http.MethodGet, "/v1/dms/abc", c.tok, ""), http.StatusBadRequest, "invalid_request")
}

func TestDMOpenRefusals(t *testing.T) {
	f := newDMFixture(t)
	f.srv.setDMNewLimit(inviteLimit{name: "relaxed", burst: testRelaxedBurst, window: time.Hour})
	ctx := context.Background()
	a, c := f.join(t), f.join(t)

	// The target is the caller, an agent, or nobody: one 400 for the first
	// two, and the same 404 for an id never issued.
	f.wantError(t, f.open(t, a, a.p.ID), http.StatusBadRequest, "invalid_request")
	f.wantError(t, f.open(t, a, f.bot.ID), http.StatusBadRequest, "invalid_request")
	gone := f.open(t, a, 987654)
	f.wantError(t, gone, http.StatusNotFound, "principal_not_found")
	// Hidden is not told apart from never issued.
	hidden := f.join(t)
	if _, err := f.srv.store.HidePrincipal(ctx, "system", hidden.p.ID, store.HideOptions{}); err != nil {
		t.Fatalf("HidePrincipal: %v", err)
	}
	hid := f.open(t, a, hidden.p.ID)
	f.wantError(t, hid, http.StatusNotFound, "principal_not_found")
	if hid.Body.String() != gone.Body.String() {
		t.Errorf("hidden principal %q differs from an unknown id %q", hid.Body, gone.Body)
	}
	// Malformed bodies spend nothing and never reach the store.
	for _, body := range []string{``, `{}`, `{"peer_id":0}`, `{"peer_id":"x"}`, `{"peer_id":5,"payload":{}}`, `{"peer_id":5} {}`} {
		f.wantError(t, f.do(t, http.MethodPost, "/v1/dms", a.tok, body), http.StatusBadRequest, "invalid_request")
	}

	// A peer who accepts nobody cannot be introduced to; nothing is created,
	// so they can see no conversation either.
	if _, err := f.srv.store.SetDMPreferences(ctx, c.p.ID, schema.DMPreferencesV1{Accept: schema.DMAcceptNobody}, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.wantError(t, f.open(t, a, c.p.ID), http.StatusForbidden, schema.ErrorCodeContactUnavailable)
	if got := f.list(t, a, ""); len(got.Conversations) != 0 {
		t.Errorf("a's list after a refused open = %+v", got)
	}
	var e schema.Error
	dmDecode(t, f.open(t, a, c.p.ID), &e)
	if want, _ := schema.DMRefusalError(schema.ErrorCodeContactUnavailable); e != want {
		t.Errorf("contact refusal body = %+v, want the fixed %+v", e, want)
	}

	// The caller's own switch: a new pair is dms_off, a resume is not.
	d := f.join(t)
	resumed := f.mustOpen(t, d, a.p.ID)
	if _, err := f.srv.store.SetDMAllowed(ctx, "system", f.root.ID, d.p.ID, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.wantError(t, f.open(t, d, c.p.ID), http.StatusForbidden, schema.ErrorCodeDMsOff)
	f.wantError(t, f.open(t, d, 987654), http.StatusForbidden, schema.ErrorCodeDMsOff) // before the target is looked up
	if again := f.mustOpen(t, d, a.p.ID); again.ID != resumed.ID {
		t.Errorf("resume while off = %d, want %d", again.ID, resumed.ID)
	}
	// Reads and the state are not writes: they still work while off.
	if rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/dms/%d/state", resumed.ID), d.tok, `{"state":"archived"}`); rec.Code != http.StatusOK {
		t.Errorf("state while off: %d %s", rec.Code, rec.Body)
	}
}

func TestDMNewConversationBucket(t *testing.T) {
	f := newDMFixture(t)
	a, b, c, d := f.join(t), f.join(t), f.join(t), f.join(t)
	f.srv.setDMNewLimit(inviteLimit{name: "dm_new_conversations", burst: 2, window: 24 * time.Hour})
	f.mustOpen(t, a, b.p.ID)
	// A resume spends nothing, either way round.
	for range 3 {
		f.mustOpen(t, a, b.p.ID)
	}
	f.mustOpen(t, b, a.p.ID) // b's first sight of a pair a opened silently is an introduction: one token for b
	// A refused open of a new pair spends a token too: probing costs.
	f.wantError(t, f.open(t, a, 987654), http.StatusNotFound, "principal_not_found")
	rec := f.open(t, a, c.p.ID)
	f.wantError(t, rec, http.StatusTooManyRequests, "rate_limited")
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	// The bucket is per person.
	f.mustOpen(t, d, c.p.ID)
	// And a resume is still allowed once it is empty.
	f.mustOpen(t, a, b.p.ID)
}

func TestDMRoutesRefuseAgentsGuestsAndAuthOff(t *testing.T) {
	f := newDMFixture(t)
	a := f.join(t)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/dms", `{"peer_id":1}`},
		{http.MethodGet, "/v1/dms", ""},
		{http.MethodGet, "/v1/dms/1", ""},
		{http.MethodPut, "/v1/dms/1/state", `{"state":"open"}`},
	} {
		if rec := f.do(t, tc.method, tc.path, f.botTok, tc.body); rec.Code != http.StatusForbidden {
			t.Errorf("agent %s %s: %d %s, want 403", tc.method, tc.path, rec.Code, rec.Body)
		}
		if rec := f.do(t, tc.method, tc.path, "", tc.body); rec.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s: %d, want 401", tc.method, tc.path, rec.Code)
		}
		// An operator-made credential for a member is refused, reads included.
		_, tok, err := f.srv.store.CreateCredential(context.Background(), "principal:"+strconv.FormatInt(f.root.ID, 10), a.p.ID, "act as", nil)
		if err != nil {
			t.Fatal(err)
		}
		rec := f.do(t, tc.method, tc.path, tok, tc.body)
		f.wantError(t, rec, http.StatusForbidden, schema.ErrorCodeOwnSignInRequired)
	}
	// Under AuthOff there is no person to hold a conversation.
	off := newAuthFixture(t, AuthOff)
	rec := off.do(t, http.MethodPost, "/v1/dms", "", `{"peer_id":1}`)
	if rec.Code != http.StatusBadRequest || answerCode(t, rec) != schema.ErrorCodeDMRequireAuth {
		t.Errorf("AuthOff open: %d %s", rec.Code, rec.Body)
	}
}
