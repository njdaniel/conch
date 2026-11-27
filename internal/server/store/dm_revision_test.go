package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestDMRevisionCountsMessagesEditsAndRemovals(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	rev := func() DMConversation {
		t.Helper()
		c, err := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := rev(); c.Revision != 0 || !c.CreatedAt.IsZero() {
		t.Fatalf("empty conversation = revision %d, created_at %v; want 0 and none", c.Revision, c.CreatedAt)
	}
	m1 := f.send(t, conv, f.alice, "one")
	m2 := f.send(t, conv, f.bob, "two")
	if m1.Revision != 1 || m2.Revision != 2 {
		t.Fatalf("message revisions = %d, %d; want 1, 2", m1.Revision, m2.Revision)
	}
	c := rev()
	if c.Revision != 2 || !c.CreatedAt.Equal(m1.CreatedAt) {
		t.Fatalf("after two messages = revision %d, created_at %v; want 2 and the first message's time %v", c.Revision, c.CreatedAt, m1.CreatedAt)
	}
	edit, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, m1.ID, "one, edited", func() time.Time { return f.tick() })
	if err != nil || !edit.Changed || edit.Message.Revision != 3 {
		t.Fatalf("edit = %+v, %v; want revision 3", edit, err)
	}
	// An edit that changes nothing, a read mark and a state change bump nothing.
	if _, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, m1.ID, "one, edited", func() time.Time { return f.tick() }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.MarkDMRead(ctx, f.bob.ID, conv.ID, m2.ID, f.tick()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetDMState(ctx, f.bob.ID, conv.ID, schema.DMStateArchived, f.tick()); err != nil {
		t.Fatal(err)
	}
	if got := rev().Revision; got != 3 {
		t.Fatalf("revision = %d after a no-op edit, a read mark and an archive; want 3", got)
	}
	rm, err := f.s.RemoveDMMessage(ctx, f.bob.ID, conv.ID, m2.ID, f.tick())
	if err != nil || !rm.Changed || rm.Message.Revision != 4 {
		t.Fatalf("removal = %+v, %v; want revision 4", rm, err)
	}
	if again, err := f.s.RemoveDMMessage(ctx, f.bob.ID, conv.ID, m2.ID, f.tick()); err != nil || again.Changed || again.Message.Revision != 4 {
		t.Fatalf("repeat removal = %+v, %v; want unchanged at 4", again, err)
	}
	if got := rev().Revision; got != 4 {
		t.Fatalf("revision = %d, want 4", got)
	}
	// Another conversation counts apart.
	other := f.openPair(t, f.alice, f.carol)
	if m := f.send(t, other, f.alice, "x"); m.Revision != 1 {
		t.Errorf("first message of another conversation has revision %d, want 1", m.Revision)
	}
}

func TestListDMChanges(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	m1 := f.send(t, conv, f.alice, "one")
	m2 := f.send(t, conv, f.bob, "two")
	m3 := f.send(t, conv, f.alice, "three")
	if _, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, m1.ID, "one!", func() time.Time { return f.tick() }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RemoveDMMessage(ctx, f.bob.ID, conv.ID, m2.ID, f.tick()); err != nil {
		t.Fatal(err)
	}
	ids := func(after int64, limit int, viewer Principal) (got []int64, revs []int64, msgs []DMMessage) {
		t.Helper()
		msgs, err := f.s.ListDMChanges(ctx, viewer.ID, conv.ID, after, limit)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			got = append(got, m.ID)
			revs = append(revs, m.Revision)
		}
		return got, revs, msgs
	}
	// Everything, once each, as it is now, by revision: m3 (3), m1 (4), m2 (5).
	got, revs, msgs := ids(0, 10, f.alice)
	if len(got) != 3 || got[0] != m3.ID || got[1] != m1.ID || got[2] != m2.ID || revs[0] != 3 || revs[1] != 4 || revs[2] != 5 {
		t.Fatalf("changes = ids %v revisions %v; want [%d %d %d] at [3 4 5]", got, revs, m3.ID, m1.ID, m2.ID)
	}
	if msgs[1].Body != "one!" || !msgs[2].Removed || msgs[2].Body != schema.MessageRemovedByAuthorBody {
		t.Errorf("changes carry %+v; want the edit as it is and the tombstone", msgs)
	}
	if got, _, _ := ids(3, 10, f.bob); len(got) != 2 || got[0] != m1.ID {
		t.Errorf("after 3 = %v, want the edit and the removal", got)
	}
	if got, _, _ := ids(0, 2, f.alice); len(got) != 2 || got[1] != m1.ID {
		t.Errorf("limit 2 = %v", got)
	}
	if got, _, _ := ids(5, 10, f.alice); len(got) != 0 {
		t.Errorf("after the newest = %v, want none", got)
	}
	// The visibility function governs it: a stranger, a disabled participant.
	if _, err := f.s.ListDMChanges(ctx, f.carol.ID, conv.ID, 0, 10); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("a stranger reads changes: %v", err)
	}
	if _, err := f.s.ListDMChanges(ctx, f.alice.ID, conv.ID, -1, 10); err == nil {
		t.Error("a negative revision was accepted")
	}
	if _, err := f.s.DisablePrincipal(ctx, "system", f.bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ListDMChanges(ctx, f.bob.ID, conv.ID, 0, 10); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("a disabled participant reads changes: %v", err)
	}
}

