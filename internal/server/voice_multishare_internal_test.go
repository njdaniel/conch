package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of the room's share map itself (issue #1508): what only the poller's
// own state can show.

// TestVoiceShareCapIsReadAfterTheEntitlement: a caller whose right was taken
// away while the room was full is refused for the right, not told the room is
// full (design §4): voice_not_allowed, not voice_share_full.
func TestVoiceShareCapIsReadAfterTheEntitlement(t *testing.T) {
	f, room := newShareFixture(t)
	names := []string{"ann", "ann2"}
	for i := 3; i <= voiceShareCap+1; i++ {
		name := fmt.Sprintf("m%d", i)
		f.addSharer(t, room, name, int64(i)*1_000)
		names = append(names, name)
	}
	f.at(10_000)
	f.pass(t)
	for _, name := range names[:voiceShareCap] {
		f.mustClaim(t, name)
	}
	r, ok, err := f.srv.voiceChannelRoom(context.Background(), f.ops.ID)
	if err != nil || !ok {
		t.Fatalf("room: %v %v", ok, err)
	}
	seventh := f.ids[names[voiceShareCap]]
	if _, err := f.srv.voice.claimShare(context.Background(), r, seventh, nil); !errors.Is(err, errVoiceShareFull) {
		t.Fatalf("an entitled claim into a full room: %v, want errVoiceShareFull", err)
	}
	f.revokeShareScreen(t)
	grants := f.lk.count("UpdateParticipant")
	if _, err := f.srv.voice.claimShare(context.Background(), r, seventh, nil); !errors.Is(err, errVoiceShareNotAllowed) {
		t.Fatalf("a revoked claim into a full room: %v, want errVoiceShareNotAllowed", err)
	}
	if f.lk.count("UpdateParticipant") != grants {
		t.Error("a refused claim asked LiveKit for something")
	}
}

// TestVoiceMetricCountsShares: conch_voice_shares_active counts started
// shares across rooms and counts no pending claim.
func TestVoiceMetricCountsShares(t *testing.T) {
	f, room := newShareFixture(t)
	f.addSharer(t, room, "third", 3_000)
	f.at(1_000)
	f.pass(t)
	f.mustClaim(t, "ann")
	f.mustClaim(t, "ann2")
	r := f.srv.voice.roomNamed(room)
	f.srv.voice.mu.Lock()
	r.shares[f.ids["third"]] = &voiceShare{principalID: f.ids["third"], startedAt: f.clock.Now(), pending: true}
	f.srv.voice.mu.Unlock()
	if _, _, shares := f.srv.voice.metricCounts(); shares != 2 {
		t.Errorf("shares = %d, want 2 started and one pending uncounted", shares)
	}
	f.srv.voice.mu.Lock()
	r.shares[f.ids["third"]].pending = false
	f.srv.voice.mu.Unlock()
	if _, _, shares := f.srv.voice.metricCounts(); shares != 3 {
		t.Errorf("shares = %d, want 3", shares)
	}
}

// TestVoiceShareStopPersonWithoutShare: a moderator's stop naming a person
// who holds none changes nothing, even when another person's share is there.
func TestVoiceShareStopPersonWithoutShare(t *testing.T) {
	f, room := newShareFixture(t)
	f.mustClaim(t, "ann")
	r, _, _ := f.srv.voiceChannelRoom(context.Background(), f.ops.ID)
	rows := len(f.shareRows(t))
	if err := f.srv.voice.stopShare(context.Background(), r, f.ids["root"], f.ids["ann2"], nil); err != nil {
		t.Fatal(err)
	}
	if len(f.shareRows(t)) != rows || !f.granted(t, room, "ann") {
		t.Error("a stop of a person who shares nothing touched another's stream")
	}
	stale := schema.NewTimestamp(time.Unix(1, 0))
	if err := f.srv.voice.stopShare(context.Background(), r, f.ids["ann2"], f.ids["ann2"], &stale); !errors.Is(err, errVoiceShareStale) {
		t.Errorf("a named stale release by a non-holder: %v, want stale", err)
	}
}
