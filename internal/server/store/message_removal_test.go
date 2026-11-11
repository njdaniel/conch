package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Message removal in the store (issue #852, docs/design/moderation.md §7):
// the one visibility rule decides who can remove what, the removal and its
// audit row are one transaction, a repeat writes nothing, and no read hands
// out an original.

func (f *audienceFixture) countRows(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *audienceFixture) removeAs(who Principal, msg int64) (MessageRemoval, error) {
	return f.s.RemoveMessage(context.Background(), f.ch.ID, rdr(who), msg, principalActor(who.ID), time.Now())
}

func TestRemoveMessageRules(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	yes, no := true, false
	// erin manages the channel; alice (the creator of the nets) is a member.
	if _, err := f.s.PutChannelMember(ctx, "system", f.ch.ID, f.erin.ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	wide := f.wide(t, f.alice, "SECRET wide")
	net1, _ := f.toNet(t, f.alice, f.n1, "SECRET net one")
	whisper := f.whisper(t, f.alice, "SECRET whisper", f.dave)
	other, err := f.s.InsertMessageV1(ctx, f.other.ID, f.alice.ID, "SECRET elsewhere", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		who  Principal
		msg  int64
		want error
	}{
		{"a plain member", f.dave, wide, ErrManagerRefused},
		{"a stranger", f.carol, wide, ErrNotChannelMember},
		{"the manager, a net message off their net", f.erin, net1, ErrMessageNotFound},
		{"the manager, a whisper they are not in", f.erin, whisper, ErrMessageNotFound},
		{"the manager, a ghost", f.erin, 99999, ErrMessageNotFound},
		{"the manager, a message of another channel", f.erin, other.ID, ErrMessageNotFound},
		{"the manager, a non-positive id", f.erin, 0, ErrMessageNotFound},
		{"the manager, the channel-wide message", f.erin, wide, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.removeAs(tt.who, tt.msg)
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if n := f.countRows(t, "SELECT COUNT(*) FROM message_removals"); n != 1 {
		t.Errorf("%d removal rows, want only the successful one", n)
	}
	// A reader that is not scoped is no one's moderator.
	if _, err := f.s.RemoveMessage(ctx, f.ch.ID, Reader{PrincipalID: f.erin.ID}, net1, "x", time.Now()); !errors.Is(err, ErrNotChannelMember) {
		t.Errorf("an unscoped reader: %v", err)
	}
}

func TestRemoveMessageIdempotentAtomicAndAudited(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	yes, no := true, false
	if _, err := f.s.PutChannelMember(ctx, "system", f.ch.ID, f.erin.ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	msg := f.wide(t, f.alice, "SECRET body")
	// A failing audit write rolls the removal back.
	if _, err := f.s.db.Exec(`CREATE TRIGGER fail_removal_audit BEFORE INSERT ON audit_events WHEN NEW.action = 'message_removed'
		BEGIN SELECT RAISE(ABORT, 'audit down'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.removeAs(f.erin, msg); err == nil {
		t.Fatal("the removal committed with its audit row failing")
	}
	if n := f.countRows(t, "SELECT COUNT(*) FROM message_removals"); n != 0 {
		t.Errorf("%d removal rows after a failed audit", n)
	}
	if _, err := f.s.db.Exec("DROP TRIGGER fail_removal_audit"); err != nil {
		t.Fatal(err)
	}
	first, err := f.removeAs(f.erin, msg)
	if err != nil || !first.Changed || !first.Message.Removed || first.Message.Body != schema.MessageRemovedBody || first.Message.Payload != nil {
		t.Fatalf("first removal = %+v, %v", first, err)
	}
	again, err := f.removeAs(f.erin, msg)
	if err != nil || again.Changed || !again.Message.Removed {
		t.Fatalf("second removal = %+v, %v; want a no-op", again, err)
	}
	if n := f.countRows(t, "SELECT COUNT(*) FROM audit_events WHERE action = ? AND subject = ?", AuditMessageRemoved, messageSubject(msg)); n != 1 {
		t.Errorf("%d audit rows for two removals, want 1", n)
	}
	var detail, actor string
	if err := f.s.db.QueryRow("SELECT actor, detail FROM audit_events WHERE action = ?", AuditMessageRemoved).Scan(&actor, &detail); err != nil {
		t.Fatal(err)
	}
	if actor != principalActor(f.erin.ID) || strings.Contains(detail, "SECRET") || detail != fmt.Sprintf("channel=%d", f.ch.ID) {
		t.Errorf("audit row = %q %q", actor, detail)
	}
}

// Every read of a removed message projects the tombstone, and nothing returns
// the original: the lists in both directions, a thread, a search, the reply
// root, the author's own reads, and a file attached to it.
func TestRemovedMessageIsATombstoneOnEveryRead(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	yes, no := true, false
	if _, err := f.s.PutChannelMember(ctx, "system", f.ch.ID, f.erin.ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	root := f.wide(t, f.alice, "SECRET needle root")
	reply, _, err := f.s.InsertReplyMessage(ctx, ReplyPost{ChannelID: f.ch.ID, AuthorID: f.dave.ID, RootID: root, Body: "SECRET needle reply"}, rdr(f.dave))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.removeAs(f.erin, root); err != nil {
		t.Fatal(err)
	}
	check := func(what string, msgs []Message, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		found := false
		for _, m := range msgs {
			if strings.Contains(m.Body, "SECRET needle root") {
				t.Errorf("%s returned the original: %+v", what, m)
			}
			if m.ID == root {
				found = true
				if !m.Removed || m.Body != schema.MessageRemovedBody || m.Payload != nil || !m.EditedAt.IsZero() {
					t.Errorf("%s: root = %+v, want the tombstone", what, m)
				}
			}
		}
		if !found {
			t.Errorf("%s did not return the tombstone of %d", what, root)
		}
	}
	r := rdr(f.dave)
	msgs, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, r, MessagePage{}, 10)
	check("ascending list", msgs, err)
	msgs, err = f.s.ListVisibleMessagesPage(ctx, f.ch.ID, r, MessagePage{NewestFirst: true}, 10)
	check("newest-first list", msgs, err)
	msgs, err = f.s.ListThreadMessages(ctx, f.ch.ID, r, root, 0, 10)
	check("thread", msgs, err)
	if len(msgs) != 2 || msgs[1].ID != reply.ID || msgs[1].Removed || msgs[1].ThreadRootID != root {
		t.Errorf("the reply of a removed root = %+v", msgs)
	}
	rm, err := f.s.ReplyRootMessage(ctx, f.ch.ID, r, root)
	check("reply root", []Message{rm}, err)
	own, err := f.s.ListVisibleMessages(ctx, f.ch.ID, ChannelWideOnly, 0, 10)
	check("channel-wide reader", own, err)
	// Search never returns it, before the limit.
	found, err := f.s.SearchVisibleMessages(ctx, f.ch.ID, r, []string{"needle"}, 0, 1)
	if err != nil || len(found) != 1 || found[0].ID != reply.ID {
		t.Errorf("search with limit 1 = %+v, %v; want the reply alone", found, err)
	}
	// The author's own reads.
	if _, err := f.s.OwnMessageRevisions(ctx, f.ch.ID, rdr(f.alice), root); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("revisions of a removed message: %v", err)
	}
	if _, err := f.s.EditOwnMessage(ctx, f.ch.ID, rdr(f.alice), root, "SECRET again", time.Now); !errors.Is(err, ErrMessageNotEditable) {
		t.Errorf("editing a removed message: %v", err)
	}
	// A new reply to the removed root is the unusable-root answer.
	if _, _, err := f.s.InsertReplyMessage(ctx, ReplyPost{ChannelID: f.ch.ID, AuthorID: f.dave.ID, RootID: root, Body: "late"}, rdr(f.dave)); !errors.Is(err, ErrThreadNotFound) {
		t.Errorf("reply to a removed root: %v", err)
	}
	// The messages row itself is untouched: the original is in the database.
	var body string
	if err := f.s.db.QueryRow("SELECT body FROM messages WHERE id = ?", root).Scan(&body); err != nil || body != "SECRET needle root" {
		t.Errorf("the stored row = %q, %v; the original must stay in the database", body, err)
	}
}
