package store

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// PushTargets (issue #1715): whom a new message wakes. The store is asked per
// message, after the post, so every row here changes one fact and asks again.

type pushTargetFixture struct {
	*feedFixture
	reg schema.PushRegistrationV1 // dave's phone
}

func newPushTargetFixture(t *testing.T) *pushTargetFixture {
	t.Helper()
	f := &pushTargetFixture{feedFixture: newFeedFixture(t)}
	f.reg = registerPush(t, f.s, f.cred, webPushTarget("-dave"))
	return f
}

// targets asks PushTargets about message id in channel, by author, now.
func (f *pushTargetFixture) targets(t *testing.T, channelID, messageID int64) []string {
	t.Helper()
	got, err := f.s.PushTargets(context.Background(), channelID, messageID, time.Now())
	if err != nil {
		t.Fatalf("PushTargets: %v", err)
	}
	ids := []string{}
	for _, g := range got {
		ids = append(ids, g.RegistrationID)
	}
	return ids
}

func (f *pushTargetFixture) wakes(t *testing.T, channelID, messageID int64, _ Principal) bool {
	t.Helper()
	return slices.Contains(f.targets(t, channelID, messageID), f.reg.ID)
}

func (f *pushTargetFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// TestPushTargetsPolicy: the credential's policy and mutes, over the messages
// of the feed's own fixture (the feed and the wake agree).
func TestPushTargetsPolicy(t *testing.T) {
	f := newPushTargetFixture(t)
	ctx := context.Background()
	if _, err := f.s.AddChannelMember(ctx, "system", f.other.ID, f.dave.ID, 0); err != nil {
		t.Fatal(err)
	}
	type msg struct {
		channel int64
		id      int64
		author  Principal
	}
	m := map[string]msg{}
	add := func(name string, channel int64, author Principal, id int64) { m[name] = msg{channel, id, author} }
	add("plain", f.ch.ID, f.frank, f.post(t, f.ch.ID, f.frank, "hello there"))
	add("mention", f.ch.ID, f.frank, f.post(t, f.ch.ID, f.frank, "ping @dave please"))
	add("other name", f.ch.ID, f.frank, f.post(t, f.ch.ID, f.frank, "ping @davey please"))
	add("whisper", f.ch.ID, f.erin, f.whisper(t, f.erin, "psst", f.dave))
	add("whisper to someone else", f.ch.ID, f.erin, f.whisper(t, f.erin, "psst @dave", f.frank))
	add("own", f.ch.ID, f.dave, f.post(t, f.ch.ID, f.dave, "@dave note to self"))
	hn, _ := f.toNet(t, f.erin, f.n2, "net two @dave")
	add("net dave is not in", f.ch.ID, f.erin, hn)
	add("elsewhere", f.other.ID, f.frank, f.post(t, f.other.ID, f.frank, "in the other channel"))
	add("removed", f.ch.ID, f.frank, f.post(t, f.ch.ID, f.frank, "@dave removed"))
	f.exec(t, "INSERT INTO message_removals (message_id, channel_id, removed_at) VALUES (?, ?, 1)", m["removed"].id, f.ch.ID)
	add("nonexistent", f.ch.ID, f.frank, 987654)
	add("wrong channel", f.other.ID, f.frank, m["plain"].id) // a real message, named under another channel

	tests := []struct {
		policy schema.NotificationPolicy
		muted  []int64
		want   map[string]bool
	}{
		{schema.NotificationPolicyOff, nil, map[string]bool{}},
		{schema.NotificationPolicyMentions, nil, map[string]bool{"mention": true, "whisper": true}},
		{schema.NotificationPolicyAll, nil, map[string]bool{"plain": true, "mention": true, "other name": true, "whisper": true, "elsewhere": true}},
		{schema.NotificationPolicyAll, []int64{f.other.ID}, map[string]bool{"plain": true, "mention": true, "other name": true, "whisper": true}},
		{schema.NotificationPolicyAll, []int64{f.ch.ID}, map[string]bool{"elsewhere": true}},
		{schema.NotificationPolicyMentions, []int64{f.ch.ID}, map[string]bool{}},
		{schema.NotificationPolicyMentions, []int64{987654}, map[string]bool{"mention": true, "whisper": true}},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s muted %v", tt.policy, tt.muted), func(t *testing.T) {
			f.policy(t, tt.policy, false, tt.muted...)
			for name, ms := range m {
				if got := f.wakes(t, ms.channel, ms.id, ms.author); got != tt.want[name] {
					t.Errorf("%s: wakes = %v, want %v", name, got, tt.want[name])
				}
			}
		})
	}
}

