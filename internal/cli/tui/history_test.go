package tui

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// historyAPI is a channel of count messages with ids 11, 12, ... that answers
// list calls the way the server does, and records every call.
type historyAPI struct {
	stubAPI
	count int

	mu     sync.Mutex
	lists  []cli.MessagesPage
	limits []int
	opened chan int64
}

const historyFirstID = 11

func newHistoryAPI(count int) *historyAPI {
	return &historyAPI{count: count, opened: make(chan int64, 8)}
}

func (a *historyAPI) ListMessagesV2Page(_ context.Context, _ string, page cli.MessagesPage, limit int) (schema.ListMessagesResponseV2, error) {
	a.mu.Lock()
	a.lists = append(a.lists, page)
	a.limits = append(a.limits, limit)
	a.mu.Unlock()
	if !page.NewestFirst {
		return schema.ListMessagesResponseV2{}, errors.New("the TUI must read history newest first")
	}
	var resp schema.ListMessagesResponseV2
	for i := a.count - 1; i >= 0 && len(resp.Messages) < limit; i-- {
		id := int64(historyFirstID + i)
		if page.Before > 0 && id >= page.Before {
			continue
		}
		resp.Messages = append(resp.Messages, schema.MessageV2{ID: id})
	}
	return resp, nil
}

func (a *historyAPI) SubscribeV2After(ctx context.Context, _ string, after int64, _ func(), _ func(schema.MessageV2) error) error {
	a.opened <- after
	<-ctx.Done()
	return ctx.Err()
}

func (a *historyAPI) calls() ([]cli.MessagesPage, []int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]cli.MessagesPage(nil), a.lists...), append([]int(nil), a.limits...)
}

func awaitAfter(t *testing.T, a *historyAPI) int64 {
	t.Helper()
	select {
	case after := <-a.opened:
		return after
	case <-time.After(2 * time.Second):
		t.Fatal("the socket never opened")
		return 0
	}
}

func historyModel(t *testing.T, api *historyAPI) Model {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := NewModel(ctx, api, 7, []string{"general"})
	m.after = func(time.Duration, tea.Msg) tea.Cmd { return func() tea.Msg { return nil } }
	return m
}

// openChannel runs the first load and follow of the selected channel and feeds
// the result back into the model.
func openChannel(m Model) Model {
	m.subscribed["general"] = true
	for _, msg := range run(m.loadAndFollow()) {
		m, _ = update(m, msg)
	}
	return m
}

func TestFirstLoadIsOneNewestFirstRequest(t *testing.T) {
	tests := []struct {
		name      string
		count     int
		wantFirst int64 // id of the oldest message shown
		wantShown int
	}{
		{"500 messages show the newest 100", 500, historyFirstID + 400, 100},
		{"exactly one page", 100, historyFirstID, 100},
		{"fewer than a page show all of them", 7, historyFirstID, 7},
		{"an empty channel", 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newHistoryAPI(tt.count)
			m := openChannel(historyModel(t, api))

			lists, limits := api.calls()
			if len(lists) != 1 || !lists[0].NewestFirst || lists[0].Before != 0 || lists[0].After != 0 || limits[0] != 100 {
				t.Errorf("history calls = %+v limits %v, want one newest-first request for 100", lists, limits)
			}
			shown := m.messages["general"]
			if len(shown) != tt.wantShown {
				t.Fatalf("shown %d messages, want %d", len(shown), tt.wantShown)
			}
			for i, message := range shown {
				if want := tt.wantFirst + int64(i); message.ID != want {
					t.Fatalf("message %d has id %d, want %d (ascending, ending at the newest)", i, message.ID, want)
				}
			}
			var wantAfter int64
			if tt.count > 0 {
				wantAfter = int64(historyFirstID + tt.count - 1)
			}
			if after := awaitAfter(t, api); after != wantAfter {
				t.Errorf("socket after = %d, want the newest id %d", after, wantAfter)
			}
		})
	}
}

func TestReconnectResumesFromStreamedWithoutReload(t *testing.T) {
	api := newHistoryAPI(500)
	m := openChannel(historyModel(t, api))
	newest := int64(historyFirstID + 499)
	if after := awaitAfter(t, api); after != newest {
		t.Fatalf("socket after = %d, want %d", after, newest)
	}
	// Two messages arrive live, then the socket drops.
	for _, id := range []int64{newest + 1, newest + 2} {
		m, _ = update(m, messageReceived{channel: "general", message: schema.MessageV2{ID: id}})
	}
	m, _ = update(m, subscriptionEnded{channel: "general", err: errors.New("eof")})
	m, cmd := update(m, resubscribeDue{channel: "general"})
	run(cmd)

	if after := awaitAfter(t, api); after != newest+2 {
		t.Errorf("resumed after = %d, want %d", after, newest+2)
	}
	if lists, _ := api.calls(); len(lists) != 1 {
		t.Errorf("history calls = %d after a reconnect, want the one from the first load", len(lists))
	}
	if got := len(m.messages["general"]); got != 102 {
		t.Errorf("shown %d messages, want 100 loaded and 2 streamed", got)
	}
}
