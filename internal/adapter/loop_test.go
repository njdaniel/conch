package adapter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

// fakeMCP is the server as the adapter sees it. Every call is also recorded,
// in order, in events, so a test can say what came before what; the heartbeat
// and the intervention watcher call it from their own goroutines, hence mu.
type fakeMCP struct {
	mu    sync.Mutex
	pages map[int64]schema.ListMessagesResponseV2
	errAt map[int64]error
	reads []int64
	posts []string // bodies of successful posts
	// attempts records every post_message call, successful or refused, with
	// the audience it carried (nil is channel-wide).
	attempts []postAttempt
	postErr  error // returned by every post when set

	events      []string // "start_run", "update_run:running", "activity:command_started", ...
	registered  []schema.CreateSessionRequestV1
	runs        []schema.CreateRunRequestV1
	runStates   []string // state[:outcome] of each update_run
	activities  []activityCall
	acks        []ackCall
	approvals   []schema.CreateApprovalRequestV1
	interv      []schema.InterventionV1 // what people asked for; ack changes state
	decisions   []schema.AwaitDecisionOutput
	nextRunID   int64
	beats       chan struct{} // receives one value per heartbeat, if set
	polled      chan struct{} // receives one value per poll_interventions, if set
	startErr    error
	activityErr error
	endCalls    int
	// offline is how many more times each report ("start_run", "update_run",
	// "post_activity") is refused with session_offline; beatErr is returned
	// by every heartbeat when set.
	offline map[string]int
	beatErr error
	// agents are the authors that are agents; every other author in pages is a
	// human member. unlisted are authors the member list leaves out, and
	// membersErr fails the list.
	agents, unlisted map[int64]bool
	membersErr       error
	memberCalls      int
}

func (f *fakeMCP) members(context.Context, string) ([]schema.ChannelMemberV1, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.memberCalls++
	if f.membersErr != nil {
		return nil, f.membersErr
	}
	seen := map[int64]bool{}
	var out []schema.ChannelMemberV1
	for _, page := range f.pages {
		for _, m := range page.Messages {
			if seen[m.AuthorID] || f.unlisted[m.AuthorID] {
				continue
			}
			seen[m.AuthorID] = true
			kind := schema.PrincipalHuman
			if f.agents[m.AuthorID] {
				kind = schema.PrincipalAgent
			}
			out = append(out, schema.ChannelMemberV1{PrincipalID: m.AuthorID, Kind: kind})
		}
	}
	return out, nil
}

// refuseOffline reports whether this call is refused with session_offline,
// and counts the refusal down. The caller holds mu.
func (f *fakeMCP) refuseOffline(call string) error {
	if f.offline[call] > 0 {
		f.offline[call]--
		return &mcpclient.ToolError{Method: call, Code: schema.ErrorCodeSessionOffline}
	}
	return nil
}

type postAttempt struct {
	body     string
	audience *schema.Audience
}

type activityCall struct {
	runID                  int64
	kind                   schema.ActivityKind
	summary, outcome, data string
}

type ackCall struct {
	id     int64
	state  schema.InterventionState
	reason string
}

func (f *fakeMCP) record(event string) { f.events = append(f.events, event) }

func (f *fakeMCP) readChannel(_ context.Context, _ string, after int64, _ int) (schema.ListMessagesResponseV2, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, after)
	if err := f.errAt[after]; err != nil {
		return schema.ListMessagesResponseV2{}, err
	}
	return f.pages[after], nil
}

func (f *fakeMCP) postMessage(_ context.Context, _, body string, audience *schema.Audience) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("post_message")
	f.attempts = append(f.attempts, postAttempt{body: body, audience: audience})
	if f.postErr != nil {
		return f.postErr
	}
	f.posts = append(f.posts, body)
	return nil
}

func (f *fakeMCP) RegisterSession(_ context.Context, req schema.CreateSessionRequestV1) (schema.SessionV1, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("register_session")
	f.registered = append(f.registered, req)
	return schema.SessionV1{ID: 7}, nil
}

func (f *fakeMCP) Heartbeat(context.Context, int64) (schema.SessionV1, error) {
	f.mu.Lock()
	f.record("heartbeat")
	beats, beatErr := f.beats, f.beatErr
	f.mu.Unlock()
	if beats != nil {
		beats <- struct{}{}
	}
	return schema.SessionV1{ID: 7}, beatErr
}

func (f *fakeMCP) EndSession(context.Context, int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("end_session")
	f.endCalls++
	return nil
}

func (f *fakeMCP) StartRun(_ context.Context, _ int64, req schema.CreateRunRequestV1) (schema.RunV1, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refuseOffline("start_run"); err != nil {
		f.record("start_run:refused")
		return schema.RunV1{}, err
	}
	f.record("start_run")
	if f.startErr != nil {
		return schema.RunV1{}, f.startErr
	}
	f.runs = append(f.runs, req)
	f.nextRunID++
	return schema.RunV1{ID: f.nextRunID, ChannelID: 3}, nil
}

