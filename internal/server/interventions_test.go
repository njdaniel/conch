package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Interventions, end to end through the handlers (issue #287). Every
// InterventionV1 and list a handler sends is decoded and run through its
// Validate here, so a document the schema refuses fails the test that
// produced it.

// interventionFixture is the run fixture and besides:
//
//   - sess, bot's session, which declares message and cancel;
//   - run, bot's running run on sess, answering alice's whisper to bot, so
//     its audience is alice and bot.
type interventionFixture struct {
	*runFixture
	sess int64
	run  schema.RunV1
}

const interventionAgentBody = `{"client":"adapter","runtime":"claude-code","model":"opus","host":"build-7","interventions":["message","cancel"]}`

func newInterventionFixture(t *testing.T) *interventionFixture {
	t.Helper()
	f := &interventionFixture{runFixture: newRunFixture(t)}
	f.sess = f.create(t, f.botTok, interventionAgentBody).ID
	f.run = f.newRun(t)
	return f
}

// newRun starts another running run of bot on sess answering the whisper.
func (f *interventionFixture) newRun(t *testing.T) schema.RunV1 {
	t.Helper()
	run := f.start(t, f.botTok, f.sess, "general", f.whisper, "answer alice about the deploy")
	return f.mustState(t, f.botTok, run.ID, schema.RunStateRunning)
}

func runInterventionsPath(runID int64) string { return runPath(runID, "/interventions") }
func interventionPath(id int64, suffix string) string {
	return fmt.Sprintf("/v1/interventions/%d%s", id, suffix)
}

func askBody(kind schema.InterventionKind, body string) string {
	b, _ := json.Marshal(schema.CreateInterventionRequestV1{Kind: kind, Body: body})
	return string(b)
}

func ackBody(state schema.InterventionState, reason string) string {
	b, _ := json.Marshal(schema.AckInterventionRequestV1{State: state, Reason: reason})
	return string(b)
}

func validIntervention(t *testing.T, rec *httptest.ResponseRecorder) schema.InterventionV1 {
	t.Helper()
	doc := decodeBody[schema.InterventionV1](t, rec)
	if err := doc.Validate(); err != nil {
		t.Fatalf("the server sent an invalid intervention: %v\n%s", err, rec.Body)
	}
	return doc
}

func validInterventionList(t *testing.T, rec *httptest.ResponseRecorder) schema.ListInterventionsResponseV1 {
	t.Helper()
	if !strings.Contains(rec.Body.String(), `"interventions":[`) {
		t.Fatalf("interventions is not an array: %s", rec.Body)
	}
	doc := decodeBody[schema.ListInterventionsResponseV1](t, rec)
	for _, iv := range doc.Interventions {
		if err := iv.Validate(); err != nil {
			t.Fatalf("the server listed an invalid intervention: %v\n%s", err, rec.Body)
		}
	}
	return doc
}

// ask requests an intervention and returns the raw answer.
func (f *interventionFixture) ask(t *testing.T, token string, runID int64, kind schema.InterventionKind, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, "POST", runInterventionsPath(runID), token, askBody(kind, body))
}

// mustAsk requests a cancel (or, for a message, one with a body) as alice and
// fails unless it is created.
func (f *interventionFixture) mustAsk(t *testing.T, token string, runID int64, kind schema.InterventionKind) schema.InterventionV1 {
	t.Helper()
	body := ""
	if kind == schema.InterventionKindMessage {
		body = "please also run the linter"
	}
	rec := f.ask(t, token, runID, kind, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ask %s = %d %s", kind, rec.Code, rec.Body)
	}
	return validIntervention(t, rec)
}

func (f *interventionFixture) ack(t *testing.T, token string, id int64, state schema.InterventionState, reason string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, "POST", interventionPath(id, "/ack"), token, ackBody(state, reason))
}

func (f *interventionFixture) mustAck(t *testing.T, id int64, state schema.InterventionState, reason string) schema.InterventionV1 {
	t.Helper()
	rec := f.ack(t, f.botTok, id, state, reason)
	if rec.Code != http.StatusOK {
		t.Fatalf("ack %s = %d %s", state, rec.Code, rec.Body)
	}
	return validIntervention(t, rec)
}

func (f *interventionFixture) poll(t *testing.T, token string, sess int64, query string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, "GET", sessionPath(sess, "/interventions"+query), token, "")
}

func (f *interventionFixture) polledIDs(t *testing.T, token string, sess int64, query string) []int64 {
	t.Helper()
	rec := f.poll(t, token, sess, query)
	if rec.Code != http.StatusOK {
		t.Fatalf("poll %s = %d %s", query, rec.Code, rec.Body)
	}
	ids := []int64{}
	for _, iv := range validInterventionList(t, rec).Interventions {
		ids = append(ids, iv.ID)
	}
	return ids
}

