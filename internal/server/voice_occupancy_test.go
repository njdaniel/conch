package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Voice occupancy (issue #596, docs/design/voice-occupancy.md): the read, its
// socket, and what the poller wakes them for. The fixture is the presence
// fixture (voice_presence_fake_test.go); joinWithoutVoiceP adds a member of
// ops whose invite did not grant voice.

// joinWithoutVoiceP adds "vic", a member of ops through an operator's invite
// that does not grant voice (issue #437), to a presence fixture.
func (f *presenceFixture) joinWithoutVoiceP(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	inv, err := f.srv.store.CreateInvite(ctx, "system", f.root.ID,
		schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"ops"}}, store.InviteOptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	joined, err := f.srv.store.RedeemInvite(ctx, store.HashInviteCode(inv.Code), "vic", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if joined.Principal.VoiceAllowed {
		t.Fatal("the joiner holds voice")
	}
	f.tokens["vic"], f.ids["vic"] = joined.Token, joined.Principal.ID
}

// occ renders a document as the test reads it: "x" not configured, "-"
// unavailable, else "name:count" per channel in order, comma-separated ("" for
// no channels).
func (f *presenceFixture) occ(doc schema.VoiceOccupancyV1) string {
	switch {
	case !doc.Configured:
		return "x"
	case !doc.Available:
		return "-"
	}
	names := map[int64]string{f.ops.ID: "ops", f.ops2.ID: "ops2"}
	var out []string
	for _, c := range doc.Channels {
		n, ok := names[c.ChannelID]
		if !ok {
			n = fmt.Sprintf("channel%d", c.ChannelID)
		}
		out = append(out, fmt.Sprintf("%s:%d", n, c.InVoice))
	}
	return strings.Join(out, ",")
}

// getOccupancy reads GET /v1/voice/occupancy as who.
func (f *presenceFixture) getOccupancy(t *testing.T, who string) (wireResult, schema.VoiceOccupancyV1) {
	t.Helper()
	res := f.callREST(t, http.MethodGet, "/v1/voice/occupancy", f.tokens[who], "")
	var doc schema.VoiceOccupancyV1
	if res.status != http.StatusOK {
		return res, doc
	}
	dec := json.NewDecoder(strings.NewReader(res.body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("document does not validate: %v; %s", err, res.body)
	}
	return res, doc
}

// live makes the poller contact LiveKit, with ann and ann2 in ops's room and
// nobody in ops2's, and returns the two room names.
func (f *presenceFixture) live(t *testing.T) (ops, ops2 string) {
	t.Helper()
	ops, ops2 = f.inUse(t, f.ops), f.inUse(t, f.ops2)
	f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000})
	f.lk.setRoom(ops2)
	f.pass(t)
	return ops, ops2
}

