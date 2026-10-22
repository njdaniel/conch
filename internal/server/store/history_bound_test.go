package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The guest history bound (issue #719, ADR-009, design §4.3): every read in
// audience.go intersects the audience rule with the reader's membership
// watermark. These tests seed markers on both sides of a guest's admission,
// channel-wide, on a net, in whispers, in threads, as files and as runs, and
// ask every function for them.

// boundFixture is the intervention world (general with alice, bob, dave,
// erin and frank, nets n1 and n2, the agent agt and its runs on two sessions,
// all from before) plus a guest admitted to general after it, seated on n1,
// and markers posted before and after the admission. The runs made after are
// on sess2, which takes interventions.
type boundFixture struct {
	*interventionWorld
	guest CreatedGuest
	// From before the admission.
	preWide, preRoot, preReply, preFileMsg, preWhisperToFrank int64
	preFile                                                   File
	preIntervention                                           int64
	// From after it.
	postWide, postNet, postWhisper, postRoot, postReply, oldThreadReply, guestPost, postFileMsg int64
	postFile, postUnattached                                                                    File
	postRun, postRunNoMessage, postActivity, postIntervention                                   int64
}

func newBoundFixture(t *testing.T) *boundFixture {
	t.Helper()
	ctx := context.Background()
	f := &boundFixture{interventionWorld: newInterventionWorld(t)}

	f.preWide = f.wide(t, f.frank, "marker before")
	f.preRoot = f.wide(t, f.frank, "old thread root")
	f.preReply = f.mustReplyID(t, f.alice, f.preRoot, "old thread reply")
	f.preFile = f.file(t, f.alice, "old.txt")
	if err := f.attachWide(f.alice, f.preFile.ID); err != nil {
		t.Fatal(err)
	}
	f.preFileMsg = f.lastID(t)
	f.preWhisperToFrank = f.whisper(t, f.erin, "old private marker", f.frank)
	iv, err := f.s.CreateIntervention(ctx, principalActor(f.alice.ID),
		InterventionParams{RunID: f.rWideMsg, Kind: schema.InterventionKindCancel}, Reader{PrincipalID: f.alice.ID, Scoped: true}, runT0)
	if err != nil {
		t.Fatal(err)
	}
	f.preIntervention = iv.ID

	if f.guest, err = f.s.CreateGuest(ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.ch.ID}, Idle: time.Hour, Absolute: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	f.put(t, "n1", f.guest.Principal, schema.NetRoleMember)

	f.postWide = f.wide(t, f.frank, "marker after")
	f.postNet, _ = f.toNet(t, f.alice, f.n1, "net marker after")
	f.postWhisper = f.whisper(t, f.erin, "private marker after", f.guest.Principal)
	f.postRoot = f.wide(t, f.frank, "new thread root")
	f.postReply = f.mustReplyID(t, f.alice, f.postRoot, "new thread reply")
	f.oldThreadReply = f.mustReplyID(t, f.alice, f.preRoot, "marker in an old thread")
	f.guestPost = f.wide(t, f.guest.Principal, "the guest speaks")
	f.postFile = f.file(t, f.alice, "new.txt")
	if err := f.attachWide(f.alice, f.postFile.ID); err != nil {
		t.Fatal(err)
	}
	f.postFileMsg = f.lastID(t)
	f.postUnattached = f.file(t, f.alice, "unattached.txt")

	f.postRunNoMessage = f.runOn2(t, 0, "after, on nothing")
	f.postRun = f.runOn2(t, f.postWide, "after, on a new message")
	f.postActivity = f.mustActivity(t, f.postRun, "working")
	iv, err = f.s.CreateIntervention(ctx, principalActor(f.alice.ID),
		InterventionParams{RunID: f.postRun, Kind: schema.InterventionKindCancel}, Reader{PrincipalID: f.alice.ID, Scoped: true}, runT0)
	if err != nil {
		t.Fatal(err)
	}
	f.postIntervention = iv.ID
	return f
}

// runOn2 starts a run in general on sess2.
func (f *boundFixture) runOn2(t *testing.T, messageID int64, task string) int64 {
	t.Helper()
	r, err := f.s.CreateRun(context.Background(), principalActor(f.agt.ID), RunParams{
		SessionID: f.sess2.ID, CredentialID: f.agtCred, ChannelID: f.ch.ID, MessageID: messageID, Task: task,
	}, f.agtReader(), runT0)
	if err != nil {
		t.Fatalf("CreateRun(%s): %v", task, err)
	}
	return r.ID
}

func (f *boundFixture) mustReplyID(t *testing.T, author Principal, root int64, body string) int64 {
	t.Helper()
	m, _ := f.mustReply(t, author, root, body)
	return m.ID
}

// lastID is the newest message of general, read the way a member reads it.
func (f *boundFixture) lastID(t *testing.T) int64 {
	t.Helper()
	msgs, err := f.s.ListVisibleMessagesPage(context.Background(), f.ch.ID, Reader{PrincipalID: f.alice.ID, Scoped: true},
		MessagePage{NewestFirst: true}, 1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("newest message: %v, %v", msgs, err)
	}
	return msgs[0].ID
}

func (f *boundFixture) guestReader(scoped bool) Reader {
	return Reader{PrincipalID: f.guest.Principal.ID, Scoped: scoped}
}

// ghost is an id no object has.
const ghost = 1 << 40

// Every message read gives the guest exactly the messages after its bound
// that its audience allows, on v2 and on v0/v1, and a member exactly what it
// got before guests existed.
func TestHistoryBoundMessageReads(t *testing.T) {
	ctx := context.Background()
	f := newBoundFixture(t)
	afterV2 := []int64{f.postWide, f.postNet, f.postWhisper, f.postRoot, f.postReply, f.guestPost, f.postFileMsg}
	afterV1 := []int64{f.postWide, f.postRoot, f.postReply, f.guestPost, f.postFileMsg}
	tests := []struct {
		name    string
		reader  Reader
		want    []int64
		present []int64 // must be in the list (a member's pre-admission view)
		absent  []int64
	}{
		{"guest (v2)", f.guestReader(true), afterV2, nil, nil},
		{"guest (v0/v1)", f.guestReader(false), afterV1, nil, nil},
		{"member alice (v2)", Reader{f.alice.ID, true}, nil,
			[]int64{f.preWide, f.preRoot, f.preReply, f.preFileMsg, f.netMsg, f.postWide, f.postNet, f.oldThreadReply, f.guestPost},
			[]int64{f.postWhisper, f.preWhisperToFrank}},
		{"member frank (v2)", Reader{f.frank.ID, true}, nil,
			[]int64{f.preWide, f.preWhisperToFrank, f.oldThreadReply}, []int64{f.postWhisper, f.netMsg}},
		{"member erin (v1)", Reader{f.erin.ID, false}, nil,
			[]int64{f.preWide, f.preReply, f.oldThreadReply}, []int64{f.postWhisper, f.preWhisperToFrank}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := visibleIDs(t, f.s, f.ch.ID, tt.reader)
			if tt.want != nil && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ListVisibleMessages = %v, want %v", got, tt.want)
			}
			for _, id := range tt.present {
				if !slices.Contains(got, id) {
					t.Errorf("ListVisibleMessages lost %d: %v", id, got)
				}
			}
			for _, id := range tt.absent {
				if slices.Contains(got, id) {
					t.Errorf("ListVisibleMessages has %d: %v", id, got)
				}
			}
			// Newest first is the same set backwards.
			desc, err := f.s.ListVisibleMessagesPage(ctx, f.ch.ID, tt.reader, MessagePage{NewestFirst: true}, 100)
			if err != nil {
				t.Fatal(err)
			}
			rev := messageIDs(desc)
			slices.Reverse(rev)
			if !reflect.DeepEqual(rev, got) {
				t.Errorf("newest first = %v, ascending %v", messageIDs(desc), got)
			}
			// The unread count counts what the list shows, from any cursor.
			for _, after := range []int64{0, f.preWide, f.postWide} {
				n, err := f.s.CountVisibleMessagesAfter(ctx, f.ch.ID, tt.reader, after, 100)
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				for _, id := range got {
					if id > after {
						want++
					}
				}
				if n != want {
					t.Errorf("CountVisibleMessagesAfter(%d) = %d, want %d", after, n, want)
				}
			}
			// A search for a word every marker holds finds the visible ones.
			found, err := f.s.SearchVisibleMessages(ctx, f.ch.ID, tt.reader, []string{"marker"}, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range found {
				if !slices.Contains(got, m.ID) {
					t.Errorf("search found %d, which the list does not show", m.ID)
				}
			}
		})
	}

	// The guest's searches, exactly.
	for _, scoped := range []bool{true, false} {
		found, err := f.s.SearchVisibleMessages(ctx, f.ch.ID, f.guestReader(scoped), []string{"marker"}, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		want := []int64{f.postWide}
		if scoped {
			want = []int64{f.postWhisper, f.postNet, f.postWide}
		}
		if got := messageIDs(found); !reflect.DeepEqual(got, want) {
			t.Errorf("guest (scoped %v) search = %v, want %v", scoped, got, want)
		}
	}
}

// A pre-bound thread root, reply, file, run, activity or intervention is
// answered as one that does not exist, whichever function is asked, and the
// same objects after the bound are served.
func TestHistoryBoundDirectReads(t *testing.T) {
	ctx := context.Background()
	f := newBoundFixture(t)
	g := f.guestReader(true)
	preActivity := f.mustActivity(t, f.rWideMsg, "old work")

	// err returns the error a read of id gives the guest, nil when served.
	reads := []struct {
		name string
		read func(id int64) error
		pre  []int64
		post []int64
	}{
		{"ReplyRootMessage", func(id int64) error {
			_, err := f.s.ReplyRootMessage(ctx, f.ch.ID, g, id)
			return err
		}, []int64{f.preRoot, f.preWide}, []int64{f.postRoot, f.postWide}},
		{"ListThreadMessages", func(id int64) error {
			_, err := f.s.ListThreadMessages(ctx, f.ch.ID, g, id, 0, 10)
			return err
		}, []int64{f.preRoot}, []int64{f.postRoot}},
		{"FileVisibleTo", func(id int64) error {
			_, err := f.s.FileVisibleTo(ctx, id, g)
			return err
		}, []int64{f.preFile.ID, f.postUnattached.ID}, []int64{f.postFile.ID}},
		{"RunVisible", func(id int64) error {
			_, err := f.s.RunVisible(ctx, id, g)
			return err
		}, []int64{f.runWide, f.runWideMsg, f.runNet, f.runWhisper, f.rWide, f.rWideMsg, f.postRunNoMessage}, []int64{f.postRun}},
		{"RunAnchorAudience", func(id int64) error {
			_, err := f.s.RunAnchorAudience(ctx, f.ch.ID, id, g)
			return err
		}, []int64{f.preWide, f.netMsg, f.preReply, f.oldThreadReply}, []int64{f.postWide, f.postNet}},
		{"ListVisibleActivity", func(id int64) error {
			_, err := f.s.ListVisibleActivity(ctx, id, g, 0, 10)
			return err
		}, []int64{f.rWideMsg, f.postRunNoMessage}, []int64{f.postRun}},
		{"ListVisibleInterventions", func(id int64) error {
			_, err := f.s.ListVisibleInterventions(ctx, id, g, 0, 10)
			return err
		}, []int64{f.rWideMsg}, []int64{f.postRun}},
		{"InterventionVisible", func(id int64) error {
			_, err := f.s.InterventionVisible(ctx, id, g)
			return err
		}, []int64{f.preIntervention}, []int64{f.postIntervention}},
	}
	for _, rd := range reads {
		t.Run(rd.name, func(t *testing.T) {
			want := rd.read(ghost)
			if want == nil {
				t.Fatalf("a ghost was served")
			}
			for _, id := range rd.pre {
				if err := rd.read(id); !errors.Is(err, want) || err.Error() != want.Error() {
					t.Errorf("pre-bound %d: %v, want the ghost's %v", id, err, want)
				}
			}
			for _, id := range rd.post {
				if err := rd.read(id); err != nil {
					t.Errorf("post-bound %d: %v", id, err)
				}
			}
		})
	}

	// The thread after the bound is the root and its reply; an old thread's
	// new reply is not a way into it.
	thread, err := f.s.ListThreadMessages(ctx, f.ch.ID, g, f.postRoot, 0, 10)
	if err != nil || !reflect.DeepEqual(messageIDs(thread), []int64{f.postRoot, f.postReply}) {
		t.Errorf("new thread = %v, %v", messageIDs(thread), err)
	}
	// The guest cannot reply to an old root, and nothing is stored.
	before := f.count(t, "messages")
	if _, _, err := f.s.InsertReplyMessage(ctx, ReplyPost{ChannelID: f.ch.ID, AuthorID: f.guest.Principal.ID, RootID: f.preRoot, Body: "hi"}, g); !errors.Is(err, ErrThreadNotFound) {
		t.Errorf("reply to an old root = %v, want ErrThreadNotFound", err)
	}
	if after := f.count(t, "messages"); after != before {
		t.Errorf("a refused reply stored %d messages", after-before)
	}
	if _, _, err := f.s.InsertReplyMessage(ctx, ReplyPost{ChannelID: f.ch.ID, AuthorID: f.guest.Principal.ID, RootID: f.postRoot, Body: "hi"}, g); err != nil {
		t.Errorf("reply to a new root = %v", err)
	}
	// Nor start an intervention on a run from before.
	if _, err := f.s.CreateIntervention(ctx, principalActor(f.guest.Principal.ID),
		InterventionParams{RunID: f.rWideMsg, Kind: schema.InterventionKindCancel}, g, runT0); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("intervention on an old run = %v, want ErrRunNotFound", err)
	}

	// Listings: only what is after the bound, and the activity of a visible
	// run is the entry made after.
	if got := visibleRunIDs(t, f.s, f.ch.ID, g); !reflect.DeepEqual(got, []int64{f.postRun}) {
		t.Errorf("guest runs = %v, want [%d]", got, f.postRun)
	}
	acts, err := f.s.ListVisibleActivity(ctx, f.postRun, g, 0, 10)
	if err != nil || len(acts) != 1 || acts[0].ID != f.postActivity {
		t.Errorf("activity = %+v, %v", acts, err)
	}
	ivs, err := f.s.ListVisibleInterventions(ctx, f.postRun, g, 0, 10)
	if err != nil || len(ivs) != 1 || ivs[0].ID != f.postIntervention {
		t.Errorf("interventions = %+v, %v", ivs, err)
	}
	views, err := f.s.SessionRunViews(ctx, []int64{f.sess2.ID}, g)
	if err != nil {
		t.Fatal(err)
	}
	if v := views[f.sess2.ID]; v.CurrentRun != f.postRun || v.Task != "after, on a new message" {
		t.Errorf("guest's view of the session = %+v", v)
	}

	// A member sees the runs, files and interventions from before, as ever.
	alice := Reader{PrincipalID: f.alice.ID, Scoped: true}
	if _, err := f.s.FileVisibleTo(ctx, f.preFile.ID, alice); err != nil {
		t.Errorf("alice's old file: %v", err)
	}
	if _, err := f.s.FileVisibleTo(ctx, f.postUnattached.ID, alice); err != nil {
		t.Errorf("alice's own unattached file: %v", err)
	}
	for _, id := range []int64{f.runWide, f.runWideMsg, f.runNet, f.postRunNoMessage} {
		if _, err := f.s.RunVisible(ctx, id, alice); err != nil {
			t.Errorf("alice's run %d: %v", id, err)
		}
	}
	if _, err := f.s.InterventionVisible(ctx, f.preIntervention, alice); err != nil {
		t.Errorf("alice's old intervention: %v", err)
	}
	if acts, err := f.s.ListVisibleActivity(ctx, f.rWideMsg, alice, 0, 10); err != nil || len(acts) != 1 || acts[0].ID != preActivity {
		t.Errorf("alice's old activity = %+v, %v", acts, err)
	}
}