func (f *fakeMCP) UpdateRun(_ context.Context, runID int64, req schema.SetRunStateRequestV1) (schema.RunV1, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := string(req.State)
	if req.Outcome != "" {
		state += ":" + req.Outcome
	}
	if err := f.refuseOffline("update_run"); err != nil {
		f.record("update_run:" + state + ":refused")
		return schema.RunV1{}, err
	}
	f.record("update_run:" + state)
	f.runStates = append(f.runStates, state)
	return schema.RunV1{ID: runID}, nil
}

func (f *fakeMCP) PostActivity(_ context.Context, runID int64, req schema.AppendActivityRequestV1) (schema.AgentActivityV1, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refuseOffline("post_activity"); err != nil {
		f.record("activity:" + string(req.Kind) + ":refused")
		return schema.AgentActivityV1{}, err
	}
	f.record("activity:" + string(req.Kind))
	f.activities = append(f.activities, activityCall{runID: runID, kind: req.Kind, summary: req.Summary, outcome: req.Outcome, data: string(req.Data)})
	return schema.AgentActivityV1{}, f.activityErr
}

func (f *fakeMCP) PollInterventions(_ context.Context, _ int64, state schema.InterventionState, after int64) (schema.ListInterventionsResponseV1, error) {
	f.mu.Lock()
	list := schema.ListInterventionsResponseV1{Interventions: []schema.InterventionV1{}}
	for _, iv := range f.interv {
		if iv.State == state && iv.ID > after {
			list.Interventions = append(list.Interventions, iv)
		}
	}
	polled := f.polled
	f.mu.Unlock()
	if polled != nil {
		polled <- struct{}{}
	}
	return list, nil
}

func (f *fakeMCP) AnswerIntervention(_ context.Context, id int64, req schema.AckInterventionRequestV1) (schema.InterventionV1, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(fmt.Sprintf("ack:%d:%s", id, req.State))
	f.acks = append(f.acks, ackCall{id: id, state: req.State, reason: req.Reason})
	for i := range f.interv {
		if f.interv[i].ID == id {
			f.interv[i].State = req.State
			return f.interv[i], nil
		}
	}
	return schema.InterventionV1{}, errors.New("no such intervention")
}

func (f *fakeMCP) RequestApproval(_ context.Context, req schema.CreateApprovalRequestV1) (schema.RequestApprovalOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("request_approval")
	f.approvals = append(f.approvals, req)
	return schema.RequestApprovalOutput{ID: 41, Title: req.Title, State: schema.ApprovalStatePending}, nil
}

func (f *fakeMCP) AwaitDecision(context.Context, int64, int64) (schema.AwaitDecisionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("await_decision")
	if len(f.decisions) == 0 {
		return schema.AwaitDecisionOutput{State: schema.ApprovalStateResolved, Resolution: &schema.ApprovalResolutionV1{ApprovalID: 41, Outcome: schema.OutcomeApproved}}, nil
	}
	next := f.decisions[0]
	f.decisions = f.decisions[1:]
	return next, nil
}

// eventLog is a copy of the recorded calls, for a test that is done with the
// goroutines that made them.
func (f *fakeMCP) eventLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

// fakeClaude stands in for the command. fn, when set, is the reply: it gets the
// context, the prompt and the run's fixture handler.
type fakeClaude struct {
	reply   string
	err     error
	prompts []string
	fn      func(ctx context.Context, prompt string, fx fixtureHandler) (string, error)
}

func (f *fakeClaude) Reply(ctx context.Context, prompt string, fx fixtureHandler) (string, error) {
	f.prompts = append(f.prompts, prompt)
	if f.fn != nil {
		return f.fn(ctx, prompt, fx)
	}
	return f.reply, f.err
}

func message(id, author int64, body string) schema.MessageV2 {
	return schema.MessageV2{ID: id, AuthorID: author, Body: body}
}

func testConfig() config {
	return config{Channel: "ops", PrincipalID: 9, ContextMessages: 20, PollInterval: time.Second, MaxBackoff: 8 * time.Second}
}

func TestPollOnceCursorAdvancement(t *testing.T) {
	tests := []struct {
		name      string
		pages     map[int64]schema.ListMessagesResponseV2
		wantSeen  int64
		wantReads []int64
	}{
		{
			name:     "single final page with omitted next_after",
			pages:    map[int64]schema.ListMessagesResponseV2{3: {Messages: []schema.MessageV2{message(4, 9, "self")}}},
			wantSeen: 4, wantReads: []int64{3},
		},
		{
			name: "multiple pages",
			pages: map[int64]schema.ListMessagesResponseV2{
				3: {Messages: []schema.MessageV2{message(4, 9, "self")}, NextAfter: 4},
				4: {Messages: []schema.MessageV2{message(5, 9, "self"), message(6, 9, "self")}},
			},
			wantSeen: 6, wantReads: []int64{3, 4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{pages: tt.pages}
			loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: &fakeClaude{}, lastSeen: 3}
			if err := loop.pollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if loop.lastSeen != tt.wantSeen {
				t.Fatalf("lastSeen = %d, want %d", loop.lastSeen, tt.wantSeen)
			}
			if !reflect.DeepEqual(mcp.reads, tt.wantReads) {
				t.Fatalf("reads = %v, want %v", mcp.reads, tt.wantReads)
			}
		})
	}
}

