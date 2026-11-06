package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #1165: a demoted operator's member links that a member could not hold
// are closed at demotion, so turning the brake on later never revives them,
// and the close names link=member.
func TestDemotionClosesMemberLinksAMemberCannotHold(t *testing.T) {
	tests := []struct {
		name         string
		brakeOn      bool // may_invite when demoted
		wantNeverEnd bool // the never-expiring link is closed
		wantPlain    bool // the ordinary (expiring) link is closed
	}{
		{"brake off closes every member link", false, true, true},
		{"brake on closes only the never-expiring link", true, true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newInviteFixture(t)
			op2 := f.operator2()
			if tc.brakeOn {
				if _, err := f.s.SetMayInvite(f.ctx, "system", op2.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			never := memberLink()
			never.NeverExpires = true
			forever := f.mustLink(op2, never)
			plain := f.mustLink(op2, memberLink())

			res, err := f.s.SetRole(f.ctx, "system", f.op.ID, op2.ID, RoleMember, nil)
			if err != nil {
				t.Fatal(err)
			}
			wantClosed := 0
			for _, c := range []struct {
				id   int64
				want bool
			}{{forever.Invite.ID, tc.wantNeverEnd}, {plain.Invite.ID, tc.wantPlain}} {
				if got := f.closedAt(c.id); got != c.want {
					t.Errorf("invite %d closed = %t, want %t", c.id, got, c.want)
				}
				if c.want {
					wantClosed++
				}
			}
			if res.InvitesClosed != wantClosed {
				t.Errorf("InvitesClosed = %d, want %d", res.InvitesClosed, wantClosed)
			}

			// The brake going on (or staying on) must not bring the link back.
			if !tc.brakeOn {
				if _, err := f.s.SetMayInvite(f.ctx, "system", op2.ID, true); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.redeem(forever.Code, "joiner-never"); !errors.Is(err, ErrInviteInvalid) {
				t.Errorf("redeeming the never-expiring link: error = %v, want ErrInviteInvalid", err)
			}
			_, err = f.redeem(plain.Code, "joiner-plain")
			if tc.wantPlain && !errors.Is(err, ErrInviteInvalid) {
				t.Errorf("redeeming the closed ordinary link: error = %v, want ErrInviteInvalid", err)
			}
			if !tc.wantPlain && err != nil {
				t.Errorf("redeeming the ordinary link a member may hold: %v", err)
			}

			// Each close is audited as a member link.
			var named int
			for _, e := range f.audit() {
				if e.Action == AuditInviteClosed && strings.Contains(e.Detail, "link=member") &&
					strings.Contains(e.Detail, "reason="+InviteClosePermissionRevoked) {
					named++
				}
			}
			if named != wantClosed {
				t.Errorf("invite_closed rows naming link=member = %d, want %d", named, wantClosed)
			}
		})
	}
}

// A close by hand of a member link names link=member too; a legacy invite's
// row is unchanged.
func TestInviteClosedDetailNamesMemberLink(t *testing.T) {
	tests := []struct {
		link string
		want string
	}{
		{linkMember, "invite=7 kind=open link=member uses=2 reason=creator"},
		{linkLegacy, "invite=7 kind=open uses=2 reason=creator"},
	}
	for _, tc := range tests {
		if got := inviteClosedDetail(7, schema.InviteKindOpen, tc.link, 2, InviteCloseCreator); got != tc.want {
			t.Errorf("link %s: detail = %q, want %q", tc.link, got, tc.want)
		}
	}
}
