package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// stepCheck is a TOTPCheck that accepts any code as step, as the server's
// would a right one; the store tests need no cryptography.
func stepCheck(step int64) TOTPCheck {
	return func(_ []byte, lastStep int64) (int64, error) {
		if step <= lastStep {
			return 0, ErrSecondFactorWrong
		}
		return step, nil
	}
}

// wrongCheck refuses every code.
func wrongCheck([]byte, int64) (int64, error) { return 0, ErrSecondFactorWrong }

// errUnreadable stands for a secret the server cannot open.
var errUnreadable = errors.New("test: cannot open the secret")

func unreadableCheck([]byte, int64) (int64, error) { return 0, errUnreadable }

// challenge makes w's operator a pending login bound to address key "addr"
// and returns its hash.
func (w *loginWorld) challenge(t *testing.T) string {
	t.Helper()
	pending, err := w.s.CreatePendingLogin(context.Background(), w.issue(t, w.op), "addr", w.now)
	if err != nil {
		t.Fatal(err)
	}
	return HashChallenge(pending.Challenge)
}

func (w *loginWorld) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func (w *loginWorld) attempts(t *testing.T, hash string) (int, bool) {
	t.Helper()
	var (
		n     int
		spent bool
	)
	if err := w.s.db.QueryRow("SELECT attempts, spent_at IS NOT NULL FROM pending_logins WHERE challenge_hash = ?", hash).Scan(&n, &spent); err != nil {
		t.Fatal(err)
	}
	return n, spent
}

// A right code completes the challenge once: the operator gets a browser
// session with both bounds, the challenge is spent, the step recorded, and
// login and credential_created are audited.
func TestCompleteTOTPLogin(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	hash := w.challenge(t)
	if _, err := w.s.PendingLoginOf(ctx, hash, w.now); err != nil {
		t.Fatalf("PendingLoginOf = %v", err)
	}
	got, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: hash, AddressMatches: true, Check: stepCheck(100)}, w.now)
	if err != nil {
		t.Fatalf("CompleteTOTPLogin = %v", err)
	}
	if got.Principal.ID != w.op.ID || got.Token == "" || got.Credential.Label != OperatorSessionLabel {
		t.Errorf("issued %+v", got)
	}
	if got.Credential.ExpiresAt == nil || !got.Credential.ExpiresAt.Time().Equal(w.now.Truncate(time.Millisecond).Add(12*time.Hour)) {
		t.Errorf("expires_at = %v, want twelve hours on", got.Credential.ExpiresAt)
	}
	if got.Credential.IdleExpiresAt == nil || !got.Credential.IdleExpiresAt.Time().Equal(w.now.Truncate(time.Millisecond).Add(time.Hour)) {
		t.Errorf("idle_expires_at = %v, want an hour on", got.Credential.IdleExpiresAt)
	}
	p, cred, err := w.s.ResolveRequestCredential(ctx, got.Token)
	if err != nil || p.ID != w.op.ID || !cred.Session || !cred.IdleBounded || cred.Scope != schema.CredentialScopeFull {
		t.Errorf("the session resolves as %+v %+v %v", p, cred, err)
	}
	var lastStep int64
	if err := w.s.db.QueryRow("SELECT last_step FROM totp WHERE principal_id = ?", w.op.ID).Scan(&lastStep); err != nil || lastStep != 100 {
		t.Errorf("last_step = %d, %v; want 100", lastStep, err)
	}
	if _, spent := w.attempts(t, hash); !spent {
		t.Error("the challenge was not spent")
	}
	if _, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: hash, AddressMatches: true, Check: stepCheck(101)}, w.now); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("a second completion = %v, want ErrChallengeInvalid", err)
	}
	var actions []string
	for _, e := range allAuditEvents(t, w.s) {
		actions = append(actions, e.Action)
	}
	joined := strings.Join(actions, " ")
	if !strings.HasSuffix(joined, AuditLogin+" credential_created") {
		t.Errorf("audit ends %q, want login credential_created", joined)
	}
}

