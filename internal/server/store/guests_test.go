package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Temporary guests in the store (issue #705): identities, the reserved
// namespace, attribution, bounds and the terminal transitions.

type guestFixture struct {
	t       *testing.T
	s       *Store
	ctx     context.Context
	op      Principal
	alice   Principal
	general Channel
	lobby   Channel
	ended   []EndedGuest
	mu      sync.Mutex
}

func newGuestFixture(t *testing.T) *guestFixture {
	t.Helper()
	s := openTestStore(t)
	ctx := context.Background()
	f := &guestFixture{t: t, s: s, ctx: ctx}
	var err error
	if f.op, _, _, err = s.BootstrapOperator(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	if f.alice, err = s.CreatePrincipal(ctx, PrincipalHuman, "alice"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dst  *Channel
		name string
	}{{&f.general, "general"}, {&f.lobby, "lobby"}} {
		if *c.dst, err = s.CreateChannel(ctx, c.name); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AddChannelMember(ctx, "system", c.dst.ID, f.alice.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	s.OnGuestsEnded(func(e []EndedGuest) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.ended = append(f.ended, e...)
	})
	return f
}

// guest starts a visit in general with the default test bounds.
func (f *guestFixture) guest(label string) CreatedGuest {
	f.t.Helper()
	g, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Label: label, Origin: schema.GuestOriginAddress,
		Channels: []int64{f.general.ID}, Idle: time.Hour, Absolute: 24 * time.Hour, VoiceAllowed: true})
	if err != nil {
		f.t.Fatalf("CreateGuest: %v", err)
	}
	return g
}

func (f *guestFixture) visit(id int64) GuestVisit {
	f.t.Helper()
	v, err := f.s.GuestVisitOf(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *guestFixture) endedCalls() []EndedGuest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]EndedGuest(nil), f.ended...)
}

func (f *guestFixture) audit(action string) []AuditEvent {
	f.t.Helper()
	evs, err := f.s.ListAuditEvents(f.ctx, 0, 100000)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []AuditEvent
	for _, e := range evs {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func (f *guestFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.db.QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func TestGuestHandlesAreWellFormedAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		h, err := newGuestHandle()
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.ValidateGuestHandle(h); err != nil {
			t.Fatalf("handle %q: %v", h, err)
		}
		if seen[h] {
			t.Fatalf("handle %q drawn twice", h)
		}
		seen[h] = true
	}
}

func TestCreateGuest(t *testing.T) {
	f := newGuestFixture(t)
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	now := t0
	setCredentialClock(t, &now)
	if _, err := f.s.InsertMessage(f.ctx, f.general.ID, f.alice.ID, "before the visit"); err != nil {
		t.Fatal(err)
	}
	var last int64
	if err := f.s.db.QueryRow("SELECT MAX(id) FROM messages WHERE channel_id = ?", f.general.ID).Scan(&last); err != nil {
		t.Fatal(err)
	}
	g, err := f.s.CreateGuest(f.ctx, "invite:7", NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.general.ID, f.lobby.ID, f.general.ID}, Idle: 2 * time.Hour, Absolute: 24 * time.Hour,
		CredentialLifetime: 6 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateGuestHandle(g.Principal.Name); err != nil || g.Visit.Handle != g.Principal.Name {
		t.Fatalf("name %q, handle %q: %v", g.Principal.Name, g.Visit.Handle, err)
	}
	if g.Principal.Kind != PrincipalHuman || g.Principal.Role != RoleMember || !g.Principal.IsGuest() {
		t.Errorf("principal = %+v, want a human member in guest state", g.Principal)
	}
	// The invite's shorter lifetime is the hard deadline, and the credential
	// expires with it.
	if want := t0.Add(6 * time.Hour); !g.Visit.ExpiresAt.Equal(want) || g.Credential.ExpiresAt == nil || !g.Credential.ExpiresAt.Time().Equal(want) {
		t.Errorf("deadline = %v, credential expiry %v, want %v", g.Visit.ExpiresAt, g.Credential.ExpiresAt, want)
	}
	if g.Credential.IdleExpiresAt == nil || !g.Credential.IdleExpiresAt.Time().Equal(t0.Add(2*time.Hour)) || g.Credential.Renews {
		t.Errorf("credential = %+v, want an idle bound two hours out, not renewing", g.Credential)
	}
	if err := g.Visit.V1().Validate(); err != nil {
		t.Errorf("visit does not validate: %v", err)
	}
	p, err := f.s.ResolveCredential(f.ctx, g.Token)
	if err != nil || p.ID != g.Principal.ID || !p.IsGuest() {
		t.Fatalf("resolve = %+v, %v", p, err)
	}
	members, err := f.s.ListChannelMembers(f.ctx, f.general.ID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range members {
		if m.PrincipalID != g.Principal.ID {
			continue
		}
		found = true
		if m.CanDecide || m.HistoryAfter != last || m.Guest == nil || m.Guest.Handle != g.Visit.Handle || m.Guest.Label != "Pat" {
			t.Errorf("guest member = %+v (guest %+v), want no decide right, history after %d and its attribution", m, m.Guest, last)
		}
	}
	if !found {
		t.Fatal("the guest is not a member of general")
	}
	lobby, err := f.s.ListChannelMembers(f.ctx, f.lobby.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range lobby {
		if m.PrincipalID == g.Principal.ID && m.HistoryAfter != 0 {
			t.Errorf("lobby has no messages: history_after = %d, want 0", m.HistoryAfter)
		}
	}
	if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ?", g.Principal.ID); n != 2 {
		t.Errorf("memberships = %d, want 2 (general named twice)", n)
	}
	started := f.audit(AuditGuestStarted)
	if len(started) != 1 || started[0].Actor != "invite:7" || started[0].Detail != "origin=address" {
		t.Errorf("guest_started = %+v", started)
	}
	var made int
	for _, e := range f.audit("credential_created") {
		if e.Subject == principalActor(g.Principal.ID) {
			made++
		}
	}
	if made != 1 {
		t.Errorf("credential_created rows for the guest = %d, want 1", made)
	}
}

func TestCreateGuestRefusals(t *testing.T) {
	f := newGuestFixture(t)
	base := NewGuest{Origin: schema.GuestOriginAddress, Idle: time.Hour, Absolute: time.Hour}
	tests := []struct {
		name string
		edit func(*NewGuest)
		want error
	}{
		{"invite origin without its invite", func(g *NewGuest) { g.Origin = schema.GuestOriginInvite }, nil},
		{"address origin naming an invite", func(g *NewGuest) { g.InviteID = 3 }, nil},
		{"unknown origin", func(g *NewGuest) { g.Origin = "door" }, nil},
		{"no idle bound", func(g *NewGuest) { g.Idle = 0 }, nil},
		{"unknown channel", func(g *NewGuest) { g.Channels = []int64{9999} }, ErrNotFound},
		{"label with the separator", func(g *NewGuest) { g.Label = "Pat · a7k4" }, ErrGuestLabelInvalid},
		{"label in the guest namespace", func(g *NewGuest) { g.Label = "Guest one" }, ErrGuestLabelInvalid},
		{"label a look-alike of the namespace", func(g *NewGuest) { g.Label = "guеst" }, ErrGuestLabelInvalid},
		{"label in the hidden namespace", func(g *NewGuest) { g.Label = "hidden-3" }, ErrGuestLabelInvalid},
		{"label with a bidi control", func(g *NewGuest) { g.Label = "Pat\u202e" }, ErrGuestLabelInvalid},
		{"label of only fillers", func(g *NewGuest) { g.Label = "\u3164" }, ErrGuestLabelInvalid},
		{"label the server's name", func(g *NewGuest) { g.Label = "Conch"; g.ServerName = "conch" }, ErrGuestLabelInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := base
			tt.edit(&g)
			_, err := f.s.CreateGuest(f.ctx, "system", g)
			if err == nil || (tt.want != nil && !errors.Is(err, tt.want)) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if errors.Is(tt.want, ErrGuestLabelInvalid) && strings.Contains(err.Error(), g.Label) {
				t.Errorf("the error %q echoes the label", err)
			}
		})
	}
	if n := f.count("SELECT COUNT(*) FROM guest_visits"); n != 0 {
		t.Errorf("visits after refusals = %d, want none", n)
	}
	if n := f.count("SELECT COUNT(*) FROM principals WHERE name LIKE 'guest-%'"); n != 0 {
		t.Errorf("principals after refusals = %d, want none", n)
	}
}

// Generated handles are unique under concurrency and a visit's handle is
// never given out again, even after it ends.
func TestGuestHandlesUniqueUnderConcurrency(t *testing.T) {
	f := newGuestFixture(t)
	const n = 24
	var wg sync.WaitGroup
	handles := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Origin: schema.GuestOriginAddress,
				Channels: []int64{f.general.ID}, Idle: time.Hour, Absolute: time.Hour})
			handles[i], errs[i] = g.Visit.Handle, err
		}()
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, h := range handles {
		if errs[i] != nil {
			t.Fatalf("guest %d: %v", i, errs[i])
		}
		if seen[h] {
			t.Fatalf("handle %q given twice", h)
		}
		seen[h] = true
	}
	// A handle already used, by a visit long ended, is never drawn again:
	// the store refuses it however it comes up.
	var id int64
	if err := f.s.db.QueryRow("SELECT principal_id FROM guest_visits LIMIT 1").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EndGuestVisit(f.ctx, "system", id, schema.GuestEndOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`INSERT INTO guest_visits (principal_id, handle, origin, state, started_at, expires_at, idle_ms)
		VALUES (?, ?, 'address', 'active', 1, 2, 3)`, f.alice.ID, handles[0]); err == nil || !isUniqueConstraintErr(err) {
		t.Errorf("a second visit with a used handle: %v, want a unique constraint failure", err)
	}
}