// A read mark cannot be moved onto, or count, anything before the bound.
func TestHistoryBoundReadMark(t *testing.T) {
	ctx := context.Background()
	f := newBoundFixture(t)
	g := f.guestReader(true)
	newest := f.lastID(t)
	mark, err := f.s.MarkRead(ctx, f.ch.ID, g, newest+1000, runT0)
	if err != nil {
		t.Fatal(err)
	}
	if mark.LastReadID != f.postFileMsg {
		t.Errorf("mark = %d, want the guest's newest visible message %d", mark.LastReadID, f.postFileMsg)
	}
	n, err := f.s.CountVisibleMessagesAfter(ctx, f.ch.ID, g, 0, 100)
	if err != nil || n != 7 {
		t.Errorf("unread from 0 = %d, %v; want the 7 after the bound", n, err)
	}
}

// Default-deny: a guest whose membership has no bound (its provenance
// cannot be shown) is given nothing of the channel, not everything.
func TestHistoryBoundDefaultDeny(t *testing.T) {
	ctx := context.Background()
	f := newBoundFixture(t)
	if _, err := f.s.db.Exec("UPDATE channel_members SET history_after = NULL WHERE principal_id = ?", f.guest.Principal.ID); err != nil {
		t.Fatal(err)
	}
	for _, scoped := range []bool{true, false} {
		g := f.guestReader(scoped)
		if got := visibleIDs(t, f.s, f.ch.ID, g); len(got) != 0 {
			t.Errorf("scoped %v: messages %v, want none", scoped, got)
		}
		if n, err := f.s.CountVisibleMessagesAfter(ctx, f.ch.ID, g, 0, 100); err != nil || n != 0 {
			t.Errorf("scoped %v: count %d, %v", scoped, n, err)
		}
		if _, err := f.s.FileVisibleTo(ctx, f.postFile.ID, g); !errors.Is(err, ErrNotFound) {
			t.Errorf("scoped %v: file = %v", scoped, err)
		}
		if got := visibleRunIDs(t, f.s, f.ch.ID, g); len(got) != 0 {
			t.Errorf("scoped %v: runs %v", scoped, got)
		}
	}
	bounded, err := f.s.HistoryBounded(ctx, f.ch.ID, f.guest.Principal.ID)
	if err != nil || !bounded {
		t.Errorf("HistoryBounded = %v, %v; a guest is bounded whatever its membership says", bounded, err)
	}
}

