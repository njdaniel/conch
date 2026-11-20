package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Personal blocks through the handlers (issue #1832): the routes and their
// access, the owner-only rule, the limit, idempotence, the owner-only signal,
// and that the blocked person sees no difference anywhere.

func blockPath(id int64) string { return fmt.Sprintf("/v1/blocks/%d", id) }

func decodeBlocks(t *testing.T, rec *httptest.ResponseRecorder) schema.ListBlocksResponseV1 {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/blocks = %d %s", rec.Code, rec.Body)
	}
	var resp schema.ListBlocksResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if err := resp.Validate(); err != nil {
		t.Fatalf("list does not validate: %v", err)
	}
	return resp
}

func TestBlockRoutes(t *testing.T) {
	f := newDirectoryFixture(t)
	f.srv.relaxNotificationLimits()

	empty := `{"blocks":[],"limit":1000}` + "\n"
	if rec := f.do(t, "GET", "/v1/blocks", f.aliceTok, ""); rec.Code != 200 || rec.Body.String() != empty {
		t.Fatalf("fresh list = %d %q, want 200 %q", rec.Code, rec.Body, empty)
	}

	// Block a person and an agent; the first PUT and the repeat agree.
	rec := f.do(t, "PUT", blockPath(f.outsider.ID), f.aliceTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	var first schema.BlockV1
	if err := json.Unmarshal(rec.Body.Bytes(), &first); err != nil || first.Validate() != nil || first.PrincipalID != f.outsider.ID {
		t.Fatalf("PUT answered %s (%v)", rec.Body, err)
	}
	if again := f.do(t, "PUT", blockPath(f.outsider.ID), f.aliceTok, ""); again.Code != 200 || again.Body.String() != rec.Body.String() {
		t.Errorf("repeat PUT = %d %s, want the same entry %s", again.Code, again.Body, rec.Body)
	}
	if rec := f.do(t, "PUT", blockPath(f.bot.ID), f.aliceTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("block an agent = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "PUT", blockPath(f.op2.ID), f.aliceTok, ""); rec.Code != http.StatusOK {
		t.Fatalf("block an operator = %d %s", rec.Code, rec.Body)
	}
	// A disabled person is not listed in the directory: the same 404 as an
	// id never issued, byte for byte.
	ghost := f.do(t, "PUT", blockPath(987654), f.aliceTok, "")
	if rec := f.do(t, "PUT", blockPath(f.disabled.ID), f.aliceTok, ""); rec.Code != http.StatusNotFound || rec.Body.String() != ghost.Body.String() {
		t.Fatalf("block a disabled person = %d %s, want the ghost's %d %s", rec.Code, rec.Body, ghost.Code, ghost.Body)
	}
	list := decodeBlocks(t, f.do(t, "GET", "/v1/blocks", f.aliceTok, ""))
	// Oldest first; these three share a millisecond, so only the count and
	// the limit are asserted here (the store's test holds the order).
	if len(list.Blocks) != 3 || list.Limit != schema.MaxBlocks {
		t.Errorf("list = %+v", list)
	}

	refusals := []struct {
		name, method, path string
		status             int
		code               string
	}{
		{"yourself", "PUT", blockPath(f.alice.ID), 400, "invalid_request"},
		{"nobody", "PUT", blockPath(987654), 404, "principal_not_found"},
		{"a hidden person", "PUT", blockPath(f.hidden.ID), 404, "principal_not_found"},
		{"a zero id", "PUT", "/v1/blocks/0", 400, ""},
		{"not a number", "PUT", "/v1/blocks/marlow", 400, ""},
		{"delete not a number", "DELETE", "/v1/blocks/marlow", 400, ""},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, tt.method, tt.path, f.aliceTok, "")
			if tt.code == "" {
				if rec.Code != tt.status {
					t.Fatalf("= %d %s, want %d", rec.Code, rec.Body, tt.status)
				}
				return
			}
			assertErrorBody(t, rec, tt.status, tt.code)
		})
	}

	// DELETE is 204 whether or not there was a block, or anyone.
	for _, path := range []string{blockPath(f.outsider.ID), blockPath(f.outsider.ID), blockPath(987654), blockPath(f.alice.ID)} {
		if rec := f.do(t, "DELETE", path, f.aliceTok, ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Errorf("DELETE %s = %d %q, want 204 and no body", path, rec.Code, rec.Body)
		}
	}
	if got := decodeBlocks(t, f.do(t, "GET", "/v1/blocks", f.aliceTok, "")); len(got.Blocks) != 2 {
		t.Errorf("after delete the list holds %d, want 2", len(got.Blocks))
	}
}

