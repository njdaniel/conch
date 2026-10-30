package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Message reactions in the store (issue #765, docs/design/message-reactions.md
// §14). The world is the history-bound fixture: general with alice, bob, dave,
// erin and frank, nets n1 (alice and dave members, bob a monitor, the guest a
// member) and n2 (alice and erin), an agent, and a guest admitted after the
// pre* markers were posted.

const reactionsMigration = 37

func rdr(p Principal) Reader { return Reader{PrincipalID: p.ID, Scoped: true} }

var reactionNow = func() time.Time { return time.Now() }

// rkey canonicalizes s, failing the test for a string that is not an emoji.
func rkey(t *testing.T, s string) string {
	t.Helper()
	k, err := schema.CanonicalReactionKey(s)
	if err != nil {
		t.Fatalf("%q is not an emoji: %v", s, err)
	}
	return k
}

var (
	thumbs = "\U0001F44D"
	heart  = "❤️"
	party  = "\U0001F389"
)

// manyEmoji are 25 distinct emoji.
var manyEmoji = []string{
	"😀", "😃", "😄", "😁", "😆", "😅", "😂", "🤣", "😊", "😇", "🙂", "🙃", "😉", "😌", "😍", "🥰", "😘", "😗", "😙", "😚",
	"😋", "😛", "😝", "😜", "🤪",
}

func (f *boundFixture) set(t *testing.T, ch int64, who Principal, msg int64, key string) ReactionChange {
	t.Helper()
	c, err := f.s.SetReaction(context.Background(), ch, rdr(who), msg, rkey(t, key), reactionNow)
	if err != nil {
		t.Fatalf("SetReaction(%d, %q): %v", msg, key, err)
	}
	return c
}

func (f *boundFixture) summary(t *testing.T, who Principal, msg int64) ReactionSummary {
	t.Helper()
	s, err := f.s.ReactionSummaryOf(context.Background(), f.ch.ID, rdr(who), msg, reactionNow)
	if err != nil {
		t.Fatalf("ReactionSummaryOf(%d) as %d: %v", msg, who.ID, err)
	}
	return s
}

func counts(s ReactionSummary) map[string]int {
	m := map[string]int{}
	for _, r := range s.Reactions {
		m[r.Emoji] = r.Count
	}
	return m
}

func reactionRows(t *testing.T, s *Store) int {
	t.Helper()
	return countRows(t, s, "message_reactions")
}

// W1 and W5: set, set again, remove, remove again, and a remove by someone who
// never reacted. One row after the first set, none after the first remove, two
// audit rows in all, and the summary's me follows.
func TestSetAndRemoveReaction(t *testing.T) {
	f := newBoundFixture(t)
	msg := f.postWide
	steps := []struct {
		name        string
		set         bool
		who         Principal
		wantChanged bool
		wantRows    int
		wantMe      bool
		wantCount   int
	}{
		{"first set", true, f.alice, true, 1, true, 1},
		{"set again", true, f.alice, false, 1, true, 1},
		{"another person joins the key", true, f.dave, true, 2, true, 2},
		{"remove", false, f.alice, true, 1, false, 1},
		{"remove again", false, f.alice, false, 1, false, 1},
		{"remove by someone who never reacted", false, f.frank, false, 1, false, 1},
	}
	for _, st := range steps {
		var c ReactionChange
		var err error
		if st.set {
			c, err = f.s.SetReaction(context.Background(), f.ch.ID, rdr(st.who), msg, rkey(t, thumbs), reactionNow)
		} else {
			c, err = f.s.RemoveReaction(context.Background(), f.ch.ID, rdr(st.who), msg, rkey(t, thumbs), reactionNow)
		}
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if c.Changed != st.wantChanged {
			t.Errorf("%s: changed = %v, want %v", st.name, c.Changed, st.wantChanged)
		}
		if got := reactionRows(t, f.s); got != st.wantRows {
			t.Errorf("%s: %d rows, want %d", st.name, got, st.wantRows)
		}
		var me bool
		var n int
		for _, r := range c.Summary.Reactions {
			me, n = r.Me, r.Count
		}
		if me != st.wantMe || n != st.wantCount {
			t.Errorf("%s: summary me=%v count=%d, want me=%v count=%d (%+v)", st.name, me, n, st.wantMe, st.wantCount, c.Summary)
		}
	}
	// alice set and removed, dave set: three changes, three audit rows. The
	// repeats and the stranger's remove wrote none (A1).
	if got := auditCount(t, f.s, AuditReactionSet); got != 2 {
		t.Errorf("%d reaction_set audit rows, want 2", got)
	}
	if got := auditCount(t, f.s, AuditReactionRemoved); got != 1 {
		t.Errorf("%d reaction_removed audit rows, want 1", got)
	}
	evs, err := f.s.ListAuditEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Action == AuditReactionSet || e.Action == AuditReactionRemoved {
			if e.Subject != fmt.Sprintf("message:%d", msg) || !strings.HasPrefix(e.Detail, fmt.Sprintf("channel=%d emoji=", f.ch.ID)) || strings.Contains(e.Detail, "marker") {
				t.Errorf("audit row %+v: want subject message:%d and a detail of channel and emoji only", e, msg)
			}
			if !strings.HasPrefix(e.Actor, "principal:") {
				t.Errorf("audit actor = %q", e.Actor)
			}
		}
	}
}