// Conversion keeps the bounds; a membership an operator gives the member
// afterwards is an ordinary one. Removal and re-admission of a guest gives
// a fresh bound. Ending the visit takes its memberships and read marks, and
// a new visit reaches nothing of the old one's.
func TestHistoryBoundLifecycle(t *testing.T) {
	ctx := context.Background()

	t.Run("conversion keeps the bound", func(t *testing.T) {
		f := newBoundFixture(t)
		before := visibleIDs(t, f.s, f.ch.ID, f.guestReader(true))
		if _, err := f.s.ConvertGuest(ctx, "system", GuestConversion{PrincipalID: f.guest.Principal.ID, Name: "patricia",
			CredentialLabel: "member", CredentialLifetime: time.Hour}); err != nil {
			t.Fatal(err)
		}
		if got := visibleIDs(t, f.s, f.ch.ID, f.guestReader(true)); !reflect.DeepEqual(got, before) {
			t.Errorf("after conversion = %v, before %v", got, before)
		}
		if _, err := f.s.FileVisibleTo(ctx, f.preFile.ID, f.guestReader(true)); !errors.Is(err, ErrNotFound) {
			t.Errorf("old file after conversion = %v", err)
		}
		if _, err := f.s.RunVisible(ctx, f.rWideMsg, f.guestReader(true)); !errors.Is(err, ErrRunNotFound) {
			t.Errorf("old run after conversion = %v", err)
		}
		bounded, err := f.s.HistoryBounded(ctx, f.ch.ID, f.guest.Principal.ID)
		if err != nil || !bounded {
			t.Errorf("HistoryBounded in general = %v, %v", bounded, err)
		}
		// A channel the member is added to later, by an operator's grant, has
		// no bound: the member reads it as any member does.
		if _, err := f.s.AddChannelMember(ctx, "system", f.other.ID, f.guest.Principal.ID, 0); err != nil {
			t.Fatal(err)
		}
		if got := visibleRunIDs(t, f.s, f.other.ID, f.guestReader(true)); !reflect.DeepEqual(got, []int64{f.runElse, f.rElse}) {
			t.Errorf("runs of other = %v, want [%d %d]", got, f.runElse, f.rElse)
		}
		if bounded, err := f.s.HistoryBounded(ctx, f.other.ID, f.guest.Principal.ID); err != nil || bounded {
			t.Errorf("HistoryBounded in other = %v, %v", bounded, err)
		}
	})

	t.Run("re-admission has a fresh bound", func(t *testing.T) {
		f := newBoundFixture(t)
		if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.guest.Principal.ID); err != nil {
			t.Fatal(err)
		}
		between := f.wide(t, f.frank, "said while the guest was out")
		if _, err := f.s.PutChannelMember(ctx, "system", f.ch.ID, f.guest.Principal.ID, 0, nil); err != nil {
			t.Fatal(err)
		}
		later := f.wide(t, f.frank, "said after it came back")
		if got := visibleIDs(t, f.s, f.ch.ID, f.guestReader(true)); !reflect.DeepEqual(got, []int64{later}) {
			t.Errorf("after re-admission = %v, want [%d] (not %d nor anything before)", got, later, between)
		}
	})

	t.Run("an ended visit leaves nothing for the next", func(t *testing.T) {
		f := newBoundFixture(t)
		if _, err := f.s.MarkRead(ctx, f.ch.ID, f.guestReader(true), f.postWide, runT0); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.EndGuestVisit(ctx, "system", f.guest.Principal.ID, schema.GuestEndVisit); err != nil {
			t.Fatal(err)
		}
		var n int
		for _, q := range []string{"SELECT COUNT(*) FROM channel_members WHERE principal_id = ?",
			"SELECT COUNT(*) FROM read_marks WHERE principal_id = ?",
			"SELECT COUNT(*) FROM net_members WHERE principal_id = ?"} {
			if err := f.s.db.QueryRow(q, f.guest.Principal.ID).Scan(&n); err != nil || n != 0 {
				t.Errorf("%s = %d, %v; want 0", q, n, err)
			}
		}
		next, err := f.s.CreateGuest(ctx, "system", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
			Channels: []int64{f.ch.ID}, Idle: time.Hour, Absolute: 24 * time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if next.Principal.ID == f.guest.Principal.ID {
			t.Fatal("the new visit has the old principal")
		}
		f.put(t, "n1", next.Principal, schema.NetRoleMember)
		r := Reader{PrincipalID: next.Principal.ID, Scoped: true}
		if got := visibleIDs(t, f.s, f.ch.ID, r); len(got) != 0 {
			t.Errorf("the new visit sees %v", got)
		}
		for _, id := range []int64{f.postWhisper, f.guestPost, f.postRoot} {
			if _, err := f.s.ReplyRootMessage(ctx, f.ch.ID, r, id); !errors.Is(err, ErrThreadNotFound) {
				t.Errorf("the new visit reaches %d: %v", id, err)
			}
		}
		marks, err := f.s.ReadMarksFor(ctx, next.Principal.ID)
		if err != nil || len(marks) != 0 {
			t.Errorf("the new visit's marks = %v, %v", marks, err)
		}
		// Members still read the old visit's posts, under its attribution.
		msgs, err := f.s.ListVisibleMessages(ctx, f.ch.ID, Reader{PrincipalID: f.alice.ID, Scoped: true}, f.guestPost-1, 1)
		if err != nil || len(msgs) != 1 || msgs[0].GuestAuthor == nil || msgs[0].GuestAuthor.Handle != f.guest.Visit.Handle {
			t.Errorf("the old guest's post to a member = %+v, %v", msgs, err)
		}
	})
}

// HistoryBounded is true for a guest and for a membership with a bound,
// false for every other member and for a reader outside the channel.
func TestHistoryBounded(t *testing.T) {
	ctx := context.Background()
	f := newBoundFixture(t)
	tests := []struct {
		name      string
		channel   int64
		principal int64
		want      bool
	}{
		{"guest in its channel", f.ch.ID, f.guest.Principal.ID, true},
		{"guest outside its channels", f.other.ID, f.guest.Principal.ID, true},
		{"member", f.ch.ID, f.alice.ID, false},
		{"agent", f.ch.ID, f.agt.ID, false},
		{"non-member", f.ch.ID, f.carol.ID, false},
		{"nobody", f.ch.ID, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.s.HistoryBounded(ctx, tt.channel, tt.principal)
			if err != nil || got != tt.want {
				t.Errorf("HistoryBounded = %v, %v; want %v", got, err, tt.want)
			}
		})
	}
}
