package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Password sign-in (issue #707, docs/design/guests-and-accounts.md §5.2–5.3,
// §7.2). POST /v1/login is open to anyone. A member's right password earns
// a member credential; an operator's earns only a pending-login challenge,
// which the second factor completes, and an operator whose setup is
// incomplete earns nothing. Every other outcome is one answer, 401
// auth_failed, after comparable work: an unknown name, a wrong password, a
// disabled, hidden or agent principal and one with no password all cost one
// Argon2id verification (a dummy one where there is no record).
//
// Limits, all before any hashing, all in memory like the invite limits
// (invite_limits.go):
//
//   - per client address, ten failures in ten minutes; a sign-in that issues
//     a credential spends nothing;
//   - per typed name, whether or not anyone has it (review F4): five
//     failures in ten minutes, then an immediate 429 with Retry-After, never
//     a delay. Only an issued credential resets it, so knowing an operator's
//     password buys no fresh guesses at the code (review F2): a challenge
//     spends from the window like a failure;
//   - per typed name, thirty issued credentials an hour (review F9);
//   - the anonymous hashing pool (password.go).
//
// None of these locks an account: each refills with time. The audit trail
// gets login_failed, thinned like the invite refusals, with the name's key
// when it names a principal (the constant unknown otherwise) and a reason
// code; never the password, the address or the typed name of nobody.

// Audit actions written by the sign-in route. The store writes login.
const auditLoginFailed = "login_failed"

// Reasons of a login_failed row. They are for the operator reading the
// trail; the caller is told none of them.
const (
	loginReasonUnknown         = "unknown"
	loginReasonNotAPerson      = "not_a_person"
	loginReasonDisabled        = "disabled"
	loginReasonHidden          = "hidden"
	loginReasonGuest           = "guest"
	loginReasonNoPassword      = "no_password"
	loginReasonWrongPassword   = "wrong_password"
	loginReasonBadRecord       = "bad_record"
	loginReasonSetupIncomplete = "setup_incomplete"
	loginReasonStale           = "stale"
	loginReasonRateLimited     = "rate_limited"
)

// The recommended defaults of design §8.
const (
	defaultLoginFailuresPerAddress = 10
	defaultLoginFailuresPerName    = 5
	defaultLoginFailureWindow      = 10 * time.Minute
	defaultLoginSuccessesPerName   = 30
	defaultLoginSuccessWindow      = time.Hour
	// defaultNameProbesPerAddress is how many answers of principal_exists
	// one address may have an hour from the routes that make an account
	// with a password (design §8): enough for a person to try a few names,
	// not enough to list who is here.
	defaultNameProbesPerAddress = 5
	defaultNameProbeWindow      = time.Hour
)

// loginLimits are the sign-in limits. Tests replace them with
// Server.setLoginLimits.
type loginLimits struct {
	failuresPerAddress   inviteLimit
	failuresPerName      inviteLimit
	successesPerName     inviteLimit
	nameProbesPerAddress inviteLimit
}

func defaultLoginLimits() loginLimits {
	return loginLimits{
		failuresPerAddress:   inviteLimit{"login_failures_per_address", defaultLoginFailuresPerAddress, defaultLoginFailureWindow},
		failuresPerName:      inviteLimit{"login_failures_per_name", defaultLoginFailuresPerName, defaultLoginFailureWindow},
		successesPerName:     inviteLimit{"login_successes_per_name", defaultLoginSuccessesPerName, defaultLoginSuccessWindow},
		nameProbesPerAddress: inviteLimit{"name_probes_per_address", defaultNameProbesPerAddress, defaultNameProbeWindow},
	}
}

// loginLimitsFor is the sign-in limits cfg asks for. The test-only relaxed
// invite limits (Config.TestRelaxInviteLimits) relax the per-address ones
// here too, for the same reason: a browser suite runs from one address.
// The per-name limits stay as they are.
func loginLimitsFor(cfg Config) loginLimits {
	l := defaultLoginLimits()
	if cfg.TestRelaxInviteLimits {
		l.failuresPerAddress.burst = testRelaxedPerAddressBurst
		l.nameProbesPerAddress.burst = testRelaxedPerAddressBurst
	}
	return l
}

// loginLimiter holds the sign-in buckets. One lock: every operation is a
// few arithmetic steps.
type loginLimiter struct {
	mu                                      sync.Mutex
	limits                                  loginLimits
	addrFailures, nameFailures, nameSuccess bucketSet
	nameProbes                              bucketSet
	// audits thins login_failed rows, by the rule of denialCoalescer, under
	// principal 0. Its own, so a flood of sign-ins cannot crowd the invite
	// refusals out of theirs.
	audits denialCoalescer
}

func newLoginLimiter(l loginLimits) *loginLimiter { return &loginLimiter{limits: l} }

// setLoginLimits replaces the sign-in limits and forgets every bucket. For
// tests, before the server serves.
func (s *Server) setLoginLimits(l loginLimits) { s.logins = newLoginLimiter(l) }

// nameOverflowKey is the bucket a name shares once a name table is full:
// one of maxInviteOverflowKeys, picked by a hash of the key, so a flood of
// made-up names shares a few thousand buckets and does not drain the one a
// real name would otherwise fall into.
func nameOverflowKey(key string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return inviteOverflowKey + "|name|" + strconv.FormatUint(uint64(h.Sum32()%maxInviteOverflowKeys), 10)
}

// takeAddress spends one failure of addr's budget, given back by
// refundAddress if the attempt issues a credential.
func (l *loginLimiter) takeAddress(addr netip.Addr, now time.Time) (inviteLimit, bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.limits.failuresPerAddress
	ok, retry := l.addrFailures.bucket(limit, limitKey(addr), overflowKey(addr), now).take(limit, now)
	return limit, ok, retry
}

func (l *loginLimiter) refundAddress(addr netip.Addr, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.limits.failuresPerAddress
	l.addrFailures.bucket(limit, limitKey(addr), overflowKey(addr), now).refund(limit, now)
}

// takeName reserves one failure and one success of the name key's windows;
// the outcome then settles which it was (nameFailed, nameSucceeded,
// nameCancelled). Both are taken before any hashing, so neither window can
// be overrun by requests in flight, and the 429 of either comes before the
// password is looked at, whether it is right or not.
func (l *loginLimiter) takeName(key string, now time.Time) (inviteLimit, bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fl, sl := l.limits.failuresPerName, l.limits.successesPerName
	overflow := nameOverflowKey(key)
	fb := l.nameFailures.bucket(fl, key, overflow, now)
	if ok, retry := fb.take(fl, now); !ok {
		return fl, false, retry
	}
	if ok, retry := l.nameSuccess.bucket(sl, key, overflow, now).take(sl, now); !ok {
		fb.refund(fl, now)
		return sl, false, retry
	}
	return inviteLimit{}, true, 0
}

// nameFailed settles a reservation as a failure: the failure stands and the
// success is given back. A challenge is settled the same way (review F2).
func (l *loginLimiter) nameFailed(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	sl := l.limits.successesPerName
	l.nameSuccess.bucket(sl, key, nameOverflowKey(key), now).refund(sl, now)
}

// nameSucceeded settles a reservation as an issued credential: the success
// stands and the name's failure window starts again. Only the name's own
// bucket is reset; one it shares with other names past the table's bound
// gets back the one failure this attempt reserved, so a success never
// clears anybody else's throttling.
func (l *loginLimiter) nameSucceeded(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fl := l.limits.failuresPerName
	if _, own := l.nameFailures.buckets[key]; own {
		delete(l.nameFailures.buckets, key)
		return
	}
	l.nameFailures.bucket(fl, key, nameOverflowKey(key), now).refund(fl, now)
}

// nameCancelled gives back the whole reservation: the attempt looked at no
// password (the pool was full, the store failed).
func (l *loginLimiter) nameCancelled(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fl, sl := l.limits.failuresPerName, l.limits.successesPerName
	overflow := nameOverflowKey(key)
	l.nameFailures.bucket(fl, key, overflow, now).refund(fl, now)
	l.nameSuccess.bucket(sl, key, overflow, now).refund(sl, now)
}

// peekNameProbe reports whether addr has a name probe left, without
// spending it: the routes that make an account check it before their work
// and spend it (spendNameProbe) only when the name turns out taken.
func (l *loginLimiter) peekNameProbe(addr netip.Addr, now time.Time) (inviteLimit, bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.limits.nameProbesPerAddress
	b := l.nameProbes.bucket(limit, limitKey(addr), overflowKey(addr), now)
	ok, retry := b.take(limit, now)
	if ok {
		b.refund(limit, now)
	}
	return limit, ok, retry
}

func (l *loginLimiter) spendNameProbe(addr netip.Addr, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	limit := l.limits.nameProbesPerAddress
	l.nameProbes.bucket(limit, limitKey(addr), overflowKey(addr), now).take(limit, now)
}

// newAddressMACKey returns the key pendingAddressKey uses: random, made once
// per process, never stored or logged. A restart makes every outstanding
// challenge's address binding fail, which costs at most five minutes of
// challenges.
func newAddressMACKey() []byte {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		// crypto/rand does not fail on a supported platform; if it did,
		// no key is safer than a predictable one.
		panic(fmt.Sprintf("login: address key: %v", err))
	}
	return k
}