// W2: fifty concurrent sets of one (message, principal, emoji) store one row
// and every caller gets the same answer, no error.
func TestConcurrentSetsStoreOneRow(t *testing.T) {
	f := newBoundFixture(t)
	const n = 50
	var wg sync.WaitGroup
	got := make([]ReactionChange, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = f.s.SetReaction(context.Background(), f.ch.ID, rdr(f.alice), f.postWide, rkey(t, thumbs), reactionNow)
		}()
	}
	wg.Wait()
	changed := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("set %d: %v", i, errs[i])
		}
		if got[i].Changed {
			changed++
		}
		if !reflect.DeepEqual(counts(got[i].Summary), map[string]int{rkey(t, thumbs): 1}) {
			t.Errorf("set %d answered %+v", i, got[i].Summary)
		}
	}
	if changed != 1 || reactionRows(t, f.s) != 1 || auditCount(t, f.s, AuditReactionSet) != 1 {
		t.Errorf("%d sets changed a row, %d rows, %d audit rows; want one each", changed, reactionRows(t, f.s), auditCount(t, f.s, AuditReactionSet))
	}
}

// W4: the limits. 20 distinct emoji on a message, 10 reactions by one person,
// and joining an existing key at the kinds limit.
func TestReactionLimits(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	msg := f.postWide
	for i, e := range manyEmoji[:10] {
		f.set(t, f.ch.ID, f.alice, msg, e)
		_ = i
	}
	// An 11th by alice is refused and stores nothing.
	if _, err := f.s.SetReaction(ctx, f.ch.ID, rdr(f.alice), msg, rkey(t, manyEmoji[10]), reactionNow); !errors.Is(err, ErrTooManyReactions) {
		t.Errorf("11th reaction by one person = %v, want ErrTooManyReactions", err)
	}
	for _, e := range manyEmoji[10:20] {
		f.set(t, f.ch.ID, f.bob, msg, e)
	}
	rows := reactionRows(t, f.s)
	if rows != 20 {
		t.Fatalf("%d rows, want 20", rows)
	}
	// A 21st kind is refused, by someone with room of their own.
	if _, err := f.s.SetReaction(ctx, f.ch.ID, rdr(f.dave), msg, rkey(t, manyEmoji[20]), reactionNow); !errors.Is(err, ErrTooManyReactions) {
		t.Errorf("21st kind = %v, want ErrTooManyReactions", err)
	}
	if got := reactionRows(t, f.s); got != rows {
		t.Errorf("a refused set left %d rows, had %d", got, rows)
	}
	// Joining a kind already on the message is allowed at the limit.
	c, err := f.s.SetReaction(ctx, f.ch.ID, rdr(f.dave), msg, rkey(t, manyEmoji[0]), reactionNow)
	if err != nil || !c.Changed {
		t.Errorf("joining an existing kind at the limit = %+v, %v", c, err)
	}
	if got := auditCount(t, f.s, AuditReactionSet); got != 21 {
		t.Errorf("%d reaction_set rows, want 21 (the two refusals wrote none)", got)
	}
}

