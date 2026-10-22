package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/njdaniel/conch/pkg/schema"
)

// Passwords (ADR-009 §5, docs/design/guests-and-accounts.md §5.1–5.2,
// issue #707). A password is normalized to NFC, held to the schema's bounds
// and the policy below, and stored only as an Argon2id record:
//
//	$argon2id$v=19$m=<KiB>,t=<iterations>,p=<lanes>$<salt>$<hash>
//
// with the salt and hash in unpadded standard base64. Every hash and every
// verification runs inside one of two bounded pools (hashPool), so password
// work is the one thing an anonymous flood cannot make the server do without
// limit, and the memory it can take is bounded: two pools of two running
// hashes, each the record's memory. Nothing here logs or returns a password,
// a salt or a hash: errors name the rule, never the input.

// argon2Params are the parameters of one Argon2id hash.
type argon2Params struct {
	memoryKiB  uint32
	iterations uint32
	threads    uint8
	saltLen    int
	keyLen     int
}

// argon2Bounds are what a stored record's parameters may be. A record
// outside them is not verified at all: a tampered or corrupt row must not
// make one sign-in allocate gigabytes, and a weak one must not pass.
type argon2Bounds struct {
	minMemoryKiB, maxMemoryKiB   uint32
	minIterations, maxIterations uint32
	maxThreads                   uint8
	minSaltLen, maxSaltLen       int
	minKeyLen, maxKeyLen         int
}

// The design's baseline (§5.1): 64 MiB, three passes, one lane, a 16-byte
// salt and a 32-byte result. The floor is OWASP's documented minimum for
// Argon2id (19 MiB, two passes, one lane); a tuning change is a reviewed
// edit of these lines and never goes below it.
var (
	productionArgon2 = argon2Params{memoryKiB: 64 * 1024, iterations: 3, threads: 1, saltLen: 16, keyLen: 32}
	productionBounds = argon2Bounds{
		minMemoryKiB: 19 * 1024, maxMemoryKiB: 256 * 1024,
		minIterations: 2, maxIterations: 10,
		maxThreads: 4,
		minSaltLen: 16, maxSaltLen: 64,
		minKeyLen: 32, maxKeyLen: 64,
	}
)

// passwordConfigForNew is the hashing configuration New gives a server. It
// is a variable only so this package's tests can make hashing cheap; the
// tests of the production parameters build their own hasher, and nothing
// else reassigns it.
var passwordConfigForNew = struct {
	params argon2Params
	bounds argon2Bounds
}{productionArgon2, productionBounds}

const argon2Prefix = "$argon2id$"

var (
	// errPasswordRecord is a stored record that cannot be verified: not an
	// Argon2id record, or one whose parameters are out of bounds. The caller
	// treats it as a failed sign-in and logs that the record is bad, never
	// the record.
	errPasswordRecord = errors.New("server: password record is not a usable argon2id record")
	// errPasswordUnacceptable is a password that has the schema's shape but
	// not the server's policy (ErrorCodePasswordUnacceptable).
	errPasswordUnacceptable = errors.New("server: password is not acceptable")
)

// passwordHasher hashes and verifies passwords with one set of parameters.
type passwordHasher struct {
	params argon2Params
	bounds argon2Bounds
	// dummySalt and dummyKey are what a dummy verification compares
	// against: random, made once, matching no password.
	dummySalt, dummyKey []byte
}

// newPasswordHasher returns a hasher for params, refused when params are
// outside bounds: the server never hashes below the floor it verifies.
func newPasswordHasher(params argon2Params, bounds argon2Bounds) (*passwordHasher, error) {
	if !bounds.allow(params) {
		return nil, errors.New("server: argon2id parameters are outside the allowed bounds")
	}
	h := &passwordHasher{params: params, bounds: bounds,
		dummySalt: make([]byte, params.saltLen), dummyKey: make([]byte, params.keyLen)}
	if _, err := rand.Read(h.dummySalt); err != nil {
		return nil, fmt.Errorf("server: dummy salt: %w", err)
	}
	if _, err := rand.Read(h.dummyKey); err != nil {
		return nil, fmt.Errorf("server: dummy key: %w", err)
	}
	return h, nil
}

