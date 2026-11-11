package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// issuedBy reads the issued_by column of one credential or hook row: the id,
// or 0 for NULL.
func issuedBy(t *testing.T, s *Store, table string, id int64) int64 {
	t.Helper()
	var v sql.NullInt64
	if err := s.db.QueryRow("SELECT issued_by FROM "+table+" WHERE id = ?", id).Scan(&v); err != nil {
		t.Fatalf("read %s %d: %v", table, id, err)
	}
	return v.Int64
}

// Every issuing path records its issuer (issue #1024): a create and a
// rotate by an operator name the operator, a create by the system names
// nobody, a hook names whoever bound it, and a rotation by someone other
// than the maker names the rotator on the replacement row.
func TestIssuedByWrittenByEveryPath(t *testing.T) {
	f := newPermFixture(t)
	op1, op2, member := f.ids["operator-1"], f.ids["operator-no-invite"], f.ids["member-1"]
	ch, err := f.s.ChannelByName(f.ctx, "channel-a")
	if err != nil {
		t.Fatal(err)
	}

	byOp, _, err := f.s.CreateCredential(f.ctx, principalActor(op1), member, "by operator-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	bySystem, _, err := f.s.CreateCredential(f.ctx, "system", member, "by nobody", nil)
	if err != nil {
		t.Fatal(err)
	}
	own, _, err := f.s.CreateCredential(f.ctx, principalActor(op1), op1, "its own", nil)
	if err != nil {
		t.Fatal(err)
	}
	rotated, _, err := f.s.RotateCredential(f.ctx, principalActor(op2), byOp.ID)
	if err != nil {
		t.Fatal(err)
	}
	hook, err := f.s.CreateHook(f.ctx, principalActor(op1), "issued-by-hook-token", ch.ID, member)
	if err != nil {
		t.Fatal(err)
	}
	systemHook, err := f.s.CreateHook(f.ctx, "system", "issued-by-system-hook-token", ch.ID, member)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		table string
		id    int64
		want  int64
	}{
		{"created by an operator", "credentials", byOp.ID, op1},
		{"created by the system", "credentials", bySystem.ID, 0},
		{"an operator's own", "credentials", own.ID, op1},
		{"rotated by another operator", "credentials", rotated.ID, op2},
		{"a hook bound by an operator", "hooks", hook.ID, op1},
		{"a hook bound by the system", "hooks", systemHook.ID, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := issuedBy(t, f.s, tt.table, tt.id); got != tt.want {
				t.Errorf("issued_by = %d, want %d", got, tt.want)
			}
		})
	}
	// The rotation revoked the original: its issuer stands, unread.
	if got := issuedBy(t, f.s, "credentials", byOp.ID); got != op1 {
		t.Errorf("the replaced credential's issued_by = %d, want %d", got, op1)
	}
}

