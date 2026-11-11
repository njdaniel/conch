package server

import (
	"context"
	"net/http"

	"github.com/njdaniel/conch/internal/server/store"
)

// Message removal (issue #852, docs/design/moderation.md §7). DELETE
// /v2/channels/{channel}/messages/{message_id} is in the accessChannelManager
// class: the guard admits an owner, an operator holding manage_channels, or a
// manager of the channel who is a member. The class is not a content right.
// This handler then applies the ordinary content rules, in this order:
//
//  1. the channel gate (moderationChannel): an unknown channel and a caller
//     who is not, now, a member of it (an operator outside the channel
//     included) get the one 404 channel_not_found;
//  2. the message id is a positive integer, else 400 invalid_request;
//  3. the store, in one transaction (store.RemoveMessage): membership and
//     standing again under the write lock, then the target by the one message
//     rule and the caller's history bound. An unknown id, one in another
//     channel and one the caller may not see (a whisper or net message they
//     are not on, one before their bound) are the one 404 message_not_found,
//     owners and operators included. Then the removal row and the
//     message_removed audit row, atomically. A message already removed is
//     204 and writes nothing.
//
// The body, payload, author and recipient list are in no answer, frame or
// audit row. An archived channel allows it (Q1). A message carrying an
// approval or admin-action card may be removed (Q2): only the message's
// payload is suppressed, the approval itself lives in /v1/approvals and is
// decided through its own routes.
//
// After a commit that removed the message, the tombstone is broadcast
// (broadcastMessageRemoved).
func (s *Server) handleRemoveMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	channel, ok := s.moderationChannel(w, r)
	if !ok {
		return
	}
	messageID, serr := parseEditMessageID(r.PathValue("message_id"))
	if serr != nil {
		writeReactionError(w, serr)
		return
	}
	removal, err := s.store.RemoveMessage(ctx, channel.ID, readerFor(r, apiV2), messageID, auditActor(ctx), s.now())
	if serr := reactionStoreError(ctx, err); serr != nil {
		writeReactionError(w, serr)
		return
	}
	if removal.Changed {
		s.broadcastMessageRemoved(ctx, removal)
	}
	w.WriteHeader(http.StatusNoContent)
}

// broadcastMessageRemoved tells the live audience, after the commit, with the
// tombstone and nothing else:
//
//   - a channel-wide message goes to every v0, v1 and v2 socket of the channel
//     (each socket's history gate still applies), to the configured
//     Broadcaster as the safe v0 tombstone, as an ordinary channel-wide post
//     does, and to the events sockets as conch.message_removed.v1. v2 sockets
//     get the v2 envelope with removed: true; v0 and v1 get the fixed body;
//   - a scoped message goes to the v2 sockets of its original recipients only.
//     It never reaches a v0 or v1 socket, the Broadcaster, or any global
//     notification: nothing reveals that a whisper or net message existed.
//     The events sockets read it with each subscriber's own reader, so a
//     subscriber who may not see it is sent nothing.
func (s *Server) broadcastMessageRemoved(ctx context.Context, removal store.MessageRemoval) {
	msg := removal.Message
	v2 := messageV2FromStore(msg)
	if msg.Audience != nil {
		s.hub.BroadcastMessageV2(ctx, v2, removal.Recipients)
	} else {
		v0 := messageV0FromStore(msg)
		s.hub.BroadcastMessage(ctx, v0)
		s.hub.BroadcastReply(ctx, messageV1FromStore(msg), v2)
		s.broadcaster.BroadcastMessage(ctx, v0)
	}
	s.events.notifyMessageRemoved(msg.ChannelID, msg.ID)
}