// pendingAddressKey is what a pending login keeps of the client address it
// was issued to (review F14): an HMAC-SHA256 of the address's limit key
// (limitKey) under the process's random key, in hex. The address itself is
// never at rest, and a plain hash would not do: the IPv4 space is small
// enough to try every address.
func (s *Server) pendingAddressKey(addr netip.Addr) string {
	m := hmac.New(sha256.New, s.addressMAC)
	_, _ = m.Write([]byte(limitKey(addr)))
	return hex.EncodeToString(m.Sum(nil))
}

// pendingAddressMatches reports, in constant time, whether stored, a pending
// login's address_key, was made for addr's limit key by this process. It is
// the check the TOTP completion makes before it looks at the code.
func (s *Server) pendingAddressMatches(stored string, addr netip.Addr) bool {
	return hmac.Equal([]byte(stored), []byte(s.pendingAddressKey(addr)))
}

// passwordRoute checks what every anonymous route that takes a password
// checks first (design §7.2, §7.4): authentication is on, since with it off
// there is no account to sign in to (403 accounts_require_auth), and the
// request came over HTTPS or from loopback (403 https_required,
// secureTransport). It writes the refusal and reports false.
func (s *Server) passwordRoute(w http.ResponseWriter, r *http.Request) bool {
	if !s.cfg.authRequired() {
		writeError(w, http.StatusForbidden, schema.ErrorCodeAccountsRequireAuth,
			"this server runs with authentication off, so it has no accounts to sign in to")
		return false
	}
	if !secureTransport(r, s.cfg.TrustedProxies) {
		writeError(w, http.StatusForbidden, schema.ErrorCodeHTTPSRequired,
			"passwords are accepted only over HTTPS; behind a reverse proxy, name it with --trusted-proxies")
		return false
	}
	return true
}

