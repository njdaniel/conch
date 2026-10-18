package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The MCP tools for sessions, runs and interventions (issue #267). Each is a
// front on the function its REST route calls, so the tests here hold the two
// fronts to each other: every refusal an agent can reach over REST is the
// same refusal over MCP, and what a tool returns is the document the route
// returns.

// mcpToolResult is one tools/call answer: "ok" with the structured content,
// or the code of the schema.Error the tool refused with.
type mcpToolResult struct {
	code string
	out  json.RawMessage
}

func (f *interventionFixture) tool(t *testing.T, token, name string, args map[string]any) mcpToolResult {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: http %d %s", name, rec.Code, rec.Body)
	}
	var response struct {
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
			Content           []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("%s: decode %s: %v", name, rec.Body, err)
	}
	if !response.Result.IsError {
		return mcpToolResult{code: "ok", out: response.Result.StructuredContent}
	}
	var serr schema.Error
	if len(response.Result.Content) == 0 || json.Unmarshal([]byte(response.Result.Content[0].Text), &serr) != nil || serr.Code == "" {
		t.Fatalf("%s: a refusal that is not a schema.Error: %s", name, rec.Body)
	}
	return mcpToolResult{code: serr.Code}
}

// mustTool calls a tool that must succeed and decodes what it returned.
func mustTool[T any](t *testing.T, f *interventionFixture, token, name string, args map[string]any) T {
	t.Helper()
	res := f.tool(t, token, name, args)
	if res.code != "ok" {
		t.Fatalf("%s(%v) = %s", name, args, res.code)
	}
	var out T
	if err := json.Unmarshal(res.out, &out); err != nil {
		t.Fatalf("%s: decode %s: %v", name, res.out, err)
	}
	return out
}

// restCode is a REST answer reduced as tool is: "ok" or the error code.
func restCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code < 300 {
		return "ok"
	}
	var serr schema.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &serr); err != nil || serr.Code == "" {
		t.Fatalf("a REST refusal that is not a schema.Error: %d %s", rec.Code, rec.Body)
	}
	return serr.Code
}