func TestPollOnceFiltersSelfAndAdvancesCursor(t *testing.T) {
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{
		2: {Messages: []schema.MessageV2{message(3, 9, "ignore"), message(4, 2, "hello"), message(5, 9, "ignore too")}},
		0: {Messages: []schema.MessageV2{message(1, 3, "context"), message(2, 4, "older")}},
	}}
	claude := &fakeClaude{reply: "hi"}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude, lastSeen: 2}
	if err := loop.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if loop.lastSeen != 5 {
		t.Fatalf("lastSeen = %d, want 5", loop.lastSeen)
	}
	if len(claude.prompts) != 1 {
		t.Fatalf("Claude calls = %d, want 1", len(claude.prompts))
	}
	if strings.Contains(claude.prompts[0], "ignore") || !strings.Contains(claude.prompts[0], "2: hello") {
		t.Fatalf("prompt did not filter self messages:\n%s", claude.prompts[0])
	}
	if !reflect.DeepEqual(mcp.posts, []string{"hi"}) {
		t.Fatalf("posts = %v", mcp.posts)
	}
}

func TestPollOnceWhitespaceReplyDoesNotPost(t *testing.T) {
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{1: {Messages: []schema.MessageV2{message(2, 3, "question")}}, 0: {Messages: []schema.MessageV2{message(1, 2, "old")}}}}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: &fakeClaude{reply: " \n\t"}, lastSeen: 1}
	if err := loop.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mcp.posts) != 0 {
		t.Fatalf("posts = %v, want none", mcp.posts)
	}
}

func TestPollOnceContextComesFromRollingBufferNotFullReplay(t *testing.T) {
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{
		0: {Messages: []schema.MessageV2{message(1, 5, "seed one"), message(2, 5, "seed two")}},
		2: {Messages: []schema.MessageV2{message(3, 4, "first human line")}},
		3: {Messages: []schema.MessageV2{message(4, 4, "second human line")}},
	}}
	claude := &fakeClaude{reply: "ack"}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude}

	if err := loop.seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := loop.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := loop.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The channel's true start (cursor 0) must be read exactly once, during
	// seed — never again on later polls, even though every poll here needs
	// context (each new batch is a single message, well under
	// ContextMessages=20). A regression back to re-draining from 0 for
	// context would show up here as extra 0 reads.
	wantReads := []int64{0, 2, 3}
	if !reflect.DeepEqual(mcp.reads, wantReads) {
		t.Fatalf("reads = %v, want %v (context must come from the in-memory rolling buffer, not a full re-drain)", mcp.reads, wantReads)
	}
	if len(claude.prompts) != 2 {
		t.Fatalf("Claude calls = %d, want 2", len(claude.prompts))
	}
	// The second poll's prompt must carry the seeded backlog plus the first
	// poll's message as rolling context, proving the buffer actually
	// accumulates rather than just replacing itself.
	second := claude.prompts[1]
	for _, want := range []string{"5: seed one", "5: seed two", "4: first human line", "4: second human line"} {
		if !strings.Contains(second, want) {
			t.Fatalf("second prompt missing %q:\n%s", want, second)
		}
	}
}

func TestBuildPromptTruncatesContextAndPreservesOrdering(t *testing.T) {
	prompt := buildPrompt(
		[]schema.MessageV2{message(1, 1, "first"), message(2, 2, "second"), message(3, 3, "third")},
		[]schema.MessageV2{message(4, 4, "new one"), message(5, 5, "new two")}, 4, nil,
	)
	wantOrder := []string{"2: second", "3: third", "4: new one", "5: new two"}
	position := -1
	for _, text := range wantOrder {
		next := strings.Index(prompt, text)
		if next <= position {
			t.Fatalf("%q not in order in prompt:\n%s", text, prompt)
		}
		position = next
	}
	if strings.Contains(prompt, "1: first") {
		t.Fatalf("prompt retained truncated context:\n%s", prompt)
	}
}