func (b argon2Bounds) allow(p argon2Params) bool {
	return p.memoryKiB >= b.minMemoryKiB && p.memoryKiB <= b.maxMemoryKiB &&
		p.iterations >= b.minIterations && p.iterations <= b.maxIterations &&
		p.threads >= 1 && p.threads <= b.maxThreads &&
		p.saltLen >= b.minSaltLen && p.saltLen <= b.maxSaltLen &&
		p.keyLen >= b.minKeyLen && p.keyLen <= b.maxKeyLen &&
		// Argon2 needs at least 8 KiB per lane.
		p.memoryKiB >= 8*uint32(p.threads)
}

// keyLen32 is the key length as Argon2 takes it. Every params value that
// reaches Argon2 has passed argon2Bounds.allow, which holds the length to at
// most maxKeyLen, so the conversion cannot overflow.
func (p argon2Params) keyLen32() uint32 {
	return uint32(p.keyLen) // #nosec G115 -- bounded by argon2Bounds (at most 64) before any use
}

// hash returns the encoded record of password, which the caller has already
// normalized (normalizePassword). It does the work at once: callers run it
// inside a hashPool slot.
func (h *passwordHasher) hash(password string) (string, error) {
	salt := make([]byte, h.params.saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("server: password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.params.iterations, h.params.memoryKiB, h.params.threads, h.params.keyLen32())
	return encodeArgon2(h.params, salt, key), nil
}

func encodeArgon2(p argon2Params, salt, key []byte) string {
	return fmt.Sprintf("%sv=%d$m=%d,t=%d,p=%d$%s$%s", argon2Prefix, argon2.Version, p.memoryKiB, p.iterations, p.threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// decodeArgon2 parses an encoded record and checks it against bounds before
// anything is allocated for it. The parse is strict: one spelling of each
// field, in order, so two records that verify alike are the same text.
func decodeArgon2(record string, bounds argon2Bounds) (argon2Params, []byte, []byte, error) {
	rest, ok := strings.CutPrefix(record, argon2Prefix)
	if !ok {
		return argon2Params{}, nil, nil, errPasswordRecord
	}
	parts := strings.Split(rest, "$")
	if len(parts) != 4 || parts[0] != "v="+strconv.Itoa(argon2.Version) {
		return argon2Params{}, nil, nil, errPasswordRecord
	}
	var p argon2Params
	fields := strings.Split(parts[1], ",")
	if len(fields) != 3 {
		return argon2Params{}, nil, nil, errPasswordRecord
	}
	for i, want := range []string{"m=", "t=", "p="} {
		v, ok := strings.CutPrefix(fields[i], want)
		// No sign, no leading zero, no spaces: strconv alone would take "+3".
		if !ok || v == "" || (len(v) > 1 && v[0] == '0') || strings.TrimLeft(v, "0123456789") != "" {
			return argon2Params{}, nil, nil, errPasswordRecord
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return argon2Params{}, nil, nil, errPasswordRecord
		}
		switch i {
		case 0:
			p.memoryKiB = uint32(n)
		case 1:
			p.iterations = uint32(n)
		default:
			if n > 255 {
				return argon2Params{}, nil, nil, errPasswordRecord
			}
			p.threads = uint8(n)
		}
	}
	// Lengths are checked on the encoded text first, so a huge field is
	// refused before it is decoded.
	if len(parts[2]) > base64.RawStdEncoding.EncodedLen(bounds.maxSaltLen) ||
		len(parts[3]) > base64.RawStdEncoding.EncodedLen(bounds.maxKeyLen) {
		return argon2Params{}, nil, nil, errPasswordRecord
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[2])
	if err != nil {
		return argon2Params{}, nil, nil, errPasswordRecord
	}
	key, err := base64.RawStdEncoding.Strict().DecodeString(parts[3])
	if err != nil {
		return argon2Params{}, nil, nil, errPasswordRecord
	}
	p.saltLen, p.keyLen = len(salt), len(key)
	if !bounds.allow(p) {
		return argon2Params{}, nil, nil, errPasswordRecord
	}
	return p, salt, key, nil
}

// verify reports whether password matches record, comparing in constant
// time, and whether the record should be rehashed with the current
// parameters. An unusable record is errPasswordRecord, after the same work
// as a dummy verification, so a bad row costs what a good one does.
func (h *passwordHasher) verify(record, password string) (ok, rehash bool, err error) {
	p, salt, key, err := decodeArgon2(record, h.bounds)
	if err != nil {
		h.dummyVerify(password)
		return false, false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.iterations, p.memoryKiB, p.threads, p.keyLen32())
	if subtle.ConstantTimeCompare(got, key) != 1 {
		return false, false, nil
	}
	return true, p != h.params, nil
}

// dummyVerify does the work of verifying password against a record of the
// current parameters, and matches nothing. It is what a sign-in for a name
// with no usable password costs, so the answer's timing says nothing about
// whether the name exists (design §5.2).
func (h *passwordHasher) dummyVerify(password string) {
	got := argon2.IDKey([]byte(password), h.dummySalt, h.params.iterations, h.params.memoryKiB, h.params.threads, h.params.keyLen32())
	subtle.ConstantTimeCompare(got, h.dummyKey)
}

// normalizePassword is a password to be set: NFC, then the schema's bounds
// again, since composition can change a string's length (design §5.2). The
// error names the rule, never the password.
func normalizePassword(password string) (string, error) {
	if !utf8.ValidString(password) {
		return "", errors.New("password must be valid UTF-8")
	}
	n := norm.NFC.String(password)
	if err := schema.ValidatePassword(n); err != nil {
		return "", errors.New(strings.TrimPrefix(err.Error(), "schema: "))
	}
	return n, nil
}

// normalizePresented is a password presented to sign in: NFC, as it was
// when it was set. It reports false for one that no account can have (over
// the byte bound after normalization), which the caller fails as a wrong
// password, after the same work.
func normalizePresented(password string) (string, bool) {
	n := norm.NFC.String(password)
	return n, len(n) <= schema.MaxPasswordBytes
}

//go:embed common_passwords.txt
var commonPasswordsText string

// maxCommonPasswords bounds the bundled list (design §5.2: bounded); a test
// holds the file to it.
const maxCommonPasswords = 4096

var (
	commonPasswordsOnce sync.Once
	commonPasswords     map[string]struct{}
	passwordFold        = cases.Fold()
)

// passwordKey is the form the blocklist and the name checks compare: case
// folded, with every space removed, so "Correct Horse Battery Staple" is the
// entry correcthorsebatterystaple.
func passwordKey(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, passwordFold.String(norm.NFC.String(s)))
}

func loadCommonPasswords() map[string]struct{} {
	commonPasswordsOnce.Do(func() {
		commonPasswords = make(map[string]struct{})
		sc := bufio.NewScanner(strings.NewReader(commonPasswordsText))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			commonPasswords[passwordKey(line)] = struct{}{}
		}
	})
	return commonPasswords
}

// minContextWordRunes is the shortest name the policy looks for inside a
// password: a two- or three-letter name is in too many good passwords to
// refuse them all.
const minContextWordRunes = 4

// checkPasswordPolicy reports errPasswordUnacceptable for a normalized
// password that is on the bundled common-password list, is one short unit
// repeated, or contains one of words (the account's name, the server's): the
// policy of design §5.2 beyond the schema's bounds. There is no composition
// rule and no online lookup.
func checkPasswordPolicy(password string, words ...string) error {
	key := passwordKey(password)
	if _, common := loadCommonPasswords()[key]; common {
		return errPasswordUnacceptable
	}
	if repeatedUnit(key) {
		return errPasswordUnacceptable
	}
	for _, w := range words {
		wk := passwordKey(w)
		if utf8.RuneCountInString(wk) >= minContextWordRunes && strings.Contains(key, wk) {
			return errPasswordUnacceptable
		}
	}
	return nil
}

// repeatedUnit reports whether s is one unit of at most four characters
// written over and over ("aaaa…", "abab…", "1234123412341234").
func repeatedUnit(s string) bool {
	r := []rune(s)
	for unit := 1; unit <= 4 && unit < len(r); unit++ {
		same := true
		for i := unit; i < len(r); i++ {
			if r[i] != r[i%unit] {
				same = false
				break
			}
		}
		if same {
			return true
		}
	}
	return false
}

// Hashing pools (design §5.1, security review F3). There are two, so an
// anonymous login flood cannot keep an operator from re-authenticating:
// the anonymous pool serves sign-in, signup, invite redemption and reset
// completion; the authenticated pool serves conversion, password change,
// re-authentication and device links. Each runs at most hashPoolRunning
// hashes at once and lets at most hashPoolQueued more wait, for at most
// hashPoolMaxWait; a request past that is refused at once with 429 and
// Retry-After. Capacity is taken before any hash memory is allocated, and no
// caller holds a SQLite write transaction while it hashes.
const (
	hashPoolRunning = 2
	hashPoolQueued  = 8
	hashPoolMaxWait = 5 * time.Second
	// hashPoolRetryAfter is the Retry-After of a refusal: the queue drains
	// in about a second at the production parameters.
	hashPoolRetryAfter = time.Second
)

// errHashPoolBusy is a request the pool had no room for, or that waited
// longer than the pool allows.
var errHashPoolBusy = errors.New("server: password work is at capacity")

// hashPool bounds password work. The zero value is not usable; see
// newHashPool.
type hashPool struct {
	name    string
	slots   chan struct{}
	mu      sync.Mutex
	waiting int
	queued  int
	maxWait time.Duration
}

func newHashPool(name string, running, queued int, maxWait time.Duration) *hashPool {
	return &hashPool{name: name, slots: make(chan struct{}, running), queued: queued, maxWait: maxWait}
}

// do runs fn holding one of the pool's slots. It returns errHashPoolBusy when
// the queue is full or the wait passes maxWait, and ctx's error when ctx ends
// while it waits; fn has not run in either case. Once fn has started it runs
// to the end: Argon2 cannot be stopped part way, and the slot is held until
// it is done, so the memory bound holds even for a caller that has gone.
func (p *hashPool) do(ctx context.Context, fn func()) error {
	select {
	case p.slots <- struct{}{}:
	default:
		if err := p.wait(ctx); err != nil {
			return err
		}
	}
	defer func() { <-p.slots }()
	fn()
	return nil
}

// wait queues for a slot, and returns holding one, or with an error and
// holding none.
func (p *hashPool) wait(ctx context.Context) error {
	p.mu.Lock()
	if p.waiting >= p.queued {
		p.mu.Unlock()
		return errHashPoolBusy
	}
	p.waiting++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.waiting--
		p.mu.Unlock()
	}()
	timer := time.NewTimer(p.maxWait)
	defer timer.Stop()
	select {
	case p.slots <- struct{}{}:
		return nil
	case <-timer.C:
		return errHashPoolBusy
	case <-ctx.Done():
		return ctx.Err()
	}
}

// passwords is the server's hashing: one hasher and the two pools. The
// methods below are the primitives every password route uses (sign-in,
// signup and invite redemption here; enrolment, change, reset and
// re-authentication in the routes that follow).
type passwords struct {
	hasher        *passwordHasher
	anonymous     *hashPool
	authenticated *hashPool
}

func newPasswords() *passwords {
	h, err := newPasswordHasher(passwordConfigForNew.params, passwordConfigForNew.bounds)
	if err != nil {
		// The parameters are constants of this file, checked by its tests.
		panic(err)
	}
	return &passwords{
		hasher:        h,
		anonymous:     newHashPool("anonymous", hashPoolRunning, hashPoolQueued, hashPoolMaxWait),
		authenticated: newHashPool("authenticated", hashPoolRunning, hashPoolQueued, hashPoolMaxWait),
	}
}

// hashIn hashes a normalized password in pool.
func (pw *passwords) hashIn(ctx context.Context, pool *hashPool, password string) (string, error) {
	var (
		record string
		err    error
	)
	if perr := pool.do(ctx, func() { record, err = pw.hasher.hash(password) }); perr != nil {
		return "", perr
	}
	return record, err
}

// verifyResult is the outcome of one verification in a pool.
type verifyResult struct {
	ok bool
	// rehash, when ok, is the password hashed afresh with the current
	// parameters because the stored record's differ; empty otherwise.
	rehash string
	// badRecord says the stored record could not be verified at all.
	badRecord bool
}

// verifyIn verifies password against record in pool; an empty record is a
// dummy verification, which never matches. A record due for an upgrade is
// rehashed in the same slot, so the upgrade costs no second wait.
func (pw *passwords) verifyIn(ctx context.Context, pool *hashPool, record, password string) (verifyResult, error) {
	var res verifyResult
	err := pool.do(ctx, func() {
		if record == "" {
			pw.hasher.dummyVerify(password)
			return
		}
		ok, rehash, err := pw.hasher.verify(record, password)
		if err != nil {
			res.badRecord = true
			return
		}
		res.ok = ok
		if ok && rehash {
			// A failed rehash costs only the upgrade, which the next
			// sign-in tries again.
			if fresh, err := pw.hasher.hash(password); err == nil {
				res.rehash = fresh
			}
		}
	})
	return res, err
}