// The whole story the issue is about: a person asks the agent's run to take a
// message and to stop; the adapter polls, sees both, and answers. The run is
// left exactly as the agent last set it, until the adapter reports through
// the state route, and the audit log names the person and the kind and
// nothing of what was said.
func TestInterventionOnARunEndToEnd(t *testing.T) {
	f := newInterventionFixture(t)
	const instruction = "also run the linter before you answer"
	f.clock.Advance(time.Second)

	rec := f.ask(t, f.aliceTok, f.run.ID, schema.InterventionKindMessage, instruction)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ask message = %d %s", rec.Code, rec.Body)
	}
	msg := validIntervention(t, rec)
	if msg.RunID != f.run.ID || msg.Kind != schema.InterventionKindMessage || msg.Body != instruction ||
		msg.State != schema.InterventionStateRequested || msg.RequestedBy != f.alice.ID ||
		!msg.RequestedAt.Time().Equal(f.clock.Now()) || msg.AnsweredAt != nil || msg.Reason != "" {
		t.Fatalf("created message intervention = %+v", msg)
	}
	cancel := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel)
	if cancel.Body != "" || cancel.ID <= msg.ID {
		t.Fatalf("created cancel intervention = %+v", cancel)
	}

	// A member of the channel outside the whisper, and the operator, cannot
	// ask, list or read; the answer is the one a run that does not exist
	// gets.
	for name, tok := range map[string]string{"bob": f.bobTok, "operator": f.rootTok} {
		t.Run(name, func(t *testing.T) {
			f.assertNoInterventionAccess(t, tok, f.run.ID, msg.ID)
		})
	}

	// The adapter polls for what is waiting, oldest first.
	rec = f.poll(t, f.botTok, f.sess, "?state=requested")
	if rec.Code != http.StatusOK {
		t.Fatalf("poll = %d %s", rec.Code, rec.Body)
	}
	got := validInterventionList(t, rec).Interventions
	if len(got) != 2 || got[0].ID != msg.ID || got[1].ID != cancel.ID || got[0].Body != instruction {
		t.Fatalf("polled = %+v, want the message then the cancel", got)
	}
	// The person who asked reads the same two.
	if mine := validInterventionList(t, f.do(t, "GET", runInterventionsPath(f.run.ID), f.aliceTok, "")).Interventions; len(mine) != 2 {
		t.Errorf("alice lists %+v", mine)
	}
	if one := validIntervention(t, f.do(t, "GET", interventionPath(cancel.ID, ""), f.aliceTok, "")); one.ID != cancel.ID {
		t.Errorf("alice reads %+v", one)
	}

	f.clock.Advance(2 * time.Second)
	answered := f.mustAck(t, msg.ID, schema.InterventionStateApplied, "")
	if answered.State != schema.InterventionStateApplied || answered.AnsweredAt == nil || !answered.AnsweredAt.Time().Equal(f.clock.Now()) {
		t.Errorf("answered message = %+v", answered)
	}
	const why = "the sandbox had no way to stop the build"
	f.mustAck(t, cancel.ID, schema.InterventionStateAcknowledged, "")
	done := f.mustAck(t, cancel.ID, schema.InterventionStateApplied, why)
	if done.Reason != why {
		t.Errorf("answered cancel = %+v", done)
	}
	if left := f.polledIDs(t, f.botTok, f.sess, "?state=requested"); len(left) != 0 {
		t.Errorf("still waiting after both were answered: %v", left)
	}
	if rec := f.poll(t, f.botTok, f.sess, "?state=requested"); !strings.Contains(rec.Body.String(), `"interventions":[]`) {
		t.Errorf("an empty poll is not an empty array: %s", rec.Body)
	}

	// An applied cancel is the adapter saying it stopped the work. The run is
	// still what the agent last set, and its activity has not been touched,
	// until the adapter reports through the state route.
	after := validRun(t, f.do(t, "GET", runPath(f.run.ID, ""), f.aliceTok, ""))
	if after.State != schema.RunStateRunning || after.Outcome != "" || after.EndedAt != nil {
		t.Errorf("run after an applied cancel = %+v; want it still running", after)
	}
	if page := validActivityList(t, f.do(t, "GET", runPath(f.run.ID, "/activity"), f.aliceTok, "")); len(page.Activity) != 0 {
		t.Errorf("activity after the interventions = %+v", page)
	}
	if ended := f.mustState(t, f.botTok, f.run.ID, schema.RunStateCancelled); ended.State != schema.RunStateCancelled {
		t.Errorf("run after the adapter's report = %+v", ended)
	}

	// The audit log: who asked what of which run, and the state of each
	// answer, and none of the words said.
	requested := f.auditRows(t, store.AuditInterventionRequested)
	if len(requested) != 2 {
		t.Fatalf("intervention_requested rows = %+v", requested)
	}
	for i, want := range []schema.InterventionV1{msg, cancel} {
		row := requested[i]
		detail := fmt.Sprintf("run=%d kind=%s requested_by=%d", f.run.ID, want.Kind, f.alice.ID)
		if row.Actor != fmt.Sprintf("principal:%d", f.alice.ID) || row.Subject != fmt.Sprintf("intervention:%d", want.ID) || row.Detail != detail {
			t.Errorf("intervention_requested[%d] = %+v, want actor alice and detail %q", i, row, detail)
		}
	}
	answers := f.auditRows(t, store.AuditInterventionAnswered)
	var states []string
	for _, row := range answers {
		if row.Actor != fmt.Sprintf("principal:%d", f.bot.ID) {
			t.Errorf("intervention_answered actor = %q, want the agent", row.Actor)
		}
		states = append(states, row.Detail[strings.LastIndex(row.Detail, "state=")+len("state="):])
	}
	if strings.Join(states, ",") != "applied,acknowledged,applied" {
		t.Errorf("intervention_answered states = %v", states)
	}
	for _, row := range f.audit(t) {
		if text := row.Actor + row.Action + row.Subject + row.Detail; strings.Contains(text, instruction) || strings.Contains(text, why) {
			t.Errorf("audit row %+v holds what was said", row)
		}
	}
}

// assertNoInterventionAccess checks that token gets, on every route that
// names the run or one of its interventions, exactly the answer a missing
// run or intervention gets, and that nothing was stored by the attempt.
func (f *interventionFixture) assertNoInterventionAccess(t *testing.T, token string, runID, interventionID int64) {
	t.Helper()
	before := len(f.auditRows(t, store.AuditInterventionRequested)) + len(f.auditRows(t, store.AuditInterventionUnsupported))
	for _, c := range []struct {
		method, real, missing, body, code string
	}{
		{"POST", runInterventionsPath(runID), runInterventionsPath(999999), askBody(schema.InterventionKindCancel, ""), schema.ErrorCodeRunNotFound},
		{"GET", runInterventionsPath(runID), runInterventionsPath(999999), "", schema.ErrorCodeRunNotFound},
		{"GET", interventionPath(interventionID, ""), interventionPath(999999, ""), "", schema.ErrorCodeInterventionNotFound},
		{"POST", interventionPath(interventionID, "/ack"), interventionPath(999999, "/ack"), ackBody(schema.InterventionStateApplied, ""), schema.ErrorCodeInterventionNotFound},
	} {
		got := f.do(t, c.method, c.real, token, c.body)
		missing := f.do(t, c.method, c.missing, token, c.body)
		assertErrorBody(t, got, http.StatusNotFound, c.code)
		if got.Body.String() != missing.Body.String() || got.Code != missing.Code {
			t.Errorf("%s %s answers %d %s, a missing one %d %s", c.method, c.real, got.Code, got.Body, missing.Code, missing.Body)
		}
	}
	if after := len(f.auditRows(t, store.AuditInterventionRequested)) + len(f.auditRows(t, store.AuditInterventionUnsupported)); after != before {
		t.Errorf("attempts by a reader who may not see the run wrote %d audit rows", after-before)
	}
}

