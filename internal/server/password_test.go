package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every server this package's tests build hashes with the cheapest
// parameters Argon2 takes, so the hundreds of fixtures that sign someone in
// or redeem an invite cost microseconds. The tests below that are about the
// production parameters build their own hasher with them.
var (
	testArgon2 = argon2Params{memoryKiB: 64, iterations: 1, threads: 1, saltLen: 16, keyLen: 32}
	testBounds = argon2Bounds{
		minMemoryKiB: 8, maxMemoryKiB: 256 * 1024,
		minIterations: 1, maxIterations: 10,
		maxThreads: 4,
		minSaltLen: 8, maxSaltLen: 64,
		minKeyLen: 16, maxKeyLen: 64,
	}
)

func init() {
	passwordConfigForNew.params = testArgon2
	passwordConfigForNew.bounds = testBounds
}

func testHasher(t testing.TB) *passwordHasher {
	t.Helper()
	h, err := newPasswordHasher(testArgon2, testBounds)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The reference implementation's Argon2id vector (phc-winner-argon2,
// src/test.c): "password" with salt "somesalt", t=2, m=64 MiB, p=1. It pins
// the encoding and the algorithm to an implementation other than the one in
// use.
const argon2Reference = "$argon2id$v=19$m=65536,t=2,p=1$c29tZXNhbHQ$CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc"

func TestArgon2ReferenceVector(t *testing.T) {
	h := testHasher(t)
	ok, rehash, err := h.verify(argon2Reference, "password")
	if err != nil || !ok {
		t.Fatalf("verify(reference, password) = %v, %v; want a match", ok, err)
	}
	if !rehash {
		t.Error("a record of other parameters than the hasher's is not marked for rehashing")
	}
	if ok, _, err := h.verify(argon2Reference, "Password"); err != nil || ok {
		t.Errorf("verify(reference, Password) = %v, %v; want no match", ok, err)
	}
	p, salt, key, err := decodeArgon2(argon2Reference, testBounds)
	if err != nil {
		t.Fatal(err)
	}
	if got := encodeArgon2(p, salt, key); got != argon2Reference {
		t.Errorf("re-encoded %q, want %q", got, argon2Reference)
	}
	// The salt is shorter than the server ever makes, so the production
	// bounds refuse to verify it at all.
	if _, _, _, err := decodeArgon2(argon2Reference, productionBounds); !errors.Is(err, errPasswordRecord) {
		t.Errorf("decode under the production bounds = %v, want errPasswordRecord (an 8-byte salt)", err)
	}
}

// The production parameters are the design's baseline and sit inside the
// bounds, whose floor is OWASP's minimum: a record they make verifies, and
// two records of one password differ by their random salts.
func TestProductionParameters(t *testing.T) {
	want := argon2Params{memoryKiB: 64 * 1024, iterations: 3, threads: 1, saltLen: 16, keyLen: 32}
	if productionArgon2 != want {
		t.Errorf("production parameters %+v, want the design's %+v", productionArgon2, want)
	}
	if productionBounds.minMemoryKiB < 19*1024 || productionBounds.minIterations < 2 || productionBounds.minSaltLen < 16 || productionBounds.minKeyLen < 32 {
		t.Errorf("production bounds %+v go below OWASP's minimum (19 MiB, 2 iterations) or the design's salt and key", productionBounds)
	}
	h, err := newPasswordHasher(productionArgon2, productionBounds)
	if err != nil {
		t.Fatal(err)
	}
	const password = "a long and unremarkable sentence" // #nosec G101 -- a test password
	r1, err := h.hash(password)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := h.hash(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(r1, "$argon2id$v=19$m=65536,t=3,p=1$") || r1 == r2 {
		t.Errorf("records %q and %q: want the production prefix and different salts", r1, r2)
	}
	for _, r := range []string{r1, r2} {
		if ok, rehash, err := h.verify(r, password); err != nil || !ok || rehash {
			t.Errorf("verify(own record) = %v, %v, %v; want a match needing no rehash", ok, rehash, err)
		}
		if strings.Contains(r, password) {
			t.Error("the record carries the password")
		}
	}
}

func TestNewPasswordHasherRefusesParameters(t *testing.T) {
	for _, tt := range []struct {
		name string
		p    argon2Params
	}{
		{"below OWASP's memory", argon2Params{memoryKiB: 19*1024 - 1, iterations: 3, threads: 1, saltLen: 16, keyLen: 32}},
		{"one pass", argon2Params{memoryKiB: 64 * 1024, iterations: 1, threads: 1, saltLen: 16, keyLen: 32}},
		{"no lanes", argon2Params{memoryKiB: 64 * 1024, iterations: 3, threads: 0, saltLen: 16, keyLen: 32}},
		{"short salt", argon2Params{memoryKiB: 64 * 1024, iterations: 3, threads: 1, saltLen: 8, keyLen: 32}},
		{"short key", argon2Params{memoryKiB: 64 * 1024, iterations: 3, threads: 1, saltLen: 16, keyLen: 16}},
		{"a gigabyte", argon2Params{memoryKiB: 1 << 20, iterations: 3, threads: 1, saltLen: 16, keyLen: 32}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newPasswordHasher(tt.p, productionBounds); err == nil {
				t.Errorf("newPasswordHasher(%+v) accepted parameters outside the production bounds", tt.p)
			}
		})
	}
}

// A stored record is parsed strictly and bounded before anything is
// allocated for it: a corrupt or tampered row is refused, never verified.
func TestDecodeArgon2Refuses(t *testing.T) {
	const salt, key = "c2FsdHNhbHRzYWx0c2FsdA", "CTFhFdXPJO1aFaMaO6Mm5c8y7cJHAph8ArZWb2GRPPc"
	good := "$argon2id$v=19$m=65536,t=3,p=1$" + salt + "$" + key
	if _, _, _, err := decodeArgon2(good, productionBounds); err != nil {
		t.Fatalf("the well-formed record is refused: %v", err)
	}
	for _, tt := range []struct{ name, record string }{
		{"empty", ""},
		{"argon2i", "$argon2i$v=19$m=65536,t=3,p=1$" + salt + "$" + key},
		{"argon2d", "$argon2d$v=19$m=65536,t=3,p=1$" + salt + "$" + key},
		{"old version", "$argon2id$v=16$m=65536,t=3,p=1$" + salt + "$" + key},
		{"no version", "$argon2id$m=65536,t=3,p=1$" + salt + "$" + key},
		{"fields out of order", "$argon2id$v=19$t=3,m=65536,p=1$" + salt + "$" + key},
		{"missing field", "$argon2id$v=19$m=65536,t=3$" + salt + "$" + key},
		{"extra field", "$argon2id$v=19$m=65536,t=3,p=1,k=1$" + salt + "$" + key},
		{"leading zero", "$argon2id$v=19$m=065536,t=3,p=1$" + salt + "$" + key},
		{"plus sign", "$argon2id$v=19$m=+65536,t=3,p=1$" + salt + "$" + key},
		{"space", "$argon2id$v=19$m= 65536,t=3,p=1$" + salt + "$" + key},
		{"too much memory", "$argon2id$v=19$m=4194304,t=3,p=1$" + salt + "$" + key},
		{"overflowing memory", "$argon2id$v=19$m=99999999999,t=3,p=1$" + salt + "$" + key},
		{"too little memory", "$argon2id$v=19$m=1024,t=3,p=1$" + salt + "$" + key},
		{"too many passes", "$argon2id$v=19$m=65536,t=1000,p=1$" + salt + "$" + key},
		{"too few passes", "$argon2id$v=19$m=65536,t=1,p=1$" + salt + "$" + key},
		{"no lanes", "$argon2id$v=19$m=65536,t=3,p=0$" + salt + "$" + key},
		{"too many lanes", "$argon2id$v=19$m=65536,t=3,p=255$" + salt + "$" + key},
		{"short salt", "$argon2id$v=19$m=65536,t=3,p=1$c29tZXNhbHQ$" + key},
		{"huge salt", "$argon2id$v=19$m=65536,t=3,p=1$" + strings.Repeat("A", 1<<16) + "$" + key},
		{"short key", "$argon2id$v=19$m=65536,t=3,p=1$" + salt + "$CTFhFdXPJO1aFaMa"},
		{"padded base64", "$argon2id$v=19$m=65536,t=3,p=1$" + salt + "==$" + key},
		{"not base64", "$argon2id$v=19$m=65536,t=3,p=1$" + salt + "$" + strings.Repeat("!", 43)},
		{"trailing field", good + "$x"},
		{"no hash", "$argon2id$v=19$m=65536,t=3,p=1$" + salt},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, _, err := decodeArgon2(tt.record, productionBounds); !errors.Is(err, errPasswordRecord) {
				t.Errorf("decodeArgon2 = %v, want errPasswordRecord", err)
			}
		})
	}
}