func TestLoadConfig(t *testing.T) {
	base := map[string]string{"CONCH_BOT_TOKEN": "token", "CONCH_BOT_PRINCIPAL_ID": "7", "CONCH_BOT_CHANNEL": "ops"}
	lookup := func(values map[string]string) envLookup {
		return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	}
	t.Run("defaults", func(t *testing.T) {
		cfg, err := loadConfig(lookup(base))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Server != "http://127.0.0.1:8080" || cfg.PollInterval != 5*time.Second || cfg.MaxBackoff != time.Minute || cfg.ContextMessages != 20 || cfg.Model != "sonnet" || cfg.ReplyTimeout != 2*time.Minute || cfg.Command.args[0] != "claude" || cfg.LockFile == "" {
			t.Fatalf("unexpected defaults: %+v", cfg)
		}
	})
	for _, key := range []string{"CONCH_BOT_TOKEN", "CONCH_BOT_PRINCIPAL_ID", "CONCH_BOT_CHANNEL"} {
		t.Run("missing "+key, func(t *testing.T) {
			values := map[string]string{}
			for k, v := range base {
				values[k] = v
			}
			delete(values, key)
			_, err := loadConfig(lookup(values))
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("error = %v, want clear %s error", err, key)
			}
		})
	}
	t.Run("comma channel", func(t *testing.T) {
		values := map[string]string{}
		for k, v := range base {
			values[k] = v
		}
		values["CONCH_BOT_CHANNEL"] = "ops,dev"
		_, err := loadConfig(lookup(values))
		if err == nil || !strings.Contains(err.Error(), "comma") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRunBackoffErrorErrorSuccess(t *testing.T) {
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{0: {}}, errAt: map[int64]error{}}
	claude := &fakeClaude{}
	ctx, cancel := context.WithCancel(context.Background())
	var delays []time.Duration
	iterations := 0
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude}
	loop.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		iterations++
		switch iterations {
		case 1:
			mcp.errAt[0] = errors.New("second failure")
		case 2:
			delete(mcp.errAt, 0)
		case 3:
			cancel()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	// Make the first iteration fail after the successful seed.
	mcp.errAt[0] = nil
	seedCalls := 0
	originalSleep := loop.sleep
	loop.sleep = func(ctx context.Context, delay time.Duration) error { return originalSleep(ctx, delay) }
	// The fake distinguishes seed from polling by installing the first error
	// immediately after seed through a wrapper client.
	wrapped := &seedAwareMCP{fakeMCP: mcp, calls: &seedCalls}
	loop.mcp = wrapped
	if err := loop.run(ctx); err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{2 * time.Second, 4 * time.Second, time.Second}; !reflect.DeepEqual(delays, want) {
		t.Fatalf("delays = %v, want %v", delays, want)
	}
}

type seedAwareMCP struct {
	*fakeMCP
	calls *int
}

func (s *seedAwareMCP) readChannel(ctx context.Context, channel string, after int64, limit int) (schema.ListMessagesResponseV2, error) {
	*s.calls++
	if *s.calls == 2 {
		return schema.ListMessagesResponseV2{}, errors.New("first failure")
	}
	return s.fakeMCP.readChannel(ctx, channel, after, limit)
}

func scoped(id, author int64, body string, audience schema.Audience) schema.MessageV2 {
	m := message(id, author, body)
	m.Audience = &audience
	return m
}

// pagesFor builds fake pages so that each batch is read after the previous
// batch's last id.
func pagesFor(batches ...[]schema.MessageV2) map[int64]schema.ListMessagesResponseV2 {
	pages := map[int64]schema.ListMessagesResponseV2{}
	after := int64(0)
	for _, batch := range batches {
		pages[after] = schema.ListMessagesResponseV2{Messages: batch}
		if len(batch) > 0 {
			after = batch[len(batch)-1].ID
		}
	}
	return pages
}

var (
	testNet     = schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}
	otherNet    = schema.Audience{Kind: schema.AudienceKindNet, NetID: 5}
	testWhisper = schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{2, 9}}
	unknownKind = schema.Audience{Kind: "everyone", NetID: 4}
)

