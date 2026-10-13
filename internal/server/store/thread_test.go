package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// reply stores a reply by author to root, as the server does for a verified
// poster.
func (f *audienceFixture) reply(author Principal, root int64, body string) (Message, []int64, error) {
	return f.s.InsertReplyMessage(context.Background(),
		ReplyPost{ChannelID: f.ch.ID, AuthorID: author.ID, RootID: root, Body: body},
		Reader{PrincipalID: author.ID, Scoped: true})
}

func (f *audienceFixture) mustReply(t *testing.T, author Principal, root int64, body string) (Message, []int64) {
	t.Helper()
	m, rec, err := f.reply(author, root, body)
	if err != nil {
		t.Fatalf("reply by %d to %d: %v", author.ID, root, err)
	}
	return m, rec
}

// storedRecipients reads the recipient rows of a message straight from the
// table, ascending: the source of truth for who a message reached.
func storedRecipients(t *testing.T, s *Store, id int64) []int64 {
	t.Helper()
	rows, err := s.db.Query("SELECT principal_id FROM message_recipients WHERE message_id = ? ORDER BY principal_id", id)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	ids := []int64{}
	for rows.Next() {
		var p int64
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func contains(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func ids(ps ...Principal) []int64 {
	out := make([]int64, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}

// A reply is seen by exactly the people who can see its root: the stored
// recipients of the reply are the stored recipients of the root, for each kind
// of root and whoever replies, and every principal in the fixture sees the
// reply if and only if they see the root.
func TestReplyAudienceEqualsRootAudience(t *testing.T) {
	type root struct {
		name string
		post func(f *audienceFixture) int64
	}
	roots := []root{
		{"channel-wide", func(f *audienceFixture) int64 { return f.wide(t, f.alice, "root") }},
		{"net", func(f *audienceFixture) int64 { id, _ := f.toNet(t, f.alice, f.n1, "root"); return id }},
		{"whisper to two", func(f *audienceFixture) int64 { return f.whisper(t, f.alice, "root", f.bob, f.dave) }},
		{"whisper to one", func(f *audienceFixture) int64 { return f.whisper(t, f.alice, "root", f.erin) }},
	}
	for _, r := range roots {
		for _, replier := range []string{"alice", "bob", "dave", "erin", "frank"} {
			t.Run(r.name+"/"+replier, func(t *testing.T) {
				f := newAudienceFixture(t)
				rootID := r.post(f)
				who := map[string]Principal{"alice": f.alice, "bob": f.bob, "dave": f.dave, "erin": f.erin, "frank": f.frank}[replier]
				rootSeen := map[int64]bool{}
				everyone := []Principal{f.alice, f.bob, f.dave, f.erin, f.frank}
				for _, p := range everyone {
					rootSeen[p.ID] = contains(visibleIDs(t, f.s, f.ch.ID, Reader{p.ID, true}), rootID)
				}
				m, rec, err := f.reply(who, rootID, "re")
				if !rootSeen[who.ID] {
					if !errors.Is(err, ErrThreadNotFound) {
						t.Fatalf("a reply by someone who cannot see the root = %v, want ErrThreadNotFound", err)
					}
					return
				}
				if errors.Is(err, ErrNetMonitorOnly) {
					if r.name != "net" || replier != "bob" {
						t.Fatalf("unexpected monitor refusal for %s", replier)
					}
					return
				}
				if err != nil {
					t.Fatalf("reply: %v", err)
				}
				if m.ThreadRootID != rootID {
					t.Errorf("ThreadRootID = %d, want %d", m.ThreadRootID, rootID)
				}
				want := storedRecipients(t, f.s, rootID)
				if got := storedRecipients(t, f.s, m.ID); !reflect.DeepEqual(got, want) {
					t.Errorf("reply recipients = %v, root recipients = %v", got, want)
				}
				if len(want) > 0 && !reflect.DeepEqual(rec, want) {
					t.Errorf("returned recipients = %v, want %v", rec, want)
				}
				if len(want) == 0 && rec != nil {
					t.Errorf("a channel-wide reply returned recipients %v", rec)
				}
				for _, p := range everyone {
					replySeen := contains(visibleIDs(t, f.s, f.ch.ID, Reader{p.ID, true}), m.ID)
					if replySeen != rootSeen[p.ID] {
						t.Errorf("principal %d: sees the root = %v, sees the reply = %v", p.ID, rootSeen[p.ID], replySeen)
					}
				}
				// The wire audience of a scoped reply is its root's.
				switch r.name {
				case "net":
					if m.Audience == nil || m.Audience.Kind != schema.AudienceKindNet || m.Audience.NetID != f.n1.ID {
						t.Errorf("audience = %+v, want net %d", m.Audience, f.n1.ID)
					}
				case "channel-wide":
					if m.Audience != nil {
						t.Errorf("audience = %+v, want none", m.Audience)
					}
				default:
					if m.Audience == nil || m.Audience.Kind != schema.AudienceKindPrincipals || !reflect.DeepEqual(m.Audience.PrincipalIDs, want) {
						t.Errorf("audience = %+v, want principals %v", m.Audience, want)
					}
				}
				// And it reads back with the field, not only on insert.
				msgs, err := f.s.ListVisibleMessages(context.Background(), f.ch.ID, Reader{who.ID, true}, rootID, 10)
				if err != nil || len(msgs) != 1 || msgs[0].ID != m.ID || msgs[0].ThreadRootID != rootID {
					t.Errorf("read back = %+v (%v), want the reply with its root", msgs, err)
				}
			})
		}
	}
}

// In a whisper from A to B, B's reply reaches A and B and nobody else.
func TestWhisperReplyByRecipientReachesTheAuthor(t *testing.T) {
	f := newAudienceFixture(t)
	root := f.whisper(t, f.alice, "psst", f.bob)
	m, rec := f.mustReply(t, f.bob, root, "heard")
	if want := ids(f.alice, f.bob); !reflect.DeepEqual(rec, want) || !reflect.DeepEqual(storedRecipients(t, f.s, m.ID), want) {
		t.Fatalf("recipients = %v (stored %v), want %v", rec, storedRecipients(t, f.s, m.ID), want)
	}
	for _, p := range []Principal{f.dave, f.erin, f.frank} {
		if contains(visibleIDs(t, f.s, f.ch.ID, Reader{p.ID, true}), m.ID) {
			t.Errorf("principal %d sees a reply in a whisper they are not in", p.ID)
		}
	}
	if !contains(visibleIDs(t, f.s, f.ch.ID, Reader{f.alice.ID, true}), m.ID) {
		t.Error("the author of the whisper does not see the recipient's reply")
	}
}

// The reply goes to those who can see the root now, not to the roster as it
// was or as it is on the net.
func TestReplyAudienceFollowsWhoCanSeeTheRootNow(t *testing.T) {
	t.Run("someone who joined the net after the root cannot see it, nor its reply", func(t *testing.T) {
		f := newAudienceFixture(t)
		root, _ := f.toNet(t, f.alice, f.n1, "root")
		f.put(t, "n1", f.frank, schema.NetRoleMember)
		m, rec := f.mustReply(t, f.alice, root, "re")
		if want := ids(f.alice, f.bob, f.dave); !reflect.DeepEqual(rec, want) {
			t.Errorf("recipients = %v, want %v (not frank, who joined later)", rec, want)
		}
		if contains(visibleIDs(t, f.s, f.ch.ID, Reader{f.frank.ID, true}), m.ID) {
			t.Error("a later joiner sees a reply to a message they cannot see")
		}
	})
	t.Run("someone taken off the net keeps the root and gets no reply", func(t *testing.T) {
		// Taking a principal off a net cuts them off from what is said on it
		// from then on. They keep what they were sent, the root included, but
		// an old thread must not go on delivering the net's talk to them.
		f := newAudienceFixture(t)
		root, _ := f.toNet(t, f.alice, f.n1, "root")
		if _, err := f.s.RemoveNetMember(context.Background(), "system", f.ch.ID, "n1", f.dave.ID); err != nil {
			t.Fatal(err)
		}
		if !contains(visibleIDs(t, f.s, f.ch.ID, Reader{f.dave.ID, true}), root) {
			t.Fatal("setup: dave no longer sees the root")
		}
		m, rec := f.mustReply(t, f.alice, root, "re")
		if want := ids(f.alice, f.bob); !reflect.DeepEqual(rec, want) {
			t.Errorf("recipients = %v, want %v (not dave, who is off the net)", rec, want)
		}
		if got := storedRecipients(t, f.s, m.ID); !reflect.DeepEqual(got, ids(f.alice, f.bob)) {
			t.Errorf("stored recipients = %v, want alice and bob", got)
		}
		if contains(visibleIDs(t, f.s, f.ch.ID, Reader{f.dave.ID, true}), m.ID) {
			t.Error("dave, taken off the net, was sent a reply posted on it afterwards")
		}
		// And he cannot reply into it himself: he is no longer on the net.
		if _, _, err := f.reply(f.dave, root, "still here?"); !errors.Is(err, ErrNetNotFound) {
			t.Errorf("a reply from someone off the net: err = %v, want ErrNetNotFound", err)
		}
	})
	t.Run("a recipient who left the channel gets neither", func(t *testing.T) {
		f := newAudienceFixture(t)
		root := f.whisper(t, f.alice, "root", f.bob, f.dave)
		if _, err := f.s.RemoveChannelMember(context.Background(), "system", f.ch.ID, f.dave.ID); err != nil {
			t.Fatal(err)
		}
		m, rec := f.mustReply(t, f.bob, root, "re")
		if want := ids(f.alice, f.bob); !reflect.DeepEqual(rec, want) || !reflect.DeepEqual(m.Audience.PrincipalIDs, want) {
			t.Errorf("recipients = %v, audience = %+v, want %v", rec, m.Audience, want)
		}
		if got := storedRecipients(t, f.s, m.ID); !reflect.DeepEqual(got, ids(f.alice, f.bob)) {
			t.Errorf("stored recipients = %v", got)
		}
	})
	t.Run("a whisper left with only its poster is refused", func(t *testing.T) {
		f := newAudienceFixture(t)
		root := f.whisper(t, f.alice, "root", f.bob)
		if _, err := f.s.RemoveChannelMember(context.Background(), "system", f.ch.ID, f.alice.ID); err != nil {
			t.Fatal(err)
		}
		before := f.count(t)
		if _, _, err := f.reply(f.bob, root, "re"); !errors.Is(err, ErrInvalidAudience) {
			t.Errorf("err = %v, want ErrInvalidAudience", err)
		}
		if f.count(t) != before {
			t.Error("a refused reply stored a message")
		}
	})
}

func (f *audienceFixture) count(t *testing.T) int {
	t.Helper()
	n, err := f.s.CountMessages(context.Background(), f.ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func auditRows(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The four unusable roots, and the cases that fail closed, answer with one
// error and store nothing, audit rows included.
func TestReplyRefusedRootsStoreNothing(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	wide := f.wide(t, f.alice, "wide")
	whisper := f.whisper(t, f.alice, "secret", f.bob)
	elsewhere, err := f.s.InsertMessageV1(ctx, f.other.ID, f.alice.ID, "elsewhere", nil)
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := f.mustReply(t, f.bob, wide, "a reply")

	tests := []struct {
		name   string
		author Principal
		reader Reader
		root   int64
		want   error
	}{
		{"unknown", f.bob, Reader{f.bob.ID, true}, 9999, ErrThreadNotFound},
		{"zero", f.bob, Reader{f.bob.ID, true}, 0, ErrThreadNotFound},
		{"other channel", f.bob, Reader{f.bob.ID, true}, elsewhere.ID, ErrThreadNotFound},
		{"itself a reply", f.bob, Reader{f.bob.ID, true}, reply.ID, ErrThreadNotFound},
		{"not visible: a whisper to others", f.dave, Reader{f.dave.ID, true}, whisper, ErrThreadNotFound},
		{"not visible: a reader who cannot express an audience", f.bob, Reader{f.bob.ID, false}, whisper, ErrThreadNotFound},
		{"not visible: no identity", f.bob, Reader{}, whisper, ErrThreadNotFound},
		{"reader is not the poster", f.bob, Reader{f.alice.ID, true}, wide, ErrThreadNotFound},
		{"poster not in the channel", f.carol, Reader{f.carol.ID, true}, wide, ErrNotChannelMember},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages, audit := f.count(t), auditRows(t, f.s)
			_, _, err := f.s.InsertReplyMessage(ctx,
				ReplyPost{ChannelID: f.ch.ID, AuthorID: tt.author.ID, RootID: tt.root, Body: "x"}, tt.reader)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if f.count(t) != messages || auditRows(t, f.s) != audit {
				t.Errorf("a refused reply stored something: messages %d -> %d, audit rows %d -> %d",
					messages, f.count(t), audit, auditRows(t, f.s))
			}
			if _, err := f.s.ReplyRootMessage(ctx, f.ch.ID, tt.reader, tt.root); tt.reader.PrincipalID == tt.author.ID &&
				errors.Is(tt.want, ErrThreadNotFound) && !errors.Is(err, ErrThreadNotFound) {
				t.Errorf("ReplyRootMessage = %v, want ErrThreadNotFound", err)
			}
		})
	}

	// Without an identity only a channel-wide root can be answered, with no
	// membership to check.
	if m, rec, err := f.s.InsertReplyMessage(ctx, ReplyPost{ChannelID: f.ch.ID, AuthorID: f.bob.ID, RootID: wide, Body: "x"}, Reader{}); err != nil || rec != nil || m.ThreadRootID != wide {
		t.Errorf("unauthenticated reply to a channel-wide root = %+v, %v, %v", m, rec, err)
	}
}

// A scoped reply writes the same audit rows any scoped post writes.
func TestScopedReplyWritesTheScopedAuditRow(t *testing.T) {
	f := newAudienceFixture(t)
	root, _ := f.toNet(t, f.alice, f.n1, "root")
	m, _ := f.mustReply(t, f.dave, root, "re")
	events, err := f.s.ListAuditEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("message:%d", m.ID)
	var actions []string
	var detail string
	for _, e := range events {
		if e.Subject == subject {
			actions = append(actions, e.Action)
			if e.Action == AuditMessageScoped {
				detail = e.Detail
			}
		}
	}
	if want := []string{"message.post", AuditMessageScoped}; !reflect.DeepEqual(actions, want) {
		t.Errorf("audit actions for the reply = %v, want %v", actions, want)
	}
	if want := fmt.Sprintf("channel=%d audience=net net=%d recipients=%d,%d,%d", f.ch.ID, f.n1.ID, f.alice.ID, f.bob.ID, f.dave.ID); detail != want {
		t.Errorf("detail = %q, want %q", detail, want)
	}
}

func threadIDs(t *testing.T, s *Store, channel int64, r Reader, root, after int64, limit int) ([]int64, error) {
	t.Helper()
	msgs, err := s.ListThreadMessages(context.Background(), channel, r, root, after, limit)
	if err != nil {
		return nil, err
	}
	out := []int64{}
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out, nil
}

// The thread listing is the channel listing narrowed to the thread, for every
// reader and every kind of root; it pages by after and limit; and a root the
// reader cannot see is the one refusal.
func TestListThreadMessages(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	wide := f.wide(t, f.alice, "wide root")
	f.wide(t, f.bob, "noise")
	netRoot, _ := f.toNet(t, f.alice, f.n1, "net root")
	whisperRoot := f.whisper(t, f.alice, "whisper root", f.bob)
	var wideReplies, netReplies, whisperReplies []int64
	for i := 0; i < 3; i++ {
		m, _ := f.mustReply(t, f.bob, wide, fmt.Sprint("w", i))
		wideReplies = append(wideReplies, m.ID)
		f.wide(t, f.erin, "more noise")
		m, _ = f.mustReply(t, f.dave, netRoot, fmt.Sprint("n", i))
		netReplies = append(netReplies, m.ID)
		m, _ = f.mustReply(t, f.alice, whisperRoot, fmt.Sprint("p", i))
		whisperReplies = append(whisperReplies, m.ID)
	}
	threads := map[int64][]int64{
		wide:        append([]int64{wide}, wideReplies...),
		netRoot:     append([]int64{netRoot}, netReplies...),
		whisperRoot: append([]int64{whisperRoot}, whisperReplies...),
	}

	// Against the channel listing, for every reader.
	for _, p := range []Principal{f.alice, f.bob, f.dave, f.erin, f.frank} {
		reader := Reader{p.ID, true}
		all, err := f.s.ListVisibleMessages(ctx, f.ch.ID, reader, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for root, thread := range threads {
			var want []int64
			for _, m := range all {
				if m.ID == root || m.ThreadRootID == root {
					want = append(want, m.ID)
				}
			}
			got, err := threadIDs(t, f.s, f.ch.ID, reader, root, 0, 1000)
			if len(want) == 0 || want[0] != root {
				// The reader cannot see the root: one answer, whatever the thread holds.
				if !errors.Is(err, ErrThreadNotFound) {
					t.Errorf("principal %d, root %d: err = %v, want ErrThreadNotFound", p.ID, root, err)
				}
				continue
			}
			if err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(got, thread) {
				t.Errorf("principal %d, root %d: thread = %v (%v), channel listing narrowed = %v, want %v", p.ID, root, got, err, want, thread)
			}
		}
	}

	// Paging: the root comes first, then the replies, by after and limit.
	reader := Reader{f.bob.ID, true}
	thread := threads[wide]
	for limit := 1; limit <= len(thread)+1; limit++ {
		var got []int64
		after := int64(0)
		for pages := 0; pages < 10; pages++ {
			page, err := threadIDs(t, f.s, f.ch.ID, reader, wide, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			got = append(got, page...)
			after = page[len(page)-1]
		}
		if !reflect.DeepEqual(got, thread) {
			t.Errorf("limit %d: pages = %v, want %v", limit, got, thread)
		}
	}
	if got, _ := threadIDs(t, f.s, f.ch.ID, reader, wide, wide, 10); !reflect.DeepEqual(got, wideReplies) {
		t.Errorf("after the root: %v, want just the replies %v", got, wideReplies)
	}

	// The refusals are one error: a reply, unknown, another channel, no identity.
	for name, tt := range map[string]struct {
		channel, root int64
		reader        Reader
	}{
		"a reply":       {f.ch.ID, wideReplies[0], reader},
		"unknown":       {f.ch.ID, 9999, reader},
		"other channel": {f.other.ID, wide, reader},
		"v1 reader":     {f.ch.ID, whisperRoot, Reader{f.bob.ID, false}},
		"no identity":   {f.ch.ID, netRoot, Reader{}},
	} {
		if _, err := threadIDs(t, f.s, tt.channel, tt.reader, tt.root, 0, 10); !errors.Is(err, ErrThreadNotFound) {
			t.Errorf("%s: err = %v, want ErrThreadNotFound", name, err)
		}
	}
	if _, err := f.s.ListThreadMessages(ctx, f.ch.ID, reader, wide, 0, 0); err == nil {
		t.Error("a limit of 0 was accepted")
	}
}

// Visibility is applied to every row of the thread in the statement that
// selects it. A reply stored with an audience narrower than its root's (which
// the server does not do, but the table allows) is skipped for a reader who
// cannot see it without filling a page or moving a cursor.
func TestThreadListingFiltersRowsInTheStatement(t *testing.T) {
	f := newAudienceFixture(t)
	wide := f.wide(t, f.alice, "root")
	var want []int64
	for i := 0; i < 4; i++ {
		m, _ := f.mustReply(t, f.alice, wide, fmt.Sprint("open", i))
		want = append(want, m.ID)
		res, err := f.s.db.Exec(`INSERT INTO messages (channel_id, author_id, body, created_at, audience_kind, thread_root_id)
			VALUES (?, ?, 'hidden', 1, 'principals', ?)`, f.ch.ID, f.alice.ID, wide)
		if err != nil {
			t.Fatal(err)
		}
		hidden, _ := res.LastInsertId()
		if _, err := f.s.db.Exec(`INSERT INTO message_recipients (message_id, principal_id) VALUES (?, ?)`, hidden, f.alice.ID); err != nil {
			t.Fatal(err)
		}
	}
	want = append([]int64{wide}, want...)
	for limit := 1; limit <= 3; limit++ {
		var got []int64
		after := int64(0)
		for pages := 0; pages < 20; pages++ {
			page, err := threadIDs(t, f.s, f.ch.ID, Reader{f.erin.ID, true}, wide, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			if len(page) != limit && len(got)+len(page) != len(want) {
				t.Errorf("limit %d: a short page %v before the end: hidden rows were counted", limit, page)
			}
			got = append(got, page...)
			after = page[len(page)-1]
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("limit %d: erin saw %v, want %v", limit, got, want)
		}
	}
}

// A root that stops being visible between the caller's check and the insert:
// the poster is removed from the channel while the reply is being stored. The
// reply is either stored, because the poster could still see the root when its
// transaction ran, or refused; it is never stored after the removal.
func TestReplyRacingARemovalIsNeverStoredAfterIt(t *testing.T) {
	for _, kind := range []string{"net", "whisper"} {
		t.Run(kind, func(t *testing.T) {
			f := newAudienceFixture(t)
			ctx := context.Background()
			stored, refused := 0, 0
			for i := 0; i < 40; i++ {
				var root int64
				if kind == "net" {
					root, _ = f.toNet(t, f.alice, f.n1, "root")
				} else {
					root = f.whisper(t, f.alice, "root", f.dave)
				}
				var wg sync.WaitGroup
				var replyErr, removeErr error
				var reply Message
				start := make(chan struct{})
				wg.Add(2)
				go func() {
					defer wg.Done()
					<-start
					reply, _, replyErr = f.reply(f.dave, root, "race")
				}()
				go func() {
					defer wg.Done()
					<-start
					_, removeErr = f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.dave.ID)
				}()
				close(start)
				wg.Wait()
				if removeErr != nil {
					t.Fatal(removeErr)
				}
				switch {
				case replyErr == nil:
					stored++
					// Stored: its audit row must precede the removal's.
					if replyAuditID(t, f.s, reply.ID) > removalAuditID(t, f.s) {
						t.Fatalf("iteration %d: the reply was stored after the poster was removed", i)
					}
				case errors.Is(replyErr, ErrNotChannelMember), errors.Is(replyErr, ErrThreadNotFound):
					refused++
				default:
					t.Fatalf("iteration %d: unexpected error %v", i, replyErr)
				}
				// Back for the next round.
				if _, err := f.s.AddChannelMember(ctx, "system", f.ch.ID, f.dave.ID, 0); err != nil {
					t.Fatal(err)
				}
				if kind == "net" {
					f.put(t, "n1", f.dave, schema.NetRoleMember)
				}
			}
			t.Logf("%s: %d replies stored, %d refused", kind, stored, refused)
		})
	}
}

func replyAuditID(t *testing.T, s *Store, messageID int64) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRow(`SELECT MIN(id) FROM audit_events WHERE subject = ?`, fmt.Sprintf("message:%d", messageID)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// removalAuditID is the audit row of the latest removal of principalID from a
// channel.
func removalAuditID(t *testing.T, s *Store) int64 {
	t.Helper()
	var id int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM audit_events WHERE action = ?`, AuditMemberRemoved).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatal("no removal audit row found; the race test cannot order the two writes")
	}
	return id
}

// Migration 17 over a database that already holds messages: every one reads
// back with no thread_root_id, and a reply can be stored afterwards.
func TestThreadMigrationFromSchema16(t *testing.T) {
	const pre = 16
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pre; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'human', 'sam', 'member', 2)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4), (1, 2, NULL, 4)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old', 5)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at, audience_kind) VALUES (2, 1, 1, 'old whisper', 6, 'principals')`,
		`INSERT INTO message_recipients (message_id, principal_id) VALUES (2, 1), (2, 2)`,
		`PRAGMA user_version = 16`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer func() { _ = s.Close() }()
	msgs, err := s.ListVisibleMessages(ctx, 1, Reader{2, true}, 0, 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("list after migration = %+v, %v", msgs, err)
	}
	for _, m := range msgs {
		if m.ThreadRootID != 0 {
			t.Errorf("message %d reads back with thread_root_id %d, want none", m.ID, m.ThreadRootID)
		}
	}
	m, rec, err := s.InsertReplyMessage(ctx, ReplyPost{ChannelID: 1, AuthorID: 2, RootID: 2, Body: "re"}, Reader{2, true})
	if err != nil || m.ThreadRootID != 2 || !reflect.DeepEqual(rec, []int64{1, 2}) {
		t.Fatalf("reply to an old whisper = %+v, %v, %v", m, rec, err)
	}
	if got, err := threadIDs(t, s, 1, Reader{1, true}, 2, 0, 10); err != nil || !reflect.DeepEqual(got, []int64{2, m.ID}) {
		t.Errorf("thread of the old whisper = %v, %v", got, err)
	}
	// A reply cannot name a root that does not exist, even past the API.
	if _, err := s.db.Exec(`INSERT INTO messages (channel_id, author_id, body, created_at, thread_root_id) VALUES (1, 1, 'x', 1, 999)`); err == nil {
		t.Error("the foreign key on thread_root_id is not enforced")
	}
}