func jsonBody(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// An agent goes through a whole run over MCP alone, and every document a tool
// returns is the one the matching REST read returns, valid. What it changes is
// audited with the agent as the actor, as over REST.
func TestMCPSessionToolsWholeRun(t *testing.T) {
	f := newInterventionFixture(t)
	sess := mustTool[schema.SessionV1](t, f, f.botTok, "register_session", map[string]any{
		"client": "adapter", "runtime": "claude-code", "model": "opus", "interventions": []string{"message", "cancel"},
	})
	if err := sess.Validate(); err != nil || sess.PrincipalID != f.bot.ID || sess.State != schema.SessionStateOnline || sess.Runtime != "claude-code" {
		t.Fatalf("register_session = %+v, %v", sess, err)
	}
	run := mustTool[schema.RunV1](t, f, f.botTok, "start_run", map[string]any{
		"session_id": sess.ID, "channel": "general", "message_id": f.whisper, "task": "answer alice",
	})
	if err := run.Validate(); err != nil || run.SessionID != sess.ID || run.MessageID != f.whisper || run.State != schema.RunStateQueued {
		t.Fatalf("start_run = %+v, %v", run, err)
	}
	run = mustTool[schema.RunV1](t, f, f.botTok, "update_run", map[string]any{"run_id": run.ID, "state": "running"})
	if got := validRun(t, f.do(t, "GET", runPath(run.ID, ""), f.botTok, "")); !jsonEqual(t, got, run) {
		t.Errorf("update_run returned %+v, GET returns %+v", run, got)
	}
	entry := mustTool[schema.AgentActivityV1](t, f, f.botTok, "post_activity", map[string]any{
		"run_id": run.ID, "kind": "command_finished", "summary": "ran the tests", "data": map[string]any{"passed": 12},
	})
	if err := entry.Validate(); err != nil || string(entry.Data) != `{"passed":12}` {
		t.Fatalf("post_activity = %+v (%s), %v", entry, entry.Data, err)
	}
	listed := validActivityList(t, f.do(t, "GET", runPath(run.ID, "/activity"), f.aliceTok, ""))
	if len(listed.Activity) != 1 || !jsonEqual(t, listed.Activity[0], entry) {
		t.Errorf("alice reads %+v, post_activity returned %+v", listed.Activity, entry)
	}

	// Alice intervenes over REST; the agent polls and answers over MCP.
	asked := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindMessage)
	polled := mustTool[schema.ListInterventionsResponseV1](t, f, f.botTok, "poll_interventions", map[string]any{"session_id": sess.ID, "state": "requested"})
	if len(polled.Interventions) != 1 || !jsonEqual(t, polled.Interventions[0], asked) {
		t.Fatalf("poll_interventions = %+v, want [%+v]", polled.Interventions, asked)
	}
	answered := mustTool[schema.InterventionV1](t, f, f.botTok, "ack_intervention", map[string]any{"intervention_id": asked.ID, "state": "applied"})
	if err := answered.Validate(); err != nil || answered.State != schema.InterventionStateApplied {
		t.Fatalf("ack_intervention = %+v, %v", answered, err)
	}
	if got := validIntervention(t, f.do(t, "GET", interventionPath(asked.ID, ""), f.aliceTok, "")); !jsonEqual(t, got, answered) {
		t.Errorf("alice reads %+v, ack_intervention returned %+v", got, answered)
	}
	// Empty is an array, not null.
	if empty := f.tool(t, f.botTok, "poll_interventions", map[string]any{"session_id": sess.ID, "state": "requested"}); !strings.Contains(string(empty.out), `"interventions":[]`) {
		t.Errorf("an empty poll = %s", empty.out)
	}

	beat := mustTool[schema.SessionV1](t, f, f.botTok, "heartbeat", map[string]any{"session_id": sess.ID})
	if beat.ID != sess.ID || beat.CurrentRun != run.ID {
		t.Errorf("heartbeat = %+v; want session %d with current run %d", beat, sess.ID, run.ID)
	}
	if done := mustTool[schema.RunV1](t, f, f.botTok, "update_run", map[string]any{"run_id": run.ID, "state": "completed", "outcome": "answered"}); done.State != schema.RunStateCompleted || done.EndedAt == nil {
		t.Errorf("update_run completed = %+v", done)
	}
	if res := f.tool(t, f.botTok, "end_session", map[string]any{"session_id": sess.ID}); res.code != "ok" {
		t.Fatalf("end_session = %s", res.code)
	}
	if res := f.tool(t, f.botTok, "heartbeat", map[string]any{"session_id": sess.ID}); res.code != schema.ErrorCodeSessionNotFound {
		t.Errorf("heartbeat after end_session = %s", res.code)
	}

	// The audit names the agent, never "system".
	actor := fmt.Sprintf("principal:%d", f.bot.ID)
	for _, action := range []string{store.AuditSessionStarted, store.AuditSessionEnded, store.AuditRunStarted, store.AuditRunEnded, store.AuditInterventionAnswered} {
		rows := f.auditRows(t, action)
		if len(rows) == 0 || rows[len(rows)-1].Actor != actor {
			t.Errorf("%s audit rows = %+v; want the last by %s", action, rows, actor)
		}
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x, y)
}

