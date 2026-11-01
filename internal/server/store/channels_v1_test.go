package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #981: the store decides who may create a channel, and the settings
// rules, under its write lock.

func TestCreateChannelV1Standing(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	bot, _ := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
	hidden, _ := s.CreatePrincipal(ctx, PrincipalHuman, "hidden")
	if _, err := s.db.ExecContext(ctx, "UPDATE principals SET disabled_at = 1, hidden_at = 1 WHERE id = ?", hidden.ID); err != nil {
		t.Fatal(err)
	}
	lim := RenameLimits{Renames: 10, Attempts: 30}
	tests := []struct {
		name    string
		caller  int64
		vis     schema.ChannelVisibility
		wantErr error
	}{
		{"member, public", alice.ID, schema.ChannelPublic, nil},
		{"member, private", alice.ID, schema.ChannelPrivate, nil},
		{"agent", bot.ID, schema.ChannelPublic, ErrNotCommunityMember},
		{"hidden human", hidden.ID, schema.ChannelPublic, ErrNotCommunityMember},
		{"unknown principal", 9999, schema.ChannelPublic, ErrNotCommunityMember},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := s.CreateChannelV1(ctx, ChannelCreate{CallerID: tt.caller, Name: "room-" + string(tt.vis) + "-" + tt.name, Visibility: tt.vis, Limits: lim}, time.Now())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			got, err := s.ChannelByID(ctx, ch.ID)
			if err != nil || got.Visibility != tt.vis || got.IsDefault {
				t.Errorf("stored channel = %+v, %v", got, err)
			}
		})
	}
}

func TestChannelSettingsStore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	root, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	ch, err := s.CreateChannelAs(ctx, "system", "room", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	public, private, yes := schema.ChannelPublic, schema.ChannelPrivate, true

	// A member, holding no manage_channels, is refused before anything is
	// read: the same for a channel that exists and one that does not.
	for _, id := range []int64{ch.ID, 9999} {
		if _, err := s.ChannelSettings(ctx, "principal:2", alice.ID, id, ChannelSettingsChange{Visibility: &public}, time.Now()); !errors.Is(err, ErrManageChannelsRequired) {
			t.Errorf("member on %d: err = %v, want ErrManageChannelsRequired", id, err)
		}
	}
	if _, err := s.ChannelSettings(ctx, "principal:1", root.ID, 9999, ChannelSettingsChange{Visibility: &public}, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown channel: err = %v, want ErrNotFound", err)
	}
	// Marking a private channel default, alone, conflicts and writes nothing.
	if _, err := s.ChannelSettings(ctx, "principal:1", root.ID, ch.ID, ChannelSettingsChange{Default: &yes}, time.Now()); !errors.Is(err, ErrChannelSettingsConflict) {
		t.Fatalf("private default: err = %v", err)
	}
	if got, _ := s.ChannelByID(ctx, ch.ID); got.IsDefault || got.Visibility != schema.ChannelPrivate {
		t.Errorf("after the conflict = %+v", got)
	}
	// Public and default at once; then asking for the same writes nothing.
	res, err := s.ChannelSettings(ctx, "principal:1", root.ID, ch.ID, ChannelSettingsChange{Visibility: &public, Default: &yes}, time.Now())
	if err != nil || !res.Changed || !res.Channel.IsDefault || res.Channel.Visibility != schema.ChannelPublic {
		t.Fatalf("public default = %+v, %v", res, err)
	}
	if res, err := s.ChannelSettings(ctx, "principal:1", root.ID, ch.ID, ChannelSettingsChange{Default: &yes}, time.Now()); err != nil || res.Changed {
		t.Errorf("no-op = %+v, %v", res, err)
	}
	// Private while default conflicts.
	if _, err := s.ChannelSettings(ctx, "principal:1", root.ID, ch.ID, ChannelSettingsChange{Visibility: &private}, time.Now()); !errors.Is(err, ErrChannelSettingsConflict) {
		t.Errorf("default turned private: err = %v", err)
	}
}