// V2 to V6, hidden, private, net and whisper messages: every kind of reader
// against every kind of message, for the summary, the reactors and the batch
// read, and for a set. A message the reader cannot see is ErrMessageNotFound,
// the same error as one that does not exist.
func TestReactionVisibility(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	netMsg := f.postNet      // alice's post to n1: alice, bob (monitor), dave, the guest
	whisper := f.postWhisper // erin to the guest
	replyToWhisper := f.mustReplyID(t, f.erin, whisper, "reply to the whisper")
	wide := f.postWide
	// A principal taken off n1 after the post still sees it.
	if _, err := f.s.RemoveNetMember(ctx, "system", f.ch.ID, "n1", f.dave.ID); err != nil {
		t.Fatal(err)
	}

	type want struct{ see, set bool }
	tests := []struct {
		name string
		msg  int64
		who  Principal
		want want
	}{
		{"wide: a member", wide, f.frank, want{true, true}},
		{"wide: the agent", wide, f.agt, want{true, true}},
		{"net: a member on the net", netMsg, f.alice, want{true, true}},
		{"net: a monitor reads, may not set", netMsg, f.bob, want{true, false}},
		{"net: a member taken off since the post reads, may not set", netMsg, f.dave, want{true, false}},
		{"net: on another net", netMsg, f.erin, want{false, false}},
		{"net: on no net", netMsg, f.frank, want{false, false}},
		{"whisper: the recipient", whisper, f.guest.Principal, want{true, true}},
		{"whisper: the author", whisper, f.erin, want{true, true}},
		{"whisper: another member", whisper, f.frank, want{false, false}},
		{"whisper: a net member", whisper, f.alice, want{false, false}},
		{"reply to a whisper follows the root: recipient", replyToWhisper, f.guest.Principal, want{true, true}},
		{"reply to a whisper follows the root: stranger", replyToWhisper, f.frank, want{false, false}},
		{"ghost id", 1 << 40, f.alice, want{false, false}},
		{"before the guest's bound", f.preWide, f.guest.Principal, want{false, false}},
		{"a reply in an old thread, after the bound", f.oldThreadReply, f.guest.Principal, want{false, false}},
		{"a non-member of the channel", wide, f.carol, want{false, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Whoever can set once sets, so there is something to find.
			// Other readers, seeded below, put a reaction on every message.
			_, sumErr := f.s.ReactionSummaryOf(ctx, f.ch.ID, rdr(tt.who), tt.msg, reactionNow)
			_, listErr := f.s.ListReactorsOf(ctx, f.ch.ID, rdr(tt.who), tt.msg, rkey(t, thumbs), 0, 10)
			batch, batchErr := f.s.ReactionSummariesOf(ctx, f.ch.ID, rdr(tt.who), []int64{tt.msg}, reactionNow)
			_, setErr := f.s.SetReaction(ctx, f.ch.ID, rdr(tt.who), tt.msg, rkey(t, party), reactionNow)
			_, rmErr := f.s.RemoveReaction(ctx, f.ch.ID, rdr(tt.who), tt.msg, rkey(t, party), reactionNow)
			if tt.who.ID == f.carol.ID {
				for name, err := range map[string]error{"summary": sumErr, "list": listErr, "batch": batchErr, "set": setErr, "remove": rmErr} {
					if !errors.Is(err, ErrNotChannelMember) {
						t.Errorf("%s as a non-member = %v, want ErrNotChannelMember", name, err)
					}
				}
				return
			}
			if tt.want.see {
				for name, err := range map[string]error{"summary": sumErr, "list": listErr, "batch": batchErr, "remove": rmErr} {
					if err != nil {
						t.Errorf("%s = %v, want success", name, err)
					}
				}
				if tt.want.set && setErr != nil {
					t.Errorf("set = %v, want success", setErr)
				}
				if !tt.want.set && !errors.Is(setErr, ErrReactionNotPermitted) {
					t.Errorf("set = %v, want ErrReactionNotPermitted", setErr)
				}
				return
			}
			for name, err := range map[string]error{"summary": sumErr, "list": listErr, "set": setErr, "remove": rmErr} {
				if !errors.Is(err, ErrMessageNotFound) {
					t.Errorf("%s = %v, want ErrMessageNotFound", name, err)
				}
			}
			if batchErr != nil || len(batch) != 0 {
				t.Errorf("batch = %v, %v, want it to omit the message", batch, batchErr)
			}
		})
	}

	// A refused or invisible set stored nothing, and wrote no audit row: only
	// the sets that were allowed did.
	party := rkey(t, "🎉")
	var n int
	if err := f.s.db.QueryRow("SELECT COUNT(*) FROM message_reactions WHERE emoji = ? AND message_id = ?", party, f.preWide).Scan(&n); err != nil || n != 0 {
		t.Errorf("a reaction was stored on a message the guest cannot see: %d, %v", n, err)
	}
}

// Seeding every visible message with a reaction from someone who can see it,
// the readers who cannot see it see neither the count nor the reactor, in the
// summary, the list or the batch, and cannot tell it from a ghost.
func TestReactionsLeakNothing(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	whisper := f.postWhisper
	netMsg := f.postNet
	f.set(t, f.ch.ID, f.erin, whisper, "👀")
	f.set(t, f.ch.ID, f.alice, netMsg, "👀")
	f.set(t, f.ch.ID, f.frank, f.postWide, "👀")
	for _, tt := range []struct {
		who  Principal
		msg  int64
		name string
	}{{f.frank, whisper, "frank / whisper"}, {f.alice, whisper, "alice / whisper"}, {f.frank, netMsg, "frank / net message"}, {f.erin, netMsg, "erin / net message"}} {
		_, err := f.s.ReactionSummaryOf(ctx, f.ch.ID, rdr(tt.who), tt.msg, reactionNow)
		_, ghostErr := f.s.ReactionSummaryOf(ctx, f.ch.ID, rdr(tt.who), 1<<40, reactionNow)
		if !errors.Is(err, ErrMessageNotFound) || err.Error() != ghostErr.Error() {
			t.Errorf("%s: %v, a ghost gives %v", tt.name, err, ghostErr)
		}
		batch, err := f.s.ReactionSummariesOf(ctx, f.ch.ID, rdr(tt.who), []int64{tt.msg, f.postWide}, reactionNow)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range batch {
			if s.MessageID == tt.msg {
				t.Errorf("%s: the batch read answered for an invisible message: %+v", tt.name, s)
			}
		}
	}
	// A message in another channel through this channel is the same answer.
	other, err := f.s.CreateChannel(ctx, "elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.AddChannelMember(ctx, "system", other.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ReactionSummaryOf(ctx, other.ID, rdr(f.alice), f.postWide, reactionNow); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("a message of another channel = %v, want ErrMessageNotFound", err)
	}
	if _, err := f.s.SetReaction(ctx, other.ID, rdr(f.alice), f.postWide, rkey(t, thumbs), reactionNow); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("a set on a message of another channel = %v, want ErrMessageNotFound", err)
	}
	// Readers who are not scoped (v0, v1) or have no identity see nothing.
	for _, r := range []Reader{{PrincipalID: f.alice.ID}, {Scoped: true}, ChannelWideOnly} {
		if _, err := f.s.ReactionSummaryOf(ctx, f.ch.ID, r, f.postWide, reactionNow); err == nil {
			t.Errorf("reader %+v read a summary", r)
		}
	}
}

