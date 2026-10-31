package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The events socket, end to end (issue #269). Every frame is decoded by its
// schema field and run through that document's Validate. No test sleeps for
// timing: the re-check interval, the keep-alive ticks and the server clock
// are injected, and every read has a deadline so a missing frame fails the
// test instead of hanging it.

const eventsReadTimeout = 5 * time.Second

// eventFrame is one decoded frame; only the field its schema names is set.
type eventFrame struct {
	schema       string
	roster       schema.SessionRosterV1
	run          schema.RunV1
	activity     schema.AgentActivityV1
	intervention schema.InterventionV1
	members      schema.ChannelMembersV1
	channel      schema.ChannelDocV1
	readState    schema.ReadStateV1
	message      schema.MessageV2
}

func (fr eventFrame) String() string {
	switch fr.schema {
	case schema.SessionRosterSchemaV1:
		return fmt.Sprintf("roster%+v", fr.roster)
	case schema.ChannelMembersSchemaV1:
		return fmt.Sprintf("members%+v", fr.members)
	case schema.RunSchemaV1:
		return fmt.Sprintf("run%+v", fr.run)
	case schema.AgentActivitySchemaV1:
		return fmt.Sprintf("activity%+v", fr.activity)
	case schema.InterventionSchemaV1:
		return fmt.Sprintf("intervention%+v", fr.intervention)
	case schema.ChannelSchemaV1:
		return fmt.Sprintf("channel%+v", fr.channel)
	}
	return fr.schema
}

// eventsClient is one open events socket.
type eventsClient struct {
	t    *testing.T
	conn *websocket.Conn
}

// dialEvents opens the events socket at path with a bearer token, or with
// none when token is empty (a ticket in the path).
func dialEvents(t *testing.T, base, path, token string) *eventsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	var hdr http.Header
	if token != "" {
		hdr = http.Header{"Authorization": {"Bearer " + token}}
	}
	conn, resp, err := websocket.Dial(ctx, base+path, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			_ = resp.Body.Close()
		}
		t.Fatalf("dial %s = %v (status %d)", path, err, status)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &eventsClient{t: t, conn: conn}
}

// next reads and validates one frame.
func (c *eventsClient) next() eventFrame {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	typ, raw, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("read a frame: %v", err)
	}
	if typ != websocket.MessageText {
		c.t.Fatalf("frame type = %v, want text", typ)
	}
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		c.t.Fatalf("frame is not JSON: %v\n%s", err, raw)
	}
	fr := eventFrame{schema: head.Schema}
	var verr error
	switch head.Schema {
	case schema.SessionRosterSchemaV1:
		if err = json.Unmarshal(raw, &fr.roster); err == nil {
			verr = fr.roster.Validate()
		}
	case schema.RunSchemaV1:
		if err = json.Unmarshal(raw, &fr.run); err == nil {
			verr = fr.run.Validate()
		}
	case schema.AgentActivitySchemaV1:
		if err = json.Unmarshal(raw, &fr.activity); err == nil {
			verr = fr.activity.Validate()
		}
	case schema.InterventionSchemaV1:
		if err = json.Unmarshal(raw, &fr.intervention); err == nil {
			verr = fr.intervention.Validate()
		}
	case schema.ChannelMembersSchemaV1:
		if err = json.Unmarshal(raw, &fr.members); err == nil {
			verr = fr.members.Validate()
		}
	case schema.ChannelSchemaV1:
		if err = json.Unmarshal(raw, &fr.channel); err == nil {
			verr = fr.channel.Validate()
		}
	case schema.ReadStateSchemaV1:
		if err = json.Unmarshal(raw, &fr.readState); err == nil {
			verr = fr.readState.Validate()
		}
	case schema.MessageSchemaV2:
		if err = json.Unmarshal(raw, &fr.message); err == nil {
			verr = fr.message.Validate()
		}
	case schema.ChannelsChangedSchemaV1:
		var doc schema.ChannelsChangedV1
		if err = json.Unmarshal(raw, &doc); err == nil {
			verr = doc.Validate()
		}
	default:
		c.t.Fatalf("a frame of schema %q, which the events socket does not send:\n%s", head.Schema, raw)
	}
	if err != nil {
		c.t.Fatalf("decode a %s frame: %v\n%s", head.Schema, err, raw)
	}
	if verr != nil {
		c.t.Fatalf("the server sent an invalid %s frame: %v\n%s", head.Schema, verr, raw)
	}
	return fr
}

