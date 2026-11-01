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
// endSharesOf, which the first row runs. rotates marks the paths that rotate
// the room, which each test runs with LiveKit's DeleteRoom answering and
// failing: a failed deletion leaves the old room, and any grant in it, until
// the sweep.
var revocations = []struct {
	name    string
	rotates bool
	do      func(t *testing.T, f *presenceFixture)
}{
	{"permissions PUT", false, func(t *testing.T, f *presenceFixture) {
		f.revokeShareScreen(t)
		f.srv.voice.endSharesOf(context.Background(), f.ids["ann"], 0, voiceShareStopAccessLost)
	}},
	{"membership loss", true, func(t *testing.T, f *presenceFixture) {
		if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann"]); err != nil {
			t.Error(err)
		}
		f.srv.voiceMemberRemoved(context.Background(), f.ops.ID, f.ids["ann"])
	}},
	{"disabled", true, func(t *testing.T, f *presenceFixture) {
		if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.ids["ann"]); err != nil {
			t.Error(err)
		}
		f.srv.principalDisabled(context.Background(), f.ids["ann"])
	}},
}

// revocationCase is one row of revocations with DeleteRoom answering or not.
type revocationCase struct {
	name       string
	deleteFail bool
	do         func(t *testing.T, f *presenceFixture)
}

func revocationCases() []revocationCase {
	var out []revocationCase
	for _, rev := range revocations {
		out = append(out, revocationCase{rev.name, false, rev.do})
		if rev.rotates {
			out = append(out, revocationCase{rev.name + "/delete fails", true, rev.do})
		}
	}
	return out
}

func (c revocationCase) setUp(f *presenceFixture, room string) {
	if c.deleteFail {
		f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
	}
}

// TestVoiceShareClaimRevokedBeforeGrant is the interleaving. The claim has
// been admitted (the caller's authority was read up front) and is stopped,
// either before it looks up the room's state or at the room's claims, while a
// revocation commits and answers; then the claim proceeds. It must not ask
// LiveKit for the screen at all, whichever revocation it was, and must leave
// no share behind. Where the permission was revoked it is refused as the
// up-front check refuses a person without voice: 403 voice_not_allowed.
//
// "enter" is the case a rotation makes worst: the room's state was forgotten,
// the claim makes a fresh one for the old name, and only a re-read of the
// room and the person's place (shareEntitled) stops it.
func TestVoiceShareClaimRevokedBeforeGrant(t *testing.T) {
	for _, stage := range []string{"enter", "lock"} {
		for _, rev := range revocationCases() {
			t.Run(stage+"/"+rev.name, func(t *testing.T) {
				f, room := newShareFixture(t)
				rev.setUp(f, room)
				admitted, proceed := make(chan struct{}), make(chan struct{})
				f.srv.voice.claimSeam = func(at string) {
					if at == stage {
						close(admitted)
						<-proceed
					}
				}
				done := make(chan wireResult, 1)
				go func() { done <- f.claimShare(t, "ann", "ops") }()
				<-admitted
				rev.do(t, f) // committed and answered
				close(proceed)
				res := <-done
				if res.status == http.StatusOK {
					t.Fatalf("a claim was granted after the revocation answered: %s", res.body)
				}
				if rev.name == "permissions PUT" && (res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed) {
					t.Errorf("claim after the revocation: %d %s, want 403 voice_not_allowed", res.status, res.body)
				}
				if n := f.screenGrants("ann"); n != 0 {
					t.Errorf("UpdateParticipant was asked for the screen %d times after the revocation answered", n)
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
}

// TestVoiceShareRevocationWaitsForClaimInFlight: the claim has passed its
// re-read and is inside LiveKit's answer to the grant when the revocation
// commits. The revocation's stop path must find the claim (in the room, or
// in a room just retired by the revocation's own rotation), and must not
// answer while the grant is still in flight. When it answers, nothing is
// granted and nobody holds the share, even where DeleteRoom failed and the
// old room is still there with ann in it.
func TestVoiceShareRevocationWaitsForClaimInFlight(t *testing.T) {
	for _, rev := range revocationCases() {
		t.Run(rev.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			rev.setUp(f, room)
			ann := f.identity("ann")
			granting, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.lk.setOnUpdate(func(_, identity string, share bool) {
				if share && identity == ann {
					once.Do(func() {
						close(granting)
						<-release
					})
				}
			})
			waiting := make(chan int, 1)
			f.srv.voice.endSharesSeam = func(rooms int) {
				select {
				case waiting <- rooms: // the first call only
				default:
				}
			}
			claimed := make(chan wireResult, 1)
			go func() { claimed <- f.claimShare(t, "ann", "ops") }()
			<-granting
			revoked := make(chan struct{})
			go func() {
				defer close(revoked)
				rev.do(t, f)
			}()
			// The revocation has committed and its hook has reached the stop
			// path; it must have found the claim, and cannot answer while
			// the grant is held inside LiveKit.
			if n := <-waiting; n == 0 {
				t.Error("the revocation's stop path found no share to wait for")
			}
			select {
			case <-revoked:
				t.Error("the revocation answered while the grant was still in flight")
			default:
			}
			close(release)
			<-revoked
			// Read at the moment the revocation has answered.
			if f.granted(t, room, "ann") {
				t.Error("the screen is still granted when the revocation answered")
			}
			if holder := f.shareState(room); holder != 0 {
				t.Errorf("the poller still holds a share for %d", holder)
			}
			<-claimed
			if rev.name == "permissions PUT" {
				want := []string{"ann started audio=true", "ann stopped principal=ann reason=access_lost"}
				if got := f.shareRows(t); !slices.Equal(got, want) {
					t.Errorf("share rows %q, want %q", got, want)
				}
			}
			f.srv.voice.mu.Lock()
			left := len(f.srv.voice.retiring)
			f.srv.voice.mu.Unlock()
			if left != 0 {
				t.Errorf("%d retired rooms are still held for their claims", left)
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
