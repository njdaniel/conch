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
	return f.s.RemoveMessage(context.Background(), f.ch.ID, rdr(who), msg, principalActor(who.ID), 0, time.Now())
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
	if _, err := f.s.RemoveMessage(ctx, f.ch.ID, Reader{PrincipalID: f.erin.ID}, net1, "x", 0, time.Now()); !errors.Is(err, ErrNotChannelMember) {
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

func TestRemoveOwnMessageRules(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	wide := f.wide(t, f.alice, "SECRET author's text")
	whisper := f.whisper(t, f.alice, "SECRET whisper", f.dave)
	net, _ := f.toNet(t, f.alice, f.n1, "SECRET net")
	for _, tt := range []struct {
		name string
		who  Principal
		id   int64
		want error
	}{
		{"non-author visible", f.dave, wide, ErrManagerRefused},
		{"non-author invisible", f.frank, whisper, ErrMessageNotFound},
		{"unknown", f.alice, 999999, ErrMessageNotFound},
		{"non-member", f.carol, wide, ErrNotChannelMember},
		{"own channel-wide", f.alice, wide, nil},
		{"own whisper", f.alice, whisper, nil},
		{"own net", f.alice, net, nil},
		{"repeat", f.alice, wide, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.s.RemoveOwnMessage(ctx, f.ch.ID, rdr(tt.who), tt.id, principalActor(tt.who.ID), time.Now().Add(24*time.Hour))
			if !errors.Is(err, tt.want) {
				t.Fatalf("error %v, want %v", err, tt.want)
			}
			if err == nil && (!got.Message.RemovedByAuthor || got.Message.Body != schema.MessageRemovedByAuthorBody) {
				t.Fatalf("wrong tombstone: %+v", got)
			}
			if tt.name == "repeat" && got.Changed {
				t.Fatal("repeat wrote a removal")
			}
		})
	}
	if n := f.countRows(t, "SELECT COUNT(*) FROM audit_events WHERE action = ? AND detail = ?", AuditMessageRemoved, fmt.Sprintf("channel=%d removed_by=author", f.ch.ID)); n != 3 {
		t.Fatalf("%d author audits, want 3", n)
	}
	// The projection, not only the write answer, carries the author kind.
	got, err := f.s.ListVisibleMessages(ctx, f.ch.ID, rdr(f.alice), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range got {
		if !m.RemovedByAuthor || m.Body != schema.MessageRemovedByAuthorBody || m.Payload != nil || !m.EditedAt.IsZero() {
			t.Fatalf("unsafe projection: %+v", m)
		}
	}
}

func TestRemoveOwnMessageProtectsCardsAndHistory(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	for _, name := range []string{schema.ApprovalV1Name, schema.AdminActionV1Name, "conch.approval.v2"} {
		row, err := f.s.db.Exec("INSERT INTO messages(channel_id, author_id, body, created_at, payload_schema, payload_json) VALUES (?, ?, 'SECRET card', 1, ?, '{}')", f.ch.ID, f.alice.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		id, err := row.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.RemoveOwnMessage(ctx, f.ch.ID, rdr(f.alice), id, "alice", time.Now()); !errors.Is(err, ErrManagerRefused) {
			t.Fatalf("card %s: %v", name, err)
		}
	}
	id := f.wide(t, f.alice, "before bound")
	if _, err := f.s.db.Exec("UPDATE channel_members SET history_after = ? WHERE channel_id = ? AND principal_id = ?", id, f.ch.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RemoveOwnMessage(ctx, f.ch.ID, rdr(f.alice), id, "alice", time.Now()); !errors.Is(err, ErrMessageNotFound) {
		t.Fatalf("before bound: %v", err)
	}
}

func TestAuthorRemovalRejectsAgentsAndPreservesModeration(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	agent, err := f.s.CreatePrincipal(ctx, PrincipalAgent, "agent-remover")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AddChannelMember(ctx, "system", f.ch.ID, agent.ID, 0); err != nil {
		t.Fatal(err)
	}
	id := f.wide(t, agent, "agent text")
	if _, err := f.s.RemoveOwnMessage(ctx, f.ch.ID, rdr(agent), id, "agent", time.Now()); !errors.Is(err, ErrManagerRefused) {
		t.Fatalf("agent can remove own: %v", err)
	}
	yes, no := true, false
	if _, err := f.s.PutChannelMember(ctx, "system", f.ch.ID, f.alice.ID, 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
	own := f.wide(t, f.alice, "manager's own text")
	removed, err := f.removeAs(f.alice, own)
	if err != nil || removed.Message.RemovedByAuthor || removed.Message.Body != schema.MessageRemovedBody {
		t.Fatalf("moderator's own removal changed: %+v %v", removed, err)
	}
	repeated, err := f.s.RemoveOwnMessage(ctx, f.ch.ID, rdr(f.alice), own, "alice", time.Now())
	if err != nil || repeated.Changed || repeated.Message.RemovedByAuthor || repeated.Message.Body != schema.MessageRemovedBody {
		t.Fatalf("author retry changed moderator removal: %+v %v", repeated, err)
	}
}

func TestAuthorRemovalMigrationPreservesOldModeratorTombstone(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/conch.db"
	s := openTestStoreAt(t, path)
	ch, err := s.CreateChannel(ctx, "old-removal")
	if err != nil {
		t.Fatal(err)
	}
	person, err := s.CreatePrincipal(ctx, PrincipalHuman, "old-moderator")
	if err != nil {
		t.Fatal(err)
	}
	msg, err := s.InsertMessageV1(ctx, ch.ID, person.ID, "SECRET old text", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the previous schema, including a moderator's own removal.
	if _, err := s.db.Exec("DROP TABLE blocks"); err != nil { // migration 51 (issue #1832)
		t.Fatal(err)
	}
	if _, err := s.db.Exec("ALTER TABLE message_removals DROP COLUMN by_author"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO message_removals(message_id, channel_id, removed_by, removed_at) VALUES (?, ?, ?, 1)", msg.ID, ch.ID, person.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 49"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	migrated := openTestStoreAt(t, path)
	messages, err := migrated.ListVisibleMessages(ctx, ch.ID, Reader{}, 0, 10)
	if err != nil || len(messages) != 1 {
		t.Fatalf("read migrated removal: %+v %v", messages, err)
	}
	if messages[0].RemovedByAuthor || messages[0].Body != schema.MessageRemovedBody {
		t.Fatalf("migration relabeled old removal: %+v", messages[0])
	}
}