// The explicit clause of §6.1: for a channel-wide message messageVisible does
// not ask that the reader is still a member and enabled, so the probe does. A
// reader removed from the channel, or disabled, finds nothing.
func TestReactionProbeAsksMembershipAndEnabled(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	f.set(t, f.ch.ID, f.alice, f.postWide, thumbs)
	probe := func(p Principal) error {
		_, err := reactionTargetTx(ctx, f.s.db, f.ch.ID, rdr(p), f.postWide)
		return err
	}
	if err := probe(f.frank); err != nil {
		t.Fatalf("a member's probe = %v", err)
	}
	if err := probe(f.carol); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("a stranger's probe of a channel-wide message = %v, want ErrMessageNotFound", err)
	}
	if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.frank.ID); err != nil {
		t.Fatal(err)
	}
	if err := probe(f.frank); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("a removed member's probe of a channel-wide message = %v, want ErrMessageNotFound", err)
	}
	if _, err := f.s.DisablePrincipal(ctx, "system", f.dave.ID); err != nil {
		t.Fatal(err)
	}
	if err := probe(f.dave); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("a disabled member's probe = %v, want ErrMessageNotFound", err)
	}
	for _, p := range []Principal{f.frank, f.dave} {
		if _, err := f.s.ReactionSummaryOf(ctx, f.ch.ID, rdr(p), f.postWide, reactionNow); !errors.Is(err, ErrNotChannelMember) {
			t.Errorf("summary for %d = %v, want ErrNotChannelMember", p.ID, err)
		}
		if _, err := f.s.SetReaction(ctx, f.ch.ID, rdr(p), f.postWide, rkey(t, party), reactionNow); !errors.Is(err, ErrNotChannelMember) {
			t.Errorf("set for %d = %v, want ErrNotChannelMember", p.ID, err)
		}
	}
	batch, err := f.s.ReactionSummariesOf(ctx, f.ch.ID, rdr(f.alice), []int64{f.postWide}, reactionNow)
	if err != nil || len(batch) != 1 {
		t.Errorf("a member's batch = %v, %v", batch, err)
	}
}

