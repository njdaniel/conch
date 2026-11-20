package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// dmFixture is a store with four people, an agent and a clock.
type dmFixture struct {
	s                       *Store
	alice, bob, carol, dave Principal
	agent                   Principal
	now                     time.Time
}

func newDMFixture(t *testing.T) *dmFixture {
	t.Helper()
	ctx := context.Background()
	f := &dmFixture{s: openTestStore(t), now: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
	for _, p := range []struct {
		name string
		kind PrincipalKind
		dst  *Principal
	}{
		{"alice", PrincipalHuman, &f.alice}, {"bob", PrincipalHuman, &f.bob},
		{"carol", PrincipalHuman, &f.carol}, {"dave", PrincipalHuman, &f.dave},
		{"robot", PrincipalAgent, &f.agent},
	} {
		var err error
		if *p.dst, err = f.s.CreatePrincipal(ctx, p.kind, p.name); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// tick advances the fixture's clock and returns it.
func (f *dmFixture) tick() time.Time {
	f.now = f.now.Add(time.Second)
	return f.now
}

func (f *dmFixture) open(t *testing.T, caller, peer Principal) DMConversation {
	t.Helper()
	c, err := f.s.OpenDM(context.Background(), caller.ID, peer.ID, f.tick())
	if err != nil {
		t.Fatalf("OpenDM(%s, %s): %v", caller.Name, peer.Name, err)
	}
	return c
}

// openPair opens the pair from both sides, as two people who both went to the
// conversation (an opened pair is the opener's alone until it has a message).
func (f *dmFixture) openPair(t *testing.T, a, b Principal) DMConversation {
	t.Helper()
	c := f.open(t, a, b)
	if again := f.open(t, b, a); again.ID != c.ID {
		t.Fatalf("the two sides opened conversations %d and %d", c.ID, again.ID)
	}
	return c
}

var dmNonce int

// send writes a message and fails the test on error.
func (f *dmFixture) send(t *testing.T, c DMConversation, from Principal, body string, files ...int64) DMMessage {
	t.Helper()
	dmNonce++
	m, replayed, err := f.s.InsertDMMessage(context.Background(),
		DMPost{ConversationID: c.ID, AuthorID: from.ID, Body: body, FileIDs: files, ClientNonce: fmt.Sprintf("n%d", dmNonce)}, f.tick())
	if err != nil || replayed {
		t.Fatalf("send %q: replayed=%v err=%v", body, replayed, err)
	}
	return m
}

func (f *dmFixture) auditCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM audit_events").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOpenDMRules(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	if _, err := f.s.DisablePrincipal(ctx, "system", f.dave.ID); err != nil {
		t.Fatal(err)
	}
	// A guest is a human with an active visit (migration 31).
	guest := f.carol
	if _, err := f.s.db.Exec(`INSERT INTO guest_visits (principal_id, handle, origin, state, started_at, expires_at, idle_ms)
		VALUES (?, 'g-1', 'address', 'active', 1, 2, 1000)`, guest.ID); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name         string
		caller, peer int64
		want         error
	}{
		{"two people", f.alice.ID, f.bob.ID, nil},
		{"the same pair, the other way", f.bob.ID, f.alice.ID, nil},
		{"self", f.alice.ID, f.alice.ID, ErrDMSelf},
		{"an agent peer", f.alice.ID, f.agent.ID, ErrDMNotHuman},
		{"an agent caller", f.agent.ID, f.alice.ID, ErrDMNoStanding},
		{"no such peer", f.alice.ID, 9999, ErrPrincipalNotFound},
		{"no peer at all", f.alice.ID, 0, ErrPrincipalNotFound},
		{"no caller", 0, f.alice.ID, ErrDMNoStanding},
		{"a disabled caller", f.dave.ID, f.alice.ID, ErrDMNoStanding},
		{"a disabled peer, new pair", f.alice.ID, f.dave.ID, ErrDMContactUnavailable},
		{"a guest caller", guest.ID, f.alice.ID, ErrDMNoStanding},
		{"a guest peer, new pair", f.alice.ID, guest.ID, ErrDMContactUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := f.auditCount(t)
			conv, err := f.s.OpenDM(ctx, c.caller, c.peer, f.tick())
			if !errors.Is(err, c.want) {
				t.Fatalf("OpenDM = %v, want %v", err, c.want)
			}
			if c.want == nil && (conv.ID == 0 || conv.PeerID != c.peer) {
				t.Errorf("conversation %+v, want one with peer %d", conv, c.peer)
			}
			if c.want != nil && conv.ID != 0 {
				t.Errorf("a refused open returned %+v", conv)
			}
			if after := f.auditCount(t); after != before {
				t.Errorf("open wrote %d audit rows; no DM act writes one", after-before)
			}
		})
	}
	// Refusals store nothing: only the alice/bob pair exists.
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM dm_conversations").Scan(&n); err != nil || n != 1 {
		t.Errorf("%d conversations (%v), want exactly the one pair opened", n, err)
	}
}

