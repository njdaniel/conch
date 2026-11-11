package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// largeFeedWorld is a server of about a million messages for the feed's cost
// (issue #1725): the reader (dave) is a member of `mine` channels and not of
// `others`, and every message is written by frank, interleaved round-robin
// over all channels, so message ids of the reader's channels are spread
// through the table as they are on a real server.
type largeFeedWorld struct {
	f       *feedFixture
	mine    []int64
	others  []int64
	perChan int
}

func newLargeFeedWorld(t *testing.T, mine, others, perChan int) *largeFeedWorld {
	t.Helper()
	f := newFeedFixture(t)
	w := &largeFeedWorld{f: f, perChan: perChan}
	ctx := context.Background()
	make1 := func(prefix string, i int, member bool) int64 {
		ch, err := f.s.CreateChannel(ctx, fmt.Sprintf("%s%d", prefix, i))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.AddChannelMember(ctx, "system", ch.ID, f.frank.ID, 0); err != nil {
			t.Fatal(err)
		}
		if member {
			if _, err := f.s.AddChannelMember(ctx, "system", ch.ID, f.dave.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
		return ch.ID
	}
	for i := range mine {
		w.mine = append(w.mine, make1("mine", i, true))
	}
	for i := range others {
		w.others = append(w.others, make1("other", i, false))
	}
	all := append(append([]int64{}, w.mine...), w.others...)
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare("INSERT INTO messages (channel_id, author_id, body, created_at) VALUES (?, ?, ?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	for i := range perChan {
		for _, ch := range all {
			if _, err := stmt.Exec(ch, f.frank.ID, "chatter", notificationsT0.UnixMilli()+int64(i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return w
}

// markLeaving sets the reader's read mark in each of their channels so that
// leave messages of the channel are unread (0: none).
func (w *largeFeedWorld) markLeaving(t *testing.T, leave int) {
	t.Helper()
	if _, err := w.f.s.db.Exec("DELETE FROM read_marks"); err != nil {
		t.Fatal(err)
	}
	for _, ch := range w.mine {
		var mark int64
		if err := w.f.s.db.QueryRow("SELECT id FROM messages WHERE channel_id = ? ORDER BY id DESC LIMIT 1 OFFSET ?", ch, leave).Scan(&mark); err != nil {
			t.Fatal(err)
		}
		if _, err := w.f.s.db.Exec("INSERT INTO read_marks (principal_id, channel_id, last_read_id, updated_at) VALUES (?, ?, ?, 0)", w.f.dave.ID, ch, mark); err != nil {
			t.Fatal(err)
		}
	}
}

// TestPendingNotificationsLargeServerTiming prints the time of one feed call
// on a server of about a million messages, for a reader with a large backlog,
// for one with nothing new, and for a reader with a thousand-message backlog
// on the same server, with and without ANALYZE. It asserts only that the
// answers are right and that no call is slower than a generous ceiling (the
// numbers are in the log, run with -v): a timing test must not be a flake.
// Skipped under -short and unless CONCH_BENCH is set, so make check does not
// build a million rows.
func TestPendingNotificationsLargeServerTiming(t *testing.T) {
	if testing.Short() || os.Getenv("CONCH_BENCH") == "" {
		t.Skip("set CONCH_BENCH=1 to build a million-message server")
	}
	// 1M messages: 5 channels the reader is in (4 x 100k unread each... see
	// perChan), 5 they are not in.
	w := newLargeFeedWorld(t, 5, 5, 100_000)
	f := w.f
	f.policy(t, schema.NotificationPolicyAll, false)
	type result = schema.ListPendingNotificationsResponseV1
	cases := []struct {
		name   string
		policy schema.NotificationPolicy
		leave  int // unread messages left in each of the reader's channels
		want   func(result) bool
	}{
		{"backlog 500k, all, first page", schema.NotificationPolicyAll, 100_000,
			func(r result) bool { return len(r.Summaries) == schema.MaxPendingNotificationsPage && r.More }},
		{"backlog 500k, mentions, 2000 examined", schema.NotificationPolicyMentions, 100_000,
			func(r result) bool { return len(r.Summaries) == 0 && r.More }},
		{"backlog 500, all, first page", schema.NotificationPolicyAll, 100,
			func(r result) bool { return len(r.Summaries) == schema.MaxPendingNotificationsPage && r.More }},
		{"nothing new, 500k messages elsewhere", schema.NotificationPolicyAll, 0,
			func(r result) bool { return len(r.Summaries) == 0 && !r.More }},
	}
	for _, analyze := range []bool{false, true} {
		if analyze {
			if _, err := f.s.db.Exec("ANALYZE"); err != nil {
				t.Fatal(err)
			}
		}
		for _, tc := range cases {
			f.policy(t, tc.policy, false)
			if tc.leave >= 100_000 {
				if _, err := f.s.db.Exec("DELETE FROM read_marks"); err != nil {
					t.Fatal(err)
				}
			} else {
				w.markLeaving(t, tc.leave)
			}
			start := time.Now()
			resp := f.feed(t, 0)
			d := time.Since(start)
			if !tc.want(resp) {
				t.Errorf("analyze=%v %s: unexpected answer: %d summaries, more=%v", analyze, tc.name, len(resp.Summaries), resp.More)
			}
			t.Logf("analyze=%-5v %-40s %v", analyze, tc.name, d)
			if d > 5*time.Second {
				t.Errorf("analyze=%v %s: took %v", analyze, tc.name, d)
			}
		}
	}
}

// A reader in a great many channels: the cost is a seek per channel and the
// bound's worth of rows, so it is measured (CONCH_BENCH=1, -v) rather than
// assumed. 2,000 channels of 150 messages each, all unread.
func TestPendingNotificationsManyChannelsTiming(t *testing.T) {
	if testing.Short() || os.Getenv("CONCH_BENCH") == "" {
		t.Skip("set CONCH_BENCH=1 to build a 300k-message server")
	}
	w := newLargeFeedWorld(t, 2000, 0, 150)
	f := w.f
	f.policy(t, schema.NotificationPolicyMentions, false)
	start := time.Now()
	resp := f.feed(t, 0)
	d := time.Since(start)
	t.Logf("2000 channels, 300k unread, mentions policy: %v", d)
	if len(resp.Summaries) != 0 || !resp.More {
		t.Errorf("answer = %d summaries, more=%v, want none and more", len(resp.Summaries), resp.More)
	}
	if d > 5*time.Second {
		t.Errorf("took %v", d)
	}
}

// A reader in a very great many channels (issue #1730): the heap merge makes
// the merge itself O(output x log channels), so the cost is the seek per
// channel. 20,000 channels of 5 messages each, all unread.
func TestPendingNotificationsVeryManyChannelsTiming(t *testing.T) {
	if testing.Short() || os.Getenv("CONCH_BENCH") == "" {
		t.Skip("set CONCH_BENCH=1 to build a 100k-message, 20,000-channel server")
	}
	w := newLargeFeedWorld(t, 20_000, 0, 5)
	f := w.f
	for _, policy := range []schema.NotificationPolicy{schema.NotificationPolicyMentions, schema.NotificationPolicyAll} {
		f.policy(t, policy, false)
		start := time.Now()
		resp := f.feed(t, 0)
		d := time.Since(start)
		t.Logf("20000 channels, 100k unread, %s policy: %v", policy, d)
		wantPage := policy == schema.NotificationPolicyAll
		if (len(resp.Summaries) == schema.MaxPendingNotificationsPage) != wantPage || !resp.More {
			t.Errorf("%s: answer = %d summaries, more=%v", policy, len(resp.Summaries), resp.More)
		}
		if d > 10*time.Second {
			t.Errorf("%s: took %v", policy, d)
		}
	}
}

// Hidden whispers in the reader's own channel (issue #1730): the residual the
// design accepts. Every whisper is addressed to someone else, so the answer
// is empty whatever the volume; the time is the walk past them, and is
// logged, to show that it grows with the volume and that the answer does not.
func TestPendingNotificationsHiddenWhispersTiming(t *testing.T) {
	if testing.Short() || os.Getenv("CONCH_BENCH") == "" {
		t.Skip("set CONCH_BENCH=1 to build a 400k-whisper server")
	}
	f := newFeedFixture(t)
	f.policy(t, schema.NotificationPolicyAll, false)
	ctx := context.Background()
	ch, err := f.s.CreateChannel(ctx, "whispers")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []int64{f.dave.ID, f.frank.ID, f.erin.ID} {
		if _, err := f.s.AddChannelMember(ctx, "system", ch.ID, p, 0); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	msg, err := tx.Prepare("INSERT INTO messages (channel_id, author_id, body, created_at, audience_kind) VALUES (?, ?, 'psst', ?, 'principals')")
	if err != nil {
		t.Fatal(err)
	}
	rcpt, err := tx.Prepare("INSERT INTO message_recipients (message_id, principal_id) VALUES (?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	built := 0
	for _, volume := range []int{100_000, 400_000} {
		for ; built < volume; built++ {
			res, err := msg.Exec(ch.ID, f.frank.ID, notificationsT0.UnixMilli()+int64(built))
			if err != nil {
				t.Fatal(err)
			}
			id, _ := res.LastInsertId()
			if _, err := rcpt.Exec(id, f.erin.ID); err != nil {
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		resp := f.feed(t, 0)
		d := time.Since(start)
		t.Logf("%d whispers not to the reader, in their channel: %v", volume, d)
		if len(resp.Summaries) != 0 || resp.More {
			t.Errorf("answer = %d summaries, more=%v, want an empty answer", len(resp.Summaries), resp.More)
		}
		if d > 5*time.Second {
			t.Errorf("%d whispers: took %v", volume, d)
		}
		if tx, err = f.s.db.Begin(); err != nil {
			t.Fatal(err)
		}
		if msg, err = tx.Prepare("INSERT INTO messages (channel_id, author_id, body, created_at, audience_kind) VALUES (?, ?, 'psst', ?, 'principals')"); err != nil {
			t.Fatal(err)
		}
		if rcpt, err = tx.Prepare("INSERT INTO message_recipients (message_id, principal_id) VALUES (?, ?)"); err != nil {
			t.Fatal(err)
		}
	}
	_ = tx.Rollback()
}