// Every row of the route table, as every kind of caller: who may do what, and
// the answer for each who may not. A run or intervention the caller may not
// see answers exactly as one that does not exist.
func TestInterventionRouteAuthority(t *testing.T) {
	f := newInterventionFixture(t)
	secondBotCred := func() string {
		_, tok, err := f.srv.store.CreateCredential(context.Background(), "system", f.bot.ID, "second", nil)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}()
	type routeCase struct {
		name   string
		route  string
		token  string
		status int
		code   string
		// hidden: the answer must be byte for byte the one a missing target
		// gets.
		hidden bool
	}
	cases := []routeCase{
		// POST /v1/runs/{id}/interventions: a human who can see the run.
		{"ask: the whisper's reader", "ask", f.aliceTok, 201, "", false},
		{"ask: a member outside the whisper", "ask", f.bobTok, 404, schema.ErrorCodeRunNotFound, true},
		{"ask: the operator, who cannot see the run", "ask", f.rootTok, 404, schema.ErrorCodeRunNotFound, true},
		{"ask: a human in no channel", "ask", f.carolTok, 404, schema.ErrorCodeRunNotFound, true},
		{"ask: the run's own agent", "ask", f.botTok, 403, "human_required", true},
		{"ask: another agent", "ask", f.rivalTok, 403, "human_required", true},
		{"ask: an agent with a narrow manifest", "ask", f.shyTok, 403, "human_required", true},
		// GET /v1/runs/{id}/interventions: readers who can see the run.
		{"list: the whisper's reader", "list", f.aliceTok, 200, "", false},
		{"list: the run's own agent", "list", f.botTok, 200, "", false},
		{"list: a member outside the whisper", "list", f.bobTok, 404, schema.ErrorCodeRunNotFound, true},
		{"list: the operator", "list", f.rootTok, 404, schema.ErrorCodeRunNotFound, true},
		{"list: a human in no channel", "list", f.carolTok, 404, schema.ErrorCodeRunNotFound, true},
		{"list: another agent", "list", f.rivalTok, 404, schema.ErrorCodeRunNotFound, true},
		// GET /v1/interventions/{id}.
		{"get: the whisper's reader", "get", f.aliceTok, 200, "", false},
		{"get: the run's own agent", "get", f.botTok, 200, "", false},
		{"get: a member outside the whisper", "get", f.bobTok, 404, schema.ErrorCodeInterventionNotFound, true},
		{"get: the operator", "get", f.rootTok, 404, schema.ErrorCodeInterventionNotFound, true},
		{"get: a human in no channel", "get", f.carolTok, 404, schema.ErrorCodeInterventionNotFound, true},
		{"get: another agent", "get", f.rivalTok, 404, schema.ErrorCodeInterventionNotFound, true},
		// GET /v1/sessions/{id}/interventions: the credential that owns it.
		{"poll: the owning credential", "poll", f.botTok, 200, "", false},
		{"poll: another credential of the same agent", "poll", secondBotCred, 404, schema.ErrorCodeSessionNotFound, true},
		{"poll: another agent", "poll", f.rivalTok, 404, schema.ErrorCodeSessionNotFound, true},
		{"poll: the person who asked", "poll", f.aliceTok, 404, schema.ErrorCodeSessionNotFound, true},
		{"poll: the operator", "poll", f.rootTok, 404, schema.ErrorCodeSessionNotFound, true},
		// POST /v1/interventions/{id}/ack: the credential that owns the
		// run's session.
		{"ack: the owning credential", "ack", f.botTok, 200, "", false},
		{"ack: another credential of the same agent", "ack", secondBotCred, 404, schema.ErrorCodeInterventionNotFound, true},
		{"ack: another agent in the channel", "ack", f.rivalTok, 404, schema.ErrorCodeInterventionNotFound, true},
		{"ack: the person who asked", "ack", f.aliceTok, 404, schema.ErrorCodeInterventionNotFound, true},
		{"ack: a member outside the whisper", "ack", f.bobTok, 404, schema.ErrorCodeInterventionNotFound, true},
		{"ack: the operator", "ack", f.rootTok, 404, schema.ErrorCodeInterventionNotFound, true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			run := f.newRun(t)
			iv := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel)
			var method, real, missing, body string
			switch tt.route {
			case "ask":
				method, real, missing, body = "POST", runInterventionsPath(run.ID), runInterventionsPath(999999), askBody(schema.InterventionKindCancel, "")
			case "list":
				method, real, missing = "GET", runInterventionsPath(run.ID), runInterventionsPath(999999)
			case "get":
				method, real, missing = "GET", interventionPath(iv.ID, ""), interventionPath(999999, "")
			case "poll":
				method, real, missing = "GET", sessionPath(f.sess, "/interventions"), sessionPath(999999, "/interventions")
			case "ack":
				method, real, missing, body = "POST", interventionPath(iv.ID, "/ack"), interventionPath(999999, "/ack"), ackBody(schema.InterventionStateApplied, "")
			}
			got := f.do(t, method, real, tt.token, body)
			if tt.status >= 400 {
				assertErrorBody(t, got, tt.status, tt.code)
			} else if got.Code != tt.status {
				t.Fatalf("%s %s = %d %s, want %d", method, real, got.Code, got.Body, tt.status)
			}
			switch tt.route {
			case "ask":
				if tt.status == 201 {
					if doc := validIntervention(t, got); doc.RunID != run.ID || doc.RequestedBy != f.alice.ID {
						t.Errorf("created %+v", doc)
					}
				}
			case "list":
				if tt.status == 200 && len(validInterventionList(t, got).Interventions) != 1 {
					t.Errorf("list = %s", got.Body)
				}
			case "get", "ack":
				if tt.status == 200 && validIntervention(t, got).ID != iv.ID {
					t.Errorf("read %s", got.Body)
				}
			case "poll":
				if tt.status == 200 && !slices.Equal(interventionIDs(validInterventionList(t, got).Interventions), f.sessionInterventionIDs(t)) {
					t.Errorf("poll = %s", got.Body)
				}
			}
			if tt.hidden {
				// Compared with the same call at a target that is not there,
				// by the same caller.
				m := f.do(t, method, missing, tt.token, body)
				if got.Code != m.Code || got.Body.String() != m.Body.String() {
					t.Errorf("answers %d %s; a missing target answers %d %s", got.Code, got.Body, m.Code, m.Body)
				}
				// And nothing was changed: the intervention is still waiting.
				if tt.route == "ack" {
					if cur := validIntervention(t, f.do(t, "GET", interventionPath(iv.ID, ""), f.aliceTok, "")); cur.State != schema.InterventionStateRequested {
						t.Errorf("the refused answer changed it: %+v", cur)
					}
				}
			}
		})
	}
}

func interventionIDs(list []schema.InterventionV1) []int64 {
	out := []int64{}
	for _, iv := range list {
		out = append(out, iv.ID)
	}
	return out
}

