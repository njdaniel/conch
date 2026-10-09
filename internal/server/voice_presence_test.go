package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func TestParseVoiceIdentity(t *testing.T) {
	tests := []struct {
		in     string
		wantID int64
		wantOK bool
	}{
		{"p1", 1, true},
		{"p7", 7, true},
		{"p123456", 123456, true},
		{"", 0, false},
		{"p", 0, false},
		{"p0", 0, false},
		{"p07", 0, false},
		{"p+7", 0, false},
		{"p-7", 0, false},
		{"p7 ", 0, false},
		{" p7", 0, false},
		{"P7", 0, false},
		{"7", 0, false},
		{"p7x", 0, false},
		{"pp7", 0, false},
		{"p1.5", 0, false},
		{"p٣", 0, false}, // non-ASCII digit
		{"p99999999999999999999", 0, false},
		{"agent-1", 0, false},
	}
	for _, tt := range tests {
		id, ok := parseVoiceIdentity(tt.in)
		if id != tt.wantID || ok != tt.wantOK {
			t.Errorf("parseVoiceIdentity(%q) = %d, %v; want %d, %v", tt.in, id, ok, tt.wantID, tt.wantOK)
		}
	}
}

// TestVoicePollerSessionMarksRoomInUse: a pass polls nothing until a session
// is issued, and a session issued through the endpoint puts its room in use
// before any token exists.
func TestVoicePollerSessionMarksRoomInUse(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.pass(t)
	if n := len(f.lk.calls()); n != 0 {
		t.Fatalf("idle pass made %d LiveKit calls, want 0", n)
	}
	decodeSession(t, f.session(t, "ann", "ops"))
	if !f.srv.voice.anyInUse() {
		t.Fatal("room is not in use after a session")
	}
	f.pass(t)
	if got := f.lk.count("ListParticipants"); got != 1 {
		t.Fatalf("ListParticipants after session = %d, want 1", got)
	}
	// Two minutes on, with nobody in the room, it is idle again.
	f.clock.Advance(voiceSessionRecent + time.Second)
	if f.srv.voice.anyInUse() {
		t.Fatal("room still in use two minutes after the session with nobody in it")
	}
}

// TestVoicePollerSession is the scripted session: join, unmute, mute, leave.
// The audit has exactly the expected events in order, once each, with the
// principal as actor and source=observed.
func TestVoicePollerSession(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)

	steps := []struct {
		name   string
		script func()
		want   string // who() after the pass
		events []string
	}{
		{"empty room, first contact", func() {}, "", nil},
		{"ann joins", func() {
			f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000, published: true, muted: true})
		},
			"ann", []string{"voice_joined " + f.actor("ann")}},
		{"nothing changes", func() {}, "ann", nil},
		{"ann unmutes", func() { f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) { p.muted = false }) },
			"ann*", []string{"voice_transmit_started " + f.actor("ann")}},
		{"nothing changes while she talks", func() {}, "ann*", nil},
		{"ann mutes", func() { f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) { p.muted = true }) },
			"ann", []string{"voice_transmit_stopped " + f.actor("ann")}},
		{"ann2 joins without a mic", func() {
			f.lk.setRoom(room,
				fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000, published: true, muted: true},
				fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
		}, "ann,ann2", []string{"voice_joined " + f.actor("ann2")}},
		{"ann leaves", func() { f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000}) },
			"ann2", []string{"voice_left " + f.actor("ann")}},
		{"ann2 leaves", func() { f.lk.setRoom(room) }, "", []string{"voice_left " + f.actor("ann2")}},
	}
	var wantAll []string
	for _, st := range steps {
		st.script()
		f.pass(t)
		f.clock.Advance(voicePollInterval)
		got := f.who(t, f.ops)
		if got == "-" {
			t.Fatalf("%s: snapshot has no room", st.name)
		}
		if got != st.want {
			t.Errorf("%s: room = %q, want %q", st.name, got, st.want)
		}
		wantAll = append(wantAll, st.events...)
		if got := f.voiceAudit(t); !slices.Equal(got, wantAll) {
			t.Fatalf("%s: audit = %q, want %q", st.name, got, wantAll)
		}
	}

	for _, e := range f.audit(t) {
		if !strings.HasPrefix(e.Action, "voice_") || e.Action == store.AuditVoiceSessionIssued {
			continue
		}
		if e.Subject != fmt.Sprintf("channel:%d", f.ops.ID) {
			t.Errorf("%s subject = %q", e.Action, e.Subject)
		}
		if want := fmt.Sprintf("channel=%d audience=channel source=observed", f.ops.ID); e.Detail != want {
			t.Errorf("%s detail = %q, want %q", e.Action, e.Detail, want)
		}
	}
}

// TestVoicePollerJoinedAtAndDocument: the snapshot carries LiveKit's join
// time, can_publish, the schema name, and passes Validate; it is served
// as-is by the endpoint.
func TestVoicePollerJoinedAtAndDocument(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	f.lk.setRoom(room,
		fakeParticipant{identity: f.identity("ann2"), joinedMs: 1_700_000_002_000},
		fakeParticipant{identity: f.identity("ann"), joinedMs: 1_700_000_001_000, published: true, muted: true})
	f.pass(t)

	res := f.callREST(t, http.MethodGet, "/v1/channels/ops/voice", f.tokens["ann"], "")
	if res.status != http.StatusOK {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	var got schema.VoicePresenceV1
	if err := json.Unmarshal([]byte(res.body), &got); err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("document does not validate: %v", err)
	}
	if !got.Configured || !got.Available || got.ChannelID != f.ops.ID || got.Schema != schema.VoicePresenceSchemaV1 || len(got.Rooms) != 1 {
		t.Fatalf("document = %+v", got)
	}
	parts := got.Rooms[0].Participants
	if len(parts) != 2 || parts[0].PrincipalID >= parts[1].PrincipalID {
		t.Fatalf("participants not ordered by principal id: %+v", parts)
	}
	for _, p := range parts {
		want := time.UnixMilli(1_700_000_001_000)
		if p.PrincipalID == f.ids["ann2"] {
			want = time.UnixMilli(1_700_000_002_000)
		}
		if !p.JoinedAt.Time().Equal(want) || !p.CanPublish || p.Transmitting {
			t.Errorf("participant %+v, want joined %v, can_publish, not transmitting", p, want)
		}
	}
}

// TestVoicePollerChangesBetweenPasses covers what a table of single steps does
// not: leaving while transmitting, and a second connection displacing the
// first between two passes.
func TestVoicePollerChangesBetweenPasses(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	ann := f.identity("ann")
	a := f.actor("ann")

	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000, published: true})
	f.pass(t) // joined, started
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 5_000, published: true, muted: true})
	f.pass(t) // displaced: stop, left, joined (not transmitting now)
	f.lk.setRoom(room)
	f.pass(t) // left

	want := []string{
		"voice_joined " + a, "voice_transmit_started " + a,
		"voice_transmit_stopped " + a, "voice_left " + a, "voice_joined " + a,
		"voice_left " + a,
	}
	if got := f.voiceAudit(t); !slices.Equal(got, want) {
		t.Fatalf("audit = %q, want %q", got, want)
	}

	// Leaving while transmitting closes the transmission first.
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 9_000, published: true})
	f.pass(t)
	f.lk.setRoom(room)
	f.pass(t)
	got := f.voiceAudit(t)
	if tail := got[len(got)-4:]; !slices.Equal(tail, []string{
		"voice_joined " + a, "voice_transmit_started " + a, "voice_transmit_stopped " + a, "voice_left " + a}) {
		t.Fatalf("audit tail = %q", tail)
	}
}