// The list is its owner's alone: the person blocked, another member, an
// operator and the owner of the server all read their own (empty) list, and
// a DELETE of theirs touches nothing of the owner's.
func TestBlocksAreTheOwnersAlone(t *testing.T) {
	f := newDirectoryFixture(t)
	f.srv.relaxNotificationLimits()
	if rec := f.do(t, "PUT", blockPath(f.outsider.ID), f.aliceTok, ""); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	empty := `{"blocks":[],"limit":1000}` + "\n"
	for name, tok := range map[string]string{"the blocked person": f.outsiderTok, "another member": f.candTok, "an operator": f.op2Tok, "the owner": f.rootTok} {
		t.Run(name, func(t *testing.T) {
			if rec := f.do(t, "GET", "/v1/blocks", tok, ""); rec.Code != 200 || rec.Body.String() != empty {
				t.Errorf("GET = %d %q, want 200 %q", rec.Code, rec.Body, empty)
			}
			if rec := f.do(t, "DELETE", blockPath(f.outsider.ID), tok, ""); rec.Code != http.StatusNoContent {
				t.Errorf("DELETE = %d", rec.Code)
			}
		})
	}
	if got := decodeBlocks(t, f.do(t, "GET", "/v1/blocks", f.aliceTok, "")); len(got.Blocks) != 1 {
		t.Errorf("the owner's list holds %d after others' deletes, want 1", len(got.Blocks))
	}
}