// sessionInterventionIDs is every intervention on the fixture's session, by
// the store, for comparing a poll with.
func (f *interventionFixture) sessionInterventionIDs(t *testing.T) []int64 {
	t.Helper()
	list, err := f.srv.store.ListSessionInterventions(context.Background(), f.sess, f.credentialID(t, f.botTok),
		store.Reader{PrincipalID: f.bot.ID, Scoped: true}, "", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := []int64{}
	for _, iv := range list {
		out = append(out, iv.ID)
	}
	return out
}

// An agent cannot ask for an intervention, on its own run or another's, and
// whatever it holds; and it is told so the same way for a run that is not
// there.
func TestAgentsCannotRequestInterventions(t *testing.T) {
	f := newInterventionFixture(t)
	rivalSess := f.create(t, f.rivalTok, interventionAgentBody).ID
	rivalRun := f.start(t, f.rivalTok, rivalSess, "general", 0, "rival's work")
	before := len(f.sessionInterventionIDs(t))
	for name, c := range map[string]struct {
		token string
		run   int64
	}{
		"bot on its own run":            {f.botTok, f.run.ID},
		"bot on rival's":                {f.botTok, rivalRun.ID},
		"rival on bot's":                {f.rivalTok, f.run.ID},
		"rival on its own":              {f.rivalTok, rivalRun.ID},
		"an agent on a run not there":   {f.shyTok, 999999},
		"an agent with no runs.manage":  {f.muteTok, f.run.ID},
		"bot on a run it cannot see":    {f.botTok, f.start(t, f.rivalTok, rivalSess, "general", f.whisperTo(t, f.alice, "just rival", f.rival), "t").ID},
		"bot on a run in another place": {f.botTok, f.start(t, f.botTok, f.sess, "test-private", 0, "t").ID},
	} {
		t.Run(name, func(t *testing.T) {
			assertErrorBody(t, f.ask(t, c.token, c.run, schema.InterventionKindCancel, ""), http.StatusForbidden, "human_required")
		})
	}
	if after := len(f.sessionInterventionIDs(t)); after != before {
		t.Errorf("an agent's request stored %d interventions", after-before)
	}
	if n := len(f.auditRows(t, store.AuditInterventionRequested)); n != 0 {
		t.Errorf("an agent's request was audited as a request: %d rows", n)
	}
}

// The body rules are the schema's: a message has a body, nothing else takes
// one, the kind is one of the four, and the JSON is JSON. A refusal stores
// nothing.
func TestInterventionRequestValidation(t *testing.T) {
	f := newInterventionFixture(t)
	tests := []struct {
		name string
		body string
		want int
	}{
		{"a message with its body", `{"kind":"message","body":"go left"}`, 201},
		{"a message at the length limit", askBody(schema.InterventionKindMessage, strings.Repeat("x", schema.MaxInterventionBodyLength)), 201},
		{"a cancel", `{"kind":"cancel"}`, 201},
		{"a message with no body", `{"kind":"message"}`, 400},
		{"a message with a blank body", `{"kind":"message","body":"   "}`, 400},
		{"a message over the limit", askBody(schema.InterventionKindMessage, strings.Repeat("x", schema.MaxInterventionBodyLength+1)), 400},
		{"a message with an escape sequence", askBody(schema.InterventionKindMessage, "\x1b[2Jlooks fine"), 400},
		{"a cancel with a body", `{"kind":"cancel","body":"now"}`, 400},
		{"a pause with a body", `{"kind":"pause","body":"now"}`, 400},
		{"an unknown kind", `{"kind":"reboot"}`, 400},
		{"no kind", `{}`, 400},
		{"an empty body", ``, 400},
		{"not JSON", `cancel`, 400},
		{"an array", `[]`, 400},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run := f.newRun(t)
			rec := f.do(t, "POST", runInterventionsPath(run.ID), f.aliceTok, tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
			if tt.want == 400 {
				assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")
			} else {
				validIntervention(t, rec)
			}
			list := validInterventionList(t, f.do(t, "GET", runInterventionsPath(run.ID), f.aliceTok, ""))
			want := 0
			if tt.want == 201 {
				want = 1
			}
			if len(list.Interventions) != want {
				t.Errorf("the run holds %d interventions, want %d", len(list.Interventions), want)
			}
		})
	}
	// A run the caller may not see is not found whatever the body is.
	run := f.newRun(t)
	assertErrorBody(t, f.do(t, "POST", runInterventionsPath(run.ID), f.bobTok, `nonsense`), http.StatusNotFound, schema.ErrorCodeRunNotFound)
	// An oversized body is 413, as for the other run routes.
	big := fmt.Sprintf(`{"kind":"message","body":%q}`, strings.Repeat("x", 2<<20))
	assertErrorBody(t, f.do(t, "POST", runInterventionsPath(run.ID), f.aliceTok, big), http.StatusRequestEntityTooLarge, "request_too_large")
}

// A kind the run's session did not declare is refused with 409, stores
// nothing, and the refusal is audited once with the requester and without the
// words. A kind it did declare is taken, pause included, though nothing
// answers pause yet.
func TestUnsupportedInterventionKinds(t *testing.T) {
	f := newInterventionFixture(t)
	pauseSess := f.create(t, f.rivalTok, agentBody).ID // declares pause and cancel
	pauseRun := f.start(t, f.rivalTok, pauseSess, "general", 0, "rival's work")
	noneSess := f.create(t, f.shyTok, `{"client":"adapter"}`).ID // declares nothing
	noneRun := f.start(t, f.shyTok, noneSess, "general", 0, "shy's work")
	const instruction = "an instruction nobody else reads"
	tests := []struct {
		name string
		run  int64
		kind schema.InterventionKind
		body string
		want int
	}{
		{"pause on a session that declared message and cancel", f.run.ID, schema.InterventionKindPause, "", 409},
		{"resume on the same", f.run.ID, schema.InterventionKindResume, "", 409},
		{"message on a session that declared pause and cancel", pauseRun.ID, schema.InterventionKindMessage, instruction, 409},
		{"pause on that session, which declared it", pauseRun.ID, schema.InterventionKindPause, "", 201},
		{"cancel on that session", pauseRun.ID, schema.InterventionKindCancel, "", 201},
		{"resume on that session", pauseRun.ID, schema.InterventionKindResume, "", 409},
		{"cancel on a session that declared nothing", noneRun.ID, schema.InterventionKindCancel, "", 409},
		{"message on a session that declared nothing", noneRun.ID, schema.InterventionKindMessage, instruction, 409},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refusals, requests := len(f.auditRows(t, store.AuditInterventionUnsupported)), len(f.auditRows(t, store.AuditInterventionRequested))
			rec := f.ask(t, f.aliceTok, tt.run, tt.kind, tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
			newRefusals := f.auditRows(t, store.AuditInterventionUnsupported)[refusals:]
			newRequests := f.auditRows(t, store.AuditInterventionRequested)[requests:]
			if tt.want == 201 {
				if validIntervention(t, rec).Kind != tt.kind || len(newRefusals) != 0 || len(newRequests) != 1 {
					t.Errorf("accepted: %s; refusals %v, requests %v", rec.Body, newRefusals, newRequests)
				}
				return
			}
			assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodeInterventionUnsupported)
			if len(newRequests) != 0 || len(newRefusals) != 1 {
				t.Fatalf("a refused request wrote requests %v and refusals %v; want exactly one refusal", newRequests, newRefusals)
			}
			row := newRefusals[0]
			if row.Actor != fmt.Sprintf("principal:%d", f.alice.ID) || row.Subject != fmt.Sprintf("run:%d", tt.run) ||
				row.Detail != fmt.Sprintf("run=%d kind=%s requested_by=%d", tt.run, tt.kind, f.alice.ID) {
				t.Errorf("refusal row = %+v", row)
			}
			if strings.Contains(row.Detail, instruction) {
				t.Errorf("the refusal's audit row holds the instruction: %+v", row)
			}
		})
	}
	// The refused ones left no row behind.
	if list := validInterventionList(t, f.do(t, "GET", runInterventionsPath(f.run.ID), f.aliceTok, "")); len(list.Interventions) != 0 {
		t.Errorf("run holds %+v", list)
	}
	if list := validInterventionList(t, f.do(t, "GET", runInterventionsPath(noneRun.ID), f.aliceTok, "")); len(list.Interventions) != 0 {
		t.Errorf("run holds %+v", list)
	}
	// A refusal for a reader who cannot see the run is not audited: that
	// answer is run_not_found and says nothing of the session.
	before := len(f.auditRows(t, store.AuditInterventionUnsupported))
	assertErrorBody(t, f.ask(t, f.bobTok, f.run.ID, schema.InterventionKindPause, ""), http.StatusNotFound, schema.ErrorCodeRunNotFound)
	if len(f.auditRows(t, store.AuditInterventionUnsupported)) != before {
		t.Error("a request by someone who cannot see the run was audited as unsupported")
	}
}

