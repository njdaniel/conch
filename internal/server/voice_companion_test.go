package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of the companion and viewer participants as the poller sees them
// (issue #932, docs/design/voice-control-plane.md §13): p<id>.screen and
// p<id>.view are folded into the person, an orphaned companion is removed
// after the grace, entitlement loss removes all three identities, and a share
// is the same share whichever of the person's participants publishes it. All
// against the scripted LiveKit and the fixture's manual clock; nobody has been
// issued a companion token yet, the fake simply lists the identities.

// ---------------------------------------------------------------------------
// Helpers

// screenID and viewID are who's companion and viewer identities.
func (f *presenceFixture) screenID(who string) string { return f.identity(who) + ".screen" }
func (f *presenceFixture) viewID(who string) string   { return f.identity(who) + ".view" }

// participantOf returns who's participant in the snapshot of ops, or false.
func (f *presenceFixture) participantOf(t *testing.T, who string) (schema.VoiceParticipant, bool) {
	t.Helper()
	for _, room := range f.snap(t, f.ops).Rooms {
		for _, p := range room.Participants {
			if p.PrincipalID == f.ids[who] {
				return p, true
			}
		}
	}
	return schema.VoiceParticipant{}, false
}

// inVoice is the number of people the occupancy document counts in ops.
func (f *presenceFixture) inVoice(t *testing.T) int {
	t.Helper()
	_, doc := f.getOccupancy(t, "ann")
	for _, c := range doc.Channels {
		if c.ChannelID == f.ops.ID {
			return c.InVoice
		}
	}
	t.Fatalf("occupancy %+v has no row for ops", doc)
	return 0
}

// removedRows lists the voice_participant_removed details in order.
func (f *presenceFixture) removedRows(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range f.audits(t, store.AuditVoiceParticipantRemoved) {
		if want := fmt.Sprintf("channel:%d", f.ops.ID); e.Subject != want || e.Actor != "system" {
			t.Errorf("removal row has subject %q and actor %q, want %q and system", e.Subject, e.Actor, want)
		}
		out = append(out, e.Detail)
	}
	return out
}

// ---------------------------------------------------------------------------
// Folding

// TestVoiceCompanionFolded: a person and their suffixed participants are one
// person in presence and in occupancy; transmitting and the join time come from
// p<id> alone; a viewer alone is the person in the room; a companion alone is
// not.
func TestVoiceCompanionFolded(t *testing.T) {
	const annJoined, otherJoined = 1_700_000_001_000, 1_700_000_009_000
	tests := []struct {
		name string
		// room is what LiveKit lists, as a function of the fixture.
		room func(f *presenceFixture) []fakeParticipant
		// wantWho is presence ("-" for no room entry), wantInVoice the count.
		wantWho     string
		wantInVoice int
		// wantJoined is ann's join time in ms; 0 when ann is not shown.
		wantJoined int64
		wantAudit  func(f *presenceFixture) []string
	}{
		{
			name: "person and companion",
			room: func(f *presenceFixture) []fakeParticipant {
				return []fakeParticipant{
					{identity: f.identity("ann"), joinedMs: annJoined, published: true, muted: true},
					// An unmuted microphone on the companion is nobody speaking.
					{identity: f.screenID("ann"), joinedMs: otherJoined, published: true},
				}
			},
			wantWho: "ann", wantInVoice: 1, wantJoined: annJoined,
			wantAudit: func(f *presenceFixture) []string { return []string{"voice_joined " + f.actor("ann")} },
		},
		{
			name: "person transmitting and companion",
			room: func(f *presenceFixture) []fakeParticipant {
				return []fakeParticipant{
					{identity: f.screenID("ann"), joinedMs: otherJoined, published: true, muted: true},
					{identity: f.identity("ann"), joinedMs: annJoined, published: true},
				}
			},
			wantWho: "ann*", wantInVoice: 1, wantJoined: annJoined,
			wantAudit: func(f *presenceFixture) []string {
				// ann's own microphone, unreported: nobody else's
				return []string{"voice_joined " + f.actor("ann"), "voice_transmit_unreported " + f.actor("ann")}
			},
		},
		{
			name: "person and viewer",
			room: func(f *presenceFixture) []fakeParticipant {
				return []fakeParticipant{
					{identity: f.viewID("ann"), joinedMs: otherJoined},
					{identity: f.identity("ann"), joinedMs: annJoined, published: true, muted: true},
				}
			},
			wantWho: "ann", wantInVoice: 1, wantJoined: annJoined,
			wantAudit: func(f *presenceFixture) []string { return []string{"voice_joined " + f.actor("ann")} },
		},
		{
			name: "person, companion and viewer",
			room: func(f *presenceFixture) []fakeParticipant {
				return []fakeParticipant{
					{identity: f.identity("ann"), joinedMs: annJoined, published: true, muted: true},
					{identity: f.screenID("ann"), joinedMs: otherJoined},
					{identity: f.viewID("ann"), joinedMs: otherJoined},
				}
			},
			wantWho: "ann", wantInVoice: 1, wantJoined: annJoined,
			wantAudit: func(f *presenceFixture) []string { return []string{"voice_joined " + f.actor("ann")} },
		},
		{
			name: "viewer alone",
			room: func(f *presenceFixture) []fakeParticipant {
				return []fakeParticipant{{identity: f.viewID("ann"), joinedMs: otherJoined}}
			},
			wantWho: "ann", wantInVoice: 1, wantJoined: otherJoined,
			wantAudit: func(f *presenceFixture) []string { return []string{"voice_joined " + f.actor("ann")} },
		},
		{
			name: "viewer and companion without the person",
			room: func(f *presenceFixture) []fakeParticipant {
				return []fakeParticipant{
					{identity: f.screenID("ann"), joinedMs: annJoined},
					{identity: f.viewID("ann"), joinedMs: otherJoined},
				}
			},
			wantWho: "ann", wantInVoice: 1, wantJoined: otherJoined,
			wantAudit: func(f *presenceFixture) []string { return []string{"voice_joined " + f.actor("ann")} },
		},
		{
			name: "companion alone is not a person in the room",
			room: func(f *presenceFixture) []fakeParticipant {
				return []fakeParticipant{{identity: f.screenID("ann"), joinedMs: annJoined}}
			},
			wantWho: "", wantInVoice: 0,
			wantAudit: func(*presenceFixture) []string { return nil },
		},
		{
			name: "two people, one with a companion",
			room: func(f *presenceFixture) []fakeParticipant {
				return []fakeParticipant{
					{identity: f.identity("ann"), joinedMs: annJoined},
					{identity: f.screenID("ann"), joinedMs: otherJoined},
					{identity: f.identity("ann2"), joinedMs: otherJoined},
				}
			},
			wantWho: "ann,ann2", wantInVoice: 2, wantJoined: annJoined,
			wantAudit: func(f *presenceFixture) []string {
				return []string{"voice_joined " + f.actor("ann"), "voice_joined " + f.actor("ann2")}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			room := f.inUse(t, f.ops)
			f.lk.setRoom(room, tt.room(f)...)
			f.at(0)
			f.pass(t)
			f.pass(t) // and again: nothing new is written for the same room

			if got := f.who(t, f.ops); tt.wantWho == "" && got != "" || tt.wantWho != "" && got != tt.wantWho {
				t.Errorf("presence = %q, want %q", got, tt.wantWho)
			}
			if got := f.inVoice(t); got != tt.wantInVoice {
				t.Errorf("occupancy counts %d in voice, want %d", got, tt.wantInVoice)
			}
			if tt.wantJoined != 0 {
				p, ok := f.participantOf(t, "ann")
				if !ok {
					t.Fatal("ann is not in presence")
				}
				if !p.CanPublish || !p.JoinedAt.Time().Equal(time.UnixMilli(tt.wantJoined)) {
					t.Errorf("ann = %+v, want can_publish and joined at %d", p, tt.wantJoined)
				}
			}
			if got, want := f.voiceAudit(t), tt.wantAudit(f); !slices.Equal(got, want) {
				t.Errorf("audit = %q, want %q", got, want)
			}
			if n := f.lk.count("RemoveParticipant"); n != 0 {
				t.Errorf("%d removals within the grace", n)
			}
		})
	}
}

