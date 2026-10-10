package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of transmit reports through a server (issue #135,
// docs/design/conch-voice.md §6): the endpoint, its bound, and the poller
// applying the transmit rule. The rule itself is tested as tables in
// voice_transmit_rule_test.go; here the passes are real passes over a scripted
// LiveKit and the reports are real requests, with the fixture's manual clock.
// Nothing sleeps. Every scenario ends by checking that the rows in the audit
// log pair up (assertTransmitPairs).

// ---------------------------------------------------------------------------
// Fixture additions

// at sets the fixture's clock to ms milliseconds after its start, which is
// ruleT0.
func (f *presenceFixture) at(ms int) {
	f.clock.Set(ruleT0.Add(time.Duration(ms) * time.Millisecond))
}

func (f *presenceFixture) reportBody(t *testing.T, who, channel, body string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPost, "/v1/channels/"+channel+"/voice/transmit", f.tokens[who], body)
}

// report sends one transmit report as who.
func (f *presenceFixture) report(t *testing.T, who, channel, state string) wireResult {
	t.Helper()
	return f.reportBody(t, who, channel, `{"state":"`+state+`"}`)
}

// mustReport sends a report in ops that must succeed: 204 and no body at all.
func (f *presenceFixture) mustReport(t *testing.T, who, state string) {
	t.Helper()
	rec := f.do(t, http.MethodPost, "/v1/channels/ops/voice/transmit", f.tokens[who], `{"state":"`+state+`"}`)
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Fatalf("%s reports %s: status %d body %q, want 204 and no body", who, state, rec.Code, rec.Body)
	}
}

// transmitRows lists the transmit rows of the audit log in the order they were
// written, as "<who> <action>@<ms> <source>[ <reason>]". It checks on the way
// that each has the subject and detail the poller's rows have always had: the
// channel, the audience, and where the row came from.
func (f *presenceFixture) transmitRows(t *testing.T) []string {
	t.Helper()
	names := map[string]string{}
	for name, id := range f.ids {
		names[fmt.Sprintf("principal:%d", id)] = name
	}
	var out []string
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditVoiceTransmitStarted, store.AuditVoiceTransmitStopped, store.AuditVoiceTransmitUnreported:
		default:
			continue
		}
		who, ok := names[e.Actor]
		if !ok {
			t.Errorf("%s has actor %q, want a principal", e.Action, e.Actor)
		}
		if want := fmt.Sprintf("channel:%d", f.ops.ID); e.Subject != want {
			t.Errorf("%s has subject %q, want %q", e.Action, e.Subject, want)
		}
		rest, ok := strings.CutPrefix(e.Detail, fmt.Sprintf("channel=%d audience=channel source=", f.ops.ID))
		if !ok {
			t.Errorf("%s has detail %q, want it to begin with the channel, the audience and the source", e.Action, e.Detail)
		}
		rest = strings.Replace(rest, " reason=", " ", 1)
		out = append(out, fmt.Sprintf("%s %s@%d %s", who, strings.TrimPrefix(e.Action, "voice_transmit_"), e.CreatedAt.Sub(ruleT0).Milliseconds(), rest))
	}
	return out
}

// liveStep is one thing that happens in a scenario run through a server: a
// change of ann's connection in LiveKit, a report from ann, or a pass.
type liveStep struct {
	at int // milliseconds after the fixture's start
	do string
}

// runLive plays steps for ann in room. A change in LiveKit is seen by the next
// "pass" step and by nothing else.
func (f *presenceFixture) runLive(t *testing.T, room string, steps []liveStep) {
	t.Helper()
	ann := f.identity("ann")
	for _, st := range steps {
		f.at(st.at)
		switch st.do {
		case "join":
			f.lk.setRoom(room, fakeParticipant{identity: ann, joinedMs: 1_000, published: true, muted: true})
		case "unmute":
			f.lk.update(room, ann, func(p *fakeParticipant) { p.muted = false })
		case "mute":
			f.lk.update(room, ann, func(p *fakeParticipant) { p.muted = true })
		case "leave":
			f.lk.setRoom(room)
		case "pass":
			f.pass(t)
		case "started", "stopped":
			f.mustReport(t, "ann", st.do)
		default:
			t.Fatalf("unknown step %q", st.do)
		}
	}
}

// ---------------------------------------------------------------------------
// The poller and the endpoint together

// TestVoiceTransmitScenarios runs each sequence the issue's acceptance
// criteria name through the endpoint and the poller. ann holds a session and
// is muted in the room at the start.
func TestVoiceTransmitScenarios(t *testing.T) {
	// open is how every scenario begins: ann in the room, muted, seen.
	open := []liveStep{{0, "join"}, {0, "pass"}}
	tests := []struct {
		name  string
		steps []liveStep
		want  []string
	}{
		{"started then stopped is exactly two rows; repeating either writes nothing more", []liveStep{
			{100, "started"}, {110, "started"}, {120, "started"}, {200, "stopped"}, {210, "stopped"}, {500, "pass"},
		}, []string{"ann started@100 reported", "ann stopped@200 reported"}},

		{"a 50 ms press that no pass falls in", []liveStep{
			{100, "unmute"}, {100, "started"}, {150, "stopped"}, {150, "mute"}, {500, "pass"},
		}, []string{"ann started@100 reported", "ann stopped@150 reported"}},
		{"a 50 ms press that a pass falls in", []liveStep{
			{480, "started"}, {480, "unmute"}, {500, "pass"}, {530, "stopped"}, {530, "mute"}, {1000, "pass"},
		}, []string{"ann started@480 reported", "ann stopped@530 reported"}},

		{"never reports, unmuted over many passes", []liveStep{
			{400, "unmute"}, {500, "pass"}, {1000, "pass"}, {1500, "pass"}, {1600, "mute"}, {2000, "pass"},
		}, []string{"ann unreported@500 observed", "ann stopped@2000 observed muted"}},
		{"never reports, unmuted on a single pass, then muted", []liveStep{
			{400, "unmute"}, {500, "pass"}, {600, "mute"}, {1000, "pass"},
		}, []string{"ann unreported@500 observed", "ann stopped@1000 observed muted"}},
		{"never reports, unmuted on a single pass, then gone", []liveStep{
			{400, "unmute"}, {500, "pass"}, {600, "leave"}, {1000, "pass"},
		}, []string{"ann unreported@500 observed", "ann stopped@1000 observed left"}},

		{"an honest press: one pass between the unmute and the started report", []liveStep{
			{490, "unmute"}, {500, "pass"}, {520, "started"}, {1000, "pass"}, {1200, "stopped"}, {1200, "mute"}, {1500, "pass"},
		}, []string{"ann started@520 reported", "ann stopped@1200 reported"}},
		{"an honest press: one pass between the started report and the unmute", []liveStep{
			{480, "started"}, {500, "pass"}, {520, "unmute"}, {1000, "pass"}, {1200, "stopped"}, {1200, "mute"}, {1500, "pass"},
		}, []string{"ann started@480 reported", "ann stopped@1200 reported"}},
		{"an honest release: one pass between the stopped report and the mute", []liveStep{
			{100, "started"}, {100, "unmute"}, {500, "pass"}, {980, "stopped"}, {1000, "pass"}, {1020, "mute"}, {1500, "pass"},
		}, []string{"ann started@100 reported", "ann stopped@980 reported"}},
		{"an honest release: one pass between the mute and the stopped report", []liveStep{
			{100, "started"}, {100, "unmute"}, {500, "pass"}, {980, "mute"}, {1000, "pass"}, {1020, "stopped"}, {1500, "pass"},
		}, []string{"ann started@100 reported", "ann stopped@1020 reported"}},

		{"an honest quick release and re-press that lands on a pass: excused once", []liveStep{
			{100, "started"}, {100, "unmute"}, {500, "pass"},
			{990, "stopped"}, {1000, "pass"}, {1010, "started"}, {1500, "pass"},
			{1700, "stopped"}, {1700, "mute"}, {2000, "pass"},
		}, []string{"ann started@100 reported", "ann stopped@990 reported", "ann started@1010 reported", "ann stopped@1700 reported"}},
		{"a second such coincidence within 3 s is recorded", []liveStep{
			{100, "started"}, {100, "unmute"}, {500, "pass"},
			{990, "stopped"}, {1000, "pass"}, {1010, "started"}, {1500, "pass"},
			{2000, "pass"}, {2500, "pass"}, {3000, "pass"},
			{3490, "stopped"}, {3500, "pass"}, {3510, "started"}, {4000, "pass"},
			{4200, "stopped"}, {4200, "mute"}, {4500, "pass"},
		}, []string{
			"ann started@100 reported", "ann stopped@990 reported", "ann started@1010 reported",
			"ann stopped@3490 reported", "ann started@3510 reported",
			"ann unreported@3500 observed", "ann stopped@4000 observed reported",
			"ann stopped@4200 reported",
		}},

		{"started, then stopped, and stays unmuted: opened on the second pass after, timed at the first", []liveStep{
			{100, "started"}, {100, "unmute"}, {500, "pass"}, {600, "stopped"},
			{1000, "pass"}, {1500, "pass"}, {2000, "pass"}, {2100, "mute"}, {2500, "pass"},
		}, []string{"ann started@100 reported", "ann stopped@600 reported", "ann unreported@1000 observed", "ann stopped@2500 observed muted"}},

		{"a started report arriving after an unreported transmission was opened", []liveStep{
			{400, "unmute"}, {500, "pass"}, {1000, "pass"}, {1200, "started"}, {1500, "pass"}, {2000, "pass"},
			{2300, "stopped"}, {2300, "mute"}, {2500, "pass"},
		}, []string{"ann unreported@500 observed", "ann started@1200 reported", "ann stopped@1500 observed reported", "ann stopped@2300 reported"}},
		{"reported first and observed after: nothing from the poller", []liveStep{
			{100, "started"}, {500, "pass"}, {600, "unmute"}, {1000, "pass"}, {1500, "pass"},
			{1700, "stopped"}, {1700, "mute"}, {2000, "pass"},
		}, []string{"ann started@100 reported", "ann stopped@1700 reported"}},

		{"a reported started with no transmission is closed after 2 s; a stopped afterwards writes nothing", []liveStep{
			{100, "started"}, {500, "pass"}, {1000, "pass"}, {1500, "pass"}, {2000, "pass"}, {2099, "pass"}, {2100, "pass"},
			{2200, "stopped"}, {2500, "pass"},
		}, []string{"ann started@100 reported", "ann stopped@2100 observed no_stop_report"}},

		{"left while reported started", []liveStep{
			{100, "started"}, {100, "unmute"}, {500, "pass"}, {600, "leave"}, {1000, "pass"}, {1100, "stopped"}, {1500, "pass"},
		}, []string{"ann started@100 reported", "ann stopped@1000 observed left"}},

		{"one closing row: the client caught up", []liveStep{
			{400, "unmute"}, {500, "pass"}, {1000, "pass"}, {1200, "started"}, {1500, "pass"}, {1600, "stopped"}, {1600, "mute"}, {2000, "pass"},
		}, []string{"ann unreported@500 observed", "ann started@1200 reported", "ann stopped@1500 observed reported", "ann stopped@1600 reported"}},
		{"one closing row: muted comes before reported", []liveStep{
			{400, "unmute"}, {500, "pass"}, {1000, "pass"}, {1200, "started"}, {1300, "mute"}, {1500, "pass"}, {1600, "stopped"}, {2000, "pass"},
		}, []string{"ann unreported@500 observed", "ann started@1200 reported", "ann stopped@1500 observed muted", "ann stopped@1600 reported"}},
		{"one closing row: left comes before reported, and closes the reported start too", []liveStep{
			{400, "unmute"}, {500, "pass"}, {1000, "pass"}, {1200, "started"}, {1300, "leave"}, {1500, "pass"}, {1600, "stopped"}, {2000, "pass"},
		}, []string{"ann unreported@500 observed", "ann started@1200 reported", "ann stopped@1500 observed left"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
			f.runLive(t, room, append(slices.Clone(open), tt.steps...))
			if got := f.transmitRows(t); !slices.Equal(got, tt.want) {
				t.Errorf("transmit rows:\n got  %q\n want %q", got, tt.want)
			}
			assertTransmitPairs(t, f.audit(t))
			// The poller never writes a start any more.
			for _, e := range f.audits(t, store.AuditVoiceTransmitStarted) {
				if !strings.Contains(e.Detail, "source=reported") {
					t.Errorf("a voice_transmit_started row from the poller: %q", e.Detail)
				}
			}
		})
	}
}

