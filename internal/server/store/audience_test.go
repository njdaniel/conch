package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// audienceFixture is a channel "general" with members alice, bob, dave, erin
// and frank, a non-member carol, and two nets:
//
//	n1: alice (member), bob (monitor), dave (member)
//	n2: alice (member), erin (member)
//
// frank is on no net.
type audienceFixture struct {
	*netFixture
	dave, erin, frank Principal
	n1, n2            Net
}

func newAudienceFixture(t *testing.T) *audienceFixture {
	t.Helper()
	ctx := context.Background()
	f := &audienceFixture{netFixture: newNetFixture(t)}
	for _, p := range []struct {
		name string
		dst  *Principal
	}{{"dave", &f.dave}, {"erin", &f.erin}, {"frank", &f.frank}} {
		var err error
		if *p.dst, err = f.s.CreatePrincipal(ctx, PrincipalHuman, p.name); err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.AddChannelMember(ctx, "system", f.ch.ID, p.dst.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	f.n1 = f.mustNet(t, "n1")
	f.n2 = f.mustNet(t, "n2")
	f.put(t, "n1", f.alice, schema.NetRoleMember)
	f.put(t, "n1", f.bob, schema.NetRoleMonitor)
	f.put(t, "n1", f.dave, schema.NetRoleMember)
	f.put(t, "n2", f.alice, schema.NetRoleMember)
	f.put(t, "n2", f.erin, schema.NetRoleMember)
	return f
}

func (f *audienceFixture) wide(t *testing.T, author Principal, body string) int64 {
	t.Helper()
	m, err := f.s.InsertMessageV1(context.Background(), f.ch.ID, author.ID, body, nil)
	if err != nil {
		t.Fatal(err)
	}
	return m.ID
}

func (f *audienceFixture) toNet(t *testing.T, author Principal, n Net, body string) (int64, []int64) {
	t.Helper()
	m, rec, err := f.s.InsertScopedMessage(context.Background(), ScopedPost{
		ChannelID: f.ch.ID, AuthorID: author.ID, Body: body,
		Audience: schema.Audience{Kind: schema.AudienceKindNet, NetID: n.ID},
	})
	if err != nil {
		t.Fatalf("scoped post to net %d: %v", n.ID, err)
	}
	return m.ID, rec
}

func (f *audienceFixture) whisper(t *testing.T, author Principal, body string, to ...Principal) int64 {
	t.Helper()
	ids := make([]int64, len(to))
	for i, p := range to {
		ids[i] = p.ID
	}
	m, _, err := f.s.InsertScopedMessage(context.Background(), ScopedPost{
		ChannelID: f.ch.ID, AuthorID: author.ID, Body: body,
		Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: ids}.Normalize(author.ID),
	})
	if err != nil {
		t.Fatalf("whisper: %v", err)
	}
	return m.ID
}

func visibleIDs(t *testing.T, s *Store, channelID int64, r Reader) []int64 {
	t.Helper()
	msgs, err := s.ListVisibleMessages(context.Background(), channelID, r, 0, 100)
	if err != nil {
		t.Fatalf("ListVisibleMessages: %v", err)
	}
	ids := []int64{}
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	return ids
}

// The visibility function is the only way to read messages. This is the exact
// set each reader gets from a mix of channel-wide, net, second-net and
// whisper messages interleaved.
func TestListVisibleMessagesExactSets(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	w1 := f.wide(t, f.frank, "wide 1")
	m1, _ := f.toNet(t, f.alice, f.n1, "net one")
	w2 := f.wide(t, f.frank, "wide 2")
	m2, _ := f.toNet(t, f.erin, f.n2, "net two")
	wh := f.whisper(t, f.erin, "whisper", f.dave)
	w3 := f.wide(t, f.frank, "wide 3")

	disabled := f.dave
	removed := f.erin
	// Disable and removal are applied to throwaway copies of the data below;
	// the main table runs on the untouched fixture.
	tests := []struct {
		name   string
		reader Reader
		want   []int64
	}{
		{"author of the net post (v2)", Reader{f.alice.ID, true}, []int64{w1, m1, w2, m2, w3}},
		{"net member (v2)", Reader{f.dave.ID, true}, []int64{w1, m1, w2, wh, w3}},
		{"net monitor (v2)", Reader{f.bob.ID, true}, []int64{w1, m1, w2, w3}},
		{"whisper author (v2)", Reader{f.erin.ID, true}, []int64{w1, w2, m2, wh, w3}},
		{"channel member outside every audience (v2)", Reader{f.frank.ID, true}, []int64{w1, w2, w3}},
		{"non-member (v2)", Reader{f.carol.ID, true}, []int64{w1, w2, w3}},
		{"no verified reader (v2)", Reader{0, true}, []int64{w1, w2, w3}},
		{"net member, v0/v1 reader", Reader{f.dave.ID, false}, []int64{w1, w2, w3}},
		{"whisper author, v0/v1 reader", Reader{f.erin.ID, false}, []int64{w1, w2, w3}},
		{"channel-wide only", ChannelWideOnly, []int64{w1, w2, w3}},
		{"unknown principal id (v2)", Reader{9999, true}, []int64{w1, w2, w3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := visibleIDs(t, f.s, f.ch.ID, tt.reader); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("visible = %v, want %v", got, tt.want)
			}
		})
	}

	// A disabled recipient keeps its rows and seats (re-enabling restores
	// them) but reads nothing scoped while disabled.
	t.Run("disabled recipient", func(t *testing.T) {
		if _, err := f.s.DisablePrincipal(ctx, "system", disabled.ID); err != nil {
			t.Fatal(err)
		}
		want := []int64{w1, w2, w3}
		if got := visibleIDs(t, f.s, f.ch.ID, Reader{disabled.ID, true}); !reflect.DeepEqual(got, want) {
			t.Errorf("disabled recipient sees %v, want %v", got, want)
		}
		if _, err := f.s.EnablePrincipal(ctx, "system", disabled.ID); err != nil {
			t.Fatal(err)
		}
		want = []int64{w1, m1, w2, wh, w3}
		if got := visibleIDs(t, f.s, f.ch.ID, Reader{disabled.ID, true}); !reflect.DeepEqual(got, want) {
			t.Errorf("re-enabled recipient sees %v, want %v", got, want)
		}
	})

	// Removal from the channel hides every scoped message, those received
	// earlier included; channel-wide visibility is the handlers' membership
	// gate, so the store still returns it for a channel-wide-only reader.
	t.Run("removed from the channel", func(t *testing.T) {
		if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, removed.ID); err != nil {
			t.Fatal(err)
		}
		want := []int64{w1, w2, w3}
		if got := visibleIDs(t, f.s, f.ch.ID, Reader{removed.ID, true}); !reflect.DeepEqual(got, want) {
			t.Errorf("removed member sees %v, want %v", got, want)
		}
	})
}

