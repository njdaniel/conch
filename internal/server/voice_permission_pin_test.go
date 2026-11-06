package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// Pinning tests for issue #1239 (webcam C4a, ADR-015 decision 5): every
// UpdateParticipant conchd makes for the share is one function's, and moving
// the four call sites onto it changed no request. Each case runs a scenario
// over the scripted LiveKit and compares every request it recorded, in order,
// with what the four fixed call sites wrote before the refactor (recorded at
// c73211a). A request is written "identity-role pub sub [sources]".

// updatesLog renders the UpdateParticipant requests the fake received, in
// order, with each identity shown as who's person ("ann"), companion
// ("ann.screen") or viewer ("ann.view").
func (f *presenceFixture) updatesLog(who ...string) []string {
	f.lk.mu.Lock()
	defer f.lk.mu.Unlock()
	names := map[string]string{}
	for _, w := range who {
		names[f.identity(w)] = w
		names[f.screenID(w)] = w + ".screen"
		names[f.viewID(w)] = w + ".view"
	}
	var out []string
	for _, u := range f.lk.updates {
		name, ok := names[u.identity]
		if !ok {
			name = "?" + u.identity
		}
		src := "null"
		if u.sources != nil {
			src = "[" + strings.Join(u.sources, ",") + "]"
		}
		out = append(out, fmt.Sprintf("%s pub=%v sub=%v %s", name, u.canPublish, u.canSubscribe, src))
	}
	return out
}

