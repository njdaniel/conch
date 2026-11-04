package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The directory signal (issue #988, community-model §13.6):
// conch.directory_changed.v1 goes only to the viewers whose directory answer
// changed. These tests read who is owed it from the events hub itself, so an
// absence needs no wait; TestDirectorySignalOnTheSocket reads real frames.

type signalViewers map[string]*eventsSub

// subscribe opens an events subscription, as a socket would, for each human
// and the agent.
func (f *hideFixture) subscribe(people map[string]store.Principal) signalViewers {
	out := signalViewers{}
	for name, p := range people {
		out[name] = f.srv.events.subscribeKind(f.chA.ID, p.ID, 0, p.Kind == store.PrincipalHuman)
	}
	return out
}

// owed reports, and clears, who is owed a directory frame.
func (v signalViewers) owed() []string {
	var out []string
	for name, sub := range v {
		if sub.takeDirectory() {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

func (f *hideFixture) setMembersSee(t *testing.T, on bool) {
	t.Helper()
	doc, err := f.srv.store.Permissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(schema.PutPermissionsRequestV1{
		Revision: doc.Revision,
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: on}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "PUT", "/v1/permissions", f.rootTok, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("put permissions = %d %s", rec.Code, rec.Body)
	}
}

// TestDirectorySignalAudience: with members unable to see the directory, a
// membership change reaches the subject, the operators and the members whose
// knowing of the subject changed, and nobody else; with them able, nobody; a
// toggle reaches every human; a change to a principal reaches every viewer
// who may know it.
func TestDirectorySignalAudience(t *testing.T) {
	f := newHideFixture(t)
	ctx := context.Background()
	erin, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "erin")
	if err != nil {
		t.Fatal(err)
	}
	_, erinTok, err := f.srv.store.CreateCredential(ctx, "system", erin.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	// zed is in the public channel C already, and bob is not.
	if _, err := f.srv.store.PutChannelMember(ctx, "system", f.chC.ID, f.zed.ID, 0, nil); err != nil {
		t.Fatal(err)
	}
	people := map[string]store.Principal{"root": f.root, "zed": f.zed, "bob": f.bob, "carol": f.carol, "erin": erin, "dave": f.dave, "agent": f.agentA}
	v := f.subscribe(people)
	v.owed() // a fresh subscription owes nothing; clear regardless

	do := func(method, path, tok, body string, want int) {
		t.Helper()
		if rec := f.do(t, method, path, tok, body); rec.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, path, rec.Code, want, rec.Body)
		}
	}
	erinPath := fmt.Sprintf("%d", erin.ID)

	// With the members holding the directory (the default), a membership
	// changes nobody's list.
	do("PUT", "/v1/channels/hide-b/members/"+erinPath, f.rootTok, "", http.StatusNoContent)
	if got := v.owed(); len(got) != 0 {
		t.Errorf("the directory is on, a membership was added: owed %v", got)
	}
	do("DELETE", "/v1/channels/hide-b/members/"+erinPath, f.rootTok, "", http.StatusNoContent)
	if got := v.owed(); len(got) != 0 {
		t.Errorf("the directory is on, a membership was removed: owed %v", got)
	}

	// The owner switches the directory off: every human, no agent.
	f.setMembersSee(t, false)
	if got, want := v.owed(), []string{"bob", "carol", "dave", "erin", "root", "zed"}; !slices.Equal(got, want) {
		t.Fatalf("the toggle owed %v, want %v", got, want)
	}

	steps := []struct {
		name string
		act  func()
		want []string
	}{
		{
			// The design's "bob joins B", with erin as the newcomer: zed in
			// A hears nothing; the owner and the members of B who did not
			// know erin do.
			name: "erin is added to B",
			act:  func() { do("PUT", "/v1/channels/hide-b/members/"+erinPath, f.rootTok, "", http.StatusNoContent) },
			want: []string{"bob", "erin", "root"},
		},
		{
			name: "erin is added to B again",
			act:  func() { do("PUT", "/v1/channels/hide-b/members/"+erinPath, f.rootTok, "", http.StatusNoContent) },
			want: nil,
		},
		{
			// "bob joins C, where alice is": zed hears, bob (in B) does not.
			name: "erin joins C, where zed is",
			act:  func() { do("POST", "/v1/channels/hide-c/join", erinTok, "", http.StatusCreated) },
			want: []string{"erin", "root", "zed"},
		},
		{
			name: "erin leaves C",
			act:  func() { do("POST", "/v1/channels/hide-c/leave", erinTok, "", http.StatusNoContent) },
			want: []string{"erin", "root", "zed"},
		},
		{
			// A's members who did not know erin do; bob, who shares B with
			// her and not A, is not told again.
			name: "erin is added to A, which zed manages",
			act:  func() { do("PUT", "/v1/channels/hide-a/members/"+erinPath, f.rootTok, "", http.StatusNoContent) },
			want: []string{"dave", "erin", "root", "zed"},
		},
		{
			name: "erin is renamed",
			act: func() {
				do("PUT", "/v1/principals/"+erinPath+"/name", f.rootTok, `{"name":"erin2"}`, http.StatusOK)
			},
			want: []string{"bob", "dave", "erin", "root", "zed"},
		},
		{
			name: "a principal is created",
			act: func() {
				do("POST", "/v0/principals", f.rootTok, `{"kind":"human","name":"newbie"}`, http.StatusCreated)
			},
			want: []string{"root"},
		},
	}
	for _, st := range steps {
		st.act()
		if got := v.owed(); !slices.Equal(got, st.want) {
			t.Errorf("%s: owed %v, want %v", st.name, got, st.want)
		}
	}

	// Back on: a rename now reaches every human, since every member may
	// know every principal; a membership reaches nobody.
	f.setMembersSee(t, true)
	v.owed()
	do("PUT", "/v1/principals/"+erinPath+"/name", f.rootTok, `{"name":"erin3"}`, http.StatusOK)
	if got, want := v.owed(), []string{"bob", "carol", "dave", "erin", "root", "zed"}; !slices.Equal(got, want) {
		t.Errorf("a rename with the directory on owed %v, want %v", got, want)
	}
	do("DELETE", "/v1/channels/hide-a/members/"+erinPath, f.rootTok, "", http.StatusNoContent)
	if got := v.owed(); len(got) != 0 {
		t.Errorf("a removal with the directory on owed %v", got)
	}
}

// TestDirectorySignalHideAndDisable: a hidden principal's departure reaches
// the viewers who could know it before its memberships went; a disable and
// an enable reach the viewers who may know it; a repeat reaches nobody.
func TestDirectorySignalHideAndDisable(t *testing.T) {
	f := newHideFixture(t)
	f.membersSee(t, false)
	v := f.subscribe(map[string]store.Principal{"root": f.root, "zed": f.zed, "bob": f.bob, "dave": f.dave, "carol": f.carol})
	v.owed()
	do := func(path string, want int) {
		t.Helper()
		if rec := f.do(t, "POST", path, f.rootTok, ""); rec.Code != want {
			t.Fatalf("POST %s = %d, want %d: %s", path, rec.Code, want, rec.Body)
		}
	}
	// dave shares A with zed. (A disabled person's own sockets are dropped, so
	// they are not owed anything.)
	do(fmt.Sprintf("/v1/principals/%d/disable", f.dave.ID), http.StatusNoContent)
	if got, want := v.owed(), []string{"root", "zed"}; !slices.Equal(got, want) {
		t.Errorf("disable owed %v, want %v", got, want)
	}
	do(fmt.Sprintf("/v1/principals/%d/disable", f.dave.ID), http.StatusNoContent)
	if got := v.owed(); len(got) != 0 {
		t.Errorf("a repeated disable owed %v", got)
	}
	do(fmt.Sprintf("/v1/principals/%d/enable", f.dave.ID), http.StatusNoContent)
	if got, want := v.owed(), []string{"root", "zed"}; !slices.Equal(got, want) {
		t.Errorf("enable owed %v, want %v", got, want)
	}
	// Hiding takes dave out of A: zed shared it with him before.
	do(fmt.Sprintf("/v1/principals/%d/hide", f.dave.ID), http.StatusOK)
	got := v.owed()
	if !slices.Contains(got, "zed") || !slices.Contains(got, "root") || slices.Contains(got, "bob") || slices.Contains(got, "carol") {
		t.Errorf("hide owed %v, want zed and root and neither bob nor carol", got)
	}
}

func isDirectoryChanged(fr eventFrame) bool { return fr.schema == schema.DirectoryChangedSchemaV1 }

// TestDirectorySignalOnTheSocket: the frame is delivered over a real events
// socket and nothing more than the schema; two toggles inside the roster
// floor are one frame.
func TestDirectorySignalOnTheSocket(t *testing.T) {
	f := newHideFixture(t)
	const floor = 40 * time.Millisecond
	f.srv.eventsRosterFloor = floor
	base := wsTestServer(t, f.srv)
	zed := dialEvents(t, base, "/v1/events/ws?channel=hide-a", f.zedTok)
	zed.next() // the roster

	f.setMembersSee(t, false)
	f.setMembersSee(t, true)
	time.Sleep(10 * floor)
	// A barrier that is not a directory frame: carol joins A, and the
	// member list says so. The directory is on again, so this owes no
	// directory frame.
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/hide-a/members/%d", f.carol.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("barrier = %d %s", rec.Code, rec.Body)
	}
	seen := zed.until("the members barrier", isMembers(func(d schema.ChannelMembersV1) bool {
		_, ok := listsMember(d, f.carol.ID)
		return ok
	}))
	n := 0
	for _, fr := range seen {
		if isDirectoryChanged(fr) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("two toggles inside the floor sent %d directory frames, want 1: %v", n, seen)
	}
}

// TestDirectorySignalHideReachesTheReadersOfItsMessages (issue #1282): a hide
// also tells the humans who hold a message of the hidden person that they may
// read, so their page asks the authors route again and names the author
// "hidden-<id>". Under shared_channels that is exactly the members for whom
// the authors route names the author: not a member whose only message of the
// author is a whisper they were not sent, nor one in no channel the author
// wrote in. Under everyone every human is told anyway.
func TestDirectorySignalHideReachesTheReadersOfItsMessages(t *testing.T) {
	tests := []struct {
		name        string
		membersSee  bool
		want        []string
		wantNotOwed []string
	}{
		{name: "shared_channels", membersSee: false, want: []string{"dave", "mia0", "root", "zed"}, wantNotOwed: []string{"carol", "mia", "agent"}},
		{name: "everyone", membersSee: true, want: []string{"carol", "dave", "mia", "mia0", "root", "zed"}, wantNotOwed: []string{"agent"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newHideFixture(t)
			ctx := context.Background()
			st := f.srv.store
			put := func(c store.Channel, p store.Principal) {
				t.Helper()
				if _, err := st.PutChannelMember(ctx, "system", c.ID, p.ID, 0, nil); err != nil {
					t.Fatal(err)
				}
			}
			mk := func(name string) store.Principal {
				t.Helper()
				p, err := st.CreatePrincipal(ctx, store.PrincipalHuman, name)
				if err != nil {
					t.Fatal(err)
				}
				return p
			}
			// mia is in the public channel C, where bob's only message is a
			// whisper to the owner; mia0 is in B, a channel bob still shares
			// when hidden (and so a knower).
			mia, mia0 := mk("mia"), mk("mia0")
			put(f.chC, mia)
			put(f.chC, f.root)
			put(f.chB, mia0)
			// bob once wrote in A (everyone there may read it) and left it.
			put(f.chA, f.bob)
			if _, err := st.InsertMessageV1(ctx, f.chA.ID, f.bob.ID, "hello A", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := st.RemoveChannelMember(ctx, "system", f.chA.ID, f.bob.ID); err != nil {
				t.Fatal(err)
			}
			put(f.chC, f.bob)
			if _, _, err := st.InsertScopedMessage(ctx, store.ScopedPost{
				ChannelID: f.chC.ID, AuthorID: f.bob.ID, Body: "psst",
				Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{f.root.ID}}.Normalize(f.bob.ID),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.RemoveChannelMember(ctx, "system", f.chC.ID, f.bob.ID); err != nil {
				t.Fatal(err)
			}
			f.membersSee(t, tc.membersSee)
			v := f.subscribe(map[string]store.Principal{"root": f.root, "zed": f.zed, "dave": f.dave, "carol": f.carol, "mia": mia, "mia0": mia0, "agent": f.agentA})
			v.owed()
			if rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/hide", f.bob.ID), f.rootTok, ""); rec.Code != http.StatusOK {
				t.Fatalf("hide = %d %s", rec.Code, rec.Body)
			}
			got := v.owed()
			if !slices.Equal(got, tc.want) {
				t.Errorf("hide owed %v, want %v (never %v)", got, tc.want, tc.wantNotOwed)
			}
		})
	}
}
