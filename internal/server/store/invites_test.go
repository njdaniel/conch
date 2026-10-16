package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// inviteFixture is a store with an operator, two ordinary members and three
// channels: alice may invite and is in general and lobby; bob may not invite.
type inviteFixture struct {
	t                    *testing.T
	s                    *Store
	ctx                  context.Context
	now                  time.Time
	op, alice, bob       Principal
	general, lobby, priv Channel
}

func newInviteFixture(t *testing.T) *inviteFixture {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	f := &inviteFixture{t: t, s: s, ctx: ctx, now: time.Now().Truncate(time.Millisecond)}
	op, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	f.op = op
	for _, c := range []struct {
		dst  *Principal
		name string
	}{{&f.alice, "alice"}, {&f.bob, "bob"}} {
		p, err := s.CreatePrincipal(ctx, PrincipalHuman, c.name)
		if err != nil {
			t.Fatal(err)
		}
		*c.dst = p
	}
	for _, c := range []struct {
		dst  *Channel
		name string
	}{{&f.general, "general"}, {&f.lobby, "lobby"}, {&f.priv, "private"}} {
		ch, err := s.CreateChannel(ctx, c.name)
		if err != nil {
			t.Fatal(err)
		}
		*c.dst = ch
	}
	for _, m := range []struct {
		ch Channel
		p  Principal
	}{{f.general, f.alice}, {f.lobby, f.alice}, {f.general, f.bob}} {
		if _, err := s.AddChannelMember(ctx, "system", m.ch.ID, m.p.ID, f.op.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetMayInvite(ctx, "system", f.alice.ID, true); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *inviteFixture) create(creator Principal, req schema.CreateInviteRequestV1) (schema.CreateInviteResponseV1, error) {
	f.t.Helper()
	return f.s.CreateInvite(f.ctx, principalActor(creator.ID), creator.ID, req, InviteOptions{}, f.now)
}

func (f *inviteFixture) mustCreate(creator Principal, req schema.CreateInviteRequestV1) schema.CreateInviteResponseV1 {
	f.t.Helper()
	resp, err := f.create(creator, req)
	if err != nil {
		f.t.Fatalf("CreateInvite: %v", err)
	}
	return resp
}

func (f *inviteFixture) redeem(code, name string) (RedeemedInvite, error) {
	f.t.Helper()
	return f.s.RedeemInvite(f.ctx, HashInviteCode(code), name, f.now)
}

func (f *inviteFixture) audit() []AuditEvent {
	f.t.Helper()
	evs, err := f.s.ListAuditEvents(f.ctx, 0, 100000)
	if err != nil {
		f.t.Fatal(err)
	}
	return evs
}

func (f *inviteFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func personal(channels ...string) schema.CreateInviteRequestV1 {
	return schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: channels}
}

func open(channels ...string) schema.CreateInviteRequestV1 {
	return schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: channels}
}

func TestCreateInviteRules(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name    string
		creator func(f *inviteFixture) Principal
		req     schema.CreateInviteRequestV1
		setup   func(f *inviteFixture)
		wantErr error
	}{
		{"member with the right, own channels", func(f *inviteFixture) Principal { return f.alice }, personal("general", "lobby"), nil, nil},
		{"operator names any channel", func(f *inviteFixture) Principal { return f.op }, personal("private"), nil, nil},
		{"member without the right", func(f *inviteFixture) Principal { return f.bob }, personal("general"), nil, ErrInviteNotAllowed},
		{"channel the creator is not in", func(f *inviteFixture) Principal { return f.alice }, personal("private"), nil, ErrInviteGrantExceedsInviter},
		{"one channel in, one out", func(f *inviteFixture) Principal { return f.alice }, personal("general", "private"), nil, ErrInviteGrantExceedsInviter},
		{"channel that does not exist, member", func(f *inviteFixture) Principal { return f.alice }, personal("nope"), nil, ErrInviteGrantExceedsInviter},
		{"channel that does not exist, operator", func(f *inviteFixture) Principal { return f.op }, personal("nope"), nil, ErrNotFound},
		{"voice without voice_allowed", func(f *inviteFixture) Principal { return f.alice }, func() schema.CreateInviteRequestV1 {
			r := personal("general")
			r.Voice = true
			return r
		}(), func(f *inviteFixture) {
			if _, err := f.s.db.Exec("UPDATE principals SET voice_allowed = 0 WHERE id = ?", f.alice.ID); err != nil {
				f.t.Fatal(err)
			}
		}, ErrInviteGrantExceedsInviter},
		{"voice with voice_allowed", func(f *inviteFixture) Principal { return f.alice }, func() schema.CreateInviteRequestV1 {
			r := personal("general")
			r.Voice = true
			return r
		}(), nil, nil},
		{"right to invite taken away", func(f *inviteFixture) Principal { return f.alice }, personal("general"), func(f *inviteFixture) {
			if _, err := f.s.SetMayInvite(f.ctx, "system", f.alice.ID, false); err != nil {
				f.t.Fatal(err)
			}
		}, ErrInviteNotAllowed},
		// A tester who may invite still cannot pass the right on (#457).
		{"a tester cannot grant the right to invite", func(f *inviteFixture) Principal { return f.alice }, func() schema.CreateInviteRequestV1 {
			r := personal("general")
			r.MayInvite = &yes
			return r
		}(), nil, ErrInviteGrantExceedsInviter},
		{"an operator grants the right to invite", func(f *inviteFixture) Principal { return f.op }, func() schema.CreateInviteRequestV1 {
			r := personal("general")
			r.MayInvite = &yes
			return r
		}(), nil, nil},
		{"may_invite declined", func(f *inviteFixture) Principal { return f.alice }, func() schema.CreateInviteRequestV1 {
			r := personal("general")
			r.MayInvite = &no
			return r
		}(), nil, nil},
		{"open link by a member with the right", func(f *inviteFixture) Principal { return f.alice }, open("general"), nil, ErrInviteNotAllowed},
		{"open link by an operator", func(f *inviteFixture) Principal { return f.op }, open("general", "private"), nil, nil},
		{"paused", func(f *inviteFixture) Principal { return f.op }, personal("general"), func(f *inviteFixture) {
			if _, err := f.s.SetInvitesPaused(f.ctx, "system", true); err != nil {
				f.t.Fatal(err)
			}
		}, ErrInvitesPaused},
		{"disabled creator", func(f *inviteFixture) Principal { return f.alice }, personal("general"), func(f *inviteFixture) {
			if _, err := f.s.DisablePrincipal(f.ctx, "system", f.alice.ID); err != nil {
				f.t.Fatal(err)
			}
		}, ErrInviteNotAllowed},
		{"agent creator", func(f *inviteFixture) Principal {
			a, err := f.s.CreatePrincipal(f.ctx, PrincipalAgent, "bot")
			if err != nil {
				f.t.Fatal(err)
			}
			return a
		}, personal("general"), nil, ErrInviteNotAllowed},
		{"unknown creator", func(f *inviteFixture) Principal { return Principal{ID: 9999} }, personal("general"), nil, ErrPrincipalNotFound},
		{"invalid request", func(f *inviteFixture) Principal { return f.op }, personal(), nil, ErrInviteRequestInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			if tt.setup != nil {
				tt.setup(f)
			}
			creator := tt.creator(f)
			before := f.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditInviteCreated)
			resp, err := f.create(creator, tt.req)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CreateInvite error = %v, want %v", err, tt.wantErr)
			}
			created := f.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditInviteCreated) - before
			if tt.wantErr != nil {
				if f.count("SELECT COUNT(*) FROM invites") != 0 || created != 0 {
					t.Errorf("a refused invite left rows behind")
				}
				return
			}
			if created != 1 {
				t.Errorf("invite_created rows = %d, want 1", created)
			}
			if resp.Invite.CreatedBy != creator.ID || resp.Invite.Kind != tt.req.Kind || len(resp.Invite.Channels) != len(tt.req.Channels) {
				t.Errorf("invite = %+v", resp.Invite)
			}
			if err := schema.ValidateInviteCode(resp.Code); err != nil {
				t.Errorf("code shape: %v", err)
			}
		})
	}
}