func TestVerifyRecords(t *testing.T) {
	h := testHasher(t)
	record, err := h.hash("the right password, long enough")
	if err != nil {
		t.Fatal(err)
	}
	older, err := newPasswordHasher(argon2Params{memoryKiB: 64, iterations: 2, threads: 1, saltLen: 16, keyLen: 32}, testBounds)
	if err != nil {
		t.Fatal(err)
	}
	olderRecord, err := older.hash("the right password, long enough")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, record, password string
		ok, rehash             bool
		err                    error
	}{
		{"right", record, "the right password, long enough", true, false, nil},
		{"wrong", record, "the wrong password, long enough", false, false, nil},
		{"one character more", record, "the right password, long enough.", false, false, nil},
		{"older parameters", olderRecord, "the right password, long enough", true, true, nil},
		{"older parameters, wrong", olderRecord, "the wrong password, long enough", false, false, nil},
		{"not a record", "plaintext", "plaintext", false, false, errPasswordRecord},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ok, rehash, err := h.verify(tt.record, tt.password)
			if ok != tt.ok || rehash != tt.rehash || !errors.Is(err, tt.err) {
				t.Errorf("verify = %v, %v, %v; want %v, %v, %v", ok, rehash, err, tt.ok, tt.rehash, tt.err)
			}
		})
	}
}

