package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

type reactionStub struct {
	stubAPI
	summaries  schema.ReactionSummariesResponseV1
	batchErr   error
	writeErr   error
	batches    [][]int64
	writeChan  string
	writeID    int64
	writeKey   string
	writeSet   bool
	list       schema.ListReactorsResponseV1
	postedBody string
}

func (a *reactionStub) ReactionSummaries(_ context.Context, _ string, ids []int64) (schema.ReactionSummariesResponseV1, error) {
	a.batches = append(a.batches, append([]int64(nil), ids...))
	return a.summaries, a.batchErr
}

func (a *reactionStub) SetReaction(_ context.Context, channel string, id int64, key string) (schema.ReactionSummaryV1, error) {
	a.writeChan, a.writeID, a.writeKey, a.writeSet = channel, id, key, true
	return tuiReactionSummary(id, 2, schema.ReactionCountV1{Emoji: key, Count: 3, Me: true}), a.writeErr
}

func (a *reactionStub) RemoveReaction(_ context.Context, channel string, id int64, key string) (schema.ReactionSummaryV1, error) {
	a.writeChan, a.writeID, a.writeKey, a.writeSet = channel, id, key, false
	return tuiReactionSummary(id, 2), a.writeErr
}

func (a *reactionStub) ListReactors(context.Context, string, int64, string, int64, int) (schema.ListReactorsResponseV1, error) {
	return a.list, nil
}

func (a *reactionStub) PostMessageV2(_ context.Context, _ string, _ int64, body string, _ *schema.Audience) (schema.MessageV2, error) {
	a.postedBody = body
	return schema.MessageV2{ID: 100, Body: body}, nil
}

func tuiReactionSummary(id int64, second int, counts ...schema.ReactionCountV1) schema.ReactionSummaryV1 {
	if counts == nil {
		counts = []schema.ReactionCountV1{}
	}
	return schema.ReactionSummaryV1{Schema: schema.ReactionSummarySchemaV1, ChannelID: 3, MessageID: id, Reactions: counts,
		AsOf: schema.NewTimestamp(time.Date(2026, time.November, 6, 12, 0, second, 0, time.UTC))}
}

func reactionModelForTest() (Model, *reactionStub) {
	api := &reactionStub{}
	m := NewModel(context.Background(), api, 7, []string{"general", "ops"})
	m.width, m.height = 80, 24
	m.messages["general"] = []schema.MessageV2{{ID: 41, AuthorID: 8, Body: "hello"}}
	m.loaded["general"] = true
	m.members["general"] = map[int64]schema.ChannelMemberV1{7: {PrincipalID: 7, Name: "alice"}}
	return m, api
}

func TestReactionInputAndCounts(t *testing.T) {
	tests := []struct {
		name, command, wantKey string
		set                    bool
	}{
		{"alias", "/react 41 :thumbsup:", "👍", true},
		{"Unicode skin tone", "/react 41 👍🏽", "👍🏽", true},
		{"ZWJ", "/react 👨‍👩‍👧", "👨‍👩‍👧", true},
		{"flag", "/react 🇫🇷", "🇫🇷", true},
		{"remove", "/unreact 41 :+1:", "👍", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, api := reactionModelForTest()
			m, cmd := submitText(t, m, tt.command)
			if cmd == nil {
				t.Fatalf("command produced no request: %q", m.status())
			}
			m = feed(m, cmd)
			if api.writeChan != "general" || api.writeID != 41 || api.writeKey != tt.wantKey || api.writeSet != tt.set {
				t.Errorf("write = %s %d %q set %v", api.writeChan, api.writeID, api.writeKey, api.writeSet)
			}
			if m.input != "" || m.react.writing {
				t.Errorf("accepted input/writing = %q/%v", m.input, m.react.writing)
			}
			if tt.set && !strings.Contains(m.View(), tt.wantKey+" x3 (you)") {
				t.Errorf("count and own state not shown:\n%s", m.View())
			}
		})
	}
}