func writeAuthFailed(w http.ResponseWriter) {
	writeError(w, http.StatusUnauthorized, schema.ErrorCodeAuthFailed, "the name or password is not right")
}

// writePasswordBusy answers a request the hashing pool had no room for.
func writePasswordBusy(w http.ResponseWriter) {
	writeRateLimited(w, &rateLimitedError{retryAfter: hashPoolRetryAfter})
}

// handleLogin serves POST /v1/login. The order is part of the contract: the
// transport check, the address limit before the body is read, a malformed
// body a 400 that says nothing about any account, the name's windows, then
// the one verification, then the store's re-check inside the issuing
// transaction.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.passwordRoute(w, r) {
		return
	}
	addr := clientAddr(r, s.cfg.TrustedProxies)
	if limit, ok, retry := s.logins.takeAddress(addr, s.now()); !ok {
		s.refuseLoginRateLimited(w, r, limit, retry, addr)
		return
	}
	var req schema.LoginRequestV1
	if !decodeInviteBody(w, r, &req) {
		return
	}
	if req.Validate() != nil {
		// A fixed message: the schema's names the rule, but nothing here
		// needs to say which half was malformed.
		writeError(w, http.StatusBadRequest, "invalid_request", "the name or password is malformed")
		return
	}
	key := names.KeysOf(req.Name).Folded
	if limit, ok, retry := s.logins.takeName(key, s.now()); !ok {
		s.refuseLoginRateLimited(w, r, limit, retry, addr)
		return
	}

	cand, found, err := s.store.LoginCandidate(ctx, req.Name)
	if err != nil {
		s.logins.nameCancelled(key, s.now())
		s.logins.refundAddress(addr, s.now())
		slog.ErrorContext(ctx, "login: look up the name failed", "error", err)
		writeInternalError(w)
		return
	}
	password, fits := normalizePresented(req.Password)
	record, reason := "", ""
	p := cand.Principal
	switch {
	case !found:
		reason = loginReasonUnknown
	case p.Kind != store.PrincipalHuman:
		reason = loginReasonNotAPerson
	case cand.IsGuestTarget():
		reason = loginReasonGuest
	case p.HiddenAt != nil:
		reason = loginReasonHidden
	case p.DisabledAt != nil:
		reason = loginReasonDisabled
	case cand.Record == "":
		reason = loginReasonNoPassword
	case !fits:
		reason = loginReasonWrongPassword
	default:
		record = cand.Record
	}
	// One verification whatever the reason: a dummy one when there is no
	// record to check, so every failure costs what a wrong password does.
	res, err := s.passwords.verifyIn(ctx, s.passwords.anonymous, record, password)
	if err != nil {
		s.logins.nameCancelled(key, s.now())
		s.logins.refundAddress(addr, s.now())
		if errors.Is(err, errHashPoolBusy) {
			s.refuseLoginRateLimited(w, r, inviteLimit{name: "hash_pool_" + s.passwords.anonymous.name}, hashPoolRetryAfter, addr)
			return
		}
		// The caller has gone while it waited; nothing was looked at.
		writePasswordBusy(w)
		return
	}
	switch {
	case reason != "":
	case res.badRecord:
		reason = loginReasonBadRecord
		slog.ErrorContext(ctx, "login: a stored password record cannot be verified; the person cannot sign in until it is set again", "principal", p.ID)
	case !res.ok:
		reason = loginReasonWrongPassword
	case p.Role == store.RoleOperator && !cand.TOTPEnrolled:
		// Never signed in and never a challenge: an operator's browser
		// sign-in needs both factors, set up through a host-made token.
		reason = loginReasonSetupIncomplete
	}
	if reason != "" {
		s.loginFailed(w, r, key, found, reason)
		return
	}

	if s.beforeLoginIssue != nil {
		s.beforeLoginIssue()
	}
	issue := store.LoginIssue{PrincipalID: p.ID, AuthGeneration: cand.AuthGeneration, Record: cand.Record, Rehash: res.rehash}
	if p.Role == store.RoleOperator {
		pending, err := s.store.CreatePendingLogin(ctx, issue, s.pendingAddressKey(addr), s.now())
		switch {
		case errors.Is(err, store.ErrLoginStale):
			s.loginFailed(w, r, key, true, loginReasonStale)
		case err != nil:
			s.logins.nameCancelled(key, s.now())
			slog.ErrorContext(ctx, "login: record the challenge failed", "principal", p.ID, "error", err)
			writeInternalError(w)
		default:
			// The challenge spends from the name's window and the address's
			// budget as a failure would: only a credential resets them.
			s.logins.nameFailed(key, s.now())
			expires := schema.NewTimestamp(pending.ExpiresAt)
			writeSecretJSON(w, http.StatusOK, schema.LoginResponseV1{
				Next: schema.LoginNextTOTP, Challenge: pending.Challenge, ChallengeExpiresAt: &expires,
			})
		}
		return
	}
	issued, err := s.store.IssueLoginCredential(ctx, issue, s.now())
	switch {
	case errors.Is(err, store.ErrLoginStale):
		s.loginFailed(w, r, key, true, loginReasonStale)
	case err != nil:
		s.logins.nameCancelled(key, s.now())
		s.logins.refundAddress(addr, s.now())
		slog.ErrorContext(ctx, "login: issue the credential failed", "principal", p.ID, "error", err)
		writeInternalError(w)
	default:
		s.logins.nameSucceeded(key, s.now())
		s.logins.refundAddress(addr, s.now())
		slog.InfoContext(ctx, "login: signed in", "principal", issued.Principal.ID, "credential", issued.Credential.ID)
		principal := principalFromStore(issued.Principal)
		writeSecretJSON(w, http.StatusOK, schema.LoginResponseV1{
			Next: schema.LoginNextSignedIn, Principal: &principal, Credential: &issued.Credential, Token: issued.Token,
		})
	}
}

