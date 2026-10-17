package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// A runtime outside Conch reporting over the public REST contract (issue
// #386, docs/design/webhook-adapter.md §3). The sender is a CI-style job: an
// agent principal with a dedicated credential, a minimal manifest and nothing
// but HTTP. Every document the server sends is run through its Validate, and
// the test touches no handler, store call or route a real sender could not.

const externalTask = "nightly integration build 7421"

// extFixture is the session fixture plus the sender: ci, an agent in general
// whose manifest grants sessions.manage and runs.manage with a rate limit on
// each, and post in general; and "private", a channel the sender is not in.
type extFixture struct {
	*sessionFixture
	ci      store.Principal
	ciTok   string
	private store.Channel
}

func newExtFixture(t *testing.T) *extFixture {
	t.Helper()
	ctx := context.Background()
	f := &extFixture{sessionFixture: newSessionFixture(t)}
	var err error
	if f.ci, err = f.srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "ci"); err != nil {
		t.Fatal(err)
	}
	if _, f.ciTok, err = f.srv.store.CreateCredential(ctx, "system", f.ci.ID, "ci job", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.general.ID, f.ci.ID, 0); err != nil {
		t.Fatal(err)
	}
	if f.private, err = f.srv.store.CreateChannel(ctx, "private"); err != nil {
		t.Fatal(err)
	}
	f.manifest(t, []schema.Capability{schema.CapabilitySessionsManage, schema.CapabilityRunsManage},
		[]schema.RateLimit{rl(schema.CapabilitySessionsManage, 100, 60), rl(schema.CapabilityRunsManage, 100, 60)},
		f.general.ID, f.private.ID)
	return f
}

// manifest replaces the sender's manifest: the capabilities, the limits, and
// post alone in each channel.
func (f *extFixture) manifest(t *testing.T, caps []schema.Capability, limits []schema.RateLimit, channels ...int64) {
	t.Helper()
	req := schema.PutAgentManifestRequestV1{DisplayName: "ci", Tier: schema.AgentTierC, Capabilities: caps, RateLimits: limits}
	for _, id := range channels {
		req.Channels = append(req.Channels, schema.ChannelGrant{ChannelID: id, Permissions: []schema.ChannelPermission{schema.ChannelPermissionPost}})
	}
	if _, _, err := f.srv.store.PutAgentManifest(context.Background(), "system", f.ci.ID, req); err != nil {
		t.Fatal(err)
	}
}