// A run that has ended takes no more requests (409 invalid_transition), but
// what was asked before it ended can still be answered, and the answer does
// not touch the run.
func TestInterventionsOnAnEndedRun(t *testing.T) {
	f := newInterventionFixture(t)
	for _, end := range []schema.RunState{schema.RunStateCompleted, schema.RunStateFailed, schema.RunStateCancelled} {
		t.Run(string(end), func(t *testing.T) {
			run := f.newRun(t)
			waiting := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel)
			f.mustState(t, f.botTok, run.ID, end)
			count := len(validInterventionList(t, f.do(t, "GET", runInterventionsPath(run.ID), f.aliceTok, "")).Interventions)
			assertErrorBody(t, f.ask(t, f.aliceTok, run.ID, schema.InterventionKindCancel, ""), http.StatusConflict, schema.ErrorCodeInvalidTransition)
			assertErrorBody(t, f.ask(t, f.aliceTok, run.ID, schema.InterventionKindMessage, "too late"), http.StatusConflict, schema.ErrorCodeInvalidTransition)
			if got := len(validInterventionList(t, f.do(t, "GET", runInterventionsPath(run.ID), f.aliceTok, "")).Interventions); got != count {
				t.Errorf("an ended run took %d more interventions", got-count)
			}
			before := f.do(t, "GET", runPath(run.ID, ""), f.aliceTok, "").Body.String()
			if got := f.mustAck(t, waiting.ID, schema.InterventionStateApplied, ""); got.State != schema.InterventionStateApplied {
				t.Errorf("answer = %+v", got)
			}
			if after := f.do(t, "GET", runPath(run.ID, ""), f.aliceTok, "").Body.String(); after != before {
				t.Errorf("run changed by an answer: %s -> %s", before, after)
			}
		})
	}
}

// At most sixteen interventions wait on one run. The next is 429; answering
// one makes room for one; another run has its own sixteen.
func TestInterventionBound(t *testing.T) {
	f := newInterventionFixture(t)
	ids := []int64{}
	for i := 0; i < store.MaxRequestedInterventions; i++ {
		ids = append(ids, f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel).ID)
	}
	over := f.ask(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel, "")
	assertErrorBody(t, over, http.StatusTooManyRequests, schema.ErrorCodeTooManyInterventions)
	// The refusal is not a request and not an unsupported kind.
	if n := len(f.auditRows(t, store.AuditInterventionRequested)); n != store.MaxRequestedInterventions {
		t.Errorf("intervention_requested rows = %d, want %d", n, store.MaxRequestedInterventions)
	}
	if n := len(f.auditRows(t, store.AuditInterventionUnsupported)); n != 0 {
		t.Errorf("a full run was audited as unsupported: %d rows", n)
	}
	if got := len(validInterventionList(t, f.do(t, "GET", runInterventionsPath(f.run.ID), f.aliceTok, "")).Interventions); got != store.MaxRequestedInterventions {
		t.Errorf("the run holds %d, want %d", got, store.MaxRequestedInterventions)
	}
	// An acknowledged one is no longer waiting: it makes room.
	f.mustAck(t, ids[0], schema.InterventionStateAcknowledged, "")
	f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel)
	assertErrorBody(t, f.ask(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel, ""), http.StatusTooManyRequests, schema.ErrorCodeTooManyInterventions)
	// The bound counts every kind: a message is held to it too.
	assertErrorBody(t, f.ask(t, f.aliceTok, f.run.ID, schema.InterventionKindMessage, "hello"), http.StatusTooManyRequests, schema.ErrorCodeTooManyInterventions)
	// The bound belongs to the run.
	f.mustAsk(t, f.aliceTok, f.newRun(t).ID, schema.InterventionKindCancel)
}

// Every pair of states over the route: the legal moves are 200 and the rest
// 409 invalid_transition, with the intervention untouched; the state requested
// is not an answer; a failed answer carries a reason; the reason is bounded.
func TestAckInterventionMovesOverTheRoute(t *testing.T) {
	f := newInterventionFixture(t)
	states := []schema.InterventionState{
		schema.InterventionStateRequested, schema.InterventionStateAcknowledged,
		schema.InterventionStateApplied, schema.InterventionStateFailed,
	}
	// Independent of the code under test: from requested to any answer, from
	// acknowledged to applied or failed, and nowhere else.
	legal := map[[2]schema.InterventionState]bool{
		{"requested", "acknowledged"}: true, {"requested", "applied"}: true, {"requested", "failed"}: true,
		{"acknowledged", "applied"}: true, {"acknowledged", "failed"}: true,
	}
	via := map[schema.InterventionState]schema.InterventionState{
		schema.InterventionStateAcknowledged: schema.InterventionStateAcknowledged,
		schema.InterventionStateApplied:      schema.InterventionStateApplied,
		schema.InterventionStateFailed:       schema.InterventionStateFailed,
	}
	for _, from := range states {
		for _, to := range states {
			t.Run(fmt.Sprintf("%s to %s", from, to), func(t *testing.T) {
				run := f.newRun(t)
				iv := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel)
				if step, ok := via[from]; ok {
					f.mustAck(t, iv.ID, step, "step")
				}
				rec := f.ack(t, f.botTok, iv.ID, to, "because")
				cur := validIntervention(t, f.do(t, "GET", interventionPath(iv.ID, ""), f.aliceTok, ""))
				switch {
				case to == schema.InterventionStateRequested:
					// Not an answer at all: the schema refuses it before any rule.
					assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")
					if cur.State != from {
						t.Errorf("state = %s after a refused answer, want %s", cur.State, from)
					}
				case legal[[2]schema.InterventionState{from, to}]:
					if rec.Code != http.StatusOK {
						t.Fatalf("legal move = %d %s", rec.Code, rec.Body)
					}
					got := validIntervention(t, rec)
					if got.State != to || got.Reason != "because" || got.AnsweredAt == nil || cur.State != to {
						t.Errorf("answered = %+v, stored %+v", got, cur)
					}
				default:
					assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodeInvalidTransition)
					if cur.State != from {
						t.Errorf("state = %s after an illegal move, want %s", cur.State, from)
					}
				}
			})
		}
	}
	reasons := []struct {
		name   string
		state  schema.InterventionState
		reason string
		want   int
	}{
		{"failed with a reason", schema.InterventionStateFailed, "the sandbox refused", 200},
		{"failed with no reason", schema.InterventionStateFailed, "", 400},
		{"failed with a blank reason", schema.InterventionStateFailed, "  ", 400},
		{"applied with no reason", schema.InterventionStateApplied, "", 200},
		{"acknowledged with no reason", schema.InterventionStateAcknowledged, "", 200},
		{"a reason at the limit", schema.InterventionStateFailed, strings.Repeat("r", schema.MaxInterventionReasonLength), 200},
		{"a reason over the limit", schema.InterventionStateFailed, strings.Repeat("r", schema.MaxInterventionReasonLength+1), 400},
		{"a reason with a carriage return", schema.InterventionStateFailed, "busy\rapplied", 400},
	}
	for _, tt := range reasons {
		t.Run(tt.name, func(t *testing.T) {
			run := f.newRun(t)
			iv := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel)
			rec := f.ack(t, f.botTok, iv.ID, tt.state, tt.reason)
			if rec.Code != tt.want {
				t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
			if tt.want == 400 {
				assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")
				if cur := validIntervention(t, f.do(t, "GET", interventionPath(iv.ID, ""), f.aliceTok, "")); cur.State != schema.InterventionStateRequested {
					t.Errorf("a refused answer changed it: %+v", cur)
				}
				return
			}
			if got := validIntervention(t, rec); got.Reason != tt.reason {
				t.Errorf("reason = %q, want %q", got.Reason, tt.reason)
			}
		})
	}
	// Not JSON, and an id that is not a number.
	run := f.newRun(t)
	iv := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel)
	assertErrorBody(t, f.do(t, "POST", interventionPath(iv.ID, "/ack"), f.botTok, `applied`), http.StatusBadRequest, "invalid_request")
	assertErrorBody(t, f.do(t, "POST", "/v1/interventions/x/ack", f.botTok, ackBody(schema.InterventionStateApplied, "")), http.StatusNotFound, schema.ErrorCodeInterventionNotFound)
	assertErrorBody(t, f.do(t, "GET", "/v1/interventions/-1", f.aliceTok, ""), http.StatusNotFound, schema.ErrorCodeInterventionNotFound)
	assertErrorBody(t, f.do(t, "GET", "/v1/runs/x/interventions", f.aliceTok, ""), http.StatusNotFound, schema.ErrorCodeRunNotFound)
}