func TestCreateInviteDefaultsAndStorage(t *testing.T) {
	f := newInviteFixture(t)
	p := f.mustCreate(f.alice, personal("general"))
	o := f.mustCreate(f.op, open("general"))

	if p.Invite.MaxUses == nil || *p.Invite.MaxUses != 1 {
		t.Errorf("personal max_uses = %v, want 1", p.Invite.MaxUses)
	}
	if p.Invite.MayInvite || p.Invite.Voice {
		t.Errorf("personal grants = voice %v may_invite %v, want false/false (#457)", p.Invite.Voice, p.Invite.MayInvite)
	}
	if got, want := p.Invite.ExpiresAt.Time(), f.now.Add(7*24*time.Hour); !got.Equal(want) {
		t.Errorf("personal expiry = %v, want %v", got, want)
	}
	if o.Invite.MaxUses != nil || o.Invite.MayInvite {
		t.Errorf("open max_uses = %v may_invite = %v, want uncapped/false", o.Invite.MaxUses, o.Invite.MayInvite)
	}
	if got, want := o.Invite.ExpiresAt.Time(), f.now.Add(24*time.Hour); !got.Equal(want) {
		t.Errorf("open expiry = %v, want %v", got, want)
	}
	if p.Invite.CredentialLifetimeSeconds != 90*24*3600 {
		t.Errorf("credential lifetime = %d", p.Invite.CredentialLifetimeSeconds)
	}

	// Only a hash is stored.
	var hash string
	if err := f.s.db.QueryRow("SELECT code_hash FROM invites WHERE id = ?", p.Invite.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == p.Code || hash != HashInviteCode(p.Code) || len(hash) != 64 {
		t.Errorf("stored code_hash is not the SHA-256 of the code")
	}
	var dump string
	rows, err := f.s.db.Query("SELECT kind || code_hash || created_by FROM invites")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		dump += s
	}
	if strings.Contains(dump, p.Code) {
		t.Error("the code is stored in clear")
	}
}

func TestCreateInviteAuditDetail(t *testing.T) {
	f := newInviteFixture(t)
	capN := int64(25)
	req := open("general", "lobby")
	req.MaxUses = &capN
	req.Voice = true
	resp := f.mustCreate(f.op, req)
	var got AuditEvent
	for _, e := range f.audit() {
		if e.Action == AuditInviteCreated {
			got = e
		}
	}
	want := fmt.Sprintf("invite=%d kind=open channels=%d,%d voice=true may_invite=false expires=%s max_uses=25",
		resp.Invite.ID, f.general.ID, f.lobby.ID, resp.Invite.ExpiresAt.Time().Format(time.RFC3339))
	if got.Detail != want || got.Subject != inviteSubject(resp.Invite.ID) || got.Actor != principalActor(f.op.ID) {
		t.Errorf("audit = %+v\nwant detail %q", got, want)
	}
	resp2 := f.mustCreate(f.op, open("general"))
	for _, e := range f.audit() {
		if e.Subject == inviteSubject(resp2.Invite.ID) && !strings.HasSuffix(e.Detail, "max_uses=none") {
			t.Errorf("uncapped detail = %q, want max_uses=none", e.Detail)
		}
	}
}