// A message's audience is fixed when it is posted.
func TestScopedSnapshotAtPostTime(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	before, rec := f.toNet(t, f.alice, f.n1, "before frank joined")
	if want := []int64{f.alice.ID, f.bob.ID, f.dave.ID}; !reflect.DeepEqual(rec, want) {
		t.Fatalf("resolved recipients = %v, want %v", rec, want)
	}

	f.put(t, "n1", f.frank, schema.NetRoleMember)
	after, rec := f.toNet(t, f.alice, f.n1, "after frank joined")
	if want := []int64{f.alice.ID, f.bob.ID, f.dave.ID, f.frank.ID}; !reflect.DeepEqual(rec, want) {
		t.Fatalf("recipients after the join = %v, want %v", rec, want)
	}
	// Joining a net reveals no history.
	if got, want := visibleIDs(t, f.s, f.ch.ID, Reader{f.frank.ID, true}), []int64{after}; !reflect.DeepEqual(got, want) {
		t.Errorf("late joiner sees %v, want %v", got, want)
	}

	// Leaving a net hides nothing already received and receives nothing new.
	if _, err := f.s.RemoveNetMember(ctx, "system", f.ch.ID, "n1", f.dave.ID); err != nil {
		t.Fatal(err)
	}
	later, rec := f.toNet(t, f.alice, f.n1, "after dave left")
	if want := []int64{f.alice.ID, f.bob.ID, f.frank.ID}; !reflect.DeepEqual(rec, want) {
		t.Fatalf("recipients after the leave = %v, want %v", rec, want)
	}
	if got, want := visibleIDs(t, f.s, f.ch.ID, Reader{f.dave.ID, true}), []int64{before, after}; !reflect.DeepEqual(got, want) {
		t.Errorf("leaver sees %v, want %v", got, want)
	}
	if got := visibleIDs(t, f.s, f.ch.ID, Reader{f.alice.ID, true}); !reflect.DeepEqual(got, []int64{before, after, later}) {
		t.Errorf("author sees %v", got)
	}

	// Leaving the channel removes the net seat and hides everything scoped.
	if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.bob.ID); err != nil {
		t.Fatal(err)
	}
	if got := visibleIDs(t, f.s, f.ch.ID, Reader{f.bob.ID, true}); len(got) != 0 {
		t.Errorf("principal removed from the channel sees %v", got)
	}
	_, rec = f.toNet(t, f.alice, f.n1, "after bob left the channel")
	for _, r := range rec {
		if r == f.bob.ID {
			t.Errorf("a principal who left the channel is a recipient: %v", rec)
		}
	}
}

