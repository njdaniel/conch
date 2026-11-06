package main

import (
	"net/http"
	"net/url"

	"github.com/njdaniel/conch/pkg/schema"
)

// Message editing (issue #845, docs/design/message-editing.md §12). The edit
// and the revision read are tried four ways:
//
//   - through a channel that is not hers: channel_not_found, as every channel
//     route answers, ghost-identical;
//   - naming A's message through a channel that is hers, as a member and as
//     B's manager: message_not_found, as a message id far past any in use
//     answers (an author is the only one who learns more, and a manager has
//     no exemption);
//   - as B's agent: human_required, decided before any lookup, so the same
//     for A's channel and a ghost one;
//   - as a guest, naming a message from before her visit through the plaza:
//     message_not_found too.
//
// The cases inside one channel (a member who can read the message but did not
// write it, a whisper's non-recipient, a net message's outsider, an operator
// or owner who is a member) are byte-identical to a ghost id in
// internal/server's TestMessageEditChannelGate and TestMessageRevisionsExactSets.

type editRoute struct {
	route string
	// probe builds the request over channel ch (path-escaped) and message.
	probe func(ch string, message int64) probe
}

var editRoutes = []editRoute{
	{route: "PUT /v2/channels/{channel}/messages/{message_id}/body",
		probe: func(ch string, m int64) probe {
			return put(editBase(ch, m)+"/body", schema.EditMessageRequestV1{Body: "an edit from outside"})
		}},
	{route: "GET /v2/channels/{channel}/messages/{message_id}/revisions",
		probe: func(ch string, m int64) probe { return get(editBase(ch, m) + "/revisions") }},
}

func editBase(ch string, m int64) string { return "/v2/channels/" + ch + "/messages/" + id(m) }

// editMatrixRows are B's members' rows and B's manager's.
func editMatrixRows() []row {
	var rows []row
	for _, r := range editRoutes {
		rows = append(rows,
			row{route: r.route, what: "edits through A's channel", pick: aChannel, want: channelNotFound,
				probe: func(w *world, s subject) probe { return r.probe(url.PathEscape(s.name), w.a.root) }},
			row{route: r.route, what: "names A's message through her own channel", pick: aMessage, want: messageNotFound,
				probe: func(w *world, s subject) probe { return r.probe(url.PathEscape(bChannel(w)), s.id) }},
			row{route: r.route, what: "names A's message through her own channel, as B's manager", pick: aMessage, manager: true, want: messageNotFound,
				probe: func(w *world, s subject) probe { return r.probe(url.PathEscape(bChannel(w)), s.id) }},
		)
	}
	return rows
}

// editAgentRows are B's agent's: refused as an agent, whatever it names.
func editAgentRows() []row {
	humanRequired := answer{http.StatusForbidden, schema.ErrorCodeHumanRequired}
	var rows []row
	for _, r := range editRoutes {
		rows = append(rows,
			row{route: r.route, what: "edits through A's channel as an agent", pick: aChannel, want: humanRequired,
				probe: func(w *world, s subject) probe { return r.probe(url.PathEscape(s.name), w.a.root) }},
			row{route: r.route, what: "names A's message through its own channel as an agent", pick: aMessage, want: humanRequired,
				probe: func(w *world, s subject) probe { return r.probe(url.PathEscape(bChannel(w)), s.id) }},
		)
	}
	return rows
}

// editHistoryRows are the guest's: a message from before her visit is
// message_not_found on both routes, and stays so for the member she may
// become.
func editHistoryRows() []row {
	var rows []row
	for _, r := range editRoutes {
		rows = append(rows, row{route: r.route, what: "names the message from before her visit", pick: pPreRoot, want: messageNotFound,
			probe: func(w *world, s subject) probe { return r.probe(pch(w), s.id) }})
	}
	return rows
}