// §11.1: reaction rows outlive the reactor's access. Leaving the channel, being
// taken off a net, being disabled or hidden changes no count anyone has seen,
// and the reactor shows in the list still.
func TestReactionsOutliveAccess(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	for _, who := range []Principal{f.alice, f.dave, f.erin, f.frank} {
		f.set(t, f.ch.ID, who, f.postWide, thumbs)
	}
	if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.alice.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.DisablePrincipal(ctx, "system", f.dave.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.HidePrincipal(ctx, "system", f.erin.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := counts(f.summary(t, f.frank, f.postWide)); got[rkey(t, thumbs)] != 4 {
		t.Errorf("after the reactors lost access the count is %v, want 4", got)
	}
	page, err := f.s.ListReactorsOf(ctx, f.ch.ID, rdr(f.frank), f.postWide, rkey(t, thumbs), 0, 10)
	if err != nil || len(page.Reactors) != 4 {
		t.Fatalf("reactors = %+v, %v; want all 4", page, err)
	}
	// The removed member cannot change its row, and a re-added one can.
	if _, err := f.s.RemoveReaction(ctx, f.ch.ID, rdr(f.alice), f.postWide, rkey(t, thumbs), reactionNow); !errors.Is(err, ErrNotChannelMember) {
		t.Errorf("a removed member's remove = %v, want ErrNotChannelMember", err)
	}
	if _, err := f.s.AddChannelMember(ctx, "system", f.ch.ID, f.alice.ID, 0); err != nil {
		t.Fatal(err)
	}
	c, err := f.s.RemoveReaction(ctx, f.ch.ID, rdr(f.alice), f.postWide, rkey(t, thumbs), reactionNow)
	if err != nil || !c.Changed {
		t.Errorf("a re-added member's remove = %+v, %v", c, err)
	}
}

// G1 to G4: the guest's watermark, the attribution it leaves, what ending the
// visit does, and what conversion does.
func TestGuestReactions(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	g := f.guest.Principal
	// G1: before the bound, a reply in an old thread, after the bound.
	for _, msg := range []int64{f.preWide, f.oldThreadReply, f.preRoot} {
		if _, err := f.s.SetReaction(ctx, f.ch.ID, rdr(g), msg, rkey(t, thumbs), reactionNow); !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("guest set on %d = %v, want ErrMessageNotFound", msg, err)
		}
		if _, err := f.s.ReactionSummaryOf(ctx, f.ch.ID, rdr(g), msg, reactionNow); !errors.Is(err, ErrMessageNotFound) {
			t.Errorf("guest read of %d = %v, want ErrMessageNotFound", msg, err)
		}
	}
	// Reactions on the old messages exist, from members, and are invisible to the guest.
	f.set(t, f.ch.ID, f.alice, f.preWide, thumbs)
	batch, err := f.s.ReactionSummariesOf(ctx, f.ch.ID, rdr(g), []int64{f.preWide, f.postWide}, reactionNow)
	if err != nil || len(batch) != 0 {
		t.Errorf("guest batch = %v, %v; want the old message absent", batch, err)
	}
	// G2: a set after the bound carries the attribution, for everyone who sees the message.
	f.set(t, f.ch.ID, g, f.postWide, thumbs)
	page, err := f.s.ListReactorsOf(ctx, f.ch.ID, rdr(f.frank), f.postWide, rkey(t, thumbs), 0, 10)
	if err != nil || len(page.Reactors) != 1 || page.Reactors[0].Guest == nil || page.Reactors[0].Guest.Label != "Pat" ||
		page.Reactors[0].Guest.Handle != f.guest.Visit.Handle || page.Reactors[0].Guest.Tag != schema.GuestTag(f.guest.Visit.Handle) {
		t.Fatalf("reactors = %+v, %v; want the guest's attribution", page, err)
	}
	f.set(t, f.ch.ID, f.frank, f.postWide, thumbs)

	// G3: ending the visit keeps the row and its attribution; the former guest
	// can do nothing, and a member still sees the count.
	if _, err := f.s.EndGuestVisit(ctx, "system", g.ID, schema.GuestEndVisit); err != nil {
		t.Fatal(err)
	}
	if got := counts(f.summary(t, f.frank, f.postWide)); got[rkey(t, thumbs)] != 2 {
		t.Errorf("after the visit ended the count is %v, want 2", got)
	}
	page, _ = f.s.ListReactorsOf(ctx, f.ch.ID, rdr(f.frank), f.postWide, rkey(t, thumbs), 0, 10)
	if len(page.Reactors) != 2 || page.Reactors[1].Guest == nil || page.Reactors[1].Guest.Label != "Pat" || page.Reactors[0].Guest != nil {
		t.Errorf("reactors after the visit = %+v, want the attribution kept", page)
	}
	for name, err := range map[string]error{
		"set": func() error {
			_, e := f.s.SetReaction(ctx, f.ch.ID, rdr(g), f.postWide, rkey(t, party), reactionNow)
			return e
		}(),
		"remove": func() error {
			_, e := f.s.RemoveReaction(ctx, f.ch.ID, rdr(g), f.postWide, rkey(t, thumbs), reactionNow)
			return e
		}(),
		"read": func() error {
			_, e := f.s.ReactionSummaryOf(ctx, f.ch.ID, rdr(g), f.postWide, reactionNow)
			return e
		}(),
	} {
		if !errors.Is(err, ErrNotChannelMember) {
			t.Errorf("an ended guest's %s = %v, want ErrNotChannelMember", name, err)
		}
	}
	if reactionRows(t, f.s) != 3 {
		t.Errorf("%d rows after the visit ended, want 3 kept", reactionRows(t, f.s))
	}
}

// G4: conversion keeps the principal, so the rows are the member's: no
// duplicate, no reassignment; the bound stays; rows from the visit keep the
// guest attribution and rows after it carry none.
func TestConvertedGuestReactions(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	allowAddressAccounts(t, f.s)
	g := f.guest.Principal
	f.set(t, f.ch.ID, g, f.postWide, thumbs)
	if _, err := f.s.ConvertGuest(ctx, "system", GuestConversion{Record: testRecord("conv"), PrincipalID: g.ID, Name: "patricia",
		CredentialLabel: "member", CredentialLifetime: time.Hour}); err != nil {
		t.Fatal(err)
	}
	s := f.summary(t, g, f.postWide)
	if len(s.Reactions) != 1 || !s.Reactions[0].Me || s.Reactions[0].Count != 1 {
		t.Fatalf("after conversion: %+v, want the same reaction, mine", s)
	}
	// Setting it again is a no-op, not a duplicate; a new key is a new row.
	if c := f.set(t, f.ch.ID, g, f.postWide, thumbs); c.Changed {
		t.Error("setting the converted member's own reaction again changed a row")
	}
	f.set(t, f.ch.ID, g, f.postWide, party)
	if _, err := f.s.SetReaction(ctx, f.ch.ID, rdr(g), f.preWide, rkey(t, thumbs), reactionNow); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("the bound after conversion: set = %v, want ErrMessageNotFound", err)
	}
	for _, tt := range []struct {
		key       string
		wantGuest bool
	}{{thumbs, true}, {party, false}} {
		page, err := f.s.ListReactorsOf(ctx, f.ch.ID, rdr(f.frank), f.postWide, rkey(t, tt.key), 0, 10)
		if err != nil || len(page.Reactors) != 1 || (page.Reactors[0].Guest != nil) != tt.wantGuest || page.Reactors[0].PrincipalID != g.ID {
			t.Errorf("reactors of %s = %+v, %v; want the member, with a guest attribution = %v", tt.key, page, err, tt.wantGuest)
		}
	}
}

// R1: a set racing a membership removal either commits and is retained, or is
// refused as a stranger's; never anything else, and the count equals the rows.
func TestSetRacingMembershipRemoval(t *testing.T) {
	for range 20 {
		f := newBoundFixture(t)
		ctx := context.Background()
		var wg sync.WaitGroup
		var setErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, setErr = f.s.SetReaction(ctx, f.ch.ID, rdr(f.frank), f.postWide, rkey(t, thumbs), reactionNow)
		}()
		go func() {
			defer wg.Done()
			if _, err := f.s.RemoveChannelMember(ctx, "system", f.ch.ID, f.frank.ID); err != nil {
				t.Error(err)
			}
		}()
		wg.Wait()
		rows := reactionRows(t, f.s)
		switch {
		case setErr == nil && rows == 1:
		case errors.Is(setErr, ErrNotChannelMember) && rows == 0:
		default:
			t.Fatalf("set = %v with %d rows", setErr, rows)
		}
		if got := counts(f.summary(t, f.alice, f.postWide))[rkey(t, thumbs)]; got != rows {
			t.Fatalf("count %d but %d rows", got, rows)
		}
	}
}

