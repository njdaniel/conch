package store

// The four cases #2096's security review found no test pinned: each fails
// if its rule in dm_policy.go is removed (the review's mutation check).

import (
	"context"
	"errors"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// a disabled target who shares a channel with the caller.
func TestDMPolicyDisabledChannelMateIsUnavailable(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	f.shareChannel(t, "shared", f.alice, f.bob, f.carol)
	conv := f.openPair(t, f.alice, f.bob)
	f.send(t, conv, f.alice, "hi")
	f.disable(t, f.bob)
	f.disable(t, f.carol)
	if err := f.trySend(conv, f.alice, "to a disabled channel-mate"); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("send to a disabled channel-mate: %v", err)
	}
	// A new pair answers as PutBlock does (#2099): not found, not contact.
	if _, err := f.s.OpenDM(ctx, f.alice.ID, f.carol.ID, f.tick()); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("open a new pair with a disabled channel-mate: %v", err)
	}
	// The existing pair still resumes, read only.
	if c, err := f.s.OpenDM(ctx, f.alice.ID, f.bob.ID, f.tick()); err != nil || c.ID != conv.ID {
		t.Errorf("resume with a disabled channel-mate = %+v, %v", c, err)
	}
}

type dmProbe struct {
	name string
	id   int64
}

func newDMProbeFixture(t *testing.T, seeAll bool) (*dmFixture, []dmProbe) {
	t.Helper()
	ctx := context.Background()
	f := newDMFixture(t)
	if _, err := f.s.db.Exec(`INSERT INTO guest_visits (principal_id, handle, origin, state, started_at, expires_at, idle_ms)
		VALUES (?, 'g-probe', 'address', 'active', 1, 2, 1000)`, f.carol.ID); err != nil {
		t.Fatal(err)
	}
	f.disable(t, f.dave)
	hidden, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "probe-gone")
	if err != nil {
		t.Fatal(err)
	}
	f.hide(t, hidden)
	disabledAgent, err := f.s.CreatePrincipal(ctx, PrincipalAgent, "disabled-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DisablePrincipal(ctx, "system", disabledAgent.ID); err != nil {
		t.Fatal(err)
	}
	ended, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "probe-ended")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO guest_visits (principal_id, handle, origin, state, started_at, expires_at, idle_ms, ended_at, end_reason)
		VALUES (?, 'g-ended', 'address', 'ended', 1, 2, 1000, 2, 'end_visit')`, ended.ID); err != nil {
		t.Fatal(err)
	}
	// Alice knows these targets even with hide_members. In particular, the
	// invitation would admit the ended guest through knowsCond.
	if _, err := f.s.db.Exec(`UPDATE principals SET invited_by = ? WHERE id IN (?, ?, ?)`,
		f.alice.ID, f.dave.ID, disabledAgent.ID, ended.ID); err != nil {
		t.Fatal(err)
	}
	probes := []dmProbe{
		{"never issued", 987654},
		{"hidden", hidden.ID},
		{"disabled human", f.dave.ID},
		{"disabled agent", disabledAgent.ID},
		{"active guest, no shared channel", f.carol.ID},
		{"ended guest invited by caller", ended.ID},
	}
	if !seeAll {
		f.hideMembers(t)
		probes = append(probes, dmProbe{"unknown member", f.bob.ID})
	}
	return f, probes
}

// OpenDM on a new pair and PutBlock refuse the same set of id probes (#2099).
func TestDMOpenAndPutBlockAnswerTheSameProbeSet(t *testing.T) {
	ctx := context.Background()
	for mode, seeAll := range map[string]bool{"full_directory": true, "hide_members": false} {
		t.Run(mode, func(t *testing.T) {
			f, probes := newDMProbeFixture(t, seeAll)
			for _, c := range probes {
				t.Run(c.name, func(t *testing.T) {
					_, openErr := f.s.OpenDM(ctx, f.alice.ID, c.id, f.tick())
					_, _, blockErr := f.s.PutBlock(ctx, f.alice.ID, c.id, seeAll, f.tick())
					if !errors.Is(openErr, ErrPrincipalNotFound) || openErr.Error() != ErrPrincipalNotFound.Error() {
						t.Errorf("OpenDM = %v, want principal not found", openErr)
					}
					if !errors.Is(blockErr, ErrNotFound) {
						t.Errorf("PutBlock = %v, want not found", blockErr)
					}
				})
			}
		})
	}
}

func TestOpenDMRunsTheSameStatementsInEveryProbeClass(t *testing.T) {
	ctx := context.Background()
	for mode, seeAll := range map[string]bool{"full_directory": true, "hide_members": false} {
		t.Run(mode, func(t *testing.T) {
			f, probes := newDMProbeFixture(t, seeAll)
			var want int
			for _, probe := range probes {
				t.Run(probe.name, func(t *testing.T) {
					var count int
					err := f.s.withImmediateTx(ctx, func(tx execer) error {
						c := &countingExecer{execer: tx}
						_, err := openDMTx(ctx, c, f.alice.ID, probe.id, f.tick(), nil)
						count = c.n
						return err
					})
					if !errors.Is(err, ErrPrincipalNotFound) {
						t.Fatalf("openDMTx = %v, want principal not found", err)
					}
					if want == 0 {
						want = count
					}
					if count != want {
						t.Errorf("openDMTx ran %d statements, a never-issued id ran %d", count, want)
					}
				})
			}
		})
	}
}

// on hide_members the target knows the caller (the caller joined by
// the target's link) but the caller may not know the target.
func TestDMPolicyCallerMustKnowTargetOnWrite(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	mine := f.send(t, conv, f.alice, "hi")
	f.hideMembers(t)
	if _, err := f.s.db.Exec("UPDATE principals SET invited_by = ? WHERE id = ?", f.bob.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.trySend(conv, f.alice, "to someone I may not know"); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("send: %v", err)
	}
	if _, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, mine.ID, "edited", f.tick); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("edit: %v", err)
	}
}

// an ended guest is not found, in the same words.
func TestDMPolicyEndedGuestIsNotFound(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	if _, err := f.s.db.Exec(`INSERT INTO guest_visits (principal_id, handle, origin, state, started_at, expires_at, idle_ms, ended_at, end_reason)
		VALUES (?, 'g1', 'address', 'ended', 1, 2, 1000, 2, 'end_visit')`, f.carol.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.s.OpenDM(ctx, f.alice.ID, f.carol.ID, f.tick())
	if !errors.Is(err, ErrPrincipalNotFound) || err.Error() != ErrPrincipalNotFound.Error() {
		t.Errorf("ended guest: %v", err)
	}
}

// a ban ends "shares a channel" for the shared_channel preference.
func TestDMPolicyBanEndsSharingAChannel(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	ch, err := f.s.CreateChannel(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []Principal{f.alice, f.bob} {
		if _, err := f.s.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	conv := f.openPair(t, f.alice, f.bob)
	f.send(t, conv, f.alice, "hi")
	f.prefer(t, f.bob, schema.DMAcceptSharedChannel)
	if err := f.trySend(conv, f.alice, "while sharing"); err != nil {
		t.Fatalf("send while sharing: %v", err)
	}
	if _, err := f.s.db.Exec("INSERT INTO channel_bans (channel_id, principal_id, created_at) VALUES (?, ?, 1)", ch.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.trySend(conv, f.alice, "after a ban"); !errors.Is(err, ErrDMContactUnavailable) {
		t.Errorf("send after a ban from the only shared channel: %v", err)
	}
}
