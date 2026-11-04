package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of a share_screen revocation against a room a rotation is retiring
// (issue #1265, S3; the share's half of #1258's camera fix): the rotation has
// ended ann's share, so there is no share left to end, but its withdrawal of
// her grant is still to be made, and the revocation must not answer before it
// is. DeleteRoom fails throughout, so the old room, and the grant in it, stay.

// shareRevokeHooks are the revocations of share_screen whose hooks end shares
// by principal: the permissions PUT (endWithheldShares, over
// sharingPrincipals) and a role change through its route (endShareIfWithheld).
// setup, when set, runs before anything is claimed; revoke is the store write
// and the hook, or the route, and returns once it has answered.
var shareRevokeHooks = []struct {
	name   string
	setup  func(t *testing.T, f *presenceFixture)
	revoke func(t *testing.T, f *presenceFixture)
}{
	{"permissions PUT", nil, func(t *testing.T, f *presenceFixture) {
		f.revokeShareScreen(t)
		f.srv.endWithheldShares(context.Background())
	}},
	// ann is an operator and only the operator column keeps share_screen, so
	// demoting her withholds it and the route's hook ends her share.
	{"role change", func(t *testing.T, f *presenceFixture) {
		f.srv.credRecheckInterval = 1 << 62
		if _, err := f.srv.store.SetRole(context.Background(), fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, f.ids["ann"], "operator", nil); err != nil {
			t.Fatal(err)
		}
	}, func(t *testing.T, f *presenceFixture) {
		f.setShareScreen(t, false, schema.PermissionRoleMember)
		rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/principals/%d/role", f.ids["ann"]), f.rootTok, `{"role":"member"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("role change = %d %s, want 200", rec.Code, rec.Body)
		}
	}},
}

// setShareScreen sets share_screen's cell to on in the given columns, as the
// owner, through the store: the write the permissions PUT makes, without its
// hook.
func (f *presenceFixture) setShareScreen(t *testing.T, on bool, roles ...schema.PermissionRole) {
	t.Helper()
	ctx := context.Background()
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Error(err)
		return
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision}
	for _, r := range roles {
		req.Changes = append(req.Changes, schema.PermissionChangeV1{Permission: schema.PermShareScreen, Role: r, Granted: on})
	}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, req, time.Now()); err != nil {
		t.Errorf("set share_screen %v in %v: %v", on, roles, err)
	}
}

// shareHeldAnswer reports that answered stays open for a moment: what a test
// calls while it holds the withdrawal the revocation must wait for.
func shareHeldAnswer(t *testing.T, answered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-answered:
		t.Fatalf("%s: the share_screen revocation answered while ann's screen grant was still in flight", what)
	case <-time.After(100 * time.Millisecond):
	}
}

// newRetiringShareFixture is the share fixture with hook's setup run, DeleteRoom
// failing for the room, and ann holding the share.
func newRetiringShareFixture(t *testing.T, setup func(t *testing.T, f *presenceFixture)) (*presenceFixture, string) {
	t.Helper()
	f, room := newShareFixture(t)
	if setup != nil {
		setup(t, f)
	}
	f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
	f.mustClaim(t, "ann")
	if !f.granted(t, room, "ann") {
		t.Fatal("ann's claim granted nothing")
	}
	return f, room
}

// TestVoiceShareRevocationWaitsForARetiringRoomsWithdrawal: a rotation (ann2's
// credential revoked) forgets the room with no claim running, ending ann's
// share, and its withdrawal of her grant (retireRoom) is held inside
// LiveKit. The room is in neither rooms nor, under ann's id, retiring. A
// share_screen revocation meanwhile must still find ann and wait for that
// withdrawal before it answers.
func TestVoiceShareRevocationWaitsForARetiringRoomsWithdrawal(t *testing.T) {
	for _, hook := range shareRevokeHooks {
		t.Run(hook.name, func(t *testing.T) {
			f, room := newRetiringShareFixture(t, hook.setup)
			ann := f.identity("ann")
			withdrawing, release := make(chan struct{}), make(chan struct{})
			var once, releaseOnce sync.Once
			f.lk.setOnUpdate(func(_, identity string, share bool) {
				if !share && identity == ann {
					once.Do(func() {
						close(withdrawing)
						<-release
					})
				}
			})
			// However the test ends, the held withdrawal is let go before the
			// fake LiveKit is closed (cleanups run last registered first).
			letGo := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(letGo)
			id := f.credID(t, "ann2")
			rotated := make(chan struct{})
			go func() {
				defer close(rotated)
				if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
					t.Error(err)
				}
				f.srv.credentialRevoked(context.Background(), id)
			}()
			waitOr(t, withdrawing, "the rotation to withdraw ann's screen")
			f.srv.voice.mu.Lock()
			inRooms := f.srv.voice.rooms[room] != nil
			f.srv.voice.mu.Unlock()
			if inRooms {
				t.Fatal("the rotated room is still in rooms: the interleaving was not reached")
			}
			answered := make(chan struct{})
			go func() {
				defer close(answered)
				hook.revoke(t, f)
			}()
			shareHeldAnswer(t, answered, "a retiring room's withdrawal")
			letGo()
			waitOr(t, answered, "the revocation to answer")
			if f.granted(t, room, "ann") {
				t.Error("ann's screen is still granted in the old room when the revocation answered")
			}
			waitOr(t, rotated, "the rotation's hook")
		})
	}
}