func TestInviteAllowance(t *testing.T) {
	f := newInviteFixture(t)
	var first schema.CreateInviteResponseV1
	for i := 0; i < DefaultPersonalInviteAllowance; i++ {
		r := f.mustCreate(f.alice, personal("general"))
		if i == 0 {
			first = r
		}
	}
	if _, err := f.create(f.alice, personal("general")); !errors.Is(err, ErrInviteAllowanceExceeded) {
		t.Fatalf("sixth invite error = %v, want ErrInviteAllowanceExceeded", err)
	}

	t.Run("an operator has no allowance", func(t *testing.T) {
		for i := 0; i < DefaultPersonalInviteAllowance+2; i++ {
			f.mustCreate(f.op, personal("general"))
		}
	})
	t.Run("open links do not count", func(t *testing.T) {
		f.mustCreate(f.op, open("general"))
	})
	t.Run("a used invite no longer counts", func(t *testing.T) {
		if _, err := f.redeem(first.Code, "joiner1"); err != nil {
			t.Fatal(err)
		}
		f.mustCreate(f.alice, personal("general"))
		if _, err := f.create(f.alice, personal("general")); !errors.Is(err, ErrInviteAllowanceExceeded) {
			t.Errorf("error = %v, want ErrInviteAllowanceExceeded", err)
		}
	})
	t.Run("a closed invite no longer counts", func(t *testing.T) {
		list, err := f.s.ListInvites(f.ctx, InviteFilter{CreatedBy: f.alice.ID, State: InviteStateOpen}, f.now)
		if err != nil || len(list) != DefaultPersonalInviteAllowance {
			t.Fatalf("open list = %d, %v", len(list), err)
		}
		if _, err := f.s.CloseInvite(f.ctx, "system", f.alice.ID, list[0].ID, f.now); err != nil {
			t.Fatal(err)
		}
		f.mustCreate(f.alice, personal("general"))
	})
	t.Run("an expired invite no longer counts", func(t *testing.T) {
		later := f.now.Add(8 * 24 * time.Hour)
		if _, err := f.s.CreateInvite(f.ctx, "system", f.alice.ID, personal("general"), InviteOptions{}, later); err != nil {
			t.Fatalf("after expiry: %v", err)
		}
	})
	t.Run("the allowance is a parameter", func(t *testing.T) {
		g := newInviteFixture(t)
		opts := InviteOptions{Allowance: 2}
		for i := 0; i < 2; i++ {
			if _, err := g.s.CreateInvite(g.ctx, "system", g.alice.ID, personal("general"), opts, g.now); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := g.s.CreateInvite(g.ctx, "system", g.alice.ID, personal("general"), opts, g.now); !errors.Is(err, ErrInviteAllowanceExceeded) {
			t.Errorf("error = %v, want ErrInviteAllowanceExceeded", err)
		}
	})
}

func TestRedeemInviteCreatesAccount(t *testing.T) {
	f := newInviteFixture(t)
	req := personal("general", "lobby")
	req.Voice = true
	inv := f.mustCreate(f.alice, req)
	startAudit := len(f.audit())

	got, err := f.redeem(inv.Code, "river")
	if err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}
	p := got.Principal
	if p.Kind != PrincipalHuman || p.Role != RoleMember || p.Name != "river" || p.InvitedBy != f.alice.ID || p.InviteID != inv.Invite.ID {
		t.Errorf("principal = %+v", p)
	}
	if p.MayInvite || !p.VoiceAllowed {
		t.Errorf("grants = may_invite %v voice %v, want no right to invite (#457) and voice", p.MayInvite, p.VoiceAllowed)
	}
	stored, err := f.s.PrincipalByID(f.ctx, p.ID)
	if err != nil || stored.InvitedBy != f.alice.ID || stored.InviteID != inv.Invite.ID || stored.Role != RoleMember || stored.MayInvite || !stored.VoiceAllowed {
		t.Errorf("stored principal = %+v, %v", stored, err)
	}
	if n := f.count("SELECT COUNT(*) FROM principals WHERE invite_id = ?", inv.Invite.ID); n != 1 {
		t.Errorf("principals through the invite = %d, want 1", n)
	}
	if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ? AND added_by = ?", p.ID, f.alice.ID); n != 2 {
		t.Errorf("memberships = %d, want 2", n)
	}
	if n := f.count("SELECT COUNT(*) FROM credentials WHERE principal_id = ?", p.ID); n != 1 {
		t.Errorf("credentials = %d, want 1", n)
	}
	if got.Credential.Label != fmt.Sprintf("invite %d", inv.Invite.ID) || got.Credential.ExpiresAt == nil ||
		!got.Credential.ExpiresAt.Time().Equal(f.now.Add(90*24*time.Hour)) {
		t.Errorf("credential = %+v", got.Credential)
	}
	resolved, err := f.s.ResolveCredential(f.ctx, got.Token)
	if err != nil || resolved.ID != p.ID || resolved.InvitedBy != f.alice.ID || resolved.MayInvite {
		t.Errorf("ResolveCredential = %+v, %v", resolved, err)
	}
	if n := f.count("SELECT uses FROM invites WHERE id = ?", inv.Invite.ID); n != 1 {
		t.Errorf("uses = %d, want 1", n)
	}

	// The audit rows, in order, all in the redemption.
	actor := inviteSubject(inv.Invite.ID)
	wantRows := []struct{ action, subject, detail string }{
		{AuditInviteRedeemed, inviteSubject(inv.Invite.ID), fmt.Sprintf("invite=%d principal=%d uses=1 dropped_channels=none", inv.Invite.ID, p.ID)},
		{AuditPrincipalCreated, principalActor(p.ID), fmt.Sprintf("kind=human invite=%d inviter=%d", inv.Invite.ID, f.alice.ID)},
		{AuditMemberAdded, channelSubject(f.general.ID), memberDetail(p.ID)},
		{AuditMemberAdded, channelSubject(f.lobby.ID), memberDetail(p.ID)},
		{"credential_created", principalActor(p.ID), credentialDetail(got.Credential.ID, got.Credential.Label)},
	}
	rows := f.audit()[startAudit:]
	if len(rows) != len(wantRows) {
		t.Fatalf("audit rows = %d, want %d: %+v", len(rows), len(wantRows), rows)
	}
	for i, w := range wantRows {
		if rows[i].Action != w.action || rows[i].Subject != w.subject || rows[i].Detail != w.detail || rows[i].Actor != actor {
			t.Errorf("audit[%d] = %+v, want %+v actor %s", i, rows[i], w, actor)
		}
	}
}

