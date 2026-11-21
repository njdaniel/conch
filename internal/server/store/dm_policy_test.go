package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// The DM contact policy (issue #2018, dm_policy.go; ADR-017 Decisions 3, 4,
// 9, 10 and 11; design §3.2). Alice is the caller throughout and bob the
// target, unless a case says otherwise.

// hideMembers turns member see_member_directory off: a hide_members server,
// where a member knows only the people they share a channel with.
func (f *dmFixture) hideMembers(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// Only an owner (or a holder of change_server_settings) writes the
	// document; actor 0 is refused.
	owner, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE principals SET role = 'operator', owner = 1 WHERE id = ?", owner.ID); err != nil {
		t.Fatal(err)
	}
	doc, err := f.s.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.PutPermissions(ctx, principalActor(owner.ID), owner.ID, schema.PutPermissionsRequestV1{
		Revision: doc.Revision,
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: false}},
	}, f.tick()); err != nil {
		t.Fatal(err)
	}
}

// shareChannel puts the people in one new channel.
func (f *dmFixture) shareChannel(t *testing.T, name string, people ...Principal) {
	t.Helper()
	ctx := context.Background()
	ch, err := f.s.CreateChannel(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range people {
		if _, err := f.s.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *dmFixture) block(t *testing.T, owner, blocked Principal) {
	t.Helper()
	if _, _, err := f.s.PutBlock(context.Background(), owner.ID, blocked.ID, true, f.tick()); err != nil {
		t.Fatal(err)
	}
}

func (f *dmFixture) prefer(t *testing.T, who Principal, accept schema.DMAcceptPolicy) {
	t.Helper()
	if _, err := f.s.SetDMPreferences(context.Background(), who.ID, schema.DMPreferencesV1{Accept: accept}, f.tick()); err != nil {
		t.Fatal(err)
	}
}

func (f *dmFixture) switchOff(t *testing.T, who Principal) {
	t.Helper()
	if _, err := f.s.SetDMAllowed(context.Background(), "system", 0, who.ID, false, f.tick()); err != nil {
		t.Fatal(err)
	}
}

func (f *dmFixture) serverOff(t *testing.T) {
	t.Helper()
	if _, err := f.s.SetDMServerSettings(context.Background(), "system", 0, schema.DMServerSettingsV1{DMsEnabled: false}, f.tick()); err != nil {
		t.Fatal(err)
	}
}

func (f *dmFixture) disable(t *testing.T, who Principal) {
	t.Helper()
	if _, err := f.s.DisablePrincipal(context.Background(), "system", who.ID); err != nil {
		t.Fatal(err)
	}
}

func (f *dmFixture) hide(t *testing.T, who Principal) {
	t.Helper()
	if _, err := f.s.HidePrincipal(context.Background(), "system", who.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
}

// trySend sends as from and returns the error.
func (f *dmFixture) trySend(c DMConversation, from Principal, body string) error {
	dmNonce++
	_, _, err := f.s.InsertDMMessage(context.Background(),
		DMPost{ConversationID: c.ID, AuthorID: from.ID, Body: body, ClientNonce: fmt.Sprintf("p%d", dmNonce)}, f.tick())
	return err
}

// dmRowCounts is how many rows the DM tables hold, to prove a refusal stored
// nothing.
func (f *dmFixture) dmRowCounts(t *testing.T) [3]int {
	t.Helper()
	var n [3]int
	for i, table := range []string{"dm_conversations", "dm_viewer_state", "direct_messages"} {
		if err := f.s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n[i]); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// TestDMContactPolicy is the contact and refusal matrix of design §3.2 for a
// new pair and for a pair that already talked: who may open, resume, send,
// edit, remove and read, and how the peer is labelled.
func TestDMContactPolicy(t *testing.T) {
	ctx := context.Background()
	type want struct {
		openNew  error // alice opens a pair she has never seen
		resume   error // alice opens the pair she already talked in
		send     error // alice sends in it
		read     bool  // alice still reads it, and removes her own message
		former   bool  // alice is shown bob as a former member
		bobReads bool  // bob still reads it
	}
	for _, c := range []struct {
		name string
		// setup runs after the existing pair (alice, carol... see below) has
		// talked, and before the checks.
		setup func(t *testing.T, f *dmFixture)
		want  want
	}{
		{"two members", nil,
			want{nil, nil, nil, true, false, true}},
		{"the target blocks the caller", func(t *testing.T, f *dmFixture) { f.block(t, f.bob, f.alice) },
			want{ErrDMContactUnavailable, nil, ErrDMContactUnavailable, true, false, true}},
		{"the caller blocks the target", func(t *testing.T, f *dmFixture) { f.block(t, f.alice, f.bob) },
			want{ErrDMContactUnavailable, nil, ErrDMContactUnavailable, true, false, true}},
		{"the target accepts nobody", func(t *testing.T, f *dmFixture) { f.prefer(t, f.bob, schema.DMAcceptNobody) },
			want{ErrDMContactUnavailable, nil, ErrDMContactUnavailable, true, false, true}},
		{"the target accepts shared channels, none shared", func(t *testing.T, f *dmFixture) { f.prefer(t, f.bob, schema.DMAcceptSharedChannel) },
			want{ErrDMContactUnavailable, nil, ErrDMContactUnavailable, true, false, true}},
		{"the target accepts shared channels, one shared", func(t *testing.T, f *dmFixture) {
			f.prefer(t, f.bob, schema.DMAcceptSharedChannel)
			f.shareChannel(t, "shared", f.alice, f.bob)
		}, want{nil, nil, nil, true, false, true}},
		{"the caller accepts nobody (it is the recipient's setting)", func(t *testing.T, f *dmFixture) { f.prefer(t, f.alice, schema.DMAcceptNobody) },
			want{nil, nil, nil, true, false, true}},
		{"the target is switched off", func(t *testing.T, f *dmFixture) { f.switchOff(t, f.bob) },
			want{ErrDMContactUnavailable, nil, ErrDMContactUnavailable, true, false, true}},
		{"the caller is switched off", func(t *testing.T, f *dmFixture) { f.switchOff(t, f.alice) },
			want{ErrDMsOff, nil, ErrDMsOff, true, false, true}},
		{"the server is switched off", func(t *testing.T, f *dmFixture) { f.serverOff(t) },
			want{ErrDMsOff, nil, ErrDMsOff, true, false, true}},
		{"hide_members, no channel shared", func(t *testing.T, f *dmFixture) { f.hideMembers(t) },
			// The target is hidden from the caller: not found by id, though
			// what was said reads on, with bob named (Decision 11).
			want{ErrPrincipalNotFound, ErrPrincipalNotFound, ErrDMContactUnavailable, true, false, true}},
		{"hide_members, a channel shared", func(t *testing.T, f *dmFixture) {
			f.hideMembers(t)
			f.shareChannel(t, "shared", f.alice, f.bob)
		}, want{nil, nil, nil, true, false, true}},
		{"hide_members, the caller sees the target but not the other way", func(t *testing.T, f *dmFixture) {
			f.hideMembers(t)
			// bob joined through alice's link (knowsCond's maker clause):
			// alice knows bob; bob does not know alice.
			if _, err := f.s.db.Exec("UPDATE principals SET invited_by = ? WHERE id = ?", f.alice.ID, f.bob.ID); err != nil {
				t.Fatal(err)
			}
		}, want{ErrDMContactUnavailable, nil, ErrDMContactUnavailable, true, false, true}},
		{"the target is disabled", func(t *testing.T, f *dmFixture) { f.disable(t, f.bob) },
			// A new pair is not found, as PutBlock answers (#2099); the pair
			// that exists still resumes, and send and edit stay refused.
			want{ErrPrincipalNotFound, nil, ErrDMContactUnavailable, true, false, false}},
		{"the target is disabled on hide_members, no longer sharing a channel", func(t *testing.T, f *dmFixture) {
			f.hideMembers(t)
			f.disable(t, f.bob)
		}, want{ErrPrincipalNotFound, ErrPrincipalNotFound, ErrDMContactUnavailable, true, true, false}},
		{"the target is removed (disabled and hidden)", func(t *testing.T, f *dmFixture) { f.hide(t, f.bob) },
			want{ErrPrincipalNotFound, ErrPrincipalNotFound, ErrDMContactUnavailable, true, true, false}},
		{"the target is restored", func(t *testing.T, f *dmFixture) {
			f.disable(t, f.bob)
			if _, err := f.s.EnablePrincipal(ctx, "system", f.bob.ID); err != nil {
				t.Fatal(err)
			}
		}, want{nil, nil, nil, true, false, true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newDMFixture(t)
			// The existing pair: alice and bob talked before the change.
			// The new pair is alice and a fresh copy of the same situation,
			// carol in bob's place, set up the same way below.
			conv := f.openPair(t, f.alice, f.bob)
			mine := f.send(t, conv, f.alice, "hello bob")
			f.send(t, conv, f.bob, "hello alice")
			if c.setup != nil {
				c.setup(t, f)
			}

			if _, err := f.s.OpenDM(ctx, f.alice.ID, f.bob.ID, f.tick()); !errors.Is(err, c.want.resume) {
				t.Errorf("resume: %v, want %v", err, c.want.resume)
			}
			if err := f.trySend(conv, f.alice, "again"); !errors.Is(err, c.want.send) {
				t.Errorf("send: %v, want %v", err, c.want.send)
			}
			if _, err := f.s.EditDMMessage(ctx, f.alice.ID, conv.ID, mine.ID, "hello again", f.tick); !errors.Is(err, c.want.send) {
				t.Errorf("edit: %v, want %v", err, c.want.send)
			}
			got, err := f.s.DMConversationFor(ctx, f.alice.ID, conv.ID)
			if (err == nil) != c.want.read {
				t.Fatalf("read: %v, want readable %v", err, c.want.read)
			}
			if got.PeerFormer != c.want.former {
				t.Errorf("peer former = %v, want %v", got.PeerFormer, c.want.former)
			}
			if list, err := f.s.ListDMConversations(ctx, f.alice.ID, schema.DMStateOpen, DMListCursor{}, 10); err != nil || len(list) != 1 || list[0].PeerFormer != c.want.former {
				t.Errorf("list: %+v, %v; want one with former %v", list, err, c.want.former)
			}
			if msgs, err := f.s.ListDMMessagesPage(ctx, f.alice.ID, conv.ID, MessagePage{}, 10); err != nil || len(msgs) < 2 {
				t.Errorf("history: %d messages, %v", len(msgs), err)
			}
			if _, err := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID); (err == nil) != c.want.bobReads {
				t.Errorf("bob reads: %v, want %v", err, c.want.bobReads)
			}
			if _, err := f.s.RemoveDMMessage(ctx, f.alice.ID, conv.ID, mine.ID, f.tick()); err != nil {
				t.Errorf("removing her own message: %v", err)
			}

			// A new pair in the same state: carol in bob's place.
			g := newDMFixture(t)
			g.bob, g.carol = g.carol, g.bob // the fresh bob has never talked to alice
			if c.setup != nil {
				c.setup(t, g)
			}
			before := g.dmRowCounts(t)
			_, err = g.s.OpenDM(ctx, g.alice.ID, g.bob.ID, g.tick())
			if !errors.Is(err, c.want.openNew) {
				t.Errorf("open a new pair: %v, want %v", err, c.want.openNew)
			}
			if err != nil && g.dmRowCounts(t) != before {
				t.Errorf("a refused open stored rows: %v then %v", before, g.dmRowCounts(t))
			}
		})
	}
}

// TestDMPolicyActors covers the callers who are not one of two members:
// a third member, an operator, an agent, anonymous, a disabled caller, and a
// same-name replacement of a removed one.
func TestDMPolicyActors(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	f.send(t, conv, f.alice, "between us")
	root, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec("UPDATE principals SET role = 'operator', owner = 1 WHERE id = ?", root.ID); err != nil {
		t.Fatal(err)
	}
	for _, who := range []struct {
		name string
		id   int64
	}{{"a third member", f.carol.ID}, {"an operator", root.ID}, {"an agent", f.agent.ID}, {"anonymous", 0}} {
		if _, err := f.s.DMConversationFor(ctx, who.id, conv.ID); !errors.Is(err, ErrDMNotFound) {
			t.Errorf("%s reads the pair: %v", who.name, err)
		}
		_, _, err := f.s.InsertDMMessage(ctx, DMPost{ConversationID: conv.ID, AuthorID: who.id, Body: "hi", ClientNonce: "x" + who.name}, f.tick())
		if err == nil {
			t.Errorf("%s wrote in the pair", who.name)
		}
	}
	for _, c := range []struct {
		name         string
		caller, peer int64
		want         error
	}{
		{"an agent caller", f.agent.ID, f.bob.ID, ErrDMNoStanding},
		{"an anonymous caller", 0, f.bob.ID, ErrDMNoStanding},
		{"an agent target", f.alice.ID, f.agent.ID, ErrDMNotHuman},
		{"an operator, a new pair", root.ID, f.bob.ID, nil},
	} {
		if _, err := f.s.OpenDM(ctx, c.caller, c.peer, f.tick()); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}

	// A disabled caller loses everything at once, and keeps it to resume.
	f.disable(t, f.alice)
	if _, err := f.s.DMConversationFor(ctx, f.alice.ID, conv.ID); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("a disabled caller reads: %v", err)
	}
	if err := f.trySend(conv, f.alice, "still here?"); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("a disabled caller sends: %v", err)
	}
	if _, err := f.s.OpenDM(ctx, f.alice.ID, f.bob.ID, f.tick()); !errors.Is(err, ErrDMNoStanding) {
		t.Errorf("a disabled caller opens: %v", err)
	}

	// Removed, then a new principal with the same name: the new one gets
	// none of the old one's conversations, and a new pair is a new id.
	f.hide(t, f.alice)
	again, err := f.s.CreatePrincipal(ctx, PrincipalHuman, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DMConversationFor(ctx, again.ID, conv.ID); !errors.Is(err, ErrDMNotFound) {
		t.Errorf("the same-name replacement reads the old pair: %v", err)
	}
	if list, err := f.s.ListDMConversations(ctx, again.ID, schema.DMStateOpen, DMListCursor{}, 10); err != nil || len(list) != 0 {
		t.Errorf("the same-name replacement lists %v, %v", list, err)
	}
	fresh, err := f.s.OpenDM(ctx, again.ID, f.bob.ID, f.tick())
	if err != nil || fresh.ID == conv.ID {
		t.Errorf("the same-name replacement opened %d (%v); the old pair is %d", fresh.ID, err, conv.ID)
	}
	// And bob still reads the old pair, with its author a former member.
	if got, err := f.s.DMConversationFor(ctx, f.bob.ID, conv.ID); err != nil || !got.PeerFormer {
		t.Errorf("bob reads the old pair: %+v, %v; want its peer former", got, err)
	}
}

// TestDMPrincipalNotFoundIsOneAnswer: a peer the caller may not know, a
// hidden one, an ended guest and an id never issued are one error, text and
// all, on a new pair and on a resume.
func TestDMPrincipalNotFoundIsOneAnswer(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.dave)
	f.send(t, conv, f.alice, "before")
	f.hideMembers(t)
	f.hide(t, f.carol)
	// dave is unknown to alice now (no channel), and bob was never known.
	answers := map[string]error{}
	for name, id := range map[string]int64{
		"never issued": 99999, "unknown on hide_members": f.bob.ID,
		"hidden": f.carol.ID, "unknown, an existing pair": f.dave.ID,
	} {
		_, err := f.s.OpenDM(ctx, f.alice.ID, id, f.tick())
		answers[name] = err
	}
	for name, err := range answers {
		if !errors.Is(err, ErrPrincipalNotFound) || err.Error() != ErrPrincipalNotFound.Error() {
			t.Errorf("%s: %v, want exactly %v", name, err, ErrPrincipalNotFound)
		}
	}
}

// TestDMContactRefusalNeverSaysWhy: every reason the target cannot be
// contacted is one error with one text, from either side of a block, so no
// answer tells which reason it was or who set a block.
func TestDMContactRefusalNeverSaysWhy(t *testing.T) {
	ctx := context.Background()
	reasons := map[string]func(t *testing.T, f *dmFixture){
		"bob blocks alice": func(t *testing.T, f *dmFixture) { f.block(t, f.bob, f.alice) },
		"alice blocks bob": func(t *testing.T, f *dmFixture) { f.block(t, f.alice, f.bob) },
		"bob accepts nobody": func(t *testing.T, f *dmFixture) {
			f.prefer(t, f.bob, schema.DMAcceptNobody)
		},
		"bob is switched off": func(t *testing.T, f *dmFixture) { f.switchOff(t, f.bob) },
	}
	texts := map[string]bool{}
	for name, setup := range reasons {
		f := newDMFixture(t)
		conv := f.openPair(t, f.alice, f.bob)
		f.send(t, conv, f.alice, "hi")
		setup(t, f)
		if err := f.trySend(conv, f.alice, "are you there"); !errors.Is(err, ErrDMContactUnavailable) {
			t.Errorf("%s: send: %v", name, err)
		} else {
			texts[err.Error()] = true
		}
		_, err := f.s.OpenDM(ctx, f.alice.ID, f.bob.ID, f.tick())
		if err != nil {
			t.Errorf("%s: resuming a visible conversation: %v", name, err)
		}
		// A new pair with the same reason, from the same side.
		g := newDMFixture(t)
		g.bob, g.carol = g.carol, g.bob
		setup(t, g)
		if _, err := g.s.OpenDM(ctx, g.alice.ID, g.bob.ID, g.tick()); !errors.Is(err, ErrDMContactUnavailable) {
			t.Errorf("%s: open a new pair: %v", name, err)
		} else {
			texts[err.Error()] = true
		}
	}
	// A disabled target is a send refusal like the rest, but a new pair is
	// not found (#2099), so it is outside the texts of the open below.
	{
		f := newDMFixture(t)
		conv := f.openPair(t, f.alice, f.bob)
		f.send(t, conv, f.alice, "hi")
		f.disable(t, f.bob)
		if err := f.trySend(conv, f.alice, "are you there"); !errors.Is(err, ErrDMContactUnavailable) {
			t.Errorf("bob is disabled: send: %v", err)
		} else {
			texts[err.Error()] = true
		}
	}
	if len(texts) != 1 || !texts[ErrDMContactUnavailable.Error()] {
		t.Errorf("refusal texts = %v, want only %q", texts, ErrDMContactUnavailable.Error())
	}

	// From the blocked side and the blocking side alike, sends are refused
	// with the same answer, and neither side's answer differs.
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	f.send(t, conv, f.alice, "hi")
	f.block(t, f.bob, f.alice)
	errAlice, errBob := f.trySend(conv, f.alice, "a"), f.trySend(conv, f.bob, "b")
	if !errors.Is(errAlice, ErrDMContactUnavailable) || errAlice.Error() != errBob.Error() {
		t.Errorf("blocked side %v, blocking side %v; want the same refusal", errAlice, errBob)
	}
}

// TestDMContactIsCheckedInTheWrite: the policy is read inside each write's
// transaction, so a change takes effect on the very next write, and undoing
// it restores contact.
func TestDMContactIsCheckedInTheWrite(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)
	conv := f.openPair(t, f.alice, f.bob)
	f.send(t, conv, f.alice, "one")
	f.block(t, f.bob, f.alice)
	if err := f.trySend(conv, f.alice, "two"); !errors.Is(err, ErrDMContactUnavailable) {
		t.Fatalf("send after a block: %v", err)
	}
	if _, err := f.s.DeleteBlock(ctx, f.bob.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.trySend(conv, f.alice, "three"); err != nil {
		t.Fatalf("send after the block is lifted: %v", err)
	}
	f.prefer(t, f.bob, schema.DMAcceptNobody)
	if err := f.trySend(conv, f.alice, "four"); !errors.Is(err, ErrDMContactUnavailable) {
		t.Fatalf("send after nobody: %v", err)
	}
	f.prefer(t, f.bob, schema.DMAcceptEveryone)
	f.serverOff(t)
	if err := f.trySend(conv, f.alice, "five"); !errors.Is(err, ErrDMsOff) {
		t.Fatalf("send with the server off: %v", err)
	}
	// The server switch answers before the conversation is looked up.
	if err := f.trySend(DMConversation{ID: 424242}, f.alice, "six"); !errors.Is(err, ErrDMsOff) {
		t.Fatalf("send to no conversation with the server off: %v", err)
	}
}

// TestDMPairReachable is the call policy's "revoked" row (design §4.1).
func TestDMPairReachable(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, f *dmFixture)
		want  bool
	}{
		{"two members", nil, true},
		{"a block", func(t *testing.T, f *dmFixture) { f.block(t, f.bob, f.alice) }, false},
		{"a disabled side", func(t *testing.T, f *dmFixture) { f.disable(t, f.alice) }, false},
		{"a switched-off side", func(t *testing.T, f *dmFixture) { f.switchOff(t, f.bob) }, false},
		{"the server off", func(t *testing.T, f *dmFixture) { f.serverOff(t) }, false},
		{"a preference does not end a call", func(t *testing.T, f *dmFixture) { f.prefer(t, f.bob, schema.DMAcceptNobody) }, true},
	} {
		f := newDMFixture(t)
		if c.setup != nil {
			c.setup(t, f)
		}
		for _, pair := range [][2]int64{{f.alice.ID, f.bob.ID}, {f.bob.ID, f.alice.ID}} {
			got, err := dmPairReachableTx(ctx, f.s.db, pair[0], pair[1])
			if err != nil || got != c.want {
				t.Errorf("%s %v: %v, %v; want %v", c.name, pair, got, err, c.want)
			}
		}
	}
}