// TestVoiceTransmitPresenceStillShowsWhatIsSeen: presence says who is talking
// as the poller sees it, whatever was or was not reported, and a report by
// itself changes nothing a reader of presence sees.
func TestVoiceTransmitPresenceStillShowsWhatIsSeen(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	f.runLive(t, room, []liveStep{{0, "join"}, {0, "pass"}, {100, "started"}})
	if got := f.who(t, f.ops); got != "ann" {
		t.Errorf("after a report with the microphone muted: %q, want ann not talking", got)
	}
	f.runLive(t, room, []liveStep{{200, "stopped"}, {300, "unmute"}, {500, "pass"}})
	if got := f.who(t, f.ops); got != "ann*" {
		t.Errorf("unmuted and unreported: %q, want ann talking", got)
	}
	f.runLive(t, room, []liveStep{{600, "mute"}, {1000, "pass"}})
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitHolderWhoNeverConnects: a holder that reports `started`
// long after its session was issued, without ever connecting to LiveKit. The
// report puts the room in use and wakes the loop, so the poller looks at it;
// the holder counts as not transmitting, and the start is closed after 2 s
// with reason=no_stop_report (§6). A `stopped` arriving afterwards writes
// nothing.
func TestVoiceTransmitHolderWhoNeverConnects(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	decodeSession(t, f.session(t, "ann", "ops"))
	f.pass(t)
	// Long after: the session no longer keeps the room in use, and the loop
	// is asleep until the next sweep.
	f.clock.Advance(voiceSessionRecent + time.Minute)
	f.sweep(t)
	f.pass(t) // LiveKit still has the empty room; one look and it is idle
	if f.srv.voice.anyInUse() {
		t.Fatal("the room is still in use")
	}
	select {
	case <-f.srv.voice.wakeLoop:
	default:
	}
	start := f.clock.Now()
	listed := f.lk.count("ListParticipants")

	f.mustReport(t, "ann", "started")
	if !f.srv.voice.anyInUse() {
		t.Fatal("a reported started did not put the room in use")
	}
	select {
	case <-f.srv.voice.wakeLoop:
	default:
		t.Fatal("a reported started for a room nobody was watching did not wake the loop")
	}
	if d := f.srv.voice.nextDelay(); d != testPassGap {
		t.Errorf("delay with a reported started open = %v, want the gap between passes", d)
	}
	// A second report for a room that is now in use wakes nothing: a report
	// cannot be used to choose when a pass happens.
	f.mustReport(t, "ann", "started")
	select {
	case <-f.srv.voice.wakeLoop:
		t.Error("a report for a room already in use woke the loop")
	default:
	}

	for _, ms := range []int{500, 1000, 1500, 1999} {
		f.clock.Set(start.Add(time.Duration(ms) * time.Millisecond))
		f.pass(t)
	}
	if got := len(f.audits(t, store.AuditVoiceTransmitStopped)); got != 0 {
		t.Fatalf("closed before 2 s: %d rows", got)
	}
	f.clock.Set(start.Add(2 * time.Second))
	f.pass(t)
	closed := f.audits(t, store.AuditVoiceTransmitStopped)
	if len(closed) != 1 || !strings.HasSuffix(closed[0].Detail, "source=observed reason=no_stop_report") || closed[0].Actor != f.actor("ann") || !closed[0].CreatedAt.Equal(start.Add(2*time.Second)) {
		t.Fatalf("rows closing the start = %+v, want one, by the poller, reason=no_stop_report, timed at the pass", closed)
	}
	if got := f.lk.count("ListParticipants") - listed; got != 5 {
		t.Errorf("the room was polled %d times because of the report, want 5", got)
	}
	// Settled: the room is idle again.
	if f.srv.voice.anyInUse() {
		t.Error("the room is still in use after the start was closed")
	}
	f.mustReport(t, "ann", "stopped")
	if got := len(f.transmitRows(t)); got != 2 {
		t.Errorf("transmit rows = %d, want the started and its close, and nothing for the late stopped", got)
	}
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitRotation: when a room is rotated, every transmission open
// in it, reported or unreported, is closed at once with reason=left, before
// the state is dropped, and before the leaving is written. That includes a
// reported `started` from a holder who never connected, rotated within 2 s of
// the report. A credential whose room was rotated away holds no session.
func TestVoiceTransmitRotation(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	// ann2 holds a session under a second credential, which is what gets
	// revoked; she never connects, and reports a press.
	spareID, spareTok := f.credentialWithExpiry(t, "ann2", time.Now().Add(time.Hour))
	f.tokens["ann2-spare"] = spareTok
	decodeSession(t, f.callREST(t, http.MethodPost, "/v1/channels/ops/voice/session", spareTok, ""))

	// ann is in the room, reports honestly and is talking. bob is there
	// without a session and transmits unreported; he is not a member, but
	// the test stops the poller removing him so that his transmission is
	// open when the room is rotated.
	f.srv.voice.entitle = func(_ context.Context, identity string, _ int64) (int64, string, error) {
		id, _ := parseVoiceIdentity(identity)
		return id, "", nil
	}
	f.at(0)
	f.lk.setRoom(room,
		fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000, published: true, muted: true},
		fakeParticipant{identity: f.identity("bob"), joinedMs: 2_000, published: true})
	f.pass(t)
	f.at(100)
	f.mustReport(t, "ann", "started")
	f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) { p.muted = false })
	f.at(500)
	f.pass(t)
	f.at(900)
	f.mustReport(t, "ann2-spare", "started")

	// 300 ms after ann2's report, her credential is revoked: the room rotates.
	f.at(1200)
	if code := f.doOrdered(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", spareID), f.rootTok); code != 204 {
		t.Fatalf("revoke status = %d", code)
	}
	if len(f.audits(t, store.AuditVoiceRoomRotated)) != 1 || f.room(t, f.ops) == room {
		t.Fatal("the room was not rotated")
	}
	want := []string{
		"ann started@100 reported",
		"bob unreported@0 observed", // written by the pass at 500, timed at the one before
		"ann2 started@900 reported",
		"ann stopped@1200 observed left",
		"ann2 stopped@1200 observed left",
		"bob stopped@1200 observed left",
	}
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("transmit rows:\n got  %q\n want %q", got, want)
	}
	// Each close comes before its principal's voice_left.
	audit := f.voiceAudit(t)
	for _, who := range []string{"ann", "bob"} {
		stop := slices.Index(audit, "voice_transmit_stopped "+f.actor(who))
		left := slices.Index(audit, "voice_left "+f.actor(who))
		if stop < 0 || left < 0 || stop > left {
			t.Errorf("%s: the close (%d) is not before the leaving (%d) in %q", who, stop, left, audit)
		}
	}
	assertTransmitPairs(t, f.audit(t))

	// Nothing of the old room is left to report into, and its holders are
	// gone: ann's credential holds no session for the channel's new room.
	before := len(f.transmitRows(t))
	for _, state := range []string{"stopped", "started"} {
		if res := f.report(t, "ann", "ops", state); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Errorf("ann reports %s after the rotation: %d %s, want 409 voice_no_session", state, res.status, res.body)
		}
	}
	if got := len(f.transmitRows(t)); got != before {
		t.Errorf("a report into a rotated room was audited as a transmission: %d rows, was %d", got, before)
	}
	// A new session is a new room and a new reported state.
	decodeSession(t, f.session(t, "ann", "ops"))
	f.at(2000)
	f.mustReport(t, "ann", "started")
	f.at(2100)
	f.mustReport(t, "ann", "stopped")
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitRemovedByName: a transmission open when someone is removed
// from the room by name is closed at once with reason=left, at the moment they
// leave presence and before their leaving is written. Someone removed by name
// holds no session (a holder's room is rotated instead), so what can be open
// for them is an unreported transmission.
func TestVoiceTransmitRemovedByName(t *testing.T) {
	tests := []struct {
		name string
		// passes is how many passes see ann2 unmuted before she is removed.
		passes int
		// remove takes ann2's place away; breakRooms makes the read of her
		// rooms fail first, which is the path that calls no LiveKit.
		remove     func(f *presenceFixture) (method, path string)
		breakRooms bool
		want       []string
	}{
		{"member removed, an unreported transmission open", 2,
			func(f *presenceFixture) (string, string) {
				return "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann2"])
			}, false,
			[]string{"ann2 unreported@0 observed", "ann2 stopped@700 observed left"}},
		{"member removed after a single unaccounted pass", 1,
			func(f *presenceFixture) (string, string) {
				return "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann2"])
			}, false,
			[]string{"ann2 unreported@0 observed", "ann2 stopped@700 observed left"}},
		{"principal disabled", 2,
			func(f *presenceFixture) (string, string) {
				return "POST", fmt.Sprintf("/v1/principals/%d/disable", f.ids["ann2"])
			}, false,
			[]string{"ann2 unreported@0 observed", "ann2 stopped@700 observed left"}},
		{"principal disabled, her rooms unreadable", 2,
			func(f *presenceFixture) (string, string) {
				return "POST", fmt.Sprintf("/v1/principals/%d/disable", f.ids["ann2"])
			}, true,
			[]string{"ann2 unreported@0 observed", "ann2 stopped@700 observed left"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			// ann holds the session; ann2 is in the room without one.
			room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
			f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann2"), joinedMs: 2_000, published: true})
			for i := range tt.passes {
				f.at(i * 500)
				f.pass(t)
			}
			if tt.breakRooms {
				f.srv.voice.memberRooms = func(context.Context, int64) ([]store.VoiceRoom, error) {
					return nil, errors.New("store unavailable")
				}
			}
			f.at(700)
			method, path := tt.remove(f)
			if code := f.doOrdered(t, method, path, f.rootTok); code != 204 {
				t.Fatalf("status = %d", code)
			}
			if got := f.transmitRows(t); !slices.Equal(got, tt.want) {
				t.Errorf("transmit rows:\n got  %q\n want %q", got, tt.want)
			}
			audit := f.voiceAudit(t)
			stop := slices.Index(audit, "voice_transmit_stopped "+f.actor("ann2"))
			left := slices.Index(audit, "voice_left "+f.actor("ann2"))
			if stop < 0 || left < 0 || stop > left {
				t.Errorf("the close (%d) is not before the leaving (%d) in %q", stop, left, audit)
			}
			if len(f.audits(t, store.AuditVoiceRoomRotated)) != 0 {
				t.Error("the room was rotated: ann2 held no session")
			}
			// A pass after it finds nothing more to say about her.
			f.lk.setRoom(room)
			f.at(1200)
			f.pass(t)
			if got := f.transmitRows(t); !slices.Equal(got, tt.want) {
				t.Errorf("after a later pass:\n got  %q\n want %q", got, tt.want)
			}
			assertTransmitPairs(t, f.audit(t))
		})
	}
}

// TestVoiceTransmitUnreadableParticipant: on a pass that cannot read someone's
// entitlement, that participant keeps the state they had, and the transmit
// rule is told nothing about them: not that they are gone, and not what the
// pass saw.
func TestVoiceTransmitUnreadableParticipant(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	f.runLive(t, room, []liveStep{{0, "join"}, {0, "pass"}, {100, "started"}, {100, "unmute"}, {500, "pass"}})

	real := f.srv.voice.entitle
	f.srv.voice.entitle = func(context.Context, string, int64) (int64, string, error) {
		return f.ids["ann"], "", errors.New("store unavailable")
	}
	// She mutes without reporting, then the store fails for two passes.
	f.runLive(t, room, []liveStep{{600, "mute"}, {1000, "pass"}, {1500, "pass"}})
	if got, want := f.transmitRows(t), []string{"ann started@100 reported"}; !slices.Equal(got, want) {
		t.Fatalf("while unreadable: rows %q, want %q", got, want)
	}
	if got := f.who(t, f.ops); got != "ann*" {
		t.Errorf("while unreadable: %q, want ann exactly as she was", got)
	}
	// Readable again. The last pass that saw her transmitting was at 500,
	// so the 2 s rule closes the start on the first pass at or after 2500.
	f.srv.voice.entitle = real
	f.runLive(t, room, []liveStep{{2000, "pass"}, {2499, "pass"}, {2500, "pass"}})
	want := []string{"ann started@100 reported", "ann stopped@2500 observed no_stop_report"}
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("rows:\n got  %q\n want %q", got, want)
	}
	assertTransmitPairs(t, f.audit(t))
}

// restart replaces the poller with a fresh one, as a restart of conchd does:
// everything in memory is gone, the store is as it was.
func (f *presenceFixture) restart() {
	p := newVoicePoller(f.srv)
	p.now = f.clock.Now
	p.gap = func() time.Duration { return testPassGap }
	f.srv.voice = p
}

// TestVoiceTransmitRestartMidPress: conchd restarts during a press. Reported
// state is in memory and is lost, so it reads `stopped`: the poller, finding
// the participant already transmitting, opens an unreported transmission and
// closes it when the microphone is muted, and the client's `stopped` report
// changes nothing. The `started` row written before the restart is the one
// documented case of an opening row with no closing row of its own (§6).
func TestVoiceTransmitRestartMidPress(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	f.runLive(t, room, []liveStep{{0, "join"}, {0, "pass"}, {100, "started"}, {100, "unmute"}, {500, "pass"}})

	f.restart()
	f.at(5000)
	f.sweep(t) // the startup sweep finds the room in LiveKit
	f.runLive(t, room, []liveStep{{5000, "pass"}, {5500, "pass"}, {6000, "pass"}, {6200, "stopped"}, {6200, "mute"}, {6500, "pass"}})

	want := []string{
		"ann started@100 reported",
		"ann unreported@5000 observed",
		"ann stopped@6500 observed muted",
	}
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("transmit rows:\n got  %q\n want %q", got, want)
	}
	// The pairing check finds exactly that one row unclosed, and nothing
	// else wrong.
	problems, open := checkTransmitPairs(logRowsFromAudit(f.audit(t)))
	if len(problems) != 0 || len(open) != 1 || !strings.Contains(open[0], "voice_transmit_started") {
		t.Errorf("pairing: problems %q, open %q; want only the started written before the restart", problems, open)
	}
}