func TestReactionInputRefusals(t *testing.T) {
	t.Run("unknown shortcode", func(t *testing.T) {
		m, api := reactionModelForTest()
		m, cmd := submitText(t, m, "/react 41 :unknown:")
		if cmd != nil || api.writeID != 0 || !strings.Contains(m.status(), "unknown emoji") {
			t.Errorf("unknown shortcode: cmd %v, write %d, status %q", cmd, api.writeID, m.status())
		}
	})
	t.Run("late response stays with its channel", func(t *testing.T) {
		m, _ := reactionModelForTest()
		m, cmd := submitText(t, m, "/react 41 👍")
		msg := cmd()
		m.selected = 1
		m, _ = update(m, msg)
		if len(m.react.messages["general"]) != 1 || len(m.react.messages["ops"]) != 0 || m.input == "" {
			t.Errorf("late write state = general %v, ops %v, input %q", m.react.messages["general"], m.react.messages["ops"], m.input)
		}
	})
	t.Run("hostile denial is escaped", func(t *testing.T) {
		m, api := reactionModelForTest()
		m, cmd := submitText(t, m, "/react 41 👍")
		api.writeErr = errors.New("denied\x1b[31m\u202e")
		m = feed(m, cmd)
		if strings.Contains(m.status(), "\x1b") || strings.Contains(m.status(), "\u202e") {
			t.Errorf("hostile refusal reached status %q", m.status())
		}
	})
}

func TestReactionKeysTargetVisibleMessage(t *testing.T) {
	m, _ := reactionModelForTest()
	m.messages["general"] = append(m.messages["general"], schema.MessageV2{ID: 42, Body: "newest"})
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyCtrlR})
	if m.input != "/react 42 " {
		t.Errorf("ctrl+r input = %q", m.input)
	}
	m.input = ""
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyCtrlU})
	if m.input != "/unreact 42 " {
		t.Errorf("ctrl+u input = %q", m.input)
	}
	m.input = ""
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyCtrlO})
	if m.input != "/reactors 42 " {
		t.Errorf("ctrl+o input = %q", m.input)
	}
	if m.focus != focusChannels {
		t.Error("reaction shortcuts moved focus away from channel navigation")
	}
}

func TestReactionHydrationOrdersAndClears(t *testing.T) {
	m, api := reactionModelForTest()
	ctx, cancel := context.WithCancel(m.ctx)
	m.ctx = ctx
	cancel() // Model-only event commands must not wait for a socket.
	for _, second := range []int{2, 1} {
		api.summaries.Summaries = []schema.ReactionSummaryV1{tuiReactionSummary(41, second, schema.ReactionCountV1{Emoji: "👍", Count: second + 1})}
		m = feed(m, m.reloadReactions("general"))
	}
	if got := m.react.messages["general"][41].Reactions[0].Count; got != 3 {
		t.Errorf("older hydration rolled count back to %d", got)
	}
	api.summaries.Summaries = nil
	m = feed(m, m.reloadReactions("general"))
	if got := m.react.messages["general"][41]; len(got.Reactions) != 0 {
		t.Error("an omitted id did not clear its counts")
	}
	m.ev.gen = 1
	incoming := tuiReactionSummary(41, 4, schema.ReactionCountV1{Emoji: "👍", Count: 4})
	m, cmd := update(m, reactionSummaryReceived{gen: 1, channel: "general", summary: incoming})
	api.summaries.Summaries = []schema.ReactionSummaryV1{incoming}
	m = feed(m, cmd)
	m, _ = update(m, reactionSummaryReceived{gen: 0, channel: "general", summary: tuiReactionSummary(41, 5)})
	if got := m.react.messages["general"][41]; len(got.Reactions) != 1 || got.Reactions[0].Count != 4 {
		t.Errorf("stale event changed state: %+v", got)
	}
}

func TestReactionReconnectRehydrates(t *testing.T) {
	m, api := reactionModelForTest()
	m.react.messages["general"] = map[int64]schema.ReactionSummaryV1{41: tuiReactionSummary(41, 1, schema.ReactionCountV1{Emoji: "👍", Count: 1})}
	api.summaries = schema.ReactionSummariesResponseV1{Summaries: []schema.ReactionSummaryV1{
		tuiReactionSummary(41, 3, schema.ReactionCountV1{Emoji: "👍", Count: 4, Me: true}),
	}}
	m.ev.gen = 1
	m.ev.state = eventsDown
	m, cmd := update(m, eventsRosterReceived{gen: 1})
	m = feed(m, cmd)
	if len(api.batches) != 1 || len(api.batches[0]) != 1 || api.batches[0][0] != 41 {
		t.Fatalf("reconnect batches = %v", api.batches)
	}
	if got := m.react.messages["general"][41].Reactions[0].Count; got != 4 {
		t.Errorf("count after reconnect = %d, want 4", got)
	}
}

