package server

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
)

// TestVoiceShareHeldShareWithdrawnWhenRotationDeleteFails (issue #1087): ann
// holds the share, no claim is running, and a rotation retires the room while
// LiveKit's DeleteRoom fails: the old room, and ann in it, stay. Once the
// hook (or the sweep's rotation) has answered, the screen is not granted, and
// the share ended with reason=rotated.
func TestVoiceShareHeldShareWithdrawnWhenRotationDeleteFails(t *testing.T) {
	rows := heldShareRotationRows()
	// No hook at all: the sweep's rotation does the withdrawal.
	rows = append(rows, revocationCase{"sweep rotates/delete fails", true, func(t *testing.T, f *presenceFixture) {
		id := f.credID(t, "ann")
		if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
			t.Error(err)
		}
		f.sweepRotates(t)
	}})
	for _, rev := range rows {
		t.Run(rev.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			rev.setUp(f, room)
			f.mustClaim(t, "ann")
			if !f.granted(t, room, "ann") {
				t.Fatal("ann's claim granted nothing")
			}
			rev.do(t, f) // committed and answered
			if f.granted(t, room, "ann") {
				t.Error("the screen is still granted when the revocation answered")
			}
			if !f.lk.has(room) {
				t.Error("the old room was deleted: the test does not exercise a failed DeleteRoom")
			}
			if holder := f.shareState(room); holder != 0 {
				t.Errorf("the poller still holds a share for %d", holder)
			}
			// Membership loss and disabling also end shares by principal,
			// but after the rotation has ended it: one stopped row.
			want := []string{"ann started audio=true", "ann stopped principal=ann reason=rotated"}
			if got := f.shareRows(t); !slices.Equal(got, want) {
				t.Errorf("share rows %q, want %q", got, want)
			}
		})
	}
}

// heldShareRotationRows are the rotating revocations of the matrix, with
// DeleteRoom failing, bar those where another rotator has committed and not
// yet forgotten the room.
func heldShareRotationRows() []revocationCase {
	var out []revocationCase
	for _, rev := range revocationCases() {
		if rev.deleteFail && !strings.Contains(rev.name, "not yet forgotten") {
			out = append(out, rev)
		}
	}
	return out
}

// revokeWhileClaimHeld runs rev with a claim held by hold(), which returns
// once the claim is stopped, and a function that lets it go. rev runs in a
// goroutine; the test waits until its hook has retired the room and reached
// the wait for running claims, so the rotation has committed while the claim
// is running. It returns once rev has answered.
func revokeWhileClaimHeld(t *testing.T, f *presenceFixture, rev revocationCase, release func()) {
	t.Helper()
	waiting := make(chan struct{})
	var once sync.Once
	f.srv.voice.endSharesSeam = func(int) { once.Do(func() { close(waiting) }) }
	f.credID(t, "ann") // the rows' own lookups, in the goroutine below, then cannot fail
	revoked := make(chan struct{})
	go func() {
		defer close(revoked)
		rev.do(t, f)
	}()
	<-waiting
	release()
	<-revoked
}

// TestVoiceShareHeldShareWithdrawnWithOtherClaimRunning (issue #1087): ann
// holds the share and a claim of ann2's is running in the room (it is refused
// room_gone once the room is retired, and as busy before) when a rotation
// retires the room with DeleteRoom failing.
// That claim never touches ann's grant, so the debt left for it
// (owedWithdraw) is paid in its defer, before the hook answers.
func TestVoiceShareHeldShareWithdrawnWithOtherClaimRunning(t *testing.T) {
	for _, rev := range heldShareRotationRows() {
		t.Run(rev.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			rev.setUp(f, room)
			f.mustClaim(t, "ann")
			inside, proceed := make(chan struct{}), make(chan struct{})
			f.srv.voice.claimSeam = func(at string) {
				if at == "claimed" {
					close(inside)
					<-proceed
				}
			}
			claimed := make(chan wireResult, 1)
			go func() { claimed <- f.claimShare(t, "ann2", "ops") }()
			<-inside
			var once sync.Once
			revokeWhileClaimHeld(t, f, rev, func() { once.Do(func() { close(proceed) }) })
			if f.granted(t, room, "ann") {
				t.Error("ann's screen is still granted when the revocation answered")
			}
			if res := <-claimed; res.status == http.StatusOK {
				t.Errorf("ann2's claim was granted: %s", res.body)
			}
			if f.granted(t, room, "ann2") {
				t.Error("ann2's screen is granted")
			}
		})
	}
}