// ---------------------------------------------------------------------------
// The endpoint

// TestVoiceTransmitRefusals runs every refusal. None of them is audited as a
// transmission, creates a room, or reaches LiveKit.
func TestVoiceTransmitRefusals(t *testing.T) {
	const started = `{"state":"started"}`
	// held gives who a session in ops, so that the refusal under test is the
	// only thing between the request and a recorded report.
	held := func(who string) func(*testing.T, *presenceFixture) {
		return func(t *testing.T, f *presenceFixture) { decodeSession(t, f.session(t, who, "ops")) }
	}
	tests := []struct {
		name       string
		opts       presenceOpts
		setup      func(*testing.T, *presenceFixture)
		who        string
		channel    string
		body       string
		wantStatus int
		wantCode   string
		wantDenied string // reason of the one access_denied row, if any
	}{
		// The caller, checked as for a session and in the same order.
		{"no credential", presenceOpts{auth: AuthRequired}, held("ann"), "", "ops", started, 401, "unauthenticated", ""},
		{"a disabled principal", presenceOpts{auth: AuthRequired}, nil, "dora", "ops", started, 401, "unauthenticated", ""},
		{"authentication off", presenceOpts{auth: AuthOff}, nil, "", "ops", started, 400, schema.ErrorCodeVoiceRequiresAuth, ""},
		{"authentication off, unknown channel", presenceOpts{auth: AuthOff}, nil, "", "nosuch", started, 400, schema.ErrorCodeVoiceRequiresAuth, ""},
		{"a non-member", presenceOpts{auth: AuthRequired}, held("ann"), "bob", "ops", started, 404, "channel_not_found", ""},
		{"an unknown channel", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "nosuch", started, 404, "channel_not_found", ""},
		{"an operator who is not a member", presenceOpts{auth: AuthRequired}, held("ann"), "root", "ops", started, 404, "channel_not_found", ""},
		{"a non-member with a malformed body", presenceOpts{auth: AuthRequired}, nil, "bob", "ops", `{"state":`, 404, "channel_not_found", ""},
		{"a non-member, voice not configured", presenceOpts{auth: AuthRequired, unconfigured: true}, nil, "bob", "ops", started, 404, "channel_not_found", ""},
		{"an agent member", presenceOpts{auth: AuthRequired}, held("ann"), "robo", "ops", started, 403, "forbidden", denyAgentVoice},
		{"an agent that is not a member", presenceOpts{auth: AuthRequired}, nil, "robo", "ops2", started, 404, "channel_not_found", denyNotMember},
		{"an agent member, voice not configured", presenceOpts{auth: AuthRequired, unconfigured: true}, nil, "robo", "ops", started, 403, "forbidden", denyAgentVoice},
		{"a member, voice not configured", presenceOpts{auth: AuthRequired, unconfigured: true}, nil, "ann", "ops", started, 503, schema.ErrorCodeVoiceNotConfigured, ""},
		{"a member, voice not configured, malformed body", presenceOpts{auth: AuthRequired, unconfigured: true}, nil, "ann", "ops", `nonsense`, 503, schema.ErrorCodeVoiceNotConfigured, ""},

		// The body, read only for a caller who may report.
		{"an empty body", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", ``, 400, "invalid_request", ""},
		{"not JSON", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `started`, 400, "invalid_request", ""},
		{"no state", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `{}`, 400, "invalid_request", ""},
		{"an unknown state", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `{"state":"paused"}`, 400, "invalid_request", ""},
		{"a state in the wrong case", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `{"state":"Started"}`, 400, "invalid_request", ""},
		{"a time of the client's own", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `{"state":"started","at":"2020-01-01T00:00:00Z"}`, 400, "invalid_request", ""},
		{"a principal of the client's choosing", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `{"state":"started","principal_id":3}`, 400, "invalid_request", ""},
		{"two reports in one body", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", started + `{"state":"stopped"}`, 400, "invalid_request", ""},
		{"an array", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `[` + started + `]`, 400, "invalid_request", ""},
		{"an audience of an unknown kind", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `{"state":"started","audience":{"kind":"room"}}`, 400, "invalid_request", ""},
		{"a body over the bound", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `{"state":"started"` + strings.Repeat(" ", voiceReportMaxBytes) + `}`, 413, "request_too_large", ""},

		// The session.
		{"a member who was never issued a session", presenceOpts{auth: AuthRequired}, nil, "ann", "ops", started, 409, schema.ErrorCodeVoiceNoSession, ""},
		{"a member with no session, reporting stopped", presenceOpts{auth: AuthRequired}, nil, "ann", "ops", `{"state":"stopped"}`, 409, schema.ErrorCodeVoiceNoSession, ""},
		{"a member with no session while another member holds one", presenceOpts{auth: AuthRequired}, held("ann"), "ann2", "ops", started, 409, schema.ErrorCodeVoiceNoSession, ""},
		{"a session in another channel", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops2", started, 409, schema.ErrorCodeVoiceNoSession, ""},
		{"a session under another credential of the same principal", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) {
				held("ann")(t, f)
				_, tok := f.credentialWithExpiry(t, "ann", time.Now().Add(time.Hour))
				f.tokens["ann-other"] = tok
			}, "ann-other", "ops", started, 409, schema.ErrorCodeVoiceNoSession, ""},
		{"a credential whose room was rotated away", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) {
				held("ann")(t, f)
				spareID, spareTok := f.credentialWithExpiry(t, "ann2", time.Now().Add(time.Hour))
				decodeSession(t, f.callREST(t, http.MethodPost, "/v1/channels/ops/voice/session", spareTok, ""))
				if code := f.doOrdered(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", spareID), f.rootTok); code != 204 {
					t.Fatalf("revoke status = %d", code)
				}
				if len(f.audits(t, store.AuditVoiceRoomRotated)) != 1 {
					t.Fatal("the room was not rotated")
				}
			}, "ann", "ops", started, 409, schema.ErrorCodeVoiceNoSession, ""},
		// A holder is a principal and its credential together. The session
		// endpoint never records one without the other, but the check does
		// not lean on that: a row that pairs ann2's credential with another
		// principal makes ann2 the holder of nothing.
		{"a holder row naming this credential for another principal", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) {
				room := f.holder(t, "ann", f.ops)
				if err := f.srv.store.RecordVoiceHolder(context.Background(), room.ID, f.ids["ann"], f.credID(t, "ann2")); err != nil {
					t.Fatal(err)
				}
			}, "ann2", "ops", started, 409, schema.ErrorCodeVoiceNoSession, ""},
		// V4 has no room for any audience but the whole channel.
		{"a net audience", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `{"state":"started","audience":{"kind":"net","net_id":3}}`, 409, schema.ErrorCodeVoiceNoSession, ""},
		{"a whisper audience", presenceOpts{auth: AuthRequired}, held("ann"), "ann", "ops", `{"state":"started","audience":{"kind":"principals","principal_ids":[2,3]}}`, 409, schema.ErrorCodeVoiceNoSession, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, tt.opts)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			rooms, err := f.srv.store.CountVoiceRooms(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			calls := len(f.lk.calls())
			denied := len(f.audits(t, "access_denied"))

			res := f.reportBody(t, tt.who, tt.channel, tt.body)
			if res.status != tt.wantStatus || errCode(t, res.body) != tt.wantCode {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if got := f.transmitRows(t); len(got) != 0 {
				t.Errorf("a refused report was audited as a transmission: %q", got)
			}
			if got := len(f.audits(t, store.AuditVoiceReportRateLimited)); got != 0 {
				t.Errorf("voice_report_rate_limited rows = %d, want 0", got)
			}
			if after, err := f.srv.store.CountVoiceRooms(context.Background()); err != nil || after != rooms {
				t.Errorf("voice_rooms rows = %d (err %v), was %d: a refused report made a room", after, err, rooms)
			}
			if got := len(f.lk.calls()); got != calls {
				t.Errorf("a report made %d requests to LiveKit", got-calls)
			}
			if f.srv.voice.anyInUse() && tt.setup == nil {
				t.Error("a refused report put a room in use")
			}
			rows := f.audits(t, "access_denied")[denied:]
			switch {
			case tt.wantDenied == "" && len(rows) != 0:
				t.Errorf("access_denied rows = %+v, want none", rows)
			case tt.wantDenied != "":
				if len(rows) != 1 || !strings.Contains(rows[0].Detail, "reason="+tt.wantDenied) || rows[0].Subject != "POST /v1/channels/{channel}/voice/transmit" {
					t.Errorf("access_denied rows = %+v, want one with reason=%s for this route", rows, tt.wantDenied)
				}
			}
			// What the caller is told names no room and no credential.
			for _, secret := range []string{"conch-", "conch_", f.tokens["ann"]} {
				if strings.Contains(res.body, secret) {
					t.Errorf("the refusal contains %q: %s", secret, res.body)
				}
			}
		})
	}
}

// TestVoiceTransmitNotFoundIsIdentical: a non-member is answered with the
// unknown-channel 404, byte for byte, and it is the same 404 the session
// endpoint gives the same caller: status, body and content type.
func TestVoiceTransmitNotFoundIsIdentical(t *testing.T) {
	for _, opts := range []presenceOpts{{auth: AuthRequired}, {auth: AuthRequired, unconfigured: true}} {
		f := newPresenceFixture(t, opts)
		if !opts.unconfigured {
			decodeSession(t, f.session(t, "ann", "ops"))
		}
		want := f.report(t, "ann", "nosuch", "started")
		if want.status != 404 || errCode(t, want.body) != "channel_not_found" {
			t.Fatalf("unknown channel answer = %+v", want)
		}
		for _, c := range []struct{ who, channel, body string }{
			{"bob", "ops", `{"state":"started"}`},
			{"bob", "ops", `{"state":"stopped"}`},
			{"bob", "ops", ``},
			{"bob", "ops", `{"state":"started","audience":{"kind":"net","net_id":3}}`},
			{"bob", "nosuch", `{"state":"started"}`},
			{"root", "ops", `{"state":"started"}`},
			{"robo", "ops2", `{"state":"started"}`},
		} {
			if got := f.reportBody(t, c.who, c.channel, c.body); got != want {
				t.Errorf("unconfigured=%v: %s reporting %q in %s: %+v differs from the unknown-channel answer %+v", opts.unconfigured, c.who, c.body, c.channel, got, want)
			}
			if got := f.session(t, c.who, c.channel); got != want {
				t.Errorf("unconfigured=%v: the session endpoint answers %s in %s with %+v, the transmit endpoint with %+v", opts.unconfigured, c.who, c.channel, got, want)
			}
		}
		// A non-member can send as many as it likes: it is never counted
		// against a bound it has no business reaching, and never audited.
		for range voiceReportBurst * 3 {
			if got := f.report(t, "bob", "ops", "started"); got != want {
				t.Fatalf("a non-member's repeated report: %+v", got)
			}
		}
		if n := len(f.audits(t, store.AuditVoiceReportRateLimited)); n != 0 {
			t.Errorf("a non-member's reports wrote %d rate-limit rows", n)
		}
	}
}

// TestVoiceTransmitSameRefusalsAsSession: for every caller the session
// endpoint refuses before it looks at its own state, the transmit endpoint
// gives the same status and code. The two share the code that decides
// (voiceSessionCaller); this is what would notice them drifting apart.
func TestVoiceTransmitSameRefusalsAsSession(t *testing.T) {
	for _, opts := range []presenceOpts{{auth: AuthRequired}, {auth: AuthRequired, unconfigured: true}, {auth: AuthOff}, {auth: AuthOff, unconfigured: true}} {
		f := newPresenceFixture(t, opts)
		for _, who := range []string{"", "ann", "bob", "root", "robo", "dora"} {
			for _, channel := range []string{"ops", "ops2", "nosuch"} {
				// The report goes first: a caller the session endpoint is
				// about to issue a session to holds none yet.
				r := f.report(t, who, channel, "started")
				s := f.session(t, who, channel)
				if s.status == http.StatusOK {
					// Not a refusal: the caller may have a session, and with
					// none yet the transmit endpoint says exactly that.
					if r.status != http.StatusConflict || errCode(t, r.body) != schema.ErrorCodeVoiceNoSession {
						t.Errorf("auth=%q unconfigured=%v %q in %s: a caller who can get a session and has none reports: %d %s, want 409 voice_no_session", opts.auth, opts.unconfigured, who, channel, r.status, r.body)
					}
					continue
				}
				if r != s {
					t.Errorf("auth=%q unconfigured=%v %q in %s: session %+v, transmit %+v", opts.auth, opts.unconfigured, who, channel, s, r)
				}
			}
		}
	}
}