// TestVoiceCompanionPresenceChanges: the pieces of one person coming and going
// write rows for the person, never for a companion or a viewer beside them.
func TestVoiceCompanionPresenceChanges(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	a := f.actor("ann")
	ann := fakeParticipant{identity: f.identity("ann"), joinedMs: 5_000, published: true, muted: true}
	view := fakeParticipant{identity: f.viewID("ann"), joinedMs: 3_000}
	screen := fakeParticipant{identity: f.screenID("ann"), joinedMs: 4_000}

	steps := []struct {
		name  string
		parts []fakeParticipant
		who   string
		audit []string
	}{
		{"the viewer arrives first: the person joins", []fakeParticipant{view}, "ann", []string{"voice_joined " + a}},
		{"the person arrives beside the viewer: no row", []fakeParticipant{view, ann}, "ann", []string{"voice_joined " + a}},
		{"the companion arrives: no row", []fakeParticipant{view, ann, screen}, "ann", []string{"voice_joined " + a}},
		{"the person goes, viewer and companion stay: still in the room", []fakeParticipant{view, screen}, "ann", []string{"voice_joined " + a}},
		{"the person returns: no row", []fakeParticipant{view, ann, screen}, "ann", []string{"voice_joined " + a}},
		{"the viewer goes: no row", []fakeParticipant{ann, screen}, "ann", []string{"voice_joined " + a}},
		{"the companion goes: no row", []fakeParticipant{ann}, "ann", []string{"voice_joined " + a}},
		{"the viewer returns, the person goes: still in the room", []fakeParticipant{view}, "ann", []string{"voice_joined " + a}},
		{"the viewer goes: the person leaves", nil, "", []string{"voice_joined " + a, "voice_left " + a}},
	}
	for i, st := range steps {
		f.lk.setRoom(room, st.parts...)
		f.at((i + 1) * 100)
		f.pass(t)
		if got := f.who(t, f.ops); got != st.who {
			t.Fatalf("%s: presence = %q, want %q", st.name, got, st.who)
		}
		if got := f.voiceAudit(t); !slices.Equal(got, st.audit) {
			t.Fatalf("%s: audit = %q, want %q", st.name, got, st.audit)
		}
	}
	if n := f.lk.count("RemoveParticipant"); n != 0 {
		t.Errorf("%d removals", n)
	}
}

// ---------------------------------------------------------------------------
// Orphans

// TestVoiceCompanionOrphaned: a companion listed on every pass for the grace
// with no p<id> in the room is removed, once, with its own row; a shorter
// absence, a pass that does not list it, or the person's return, is not enough.
// A viewer is never removed for this.
func TestVoiceCompanionOrphaned(t *testing.T) {
	grace := int(voiceCompanionGrace / time.Millisecond)
	if grace != 5_500 {
		t.Fatalf("the grace is %d ms, want 5500 (voicePassGapMax + voiceEvictTimeout)", grace)
	}
	type step struct {
		at    int
		parts func(f *presenceFixture) []fakeParticipant
		// wantGone says the companion is not in LiveKit after the pass.
		wantGone bool
	}
	ann := func(f *presenceFixture) fakeParticipant {
		return fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000, published: true, muted: true}
	}
	screen := func(f *presenceFixture) fakeParticipant {
		return fakeParticipant{identity: f.screenID("ann"), joinedMs: 2_000}
	}
	only := func(f *presenceFixture) []fakeParticipant { return []fakeParticipant{screen(f)} }
	both := func(f *presenceFixture) []fakeParticipant { return []fakeParticipant{ann(f), screen(f)} }
	tests := []struct {
		name        string
		steps       []step
		wantRemoved int // voice_companion_removed rows
	}{
		{
			name:  "one pass short of the grace",
			steps: []step{{0, only, false}, {2_000, only, false}, {grace - 1, only, false}},
		},
		{
			name:        "the grace",
			steps:       []step{{0, only, false}, {2_000, only, false}, {grace, only, true}},
			wantRemoved: 1,
		},
		{
			name:        "past the grace, on a later pass",
			steps:       []step{{0, only, false}, {1_500, only, false}, {3_000, only, false}, {4_500, only, false}, {6_000, only, true}},
			wantRemoved: 1,
		},
		{
			name: "the person back in time starts the count again",
			steps: []step{
				{0, only, false}, {3_000, only, false}, {4_000, both, false},
				{5_000, only, false}, {5_000 + grace - 1, only, false}, {5_000 + grace, only, true},
			},
			wantRemoved: 1,
		},
		{
			name: "a pass that does not list the companion starts the count again",
			steps: []step{
				{0, only, false}, {3_000, only, false}, {4_000, func(*presenceFixture) []fakeParticipant { return nil }, false},
				{5_000, only, false}, {5_000 + grace - 1, only, false},
			},
		},
		{
			name: "a person with their companion is never an orphan",
			steps: []step{
				{0, both, false}, {grace, both, false}, {10 * grace, both, false},
			},
		},
		{
			name: "a viewer alone is never removed for want of the person",
			steps: []step{
				{0, func(f *presenceFixture) []fakeParticipant {
					return []fakeParticipant{{identity: f.viewID("ann"), joinedMs: 2_000}}
				}, false},
				{grace, func(f *presenceFixture) []fakeParticipant {
					return []fakeParticipant{{identity: f.viewID("ann"), joinedMs: 2_000}}
				}, false},
				{10 * grace, func(f *presenceFixture) []fakeParticipant {
					return []fakeParticipant{{identity: f.viewID("ann"), joinedMs: 2_000}}
				}, false},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			room := f.inUse(t, f.ops)
			for _, st := range tt.steps {
				f.lk.setRoom(room, st.parts(f)...)
				f.at(st.at)
				f.pass(t)
				if gone := !f.lk.in(room, f.screenID("ann")) && len(st.parts(f)) > 0 && slices.ContainsFunc(st.parts(f), func(p fakeParticipant) bool { return p.identity == f.screenID("ann") }); gone != st.wantGone {
					t.Fatalf("at %d ms: companion removed = %v, want %v", st.at, gone, st.wantGone)
				}
			}
			rows := f.audits(t, store.AuditVoiceCompanionRemoved)
			if len(rows) != tt.wantRemoved {
				t.Fatalf("voice_companion_removed rows = %+v, want %d", rows, tt.wantRemoved)
			}
			for _, e := range rows {
				want := fmt.Sprintf("channel=%d principal=%d participant=screen reason=orphaned", f.ops.ID, f.ids["ann"])
				if e.Actor != "system" || e.Subject != fmt.Sprintf("channel:%d", f.ops.ID) || e.Detail != want {
					t.Errorf("row = %+v, want actor system, subject channel:%d and detail %q", e, f.ops.ID, want)
				}
			}
			if got := f.lk.count("RemoveParticipant"); got != tt.wantRemoved {
				t.Errorf("RemoveParticipant calls = %d, want %d", got, tt.wantRemoved)
			}
		})
	}
}