// TestVoiceShareHeldShareWithdrawnWithOwnClaimAgain (issue #1087): ann holds
// the share and claims again; the claim has read the room as live after its
// grant, and the rotation commits only then, DeleteRoom failing. The claim
// answers 200 without seeing the retirement; the debt left for it is paid in
// its defer, before the hook answers.
func TestVoiceShareHeldShareWithdrawnWithOwnClaimAgain(t *testing.T) {
	for _, rev := range heldShareRotationRows() {
		t.Run(rev.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			rev.setUp(f, room)
			f.mustClaim(t, "ann")
			read, proceed := make(chan struct{}), make(chan struct{})
			f.srv.voice.liveReadSeam = func(context.Context, int64) (bool, error) {
				close(read)
				<-proceed
				return true, nil
			}
			claimed := make(chan wireResult, 1)
			go func() { claimed <- f.claimShare(t, "ann", "ops") }()
			<-read
			var once sync.Once
			revokeWhileClaimHeld(t, f, rev, func() { once.Do(func() { close(proceed) }) })
			if f.granted(t, room, "ann") {
				t.Error("ann's screen is still granted when the revocation answered")
			}
			<-claimed
		})
	}
}

// TestVoiceShareProxyForgetKeepsRotationsWithdrawal (issue #1087): a rotation
// is committed in the store but its room is not yet forgotten when another
// path that found the room retired forgets it first (voiceDropIfRetired; the
// transmit report does the same through retireRoom). That forget ends the
// share, so it must withdraw the grant too: the rotator's own forget then
// finds nothing to do. DeleteRoom fails, and once the hook has answered no
// grant is left.
func TestVoiceShareProxyForgetKeepsRotationsWithdrawal(t *testing.T) {
	f, room := newShareFixture(t)
	f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
	f.mustClaim(t, "ann")
	old, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := f.credID(t, "ann")
	if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
		t.Fatal(err)
	}
	f.rotateInStoreOnly(t)
	f.srv.voiceDropIfRetired(context.Background(), old) // wins the room's order
	f.srv.credentialRevoked(context.Background(), id)   // the hook answers
	if f.granted(t, room, "ann") {
		t.Error("the screen is still granted when the hook answered")
	}
	if holder := f.shareState(room); holder != 0 {
		t.Errorf("the poller still holds a share for %d", holder)
	}
}

// TestVoiceShareDebtRegistersRetiringRoom (issue #1087): a claim of ann2's
// looked the room up before the rotation forgot it and takes the room's
// claims only between forgetRoom's return and retireRoom's look at claimer
// (the "retire" seam). forgetRoom has already put the room in retiring (value
// 0) with withdrawDone open, so the hook's wait finds two steps: that
// withdrawal and, once retireRoom hands it the debt and records the claim in
// retiring, the claim's own claimDone. When it answers ann's grant is gone.
// (The debt branch's retiring entry matters to endSharesOf's by-principal
// match; this test pins the hook's wait, not that.)
func TestVoiceShareDebtRegistersRetiringRoom(t *testing.T) {
	f, room := newShareFixture(t)
	f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
	f.mustClaim(t, "ann")
	id := f.credID(t, "ann")
	atLock, past := make(chan struct{}), make(chan struct{})
	inside, release := make(chan struct{}), make(chan struct{})
	f.srv.voice.claimSeam = func(at string) {
		switch at {
		case "lock": // looked the room up; waits for the rotation to forget it
			close(atLock)
			<-past
		case "claimed": // holds the claims, claimer set
			close(inside)
			<-release
		case "retire": // forgetRoom has returned; let the claim in, then look
			close(past)
			<-inside
		}
	}
	waiting := make(chan int, 1)
	f.srv.voice.endSharesSeam = func(n int) {
		select {
		case waiting <- n:
		default:
		}
	}
	claimed := make(chan wireResult, 1)
	go func() { claimed <- f.claimShare(t, "ann2", "ops") }()
	<-atLock
	if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
		t.Fatal(err)
	}
	revoked := make(chan struct{})
	go func() {
		defer close(revoked)
		f.srv.credentialRevoked(context.Background(), id)
	}()
	if n := <-waiting; n != 2 {
		t.Errorf("the hook's wait found %d pending steps, want 2 (the claim and the withdrawal it owes, both on the retiring room)", n)
	}
	close(release)
	<-revoked
	if f.granted(t, room, "ann") {
		t.Error("ann's screen is still granted when the hook answered")
	}
	if res := <-claimed; res.status == http.StatusOK {
		t.Errorf("ann2's claim was granted: %s", res.body)
	}
}

