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

// Runs and activity, end to end through the handlers (issue #266). Every
// RunV1, AgentActivityV1 and SessionV1 a handler sends is decoded and run
// through its Validate here, so a document the schema refuses fails the test
// that produced it.

// runFixture is the session fixture (root the operator, alice and bob humans,
// bot an agent, mute an agent without runs.manage, all in "general"; carol in
// no channel) and besides:
//
//   - "test-private", whose members are alice and bot;
//   - "elsewhere", whose only member is alice;
//   - rival, an agent in general that may report runs, read, post and whisper
//     there: a second agent with runs.manage in the same channel;
//   - shy, an agent in general that may report runs, read and post there, but
//     not whisper;
//   - bot's manifest: every capability; in general read, post, whisper and
//     post_net, and in test-private read and post. No whisper_agent.
//
// whisper is alice's whisper to bot in general, and private is a channel-wide
// message in test-private.
type runFixture struct {
	*sessionFixture
	private, elsewhere store.Channel
	rival, shy         store.Principal
	rivalTok, shyTok   string
	whisper            int64
	privateMsg         int64
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	ctx := context.Background()
	f := &runFixture{sessionFixture: newSessionFixture(t)}
	var err error
	if f.private, err = f.srv.store.CreateChannel(ctx, "test-private"); err != nil {
		t.Fatal(err)
	}
	if f.elsewhere, err = f.srv.store.CreateChannel(ctx, "elsewhere"); err != nil {
		t.Fatal(err)
	}
	f.join(t, f.private.ID, f.alice.ID, f.bot.ID)
	f.join(t, f.elsewhere.ID, f.alice.ID)
	mk := func(name string) (store.Principal, string) {
		p, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalAgent, name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := f.srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.join(t, f.general.ID, p.ID)
		return p, tok
	}
	f.rival, f.rivalTok = mk("rival")
	f.shy, f.shyTok = mk("shy")
	all := schema.Capabilities()
	f.grant(t, f.bot.ID, all, nil, map[int64][]schema.ChannelPermission{
		f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionWhisper, schema.ChannelPermissionPostNet},
		f.private.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost},
	})
	f.grant(t, f.rival.ID, all, nil, map[int64][]schema.ChannelPermission{
		f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionWhisper},
	})
	f.grant(t, f.shy.ID, all, nil, map[int64][]schema.ChannelPermission{
		f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost},
	})
	f.whisper = f.whisperTo(t, f.alice, "is the deploy safe?", f.bot)
	m, err := f.srv.store.InsertMessageV1(ctx, f.private.ID, f.alice.ID, "private question", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.privateMsg = m.ID
	return f
}

func (f *runFixture) join(t *testing.T, channelID int64, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if _, err := f.srv.store.AddChannelMember(context.Background(), "system", channelID, id, 0); err != nil {
			t.Fatal(err)
		}
	}
}

// grant replaces an agent's manifest.
func (f *runFixture) grant(t *testing.T, agentID int64, caps []schema.Capability, limits []schema.RateLimit, perms map[int64][]schema.ChannelPermission) {
	t.Helper()
	req := schema.PutAgentManifestRequestV1{DisplayName: "test agent", Tier: schema.AgentTierC, Capabilities: caps, RateLimits: limits}
	for id, p := range perms {
		req.Channels = append(req.Channels, schema.ChannelGrant{ChannelID: id, Permissions: p})
	}
	if _, _, err := f.srv.store.PutAgentManifest(context.Background(), "system", agentID, req); err != nil {
		t.Fatalf("PutAgentManifest: %v", err)
	}
}

func (f *runFixture) whisperTo(t *testing.T, from store.Principal, body string, to ...store.Principal) int64 {
	t.Helper()
	ids := []int64{}
	for _, p := range to {
		ids = append(ids, p.ID)
	}
	m, _, err := f.srv.store.InsertScopedMessage(context.Background(), store.ScopedPost{
		ChannelID: f.general.ID, AuthorID: from.ID, Body: body,
		Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: ids}.Normalize(from.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

// session makes an agent session with token through the API.
func (f *runFixture) session(t *testing.T, token string) int64 {
	t.Helper()
	return f.create(t, token, agentBody).ID
}

func runBody(channel string, message int64, task string) string {
	b, _ := json.Marshal(schema.CreateRunRequestV1{Channel: channel, MessageID: message, Task: task})
	return string(b)
}

func runPath(id int64, suffix string) string { return fmt.Sprintf("/v1/runs/%d%s", id, suffix) }

// validRun decodes rec as a RunV1 and fails unless it is valid.
func validRun(t *testing.T, rec *httptest.ResponseRecorder) schema.RunV1 {
	t.Helper()
	doc := decodeBody[schema.RunV1](t, rec)
	if err := doc.Validate(); err != nil {
		t.Fatalf("the server sent an invalid run: %v\n%s", err, rec.Body)
	}
	return doc
}

func validActivity(t *testing.T, rec *httptest.ResponseRecorder) schema.AgentActivityV1 {
	t.Helper()
	doc := decodeBody[schema.AgentActivityV1](t, rec)
	if err := doc.Validate(); err != nil {
		t.Fatalf("the server sent an invalid activity entry: %v\n%s", err, rec.Body)
	}
	return doc
}

func validRunList(t *testing.T, rec *httptest.ResponseRecorder) schema.ListRunsResponseV1 {
	t.Helper()
	if !strings.Contains(rec.Body.String(), `"runs":[`) {
		t.Fatalf("runs is not an array: %s", rec.Body)
	}
	doc := decodeBody[schema.ListRunsResponseV1](t, rec)
	for _, r := range doc.Runs {
		if err := r.Validate(); err != nil {
			t.Fatalf("the server listed an invalid run: %v\n%s", err, rec.Body)
		}
	}
	return doc
}

func validActivityList(t *testing.T, rec *httptest.ResponseRecorder) schema.ListActivityResponseV1 {
	t.Helper()
	if !strings.Contains(rec.Body.String(), `"activity":[`) {
		t.Fatalf("activity is not an array: %s", rec.Body)
	}
	doc := decodeBody[schema.ListActivityResponseV1](t, rec)
	for _, a := range doc.Activity {
		if err := a.Validate(); err != nil {
			t.Fatalf("the server listed an invalid activity entry: %v\n%s", err, rec.Body)
		}
	}
	return doc
}

// start starts a run as bot on sess and fails unless it is created.
func (f *runFixture) start(t *testing.T, token string, sess int64, channel string, message int64, task string) schema.RunV1 {
	t.Helper()
	rec := f.do(t, "POST", sessionPath(sess, "/runs"), token, runBody(channel, message, task))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start run = %d %s", rec.Code, rec.Body)
	}
	return validRun(t, rec)
}

func (f *runFixture) setState(t *testing.T, token string, id int64, state schema.RunState) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, "POST", runPath(id, "/state"), token, fmt.Sprintf(`{"state":%q}`, state))
}

