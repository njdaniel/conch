package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

type fakeMCP struct {
	pages map[int64]schema.ListMessagesResponseV2
	errAt map[int64]error
	reads []int64
	posts []string // bodies of successful posts
	// attempts records every post_message call, successful or refused, with
	// the audience it carried (nil is channel-wide).
	attempts []postAttempt
	postErr  error // returned by every post when set
}

type postAttempt struct {
	body     string
	audience *schema.Audience
}

func (f *fakeMCP) readChannel(_ context.Context, _ string, after int64, _ int) (schema.ListMessagesResponseV2, error) {
	f.reads = append(f.reads, after)
	if err := f.errAt[after]; err != nil {
		return schema.ListMessagesResponseV2{}, err
	}
	return f.pages[after], nil
}

func (f *fakeMCP) postMessage(_ context.Context, _, body string, audience *schema.Audience) error {
	f.attempts = append(f.attempts, postAttempt{body: body, audience: audience})
	if f.postErr != nil {
		return f.postErr
	}
	f.posts = append(f.posts, body)
	return nil
}

type fakeClaude struct {
	reply   string
	err     error
	prompts []string
}

func (f *fakeClaude) Reply(_ context.Context, prompt string) (string, error) {
	f.prompts = append(f.prompts, prompt)
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
		[]schema.MessageV2{message(4, 4, "new one"), message(5, 5, "new two")}, 4,
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
		if cfg.Server != "http://127.0.0.1:8080" || cfg.PollInterval != 5*time.Second || cfg.MaxBackoff != time.Minute || cfg.ContextMessages != 20 || cfg.Model != "sonnet" || cfg.ReplyTimeout != 2*time.Minute || cfg.ClaudeBin != "claude" || cfg.LockFile == "" {
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