// TestVoiceTransmitWithLiveKitDown: the endpoint never contacts LiveKit, so
// with LiveKit unreachable a holder's reports are still accepted and recorded.
// (The poller cannot check them until LiveKit answers again.)
func TestVoiceTransmitWithLiveKitDown(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	decodeSession(t, f.session(t, "ann", "ops"))
	f.lk.setListStatus(http.StatusInternalServerError)
	f.at(0)
	f.pass(t)
	if s := f.snap(t, f.ops); s.Available {
		t.Fatal("presence is available with LiveKit failing")
	}
	calls := len(f.lk.calls())
	f.at(100)
	f.mustReport(t, "ann", "started")
	f.at(900)
	f.mustReport(t, "ann", "stopped")
	if got := len(f.lk.calls()); got != calls {
		t.Errorf("the reports made %d requests to LiveKit", got-calls)
	}
	want := []string{"ann started@100 reported", "ann stopped@900 reported"}
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
	}
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitBound: reports are bounded per principal by a bucket of
// voiceReportBurst refilled at voiceReportPerSecond. Past it the answer is
// 429, and refusals are audited once per window, not per request.
func TestVoiceTransmitBound(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	decodeSession(t, f.session(t, "ann", "ops"))
	decodeSession(t, f.session(t, "ann2", "ops"))
	f.at(0)

	states := []string{"started", "stopped"}
	send := func(who string, i int) wireResult {
		t.Helper()
		rec := f.do(t, http.MethodPost, "/v1/channels/ops/voice/transmit", f.tokens[who], `{"state":"`+states[i%2]+`"}`)
		res := wireResult{rec.Code, rec.Body.String(), rec.Header().Get("Content-Type")}
		if rec.Code == http.StatusTooManyRequests {
			if errCode(t, res.body) != schema.ErrorCodeVoiceReportRateLimited {
				t.Fatalf("429 body = %s", res.body)
			}
			if rec.Header().Get("Retry-After") == "" {
				t.Error("a 429 without Retry-After")
			}
		}
		return res
	}
	limited := func() []store.AuditEvent { return f.audits(t, store.AuditVoiceReportRateLimited) }

	// The burst: thirty reports at one instant, each a change of state.
	for i := range voiceReportBurst {
		if res := send("ann", i); res.status != http.StatusNoContent {
			t.Fatalf("report %d of the burst: %d %s", i+1, res.status, res.body)
		}
	}
	if got := len(f.transmitRows(t)); got != voiceReportBurst {
		t.Fatalf("transmit rows after the burst = %d, want %d", got, voiceReportBurst)
	}
	// The next is refused, and that refusal is the one row.
	if res := send("ann", 0); res.status != http.StatusTooManyRequests {
		t.Fatalf("report past the bound: %d %s", res.status, res.body)
	}
	rows := limited()
	if len(rows) != 1 {
		t.Fatalf("rate-limit rows = %d, want 1", len(rows))
	}
	wantDetail := fmt.Sprintf("channel=%d burst=%d per_second=%d window_seconds=%d suppressed=0", f.ops.ID, voiceReportBurst, voiceReportPerSecond, int(voiceReportRefusalWindow/time.Second))
	if rows[0].Actor != f.actor("ann") || rows[0].Subject != fmt.Sprintf("channel:%d", f.ops.ID) || rows[0].Detail != wantDetail || !rows[0].CreatedAt.Equal(ruleT0) {
		t.Errorf("rate-limit row = %+v, want actor %s, the channel, detail %q, timed at the refusal", rows[0], f.actor("ann"), wantDetail)
	}
	// A flood writes nothing more: no refusal row, no transmit row, no
	// change of the reported state.
	for i := range 200 {
		if res := send("ann", i); res.status != http.StatusTooManyRequests {
			t.Fatalf("flood request %d: %d %s", i, res.status, res.body)
		}
	}
	if got := len(limited()); got != 1 {
		t.Errorf("rate-limit rows after a flood = %d, want 1", got)
	}
	if got := len(f.transmitRows(t)); got != voiceReportBurst {
		t.Errorf("transmit rows after a flood = %d, want %d: a refused report was applied", got, voiceReportBurst)
	}
	// Another principal has its own bucket.
	if res := send("ann2", 0); res.status != http.StatusNoContent {
		t.Errorf("ann2 while ann is over the bound: %d %s", res.status, res.body)
	}
	if res := send("ann2", 1); res.status != http.StatusNoContent {
		t.Errorf("ann2's second report: %d %s", res.status, res.body)
	}

	// Refilled at four a second: 249 ms buys nothing, 250 ms one report.
	f.at(249)
	if res := send("ann", 0); res.status != http.StatusTooManyRequests {
		t.Errorf("249 ms later: %d, want still refused", res.status)
	}
	f.at(250)
	if res := send("ann", 0); res.status != http.StatusNoContent {
		t.Errorf("250 ms later: %d %s, want one report allowed", res.status, res.body)
	}
	if res := send("ann", 1); res.status != http.StatusTooManyRequests {
		t.Errorf("and the one after it: %d, want refused", res.status)
	}
	if got := len(limited()); got != 1 {
		t.Errorf("rate-limit rows inside the window = %d, want still 1", got)
	}
	// A second of quiet buys four.
	f.at(1250)
	for i := 1; i <= voiceReportPerSecond; i++ {
		if res := send("ann", i); res.status != http.StatusNoContent {
			t.Errorf("after a second, report %d: %d %s", i, res.status, res.body)
		}
	}
	if res := send("ann", 1); res.status != http.StatusTooManyRequests {
		t.Errorf("the fifth after a second: %d, want refused", res.status)
	}

	// The window: one millisecond short of a minute after the audited
	// refusal nothing is written; at the minute the next refusal is, and it
	// says how many went unwritten: 200 in the flood, one at 249 ms, one at
	// 250 ms, one at 1250 ms, and the one just below.
	window := int(voiceReportRefusalWindow / time.Millisecond)
	f.at(window - 1)
	for i := range voiceReportBurst + 1 {
		send("ann", i)
	}
	if got := len(limited()); got != 1 {
		t.Fatalf("rate-limit rows just inside the window = %d, want 1", got)
	}
	f.at(window)
	if res := send("ann", 0); res.status != http.StatusTooManyRequests {
		t.Fatalf("at the end of the window: %d, want refused (the bucket was just emptied)", res.status)
	}
	rows = limited()
	if len(rows) != 2 || !strings.HasSuffix(rows[1].Detail, " suppressed=204") || !rows[1].CreatedAt.Equal(ruleT0.Add(voiceReportRefusalWindow)) {
		t.Fatalf("rate-limit rows = %+v, want a second one, timed at the minute, with suppressed=204", rows)
	}

	// Left alone the bucket fills to the burst and no further.
	f.at(window + 3_600_000)
	ok := 0
	for i := range voiceReportBurst * 2 {
		if send("ann", i).status == http.StatusNoContent {
			ok++
		}
	}
	if ok != voiceReportBurst {
		t.Errorf("after an hour of quiet %d reports were allowed at once, want %d", ok, voiceReportBurst)
	}

	// Everything the flood did write pairs up once the last start is closed.
	f.at(window + 3_600_000 + 10_000)
	f.mustReport(t, "ann", "stopped")
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitBoundCountsEveryRequest: every request from a caller who
// may report at all counts against the bound, whatever becomes of it. A flood
// of malformed bodies, of reports with no session, or of reports that change
// nothing is cut off like any other, and none of it is a transmission.
func TestVoiceTransmitBoundCountsEveryRequest(t *testing.T) {
	tests := []struct {
		name       string
		who        string
		body       string
		wantStatus int
	}{
		{"malformed bodies", "ann", `{"state":"paused"}`, http.StatusBadRequest},
		{"bodies over the size bound", "ann", `{"state":"started"` + strings.Repeat(" ", voiceReportMaxBytes) + `}`, http.StatusRequestEntityTooLarge},
		{"reports for an audience with no room", "ann", `{"state":"started","audience":{"kind":"net","net_id":3}}`, http.StatusConflict},
		{"reports from a member with no session", "ann2", `{"state":"started"}`, http.StatusConflict},
		{"reports that change nothing", "ann", `{"state":"stopped"}`, http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			decodeSession(t, f.session(t, "ann", "ops"))
			f.at(0)
			for i := range voiceReportBurst {
				if res := f.reportBody(t, tt.who, "ops", tt.body); res.status != tt.wantStatus {
					t.Fatalf("request %d: %d %s, want %d", i+1, res.status, res.body, tt.wantStatus)
				}
			}
			for i := range 5 {
				if res := f.reportBody(t, tt.who, "ops", tt.body); res.status != http.StatusTooManyRequests || errCode(t, res.body) != schema.ErrorCodeVoiceReportRateLimited {
					t.Fatalf("request %d past the bound: %d %s, want 429", i+1, res.status, res.body)
				}
			}
			if got := len(f.audits(t, store.AuditVoiceReportRateLimited)); got != 1 {
				t.Errorf("rate-limit rows = %d, want 1", got)
			}
			if got := f.transmitRows(t); len(got) != 0 {
				t.Errorf("transmit rows = %q, want none", got)
			}
		})
	}
}

// TestVoiceTransmitDoesNotChooseWhenAPassHappens: a report for a room that is
// already being polled wakes nothing, whether or not it changes the reported
// state. The gaps between passes are random so that a client cannot know when
// the next one is; a report that brought one forward would tell it.
func TestVoiceTransmitDoesNotChooseWhenAPassHappens(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	f.runLive(t, room, []liveStep{{0, "join"}, {0, "pass"}})
	select {
	case <-f.srv.voice.wakeLoop: // the session's own wake-up
	default:
	}
	passes := f.lk.count("ListParticipants")
	for i, state := range []string{"started", "started", "stopped", "stopped", "started", "stopped"} {
		f.at(100 + 10*i)
		f.mustReport(t, "ann", state)
		select {
		case <-f.srv.voice.wakeLoop:
			t.Fatalf("report %d (%s) for a room in use woke the loop", i+1, state)
		default:
		}
	}
	if got := f.lk.count("ListParticipants"); got != passes {
		t.Errorf("the reports caused %d polls of the room", got-passes)
	}
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceReportLimiter is the bucket by itself, with explicit times.
func TestVoiceReportLimiter(t *testing.T) {
	at := func(ms int) func() time.Time {
		return func() time.Time { return ruleT0.Add(time.Duration(ms) * time.Millisecond) }
	}
	type call struct {
		ms             int
		pid            int64
		wantAllowed    bool
		wantAudit      bool
		wantSuppressed int
	}
	// drain is voiceReportBurst allowed calls for pid at ms.
	drain := func(ms int, pid int64) []call {
		calls := make([]call, voiceReportBurst)
		for i := range calls {
			calls[i] = call{ms, pid, true, false, 0}
		}
		return calls
	}
	tests := []struct {
		name  string
		calls []call
	}{
		{"the burst, then refused, audited once", append(drain(0, 1),
			call{0, 1, false, true, 0}, call{0, 1, false, false, 0}, call{100, 1, false, false, 0})},
		{"one report every 250 ms for ever", []call{
			{0, 1, true, false, 0}, {250, 1, true, false, 0}, {500, 1, true, false, 0}, {750, 1, true, false, 0},
		}},
		{"a quarter second buys one report, not two", append(drain(0, 1),
			call{249, 1, false, true, 0}, call{250, 1, true, false, 0}, call{251, 1, false, false, 0}, call{499, 1, false, false, 0}, call{500, 1, true, false, 0})},
		{"principals do not share a bucket", append(drain(0, 1),
			call{0, 1, false, true, 0}, call{0, 2, true, false, 0}, call{0, 1, false, false, 0})},
		{"a refusal inside the window writes nothing; one after it does, and counts those", append(drain(0, 1),
			call{0, 1, false, true, 0}, call{1, 1, false, false, 0}, call{2, 1, false, false, 0},
			// The bucket has filled by now; it is emptied again a
			// millisecond before the window ends.
			call{59_998, 1, true, false, 0})},
		{"the bucket never holds more than the burst", append(append(drain(0, 1), drain(3_600_000, 1)...),
			call{3_600_000, 1, false, true, 0})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var l voiceReportLimiter
			for i, c := range tt.calls {
				allowed, audit, suppressed, when := l.take(c.pid, at(c.ms))
				if allowed != c.wantAllowed || audit != c.wantAudit || suppressed != c.wantSuppressed {
					t.Fatalf("call %d (principal %d at %d ms): allowed=%v audit=%v suppressed=%d, want %v %v %d", i, c.pid, c.ms, allowed, audit, suppressed, c.wantAllowed, c.wantAudit, c.wantSuppressed)
				}
				if !when.Equal(at(c.ms)()) {
					t.Fatalf("call %d: time %v, want the clock's", i, when)
				}
			}
		})
	}

	t.Run("a new window after a minute says what was suppressed", func(t *testing.T) {
		var l voiceReportLimiter
		for range voiceReportBurst {
			l.take(1, at(0))
		}
		for i := range 8 {
			if _, audit, _, _ := l.take(1, at(0)); audit != (i == 0) {
				t.Fatalf("refusal %d: audit=%v", i, audit)
			}
		}
		// Keep it empty across the minute, so the refusal at 60 s is one.
		for ms := 250; ms < 60_000; ms += 250 {
			l.take(1, at(ms))
		}
		for range 3 {
			if allowed, audit, _, _ := l.take(1, at(59_999)); allowed || audit {
				t.Fatalf("at 59.999 s: allowed=%v audit=%v, want a refusal that writes nothing", allowed, audit)
			}
		}
		l.take(1, at(60_000)) // the token that 60 s buys
		allowed, audit, suppressed, _ := l.take(1, at(60_000))
		if allowed || !audit || suppressed != 7+3 {
			t.Errorf("at 60 s: allowed=%v audit=%v suppressed=%d, want a refusal, audited, with the 10 unwritten ones counted", allowed, audit, suppressed)
		}
	})

	t.Run("idle buckets are dropped", func(t *testing.T) {
		var l voiceReportLimiter
		for pid := int64(1); pid <= 50; pid++ {
			l.take(pid, at(0))
		}
		l.take(99, at(10*60_000))
		if got := len(l.buckets); got != 1 {
			t.Errorf("buckets ten minutes later = %d, want only the one just used", got)
		}
		// One whose refusal window is still running is kept, though it has
		// had time to fill: dropping it would let the next refusal write a
		// row before the window was up. Buckets are looked over at 11:00
		// (a minute after the last time) and again at 12:05; 7 is refused at
		// 11:30, so at 12:05 its window has 25 s to run.
		l.take(99, at(11*60_000))
		for range voiceReportBurst + 1 {
			l.take(7, at(11*60_000+30_000))
		}
		l.take(99, at(12*60_000+5_000))
		if _, ok := l.buckets[7]; !ok {
			t.Fatal("a bucket inside its refusal window was dropped")
		}
		for range voiceReportBurst {
			l.take(7, at(12*60_000+5_000))
		}
		if allowed, audit, _, _ := l.take(7, at(12*60_000+5_000)); allowed || audit {
			t.Errorf("a refusal 35 s after the audited one: allowed=%v audit=%v, want refused and not audited", allowed, audit)
		}
		// Once the window is over and it has filled, it goes.
		l.take(99, at(14*60_000))
		if _, ok := l.buckets[7]; ok {
			t.Error("an idle bucket whose refusal window is over was kept")
		}
	})
}