// Every way a completion can fail, and what it leaves behind: a wrong code
// or address is an attempt, counted and committed; a change to the operator
// is stale and writes nothing; a check that cannot run counts nothing.
func TestCompleteTOTPLoginRefusals(t *testing.T) {
	tests := []struct {
		name     string
		between  func(t *testing.T, w *loginWorld)
		address  bool
		check    TOTPCheck
		want     error
		attempts int
	}{
		{"a wrong code", func(*testing.T, *loginWorld) {}, true, wrongCheck, ErrSecondFactorWrong, 1},
		{"another address", func(*testing.T, *loginWorld) {}, false, stepCheck(100), ErrSecondFactorWrong, 1},
		{"a replayed step", func(t *testing.T, w *loginWorld) {
			w.exec(t, "UPDATE totp SET last_step = 100 WHERE principal_id = ?", w.op.ID)
		}, true, stepCheck(100), ErrSecondFactorWrong, 1},
		{"the secret cannot be opened", func(*testing.T, *loginWorld) {}, true, unreadableCheck, errUnreadable, 0},
		{"the operator was disabled", func(t *testing.T, w *loginWorld) {
			w.exec(t, "UPDATE principals SET disabled_at = 1 WHERE id = ?", w.op.ID)
		}, true, stepCheck(100), ErrLoginStale, 0},
		{"the operator is a member now", func(t *testing.T, w *loginWorld) {
			w.exec(t, "UPDATE principals SET role = 'member' WHERE id = ?", w.op.ID)
		}, true, stepCheck(100), ErrLoginStale, 0},
		{"the second factor was removed", func(t *testing.T, w *loginWorld) {
			w.exec(t, "DELETE FROM totp WHERE principal_id = ?", w.op.ID)
		}, true, stepCheck(100), ErrLoginStale, 0},
		{"the generation moved", func(t *testing.T, w *loginWorld) {
			w.exec(t, "UPDATE principals SET auth_generation = auth_generation + 1 WHERE id = ?", w.op.ID)
		}, true, stepCheck(100), ErrLoginStale, 0},
		{"the challenge expired", func(t *testing.T, w *loginWorld) {
			w.exec(t, "UPDATE pending_logins SET expires_at = ?", w.now.UnixMilli())
		}, true, stepCheck(100), ErrChallengeInvalid, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newLoginWorld(t)
			hash := w.challenge(t)
			tt.between(t, w)
			before := w.credentials(t)
			_, err := w.s.CompleteTOTPLogin(context.Background(), TOTPLogin{ChallengeHash: hash, AddressMatches: tt.address, Check: tt.check}, w.now)
			if !errors.Is(err, tt.want) {
				t.Fatalf("CompleteTOTPLogin = %v, want %v", err, tt.want)
			}
			if w.credentials(t) != before {
				t.Error("a refused completion issued a credential")
			}
			if n, _ := w.attempts(t, hash); n != tt.attempts {
				t.Errorf("attempts = %d, want %d", n, tt.attempts)
			}
		})
	}
}

// A challenge takes schema.MaxTOTPAttemptsPerChallenge wrong codes and is
// spent at the last: a right code after that is refused.
func TestCompleteTOTPLoginAttemptBound(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	hash := w.challenge(t)
	for i := 1; i <= schema.MaxTOTPAttemptsPerChallenge; i++ {
		if _, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: hash, AddressMatches: true, Check: wrongCheck}, w.now); !errors.Is(err, ErrSecondFactorWrong) {
			t.Fatalf("attempt %d = %v", i, err)
		}
		n, spent := w.attempts(t, hash)
		if n != i || spent != (i == schema.MaxTOTPAttemptsPerChallenge) {
			t.Fatalf("after %d: attempts %d spent %v", i, n, spent)
		}
	}
	if _, err := w.s.PendingLoginOf(ctx, hash, w.now); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("PendingLoginOf a spent challenge = %v", err)
	}
	if _, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: hash, AddressMatches: true, Check: stepCheck(100)}, w.now); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("a right code after the last attempt = %v, want ErrChallengeInvalid", err)
	}
}

// One code, used at a sign-in and a re-authentication at once, passes
// once: the step is taken in the transaction that checks it (design §5.3).
func TestTOTPStepConsumedOnceAcrossLoginAndReauth(t *testing.T) {
	for round := range 20 {
		ctx := context.Background()
		w := newLoginWorld(t)
		session, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: w.challenge(t), AddressMatches: true, Check: stepCheck(10)}, w.now)
		if err != nil {
			t.Fatal(err)
		}
		hash := w.challenge(t)
		var (
			wg   sync.WaitGroup
			errs [2]error
		)
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, errs[0] = w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: hash, AddressMatches: true, Check: stepCheck(11)}, w.now)
		}()
		go func() {
			defer wg.Done()
			_, errs[1] = w.s.RecordOperatorRecentAuth(ctx, w.issue(t, w.op), session.Credential.ID, stepCheck(11), w.now)
		}()
		wg.Wait()
		ok := 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case !errors.Is(err, ErrSecondFactorWrong):
				t.Fatalf("round %d: unexpected %v", round, err)
			}
		}
		if ok != 1 {
			t.Fatalf("round %d: %d of the two uses of one step passed (%v), want exactly 1", round, ok, errs)
		}
	}
}

