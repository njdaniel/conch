package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

var sessionT0 = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// sessionWorld is a store with two principals and a credential each, plus a
// second credential for the first.
type sessionWorld struct {
	s                 *Store
	ann, bea          Principal
	annCred, annCred2 int64
	beaCred           int64
	channel           Channel
}

func newSessionWorld(t *testing.T) *sessionWorld {
	t.Helper()
	ctx := context.Background()
	w := &sessionWorld{s: openTestStore(t)}
	var err error
	if w.ann, err = w.s.CreatePrincipal(ctx, PrincipalHuman, "ann"); err != nil {
		t.Fatal(err)
	}
	if w.bea, err = w.s.CreatePrincipal(ctx, PrincipalAgent, "bea"); err != nil {
		t.Fatal(err)
	}
	mk := func(p Principal) int64 {
		c, _, err := w.s.CreateCredential(ctx, "system", p.ID, "t", nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	w.annCred, w.annCred2, w.beaCred = mk(w.ann), mk(w.ann), mk(w.bea)
	if w.channel, err = w.s.CreateChannel(ctx, "room"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Principal{w.ann, w.bea} {
		if _, err := w.s.AddChannelMember(ctx, "system", w.channel.ID, p.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	return w
}

func (w *sessionWorld) create(t *testing.T, p Principal, cred int64, at time.Time) Session {
	t.Helper()
	sess, err := w.s.CreateSession(context.Background(), principalActor(p.ID), SessionParams{PrincipalID: p.ID, CredentialID: cred, Client: "tui"}, at)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return sess
}

func (w *sessionWorld) audit(t *testing.T, action string) []AuditEvent {
	t.Helper()
	events, err := w.s.ListAuditEvents(context.Background(), 0, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var out []AuditEvent
	for _, e := range events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func (w *sessionWorld) rows(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.s.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Migration 18 over a database that holds principals, credentials and
// messages: it adds the table and its indexes, touches nothing else, and
// sessions can be made afterwards (like TestThreadMigrationFromSchema16).
func TestSessionsMigrationFromSchema17(t *testing.T) {
	const pre = 17
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
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, role, created_at) VALUES (1, 'human', 'nick', 'operator', 1), (2, 'agent', 'bot', 'member', 2)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 3)`,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at) VALUES (1, 1, NULL, 4), (1, 2, NULL, 4)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 1, 'old', 5)`,
		`INSERT INTO credentials (id, principal_id, label, token_hash, created_at) VALUES (1, 1, 'a', 'h1', 6), (2, 2, 'b', 'h2', 6)`,
		`PRAGMA user_version = 17`,
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
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 18 {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil || n != 0 {
		t.Errorf("sessions after migration = %d, %v; want an empty table", n, err)
	}
	for _, idx := range []string{"sessions_by_principal", "sessions_by_credential", "sessions_live_by_last_seen"} {
		var name string
		if err := s.db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?", idx).Scan(&name); err != nil {
			t.Errorf("index %s missing: %v", idx, err)
		}
	}
	for _, c := range []struct {
		table string
		want  int
	}{{"principals", 2}, {"credentials", 2}, {"messages", 1}, {"channel_members", 2}} {
		var got int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + c.table).Scan(&got); err != nil || got != c.want {
			t.Errorf("%s rows after migration = %d, %v; want %d", c.table, got, err, c.want)
		}
	}

	sess, err := s.CreateSession(ctx, "principal:2", SessionParams{PrincipalID: 2, CredentialID: 2, Client: "adapter", Interventions: []schema.InterventionKind{schema.InterventionKindPause}}, sessionT0)
	if err != nil || sess.Kind != PrincipalAgent || sess.Name != "bot" || !reflect.DeepEqual(sess.Interventions, []schema.InterventionKind{schema.InterventionKindPause}) {
		t.Fatalf("session after migration = %+v, %v", sess, err)
	}
	got, err := s.ListChannelSessions(ctx, 1, sessionT0, time.Hour)
	if err != nil || len(got) != 1 || got[0].ID != sess.ID {
		t.Fatalf("list after migration = %+v, %v", got, err)
	}
	// The table's own constraints hold against writes that go round the API.
	for _, stmt := range []string{
		`INSERT INTO sessions (principal_id, credential_id, client, started_at, last_seen_at, state) VALUES (1, 1, 'x', 1, 1, 'dozing')`,
		`INSERT INTO sessions (principal_id, credential_id, client, started_at, last_seen_at, ended_at, state) VALUES (1, 1, 'x', 1, 1, 2, 'online')`,
		`INSERT INTO sessions (principal_id, credential_id, client, started_at, last_seen_at, state) VALUES (99, 1, 'x', 1, 1, 'online')`,
		`INSERT INTO sessions (principal_id, credential_id, client, started_at, last_seen_at, state) VALUES (1, 99, 'x', 1, 1, 'online')`,
	} {
		if _, err := s.db.Exec(stmt); err == nil {
			t.Errorf("the table accepted %q", stmt)
		}
	}
}

func TestCreateSessionRefusals(t *testing.T) {
	w := newSessionWorld(t)
	ctx := context.Background()
	// A credential that expires: made valid, then the store's clock passes it.
	soon := time.Now().Add(2 * time.Hour)
	expCred, _, err := w.s.CreateCredential(ctx, "system", w.ann.ID, "short", &soon)
	if err != nil {
		t.Fatal(err)
	}
	revoked := w.annCred2
	if err := w.s.RevokeCredential(ctx, "system", revoked); err != nil {
		t.Fatal(err)
	}
	disabledP, err := w.s.CreatePrincipal(ctx, PrincipalHuman, "dis")
	if err != nil {
		t.Fatal(err)
	}
	disCred, _, err := w.s.CreateCredential(ctx, "system", disabledP.ID, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.DisablePrincipal(ctx, "system", disabledP.ID); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		p    SessionParams
		want error
	}{
		{"a revoked credential", SessionParams{PrincipalID: w.ann.ID, CredentialID: revoked, Client: "x"}, ErrCredentialInvalid},
		{"an unknown credential", SessionParams{PrincipalID: w.ann.ID, CredentialID: 9999, Client: "x"}, ErrCredentialInvalid},
		{"another principal's credential", SessionParams{PrincipalID: w.bea.ID, CredentialID: w.annCred, Client: "x"}, ErrCredentialInvalid},
		{"a disabled principal", SessionParams{PrincipalID: disabledP.ID, CredentialID: disCred.ID, Client: "x"}, ErrCredentialInvalid},
		{"a live credential", SessionParams{PrincipalID: w.ann.ID, CredentialID: expCred.ID, Client: "x"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := w.rows(t)
			_, err := w.s.CreateSession(ctx, "system", tt.p, sessionT0)
			if !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			wantRows := before
			if tt.want == nil {
				wantRows++
			}
			if got := w.rows(t); got != wantRows {
				t.Errorf("rows = %d, want %d", got, wantRows)
			}
		})
	}
	// A credential that has expired by the store's clock gets no session.
	old := credentialNow
	t.Cleanup(func() { credentialNow = old })
	credentialNow = func() time.Time { return soon.Add(time.Second) }
	if _, err := w.s.CreateSession(ctx, "system", SessionParams{PrincipalID: w.ann.ID, CredentialID: expCred.ID, Client: "x"}, sessionT0); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("an expired credential: err = %v, want ErrCredentialInvalid", err)
	}
}

func TestSessionBound(t *testing.T) {
	w := newSessionWorld(t)
	ctx := context.Background()
	var ids []int64
	for range MaxLiveSessionsPerCredential {
		ids = append(ids, w.create(t, w.ann, w.annCred, sessionT0).ID)
	}
	_, err := w.s.CreateSession(ctx, "system", SessionParams{PrincipalID: w.ann.ID, CredentialID: w.annCred, Client: "x"}, sessionT0)
	if !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("err = %v, want ErrTooManySessions", err)
	}
	if n := w.rows(t); n != MaxLiveSessionsPerCredential {
		t.Errorf("rows = %d, want %d: the refused create wrote one", n, MaxLiveSessionsPerCredential)
	}
	if n := len(w.audit(t, AuditSessionStarted)); n != MaxLiveSessionsPerCredential {
		t.Errorf("session_started rows = %d", n)
	}
	// An offline session does not count, so a client that crashed sixteen
	// times is not locked out by the sessions it left behind.
	if _, err := w.s.SweepSessions(ctx, sessionT0.Add(time.Hour), 30*time.Second, 90*time.Second); err != nil {
		t.Fatal(err)
	}
	fresh := w.create(t, w.ann, w.annCred, sessionT0.Add(time.Hour))
	// Reviving an old one is held to the same bound: with 16 online, a
	// heartbeat on an offline session is refused and changes nothing.
	for range MaxLiveSessionsPerCredential - 1 {
		w.create(t, w.ann, w.annCred, sessionT0.Add(time.Hour))
	}
	if _, err := w.s.HeartbeatSession(ctx, ids[1], w.annCred, sessionT0.Add(time.Hour)); !errors.Is(err, ErrTooManySessions) {
		t.Errorf("reviving past the bound: err = %v, want ErrTooManySessions", err)
	}
	// A heartbeat on one that is already online is never refused.
	if _, err := w.s.HeartbeatSession(ctx, fresh.ID, w.annCred, sessionT0.Add(time.Hour)); err != nil {
		t.Errorf("heartbeat on an online session at the bound: %v", err)
	}
	// Ending one of the sixteen online frees a place.
	if err := w.s.EndSession(ctx, "system", fresh.ID, w.annCred, sessionT0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	w.create(t, w.ann, w.annCred, sessionT0.Add(time.Hour))
	// And the bound is the credential's.
	w.create(t, w.ann, w.annCred2, sessionT0)
}

func TestSessionStoreNeverReturnsNilInterventions(t *testing.T) {
	w := newSessionWorld(t)
	ctx := context.Background()
	for name, in := range map[string][]schema.InterventionKind{"nil": nil, "empty": {}} {
		sess, err := w.s.CreateSession(ctx, "system", SessionParams{PrincipalID: w.ann.ID, CredentialID: w.annCred, Client: name, Interventions: in}, sessionT0)
		if err != nil || sess.Interventions == nil || len(sess.Interventions) != 0 {
			t.Errorf("%s: %+v, %v; want a non-nil empty list", name, sess.Interventions, err)
		}
	}
	// A stored JSON null reads back as a list too.
	if _, err := w.s.db.Exec(`UPDATE sessions SET interventions = 'null'`); err != nil {
		t.Fatal(err)
	}
	got, err := w.s.ListChannelSessions(ctx, w.channel.ID, sessionT0, time.Hour)
	if err != nil || len(got) != 2 || got[0].Interventions == nil {
		t.Errorf("list = %+v, %v", got, err)
	}
}

// A clock that steps back must not store a session SessionV1.Validate would
// refuse: last_seen_at and ended_at are never before started_at.
func TestSessionTimestampsNeverPrecedeStart(t *testing.T) {
	w := newSessionWorld(t)
	ctx := context.Background()
	sess := w.create(t, w.ann, w.annCred, sessionT0)
	beat, err := w.s.HeartbeatSession(ctx, sess.ID, w.annCred, sessionT0.Add(-time.Hour))
	if err != nil || beat.LastSeenAt.Before(beat.StartedAt) {
		t.Fatalf("heartbeat from the past: %+v, %v", beat, err)
	}
	if err := w.s.EndSession(ctx, "system", sess.ID, w.annCred, sessionT0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	var started, ended int64
	if err := w.s.db.QueryRow("SELECT started_at, ended_at FROM sessions WHERE id = ?", sess.ID).Scan(&started, &ended); err != nil || ended < started {
		t.Errorf("started %d, ended %d, %v", started, ended, err)
	}
}

func TestSweepSessions(t *testing.T) {
	w := newSessionWorld(t)
	ctx := context.Background()
	stale, offline := 30*time.Second, 90*time.Second
	a := w.create(t, w.ann, w.annCred, sessionT0)
	b := w.create(t, w.bea, w.beaCred, sessionT0.Add(20*time.Second))
	ended := w.create(t, w.ann, w.annCred2, sessionT0)
	if err := w.s.EndSession(ctx, "system", ended.ID, w.annCred2, sessionT0); err != nil {
		t.Fatal(err)
	}

	state := func(id int64) schema.SessionState {
		var st string
		if err := w.s.db.QueryRow("SELECT state FROM sessions WHERE id = ?", id).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return schema.SessionState(st)
	}
	for _, step := range []struct {
		at             time.Duration
		want           SweepResult
		stateA, stateB schema.SessionState
		offlineRows    int
	}{
		{30 * time.Second, SweepResult{}, schema.SessionStateOnline, schema.SessionStateOnline, 0}, // exactly at the threshold: not older
		{31 * time.Second, SweepResult{Stale: 1}, schema.SessionStateStale, schema.SessionStateOnline, 0},
		{31 * time.Second, SweepResult{}, schema.SessionStateStale, schema.SessionStateOnline, 0}, // idempotent
		{51 * time.Second, SweepResult{Stale: 1}, schema.SessionStateStale, schema.SessionStateStale, 0},
		{91 * time.Second, SweepResult{Offline: 1}, schema.SessionStateOffline, schema.SessionStateStale, 1},
		{200 * time.Second, SweepResult{Offline: 1}, schema.SessionStateOffline, schema.SessionStateOffline, 2},
		{300 * time.Second, SweepResult{}, schema.SessionStateOffline, schema.SessionStateOffline, 2},
	} {
		got, err := w.s.SweepSessions(ctx, sessionT0.Add(step.at), stale, offline)
		if err != nil || got != step.want {
			t.Fatalf("at +%s: %+v, %v; want %+v", step.at, got, err, step.want)
		}
		if state(a.ID) != step.stateA || state(b.ID) != step.stateB {
			t.Fatalf("at +%s: states %s/%s, want %s/%s", step.at, state(a.ID), state(b.ID), step.stateA, step.stateB)
		}
		if n := len(w.audit(t, AuditSessionOffline)); n != step.offlineRows {
			t.Fatalf("at +%s: session_offline rows = %d, want %d", step.at, n, step.offlineRows)
		}
	}
	// The ended session was never touched by the sweeper, and nothing was deleted.
	var endedState string
	var endedAt sql.NullInt64
	if err := w.s.db.QueryRow("SELECT state, ended_at FROM sessions WHERE id = ?", ended.ID).Scan(&endedState, &endedAt); err != nil || endedState != "offline" || !endedAt.Valid {
		t.Errorf("ended session = %s %v %v", endedState, endedAt, err)
	}
	if n := w.rows(t); n != 3 {
		t.Errorf("rows = %d, want 3: the sweeper deleted one", n)
	}
	if n := len(w.audit(t, AuditSessionEnded)); n != 1 {
		t.Errorf("session_ended rows = %d, want 1 (the sweeper must not write them)", n)
	}
	// A heartbeat revives, and the next silence is a second session_offline.
	if _, err := w.s.HeartbeatSession(ctx, a.ID, w.annCred, sessionT0.Add(400*time.Second)); err != nil || state(a.ID) != schema.SessionStateOnline {
		t.Fatalf("revive: %s, %v", state(a.ID), err)
	}
	if _, err := w.s.SweepSessions(ctx, sessionT0.Add(500*time.Second), stale, offline); err != nil {
		t.Fatal(err)
	}
	if n := len(w.audit(t, AuditSessionOffline)); n != 3 {
		t.Errorf("session_offline rows after the second time offline = %d, want 3", n)
	}
}

// Revoking, rotating, revoke-all and disabling end the right sessions inside
// their own transactions: when the transaction fails, nothing is ended, and
// when it succeeds both happen.
func TestRevocationEndsSessionsAtomically(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		act   func(w *sessionWorld) error
		ended func(a1, a2, b1 Session) []int64
		cause string
	}{
		{"revoke", func(w *sessionWorld) error { return w.s.RevokeCredential(ctx, "op", w.annCred) },
			func(a1, _, _ Session) []int64 { return []int64{a1.ID} }, SessionEndedByRevocation},
		{"rotate", func(w *sessionWorld) error { _, _, err := w.s.RotateCredential(ctx, "op", w.annCred); return err },
			func(a1, _, _ Session) []int64 { return []int64{a1.ID} }, SessionEndedByRevocation},
		{"revoke-all", func(w *sessionWorld) error { _, err := w.s.RevokeAllCredentials(ctx, "op", w.ann.ID); return err },
			func(a1, a2, _ Session) []int64 { return []int64{a1.ID, a2.ID} }, SessionEndedByRevocation},
		{"disable", func(w *sessionWorld) error { _, err := w.s.DisablePrincipal(ctx, "op", w.ann.ID); return err },
			func(a1, a2, _ Session) []int64 { return []int64{a1.ID, a2.ID} }, SessionEndedPrincipalDisabled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newSessionWorld(t)
			a1 := w.create(t, w.ann, w.annCred, sessionT0)
			a2 := w.create(t, w.ann, w.annCred2, sessionT0)
			b1 := w.create(t, w.bea, w.beaCred, sessionT0)
			if err := tt.act(w); err != nil {
				t.Fatal(err)
			}
			want := map[int64]bool{}
			for _, id := range tt.ended(a1, a2, b1) {
				want[id] = true
			}
			for _, s := range []Session{a1, a2, b1} {
				var ended sql.NullInt64
				var state string
				if err := w.s.db.QueryRow("SELECT ended_at, state FROM sessions WHERE id = ?", s.ID).Scan(&ended, &state); err != nil {
					t.Fatal(err)
				}
				if ended.Valid != want[s.ID] {
					t.Errorf("session %d ended = %v, want %v", s.ID, ended.Valid, want[s.ID])
				}
				if want[s.ID] && state != "offline" {
					t.Errorf("ended session %d has state %s, want offline", s.ID, state)
				}
				if !want[s.ID] && state != "online" {
					t.Errorf("surviving session %d has state %s, want online", s.ID, state)
				}
			}
			rows := w.audit(t, AuditSessionEnded)
			if len(rows) != len(want) {
				t.Fatalf("session_ended rows = %d, want %d", len(rows), len(want))
			}
			for _, r := range rows {
				if r.Actor != "op" || !strings.Contains(r.Detail, "cause="+tt.cause) {
					t.Errorf("row = %+v, want actor op and cause %s", r, tt.cause)
				}
			}
		})
	}

	t.Run("a revoke that is refused ends nothing", func(t *testing.T) {
		// The last live operator credential cannot be revoked (ErrLastOperator):
		// the transaction is refused, and the operator's session lives.
		w := newSessionWorld(t)
		op, _, tok, err := w.s.BootstrapOperator(ctx, "root")
		if err != nil {
			t.Fatal(err)
		}
		_, opCred, err := w.s.ResolveCredentialDetail(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		sess := w.create(t, op, opCred, sessionT0)
		if err := w.s.RevokeCredential(ctx, "op", opCred); !errors.Is(err, ErrLastOperator) {
			t.Fatalf("err = %v, want ErrLastOperator", err)
		}
		var ended sql.NullInt64
		if err := w.s.db.QueryRow("SELECT ended_at FROM sessions WHERE id = ?", sess.ID).Scan(&ended); err != nil || ended.Valid {
			t.Errorf("a refused revoke ended the session: %v, %v", ended, err)
		}
		if n := len(w.audit(t, AuditSessionEnded)); n != 0 {
			t.Errorf("session_ended rows = %d, want 0", n)
		}
	})

	t.Run("revoking twice ends and audits once", func(t *testing.T) {
		w := newSessionWorld(t)
		w.create(t, w.ann, w.annCred, sessionT0)
		for range 2 {
			if err := w.s.RevokeCredential(ctx, "op", w.annCred); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(w.audit(t, AuditSessionEnded)); n != 1 {
			t.Errorf("session_ended rows = %d, want 1", n)
		}
	})
}

func TestListChannelSessions(t *testing.T) {
	w := newSessionWorld(t)
	ctx := context.Background()
	a := w.create(t, w.ann, w.annCred, sessionT0)
	b := w.create(t, w.bea, w.beaCred, sessionT0.Add(time.Hour))
	got, err := w.s.ListChannelSessions(ctx, w.channel.ID, sessionT0.Add(time.Hour), 90*time.Minute)
	if err != nil || len(got) != 2 || got[0].ID != a.ID || got[1].ID != b.ID {
		t.Fatalf("list = %+v, %v", got, err)
	}
	// Last seen exactly at the window's edge is still listed; one ms past is not.
	got, err = w.s.ListChannelSessions(ctx, w.channel.ID, sessionT0.Add(time.Hour), time.Hour)
	if err != nil || len(got) != 2 {
		t.Errorf("at the edge: %+v, %v; want both", got, err)
	}
	got, err = w.s.ListChannelSessions(ctx, w.channel.ID, sessionT0.Add(time.Hour+time.Millisecond), time.Hour)
	if err != nil || len(got) != 1 || got[0].ID != b.ID {
		t.Errorf("past the edge: %+v, %v; want only b", got, err)
	}
	// Nobody's: a channel that exists and holds nobody gets an empty, non-nil list.
	empty, err := w.s.CreateChannel(ctx, "empty")
	if err != nil {
		t.Fatal(err)
	}
	got, err = w.s.ListChannelSessions(ctx, empty.ID, sessionT0, time.Hour)
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty channel: %#v, %v", got, err)
	}
	// Removal from the channel removes the person's sessions from its list.
	if _, err := w.s.RemoveChannelMember(ctx, "system", w.channel.ID, w.ann.ID); err != nil {
		t.Fatal(err)
	}
	got, err = w.s.ListChannelSessions(ctx, w.channel.ID, sessionT0.Add(time.Hour), time.Hour)
	if err != nil || len(got) != 1 || got[0].ID != b.ID {
		t.Errorf("after removal: %+v, %v", got, err)
	}
}

// Bootstrapping the first operator revokes every credential issued before it
// (the credential routes were open until then), and a revoked credential's
// sessions end with it there too.
func TestBootstrapEndsSessionsOfTheCredentialsItRevokes(t *testing.T) {
	w := newSessionWorld(t)
	ctx := context.Background()
	sess := w.create(t, w.ann, w.annCred, sessionT0)
	if _, _, _, err := w.s.BootstrapOperator(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	ended := w.audit(t, AuditSessionEnded)
	if len(ended) != 1 || ended[0].Subject != sessionSubject(sess.ID) || !strings.Contains(ended[0].Detail, "cause="+SessionEndedByRevocation) {
		t.Fatalf("session_ended rows = %+v, want one for session %d with cause %s", ended, sess.ID, SessionEndedByRevocation)
	}
	if _, err := w.s.HeartbeatSession(ctx, sess.ID, w.annCred, sessionT0.Add(time.Second)); err == nil {
		t.Error("heartbeat on a session whose credential bootstrap revoked succeeded")
	}
}

// A credential's expiry ends its sessions at the next sweep (issue #301),
// whether the session was online or had already gone offline, with a
// session_ended event of cause credential_expired; a credential with no
// expiry is never touched, and a second sweep writes nothing more.
func TestSweepSessionsEndsExpiredCredentials(t *testing.T) {
	w := newSessionWorld(t)
	ctx := context.Background()
	stale, offline := 30*time.Second, 90*time.Second
	// The store checks expiry against its own clock when a credential or a
	// session is made, which is the real one; the sweeps below use ours.
	exp := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	mk := func(p Principal, expiresAt *time.Time) int64 {
		c, _, err := w.s.CreateCredential(ctx, "system", p.ID, "t", expiresAt)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	online := w.create(t, w.ann, mk(w.ann, &exp), exp.Add(-time.Minute))
	idle := w.create(t, w.bea, mk(w.bea, &exp), exp.Add(-10*time.Minute))
	forever := w.create(t, w.ann, w.annCred, exp.Add(-time.Minute))
	later := exp.Add(time.Hour)
	laterSess := w.create(t, w.bea, mk(w.bea, &later), exp.Add(-time.Minute))

	endedAt := func(id int64) (ended sql.NullInt64, state string) {
		if err := w.s.db.QueryRow("SELECT ended_at, state FROM sessions WHERE id = ?", id).Scan(&ended, &state); err != nil {
			t.Fatal(err)
		}
		return ended, state
	}
	// Before the expiry nothing is ended; the sessions age the ordinary way.
	got, err := w.s.SweepSessions(ctx, exp.Add(-time.Millisecond), stale, offline)
	if err != nil {
		t.Fatal(err)
	}
	if want := (SweepResult{Stale: 3, Offline: 1}); got != want {
		t.Fatalf("before expiry: %+v, want %+v", got, want)
	}
	if n := len(w.audit(t, AuditSessionEnded)); n != 0 {
		t.Fatalf("session_ended rows before expiry = %d", n)
	}

	// Exactly at the expiry, as authentication judges it (expires_at <= now).
	got, err = w.s.SweepSessions(ctx, exp, stale, offline)
	if err != nil {
		t.Fatal(err)
	}
	if got.Expired != 2 {
		t.Fatalf("at expiry: Expired = %d, want 2 (%+v)", got.Expired, got)
	}
	for _, tt := range []struct {
		name  string
		id    int64
		ended bool
	}{
		{"expired while online", online.ID, true},
		{"expired while offline", idle.ID, true},
		{"credential without expiry", forever.ID, false},
		{"credential that expires later", laterSess.ID, false},
	} {
		ended, state := endedAt(tt.id)
		if ended.Valid != tt.ended {
			t.Errorf("%s: ended = %v, want %v", tt.name, ended.Valid, tt.ended)
		}
		if tt.ended && (state != "offline" || ended.Int64 != exp.UnixMilli()) {
			t.Errorf("%s: state %q, ended_at %d, want offline at %d", tt.name, state, ended.Int64, exp.UnixMilli())
		}
	}
	events := w.audit(t, AuditSessionEnded)
	if len(events) != 2 {
		t.Fatalf("session_ended rows = %d, want 2: %+v", len(events), events)
	}
	for i, sess := range []Session{online, idle} {
		e := events[i]
		wantDetail := "cause=credential_expired " + sessionDetail(sess.PrincipalID, sess.CredentialID)
		if e.Actor != "system" || e.Subject != sessionSubject(sess.ID) || e.Detail != wantDetail {
			t.Errorf("event %d = %+v, want actor system, subject %s, detail %q", i, e, sessionSubject(sess.ID), wantDetail)
		}
	}

	// A second sweep, then one much later: nothing ends twice.
	for _, at := range []time.Time{exp, exp.Add(30 * time.Minute)} {
		got, err := w.s.SweepSessions(ctx, at, stale, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if got.Expired != 0 {
			t.Errorf("sweep at %v: Expired = %d, want 0", at, got.Expired)
		}
	}
	if n := len(w.audit(t, AuditSessionEnded)); n != 2 {
		t.Errorf("session_ended rows after more sweeps = %d, want 2", n)
	}
	// The one that expires later goes at its own time.
	if got, err := w.s.SweepSessions(ctx, later, stale, 24*time.Hour); err != nil || got.Expired != 1 {
		t.Errorf("at the later expiry: %+v, %v, want Expired 1", got, err)
	}
}

// The expired-credential statement finds sessions through indexes only: the
// credentials it looks at are walked by primary key or scanned (a table that
// holds credentials, not sessions) and the sessions come from
// sessions_by_credential, never from a scan of the sessions table.
func TestSweepExpiredQueryPlan(t *testing.T) {
	w := newSessionWorld(t)
	rows, err := w.s.db.Query(`EXPLAIN QUERY PLAN SELECT id, principal_id, credential_id FROM sessions
		WHERE ended_at IS NULL AND credential_id IN (SELECT id FROM credentials WHERE expires_at IS NOT NULL AND expires_at <= ?) ORDER BY id`, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	t.Log(strings.Join(plan, "\n"))
	if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "sessions_by_credential") || strings.Contains(joined, "SCAN sessions") || strings.Contains(joined, "SCAN s") {
		t.Errorf("plan does not search sessions by credential:\n%s", joined)
	}
}
