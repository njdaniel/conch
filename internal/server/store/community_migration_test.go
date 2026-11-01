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

// rewindToBeforeCommunity undoes migration 40's DDL on the database at path
// and sets its version to 39, so that opening it runs migration 40 over the
// rows already there. The rows are made through the store at the current
// version first, which needs no hand-written SQL for credentials, visits or
// invites; migration 40's Go step leaves a new database alone, so the rows
// are exactly what a server at 39 would hold.
// Migration 41 and later: when you add a migration, extend this undo list
// (and its twin in internal/server/community_migration_test.go) with your DDL in reverse, or this rewind
// leaves a database newer than 39 and the test no longer runs migration 40.
func rewindToBeforeCommunity(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		"DROP TRIGGER channels_default_public_insert",
		"DROP TRIGGER channels_default_public_update",
		"ALTER TABLE channels DROP COLUMN visibility",
		"ALTER TABLE channels DROP COLUMN is_default",
		"ALTER TABLE channels DROP COLUMN default_listed_by",
		"DROP TABLE legacy_history_bounds",
		"ALTER TABLE invites DROP COLUMN link",
		"DELETE FROM server_settings WHERE key IN ('open_signup', 'member_link_default_expiry_seconds')",
		fmt.Sprintf("PRAGMA user_version = %d", communityMigration-1),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
}

// communityAlpha is a small alpha before migration 40 (issue #979, design
// §6): #general in open_channels and #belgium not; an owner holding the
// right to decide in #general; alice with a bound on #general and a seat in
// #belgium; bob with a bound on #belgium only; carol disabled; a converted
// guest; an ended guest; an active guest in #belgium and another in
// #general; open personal and open invites; guest-admitting invites naming
// #belgium and #general, and an open both invite; guests_enabled true,
// open_joining both and join_voice false.
type communityAlpha struct {
	path                                    string
	owner, alice, bob, carol                Principal
	converted, ended, activeBelg, activeGen Principal
	general, belgium                        Channel
	accountInvites, guestInvites            []int64
}