// until reads frames until one satisfies match, and returns every frame read,
// the matching one last.
func (c *eventsClient) until(what string, match func(eventFrame) bool) []eventFrame {
	c.t.Helper()
	var seen []eventFrame
	for range 64 {
		fr := c.next()
		seen = append(seen, fr)
		if match(fr) {
			return seen
		}
	}
	c.t.Fatalf("no %s in %d frames: %v", what, len(seen), seen)
	return nil
}

// closed reads until the socket is closed and returns how.
func (c *eventsClient) closed() websocket.CloseError {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	for {
		_, _, err := c.conn.Read(ctx)
		if err == nil {
			continue // a frame sent before the close
		}
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			c.t.Fatalf("the socket ended with %v, want a close frame", err)
		}
		return ce
	}
}

func isRoster(match func(schema.SessionRosterV1) bool) func(eventFrame) bool {
	return func(fr eventFrame) bool {
		return fr.schema == schema.SessionRosterSchemaV1 && match(fr.roster)
	}
}

func isRun(match func(schema.RunV1) bool) func(eventFrame) bool {
	return func(fr eventFrame) bool { return fr.schema == schema.RunSchemaV1 && match(fr.run) }
}

func rosterSession(r schema.SessionRosterV1, principalID int64) (schema.SessionV1, bool) {
	for _, s := range r.Sessions {
		if s.PrincipalID == principalID {
			return s, true
		}
	}
	return schema.SessionV1{}, false
}

// rosterHas matches a roster that shows principalID's session with status.
func rosterHas(principalID int64, status schema.SessionStatus) func(schema.SessionRosterV1) bool {
	return func(r schema.SessionRosterV1) bool {
		s, ok := rosterSession(r, principalID)
		return ok && s.Status == status
	}
}

func rosterLacks(principalID int64) func(schema.SessionRosterV1) bool {
	return func(r schema.SessionRosterV1) bool {
		_, ok := rosterSession(r, principalID)
		return !ok
	}
}

// subscribers is how many events sockets are registered.
func (s *Server) eventSubscribers() int {
	s.events.mu.Lock()
	defer s.events.mu.Unlock()
	return len(s.events.subs)
}