// TestVoicePollerUnentitled: everyone who must not be in the room is removed
// on the pass that sees them, audited with a reason, and never appears in a
// snapshot: not after the pass, not while the removal is under way, and no
// subscriber is woken for them.
func TestVoicePollerUnentitled(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	tests := []struct {
		name     string
		identity string
		reason   string
		wantPID  string // principal=... in the audit detail
	}{
		{"non-member", f.identity("bob"), voiceReasonNotMember, fmt.Sprint(f.ids["bob"])},
		{"disabled member", f.identity("dora"), voiceReasonDisabled, fmt.Sprint(f.ids["dora"])},
		{"agent member", f.identity("robo"), voiceReasonNotHuman, fmt.Sprint(f.ids["robo"])},
		{"operator who is not a member", f.identity("root"), voiceReasonNotMember, fmt.Sprint(f.ids["root"])},
		{"identity that is not p<id>", "robo-1", voiceReasonBadIdentity, "none"},
		{"leading zero", "p0" + fmt.Sprint(f.ids["ann"]), voiceReasonBadIdentity, "none"},
		{"names no principal", "p99999", voiceReasonUnknownPrincipal, "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			room := f.inUse(t, f.ops)
			sub := f.srv.voice.subscribe(f.ops.ID, f.ids["ann"])
			defer sub.cancel()
			f.pass(t) // first contact: available, empty
			<-sub.wake

			var mu sync.Mutex
			var midRemoval []string
			f.lk.setOnRemove(func(string, string) {
				who := f.who(t, f.ops)
				mu.Lock()
				defer mu.Unlock()
				midRemoval = append(midRemoval, who)
			})
			f.lk.setRoom(room, fakeParticipant{identity: tt.identity, joinedMs: 1_000, published: true})
			f.pass(t)

			if got := f.who(t, f.ops); got != "" {
				t.Errorf("snapshot after the pass = %q, want an empty room", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(midRemoval, []string{""}) {
				t.Errorf("snapshots taken during removal = %q, want one empty room", midRemoval)
			}
			select {
			case <-sub.wake:
				t.Error("a subscriber was woken for a participant who never entered presence")
			default:
			}
			removes := 0
			for _, c := range f.lk.calls() {
				if c.method == "RemoveParticipant" && c.identity == tt.identity && c.room == room {
					removes++
				}
			}
			if removes != 1 {
				t.Errorf("RemoveParticipant calls = %d, want 1", removes)
			}
			var rows []store.AuditEvent
			for _, e := range f.audit(t) {
				if e.Action == store.AuditVoiceParticipantRemoved {
					rows = append(rows, e)
				}
			}
			want := fmt.Sprintf("channel=%d audience=channel principal=%s reason=%s source=observed", f.ops.ID, tt.wantPID, tt.reason)
			if len(rows) != 1 || rows[0].Detail != want || rows[0].Actor != "system" {
				t.Errorf("removed rows = %+v, want one with detail %q", rows, want)
			}
			for _, a := range f.voiceAudit(t) {
				if strings.HasPrefix(a, "voice_joined") || strings.HasPrefix(a, "voice_left") {
					t.Errorf("unentitled participant produced %q", a)
				}
			}
		})
	}
	_ = f
}

// TestVoicePollerRemovalRetry: a failed RemoveParticipant is retried on the
// next pass; the participant never appears; one removal is audited when it
// succeeds, and a participant LiveKit is slow to drop is not audited twice.
func TestVoicePollerRemovalRetry(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	bob := f.identity("bob")
	f.lk.setRoom(room, fakeParticipant{identity: bob, joinedMs: 1_000})
	f.lk.setRemoveStatus(http.StatusInternalServerError)

	for i := 1; i <= 2; i++ {
		f.pass(t)
		if got := f.who(t, f.ops); got != "" {
			t.Fatalf("pass %d: bob in snapshot: %q", i, got)
		}
		if got := f.lk.count("RemoveParticipant"); got != i {
			t.Fatalf("pass %d: RemoveParticipant calls = %d", i, got)
		}
	}
	if got := f.voiceAudit(t); len(got) != 0 {
		t.Fatalf("audit before a removal succeeded = %q", got)
	}
	if !strings.Contains(f.logs.buf.String(), "removal failed") {
		t.Error("a failed removal was not logged")
	}

	f.lk.setRemoveStatus(0)
	f.lk.setKeepAfterRemove(true) // LiveKit answers 200 but is slow to close the connection
	f.pass(t)
	f.pass(t)
	if got := f.lk.count("RemoveParticipant"); got != 4 {
		t.Fatalf("RemoveParticipant calls = %d, want 4 (each pass retries until bob is gone)", got)
	}
	if got := f.voiceAudit(t); !slices.Equal(got, []string{"voice_participant_removed system"}) {
		t.Fatalf("audit = %q, want one removal", got)
	}
	f.lk.setKeepAfterRemove(false)
	f.pass(t)
	f.pass(t)
	if got := f.lk.count("RemoveParticipant"); got != 5 {
		t.Fatalf("RemoveParticipant calls = %d, want 5 (none once bob is gone)", got)
	}
}

// TestVoicePollerOutageAndRecovery: with LiveKit failing, presence is
// unavailable with no participants, one event is written per outage, retries
// back off 1 s doubling to 30 s on the injected clock, and recovery reports
// the room's real state without re-auditing joins for people who stayed.
func TestVoicePollerOutageAndRecovery(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	ann, ann2 := f.identity("ann"), f.identity("ann2")
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000}, fakeParticipant{identity: ann2, joinedMs: 2_000})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann,ann2" {
		t.Fatalf("before the outage: %q", got)
	}

	f.lk.setListStatus(http.StatusInternalServerError)
	wantWaits := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, wait := range wantWaits {
		before := f.lk.count("ListParticipants")
		f.pass(t) // attempt i
		if got := f.lk.count("ListParticipants"); got != before+1 {
			t.Fatalf("attempt %d: requests = %d, want %d", i, got, before+1)
		}
		s := f.snap(t, f.ops)
		if !s.Configured || s.Available || len(s.Rooms) != 0 {
			t.Fatalf("attempt %d: snapshot during outage = %+v", i, s)
		}
		// Held back until the wait has passed: the pass does not run.
		f.clock.Advance(wait - time.Millisecond)
		if f.srv.voice.runPass(context.Background()) {
			t.Fatalf("attempt %d: a pass ran %v into a %v backoff", i, wait-time.Millisecond, wait)
		}
		if got := f.lk.count("ListParticipants"); got != before+1 {
			t.Fatalf("attempt %d: a held pass reached LiveKit", i)
		}
		f.clock.Advance(time.Millisecond)
	}
	if got := countAction(f.audit(t), store.AuditVoiceEnforcementUnavailable); got != 1 {
		t.Fatalf("voice_enforcement_unavailable events = %d, want 1 for the whole outage", got)
	}
	if got := f.voiceAudit(t); got[len(got)-1] != "voice_enforcement_unavailable system" {
		t.Fatalf("audit = %q", got)
	}

	// Recovery: ann2 left during the outage, ann stayed, bob arrived.
	f.lk.setListStatus(0)
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 7_000})
	f.pass(t)
	s := f.snap(t, f.ops)
	if !s.Available || f.who(t, f.ops) != "ann,ann2" {
		t.Fatalf("after recovery: %+v / %q", s, f.who(t, f.ops))
	}
	// ann stayed (no new join); ann2 rejoined with a new connection (left, joined).
	var joins, lefts []string
	for _, a := range f.voiceAudit(t) {
		switch {
		case strings.HasPrefix(a, "voice_joined"):
			joins = append(joins, a)
		case strings.HasPrefix(a, "voice_left"):
			lefts = append(lefts, a)
		}
	}
	if len(joins) != 3 || len(lefts) != 1 || lefts[0] != "voice_left "+f.actor("ann2") {
		t.Fatalf("joins %q lefts %q: want ann and ann2 joined once, ann2 re-joined, ann2 left once", joins, lefts)
	}

	// A second outage is a second event.
	f.lk.setListStatus(http.StatusInternalServerError)
	f.pass(t)
	f.clock.Advance(time.Second)
	f.pass(t)
	if got := countAction(f.audit(t), store.AuditVoiceEnforcementUnavailable); got != 2 {
		t.Fatalf("voice_enforcement_unavailable events = %d, want 2", got)
	}
}

// TestVoicePollerOutageNeverLeaksLastSeen: while LiveKit is down the people
// last seen are not reported, even though the poller remembers them so that
// recovery does not re-audit their joins.
func TestVoicePollerOutageNeverLeaksLastSeen(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
	f.pass(t)
	f.lk.setListStatus(http.StatusServiceUnavailable)
	f.pass(t)
	res := f.callREST(t, http.MethodGet, "/v1/channels/ops/voice", f.tokens["ann2"], "")
	var s schema.VoicePresenceV1
	if err := json.Unmarshal([]byte(res.body), &s); err != nil || s.Available || len(s.Rooms) != 0 || !s.Configured {
		t.Fatalf("snapshot during outage: %v %s", err, res.body)
	}
	if !strings.Contains(res.body, `"rooms":[]`) {
		t.Errorf("rooms must encode as [], got %s", res.body)
	}
}

// TestVoicePollerSweepFindsRoomsAfterRestart: after a restart the poller has
// no memory of sessions. The first sweep marks every stored room LiveKit
// lists, whatever it reports as the participant count (zero here), and the
// next pass finds who is in it: the entitled appear, the others are removed.
func TestVoicePollerSweepFindsRoomsAfterRestart(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	opsRoom := f.room(t, f.ops)   // stored and listed
	ops2Room := f.room(t, f.ops2) // stored, not in LiveKit
	f.lk.setRoom(opsRoom,
		fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000},
		fakeParticipant{identity: f.identity("bob"), joinedMs: 2_000})
	f.lk.setRoom("conch-not-ours")
	f.lk.reportedCount = 0

	f.pass(t)
	if got := len(f.lk.calls()); got != 0 {
		t.Fatalf("pass before any sweep made %d calls", got)
	}
	if s := f.snap(t, f.ops); s.Available {
		t.Fatal("presence is available before LiveKit has been asked")
	}

	f.sweep(t)
	if got := f.lk.calls(); len(got) != 1 || got[0].method != "ListRooms" {
		t.Fatalf("sweep calls = %+v, want one ListRooms", got)
	}
	f.pass(t)
	polled := map[string]int{}
	for _, c := range f.lk.calls() {
		if c.method == "ListParticipants" {
			polled[c.room]++
		}
	}
	if len(polled) != 1 || polled[opsRoom] != 1 || polled[ops2Room] != 0 {
		t.Fatalf("rooms polled = %v, want only the stored room LiveKit lists", polled)
	}
	if got := f.who(t, f.ops); got != "ann" {
		t.Fatalf("after restart: room = %q, want ann", got)
	}
	if got := f.lk.count("RemoveParticipant"); got != 1 {
		t.Fatalf("RemoveParticipant calls = %d, want bob removed", got)
	}
	want := []string{"voice_joined " + f.actor("ann")}
	var got []string
	for _, a := range f.voiceAudit(t) {
		if !strings.HasPrefix(a, "voice_participant_removed") {
			got = append(got, a)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("audit = %q, want %q", got, want)
	}
	// The mark was for one pass: with ann in the room it stays in use because
	// someone is there, and ops2, never listed, stays out.
	if !f.srv.voice.anyInUse() {
		t.Fatal("room with a participant is not in use")
	}
}

// TestVoicePollerIdle: with no room in use the only call is the sweep, and a
// room leaves "in use" once empty and unmarked.
func TestVoicePollerIdle(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.room(t, f.ops)

	for range 5 {
		f.pass(t)
		f.clock.Advance(voicePollInterval)
	}
	if got := len(f.lk.calls()); got != 0 {
		t.Fatalf("idle passes made %d calls", got)
	}
	f.sweep(t)
	f.pass(t)
	if got := f.lk.calls(); len(got) != 1 || got[0].method != "ListRooms" {
		t.Fatalf("calls = %+v, want only the sweep (LiveKit lists no rooms)", got)
	}
	if d := f.srv.voice.nextDelay(); d != voiceSweepInterval {
		t.Fatalf("idle delay = %v, want the sweep interval", d)
	}

	// Sweep finds the room: marked, polled once, empty, idle again.
	f.lk.setRoom(room)
	f.clock.Advance(voiceSweepInterval)
	f.sweep(t)
	if d := f.srv.voice.nextDelay(); d != voicePollInterval {
		t.Fatalf("delay with a room in use = %v, want %v", d, voicePollInterval)
	}
	f.pass(t)
	if f.srv.voice.anyInUse() {
		t.Fatal("empty, unmarked room still in use after being polled")
	}
	before := len(f.lk.calls())
	f.pass(t)
	if len(f.lk.calls()) != before {
		t.Fatal("a pass polled an idle room")
	}
}

// TestVoicePollerConcurrency: at most eight rooms are polled at once, and a
// pass still running is not started again.
func TestVoicePollerConcurrency(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	const rooms = 12
	ctx := context.Background()
	for i := range rooms {
		ch, err := f.srv.store.CreateChannel(ctx, fmt.Sprintf("room%d", i))
		if err != nil {
			t.Fatal(err)
		}
		f.inUse(t, ch)
	}
	release := make(chan struct{})
	f.lk.setHoldList(release)

	done := make(chan bool)
	go func() { done <- f.srv.voice.runPass(ctx) }()
	waitUntil(t, "eight rooms in flight", func() bool { return f.lk.currentInflight() == voicePollConcurrency })

	if f.srv.voice.runPass(ctx) {
		t.Error("a second pass started while the first was running")
	}
	if got := f.lk.currentInflight(); got != voicePollConcurrency {
		t.Errorf("in flight = %d, want %d", got, voicePollConcurrency)
	}
	close(release)
	if ran := <-done; !ran {
		t.Fatal("first pass did not run")
	}
	if got := f.lk.maxConcurrent(); got != voicePollConcurrency {
		t.Errorf("max concurrent ListParticipants = %d, want %d", got, voicePollConcurrency)
	}
	if got := f.lk.count("ListParticipants"); got != rooms {
		t.Errorf("ListParticipants = %d, want one per room (%d)", got, rooms)
	}
}