// TestVoiceReportBoundIsWhatIsDocumented pins the numbers the bound is stated
// with (voice-control-plane.md §7, the issue): a burst of 30 reports, 4 a
// second after that, refusals audited once a minute. Two presses a second,
// sustained, never reach it; changing a number is a decision, not a tweak.
func TestVoiceReportBoundIsWhatIsDocumented(t *testing.T) {
	if voiceReportBurst != 30 || voiceReportPerSecond != 4 || voiceReportRefusalWindow != time.Minute {
		t.Errorf("the bound is a burst of %d, %d a second, refusals audited every %v; the documents say 30, 4 and a minute",
			voiceReportBurst, voiceReportPerSecond, voiceReportRefusalWindow)
	}
	if voiceReportCost != 250*time.Millisecond {
		t.Errorf("a report costs %v of credit, want 250 ms", voiceReportCost)
	}
	// The fastest plausible key use: five presses a second (ten reports),
	// for three seconds, from a full bucket. All of it is allowed.
	var l voiceReportLimiter
	for ms := 0; ms < 3000; ms += 100 {
		if allowed, _, _, _ := l.take(1, func() time.Time { return ruleT0.Add(time.Duration(ms) * time.Millisecond) }); !allowed {
			t.Fatalf("five presses a second were refused after %d ms", ms)
		}
	}
	// And two presses a second for an hour.
	var steady voiceReportLimiter
	for ms := 0; ms < 3_600_000; ms += 250 {
		if allowed, _, _, _ := steady.take(1, func() time.Time { return ruleT0.Add(time.Duration(ms) * time.Millisecond) }); !allowed {
			t.Fatalf("two presses a second were refused after %d ms", ms)
		}
	}
}

// TestVoicePassGap: the gap between passes is 100 ms plus an exponentially
// distributed time with mean 400 ms, capped at 1.5 s, drawn from the operating
// system's random source afresh for every gap.
//
// The bounds on the statistics are six or more standard errors wide at this
// number of draws: a correct generator fails them about once in a billion
// runs, and a wrong distribution (uniform, a different mean, no floor, no cap)
// is far outside them.
func TestVoicePassGap(t *testing.T) {
	if voicePassGapMin != 100*time.Millisecond || voicePassGapMean != 400*time.Millisecond || voicePassGapMax != 1500*time.Millisecond {
		t.Fatalf("the gap is %v plus an exponential with mean %v, capped at %v; want 100 ms, 400 ms and 1.5 s", voicePassGapMin, voicePassGapMean, voicePassGapMax)
	}
	const draws = 40000
	gaps := make([]time.Duration, draws)
	seen := make(map[time.Duration]bool, draws)
	lo, hi := voicePassGapMax, voicePassGapMin
	var sum time.Duration
	for i := range gaps {
		d := voicePassGap()
		if d < voicePassGapMin || d > voicePassGapMax {
			t.Fatalf("a gap of %v is outside %v to %v", d, voicePassGapMin, voicePassGapMax)
		}
		gaps[i] = d
		seen[d] = true
		lo, hi = min(lo, d), max(hi, d)
		sum += d
	}
	// share is the fraction of the gaps that pass a test.
	share := func(of []time.Duration, pass func(time.Duration) bool) float64 {
		n := 0
		for _, d := range of {
			if pass(d) {
				n++
			}
		}
		return float64(n) / float64(len(of))
	}
	// tail is what an exponential with the mean gives for exceeding d.
	tail := func(d time.Duration) float64 { return math.Exp(-float64(d) / float64(voicePassGapMean)) }
	near := func(what string, got, want, tolerance float64) {
		t.Helper()
		if math.Abs(got-want) > tolerance {
			t.Errorf("%s = %.4f, want %.4f within %.4f", what, got, want, tolerance)
		}
	}

	// Not constant, and not a handful of values: drawn at nanosecond
	// resolution, so all but the capped ones differ.
	if len(seen) < draws*9/10 {
		t.Errorf("%d draws gave %d different gaps", draws, len(seen))
	}
	// The floor is reached and so is the cap: a known pass promises 100 ms
	// of quiet and no more, and nothing waits longer than 1.5 s.
	if lo > voicePassGapMin+2*time.Millisecond || hi != voicePassGapMax {
		t.Errorf("gaps ran from %v to %v, want from within 2 ms of %v to exactly %v", lo, hi, voicePassGapMin, voicePassGapMax)
	}
	// The mean: the floor, plus the exponential's mean less what the cap
	// cuts off. About 488 ms.
	wantMean := float64(voicePassGapMin) + float64(voicePassGapMean)*(1-tail(voicePassGapMax-voicePassGapMin))
	near("the mean gap in ms", float64(sum)/draws/1e6, wantMean/1e6, 12)
	// What an exponential gives, at several points: 12% of gaps are under
	// 150 ms, 46% under 350 ms (the old least gap), 63% under 500 ms, 89%
	// under 1 s, and 3% are cut to the cap.
	for _, under := range []time.Duration{150 * time.Millisecond, 350 * time.Millisecond, 500 * time.Millisecond, time.Second} {
		got := share(gaps, func(d time.Duration) bool { return d < under })
		near(fmt.Sprintf("the share of gaps under %v", under), got, 1-tail(under-voicePassGapMin), 0.015)
	}
	near("the share of gaps at the cap", share(gaps, func(d time.Duration) bool { return d == voicePassGapMax }), tail(voicePassGapMax-voicePassGapMin), 0.006)
	// No memory: however long it has been since the last pass (past the
	// floor), the chance that the next 200 ms go by without one is the same,
	// 61%. With the evenly bounded gap this replaced, it fell to nothing as
	// the wait went on, which is what let a client time itself.
	for _, waited := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 600 * time.Millisecond} {
		var still []time.Duration
		for _, d := range gaps {
			if d > waited {
				still = append(still, d)
			}
		}
		got := share(still, func(d time.Duration) bool { return d > waited+200*time.Millisecond })
		near(fmt.Sprintf("the share of gaps over %v that go on another 200 ms", waited), got, tail(200*time.Millisecond), 0.03)
	}

	// The poller draws from it, once for every wait, while a room is in use.
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, LiveKit: voiceConfig(t, newScriptedLiveKit(t).URL)})
	ctx := context.Background()
	ch, err := srv.store.CreateChannel(ctx, "ops")
	if err != nil {
		t.Fatal(err)
	}
	room, err := srv.store.ChannelVoiceRoom(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Any sweep time will do; it only has to be further off than a gap.
	srv.voice.mu.Lock()
	srv.voice.nextSweep = time.Now().Add(time.Hour)
	srv.voice.mu.Unlock()
	if d := srv.voice.nextDelay(); d < time.Minute {
		t.Fatalf("idle delay = %v, want the wait for the sweep", d)
	}
	srv.voice.noteSession(room)
	delays := map[time.Duration]bool{}
	for range 50 {
		d := srv.voice.nextDelay()
		if d < voicePassGapMin || d > voicePassGapMax {
			t.Fatalf("delay with a room in use = %v, outside %v to %v", d, voicePassGapMin, voicePassGapMax)
		}
		delays[d] = true
	}
	if len(delays) < 40 {
		t.Errorf("50 waits used %d different gaps: the gaps are all but equal", len(delays))
	}

	// And the source is crypto/rand: the file that draws the gap imports it
	// and no generator that could be seeded from the clock.
	imports := sourceImports(t, "voice_presence.go")
	if !slices.Contains(imports, "crypto/rand") {
		t.Error("voice_presence.go does not import crypto/rand")
	}
	for _, banned := range []string{"math/rand", "math/rand/v2"} {
		if slices.Contains(imports, banned) {
			t.Errorf("voice_presence.go imports %s", banned)
		}
	}
}

// TestVoiceGapIsDrawnOnlyForAPass: the loop draws a gap when there is a pass
// to wait for, once for each wait, and not while it is idle until the next
// sweep. The gap function is what its comment says it is, the pause before the
// next pass while a room is in use; one that hands out a fixed sequence is not
// used up by an idle server.
func TestVoiceGapIsDrawnOnlyForAPass(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	draws := 0
	f.srv.voice.gap = func() time.Duration {
		draws++
		return testPassGap
	}
	f.sweep(t)
	for range 5 {
		if d := f.srv.voice.nextDelay(); d != voiceSweepInterval {
			t.Fatalf("idle delay = %v, want the wait for the sweep", d)
		}
	}
	if draws != 0 {
		t.Errorf("an idle loop drew %d gaps in 5 waits, want none", draws)
	}
	f.inUse(t, f.ops)
	for range 5 {
		if d := f.srv.voice.nextDelay(); d != testPassGap {
			t.Fatalf("delay with a room in use = %v, want the gap drawn", d)
		}
	}
	if draws != 5 {
		t.Errorf("5 waits with a room in use drew %d gaps, want one each", draws)
	}
}