// TestEventsSnapshotThenEachChange: a subscriber gets the roster on connect
// and then a frame for each change the issue lists.
func TestEventsSnapshotThenEachChange(t *testing.T) {
	f := &interventionFixture{runFixture: newRunFixture(t)} // no session or run yet: only its helpers
	ctx := context.Background()
	base := wsTestServer(t, f.srv)
	alice := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)

	first := alice.next()
	if first.schema != schema.SessionRosterSchemaV1 || first.roster.ChannelID != f.general.ID || len(first.roster.Sessions) != 0 {
		t.Fatalf("first frame = %v, want the empty roster of general", first)
	}

	var (
		botSess, bobSess int64
		run              schema.RunV1
		activity         schema.AgentActivityV1
		iv               schema.InterventionV1
	)
	steps := []struct {
		name  string
		act   func()
		match func(eventFrame) bool
		check func(t *testing.T, seen []eventFrame)
	}{
		{
			name:  "a session is created",
			act:   func() { botSess = f.create(t, f.botTok, interventionAgentBody).ID },
			match: isRoster(rosterHas(f.bot.ID, schema.SessionStatusIdle)),
		},
		{
			name:  "a run is created",
			act:   func() { run = f.start(t, f.botTok, botSess, "general", 0, "tidy the build") },
			match: isRun(func(r schema.RunV1) bool { return r.State == schema.RunStateQueued }),
			check: func(t *testing.T, seen []eventFrame) {
				// The roster for the new status comes with it.
				if !hasFrame(seen, isRoster(rosterHas(f.bot.ID, schema.SessionStatusRunning))) {
					t.Errorf("no roster showing the agent running in %v", seen)
				}
			},
		},
		{
			name:  "a run changes state",
			act:   func() { f.mustState(t, f.botTok, run.ID, schema.RunStateRunning) },
			match: isRun(func(r schema.RunV1) bool { return r.State == schema.RunStateRunning && r.ID == run.ID }),
		},
		{
			name: "activity is appended",
			act: func() {
				rec := f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, activityBody)
				if rec.Code != http.StatusCreated {
					t.Fatalf("append activity = %d %s", rec.Code, rec.Body)
				}
				activity = validActivity(t, rec)
			},
			match: func(fr eventFrame) bool { return fr.schema == schema.AgentActivitySchemaV1 },
			check: func(t *testing.T, seen []eventFrame) {
				got := seen[len(seen)-1].activity
				if got.ID != activity.ID || got.RunID != run.ID || got.Summary != activity.Summary {
					t.Errorf("activity frame = %+v, want %+v", got, activity)
				}
			},
		},
		{
			name:  "an intervention is created",
			act:   func() { iv = f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel) },
			match: func(fr eventFrame) bool { return fr.schema == schema.InterventionSchemaV1 },
			check: func(t *testing.T, seen []eventFrame) {
				got := seen[len(seen)-1].intervention
				if got.ID != iv.ID || got.State != schema.InterventionStateRequested {
					t.Errorf("intervention frame = %+v, want %d requested", got, iv.ID)
				}
			},
		},
		{
			name: "an intervention is answered",
			act: func() {
				f.mustAck(t, iv.ID, schema.InterventionStateApplied, "")
			},
			match: func(fr eventFrame) bool {
				return fr.schema == schema.InterventionSchemaV1 && fr.intervention.State == schema.InterventionStateApplied
			},
		},
		{
			name:  "a status changes",
			act:   func() { f.mustState(t, f.botTok, run.ID, schema.RunStateBlocked) },
			match: isRoster(rosterHas(f.bot.ID, schema.SessionStatusBlocked)),
		},
		{
			name: "the sweeper marks a session stale",
			act: func() {
				f.clock.Advance(DefaultSessionStale + time.Second)
				f.srv.sweepSessions(ctx)
			},
			match: isRoster(rosterHas(f.bot.ID, schema.SessionStatusStale)),
		},
		{
			name: "the sweeper marks it offline",
			act: func() {
				f.clock.Advance(DefaultSessionOffline)
				f.srv.sweepSessions(ctx)
			},
			match: isRoster(rosterHas(f.bot.ID, schema.SessionStatusOffline)),
		},
		{
			name: "a heartbeat revives it",
			act: func() {
				if rec := f.do(t, "POST", sessionPath(botSess, "/heartbeat"), f.botTok, ""); rec.Code != http.StatusOK {
					t.Fatalf("heartbeat = %d %s", rec.Code, rec.Body)
				}
			},
			match: isRoster(rosterHas(f.bot.ID, schema.SessionStatusBlocked)),
		},
		{
			name: "a session ends",
			act: func() {
				if rec := f.do(t, "DELETE", sessionPath(botSess, ""), f.botTok, ""); rec.Code != http.StatusNoContent {
					t.Fatalf("end session = %d %s", rec.Code, rec.Body)
				}
			},
			match: isRoster(rosterLacks(f.bot.ID)),
		},
		{
			name:  "a member's session appears with it",
			act:   func() { bobSess = f.create(t, f.bobTok, humanBody).ID },
			match: isRoster(rosterHas(f.bob.ID, schema.SessionStatusOnline)),
		},
		{
			name: "a member is removed",
			act: func() {
				if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/general/members/%d", f.bob.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
					t.Fatalf("remove member = %d %s", rec.Code, rec.Body)
				}
			},
			match: isRoster(rosterLacks(f.bob.ID)),
		},
		{
			name: "a member is added",
			act: func() {
				if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/general/members/%d", f.bob.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
					t.Fatalf("add member = %d %s", rec.Code, rec.Body)
				}
			},
			match: isRoster(rosterHas(f.bob.ID, schema.SessionStatusOnline)),
		},
	}
	for _, step := range steps {
		step.act()
		seen := alice.until(step.name, step.match)
		if step.check != nil {
			step.check(t, seen)
		}
	}
	_ = bobSess
}

func hasFrame(frames []eventFrame, match func(eventFrame) bool) bool {
	for _, fr := range frames {
		if match(fr) {
			return true
		}
	}
	return false
}