// send marshals doc with the schema type's own encoding.
func send(t *testing.T, doc any) string {
	t.Helper()
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// startRun registers a session declaring the given interventions and starts a
// run in general as the sender.
func (f *extFixture) startRun(t *testing.T, kinds ...schema.InterventionKind) (schema.SessionV1, schema.RunV1) {
	t.Helper()
	sess := f.create(t, f.ciTok, send(t, schema.CreateSessionRequestV1{Client: "ci-job", Runtime: "github-actions", Host: "runner-3", Interventions: kinds}))
	rec := f.do(t, "POST", sessionPath(sess.ID, "/runs"), f.ciTok, send(t, schema.CreateRunRequestV1{Channel: "general", Task: externalTask}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start run = %d %s", rec.Code, rec.Body)
	}
	return sess, validRun(t, rec)
}

func TestExternalRuntimeHappyPath(t *testing.T) {
	f := newExtFixture(t)
	base := wsTestServer(t, f.srv)
	alice := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
	if first := alice.next(); first.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("first frame = %v, want the roster", first)
	}

	var (
		sess schema.SessionV1
		run  schema.RunV1
		iv   schema.InterventionV1
	)
	// Each step is one call a sender makes, in the order docs/manual.md gives.
	steps := []struct {
		name   string
		token  string
		method string
		path   func() string
		body   func() string
		want   int
		check  func(t *testing.T, rec *httptest.ResponseRecorder)
	}{
		{
			name: "register", token: f.ciTok, method: "POST",
			path: func() string { return "/v1/sessions" },
			body: func() string {
				return send(t, schema.CreateSessionRequestV1{Client: "ci-job", Runtime: "github-actions", Host: "runner-3", Interventions: []schema.InterventionKind{schema.InterventionKindCancel}})
			},
			want:  http.StatusCreated,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) { sess = validSession(t, rec) },
		},
		{
			name: "heartbeat", token: f.ciTok, method: "POST",
			path: func() string { return sessionPath(sess.ID, "/heartbeat") },
			want: http.StatusOK, check: func(t *testing.T, rec *httptest.ResponseRecorder) { validSession(t, rec) },
		},
		{
			name: "start run", token: f.ciTok, method: "POST",
			path: func() string { return sessionPath(sess.ID, "/runs") },
			body: func() string {
				return send(t, schema.CreateRunRequestV1{Channel: "general", Task: externalTask, Link: "https://ci.example.com/jobs/7421"})
			},
			want: http.StatusCreated, check: func(t *testing.T, rec *httptest.ResponseRecorder) { run = validRun(t, rec) },
		},
		{
			name: "state starting", token: f.ciTok, method: "POST",
			path: func() string { return runPath(run.ID, "/state") },
			body: func() string { return send(t, schema.SetRunStateRequestV1{State: schema.RunStateStarting}) },
			want: http.StatusOK, check: func(t *testing.T, rec *httptest.ResponseRecorder) { validRun(t, rec) },
		},
		{
			name: "state running", token: f.ciTok, method: "POST",
			path: func() string { return runPath(run.ID, "/state") },
			body: func() string { return send(t, schema.SetRunStateRequestV1{State: schema.RunStateRunning}) },
			want: http.StatusOK, check: func(t *testing.T, rec *httptest.ResponseRecorder) { validRun(t, rec) },
		},
		{
			name: "activity: a step starts", token: f.ciTok, method: "POST",
			path: func() string { return runPath(run.ID, "/activity") },
			body: func() string {
				return send(t, schema.AppendActivityRequestV1{Kind: schema.ActivityKindCommandStarted, Summary: "go test ./..."})
			},
			want: http.StatusCreated, check: func(t *testing.T, rec *httptest.ResponseRecorder) { validActivity(t, rec) },
		},
		{
			name: "activity: the step ends", token: f.ciTok, method: "POST",
			path: func() string { return runPath(run.ID, "/activity") },
			body: func() string {
				return send(t, schema.AppendActivityRequestV1{Kind: schema.ActivityKindCommandFinished, Summary: "go test ./...", Outcome: "passed", Data: json.RawMessage(`{"exit":0}`)})
			},
			want: http.StatusCreated, check: func(t *testing.T, rec *httptest.ResponseRecorder) { validActivity(t, rec) },
		},
		{
			name: "a person asks for a cancel", token: f.aliceTok, method: "POST",
			path: func() string { return runInterventionsPath(run.ID) },
			body: func() string { return askBody(schema.InterventionKindCancel, "") },
			want: http.StatusCreated, check: func(t *testing.T, rec *httptest.ResponseRecorder) { iv = validIntervention(t, rec) },
		},
		{
			name: "poll interventions", token: f.ciTok, method: "GET",
			path: func() string { return sessionPath(sess.ID, "/interventions?state=requested") },
			want: http.StatusOK,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if list := validInterventionList(t, rec); len(list.Interventions) != 1 || list.Interventions[0].ID != iv.ID {
					t.Errorf("polled %+v, want intervention %d", list, iv.ID)
				}
			},
		},
		{
			name: "ack the cancel", token: f.ciTok, method: "POST",
			path: func() string { return interventionPath(iv.ID, "/ack") },
			body: func() string { return ackBody(schema.InterventionStateApplied, "") },
			want: http.StatusOK, check: func(t *testing.T, rec *httptest.ResponseRecorder) { validIntervention(t, rec) },
		},
		{
			name: "activity: the run ends", token: f.ciTok, method: "POST",
			path: func() string { return runPath(run.ID, "/activity") },
			body: func() string {
				return send(t, schema.AppendActivityRequestV1{Kind: schema.ActivityKindRunEnded, Summary: "run ended", Outcome: "cancelled"})
			},
			want: http.StatusCreated, check: func(t *testing.T, rec *httptest.ResponseRecorder) { validActivity(t, rec) },
		},
		{
			name: "state cancelled", token: f.ciTok, method: "POST",
			path: func() string { return runPath(run.ID, "/state") },
			body: func() string {
				return send(t, schema.SetRunStateRequestV1{State: schema.RunStateCancelled, Outcome: "cancelled by request"})
			},
			want: http.StatusOK,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if got := validRun(t, rec); got.State != schema.RunStateCancelled || got.EndedAt == nil {
					t.Errorf("run = %+v, want cancelled and ended", got)
				}
			},
		},
		{
			name: "end the session", token: f.ciTok, method: "DELETE",
			path: func() string { return sessionPath(sess.ID, "") },
			want: http.StatusNoContent,
		},
	}
	for _, st := range steps {
		body := ""
		if st.body != nil {
			body = st.body()
		}
		rec := f.do(t, st.method, st.path(), st.token, body)
		if rec.Code != st.want {
			t.Fatalf("%s = %d %s, want %d", st.name, rec.Code, rec.Body, st.want)
		}
		if st.check != nil {
			st.check(t, rec)
		}
	}

	// A member's socket was sent the session, the run and the activity. Runs
	// that change state twice within one tick may arrive as the later state
	// only, so the early states are not asked for.
	seen := alice.until("the run's end", isRun(func(r schema.RunV1) bool { return r.ID == run.ID && r.State == schema.RunStateCancelled }))
	for _, w := range []struct {
		what  string
		match func(eventFrame) bool
	}{
		{"the session in a roster", isRoster(rosterHas(f.ci.ID, schema.SessionStatusRunning))},
		{"the run running", isRun(func(r schema.RunV1) bool { return r.ID == run.ID && r.State == schema.RunStateRunning })},
		{"an activity entry", func(fr eventFrame) bool {
			return fr.schema == schema.AgentActivitySchemaV1 && fr.activity.Kind == schema.ActivityKindCommandFinished
		}},
	} {
		if !hasFrame(seen, w.match) {
			t.Errorf("no frame for %s in %v", w.what, seen)
		}
	}

	// The audit trail has the four events in order, and none of its rows
	// holds the task.
	var got []string
	for _, e := range f.audit(t) {
		switch e.Action {
		case store.AuditSessionStarted, store.AuditRunStarted, store.AuditRunEnded, store.AuditSessionEnded:
			got = append(got, e.Action)
		}
		if strings.Contains(e.Detail, externalTask) || strings.Contains(e.Subject, externalTask) {
			t.Errorf("audit row %s holds the task: %+v", e.Action, e)
		}
	}
	want := []string{store.AuditSessionStarted, store.AuditRunStarted, store.AuditRunEnded, store.AuditSessionEnded}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("audit actions = %v, want %v", got, want)
	}
}

