package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPrivateCallGoldenFixtures(t *testing.T) {
	calls := []string{
		"private-call-v1-ringing-outgoing", "private-call-v1-ringing-incoming", "private-call-v1-accepted",
		"private-call-v1-active", "private-call-v1-declined", "private-call-v1-cancelled", "private-call-v1-missed",
		"private-call-v1-ended", "private-call-v1-ended-gone", "private-call-v1-ended-no-reason",
		"private-call-v1-failed-join-timeout", "private-call-v1-failed-restart", "private-call-v1-unavailable",
	}
	tests := []struct {
		file string
		new  func() dmValidator
	}{
		{"invite-private-call-request-v1.json", func() dmValidator { return new(InvitePrivateCallRequestV1) }},
		{"private-call-action-request-v1.json", func() dmValidator { return new(PrivateCallActionRequestV1) }},
		{"private-call-response-v1.json", func() dmValidator { return new(PrivateCallResponseV1) }},
		{"private-call-response-v1-replayed.json", func() dmValidator { return new(PrivateCallResponseV1) }},
		{"private-call-response-v1-unavailable.json", func() dmValidator { return new(PrivateCallResponseV1) }},
		{"private-call-response-v1-glare.json", func() dmValidator { return new(PrivateCallResponseV1) }},
		{"private-call-response-v1-accepted.json", func() dmValidator { return new(PrivateCallResponseV1) }},
		{"private-call-session-v1.json", func() dmValidator { return new(PrivateCallSessionV1) }},
		{"private-call-ringing-v1.json", func() dmValidator { return new(PrivateCallRingingV1) }},
		{"private-call-stop-ringing-v1.json", func() dmValidator { return new(PrivateCallStopRingingV1) }},
		{"private-call-state-v1.json", func() dmValidator { return new(PrivateCallStateV1) }},
		{"private-call-state-v1-terminal.json", func() dmValidator { return new(PrivateCallStateV1) }},
		{"private-call-snapshot-v1.json", func() dmValidator { return new(PrivateCallSnapshotV1) }},
		{"private-call-snapshot-v1-idle.json", func() dmValidator { return new(PrivateCallSnapshotV1) }},
		{"list-private-calls-response-v1.json", func() dmValidator { return new(ListPrivateCallsResponseV1) }},
		{"list-private-calls-response-v1-empty.json", func() dmValidator { return new(ListPrivateCallsResponseV1) }},
	}
	for _, c := range calls {
		tests = append(tests, struct {
			file string
			new  func() dmValidator
		}{c + ".json", func() dmValidator { return new(PrivateCallV1) }})
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) { assertValidGoldenFixture(t, tt.file, tt.new) })
	}
	for _, f := range []string{"error-call-not-found.json", "error-call-in-progress.json", "error-call-action-conflict.json", "error-call-wrong-device.json"} {
		t.Run(f, func(t *testing.T) { assertGoldenFixture(t, f, func() any { return new(Error) }) })
	}
}

// TestPrivateCallTransitionsGolden pins the table's wire form: a change to a
// row is a change to the contract and shows in this fixture's diff.
func TestPrivateCallTransitionsGolden(t *testing.T) {
	doc := struct {
		Transitions []PrivateCallTransition `json:"transitions"`
	}{PrivateCallTransitions}
	got, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "private-call-transitions-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("transition table differs from testdata/private-call-transitions-v1.json\ngot:\n%s", got)
	}
}

var allPrivateCallStates = []PrivateCallState{
	PrivateCallStateUnavailable, PrivateCallStateRinging, PrivateCallStateAccepted, PrivateCallStateActive,
	PrivateCallStateDeclined, PrivateCallStateCancelled, PrivateCallStateMissed, PrivateCallStateEnded,
	PrivateCallStateFailed,
}