// TestEventsWhisperRunStaysInTheWhisper: a channel member outside a whisper
// receives nothing about a run that answers it, and its roster shows no
// current run or task for it, while the members in the whisper get all of it.
func TestEventsWhisperRunStaysInTheWhisper(t *testing.T) {
	f := &interventionFixture{runFixture: newRunFixture(t)}
	base := wsTestServer(t, f.srv)
	sess := f.session(t, f.botTok)

	alice := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
	bob := dialEvents(t, base, "/v1/events/ws?channel=general", f.bobTok)
	alice.next()
	bob.next()

	// Alice is in the whisper; bob and the operator are not.
	run := f.start(t, f.botTok, sess, "general", f.whisper, "answer alice about the deploy")
	rec := f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, activityBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("append activity = %d %s", rec.Code, rec.Body)
	}
	iv := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel)

	// Alice's socket carries all of it, and her roster names the run.
	var sawRunning bool
	for _, want := range []struct {
		name  string
		match func(eventFrame) bool
	}{
		{"the run", isRun(func(r schema.RunV1) bool { return r.ID == run.ID && r.MessageID == f.whisper })},
		{"the activity", func(fr eventFrame) bool {
			return fr.schema == schema.AgentActivitySchemaV1 && fr.activity.RunID == run.ID
		}},
		{"the intervention", func(fr eventFrame) bool {
			return fr.schema == schema.InterventionSchemaV1 && fr.intervention.ID == iv.ID
		}},
	} {
		for _, fr := range alice.until(want.name, want.match) {
			if fr.schema == schema.SessionRosterSchemaV1 {
				if s, ok := rosterSession(fr.roster, f.bot.ID); ok && s.CurrentRun == run.ID && s.Task != "" {
					sawRunning = true
				}
			}
		}
	}
	if !sawRunning {
		t.Error("alice's roster never named the run and its task")
	}

	// Bob is told the agent is busy and nothing more. A run in the open that
	// follows is the marker: everything he is owed about the whisper run would
	// have come before it.
	f.start(t, f.botTok, sess, "general", 0, "public work")
	var seen []eventFrame
	seen = append(seen, bob.until("the public run", isRun(func(r schema.RunV1) bool { return r.Task == "public work" }))...)
	if iv2 := f.mustAsk(t, f.aliceTok, run.ID, schema.InterventionKindCancel); iv2.ID == 0 {
		t.Fatal("second intervention was not created")
	}
	rec = f.do(t, "POST", runPath(run.ID, "/activity"), f.botTok, activityBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("append activity = %d %s", rec.Code, rec.Body)
	}
	f.start(t, f.botTok, sess, "general", 0, "more public work")
	seen = append(seen, bob.until("the next public run", isRun(func(r schema.RunV1) bool { return r.Task == "more public work" }))...)

	sawBusy := false
	for _, fr := range seen {
		switch fr.schema {
		case schema.RunSchemaV1:
			if fr.run.ID == run.ID {
				t.Errorf("bob was sent the whisper run: %v", fr)
			}
		case schema.AgentActivitySchemaV1:
			t.Errorf("bob was sent activity: %v", fr)
		case schema.InterventionSchemaV1:
			t.Errorf("bob was sent an intervention: %v", fr)
		case schema.SessionRosterSchemaV1:
			s, ok := rosterSession(fr.roster, f.bot.ID)
			if !ok {
				continue
			}
			if s.Status == schema.SessionStatusRunning {
				sawBusy = true
			}
			// The public runs may be named; the whisper run and its task may not.
			if s.CurrentRun == run.ID || s.Task == "answer alice about the deploy" {
				t.Errorf("bob's roster shows the whisper run: %+v", s)
			}
		}
	}
	if !sawBusy {
		t.Error("bob's roster never showed the agent running")
	}
}

