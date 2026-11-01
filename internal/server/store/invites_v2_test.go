package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Member links (issue #985, design §2.1 and §3.3). The fixture is the invite
// fixture of invites_test.go: root the owner-operator; alice a member who may
// invite, in general and lobby; bob a member whose may_invite brake is off, in
// general; "private" a channel nobody but its creator's default membership
// reaches.

func memberLink() schema.CreateInviteRequestV2 {
	return schema.CreateInviteRequestV2{Kind: schema.InviteKindMember}
}

func (f *inviteFixture) createLink(creator Principal, req schema.CreateInviteRequestV2) (schema.CreateInviteResponseV2, error) {
	f.t.Helper()
	return f.s.CreateMemberLink(f.ctx, principalActor(creator.ID), creator.ID, req, InviteOptions{}, f.now)
}

func (f *inviteFixture) mustLink(creator Principal, req schema.CreateInviteRequestV2) schema.CreateInviteResponseV2 {
	f.t.Helper()
	resp, err := f.createLink(creator, req)
	if err != nil {
		f.t.Fatalf("CreateMemberLink: %v", err)
	}
	return resp
}

// setExpiryDefault writes the member_link_default_expiry_seconds setting.
func (f *inviteFixture) setExpiryDefault(v string) {
	f.t.Helper()
	if _, err := f.s.db.Exec(
		`INSERT INTO server_settings (key, value, updated_at) VALUES ('member_link_default_expiry_seconds', ?, 0)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, v); err != nil {
		f.t.Fatal(err)
	}
}

// stored reads the columns a member link is told apart by.
func (f *inviteFixture) stored(id int64) (kind, link, joinAs string, voice, mayInvite bool, expires int64) {
	f.t.Helper()
	if err := f.s.db.QueryRow(`SELECT kind, link, join_as, voice, may_invite, expires_at FROM invites WHERE id = ?`, id).
		Scan(&kind, &link, &joinAs, &voice, &mayInvite, &expires); err != nil {
		f.t.Fatal(err)
	}
	return
}

// operator2 is a second operator who is not an owner.
func (f *inviteFixture) operator2() Principal {
	f.t.Helper()
	p, err := f.s.CreatePrincipal(f.ctx, PrincipalHuman, "operator2")
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.s.SetRole(f.ctx, "system", f.op.ID, p.ID, RoleOperator, nil); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func TestCreateMemberLink(t *testing.T) {
	week := time.Duration(schema.DefaultMemberLinkExpirySeconds) * time.Second
	tests := []struct {
		name    string
		setup   func(*inviteFixture)
		creator func(*inviteFixture) Principal
		req     func(*inviteFixture) schema.CreateInviteRequestV2
		wantErr error
		// wantPerm is the permission a refusal names.
		wantPerm schema.Permission
		// wantExpires is how far past now the link expires; 0 is never.
		wantExpires time.Duration
	}{
		{"member with the brake on", nil, func(f *inviteFixture) Principal { return f.alice },
			func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, nil, "", week},
		{"operator", nil, func(f *inviteFixture) Principal { return f.op },
			func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, nil, "", week},
		{"member with the brake off", nil, func(f *inviteFixture) Principal { return f.bob },
			func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, ErrInviteNotAllowed, schema.PermMakeMemberLinks, 0},
		{"disabled", func(f *inviteFixture) {
			if _, err := f.s.DisablePrincipal(f.ctx, "system", f.alice.ID); err != nil {
				f.t.Fatal(err)
			}
		}, func(f *inviteFixture) Principal { return f.alice },
			func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, ErrInviteNotAllowed, "", 0},
		{"an agent", nil, func(f *inviteFixture) Principal {
			p, err := f.s.CreatePrincipal(f.ctx, PrincipalAgent, "bot")
			if err != nil {
				f.t.Fatal(err)
			}
			return p
		}, func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, ErrInviteNotAllowed, "", 0},
		{"a guest", nil, func(f *inviteFixture) Principal {
			g, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
				Channels: []int64{f.general.ID}, Idle: time.Hour, Absolute: time.Hour})
			if err != nil {
				f.t.Fatal(err)
			}
			return g.Principal
		}, func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, ErrInviteNotAllowed, "", 0},
		{"member, never_expires", nil, func(f *inviteFixture) Principal { return f.alice },
			func(*inviteFixture) schema.CreateInviteRequestV2 {
				r := memberLink()
				r.NeverExpires = true
				return r
			}, ErrInviteNotAllowed, schema.PermManageMembers, 0},
		{"operator, never_expires", nil, func(f *inviteFixture) Principal { return f.op },
			func(*inviteFixture) schema.CreateInviteRequestV2 {
				r := memberLink()
				r.NeverExpires = true
				return r
			}, nil, "", 0},
		{"non-owner operator, never_expires", nil, func(f *inviteFixture) Principal { return f.operator2() },
			func(*inviteFixture) schema.CreateInviteRequestV2 {
				r := memberLink()
				r.NeverExpires = true
				return r
			}, nil, "", 0},
		{"the default expiry setting is 0: none", func(f *inviteFixture) { f.setExpiryDefault("0") }, func(f *inviteFixture) Principal { return f.alice },
			func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, nil, "", 0},
		{"the default expiry setting is an hour", func(f *inviteFixture) { f.setExpiryDefault("3600") }, func(f *inviteFixture) Principal { return f.alice },
			func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, nil, "", time.Hour},
		{"an unreadable default expiry setting is the default", func(f *inviteFixture) { f.setExpiryDefault("soon") }, func(f *inviteFixture) Principal { return f.alice },
			func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, nil, "", week},
		{"a default expiry setting past the bound is the default", func(f *inviteFixture) { f.setExpiryDefault("999999999") }, func(f *inviteFixture) Principal { return f.alice },
			func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, nil, "", week},
		{"an expiry named", nil, func(f *inviteFixture) Principal { return f.alice },
			func(f *inviteFixture) schema.CreateInviteRequestV2 {
				r := memberLink()
				ts := schema.NewTimestamp(f.now.Add(3 * time.Hour))
				r.ExpiresAt = &ts
				return r
			}, nil, "", 3 * time.Hour},
		{"an expiry named past the bound", nil, func(f *inviteFixture) Principal { return f.op },
			func(f *inviteFixture) schema.CreateInviteRequestV2 {
				r := memberLink()
				ts := schema.NewTimestamp(f.now.Add(time.Duration(schema.MaxMemberLinkExpirySeconds+60) * time.Second))
				r.ExpiresAt = &ts
				return r
			}, ErrInviteRequestInvalid, "", 0},
		{"kind guest", nil, func(f *inviteFixture) Principal { return f.op },
			func(*inviteFixture) schema.CreateInviteRequestV2 {
				return schema.CreateInviteRequestV2{Kind: "guest"}
			}, ErrInviteRequestInvalid, "", 0},
		{"kind personal", nil, func(f *inviteFixture) Principal { return f.op },
			func(*inviteFixture) schema.CreateInviteRequestV2 {
				return schema.CreateInviteRequestV2{Kind: "personal"}
			}, ErrInviteRequestInvalid, "", 0},
		{"paused", func(f *inviteFixture) {
			if _, err := f.s.SetInvitesPaused(f.ctx, "system", true); err != nil {
				f.t.Fatal(err)
			}
		}, func(f *inviteFixture) Principal { return f.op },
			func(*inviteFixture) schema.CreateInviteRequestV2 { return memberLink() }, ErrInvitesPaused, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			creator := tt.creator(f)
			resp, err := f.createLink(creator, tt.req(f))
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				var pe *PermissionError
				if tt.wantPerm != "" && (!errors.As(err, &pe) || pe.Permission != tt.wantPerm) {
					t.Errorf("error = %#v, want a PermissionError naming %s", err, tt.wantPerm)
				}
				if n := f.count("SELECT COUNT(*) FROM invites"); n != 0 {
					t.Errorf("%d invites after a refusal, want none", n)
				}
				if n := f.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditInviteCreated); n != 0 {
					t.Errorf("%d invite_created rows after a refusal, want none", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("CreateMemberLink: %v", err)
			}
			kind, link, joinAs, voice, mayInvite, expires := f.stored(resp.Invite.ID)
			if kind != "open" || link != "member" || joinAs != "account" || voice || mayInvite {
				t.Errorf("stored kind=%s link=%s join_as=%s voice=%t may_invite=%t, want open member account false false", kind, link, joinAs, voice, mayInvite)
			}
			if tt.wantExpires == 0 {
				if expires != schema.NoExpiryMillis || resp.Invite.ExpiresAt != nil {
					t.Errorf("stored expires_at %d, wire %v, want the sentinel and none", expires, resp.Invite.ExpiresAt)
				}
			} else {
				want := f.now.Add(tt.wantExpires).UnixMilli()
				if expires != want || resp.Invite.ExpiresAt == nil || resp.Invite.ExpiresAt.Time().UnixMilli() != want {
					t.Errorf("stored expires_at %d, wire %v, want %d", expires, resp.Invite.ExpiresAt, want)
				}
			}
			if resp.Invite.Kind != schema.InviteKindMember || resp.Invite.Legacy || resp.Invite.MaxUses != nil || resp.Invite.CreatedBy != creator.ID {
				t.Errorf("invite = %+v", resp.Invite)
			}
			if err := schema.ValidateInviteCode(resp.Code); err != nil {
				t.Errorf("code: %v", err)
			}
			if f.count("SELECT COUNT(*) FROM invite_channels WHERE invite_id = ?", resp.Invite.ID) != 0 {
				t.Errorf("a member link with no landing channels has channel rows")
			}
			rows := 0
			for _, e := range f.audit() {
				if e.Action == AuditInviteCreated {
					rows++
					if strings.Contains(e.Detail, resp.Code) || strings.Contains(e.Detail, HashInviteCode(resp.Code)) {
						t.Errorf("the code or its hash is in the audit row: %s", e.Detail)
					}
				}
			}
			if rows != 1 {
				t.Errorf("%d invite_created rows, want 1", rows)
			}
		})
	}
	t.Run("no link is ever written with link guest", func(t *testing.T) {
		f := newInviteFixture(t)
		_, _ = f.createLink(f.op, schema.CreateInviteRequestV2{Kind: "guest"})
		if n := f.count("SELECT COUNT(*) FROM invites WHERE link = 'guest'"); n != 0 {
			t.Errorf("%d guest links", n)
		}
	})
}

// Landing channels (O5): only a holder of manage_channels names them, and a
// caller without it gets one answer for a private channel, a public one and
// one that does not exist, decided before any lookup.
func TestCreateMemberLinkLandingChannels(t *testing.T) {
	land := func(names ...string) schema.CreateInviteRequestV2 {
		r := memberLink()
		r.LandingChannels = names
		return r
	}
	f := newInviteFixture(t)
	// A member, in general, naming a channel in or out of their reach.
	var texts []string
	for _, name := range []string{"general", "lobby", "private", "nosuch"} {
		_, err := f.createLink(f.alice, land(name))
		if !errors.Is(err, ErrInviteGrantExceedsInviter) {
			t.Fatalf("member naming %q: error = %v, want ErrInviteGrantExceedsInviter", name, err)
		}
		texts = append(texts, err.Error())
	}
	for _, text := range texts[1:] {
		if text != texts[0] {
			t.Errorf("refusals differ by channel: %q and %q", texts[0], text)
		}
	}
	if n := f.count("SELECT COUNT(*) FROM invites"); n != 0 {
		t.Errorf("%d invites after the refusals", n)
	}

	// A holder of manage_channels names any channel, private included, and a
	// name that is no channel is not found.
	resp := f.mustLink(f.op, land("private", "general"))
	if len(resp.Invite.LandingChannels) != 2 || resp.Invite.Channels != nil {
		t.Errorf("landing channels = %+v channels = %+v", resp.Invite.LandingChannels, resp.Invite.Channels)
	}
	if _, err := f.createLink(f.op, land("nosuch")); !errors.Is(err, ErrNotFound) {
		t.Errorf("operator naming no channel: error = %v, want ErrNotFound", err)
	}
	if n := f.count("SELECT COUNT(*) FROM invite_channels WHERE invite_id = ?", resp.Invite.ID); n != 2 {
		t.Errorf("%d channel rows, want 2", n)
	}

	// The link lands where its maker said while the maker still holds
	// manage_channels, and nowhere when it does not (fail closed).
	r, kept, dropped, err := usableInviteTx(f.ctx, f.s.db, HashInviteCode(resp.Code), f.now)
	if err != nil || len(kept) != 2 || len(dropped) != 0 || r.link != "member" {
		t.Fatalf("usable = %v kept %v dropped %v", err, kept, dropped)
	}
	op2 := f.operator2()
	second := f.mustLink(op2, land("private"))
	withhold(t, f, schema.PermManageChannels)
	if _, _, _, err := usableInviteTx(f.ctx, f.s.db, HashInviteCode(second.Code), f.now); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("a landing link whose maker lost manage_channels: error = %v, want ErrInviteInvalid", err)
	}
}

// The allowance counts the maker's open member links, an uncapped one too;
// operators have none.
func TestMemberLinkAllowance(t *testing.T) {
	f := newInviteFixture(t)
	opts := InviteOptions{Allowance: 2}
	create := func(p Principal, req schema.CreateInviteRequestV2) (schema.CreateInviteResponseV2, error) {
		return f.s.CreateMemberLink(f.ctx, "system", p.ID, req, opts, f.now)
	}
	one := int64(1)
	// A legacy personal invite and a legacy open invite of the same kind as a
	// member link do not count, even one the member holds.
	f.mustCreate(f.alice, personal("general"))
	legacyOpen := f.mustCreate(f.op, open("general"))
	if _, err := f.s.db.Exec("UPDATE invites SET created_by = ? WHERE id = ?", f.alice.ID, legacyOpen.Invite.ID); err != nil {
		t.Fatal(err)
	}
	if left, _ := f.s.InvitesRemaining(f.ctx, f.alice.ID, opts, f.now); left != 2 {
		t.Fatalf("remaining with a legacy open invite and a personal one = %d, want 2", left)
	}
	first, err := create(f.alice, memberLink()) // uncapped
	if err != nil {
		t.Fatal(err)
	}
	capped := memberLink()
	capped.MaxUses = &one
	if _, err := create(f.alice, capped); err != nil {
		t.Fatal(err)
	}
	if left, _ := f.s.InvitesRemaining(f.ctx, f.alice.ID, opts, f.now); left != 0 {
		t.Errorf("remaining = %d, want 0", left)
	}
	if _, err := create(f.alice, memberLink()); !errors.Is(err, ErrInviteAllowanceExceeded) {
		t.Fatalf("third: error = %v, want ErrInviteAllowanceExceeded", err)
	}
	// Operators have none.
	for range 3 {
		if _, err := create(f.op, memberLink()); err != nil {
			t.Fatalf("operator: %v", err)
		}
	}
	// A closed one frees a place; so does one used up.
	if _, err := f.s.CloseInvite(f.ctx, "system", f.alice.ID, first.Invite.ID, f.now); err != nil {
		t.Fatal(err)
	}
	if left, _ := f.s.InvitesRemaining(f.ctx, f.alice.ID, opts, f.now); left != 1 {
		t.Errorf("remaining after a close = %d, want 1", left)
	}
	f.legacyOpenInviteCountsNothing(opts)
}

// legacyOpenInviteCountsNothing: an operator's legacy open invite is the same
// kind as a member link and is not counted for anyone.
func (f *inviteFixture) legacyOpenInviteCountsNothing(opts InviteOptions) {
	f.t.Helper()
	before, _ := f.s.InvitesRemaining(f.ctx, f.op.ID, opts, f.now)
	f.mustCreate(f.op, open("general"))
	if after, _ := f.s.InvitesRemaining(f.ctx, f.op.ID, opts, f.now); after != before {
		f.t.Errorf("a legacy open invite changed the remaining count %d -> %d", before, after)
	}
}

// TestInvitesRemainingCountsMemberLinks replaces the personal-invite count
// of the allowance (issue #985): the same table, over member links, with the
// uncapped link counting as open.
func TestInvitesRemainingCountsMemberLinks(t *testing.T) {
	week := 8 * 24 * time.Hour
	openOne := func(f *inviteFixture) { f.mustLink(f.alice, memberLink()) }
	usedOne := func(f *inviteFixture) {
		one := int64(1)
		r := memberLink()
		r.MaxUses = &one
		if _, err := f.redeem(f.mustLink(f.alice, r).Code, "joiner"); err != nil {
			f.t.Fatal(err)
		}
	}
	expiredOne := func(f *inviteFixture) {
		if _, err := f.s.CreateMemberLink(f.ctx, "system", f.alice.ID, memberLink(), InviteOptions{}, f.now.Add(-week)); err != nil {
			f.t.Fatal(err)
		}
	}
	closedOne := func(f *inviteFixture) {
		id := f.mustLink(f.alice, memberLink()).Invite.ID
		if _, err := f.s.CloseInvite(f.ctx, "system", f.alice.ID, id, f.now); err != nil {
			f.t.Fatal(err)
		}
	}
	tests := []struct {
		name      string
		allowance int
		setup     []func(*inviteFixture)
		want      int
	}{
		{"none", 0, nil, DefaultPersonalInviteAllowance},
		{"one open, uncapped", 0, []func(*inviteFixture){openOne}, DefaultPersonalInviteAllowance - 1},
		{"one used", 0, []func(*inviteFixture){usedOne}, DefaultPersonalInviteAllowance},
		{"one expired", 0, []func(*inviteFixture){expiredOne}, DefaultPersonalInviteAllowance},
		{"one closed", 0, []func(*inviteFixture){closedOne}, DefaultPersonalInviteAllowance},
		{"one of each", 0, []func(*inviteFixture){openOne, usedOne, expiredOne, closedOne}, DefaultPersonalInviteAllowance - 1},
		{"legacy invites do not count", 0, []func(*inviteFixture){func(f *inviteFixture) {
			f.mustCreate(f.alice, personal("general"))
			f.mustCreate(f.op, open("general"))
		}}, DefaultPersonalInviteAllowance},
		{"others' links do not count", 0, []func(*inviteFixture){func(f *inviteFixture) { f.mustLink(f.op, memberLink()) }}, DefaultPersonalInviteAllowance},
		{"the allowance is a parameter", 2, []func(*inviteFixture){openOne}, 1},
		{"used up", 2, []func(*inviteFixture){openOne, openOne}, 0},
		{"never below zero when the allowance is lowered", 1, []func(*inviteFixture){openOne, openOne, openOne}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			for _, setup := range tt.setup {
				setup(f)
			}
			opts := InviteOptions{Allowance: tt.allowance}
			got, err := f.s.InvitesRemaining(f.ctx, f.alice.ID, opts, f.now)
			if err != nil || got != tt.want {
				t.Fatalf("InvitesRemaining = %d, %v, want %d", got, err, tt.want)
			}
			_, err = f.s.CreateMemberLink(f.ctx, "system", f.alice.ID, memberLink(), opts, f.now)
			if refused := errors.Is(err, ErrInviteAllowanceExceeded); refused != (tt.want == 0) {
				t.Errorf("CreateMemberLink with %d remaining: error = %v", tt.want, err)
			}
		})
	}
}

// The brake: switching may_invite off closes the maker's open member links
// and personal invites in one transaction, each audited; switching it on
// reopens nothing; an operator's links stand whatever the brake says.
func TestMayInviteBrakeClosesMemberLinks(t *testing.T) {
	t.Run("a member's", func(t *testing.T) {
		f := newInviteFixture(t)
		link := f.mustLink(f.alice, memberLink())
		other := f.mustLink(f.alice, memberLink())
		legacy := f.mustCreate(f.alice, personal("general"))
		bobs := f.mustLink(f.op, memberLink())
		// Already closed: not closed again, no second row.
		if _, err := f.s.CloseInvite(f.ctx, "system", f.alice.ID, other.Invite.ID, f.now); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SetMayInvite(f.ctx, "system", f.alice.ID, false); err != nil {
			t.Fatal(err)
		}
		for _, id := range []int64{link.Invite.ID, legacy.Invite.ID} {
			if !f.closedAt(id) {
				t.Errorf("invite %d is open after the brake went off", id)
			}
		}
		if f.closedAt(bobs.Invite.ID) {
			t.Errorf("the operator's link was closed by another person's brake")
		}
		want := []string{
			fmt.Sprintf("invite:%d reason=creator", other.Invite.ID),
			fmt.Sprintf("invite:%d reason=permission_revoked", link.Invite.ID),
			fmt.Sprintf("invite:%d reason=permission_revoked", legacy.Invite.ID),
		}
		if got := f.closedReasons(); !slices.Equal(got, want) {
			t.Errorf("invite_closed rows = %q, want %q", got, want)
		}
		// On again reopens nothing, and the person may make a new one.
		if _, err := f.s.SetMayInvite(f.ctx, "system", f.alice.ID, true); err != nil {
			t.Fatal(err)
		}
		if !f.closedAt(link.Invite.ID) {
			t.Errorf("switching the brake on reopened a link")
		}
		if _, err := f.createLink(f.alice, memberLink()); err != nil {
			t.Errorf("a new link after the brake came back: %v", err)
		}
	})
	t.Run("an operator's stand", func(t *testing.T) {
		f := newInviteFixture(t)
		op2 := f.operator2()
		link := f.mustLink(op2, memberLink())
		if _, err := f.s.SetMayInvite(f.ctx, "system", op2.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SetMayInvite(f.ctx, "system", op2.ID, false); err != nil {
			t.Fatal(err)
		}
		if f.closedAt(link.Invite.ID) {
			t.Errorf("an operator's link was closed by their brake")
		}
		if got := f.closedReasons(); len(got) != 0 {
			t.Errorf("invite_closed rows = %q, want none", got)
		}
		if _, err := f.s.PreviewMemberLink(f.ctx, HashInviteCode(link.Code), f.now); err != nil {
			t.Errorf("the operator's link with the brake off: %v", err)
		}
		if _, err := f.createLink(op2, memberLink()); err != nil {
			t.Errorf("an operator with the brake off makes a link: %v", err)
		}
	})
}

// Demoting an operator closes the legacy open links that stood on the role
// and leaves their member links standing.
func TestDemotionLeavesMemberLinks(t *testing.T) {
	f := newInviteFixture(t)
	op2 := f.operator2()
	if _, err := f.s.SetMayInvite(f.ctx, "system", op2.ID, true); err != nil {
		t.Fatal(err)
	}
	legacyOpen := f.mustCreate(op2, open("general"))
	link := f.mustLink(op2, memberLink())
	res, err := f.s.SetRole(f.ctx, "system", f.op.ID, op2.ID, RoleMember, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.InvitesClosed != 1 {
		t.Errorf("InvitesClosed = %d, want 1 (the legacy open link)", res.InvitesClosed)
	}
	if !f.closedAt(legacyOpen.Invite.ID) || f.closedAt(link.Invite.ID) {
		t.Errorf("legacy open closed %t, member link closed %t, want true and false", f.closedAt(legacyOpen.Invite.ID), f.closedAt(link.Invite.ID))
	}
	// Still usable: the demoted maker is a member with the brake on.
	if _, err := f.s.PreviewMemberLink(f.ctx, HashInviteCode(link.Code), f.now); err != nil {
		t.Errorf("the demoted maker's link: %v", err)
	}
}

// A member link and a legacy open invite stored with the same kind are told
// apart by every rule keyed on link.
func TestMemberLinkAndLegacyOpenAreToldApart(t *testing.T) {
	f := newInviteFixture(t)
	if _, err := f.s.db.Exec("UPDATE channels SET visibility = 'public', is_default = 1 WHERE id = ?", f.general.ID); err != nil {
		t.Fatal(err)
	}
	link := f.mustLink(f.op, memberLink())
	legacy := f.mustCreate(f.op, open("lobby"))
	if k1, l1, _, _, _, _ := f.stored(link.Invite.ID); k1 != "open" || l1 != "member" {
		t.Fatalf("link stored as %s/%s", k1, l1)
	}
	if k2, l2, _, _, _, _ := f.stored(legacy.Invite.ID); k2 != "open" || l2 != "legacy" {
		t.Fatalf("legacy stored as %s/%s", k2, l2)
	}

	names := func(code string) []string {
		_, kept, _, err := usableInviteTx(f.ctx, f.s.db, HashInviteCode(code), f.now)
		if err != nil {
			t.Fatalf("usable: %v", err)
		}
		var out []string
		for _, c := range kept {
			out = append(out, c.Name)
		}
		return out
	}
	// effectiveChannelsTx: the member link grants the defaults; the legacy
	// invite keeps its own named channel.
	if got := names(link.Code); !slices.Equal(got, []string{"general"}) {
		t.Errorf("member link lands in %v, want [general]", got)
	}
	if got := names(legacy.Code); !slices.Equal(got, []string{"lobby"}) {
		t.Errorf("legacy open invite lands in %v, want [lobby]", got)
	}
	// No default channel: a member link is still good (a member in no
	// channel); a legacy invite with none of its own left is not.
	if _, err := f.s.db.Exec("UPDATE channels SET is_default = 0"); err != nil {
		t.Fatal(err)
	}
	if got := names(link.Code); len(got) != 0 {
		t.Errorf("member link with no defaults lands in %v", got)
	}
	if _, err := f.s.db.Exec("DELETE FROM invite_channels WHERE invite_id = ?", legacy.Invite.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := usableInviteTx(f.ctx, f.s.db, HashInviteCode(legacy.Code), f.now); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("legacy invite with no channels: error = %v, want ErrInviteInvalid", err)
	}

	// makerStillAllowed: an operator holds manage_members and
	// make_member_links alike, so both stand; the maker who loses the brake
	// loses the member link only where it rests on the brake.
	if _, err := f.s.SetMayInvite(f.ctx, "system", f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	mine := f.mustLink(f.alice, memberLink())
	if _, err := f.s.db.Exec("UPDATE principals SET may_invite = 0 WHERE id = ?", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PreviewMemberLink(f.ctx, HashInviteCode(mine.Code), f.now); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("a member link whose maker lost the brake: error = %v, want ErrInviteInvalid", err)
	}
}

// makerStillAllowed fails closed for a maker who became an ended guest, was
// disabled, lost the brake or is an agent, and the link works again when the
// brake comes back (it was not closed by a column edit).
func TestMemberLinkMakerStillAllowed(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(*inviteFixture, Principal)
	}{
		{"disabled", func(f *inviteFixture, p Principal) {
			if _, err := f.s.db.Exec("UPDATE principals SET disabled_at = ? WHERE id = ?", f.now.UnixMilli(), p.ID); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"hidden", func(f *inviteFixture, p Principal) {
			if _, err := f.s.db.Exec("UPDATE principals SET disabled_at = ?, hidden_at = ? WHERE id = ?", f.now.UnixMilli(), f.now.UnixMilli(), p.ID); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"lost the brake", func(f *inviteFixture, p Principal) {
			if _, err := f.s.db.Exec("UPDATE principals SET may_invite = 0 WHERE id = ?", p.ID); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"became an ended guest", func(f *inviteFixture, p Principal) {
			g, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
				Channels: []int64{f.general.ID}, Idle: time.Hour, Absolute: time.Hour})
			if err != nil {
				f.t.Fatal(err)
			}
			if _, err := f.s.EndGuestVisit(f.ctx, "system", g.Principal.ID, schema.GuestEndOperator); err != nil {
				f.t.Fatal(err)
			}
			// The link is the ended guest's.
			if _, err := f.s.db.Exec("UPDATE invites SET created_by = ? WHERE created_by = ?", g.Principal.ID, p.ID); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"became an active guest", func(f *inviteFixture, p Principal) {
			g, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
				Channels: []int64{f.general.ID}, Idle: time.Hour, Absolute: time.Hour})
			if err != nil {
				f.t.Fatal(err)
			}
			if _, err := f.s.db.Exec("UPDATE invites SET created_by = ? WHERE created_by = ?", g.Principal.ID, p.ID); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"is an agent", func(f *inviteFixture, p Principal) {
			a, err := f.s.CreatePrincipal(f.ctx, PrincipalAgent, "bot")
			if err != nil {
				f.t.Fatal(err)
			}
			if _, err := f.s.db.Exec("UPDATE invites SET created_by = ? WHERE created_by = ?", a.ID, p.ID); err != nil {
				f.t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			link := f.mustLink(f.alice, memberLink())
			hash := HashInviteCode(link.Code)
			if _, err := f.s.PreviewMemberLink(f.ctx, hash, f.now); err != nil {
				t.Fatalf("before: %v", err)
			}
			tt.break_(f, f.alice)
			if _, err := f.s.PreviewMemberLink(f.ctx, hash, f.now); !errors.Is(err, ErrInviteInvalid) {
				t.Errorf("preview after: error = %v, want ErrInviteInvalid", err)
			}
			if _, err := f.redeem(link.Code, "newcomer"); !errors.Is(err, ErrInviteInvalid) {
				t.Errorf("redeem after: error = %v, want ErrInviteInvalid", err)
			}
		})
	}
}

// ListMemberLinks answers the maker's own links, or everyone's, and leaves
// out the legacy invites that admitted guests, which ListInvites keeps.
func TestListMemberLinks(t *testing.T) {
	f := newInviteFixture(t)
	mine := f.mustLink(f.alice, memberLink())
	capped := memberLink()
	n := int64(3)
	capped.MaxUses = &n
	cappedLink := f.mustLink(f.alice, capped)
	ops := f.mustLink(f.op, memberLink())
	plain := f.mustCreate(f.op, open("general"))
	guestOnly := legacyCreateInvite(f.t, f.s, "system", f.op.ID, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}, JoinAs: schema.InviteJoinGuest}, f.now)
	both := legacyCreateInvite(f.t, f.s, "system", f.op.ID, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}, JoinAs: schema.InviteJoinBoth}, f.now)
	for _, id := range []int64{guestOnly.Invite.ID, both.Invite.ID} {
		if _, err := f.s.CloseInvite(f.ctx, "system", f.op.ID, id, f.now); err != nil {
			t.Fatal(err)
		}
	}

	ids := func(in []schema.InviteV2) []int64 {
		out := []int64{}
		for _, i := range in {
			out = append(out, i.ID)
		}
		return out
	}
	own, err := f.s.ListMemberLinks(f.ctx, f.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(own), []int64{cappedLink.Invite.ID, mine.Invite.ID}; !slices.Equal(got, want) {
		t.Errorf("alice's links = %v, want %v", got, want)
	}
	all, err := f.s.ListMemberLinks(f.ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ids(all), []int64{plain.Invite.ID, ops.Invite.ID, cappedLink.Invite.ID, mine.Invite.ID}; !slices.Equal(got, want) {
		t.Errorf("every link = %v, want %v (no guest-admitting invite)", got, want)
	}
	for _, in := range all {
		switch in.ID {
		case plain.Invite.ID:
			if !in.Legacy || in.Kind != schema.InviteKindMember {
				t.Errorf("legacy invite = %+v", in)
			}
		case cappedLink.Invite.ID:
			if in.Legacy || in.MaxUses == nil || *in.MaxUses != 3 {
				t.Errorf("capped link = %+v", in)
			}
		case mine.Invite.ID:
			if in.MaxUses != nil || in.CreatedByName != "alice" {
				t.Errorf("uncapped link = %+v", in)
			}
		}
	}
	// v1 still lists the closed guest-admitting invites.
	v1, err := f.s.ListInvites(f.ctx, InviteFilter{}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, i := range v1 {
		if i.ID == guestOnly.Invite.ID || i.ID == both.Invite.ID {
			seen++
			if i.ClosedAt == nil {
				t.Errorf("v1 shows invite %d open", i.ID)
			}
		}
	}
	if seen != 2 {
		t.Errorf("v1 lists %d of the guest-admitting invites, want 2", seen)
	}
}

// Joiners: the maker and a holder of manage_members read them; anyone else,
// whoever, gets ErrNotFound, the error of an id that does not exist. A
// guest visit that came through a legacy invite is not one.
func TestMemberLinkJoiners(t *testing.T) {
	f := newInviteFixture(t)
	link := f.mustLink(f.alice, memberLink())
	wren := f.join(link.Code, "wren")
	f.join(link.Code, "moss")
	if _, err := f.s.DisablePrincipal(f.ctx, "system", wren.ID); err != nil {
		t.Fatal(err)
	}
	// A joiner whose account was deleted is no longer known to the maker.
	gone := f.join(link.Code, "gone")
	if _, err := f.s.db.Exec("UPDATE principals SET disabled_at = ?, hidden_at = ? WHERE id = ?", f.now.UnixMilli(), f.now.UnixMilli(), gone.ID); err != nil {
		t.Fatal(err)
	}
	guest, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.general.ID}, Idle: time.Hour, Absolute: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := f.s.CreatePrincipal(f.ctx, PrincipalAgent, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE principals SET invite_id = ? WHERE id = ?", link.Invite.ID, guest.Principal.ID); err != nil {
		t.Fatal(err)
	}
	ghost := link.Invite.ID + 1000

	for _, tt := range []struct {
		name   string
		caller int64
		invite int64
		want   []string // names; nil means not found
	}{
		{"the maker", f.alice.ID, link.Invite.ID, []string{"wren", "moss"}},
		{"an operator", f.op.ID, link.Invite.ID, []string{"wren", "moss"}},
		{"a non-owner operator", f.operator2().ID, link.Invite.ID, []string{"wren", "moss"}},
		{"another member", f.bob.ID, link.Invite.ID, nil},
		{"a joiner", wren.ID, link.Invite.ID, nil},
		{"a guest", guest.Principal.ID, link.Invite.ID, nil},
		{"an agent", agent.ID, link.Invite.ID, nil},
		{"nobody", 0, link.Invite.ID, nil},
		{"a ghost id, as an operator", f.op.ID, ghost, nil},
		{"a ghost id, as a member", f.alice.ID, ghost, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.s.MemberLinkJoiners(f.ctx, tt.caller, tt.invite)
			if tt.want == nil {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("error = %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, j := range got {
				names = append(names, j.Name)
			}
			if !slices.Equal(names, tt.want) {
				t.Errorf("joiners = %v, want %v", names, tt.want)
			}
			if !got[0].Disabled || got[1].Disabled {
				t.Errorf("disabled flags = %t %t, want true false", got[0].Disabled, got[1].Disabled)
			}
		})
	}
	// A legacy invite that admitted guests is not a v2 link to anyone, an
	// operator included; its visits are the guests list's.
	guestOnly := legacyCreateInvite(f.t, f.s, "system", f.op.ID, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}, JoinAs: schema.InviteJoinGuest}, f.now)
	if _, err := f.s.MemberLinkJoiners(f.ctx, f.op.ID, guestOnly.Invite.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("joiners of a guest-admitting invite: error = %v, want ErrNotFound", err)
	}
	// The count on the wire is the list.
	links, err := f.s.ListMemberLinks(f.ctx, f.alice.ID)
	if err != nil || len(links) != 1 || links[0].Joiners != 2 {
		t.Errorf("list = %+v, %v, want one link with 2 joiners", links, err)
	}
}

// The preview says the inviter, the channels as they are now, the expiry and
// the switches, and nothing else; a code that is no good is one error.
func TestPreviewMemberLink(t *testing.T) {
	f := newInviteFixture(t)
	if _, err := f.s.db.Exec("UPDATE channels SET visibility = 'public', is_default = 1 WHERE id = ?", f.general.ID); err != nil {
		t.Fatal(err)
	}
	link := f.mustLink(f.alice, memberLink())
	got, err := f.s.PreviewMemberLink(f.ctx, HashInviteCode(link.Code), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != schema.InviteKindMember || got.Inviter != "alice" || !slices.Equal(got.Channels, []string{"general"}) ||
		got.ExpiresAt == nil || !got.Join.Account || got.Join.Guest {
		t.Errorf("preview = %+v", got)
	}
	// The channels are the defaults as they are now, not as they were.
	if _, err := f.s.db.Exec("UPDATE channels SET visibility = 'public', is_default = 1 WHERE id = ?", f.lobby.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = f.s.PreviewMemberLink(f.ctx, HashInviteCode(link.Code), f.now); !slices.Equal(got.Channels, []string{"general", "lobby"}) {
		t.Errorf("channels = %v, want the two defaults", got.Channels)
	}
	// A link that never expires omits the expiry.
	never := memberLink()
	never.NeverExpires = true
	forever := f.mustLink(f.op, never)
	if got, _ = f.s.PreviewMemberLink(f.ctx, HashInviteCode(forever.Code), f.now); got.ExpiresAt != nil {
		t.Errorf("expires_at = %v for a link that never expires", got.ExpiresAt)
	}
	// A landing channel's name is shown, private or not (by design).
	landing := memberLink()
	landing.LandingChannels = []string{"private"}
	land := f.mustLink(f.op, landing)
	if got, _ = f.s.PreviewMemberLink(f.ctx, HashInviteCode(land.Code), f.now); !slices.Equal(got.Channels, []string{"private"}) {
		t.Errorf("landing preview channels = %v, want [private]", got.Channels)
	}

	// Every reason a code is no good is the same error, and a legacy invite
	// that admitted guests is among them.
	closed := f.mustLink(f.alice, memberLink())
	if _, err := f.s.CloseInvite(f.ctx, "system", f.alice.ID, closed.Invite.ID, f.now); err != nil {
		t.Fatal(err)
	}
	guestOnly := legacyCreateInvite(f.t, f.s, "system", f.op.ID, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}, JoinAs: schema.InviteJoinGuest}, f.now)
	for name, code := range map[string]string{"unknown": fakeCode, "closed": closed.Code, "guest-admitting": guestOnly.Code} {
		if _, err := f.s.PreviewMemberLink(f.ctx, HashInviteCode(code), f.now); !errors.Is(err, ErrInviteInvalid) {
			t.Errorf("%s: error = %v, want ErrInviteInvalid", name, err)
		}
	}
	if _, err := f.s.PreviewMemberLink(f.ctx, HashInviteCode(link.Code), f.now.Add(8*24*time.Hour)); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("expired: error = %v, want ErrInviteInvalid", err)
	}
	if _, err := f.s.SetInvitesPaused(f.ctx, "system", true); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PreviewMemberLink(f.ctx, HashInviteCode(link.Code), f.now); !errors.Is(err, ErrInvitesPaused) {
		t.Errorf("paused: error = %v, want ErrInvitesPaused", err)
	}
}

// fakeCode is well-formed and matches no invite.
var fakeCode = schema.InviteCodePrefix + strings.Repeat("Q", 43)

// withhold switches perms off in the operator column of the permissions
// document, as the owner does; the fixture's operator, an owner, holds
// everything still.
func withhold(t *testing.T, f *inviteFixture, perms ...schema.Permission) {
	t.Helper()
	doc, err := f.s.Permissions(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision}
	for _, p := range perms {
		req.Changes = append(req.Changes, schema.PermissionChangeV1{Permission: p, Role: schema.PermissionRoleOperator, Granted: false})
	}
	if _, err := f.s.PutPermissions(f.ctx, "system", f.op.ID, req, time.Now()); err != nil {
		t.Fatalf("withhold %v: %v", perms, err)
	}
}
