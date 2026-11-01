package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Accounts in the store (issue #711): an account from the bare address
// (JoinAsAccount), the conversion's admission by policy or permit, and the
// permits themselves. The routes' matrices are in
// internal/server/conversion_test.go.

// putJoin applies req as PutJoinSettings did before guests were removed
// (issue #1005), guests and all: the policies of the visits these tests
// convert are those of a database from before then.
func (f *inviteFixture) putJoin(req schema.PutJoinSettingsRequestV1) {
	f.t.Helper()
	legacyJoinSettings(f.t, f.s, req)
}

// Every mode and pause of the bare address: an account is made exactly
// where addressAccountPolicy lets one be, and a refusal writes nothing.
func TestJoinAsAccountPolicy(t *testing.T) {
	tests := []struct {
		mode   schema.OpenJoiningMode
		paused bool
		want   error
	}{
		{schema.OpenJoiningOff, false, ErrJoiningClosed},
		{schema.OpenJoiningGuest, false, ErrJoiningClosed},
		{schema.OpenJoiningAccount, false, nil},
		{schema.OpenJoiningBoth, false, nil},
		{schema.OpenJoiningAccount, true, ErrJoiningPaused},
		{schema.OpenJoiningBoth, true, ErrJoiningPaused},
		{schema.OpenJoiningGuest, true, ErrJoiningClosed},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s paused=%t", tt.mode, tt.paused), func(t *testing.T) {
			f := newInviteFixture(t)
			f.putJoin(schema.PutJoinSettingsRequestV1{GuestsEnabled: boolRef(true), OpenJoining: ptrMode(tt.mode),
				JoiningPaused: boolRef(tt.paused), OpenChannels: &[]string{"general"}})
			before := f.count("SELECT COUNT(*) FROM audit_events")
			err := f.s.CheckJoinAccount(f.ctx)
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Errorf("CheckJoinAccount = %v, want %v", err, tt.want)
			}
			_, err = f.s.JoinAsAccount(f.ctx, "newcomer", testRecord("n"), f.now)
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Fatalf("JoinAsAccount = %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				if n := f.count("SELECT COUNT(*) FROM principals WHERE name = 'newcomer'"); n != 0 {
					t.Errorf("a refused account was made")
				}
				if n := f.count("SELECT COUNT(*) FROM audit_events"); n != before {
					t.Errorf("a refusal wrote %d audit rows", n-before)
				}
			}
		})
	}
}