func TestReactionServerCompatibility(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        any
		unsupported bool
	}{
		{name: "old server", status: http.StatusNotFound, unsupported: true},
		{name: "structured refusal is not an old server", status: http.StatusNotFound, body: schema.Error{Code: schema.ErrorCodeMessageNotFound, Message: "message not found"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				if tt.body != nil {
					_ = json.NewEncoder(w).Encode(tt.body)
				}
			}))
			defer server.Close()
			client, err := cli.NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, batchErr := client.ReactionSummaries(context.Background(), "general", []int64{41})
			m, api := reactionModelForTest()
			m.react.messages["general"] = map[int64]schema.ReactionSummaryV1{41: tuiReactionSummary(41, 1, schema.ReactionCountV1{Emoji: "👍", Count: 1})}
			api.batchErr = batchErr
			m, _ = update(m, reactionsLoaded{channel: "general", requested: []int64{41}, err: batchErr})
			if got := m.react.unsupported["general"]; got != tt.unsupported {
				t.Errorf("unsupported = %v, want %v (error %v)", got, tt.unsupported, batchErr)
			}
			if tt.unsupported && len(m.react.messages["general"]) != 0 {
				t.Error("an old server kept stale counts")
			}
			if !tt.unsupported && len(m.react.messages["general"]) != 1 {
				t.Error("a structured refusal corrupted existing counts")
			}
		})
	}
}

func TestReactionBatchChunks(t *testing.T) {
	m, api := reactionModelForTest()
	messages := make([]schema.MessageV2, 205)
	for i := range messages {
		messages[i].ID = int64(i + 1)
	}
	for _, msg := range collect(m.loadReactions("general", messages)) {
		m, _ = update(m, msg)
	}
	if len(api.batches) != 3 || len(api.batches[0]) != 100 || len(api.batches[1]) != 100 || len(api.batches[2]) != 5 {
		t.Errorf("batch sizes = %v, want 100, 100, 5", api.batches)
	}
}

func TestReactionRenderingCompoundAndText(t *testing.T) {
	m, _ := reactionModelForTest()
	m.width, m.height = 40, 12
	counts := []schema.ReactionCountV1{
		{Emoji: "👍🏽", Count: 2, Me: true},
		{Emoji: "👨‍👩‍👧", Count: 3},
		{Emoji: "🇫🇷", Count: 4},
	}
	m.react.messages["general"] = map[int64]schema.ReactionSummaryV1{41: tuiReactionSummary(41, 1, counts...)}
	lines := m.messageBlock("general", m.messages["general"][0], 42)
	joined := strings.Join(lines, "\n")
	for _, key := range []string{"👍🏽", "👨‍👩‍👧", "🇫🇷"} {
		if !strings.Contains(joined, key) {
			t.Errorf("compound emoji %q was split or lost:\n%s", key, joined)
		}
	}
	for _, line := range lines {
		if got := cellWidth(stripANSI(line)); got > 40 {
			t.Errorf("line is %d cells, want at most 40: %q", got, line)
		}
	}
	m, cmd := submitText(t, m, "/emoji text")
	if cmd != nil {
		t.Fatal("text fallback started a command")
	}
	plain := strings.Join(m.messageBlock("general", m.messages["general"][0], 42), "\n")
	for _, want := range []string{":thumbs_up_medium_skin_tone: x2", "(you)", ":family_man_woman_girl: x3", ":flag_france: x4"} {
		if !strings.Contains(plain, want) {
			t.Errorf("text fallback lacks %q:\n%s", want, plain)
		}
	}
}

func TestReactionDraftAndComposerKeepUnicodeIntact(t *testing.T) {
	m, api := reactionModelForTest()
	m.input = "family 👨‍👩‍👧"
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyBackspace})
	if m.input != "family " {
		t.Errorf("backspace split a ZWJ family: %q", m.input)
	}
	m, cmd := submitText(t, m, "nice :tada: :unknown: 👍🏽 🇫🇷")
	m = feed(m, cmd)
	if api.postedBody != "nice 🎉 :unknown: 👍🏽 🇫🇷" {
		t.Errorf("posted body = %q", api.postedBody)
	}
}