func (f *runFixture) mustState(t *testing.T, token string, id int64, state schema.RunState) schema.RunV1 {
	t.Helper()
	rec := f.setState(t, token, id, state)
	if rec.Code != http.StatusOK {
		t.Fatalf("set state %s = %d %s", state, rec.Code, rec.Body)
	}
	return validRun(t, rec)
}

const activityBody = `{"kind":"command_started","summary":"go test ./...","data":{"cmd":"go test","args":["./..."]}}`

// listedRunIDs is the runs of channel token may see.
func (f *runFixture) listedRunIDs(t *testing.T, token, channel string) []int64 {
	t.Helper()
	rec := f.do(t, "GET", "/v1/runs?channel="+channel, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list runs = %d %s", rec.Code, rec.Body)
	}
	ids := []int64{}
	for _, r := range validRunList(t, rec).Runs {
		ids = append(ids, r.ID)
	}
	return ids
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// assertHidden checks that token gets, on both read routes for run id, exactly
// the answer a run that does not exist gets, and that the run is not listed in
// any channel token may list.
func (f *runFixture) assertHidden(t *testing.T, token string, id int64) {
	t.Helper()
	for _, suffix := range []string{"", "/activity"} {
		got := f.do(t, "GET", runPath(id, suffix), token, "")
		missing := f.do(t, "GET", runPath(999999, suffix), token, "")
		assertErrorBody(t, got, http.StatusNotFound, schema.ErrorCodeRunNotFound)
		if got.Body.String() != missing.Body.String() || got.Code != missing.Code {
			t.Errorf("GET %s: hidden run answers %d %s, a missing one %d %s", runPath(id, suffix), got.Code, got.Body, missing.Code, missing.Body)
		}
	}
	for _, channel := range []string{"general", "test-private", "elsewhere"} {
		if rec := f.do(t, "GET", "/v1/runs?channel="+channel, token, ""); rec.Code == http.StatusOK {
			if containsID(f.listedRunIDs(t, token, channel), id) {
				t.Errorf("run %d is listed in %s for a reader who may not see it", id, channel)
			}
		}
	}
}

// assertVisible checks that token reads run id and its activity.
func (f *runFixture) assertVisible(t *testing.T, token string, id int64, channel string) {
	t.Helper()
	rec := f.do(t, "GET", runPath(id, ""), token, "")
	if rec.Code != http.StatusOK || validRun(t, rec).ID != id {
		t.Errorf("GET run %d = %d %s", id, rec.Code, rec.Body)
	}
	rec = f.do(t, "GET", runPath(id, "/activity"), token, "")
	if rec.Code != http.StatusOK {
		t.Errorf("GET activity of %d = %d %s", id, rec.Code, rec.Body)
	} else {
		validActivityList(t, rec)
	}
	if !containsID(f.listedRunIDs(t, token, channel), id) {
		t.Errorf("run %d is not listed for a reader who may see it", id)
	}
}

// sessionView is the roster entry for session id as token sees it.
func (f *runFixture) sessionView(t *testing.T, token string, id int64) schema.SessionV1 {
	t.Helper()
	s, ok := f.rosterByID(t, token)[id]
	if !ok {
		t.Fatalf("session %d is not in the roster", id)
	}
	return s
}

// The whole story the issue is about: an agent answers a whisper, and the run,
// its activity and the roster's current_run and task reach exactly the
// whisper's readers. Everyone else in the channel sees the session's status
// and nothing of the run, and gets the answer a missing run gets.
func TestRunAnsweringAWhisperStaysInTheWhisper(t *testing.T) {
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	run := f.start(t, f.botTok, sess, "general", f.whisper, "answer alice about the deploy")
	if run.State != schema.RunStateQueued || run.ChannelID != f.general.ID || run.MessageID != f.whisper ||
		run.SessionID != sess || run.PrincipalID != f.bot.ID || run.EndedAt != nil {
		t.Fatalf("created run = %+v", run)
	}
	running := f.mustState(t, f.botTok, run.ID, schema.RunStateRunning)
	if running.State != schema.RunStateRunning {
		t.Fatalf("state = %s", running.State)
	}
	rec := f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, activityBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("append activity = %d %s", rec.Code, rec.Body)
	}
	entry := validActivity(t, rec)
	if entry.RunID != run.ID || entry.PrincipalID != f.bot.ID || entry.Kind != schema.ActivityKindCommandStarted ||
		string(entry.Data) != `{"cmd":"go test","args":["./..."]}` || !entry.At.Time().Equal(f.clock.Now()) {
		t.Errorf("entry = %+v (data %s)", entry, entry.Data)
	}

	// Readers of the whisper: alice and the agent itself.
	for _, tok := range []string{f.aliceTok, f.botTok} {
		f.assertVisible(t, tok, run.ID, "general")
		s := f.sessionView(t, tok, sess)
		if s.CurrentRun != run.ID || s.Task != run.Task || s.Status != schema.SessionStatusRunning || s.OutcomeUnknown {
			t.Errorf("whisper reader's roster entry = %+v", s)
		}
	}
	page := validActivityList(t, f.do(t, "GET", runPath(run.ID, "/activity"), f.aliceTok, ""))
	if len(page.Activity) != 1 || page.Activity[0].ID != entry.ID || string(page.Activity[0].Data) != string(entry.Data) {
		t.Errorf("activity page = %+v", page)
	}
	// Everyone else in general: bob, the operator (no exemption), and the
	// other agents.
	for name, tok := range map[string]string{"bob": f.bobTok, "operator": f.rootTok, "rival": f.rivalTok, "shy": f.shyTok} {
		t.Run(name, func(t *testing.T) {
			f.assertHidden(t, tok, run.ID)
			s := f.sessionView(t, tok, sess)
			if s.CurrentRun != 0 || s.Task != "" {
				t.Errorf("roster entry shows the run: %+v", s)
			}
			if s.Status != schema.SessionStatusRunning {
				t.Errorf("status = %s, want running for every reader", s.Status)
			}
			if strings.Contains(f.do(t, "GET", "/v1/sessions?channel=general", tok, "").Body.String(), "deploy") {
				t.Error("the task reached the roster")
			}
		})
	}
	// carol is in no channel: the run and the roster are both closed to her.
	f.assertHidden(t, f.carolTok, run.ID)

	done := f.mustState(t, f.botTok, run.ID, schema.RunStateCompleted)
	if done.EndedAt == nil || !done.EndedAt.Time().Equal(f.clock.Now()) {
		t.Errorf("completed run = %+v", done)
	}
	if s := f.sessionView(t, f.aliceTok, sess); s.Status != schema.SessionStatusIdle || s.CurrentRun != 0 || s.Task != "" {
		t.Errorf("after completing: %+v", s)
	}

	// Audit: one run_started and one run_ended with the final state, by the
	// agent, naming no text of the run; nothing for the activity entry or for
	// the move to running.
	started, ended := f.auditRows(t, store.AuditRunStarted), f.auditRows(t, store.AuditRunEnded)
	if len(started) != 1 || len(ended) != 1 {
		t.Fatalf("run_started %d, run_ended %d; want 1 and 1", len(started), len(ended))
	}
	subject := fmt.Sprintf("run:%d", run.ID)
	actor := fmt.Sprintf("principal:%d", f.bot.ID)
	if started[0].Subject != subject || started[0].Actor != actor ||
		started[0].Detail != fmt.Sprintf("session=%d principal=%d channel=%d message=%d", sess, f.bot.ID, f.general.ID, f.whisper) {
		t.Errorf("run_started = %+v", started[0])
	}
	if ended[0].Subject != subject || ended[0].Actor != actor || !strings.HasPrefix(ended[0].Detail, "state=completed ") {
		t.Errorf("run_ended = %+v", ended[0])
	}
	for _, e := range f.audit(t) {
		if strings.Contains(e.Detail+e.Subject, "deploy") || strings.Contains(e.Detail+e.Subject, "go test") {
			t.Errorf("an audit row carries the run's text: %+v", e)
		}
	}
}