// TestEventsRefusals: what the upgrade asks of the caller, answered before
// anything is upgraded.
func TestEventsRefusals(t *testing.T) {
	f := newRunFixture(t)
	base := wsTestServer(t, f.srv)
	botSess := f.session(t, f.botTok)
	aliceOther := f.create(t, f.aliceTok2, humanBody).ID
	ended := f.create(t, f.aliceTok, humanBody).ID
	if rec := f.do(t, "DELETE", sessionPath(ended, ""), f.aliceTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("end session = %d %s", rec.Code, rec.Body)
	}
	blind, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalAgent, "blind")
	if err != nil {
		t.Fatal(err)
	}
	_, blindTok, err := f.srv.store.CreateCredential(context.Background(), "system", blind.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.join(t, f.general.ID, blind.ID)
	f.grant(t, blind.ID, []schema.Capability{schema.CapabilitySessionsManage}, nil, map[int64][]schema.ChannelPermission{
		f.general.ID: {schema.ChannelPermissionRead},
	})

	tests := []struct {
		name      string
		path      string
		token     string
		status    int
		code      string
		sameAs404 bool // the answer must equal the one an unknown channel gets
	}{
		{"no channel", "/v1/events/ws", f.aliceTok, http.StatusBadRequest, "invalid_request", false},
		{"unknown channel", "/v1/events/ws?channel=nosuch", f.aliceTok, http.StatusNotFound, "channel_not_found", false},
		{"not a member", "/v1/events/ws?channel=general", f.carolTok, http.StatusNotFound, "channel_not_found", true},
		{"no credential", "/v1/events/ws?channel=general", "", http.StatusUnauthorized, "unauthenticated", false},
		{"another credential's session", fmt.Sprintf("/v1/events/ws?channel=general&session=%d", aliceOther), f.aliceTok, http.StatusNotFound, schema.ErrorCodeSessionNotFound, false},
		{"another principal's session", fmt.Sprintf("/v1/events/ws?channel=general&session=%d", botSess), f.aliceTok, http.StatusNotFound, schema.ErrorCodeSessionNotFound, false},
		{"a session that does not exist", "/v1/events/ws?channel=general&session=99999", f.aliceTok, http.StatusNotFound, schema.ErrorCodeSessionNotFound, false},
		{"a session that is not a number", "/v1/events/ws?channel=general&session=abc", f.aliceTok, http.StatusNotFound, schema.ErrorCodeSessionNotFound, false},
		{"a session of zero", "/v1/events/ws?channel=general&session=0", f.aliceTok, http.StatusNotFound, schema.ErrorCodeSessionNotFound, false},
		{"an ended session", fmt.Sprintf("/v1/events/ws?channel=general&session=%d", ended), f.aliceTok, http.StatusNotFound, schema.ErrorCodeSessionNotFound, false},
		{"an agent that may not read", "/v1/events/ws?channel=general", blindTok, http.StatusForbidden, "forbidden", false},
		{"an agent without sessions.manage keeping a session", fmt.Sprintf("/v1/events/ws?channel=general&session=%d", botSess), f.muteTok, http.StatusForbidden, "forbidden", false},
	}
	unknown := callWS(t, base, "/v1/events/ws?channel=nosuch", f.carolTok)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := callWS(t, base, tt.path, tt.token)
			if got.status != tt.status || !strings.Contains(got.body, `"`+tt.code+`"`) {
				t.Fatalf("answer = %d %s, want %d %s", got.status, got.body, tt.status, tt.code)
			}
			if tt.sameAs404 && got != unknown {
				t.Errorf("answer = %+v, want the unknown channel's %+v", got, unknown)
			}
		})
	}
	if n := f.srv.eventSubscribers(); n != 0 {
		t.Errorf("%d subscribers are registered after only refusals", n)
	}
}

// TestEventsAuthOff: sessions need a verified caller, so with authentication
// off the socket answers as the sessions routes do.
func TestEventsAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	base := wsTestServer(t, f.srv)
	got := callWS(t, base, "/v1/events/ws?channel=general", "")
	if got.status != http.StatusBadRequest || !strings.Contains(got.body, schema.ErrorCodeSessionsRequireAuth) {
		t.Fatalf("answer = %d %s, want 400 %s", got.status, got.body, schema.ErrorCodeSessionsRequireAuth)
	}
}