func TestOpenDMOpposedConcurrentOpensMakeOneConversation(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	const each = 8
	var wg sync.WaitGroup
	type res struct {
		c       DMConversation
		err     error
	}
	results := make(chan res, 2*each)
	start := make(chan struct{})
	for i := 0; i < each; i++ {
		for _, pair := range [][2]int64{{f.alice.ID, f.bob.ID}, {f.bob.ID, f.alice.ID}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				c, err := f.s.OpenDM(ctx, pair[0], pair[1], time.Now())
				results <- res{c, err}
			}()
		}
	}
	close(start)
	wg.Wait()
	close(results)
	ids := map[int64]int{}
	for r := range results {
		if r.err != nil {
			t.Fatalf("OpenDM: %v", r.err)
		}
		ids[r.c.ID]++
	}
	if len(ids) != 1 {
		t.Errorf("ids = %v; want one id from every open", ids)
	}
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM dm_conversations").Scan(&n); err != nil || n != 1 {
		t.Errorf("%d conversation rows (%v), want 1", n, err)
	}
}

func TestOpenDMRollbackThenRetry(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	boom := errors.New("boom")
	// The open succeeds inside a transaction that then fails: nothing stays.
	err := f.s.withImmediateTx(ctx, func(tx execer) error {
		c, err := openDMTx(ctx, tx, f.alice.ID, f.bob.ID, f.tick())
		if err != nil || c.ID == 0 {
			t.Fatalf("openDMTx = %+v, %v", c, err)
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("transaction = %v, want boom", err)
	}
	for _, table := range []string{"dm_conversations", "dm_viewer_state"} {
		var n int
		if err := f.s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s has %d rows (%v) after a rolled-back open", table, n, err)
		}
	}
	// A cancelled context is the same: it stores nothing and a retry works.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.s.OpenDM(cancelled, f.alice.ID, f.bob.ID, f.tick()); err == nil {
		t.Fatal("OpenDM with a cancelled context succeeded")
	}
	c, err := f.s.OpenDM(ctx, f.bob.ID, f.alice.ID, f.tick())
	if err != nil || c.ID == 0 {
		t.Fatalf("retry = %+v, %v; want a new conversation", c, err)
	}
	again, err := f.s.OpenDM(ctx, f.alice.ID, f.bob.ID, f.tick())
	if err != nil || again.ID != c.ID {
		t.Errorf("second open = %+v, %v; want id %d resumed", again, err, c.ID)
	}
}

func TestDMTablesRefuseWhatTheRulesForbid(t *testing.T) {
	f := newDMFixture(t)
	exec := func(q string, args ...any) error { _, err := f.s.db.Exec(q, args...); return err }
	a, b := f.alice.ID, f.bob.ID
	for _, c := range []struct {
		name string
		sql  string
		args []any
	}{
		{"a conversation with oneself", `INSERT INTO dm_conversations (lo, hi, created_at) VALUES (?, ?, 1)`, []any{a, a}},
		{"an unsorted pair", `INSERT INTO dm_conversations (lo, hi, created_at) VALUES (?, ?, 1)`, []any{b, a}},
		{"an agent in a pair", `INSERT INTO dm_conversations (lo, hi, created_at) VALUES (?, ?, 1)`, []any{a, f.agent.ID}},
		{"a missing principal", `INSERT INTO dm_conversations (lo, hi, created_at) VALUES (?, ?, 1)`, []any{a, 9999}},
	} {
		if err := exec(c.sql, c.args...); err == nil {
			t.Errorf("%s: the insert succeeded", c.name)
		}
	}
	conv := f.openPair(t, f.alice, f.bob)
	if err := exec(`INSERT INTO dm_conversations (lo, hi, created_at) VALUES (?, ?, 1)`, a, b); err == nil {
		t.Error("a second row for one pair was accepted")
	}
	msg := f.send(t, conv, f.alice, "hello")
	for _, c := range []struct {
		name string
		sql  string
		args []any
	}{
		{"moving a pair", `UPDATE dm_conversations SET hi = ? WHERE id = ?`, []any{f.carol.ID, conv.ID}},
		{"deleting a pair", `DELETE FROM dm_conversations WHERE id = ?`, []any{conv.ID}},
		{"a third member writing", `INSERT INTO direct_messages (conversation_id, author_id, body, created_at, client_nonce, revision) VALUES (?, ?, 'x', 1, 'z', 99)`, []any{conv.ID, f.carol.ID}},
		{"a third member's view", `INSERT INTO dm_viewer_state (principal_id, conversation_id, state, updated_at) VALUES (?, ?, 'open', 1)`, []any{f.carol.ID, conv.ID}},
		{"a third member's upload", `INSERT INTO dm_files (conversation_id, uploader_id, filename, media_type, size, sha256, created_at) VALUES (?, ?, 'f', 't', 1, ?, 1)`, []any{conv.ID, f.carol.ID, fmt.Sprintf("%064d", 1)}},
		{"moving a message", `UPDATE direct_messages SET conversation_id = 99 WHERE id = ?`, []any{msg.ID}},
		{"changing an author", `UPDATE direct_messages SET author_id = ? WHERE id = ?`, []any{b, msg.ID}},
		{"deleting a message", `DELETE FROM direct_messages WHERE id = ?`, []any{msg.ID}},
		{"another person's removal", `INSERT INTO direct_message_removals (message_id, conversation_id, removed_by, removed_at) VALUES (?, ?, ?, 1)`, []any{msg.ID, conv.ID, b}},
		{"a state outside the vocabulary", `UPDATE dm_viewer_state SET state = 'gone' WHERE conversation_id = ?`, []any{conv.ID}},
	} {
		if err := exec(c.sql, c.args...); err == nil {
			t.Errorf("%s: the statement succeeded", c.name)
		}
	}
}