func newCommunityAlpha(t *testing.T) communityAlpha {
	t.Helper()
	ctx := context.Background()
	path := newTestDBPath(t)
	s := openTestStoreAt(t, path)
	var a communityAlpha
	a.path = path
	var err error
	if a.owner, _, _, err = s.BootstrapOperator(ctx, "owner"); err != nil {
		t.Fatal(err)
	}
	mk := func(name string) Principal {
		p, err := s.CreatePrincipal(ctx, PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a.alice, a.bob, a.carol = mk("alice"), mk("bob"), mk("carol")
	if a.general, err = s.CreateChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	if a.belgium, err = s.CreateChannel(ctx, "belgium"); err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct {
		ch     Channel
		p      Principal
		decide bool
	}{
		{a.general, a.owner, true}, {a.general, a.alice, true}, {a.belgium, a.alice, true}, {a.belgium, a.bob, true},
	} {
		d := m.decide
		if _, err := s.PutChannelMember(ctx, "system", m.ch.ID, m.p.ID, 0, &d); err != nil {
			t.Fatal(err)
		}
	}
	guest := func(label string, ch Channel) Principal {
		g, err := s.CreateGuest(ctx, "system", NewGuest{Label: label, Origin: schema.GuestOriginAddress, Channels: []int64{ch.ID}, Idle: time.Hour, Absolute: 2 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return g.Principal
	}
	a.converted = guest("to-convert", a.general)
	a.ended = guest("to-end", a.general)
	a.activeBelg = guest("live-belgium", a.belgium)
	a.activeGen = guest("live-general", a.general)
	if _, err := s.EndGuestVisit(ctx, "system", a.ended.ID, schema.GuestEndOperator); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	personal := schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"general"}}
	open := schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"belgium"}}
	for _, req := range []schema.CreateInviteRequestV1{personal, open} {
		resp, err := s.CreateInvite(ctx, "system", a.owner.ID, req, InviteOptions{}, now)
		if err != nil {
			t.Fatal(err)
		}
		a.accountInvites = append(a.accountInvites, resp.Invite.ID)
	}
	for _, g := range []struct {
		ch   string
		mode schema.InviteJoinMode
	}{{"belgium", schema.InviteJoinGuest}, {"general", schema.InviteJoinGuest}, {"general", schema.InviteJoinBoth}} {
		// As CreateInvite made it before #1005: join_as set underneath.
		resp, err := s.CreateInvite(ctx, "system", a.owner.ID, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{g.ch}}, InviteOptions{}, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec("UPDATE invites SET join_as = ? WHERE id = ?", string(g.mode), resp.Invite.ID); err != nil {
			t.Fatal(err)
		}
		a.guestInvites = append(a.guestInvites, resp.Invite.ID)
	}
	for _, stmt := range []string{
		// The converted guest, as ConvertGuest leaves its visit.
		fmt.Sprintf("UPDATE guest_visits SET state = 'converted', converted_at = 1 WHERE principal_id = %d", a.converted.ID),
		// Bounds on two members' memberships, and voice and links off for
		// members, the ended guest and carol.
		fmt.Sprintf("UPDATE channel_members SET history_after = 1000 WHERE (channel_id, principal_id) IN ((%d, %d), (%d, %d))",
			a.general.ID, a.alice.ID, a.belgium.ID, a.bob.ID),
		"UPDATE principals SET voice_allowed = 0, may_invite = 0 WHERE kind = 'human'",
		fmt.Sprintf("UPDATE principals SET disabled_at = 1 WHERE id = %d", a.carol.ID),
		fmt.Sprintf("INSERT INTO open_channels (channel_id, added_by, added_at) VALUES (%d, %d, 0)", a.general.ID, a.owner.ID),
		`INSERT INTO server_settings (key, value, updated_at) VALUES ('guests_enabled', 'true', 0), ('open_joining', 'both', 0), ('join_voice', 'false', 0)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rewindToBeforeCommunity(t, path)
	return a
}

// TestCommunityMigration: migration 40 converts a small alpha as design §6
// says, in one transaction, and opening it again changes nothing.
func TestCommunityMigration(t *testing.T) {
	ctx := context.Background()
	a := newCommunityAlpha(t)
	s := openTestStoreAt(t, a.path)
	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < communityMigration {
		t.Fatalf("user_version = %d, %v", version, err)
	}

	// Steps 1 to 3: #general is a public default; #belgium private.
	gen, err := s.ChannelByID(ctx, a.general.ID)
	if err != nil || gen.Visibility != schema.ChannelPublic || !gen.IsDefault {
		t.Errorf("general = %+v, %v; want public and default", gen, err)
	}
	belg, err := s.ChannelByID(ctx, a.belgium.ID)
	if err != nil || belg.Visibility != schema.ChannelPrivate || belg.IsDefault {
		t.Errorf("belgium = %+v, %v; want private, not default", belg, err)
	}
	if n := count("SELECT COUNT(*) FROM channel_members WHERE channel_id = ?", a.belgium.ID); n != 2 {
		t.Errorf("belgium members = %d, want alice and bob only", n)
	}

	// Step 4: both active visits ended through End visit, audited, rows kept.
	for _, g := range []Principal{a.activeBelg, a.activeGen} {
		v, err := s.GuestVisitOf(ctx, g.ID)
		if err != nil || v.State != schema.GuestStateEnded || v.EndReason != schema.GuestEndOperator {
			t.Errorf("visit of %s = %+v, %v; want ended by an operator", g.Name, v, err)
		}
		if n := count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ?", g.ID); n != 0 {
			t.Errorf("%s keeps %d memberships", g.Name, n)
		}
		if n := count("SELECT COUNT(*) FROM credentials WHERE principal_id = ? AND revoked_at IS NULL", g.ID); n != 0 {
			t.Errorf("%s keeps %d live credentials", g.Name, n)
		}
		if n := count("SELECT COUNT(*) FROM audit_events WHERE action = ? AND actor = ? AND subject = ?", AuditGuestEnded, migrationActor, principalActor(g.ID)); n != 1 {
			t.Errorf("%s: %d guest_ended rows by the migration, want 1", g.Name, n)
		}
	}
	if n := count("SELECT COUNT(*) FROM guest_visits"); n != 4 {
		t.Errorf("guest_visits rows = %d, want all 4 kept", n)
	}

	// Step 4: every guest-admitting invite closed, reason guests_removed;
	// the account invites stay open; no invite row deleted.
	for _, id := range a.guestInvites {
		if n := count("SELECT COUNT(*) FROM invites WHERE id = ? AND closed_at IS NOT NULL", id); n != 1 {
			t.Errorf("guest invite %d is open", id)
		}
		if n := count("SELECT COUNT(*) FROM audit_events WHERE action = ? AND subject = ? AND detail LIKE ?", AuditInviteClosed, inviteSubject(id), "%reason=guests_removed%"); n != 1 {
			t.Errorf("guest invite %d: %d invite_closed rows with reason guests_removed, want 1", id, n)
		}
	}
	for _, id := range a.accountInvites {
		if n := count("SELECT COUNT(*) FROM invites WHERE id = ? AND closed_at IS NULL", id); n != 1 {
			t.Errorf("account invite %d was closed", id)
		}
	}
	if n := count("SELECT COUNT(*) FROM invites"); n != 5 {
		t.Errorf("invites rows = %d, want 5", n)
	}

	// Step 5: the members' bounds kept and cleared.
	if n := count("SELECT COUNT(*) FROM channel_members WHERE history_after IS NOT NULL AND principal_id IN (?, ?)", a.alice.ID, a.bob.ID); n != 0 {
		t.Errorf("%d member bounds left", n)
	}
	if n := count("SELECT COUNT(*) FROM legacy_history_bounds WHERE history_after = 1000"); n != 2 {
		t.Errorf("legacy_history_bounds rows = %d, want alice's and bob's", n)
	}

	// Step 6: P is in #general, new rows without the right and without a
	// bound; rows already there untouched (O12); guests left out. The
	// converted guest was in #general already, so two rows are new (bob's
	// and carol's), and its bound was one of the three cleared.
	members, err := s.ListChannelMembers(ctx, a.general.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]ChannelMember{}
	for _, m := range members {
		byID[m.PrincipalID] = m
	}
	for _, p := range []Principal{a.owner, a.alice, a.bob, a.carol, a.converted} {
		m, ok := byID[p.ID]
		if !ok {
			t.Errorf("%s is not in general", p.Name)
			continue
		}
		wantDecide := p.ID == a.owner.ID || p.ID == a.alice.ID
		if m.CanDecide != wantDecide || m.HistoryAfter != 0 {
			t.Errorf("%s in general = %+v; want can_decide %v and no bound", p.Name, m, wantDecide)
		}
	}
	for _, g := range []Principal{a.ended, a.activeBelg, a.activeGen} {
		if in, err := s.IsChannelMember(ctx, a.general.ID, g.ID); err != nil || in {
			t.Errorf("%s in general = %v, %v; want not", g.Name, in, err)
		}
	}

	// Step 7: voice and links on for P, carol included; the guests kept.
	for _, p := range []Principal{a.owner, a.alice, a.bob, a.carol, a.converted} {
		got, err := s.PrincipalByID(ctx, p.ID)
		if err != nil || !got.VoiceAllowed || !got.MayInvite {
			t.Errorf("%s = %+v, %v; want voice and links on", p.Name, got, err)
		}
	}
	for _, g := range []Principal{a.ended, a.activeBelg, a.activeGen} {
		got, err := s.PrincipalByID(ctx, g.ID)
		if err != nil || got.VoiceAllowed || got.MayInvite {
			t.Errorf("guest %s = %+v, %v; want voice and links unchanged (off)", g.Name, got, err)
		}
	}

	// Step 8: every invite is legacy, and link takes only its three values.
	if n := count("SELECT COUNT(*) FROM invites WHERE link <> 'legacy'"); n != 0 {
		t.Errorf("%d invites are not legacy", n)
	}
	if _, err := s.db.Exec("UPDATE invites SET link = 'bogus'"); err == nil {
		t.Error("invites.link took a value outside legacy, member and guest")
	}

	// Step 9: the settings mapped.
	set, _, err := s.JoinSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if set.GuestsEnabled || set.OpenJoining != schema.OpenJoiningAccount || set.OpenSignup == nil || !*set.OpenSignup ||
		set.MemberLinkDefaultExpirySeconds == nil || *set.MemberLinkDefaultExpirySeconds != schema.DefaultMemberLinkExpirySeconds ||
		!set.JoinVoice || len(set.OpenChannels) != 1 || set.OpenChannels[0] != "general" {
		t.Errorf("join settings = %+v", set)
	}
	if n := count("SELECT COUNT(*) FROM server_settings WHERE key IN ('open_joining', 'join_voice')"); n != 0 {
		t.Errorf("%d retired settings rows left", n)
	}
	if n := count("SELECT COUNT(*) FROM server_settings WHERE key = 'guests_enabled' AND value = 'false'"); n != 1 {
		t.Error("guests_enabled is not false")
	}
	on := true
	if _, err := s.PutJoinSettings(ctx, "system", 0, schema.PutJoinSettingsRequestV1{GuestsEnabled: &on}); !errors.Is(err, ErrJoinPolicyConflict) {
		t.Errorf("turning guests on = %v, want the join policy conflict", err)
	}

	// Step 10: exactly one community_migrated row, with the counts.
	events := allAuditEvents(t, s)
	var mig []AuditEvent
	for _, e := range events {
		if e.Action == AuditCommunityMigrated {
			mig = append(mig, e)
		}
	}
	const wantDetail = "public_defaults=1 visits_ended=2 invites_closed=3 bounds_cleared=3 memberships_added=2 principals_flipped=5 invites_legacy=5 open_signup=true guests_enabled=false"
	if len(mig) != 1 || mig[0].Actor != migrationActor || mig[0].Subject != "server" || mig[0].Detail != wantDetail {
		t.Errorf("community_migrated rows = %+v, want one with %q", mig, wantDetail)
	}

	// The member predicate: the converted guest is a member, the ended one
	// is not.
	for _, c := range []struct {
		p    Principal
		want bool
	}{{a.converted, true}, {a.ended, false}, {a.activeBelg, false}, {a.alice, true}} {
		if got, err := isMemberTx(ctx, s.db, c.p.ID); err != nil || got != c.want {
			t.Errorf("isMemberTx(%s) = %v, %v; want %v", c.p.Name, got, err, c.want)
		}
	}

	// Enabling carol afterwards changes nothing but disabled_at.
	if _, err := s.EnablePrincipal(ctx, "system", a.carol.ID); err != nil {
		t.Fatal(err)
	}
	carol, err := s.PrincipalByID(ctx, a.carol.ID)
	if err != nil || carol.DisabledAt != nil || !carol.VoiceAllowed || !carol.MayInvite {
		t.Errorf("carol after enable = %+v, %v", carol, err)
	}
	if in, err := s.IsChannelMember(ctx, a.general.ID, a.carol.ID); err != nil || !in {
		t.Errorf("carol in general after enable = %v, %v", in, err)
	}

	// Opening it again runs nothing more.
	before := len(events)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTestStoreAt(t, a.path)
	if after := len(allAuditEvents(t, s)); after != before+1 {
		// +1: carol's principal_enabled row above.
		t.Errorf("audit rows after reopening = %d, want %d", after, before+1)
	}
}

// The two triggers refuse a private default channel, on insert and on
// update, and the store's own write makes a default channel public.
func TestDefaultChannelMustBePublic(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	ch, err := s.CreateChannel(ctx, "lobby")
	if err != nil {
		t.Fatal(err)
	}
	if ch.Visibility != schema.ChannelPrivate || ch.IsDefault {
		t.Fatalf("a new channel = %+v, want private and not default", ch)
	}
	for _, stmt := range []string{
		"INSERT INTO channels (name, name_key, name_skel, created_at, visibility, is_default) VALUES ('x', 'x', 'x', 0, 'private', 1)",
		fmt.Sprintf("UPDATE channels SET is_default = 1 WHERE id = %d", ch.ID),
	} {
		_, err := s.db.Exec(stmt)
		if err == nil || !strings.Contains(err.Error(), "a default channel must be public") {
			t.Errorf("%s = %v, want the trigger's refusal", stmt, err)
		}
	}
	if madePublic, err := setDefaultChannelTx(ctx, s.db, ch.ID, true, 0); err != nil || !madePublic {
		t.Fatalf("marking a private channel default = %v, %v; want it made public", madePublic, err)
	}
	if madePublic, err := setDefaultChannelTx(ctx, s.db, ch.ID, true, 0); err != nil || madePublic {
		t.Fatalf("marking it again = %v, %v; want nothing made public", madePublic, err)
	}
	if _, err := s.db.Exec("UPDATE channels SET visibility = 'private' WHERE id = ?", ch.ID); err == nil {
		t.Error("a default channel was made private")
	}
	got, err := s.ChannelByID(ctx, ch.ID)
	if err != nil || got.Visibility != schema.ChannelPublic || !got.IsDefault {
		t.Errorf("after marking default = %+v, %v; want public and default", got, err)
	}
}

// isMemberTx is design §1.1's member predicate.
func TestIsMemberTx(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	mk := func(kind PrincipalKind, name string) Principal {
		p, err := s.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	enabled := mk(PrincipalHuman, "enabled")
	disabled := mk(PrincipalHuman, "disabled")
	hidden := mk(PrincipalHuman, "hidden")
	agent := mk(PrincipalAgent, "agent")
	ch, err := s.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	guest := func(label string) Principal {
		g, err := s.CreateGuest(ctx, "system", NewGuest{Label: label, Origin: schema.GuestOriginAddress, Channels: []int64{ch.ID}, Idle: time.Hour, Absolute: 2 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		return g.Principal
	}
	active, ended, converted := guest("active"), guest("ended"), guest("converted")
	if _, err := s.EndGuestVisit(ctx, "system", ended.ID, schema.GuestEndOperator); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		fmt.Sprintf("UPDATE principals SET disabled_at = 1 WHERE id = %d", disabled.ID),
		fmt.Sprintf("UPDATE principals SET disabled_at = 1, hidden_at = 1 WHERE id = %d", hidden.ID),
		fmt.Sprintf("UPDATE guest_visits SET state = 'converted', converted_at = 1 WHERE principal_id = %d", converted.ID),
	} {
		if _, err := s.db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		name string
		id   int64
		want bool
	}{
		{"an enabled human", enabled.ID, true},
		{"a disabled human", disabled.ID, false},
		{"a hidden human", hidden.ID, false},
		{"an active guest", active.ID, false},
		{"an ended guest", ended.ID, false},
		{"a converted guest", converted.ID, true},
		{"an agent", agent.ID, false},
		{"nobody", 9999, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := isMemberTx(ctx, s.db, c.id)
			if err != nil || got != c.want {
				t.Errorf("isMemberTx = %v, %v; want %v", got, err, c.want)
			}
		})
	}
}

// Listing a private channel in the joining settings makes it public and a
// default (until I7 refuses it), and its open_channel_added row says
// visibility=private->public; listing one already public says nothing more,
// and unlisting leaves it public (security review SF1, issue #979).
func TestListingAChannelAuditsItsVisibility(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	red, err := s.CreateChannel(ctx, "red")
	if err != nil {
		t.Fatal(err)
	}
	account := schema.OpenJoiningAccount
	put := func(names ...string) {
		t.Helper()
		if _, err := s.PutJoinSettings(ctx, "system", 0, schema.PutJoinSettingsRequestV1{OpenJoining: &account, OpenChannels: &names}); err != nil {
			t.Fatal(err)
		}
	}
	added := func() []string {
		t.Helper()
		var out []string
		for _, e := range allAuditEvents(t, s) {
			if e.Action == AuditOpenChannelAdded && e.Subject == channelSubject(red.ID) {
				out = append(out, e.Detail)
			}
		}
		return out
	}
	if _, err := s.CreateChannel(ctx, "blue"); err != nil {
		t.Fatal(err)
	}
	put("red")
	if got := added(); len(got) != 1 || got[0] != "visibility=private->public" {
		t.Fatalf("open_channel_added details = %q, want one visibility=private->public", got)
	}
	put("blue")
	if ch, err := s.ChannelByID(ctx, red.ID); err != nil || ch.Visibility != schema.ChannelPublic || ch.IsDefault {
		t.Fatalf("red after unlisting = %+v, %v; want public, not default", ch, err)
	}
	put("red", "blue")
	if got := added(); len(got) != 2 || got[1] != "" {
		t.Fatalf("open_channel_added details = %q, want the second with no detail", got)
	}
}