// Each of the eight tools needs its capability. An agent whose manifest does
// not grant it is refused before anything is read, the refusal is audited as
// for the other tools (subject mcp:<tool>, capability_not_granted), and
// nothing is written.
func TestMCPSessionToolsCapabilityGate(t *testing.T) {
	f := newInterventionFixture(t)
	iv := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel)
	tools := map[string]map[string]any{
		"register_session":   {"client": "adapter"},
		"heartbeat":          {"session_id": f.sess},
		"end_session":        {"session_id": f.sess},
		"start_run":          {"session_id": f.sess, "channel": "general", "task": "t"},
		"update_run":         {"run_id": f.run.ID, "state": "blocked"},
		"post_activity":      {"run_id": f.run.ID, "kind": "command_started", "summary": "s"},
		"poll_interventions": {"session_id": f.sess},
		"ack_intervention":   {"intervention_id": iv.ID, "state": "acknowledged"},
	}
	need := schema.MCPToolCapabilities()
	for name, args := range tools {
		t.Run(name, func(t *testing.T) {
			// Everything but the one capability the tool needs.
			var caps []schema.Capability
			for _, c := range schema.Capabilities() {
				if c != need[name] {
					caps = append(caps, c)
				}
			}
			f.grant(t, f.bot.ID, caps, nil, map[int64][]schema.ChannelPermission{
				f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionWhisper, schema.ChannelPermissionPostNet},
			})
			before := f.auditCount(t)
			if res := f.tool(t, f.botTok, name, args); res.code != "forbidden" {
				t.Fatalf("%s without %s = %s, want forbidden", name, need[name], res.code)
			}
			rows := f.auditRows(t, "access_denied")
			last := rows[len(rows)-1]
			if last.Subject != "mcp:"+name || !strings.Contains(last.Detail, "capability="+string(need[name])) || !strings.Contains(last.Detail, "reason="+denyCapability) {
				t.Errorf("denial audited as %+v", last)
			}
			if got := f.auditCount(t); got != before+1 {
				t.Errorf("the refused call wrote %d audit rows, want only the denial", got-before)
			}
		})
	}
	// An agent with no manifest at all, the same.
	bare, err := f.srv.store.CreatePrincipal(t.Context(), store.PrincipalAgent, "bare")
	if err != nil {
		t.Fatal(err)
	}
	_, bareTok, err := f.srv.store.CreateCredential(t.Context(), "system", bare.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.join(t, f.general.ID, bare.ID)
	for name, args := range tools {
		if res := f.tool(t, bareTok, name, args); res.code != "forbidden" {
			t.Errorf("%s without a manifest = %s, want forbidden", name, res.code)
		}
	}
}

func (f *interventionFixture) auditCount(t *testing.T) int {
	t.Helper()
	events, err := f.srv.store.ListAuditEvents(t.Context(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	return len(events)
}

// parityCase is one request an agent can make both ways, and the refusal
// both must give.
type parityCase struct {
	name         string
	token        string
	method, path string
	body         string
	tool         string
	args         map[string]any
	want         string
}

func (f *interventionFixture) checkParity(t *testing.T, cases []parityCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := f.auditCount(t)
			rest := restCode(t, f.do(t, tc.method, tc.path, tc.token, tc.body))
			mid := f.auditCount(t)
			mcp := f.tool(t, tc.token, tc.tool, tc.args).code
			if rest != tc.want || mcp != tc.want {
				t.Errorf("REST %s %s = %s, MCP %s = %s; want %s from both", tc.method, tc.path, rest, tc.tool, mcp, tc.want)
			}
			// What one front audits for a refusal, the other does too.
			if rest, mcp := mid-before, f.auditCount(t)-mid; rest != mcp {
				t.Errorf("REST wrote %d audit rows, MCP %d", rest, mcp)
			}
		})
	}
}