// The reserved namespace (design §4.1, B2): every permanent create, rename,
// redemption and bootstrap refuses a name whose folded key or skeleton
// begins with "guest", with no bypass for an operator.
func TestReservedGuestNamespace(t *testing.T) {
	reserved := []string{"guest", "Guest-1", "GUESTS", "guesthouse", "guеst" /* Cyrillic e */, "gսest" /* Armenian u */, "guest-a7k4m2q9b3c5d6e8"}
	allowed := []string{"quest", "gues", "ghost", "a-guest", "g-uest"}
	f := newGuestFixture(t)
	inv := &inviteFixture{t: t, s: f.s, ctx: f.ctx, now: time.Now().Truncate(time.Millisecond), op: f.op}
	open := inv.mustCreate(f.op, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"general"}})
	writes := []struct {
		name  string
		write func(name string) error
	}{
		{"create", func(name string) error {
			_, err := f.s.CreatePrincipalAs(f.ctx, "system", PrincipalHuman, name)
			return err
		}},
		{"create agent", func(name string) error {
			_, err := f.s.CreatePrincipalAs(f.ctx, "system", PrincipalAgent, name)
			return err
		}},
		{"operator rename", func(name string) error {
			_, err := f.s.RenamePrincipal(f.ctx, principalActor(f.op.ID), f.alice.ID, name, RenameLimits{}, time.Now())
			return err
		}},
		{"own rename", func(name string) error {
			_, err := f.s.RenamePrincipal(f.ctx, principalActor(f.alice.ID), f.alice.ID, name, RenameLimits{Renames: 100, Attempts: 100}, time.Now())
			return err
		}},
		{"v1 invite redemption", func(name string) error { _, err := inv.redeem(open.Code, name); return err }},
	}
	for _, w := range writes {
		for _, name := range reserved {
			t.Run(w.name+"/"+name, func(t *testing.T) {
				err := w.write(name)
				if !errors.Is(err, ErrGuestNameReserved) || !errors.Is(err, ErrNameReserved) || !errors.Is(err, ErrDuplicate) {
					t.Fatalf("err = %v, want the guest reservation (also ErrNameReserved and ErrDuplicate)", err)
				}
			})
		}
	}
	for i, name := range allowed {
		if _, err := f.s.CreatePrincipalAs(f.ctx, "system", PrincipalHuman, name); err != nil {
			t.Errorf("create %q: %v", name, err)
		}
		if _, err := inv.redeem(open.Code, fmt.Sprintf("%s%d", name, i)); err != nil {
			t.Errorf("redeem as %q: %v", name, err)
		}
	}
	// No principal was made under a reserved name, and alice kept hers.
	if ids, err := f.s.ReservedGuestNames(f.ctx); err != nil || len(ids) != 0 {
		t.Errorf("reserved names in use = %v, %v; want none", ids, err)
	}
	if p, err := f.s.PrincipalByID(f.ctx, f.alice.ID); err != nil || p.Name != "alice" {
		t.Errorf("alice = %+v, %v", p, err)
	}
	// The first operator is held to it too.
	s2 := openTestStore(t)
	if _, err := s2.BootstrapOperatorWith(f.ctx, "Guest-admin", BootstrapOptions{}); !errors.Is(err, ErrGuestNameReserved) {
		t.Errorf("bootstrap as Guest-admin: %v, want the guest reservation", err)
	}
	// A guest's own handle is the one name in the namespace the store
	// writes, through CreateGuest alone.
	g := f.guest("")
	if !strings.HasPrefix(g.Principal.Name, schema.GuestNamePrefix) {
		t.Errorf("guest name %q", g.Principal.Name)
	}
}