// An operator's proof needs the code; a wrong one writes nothing.
func TestRecordOperatorRecentAuth(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	session, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: w.challenge(t), AddressMatches: true, Check: stepCheck(10)}, w.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.RecordOperatorRecentAuth(ctx, w.issue(t, w.op), session.Credential.ID, nil, w.now); !errors.Is(err, ErrSecondFactorRequired) {
		t.Errorf("no check = %v, want ErrSecondFactorRequired", err)
	}
	if _, err := w.s.RecordOperatorRecentAuth(ctx, w.issue(t, w.op), session.Credential.ID, wrongCheck, w.now); !errors.Is(err, ErrSecondFactorWrong) {
		t.Errorf("a wrong code = %v, want ErrSecondFactorWrong", err)
	}
	if ok, _ := w.s.RecentAuthStands(ctx, session.Credential.ID, w.now); ok {
		t.Error("a refused proof stands")
	}
	if _, err := w.s.RecordOperatorRecentAuth(ctx, w.issue(t, w.op), session.Credential.ID, stepCheck(11), w.now); err != nil {
		t.Fatalf("a right code = %v", err)
	}
	if ok, _ := w.s.RecentAuthStands(ctx, session.Credential.ID, w.now); !ok {
		t.Error("the proof does not stand")
	}
	// A script credential of the operator gets none (the S15 boundary).
	script, _, err := w.s.CreateCredential(ctx, "system", w.op.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.RecordOperatorRecentAuth(ctx, w.issue(t, w.op), script.ID, stepCheck(12), w.now); !errors.Is(err, ErrNotASession) {
		t.Errorf("a script credential = %v, want ErrNotASession", err)
	}
}

// An operator's change takes the code in the writing transaction, and the
// rotated session keeps its absolute expiry and its idle bound.
func TestChangePasswordOperator(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	session, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: w.challenge(t), AddressMatches: true, Check: stepCheck(10)}, w.now)
	if err != nil {
		t.Fatal(err)
	}
	issue := w.issue(t, w.op)
	in := PasswordChange{PrincipalID: w.op.ID, CredentialID: session.Credential.ID, AuthGeneration: issue.AuthGeneration,
		Record: issue.Record, NewRecord: testRecord("new")}
	in.SecondFactor = wrongCheck
	if _, err := w.s.ChangePassword(ctx, "system", in, w.now); !errors.Is(err, ErrSecondFactorWrong) {
		t.Fatalf("a wrong code = %v", err)
	}
	in.SecondFactor = stepCheck(11)
	later := w.now.Add(time.Minute)
	res, err := w.s.ChangePassword(ctx, "system", in, later)
	if err != nil {
		t.Fatalf("ChangePassword = %v", err)
	}
	if res.Credential.ExpiresAt == nil || !res.Credential.ExpiresAt.Time().Equal(session.Credential.ExpiresAt.Time()) {
		t.Errorf("rotated expiry %v, want the session's %v", res.Credential.ExpiresAt, session.Credential.ExpiresAt)
	}
	if res.Credential.IdleExpiresAt == nil || !res.Credential.IdleExpiresAt.Time().Equal(later.Truncate(time.Millisecond).Add(time.Hour)) {
		t.Errorf("rotated idle bound %v, want an hour from the change", res.Credential.IdleExpiresAt)
	}
	if _, cred, err := w.s.ResolveRequestCredential(ctx, res.Token); err != nil || !cred.IdleBounded {
		t.Errorf("the rotated session resolves as %+v, %v", cred, err)
	}
}

// setupWorld is an operator with a password and no second factor, as one
// made before passwords is once a password is set, and the setup credential
// a setup token answers.
type setupWorld struct {
	*loginWorld
	setup RedeemedSetup
}