// TestDMSettings: the preference, the per-person switch and the server switch
// as stored, who may write them, and what is audited.
func TestDMSettings(t *testing.T) {
	ctx := context.Background()
	f := newDMFixture(t)

	if p, err := f.s.DMPreferences(ctx, f.alice.ID); err != nil || p != schema.DefaultDMPreferences() {
		t.Errorf("default preference %+v, %v", p, err)
	}
	if _, err := f.s.SetDMPreferences(ctx, f.alice.ID, schema.DMPreferencesV1{Accept: "friends"}, f.tick()); err == nil {
		t.Error("a preference outside the vocabulary was stored")
	}
	if _, err := f.s.SetDMPreferences(ctx, f.agent.ID, schema.DMPreferencesV1{Accept: schema.DMAcceptNobody}, f.tick()); !errors.Is(err, ErrDMNoStanding) {
		t.Errorf("an agent's preference: %v", err)
	}
	audits := f.auditCount(t)
	f.prefer(t, f.alice, schema.DMAcceptSharedChannel)
	if p, _ := f.s.DMPreferences(ctx, f.alice.ID); p.Accept != schema.DMAcceptSharedChannel {
		t.Errorf("preference = %v", p.Accept)
	}
	if f.auditCount(t) != audits {
		t.Error("a person's own preference was audited")
	}

	if s, err := f.s.DMServerSettings(ctx); err != nil || !s.DMsEnabled {
		t.Errorf("the server switch starts %+v, %v; want on", s, err)
	}
	if on, err := f.s.DMsEnabledFor(ctx, f.alice.ID); err != nil || !on {
		t.Errorf("alice enabled = %v, %v", on, err)
	}

	// Members hold neither manage_members nor change_server_settings.
	if _, err := f.s.SetDMAllowed(ctx, principalActor(f.carol.ID), f.carol.ID, f.bob.ID, false, f.tick()); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("a member switches bob off: %v", err)
	}
	if _, err := f.s.SetDMServerSettings(ctx, principalActor(f.carol.ID), f.carol.ID, schema.DMServerSettingsV1{}, f.tick()); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("a member switches the server off: %v", err)
	}
	if _, err := f.s.SetDMAllowed(ctx, "system", 0, f.agent.ID, false, f.tick()); !errors.Is(err, ErrNotAPerson) {
		t.Errorf("an agent switched: %v", err)
	}
	if _, err := f.s.SetDMAllowed(ctx, "system", 0, 99999, false, f.tick()); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("nobody switched: %v", err)
	}

	audits = f.auditCount(t)
	for i, wantChanged := range []bool{true, false} {
		changed, err := f.s.SetDMAllowed(ctx, "system", 0, f.bob.ID, false, f.tick())
		if err != nil || changed != wantChanged {
			t.Errorf("switch bob off, pass %d: %v, %v", i, changed, err)
		}
	}
	var action, subject, detail string
	if err := f.s.db.QueryRow("SELECT action, subject, detail FROM audit_events ORDER BY id DESC LIMIT 1").Scan(&action, &subject, &detail); err != nil {
		t.Fatal(err)
	}
	if f.auditCount(t) != audits+1 || action != AuditDMPermissionChanged || subject != principalActor(f.bob.ID) || detail != "dm_allowed=false" {
		t.Errorf("the switch's audit: %d rows, %s %s %s", f.auditCount(t)-audits, action, subject, detail)
	}
	if on, _ := f.s.DMsEnabledFor(ctx, f.bob.ID); on {
		t.Error("bob is still enabled")
	}

	audits = f.auditCount(t)
	for i, wantChanged := range []bool{true, false} {
		changed, err := f.s.SetDMServerSettings(ctx, "system", 0, schema.DMServerSettingsV1{DMsEnabled: false}, f.tick())
		if err != nil || changed != wantChanged {
			t.Errorf("switch the server off, pass %d: %v, %v", i, changed, err)
		}
	}
	if err := f.s.db.QueryRow("SELECT action, subject, detail FROM audit_events ORDER BY id DESC LIMIT 1").Scan(&action, &subject, &detail); err != nil {
		t.Fatal(err)
	}
	if f.auditCount(t) != audits+1 || action != AuditDMSettingsChanged || subject != "server" || detail != "dms_enabled=false" {
		t.Errorf("the server switch's audit: %d rows, %s %s %s", f.auditCount(t)-audits, action, subject, detail)
	}
	if on, _ := f.s.DMsEnabledFor(ctx, f.alice.ID); on {
		t.Error("alice is enabled with the server off")
	}

	// A damaged server row refuses rather than allows.
	if _, err := f.s.db.Exec("UPDATE server_settings SET value = 'yes' WHERE key = ?", settingDMsEnabled); err != nil {
		t.Fatal(err)
	}
	if s, _ := f.s.DMServerSettings(ctx); s.DMsEnabled {
		t.Error("a damaged server switch reads as on")
	}
}