// TestVoiceOccupancyEndpoint: the read, by caller. A channel the caller is not
// a member of is absent, never a zero; operators get no exemption; agents and
// people without voice are refused and audited; nothing reads LiveKit.
func TestVoiceOccupancyEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		opts       presenceOpts
		who        string
		wantStatus int
		wantCode   string
		want       string // the document, as occ renders it
		wantDenied int
	}{
		{"member of both channels, one with people", presenceOpts{auth: AuthRequired}, "ann", 200, "", "ops:2,ops2:0", 0},
		{"member of one channel: the other is absent", presenceOpts{auth: AuthRequired}, "ann2", 200, "", "ops:2", 0},
		{"member of nothing", presenceOpts{auth: AuthRequired}, "bob", 200, "", "", 0},
		{"operator who is a member of nothing", presenceOpts{auth: AuthRequired}, "root", 200, "", "", 0},
		{"agent member", presenceOpts{auth: AuthRequired}, "robo", 403, "forbidden", "", 1},
		{"member without voice", presenceOpts{auth: AuthRequired}, "vic", 403, schema.ErrorCodeVoiceNotAllowed, "", 1},
		{"disabled", presenceOpts{auth: AuthRequired}, "dora", 401, "unauthenticated", "", 0},
		{"unauthenticated", presenceOpts{auth: AuthRequired}, "", 401, "unauthenticated", "", 0},
		{"auth off", presenceOpts{auth: AuthOff}, "", 400, schema.ErrorCodeVoiceRequiresAuth, "", 0},
		{"not configured, member", presenceOpts{auth: AuthRequired, unconfigured: true}, "ann", 200, "", "x", 0},
		{"not configured, agent", presenceOpts{auth: AuthRequired, unconfigured: true}, "robo", 403, "forbidden", "", 1},
		{"not configured, member without voice", presenceOpts{auth: AuthRequired, unconfigured: true}, "vic", 403, schema.ErrorCodeVoiceNotAllowed, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, tt.opts)
			if tt.opts.auth == AuthRequired {
				f.joinWithoutVoiceP(t)
			}
			if !tt.opts.unconfigured {
				f.live(t)
			}
			calls := len(f.lk.calls())
			denied := len(f.audits(t, "access_denied"))
			res, doc := f.getOccupancy(t, tt.who)
			if res.status != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", res.status, tt.wantStatus, res.body)
			}
			if tt.wantCode != "" && errCode(t, res.body) != tt.wantCode {
				t.Errorf("code = %s, want %s", errCode(t, res.body), tt.wantCode)
			}
			if got := len(f.audits(t, "access_denied")) - denied; got != tt.wantDenied {
				t.Errorf("access_denied events = %d, want %d", got, tt.wantDenied)
			}
			if got := len(f.lk.calls()) - calls; got != 0 {
				t.Errorf("reading occupancy made %d LiveKit calls", got)
			}
			if tt.wantStatus != 200 {
				return
			}
			if got := f.occ(doc); got != tt.want {
				t.Errorf("document = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("an agent's refusal is audited as a voice refusal and names no channel", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		f.callREST(t, http.MethodGet, "/v1/voice/occupancy", f.tokens["robo"], "")
		var found bool
		for _, e := range f.audits(t, "access_denied") {
			if e.Actor == f.actor("robo") && e.Subject == "GET /v1/voice/occupancy" {
				found = true
				if !strings.Contains(e.Detail, "target=none") || !strings.Contains(e.Detail, "reason="+denyAgentVoice) {
					t.Errorf("detail = %q, want target=none reason=%s", e.Detail, denyAgentVoice)
				}
			}
		}
		if !found {
			t.Error("no access_denied row for the agent")
		}
	})
}

// TestVoiceOccupancyCountsAsPresence: a count is what the channel's presence
// lists: the viewer counted, nobody in a room that could not be read, nothing
// at all during an outage, and never a room with an audience (a net's), which
// the poller does not track or ask LiveKit about.
func TestVoiceOccupancyCountsAsPresence(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ops, ops2 := f.live(t)
	read := func() string {
		t.Helper()
		_, doc := f.getOccupancy(t, "ann")
		return f.occ(doc)
	}
	if got := read(); got != "ops:2,ops2:0" {
		t.Fatalf("start = %q", got)
	}
	if n := len(f.snap(t, f.ops).Rooms[0].Participants); n != 2 {
		t.Fatalf("presence lists %d, the count says 2", n)
	}

	// A net's room in ops, with people LiveKit lists: not counted.
	net := store.VoiceRoom{ID: 999, ChannelID: f.ops.ID, NetID: 3, RoomName: ops + "-net"}
	f.srv.voice.noteSession(net)
	f.lk.setRoom(net.RoomName, fakeParticipant{identity: f.identity("ann2"), joinedMs: 3_000}, fakeParticipant{identity: f.identity("bob"), joinedMs: 3_000})
	f.clock.Advance(testPassGap)
	f.pass(t)
	if got := read(); got != "ops:2,ops2:0" {
		t.Errorf("with a net room in use = %q, want it not counted", got)
	}
	if n := f.lk.callsFor("ListParticipants", net.RoomName); n != 0 {
		t.Errorf("the net room was read %d times", n)
	}

	// ann joins ops2 too: one person in two channels counts in each.
	f.lk.setRoom(ops2, fakeParticipant{identity: f.identity("ann"), joinedMs: 4_000})
	f.clock.Advance(testPassGap)
	f.pass(t)
	if got := read(); got != "ops:2,ops2:1" {
		t.Errorf("after ann joins ops2 = %q", got)
	}

	// A room that cannot be read shows nobody until it is read again.
	f.lk.config(func(l *scriptedLiveKit) { l.listFail = map[string]bool{ops: true} })
	f.clock.Advance(testPassGap)
	f.pass(t)
	if got := read(); got != "ops:0,ops2:1" {
		t.Errorf("with ops unreadable = %q", got)
	}
	f.lk.config(func(l *scriptedLiveKit) { l.listFail = nil })
	f.clock.Advance(testPassGap)
	f.pass(t)
	if got := read(); got != "ops:2,ops2:1" {
		t.Errorf("once readable again = %q", got)
	}

	// An outage reports no counts at all.
	f.lk.setListStatus(http.StatusServiceUnavailable)
	f.clock.Advance(testPassGap)
	f.pass(t)
	if got := read(); got != "-" {
		t.Errorf("during an outage = %q, want unavailable with no channels", got)
	}
}