// TestEventsTicketAndBearer: the socket opens with either credential, and a
// ticket is spent by opening it.
func TestEventsTicketAndBearer(t *testing.T) {
	f := newSessionFixture(t)
	base := wsTestServer(t, f.srv)
	ticket := func() string {
		rec := f.do(t, "POST", "/v1/ws-tickets", f.aliceTok, "")
		if rec.Code != http.StatusCreated {
			t.Fatalf("issue ticket = %d %s", rec.Code, rec.Body)
		}
		return decodeBody[schema.WSTicketV1](t, rec).Ticket
	}
	tk := ticket()
	for name, c := range map[string]*eventsClient{
		"bearer": dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok),
		"ticket": dialEvents(t, base, "/v1/events/ws?channel=general&ticket="+tk, ""),
	} {
		if fr := c.next(); fr.schema != schema.SessionRosterSchemaV1 || fr.roster.ChannelID != f.general.ID {
			t.Errorf("%s: first frame = %v, want the roster of general", name, fr)
		}
	}
	if got := callWS(t, base, "/v1/events/ws?channel=general&ticket="+tk, ""); got.status != http.StatusUnauthorized {
		t.Errorf("a spent ticket = %d, want 401", got.status)
	}
	// A ticket is for a socket: another route wants a header.
	if got := f.do(t, "GET", "/v1/sessions?channel=general&ticket="+ticket(), "", ""); got.Code != http.StatusUnauthorized {
		t.Errorf("a ticket on the roster route = %d, want 401", got.Code)
	}
}

// TestEventsKeepsTheSessionAlive: with &session= the server refreshes that
// session on the keep-alive ticks, through the heartbeat's own store call, so
// it stays online past the stale threshold while another session of the same
// roster goes stale; a revived session is revived the same way; an ended one
// is not, and ending it closes the socket.
func TestEventsKeepsTheSessionAlive(t *testing.T) {
	f := newSessionFixture(t)
	ctx := context.Background()
	ticks := make(chan time.Time)
	f.srv.keepAliveTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	base := wsTestServer(t, f.srv)

	mine := f.create(t, f.aliceTok, humanBody)
	other := f.create(t, f.bobTok, humanBody)
	c := dialEvents(t, base, fmt.Sprintf("/v1/events/ws?channel=general&session=%d", mine.ID), f.aliceTok)
	c.next()
	// tick delivers one keep-alive and returns once the socket has dealt with
	// it: the second send is only taken when its loop is back at the select.
	// The second is a keep-alive too, and may still be running when tick
	// returns.
	tick := func() {
		t.Helper()
		for range 2 {
			select {
			case ticks <- f.clock.Now():
			case <-time.After(eventsReadTimeout):
				t.Fatal("the socket took no keep-alive tick")
			}
		}
	}

	// Five ticks ten seconds apart: past the stale threshold, with the
	// session refreshed each time.
	for range 5 {
		f.clock.Advance(10 * time.Second)
		tick()
	}
	f.srv.sweepSessions(ctx)
	rec := f.do(t, "GET", "/v1/sessions?channel=general", f.aliceTok, "")
	roster := validRoster(t, rec)
	if s, _ := rosterSession(roster, f.alice.ID); s.State != schema.SessionStateOnline {
		t.Errorf("the kept session is %q, want online: %+v", s.State, s)
	}
	if s, _ := rosterSession(roster, f.bob.ID); s.State != schema.SessionStateStale || s.ID != other.ID {
		t.Errorf("the session nobody kept is %q, want stale: %+v", s.State, s)
	}

	// The socket told its client: the kept session online, the other stale.
	c.until("a roster with the other session stale", isRoster(func(r schema.SessionRosterV1) bool {
		a, okA := rosterSession(r, f.alice.ID)
		b, okB := rosterSession(r, f.bob.ID)
		return okA && okB && a.State == schema.SessionStateOnline && b.State == schema.SessionStateStale
	}))

	// Let the kept session go quiet by not ticking; the sweeper marks it
	// stale, and the next tick revives it the way a heartbeat would.
	f.clock.Advance(DefaultSessionStale + time.Second)
	f.srv.sweepSessions(ctx)
	c.until("the kept session stale", isRoster(func(r schema.SessionRosterV1) bool {
		a, ok := rosterSession(r, f.alice.ID)
		return ok && a.State == schema.SessionStateStale
	}))
	tick()
	revived := c.until("the kept session online again", isRoster(func(r schema.SessionRosterV1) bool {
		a, ok := rosterSession(r, f.alice.ID)
		return ok && a.State == schema.SessionStateOnline
	}))
	got, _ := rosterSession(revived[len(revived)-1].roster, f.alice.ID)
	if want := schema.NewTimestamp(f.clock.Now()); got.LastSeenAt != want {
		t.Errorf("last_seen_at = %v, want the clock's %v", got.LastSeenAt, want)
	}

	// Ended while the socket is open: the next keep-alive finds it gone, does
	// not bring it back, and closes the socket with 1008 "session ended". (This
	// replaces the old assertion that the socket carried on.) The last tick()
	// left one heartbeat in flight, which may have run after the DELETE and
	// closed the socket already; either way the close is what is read, so the
	// extra tick is offered with a bound and not waited for.
	if rec := f.do(t, "DELETE", sessionPath(mine.ID, ""), f.aliceTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("end session = %d %s", rec.Code, rec.Body)
	}
	select {
	case ticks <- f.clock.Now():
	case <-time.After(time.Second):
	}
	if ce := c.closed(); ce.Code != websocket.StatusPolicyViolation || ce.Reason != "session ended" {
		t.Errorf("close after the session ended = %d %q, want 1008 %q", ce.Code, ce.Reason, "session ended")
	}
	if _, err := f.srv.store.OwnedSession(ctx, mine.ID, f.credentialID(t, f.aliceTok)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("OwnedSession of the ended session = %v, want ErrNotFound", err)
	}
}

