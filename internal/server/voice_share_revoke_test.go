package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of revocation against a claim in flight (issue #1061; ADR-014
// "revocation takes effect before the response", ADR-015 decision 6,
// docs/design/webcam-video.md §4.2 steps 9 to 12). The room's claims lock is
// held by the test, or by a claim stopped inside LiveKit's answer, so the
// interleavings are forced and nothing sleeps.

// revokeShareScreen turns share_screen off for members and operators, as the
// owner, through the store: the write the permissions PUT makes. The route's
// hook is endSharesOf, which the caller runs next, before "answering".
func (f *presenceFixture) revokeShareScreen(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision, Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleOperator, Granted: false},
	}}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, req, time.Now()); err != nil {
		t.Fatalf("turn share_screen off: %v", err)
	}
}

// screenGrants counts the UpdateParticipant requests that granted who the
// screen-share source.
func (f *presenceFixture) screenGrants(who string) int {
	n := 0
	for _, u := range f.lk.updatesFor(f.identity(who)) {
		if slices.Contains(u.sources, "SCREEN_SHARE") {
			n++
		}
	}
	return n
}

// revocations are the paths that take ann's place or right to share, each
// run as the code that runs it does: the store write that commits, then the
// hook that answers. No route flips voice_allowed after a join, so the voice
// switch has no row of its own: it is read by the same Permitted call as the
// permissions PUT (brakeWithholds) and would end shares through the same
// endSharesOf, which the first row runs.
var revocations = []struct {
	name string
	do   func(t *testing.T, f *presenceFixture)
}{
	{"permissions PUT", func(t *testing.T, f *presenceFixture) {
		f.revokeShareScreen(t)
		f.srv.voice.endSharesOf(context.Background(), f.ids["ann"], 0, voiceShareStopAccessLost)
	}},
	{"membership loss", func(t *testing.T, f *presenceFixture) {
		if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann"]); err != nil {
			t.Error(err)
		}
		f.srv.voiceMemberRemoved(context.Background(), f.ops.ID, f.ids["ann"])
	}},
	{"disabled", func(t *testing.T, f *presenceFixture) {
		if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.ids["ann"]); err != nil {
			t.Error(err)
		}
		f.srv.principalDisabled(context.Background(), f.ids["ann"])
	}},
}

// TestVoiceShareClaimRevokedBeforeGrant is the interleaving. The claim has
// been admitted (the caller's authority was read up front) and waits for the
// room's claims; a revocation commits and answers; then the claim proceeds.
// It must not grant the screen, whichever revocation it was, and must leave
// no share behind. Where the permission was revoked it is refused as the
// up-front check refuses a person without voice: 403 voice_not_allowed.
func TestVoiceShareClaimRevokedBeforeGrant(t *testing.T) {
	for _, rev := range revocations {
		t.Run(rev.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			r := f.srv.voice.roomNamed(room)
			r.claims.Lock()
			done := make(chan wireResult, 1)
			go func() { done <- f.claimShare(t, "ann", "ops") }()
			rev.do(t, f)
			r.claims.Unlock()
			res := <-done
			if res.status == http.StatusOK {
				t.Fatalf("a claim was granted after the revocation answered: %s", res.body)
			}
			if rev.name == "permissions PUT" && (res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed) {
				t.Errorf("claim after the revocation: %d %s, want 403 voice_not_allowed", res.status, res.body)
			}
			if n := f.screenGrants("ann"); n != 0 {
				t.Errorf("UpdateParticipant granted the screen %d times after the revocation answered", n)
			}
			if f.granted(t, room, "ann") {
				t.Error("the screen is granted after the revocation answered")
			}
			if holder := f.shareState(room); holder != 0 {
				t.Errorf("the poller holds a share for %d", holder)
			}
			if rows := f.shareRows(t); len(rows) != 0 {
				t.Errorf("share rows %q, want none", rows)
			}
		})
	}
}

// TestVoiceShareRevocationWaitsForClaimInFlight: the claim has passed its
// re-read and is inside LiveKit's answer when the revocation commits. The
// revocation's stop path takes the room's claims, so it waits for the claim
// and then ends the share the claim made: when both are done nothing is
// granted and nobody holds the share, whichever revocation it was.
func TestVoiceShareRevocationWaitsForClaimInFlight(t *testing.T) {
	for _, rev := range revocations {
		t.Run(rev.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			granting, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.lk.setOnUpdate(func(_, _ string, share bool) {
				if share {
					once.Do(func() {
						close(granting)
						<-release
					})
				}
			})
			claimed := make(chan wireResult, 1)
			go func() { claimed <- f.claimShare(t, "ann", "ops") }()
			<-granting
			revoked := make(chan struct{})
			go func() {
				defer close(revoked)
				rev.do(t, f)
			}()
			close(release)
			<-claimed
			<-revoked
			if f.granted(t, room, "ann") {
				t.Error("the screen is still granted after the revocation answered")
			}
			if holder := f.shareState(room); holder != 0 {
				t.Errorf("the poller still holds a share for %d", holder)
			}
			if rev.name == "permissions PUT" {
				want := []string{"ann started audio=true", "ann stopped principal=ann reason=access_lost"}
				if got := f.shareRows(t); !slices.Equal(got, want) {
					t.Errorf("share rows %q, want %q", got, want)
				}
			}
		})
	}
}

// TestVoiceShareReclaimAfterRevocation: a holder whose right was taken away
// and who claims again is refused, and loses the share they held.
func TestVoiceShareReclaimAfterRevocation(t *testing.T) {
	f, room := newShareFixture(t)
	f.mustClaim(t, "ann")
	f.revokeShareScreen(t)
	res := f.claimShare(t, "ann", "ops")
	if res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed {
		t.Fatalf("holder reclaims after revocation: %d %s, want 403 voice_not_allowed", res.status, res.body)
	}
	if f.granted(t, room, "ann") || f.shareState(room) != 0 {
		t.Errorf("the revoked holder still holds the share (granted=%v holder=%d)", f.granted(t, room, "ann"), f.shareState(room))
	}
	want := []string{"ann started audio=true", "ann stopped principal=ann reason=access_lost"}
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}
	if strings.Contains(res.body, "share_screen") {
		t.Errorf("the refusal names the permission: %s", res.body)
	}
}