// A run in test-private is invisible to a member of general only, on every
// read and in the roster; a member of both sees it.
func TestRunInAnotherChannelIsInvisible(t *testing.T) {
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	run := f.start(t, f.botTok, sess, "test-private", 0, "private work")
	f.assertHidden(t, f.bobTok, run.ID)
	f.assertHidden(t, f.rootTok, run.ID)
	assertErrorBody(t, f.do(t, "GET", "/v1/runs?channel=test-private", f.bobTok, ""), http.StatusNotFound, "channel_not_found")
	if s := f.sessionView(t, f.bobTok, sess); s.CurrentRun != 0 || s.Task != "" || s.Status != schema.SessionStatusRunning {
		t.Errorf("bob's roster entry = %+v", s)
	}
	f.assertVisible(t, f.aliceTok, run.ID, "test-private")
	if s := f.sessionView(t, f.aliceTok, sess); s.CurrentRun != run.ID || s.Task != "private work" {
		t.Errorf("alice's roster entry = %+v", s)
	}
}

// Removing a reader from the channel hides its runs from them, those they saw
// before included, as for scoped messages; a channel-wide run as well.
func TestRemovedReaderLosesRuns(t *testing.T) {
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	wide := f.start(t, f.botTok, sess, "general", 0, "open work")
	secret := f.start(t, f.botTok, sess, "general", f.whisper, "answer alice")
	for _, id := range []int64{wide.ID, secret.ID} {
		f.assertVisible(t, f.aliceTok, id, "general")
	}
	if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.general.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{wide.ID, secret.ID} {
		f.assertHidden(t, f.aliceTok, id)
	}
	// The agent removed from the channel cannot report on its runs there.
	if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.general.ID, f.bot.ID); err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, f.setState(t, f.botTok, wide.ID, schema.RunStateRunning), http.StatusNotFound, schema.ErrorCodeRunNotFound)
}