// TestVoiceImmediateRemoval: removing a member and disabling a principal each
// call RemoveParticipant before the HTTP response is written, for every room of
// every affected channel, for a person who was never issued a session (so no
// rotation covers them); a failure there does not fail the request and later
// passes finish the job. A revoke-all removes nobody by name any more: the rooms
// its credentials held are rotated instead (issue #161), see
// TestVoiceHookRotation.
func TestVoiceImmediateRemoval(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      func(f *presenceFixture) string
		wantCode  int
		wantRooms []string // channels whose rooms are called
		reason    string
		// audited is whether voice_participant_removed names the reason.
	}{
		{"member removed", "DELETE", func(f *presenceFixture) string { return fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]) }, 204, []string{"ops"}, voiceReasonMemberRemoved},
		{"principal disabled", "POST", func(f *presenceFixture) string { return fmt.Sprintf("/v1/principals/%d/disable", f.ids["ann"]) }, 204, []string{"ops", "ops2"}, voiceReasonPrincipalOff},
	}
	for _, tt := range tests {
		for _, failing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/livekit failing=%v", tt.name, failing), func(t *testing.T) {
				f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
				rooms := map[string]string{"ops": f.inUse(t, f.ops), "ops2": f.inUse(t, f.ops2)}
				ann := f.identity("ann")
				for _, name := range []string{"ops", "ops2"} {
					f.lk.setRoom(rooms[name], fakeParticipant{identity: ann, joinedMs: 1_000, published: true},
						fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
				}
				f.pass(t)
				if got := f.who(t, f.ops); got != "ann*,ann2" {
					t.Fatalf("before: %q", got)
				}
				base := len(f.lk.calls())
				if failing {
					f.lk.setRemoveStatus(http.StatusInternalServerError)
				}

				code := f.doOrdered(t, tt.method, tt.path(f), f.rootTok)
				if code != tt.wantCode {
					t.Fatalf("status = %d, want %d (a LiveKit failure must not fail the request)", code, tt.wantCode)
				}

				var removed []string
				respAt := 0
				for _, c := range f.lk.calls()[base:] {
					switch c.method {
					case "RemoveParticipant":
						if c.identity != ann {
							t.Errorf("removed %q, want ann only", c.identity)
						}
						if respAt != 0 {
							t.Errorf("RemoveParticipant (seq %d) came after the response (seq %d)", c.seq, respAt)
						}
						removed = append(removed, c.room)
					case "http-response":
						respAt = c.seq
					}
				}
				var wantRooms []string
				for _, ch := range tt.wantRooms {
					wantRooms = append(wantRooms, rooms[ch])
				}
				slices.Sort(removed)
				slices.Sort(wantRooms)
				if !slices.Equal(removed, wantRooms) {
					t.Fatalf("RemoveParticipant rooms = %d, want the rooms of %v", len(removed), tt.wantRooms)
				}
				if respAt == 0 {
					t.Fatal("no response marker")
				}
				// She is out of presence at once, with no pass in between.
				if got := f.who(t, f.ops); got != "ann2" {
					t.Errorf("snapshot right after = %q, want ann2 only", got)
				}

				// Passes finish what a failure left: one retry removes her again.
				f.lk.setRemoveStatus(0)
				f.clock.Advance(voicePollInterval)
				f.pass(t)
				if got := f.who(t, f.ops); got != "ann2" {
					t.Errorf("snapshot after a pass = %q, want ann2 only", got)
				}
				if failing {
					if got := f.lk.count("RemoveParticipant"); got <= len(tt.wantRooms) {
						t.Errorf("RemoveParticipant calls = %d: the pass did not retry", got)
					}
					for _, name := range tt.wantRooms {
						if f.lk.in(rooms[name], ann) {
							t.Errorf("ann still in %s after the retry", name)
						}
					}
				}
				// Further passes make no more removals: the retry succeeded.
				calls := f.lk.count("RemoveParticipant")
				f.pass(t)
				if f.lk.count("RemoveParticipant") != calls {
					t.Error("a pass removed again after the retry succeeded")
				}
				// The audit says why, once per room, and ends with her leaving.
				var reasons []string
				for _, e := range f.audit(t) {
					if e.Action == store.AuditVoiceParticipantRemoved {
						reasons = append(reasons, e.Detail[strings.LastIndex(e.Detail, "reason="):])
					}
				}
				if len(reasons) == 0 {
					t.Fatal("no voice_participant_removed event")
				}
				for _, r := range reasons {
					if !strings.HasPrefix(r, "reason=") || r == "reason=" {
						t.Errorf("removal detail %q", r)
					}
				}
			})
		}
	}
}

// TestVoicePresenceEndpoint: the snapshot, by caller.
func TestVoicePresenceEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		opts       presenceOpts
		who        string
		channel    string
		wantStatus int
		wantCode   string
		wantDenied int
	}{
		{"member", presenceOpts{auth: AuthRequired}, "ann", "ops", 200, "", 0},
		{"another member", presenceOpts{auth: AuthRequired}, "ann2", "ops", 200, "", 0},
		{"non-member", presenceOpts{auth: AuthRequired}, "bob", "ops", 404, "channel_not_found", 0},
		{"unknown channel", presenceOpts{auth: AuthRequired}, "ann", "nosuch", 404, "channel_not_found", 0},
		{"operator who is not a member", presenceOpts{auth: AuthRequired}, "root", "ops", 404, "channel_not_found", 0},
		{"agent member", presenceOpts{auth: AuthRequired}, "robo", "ops", 403, "forbidden", 1},
		{"agent non-member", presenceOpts{auth: AuthRequired}, "robo", "ops2", 404, "channel_not_found", 1},
		{"disabled", presenceOpts{auth: AuthRequired}, "dora", "ops", 401, "unauthenticated", 0},
		{"unauthenticated", presenceOpts{auth: AuthRequired}, "", "ops", 401, "unauthenticated", 0},
		{"auth off", presenceOpts{auth: AuthOff}, "", "ops", 400, "voice_requires_auth", 0},
		{"not configured, member", presenceOpts{auth: AuthRequired, unconfigured: true}, "ann", "ops", 200, "", 0},
		{"not configured, non-member", presenceOpts{auth: AuthRequired, unconfigured: true}, "bob", "ops", 404, "channel_not_found", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, tt.opts)
			res := f.callREST(t, http.MethodGet, "/v1/channels/"+tt.channel+"/voice", f.tokens[tt.who], "")
			if res.status != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", res.status, tt.wantStatus, res.body)
			}
			if tt.wantCode != "" && errCode(t, res.body) != tt.wantCode {
				t.Errorf("code = %s, want %s", errCode(t, res.body), tt.wantCode)
			}
			if got := len(f.audits(t, "access_denied")); got != tt.wantDenied {
				t.Errorf("access_denied events = %d, want %d", got, tt.wantDenied)
			}
			if calls := len(f.lk.calls()); calls != 0 {
				t.Errorf("reading presence made %d LiveKit calls", calls)
			}
			if tt.wantStatus != 200 {
				return
			}
			var s schema.VoicePresenceV1
			if err := json.Unmarshal([]byte(res.body), &s); err != nil {
				t.Fatal(err)
			}
			if err := s.Validate(); err != nil {
				t.Fatal(err)
			}
			if tt.opts.unconfigured {
				if s.Configured || s.Available || len(s.Rooms) != 0 {
					t.Errorf("not configured snapshot = %+v", s)
				}
			} else if !s.Configured || s.Available {
				t.Errorf("before LiveKit has answered: %+v, want configured, not available", s)
			}
		})
	}

	t.Run("non-member and unknown channel are byte-identical", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		a := f.callREST(t, http.MethodGet, "/v1/channels/ops/voice", f.tokens["bob"], "")
		b := f.callREST(t, http.MethodGet, "/v1/channels/nosuch/voice", f.tokens["bob"], "")
		c := f.callREST(t, http.MethodGet, "/v1/channels/ops/voice", f.tokens["root"], "")
		if a != b || a != c {
			t.Errorf("responses differ:\n%+v\n%+v\n%+v", a, b, c)
		}
	})
}

// dialVoice opens the presence socket as who.
func dialVoice(t *testing.T, base, channel, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, base+"/v1/voice/ws?channel="+channel, &websocket.DialOptions{HTTPHeader: hdr}) //nolint:bodyclose // closed below
	if err == nil {
		t.Cleanup(func() { _ = conn.CloseNow() })
	} else if resp != nil {
		_ = resp.Body.Close()
	}
	return conn, resp, err
}

func readPresence(t *testing.T, conn *websocket.Conn) schema.VoicePresenceV1 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var s schema.VoicePresenceV1
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("frame does not validate: %v; %s", err, raw)
	}
	return s
}

// expectClosed reads until the server closes the socket and returns the close
// status.
func expectClosed(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			code := websocket.CloseStatus(err)
			if code == -1 {
				t.Fatalf("socket ended without a close frame: %v", err)
			}
			return code
		}
	}
}

// TestVoicePresenceSocketStreams: a snapshot on connect, a new one for every
// change, and nothing when nothing changed (a duplicate would be read in
// place of the next change).
func TestVoicePresenceSocketStreams(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	base := wsTestServer(t, f.srv)
	conn, _, err := dialVoice(t, base, "ops", f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	if s := readPresence(t, conn); !s.Configured || s.Available || len(s.Rooms) != 0 {
		t.Fatalf("on connect: %+v", s)
	}
	steps := []struct {
		name   string
		script func()
		want   string
	}{
		{"first contact", func() {}, ""},
		{"nothing changed, then ann2 joins", func() {
			f.pass(t)
			f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann2"), joinedMs: 1_000, published: true, muted: true})
		}, "ann2"},
		{"nothing changed, then she talks", func() {
			f.pass(t)
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.muted = false })
		}, "ann2*"},
		{"she stops", func() { f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.muted = true }) }, "ann2"},
		{"she leaves", func() { f.lk.setRoom(room) }, ""},
	}
	byID := map[int64]string{}
	for n, id := range f.ids {
		byID[id] = n
	}
	for _, st := range steps {
		st.script()
		f.pass(t)
		s := readPresence(t, conn)
		if !s.Available || len(s.Rooms) != 1 {
			t.Fatalf("%s: %+v", st.name, s)
		}
		var names []string
		for _, p := range s.Rooms[0].Participants {
			n := byID[p.PrincipalID]
			if p.Transmitting {
				n += "*"
			}
			names = append(names, n)
		}
		if got := strings.Join(names, ","); got != st.want {
			t.Errorf("%s: frame = %q, want %q", st.name, got, st.want)
		}
	}
	// An outage is a change too, and carries no participants.
	f.lk.setListStatus(http.StatusInternalServerError)
	f.pass(t)
	if s := readPresence(t, conn); s.Available || len(s.Rooms) != 0 {
		t.Fatalf("outage frame: %+v", s)
	}
}