func TestRedeemInviteGrants(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name      string
		req       schema.CreateInviteRequestV1
		creator   func(f *inviteFixture) Principal
		wantMay   bool
		wantVoice bool
	}{
		// Nobody who joins may invite unless an operator grants it (#457).
		{"personal default", personal("general"), func(f *inviteFixture) Principal { return f.alice }, false, false},
		{"personal without the right to invite", func() schema.CreateInviteRequestV1 { r := personal("general"); r.MayInvite = &no; return r }(), func(f *inviteFixture) Principal { return f.alice }, false, false},
		{"personal with voice", func() schema.CreateInviteRequestV1 { r := personal("general"); r.Voice = true; return r }(), func(f *inviteFixture) Principal { return f.alice }, false, true},
		{"an operator's personal invite grants the right", func() schema.CreateInviteRequestV1 { r := personal("general"); r.MayInvite = &yes; return r }(), func(f *inviteFixture) Principal { return f.op }, true, false},
		{"open default", open("general"), func(f *inviteFixture) Principal { return f.op }, false, false},
		{"open that lets the joiner invite", func() schema.CreateInviteRequestV1 { r := open("general"); r.MayInvite = &yes; return r }(), func(f *inviteFixture) Principal { return f.op }, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			inv := f.mustCreate(tt.creator(f), tt.req)
			got, err := f.redeem(inv.Code, "joiner")
			if err != nil {
				t.Fatal(err)
			}
			if got.Principal.MayInvite != tt.wantMay || got.Principal.VoiceAllowed != tt.wantVoice ||
				got.Principal.Role != RoleMember || got.Principal.Kind != PrincipalHuman {
				t.Errorf("principal = %+v", got.Principal)
			}
		})
	}
}

func TestRedeemInviteRefusals(t *testing.T) {
	tests := []struct {
		name    string
		kind    string // "personal" or "open"
		prepare func(f *inviteFixture, inv schema.CreateInviteResponseV1) (code string, now time.Time)
		wantErr error
	}{
		{"second use of a personal invite", "personal", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			if _, err := f.redeem(inv.Code, "first"); err != nil {
				f.t.Fatal(err)
			}
			return inv.Code, f.now
		}, ErrInviteInvalid},
		{"unknown code", "personal", func(f *inviteFixture, _ schema.CreateInviteResponseV1) (string, time.Time) {
			c, err := NewInviteCode()
			if err != nil {
				f.t.Fatal(err)
			}
			return c, f.now
		}, ErrInviteInvalid},
		{"expired", "personal", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			return inv.Code, inv.Invite.ExpiresAt.Time()
		}, ErrInviteInvalid},
		{"expired open link", "open", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			return inv.Code, inv.Invite.ExpiresAt.Time().Add(time.Second)
		}, ErrInviteInvalid},
		{"closed", "personal", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			if _, err := f.s.CloseInvite(f.ctx, "system", f.op.ID, inv.Invite.ID, f.now); err != nil {
				f.t.Fatal(err)
			}
			return inv.Code, f.now
		}, ErrInviteInvalid},
		{"closed open link", "open", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			if _, err := f.s.CloseInvite(f.ctx, "system", f.op.ID, inv.Invite.ID, f.now); err != nil {
				f.t.Fatal(err)
			}
			return inv.Code, f.now
		}, ErrInviteInvalid},
		{"paused", "personal", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			if _, err := f.s.SetInvitesPaused(f.ctx, "system", true); err != nil {
				f.t.Fatal(err)
			}
			return inv.Code, f.now
		}, ErrInvitesPaused},
		{"paused, unknown code", "personal", func(f *inviteFixture, _ schema.CreateInviteResponseV1) (string, time.Time) {
			if _, err := f.s.SetInvitesPaused(f.ctx, "system", true); err != nil {
				f.t.Fatal(err)
			}
			c, _ := NewInviteCode()
			return c, f.now
		}, ErrInvitesPaused},
		{"inviter disabled", "personal", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			if _, err := f.s.DisablePrincipal(f.ctx, "system", f.alice.ID); err != nil {
				f.t.Fatal(err)
			}
			return inv.Code, f.now
		}, ErrInviteInvalid},
		{"operator inviter disabled", "open", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			// DisablePrincipal refuses the last operator, so go underneath it.
			if _, err := f.s.db.Exec("UPDATE principals SET disabled_at = 1 WHERE id = ?", f.op.ID); err != nil {
				f.t.Fatal(err)
			}
			return inv.Code, f.now
		}, ErrInviteInvalid},
		{"inviter lost the right to invite", "personal", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			if _, err := f.s.SetMayInvite(f.ctx, "system", f.alice.ID, false); err != nil {
				f.t.Fatal(err)
			}
			return inv.Code, f.now
		}, ErrInviteInvalid},
		{"inviter left every channel it named", "personal", func(f *inviteFixture, inv schema.CreateInviteResponseV1) (string, time.Time) {
			if _, err := f.s.RemoveChannelMember(f.ctx, "system", f.general.ID, f.alice.ID); err != nil {
				f.t.Fatal(err)
			}
			return inv.Code, f.now
		}, ErrInviteInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			var inv schema.CreateInviteResponseV1
			if tt.kind == "open" {
				inv = f.mustCreate(f.op, open("general"))
			} else {
				inv = f.mustCreate(f.alice, personal("general"))
			}
			code, now := tt.prepare(f, inv)
			principals := f.count("SELECT COUNT(*) FROM principals")
			creds := f.count("SELECT COUNT(*) FROM credentials")
			members := f.count("SELECT COUNT(*) FROM channel_members")
			audits := len(f.audit())

			_, err := f.s.RedeemInvite(f.ctx, HashInviteCode(code), "late", now)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("RedeemInvite error = %v, want %v", err, tt.wantErr)
			}
			if _, perr := f.s.PreviewInvite(f.ctx, HashInviteCode(code), now); !errors.Is(perr, tt.wantErr) {
				t.Errorf("PreviewInvite error = %v, want %v", perr, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), code) {
				t.Error("the error contains the code")
			}
			if f.count("SELECT COUNT(*) FROM principals") != principals || f.count("SELECT COUNT(*) FROM credentials") != creds ||
				f.count("SELECT COUNT(*) FROM channel_members") != members || len(f.audit()) != audits {
				t.Error("a refused redemption wrote rows")
			}
		})
	}
}

