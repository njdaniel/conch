package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRunGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "run answering a message, running, with a link", file: "run-v1.json", new: func() any { return new(RunV1) }},
		{name: "run with no message, completed", file: "run-v1-no-message.json", new: func() any { return new(RunV1) }},
		{name: "create request", file: "create-run-request-v1.json", new: func() any { return new(CreateRunRequestV1) }},
		{name: "set state request", file: "set-run-state-request-v1.json", new: func() any { return new(SetRunStateRequestV1) }},
		{name: "list response", file: "list-runs-response-v1.json", new: func() any { return new(ListRunsResponseV1) }},
		{name: "list response, empty", file: "list-runs-response-v1-empty.json", new: func() any { return new(ListRunsResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertP3GoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestRunSchemaName(t *testing.T) {
	if RunSchemaV1 != "conch.run.v1" {
		t.Fatalf("RunSchemaV1 = %q", RunSchemaV1)
	}
}

func TestRunErrorCodes(t *testing.T) {
	assertErrorCodes(t, map[string]string{
		ErrorCodeRunNotFound:       "run_not_found",
		ErrorCodeInvalidTransition: "invalid_transition",
	})
}

// allRunStates is the whole vocabulary, for the exhaustive tests below.
var allRunStates = []RunState{
	RunStateQueued, RunStateStarting, RunStateRunning, RunStateBlocked,
	RunStateWaiting, RunStateCompleted, RunStateFailed, RunStateCancelled,
}

func TestRunStateValid(t *testing.T) {
	for _, s := range allRunStates {
		if !s.Valid() {
			t.Errorf("run state %q should be valid", s)
		}
	}
	for _, s := range []RunState{"", "done", "succeeded", "canceled", "paused", "online", "idle", "Running", "QUEUED", " running", "*"} {
		if s.Valid() {
			t.Errorf("run state %q should be invalid", s)
		}
	}
}

func TestRunStateTerminal(t *testing.T) {
	terminal := map[RunState]bool{RunStateCompleted: true, RunStateFailed: true, RunStateCancelled: true}
	for _, s := range allRunStates {
		if got := RunStateTerminal(s); got != terminal[s] {
			t.Errorf("RunStateTerminal(%q) = %v, want %v", s, got, terminal[s])
		}
	}
	for _, s := range []RunState{"", "done", "Completed"} {
		if RunStateTerminal(s) {
			t.Errorf("RunStateTerminal(%q) = true for an unknown state", s)
		}
	}
}

// runTransitionTable is the issue's table restated independently of the
// package's copy, so a change to one without the other fails here.
var runTransitionTable = map[RunState][]RunState{
	RunStateQueued:   {RunStateStarting, RunStateRunning, RunStateFailed, RunStateCancelled},
	RunStateStarting: {RunStateRunning, RunStateFailed, RunStateCancelled},
	RunStateRunning:  {RunStateBlocked, RunStateWaiting, RunStateCompleted, RunStateFailed, RunStateCancelled},
	RunStateBlocked:  {RunStateRunning, RunStateFailed, RunStateCancelled},
	RunStateWaiting:  {RunStateRunning, RunStateCompleted, RunStateFailed, RunStateCancelled},
}

// TestRunTransitionAllowed checks every ordered pair of states, legal and
// illegal, including each state against itself and every exit from each
// terminal state, plus pairs involving an unknown state.
func TestRunTransitionAllowed(t *testing.T) {
	allowed := map[[2]RunState]bool{}
	for from, tos := range runTransitionTable {
		for _, to := range tos {
			allowed[[2]RunState{from, to}] = true
		}
	}
	legal := 0
	for _, from := range allRunStates {
		for _, to := range allRunStates {
			want := allowed[[2]RunState{from, to}]
			if want {
				legal++
			}
			if got := RunTransitionAllowed(from, to); got != want {
				t.Errorf("RunTransitionAllowed(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	if legal != 19 {
		t.Errorf("the table lists %d legal transitions, want 19", legal)
	}

	// A terminal state has no exit, not even to itself or to another
	// terminal state.
	for _, from := range allRunStates {
		if !RunStateTerminal(from) {
			continue
		}
		for _, to := range allRunStates {
			if RunTransitionAllowed(from, to) {
				t.Errorf("terminal state %q may move to %q", from, to)
			}
		}
	}
	// Nothing returns to queued and nothing stays put.
	for _, s := range allRunStates {
		if RunTransitionAllowed(s, RunStateQueued) {
			t.Errorf("%q may move back to queued", s)
		}
		if RunTransitionAllowed(s, s) {
			t.Errorf("%q may move to itself", s)
		}
	}
	// An unknown state is never a source or a target.
	for _, s := range allRunStates {
		if RunTransitionAllowed(s, "done") || RunTransitionAllowed("done", s) || RunTransitionAllowed(s, "") || RunTransitionAllowed("", s) {
			t.Errorf("an unknown state takes part in a transition with %q", s)
		}
	}
}

var (
	runStartedAt = NewTimestamp(time.Date(2026, 10, 13, 9, 1, 0, 0, time.UTC))
	runEndedAt   = NewTimestamp(time.Date(2026, 10, 13, 8, 34, 12, 500_000_000, time.UTC))
)

// fixtureRun is run-v1.json: run 41 of session 12, answering message 1203,
// running, with a link.
func fixtureRun() RunV1 {
	return RunV1{
		Schema:      RunSchemaV1,
		ID:          41,
		SessionID:   12,
		PrincipalID: 3,
		ChannelID:   7,
		MessageID:   1203,
		State:       RunStateRunning,
		Task:        "Answer the question in #ops about the failing deploy",
		Link:        "https://github.com/njdaniel/conch/pull/262",
		StartedAt:   runStartedAt,
	}
}

// fixtureCompletedRun is run-v1-no-message.json: a completed run visible to
// the whole channel, with an outcome and no link.
func fixtureCompletedRun() RunV1 {
	ended := runEndedAt
	return RunV1{
		Schema:      RunSchemaV1,
		ID:          40,
		SessionID:   12,
		PrincipalID: 3,
		ChannelID:   7,
		State:       RunStateCompleted,
		Task:        "Summarize overnight alerts",
		StartedAt:   NewTimestamp(time.Date(2026, 10, 13, 8, 30, 0, 0, time.UTC)),
		EndedAt:     &ended,
		Outcome:     "Posted a summary of 3 alerts",
	}
}

func fixtureCreateRunRequest() CreateRunRequestV1 {
	return CreateRunRequestV1{
		Channel:   "ops",
		MessageID: 1203,
		Task:      "Answer the question in #ops about the failing deploy",
		Link:      "https://github.com/njdaniel/conch/pull/262",
	}
}

func fixtureSetRunStateRequest() SetRunStateRequestV1 {
	return SetRunStateRequestV1{State: RunStateCompleted, Outcome: "Posted a summary of 3 alerts"}
}

// linkOfLength builds an https URL of exactly n characters.
func linkOfLength(n int) string {
	const prefix = "https://example.com/"
	return prefix + strings.Repeat("a", n-len(prefix))
}

func TestRunV1Validate(t *testing.T) {
	later := NewTimestamp(runStartedAt.Time().Add(time.Minute))
	earlier := NewTimestamp(runStartedAt.Time().Add(-time.Second))
	tests := []struct {
		name    string
		mutate  func(*RunV1)
		wantErr string
	}{
		{name: "valid, running with a message and a link", mutate: func(*RunV1) {}},
		{name: "valid, completed without a message", mutate: func(r *RunV1) { *r = fixtureCompletedRun() }},
		{name: "valid, queued", mutate: func(r *RunV1) { r.State = RunStateQueued }},
		{name: "valid, starting", mutate: func(r *RunV1) { r.State = RunStateStarting }},
		{name: "valid, blocked", mutate: func(r *RunV1) { r.State = RunStateBlocked }},
		{name: "valid, waiting", mutate: func(r *RunV1) { r.State = RunStateWaiting }},
		{name: "valid, failed with outcome", mutate: func(r *RunV1) { r.State, r.EndedAt, r.Outcome = RunStateFailed, &later, "go test failed" }},
		{name: "valid, cancelled without outcome", mutate: func(r *RunV1) { r.State, r.EndedAt = RunStateCancelled, &later }},
		{name: "valid, ended_at equals started_at", mutate: func(r *RunV1) { ended := runStartedAt; r.State, r.EndedAt = RunStateCompleted, &ended }},
		{name: "valid, no link", mutate: func(r *RunV1) { r.Link = "" }},
		{name: "valid, link with query and fragment", mutate: func(r *RunV1) { r.Link = "https://ci.example/job?id=7#log" }},
		{name: "valid, link with a port", mutate: func(r *RunV1) { r.Link = "https://localhost:8443/run/41" }},
		// Schemes are case-insensitive (RFC 3986); the parser normalizes.
		{name: "valid, link with upper-case scheme", mutate: func(r *RunV1) { r.Link = "HTTPS://example.com/run" }},
		{name: "valid task at the limit", mutate: func(r *RunV1) { r.Task = strings.Repeat("t", MaxRunTaskLength) }},
		{name: "valid task at the limit in multibyte characters", mutate: func(r *RunV1) { r.Task = strings.Repeat("é", MaxRunTaskLength) }},
		{name: "valid link at the limit", mutate: func(r *RunV1) { r.Link = linkOfLength(MaxRunLinkLength) }},
		{name: "valid outcome at the limit", mutate: func(r *RunV1) { *r = fixtureCompletedRun(); r.Outcome = strings.Repeat("o", MaxRunOutcomeLength) }},

		{name: "empty schema", mutate: func(r *RunV1) { r.Schema = "" }, wantErr: `run schema must be "conch.run.v1", got ""`},
		{name: "session schema", mutate: func(r *RunV1) { r.Schema = SessionSchemaV1 }, wantErr: `got "conch.session.v1"`},
		{name: "future schema version", mutate: func(r *RunV1) { r.Schema = "conch.run.v2" }, wantErr: `got "conch.run.v2"`},
		{name: "zero id", mutate: func(r *RunV1) { r.ID = 0 }, wantErr: "run id must be positive, got 0"},
		{name: "zero session", mutate: func(r *RunV1) { r.SessionID = 0 }, wantErr: "run session_id must be positive, got 0"},
		{name: "negative principal", mutate: func(r *RunV1) { r.PrincipalID = -3 }, wantErr: "run principal_id must be positive, got -3"},
		{name: "zero channel_id", mutate: func(r *RunV1) { r.ChannelID = 0 }, wantErr: "run channel_id must be positive, got 0"},
		{name: "negative channel_id", mutate: func(r *RunV1) { r.ChannelID = -7 }, wantErr: "run channel_id must be positive, got -7"},
		{name: "negative message_id", mutate: func(r *RunV1) { r.MessageID = -1 }, wantErr: "run message_id must be omitted or positive, got -1"},
		{name: "empty task", mutate: func(r *RunV1) { r.Task = "" }, wantErr: "run task is required"},
		{name: "blank task", mutate: func(r *RunV1) { r.Task = "\n" }, wantErr: "run task is required"},
		{name: "task one over the limit", mutate: func(r *RunV1) { r.Task = strings.Repeat("t", MaxRunTaskLength+1) }, wantErr: "run task must be at most 512 characters, got 513"},
		{name: "task not UTF-8", mutate: func(r *RunV1) { r.Task = "bad \xff byte" }, wantErr: "run task must be valid UTF-8"},
		// A task is one row of a roster.
		{name: "task with newline", mutate: func(r *RunV1) { r.Task = "first\nsecond" }, wantErr: "run task must be one line with no control characters"},
		{name: "task with carriage return", mutate: func(r *RunV1) { r.Task = "first\rsecond" }, wantErr: "run task must be one line with no control characters"},
		{name: "task with tab", mutate: func(r *RunV1) { r.Task = "first\tsecond" }, wantErr: "run task must be one line with no control characters"},
		{name: "task with escape", mutate: func(r *RunV1) { r.Task = "red \x1b[31mtask" }, wantErr: "run task must be one line with no control characters"},
		{name: "task with NUL", mutate: func(r *RunV1) { r.Task = "first\x00second" }, wantErr: "run task must be one line with no control characters"},

		{name: "link one over the limit", mutate: func(r *RunV1) { r.Link = linkOfLength(MaxRunLinkLength + 1) }, wantErr: "run link must be at most 512 characters, got 513"},
		{name: "link http", mutate: func(r *RunV1) { r.Link = "http://example.com/run" }, wantErr: "run link must be an https URL"},
		{name: "link javascript", mutate: func(r *RunV1) { r.Link = "javascript:alert(1)" }, wantErr: "run link must be an https URL"},
		{name: "link file", mutate: func(r *RunV1) { r.Link = "file:///etc/passwd" }, wantErr: "run link must be an https URL"},
		{name: "link relative", mutate: func(r *RunV1) { r.Link = "/pull/262" }, wantErr: "run link must be an https URL"},
		{name: "link scheme-relative", mutate: func(r *RunV1) { r.Link = "//example.com/run" }, wantErr: "run link must be an https URL"},
		{name: "link without host", mutate: func(r *RunV1) { r.Link = "https:///run" }, wantErr: "run link must be an https URL"},
		{name: "link bare word", mutate: func(r *RunV1) { r.Link = "example.com" }, wantErr: "run link must be an https URL"},
		{name: "link with a space", mutate: func(r *RunV1) { r.Link = "https://example.com/a b" }, wantErr: "run link must not contain whitespace or control characters"},
		{name: "link with a tab", mutate: func(r *RunV1) { r.Link = "https://example.com/a\tb" }, wantErr: "run link must not contain whitespace or control characters"},
		{name: "link with a newline", mutate: func(r *RunV1) { r.Link = "https://example.com/a\nb" }, wantErr: "run link must not contain whitespace or control characters"},
		{name: "link not UTF-8", mutate: func(r *RunV1) { r.Link = "https://example.com/\xff" }, wantErr: "run link must be valid UTF-8"},
		// Invisible characters: a link that reads one way and opens another.
		{name: "link with bidi override", mutate: func(r *RunV1) { r.Link = "https://example.com/" + string(rune(0x202e)) + "x" }, wantErr: "run link must not contain bidi controls or invisible formatting characters"},
		{name: "link with bidi isolate", mutate: func(r *RunV1) { r.Link = "https://example.com/" + string(rune(0x2066)) + "x" }, wantErr: "run link must not contain bidi controls or invisible formatting characters"},
		{name: "link with zero-width space", mutate: func(r *RunV1) { r.Link = "https://exam" + string(rune(0x200b)) + "ple.com/x" }, wantErr: "run link must not contain bidi controls or invisible formatting characters"},
		{name: "link with zero-width joiner", mutate: func(r *RunV1) { r.Link = "https://example.com/" + string(rune(0x200d)) }, wantErr: "run link must not contain bidi controls or invisible formatting characters"},
		{name: "link with word joiner", mutate: func(r *RunV1) { r.Link = "https://example.com/" + string(rune(0x2060)) }, wantErr: "run link must not contain bidi controls or invisible formatting characters"},
		{name: "link with soft hyphen", mutate: func(r *RunV1) { r.Link = "https://exam" + string(rune(0xad)) + "ple.com/" }, wantErr: "run link must not contain bidi controls or invisible formatting characters"},
		{name: "link with line separator", mutate: func(r *RunV1) { r.Link = "https://example.com/" + string(rune(0x2028)) }, wantErr: "run link must not contain whitespace or control characters"},
		// userinfo: what reads as the host is not where the link goes.
		{name: "link with userinfo host", mutate: func(r *RunV1) { r.Link = "https://github.com@evil.example/x" }, wantErr: "run link must not carry userinfo"},
		{name: "link with user and password", mutate: func(r *RunV1) { r.Link = "https://user:secret@example.com/x" }, wantErr: "run link must not carry userinfo"},
		{name: "link with empty userinfo", mutate: func(r *RunV1) { r.Link = "https://@example.com/x" }, wantErr: "run link must not carry userinfo"},

		{name: "empty state", mutate: func(r *RunV1) { r.State = "" }, wantErr: `run state "" is not one of queued, starting, running, blocked, waiting, completed, failed, cancelled`},
		{name: "unknown state", mutate: func(r *RunV1) { r.State = "done" }, wantErr: `run state "done" is not one of`},
		{name: "session status is not a run state", mutate: func(r *RunV1) { r.State = "idle" }, wantErr: `run state "idle" is not one of`},
		{name: "zero started_at", mutate: func(r *RunV1) { r.StartedAt = Timestamp{} }, wantErr: "run started_at is required"},
		{name: "completed without ended_at", mutate: func(r *RunV1) { r.State = RunStateCompleted }, wantErr: `run in state "completed" must carry ended_at`},
		{name: "failed without ended_at", mutate: func(r *RunV1) { r.State = RunStateFailed }, wantErr: `run in state "failed" must carry ended_at`},
		{name: "cancelled with zero ended_at", mutate: func(r *RunV1) { r.State, r.EndedAt = RunStateCancelled, &Timestamp{} }, wantErr: `run in state "cancelled" must carry ended_at`},
		{name: "running with ended_at", mutate: func(r *RunV1) { r.EndedAt = &later }, wantErr: `run in state "running" must not carry ended_at`},
		{name: "queued with ended_at", mutate: func(r *RunV1) { r.State, r.EndedAt = RunStateQueued, &later }, wantErr: `run in state "queued" must not carry ended_at`},
		{name: "ended_at before started_at", mutate: func(r *RunV1) { r.State, r.EndedAt = RunStateCompleted, &earlier }, wantErr: "run ended_at must not be before started_at"},
		{name: "outcome one over the limit", mutate: func(r *RunV1) { *r = fixtureCompletedRun(); r.Outcome = strings.Repeat("o", MaxRunOutcomeLength+1) }, wantErr: "run outcome must be at most 512 characters, got 513"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fixtureRun()
			tt.mutate(&r)
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

func TestCreateRunRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CreateRunRequestV1)
		wantErr string
	}{
		{name: "valid, with message and link", mutate: func(*CreateRunRequestV1) {}},
		{name: "valid, channel-wide without link", mutate: func(r *CreateRunRequestV1) { r.MessageID, r.Link = 0, "" }},
		{name: "valid task at the limit", mutate: func(r *CreateRunRequestV1) { r.Task = strings.Repeat("t", MaxRunTaskLength) }},
		{name: "valid link at the limit", mutate: func(r *CreateRunRequestV1) { r.Link = linkOfLength(MaxRunLinkLength) }},

		{name: "empty channel", mutate: func(r *CreateRunRequestV1) { r.Channel = "" }, wantErr: "run channel is required"},
		{name: "blank channel", mutate: func(r *CreateRunRequestV1) { r.Channel = " \t" }, wantErr: "run channel is required"},
		{name: "negative message_id", mutate: func(r *CreateRunRequestV1) { r.MessageID = -5 }, wantErr: "run message_id must be omitted or positive, got -5"},
		{name: "empty task", mutate: func(r *CreateRunRequestV1) { r.Task = "" }, wantErr: "run task is required"},
		{name: "task one over the limit", mutate: func(r *CreateRunRequestV1) { r.Task = strings.Repeat("t", MaxRunTaskLength+1) }, wantErr: "run task must be at most 512 characters, got 513"},
		{name: "task with newline", mutate: func(r *CreateRunRequestV1) { r.Task = "first\nsecond" }, wantErr: "run task must be one line with no control characters"},
		{name: "link with userinfo host", mutate: func(r *CreateRunRequestV1) { r.Link = "https://github.com@evil.example/x" }, wantErr: "run link must not carry userinfo"},
		{name: "link with bidi override", mutate: func(r *CreateRunRequestV1) { r.Link = "https://example.com/" + string(rune(0x202e)) }, wantErr: "run link must not contain bidi controls or invisible formatting characters"},
		{name: "link one over the limit", mutate: func(r *CreateRunRequestV1) { r.Link = linkOfLength(MaxRunLinkLength + 1) }, wantErr: "run link must be at most 512 characters, got 513"},
		{name: "link http", mutate: func(r *CreateRunRequestV1) { r.Link = "http://example.com/" }, wantErr: "run link must be an https URL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fixtureCreateRunRequest()
			tt.mutate(&r)
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

func TestSetRunStateRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		req     SetRunStateRequestV1
		wantErr string
	}{
		{name: "valid, completed with outcome", req: fixtureSetRunStateRequest()},
		{name: "valid, running without outcome", req: SetRunStateRequestV1{State: RunStateRunning}},
		{name: "valid, outcome at the limit", req: SetRunStateRequestV1{State: RunStateFailed, Outcome: strings.Repeat("o", MaxRunOutcomeLength)}},
		// The schema does not know the current state; reachability is the
		// server's, so even queued passes here.
		{name: "valid vocabulary, reachability not checked", req: SetRunStateRequestV1{State: RunStateQueued}},

		{name: "empty state", req: SetRunStateRequestV1{}, wantErr: `run state "" is not one of`},
		{name: "unknown state", req: SetRunStateRequestV1{State: "done"}, wantErr: `run state "done" is not one of`},
		{name: "wrong-case state", req: SetRunStateRequestV1{State: "Completed"}, wantErr: `run state "Completed" is not one of`},
		{name: "outcome one over the limit", req: SetRunStateRequestV1{State: RunStateFailed, Outcome: strings.Repeat("o", MaxRunOutcomeLength+1)}, wantErr: "run outcome must be at most 512 characters, got 513"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidation(t, tt.req.Validate(), tt.wantErr)
		})
	}
}

func TestRunEncoding(t *testing.T) {
	// A channel-wide running run has no message_id, link, ended_at or outcome
	// key at all; an empty list is an array.
	r := fixtureRun()
	r.MessageID, r.Link = 0, ""
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{`"message_id"`, `"link"`, `"ended_at"`, `"outcome"`} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("run = %s, want no %s key", raw, absent)
		}
	}
	raw, err = json.Marshal(ListRunsResponseV1{Runs: []RunV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"runs":[]}` {
		t.Errorf("empty page = %s, want runs:[] and no next_after", raw)
	}
	raw, err = json.Marshal(ListRunsResponseV1{Runs: []RunV1{fixtureRun()}, NextAfter: 41})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"next_after":41`) {
		t.Errorf("page with more = %s, want next_after:41", raw)
	}
}
