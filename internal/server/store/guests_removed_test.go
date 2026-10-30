package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Guests are removed for now (issue #1005): the settings refuse guests,
// invites admit accounts only, and Open turns off what a database from
// before the change still admits. The helpers write that older state
// underneath the refusals, for the tests of the code kept for it.

// legacyJoinSettings applies req as PutJoinSettings did before issue #1005,
// guests and all, without checks or audit: what such a database holds.
func legacyJoinSettings(t testing.TB, s *Store, req schema.PutJoinSettingsRequestV1) {
	t.Helper()
	ctx := context.Background()
	err := s.withImmediateTx(ctx, func(tx execer) error {
		cur, err := joinSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		next := req.Apply(cur)
		if _, err := writeJoinSettingsTx(ctx, tx, cur, next, time.Now()); err != nil {
			return err
		}
		if req.OpenChannels == nil {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM open_channels"); err != nil {
			return err
		}
		for _, name := range next.OpenChannels {
			if _, err := tx.ExecContext(ctx, `INSERT INTO open_channels (channel_id, added_at) SELECT id, 0 FROM channels WHERE name = ?`, name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("legacy join settings: %v", err)
	}
}

// legacyCreateInvite makes an invite as CreateInvite did before issue
// #1005: one whose join_as may admit guests.
func legacyCreateInvite(t testing.TB, s *Store, actor string, creatorID int64, req schema.CreateInviteRequestV1, opts InviteOptions, now time.Time) schema.CreateInviteResponseV1 {
	t.Helper()
	mode := req.EffectiveJoinAs()
	req.JoinAs = schema.InviteJoinAccount
	resp, err := s.CreateInvite(context.Background(), actor, creatorID, req, opts, now)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if _, err := s.db.Exec("UPDATE invites SET join_as = ? WHERE id = ?", string(mode), resp.Invite.ID); err != nil {
		t.Fatal(err)
	}
	resp.Invite.JoinAs = mode
	return resp
}

// joinRows reads the joining settings rows as stored.
func joinRows(t *testing.T, s *Store) map[string]string {
	t.Helper()
	rows, err := s.db.Query("SELECT key, value FROM server_settings WHERE key IN ('guests_enabled', 'open_joining')")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

// A database whose settings admit guests opens with them off: guests
// disabled, both made account and guest made off, the rest kept, one
// join_settings_changed row as system that says so, and a live visit left
// live. Opening it again changes and writes nothing.
func TestOpenTurnsGuestsOff(t *testing.T) {
	tests := []struct {
		name    string
		enabled string
		mode    schema.OpenJoiningMode
		want    schema.OpenJoiningMode
		detail  string // "" when nothing changes
	}{
		{"both, guests on", "true", schema.OpenJoiningBoth, schema.OpenJoiningAccount,
			"open_joining=both->account guests_enabled=true->false live_guests=1 reason=guests_removed"},
		{"guest-only, guests on", "true", schema.OpenJoiningGuest, schema.OpenJoiningOff,
			"open_joining=guest->off guests_enabled=true->false live_guests=1 reason=guests_removed"},
		{"accounts, guests on", "true", schema.OpenJoiningAccount, schema.OpenJoiningAccount,
			"guests_enabled=true->false live_guests=1 reason=guests_removed"},
		{"off, guests on", "true", schema.OpenJoiningOff, schema.OpenJoiningOff,
			"guests_enabled=true->false live_guests=1 reason=guests_removed"},
		{"both, guests off", "false", schema.OpenJoiningBoth, schema.OpenJoiningAccount,
			"open_joining=both->account live_guests=1 reason=guests_removed"},
		{"accounts, guests off", "false", schema.OpenJoiningAccount, schema.OpenJoiningAccount, ""},
		{"off, guests off", "false", schema.OpenJoiningOff, schema.OpenJoiningOff, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "conch.db")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			ch, err := s.CreateChannel(ctx, "general")
			if err != nil {
				t.Fatal(err)
			}
			g, err := s.CreateGuest(ctx, "system", NewGuest{Origin: schema.GuestOriginAddress, Channels: []int64{ch.ID}, Idle: time.Hour, Absolute: 2 * time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			// Underneath the store, as a server from before issue #1005 left
			// them.
			for k, v := range map[string]string{"guests_enabled": tt.enabled, "open_joining": string(tt.mode), "max_guests": "7"} {
				if _, err := s.db.Exec(`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, 0)
					ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec("INSERT INTO open_channels (channel_id, added_at) VALUES (?, 0)", ch.ID); err != nil {
				t.Fatal(err)
			}
			countRows := func(s *Store) int {
				var n int
				if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action = ?", AuditJoinSettingsChanged).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			before := countRows(s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}

			for i := range 2 {
				s, err = Open(ctx, path)
				if err != nil {
					t.Fatalf("open %d: %v", i+1, err)
				}
				set, live, err := s.JoinSettings(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if set.GuestsEnabled || set.OpenJoining != tt.want || set.MaxGuests != 7 || len(set.OpenChannels) != 1 {
					t.Errorf("open %d: settings = %+v, want guests off, mode %s, the rest kept", i+1, set, tt.want)
				}
				if got := joinRows(t, s); got["guests_enabled"] != "false" || got["open_joining"] != string(tt.want) {
					t.Errorf("open %d: stored rows = %v", i+1, got)
				}
				if live != 1 {
					t.Errorf("open %d: live visits = %d, want the visit kept", i+1, live)
				}
				var rows []AuditEvent
				for _, e := range allAuditEvents(t, s) {
					if e.Action == AuditJoinSettingsChanged {
						rows = append(rows, e)
					}
				}
				wantRows := before
				if tt.detail != "" {
					wantRows++
				}
				if len(rows) != wantRows {
					t.Fatalf("open %d: %d join_settings_changed rows, want %d", i+1, len(rows), wantRows)
				}
				if tt.detail != "" {
					last := rows[len(rows)-1]
					if last.Actor != "system" || last.Subject != "server" || last.Detail != tt.detail {
						t.Errorf("open %d: audit row = %+v, want system on server with %q", i+1, last, tt.detail)
					}
				}
				if v, err := s.GuestVisitOf(ctx, g.Principal.ID); err != nil || v.State != schema.GuestStateActive {
					t.Errorf("open %d: the visit = %+v, %v; want it live", i+1, v, err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// The settings refuse every request for guests, writing nothing, and any
// change they make leaves guests off whatever the rows held; an invite
// admits accounts only.
func TestGuestsRemovedRefusals(t *testing.T) {
	on, off := true, false
	guest, both, account := schema.OpenJoiningGuest, schema.OpenJoiningBoth, schema.OpenJoiningAccount
	for _, tt := range []struct {
		name string
		req  schema.PutJoinSettingsRequestV1
	}{
		{"guests on", schema.PutJoinSettingsRequestV1{GuestsEnabled: &on}},
		{"guest-only", schema.PutJoinSettingsRequestV1{OpenJoining: &guest, OpenChannels: &[]string{"general"}}},
		{"both", schema.PutJoinSettingsRequestV1{OpenJoining: &both, OpenChannels: &[]string{"general"}}},
		{"both with guests off", schema.PutJoinSettingsRequestV1{GuestsEnabled: &off, OpenJoining: &both, OpenChannels: &[]string{"general"}}},
		{"guests on with accounts", schema.PutJoinSettingsRequestV1{GuestsEnabled: &on, OpenJoining: &account, OpenChannels: &[]string{"general"}}},
	} {
		t.Run("settings "+tt.name, func(t *testing.T) {
			f := newInviteFixture(t)
			_, err := f.s.PutJoinSettings(f.ctx, "system", f.op.ID, tt.req)
			var refused *JoinSettingsError
			if !errors.As(err, &refused) || !errors.Is(err, ErrJoinPolicyConflict) || !strings.Contains(refused.Reason, "guests") {
				t.Fatalf("PutJoinSettings = %v, want a conflict about guests", err)
			}
			if n := f.count("SELECT COUNT(*) FROM server_settings WHERE key IN ('guests_enabled', 'open_joining')"); n != 0 {
				t.Errorf("a refusal wrote %d rows", n)
			}
		})
	}
	t.Run("a change over rows that admit guests", func(t *testing.T) {
		f := newInviteFixture(t)
		legacyJoinSettings(t, f.s, schema.PutJoinSettingsRequestV1{GuestsEnabled: &on, OpenJoining: &both, OpenChannels: &[]string{"general"}})
		st, err := f.s.PutJoinSettings(f.ctx, "system", f.op.ID, schema.PutJoinSettingsRequestV1{MaxGuests: int64Ref(3)})
		if err != nil {
			t.Fatal(err)
		}
		if st.Settings.GuestsEnabled || st.Settings.OpenJoining != schema.OpenJoiningAccount {
			t.Errorf("settings = %+v, want guests off and accounts", st.Settings)
		}
	})
	for _, mode := range []schema.InviteJoinMode{schema.InviteJoinGuest, schema.InviteJoinBoth} {
		t.Run(fmt.Sprintf("invite join_as %s", mode), func(t *testing.T) {
			f := newInviteFixture(t)
			req := open("general")
			req.JoinAs = mode
			if _, err := f.create(f.op, req); !errors.Is(err, ErrGuestsRemoved) {
				t.Fatalf("CreateInvite = %v, want ErrGuestsRemoved", err)
			}
			if n := f.count("SELECT COUNT(*) FROM invites"); n != 0 {
				t.Errorf("%d invites after the refusal", n)
			}
		})
	}
	// With the policy as it now always is, neither entry point of the store
	// starts a visit.
	f := newInviteFixture(t)
	req := open("general")
	req.JoinAs = schema.InviteJoinBoth
	inv := legacyCreateInvite(t, f.s, "system", f.op.ID, req, InviteOptions{}, f.now)
	legacyJoinSettings(t, f.s, schema.PutJoinSettingsRequestV1{OpenJoining: &account, OpenChannels: &[]string{"general"}})
	if _, err := f.s.JoinAsGuest(f.ctx, GuestEntry{}); !errors.Is(err, ErrGuestsDisabled) {
		t.Errorf("JoinAsGuest = %v, want ErrGuestsDisabled", err)
	}
	if _, err := f.s.RedeemInviteGuest(f.ctx, HashInviteCode(inv.Code), GuestEntry{}, f.now); !errors.Is(err, ErrGuestsDisabled) {
		t.Errorf("RedeemInviteGuest = %v, want ErrGuestsDisabled", err)
	}
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM guest_visits").Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d visits", n)
	}
}
