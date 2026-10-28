package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Temporary servers (issue #866, ADR-011): the stored mode, the switch back
// to normal, and the store's half of the five places that ask for a code
// only when one is enrolled.

func TestParseMode(t *testing.T) {
	tests := []struct {
		raw     string
		want    Mode
		wantErr bool
	}{
		{"", ModeNormal, false},
		{"normal", ModeNormal, false},
		{"temporary", ModeTemporary, false},
		{"Temporary", "", true},
		{"TEMPORARY", "", true},
		{" temporary", "", true},
		{"temporary\n", "", true},
		{"temp", "", true},
		{"ephemeral", "", true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.raw), func(t *testing.T) {
			got, err := ParseMode(tt.raw)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("ParseMode(%q) = %q, %v; want %q, error %v", tt.raw, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

// modeWorld is the login world (root, an operator and owner with a password
// and an authenticator; wren, a member with a password) with olga, an
// operator with a password and no authenticator.
type modeWorld struct {
	*loginWorld
	olga Principal
}

func newModeWorld(t *testing.T) *modeWorld {
	t.Helper()
	ctx := context.Background()
	w := &modeWorld{loginWorld: newLoginWorld(t)}
	var err error
	if w.olga, err = w.s.CreatePrincipal(ctx, PrincipalHuman, "olga"); err != nil {
		t.Fatal(err)
	}
	w.exec(t, "UPDATE principals SET role = 'operator' WHERE id = ?", w.olga.ID)
	w.olga.Role = RoleOperator
	if _, err := w.s.SetPassword(ctx, "system", w.olga.ID, testRecord("olga"), "test", w.now); err != nil {
		t.Fatal(err)
	}
	return w
}

// setting is a server's start: its mode, and whether that start allows
// password-only operators (store.PasswordOnlyAllowed of its settings).
type setting struct {
	mode         Mode
	passwordOnly bool
}

// The three starts a server can make (ADR-012 item 2): normal requiring the
// authenticator, temporary, and normal with the authenticator optional.
var (
	normalRequired = setting{ModeNormal, false}
	temporary      = setting{ModeTemporary, true}
	normalOptional = setting{ModeNormal, true}
)

func (st setting) String() string {
	return fmt.Sprintf("%s/password_only=%t", st.mode, st.passwordOnly)
}

func (w *modeWorld) start(t *testing.T, st setting) ModeStart {
	t.Helper()
	res, err := w.s.StartMode(context.Background(), st.mode, st.passwordOnly, w.now)
	if err != nil {
		t.Fatalf("StartMode(%s) = %v", st, err)
	}
	return res
}

func (w *modeWorld) stored(t *testing.T) setting {
	t.Helper()
	mode, passwordOnly, err := w.s.StoredMode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return setting{mode, passwordOnly}
}

// session issues id an operator's browser session, as a sign-in would.
func (w *modeWorld) session(t *testing.T, id int64) (int64, string) {
	t.Helper()
	token, err := newCredentialToken()
	if err != nil {
		t.Fatal(err)
	}
	var credID int64
	if err := w.s.withImmediateTx(context.Background(), func(tx execer) error {
		c, err := insertOperatorSessionTx(context.Background(), tx, id, token, w.now)
		credID = c.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return credID, token
}

func (w *modeWorld) live(t *testing.T, token string) bool {
	t.Helper()
	_, _, err := w.s.ResolveRequestCredential(context.Background(), token)
	if err != nil && !errors.Is(err, ErrCredentialInvalid) {
		t.Fatal(err)
	}
	return err == nil
}

func modeRows(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	for _, e := range allAuditEvents(t, s) {
		if e.Action == AuditServerMode {
			out = append(out, e.Actor+" "+e.Subject+" "+e.Detail)
		}
	}
	return out
}

// The first start writes the row and audits it once; the same settings
// again write nothing; any other change is audited with both old and new
// values, and revokes nothing unless password-only stops being allowed:
// normal to temporary, turning password-only on, and temporary to
// normal+optional (ADR-012 item 4) all keep every session.
func TestStartModeTransitions(t *testing.T) {
	tests := []struct {
		name     string
		starts   []setting
		wantRows []string
	}{
		{"first start, normal", []setting{normalRequired},
			[]string{"system server mode=normal password_only=false first_start=true"}},
		{"first start, temporary", []setting{temporary},
			[]string{"system server mode=temporary password_only=true first_start=true"}},
		{"first start, normal+optional", []setting{normalOptional},
			[]string{"system server mode=normal password_only=true first_start=true"}},
		{"the same settings again", []setting{normalRequired, normalRequired, normalRequired},
			[]string{"system server mode=normal password_only=false first_start=true"}},
		{"temporary again", []setting{temporary, temporary},
			[]string{"system server mode=temporary password_only=true first_start=true"}},
		{"normal to temporary", []setting{normalRequired, temporary},
			[]string{"system server mode=normal password_only=false first_start=true", "system server mode=normal->temporary password_only=false->true"}},
		{"turning password-only on", []setting{normalRequired, normalOptional},
			[]string{"system server mode=normal password_only=false first_start=true", "system server mode=normal->normal password_only=false->true"}},
		{"temporary to normal+optional", []setting{temporary, normalOptional},
			[]string{"system server mode=temporary password_only=true first_start=true", "system server mode=temporary->normal password_only=true->true"}},
		{"normal+optional to temporary", []setting{normalOptional, temporary},
			[]string{"system server mode=normal password_only=true first_start=true", "system server mode=normal->temporary password_only=true->true"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newModeWorld(t)
			if got := w.stored(t); got != normalRequired {
				t.Fatalf("stored before any start = %s; want normal, not password-only", got)
			}
			_, wrenTok := mustMemberLogin(t, w)
			_, rootTok := w.session(t, w.op.ID)
			for i, st := range tt.starts {
				res := w.start(t, st)
				if wantChanged := i == 0 || tt.starts[i-1] != st; res.Changed != wantChanged {
					t.Errorf("start %d (%s): Changed = %v, want %v", i, st, res.Changed, wantChanged)
				}
				if res.Ended || res.SessionsRevoked+res.ProofsCleared+res.PendingLoginsSpent+res.SetupCredentialsRevoked != 0 {
					t.Errorf("start %d (%s) ended something: %+v", i, st, res)
				}
				// olga has a password and no authenticator: counted on every
				// start that requires one, changed or not, and on no other.
				if want := map[bool]int64{false: 1, true: 0}[st.passwordOnly]; res.OperatorsWithoutAuthenticator != want {
					t.Errorf("start %d (%s): %d operators without an authenticator, want %d", i, st, res.OperatorsWithoutAuthenticator, want)
				}
				if i > 0 && (res.Previous != tt.starts[i-1].mode || res.PreviousPasswordOnly != tt.starts[i-1].passwordOnly) {
					t.Errorf("start %d (%s): previous = %s/%t", i, st, res.Previous, res.PreviousPasswordOnly)
				}
			}
			if got := modeRows(t, w.s); strings.Join(got, "\n") != strings.Join(tt.wantRows, "\n") {
				t.Errorf("server_mode rows = %q, want %q", got, tt.wantRows)
			}
			if got, want := w.stored(t), tt.starts[len(tt.starts)-1]; got != want {
				t.Errorf("stored = %s, want %s", got, want)
			}
			if !w.live(t, wrenTok) || !w.live(t, rootTok) {
				t.Error("a session ended on a start that ends nothing")
			}
		})
	}
	w := newModeWorld(t)
	if _, err := w.s.StartMode(context.Background(), "ephemeral", true, time.Now()); err == nil {
		t.Error("StartMode took a mode that is not one")
	}
	if _, err := w.s.StartMode(context.Background(), ModeTemporary, false, time.Now()); err == nil {
		t.Error("StartMode took a temporary server that does not allow password-only")
	}
	if got := modeRows(t, w.s); len(got) != 0 {
		t.Errorf("refused starts wrote %q", got)
	}
}

func mustMemberLogin(t *testing.T, w *modeWorld) (int64, string) {
	t.Helper()
	issued, err := w.s.IssueLoginCredential(context.Background(), w.issue(t, w.member), w.now)
	if err != nil {
		t.Fatal(err)
	}
	return issued.Credential.ID, issued.Token
}

// temporaryWorld is a mode world started with password-only allowed
// (temporary, or normal+optional), holding one of everything turning
// password-only off must end or keep (ADR-011 item 5, ADR-012 item 4).
type temporaryWorld struct {
	*modeWorld
	rootTok, olgaTok, wrenTok              string
	rootScript, olgaScript                 string
	restrictedSetup, openSetup             string
	pendingHash                            string
	rootSessionID, olgaSessionID, wrenCred int64
}

func newTemporaryWorld(t *testing.T, from setting) *temporaryWorld {
	t.Helper()
	ctx := context.Background()
	w := &temporaryWorld{modeWorld: newModeWorld(t)}
	w.start(t, from)
	// olga sets her password on a setup credential first: a replacement
	// moves her generation on, which would end what she holds already.
	tok, err := w.s.IssueSetupToken(ctx, w.olga.ID, w.now)
	if err != nil {
		t.Fatal(err)
	}
	setup, err := w.s.RedeemSetupToken(ctx, HashSetupToken(tok.Token), w.now)
	if err != nil {
		t.Fatal(err)
	}
	res, err := w.s.EnrollPassword(ctx, "system", PasswordEnrollment{
		PrincipalID: w.olga.ID, CredentialID: setup.Credential.ID, Record: testRecord("olga-2"),
	}, w.now)
	if err != nil || res.SetupRestricted != setup.Credential.ID || res.SetupEnded != 0 {
		t.Fatalf("EnrollPassword with password-only allowed = %+v, %v; want the setup credential restricted", res, err)
	}
	w.restrictedSetup = setup.Token
	// And a second setup credential, which has set nothing.
	tok, err = w.s.IssueSetupToken(ctx, w.olga.ID, w.now)
	if err != nil {
		t.Fatal(err)
	}
	if setup, err = w.s.RedeemSetupToken(ctx, HashSetupToken(tok.Token), w.now); err != nil {
		t.Fatal(err)
	}
	w.openSetup = setup.Token
	// root, enrolled, signs in with both factors; olga with her password
	// alone; wren as a member. Each proves the password again.
	session, err := w.s.CompleteTOTPLogin(ctx, TOTPLogin{ChallengeHash: w.challenge(t), AddressMatches: true, Check: stepCheck(10)}, w.now)
	if err != nil {
		t.Fatal(err)
	}
	w.rootSessionID, w.rootTok = session.Credential.ID, session.Token
	if _, err := w.s.RecordOperatorRecentAuth(ctx, w.issue(t, w.op), w.rootSessionID, stepCheck(11), w.now); err != nil {
		t.Fatal(err)
	}
	olga, err := w.s.IssuePasswordOnlyOperatorSession(ctx, w.issue(t, w.olga), w.now)
	if err != nil {
		t.Fatalf("IssuePasswordOnlyOperatorSession = %v", err)
	}
	w.olgaSessionID, w.olgaTok = olga.Credential.ID, olga.Token
	if _, err := w.s.RecordPasswordOnlyOperatorRecentAuth(ctx, w.issue(t, w.olga), w.olgaSessionID, w.now); err != nil {
		t.Fatalf("RecordPasswordOnlyOperatorRecentAuth = %v", err)
	}
	w.wrenCred, w.wrenTok = mustMemberLogin(t, w.modeWorld)
	if _, err := w.s.RecordRecentAuth(ctx, w.issue(t, w.member), w.wrenCred, w.now); err != nil {
		t.Fatal(err)
	}
	// Script credentials, and a pending login of root's.
	if _, w.rootScript, err = w.s.CreateCredential(ctx, "system", w.op.ID, "cli", nil); err != nil {
		t.Fatal(err)
	}
	if _, w.olgaScript, err = w.s.CreateCredential(ctx, "system", w.olga.ID, "cli", nil); err != nil {
		t.Fatal(err)
	}
	w.pendingHash = w.challenge(t)
	for what, tok := range w.tokens() {
		if !w.live(t, tok) {
			t.Fatalf("%s is not live before the switch", what)
		}
	}
	return w
}

func (w *temporaryWorld) tokens() map[string]string {
	return map[string]string{
		"root's browser session": w.rootTok, "olga's browser session": w.olgaTok, "wren's session": w.wrenTok,
		"root's script credential": w.rootScript, "olga's script credential": w.olgaScript,
		"the restricted setup credential": w.restrictedSetup, "the open setup credential": w.openSetup,
	}
}

func (w *temporaryWorld) proof(t *testing.T, cred int64) bool {
	t.Helper()
	ok, err := w.s.RecentAuthStands(context.Background(), cred, w.now)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

func (w *temporaryWorld) pendingUsable(t *testing.T) bool {
	t.Helper()
	_, err := w.s.PendingLoginOf(context.Background(), w.pendingHash, w.now)
	if err != nil && !errors.Is(err, ErrChallengeInvalid) {
		t.Fatal(err)
	}
	return err == nil
}

func (w *temporaryWorld) generations(t *testing.T) string {
	t.Helper()
	var g string
	if err := w.s.db.QueryRow("SELECT group_concat(auth_generation) FROM (SELECT auth_generation FROM principals ORDER BY id)").Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

// When password-only stops being allowed (temporary to normal, or
// normal+optional to required), every operator browser session (root's,
// enrolled, too), every recent-auth proof (wren's too), the pending login and
// both setup credentials are dead, in one transaction with one server_mode
// row naming both settings, the counts and the one operator (olga) left with
// a password and no authenticator; script credentials and the member's
// session are alive, and no generation moved.
func TestStartModeBackToNormal(t *testing.T) {
	ctx := context.Background()
	for _, from := range []setting{temporary, normalOptional} {
		t.Run(from.String(), func(t *testing.T) {
			w := newTemporaryWorld(t, from)
			generations := w.generations(t)
			before := len(allAuditEvents(t, w.s))
			res := w.start(t, normalRequired)
			want := ModeStart{Previous: from.mode, PreviousPasswordOnly: true, Changed: true, Ended: true,
				SessionsRevoked: 2, ProofsCleared: 3, PendingLoginsSpent: 1, SetupCredentialsRevoked: 2, OperatorsWithoutAuthenticator: 1}
			if res != want {
				t.Errorf("StartMode = %+v, want %+v", res, want)
			}
			for what, tok := range w.tokens() {
				wantLive := strings.Contains(what, "script") || what == "wren's session"
				if got := w.live(t, tok); got != wantLive {
					t.Errorf("%s live = %v, want %v", what, got, wantLive)
				}
			}
			for _, cred := range []int64{w.rootSessionID, w.olgaSessionID, w.wrenCred} {
				if w.proof(t, cred) {
					t.Errorf("credential %d still holds a recent-auth proof", cred)
				}
			}
			if w.pendingUsable(t) {
				t.Error("the pending login is still usable")
			}
			if got := w.generations(t); got != generations {
				t.Errorf("generations moved: %s, then %s", generations, got)
			}
			rows := modeRows(t, w.s)
			wantRow := fmt.Sprintf("system server mode=%s->normal password_only=true->false sessions_revoked=2 proofs_cleared=3 pending_logins_spent=1 setup_credentials_revoked=2 operators_without_authenticator=1", from.mode)
			if len(rows) != 2 || rows[1] != wantRow {
				t.Errorf("server_mode rows = %q, want the second %q", rows, wantRow)
			}
			var revoked, bumped int
			for _, e := range allAuditEvents(t, w.s)[before:] {
				switch {
				case e.Action == "credential_revoked" && strings.HasSuffix(e.Detail, "reason=password_only_ended") && e.Actor == "system":
					revoked++
				case e.Action == AuditAuthGenerationBumped:
					bumped++
				}
			}
			if revoked != 4 || bumped != 0 {
				t.Errorf("%d credential_revoked rows and %d bumps, want 4 and none", revoked, bumped)
			}
			// From now on olga, with no authenticator, gets nothing without a
			// code.
			if _, err := w.s.IssuePasswordOnlyOperatorSession(ctx, w.issue(t, w.olga), w.now); !errors.Is(err, ErrLoginStale) {
				t.Errorf("a password-only session with the authenticator required = %v, want ErrLoginStale", err)
			}
			// And a second start the same ends nothing more.
			if again := w.start(t, normalRequired); again.Changed {
				t.Errorf("a second start = %+v", again)
			}
		})
	}
	// Temporary to normal+optional keeps all of it: a password still signs
	// operators in (ADR-012 item 4).
	w := newTemporaryWorld(t, temporary)
	if res := w.start(t, normalOptional); res.Ended || !res.Changed {
		t.Errorf("temporary to normal+optional = %+v, want a change that ends nothing", res)
	}
	for what, tok := range w.tokens() {
		if !w.live(t, tok) {
			t.Errorf("%s ended going from temporary to normal+optional", what)
		}
	}
	if !w.proof(t, w.olgaSessionID) || !w.pendingUsable(t) {
		t.Error("a proof or the pending login ended going from temporary to normal+optional")
	}
}

// A switch back to normal that fails part-way writes nothing: the row still
// says temporary, and every session, proof, pending login and setup
// credential is as it was. Once the cause is gone, starting again finishes it.
func TestStartModeAllOrNothing(t *testing.T) {
	for _, tt := range []struct{ name, trigger string }{
		{"while revoking a session", `CREATE TRIGGER fail_mode BEFORE UPDATE OF revoked_at ON credentials WHEN NEW.scope = 'full'
			BEGIN SELECT RAISE(ABORT, 'injected'); END`},
		{"while spending pending logins", `CREATE TRIGGER fail_mode BEFORE UPDATE OF spent_at ON pending_logins
			BEGIN SELECT RAISE(ABORT, 'injected'); END`},
		{"while revoking a setup credential", `CREATE TRIGGER fail_mode BEFORE UPDATE OF revoked_at ON credentials WHEN NEW.scope = 'setup'
			BEGIN SELECT RAISE(ABORT, 'injected'); END`},
		{"while recording the mode", `CREATE TRIGGER fail_mode BEFORE UPDATE ON server_mode
			BEGIN SELECT RAISE(ABORT, 'injected'); END`},
		{"at the audit row", `CREATE TRIGGER fail_mode BEFORE INSERT ON audit_events WHEN NEW.action = 'server_mode'
			BEGIN SELECT RAISE(ABORT, 'injected'); END`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			w := newTemporaryWorld(t, temporary)
			w.exec(t, tt.trigger)
			before := len(allAuditEvents(t, w.s))
			if _, err := w.s.StartMode(ctx, ModeNormal, false, w.now); err == nil || !strings.Contains(err.Error(), "injected") {
				t.Fatalf("StartMode = %v, want the injected failure", err)
			}
			if got := w.stored(t); got != temporary {
				t.Errorf("stored after a failed switch = %s; want temporary", got)
			}
			for what, tok := range w.tokens() {
				if !w.live(t, tok) {
					t.Errorf("%s ended although the switch failed", what)
				}
			}
			for _, cred := range []int64{w.rootSessionID, w.olgaSessionID, w.wrenCred} {
				if !w.proof(t, cred) {
					t.Errorf("credential %d lost its proof although the switch failed", cred)
				}
			}
			if !w.pendingUsable(t) {
				t.Error("the pending login was spent although the switch failed")
			}
			if got := len(allAuditEvents(t, w.s)); got != before {
				t.Errorf("%d audit rows written by a failed switch", got-before)
			}
			w.exec(t, "DROP TRIGGER fail_mode")
			if res := w.start(t, normalRequired); res.SessionsRevoked != 2 || res.SetupCredentialsRevoked != 2 {
				t.Errorf("the retried switch = %+v", res)
			}
		})
	}
}

// The store's half of ADR-011 item 2.1: a password-only proof, session or
// change is let through only on a store whose flag allows password-only
// (temporary, or normal+optional), for an operator with no authenticator. An
// enrolled operator, a store that requires the authenticator and a store no
// server has started on are all asked for the code.
func TestPasswordOnlyOperatorStore(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		start    *setting // nil is a store nobody started
		enrolled bool
		want     bool
	}{
		{"temporary, no authenticator", &temporary, false, true},
		{"temporary, enrolled", &temporary, true, false},
		{"normal+optional, no authenticator", &normalOptional, false, true},
		{"normal+optional, enrolled", &normalOptional, true, false},
		{"normal, no authenticator", &normalRequired, false, false},
		{"normal, enrolled", &normalRequired, true, false},
		{"never started, no authenticator", nil, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newModeWorld(t)
			if tt.start != nil {
				w.start(t, *tt.start)
			}
			if tt.enrolled {
				enrollTOTP(t, w.s, w.olga.ID)
			}
			_, err := w.s.IssuePasswordOnlyOperatorSession(ctx, w.issue(t, w.olga), w.now)
			if (err == nil) != tt.want || (!tt.want && !errors.Is(err, ErrLoginStale)) {
				t.Errorf("IssuePasswordOnlyOperatorSession = %v, want allowed %v", err, tt.want)
			}
			cred, _ := w.session(t, w.olga.ID)
			_, err = w.s.RecordPasswordOnlyOperatorRecentAuth(ctx, w.issue(t, w.olga), cred, w.now)
			if (err == nil) != tt.want || (!tt.want && !errors.Is(err, ErrSecondFactorRequired)) {
				t.Errorf("RecordPasswordOnlyOperatorRecentAuth = %v, want allowed %v", err, tt.want)
			}
			// The code-checking route is unchanged: a nil check is still
			// refused there, whatever the mode.
			if _, err := w.s.RecordOperatorRecentAuth(ctx, w.issue(t, w.olga), cred, nil, w.now); !errors.Is(err, ErrSecondFactorRequired) {
				t.Errorf("RecordOperatorRecentAuth with no check = %v", err)
			}
			issue := w.issue(t, w.olga)
			_, err = w.s.ChangePassword(ctx, "system", PasswordChange{PrincipalID: w.olga.ID, CredentialID: cred,
				AuthGeneration: issue.AuthGeneration, Record: issue.Record, NewRecord: testRecord("olga-new")}, w.now)
			if (err == nil) != tt.want || (!tt.want && !errors.Is(err, ErrSecondFactorRequired)) {
				t.Errorf("ChangePassword with no code = %v, want allowed %v", err, tt.want)
			}
			if tt.enrolled {
				// An enrolled operator's code is still taken, in any mode.
				cred, _ := w.session(t, w.olga.ID)
				if _, err := w.s.RecordOperatorRecentAuth(ctx, w.issue(t, w.olga), cred, stepCheck(20), w.now); err != nil {
					t.Errorf("RecordOperatorRecentAuth with a right code = %v", err)
				}
				issue := w.issue(t, w.olga)
				if _, err := w.s.ChangePassword(ctx, "system", PasswordChange{PrincipalID: w.olga.ID, CredentialID: cred,
					AuthGeneration: issue.AuthGeneration, Record: issue.Record, NewRecord: testRecord("olga-coded"), SecondFactor: stepCheck(21)}, w.now); err != nil {
					t.Errorf("ChangePassword with a right code = %v", err)
				}
			}
			// A member is unchanged.
			wrenCred, _ := mustMemberLogin(t, w)
			if _, err := w.s.RecordRecentAuth(ctx, w.issue(t, w.member), wrenCred, w.now); err != nil {
				t.Errorf("a member's proof = %v", err)
			}
		})
	}
}

// Setting the password on a setup credential: with an authenticator it ends
// the credential whatever the settings; without one, where password-only is
// allowed (temporary, or normal+optional) it completes setup and restricts
// the credential in the same transaction, which then refuses a second
// password and is ended by a confirmed enrolment; where the authenticator is
// required it stays as it is.
func TestEnrollPasswordTemporary(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name           string
		start          setting
		enrolled       bool
		wantRestricted bool
		wantEnded      bool
	}{
		{"temporary, no authenticator", temporary, false, true, false},
		{"temporary, enrolled", temporary, true, false, true},
		{"normal+optional, no authenticator", normalOptional, false, true, false},
		{"normal+optional, enrolled", normalOptional, true, false, true},
		{"normal, no authenticator", normalRequired, false, false, false},
		{"normal, enrolled", normalRequired, true, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newModeWorld(t)
			w.start(t, tt.start)
			tok, err := w.s.IssueSetupToken(ctx, w.olga.ID, w.now)
			if err != nil {
				t.Fatal(err)
			}
			setup, err := w.s.RedeemSetupToken(ctx, HashSetupToken(tok.Token), w.now)
			if err != nil {
				t.Fatal(err)
			}
			if tt.enrolled {
				enrollTOTP(t, w.s, w.olga.ID)
			}
			in := PasswordEnrollment{PrincipalID: w.olga.ID, CredentialID: setup.Credential.ID, Record: testRecord("o1")}
			res, err := w.s.EnrollPassword(ctx, "system", in, w.now)
			if err != nil {
				t.Fatalf("EnrollPassword = %v", err)
			}
			if (res.SetupRestricted != 0) != tt.wantRestricted || (res.SetupEnded != 0) != tt.wantEnded {
				t.Errorf("EnrollPassword = %+v, want restricted %v, ended %v", res, tt.wantRestricted, tt.wantEnded)
			}
			_, cred, err := w.s.ResolveRequestCredential(ctx, setup.Token)
			if tt.wantEnded {
				if err == nil {
					t.Error("the setup credential still resolves after setup completed")
				}
				return
			}
			if err != nil || cred.SetupTOTPOnly != tt.wantRestricted {
				t.Fatalf("the setup credential resolves as %+v, %v; want SetupTOTPOnly %v", cred, err, tt.wantRestricted)
			}
			in.Record = testRecord("o2")
			_, err = w.s.EnrollPassword(ctx, "system", in, w.now)
			switch {
			case tt.wantRestricted && !errors.Is(err, ErrSetupCredentialRestricted):
				t.Errorf("a second password on the restricted credential = %v, want ErrSetupCredentialRestricted", err)
			case !tt.wantRestricted && err != nil:
				t.Errorf("a second password with the authenticator required = %v, want it set as before", err)
			}
			if tt.wantRestricted {
				if w.recordOf(t, w.olga.ID) != testRecord("o1") {
					t.Error("the restricted credential replaced the password")
				}
				// The authenticator step stays on offer, and confirming ends it.
				if err := w.s.BeginTOTPEnrollment(ctx, w.olga.ID, setup.Credential.ID, []byte{1}, w.now); err != nil {
					t.Fatalf("BeginTOTPEnrollment on the restricted credential = %v", err)
				}
				done, err := w.s.ConfirmTOTP(ctx, "system", TOTPConfirmation{PrincipalID: w.olga.ID, CredentialID: setup.Credential.ID,
					Check: stepCheck(5)}, w.now)
				if err != nil || done.SetupEnded != setup.Credential.ID {
					t.Errorf("ConfirmTOTP = %+v, %v; want the setup credential ended", done, err)
				}
			}
		})
	}
}

func (w *modeWorld) recordOf(t *testing.T, id int64) string {
	t.Helper()
	var r string
	if err := w.s.db.QueryRow("SELECT hash FROM passwords WHERE principal_id = ?", id).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Skip (ADR-011 item 2.1) signs a setup credential out, even the only
// credential of the only owner: the lock-out guard counts no setup
// credential, so ending one changes nothing it guards. A full credential of
// that owner is still refused, so the guard itself is as it was.
func TestRevokeOwnSetupCredential(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	res, err := s.BootstrapOperatorWith(ctx, "root", BootstrapOptions{Setup: true})
	if err != nil {
		t.Fatal(err)
	}
	setup, err := s.RedeemSetupToken(ctx, HashSetupToken(res.SetupToken.Token), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeOwnCredential(ctx, "system", setup.Credential.ID); err != nil {
		t.Errorf("signing the setup credential out = %v, want it revoked", err)
	}
	if _, _, err := s.ResolveRequestCredential(ctx, setup.Token); !errors.Is(err, ErrCredentialInvalid) {
		t.Errorf("the signed-out setup credential resolves: %v", err)
	}
	full, _, err := s.CreateCredential(ctx, "system", res.Principal.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeOwnCredential(ctx, "system", full.ID); !errors.Is(err, ErrLastOperator) {
		t.Errorf("revoking the only owner's last credential = %v, want ErrLastOperator", err)
	}
}

// Migration 36 refuses the restriction on anything but a setup credential,
// a mode that is not one, and a temporary server without password-only.
func TestSetupTOTPOnlyIsForSetupCredentials(t *testing.T) {
	w := newModeWorld(t)
	cred, _, err := w.s.CreateCredential(context.Background(), "system", w.op.ID, "cli", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.db.Exec("UPDATE credentials SET setup_totp_only = 1 WHERE id = ?", cred.ID); err == nil {
		t.Error("a full credential was marked setup_totp_only")
	}
	if _, err := w.s.db.Exec("INSERT INTO server_mode (id, mode, password_only, updated_at) VALUES (1, 'ephemeral', 1, 0)"); err == nil {
		t.Error("the mode row took a mode that is not one")
	}
	if _, err := w.s.db.Exec("INSERT INTO server_mode (id, mode, password_only, updated_at) VALUES (1, 'temporary', 0, 0)"); err == nil {
		t.Error("the mode row took a temporary server that does not allow password-only")
	}
}

// newGuardWorld is a server's only owner, root, bootstrapped with a setup
// token (no credential of its own), on a store started with st.
func newGuardWorld(t *testing.T, st setting) (*modeWorld, Principal) {
	t.Helper()
	s := openTestStore(t)
	res, err := s.BootstrapOperatorWith(context.Background(), "root", BootstrapOptions{Setup: true})
	if err != nil {
		t.Fatal(err)
	}
	w := &modeWorld{loginWorld: &loginWorld{s: s, now: time.Now()}}
	w.start(t, st)
	return w, res.Principal
}

func (w *modeWorld) password(t *testing.T, id int64) {
	t.Helper()
	if _, err := w.s.SetPassword(context.Background(), "system", id, testRecord(fmt.Sprint(id)), "test", w.now); err != nil {
		t.Fatal(err)
	}
}

func wantLastOperator(t *testing.T, what string, err error, refused bool) {
	t.Helper()
	switch {
	case refused && !errors.Is(err, ErrLastOperator):
		t.Errorf("%s = %v, want ErrLastOperator", what, err)
	case !refused && err != nil:
		t.Errorf("%s = %v, want it done", what, err)
	}
}

// The lock-out guard counts what the stored flag allows (ADR-011 item 2.1,
// ADR-012 item 3): where password-only is allowed (temporary, or
// normal+optional) an owner with a password can sign in again, so their last
// session may end; where the authenticator is required a password-only
// owner's setup is incomplete and is not counted. An owner with no password
// is counted under no setting, and a setup credential never is.
func TestLastOwnerGuardByMode(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []setting{temporary, normalOptional, normalRequired} {
		allowed := mode.passwordOnly
		// A password-only owner holding one browser session and nothing
		// else. Disabling or hiding the only owner leaves their own factors
		// out, so it is refused in both modes.
		for _, op := range []struct {
			name    string
			do      func(w *modeWorld, root Principal, sess int64) error
			refused bool
		}{
			{"signing the last session out", func(w *modeWorld, _ Principal, sess int64) error {
				return w.s.RevokeOwnCredential(ctx, "system", sess)
			}, !allowed},
			{"an admin revoke of the last session", func(w *modeWorld, _ Principal, sess int64) error {
				return w.s.RevokeCredential(ctx, "system", sess)
			}, !allowed},
			{"signing out everywhere", func(w *modeWorld, root Principal, _ int64) error {
				_, err := w.s.RevokeAllCredentials(ctx, "system", root.ID)
				return err
			}, !allowed},
			{"disabling the only owner", func(w *modeWorld, root Principal, _ int64) error {
				_, err := w.s.DisablePrincipal(ctx, "system", root.ID)
				return err
			}, true},
			{"hiding the only owner", func(w *modeWorld, root Principal, _ int64) error {
				_, err := w.s.HidePrincipal(ctx, "system", root.ID, HideOptions{})
				return err
			}, true},
			// Unmarking (ADR-010's owner mark) the only owner leaves their
			// factors out too, so it is refused under every setting.
			{"unmarking the only owner", func(w *modeWorld, root Principal, _ int64) error {
				_, err := w.s.SetRole(ctx, "system", root.ID, root.ID, RoleOperator, &noOwnerMark)
				return err
			}, true},
		} {
			t.Run(mode.String()+"/a password-only owner/"+op.name, func(t *testing.T) {
				w, root := newGuardWorld(t, mode)
				w.password(t, root.ID)
				sess, _ := w.session(t, root.ID)
				wantLastOperator(t, op.name, op.do(w, root, sess), op.refused)
			})
		}
		t.Run(mode.String()+"/an owner with no password and a setup credential", func(t *testing.T) {
			w, root := newGuardWorld(t, mode)
			tok, err := w.s.IssueSetupToken(ctx, root.ID, w.now)
			if err != nil {
				t.Fatal(err)
			}
			setup, err := w.s.RedeemSetupToken(ctx, HashSetupToken(tok.Token), w.now)
			if err != nil {
				t.Fatal(err)
			}
			script, _, err := w.s.CreateCredential(ctx, "system", root.ID, "cli", nil)
			if err != nil {
				t.Fatal(err)
			}
			// Only the script credential signs root in: no password, and
			// the setup credential is no sign-in.
			wantLastOperator(t, "revoking the last script credential", w.s.RevokeCredential(ctx, "system", script.ID), true)
			_, err = w.s.RevokeAllCredentials(ctx, "system", root.ID)
			wantLastOperator(t, "signing out everywhere", err, true)
			_, err = w.s.DisablePrincipal(ctx, "system", root.ID)
			wantLastOperator(t, "disabling the only owner", err, true)
			// Ending the setup credential changes nothing the guard counts,
			// so Skip works.
			wantLastOperator(t, "signing the setup credential out", w.s.RevokeOwnCredential(ctx, "system", setup.Credential.ID), false)
		})
		// root holds both factors and a session; olga, another owner, has a
		// password alone and no credential. Disabling, hiding, demoting or
		// unmarking root, or signing root out everywhere (and so ending
		// root's session), leaves only olga: allowed exactly where a password
		// alone signs her in.
		for _, op := range []string{"disable", "hide", "demote", "unmark", "revoke-all"} {
			t.Run(mode.String()+"/another password-only owner/"+op, func(t *testing.T) {
				w, root := newGuardWorld(t, mode)
				w.password(t, root.ID)
				enrollTOTP(t, w.s, root.ID)
				olga, err := w.s.CreatePrincipal(ctx, PrincipalHuman, "olga")
				if err != nil {
					t.Fatal(err)
				}
				w.exec(t, "UPDATE principals SET role = 'operator', owner = 1 WHERE id = ?", olga.ID)
				w.password(t, olga.ID)
				w.session(t, root.ID)
				switch op {
				case "disable":
					_, err = w.s.DisablePrincipal(ctx, "system", root.ID)
				case "hide":
					_, err = w.s.HidePrincipal(ctx, "system", root.ID, HideOptions{})
				case "demote":
					_, err = w.s.SetRole(ctx, "system", olga.ID, root.ID, RoleMember, nil)
				case "unmark":
					_, err = w.s.SetRole(ctx, "system", olga.ID, root.ID, RoleOperator, &noOwnerMark)
				case "revoke-all":
					// root's own factors still count after a sign-out
					// everywhere, in either mode.
					_, err = w.s.RevokeAllCredentials(ctx, "system", root.ID)
					wantLastOperator(t, op, err, false)
					return
				}
				wantLastOperator(t, op+" root, leaving a password-only owner", err, !allowed)
			})
		}
		t.Run(mode.String()+"/an enrolled owner's last session", func(t *testing.T) {
			w, root := newGuardWorld(t, mode)
			w.password(t, root.ID)
			enrollTOTP(t, w.s, root.ID)
			sess, _ := w.session(t, root.ID)
			wantLastOperator(t, "signing the last session out", w.s.RevokeOwnCredential(ctx, "system", sess), false)
		})
	}
}

// noOwnerMark is SetRole's owner argument that clears the mark.
var noOwnerMark = false

// guardState is everything a guarded operation could change, as one string:
// both owners' role, mark, disabled and hidden state, every credential and
// whether it is revoked, and the server_mode row.
func (w *modeWorld) guardState(t *testing.T) string {
	t.Helper()
	var principals, creds, mode string
	if err := w.s.db.QueryRow(`SELECT group_concat(id || ':' || role || ':' || owner || ':' || COALESCE(disabled_at, '-') || ':' || COALESCE(hidden_at, '-'), ',')
		FROM (SELECT * FROM principals ORDER BY id)`).Scan(&principals); err != nil {
		t.Fatal(err)
	}
	if err := w.s.db.QueryRow(`SELECT COALESCE(group_concat(id || ':' || COALESCE(revoked_at, '-'), ','), '')
		FROM (SELECT * FROM credentials ORDER BY id)`).Scan(&creds); err != nil {
		t.Fatal(err)
	}
	if err := w.s.db.QueryRow(`SELECT mode || ':' || password_only FROM server_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return principals + " | " + creds + " | " + mode
}

// When password-only stops being allowed (ADR-012 items 3 and 4), the guard
// goes back to the strict rule at once: two owners with a password and no
// authenticator, root holding (after the restart) only a script credential or
// only a fresh session, olga nothing. Every guarded operation that would
// leave no owner with both factors or a live credential is refused, and
// writes nothing: the principals, the credentials and the stored settings
// are as they were. Under password-only the same operations are allowed
// (TestLastOwnerGuardByMode), so this is the flag, read back, doing it.
func TestLastOwnerGuardAfterPasswordOnlyEnds(t *testing.T) {
	ctx := context.Background()
	for _, from := range []setting{temporary, normalOptional} {
		for _, op := range []struct {
			name string
			// script says root keeps a script credential through the
			// restart; otherwise root gets a fresh session after it.
			script bool
			do     func(w *modeWorld, root, olga Principal, cred int64) error
		}{
			{"revoking the last credential", true, func(w *modeWorld, _, _ Principal, cred int64) error {
				return w.s.RevokeCredential(ctx, "system", cred)
			}},
			{"signing out of the last session", false, func(w *modeWorld, _, _ Principal, cred int64) error {
				return w.s.RevokeOwnCredential(ctx, "system", cred)
			}},
			{"signing out everywhere", true, func(w *modeWorld, root, _ Principal, _ int64) error {
				_, err := w.s.RevokeAllCredentials(ctx, "system", root.ID)
				return err
			}},
			{"disabling", true, func(w *modeWorld, root, _ Principal, _ int64) error {
				_, err := w.s.DisablePrincipal(ctx, "system", root.ID)
				return err
			}},
			{"hiding", true, func(w *modeWorld, root, _ Principal, _ int64) error {
				_, err := w.s.HidePrincipal(ctx, "system", root.ID, HideOptions{})
				return err
			}},
			{"demoting", true, func(w *modeWorld, root, olga Principal, _ int64) error {
				_, err := w.s.SetRole(ctx, "system", olga.ID, root.ID, RoleMember, nil)
				return err
			}},
			{"unmarking", true, func(w *modeWorld, root, olga Principal, _ int64) error {
				_, err := w.s.SetRole(ctx, "system", olga.ID, root.ID, RoleOperator, &noOwnerMark)
				return err
			}},
		} {
			t.Run(from.String()+"/"+op.name, func(t *testing.T) {
				w, root := newGuardWorld(t, from)
				w.password(t, root.ID)
				olga, err := w.s.CreatePrincipal(ctx, PrincipalHuman, "olga")
				if err != nil {
					t.Fatal(err)
				}
				w.exec(t, "UPDATE principals SET role = 'operator', owner = 1 WHERE id = ?", olga.ID)
				w.password(t, olga.ID)
				// A password-only session, which the restart ends.
				_, sessTok := w.session(t, root.ID)
				var cred int64
				if op.script {
					c, _, err := w.s.CreateCredential(ctx, "system", root.ID, "cli", nil)
					if err != nil {
						t.Fatal(err)
					}
					cred = c.ID
				}
				res := w.start(t, normalRequired)
				if !res.Ended || res.OperatorsWithoutAuthenticator != 2 {
					t.Fatalf("the restart = %+v, want password-only ended and two operators counted", res)
				}
				if w.live(t, sessTok) {
					t.Fatal("the password-only session survived the restart")
				}
				if !op.script {
					cred, _ = w.session(t, root.ID)
				}
				before := w.guardState(t)
				wantLastOperator(t, op.name, op.do(w, root, olga, cred), true)
				if after := w.guardState(t); after != before {
					t.Errorf("a refused %s changed the store:\n before %s\n after  %s", op.name, before, after)
				}
			})
		}
	}
}
