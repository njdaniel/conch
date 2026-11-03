package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #982: the store decides who may join and leave a channel under its
// write lock.

func TestJoinChannelStanding(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	bot, _ := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
	hidden, _ := s.CreatePrincipal(ctx, PrincipalHuman, "hidden")
	if _, err := s.db.ExecContext(ctx, "UPDATE principals SET disabled_at = 1, hidden_at = 1 WHERE id = ?", hidden.ID); err != nil {
		t.Fatal(err)
	}
	pub, err := s.CreateChannel(ctx, "square")
	if err != nil {
		t.Fatal(err)
	}
	public := schema.ChannelPublic
	if _, err := s.ChannelSettings(ctx, "system", 0, pub.ID, ChannelSettingsChange{Visibility: &public}, time.Now()); err != nil {
		t.Fatal(err)
	}
	priv, err := s.CreateChannel(ctx, "back-room")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		caller  int64
		channel int64
		wantErr error
	}{
		{"member, public", alice.ID, pub.ID, nil},
		{"member, private", alice.ID, priv.ID, ErrNotFound},
		{"member, no such channel", alice.ID, 9999, ErrNotFound},
		{"agent", bot.ID, pub.ID, ErrChannelNotJoinable},
		{"hidden human", hidden.ID, pub.ID, ErrChannelNotJoinable},
		{"unknown principal", 9999, pub.ID, ErrChannelNotJoinable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := s.JoinChannel(ctx, tt.channel, tt.caller, time.Now())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				if in, err := s.IsChannelMember(ctx, tt.channel, tt.caller); err != nil || in {
					t.Errorf("refused caller is a member: %v, %v", in, err)
				}
				return
			}
			if !res.Added || res.Member.CanDecide || res.Member.Manages || res.Member.HistoryAfter != 0 || res.Member.AddedBy != tt.caller {
				t.Errorf("membership = %+v, want added, can_decide 0, manages 0, no bound, added by the member", res)
			}
		})
	}
}

func TestLeaveChannelStore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	bob, _ := s.CreatePrincipal(ctx, PrincipalHuman, "bob")
	priv, err := s.CreateChannel(ctx, "back-room")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Principal{alice, bob} {
		if _, err := s.AddChannelMember(ctx, "system", priv.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.LeaveChannel(ctx, 9999, alice.ID, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("leave of no channel = %v, want ErrNotFound", err)
	}
	carol, _ := s.CreatePrincipal(ctx, PrincipalHuman, "carol")
	if err := s.LeaveChannel(ctx, priv.ID, carol.ID, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("leave by a non-member = %v, want ErrNotFound", err)
	}
	if err := s.LeaveChannel(ctx, priv.ID, alice.ID, time.Now()); err != nil {
		t.Fatalf("alice leaves = %v", err)
	}
	if err := s.LeaveChannel(ctx, priv.ID, bob.ID, time.Now()); !errors.Is(err, ErrChannelLastMember) {
		t.Errorf("last member leaves a private channel = %v, want ErrChannelLastMember", err)
	}
	if in, err := s.IsChannelMember(ctx, priv.ID, bob.ID); err != nil || !in {
		t.Errorf("bob after the refusal: member %v, %v", in, err)
	}
}

func TestChannelDirectoryStore(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	alice, _ := s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	public := schema.ChannelPublic
	mk := func(name string, pub bool, members ...int64) Channel {
		t.Helper()
		ch, err := s.CreateChannel(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		if pub {
			if _, err := s.ChannelSettings(ctx, "system", 0, ch.ID, ChannelSettingsChange{Visibility: &public}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		for _, id := range members {
			if _, err := s.AddChannelMember(ctx, "system", ch.ID, id, 0); err != nil {
				t.Fatal(err)
			}
		}
		return ch
	}
	open := mk("open", true)
	mine := mk("mine", false, alice.ID)
	theirs := mk("theirs", false)

	names := func(ls []ChannelListing) map[string]ChannelListing {
		m := map[string]ChannelListing{}
		for _, l := range ls {
			m[l.Name] = l
		}
		return m
	}
	member, err := s.ChannelDirectory(ctx, alice.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	got := names(member)
	if _, ok := got[theirs.Name]; ok {
		t.Errorf("member view lists a private channel alice is not in: %+v", got)
	}
	if l, ok := got[open.Name]; !ok || l.Joined || l.Members != -1 {
		t.Errorf("member view of open = %+v (listed %v)", l, ok)
	}
	if l, ok := got[mine.Name]; !ok || !l.Joined || l.Members != -1 {
		t.Errorf("member view of mine = %+v (listed %v)", l, ok)
	}
	all, err := s.ChannelDirectory(ctx, alice.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	got = names(all)
	for name, n := range map[string]int64{open.Name: 0, mine.Name: 1, theirs.Name: 0} {
		if l, ok := got[name]; !ok || l.Members != n {
			t.Errorf("scope=all %s = %+v (listed %v), want %d members", name, l, ok, n)
		}
	}
}