// dialOccupancy opens the occupancy socket as token.
func dialOccupancy(t *testing.T, base, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hdr := http.Header{}
	if token != "" {
		hdr.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.Dial(ctx, base+"/v1/voice/occupancy/ws", &websocket.DialOptions{HTTPHeader: hdr}) //nolint:bodyclose // closed below
	if err == nil {
		t.Cleanup(func() { _ = conn.CloseNow() })
	} else if resp != nil {
		_ = resp.Body.Close()
	}
	return conn, resp, err
}

func readOccupancy(t *testing.T, conn *websocket.Conn) schema.VoiceOccupancyV1 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc schema.VoiceOccupancyV1
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("frame does not validate: %v; %s", err, raw)
	}
	return doc
}

// TestVoiceOccupancySocketStreams: a document on connect, a new one for every
// change to a count, and none for anything else. A frame sent for a change
// that moves no count (a transmit flip, a reconnection) would be read in
// place of the next step's, so each step's frame is exactly its own.
func TestVoiceOccupancySocketStreams(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ops, ops2 := f.inUse(t, f.ops), f.inUse(t, f.ops2)
	base := wsTestServer(t, f.srv)
	conn, _, err := dialOccupancy(t, base, f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	if got := f.occ(readOccupancy(t, conn)); got != "-" {
		t.Fatalf("on connect, before LiveKit has answered: %q", got)
	}
	steps := []struct {
		name   string
		script func()
		want   string
	}{
		{"first contact", func() {}, "ops:0,ops2:0"},
		{"nothing changed, then ann2 joins ops", func() {
			f.pass(t)
			f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann2"), joinedMs: 1_000, published: true, muted: true})
		}, "ops:1,ops2:0"},
		{"she talks, then ann joins ops2", func() {
			f.lk.update(ops, f.identity("ann2"), func(p *fakeParticipant) { p.muted = false })
			f.pass(t)
			f.lk.setRoom(ops2, fakeParticipant{identity: f.identity("ann"), joinedMs: 2_000})
		}, "ops:1,ops2:1"},
		{"ann2 reconnects, then ann joins ops", func() {
			f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann2"), joinedMs: 5_000, published: true})
			f.pass(t)
			f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann2"), joinedMs: 5_000, published: true}, fakeParticipant{identity: f.identity("ann"), joinedMs: 6_000})
		}, "ops:2,ops2:1"},
		{"ops cannot be read", func() { f.lk.config(func(l *scriptedLiveKit) { l.listFail = map[string]bool{ops: true} }) }, "ops:0,ops2:1"},
		{"ops is read again", func() { f.lk.config(func(l *scriptedLiveKit) { l.listFail = nil }) }, "ops:2,ops2:1"},
		{"ann2 leaves", func() {
			f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann"), joinedMs: 6_000})
		}, "ops:1,ops2:1"},
	}
	for _, st := range steps {
		st.script()
		f.clock.Advance(testPassGap)
		f.pass(t)
		if got := f.occ(readOccupancy(t, conn)); got != st.want {
			t.Errorf("%s: frame = %q, want %q", st.name, got, st.want)
		}
	}
	// An outage is a change too, and carries no counts; so is the recovery,
	// which shows the rooms once they are read.
	f.lk.setListStatus(http.StatusInternalServerError)
	f.clock.Advance(testPassGap)
	f.pass(t)
	if got := f.occ(readOccupancy(t, conn)); got != "-" {
		t.Fatalf("outage frame: %q", got)
	}
	f.lk.setListStatus(0)
	f.clock.Advance(time.Minute)
	f.pass(t)
	if got := f.occ(readOccupancy(t, conn)); got != "ops:1,ops2:1" {
		t.Errorf("recovery frame: %q", got)
	}
}

// TestVoiceOccupancyWakesOnCountsOnly: the poller wakes an occupancy
// subscriber when a count changes and for nothing else. A transmit flip wakes
// the channel's presence subscribers and not the occupancy ones.
func TestVoiceOccupancyWakesOnCountsOnly(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ops, _ := f.live(t)
	occ := f.srv.voice.subscribeOccupancy(f.ids["ann"], 0)
	defer occ.cancel()
	pres := f.srv.voice.subscribe(f.ops.ID, f.ids["ann"], 0)
	defer pres.cancel()
	drain := func() {
		for _, c := range []chan struct{}{occ.wake, pres.wake} {
			select {
			case <-c:
			default:
			}
		}
	}
	tests := []struct {
		name              string
		script            func()
		wantOcc, wantPres bool
	}{
		{"ann2 talks", func() { f.lk.update(ops, f.identity("ann2"), func(p *fakeParticipant) { p.published = true }) }, false, true},
		{"ann2 stops", func() { f.lk.update(ops, f.identity("ann2"), func(p *fakeParticipant) { p.muted = true }) }, false, true},
		{"nothing changes", func() {}, false, false},
		{"ann2 reconnects", func() {
			f.lk.update(ops, f.identity("ann2"), func(p *fakeParticipant) { p.joinedMs = 9_000 })
		}, false, true},
		{"ann2 leaves", func() { f.lk.drop(ops, f.identity("ann2")) }, true, true},
		{"ann2 comes back", func() {
			f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000}, fakeParticipant{identity: f.identity("ann2"), joinedMs: 10_000})
		}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			drain()
			tt.script()
			f.clock.Advance(testPassGap)
			f.pass(t)
			if got := len(occ.wake) == 1; got != tt.wantOcc {
				t.Errorf("occupancy woken = %v, want %v", got, tt.wantOcc)
			}
			if got := len(pres.wake) == 1; got != tt.wantPres {
				t.Errorf("presence woken = %v, want %v", got, tt.wantPres)
			}
		})
	}
	// Nothing an occupancy subscriber does wakes the poller's loop or calls
	// LiveKit: a subscription and a read change neither.
	// The loop is not running here; a wake-up the fixture's sessions queued
	// is taken off first.
	select {
	case <-f.srv.voice.wakeLoop:
	default:
	}
	calls := len(f.lk.calls())
	sub := f.srv.voice.subscribeOccupancy(f.ids["ann2"], 0)
	defer sub.cancel()
	f.getOccupancy(t, "ann2")
	if len(f.srv.voice.wakeLoop) != 0 {
		t.Error("an occupancy subscriber woke the poller's loop")
	}
	if got := len(f.lk.calls()) - calls; got != 0 {
		t.Errorf("an occupancy subscriber made %d LiveKit calls", got)
	}
}