// Starting a run: agents only, on a session of their own credential, and
// every refusal at the channel is exactly the unknown-channel answer, so
// nothing about a message the agent cannot use is told. A refused start
// stores and audits nothing.
func TestCreateRunRules(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	botSess := f.session(t, f.botTok)
	rivalSess := f.session(t, f.rivalTok)
	shySess := f.session(t, f.shyTok)
	_, botTok2, err := f.srv.store.CreateCredential(ctx, "system", f.bot.ID, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	ended := f.session(t, f.botTok)
	if rec := f.do(t, "DELETE", sessionPath(ended, ""), f.botTok, ""); rec.Code != http.StatusNoContent {
		t.Fatal(rec.Code)
	}
	// mute has no sessions.manage, so its session is made in the store.
	muteSess, err := f.srv.store.CreateSession(ctx, "system", store.SessionParams{PrincipalID: f.mute.ID, CredentialID: f.credentialID(t, f.muteTok), Client: "x"}, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	notForBot := f.whisperTo(t, f.alice, "not for the bot", f.bob)
	withRival := f.whisperTo(t, f.alice, "for both agents", f.bot, f.rival)
	toShy := f.whisperTo(t, f.alice, "for shy", f.shy)
	wideMsg, err := f.srv.store.InsertMessageV1(ctx, f.general.ID, f.alice.ID, "open question", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two nets in general, each with a message from alice: bot is a member of
	// "speak" and only a monitor of "watch". bot holds post_net in general,
	// so only the net's roster can refuse it.
	onNet := map[string]int64{}
	for _, n := range []struct {
		name string
		role schema.NetRole
	}{{"speak", schema.NetRoleMember}, {"watch", schema.NetRoleMonitor}} {
		net, err := f.srv.store.CreateNet(ctx, "system", f.general.ID, n.name, f.root.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, seat := range []struct {
			id   int64
			role schema.NetRole
		}{{f.alice.ID, schema.NetRoleMember}, {f.bot.ID, n.role}} {
			if _, err := f.srv.store.PutNetMember(ctx, "system", f.general.ID, n.name, seat.id, seat.role, f.root.ID); err != nil {
				t.Fatal(err)
			}
		}
		m, rec, err := f.srv.store.InsertScopedMessage(ctx, store.ScopedPost{
			ChannelID: f.general.ID, AuthorID: f.alice.ID, Body: "on " + n.name,
			Audience: schema.Audience{Kind: schema.AudienceKindNet, NetID: net.ID},
		})
		if err != nil || len(rec) != 2 {
			t.Fatalf("post to %s = %v, recipients %v", n.name, err, rec)
		}
		onNet[n.name] = m.ID
	}

	tests := []struct {
		name  string
		token string
		sess  int64
		body  string
		want  int
		code  string
	}{
		{"a channel-wide run", f.botTok, botSess, runBody("general", 0, "t"), 201, ""},
		{"answering a channel-wide message", f.botTok, botSess, runBody("general", wideMsg.ID, "t"), 201, ""},
		{"answering a whisper to the agent", f.botTok, botSess, runBody("general", f.whisper, "t"), 201, ""},
		{"in another channel the agent may post in", f.botTok, botSess, runBody("test-private", f.privateMsg, "t"), 201, ""},
		{"answering a message on a net the agent is a member of", f.botTok, botSess, runBody("general", onNet["speak"], "t"), 201, ""},
		{"a message on a net the agent only monitors", f.botTok, botSess, runBody("general", onNet["watch"], "t"), 404, "channel_not_found"},
		{"a human", f.aliceTok, botSess, runBody("general", 0, "t"), 403, schema.ErrorCodeAgentRequired},
		{"an operator", f.rootTok, botSess, runBody("general", 0, "t"), 403, schema.ErrorCodeAgentRequired},
		{"another agent's session", f.botTok, rivalSess, runBody("general", 0, "t"), 404, schema.ErrorCodeSessionNotFound},
		{"the agent's session from its other credential", botTok2, botSess, runBody("general", 0, "t"), 404, schema.ErrorCodeSessionNotFound},
		{"an ended session", f.botTok, ended, runBody("general", 0, "t"), 404, schema.ErrorCodeSessionNotFound},
		{"an unknown session", f.botTok, 999999, runBody("general", 0, "t"), 404, schema.ErrorCodeSessionNotFound},
		{"no task", f.botTok, botSess, `{"channel":"general"}`, 400, "invalid_request"},
		{"a task with a newline", f.botTok, botSess, runBody("general", 0, "a\nb"), 400, "invalid_request"},
		{"an http link", f.botTok, botSess, `{"channel":"general","task":"t","link":"http://x.example/"}`, 400, "invalid_request"},
		{"an unknown field", f.botTok, botSess, `{"channel":"general","task":"t","state":"running"}`, 400, "invalid_request"},
		{"an unknown channel", f.botTok, botSess, runBody("nosuch", 0, "t"), 404, "channel_not_found"},
		{"a channel the agent is not in", f.botTok, botSess, runBody("elsewhere", 0, "t"), 404, "channel_not_found"},
		{"a message of another channel", f.botTok, botSess, runBody("general", f.privateMsg, "t"), 404, "channel_not_found"},
		{"a whisper the agent is not in", f.botTok, botSess, runBody("general", notForBot, "t"), 404, "channel_not_found"},
		{"a message that does not exist", f.botTok, botSess, runBody("general", 999999, "t"), 404, "channel_not_found"},
		{"no runs.manage", f.muteTok, muteSess.ID, runBody("general", 0, "t"), 404, "channel_not_found"},
		{"a whisper, without the whisper grant", f.shyTok, shySess, runBody("general", toShy, "t"), 404, "channel_not_found"},
		{"a whisper with another agent, without whisper_agent", f.botTok, botSess, runBody("general", withRival, "t"), 404, "channel_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before, audit := len(f.auditRows(t, store.AuditRunStarted)), len(f.audit(t))
			rec := f.do(t, "POST", sessionPath(tt.sess, "/runs"), tt.token, tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.want, rec.Body)
			}
			if tt.want == http.StatusCreated {
				run := validRun(t, rec)
				if len(f.auditRows(t, store.AuditRunStarted)) != before+1 || run.State != schema.RunStateQueued {
					t.Errorf("run = %+v, run_started rows %d -> %d", run, before, len(f.auditRows(t, store.AuditRunStarted)))
				}
				return
			}
			assertErrorBody(t, rec, tt.want, tt.code)
			if len(f.auditRows(t, store.AuditRunStarted)) != before {
				t.Error("a refused start wrote run_started")
			}
			for _, e := range f.audit(t)[audit:] {
				if e.Action != "access_denied" {
					t.Errorf("a refused start wrote %+v", e)
				}
			}
			if tt.code != "channel_not_found" {
				return
			}
			// Exactly the unknown-channel answer, byte for byte.
			missing := f.do(t, "POST", sessionPath(tt.sess, "/runs"), tt.token, runBody("nosuch", 0, "t"))
			if rec.Body.String() != missing.Body.String() {
				t.Errorf("refusal %s differs from the unknown-channel answer %s", rec.Body, missing.Body)
			}
		})
	}
}

// A second agent holding runs.manage in the same channel, the agent's own
// other credential, an operator and a human who can see the run all get the
// answer a missing run gets when they try to report on it. The second agent
// may read a channel-wide run (it is a member) and not a whisper run.
func TestRunOwnership(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	wide := f.start(t, f.botTok, sess, "general", 0, "open work")
	secret := f.start(t, f.botTok, sess, "general", f.whisper, "answer alice")
	_, botTok2, err := f.srv.store.CreateCredential(ctx, "system", f.bot.ID, "second", nil)
	if err != nil {
		t.Fatal(err)
	}
	callers := []struct {
		name  string
		token string
	}{
		{"a second agent with runs.manage", f.rivalTok},
		{"the agent's other credential", botTok2},
		{"an operator", f.rootTok},
		{"a human who can see the run", f.aliceTok},
	}
	for _, c := range callers {
		for _, run := range []schema.RunV1{wide, secret} {
			t.Run(fmt.Sprintf("%s/run %d", c.name, run.ID), func(t *testing.T) {
				for _, r := range []struct{ suffix, body string }{
					{"/state", `{"state":"running"}`},
					{"/activity", activityBody},
				} {
					got := f.do(t, "POST", runPath(run.ID, r.suffix), c.token, r.body)
					missing := f.do(t, "POST", runPath(999999, r.suffix), c.token, r.body)
					assertErrorBody(t, got, http.StatusNotFound, schema.ErrorCodeRunNotFound)
					if got.Body.String() != missing.Body.String() {
						t.Errorf("POST %s = %s, a missing run %s", r.suffix, got.Body, missing.Body)
					}
				}
			})
		}
	}
	f.assertVisible(t, f.rivalTok, wide.ID, "general")
	f.assertHidden(t, f.rivalTok, secret.ID)
	for _, id := range []int64{wide.ID, secret.ID} {
		rec := f.do(t, "GET", runPath(id, ""), f.botTok, "")
		if rec.Code != http.StatusOK || validRun(t, rec).State != schema.RunStateQueued {
			t.Errorf("run %d after the attempts = %d %s, want untouched", id, rec.Code, rec.Body)
		}
		if page := validActivityList(t, f.do(t, "GET", runPath(id, "/activity"), f.botTok, "")); len(page.Activity) != 0 {
			t.Errorf("run %d has activity after the attempts: %+v", id, page)
		}
	}
}

// An agent that started a run on a net message as a member of the net, and is
// then demoted to monitor, may no longer report on the run: that would go on
// speaking to the net. Its state change and its activity get the answer a
// stranger gets, and the run is left as it was.
func TestDemotedNetMemberCannotReport(t *testing.T) {
	ctx := context.Background()
	f := newRunFixture(t)
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
	sess := f.session(t, f.botTok)
	run := f.start(t, f.botTok, sess, "general", m.ID, "answer the net")
	f.mustState(t, f.botTok, run.ID, schema.RunStateRunning)

	if _, err := f.srv.store.PutNetMember(ctx, "system", f.general.ID, "speak", f.bot.ID, schema.NetRoleMonitor, f.root.ID); err != nil {
		t.Fatal(err)
	}
	ended := len(f.auditRows(t, store.AuditRunEnded))
	for _, r := range []struct{ suffix, body string }{
		{"/state", `{"state":"completed","outcome":"done"}`},
		{"/activity", activityBody},
	} {
		got := f.do(t, "POST", runPath(run.ID, r.suffix), f.botTok, r.body)
		missing := f.do(t, "POST", runPath(999999, r.suffix), f.botTok, r.body)
		assertErrorBody(t, got, http.StatusNotFound, schema.ErrorCodeRunNotFound)
		if got.Body.String() != missing.Body.String() {
			t.Errorf("POST %s = %s, a missing run %s", r.suffix, got.Body, missing.Body)
		}
	}
	after := validRun(t, f.do(t, "GET", runPath(run.ID, ""), f.aliceTok, ""))
	if after.State != schema.RunStateRunning || after.Outcome != "" || after.EndedAt != nil {
		t.Errorf("run after the refused reports = %+v, want running and untouched", after)
	}
	if page := validActivityList(t, f.do(t, "GET", runPath(run.ID, "/activity"), f.aliceTok, "")); len(page.Activity) != 0 {
		t.Errorf("activity after the refused append = %+v", page)
	}
	if len(f.auditRows(t, store.AuditRunEnded)) != ended {
		t.Error("a refused state change wrote run_ended")
	}
}

// pathFromQueued is a legal way to each state, so the transition table can be
// tried from every state.
var pathFromQueued = map[schema.RunState][]schema.RunState{
	schema.RunStateQueued:    nil,
	schema.RunStateStarting:  {schema.RunStateStarting},
	schema.RunStateRunning:   {schema.RunStateRunning},
	schema.RunStateBlocked:   {schema.RunStateRunning, schema.RunStateBlocked},
	schema.RunStateWaiting:   {schema.RunStateRunning, schema.RunStateWaiting},
	schema.RunStateCompleted: {schema.RunStateRunning, schema.RunStateCompleted},
	schema.RunStateFailed:    {schema.RunStateFailed},
	schema.RunStateCancelled: {schema.RunStateCancelled},
}

// The whole transition table over the route, from schema.RunTransitionAllowed:
// every legal change is 200, every illegal one 409 invalid_transition with the
// run untouched; entering a terminal state sets ended_at; a terminal run
// accepts no activity and no further state.
func TestRunTransitionsOverTheRoute(t *testing.T) {
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	states := []schema.RunState{
		schema.RunStateQueued, schema.RunStateStarting, schema.RunStateRunning, schema.RunStateBlocked,
		schema.RunStateWaiting, schema.RunStateCompleted, schema.RunStateFailed, schema.RunStateCancelled,
	}
	for _, from := range states {
		for _, to := range states {
			t.Run(fmt.Sprintf("%s to %s", from, to), func(t *testing.T) {
				run := f.start(t, f.botTok, sess, "general", 0, "t")
				for _, step := range pathFromQueued[from] {
					f.mustState(t, f.botTok, run.ID, step)
				}
				f.clock.Advance(time.Second)
				rec := f.do(t, "POST", runPath(run.ID, "/state"), f.botTok, fmt.Sprintf(`{"state":%q,"outcome":"because"}`, to))
				if !schema.RunTransitionAllowed(from, to) {
					assertErrorBody(t, rec, http.StatusConflict, schema.ErrorCodeInvalidTransition)
					after := validRun(t, f.do(t, "GET", runPath(run.ID, ""), f.botTok, ""))
					if after.State != from || after.Outcome != "" {
						t.Errorf("an illegal change wrote: %+v", after)
					}
				} else {
					if rec.Code != http.StatusOK {
						t.Fatalf("legal change = %d %s", rec.Code, rec.Body)
					}
					got := validRun(t, rec)
					if got.State != to || got.Outcome != "because" {
						t.Errorf("run = %+v", got)
					}
					if schema.RunStateTerminal(to) && !got.EndedAt.Time().Equal(f.clock.Now()) {
						t.Errorf("ended_at = %v, want %v", got.EndedAt, f.clock.Now())
					}
				}
				if !schema.RunStateTerminal(from) {
					return
				}
				// A terminal run takes nothing further.
				assertErrorBody(t, f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, activityBody), http.StatusConflict, schema.ErrorCodeInvalidTransition)
			})
		}
	}
}

// The roster's status for an agent session follows its runs as the issue's
// mapping says, for every reader; outcome_unknown is set exactly when the
// session is stale or offline with a run open; and a session going stale or
// offline leaves its run's state as it was (P3-07).
func TestSessionStatusFromRuns(t *testing.T) {
	tests := []struct {
		name  string
		steps [][]schema.RunState // one entry per run, started in order
		want  schema.SessionStatus
	}{
		{"no run", nil, schema.SessionStatusIdle},
		{"queued", [][]schema.RunState{{}}, schema.SessionStatusRunning},
		{"starting", [][]schema.RunState{{schema.RunStateStarting}}, schema.SessionStatusRunning},
		{"running", [][]schema.RunState{{schema.RunStateRunning}}, schema.SessionStatusRunning},
		{"blocked", [][]schema.RunState{{schema.RunStateRunning, schema.RunStateBlocked}}, schema.SessionStatusBlocked},
		{"waiting", [][]schema.RunState{{schema.RunStateRunning, schema.RunStateWaiting}}, schema.SessionStatusWaiting},
		{"completed", [][]schema.RunState{{schema.RunStateRunning, schema.RunStateCompleted}}, schema.SessionStatusIdle},
		{"cancelled", [][]schema.RunState{{schema.RunStateCancelled}}, schema.SessionStatusIdle},
		{"failed, none open", [][]schema.RunState{{schema.RunStateFailed}}, schema.SessionStatusFailed},
		{"failed, then one open", [][]schema.RunState{{schema.RunStateFailed}, {schema.RunStateRunning, schema.RunStateWaiting}}, schema.SessionStatusWaiting},
		{"failed, then completed", [][]schema.RunState{{schema.RunStateFailed}, {schema.RunStateRunning, schema.RunStateCompleted}}, schema.SessionStatusIdle},
		{"completed, then failed", [][]schema.RunState{{schema.RunStateRunning, schema.RunStateCompleted}, {schema.RunStateFailed}}, schema.SessionStatusFailed},
		{"an older open run under a newer ended one", [][]schema.RunState{{schema.RunStateRunning, schema.RunStateBlocked}, {schema.RunStateFailed}}, schema.SessionStatusBlocked},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRunFixture(t)
			sess := f.session(t, f.botTok)
			var last schema.RunV1
			open := false
			for _, steps := range tt.steps {
				last = f.start(t, f.botTok, sess, "general", 0, "t")
				for _, s := range steps {
					last = f.mustState(t, f.botTok, last.ID, s)
				}
				if !schema.RunStateTerminal(last.State) {
					open = true
				}
			}
			for _, tok := range []string{f.aliceTok, f.bobTok, f.rootTok, f.botTok} {
				if s := f.sessionView(t, tok, sess); s.Status != tt.want || s.OutcomeUnknown {
					t.Errorf("status = %s, outcome_unknown %v; want %s, false", s.Status, s.OutcomeUnknown, tt.want)
				}
			}
			// The owner's own heartbeat says the same.
			rec := f.do(t, "POST", sessionPath(sess, "/heartbeat"), f.botTok, "")
			if rec.Code != http.StatusOK || validSession(t, rec).Status != tt.want {
				t.Errorf("heartbeat = %d %s", rec.Code, rec.Body)
			}

			states := map[int64]schema.RunState{}
			for _, r := range validRunList(t, f.do(t, "GET", "/v1/runs?channel=general", f.botTok, "")).Runs {
				states[r.ID] = r.State
			}
			for _, step := range []struct {
				advance time.Duration
				state   schema.SessionState
			}{{31 * time.Second, schema.SessionStateStale}, {60 * time.Second, schema.SessionStateOffline}} {
				f.clock.Advance(step.advance)
				f.srv.sweepSessions(context.Background())
				s := f.sessionView(t, f.aliceTok, sess)
				if s.State != step.state || s.OutcomeUnknown != open {
					t.Errorf("%s: state %s, outcome_unknown %v; want %s, %v", step.state, s.State, s.OutcomeUnknown, step.state, open)
				}
				if open && s.CurrentRun == 0 {
					t.Errorf("%s: the open run is no longer shown: %+v", step.state, s)
				}
				for _, r := range validRunList(t, f.do(t, "GET", "/v1/runs?channel=general", f.botTok, "")).Runs {
					if states[r.ID] != r.State || (r.EndedAt != nil) != schema.RunStateTerminal(r.State) {
						t.Errorf("%s: run %d went from %s to %s", step.state, r.ID, states[r.ID], r.State)
					}
				}
			}
			// A heartbeat revives the session; it is online and trusted again.
			rec = f.do(t, "POST", sessionPath(sess, "/heartbeat"), f.botTok, "")
			if s := validSession(t, rec); s.Status != tt.want || s.OutcomeUnknown {
				t.Errorf("after reviving: %+v", s)
			}
			// Ending the session fails the runs still open (issue #402) and
			// leaves the ended ones as they were.
			if rec := f.do(t, "DELETE", sessionPath(sess, ""), f.botTok, ""); rec.Code != http.StatusNoContent {
				t.Fatal(rec.Code)
			}
			for _, r := range validRunList(t, f.do(t, "GET", "/v1/runs?channel=general", f.botTok, "")).Runs {
				want := states[r.ID]
				if !schema.RunStateTerminal(want) {
					want = schema.RunStateFailed
					if r.Outcome != store.RunOutcomeSessionEnded || r.EndedAt == nil {
						t.Errorf("ending the session left run %d with outcome %q, ended %v", r.ID, r.Outcome, r.EndedAt)
					}
				}
				if want != r.State {
					t.Errorf("ending the session moved run %d from %s to %s, want %s", r.ID, states[r.ID], r.State, want)
				}
			}
		})
	}
}