func TestInsertScopedMessageRules(t *testing.T) {
	f := newAudienceFixture(t)
	archived := f.mustNet(t, "old")
	f.put(t, "old", f.alice, schema.NetRoleMember)
	if _, err := f.s.ArchiveNet(context.Background(), "system", f.ch.ID, "old"); err != nil {
		t.Fatal(err)
	}
	otherNet, err := f.s.CreateNet(context.Background(), "system", f.other.ID, "elsewhere", f.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AddChannelMember(context.Background(), "system", f.other.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PutNetMember(context.Background(), "system", f.other.ID, "elsewhere", f.alice.ID, schema.NetRoleMember, 0); err != nil {
		t.Fatal(err)
	}
	net := func(id int64) schema.Audience { return schema.Audience{Kind: schema.AudienceKindNet, NetID: id} }
	who := func(ids ...int64) schema.Audience {
		return schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: ids}
	}

	tests := []struct {
		name   string
		author Principal
		aud    schema.Audience
		want   error
	}{
		{"member posts to its net", f.alice, net(f.n1.ID), nil},
		{"monitor may not transmit", f.bob, net(f.n1.ID), ErrNetMonitorOnly},
		{"not on the net", f.frank, net(f.n1.ID), ErrNetNotFound},
		{"unknown net", f.alice, net(9999), ErrNetNotFound},
		{"archived net", f.alice, net(archived.ID), ErrNetNotFound},
		{"net of another channel", f.alice, net(otherNet.ID), ErrNetNotFound},
		{"author not a channel member", f.carol, net(f.n1.ID), ErrNotChannelMember},
		{"whisper to a member", f.frank, who(f.dave.ID), nil},
		{"whisper to a non-member", f.frank, who(f.carol.ID), ErrInvalidAudience},
		{"whisper to an unknown principal", f.frank, who(9999), ErrInvalidAudience},
		{"whisper to nobody but oneself", f.frank, who(f.frank.ID), ErrInvalidAudience},
		{"whisper with no one", f.frank, who(), ErrInvalidAudience},
		{"net audience with principals", f.alice, schema.Audience{Kind: schema.AudienceKindNet, NetID: f.n1.ID, PrincipalIDs: []int64{1}}, ErrInvalidAudience},
		{"unknown kind", f.alice, schema.Audience{Kind: "channel"}, ErrInvalidAudience},
		{"whisper author not a channel member", f.carol, who(f.dave.ID), ErrNotChannelMember},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			msgsBefore, _ := f.s.CountMessages(ctx, f.ch.ID)
			eventsBefore, _ := f.s.ListAuditEvents(ctx, 0, 10000)
			aud := tt.aud
			if aud.Kind == schema.AudienceKindPrincipals && len(aud.PrincipalIDs) > 0 {
				aud = aud.Normalize(tt.author.ID)
			}
			m, rec, err := f.s.InsertScopedMessage(ctx, ScopedPost{ChannelID: f.ch.ID, AuthorID: tt.author.ID, Body: "secret body", Audience: aud})
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			msgsAfter, _ := f.s.CountMessages(ctx, f.ch.ID)
			eventsAfter, _ := f.s.ListAuditEvents(ctx, 0, 10000)
			if tt.want != nil {
				if m.ID != 0 || rec != nil {
					t.Errorf("a refused post returned %+v, %v", m, rec)
				}
				if msgsAfter != msgsBefore || len(eventsAfter) != len(eventsBefore) {
					t.Errorf("a refused post wrote: messages %d -> %d, audit %d -> %d", msgsBefore, msgsAfter, len(eventsBefore), len(eventsAfter))
				}
				return
			}
			if msgsAfter != msgsBefore+1 {
				t.Errorf("messages %d -> %d", msgsBefore, msgsAfter)
			}
			if m.Audience == nil || len(rec) < 2 {
				t.Errorf("stored %+v with recipients %v", m, rec)
			}
		})
	}
}

