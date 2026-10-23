package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/pkg/schema"
)

// hideFixture is an operator, root, and alice: a member of two channels, on
// a net in one of them, with a credential and a message.
type hideFixture struct {
	disableFixture
	general, other Channel
	net            Net
	aliceTok       string
	messageID      int64
}

func newHideFixture(t *testing.T) hideFixture {
	t.Helper()
	ctx := context.Background()
	f := hideFixture{disableFixture: newDisableFixture(t)}
	var err error
	if f.general, err = f.s.CreateChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	if f.other, err = f.s.CreateChannel(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []Channel{f.general, f.other} {
		if _, err := f.s.AddChannelMember(ctx, "system", ch.ID, f.alice.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	if f.net, err = f.s.CreateNet(ctx, "system", f.general.ID, "squad", f.op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PutNetMember(ctx, "system", f.general.ID, "squad", f.alice.ID, schema.NetRoleMember, f.op.ID); err != nil {
		t.Fatal(err)
	}
	_, f.aliceTok = f.cred(t, f.alice, nil)
	m, err := f.s.InsertMessage(ctx, f.general.ID, f.alice.ID, "hello")
	if err != nil {
		t.Fatal(err)
	}
	f.messageID = m.ID
	return f
}

func (f hideFixture) audit(t *testing.T) []AuditEvent {
	t.Helper()
	events, err := f.s.ListAuditEvents(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// Hiding disables the principal, takes it out of every channel and net,
// frees its name for anyone and keeps its messages and every earlier audit
// row, the old name among them.
func TestHidePrincipal(t *testing.T) {
	ctx := context.Background()
	f := newHideFixture(t)
	before := f.audit(t)

	res, err := f.s.HidePrincipal(ctx, principalActor(f.op.ID), f.alice.ID, HideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hidden := fmt.Sprintf("hidden-%d", f.alice.ID)
	if !res.Changed || res.Principal.Name != hidden || res.Principal.DisabledAt == nil || res.Principal.HiddenAt == nil {
		t.Errorf("result = %+v", res)
	}
	got, err := f.s.PrincipalByID(ctx, f.alice.ID)
	if err != nil || got.Name != hidden || got.DisabledAt == nil || got.HiddenAt == nil {
		t.Errorf("stored = %+v, %v", got, err)
	}
	storedKeysMatch(t, f.s)

	// Disabled: the credential is dead.
	if _, err := f.s.ResolveCredential(ctx, f.aliceTok); err == nil {
		t.Error("the hidden principal's credential still resolves")
	}
	// No membership anywhere.
	for _, table := range []string{"channel_members", "net_members"} {
		var n int
		if err := f.s.db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE principal_id = ?", f.alice.ID).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s rows of the hidden principal = %d (%v), want 0", table, n, err)
		}
	}
	members, err := f.s.ListChannelMembers(ctx, f.general.ID)
	if err != nil || len(members) != 0 {
		t.Errorf("general's members = %+v, %v; want none", members, err)
	}
	// The message stays, attributed to the renamed principal.
	var author int64
	if err := f.s.db.QueryRow("SELECT author_id FROM messages WHERE id = ?", f.messageID).Scan(&author); err != nil || author != f.alice.ID {
		t.Errorf("message author = %d, %v", author, err)
	}

	// Every earlier row is still there, unchanged, and the hide's rows follow.
	after := f.audit(t)
	if len(after) < len(before) || !reflect.DeepEqual(after[:len(before)], before) {
		t.Fatal("the hide changed an earlier audit row")
	}
	actor, subject := principalActor(f.op.ID), principalActor(f.alice.ID)
	var actions []string
	for _, e := range after[len(before):] {
		actions = append(actions, e.Action)
		if e.Actor != actor {
			t.Errorf("%s by %q, want %q", e.Action, e.Actor, actor)
		}
		switch e.Action {
		case AuditPrincipalHidden:
			if e.Subject != subject || e.Detail != `name="alice"` {
				t.Errorf("principal_hidden = %+v", e)
			}
		case AuditPrincipalRenamed:
			if e.Subject != subject || e.Detail != fmt.Sprintf(`from="alice" to=%q`, hidden) {
				t.Errorf("principal_renamed = %+v", e)
			}
		}
	}
	for action, want := range map[string]int{
		"principal_disabled":   1,
		"credential_revoked":   1,
		AuditMemberRemoved:     2,
		AuditNetMemberRemoved:  1,
		AuditPrincipalRenamed:  1,
		AuditPrincipalHidden:   1,
		AuditPrincipalUnhidden: 0,
	} {
		if got := count(actions, action); got != want {
			t.Errorf("%s rows = %d, want %d (all: %v)", action, got, want, actions)
		}
	}
	if actions[len(actions)-1] != AuditPrincipalHidden {
		t.Errorf("last row = %s, want principal_hidden", actions[len(actions)-1])
	}

	// The name is free, in any spelling, and the hidden name is not.
	if _, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "Alice"); err != nil {
		t.Errorf("create the freed name: %v", err)
	}

	// Hiding again changes and writes nothing.
	rows := countRows(t, f.s, "audit_events")
	again, err := f.s.HidePrincipal(ctx, actor, f.alice.ID, HideOptions{})
	if err != nil || again.Changed || again.Principal.Name != hidden {
		t.Errorf("second hide = %+v, %v", again, err)
	}
	if countRows(t, f.s, "audit_events") != rows {
		t.Error("a second hide wrote audit rows")
	}
	if _, err := f.s.HidePrincipal(ctx, actor, 9999, HideOptions{}); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("unknown principal = %v, want ErrPrincipalNotFound", err)
	}
}

// A hide revokes the principal's webhooks, each audited, so enabling the
// account later cannot bring one back; other principals' hooks are untouched.
func TestHideRevokesHooks(t *testing.T) {
	ctx := context.Background()
	f := newHideFixture(t)
	for _, tok := range []string{"alice-hook-1", "alice-hook-2"} {
		if _, err := f.s.CreateHookWithLabel(ctx, "system", tok, "ci", f.general.ID, f.alice.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.s.CreateHook(ctx, "system", "root-hook", f.general.ID, f.op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.HookByToken(ctx, "alice-hook-1"); err != nil {
		t.Fatalf("hook before the hide: %v", err)
	}
	if _, err := f.s.HidePrincipal(ctx, principalActor(f.op.ID), f.alice.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EnablePrincipal(ctx, "system", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AddChannelMember(ctx, "system", f.general.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{"alice-hook-1", "alice-hook-2"} {
		if _, err := f.s.HookByToken(ctx, tok); !errors.Is(err, ErrNotFound) {
			t.Errorf("hook %s after hide and enable = %v, want ErrNotFound", tok, err)
		}
	}
	if _, err := f.s.HookByToken(ctx, "root-hook"); err != nil {
		t.Errorf("another principal's hook: %v", err)
	}
	var details []string
	for _, e := range f.audit(t) {
		if e.Action == "hook_revoked" {
			details = append(details, e.Detail)
			if e.Actor != principalActor(f.op.ID) || e.Subject != principalActor(f.alice.ID) || strings.Contains(e.Detail, "alice-hook") {
				t.Errorf("hook_revoked = %+v", e)
			}
		}
	}
	if len(details) != 2 || !strings.HasSuffix(details[0], `label="ci" reason=principal hidden`) {
		t.Errorf("hook_revoked details = %q", details)
	}
}

// A principal already disabled is hidden without being disabled again.
func TestHideDisabledPrincipal(t *testing.T) {
	ctx := context.Background()
	f := newHideFixture(t)
	if _, err := f.s.DisablePrincipal(ctx, "system", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.HidePrincipal(ctx, "system", f.alice.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	actions := f.actions(t)
	if count(actions, "principal_disabled") != 1 || count(actions, AuditPrincipalHidden) != 1 || count(actions, AuditMemberRemoved) != 2 {
		t.Errorf("actions = %v", actions)
	}
}

// The last operator able to sign in cannot be hidden, and the refusal writes
// nothing. Another operator can be, unless the caller refuses operators, as
// a bulk hide does.
func TestHideOperators(t *testing.T) {
	ctx := context.Background()
	f := newHideFixture(t)
	if _, err := f.s.AddChannelMember(ctx, "system", f.general.ID, f.op.ID, 0); err != nil {
		t.Fatal(err)
	}
	rows := countRows(t, f.s, "audit_events")
	if _, err := f.s.HidePrincipal(ctx, "system", f.op.ID, HideOptions{}); !errors.Is(err, ErrLastOperator) {
		t.Fatalf("hide the last operator = %v, want ErrLastOperator", err)
	}
	op, err := f.s.PrincipalByID(ctx, f.op.ID)
	if err != nil || op.Name != "root" || op.DisabledAt != nil || op.HiddenAt != nil {
		t.Errorf("the last operator after the refusal = %+v, %v", op, err)
	}
	if ok, err := f.s.IsChannelMember(ctx, f.general.ID, f.op.ID); err != nil || !ok {
		t.Errorf("the refusal took the operator out of general (%v)", err)
	}
	if countRows(t, f.s, "audit_events") != rows {
		t.Error("the refusal wrote audit rows")
	}

	// A second owner, with a credential: either may now be hidden, but
	// not by a caller that refuses operators.
	if _, err := f.s.db.Exec("UPDATE principals SET role = 'operator', owner = 1 WHERE id = ?", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.HidePrincipal(ctx, "system", f.alice.ID, HideOptions{RefuseOperator: true}); !errors.Is(err, ErrHideOperator) {
		t.Errorf("hide an operator refusing operators = %v, want ErrHideOperator", err)
	}
	if countRows(t, f.s, "audit_events") != rows {
		t.Error("the refusal wrote audit rows")
	}
	if _, err := f.s.HidePrincipal(ctx, "system", f.op.ID, HideOptions{}); err != nil {
		t.Errorf("hide one of two operators: %v", err)
	}
}

// Enabling a hidden principal clears hidden_at and nothing else comes back:
// not its name, its memberships or its credentials.
func TestEnableAfterHide(t *testing.T) {
	ctx := context.Background()
	f := newHideFixture(t)
	if _, err := f.s.HidePrincipal(ctx, "system", f.alice.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	changed, err := f.s.EnablePrincipal(ctx, principalActor(f.op.ID), f.alice.ID)
	if err != nil || !changed {
		t.Fatalf("enable = %v, %v", changed, err)
	}
	p, err := f.s.PrincipalByID(ctx, f.alice.ID)
	if err != nil || p.DisabledAt != nil || p.HiddenAt != nil || p.Name != fmt.Sprintf("hidden-%d", f.alice.ID) {
		t.Errorf("after enable = %+v, %v", p, err)
	}
	if ok, err := f.s.IsChannelMember(ctx, f.general.ID, f.alice.ID); err != nil || ok {
		t.Errorf("membership came back (%v)", err)
	}
	if _, err := f.s.ResolveCredential(ctx, f.aliceTok); err == nil {
		t.Error("the old credential works again")
	}
	actions := f.actions(t)
	if count(actions, "principal_enabled") != 1 || count(actions, AuditPrincipalUnhidden) != 1 {
		t.Errorf("actions = %v", actions)
	}
	// Enabling a principal that was never hidden writes no principal_unhidden.
	if _, err := f.s.DisablePrincipal(ctx, "system", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EnablePrincipal(ctx, "system", f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if got := count(f.actions(t), AuditPrincipalUnhidden); got != 1 {
		t.Errorf("principal_unhidden rows = %d, want 1", got)
	}
	// Hidden again, it keeps its hidden name and is not renamed to itself.
	renames := count(f.actions(t), AuditPrincipalRenamed)
	if _, err := f.s.HidePrincipal(ctx, "system", f.alice.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := count(f.actions(t), AuditPrincipalRenamed); got != renames {
		t.Errorf("hiding a principal already called hidden-<id> renamed it (%d renames, was %d)", got, renames)
	}
}

// The schema holds a hidden principal disabled.
func TestHiddenMeansDisabled(t *testing.T) {
	f := newHideFixture(t)
	if _, err := f.s.db.Exec("UPDATE principals SET hidden_at = 1 WHERE id = ?", f.alice.ID); err == nil {
		t.Error("an enabled principal was marked hidden")
	}
}

// Should a name written before the reservation hold "hidden-<id>", the hide
// takes the next suffix instead of failing.
func TestHiddenNameTaken(t *testing.T) {
	ctx := context.Background()
	f := newHideFixture(t)
	insertNamed := func(name string) {
		t.Helper()
		k := names.KeysOf(name)
		if _, err := f.s.db.Exec(`INSERT INTO principals (kind, name, name_key, name_skel, created_at) VALUES ('human', ?, ?, ?, 1)`,
			name, k.Folded, k.Skeletal); err != nil {
			t.Fatal(err)
		}
	}
	// One in the exact spelling, one a look-alike of the first suffix.
	insertNamed(fmt.Sprintf("hidden-%d", f.alice.ID))
	insertNamed(fmt.Sprintf("HIDDEN-%d-2", f.alice.ID))
	res, err := f.s.HidePrincipal(ctx, "system", f.alice.ID, HideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("hidden-%d-3", f.alice.ID); res.Principal.Name != want {
		t.Errorf("name = %q, want %q", res.Principal.Name, want)
	}
	storedKeysMatch(t, f.s)
}

// Names whose key begins with the key of "hidden-" are reserved: no create,
// bootstrap, redemption or rename may take one, by whatever spelling, and
// nothing is written. Only a hide writes them.
func TestReservedNames(t *testing.T) {
	ctx := context.Background()
	reserved := []string{
		"hidden-12",
		"hidden-l2", // a lower-case L for the 1
		"HIDDEN-12", // case
		"hіdden-12", // a Cyrillic i
		"hidden‐12", // a Unicode hyphen
		"ｈｉｄｄｅｎ-1",  // fullwidth letters
		"hidden-",
		"hidden-someone",
	}
	paths := []struct {
		name  string
		write func(t *testing.T, s *Store, name string) error
	}{
		{"create", func(t *testing.T, s *Store, name string) error {
			_, err := s.CreatePrincipalAs(ctx, "system", PrincipalAgent, name)
			return err
		}},
		{"bootstrap", func(t *testing.T, s *Store, name string) error {
			_, err := s.BootstrapOperatorWith(ctx, name, BootstrapOptions{})
			return err
		}},
		{"redeem", func(t *testing.T, s *Store, name string) error {
			op, _, _, err := s.BootstrapOperator(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.CreateChannel(ctx, "general"); err != nil {
				t.Fatal(err)
			}
			inv, err := s.CreateInvite(ctx, principalActor(op.ID), op.ID,
				schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}, InviteOptions{}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RedeemInvite(ctx, HashInviteCode(inv.Code), name, time.Now())
			return err
		}},
		{"own rename", func(t *testing.T, s *Store, name string) error {
			p, err := s.CreatePrincipal(ctx, PrincipalHuman, "someone")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RenamePrincipal(ctx, principalActor(p.ID), p.ID, name, RenameLimits{Renames: 5, Attempts: 20}, time.Now())
			return err
		}},
		{"operator rename", func(t *testing.T, s *Store, name string) error {
			p, err := s.CreatePrincipal(ctx, PrincipalHuman, "someone")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.RenamePrincipal(ctx, "system", p.ID, name, RenameLimits{}, time.Now())
			return err
		}},
	}
	for _, path := range paths {
		for _, name := range reserved {
			t.Run(fmt.Sprintf("%s %+q", path.name, name), func(t *testing.T) {
				s := openTestStore(t)
				if err := path.write(t, s, name); !errors.Is(err, ErrDuplicate) || !errors.Is(err, ErrNameReserved) {
					t.Fatalf("error = %v, want ErrNameReserved and ErrDuplicate", err)
				}
				var n int
				if err := s.db.QueryRow("SELECT COUNT(*) FROM principals WHERE name = ?", name).Scan(&n); err != nil || n != 0 {
					t.Errorf("%d principals called %q after the refusal (%v)", n, name, err)
				}
			})
		}
	}

	// Names that only contain the word, or lack the hyphen, are anyone's.
	s := openTestStore(t)
	for _, name := range []string{"hidden", "hiddenite", "the hidden-1", "hidden 1", "unhidden-1"} {
		if _, err := s.CreatePrincipal(ctx, PrincipalHuman, name); err != nil {
			t.Errorf("create %q: %v", name, err)
		}
	}
}

// A bulk hide's selection: by name prefix (exactly as typed) or invite, by
// kind, never an operator, the caller or a principal already hidden, and at
// most limit at a time with the rest counted.
func TestHideTargets(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	op, _, _, err := s.BootstrapOperator(ctx, "test-root")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(kind PrincipalKind, name string) Principal {
		t.Helper()
		p, err := s.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	a := mk(PrincipalHuman, "test-a")
	b := mk(PrincipalAgent, "test-b")
	caller := mk(PrincipalHuman, "test-caller")
	mk(PrincipalHuman, "Test-c") // a capital T: not the prefix as typed
	mk(PrincipalHuman, "tes")
	d := mk(PrincipalHuman, "test-d")
	ж := mk(PrincipalHuman, "жжж-1")
	if _, err := s.HidePrincipal(ctx, "system", d.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	inv, err := s.CreateInvite(ctx, principalActor(op.ID), op.ID,
		schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}}, InviteOptions{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var joiners []int64
	for _, name := range []string{"test-j1", "j2"} {
		r, err := s.RedeemInvite(ctx, HashInviteCode(inv.Code), name, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		joiners = append(joiners, r.Principal.ID)
	}

	ids := func(ps []Principal) []int64 {
		out := []int64{}
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return out
	}
	tests := []struct {
		name      string
		sel       HideSelector
		limit     int
		want      []int64
		remaining int
	}{
		{"prefix", HideSelector{NamePrefix: "test-"}, 500, []int64{a.ID, b.ID, joiners[0]}, 0},
		{"prefix, humans", HideSelector{NamePrefix: "test-", Kind: PrincipalHuman}, 500, []int64{a.ID, joiners[0]}, 0},
		{"prefix, agents", HideSelector{NamePrefix: "test-", Kind: PrincipalAgent}, 500, []int64{b.ID}, 0},
		{"another script", HideSelector{NamePrefix: "жжж"}, 500, []int64{ж.ID}, 0},
		{"invite", HideSelector{InviteID: inv.Invite.ID}, 500, joiners, 0},
		{"invite and prefix", HideSelector{InviteID: inv.Invite.ID, NamePrefix: "test-"}, 500, joiners[:1], 0},
		{"capped", HideSelector{NamePrefix: "test-"}, 2, []int64{a.ID, b.ID}, 1},
		{"nobody", HideSelector{NamePrefix: "nobody"}, 500, []int64{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, remaining, err := s.HideTargets(ctx, tt.sel, caller.ID, tt.limit)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ids(got), tt.want) || remaining != tt.remaining {
				t.Errorf("targets = %v remaining %d, want %v remaining %d", ids(got), remaining, tt.want, tt.remaining)
			}
		})
	}
	if _, _, err := s.HideTargets(ctx, HideSelector{InviteID: 999}, caller.ID, 500); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown invite = %v, want ErrNotFound", err)
	}
	if got := countRows(t, s, "audit_events"); got == 0 {
		t.Fatal("setup wrote no audit rows")
	}
}

// The operator's list: states, filters, newest first, paged by id, and the
// live credential count.
func TestListPrincipals(t *testing.T) {
	ctx := context.Background()
	f := newHideFixture(t)
	now := time.Now()
	bob, err := f.s.CreatePrincipal(ctx, PrincipalAgent, "bob")
	if err != nil {
		t.Fatal(err)
	}
	carol, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "carol")
	if err != nil {
		t.Fatal(err)
	}
	f.cred(t, bob, nil)
	// A second that has expired is not live.
	expired, _ := f.cred(t, bob, nil)
	if _, err := f.s.db.Exec("UPDATE credentials SET expires_at = ? WHERE id = ?", now.Add(-time.Hour).UnixMilli(), expired); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DisablePrincipal(ctx, "system", carol.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.HidePrincipal(ctx, "system", f.alice.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	ids := func(ps []schema.PrincipalAdminV1) []int64 {
		out := []int64{}
		for _, p := range ps {
			out = append(out, p.ID)
		}
		return out
	}
	tests := []struct {
		name string
		f    PrincipalFilter
		want []int64
	}{
		{"default: everyone not hidden", PrincipalFilter{}, []int64{carol.ID, bob.ID, f.op.ID}},
		{"active", PrincipalFilter{State: schema.PrincipalStateActive}, []int64{bob.ID, f.op.ID}},
		{"disabled", PrincipalFilter{State: schema.PrincipalStateDisabled}, []int64{carol.ID}},
		{"hidden", PrincipalFilter{State: schema.PrincipalStateHidden}, []int64{f.alice.ID}},
		{"agents", PrincipalFilter{Kind: PrincipalAgent}, []int64{bob.ID}},
		{"operators", PrincipalFilter{Role: RoleOperator}, []int64{f.op.ID}},
		{"prefix", PrincipalFilter{NamePrefix: "ca"}, []int64{carol.ID}},
		{"hidden by prefix", PrincipalFilter{NamePrefix: "hidden-", State: schema.PrincipalStateHidden}, []int64{f.alice.ID}},
		{"first page", PrincipalFilter{Limit: 2}, []int64{carol.ID, bob.ID}},
		{"next page", PrincipalFilter{Limit: 2, Before: bob.ID}, []int64{f.op.ID}},
		{"invite", PrincipalFilter{InviteID: 1}, []int64{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.f.Limit == 0 {
				tt.f.Limit = 100
			}
			got, err := f.s.ListPrincipals(ctx, tt.f, now)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(ids(got), tt.want) {
				t.Errorf("ids = %v, want %v", ids(got), tt.want)
			}
		})
	}

	row, err := f.s.PrincipalAdmin(ctx, bob.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if row.LiveCredentials != 1 || row.Kind != schema.PrincipalAgent || row.Role != schema.RoleMember || row.DisabledAt != nil || row.HiddenAt != nil || row.InviteID != nil {
		t.Errorf("bob = %+v", row)
	}
	row, err = f.s.PrincipalAdmin(ctx, f.alice.ID, now)
	if err != nil || row.LiveCredentials != 0 || row.DisabledAt == nil || row.HiddenAt == nil {
		t.Errorf("alice = %+v, %v", row, err)
	}
	if _, err := f.s.PrincipalAdmin(ctx, 9999, now); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("unknown = %v, want ErrPrincipalNotFound", err)
	}
	if _, err := f.s.ListPrincipals(ctx, PrincipalFilter{State: "gone", Limit: 1}, now); err == nil {
		t.Error("an unknown state was accepted")
	}
}

// The creation paths an operator uses write their audit rows in the
// creating transaction: principal_created (kind) and channel_created (name).
func TestCreationAudit(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	p, err := s.CreatePrincipalAs(ctx, "principal:1", PrincipalAgent, "bot")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.LastAuditEvent(ctx, AuditPrincipalCreated)
	if err != nil || e.Actor != "principal:1" || e.Subject != principalActor(p.ID) || e.Detail != "kind=agent" {
		t.Errorf("principal_created = %+v, %v", e, err)
	}
	if _, err := s.CreatePrincipalAs(ctx, "", PrincipalAgent, "nobody"); err == nil {
		t.Error("a create with no actor was accepted")
	}
	ch, err := s.CreateChannelAs(ctx, "principal:1", "ops room", p.ID)
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.LastAuditEvent(ctx, AuditChannelCreated)
	if err != nil || e.Actor != "principal:1" || e.Subject != channelSubject(ch.ID) || e.Detail != `name="ops room"` {
		t.Errorf("channel_created = %+v, %v", e, err)
	}
	// A refused create writes nothing.
	rows := countRows(t, s, "audit_events")
	if _, err := s.CreatePrincipalAs(ctx, "principal:1", PrincipalAgent, "BOT"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := s.CreateChannelAs(ctx, "principal:1", "ops room", p.ID); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate channel = %v", err)
	}
	if countRows(t, s, "audit_events") != rows {
		t.Error("a refused create wrote audit rows")
	}
}

// A database from before hidden principals: the migration adds hidden_at
// (NULL for all), keeps names that are now reserved, and names them in the
// log; from then on they cannot be taken again.
func TestHiddenAtMigration(t *testing.T) {
	const pre = hiddenAtMigration - 1
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pre; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
	}
	seed := []string{"root", "hidden-7", "HIDDEN-x", "hiddenite"}
	for i, name := range seed {
		k := names.KeysOf(name)
		if _, err := db.Exec(`INSERT INTO principals (id, kind, name, name_key, name_skel, created_at) VALUES (?, 'human', ?, ?, ?, 1)`,
			i+1, name, k.Folded, k.Skeletal); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", pre)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Writer(&logs), nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if out := logs.String(); !strings.Contains(out, "reserved for hidden principals") || !strings.Contains(out, `principals="[2 3]"`) {
		t.Errorf("the migration's report = %q, want principals 2 and 3", out)
	}
	for i, name := range seed {
		p, err := s.PrincipalByID(ctx, int64(i+1))
		if err != nil || p.Name != name || p.HiddenAt != nil {
			t.Errorf("principal %d = %+v, %v", i+1, p, err)
		}
	}
	if _, err := s.CreatePrincipal(ctx, PrincipalHuman, "hidden-8"); !errors.Is(err, ErrDuplicate) {
		t.Errorf("create a reserved name after the migration = %v, want ErrDuplicate", err)
	}
	// One that has such a name may still be renamed out of it.
	if _, err := s.RenamePrincipal(ctx, "system", 2, "seven", RenameLimits{}, time.Now()); err != nil {
		t.Errorf("rename out of a reserved name: %v", err)
	}
}