// Every refusal of #265's, #266's and #287's routes that an agent can reach is
// the same refusal from the matching tool.
func TestMCPSessionToolsRefuseAsRESTDoes(t *testing.T) {
	f := newInterventionFixture(t)
	ended := f.session(t, f.botTok)
	if rec := f.do(t, "DELETE", sessionPath(ended, ""), f.botTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("end = %d", rec.Code)
	}
	rivalSess := f.session(t, f.rivalTok)
	done := f.newRun(t)
	f.mustState(t, f.botTok, done.ID, schema.RunStateCompleted)
	iv := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindCancel)
	answered := f.mustAsk(t, f.aliceTok, f.run.ID, schema.InterventionKindMessage)
	f.mustAck(t, answered.ID, schema.InterventionStateApplied, "")

	notFound := map[string]string{"session": schema.ErrorCodeSessionNotFound, "run": schema.ErrorCodeRunNotFound, "intervention": schema.ErrorCodeInterventionNotFound}
	cases := []parityCase{
		// register_session
		{"register: no client", f.botTok, "POST", "/v1/sessions", `{"client":""}`, "register_session", map[string]any{"client": ""}, "invalid_request"},
		{"register: unknown intervention kind", f.botTok, "POST", "/v1/sessions", `{"client":"a","interventions":["reboot"]}`, "register_session", map[string]any{"client": "a", "interventions": []string{"reboot"}}, "invalid_request"},
		{"register: a control character in the client", f.botTok, "POST", "/v1/sessions", `{"client":"a\u001b[2J"}`, "register_session", map[string]any{"client": "a\x1b[2J"}, "invalid_request"},
	}
	for _, route := range []struct{ tool, method, suffix string }{{"heartbeat", "POST", "/heartbeat"}, {"end_session", "DELETE", ""}} {
		for _, s := range []struct {
			name string
			id   int64
		}{{"another credential's", rivalSess}, {"an ended", ended}, {"a missing", 99999}, {"a zero", 0}} {
			cases = append(cases, parityCase{route.tool + ": " + s.name + " session", f.botTok, route.method, sessionPath(s.id, route.suffix), "", route.tool, map[string]any{"session_id": s.id}, notFound["session"]})
		}
	}
	start := func(name string, sess int64, req schema.CreateRunRequestV1, want string) parityCase {
		args := map[string]any{"session_id": sess, "channel": req.Channel, "task": req.Task}
		if req.MessageID != 0 {
			args["message_id"] = req.MessageID
		}
		return parityCase{"start_run: " + name, f.botTok, "POST", sessionPath(sess, "/runs"), jsonBody(req), "start_run", args, want}
	}
	cases = append(cases,
		start("another credential's session", rivalSess, schema.CreateRunRequestV1{Channel: "general", Task: "t"}, notFound["session"]),
		start("an ended session", ended, schema.CreateRunRequestV1{Channel: "general", Task: "t"}, notFound["session"]),
		start("a zero session", 0, schema.CreateRunRequestV1{Channel: "general", Task: "t"}, notFound["session"]),
		start("no task", f.sess, schema.CreateRunRequestV1{Channel: "general"}, "invalid_request"),
		start("a task of two lines", f.sess, schema.CreateRunRequestV1{Channel: "general", Task: "a\nb"}, "invalid_request"),
		start("an unknown channel", f.sess, schema.CreateRunRequestV1{Channel: "nosuch", Task: "t"}, "channel_not_found"),
		start("a channel it is not in", f.sess, schema.CreateRunRequestV1{Channel: "elsewhere", Task: "t"}, "channel_not_found"),
		start("a message in another channel", f.sess, schema.CreateRunRequestV1{Channel: "general", MessageID: f.privateMsg, Task: "t"}, "channel_not_found"),
		start("a missing message", f.sess, schema.CreateRunRequestV1{Channel: "general", MessageID: 99999, Task: "t"}, "channel_not_found"),
	)
	// rival is in general but outside alice's whisper to bot.
	rivalOwn := f.session(t, f.rivalTok)
	cases = append(cases, parityCase{"start_run: a whisper it cannot see", f.rivalTok, "POST", sessionPath(rivalOwn, "/runs"),
		jsonBody(schema.CreateRunRequestV1{Channel: "general", MessageID: f.whisper, Task: "t"}), "start_run",
		map[string]any{"session_id": rivalOwn, "channel": "general", "message_id": f.whisper, "task": "t"}, "channel_not_found"})

	update := func(name, token string, run int64, state string, want string) parityCase {
		return parityCase{"update_run: " + name, token, "POST", runPath(run, "/state"), jsonBody(map[string]string{"state": state}), "update_run", map[string]any{"run_id": run, "state": state}, want}
	}
	cases = append(cases,
		update("another credential's run", f.rivalTok, f.run.ID, "blocked", notFound["run"]),
		update("a missing run", f.botTok, 99999, "blocked", notFound["run"]),
		update("a zero run", f.botTok, 0, "blocked", notFound["run"]),
		update("an unknown state", f.botTok, f.run.ID, "paused", "invalid_request"),
		update("an ended run", f.botTok, done.ID, "running", schema.ErrorCodeInvalidTransition),
	)
	activity := func(name, token string, run int64, kind, summary string, want string) parityCase {
		return parityCase{"post_activity: " + name, token, "POST", runPath(run, "/activity"), jsonBody(map[string]string{"kind": kind, "summary": summary}), "post_activity", map[string]any{"run_id": run, "kind": kind, "summary": summary}, want}
	}
	cases = append(cases,
		activity("another credential's run", f.rivalTok, f.run.ID, "command_started", "s", notFound["run"]),
		activity("a zero run", f.botTok, 0, "command_started", "s", notFound["run"]),
		activity("an unknown kind", f.botTok, f.run.ID, "thought", "s", "invalid_request"),
		activity("no summary", f.botTok, f.run.ID, "command_started", "", "invalid_request"),
		activity("an ended run", f.botTok, done.ID, "command_started", "s", schema.ErrorCodeInvalidTransition),
	)
	poll := func(name, token string, sess int64, query string, args map[string]any, want string) parityCase {
		args["session_id"] = sess
		return parityCase{"poll_interventions: " + name, token, "GET", sessionPath(sess, "/interventions"+query), "", "poll_interventions", args, want}
	}
	cases = append(cases,
		poll("another credential's session", f.botTok, rivalSess, "", map[string]any{}, notFound["session"]),
		poll("an ended session", f.botTok, ended, "", map[string]any{}, notFound["session"]),
		poll("a zero session", f.botTok, 0, "", map[string]any{}, notFound["session"]),
		poll("an unknown state", f.botTok, f.sess, "?state=waiting", map[string]any{"state": "waiting"}, "invalid_request"),
		poll("a negative after", f.botTok, f.sess, "?after=-1", map[string]any{"after": -1}, "invalid_request"),
		poll("a zero limit", f.botTok, f.sess, "?limit=0", map[string]any{"limit": 0}, "invalid_request"),
		poll("a limit over 100", f.botTok, f.sess, "?limit=101", map[string]any{"limit": 101}, "invalid_request"),
	)
	ack := func(name, token string, id int64, state, reason string, want string) parityCase {
		args := map[string]any{"intervention_id": id, "state": state}
		if reason != "" {
			args["reason"] = reason
		}
		return parityCase{"ack_intervention: " + name, token, "POST", interventionPath(id, "/ack"), ackBody(schema.InterventionState(state), reason), "ack_intervention", args, want}
	}
	cases = append(cases,
		ack("another credential's run", f.rivalTok, iv.ID, "acknowledged", "", notFound["intervention"]),
		ack("a missing intervention", f.botTok, 99999, "acknowledged", "", notFound["intervention"]),
		ack("a zero intervention", f.botTok, 0, "acknowledged", "", notFound["intervention"]),
		ack("requested is not an answer", f.botTok, iv.ID, "requested", "", "invalid_request"),
		ack("failed without a reason", f.botTok, iv.ID, "failed", "", "invalid_request"),
		ack("a reason with an escape", f.botTok, iv.ID, "failed", "no\x1b[2J", "invalid_request"),
		ack("an answered one", f.botTok, answered.ID, "acknowledged", "", schema.ErrorCodeInvalidTransition),
	)
	f.checkParity(t, cases)

	// The manifest's channel gate: a run answering a whisper needs whisper in
	// the channel, on both fronts, for a report and for an answer alike.
	f.grant(t, f.bot.ID, schema.Capabilities(), nil, map[int64][]schema.ChannelPermission{
		f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost},
	})
	f.checkParity(t, []parityCase{
		update("without whisper", f.botTok, f.run.ID, "blocked", "forbidden"),
		activity("without whisper", f.botTok, f.run.ID, "command_started", "s", "forbidden"),
		ack("without whisper", f.botTok, iv.ID, "acknowledged", "", "forbidden"),
		start("without whisper", f.sess, schema.CreateRunRequestV1{Channel: "general", MessageID: f.whisper, Task: "t"}, "channel_not_found"),
	})
}