func TestDMIDSpacesAreSeparate(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	ch, err := f.s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.s.InsertMessageV1(ctx, ch.ID, f.alice.ID, "channel", nil); err != nil {
			t.Fatal(err)
		}
	}
	ab := f.openPair(t, f.alice, f.bob)
	ac := f.openPair(t, f.alice, f.carol)
	if ab.ID == ac.ID {
		t.Fatalf("two pairs share conversation id %d", ab.ID)
	}
	m1 := f.send(t, ab, f.alice, "to bob")
	m2 := f.send(t, ac, f.alice, "to carol")
	if m1.ID == m2.ID {
		t.Fatalf("two messages share id %d", m1.ID)
	}
	// The DM ids count from their own tables, not after the channel's.
	if m1.ID != 1 || ab.ID != 1 {
		t.Errorf("first conversation %d and message %d, want 1 and 1: own id spaces", ab.ID, m1.ID)
	}
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&n); err != nil || n != 3 {
		t.Errorf("channel messages = %d (%v), want the 3 posted; no DM is in the messages table", n, err)
	}
}

func TestEmptyConversationIsInvisibleToTheRecipient(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.open(t, f.alice, f.bob)
	before := f.auditCount(t)

	// The recipient: nothing by id, nothing in either list, nothing in any
	// read of it, and the same error as for an id that does not exist.
	_, errGone := f.s.DMConversationFor(ctx, f.bob.ID, 9999)
	if !errors.Is(errGone, ErrDMNotFound) {
		t.Fatalf("a missing id = %v", errGone)
	}
	if _, err := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID); !errors.Is(err, errGone) {
		t.Errorf("recipient reads the opened pair: %v, want exactly %v", err, errGone)
	}
	if _, err := f.s.ListDMMessagesPage(ctx, f.bob.ID, conv.ID, MessagePage{}, 10); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("recipient lists messages: %v", err)
	}
	if _, err := f.s.SearchDMMessages(ctx, f.bob.ID, conv.ID, []string{"x"}, 0, 10); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("recipient searches: %v", err)
	}
	if _, err := f.s.MarkDMRead(ctx, f.bob.ID, conv.ID, 1, f.tick()); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("recipient marks read: %v", err)
	}
	if _, err := f.s.SetDMState(ctx, f.bob.ID, conv.ID, schema.DMStateArchived, f.tick()); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("recipient archives: %v", err)
	}
	if _, err := f.s.AddDMFile(ctx, NewDMFile{ConversationID: conv.ID, UploaderID: f.bob.ID, Filename: "a", MediaType: "image/png", SHA256: fmt.Sprintf("%064d", 1)}, f.tick()); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("recipient uploads: %v", err)
	}
	if _, _, err := f.s.InsertDMMessage(ctx, DMPost{ConversationID: conv.ID, AuthorID: f.bob.ID, Body: "x", ClientNonce: "q"}, f.tick()); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("recipient writes by guessing the id: %v", err)
	}
	for _, st := range []schema.DMState{schema.DMStateOpen, schema.DMStateArchived} {
		for _, who := range []Principal{f.alice, f.bob} {
			got, err := f.s.ListDMConversations(ctx, who.ID, st, DMListCursor{}, 10)
			// The opener has it as an entry with no message (and no
			// created_at); the recipient has nothing until the first message.
			want := 0
			if who.ID == f.alice.ID && st == schema.DMStateOpen {
				want = 1
			}
			if err != nil || len(got) != want {
				t.Errorf("%s's %s list = %v, %v; want %d entries", who.Name, st, got, err, want)
			}
		}
	}
	// The opener keeps a handle on it, and a stranger has none.
	if c, err := f.s.DMConversationFor(ctx, f.alice.ID, conv.ID); err != nil || c.ID != conv.ID {
		t.Errorf("opener reads it: %+v, %v", c, err)
	}
	if _, err := f.s.DMConversationFor(ctx, f.carol.ID, conv.ID); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("stranger reads it: %v", err)
	}
	if after := f.auditCount(t); after != before {
		t.Errorf("%d audit rows after opening and probing", after-before)
	}

	// Opening from the other side resumes the same conversation, and is how
	// that side gets a handle on it.
	again, err := f.s.OpenDM(ctx, f.bob.ID, f.alice.ID, f.tick())
	if err != nil || again.ID != conv.ID {
		t.Fatalf("recipient opens = %+v, %v; want the same id resumed", again, err)
	}

	// The first message makes it the recipient's too, with no further step.
	conv2 := f.open(t, f.alice, f.carol)
	f.send(t, conv2, f.alice, "hi carol")
	if c, err := f.s.DMConversationFor(ctx, f.carol.ID, conv2.ID); err != nil || c.Unread != 1 || c.LastMessageID == 0 {
		t.Errorf("recipient after a message: %+v, %v; want it visible with 1 unread", c, err)
	}
	got, err := f.s.ListDMConversations(ctx, f.carol.ID, schema.DMStateOpen, DMListCursor{}, 10)
	if err != nil || len(got) != 1 || got[0].ID != conv2.ID || got[0].PeerID != f.alice.ID {
		t.Errorf("recipient's list = %+v, %v", got, err)
	}
}