func TestExternalRuntimeRefusals(t *testing.T) {
	denied := func(t *testing.T, f *extFixture, reason string) {
		t.Helper()
		for _, e := range f.auditRows(t, "access_denied") {
			if e.Actor == fmt.Sprintf("principal:%d", f.ci.ID) && strings.Contains(e.Detail, "reason="+reason) {
				return
			}
		}
		t.Errorf("no access_denied row with reason %s: %+v", reason, f.audit(t))
	}
	bigTask := strings.Repeat("x", schema.MaxPostRequestBytes)
	tests := []struct {
		name string
		run  func(t *testing.T, f *extFixture)
	}{
		{"a wrong credential", func(t *testing.T, f *extFixture) {
			assertErrorBody(t, f.do(t, "POST", "/v1/sessions", "conch_not_a_credential", `{"client":"ci-job"}`), http.StatusUnauthorized, "unauthenticated")
		}},
		{"a revoked credential", func(t *testing.T, f *extFixture) {
			sess := f.create(t, f.ciTok, `{"client":"ci-job"}`)
			if err := f.srv.store.RevokeCredential(context.Background(), "system", f.credentialID(t, f.ciTok)); err != nil {
				t.Fatal(err)
			}
			assertErrorBody(t, f.do(t, "POST", "/v1/sessions", f.ciTok, `{"client":"ci-job"}`), http.StatusUnauthorized, "unauthenticated")
			assertErrorBody(t, f.do(t, "POST", sessionPath(sess.ID, "/heartbeat"), f.ciTok, ""), http.StatusUnauthorized, "unauthenticated")
		}},
		{"a manifest without runs.manage: start is channel_not_found", func(t *testing.T, f *extFixture) {
			f.manifest(t, []schema.Capability{schema.CapabilitySessionsManage}, nil, f.general.ID)
			sess := f.create(t, f.ciTok, `{"client":"ci-job"}`)
			rec := f.do(t, "POST", sessionPath(sess.ID, "/runs"), f.ciTok, send(t, schema.CreateRunRequestV1{Channel: "general", Task: externalTask}))
			assertErrorBody(t, rec, http.StatusNotFound, "channel_not_found")
			denied(t, f, denyCapability)
		}},
		{"a manifest that lost runs.manage: a report is forbidden", func(t *testing.T, f *extFixture) {
			_, run := f.startRun(t)
			f.manifest(t, []schema.Capability{schema.CapabilitySessionsManage}, nil, f.general.ID)
			assertErrorBody(t, f.do(t, "POST", runPath(run.ID, "/state"), f.ciTok, send(t, schema.SetRunStateRequestV1{State: schema.RunStateRunning})), http.StatusForbidden, "forbidden")
			assertErrorBody(t, f.do(t, "POST", runPath(run.ID, "/activity"), f.ciTok, send(t, schema.AppendActivityRequestV1{Kind: schema.ActivityKindRunStarted})), http.StatusForbidden, "forbidden")
			denied(t, f, denyCapability)
		}},
		{"a channel the sender is not a member of", func(t *testing.T, f *extFixture) {
			sess := f.create(t, f.ciTok, `{"client":"ci-job"}`)
			rec := f.do(t, "POST", sessionPath(sess.ID, "/runs"), f.ciTok, send(t, schema.CreateRunRequestV1{Channel: "private", Task: externalTask}))
			assertErrorBody(t, rec, http.StatusNotFound, "channel_not_found")
			denied(t, f, denyNotMember)
		}},
		{"a body over 1 MiB", func(t *testing.T, f *extFixture) {
			sess := f.create(t, f.ciTok, `{"client":"ci-job"}`)
			rec := f.do(t, "POST", sessionPath(sess.ID, "/runs"), f.ciTok, `{"channel":"general","task":"`+bigTask+`"}`)
			assertErrorBody(t, rec, http.StatusRequestEntityTooLarge, "request_too_large")
		}},
		{"malformed JSON", func(t *testing.T, f *extFixture) {
			_, run := f.startRun(t)
			assertErrorBody(t, f.do(t, "POST", runPath(run.ID, "/state"), f.ciTok, `{"state":`), http.StatusBadRequest, "invalid_request")
		}},
		{"an unknown field", func(t *testing.T, f *extFixture) {
			_, run := f.startRun(t)
			assertErrorBody(t, f.do(t, "POST", runPath(run.ID, "/state"), f.ciTok, `{"state":"running","at":"2026-01-01T00:00:00Z"}`), http.StatusBadRequest, "invalid_request")
		}},
		{"activity data over 8 KiB", func(t *testing.T, f *extFixture) {
			_, run := f.startRun(t)
			data := `{"log":"` + strings.Repeat("x", schema.MaxActivityDataBytes) + `"}`
			assertErrorBody(t, f.do(t, "POST", runPath(run.ID, "/activity"), f.ciTok, `{"kind":"command_finished","data":`+data+`}`), http.StatusBadRequest, "invalid_request")
		}},
		{"an http:// link", func(t *testing.T, f *extFixture) {
			sess := f.create(t, f.ciTok, `{"client":"ci-job"}`)
			rec := f.do(t, "POST", sessionPath(sess.ID, "/runs"), f.ciTok, send(t, schema.CreateRunRequestV1{Channel: "general", Task: externalTask, Link: "http://ci.example.com/jobs/1"}))
			assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")
		}},
		{"rate limited", func(t *testing.T, f *extFixture) {
			f.manifest(t, []schema.Capability{schema.CapabilitySessionsManage, schema.CapabilityRunsManage},
				[]schema.RateLimit{rl(schema.CapabilityRunsManage, 2, 60)}, f.general.ID)
			_, run := f.startRun(t) // use 1
			running := send(t, schema.SetRunStateRequestV1{State: schema.RunStateRunning})
			if rec := f.do(t, "POST", runPath(run.ID, "/state"), f.ciTok, running); rec.Code != http.StatusOK { // use 2
				t.Fatalf("state = %d %s", rec.Code, rec.Body)
			}
			rec := f.do(t, "POST", runPath(run.ID, "/activity"), f.ciTok, send(t, schema.AppendActivityRequestV1{Kind: schema.ActivityKindRunStarted}))
			assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
			if rec.Header().Get("Retry-After") == "" {
				t.Error("no Retry-After on the 429")
			}
			if n := len(f.auditRows(t, store.AuditAgentRateLimited)); n != 1 {
				t.Errorf("agent_rate_limited rows = %d, want 1", n)
			}
		}},
		{"an intervention kind the session did not declare", func(t *testing.T, f *extFixture) {
			_, run := f.startRun(t, schema.InterventionKindCancel)
			rec := f.do(t, "POST", runInterventionsPath(run.ID), f.aliceTok, askBody(schema.InterventionKindPause, ""))
			assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodeInterventionUnsupported)
			if n := len(f.auditRows(t, store.AuditInterventionUnsupported)); n != 1 {
				t.Errorf("intervention_unsupported rows = %d, want 1", n)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.run(t, newExtFixture(t)) })
	}
}