// The list: ascending ids, a page full of visible runs, next_after only when
// another visible run exists, session= narrows; bad parameters are 400; an
// unknown channel and one the caller is not in are the same 404.
func TestListRuns(t *testing.T) {
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	other := f.session(t, f.botTok)
	var bobs []int64
	for i := 0; i < 4; i++ {
		bobs = append(bobs, f.start(t, f.botTok, sess, "general", 0, fmt.Sprintf("open %d", i)).ID)
		f.start(t, f.botTok, sess, "general", f.whisper, "hidden from bob")
	}
	onOther := f.start(t, f.botTok, other, "general", 0, "on the other session").ID
	bobs = append(bobs, onOther)

	var got []int64
	after := int64(0)
	for pages := 0; ; pages++ {
		if pages > 20 {
			t.Fatal("paging did not end")
		}
		rec := f.do(t, "GET", fmt.Sprintf("/v1/runs?channel=general&limit=2&after=%d", after), f.bobTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("list = %d %s", rec.Code, rec.Body)
		}
		page := validRunList(t, rec)
		for _, r := range page.Runs {
			got = append(got, r.ID)
		}
		if page.NextAfter == 0 {
			break
		}
		after = page.NextAfter
	}
	if fmt.Sprint(got) != fmt.Sprint(bobs) {
		t.Errorf("bob paged %v, want %v", got, bobs)
	}
	page := validRunList(t, f.do(t, "GET", fmt.Sprintf("/v1/runs?channel=general&session=%d", other), f.bobTok, ""))
	if len(page.Runs) != 1 || page.Runs[0].ID != onOther || page.NextAfter != 0 {
		t.Errorf("session filter = %+v", page)
	}
	if page := validRunList(t, f.do(t, "GET", "/v1/runs?channel=test-private", f.aliceTok, "")); len(page.Runs) != 0 {
		t.Errorf("an empty channel = %+v, want an empty array", page)
	}

	for _, c := range []struct {
		name, query string
		want        int
		code        string
	}{
		{"no channel", "", 400, "invalid_request"},
		{"a zero session", "channel=general&session=0", 400, "invalid_request"},
		{"a session that is not a number", "channel=general&session=x", 400, "invalid_request"},
		{"a limit of 0", "channel=general&limit=0", 400, "invalid_request"},
		{"a limit over 100", "channel=general&limit=101", 400, "invalid_request"},
		{"a negative after", "channel=general&after=-1", 400, "invalid_request"},
		{"an unknown channel", "channel=nosuch", 404, "channel_not_found"},
		{"a channel bob is not in", "channel=test-private", 404, "channel_not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertErrorBody(t, f.do(t, "GET", "/v1/runs?"+c.query, f.bobTok, ""), c.want, c.code)
		})
	}
	// An empty session= is no filter, as an empty after= or limit= is the
	// default.
	if page := validRunList(t, f.do(t, "GET", "/v1/runs?channel=general&session=&after=&limit=", f.bobTok, "")); len(page.Runs) != len(bobs) {
		t.Errorf("empty parameters listed %d runs, want all %d bob may see", len(page.Runs), len(bobs))
	}
	if a, b := f.do(t, "GET", "/v1/runs?channel=test-private", f.bobTok, ""), f.do(t, "GET", "/v1/runs?channel=nosuch", f.bobTok, ""); a.Body.String() != b.Body.String() {
		t.Errorf("a channel bob is not in answers %s, an unknown one %s", a.Body, b.Body)
	}
	// An agent reads the list under messages.read with read on the channel.
	f.grant(t, f.rival.ID, []schema.Capability{schema.CapabilityRunsManage}, nil, map[int64][]schema.ChannelPermission{
		f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost},
	})
	assertErrorBody(t, f.do(t, "GET", "/v1/runs?channel=general", f.rivalTok, ""), http.StatusForbidden, "forbidden")
	assertErrorBody(t, f.do(t, "GET", runPath(bobs[0], ""), f.rivalTok, ""), http.StatusForbidden, "forbidden")
}