// TestVoicePresenceSocketClosedOnLoss: the socket is closed when the
// subscriber is removed from the channel, disabled, or has every credential
// revoked, and it is closed before the response is written.
func TestVoicePresenceSocketClosedOnLoss(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   func(f *presenceFixture) string
		reason string
	}{
		{"removed from the channel", "DELETE", func(f *presenceFixture) string { return fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]) }, "no longer a member of this channel"},
		{"disabled", "POST", func(f *presenceFixture) string { return fmt.Sprintf("/v1/principals/%d/disable", f.ids["ann"]) }, reasonCredentialInvalid},
		{"all credentials revoked", "POST", func(f *presenceFixture) string {
			return fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.ids["ann"])
		}, reasonCredentialInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			base := wsTestServer(t, f.srv)
			annConn, _, err := dialVoice(t, base, "ops", f.tokens["ann"])
			if err != nil {
				t.Fatal(err)
			}
			readPresence(t, annConn)
			ann2Conn, _, err := dialVoice(t, base, "ops", f.tokens["ann2"])
			if err != nil {
				t.Fatal(err)
			}
			readPresence(t, ann2Conn)

			if code := f.doOrdered(t, tt.method, tt.path(f), f.rootTok); code != 204 && code != 200 {
				t.Fatalf("status = %d", code)
			}
			if code := expectClosed(t, annConn); code != websocket.StatusPolicyViolation {
				t.Errorf("close status = %d, want policy violation", code)
			}
			// Someone else's socket is untouched: a change still reaches it.
			room := f.inUse(t, f.ops)
			f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann2"), joinedMs: 1_000})
			f.pass(t)
			if s := readPresence(t, ann2Conn); !s.Available {
				t.Errorf("ann2 frame: %+v", s)
			}
		})
	}
}

// TestVoicePresenceSocketRefusals: pre-upgrade refusals. The agent is
// refused whatever its manifest says and audited; non-members get the shared
// 404.
func TestVoicePresenceSocketRefusals(t *testing.T) {
	tests := []struct {
		name       string
		opts       presenceOpts
		who        string
		channel    string
		wantStatus int
		wantDenied int
	}{
		{"non-member", presenceOpts{auth: AuthRequired}, "bob", "ops", 404, 0},
		{"unknown channel", presenceOpts{auth: AuthRequired}, "ann", "nosuch", 404, 0},
		{"operator non-member", presenceOpts{auth: AuthRequired}, "root", "ops", 404, 0},
		{"agent member", presenceOpts{auth: AuthRequired}, "robo", "ops", 403, 1},
		{"unauthenticated", presenceOpts{auth: AuthRequired}, "", "ops", 401, 0},
		{"auth off", presenceOpts{auth: AuthOff}, "", "ops", 400, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, tt.opts)
			base := wsTestServer(t, f.srv)
			_, resp, err := dialVoice(t, base, tt.channel, f.tokens[tt.who])
			if err == nil || resp == nil {
				t.Fatalf("dial succeeded, want %d", tt.wantStatus)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := len(f.audits(t, "access_denied")); got != tt.wantDenied {
				t.Errorf("access_denied events = %d, want %d", got, tt.wantDenied)
			}
		})
	}
}

// TestVoicePollerSlowSubscriberNeverBlocks: a subscriber that never reads
// does not hold up the poller, however many changes happen.
func TestVoicePollerSlowSubscriberNeverBlocks(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	sub := f.srv.voice.subscribe(f.ops.ID, f.ids["ann"])
	defer sub.cancel()
	for i := range 200 {
		if i%2 == 0 {
			f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann2"), joinedMs: int64(1_000 + i)})
		} else {
			f.lk.setRoom(room)
		}
		f.pass(t)
	}
	if len(sub.wake) != 1 {
		t.Errorf("pending wake-ups = %d, want them collapsed into one", len(sub.wake))
	}
}

// TestVoicePresenceNotConfigured: with voice not configured the poller never
// starts and no goroutine is left running.
func TestVoicePresenceNotConfigured(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired, unconfigured: true})
	if f.srv.lk != nil {
		t.Fatal("client built without configuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	if err := f.srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- f.srv.Serve(ctx) }()
	// A served request proves Serve is past its start-up and accepting.
	resp, err := http.Get("http://" + f.srv.Addr() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if n := loopGoroutines(); n != 0 {
		t.Fatalf("poller goroutines while serving unconfigured = %d, want 0", n)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := loopGoroutines(); n != 0 {
		t.Fatalf("poller goroutines after shutdown = %d", n)
	}
	if got := len(f.lk.calls()); got != 0 {
		t.Fatalf("LiveKit calls = %d, want 0", got)
	}
}

// TestVoicePollerShutdownMidPass: Serve returns promptly while a pass is
// blocked in LiveKit, and leaves no goroutine.
func TestVoicePollerShutdownMidPass(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.inUse(t, f.ops)
	hold := make(chan struct{}) // never released while serving: only cancellation ends the request
	f.lk.setHoldList(hold)
	t.Cleanup(func() { close(hold) })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.srv.Serve(ctx) }()
	<-f.lk.entered // a pass is in flight
	if n := loopGoroutines(); n != 1 {
		t.Fatalf("poller goroutines while serving = %d, want 1", n)
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return with a pass in flight")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("shutdown took %v", d)
	}
	if n := loopGoroutines(); n != 0 {
		t.Fatalf("poller goroutines after shutdown = %d", n)
	}
	if got := countAction(f.audit(t), store.AuditVoiceEnforcementUnavailable); got != 0 {
		t.Errorf("shutdown was reported as an outage (%d events)", got)
	}
}

// TestVoicePollerLoopRunsTheFirstSweepAtStart drives the real loop once: the
// first thing it does is a sweep, and then the marked room is polled.
func TestVoicePollerLoopRunsTheFirstSweepAtStart(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.room(t, f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.srv.Serve(ctx) }()
	waitUntil(t, "ann to appear", func() bool { return f.who(t, f.ops) == "ann" })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls := f.lk.calls(); len(calls) < 2 || calls[0].method != "ListRooms" {
		t.Fatalf("calls = %+v, want a sweep first", calls)
	}
}

// TestVoicePresenceKeepsSecrets: no token, secret or room name appears in any
// audit row, log line or presence document, over a run that issues a session,
// joins, transmits, removes someone, suffers an outage and recovers.
func TestVoicePresenceKeepsSecrets(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	resp := decodeSession(t, f.session(t, "ann", "ops"))
	token, room := resp.Rooms[0].Token, resp.Rooms[0].Room
	ann := f.identity("ann")

	var docs []string
	capture := func() {
		docs = append(docs, f.callREST(t, http.MethodGet, "/v1/channels/ops/voice", f.tokens["ann"], "").body)
	}
	f.lk.setRoom(room,
		fakeParticipant{identity: ann, joinedMs: 1_000, published: true},
		fakeParticipant{identity: f.identity("bob"), joinedMs: 2_000})
	f.pass(t)
	capture()
	f.lk.setRemoveStatus(http.StatusInternalServerError)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("bob"), joinedMs: 2_000})
	f.pass(t)
	f.lk.setRemoveStatus(0)
	f.lk.setListStatus(http.StatusInternalServerError)
	f.pass(t)
	capture()
	f.clock.Advance(time.Minute)
	f.lk.setListStatus(0)
	f.pass(t)
	f.callREST(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok, "")
	capture()

	var haystack strings.Builder
	for _, e := range f.audit(t) {
		fmt.Fprintf(&haystack, "%s|%s|%s|%s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	haystack.WriteString(f.logs.buf.String())
	for _, d := range docs {
		haystack.WriteString(d)
	}
	text := haystack.String()
	for what, secret := range map[string]string{"token": token, "token body": strings.Split(token, ".")[1], "api secret": voiceTestSecret, "api key": voiceTestKey, "room name": room} {
		if strings.Contains(text, secret) {
			t.Errorf("%s appears in an audit row, log line or presence document", what)
		}
	}
	if !strings.Contains(text, "voice_enforcement_unavailable") || !strings.Contains(text, "voice_participant_removed") {
		t.Error("the run did not exercise the outage and removal paths")
	}
}

// When one participant's entitlement cannot be read, that participant keeps
// the state they had (or stays unseen if new), and everyone else in the room
// is still handled on that pass: in particular someone removed on this pass
// leaves presence on this pass.
func TestVoicePollerOneFailedEntitlementCheck(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	ann, ann2 := f.identity("ann"), f.identity("ann2")
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000}, fakeParticipant{identity: ann2, joinedMs: 2_000, published: true})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann,ann2*" {
		t.Fatalf("before: %q", got)
	}
	// ann2's check now fails; ann loses her membership behind the poller's
	// back (no hook); bob-who-is-not-a-member and a newcomer whose check also
	// fails are in the room too.
	failing := map[string]bool{ann2: true, f.identity("dora"): true}
	real := f.srv.voice.entitle
	f.srv.voice.entitle = func(ctx context.Context, identity string, channelID int64) (int64, string, error) {
		if failing[identity] {
			id, _ := parseVoiceIdentity(identity)
			return id, "", errors.New("store unavailable")
		}
		return real(ctx, identity, channelID)
	}
	if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann"]); err != nil {
		t.Fatal(err)
	}
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000}, fakeParticipant{identity: ann2, joinedMs: 2_000},
		fakeParticipant{identity: f.identity("dora"), joinedMs: 3_000})
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann2*" {
		t.Errorf("after the pass: %q, want ann gone, ann2 exactly as she was (still transmitting), the unreadable newcomer unseen", got)
	}
	if f.lk.in(room, ann) {
		t.Error("ann was not removed on the pass that found her without membership")
	}
	if !f.lk.in(room, ann2) || !f.lk.in(room, f.identity("dora")) {
		t.Error("a participant was removed because the store could not be read")
	}
	// The store answers again: ann2's mute is noticed, the newcomer (who is
	// disabled) is removed.
	f.srv.voice.entitle = real
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann2" {
		t.Errorf("once the store answers: %q, want ann2 not transmitting", got)
	}
	if f.lk.in(room, f.identity("dora")) {
		t.Error("the disabled newcomer was not removed once her check could be read")
	}
}