// TestVoiceCompanionOrphanRemovalRetried: a removal LiveKit refuses is tried
// again on the next pass and audited once, when it works.
func TestVoiceCompanionOrphanRemovalRetried(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.screenID("ann"), joinedMs: 2_000})
	f.at(0)
	f.pass(t)
	f.lk.setRemoveStatus(http.StatusInternalServerError)
	f.at(int(voiceCompanionGrace / time.Millisecond))
	f.pass(t)
	f.at(int(voiceCompanionGrace/time.Millisecond) + 500)
	f.pass(t)
	if !f.lk.in(room, f.screenID("ann")) || len(f.audits(t, store.AuditVoiceCompanionRemoved)) != 0 {
		t.Fatal("a refused removal was taken for a removal")
	}
	if n := f.lk.count("RemoveParticipant"); n != 2 {
		t.Fatalf("RemoveParticipant calls = %d, want 2 (each pass retries)", n)
	}
	f.lk.setRemoveStatus(0)
	f.at(int(voiceCompanionGrace/time.Millisecond) + 1_000)
	f.pass(t)
	f.at(int(voiceCompanionGrace/time.Millisecond) + 1_500)
	f.pass(t)
	if f.lk.in(room, f.screenID("ann")) {
		t.Fatal("the companion is still in the room")
	}
	if n := len(f.audits(t, store.AuditVoiceCompanionRemoved)); n != 1 {
		t.Errorf("voice_companion_removed rows = %d, want 1", n)
	}
}

// ---------------------------------------------------------------------------
// Hostile identities