// TestEventsClosesWhenTheSessionEnds: ending the session a socket keeps alive
// closes that socket with 1008 "session ended" on the next keep-alive tick and
// refuses its reconnect with 404 session_not_found; a socket on the same
// channel without &session= is not closed by it.
func TestEventsClosesWhenTheSessionEnds(t *testing.T) {
	f := newSessionFixture(t)
	ticks := make(chan time.Time)
	f.srv.keepAliveTicks = func() (<-chan time.Time, func()) { return ticks, func() {} }
	base := wsTestServer(t, f.srv)

	mine := f.create(t, f.aliceTok, humanBody)
	kept := dialEvents(t, base, fmt.Sprintf("/v1/events/ws?channel=general&session=%d", mine.ID), f.aliceTok)
	kept.next()
	plain := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
	plain.next()

	if rec := f.do(t, "DELETE", sessionPath(mine.ID, ""), f.aliceTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("end session = %d %s", rec.Code, rec.Body)
	}
	select {
	case ticks <- f.clock.Now():
	case <-time.After(eventsReadTimeout):
		t.Fatal("the socket took no keep-alive tick")
	}
	if ce := kept.closed(); ce.Code != websocket.StatusPolicyViolation || ce.Reason != "session ended" {
		t.Errorf("close = %d %q, want 1008 %q", ce.Code, ce.Reason, "session ended")
	}

	tests := []struct {
		name string
		path string
		want int
	}{
		{"reconnect with the ended session", fmt.Sprintf("/v1/events/ws?channel=general&session=%d", mine.ID), http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := callWS(t, base, tc.path, f.aliceTok)
			if got.status != tc.want || !strings.Contains(got.body, "session_not_found") {
				t.Errorf("= %d %s, want %d session_not_found", got.status, got.body, tc.want)
			}
		})
	}

	// The plain socket is still open: it is sent the roster without the ended
	// session, and a later session shows up on it.
	plain.until("the ended session gone", isRoster(rosterLacks(f.alice.ID)))
	f.create(t, f.bobTok, humanBody)
	plain.until("a later session", isRoster(func(r schema.SessionRosterV1) bool {
		_, ok := rosterSession(r, f.bob.ID)
		return ok
	}))
}