// R3 and MD1, MD3: a moderator removes one reaction or all of a person's.
// Both are idempotent and audited once; the target's other things are not
// touched; an owner's, an agent's and an ended guest's reaction go too.
func TestModerateReactions(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	f.set(t, f.ch.ID, f.dave, f.postWide, thumbs)
	f.set(t, f.ch.ID, f.dave, f.postWide, party)
	f.set(t, f.ch.ID, f.agt, f.postWide, thumbs)
	f.set(t, f.ch.ID, f.guest.Principal, f.postWide, "🔥")
	if _, err := f.s.EndGuestVisit(ctx, "system", f.guest.Principal.ID, schema.GuestEndVisit); err != nil {
		t.Fatal(err)
	}
	moderate := func(msg int64, target Principal, key string) ReactionChange {
		t.Helper()
		k := ""
		if key != "" {
			k = rkey(t, key)
		}
		c, err := f.s.ModerateReactions(ctx, f.ch.ID, rdr(f.alice), msg, target.ID, k, reactionNow)
		if err != nil {
			t.Fatalf("ModerateReactions: %v", err)
		}
		return c
	}
	c := moderate(f.postWide, f.dave, thumbs)
	if !c.Changed || counts(c.Summary)[rkey(t, thumbs)] != 1 || counts(c.Summary)[rkey(t, party)] != 1 {
		t.Errorf("removing one reaction: %+v", c)
	}
	if c := moderate(f.postWide, f.dave, thumbs); c.Changed {
		t.Error("removing a reaction already gone changed a row")
	}
	if c := moderate(f.postWide, f.dave, ""); !c.Changed || counts(c.Summary)[rkey(t, party)] != 0 {
		t.Errorf("removing all of dave's: %+v", c)
	}
	if c := moderate(f.postWide, f.agt, thumbs); !c.Changed {
		t.Error("an agent's reaction was not removed")
	}
	if c := moderate(f.postWide, f.guest.Principal, ""); !c.Changed {
		t.Error("an ended guest's reaction was not removed")
	}
	// An unknown principal id removes nothing and is not an error.
	if c := moderate(f.postWide, Principal{ID: 987654}, ""); c.Changed {
		t.Error("an unknown target changed a row")
	}
	if got := auditCount(t, f.s, AuditReactionModerated); got != 4 {
		t.Errorf("%d reaction_moderated rows, want 4 (the repeats wrote none)", got)
	}
	evs, _ := f.s.ListAuditEvents(ctx, 0, 1000)
	for _, e := range evs {
		if e.Action == AuditReactionModerated && (!strings.HasPrefix(e.Detail, fmt.Sprintf("channel=%d principal=", f.ch.ID)) || !strings.Contains(e.Detail, "removed=")) {
			t.Errorf("moderation audit detail = %q", e.Detail)
		}
	}
	// It changed a record, not an account: dave is still a member.
	if m, err := f.s.IsChannelMember(ctx, f.ch.ID, f.dave.ID); err != nil || !m {
		t.Errorf("dave's membership after moderation = %v, %v", m, err)
	}
	// The moderator must see the message: a whisper they are not in is a ghost.
	if _, err := f.s.ModerateReactions(ctx, f.ch.ID, rdr(f.alice), f.postWhisper, f.dave.ID, "", reactionNow); !errors.Is(err, ErrMessageNotFound) {
		t.Errorf("moderating a whisper the moderator cannot see = %v, want ErrMessageNotFound", err)
	}
	if _, err := f.s.ModerateReactions(ctx, f.ch.ID, rdr(f.carol), f.postWide, f.dave.ID, "", reactionNow); !errors.Is(err, ErrNotChannelMember) {
		t.Errorf("moderating as a non-member = %v, want ErrNotChannelMember", err)
	}
	// R3: a moderator's removal racing the reactor's own removal and a fresh set.
	f.set(t, f.ch.ID, f.dave, f.postRoot, thumbs)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			_, _ = f.s.ModerateReactions(ctx, f.ch.ID, rdr(f.alice), f.postRoot, f.dave.ID, "", reactionNow)
		}()
		go func() {
			defer wg.Done()
			_, _ = f.s.RemoveReaction(ctx, f.ch.ID, rdr(f.dave), f.postRoot, rkey(t, thumbs), reactionNow)
		}()
		go func() {
			defer wg.Done()
			_, _ = f.s.SetReaction(ctx, f.ch.ID, rdr(f.dave), f.postRoot, rkey(t, thumbs), reactionNow)
		}()
	}
	wg.Wait()
	var rows int
	_ = f.s.db.QueryRow("SELECT COUNT(*) FROM message_reactions WHERE message_id = ?", f.postRoot).Scan(&rows)
	if got := counts(f.summary(t, f.alice, f.postRoot))[rkey(t, thumbs)]; got != rows || rows > 1 {
		t.Errorf("after the race: count %d, rows %d", got, rows)
	}
}