// LiveKit answering while the stored rooms cannot be read must not turn the
// loop into a tight retry: the sweep comes due again after voiceSweepRetry,
// not on the next tick.
func TestVoicePollerSweepStoreFailureBacksOff(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	real := f.srv.voice.storedRooms
	f.srv.voice.storedRooms = func(context.Context) ([]store.VoiceRoom, error) { return nil, errors.New("store unavailable") }
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		f.srv.voice.tick(ctx) // the loop's own step, with the clock standing still
	}
	if got := f.lk.count("ListRooms"); got != 1 {
		t.Errorf("ListRooms calls with the clock standing still = %d, want 1", got)
	}
	if d := f.srv.voice.nextDelay(); d != voiceSweepRetry {
		t.Errorf("next delay = %v, want %v", d, voiceSweepRetry)
	}
	f.clock.Advance(voiceSweepRetry)
	f.srv.voice.tick(ctx)
	if got := f.lk.count("ListRooms"); got != 2 {
		t.Errorf("ListRooms calls after the retry interval = %d, want 2", got)
	}
	// The store answers: the sweep completes and the usual interval applies.
	f.srv.voice.storedRooms = real
	f.clock.Advance(voiceSweepRetry)
	f.srv.voice.tick(ctx)
	if d := f.srv.voice.nextDelay(); d != voiceSweepInterval {
		t.Errorf("next delay after a good sweep = %v, want %v", d, voiceSweepInterval)
	}
}

// With nothing in use the loop sleeps until the next sweep, half a minute
// away. A session issued in that time must start the polling at once: found
// against a real LiveKit, where the first people to join an idle server were
// invisible, and unenforced, until the next sweep.
func TestVoicePollerLoopWakesWhenASessionIsIssued(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.srv.Serve(ctx) }()
	// The first sweep has run and found nothing: the loop is now asleep
	// until the next one (the test clock never moves, so that is 30 s away).
	waitUntil(t, "the first sweep", func() bool { return f.lk.count("ListRooms") == 1 })
	waitUntil(t, "presence to become available", func() bool { return f.snap(t, f.ops).Available })
	waitUntil(t, "the loop to go idle until the next sweep", func() bool { return f.srv.voice.nextDelay() == voiceSweepInterval })

	// A member asks for a session over the real endpoint, then joins.
	if res := f.session(t, "ann", "ops"); res.status != http.StatusOK {
		t.Fatalf("session = %d %s", res.status, res.body)
	}
	room := f.room(t, f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000},
		fakeParticipant{identity: f.identity("bob"), joinedMs: 2_000}) // bob is not a member
	started := time.Now()
	waitUntil(t, "ann to appear", func() bool { return f.who(t, f.ops) == "ann" })
	waitUntil(t, "the non-member to be removed", func() bool { return !f.lk.in(room, f.identity("bob")) })
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("presence and enforcement took %v after the session; the loop did not wake", took)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A removed member who walks back in on a token that has not yet expired is
// removed again within a pass, and that second removal is audited too (design
// note §9): it is a different connection, told apart by its join time. The
// same connection listed twice (LiveKit slow to drop it) is still one row.
func TestVoicePollerRejoinRemovalIsAudited(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	bob := f.identity("bob") // not a member of ops
	f.lk.setRoom(room, fakeParticipant{identity: bob, joinedMs: 1_000})
	f.pass(t)
	// He is back a moment later on the same token: a new connection.
	f.clock.Advance(voicePollInterval)
	f.lk.setRoom(room, fakeParticipant{identity: bob, joinedMs: 1_700})
	f.pass(t)
	if n := len(f.audits(t, store.AuditVoiceParticipantRemoved)); n != 2 {
		t.Errorf("voice_participant_removed = %d, want 2: the first removal and the rejoin", n)
	}
	// LiveKit slow to drop: the same connection is still listed on the next pass.
	f.lk.setKeepAfterRemove(true)
	f.clock.Advance(voicePollInterval)
	f.lk.setRoom(room, fakeParticipant{identity: bob, joinedMs: 2_400})
	f.pass(t)
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if n := len(f.audits(t, store.AuditVoiceParticipantRemoved)); n != 3 {
		t.Errorf("voice_participant_removed = %d, want 3: one more for the third connection, none for seeing it twice", n)
	}
	if got := f.who(t, f.ops); got != "" && got != "-" {
		t.Errorf("a non-member was shown: %q", got)
	}
}

// A pass that could not clear a room keeps it polled. Without that, a room
// whose only occupant could not be removed (or could not be checked) dropped
// out of "in use" and was not looked at again until the next sweep, half a
// minute later.
func TestVoicePollerKeepsPollingARoomItCouldNotClear(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(f *presenceFixture)
		mend   func(f *presenceFixture)
	}{
		{"removal failing",
			func(f *presenceFixture) { f.lk.setRemoveStatus(http.StatusInternalServerError) },
			func(f *presenceFixture) { f.lk.setRemoveStatus(0) }},
		{"entitlement unreadable",
			func(f *presenceFixture) {
				f.srv.voice.entitle = func(_ context.Context, identity string, _ int64) (int64, string, error) {
					id, _ := parseVoiceIdentity(identity)
					return id, "", errors.New("store unavailable")
				}
			},
			func(f *presenceFixture) { f.srv.voice.entitle = f.srv.voiceEntitlement }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			room := f.room(t, f.ops) // no session: only a sweep knows the room
			bob := f.identity("bob") // not a member
			f.lk.setRoom(room, fakeParticipant{identity: bob, joinedMs: 1_000})
			f.sweep(t)
			tt.break_(f)
			for i := 0; i < 4; i++ {
				f.clock.Advance(voicePollInterval)
				before := f.lk.count("ListParticipants")
				f.pass(t)
				if f.lk.count("ListParticipants") != before+1 {
					t.Fatalf("pass %d did not poll the room: it dropped out of use with someone still in it", i)
				}
				if got := f.who(t, f.ops); got != "" && got != "-" {
					t.Fatalf("pass %d showed %q", i, got)
				}
				if !f.lk.in(room, bob) {
					t.Fatalf("pass %d: the fake dropped bob although nothing could remove him", i)
				}
			}
			if d := f.srv.voice.nextDelay(); d != voicePollInterval {
				t.Errorf("next delay = %v, want the polling interval", d)
			}
			tt.mend(f)
			f.clock.Advance(voicePollInterval)
			f.pass(t)
			if f.lk.in(room, bob) {
				t.Error("bob was not removed once it was possible")
			}
			// With him gone the room is no longer in use.
			f.clock.Advance(voicePollInterval)
			f.pass(t)
			if f.srv.voice.anyInUse() {
				t.Error("the empty room is still in use")
			}
		})
	}
}

// Disabling a principal does not depend on knowing the principal's rooms. If
// that read fails, she still leaves presence at once, and the next pass removes
// her by entitlement. (This was the revoke-all test before issue #161 removed
// the bar; a revoke-all now rotates the rooms her credentials held and reads no
// room list, see TestVoiceHookRotation.)
func TestVoiceDisableWhenTheRoomListCannotBeRead(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	ann := f.identity("ann")
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000, published: true}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann*,ann2" {
		t.Fatalf("before: %q", got)
	}
	f.srv.voice.memberRooms = func(context.Context, int64) ([]store.VoiceRoom, error) { return nil, errors.New("store unavailable") }
	before := f.lk.count("RemoveParticipant")
	if code := f.doOrdered(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", f.ids["ann"]), f.rootTok); code != 204 {
		t.Fatalf("disable = %d: a store failure in the voice hook must not fail the request", code)
	}
	if got := f.who(t, f.ops); got != "ann2" {
		t.Errorf("right after disable: %q, want ann out of presence at once", got)
	}
	if f.lk.count("RemoveParticipant") != before {
		t.Fatal("RemoveParticipant was called although the rooms could not be read")
	}
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if f.lk.in(room, ann) {
		t.Error("the pass did not remove the disabled ann")
	}
	if got := f.who(t, f.ops); got != "ann2" {
		t.Errorf("after the pass: %q", got)
	}
	var left, removed int
	for _, e := range f.audit(t) {
		if e.Actor == f.actor("ann") && e.Action == store.AuditVoiceLeft {
			left++
		}
		if e.Action == store.AuditVoiceParticipantRemoved && strings.Contains(e.Detail, "reason="+voiceReasonDisabled) {
			removed++
		}
	}
	if left != 1 || removed != 1 {
		t.Errorf("voice_left for ann = %d, removals for a disabled principal = %d; want one of each", left, removed)
	}
}

// Asking for sessions in a loop does not make the poller run faster than its
// interval: only a room the loop was not watching wakes it.
func TestVoicePollerSessionsDoNotSpinTheLoop(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	r, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(f.srv.voice.wakeLoop); n != 0 {
		t.Fatalf("wake-ups pending before any session = %d", n)
	}
	f.srv.voice.noteSession(r)
	if n := len(f.srv.voice.wakeLoop); n != 1 {
		t.Fatalf("the first session for an idle room left %d wake-ups, want 1", n)
	}
	<-f.srv.voice.wakeLoop // the loop takes it
	for i := 0; i < 100; i++ {
		f.srv.voice.noteSession(r)
	}
	if n := len(f.srv.voice.wakeLoop); n != 0 {
		t.Errorf("100 more sessions for a room already in use left %d wake-ups, want 0", n)
	}
	// Once the room has gone quiet again, the next session wakes the loop.
	f.clock.Advance(voiceSessionRecent + time.Second)
	f.srv.voice.noteSession(r)
	if n := len(f.srv.voice.wakeLoop); n != 1 {
		t.Errorf("a session after the room went idle left %d wake-ups, want 1", n)
	}
}

// A member removed while a pass is in flight is not put back by that pass.
// The pass had already read her as entitled; without the eviction count it
// then installed what it saw, showing her again after the 204 and auditing a
// join that never happened.
func TestVoicePollerRemovalDuringAPassIsNotUndone(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	ann := f.identity("ann")
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann,ann2" {
		t.Fatalf("before: %q", got)
	}
	// The removal lands in the middle of the next pass: after the pass has
	// read ann's entitlement, before it applies what it saw. LiveKit is slow
	// to drop her, so the pass's own list still had her.
	real := f.srv.voice.entitle
	fired := false
	f.srv.voice.entitle = func(ctx context.Context, identity string, channelID int64) (int64, string, error) {
		id, reason, err := real(ctx, identity, channelID)
		if identity == ann && !fired {
			fired = true
			if code := f.doOrdered(t, "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok); code != 204 {
				t.Errorf("removal = %d", code)
			}
		}
		return id, reason, err // the answer from before the removal
	}
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if !fired {
		t.Fatal("the removal never ran")
	}
	if got := f.who(t, f.ops); got != "ann2" {
		t.Errorf("after the pass that was in flight: %q, want ann2 only", got)
	}
	f.srv.voice.entitle = real
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann2" {
		t.Errorf("a pass later: %q", got)
	}
	var mine []string
	for _, e := range f.audit(t) {
		if e.Actor == f.actor("ann") && strings.HasPrefix(e.Action, "voice_") {
			mine = append(mine, e.Action)
		}
	}
	if got := strings.Join(mine, ","); got != store.AuditVoiceJoined+","+store.AuditVoiceLeft {
		t.Errorf("ann's voice events = %s, want one join and one leave", got)
	}
}

