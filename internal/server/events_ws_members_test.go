package server

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// The member list on the events socket (issue #515): a reader holding the
// channel open learns that someone left, was hidden or was renamed, even when
// that someone had no session and so changed no roster.

func isMembers(match func(schema.ChannelMembersV1) bool) func(eventFrame) bool {
	return func(fr eventFrame) bool { return fr.schema == schema.ChannelMembersSchemaV1 && match(fr.members) }
}

func listsMember(d schema.ChannelMembersV1, principalID int64) (schema.ChannelMemberV1, bool) {
	for _, m := range d.Members {
		if m.PrincipalID == principalID {
			return m, true
		}
	}
	return schema.ChannelMemberV1{}, false
}

func membersLack(principalID int64) func(schema.ChannelMembersV1) bool {
	return func(d schema.ChannelMembersV1) bool {
		_, ok := listsMember(d, principalID)
		return !ok
	}
}

func membersName(principalID int64, name string) func(schema.ChannelMembersV1) bool {
	return func(d schema.ChannelMembersV1) bool {
		m, ok := listsMember(d, principalID)
		return ok && m.Name == name
	}
}

func countFrames(frames []eventFrame, kind string) int {
	n := 0
	for _, fr := range frames {
		if fr.schema == kind {
			n++
		}
	}
	return n
}

// TestEventsMemberListFollowsChanges: each change to general's member list
// reaches a reader as one members frame, the same list GET members answers
// that reader with (can_decide included only where REST shows it); a change
// that leaves the list as it was sends none, and a channel the changes did
// not touch hears nothing.
func TestEventsMemberListFollowsChanges(t *testing.T) {
	f := newRunFixture(t)
	base := wsTestServer(t, f.srv)
	readers := []struct {
		name  string
		token string
		conn  *eventsClient
	}{
		{name: "a member", token: f.aliceTok},
		{name: "an operator", token: f.rootTok},
	}
	for i := range readers {
		readers[i].conn = dialEvents(t, base, "/v1/events/ws?channel=general", readers[i].token)
		if first := readers[i].conn.next(); first.schema != schema.SessionRosterSchemaV1 {
			t.Fatalf("%s: first frame = %v, want the roster", readers[i].name, first)
		}
	}
	// alice is also in elsewhere, which none of the changes below touch.
	elsewhere := dialEvents(t, base, "/v1/events/ws?channel=elsewhere", f.aliceTok)
	if first := elsewhere.next(); first.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("elsewhere: first frame = %v, want the roster", first)
	}

	do := func(method, path, body string, want int) {
		t.Helper()
		if rec := f.do(t, method, path, f.rootTok, body); rec.Code != want {
			t.Fatalf("%s %s = %d %s", method, path, rec.Code, rec.Body)
		}
	}
	bobName := f.bob.Name
	steps := []struct {
		name  string
		act   func()
		match func(schema.ChannelMembersV1) bool
		check func(t *testing.T, seen []eventFrame)
	}{
		{
			// rival has no session: the roster does not change, and before
			// #515 nothing reached the reader at all.
			name: "a member with no session is removed",
			act: func() {
				do("DELETE", fmt.Sprintf("/v1/channels/general/members/%d", f.rival.ID), "", http.StatusNoContent)
			},
			match: membersLack(f.rival.ID),
			check: func(t *testing.T, seen []eventFrame) {
				if n := countFrames(seen, schema.SessionRosterSchemaV1); n != 0 {
					t.Errorf("%d roster frames for a member with no session: %v", n, seen)
				}
			},
		},
		{
			name: "a member is added",
			act: func() {
				do("PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.rival.ID), "", http.StatusNoContent)
			},
			match: func(d schema.ChannelMembersV1) bool {
				_, ok := listsMember(d, f.rival.ID)
				return ok
			},
		},
		{
			name:  "a member is renamed",
			act:   func() { do("PUT", renamePath(f.shy.ID), renameBody(t, "timid"), http.StatusOK) },
			match: membersName(f.shy.ID, "timid"),
		},
		{
			// Disabled members stay on the list (TestChannelMembers pins
			// it), so disabling mute sends no members frame: the next one is
			// the removal after it, and it still lists mute.
			name: "a member is disabled, then another removed",
			act: func() {
				do("POST", fmt.Sprintf("/v1/principals/%d/disable", f.mute.ID), "", http.StatusNoContent)
				do("DELETE", fmt.Sprintf("/v1/channels/general/members/%d", f.shy.ID), "", http.StatusNoContent)
			},
			match: membersLack(f.shy.ID),
			check: func(t *testing.T, seen []eventFrame) {
				if n := countFrames(seen, schema.ChannelMembersSchemaV1); n != 1 {
					t.Errorf("%d members frames, want only the removal's: %v", n, seen)
				}
				if _, ok := listsMember(seen[len(seen)-1].members, f.mute.ID); !ok {
					t.Errorf("the disabled member left the list: %v", seen[len(seen)-1])
				}
			},
		},
		{
			// Hiding takes bob out of every channel and renames him; no frame
			// after it may carry his old name.
			name:  "a member is hidden",
			act:   func() { do("POST", hidePath(f.bob.ID), "", http.StatusOK) },
			match: membersLack(f.bob.ID),
			check: func(t *testing.T, seen []eventFrame) {
				if last := seen[len(seen)-1]; strings.Contains(last.String(), bobName) {
					t.Errorf("the members frame still names %q: %v", bobName, last)
				}
			},
		},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			step.act()
			for _, r := range readers {
				seen := r.conn.until(step.name, isMembers(step.match))
				if step.check != nil {
					step.check(t, seen)
				}
				// The frame is what REST answers this reader now.
				rest := decodeBody[schema.ListChannelMembersResponseV1](t, f.do(t, "GET", "/v1/channels/general/members", r.token, ""))
				if got := seen[len(seen)-1].members; got.ChannelID != f.general.ID || !reflect.DeepEqual(got.Members, rest.Members) {
					t.Errorf("%s: members frame = %+v, REST = %+v", r.name, got, rest.Members)
				}
			}
		})
	}

	// elsewhere heard none of it: its next frame is the change made to it.
	do("PUT", fmt.Sprintf("/v1/channels/elsewhere/members/%d", f.carol.ID), "", http.StatusNoContent)
	if fr := elsewhere.next(); fr.schema != schema.ChannelMembersSchemaV1 || len(fr.members.Members) != 2 {
		t.Errorf("elsewhere: next frame = %v, want the list with carol added", fr)
	}
}