func TestRedeemInviteDuplicateNameWritesNothing(t *testing.T) {
	f := newInviteFixture(t)
	inv := f.mustCreate(f.alice, personal("general"))
	audits := len(f.audit())
	principals := f.count("SELECT COUNT(*) FROM principals")

	if _, err := f.redeem(inv.Code, "bob"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("error = %v, want ErrDuplicate", err)
	}
	if f.count("SELECT uses FROM invites WHERE id = ?", inv.Invite.ID) != 0 {
		t.Error("a failed redemption spent the use")
	}
	if len(f.audit()) != audits || f.count("SELECT COUNT(*) FROM principals") != principals || f.count("SELECT COUNT(*) FROM credentials WHERE label LIKE 'invite %'") != 0 {
		t.Error("a duplicate name left rows behind")
	}
	// The same invite still works with another name.
	if _, err := f.redeem(inv.Code, "bobby"); err != nil {
		t.Fatalf("retry with a free name: %v", err)
	}
	if _, err := f.redeem(inv.Code, "bobby2"); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("second redemption = %v, want ErrInviteInvalid", err)
	}
}

func TestRedeemInviteRejectsBadName(t *testing.T) {
	f := newInviteFixture(t)
	inv := f.mustCreate(f.alice, personal("general"))
	for _, name := range []string{"", "  ", "a\x1bb", strings.Repeat("x", schema.MaxPrincipalNameLength+1)} {
		if _, err := f.redeem(inv.Code, name); !errors.Is(err, ErrInviteRequestInvalid) {
			t.Errorf("name %q: error = %v, want ErrInviteRequestInvalid", name, err)
		}
	}
	if f.count("SELECT uses FROM invites") != 0 {
		t.Error("a bad name spent the use")
	}
}

func TestRedeemPersonalInviteDropsChannelsInviterLeft(t *testing.T) {
	f := newInviteFixture(t)
	inv := f.mustCreate(f.alice, personal("general", "lobby"))
	if _, err := f.s.RemoveChannelMember(f.ctx, "system", f.lobby.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	prev, err := f.s.PreviewInvite(f.ctx, HashInviteCode(inv.Code), f.now)
	if err != nil || len(prev.Channels) != 1 || prev.Channels[0] != "general" {
		t.Fatalf("preview = %+v, %v; want only general", prev, err)
	}
	got, err := f.redeem(inv.Code, "river")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.DroppedChannels) != 1 || got.DroppedChannels[0] != f.lobby.ID {
		t.Errorf("dropped = %v, want [%d]", got.DroppedChannels, f.lobby.ID)
	}
	if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ?", got.Principal.ID); n != 1 {
		t.Errorf("memberships = %d, want 1", n)
	}
	var detail string
	for _, e := range f.audit() {
		if e.Action == AuditInviteRedeemed {
			detail = e.Detail
		}
	}
	if want := fmt.Sprintf("dropped_channels=%d", f.lobby.ID); !strings.Contains(detail, want) {
		t.Errorf("redeemed detail = %q, want it to contain %q", detail, want)
	}
}

func TestRedeemOperatorInviteKeepsEveryChannel(t *testing.T) {
	f := newInviteFixture(t)
	inv := f.mustCreate(f.op, personal("general", "private"))
	// The operator is in none of these channels, and needs not be.
	got, err := f.redeem(inv.Code, "river")
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ?", got.Principal.ID); n != 2 || len(got.DroppedChannels) != 0 {
		t.Errorf("memberships = %d, dropped %v; want 2 and none", n, got.DroppedChannels)
	}
}