// A run report is not a heartbeat (design §6): a sender that reports but never
// heartbeats shows stale after the threshold, and its open run reads
// outcome_unknown.
func TestExternalRuntimeThatNeverHeartbeatsShowsStale(t *testing.T) {
	f := newExtFixture(t)
	sess, run := f.startRun(t)
	view := func() schema.SessionV1 {
		t.Helper()
		for _, s := range f.roster(t, f.aliceTok, "general").Sessions {
			if s.ID == sess.ID {
				return s
			}
		}
		t.Fatalf("session %d is not in the roster", sess.ID)
		return schema.SessionV1{}
	}
	if s := view(); s.State != schema.SessionStateOnline || s.OutcomeUnknown {
		t.Fatalf("fresh session = %+v, want online and not outcome_unknown", s)
	}
	f.clock.Advance(20 * time.Second)
	if rec := f.do(t, "POST", runPath(run.ID, "/state"), f.ciTok, send(t, schema.SetRunStateRequestV1{State: schema.RunStateRunning})); rec.Code != http.StatusOK {
		t.Fatalf("state = %d %s", rec.Code, rec.Body)
	}
	f.clock.Advance(15 * time.Second) // 35 s since the last heartbeat, 15 s since the report
	f.srv.sweepSessions(context.Background())
	if s := view(); s.State != schema.SessionStateStale || s.Status != schema.SessionStatusStale || !s.OutcomeUnknown {
		t.Errorf("session after a report and no heartbeat = %+v, want stale and outcome_unknown", s)
	}
	f.clock.Advance(60 * time.Second) // 95 s
	f.srv.sweepSessions(context.Background())
	if s := view(); s.State != schema.SessionStateOffline || !s.OutcomeUnknown {
		t.Errorf("session after 95 s = %+v, want offline and outcome_unknown", s)
	}
}