// An applied or failed answer is final, whoever repeats it, and the audit
// holds one row per recorded answer and none for a refused one.
func TestAnsweredInterventionsStayAnswered(t *testing.T) {
	f := newInterventionFixture(t)
	iv := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel)
	f.mustAck(t, iv.ID, schema.InterventionStateFailed, "could not stop it")
	for _, to := range []schema.InterventionState{schema.InterventionStateAcknowledged, schema.InterventionStateApplied, schema.InterventionStateFailed} {
		assertErrorBody(t, f.ack(t, f.botTok, iv.ID, to, "again"), http.StatusConflict, schema.ErrorCodeInvalidTransition)
	}
	if got := validIntervention(t, f.do(t, "GET", interventionPath(iv.ID, ""), f.aliceTok, "")); got.State != schema.InterventionStateFailed || got.Reason != "could not stop it" {
		t.Errorf("after the refused repeats: %+v", got)
	}
	if n := len(f.auditRows(t, store.AuditInterventionAnswered)); n != 1 {
		t.Errorf("intervention_answered rows = %d, want 1", n)
	}
}

// The poll: the state filter, paging, the owning session only.
func TestPollingASessionsInterventions(t *testing.T) {
	f := newInterventionFixture(t)
	other := f.newRun(t)
	a := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel)
	b := f.mustAsk(t, f.aliceTok, other.ID, schema.InterventionKindMessage)
	c := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindMessage)
	f.mustAck(t, c.ID, schema.InterventionStateAcknowledged, "")
	d := f.mustAsk(t, f.aliceTok, other.ID, schema.InterventionKindCancel)
	f.mustAck(t, d.ID, schema.InterventionStateFailed, "no")

	// The same agent's other session, with a run and a request of its own,
	// and another agent's session: neither shows in sess's poll.
	sess2 := f.create(t, f.botTok, interventionAgentBody).ID
	run2 := f.start(t, f.botTok, sess2, "general", 0, "second session")
	e := f.mustAsk(t, f.aliceTok, run2.ID, schema.InterventionKindCancel)
	rivalSess := f.create(t, f.rivalTok, interventionAgentBody).ID
	rivalRun := f.start(t, f.rivalTok, rivalSess, "general", 0, "rival")
	rivalAsk := f.mustAsk(t, f.aliceTok, rivalRun.ID, schema.InterventionKindCancel)

	tests := []struct {
		name  string
		query string
		want  []int64
	}{
		{"waiting", "?state=requested", []int64{a.ID, b.ID}},
		{"acknowledged", "?state=acknowledged", []int64{c.ID}},
		{"applied", "?state=applied", []int64{}},
		{"failed", "?state=failed", []int64{d.ID}},
		{"every state", "", []int64{a.ID, b.ID, c.ID, d.ID}},
		{"an empty state is every state", "?state=", []int64{a.ID, b.ID, c.ID, d.ID}},
		{"after a cursor", fmt.Sprintf("?after=%d", b.ID), []int64{c.ID, d.ID}},
		{"a page of two", "?limit=2", []int64{a.ID, b.ID}},
		{"the next page", fmt.Sprintf("?limit=2&after=%d", b.ID), []int64{c.ID, d.ID}},
		{"waiting, after a cursor", fmt.Sprintf("?state=requested&after=%d", a.ID), []int64{b.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := f.polledIDs(t, f.botTok, f.sess, tt.query); !slices.Equal(got, tt.want) {
				t.Errorf("poll %s = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
	if got := f.polledIDs(t, f.botTok, sess2, "?state=requested"); !slices.Equal(got, []int64{e.ID}) {
		t.Errorf("second session's poll = %v, want [%d]", got, e.ID)
	}
	if got := f.polledIDs(t, f.rivalTok, rivalSess, ""); !slices.Equal(got, []int64{rivalAsk.ID}) {
		t.Errorf("rival's poll = %v, want [%d]", got, rivalAsk.ID)
	}
	// Asking for another agent's session by id is the answer a session that
	// is not there gets, and the other session's requests do not show.
	assertErrorBody(t, f.poll(t, f.botTok, rivalSess, ""), http.StatusNotFound, schema.ErrorCodeSessionNotFound)

	for _, q := range []string{"?state=done", "?state=REQUESTED", "?limit=0", "?limit=101", "?limit=x", "?after=-1", "?after=x"} {
		assertErrorBody(t, f.poll(t, f.botTok, f.sess, q), http.StatusBadRequest, "invalid_request")
	}
	// The polled documents carry the person's words to the adapter.
	for _, iv := range validInterventionList(t, f.poll(t, f.botTok, f.sess, "?state=requested")).Interventions {
		if iv.ID == b.ID && iv.Body != "please also run the linter" {
			t.Errorf("polled message = %+v", iv)
		}
	}

	// A session that has ended is not polled: the adapter is gone.
	if rec := f.do(t, "DELETE", sessionPath(sess2, ""), f.botTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("end session = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.poll(t, f.botTok, sess2, ""), http.StatusNotFound, schema.ErrorCodeSessionNotFound)
	assertErrorBody(t, f.poll(t, f.botTok, 999999, ""), http.StatusNotFound, schema.ErrorCodeSessionNotFound)
	assertErrorBody(t, f.do(t, "GET", "/v1/sessions/x/interventions", f.botTok, ""), http.StatusNotFound, schema.ErrorCodeSessionNotFound)
}

// The manifest gate for the adapter's two calls. The poll is an own-session
// call (D9): it needs runs.manage and no channel, and it spends runs.manage.
// The answer is a report on the run, so it needs runs.manage and the channel
// permission posting to the run's audience needs, and spends it as well.
func TestPollAndAnswerGates(t *testing.T) {
	f := newInterventionFixture(t)
	iv := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel)
	full := map[int64][]schema.ChannelPermission{f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionWhisper}}

	t.Run("without runs.manage", func(t *testing.T) {
		f.grant(t, f.bot.ID, []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilitySessionsManage}, nil, full)
		assertErrorBody(t, f.poll(t, f.botTok, f.sess, ""), http.StatusForbidden, "forbidden")
		assertErrorBody(t, f.ack(t, f.botTok, iv.ID, schema.InterventionStateApplied, ""), http.StatusForbidden, "forbidden")
		if cur := validIntervention(t, f.do(t, "GET", interventionPath(iv.ID, ""), f.aliceTok, "")); cur.State != schema.InterventionStateRequested {
			t.Errorf("a refused answer changed it: %+v", cur)
		}
	})
	t.Run("the poll needs no channel and no sessions.manage", func(t *testing.T) {
		f.grant(t, f.bot.ID, []schema.Capability{schema.CapabilityRunsManage}, nil, nil)
		if got := f.polledIDs(t, f.botTok, f.sess, ""); !slices.Equal(got, []int64{iv.ID}) {
			t.Errorf("poll = %v", got)
		}
	})
	t.Run("the answer needs the audience permission", func(t *testing.T) {
		// The run answers a whisper: post alone does not reach it.
		f.grant(t, f.bot.ID, schema.Capabilities(), nil, map[int64][]schema.ChannelPermission{f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost}})
		assertErrorBody(t, f.ack(t, f.botTok, iv.ID, schema.InterventionStateApplied, ""), http.StatusForbidden, "forbidden")
		f.grant(t, f.bot.ID, schema.Capabilities(), nil, full)
		f.mustAck(t, iv.ID, schema.InterventionStateAcknowledged, "")
	})
	t.Run("both spend runs.manage", func(t *testing.T) {
		f.grant(t, f.bot.ID, schema.Capabilities(), []schema.RateLimit{{Capability: schema.CapabilityRunsManage, Max: 3, WindowSeconds: 3600}}, full)
		// Two polls and one answer use the three; reading spends none of it.
		f.polledIDs(t, f.botTok, f.sess, "")
		f.polledIDs(t, f.botTok, f.sess, "")
		f.do(t, "GET", interventionPath(iv.ID, ""), f.botTok, "")
		f.do(t, "GET", runInterventionsPath(f.run.ID), f.botTok, "")
		f.mustAck(t, iv.ID, schema.InterventionStateApplied, "")
		assertErrorBody(t, f.poll(t, f.botTok, f.sess, ""), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		rec := f.ack(t, f.botTok, iv.ID, schema.InterventionStateApplied, "")
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	})
}

// A reader's read of an agent: it needs messages.read with read in the run's
// channel, as for the run itself.
func TestAgentReadsOfInterventionsNeedMessagesRead(t *testing.T) {
	f := newInterventionFixture(t)
	iv := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel)
	f.grant(t, f.bot.ID, []schema.Capability{schema.CapabilityRunsManage}, nil, map[int64][]schema.ChannelPermission{f.general.ID: {schema.ChannelPermissionRead}})
	assertErrorBody(t, f.do(t, "GET", interventionPath(iv.ID, ""), f.botTok, ""), http.StatusForbidden, "forbidden")
	assertErrorBody(t, f.do(t, "GET", runInterventionsPath(f.run.ID), f.botTok, ""), http.StatusForbidden, "forbidden")
	f.grant(t, f.bot.ID, schema.Capabilities(), nil, map[int64][]schema.ChannelPermission{f.general.ID: {schema.ChannelPermissionPost}})
	assertErrorBody(t, f.do(t, "GET", interventionPath(iv.ID, ""), f.botTok, ""), http.StatusForbidden, "forbidden")
}

// Who may ask follows who may see the run, and the run follows its channel
// and message: a channel-wide run takes requests from every member of the
// channel, the operator included and no further; a run in another channel
// from that channel's members; and leaving the channel takes away a person's
// sight of what they asked and of the answers.
func TestInterventionAudienceFollowsTheRun(t *testing.T) {
	f := newInterventionFixture(t)
	wide := f.start(t, f.botTok, f.sess, "general", 0, "open work")
	f.mustState(t, f.botTok, wide.ID, schema.RunStateRunning)
	privateRun := f.start(t, f.botTok, f.sess, "test-private", f.privateMsg, "private work")

	t.Run("a channel-wide run", func(t *testing.T) {
		fromBob := f.mustAsk(t, f.bobTok, wide.ID, schema.InterventionKindCancel)
		fromRoot := f.mustAsk(t, f.rootTok, wide.ID, schema.InterventionKindMessage)
		if fromBob.RequestedBy != f.bob.ID || fromRoot.RequestedBy != f.root.ID {
			t.Errorf("requested_by = %d and %d", fromBob.RequestedBy, fromRoot.RequestedBy)
		}
		// Everyone who can see the run sees everything asked of it.
		for _, tok := range []string{f.aliceTok, f.bobTok, f.rootTok, f.botTok} {
			if got := interventionIDs(validInterventionList(t, f.do(t, "GET", runInterventionsPath(wide.ID), tok, "")).Interventions); !slices.Equal(got, []int64{fromBob.ID, fromRoot.ID}) {
				t.Errorf("list = %v", got)
			}
		}
		f.assertNoInterventionAccess(t, f.carolTok, wide.ID, fromBob.ID)
		// A reader outside the channel stays outside whatever the run is.
		assertErrorBody(t, f.ask(t, f.carolTok, wide.ID, schema.InterventionKindCancel, ""), http.StatusNotFound, schema.ErrorCodeRunNotFound)
	})
	t.Run("a run in another channel", func(t *testing.T) {
		// test-private's members are alice and bot.
		own := f.mustAsk(t, f.aliceTok, privateRun.ID, schema.InterventionKindCancel)
		f.assertNoInterventionAccess(t, f.bobTok, privateRun.ID, own.ID)
		f.assertNoInterventionAccess(t, f.rootTok, privateRun.ID, own.ID)
	})
	t.Run("leaving the channel", func(t *testing.T) {
		iv := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel)
		if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.general.ID, f.alice.ID); err != nil {
			t.Fatal(err)
		}
		f.assertNoInterventionAccess(t, f.aliceTok, f.run.ID, iv.ID)
		// The agent still answers it: the person's leaving does not unask it.
		if got := f.mustAck(t, iv.ID, schema.InterventionStateApplied, ""); got.State != schema.InterventionStateApplied {
			t.Errorf("answer = %+v", got)
		}
	})
	t.Run("the agent leaving the channel", func(t *testing.T) {
		f2 := newInterventionFixture(t)
		iv := f2.mustAsk(t, f2.aliceTok, f2.run.ID, schema.InterventionKindCancel)
		if _, err := f2.srv.store.RemoveChannelMember(context.Background(), "system", f2.general.ID, f2.bot.ID); err != nil {
			t.Fatal(err)
		}
		// It is handed nothing and cannot answer: it can no longer see the run.
		if got := f2.polledIDs(t, f2.botTok, f2.sess, ""); len(got) != 0 {
			t.Errorf("an agent that left the channel polls %v", got)
		}
		assertErrorBody(t, f2.ack(t, f2.botTok, iv.ID, schema.InterventionStateApplied, ""), http.StatusNotFound, schema.ErrorCodeInterventionNotFound)
		if cur := validIntervention(t, f2.do(t, "GET", interventionPath(iv.ID, ""), f2.aliceTok, "")); cur.State != schema.InterventionStateRequested {
			t.Errorf("a refused answer changed it: %+v", cur)
		}
	})
}