// loginFailed settles a failed sign-in: the failure stands in the name's
// window and the address's budget, login_failed is audited (thinned), and the
// caller gets the one answer. named says the key names a principal, so the
// row may carry it.
func (s *Server) loginFailed(w http.ResponseWriter, r *http.Request, key string, named bool, reason string) {
	s.logins.nameFailed(key, s.now())
	if !named {
		key = loginReasonUnknown
	}
	s.auditLoginFailed(r, fmt.Sprintf("name_key=%q reason=%s", key, reason), reason+"|"+key)
	writeAuthFailed(w)
}

// auditLoginFailed writes login_failed, thinned by kind, and reports whether
// it wrote a row.
func (s *Server) auditLoginFailed(r *http.Request, detail, kind string) bool {
	write, count := s.logins.audits.admit(0, kind, s.now())
	if !write {
		return false
	}
	actx := context.WithoutCancel(r.Context())
	if _, err := s.store.AppendAuditEvent(actx, inviteAnonymousAuditActor, auditLoginFailed, inviteAuditSubject(r),
		fmt.Sprintf("%s count=%d", detail, count)); err != nil {
		slog.ErrorContext(actx, "login: audit login_failed failed", "error", err)
	}
	return true
}

// refuseLoginRateLimited answers a sign-in refused by limit with 429 and
// Retry-After, and audits it as login_failed with reason rate_limited and the
// limit's name; the name is not looked up for it, so the row says unknown.
// The log line is thinned with the row and carries the truncated network
// only.
func (s *Server) refuseLoginRateLimited(w http.ResponseWriter, r *http.Request, limit inviteLimit, retry time.Duration, addr netip.Addr) {
	detail := fmt.Sprintf("name_key=%q reason=%s limit=%s", loginReasonUnknown, loginReasonRateLimited, limit.name)
	if s.auditLoginFailed(r, detail, loginReasonRateLimited+"|"+limit.name) {
		slog.WarnContext(r.Context(), "login: rate limit reached", "limit", limit.name, "network", logAddr(addr))
	}
	writeRateLimited(w, &rateLimitedError{retryAfter: retry})
}