// Both fronts spend the same budget: an agent at its runs.manage limit is
// refused on either, for the own-session poll and for a report.
func TestMCPSessionToolsShareTheRateLimit(t *testing.T) {
	f := newInterventionFixture(t)
	perms := map[int64][]schema.ChannelPermission{
		f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionWhisper},
	}
	f.grant(t, f.bot.ID, schema.Capabilities(), []schema.RateLimit{{Capability: schema.CapabilityRunsManage, Max: 2, WindowSeconds: 3600}}, perms)
	if rec := f.poll(t, f.botTok, f.sess, ""); rec.Code != http.StatusOK {
		t.Fatalf("first poll = %d %s", rec.Code, rec.Body)
	}
	if res := f.tool(t, f.botTok, "poll_interventions", map[string]any{"session_id": f.sess}); res.code != "ok" {
		t.Fatalf("second poll = %s", res.code)
	}
	f.checkParity(t, []parityCase{
		{"poll over the limit", f.botTok, "GET", sessionPath(f.sess, "/interventions"), "", "poll_interventions", map[string]any{"session_id": f.sess}, schema.ErrorCodeRateLimited},
		{"report over the limit", f.botTok, "POST", runPath(f.run.ID, "/activity"), `{"kind":"command_started","summary":"s"}`, "post_activity", map[string]any{"run_id": f.run.ID, "kind": "command_started", "summary": "s"}, schema.ErrorCodeRateLimited},
	})
	// sessions.manage is a budget of its own.
	if res := f.tool(t, f.botTok, "heartbeat", map[string]any{"session_id": f.sess}); res.code != "ok" {
		t.Errorf("heartbeat while runs.manage is spent = %s", res.code)
	}
}