// TestVoiceTransmitAuditFailure: a report whose audit row cannot be written
// is not applied. The caller is told it failed, and the reported state does
// not say more than the audit log does: retried once the store works, the
// report is a change again and is recorded.
func TestVoiceTransmitAuditFailure(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	f.runLive(t, room, []liveStep{{0, "join"}, {0, "pass"}})

	write := f.srv.voice.appendAudit
	failing := true
	f.srv.voice.appendAudit = func(ctx context.Context, actor, action, subject, detail string, at time.Time) (store.AuditEvent, error) {
		if failing {
			return store.AuditEvent{}, errors.New("disk full")
		}
		return write(ctx, actor, action, subject, detail, at)
	}
	f.at(100)
	if res := f.report(t, "ann", "ops", "started"); res.status != http.StatusInternalServerError {
		t.Fatalf("a report that could not be audited: %d %s, want 500", res.status, res.body)
	}
	if got := f.transmitRows(t); len(got) != 0 {
		t.Fatalf("rows = %q", got)
	}
	// Had the report stayed applied, the poller would take the press that
	// follows as accounted for and the log would never have it.
	failing = false
	f.runLive(t, room, []liveStep{{200, "unmute"}, {500, "pass"}, {1000, "pass"}, {1100, "started"}, {1500, "pass"}, {1600, "stopped"}, {1600, "mute"}, {2000, "pass"}})
	want := []string{"ann unreported@500 observed", "ann started@1100 reported", "ann stopped@1500 observed reported", "ann stopped@1600 reported"}
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("rows:\n got  %q\n want %q", got, want)
	}
	// The same for a stopped: it stays reported started until one is written.
	f.at(3000)
	f.mustReport(t, "ann", "started")
	failing = true
	f.at(3100)
	if res := f.report(t, "ann", "ops", "stopped"); res.status != http.StatusInternalServerError {
		t.Fatalf("a stopped that could not be audited: %d %s, want 500", res.status, res.body)
	}
	failing = false
	f.at(3200)
	f.mustReport(t, "ann", "stopped")
	if got := f.transmitRows(t); len(got) != 6 || got[5] != "ann stopped@3200 reported" {
		t.Errorf("rows = %q, want the retried stopped written at 3200", got)
	}
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitRacingARotation: a rotation can land between the endpoint's
// holder check and the report being applied, after the room's state was
// dropped. The report must not leave state behind for a room nobody can be in:
// what it opened is closed at once with reason=left and the state is gone.
func TestVoiceTransmitRacingARotation(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	decodeSession(t, f.session(t, "ann", "ops"))
	room, err := f.srv.store.ChannelVoiceRoom(ctx, f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The endpoint has found ann to be a holder of room. Now the rotation.
	if _, rotated, err := f.srv.store.RotateVoiceRoom(ctx, room.ID, store.VoiceRotateRevoked); err != nil || !rotated {
		t.Fatalf("rotate: %v %v", rotated, err)
	}
	f.srv.voice.forgetRoom(ctx, room.RoomName)

	f.at(100)
	if err := f.srv.voice.reportTransmit(ctx, room, f.ids["ann"], true); err != nil {
		t.Fatal(err)
	}
	f.srv.voice.mu.Lock()
	_, kept := f.srv.voice.rooms[room.RoomName]
	f.srv.voice.mu.Unlock()
	if kept {
		t.Error("the report left state behind for a retired room")
	}
	if f.srv.voice.anyInUse() {
		t.Error("a retired room is in use")
	}
	want := []string{"ann started@100 reported", "ann stopped@100 observed left"}
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("rows:\n got  %q\n want %q", got, want)
	}
	// A stopped for a room the poller knows nothing of changes nothing and
	// makes no state either.
	if err := f.srv.voice.reportTransmit(ctx, room, f.ids["ann"], false); err != nil {
		t.Fatal(err)
	}
	f.srv.voice.mu.Lock()
	_, kept = f.srv.voice.rooms[room.RoomName]
	f.srv.voice.mu.Unlock()
	if kept || len(f.transmitRows(t)) != 2 {
		t.Error("a stopped report for an unknown room made state or a row")
	}
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitKeepsSecrets: over a run with reports, refusals of every
// kind, an unreported transmission, the bound and a rotation, no token, secret
// or room name appears in an audit row, a log line or a response.
func TestVoiceTransmitKeepsSecrets(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	resp := decodeSession(t, f.session(t, "ann", "ops"))
	token, room := resp.Rooms[0].Token, resp.Rooms[0].Room
	spareID, spareTok := f.credentialWithExpiry(t, "ann2", time.Now().Add(time.Hour))
	decodeSession(t, f.callREST(t, http.MethodPost, "/v1/channels/ops/voice/session", spareTok, ""))

	var bodies []string
	keep := func(res wireResult) { bodies = append(bodies, res.body) }
	f.runLive(t, room, []liveStep{{0, "join"}, {0, "pass"}, {100, "started"}, {100, "unmute"}, {500, "pass"}, {600, "stopped"}, {1000, "pass"}, {1500, "pass"}})
	keep(f.report(t, "bob", "ops", "started"))
	keep(f.report(t, "robo", "ops", "started"))
	keep(f.report(t, "ann2", "ops", "started"))
	keep(f.reportBody(t, "ann", "ops", `{"state":"started","room":"`+room+`","token":"`+token+`"}`))
	keep(f.reportBody(t, "ann", "ops", `{"state":"`+token+`"}`))
	keep(f.reportBody(t, "ann", "ops", `{"state":"started","audience":{"kind":"net","net_id":1}}`))
	for range voiceReportBurst + 5 {
		keep(f.report(t, "ann", "ops", "started"))
	}
	// A report that cannot be audited, and so logs.
	write := f.srv.voice.appendAudit
	f.srv.voice.appendAudit = func(context.Context, string, string, string, string, time.Time) (store.AuditEvent, error) {
		return store.AuditEvent{}, errors.New("disk full")
	}
	f.at(60_000)
	keep(f.report(t, "ann", "ops", "stopped"))
	f.srv.voice.appendAudit = write
	if code := f.doOrdered(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", spareID), f.rootTok); code != 204 {
		t.Fatalf("revoke status = %d", code)
	}
	keep(f.report(t, "ann", "ops", "stopped"))

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
		"ann's login token": f.tokens["ann"], "ann2's login token": spareTok,
		"the new room's name": f.room(t, f.ops),
	} {
		if strings.Contains(text, secret) {
			t.Errorf("%s appears in an audit row, log line or response", what)
		}
	}
	for _, exercised := range []string{
		store.AuditVoiceTransmitUnreported, store.AuditVoiceReportRateLimited, store.AuditVoiceRoomRotated,
		"reason=left", schema.ErrorCodeVoiceNoSession, "could not be audited", "over the bound", "invalid_request",
	} {
		if !strings.Contains(text, exercised) {
			t.Errorf("the run did not exercise %q", exercised)
		}
	}
}

// ---------------------------------------------------------------------------
// Nobody chooses when a pass happens
//
// The rule rests on a client not knowing when the next pass is. A wake-up of
// the loop runs a pass over every room in use, at once, and starts a new gap;
// so whoever can cause a wake-up knows when a pass has just happened, and so
// when none can. The security review of #135 did it with a second channel:
// a report (or a session request) for an idle room woke the loop, and a member
// of two channels transmitted in the first for 95% of the time unrecorded.
// The loop is now woken only when no room at all was in use.

// idleSecondChannel sets up that member: ann holds a session in ops2 from long
// ago, so its room is idle, and one in ops from just now, so its room is in
// use; she is in the ops room, muted. It returns the two room names, with the
// wake-up her sessions queued taken off.
func idleSecondChannel(t *testing.T, f *presenceFixture) (ops, ops2 string) {
	t.Helper()
	ops2 = decodeSession(t, f.session(t, "ann", "ops2")).Rooms[0].Room
	f.clock.Advance(voiceSessionRecent + time.Minute)
	ops = decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	f.lk.setRoom(ops, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000, published: true, muted: true})
	select {
	case <-f.srv.voice.wakeLoop:
	default:
	}
	return ops, ops2
}

// TestVoiceLoopIsWokenOnlyWhenNothingWasInUse: a session or a report for a
// room that was idle wakes the loop if no room at all was in use (the first
// people into an idle server are seen at once), and wakes nothing if any other
// room was: that room's passes are not the caller's to schedule. The room that
// has just come into use is polled by the next pass.
func TestVoiceLoopIsWokenOnlyWhenNothingWasInUse(t *testing.T) {
	tests := []struct {
		name string
		// otherInUse: ops is in use when the request for ops2 is made.
		otherInUse bool
		request    func(t *testing.T, f *presenceFixture)
		wantWake   bool
	}{
		{"a report for an idle room, another room in use", true,
			func(t *testing.T, f *presenceFixture) { f.reportInOps2(t, "started") }, false},
		{"a session for an idle room, another room in use", true,
			func(t *testing.T, f *presenceFixture) { decodeSession(t, f.session(t, "ann", "ops2")) }, false},
		{"a report for an idle room, nothing in use", false,
			func(t *testing.T, f *presenceFixture) { f.reportInOps2(t, "started") }, true},
		{"a session for an idle room, nothing in use", false,
			func(t *testing.T, f *presenceFixture) { decodeSession(t, f.session(t, "ann", "ops2")) }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			ops, ops2 := idleSecondChannel(t, f)
			if !tt.otherInUse {
				// Long after both sessions, and nobody in either room.
				f.lk.setRoom(ops)
				f.pass(t)
				f.clock.Advance(voiceSessionRecent + time.Minute)
			}
			if got := f.srv.voice.anyInUse(); got != tt.otherInUse {
				t.Fatalf("before the request: a room in use = %v, want %v", got, tt.otherInUse)
			}
			polls := f.lk.count("ListParticipants")

			tt.request(t, f)

			if got := len(f.srv.voice.wakeLoop); (got == 1) != tt.wantWake {
				t.Errorf("wake-ups queued by the request = %d; want one = %v", got, tt.wantWake)
			}
			if got := f.lk.count("ListParticipants"); got != polls {
				t.Errorf("the request itself polled LiveKit %d times", got-polls)
			}
			// Either way the room is in use now, and the next pass polls it.
			before := f.lk.callsFor("ListParticipants", ops2)
			f.pass(t)
			if got := f.lk.callsFor("ListParticipants", ops2) - before; got != 1 {
				t.Errorf("the next pass polled the room that came into use %d times, want 1", got)
			}
		})
	}
}

// reportInOps2 sends a report from ann in ops2, her second channel, that must
// succeed.
func (f *presenceFixture) reportInOps2(t *testing.T, state string) {
	t.Helper()
	if res := f.report(t, "ann", "ops2", state); res.status != http.StatusNoContent {
		t.Fatalf("ann reports %s in ops2: %d %s", state, res.status, res.body)
	}
}

// TestVoiceRunningLoopIsNotWokenForASecondRoom is the review's probe against
// the real loop, made exact. The loop's gap function is the test's: it says
// when a turn of the loop has finished, and holds the loop there until the
// test lets it go on, with a wait so long that no pass happens by itself.
//
// With the loop asleep and ops in use, ann makes her request for idle ops2.
// The test then queues one wake-up of its own and waits for one turn of the
// loop. If the request woke the loop, that turn is the request's, and the
// test's wake-up is still in the queue when it ends. If it did not, the turn
// is the test's and the queue is empty: nothing the request did ran a pass.
func TestVoiceRunningLoopIsNotWokenForASecondRoom(t *testing.T) {
	requests := map[string]func(t *testing.T, f *presenceFixture){
		"a report":  func(t *testing.T, f *presenceFixture) { f.reportInOps2(t, "started") },
		"a session": func(t *testing.T, f *presenceFixture) { decodeSession(t, f.session(t, "ann", "ops2")) },
	}
	for name, request := range requests {
		t.Run(name, func(t *testing.T) {
			f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
			ops, _ := idleSecondChannel(t, f)
			p := f.srv.voice
			turned := make(chan struct{}, 64)
			goOn := make(chan struct{})
			p.gap = func() time.Duration {
				turned <- struct{}{}
				<-goOn
				return time.Hour
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- f.srv.Serve(ctx) }()
			defer func() {
				close(goOn) // whatever turn the loop is in, let it finish
				cancel()
				if err := <-done; err != nil {
					t.Error(err)
				}
			}()
			// The first turn: the startup sweep and a pass. Then the loop
			// sleeps (the manual clock stands still, so until the next
			// sweep, half a minute of real time away).
			<-turned
			if got := f.who(t, f.ops); got != "ann" {
				t.Fatalf("after the first pass: %q, want ann", got)
			}
			goOn <- struct{}{}
			polls := f.lk.callsFor("ListParticipants", ops)

			request(t, f)

			p.wakeLoop <- struct{}{} // the test's own wake-up; waits if one is queued
			<-turned
			if n := len(p.wakeLoop); n != 0 {
				t.Errorf("the request for an idle room woke the loop: it ran a turn of its own, and the test's wake-up is still queued (%d)", n)
			}
			if got := f.lk.callsFor("ListParticipants", ops) - polls; got != 1 {
				t.Errorf("the room in use was polled %d times since the request, want once, by the test's wake-up", got)
			}
		})
	}
}

// fakeLoop plays voicePoller.loop against the manual clock, with the poller's
// own tick and nextDelay: a turn when the wait is over, a turn at once when a
// wake-up is queued, and after either a new wait.
type fakeLoop struct {
	t       *testing.T
	f       *presenceFixture
	next    time.Time
	forced  int // turns run because a wake-up was queued
	natural int // turns run because the wait was over
}

func newFakeLoop(t *testing.T, f *presenceFixture) *fakeLoop {
	return &fakeLoop{t: t, f: f, next: f.clock.Now().Add(f.srv.voice.nextDelay())}
}

func (l *fakeLoop) turn() {
	l.f.srv.voice.tick(context.Background())
	l.next = l.f.clock.Now().Add(l.f.srv.voice.nextDelay())
}

// woken runs a turn if a wake-up is queued, as the loop would at once.
func (l *fakeLoop) woken() {
	select {
	case <-l.f.srv.voice.wakeLoop:
		l.forced++
		l.turn()
	default:
	}
}

// advance moves the clock on by d, a millisecond at a time, running a turn
// whenever the wait is over.
func (l *fakeLoop) advance(d time.Duration) {
	for range d / time.Millisecond {
		l.f.clock.Advance(time.Millisecond)
		if !l.f.clock.Now().Before(l.next) {
			l.natural++
			l.turn()
		}
	}
}