func TestReactorListUsesSafeNamesAndGuestAttribution(t *testing.T) {
	m, api := reactionModelForTest()
	created := schema.NewTimestamp(time.Now())
	api.list = schema.ListReactorsResponseV1{Reactors: []schema.ReactionV1{
		{PrincipalID: 7, Emoji: "👍", CreatedAt: created},
		{PrincipalID: 8, Emoji: "👍", CreatedAt: created, GuestReactor: &schema.GuestAttributionV1{Handle: "guest-7h3kq2m9p4r8s6t1", Label: "Nick", Tag: "7h3k"}},
	}}
	m.members["general"][7] = schema.ChannelMemberV1{PrincipalID: 7, Name: "alice\x1b[31m\u202e"}
	m, cmd := submitText(t, m, "/reactors 41 :+1:")
	m = feed(m, cmd)
	if !strings.Contains(m.status(), "Guest: Nick · 7h3k") || strings.Contains(m.status(), "\x1b") || strings.Contains(m.status(), "\u202e") {
		t.Errorf("reactor status = %q", m.status())
	}
}

func TestReactionEmptySummaryKeepsWatermark(t *testing.T) {
	for _, source := range []string{"event", "write"} {
		t.Run(source, func(t *testing.T) {
			m, _ := reactionModelForTest()
			empty := tuiReactionSummary(41, 3)
			if source == "event" {
				m, _ = update(m, reactionSummaryReceived{channel: "general", summary: empty})
			} else {
				m, _ = update(m, reactionWritten{channel: "general", messageID: 41, summary: empty})
			}
			m, _ = update(m, reactionWritten{channel: "general", messageID: 41, summary: tuiReactionSummary(41, 2, schema.ReactionCountV1{Emoji: "👍", Count: 1})})
			if got := m.react.messages["general"][41]; len(got.Reactions) != 0 || got.AsOf != empty.AsOf {
				t.Errorf("older response resurrected removed reaction: %+v", got)
			}
		})
	}
}

func TestReactionHydrationOrdersPresentAndGuardsOmissions(t *testing.T) {
	for _, result := range []string{"omitted", "nonempty"} {
		for _, change := range []string{"event", "write", "new hydration"} {
			t.Run(result+"/"+change, func(t *testing.T) {
				m, api := reactionModelForTest()
				if result == "nonempty" {
					api.summaries.Summaries = []schema.ReactionSummaryV1{tuiReactionSummary(41, 5, schema.ReactionCountV1{Emoji: "👍", Count: 1})}
				}
				old := collect(m.reloadReactions("general"))
				newer := tuiReactionSummary(41, 4, schema.ReactionCountV1{Emoji: "👍", Count: 4})
				switch change {
				case "event":
					m, _ = update(m, reactionSummaryReceived{channel: "general", summary: newer})
				case "write":
					m, _ = update(m, reactionWritten{channel: "general", messageID: 41, summary: newer})
				case "new hydration":
					api.summaries.Summaries = []schema.ReactionSummaryV1{newer}
					m = feed(m, m.reloadReactions("general"))
				}
				for _, msg := range old {
					m, _ = update(m, msg)
				}
				got := m.react.messages["general"][41]
				want := 4
				if result == "nonempty" {
					want = 1 // as_of 5 wins over 4, regardless of request revisions.
				}
				if len(got.Reactions) != 1 || got.Reactions[0].Count != want {
					t.Errorf("batch result = %+v, want count %d", got, want)
				}
			})
		}
	}
}

