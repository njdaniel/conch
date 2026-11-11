package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Hiding members from each other (issue #988, community-model §13, ADR-014):
// the member cell of see_member_directory off makes the may-know predicate
// bite. The world is the design's (§13.9): the owner (an operator) in private
// A and B; zed (the design's alice) in A only, A's manager; bob in B only;
// carol, who joined through zed's member link and is in no channel; an agent
// in A. Everyone else the directory fixture seeds is in general, which none
// of the four people share.

type hideFixture struct {
	*directoryFixture
	chA, chB, chC            store.Channel
	zed, bob, carol, dave    store.Principal
	zedTok, bobTok, carolTok string
	daveTok                  string
	agentA                   store.Principal
}

func newHideFixture(t *testing.T) *hideFixture {
	t.Helper()
	ctx := context.Background()
	f := &hideFixture{directoryFixture: newDirectoryFixture(t)}
	st := f.srv.store
	mk := func(kind store.PrincipalKind, name string) (store.Principal, string) {
		t.Helper()
		p, err := st.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := st.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		return p, tok
	}
	ch := func(name string, v schema.ChannelVisibility) store.Channel {
		t.Helper()
		c, err := st.CreateUnownedChannel(ctx, "system", name, v)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	f.chA, f.chB, f.chC = ch("hide-a", schema.ChannelPrivate), ch("hide-b", schema.ChannelPrivate), ch("hide-c", schema.ChannelPublic)
	f.zed, f.zedTok = mk(store.PrincipalHuman, "zed")
	f.bob, f.bobTok = mk(store.PrincipalHuman, "bob")
	f.carol, f.carolTok = mk(store.PrincipalHuman, "carol")
	f.dave, f.daveTok = mk(store.PrincipalHuman, "dave")
	f.agentA, _ = mk(store.PrincipalAgent, "agent-a")
	yes := true
	add := func(c store.Channel, p store.Principal, manages *bool) {
		t.Helper()
		if _, err := st.PutChannelMember(ctx, "system", c.ID, p.ID, 0, nil, manages); err != nil {
			t.Fatal(err)
		}
	}
	add(f.chA, f.root, nil)
	add(f.chB, f.root, nil)
	add(f.chA, f.zed, &yes)
	add(f.chA, f.dave, nil)
	add(f.chB, f.bob, nil)
	add(f.chA, f.agentA, nil)
	setAgentManifest(t, f.srv, f.agentA.ID, nil, f.chA.ID)
	legacyExec(t, f.srv, "UPDATE principals SET invited_by = ? WHERE id = ?", f.zed.ID, f.carol.ID)
	return f
}

// membersSee switches the member cell of see_member_directory.
func (f *hideFixture) membersSee(t *testing.T, on bool) {
	t.Helper()
	ctx := context.Background()
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, schema.PutPermissionsRequestV1{
		Revision: doc.Revision,
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: on}},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func listedIDs(page schema.ServerMembersResponseV1) []int64 {
	var out []int64
	for _, m := range page.Members {
		out = append(out, m.ID)
	}
	return out
}

// seeded keeps only the ids this fixture's people are among, so the general
// channel's cast does not enter the comparison.
func (f *hideFixture) among(ids []int64) []int64 {
	mine := []int64{f.root.ID, f.zed.ID, f.bob.ID, f.carol.ID, f.dave.ID, f.agentA.ID}
	var out []int64
	for _, id := range ids {
		if slices.Contains(mine, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func sorted(ids ...int64) []int64 {
	slices.Sort(ids)
	return ids
}

// TestHideMembersDirectory is the #964 example: the owner is in A and B, zed
// only in A, bob only in B. With the cell on, zed may know {owner, zed, dave,
// the agent, carol (O14b)}, bob {owner, bob}, the owner everyone; with it
// off, all of them see all.
func TestHideMembersDirectory(t *testing.T) {
	f := newHideFixture(t)
	everyone := sorted(f.root.ID, f.zed.ID, f.bob.ID, f.carol.ID, f.dave.ID, f.agentA.ID)
	tests := []struct {
		who   string
		token string
		off   []int64 // members hold the directory
		on    []int64 // members do not
		scope schema.DirectoryScope
	}{
		{"zed", f.zedTok, everyone, sorted(f.root.ID, f.zed.ID, f.dave.ID, f.agentA.ID, f.carol.ID), schema.DirectoryScopeSharedChannels},
		{"bob", f.bobTok, everyone, sorted(f.root.ID, f.bob.ID), schema.DirectoryScopeSharedChannels},
		{"carol", f.carolTok, everyone, sorted(f.carol.ID), schema.DirectoryScopeSharedChannels},
		{"the owner", f.rootTok, everyone, everyone, schema.DirectoryScopeEveryone},
		{"the second operator", f.op2Tok, everyone, everyone, schema.DirectoryScopeEveryone},
	}
	f.membersSee(t, false)
	for _, tt := range tests {
		t.Run(tt.who, func(t *testing.T) {
			rec, page := f.list(t, tt.token, "?limit=100")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; %s", rec.Code, rec.Body)
			}
			if got := f.among(listedIDs(page)); !slices.Equal(got, tt.on) {
				t.Errorf("with the cell off: %v, want %v", got, tt.on)
			}
			if page.Scope != tt.scope {
				t.Errorf("scope = %q, want %q", page.Scope, tt.scope)
			}
			// bob's name and id are markers in zed's register, and the
			// reverse.
			for _, pair := range [][2]string{{"zed", "bob"}, {"bob", "zed"}} {
				if tt.who == pair[0] && strings.Contains(rec.Body.String(), fmt.Sprintf("%q", pair[1])) {
					t.Errorf("%s's directory names %s: %s", pair[0], pair[1], rec.Body)
				}
			}
		})
	}

	// A toggle on and then off returns every viewer's directory to the full
	// list.
	f.membersSee(t, true)
	for _, tt := range tests {
		t.Run(tt.who+" after the toggle back", func(t *testing.T) {
			_, page := f.list(t, tt.token, "?limit=100")
			if got := f.among(listedIDs(page)); !slices.Equal(got, tt.off) {
				t.Errorf("with the cell on again: %v, want %v", got, tt.off)
			}
			if page.Scope != schema.DirectoryScopeEveryone {
				t.Errorf("scope = %q, want everyone", page.Scope)
			}
		})
	}

	// An agent is refused, with the same bytes either way.
	f.membersSee(t, false)
	rec := f.do(t, "GET", "/v1/directory/members", f.botTok, "")
	assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
}

// TestHideMembersPaging: with the cell off a page's size and cursor are what
// they would be where the unknown principals did not exist; every cursor is
// a listed id; bob's id is in none.
func TestHideMembersPaging(t *testing.T) {
	f := newHideFixture(t)
	f.membersSee(t, false)
	_, whole := f.list(t, f.zedTok, "?limit=100")
	want := listedIDs(whole)
	var got []int64
	after := int64(0)
	for pages := 0; ; pages++ {
		if pages > len(want) {
			t.Fatal("paging does not end")
		}
		q := "?limit=1"
		if after != 0 {
			q += fmt.Sprintf("&after=%d", after)
		}
		rec, page := f.list(t, f.zedTok, q)
		if rec.Code != http.StatusOK || len(page.Members) != 1 {
			t.Fatalf("page %d: %d %s", pages, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), fmt.Sprintf(`"after":%d`, f.bob.ID)) && f.bob.ID != page.Members[0].ID {
			t.Errorf("bob's id is a cursor: %s", rec.Body)
		}
		got = append(got, page.Members[0].ID)
		if page.After == 0 {
			break
		}
		if page.After != page.Members[0].ID {
			t.Errorf("after = %d, want the listed row %d", page.After, page.Members[0].ID)
		}
		after = page.After
	}
	if !slices.Equal(got, want) {
		t.Errorf("paged %v, want %v", got, want)
	}
	if slices.Contains(got, f.bob.ID) {
		t.Errorf("bob is paged to zed: %v", got)
	}
	// Starting after bob's own id is not an oracle: the page is the same as
	// after any other id in that gap.
	_, past := f.list(t, f.zedTok, fmt.Sprintf("?limit=100&after=%d", f.bob.ID))
	for _, id := range listedIDs(past) {
		if id <= f.bob.ID {
			t.Errorf("a page after %d lists %d", f.bob.ID, id)
		}
	}
}

// refusal is the bytes of one HTTP answer.
func refusal(rec *httptest.ResponseRecorder) string {
	return fmt.Sprintf("%d %s", rec.Code, rec.Body.String())
}

// TestHideMembersManagerAdd: a manager adds a member it may know; bob, a
// ghost id, a hidden principal, an ended guest and an id beyond the sequence
// answer one byte-identical 404, by id and by name; carol (O14b) is added.
// With the cell on, bob is added.
func TestHideMembersManagerAdd(t *testing.T) {
	ctx := context.Background()
	t.Run("with the cell on, bob is added", func(t *testing.T) {
		f := newHideFixture(t)
		rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/hide-a/members/%d", f.bob.ID), f.zedTok, "")
		if rec.Code != http.StatusNoContent {
			t.Fatalf("add bob = %d %s", rec.Code, rec.Body)
		}
		rec = f.do(t, "POST", "/v1/channels/hide-a/members", f.zedTok, `{"name":"carol"}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("add carol by name = %d %s", rec.Code, rec.Body)
		}
	})

	f := newHideFixture(t)
	f.membersSee(t, false)
	hidden, err := f.srv.store.PrincipalByID(ctx, f.hidden.ID)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := f.srv.store.PrincipalByID(ctx, f.endedGuest.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := func(id int64) string {
		return refusal(f.do(t, "PUT", fmt.Sprintf("/v1/channels/hide-a/members/%d", id), f.zedTok, ""))
	}
	byName := func(n string) string {
		return refusal(f.do(t, "POST", "/v1/channels/hide-a/members", f.zedTok, fmt.Sprintf(`{"name":%q}`, n)))
	}
	ghostID := byID(1 << 40)
	if !strings.HasPrefix(ghostID, "404 ") || !strings.Contains(ghostID, "principal_not_found") {
		t.Fatalf("a ghost id = %s, want the 404 principal_not_found", ghostID)
	}
	for what, id := range map[string]int64{"bob": f.bob.ID, "a hidden principal": hidden.ID, "an ended guest": ended.ID, "an unrelated member": f.outsider.ID} {
		if got := byID(id); got != ghostID {
			t.Errorf("add %s by id = %s, want the ghost's %s", what, got, ghostID)
		}
	}
	ghostName := byName("nobody-by-this-name")
	if !strings.HasPrefix(ghostName, "404 ") || !strings.Contains(ghostName, "principal_not_found") {
		t.Fatalf("a ghost name = %s", ghostName)
	}
	for what, n := range map[string]string{"bob": "bob", "a hidden principal": hidden.Name, "an ended guest": ended.Name, "an unrelated member": f.outsider.Name} {
		if got := byName(n); got != ghostName {
			t.Errorf("add %s by name = %s, want the ghost's %s", what, got, ghostName)
		}
	}
	if ghostID != ghostName {
		t.Errorf("by id and by name differ: %s vs %s", ghostID, ghostName)
	}
	// carol joined through zed's link: O14b lets zed add her.
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/hide-a/members/%d", f.carol.ID), f.zedTok, ""); rec.Code != http.StatusNoContent {
		t.Errorf("add carol by id = %s", refusal(rec))
	}
	// An operator is not bounded by the predicate.
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/hide-a/members/%d", f.bob.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Errorf("the owner adds bob = %s", refusal(rec))
	}
	// Bob is now in A, so zed may know him and add him by name.
	if rec := f.do(t, "POST", "/v1/channels/hide-a/members", f.zedTok, `{"name":"bob"}`); rec.Code != http.StatusOK {
		t.Errorf("add bob by name once he shares A = %s", refusal(rec))
	}
}

// TestHideMembersRemoveHasNoOracle: removing an id that is not a member of
// the channel answers the same whether or not the principal exists.
func TestHideMembersRemoveHasNoOracle(t *testing.T) {
	f := newHideFixture(t)
	for _, off := range []bool{false, true} {
		if off {
			f.membersSee(t, false)
		}
		exists := refusal(f.do(t, "DELETE", fmt.Sprintf("/v1/channels/hide-a/members/%d", f.bob.ID), f.zedTok, ""))
		ghost := refusal(f.do(t, "DELETE", fmt.Sprintf("/v1/channels/hide-a/members/%d", 1<<40), f.zedTok, ""))
		if exists != ghost || !strings.HasPrefix(exists, "204 ") {
			t.Errorf("cell off=%v: removing a non-member that exists = %s, one that does not = %s", off, exists, ghost)
		}
	}
}

// TestHideMembersJoiners: the maker of a link sees its joiners, and
// InviteV2.joiners counts them, whether or not a joiner shares a channel with
// the maker (O14b); anyone else gets a ghost's 404, as with the cell on.
func TestHideMembersJoiners(t *testing.T) {
	f := newInviteFixture(t, Config{})
	c := f.linkCallers(t)
	doc, err := f.srv.store.Permissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.PutPermissions(context.Background(), fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, schema.PutPermissionsRequestV1{
		Revision: doc.Revision,
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: false}},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	link := f.createMemberLink(t, c.member, schema.CreateInviteRequestV2{Kind: schema.InviteKindMember})
	for _, n := range []string{"wren", "moss"} {
		if rec := f.redeem(t, link.Code, n); rec.Code != http.StatusCreated {
			t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
		}
	}
	path := fmt.Sprintf("/v2/invites/%d/joiners", link.Invite.ID)
	rec := f.send(t, "GET", path, c.member, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the maker's joiners = %d %s", rec.Code, rec.Body)
	}
	if got := decodeBody[schema.InviteJoinersResponseV1](t, rec).Joiners; len(got) != 2 {
		t.Errorf("the maker sees %+v, want both joiners although none shares a channel with them", got)
	}
	assertErrorBody(t, f.send(t, "GET", path, c.quiet, "", ""), http.StatusNotFound, schema.ErrorCodeInviteNotFound)
	list := decodeBody[schema.ListInvitesResponseV2](t, f.send(t, "GET", "/v2/invites", c.member, "", ""))
	if len(list.Invites) != 1 || list.Invites[0].Joiners != 2 {
		t.Errorf("list = %+v, want joiners 2", list.Invites)
	}
	// And the maker's directory lists them (clause 5), and nobody else.
	_, page := (&directoryFixture{adminFixture: &adminFixture{authFixture: f.authFixture}}).list(t, c.member, "?limit=100")
	names := directoryNames(page)
	for _, n := range []string{"wren", "moss"} {
		if !slices.Contains(names, n) {
			t.Errorf("the maker's directory lacks their joiner %s: %v", n, names)
		}
	}
}

// authorFixture adds messages: dave posts to A and leaves; root posts; erin
// whispers to root only.
func (f *hideFixture) postedAuthors(t *testing.T) {
	t.Helper()
	f.postAs(t, f.daveTok, "hide-a", "dave was here")
	f.postAs(t, f.rootTok, "hide-a", "root was here")
	if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/hide-a/members/%d", f.dave.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove dave = %d %s", rec.Code, rec.Body)
	}
}

// TestChannelAuthors: a departed author is named; an author the caller may
// not read (a whisper to someone else) and one who never wrote, whether or
// not they exist, are absent; the answer does not depend on the cell; more
// than 100 ids is 400; a non-member gets the unknown channel's 404.
func TestChannelAuthors(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		t.Run(fmt.Sprintf("members hidden=%v", hidden), func(t *testing.T) {
			f := newHideFixture(t)
			f.postedAuthors(t)
			// A whisper from the owner to the agent alone: zed is not in
			// its audience.
			if rec := f.do(t, "POST", "/v2/channels/hide-a/messages", f.rootTok,
				`{"body":"psst",`+whisperAudience(f.agentA.ID)+`}`); rec.Code != http.StatusCreated {
				t.Fatalf("whisper = %d %s", rec.Code, rec.Body)
			}
			if hidden {
				f.membersSee(t, false)
			}
			ids := func(ids ...int64) string {
				parts := make([]string, len(ids))
				for i, id := range ids {
					parts[i] = fmt.Sprint(id)
				}
				return strings.Join(parts, ",")
			}
			names := func(rec *httptest.ResponseRecorder) map[int64]string {
				t.Helper()
				if rec.Code != http.StatusOK {
					t.Fatalf("authors = %d %s", rec.Code, rec.Body)
				}
				out := map[int64]string{}
				for _, a := range decodeBody[schema.ChannelAuthorsResponseV1](t, rec).Authors {
					out[a.PrincipalID] = a.Name
				}
				return out
			}
			got := names(f.do(t, "GET", "/v1/channels/hide-a/authors?ids="+ids(f.dave.ID, f.root.ID, f.bob.ID, 1<<40, f.agentA.ID), f.zedTok, ""))
			want := map[int64]string{f.dave.ID: "dave", f.root.ID: "root"}
			if len(got) != len(want) || got[f.dave.ID] != "dave" || got[f.root.ID] != "root" {
				t.Errorf("authors = %v, want %v (bob, the ghost and the agent absent)", got, want)
			}
			// The agent is in the audience of the whisper but it wrote
			// nothing; the owner wrote a whisper zed cannot read, yet also
			// an open message, so the owner is named anyway. A different
			// author of only a whisper must not be named:
			if rec := f.do(t, "POST", "/v2/channels/hide-a/messages", f.daveTok, `{"body":"x"}`); rec.Code == http.StatusCreated {
				t.Fatalf("dave posted after being removed: %s", rec.Body)
			}
			// More than the cap.
			many := make([]int64, schema.MaxChannelAuthorIDs+1)
			for i := range many {
				many[i] = int64(i + 1)
			}
			assertErrorBody(t, f.do(t, "GET", "/v1/channels/hide-a/authors?ids="+ids(many...), f.zedTok, ""), http.StatusBadRequest, "invalid_request")
			assertErrorBody(t, f.do(t, "GET", "/v1/channels/hide-a/authors", f.zedTok, ""), http.StatusBadRequest, "invalid_request")
			assertErrorBody(t, f.do(t, "GET", "/v1/channels/hide-a/authors?ids=x", f.zedTok, ""), http.StatusBadRequest, "invalid_request")
			// Exactly the cap is fine.
			if rec := f.do(t, "GET", "/v1/channels/hide-a/authors?ids="+ids(many[:schema.MaxChannelAuthorIDs]...), f.zedTok, ""); rec.Code != http.StatusOK {
				t.Errorf("exactly %d ids = %d %s", schema.MaxChannelAuthorIDs, rec.Code, rec.Body)
			}
			// A non-member is told the channel does not exist, with the
			// bytes of one that does not.
			ghost := f.do(t, "GET", "/v1/channels/no-such-channel/authors?ids=1", f.bobTok, "")
			nonMember := f.do(t, "GET", "/v1/channels/hide-a/authors?ids=1", f.bobTok, "")
			if ghost.Code != http.StatusNotFound || refusal(ghost) != refusal(nonMember) {
				t.Errorf("non-member = %s, ghost channel = %s", refusal(nonMember), refusal(ghost))
			}
		})
	}
}

// TestChannelAuthorsNamesAuthorsWhoCannotBeKnown: the route does not use the
// member predicate: an ended guest's and a disabled person's earlier messages
// name them, whatever the cell says (design §13.5). A later refactor that
// pulled the base set in would drop them.
func TestChannelAuthorsNamesAuthorsWhoCannotBeKnown(t *testing.T) {
	f := newHideFixture(t)
	ctx := context.Background()
	for _, p := range []store.Principal{f.endedGuest, f.disabled} {
		legacyExec(t, f.srv, "INSERT OR IGNORE INTO channel_members (channel_id, principal_id, created_at) VALUES (?, ?, 0)", f.chA.ID, p.ID)
		if _, err := f.srv.store.InsertMessage(ctx, f.chA.ID, p.ID, "earlier post by "+p.Name); err != nil {
			t.Fatal(err)
		}
	}
	for _, hidden := range []bool{false, true} {
		if hidden {
			f.membersSee(t, false)
		}
		rec := f.do(t, "GET", fmt.Sprintf("/v1/channels/hide-a/authors?ids=%d,%d", f.endedGuest.ID, f.disabled.ID), f.zedTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("authors = %d %s", rec.Code, rec.Body)
		}
		got := map[int64]string{}
		for _, a := range decodeBody[schema.ChannelAuthorsResponseV1](t, rec).Authors {
			got[a.PrincipalID] = a.Name
		}
		if got[f.endedGuest.ID] == "" || got[f.disabled.ID] == "" || len(got) != 2 {
			t.Errorf("cell hidden=%v: authors = %v, want the ended guest and the disabled person named", hidden, got)
		}
	}
}

// TestChannelAuthorsCapCountsDistinctIDs: duplicates do not count toward the
// cap of 100, as the store's cap is on distinct ids.
func TestChannelAuthorsCapCountsDistinctIDs(t *testing.T) {
	f := newHideFixture(t)
	var ids []string
	for i := 1; i <= schema.MaxChannelAuthorIDs; i++ {
		ids = append(ids, fmt.Sprint(i), fmt.Sprint(i))
	}
	if rec := f.do(t, "GET", "/v1/channels/hide-a/authors?ids="+strings.Join(ids, ","), f.zedTok, ""); rec.Code != http.StatusOK {
		t.Errorf("100 distinct ids, each twice = %d %s", rec.Code, rec.Body)
	}
	ids = append(ids, fmt.Sprint(schema.MaxChannelAuthorIDs+1))
	assertErrorBody(t, f.do(t, "GET", "/v1/channels/hide-a/authors?ids="+strings.Join(ids, ","), f.zedTok, ""), http.StatusBadRequest, "invalid_request")
}

// TestChannelAuthorsWhisperDoesNotCount: an author whose only message in the
// channel is a whisper the caller was not sent is absent for that caller and
// named for the recipient.
func TestChannelAuthorsWhisperDoesNotCount(t *testing.T) {
	f := newHideFixture(t)
	if _, err := f.srv.store.PutChannelMember(context.Background(), "system", f.chA.ID, f.carol.ID, 0, nil); err != nil {
		t.Fatal(err)
	}
	// carol writes one whisper, to the owner only.
	if rec := f.do(t, "POST", "/v2/channels/hide-a/messages", f.carolTok,
		`{"body":"psst",`+whisperAudience(f.root.ID)+`}`); rec.Code != http.StatusCreated {
		t.Fatalf("whisper = %d %s", rec.Code, rec.Body)
	}
	for _, tt := range []struct {
		who   string
		token string
		named bool
	}{
		{"the recipient", f.rootTok, true},
		{"the author", f.carolTok, true},
		{"someone else in the channel", f.zedTok, false},
	} {
		t.Run(tt.who, func(t *testing.T) {
			rec := f.do(t, "GET", fmt.Sprintf("/v1/channels/hide-a/authors?ids=%d", f.carol.ID), tt.token, "")
			got := decodeBody[schema.ChannelAuthorsResponseV1](t, rec)
			if named := len(got.Authors) == 1; named != tt.named {
				t.Errorf("authors = %+v, want named=%v", got.Authors, tt.named)
			}
		})
	}
}

// TestDefaultChannelExposesMembers: marking a channel default while members
// do not hold see_member_directory answers the warning; with it on there is
// none; and the permissions answer carries it while a default channel exists.
func TestDefaultChannelExposesMembers(t *testing.T) {
	f := newHideFixture(t)
	// The fixture's older channels may be defaults (a migrated server's open
	// channels are); the test starts with none.
	legacyExec(t, f.srv, "UPDATE channels SET is_default = 0, default_listed_by = NULL")
	mark := func() schema.ChannelSettingsResponseV1 {
		rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/%d/settings", f.chC.ID), f.rootTok, `{"default":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("mark default = %d %s", rec.Code, rec.Body)
		}
		return decodeBody[schema.ChannelSettingsResponseV1](t, rec)
	}
	unmark := func() {
		rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/%d/settings", f.chC.ID), f.rootTok, `{"default":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("unmark default = %d %s", rec.Code, rec.Body)
		}
	}
	warnings := func() []schema.PermissionsWarning {
		rec := f.do(t, "GET", "/v1/permissions", f.rootTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("permissions = %d %s", rec.Code, rec.Body)
		}
		return decodeBody[schema.PermissionsResponseV1](t, rec).Warnings
	}
	exposes := schema.PermissionsWarningDefaultChannelExposesMembers

	if got := mark().Warnings; len(got) != 0 {
		t.Errorf("with the directory on, marking default warned %v", got)
	}
	if slices.Contains(warnings(), exposes) {
		t.Errorf("with the directory on, the permissions answer warned %v", warnings())
	}
	unmark()
	f.membersSee(t, false)
	if slices.Contains(warnings(), exposes) {
		t.Errorf("with no default channel, the permissions answer warned %v", warnings())
	}
	if got := mark().Warnings; !slices.Equal(got, []schema.PermissionsWarning{exposes}) {
		t.Errorf("marking default with the directory off warned %v, want %v", got, exposes)
	}
	if !slices.Contains(warnings(), exposes) {
		t.Errorf("with a default channel and the directory off, the permissions answer warned %v", warnings())
	}
	// Warn, do not refuse: the channel is default.
	ch, err := f.srv.store.ChannelByName(context.Background(), "hide-c")
	if err != nil || !ch.IsDefault {
		t.Errorf("hide-c default = %v, %v", ch.IsDefault, err)
	}
}

// TestHideMembersRoleAtTheRequest: an operator sees everyone while an
// operator and only the people they may know once demoted: the role is read
// at the request, not when the token was made.
func TestHideMembersRoleAtTheRequest(t *testing.T) {
	f := newHideFixture(t)
	f.membersSee(t, false)
	if _, page := f.list(t, f.op2Tok, "?limit=100"); page.Scope != schema.DirectoryScopeEveryone || len(f.among(listedIDs(page))) != 6 {
		t.Fatalf("an operator's directory: scope %q, %v", page.Scope, f.among(listedIDs(page)))
	}
	if _, err := f.srv.store.SetRole(context.Background(), "system", f.root.ID, f.op2.ID, store.RoleMember, nil); err != nil {
		t.Fatal(err)
	}
	_, page := f.list(t, f.op2Tok, "?limit=100")
	if page.Scope != schema.DirectoryScopeSharedChannels || len(f.among(listedIDs(page))) != 0 {
		t.Errorf("a demoted operator's directory: scope %q, %v", page.Scope, f.among(listedIDs(page)))
	}
}

// TestHideMembersBaseSetWins: a disabled person, a hidden one and an ended
// guest in a channel zed is in are not listed to zed and are the ghost's 404
// to a manager adding them, whatever they share.
func TestHideMembersBaseSetWins(t *testing.T) {
	f := newHideFixture(t)
	f.membersSee(t, false)
	for _, id := range []int64{f.disabled.ID, f.hidden.ID, f.endedGuest.ID} {
		legacyExec(t, f.srv, "INSERT OR IGNORE INTO channel_members (channel_id, principal_id, created_at) VALUES (?, ?, 0)", f.chA.ID, id)
	}
	_, page := f.list(t, f.zedTok, "?limit=100")
	for _, id := range []int64{f.disabled.ID, f.hidden.ID, f.endedGuest.ID} {
		if slices.Contains(listedIDs(page), id) {
			t.Errorf("principal %d, in zed's channel, is listed to zed", id)
		}
		rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/hide-a/members/%d", id), f.zedTok, "")
		ghost := f.do(t, "PUT", fmt.Sprintf("/v1/channels/hide-a/members/%d", 1<<40), f.zedTok, "")
		if refusal(rec) != refusal(ghost) || rec.Code != http.StatusNotFound {
			t.Errorf("adding principal %d = %s, want the ghost's %s", id, refusal(rec), refusal(ghost))
		}
	}
}

// TestPendingNotificationsNameAuthorsByTheAuthorsRoute: a summary names its
// author only as GET /v1/channels/{channel}/authors would name that author to
// the reader, whether or not members hold see_member_directory (issue #1713,
// design §5.5); author_id is always sent. A whisper the reader was not sent is
// not in the feed at all, so its author is neither named nor counted.
func TestPendingNotificationsNameAuthorsByTheAuthorsRoute(t *testing.T) {
	for _, hidden := range []bool{false, true} {
		t.Run(fmt.Sprintf("members hidden=%v", hidden), func(t *testing.T) {
			f := newHideFixture(t)
			f.srv.relaxNotificationLimits()
			if _, err := f.srv.store.PutChannelMember(context.Background(), "system", f.chA.ID, f.carol.ID, 0, nil); err != nil {
				t.Fatal(err)
			}
			post := func(tok, body string) {
				t.Helper()
				if rec := f.do(t, "POST", "/v2/channels/hide-a/messages", tok, body); rec.Code != http.StatusCreated {
					t.Fatalf("post %s = %d %s", body, rec.Code, rec.Body)
				}
			}
			post(f.daveTok, `{"body":"from dave"}`)
			post(f.rootTok, `{"body":"from root"}`)
			// carol's only message to zed is a whisper to the owner: zed is
			// not in its audience.
			post(f.carolTok, `{"body":"psst",`+whisperAudience(f.root.ID)+`}`)
			if hidden {
				f.membersSee(t, false)
			}
			if rec := f.do(t, "PUT", "/v1/notifications/preferences", f.zedTok, `{"policy":"all","muted":[],"preview":true}`); rec.Code != http.StatusOK {
				t.Fatalf("PUT preferences = %d %s", rec.Code, rec.Body)
			}
			rec := f.do(t, "GET", "/v1/notifications/pending", f.zedTok, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("pending = %d %s", rec.Code, rec.Body)
			}
			page := decodeBody[schema.ListPendingNotificationsResponseV1](t, rec)
			if len(page.Summaries) != 2 {
				t.Fatalf("zed's feed = %+v, want dave's and root's posts only", page.Summaries)
			}
			for _, s := range page.Summaries {
				if s.AuthorID <= 0 {
					t.Errorf("message %d carries no author_id", s.MessageID)
				}
				route := decodeBody[schema.ChannelAuthorsResponseV1](t, f.do(t, "GET",
					fmt.Sprintf("/v1/channels/hide-a/authors?ids=%d", s.AuthorID), f.zedTok, ""))
				want := ""
				if len(route.Authors) == 1 {
					want = route.Authors[0].Name
				}
				if s.AuthorName != want {
					t.Errorf("message %d: author_name %q, the authors route names %q", s.MessageID, s.AuthorName, want)
				}
			}
			if strings.Contains(rec.Body.String(), "carol") || strings.Contains(rec.Body.String(), "psst") {
				t.Errorf("a whisper zed was not sent is in his feed: %s", rec.Body)
			}
		})
	}
}
