package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func looseLoginLimits() loginLimits {
	loose := inviteLimit{"loose", 1_000_000, time.Minute}
	return loginLimits{failuresPerAddress: loose, failuresPerName: loose, successesPerName: loose, nameProbesPerAddress: loose,
		nameProbesPerPrincipal: loose, conversionNameProbesPerAddress: loose, conversionsPerPrincipal: loose}
}

// The passwords of the login fixture's people. Each passes the policy.
const (
	alicePassword = "alice's long and private passphrase"
	rootPassword  = "the operator's long and private passphrase"
)

// loginFixture is the invite fixture with alice and root holding passwords,
// and the sign-in limits loose unless a test sets its own.
type loginFixture struct {
	*inviteFixture
}

func newLoginFixture(t *testing.T, cfg Config) *loginFixture {
	t.Helper()
	f := &loginFixture{newInviteFixture(t, cfg)}
	f.setPassword(t, f.alice.ID, alicePassword)
	f.setPassword(t, f.root.ID, rootPassword)
	return f
}

// setPassword stores password for id as enrolment would: normalized, hashed
// by the server's hasher, through the store's primitive.
func (f *loginFixture) setPassword(t *testing.T, id int64, password string) {
	t.Helper()
	n, err := normalizePassword(password)
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.srv.passwords.hasher.hash(n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.SetPassword(context.Background(), "system", id, record, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// exec runs a statement on the fixture's database file, underneath the
// store: second-factor enrolment is not this issue's, and the sign-in only
// reads whether one is confirmed.
func (f *loginFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

// queryString reads one text value from the fixture's database file.
func (f *loginFixture) queryString(t *testing.T, query string) string {
	t.Helper()
	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var out string
	if err := db.QueryRowContext(context.Background(), query).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *loginFixture) enrollTOTP(t *testing.T, id int64) {
	t.Helper()
	f.exec(t, "INSERT INTO totp (principal_id, secret, enrolled_at) VALUES (?, x'00', 1)", id)
}

func loginJSON(t *testing.T, name, password string) string {
	t.Helper()
	return inviteJSON(t, schema.LoginRequestV1{Name: name, Password: password})
}

// login signs in from remote (a test address when empty), over HTTPS.
func (f *loginFixture) login(t *testing.T, name, password, remote string) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", "/v1/login", "", loginJSON(t, name, password), remote)
}

func (f *loginFixture) credentialCount(t *testing.T, id int64) int {
	t.Helper()
	list, err := f.srv.store.ListCredentials(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

func loginFailedRows(t *testing.T, f *authFixture) []string {
	t.Helper()
	var out []string
	for _, e := range auditRows(t, f, auditLoginFailed) {
		out = append(out, e.Actor+" "+e.Subject+" "+e.Detail)
	}
	return out
}

// A member's right password, in any case of the name, signs in: the answer
// carries a live member credential, sent no-store, and login is audited.
func TestLoginMember(t *testing.T) {
	f := newLoginFixture(t, Config{})
	for _, name := range []string{"alice", "ALICE", "Alice"} {
		t.Run(name, func(t *testing.T) {
			rec := f.login(t, name, alicePassword, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("login = %d %s", rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			res := decodeBody[schema.LoginResponseV1](t, rec)
			if err := res.Validate(); err != nil || res.Next != schema.LoginNextSignedIn {
				t.Fatalf("response %+v: %v", res, err)
			}
			if res.Principal.ID != f.alice.ID || res.Credential.Label != "password sign-in" || !res.Credential.Renews {
				t.Errorf("response principal %+v credential %+v", res.Principal, res.Credential)
			}
			who := decodeBody[schema.WhoAmIResponseV1](t, f.send(t, "GET", "/v1/whoami", res.Token, "", ""))
			if who.ID != f.alice.ID || who.Role != schema.RoleMember {
				t.Errorf("whoami with the new token = %+v", who)
			}
		})
	}
	if n := len(auditRows(t, f.authFixture, store.AuditLogin)); n != 3 {
		t.Errorf("%d login rows, want 3", n)
	}
	if rows := loginFailedRows(t, f.authFixture); len(rows) != 0 {
		t.Errorf("login_failed rows for successes: %q", rows)
	}
}

// Every sign-in that does not succeed gets the same status and the same
// bytes, issues nothing, and is audited with its reason and, only for a name
// that names a principal, its key.
func TestLoginFailuresAreUniform(t *testing.T) {
	f := newLoginFixture(t, Config{})
	ctx := context.Background()
	mk := func(name string) store.Principal {
		p, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	disabled, hidden, opNoTOTP := mk("dora"), mk("hal"), mk("olga")
	for _, p := range []store.Principal{disabled, hidden, opNoTOTP} {
		f.setPassword(t, p.ID, "a password that is right for them")
	}
	if _, err := f.srv.store.DisablePrincipal(ctx, "system", disabled.ID); err != nil {
		t.Fatal(err)
	}
	hid, err := f.srv.store.HidePrincipal(ctx, "system", hidden.ID, store.HideOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Hiding renames: the hidden principal answers to its new name.
	hiddenNow := hid.Principal.Name
	f.exec(t, "UPDATE principals SET role = 'operator' WHERE id = ?", opNoTOTP.ID)

	right := "a password that is right for them"
	cases := []struct {
		name, login, password, reason, key string
	}{
		{"unknown name", "nobody-here", alicePassword, loginReasonUnknown, loginReasonUnknown},
		{"wrong password", "alice", "not alice's password at all", loginReasonWrongPassword, "alice"},
		{"short wrong password", "alice", "x", loginReasonWrongPassword, "alice"},
		{"no password", "ghost", alicePassword, loginReasonNoPassword, "ghost"},
		{"agent", "bot", alicePassword, loginReasonNotAPerson, "bot"},
		{"disabled", "dora", right, loginReasonDisabled, "dora"},
		{"hidden", hiddenNow, right, loginReasonHidden, hiddenNow},
		{"operator without a second factor", "olga", right, loginReasonSetupIncomplete, "olga"},
		{"operator, wrong password", "root", alicePassword, loginReasonWrongPassword, "root"},
		{"a guest handle", "guest-abcdefghjkmnpqrs", alicePassword, loginReasonUnknown, loginReasonUnknown},
	}
	var want []byte
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			before := len(f.audit(t))
			rec := f.login(t, tt.login, tt.password, "")
			assertErrorBody(t, rec, http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			if want == nil {
				want = rec.Body.Bytes()
			}
			if !bytes.Equal(rec.Body.Bytes(), want) {
				t.Errorf("body %q differs from %q", rec.Body.Bytes(), want)
			}
			if rec.Header().Get("WWW-Authenticate") != "" || rec.Header().Get("Retry-After") != "" {
				t.Errorf("headers %v: a failure carries no hint", rec.Header())
			}
			var rows []string
			for _, e := range f.audit(t)[before:] {
				rows = append(rows, e.Action+" "+e.Detail)
			}
			// The count is the thinning's: two cases share a name and a reason.
			wantRow := fmt.Sprintf("%s name_key=%q reason=%s count=", auditLoginFailed, tt.key, tt.reason)
			if len(rows) != 1 || !strings.HasPrefix(rows[0], wantRow) {
				t.Errorf("audit %q, want only %q", rows, wantRow)
			}
		})
	}
	if n := len(auditRows(t, f.authFixture, store.AuditLogin)); n != 0 {
		t.Errorf("%d login rows after only failures, want none", n)
	}
	for _, p := range []store.Principal{disabled, hidden, opNoTOTP} {
		if n := f.credentialCount(t, p.ID); n != 0 {
			t.Errorf("principal %d holds %d credentials after failures, want none", p.ID, n)
		}
	}
}

// An operator's right password yields a challenge and nothing else: no
// credential, no principal. Not even setup that is half done yields one.
func TestLoginOperatorYieldsChallenge(t *testing.T) {
	f := newLoginFixture(t, Config{})
	f.enrollTOTP(t, f.root.ID)
	rec := f.login(t, "root", rootPassword, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	res := decodeBody[schema.LoginResponseV1](t, rec)
	if err := res.Validate(); err != nil || res.Next != schema.LoginNextTOTP {
		t.Fatalf("response %+v: %v", res, err)
	}
	if res.Principal != nil || res.Credential != nil || res.Token != "" {
		t.Errorf("a challenge response carries %+v", res)
	}
	if n := f.credentialCount(t, f.root.ID); n != 1 {
		t.Errorf("root holds %d credentials, want only the fixture's", n)
	}
	if n := len(auditRows(t, f.authFixture, store.AuditLogin)); n != 0 {
		t.Errorf("%d login rows for a challenge, want none", n)
	}
	// The challenge is no bearer credential.
	assertErrorBody(t, f.send(t, "GET", "/v1/whoami", res.Challenge, "", ""), http.StatusUnauthorized, "unauthenticated")
}

func TestLoginMalformed(t *testing.T) {
	f := newLoginFixture(t, Config{})
	for _, tt := range []struct {
		name, body string
		want       int
		code       string
	}{
		{"not JSON", "name=alice", 400, "invalid_request"},
		{"unknown field", `{"name":"alice","password":"x","role":"operator"}`, 400, "invalid_request"},
		{"no name", loginJSON(t, "", alicePassword), 400, "invalid_request"},
		{"blank name", loginJSON(t, "   ", alicePassword), 400, "invalid_request"},
		{"control character", loginJSON(t, "ali\u0007ce", alicePassword), 400, "invalid_request"},
		{"no password", loginJSON(t, "alice", ""), 400, "invalid_request"},
		{"password past the byte bound", loginJSON(t, "alice", strings.Repeat("p", schema.MaxPasswordBytes+1)), 400, "invalid_request"},
		{"too large", `{"name":"alice","password":"` + strings.Repeat("p", inviteBodyMaxBytes) + `"}`, 413, "request_too_large"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.send(t, "POST", "/v1/login", "", tt.body, "")
			assertErrorBody(t, rec, tt.want, tt.code)
			if strings.Contains(rec.Body.String(), alicePassword) || strings.Contains(rec.Body.String(), "pppp") {
				t.Errorf("the refusal echoes the request: %s", rec.Body)
			}
		})
	}
}

// The name's window (review F4): the sixth failure in ten minutes on a name
// is an immediate 429, the same for a name nobody has, and comes before any
// hashing, so it answers even while the hashing pool is full. Inside the
// window the right password is refused too. The window refills with time,
// and a credential issued resets it.
func TestLoginNameWindow(t *testing.T) {
	// The 429 of a name someone has and of one nobody has, byte for byte.
	var refused []byte
	for _, name := range []string{"alice", "nobody-at-all"} {
		t.Run(name, func(t *testing.T) {
			f := newLoginFixture(t, Config{})
			limits := looseLoginLimits()
			limits.failuresPerName = inviteLimit{"login_failures_per_name", defaultLoginFailuresPerName, defaultLoginFailureWindow}
			f.srv.setLoginLimits(limits)
			for i := range defaultLoginFailuresPerName {
				assertErrorBody(t, f.login(t, name, fmt.Sprintf("wrong password number %d", i), fmt.Sprintf("198.51.100.%d:1", i)),
					http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			}
			release := fillPool(t, f.srv.passwords.anonymous)
			start := time.Now()
			rec := f.login(t, name, alicePassword, "203.0.113.9:1")
			assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
			if time.Since(start) > time.Second {
				t.Errorf("the 429 took %v: it waited for the full pool", time.Since(start))
			}
			if got := rec.Header().Get("Retry-After"); got != "120" {
				t.Errorf("Retry-After = %q, want 120 (one failure back every two minutes)", got)
			}
			if refused == nil {
				refused = rec.Body.Bytes()
			} else if !bytes.Equal(rec.Body.Bytes(), refused) {
				t.Errorf("the 429 for a name nobody has %q differs from alice's %q", rec.Body.Bytes(), refused)
			}
			release()
			if name != "alice" {
				return
			}
			assertErrorBody(t, f.login(t, name, alicePassword, ""), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
			f.advance(2 * time.Minute)
			if rec := f.login(t, name, alicePassword, ""); rec.Code != http.StatusOK {
				t.Fatalf("after the refill = %d %s", rec.Code, rec.Body)
			}
			// The success reset the window: five more failures are allowed.
			for i := range defaultLoginFailuresPerName {
				assertErrorBody(t, f.login(t, name, "wrong again, and again", ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
				_ = i
			}
			assertErrorBody(t, f.login(t, name, "wrong again, and again", ""), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		})
	}
}

// Ten failures per address in ten minutes, across names; a sign-in that
// succeeds spends nothing of it.
func TestLoginAddressLimit(t *testing.T) {
	f := newLoginFixture(t, Config{})
	limits := looseLoginLimits()
	limits.failuresPerAddress = inviteLimit{"login_failures_per_address", defaultLoginFailuresPerAddress, defaultLoginFailureWindow}
	f.srv.setLoginLimits(limits)
	const remote = "198.51.100.40:1"
	for range 2 * defaultLoginFailuresPerAddress {
		if rec := f.login(t, "alice", alicePassword, remote); rec.Code != http.StatusOK {
			t.Fatalf("a success was refused: %d %s", rec.Code, rec.Body)
		}
	}
	for i := range defaultLoginFailuresPerAddress {
		assertErrorBody(t, f.login(t, fmt.Sprintf("name-%d", i), "some wrong password", remote), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	}
	assertErrorBody(t, f.login(t, "alice", alicePassword, remote), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if rec := f.login(t, "alice", alicePassword, "198.51.100.41:1"); rec.Code != http.StatusOK {
		t.Errorf("another address = %d, want 200", rec.Code)
	}
}

// Thirty credentials per name an hour (review F9), refused before the
// password is looked at.
func TestLoginSuccessCap(t *testing.T) {
	f := newLoginFixture(t, Config{})
	limits := looseLoginLimits()
	limits.successesPerName = inviteLimit{"login_successes_per_name", defaultLoginSuccessesPerName, defaultLoginSuccessWindow}
	f.srv.setLoginLimits(limits)
	for i := range defaultLoginSuccessesPerName {
		if rec := f.login(t, "alice", alicePassword, ""); rec.Code != http.StatusOK {
			t.Fatalf("success %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	assertErrorBody(t, f.login(t, "alice", alicePassword, ""), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	assertErrorBody(t, f.login(t, "alice", "a wrong password, too", ""), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	f.advance(2 * time.Minute)
	if rec := f.login(t, "alice", alicePassword, ""); rec.Code != http.StatusOK {
		t.Errorf("after one refilled = %d", rec.Code)
	}
}

// A challenge spends from the name's window like a failure (review F2):
// knowing an operator's password buys no fresh guesses at the code.
func TestLoginChallengeSpendsTheWindow(t *testing.T) {
	f := newLoginFixture(t, Config{})
	f.enrollTOTP(t, f.root.ID)
	limits := looseLoginLimits()
	limits.failuresPerName = inviteLimit{"login_failures_per_name", defaultLoginFailuresPerName, defaultLoginFailureWindow}
	f.srv.setLoginLimits(limits)
	for i := range defaultLoginFailuresPerName {
		if rec := f.login(t, "root", rootPassword, fmt.Sprintf("198.51.100.%d:1", i)); rec.Code != http.StatusOK {
			t.Fatalf("challenge %d = %d %s", i+1, rec.Code, rec.Body)
		}
	}
	assertErrorBody(t, f.login(t, "root", rootPassword, "203.0.113.1:1"), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
}

// A disable, a password replacement or a generation bump that lands between
// the verification and the issuing transaction wins: the sign-in is the
// common failure and no credential is made.
func TestLoginRacesLose(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		change func(f *loginFixture) error
	}{
		{"disabled", func(f *loginFixture) error {
			_, err := f.srv.store.DisablePrincipal(ctx, "system", f.alice.ID)
			return err
		}},
		{"password reset", func(f *loginFixture) error {
			_, err := f.srv.store.SetPassword(ctx, "system", f.alice.ID, "$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHRzYWx0c2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "reset", time.Now())
			return err
		}},
		{"signed out everywhere", func(f *loginFixture) error {
			_, err := f.srv.store.BumpAuthGeneration(ctx, "system", f.alice.ID, "revoke_all", time.Now())
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newLoginFixture(t, Config{})
			f.srv.beforeLoginIssue = func() {
				if err := tt.change(f); err != nil {
					t.Error(err)
				}
			}
			before := f.credentialCount(t, f.alice.ID)
			assertErrorBody(t, f.login(t, "alice", alicePassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
			if after := f.credentialCount(t, f.alice.ID); after != before {
				t.Errorf("the losing sign-in made %d credentials", after-before)
			}
			rows := loginFailedRows(t, f.authFixture)
			if len(rows) != 1 || !strings.Contains(rows[0], "reason="+loginReasonStale) {
				t.Errorf("login_failed rows %q, want one with reason stale", rows)
			}
		})
	}
}

// A record of older parameters is upgraded by the first sign-in with the
// right password, in the issuing transaction; the generation does not move,
// so no session ends.
func TestLoginRehashes(t *testing.T) {
	f := newLoginFixture(t, Config{})
	older, err := newPasswordHasher(argon2Params{memoryKiB: 64, iterations: 2, threads: 1, saltLen: 16, keyLen: 32}, testBounds)
	if err != nil {
		t.Fatal(err)
	}
	record, err := older.hash(alicePassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.SetPassword(context.Background(), "system", f.alice.ID, record, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if rec := f.login(t, "alice", alicePassword, ""); rec.Code != http.StatusOK {
		t.Fatalf("login = %d %s", rec.Code, rec.Body)
	}
	c, _, err := f.srv.store.LoginCandidate(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if c.Record == record || !strings.HasPrefix(c.Record, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Errorf("record after the sign-in = %q, want it rehashed with the current parameters", c.Record)
	}
	if rec := f.login(t, "alice", alicePassword, ""); rec.Code != http.StatusOK {
		t.Errorf("sign-in with the upgraded record = %d", rec.Code)
	}
}

// A stored record that cannot be verified signs nobody in, costs what any
// failure costs, and is logged as bad without the record.
func TestLoginBadRecord(t *testing.T) {
	logs := captureLogs(t)
	f := newLoginFixture(t, Config{})
	const bad = "$argon2id$v=19$m=4194304,t=3,p=1$c2FsdHNhbHRzYWx0c2FsdA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := f.srv.store.SetPassword(context.Background(), "system", f.alice.ID, bad, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	assertErrorBody(t, f.login(t, "alice", alicePassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	rows := loginFailedRows(t, f.authFixture)
	if len(rows) != 1 || !strings.Contains(rows[0], "reason="+loginReasonBadRecord) {
		t.Errorf("login_failed rows %q, want one with reason bad_record", rows)
	}
	if out := logs.buf.String(); !strings.Contains(out, "cannot be verified") || strings.Contains(out, "c2FsdH") || strings.Contains(out, "4194304") {
		t.Errorf("log = %s; want the bad record named, never shown", out)
	}
}

// With the anonymous pool full, a sign-in is refused at once with 429 and
// Retry-After, and nothing of the name's window is spent.
func TestLoginPoolFull(t *testing.T) {
	f := newLoginFixture(t, Config{})
	limits := looseLoginLimits()
	limits.failuresPerName = inviteLimit{"login_failures_per_name", 1, time.Hour}
	f.srv.setLoginLimits(limits)
	release := fillPool(t, f.srv.passwords.anonymous)
	rec := f.login(t, "alice", alicePassword, "")
	assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
	}
	release()
	if rec := f.login(t, "alice", alicePassword, ""); rec.Code != http.StatusOK {
		t.Errorf("after the pool drained = %d %s: the refusal spent the name's one failure", rec.Code, rec.Body)
	}
}

// No password, token, challenge or full client address reaches a log line,
// an audit row or an error body, whatever the outcome.
func TestLoginSecretsStayOut(t *testing.T) {
	logs := captureLogs(t)
	f := newLoginFixture(t, Config{TrustedProxies: mustPrefixes(t, "127.0.0.1/32")})
	f.enrollTOTP(t, f.root.ID)
	limits := looseLoginLimits()
	limits.failuresPerName = inviteLimit{"login_failures_per_name", 2, time.Hour}
	f.srv.setLoginLimits(limits)
	const client = "198.51.100.77"
	const proxy = "127.0.0.1:9000"
	const wrong = "a wrong password somebody tried"
	var bodies []string
	login := func(name, password string) *httptest.ResponseRecorder {
		rec := f.send(t, "POST", "/v1/login", "", loginJSON(t, name, password), proxy, client)
		return rec
	}
	ok := login("alice", alicePassword)
	token := decodeBody[schema.LoginResponseV1](t, ok).Token
	chal := decodeBody[schema.LoginResponseV1](t, login("root", rootPassword)).Challenge
	for _, rec := range []*httptest.ResponseRecorder{login("alice", wrong), login("alice", wrong), login("alice", wrong), login("zq-unheard-of", wrong)} {
		bodies = append(bodies, rec.Body.String())
	}
	if token == "" || chal == "" {
		t.Fatal("no token or challenge to look for")
	}
	secrets := map[string]string{
		"password": alicePassword, "operator password": rootPassword, "wrong password": wrong,
		"token": token, "token hash": sha256Hex(token), "challenge": chal, "challenge hash": sha256Hex(chal),
		"client address": client, "unknown name": "zq-unheard-of",
	}
	// The pending login keeps an HMAC of the address, not the address nor
	// its limit key: it matches the client it was issued to and no other.
	addressKey := f.queryString(t, "SELECT address_key FROM pending_logins")
	secrets["client limit key"] = limitKey(netip.MustParseAddr(client))
	if !f.srv.pendingAddressMatches(addressKey, netip.MustParseAddr(client)) {
		t.Error("the pending login's address_key does not match the client it was issued to")
	}
	if f.srv.pendingAddressMatches(addressKey, netip.MustParseAddr("198.51.100.78")) {
		t.Error("the pending login's address_key matches another client")
	}
	if other := newLoginFixture(t, Config{}); other.srv.pendingAddressMatches(addressKey, netip.MustParseAddr(client)) {
		t.Error("another process's key matches the address_key: the key is not per process")
	}
	places := map[string]string{"logs": logs.buf.String(), "audit": auditText(t, f.authFixture), "error bodies": strings.Join(bodies, "\n"),
		"pending logins' address keys": f.queryString(t, "SELECT group_concat(address_key, ' ') FROM pending_logins")}
	for what, secret := range secrets {
		for where, text := range places {
			if strings.Contains(text, secret) {
				t.Errorf("the %s appears in the %s", what, where)
			}
		}
	}
}

// The transport rule (design §5.4): HTTPS beyond loopback, read from a
// trusted proxy's X-Forwarded-Proto and from nobody else's.
func TestSecureTransport(t *testing.T) {
	trusted := mustPrefixes(t, "127.0.0.1/32,10.0.0.0/8")
	for _, tt := range []struct {
		name   string
		remote string
		tls    bool
		header map[string][]string
		want   bool
	}{
		{"loopback, plain", "127.0.0.1:1", false, nil, true},
		{"IPv6 loopback, plain", "[::1]:1", false, nil, true},
		{"LAN, plain", "192.168.1.5:1", false, nil, false},
		{"internet, plain", "203.0.113.5:1", false, nil, false},
		{"internet, TLS", "203.0.113.5:1", true, nil, true},
		{"trusted proxy, https", "10.0.0.2:1", false, map[string][]string{"X-Forwarded-For": {"203.0.113.5"}, "X-Forwarded-Proto": {"https"}}, true},
		{"trusted proxy, HTTPS", "10.0.0.2:1", false, map[string][]string{"X-Forwarded-Proto": {"HTTPS"}}, true},
		{"trusted proxy, http", "10.0.0.2:1", false, map[string][]string{"X-Forwarded-For": {"203.0.113.5"}, "X-Forwarded-Proto": {"http"}}, false},
		{"trusted proxy, no proto", "127.0.0.1:1", false, map[string][]string{"X-Forwarded-For": {"203.0.113.5"}}, false},
		{"trusted proxy, Forwarded only", "127.0.0.1:1", false, map[string][]string{"Forwarded": {"for=203.0.113.5;proto=https"}}, false},
		{"trusted proxy, client's https left of its http", "10.0.0.2:1", false, map[string][]string{"X-Forwarded-Proto": {"https, http"}}, false},
		{"trusted proxy, its https right of a client's http", "10.0.0.2:1", false, map[string][]string{"X-Forwarded-Proto": {"http", "https"}}, true},
		{"trusted proxy over TLS says http", "10.0.0.2:1", true, map[string][]string{"X-Forwarded-Proto": {"http"}}, false},
		{"untrusted peer claims https", "203.0.113.5:1", false, map[string][]string{"X-Forwarded-Proto": {"https"}}, false},
		{"untrusted peer over TLS with a forwarded header", "203.0.113.5:1", true, map[string][]string{"X-Forwarded-For": {"198.51.100.1"}}, false},
		{"loopback with an unconfigured proxy's header", "[::1]:1", false, map[string][]string{"X-Forwarded-For": {"203.0.113.5"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/login", nil)
			r.RemoteAddr = tt.remote
			if tt.tls {
				r.TLS = &tls.ConnectionState{}
			}
			for k, vs := range tt.header {
				for _, v := range vs {
					r.Header.Add(k, v)
				}
			}
			if got := secureTransport(r, trusted); got != tt.want {
				t.Errorf("secureTransport = %v, want %v", got, tt.want)
			}
		})
	}
}

// Both password routes refuse plain HTTP from beyond loopback before reading
// the body, and answer accounts_require_auth with authentication off. The v1
// redeem takes no password and makes no account (#713): over plain HTTP too,
// with a good code, it is 409 password_required (TestRedeemV1MakesNothing).
func TestPasswordRoutesRefusals(t *testing.T) {
	plain := func(srv *Server, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.RemoteAddr = "198.51.100.3:1"
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	f := newLoginFixture(t, Config{})
	code := f.createInvite(t, f.rootTok, openLink("general")).Code
	assertErrorBody(t, plain(f.srv, "/v1/login", loginJSON(t, "alice", alicePassword)), http.StatusForbidden, schema.ErrorCodeHTTPSRequired)
	assertErrorBody(t, plain(f.srv, "/v2/invites/redeem", redeemJSON(t, code, "wren")), http.StatusForbidden, schema.ErrorCodeHTTPSRequired)

	off := newAuthFixture(t, AuthOff)
	assertErrorBody(t, off.do(t, "POST", "/v1/login", "", loginJSON(t, "alice", alicePassword)), http.StatusForbidden, schema.ErrorCodeAccountsRequireAuth)
	assertErrorBody(t, off.do(t, "POST", "/v2/invites/redeem", "", redeemJSON(t, code, "wren")), http.StatusForbidden, schema.ErrorCodeAccountsRequireAuth)

	// v1 is no way around the transport rule: it makes nothing at all.
	assertErrorBody(t, plain(f.srv, "/v1/invites/redeem", inviteJSON(t, schema.RedeemInviteRequestV1{Code: code, Name: "wren"})),
		http.StatusConflict, schema.ErrorCodePasswordRequired)
}

// Creating an account with a password through an invite, signing out, and
// signing in again with the password on a new device (design §9 test 8).
func TestRedeemV2ThenPasswordSignIn(t *testing.T) {
	f := newLoginFixture(t, Config{})
	code := f.createInvite(t, f.rootTok, openLink("general")).Code
	rec := f.redeemWithPassword(t, code, "wren")
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	joined := decodeBody[schema.RedeemInviteResponseV1](t, rec)
	if rec := f.send(t, "DELETE", "/v1/credentials/current", joined.Token, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.send(t, "GET", "/v1/whoami", joined.Token, "", ""), http.StatusUnauthorized, "unauthenticated")
	rec = f.login(t, "wren", testJoinPassword, "198.51.100.200:1")
	if rec.Code != http.StatusOK {
		t.Fatalf("sign in again = %d %s", rec.Code, rec.Body)
	}
	res := decodeBody[schema.LoginResponseV1](t, rec)
	if res.Principal.ID != joined.Principal.ID {
		t.Errorf("signed in as %d, joined as %d", res.Principal.ID, joined.Principal.ID)
	}
	var actions []string
	for _, e := range f.audit(t) {
		if e.Subject == fmt.Sprintf("principal:%d", joined.Principal.ID) {
			actions = append(actions, e.Action)
		}
	}
	for _, want := range []string{store.AuditPrincipalCreated, store.AuditPasswordSet, "credential_revoked", store.AuditLogin} {
		if !strings.Contains(strings.Join(actions, ","), want) {
			t.Errorf("audit for wren %v lacks %s", actions, want)
		}
	}
}

// The password is held to the policy before anything is hashed or used: a
// refused one leaves the invite unused and makes nobody.
func TestRedeemV2PasswordRefusals(t *testing.T) {
	f := newLoginFixture(t, Config{})
	code := f.createInvite(t, f.rootTok, openLink("general")).Code
	for _, tt := range []struct {
		name, joiner, password string
		code                   string
	}{
		{"empty", "wren", "", "invalid_request"},
		{"too long", "wren", strings.Repeat("x", schema.MaxPasswordLength+1), "invalid_request"},
		{"too long after NFC", "wren", strings.Repeat("\U0001D15E", 65), "invalid_request"},
		{"common", "wren", "correct horse battery staple", schema.ErrorCodePasswordUnacceptable},
		{"one letter repeated", "wren", strings.Repeat("z", 20), schema.ErrorCodePasswordUnacceptable},
		{"contains the name", "wrenfield", "hello I am wrenfield the great", schema.ErrorCodePasswordUnacceptable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := inviteJSON(t, schema.RedeemInviteRequestV2{Code: code, Name: tt.joiner, Password: tt.password})
			rec := f.send(t, "POST", "/v2/invites/redeem", "", body, "")
			assertErrorBody(t, rec, http.StatusBadRequest, tt.code)
			if tt.password != "" && strings.Contains(rec.Body.String(), tt.password) {
				t.Errorf("the refusal echoes the password: %s", rec.Body)
			}
		})
	}
	if n := countJoinersCreated(t, f.authFixture); n != 0 {
		t.Errorf("principal_created rows = %d, want 0", n)
	}
	if got := decodeBody[schema.ListInvitesResponseV1](t, f.send(t, "GET", "/v1/invites", f.rootTok, "", "")); got.Invites[0].Uses != 0 {
		t.Errorf("uses = %d after refusals, want 0", got.Invites[0].Uses)
	}
}

// Everything a redemption can be refused for that costs nothing is refused
// before the hash, as from the bare address (issue #794, design §5.1–§5.2):
// a password that matches or contains the server's name, and a name that is
// taken or is the server's own. Neither runs Argon2, makes anyone or spends
// a use of the invite; the name refusals spend a name probe from the
// allowance the account route from the bare address shares, the password
// ones spend none.
func TestRedeemV2RefusesBeforeHashing(t *testing.T) {
	const (
		serverName = "Harbour Club Members"
		remote     = "198.51.100.94:1"
	)
	tests := []struct {
		name, joiner, password string
		status                 int
		code, message          string
		spendsProbe            bool
	}{
		{"password is the server's name", "wren", serverName,
			http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable, "", false},
		{"password contains the server's name", "wren", "we are the harbour club members, all of us",
			http.StatusBadRequest, schema.ErrorCodePasswordUnacceptable, "", false},
		{"name taken", "alice", testJoinPassword,
			http.StatusConflict, schema.ErrorCodePrincipalExists, "that name is taken; choose another", true},
		{"name taken in another case", "ALICE", testJoinPassword,
			http.StatusConflict, schema.ErrorCodePrincipalExists, "that name is taken; choose another", true},
		{"name is the server's", "HARBOUR CLUB MEMBERS", testJoinPassword,
			http.StatusConflict, schema.ErrorCodePrincipalExists, serverNameReservedMessage, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLoginFixture(t, Config{ServerName: serverName})
			f.mustPutJoin(t, openBoth("red"))
			limits := looseLoginLimits()
			limits.nameProbesPerAddress = inviteLimit{"name_probes_per_address", 1, time.Hour}
			f.srv.setLoginLimits(limits)
			inv := f.createInvite(t, f.rootTok, openLink("red"))
			hashed := f.srv.passwords.hashes.Load()
			body := inviteJSON(t, schema.RedeemInviteRequestV2{Code: inv.Code, Name: tt.joiner, Password: tt.password})
			rec := f.send(t, "POST", "/v2/invites/redeem", "", body, remote)
			assertErrorBody(t, rec, tt.status, tt.code)
			if tt.message != "" {
				if msg := decodeBody[schema.Error](t, rec).Message; msg != tt.message {
					t.Errorf("message = %q, want %q", msg, tt.message)
				}
			}
			if strings.Contains(rec.Body.String(), tt.password) {
				t.Errorf("the refusal echoes the password: %s", rec.Body)
			}
			if n := f.srv.passwords.hashes.Load(); n != hashed {
				t.Errorf("the refusal hashed %d passwords, want none", n-hashed)
			}
			if n := countJoinersCreated(t, f.authFixture); n != 0 {
				t.Errorf("principal_created rows = %d, want 0", n)
			}
			if n := f.queryString(t, fmt.Sprintf("SELECT uses FROM invites WHERE id = %d", inv.Invite.ID)); n != "0" {
				t.Errorf("the invite's uses = %s, want none spent", n)
			}
			_, free, _ := f.srv.logins.peekNameProbe(clientAddrOf(t, remote), f.srv.now())
			if free == tt.spendsProbe {
				t.Errorf("a name probe left = %v, want %v", free, !tt.spendsProbe)
			}
			if tt.spendsProbe {
				// The allowance is the one the bare address's account route
				// spends: it is refused there too, still before any hash.
				assertErrorBody(t, f.joinAccount(t, "a-free-name", remote), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
				if n := f.srv.passwords.hashes.Load(); n != hashed {
					t.Errorf("the account route hashed %d passwords, want none", n-hashed)
				}
			}
		})
	}
}

// An invalid code costs no hashing: it is refused while the pool is full.
// So is a valid one, which the pool would have hashed, with 429.
func TestRedeemV2HashesOnlyAValidCode(t *testing.T) {
	f := newLoginFixture(t, Config{})
	code := f.createInvite(t, f.rootTok, openLink("general")).Code
	fillPool(t, f.srv.passwords.anonymous)
	assertErrorBody(t, f.redeemWithPassword(t, fakeInviteCode, "wren"), http.StatusNotFound, schema.ErrorCodeInviteInvalid)
	rec := f.redeemWithPassword(t, code, "wren")
	assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if n := countJoinersCreated(t, f.authFixture); n != 0 {
		t.Errorf("principal_created rows = %d, want 0", n)
	}
}

// A name that is taken answers principal_exists five times an hour from one
// address (design §8); the sixth attempt is a 429 before any hashing.
func TestRedeemV2NameProbes(t *testing.T) {
	f := newLoginFixture(t, Config{})
	limits := looseLoginLimits()
	limits.nameProbesPerAddress = inviteLimit{"name_probes_per_address", defaultNameProbesPerAddress, defaultNameProbeWindow}
	f.srv.setLoginLimits(limits)
	code := f.createInvite(t, f.rootTok, openLink("general")).Code
	for range defaultNameProbesPerAddress {
		assertErrorBody(t, f.redeemWithPassword(t, code, "alice"), http.StatusConflict, schema.ErrorCodePrincipalExists)
	}
	assertErrorBody(t, f.redeemWithPassword(t, code, "alice"), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	assertErrorBody(t, f.redeemWithPassword(t, code, "a-free-name"), http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	f.advance(defaultNameProbeWindow / defaultNameProbesPerAddress)
	if rec := f.redeemWithPassword(t, code, "a-free-name"); rec.Code != http.StatusCreated {
		t.Errorf("after a probe refilled = %d %s", rec.Code, rec.Body)
	}
}

// A guest's handle signs in with nothing, even with the right password
// planted for it underneath every route: the common failure, audited with
// reason guest, and no credential beyond the visit's one.
func TestLoginRefusesAGuestHandle(t *testing.T) {
	f := newLoginFixture(t, Config{})
	g := newGuest(t, f.srv, "visitor", time.Hour, 24*time.Hour, f.red.ID)
	record, err := f.srv.passwords.hasher.hash(alicePassword)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, "INSERT INTO passwords (principal_id, hash, created_at, updated_at) VALUES (?, ?, 1, 1)", g.Principal.ID, record)
	assertErrorBody(t, f.login(t, g.Principal.Name, alicePassword, ""), http.StatusUnauthorized, schema.ErrorCodeAuthFailed)
	if n := f.credentialCount(t, g.Principal.ID); n != 1 {
		t.Errorf("the guest holds %d credentials, want only its visit's", n)
	}
	rows := loginFailedRows(t, f.authFixture)
	if len(rows) != 1 || !strings.Contains(rows[0], "reason="+loginReasonGuest) {
		t.Errorf("login_failed rows %q, want one with reason guest", rows)
	}
}

// An invite that admits guests only is answered 403 joining_closed by the
// account route before any hashing (the pool is full and it still answers),
// and uses nothing; account and both make the account.
func TestRedeemV2JoinAs(t *testing.T) {
	for _, tt := range []struct {
		joinAs schema.InviteJoinMode
		want   int
	}{
		{schema.InviteJoinGuest, http.StatusForbidden},
		{schema.InviteJoinAccount, http.StatusCreated},
		{schema.InviteJoinBoth, http.StatusCreated},
	} {
		t.Run(string(tt.joinAs), func(t *testing.T) {
			f := newLoginFixture(t, Config{})
			inv := f.createInvite(t, f.rootTok, openLink("general"))
			f.exec(t, "UPDATE invites SET join_as = ? WHERE id = ?", string(tt.joinAs), inv.Invite.ID)
			if tt.want == http.StatusForbidden {
				release := fillPool(t, f.srv.passwords.anonymous)
				assertErrorBody(t, f.redeemWithPassword(t, inv.Code, "wren"), http.StatusForbidden, schema.ErrorCodeJoiningClosed)
				release()
				if n := countJoinersCreated(t, f.authFixture); n != 0 {
					t.Errorf("principal_created rows = %d, want 0", n)
				}
				return
			}
			if rec := f.redeemWithPassword(t, inv.Code, "wren"); rec.Code != tt.want {
				t.Errorf("redeem = %d %s, want %d", rec.Code, rec.Body, tt.want)
			}
		})
	}
}