// Activity: oldest first, paged with next_after and a limit of at most 100,
// the data kept as encoding/json writes it; malformed entries are 400 and
// store nothing.
func TestRunActivity(t *testing.T) {
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	run := f.start(t, f.botTok, sess, "general", 0, "t")
	var ids []int64
	for i := 0; i < 5; i++ {
		rec := f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, fmt.Sprintf(`{"kind":"reply_posted","summary":"reply %d","data": { "html" : "<b>&</b>" }}`, i))
		if rec.Code != http.StatusCreated {
			t.Fatalf("append = %d %s", rec.Code, rec.Body)
		}
		a := validActivity(t, rec)
		// Compact, and "<", ">" and "&" as six-byte escapes (the backslash is
		// spliced in so the escape is not read as a character here).
		esc := func(hex string) string { return `\` + "u00" + hex }
		if want := `{"html":"` + esc("3c") + "b" + esc("3e") + esc("26") + esc("3c") + "/b" + esc("3e") + `"}`; string(a.Data) != want {
			t.Errorf("data = %q, want the compact escaped form", a.Data)
		}
		ids = append(ids, a.ID)
	}
	var got []int64
	after := int64(0)
	for {
		page := validActivityList(t, f.do(t, "GET", fmt.Sprintf("%s?limit=2&after=%d", runPath(run.ID, "/activity"), after), f.bobTok, ""))
		for _, a := range page.Activity {
			got = append(got, a.ID)
		}
		if page.NextAfter == 0 {
			break
		}
		after = page.NextAfter
	}
	if fmt.Sprint(got) != fmt.Sprint(ids) {
		t.Errorf("paged %v, want %v", got, ids)
	}
	for _, c := range []struct{ name, body string }{
		{"an unknown kind", `{"kind":"exploded","summary":"x"}`},
		{"no summary", `{"kind":"reply_posted","summary":" "}`},
		{"a summary with a newline", `{"kind":"reply_posted","summary":"a\nb"}`},
		{"data too large", fmt.Sprintf(`{"kind":"reply_posted","summary":"x","data":%q}`, strings.Repeat("a", schema.MaxActivityDataBytes))},
		{"a client time", `{"kind":"reply_posted","summary":"x","at":"2020-01-01T00:00:00Z"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			assertErrorBody(t, f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, c.body), http.StatusBadRequest, "invalid_request")
		})
	}
	if page := validActivityList(t, f.do(t, "GET", runPath(run.ID, "/activity"), f.botTok, "")); len(page.Activity) != len(ids) {
		t.Errorf("entries = %d, want %d", len(page.Activity), len(ids))
	}
	assertErrorBody(t, f.do(t, "GET", runPath(run.ID, "/activity?limit=101"), f.botTok, ""), http.StatusBadRequest, "invalid_request")

	// No data and "data": null are the same entry: no data key, on the
	// response and on every read.
	t.Run("no data and null data", func(t *testing.T) {
		other := f.start(t, f.botTok, sess, "general", 0, "no data")
		for _, body := range []string{
			`{"kind":"reply_posted","summary":"no data"}`,
			`{"kind":"reply_posted","summary":"null data","data":null}`,
			`{"kind":"reply_posted","summary":"spaced null","data": null }`,
		} {
			rec := f.do(t, "POST", runPath(other.ID, "/activity"), f.botTok, body)
			if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), `"data"`) {
				t.Errorf("append %s = %d %s; want 201 with no data key", body, rec.Code, rec.Body)
			}
			validActivity(t, rec)
		}
		rec := f.do(t, "GET", runPath(other.ID, "/activity"), f.aliceTok, "")
		if page := validActivityList(t, rec); len(page.Activity) != 3 || strings.Contains(rec.Body.String(), `"data"`) {
			t.Errorf("read back %s; want three entries and no data key", rec.Body)
		}
	})
	assertErrorBody(t, f.do(t, "GET", "/v1/runs/x/activity", f.botTok, ""), http.StatusNotFound, schema.ErrorCodeRunNotFound)
}