// auditJoinIssuedToOthers is what IssuedToOthers answered before migration
// 42, joined from the audit trail (the A3b implementation, kept here as the
// oracle): the live credentials and hooks whose credential_created,
// credential_rotated or hook_created row names a principal actor other
// than the subject, the issuer not an owner, each once at its first row.
func auditJoinIssuedToOthers(t *testing.T, s *Store, now time.Time) []schema.IssuedByOperatorV1 {
	t.Helper()
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx,
		`SELECT actor, subject, detail, created_at FROM audit_events
		 WHERE action IN ('credential_created', 'credential_rotated', 'hook_created')
		   AND actor LIKE 'principal:%' AND subject LIKE 'principal:%' AND actor <> subject
		 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	type issued struct {
		kind             schema.IssuedKind
		id, target, from int64
		at               int64
	}
	var found []issued
	for rows.Next() {
		var actor, subject, detail string
		var at int64
		if err := rows.Scan(&actor, &subject, &detail, &at); err != nil {
			t.Fatal(err)
		}
		from, _ := principalOfActor(actor)
		target, _ := principalOfActor(subject)
		m := issuedDetail.FindStringSubmatch(detail)
		if m == nil {
			continue
		}
		id, _ := strconv.ParseInt(m[2], 10, 64)
		found = append(found, issued{kind: schema.IssuedKind(m[1]), id: id, target: target, from: from, at: at})
	}
	_ = rows.Close()
	out := []schema.IssuedByOperatorV1{}
	seen := map[string]bool{}
	for _, it := range found {
		key := string(it.kind) + ":" + strconv.FormatInt(it.id, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		p, ok, err := queryPrincipal(ctx, s.db, it.from)
		if err != nil {
			t.Fatal(err)
		}
		if ok && p.IsOwner() {
			continue
		}
		var holder int64
		var query string
		args := []any{it.id}
		switch it.kind {
		case schema.IssuedCredential:
			query = `SELECT principal_id FROM credentials WHERE id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`
			args = append(args, now.UnixMilli())
		case schema.IssuedHook:
			query = `SELECT principal_id FROM hooks WHERE id = ? AND revoked_at IS NULL`
		}
		switch err := s.db.QueryRowContext(ctx, query, args...).Scan(&holder); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			t.Fatal(err)
		}
		if holder != it.target {
			continue
		}
		out = append(out, schema.IssuedByOperatorV1{Kind: it.kind, ID: it.id, PrincipalID: it.target, IssuedBy: it.from,
			IssuedAt: schema.NewTimestamp(time.UnixMilli(it.at))})
	}
	return out
}

// seedIssued makes the mixed set the issued tests read: what operator-1 and
// operator-no-invite issued to others and to themselves, what the owner
// issued, what the system issued, what was revoked, rotated or expires, and
// hooks of the same kinds. It returns the ids the list should hold, in
// issuing order, and a now at which the expiring credential has expired.
type issuedSeed struct {
	listed      []schema.IssuedByOperatorV1
	expiring    int64
	afterExpiry time.Time
}

func seedIssued(t *testing.T, f *permFixture) issuedSeed {
	t.Helper()
	op1, op2, owner := f.ids["operator-1"], f.ids["operator-no-invite"], f.ids["owner"]
	member, member2 := f.ids["member-1"], f.ids["member-no-voice"]
	ch, err := f.s.ChannelByName(f.ctx, "channel-a")
	if err != nil {
		t.Fatal(err)
	}
	cred := func(actor string, to int64, label string, exp *time.Time) schema.CredentialV1 {
		t.Helper()
		c, _, err := f.s.CreateCredential(f.ctx, actor, to, label, exp)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		return c
	}
	hook := func(actor string, to int64, token string) Hook {
		t.Helper()
		h, err := f.s.CreateHook(f.ctx, actor, token, ch.ID, to)
		if err != nil {
			t.Fatalf("%s: %v", token, err)
		}
		return h
	}
	var seed issuedSeed
	item := func(kind schema.IssuedKind, id, to, from int64, at schema.Timestamp) {
		seed.listed = append(seed.listed, schema.IssuedByOperatorV1{Kind: kind, ID: id, PrincipalID: to, IssuedBy: from, IssuedAt: at})
	}

	c := cred(principalActor(op1), member, "op1 to member-1", nil)
	item(schema.IssuedCredential, c.ID, member, op1, c.CreatedAt)
	cred(principalActor(op1), op1, "op1's own", nil)
	revoked := cred(principalActor(op1), member2, "revoked since", nil)
	if err := f.s.RevokeCredential(f.ctx, "system", revoked.ID); err != nil {
		t.Fatal(err)
	}
	cred(principalActor(owner), member2, "the owner's", nil)
	cred("system", member, "nobody's", nil)
	// Rotated by operator-no-invite: the replacement is op2's, the original
	// is dead.
	original := cred(principalActor(op1), member2, "rotated since", nil)
	replacement, _, err := f.s.RotateCredential(f.ctx, principalActor(op2), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	item(schema.IssuedCredential, replacement.ID, member2, op2, replacement.CreatedAt)
	// Expires a day on: listed now, not after.
	exp := time.Now().Add(24 * time.Hour)
	expiring := cred(principalActor(op2), member, "expiring", &exp)
	item(schema.IssuedCredential, expiring.ID, member, op2, expiring.CreatedAt)
	seed.expiring = expiring.ID
	seed.afterExpiry = exp.Add(time.Hour)

	h := hook(principalActor(op1), member, "seed-hook-op1-member")
	item(schema.IssuedHook, h.ID, member, op1, schema.NewTimestamp(h.CreatedAt))
	hook(principalActor(op1), op1, "seed-hook-op1-own")
	hook(principalActor(owner), member, "seed-hook-owner")
	hook("system", member, "seed-hook-system")
	gone := hook(principalActor(op2), member2, "seed-hook-revoked")
	if err := f.s.RevokeHook(f.ctx, "system", gone.ID); err != nil {
		t.Fatal(err)
	}
	return seed
}

// sameIssued compares two items by value, the times by instant.
func sameIssued(a, b schema.IssuedByOperatorV1) bool {
	return a.Kind == b.Kind && a.ID == b.ID && a.PrincipalID == b.PrincipalID && a.IssuedBy == b.IssuedBy && a.IssuedAt.Time().Equal(b.IssuedAt.Time())
}

// sortedIssued orders a list as the audit join did, by issuing time then
// kind then id, so the two implementations compare whatever their tie
// order.
func sortedIssued(in []schema.IssuedByOperatorV1) []schema.IssuedByOperatorV1 {
	out := slices.Clone(in)
	slices.SortFunc(out, func(a, b schema.IssuedByOperatorV1) int {
		if c := a.IssuedAt.Time().Compare(b.IssuedAt.Time()); c != 0 {
			return c
		}
		if a.Kind != b.Kind {
			if a.Kind == schema.IssuedCredential {
				return -1
			}
			return 1
		}
		return int(a.ID - b.ID)
	})
	return out
}

// IssuedToOthers over the columns answers what the audit join answered for
// every row the trail covers (issue #1024): the same items, issuers,
// holders and times, now and once a credential has expired, and
// IssuedToOthersBy narrows it to one issuer.
func TestIssuedToOthersMatchesAuditJoin(t *testing.T) {
	f := newPermFixture(t)
	seed := seedIssued(t, f)
	now := time.Now()

	got, err := f.s.IssuedToOthers(f.ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := sortedIssued(seed.listed); !slices.EqualFunc(got, want, sameIssued) {
		t.Errorf("IssuedToOthers = %+v\nwant %+v", got, want)
	}
	if join := sortedIssued(auditJoinIssuedToOthers(t, f.s, now)); !slices.EqualFunc(got, join, sameIssued) {
		t.Errorf("IssuedToOthers = %+v\naudit join = %+v", got, join)
	}

	// Once the expiring one has expired, both leave it out.
	got, err = f.s.IssuedToOthers(f.ctx, seed.afterExpiry)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(got, func(it schema.IssuedByOperatorV1) bool {
		return it.ID == seed.expiring && it.Kind == schema.IssuedCredential
	}) {
		t.Errorf("an expired credential is listed: %+v", got)
	}
	if join := sortedIssued(auditJoinIssuedToOthers(t, f.s, seed.afterExpiry)); !slices.EqualFunc(got, join, sameIssued) {
		t.Errorf("after expiry: IssuedToOthers = %+v\naudit join = %+v", got, join)
	}

	// One issuer at a time.
	for _, name := range []string{"operator-1", "operator-no-invite"} {
		id := f.ids[name]
		by, err := f.s.IssuedToOthersBy(f.ctx, id, now)
		if err != nil {
			t.Fatal(err)
		}
		var want []schema.IssuedByOperatorV1
		for _, it := range sortedIssued(seed.listed) {
			if it.IssuedBy == id {
				want = append(want, it)
			}
		}
		if !slices.EqualFunc(by, want, sameIssued) {
			t.Errorf("IssuedToOthersBy(%s) = %+v, want %+v", name, by, want)
		}
	}
	// An owner's and an unknown id's are empty, never nil.
	for _, id := range []int64{f.ids["owner"], 99999, 0} {
		by, err := f.s.IssuedToOthersBy(f.ctx, id, now)
		if err != nil || by == nil || len(by) != 0 {
			t.Errorf("IssuedToOthersBy(%d) = %v, %v; want []", id, by, err)
		}
	}
}

// TestMigration42IssuedBy: migration 42 adds issued_by to credentials and
// hooks and fills it from the audit trail: credentials and hooks issued to
// self and to others name their issuer, a rotation names the rotator on the
// replacement, a row from before e48b6e3 (no audit row names it) stays NULL,
// and when two rows name one id the latest wins.
func TestMigration42IssuedBy(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/conch.db"
	s := openTestStoreAt(t, path)
	owner, _, _, err := s.BootstrapOperator(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	human := func(name string) int64 {
		t.Helper()
		p, err := s.CreatePrincipal(ctx, PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		return p.ID
	}
	operator := func(name string) int64 {
		t.Helper()
		id := human(name)
		mark := false
		if _, err := s.SetRole(ctx, principalActor(owner.ID), owner.ID, id, RoleOperator, &mark); err != nil {
			t.Fatal(err)
		}
		return id
	}
	op1, op2, member := operator("operator-1"), operator("operator-2"), human("member-1")
	ch, err := s.CreateChannel(ctx, "channel-a")
	if err != nil {
		t.Fatal(err)
	}
	cred := func(actor string, to int64, label string) int64 {
		t.Helper()
		c, _, err := s.CreateCredential(ctx, actor, to, label, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	toOther := cred(principalActor(op1), member, "op1 to member-1")
	toSelf := cred(principalActor(op1), op1, "op1's own")
	noIssuer := cred("system", member, "the system's")
	original := cred(principalActor(op1), member, "rotated since")
	replacement, _, err := s.RotateCredential(ctx, principalActor(op2), original)
	if err != nil {
		t.Fatal(err)
	}
	hookToOther, err := s.CreateHook(ctx, principalActor(op1), "migration-hook-to-other", ch.ID, member)
	if err != nil {
		t.Fatal(err)
	}
	hookToSelf, err := s.CreateHook(ctx, principalActor(op2), "migration-hook-to-self", ch.ID, op2)
	if err != nil {
		t.Fatal(err)
	}
	hookNoIssuer, err := s.CreateHook(ctx, "system", "migration-hook-system", ch.ID, member)
	if err != nil {
		t.Fatal(err)
	}
	// From before e48b6e3: a credential and a hook with no audit row at
	// all, as a server of that time left them.
	var before, beforeHook int64
	if err := s.db.QueryRow(
		`INSERT INTO credentials (principal_id, label, token_hash, created_at, auth_generation)
		 VALUES (?, 'from before', 'before-hash', 1000, 0) RETURNING id`, member).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(
		`INSERT INTO hooks (token_hash, label, channel_id, principal_id, created_at)
		 VALUES ('before-hook-hash', 'from before', ?, ?, 1000) RETURNING id`, ch.ID, member).Scan(&beforeHook); err != nil {
		t.Fatal(err)
	}
	// Two rows naming one id: a created row by op1, then a rotated row by
	// op2 naming the same credential (the trail of an older server that
	// rotated in place). The latest wins.
	twice := cred(principalActor(op1), member, "named twice")
	if _, err := s.AppendAuditEvent(ctx, principalActor(op2), "credential_rotated", principalActor(member),
		credentialDetail(twice, "named twice")+" replaces=0"); err != nil {
		t.Fatal(err)
	}
	// A row whose actor is no principal on record: the column stays NULL
	// rather than failing the migration.
	stray := cred("system", member, "stray actor")
	if _, err := s.AppendAuditEvent(ctx, "principal:424242", "credential_created", principalActor(member),
		credentialDetail(stray, "stray actor")); err != nil {
		t.Fatal(err)
	}
	live, err := s.IssuedToOthers(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Rewind to 41: no columns, as a server before this issue held them.
	rewind, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"DROP TABLE notification_preferences",    // migration 48 (issue #1713)
		"ALTER TABLE channels DROP COLUMN topic", // migration 47 (issue #1503)
		`CREATE TABLE open_channels (channel_id INTEGER PRIMARY KEY REFERENCES channels (id), added_by INTEGER REFERENCES principals (id), added_at INTEGER NOT NULL)`, // migration 46 (issue #998)
		"DROP TABLE message_removals",                         // migration 45 (issue #849); its triggers go with it
		"DROP TABLE channel_bans",                             // migration 45 (issue #849)
		"DROP TABLE admin_actions",                            // migration 43 (issue #1326)
		"ALTER TABLE agent_manifests DROP COLUMN admin",       // migration 43
		"ALTER TABLE agent_manifests DROP COLUMN permissions", // migration 43
		"ALTER TABLE credentials DROP COLUMN issued_by",
		"ALTER TABLE hooks DROP COLUMN issued_by",
		fmt.Sprintf("PRAGMA user_version = %d", issuedByMigration-1),
	} {
		if _, err := rewind.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	if err := rewind.Close(); err != nil {
		t.Fatal(err)
	}

	s = openTestStoreAt(t, path)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) || version < issuedByMigration {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	tests := []struct {
		name  string
		table string
		id    int64
		want  int64
	}{
		{"credential to another", "credentials", toOther, op1},
		{"credential to self", "credentials", toSelf, op1},
		{"credential by the system", "credentials", noIssuer, 0},
		{"the rotated original", "credentials", original, op1},
		{"the replacement, by the rotator", "credentials", replacement.ID, op2},
		{"credential from before e48b6e3", "credentials", before, 0},
		{"credential named twice: the latest wins", "credentials", twice, op2},
		{"credential whose actor is on no record", "credentials", stray, 0},
		{"hook to another", "hooks", hookToOther.ID, op1},
		{"hook to self", "hooks", hookToSelf.ID, op2},
		{"hook by the system", "hooks", hookNoIssuer.ID, 0},
		{"hook from before e48b6e3", "hooks", beforeHook, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := issuedBy(t, s, tt.table, tt.id); got != tt.want {
				t.Errorf("issued_by = %d, want %d", got, tt.want)
			}
		})
	}
	// The list reads the same after the backfill as the live columns gave
	// before it, the twice-named credential now op2's.
	after, err := s.IssuedToOthers(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for i := range live {
		if live[i].ID == twice && live[i].Kind == schema.IssuedCredential {
			live[i].IssuedBy = op2
		}
	}
	if !slices.EqualFunc(after, live, sameIssued) {
		t.Errorf("IssuedToOthers after the migration = %+v\nbefore %+v", after, live)
	}
}

// RevokeIssued revokes everything a principal issued to others in one step
// (issue #1024): an owner's call alone, never for an owner issuer, and it
// leaves what the issuer issued to itself and what others issued.
func TestRevokeIssued(t *testing.T) {
	f := newPermFixture(t)
	seed := seedIssued(t, f)
	owner, op1, op2 := f.ids["owner"], f.ids["operator-1"], f.ids["operator-no-invite"]
	ownerActor := principalActor(owner)

	t.Run("an operator may not", func(t *testing.T) {
		if _, err := f.s.RevokeIssued(f.ctx, principalActor(op2), op2, op1); !errors.Is(err, ErrNotOwner) {
			t.Errorf("err = %v, want ErrNotOwner", err)
		}
	})
	t.Run("an unknown issuer", func(t *testing.T) {
		if _, err := f.s.RevokeIssued(f.ctx, ownerActor, owner, 99999); !errors.Is(err, ErrPrincipalNotFound) {
			t.Errorf("err = %v, want ErrPrincipalNotFound", err)
		}
	})
	t.Run("an owner issuer is protected", func(t *testing.T) {
		for _, id := range []int64{owner, f.ids["owner-braked"]} {
			if _, err := f.s.RevokeIssued(f.ctx, ownerActor, owner, id); !errors.Is(err, ErrOwnerProtected) {
				t.Errorf("issuer %d: err = %v, want ErrOwnerProtected", id, err)
			}
		}
	})
	// Nothing above revoked anything.
	got, err := f.s.IssuedToOthers(f.ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(seed.listed) {
		t.Fatalf("a refused call revoked something: %d listed, was %d", len(got), len(seed.listed))
	}

	t.Run("operator-1's", func(t *testing.T) {
		before, err := f.s.LastAuditID(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		res, err := f.s.RevokeIssued(f.ctx, ownerActor, owner, op1)
		if err != nil {
			t.Fatal(err)
		}
		// Seeded: one live credential and one live hook to others. The
		// revoked one and the rotated original are revoked already.
		if res.CredentialsRevoked != 1 || res.HooksRevoked != 1 || len(res.Credentials) != 1 {
			t.Errorf("revoked = %+v, want 1 credential and 1 hook", res)
		}
		got, err := f.s.IssuedToOthers(f.ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range got {
			if it.IssuedBy == op1 {
				t.Errorf("still listed after the revoke: %+v", it)
			}
		}
		// operator-no-invite's two credentials stand, and operator-1's own.
		if n := len(got); n != 2 {
			t.Errorf("%d items listed after the revoke, want operator-no-invite's 2: %+v", n, got)
		}
		var own int
		if err := f.s.db.QueryRow("SELECT COUNT(*) FROM credentials WHERE principal_id = ? AND issued_by = ? AND revoked_at IS NULL", op1, op1).Scan(&own); err != nil || own != 1 {
			t.Errorf("operator-1's own live credentials = %d, %v; want 1", own, err)
		}
		events, err := f.s.ListAuditEvents(f.ctx, before, 100)
		if err != nil {
			t.Fatal(err)
		}
		var revokedCreds, revokedHooks, summaries int
		for _, e := range events {
			switch {
			case e.Action == "credential_revoked" && e.Actor == ownerActor:
				revokedCreds++
			case e.Action == "hook_revoked" && e.Actor == ownerActor:
				revokedHooks++
			case e.Action == AuditIssuedRevokedAll:
				summaries++
				if e.Actor != ownerActor || e.Subject != principalActor(op1) || e.Detail != "credentials=1 hooks=1" {
					t.Errorf("summary row = %+v", e)
				}
			}
		}
		if revokedCreds != 1 || revokedHooks != 1 || summaries != 1 {
			t.Errorf("audit rows: %d credential_revoked, %d hook_revoked, %d summaries; want 1 each", revokedCreds, revokedHooks, summaries)
		}
	})
	t.Run("a repeat revokes nothing and is audited", func(t *testing.T) {
		res, err := f.s.RevokeIssued(f.ctx, ownerActor, owner, op1)
		if err != nil || res.CredentialsRevoked != 0 || res.HooksRevoked != 0 {
			t.Errorf("repeat = %+v, %v; want zeros", res, err)
		}
		last, err := f.s.LastAuditEvent(f.ctx, AuditIssuedRevokedAll)
		if err != nil || last.Detail != "credentials=0 hooks=0" {
			t.Errorf("last summary = %+v, %v", last, err)
		}
	})
	t.Run("an expired credential is revoked and counted", func(t *testing.T) {
		// operator-no-invite issued the expiring credential and the
		// rotation's replacement. Expire the first by hand.
		if _, err := f.s.db.Exec("UPDATE credentials SET expires_at = 1 WHERE id = ?", seed.expiring); err != nil {
			t.Fatal(err)
		}
		listed, err := f.s.IssuedToOthersBy(f.ctx, op2, time.Now())
		if err != nil || len(listed) != 1 {
			t.Fatalf("listed for operator-no-invite = %+v, %v; want the replacement alone", listed, err)
		}
		res, err := f.s.RevokeIssued(f.ctx, ownerActor, owner, op2)
		if err != nil || res.CredentialsRevoked != 2 || res.HooksRevoked != 0 {
			t.Errorf("revoked = %+v, %v; want 2 credentials (one expired) and no hook", res, err)
		}
	})
	t.Run("an owner holder's are neither listed nor revoked", func(t *testing.T) {
		// Owner X issues a credential and a hook to owner O and a
		// credential to member-1, then is demoted (security review of
		// #1024, NB-1). The sweep of X ends member-1's and leaves O's: an
		// owner's credentials end deliberately, never as a side effect.
		x, member := f.ids["operator-no-settings"], f.ids["member-1"]
		promoteToOwner(t, f.s, owner, x)
		toOwner, _, err := f.s.CreateCredential(f.ctx, principalActor(x), owner, "x to the owner", nil)
		if err != nil {
			t.Fatal(err)
		}
		ch, err := f.s.ChannelByName(f.ctx, "channel-a")
		if err != nil {
			t.Fatal(err)
		}
		hookToOwner, err := f.s.CreateHook(f.ctx, principalActor(x), "x-hook-to-owner", ch.ID, owner)
		if err != nil {
			t.Fatal(err)
		}
		toMember, _, err := f.s.CreateCredential(f.ctx, principalActor(x), member, "x to member-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SetRole(f.ctx, ownerActor, owner, x, RoleMember, nil); err != nil {
			t.Fatal(err)
		}
		listed, err := f.s.IssuedToOthersBy(f.ctx, x, time.Now())
		if err != nil || len(listed) != 1 || listed[0].ID != toMember.ID || listed[0].Kind != schema.IssuedCredential {
			t.Fatalf("listed for the ex-owner = %+v, %v; want member-1's credential alone", listed, err)
		}
		res, err := f.s.RevokeIssued(f.ctx, ownerActor, owner, x)
		if err != nil || res.CredentialsRevoked != 1 || res.HooksRevoked != 0 || len(res.Credentials) != 1 || res.Credentials[0] != toMember.ID {
			t.Errorf("revoked = %+v, %v; want member-1's credential alone", res, err)
		}
		var revoked sql.NullInt64
		if err := f.s.db.QueryRow("SELECT revoked_at FROM credentials WHERE id = ?", toOwner.ID).Scan(&revoked); err != nil || revoked.Valid {
			t.Errorf("the owner's credential: revoked_at = %v, %v; want NULL", revoked, err)
		}
		if err := f.s.db.QueryRow("SELECT revoked_at FROM hooks WHERE id = ?", hookToOwner.ID).Scan(&revoked); err != nil || revoked.Valid {
			t.Errorf("the owner's hook: revoked_at = %v, %v; want NULL", revoked, err)
		}
	})
	t.Run("a demoted issuer's still revoke", func(t *testing.T) {
		member := f.ids["member-1"]
		c, _, err := f.s.CreateCredential(f.ctx, principalActor(op1), member, "before the demotion", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.SetRole(f.ctx, ownerActor, owner, op1, RoleMember, nil); err != nil {
			t.Fatal(err)
		}
		res, err := f.s.RevokeIssued(f.ctx, ownerActor, owner, op1)
		if err != nil || res.CredentialsRevoked != 1 || res.Credentials[0] != c.ID {
			t.Errorf("revoked = %+v, %v; want the one credential", res, err)
		}
	})
}
