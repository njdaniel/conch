package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

type fakeMCP struct {
	pages map[int64]schema.ListMessagesResponseV1
	errAt map[int64]error
	reads []int64
	posts []string
}

func (f *fakeMCP) readChannel(_ context.Context, _ string, after int64, _ int) (schema.ListMessagesResponseV1, error) {
	f.reads = append(f.reads, after)
	if err := f.errAt[after]; err != nil {
		return schema.ListMessagesResponseV1{}, err
	}
	return f.pages[after], nil
}

func (f *fakeMCP) postMessage(_ context.Context, _, body string) error {
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

func message(id, author int64, body string) schema.MessageV1 {
	return schema.MessageV1{ID: id, AuthorID: author, Body: body}
}

func testConfig() config {
	return config{Channel: "ops", PrincipalID: 9, ContextMessages: 20, PollInterval: time.Second, MaxBackoff: 8 * time.Second}
}

func TestPollOnceCursorAdvancement(t *testing.T) {
	tests := []struct {
		name      string
		pages     map[int64]schema.ListMessagesResponseV1
		wantSeen  int64
		wantReads []int64
	}{
		{
			name:     "single final page with omitted next_after",
			pages:    map[int64]schema.ListMessagesResponseV1{3: {Messages: []schema.MessageV1{message(4, 9, "self")}}},
			wantSeen: 4, wantReads: []int64{3},
		},
		{
			name: "multiple pages",
			pages: map[int64]schema.ListMessagesResponseV1{
				3: {Messages: []schema.MessageV1{message(4, 9, "self")}, NextAfter: 4},
				4: {Messages: []schema.MessageV1{message(5, 9, "self"), message(6, 9, "self")}},
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
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV1{
		2: {Messages: []schema.MessageV1{message(3, 9, "ignore"), message(4, 2, "hello"), message(5, 9, "ignore too")}},
		0: {Messages: []schema.MessageV1{message(1, 3, "context"), message(2, 4, "older")}},
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
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV1{1: {Messages: []schema.MessageV1{message(2, 3, "question")}}, 0: {Messages: []schema.MessageV1{message(1, 2, "old")}}}}
	loop := &botLoop{cfg: testConfig(), mcp: mcp, claude: &fakeClaude{reply: " \n\t"}, lastSeen: 1}
	if err := loop.pollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mcp.posts) != 0 {
		t.Fatalf("posts = %v, want none", mcp.posts)
	}
}

func TestPollOnceContextComesFromRollingBufferNotFullReplay(t *testing.T) {
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV1{
		0: {Messages: []schema.MessageV1{message(1, 5, "seed one"), message(2, 5, "seed two")}},
		2: {Messages: []schema.MessageV1{message(3, 4, "first human line")}},
		3: {Messages: []schema.MessageV1{message(4, 4, "second human line")}},
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
		[]schema.MessageV1{message(1, 1, "first"), message(2, 2, "second"), message(3, 3, "third")},
		[]schema.MessageV1{message(4, 4, "new one"), message(5, 5, "new two")}, 4,
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
	mcp := &fakeMCP{pages: map[int64]schema.ListMessagesResponseV1{0: {}}, errAt: map[int64]error{}}
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

func (s *seedAwareMCP) readChannel(ctx context.Context, channel string, after int64, limit int) (schema.ListMessagesResponseV1, error) {
	*s.calls++
	if *s.calls == 2 {
		return schema.ListMessagesResponseV1{}, errors.New("first failure")
	}
	return s.fakeMCP.readChannel(ctx, channel, after, limit)
}