func TestBlockRoutesAccess(t *testing.T) {
	f := newDirectoryFixture(t)
	f.srv.relaxNotificationLimits()
	routes := []struct{ method, path string }{
		{"GET", "/v1/blocks"},
		{"PUT", blockPath(f.outsider.ID)},
		{"DELETE", blockPath(f.outsider.ID)},
	}
	tests := []struct {
		who    string
		token  string
		status int
		code   string
	}{
		{"member", f.aliceTok, 0, ""},
		{"operator", f.op2Tok, 0, ""},
		{"owner", f.rootTok, 0, ""},
		{"agent", f.botTok, http.StatusForbidden, schema.ErrorCodeHumanRequired},
		{"guest", f.guestTok, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
		{"ended guest, token replayed", f.endedTok, http.StatusUnauthorized, "unauthenticated"},
		{"no credential", "", http.StatusUnauthorized, "unauthenticated"},
	}
	for _, tt := range tests {
		for _, r := range routes {
			t.Run(tt.who+"/"+r.method, func(t *testing.T) {
				rec := f.do(t, r.method, r.path, tt.token, "")
				if tt.code == "" {
					if rec.Code >= 300 {
						t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
					}
					return
				}
				assertErrorBody(t, rec, tt.status, tt.code)
			})
		}
	}
	t.Run("authentication off", func(t *testing.T) {
		off := &authFixture{srv: newTestServerWithConfig(t, Config{AuthMode: AuthOff})}
		for _, r := range routes {
			assertErrorBody(t, off.do(t, r.method, r.path, "", ""), http.StatusBadRequest, schema.ErrorCodeReadStateRequireAuth)
		}
	})
	t.Run("not set-up, previewable or administered", func(t *testing.T) {
		for _, pattern := range []string{"GET /v1/blocks", "PUT /v1/blocks/{principal_id}", "DELETE /v1/blocks/{principal_id}"} {
			found := false
			for _, rt := range f.srv.routes {
				if rt.pattern == pattern {
					found = true
					if rt.previewable {
						t.Errorf("%s is previewable", pattern)
					}
				}
			}
			if !found {
				t.Errorf("%s is not routed", pattern)
			}
			if setupScopeRoutes[pattern] || adminCatalogPatterns[pattern] {
				t.Errorf("%s is in the set-up scope or the admin catalog", pattern)
			}
		}
	})
}

// The list is full at schema.MaxBlocks: one more is 409, a repeat is not.
func TestBlockLimit(t *testing.T) {
	f := newDirectoryFixture(t)
	f.srv.relaxNotificationLimits()
	ctx := context.Background()
	now := time.Now()
	var last store.Principal
	for i := range schema.MaxBlocks {
		p, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, fmt.Sprintf("blk%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.srv.store.PutBlock(ctx, f.alice.ID, p.ID, true, now); err != nil {
			t.Fatal(err)
		}
		last = p
	}
	assertErrorBody(t, f.do(t, "PUT", blockPath(f.outsider.ID), f.aliceTok, ""), http.StatusConflict, schema.ErrorCodeBlockLimitReached)
	if rec := f.do(t, "PUT", blockPath(last.ID), f.aliceTok, ""); rec.Code != http.StatusOK {
		t.Errorf("repeat on a full list = %d %s, want 200", rec.Code, rec.Body)
	}
}

// The block routes have a write bucket of their own: blocking, 404s and
// repeats included, spends it and not the preferences', and spending the
// preferences' does not stop a block.
func TestBlockWriteBucket(t *testing.T) {
	f := newDirectoryFixture(t)
	relaxed := inviteLimit{name: "relaxed", burst: testRelaxedBurst, window: time.Hour}
	f.srv.setNotificationLimits(inviteLimit{name: "prefs", burst: 2, window: time.Hour}, relaxed)
	f.srv.setBlockWriteLimit(inviteLimit{name: "blocks", burst: 3, window: time.Hour})
	const prefs = `{"policy":"all","muted":[]}`

	// Two blocks' worth of writes (one a repeat, one a 404) and a delete.
	if rec := f.do(t, "PUT", blockPath(f.outsider.ID), f.aliceTok, ""); rec.Code != 200 {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "PUT", blockPath(f.outsider.ID), f.aliceTok, ""); rec.Code != 200 {
		t.Fatalf("repeat PUT = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "PUT", blockPath(987654), f.aliceTok, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("PUT of nobody = %d %s", rec.Code, rec.Body)
	}
	// The block bucket is empty; a 404 and a repeat counted.
	if rec := f.do(t, "DELETE", blockPath(f.outsider.ID), f.aliceTok, ""); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth block write = %d %s, want 429", rec.Code, rec.Body)
	}
	// The preferences' bucket is untouched: its two writes still go through.
	for i := range 2 {
		if rec := f.do(t, "PUT", "/v1/notifications/preferences", f.aliceTok, prefs); rec.Code != 200 {
			t.Fatalf("preferences write %d = %d %s, want 200", i+1, rec.Code, rec.Body)
		}
	}
	// And draining the preferences' bucket did not touch the blocks': a fresh
	// owner with a drained preferences bucket can still block.
	if rec := f.do(t, "PUT", "/v1/notifications/preferences", f.aliceTok, prefs); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third preferences write = %d, want 429", rec.Code)
	}
	f.srv.setBlockWriteLimit(inviteLimit{name: "blocks", burst: 3, window: time.Hour})
	if rec := f.do(t, "PUT", blockPath(f.outsider.ID), f.aliceTok, ""); rec.Code != 200 {
		t.Errorf("block with a drained preferences bucket = %d %s, want 200", rec.Code, rec.Body)
	}
}