func TestDisablingHidesConversationsAndEnablingResumesThem(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	f.send(t, conv, f.alice, "hello")
	if _, err := f.s.DisablePrincipal(ctx, "system", f.bob.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("a disabled participant reads: %v", err)
	}
	// The other side still reads the history, but cannot write to them.
	if m, err := f.s.ListDMMessagesPage(ctx, f.alice.ID, conv.ID, MessagePage{}, 10); err != nil || len(m) != 1 {
		t.Errorf("alice reads = %v, %v", m, err)
	}
	if _, _, err := f.s.InsertDMMessage(ctx, DMPost{ConversationID: conv.ID, AuthorID: f.alice.ID, Body: "x", ClientNonce: "k"}, f.tick()); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("a send to a disabled person = %v", err)
	}
	if c, err := f.s.OpenDM(ctx, f.alice.ID, f.bob.ID, f.tick()); err != nil || c.ID != conv.ID {
		t.Errorf("resuming with a disabled person = %+v, %v; reading stays possible", c, err)
	}
	if _, err := f.s.EnablePrincipal(ctx, "system", f.bob.ID); err != nil {
		t.Fatal(err)
	}
	if m, err := f.s.ListDMMessagesPage(ctx, f.bob.ID, conv.ID, MessagePage{}, 10); err != nil || len(m) != 1 {
		t.Errorf("after enabling, bob reads = %v, %v; history resumes", m, err)
	}
}

func TestInsertAndListDMMessages(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	other := f.openPair(t, f.alice, f.carol)
	var want []int64
	for i := 0; i < 5; i++ {
		who := f.alice
		if i%2 == 1 {
			who = f.bob
		}
		want = append(want, f.send(t, conv, who, fmt.Sprintf("m%d", i)).ID)
	}
	f.send(t, other, f.alice, "elsewhere")
	ids := func(ms []DMMessage) []int64 {
		out := []int64{}
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	for _, c := range []struct {
		name  string
		page  MessagePage
		limit int
		want  []int64
	}{
		{"first page", MessagePage{}, 3, want[:3]},
		{"after", MessagePage{After: want[2]}, 10, want[3:]},
		{"newest first", MessagePage{NewestFirst: true}, 2, []int64{want[4], want[3]}},
		{"before", MessagePage{Before: want[3], NewestFirst: true}, 10, []int64{want[2], want[1], want[0]}},
	} {
		got, err := f.s.ListDMMessagesPage(ctx, f.bob.ID, conv.ID, c.page, c.limit)
		if err != nil || !reflect.DeepEqual(ids(got), c.want) {
			t.Errorf("%s = %v, %v; want %v", c.name, ids(got), err, c.want)
		}
	}
	for _, bad := range []MessagePage{{After: 1, NewestFirst: true}, {Before: 1}, {After: -1}} {
		if _, err := f.s.ListDMMessagesPage(ctx, f.bob.ID, conv.ID, bad, 5); err == nil {
			t.Errorf("page %+v was accepted", bad)
		}
	}
	if _, err := f.s.ListDMMessagesPage(ctx, f.bob.ID, conv.ID, MessagePage{}, 0); err == nil {
		t.Error("a zero limit was accepted")
	}
	// The conversation of another pair is not bob's.
	if _, err := f.s.ListDMMessagesPage(ctx, f.bob.ID, other.ID, MessagePage{}, 5); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("bob lists alice and carol's conversation: %v", err)
	}
	// The nonce shows on its author's copy only.
	mine, _ := f.s.ListDMMessagesPage(ctx, f.alice.ID, conv.ID, MessagePage{}, 1)
	theirs, _ := f.s.ListDMMessagesPage(ctx, f.bob.ID, conv.ID, MessagePage{}, 1)
	if mine[0].ClientNonce == "" || theirs[0].ClientNonce != "" {
		t.Errorf("nonce: author's %q, peer's %q", mine[0].ClientNonce, theirs[0].ClientNonce)
	}
	if c, err := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID); err != nil || c.LastMessageID != want[4] || c.Unread != 3 {
		t.Errorf("bob's conversation = %+v, %v; want last %d and 3 unread (alice wrote 3)", c, err, want[4])
	}
}