// The bound of 16 live sessions per credential holds on both fronts, and they
// count together.
func TestMCPRegisterSessionBound(t *testing.T) {
	f := newInterventionFixture(t)
	for i := 0; i < 8; i++ {
		f.session(t, f.rivalTok)
		if res := f.tool(t, f.rivalTok, "register_session", map[string]any{"client": "adapter"}); res.code != "ok" {
			t.Fatalf("register_session %d = %s", i, res.code)
		}
	}
	f.checkParity(t, []parityCase{
		{"the 17th", f.rivalTok, "POST", "/v1/sessions", `{"client":"adapter"}`, "register_session", map[string]any{"client": "adapter"}, schema.ErrorCodeTooManySessions},
	})
}

// A deprecated static --mcp-token has no credential, and a session belongs to
// one: the session tools refuse it as REST refuses a caller with
// authentication off, and the other tools still work for it.
func TestMCPSessionToolsNeedAnIssuedCredential(t *testing.T) {
	f := newInterventionFixture(t)
	f.srv.cfg.MCPBearerTokens = map[string]int64{"static-bot": f.bot.ID}
	for name, args := range map[string]map[string]any{
		"register_session":   {"client": "adapter"},
		"heartbeat":          {"session_id": f.sess},
		"end_session":        {"session_id": f.sess},
		"start_run":          {"session_id": f.sess, "channel": "general", "task": "t"},
		"update_run":         {"run_id": f.run.ID, "state": "blocked"},
		"post_activity":      {"run_id": f.run.ID, "kind": "command_started", "summary": "s"},
		"poll_interventions": {"session_id": f.sess},
		"ack_intervention":   {"intervention_id": 1, "state": "acknowledged"},
	} {
		if res := f.tool(t, "static-bot", name, args); res.code != schema.ErrorCodeSessionsRequireAuth {
			t.Errorf("%s with a static token = %s, want %s", name, res.code, schema.ErrorCodeSessionsRequireAuth)
		}
	}
	if res := f.tool(t, "static-bot", "read_channel", map[string]any{"channel": "general"}); res.code != "ok" {
		t.Errorf("read_channel with a static token = %s", res.code)
	}
}

