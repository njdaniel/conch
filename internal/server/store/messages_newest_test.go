package store

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

// Newest-first pages walk the channel backwards under the same visibility
// rule as the ascending read: a message the reader may not see neither fills a
// page nor moves the cursor, and before pages back from the smallest id held
// (issue #334).
func TestListVisibleMessagesPageNewestFirst(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	// frank is on no net, so he sees the channel-wide messages and none of
	// the whispers and net messages between them.
	var franks []int64 // ascending
	for i := 0; i < 6; i++ {
		franks = append(franks, f.wide(t, f.alice, fmt.Sprintf("open %d", i)))
		f.whisper(t, f.erin, "hidden whisper", f.alice)
		f.toNet(t, f.alice, f.n1, "hidden net")
	}
	newestFirst := make([]int64, len(franks))
	for i, id := range franks {
		newestFirst[len(franks)-1-i] = id
	}
	frank := Reader{f.frank.ID, true}

	for _, limit := range []int{1, 2, 4, 100} {
		var got []int64
		before := int64(0)
		for pages := 0; ; pages++ {
			if pages > 100 {
				t.Fatalf("limit %d: paging did not terminate", limit)
			}
			page, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, frank, MessagePage{Before: before, NewestFirst: true}, limit+1)
			if err != nil {
				t.Fatal(err)
			}
			more := len(page) > limit
			if more {
				page = page[:limit]
			}
			for _, m := range page {
				got = append(got, m.ID)
				before = m.ID
			}
			if !more {
				break
			}
		}
		if !reflect.DeepEqual(got, newestFirst) {
			t.Errorf("limit %d: paged %v, want %v", limit, got, newestFirst)
		}
	}

	// A member of net n1 sees the net messages as well, newest first among
	// all of them, and still not the whisper to someone else.
	dave, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, Reader{f.dave.ID, true}, MessagePage{NewestFirst: true}, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(dave); i++ {
		if dave[i-1].ID <= dave[i].ID {
			t.Fatalf("not newest first: %d then %d", dave[i-1].ID, dave[i].ID)
		}
	}
	if len(dave) != 12 {
		t.Errorf("dave sees %d messages, want the 6 open and 6 net ones", len(dave))
	}

	// The cursor is exclusive, and a cursor at the oldest message is empty.
	page, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, frank, MessagePage{Before: franks[2], NewestFirst: true}, 100)
	if want := []int64{franks[1], franks[0]}; err != nil || !reflect.DeepEqual(messageIDs(page), want) {
		t.Errorf("before %d = %v, %v; want %v", franks[2], messageIDs(page), err, want)
	}
	if page, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, frank, MessagePage{Before: franks[0], NewestFirst: true}, 100); err != nil || len(page) != 0 {
		t.Errorf("before the oldest = %v, %v; want none", messageIDs(page), err)
	}
	// A reader who cannot be scoped sees the channel-wide messages only.
	if page, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, Reader{}, MessagePage{NewestFirst: true}, 100); err != nil || !reflect.DeepEqual(messageIDs(page), newestFirst) {
		t.Errorf("an unscoped reader read %v, %v; want %v", messageIDs(page), err, newestFirst)
	}
	// The ascending read is the same as ListVisibleMessages.
	asc, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, frank, MessagePage{After: franks[1]}, 100)
	want, err2 := f.s.ListVisibleMessages(ctx, f.ch.ID, frank, franks[1], 100)
	if err != nil || err2 != nil || !reflect.DeepEqual(asc, want) {
		t.Errorf("ascending page = %v, %v; ListVisibleMessages = %v, %v", messageIDs(asc), err, messageIDs(want), err2)
	}

	// A cursor that does not belong to the direction is a mistake, not a guess.
	for _, p := range []MessagePage{{After: 1, NewestFirst: true}, {Before: 1}} {
		if _, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, frank, p, 10); err == nil {
			t.Errorf("ListVisibleMessagesPage(%+v) = nil error", p)
		}
	}
	if _, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, frank, MessagePage{NewestFirst: true}, 0); err == nil {
		t.Error("a limit of 0 was accepted")
	}
}

func messageIDs(msgs []Message) []int64 {
	ids := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

// The message list must be served by the channel/id index in either
// direction, with no temp B-tree for the ORDER BY (issue #334).
func TestListVisibleMessagesPlan(t *testing.T) {
	f := newAudienceFixture(t)
	for _, scoped := range []bool{true, false} {
		for _, desc := range []bool{true, false} {
			t.Run(fmt.Sprintf("scoped=%v desc=%v", scoped, desc), func(t *testing.T) {
				_, visible := visibilityArgs(Reader{PrincipalID: f.frank.ID, Scoped: scoped})
				args := append([]any{f.ch.ID, int64(0), int64(math.MaxInt64)}, visible...)
				rows, err := f.s.db.Query("EXPLAIN QUERY PLAN "+listSQL(scoped, desc), append(args, 11)...)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = rows.Close() }()
				var plan []string
				for rows.Next() {
					var id, parent, unused int
					var detail string
					if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
						t.Fatal(err)
					}
					plan = append(plan, detail)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				text := strings.Join(plan, "\n")
				t.Log("\n" + text)
				if strings.Contains(text, "TEMP B-TREE") {
					t.Errorf("the plan sorts in a temp B-tree:\n%s", text)
				}
				// The outer step reads the messages themselves: it must search the
				// channel/id index, not scan by rowid and filter the channel.
				if len(plan) == 0 || !strings.Contains(plan[0], "USING INDEX messages_by_channel") {
					t.Errorf("the messages are not read through messages_by_channel:\n%s", text)
				}
			})
		}
	}
}