// A net message stores only the net id as its audience; the recipients are
// stored and audited but are not part of the message a reader gets. A whisper
// reads back as the full normalized list.
func TestScopedAudienceReadBack(t *testing.T) {
	f := newAudienceFixture(t)
	netMsg, _ := f.toNet(t, f.alice, f.n1, "net")
	whisperMsg := f.whisper(t, f.frank, "psst", f.erin, f.dave)
	msgs, err := f.s.ListVisibleMessages(context.Background(), f.ch.ID, Reader{f.dave.ID, true}, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]*schema.Audience{}
	for _, m := range msgs {
		byID[m.ID] = m.Audience
	}
	wantNet := &schema.Audience{Kind: schema.AudienceKindNet, NetID: f.n1.ID}
	if !reflect.DeepEqual(byID[netMsg], wantNet) {
		t.Errorf("net audience = %+v, want %+v", byID[netMsg], wantNet)
	}
	wantWhisper := &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{f.dave.ID, f.erin.ID, f.frank.ID}}
	if !reflect.DeepEqual(byID[whisperMsg], wantWhisper) {
		t.Errorf("whisper audience = %+v, want %+v", byID[whisperMsg], wantWhisper)
	}
}

func TestScopedMessageAudit(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	wide := f.wide(t, f.frank, "sensitive channel-wide body")
	netMsg, _ := f.toNet(t, f.alice, f.n1, "sensitive net body")
	whisperMsg := f.whisper(t, f.frank, "sensitive whisper body", f.dave)

	events, err := f.s.ListAuditEvents(ctx, 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	scoped := map[string]AuditEvent{}
	for _, e := range events {
		if e.Action == AuditMessageScoped {
			if _, dup := scoped[e.Subject]; dup {
				t.Errorf("two message_scoped events for %s", e.Subject)
			}
			scoped[e.Subject] = e
		}
		if strings.Contains(e.Detail+e.Subject+e.Actor, "sensitive") {
			t.Errorf("audit event carries message content: %+v", e)
		}
	}
	if len(scoped) != 2 {
		t.Fatalf("message_scoped events = %d, want 2 (none for the channel-wide post)", len(scoped))
	}
	if _, ok := scoped[fmt.Sprintf("message:%d", wide)]; ok {
		t.Error("a channel-wide post wrote message_scoped")
	}
	tests := []struct {
		id     int64
		actor  int64
		detail string
	}{
		{netMsg, f.alice.ID, fmt.Sprintf("channel=%d audience=net net=%d recipients=%d,%d,%d", f.ch.ID, f.n1.ID, f.alice.ID, f.bob.ID, f.dave.ID)},
		{whisperMsg, f.frank.ID, fmt.Sprintf("channel=%d audience=principals recipients=%d,%d", f.ch.ID, f.dave.ID, f.frank.ID)},
	}
	for _, tt := range tests {
		e := scoped[fmt.Sprintf("message:%d", tt.id)]
		if e.Actor != fmt.Sprintf("principal:%d", tt.actor) || e.Detail != tt.detail {
			t.Errorf("message %d audit = actor %q detail %q, want principal:%d %q", tt.id, e.Actor, e.Detail, tt.actor, tt.detail)
		}
	}
}

// next_after paging must step over scoped messages a reader cannot see without
// skipping visible ones or looping.
func TestListVisibleMessagesPaging(t *testing.T) {
	f := newAudienceFixture(t)
	var all []int64
	var dave []int64
	for i := 0; i < 6; i++ {
		w := f.wide(t, f.frank, fmt.Sprintf("wide %d", i))
		all = append(all, w)
		dave = append(dave, w)
		// Two messages in a row that dave cannot see, then one he can.
		f.toNet(t, f.alice, f.n2, "hidden")
		f.whisper(t, f.frank, "hidden", f.erin)
		m, _ := f.toNet(t, f.alice, f.n1, "visible net")
		dave = append(dave, m)
	}
	for _, limit := range []int{1, 2, 3, 5, 100} {
		var got []int64
		after := int64(0)
		for pages := 0; ; pages++ {
			if pages > 100 {
				t.Fatalf("limit %d: paging did not terminate", limit)
			}
			page, err := f.s.ListVisibleMessages(context.Background(), f.ch.ID, Reader{f.dave.ID, true}, after, limit+1)
			if err != nil {
				t.Fatal(err)
			}
			more := len(page) > limit
			if more {
				page = page[:limit]
			}
			for _, m := range page {
				got = append(got, m.ID)
				after = m.ID
			}
			if !more {
				break
			}
		}
		if !reflect.DeepEqual(got, dave) {
			t.Errorf("limit %d: paged %v, want %v", limit, got, dave)
		}
	}
	// The channel-wide-only reader pages through channel-wide ones alone.
	var wideOnly []int64
	for after := int64(0); ; {
		page, err := f.s.ListVisibleMessages(context.Background(), f.ch.ID, ChannelWideOnly, after, 4)
		if err != nil || len(page) == 0 {
			break
		}
		for _, m := range page {
			wideOnly = append(wideOnly, m.ID)
			after = m.ID
		}
	}
	if !reflect.DeepEqual(wideOnly, all) {
		t.Errorf("channel-wide-only paged %v, want %v", wideOnly, all)
	}
}

// Rows from before migration 12 are channel-wide and stay readable by every
// reader that could read them before.
func TestScopedMigrationFromSchema11(t *testing.T) {
	const pre = 11
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
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old message', 5)`,
		`PRAGMA user_version = 11`,
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
	for _, r := range []Reader{ChannelWideOnly, {2, true}, {2, false}} {
		if got := visibleIDs(t, s, 1, r); !reflect.DeepEqual(got, []int64{1}) {
			t.Errorf("reader %+v sees %v after migration, want the old message", r, got)
		}
	}
	var recipients int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM message_recipients").Scan(&recipients); err != nil || recipients != 0 {
		t.Errorf("recipients after migration = %d (%v), want none", recipients, err)
	}
	var kind sql.NullString
	if err := s.db.QueryRow("SELECT audience_kind FROM messages WHERE id = 1").Scan(&kind); err != nil || kind.Valid {
		t.Errorf("audience_kind of an old row = %v (%v), want NULL", kind, err)
	}
}

// The structural half of the single-visibility-function rule, inside the
// store: only audience.go reads rows of the messages table. A second reader
// would be a second place for a scoped message to leak from.
func TestOnlyAudienceGoReadsMessageRows(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "audience.go" {
			continue
		}
		data, err := os.ReadFile(name) // #nosec G304 -- a source file of this package, from the glob above
		if err != nil {
			t.Fatal(err)
		}
		src := string(data)
		for _, forbidden := range []string{"FROM messages", "JOIN messages", "from messages", "join messages"} {
			if strings.Contains(src, forbidden) {
				t.Errorf("%s contains %q; messages are read only in audience.go (ListVisibleMessages)", name, forbidden)
			}
		}
	}
}