// Every report re-checks the channel gate and spends runs.manage once: with a
// budget of three, the start, one state change and one activity entry use it
// up, and the next report is 429. Taking the grant away is a 403 for the
// owner.
func TestRunReportsSpendRunsManage(t *testing.T) {
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	perms := map[int64][]schema.ChannelPermission{f.general.ID: {schema.ChannelPermissionRead, schema.ChannelPermissionPost}}
	f.grant(t, f.bot.ID, schema.Capabilities(), []schema.RateLimit{{Capability: schema.CapabilityRunsManage, Max: 3, WindowSeconds: 3600}}, perms)
	run := f.start(t, f.botTok, sess, "general", 0, "t")
	f.mustState(t, f.botTok, run.ID, schema.RunStateRunning)
	if rec := f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, activityBody); rec.Code != http.StatusCreated {
		t.Fatalf("activity = %d %s", rec.Code, rec.Body)
	}
	// Reads spend nothing of runs.manage.
	f.assertVisible(t, f.botTok, run.ID, "general")
	assertErrorBody(t, f.setState(t, f.botTok, run.ID, schema.RunStateCompleted), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)

	// Without runs.manage the owner is refused, and the run is untouched.
	f.grant(t, f.bot.ID, []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilitySessionsManage}, nil, perms)
	assertErrorBody(t, f.setState(t, f.botTok, run.ID, schema.RunStateCompleted), http.StatusForbidden, "forbidden")
	assertErrorBody(t, f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, activityBody), http.StatusForbidden, "forbidden")
	if got := validRun(t, f.do(t, "GET", runPath(run.ID, ""), f.botTok, "")); got.State != schema.RunStateRunning {
		t.Errorf("state = %s, want running", got.State)
	}
	// Without post in the channel likewise.
	f.grant(t, f.bot.ID, schema.Capabilities(), nil, map[int64][]schema.ChannelPermission{f.general.ID: {schema.ChannelPermissionRead}})
	assertErrorBody(t, f.setState(t, f.botTok, run.ID, schema.RunStateCompleted), http.StatusForbidden, "forbidden")
}