// TestVoiceCompanionHostileIdentities: an identity that is not exactly
// p<id>, p<id>.screen or p<id>.view is removed as bad_identity and shown
// nowhere, whoever it looks like.
func TestVoiceCompanionHostileIdentities(t *testing.T) {
	f0 := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	id := fmt.Sprint(f0.ids["ann"])
	for _, identity := range []string{
		"p" + id + ".screen.view", "p" + id + ".view.screen", "p" + id + ".screen.screen", "p" + id + ".view.view",
		"p0" + id + ".screen", "p0" + id + ".view", "p" + id + ".", "p" + id + "..screen",
		"p" + id + ".SCREEN", "p" + id + ".Screen", "p" + id + ".VIEW", "p-" + id + ".view",
		"p" + id + ".screen\x00", "p" + id + "\x00.view", "p" + id + ".screen ", " p" + id + ".screen",
		"p" + id + ".screen2", "p" + id + ".views", "screen.p" + id, "p.screen", ".view",
		"p" + strings.Repeat("9", 400) + ".screen", "p" + strings.Repeat("9", 400),
	} {
		t.Run(fmt.Sprintf("%q", identity[:min(len(identity), 40)]), func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			room := f.inUse(t, f.ops)
			f.lk.setRoom(room, fakeParticipant{identity: identity, joinedMs: 1_000, published: true})
			f.at(0)
			f.pass(t)
			if got := f.who(t, f.ops); got != "" {
				t.Errorf("presence = %q, want an empty room", got)
			}
			if f.lk.in(room, identity) {
				t.Error("the identity is still in the room")
			}
			want := fmt.Sprintf("channel=%d audience=channel principal=none reason=bad_identity source=observed", f.ops.ID)
			if got := f.removedRows(t); !slices.Equal(got, []string{want}) {
				t.Errorf("removal rows = %q, want %q", got, want)
			}
			if got := f.voiceAudit(t); len(got) != 1 {
				t.Errorf("audit = %q, want the removal alone", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Entitlement loss

// TestVoiceCompanionRemovedWithThePerson: a person who is not entitled loses
// all three identities, each removal its own row; the person's row is what it
// has always been and the suffixed ones say which participant.
func TestVoiceCompanionRemovedWithThePerson(t *testing.T) {
	tests := []struct {
		name   string
		who    string
		reason string
		setup  func(t *testing.T, f *presenceFixture)
	}{
		{name: "not a member", who: "bob", reason: voiceReasonNotMember},
		{name: "disabled", who: "dora", reason: voiceReasonDisabled},
		{name: "not human", who: "robo", reason: voiceReasonNotHuman},
		{name: "without voice", who: "vic", reason: voiceReasonVoiceOff, setup: func(t *testing.T, f *presenceFixture) { f.joinWithoutVoiceP(t) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			if tt.setup != nil {
				tt.setup(t, f)
			}
			room := f.inUse(t, f.ops)
			f.lk.setRoom(room,
				fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000, published: true, muted: true},
				fakeParticipant{identity: f.identity(tt.who), joinedMs: 2_000, published: true},
				fakeParticipant{identity: f.screenID(tt.who), joinedMs: 3_000},
				fakeParticipant{identity: f.viewID(tt.who), joinedMs: 4_000})
			f.at(0)
			f.pass(t)
			for _, identity := range []string{f.identity(tt.who), f.screenID(tt.who), f.viewID(tt.who)} {
				if f.lk.in(room, identity) {
					t.Errorf("%s is still in the room", identity)
				}
			}
			if got := f.who(t, f.ops); got != "ann" {
				t.Errorf("presence = %q, want ann alone", got)
			}
			pid := f.ids[tt.who]
			want := []string{
				// byte for byte what the person's row has always been
				fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s source=observed", f.ops.ID, pid, tt.reason),
				fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s participant=screen source=observed", f.ops.ID, pid, tt.reason),
				fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s participant=view source=observed", f.ops.ID, pid, tt.reason),
			}
			got := f.removedRows(t)
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("removal rows = %q, want %q", got, want)
			}
		})
	}
}

// TestVoiceCompanionRemovedByTheHooks: a member removed or a principal disabled
// loses all three identities before the request is answered, and presence is
// rid of them at once.
func TestVoiceCompanionRemovedByTheHooks(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   func(f *presenceFixture) string
		reason string
		// unlisted: the companion and viewer connected after the last pass,
		// so the poller has never listed them.
		unlisted bool
	}{
		{"member removed", http.MethodDelete, func(f *presenceFixture) string { return fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]) }, voiceReasonMemberRemoved, false},
		{"principal disabled", http.MethodPost, func(f *presenceFixture) string { return fmt.Sprintf("/v1/principals/%d/disable", f.ids["ann"]) }, voiceReasonPrincipalOff, false},
		{"member removed, companion and viewer unlisted", http.MethodDelete, func(f *presenceFixture) string { return fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]) }, voiceReasonMemberRemoved, true},
		{"principal disabled, companion and viewer unlisted", http.MethodPost, func(f *presenceFixture) string { return fmt.Sprintf("/v1/principals/%d/disable", f.ids["ann"]) }, voiceReasonPrincipalOff, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			room := f.inUse(t, f.ops)
			all := []fakeParticipant{
				{identity: f.identity("ann"), joinedMs: 1_000, published: true, muted: true},
				{identity: f.screenID("ann"), joinedMs: 3_000},
				{identity: f.viewID("ann"), joinedMs: 4_000},
				{identity: f.identity("ann2"), joinedMs: 5_000}}
			if tt.unlisted {
				f.lk.setRoom(room, all[0], all[3])
			} else {
				f.lk.setRoom(room, all...)
			}
			f.at(0)
			f.pass(t)
			f.lk.setRoom(room, all...)
			if got := f.who(t, f.ops); got != "ann,ann2" {
				t.Fatalf("before: %q", got)
			}
			base := len(f.lk.calls())
			if code := f.doOrdered(t, tt.method, tt.path(f), f.rootTok); code < 200 || code > 204 {
				t.Fatalf("%s = %d", tt.name, code)
			}
			respAt, removed := 0, map[string]bool{}
			for _, c := range f.lk.calls()[base:] {
				switch c.method {
				case "RemoveParticipant":
					if respAt != 0 {
						t.Errorf("RemoveParticipant %q came after the response", c.identity)
					}
					removed[c.identity] = true
				case "http-response":
					respAt = c.seq
				}
			}
			for _, identity := range []string{f.identity("ann"), f.screenID("ann"), f.viewID("ann")} {
				if !removed[identity] || f.lk.in(room, identity) {
					t.Errorf("%s: removed by the hook = %v, still connected = %v", identity, removed[identity], f.lk.in(room, identity))
				}
			}
			if removed[f.identity("ann2")] {
				t.Error("ann2 was removed too")
			}
			if got := f.who(t, f.ops); got != "ann2" {
				t.Errorf("presence right after = %q, want ann2 alone", got)
			}
			pid := f.ids["ann"]
			want := []string{
				fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s source=observed", f.ops.ID, pid, tt.reason),
				fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s participant=screen source=observed", f.ops.ID, pid, tt.reason),
				fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s participant=view source=observed", f.ops.ID, pid, tt.reason),
			}
			if got := f.removedRows(t); !slices.Equal(got, want) {
				t.Errorf("removal rows = %q, want %q", got, want)
			}
			// A pass afterwards finds nothing more to do.
			f.at(500)
			f.pass(t)
			if n := len(f.audits(t, store.AuditVoiceParticipantRemoved)); n != 3 {
				t.Errorf("removal rows after the next pass = %d, want 3", n)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The share

// shareWithCompanion is the share fixture with ann's companion connected and
// seen by a pass.
func shareWithCompanion(t *testing.T) (*presenceFixture, string) {
	t.Helper()
	f, room := newShareFixture(t)
	f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
		fakeParticipant{identity: f.screenID("ann"), joinedMs: 3_000})
	f.at(100)
	f.pass(t)
	return f, room
}

// publishScreen sets the screen-share tracks ann's companion publishes.
func (f *presenceFixture) publishScreen(room string, screen, audio bool) {
	f.lk.update(room, f.screenID("ann"), func(p *fakeParticipant) { p.screen, p.screenAudio = screen, audio })
}

// TestVoiceShareByCompanion: a share claimed by ann and published by her
// companion is her share: published (no idle end at 15 s), with sound when the
// companion has it, ended idle 5 s after the companion's tracks go and left
// when ann herself is gone.
func TestVoiceShareByCompanion(t *testing.T) {
	a := f2name("ann")
	tests := []struct {
		name string
		run  func(t *testing.T, f *presenceFixture, room string)
		// wantSharing is presence after run; wantRows the share rows.
		wantSharing string
		wantRows    []string
	}{
		{
			name: "published by the companion, no idle end",
			run: func(t *testing.T, f *presenceFixture, room string) {
				f.publishScreen(room, true, false)
				f.at(5_000)
				f.pass(t)
				f.at(40_000)
				f.pass(t)
			},
			wantSharing: "ann",
			wantRows:    []string{a + " started audio=true"},
		},
		{
			name: "its sound follows the companion's audio track",
			run: func(t *testing.T, f *presenceFixture, room string) {
				f.publishScreen(room, true, true)
				f.at(5_000)
				f.pass(t)
				if got := f.sharing(t, f.ops); got != "ann+audio" {
					t.Fatalf("with the companion's audio track presence shows %q, want ann+audio", got)
				}
				f.publishScreen(room, true, false)
				f.at(6_000)
				f.pass(t)
			},
			wantSharing: "ann",
			wantRows:    []string{a + " started audio=true"},
		},
		{
			name: "the companion's tracks go: idle 5 s after the last pass that saw one",
			run: func(t *testing.T, f *presenceFixture, room string) {
				f.publishScreen(room, true, false)
				f.at(5_000)
				f.pass(t)
				f.publishScreen(room, false, false)
				f.at(9_999)
				f.pass(t)
				if got := f.sharing(t, f.ops); got != "ann" {
					t.Fatalf("4.999 s after the last track presence shows %q, want ann still sharing", got)
				}
				f.at(10_000)
				f.pass(t)
			},
			wantSharing: "-",
			wantRows:    []string{a + " started audio=true", a + " stopped principal=" + a + " reason=idle"},
		},
		{
			name: "nothing published by anyone: idle after 15 s as ever",
			run: func(t *testing.T, f *presenceFixture, _ string) {
				f.at(16_100)
				f.pass(t)
			},
			wantSharing: "-",
			wantRows:    []string{a + " started audio=true", a + " stopped principal=" + a + " reason=idle"},
		},
		{
			name: "ann gone, the companion still there: left",
			run: func(t *testing.T, f *presenceFixture, room string) {
				f.publishScreen(room, true, false)
				f.at(5_000)
				f.pass(t)
				f.lk.drop(room, f.identity("ann"))
				f.at(5_500)
				f.pass(t)
			},
			wantSharing: "-",
			wantRows:    []string{a + " started audio=true", a + " stopped principal=" + a + " reason=left"},
		},
		{
			name: "ann gone, only a viewer left of her: left",
			run: func(t *testing.T, f *presenceFixture, room string) {
				f.publishScreen(room, true, false)
				f.at(5_000)
				f.pass(t)
				f.lk.drop(room, f.identity("ann"))
				f.lk.drop(room, f.screenID("ann"))
				f.lk.update(room, f.identity("ann2"), func(*fakeParticipant) {})
				f.lk.mu.Lock()
				f.lk.rooms[room] = append(f.lk.rooms[room], fakeParticipant{identity: f.viewID("ann"), joinedMs: 9_000})
				f.lk.mu.Unlock()
				f.at(5_500)
				f.pass(t)
				if got := f.who(t, f.ops); got != "ann,ann2" {
					t.Fatalf("presence = %q, want the viewer standing for ann", got)
				}
			},
			wantSharing: "-",
			wantRows:    []string{a + " started audio=true", a + " stopped principal=" + a + " reason=left"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := shareWithCompanion(t)
			f.at(1_000)
			f.mustClaim(t, "ann")
			tt.run(t, f, room)
			if got := f.sharing(t, f.ops); got != tt.wantSharing {
				t.Errorf("presence shows %q sharing, want %q", got, tt.wantSharing)
			}
			if got := f.shareRows(t); !slices.Equal(got, tt.wantRows) {
				t.Errorf("share rows %q, want %q", got, tt.wantRows)
			}
			// Nothing was ever withdrawn from her companion while she held the
			// share, and nothing is audited as unclaimed.
			if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
				t.Errorf("unclaimed rows = %d, want 0", n)
			}
		})
	}
}