// Passwords are normalized to NFC before their length is checked: a password
// typed with combining accents is the same password, and as long, as one
// typed with precomposed letters.
func TestNormalizePassword(t *testing.T) {
	composed := strings.Repeat("é", 15)    // é, precomposed: 15 characters
	decomposed := strings.Repeat("é", 15) // e and a combining acute: 30 before NFC
	for _, tt := range []struct {
		name, in, want string
		err            bool
	}{
		{"plain", "fifteen letters", "fifteen letters", false},
		{"spaces kept", "  spaces  inside  ", "  spaces  inside  ", false},
		{"precomposed", composed, composed, false},
		{"decomposed becomes precomposed", decomposed, composed, false},
		{"long only before NFC", strings.Repeat("é", 8), "", true},
		{"fourteen", strings.Repeat("x", 14), "", true},
		{"too many characters", strings.Repeat("x", 129), "", true},
		// 128 four-byte characters are exactly the byte bound.
		{"at the byte bound", strings.Repeat("🐚", 128), strings.Repeat("🐚", 128), false},
		{"invalid UTF-8", "fifteen letters\xff", "", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizePassword(tt.in)
			if (err != nil) != tt.err || got != tt.want {
				t.Fatalf("normalizePassword = %q, %v; want %q, error %v", got, err, tt.want, tt.err)
			}
			if err != nil && strings.Contains(err.Error(), tt.in) {
				t.Errorf("the error %q echoes the password", err)
			}
		})
	}
}

func TestCheckPasswordPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, password string
		words          []string
		ok             bool
	}{
		{"a sentence", "rain on the tin roof at four", nil, true},
		{"common", "passwordpassword", nil, false},
		{"common, other case", "PasswordPassword", nil, false},
		{"common, with spaces", "correct horse battery staple", nil, false},
		{"common, with other spaces", "Correct Horse  Battery Staple", nil, false},
		{"one letter", "aaaaaaaaaaaaaaaa", nil, false},
		{"one pair", "abababababababab", nil, false},
		{"four repeated", "1234123412341234", nil, false},
		{"five repeated is not a unit", "abcdeabcdeabcde", nil, true},
		{"the account's name", "my name is Wrenfield, hello", []string{"wrenfield"}, false},
		{"the account's name, other case", "WRENFIELD likes long walks", []string{"wrenfield"}, false},
		{"a short name is not looked for", "the bee flew over the field", []string{"bee"}, true},
		{"the server's name", "welcome to example-chat today", []string{"wren", "example-chat"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := checkPasswordPolicy(tt.password, tt.words...)
			if (err == nil) != tt.ok {
				t.Fatalf("checkPasswordPolicy = %v, want acceptable %v", err, tt.ok)
			}
			if err != nil && (!errors.Is(err, errPasswordUnacceptable) || strings.Contains(err.Error(), tt.password)) {
				t.Errorf("error %q: want errPasswordUnacceptable, without the password", err)
			}
		})
	}
}

func TestCommonPasswordsBounded(t *testing.T) {
	list := loadCommonPasswords()
	if len(list) == 0 || len(list) > maxCommonPasswords {
		t.Fatalf("%d common passwords, want 1 to %d", len(list), maxCommonPasswords)
	}
	for k := range list {
		if k == "" || k != passwordKey(k) {
			t.Errorf("entry %q is not in its compared form", k)
		}
	}
}

// fillPool holds every running slot of p and every queue place, and returns
// the function that lets them all go. The slots are held by goroutines
// whose work blocks until released, as a hash in progress would.
func fillPool(t *testing.T, p *hashPool) (release func()) {
	t.Helper()
	gate := make(chan struct{})
	var started, done sync.WaitGroup
	for i := 0; i < cap(p.slots); i++ {
		started.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			if err := p.do(context.Background(), func() { started.Done(); <-gate }); err != nil {
				t.Errorf("holding a slot: %v", err)
				started.Done()
			}
		}()
	}
	started.Wait()
	for i := 0; i < p.queued; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			_ = p.do(context.Background(), func() {})
		}()
	}
	waitFor(t, "the queue to fill", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.waiting == p.queued })
	var once sync.Once
	release = func() { once.Do(func() { close(gate); done.Wait() }) }
	t.Cleanup(release)
	return release
}

// The pool runs at most its running slots at once and queues at most its
// queue; past that a request is refused at once, without running.
func TestHashPoolBounds(t *testing.T) {
	p := newHashPool("test", hashPoolRunning, hashPoolQueued, time.Minute)
	release := fillPool(t, p)
	ran := false
	start := time.Now()
	if err := p.do(context.Background(), func() { ran = true }); !errors.Is(err, errHashPoolBusy) {
		t.Fatalf("a request past the queue = %v, want errHashPoolBusy", err)
	}
	if ran || time.Since(start) > time.Second {
		t.Errorf("a refused request ran (%v) or waited %v", ran, time.Since(start))
	}
	release()

	// Under a flood, never more than the running slots at once.
	var running, most atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.do(context.Background(), func() {
				n := running.Add(1)
				for {
					m := most.Load()
					if n <= m || most.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				running.Add(-1)
			})
		}()
	}
	wg.Wait()
	if got := most.Load(); got < 1 || got > hashPoolRunning {
		t.Errorf("at most %d ran at once, want 1 to %d", got, hashPoolRunning)
	}
}