func TestInsertDMMessageReplayWindow(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	post := DMPost{ConversationID: conv.ID, AuthorID: f.alice.ID, Body: "once", ClientNonce: "same"}
	first, replayed, err := f.s.InsertDMMessage(ctx, post, f.now)
	if err != nil || replayed {
		t.Fatalf("first = %v, %v", replayed, err)
	}
	for _, c := range []struct {
		name       string
		at         time.Time
		author     int64
		wantReplay bool
	}{
		{"a retry", f.now.Add(time.Second), f.alice.ID, true},
		{"just inside the window", f.now.Add(schema.DMClientNonceWindowSeconds*time.Second - time.Second), f.alice.ID, true},
		{"after the window", f.now.Add(schema.DMClientNonceWindowSeconds*time.Second + time.Second), f.alice.ID, false},
		{"the peer's same nonce is another send", f.now.Add(time.Second), f.bob.ID, false},
	} {
		p := post
		p.AuthorID = c.author
		got, replayed, err := f.s.InsertDMMessage(ctx, p, c.at)
		if err != nil || replayed != c.wantReplay {
			t.Errorf("%s: replayed %v, %v; want %v", c.name, replayed, err, c.wantReplay)
			continue
		}
		if c.wantReplay && got.ID != first.ID {
			t.Errorf("%s: id %d, want the first send's %d", c.name, got.ID, first.ID)
		}
		if !c.wantReplay && got.ID == first.ID {
			t.Errorf("%s: returned the first send", c.name)
		}
	}
}

func TestDMFilesAttachRules(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	other := f.open(t, f.alice, f.carol)
	upload := func(c DMConversation, who Principal, name string) DMFile {
		t.Helper()
		file, err := f.s.AddDMFile(ctx, NewDMFile{ConversationID: c.ID, UploaderID: who.ID, Filename: name, MediaType: "image/png", Size: 3, SHA256: fmt.Sprintf("%064d", 7)}, f.tick())
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	mine1, mine2 := upload(conv, f.alice, "a.png"), upload(conv, f.alice, "b.png")
	bobs := upload(conv, f.bob, "c.png")
	elsewhere := upload(other, f.alice, "d.png")

	// An unattached file is its uploader's alone.
	if _, err := f.s.DMFileVisibleTo(ctx, f.bob.ID, mine1.ID); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("peer reads an unattached upload: %v", err)
	}
	if got, err := f.s.DMFileVisibleTo(ctx, f.alice.ID, mine1.ID); err != nil || got.ID != mine1.ID || got.ConversationID != conv.ID {
		t.Errorf("uploader reads it: %+v, %v", got, err)
	}
	for _, c := range []struct {
		name  string
		files []int64
	}{
		{"the peer's file", []int64{bobs.ID}},
		{"another conversation's file", []int64{elsewhere.ID}},
		{"an unknown file", []int64{9999}},
		{"a file named twice", []int64{mine1.ID, mine1.ID}},
	} {
		_, _, err := f.s.InsertDMMessage(ctx, DMPost{ConversationID: conv.ID, AuthorID: f.alice.ID, Body: "x", FileIDs: c.files, ClientNonce: "r-" + c.name}, f.tick())
		if !errors.Is(err, ErrDMFileNotAttachable) {
			t.Errorf("%s: %v, want ErrDMFileNotAttachable", c.name, err)
		}
	}
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM direct_messages").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d messages after refused sends (%v)", n, err)
	}
	msg := f.send(t, conv, f.alice, "two pictures", mine2.ID, mine1.ID)
	if !reflect.DeepEqual(msg.FileIDs, []int64{mine2.ID, mine1.ID}) {
		t.Errorf("file ids = %v, want the order sent %v", msg.FileIDs, []int64{mine2.ID, mine1.ID})
	}
	// Attached once: a second send of either file is refused.
	if _, _, err := f.s.InsertDMMessage(ctx, DMPost{ConversationID: conv.ID, AuthorID: f.alice.ID, Body: "again", FileIDs: []int64{mine1.ID}, ClientNonce: "again"}, f.tick()); !errors.Is(err, ErrDMFileNotAttachable) {
		t.Errorf("attaching twice: %v", err)
	}
	// Attached, it is the conversation's: the peer reads it; a stranger does not.
	if _, err := f.s.DMFileVisibleTo(ctx, f.bob.ID, mine1.ID); err != nil {
		t.Errorf("peer reads an attached file: %v", err)
	}
	if _, err := f.s.DMFileVisibleTo(ctx, f.carol.ID, mine1.ID); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("stranger reads it: %v", err)
	}
	// Removing the message takes its files with it, for both.
	if _, err := f.s.RemoveDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, f.tick()); err != nil {
		t.Fatal(err)
	}
	for _, who := range []Principal{f.alice, f.bob} {
		if _, err := f.s.DMFileVisibleTo(ctx, who.ID, mine1.ID); !errors.Is(err, ErrDMNotFound) {
			t.Errorf("%s reads a removed message's file: %v", who.Name, err)
		}
	}
}

