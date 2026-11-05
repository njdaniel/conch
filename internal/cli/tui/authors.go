package tui

import (
	"context"
	"slices"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/njdaniel/conch/pkg/schema"
)

// Departed authors (community-model §13.5): a message's author stays named for
// everyone who can read it, so the people the member list does not have are
// named by GET /v1/channels/{channel}/authors, and shown as #<id> only where
// the route did not name them. The member list always wins, and what the route
// said is dropped whenever the list is read again, so no name outlives the
// answer that gave it.

// authorNamer is the call the route needs. It is a separate interface, asserted
// on the client, so a client without it still draws #<id>.
type authorNamer interface {
	ChannelAuthors(ctx context.Context, channel string, ids []int64) ([]schema.ChannelAuthorV1, error)
}

// authorsLoaded carries what the route named for the ids it was asked about.
// err is the first batch that failed; the other batches' names are still in
// authors.
type authorsLoaded struct {
	channel string
	asked   []int64
	authors []schema.ChannelAuthorV1
	err     error
}

// forgetAuthors drops what the route named in channel, and which ids it was
// asked about, when the member list is replaced.
func (m *Model) forgetAuthors(channel string) {
	delete(m.departed, channel)
	delete(m.authorsAsked, channel)
}

// nameAuthors asks the route about the authors of channel's messages that the
// member list does not have and that have not been asked about. It asks only
// once the channel's list is loaded: before that every author is missing.
func (m *Model) nameAuthors(channel string) tea.Cmd {
	namer, ok := m.api.(authorNamer)
	list, loaded := m.members[channel]
	if !ok || !loaded {
		return nil
	}
	var ids []int64
	for _, message := range m.messages[channel] {
		id := message.AuthorID
		_, listed := list[id]
		if message.GuestAuthor != nil || listed || id <= 0 || m.authorsAsked[channel][id] || slices.Contains(ids, id) {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	if m.authorsAsked == nil {
		m.authorsAsked = make(map[string]map[int64]bool)
	}
	if m.authorsAsked[channel] == nil {
		m.authorsAsked[channel] = make(map[int64]bool)
	}
	for _, id := range ids {
		m.authorsAsked[channel][id] = true
	}
	ctx := m.ctx
	return func() tea.Msg {
		authors, err := namer.ChannelAuthors(ctx, channel, ids)
		return authorsLoaded{channel: channel, asked: ids, authors: authors, err: err}
	}
}

func (m Model) authorsLoaded(msg authorsLoaded) (tea.Model, tea.Cmd) {
	named := make(map[int64]bool, len(msg.authors))
	for _, a := range msg.authors {
		// Only what was asked: a row for another id is not trusted.
		if !slices.Contains(msg.asked, a.PrincipalID) {
			continue
		}
		if m.departed == nil {
			m.departed = make(map[string]map[int64]schema.ChannelAuthorV1)
		}
		if m.departed[msg.channel] == nil {
			m.departed[msg.channel] = make(map[int64]schema.ChannelAuthorV1)
		}
		m.departed[msg.channel][a.PrincipalID] = a
		named[a.PrincipalID] = true
	}
	// An id that no answer reached may be asked about again, by the next load
	// or message; one the route answered without a name is left as #<id>.
	if msg.err != nil {
		for _, id := range msg.asked {
			if !named[id] {
				delete(m.authorsAsked[msg.channel], id)
			}
		}
	}
	return m, nil
}