// A queued request that waits past the pool's bound is refused, and one
// whose caller goes away leaves the queue; neither runs, and the place it
// held is free again.
func TestHashPoolWaitEnds(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		p := newHashPool("test", 1, 1, 20*time.Millisecond)
		gate := make(chan struct{})
		defer close(gate)
		go func() { _ = p.do(context.Background(), func() { <-gate }) }()
		waitFor(t, "the slot to be taken", func() bool { return len(p.slots) == 1 })
		ran := false
		if err := p.do(context.Background(), func() { ran = true }); !errors.Is(err, errHashPoolBusy) || ran {
			t.Errorf("a request that waited too long = %v, ran %v; want errHashPoolBusy, not run", err, ran)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		p := newHashPool("test", 1, 1, time.Minute)
		gate := make(chan struct{})
		defer close(gate)
		go func() { _ = p.do(context.Background(), func() { <-gate }) }()
		waitFor(t, "the slot to be taken", func() bool { return len(p.slots) == 1 })
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- p.do(ctx, func() { t.Error("a cancelled request ran") }) }()
		waitFor(t, "the request to queue", func() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.waiting == 1 })
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled wait = %v, want context.Canceled", err)
		}
		p.mu.Lock()
		waiting := p.waiting
		p.mu.Unlock()
		if waiting != 0 {
			t.Errorf("%d still waiting after the cancellation; its place is not freed", waiting)
		}
	})
}

// The anonymous and authenticated pools are separate (review F3): an
// anonymous flood that fills one leaves the other serving, so an operator
// can still re-authenticate to stop it.
func TestHashPoolsSeparate(t *testing.T) {
	pw := newPasswords()
	fillPool(t, pw.anonymous)
	if _, err := pw.hashIn(context.Background(), pw.anonymous, "an anonymous request"); !errors.Is(err, errHashPoolBusy) {
		t.Fatalf("the full anonymous pool = %v, want errHashPoolBusy", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := pw.hashIn(context.Background(), pw.authenticated, "an operator re-authenticating")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the authenticated pool during the flood: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the authenticated pool is blocked by the anonymous one")
	}
}

// A verification with no record is the dummy one: it does the work and
// matches nothing, whatever the password.
func TestVerifyInDummy(t *testing.T) {
	pw := newPasswords()
	res, err := pw.verifyIn(context.Background(), pw.anonymous, "", "")
	if err != nil || res.ok || res.badRecord || res.rehash != "" {
		t.Errorf("dummy verification = %+v, %v; want no match", res, err)
	}
	res, err = pw.verifyIn(context.Background(), pw.anonymous, "$argon2id$nonsense", "anything at all here")
	if err != nil || res.ok || !res.badRecord {
		t.Errorf("verification of a bad record = %+v, %v; want a bad record, no match", res, err)
	}
}

// The cost of one production hash, and of a flood through one pool. On the
// development machine of issue #707 (AMD Ryzen 7 7700X, 16 threads, Go
// 1.25.0, shared with other work) one hash took about 75 ms and allocated
// 64 MiB; a pool of two ran about 24 a second and holds at most 128 MiB, both
// pools together 256 MiB. The target host is to be measured the same way.
func BenchmarkArgon2idProduction(b *testing.B) {
	h, err := newPasswordHasher(productionArgon2, productionBounds)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.hash("a long and unremarkable sentence"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHashPoolProduction(b *testing.B) {
	h, err := newPasswordHasher(productionArgon2, productionBounds)
	if err != nil {
		b.Fatal(err)
	}
	p := newHashPool("bench", hashPoolRunning, 1<<20, time.Hour)
	b.ReportAllocs()
	b.SetParallelism(8)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := p.do(context.Background(), func() { _, _ = h.hash("a long and unremarkable sentence") }); err != nil {
				b.Error(err)
			}
		}
	})
}