func newSetupWorld(t *testing.T, password bool) *setupWorld {
	t.Helper()
	ctx := context.Background()
	w := newLoginWorld(t)
	w.exec(t, "DELETE FROM totp WHERE principal_id = ?", w.op.ID)
	if !password {
		w.exec(t, "DELETE FROM passwords WHERE principal_id = ?", w.op.ID)
	}
	tok, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now)
	if err != nil {
		t.Fatal(err)
	}
	setup, err := w.s.RedeemSetupToken(ctx, HashSetupToken(tok.Token), w.now)
	if err != nil {
		t.Fatal(err)
	}
	return &setupWorld{loginWorld: w, setup: setup}
}

func (w *setupWorld) revoked(t *testing.T, id int64) bool {
	t.Helper()
	var revoked bool
	if err := w.s.db.QueryRow("SELECT revoked_at IS NOT NULL FROM credentials WHERE id = ?", id).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	return revoked
}

// A setup token is spent once for a setup credential of fifteen minutes,
// which only ResolveRequestCredential resolves; a token that is spent,
// expired, replaced or of an old generation redeems nothing.
func TestSetupTokens(t *testing.T) {
	ctx := context.Background()
	w := newSetupWorld(t, true)
	c := w.setup.Credential
	if c.Scope != schema.CredentialScopeSetup || c.Label != SetupCredentialLabel || c.ExpiresAt == nil ||
		!c.ExpiresAt.Time().Equal(w.now.Truncate(time.Millisecond).Add(15*time.Minute)) {
		t.Errorf("setup credential %+v", c)
	}
	if !w.setup.PasswordSet || w.setup.TOTPEnrolled {
		t.Errorf("setup state %+v", w.setup)
	}
	if _, err := w.s.ResolveCredential(ctx, w.setup.Token); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("ResolveCredential(setup) = %v, want ErrCredentialInvalid", err)
	}
	if _, cred, err := w.s.ResolveRequestCredential(ctx, w.setup.Token); err != nil || cred.Scope != schema.CredentialScopeSetup || cred.Session {
		t.Errorf("ResolveRequestCredential(setup) = %+v, %v", cred, err)
	}

	tests := []struct {
		name string
		make func(t *testing.T) string
	}{
		{"spent", func(t *testing.T) string {
			tok, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.s.RedeemSetupToken(ctx, HashSetupToken(tok.Token), w.now); err != nil {
				t.Fatal(err)
			}
			return tok.Token
		}},
		{"expired", func(t *testing.T) string {
			tok, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now.Add(-11*time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			return tok.Token
		}},
		{"replaced by a newer one", func(t *testing.T) string {
			tok, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now); err != nil {
				t.Fatal(err)
			}
			return tok.Token
		}},
		{"of an old generation", func(t *testing.T) string {
			tok, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.s.BumpAuthGeneration(ctx, "system", w.op.ID, "test", w.now); err != nil {
				t.Fatal(err)
			}
			return tok.Token
		}},
		{"for an operator disabled since", func(t *testing.T) string {
			tok, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now)
			if err != nil {
				t.Fatal(err)
			}
			w.exec(t, "UPDATE principals SET disabled_at = 1 WHERE id = ?", w.op.ID)
			t.Cleanup(func() { w.exec(t, "UPDATE principals SET disabled_at = NULL WHERE id = ?", w.op.ID) })
			return tok.Token
		}},
		{"unknown", func(*testing.T) string { return schema.SetupTokenPrefix + strings.Repeat("A", 43) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok := tt.make(t)
			before := w.credentials(t)
			if _, err := w.s.RedeemSetupToken(ctx, HashSetupToken(tok), w.now); !errors.Is(err, ErrSetupTokenInvalid) {
				t.Errorf("RedeemSetupToken = %v, want ErrSetupTokenInvalid", err)
			}
			if w.credentials(t) != before {
				t.Error("a refused token issued a credential")
			}
		})
	}
}

// Who may be given a setup token on the host without recovery: an operator
// whose setup is incomplete.
func TestIssueSetupTokenRefusals(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	if _, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now); !errors.Is(err, ErrSetupComplete) {
		t.Errorf("a complete operator = %v, want ErrSetupComplete", err)
	}
	if _, err := w.s.IssueSetupToken(ctx, w.member.ID, w.now); !errors.Is(err, ErrNotAnOperator) {
		t.Errorf("a member = %v, want ErrNotAnOperator", err)
	}
	if _, err := w.s.IssueSetupToken(ctx, 9999, w.now); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("nobody = %v, want ErrPrincipalNotFound", err)
	}
	w.exec(t, "DELETE FROM totp WHERE principal_id = ?", w.op.ID)
	w.exec(t, "UPDATE principals SET disabled_at = 1 WHERE id = ?", w.op.ID)
	if _, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now); !errors.Is(err, ErrPrincipalDisabled) {
		t.Errorf("a disabled operator = %v, want ErrPrincipalDisabled", err)
	}
}