func TestDMWhitespaceOnlyBodiesAreRefused(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	msg := f.send(t, conv, f.alice, "hello")
	for _, body := range []string{"", " ", "\t\n ", "  "} {
		_, _, err := f.s.InsertDMMessage(ctx, DMPost{ConversationID: conv.ID, AuthorID: f.alice.ID, Body: body, ClientNonce: "w"}, f.tick())
		if !errors.Is(err, ErrDMEmptyBody) {
			t.Errorf("send %q = %v, want ErrDMEmptyBody", body, err)
		}
		if _, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, body, func() time.Time { return f.tick() }); !errors.Is(err, ErrDMEmptyBody) {
			t.Errorf("edit to %q = %v, want ErrDMEmptyBody", body, err)
		}
	}
	// Not being in the conversation is answered first.
	if _, _, err := f.s.InsertDMMessage(ctx, DMPost{ConversationID: conv.ID, AuthorID: f.carol.ID, Body: " ", ClientNonce: "w"}, f.tick()); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("a stranger's blank send = %v, want ErrDMNotFound", err)
	}
	if c, _ := f.s.DMConversationFor(ctx, f.alice.ID, conv.ID); c.Revision != 1 {
		t.Errorf("revision = %d after refused sends and edits, want 1", c.Revision)
	}
}

func TestListDMConversationsOrdersByLastMessageAt(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	ab, ac, ad := f.openPair(t, f.alice, f.bob), f.openPair(t, f.alice, f.carol), f.open(t, f.alice, f.dave)
	f.send(t, ab, f.bob, "1")
	f.send(t, ac, f.carol, "2")
	f.send(t, ab, f.bob, "3")
	var all []DMConversation
	cur := DMListCursor{}
	for {
		page, err := f.s.ListDMConversations(ctx, f.alice.ID, schema.DMStateOpen, cur, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		all = append(all, page[0])
		cur = page[0].CursorAfter()
	}
	// ab (newest), ac, then ad, which has no message and was opened by alice.
	if len(all) != 3 || all[0].ID != ab.ID || all[1].ID != ac.ID || all[2].ID != ad.ID {
		t.Fatalf("list = %+v; want ab, ac, then the empty ad", all)
	}
	if !all[2].CreatedAt.IsZero() || all[2].Revision != 0 || all[2].LastMessageID != 0 {
		t.Errorf("empty entry = %+v; want no created_at, revision 0, no last message", all[2])
	}
	// Dave never sees the empty pair in his list.
	if l, err := f.s.ListDMConversations(ctx, f.dave.ID, schema.DMStateOpen, DMListCursor{}, 10); err != nil || len(l) != 0 {
		t.Errorf("dave's list = %+v, %v; want none until a message", l, err)
	}
	if got := all[0].Revision; got != 2 {
		t.Errorf("ab revision = %d, want 2", got)
	}
}