// A person removed before any pass had seen them was still removed, and the
// audit says so; someone who was not connected at all is not reported removed.
func TestVoiceImmediateRemovalIsAuditedWhenLiveKitHadThem(t *testing.T) {
	for _, connected := range []bool{true, false} {
		t.Run(fmt.Sprintf("connected=%v", connected), func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			room := f.inUse(t, f.ops)
			if connected {
				f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
			} else {
				f.lk.setRoom(room)
			}
			// No pass: the poller has not seen her.
			if code := f.doOrdered(t, "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok); code != 204 {
				t.Fatalf("removal = %d", code)
			}
			if f.lk.count("RemoveParticipant") != 1 {
				t.Fatalf("RemoveParticipant calls = %d, want 1", f.lk.count("RemoveParticipant"))
			}
			want := 0
			if connected {
				want = 1
			}
			if n := len(f.audits(t, store.AuditVoiceParticipantRemoved)); n != want {
				t.Errorf("voice_participant_removed = %d, want %d", n, want)
			}
		})
	}
}

// Recovery from an outage through a sweep does not show what was seen before
// the outage. Presence is available again as soon as LiveKit answers, but a
// room shows nobody until it has been read.
func TestVoicePollerRecoveryThroughASweepShowsNobodyUntilRead(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000, published: true})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann*" {
		t.Fatalf("before: %q", got)
	}
	f.lk.setListStatus(http.StatusServiceUnavailable)
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if s := f.snap(t, f.ops); s.Available || len(s.Rooms) != 0 {
		t.Fatalf("during the outage: %+v", s)
	}
	// She leaves during the outage. LiveKit comes back and the next thing
	// that reaches it is a sweep.
	f.lk.setRoom(room)
	f.lk.setListStatus(0)
	f.clock.Advance(voiceSweepInterval + voiceBackoffMax)
	f.sweep(t)
	s := f.snap(t, f.ops)
	if !s.Available {
		t.Fatal("presence is not available after LiveKit answered the sweep")
	}
	if got := f.who(t, f.ops); got != "" && got != "-" {
		t.Errorf("after the sweep, before any pass: %q, want nobody (she left during the outage)", got)
	}
	f.pass(t)
	if got := f.who(t, f.ops); got != "" && got != "-" {
		t.Errorf("after the pass: %q", got)
	}
	if n := len(f.audits(t, store.AuditVoiceLeft)); n != 1 {
		t.Errorf("voice_left = %d, want 1", n)
	}
}

// One room LiveKit cannot answer for is that room's problem. The others are
// still read and enforced, presence stays available, and the unreadable room
// shows nobody until it can be read again.
func TestVoicePollerOneUnreadableRoomIsNotAnOutage(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	bad, good := f.inUse(t, f.ops), f.inUse(t, f.ops2)
	f.lk.setRoom(bad, fakeParticipant{identity: f.identity("ann2"), joinedMs: 1_000})
	f.lk.setRoom(good, fakeParticipant{identity: f.identity("ann"), joinedMs: 2_000})
	f.pass(t)
	if f.who(t, f.ops) != "ann2" || f.who(t, f.ops2) != "ann" {
		t.Fatalf("before: %q / %q", f.who(t, f.ops), f.who(t, f.ops2))
	}
	f.lk.config(func(l *scriptedLiveKit) { l.listFail = map[string]bool{bad: true} })
	// Someone who must not be there walks into the good room.
	f.lk.setRoom(good, fakeParticipant{identity: f.identity("ann"), joinedMs: 2_000}, fakeParticipant{identity: f.identity("bob"), joinedMs: 3_000})
	for i := 0; i < 3; i++ {
		f.clock.Advance(voicePollInterval)
		before := f.lk.count("ListParticipants")
		f.pass(t)
		if got := f.lk.count("ListParticipants") - before; got != 2 {
			t.Fatalf("pass %d polled %d rooms, want both: the unreadable room must not hold the other back", i, got)
		}
	}
	if f.lk.in(good, f.identity("bob")) {
		t.Error("the non-member in the readable room was not removed")
	}
	if s := f.snap(t, f.ops2); !s.Available || f.who(t, f.ops2) != "ann" {
		t.Errorf("readable room: available=%v who=%q", s.Available, f.who(t, f.ops2))
	}
	if got := f.who(t, f.ops); got != "" && got != "-" {
		t.Errorf("unreadable room shows %q, want nobody", got)
	}
	if n := len(f.audits(t, store.AuditVoiceEnforcementUnavailable)); n != 0 {
		t.Errorf("voice_enforcement_unavailable = %d, want 0: LiveKit was answering", n)
	}
	// It becomes readable again: its occupant is shown, with no second join.
	f.lk.config(func(l *scriptedLiveKit) { l.listFail = nil })
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann2" {
		t.Errorf("once readable again: %q", got)
	}
	joins := 0
	for _, e := range f.audits(t, store.AuditVoiceJoined) {
		if e.Actor == f.actor("ann2") {
			joins++
		}
	}
	if joins != 1 {
		t.Errorf("voice_joined for ann2 = %d, want 1", joins)
	}
	// Every room unreadable is an outage.
	f.lk.setListStatus(http.StatusServiceUnavailable)
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if s := f.snap(t, f.ops2); s.Available {
		t.Error("presence still available with every room unreadable")
	}
}

// The removal hooks do their work even if the operator's client has gone:
// the change is committed, and what follows from it is not optional.
func TestVoiceHooksIgnoreTheCallersCancellation(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
	f.pass(t)
	// ann holds a session, so losing her credentials rotates the room; ann2
	// does not, so her removal is by name.
	f.holder(t, "ann", f.ops)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann2"]); err != nil {
		t.Fatal(err)
	}
	f.srv.voiceMemberRemoved(gone, f.ops.ID, f.ids["ann2"])
	if f.lk.in(room, f.identity("ann2")) {
		t.Errorf("ann2 still connected after the member-removed hook ran on a cancelled context")
	}
	if _, err := f.srv.store.RevokeAllCredentials(context.Background(), "system", f.ids["ann"]); err != nil {
		t.Fatal(err)
	}
	f.srv.voicePrincipalLostAccess(gone, f.ids["ann"], voiceReasonCredsRevoked)
	if f.lk.has(room) || f.lk.callsFor("DeleteRoom", room) != 1 {
		t.Errorf("the old room was not deleted by the hook on a cancelled context")
	}
	if f.lk.in(room, f.identity("ann")) || f.lk.in(room, f.identity("ann2")) {
		t.Errorf("still connected after the hooks ran on a cancelled context: ann=%v ann2=%v", f.lk.in(room, f.identity("ann")), f.lk.in(room, f.identity("ann2")))
	}
	if got := f.who(t, f.ops); got != "" && got != "-" {
		t.Errorf("presence = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Room rotation (issue #161)

// voiceTrigger is one of the four ways a holder stops being valid that a hook
// handles in the same request, each as the operator would do it.
type voiceTrigger struct {
	name     string
	method   string
	path     func(t *testing.T, f *presenceFixture, who string) string
	code     int
	channels []string // channels whose rooms rotate when `who` holds them all
	reason   string
}

func voiceTriggers() []voiceTrigger {
	return []voiceTrigger{
		{"member removed from ops", "DELETE", func(t *testing.T, f *presenceFixture, who string) string {
			return fmt.Sprintf("/v1/channels/ops/members/%d", f.ids[who])
		}, 204, []string{"ops"}, store.VoiceRotateMemberRemoved},
		{"principal disabled", "POST", func(t *testing.T, f *presenceFixture, who string) string {
			return fmt.Sprintf("/v1/principals/%d/disable", f.ids[who])
		}, 204, []string{"ops", "ops2"}, store.VoiceRotatePrincipalDisabled},
		{"all credentials revoked", "POST", func(t *testing.T, f *presenceFixture, who string) string {
			return fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.ids[who])
		}, 200, []string{"ops", "ops2"}, store.VoiceRotateRevoked},
		{"one credential revoked", "DELETE", func(t *testing.T, f *presenceFixture, who string) string {
			return fmt.Sprintf("/v1/credentials/%d", f.credID(t, who))
		}, 204, []string{"ops", "ops2"}, store.VoiceRotateRevoked},
	}
}

// TestVoiceHookRotation: each trigger rotates exactly the rooms its principal
// held a session for, deletes the old room in LiveKit before the HTTP response
// is written, never fails the request (LiveKit refusing included), leaves
// presence empty at once, writes one voice_room_rotated row per rotation with
// a reason and no room name, and a later sweep finishes a deletion that
// failed.
func TestVoiceHookRotation(t *testing.T) {
	for _, tt := range voiceTriggers() {
		for _, deleteFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/delete fails=%v", tt.name, deleteFails), func(t *testing.T) {
				f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
				ann, ann2 := f.identity("ann"), f.identity("ann2")
				ops := f.inUse(t, f.ops)
				ops2 := f.inUse(t, f.ops2)
				olds := map[string]string{"ops": ops, "ops2": ops2}
				f.holder(t, "ann", f.ops)
				f.holder(t, "ann2", f.ops)
				f.holder(t, "ann", f.ops2)
				f.lk.setRoom(ops, fakeParticipant{identity: ann, joinedMs: 1_000, published: true}, fakeParticipant{identity: ann2, joinedMs: 2_000})
				f.lk.setRoom(ops2, fakeParticipant{identity: ann, joinedMs: 3_000})
				f.pass(t)
				if got := f.who(t, f.ops); got != "ann*,ann2" {
					t.Fatalf("before: %q", got)
				}
				base := len(f.lk.calls())
				if deleteFails {
					f.lk.setDeleteStatus(http.StatusInternalServerError)
				}

				if code := f.doOrdered(t, tt.method, tt.path(t, f, "ann"), f.rootTok); code != tt.code {
					t.Fatalf("status = %d, want %d (LiveKit refusing must not fail the request)", code, tt.code)
				}

				var deleted []string
				respAt := 0
				for _, c := range f.lk.calls()[base:] {
					switch c.method {
					case "DeleteRoom":
						if respAt != 0 {
							t.Errorf("DeleteRoom (seq %d) came after the response (seq %d)", c.seq, respAt)
						}
						deleted = append(deleted, c.room)
					case "RemoveParticipant":
						t.Errorf("RemoveParticipant %q: a holder's room is rotated, nobody is removed by name", c.identity)
					case "CreateRoom":
						t.Errorf("CreateRoom %q: the new room is created by the next session, not by the hook", c.room)
					case "http-response":
						respAt = c.seq
					}
				}
				var want []string
				for _, ch := range tt.channels {
					want = append(want, olds[ch])
				}
				slices.Sort(deleted)
				slices.Sort(want)
				if !slices.Equal(deleted, want) {
					t.Fatalf("DeleteRoom for %d rooms, want the rooms of %v", len(deleted), tt.channels)
				}
				if respAt == 0 {
					t.Fatal("no response marker")
				}

				for name, ch := range map[string]store.Channel{"ops": f.ops, "ops2": f.ops2} {
					cur := f.room(t, ch)
					if rotated := slices.Contains(tt.channels, name); rotated == (cur == olds[name]) {
						t.Errorf("%s: rotated=%v but the room is %s", name, rotated, map[bool]string{true: "unchanged", false: "new"}[cur == olds[name]])
					}
					if slices.Contains(tt.channels, name) {
						if f.lk.has(olds[name]) == !deleteFails {
							t.Errorf("%s: old room in LiveKit = %v with delete failing=%v", name, f.lk.has(olds[name]), deleteFails)
						}
						// Presence shows the new room, empty.
						if got := f.who(t, ch); got != "" {
							t.Errorf("%s: presence right after = %q, want nobody", name, got)
						}
					}
				}
				// A channel whose room did not rotate keeps its people.
				if !slices.Contains(tt.channels, "ops2") {
					if got := f.who(t, f.ops2); got != "ann" {
						t.Errorf("ops2 presence = %q, want it untouched", got)
					}
				}

				// One audit row per rotation, with the reason and no room name.
				rows := f.audits(t, store.AuditVoiceRoomRotated)
				if len(rows) != len(tt.channels) {
					t.Fatalf("voice_room_rotated rows = %d, want %d", len(rows), len(tt.channels))
				}
				subjects := map[string]bool{}
				for _, e := range rows {
					subjects[e.Subject] = true
					if e.Actor != "system" || e.Detail != "reason="+tt.reason {
						t.Errorf("rotation row = %+v, want actor system and reason=%s", e, tt.reason)
					}
				}
				for _, ch := range tt.channels {
					id := f.ops.ID
					if ch == "ops2" {
						id = f.ops2.ID
					}
					if !subjects[fmt.Sprintf("channel:%d", id)] {
						t.Errorf("no rotation row for channel %s", ch)
					}
				}

				if deleteFails {
					// A later sweep finishes the deletion LiveKit refused.
					f.lk.setDeleteStatus(0)
					f.sweep(t)
					for _, ch := range tt.channels {
						if f.lk.has(olds[ch]) {
							t.Errorf("%s: the sweep did not delete the retired room", ch)
						}
						if n := f.lk.callsFor("DeleteRoom", olds[ch]); n != 2 {
							t.Errorf("%s: DeleteRoom calls = %d, want the hook's and the sweep's", ch, n)
						}
					}
				}
			})
		}
	}
}