// TestEventsMemberListSeesOnlyWhatRESTShows: can_decide on the members frame
// follows the REST rule, an operator sees it for every person and a member
// only for themselves.
func TestEventsMemberListSeesOnlyWhatRESTShows(t *testing.T) {
	f := newRunFixture(t)
	base := wsTestServer(t, f.srv)
	alice := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
	root := dialEvents(t, base, "/v1/events/ws?channel=general", f.rootTok)
	for _, c := range []*eventsClient{alice, root} {
		c.next()
	}
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.carol.ID), f.rootTok, `{"can_decide":false}`); rec.Code != http.StatusNoContent {
		t.Fatalf("add carol = %d %s", rec.Code, rec.Body)
	}
	added := func(d schema.ChannelMembersV1) bool {
		_, ok := listsMember(d, f.carol.ID)
		return ok
	}
	tests := []struct {
		name   string
		conn   *eventsClient
		viewer int64
		all    bool
	}{
		{name: "a member", conn: alice, viewer: f.alice.ID},
		{name: "an operator", conn: root, viewer: f.root.ID, all: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := tt.conn.until("carol added", isMembers(added))
			for _, m := range seen[len(seen)-1].members.Members {
				human := m.Kind == schema.PrincipalHuman
				want := human && (tt.all || m.PrincipalID == tt.viewer)
				if got := m.CanDecide != nil; got != want {
					t.Errorf("principal %d (%s) carries can_decide = %v, want %v", m.PrincipalID, m.Kind, got, want)
				}
			}
		})
	}
}

// TestEventsMemberListBaselineIsReadBeforeTheFirstRoster: a member removed
// while the first roster is being written still reaches the reader as a
// members frame (issue #528). The client reads the list over REST once it has
// that roster, so it may have read it before the removal; the socket must not
// take the list after the removal as the one the reader already holds.
func TestEventsMemberListBaselineIsReadBeforeTheFirstRoster(t *testing.T) {
	f := newRunFixture(t)
	base := wsTestServer(t, f.srv)
	var once sync.Once
	f.srv.eventsBeforeWrite = func() {
		// The first write is the first roster: the removal commits while it
		// goes out, through the store, so nothing else owes the reader a
		// frame for it.
		once.Do(func() {
			if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.general.ID, f.rival.ID); err != nil {
				t.Error(err)
			}
		})
	}
	c := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
	if first := c.next(); first.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("first frame = %v, want the roster", first)
	}
	c.until("the removal made while the first roster was written", isMembers(membersLack(f.rival.ID)))
}
