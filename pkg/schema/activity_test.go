package schema

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestActivityGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "command finished, with data", file: "agent-activity-v1.json", new: func() any { return new(AgentActivityV1) }},
		{name: "run started, no data", file: "agent-activity-v1-run-started.json", new: func() any { return new(AgentActivityV1) }},
		{name: "append request", file: "append-activity-request-v1.json", new: func() any { return new(AppendActivityRequestV1) }},
		{name: "list response, one page", file: "list-activity-response-v1.json", new: func() any { return new(ListActivityResponseV1) }},
		{name: "list response, empty", file: "list-activity-response-v1-empty.json", new: func() any { return new(ListActivityResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertP3GoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestAgentActivitySchemaName(t *testing.T) {
	if AgentActivitySchemaV1 != "conch.agent.activity.v1" {
		t.Fatalf("AgentActivitySchemaV1 = %q", AgentActivitySchemaV1)
	}
}

func TestActivityKindValid(t *testing.T) {
	for _, k := range []ActivityKind{
		ActivityKindRunStarted, ActivityKindCommandStarted, ActivityKindCommandFinished, ActivityKindReplyPosted,
		ActivityKindApprovalRequested, ActivityKindApprovalResolved, ActivityKindInterventionReceived, ActivityKindRunEnded,
	} {
		if !k.Valid() {
			t.Errorf("activity kind %q should be valid", k)
		}
	}
	for _, k := range []ActivityKind{"", "run_finished", "command_failed", "message", "cancel", "Run_Started", "RUN_ENDED", "run-started", " reply_posted", "*"} {
		if k.Valid() {
			t.Errorf("activity kind %q should be invalid", k)
		}
	}
}

var activityAt = NewTimestamp(time.Date(2026, 10, 13, 9, 3, 10, 0, time.UTC))

// fixtureActivity is agent-activity-v1.json: a finished command with its
// detail record.
func fixtureActivity() AgentActivityV1 {
	return AgentActivityV1{
		Schema:      AgentActivitySchemaV1,
		ID:          507,
		RunID:       41,
		PrincipalID: 3,
		Kind:        ActivityKindCommandFinished,
		Summary:     "ran go test ./...",
		Outcome:     "exit 0",
		At:          activityAt,
		Data:        json.RawMessage(`{"command":"go test ./...","exit_code":0,"duration_ms":4210}`),
	}
}

// fixtureRunStartedActivity is agent-activity-v1-run-started.json: the first
// entry of run 41, with no outcome and no data.
func fixtureRunStartedActivity() AgentActivityV1 {
	return AgentActivityV1{
		Schema:      AgentActivitySchemaV1,
		ID:          501,
		RunID:       41,
		PrincipalID: 3,
		Kind:        ActivityKindRunStarted,
		Summary:     "Run started",
		At:          NewTimestamp(time.Date(2026, 10, 13, 9, 1, 0, 0, time.UTC)),
	}
}

func fixtureAppendActivityRequest() AppendActivityRequestV1 {
	return AppendActivityRequestV1{
		Kind:    ActivityKindCommandFinished,
		Summary: "ran go test ./...",
		Outcome: "exit 0",
		Data:    json.RawMessage(`{"command":"go test ./...","exit_code":0,"duration_ms":4210}`),
	}
}

// dataOfBytes builds a compact JSON object of exactly n bytes whose encoded
// form is the same n bytes (no whitespace, nothing HTML-escaped).
func dataOfBytes(n int) json.RawMessage {
	const frame = `{"pad":""}`
	return json.RawMessage(`{"pad":"` + strings.Repeat("a", n-len(frame)) + `"}`)
}

// dataOfLessThans builds a JSON string of exactly n bytes raw, every content
// byte a "<", which encoding/json writes as the six bytes <: its
// encoded form is about six times n.
func dataOfLessThans(n int) json.RawMessage {
	return json.RawMessage(`"` + strings.Repeat("<", n-2) + `"`)
}

func TestAgentActivityV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*AgentActivityV1)
		wantErr string
	}{
		{name: "valid, command finished with data", mutate: func(*AgentActivityV1) {}},
		{name: "valid, run started without data", mutate: func(a *AgentActivityV1) { *a = fixtureRunStartedActivity() }},
		{name: "valid, every kind", mutate: func(a *AgentActivityV1) { a.Kind = ActivityKindInterventionReceived }},
		{name: "valid, nil data", mutate: func(a *AgentActivityV1) { a.Data = nil }},
		{name: "valid, empty data is absent", mutate: func(a *AgentActivityV1) { a.Data = json.RawMessage{} }},
		{name: "valid, data is an array", mutate: func(a *AgentActivityV1) { a.Data = json.RawMessage(`[1,2,3]`) }},
		{name: "valid, data is a string", mutate: func(a *AgentActivityV1) { a.Data = json.RawMessage(`"detail"`) }},
		{name: "valid, data at the size limit", mutate: func(a *AgentActivityV1) { a.Data = dataOfBytes(MaxActivityDataBytes) }},
		{
			// The bound is on the encoded form: raw whitespace is compacted
			// away, so a value far over the limit as received can be fine.
			name: "valid, data over the limit raw but compact within it",
			mutate: func(a *AgentActivityV1) {
				a.Data = json.RawMessage(`{"pad":   ` + strings.Repeat(" ", MaxActivityDataBytes) + `"a"}`)
			},
		},
		{name: "valid, data with HTML characters within the encoded limit", mutate: func(a *AgentActivityV1) { a.Data = dataOfLessThans(1000) }},
		{name: "valid summary at the limit", mutate: func(a *AgentActivityV1) { a.Summary = strings.Repeat("s", MaxActivitySummaryLength) }},
		{name: "valid summary at the limit in multibyte characters", mutate: func(a *AgentActivityV1) { a.Summary = strings.Repeat("é", MaxActivitySummaryLength) }},
		{name: "valid outcome at the limit", mutate: func(a *AgentActivityV1) { a.Outcome = strings.Repeat("o", MaxActivityOutcomeLength) }},
		{name: "valid outcome with newline", mutate: func(a *AgentActivityV1) { a.Outcome = "exit 1\nsee log" }},

		{name: "empty schema", mutate: func(a *AgentActivityV1) { a.Schema = "" }, wantErr: `agent activity schema must be "conch.agent.activity.v1", got ""`},
		{name: "run schema", mutate: func(a *AgentActivityV1) { a.Schema = RunSchemaV1 }, wantErr: `got "conch.run.v1"`},
		{name: "future schema version", mutate: func(a *AgentActivityV1) { a.Schema = "conch.agent.activity.v2" }, wantErr: `got "conch.agent.activity.v2"`},
		{name: "zero id", mutate: func(a *AgentActivityV1) { a.ID = 0 }, wantErr: "agent activity id must be positive, got 0"},
		{name: "zero run_id", mutate: func(a *AgentActivityV1) { a.RunID = 0 }, wantErr: "agent activity run_id must be positive, got 0"},
		{name: "negative principal_id", mutate: func(a *AgentActivityV1) { a.PrincipalID = -3 }, wantErr: "agent activity principal_id must be positive, got -3"},
		{name: "empty kind", mutate: func(a *AgentActivityV1) { a.Kind = "" }, wantErr: `agent activity kind "" is not one of run_started, command_started, command_finished, reply_posted, approval_requested, approval_resolved, intervention_received, run_ended`},
		{name: "unknown kind", mutate: func(a *AgentActivityV1) { a.Kind = "command_failed" }, wantErr: `agent activity kind "command_failed" is not one of`},
		{name: "intervention kind is not an activity kind", mutate: func(a *AgentActivityV1) { a.Kind = "cancel" }, wantErr: `agent activity kind "cancel" is not one of`},
		{name: "empty summary", mutate: func(a *AgentActivityV1) { a.Summary = "" }, wantErr: "agent activity summary is required"},
		{name: "blank summary", mutate: func(a *AgentActivityV1) { a.Summary = " \n" }, wantErr: "agent activity summary is required"},
		{name: "summary one over the limit", mutate: func(a *AgentActivityV1) { a.Summary = strings.Repeat("s", MaxActivitySummaryLength+1) }, wantErr: "agent activity summary must be at most 512 characters, got 513"},
		{name: "summary not UTF-8", mutate: func(a *AgentActivityV1) { a.Summary = "bad \xff" }, wantErr: "agent activity summary must be valid UTF-8"},
		// A summary is one row of a timeline.
		{name: "summary with newline", mutate: func(a *AgentActivityV1) { a.Summary = "line one\nline two" }, wantErr: "agent activity summary must be one line with no control characters"},
		{name: "summary with carriage return", mutate: func(a *AgentActivityV1) { a.Summary = "ran\rtests" }, wantErr: "agent activity summary must be one line with no control characters"},
		{name: "summary with tab", mutate: func(a *AgentActivityV1) { a.Summary = "ran\ttests" }, wantErr: "agent activity summary must be one line with no control characters"},
		{name: "summary with escape", mutate: func(a *AgentActivityV1) { a.Summary = "ran \x1b[31mtests" }, wantErr: "agent activity summary must be one line with no control characters"},
		{name: "summary with NUL", mutate: func(a *AgentActivityV1) { a.Summary = "ran\x00tests" }, wantErr: "agent activity summary must be one line with no control characters"},
		{name: "outcome one over the limit", mutate: func(a *AgentActivityV1) { a.Outcome = strings.Repeat("o", MaxActivityOutcomeLength+1) }, wantErr: "agent activity outcome must be at most 512 characters, got 513"},
		{name: "zero at", mutate: func(a *AgentActivityV1) { a.At = Timestamp{} }, wantErr: "agent activity at is required"},
		{name: "data one byte over the size limit", mutate: func(a *AgentActivityV1) { a.Data = dataOfBytes(MaxActivityDataBytes + 1) }, wantErr: "agent activity data must be at most 8192 bytes encoded, got 8193"},
		{
			// Under the limit as received, far over it once "<" is escaped.
			name:    "data under the limit raw but over it encoded",
			mutate:  func(a *AgentActivityV1) { a.Data = dataOfLessThans(MaxActivityDataBytes) },
			wantErr: "agent activity data must be at most 8192 bytes encoded, got 49142",
		},
		{name: "data not JSON", mutate: func(a *AgentActivityV1) { a.Data = json.RawMessage(`{not json`) }, wantErr: "agent activity data is not valid JSON"},
		{name: "data two values", mutate: func(a *AgentActivityV1) { a.Data = json.RawMessage(`{} {}`) }, wantErr: "agent activity data is not valid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fixtureActivity()
			tt.mutate(&a)
			assertValidation(t, a.Validate(), tt.wantErr)
		})
	}
}

func TestAppendActivityRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*AppendActivityRequestV1)
		wantErr string
	}{
		{name: "valid, with outcome and data", mutate: func(*AppendActivityRequestV1) {}},
		{name: "valid, summary only", mutate: func(r *AppendActivityRequestV1) { r.Outcome, r.Data = "", nil }},
		{name: "valid, data at the size limit", mutate: func(r *AppendActivityRequestV1) { r.Data = dataOfBytes(MaxActivityDataBytes) }},
		{name: "valid summary at the limit", mutate: func(r *AppendActivityRequestV1) { r.Summary = strings.Repeat("s", MaxActivitySummaryLength) }},

		{name: "empty kind", mutate: func(r *AppendActivityRequestV1) { r.Kind = "" }, wantErr: `agent activity kind "" is not one of`},
		{name: "unknown kind", mutate: func(r *AppendActivityRequestV1) { r.Kind = "thought" }, wantErr: `agent activity kind "thought" is not one of`},
		{name: "empty summary", mutate: func(r *AppendActivityRequestV1) { r.Summary = "" }, wantErr: "agent activity summary is required"},
		{name: "summary one over the limit", mutate: func(r *AppendActivityRequestV1) { r.Summary = strings.Repeat("s", MaxActivitySummaryLength+1) }, wantErr: "agent activity summary must be at most 512 characters, got 513"},
		{name: "summary with newline", mutate: func(r *AppendActivityRequestV1) { r.Summary = "one\ntwo" }, wantErr: "agent activity summary must be one line with no control characters"},
		{name: "outcome one over the limit", mutate: func(r *AppendActivityRequestV1) { r.Outcome = strings.Repeat("o", MaxActivityOutcomeLength+1) }, wantErr: "agent activity outcome must be at most 512 characters, got 513"},
		{name: "data one byte over the size limit", mutate: func(r *AppendActivityRequestV1) { r.Data = dataOfBytes(MaxActivityDataBytes + 1) }, wantErr: "agent activity data must be at most 8192 bytes encoded, got 8193"},
		{name: "data under the limit raw but over it encoded", mutate: func(r *AppendActivityRequestV1) { r.Data = dataOfLessThans(MaxActivityDataBytes) }, wantErr: "agent activity data must be at most 8192 bytes encoded"},
		{name: "data not JSON", mutate: func(r *AppendActivityRequestV1) { r.Data = json.RawMessage(`nope`) }, wantErr: "agent activity data is not valid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fixtureAppendActivityRequest()
			tt.mutate(&r)
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

// Data an adapter sent in a shape this build does not know must come out of a
// decode/re-encode round trip as the same JSON value, as a message payload
// does: key order, number spelling and nesting kept, since the schema never
// decodes it into a type. What changes is only the encoding: whitespace is
// compacted and "<", ">" and "&" are written as \u escapes.
func TestAgentActivityDataValueIsPreserved(t *testing.T) {
	raw := `{"schema":"conch.agent.activity.v1","id":9,"run_id":41,"principal_id":3,"kind":"command_finished","summary":"x","at":"2026-10-13T09:03:10.000Z",` +
		`"data":{"zeta":1, "alpha":[true,null,"s"],"nested":{"deep":{"deeper":1.50}}, "html":"a<b"}}`
	var a AgentActivityV1
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	out, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	// The "<" comes out as the six-byte escape; the backslash is spelled as
	// a rune so no tool between here and the file decodes the escape.
	want := `"data":{"zeta":1,"alpha":[true,null,"s"],"nested":{"deep":{"deeper":1.50}},"html":"a` + string(rune('\\')) + `u003cb"}`
	if !bytes.Contains(out, []byte(want)) {
		t.Errorf("re-encoded entry = %s, want it to carry %s", out, want)
	}
}

// A value that passes Validate must still pass after the server has encoded
// it and a client has decoded it: the bound is on the encoded form, so the
// round trip cannot push a value over it.
func TestAgentActivityDataBoundSurvivesRoundTrip(t *testing.T) {
	for _, data := range []json.RawMessage{
		dataOfBytes(MaxActivityDataBytes),
		dataOfLessThans(1000),
		json.RawMessage(`{"pad":   ` + strings.Repeat(" ", MaxActivityDataBytes) + `"a"}`),
	} {
		a := fixtureActivity()
		a.Data = data
		if err := a.Validate(); err != nil {
			t.Fatalf("before round trip: Validate() = %v", err)
		}
		out, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		var back AgentActivityV1
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatal(err)
		}
		if err := back.Validate(); err != nil {
			t.Errorf("after round trip: Validate() = %v", err)
		}
	}
}

func TestActivityEncoding(t *testing.T) {
	// An entry with no outcome and no data has neither key, and an empty page
	// is an array with no cursor.
	raw, err := json.Marshal(fixtureRunStartedActivity())
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"outcome"`, `"data"`} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("entry = %s, want no %s key", raw, absent)
		}
	}
	raw, err = json.Marshal(ListActivityResponseV1{Activity: []AgentActivityV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"activity":[]}` {
		t.Errorf("empty page = %s, want activity:[] and no next_after", raw)
	}
}

// Forward compatibility: a v1 reader must accept an entry from a newer server
// that added an optional field, and still validate it.
func TestAgentActivityUnknownFieldsTolerated(t *testing.T) {
	raw := `{"schema":"conch.agent.activity.v1","id":9,"run_id":41,"principal_id":3,"kind":"reply_posted","summary":"replied","at":"2026-10-13T09:03:10.000Z","message_id":1204}`
	var a AgentActivityV1
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}
