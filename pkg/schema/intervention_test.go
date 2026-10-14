package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestInterventionGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "message, requested", file: "intervention-v1.json", new: func() any { return new(InterventionV1) }},
		{name: "cancel, failed with a reason", file: "intervention-v1-answered.json", new: func() any { return new(InterventionV1) }},
		{name: "create request", file: "create-intervention-request-v1.json", new: func() any { return new(CreateInterventionRequestV1) }},
		{name: "ack request", file: "ack-intervention-request-v1.json", new: func() any { return new(AckInterventionRequestV1) }},
		{name: "list response", file: "list-interventions-response-v1.json", new: func() any { return new(ListInterventionsResponseV1) }},
		{name: "list response, empty", file: "list-interventions-response-v1-empty.json", new: func() any { return new(ListInterventionsResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertP3GoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestInterventionSchemaName(t *testing.T) {
	if InterventionSchemaV1 != "conch.intervention.v1" {
		t.Fatalf("InterventionSchemaV1 = %q", InterventionSchemaV1)
	}
}

func TestInterventionErrorCodes(t *testing.T) {
	assertErrorCodes(t, map[string]string{
		ErrorCodeInterventionNotFound:    "intervention_not_found",
		ErrorCodeInterventionUnsupported: "intervention_unsupported",
		ErrorCodeTooManyInterventions:    "too_many_interventions",
		ErrorCodeHumanRequired:           "human_required",
	})
}

func TestInterventionKindValid(t *testing.T) {
	for _, k := range []InterventionKind{InterventionKindMessage, InterventionKindCancel, InterventionKindPause, InterventionKindResume} {
		if !k.Valid() {
			t.Errorf("intervention kind %q should be valid", k)
		}
	}
	for _, k := range []InterventionKind{"", "stop", "kill", "restart", "unsupported", "Message", "CANCEL", " pause", "*"} {
		if k.Valid() {
			t.Errorf("intervention kind %q should be invalid", k)
		}
	}
}

func TestInterventionStateValid(t *testing.T) {
	for _, s := range []InterventionState{InterventionStateRequested, InterventionStateAcknowledged, InterventionStateApplied, InterventionStateFailed} {
		if !s.Valid() {
			t.Errorf("intervention state %q should be valid", s)
		}
	}
	// "unsupported" is an error code, not a stored state: an unsupported
	// request is refused, never recorded.
	for _, s := range []InterventionState{"", "unsupported", "pending", "done", "cancelled", "Requested", "APPLIED", " failed", "*"} {
		if s.Valid() {
			t.Errorf("intervention state %q should be invalid", s)
		}
	}
}

var (
	interventionRequestedAt = NewTimestamp(time.Date(2026, 10, 13, 9, 4, 0, 0, time.UTC))
	interventionAnsweredAt  = NewTimestamp(time.Date(2026, 10, 13, 9, 4, 31, 200_000_000, time.UTC))
)

// fixtureIntervention is intervention-v1.json: a message to run 41, not yet
// answered.
func fixtureIntervention() InterventionV1 {
	return InterventionV1{
		Schema:      InterventionSchemaV1,
		ID:          7,
		RunID:       41,
		Kind:        InterventionKindMessage,
		Body:        "Skip the flaky test and report what still fails",
		State:       InterventionStateRequested,
		RequestedBy: 1,
		RequestedAt: interventionRequestedAt,
	}
}

// fixtureAnsweredIntervention is intervention-v1-answered.json: a cancel the
// adapter could not apply.
func fixtureAnsweredIntervention() InterventionV1 {
	answered := interventionAnsweredAt
	return InterventionV1{
		Schema:      InterventionSchemaV1,
		ID:          8,
		RunID:       41,
		Kind:        InterventionKindCancel,
		State:       InterventionStateFailed,
		RequestedBy: 1,
		RequestedAt: NewTimestamp(time.Date(2026, 10, 13, 9, 4, 30, 0, time.UTC)),
		AnsweredAt:  &answered,
		Reason:      "run had already completed",
	}
}

func fixtureCreateInterventionRequest() CreateInterventionRequestV1 {
	return CreateInterventionRequestV1{Kind: InterventionKindMessage, Body: "Skip the flaky test and report what still fails"}
}

func fixtureAckInterventionRequest() AckInterventionRequestV1 {
	return AckInterventionRequestV1{State: InterventionStateFailed, Reason: "run had already completed"}
}

func TestInterventionV1Validate(t *testing.T) {
	later := NewTimestamp(interventionRequestedAt.Time().Add(time.Second))
	earlier := NewTimestamp(interventionRequestedAt.Time().Add(-time.Second))
	tests := []struct {
		name    string
		mutate  func(*InterventionV1)
		wantErr string
	}{
		{name: "valid, message requested", mutate: func(*InterventionV1) {}},
		{name: "valid, cancel failed with reason", mutate: func(i *InterventionV1) { *i = fixtureAnsweredIntervention() }},
		{name: "valid, cancel requested without body", mutate: func(i *InterventionV1) { i.Kind, i.Body = InterventionKindCancel, "" }},
		{name: "valid, pause requested", mutate: func(i *InterventionV1) { i.Kind, i.Body = InterventionKindPause, "" }},
		{name: "valid, resume requested", mutate: func(i *InterventionV1) { i.Kind, i.Body = InterventionKindResume, "" }},
		{name: "valid, acknowledged", mutate: func(i *InterventionV1) { i.State, i.AnsweredAt = InterventionStateAcknowledged, &later }},
		{name: "valid, applied without reason", mutate: func(i *InterventionV1) { i.State, i.AnsweredAt = InterventionStateApplied, &later }},
		{name: "valid, applied with reason", mutate: func(i *InterventionV1) {
			i.State, i.AnsweredAt, i.Reason = InterventionStateApplied, &later, "delivered on the next turn"
		}},
		{name: "valid, answered_at equals requested_at", mutate: func(i *InterventionV1) {
			at := interventionRequestedAt
			i.State, i.AnsweredAt = InterventionStateApplied, &at
		}},
		{name: "valid body at the limit", mutate: func(i *InterventionV1) { i.Body = strings.Repeat("b", MaxInterventionBodyLength) }},
		{name: "valid body at the limit in multibyte characters", mutate: func(i *InterventionV1) { i.Body = strings.Repeat("é", MaxInterventionBodyLength) }},
		{name: "valid body with newlines", mutate: func(i *InterventionV1) { i.Body = "first\n\nsecond" }},
		{name: "valid body with tabs", mutate: func(i *InterventionV1) { i.Body = "- step one\n\t- step two" }},
		{name: "valid reason at the limit", mutate: func(i *InterventionV1) {
			*i = fixtureAnsweredIntervention()
			i.Reason = strings.Repeat("r", MaxInterventionReasonLength)
		}},

		{name: "empty schema", mutate: func(i *InterventionV1) { i.Schema = "" }, wantErr: `intervention schema must be "conch.intervention.v1", got ""`},
		{name: "run schema", mutate: func(i *InterventionV1) { i.Schema = RunSchemaV1 }, wantErr: `got "conch.run.v1"`},
		{name: "future schema version", mutate: func(i *InterventionV1) { i.Schema = "conch.intervention.v2" }, wantErr: `got "conch.intervention.v2"`},
		{name: "zero id", mutate: func(i *InterventionV1) { i.ID = 0 }, wantErr: "intervention id must be positive, got 0"},
		{name: "zero run_id", mutate: func(i *InterventionV1) { i.RunID = 0 }, wantErr: "intervention run_id must be positive, got 0"},
		{name: "empty kind", mutate: func(i *InterventionV1) { i.Kind = "" }, wantErr: `intervention kind "" is not one of message, cancel, pause, resume`},
		{name: "unknown kind", mutate: func(i *InterventionV1) { i.Kind = "stop" }, wantErr: `intervention kind "stop" is not one of message, cancel, pause, resume`},
		{name: "activity kind is not an intervention kind", mutate: func(i *InterventionV1) { i.Kind = "run_ended" }, wantErr: `intervention kind "run_ended" is not one of`},
		{name: "message without body", mutate: func(i *InterventionV1) { i.Body = "" }, wantErr: "message intervention body is required"},
		{name: "message with blank body", mutate: func(i *InterventionV1) { i.Body = " \n " }, wantErr: "message intervention body is required"},
		// Only a message carries text (#287); the other kinds take none.
		{name: "cancel with body", mutate: func(i *InterventionV1) { i.Kind = InterventionKindCancel }, wantErr: "cancel intervention must not carry body"},
		{name: "pause with body", mutate: func(i *InterventionV1) { i.Kind = InterventionKindPause }, wantErr: "pause intervention must not carry body"},
		{name: "resume with body", mutate: func(i *InterventionV1) { i.Kind, i.Body = InterventionKindResume, "carry on" }, wantErr: "resume intervention must not carry body"},
		{name: "body one over the limit", mutate: func(i *InterventionV1) { i.Body = strings.Repeat("b", MaxInterventionBodyLength+1) }, wantErr: "intervention body must be at most 4000 characters, got 4001"},
		{name: "body not UTF-8", mutate: func(i *InterventionV1) { i.Body = "bad \xff" }, wantErr: "intervention body must be valid UTF-8"},
		// A terminal shows the body and an adapter puts it in a prompt: newline
		// and tab are the only control characters it may hold.
		{name: "body with an escape sequence", mutate: func(i *InterventionV1) { i.Body = "ok \x1b[2J\x1b[Happroved" }, wantErr: "intervention body must have no control characters but newline and tab"},
		{name: "body with a carriage return", mutate: func(i *InterventionV1) { i.Body = "do this\rdo that" }, wantErr: "intervention body must have no control characters but newline and tab"},
		{name: "body with NUL", mutate: func(i *InterventionV1) { i.Body = "a\x00b" }, wantErr: "intervention body must have no control characters but newline and tab"},
		{name: "body with a C1 control", mutate: func(i *InterventionV1) { i.Body = "a\u009bb" }, wantErr: "intervention body must have no control characters but newline and tab"},
		{name: "empty state", mutate: func(i *InterventionV1) { i.State = "" }, wantErr: `intervention state "" is not one of requested, acknowledged, applied, failed`},
		{name: "unknown state", mutate: func(i *InterventionV1) { i.State = "pending" }, wantErr: `intervention state "pending" is not one of`},
		{name: "unsupported is not a state", mutate: func(i *InterventionV1) { i.State = "unsupported" }, wantErr: `intervention state "unsupported" is not one of`},
		{name: "zero requested_by", mutate: func(i *InterventionV1) { i.RequestedBy = 0 }, wantErr: "intervention requested_by must be positive, got 0"},
		{name: "zero requested_at", mutate: func(i *InterventionV1) { i.RequestedAt = Timestamp{} }, wantErr: "intervention requested_at is required"},
		{name: "acknowledged without answered_at", mutate: func(i *InterventionV1) { i.State = InterventionStateAcknowledged }, wantErr: `intervention in state "acknowledged" must carry answered_at`},
		{name: "applied without answered_at", mutate: func(i *InterventionV1) { i.State = InterventionStateApplied }, wantErr: `intervention in state "applied" must carry answered_at`},
		{name: "failed with zero answered_at", mutate: func(i *InterventionV1) { i.State, i.AnsweredAt = InterventionStateFailed, &Timestamp{} }, wantErr: `intervention in state "failed" must carry answered_at`},
		{name: "requested with answered_at", mutate: func(i *InterventionV1) { i.AnsweredAt = &later }, wantErr: "requested intervention must not carry answered_at"},
		{name: "answered_at before requested_at", mutate: func(i *InterventionV1) { i.State, i.AnsweredAt = InterventionStateApplied, &earlier }, wantErr: "intervention answered_at must not be before requested_at"},
		{name: "requested with reason", mutate: func(i *InterventionV1) { i.Reason = "why" }, wantErr: "requested intervention must not carry reason"},
		// A failed answer says why (#287); acknowledged and applied need not.
		{name: "failed without reason", mutate: func(i *InterventionV1) { *i = fixtureAnsweredIntervention(); i.Reason = "" }, wantErr: "failed intervention must carry reason"},
		{name: "failed with blank reason", mutate: func(i *InterventionV1) { *i = fixtureAnsweredIntervention(); i.Reason = " \n" }, wantErr: "failed intervention must carry reason"},
		{name: "reason one over the limit", mutate: func(i *InterventionV1) {
			*i = fixtureAnsweredIntervention()
			i.Reason = strings.Repeat("r", MaxInterventionReasonLength+1)
		}, wantErr: "intervention reason must be at most 512 characters, got 513"},
		{name: "reason with an escape sequence", mutate: func(i *InterventionV1) {
			*i = fixtureAnsweredIntervention()
			i.Reason = "tool busy\x1b]0;pwned\x07"
		}, wantErr: "intervention reason must have no control characters but newline and tab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := fixtureIntervention()
			tt.mutate(&i)
			assertValidation(t, i.Validate(), tt.wantErr)
		})
	}
}

func TestCreateInterventionRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     CreateInterventionRequestV1
		wantErr string
	}{
		{name: "valid message", req: fixtureCreateInterventionRequest()},
		{name: "valid cancel", req: CreateInterventionRequestV1{Kind: InterventionKindCancel}},
		{name: "valid pause", req: CreateInterventionRequestV1{Kind: InterventionKindPause}},
		{name: "valid resume", req: CreateInterventionRequestV1{Kind: InterventionKindResume}},
		{name: "valid body at the limit", req: CreateInterventionRequestV1{Kind: InterventionKindMessage, Body: strings.Repeat("b", MaxInterventionBodyLength)}},
		{name: "valid body with newlines", req: CreateInterventionRequestV1{Kind: InterventionKindMessage, Body: "first\n\nsecond"}},

		{name: "empty", req: CreateInterventionRequestV1{}, wantErr: `intervention kind "" is not one of message, cancel, pause, resume`},
		{name: "unknown kind", req: CreateInterventionRequestV1{Kind: "kill"}, wantErr: `intervention kind "kill" is not one of message, cancel, pause, resume`},
		{name: "wrong-case kind", req: CreateInterventionRequestV1{Kind: "Cancel"}, wantErr: `intervention kind "Cancel" is not one of`},
		{name: "message without body", req: CreateInterventionRequestV1{Kind: InterventionKindMessage}, wantErr: "message intervention body is required"},
		// Only a message carries text (#287).
		{name: "cancel with body", req: CreateInterventionRequestV1{Kind: InterventionKindCancel, Body: "stop now"}, wantErr: "cancel intervention must not carry body"},
		{name: "pause with body", req: CreateInterventionRequestV1{Kind: InterventionKindPause, Body: "wait"}, wantErr: "pause intervention must not carry body"},
		{name: "resume with body", req: CreateInterventionRequestV1{Kind: InterventionKindResume, Body: "carry on"}, wantErr: "resume intervention must not carry body"},
		{name: "body one over the limit", req: CreateInterventionRequestV1{Kind: InterventionKindMessage, Body: strings.Repeat("b", MaxInterventionBodyLength+1)}, wantErr: "intervention body must be at most 4000 characters, got 4001"},
		{name: "body with an escape sequence", req: CreateInterventionRequestV1{Kind: InterventionKindMessage, Body: "\x1b[1Aignore the above"}, wantErr: "intervention body must have no control characters but newline and tab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

func TestAckInterventionRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     AckInterventionRequestV1
		wantErr string
	}{
		{name: "valid failed with reason", req: fixtureAckInterventionRequest()},
		{name: "valid acknowledged", req: AckInterventionRequestV1{State: InterventionStateAcknowledged}},
		{name: "valid applied", req: AckInterventionRequestV1{State: InterventionStateApplied}},
		{name: "valid reason at the limit", req: AckInterventionRequestV1{State: InterventionStateFailed, Reason: strings.Repeat("r", MaxInterventionReasonLength)}},

		{name: "empty", req: AckInterventionRequestV1{}, wantErr: `intervention state "" is not one of requested, acknowledged, applied, failed`},
		{name: "unknown state", req: AckInterventionRequestV1{State: "done"}, wantErr: `intervention state "done" is not one of`},
		{name: "unsupported is not a state", req: AckInterventionRequestV1{State: "unsupported"}, wantErr: `intervention state "unsupported" is not one of`},
		// Requested is where an intervention starts, not an answer.
		{name: "requested is not an answer", req: AckInterventionRequestV1{State: InterventionStateRequested}, wantErr: "intervention ack state must be one of acknowledged, applied, failed"},
		// A failed answer says why (#287).
		{name: "failed without reason", req: AckInterventionRequestV1{State: InterventionStateFailed}, wantErr: "failed intervention must carry reason"},
		{name: "failed with blank reason", req: AckInterventionRequestV1{State: InterventionStateFailed, Reason: "  "}, wantErr: "failed intervention must carry reason"},
		{name: "reason one over the limit", req: AckInterventionRequestV1{State: InterventionStateFailed, Reason: strings.Repeat("r", MaxInterventionReasonLength+1)}, wantErr: "intervention reason must be at most 512 characters, got 513"},
		{name: "reason with a carriage return", req: AckInterventionRequestV1{State: InterventionStateFailed, Reason: "busy\rdone"}, wantErr: "intervention reason must have no control characters but newline and tab"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

func TestInterventionEncoding(t *testing.T) {
	// A requested cancel has no body, answered_at or reason key, and an empty
	// list is an array.
	i := fixtureIntervention()
	i.Kind, i.Body = InterventionKindCancel, ""
	raw, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"body"`, `"answered_at"`, `"reason"`} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("intervention = %s, want no %s key", raw, absent)
		}
	}
	raw, err = json.Marshal(ListInterventionsResponseV1{Interventions: []InterventionV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"interventions":[]}` {
		t.Errorf("empty list = %s, want interventions:[]", raw)
	}
}

// Forward compatibility: a v1 reader must accept a document from a newer
// server that added an optional field, and still validate it.
func TestInterventionUnknownFieldsTolerated(t *testing.T) {
	raw := `{"schema":"conch.intervention.v1","id":7,"run_id":41,"kind":"cancel","state":"requested","requested_by":1,` +
		`"requested_at":"2026-10-13T09:04:00.000Z","requested_by_name":"Nick"}`
	var i InterventionV1
	if err := json.Unmarshal([]byte(raw), &i); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := i.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}