func TestEditDMMessage(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	msg := f.send(t, conv, f.alice, "first draft")
	posted := msg.CreatedAt

	for _, c := range []struct {
		name   string
		editor int64
		conv   int64
		msg    int64
		body   string
		at     time.Time
		want   error
	}{
		{"the peer", f.bob.ID, conv.ID, msg.ID, "mine now", posted, ErrDMNotAuthor},
		{"a stranger", f.carol.ID, conv.ID, msg.ID, "x", posted, ErrDMNotFound},
		{"an unknown message", f.alice.ID, conv.ID, 9999, "x", posted, ErrMessageNotFound},
		{"after the window", f.alice.ID, conv.ID, msg.ID, "late", posted.Add((schema.MessageEditWindowSeconds + 1) * time.Second), ErrMessageEditWindowClosed},
	} {
		_, err := f.s.EditDMMessage(ctx, c.editor, c.conv, c.msg, c.body, func() time.Time { return c.at })
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	// A clock that stands still still moves edited_at forward.
	clock := func() time.Time { return posted }
	same, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, "first draft", clock)
	if err != nil || same.Changed || same.Revision != 0 {
		t.Fatalf("the same text = %+v, %v", same, err)
	}
	e1, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, "second draft", clock)
	if err != nil || !e1.Changed || e1.Revision != 2 || e1.Message.Body != "second draft" || !e1.Message.EditedAt.After(posted) {
		t.Fatalf("first edit = %+v, %v", e1, err)
	}
	e2, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, "third draft", clock)
	if err != nil || e2.Revision != 3 || !e2.Message.EditedAt.After(e1.Message.EditedAt) {
		t.Fatalf("second edit = %+v, %v; edited_at must keep increasing", e2, err)
	}
	// The peer reads the new text; the old text is kept and no longer matches.
	got, err := f.s.DMMessageFor(ctx, f.bob.ID, conv.ID, msg.ID)
	if err != nil || got.Body != "third draft" || got.EditedAt.IsZero() {
		t.Errorf("peer reads %+v, %v", got, err)
	}
	var revs []string
	rows, err := f.s.db.Query("SELECT body FROM direct_message_revisions WHERE message_id = ? ORDER BY revision", msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		revs = append(revs, b)
	}
	_ = rows.Close()
	if !reflect.DeepEqual(revs, []string{"first draft", "second draft", "third draft"}) {
		t.Errorf("revisions = %q, want the text as posted and every edit", revs)
	}
	if hits, _ := f.s.SearchDMMessages(ctx, f.bob.ID, conv.ID, []string{"first"}, 0, 5); len(hits) != 0 {
		t.Errorf("the replaced text still matches: %v", hits)
	}
	if hits, _ := f.s.SearchDMMessages(ctx, f.bob.ID, conv.ID, []string{"third"}, 0, 5); len(hits) != 1 {
		t.Errorf("the new text does not match: %v", hits)
	}
	// The cap on edits.
	for i := 0; ; i++ {
		_, err = f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, fmt.Sprintf("draft %d", i), clock)
		if err != nil {
			break
		}
		if i > schema.MaxMessageEdits+2 {
			t.Fatal("the edit cap never applied")
		}
	}
	if !errors.Is(err, ErrMessageEditLimitReached) {
		t.Errorf("past the cap = %v, want ErrMessageEditLimitReached", err)
	}
	// A removed message cannot be edited, and nothing about the refusals wrote
	// an audit row.
	if _, err := f.s.RemoveDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, f.tick()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, "back", clock); !errors.Is(err, ErrMessageNotEditable) {
		t.Errorf("editing a removed message = %v", err)
	}
	var audits int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action LIKE '%message%'").Scan(&audits); err != nil || audits != 0 {
		t.Errorf("%d message audit rows (%v); no DM act writes one", audits, err)
	}
}

func TestRemoveDMMessage(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	keep := f.send(t, conv, f.bob, "stays")
	msg := f.send(t, conv, f.alice, "secret plans")
	if _, err := f.s.RemoveDMMessage(ctx, f.bob.ID, conv.ID, msg.ID, f.tick()); !errors.Is(err, ErrDMNotAuthor) {
		t.Errorf("the peer removes: %v; there is no removal of another's message", err)
	}
	if _, err := f.s.RemoveDMMessage(ctx, f.carol.ID, conv.ID, msg.ID, f.tick()); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("a stranger removes: %v", err)
	}
	if _, err := f.s.RemoveDMMessage(ctx, f.alice.ID, conv.ID, 9999, f.tick()); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("an unknown message: %v", err)
	}
	r, err := f.s.RemoveDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, f.tick())
	if err != nil || !r.Changed || !r.Message.Removed {
		t.Fatalf("remove = %+v, %v", r, err)
	}
	if again, err := f.s.RemoveDMMessage(ctx, f.alice.ID, conv.ID, msg.ID, f.tick()); err != nil || again.Changed {
		t.Errorf("removing twice = %+v, %v; want unchanged", again, err)
	}
	for _, who := range []Principal{f.alice, f.bob} {
		list, err := f.s.ListDMMessagesPage(ctx, who.ID, conv.ID, MessagePage{}, 10)
		if err != nil || len(list) != 2 {
			t.Fatalf("%s lists %v, %v; the tombstone keeps its place", who.Name, list, err)
		}
		tomb := list[1]
		if !tomb.Removed || tomb.Body != schema.MessageRemovedByAuthorBody || tomb.ID != msg.ID || tomb.AuthorID != f.alice.ID ||
			!tomb.EditedAt.IsZero() || len(tomb.FileIDs) != 0 {
			t.Errorf("%s reads the tombstone as %+v", who.Name, tomb)
		}
		if hits, _ := f.s.SearchDMMessages(ctx, who.ID, conv.ID, []string{"secret"}, 0, 5); len(hits) != 0 {
			t.Errorf("%s still finds the removed message: %v", who.Name, hits)
		}
	}
	if hits, _ := f.s.SearchDMMessages(ctx, f.alice.ID, conv.ID, []string{"stays"}, 0, 5); len(hits) != 1 || hits[0].ID != keep.ID {
		t.Errorf("the other message stops matching: %v", hits)
	}
	// A removal is permanent: the row cannot be undone or rewritten.
	if _, err := f.s.db.Exec("DELETE FROM direct_message_removals"); err == nil {
		t.Error("a removal could be deleted")
	}
	if _, err := f.s.db.Exec("UPDATE direct_messages SET body = 'x' WHERE id = ?", msg.ID); err == nil {
		t.Error("a removed message's row could be rewritten")
	}
}

