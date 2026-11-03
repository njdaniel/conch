package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// makeDefault makes channel ids public default channels, as an operator's
// PUT /v1/channels/{id}/settings does (a default channel is public).
func (f *inviteFixture) makeDefault(ids ...int64) {
	f.t.Helper()
	for _, id := range ids {
		if _, err := f.s.db.ExecContext(f.ctx, "UPDATE channels SET visibility = 'public', is_default = 1 WHERE id = ?", id); err != nil {
			f.t.Fatal(err)
		}
	}
}

// joinerRow is what redemption stored for a joiner: its principal's switches
// and maker, and each membership as channel:can_decide:history_after
// (-1 for none):added_by, by channel id.
type joinerRow struct {
	voice, mayInvite int
	invitedBy        int64
	memberships      []string
}

func (f *inviteFixture) joiner(id int64) joinerRow {
	f.t.Helper()
	var r joinerRow
	if err := f.s.db.QueryRowContext(f.ctx, "SELECT voice_allowed, may_invite, invited_by FROM principals WHERE id = ?", id).
		Scan(&r.voice, &r.mayInvite, &r.invitedBy); err != nil {
		f.t.Fatal(err)
	}
	rows, err := f.s.db.QueryContext(f.ctx,
		`SELECT channel_id, can_decide, COALESCE(history_after, -1), COALESCE(added_by, 0) FROM channel_members
		 WHERE principal_id = ? ORDER BY channel_id`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	r.memberships = []string{}
	for rows.Next() {
		var ch, decide, after, by int64
		if err := rows.Scan(&ch, &decide, &after, &by); err != nil {
			f.t.Fatal(err)
		}
		r.memberships = append(r.memberships, fmt.Sprintf("%d:%d:%d:%d", ch, decide, after, by))
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// Redemption follows the community model (issue #986, design §2.1): a member
// link lands its joiner in the default channels as they are at that moment,
// which may be none; a legacy invite in its own channels its maker still
// stands behind and also the defaults. Every joiner has voice and the
// may_invite brake on, no history bound, no right to decide approvals, and
// names its maker.
func TestRedemptionLandsInTheDefaultChannels(t *testing.T) {
	type made struct {
		code  string
		maker func(*inviteFixture) Principal
		own   func(*inviteFixture) []int64
	}
	links := []struct {
		name string
		make func(*inviteFixture) made
	}{
		{"an operator's member link", func(f *inviteFixture) made {
			return made{f.mustLink(f.op, memberLink()).Code, func(f *inviteFixture) Principal { return f.op }, func(*inviteFixture) []int64 { return nil }}
		}},
		{"a member's member link", func(f *inviteFixture) made {
			return made{f.mustLink(f.alice, memberLink()).Code, func(f *inviteFixture) Principal { return f.alice }, func(*inviteFixture) []int64 { return nil }}
		}},
		{"a legacy personal invite", func(f *inviteFixture) made {
			return made{f.mustCreate(f.alice, personal("general")).Code, func(f *inviteFixture) Principal { return f.alice },
				func(f *inviteFixture) []int64 { return []int64{f.general.ID} }}
		}},
		{"a legacy open invite", func(f *inviteFixture) made {
			return made{f.mustCreate(f.op, open("lobby")).Code, func(f *inviteFixture) Principal { return f.op },
				func(f *inviteFixture) []int64 { return []int64{f.lobby.ID} }}
		}},
	}
	for _, l := range links {
		for defaults := 0; defaults <= 2; defaults++ {
			t.Run(fmt.Sprintf("%s, %d default channels", l.name, defaults), func(t *testing.T) {
				f := newInviteFixture(t)
				town, err := f.s.CreateChannel(f.ctx, "town")
				if err != nil {
					t.Fatal(err)
				}
				square, err := f.s.CreateChannel(f.ctx, "square")
				if err != nil {
					t.Fatal(err)
				}
				m := l.make(f)
				// The defaults are read at redemption, not at creation.
				f.makeDefault([]int64{town.ID, square.ID}[:defaults]...)
				before := len(f.audit())
				got, err := f.redeem(m.code, "joiner")
				if err != nil {
					t.Fatalf("redeem: %v", err)
				}
				maker := m.maker(f)
				want := slices.Concat(m.own(f), []int64{town.ID, square.ID}[:defaults])
				slices.Sort(want)
				wantRows := []string{}
				for _, id := range want {
					wantRows = append(wantRows, fmt.Sprintf("%d:0:-1:%d", id, maker.ID))
				}
				row := f.joiner(got.Principal.ID)
				if row.voice != 1 || row.mayInvite != 1 || row.invitedBy != maker.ID {
					t.Errorf("joiner = %+v; want voice and the brake on, invited by %d", row, maker.ID)
				}
				if !slices.Equal(row.memberships, wantRows) {
					t.Errorf("memberships = %v, want %v (no right to decide, no bound, added by the maker)", row.memberships, wantRows)
				}
				added := 0
				for _, e := range f.audit()[before:] {
					if e.Action == AuditMemberAdded {
						added++
					}
				}
				if added != len(want) {
					t.Errorf("member_added rows = %d, want %d", added, len(want))
				}
			})
		}
	}
}

// A legacy invite keeps its own channels by today's rules, whatever their
// visibility has become, and is invite_invalid when none of them is left,
// however many default channels there are: it does not silently become a
// member link its maker never made. A legacy invite that admitted guests,
// closed by the community migration, does not redeem.
func TestLegacyInviteRedemptionEdges(t *testing.T) {
	t.Run("its channel turned private before redemption", func(t *testing.T) {
		f := newInviteFixture(t)
		f.makeDefault(f.lobby.ID)
		f.makeDefault(f.general.ID)
		inv := f.mustCreate(f.alice, personal("general"))
		if _, err := f.s.db.ExecContext(f.ctx, "UPDATE channels SET is_default = 0, visibility = 'private' WHERE id = ?", f.general.ID); err != nil {
			t.Fatal(err)
		}
		got, err := f.redeem(inv.Code, "joiner")
		if err != nil {
			t.Fatalf("redeem: %v", err)
		}
		want := []string{fmt.Sprintf("%d:0:-1:%d", f.general.ID, f.alice.ID), fmt.Sprintf("%d:0:-1:%d", f.lobby.ID, f.alice.ID)}
		slices.Sort(want)
		if row := f.joiner(got.Principal.ID); !slices.Equal(row.memberships, want) {
			t.Errorf("memberships = %v, want the private channel it named and the default %v", row.memberships, want)
		}
	})
	t.Run("none of its own channels left", func(t *testing.T) {
		f := newInviteFixture(t)
		town, err := f.s.CreateChannel(f.ctx, "town")
		if err != nil {
			t.Fatal(err)
		}
		f.makeDefault(town.ID)
		inv := f.mustCreate(f.alice, personal("lobby"))
		if _, err := f.s.db.ExecContext(f.ctx, "DELETE FROM channel_members WHERE channel_id = ? AND principal_id = ?", f.lobby.ID, f.alice.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.redeem(inv.Code, "joiner"); !errors.Is(err, ErrInviteInvalid) {
			t.Errorf("redeem = %v, want ErrInviteInvalid although a default channel exists", err)
		}
		if n := f.count("SELECT COUNT(*) FROM principals WHERE name = 'joiner'"); n != 0 {
			t.Errorf("a refused redemption made %d principals", n)
		}
	})
	for _, mode := range []schema.InviteJoinMode{schema.InviteJoinGuest, schema.InviteJoinBoth} {
		t.Run(fmt.Sprintf("a closed legacy invite that admitted %s", mode), func(t *testing.T) {
			f := newInviteFixture(t)
			req := personal("general")
			req.JoinAs = mode
			inv := f.mustCreate(f.op, req)
			// The community migration closed it (I2, issue #979).
			if _, err := f.s.db.ExecContext(f.ctx, "UPDATE invites SET closed_at = ? WHERE id = ?", f.now.UnixMilli(), inv.Invite.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.redeem(inv.Code, "joiner"); !errors.Is(err, ErrInviteInvalid) {
				t.Errorf("redeem = %v, want ErrInviteInvalid", err)
			}
		})
	}
}

// The v1 routes project a member link for old clients (design §3.3, issue
// #986): GET /v1/invites and POST /v1/invites/preview tell it as an open
// invite into the default channels as they are now, with voice, no right to
// invite passed on, joining as an account, and its expiry or the
// NoExpiryMillis sentinel. Golden answers, field for field.
func TestMemberLinkV1Projection(t *testing.T) {
	f := newInviteFixture(t)
	town, err := f.s.CreateChannel(f.ctx, "town")
	if err != nil {
		t.Fatal(err)
	}
	f.makeDefault(f.general.ID, town.ID)
	expiring := f.mustLink(f.op, memberLink())
	if expiring.Invite.ExpiresAt == nil {
		t.Fatal("the default link has no expiry")
	}
	never := memberLink()
	never.NeverExpires = true
	forever := f.mustLink(f.op, never)

	list, err := f.s.ListInvites(f.ctx, InviteFilter{CreatedBy: f.op.ID}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]schema.InviteV1{}
	for _, inv := range list {
		got[inv.ID] = inv
	}
	channels := []schema.InviteChannelV1{{ID: f.general.ID, Name: "general"}, {ID: town.ID, Name: "town"}}
	lifetime := int64(schema.DefaultInviteCredentialLifetimeSeconds)
	for _, tt := range []struct {
		name    string
		link    schema.CreateInviteResponseV2
		expires time.Time
	}{
		{"a member link", expiring, expiring.Invite.ExpiresAt.Time()},
		{"a member link that never expires", forever, time.UnixMilli(schema.NoExpiryMillis)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := schema.InviteV1{
				ID: tt.link.Invite.ID, Kind: schema.InviteKindOpen, CreatedBy: f.op.ID,
				CreatedAt: schema.NewTimestamp(f.now), ExpiresAt: schema.NewTimestamp(tt.expires),
				Channels: channels, Voice: true, MayInvite: false,
				CredentialLifetimeSeconds: lifetime, JoinAs: schema.InviteJoinAccount,
			}
			if g, w := mustJSON(t, got[tt.link.Invite.ID]), mustJSON(t, want); g != w {
				t.Errorf("GET /v1/invites projects\n %s\nwant\n %s", g, w)
			}
			pv, err := f.s.PreviewInvite(f.ctx, HashInviteCode(tt.link.Code), f.now)
			if err != nil {
				t.Fatal(err)
			}
			wantPreview := schema.InvitePreviewV1{
				Kind: schema.InviteKindOpen, Inviter: "root", Channels: []string{"general", "town"}, Voice: true,
				ExpiresAt: schema.NewTimestamp(tt.expires), Join: &schema.JoinOptionsV1{Account: true},
			}
			if g, w := mustJSON(t, pv), mustJSON(t, wantPreview); g != w {
				t.Errorf("POST /v1/invites/preview projects\n %s\nwant\n %s", g, w)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