// The batch read: ascending, only visible messages with a reaction, one
// summary each, the caller's own marks as me.
func TestReactionSummariesBatch(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	f.set(t, f.ch.ID, f.frank, f.postRoot, party)
	f.set(t, f.ch.ID, f.alice, f.postWide, thumbs)
	f.set(t, f.ch.ID, f.frank, f.postWide, thumbs)
	f.set(t, f.ch.ID, f.erin, f.postWhisper, "👀")
	got, err := f.s.ReactionSummariesOf(ctx, f.ch.ID, rdr(f.frank), []int64{f.postRoot, f.postWide, f.postWhisper, f.postReply, 1 << 40, f.postWide + 1000}, reactionNow)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, s := range got {
		ids = append(ids, s.MessageID)
	}
	if want := []int64{min(f.postWide, f.postRoot), max(f.postWide, f.postRoot)}; len(ids) != 2 || ids[0] != want[0] || ids[1] != want[1] {
		t.Errorf("batch ids = %v, want %v ascending", ids, want)
	}
	for _, s := range got {
		for _, r := range s.Reactions {
			if wantMe := s.MessageID == f.postWide || s.MessageID == f.postRoot; r.Me != wantMe {
				t.Errorf("message %d %s: me = %v", s.MessageID, r.Emoji, r.Me)
			}
		}
	}
	if _, err := f.s.ReactionSummariesOf(ctx, f.ch.ID, rdr(f.carol), []int64{f.postWide}, reactionNow); !errors.Is(err, ErrNotChannelMember) {
		t.Errorf("batch as a non-member = %v", err)
	}
}

// The reactor list: ascending by principal, paged by a cursor, an emoji nobody
// used is an empty page.
func TestListReactorsPages(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	who := []Principal{f.alice, f.bob, f.dave, f.erin, f.frank}
	for _, p := range who {
		f.set(t, f.ch.ID, p, f.postWide, thumbs)
	}
	var seen []int64
	after := int64(0)
	pages := 0
	for {
		page, err := f.s.ListReactorsOf(ctx, f.ch.ID, rdr(f.alice), f.postWide, rkey(t, thumbs), after, 2)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, r := range page.Reactors {
			seen = append(seen, r.PrincipalID)
		}
		if page.NextAfter == 0 {
			break
		}
		if page.NextAfter != page.Reactors[len(page.Reactors)-1].PrincipalID {
			t.Fatalf("next_after %d is not the page's last principal", page.NextAfter)
		}
		after = page.NextAfter
	}
	if pages != 3 || len(seen) != 5 {
		t.Errorf("%d pages, %d reactors; want 3 and 5", pages, len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Errorf("reactors out of order: %v", seen)
		}
	}
	empty, err := f.s.ListReactorsOf(ctx, f.ch.ID, rdr(f.alice), f.postWide, rkey(t, party), 0, 10)
	if err != nil || len(empty.Reactors) != 0 || empty.NextAfter != 0 {
		t.Errorf("an emoji nobody used = %+v, %v", empty, err)
	}
	if _, err := f.s.ListReactorsOf(ctx, f.ch.ID, rdr(f.alice), f.postWide, rkey(t, thumbs), 0, 0); err == nil {
		t.Error("a zero limit was accepted")
	}
}

// The summary's order is the earliest reaction still held with each key, then
// the key; counts are derived from the rows.
func TestSummaryOrder(t *testing.T) {
	f := newBoundFixture(t)
	step := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	tick := func() time.Time { step = step.Add(time.Second); return step }
	set := func(who Principal, key string) {
		t.Helper()
		if _, err := f.s.SetReaction(context.Background(), f.ch.ID, rdr(who), f.postWide, rkey(t, key), tick); err != nil {
			t.Fatal(err)
		}
	}
	set(f.alice, party)
	set(f.alice, thumbs)
	set(f.dave, heart)
	set(f.dave, thumbs)
	var order []string
	for _, r := range f.summary(t, f.frank, f.postWide).Reactions {
		order = append(order, r.Emoji)
	}
	if want := []string{rkey(t, party), rkey(t, thumbs), rkey(t, heart)}; !reflect.DeepEqual(order, want) {
		t.Errorf("order = %q, want %q", order, want)
	}
	// Removing the key's earliest reaction moves it behind a key that now leads.
	if _, err := f.s.RemoveReaction(context.Background(), f.ch.ID, rdr(f.alice), f.postWide, rkey(t, thumbs), tick); err != nil {
		t.Fatal(err)
	}
	order = nil
	for _, r := range f.summary(t, f.frank, f.postWide).Reactions {
		order = append(order, r.Emoji)
	}
	if want := []string{rkey(t, party), rkey(t, heart), rkey(t, thumbs)}; !reflect.DeepEqual(order, want) {
		t.Errorf("order after removing the earliest = %q, want %q", order, want)
	}
}