// The reply goes to the audience of the message it answers, deep-equal to the
// audience that was read.
func TestReplyAudienceMatchesIncoming(t *testing.T) {
	tests := []struct {
		name string
		in   schema.MessageV2
		want *schema.Audience
	}{
		{"channel-wide", message(1, 2, "q"), nil},
		{"net", scoped(1, 2, "q", testNet), &testNet},
		{"whisper", scoped(1, 2, "q", testWhisper), &testWhisper},
		{"unknown kind is scoped and sent back unchanged", scoped(1, 2, "q", unknownKind), &unknownKind},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{pages: pagesFor(nil, []schema.MessageV2{tt.in})}
			claude := &fakeClaude{reply: "an answer"}
			loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude}
			if err := loop.pollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(mcp.attempts) != 1 || len(claude.prompts) != 1 {
				t.Fatalf("attempts = %d, prompts = %d; want one each", len(mcp.attempts), len(claude.prompts))
			}
			if got := mcp.attempts[0].audience; !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("reply audience = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// One poll with several audiences: each gets its own model call and reply, and
// no prompt carries a body from another audience.
func TestGroupsRepliesByAudienceWithoutCrossTalk(t *testing.T) {
	batch := []schema.MessageV2{
		message(1, 2, "OPEN-one"),
		scoped(2, 2, "NET4-one", testNet),
		message(3, 3, "OPEN-two"),
		scoped(4, 2, "WHISPER-one", testWhisper),
		scoped(5, 3, "NET5-one", otherNet),
		scoped(6, 3, "NET4-two", testNet),
	}
	mcp := &fakeMCP{pages: pagesFor(nil, batch)}
	claude := &fakeClaude{reply: "an answer"}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude}
	if err := loop.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Replies are in order of each audience's first message.
	wantAudiences := []*schema.Audience{nil, &testNet, &testWhisper, &otherNet}
	if len(mcp.attempts) != len(wantAudiences) || len(claude.prompts) != len(wantAudiences) {
		t.Fatalf("attempts = %d, prompts = %d; want %d each", len(mcp.attempts), len(claude.prompts), len(wantAudiences))
	}
	wantBodies := [][]string{{"OPEN-one", "OPEN-two"}, {"NET4-one", "NET4-two"}, {"WHISPER-one"}, {"NET5-one"}}
	all := []string{"OPEN-one", "OPEN-two", "NET4-one", "NET4-two", "WHISPER-one", "NET5-one"}
	for i, want := range wantAudiences {
		if !reflect.DeepEqual(mcp.attempts[i].audience, want) {
			t.Errorf("reply %d audience = %+v, want %+v", i, mcp.attempts[i].audience, want)
		}
		for _, body := range all {
			has := strings.Contains(claude.prompts[i], body)
			shouldHave := false
			for _, w := range wantBodies[i] {
				shouldHave = shouldHave || w == body
			}
			if has != shouldHave {
				t.Errorf("prompt %d contains %q = %v, want %v:\n%s", i, body, has, shouldHave, claude.prompts[i])
			}
		}
	}
}

// History is per audience: scoped context, including what was seen while
// seeding, never appears in another audience's "Recent context".
func TestContextIsPerAudience(t *testing.T) {
	seed := []schema.MessageV2{message(1, 3, "OPEN-seed"), scoped(2, 2, "NET-seed", testNet), scoped(3, 2, "WHISPER-seed", testWhisper)}
	first := []schema.MessageV2{scoped(4, 2, "NET-live", testNet), message(5, 3, "OPEN-live")}
	second := []schema.MessageV2{scoped(6, 2, "NET-later", testNet), scoped(7, 2, "WHISPER-later", testWhisper)}
	mcp := &fakeMCP{pages: pagesFor(seed, first, second)}
	claude := &fakeClaude{reply: "an answer"}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude}
	if err := loop.seed(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := loop.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// prompts: [net, open] from the first poll, [net, whisper] from the second.
	if len(claude.prompts) != 4 {
		t.Fatalf("prompts = %d, want 4", len(claude.prompts))
	}
	tests := []struct {
		prompt     int
		has        []string
		mustNotHas []string
	}{
		{0, []string{"NET-seed", "NET-live"}, []string{"OPEN", "WHISPER"}},
		{1, []string{"OPEN-seed", "OPEN-live"}, []string{"NET", "WHISPER"}},
		{2, []string{"NET-seed", "NET-live", "NET-later"}, []string{"OPEN", "WHISPER"}},
		{3, []string{"WHISPER-seed", "WHISPER-later"}, []string{"OPEN", "NET"}},
	}
	for _, tt := range tests {
		p := claude.prompts[tt.prompt]
		for _, want := range tt.has {
			if !strings.Contains(p, want) {
				t.Errorf("prompt %d lacks %q:\n%s", tt.prompt, want, p)
			}
		}
		for _, bad := range tt.mustNotHas {
			if strings.Contains(p, bad) {
				t.Errorf("prompt %d leaks %q:\n%s", tt.prompt, bad, p)
			}
		}
	}
}

func TestContextWindowsAreBounded(t *testing.T) {
	cfg := testConfig()
	cfg.ContextMessages = 3
	loop := &botLoop{cfg: cfg}
	key := audienceKey(&testNet)
	for i := int64(1); i <= 10; i++ {
		loop.remember(key, []schema.MessageV2{message(i, 2, "m")})
	}
	if got := loop.recentSnapshot(key); len(got) != 3 || got[0].ID != 8 {
		t.Fatalf("window = %+v, want the last 3 messages", got)
	}
	// More audiences than maxAudienceWindows: the count stays capped and the
	// least recently touched window is the one dropped.
	loop = &botLoop{cfg: cfg}
	for i := int64(1); i <= maxAudienceWindows+5; i++ {
		loop.remember(audienceKey(&schema.Audience{Kind: schema.AudienceKindNet, NetID: i}), []schema.MessageV2{message(i, 2, "m")})
	}
	if len(loop.recent) != maxAudienceWindows {
		t.Fatalf("windows = %d, want %d", len(loop.recent), maxAudienceWindows)
	}
	if loop.recentSnapshot(audienceKey(&schema.Audience{Kind: schema.AudienceKindNet, NetID: 1})) != nil {
		t.Fatal("the oldest window was not evicted")
	}
	if loop.recentSnapshot(audienceKey(&schema.Audience{Kind: schema.AudienceKindNet, NetID: maxAudienceWindows + 5})) == nil {
		t.Fatal("the newest window was evicted")
	}
}

// A refused scoped reply is logged and dropped: nothing is posted, no second
// attempt with another audience, no error (which would back off), and the
// cursor is past the messages.
func TestRefusedScopedReplyIsDropped(t *testing.T) {
	for _, code := range []string{"forbidden", "net_not_found", "invalid_audience"} {
		for _, audience := range []*schema.Audience{&testNet, &testWhisper, &unknownKind} {
			t.Run(code+"/"+string(audience.Kind), func(t *testing.T) {
				mcp := &fakeMCP{
					pages:   pagesFor(nil, []schema.MessageV2{scoped(1, 2, "q", *audience), scoped(2, 2, "q2", *audience)}),
					postErr: fmt.Errorf("post: %w", &mcpclient.ToolError{Method: "tools/call", Code: code}),
				}
				loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: &fakeClaude{reply: "an answer"}}
				if err := loop.pollOnce(context.Background()); err != nil {
					t.Fatalf("a refusal must not be an error: %v", err)
				}
				if len(mcp.posts) != 0 {
					t.Fatalf("posts = %v, want none", mcp.posts)
				}
				if len(mcp.attempts) != 1 || !reflect.DeepEqual(mcp.attempts[0].audience, audience) {
					t.Fatalf("attempts = %+v, want exactly one with the original audience", mcp.attempts)
				}
				if loop.lastSeen != 2 {
					t.Fatalf("lastSeen = %d, want 2", loop.lastSeen)
				}
				// The next poll does not look at them again.
				if err := loop.pollOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
				if len(mcp.attempts) != 1 {
					t.Fatalf("attempts after next poll = %d, want 1", len(mcp.attempts))
				}
			})
		}
	}
}