// What the README says of post_activity's data: the MCP library decodes and
// re-encodes every call's arguments, so object keys come out sorted and
// numbers pass through float64, where REST stores the JSON as sent. If the
// library ever keeps the bytes, this fails and the README should say so.
func TestMCPPostActivityDataIsReencoded(t *testing.T) {
	f := newInterventionFixture(t)
	const sent = `{"b":1,"a":9007199254740993}`
	rest := validActivity(t, f.do(t, "POST", runPath(f.run.ID, "/activity"), f.botTok, `{"kind":"command_finished","summary":"s","data":`+sent+`}`))
	if string(rest.Data) != sent {
		t.Errorf("REST stored %s, want %s as sent", rest.Data, sent)
	}
	over := mustTool[schema.AgentActivityV1](t, f, f.botTok, "post_activity", map[string]any{"run_id": f.run.ID, "kind": "command_finished", "summary": "s", "data": json.RawMessage(sent)})
	if want := `{"a":9007199254740992,"b":1}`; string(over.Data) != want {
		t.Errorf("MCP stored %s, want %s (keys sorted, through float64)", over.Data, want)
	}
}

// What an agent changes over MCP reaches the events socket (issue #269) as
// what it changes over REST does: the notify calls are in the functions both
// fronts call, not in the REST handlers.
func TestMCPChangesReachTheEventsSocket(t *testing.T) {
	f := newInterventionFixture(t)
	base := wsTestServer(t, f.srv)
	c := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
	c.next() // the roster on connect

	sess := mustTool[schema.SessionV1](t, f, f.botTok, "register_session", map[string]any{"client": "adapter", "interventions": []string{"cancel"}})
	hasSession := func(id int64) func(schema.SessionRosterV1) bool {
		return func(r schema.SessionRosterV1) bool {
			for _, s := range r.Sessions {
				if s.ID == id {
					return true
				}
			}
			return false
		}
	}
	c.until("the registered session in a roster", isRoster(hasSession(sess.ID)))

	run := mustTool[schema.RunV1](t, f, f.botTok, "start_run", map[string]any{"session_id": sess.ID, "channel": "general", "message_id": f.whisper, "task": "over mcp"})
	c.until("the started run", isRun(func(r schema.RunV1) bool { return r.ID == run.ID && r.State == schema.RunStateQueued }))
	mustTool[schema.RunV1](t, f, f.botTok, "update_run", map[string]any{"run_id": run.ID, "state": "running"})
	c.until("the run running", isRun(func(r schema.RunV1) bool { return r.ID == run.ID && r.State == schema.RunStateRunning }))
	entry := mustTool[schema.AgentActivityV1](t, f, f.botTok, "post_activity", map[string]any{"run_id": run.ID, "kind": "command_started", "summary": "over mcp"})
	c.until("the activity entry", func(fr eventFrame) bool {
		return fr.schema == schema.AgentActivitySchemaV1 && fr.activity.ID == entry.ID
	})
	iv := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel)
	mustTool[schema.InterventionV1](t, f, f.botTok, "ack_intervention", map[string]any{"intervention_id": iv.ID, "state": "applied"})
	c.until("the answered intervention", func(fr eventFrame) bool {
		return fr.schema == schema.InterventionSchemaV1 && fr.intervention.ID == iv.ID && fr.intervention.State == schema.InterventionStateApplied
	})
	if res := f.tool(t, f.botTok, "end_session", map[string]any{"session_id": sess.ID}); res.code != "ok" {
		t.Fatalf("end_session = %s", res.code)
	}
	c.until("a roster without the ended session", isRoster(func(r schema.SessionRosterV1) bool { return !hasSession(sess.ID)(r) }))
}