// A database from before migration 31 keeps every name, and the names now in
// the namespace are reported; nothing is renamed, hidden or reclassified.
func TestGuestMigrationFromSchema30(t *testing.T) {
	const pre = 30
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
	for _, stmt := range []string{
		`INSERT INTO principals (id, kind, name, name_key, name_skel, role, created_at) VALUES
			(1, 'human', 'root', 'root', 'root', 'operator', 1),
			(2, 'human', 'guest-a', 'guest-a', 'guest-a', 'member', 2),
			(3, 'human', 'Guests', 'guests', 'guests', 'member', 3),
			(4, 'human', 'alice', 'alice', 'alice', 'member', 4)`,
		`INSERT INTO channels (id, name, created_at) VALUES (1, 'general', 1)`,
		`INSERT INTO channel_members (channel_id, principal_id, created_at) VALUES (1, 4, 5)`,
		`INSERT INTO credentials (id, principal_id, label, token_hash, created_at) VALUES (1, 4, 'old', 'h1', 6)`,
		`INSERT INTO invites (id, kind, code_hash, created_by, created_at, expires_at, credential_lifetime_ms) VALUES (1, 'open', 'c1', 1, 7, 8, 9)`,
		`INSERT INTO messages (id, channel_id, author_id, body, created_at) VALUES (1, 1, 4, 'old', 10)`,
		`PRAGMA user_version = 30`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s := openTestStoreAt(t, path)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < 31 {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	ids, err := s.ReservedGuestNames(ctx)
	if err != nil || fmt.Sprint(ids) != "[2 3]" {
		t.Errorf("reserved names in use = %v, %v; want [2 3]", ids, err)
	}
	for id, name := range map[int64]string{2: "guest-a", 3: "Guests", 4: "alice"} {
		p, err := s.PrincipalByID(ctx, id)
		if err != nil || p.Name != name || p.GuestState != "" || p.HiddenAt != nil || p.DisabledAt != nil {
			t.Errorf("principal %d = %+v, %v; want %q, untouched and no guest", id, p, err, name)
		}
	}
	for _, c := range []struct {
		q    string
		want int
	}{
		{"SELECT COUNT(*) FROM channel_members WHERE history_after IS NULL", 1},
		{"SELECT COUNT(*) FROM credentials WHERE idle_ms IS NULL AND last_used_at IS NULL", 1},
		{"SELECT COUNT(*) FROM invites WHERE join_as = 'account'", 1},
		{"SELECT COUNT(*) FROM guest_visits", 0},
		{"SELECT COUNT(*) FROM message_guest_attribution", 0},
		{"SELECT COUNT(*) FROM open_channels", 0},
	} {
		var n int
		if err := s.db.QueryRow(c.q).Scan(&n); err != nil || n != c.want {
			t.Errorf("%s = %d, %v; want %d", c.q, n, err, c.want)
		}
	}
	// An old reserved name may still be renamed out of the namespace by an
	// operator, and not to another name in it.
	if _, err := s.RenamePrincipal(ctx, "principal:1", 2, "guest-b", RenameLimits{}, time.Now()); !errors.Is(err, ErrGuestNameReserved) {
		t.Errorf("rename to guest-b: %v, want the guest reservation", err)
	}
	if _, err := s.RenamePrincipal(ctx, "principal:1", 2, "visitor-a", RenameLimits{}, time.Now()); err != nil {
		t.Errorf("rename out of the namespace: %v", err)
	}
}

// Posts keep their guest attribution through End visit, conversion and the
// hide of the member the guest became; posts made after conversion carry
// none; and the attribution cannot be changed or removed.
func TestGuestAttributionIsImmutable(t *testing.T) {
	f := newGuestFixture(t)
	g := f.guest("Pat")
	want := schema.GuestAttributionV1{Handle: g.Visit.Handle, Label: "Pat", Tag: schema.GuestTag(g.Visit.Handle)}
	posted, err := f.s.InsertMessage(f.ctx, f.general.ID, g.Principal.ID, "hello as a guest")
	if err != nil {
		t.Fatal(err)
	}
	if posted.GuestAuthor == nil || *posted.GuestAuthor != want {
		t.Fatalf("posted guest_author = %+v, want %+v", posted.GuestAuthor, want)
	}
	reply, _, err := f.s.InsertReplyMessage(f.ctx, ReplyPost{ChannelID: f.general.ID, AuthorID: g.Principal.ID, RootID: posted.ID, Body: "and again"},
		Reader{Scoped: true, PrincipalID: g.Principal.ID})
	if err != nil {
		t.Fatal(err)
	}
	if reply.GuestAuthor == nil || *reply.GuestAuthor != want {
		t.Fatalf("reply guest_author = %+v", reply.GuestAuthor)
	}
	aliceMsg, err := f.s.InsertMessage(f.ctx, f.general.ID, f.alice.ID, "a member's post")
	if err != nil {
		t.Fatal(err)
	}
	if aliceMsg.GuestAuthor != nil {
		t.Errorf("member post carries guest_author %+v", aliceMsg.GuestAuthor)
	}
	read := func(stage string) {
		t.Helper()
		msgs, err := f.s.ListVisibleMessages(f.ctx, f.general.ID, Reader{Scoped: true, PrincipalID: f.alice.ID}, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		got := map[int64]*schema.GuestAttributionV1{}
		for _, m := range msgs {
			got[m.ID] = m.GuestAuthor
		}
		for _, id := range []int64{posted.ID, reply.ID} {
			if got[id] == nil || *got[id] != want {
				t.Errorf("%s: message %d guest_author = %+v, want %+v", stage, id, got[id], want)
			}
		}
		if got[aliceMsg.ID] != nil {
			t.Errorf("%s: alice's message has guest_author", stage)
		}
	}
	read("live")

	conv, err := f.s.ConvertGuest(f.ctx, principalActor(g.Principal.ID), GuestConversion{PrincipalID: g.Principal.ID, Name: "pat",
		CredentialLabel: "member", CredentialLifetime: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if conv.Principal.ID != g.Principal.ID || conv.Principal.Name != "pat" || conv.Principal.IsGuest() {
		t.Fatalf("converted = %+v", conv.Principal)
	}
	read("converted")
	after, err := f.s.InsertMessage(f.ctx, f.general.ID, g.Principal.ID, "as a member")
	if err != nil {
		t.Fatal(err)
	}
	if after.GuestAuthor != nil {
		t.Errorf("post after conversion carries guest_author %+v", after.GuestAuthor)
	}
	if _, err := f.s.HidePrincipal(f.ctx, principalActor(f.op.ID), g.Principal.ID, HideOptions{}); err != nil {
		t.Fatal(err)
	}
	read("hidden")

	for _, q := range []string{
		"UPDATE message_guest_attribution SET label = 'Mallory'",
		"DELETE FROM message_guest_attribution",
	} {
		if _, err := f.s.db.Exec(q); err == nil {
			t.Errorf("%s succeeded; attribution must be immutable", q)
		}
	}
}

// A visit that ended keeps its posts' attribution, and its principal is
// neither renamed nor hidden.
func TestEndedGuestKeepsAttribution(t *testing.T) {
	f := newGuestFixture(t)
	g := f.guest("")
	m, err := f.s.InsertMessage(f.ctx, f.general.ID, g.Principal.ID, "hi")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.EndGuestVisit(f.ctx, principalActor(g.Principal.ID), g.Principal.ID, schema.GuestEndVisit); err != nil {
		t.Fatal(err)
	}
	msgs, err := f.s.ListVisibleMessages(f.ctx, f.general.ID, Reader{Scoped: true, PrincipalID: f.alice.ID}, 0, 10)
	if err != nil || len(msgs) != 1 || msgs[0].ID != m.ID || msgs[0].GuestAuthor == nil || msgs[0].GuestAuthor.Handle != g.Visit.Handle || msgs[0].GuestAuthor.Label != "" {
		t.Fatalf("messages after end = %+v, %v", msgs, err)
	}
	if _, err := f.s.HidePrincipal(f.ctx, principalActor(f.op.ID), g.Principal.ID, HideOptions{}); !errors.Is(err, ErrGuestNotHideable) {
		t.Errorf("hide an ended guest: %v, want ErrGuestNotHideable", err)
	}
	if p, err := f.s.PrincipalByID(f.ctx, g.Principal.ID); err != nil || p.Name != g.Visit.Handle {
		t.Errorf("principal = %+v, %v; want its handle", p, err)
	}
}

// Idle expiry: only NoteGuestUse moves it, at most once a step, never
// past the hard deadline, and never for a credential already idle; access is
// refused at the deadline whether or not a sweep has run.
func TestGuestIdleBound(t *testing.T) {
	f := newGuestFixture(t)
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	now := t0
	setCredentialClock(t, &now)
	g, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Origin: schema.GuestOriginAddress, Channels: []int64{f.general.ID},
		Idle: 10 * time.Minute, Absolute: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	live := func() bool {
		t.Helper()
		_, err := f.s.ResolveCredential(f.ctx, g.Token)
		ok, lerr := f.s.CredentialLive(f.ctx, g.Credential.ID)
		if lerr != nil {
			t.Fatal(lerr)
		}
		if (err == nil) != ok {
			t.Fatalf("resolve says %v, CredentialLive says %v", err, ok)
		}
		if err != nil && !errors.Is(err, ErrCredentialInvalid) {
			t.Fatal(err)
		}
		return ok
	}
	steps := []struct {
		at       time.Duration
		use      bool
		wantLive bool
		wantIdle time.Duration // where the idle expiry stands after, from t0
	}{
		{at: 30 * time.Second, use: true, wantLive: true, wantIdle: 10 * time.Minute}, // within the step of creation: no write
		{at: 9 * time.Minute, use: true, wantLive: true, wantIdle: 19 * time.Minute},  // moves it
		{at: 9*time.Minute + 30*time.Second, use: true, wantLive: true, wantIdle: 19 * time.Minute},
		{at: 18 * time.Minute, use: false, wantLive: true, wantIdle: 19 * time.Minute},
		{at: 19 * time.Minute, use: false, wantLive: false, wantIdle: 19 * time.Minute},            // idle: refused before any sweep
		{at: 19*time.Minute + time.Second, use: true, wantLive: false, wantIdle: 19 * time.Minute}, // a use cannot revive it
	}
	for _, st := range steps {
		now = t0.Add(st.at)
		if got := live(); got != st.wantLive {
			t.Fatalf("at %v: live = %v, want %v", st.at, got, st.wantLive)
		}
		if st.use {
			f.s.NoteGuestUse(f.ctx, g.Credential.ID)
		}
		v := f.visit(g.Principal.ID)
		if want := t0.Add(st.wantIdle); !v.IdleExpiresAt.Equal(want) {
			t.Fatalf("at %v: idle expiry = %v, want %v", st.at, v.IdleExpiresAt, want)
		}
		if v.State != schema.GuestStateActive {
			t.Fatalf("at %v: state %s before any sweep", st.at, v.State)
		}
	}
	ended, err := f.s.SweepGuests(f.ctx)
	if err != nil || len(ended) != 1 || ended[0].Reason != schema.GuestEndIdle || ended[0].PrincipalID != g.Principal.ID {
		t.Fatalf("sweep = %+v, %v; want the guest ended as idle", ended, err)
	}

	// The hard deadline holds against a busy guest: activity never moves
	// the idle expiry past it.
	now = t0
	h, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Origin: schema.GuestOriginAddress, Idle: 10 * time.Minute, Absolute: 15 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	now = t0.Add(9 * time.Minute)
	f.s.NoteGuestUse(f.ctx, h.Credential.ID)
	if v := f.visit(h.Principal.ID); !v.IdleExpiresAt.Equal(t0.Add(15 * time.Minute)) {
		t.Errorf("idle expiry = %v, want capped at the deadline %v", v.IdleExpiresAt, t0.Add(15*time.Minute))
	}
	now = t0.Add(15 * time.Minute)
	if _, err := f.s.ResolveCredential(f.ctx, h.Token); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("at the deadline: %v, want refused", err)
	}
	ended, err = f.s.SweepGuests(f.ctx)
	if err != nil || len(ended) != 1 || ended[0].Reason != schema.GuestEndExpired {
		t.Fatalf("sweep at the deadline = %+v, %v; want ended as expired", ended, err)
	}
}

// A visit past its deadline while the server is down is refused as soon as
// it opens again, and the first sweep ends it.
func TestGuestExpiryAcrossRestart(t *testing.T) {
	path := newTestDBPath(t)
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	now := t0
	setCredentialClock(t, &now)
	ctx := context.Background()
	g, err := s.CreateGuest(ctx, "system", NewGuest{Origin: schema.GuestOriginAddress, Idle: time.Hour, Absolute: 2 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	now = t0.Add(3 * time.Hour)
	s = openTestStoreAt(t, path)
	if _, err := s.ResolveCredential(ctx, g.Token); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("after restart past the deadline: %v, want refused", err)
	}
	if n, err := s.LiveGuests(ctx); err != nil || n != 0 {
		t.Errorf("live guests = %d, %v; want 0", n, err)
	}
	ended, err := s.SweepGuests(ctx)
	if err != nil || len(ended) != 1 || ended[0].Reason != schema.GuestEndExpired {
		t.Fatalf("first sweep = %+v, %v", ended, err)
	}
}

// Terminal guests do not occupy the cap, and the cap counts in the creating
// transaction.
func TestGuestCapCountsLiveVisitsOnly(t *testing.T) {
	f := newGuestFixture(t)
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	now := t0
	setCredentialClock(t, &now)
	mk := func(idle time.Duration) (CreatedGuest, error) {
		return f.s.CreateGuest(f.ctx, "system", NewGuest{Origin: schema.GuestOriginAddress, Idle: idle, Absolute: 24 * time.Hour, MaxLive: 2})
	}
	a, err := mk(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b, err := mk(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mk(time.Hour); !errors.Is(err, ErrGuestsFull) {
		t.Fatalf("third live visit: %v, want ErrGuestsFull", err)
	}
	// a goes idle: it no longer counts, before any sweep.
	now = t0.Add(2 * time.Minute)
	c, err := mk(time.Hour)
	if err != nil {
		t.Fatalf("with one visit idle: %v", err)
	}
	// b ends: it frees its place too.
	if _, err := f.s.EndGuestVisit(f.ctx, "system", b.Principal.ID, schema.GuestEndOperator); err != nil {
		t.Fatal(err)
	}
	if _, err := mk(time.Hour); err != nil {
		t.Fatalf("after an end: %v", err)
	}
	if _, err := mk(time.Hour); !errors.Is(err, ErrGuestsFull) {
		t.Fatalf("full again: %v", err)
	}
	if n, err := f.s.LiveGuests(f.ctx); err != nil || n != 2 {
		t.Errorf("live = %d, %v; want 2", n, err)
	}
	_ = a
	_ = c

	// Concurrent creations never overshoot the cap.
	g := newGuestFixture(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	made := 0
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.s.CreateGuest(g.ctx, "system", NewGuest{Origin: schema.GuestOriginAddress, Idle: time.Hour, Absolute: time.Hour, MaxLive: 5})
			if err == nil {
				mu.Lock()
				made++
				mu.Unlock()
			} else if !errors.Is(err, ErrGuestsFull) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if made != 5 {
		t.Errorf("made %d visits under a cap of 5", made)
	}
}

// Every way a visit ends, with its reason; each removes its memberships and
// its credential in the same transaction, tells OnGuestsEnded, and leaves a
// visit nothing revives.
func TestGuestEndPaths(t *testing.T) {
	tests := []struct {
		name   string
		end    func(f *guestFixture, g CreatedGuest) error
		reason schema.GuestEndReason
		actor  func(f *guestFixture, g CreatedGuest) string
	}{
		{"end visit", func(f *guestFixture, g CreatedGuest) error {
			_, err := f.s.EndGuestVisit(f.ctx, principalActor(g.Principal.ID), g.Principal.ID, schema.GuestEndVisit)
			return err
		}, schema.GuestEndVisit, func(_ *guestFixture, g CreatedGuest) string { return principalActor(g.Principal.ID) }},
		{"sign out", func(f *guestFixture, g CreatedGuest) error {
			return f.s.RevokeOwnCredential(f.ctx, principalActor(g.Principal.ID), g.Credential.ID)
		}, schema.GuestEndVisit, func(_ *guestFixture, g CreatedGuest) string { return principalActor(g.Principal.ID) }},
		{"end its sign-in by id", func(f *guestFixture, g CreatedGuest) error {
			return f.s.RevokeCredentialOf(f.ctx, principalActor(g.Principal.ID), g.Principal.ID, g.Credential.ID)
		}, schema.GuestEndVisit, func(_ *guestFixture, g CreatedGuest) string { return principalActor(g.Principal.ID) }},
		{"sign out everywhere", func(f *guestFixture, g CreatedGuest) error {
			_, err := f.s.RevokeAllCredentials(f.ctx, principalActor(g.Principal.ID), g.Principal.ID)
			return err
		}, schema.GuestEndVisit, func(_ *guestFixture, g CreatedGuest) string { return principalActor(g.Principal.ID) }},
		{"operator revokes", func(f *guestFixture, g CreatedGuest) error {
			return f.s.RevokeCredential(f.ctx, principalActor(f.op.ID), g.Credential.ID)
		}, schema.GuestEndOperator, func(f *guestFixture, _ CreatedGuest) string { return principalActor(f.op.ID) }},
		{"operator revokes all", func(f *guestFixture, g CreatedGuest) error {
			_, err := f.s.RevokeAllCredentials(f.ctx, principalActor(f.op.ID), g.Principal.ID)
			return err
		}, schema.GuestEndOperator, func(f *guestFixture, _ CreatedGuest) string { return principalActor(f.op.ID) }},
		{"operator disables", func(f *guestFixture, g CreatedGuest) error {
			_, err := f.s.DisablePrincipal(f.ctx, principalActor(f.op.ID), g.Principal.ID)
			return err
		}, schema.GuestEndOperator, func(f *guestFixture, _ CreatedGuest) string { return principalActor(f.op.ID) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newGuestFixture(t)
			g, err := f.s.CreateGuest(f.ctx, "system", NewGuest{Origin: schema.GuestOriginAddress,
				Channels: []int64{f.general.ID, f.lobby.ID}, Idle: time.Hour, Absolute: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.end(f, g); err != nil {
				t.Fatal(err)
			}
			v := f.visit(g.Principal.ID)
			if v.State != schema.GuestStateEnded || v.EndReason != tt.reason || v.EndedAt == nil {
				t.Fatalf("visit = %+v, want ended with %s", v, tt.reason)
			}
			if err := v.V1().Validate(); err != nil {
				t.Errorf("ended visit does not validate: %v", err)
			}
			if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ?", g.Principal.ID); n != 0 {
				t.Errorf("memberships after the end = %d", n)
			}
			if n := f.count("SELECT COUNT(*) FROM credentials WHERE principal_id = ? AND revoked_at IS NULL", g.Principal.ID); n != 0 {
				t.Errorf("live credentials after the end = %d", n)
			}
			if _, err := f.s.ResolveCredential(f.ctx, g.Token); !errors.Is(err, ErrCredentialInvalid) {
				t.Errorf("resolve after the end: %v", err)
			}
			rows := f.audit(AuditGuestEnded)
			if len(rows) != 1 || rows[0].Detail != "reason="+string(tt.reason) || rows[0].Actor != tt.actor(f, g) {
				t.Errorf("guest_ended rows = %+v", rows)
			}
			calls := f.endedCalls()
			if len(calls) != 1 || calls[0].PrincipalID != g.Principal.ID || fmt.Sprint(calls[0].Channels) != fmt.Sprint([]int64{f.general.ID, f.lobby.ID}) {
				t.Errorf("OnGuestsEnded calls = %+v", calls)
			}
			// Nothing brings it back: not an enable, not a credential, not
			// a second end, not a conversion, not a membership.
			if _, err := f.s.EnablePrincipal(f.ctx, "system", g.Principal.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := f.s.CreateCredential(f.ctx, "system", g.Principal.ID, "again", nil); !errors.Is(err, ErrGuestNotAllowed) {
				t.Errorf("credential for an ended guest: %v, want ErrGuestNotAllowed", err)
			}
			if _, err := f.s.EndGuestVisit(f.ctx, "system", g.Principal.ID, schema.GuestEndOperator); !errors.Is(err, ErrGuestEnded) {
				t.Errorf("second end: %v, want ErrGuestEnded", err)
			}
			if _, err := f.s.ConvertGuest(f.ctx, "system", GuestConversion{PrincipalID: g.Principal.ID, Name: "late",
				CredentialLabel: "m", CredentialLifetime: time.Hour}); !errors.Is(err, ErrGuestEnded) {
				t.Errorf("convert after the end: %v, want ErrGuestEnded", err)
			}
			if _, err := f.s.AddChannelMember(f.ctx, "system", f.general.ID, g.Principal.ID, 0); !errors.Is(err, ErrGuestEnded) {
				t.Errorf("add an ended guest: %v, want ErrGuestEnded", err)
			}
			if _, err := f.s.db.Exec("UPDATE guest_visits SET state = 'active', ended_at = NULL, end_reason = NULL WHERE principal_id = ?", g.Principal.ID); err == nil {
				t.Error("a direct update revived the visit; the trigger must refuse it")
			}
			if len(f.endedCalls()) != 1 {
				t.Errorf("OnGuestsEnded called again: %+v", f.endedCalls())
			}
		})
	}
}

// Of an end and a conversion racing on one visit, exactly one wins, and the
// loser writes nothing.
func TestGuestTerminalTransitionRace(t *testing.T) {
	f := newGuestFixture(t)
	for i := range 20 {
		g := f.guest("")
		var wg sync.WaitGroup
		var endErr, convErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, endErr = f.s.EndGuestVisit(f.ctx, "system", g.Principal.ID, schema.GuestEndVisit)
		}()
		go func() {
			defer wg.Done()
			<-start
			_, convErr = f.s.ConvertGuest(f.ctx, "system", GuestConversion{PrincipalID: g.Principal.ID, Name: fmt.Sprintf("member%d", i),
				CredentialLabel: "m", CredentialLifetime: time.Hour})
		}()
		close(start)
		wg.Wait()
		if (endErr == nil) == (convErr == nil) {
			t.Fatalf("round %d: end %v, convert %v; want exactly one to win", i, endErr, convErr)
		}
		loser := endErr
		if loser == nil {
			loser = convErr
		}
		if !errors.Is(loser, ErrGuestEnded) {
			t.Fatalf("round %d: loser's error %v, want ErrGuestEnded", i, loser)
		}
		v := f.visit(g.Principal.ID)
		p, err := f.s.PrincipalByID(f.ctx, g.Principal.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch endErr {
		case nil:
			if v.State != schema.GuestStateEnded || p.Name != g.Visit.Handle {
				t.Fatalf("round %d: end won but visit %s, name %q", i, v.State, p.Name)
			}
		default:
			if v.State != schema.GuestStateConverted || p.Name != fmt.Sprintf("member%d", i) ||
				f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ?", g.Principal.ID) != 1 {
				t.Fatalf("round %d: conversion won but visit %s, name %q", i, v.State, p.Name)
			}
		}
	}
}

// Conversion keeps the principal, its memberships and history bounds, gives
// no right, refuses a reserved or taken name and a duplicate request, and
// rolls back whole when the work inside it fails.
func TestConvertGuest(t *testing.T) {
	f := newGuestFixture(t)
	if _, err := f.s.InsertMessage(f.ctx, f.general.ID, f.alice.ID, "old"); err != nil {
		t.Fatal(err)
	}
	g := f.guest("Pat")
	conv := GuestConversion{PrincipalID: g.Principal.ID, CredentialLabel: "member", CredentialLifetime: time.Hour}
	for _, name := range []string{"Guest-pat", "alice", "hidden-9"} {
		c := conv
		c.Name = name
		if _, err := f.s.ConvertGuest(f.ctx, "system", c); !errors.Is(err, ErrDuplicate) {
			t.Errorf("convert to %q: %v, want refused as taken or reserved", name, err)
		}
	}
	c := conv
	c.Name = "pat"
	c.within = func(context.Context, execer) error { return errors.New("the password could not be stored") }
	if _, err := f.s.ConvertGuest(f.ctx, "system", c); err == nil {
		t.Fatal("a failure inside the conversion was not returned")
	}
	if v := f.visit(g.Principal.ID); v.State != schema.GuestStateActive {
		t.Fatalf("after a failed conversion: %s", v.State)
	}
	if _, err := f.s.ResolveCredential(f.ctx, g.Token); err != nil {
		t.Fatalf("the guest token after a failed conversion: %v", err)
	}
	before := f.count("SELECT history_after FROM channel_members WHERE principal_id = ?", g.Principal.ID)
	c.within = nil
	out, err := f.s.ConvertGuest(f.ctx, principalActor(g.Principal.ID), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.ResolveCredential(f.ctx, g.Token); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("the old guest token after conversion: %v, want refused", err)
	}
	p, err := f.s.ResolveCredential(f.ctx, out.Token)
	if err != nil || p.ID != g.Principal.ID || p.IsGuest() || p.GuestState != schema.GuestStateConverted || p.Name != "pat" {
		t.Fatalf("the member token: %+v, %v", p, err)
	}
	if after := f.count("SELECT history_after FROM channel_members WHERE principal_id = ?", g.Principal.ID); after != before || after == 0 {
		t.Errorf("history bound %d after conversion, %d before; want kept", after, before)
	}
	if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ? AND can_decide = 1", g.Principal.ID); n != 0 {
		t.Errorf("conversion gave the decide right in %d channels", n)
	}
	if _, err := f.s.ConvertGuest(f.ctx, "system", c); !errors.Is(err, ErrGuestEnded) {
		t.Errorf("a duplicate conversion: %v, want ErrGuestEnded", err)
	}
	if rows := f.audit(AuditGuestConverted); len(rows) != 1 || !strings.Contains(rows[0].Detail, g.Visit.Handle) {
		t.Errorf("guest_converted rows = %+v", rows)
	}
	// A converted member is a member: it may be renamed, given credentials
	// and the right to decide, through the ordinary audited writes.
	if _, _, err := f.s.CreateCredential(f.ctx, "system", g.Principal.ID, "laptop", nil); err != nil {
		t.Errorf("credential for a converted member: %v", err)
	}
	if _, err := f.s.SetChannelMemberCanDecide(f.ctx, "system", f.general.ID, g.Principal.ID, true); err != nil {
		t.Errorf("grant to a converted member: %v", err)
	}
}

// What a guest may not be given or have done to it, by anyone.
func TestGuestRefusals(t *testing.T) {
	f := newGuestFixture(t)
	g := f.guest("")
	yes, no := true, false
	tests := []struct {
		name string
		do   func() error
		want error
	}{
		{"a second credential", func() error {
			_, _, err := f.s.CreateCredential(f.ctx, "system", g.Principal.ID, "x", nil)
			return err
		}, ErrGuestNotAllowed},
		{"a renewable credential", func() error {
			_, _, err := f.s.CreateRenewableCredential(f.ctx, "system", g.Principal.ID, "x", 72*time.Hour)
			return err
		}, ErrGuestNotAllowed},
		{"a rotation", func() error {
			_, _, err := f.s.RotateCredential(f.ctx, "system", g.Credential.ID)
			return err
		}, ErrGuestNotAllowed},
		{"a sign-in link", func() error {
			_, err := f.s.CreateSignInLink(f.ctx, principalActor(f.op.ID), f.op.ID, g.Principal.ID, schema.CreateSignInLinkRequestV1{}, time.Now())
			return err
		}, ErrSignInLinkTarget},
		{"a rename by an operator", func() error {
			_, err := f.s.RenamePrincipal(f.ctx, principalActor(f.op.ID), g.Principal.ID, "pat", RenameLimits{}, time.Now())
			return err
		}, ErrGuestNotHideable},
		{"a rename by itself, to its own name", func() error {
			_, err := f.s.RenamePrincipal(f.ctx, principalActor(g.Principal.ID), g.Principal.ID, g.Visit.Handle, RenameLimits{Renames: 1, Attempts: 1}, time.Now())
			return err
		}, ErrGuestNotHideable},
		{"a hide", func() error {
			_, err := f.s.HidePrincipal(f.ctx, principalActor(f.op.ID), g.Principal.ID, HideOptions{})
			return err
		}, ErrGuestNotHideable},
		{"the right to decide on admission", func() error {
			_, err := f.s.PutChannelMember(f.ctx, "system", f.lobby.ID, g.Principal.ID, 0, &yes)
			return err
		}, ErrGuestNotAllowed},
		{"the right to decide as a member", func() error {
			_, err := f.s.SetChannelMemberCanDecide(f.ctx, "system", f.general.ID, g.Principal.ID, true)
			return err
		}, ErrGuestNotAllowed},
		{"the right to decide through put", func() error {
			_, err := f.s.PutChannelMember(f.ctx, "system", f.general.ID, g.Principal.ID, 0, &yes)
			return err
		}, ErrGuestNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.do(); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if n := f.count("SELECT COUNT(*) FROM credentials WHERE principal_id = ?", g.Principal.ID); n != 1 {
		t.Errorf("credentials = %d, want the one", n)
	}
	if n := f.count("SELECT COUNT(*) FROM channel_members WHERE principal_id = ? AND can_decide = 1", g.Principal.ID); n != 0 {
		t.Errorf("decide rights = %d, want none", n)
	}
	// An operator may admit the guest to another channel, without the right
	// and with a fresh history bound; an explicit false is fine.
	if _, err := f.s.InsertMessage(f.ctx, f.lobby.ID, f.alice.ID, "lobby, before"); err != nil {
		t.Fatal(err)
	}
	if added, err := f.s.PutChannelMember(f.ctx, "system", f.lobby.ID, g.Principal.ID, f.op.ID, nil); err != nil || !added {
		t.Fatalf("admit to lobby: %v, %v", added, err)
	}
	if added, err := f.s.PutChannelMember(f.ctx, "system", f.lobby.ID, g.Principal.ID, f.op.ID, &no); err != nil || added {
		t.Fatalf("re-admit with false: %v, %v", added, err)
	}
	var decide int
	var after sql.NullInt64
	if err := f.s.db.QueryRow("SELECT can_decide, history_after FROM channel_members WHERE channel_id = ? AND principal_id = ?",
		f.lobby.ID, g.Principal.ID).Scan(&decide, &after); err != nil {
		t.Fatal(err)
	}
	if decide != 0 || !after.Valid || after.Int64 == 0 {
		t.Errorf("lobby membership: can_decide %d, history_after %v; want 0 and a bound", decide, after)
	}
	// A bulk hide never selects a guest.
	targets, _, err := f.s.HideTargets(f.ctx, HideSelector{NamePrefix: schema.GuestNamePrefix}, f.op.ID, 10)
	if err != nil || len(targets) != 0 {
		t.Errorf("bulk hide targets = %+v, %v; want none", targets, err)
	}
}

// What the operator's lists show of guests.
func TestGuestListings(t *testing.T) {
	f := newGuestFixture(t)
	g := f.guest("")
	ended := f.guest("")
	if _, err := f.s.EndGuestVisit(f.ctx, "system", ended.Principal.ID, schema.GuestEndOperator); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		state schema.GuestState
		want  []int64
	}{
		{schema.GuestStateActive, []int64{g.Principal.ID}},
		{schema.GuestStateEnded, []int64{ended.Principal.ID}},
		{schema.GuestStateConverted, nil},
	} {
		rows, err := f.s.ListPrincipals(f.ctx, PrincipalFilter{Guest: c.state, Limit: 10}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		var got []int64
		for _, r := range rows {
			got = append(got, r.ID)
			if r.GuestState != c.state {
				t.Errorf("guest_state = %q, want %q", r.GuestState, c.state)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("?guest=%s lists %v, want %v", c.state, got, c.want)
		}
	}
	row, err := f.s.PrincipalAdmin(f.ctx, f.alice.ID, time.Now())
	if err != nil || row.GuestState != "" {
		t.Errorf("alice = %+v, %v; want no guest state", row, err)
	}
	creds, err := f.s.ListLiveCredentials(f.ctx, g.Principal.ID)
	if err != nil || len(creds) != 1 || creds[0].IdleExpiresAt == nil {
		t.Errorf("own credentials = %+v, %v; want the one, with its idle expiry", creds, err)
	}
	next, ok, err := f.s.NextGuestDeadline(f.ctx)
	if err != nil || !ok || !next.Equal(g.Visit.IdleExpiresAt) {
		t.Errorf("next deadline = %v, %v, %v; want %v", next, ok, err, g.Visit.IdleExpiresAt)
	}
}