// TestVoiceShareHookWaitsForProxyWithdrawal (issue #1087): the rotation is
// committed in the store, and another path that found the room retired
// forgets it first (voiceDropIfRetired, or a transmit report) and withdraws
// the grant. That withdrawal is held inside LiveKit while the rotator's hook
// runs: the hook has nothing to forget (its rotation finds the room already
// rotated), but it must not answer until the withdrawal is made
// (voiceRoomState.withdrawDone). DeleteRoom fails.
func TestVoiceShareHookWaitsForProxyWithdrawal(t *testing.T) {
	proxies := []struct {
		name string
		do   func(t *testing.T, f *presenceFixture, old store.VoiceRoom)
	}{
		{"voiceDropIfRetired", func(t *testing.T, f *presenceFixture, old store.VoiceRoom) {
			f.srv.voiceDropIfRetired(context.Background(), old)
		}},
		{"transmit report", func(t *testing.T, f *presenceFixture, old store.VoiceRoom) {
			if err := f.srv.voice.reportTransmit(context.Background(), old, f.ids["ann2"], true); err != nil {
				t.Error(err)
			}
		}},
	}
	for _, proxy := range proxies {
		t.Run(proxy.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
			f.mustClaim(t, "ann")
			old, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
			if err != nil {
				t.Fatal(err)
			}
			id := f.credID(t, "ann")
			ann := f.identity("ann")
			withdrawing, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.lk.setOnUpdate(func(_, identity string, share bool) {
				if !share && identity == ann {
					once.Do(func() {
						close(withdrawing)
						<-release
					})
				}
			})
			waiting := make(chan int, 1)
			f.srv.voice.endSharesSeam = func(n int) {
				select {
				case waiting <- n:
				default:
				}
			}
			if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
				t.Fatal(err)
			}
			f.rotateInStoreOnly(t)
			proxied := make(chan struct{})
			go func() {
				defer close(proxied)
				proxy.do(t, f, old) // wins the room's order, then withdraws
			}()
			select {
			case <-withdrawing:
			case <-time.After(10 * time.Second):
				t.Fatal("the proxy's withdrawal never reached LiveKit")
			}
			hooked := make(chan struct{})
			go func() {
				defer close(hooked)
				f.srv.credentialRevoked(context.Background(), id)
			}()
			if n := <-waiting; n != 1 {
				t.Errorf("the hook's wait found %d pending steps, want 1 (the proxy's withdrawal)", n)
			}
			close(release)
			<-hooked
			if f.granted(t, room, "ann") {
				t.Error("the screen is still granted when the hook answered")
			}
			<-proxied
			f.srv.voice.mu.Lock()
			left := len(f.srv.voice.retiring)
			f.srv.voice.mu.Unlock()
			if left != 0 {
				t.Errorf("%d retired rooms are still held", left)
			}
		})
	}
}

// TestVoiceShareRefusedClaimLeavesProxyWithdrawalToItsOwner (issue #1087): a
// claim that looked the room up before a forget, and takes the claims after
// it, is refused room_gone and returns; it was handed no debt, so its defer
// must not close withdrawDone or drop the retiring entry of the forget that
// is still to withdraw. The claim runs in full between forgetRoom and
// retireRoom's look at claimer (the "retire" seam), and the withdrawal is
// held inside LiveKit while the rotator's hook runs: the hook must still
// find the withdrawal to wait for, and answer only after it.
func TestVoiceShareRefusedClaimLeavesProxyWithdrawalToItsOwner(t *testing.T) {
	f, room := newShareFixture(t)
	f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
	f.mustClaim(t, "ann")
	old, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := f.credID(t, "ann")
	ann := f.identity("ann")
	atLock, past, claimDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	f.srv.voice.claimSeam = func(at string) {
		switch at {
		case "lock": // looked the room up; waits for the forget
			close(atLock)
			<-past
		case "retire": // forgotten, claimer still 0: the claim runs in full now
			close(past)
			<-claimDone
		}
	}
	withdrawing, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.lk.setOnUpdate(func(_, identity string, share bool) {
		if !share && identity == ann {
			once.Do(func() {
				close(withdrawing)
				<-release
			})
		}
	})
	waiting := make(chan int, 1)
	f.srv.voice.endSharesSeam = func(n int) {
		select {
		case waiting <- n:
		default:
		}
	}
	claimed := make(chan wireResult, 1)
	go func() {
		claimed <- f.claimShare(t, "ann2", "ops")
		close(claimDone)
	}()
	<-atLock
	if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
		t.Fatal(err)
	}
	f.rotateInStoreOnly(t)
	proxied := make(chan struct{})
	go func() {
		defer close(proxied)
		f.srv.voiceDropIfRetired(context.Background(), old) // forgets first, then withdraws
	}()
	select {
	case <-withdrawing:
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy's withdrawal never reached LiveKit")
	}
	if res := <-claimed; res.status == http.StatusOK {
		t.Errorf("ann2's claim was granted: %s", res.body)
	}
	hooked := make(chan struct{})
	go func() {
		defer close(hooked)
		f.srv.credentialRevoked(context.Background(), id)
	}()
	if n := <-waiting; n != 1 {
		t.Errorf("the hook's wait found %d pending steps, want 1 (the proxy's withdrawal)", n)
	}
	close(release)
	<-hooked
	if f.granted(t, room, "ann") {
		t.Error("the screen is still granted when the hook answered")
	}
	<-proxied
}