// Enrolment is pending until a code confirms it; a second enrolment replaces
// a pending one; the confirmation records the code's step and, with a
// password set, completes setup by revoking the setup credential.
func TestTOTPEnrollmentAndConfirm(t *testing.T) {
	for _, password := range []bool{true, false} {
		t.Run(map[bool]string{true: "with a password", false: "before the password"}[password], func(t *testing.T) {
			ctx := context.Background()
			w := newSetupWorld(t, password)
			id, cred := w.op.ID, w.setup.Credential.ID
			if _, err := w.s.ConfirmTOTP(ctx, "system", TOTPConfirmation{id, cred, stepCheck(5)}, w.now); !errors.Is(err, ErrTOTPNotEnrolled) {
				t.Errorf("confirm before enrolling = %v, want ErrTOTPNotEnrolled", err)
			}
			if err := w.s.BeginTOTPEnrollment(ctx, id, cred, []byte("first"), w.now); err != nil {
				t.Fatal(err)
			}
			if err := w.s.BeginTOTPEnrollment(ctx, id, cred, []byte("second"), w.now); err != nil {
				t.Fatal(err)
			}
			var seen []byte
			check := func(sealed []byte, last int64) (int64, error) {
				seen = sealed
				return stepCheck(5)(sealed, last)
			}
			if _, err := w.s.ConfirmTOTP(ctx, "system", TOTPConfirmation{id, cred, wrongCheck}, w.now); !errors.Is(err, ErrSecondFactorWrong) {
				t.Errorf("a wrong code = %v", err)
			}
			res, err := w.s.ConfirmTOTP(ctx, "system", TOTPConfirmation{id, cred, check}, w.now)
			if err != nil {
				t.Fatalf("ConfirmTOTP = %v", err)
			}
			if !bytes.Equal(seen, []byte("second")) {
				t.Errorf("the code was checked against %q, want the latest enrolment", seen)
			}
			if (res.SetupEnded != 0) != password || w.revoked(t, cred) != password {
				t.Errorf("setup ended %d, credential revoked %v; want both %v", res.SetupEnded, w.revoked(t, cred), password)
			}
			var lastStep int64
			if err := w.s.db.QueryRow("SELECT last_step FROM totp WHERE principal_id = ?", id).Scan(&lastStep); err != nil || lastStep != 5 {
				t.Errorf("last_step = %d, %v; want 5", lastStep, err)
			}
			if !password {
				if err := w.s.BeginTOTPEnrollment(ctx, id, cred, []byte("third"), w.now); !errors.Is(err, ErrTOTPAlreadyEnrolled) {
					t.Errorf("enrol again = %v, want ErrTOTPAlreadyEnrolled", err)
				}
				// The password, set after, completes the setup.
				up, err := w.s.EnrollPassword(ctx, "system", PasswordEnrollment{PrincipalID: id, CredentialID: cred, Record: testRecord("op")}, w.now)
				if err != nil || up.SetupEnded != cred || !w.revoked(t, cred) {
					t.Errorf("EnrollPassword after the second factor = %+v, %v; want the setup credential ended", up, err)
				}
			}
		})
	}
}

