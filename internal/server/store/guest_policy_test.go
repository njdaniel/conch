package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The guest entry policy in the store (issue #710): how the settings are
// read, and what the admitting transactions refuse and leave unwritten. The
// routes' matrix is in internal/server/join_test.go.

func boolRef(b bool) *bool    { return &b }
func int64Ref(n int64) *int64 { return &n }

// A settings row that cannot be read fails closed: no switch admits, a pause
// holds, a mode is off and a number is its default.
func TestJoinSettingsFailClosed(t *testing.T) {
	f := newInviteFixture(t)
	got, err := joinSettingsTx(f.ctx, f.s.db)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(got) != fmt.Sprint(schema.DefaultJoinSettings()) {
		t.Errorf("no rows = %+v, want the defaults", got)
	}
	for key, value := range map[string]string{
		settingGuestsEnabled: "yes please", settingJoiningPaused: "nah", settingJoinVoice: "maybe",
		settingOpenJoining: "sometimes", settingMaxGuests: "lots", settingGuestIdleSeconds: "",
	} {
		if _, err := f.s.db.Exec("INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, 0)", key, value); err != nil {
			t.Fatal(err)
		}
	}
	got, err = joinSettingsTx(f.ctx, f.s.db)
	if err != nil {
		t.Fatal(err)
	}
	want := schema.DefaultJoinSettings()
	want.JoiningPaused = true
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("unreadable rows = %+v, want %+v", got, want)
	}
}

// A refused settings change writes nothing: not the fields before the
// refusal, not an audit row.
func TestPutJoinSettingsRefusalWritesNothing(t *testing.T) {
	f := newInviteFixture(t)
	tests := []struct {
		name string
		req  schema.PutJoinSettingsRequestV1
		kind error
	}{
		{"guest-only with guests off", schema.PutJoinSettingsRequestV1{JoinVoice: boolRef(true), OpenJoining: ptrMode(schema.OpenJoiningGuest),
			OpenChannels: &[]string{"general"}}, ErrJoinPolicyConflict},
		{"a channel that does not exist", schema.PutJoinSettingsRequestV1{JoinVoice: boolRef(true), OpenChannels: &[]string{"nosuch"}}, ErrJoinSettingsInvalid},
		{"a bound out of range", schema.PutJoinSettingsRequestV1{JoinVoice: boolRef(true), MaxGuests: int64Ref(schema.MaxMaxGuests + 1)}, ErrJoinSettingsInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := f.s.PutJoinSettings(f.ctx, "system", f.op.ID, tt.req)
			var refused *JoinSettingsError
			if !errors.As(err, &refused) || !errors.Is(err, tt.kind) || refused.Reason == "" {
				t.Fatalf("err = %v, want a %v", err, tt.kind)
			}
		})
	}
	if n := f.count("SELECT COUNT(*) FROM server_settings WHERE key <> ?", settingInvitesPaused); n != 0 {
		t.Errorf("refusals wrote %d settings rows", n)
	}
	if n := f.count("SELECT COUNT(*) FROM audit_events WHERE action IN (?, ?)", AuditJoinSettingsChanged, AuditOpenChannelAdded); n != 0 {
		t.Errorf("refusals wrote %d audit rows", n)
	}
}

func ptrMode(m schema.OpenJoiningMode) *schema.OpenJoiningMode { return &m }

// A guest redemption that a cap refuses spends no use of the invite and
// makes nothing; one that succeeds spends one.
func TestRedeemInviteGuestFullSpendsNothing(t *testing.T) {
	f := newInviteFixture(t)
	if _, err := f.s.PutJoinSettings(f.ctx, "system", f.op.ID, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolRef(true), MaxGuests: int64Ref(1)}); err != nil {
		t.Fatal(err)
	}
	req := open("general")
	req.JoinAs = schema.InviteJoinBoth
	inv := f.mustCreate(f.op, req)
	if _, err := f.s.RedeemInviteGuest(f.ctx, HashInviteCode(inv.Code), GuestEntry{}, f.now); err != nil {
		t.Fatal(err)
	}
	principals := f.count("SELECT COUNT(*) FROM principals")
	_, err := f.s.RedeemInviteGuest(f.ctx, HashInviteCode(inv.Code), GuestEntry{}, f.now)
	if !errors.Is(err, ErrGuestsFull) {
		t.Fatalf("over the cap: %v", err)
	}
	if n := f.count("SELECT uses FROM invites WHERE id = ?", inv.Invite.ID); n != 1 {
		t.Errorf("uses = %d, want the one success only", n)
	}
	if n := f.count("SELECT COUNT(*) FROM principals"); n != principals {
		t.Errorf("a refused visit made a principal: %d -> %d", principals, n)
	}
	// The visit's deadline is the earlier of the absolute bound and the
	// invite's credential lifetime: the day bound here.
	visits, _, err := f.s.ListGuests(f.ctx, GuestFilter{State: schema.GuestStateActive, Limit: 10})
	if err != nil || len(visits) != 1 {
		t.Fatalf("visits = %+v, %v", visits, err)
	}
	if d := visits[0].ExpiresAt.Sub(visits[0].StartedAt); d != schema.DefaultGuestAbsoluteSeconds*time.Second {
		t.Errorf("visit lasts %v", d)
	}
}

// The predicates the previews and the writes share.
func TestGuestEntryPredicates(t *testing.T) {
	on := schema.DefaultJoinSettings()
	on.GuestsEnabled, on.OpenJoining, on.MaxGuests, on.MaxGuestsPerAddress = true, schema.OpenJoiningBoth, 2, 1
	tests := []struct {
		name                     string
		set                      schema.JoinSettingsV1
		channels, live, fromAddr int
		want                     schema.JoinOptionsV1
	}{
		{"open", on, 1, 0, 0, schema.JoinOptionsV1{Guest: true}},
		{"no channel", on, 0, 0, 0, schema.JoinOptionsV1{}},
		{"server cap", on, 1, 2, 0, schema.JoinOptionsV1{GuestsFull: true}},
		{"address cap", on, 1, 1, 1, schema.JoinOptionsV1{GuestsFull: true}},
		{"uncapped", func() schema.JoinSettingsV1 { s := on; s.MaxGuests = 0; return s }(), 1, 9000, 0, schema.JoinOptionsV1{Guest: true}},
		{"paused", func() schema.JoinSettingsV1 { s := on; s.JoiningPaused = true; return s }(), 1, 0, 0, schema.JoinOptionsV1{Paused: true}},
		{"accounts only", func() schema.JoinSettingsV1 { s := on; s.OpenJoining = schema.OpenJoiningAccount; return s }(), 1, 0, 0, schema.JoinOptionsV1{}},
		{"guests off", func() schema.JoinSettingsV1 { s := on; s.GuestsEnabled = false; return s }(), 1, 0, 0, schema.JoinOptionsV1{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AddressJoinOptions(tt.set, tt.channels, tt.live, tt.fromAddr); got != tt.want {
				t.Errorf("AddressJoinOptions = %+v, want %+v", got, tt.want)
			}
		})
	}
}