// TestPushTargetsAgreeWithTheFeed: for every message of the feed's fixture and
// every policy, a message wakes the credential exactly when the feed lists it.
func TestPushTargetsAgreeWithTheFeed(t *testing.T) {
	f := newPushTargetFixture(t)
	ctx := context.Background()
	if _, err := f.s.AddChannelMember(ctx, "system", f.other.ID, f.dave.ID, 0); err != nil {
		t.Fatal(err)
	}
	type msg struct {
		channel int64
		id      int64
		author  Principal
	}
	var all []msg
	for _, b := range []string{"hello", "@dave hi", "@DAVE shout", "no @davex"} {
		all = append(all, msg{f.ch.ID, f.post(t, f.ch.ID, f.frank, b), f.frank})
	}
	all = append(all, msg{f.other.ID, f.post(t, f.other.ID, f.erin, "other @dave"), f.erin})
	all = append(all, msg{f.ch.ID, f.whisper(t, f.alice, "psst", f.dave), f.alice})
	all = append(all, msg{f.ch.ID, f.whisper(t, f.alice, "psst", f.frank), f.alice})
	nid, _ := f.toNet(t, f.alice, f.n1, "net @dave")
	all = append(all, msg{f.ch.ID, nid, f.alice})
	nid2, _ := f.toNet(t, f.erin, f.n2, "net two")
	all = append(all, msg{f.ch.ID, nid2, f.erin})
	for _, policy := range []schema.NotificationPolicy{schema.NotificationPolicyOff, schema.NotificationPolicyMentions, schema.NotificationPolicyAll} {
		f.policy(t, policy, false, f.other.ID)
		listed := map[int64]bool{}
		for _, s := range f.feed(t, 0).Summaries {
			listed[s.MessageID] = true
		}
		for _, ms := range all {
			if got := f.wakes(t, ms.channel, ms.id, ms.author); got != listed[ms.id] {
				t.Errorf("policy %s, message %d: wakes = %v, the feed lists it = %v", policy, ms.id, got, listed[ms.id])
			}
		}
	}
}

// TestPushTargetsFacts: each row breaks one fact a wake rests on, with the
// registration itself still active, so the query is what refuses.
func TestPushTargetsFacts(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(t *testing.T, f *pushTargetFixture)
	}{
		{"the principal was removed from the channel", func(t *testing.T, f *pushTargetFixture) {
			if _, err := f.s.RemoveChannelMember(context.Background(), "system", f.ch.ID, f.dave.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"the principal was banned", func(t *testing.T, f *pushTargetFixture) {
			if _, err := f.s.BanChannelMember(context.Background(), "system", f.ch.ID, f.dave.ID, 0); err != nil {
				t.Fatal(err)
			}
		}},
		{"the credential was revoked", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "UPDATE credentials SET revoked_at = 1 WHERE id = ?", f.cred)
		}},
		{"the credential expired", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "UPDATE credentials SET expires_at = ? WHERE id = ?", time.Now().Add(-time.Minute).UnixMilli(), f.cred)
		}},
		{"the credential went idle past its bound", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "UPDATE credentials SET idle_ms = 1000, last_used_at = ? WHERE id = ?", time.Now().Add(-time.Hour).UnixMilli(), f.cred)
		}},
		{"a restore moved the generation on (revoked_at unset)", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "UPDATE principals SET auth_generation = auth_generation + 1 WHERE id = ?", f.dave.ID)
		}},
		{"the credential's scope is not full", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "UPDATE credentials SET scope = 'setup' WHERE id = ?", f.cred)
		}},
		{"the principal was disabled", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "UPDATE principals SET disabled_at = 1 WHERE id = ?", f.dave.ID)
		}},
		{"the principal was hidden", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "UPDATE principals SET disabled_at = 1, hidden_at = 1 WHERE id = ?", f.dave.ID)
		}},
		{"the registration ended", func(t *testing.T, f *pushTargetFixture) {
			if err := f.s.DeletePushRegistration(context.Background(), f.cred, f.reg.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
		}},
		{"the message was removed", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "INSERT INTO message_removals (message_id, channel_id, removed_at) SELECT id, channel_id, 1 FROM messages")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPushTargetFixture(t)
			f.policy(t, schema.NotificationPolicyAll, false)
			id := f.post(t, f.ch.ID, f.frank, "hello @dave")
			if !f.wakes(t, f.ch.ID, id, f.frank) {
				t.Fatal("the control did not wake before the fact was broken")
			}
			tt.break_(t, f)
			if f.wakes(t, f.ch.ID, id, f.frank) {
				t.Error("woke although the fact no longer holds")
			}
		})
	}
	t.Run("the author is never woken", func(t *testing.T) {
		f := newPushTargetFixture(t)
		f.policy(t, schema.NotificationPolicyAll, false)
		id := f.post(t, f.ch.ID, f.dave, "@dave to me")
		if f.wakes(t, f.ch.ID, id, f.dave) {
			t.Error("the author's own phone was woken")
		}
	})
	t.Run("only webpush registrations, only active ones, and only the right principals", func(t *testing.T) {
		f := newPushTargetFixture(t)
		f.policy(t, schema.NotificationPolicyAll, false)
		// An agent has no registration; a person outside the channel does.
		ghost, err := f.s.CreatePrincipal(context.Background(), PrincipalHuman, "ghost")
		if err != nil {
			t.Fatal(err)
		}
		gc := credentialOf(t, f.s, ghost, "phone")
		putPrefs(t, f.s, gc, schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{}})
		gr := registerPush(t, f.s, gc, webPushTarget("-ghost"))
		relay := registerPush(t, f.s, f.cred, relayTarget())
		id := f.post(t, f.ch.ID, f.frank, "hello")
		got := f.targets(t, f.ch.ID, id)
		if slices.Contains(got, gr.ID) {
			t.Error("a non-member's registration was woken")
		}
		if slices.Contains(got, relay.ID) {
			t.Error("a relay registration was woken by the webpush sender")
		}
		if !slices.Contains(got, f.reg.ID) {
			t.Errorf("targets = %v, want dave's", got)
		}
	})
	t.Run("the principal's current name decides a mention", func(t *testing.T) {
		f := newPushTargetFixture(t)
		f.policy(t, schema.NotificationPolicyMentions, false)
		id := f.post(t, f.ch.ID, f.frank, "hey @dave2")
		if f.wakes(t, f.ch.ID, id, f.frank) {
			t.Fatal("a mention of another name woke dave")
		}
		f.exec(t, "UPDATE principals SET name = 'dave2' WHERE id = ?", f.dave.ID)
		if !f.wakes(t, f.ch.ID, id, f.frank) {
			t.Error("the renamed principal's new name was not matched")
		}
	})
}