// conch.blocks_changed.v1 is owed to the owner's human sockets and to no one
// else's, and only for a change that took effect.
func TestBlocksChangedSignal(t *testing.T) {
	f := newDirectoryFixture(t)
	f.srv.relaxNotificationLimits()
	subs := map[string]*eventsSub{
		"alice":       f.srv.events.subscribeKind(f.general.ID, f.alice.ID, 0, true),
		"alice-phone": f.srv.events.subscribeKind(f.general.ID, f.alice.ID, 0, true),
		"outsider":    f.srv.events.subscribeKind(f.general.ID, f.outsider.ID, 0, true),
		"op2":         f.srv.events.subscribeKind(f.general.ID, f.op2.ID, 0, true),
		"bot":         f.srv.events.subscribeKind(f.general.ID, f.bot.ID, 0, false),
	}
	owed := func() map[string]bool {
		out := map[string]bool{}
		for name, sub := range subs {
			if sub.takeBlocks() {
				out[name] = true
			}
		}
		return out
	}
	steps := []struct {
		name, method, path string
		want               map[string]bool
	}{
		{"block", "PUT", blockPath(f.outsider.ID), map[string]bool{"alice": true, "alice-phone": true}},
		{"repeat block", "PUT", blockPath(f.outsider.ID), map[string]bool{}},
		{"unblock", "DELETE", blockPath(f.outsider.ID), map[string]bool{"alice": true, "alice-phone": true}},
		{"repeat unblock", "DELETE", blockPath(f.outsider.ID), map[string]bool{}},
		{"a refused block", "PUT", blockPath(987654), map[string]bool{}},
	}
	for _, st := range steps {
		f.do(t, st.method, st.path, f.aliceTok, "")
		got := owed()
		if len(got) != len(st.want) {
			t.Errorf("%s: owed %v, want %v", st.name, got, st.want)
			continue
		}
		for k := range st.want {
			if !got[k] {
				t.Errorf("%s: owed %v, want %v", st.name, got, st.want)
			}
		}
	}
}

// The isolation row: everything the blocked person can read is byte for byte
// the same before and after they are blocked, and nothing is owed to their
// sockets. A block is the blocker's preference and nothing else.
func TestBlockedPersonSeesNoDifference(t *testing.T) {
	f := newDirectoryFixture(t)
	f.srv.relaxNotificationLimits()
	ctx := context.Background()
	// alice and the outsider share general; alice has posted there.
	if _, err := f.srv.store.PutChannelMember(ctx, "system", f.general.ID, f.outsider.ID, 0, nil); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "POST", "/v1/channels/"+f.general.Name+"/messages", f.aliceTok, `{"body":"hello @outsider"}`); rec.Code >= 300 {
		t.Fatalf("post = %d %s", rec.Code, rec.Body)
	}
	reads := []string{
		"/v1/blocks",
		"/v1/whoami",
		"/v1/channels",
		"/v1/read-state",
		"/v1/directory/members",
		"/v1/channels/" + f.general.Name + "/messages",
		"/v1/channels/" + f.general.Name + "/members",
		fmt.Sprintf("/v1/channels/%s/authors?ids=%d", f.general.Name, f.alice.ID),
		"/v1/notifications/pending",
	}
	snapshot := func() map[string]string {
		out := map[string]string{}
		for _, path := range reads {
			rec := f.do(t, "GET", path, f.outsiderTok, "")
			out[path] = fmt.Sprintf("%d %s", rec.Code, strings.TrimSpace(rec.Body.String()))
		}
		return out
	}
	before := snapshot()
	sub := f.srv.events.subscribeKind(f.general.ID, f.outsider.ID, 0, true)
	sub.take() // its first roster
	if rec := f.do(t, "PUT", blockPath(f.outsider.ID), f.aliceTok, ""); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	after := snapshot()
	for _, path := range reads {
		if before[path] != after[path] {
			t.Errorf("%s changed for the blocked person:\nbefore %s\nafter  %s", path, before[path], after[path])
		}
	}
	roster, readState, channels, refs, _ := sub.take()
	if roster || readState || channels || len(refs) != 0 || sub.takeBlocks() || sub.takeDirectory() || sub.takePermissions() {
		t.Error("the blocked person's socket was owed something by the block")
	}
	// Blocking them back is an ordinary block of their own, and blocking a
	// person who has blocked you answers exactly as any block does.
	rec := f.do(t, "PUT", blockPath(f.alice.ID), f.outsiderTok, "")
	var b schema.BlockV1
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &b) != nil || b.PrincipalID != f.alice.ID {
		t.Errorf("block back = %d %s", rec.Code, rec.Body)
	}
}