// An agent that started a run on a net message as a member, and is then
// demoted to monitor on the net, may no longer answer for the run: an answer
// reaches the net's audience as a report does. The person's request stands
// and is untouched.
func TestDemotedNetMemberCannotAnswerInterventions(t *testing.T) {
	ctx := context.Background()
	f := newInterventionFixture(t)
	net, err := f.srv.store.CreateNet(ctx, "system", f.general.ID, "speak", f.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{f.alice.ID, f.bot.ID} {
		if _, err := f.srv.store.PutNetMember(ctx, "system", f.general.ID, "speak", id, schema.NetRoleMember, f.root.ID); err != nil {
			t.Fatal(err)
		}
	}
	m, _, err := f.srv.store.InsertScopedMessage(ctx, store.ScopedPost{
		ChannelID: f.general.ID, AuthorID: f.alice.ID, Body: "net question",
		Audience: schema.Audience{Kind: schema.AudienceKindNet, NetID: net.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := f.start(t, f.botTok, f.sess, "general", m.ID, "answer the net")
	f.mustState(t, f.botTok, run.ID, schema.RunStateRunning)
	iv := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel)

	if _, err := f.srv.store.PutNetMember(ctx, "system", f.general.ID, "speak", f.bot.ID, schema.NetRoleMonitor, f.root.ID); err != nil {
		t.Fatal(err)
	}
	got := f.ack(t, f.botTok, iv.ID, schema.InterventionStateApplied, "")
	missing := f.ack(t, f.botTok, 999999, schema.InterventionStateApplied, "")
	assertErrorBody(t, got, http.StatusNotFound, schema.ErrorCodeInterventionNotFound)
	if got.Body.String() != missing.Body.String() {
		t.Errorf("answer = %s, a missing one %s", got.Body, missing.Body)
	}
	if cur := validIntervention(t, f.do(t, "GET", interventionPath(iv.ID, ""), f.aliceTok, "")); cur.State != schema.InterventionStateRequested {
		t.Errorf("the refused answer changed it: %+v", cur)
	}
	if n := len(f.auditRows(t, store.AuditInterventionAnswered)); n != 0 {
		t.Errorf("a refused answer was audited: %d rows", n)
	}
	// The net's members can still ask of the run.
	f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel)
}

// An intervention never changes its run, whatever its kind and however it is
// answered: the run row and its activity are the same before and after.
func TestInterventionsNeverChangeTheRun(t *testing.T) {
	f := newInterventionFixture(t)
	f.mustState(t, f.botTok, f.run.ID, schema.RunStateBlocked)
	if rec := f.do(t, "POST", runPath(f.run.ID, "/activity"), f.botTok, activityBody); rec.Code != http.StatusCreated {
		t.Fatalf("activity = %d %s", rec.Code, rec.Body)
	}
	snapshot := func() string {
		run := f.do(t, "GET", runPath(f.run.ID, ""), f.aliceTok, "").Body.String()
		activity := f.do(t, "GET", runPath(f.run.ID, "/activity"), f.aliceTok, "").Body.String()
		roster, _ := json.Marshal(f.roster(t, f.aliceTok, "general"))
		return run + activity + string(roster)
	}
	before := snapshot()
	for _, kind := range []schema.InterventionKind{schema.InterventionKindCancel, schema.InterventionKindMessage} {
		for _, end := range []schema.InterventionState{schema.InterventionStateAcknowledged, schema.InterventionStateApplied, schema.InterventionStateFailed} {
			iv := f.mustAsk(t, f.aliceTok, f.run.ID, kind)
			f.mustAck(t, iv.ID, end, "reason")
			if end == schema.InterventionStateAcknowledged {
				f.mustAck(t, iv.ID, schema.InterventionStateApplied, "")
			}
		}
	}
	if after := snapshot(); after != before {
		t.Errorf("the run, its activity or the roster changed:\nbefore %s\nafter  %s", before, after)
	}
}

// No audit row holds the words a person said or the reason an adapter gave,
// whichever the route and whether or not the request was refused.
func TestInterventionAuditNeverHoldsWhatWasSaid(t *testing.T) {
	f := newInterventionFixture(t)
	const said, because, refused = "SAID-ONLY-TO-THE-RUN", "BECAUSE-ONLY-TO-THE-PERSON", "NEVER-ACCEPTED"
	// One with the default words, one with ours.
	f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindMessage)
	msg := f.ask(t, f.aliceTok, f.run.ID, schema.InterventionKindMessage, said)
	if msg.Code != http.StatusCreated {
		t.Fatal(msg.Body)
	}
	id := validIntervention(t, msg).ID
	f.mustAck(t, id, schema.InterventionStateFailed, because)
	// A refusal for an undeclared kind, with words.
	rival := f.create(t, f.rivalTok, `{"client":"adapter","interventions":["cancel"]}`).ID
	rivalRun := f.start(t, f.rivalTok, rival, "general", 0, "t")
	assertErrorBody(t, f.ask(t, f.aliceTok, rivalRun.ID, schema.InterventionKindMessage, refused), http.StatusConflict, schema.ErrorCodeInterventionUnsupported)
	rows := f.audit(t)
	var text strings.Builder
	for _, e := range rows {
		fmt.Fprintf(&text, "%s|%s|%s|%s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	for _, words := range []string{said, because, refused, "please also run the linter"} {
		if strings.Contains(text.String(), words) {
			t.Errorf("an audit row holds %q:\n%s", words, text.String())
		}
	}
	if len(f.auditRows(t, store.AuditInterventionRequested)) != 2 || len(f.auditRows(t, store.AuditInterventionAnswered)) != 1 ||
		len(f.auditRows(t, store.AuditInterventionUnsupported)) != 1 {
		t.Errorf("audit rows: %s", text.String())
	}
}