func TestMarkDMReadClampsLikeAChannel(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	other := f.openPair(t, f.bob, f.carol)
	m1 := f.send(t, conv, f.alice, "1")
	m2 := f.send(t, conv, f.alice, "2")
	m3 := f.send(t, conv, f.alice, "3")
	own := f.send(t, conv, f.bob, "bob's own")
	foreign := f.send(t, other, f.carol, "elsewhere")
	if c, _ := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID); c.Unread != 3 || c.LastReadID != 0 {
		t.Fatalf("before: %+v, want 3 unread; bob's own post is not unread to him", c)
	}
	mark := func(id int64) DMReadMark {
		t.Helper()
		m, err := f.s.MarkDMRead(ctx, f.bob.ID, conv.ID, id, f.tick())
		if err != nil {
			t.Fatalf("MarkDMRead(%d): %v", id, err)
		}
		return m
	}
	if m := mark(0); m.LastReadID != 0 || !m.UpdatedAt.IsZero() || m.Unread != 3 {
		t.Errorf("a mark of 0 on a never-marked conversation = %+v, want nothing stored", m)
	}
	first := mark(m2.ID)
	if first.LastReadID != m2.ID || first.Unread != 1 {
		t.Errorf("mark %d = %+v, want unread 1", m2.ID, first)
	}
	if back := mark(m1.ID); back.LastReadID != m2.ID || !back.UpdatedAt.Equal(first.UpdatedAt) {
		t.Errorf("a lower mark = %+v, want the mark and its time unchanged (%+v)", back, first)
	}
	if same := mark(m2.ID); !same.UpdatedAt.Equal(first.UpdatedAt) {
		t.Errorf("the same mark moved updated_at: %v -> %v", first.UpdatedAt, same.UpdatedAt)
	}
	// A mark above the newest message, or at another conversation's message,
	// is the newest message here, so it can not probe for ids.
	for _, id := range []int64{foreign.ID + 1000, foreign.ID} {
		if m := mark(id); m.LastReadID != own.ID || m.Unread != 0 {
			t.Errorf("mark %d = %+v, want clamped to %d with nothing unread", id, m, own.ID)
		}
	}
	if m := mark(m3.ID); m.LastReadID != own.ID {
		t.Errorf("a mark below the stored one moved it: %+v", m)
	}
	if _, err := f.s.MarkDMRead(ctx, f.bob.ID, conv.ID, -1, f.tick()); err == nil {
		t.Error("a negative mark was accepted")
	}
	// The peer's mark is not bob's.
	if got, err := f.s.DMReadMarkFor(ctx, f.alice.ID, conv.ID); err != nil || got.LastReadID != 0 || got.Unread != 1 {
		t.Errorf("alice's mark = %+v, %v; want untouched with bob's one message unread", got, err)
	}
	// A removed message is not unread.
	m5 := f.send(t, conv, f.alice, "to be removed")
	if got, _ := f.s.DMReadMarkFor(ctx, f.bob.ID, conv.ID); got.Unread != 1 {
		t.Fatalf("unread = %d, want 1", got.Unread)
	}
	if _, err := f.s.RemoveDMMessage(ctx, f.alice.ID, conv.ID, m5.ID, f.tick()); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.s.DMReadMarkFor(ctx, f.bob.ID, conv.ID); got.Unread != 0 {
		t.Errorf("unread = %d after removal, want 0", got.Unread)
	}
}

