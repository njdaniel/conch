package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Personal blocks (issue #1832): the owner's list, its limit and
// idempotence, and that the owner's feed and wakes leave a blocked author out
// while a decision still comes through.

func TestBlocksOwnList(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mk := func(kind PrincipalKind, name string) Principal {
		p, err := s.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	owner, target, bot, other := mk(PrincipalHuman, "owner"), mk(PrincipalHuman, "target"), mk(PrincipalAgent, "bot"), mk(PrincipalHuman, "other")
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

	first, created, err := s.PutBlock(ctx, owner.ID, target.ID, true, at)
	if err != nil || !created {
		t.Fatalf("PutBlock = %v, created %v; want a new block", err, created)
	}
	if first.PrincipalID != target.ID || first.Name != "target" || first.Kind != schema.PrincipalHuman || !first.CreatedAt.Time().Equal(at) {
		t.Errorf("entry = %+v", first)
	}
	// Idempotent: the same entry, with its original time.
	again, created, err := s.PutBlock(ctx, owner.ID, target.ID, true, at.Add(time.Hour))
	if err != nil || created || again != first {
		t.Errorf("second PutBlock = %+v, created %v, %v; want %+v, false, nil", again, created, err, first)
	}
	if _, _, err := s.PutBlock(ctx, owner.ID, bot.ID, true, at.Add(time.Minute)); err != nil {
		t.Fatalf("block an agent: %v", err)
	}

	list, err := s.ListBlocks(ctx, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp := schema.ListBlocksResponseV1{Blocks: list, Limit: schema.MaxBlocks}
	if err := resp.Validate(); err != nil {
		t.Errorf("list does not validate: %v", err)
	}
	if len(list) != 2 || list[0].PrincipalID != target.ID || list[1].PrincipalID != bot.ID || list[1].Kind != schema.PrincipalAgent {
		t.Errorf("list = %+v, want target then bot", list)
	}

	// Nobody else's list shows it: not the blocked person's, not a third
	// person's, and not an id that is nobody.
	for _, id := range []int64{target.ID, other.ID, bot.ID, 987654} {
		l, err := s.ListBlocks(ctx, id)
		if err != nil || l == nil || len(l) != 0 {
			t.Errorf("ListBlocks(%d) = %v, %v; want an empty list", id, l, err)
		}
	}
	for _, q := range [][2]int64{{target.ID, owner.ID}, {other.ID, target.ID}} {
		if got, err := s.IsBlocked(ctx, q[0], q[1]); err != nil || got {
			t.Errorf("IsBlocked(%d, %d) = %v, %v; want false", q[0], q[1], got, err)
		}
	}
	if got, err := s.IsBlocked(ctx, owner.ID, target.ID); err != nil || !got {
		t.Errorf("IsBlocked(owner, target) = %v, %v; want true", got, err)
	}

	// Refusals.
	if _, _, err := s.PutBlock(ctx, owner.ID, owner.ID, true, at); !errors.Is(err, ErrBlockSelf) {
		t.Errorf("block self = %v, want ErrBlockSelf", err)
	}
	if _, _, err := s.PutBlock(ctx, owner.ID, 987654, true, at); !errors.Is(err, ErrNotFound) {
		t.Errorf("block nobody = %v, want ErrNotFound", err)
	}
	// Without the directory, a principal the owner shares no channel with
	// is not found either: a PUT is no probe.
	if _, _, err := s.PutBlock(ctx, owner.ID, other.ID, false, at); !errors.Is(err, ErrNotFound) {
		t.Errorf("block a stranger without the directory = %v, want ErrNotFound", err)
	}

	// Delete is idempotent.
	for i, want := range []bool{true, false} {
		removed, err := s.DeleteBlock(ctx, owner.ID, target.ID)
		if err != nil || removed != want {
			t.Errorf("DeleteBlock #%d = %v, %v; want %v", i+1, removed, err, want)
		}
	}
	if removed, err := s.DeleteBlock(ctx, other.ID, bot.ID); err != nil || removed {
		t.Errorf("DeleteBlock of another's = %v, %v; want false", removed, err)
	}
	if got, _ := s.IsBlocked(ctx, owner.ID, bot.ID); !got {
		t.Error("another's delete removed the owner's block")
	}
}

func TestBlocksLimit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	owner, err := s.CreatePrincipal(ctx, PrincipalHuman, "owner")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// Fill the list directly; PutBlock is what is under test at the edge.
	ids := make([]int64, 0, schema.MaxBlocks+2)
	for i := range schema.MaxBlocks + 2 {
		p, err := s.CreatePrincipal(ctx, PrincipalHuman, fmt.Sprintf("p%d", i))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, p.ID)
	}
	for _, id := range ids[:schema.MaxBlocks-1] {
		if _, err := s.db.Exec(`INSERT INTO blocks (owner_id, blocked_id, created_at) VALUES (?, ?, ?)`, owner.ID, id, now.UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	// Two concurrent PUTs for the last place: exactly one fits.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, errs[i] = s.PutBlock(ctx, owner.ID, ids[schema.MaxBlocks-1+i], true, now)
		}()
	}
	wg.Wait()
	full := 0
	for _, err := range errs {
		switch {
		case errors.Is(err, ErrBlockLimit):
			full++
		case err != nil:
			t.Fatalf("PutBlock: %v", err)
		}
	}
	if full != 1 {
		t.Errorf("%d of two PUTs for the last place were refused, want 1", full)
	}
	list, err := s.ListBlocks(ctx, owner.ID)
	if err != nil || len(list) != schema.MaxBlocks {
		t.Fatalf("list holds %d, %v; want %d", len(list), err, schema.MaxBlocks)
	}
	// A principal already on a full list is still answered.
	if _, created, err := s.PutBlock(ctx, owner.ID, ids[0], true, now); err != nil || created {
		t.Errorf("re-PUT on a full list = %v, created %v; want nil, false", err, created)
	}
	// After a delete there is room again.
	if _, err := s.DeleteBlock(ctx, owner.ID, ids[0]); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutBlock(ctx, owner.ID, ids[schema.MaxBlocks+1], true, now); err != nil {
		t.Errorf("PUT after a delete = %v, want nil", err)
	}
}

// A hidden principal is not found, as in the directory; a disabled one can
// still be blocked.
func TestBlocksHiddenTarget(t *testing.T) {
	f := newFeedFixture(t)
	ctx := context.Background()
	if _, err := f.s.DisablePrincipal(ctx, "system", f.frank.ID); err != nil {
		t.Fatal(err)
	}
	// frank shares dave's channel: known without the directory.
	if _, _, err := f.s.PutBlock(ctx, f.dave.ID, f.frank.ID, false, time.Now()); err != nil {
		t.Errorf("block a disabled channel-mate = %v, want nil", err)
	}
	if _, err := f.s.db.Exec(`UPDATE principals SET hidden_at = 1 WHERE id = ?`, f.erin.ID); err == nil {
		t.Fatal("hid an enabled principal; the CHECK should refuse it")
	}
	if _, err := f.s.DisablePrincipal(ctx, "system", f.erin.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`UPDATE principals SET hidden_at = 1 WHERE id = ?`, f.erin.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.s.PutBlock(ctx, f.dave.ID, f.erin.ID, true, time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("block a hidden principal = %v, want ErrNotFound", err)
	}
}

// decisionCard writes a message carrying an approval payload, as the
// approval path does, without the rest of the approval.
func decisionCard(t *testing.T, s *Store, channelID int64, author Principal) int64 {
	t.Helper()
	res, err := s.db.Exec(`INSERT INTO messages (channel_id, author_id, body, created_at, payload_schema, payload_json) VALUES (?, ?, '', ?, ?, '{}')`,
		channelID, author.ID, time.Now().UnixMilli(), schema.ApprovalV1Name)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// The owner's feed and wakes leave out a blocked author's messages and
// mentions, keep everyone else's, keep a decision card from the blocked
// author, and change nothing for anyone else.
func TestBlocksQuietTheOwnersAlerts(t *testing.T) {
	f := newPushTargetFixture(t)
	ctx := context.Background()
	f.policy(t, schema.NotificationPolicyAll, false)
	// frank reads the same channel, with a phone of his own.
	frankCred := credentialOf(t, f.s, f.frank, "frank-phone")
	putPrefs(t, f.s, frankCred, schema.NotificationPreferencesV1{Policy: schema.NotificationPolicyAll, Muted: []int64{}})
	frankReg := registerPush(t, f.s, frankCred, webPushTarget("-frank"))

	plain := f.post(t, f.ch.ID, f.frank, "hello there")
	mention := f.post(t, f.ch.ID, f.frank, "ping @dave please")
	byErin := f.post(t, f.ch.ID, f.erin, "erin here")
	card := decisionCard(t, f.s, f.ch.ID, f.frank)
	toFrank := f.post(t, f.ch.ID, f.dave, "dave to frank")

	if _, _, err := f.s.PutBlock(ctx, f.dave.ID, f.frank.ID, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	wantWake := map[int64]bool{plain: false, mention: false, byErin: true, card: true}
	for id, want := range wantWake {
		if got := f.wakes(t, f.ch.ID, id, f.frank); got != want {
			t.Errorf("message %d: dave woken = %v, want %v", id, got, want)
		}
	}
	page := f.feed(t, 0)
	listed := map[int64]bool{}
	for _, sm := range page.Summaries {
		listed[sm.MessageID] = true
	}
	for id, want := range wantWake {
		if listed[id] != want {
			t.Errorf("message %d: in dave's feed = %v, want %v", id, listed[id], want)
		}
	}
	// The blocked person is unaffected: dave's message still wakes frank.
	got, err := f.s.PushTargets(ctx, f.ch.ID, toFrank, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range got {
		found = found || g.RegistrationID == frankReg.ID
	}
	if !found {
		t.Error("frank's phone was not woken by dave's message: a block changed what the blocked person gets")
	}
	// Unblocked, frank's messages wake dave again.
	if _, err := f.s.DeleteBlock(ctx, f.dave.ID, f.frank.ID); err != nil {
		t.Fatal(err)
	}
	if !f.wakes(t, f.ch.ID, mention, f.frank) {
		t.Error("after unblock, frank's mention does not wake dave")
	}
}