// f2name is the name shareRows gives a principal.
func f2name(who string) string { return who }

// TestVoiceShareUnclaimedCompanion: a screen published by a companion whose
// person holds no claim is audited as unclaimed against the person, and the
// companion (not the person) is withdrawn to publishing nothing; when LiveKit
// will not do that, the companion is removed.
func TestVoiceShareUnclaimedCompanion(t *testing.T) {
	t.Run("withdrawn", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.publishScreen(room, true, true)
		f.lk.update(room, f.screenID("ann"), func(p *fakeParticipant) { p.shareGranted = true })
		f.at(500)
		f.pass(t)
		ups := f.lk.updatesFor(f.screenID("ann"))
		if len(ups) != 1 || ups[0].canPublish || ups[0].sources == nil || len(ups[0].sources) != 0 {
			t.Fatalf("companion updates = %+v, want one: canPublish false, sources []", ups)
		}
		if got := f.lk.updatesFor(f.identity("ann")); len(got) != 0 {
			t.Errorf("ann's own participant was updated: %+v", got)
		}
		if p, _ := f.lk.participant(room, f.screenID("ann")); p.screen || p.screenAudio || p.shareGranted {
			t.Errorf("the companion still has %+v", p)
		}
		if !f.lk.in(room, f.screenID("ann")) || !f.lk.in(room, f.identity("ann")) {
			t.Error("withdrawing removed somebody")
		}
		rows := f.audits(t, store.AuditVoiceShareUnclaimed)
		wantDetail := fmt.Sprintf("channel=%d sources=screen_share,screen_share_audio participant=screen source=observed", f.ops.ID)
		if len(rows) != 1 || rows[0].Actor != f.actor("ann") || rows[0].Detail != wantDetail {
			t.Fatalf("unclaimed rows = %+v, want one by ann with detail %q", rows, wantDetail)
		}
		// Once, not on every pass.
		f.at(1_000)
		f.pass(t)
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 1 {
			t.Errorf("unclaimed rows after another pass = %d, want 1", n)
		}
		if got := f.sharing(t, f.ops); got != "-" {
			t.Errorf("presence shows %q sharing", got)
		}
	})
	t.Run("another person's share is not the companion's", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.mustClaim(t, "ann2")
		f.publishScreen(room, true, false)
		f.at(500)
		f.pass(t)
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 1 {
			t.Errorf("unclaimed rows = %d, want 1: ann's companion publishes ann2's share", n)
		}
		if got := f.lk.updatesFor(f.screenID("ann")); len(got) == 0 || got[len(got)-1].canPublish {
			t.Errorf("companion updates = %+v, want it withdrawn", got)
		}
	})
	t.Run("not withdrawn: removed", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.publishScreen(room, true, false)
		f.lk.setUpdateStatus(http.StatusInternalServerError)
		f.at(500)
		f.pass(t)
		if f.lk.in(room, f.screenID("ann")) {
			t.Fatal("the companion is still connected with an unclaimed share")
		}
		if !f.lk.in(room, f.identity("ann")) {
			t.Error("ann herself was removed")
		}
		want := fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s participant=screen source=observed", f.ops.ID, f.ids["ann"], voiceReasonShareUnclaimed)
		if got := f.removedRows(t); !slices.Equal(got, []string{want}) {
			t.Errorf("removal rows = %q, want %q", got, want)
		}
		if got := f.who(t, f.ops); got != "ann,ann2" {
			t.Errorf("presence = %q: the person stays", got)
		}
	})
	t.Run("a share stopped is withdrawn from the companion", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.at(1_000)
		f.mustClaim(t, "ann")
		f.publishScreen(room, true, false)
		f.at(2_000)
		f.pass(t)
		f.mustStop(t, "ann")
		f.at(2_500)
		f.pass(t)
		p, _ := f.lk.participant(room, f.screenID("ann"))
		if p.screen || p.shareGranted {
			t.Errorf("after the stop the companion has %+v", p)
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("a stopped share was audited as unclaimed %d times", n)
		}
	})
}