// TestPushTargetsAreBounded: with more candidates than the scan bound the
// answer is cut at the bound, never read whole. The bound is lowered for the
// test.
func TestPushTargetsAreBounded(t *testing.T) {
	f := newPushTargetFixture(t)
	f.policy(t, schema.NotificationPolicyAll, false)
	ctx := context.Background()
	// Candidates: dave's, and three more people's, each a member with a phone.
	for _, name := range []string{"p1", "p2", "p3"} {
		p, err := f.s.CreatePrincipal(ctx, PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.AddChannelMember(ctx, "system", f.ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
		c := credentialOf(t, f.s, p, "phone")
		putPrefs(t, f.s, c, schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{}})
		registerPush(t, f.s, c, webPushTarget("-"+name))
	}
	id := f.post(t, f.ch.ID, f.alice, "hello")
	all := f.targets(t, f.ch.ID, id)
	if len(all) != 4 {
		t.Fatalf("unbounded targets = %d, want 4", len(all))
	}
	old := pushTargetScanBound
	pushTargetScanBound = 2
	t.Cleanup(func() { pushTargetScanBound = old })
	if got := f.targets(t, f.ch.ID, id); len(got) != 2 {
		t.Errorf("with a bound of 2 the answer has %d targets, want 2 (cut, not read whole)", len(got))
	}
	if PushTargetScanBound < 1 || PushTargetScanBound > 100000 {
		t.Errorf("PushTargetScanBound = %d", PushTargetScanBound)
	}
}

// TestPushTargetsHistoryBoundAndGuests: a message before a member's history
// bound does not wake them, and a guest is woken only for what is after their
// admission and only while their visit lasts.
func TestPushTargetsHistoryBoundAndGuests(t *testing.T) {
	t.Run("a member's history bound", func(t *testing.T) {
		f := newPushTargetFixture(t)
		f.policy(t, schema.NotificationPolicyAll, false)
		before := f.post(t, f.ch.ID, f.frank, "before the bound")
		f.exec(t, "UPDATE channel_members SET history_after = ? WHERE channel_id = ? AND principal_id = ?", before, f.ch.ID, f.dave.ID)
		after := f.post(t, f.ch.ID, f.frank, "after the bound")
		if f.wakes(t, f.ch.ID, before, f.frank) {
			t.Error("a message at the history bound woke the member")
		}
		if !f.wakes(t, f.ch.ID, after, f.frank) {
			t.Error("a message after the history bound did not wake the member")
		}
	})
	t.Run("a guest", func(t *testing.T) {
		g := newGuestFixture(t)
		ctx := context.Background()
		old, err := g.s.InsertMessageV1(ctx, g.general.ID, g.alice.ID, "before the guest came", nil)
		if err != nil {
			t.Fatal(err)
		}
		guest := g.guest("visitor")
		putPrefs(t, g.s, guest.Credential.ID, schema.NotificationPreferencesV1{Policy: "all", Muted: []int64{}})
		reg := registerPush(t, g.s, guest.Credential.ID, webPushTarget("-guest"))
		fresh, err := g.s.InsertMessageV1(ctx, g.general.ID, g.alice.ID, "after the guest came", nil)
		if err != nil {
			t.Fatal(err)
		}
		woken := func(id int64) bool {
			got, err := g.s.PushTargets(ctx, g.general.ID, id, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range got {
				if w.RegistrationID == reg.ID {
					return true
				}
			}
			return false
		}
		if woken(old.ID) {
			t.Error("a guest was woken for a message from before their visit")
		}
		if !woken(fresh.ID) {
			t.Error("a guest was not woken for a message after their visit began")
		}
		// Past the visit's own deadline the credential is not live.
		g.exec(t, "UPDATE guest_visits SET state = 'ended', ended_at = 1, end_reason = 'end_visit' WHERE principal_id = ?", guest.Principal.ID)
		if woken(fresh.ID) {
			t.Error("a guest whose visit ended was woken")
		}
	})
}

func (g *guestFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := g.s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// TestPushRegistrationLive: the check made before a retry, by id.
func TestPushRegistrationLive(t *testing.T) {
	tests := []struct {
		name   string
		break_ func(t *testing.T, f *pushTargetFixture)
		live   bool
	}{
		{"untouched", func(*testing.T, *pushTargetFixture) {}, true},
		{"the registration ended", func(t *testing.T, f *pushTargetFixture) {
			if err := f.s.DeletePushRegistration(context.Background(), f.cred, f.reg.ID, time.Now()); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"the credential was revoked", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "UPDATE credentials SET revoked_at = 1 WHERE id = ?", f.cred)
		}, false},
		{"the generation moved on", func(t *testing.T, f *pushTargetFixture) {
			f.exec(t, "UPDATE principals SET auth_generation = auth_generation + 1 WHERE id = ?", f.dave.ID)
		}, false},
		{"the principal was removed from the channel (not this check's business)", func(t *testing.T, f *pushTargetFixture) {
			if _, err := f.s.RemoveChannelMember(context.Background(), "system", f.ch.ID, f.dave.ID); err != nil {
				t.Fatal(err)
			}
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPushTargetFixture(t)
			tt.break_(t, f)
			got, err := f.s.PushRegistrationLive(context.Background(), f.reg.ID, time.Now())
			if err != nil || got != tt.live {
				t.Errorf("live = %v, %v; want %v", got, err, tt.live)
			}
		})
	}
	f := newPushTargetFixture(t)
	if got, _ := f.s.PushRegistrationLive(context.Background(), "no-such-id", time.Now()); got {
		t.Error("an id never issued is live")
	}
}

// TestMarkAndEndPushRegistration: the wake time is written for an active
// registration only, and a provider rejection ends it once with one audit row.
func TestMarkAndEndPushRegistration(t *testing.T) {
	f := newPushTargetFixture(t)
	ctx := context.Background()
	at := time.UnixMilli(1_700_000_000_000)
	if err := f.s.MarkPushWake(ctx, f.reg.ID, at); err != nil {
		t.Fatal(err)
	}
	got, err := f.s.GetOwnPushRegistration(ctx, f.cred, f.reg.ID)
	if err != nil || got.LastWakeAt == nil || !got.LastWakeAt.Time().Equal(at) {
		t.Fatalf("last_wake_at = %+v, %v", got.LastWakeAt, err)
	}
	for i := range 3 {
		ended, err := f.s.EndPushRegistrationRejected(ctx, f.reg.ID, at)
		if err != nil {
			t.Fatal(err)
		}
		if ended != (i == 0) {
			t.Errorf("call %d: ended = %v", i+1, ended)
		}
	}
	requireEnded(t, f.s, f.reg.ID, schema.PushRegistrationEndProviderRejected)
	rows := 0
	for _, d := range pushAuditDetails(t, f.s, schema.AuditActionPushRegistrationEnded) {
		if d == pushDetail(f.reg.ID, "webpush", "provider_rejected") {
			rows++
		}
	}
	if rows != 1 {
		t.Errorf("%d audit rows for the rejection, want exactly 1", rows)
	}
	if err := f.s.MarkPushWake(ctx, f.reg.ID, at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if r := pushRowOf(t, f.s, f.reg.ID); r.state != "expired" {
		t.Errorf("a wake mark revived the registration: %+v", r)
	}
	// And no more targets.
	f.policy(t, schema.NotificationPolicyAll, false)
	if f.wakes(t, f.ch.ID, f.post(t, f.ch.ID, f.frank, "again"), f.frank) {
		t.Error("an ended registration is still a target")
	}
}