// TestVoiceSecondChannelCannotHideATransmission is the attack the review ran,
// played against the manual clock: ann, in ops with a session in idle ops2,
// tries to run a pass over ops whenever she likes by reporting in ops2, and
// transmits in ops only just after each one.
//
// Before the fix every `started` in ops2 ran a pass and started a new gap, so
// no pass ever fell while she was unmuted: nothing was written in ops. Now her
// reports wake nothing, the passes come when the gaps say, and they find her.
func TestVoiceSecondChannelCannotHideATransmission(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ops, _ := idleSecondChannel(t, f)
	ann := f.identity("ann")
	loop := newFakeLoop(t, f)
	loop.advance(time.Second)
	if got := f.who(t, f.ops); got != "ann" {
		t.Fatalf("before the attack: %q, want ann, muted", got)
	}
	start := f.clock.Now()
	var unmuted time.Duration
	for f.clock.Now().Sub(start) < 6*time.Second {
		// Muted: ask for a pass, by a `started` for the idle room.
		f.reportInOps2(t, "started")
		loop.woken()
		loop.advance(5 * time.Millisecond)
		f.reportInOps2(t, "stopped")
		// A pass has just happened, she believes: 300 ms are safe.
		f.lk.update(ops, ann, func(p *fakeParticipant) { p.muted = false })
		loop.advance(300 * time.Millisecond)
		f.lk.update(ops, ann, func(p *fakeParticipant) { p.muted = true })
		unmuted += 300 * time.Millisecond
		loop.advance(5 * time.Millisecond)
	}
	total := f.clock.Now().Sub(start)
	loop.advance(3 * time.Second)

	if loop.forced != 0 {
		t.Errorf("her reports for the idle room ran %d passes", loop.forced)
	}
	var inOps []string
	for _, e := range f.audit(t) {
		if e.Subject == fmt.Sprintf("channel:%d", f.ops.ID) && strings.HasPrefix(e.Action, "voice_transmit_") {
			inOps = append(inOps, e.Action)
		}
	}
	unreported := 0
	for _, a := range inOps {
		if a == store.AuditVoiceTransmitUnreported {
			unreported++
		}
	}
	t.Logf("unmuted in ops for %v of %v; %d passes by the clock, %d forced; rows in ops: %d, of them unreported: %d", unmuted, total, loop.natural, loop.forced, len(inOps), unreported)
	// She was unmuted for 300 ms of every 310 and the fixture's passes are
	// 500 ms apart: every pass during the attack sees her.
	if unreported == 0 {
		t.Errorf("unmuted in ops for %v of %v and no unreported transmission was recorded there", unmuted, total)
	}
	for _, a := range inOps {
		if a == store.AuditVoiceTransmitStarted {
			t.Errorf("a voice_transmit_started row in ops: she reported nothing there")
		}
	}
	// What she reported in the idle room is on the record too, and pairs up.
	problems, open := checkTransmitPairs(logRowsFromAudit(f.audit(t)))
	for _, p := range append(problems, open...) {
		t.Errorf("pairing: %s", p)
	}
}

// TestVoiceTransmitBoundIsPerPrincipalAcrossChannels: the bound is on the
// principal, not on the principal in a channel. Reports in two channels draw
// on one bucket; otherwise a member of ten channels would have ten times the
// allowance, and ten times the rows.
func TestVoiceTransmitBoundIsPerPrincipalAcrossChannels(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	decodeSession(t, f.session(t, "ann", "ops"))
	decodeSession(t, f.session(t, "ann", "ops2"))
	f.at(0)
	states := []string{"started", "stopped"}
	for i := range voiceReportBurst {
		channel := []string{"ops", "ops2"}[i%2]
		if res := f.report(t, "ann", channel, states[(i/2)%2]); res.status != http.StatusNoContent {
			t.Fatalf("report %d of the burst, in %s: %d %s", i+1, channel, res.status, res.body)
		}
	}
	for _, channel := range []string{"ops", "ops2", "ops"} {
		if res := f.report(t, "ann", channel, "started"); res.status != http.StatusTooManyRequests {
			t.Errorf("after a burst spent across both channels, a report in %s: %d %s, want 429", channel, res.status, res.body)
		}
	}
	if got := len(f.audits(t, store.AuditVoiceReportRateLimited)); got != 1 {
		t.Errorf("rate-limit rows = %d, want 1: the window is the principal's too", got)
	}
}

// TestVoiceTransmitRowSurvivesTheCallerHangingUp: once a report has changed
// the reported state its row is written whether or not the caller is still
// there. A client that hangs up in that instant must not leave a reported
// state with no row, or be able to take a report back by hanging up.
func TestVoiceTransmitRowSurvivesTheCallerHangingUp(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	f.runLive(t, room, []liveStep{{0, "join"}, {0, "pass"}})

	ctx, hangUp := context.WithCancel(context.Background())
	defer hangUp()
	write := f.srv.voice.appendAudit
	f.srv.voice.appendAudit = func(ctx context.Context, actor, action, subject, detail string, at time.Time) (store.AuditEvent, error) {
		hangUp() // the state has changed; the caller goes away before the write
		// The write outlives the caller, but not for ever: the room's order
		// is held while it runs, and a pass over the room waits for it.
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > voiceEvictTimeout {
			t.Errorf("the report's audit write has no deadline within %v (deadline %v, set %v)", voiceEvictTimeout, deadline, ok)
		}
		return write(ctx, actor, action, subject, detail, at)
	}
	f.at(100)
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/ops/voice/transmit", strings.NewReader(`{"state":"started"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.tokens["ann"])
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if ctx.Err() == nil {
		t.Fatal("the caller's context was not cancelled: the test did not hang up")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status %d %s, want 204", rec.Code, rec.Body)
	}
	if got, want := f.transmitRows(t), []string{"ann started@100 reported"}; !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
	}
	// Nor is what follows the write tied to the caller: the check that the
	// room was not rotated meanwhile ran, and did not fail for a hang-up.
	if logged := f.logs.buf.String(); strings.Contains(logged, "could not tell whether a room was retired") || strings.Contains(logged, "could not be audited") {
		t.Errorf("the hang-up was logged as a failure:\n%s", logged)
	}
	f.srv.voice.appendAudit = write
	f.runLive(t, room, []liveStep{{200, "unmute"}, {500, "pass"}, {1000, "pass"}, {1100, "stopped"}, {1100, "mute"}, {1500, "pass"}})
	if got, want := f.transmitRows(t), []string{"ann started@100 reported", "ann stopped@1100 reported"}; !slices.Equal(got, want) {
		t.Errorf("the press that followed: rows %q, want %q (the report had been applied)", got, want)
	}
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitOneReportAtATime: the reports for a room are applied one at
// a time, each after the one before has its row or has been taken back.
//
// The review's sequence: reported `started`; A, a `stopped`, is applied and
// its audit write hangs; B, a `started`, and C, a `stopped`, follow; A's write
// then fails. Were B applied while A hung, it would be recorded as a change
// from a state the log never had: two `started` rows running, one without a
// close. And taking A back by comparing values, as the first version did,
// found the state as C had left it and flipped that. Now B waits for A, finds
// the state `started` again, and changes nothing; C is the one `stopped`.
func TestVoiceTransmitOneReportAtATime(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	f.runLive(t, room, []liveStep{{0, "join"}, {0, "pass"}, {100, "started"}})

	p := f.srv.voice
	write := p.appendAudit
	hold, entered := make(chan struct{}), make(chan struct{})
	first := true
	p.appendAudit = func(ctx context.Context, actor, action, subject, detail string, at time.Time) (store.AuditEvent, error) {
		if first {
			first = false
			close(entered)
			<-hold
			return store.AuditEvent{}, errors.New("database is locked")
		}
		return write(ctx, actor, action, subject, detail, at)
	}
	f.at(200)
	a := make(chan int, 1)
	go func() { a <- f.report(t, "ann", "ops", "stopped").status }() // A: applied, its write hangs
	<-entered
	// A holds the room's one turn for reports for as long as its row is
	// unwritten.
	if r := p.roomNamed(room); len(r.reports) != 1 {
		t.Fatal("the room's turn for reports is free while a report's audit row is being written: a second report could be applied on top of it")
	}
	f.at(300)
	b := make(chan int, 1)
	go func() { b <- f.report(t, "ann", "ops", "started").status }() // B: must wait for A
	close(hold)                                                      // A's write fails now
	if st := <-a; st != http.StatusInternalServerError {
		t.Fatalf("A, whose row could not be written: %d, want 500", st)
	}
	if st := <-b; st != http.StatusNoContent {
		t.Fatalf("B: %d, want 204", st)
	}
	f.at(400)
	f.mustReport(t, "ann", "stopped") // C

	want := []string{"ann started@100 reported", "ann stopped@400 reported"}
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("rows:\n got  %q\n want %q", got, want)
	}
	p.mu.Lock()
	reported := p.rooms[room].transmit.tracks[f.ids["ann"]].reported
	p.mu.Unlock()
	if reported {
		t.Error("the reported state is started after the last answered report, a stopped")
	}
	// So a transmission that follows, unreported, is recorded as that.
	f.runLive(t, room, []liveStep{{500, "unmute"}, {600, "pass"}, {1100, "pass"}, {1200, "mute"}, {1600, "pass"}})
	want = append(want, "ann unreported@600 observed", "ann stopped@1600 observed muted")
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("rows after an unreported transmission:\n got  %q\n want %q", got, want)
	}
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitTimesAreReadUnderTheLock: the invariant that puts reports
// and passes in one order. Everything that feeds the transmit rule reads the
// time it feeds it under the poller's lock, in the same critical section that
// applies it. If a pass read its time and then waited for the lock, a report
// could be applied in between with a later time and an earlier place in the
// order; the audit log would then show a report as having come after a pass
// that in fact read the state it left.
//
// The test's clock looks at who is reading it and, for those functions, at
// whether the lock is held. Nothing else is running, so the lock is held
// exactly when the reader holds it.
//
// It checks the second invariant the same way: the one that puts the room's
// transmit rows into the audit log in the order the rule produced them. Each
// of those functions must hold the room's order (voiceRoomState.order) when it
// changes the rule's state, which is when it reads the clock, and must still
// hold it when the rows are written. If a report released it before its row
// was written, a pass or a rotation could write the row that closes the
// transmission first; if a pass did not take it, it could read a reported
// state whose row is not in the log yet.
func TestVoiceTransmitTimesAreReadUnderTheLock(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	p := f.srv.voice
	var state *voiceRoomState // the ops room's, once it exists
	orderHeld := func() bool {
		if state.order.TryLock() {
			state.order.Unlock()
			return false
		}
		return true
	}
	mustHold := map[string]int{"applyRoom": 0, "reportTransmit": 0, "forget": 0, "forgetEverywhere": 0, "forgetRoom": 0}
	p.now = func() time.Time {
		var pcs [1]uintptr
		if runtime.Callers(2, pcs[:]) == 1 {
			frame, _ := runtime.CallersFrames(pcs[:]).Next()
			reader := frame.Function[strings.LastIndex(frame.Function, ".")+1:]
			if _, ok := mustHold[reader]; ok && strings.Contains(frame.Function, "(*voicePoller)") {
				mustHold[reader]++
				if p.mu.TryLock() {
					p.mu.Unlock()
					t.Errorf("%s read the clock without holding the poller's lock", reader)
				}
				if !orderHeld() {
					t.Errorf("%s changed the room's transmit state without holding the room's order", reader)
				}
			}
		}
		return f.clock.Now()
	}
	write := p.appendAudit
	rowsUnderOrder := 0
	p.appendAudit = func(ctx context.Context, actor, action, subject, detail string, at time.Time) (store.AuditEvent, error) {
		if !orderHeld() {
			t.Errorf("%s (%s) was written without the room's order held", action, detail)
		}
		rowsUnderOrder++
		return write(ctx, actor, action, subject, detail, at)
	}

	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	state = p.roomNamed(room)
	ann, ann2, bob := f.identity("ann"), f.identity("ann2"), f.identity("bob")
	p.entitle = func(_ context.Context, identity string, _ int64) (int64, string, error) {
		id, _ := parseVoiceIdentity(identity)
		return id, "", nil
	}
	f.lk.setRoom(room,
		fakeParticipant{identity: ann, joinedMs: 1_000, published: true},
		fakeParticipant{identity: ann2, joinedMs: 2_000, published: true},
		fakeParticipant{identity: bob, joinedMs: 3_000, published: true})
	f.at(0)
	f.pass(t) // applyRoom
	f.at(100)
	f.mustReport(t, "ann", "started") // reportTransmit
	f.at(500)
	f.pass(t)
	// ann2 is removed by name: forget.
	f.at(600)
	if code := f.doOrdered(t, "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann2"]), f.rootTok); code != 204 {
		t.Fatalf("remove status = %d", code)
	}
	// bob is disabled while his rooms cannot be read: forgetEverywhere.
	p.memberRooms = func(context.Context, int64) ([]store.VoiceRoom, error) { return nil, errors.New("store unavailable") }
	f.at(700)
	if code := f.doOrdered(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", f.ids["bob"]), f.rootTok); code != 204 {
		t.Fatalf("disable status = %d", code)
	}
	// ann's credential is replaced: the room is rotated, forgetRoom.
	f.at(800)
	if code := f.doOrdered(t, "POST", fmt.Sprintf("/v1/credentials/%d/rotate", f.credID(t, "ann")), f.rootTok); code != 201 {
		t.Fatalf("rotate status = %d", code)
	}
	for reader, n := range mustHold {
		if n == 0 {
			t.Errorf("%s never read the clock: the scenario did not exercise it", reader)
		}
	}
	want := []string{
		"ann started@100 reported",
		"ann2 unreported@0 observed", "bob unreported@0 observed",
		"ann2 stopped@600 observed left",
		"bob stopped@700 observed left",
		"ann stopped@800 observed left",
	}
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("rows:\n got  %q\n want %q", got, want)
	}
	if rowsUnderOrder != len(want) {
		t.Errorf("%d transmit rows were checked for the room's order as they were written, want all %d", rowsUnderOrder, len(want))
	}
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitRowIsWrittenBeforeWhatClosesIt: a report's row is in the
// audit log before any row derived from the state the report left. The review
// of #135 found that it need not be: the report was applied under the poller's
// lock and its row written after the lock was released, so a pass, a removal
// or a rotation landing in between wrote `voice_transmit_stopped reason=left`
// first, and the `voice_transmit_started` it closed came after it. Read in the
// order written, the log had a close with nothing open and a start never
// closed.
//
// Here the report's write is held, the thing that closes the transmission is
// started, and the write is let go. Whatever the closer is, it has to wait for
// the report's row.
func TestVoiceTransmitRowIsWrittenBeforeWhatClosesIt(t *testing.T) {
	closers := []struct {
		name  string
		close func(t *testing.T, f *presenceFixture, room store.VoiceRoom)
	}{
		{"the room is rotated", func(t *testing.T, f *presenceFixture, room store.VoiceRoom) {
			f.srv.voice.forgetRoom(context.Background(), room.RoomName)
		}},
		{"a pass finds her gone", func(t *testing.T, f *presenceFixture, room store.VoiceRoom) {
			f.srv.voice.runPass(context.Background())
		}},
		{"she is removed by name", func(t *testing.T, f *presenceFixture, room store.VoiceRoom) {
			f.srv.voice.evict(context.Background(), []store.VoiceRoom{room}, f.ids["ann"], voiceReasonMemberRemoved)
		}},
	}
	for _, tt := range closers {
		for _, fails := range []bool{false, true} {
			name := tt.name
			if fails {
				name += ", and the report's row cannot be written"
			}
			t.Run(name, func(t *testing.T) {
				f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
				ctx := context.Background()
				roomName := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
				room, err := f.srv.store.ChannelVoiceRoom(ctx, f.ops.ID)
				if err != nil {
					t.Fatal(err)
				}
				f.runLive(t, roomName, []liveStep{{0, "join"}, {0, "pass"}})
				p := f.srv.voice
				state := p.roomNamed(roomName)

				write := p.appendAudit
				hold, entered := make(chan struct{}), make(chan struct{})
				first := true
				p.appendAudit = func(ctx context.Context, actor, action, subject, detail string, at time.Time) (store.AuditEvent, error) {
					if first {
						first = false
						close(entered)
						<-hold
						if fails {
							return store.AuditEvent{}, errors.New("database is locked")
						}
					}
					return write(ctx, actor, action, subject, detail, at)
				}
				f.at(100)
				reported := make(chan int, 1)
				go func() { reported <- f.report(t, "ann", "ops", "started").status }()
				<-entered // applied; its row is being written

				// The report holds the room's order for as long as that takes.
				if state.order.TryLock() {
					state.order.Unlock()
					t.Fatal("the room's order is free while a report's row is being written: whatever closes the transmission can be written first")
				}
				// What closes the transmission happens now, as far as the
				// clock goes; it cannot be applied until the report is done.
				f.at(200)
				if tt.name == "the room is rotated" {
					if _, _, err := f.srv.store.RotateVoiceRoom(ctx, room.ID, store.VoiceRotateRevoked); err != nil {
						t.Fatal(err)
					}
				}
				f.lk.setRoom(roomName) // she is gone from LiveKit, for the pass
				closed := make(chan struct{})
				go func() { tt.close(t, f, room); close(closed) }()
				close(hold)
				status := <-reported
				<-closed

				want := []string{"ann started@100 reported", "ann stopped@200 observed left"}
				wantStatus := http.StatusNoContent
				if fails {
					// Taken back whole: nothing was derived from it, so the
					// log has neither the start nor a close for it.
					want, wantStatus = nil, http.StatusInternalServerError
				}
				if status != wantStatus {
					t.Errorf("the report was answered %d, want %d", status, wantStatus)
				}
				if got := f.transmitRows(t); !slices.Equal(got, want) {
					t.Errorf("transmit rows, in the order written:\n got  %q\n want %q", got, want)
				}
				assertTransmitPairs(t, f.audit(t))
			})
		}
	}
}