func TestOpenInviteUncappedAndCapped(t *testing.T) {
	f := newInviteFixture(t)
	t.Run("uncapped redeems any number of times", func(t *testing.T) {
		inv := f.mustCreate(f.op, open("general"))
		for i := 0; i < 12; i++ {
			if _, err := f.redeem(inv.Code, fmt.Sprintf("guest%d", i)); err != nil {
				t.Fatalf("redemption %d: %v", i, err)
			}
		}
		list, err := f.s.ListInvites(f.ctx, InviteFilter{Kind: schema.InviteKindOpen}, f.now)
		if err != nil || len(list) != 1 || list[0].Uses != 12 || list[0].MaxUses != nil {
			t.Errorf("list = %+v, %v", list, err)
		}
		joiners, err := f.s.InviteJoiners(f.ctx, inv.Invite.ID)
		if err != nil || len(joiners) != 12 || joiners[0].InvitedBy != f.op.ID {
			t.Errorf("joiners = %d, %v", len(joiners), err)
		}
	})
	t.Run("capped stops at the cap", func(t *testing.T) {
		req := open("general")
		two := int64(2)
		req.MaxUses = &two
		inv := f.mustCreate(f.op, req)
		for i := 0; i < 2; i++ {
			if _, err := f.redeem(inv.Code, fmt.Sprintf("capped%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.redeem(inv.Code, "capped2"); !errors.Is(err, ErrInviteInvalid) {
			t.Errorf("third redemption = %v, want ErrInviteInvalid", err)
		}
	})
}

func TestRedeemConcurrently(t *testing.T) {
	tests := []struct {
		name     string
		req      schema.CreateInviteRequestV1
		attempts int
		want     int
	}{
		{"personal invite, one winner", personal("general"), 8, 1},
		{"open link capped at 3", func() schema.CreateInviteRequestV1 { r := open("general"); n := int64(3); r.MaxUses = &n; return r }(), 10, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			inv := f.mustCreate(f.op, tt.req)
			var wg sync.WaitGroup
			results := make([]error, tt.attempts)
			for i := 0; i < tt.attempts; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, results[i] = f.redeem(inv.Code, fmt.Sprintf("racer%d", i))
				}()
			}
			wg.Wait()
			wins := 0
			for _, err := range results {
				switch {
				case err == nil:
					wins++
				case !errors.Is(err, ErrInviteInvalid):
					t.Errorf("loser error = %v, want ErrInviteInvalid", err)
				}
			}
			if wins != tt.want {
				t.Errorf("winners = %d, want %d", wins, tt.want)
			}
			if n := f.count("SELECT COUNT(*) FROM principals WHERE invite_id = ?", inv.Invite.ID); n != tt.want {
				t.Errorf("principals through the invite = %d, want %d", n, tt.want)
			}
			if n := f.count("SELECT uses FROM invites WHERE id = ?", inv.Invite.ID); n != tt.want {
				t.Errorf("uses = %d, want %d", n, tt.want)
			}
		})
	}
}

func TestCloseInvite(t *testing.T) {
	tests := []struct {
		name        string
		closer      func(f *inviteFixture) int64
		wantErr     error
		wantChanged bool
	}{
		{"its maker", func(f *inviteFixture) int64 { return f.alice.ID }, nil, true},
		{"an operator", func(f *inviteFixture) int64 { return f.op.ID }, nil, true},
		{"someone else", func(f *inviteFixture) int64 { return f.bob.ID }, ErrNotFound, false},
		{"an unknown principal", func(f *inviteFixture) int64 { return 9999 }, ErrNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			inv := f.mustCreate(f.alice, personal("general"))
			changed, err := f.s.CloseInvite(f.ctx, "system", tt.closer(f), inv.Invite.ID, f.now)
			if !errors.Is(err, tt.wantErr) || changed != tt.wantChanged {
				t.Fatalf("CloseInvite = %v, %v; want %v, %v", changed, err, tt.wantErr, tt.wantChanged)
			}
			closedRows := f.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditInviteClosed)
			if (closedRows == 1) != tt.wantChanged {
				t.Errorf("invite_closed rows = %d", closedRows)
			}
			if tt.wantChanged {
				// Closing again is a no-op without a second audit row.
				if changed, err := f.s.CloseInvite(f.ctx, "system", f.op.ID, inv.Invite.ID, f.now); err != nil || changed {
					t.Errorf("second close = %v, %v; want false, nil", changed, err)
				}
				if f.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditInviteClosed) != 1 {
					t.Error("a second close wrote a second audit row")
				}
				if _, err := f.redeem(inv.Code, "late"); !errors.Is(err, ErrInviteInvalid) {
					t.Errorf("redeem after close = %v", err)
				}
			}
		})
	}
	t.Run("an unknown invite", func(t *testing.T) {
		f := newInviteFixture(t)
		if _, err := f.s.CloseInvite(f.ctx, "system", f.op.ID, 9999, f.now); !errors.Is(err, ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})
}

func TestListInvitesFilters(t *testing.T) {
	f := newInviteFixture(t)
	used := f.mustCreate(f.alice, personal("general"))
	if _, err := f.redeem(used.Code, "joiner"); err != nil {
		t.Fatal(err)
	}
	closed := f.mustCreate(f.alice, personal("general"))
	if _, err := f.s.CloseInvite(f.ctx, "system", f.alice.ID, closed.Invite.ID, f.now); err != nil {
		t.Fatal(err)
	}
	fresh := f.mustCreate(f.alice, personal("lobby"))
	opn := f.mustCreate(f.op, open("general"))
	later := f.now.Add(8 * 24 * time.Hour)

	ids := func(l []schema.InviteV1) []int64 {
		out := make([]int64, len(l))
		for i, inv := range l {
			out[i] = inv.ID
		}
		return out
	}
	tests := []struct {
		name string
		f    InviteFilter
		now  time.Time
		want []int64
	}{
		{"all, newest first", InviteFilter{}, f.now, []int64{opn.Invite.ID, fresh.Invite.ID, closed.Invite.ID, used.Invite.ID}},
		{"by creator", InviteFilter{CreatedBy: f.alice.ID}, f.now, []int64{fresh.Invite.ID, closed.Invite.ID, used.Invite.ID}},
		{"by kind", InviteFilter{Kind: schema.InviteKindOpen}, f.now, []int64{opn.Invite.ID}},
		{"open", InviteFilter{State: InviteStateOpen}, f.now, []int64{opn.Invite.ID, fresh.Invite.ID}},
		{"closed", InviteFilter{State: InviteStateClosed}, f.now, []int64{closed.Invite.ID}},
		{"used", InviteFilter{State: InviteStateUsed}, f.now, []int64{used.Invite.ID}},
		{"expired", InviteFilter{State: InviteStateExpired}, later, []int64{opn.Invite.ID, fresh.Invite.ID}},
		{"nothing matches", InviteFilter{CreatedBy: f.bob.ID}, f.now, []int64{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.s.ListInvites(f.ctx, tt.f, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("ListInvites returned nil, want an empty slice")
			}
			if g := ids(got); fmt.Sprint(g) != fmt.Sprint(tt.want) {
				t.Errorf("ids = %v, want %v", g, tt.want)
			}
		})
	}
}