// TestVoiceShareClaimRestoresCompanion: a claim grants the person's own
// connection first, then lets a companion in the room publish the screen and
// nothing else; a failure of the second does not undo the claim.
func TestVoiceShareClaimRestoresCompanion(t *testing.T) {
	t.Run("a companion in the room", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.mustClaim(t, "ann")
		if ups := f.lk.updatesFor(f.identity("ann")); len(ups) != 1 || !ups[0].canPublish || !slices.Equal(ups[0].sources, []string{"MICROPHONE", "SCREEN_SHARE", "SCREEN_SHARE_AUDIO"}) {
			t.Errorf("ann's updates = %+v, want the bundled grant", ups)
		}
		ups := f.lk.updatesFor(f.screenID("ann"))
		if len(ups) != 1 || !ups[0].canPublish || !slices.Equal(ups[0].sources, []string{"SCREEN_SHARE"}) {
			t.Fatalf("companion updates = %+v, want canPublish with [SCREEN_SHARE]", ups)
		}
		var order []string
		for _, c := range f.lk.calls() {
			if c.method == "UpdateParticipant" {
				order = append(order, c.identity)
			}
		}
		if want := []string{f.identity("ann"), f.screenID("ann")}; !slices.Equal(order, want) {
			t.Errorf("update order = %q, want %q", order, want)
		}
		if !f.granted(t, room, "ann") {
			t.Error("ann was not granted")
		}
		if p, _ := f.lk.participant(room, f.screenID("ann")); !p.shareGranted {
			t.Error("the companion was not granted the screen")
		}
		// Claiming again restores a companion that came back without its grant.
		f.lk.update(room, f.screenID("ann"), func(p *fakeParticipant) { p.shareGranted = false })
		f.mustClaim(t, "ann")
		if p, _ := f.lk.participant(room, f.screenID("ann")); !p.shareGranted {
			t.Error("the second claim did not restore the companion")
		}
	})
	t.Run("no companion: no call", func(t *testing.T) {
		f, _ := newShareFixture(t)
		f.mustClaim(t, "ann")
		if n := f.lk.count("UpdateParticipant"); n != 1 {
			t.Errorf("UpdateParticipant calls = %d, want 1", n)
		}
	})
	t.Run("the companion's grant fails: the claim stands", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		token := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Token
		f.lk.setUpdateFail(f.screenID("ann"), true)
		f.mustClaim(t, "ann")
		if got := f.shareState(room); got != f.ids["ann"] {
			t.Fatalf("the share is held by %d, want ann", got)
		}
		if got := f.sharing(t, f.ops); got != "ann" {
			t.Errorf("presence shows %q sharing, want ann", got)
		}
		if n := len(f.audits(t, store.AuditVoiceShareStarted)); n != 1 {
			t.Errorf("share_started rows = %d, want 1", n)
		}
		logs := f.logs.buf.String()
		if !strings.Contains(logs, "granting the screen to a companion failed") {
			t.Error("the failure was not logged")
		}
		for what, secret := range map[string]string{"join token": token, "room name": room, "api secret": voiceTestSecret} {
			if strings.Contains(logs, secret) {
				t.Errorf("%s appears in the log", what)
			}
		}
		// And it is restored by the next claim.
		f.lk.setUpdateFail(f.screenID("ann"), false)
		f.mustClaim(t, "ann")
		if p, _ := f.lk.participant(room, f.screenID("ann")); !p.shareGranted {
			t.Error("the next claim did not restore the companion")
		}
	})
	t.Run("ann not connected: the companion is not touched", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.lk.drop(room, f.identity("ann"))
		if res := f.claimShare(t, "ann", "ops"); res.status != http.StatusConflict {
			t.Fatalf("claim = %d %s, want 409", res.status, res.body)
		}
		if got := f.lk.updatesFor(f.screenID("ann")); len(got) != 0 {
			t.Errorf("the companion was updated for a claim that failed: %+v", got)
		}
	})
}