func TestReactionRosterBeforeReplayHydratesInBatches(t *testing.T) {
	for _, count := range []int{2, 205} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			m, api := reactionModelForTest()
			ctx, cancel := context.WithCancel(m.ctx)
			m.ctx = ctx
			cancel() // No socket waiter should block this model-only test.
			m.ev.state = eventsDown
			m, cmd := update(m, eventsRosterReceived{})
			m = feed(m, cmd)
			api.batches = nil
			var pending []tea.Cmd
			for i := 0; i < count; i++ {
				id := int64(42 + i)
				api.summaries.Summaries = append(api.summaries.Summaries, tuiReactionSummary(id, 4, schema.ReactionCountV1{Emoji: "👍", Count: 2}))
				m, cmd = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: id, Body: "replayed"}})
				pending = append(pending, cmd)
			}
			for _, cmd := range pending {
				m = feed(m, cmd)
			}
			if want := (count + schema.MaxReactionBatchMessages - 1) / schema.MaxReactionBatchMessages; len(api.batches) != want {
				t.Fatalf("replay batches = %v, want %d batched requests", api.batches, want)
			}
			for i := 0; i < count; i++ {
				if got := m.react.messages["general"][int64(42+i)]; len(got.Reactions) != 1 || got.Reactions[0].Count != 2 {
					t.Errorf("replayed message %d lacks reactions: %+v", 42+i, got)
				}
			}
		})
	}
}

func TestReactionKeysPreserveComposerDraft(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyCtrlR, tea.KeyCtrlU, tea.KeyCtrlO} {
		t.Run(tea.KeyMsg{Type: key}.String(), func(t *testing.T) {
			for _, draft := range []string{"unfinished 👨‍👩‍👧 draft", " "} {
				m, _ := reactionModelForTest()
				m.input = draft
				m, cmd := update(m, tea.KeyMsg{Type: key})
				if m.input != draft || cmd != nil {
					t.Errorf("shortcut replaced draft %q with %q (cmd %v)", draft, m.input, cmd)
				}
			}
		})
	}
}

func TestReactionHydrationRevisionIsPerMessage(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitted=%v", omitted), func(t *testing.T) {
			m, api := reactionModelForTest()
			m.messages["general"] = append(m.messages["general"], schema.MessageV2{ID: 42})
			m.react.messages["general"] = map[int64]schema.ReactionSummaryV1{42: tuiReactionSummary(42, 1, schema.ReactionCountV1{Emoji: "👍", Count: 1})}
			if !omitted {
				api.summaries.Summaries = []schema.ReactionSummaryV1{tuiReactionSummary(42, 2, schema.ReactionCountV1{Emoji: "👍", Count: 2})}
			}
			pending := collect(m.reloadReactions("general"))
			m, _ = update(m, reactionSummaryReceived{channel: "general", summary: tuiReactionSummary(41, 3, schema.ReactionCountV1{Emoji: "👍", Count: 3})})
			for _, msg := range pending {
				m, _ = update(m, msg)
			}
			if got := m.react.messages["general"][41]; len(got.Reactions) != 1 || got.Reactions[0].Count != 3 {
				t.Errorf("batch changed live state: %+v", got)
			}
			got := m.react.messages["general"][42]
			if omitted && len(got.Reactions) != 0 || !omitted && (len(got.Reactions) != 1 || got.Reactions[0].Count != 2) {
				t.Errorf("batch did not update unchanged message: %+v", got)
			}
		})
	}
}

func TestReactionOverlappingHydrationFailure(t *testing.T) {
	for _, omitted := range []bool{false, true} {
		t.Run(fmt.Sprintf("omitted=%v", omitted), func(t *testing.T) {
			m, api := reactionModelForTest()
			m.react.messages["general"] = map[int64]schema.ReactionSummaryV1{41: tuiReactionSummary(41, 1, schema.ReactionCountV1{Emoji: "👍", Count: 1})}
			if !omitted {
				api.summaries.Summaries = []schema.ReactionSummaryV1{tuiReactionSummary(41, 3, schema.ReactionCountV1{Emoji: "👍", Count: 3})}
			}
			older := collect(m.reloadReactions("general"))
			api.batchErr = errors.New("read failed")
			m = feed(m, m.reloadReactions("general"))
			for _, msg := range older {
				m, _ = update(m, msg)
			}
			got := m.react.messages["general"][41]
			if omitted && len(got.Reactions) != 0 || !omitted && (len(got.Reactions) != 1 || got.Reactions[0].Count != 3) {
				t.Fatalf("failed newer request discarded successful older result: %+v", got)
			}
		})
	}
}