// Only an operator's setup credential enrols (review F7).
func TestTOTPEnrollmentRefusals(t *testing.T) {
	ctx := context.Background()
	w := newSetupWorld(t, true)
	script, _, err := w.s.CreateCredential(ctx, "system", w.op.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.s.BeginTOTPEnrollment(ctx, w.op.ID, script.ID, []byte("x"), w.now); !errors.Is(err, ErrSetupOnHost) {
		t.Errorf("a script credential = %v, want ErrSetupOnHost", err)
	}
	member, _, err := w.s.CreateCredential(ctx, "system", w.member.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.s.BeginTOTPEnrollment(ctx, w.member.ID, member.ID, []byte("x"), w.now); !errors.Is(err, ErrSetupOnHost) {
		t.Errorf("a member = %v, want ErrSetupOnHost", err)
	}
	if err := w.s.BeginTOTPEnrollment(ctx, w.op.ID, w.setup.Credential.ID, []byte("x"), w.now.Add(16*time.Minute)); !errors.Is(err, ErrLoginStale) {
		t.Errorf("an expired setup credential = %v, want ErrLoginStale", err)
	}
}

// Recovery revokes every credential, removes the second factor, ends every
// pending login and moves the generation on, audits it all and makes a
// setup token that works; the password stays until it is replaced.
func TestRecoverOperator(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	session, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: w.challenge(t), AddressMatches: true, Check: stepCheck(10)}, w.now)
	if err != nil {
		t.Fatal(err)
	}
	pending := w.challenge(t)
	old, err := w.s.IssueSetupToken(ctx, w.op.ID, w.now)
	if !errors.Is(err, ErrSetupComplete) {
		t.Fatalf("IssueSetupToken = %+v, %v", old, err)
	}
	rec, err := w.s.RecoverOperator(ctx, w.op.ID, w.now)
	if err != nil {
		t.Fatalf("RecoverOperator = %v", err)
	}
	if rec.Revoked < 2 || rec.Setup.Token == "" || !strings.HasPrefix(rec.Setup.Token, schema.SetupTokenPrefix) {
		t.Errorf("recovery %+v", rec)
	}
	if live, _ := w.s.CredentialLive(ctx, session.Credential.ID); live {
		t.Error("the operator's session survived recovery")
	}
	if _, err := w.s.PendingLoginOf(ctx, pending, w.now); !errors.Is(err, ErrChallengeInvalid) {
		t.Errorf("a pending login survived recovery: %v", err)
	}
	if c, _, _ := w.s.LoginCandidate(ctx, "root"); c.TOTPEnrolled || c.Record == "" {
		t.Errorf("after recovery: second factor %v, password %q; want none and kept", c.TOTPEnrolled, c.Record)
	}
	actions := map[string]bool{}
	for _, e := range allAuditEvents(t, w.s) {
		actions[e.Action] = true
		for _, secret := range []string{rec.Setup.Token, session.Token} {
			if strings.Contains(e.Detail, secret) || strings.Contains(e.Detail, HashSetupToken(secret)) {
				t.Errorf("audit %s holds a secret", e.Action)
			}
		}
	}
	for _, want := range []string{AuditOperatorRecovered, AuditAuthGenerationBumped, AuditSetupTokenIssued, "credential_revoked"} {
		if !actions[want] {
			t.Errorf("no %s row", want)
		}
	}
	if _, err := w.s.RedeemSetupToken(ctx, HashSetupToken(rec.Setup.Token), w.now); err != nil {
		t.Errorf("the recovery's token = %v", err)
	}
	if _, err := w.s.RecoverOperator(ctx, w.member.ID, w.now); !errors.Is(err, ErrNotAnOperator) {
		t.Errorf("recovering a member = %v, want ErrNotAnOperator", err)
	}
}

// The first operator made with a setup token has no credential at all
// until setup is done.
func TestBootstrapOperatorSetup(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	res, err := s.BootstrapOperatorWith(ctx, "root", BootstrapOptions{Setup: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Token != "" || res.Credential.ID != 0 || res.SetupToken.Token == "" || res.SetupToken.PrincipalID != res.Principal.ID {
		t.Errorf("bootstrap %+v", res)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM credentials").Scan(&n); err != nil || n != 0 {
		t.Errorf("%d credentials, %v; want none", n, err)
	}
	got, err := s.RedeemSetupToken(ctx, HashSetupToken(res.SetupToken.Token), time.Now())
	if err != nil || got.PasswordSet || got.TOTPEnrolled {
		t.Errorf("redeem = %+v, %v", got, err)
	}
}

// The sweep deletes spent and expired challenges and keeps live ones.
func TestSweepPendingLogins(t *testing.T) {
	ctx := context.Background()
	w := newLoginWorld(t)
	live, spent, expired := w.challenge(t), w.challenge(t), w.challenge(t)
	w.exec(t, "UPDATE pending_logins SET spent_at = 1 WHERE challenge_hash = ?", spent)
	w.exec(t, "UPDATE pending_logins SET expires_at = ? WHERE challenge_hash = ?", w.now.UnixMilli(), expired)
	n, err := w.s.SweepPendingLogins(ctx, w.now)
	if err != nil || n != 2 {
		t.Fatalf("SweepPendingLogins = %d, %v; want 2", n, err)
	}
	if _, err := w.s.PendingLoginOf(ctx, live, w.now); err != nil {
		t.Errorf("the live challenge was swept: %v", err)
	}
}