// TestVoiceOccupancySlowSubscriberNeverBlocks: a subscriber that never reads
// does not hold up the poller, however many counts change.
func TestVoiceOccupancySlowSubscriberNeverBlocks(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := f.inUse(t, f.ops)
	sub := f.srv.voice.subscribeOccupancy(f.ids["ann"], 0)
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

// TestVoiceOccupancySocketRemovalDropsTheRow: a member removed from a channel
// keeps the socket and loses that channel's row; the row is gone from the
// subscription before the removal is answered, and the next frame has none.
// Being added shows at the next re-check.
func TestVoiceOccupancySocketRemovalDropsTheRow(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	// Short, so that being added shows soon; set before any socket opens.
	f.srv.credRecheckInterval = 20 * time.Millisecond
	f.live(t)
	base := wsTestServer(t, f.srv)
	conn, _, err := dialOccupancy(t, base, f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	if got := f.occ(readOccupancy(t, conn)); got != "ops:2,ops2:0" {
		t.Fatalf("on connect: %q", got)
	}
	if rec := f.do(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops2/members/%d", f.ids["ann"]), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	// Before the next frame is read: the hook ran before the answer.
	f.srv.voice.mu.Lock()
	for sub := range f.srv.voice.occSubs {
		if sub.principalID == f.ids["ann"] && slices.Contains(sub.channels, f.ops2.ID) {
			t.Error("the removal was answered with ops2 still in ann's occupancy")
		}
	}
	f.srv.voice.mu.Unlock()
	if got := f.occ(readOccupancy(t, conn)); got != "ops:2" {
		t.Errorf("after the removal: %q, want ops2 absent", got)
	}
	// Still open: a change reaches it.
	f.lk.drop(f.room(t, f.ops), f.identity("ann2"))
	f.clock.Advance(testPassGap)
	f.pass(t)
	if got := f.occ(readOccupancy(t, conn)); got != "ops:1" {
		t.Errorf("after ann2 leaves: %q", got)
	}

	// Added: the next re-check reads the channels again.
	conn2, _, err := dialOccupancy(t, base, f.tokens["ann2"])
	if err != nil {
		t.Fatal(err)
	}
	if got := f.occ(readOccupancy(t, conn2)); got != "ops:1" {
		t.Fatalf("ann2 on connect: %q", got)
	}
	if rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/ops2/members/%d", f.ids["ann2"]), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("add = %d %s", rec.Code, rec.Body)
	}
	if got := f.occ(readOccupancy(t, conn2)); got != "ops:1,ops2:0" {
		t.Errorf("after ann2 is added to ops2: %q", got)
	}
}

// TestVoiceOccupancyReadDoesNotUndoARemoval: a read of the channels that
// began before a removal is not applied after it.
func TestVoiceOccupancyReadDoesNotUndoARemoval(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	sub := f.srv.voice.subscribeOccupancy(f.ids["ann"], 0)
	defer sub.cancel()
	if !sub.setChannels([]int64{f.ops.ID, f.ops2.ID}, sub.generation()) {
		t.Fatal("first set refused")
	}
	since := sub.generation()
	// The read returns both channels; the removal lands before it is applied.
	f.srv.voice.occupancyChannelLost(f.ops2.ID, f.ids["ann"])
	if sub.setChannels([]int64{f.ops.ID, f.ops2.ID}, since) {
		t.Fatal("a read from before the removal was applied")
	}
	if err := f.srv.loadOccupancyChannels(context.Background(), sub); err != nil {
		t.Fatal(err)
	}
	f.srv.voice.mu.Lock()
	got := slices.Clone(sub.channels)
	f.srv.voice.mu.Unlock()
	if !slices.Equal(got, []int64{f.ops.ID, f.ops2.ID}) {
		// The store still has ann in ops2 (only the hook ran): a fresh read
		// is the truth.
		t.Errorf("after a fresh read: %v", got)
	}
	// The removal of one viewer leaves another's subscription alone.
	other := f.srv.voice.subscribeOccupancy(f.ids["ann2"], 0)
	defer other.cancel()
	if !other.setChannels([]int64{f.ops.ID}, other.generation()) {
		t.Fatal("set refused")
	}
	before := other.generation()
	f.srv.voice.occupancyChannelLost(f.ops.ID, f.ids["ann"])
	if other.generation() != before || len(other.wake) != 0 {
		t.Error("ann's removal touched ann2's subscription")
	}
}

// TestVoiceOccupancySocketClosedOnLoss: the socket is closed, before the
// response is written, when its principal is disabled or hidden, signs out
// everywhere, or the credential it was opened with is revoked; someone else's
// socket is untouched.
func TestVoiceOccupancySocketClosedOnLoss(t *testing.T) {
	tests := []struct {
		name   string
		method string
		// path is the operator's request; cred is the credential ann's
		// socket was opened with.
		path func(f *presenceFixture, cred int64) string
	}{
		{"disabled", "POST", func(f *presenceFixture, _ int64) string {
			return fmt.Sprintf("/v1/principals/%d/disable", f.ids["ann"])
		}},
		{"hidden", "POST", func(f *presenceFixture, _ int64) string { return fmt.Sprintf("/v1/principals/%d/hide", f.ids["ann"]) }},
		{"all credentials revoked", "POST", func(f *presenceFixture, _ int64) string {
			return fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.ids["ann"])
		}},
		{"the credential revoked", "DELETE", func(_ *presenceFixture, cred int64) string { return fmt.Sprintf("/v1/credentials/%d", cred) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			f.srv.credRecheckInterval = time.Hour // the hooks close it, not the re-check
			f.live(t)
			base := wsTestServer(t, f.srv)
			annConn, _, err := dialOccupancy(t, base, f.tokens["ann"])
			if err != nil {
				t.Fatal(err)
			}
			readOccupancy(t, annConn)
			ann2Conn, _, err := dialOccupancy(t, base, f.tokens["ann2"])
			if err != nil {
				t.Fatal(err)
			}
			readOccupancy(t, ann2Conn)
			annCred := f.credID(t, "ann")

			if code := f.doOrdered(t, tt.method, tt.path(f, annCred), f.rootTok); code != 204 && code != 200 {
				t.Fatalf("status = %d", code)
			}
			// Dropped before the answer: no occupancy subscription of ann's
			// is left once the request returned.
			f.srv.voice.mu.Lock()
			for sub := range f.srv.voice.occSubs {
				if sub.principalID == f.ids["ann"] && sub.credentialID == annCred {
					t.Error("ann's occupancy subscription outlived the request")
				}
			}
			f.srv.voice.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, raw, err := annConn.Read(ctx)
			if err == nil {
				t.Fatalf("ann's socket sent a frame after the loss: %s", raw)
			}
			closeErrContains(t, err, websocket.StatusPolicyViolation, reasonRevoked)
			// ann2's is untouched: a change still reaches it.
			f.lk.drop(f.room(t, f.ops), f.identity("ann"))
			f.clock.Advance(testPassGap)
			f.pass(t)
			if got := f.occ(readOccupancy(t, ann2Conn)); got != "ops:1" {
				t.Errorf("ann2 frame: %q", got)
			}
		})
	}

	t.Run("shutdown", func(t *testing.T) {
		f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
		base := wsTestServer(t, f.srv)
		conn, _, err := dialOccupancy(t, base, f.tokens["ann"])
		if err != nil {
			t.Fatal(err)
		}
		readOccupancy(t, conn)
		f.srv.voice.closeAll()
		if code := expectClosed(t, conn); code != websocket.StatusGoingAway {
			t.Errorf("close status = %d, want going away", code)
		}
		if sub := f.srv.voice.subscribeOccupancy(f.ids["ann"], 0); !sub.gone {
			t.Error("a subscription after shutdown was not dropped")
		}
	})
}

// TestVoiceOccupancySocketRefusals: refusals before the upgrade, as on the
// presence socket. An agent and a person without voice are refused and
// audited; nothing refused is counted against the credential's sockets.
func TestVoiceOccupancySocketRefusals(t *testing.T) {
	tests := []struct {
		name       string
		opts       presenceOpts
		who        string
		wantStatus int
		wantCode   string
		wantDenied int
	}{
		{"agent member", presenceOpts{auth: AuthRequired}, "robo", 403, "forbidden", 1},
		{"member without voice", presenceOpts{auth: AuthRequired}, "vic", 403, schema.ErrorCodeVoiceNotAllowed, 1},
		{"not configured, member without voice", presenceOpts{auth: AuthRequired, unconfigured: true}, "vic", 403, schema.ErrorCodeVoiceNotAllowed, 1},
		{"disabled", presenceOpts{auth: AuthRequired}, "dora", 401, "unauthenticated", 0},
		{"unauthenticated", presenceOpts{auth: AuthRequired}, "", 401, "unauthenticated", 0},
		{"auth off", presenceOpts{auth: AuthOff}, "", 400, schema.ErrorCodeVoiceRequiresAuth, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, tt.opts)
			if tt.opts.auth == AuthRequired {
				f.joinWithoutVoiceP(t)
			}
			base := wsTestServer(t, f.srv)
			denied := len(f.audits(t, "access_denied"))
			_, resp, err := dialOccupancy(t, base, f.tokens[tt.who])
			if err == nil || resp == nil {
				t.Fatalf("dial succeeded, want %d", tt.wantStatus)
			}
			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if got := len(f.audits(t, "access_denied")) - denied; got != tt.wantDenied {
				t.Errorf("access_denied events = %d, want %d", got, tt.wantDenied)
			}
			if tt.who != "" && tt.who != "dora" {
				if n := f.srv.sockets.count(f.credID(t, tt.who)); n != 0 {
					t.Errorf("a refused upgrade left a count of %d", n)
				}
			}
			f.srv.voice.mu.Lock()
			n := len(f.srv.voice.occSubs)
			f.srv.voice.mu.Unlock()
			if n != 0 {
				t.Errorf("a refused upgrade left %d occupancy subscriptions", n)
			}
		})
	}
}

// TestVoiceOccupancyNotConfigured: with voice not configured the socket
// carries one document, which says so, and the read the same.
func TestVoiceOccupancyNotConfigured(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired, unconfigured: true})
	base := wsTestServer(t, f.srv)
	conn, _, err := dialOccupancy(t, base, f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	if got := f.occ(readOccupancy(t, conn)); got != "x" {
		t.Errorf("frame = %q, want not configured", got)
	}
	if _, doc := f.getOccupancy(t, "ann"); f.occ(doc) != "x" {
		t.Errorf("read = %q, want not configured", f.occ(doc))
	}
}