func TestDMViewerStateIsPerViewer(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	f.send(t, conv, f.alice, "one")
	inList := func(who Principal, st schema.DMState) bool {
		t.Helper()
		l, err := f.s.ListDMConversations(ctx, who.ID, st, DMListCursor{}, 10)
		if err != nil {
			t.Fatal(err)
		}
		return len(l) == 1
	}
	if !inList(f.alice, schema.DMStateOpen) || !inList(f.bob, schema.DMStateOpen) {
		t.Fatal("both start with it open")
	}
	// Bob archives: his list changes, alice's does not.
	if c, err := f.s.SetDMState(ctx, f.bob.ID, conv.ID, schema.DMStateArchived, f.tick()); err != nil || c.State != schema.DMStateArchived {
		t.Fatalf("archive = %+v, %v", c, err)
	}
	if inList(f.bob, schema.DMStateOpen) || !inList(f.bob, schema.DMStateArchived) || !inList(f.alice, schema.DMStateOpen) {
		t.Error("archiving leaked across viewers or missed the archive list")
	}
	// A new message leaves it archived and unread.
	f.send(t, conv, f.alice, "two")
	if c, _ := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID); c.State != schema.DMStateArchived || c.Unread != 2 {
		t.Errorf("after a message to an archived conversation: %+v", c)
	}
	// Hiding takes it out of every list; reading by id still works; the next
	// message brings it back.
	if _, err := f.s.SetDMState(ctx, f.bob.ID, conv.ID, schema.DMStateHidden, f.tick()); err != nil {
		t.Fatal(err)
	}
	if inList(f.bob, schema.DMStateOpen) || inList(f.bob, schema.DMStateArchived) {
		t.Error("a hidden conversation is in a list")
	}
	if c, err := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID); err != nil || c.State != schema.DMStateHidden {
		t.Errorf("a hidden conversation by id = %+v, %v", c, err)
	}
	f.send(t, conv, f.alice, "three")
	if c, _ := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID); c.State != schema.DMStateOpen {
		t.Errorf("state after a message to a hidden conversation = %q, want open", c.State)
	}
	// Opening the pair again also resumes it.
	if _, err := f.s.SetDMState(ctx, f.bob.ID, conv.ID, schema.DMStateHidden, f.tick()); err != nil {
		t.Fatal(err)
	}
	if c, err := f.s.OpenDM(ctx, f.bob.ID, f.alice.ID, f.tick()); err != nil || c.State != schema.DMStateOpen {
		t.Errorf("open after hide = %+v, %v", c, err)
	}
	if _, err := f.s.SetDMState(ctx, f.bob.ID, conv.ID, "gone", f.tick()); err == nil {
		t.Error("a state outside the vocabulary was accepted")
	}
	if _, err := f.s.ListDMConversations(ctx, f.bob.ID, schema.DMStateHidden, DMListCursor{}, 5); err == nil {
		t.Error("the hidden list exists")
	}
}

func TestListDMConversationsOrderAndPaging(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	ab, ac, ad := f.openPair(t, f.alice, f.bob), f.openPair(t, f.alice, f.carol), f.openPair(t, f.alice, f.dave)
	f.send(t, ab, f.bob, "1")
	f.send(t, ac, f.carol, "2")
	f.send(t, ad, f.dave, "3")
	f.send(t, ab, f.bob, "4") // ab is now the newest
	got, err := f.s.ListDMConversations(ctx, f.alice.ID, schema.DMStateOpen, DMListCursor{}, 2)
	if err != nil || len(got) != 2 || got[0].ID != ab.ID || got[1].ID != ad.ID {
		t.Fatalf("first page = %+v, %v; want ab then ad", got, err)
	}
	next, err := f.s.ListDMConversations(ctx, f.alice.ID, schema.DMStateOpen, got[1].CursorAfter(), 2)
	if err != nil || len(next) != 1 || next[0].ID != ac.ID {
		t.Fatalf("second page = %+v, %v; want ac", next, err)
	}
	if got[0].Unread != 2 || got[0].PeerID != f.bob.ID {
		t.Errorf("row = %+v, want 2 unread from bob", got[0])
	}
}

func TestSearchDMMessagesStaysInTheConversation(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	ab, ac := f.openPair(t, f.alice, f.bob), f.openPair(t, f.alice, f.carol)
	m1 := f.send(t, ab, f.alice, "the launch codes are ready")
	f.send(t, ac, f.alice, "the launch party is friday")
	m3 := f.send(t, ab, f.bob, "launch confirmed")
	hits, err := f.s.SearchDMMessages(ctx, f.bob.ID, ab.ID, []string{"launch"}, 0, 10)
	if err != nil || len(hits) != 2 || hits[0].ID != m3.ID || hits[1].ID != m1.ID {
		t.Fatalf("search = %+v, %v; want only this conversation's, newest first", hits, err)
	}
	page, _ := f.s.SearchDMMessages(ctx, f.bob.ID, ab.ID, []string{"launch"}, m3.ID, 10)
	if len(page) != 1 || page[0].ID != m1.ID {
		t.Errorf("search before = %+v", page)
	}
	if none, err := f.s.SearchDMMessages(ctx, f.bob.ID, ab.ID, []string{"friday"}, 0, 10); err != nil || len(none) != 0 {
		t.Errorf("another conversation's word matched: %+v, %v", none, err)
	}
	// FTS syntax in the words is only words.
	for _, w := range []string{`launch OR friday`, `"`, `*`, `fts_conversation:d2`, ``} {
		got, err := f.s.SearchDMMessages(ctx, f.bob.ID, ab.ID, []string{w}, 0, 10)
		if err != nil {
			t.Errorf("search %q: %v", w, err)
		}
		for _, m := range got {
			if m.ConversationID != ab.ID {
				t.Errorf("search %q returned %+v from another conversation", w, m)
			}
		}
	}
	if _, err := f.s.SearchDMMessages(ctx, f.carol.ID, ab.ID, []string{"launch"}, 0, 10); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("a stranger searches: %v", err)
	}
}