func TestVoiceSharePermissionRequestsArePinned(t *testing.T) {
	const (
		bundle = "[MICROPHONE,SCREEN_SHARE,SCREEN_SHARE_AUDIO]"
		mic    = "[MICROPHONE]"
	)
	// withCompanion adds ann's companion to the room, listed by a pass.
	withCompanion := func(subscribes bool, extra func(p *fakeParticipant)) func(t *testing.T) (*presenceFixture, string) {
		return func(t *testing.T) (*presenceFixture, string) {
			t.Helper()
			f, room := newShareFixture(t)
			c := fakeParticipant{identity: f.screenID("ann"), joinedMs: 3_000, noSubscribe: !subscribes}
			if extra != nil {
				extra(&c)
			}
			f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"), c)
			f.at(100)
			f.pass(t)
			return f, room
		}
	}
	plain := func(t *testing.T) (*presenceFixture, string) { t.Helper(); return newShareFixture(t) }
	tests := []struct {
		name  string
		setup func(t *testing.T) (*presenceFixture, string)
		run   func(t *testing.T, f *presenceFixture, room string)
		want  []string
	}{
		{"share grant, no companion", plain,
			func(t *testing.T, f *presenceFixture, _ string) { f.mustClaim(t, "ann") },
			[]string{"ann pub=true sub=true " + bundle}},
		{"share grant, subscribing companion", withCompanion(true, nil),
			func(t *testing.T, f *presenceFixture, _ string) { f.mustClaim(t, "ann") },
			[]string{"ann pub=true sub=true " + bundle, "ann.screen pub=true sub=true [SCREEN_SHARE]"}},
		{"share grant, publish-only companion", withCompanion(false, nil),
			func(t *testing.T, f *presenceFixture, _ string) { f.mustClaim(t, "ann") },
			[]string{"ann pub=true sub=true " + bundle, "ann.screen pub=true sub=false [SCREEN_SHARE]"}},
		{"share stop, no companion", plain,
			func(t *testing.T, f *presenceFixture, _ string) { f.mustClaim(t, "ann"); f.mustStop(t, "ann") },
			[]string{"ann pub=true sub=true " + bundle, "ann pub=true sub=true " + mic}},
		{"share stop, subscribing companion", withCompanion(true, nil),
			func(t *testing.T, f *presenceFixture, _ string) { f.mustClaim(t, "ann"); f.mustStop(t, "ann") },
			[]string{
				"ann pub=true sub=true " + bundle, "ann.screen pub=true sub=true [SCREEN_SHARE]",
				"ann pub=true sub=true " + mic, "ann.screen pub=false sub=true []",
			}},
		{"share stop, publish-only companion", withCompanion(false, nil),
			func(t *testing.T, f *presenceFixture, _ string) { f.mustClaim(t, "ann"); f.mustStop(t, "ann") },
			[]string{
				"ann pub=true sub=true " + bundle, "ann.screen pub=true sub=false [SCREEN_SHARE]",
				"ann pub=true sub=true " + mic, "ann.screen pub=false sub=false []",
			}},
		{"share stop, companion lookup fails: canSubscribe false", withCompanion(true, nil),
			func(t *testing.T, f *presenceFixture, _ string) {
				f.mustClaim(t, "ann")
				f.lk.setGetStatus(500)
				f.mustStop(t, "ann")
			},
			[]string{
				"ann pub=true sub=true " + bundle, "ann.screen pub=true sub=true [SCREEN_SHARE]",
				"ann pub=true sub=true " + mic, "ann.screen pub=false sub=false []",
			}},
		{"pass withdrawal from a non-holder", plain,
			func(t *testing.T, f *presenceFixture, room string) {
				f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.shareGranted, p.screen = true, true })
				f.at(500)
				f.pass(t)
			},
			[]string{"ann2 pub=true sub=true " + mic}},
		{"pass withdrawal from a listen-only non-holder", plain,
			func(t *testing.T, f *presenceFixture, room string) {
				f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.listenOnly, p.shareGranted, p.screen = true, true, true })
				f.at(500)
				f.pass(t)
			},
			[]string{"ann2 pub=false sub=true " + mic}},
		{"pass withdrawal from an unclaimed subscribing companion", withCompanion(true, func(p *fakeParticipant) { p.shareGranted, p.screen = true, true }),
			func(t *testing.T, f *presenceFixture, _ string) { f.at(500); f.pass(t) },
			[]string{"ann.screen pub=false sub=true []"}},
		{"pass withdrawal from an unclaimed publish-only companion", withCompanion(false, func(p *fakeParticipant) { p.shareGranted, p.screen = true, true }),
			func(t *testing.T, f *presenceFixture, _ string) { f.at(500); f.pass(t) },
			[]string{"ann.screen pub=false sub=false []"}},
		{"pass withdrawal from an unclaimed viewer", plain,
			func(t *testing.T, f *presenceFixture, room string) {
				f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
					fakeParticipant{identity: f.viewID("ann"), joinedMs: 3_000, listenOnly: true, screen: true})
				f.at(500)
				f.pass(t)
			},
			[]string{"ann.view pub=false sub=true []"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := tt.setup(t)
			tt.run(t, f, room)
			if got := f.updatesLog("ann", "ann2"); !slices.Equal(got, tt.want) {
				t.Errorf("UpdateParticipant requests =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// TestVoiceShareWithdrawalSparesAClaimMadeAfterThePlan: a pass plans a
// withdrawal from ann2 under r.order, and ann2 claims the share before the
// withdrawal is made under r.claims. The withdrawal is then not made: what the
// claim granted stands.
func TestVoiceShareWithdrawalSparesAClaimMadeAfterThePlan(t *testing.T) {
	f, room := newShareFixture(t)
	f.mustClaim(t, "ann2")
	before := f.updatesLog("ann", "ann2")
	p := f.srv.voice
	p.mu.Lock()
	r := p.rooms[room]
	p.mu.Unlock()
	planned := []shareWithdrawal{
		{principalID: f.ids["ann2"], role: voiceRolePerson, canPublish: true, sharing: true, unclaimed: true},
		{principalID: f.ids["ann2"], role: voiceRoleScreen, canSubscribe: true},
	}
	if again := p.withdrawShares(context.Background(), r, planned); again {
		t.Error("the pass wants another round for withdrawals it skipped")
	}
	if after := f.updatesLog("ann", "ann2"); !slices.Equal(after, before) {
		t.Errorf("the withdrawal wrote %q after the claim", after[len(before):])
	}
	if got := f.shareState(room); got != f.ids["ann2"] {
		t.Errorf("the share is held by %d, want ann2", got)
	}
}