// A non-refusal failure (transport) is an error, but still never widens, does
// not stop another audience's reply, and the messages are not read again.
func TestFailedScopedReplyDoesNotWiden(t *testing.T) {
	mcp := &fakeMCP{
		pages:   pagesFor(nil, []schema.MessageV2{scoped(1, 2, "q", testNet), message(2, 3, "open")}),
		postErr: errors.New("connection reset"),
	}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: &fakeClaude{reply: "an answer"}}
	if err := loop.pollOnce(context.Background()); err == nil {
		t.Fatal("a transport failure should be reported")
	}
	if len(mcp.attempts) != 2 || !reflect.DeepEqual(mcp.attempts[0].audience, &testNet) || mcp.attempts[1].audience != nil {
		t.Fatalf("attempts = %+v: want one net attempt then the channel-wide reply to the open message", mcp.attempts)
	}
	if loop.lastSeen != 2 {
		t.Fatalf("lastSeen = %d, want 2", loop.lastSeen)
	}
	if err := loop.pollOnce(context.Background()); err != nil || len(mcp.attempts) != 2 {
		t.Fatalf("the messages were retried: err=%v attempts=%d", err, len(mcp.attempts))
	}
}

// The bot's own scoped messages get no reply, but are context for that same
// audience only.
func TestOwnScopedMessagesAreContextOnly(t *testing.T) {
	first := []schema.MessageV2{scoped(1, 9, "OWN-net", testNet), message(2, 3, "open question")}
	second := []schema.MessageV2{scoped(3, 2, "net question", testNet)}
	mcp := &fakeMCP{pages: pagesFor(nil, first, second)}
	claude := &fakeClaude{reply: "an answer"}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude}
	for i := 0; i < 2; i++ {
		if err := loop.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(mcp.attempts) != 2 || mcp.attempts[0].audience != nil || !reflect.DeepEqual(mcp.attempts[1].audience, &testNet) {
		t.Fatalf("attempts = %+v: want a channel-wide reply, then a net reply", mcp.attempts)
	}
	if strings.Contains(claude.prompts[0], "OWN-net") {
		t.Errorf("own net message leaked into the channel-wide prompt:\n%s", claude.prompts[0])
	}
	if !strings.Contains(claude.prompts[1], "9: OWN-net") {
		t.Errorf("own net message is not context for the net:\n%s", claude.prompts[1])
	}
}

func TestAudienceKey(t *testing.T) {
	tests := []struct {
		name string
		a, b *schema.Audience
		same bool
	}{
		{"channel-wide equals itself", nil, nil, true},
		{"same net", &testNet, &schema.Audience{Kind: schema.AudienceKindNet, NetID: 4}, true},
		{"different nets", &testNet, &otherNet, false},
		{"whispers with the same participants in another order", &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{3, 1, 2}}, &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{1, 2, 3}}, true},
		{"whispers with different participants", &testWhisper, &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{2, 9, 10}}, false},
		{"net vs whisper", &testNet, &testWhisper, false},
		{"net 4 vs whisper to principal 4", &testNet, &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{4}}, false},
		{"unknown kind vs channel-wide", &unknownKind, nil, false},
		{"unknown kind vs net with the same id", &unknownKind, &testNet, false},
		{"unknown kind vs another unknown kind", &unknownKind, &schema.Audience{Kind: "other", NetID: 4}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := audienceKey(tt.a) == audienceKey(tt.b); got != tt.same {
				t.Fatalf("keys equal = %v, want %v", got, tt.same)
			}
		})
	}
	// Keying does not reorder the audience the bot sends back.
	w := schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{3, 1}}
	_ = audienceKey(&w)
	if !reflect.DeepEqual(w.PrincipalIDs, []int64{3, 1}) {
		t.Fatalf("audienceKey mutated its input: %v", w.PrincipalIDs)
	}
}