// What an account from the bare address is made with: its password, every
// open channel with can_decide 0 and a history bound at the channel's last
// message, join_voice, no right to invite, a renewable credential, and the
// audit rows, all or nothing.
func TestJoinAsAccount(t *testing.T) {
	f := newInviteFixture(t)
	last, err := f.s.InsertMessage(f.ctx, f.general.ID, f.alice.ID, "before")
	if err != nil {
		t.Fatal(err)
	}
	f.putJoin(schema.PutJoinSettingsRequestV1{OpenJoining: ptrMode(schema.OpenJoiningAccount), JoinVoice: boolRef(false),
		OpenChannels: &[]string{"general", "lobby"}})

	// Names that are not to be had make nothing, the guest namespace and
	// its look-alikes included.
	for _, name := range []string{"alice", "ALICE", "Guest-x", "guеst-x", "hidden-3"} {
		if _, err := f.s.JoinAsAccount(f.ctx, name, testRecord("n"), f.now); !errors.Is(err, ErrDuplicate) {
			t.Errorf("JoinAsAccount(%q) = %v, want refused as taken or reserved", name, err)
		}
	}
	if _, err := f.s.JoinAsAccount(f.ctx, "newcomer", "plain text", f.now); !errors.Is(err, ErrPasswordRecordInvalid) {
		t.Errorf("a record that is not one = %v", err)
	}
	if n := f.count("SELECT COUNT(*) FROM passwords"); n != 0 {
		t.Fatalf("refusals stored %d passwords", n)
	}

	a, err := f.s.JoinAsAccount(f.ctx, "newcomer", testRecord("n"), f.now)
	if err != nil {
		t.Fatal(err)
	}
	// Voice is on for every account since migration 40 (join_voice reads
	// true, ADR-013 decision 5).
	if a.Principal.Name != "newcomer" || a.Principal.Role != RoleMember || !a.Principal.VoiceAllowed || a.Principal.MayInvite ||
		len(a.Channels) != 2 || !a.Credential.Renews || a.Token == "" {
		t.Fatalf("account = %+v", a)
	}
	p, err := f.s.ResolveCredential(f.ctx, a.Token)
	if err != nil || p.ID != a.Principal.ID || p.IsGuest() || p.InviteID != 0 || p.InvitedBy != 0 {
		t.Fatalf("the token resolves to %+v, %v", p, err)
	}
	if n := f.count("SELECT COUNT(*) FROM passwords WHERE principal_id = ? AND hash = ?", p.ID, testRecord("n")); n != 1 {
		t.Error("the password is not stored")
	}
	if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ? AND can_decide = 0", p.ID); n != 2 {
		t.Errorf("%d memberships without the decide right, want 2", n)
	}
	if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ? AND channel_id = ? AND history_after = ?", p.ID, f.general.ID, last.ID); n != 1 {
		t.Error("general's membership has no history bound at its last message")
	}
	if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ? AND channel_id = ? AND history_after = 0", p.ID, f.lobby.ID); n != 1 {
		t.Error("the empty lobby's membership has no bound of 0")
	}
	if n := f.count("SELECT COUNT(*) FROM guest_visits WHERE principal_id = ?", p.ID); n != 0 {
		t.Error("an account was made a guest")
	}
	var actions []string
	for _, e := range f.audit() {
		if e.Subject == principalActor(p.ID) || strings.Contains(e.Detail, fmt.Sprintf("principal=%d", p.ID)) {
			actions = append(actions, e.Actor+" "+e.Action)
			if strings.Contains(e.Detail, a.Token) || strings.Contains(e.Detail, testRecord("n")) {
				t.Errorf("%s audits a secret", e.Action)
			}
		}
	}
	want := []string{"system principal_created", "system password_set", "system member_added", "system member_added", "system credential_created"}
	if strings.Join(actions, "|") != strings.Join(want, "|") {
		t.Errorf("audit = %q, want %q", actions, want)
	}
}

