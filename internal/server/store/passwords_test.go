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

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/pkg/schema"
)

// testRecord is a record the store takes for a password's: it has the
// prefix. The store never verifies one, so its hash need not be real; the
// server's tests verify real ones.
func testRecord(tag string) string {
	return "$argon2id$v=19$m=65536,t=3,p=1$c2FsdHNhbHRzYWx0c2FsdA$" + tag
}

// enrollTOTP gives principal id a confirmed second factor, underneath the
// store: enrolment is not this issue's, and the sign-in only reads whether
// one is confirmed.
func enrollTOTP(t *testing.T, s *Store, id int64) {
	t.Helper()
	if _, err := s.db.Exec("INSERT INTO totp (principal_id, secret, enrolled_at) VALUES (?, x'00', 1)", id); err != nil {
		t.Fatal(err)
	}
}

func allAuditEvents(t *testing.T, s *Store) []AuditEvent {
	t.Helper()
	evs, err := s.ListAuditEvents(context.Background(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

// A database from before passwords keeps every principal, credential and
// sign-in link live through the migration: every row is generation 0, as
// every principal is, every credential has full scope and every link is a
// sign-in link. Nobody has a password.
func TestPasswordsMigration(t *testing.T) {
	const pre = passwordsMigration - 1
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
	token, err := newCredentialToken()
	if err != nil {
		t.Fatal(err)
	}
	k := names.KeysOf("wren")
	for _, stmt := range []string{
		fmt.Sprintf(`INSERT INTO principals (id, kind, name, name_key, name_skel, role, created_at) VALUES (1, 'human', 'wren', '%s', '%s', 'member', 1)`, k.Folded, k.Skeletal),
		fmt.Sprintf(`INSERT INTO credentials (id, principal_id, label, token_hash, created_at) VALUES (1, 1, 'old', '%s', 1)`, hashCredentialToken(token)),
		`INSERT INTO sign_in_links (id, code_hash, principal_id, created_by, created_at, expires_at) VALUES (1, 'h', 1, 1, 1, 2)`,
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
	t.Cleanup(func() { _ = s.Close() })

	if p, err := s.ResolveCredential(ctx, token); err != nil || p.ID != 1 {
		t.Fatalf("the old credential after the migration = %+v, %v; want principal 1", p, err)
	}
	if live, err := s.CredentialLive(ctx, 1); err != nil || !live {
		t.Errorf("CredentialLive after the migration = %v, %v; want live", live, err)
	}
	var (
		pGen, cGen, lGen int64
		scope, purpose   string
		passwords        int
	)
	if err := s.db.QueryRow(`SELECT p.auth_generation, c.auth_generation, c.scope, l.auth_generation, l.purpose,
		(SELECT COUNT(*) FROM passwords)
		FROM principals p, credentials c, sign_in_links l WHERE p.id = 1 AND c.id = 1 AND l.id = 1`,
	).Scan(&pGen, &cGen, &scope, &lGen, &purpose, &passwords); err != nil {
		t.Fatal(err)
	}
	if pGen != 0 || cGen != 0 || lGen != 0 || scope != "full" || purpose != "sign_in" || passwords != 0 {
		t.Errorf("after the migration: generations %d/%d/%d, scope %q, purpose %q, %d passwords; want 0/0/0, full, sign_in, 0",
			pGen, cGen, lGen, scope, purpose, passwords)
	}
	c, found, err := s.LoginCandidate(ctx, "wren")
	if err != nil || !found || c.Record != "" || c.TOTPEnrolled {
		t.Errorf("LoginCandidate(wren) = %+v, %v, %v; want found, with no password", c, found, err)
	}
}

// Storing a first password ends nothing; replacing it moves the generation
// on, which ends every credential issued before, however it is checked; a
// credential issued after is live. Agents, unknown principals and records
// that are not records are refused and write nothing.
func TestSetPassword(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	wren, err := s.CreatePrincipal(ctx, PrincipalHuman, "wren")
	if err != nil {
		t.Fatal(err)
	}
	bot, err := s.CreatePrincipal(ctx, PrincipalAgent, "bot")
	if err != nil {
		t.Fatal(err)
	}
	old, oldTok, err := s.CreateCredential(ctx, "system", wren.ID, "old", nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name   string
		id     int64
		record string
		want   error
	}{
		{"agent", bot.ID, testRecord("a"), ErrPasswordNotAllowed},
		{"unknown principal", 9999, testRecord("a"), ErrPrincipalNotFound},
		{"plaintext", wren.ID, "correct horse battery staple", ErrPasswordRecordInvalid},
		{"empty", wren.ID, "", ErrPasswordRecordInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(allAuditEvents(t, s))
			if _, err := s.SetPassword(ctx, "system", tt.id, tt.record, "test", now); !errors.Is(err, tt.want) {
				t.Errorf("SetPassword = %v, want %v", err, tt.want)
			}
			if after := len(allAuditEvents(t, s)); after != before {
				t.Errorf("a refused SetPassword wrote %d audit rows", after-before)
			}
		})
	}
	var rows int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM passwords").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("passwords after refusals = %d, %v; want none", rows, err)
	}

	replaced, err := s.SetPassword(ctx, "principal:1", wren.ID, testRecord("first"), "enrollment", now)
	if err != nil || replaced {
		t.Fatalf("first SetPassword = %v, %v; want stored, nothing replaced", replaced, err)
	}
	if _, err := s.ResolveCredential(ctx, oldTok); err != nil {
		t.Fatalf("a first password ended a credential: %v", err)
	}

	replaced, err = s.SetPassword(ctx, "principal:1", wren.ID, testRecord("second"), "reset", now)
	if err != nil || !replaced {
		t.Fatalf("second SetPassword = %v, %v; want replaced", replaced, err)
	}
	if _, err := s.ResolveCredential(ctx, oldTok); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("ResolveCredential after the generation moved = %v, want ErrCredentialInvalid", err)
	}
	if live, err := s.CredentialLive(ctx, old.ID); err != nil || live {
		t.Errorf("CredentialLive after the generation moved = %v, %v; want not live", live, err)
	}
	if own, err := s.ListLiveCredentials(ctx, wren.ID); err != nil || len(own) != 0 {
		t.Errorf("ListLiveCredentials after the generation moved = %+v, %v; want none", own, err)
	}
	if _, _, err := s.RotateCredential(ctx, "system", old.ID); !errors.Is(err, ErrCredentialRevoked) {
		t.Errorf("rotating a credential of the old generation = %v, want ErrCredentialRevoked: rotation must not revive it", err)
	}
	_, newTok, err := s.CreateCredential(ctx, "system", wren.ID, "new", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveCredential(ctx, newTok); err != nil {
		t.Errorf("a credential issued after the bump does not resolve: %v", err)
	}

	var details []string
	for _, e := range allAuditEvents(t, s) {
		switch e.Action {
		case AuditPasswordSet, AuditAuthGenerationBumped:
			details = append(details, e.Action+" "+e.Detail)
			if strings.Contains(e.Detail, "argon2") || strings.Contains(e.Detail, "c2FsdH") {
				t.Errorf("audit detail %q carries the record", e.Detail)
			}
		}
	}
	want := []string{
		"password_set via=enrollment replaced=false",
		"password_set via=reset replaced=true",
		"auth_generation_bumped cause=password_replaced generation=1",
	}
	if strings.Join(details, "\n") != strings.Join(want, "\n") {
		t.Errorf("audit rows:\n%s\nwant:\n%s", strings.Join(details, "\n"), strings.Join(want, "\n"))
	}
}

// A setup-scoped credential resolves nowhere in this build, which fails
// closed until the middleware's scope check exists.
func TestSetupScopeDoesNotResolve(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "wren")
	if err != nil {
		t.Fatal(err)
	}
	c, tok, err := s.CreateCredential(ctx, "system", p.ID, "setup", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE credentials SET scope = 'setup' WHERE id = ?", c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveCredential(ctx, tok); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("ResolveCredential(setup scope) = %v, want ErrCredentialInvalid", err)
	}
	if live, err := s.CredentialLive(ctx, c.ID); err != nil || live {
		t.Errorf("CredentialLive(setup scope) = %v, %v; want not live", live, err)
	}
	if _, _, err := s.RotateCredential(ctx, "system", c.ID); !errors.Is(err, ErrCredentialRevoked) {
		t.Errorf("RotateCredential(setup scope) = %v, want ErrCredentialRevoked: rotation must not widen it to full", err)
	}
}

func TestLoginCandidate(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	now := time.Now()
	wren, err := s.CreatePrincipal(ctx, PrincipalHuman, "Wren")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetPassword(ctx, "system", wren.ID, testRecord("w"), "test", now); err != nil {
		t.Fatal(err)
	}
	op, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	enrollTOTP(t, s, op.ID)
	// Two principals with one name key, as a database from before keys can
	// hold (issue #441): written underneath, as the store refuses it now.
	k := names.KeysOf("alice")
	for i, name := range []string{"Alice", "alice"} {
		if _, err := s.db.Exec(`INSERT INTO principals (kind, name, name_key, name_skel, role, created_at) VALUES ('human', ?, ?, ?, 'member', ?)`,
			name, k.Folded, k.Skeletal, i); err != nil {
			t.Fatal(err)
		}
	}

	for _, tt := range []struct {
		typed      string
		wantName   string // empty: not found
		wantRecord string
		wantTOTP   bool
	}{
		{"Wren", "Wren", testRecord("w"), false},
		{"wren", "Wren", testRecord("w"), false},
		{"WREN", "Wren", testRecord("w"), false},
		{"root", "root", "", true},
		{"nobody", "", "", false},
		{"Alice", "Alice", "", false},
		{"alice", "alice", "", false},
		{"ALICE", "", "", false},
		{"ㅤ", "", "", false},
	} {
		t.Run(tt.typed, func(t *testing.T) {
			c, found, err := s.LoginCandidate(ctx, tt.typed)
			if err != nil {
				t.Fatal(err)
			}
			if found != (tt.wantName != "") || (found && c.Principal.Name != tt.wantName) {
				t.Fatalf("LoginCandidate(%q) = %q, %v; want %q", tt.typed, c.Principal.Name, found, tt.wantName)
			}
			if c.Record != tt.wantRecord || c.TOTPEnrolled != tt.wantTOTP {
				t.Errorf("LoginCandidate(%q) record %q totp %v; want %q, %v", tt.typed, c.Record, c.TOTPEnrolled, tt.wantRecord, tt.wantTOTP)
			}
		})
	}
}

// loginWorld is a member and an operator, each with a password, the
// operator with a second factor.
type loginWorld struct {
	s          *Store
	member, op Principal
	now        time.Time
}

func newLoginWorld(t *testing.T) *loginWorld {
	t.Helper()
	ctx := context.Background()
	s := openTestStore(t)
	w := &loginWorld{s: s, now: time.Now()}
	op, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	w.op = op
	if w.member, err = s.CreatePrincipal(ctx, PrincipalHuman, "wren"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{w.op.ID, w.member.ID} {
		if _, err := s.SetPassword(ctx, "system", id, testRecord(fmt.Sprint(id)), "test", w.now); err != nil {
			t.Fatal(err)
		}
	}
	enrollTOTP(t, s, w.op.ID)
	return w
}

func (w *loginWorld) issue(t *testing.T, p Principal) LoginIssue {
	t.Helper()
	c, found, err := w.s.LoginCandidate(context.Background(), p.Name)
	if err != nil || !found {
		t.Fatalf("LoginCandidate(%s) = %v, %v", p.Name, found, err)
	}
	return LoginIssue{PrincipalID: c.Principal.ID, AuthGeneration: c.AuthGeneration, Record: c.Record}
}

func (w *loginWorld) credentials(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.s.db.QueryRow("SELECT COUNT(*) FROM credentials").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Everything a verification relied on is re-read in the issuing
// transaction: any change in between is ErrLoginStale, and nothing is
// written. With no change the member gets a live credential of the current
// generation, audited as login and credential_created.
func TestIssueLoginCredentialRechecks(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		change func(t *testing.T, w *loginWorld, in *LoginIssue)
		want   error
	}{
		{"nothing changed", func(*testing.T, *loginWorld, *LoginIssue) {}, nil},
		{"disabled", func(t *testing.T, w *loginWorld, _ *LoginIssue) {
			if _, err := w.s.DisablePrincipal(ctx, "system", w.member.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrLoginStale},
		{"hidden", func(t *testing.T, w *loginWorld, _ *LoginIssue) {
			if _, err := w.s.HidePrincipal(ctx, "system", w.member.ID, HideOptions{}); err != nil {
				t.Fatal(err)
			}
		}, ErrLoginStale},
		{"password replaced", func(t *testing.T, w *loginWorld, _ *LoginIssue) {
			if _, err := w.s.SetPassword(ctx, "system", w.member.ID, testRecord("other"), "reset", w.now); err != nil {
				t.Fatal(err)
			}
		}, ErrLoginStale},
		{"generation moved", func(t *testing.T, w *loginWorld, _ *LoginIssue) {
			if _, err := w.s.BumpAuthGeneration(ctx, "system", w.member.ID, "revoke_all", w.now); err != nil {
				t.Fatal(err)
			}
		}, ErrLoginStale},
		{"made an operator", func(t *testing.T, w *loginWorld, _ *LoginIssue) {
			if _, err := w.s.db.Exec("UPDATE principals SET role = 'operator' WHERE id = ?", w.member.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrLoginStale},
		{"another record verified", func(_ *testing.T, _ *loginWorld, in *LoginIssue) { in.Record = testRecord("guess") }, ErrLoginStale},
		{"no record verified", func(_ *testing.T, _ *loginWorld, in *LoginIssue) { in.Record = "" }, ErrLoginStale},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newLoginWorld(t)
			in := w.issue(t, w.member)
			tt.change(t, w, &in)
			before := w.credentials(t)
			got, err := w.s.IssueLoginCredential(ctx, in, w.now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("IssueLoginCredential = %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				if after := w.credentials(t); after != before {
					t.Errorf("a stale sign-in issued %d credentials", after-before)
				}
				return
			}
			p, err := w.s.ResolveCredential(ctx, got.Token)
			if err != nil || p.ID != w.member.ID {
				t.Fatalf("the issued credential resolves to %+v, %v", p, err)
			}
			if got.Credential.Label != "password sign-in" || !got.Credential.Renews || got.Credential.ExpiresAt == nil {
				t.Errorf("credential = %+v, want a renewable one labelled password sign-in", got.Credential)
			}
			var rows []string
			for _, e := range allAuditEvents(t, w.s) {
				if e.Action == AuditLogin || (e.Action == "credential_created" && strings.Contains(e.Detail, "password sign-in")) {
					rows = append(rows, fmt.Sprintf("%s %s %s %s", e.Actor, e.Action, e.Subject, e.Detail))
				}
			}
			actor := principalActor(w.member.ID)
			want := []string{
				fmt.Sprintf("%s login %s credential=%d", actor, actor, got.Credential.ID),
				fmt.Sprintf("%s credential_created %s %s", actor, actor, credentialDetail(got.Credential.ID, "password sign-in")),
			}
			if strings.Join(rows, "\n") != strings.Join(want, "\n") {
				t.Errorf("audit:\n%s\nwant:\n%s", strings.Join(rows, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}

// A rehash is written only by a sign-in that is issued, and only over the
// record that was verified.
func TestIssueLoginCredentialRehash(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	in := w.issue(t, w.member)
	in.Rehash = testRecord("upgraded")
	if _, err := w.s.IssueLoginCredential(ctx, in, w.now); err != nil {
		t.Fatal(err)
	}
	c, _, err := w.s.LoginCandidate(ctx, w.member.Name)
	if err != nil || c.Record != testRecord("upgraded") || c.AuthGeneration != in.AuthGeneration {
		t.Fatalf("after the rehash: record %q generation %d, %v; want the upgraded record and an unchanged generation", c.Record, c.AuthGeneration, err)
	}
	// A rehash that is not a record is refused with the sign-in.
	in = w.issue(t, w.member)
	in.Rehash = "plaintext"
	if _, err := w.s.IssueLoginCredential(ctx, in, w.now); !errors.Is(err, ErrPasswordRecordInvalid) {
		t.Errorf("a plaintext rehash = %v, want ErrPasswordRecordInvalid", err)
	}
}

// An operator's verified password yields a challenge and no credential,
// bound to the operator, its generation and the address it was issued to;
// only the challenge's hash is kept. An operator with no confirmed second
// factor, and a member, get no challenge.
func TestCreatePendingLogin(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	before := w.credentials(t)
	pending, err := w.s.CreatePendingLogin(ctx, w.issue(t, w.op), "203.0.113.7", w.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidatePendingLoginChallenge(pending.Challenge); err != nil {
		t.Errorf("challenge: %v", err)
	}
	if got, want := pending.ExpiresAt, w.now.Truncate(time.Millisecond).Add(schema.PendingLoginLifetimeSeconds*time.Second); !got.Equal(want) {
		t.Errorf("expires at %v, want %v", got, want)
	}
	if after := w.credentials(t); after != before {
		t.Errorf("a challenge issued %d credentials", after-before)
	}
	var (
		hash, addr     string
		pid, gen, used int64
	)
	if err := w.s.db.QueryRow("SELECT challenge_hash, principal_id, address_key, auth_generation, attempts FROM pending_logins").
		Scan(&hash, &pid, &addr, &gen, &used); err != nil {
		t.Fatal(err)
	}
	if hash != hashCredentialToken(pending.Challenge) || hash == pending.Challenge || pid != w.op.ID || addr != "203.0.113.7" || gen != 0 || used != 0 {
		t.Errorf("pending login row: hash %q principal %d address %q generation %d attempts %d", hash, pid, addr, gen, used)
	}

	if _, err := w.s.db.Exec("DELETE FROM totp WHERE principal_id = ?", w.op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.CreatePendingLogin(ctx, w.issue(t, w.op), "x", w.now); !errors.Is(err, ErrLoginStale) {
		t.Errorf("an operator without a second factor = %v, want ErrLoginStale", err)
	}
	if _, err := w.s.CreatePendingLogin(ctx, w.issue(t, w.member), "x", w.now); !errors.Is(err, ErrLoginStale) {
		t.Errorf("a member = %v, want ErrLoginStale", err)
	}
	if _, err := w.s.IssueLoginCredential(ctx, w.issue(t, w.op), w.now); !errors.Is(err, ErrLoginStale) {
		t.Errorf("an operator issued a member's credential = %v, want ErrLoginStale", err)
	}
}

// A sign-in racing a generation bump or a disable never leaves a credential
// that outlives them: whichever commits first, the sign-in is refused or its
// credential is dead.
func TestIssueLoginCredentialRaces(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name string
		race func(w *loginWorld) error
	}{
		{"generation bump", func(w *loginWorld) error {
			_, err := w.s.BumpAuthGeneration(ctx, "system", w.member.ID, "revoke_all", w.now)
			return err
		}},
		{"disable", func(w *loginWorld) error {
			_, err := w.s.DisablePrincipal(ctx, "system", w.member.ID)
			return err
		}},
		{"password replaced", func(w *loginWorld) error {
			_, err := w.s.SetPassword(ctx, "system", w.member.ID, testRecord("new"), "reset", w.now)
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for i := 0; i < 20; i++ {
				w := newLoginWorld(t)
				in := w.issue(t, w.member)
				var (
					wg       sync.WaitGroup
					issued   IssuedLogin
					issueErr error
					raceErr  error
				)
				wg.Add(2)
				go func() { defer wg.Done(); issued, issueErr = w.s.IssueLoginCredential(ctx, in, w.now) }()
				go func() { defer wg.Done(); raceErr = tt.race(w) }()
				wg.Wait()
				if raceErr != nil {
					t.Fatal(raceErr)
				}
				switch {
				case errors.Is(issueErr, ErrLoginStale):
				case issueErr != nil:
					t.Fatal(issueErr)
				default:
					if _, err := w.s.ResolveCredential(ctx, issued.Token); !errors.Is(err, ErrCredentialInvalid) {
						t.Fatalf("round %d: a credential issued in the race outlives it: %v", i, err)
					}
				}
			}
		})
	}
}

// A redemption with a password stores it with the account, audited after
// principal_created; one that fails stores no password and uses nothing.
func TestRedeemInviteWithPassword(t *testing.T) {
	f := newInviteFixture(t)
	inv := f.mustCreate(f.op, open("general"))
	res, err := f.s.RedeemInviteWithPassword(f.ctx, HashInviteCode(inv.Code), "wren", testRecord("w"), f.now)
	if err != nil {
		t.Fatal(err)
	}
	c, found, err := f.s.LoginCandidate(f.ctx, "wren")
	if err != nil || !found || c.Principal.ID != res.Principal.ID || c.Record != testRecord("w") {
		t.Fatalf("after redeeming: %+v, %v, %v", c, found, err)
	}
	var actions []string
	for _, e := range f.audit() {
		if e.Actor == inviteSubject(inv.Invite.ID) {
			actions = append(actions, e.Action)
		}
	}
	want := []string{AuditInviteRedeemed, AuditPrincipalCreated, AuditPasswordSet, AuditMemberAdded, "credential_created"}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Errorf("audit actions %v, want %v", actions, want)
	}

	for _, tt := range []struct {
		name, joiner, record string
		want                 error
	}{
		{"taken name", "wren", testRecord("x"), ErrDuplicate},
		{"not a record", "ash", "hunter2hunter2hunter2", ErrPasswordRecordInvalid},
	} {
		t.Run(tt.name, func(t *testing.T) {
			passwords := f.count("SELECT COUNT(*) FROM passwords")
			uses := f.count("SELECT uses FROM invites WHERE id = ?", inv.Invite.ID)
			if _, err := f.s.RedeemInviteWithPassword(f.ctx, HashInviteCode(inv.Code), tt.joiner, tt.record, f.now); !errors.Is(err, tt.want) {
				t.Fatalf("RedeemInviteWithPassword = %v, want %v", err, tt.want)
			}
			if got := f.count("SELECT COUNT(*) FROM passwords"); got != passwords {
				t.Errorf("a failed redemption stored %d passwords", got-passwords)
			}
			if got := f.count("SELECT uses FROM invites WHERE id = ?", inv.Invite.ID); got != uses {
				t.Errorf("a failed redemption used the invite: uses %d, was %d", got, uses)
			}
		})
	}
}

// A guest never signs in with a password, live or ended, even with a
// password row planted for it underneath every route; once converted it is
// a member and may (issue #705).
func TestLoginRefusesGuests(t *testing.T) {
	f := newGuestFixture(t)
	plant := func(id int64) {
		t.Helper()
		if _, err := f.s.db.Exec("INSERT INTO passwords (principal_id, hash, created_at, updated_at) VALUES (?, ?, 1, 1)", id, testRecord("g")); err != nil {
			t.Fatal(err)
		}
	}
	live := f.guest("live")
	ended := f.guest("ended")
	if _, err := f.s.EndGuestVisit(f.ctx, "system", ended.Principal.ID, schema.GuestEndVisit); err != nil {
		t.Fatal(err)
	}
	for _, g := range []CreatedGuest{live, ended} {
		plant(g.Principal.ID)
		c, found, err := f.s.LoginCandidate(f.ctx, g.Principal.Name)
		if err != nil || !found || !c.IsGuestTarget() {
			t.Fatalf("LoginCandidate(%s) = %+v, %v, %v; want found, a guest", g.Principal.Name, c, found, err)
		}
		in := LoginIssue{PrincipalID: c.Principal.ID, AuthGeneration: c.AuthGeneration, Record: c.Record}
		if _, err := f.s.IssueLoginCredential(f.ctx, in, time.Now()); !errors.Is(err, ErrLoginStale) {
			t.Errorf("IssueLoginCredential for guest %s = %v, want ErrLoginStale", g.Principal.Name, err)
		}
	}

	conv := f.guest("convert")
	if _, err := f.s.ConvertGuest(f.ctx, "system", GuestConversion{PrincipalID: conv.Principal.ID, Name: "pat",
		CredentialLabel: "member", CredentialLifetime: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetPassword(f.ctx, "system", conv.Principal.ID, testRecord("pat"), "conversion", time.Now()); err != nil {
		t.Fatal(err)
	}
	c, found, err := f.s.LoginCandidate(f.ctx, "pat")
	if err != nil || !found || c.IsGuestTarget() {
		t.Fatalf("LoginCandidate(pat) = %+v, %v, %v; want the converted member", c, found, err)
	}
	if _, err := f.s.IssueLoginCredential(f.ctx, LoginIssue{PrincipalID: c.Principal.ID, AuthGeneration: c.AuthGeneration, Record: c.Record}, time.Now()); err != nil {
		t.Errorf("a converted guest's sign-in = %v, want a credential", err)
	}
}

// An invite that admits guests only makes no account through the password
// route, before and inside the redeeming transaction; account and both do.
func TestRedeemInviteWithPasswordJoinAs(t *testing.T) {
	for _, tt := range []struct {
		joinAs schema.InviteJoinMode
		want   error
	}{
		{schema.InviteJoinGuest, ErrJoiningClosed},
		{schema.InviteJoinAccount, nil},
		{schema.InviteJoinBoth, nil},
	} {
		t.Run(string(tt.joinAs), func(t *testing.T) {
			f := newInviteFixture(t)
			inv := f.mustCreate(f.op, open("general"))
			if _, err := f.s.db.Exec("UPDATE invites SET join_as = ? WHERE id = ?", string(tt.joinAs), inv.Invite.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.s.CheckInviteForAccount(f.ctx, HashInviteCode(inv.Code), f.now); !errors.Is(err, tt.want) {
				t.Errorf("CheckInviteForAccount = %v, want %v", err, tt.want)
			}
			if _, err := f.s.RedeemInviteWithPassword(f.ctx, HashInviteCode(inv.Code), "wren", testRecord("w"), f.now); !errors.Is(err, tt.want) {
				t.Errorf("RedeemInviteWithPassword = %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				if n := f.count("SELECT uses FROM invites WHERE id = ?", inv.Invite.ID); n != 0 {
					t.Errorf("a refused redemption used the invite: uses %d", n)
				}
			}
		})
	}
	// An invalid code is invalid whatever a real invite's mode.
	f := newInviteFixture(t)
	if err := f.s.CheckInviteForAccount(f.ctx, HashInviteCode(schema.InviteCodePrefix+strings.Repeat("Q", 43)), f.now); !errors.Is(err, ErrInviteInvalid) {
		t.Errorf("CheckInviteForAccount(unknown) = %v, want ErrInviteInvalid", err)
	}
}

// The last-operator guard counts only credentials that would sign someone
// in: one from an old authentication generation does not keep a second
// operator able to sign in, so disabling the last current one is refused.
// The admin listing's count agrees.
func TestLastOperatorCountsCurrentCredentials(t *testing.T) {
	for _, tt := range []struct {
		name  string
		stale bool
		want  error
	}{
		{"the other operator's credential is current", false, nil},
		{"the other operator's credential is from an old generation", true, ErrLastOperator},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			root, _, _, err := s.BootstrapOperator(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			other, err := s.CreatePrincipal(ctx, PrincipalHuman, "second")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("UPDATE principals SET role = 'operator' WHERE id = ?", other.ID); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.CreateCredential(ctx, "system", other.ID, "op", nil); err != nil {
				t.Fatal(err)
			}
			wantLive := 1
			if tt.stale {
				if _, err := s.BumpAuthGeneration(ctx, "system", other.ID, "revoke_all", time.Now()); err != nil {
					t.Fatal(err)
				}
				wantLive = 0
			}
			if adm, err := s.PrincipalAdmin(ctx, other.ID, time.Now()); err != nil || adm.LiveCredentials != wantLive {
				t.Errorf("live_credentials = %d, %v; want %d", adm.LiveCredentials, err, wantLive)
			}
			if _, err := s.DisablePrincipal(ctx, "system", root.ID); !errors.Is(err, tt.want) {
				t.Errorf("DisablePrincipal(root) = %v, want %v", err, tt.want)
			}
		})
	}
}

// A guest, live or ended, has no password to set; a converted one is a
// member and may.
func TestSetPasswordRefusesGuests(t *testing.T) {
	f := newGuestFixture(t)
	live := f.guest("live")
	ended := f.guest("ended")
	if _, err := f.s.EndGuestVisit(f.ctx, "system", ended.Principal.ID, schema.GuestEndVisit); err != nil {
		t.Fatal(err)
	}
	for _, g := range []CreatedGuest{live, ended} {
		if _, err := f.s.SetPassword(f.ctx, "system", g.Principal.ID, testRecord("g"), "test", time.Now()); !errors.Is(err, ErrGuestNotAllowed) {
			t.Errorf("SetPassword(%s) = %v, want ErrGuestNotAllowed", g.Principal.Name, err)
		}
	}
	if n := f.count("SELECT COUNT(*) FROM passwords"); n != 0 {
		t.Errorf("%d passwords stored for guests", n)
	}
	conv := f.guest("convert")
	if _, err := f.s.ConvertGuest(f.ctx, "system", GuestConversion{PrincipalID: conv.Principal.ID, Name: "pat",
		CredentialLabel: "member", CredentialLifetime: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.SetPassword(f.ctx, "system", conv.Principal.ID, testRecord("pat"), "conversion", time.Now()); err != nil {
		t.Errorf("SetPassword(converted) = %v, want stored", err)
	}
}

// recoveryWorld is the invite fixture (op, alice, bob) for issue #708's
// transactions: alice has a password and two sessions, bob is a member who
// predates passwords with one session.
type recoveryWorld struct {
	*inviteFixture
	aliceA, aliceB, bobCred int64
	aliceATok               string
}

func newRecoveryWorld(t *testing.T) *recoveryWorld {
	t.Helper()
	w := &recoveryWorld{inviteFixture: newInviteFixture(t)}
	if _, err := w.s.SetPassword(w.ctx, "system", w.alice.ID, testRecord("alice"), "test", w.now); err != nil {
		t.Fatal(err)
	}
	w.aliceA, w.aliceATok = w.session(w.alice.ID)
	w.aliceB, _ = w.session(w.alice.ID)
	w.bobCred, _ = w.session(w.bob.ID)
	return w
}

// session issues id a renewable credential, as a browser sign-in is.
func (w *recoveryWorld) session(id int64) (int64, string) {
	w.t.Helper()
	c, tok, err := w.s.CreateRenewableCredential(w.ctx, "system", id, "browser", 30*24*time.Hour)
	if err != nil {
		w.t.Fatal(err)
	}
	return c.ID, tok
}

func (w *recoveryWorld) link(purpose schema.SignInLinkPurpose, target Principal) schema.CreateSignInLinkResponseV1 {
	w.t.Helper()
	resp, err := w.makeSignInLink(w.op, target, schema.CreateSignInLinkRequestV1{Purpose: purpose})
	if err != nil {
		w.t.Fatalf("CreateSignInLink(%s): %v", purpose, err)
	}
	return resp
}

func (w *recoveryWorld) live(id int64) bool {
	w.t.Helper()
	ok, err := w.s.CredentialLive(w.ctx, id)
	if err != nil {
		w.t.Fatal(err)
	}
	return ok
}

func (w *recoveryWorld) generation(id int64) int64 {
	w.t.Helper()
	var g int64
	if err := w.s.db.QueryRow("SELECT auth_generation FROM principals WHERE id = ?", id).Scan(&g); err != nil {
		w.t.Fatal(err)
	}
	return g
}

func (w *recoveryWorld) record(id int64) string {
	w.t.Helper()
	var r sql.NullString
	if err := w.s.db.QueryRow("SELECT hash FROM passwords WHERE principal_id = ?", id).Scan(&r); err != nil && !errors.Is(err, sql.ErrNoRows) {
		w.t.Fatal(err)
	}
	return r.String
}

func (w *recoveryWorld) change(cred int64, record string) PasswordChange {
	return PasswordChange{PrincipalID: w.alice.ID, CredentialID: cred, AuthGeneration: w.generation(w.alice.ID),
		Record: w.record(w.alice.ID), NewRecord: record}
}

// A link records its person's generation and is good only while it is
// theirs, whatever its purpose; each purpose is taken only where it is
// named, and the making of a reset or an enrollment link is audited by name.
func TestSignInLinkPurposes(t *testing.T) {
	tests := []struct {
		purpose  schema.SignInLinkPurpose
		action   string
		previews bool
		redeems  bool
		lifetime time.Duration
	}{
		{schema.SignInLinkPurposeSignIn, AuditSignInLinkCreated, true, true, schema.DefaultSignInLinkLifetime},
		{schema.SignInLinkPurposeReset, AuditPasswordResetIssued, true, false, schema.DefaultRestrictedLinkLifetime},
		{schema.SignInLinkPurposeEnrollment, AuditEnrollmentIssued, false, false, schema.DefaultRestrictedLinkLifetime},
	}
	for _, tt := range tests {
		t.Run(string(tt.purpose), func(t *testing.T) {
			w := newRecoveryWorld(t)
			l := w.link(tt.purpose, w.alice)
			if l.SignInLink.Purpose != tt.purpose || !l.SignInLink.ExpiresAt.Time().Equal(w.now.Add(tt.lifetime)) {
				t.Errorf("link = %+v, want purpose %s for %s", l.SignInLink, tt.purpose, tt.lifetime)
			}
			var purpose string
			var gen int64
			if err := w.s.db.QueryRow("SELECT purpose, auth_generation FROM sign_in_links WHERE id = ?", l.SignInLink.ID).Scan(&purpose, &gen); err != nil {
				t.Fatal(err)
			}
			if purpose != string(tt.purpose) || gen != w.generation(w.alice.ID) {
				t.Errorf("row purpose %q generation %d, want %q and alice's %d", purpose, gen, tt.purpose, w.generation(w.alice.ID))
			}
			evs := w.audit()
			if last := evs[len(evs)-1]; last.Action != tt.action || last.Subject != principalActor(w.alice.ID) {
				t.Errorf("audit = %+v, want %s about alice", last, tt.action)
			}
			p, err := w.s.PreviewSignInLink(w.ctx, HashSignInLinkCode(l.Code), w.now)
			if tt.previews != (err == nil) || (err == nil && (p.Name != "alice" || p.Purpose != tt.purpose)) {
				t.Errorf("preview = %+v, %v; want previews=%v with the purpose", p, err, tt.previews)
			}
			if _, err := w.useSignInLink(l.Code, w.now); tt.redeems != (err == nil) {
				t.Errorf("redeem = %v, want redeems=%v", err, tt.redeems)
			}
		})
	}
}

// A link made before the generation moved on is refused by the generation
// check itself, before anything closes it; and a bump closes every open link
// of the principal, audited with reason auth_changed.
func TestSignInLinkGeneration(t *testing.T) {
	w := newRecoveryWorld(t)
	stale := w.link(schema.SignInLinkPurposeSignIn, w.alice)
	w.exec("UPDATE principals SET auth_generation = auth_generation + 1 WHERE id = ?", w.alice.ID)
	if _, err := w.s.PreviewSignInLink(w.ctx, HashSignInLinkCode(stale.Code), w.now); !errors.Is(err, ErrSignInLinkInvalid) {
		t.Errorf("preview of a link of an old generation = %v, want ErrSignInLinkInvalid", err)
	}
	if _, err := w.useSignInLink(stale.Code, w.now); !errors.Is(err, ErrSignInLinkInvalid) {
		t.Errorf("redeem of a link of an old generation = %v, want ErrSignInLinkInvalid", err)
	}

	open := w.link(schema.SignInLinkPurposeReset, w.alice)
	bobs := w.link(schema.SignInLinkPurposeSignIn, w.bob)
	if _, err := w.s.BumpAuthGeneration(w.ctx, "system", w.alice.ID, "test", w.now); err != nil {
		t.Fatal(err)
	}
	var closed bool
	if err := w.s.db.QueryRow("SELECT closed_at IS NOT NULL FROM sign_in_links WHERE id = ?", open.SignInLink.ID).Scan(&closed); err != nil || !closed {
		t.Errorf("alice's open link closed = %v, %v; want closed by the bump", closed, err)
	}
	want := fmt.Sprintf("sign_in_link=%d reason=%s", open.SignInLink.ID, SignInLinkCloseAuthChanged)
	found := false
	for _, e := range w.audit() {
		if e.Action == AuditSignInLinkClosed && e.Detail == want {
			found = true
		}
	}
	if !found {
		t.Errorf("no sign_in_link_closed row %q", want)
	}
	if _, err := w.useSignInLink(bobs.Code, w.now); err != nil {
		t.Errorf("bob's link after alice's bump = %v, want it to work", err)
	}
}

// Enrolment: a first password needs a usable enrollment link for the
// caller, and anything else writes nothing.
func TestEnrollPasswordRefusals(t *testing.T) {
	tests := []struct {
		name string
		// in builds the enrolment; it may prepare the world.
		in   func(w *recoveryWorld) PasswordEnrollment
		want error
	}{
		{"no proof", func(w *recoveryWorld) PasswordEnrollment {
			return PasswordEnrollment{PrincipalID: w.bob.ID, CredentialID: w.bobCred, Record: testRecord("b")}
		}, ErrEnrollmentProofRequired},
		{"a proof for somebody else", func(w *recoveryWorld) PasswordEnrollment {
			l := w.link(schema.SignInLinkPurposeEnrollment, w.alice)
			return PasswordEnrollment{PrincipalID: w.bob.ID, CredentialID: w.bobCred, Record: testRecord("b"), ProofHash: HashSignInLinkCode(l.Code)}
		}, ErrSignInLinkInvalid},
		{"a reset link", func(w *recoveryWorld) PasswordEnrollment {
			l := w.link(schema.SignInLinkPurposeReset, w.bob)
			return PasswordEnrollment{PrincipalID: w.bob.ID, CredentialID: w.bobCred, Record: testRecord("b"), ProofHash: HashSignInLinkCode(l.Code)}
		}, ErrSignInLinkInvalid},
		{"a replayed proof", func(w *recoveryWorld) PasswordEnrollment {
			l := w.link(schema.SignInLinkPurposeEnrollment, w.bob)
			w.exec("UPDATE sign_in_links SET closed_at = ? WHERE id = ?", w.now.UnixMilli(), l.SignInLink.ID)
			return PasswordEnrollment{PrincipalID: w.bob.ID, CredentialID: w.bobCred, Record: testRecord("b"), ProofHash: HashSignInLinkCode(l.Code)}
		}, ErrSignInLinkInvalid},
		{"a dead credential", func(w *recoveryWorld) PasswordEnrollment {
			l := w.link(schema.SignInLinkPurposeEnrollment, w.bob)
			if err := w.s.RevokeCredential(w.ctx, "system", w.bobCred); err != nil {
				w.t.Fatal(err)
			}
			return PasswordEnrollment{PrincipalID: w.bob.ID, CredentialID: w.bobCred, Record: testRecord("b"), ProofHash: HashSignInLinkCode(l.Code)}
		}, ErrLoginStale},
		{"another's credential", func(w *recoveryWorld) PasswordEnrollment {
			l := w.link(schema.SignInLinkPurposeEnrollment, w.bob)
			return PasswordEnrollment{PrincipalID: w.bob.ID, CredentialID: w.aliceA, Record: testRecord("b"), ProofHash: HashSignInLinkCode(l.Code)}
		}, ErrLoginStale},
		{"a password already set", func(w *recoveryWorld) PasswordEnrollment {
			return PasswordEnrollment{PrincipalID: w.alice.ID, CredentialID: w.aliceA, Record: testRecord("a2")}
		}, ErrPasswordAlreadySet},
		{"an operator", func(w *recoveryWorld) PasswordEnrollment {
			c, _, err := w.s.CreateCredential(w.ctx, "system", w.op.ID, "cli", nil)
			if err != nil {
				w.t.Fatal(err)
			}
			return PasswordEnrollment{PrincipalID: w.op.ID, CredentialID: c.ID, Record: testRecord("o")}
		}, ErrSetupOnHost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newRecoveryWorld(t)
			in := tt.in(w)
			before := len(w.audit())
			if _, err := w.s.EnrollPassword(w.ctx, "system", in, w.now); !errors.Is(err, tt.want) {
				t.Fatalf("EnrollPassword = %v, want %v", err, tt.want)
			}
			if w.record(w.bob.ID) != "" || w.record(w.op.ID) != "" {
				t.Error("a refused enrolment stored a password")
			}
			if n := len(w.audit()); n != before {
				t.Errorf("a refused enrolment wrote %d audit rows", n-before)
			}
		})
	}
}

// A member's enrolment spends the proof, rotates the session it came on and
// ends nothing else: the generation stays.
func TestEnrollPassword(t *testing.T) {
	w := newRecoveryWorld(t)
	other, _ := w.session(w.bob.ID)
	l := w.link(schema.SignInLinkPurposeEnrollment, w.bob)
	res, err := w.s.EnrollPassword(w.ctx, principalActor(w.bob.ID), PasswordEnrollment{PrincipalID: w.bob.ID, CredentialID: w.bobCred,
		Record: testRecord("b"), ProofHash: HashSignInLinkCode(l.Code)}, w.now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rotated != w.bobCred || res.Token == "" || res.Bumped || res.Revoked != 0 {
		t.Errorf("result = %+v, want the session rotated and nothing else", res)
	}
	if w.record(w.bob.ID) != testRecord("b") || w.generation(w.bob.ID) != 0 {
		t.Errorf("record %q generation %d, want stored at generation 0", w.record(w.bob.ID), w.generation(w.bob.ID))
	}
	if w.live(w.bobCred) || !w.live(res.Credential.ID) || !w.live(other) {
		t.Error("want the old session dead, the rotated one and bob's other session live")
	}
	if _, err := w.s.EnrollPassword(w.ctx, "system", PasswordEnrollment{PrincipalID: w.bob.ID, CredentialID: res.Credential.ID,
		Record: testRecord("b2"), ProofHash: HashSignInLinkCode(l.Code)}, w.now); !errors.Is(err, ErrPasswordAlreadySet) {
		t.Errorf("second enrolment = %v, want ErrPasswordAlreadySet", err)
	}
}

// One proof, used from several of the member's sessions at once: exactly one
// enrolment stores a password; every other is refused and writes nothing.
func TestEnrollPasswordConcurrently(t *testing.T) {
	w := newRecoveryWorld(t)
	l := w.link(schema.SignInLinkPurposeEnrollment, w.bob)
	const n = 4
	creds := []int64{w.bobCred}
	for len(creds) < n {
		c, _ := w.session(w.bob.ID)
		creds = append(creds, c)
	}
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i, c := range creds {
		wg.Add(1)
		go func(i int, c int64) {
			defer wg.Done()
			_, err := w.s.EnrollPassword(w.ctx, "system", PasswordEnrollment{PrincipalID: w.bob.ID, CredentialID: c,
				Record: testRecord(fmt.Sprint("b", i)), ProofHash: HashSignInLinkCode(l.Code)}, w.now)
			errs <- err
		}(i, c)
	}
	wg.Wait()
	close(errs)
	won := 0
	for err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrPasswordAlreadySet) && !errors.Is(err, ErrSignInLinkInvalid):
			t.Errorf("a racing enrolment = %v", err)
		}
	}
	if won != 1 {
		t.Errorf("%d enrolments won, want 1", won)
	}
	var sets int
	for _, e := range w.audit() {
		if e.Action == AuditPasswordSet && e.Subject == principalActor(w.bob.ID) {
			sets++
		}
	}
	if sets != 1 {
		t.Errorf("password_set rows for bob = %d, want 1", sets)
	}
}

// On a setup-scoped credential the token was the proof: an operator's first
// password ends nothing, and a replacement moves the generation on and
// revokes every other credential while the setup credential carries on.
func TestEnrollPasswordSetupScope(t *testing.T) {
	w := newRecoveryWorld(t)
	setup, _, err := w.s.CreateCredential(w.ctx, "system", w.op.ID, "setup", nil)
	if err != nil {
		t.Fatal(err)
	}
	w.exec("UPDATE credentials SET scope = 'setup' WHERE id = ?", setup.ID)
	script, _, err := w.s.CreateCredential(w.ctx, "system", w.op.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	in := PasswordEnrollment{PrincipalID: w.op.ID, CredentialID: setup.ID, Record: testRecord("o1")}
	res, err := w.s.EnrollPassword(w.ctx, "system", in, w.now)
	if err != nil || res.Bumped || res.Token != "" || res.Rotated != 0 || w.generation(w.op.ID) != 0 || !w.live(script.ID) {
		t.Fatalf("first setup password = %+v, %v; want stored, nothing ended", res, err)
	}
	in.Record = testRecord("o2")
	res, err = w.s.EnrollPassword(w.ctx, "system", in, w.now)
	if err != nil || !res.Bumped || res.Token != "" || res.Revoked < 1 || w.generation(w.op.ID) != 1 {
		t.Fatalf("replacement = %+v, %v; want a bump and the others revoked", res, err)
	}
	if w.live(script.ID) {
		t.Error("the operator's script credential survived the replacement")
	}
	var current bool
	if err := w.s.db.QueryRow(`SELECT c.auth_generation = p.auth_generation AND c.revoked_at IS NULL
		FROM credentials c JOIN principals p ON p.id = c.principal_id WHERE c.id = ?`, setup.ID).Scan(&current); err != nil || !current {
		t.Errorf("the setup credential after the replacement current = %v, %v; want it carried on", current, err)
	}
	if w.record(w.op.ID) != testRecord("o2") {
		t.Error("the replacement was not stored")
	}
}

// A change re-checks everything its verification relied on: any change in
// between is ErrLoginStale and writes nothing; an operator is always
// refused.
func TestChangePasswordRechecks(t *testing.T) {
	tests := []struct {
		name string
		// between runs after the change was read and before it is written.
		between func(w *recoveryWorld)
		cred    func(w *recoveryWorld) int64
		want    error
	}{
		{"the password changed", func(w *recoveryWorld) {
			w.exec("UPDATE passwords SET hash = ? WHERE principal_id = ?", testRecord("other"), w.alice.ID)
		}, func(w *recoveryWorld) int64 { return w.aliceA }, ErrLoginStale},
		{"the generation moved", func(w *recoveryWorld) {
			if _, err := w.s.BumpAuthGeneration(w.ctx, "system", w.alice.ID, "test", w.now); err != nil {
				w.t.Fatal(err)
			}
		}, func(w *recoveryWorld) int64 { return w.aliceA }, ErrLoginStale},
		{"the credential was revoked", func(w *recoveryWorld) {
			if err := w.s.RevokeCredential(w.ctx, "system", w.aliceA); err != nil {
				w.t.Fatal(err)
			}
		}, func(w *recoveryWorld) int64 { return w.aliceA }, ErrLoginStale},
		{"the member was disabled", func(w *recoveryWorld) {
			if _, err := w.s.DisablePrincipal(w.ctx, "system", w.alice.ID); err != nil {
				w.t.Fatal(err)
			}
		}, func(w *recoveryWorld) int64 { return w.aliceA }, ErrPrincipalDisabled},
		{"a setup-scoped credential", func(w *recoveryWorld) {
			w.exec("UPDATE credentials SET scope = 'setup' WHERE id = ?", w.aliceA)
		}, func(w *recoveryWorld) int64 { return w.aliceA }, ErrLoginStale},
		{"another's credential", func(*recoveryWorld) {}, func(w *recoveryWorld) int64 { return w.bobCred }, ErrLoginStale},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newRecoveryWorld(t)
			in := w.change(tt.cred(w), testRecord("new"))
			tt.between(w)
			before := len(w.audit())
			if _, err := w.s.ChangePassword(w.ctx, "system", in, w.now); !errors.Is(err, tt.want) {
				t.Fatalf("ChangePassword = %v, want %v", err, tt.want)
			}
			if w.record(w.alice.ID) == testRecord("new") || len(w.audit()) != before {
				t.Error("a refused change wrote something")
			}
		})
	}
	w := newRecoveryWorld(t)
	c, _, err := w.s.CreateCredential(w.ctx, "system", w.op.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.SetPassword(w.ctx, "system", w.op.ID, testRecord("op"), "test", w.now); err != nil {
		t.Fatal(err)
	}
	in := PasswordChange{PrincipalID: w.op.ID, CredentialID: c.ID, AuthGeneration: w.generation(w.op.ID), Record: testRecord("op"), NewRecord: testRecord("op2")}
	if _, err := w.s.ChangePassword(w.ctx, "system", in, w.now); !errors.Is(err, ErrSecondFactorRequired) {
		t.Errorf("an operator's change = %v, want ErrSecondFactorRequired", err)
	}
}

// A change stores the password, moves the generation on, revokes the other
// credentials, spends every pending login, clears every proof and rotates
// the session it came on, keeping its label and lifetime.
func TestChangePasswordEndsTheRest(t *testing.T) {
	w := newRecoveryWorld(t)
	w.exec(`INSERT INTO pending_logins (challenge_hash, principal_id, address_key, auth_generation, created_at, expires_at)
		VALUES ('h', ?, 'k', 0, ?, ?)`, w.alice.ID, w.now.UnixMilli(), w.now.Add(time.Minute).UnixMilli())
	if _, err := w.s.RecordRecentAuth(w.ctx, LoginIssue{PrincipalID: w.alice.ID, AuthGeneration: 0, Record: testRecord("alice")}, w.aliceB, w.now); err != nil {
		t.Fatal(err)
	}
	res, err := w.s.ChangePassword(w.ctx, principalActor(w.alice.ID), w.change(w.aliceA, testRecord("new")), w.now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Bumped || res.Revoked != 1 || res.Rotated != w.aliceA || res.Token == "" || res.Credential.Label != "browser" || !res.Credential.Renews {
		t.Errorf("result = %+v, want B revoked and A rotated", res)
	}
	if w.record(w.alice.ID) != testRecord("new") || w.generation(w.alice.ID) != 1 {
		t.Error("the change is not stored, or the generation did not move")
	}
	if w.live(w.aliceA) || w.live(w.aliceB) || !w.live(res.Credential.ID) || !w.live(w.bobCred) {
		t.Error("want A and B dead, the rotated session and bob live")
	}
	var spent, proofs int
	if err := w.s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM pending_logins WHERE spent_at IS NOT NULL),
		(SELECT COUNT(*) FROM credentials WHERE recent_auth_until IS NOT NULL)`).Scan(&spent, &proofs); err != nil {
		t.Fatal(err)
	}
	if spent != 1 || proofs != 0 {
		t.Errorf("%d pending logins spent, %d proofs left; want 1 and 0", spent, proofs)
	}
}

// A reset is the link's to make, once: of concurrent uses one wins, and a
// target that may no longer be given a link is refused at completion.
func TestResetPasswordStore(t *testing.T) {
	w := newRecoveryWorld(t)
	l := w.link(schema.SignInLinkPurposeReset, w.alice)
	if p, err := w.s.CheckResetLink(w.ctx, HashSignInLinkCode(l.Code), w.now); err != nil || p.ID != w.alice.ID {
		t.Fatalf("CheckResetLink = %+v, %v", p, err)
	}
	const n = 4
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := w.s.ResetPassword(w.ctx, HashSignInLinkCode(l.Code), testRecord(fmt.Sprint("r", i)), w.now)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	won := 0
	for err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, ErrSignInLinkInvalid):
			t.Errorf("a racing reset = %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d resets won, want 1", won)
	}
	if w.live(w.aliceA) || w.live(w.aliceB) || w.generation(w.alice.ID) != 1 {
		t.Error("the reset left a credential live or the generation in place")
	}

	for _, tt := range []struct {
		name   string
		change string
	}{
		{"made an operator", "UPDATE principals SET role = 'operator' WHERE id = ?"},
		{"disabled", "UPDATE principals SET disabled_at = 1 WHERE id = ?"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newRecoveryWorld(t)
			l := w.link(schema.SignInLinkPurposeReset, w.bob)
			w.exec(tt.change, w.bob.ID)
			if _, err := w.s.ResetPassword(w.ctx, HashSignInLinkCode(l.Code), testRecord("b"), w.now); !errors.Is(err, ErrSignInLinkInvalid) {
				t.Errorf("ResetPassword = %v, want ErrSignInLinkInvalid", err)
			}
			if w.record(w.bob.ID) != "" {
				t.Error("a refused reset stored a password")
			}
		})
	}
}

// The recent-password proof is recorded only on a sign-in session that is
// still live and current, for a verification that still holds, and every
// bump (a sign-out everywhere and a disable among them) ends it.
func TestRecordRecentAuth(t *testing.T) {
	w := newRecoveryWorld(t)
	issue := LoginIssue{PrincipalID: w.alice.ID, AuthGeneration: 0, Record: testRecord("alice")}
	script, _, err := w.s.CreateCredential(w.ctx, "system", w.alice.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.RecordRecentAuth(w.ctx, issue, script.ID, w.now); !errors.Is(err, ErrNotASession) {
		t.Errorf("a script credential = %v, want ErrNotASession", err)
	}
	if _, err := w.s.RecordRecentAuth(w.ctx, LoginIssue{PrincipalID: w.alice.ID, Record: testRecord("stale")}, w.aliceA, w.now); !errors.Is(err, ErrLoginStale) {
		t.Errorf("a stale verification = %v, want ErrLoginStale", err)
	}
	until, err := w.s.RecordRecentAuth(w.ctx, issue, w.aliceA, w.now)
	if err != nil || !until.Equal(w.now.Add(schema.RecentAuthLifetimeSeconds*time.Second)) {
		t.Fatalf("RecordRecentAuth = %v, %v", until, err)
	}
	for _, c := range []struct {
		cred int64
		at   time.Time
		want bool
	}{
		{w.aliceA, w.now, true}, {w.aliceA, until.Add(-time.Millisecond), true}, {w.aliceA, until, false}, {w.aliceB, w.now, false},
	} {
		if got, err := w.s.RecentAuthStands(w.ctx, c.cred, c.at); err != nil || got != c.want {
			t.Errorf("RecentAuthStands(%d, %v) = %v, %v; want %v", c.cred, c.at, got, err, c.want)
		}
	}

	for _, tt := range []struct {
		name string
		end  func(w *recoveryWorld)
	}{
		{"a sign-out everywhere", func(w *recoveryWorld) {
			if _, err := w.s.RevokeAllCredentials(w.ctx, principalActor(w.alice.ID), w.alice.ID); err != nil {
				w.t.Fatal(err)
			}
		}},
		{"a disable", func(w *recoveryWorld) {
			if _, err := w.s.DisablePrincipal(w.ctx, "system", w.alice.ID); err != nil {
				w.t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newRecoveryWorld(t)
			w.exec(`INSERT INTO pending_logins (challenge_hash, principal_id, address_key, auth_generation, created_at, expires_at)
				VALUES ('h', ?, 'k', 0, ?, ?)`, w.alice.ID, w.now.UnixMilli(), w.now.Add(time.Minute).UnixMilli())
			if _, err := w.s.RecordRecentAuth(w.ctx, issue, w.aliceA, w.now); err != nil {
				t.Fatal(err)
			}
			tt.end(w)
			var proofs, unspent int
			if err := w.s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM credentials WHERE recent_auth_until IS NOT NULL),
				(SELECT COUNT(*) FROM pending_logins WHERE spent_at IS NULL)`).Scan(&proofs, &unspent); err != nil {
				t.Fatal(err)
			}
			if proofs != 0 || unspent != 0 || w.generation(w.alice.ID) != 1 {
				t.Errorf("after %s: %d proofs, %d unspent pending logins, generation %d; want 0, 0, 1", tt.name, proofs, unspent, w.generation(w.alice.ID))
			}
		})
	}
}