// TestVoiceCompanionKeepsSecrets: nothing the new paths log or audit carries a
// token, the API secret or a room name.
func TestVoiceCompanionKeepsSecrets(t *testing.T) {
	f, room := shareWithCompanion(t)
	token := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Token
	var bodies []string
	keep := func(res wireResult) { bodies = append(bodies, res.body) }
	// An unclaimed screen, withdrawn.
	f.publishScreen(room, true, true)
	f.at(500)
	f.pass(t)
	// A claim whose companion grant fails, then a stop.
	f.lk.setUpdateFail(f.screenID("ann"), true)
	keep(f.claimShare(t, "ann", "ops"))
	f.lk.setUpdateFail(f.screenID("ann"), false)
	keep(f.stopShare(t, "ann", "ops"))
	f.at(1_000)
	f.pass(t)
	// A companion that cannot be withdrawn is removed.
	f.lk.setUpdateStatus(http.StatusInternalServerError)
	f.publishScreen(room, true, false)
	f.at(1_500)
	f.pass(t)
	f.lk.setUpdateStatus(0)
	// A companion whose person is gone: the removal fails, then works.
	f.lk.mu.Lock()
	f.lk.rooms[room] = append(f.lk.rooms[room], fakeParticipant{identity: f.screenID("ann"), joinedMs: 9_000})
	f.lk.mu.Unlock()
	f.lk.drop(room, f.identity("ann"))
	grace := int(voiceCompanionGrace / time.Millisecond)
	f.at(2_000)
	f.pass(t)
	f.lk.setRemoveStatus(http.StatusInternalServerError)
	f.at(2_000 + grace)
	f.pass(t)
	f.lk.setRemoveStatus(0)
	f.at(3_000 + grace)
	f.pass(t)

	var haystack strings.Builder
	for _, e := range f.audit(t) {
		fmt.Fprintf(&haystack, "%s|%s|%s|%s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	haystack.WriteString(f.logs.buf.String())
	for _, b := range bodies {
		haystack.WriteString(b)
	}
	text := haystack.String()
	for what, secret := range map[string]string{
		"join token": token, "join token body": strings.Split(token, ".")[1],
		"api secret": voiceTestSecret, "api key": voiceTestKey, "room name": room,
		"ann's login token": f.tokens["ann"],
	} {
		if strings.Contains(text, secret) {
			t.Errorf("%s appears in an audit row, log line or response", what)
		}
	}
	for _, exercised := range []string{
		store.AuditVoiceShareUnclaimed, store.AuditVoiceCompanionRemoved, "participant=screen",
		"granting the screen to a companion failed", "removing an orphaned companion failed",
	} {
		if !strings.Contains(text, exercised) {
			t.Errorf("the run did not exercise %q", exercised)
		}
	}
}

// TestVoiceCompanionHookWithNoCompanion: the hooks ask LiveKit to remove a
// person's companion and viewer in every room, and LiveKit's not_found
// (nobody by that identity) writes no row: a person with neither gets the one
// row they always got.
func TestVoiceCompanionHookWithNoCompanion(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
	f.at(0)
	f.pass(t)
	base := len(f.lk.calls())
	if code := f.doOrdered(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok); code != 204 {
		t.Fatalf("removal = %d", code)
	}
	asked := map[string]bool{}
	for _, c := range f.lk.calls()[base:] {
		if c.method == "RemoveParticipant" {
			asked[c.identity] = true
		}
	}
	for _, identity := range []string{f.identity("ann"), f.screenID("ann"), f.viewID("ann")} {
		if !asked[identity] {
			t.Errorf("LiveKit was not asked to remove %s", identity)
		}
	}
	want := fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s source=observed", f.ops.ID, f.ids["ann"], voiceReasonMemberRemoved)
	if got := f.removedRows(t); !slices.Equal(got, []string{want}) {
		t.Errorf("removal rows = %q, want the person's alone", got)
	}
}

// ---------------------------------------------------------------------------
// A companion that may not subscribe

// TestVoiceCompanionKeepsItsSubscribe: LiveKit replaces a permission whole, so
// a grant or a withdrawal must carry the companion's own canSubscribe. A
// publish-only companion (B1) is never handed the right to receive everyone's
// voice by a claim, a stop, an unclaimed screen or a rollback; a receiving
// one (B2) keeps it.
func TestVoiceCompanionKeepsItsSubscribe(t *testing.T) {
	for _, subscribes := range []bool{false, true} {
		name := map[bool]string{false: "publish-only", true: "subscribing"}[subscribes]
		setup := func(t *testing.T) (*presenceFixture, string) {
			t.Helper()
			f, room := newShareFixture(t)
			f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
				fakeParticipant{identity: f.screenID("ann"), joinedMs: 3_000, noSubscribe: !subscribes})
			f.at(100)
			f.pass(t)
			return f, room
		}
		check := func(t *testing.T, f *presenceFixture, room, when string) {
			t.Helper()
			for _, u := range f.lk.updatesFor(f.screenID("ann")) {
				if u.canSubscribe != subscribes {
					t.Errorf("%s: an update for the companion set canSubscribe %v, want %v: %+v", when, u.canSubscribe, subscribes, u)
				}
			}
			if p, _ := f.lk.participant(room, f.screenID("ann")); p.noSubscribe == subscribes {
				t.Errorf("%s: the companion's canSubscribe is now %v, want %v", when, !p.noSubscribe, subscribes)
			}
		}
		t.Run(name+"/claim", func(t *testing.T) {
			f, room := setup(t)
			f.mustClaim(t, "ann")
			if n := len(f.lk.updatesFor(f.screenID("ann"))); n != 1 {
				t.Fatalf("companion updates = %d, want the grant", n)
			}
			check(t, f, room, "after the claim")
		})
		t.Run(name+"/stop", func(t *testing.T) {
			f, room := setup(t)
			f.mustClaim(t, "ann")
			f.publishScreen(room, true, false)
			f.mustStop(t, "ann")
			if n := len(f.lk.updatesFor(f.screenID("ann"))); n != 2 {
				t.Fatalf("companion updates = %d, want the grant and the withdrawal", n)
			}
			check(t, f, room, "after the stop")
		})
		t.Run(name+"/unclaimed", func(t *testing.T) {
			f, room := setup(t)
			f.publishScreen(room, true, false)
			f.at(500)
			f.pass(t)
			if n := len(f.lk.updatesFor(f.screenID("ann"))); n != 1 {
				t.Fatalf("companion updates = %d, want the withdrawal", n)
			}
			check(t, f, room, "after the pass")
		})
		t.Run(name+"/pass after a stop", func(t *testing.T) {
			f, room := setup(t)
			f.mustClaim(t, "ann")
			f.publishScreen(room, true, false)
			f.lk.setUpdateFail(f.screenID("ann"), true)
			f.mustStop(t, "ann") // the companion's withdrawal fails; the pass retries
			f.lk.setUpdateFail(f.screenID("ann"), false)
			f.at(500)
			f.pass(t)
			if p, _ := f.lk.participant(room, f.screenID("ann")); p.screen || p.shareGranted {
				t.Errorf("the pass did not withdraw the companion: %+v", p)
			}
			check(t, f, room, "after the retry")
		})
	}
}

// ---------------------------------------------------------------------------
// Revocation is synchronous

// requireWithdrawn fails unless ann's companion has neither a screen grant nor
// a published screen in LiveKit, and its last update took everything away.
func requireWithdrawn(t *testing.T, f *presenceFixture, room, when string) {
	t.Helper()
	p, ok := f.lk.participant(room, f.screenID("ann"))
	if !ok {
		t.Fatalf("%s: the companion is not in the room", when)
	}
	if p.shareGranted || p.screen || p.screenAudio {
		t.Errorf("%s: the companion still has %+v", when, p)
	}
	ups := f.lk.updatesFor(f.screenID("ann"))
	if len(ups) == 0 || ups[len(ups)-1].canPublish || len(ups[len(ups)-1].sources) != 0 {
		t.Errorf("%s: the companion's last update = %+v, want canPublish false and no sources", when, ups)
	}
}

// TestVoiceShareRevocationReachesTheCompanion: every way a claim is revoked
// withdraws the companion's grant before the request is answered, with no pass
// in between: a stop (by the sharer and by an operator), a claim whose audit
// row cannot be written, a claim that finds its room retired, and a repeated
// claim whose share ended meanwhile.
func TestVoiceShareRevocationReachesTheCompanion(t *testing.T) {
	t.Run("stop by the sharer", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.mustClaim(t, "ann")
		f.publishScreen(room, true, true)
		f.mustStop(t, "ann")
		requireWithdrawn(t, f, room, "after the stop")
	})
	t.Run("stop by an operator", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.mustClaim(t, "ann")
		f.publishScreen(room, true, false)
		f.mustStop(t, "root")
		requireWithdrawn(t, f, room, "after the operator's stop")
	})
	t.Run("the audit row cannot be written", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.publishScreen(room, true, false)
		write := f.srv.voice.appendAudit
		f.srv.voice.appendAudit = func(context.Context, string, string, string, string, time.Time) (store.AuditEvent, error) {
			return store.AuditEvent{}, errors.New("disk full")
		}
		res := f.claimShare(t, "ann", "ops")
		f.srv.voice.appendAudit = write
		if res.status != http.StatusInternalServerError {
			t.Fatalf("claim = %d %s, want 500", res.status, res.body)
		}
		requireWithdrawn(t, f, room, "after the failed claim")
	})
	t.Run("the room is rotated during the claim", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.publishScreen(room, true, false)
		stored, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
		if err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		f.lk.setOnUpdate(func(_, _ string, share bool) {
			if share {
				once.Do(func() {
					if _, _, err := f.srv.store.RotateVoiceRoom(context.Background(), stored.ID, store.VoiceRotateMemberRemoved); err != nil {
						t.Error(err)
					}
				})
			}
		})
		res := f.claimShare(t, "ann", "ops")
		if res.status != http.StatusConflict {
			t.Fatalf("claim = %d %s, want 409", res.status, res.body)
		}
		if n := f.lk.callsFor("DeleteRoom", room); n != 0 {
			t.Errorf("DeleteRoom calls = %d; the test means the grant to be taken back without one", n)
		}
		requireWithdrawn(t, f, room, "after the claim in a rotated room")
	})
	t.Run("a repeated claim whose share ended meanwhile", func(t *testing.T) {
		f, room := shareWithCompanion(t)
		f.mustClaim(t, "ann")
		f.publishScreen(room, true, false)
		var once sync.Once
		f.lk.setOnUpdate(func(_, identity string, share bool) {
			if share && identity == f.identity("ann") {
				once.Do(func() { // a pass ended the share while it was granted again
					f.srv.voice.mu.Lock()
					f.srv.voice.rooms[room].share = nil
					f.srv.voice.mu.Unlock()
				})
			}
		})
		if res := f.claimShare(t, "ann", "ops"); res.status != http.StatusConflict {
			t.Fatalf("claim = %d %s, want 409", res.status, res.body)
		}
		requireWithdrawn(t, f, room, "after the repeated claim")
	})
}

