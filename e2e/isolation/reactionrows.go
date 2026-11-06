package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"

	"github.com/njdaniel/conch/pkg/schema"
)

// Reactions (issue #765, docs/design/message-reactions.md §14 V1, V2, MD2).
// The seven routes are tried three ways:
//
//   - through a channel that is not hers: channel_not_found, as every channel
//     route answers, ghost-identical;
//   - naming a message that is not hers through a channel that is hers:
//     message_not_found, as a message id far past any in use answers;
//   - as a guest, naming a message from before her visit through the plaza:
//     message_not_found too, and the batch read omits it.
//
// The two moderation routes are a manager's: B's manager is told the message
// is not hers, a plain member that the route is not hers, and an agent is
// refused the class before any lookup.

const thumbsUpEmoji = "\U0001F44D"

type reactionRoute struct {
	route string
	// probe builds the request over channel ch (path-escaped) and message.
	probe func(ch string, message int64) probe
	// moderation is the manager's routes.
	moderation bool
	// batch is the one that omits instead of refusing.
	batch bool
}

var reactionRoutes = []reactionRoute{
	{route: "PUT /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}",
		probe: func(ch string, m int64) probe { return put(reactionBase(ch, m)+reactionEmoji(), nil) }},
	{route: "DELETE /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}",
		probe: func(ch string, m int64) probe { return del(reactionBase(ch, m) + reactionEmoji()) }},
	{route: "GET /v2/channels/{channel}/messages/{message_id}/reactions",
		probe: func(ch string, m int64) probe { return get(reactionBase(ch, m) + "/reactions") }},
	{route: "GET /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}",
		probe: func(ch string, m int64) probe { return get(reactionBase(ch, m) + reactionEmoji()) }},
	{route: "GET /v2/channels/{channel}/reactions", batch: true,
		probe: func(ch string, m int64) probe { return get("/v2/channels/" + ch + "/reactions?messages=" + id(m)) }},
	{route: "DELETE /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}/{principal_id}", moderation: true,
		probe: func(ch string, m int64) probe { return del(reactionBase(ch, m) + reactionEmoji() + "/1") }},
	{route: "DELETE /v2/channels/{channel}/messages/{message_id}/reactors/{principal_id}", moderation: true,
		probe: func(ch string, m int64) probe { return del(reactionBase(ch, m) + "/reactors/1") }},
}

func reactionBase(ch string, m int64) string { return "/v2/channels/" + ch + "/messages/" + id(m) }
func reactionEmoji() string                  { return "/reactions/" + url.PathEscape(thumbsUpEmoji) }

var messageNotFound = answer{http.StatusNotFound, "message_not_found"}

// emptyBatch checks a batch read's success: it names nothing of the message.
func emptyBatch(_ *world, _ api, body []byte) error {
	if !bytes.Contains(body, []byte(`"summaries":[]`)) {
		return fmt.Errorf("the batch read answered %s, want it to omit the message", body)
	}
	return nil
}

// reactionMatrixRows are B's members' rows and B's manager's.
func reactionMatrixRows() []row {
	var rows []row
	for _, r := range reactionRoutes {
		rows = append(rows, row{route: r.route, what: "reacts through A's channel", pick: aChannel, want: channelNotFound,
			probe: func(w *world, s subject) probe { return r.probe(url.PathEscape(s.name), w.a.root) }})
		switch {
		case r.batch:
			rows = append(rows, row{route: r.route, what: "reads reactions on A's message through her own channel", pick: aMessage, own: true,
				inspect: emptyBatch,
				probe:   func(w *world, s subject) probe { return r.probe(url.PathEscape(bChannel(w)), s.id) }})
		case r.moderation:
			// B's manager (bea manages B's first channel) is told the message
			// is not hers; a plain member is refused the route.
			rows = append(rows,
				row{route: r.route, what: "removes a reaction on A's message through her own channel, as B's manager", pick: aMessage, manager: true, want: messageNotFound,
					probe: func(w *world, s subject) probe { return r.probe(url.PathEscape(bChannel(w)), s.id) }})
		default:
			rows = append(rows, row{route: r.route, what: "names A's message through her own channel", pick: aMessage, want: messageNotFound,
				probe: func(w *world, s subject) probe { return r.probe(url.PathEscape(bChannel(w)), s.id) }})
		}
	}
	return rows
}

func bChannel(w *world) string { return w.b.main() }

// reactionAgentRows are B's agent's: the channel gate, and the class that
// keeps it off the moderation routes.
func reactionAgentRows() []row {
	var rows []row
	for _, r := range reactionRoutes {
		want := channelNotFound
		if r.moderation {
			want = answer{http.StatusForbidden, "forbidden"}
		}
		rows = append(rows, row{route: r.route, what: "reacts through A's channel as an agent", pick: aChannel, want: want,
			probe: func(w *world, s subject) probe { return r.probe(url.PathEscape(s.name), w.a.root) }})
	}
	return rows
}

// reactionHistoryRows are the guest's: a message from before her visit is
// message_not_found on every route, the batch read omits it, and the
// moderation routes are not hers (nor the member's she may become).
func reactionHistoryRows() []row {
	var rows []row
	for _, r := range reactionRoutes {
		switch {
		case r.batch:
			rows = append(rows, row{route: r.route, what: "reads reactions on the message from before her visit", pick: pPreRoot, own: true,
				inspect: emptyBatch,
				probe:   func(w *world, s subject) probe { return r.probe(pch(w), s.id) }})
		case r.moderation:
			rows = append(rows, row{route: r.route, what: "removes a reaction in the plaza", pick: pPreRoot, want: answer{http.StatusForbidden, "forbidden"},
				probe: func(w *world, s subject) probe { return r.probe(pch(w), s.id) }})
		default:
			rows = append(rows, row{route: r.route, what: "names the message from before her visit", pick: pPreRoot, want: messageNotFound,
				probe: func(w *world, s subject) probe { return r.probe(pch(w), s.id) }})
		}
	}
	return rows
}

var _ = schema.ReactionSummarySchemaV1
