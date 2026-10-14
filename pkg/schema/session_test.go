package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// assertP3GoldenFixture is assertGoldenFixture plus a validation pass for the
// P3 shapes: a document validates itself, and a list response validates each
// entry it carries. A published fixture must be a value the schema accepts.
func assertP3GoldenFixture(t *testing.T, file string, new func() any) {
	t.Helper()
	assertGoldenFixture(t, file, new)
	raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name comes from this package's own test table
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	value := new()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	if err := validateP3Fixture(value); err != nil {
		t.Errorf("fixture does not validate: %v", err)
	}
}

func validateP3Fixture(value any) error {
	switch v := value.(type) {
	case *ListRunsResponseV1:
		for _, r := range v.Runs {
			if err := r.Validate(); err != nil {
				return err
			}
		}
	case *ListActivityResponseV1:
		for _, a := range v.Activity {
			if err := a.Validate(); err != nil {
				return err
			}
		}
	case *ListInterventionsResponseV1:
		for _, i := range v.Interventions {
			if err := i.Validate(); err != nil {
				return err
			}
		}
	case validator:
		return v.Validate()
	default:
		return fmt.Errorf("fixture type %T has no Validate", value)
	}
	return nil
}

func TestSessionGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "agent session, running", file: "session-v1.json", new: func() any { return new(SessionV1) }},
		{name: "human session, online", file: "session-v1-human.json", new: func() any { return new(SessionV1) }},
		{name: "agent session, ended with a run open", file: "session-v1-offline.json", new: func() any { return new(SessionV1) }},
		{name: "roster, a human and an agent", file: "session-roster-v1.json", new: func() any { return new(SessionRosterV1) }},
		{name: "roster, nobody connected", file: "session-roster-v1-empty.json", new: func() any { return new(SessionRosterV1) }},
		{name: "create request, agent", file: "create-session-request-v1.json", new: func() any { return new(CreateSessionRequestV1) }},
		{name: "create request, human", file: "create-session-request-v1-human.json", new: func() any { return new(CreateSessionRequestV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertP3GoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestSessionSchemaNames(t *testing.T) {
	if SessionSchemaV1 != "conch.session.v1" {
		t.Errorf("SessionSchemaV1 = %q", SessionSchemaV1)
	}
	if SessionRosterSchemaV1 != "conch.session_roster.v1" {
		t.Errorf("SessionRosterSchemaV1 = %q", SessionRosterSchemaV1)
	}
}

// assertErrorCodes checks that each code is the stable wire value clients
// match on and that it survives the Error body unchanged.
func assertErrorCodes(t *testing.T, codes map[string]string) {
	t.Helper()
	for got, want := range codes {
		if got != want {
			t.Errorf("error code = %q, want %q", got, want)
		}
		raw, err := json.Marshal(Error{Code: got, Message: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), `"code":"`+want+`"`) {
			t.Errorf("Error with code %q encodes as %s", got, raw)
		}
	}
}

func TestSessionErrorCodes(t *testing.T) {
	assertErrorCodes(t, map[string]string{
		ErrorCodeSessionNotFound:     "session_not_found",
		ErrorCodeSessionsRequireAuth: "sessions_require_auth",
		ErrorCodeTooManySessions:     "too_many_sessions",
		ErrorCodeAgentRequired:       "agent_required",
	})
}

func TestSessionStateValid(t *testing.T) {
	for _, s := range []SessionState{SessionStateOnline, SessionStateStale, SessionStateOffline} {
		if !s.Valid() {
			t.Errorf("session state %q should be valid", s)
		}
	}
	// Statuses that are not states, other vocabularies' words, case and
	// whitespace variants: none is a state.
	for _, s := range []SessionState{"", "idle", "running", "blocked", "ended", "Online", "ONLINE", " online", "online ", "*", "queued"} {
		if s.Valid() {
			t.Errorf("session state %q should be invalid", s)
		}
	}
}

func TestSessionStatusValid(t *testing.T) {
	for _, s := range []SessionStatus{
		SessionStatusOnline, SessionStatusIdle, SessionStatusRunning, SessionStatusBlocked,
		SessionStatusWaiting, SessionStatusFailed, SessionStatusStale, SessionStatusOffline,
	} {
		if !s.Valid() {
			t.Errorf("session status %q should be valid", s)
		}
	}
	for _, s := range []SessionStatus{"", "queued", "starting", "completed", "cancelled", "busy", "Idle", "RUNNING", " online", "*"} {
		if s.Valid() {
			t.Errorf("session status %q should be invalid", s)
		}
	}
}

var (
	sessionStartedAt  = NewTimestamp(time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC))
	sessionLastSeenAt = NewTimestamp(time.Date(2026, 10, 13, 9, 5, 30, 250_000_000, time.UTC))
)

// fixtureAgentSession is the agent session of session-v1.json: online and
// running run 41, with every agent-only field set.
func fixtureAgentSession() SessionV1 {
	return SessionV1{
		Schema:        SessionSchemaV1,
		ID:            12,
		PrincipalID:   3,
		Kind:          PrincipalAgent,
		Name:          "Leviathan",
		Client:        "conch-adapter/0.3.0",
		State:         SessionStateOnline,
		Status:        SessionStatusRunning,
		StartedAt:     sessionStartedAt,
		LastSeenAt:    sessionLastSeenAt,
		Runtime:       "claude-code",
		Model:         "claude-fable-5-1",
		Host:          "nick-desktop",
		Interventions: []InterventionKind{InterventionKindMessage, InterventionKindCancel},
		CurrentRun:    41,
		Task:          "Answer the question in #ops about the failing deploy",
	}
}

// fixtureHumanSession is the human session of session-v1-human.json.
func fixtureHumanSession() SessionV1 {
	return SessionV1{
		Schema:        SessionSchemaV1,
		ID:            11,
		PrincipalID:   1,
		Kind:          PrincipalHuman,
		Name:          "Nick",
		Client:        "conch-tui/0.9.0",
		State:         SessionStateOnline,
		Status:        SessionStatusOnline,
		StartedAt:     NewTimestamp(time.Date(2026, 10, 13, 8, 58, 12, 0, time.UTC)),
		LastSeenAt:    NewTimestamp(time.Date(2026, 10, 13, 9, 5, 0, 0, time.UTC)),
		Interventions: []InterventionKind{},
	}
}

// fixtureOfflineSession is session-v1-offline.json: an agent session that
// ended with a run still open, so its outcome is unknown.
func fixtureOfflineSession() SessionV1 {
	ended := NewTimestamp(time.Date(2026, 10, 12, 22, 41, 30, 0, time.UTC))
	return SessionV1{
		Schema:         SessionSchemaV1,
		ID:             9,
		PrincipalID:    3,
		Kind:           PrincipalAgent,
		Name:           "Leviathan",
		Client:         "conch-adapter/0.3.0",
		State:          SessionStateOffline,
		Status:         SessionStatusOffline,
		StartedAt:      NewTimestamp(time.Date(2026, 10, 12, 22, 0, 0, 0, time.UTC)),
		LastSeenAt:     NewTimestamp(time.Date(2026, 10, 12, 22, 40, 0, 0, time.UTC)),
		EndedAt:        &ended,
		Runtime:        "claude-code",
		Model:          "claude-fable-5-1",
		Host:           "nick-desktop",
		Interventions:  []InterventionKind{InterventionKindMessage, InterventionKindCancel},
		OutcomeUnknown: true,
	}
}

func fixtureSessionRoster() SessionRosterV1 {
	return SessionRosterV1{
		Schema:    SessionRosterSchemaV1,
		ChannelID: 7,
		Sessions:  []SessionV1{fixtureHumanSession(), fixtureAgentSession()},
	}
}

func fixtureCreateSessionRequest() CreateSessionRequestV1 {
	return CreateSessionRequestV1{
		Client:        "conch-adapter/0.3.0",
		Runtime:       "claude-code",
		Model:         "claude-fable-5-1",
		Host:          "nick-desktop",
		Interventions: []InterventionKind{InterventionKindMessage, InterventionKindCancel},
	}
}

func TestSessionV1Validate(t *testing.T) {
	earlier := NewTimestamp(sessionStartedAt.Time().Add(-time.Second))
	later := NewTimestamp(sessionLastSeenAt.Time().Add(time.Minute))
	tests := []struct {
		name    string
		mutate  func(*SessionV1)
		wantErr string
	}{
		{name: "valid agent, running", mutate: func(*SessionV1) {}},
		{name: "valid agent, idle with no run", mutate: func(s *SessionV1) { s.Status, s.CurrentRun, s.Task = SessionStatusIdle, 0, "" }},
		{name: "valid agent, blocked", mutate: func(s *SessionV1) { s.Status = SessionStatusBlocked }},
		{name: "valid agent, waiting", mutate: func(s *SessionV1) { s.Status = SessionStatusWaiting }},
		{name: "valid agent, failed", mutate: func(s *SessionV1) { s.Status, s.CurrentRun, s.Task = SessionStatusFailed, 0, "" }},
		{
			// The reader may not see the run: status is shown, the run is not.
			name:   "valid agent, running with the run hidden",
			mutate: func(s *SessionV1) { s.CurrentRun, s.Task = 0, "" },
		},
		{name: "valid agent, stale", mutate: func(s *SessionV1) { s.State, s.Status = SessionStateStale, SessionStatusStale }},
		{name: "valid agent, stale with outcome unknown", mutate: func(s *SessionV1) { s.State, s.Status, s.OutcomeUnknown = SessionStateStale, SessionStatusStale, true }},
		{name: "valid agent, offline without ended_at", mutate: func(s *SessionV1) { s.State, s.Status = SessionStateOffline, SessionStatusOffline }},
		{
			// Offline but not ended: a heartbeat may still revive it, and
			// meanwhile its open run's outcome is unknown.
			name: "valid agent, offline with outcome unknown and no ended_at",
			mutate: func(s *SessionV1) {
				s.State, s.Status, s.OutcomeUnknown = SessionStateOffline, SessionStatusOffline, true
			},
		},
		{name: "valid agent, offline and ended", mutate: func(s *SessionV1) { *s = fixtureOfflineSession() }},
		{name: "valid agent, no labels", mutate: func(s *SessionV1) { s.Runtime, s.Model, s.Host = "", "", "" }},
		{name: "valid agent, declares no interventions", mutate: func(s *SessionV1) { s.Interventions = nil }},
		{name: "valid agent, declares every intervention", mutate: func(s *SessionV1) {
			s.Interventions = []InterventionKind{InterventionKindMessage, InterventionKindCancel, InterventionKindPause, InterventionKindResume}
		}},
		{name: "valid agent, last_seen_at equals started_at", mutate: func(s *SessionV1) { s.LastSeenAt = s.StartedAt }},
		{name: "valid human", mutate: func(s *SessionV1) { *s = fixtureHumanSession() }},
		{name: "valid human, nil interventions", mutate: func(s *SessionV1) { *s = fixtureHumanSession(); s.Interventions = nil }},
		{name: "valid human, stale", mutate: func(s *SessionV1) {
			*s = fixtureHumanSession()
			s.State, s.Status = SessionStateStale, SessionStatusStale
		}},
		{name: "valid human, offline", mutate: func(s *SessionV1) {
			*s = fixtureHumanSession()
			s.State, s.Status = SessionStateOffline, SessionStatusOffline
		}},
		{name: "valid client at the limit", mutate: func(s *SessionV1) { s.Client = strings.Repeat("c", MaxSessionClientLength) }},
		{name: "valid runtime at the limit", mutate: func(s *SessionV1) { s.Runtime = strings.Repeat("r", MaxSessionLabelLength) }},
		{name: "valid model at the limit", mutate: func(s *SessionV1) { s.Model = strings.Repeat("m", MaxSessionLabelLength) }},
		{name: "valid host at the limit", mutate: func(s *SessionV1) { s.Host = strings.Repeat("h", MaxSessionLabelLength) }},
		{name: "valid host at the limit in multibyte characters", mutate: func(s *SessionV1) { s.Host = strings.Repeat("é", MaxSessionLabelLength) }},
		{name: "valid task at the limit", mutate: func(s *SessionV1) { s.Task = strings.Repeat("t", MaxRunTaskLength) }},
		{name: "valid task with newlines", mutate: func(s *SessionV1) { s.Task = "first line\nsecond line" }},

		{name: "empty schema", mutate: func(s *SessionV1) { s.Schema = "" }, wantErr: `session schema must be "conch.session.v1", got ""`},
		{name: "roster schema", mutate: func(s *SessionV1) { s.Schema = SessionRosterSchemaV1 }, wantErr: `got "conch.session_roster.v1"`},
		{name: "future schema version", mutate: func(s *SessionV1) { s.Schema = "conch.session.v2" }, wantErr: `got "conch.session.v2"`},
		{name: "zero id", mutate: func(s *SessionV1) { s.ID = 0 }, wantErr: "session id must be positive, got 0"},
		{name: "negative id", mutate: func(s *SessionV1) { s.ID = -12 }, wantErr: "session id must be positive, got -12"},
		{name: "zero principal", mutate: func(s *SessionV1) { s.PrincipalID = 0 }, wantErr: "session principal_id must be positive, got 0"},
		{name: "empty kind", mutate: func(s *SessionV1) { s.Kind = "" }, wantErr: `session kind "" is not one of human, agent`},
		{name: "unknown kind", mutate: func(s *SessionV1) { s.Kind = "bot" }, wantErr: `session kind "bot" is not one of human, agent`},
		{name: "upper-case kind", mutate: func(s *SessionV1) { s.Kind = "Agent" }, wantErr: `session kind "Agent" is not one of human, agent`},
		{name: "empty name", mutate: func(s *SessionV1) { s.Name = "" }, wantErr: "session name is required"},
		{name: "blank name", mutate: func(s *SessionV1) { s.Name = " \t" }, wantErr: "session name is required"},
		{name: "empty client", mutate: func(s *SessionV1) { s.Client = "" }, wantErr: "session client is required"},
		{name: "blank client", mutate: func(s *SessionV1) { s.Client = "  " }, wantErr: "session client is required"},
		{name: "client one over the limit", mutate: func(s *SessionV1) { s.Client = strings.Repeat("c", MaxSessionClientLength+1) }, wantErr: "session client must be at most 64 characters, got 65"},
		{name: "client with a control character", mutate: func(s *SessionV1) { s.Client = "conch\x1b[31m" }, wantErr: "session client must not contain control characters"},
		{name: "runtime one over the limit", mutate: func(s *SessionV1) { s.Runtime = strings.Repeat("r", MaxSessionLabelLength+1) }, wantErr: "session runtime must be at most 128 characters, got 129"},
		{name: "model one over the limit", mutate: func(s *SessionV1) { s.Model = strings.Repeat("m", MaxSessionLabelLength+1) }, wantErr: "session model must be at most 128 characters, got 129"},
		{name: "host one over the limit", mutate: func(s *SessionV1) { s.Host = strings.Repeat("h", MaxSessionLabelLength+1) }, wantErr: "session host must be at most 128 characters, got 129"},
		{name: "host with a bidi override", mutate: func(s *SessionV1) { s.Host = "host\u202etsoh" }, wantErr: "session host must not contain control characters, line or paragraph separators, or bidi controls"},
		{name: "model with trailing whitespace", mutate: func(s *SessionV1) { s.Model = "claude " }, wantErr: "session model must not begin or end with whitespace"},

		{name: "empty state", mutate: func(s *SessionV1) { s.State = "" }, wantErr: `session state "" is not one of online, stale, offline`},
		{name: "unknown state", mutate: func(s *SessionV1) { s.State = "ended" }, wantErr: `session state "ended" is not one of online, stale, offline`},
		{name: "status is not a state", mutate: func(s *SessionV1) { s.State = "running" }, wantErr: `session state "running" is not one of online, stale, offline`},
		{name: "empty status", mutate: func(s *SessionV1) { s.Status = "" }, wantErr: `session status "" is not one of online, idle, running, blocked, waiting, failed, stale, offline`},
		{name: "unknown status", mutate: func(s *SessionV1) { s.Status = "busy" }, wantErr: `session status "busy" is not one of`},
		{name: "run state is not a status", mutate: func(s *SessionV1) { s.Status = "queued" }, wantErr: `session status "queued" is not one of`},
		{name: "online with status stale", mutate: func(s *SessionV1) { s.Status = SessionStatusStale }, wantErr: `online session must not have status "stale"`},
		{name: "online with status offline", mutate: func(s *SessionV1) { s.Status = SessionStatusOffline }, wantErr: `online session must not have status "offline"`},
		{name: "stale with status running", mutate: func(s *SessionV1) { s.State = SessionStateStale }, wantErr: `stale session must have status "stale", got "running"`},
		{name: "stale with status offline", mutate: func(s *SessionV1) { s.State, s.Status = SessionStateStale, SessionStatusOffline }, wantErr: `stale session must have status "stale", got "offline"`},
		{name: "offline with status running", mutate: func(s *SessionV1) { s.State = SessionStateOffline }, wantErr: `offline session must have status "offline", got "running"`},
		{name: "offline with status stale", mutate: func(s *SessionV1) { s.State, s.Status = SessionStateOffline, SessionStatusStale }, wantErr: `offline session must have status "offline", got "stale"`},
		// An online session's runs are being reported; their outcome is not
		// unknown.
		{name: "online with outcome unknown", mutate: func(s *SessionV1) { s.OutcomeUnknown = true }, wantErr: "online session must not report outcome_unknown"},
		{name: "online idle with outcome unknown", mutate: func(s *SessionV1) { s.Status, s.CurrentRun, s.Task, s.OutcomeUnknown = SessionStatusIdle, 0, "", true }, wantErr: "online session must not report outcome_unknown"},

		{name: "zero started_at", mutate: func(s *SessionV1) { s.StartedAt = Timestamp{} }, wantErr: "session started_at is required"},
		{name: "zero last_seen_at", mutate: func(s *SessionV1) { s.LastSeenAt = Timestamp{} }, wantErr: "session last_seen_at is required"},
		{name: "last_seen_at before started_at", mutate: func(s *SessionV1) { s.LastSeenAt = earlier }, wantErr: "session last_seen_at must not be before started_at"},
		{name: "zero ended_at", mutate: func(s *SessionV1) {
			s.State, s.Status, s.EndedAt = SessionStateOffline, SessionStatusOffline, &Timestamp{}
		}, wantErr: "session ended_at must be omitted or set"},
		{name: "ended_at before started_at", mutate: func(s *SessionV1) { s.State, s.Status, s.EndedAt = SessionStateOffline, SessionStatusOffline, &earlier }, wantErr: "session ended_at must not be before started_at"},
		{name: "ended but online", mutate: func(s *SessionV1) { s.EndedAt = &later }, wantErr: `session with ended_at must be offline, got state "online"`},
		{name: "ended but stale", mutate: func(s *SessionV1) { s.State, s.Status, s.EndedAt = SessionStateStale, SessionStatusStale, &later }, wantErr: `session with ended_at must be offline, got state "stale"`},

		{name: "unknown intervention kind", mutate: func(s *SessionV1) { s.Interventions = []InterventionKind{"restart"} }, wantErr: `intervention kind "restart" is not one of message, cancel, pause, resume`},
		{name: "empty intervention kind", mutate: func(s *SessionV1) { s.Interventions = []InterventionKind{""} }, wantErr: `intervention kind "" is not one of message, cancel, pause, resume`},
		{name: "duplicate intervention kind", mutate: func(s *SessionV1) {
			s.Interventions = []InterventionKind{InterventionKindCancel, InterventionKindCancel}
		}, wantErr: `interventions lists kind "cancel" more than once`},

		{name: "negative current_run", mutate: func(s *SessionV1) { s.CurrentRun = -1 }, wantErr: "session current_run must be omitted or positive, got -1"},
		{name: "task without current_run", mutate: func(s *SessionV1) { s.CurrentRun = 0 }, wantErr: "session current_run and task must be present together or omitted together"},
		{name: "current_run without task", mutate: func(s *SessionV1) { s.Task = "" }, wantErr: "session current_run and task must be present together or omitted together"},
		{name: "task one over the limit", mutate: func(s *SessionV1) { s.Task = strings.Repeat("t", MaxRunTaskLength+1) }, wantErr: "session task must be at most 512 characters, got 513"},
		{name: "task not UTF-8", mutate: func(s *SessionV1) { s.Task = "bad \xff byte" }, wantErr: "session task must be valid UTF-8"},

		// Agent-only fields on a human session (acceptance criterion).
		{name: "human with runtime", mutate: func(s *SessionV1) { *s = fixtureHumanSession(); s.Runtime = "claude-code" }, wantErr: "human session must not carry runtime, model or host"},
		{name: "human with model", mutate: func(s *SessionV1) { *s = fixtureHumanSession(); s.Model = "claude-fable-5-1" }, wantErr: "human session must not carry runtime, model or host"},
		{name: "human with host", mutate: func(s *SessionV1) { *s = fixtureHumanSession(); s.Host = "nick-desktop" }, wantErr: "human session must not carry runtime, model or host"},
		{name: "human with interventions", mutate: func(s *SessionV1) {
			*s = fixtureHumanSession()
			s.Interventions = []InterventionKind{InterventionKindMessage}
		}, wantErr: "human session must not declare interventions"},
		{name: "human with a current run", mutate: func(s *SessionV1) { *s = fixtureHumanSession(); s.CurrentRun, s.Task = 41, "work" }, wantErr: "human session must not carry current_run or task"},
		{name: "human with outcome unknown", mutate: func(s *SessionV1) {
			// Stale, so the online rule does not fire first: a human has
			// no runs whose outcome could be unknown.
			*s = fixtureHumanSession()
			s.State, s.Status, s.OutcomeUnknown = SessionStateStale, SessionStatusStale, true
		}, wantErr: "human session must not report outcome_unknown"},
		{name: "human with status idle", mutate: func(s *SessionV1) { *s = fixtureHumanSession(); s.Status = SessionStatusIdle }, wantErr: `human session status must be one of online, stale, offline, got "idle"`},
		{name: "human with status running", mutate: func(s *SessionV1) { *s = fixtureHumanSession(); s.Status = SessionStatusRunning }, wantErr: `human session status must be one of online, stale, offline, got "running"`},
		{name: "human with status failed", mutate: func(s *SessionV1) { *s = fixtureHumanSession(); s.Status = SessionStatusFailed }, wantErr: `human session status must be one of online, stale, offline, got "failed"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := fixtureAgentSession()
			tt.mutate(&s)
			assertValidation(t, s.Validate(), tt.wantErr)
		})
	}
}

func TestSessionRosterV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SessionRosterV1)
		wantErr string
	}{
		{name: "valid, a human and an agent", mutate: func(*SessionRosterV1) {}},
		{name: "valid, nil sessions", mutate: func(r *SessionRosterV1) { r.Sessions = nil }},
		{name: "valid, empty sessions", mutate: func(r *SessionRosterV1) { r.Sessions = []SessionV1{} }},
		{
			// One principal on two clients is two sessions.
			name: "valid, same principal twice",
			mutate: func(r *SessionRosterV1) {
				second := fixtureAgentSession()
				second.ID = 13
				r.Sessions = append(r.Sessions, second)
			},
		},

		{name: "empty schema", mutate: func(r *SessionRosterV1) { r.Schema = "" }, wantErr: `session roster schema must be "conch.session_roster.v1", got ""`},
		{name: "session schema", mutate: func(r *SessionRosterV1) { r.Schema = SessionSchemaV1 }, wantErr: `got "conch.session.v1"`},
		{name: "zero channel_id", mutate: func(r *SessionRosterV1) { r.ChannelID = 0 }, wantErr: "session roster channel_id must be positive, got 0"},
		{name: "negative channel_id", mutate: func(r *SessionRosterV1) { r.ChannelID = -7 }, wantErr: "session roster channel_id must be positive, got -7"},
		{name: "entry without schema", mutate: func(r *SessionRosterV1) { r.Sessions[0].Schema = "" }, wantErr: `session schema must be "conch.session.v1", got ""`},
		{name: "entry with unknown state", mutate: func(r *SessionRosterV1) { r.Sessions[1].State = "ended" }, wantErr: `session state "ended" is not one of`},
		{name: "human entry with runtime", mutate: func(r *SessionRosterV1) { r.Sessions[0].Runtime = "x" }, wantErr: "human session must not carry runtime, model or host"},
		{name: "duplicate session id", mutate: func(r *SessionRosterV1) { r.Sessions[1].ID = r.Sessions[0].ID }, wantErr: "session roster lists session 11 more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fixtureSessionRoster()
			tt.mutate(&r)
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

func TestCreateSessionRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CreateSessionRequestV1)
		wantErr string
	}{
		{name: "valid agent request", mutate: func(*CreateSessionRequestV1) {}},
		{name: "valid human request: client only", mutate: func(r *CreateSessionRequestV1) { *r = CreateSessionRequestV1{Client: "conch-tui/0.9.0"} }},
		{name: "valid, empty interventions", mutate: func(r *CreateSessionRequestV1) { r.Interventions = []InterventionKind{} }},
		{name: "valid client at the limit", mutate: func(r *CreateSessionRequestV1) { r.Client = strings.Repeat("c", MaxSessionClientLength) }},
		{name: "valid runtime at the limit", mutate: func(r *CreateSessionRequestV1) { r.Runtime = strings.Repeat("r", MaxSessionLabelLength) }},

		{name: "empty client", mutate: func(r *CreateSessionRequestV1) { r.Client = "" }, wantErr: "session client is required"},
		{name: "client one over the limit", mutate: func(r *CreateSessionRequestV1) { r.Client = strings.Repeat("c", MaxSessionClientLength+1) }, wantErr: "session client must be at most 64 characters, got 65"},
		{name: "runtime one over the limit", mutate: func(r *CreateSessionRequestV1) { r.Runtime = strings.Repeat("r", MaxSessionLabelLength+1) }, wantErr: "session runtime must be at most 128 characters, got 129"},
		{name: "model one over the limit", mutate: func(r *CreateSessionRequestV1) { r.Model = strings.Repeat("m", MaxSessionLabelLength+1) }, wantErr: "session model must be at most 128 characters, got 129"},
		{name: "host one over the limit", mutate: func(r *CreateSessionRequestV1) { r.Host = strings.Repeat("h", MaxSessionLabelLength+1) }, wantErr: "session host must be at most 128 characters, got 129"},
		{name: "host with a control character", mutate: func(r *CreateSessionRequestV1) { r.Host = "a\nb" }, wantErr: "session host must not contain control characters"},
		{name: "unknown intervention kind", mutate: func(r *CreateSessionRequestV1) { r.Interventions = []InterventionKind{"kill"} }, wantErr: `intervention kind "kill" is not one of message, cancel, pause, resume`},
		{name: "duplicate intervention kind", mutate: func(r *CreateSessionRequestV1) { r.Interventions = append(r.Interventions, InterventionKindMessage) }, wantErr: `interventions lists kind "message" more than once`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := fixtureCreateSessionRequest()
			tt.mutate(&r)
			assertValidation(t, r.Validate(), tt.wantErr)
		})
	}
}

// Forward compatibility: a v1 reader must accept a document from a newer
// server that added an optional field, and still validate it.
func TestSessionUnknownFieldsTolerated(t *testing.T) {
	raw := `{"schema":"conch.session.v1","id":12,"principal_id":3,"kind":"agent","name":"Leviathan","client":"conch-adapter/0.3.0",` +
		`"state":"online","status":"idle","started_at":"2026-10-13T09:00:00.000Z","last_seen_at":"2026-10-13T09:05:30.250Z",` +
		`"interventions":[],"outcome_unknown":false,"heartbeat_interval_ms":15000,"labels":{"team":"ops"}}`
	var s SessionV1
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
	if s.ID != 12 || s.Status != SessionStatusIdle {
		t.Errorf("decoded session lost known fields: %+v", s)
	}
}

func TestSessionEncoding(t *testing.T) {
	// A human session spells out its empty interventions and its false
	// outcome_unknown: a reader must not have to know what an omitted field
	// means, and a roster with nobody in it is an empty array.
	raw, err := json.Marshal(fixtureHumanSession())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"interventions":[]`, `"outcome_unknown":false`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("human session = %s, want it to contain %s", raw, want)
		}
	}
	for _, absent := range []string{`"runtime"`, `"model"`, `"host"`, `"current_run"`, `"task"`, `"ended_at"`} {
		if strings.Contains(string(raw), absent) {
			t.Errorf("human session = %s, want no %s key", raw, absent)
		}
	}
	raw, err = json.Marshal(SessionRosterV1{Schema: SessionRosterSchemaV1, ChannelID: 7, Sessions: []SessionV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"sessions":[]`) {
		t.Errorf("empty roster = %s, want sessions:[]", raw)
	}
}