var allPrivateCallEvents = []string{
	PrivateCallEventInvite, PrivateCallEventInviteUnavailable, PrivateCallEventAccept, PrivateCallEventDecline,
	PrivateCallEventCancel, PrivateCallEventRingExpired, PrivateCallEventJoined, PrivateCallEventJoinExpired,
	PrivateCallEventHangup, PrivateCallEventGraceExpired, PrivateCallEventRevoked, PrivateCallEventRestart,
}

func TestPrivateCallTransitionTable(t *testing.T) {
	// Every (state, event) pair: allowed exactly when the design's table says.
	allowed := map[string]map[string]PrivateCallState{
		PrivateCallFromNone: {
			PrivateCallEventInvite:            PrivateCallStateRinging,
			PrivateCallEventInviteUnavailable: PrivateCallStateUnavailable,
		},
		string(PrivateCallStateRinging): {
			PrivateCallEventAccept:      PrivateCallStateAccepted,
			PrivateCallEventDecline:     PrivateCallStateDeclined,
			PrivateCallEventCancel:      PrivateCallStateCancelled,
			PrivateCallEventRingExpired: PrivateCallStateMissed,
			PrivateCallEventRevoked:     PrivateCallStateCancelled,
			PrivateCallEventRestart:     PrivateCallStateFailed,
		},
		string(PrivateCallStateAccepted): {
			PrivateCallEventJoined:      PrivateCallStateActive,
			PrivateCallEventJoinExpired: PrivateCallStateFailed,
			PrivateCallEventHangup:      PrivateCallStateEnded,
			PrivateCallEventRevoked:     PrivateCallStateEnded,
			PrivateCallEventRestart:     PrivateCallStateFailed,
		},
		string(PrivateCallStateActive): {
			PrivateCallEventHangup:       PrivateCallStateEnded,
			PrivateCallEventGraceExpired: PrivateCallStateEnded,
			PrivateCallEventRevoked:      PrivateCallStateEnded,
			PrivateCallEventRestart:      PrivateCallStateFailed,
		},
	}
	froms := []string{PrivateCallFromNone}
	for _, s := range allPrivateCallStates {
		froms = append(froms, string(s))
	}
	for _, from := range froms {
		for _, ev := range allPrivateCallEvents {
			t.Run(from+"/"+ev, func(t *testing.T) {
				row, ok := NextPrivateCall(from, ev)
				want, wantOK := allowed[from][ev]
				if ok != wantOK || (ok && row.To != want) {
					t.Fatalf("NextPrivateCall(%s, %s) = %v, %v; want %v, %v", from, ev, row.To, ok, want, wantOK)
				}
			})
		}
	}
	rows := 0
	for _, m := range allowed {
		rows += len(m)
	}
	if rows != len(PrivateCallTransitions) {
		t.Errorf("table has %d rows, the contract %d", len(PrivateCallTransitions), rows)
	}
}

func TestPrivateCallTerminalStatesAreFinal(t *testing.T) {
	for _, s := range allPrivateCallStates {
		wantTerminal := s != PrivateCallStateRinging && s != PrivateCallStateAccepted && s != PrivateCallStateActive
		if s.Terminal() != wantTerminal || !s.Valid() {
			t.Errorf("%s: Terminal() = %v, Valid() = %v", s, s.Terminal(), s.Valid())
		}
	}
	if PrivateCallState(PrivateCallFromNone).Valid() {
		t.Error("none is a table marker, not a state")
	}
	for _, row := range PrivateCallTransitions {
		if PrivateCallState(row.From).Terminal() {
			t.Errorf("row leaves terminal state %s: %+v", row.From, row)
		}
		if !row.To.Valid() {
			t.Errorf("row to a non-state: %+v", row)
		}
		// A row's reason must be one its target state accepts.
		doc := validCall(row.To)
		doc.Reason = row.Reason
		if err := doc.validateReason(); err != nil {
			t.Errorf("row %+v: %v", row, err)
		}
	}
	// No step, of any shape, takes a terminal attempt anywhere.
	for _, from := range allPrivateCallStates {
		if !from.Terminal() {
			continue
		}
		for _, to := range allPrivateCallStates {
			prev, next := validCall(from), validCall(to)
			next.Revision = prev.Revision + 1
			if err := ValidatePrivateCallStep(prev, next); err == nil {
				t.Errorf("%s -> %s was accepted", from, to)
			}
		}
	}
}