func TestInvitesPausedSwitch(t *testing.T) {
	f := newInviteFixture(t)
	if p, err := f.s.InvitesPaused(f.ctx); err != nil || p {
		t.Fatalf("initially paused = %v, %v", p, err)
	}
	inv := f.mustCreate(f.alice, personal("general"))
	steps := []struct {
		pause       bool
		wantChanged bool
		wantAction  string
	}{
		{true, true, AuditInvitesPaused},
		{true, false, ""},
		{false, true, AuditInvitesResumed},
		{false, false, ""},
	}
	for _, st := range steps {
		before := len(f.audit())
		changed, err := f.s.SetInvitesPaused(f.ctx, principalActor(f.op.ID), st.pause)
		if err != nil || changed != st.wantChanged {
			t.Fatalf("SetInvitesPaused(%v) = %v, %v", st.pause, changed, err)
		}
		added := f.audit()[before:]
		if st.wantAction == "" && len(added) != 0 || st.wantAction != "" && (len(added) != 1 || added[0].Action != st.wantAction || added[0].Actor != principalActor(f.op.ID)) {
			t.Errorf("audit rows after SetInvitesPaused(%v) = %+v", st.pause, added)
		}
		if p, _ := f.s.InvitesPaused(f.ctx); p != st.pause {
			t.Errorf("InvitesPaused = %v, want %v", p, st.pause)
		}
	}
	// Resumed: the invite made before the pause works again.
	if _, err := f.redeem(inv.Code, "river"); err != nil {
		t.Errorf("redeem after resume: %v", err)
	}
}

func TestSetMayInvite(t *testing.T) {
	f := newInviteFixture(t)
	agent, err := f.s.CreatePrincipal(f.ctx, PrincipalAgent, "bot")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		id          int64
		may         bool
		wantChanged bool
		wantErr     error
	}{
		{"grant", f.bob.ID, true, true, nil},
		{"grant again", f.bob.ID, true, false, nil},
		{"revoke", f.bob.ID, false, true, nil},
		{"an agent", agent.ID, true, false, ErrInviteNotAllowed},
		{"unknown", 9999, true, false, ErrPrincipalNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditInvitePermissionChanged)
			changed, err := f.s.SetMayInvite(f.ctx, principalActor(f.op.ID), tt.id, tt.may)
			if !errors.Is(err, tt.wantErr) || changed != tt.wantChanged {
				t.Fatalf("SetMayInvite = %v, %v; want %v, %v", changed, err, tt.wantChanged, tt.wantErr)
			}
			rows := f.count("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditInvitePermissionChanged) - before
			if (rows == 1) != tt.wantChanged || rows > 1 {
				t.Errorf("invite_permission_changed rows = %d", rows)
			}
			if tt.wantChanged {
				var e AuditEvent
				for _, ev := range f.audit() {
					if ev.Action == AuditInvitePermissionChanged {
						e = ev
					}
				}
				if e.Subject != principalActor(tt.id) || e.Detail != fmt.Sprintf("may_invite=%t", tt.may) {
					t.Errorf("audit = %+v", e)
				}
			}
		})
	}
}