// A session holds at most store.MaxOpenRunsPerSession runs that have not
// ended: the next start is 429 too_many_runs and stores nothing, another
// session is unaffected, and ending a run frees a slot.
func TestRunOpenBoundOverTheRoute(t *testing.T) {
	f := newRunFixture(t)
	sess := f.session(t, f.botTok)
	var first schema.RunV1
	for i := range store.MaxOpenRunsPerSession {
		run := f.start(t, f.botTok, sess, "general", 0, "t")
		if i == 0 {
			first = run
		}
	}
	before := len(f.listedRunIDs(t, f.botTok, "general"))
	assertErrorBody(t, f.do(t, "POST", sessionPath(sess, "/runs"), f.botTok, runBody("general", 0, "one too many")),
		http.StatusTooManyRequests, schema.ErrorCodeTooManyRuns)
	if got := len(f.listedRunIDs(t, f.botTok, "general")); got != before {
		t.Errorf("runs listed = %d, want %d: the refused start stored one", got, before)
	}
	f.start(t, f.botTok, f.session(t, f.botTok), "general", 0, "another session")
	f.mustState(t, f.botTok, first.ID, schema.RunStateCancelled)
	f.start(t, f.botTok, sess, "general", 0, "into the freed slot")
	assertErrorBody(t, f.do(t, "POST", sessionPath(sess, "/runs"), f.botTok, runBody("general", 0, "full again")),
		http.StatusTooManyRequests, schema.ErrorCodeTooManyRuns)
}

// fillActivity gives run runID n entries through the store, the same
// transaction a report uses, so a bound of ten thousand is reached without
// ten thousand HTTP requests. It still takes seconds (minutes under -race),
// so only the one test that needs it calls it.
func (f *runFixture) fillActivity(t *testing.T, runID int64, n int) {
	t.Helper()
	reader := store.Reader{PrincipalID: f.bot.ID, Scoped: true}
	_, cred, err := f.srv.store.ResolveCredentialDetail(t.Context(), f.botTok)
	if err != nil {
		t.Fatal(err)
	}
	for range n {
		if _, err := f.srv.store.AppendRunActivity(t.Context(), runID, cred, reader,
			store.ActivityParams{Kind: schema.ActivityKindCommandStarted, Summary: "fill"}, time.Now()); err != nil {
			t.Fatalf("fill: %v", err)
		}
	}
}
