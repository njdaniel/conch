package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Message edits in the store (issue #845, docs/design/message-editing.md §8,
// §14.1): the migration and its triggers, the window, the limit, the no-op,
// the edited_at rule, the search index, the refusals and the audit row.

const messageEditsMigration = 38

// editAt edits as who at the given time.
func (f *audienceFixture) editAt(who Principal, msg int64, body string, at time.Time) (MessageEdit, error) {
	return f.s.EditOwnMessage(context.Background(), f.ch.ID, rdr(who), msg, body, func() time.Time { return at })
}

// createdAt is message msg's created_at as alice reads it.
func (f *audienceFixture) createdAt(t *testing.T, msg int64) time.Time {
	t.Helper()
	var ms int64
	if err := f.s.db.QueryRow("SELECT created_at FROM messages WHERE id = ?", msg).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	return time.UnixMilli(ms)
}

func (f *audienceFixture) revisions(t *testing.T, who Principal, msg int64) []MessageRevision {
	t.Helper()
	revs, err := f.s.OwnMessageRevisions(context.Background(), f.ch.ID, rdr(who), msg)
	if err != nil {
		t.Fatalf("OwnMessageRevisions(%d): %v", msg, err)
	}
	return revs
}

// revisionsDoc is revs as the wire answer, validated: a store answer the
// schema refuses would be a server bug.
func revisionsDoc(t *testing.T, channelID, msg int64, revs []MessageRevision) schema.ListMessageRevisionsResponseV1 {
	t.Helper()
	doc := schema.ListMessageRevisionsResponseV1{ChannelID: channelID, MessageID: msg}
	for _, r := range revs {
		doc.Revisions = append(doc.Revisions, schema.MessageRevisionV1{Revision: r.Revision, Body: r.Body, CreatedAt: schema.NewTimestamp(r.CreatedAt)})
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("revisions %+v are not a valid answer: %v", revs, err)
	}
	return doc
}

// A database from the migration before this one opens, its messages read as
// before (with no edited_at), and they can be edited.
func TestMessageEditsMigration(t *testing.T) {
	const pre = messageEditsMigration - 1
	if len(migrations) < messageEditsMigration {
		t.Fatalf("%d migrations, the edits migration is %d", len(migrations), messageEditsMigration)
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := range pre {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
		if step := migrationSteps[i+1]; step != nil {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := step(ctx, tx); err != nil {
				t.Fatalf("migration step %d: %v", i+1, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := time.Now().UnixMilli()
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, name_key, name_skel, role, created_at) VALUES (1, 'human', 'nick', 'nick', 'nick', 'operator', 1), (2, 'human', 'sam', 'sam', 'sam', 'member', 2)`,
		`INSERT INTO channels (id, name, name_key, name_skel, created_at) VALUES (1, 'general', 'general', 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4), (1, 2, NULL, 4)`,
		fmt.Sprintf(`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 2, 'old message', %d)`, now),
		fmt.Sprintf(`PRAGMA user_version = %d`, pre),
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
	sam := Reader{PrincipalID: 2, Scoped: true}
	m, err := s.ListVisibleMessages(ctx, 1, sam, 0, 10)
	if err != nil || len(m) != 1 || m[0].Body != "old message" || !m[0].EditedAt.IsZero() {
		t.Fatalf("the old message after the migration = %+v, %v", m, err)
	}
	if n := countRows(t, s, "message_revisions"); n != 0 {
		t.Errorf("%d revisions after the migration, want none", n)
	}
	e, err := s.EditOwnMessage(ctx, 1, sam, 1, "old message, fixed", time.Now)
	if err != nil || !e.Changed || e.Revision != 2 {
		t.Fatalf("an edit of a message from before the migration = %+v, %v", e, err)
	}
	if hits, err := s.SearchVisibleMessages(ctx, 1, sam, []string{"fixed"}, 0, 10); err != nil || len(hits) != 1 {
		t.Errorf("search after editing a migrated message = %v, %v", hits, err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
}

// The triggers hold the store to the design: only body and edited_at of a
// message change, edited_at only forward, and revisions are append-only.
func TestMessageEditTriggers(t *testing.T) {
	f := newAudienceFixture(t)
	msg := f.wide(t, f.alice, "protected")
	root := f.wide(t, f.alice, "another")
	created := f.createdAt(t, msg)
	if _, err := f.editAt(f.alice, msg, "protected, edited", created.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, stmt string
		args       []any
		want       string
	}{
		{"id", "UPDATE messages SET id = 999999 WHERE id = ?", []any{msg}, "only a message body"},
		// The rowid alias changes id without naming the column (security review).
		{"rowid", "UPDATE messages SET rowid = 999999 WHERE id = ?", []any{msg}, "only a message body"},
		{"channel_id", "UPDATE messages SET channel_id = ? WHERE id = ?", []any{f.other.ID, msg}, "only a message body"},
		{"author_id", "UPDATE messages SET author_id = ? WHERE id = ?", []any{f.bob.ID, msg}, "only a message body"},
		{"created_at", "UPDATE messages SET created_at = 1 WHERE id = ?", []any{msg}, "only a message body"},
		{"payload_schema", "UPDATE messages SET payload_schema = 'conch.approval.v1' WHERE id = ?", []any{msg}, "only a message body"},
		{"payload_json", "UPDATE messages SET payload_json = '{}' WHERE id = ?", []any{msg}, "only a message body"},
		{"audience_kind", "UPDATE messages SET audience_kind = 'principals' WHERE id = ?", []any{msg}, "only a message body"},
		{"net_id", "UPDATE messages SET net_id = ? WHERE id = ?", []any{f.n1.ID, msg}, "only a message body"},
		{"thread_root_id", "UPDATE messages SET thread_root_id = ? WHERE id = ?", []any{root, msg}, "only a message body"},
		{"edited_at back", "UPDATE messages SET edited_at = edited_at - 1 WHERE id = ?", []any{msg}, "edited_at must move forward"},
		{"edited_at same", "UPDATE messages SET edited_at = edited_at WHERE id = ?", []any{msg}, "edited_at must move forward"},
		{"edited_at cleared", "UPDATE messages SET edited_at = NULL WHERE id = ?", []any{msg}, "edited_at must move forward"},
		{"edited_at at created_at", "UPDATE messages SET edited_at = created_at WHERE id = ?", []any{root}, "edited_at must move forward"},
		{"revision updated", "UPDATE message_revisions SET body = 'x' WHERE message_id = ?", []any{msg}, "append-only"},
		{"revision deleted", "DELETE FROM message_revisions WHERE message_id = ?", []any{msg}, "append-only"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.s.db.Exec(tt.stmt, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("%s = %v, want a refusal %q", tt.stmt, err, tt.want)
			}
		})
	}
	if got := f.revisions(t, f.alice, msg); len(got) != 2 || got[0].Body != "protected" || got[1].Body != "protected, edited" {
		t.Errorf("revisions after the refused statements = %+v", got)
	}
}

// The window is one hour from created_at on the server's clock, the hour
// itself included.
func TestEditOwnMessageWindow(t *testing.T) {
	window := schema.MessageEditWindowSeconds * time.Second
	for _, tt := range []struct {
		name  string
		after time.Duration
		want  error
	}{
		{"at once", 0, nil},
		{"one millisecond", time.Millisecond, nil},
		{"exactly the hour", window, nil},
		{"the hour and a millisecond", window + time.Millisecond, ErrMessageEditWindowClosed},
		{"a day", 24 * time.Hour, ErrMessageEditWindowClosed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newAudienceFixture(t)
			msg := f.wide(t, f.alice, "window")
			e, err := f.editAt(f.alice, msg, "window, edited", f.createdAt(t, msg).Add(tt.after))
			if !errors.Is(err, tt.want) {
				t.Fatalf("edit after %v = %+v, %v; want %v", tt.after, e, err, tt.want)
			}
			wantRows := 2
			if tt.want != nil {
				wantRows = 0
			}
			if n := countRows(t, f.s, "message_revisions"); n != wantRows {
				t.Errorf("%d revisions, want %d", n, wantRows)
			}
		})
	}
}

// Ten edits, then the limit: eleven revisions, revision 1 written by the first
// edit only, one audit row per edit with the revision it made.
func TestEditOwnMessageLimit(t *testing.T) {
	f := newAudienceFixture(t)
	msg := f.wide(t, f.alice, "v1")
	at := f.createdAt(t, msg)
	if n := countRows(t, f.s, "message_revisions"); n != 0 {
		t.Fatalf("a message never edited has %d revisions", n)
	}
	for i := 1; i <= schema.MaxMessageEdits; i++ {
		at = at.Add(time.Second)
		e, err := f.editAt(f.alice, msg, fmt.Sprintf("v%d", i+1), at)
		if err != nil || !e.Changed || e.Revision != i+1 {
			t.Fatalf("edit %d = %+v, %v", i, e, err)
		}
		if n := countRows(t, f.s, "message_revisions"); n != i+1 {
			t.Errorf("after edit %d: %d revisions, want %d", i, n, i+1)
		}
	}
	if _, err := f.editAt(f.alice, msg, "one too many", at.Add(time.Second)); !errors.Is(err, ErrMessageEditLimitReached) {
		t.Fatalf("edit %d = %v, want the limit", schema.MaxMessageEdits+1, err)
	}
	revs := f.revisions(t, f.alice, msg)
	doc := revisionsDoc(t, f.ch.ID, msg, revs)
	if len(doc.Revisions) != schema.MaxMessageEdits+1 || doc.Revisions[0].Body != "v1" || doc.Revisions[10].Body != "v11" {
		t.Errorf("revisions = %+v", revs)
	}
	var details []string
	evs, err := f.s.ListAuditEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Action == AuditMessageEdited {
			details = append(details, e.Detail)
			if e.Subject != fmt.Sprintf("message:%d", msg) || e.Actor != principalActor(f.alice.ID) {
				t.Errorf("audit row %+v", e)
			}
		}
		if strings.Contains(e.Detail, "v1") || strings.Contains(e.Subject, "v1") || strings.Contains(e.Detail, "too many") {
			t.Errorf("an audit row holds message text: %+v", e)
		}
	}
	if len(details) != schema.MaxMessageEdits {
		t.Fatalf("%d message_edited rows, want %d: %v", len(details), schema.MaxMessageEdits, details)
	}
	for i, d := range details {
		if want := fmt.Sprintf("channel=%d revision=%d", f.ch.ID, i+2); d != want {
			t.Errorf("audit detail %d = %q, want %q", i, d, want)
		}
	}
	// The message holds the last text and the last edit's time.
	m := visibleByID(t, f.s, f.ch.ID, rdr(f.bob), msg)
	if m.Body != "v11" || !m.EditedAt.Equal(revs[10].CreatedAt) {
		t.Errorf("message after the edits = %+v", m)
	}
}

// visibleByID reads message id as r does, failing when r cannot.
func visibleByID(t *testing.T, s *Store, channelID int64, r Reader, id int64) Message {
	t.Helper()
	msgs, err := s.ListVisibleMessages(context.Background(), channelID, r, id-1, 1)
	if err != nil || len(msgs) != 1 || msgs[0].ID != id {
		t.Fatalf("read message %d: %+v, %v", id, msgs, err)
	}
	return msgs[0]
}

// A body equal to the current text writes nothing, before the window and the
// limit are asked: a retried PUT whose answer was lost succeeds late.
func TestEditOwnMessageNoOp(t *testing.T) {
	f := newAudienceFixture(t)
	msg := f.wide(t, f.alice, "same")
	created := f.createdAt(t, msg)
	late := created.Add(2 * time.Hour)
	for _, tt := range []struct {
		name string
		at   time.Time
	}{{"in the window", created.Add(time.Second)}, {"after the window", late}} {
		e, err := f.editAt(f.alice, msg, "same", tt.at)
		if err != nil || e.Changed || e.Revision != 0 || e.Message.Body != "same" || !e.Message.EditedAt.IsZero() {
			t.Errorf("%s: no-op = %+v, %v", tt.name, e, err)
		}
	}
	if n := countRows(t, f.s, "message_revisions"); n != 0 {
		t.Errorf("a no-op wrote %d revisions", n)
	}
	if n := auditCount(t, f.s, AuditMessageEdited); n != 0 {
		t.Errorf("a no-op wrote %d audit rows", n)
	}
	// After an edit, repeating it is a no-op too: edited_at does not move.
	e1, err := f.editAt(f.alice, msg, "changed", created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	e2, err := f.editAt(f.alice, msg, "changed", late)
	if err != nil || e2.Changed || !e2.Message.EditedAt.Equal(e1.Message.EditedAt) {
		t.Errorf("repeating an edit = %+v, %v; first %+v", e2, err, e1)
	}
	// A difference in white space alone is a change: the text is stored as
	// sent.
	if e3, err := f.editAt(f.alice, msg, "changed ", created.Add(2*time.Second)); err != nil || !e3.Changed {
		t.Errorf("a trailing space = %+v, %v", e3, err)
	}
}

// edited_at is strictly after created_at and strictly increasing per message,
// for two edits in one millisecond and for a clock that stepped back.
func TestEditedAtIsMonotonic(t *testing.T) {
	f := newAudienceFixture(t)
	msg := f.wide(t, f.alice, "t0")
	created := f.createdAt(t, msg)
	steps := []struct {
		name string
		at   time.Time
	}{
		{"at created_at", created},
		{"the same millisecond", created},
		{"later", created.Add(time.Minute)},
		{"the clock stepped back", created.Add(time.Second)},
		{"back before the post", created.Add(-time.Hour)},
	}
	prev := created
	for i, st := range steps {
		e, err := f.editAt(f.alice, msg, fmt.Sprintf("t%d", i+1), st.at)
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if !e.Message.EditedAt.After(prev) {
			t.Errorf("%s: edited_at %v is not after %v", st.name, e.Message.EditedAt, prev)
		}
		v2 := schema.MessageV2{Schema: schema.MessageSchemaV2, ID: msg, ChannelID: f.ch.ID, AuthorID: f.alice.ID,
			Body: e.Message.Body, CreatedAt: schema.NewTimestamp(e.Message.CreatedAt)}
		at := schema.NewTimestamp(e.Message.EditedAt)
		v2.EditedAt = &at
		if err := v2.Validate(); err != nil {
			t.Errorf("%s: the edited message is not a valid MessageV2: %v", st.name, err)
		}
		prev = e.Message.EditedAt
	}
	revisionsDoc(t, f.ch.ID, msg, f.revisions(t, f.alice, msg))
}

// The index holds the current text only: the old word stops matching, the new
// one matches, other messages are untouched, and FTS5 finds the index sound
// after many edits.
func TestEditOwnMessageSearch(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	msg := f.wide(t, f.alice, "the quick brownfox")
	other := f.wide(t, f.bob, "another brownfox here")
	whisper := f.whisper(t, f.alice, "whispered brownfox", f.erin)
	search := func(r Reader, word string) []int64 {
		t.Helper()
		hits, err := f.s.SearchVisibleMessages(ctx, f.ch.ID, r, []string{word}, 0, 50)
		if err != nil {
			t.Fatal(err)
		}
		ids := []int64{}
		for _, m := range hits {
			ids = append(ids, m.ID)
		}
		return ids
	}
	at := f.createdAt(t, msg)
	if _, err := f.editAt(f.alice, msg, "the quick redfox", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := search(rdr(f.bob), "brownfox"); fmt.Sprint(got) != fmt.Sprint([]int64{other}) {
		t.Errorf("the old word finds %v, want only the other message %d", got, other)
	}
	if got := search(rdr(f.bob), "redfox"); fmt.Sprint(got) != fmt.Sprint([]int64{msg}) {
		t.Errorf("the new word finds %v, want %d", got, msg)
	}
	if got := search(rdr(f.bob), "quick"); fmt.Sprint(got) != fmt.Sprint([]int64{msg}) {
		t.Errorf("a word in both versions finds %v, want %d", got, msg)
	}
	// A whisper's edit is indexed like any other, and found only by its
	// readers.
	if _, err := f.editAt(f.alice, whisper, "whispered greenfox", f.createdAt(t, whisper).Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := search(rdr(f.erin), "greenfox"); fmt.Sprint(got) != fmt.Sprint([]int64{whisper}) {
		t.Errorf("the whisper's recipient finds %v", got)
	}
	if got := search(rdr(f.bob), "greenfox"); len(got) != 0 {
		t.Errorf("a stranger to the whisper finds %v", got)
	}
	for i := range schema.MaxMessageEdits - 1 {
		at = at.Add(time.Second)
		if _, err := f.editAt(f.alice, msg, fmt.Sprintf("edit number%d of many", i), at.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.db.Exec(`INSERT INTO messages_fts (messages_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
		t.Errorf("FTS5 integrity-check after edits: %v", err)
	}
	if got := search(rdr(f.bob), "number8"); fmt.Sprint(got) != fmt.Sprint([]int64{msg}) {
		t.Errorf("the last text finds %v", got)
	}
	if got := search(rdr(f.bob), "number7"); len(got) != 0 {
		t.Errorf("an earlier text finds %v", got)
	}
}

// Who may edit what: one answer for every message that is not the reader's
// own and visible, and message_not_editable only for the author's own.
func TestEditOwnMessageRefusals(t *testing.T) {
	f := newAudienceFixture(t)
	ctx := context.Background()
	wide := f.wide(t, f.alice, "alice says")
	netMsg, _ := f.toNet(t, f.alice, f.n1, "on the net")
	netLost, _ := f.toNet(t, f.dave, f.n1, "dave on the net")
	whisper := f.whisper(t, f.alice, "to erin", f.erin)
	signal, err := f.s.InsertMessageV1(ctx, f.ch.ID, f.alice.ID, "BUY 10", &schema.Payload{Schema: "leviathan.trade_signal.v1", Data: []byte(`{"side":"buy"}`)})
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := f.s.InsertMessageV1(ctx, f.ch.ID, f.alice.ID, "something typed", &schema.Payload{Schema: "example.unknown.v9", Data: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	otherCh, err := f.s.InsertMessageV1(ctx, f.other.ID, f.alice.ID, "elsewhere", nil)
	if err != nil {
		t.Fatal(err)
	}
	// dave is taken off n1: he can still read his message, may no longer
	// transmit there.
	if _, err := f.s.RemoveNetMember(ctx, "system", f.ch.ID, "n1", f.dave.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, tt := range []struct {
		name   string
		who    Principal
		reader Reader
		msg    int64
		want   error
	}{
		{"the author, channel-wide", f.alice, rdr(f.alice), wide, nil},
		{"the author, on her net", f.alice, rdr(f.alice), netMsg, nil},
		{"the author, her whisper", f.alice, rdr(f.alice), whisper, nil},
		{"another member, a message they can read", f.bob, rdr(f.bob), wide, ErrMessageNotFound},
		{"the whisper's recipient", f.erin, rdr(f.erin), whisper, ErrMessageNotFound},
		{"a stranger to the whisper", f.bob, rdr(f.bob), whisper, ErrMessageNotFound},
		{"a net member, not the author", f.dave, rdr(f.dave), netMsg, ErrMessageNotFound},
		{"an unknown id", f.alice, rdr(f.alice), 999999, ErrMessageNotFound},
		{"another channel's message, by its author", f.alice, rdr(f.alice), otherCh.ID, ErrMessageNotFound},
		{"a reader that cannot express an audience", f.alice, Reader{PrincipalID: f.alice.ID}, wide, ErrNotChannelMember},
		{"a non-member", f.carol, rdr(f.carol), wide, ErrNotChannelMember},
		{"a typed signal", f.alice, rdr(f.alice), signal.ID, ErrMessageNotEditable},
		{"an unknown payload", f.alice, rdr(f.alice), unknown.ID, ErrMessageNotEditable},
		{"the author taken off the net", f.dave, rdr(f.dave), netLost, ErrMessageNotEditable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := countRows(t, f.s, "message_revisions")
			_, err := f.s.EditOwnMessage(ctx, f.ch.ID, tt.reader, tt.msg, "new text "+tt.name, func() time.Time { return now })
			if !errors.Is(err, tt.want) {
				t.Fatalf("edit = %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				if n := countRows(t, f.s, "message_revisions"); n != before {
					t.Errorf("a refusal wrote %d revisions", n-before)
				}
				// The revision read refuses the same way, except that it
				// has no notion of editable.
				_, rerr := f.s.OwnMessageRevisions(ctx, f.ch.ID, tt.reader, tt.msg)
				if !errors.Is(tt.want, ErrMessageNotEditable) && !errors.Is(rerr, tt.want) {
					t.Errorf("revisions = %v, want %v", rerr, tt.want)
				}
			}
		})
	}
	// A monitor's message: bob is a monitor and could never have posted to
	// n1, so the case is a seat demoted after the post.
	alicePost, _ := f.toNet(t, f.alice, f.n1, "before demotion")
	f.put(t, "n1", f.alice, schema.NetRoleMonitor)
	if _, err := f.s.EditOwnMessage(ctx, f.ch.ID, rdr(f.alice), alicePost, "after demotion", time.Now); !errors.Is(err, ErrMessageNotEditable) {
		t.Errorf("a monitor's edit of her net message = %v", err)
	}
	// The author removed from the channel is a stranger.
	if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EditOwnMessage(ctx, f.ch.ID, rdr(f.alice), wide, "gone", time.Now); !errors.Is(err, ErrNotChannelMember) {
		t.Errorf("a removed author = %v", err)
	}
	if _, err := f.s.OwnMessageRevisions(ctx, f.ch.ID, rdr(f.alice), wide); !errors.Is(err, ErrNotChannelMember) {
		t.Errorf("a removed author's revisions = %v", err)
	}
}

// What an edit leaves alone: the payload and its files, the audience and
// recipients, the thread links, the guest attribution and reactions.
func TestEditOwnMessageKeepsEverythingElse(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	alice := rdr(f.alice)
	rowOf := func(id int64) string {
		t.Helper()
		var row string
		if err := f.s.db.QueryRow(`SELECT json_array(id, channel_id, author_id, created_at, payload_schema, quote(payload_json), audience_kind, net_id, thread_root_id,
			(SELECT group_concat(principal_id) FROM message_recipients WHERE message_id = m.id),
			(SELECT group_concat(id || ':' || COALESCE(message_id, '')) FROM files WHERE message_id = m.id),
			(SELECT group_concat(message_id || ':' || principal_id || ':' || emoji) FROM message_reactions WHERE message_id = m.id),
			(SELECT handle || ':' || label FROM message_guest_attribution WHERE message_id = m.id),
			(SELECT group_concat(id) FROM messages r WHERE r.thread_root_id = m.id))
			FROM messages m WHERE id = ?`, id).Scan(&row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	if _, err := f.s.SetReaction(ctx, f.ch.ID, rdr(f.dave), f.postFileMsg, rkey(t, thumbs), reactionNow); err != nil {
		t.Fatal(err)
	}
	guest := Reader{PrincipalID: f.guest.Principal.ID, Scoped: true}
	for _, tt := range []struct {
		name   string
		reader Reader
		msg    int64
	}{
		{"a file attachment", alice, f.postFileMsg},
		{"a thread root", Reader{PrincipalID: f.frank.ID, Scoped: true}, f.postRoot},
		{"a reply", alice, f.postReply},
		{"a net message", alice, f.postNet},
		{"a whisper", Reader{PrincipalID: f.erin.ID, Scoped: true}, f.postWhisper},
		{"a guest's post", guest, f.guestPost},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := rowOf(tt.msg)
			beforeMsg := visibleByID(t, f.s, f.ch.ID, tt.reader, tt.msg)
			e, err := f.s.EditOwnMessage(ctx, f.ch.ID, tt.reader, tt.msg, "edited: "+tt.name, time.Now)
			if err != nil || !e.Changed {
				t.Fatalf("edit = %+v, %v", e, err)
			}
			if after := rowOf(tt.msg); after != before {
				t.Errorf("the row changed beyond its text:\n before %s\n after  %s", before, after)
			}
			got := visibleByID(t, f.s, f.ch.ID, tt.reader, tt.msg)
			if got.Body != "edited: "+tt.name || got.EditedAt.IsZero() {
				t.Errorf("read back %+v", got)
			}
			got.Body, got.EditedAt = beforeMsg.Body, beforeMsg.EditedAt
			if !reflect.DeepEqual(got, beforeMsg) {
				t.Errorf("the projection changed beyond its text:\n before %+v\n after  %+v", beforeMsg, got)
			}
			if answered := e.Message; answered.Body != "edited: "+tt.name || answered.EditedAt.IsZero() ||
				!reflect.DeepEqual(answered.Audience, beforeMsg.Audience) || !reflect.DeepEqual(answered.GuestAuthor, beforeMsg.GuestAuthor) ||
				!reflect.DeepEqual(answered.Payload, beforeMsg.Payload) || answered.ThreadRootID != beforeMsg.ThreadRootID {
				t.Errorf("the edit's answer lost the audience or attribution: %+v", e.Message)
			}
			if (tt.msg == f.postNet || tt.msg == f.postWhisper) && len(e.Recipients) == 0 {
				t.Errorf("a scoped message's edit answered no recipients")
			}
			if tt.msg != f.postNet && tt.msg != f.postWhisper && e.Recipients != nil {
				t.Errorf("a channel-wide message's edit answered recipients %v", e.Recipients)
			}
		})
	}
	// The guest's message is still from before nobody's bound; the history
	// bound still hides the old thread from the guest after its root is
	// edited.
	if _, err := f.s.EditOwnMessage(ctx, f.ch.ID, Reader{PrincipalID: f.frank.ID, Scoped: true}, f.preRoot, "old root, edited", time.Now); !errors.Is(err, ErrMessageEditWindowClosed) && err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EditOwnMessage(ctx, f.ch.ID, guest, f.preWide, "x", time.Now); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("the guest editing a message from before her visit = %v", err)
	}
	if _, err := f.s.OwnMessageRevisions(ctx, f.ch.ID, guest, f.preWide); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("the guest's revisions of a message from before her visit = %v", err)
	}
	if ids := visibleIDs(t, f.s, f.ch.ID, guest); containsID(ids, f.preRoot) || containsID(ids, f.preWide) {
		t.Errorf("the guest sees %v, which holds a message from before her visit", ids)
	}
}

func containsID(ids []int64, id int64) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// The revision list: one revision for a message never edited, every version
// after edits, and only for the author.
func TestOwnMessageRevisions(t *testing.T) {
	f := newAudienceFixture(t)
	msg := f.wide(t, f.alice, "as posted")
	created := f.createdAt(t, msg)
	revs := f.revisions(t, f.alice, msg)
	if len(revs) != 1 || revs[0].Revision != 1 || revs[0].Body != "as posted" || !revs[0].CreatedAt.Equal(created) {
		t.Fatalf("an unedited message's revisions = %+v", revs)
	}
	revisionsDoc(t, f.ch.ID, msg, revs)
	if _, err := f.editAt(f.alice, msg, "second", created.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.editAt(f.alice, msg, "third", created.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	revs = f.revisions(t, f.alice, msg)
	doc := revisionsDoc(t, f.ch.ID, msg, revs)
	if len(doc.Revisions) != 3 || doc.Revisions[0].Body != "as posted" || doc.Revisions[1].Body != "second" || doc.Revisions[2].Body != "third" ||
		!revs[0].CreatedAt.Equal(created) || !revs[1].CreatedAt.Equal(created.Add(time.Minute)) {
		t.Errorf("revisions = %+v", revs)
	}
	// After the window the author still reads them.
	if _, err := f.editAt(f.alice, msg, "fourth", created.Add(2*time.Hour)); !errors.Is(err, ErrMessageEditWindowClosed) {
		t.Fatalf("a late edit = %v", err)
	}
	if got := f.revisions(t, f.alice, msg); len(got) != 3 {
		t.Errorf("revisions after the window = %+v", got)
	}
	if _, err := f.s.OwnMessageRevisions(context.Background(), f.ch.ID, rdr(f.bob), msg); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("another reader's revisions = %v", err)
	}
}