// TestInviteSecretsNeverLeak runs a create, a preview, a redeem and the
// refusals, and scans every audit row, every captured log line and every error
// string for the code, its hash, the token and the token's hash.
func TestInviteSecretsNeverLeak(t *testing.T) {
	logs := newLogCapture(t)

	f := newInviteFixture(t)
	var errs []string
	note := func(err error) {
		if err != nil {
			errs = append(errs, err.Error())
		}
	}
	inv := f.mustCreate(f.alice, personal("general"))
	_, err := f.s.PreviewInvite(f.ctx, HashInviteCode(inv.Code), f.now)
	note(err)
	got, err := f.redeem(inv.Code, "river")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.redeem(inv.Code, "again")
	note(err)
	_, err = f.redeem(inv.Code, "bob")
	note(err)
	_, err = f.s.PreviewInvite(f.ctx, HashInviteCode(inv.Code), f.now)
	note(err)
	_, err = f.s.CloseInvite(f.ctx, principalActor(f.alice.ID), f.alice.ID, inv.Invite.ID, f.now)
	note(err)

	secrets := map[string]string{
		"code":       inv.Code,
		"code hash":  HashInviteCode(inv.Code),
		"token":      got.Token,
		"token hash": hashCredentialToken(got.Token),
	}
	var haystack []string
	for _, e := range f.audit() {
		haystack = append(haystack, e.Actor, e.Action, e.Subject, e.Detail)
	}
	if len(haystack) == 0 {
		t.Fatal("no audit rows")
	}
	haystack = append(haystack, logs.String())
	haystack = append(haystack, errs...)
	// Also the invite as the wire shows it.
	haystack = append(haystack, fmt.Sprintf("%+v", inv.Invite))
	for what, secret := range secrets {
		for _, h := range haystack {
			if strings.Contains(h, secret) {
				t.Errorf("the %s appears in %q", what, truncate(h, 80))
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func TestPreviewInviteShowsLittle(t *testing.T) {
	f := newInviteFixture(t)
	req := personal("general", "lobby")
	req.Voice = true
	inv := f.mustCreate(f.alice, req)
	got, err := f.s.PreviewInvite(f.ctx, HashInviteCode(inv.Code), f.now)
	if err != nil {
		t.Fatal(err)
	}
	want := schema.InvitePreviewV1{
		Kind: schema.InviteKindPersonal, Inviter: "alice", Channels: []string{"general", "lobby"}, Voice: true,
		ExpiresAt: inv.Invite.ExpiresAt,
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("preview = %+v, want %+v", got, want)
	}
	if f.count("SELECT uses FROM invites") != 0 {
		t.Error("a preview spent a use")
	}
	// Voice is what the maker still holds, not only what was asked for.
	if _, err := f.s.db.Exec("UPDATE principals SET voice_allowed = 0 WHERE id = ?", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	got, err = f.s.PreviewInvite(f.ctx, HashInviteCode(inv.Code), f.now)
	if err != nil || got.Voice {
		t.Errorf("preview after the maker lost voice = %+v, %v; want voice false", got, err)
	}
	r, err := f.redeem(inv.Code, "river")
	if err != nil || r.Principal.VoiceAllowed {
		t.Errorf("redeem after the maker lost voice: voice %v, %v; want false", r.Principal.VoiceAllowed, err)
	}
}

// TestInvitesMigrationFromSchema23 opens a database at version 23 holding
// principals, channels, members, messages and credentials and checks that
// migration 24 keeps them, gives every principal may_invite 0 and
// voice_allowed 1, and is a no-op on reopen.
func TestInvitesMigrationFromSchema23(t *testing.T) {
	const pre = 23
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
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'human', 'ada', 'member', 2), (3, 'agent', 'bot', 'member', 3)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4), (1, 2, 1, 4)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old', 5)`,
		`INSERT INTO credentials (principal_id, label, token_hash, created_at) VALUES (1, 'bootstrap', 'aa', 6), (2, 'laptop', 'bb', 6)`,
		`PRAGMA user_version = 23`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for pass := 1; pass <= 2; pass++ {
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("Open pass %d: %v", pass, err)
		}
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 24 {
			t.Fatalf("pass %d: user_version = %d, %v; want %d", pass, version, err, len(migrations))
		}
		rows, err := s.db.Query(`SELECT id, may_invite, voice_allowed, invited_by, invite_id FROM principals ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for rows.Next() {
			var id, may, voice int64
			var by, inv sql.NullInt64
			if err := rows.Scan(&id, &may, &voice, &by, &inv); err != nil {
				t.Fatal(err)
			}
			if may != 0 || voice != 1 || by.Valid || inv.Valid {
				t.Errorf("pass %d: principal %d: may_invite %d voice_allowed %d invited_by %v invite_id %v", pass, id, may, voice, by, inv)
			}
			n++
		}
		_ = rows.Close()
		if n != 3 {
			t.Errorf("pass %d: principals = %d, want 3", pass, n)
		}
		// Pass 1 made one invite after migrating; pass 2 must find it, not a fresh table.
		invites := pass - 1
		for table, want := range map[string]int{"channel_members": 2, "messages": 1, "credentials": 2, "invites": invites, "invite_channels": invites, "server_settings": 0} {
			var got int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&got); err != nil || got != want {
				t.Errorf("pass %d: %s rows = %d, %v; want %d", pass, table, got, err, want)
			}
		}
		// The migrated database takes an invite end to end.
		if pass == 1 {
			ada, err := s.PrincipalByID(ctx, 2)
			if err != nil || ada.MayInvite || !ada.VoiceAllowed {
				t.Errorf("PrincipalByID = %+v, %v", ada, err)
			}
			if _, err := s.CreateInvite(ctx, "system", 2, personal("general"), InviteOptions{}, time.Now()); !errors.Is(err, ErrInviteNotAllowed) {
				t.Errorf("an existing member made an invite: %v", err)
			}
			if _, err := s.CreateInvite(ctx, "system", 1, personal("general"), InviteOptions{}, time.Now()); err != nil {
				t.Errorf("the operator could not make an invite: %v", err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// The pause switch fails closed: a stored value that is not a clear boolean
// counts as paused.
func TestInvitesPausedFailsClosed(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	for _, tt := range []struct {
		value  string
		paused bool
	}{{"true", true}, {"false", false}, {"1", true}, {"0", false}, {"TRUE", true}, {"yes", true}, {"", true}} {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO server_settings (key, value, updated_at) VALUES ('invites_paused', ?, 1)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, tt.value); err != nil {
			t.Fatal(err)
		}
		got, err := s.InvitesPaused(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got != tt.paused {
			t.Errorf("stored %q: paused = %v, want %v", tt.value, got, tt.paused)
		}
	}
}

// Redemption checks the maker again: an invite row that carries the right to
// invite but was made by someone who is not an operator gives the joiner no
// such right, however the row came to be (issue #457).
func TestRedeemDoesNotTrustAStoredMayInvite(t *testing.T) {
	f := newInviteFixture(t)
	inv := f.mustCreate(f.alice, personal("general"))
	if _, err := f.s.db.ExecContext(f.ctx, "UPDATE invites SET may_invite = 1 WHERE id = ?", inv.Invite.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.redeem(inv.Code, "joiner")
	if err != nil {
		t.Fatal(err)
	}
	if got.Principal.MayInvite {
		t.Error("the joiner holds the right to invite from a tester's invite")
	}
	if stored, err := f.s.PrincipalByID(f.ctx, got.Principal.ID); err != nil || stored.MayInvite {
		t.Errorf("stored principal = %+v, %v; want no right to invite", stored, err)
	}
	if resolved, err := f.s.ResolveCredential(f.ctx, got.Token); err != nil || resolved.MayInvite {
		t.Errorf("ResolveCredential = %+v, %v; want no right to invite", resolved, err)
	}
}