func TestReactionOmissionConfirmsNonBatchSummaries(t *testing.T) {
	for _, source := range []string{"delayed write", "new event"} {
		for _, present := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/present=%v", source, present), func(t *testing.T) {
				m, api := reactionModelForTest()
				close(m.events) // No socket waiter in this model-only test.
				m.after = func(_ time.Duration, msg tea.Msg) tea.Cmd { return func() tea.Msg { return msg } }
				m.react.messages["general"] = map[int64]schema.ReactionSummaryV1{41: tuiReactionSummary(41, 1, schema.ReactionCountV1{Emoji: "👍", Count: 1})}
				m = feed(m, m.reloadReactions("general")) // An unversioned omission.
				second := 2
				if source == "new event" {
					second = 5
				}
				incoming := tuiReactionSummary(41, second, schema.ReactionCountV1{Emoji: "👍", Count: 2})
				var msg tea.Msg = reactionWritten{channel: "general", messageID: 41, summary: incoming}
				if source == "new event" {
					msg = reactionSummaryReceived{channel: "general", summary: incoming}
				}
				m, cmd := update(m, msg)
				if len(m.react.messages["general"][41].Reactions) != 0 {
					t.Fatal("non-batch summary resurrected an unversioned clear")
				}
				if cmd == nil || len(m.react.pending["general"]) != 1 {
					t.Fatal("confirming hydration was not queued")
				}
				// Repeated frames and replay hydration share the same timer and ID.
				m, _ = update(m, msg)
				if extra := m.queueReactionHydration("general", schema.MessageV2{ID: 41}); extra != nil {
					t.Fatal("confirmation did not coalesce with pending hydration")
				}
				api.batchErr = errors.New("confirmation failed")
				for _, due := range collect(cmd) {
					m, cmd = update(m, due)
					for _, result := range collect(cmd) {
						m, cmd = update(m, result)
					}
				}
				if len(m.react.messages["general"][41].Reactions) != 0 {
					t.Fatal("failed confirmation changed counts")
				}
				if cmd == nil {
					t.Fatal("failed confirmation lost unversioned state")
				}
				api.batchErr = nil
				if present {
					api.summaries.Summaries = []schema.ReactionSummaryV1{tuiReactionSummary(41, 6, schema.ReactionCountV1{Emoji: "👍", Count: 6})}
				}
				m = feed(m, cmd)
				got := m.react.messages["general"][41]
				if present && (len(got.Reactions) != 1 || got.Reactions[0].Count != 6) || !present && len(got.Reactions) != 0 {
					t.Fatalf("confirmation did not decide state: %+v", got)
				}
				// Either confirming answer clears the mark, so future summaries apply.
				m, _ = update(m, reactionSummaryReceived{channel: "general", summary: tuiReactionSummary(41, 7, schema.ReactionCountV1{Emoji: "👍", Count: 7})})
				if got := m.react.messages["general"][41]; len(got.Reactions) != 1 || got.Reactions[0].Count != 7 {
					t.Fatalf("confirmation left message unversioned: %+v", got)
				}
			})
		}
	}
}

func TestReactionRevisionsTrackAppliedState(t *testing.T) {
	for _, source := range []string{"batch", "event", "write"} {
		for _, second := range []int{2, 3, 4} {
			t.Run(fmt.Sprintf("%s/as_of=%d", source, second), func(t *testing.T) {
				m, api := reactionModelForTest()
				m.react.messages["general"] = map[int64]schema.ReactionSummaryV1{41: tuiReactionSummary(41, 3, schema.ReactionCountV1{Emoji: "👍", Count: 3})}
				m.reactionRevisions("general")[41] = 1
				incoming := tuiReactionSummary(41, second, schema.ReactionCountV1{Emoji: "👍", Count: second})
				api.summaries.Summaries = []schema.ReactionSummaryV1{incoming}
				cmd := m.reloadReactions("general")
				if got := m.reactionRevisions("general")[41]; got != 1 {
					t.Fatalf("request start bumped revision to %d", got)
				}
				switch source {
				case "batch":
					m = feed(m, cmd)
				case "event":
					m, _ = update(m, reactionSummaryReceived{channel: "general", summary: incoming})
				case "write":
					m, _ = update(m, reactionWritten{channel: "general", messageID: 41, summary: incoming})
				}
				wantRevision := uint64(2)
				if second < 3 {
					wantRevision = 1
				}
				if got := m.reactionRevisions("general")[41]; got != wantRevision {
					t.Errorf("revision = %d, want %d", got, wantRevision)
				}
				if got := m.react.messages["general"][41]; got.AsOf != tuiReactionSummary(41, max(3, second)).AsOf {
					t.Errorf("summary did not follow timestamp order: %+v", got)
				}
			})
		}
	}
}

