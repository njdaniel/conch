package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/pkg/schema"
)

// authorsAPI is a fake server whose authors route answers what the test set,
// and which records what it was asked.
type authorsAPI struct {
	stubAPI
	mu    sync.Mutex
	named []schema.ChannelAuthorV1
	err   error
	asked [][]int64
}

func (a *authorsAPI) ChannelAuthors(_ context.Context, _ string, ids []int64) ([]schema.ChannelAuthorV1, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked = append(a.asked, append([]int64(nil), ids...))
	return a.named, a.err
}

func (a *authorsAPI) calls() [][]int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]int64(nil), a.asked...)
}

// namedAfter runs the commands cmd yields and feeds their messages back.
func namedAfter(m Model, cmd tea.Cmd) Model {
	for _, msg := range collect(cmd) {
		var next tea.Cmd
		m, next = update(m, msg)
		m = namedAfter(m, next)
	}
	return m
}

func historyOf(m Model) string { return strings.Join(paneLines(stripANSI(m.View())), "\n") }

func departedModel(api *authorsAPI) Model {
	m := authorModel(api, schema.ChannelMemberV1{PrincipalID: 7, Name: "nick", Kind: schema.PrincipalHuman})
	m.messages["ops"] = nil
	var cmd tea.Cmd
	m, cmd = update(m, messagesLoaded{channel: "ops", messages: []schema.MessageV2{
		{ID: 1, AuthorID: 7, Body: "first", CreatedAt: at(13, 12, 0)},
		{ID: 2, AuthorID: 5, Body: "second", CreatedAt: at(13, 12, 1)},
		{ID: 3, AuthorID: 6, Body: "third", CreatedAt: at(13, 12, 2)},
	}})
	return namedAfter(m, cmd)
}

func TestHistoryNamesDepartedAuthorsThroughTheRoute(t *testing.T) {
	api := &authorsAPI{named: []schema.ChannelAuthorV1{{PrincipalID: 5, Name: "dee", Kind: schema.PrincipalAgent}}}
	m := departedModel(api)
	view := historyOf(m)
	for _, want := range []string{"nick: first", "dee [agent]: second", "#6: third"} {
		if !strings.Contains(view, want) {
			t.Errorf("history lacks %q:\n%s", want, view)
		}
	}
	// One ask for the two authors the list lacks; a second load asks nothing new.
	if got := api.calls(); len(got) != 1 || len(got[0]) != 2 {
		t.Errorf("asked = %v, want one call for ids 5 and 6", got)
	}
	m.addMessages("ops", []schema.MessageV2{{ID: 4, AuthorID: 5, Body: "again", CreatedAt: at(13, 12, 3)}})
	if cmd := m.nameAuthors("ops"); cmd != nil {
		t.Error("an author already asked about is asked about again")
	}
	if got := api.calls(); len(got) != 1 {
		t.Errorf("an author already asked about was asked again: %v", got)
	}
}

func TestHistoryDoesNotTrustWhatItDidNotAsk(t *testing.T) {
	api := &authorsAPI{named: []schema.ChannelAuthorV1{
		{PrincipalID: 5, Name: "dee", Kind: schema.PrincipalHuman},
		// Not asked about: a hostile server naming someone else.
		{PrincipalID: 7, Name: "impostor", Kind: schema.PrincipalHuman},
		{PrincipalID: 99, Name: "stranger", Kind: schema.PrincipalHuman},
	}}
	m := departedModel(api)
	view := historyOf(m)
	if strings.Contains(view, "impostor") || strings.Contains(view, "stranger") {
		t.Errorf("history shows a name it did not ask for:\n%s", view)
	}
	if !strings.Contains(view, "nick: first") || !strings.Contains(view, "dee: second") {
		t.Errorf("history:\n%s", view)
	}
}

func TestHistoryCleansAHostileAuthorName(t *testing.T) {
	api := &authorsAPI{named: []schema.ChannelAuthorV1{{PrincipalID: 5, Name: "d\x1b[2Jee" + string(rune(0x202e)), Kind: schema.PrincipalHuman}}}
	m := departedModel(api)
	if view := m.View(); strings.Contains(view, "\x1b[2J") || strings.ContainsRune(view, 0x202e) {
		t.Errorf("a hostile author name reached the screen:\n%q", view)
	}
}

func TestHistoryFallsBackToTheNumberWhenTheRouteDoesNotAnswer(t *testing.T) {
	api := &authorsAPI{err: errors.New("boom")}
	m := departedModel(api)
	view := historyOf(m)
	if !strings.Contains(view, "#5: second") || !strings.Contains(view, "#6: third") {
		t.Errorf("history:\n%s", view)
	}
	if strings.Contains(view, "boom") {
		t.Errorf("history shows the failure:\n%s", view)
	}
}

func TestDepartedNamesAreDroppedWhenTheListIsRead(t *testing.T) {
	api := &authorsAPI{named: []schema.ChannelAuthorV1{{PrincipalID: 5, Name: "dee", Kind: schema.PrincipalHuman}}}
	m := departedModel(api)
	if !strings.Contains(historyOf(m), "dee: second") {
		t.Fatalf("not named:\n%s", historyOf(m))
	}
	// The list is read again, and the route no longer names the author.
	api.named = nil
	var cmd tea.Cmd
	m, cmd = update(m, membersLoaded{channel: "ops", revision: m.memberRevision["ops"], members: []schema.ChannelMemberV1{{PrincipalID: 7, Name: "nick", Kind: schema.PrincipalHuman}}})
	m = namedAfter(m, cmd)
	if view := historyOf(m); strings.Contains(view, "dee") || !strings.Contains(view, "#5: second") {
		t.Errorf("a name outlived the list that was replaced:\n%s", view)
	}
}

func TestAuthorsAreNotAskedBeforeTheListLoads(t *testing.T) {
	api := &authorsAPI{}
	m := authorModel(api)
	var cmd tea.Cmd
	m, cmd = update(m, messagesLoaded{channel: "ops", messages: []schema.MessageV2{{ID: 1, AuthorID: 5, Body: "x", CreatedAt: at(13, 12, 0)}}})
	_ = namedAfter(m, cmd)
	if got := api.calls(); len(got) != 0 {
		t.Errorf("asked before the list loaded: %v", got)
	}
}

func TestCommunitySaysWhenItIsOnlyThePeopleSharedWith(t *testing.T) {
	const line = "You see the people you share a channel with"
	for _, tt := range []struct {
		scope schema.DirectoryScope
		want  bool
	}{
		{schema.DirectoryScopeSharedChannels, true},
		{schema.DirectoryScopeEveryone, false},
		{"", false},
		{"something_new", false},
	} {
		api := &communityAPI{pages: map[int64]schema.ServerMembersResponseV1{
			0: {Members: []schema.ServerMemberV1{member(1, schema.PrincipalHuman, "alice", schema.RoleMember)}, Scope: tt.scope},
		}}
		view := plain(communityOf(t, api, schema.RoleMember))
		if got := strings.Contains(view, line); got != tt.want {
			t.Errorf("scope %q: line shown = %v, want %v:\n%s", tt.scope, got, tt.want, view)
		}
		if !strings.Contains(view, "alice") {
			t.Errorf("scope %q: the list is gone:\n%s", tt.scope, view)
		}
	}
}