// The conversion's admission, without a permit, by the policy of the visit's
// origin; and with one, by the permit alone. A refusal writes nothing and
// spends no permit.
func TestConversionAdmission(t *testing.T) {
	f := newGuestFixture(t)
	convert := func(g CreatedGuest, name, permit string) error {
		t.Helper()
		c := GuestConversion{PrincipalID: g.Principal.ID, Name: name, Record: testRecord(name)}
		if permit != "" {
			c.PermitHash = HashSignInLinkCode(permit)
		}
		_, err := f.s.ConvertGuest(f.ctx, "system", c)
		return err
	}
	permit := func(g CreatedGuest, by Principal) schema.CreateSignInLinkResponseV1 {
		t.Helper()
		p, err := f.s.CreateConversionPermit(f.ctx, "system", by.ID, g.Principal.ID, schema.CreateConversionPermitRequestV1{}, time.Now())
		if err != nil {
			t.Fatalf("CreateConversionPermit: %v", err)
		}
		return p
	}

	// The bare address offers no account: the visit stays a guest.
	g := f.guest("")
	if err := convert(g, "one", ""); !errors.Is(err, ErrConversionNotAllowed) {
		t.Fatalf("closed policy = %v", err)
	}
	if ok, err := f.s.GuestCanConvert(f.ctx, g.Principal.ID, time.Now()); err != nil || ok {
		t.Errorf("GuestCanConvert under a closed policy = %v, %v", ok, err)
	}

	// Permits that do not work: another guest's, a member's sign-in link,
	// one of an older authentication generation, one whose maker is no
	// longer an operator, one never made.
	other := f.guest("")
	if err := convert(g, "one", permit(other, f.op).Code); !errors.Is(err, ErrConversionPermitInvalid) {
		t.Errorf("another guest's permit = %v", err)
	}
	link, err := f.s.CreateSignInLink(f.ctx, "system", f.op.ID, f.alice.ID, schema.CreateSignInLinkRequestV1{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := convert(g, "one", link.Code); !errors.Is(err, ErrConversionPermitInvalid) {
		t.Errorf("a sign-in link as a permit = %v", err)
	}
	old := permit(g, f.op)
	if _, err := f.s.db.Exec("UPDATE sign_in_links SET auth_generation = auth_generation + 7 WHERE id = ?", old.SignInLink.ID); err != nil {
		t.Fatal(err)
	}
	if err := convert(g, "one", old.Code); !errors.Is(err, ErrConversionPermitInvalid) {
		t.Errorf("a permit of another generation = %v", err)
	}
	// A guest whose generation moved on has no way in left, and converts no
	// more than an ended one.
	stale := f.guest("")
	if _, err := f.s.BumpAuthGeneration(f.ctx, "system", stale.Principal.ID, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := convert(stale, "stale", permit(other, f.op).Code); !errors.Is(err, ErrGuestEnded) {
		t.Errorf("a guest of an older generation = %v", err)
	}
	second, err := f.s.CreatePrincipal(f.ctx, PrincipalHuman, "second-operator")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE principals SET role = 'operator' WHERE id = ?", second.ID); err != nil {
		t.Fatal(err)
	}
	second.Role = RoleOperator
	demoted := permit(g, second)
	if _, err := f.s.db.Exec("UPDATE principals SET role = 'member' WHERE id = ?", second.ID); err != nil {
		t.Fatal(err)
	}
	if err := convert(g, "one", demoted.Code); !errors.Is(err, ErrConversionPermitInvalid) {
		t.Errorf("a permit whose maker is no longer an operator = %v", err)
	}
	if err := convert(g, "one", "conch_sil_never"); !errors.Is(err, ErrConversionPermitInvalid) {
		t.Errorf("a permit never made = %v", err)
	}
	if _, err := f.s.CreateConversionPermit(f.ctx, "system", f.alice.ID, g.Principal.ID, schema.CreateConversionPermitRequestV1{}, time.Now()); !errors.Is(err, ErrSignInLinkNotAllowed) {
		t.Errorf("a member's permit = %v", err)
	}
	if _, err := f.s.CreateConversionPermit(f.ctx, "system", f.op.ID, f.alice.ID, schema.CreateConversionPermitRequestV1{}, time.Now()); !errors.Is(err, ErrNotAGuest) {
		t.Errorf("a permit for a member = %v", err)
	}

	// A good permit, and a failure after every write: the permit is not
	// spent and the guest is unchanged; then it converts.
	good := permit(g, f.op)
	c := GuestConversion{PrincipalID: g.Principal.ID, Name: "one", Record: testRecord("one"), PermitHash: HashSignInLinkCode(good.Code),
		within: func(context.Context, execer) error { return errors.New("the disk is full") }}
	if _, err := f.s.ConvertGuest(f.ctx, "system", c); err == nil {
		t.Fatal("a failure inside the conversion was not returned")
	}
	if f.count("SELECT COUNT(*) FROM sign_in_links WHERE id = ? AND closed_at IS NULL AND used_at IS NULL", good.SignInLink.ID) != 1 ||
		f.count("SELECT COUNT(*) FROM passwords WHERE principal_id = ?", g.Principal.ID) != 0 || f.visit(g.Principal.ID).State != schema.GuestStateActive {
		t.Fatal("a failed conversion spent its permit or changed the guest")
	}
	c.within = nil
	out, err := f.s.ConvertGuest(f.ctx, "system", c)
	if err != nil {
		t.Fatal(err)
	}
	if out.Principal.ID != g.Principal.ID || len(out.Revoked) != 1 || out.Revoked[0] != g.Credential.ID || fmt.Sprint(out.Channels) != fmt.Sprint([]int64{f.general.ID}) {
		t.Errorf("converted = %+v", out)
	}
	if f.count("SELECT COUNT(*) FROM sign_in_links WHERE id = ? AND closed_at IS NOT NULL", good.SignInLink.ID) != 1 {
		t.Error("the permit was not spent")
	}
	if f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ? AND can_decide <> 0", g.Principal.ID) != 0 {
		t.Error("conversion wrote can_decide")
	}
	if rows := f.audit(AuditPasswordSet); len(rows) != 1 || rows[0].Detail != fmt.Sprintf("via=conversion sign_in_link=%d", good.SignInLink.ID) {
		t.Errorf("password_set rows = %+v", rows)
	}
	if _, err := f.s.CreateConversionPermit(f.ctx, "system", f.op.ID, g.Principal.ID, schema.CreateConversionPermitRequestV1{}, time.Now()); !errors.Is(err, ErrGuestEnded) {
		t.Errorf("a permit for a converted visit = %v", err)
	}

	// The policy, without a permit: the bare address offering accounts lets
	// the other guest convert, and a pause stops it.
	allowAddressAccounts(t, f.s)
	if _, err := f.s.db.Exec(`INSERT INTO server_settings (key, value, updated_at) VALUES ('joining_paused', 'true', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := convert(other, "two", ""); !errors.Is(err, ErrJoiningPaused) {
		t.Errorf("paused = %v", err)
	}
	if _, err := f.s.db.Exec(`UPDATE server_settings SET value = 'false' WHERE key = 'joining_paused'`); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.s.GuestCanConvert(f.ctx, other.Principal.ID, time.Now()); err != nil || !ok {
		t.Errorf("GuestCanConvert under an open policy = %v, %v", ok, err)
	}
	if err := convert(other, "two", ""); err != nil {
		t.Errorf("open policy = %v", err)
	}
	// Its unused permit closed with the conversion.
	if f.count("SELECT COUNT(*) FROM sign_in_links WHERE principal_id = ? AND closed_at IS NULL AND used_at IS NULL", other.Principal.ID) != 0 {
		t.Error("the converted guest still holds an open permit")
	}
}

// A visit through an invite converts while its invite admits accounts and is
// open, whatever its uses; an invite that admits guests only, a closed or
// expired one, and paused invites refuse.
func TestConversionPolicyOfInviteVisits(t *testing.T) {
	tests := []struct {
		name   string
		joinAs schema.InviteJoinMode
		after  func(f *inviteFixture, inviteID int64)
		want   error
	}{
		{"both", schema.InviteJoinBoth, nil, nil},
		{"account", schema.InviteJoinAccount, nil, nil},
		{"guest", schema.InviteJoinGuest, nil, ErrConversionNotAllowed},
		{"closed", schema.InviteJoinBoth, func(f *inviteFixture, id int64) {
			if _, err := f.s.CloseInvite(f.ctx, "system", f.op.ID, id, f.now); err != nil {
				f.t.Fatal(err)
			}
		}, ErrConversionNotAllowed},
		{"expired", schema.InviteJoinBoth, func(f *inviteFixture, id int64) {
			if _, err := f.s.db.Exec("UPDATE invites SET expires_at = ? WHERE id = ?", f.now.Add(-time.Minute).UnixMilli(), id); err != nil {
				f.t.Fatal(err)
			}
		}, ErrConversionNotAllowed},
		{"paused", schema.InviteJoinBoth, func(f *inviteFixture, _ int64) {
			if _, err := f.s.SetInvitesPaused(f.ctx, "system", true); err != nil {
				f.t.Fatal(err)
			}
		}, ErrInvitesPaused},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			req := personal("general")
			req.JoinAs = tt.joinAs
			inv := f.mustCreate(f.op, req)
			// The one use of the personal invite is spent here, by the
			// visit's admission; the conversion does not need another.
			if _, err := f.s.db.Exec("UPDATE invites SET uses = max_uses WHERE id = ?", inv.Invite.ID); err != nil {
				t.Fatal(err)
			}
			g, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Origin: schema.GuestOriginInvite, InviteID: inv.Invite.ID,
				Channels: []int64{f.general.ID}, Idle: time.Hour, Absolute: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			if tt.after != nil {
				tt.after(f, inv.Invite.ID)
			}
			_, err = f.s.ConvertGuest(f.ctx, "system", GuestConversion{PrincipalID: g.Principal.ID, Name: "member", Record: testRecord("m"), Now: f.now})
			if !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
				t.Fatalf("ConvertGuest = %v, want %v", err, tt.want)
			}
			if tt.want == nil {
				p, err := f.s.PrincipalByID(f.ctx, g.Principal.ID)
				if err != nil || p.InviteID != inv.Invite.ID || p.GuestState != schema.GuestStateConverted {
					t.Errorf("converted = %+v, %v; want its invite kept", p, err)
				}
			}
		})
	}
}
