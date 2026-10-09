package approvals

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #158, approval-object.md §5.1. Once a transition has committed, its
// notification gets at least one attempt and every attempt gets an audit row,
// whatever the store or the process does in between. These tests make one
// store call fail, or stop the manager at the wrong moment, and read the
// audit log.

// faultyStore is the real store with chosen calls made to fail.
type faultyStore struct {
	*store.Store
	mu    sync.Mutex
	fail  map[string]int // method -> failures left; negative means until cleared
	calls map[string]int
	// lie holds, per method, how many calls are still to be carried out in
	// full and then reported as failed: a commit whose answer was lost.
	lie map[string]int
}

var errInjected = errors.New("injected store failure")

func newFaultyStore(s *store.Store) *faultyStore {
	return &faultyStore{Store: s, fail: map[string]int{}, calls: map[string]int{}, lie: map[string]int{}}
}

// lying reports whether this call, having been carried out, is to be
// reported as failed.
func (f *faultyStore) lying(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lie[method] > 0 {
		f.lie[method]--
		return true
	}
	return false
}

func (f *faultyStore) set(method string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[method] = n
}

func (f *faultyStore) called(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// failing counts a call and reports whether it is to fail.
func (f *faultyStore) failing(method string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	switch n := f.fail[method]; {
	case n < 0:
		return true
	case n > 0:
		f.fail[method] = n - 1
		return true
	}
	return false
}

func (f *faultyStore) ApprovalByID(ctx context.Context, id int64) (store.Approval, error) {
	if f.failing("ApprovalByID") {
		return store.Approval{}, errInjected
	}
	return f.Store.ApprovalByID(ctx, id)
}

func (f *faultyStore) ResolutionByApprovalID(ctx context.Context, id int64) (schema.ApprovalResolutionV1, error) {
	if f.failing("ResolutionByApprovalID") {
		return schema.ApprovalResolutionV1{}, errInjected
	}
	return f.Store.ResolutionByApprovalID(ctx, id)
}

func (f *faultyStore) EscalateApproval(ctx context.Context, id int64) (bool, error) {
	if f.failing("EscalateApproval") {
		return false, errInjected
	}
	done, err := f.Store.EscalateApproval(ctx, id)
	if err == nil && f.lying("EscalateApproval") {
		return false, errInjected
	}
	return done, err
}

func (f *faultyStore) ExpireApproval(ctx context.Context, id int64) (*schema.ApprovalResolutionV1, error) {
	if f.failing("ExpireApproval") {
		return nil, errInjected
	}
	r, err := f.Store.ExpireApproval(ctx, id)
	if err == nil && f.lying("ExpireApproval") {
		return nil, errInjected
	}
	return r, err
}

func (f *faultyStore) LastAuditID(ctx context.Context) (int64, error) {
	if f.failing("LastAuditID") {
		return 0, errInjected
	}
	return f.Store.LastAuditID(ctx)
}

// AppendAuditEvent fails for the notify rows only ("AppendNotify") or for the
// start row only ("AppendStarted"), so a test can lose one and not the other.
func (f *faultyStore) AppendAuditEvent(ctx context.Context, actor, action, subject, detail string) (store.AuditEvent, error) {
	method := "AppendNotify"
	if action == AuditApprovalsStarted {
		method = "AppendStarted"
	}
	if f.failing(method) {
		return store.AuditEvent{}, errInjected
	}
	return f.Store.AppendAuditEvent(ctx, actor, action, subject, detail)
}

func (f *faultyStore) ListAuditEvents(ctx context.Context, afterID int64, limit int) ([]store.AuditEvent, error) {
	if f.failing("ListAuditEvents") {
		return nil, errInjected
	}
	return f.Store.ListAuditEvents(ctx, afterID, limit)
}

func (f *faultyStore) LastAuditEvent(ctx context.Context, action string) (store.AuditEvent, error) {
	if f.failing("LastAuditEvent") {
		return store.AuditEvent{}, errInjected
	}
	return f.Store.LastAuditEvent(ctx, action)
}

// quick is a manager on a faulty store that retries every few milliseconds.
func quick(s *store.Store, n Notifier) (*Manager, *faultyStore) {
	f := newFaultyStore(s)
	m := New(s, n)
	m.store = f
	m.retryEvery = 5 * time.Millisecond
	return m, f
}

// row is one audit event about an approval, as "action detail".
func rows(t *testing.T, s *store.Store, approvalID int64) []string {
	t.Helper()
	events, err := s.ListAuditEvents(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		if e.Subject == fmt.Sprintf("approval:%d", approvalID) {
			switch e.Action {
			case AuditNotifySent, AuditNotifyFailed:
				out = append(out, e.Action+" "+e.Detail)
			default:
				out = append(out, e.Action)
			}
		}
	}
	return out
}

// waitRows waits until the audit rows about the approval are want.
func waitRows(t *testing.T, s *store.Store, approvalID int64, want []string) {
	t.Helper()
	deadline := time.Now().Add(waitBudget)
	var got []string
	for time.Now().Before(deadline) {
		if got = rows(t, s, approvalID); slices.Equal(got, want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("audit rows for approval %d:\n got  %q\n want %q", approvalID, got, want)
}

// notLoadedRow is the notify row for an attempt that could not be made
// because the approval could not be read.
func notLoadedRow(event string) string {
	return fmt.Sprintf("%s event=%s error=%q", AuditNotifyFailed, event, whyNotLoaded)
}

func farFuture() (time.Time, time.Time) {
	now := time.Now()
	return now.Add(time.Hour), now.Add(2 * time.Hour)
}

// A read that fails after the commit is tried again, and the notification is
// then delivered: one attempt, one row.
func TestLoadFailureAfterCommitIsRetried(t *testing.T) {
	tests := []struct {
		name   string
		method string
		decide bool
		want   func(id int64) []string
		events []string
	}{
		{"created: the approval cannot be read", "ApprovalByID", false,
			func(int64) []string {
				return []string{store.AuditApprovalCreated, AuditNotifySent + " event=created"}
			}, []string{"created"}},
		{"resolved: the approval cannot be read", "ApprovalByID", true,
			func(int64) []string {
				return []string{store.AuditApprovalCreated, AuditNotifySent + " event=created", store.AuditDecisionCast,
					store.AuditApprovalResolved, AuditNotifySent + " event=resolved"}
			}, []string{"created", "resolved"}},
		{"resolved: the resolution cannot be read", "ResolutionByApprovalID", true,
			func(int64) []string {
				return []string{store.AuditApprovalCreated, AuditNotifySent + " event=created", store.AuditDecisionCast,
					store.AuditApprovalResolved, AuditNotifySent + " event=resolved"}
			}, []string{"created", "resolved"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := openTestStore(t)
			n := &recordingNotifier{}
			m, f := quick(s, n)
			defer m.Close()
			ctx := context.Background()
			channelID, agentID, humanID := fixture(t, s)
			deadline, grace := farFuture()

			if !tt.decide {
				f.set(tt.method, 3)
			}
			a, err := m.Create(ctx, params(channelID, agentID, deadline, grace))
			if err != nil {
				t.Fatalf("Create must not fail because the read-back did: %v", err)
			}
			if tt.decide {
				f.set(tt.method, 3)
				if _, r, err := m.Decide(ctx, a.ID, humanID, "approve", "fine"); err != nil || r == nil {
					t.Fatalf("Decide = %+v, %v; must not fail because the read-back did", r, err)
				}
			}
			waitRows(t, s, a.ID, tt.want(a.ID))
			if got := n.recorded(); !slices.Equal(got, tt.events) {
				t.Errorf("deliveries = %v, want %v (each exactly once)", got, tt.events)
			}
			// What is delivered after a retry is the stored resolution, not
			// an empty one.
			if tt.decide {
				got := n.resolved()
				if len(got) != 1 || got[0].ApprovalID != a.ID || got[0].Outcome != schema.OutcomeApproved || len(got[0].Decisions) != 1 {
					t.Errorf("resolution delivered = %+v, want the approval's own", got)
				}
			}
		})
	}
}

// A read that keeps failing ends in a row that says the notification was not
// attempted, not in a log line alone.
func TestLoadFailureThatPersistsIsRecordedAsNotAttempted(t *testing.T) {
	s := openTestStore(t)
	n := &recordingNotifier{}
	m, f := quick(s, n)
	defer m.Close()
	channelID, agentID, _ := fixture(t, s)
	deadline, grace := farFuture()

	f.set("ApprovalByID", -1)
	a, err := m.Create(context.Background(), params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	waitRows(t, s, a.ID, []string{store.AuditApprovalCreated, notLoadedRow("created")})
	if got := n.recorded(); len(got) != 0 {
		t.Errorf("deliveries = %v, want none: there was nothing to send", got)
	}
	if got := f.called("ApprovalByID"); got != 1+loadRetries {
		t.Errorf("reads = %d, want the first and %d retries", got, loadRetries)
	}
	// And it stops there: no further row, no further read.
	time.Sleep(10 * m.retryEvery)
	if got := f.called("ApprovalByID"); got != 1+loadRetries {
		t.Errorf("reads after giving up = %d, want %d", got, 1+loadRetries)
	}
	waitRows(t, s, a.ID, []string{store.AuditApprovalCreated, notLoadedRow("created")})
}

// A notify row the store refuses is written later. The notification is not
// sent a second time: the attempt was made.
func TestFailedNotifyAuditInsertIsRetriedWithoutSendingAgain(t *testing.T) {
	for _, failDelivery := range []bool{false, true} {
		t.Run(fmt.Sprintf("delivery fails=%v", failDelivery), func(t *testing.T) {
			s := openTestStore(t)
			n := &recordingNotifier{fail: failDelivery}
			m, f := quick(s, n)
			defer m.Close()
			channelID, agentID, _ := fixture(t, s)
			deadline, grace := farFuture()

			f.set("AppendNotify", 4)
			a, err := m.Create(context.Background(), params(channelID, agentID, deadline, grace))
			if err != nil {
				t.Fatal(err)
			}
			want := AuditNotifySent + " event=created"
			if failDelivery {
				want = fmt.Sprintf("%s event=created error=%q", AuditNotifyFailed, "ntfy unreachable")
			}
			waitRows(t, s, a.ID, []string{store.AuditApprovalCreated, want})
			if got := f.called("AppendNotify"); got != 5 {
				t.Errorf("appends = %d, want 4 refused and 1 written", got)
			}
			if got := n.recorded(); !slices.Equal(got, []string{"created"}) {
				t.Errorf("deliveries = %v, want exactly one", got)
			}
		})
	}
}

// At the deadline, a store that cannot be read must not cost the approval its
// grace timer, and the escalation's notification must end in a row.
func TestEscalationWhenTheApprovalCannotBeRead(t *testing.T) {
	s := openTestStore(t)
	n := &recordingNotifier{}
	m, f := quick(s, n)
	defer m.Close()
	ctx := context.Background()
	channelID, agentID, _ := fixture(t, s)

	now := time.Now()
	a, err := m.Create(ctx, params(channelID, agentID, now.Add(30*time.Millisecond), now.Add(60*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	f.set("ApprovalByID", -1)
	// The escalation commits; its notification cannot be attempted.
	waitRows(t, s, a.ID, []string{
		store.AuditApprovalCreated, AuditNotifySent + " event=created",
		store.AuditApprovalEscalated, notLoadedRow("escalated"),
	})
	// The grace deadline has passed by now and the store is still unreadable:
	// the approval must not have expired on a guess, and must not be stuck.
	time.Sleep(80 * time.Millisecond)
	if got, err := s.ApprovalByID(ctx, a.ID); err != nil || got.State != schema.ApprovalStateEscalated {
		t.Fatalf("state while unreadable = %v, %v; want escalated", got.State, err)
	}
	f.set("ApprovalByID", 0)
	// The store answers: the grace timer is armed (already due) and fires.
	waitRows(t, s, a.ID, []string{
		store.AuditApprovalCreated, AuditNotifySent + " event=created",
		store.AuditApprovalEscalated, notLoadedRow("escalated"),
		store.AuditApprovalExpired, AuditNotifySent + " event=expired",
	})
	if got := n.recorded(); !slices.Equal(got, []string{"created", "resolved"}) {
		t.Errorf("deliveries = %v, want created then the expiry", got)
	}
}

// A transition the store refuses at the timer is tried again, instead of the
// approval staying where it was until the next restart.
func TestTimerTransitionsAreRetriedAfterAStoreFailure(t *testing.T) {
	s := openTestStore(t)
	n := &recordingNotifier{}
	m, f := quick(s, n)
	defer m.Close()
	ctx := context.Background()
	channelID, agentID, _ := fixture(t, s)

	f.set("EscalateApproval", 3)
	f.set("ExpireApproval", 3)
	now := time.Now()
	a, err := m.Create(ctx, params(channelID, agentID, now.Add(20*time.Millisecond), now.Add(40*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	waitRows(t, s, a.ID, []string{
		store.AuditApprovalCreated, AuditNotifySent + " event=created",
		store.AuditApprovalEscalated, AuditNotifySent + " event=escalated",
		store.AuditApprovalExpired, AuditNotifySent + " event=expired",
	})
	if e, x := f.called("EscalateApproval"), f.called("ExpireApproval"); e != 4 || x != 4 {
		t.Errorf("escalate calls = %d, expire calls = %d; want 4 each (3 refused, 1 done)", e, x)
	}
}

// Close with a step still waiting to be retried returns, and leaves the
// transition without a row for the next start to find.
func TestCloseDropsWaitingRetries(t *testing.T) {
	s := openTestStore(t)
	n := &recordingNotifier{}
	m, f := quick(s, n)
	m.retryEvery = time.Hour
	channelID, agentID, _ := fixture(t, s)
	deadline, grace := farFuture()
	f.set("AppendNotify", -1)
	a, err := m.Create(context.Background(), params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return with a retry waiting")
	}
	if got := rows(t, s, a.ID); !slices.Equal(got, []string{store.AuditApprovalCreated}) {
		t.Errorf("rows = %q, want the creation alone", got)
	}
}

// ---------------------------------------------------------------------------
// Catch-up at start

// restart is a store on disk that a test can close and open again, the way a
// conchd that stopped and started does.
type restart struct {
	t    *testing.T
	path string
	s    *store.Store
}

func newRestart(t *testing.T) *restart {
	t.Helper()
	r := &restart{t: t, path: filepath.Join(t.TempDir(), "conch.db")}
	r.open()
	t.Cleanup(func() { _ = r.s.Close() })
	return r
}

func (r *restart) open() {
	r.t.Helper()
	s, err := store.Open(context.Background(), r.path)
	if err != nil {
		r.t.Fatalf("open store: %v", err)
	}
	r.s = s
}

// again closes the store and opens it afresh.
func (r *restart) again() {
	r.t.Helper()
	if err := r.s.Close(); err != nil {
		r.t.Fatalf("close store: %v", err)
	}
	r.open()
}

// start is one conchd start: a new manager on the store, rehydrated.
func (r *restart) start(n Notifier) (*Manager, *faultyStore) {
	r.t.Helper()
	m, f := quick(r.s, n)
	if err := m.Rehydrate(context.Background()); err != nil {
		r.t.Fatalf("Rehydrate: %v", err)
	}
	return m, f
}

// log is the whole audit log as "action subject detail" lines for the
// approval machinery: transitions, notify rows and start rows.
func (r *restart) log() []string {
	r.t.Helper()
	events, err := r.s.ListAuditEvents(context.Background(), 0, 10000)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		switch e.Action {
		case AuditApprovalsStarted:
			state, _ := startedRow(e)
			out = append(out, "started "+state)
		case AuditNotifySent, AuditNotifyFailed:
			out = append(out, e.Action+" "+e.Subject+" "+e.Detail)
		case store.AuditApprovalCreated, store.AuditApprovalEscalated, store.AuditApprovalResolved, store.AuditApprovalExpired:
			out = append(out, e.Action+" "+e.Subject)
		}
	}
	return out
}

func (r *restart) wantLog(want ...string) {
	r.t.Helper()
	if got := r.log(); !slices.Equal(got, want) {
		r.t.Fatalf("audit log:\n got  %s\n want %s", strings.Join(got, "\n      "), strings.Join(want, "\n      "))
	}
}

// waitLog waits until the log is want: a catch-up that could not finish at
// start keeps trying in the background.
func (r *restart) waitLog(want ...string) {
	r.t.Helper()
	deadline := time.Now().Add(waitBudget)
	for time.Now().Before(deadline) {
		if slices.Equal(r.log(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.wantLog(want...)
}

// A conchd that stops between a commit and the notification leaves a
// transition with no notify row. The next start attempts it, in log order,
// before it writes its own start row; the start after that owes nothing.
func TestCatchUpAfterStoppingBetweenCommitAndNotify(t *testing.T) {
	r := newRestart(t)
	ctx := context.Background()
	channelID, agentID, humanID := fixture(t, r.s)
	deadline, grace := farFuture()

	n1 := &recordingNotifier{}
	m1, _ := r.start(n1)
	told, err := m1.Create(ctx, params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	// The process dies with two transitions committed and nobody told: a
	// decision that resolved the first approval, and a second approval. The
	// store calls are what a handler had completed when it died.
	if _, res, err := r.s.CastDecision(ctx, told.ID, humanID, "approve", "fine"); err != nil || res == nil {
		t.Fatalf("CastDecision = %+v, %v", res, err)
	}
	untold, err := r.s.CreateApproval(ctx, params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	m1.Close()
	r.again()

	n2 := &recordingNotifier{}
	m2, _ := r.start(n2)
	if got := n2.recorded(); !slices.Equal(got, []string{"resolved", "created"}) {
		t.Fatalf("deliveries at the next start = %v, want the resolution then the creation", got)
	}
	if got := n2.resolved(); len(got) != 1 || got[0].ApprovalID != told.ID || got[0].Outcome != schema.OutcomeApproved {
		t.Errorf("resolution delivered at the next start = %+v, want the stored one", got)
	}
	a1, a2 := fmt.Sprintf("approval:%d", told.ID), fmt.Sprintf("approval:%d", untold.ID)
	want := []string{
		"started notifications=on",
		"approval_created " + a1,
		"notify_sent " + a1 + " event=created",
		"approval_resolved " + a1,
		"approval_created " + a2,
		"notify_sent " + a1 + " event=resolved",
		"notify_sent " + a2 + " event=created",
		"started notifications=on",
	}
	r.wantLog(want...)
	m2.Close()
	r.again()

	n3 := &recordingNotifier{}
	m3, _ := r.start(n3)
	defer m3.Close()
	if got := n3.recorded(); len(got) != 0 {
		t.Errorf("deliveries at the start after = %v, want none", got)
	}
	r.wantLog(append(want, "started notifications=on")...)
}

// The delivery was made but its row was not written when conchd stopped. The
// next start cannot know that, so it delivers again: at least once.
func TestCatchUpWhenOnlyTheRowWasLost(t *testing.T) {
	r := newRestart(t)
	channelID, agentID, _ := fixture(t, r.s)
	deadline, grace := farFuture()

	n1 := &recordingNotifier{}
	m1, f1 := r.start(n1)
	f1.set("AppendNotify", -1)
	a, err := m1.Create(context.Background(), params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	if got := n1.recorded(); !slices.Equal(got, []string{"created"}) {
		t.Fatalf("deliveries = %v", got)
	}
	m1.Close() // the retry of the row is still waiting
	r.again()

	n2 := &recordingNotifier{}
	m2, _ := r.start(n2)
	defer m2.Close()
	if got := n2.recorded(); !slices.Equal(got, []string{"created"}) {
		t.Fatalf("deliveries at the next start = %v, want the creation again", got)
	}
	subject := fmt.Sprintf("approval:%d", a.ID)
	r.wantLog("started notifications=on", "approval_created "+subject, "notify_sent "+subject+" event=created", "started notifications=on")
}

// What a start does about a transition left without a row, case by case.
func TestCatchUpCases(t *testing.T) {
	on, off := "started notifications=on", "started notifications=off"
	tests := []struct {
		name string
		// first says whether the start during which the approval was created
		// had a notifier; "none" means the manager was never rehydrated, as
		// on a database from before start rows existed.
		first string
		// second configures the next start.
		second     func(m *Manager, f *faultyStore)
		noNotifier bool
		prepare    func(f *faultyStore)
		deliveries []string
		log        func(subject string) []string
	}{
		{name: "owed and delivered", first: "on", deliveries: []string{"created"},
			log: func(a string) []string {
				return []string{on, "approval_created " + a, "notify_sent " + a + " event=created", on}
			}},
		{name: "the previous start had no notifier: nothing is owed", first: "off",
			log: func(a string) []string { return []string{off, "approval_created " + a, on} }},
		{name: "no previous start row: nowhere to read from", first: "none",
			log: func(a string) []string { return []string{"approval_created " + a, on} }},
		{name: "notifications are off now: abandoned, and the log says off", first: "on", noNotifier: true,
			log: func(a string) []string { return []string{on, "approval_created " + a, off} }},
		{name: "the budget is spent: recorded as not attempted", first: "on",
			second: func(m *Manager, _ *faultyStore) { m.catchUpBudget = -time.Second },
			log: func(a string) []string {
				return []string{on, "approval_created " + a, "notify_failed " + a + fmt.Sprintf(" event=created error=%q", whyBudgetSpent), on}
			}},
		{name: "the approval cannot be read: recorded as not attempted", first: "on",
			prepare: func(f *faultyStore) { f.set("ApprovalByID", -1) },
			log: func(a string) []string {
				return []string{on, "approval_created " + a, "notify_failed " + a + fmt.Sprintf(" event=created error=%q", whyNotLoaded), on}
			}},
		{name: "the row cannot be written: no start row, so the next start tries again", first: "on",
			prepare: func(f *faultyStore) { f.set("AppendNotify", -1) }, deliveries: []string{"created"},
			log: func(a string) []string { return []string{on, "approval_created " + a} }},
		{name: "the log cannot be read: no start row", first: "on",
			prepare: func(f *faultyStore) { f.set("ListAuditEvents", -1) },
			log:     func(a string) []string { return []string{on, "approval_created " + a} }},
		{name: "the last start cannot be read: no start row", first: "on",
			prepare: func(f *faultyStore) { f.set("LastAuditEvent", -1) },
			log:     func(a string) []string { return []string{on, "approval_created " + a} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRestart(t)
			channelID, agentID, _ := fixture(t, r.s)
			deadline, grace := farFuture()
			switch tt.first {
			case "on":
				m, _ := r.start(&recordingNotifier{})
				m.Close()
			case "off":
				m, _ := r.start(nil)
				m.Close()
			}
			// Committed, and conchd stopped before telling anyone.
			a, err := r.s.CreateApproval(context.Background(), params(channelID, agentID, deadline, grace))
			if err != nil {
				t.Fatal(err)
			}
			r.again()

			n := &recordingNotifier{}
			var notifier Notifier = n
			if tt.noNotifier {
				notifier = nil
			}
			m, f := quick(r.s, notifier)
			if tt.second != nil {
				tt.second(m, f)
			}
			if tt.prepare != nil {
				tt.prepare(f)
			}
			if err := m.Rehydrate(context.Background()); err != nil {
				t.Fatalf("Rehydrate must not fail because catch-up did: %v", err)
			}
			defer m.Close()
			if got := n.recorded(); !slices.Equal(got, tt.deliveries) {
				t.Errorf("deliveries = %v, want %v", got, tt.deliveries)
			}
			r.wantLog(tt.log(fmt.Sprintf("approval:%d", a.ID))...)
		})
	}
}

// A catch-up that could not write its rows wrote no start row, so the start
// after it reads from the same place and finishes the job.
func TestCatchUpIsRepeatedUntilEveryRowIsWritten(t *testing.T) {
	r := newRestart(t)
	channelID, agentID, _ := fixture(t, r.s)
	deadline, grace := farFuture()
	m1, _ := r.start(&recordingNotifier{})
	m1.Close()
	a, err := r.s.CreateApproval(context.Background(), params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("approval:%d", a.ID)
	r.again()

	n2 := &recordingNotifier{}
	m2, f2 := quick(r.s, n2)
	f2.set("AppendNotify", -1)
	if err := m2.Rehydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	m2.Close()
	r.wantLog("started notifications=on", "approval_created "+subject)
	r.again()

	n3 := &recordingNotifier{}
	m3, _ := r.start(n3)
	defer m3.Close()
	if got := n3.recorded(); !slices.Equal(got, []string{"created"}) {
		t.Errorf("deliveries at the third start = %v, want the creation", got)
	}
	r.wantLog("started notifications=on", "approval_created "+subject, "notify_sent "+subject+" event=created", "started notifications=on")
}

// A start row that cannot be written does not fail the start, and the next
// start reads from the older one: every transition since still has its row.
func TestCatchUpSurvivesALostStartRow(t *testing.T) {
	r := newRestart(t)
	channelID, agentID, _ := fixture(t, r.s)
	deadline, grace := farFuture()
	m1, _ := r.start(&recordingNotifier{})
	m1.Close()
	r.again()

	m2, f2 := quick(r.s, &recordingNotifier{})
	f2.set("AppendStarted", -1)
	if err := m2.Rehydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	told, err := m2.Create(context.Background(), params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	untold, err := r.s.CreateApproval(context.Background(), params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	m2.Close()
	r.again()

	n3 := &recordingNotifier{}
	m3, _ := r.start(n3)
	defer m3.Close()
	if got := n3.recorded(); !slices.Equal(got, []string{"created"}) {
		t.Errorf("deliveries = %v, want only the approval nobody was told about", got)
	}
	a1, a2 := fmt.Sprintf("approval:%d", told.ID), fmt.Sprintf("approval:%d", untold.ID)
	r.wantLog("started notifications=on",
		"approval_created "+a1, "notify_sent "+a1+" event=created",
		"approval_created "+a2, "notify_sent "+a2+" event=created",
		"started notifications=on")
}

// Catch-up reads the log in pages; a run longer than one page loses nothing.
func TestCatchUpReadsPastOnePage(t *testing.T) {
	r := newRestart(t)
	ctx := context.Background()
	channelID, agentID, _ := fixture(t, r.s)
	deadline, grace := farFuture()
	m1, _ := r.start(&recordingNotifier{})
	m1.Close()
	for i := 0; i < auditPage+20; i++ {
		if _, err := r.s.AppendAuditEvent(ctx, "system", "filler", "none", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.s.CreateApproval(ctx, params(channelID, agentID, deadline, grace)); err != nil {
		t.Fatal(err)
	}
	r.again()
	n := &recordingNotifier{}
	m2, _ := r.start(n)
	defer m2.Close()
	if got := n.recorded(); !slices.Equal(got, []string{"created"}) {
		t.Errorf("deliveries = %v, want the creation", got)
	}
}

// ---------------------------------------------------------------------------
// Review of #169

// The start row could not be written at the start. It is written as soon as
// the store takes it, and what it says holds for the next start whichever way
// the notifier setting changed in between. Before, a start whose row was lost
// left the older row standing: transitions of a run with notifications on
// were never caught up (the older row said off), and those of a run with
// notifications off were delivered later as if owed (the older row said on).
func TestStartRowIsRetriedSoTheNextStartReadsTheRightRun(t *testing.T) {
	on, off := "started notifications=on", "started notifications=off"
	tests := []struct {
		name       string
		first      bool // the first start has a notifier
		second     bool // so does the second, whose start row is refused at first
		deliveries []string
		log        func(a string) []string
	}{
		{"off, then on with the row refused: the on run is caught up", false, true, []string{"created"},
			func(a string) []string {
				return []string{off, on, "approval_created " + a, "notify_sent " + a + " event=created", on}
			}},
		{"on, then off with the row refused: the off run is not replayed", true, false, nil,
			func(a string) []string { return []string{on, off, "approval_created " + a, on} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRestart(t)
			channelID, agentID, _ := fixture(t, r.s)
			deadline, grace := farFuture()
			notifier := func(have bool) Notifier {
				if have {
					return &recordingNotifier{}
				}
				return nil
			}
			m1, _ := r.start(notifier(tt.first))
			m1.Close()
			r.again()

			m2, f2 := quick(r.s, notifier(tt.second))
			f2.set("AppendStarted", 3)
			if err := m2.Rehydrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			states := map[bool]string{true: on, false: off}
			r.waitLog(states[tt.first], states[tt.second])
			// Committed during the second run, and conchd stops before
			// telling anyone.
			a, err := r.s.CreateApproval(context.Background(), params(channelID, agentID, deadline, grace))
			if err != nil {
				t.Fatal(err)
			}
			m2.Close()
			r.again()

			n3 := &recordingNotifier{}
			m3, _ := r.start(n3)
			defer m3.Close()
			if got := n3.recorded(); !slices.Equal(got, tt.deliveries) {
				t.Errorf("deliveries at the third start = %v, want %v", got, tt.deliveries)
			}
			r.wantLog(tt.log(fmt.Sprintf("approval:%d", a.ID))...)
		})
	}
}

// A start row written late lands after transitions of its own run. The next
// start must still read those: it reads from where the row says the past
// ended, not from the row.
func TestLateStartRowDoesNotHideItsOwnRun(t *testing.T) {
	r := newRestart(t)
	channelID, agentID, _ := fixture(t, r.s)
	deadline, grace := farFuture()
	m1, _ := r.start(&recordingNotifier{})
	m1.Close()
	r.again()

	m2, f2 := quick(r.s, &recordingNotifier{})
	f2.set("AppendStarted", -1)
	if err := m2.Rehydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, err := r.s.CreateApproval(context.Background(), params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("approval:%d", a.ID)
	f2.set("AppendStarted", 0)
	r.waitLog("started notifications=on", "approval_created "+subject, "started notifications=on")
	m2.Close()
	r.again()

	n3 := &recordingNotifier{}
	m3, _ := r.start(n3)
	defer m3.Close()
	if got := n3.recorded(); !slices.Equal(got, []string{"created"}) {
		t.Fatalf("deliveries = %v, want the creation that came before the late start row", got)
	}
	r.wantLog("started notifications=on", "approval_created "+subject, "started notifications=on",
		"notify_sent "+subject+" event=created", "started notifications=on")
}

// Catch-up that cannot finish at start finishes in the same process once the
// store lets it, without delivering anything a second time.
func TestCatchUpFinishesInTheSameProcess(t *testing.T) {
	for _, method := range []string{"AppendNotify", "ListAuditEvents", "LastAuditEvent"} {
		t.Run(method+" fails at first", func(t *testing.T) {
			r := newRestart(t)
			channelID, agentID, _ := fixture(t, r.s)
			deadline, grace := farFuture()
			m1, _ := r.start(&recordingNotifier{})
			m1.Close()
			a, err := r.s.CreateApproval(context.Background(), params(channelID, agentID, deadline, grace))
			if err != nil {
				t.Fatal(err)
			}
			subject := fmt.Sprintf("approval:%d", a.ID)
			r.again()

			n := &recordingNotifier{}
			m2, f2 := quick(r.s, n)
			defer m2.Close()
			f2.set(method, 4)
			if err := m2.Rehydrate(context.Background()); err != nil {
				t.Fatal(err)
			}
			r.waitLog("started notifications=on", "approval_created "+subject, "notify_sent "+subject+" event=created", "started notifications=on")
			if got := n.recorded(); !slices.Equal(got, []string{"created"}) {
				t.Errorf("deliveries = %v, want exactly one", got)
			}
		})
	}
}

// slowNotifier answers only when its context ends.
type slowNotifier struct{ recordingNotifier }

func (n *slowNotifier) ApprovalCreated(ctx context.Context, _ store.Approval) error {
	<-ctx.Done()
	return n.record("created", ctx.Err())
}

// The start-up budget bounds the delivery in progress, not only whether the
// next one starts: a notifier that does not answer cannot hold up start-up.
func TestCatchUpBudgetBoundsTheDeliveryItself(t *testing.T) {
	r := newRestart(t)
	channelID, agentID, _ := fixture(t, r.s)
	deadline, grace := farFuture()
	m1, _ := r.start(&recordingNotifier{})
	m1.Close()
	a, err := r.s.CreateApproval(context.Background(), params(channelID, agentID, deadline, grace))
	if err != nil {
		t.Fatal(err)
	}
	subject := fmt.Sprintf("approval:%d", a.ID)
	r.again()

	m2, _ := quick(r.s, &slowNotifier{})
	m2.catchUpBudget = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- m2.Rehydrate(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Rehydrate did not return: the budget does not bound a delivery in progress")
	}
	defer m2.Close()
	r.wantLog("started notifications=on", "approval_created "+subject,
		"notify_failed "+subject+fmt.Sprintf(" event=created error=%q", context.DeadlineExceeded.Error()), "started notifications=on")
}

// panickyNotifier panics on every delivery.
type panickyNotifier struct{}

func (panickyNotifier) ApprovalCreated(context.Context, store.Approval) error {
	panic("notifier bug")
}
func (panickyNotifier) ApprovalEscalated(context.Context, store.Approval) error {
	panic("notifier bug")
}
func (panickyNotifier) ApprovalResolved(context.Context, store.Approval, schema.ApprovalResolutionV1) error {
	panic("notifier bug")
}

// A notifier that panics has failed a delivery. It must not cost an approval
// its timers, nor conchd its start: before, a panic in Create left the
// approval without a deadline timer, and one during catch-up panicked every
// start on the same owed transition.
func TestAPanickingNotifierFailsOnlyTheDelivery(t *testing.T) {
	r := newRestart(t)
	ctx := context.Background()
	channelID, agentID, _ := fixture(t, r.s)
	failed := func(event string) string {
		return fmt.Sprintf("%s event=%s error=%q", AuditNotifyFailed, event, errNotifierPanicked.Error())
	}

	m1, _ := r.start(panickyNotifier{})
	now := time.Now()
	a, err := m1.Create(ctx, params(channelID, agentID, now.Add(20*time.Millisecond), now.Add(40*time.Millisecond)))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// The timers were armed: the approval walks to expired, each
	// notification recorded as failed.
	waitRows(t, r.s, a.ID, []string{
		store.AuditApprovalCreated, failed("created"),
		store.AuditApprovalEscalated, failed("escalated"),
		store.AuditApprovalExpired, failed("expired"),
	})
	// And at a start: an owed transition does not stop Rehydrate.
	farDeadline, farGrace := farFuture()
	untold, err := r.s.CreateApproval(ctx, params(channelID, agentID, farDeadline, farGrace))
	if err != nil {
		t.Fatal(err)
	}
	m1.Close()
	r.again()
	m2, _ := r.start(panickyNotifier{})
	defer m2.Close()
	waitRows(t, r.s, untold.ID, []string{store.AuditApprovalCreated, failed("created")})
}

// The store carries out a timer's transition and then reports failure (the
// answer to a commit can be lost). The retry finds the transition done; what
// follows a commit, the notification and for an escalation the grace timer,
// must still happen, once.
func TestTimerTransitionThatCommittedButReportedFailure(t *testing.T) {
	s := openTestStore(t)
	n := &recordingNotifier{}
	m, f := quick(s, n)
	defer m.Close()
	ctx := context.Background()
	channelID, agentID, _ := fixture(t, s)

	f.mu.Lock()
	f.lie["EscalateApproval"], f.lie["ExpireApproval"] = 1, 1
	f.mu.Unlock()
	now := time.Now()
	a, err := m.Create(ctx, params(channelID, agentID, now.Add(20*time.Millisecond), now.Add(40*time.Millisecond)))
	if err != nil {
		t.Fatal(err)
	}
	waitRows(t, s, a.ID, []string{
		store.AuditApprovalCreated, AuditNotifySent + " event=created",
		store.AuditApprovalEscalated, AuditNotifySent + " event=escalated",
		store.AuditApprovalExpired, AuditNotifySent + " event=expired",
	})
	time.Sleep(10 * m.retryEvery)
	if got := n.recorded(); !slices.Equal(got, []string{"created", "escalated", "resolved"}) {
		t.Errorf("deliveries = %v, want each once", got)
	}
}

// A decision made between a failed escalation and its retry is not announced
// as an escalation: the retry acts only if the approval is escalated.
func TestRetriedEscalationOfADecidedApprovalAnnouncesNothing(t *testing.T) {
	s := openTestStore(t)
	n := &recordingNotifier{}
	m, f := quick(s, n)
	m.retryEvery = 50 * time.Millisecond
	defer m.Close()
	ctx := context.Background()
	channelID, agentID, humanID := fixture(t, s)

	f.set("EscalateApproval", 1)
	now := time.Now()
	a, err := m.Create(ctx, params(channelID, agentID, now.Add(10*time.Millisecond), now.Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return f.called("EscalateApproval") == 1 })
	// The decision goes straight to the store, as one does that commits
	// while the retry's callback is already running: Decide would stop a
	// timer that is still waiting, and then there is nothing to test.
	if _, res, err := s.CastDecision(ctx, a.ID, humanID, "approve", "in time"); err != nil || res == nil {
		t.Fatalf("CastDecision = %+v, %v", res, err)
	}
	waitFor(t, func() bool { return f.called("EscalateApproval") == 2 })
	time.Sleep(2 * m.retryEvery)
	waitRows(t, s, a.ID, []string{
		store.AuditApprovalCreated, AuditNotifySent + " event=created",
		store.AuditDecisionCast, store.AuditApprovalResolved,
	})
	if got := n.recorded(); !slices.Equal(got, []string{"created"}) {
		t.Errorf("deliveries = %v, want no escalation", got)
	}
}

// The start row says where the next start reads from, and a row without that
// (or with more after it) is still understood.
func TestStartedRow(t *testing.T) {
	tests := []struct {
		detail    string
		wantState string
		wantSince int64
	}{
		{"notifications=on since=41", notificationsOn, 41},
		{"notifications=off since=0", notificationsOff, 0},
		{"notifications=on", notificationsOn, 7},
		{"notifications=on since=41 later=addition", notificationsOn, 41},
		{"notifications=on since=nonsense", notificationsOn, 7},
		{"", "", 7},
	}
	for _, tt := range tests {
		state, since := startedRow(store.AuditEvent{ID: 7, Detail: tt.detail})
		if state != tt.wantState || since != tt.wantSince {
			t.Errorf("startedRow(%q) = %q, %d; want %q, %d", tt.detail, state, since, tt.wantState, tt.wantSince)
		}
	}
}

// Rehydrate cannot say what the past is without the end of the log, so it
// fails as it does when the open approvals cannot be listed.
func TestRehydrateFailsWhenTheLogCannotBeRead(t *testing.T) {
	s := openTestStore(t)
	m, f := quick(s, &recordingNotifier{})
	defer m.Close()
	f.set("LastAuditID", 1)
	if err := m.Rehydrate(context.Background()); !errors.Is(err, errInjected) {
		t.Fatalf("Rehydrate = %v, want the store's error", err)
	}
}

// heldNotifier holds each delivery until released.
type heldNotifier struct {
	recordingNotifier
	arrived chan struct{}
	release chan struct{}
}

func (n *heldNotifier) ApprovalCreated(context.Context, store.Approval) error {
	n.arrived <- struct{}{}
	<-n.release
	return n.record("created", nil)
}

// A catch-up still being retried while conchd serves concerns the past only.
// A transition of this run that is between its commit and its notify row at
// that moment is being announced by its own request; catch-up must not take
// it for owed and deliver it a second time.
func TestCatchUpRetryLeavesThisRunAlone(t *testing.T) {
	r := newRestart(t)
	channelID, agentID, _ := fixture(t, r.s)
	deadline, grace := farFuture()
	m1, _ := r.start(&recordingNotifier{})
	m1.Close()
	r.again()

	n := &heldNotifier{arrived: make(chan struct{}, 8), release: make(chan struct{}, 8)}
	m2, f2 := quick(r.s, n)
	defer m2.Close()
	// Runs before Close: a delivery still held would keep Close waiting.
	defer close(n.release)
	f2.set("LastAuditEvent", -1) // catch-up cannot finish yet
	if err := m2.Rehydrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	created := make(chan store.Approval, 1)
	go func() {
		a, err := m2.Create(context.Background(), params(channelID, agentID, deadline, grace))
		if err != nil {
			t.Errorf("Create: %v", err)
		}
		created <- a
	}()
	<-n.arrived // committed; its notification is in flight and has no row
	// Catch-up now runs to the end, with that transition in the log. Either
	// it writes its start row, or (the defect) it delivers the transition.
	f2.set("LastAuditEvent", 0)
	finished := func() bool { log := r.log(); return log[len(log)-1] == "started notifications=on" }
	for stop := time.Now().Add(waitBudget); !finished(); time.Sleep(5 * time.Millisecond) {
		if len(n.arrived) > 0 {
			t.Fatal("catch-up delivered a transition of this run that its own request was still announcing")
		}
		if time.Now().After(stop) {
			t.Fatal("catch-up never finished")
		}
	}
	n.release <- struct{}{}
	a := <-created
	subject := fmt.Sprintf("approval:%d", a.ID)
	r.waitLog("started notifications=on", "approval_created "+subject, "started notifications=on", "notify_sent "+subject+" event=created")
	if got := n.recorded(); !slices.Equal(got, []string{"created"}) {
		t.Errorf("deliveries = %v, want exactly one", got)
	}
}