// A person who was never issued a session for the current room rotates
// nothing and disconnects nobody else, whichever way they lose their place.
func TestVoiceHookNeverIssuedRotatesNothing(t *testing.T) {
	for _, tt := range voiceTriggers() {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			ann2 := f.identity("ann2")
			ops := f.inUse(t, f.ops)
			f.holder(t, "ann2", f.ops)
			f.lk.setRoom(ops, fakeParticipant{identity: ann2, joinedMs: 2_000})
			f.pass(t)
			base := len(f.lk.calls())

			if code := f.doOrdered(t, tt.method, tt.path(t, f, "ann"), f.rootTok); code != tt.code {
				t.Fatalf("status = %d, want %d", code, tt.code)
			}
			for _, c := range f.lk.calls()[base:] {
				switch c.method {
				case "DeleteRoom", "CreateRoom":
					t.Errorf("%s %q: nothing may be rotated or created", c.method, c.room)
				case "RemoveParticipant":
					if c.identity == ann2 {
						t.Errorf("ann2 was removed")
					}
				}
			}
			if f.room(t, f.ops) != ops {
				t.Error("the room was renamed")
			}
			if !f.lk.in(ops, ann2) || f.who(t, f.ops) != "ann2" {
				t.Errorf("ann2 was disturbed: in room %v, presence %q", f.lk.in(ops, ann2), f.who(t, f.ops))
			}
			if n := len(f.audits(t, store.AuditVoiceRoomRotated)); n != 0 {
				t.Errorf("voice_room_rotated rows = %d, want 0", n)
			}
		})
	}
}

// Revoking a credential nobody was issued a session under rotates nothing,
// even for a principal who holds a session under another credential.
func TestVoiceRevokingAnUnusedCredentialRotatesNothing(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	ops := f.inUse(t, f.ops)
	f.holder(t, "ann", f.ops)
	spare, _, err := f.srv.store.CreateCredential(ctx, "system", f.ids["ann"], "spare", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
	f.pass(t)
	if code := f.doOrdered(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", spare.ID), f.rootTok); code != 204 {
		t.Fatalf("status = %d", code)
	}
	if f.lk.count("DeleteRoom") != 0 || f.room(t, f.ops) != ops || len(f.audits(t, store.AuditVoiceRoomRotated)) != 0 {
		t.Error("revoking an unused credential rotated the room")
	}
	// Replacing the credential the session was issued under does rotate it.
	if code := f.doOrdered(t, "POST", fmt.Sprintf("/v1/credentials/%d/rotate", f.credID(t, "ann")), f.rootTok); code != 201 {
		t.Fatalf("rotate status = %d", code)
	}
	if f.lk.callsFor("DeleteRoom", ops) != 1 || f.room(t, f.ops) == ops {
		t.Error("rotating the session's credential did not rotate the room")
	}
}

// credentialWithExpiry gives who a second credential that expires at exp and
// returns its token.
func (f *presenceFixture) credentialWithExpiry(t *testing.T, who string, exp time.Time) (int64, string) {
	t.Helper()
	cred, tok, err := f.srv.store.CreateCredential(context.Background(), "system", f.ids[who], "short", &exp)
	if err != nil {
		t.Fatal(err)
	}
	return cred.ID, tok
}

// TestVoiceSweepRotation: the invariant is also checked at every sweep, which
// is what covers expiry, a restart, and a failed DeleteRoom.
func TestVoiceSweepRotation(t *testing.T) {
	t.Run("an expired credential's room is rotated within one sweep", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		ops := f.inUse(t, f.ops)
		exp := time.Now().Add(time.Hour)
		_, tok := f.credentialWithExpiry(t, "ann", exp)
		if res := f.callREST(t, "POST", "/v1/channels/ops/voice/session", tok, ""); res.status != 200 {
			t.Fatalf("session = %d %s", res.status, res.body)
		}
		f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
		f.clock.Set(exp.Add(-time.Minute))
		f.sweep(t)
		if f.lk.count("DeleteRoom") != 0 || f.room(t, f.ops) != ops {
			t.Fatal("rotated before the credential expired")
		}
		f.clock.Set(exp.Add(time.Minute))
		f.sweep(t)
		if f.lk.callsFor("DeleteRoom", ops) != 1 || f.lk.has(ops) || f.room(t, f.ops) == ops {
			t.Errorf("after expiry: DeleteRoom calls %d, old room present %v, room unchanged %v",
				f.lk.callsFor("DeleteRoom", ops), f.lk.has(ops), f.room(t, f.ops) == ops)
		}
		rows := f.audits(t, store.AuditVoiceRoomRotated)
		if len(rows) != 1 || rows[0].Detail != "reason="+store.VoiceRotateExpired {
			t.Errorf("rotation rows = %+v", rows)
		}
		// Another sweep finds nothing more to do.
		f.sweep(t)
		if len(f.audits(t, store.AuditVoiceRoomRotated)) != 1 {
			t.Error("a second sweep rotated again")
		}
	})

	t.Run("a restart finds a room whose holder was invalidated while the server was down", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		ops := f.room(t, f.ops)
		f.holder(t, "ann", f.ops)
		f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
		// No hook runs: the change is made behind the server's back.
		if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann"]); err != nil {
			t.Fatal(err)
		}
		fresh := newVoicePoller(f.srv)
		fresh.now = f.clock.Now
		if !fresh.runSweep(context.Background()) {
			t.Fatal("the sweep did not run")
		}
		if f.lk.callsFor("DeleteRoom", ops) != 1 || f.lk.has(ops) || f.room(t, f.ops) == ops {
			t.Error("the restarted poller did not rotate and delete the room")
		}
		if rows := f.audits(t, store.AuditVoiceRoomRotated); len(rows) != 1 || rows[0].Detail != "reason="+store.VoiceRotateMemberRemoved {
			t.Errorf("rotation rows = %+v", rows)
		}
	})

	t.Run("a retired room LiveKit still lists is deleted, and left alone once it is gone", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		ctx := context.Background()
		stuck := f.holder(t, "ann", f.ops)
		unlisted := f.holder(t, "ann", f.ops2)
		f.lk.setRoom(stuck.RoomName, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
		// A crash between the rotation and the DeleteRoom: the store has
		// retired both rooms, LiveKit has only one of them.
		for _, r := range []store.VoiceRoom{stuck, unlisted} {
			if _, ok, err := f.srv.store.RotateVoiceRoom(ctx, r.ID, store.VoiceRotateMemberRemoved); err != nil || !ok {
				t.Fatalf("rotate: %v %v", ok, err)
			}
		}
		f.lk.setDeleteStatus(http.StatusInternalServerError)
		f.sweep(t)
		if !f.lk.has(stuck.RoomName) || f.lk.callsFor("DeleteRoom", stuck.RoomName) != 1 {
			t.Fatal("the sweep did not try to delete the listed retired room")
		}
		f.lk.setDeleteStatus(0)
		f.sweep(t)
		if f.lk.has(stuck.RoomName) || f.lk.callsFor("DeleteRoom", stuck.RoomName) != 2 {
			t.Fatal("the sweep did not finish the deletion")
		}
		f.sweep(t)
		f.sweep(t)
		if n := f.lk.callsFor("DeleteRoom", stuck.RoomName); n != 2 {
			t.Errorf("DeleteRoom calls for a room LiveKit no longer lists = %d, want it left alone", n)
		}
		if n := f.lk.callsFor("DeleteRoom", unlisted.RoomName); n != 0 {
			t.Errorf("DeleteRoom calls for a retired room LiveKit never listed = %d", n)
		}
	})

	t.Run("a sweep rotates while LiveKit is down and deletes once it is back", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		ops := f.room(t, f.ops)
		f.holder(t, "ann", f.ops)
		f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
		if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann"]); err != nil {
			t.Fatal(err)
		}
		f.lk.setListStatus(http.StatusServiceUnavailable)
		f.sweep(t)
		if f.room(t, f.ops) == ops {
			t.Error("the room was not rotated while LiveKit was down: the invariant needs only the store")
		}
		if !f.lk.has(ops) {
			t.Fatal("the fake lost the room")
		}
		f.lk.setListStatus(0)
		f.clock.Advance(time.Minute) // past the outage backoff
		f.sweep(t)
		if f.lk.has(ops) {
			t.Error("the retired room was not deleted once LiveKit answered")
		}
	})

	t.Run("a store failure on the invariant is retried soon and does not stop the rest", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		ops := f.inUse(t, f.ops)
		f.holder(t, "ann", f.ops)
		f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
		if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann"]); err != nil {
			t.Fatal(err)
		}
		real := f.srv.voice.invalid
		f.srv.voice.invalid = func(context.Context, time.Time) ([]store.VoiceViolation, error) {
			return nil, errors.New("store unavailable")
		}
		start := f.clock.Now()
		f.sweep(t)
		f.srv.voice.mu.Lock()
		next := f.srv.voice.nextSweep
		marked := f.srv.voice.rooms[ops] != nil && f.srv.voice.rooms[ops].marked
		f.srv.voice.mu.Unlock()
		if want := start.Add(voiceSweepRetry); !next.Equal(want) {
			t.Errorf("next sweep = %v, want %v: a failed check must not wait a full interval", next.Sub(start), voiceSweepRetry)
		}
		if !marked {
			t.Error("the rest of the sweep (marking rooms LiveKit lists) did not run")
		}
		if f.room(t, f.ops) != ops {
			t.Error("rotated although the check failed")
		}
		f.srv.voice.invalid = real
		f.clock.Advance(voiceSweepRetry)
		f.sweep(t)
		if f.room(t, f.ops) == ops || f.lk.has(ops) {
			t.Error("the retry did not rotate and delete the room")
		}
	})
}