// ---------------------------------------------------------------------------
// The viewer

// TestVoiceShareUnclaimedViewer: a viewer beside its person is judged like
// the rest. Nothing it holds is a share, so a screen it publishes is
// unclaimed even while its person holds the share, and it is withdrawn to
// nothing (never the person's microphone bundle) or removed.
func TestVoiceShareUnclaimedViewer(t *testing.T) {
	viewer := func(f *presenceFixture) fakeParticipant {
		return fakeParticipant{identity: f.viewID("ann"), joinedMs: 3_000, listenOnly: true, screen: true}
	}
	setup := func(t *testing.T) (*presenceFixture, string) {
		t.Helper()
		f, room := newShareFixture(t)
		f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"), viewer(f))
		return f, room
	}
	for _, holds := range []bool{false, true} {
		name := map[bool]string{false: "no claim", true: "its person holds the share"}[holds]
		t.Run(name+"/withdrawn", func(t *testing.T) {
			f, room := setup(t)
			if holds {
				f.mustClaim(t, "ann")
			}
			f.at(500)
			f.pass(t)
			ups := f.lk.updatesFor(f.viewID("ann"))
			if len(ups) != 1 || ups[0].canPublish || !ups[0].canSubscribe || ups[0].sources == nil || len(ups[0].sources) != 0 {
				t.Fatalf("viewer updates = %+v, want one: canSubscribe, no publish, sources []", ups)
			}
			if p, _ := f.lk.participant(room, f.viewID("ann")); p.screen {
				t.Error("the viewer still publishes a screen")
			}
			rows := f.audits(t, store.AuditVoiceShareUnclaimed)
			wantDetail := fmt.Sprintf("channel=%d sources=screen_share participant=view source=observed", f.ops.ID)
			if len(rows) != 1 || rows[0].Actor != f.actor("ann") || rows[0].Detail != wantDetail {
				t.Fatalf("unclaimed rows = %+v, want one by ann with detail %q", rows, wantDetail)
			}
			// Once, and the person's own participant and share are not touched.
			f.at(1_000)
			f.pass(t)
			if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 1 {
				t.Errorf("unclaimed rows after another pass = %d, want 1", n)
			}
			if got := f.lk.updatesFor(f.identity("ann")); len(got) != btoi(holds) {
				t.Errorf("ann's own participant was updated %d times: %+v", len(got), got)
			}
			if holds && f.shareState(room) != f.ids["ann"] {
				t.Error("the viewer's screen ended its person's share")
			}
		})
		t.Run(name+"/not withdrawn: removed", func(t *testing.T) {
			f, room := setup(t)
			if holds {
				f.mustClaim(t, "ann")
			}
			f.lk.setUpdateFail(f.viewID("ann"), true)
			f.at(500)
			f.pass(t)
			if f.lk.in(room, f.viewID("ann")) {
				t.Fatal("the viewer is still connected with a screen")
			}
			if !f.lk.in(room, f.identity("ann")) {
				t.Error("ann herself was removed")
			}
			want := fmt.Sprintf("channel=%d audience=channel principal=%d reason=%s participant=view source=observed", f.ops.ID, f.ids["ann"], voiceReasonShareUnclaimed)
			if got := f.removedRows(t); !slices.Equal(got, []string{want}) {
				t.Errorf("removal rows = %q, want %q", got, want)
			}
		})
	}
	t.Run("a viewer with a source grant and no track", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
			fakeParticipant{identity: f.viewID("ann"), joinedMs: 3_000, shareGranted: true})
		f.at(500)
		f.pass(t)
		if p, _ := f.lk.participant(room, f.viewID("ann")); p.shareGranted {
			t.Error("the viewer still holds a source grant")
		}
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
			t.Errorf("unclaimed rows = %d: nothing was published", n)
		}
	})
	t.Run("a viewer alone, standing in for the person, is judged too", func(t *testing.T) {
		f, room := newShareFixture(t)
		f.lk.setRoom(room, mustParticipant(t, f, room, "ann2"), viewer(f))
		f.at(500)
		f.pass(t)
		if p, _ := f.lk.participant(room, f.viewID("ann")); p.screen {
			t.Error("the viewer still publishes a screen")
		}
		if ups := f.lk.updatesFor(f.identity("ann")); len(ups) != 0 {
			t.Errorf("an update was sent to p<id>, which is not in the room: %+v", ups)
		}
	})
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