func TestReactionConfirmationRecovery(t *testing.T) {
	for _, source := range []string{"frame", "write"} {
		for _, failure := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failure=%v", source, failure), func(t *testing.T) {
				m, api := reactionModelForTest()
				m.after = func(_ time.Duration, msg tea.Msg) tea.Cmd { return func() tea.Msg { return msg } }
				m = feed(m, m.reloadReactions("general"))
				incoming := tuiReactionSummary(41, 2, schema.ReactionCountV1{Emoji: "👍", Count: 1})
				_ = m.receiveReactionSummary("general", incoming)
				if failure {
					api.batchErr = errors.New("transient read failure")
				} else {
					api.summaries.Summaries = []schema.ReactionSummaryV1{incoming}
				}
				// Start C1 and hold its result while the removal arrives.
				_, cmd := update(m, reactionHydrationDue{channel: "general"})
				results := collect(cmd)
				if !failure {
					removal := tuiReactionSummary(41, 3)
					if source == "write" {
						m, cmd = update(m, reactionWritten{channel: "general", messageID: 41, summary: removal})
					} else {
						cmd = m.receiveReactionSummary("general", removal)
					}
					if cmd != nil {
						t.Error("started overlapping confirmation for removal")
					}
				}
				api.batchErr = nil
				api.summaries.Summaries = nil // C2 observes the removal.
				if failure {
					api.summaries.Summaries = []schema.ReactionSummaryV1{incoming}
				}
				for _, result := range results {
					m, cmd = update(m, result)
				}
				if cmd == nil {
					t.Fatal("confirmation work lost; no follow-up scheduled")
				}
				m = feed(m, cmd)
				got := m.react.messages["general"][41]
				if failure && len(got.Reactions) != 1 || !failure && len(got.Reactions) != 0 {
					t.Fatalf("confirmation did not recover: %+v", got)
				}
				if len(api.batches) != 3 {
					t.Fatalf("reads = %d, want initial omission, C1, C2", len(api.batches))
				}
			})
		}
	}
}

func TestReactionConfirmationBackoffAndStop(t *testing.T) {
	for _, stop := range []string{"model stopped", "channel closed", "success"} {
		t.Run(stop, func(t *testing.T) {
			m, api := reactionModelForTest()
			ctx, cancel := context.WithCancel(m.ctx)
			defer cancel()
			m.ctx = ctx
			var delays []time.Duration
			m.after = func(delay time.Duration, msg tea.Msg) tea.Cmd {
				delays = append(delays, delay)
				return func() tea.Msg { return msg }
			}
			m = feed(m, m.reloadReactions("general"))
			_ = m.receiveReactionSummary("general", tuiReactionSummary(41, 2))
			api.batchErr = errors.New("unavailable")
			cmd := m.flushReactionHydration("general")
			for i, seconds := range []int{1, 2, 4, 8, 16, 30, 30} {
				for _, result := range collect(cmd) {
					m, cmd = update(m, result)
				}
				if len(delays) != i+1 || delays[i] != time.Duration(seconds)*time.Second {
					t.Fatalf("retry delays = %v at attempt %d", delays, i)
				}
				if i < 6 {
					for _, due := range collect(cmd) {
						m, cmd = update(m, due)
					}
				}
			}
			before := len(api.batches)
			switch stop {
			case "model stopped":
				cancel()
			case "channel closed":
				m.channels = []string{"ops"}
			case "success":
				api.batchErr = nil
			}
			m = feed(m, cmd)
			wantReads := before
			if stop == "success" {
				wantReads++
			}
			if len(api.batches) != wantReads || len(delays) != 7 {
				t.Fatalf("work continued after %s: reads=%d, delays=%v", stop, len(api.batches), delays)
			}
			if len(m.react.confirming["general"]) != 0 {
				t.Fatal("confirmation work was not retired")
			}
		})
	}
}