// Scoped messages never reach a prompt for a different audience, and the
// cursor moves past everything. (Before issue #120 the bot dropped scoped
// messages outright; now it answers them in their own audience.)
func TestScopedMessagesNeverReachAnotherAudiencesPrompt(t *testing.T) {
	tests := []struct {
		name         string
		seed         []schema.MessageV2
		first        []schema.MessageV2
		second       []schema.MessageV2
		wantLastSeen int64
	}{
		{"only scoped messages", nil, []schema.MessageV2{scoped(1, 2, "SECRET-net", testNet), scoped(2, 2, "SECRET-whisper", testWhisper)}, nil, 2},
		{"scoped beside channel-wide", nil, []schema.MessageV2{scoped(1, 2, "SECRET-net", testNet), message(2, 3, "open question"), scoped(3, 2, "SECRET-whisper", testWhisper)}, nil, 3},
		{"a scoped message is not context for a later channel-wide reply", nil, []schema.MessageV2{scoped(1, 2, "SECRET-net", testNet)}, []schema.MessageV2{message(2, 3, "open question")}, 2},
		{"a scoped message seen while seeding is not context either", []schema.MessageV2{message(1, 3, "old open"), scoped(2, 2, "SECRET-seed", testWhisper)}, []schema.MessageV2{message(3, 3, "open question")}, nil, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{pages: pagesFor(tt.seed, tt.first, tt.second)}
			claude := &fakeClaude{reply: "an answer"}
			loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude}
			if tt.seed != nil {
				if err := loop.seed(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			for _, batch := range [][]schema.MessageV2{tt.first, tt.second} {
				if batch == nil {
					continue
				}
				if err := loop.pollOnce(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			if len(claude.prompts) != len(mcp.attempts) {
				t.Fatalf("prompts = %d, replies = %d", len(claude.prompts), len(mcp.attempts))
			}
			for i, prompt := range claude.prompts {
				if mcp.attempts[i].audience == nil && strings.Contains(prompt, "SECRET") {
					t.Errorf("a scoped message reached a channel-wide prompt:\n%s", prompt)
				}
			}
			if loop.lastSeen != tt.wantLastSeen {
				t.Errorf("lastSeen = %d, want %d", loop.lastSeen, tt.wantLastSeen)
			}
		})
	}
}

// registerPostsMCP is a server where a person posts the instant the adapter's
// session opens: the message a check sends when it sees the session online.
type registerPostsMCP struct {
	*fakeMCP
	posted schema.MessageV2
}

func (r *registerPostsMCP) RegisterSession(ctx context.Context, req schema.CreateSessionRequestV1) (schema.SessionV1, error) {
	r.mu.Lock()
	r.pages[0] = schema.ListMessagesResponseV2{Messages: []schema.MessageV2{r.posted}}
	r.mu.Unlock()
	return r.fakeMCP.RegisterSession(ctx, req)
}

// A message sent as soon as the session is online must be answered: the
// session opening is the signal that the adapter is listening, so the cursor
// is seeded before it, not after.
func TestRunAnswersMessageSentWhenSessionOpens(t *testing.T) {
	mcp := &registerPostsMCP{
		fakeMCP: &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{0: {}}},
		posted:  message(1, 5, "please answer"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: &fakeClaude{reply: "answered"}}
	loop.sleep = func(context.Context, time.Duration) error {
		cancel()
		return ctx.Err()
	}
	if err := loop.run(ctx); err != nil {
		t.Fatal(err)
	}
	if want := []string{"answered"}; !reflect.DeepEqual(mcp.posts, want) {
		t.Fatalf("posts = %v, want %v", mcp.posts, want)
	}
}

func TestPollOnceAgentMessages(t *testing.T) {
	tests := []struct {
		name       string
		messages   []schema.MessageV2
		agents     map[int64]bool
		unlisted   map[int64]bool
		replyAgent bool
		wantRuns   int
		wantIn     []string
	}{
		{
			name:     "an agent message alone starts no run",
			messages: []schema.MessageV2{message(1, 2, "from an agent")},
			agents:   map[int64]bool{2: true},
			wantRuns: 0,
		},
		{
			name:     "a human message with an agent message is one run with both",
			messages: []schema.MessageV2{message(1, 2, "from an agent"), message(2, 3, "from a person")},
			agents:   map[int64]bool{2: true},
			wantRuns: 1,
			wantIn:   []string{"2: from an agent", "3: from a person"},
		},
		{
			name:       "opt in answers an agent message",
			messages:   []schema.MessageV2{message(1, 2, "from an agent")},
			agents:     map[int64]bool{2: true},
			replyAgent: true,
			wantRuns:   1,
			wantIn:     []string{"2: from an agent"},
		},
		{
			name:     "an author the member list does not know is not answered",
			messages: []schema.MessageV2{message(1, 4, "who is this")},
			unlisted: map[int64]bool{4: true},
			wantRuns: 0,
		},
		{
			name:     "its own message is never answered",
			messages: []schema.MessageV2{message(1, 9, "mine")},
			wantRuns: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{0: {Messages: tt.messages}}, agents: tt.agents, unlisted: tt.unlisted}
			claude := &fakeClaude{reply: "ok"}
			cfg := testConfig()
			cfg.ReplyToAgents = tt.replyAgent
			loop := &botLoop{cfg: cfg, mcp: mcp, claude: claude}
			if err := loop.pollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(claude.prompts) != tt.wantRuns {
				t.Fatalf("runs = %d, want %d", len(claude.prompts), tt.wantRuns)
			}
			for _, want := range tt.wantIn {
				if !strings.Contains(claude.prompts[0], want) {
					t.Errorf("prompt lacks %q:\n%s", want, claude.prompts[0])
				}
			}
			if loop.lastSeen != tt.messages[len(tt.messages)-1].ID {
				t.Errorf("lastSeen = %d", loop.lastSeen)
			}
		})
	}
}