// W7: counts equal rows after a restart, for every emoji.
func TestReactionCountsSurviveRestart(t *testing.T) {
	f := newBoundFixture(t)
	for i, who := range []Principal{f.alice, f.bob, f.dave, f.erin} {
		f.set(t, f.ch.ID, who, f.postWide, thumbs)
		if i%2 == 0 {
			f.set(t, f.ch.ID, who, f.postWide, party)
		}
	}
	path := f.s.dbPath(t)
	before := counts(f.summary(t, f.frank, f.postWide))
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	sum, err := s.ReactionSummaryOf(context.Background(), f.ch.ID, rdr(f.frank), f.postWide, reactionNow)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(counts(sum), before) {
		t.Errorf("after the restart %v, before %v", counts(sum), before)
	}
	rows, err := s.db.Query("SELECT emoji, COUNT(*) FROM message_reactions WHERE message_id = ? GROUP BY emoji", f.postWide)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var e string
		var n int
		if err := rows.Scan(&e, &n); err != nil {
			t.Fatal(err)
		}
		if counts(sum)[e] != n {
			t.Errorf("%s: summary %d, rows %d", e, counts(sum)[e], n)
		}
	}
}

// dbPath is the path of the test store's file.
func (s *Store) dbPath(t *testing.T) string {
	t.Helper()
	var seq int
	var name, file string
	if err := s.db.QueryRow("PRAGMA database_list").Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	return file
}

// A key the store was not given canonical is refused before anything is read
// or written. C5 and C6: stored keys are never validated on read, so a key from
// a catalog this one no longer lists is still served, and a message that holds
// more kinds than the write limit still summarizes within the wire bound.
func TestReactionKeysAndOldData(t *testing.T) {
	f := newBoundFixture(t)
	ctx := context.Background()
	for _, bad := range []string{"", "x", "👍👍", "\xff", ":+1:", "👍\u202e"} {
		if _, err := f.s.SetReaction(ctx, f.ch.ID, rdr(f.alice), f.postWide, bad, reactionNow); !errors.Is(err, schema.ErrInvalidReaction) {
			t.Errorf("set %q = %v, want schema.ErrInvalidReaction", bad, err)
		}
		if _, err := f.s.RemoveReaction(ctx, f.ch.ID, rdr(f.alice), f.postWide, bad, reactionNow); !errors.Is(err, schema.ErrInvalidReaction) {
			t.Errorf("remove %q = %v, want schema.ErrInvalidReaction", bad, err)
		}
		if _, err := f.s.ListReactorsOf(ctx, f.ch.ID, rdr(f.alice), f.postWide, bad, 0, 5); !errors.Is(err, schema.ErrInvalidReaction) {
			t.Errorf("list %q = %v, want schema.ErrInvalidReaction", bad, err)
		}
	}
	// An unqualified spelling is not canonical either: the handlers canonicalize first.
	if _, err := f.s.SetReaction(ctx, f.ch.ID, rdr(f.alice), f.postWide, "❤", reactionNow); !errors.Is(err, schema.ErrInvalidReaction) {
		t.Errorf("set of a non-canonical spelling = %v", err)
	}
	if reactionRows(t, f.s) != 0 {
		t.Fatalf("a refused key stored a row")
	}
	// Seeded as an older catalog or a lower limit would have left them.
	for i := range 25 {
		if _, err := f.s.db.Exec(`INSERT INTO message_reactions (message_id, emoji, principal_id, channel_id, created_at) VALUES (?, ?, ?, ?, ?)`,
			f.postWide, fmt.Sprintf("old-key-%02d", i), f.alice.ID, f.ch.ID, int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	s := f.summary(t, f.frank, f.postWide)
	if len(s.Reactions) != 25 {
		t.Errorf("a summary of 25 stored kinds has %d", len(s.Reactions))
	}
	if schema.MaxReactionKindsOnWire < 25 {
		t.Error("the wire bound is below the seeded kinds")
	}
}

// C4: a database from the migration before this one opens and serves, and gets
// the table; nothing is backfilled and nothing is rewritten.
func TestReactionsMigration(t *testing.T) {
	const pre = reactionsMigration - 1
	if len(migrations) < reactionsMigration {
		t.Fatalf("%d migrations, the reactions migration is %d", len(migrations), reactionsMigration)
	}
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := range pre {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
		if step := migrationSteps[i+1]; step != nil {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := step(ctx, tx); err != nil {
				t.Fatalf("migration step %d: %v", i+1, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	var tables int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = 'message_reactions'").Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("the table exists before its migration: %d, %v", tables, err)
	}
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, name_key, name_skel, role, created_at) VALUES (1, 'human', 'nick', 'nick', 'nick', 'operator', 1), (2, 'human', 'sam', 'sam', 'sam', 'member', 2)`,
		`INSERT INTO channels (id, name, name_key, name_skel, created_at) VALUES (1, 'general', 'general', 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4), (1, 2, NULL, 4)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old message', 5)`,
		fmt.Sprintf(`PRAGMA user_version = %d`, pre),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	defer func() { _ = s.Close() }()
	if n := reactionRows(t, s); n != 0 {
		t.Errorf("%d reactions after the migration, want none", n)
	}
	sam := Reader{PrincipalID: 2, Scoped: true}
	c, err := s.SetReaction(ctx, 1, sam, 1, rkey(t, thumbs), reactionNow)
	if err != nil || !c.Changed {
		t.Fatalf("a reaction on a message from before the migration = %+v, %v", c, err)
	}
	if m, err := s.ListVisibleMessages(ctx, 1, sam, 0, 10); err != nil || len(m) != 1 || m[0].Body != "old message" {
		t.Errorf("the old message after the migration = %v, %v", m, err)
	}
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Errorf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
}