// validCall is a well-formed document in the state, the caller's view.
func validCall(s PrivateCallState) PrivateCallV1 {
	created := NewTimestamp(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	end := NewTimestamp(time.Date(2026, 11, 19, 10, 5, 0, 0, time.UTC))
	c := PrivateCallV1{
		Schema: PrivateCallSchemaV1, ID: "c4Kq9ZxT2mWb7LpR1vNs3A", ConversationID: 12,
		CallerID: 3, CalleeID: 5, Role: PrivateCallRoleCaller, State: s, Revision: 5, CreatedAt: created,
	}
	switch s {
	case PrivateCallStateRinging:
		c.Revision = 1
		d := NewTimestamp(created.Time().Add(PrivateCallRingSeconds * time.Second))
		c.RingDeadline = &d
	case PrivateCallStateAccepted:
		d := NewTimestamp(created.Time().Add(42 * time.Second))
		c.JoinDeadline = &d
	case PrivateCallStateUnavailable:
		c.Revision = 1
		c.EndedAt = &created
	case PrivateCallStateFailed:
		c.Reason = PrivateCallReasonServerRestart
		c.EndedAt = &end
	case PrivateCallStateActive:
	default:
		c.EndedAt = &end
	}
	return c
}

func TestPrivateCallSequencesGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "private-call-sequences-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Sequences []struct {
			Name  string          `json:"name"`
			Steps []PrivateCallV1 `json:"steps"`
		} `json:"sequences"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Sequences) == 0 {
		t.Fatal("no sequences")
	}
	for _, seq := range doc.Sequences {
		t.Run(seq.Name, func(t *testing.T) {
			for i := 1; i < len(seq.Steps); i++ {
				if err := ValidatePrivateCallStep(seq.Steps[i-1], seq.Steps[i]); err != nil {
					t.Errorf("step %d: %v", i, err)
				}
			}
			last := seq.Steps[len(seq.Steps)-1]
			if !last.State.Terminal() {
				t.Errorf("ends in %s, not a terminal state", last.State)
			}
			// The final document can never be followed by a live one.
			live := last
			live.State, live.Reason, live.EndedAt = PrivateCallStateActive, "", nil
			live.Revision++
			if err := ValidatePrivateCallStep(last, live); err == nil {
				t.Error("a terminal attempt was reinterpreted as active")
			}
		})
	}
}

func TestPrivateCallStepRefusals(t *testing.T) {
	ringing := validCall(PrivateCallStateRinging)
	accepted := validCall(PrivateCallStateAccepted)
	accepted.Revision = 2
	tests := []struct {
		name   string
		mutate func(next *PrivateCallV1)
	}{
		{"revision skipped", func(n *PrivateCallV1) { n.Revision = 3 }},
		{"revision repeated", func(n *PrivateCallV1) { n.Revision = 1 }},
		{"another attempt", func(n *PrivateCallV1) { n.ID = "Xe3Lm0Wq8NtRz5YbHc1uVg" }},
		{"another pair", func(n *PrivateCallV1) { n.CalleeID = 9 }},
		{"another reader", func(n *PrivateCallV1) { n.Role = PrivateCallRoleCallee; n.SelectedHere = false }},
		{"caller credential gains selection on acceptance", func(n *PrivateCallV1) { n.SelectedHere = true }},
	}
	ringing.SelectedHere = true
	if err := ValidatePrivateCallStep(ringing, accepted); err == nil {
		t.Error("caller credential loses selection on acceptance")
	}
	ringing.SelectedHere = false
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := accepted
			tt.mutate(&next)
			if err := ValidatePrivateCallStep(ringing, next); err == nil {
				t.Error("accepted")
			}
		})
	}
	// ringing -> active skips acceptance; accepted -> missed is not a transition.
	active := validCall(PrivateCallStateActive)
	active.Revision = 2
	if err := ValidatePrivateCallStep(ringing, active); err == nil {
		t.Error("ringing -> active accepted")
	}
	// A reason the table does not give for the pair.
	ended := validCall(PrivateCallStateEnded)
	ended.Revision = 3
	ended.Reason = PrivateCallReasonGone
	if err := ValidatePrivateCallStep(accepted, ended); err == nil {
		t.Error("accepted -> ended (gone) accepted; gone is from active")
	}
	active.Revision = 3
	active.Role, accepted.Role = PrivateCallRoleCallee, PrivateCallRoleCallee
	active.SelectedHere = true
	if err := ValidatePrivateCallStep(accepted, active); err == nil {
		t.Error("callee credential gains selection after acceptance")
	}
}

func TestPrivateCallValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *PrivateCallV1)
		want   string
	}{
		{"wrong schema", func(c *PrivateCallV1) { c.Schema = "conch.private_call.v2" }, "schema must be"},
		{"short id", func(c *PrivateCallV1) { c.ID = "abc" }, "id must be"},
		{"id with a slash", func(c *PrivateCallV1) { c.ID = "c4Kq9ZxT2mWb7LpR/vNs3A" }, "id must be"},
		{"no conversation", func(c *PrivateCallV1) { c.ConversationID = 0 }, "conversation_id"},
		{"calling oneself", func(c *PrivateCallV1) { c.CalleeID = c.CallerID }, "distinct"},
		{"bad role", func(c *PrivateCallV1) { c.Role = "viewer" }, "role"},
		{"bad state", func(c *PrivateCallV1) { c.State = "none" }, "state"},
		{"reason on ringing", func(c *PrivateCallV1) { c.Reason = PrivateCallReasonHangup }, "carries no reason"},
		{"revision zero", func(c *PrivateCallV1) { c.Revision = 0 }, "revision"},
		{"ringing at revision 2", func(c *PrivateCallV1) { c.Revision = 2 }, "revision must be 1"},
		{"no created_at", func(c *PrivateCallV1) { c.CreatedAt = Timestamp{} }, "created_at"},
		{"no ring deadline", func(c *PrivateCallV1) { c.RingDeadline = nil }, "requires ring_deadline"},
		{"wrong ring deadline", func(c *PrivateCallV1) {
			d := NewTimestamp(c.CreatedAt.Time().Add(41 * time.Second))
			c.RingDeadline = &d
		}, "ring_deadline must be"},
		{"ended_at while ringing", func(c *PrivateCallV1) { c.EndedAt = &c.CreatedAt }, "must not carry ended_at"},
		{"callee selected while ringing", func(c *PrivateCallV1) { c.Role = PrivateCallRoleCallee; c.SelectedHere = true }, "no selected callee"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validCall(PrivateCallStateRinging)
			tt.mutate(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}

	states := []struct {
		name   string
		state  PrivateCallState
		mutate func(c *PrivateCallV1)
		want   string
	}{
		{"unavailable carries a reason", PrivateCallStateUnavailable, func(c *PrivateCallV1) { c.Reason = PrivateCallReasonGone }, "carries no reason"},
		{"unavailable to the callee", PrivateCallStateUnavailable, func(c *PrivateCallV1) { c.Role = PrivateCallRoleCallee }, "caller's alone"},
		{"unavailable at revision 2", PrivateCallStateUnavailable, func(c *PrivateCallV1) { c.Revision = 2 }, "revision must be 1"},
		{"terminal without ended_at", PrivateCallStateMissed, func(c *PrivateCallV1) { c.EndedAt = nil }, "requires ended_at"},
		{"terminal selected", PrivateCallStateDeclined, func(c *PrivateCallV1) { c.SelectedHere = true }, "no selected endpoint"},
		{"ended before created", PrivateCallStateEnded, func(c *PrivateCallV1) {
			d := NewTimestamp(c.CreatedAt.Time().Add(-time.Second))
			c.EndedAt = &d
		}, "before created_at"},
		{"failed without a reason", PrivateCallStateFailed, func(c *PrivateCallV1) { c.Reason = "" }, "failed"},
		{"failed for an access change", PrivateCallStateFailed, func(c *PrivateCallV1) { c.Reason = "revoked" }, "failed"},
		{"ended for an access change", PrivateCallStateEnded, func(c *PrivateCallV1) { c.Reason = "revoked" }, "ended"},
		{"active at revision 2", PrivateCallStateActive, func(c *PrivateCallV1) { c.Revision = 2 }, "at least 3"},
		{"accepted without join deadline", PrivateCallStateAccepted, func(c *PrivateCallV1) { c.JoinDeadline = nil }, "requires join_deadline"},
		{"join deadline too late", PrivateCallStateAccepted, func(c *PrivateCallV1) {
			d := NewTimestamp(c.CreatedAt.Time().Add(71 * time.Second))
			c.JoinDeadline = &d
		}, "join_deadline"},
		{"active with a ring deadline", PrivateCallStateActive, func(c *PrivateCallV1) { c.RingDeadline = &c.CreatedAt }, "must not carry ring_deadline"},
	}
	for _, tt := range states {
		t.Run(tt.name, func(t *testing.T) {
			c := validCall(tt.state)
			tt.mutate(&c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
	for _, s := range allPrivateCallStates {
		if err := validCall(s).Validate(); err != nil {
			t.Errorf("valid %s call: %v", s, err)
		}
	}
	if err := validCall(PrivateCallStateEnded).Validate(); err != nil {
		t.Errorf("an ended call from an access change has no reason: %v", err)
	}
}

func TestPrivateCallRequestsAndBodies(t *testing.T) {
	if err := (InvitePrivateCallRequestV1{}).Validate(); err == nil {
		t.Error("invite without an action id")
	}
	if err := (InvitePrivateCallRequestV1{ActionID: strings.Repeat("a", MaxPrivateCallActionIDBytes+1)}).Validate(); err == nil {
		t.Error("invite with an over-long action id")
	}
	if err := (InvitePrivateCallRequestV1{ActionID: "a b"}).Validate(); err == nil {
		t.Error("invite with a space in the action id")
	}
	if err := (PrivateCallActionRequestV1{ActionID: "a"}).Validate(); err == nil {
		t.Error("action without a revision")
	}
	if err := (PrivateCallActionRequestV1{ActionID: "a", Revision: 1}).Validate(); err != nil {
		t.Error(err)
	}
	// An invite or action body has no field for a device, room, camera or share: the
	// server's strict decoding refuses one (400 invalid_request).
	for _, tt := range []struct {
		name string
		body string
		new  func() dmValidator
	}{
		{"invite", `{"action_id":"a"}`, func() dmValidator { return new(InvitePrivateCallRequestV1) }},
		{"action", `{"action_id":"a","revision":1}`, func() dmValidator { return new(PrivateCallActionRequestV1) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			decode := func(body string, into dmValidator) error {
				dec := json.NewDecoder(strings.NewReader(body))
				dec.DisallowUnknownFields()
				return dec.Decode(into)
			}
			valid := tt.new()
			if err := decode(tt.body, valid); err != nil {
				t.Fatal(err)
			}
			if err := valid.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"room", "token", "device", "credential_id", "camera", "screen_share", "video"} {
				body := strings.TrimSuffix(tt.body, "}") + `,"` + field + `":1}`
				want := `json: unknown field "` + field + `"`
				if err := decode(body, tt.new()); err == nil || err.Error() != want {
					t.Errorf("decode with %q = %v, want %s", field, err, want)
				}
			}
		})
	}
}

func TestPrivateCallAcceptRaceFixtures(t *testing.T) {
	// The exact retry receives HTTP 200 and keeps the winning selection.
	var winner PrivateCallResponseV1
	assertValidGoldenFixture(t, "private-call-response-v1-accept-replayed.json", func() dmValidator { return &winner })
	if !winner.Replayed || !winner.Call.SelectedHere || winner.Call.Role != PrivateCallRoleCallee ||
		winner.Call.State != PrivateCallStateAccepted || winner.Call.Revision != 2 {
		t.Fatalf("winning retry = %+v", winner)
	}

	// A competing credential receives HTTP 409 for the same revision, even
	// if it submitted the same action id. Only its own selection differs.
	var loser PrivateCallConflictV1
	assertValidGoldenFixture(t, "private-call-conflict-v1.json", func() dmValidator { return &loser })
	want := winner.Call
	want.SelectedHere = false
	if !reflect.DeepEqual(loser.Call, want) {
		t.Errorf("competing accept = %+v, want %+v", loser.Call, want)
	}
}

func TestPrivateCallUnavailableStateFrame(t *testing.T) {
	raw, err := os.ReadFile("testdata/invalid-private-call-state-v1-unavailable.json")
	if err != nil {
		t.Fatal(err)
	}
	var frame PrivateCallStateV1
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&frame); err != nil {
		t.Fatal(err)
	}
	if err := frame.Call.Validate(); err != nil {
		t.Fatalf("the embedded caller view should be valid in an invite response: %v", err)
	}
	if err := frame.Validate(); err == nil || !strings.Contains(err.Error(), "unavailable call cannot appear in a state frame") {
		t.Errorf("unavailable state frame: %v", err)
	}
}

func TestPrivateCallResponses(t *testing.T) {
	incoming := validCall(PrivateCallStateRinging)
	incoming.Role = PrivateCallRoleCallee
	if err := (PrivateCallResponseV1{Call: incoming, Glare: true}).Validate(); err != nil {
		t.Errorf("glare with an incoming ring: %v", err)
	}
	if err := (PrivateCallResponseV1{Call: validCall(PrivateCallStateRinging), Glare: true}).Validate(); err == nil {
		t.Error("glare with an outgoing call")
	}
	acc := incoming
	acc.State, acc.Revision, acc.RingDeadline = PrivateCallStateAccepted, 2, nil
	d := NewTimestamp(acc.CreatedAt.Time().Add(40 * time.Second))
	acc.JoinDeadline = &d
	if err := (PrivateCallResponseV1{Call: acc, Glare: true}).Validate(); err == nil {
		t.Error("glare with an accepted call")
	}

	conflict := PrivateCallConflictV1{Code: ErrorCodeCallStateChanged, Message: "x", Call: acc}
	if err := conflict.Validate(); err != nil {
		t.Error(err)
	}
	conflict.Call = validCall(PrivateCallStateUnavailable)
	if err := conflict.Validate(); err == nil {
		t.Error("a call lookup exposes an unavailable attempt")
	}
	conflict.Call = acc
	conflict.Code = "forbidden"
	if err := conflict.Validate(); err == nil {
		t.Error("conflict with another code")
	}

	snapTerminal := PrivateCallSnapshotV1{
		Schema: PrivateCallSnapshotSchemaV1, TakenAt: incoming.CreatedAt,
	}
	ended := validCall(PrivateCallStateEnded)
	snapTerminal.Call = &ended
	if err := snapTerminal.Validate(); err == nil {
		t.Error("a snapshot lists a terminal call as current")
	}

	stop := PrivateCallStopRingingV1{Schema: PrivateCallStopRingingSchemaV1, CallID: incoming.ID, Revision: 2, State: PrivateCallStateActive}
	if err := stop.Validate(); err == nil {
		t.Error("stop-ringing to a state a ringing call cannot reach")
	}
	ring := PrivateCallRingingV1{Schema: PrivateCallRingingSchemaV1, Call: validCall(PrivateCallStateRinging)}
	if err := ring.Validate(); err == nil {
		t.Error("a ringing frame for the caller's own outgoing call")
	}
	if err := (PrivateCallSessionV1{Schema: PrivateCallSessionSchemaV1, CallID: incoming.ID, Revision: 1,
		LivekitURL: "ws://x", Identity: "p3", Room: "r", Token: "t", ExpiresAt: incoming.CreatedAt}).Validate(); err == nil {
		t.Error("a grant at revision 1, before acceptance")
	}
}

func TestPrivateCallHistoryRefusals(t *testing.T) {
	missedCallee := validCall(PrivateCallStateMissed)
	missedCallee.Role = PrivateCallRoleCallee
	missedCaller := validCall(PrivateCallStateMissed)
	older := validCall(PrivateCallStateDeclined)
	older.CreatedAt = NewTimestamp(older.CreatedAt.Time().Add(-time.Hour))
	older.EndedAt = &older.CreatedAt
	newer := validCall(PrivateCallStateDeclined)
	other := validCall(PrivateCallStateDeclined)
	other.ConversationID = 13
	tests := []struct {
		name  string
		calls []PrivateCallV1
		ok    bool
	}{
		{"missed to the callee", []PrivateCallV1{missedCallee}, true},
		{"missed to the caller", []PrivateCallV1{missedCaller}, false},
		{"unavailable", []PrivateCallV1{validCall(PrivateCallStateUnavailable)}, false},
		{"still live", []PrivateCallV1{validCall(PrivateCallStateActive)}, false},
		{"newest first", []PrivateCallV1{newer, older}, true},
		{"oldest first", []PrivateCallV1{older, newer}, false},
		{"two conversations", []PrivateCallV1{newer, other}, false},
		{"null", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ListPrivateCallsResponseV1{Calls: tt.calls}.Validate()
			if (err == nil) != tt.ok {
				t.Errorf("err = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// TestPrivateCallShapesNameNoRoom pins the privacy rule: no room name, token
// or URL in any call shape but the grant, no device or credential id, and no
// camera, video or share field anywhere (ADR-017 Decision 6).
func TestPrivateCallShapesNameNoRoom(t *testing.T) {
	shapes := []any{
		PrivateCallV1{}, InvitePrivateCallRequestV1{}, PrivateCallActionRequestV1{}, PrivateCallResponseV1{},
		PrivateCallConflictV1{}, PrivateCallRingingV1{}, PrivateCallStopRingingV1{}, PrivateCallStateV1{},
		PrivateCallSnapshotV1{}, ListPrivateCallsResponseV1{}, PrivateCallSessionV1{},
	}
	grantOnly := []string{"room", "token", "url", "identity"}
	never := []string{"camera", "video", "screen", "share", "credential", "device", "endpoint", "companion", "audience", "channel"}
	for _, s := range shapes {
		typ := reflect.TypeOf(s)
		for _, name := range jsonNames(typ) {
			for _, bad := range never {
				if strings.Contains(name, bad) {
					t.Errorf("%s has a %q field; calls carry none", typ.Name(), name)
				}
			}
			if typ == reflect.TypeOf(PrivateCallSessionV1{}) {
				continue
			}
			for _, bad := range grantOnly {
				if strings.Contains(name, bad) {
					t.Errorf("%s has a %q field; only the media-join grant carries one", typ.Name(), name)
				}
			}
		}
	}
}

// jsonNames is every json field name reachable from typ.
func jsonNames(typ reflect.Type) []string {
	for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct || typ == reflect.TypeOf(Timestamp{}) {
		return nil
	}
	var out []string
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" {
			out = append(out, name)
		}
		out = append(out, jsonNames(f.Type)...)
	}
	return out
}

func TestPrivateCallTimings(t *testing.T) {
	if PrivateCallRingSeconds != 40 || PrivateCallJoinSeconds != 30 || PrivateCallReconnectGraceSeconds != 20 {
		t.Errorf("v1 timings are 40/30/20, got %d/%d/%d", PrivateCallRingSeconds, PrivateCallJoinSeconds, PrivateCallReconnectGraceSeconds)
	}
}