// After a rotation: the next session names the new room and CreateRoom is
// called for it and never again for the old name; the per-pass removal still
// works in the new room; a participant LiveKit still lists in the old room is
// not shown.
func TestVoiceAfterRotation(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	first := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	decodeSession(t, f.session(t, "ann2", "ops"))
	f.lk.setRoom(first, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann,ann2" {
		t.Fatalf("before: %q", got)
	}
	// ann2 leaves the channel; LiveKit refuses to delete, so the old room
	// lingers with both of them in it.
	f.lk.setDeleteStatus(http.StatusInternalServerError)
	if code := f.doOrdered(t, "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann2"]), f.rootTok); code != 204 {
		t.Fatalf("remove member = %d", code)
	}
	if !f.lk.has(first) {
		t.Fatal("the fake deleted the room although it was told to fail")
	}
	creates := f.lk.callsFor("CreateRoom", first)

	// The old room is not shown even though LiveKit lists people in it, and
	// the poller does not read it any more.
	lists := f.lk.callsFor("ListParticipants", first)
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if got := f.who(t, f.ops); got != "" {
		t.Errorf("presence = %q, want nobody (the people in the old room are not shown)", got)
	}
	if n := f.lk.callsFor("ListParticipants", first); n != lists {
		t.Errorf("the poller read the retired room %d more times", n-lists)
	}

	// ann's next session is for the new room, which is created in LiveKit.
	next := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	if next == first || !voiceRoomNameRE.MatchString(next) {
		t.Fatalf("new session names %q after rotating %q", next, first)
	}
	if f.lk.callsFor("CreateRoom", next) != 1 || f.lk.callsFor("CreateRoom", first) != creates {
		t.Errorf("CreateRoom: new room %d times, old room %d more times", f.lk.callsFor("CreateRoom", next), f.lk.callsFor("CreateRoom", first)-creates)
	}
	// The per-pass removal of an unentitled participant works in the new room.
	f.lk.setRoom(next, fakeParticipant{identity: f.identity("ann"), joinedMs: 5_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 6_000})
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	if f.lk.in(next, f.identity("ann2")) || f.who(t, f.ops) != "ann" {
		t.Errorf("ann2 in the new room = %v, presence %q; want her removed and only ann shown", f.lk.in(next, f.identity("ann2")), f.who(t, f.ops))
	}
	// The sweep deletes the old room once LiveKit lets it, and never creates it.
	f.lk.setDeleteStatus(0)
	f.sweep(t)
	if f.lk.has(first) {
		t.Error("the sweep did not delete the retired room")
	}
	if f.lk.callsFor("CreateRoom", first) != creates {
		t.Error("the old room was created again")
	}
	// The member who is still entitled can rejoin; the removed one is refused.
	if res := f.session(t, "ann2", "ops"); res.status != 404 {
		t.Errorf("removed member's session = %d", res.status)
	}
}

// TestVoiceRotationKeepsSecrets: across a rotation neither room name, no
// token, and not the API secret appears in an audit row, a captured log line
// or a presence document.
func TestVoiceRotationKeepsSecrets(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	g1 := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0]
	g2 := decodeSession(t, f.session(t, "ann2", "ops")).Rooms[0]
	f.lk.setRoom(g1.Room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000, published: true}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
	f.pass(t)
	f.lk.setDeleteStatus(http.StatusInternalServerError) // exercise the failure log too
	if code := f.doOrdered(t, "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann2"]), f.rootTok); code != 204 {
		t.Fatalf("remove member = %d", code)
	}
	f.lk.setDeleteStatus(0)
	f.sweep(t)
	g3 := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0]
	if g3.Room == g1.Room {
		t.Fatal("room not rotated")
	}
	f.lk.setRoom(g3.Room, fakeParticipant{identity: f.identity("ann"), joinedMs: 9_000})
	f.clock.Advance(voicePollInterval)
	f.pass(t)

	needles := []string{g1.Room, g3.Room, g1.Token, g2.Token, g3.Token, voiceTestSecret}
	var haystack strings.Builder
	for _, e := range f.audit(t) {
		fmt.Fprintf(&haystack, "%s %s %s %s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	haystack.WriteString(f.logs.buf.String())
	for _, ch := range []store.Channel{f.ops, f.ops2} {
		raw, err := json.Marshal(f.snap(t, ch))
		if err != nil {
			t.Fatal(err)
		}
		haystack.Write(raw)
	}
	for i, n := range needles {
		if strings.Contains(haystack.String(), n) {
			t.Errorf("needle %d (%d chars) appears in an audit row, log line or presence document", i, len(n))
		}
	}
	if !strings.Contains(haystack.String(), "voice_room_rotated") {
		t.Error("the run did not rotate a room")
	}
}

// A room rotated away while a pass is reading it: what the pass saw is in a
// room that no longer exists, so it reports nothing. Without this a person
// who joined the old room in that instant would be audited as joining after
// everyone in it had been recorded as leaving.
func TestVoicePollerPassInFlightDuringARotationReportsNothing(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	ann, ann2 := f.identity("ann"), f.identity("ann2")
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000})
	f.pass(t)
	if got := f.who(t, f.ops); got != "ann" {
		t.Fatalf("before: %q", got)
	}
	// ann2 joins; while the pass is checking her, the room is rotated away.
	f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000}, fakeParticipant{identity: ann2, joinedMs: 2_000, published: true})
	real := f.srv.voice.entitle
	fired := false
	f.srv.voice.entitle = func(ctx context.Context, identity string, channelID int64) (int64, string, error) {
		if identity == ann2 && !fired {
			fired = true
			f.srv.voice.forgetRoom(ctx, room)
		}
		return real(ctx, identity, channelID)
	}
	f.clock.Advance(voicePollInterval)
	f.pass(t)
	f.srv.voice.entitle = real
	if !fired {
		t.Fatal("the rotation never ran")
	}
	if got := f.who(t, f.ops); got != "" && got != "-" {
		t.Errorf("presence after the rotation = %q, want nobody", got)
	}
	var events []string
	for _, e := range f.audit(t) {
		if strings.HasPrefix(e.Action, "voice_") && e.Action != store.AuditVoiceSessionIssued {
			events = append(events, e.Actor+" "+e.Action)
		}
	}
	want := []string{f.actor("ann") + " " + store.AuditVoiceJoined, f.actor("ann") + " " + store.AuditVoiceLeft}
	if !slices.Equal(events, want) {
		t.Errorf("voice events = %v, want %v: nothing from the pass that was in flight", events, want)
	}
}

// A retired room that could not be deleted is not left for half a minute:
// until it is gone, the person who lost their place is still connected to it
// and nobody is shown in presence. The sweep is brought forward, and keeps
// coming soon for as long as the delete fails. (Security review of #166, S1.)
func TestVoiceFailedDeleteBringsTheSweepForward(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	ann, ann2 := f.identity("ann"), f.identity("ann2")
	old := f.inUse(t, f.ops)
	f.holder(t, "ann", f.ops)
	f.holder(t, "ann2", f.ops)
	f.sweep(t)
	f.lk.setRoom(old, fakeParticipant{identity: ann, joinedMs: 1_000}, fakeParticipant{identity: ann2, joinedMs: 2_000})
	f.pass(t)
	if d := f.srv.voice.nextDelay(); d > voicePollInterval {
		t.Fatalf("next delay before = %v", d)
	}

	f.lk.setDeleteStatus(http.StatusInternalServerError)
	if rec := f.do(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	if !f.lk.in(old, ann) {
		t.Fatal("the fake deleted the room although it answered 500")
	}
	f.srv.voice.mu.Lock()
	due := f.srv.voice.nextSweep.Sub(f.clock.Now())
	f.srv.voice.mu.Unlock()
	if due > voiceSweepRetry {
		t.Fatalf("next sweep in %v after a failed delete, want within %v", due, voiceSweepRetry)
	}

	// Still failing: the sweep tries, fails, and is due soon again.
	deletes := f.lk.callsFor("DeleteRoom", old)
	f.clock.Advance(voiceSweepRetry)
	f.srv.voice.tick(ctx)
	if got := f.lk.callsFor("DeleteRoom", old); got != deletes+1 {
		t.Fatalf("DeleteRoom calls after the retry interval = %d, want %d", got, deletes+1)
	}
	f.srv.voice.mu.Lock()
	due = f.srv.voice.nextSweep.Sub(f.clock.Now())
	f.srv.voice.mu.Unlock()
	if due > voiceSweepRetry {
		t.Fatalf("next sweep in %v after a second failed delete, want within %v", due, voiceSweepRetry)
	}

	// LiveKit answers: the room goes one retry interval later.
	f.lk.setDeleteStatus(0)
	f.clock.Advance(voiceSweepRetry)
	f.srv.voice.tick(ctx)
	if f.lk.has(old) {
		t.Error("the retired room is still in LiveKit one retry interval after it could be deleted")
	}
}

// A sweep's read of the holders failing is logged: while it fails, a
// credential expiring rotates nothing. (Security review of #166, S4.)
func TestVoiceSweepLogsAFailedHolderCheck(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.srv.voice.invalid = func(context.Context, time.Time) ([]store.VoiceViolation, error) {
		return nil, errors.New("store unavailable")
	}
	f.sweep(t)
	if !strings.Contains(f.logs.buf.String(), "could not check the holders") {
		t.Errorf("no log line for the failed check:\n%s", f.logs.buf.String())
	}
	if d := f.srv.voice.nextDelay(); d > voiceSweepRetry {
		t.Errorf("next delay = %v, want at most %v", d, voiceSweepRetry)
	}
}