// The agent message stays in the audience's window, so it is in the prompt of
// the next human message even though it started nothing.
func TestAgentMessageIsHistoryForLaterHumanMessage(t *testing.T) {
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{
		0: {Messages: []schema.MessageV2{message(1, 2, "agent said")}},
		1: {Messages: []schema.MessageV2{message(2, 3, "person asks")}},
	}, agents: map[int64]bool{2: true}}
	claude := &fakeClaude{reply: "ok"}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude}
	for range 2 {
		if err := loop.pollOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(claude.prompts) != 1 || !strings.Contains(claude.prompts[0], "2: agent said") || !strings.Contains(claude.prompts[0], "3: person asks") {
		t.Fatalf("prompts = %q", claude.prompts)
	}
}

// A failed member lookup must not swallow the message: the cursor stays, and
// the next poll answers it.
func TestMemberLookupFailureRetriesMessage(t *testing.T) {
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV2{0: {Messages: []schema.MessageV2{message(1, 3, "hello")}}}, membersErr: errors.New("down")}
	claude := &fakeClaude{reply: "ok"}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: claude}
	if err := loop.pollOnce(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if loop.lastSeen != 0 || len(claude.prompts) != 0 {
		t.Fatalf("lastSeen = %d, runs = %d", loop.lastSeen, len(claude.prompts))
	}
	mcp.membersErr = nil
	if err := loop.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(claude.prompts) != 1 {
		t.Fatalf("runs = %d, want 1", len(claude.prompts))
	}
}

// Two adapters in one channel, one human message: each replies once, then
// neither answers the other's reply.
func TestTwoAdaptersAnswerOnceAndStop(t *testing.T) {
	pages := func() map[int64]schema.ListMessagesResponseV2 {
		return map[int64]schema.ListMessagesResponseV2{0: {Messages: []schema.MessageV2{message(1, 3, "hello")}}}
	}
	cfgA, cfgB := testConfig(), testConfig()
	cfgA.PrincipalID, cfgB.PrincipalID = 11, 12
	mcpA := &fakeMCP{pages: pages(), agents: map[int64]bool{11: true, 12: true}}
	mcpB := &fakeMCP{pages: pages(), agents: map[int64]bool{11: true, 12: true}}
	claudeA, claudeB := &fakeClaude{reply: "from A"}, &fakeClaude{reply: "from B"}
	a := &botLoop{cfg: cfgA, mcp: mcpA, claude: claudeA}
	b := &botLoop{cfg: cfgB, mcp: mcpB, claude: claudeB}
	// Each poll, the channel holds the human message and the replies so far.
	for round := 0; round < 3; round++ {
		replyA, replyB := message(2, 11, "from A"), message(3, 12, "from B")
		msgs := []schema.MessageV2{message(1, 3, "hello")}
		if round > 0 {
			msgs = []schema.MessageV2{replyA, replyB}
		}
		for _, l := range []*botLoop{a, b} {
			l.mcp.(*fakeMCP).pages = map[int64]schema.ListMessagesResponseV2{l.lastSeen: {Messages: msgs}}
			if err := l.pollOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(claudeA.prompts) != 1 || len(claudeB.prompts) != 1 {
		t.Fatalf("runs: A %d, B %d, want 1 each", len(claudeA.prompts), len(claudeB.prompts))
	}
}

func TestMCPAdapterMembers(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"members":[{"principal_id":2,"kind":"agent"},{"principal_id":3,"kind":"human"}]}`))
	}))
	defer srv.Close()
	got, err := mcpAdapter{server: srv.URL, token: "tok"}.members(context.Background(), "ops")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/channels/ops/members" || gotAuth != "Bearer tok" {
		t.Fatalf("path %q, auth %q", gotPath, gotAuth)
	}
	if len(got) != 2 || got[0].Kind != schema.PrincipalAgent || got[1].Kind != schema.PrincipalHuman {
		t.Fatalf("members = %+v", got)
	}
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer refusing.Close()
	if _, err := (mcpAdapter{server: refusing.URL, token: "tok"}).members(context.Background(), "ops"); err == nil {
		t.Fatal("want an error for a refused list")
	}
}