// TestVoiceTransmitReportThatNeverGetsItsTurn: a report waits its turn behind
// the one whose row is being written. If its caller goes away while it waits,
// it is dropped, not applied; if the room is rotated while it waits, it is
// refused as having no session. Neither writes a row or changes the state.
func TestVoiceTransmitReportThatNeverGetsItsTurn(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	roomName := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	decodeSession(t, f.session(t, "ann2", "ops"))
	room, err := f.srv.store.ChannelVoiceRoom(ctx, f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := f.srv.voice
	write := p.appendAudit
	hold, entered := make(chan struct{}), make(chan struct{})
	first := true
	p.appendAudit = func(ctx context.Context, actor, action, subject, detail string, at time.Time) (store.AuditEvent, error) {
		if first {
			first = false
			close(entered)
			<-hold
		}
		return write(ctx, actor, action, subject, detail, at)
	}
	f.at(100)
	a := make(chan int, 1)
	go func() { a <- f.report(t, "ann", "ops", "started").status }() // holds the turn; its write hangs
	<-entered

	// A caller who has already gone: the turn is taken, so the only thing
	// its report can do is notice.
	gone, hangUp := context.WithCancel(ctx)
	hangUp()
	abandoned := make(chan error, 1)
	go func() { abandoned <- p.reportTransmit(gone, room, f.ids["ann2"], true) }()
	select {
	case err := <-abandoned:
		if !errors.Is(err, errVoiceReportAbandoned) {
			t.Errorf("a report whose caller went away while it waited: %v, want errVoiceReportAbandoned", err)
		}
	case <-time.After(10 * time.Second):
		// Only on failure: it is still waiting for a turn it cannot have.
		close(hold)
		t.Fatal("a report whose caller had gone away went on waiting for its turn")
	}

	state := p.roomNamed(roomName)
	close(hold)
	if st := <-a; st != http.StatusNoContent {
		t.Fatalf("ann's report: %d", st)
	}

	// The room is rotated, and its state dropped, while a report that had
	// already found that state waits for its turn. The waiting is played by
	// hand: the report is given the state it would have been holding.
	f.at(200)
	if _, _, err := f.srv.store.RotateVoiceRoom(ctx, room.ID, store.VoiceRotateRevoked); err != nil {
		t.Fatal(err)
	}
	p.forgetRoom(ctx, roomName)
	p.mu.Lock()
	p.rooms[roomName] = state
	p.mu.Unlock()
	if err := p.reportTransmit(ctx, room, f.ids["ann2"], true); !errors.Is(err, errVoiceReportRoomGone) {
		t.Errorf("a report whose room was rotated while it waited: %v, want errVoiceReportRoomGone", err)
	}
	p.mu.Lock()
	delete(p.rooms, roomName)
	p.mu.Unlock()
	want := []string{"ann started@100 reported", "ann stopped@200 observed left"}
	if got := f.transmitRows(t); !slices.Equal(got, want) {
		t.Errorf("rows:\n got  %q\n want %q (nothing of ann2's)", got, want)
	}
	if p.anyInUse() {
		t.Error("a retired room is in use")
	}
	assertTransmitPairs(t, f.audit(t))

	// Through the endpoint such a report is answered as having no session,
	// as if the rotation had come first. (The state is marked by hand: in
	// the race the endpoint's holder check, made before the rotation, has
	// already passed.)
	f2 := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	name2 := decodeSession(t, f2.session(t, "ann", "ops")).Rooms[0].Room
	f2.srv.voice.mu.Lock()
	f2.srv.voice.rooms[name2].retired = true
	f2.srv.voice.mu.Unlock()
	if res := f2.report(t, "ann", "ops", "started"); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
		t.Errorf("a report for a room rotated before its turn: %d %s, want 409 voice_no_session", res.status, res.body)
	}
	if got := f2.transmitRows(t); len(got) != 0 {
		t.Errorf("it wrote %q", got)
	}
}

// TestVoiceRoomForgottenWhileItsOrderWasAwaited: whoever waits for a room's
// order may find, when its turn comes, that the room was forgotten meanwhile
// (a rotation). Everyone in it has then been recorded as having left, once,
// and nothing more is to be written for it: not by a second forgetting, not by
// a removal by name. The waiting is played by hand, by handing each of them
// the state it would have been holding.
func TestVoiceRoomForgottenWhileItsOrderWasAwaited(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	name := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	f.runLive(t, name, []liveStep{{0, "join"}, {0, "pass"}, {100, "started"}, {100, "unmute"}, {500, "pass"}})
	p := f.srv.voice
	state := p.roomNamed(name)

	f.at(600)
	p.forgetRoom(ctx, name)
	want := []string{"voice_joined " + f.actor("ann"), "voice_transmit_started " + f.actor("ann"), "voice_transmit_stopped " + f.actor("ann"), "voice_left " + f.actor("ann")}
	if got := f.voiceAudit(t); !slices.Equal(got, want) {
		t.Fatalf("after the room was forgotten: %q, want %q", got, want)
	}

	p.mu.Lock()
	p.rooms[name] = state // what each of the three below was waiting with
	p.mu.Unlock()
	f.at(700)
	p.forgetRoom(ctx, name)
	if _, ok := p.forget(ctx, name, f.ids["ann"]); ok {
		t.Error("forget found ann in a room that had been forgotten")
	}
	p.forgetEverywhere(ctx, f.ids["ann"])
	if got := f.voiceAudit(t); !slices.Equal(got, want) {
		t.Errorf("after three more arrived at the forgotten room: %q, want nothing more than %q", got, want)
	}
	assertTransmitPairs(t, f.audit(t))
}

// TestVoiceTransmitConcurrent: reports arrive while passes run. Whatever the
// interleaving, a report is applied whole, in one order with the passes, and
// the rows pair up. Run under -race, this is also the check that the reported
// state and the pass's reading of it are under one lock.
func TestVoiceTransmitConcurrent(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
	decodeSession(t, f.session(t, "ann2", "ops"))
	// A clock that moves on every reading, so that every report and every
	// pass has its own instant and the audit rows have one order in time.
	var tick sync.Mutex
	now := ruleT0
	f.srv.voice.now = func() time.Time {
		tick.Lock()
		defer tick.Unlock()
		now = now.Add(7 * time.Millisecond)
		return now
	}
	ann, ann2 := f.identity("ann"), f.identity("ann2")
	f.lk.setRoom(room,
		fakeParticipant{identity: ann, joinedMs: 1_000, published: true, muted: true},
		fakeParticipant{identity: ann2, joinedMs: 2_000, published: true, muted: true})
	f.pass(t)

	const rounds = 150
	var wg sync.WaitGroup
	for _, who := range []string{"ann", "ann2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			states := []string{"started", "stopped"}
			for i := range rounds {
				rec := f.do(t, http.MethodPost, "/v1/channels/ops/voice/transmit", f.tokens[who], `{"state":"`+states[i%2]+`"}`)
				if rec.Code != http.StatusNoContent && rec.Code != http.StatusTooManyRequests {
					t.Errorf("%s report %d: %d %s", who, i, rec.Code, rec.Body)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range rounds {
			// The microphones change on their own schedule, agreeing with
			// the reports only by chance.
			f.lk.update(room, ann, func(p *fakeParticipant) { p.muted = i%3 == 0 })
			f.lk.update(room, ann2, func(p *fakeParticipant) { p.muted = i%5 < 2 })
			f.srv.voice.runPass(context.Background())
		}
	}()
	wg.Wait()

	// Everyone goes quiet and nothing more is reported: a little over 2 s
	// of passes settles whatever is open.
	f.lk.update(room, ann, func(p *fakeParticipant) { p.muted = true })
	f.lk.update(room, ann2, func(p *fakeParticipant) { p.muted = true })
	// Each pass reads the clock at least twice, so 250 of them are well
	// over 2 s of it.
	for range 250 {
		f.pass(t)
	}
	f.srv.voice.mu.Lock()
	unsettled := f.srv.voice.rooms[room].transmit.unsettled()
	f.srv.voice.mu.Unlock()
	if unsettled {
		t.Error("the room's transmit state is still unsettled")
	}

	events, err := f.srv.store.ListAuditEvents(context.Background(), 0, 100_000)
	if err != nil {
		t.Fatal(err)
	}
	// The rows pair up read in the order they were written: a room's transmit
	// rows go into the log in the order the rule produced them
	// (voiceRoomState.order), whoever wrote them.
	rows := logRowsFromAudit(events)
	if len(rows) < 20 {
		t.Fatalf("only %d transmit rows: the run exercised nothing", len(rows))
	}
	problems, open := checkTransmitPairs(rows)
	for _, p := range append(problems, open...) {
		t.Errorf("pairing, in the order written: %s", p)
	}
	// And in that order their times do not go back, but for the row that
	// opens an unreported transmission, which is timed at an earlier pass.
	var last time.Time
	for _, r := range rows {
		if r.action == transmitActionUnreported {
			continue
		}
		if r.at.Before(last) {
			t.Errorf("%s %s (%s) at %v was written after a row timed %v", r.who, r.action, r.source, r.at.Sub(ruleT0), last.Sub(ruleT0))
		}
		last = r.at
	}
	// Read in order of time instead, they pair up too.
	slices.SortStableFunc(events, func(a, b store.AuditEvent) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	problems, open = checkTransmitPairs(logRowsFromAudit(events))
	for _, p := range append(problems, open...) {
		t.Errorf("pairing, in order of time: %s", p)
	}
}