// TestEventsChannelRenamed (issue #727): a rename sends every events socket
// of the channel exactly one conch.channel.v1, the channel as it now is, and
// leaves the socket open; an events socket of another channel hears nothing,
// and the message and voice presence sockets of the channel, which are bound
// to its id, are sent nothing and stay open. The voice room is the same room.
func TestEventsChannelRenamed(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	base := wsTestServer(t, f.srv)
	room := f.inUse(t, f.ops)

	// Three events sockets of ops: ann twice (two tabs) and ann2.
	var renamedSocks []*eventsClient
	for _, who := range []string{"ann", "ann", "ann2"} {
		c := dialEvents(t, base, "/v1/events/ws?channel=ops", f.tokens[who])
		if first := c.next(); first.schema != schema.SessionRosterSchemaV1 {
			t.Fatalf("%s on ops: first frame = %v, want the roster", who, first)
		}
		renamedSocks = append(renamedSocks, c)
	}
	other := dialEvents(t, base, "/v1/events/ws?channel=ops2", f.tokens["ann"])
	if first := other.next(); first.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("ops2: first frame = %v, want the roster", first)
	}
	live := dialAs(t, ctx, base, "/v2/ws?channel=ops", f.tokens["ann"])
	voice, _, err := dialVoice(t, base, "ops", f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	readPresence(t, voice)

	rec := f.do(t, http.MethodPut, channelNamePath(f.ops.ID), f.rootTok, channelRenameBody(t, "ops", "ops-renamed"))
	if rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}

	want := schema.ChannelDocV1{Schema: schema.ChannelSchemaV1, ChannelID: f.ops.ID, Name: "ops-renamed", CreatedAt: schema.NewTimestamp(f.ops.CreatedAt)}
	for i, c := range renamedSocks {
		if fr := c.next(); fr.schema != schema.ChannelSchemaV1 || fr.channel != want {
			t.Fatalf("socket %d: frame after the rename = %v, want %+v", i, fr, want)
		}
	}

	// Exactly one, and the sockets still work: the next change to ops, made
	// by its new name, is the next thing each is sent, with no second
	// channel document before it.
	if rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/ops-renamed/members/%d", f.ids["bob"]), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("add bob to ops = %d %s", rec.Code, rec.Body)
	}
	for i, c := range renamedSocks {
		seen := c.until("bob added to ops", isMembers(func(d schema.ChannelMembersV1) bool {
			_, ok := listsMember(d, f.ids["bob"])
			return ok
		}))
		if n := countFrames(seen, schema.ChannelSchemaV1); n != 0 {
			t.Errorf("socket %d: %d more channel documents: %v", i, n, seen)
		}
	}

	// ops2 heard nothing about the rename of ops: no channel document comes
	// before the change made to it. ann owns this socket and is a member of
	// ops, so the rename also sent it conch.channels_changed.v1 (#1049); that
	// frame may arrive before or after the member list, depending on timing,
	// and is the only other frame allowed before it.
	if rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/ops2/members/%d", f.ids["bob"]), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("add bob to ops2 = %d %s", rec.Code, rec.Body)
	}
	seen := other.until("ops2's member list", isMembers(func(d schema.ChannelMembersV1) bool {
		return d.ChannelID == f.ops2.ID
	}))
	for _, fr := range seen[:len(seen)-1] {
		if fr.schema != schema.ChannelsChangedSchemaV1 {
			t.Errorf("ops2: frame before its member list = %v, want only %s: %v", fr, schema.ChannelsChangedSchemaV1, seen)
		}
	}

	// The message socket's next frame is the next message, posted by the
	// new name.
	posted := f.postAs(t, f.tokens["ann2"], "ops-renamed", "after the rename")
	rctx, cancel := context.WithTimeout(ctx, eventsReadTimeout)
	defer cancel()
	_, raw, err := live.Read(rctx)
	if err != nil {
		t.Fatalf("the message socket after the rename: %v", err)
	}
	var m schema.MessageV2
	if err := json.Unmarshal(raw, &m); err != nil || m.Validate() != nil || m.ID != posted || m.ChannelID != f.ops.ID {
		t.Errorf("the message socket's next frame = %s, want message %d", raw, posted)
	}

	// The presence socket's next frame is the next presence, in the same
	// room: readPresence fails on any other document.
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann2"), joinedMs: 1_000, published: true, muted: true})
	f.pass(t)
	if s := readPresence(t, voice); !s.Available || len(s.Rooms) != 1 || len(s.Rooms[0].Participants) != 1 || s.Rooms[0].Participants[0].PrincipalID != f.ids["ann2"] {
		t.Errorf("the presence socket's next frame = %+v, want ann2 in the room", s)
	}
	if after, err := f.srv.store.ChannelVoiceRoom(ctx, f.ops.ID); err != nil || after.RoomName != room {
		t.Errorf("voice room after the rename = %+v (%v), want %q", after, err, room)
	}
}
