package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Archiving a channel (issue #844): migration 41, the settings change, and
// every guard of the archived state. The guards live in the store, inside the
// transaction that would write, so each test here asks the store directly.

// setArchived archives (true) or restores (false) ch through the settings
// change, as the system.
func setArchived(t *testing.T, s *Store, ch Channel, archived bool) ChannelSettingsResult {
	t.Helper()
	res, err := s.ChannelSettings(context.Background(), "system", 0, ch.ID, ChannelSettingsChange{Archived: &archived}, time.Now())
	if err != nil {
		t.Fatalf("ChannelSettings(archived=%t): %v", archived, err)
	}
	return res
}

// rowCounts is every table an archived-channel write could touch, so a
// refusal can be shown to have written nothing.
func rowCounts(t *testing.T, s *Store) string {
	t.Helper()
	var parts []string
	for _, table := range []string{"messages", "message_reactions", "files", "channel_members", "invites", "audit_events", "message_recipients", "message_revisions"} {
		var n int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		parts = append(parts, fmt.Sprintf("%s=%d", table, n))
	}
	return strings.Join(parts, " ")
}

// TestMigration41ArchivedAt: migration 41 adds archived_at, NULL for every
// existing channel, so every channel reads live afterwards.
func TestMigration41ArchivedAt(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/conch.db"
	s := openTestStoreAt(t, path)
	for _, name := range []string{"one", "two"} {
		if _, err := s.CreateChannel(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Rewind to 40: no column, as a server before this issue held it.
	rewind, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		// Migration 45 (issue #849) first, then 43 (issue #1326), then 42's
		// columns, then 41's.
		"DROP TABLE message_removals", // migration 45 (issue #849); its triggers go with it
		"DROP TABLE channel_bans",     // migration 45 (issue #849)
		"DROP TABLE admin_actions",
		"ALTER TABLE agent_manifests DROP COLUMN admin",
		"ALTER TABLE agent_manifests DROP COLUMN permissions",
		"ALTER TABLE credentials DROP COLUMN issued_by", // migration 42 (issue #1024)
		"ALTER TABLE hooks DROP COLUMN issued_by",       // migration 42 (issue #1024)
		"ALTER TABLE channels DROP COLUMN archived_at",
		fmt.Sprintf("PRAGMA user_version = %d", archiveMigration-1),
	} {
		if _, err := rewind.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	if err := rewind.Close(); err != nil {
		t.Fatal(err)
	}

	s = openTestStoreAt(t, path)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < archiveMigration {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	channels, err := s.ListChannels(ctx)
	if err != nil || len(channels) != 2 {
		t.Fatalf("ListChannels = %v, %v", channels, err)
	}
	for _, c := range channels {
		if c.Archived() || !c.ArchivedAt.IsZero() {
			t.Errorf("channel %q is archived after migration: %+v", c.Name, c)
		}
	}
	var null int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM channels WHERE archived_at IS NULL").Scan(&null); err != nil || null != 2 {
		t.Errorf("channels with a NULL archived_at = %d, %v; want 2", null, err)
	}
}

// TestChannelSettingsArchive: archive and restore are one settings change,
// audited as the visibility and default changes are, idempotent, and the
// default refusal changes nothing.
func TestChannelSettingsArchive(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, _ := s.CreateChannel(ctx, "old")
	def, _ := s.CreateChannel(ctx, "front")
	pub := schema.ChannelPublic
	yes := true
	if _, err := s.ChannelSettings(ctx, "system", 0, def.ID, ChannelSettingsChange{Visibility: &pub, Default: &yes}, time.Now()); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(1_800_000_000, 0)
	res := setArchivedAt(t, s, ch, true, now)
	if !res.Changed || !res.Channel.Archived() || !res.Channel.ArchivedAt.Equal(now) {
		t.Fatalf("archive = %+v, want changed and archived at %v", res, now)
	}
	got, err := s.ChannelByID(ctx, ch.ID)
	if err != nil || !got.Archived() || !got.ArchivedAt.Equal(now) {
		t.Fatalf("ChannelByID = %+v, %v; want archived at %v", got, err, now)
	}
	if again := setArchived(t, s, ch, true); again.Changed {
		t.Errorf("archiving an archived channel changed it: %+v", again)
	}
	if res := setArchived(t, s, ch, false); !res.Changed || res.Channel.Archived() {
		t.Errorf("restore = %+v, want changed and live", res)
	}
	if again := setArchived(t, s, ch, false); again.Changed {
		t.Errorf("restoring a live channel changed it: %+v", again)
	}

	var details []string
	events, err := s.ListAuditEvents(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Action == AuditChannelSettingsChanged && e.Subject == channelSubject(ch.ID) {
			details = append(details, e.Detail)
		}
	}
	if want := []string{"archived=false->true", "archived=true->false"}; len(details) != 2 || details[0] != want[0] || details[1] != want[1] {
		t.Errorf("channel_settings_changed details = %q, want %q", details, want)
	}

	// The default refusals: nothing changes and nothing is audited.
	no := false
	tests := []struct {
		name   string
		target Channel
		change ChannelSettingsChange
	}{
		{"archive a default channel", def, ChannelSettingsChange{Archived: &yes}},
		{"archive and unmark in one request", def, ChannelSettingsChange{Archived: &yes, Default: &no}},
		{"archive and mark default together", ch, ChannelSettingsChange{Archived: &yes, Default: &yes, Visibility: &pub}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := rowCounts(t, s)
			_, err := s.ChannelSettings(ctx, "system", 0, tt.target.ID, tt.change, time.Now())
			if !errors.Is(err, ErrChannelArchiveDefault) {
				t.Fatalf("err = %v, want ErrChannelArchiveDefault", err)
			}
			if after := rowCounts(t, s); after != before {
				t.Errorf("a refused archive wrote rows: %s -> %s", before, after)
			}
			got, err := s.ChannelByID(ctx, tt.target.ID)
			if err != nil || got.Archived() {
				t.Errorf("channel after the refusal = %+v, %v; want live", got, err)
			}
		})
	}
	// An archived channel is not marked default later either.
	setArchived(t, s, ch, true)
	if _, err := s.ChannelSettings(ctx, "system", 0, ch.ID, ChannelSettingsChange{Visibility: &pub, Default: &yes}, time.Now()); !errors.Is(err, ErrChannelArchiveDefault) {
		t.Errorf("marking an archived channel default = %v, want ErrChannelArchiveDefault", err)
	}
	// Nor through the join settings' open_channels.
	if _, err := s.ChannelSettings(ctx, "system", 0, ch.ID, ChannelSettingsChange{Visibility: &pub}, time.Now()); err != nil {
		t.Fatal(err)
	}
	openChannels := []string{"front", "old"}
	if _, err := s.PutJoinSettings(ctx, "system", 0, schema.PutJoinSettingsRequestV1{OpenChannels: &openChannels}); !errors.Is(err, ErrChannelArchiveDefault) {
		t.Errorf("open_channels naming an archived channel = %v, want ErrChannelArchiveDefault", err)
	}
}

func setArchivedAt(t *testing.T, s *Store, ch Channel, archived bool, at time.Time) ChannelSettingsResult {
	t.Helper()
	res, err := s.ChannelSettings(context.Background(), "system", 0, ch.ID, ChannelSettingsChange{Archived: &archived}, at)
	if err != nil {
		t.Fatalf("ChannelSettings(archived=%t): %v", archived, err)
	}
	return res
}

// TestChannelSettingsArchiveNeedsManageChannels: the store re-checks
// manage_channels under its lock, for archive as for every setting; a channel
// manager without it is refused and nothing changes.
func TestChannelSettingsArchiveNeedsManageChannels(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	op, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	mgr, _ := s.CreatePrincipal(ctx, PrincipalHuman, "mgr")
	member, _ := s.CreatePrincipal(ctx, PrincipalHuman, "member")
	ch, err := s.CreateChannelAs(ctx, "system", "room", mgr.ID)
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	for _, tt := range []struct {
		name  string
		actor int64
		want  error
	}{
		{"owner", op.ID, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.ChannelSettings(ctx, "system", tt.actor, ch.ID, ChannelSettingsChange{Archived: &yes}, time.Now()); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	setArchived(t, s, ch, false)
	for _, tt := range []struct {
		name  string
		actor int64
	}{
		{"a member without manage_channels", member.ID},
		{"the channel's manager without manage_channels", mgr.ID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := rowCounts(t, s)
			if _, err := s.ChannelSettings(ctx, "system", tt.actor, ch.ID, ChannelSettingsChange{Archived: &yes}, time.Now()); !errors.Is(err, ErrManageChannelsRequired) {
				t.Fatalf("err = %v, want ErrManageChannelsRequired", err)
			}
			if after := rowCounts(t, s); after != before {
				t.Errorf("a refused archive wrote rows: %s -> %s", before, after)
			}
			if got, _ := s.ChannelByID(ctx, ch.ID); got.Archived() {
				t.Error("the channel is archived after a refusal")
			}
		})
	}
}

// TestArchivedGuards: while a channel is archived each write refuses with
// ErrChannelArchived and writes nothing, and the same call works before the
// archive and after the restore.
func TestArchivedGuards(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	s := f.s
	// A channel carol may join, and a root, a reaction and a message to edit.
	pub := schema.ChannelPublic
	if _, err := s.ChannelSettings(ctx, "system", 0, f.ch.ID, ChannelSettingsChange{Visibility: &pub}, time.Now()); err != nil {
		t.Fatal(err)
	}
	root := f.wide(t, f.alice, "root")
	edited := f.wide(t, f.alice, "to edit")
	toReact := f.wide(t, f.bob, "to react to")
	// bob holds a reaction on his own message, so that RemoveReaction has one to remove.
	held := rkey(t, thumbs)
	if _, err := s.SetReaction(ctx, f.ch.ID, rdr(f.bob), toReact, held, reactionNow); err != nil {
		t.Fatal(err)
	}
	var fileSeq int
	// repeatable calls: each can be made again after the restore.
	tests := []struct {
		name string
		call func() error
		// afterRestore is the error the call gives once the channel is live
		// again; nil means it works.
		afterRestore error
	}{
		{"channel-wide post", func() error {
			_, err := s.InsertMessageV1(ctx, f.ch.ID, f.alice.ID, "hello", nil)
			return err
		}, nil},
		{"reply to a channel-wide root", func() error {
			_, _, err := s.InsertReplyMessage(ctx, ReplyPost{ChannelID: f.ch.ID, AuthorID: f.alice.ID, RootID: root, Body: "re"}, rdr(f.alice))
			return err
		}, nil},
		{"whisper", func() error {
			_, _, err := s.InsertScopedMessage(ctx, ScopedPost{ChannelID: f.ch.ID, AuthorID: f.alice.ID, Body: "psst",
				Audience: schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{f.alice.ID, f.bob.ID}}})
			return err
		}, nil},
		{"net post", func() error {
			_, _, err := s.InsertScopedMessage(ctx, ScopedPost{ChannelID: f.ch.ID, AuthorID: f.alice.ID, Body: "net",
				Audience: schema.Audience{Kind: schema.AudienceKindNet, NetID: f.n1.ID}})
			return err
		}, nil},
		{"edit", func() error {
			_, err := f.s.EditOwnMessage(ctx, f.ch.ID, rdr(f.alice), edited, "edited", time.Now)
			return err
		}, nil},
		{"edit that changes nothing", func() error {
			_, err := f.s.EditOwnMessage(ctx, f.ch.ID, rdr(f.alice), root, "root", time.Now)
			return err
		}, nil},
		{"set reaction", func() error {
			_, err := s.SetReaction(ctx, f.ch.ID, rdr(f.alice), toReact, rkey(t, party), reactionNow)
			return err
		}, nil},
		{"remove reaction", func() error {
			_, err := s.RemoveReaction(ctx, f.ch.ID, rdr(f.bob), toReact, held, reactionNow)
			return err
		}, nil},
		{"moderate reactions", func() error {
			_, err := s.ModerateReactions(ctx, f.ch.ID, rdr(f.alice), toReact, f.bob.ID, "", reactionNow)
			// alice is no moderator: live, the same call is the manager refusal.
			return err
		}, ErrManagerRefused},
		{"upload", func() error {
			fileSeq++
			_, err := s.InsertFile(ctx, NewFile{ChannelID: f.ch.ID, UploaderID: f.alice.ID, Filename: "a.txt", MediaType: "text/plain", Size: 3, SHA256: fmt.Sprintf("%064x", fileSeq)})
			return err
		}, nil},
		{"self-join", func() error {
			_, err := s.JoinChannel(ctx, f.ch.ID, f.carol.ID, time.Now())
			return err
		}, nil},
		{"add a member (put)", func() error {
			_, err := s.PutChannelMember(ctx, "system", f.ch.ID, f.carol.ID, 0, nil)
			return err
		}, nil},
		{"add a member (if absent)", func() error {
			_, _, err := s.AddChannelMemberIfAbsent(ctx, "system", f.ch.ID, 0, MemberTarget{ID: f.carol.ID}, nil)
			return err
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Live first: the guard is the only thing that refuses. A call that
			// adds a member is undone so that it can be made again.
			setArchived(t, s, f.ch, false)
			if err := tt.call(); err != nil && !errors.Is(err, tt.afterRestore) {
				t.Fatalf("call on a live channel: %v", err)
			}
			if _, err := s.RemoveChannelMember(ctx, "system", f.ch.ID, f.carol.ID); err != nil {
				t.Fatal(err)
			}

			setArchived(t, s, f.ch, true)
			before := rowCounts(t, s)
			if err := tt.call(); !errors.Is(err, ErrChannelArchived) {
				t.Fatalf("call on an archived channel = %v, want ErrChannelArchived", err)
			}
			if after := rowCounts(t, s); after != before {
				t.Errorf("a refused call wrote rows: %s -> %s", before, after)
			}

			setArchived(t, s, f.ch, false)
			if err := tt.call(); !errors.Is(err, tt.afterRestore) && err != nil {
				t.Errorf("call after the restore = %v, want %v", err, tt.afterRestore)
			}
			if _, err := s.RemoveChannelMember(ctx, "system", f.ch.ID, f.carol.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestArchivedGuardsAreInTheInsertTransactions: the two INSERT INTO messages
// are guarded where they insert, so even a caller that skipped every other
// check cannot post to an archived channel.
func TestArchivedGuardsAreInTheInsertTransactions(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	setArchived(t, f.s, f.ch, true)
	err := f.s.withImmediateTx(ctx, func(tx execer) error {
		if _, err := insertChannelWideTx(ctx, tx, f.ch.ID, f.alice.ID, "x", nil, 0, time.Now()); !errors.Is(err, ErrChannelArchived) {
			t.Errorf("insertChannelWideTx = %v, want ErrChannelArchived", err)
		}
		if _, err := insertScopedTx(ctx, tx, ScopedPost{ChannelID: f.ch.ID, AuthorID: f.alice.ID, Body: "x"}, 0, nil,
			schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{f.alice.ID, f.bob.ID}}, []int64{f.alice.ID, f.bob.ID}, time.Now()); !errors.Is(err, ErrChannelArchived) {
			t.Errorf("insertScopedTx = %v, want ErrChannelArchived", err)
		}
		return errors.New("roll back")
	})
	if err == nil || err.Error() != "roll back" {
		t.Fatal(err)
	}
}

// TestArchivedAllowsReadingLeavingAndRemoving: what an archived channel still
// does (decision 6): history, read marks, leaving, an operator's removal.
func TestArchivedAllowsReadingLeavingAndRemoving(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	s := f.s
	first := f.wide(t, f.alice, "one")
	last := f.wide(t, f.bob, "two")
	setArchived(t, s, f.ch, true)

	msgs, err := s.ListVisibleMessages(ctx, f.ch.ID, rdr(f.alice), 0, 100)
	if err != nil || len(msgs) < 2 {
		t.Fatalf("history of an archived channel = %d messages, %v", len(msgs), err)
	}
	if _, err := s.MarkRead(ctx, f.ch.ID, rdr(f.alice), last, time.Now()); err != nil {
		t.Errorf("MarkRead in an archived channel: %v", err)
	}
	_ = first
	if err := s.LeaveChannel(ctx, f.ch.ID, f.dave.ID, time.Now()); err != nil {
		t.Errorf("leaving an archived channel: %v", err)
	}
	if removed, err := s.RemoveChannelMember(ctx, "system", f.ch.ID, f.erin.ID); err != nil || !removed {
		t.Errorf("removing a member of an archived channel = %v, %v", removed, err)
	}
}

// TestArchiveRestoreKeepsHistoryMembersAndReadMarks: the rows are
// byte-for-byte what they were across archive and restore.
func TestArchiveRestoreKeepsHistoryMembersAndReadMarks(t *testing.T) {
	ctx := context.Background()
	f := newAudienceFixture(t)
	s := f.s
	f.wide(t, f.alice, "one")
	m2 := f.wide(t, f.bob, "two")
	if _, err := s.MarkRead(ctx, f.ch.ID, rdr(f.alice), m2, time.Now()); err != nil {
		t.Fatal(err)
	}
	dump := func() string {
		var b strings.Builder
		for _, q := range []string{
			"SELECT id, channel_id, author_id, body, created_at, COALESCE(edited_at, 0) FROM messages ORDER BY id",
			"SELECT channel_id, principal_id, added_by, created_at, can_decide, manages FROM channel_members ORDER BY channel_id, principal_id",
			"SELECT * FROM read_marks ORDER BY 1, 2",
		} {
			rows, err := s.db.Query(q)
			if err != nil {
				t.Fatal(err)
			}
			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				fmt.Fprintln(&b, vals...)
			}
			_ = rows.Close()
		}
		return b.String()
	}
	before := dump()
	if strings.Count(before, "\n") < 6 {
		t.Fatalf("dump is too small to mean anything:\n%s", before)
	}
	setArchived(t, s, f.ch, true)
	setArchived(t, s, f.ch, false)
	if after := dump(); after != before {
		t.Errorf("archive and restore changed rows:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestArchivedNameStaysTaken: creating or renaming to an archived channel's
// name is the existing duplicate refusal (decision 12), by name_key.
func TestArchivedNameStaysTaken(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	old, _ := s.CreateChannel(ctx, "Old-Project")
	other, _ := s.CreateChannel(ctx, "other")
	setArchived(t, s, old, true)
	if _, err := s.CreateChannel(ctx, "old-project"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("creating the archived name = %v, want ErrDuplicate", err)
	}
	if _, err := s.RenameChannel(ctx, "system", other.ID, "other", "OLD-PROJECT", time.Now()); !errors.Is(err, ErrDuplicate) {
		t.Errorf("renaming to the archived name = %v, want ErrDuplicate", err)
	}
}

// TestArchivedDirectory: a member sees an archived channel in their listing
// with its time; a non-member does not see it in the member directory; the
// scope=all listing shows it (decision 9).
func TestArchivedDirectory(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	bob, _ := s.CreatePrincipal(ctx, PrincipalHuman, "bob")
	ch, _ := s.CreateChannel(ctx, "old")
	pub := schema.ChannelPublic
	if _, err := s.ChannelSettings(ctx, "system", 0, ch.ID, ChannelSettingsChange{Visibility: &pub}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddChannelMember(ctx, "system", ch.ID, alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	at := time.Unix(1_800_000_000, 0)
	setArchivedAt(t, s, ch, true, at)

	names := func(l []ChannelListing) []string {
		var out []string
		for _, c := range l {
			out = append(out, c.Name)
		}
		return out
	}
	for _, tt := range []struct {
		name   string
		viewer int64
		all    bool
		want   []string
	}{
		{"a member sees it", alice.ID, false, []string{"old"}},
		{"a non-member does not", bob.ID, false, nil},
		{"scope=all shows it to an operator", bob.ID, true, []string{"old"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l, err := s.ChannelDirectory(ctx, tt.viewer, tt.all)
			if err != nil {
				t.Fatal(err)
			}
			if got := names(l); strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("listing = %v, want %v", got, tt.want)
			}
			for _, c := range l {
				if !c.Archived() || !c.ArchivedAt.Equal(at) {
					t.Errorf("row %+v is not marked archived at %v", c, at)
				}
			}
		})
	}
	mine, err := s.ListChannelsForPrincipal(ctx, alice.ID)
	if err != nil || len(mine) != 1 || !mine[0].Archived() {
		t.Errorf("ListChannelsForPrincipal = %+v, %v; want the archived channel marked", mine, err)
	}
}

// TestArchivedInvites: an invite or link naming an archived channel is not
// made, and an older one that names it redeems without it (decision 6).
func TestArchivedInvites(t *testing.T) {
	f := newInviteFixture(t)
	// An older legacy invite naming general and lobby, made before the archive.
	older := f.mustCreate(f.op, open("general", "lobby"))
	archivedBefore := f.count("SELECT COUNT(*) FROM invites")

	setArchived(f.t, f.s, f.lobby, true)

	// Creation: an operator (manage_channels) and a member in the channel.
	if _, err := f.create(f.op, open("lobby")); !errors.Is(err, ErrChannelArchived) {
		t.Errorf("CreateInvite as the operator naming an archived channel = %v, want ErrChannelArchived", err)
	}
	if _, err := f.create(f.alice, personal("lobby")); !errors.Is(err, ErrChannelArchived) {
		t.Errorf("CreateInvite as a member of the archived channel = %v, want ErrChannelArchived", err)
	}
	req := memberLink()
	req.LandingChannels = []string{"lobby"}
	if _, err := f.createLink(f.op, req); !errors.Is(err, ErrChannelArchived) {
		t.Errorf("CreateMemberLink landing in an archived channel = %v, want ErrChannelArchived", err)
	}
	if n := f.count("SELECT COUNT(*) FROM invites"); n != archivedBefore {
		t.Errorf("refused creations stored %d invites, want %d", n, archivedBefore)
	}
	// A creator who is not in the channel gets the existing refusal, which
	// says nothing about it being archived.
	setArchived(f.t, f.s, f.priv, true)
	if _, err := f.create(f.alice, personal("private")); !errors.Is(err, ErrInviteGrantExceedsInviter) {
		t.Errorf("CreateInvite by a non-member of the archived channel = %v, want ErrInviteGrantExceedsInviter", err)
	}
	// Other channels still work.
	if _, err := f.create(f.alice, personal("general")); err != nil {
		t.Errorf("CreateInvite naming a live channel: %v", err)
	}

	// Redemption: the older invite succeeds, in general only.
	got, err := f.redeem(older.Code, "joiner")
	if err != nil {
		t.Fatalf("redeeming an older invite that names an archived channel: %v", err)
	}
	j := f.joiner(got.Principal.ID)
	for _, m := range j.memberships {
		if strings.HasPrefix(m, fmt.Sprintf("%d:", f.lobby.ID)) {
			t.Errorf("the joiner is in the archived channel: %v", j.memberships)
		}
	}
	if len(j.memberships) != 1 || !strings.HasPrefix(j.memberships[0], fmt.Sprintf("%d:", f.general.ID)) {
		t.Errorf("memberships = %v, want only general", j.memberships)
	}
	if !containsInt64(got.DroppedChannels, f.lobby.ID) {
		t.Errorf("dropped channels = %v, want the archived one named", got.DroppedChannels)
	}
}

// TestArchivedInviteWithNothingLeftIsInvalid: an invite whose every channel is
// archived has nowhere to put its joiner and is refused as one whose maker
// lost access is.
func TestArchivedInviteWithNothingLeftIsInvalid(t *testing.T) {
	f := newInviteFixture(t)
	inv := f.mustCreate(f.op, open("lobby"))
	setArchived(t, f.s, f.lobby, true)
	if _, err := f.redeem(inv.Code, "joiner"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("redeeming an invite whose only channel is archived = %v, want ErrInviteInvalid", err)
	}
}

// TestArchivedChannelManagerCannotAddMembers: the manager's own add path (the
// manager standing branch of putChannelMember) is guarded too, and the
// manager can add again after the restore.
func TestArchivedChannelManagerCannotAddMembers(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	mgr, _ := s.CreatePrincipal(ctx, PrincipalHuman, "mgr")
	carol, _ := s.CreatePrincipal(ctx, PrincipalHuman, "carol")
	ch, err := s.CreateChannel(ctx, "room")
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	if _, err := s.PutChannelMember(ctx, "system", ch.ID, mgr.ID, 0, nil, &yes); err != nil {
		t.Fatal(err)
	}
	setArchived(t, s, ch, true)
	before := rowCounts(t, s)
	if _, err := s.PutChannelMember(ctx, principalActor(mgr.ID), ch.ID, carol.ID, mgr.ID, nil); !errors.Is(err, ErrChannelArchived) {
		t.Fatalf("a manager adding to an archived channel = %v, want ErrChannelArchived", err)
	}
	if _, _, err := s.AddChannelMemberIfAbsent(ctx, principalActor(mgr.ID), ch.ID, mgr.ID, MemberTarget{ID: carol.ID}, nil); !errors.Is(err, ErrChannelArchived) {
		t.Fatalf("a manager adding by name or id to an archived channel = %v, want ErrChannelArchived", err)
	}
	if after := rowCounts(t, s); after != before {
		t.Errorf("a refused add wrote rows: %s -> %s", before, after)
	}
	setArchived(t, s, ch, false)
	if added, err := s.PutChannelMember(ctx, principalActor(mgr.ID), ch.ID, carol.ID, mgr.ID, nil); err != nil || !added {
		t.Errorf("a manager adding after the restore = %v, %v", added, err)
	}
}