// TestVoiceShareRevocationFindsARoomForgottenAfterItsLook: the revocation's
// stop path looks while the room is live, with ann's share in it, and is
// held at the room's claims. A rotation then forgets the room (ann's share
// ends, rotated) and is held before retireRoom takes the claims to withdraw
// her grant. The stop path gets the claims first and finds no share to end;
// it must still wait for the withdrawal before the revocation answers.
func TestVoiceShareRevocationFindsARoomForgottenAfterItsLook(t *testing.T) {
	for _, hook := range shareRevokeHooks {
		t.Run(hook.name, func(t *testing.T) {
			f, room := newRetiringShareFixture(t, hook.setup)
			r := f.srv.voice.roomNamed(room)
			r.claims.Lock()
			unlocked := false
			t.Cleanup(func() {
				if !unlocked {
					r.claims.Unlock()
				}
			})
			looked := make(chan struct{})
			var lookOnce sync.Once
			f.srv.voice.endSharesSeam = func(n int) {
				if n > 0 {
					lookOnce.Do(func() { close(looked) })
				}
			}
			atRetire, goRetire := make(chan struct{}), make(chan struct{})
			var retireOnce, goOnce sync.Once
			letRetire := func() { goOnce.Do(func() { close(goRetire) }) }
			t.Cleanup(letRetire)
			f.srv.voice.claimSeam = func(at string) {
				if at == "retire" {
					retireOnce.Do(func() { close(atRetire) })
					<-goRetire
				}
			}
			id := f.credID(t, "ann2")
			answered := make(chan struct{})
			go func() {
				defer close(answered)
				hook.revoke(t, f)
			}()
			waitOr(t, looked, "the stop path to find ann's room")
			rotated := make(chan struct{})
			go func() {
				defer close(rotated)
				if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
					t.Error(err)
				}
				f.srv.credentialRevoked(context.Background(), id)
			}()
			waitOr(t, atRetire, "the rotation to forget the room")
			unlocked = true
			r.claims.Unlock() // the stop path takes the claims before retireRoom
			shareHeldAnswer(t, answered, "forgotten after the stop path's look")
			letRetire()
			waitOr(t, answered, "the revocation to answer")
			if f.granted(t, room, "ann") {
				t.Error("ann's screen is still granted in the old room when the revocation answered")
			}
			waitOr(t, rotated, "the rotation's hook")
		})
	}
}

// TestVoiceShareRevocationWaitsWhenAClaimEndsBeforeRetireRoom: a rotation
// forgets the room while a claim of ann2's is running in it (held with the
// room's claims, the claim recorded), so the room is retiring under ann2's
// id, and ends ann's share there. ann2's claim then returns, refused, before
// retireRoom has looked at the room again. A share_screen revocation of ann
// made in between must still wait for retireRoom's withdrawal of her grant.
func TestVoiceShareRevocationWaitsWhenAClaimEndsBeforeRetireRoom(t *testing.T) {
	for _, hook := range shareRevokeHooks {
		t.Run(hook.name, func(t *testing.T) {
			f, room := newRetiringShareFixture(t, hook.setup)
			old, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
			if err != nil {
				t.Fatal(err)
			}
			inside, proceed := make(chan struct{}), make(chan struct{})
			atRetire, goRetire := make(chan struct{}), make(chan struct{})
			var insideOnce, proceedOnce, retireOnce, goOnce sync.Once
			letProceed := func() { proceedOnce.Do(func() { close(proceed) }) }
			letRetire := func() { goOnce.Do(func() { close(goRetire) }) }
			t.Cleanup(letRetire)
			t.Cleanup(letProceed)
			f.srv.voice.claimSeam = func(at string) {
				switch at {
				case "claimed":
					insideOnce.Do(func() { close(inside) })
					<-proceed
				case "retire":
					retireOnce.Do(func() { close(atRetire) })
					<-goRetire
				}
			}
			claimed := make(chan wireResult, 1)
			go func() { claimed <- f.claimShare(t, "ann2", "ops") }()
			waitOr(t, inside, "ann2's claim to be recorded in the room")
			f.rotateInStoreOnly(t)
			rotated := make(chan struct{})
			go func() {
				defer close(rotated)
				f.srv.voice.retireRoom(context.Background(), old)
			}()
			waitOr(t, atRetire, "the rotation to forget the room")
			letProceed()
			select {
			case res := <-claimed:
				if res.status == http.StatusOK {
					t.Errorf("ann2's claim in a retired room was granted: %s", res.body)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("ann2's claim never returned")
			}
			answered := make(chan struct{})
			go func() {
				defer close(answered)
				hook.revoke(t, f)
			}()
			shareHeldAnswer(t, answered, "a claim ended between the forget and retireRoom")
			letRetire()
			waitOr(t, answered, "the revocation to answer")
			if f.granted(t, room, "ann") {
				t.Error("ann's screen is still granted in the old room when the revocation answered")
			}
			waitOr(t, rotated, "the rotation")
			f.srv.voice.mu.Lock()
			left := len(f.srv.voice.retiring)
			f.srv.voice.mu.Unlock()
			if left != 0 {
				t.Errorf("%d retired rooms are still held", left)
			}
		})
	}
}
