package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The notice is written only for a link another principal made: the member's
// own (the maker is the member) and a link with no maker write none. Every
// read and dismissal is scoped to the principal.
func TestAccountNotices(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	mk := func(name string) Principal {
		p, err := s.CreatePrincipal(ctx, PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	maker, member, other := mk("opal"), mk("wren"), mk("kit")
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	record := func(principal, by int64) {
		t.Helper()
		if err := s.withImmediateTx(ctx, func(tx execer) error {
			return noticePasswordSetByLinkTx(ctx, tx, principal, by, now)
		}); err != nil {
			t.Fatal(err)
		}
	}
	list := func(p Principal) []schema.AccountNoticeV1 {
		t.Helper()
		got, err := s.ListAccountNotices(ctx, p.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	record(member.ID, member.ID) // the member's own link
	record(member.ID, 0)         // no maker
	if got := list(member); len(got) != 0 {
		t.Fatalf("notices after the member's own link and a makerless one = %+v, want none", got)
	}

	record(member.ID, maker.ID)
	got := list(member)
	if len(got) != 1 || got[0].Kind != schema.AccountNoticeKindPasswordSetByLink || got[0].ByName != "opal" || !got[0].At.Time().Equal(now) {
		t.Fatalf("notices = %+v, want one by opal at %v", got, now)
	}
	if got := list(other); len(got) != 0 {
		t.Errorf("another member's notices = %+v, want none", got)
	}

	// Another member cannot dismiss it, nor tell whether it exists.
	if err := s.DismissAccountNotice(ctx, other.ID, got[0].ID, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("dismiss by another member = %v, want ErrNotFound", err)
	}
	if len(list(member)) != 1 {
		t.Fatal("another member's dismissal cleared the notice")
	}
	if err := s.DismissAccountNotice(ctx, member.ID, got[0].ID, now); err != nil {
		t.Fatalf("dismiss = %v", err)
	}
	if err := s.DismissAccountNotice(ctx, member.ID, got[0].ID, now); err != nil {
		t.Errorf("dismiss again = %v, want nil", err)
	}
	if err := s.DismissAccountNotice(ctx, member.ID, got[0].ID+99, now); !errors.Is(err, ErrNotFound) {
		t.Errorf("dismiss an id never issued = %v, want ErrNotFound", err)
	}
	if got := list(member); len(got) != 0 || got == nil {
		t.Errorf("notices after dismissing = %+v, want an empty, non-nil list", got)
	}
}
