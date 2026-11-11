package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/push/pushtest"
	"github.com/njdaniel/conch/pkg/schema"
)

// --- doubles ---------------------------------------------------------------

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type observer struct {
	mu       sync.Mutex
	wakes    map[string]int
	failures map[string]int
}

func newObserver() *observer {
	return &observer{wakes: map[string]int{}, failures: map[string]int{}}
}

func (o *observer) Wake(outcome string) {
	o.mu.Lock()
	o.wakes[outcome]++
	o.mu.Unlock()
}

func (o *observer) Failure(class string) {
	o.mu.Lock()
	o.failures[class]++
	o.mu.Unlock()
}

func (o *observer) wake(outcome string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.wakes[outcome]
}

func (o *observer) failure(class string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.failures[class]
}

// source is a Source with a fixed target list.
type source struct {
	mu      sync.Mutex
	targets []Target
	posts   []Post
	marked  []string
	reject  []string
	err     error
}

func (s *source) Targets(_ context.Context, p Post, _ time.Time) ([]Target, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts = append(s.posts, p)
	return slices.Clone(s.targets), s.err
}

func (s *source) MarkWake(_ context.Context, id string, _ time.Time) error {
	s.mu.Lock()
	s.marked = append(s.marked, id)
	s.mu.Unlock()
	return nil
}

func (s *source) Reject(_ context.Context, id string, _ time.Time) error {
	s.mu.Lock()
	s.reject = append(s.reject, id)
	s.mu.Unlock()
	return nil
}

func (s *source) rejected() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reject)
}

func (s *source) markedIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.marked)
}

// timers is an AfterFunc that records what it was asked and fires on demand.
type timers struct {
	mu      sync.Mutex
	pending []*timer
}

type timer struct {
	d       time.Duration
	f       func()
	stopped bool
}

func (ts *timers) after(d time.Duration, f func()) func() bool {
	t := &timer{d: d, f: f}
	ts.mu.Lock()
	ts.pending = append(ts.pending, t)
	ts.mu.Unlock()
	return func() bool {
		ts.mu.Lock()
		defer ts.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

func (ts *timers) delays() []time.Duration {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	var out []time.Duration
	for _, t := range ts.pending {
		if !t.stopped {
			out = append(out, t.d)
		}
	}
	return out
}

// fireNext fires the oldest live timer and reports whether there was one.
func (ts *timers) fireNext() bool {
	ts.mu.Lock()
	var next *timer
	for _, t := range ts.pending {
		if !t.stopped {
			next = t
			t.stopped = true
			break
		}
	}
	ts.mu.Unlock()
	if next == nil {
		return false
	}
	next.f()
	return true
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// harness is a real Sender, over the real address-pinned client, against the
// fake provider.
type harness struct {
	prov   *pushtest.Provider
	sub    *pushtest.Subscriber
	src    *source
	obs    *observer
	clk    *clock
	timers *timers
	key    *VAPIDKey
	sender *Sender
	res    *fakeResolver
	target Target
}

type harnessOpts struct {
	resolver *fakeResolver
	subject  string
	expire   time.Duration
	perMin   int
	queue    int
}

// newHarness builds a sender that reaches the provider through the real
// client and dialer, with 127.0.0.1 standing in for a public address. The
// endpoint's host is example.com, which the provider's certificate names, and
// which the resolver maps to the provider's address.
func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	permitLoopback(t)
	h := &harness{prov: pushtest.New(t), sub: pushtest.NewSubscriber(t), src: &source{}, obs: newObserver(), clk: newClock(), timers: &timers{}}
	h.key = newTestKey(t)
	h.res = o.resolver
	if h.res == nil {
		h.res = fixed("127.0.0.1")
	}
	pool := x509.NewCertPool()
	pool.AddCert(h.prov.Certificate())
	client := newClient(NewDialer(h.res, false), pool)

	u, err := url.Parse(h.prov.Endpoint(h.sub))
	if err != nil {
		t.Fatal(err)
	}
	h.target = Target{
		RegistrationID: "reg-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		Endpoint:       "https://example.com:" + u.Port() + u.Path,
		P256DH:         h.sub.P256DH(),
		Auth:           h.sub.Auth(),
	}
	h.src.targets = []Target{h.target}
	h.sender, err = NewSender(SenderConfig{
		Source: h.src, Observer: h.obs, Client: client, Key: h.key,
		Subject: o.subject, AllowPrivate: true, Expire: o.expire, MaxPerMinute: o.perMin,
		Now: h.clk.Now, AfterFunc: h.timers.after, QueueSize: o.queue,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) start(t *testing.T) {
	t.Helper()
	h.sender.Start(context.Background())
	t.Cleanup(h.sender.Stop)
}

func newTestKey(t *testing.T) *VAPIDKey {
	t.Helper()
	k, err := LoadOrCreateVAPIDKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var post = Post{ChannelID: 1, MessageID: 10, AuthorID: 99}

// --- tests -----------------------------------------------------------------

// TestWakeContent: the provider decrypts exactly the wake, the headers are
// exactly the listed ones, and the VAPID token verifies against the served
// key, for the endpoint's origin, within a day.
func TestWakeContent(t *testing.T) {
	for _, tt := range []struct {
		name, subject string
		expire        time.Duration
		wantTTL       string
	}{
		{"no subject, default TTL", "", 0, "86400"},
		{"mailto subject", "mailto:ops@example.org", 2 * time.Hour, "7200"},
		{"https subject", "https://example.org/contact", 28 * 24 * time.Hour, "2419200"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{subject: tt.subject, expire: tt.expire})
			h.start(t)
			h.sender.Enqueue(post)
			d := h.prov.WaitFor(t, 1, 5*time.Second)[0]

			if d.DecryptErr != nil {
				t.Fatalf("decrypt: %v", d.DecryptErr)
			}
			want := `{"schema":"conch.push_wake.v1","registration":"` + h.target.RegistrationID + `"}`
			if string(d.Plain) != want {
				t.Errorf("body = %s, want %s", d.Plain, want)
			}
			if d.Registration != h.target.RegistrationID {
				t.Errorf("registration = %q", d.Registration)
			}
			// Every header but the ones net/http itself adds.
			allowed := map[string]string{
				"Authorization": "", "Ttl": tt.wantTTL, "Topic": "wake", "Urgency": "normal",
				"Content-Encoding": "aes128gcm", "Content-Length": "", "Accept-Encoding": "", "Connection": "close",
			}
			for k, v := range d.Header {
				want, ok := allowed[k]
				if !ok {
					t.Errorf("unexpected header %s: %v", k, v)
					continue
				}
				if want != "" && v[0] != want {
					t.Errorf("header %s = %q, want %q", k, v[0], want)
				}
			}
			for _, k := range []string{"Authorization", "Ttl", "Topic", "Urgency", "Content-Encoding"} {
				if d.Header.Get(k) == "" {
					t.Errorf("header %s is missing", k)
				}
			}
			v := d.VAPID
			if v.Err != nil || !v.SigOK || v.Scheme != "vapid" {
				t.Fatalf("vapid = %+v", v)
			}
			if v.Key != h.key.PublicKey() {
				t.Error("k is not the served vapid_public_key")
			}
			if v.Header["alg"] != "ES256" || v.Header["typ"] != "JWT" {
				t.Errorf("jwt header = %v", v.Header)
			}
			if eu, _ := url.Parse(h.target.Endpoint); v.Claims["aud"] != "https://"+eu.Host {
				t.Errorf("aud = %v, want the endpoint's origin", v.Claims["aud"])
			}
			exp, _ := v.Claims["exp"].(float64)
			if left := time.Unix(int64(exp), 0).Sub(h.clk.Now()); left <= 0 || left > 24*time.Hour {
				t.Errorf("exp is %s ahead, want within 24h", left)
			}
			sub, has := v.Claims["sub"]
			if (tt.subject == "") == has || (has && sub != tt.subject) {
				t.Errorf("sub = %v (present %v), want %q", sub, has, tt.subject)
			}
			waitFor(t, "the wake to be recorded", func() bool { return len(h.src.markedIDs()) == 1 })
			if h.obs.wake(OutcomeSent) != 1 {
				t.Error("the wake was not counted as sent")
			}
		})
	}
}

// TestJWT checks the token by hand: ES256 over the signing input, claims, and
// the rejection of a changed byte.
func TestJWT(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	exp := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name, aud, sub string
	}{
		{"no subject", "https://push.example.net", ""},
		{"subject", "https://push.example.net:8443", "mailto:a@example.org"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jwt, err := SignVAPID(key, tt.aud, tt.sub, exp)
			if err != nil {
				t.Fatal(err)
			}
			pub, _ := key.PublicKey.ECDH()
			v := pushtestVerify(AuthorizationHeader(jwt, b64url(pub.Bytes())))
			if v.Err != nil || !v.SigOK {
				t.Fatalf("verify = %+v", v)
			}
			if v.Claims["aud"] != tt.aud || int64(v.Claims["exp"].(float64)) != exp.Unix() {
				t.Errorf("claims = %v", v.Claims)
			}
			if _, has := v.Claims["sub"]; has != (tt.sub != "") {
				t.Errorf("sub presence = %v", has)
			}
			// A changed claim breaks the signature.
			parts := strings.Split(jwt, ".")
			forged := parts[0] + "." + b64url([]byte(`{"aud":"https://evil.example","exp":1800000000}`)) + "." + parts[2]
			if f := pushtestVerify(AuthorizationHeader(forged, b64url(pub.Bytes()))); f.SigOK {
				t.Error("a forged token verified")
			}
		})
	}
}

func TestValidateSubject(t *testing.T) {
	tests := []struct {
		sub string
		ok  bool
	}{
		{"", true},
		{"mailto:ops@example.org", true},
		{"https://example.org/contact", true},
		{"mailto:", false},
		{"https:", false},
		{"https://", false},
		{"http://example.org", false},
		{"ops@example.org", false},
		{"mailto:a b@example.org", false},
		{"mailto:a\r\n@example.org", false},
		{"javascript:alert(1)", false},
		{"mailto:" + strings.Repeat("a", 300), false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%.30q", tt.sub), func(t *testing.T) {
			if err := ValidateSubject(tt.sub); (err == nil) != tt.ok {
				t.Errorf("ValidateSubject = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// TestStatuses drives the provider's scripted answers through the real sender,
// and checks what each leads to: the outcome, the failure class, the retries
// and the store calls.
func TestStatuses(t *testing.T) {
	tests := []struct {
		name      string
		reply     pushtest.Reply
		outcome   string
		class     string
		requests  int
		rejected  bool
		marked    bool
		firstWait time.Duration // delay of the first retry, if one is scheduled
	}{
		{"201 sent", pushtest.Reply{Status: 201}, OutcomeSent, "", 1, false, true, 0},
		{"200 sent", pushtest.Reply{Status: 200}, OutcomeSent, "", 1, false, true, 0},
		{"404 rejects", pushtest.Reply{Status: 404}, OutcomeProviderRejected, ClassStatus4xx, 1, true, false, 0},
		{"410 rejects", pushtest.Reply{Status: 410}, OutcomeProviderRejected, ClassStatus4xx, 1, true, false, 0},
		{"400 is not retried", pushtest.Reply{Status: 400}, OutcomeFailed, ClassStatus4xx, 1, false, false, 0},
		{"413 is not retried", pushtest.Reply{Status: 413}, OutcomeFailed, ClassStatus4xx, 1, false, false, 0},
		{"302 is not followed", pushtest.Reply{Status: 302, Location: "https://example.com/elsewhere"}, OutcomeFailed, ClassStatus3xx, 1, false, false, 0},
		{"500 retries", pushtest.Reply{Status: 500}, "", ClassStatus5xx, 1, false, false, 2 * time.Second},
		{"503 retries", pushtest.Reply{Status: 503}, "", ClassStatus5xx, 1, false, false, 2 * time.Second},
		{"429 with Retry-After seconds", pushtest.Reply{Status: 429, RetryAfter: "7"}, "", ClassStatus4xx, 1, false, false, 7 * time.Second},
		{"429 with a short Retry-After keeps the backoff", pushtest.Reply{Status: 429, RetryAfter: "1"}, "", ClassStatus4xx, 1, false, false, 2 * time.Second},
		{"429 with a Retry-After past the window is not retried", pushtest.Reply{Status: 429, RetryAfter: "3600"}, OutcomeFailed, ClassStatus4xx, 1, false, false, 0},
		{"429 with garbage Retry-After uses the backoff", pushtest.Reply{Status: 429, RetryAfter: "soon"}, "", ClassStatus4xx, 1, false, false, 2 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{})
			h.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return tt.reply })
			h.start(t)
			h.sender.Enqueue(post)
			h.prov.WaitFor(t, tt.requests, 5*time.Second)
			if tt.outcome != "" {
				waitFor(t, "outcome "+tt.outcome, func() bool { return h.obs.wake(tt.outcome) == 1 })
			} else {
				waitFor(t, "a retry to be scheduled", func() bool { return len(h.timers.delays()) == 1 })
				if got := h.timers.delays()[0]; got != tt.firstWait {
					t.Errorf("first retry in %s, want %s", got, tt.firstWait)
				}
			}
			if tt.class != "" {
				waitFor(t, "failure "+tt.class, func() bool { return h.obs.failure(tt.class) == 1 })
			} else if n := h.obs.failure(ClassStatus4xx) + h.obs.failure(ClassStatus5xx); n != 0 {
				t.Errorf("%d failures counted for a success", n)
			}
			if got := len(h.src.rejected()) == 1; got != tt.rejected {
				t.Errorf("rejected = %v, want %v (%v)", got, tt.rejected, h.src.rejected())
			}
			if got := len(h.src.markedIDs()) == 1; got != tt.marked {
				t.Errorf("marked = %v, want %v", got, tt.marked)
			}
			time.Sleep(20 * time.Millisecond)
			if n := h.prov.Count(); n != tt.requests {
				t.Errorf("provider got %d requests, want %d (a redirect or a retry was made)", n, tt.requests)
			}
		})
	}
}

// TestRetries: up to two retries, then the wake is failed; each retry is a new
// request; the timers are the only waiting.
func TestRetries(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return pushtest.Reply{Status: 500} })
	h.start(t)
	h.sender.Enqueue(post)

	wantDelays := []time.Duration{2 * time.Second, 10 * time.Second}
	for i, want := range wantDelays {
		h.prov.WaitFor(t, i+1, 5*time.Second)
		waitFor(t, fmt.Sprintf("retry %d to be scheduled", i+1), func() bool { return len(h.timers.delays()) == 1 })
		if got := h.timers.delays()[0]; got != want {
			t.Errorf("retry %d in %s, want %s", i+1, got, want)
		}
		h.clk.Advance(want)
		if !h.timers.fireNext() {
			t.Fatal("no timer to fire")
		}
	}
	h.prov.WaitFor(t, 3, 5*time.Second)
	waitFor(t, "the wake to fail", func() bool { return h.obs.wake(OutcomeFailed) == 1 })
	time.Sleep(20 * time.Millisecond)
	if n := h.prov.Count(); n != 3 {
		t.Errorf("requests = %d, want 1 attempt and 2 retries", n)
	}
	if d := h.timers.delays(); len(d) != 0 {
		t.Errorf("a retry is still scheduled: %v", d)
	}
	if got := h.obs.failure(ClassStatus5xx); got != 3 {
		t.Errorf("failures counted = %d, want one per attempt", got)
	}
}

// TestRetryThenSuccess: a 503 and then a 201 is one wake sent.
func TestRetryThenSuccess(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.prov.Script(func(n int, _ *pushtest.Delivery) pushtest.Reply {
		if n == 1 {
			return pushtest.Reply{Status: 503}
		}
		return pushtest.Reply{Status: 201}
	})
	h.start(t)
	h.sender.Enqueue(post)
	waitFor(t, "a retry", func() bool { return len(h.timers.delays()) == 1 })
	h.timers.fireNext()
	waitFor(t, "sent", func() bool { return h.obs.wake(OutcomeSent) == 1 })
	if h.obs.wake(OutcomeFailed) != 0 || h.prov.Count() != 2 {
		t.Errorf("failed=%d requests=%d", h.obs.wake(OutcomeFailed), h.prov.Count())
	}
}

// TestRetryReResolvesAndRechecks is the integration row of T16 for the
// sender: a retry dials, resolves and checks again, so a host that turned
// private between the attempts is refused on the retry.
func TestRetryReResolvesAndRechecks(t *testing.T) {
	res := sequence([]string{"127.0.0.1"}, []string{"10.0.0.7"})
	h := newHarness(t, harnessOpts{resolver: res})
	h.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return pushtest.Reply{Status: 500} })
	h.start(t)
	h.sender.Enqueue(post)
	waitFor(t, "a retry", func() bool { return len(h.timers.delays()) == 1 })
	h.timers.fireNext()
	waitFor(t, "the retry to be refused", func() bool { return h.obs.failure(ClassAddressRefused) == 1 })
	waitFor(t, "the wake to fail", func() bool { return h.obs.wake(OutcomeFailed) == 1 })
	if h.prov.Count() != 1 {
		t.Errorf("the provider got %d requests; the retry must not reach it", h.prov.Count())
	}
	if res.calls() != 2 {
		t.Errorf("lookups = %d, want one per attempt", res.calls())
	}
}

// TestRedirectNotFollowed is the other integration row: a 302 is returned as
// the answer, counted as "status 3xx", and the Location is not requested.
func TestRedirectNotFollowed(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply {
		return pushtest.Reply{Status: 302, Location: "https://example.com/internal"}
	})
	h.start(t)
	h.sender.Enqueue(post)
	waitFor(t, "status 3xx", func() bool { return h.obs.failure(ClassStatus3xx) == 1 })
	waitFor(t, "failed", func() bool { return h.obs.wake(OutcomeFailed) == 1 })
	time.Sleep(20 * time.Millisecond)
	if h.prov.Count() != 1 || len(h.timers.delays()) != 0 {
		t.Errorf("requests=%d retries=%v; a 3xx is neither followed nor retried", h.prov.Count(), h.timers.delays())
	}
}

// TestPortRuleBeforeSend: a stored endpoint on a port the rule does not allow
// is refused before anything is dialled, as it is at registration.
func TestPortRuleBeforeSend(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     string
		allowPrivate bool
		sent         bool
	}{
		{"8443 without allow-private", "https://example.com:8443/x", false, false},
		{"8443 with allow-private", "https://example.com:8443/x", true, true},
		{"explicit 443", "https://example.com:443/x", false, true},
		{"no port", "https://example.com/x", false, true},
		{"port 0", "https://example.com:0/x", true, false},
		{"port 99999", "https://example.com:99999/x", true, false},
		{"not a number", "https://example.com:http/x", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EndpointPortAllowed(tt.endpoint, tt.allowPrivate); got != (tt.sent) {
				t.Errorf("EndpointPortAllowed = %v, want %v", got, tt.sent)
			}
		})
	}
	// Through the sender: refused, counted, and the resolver never asked.
	res := fixed("127.0.0.1")
	h := newHarness(t, harnessOpts{resolver: res})
	h.sender.cfg.AllowPrivate = false
	h.src.targets[0].Endpoint = "https://example.com:8443/x"
	h.start(t)
	h.sender.Enqueue(post)
	waitFor(t, "refused", func() bool { return h.obs.failure(ClassAddressRefused) == 1 })
	if res.calls() != 0 || h.prov.Count() != 0 {
		t.Errorf("lookups=%d requests=%d; nothing may be dialled", res.calls(), h.prov.Count())
	}
}

// TestHangingProviderTimesOut: a provider that never answers costs one worker
// for the client's timeout, and is a "timeout" failure. Other wakes go on.
func TestHangingProviderTimesOut(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.prov.Script(func(n int, _ *pushtest.Delivery) pushtest.Reply {
		if n == 1 {
			return pushtest.Reply{Hang: true}
		}
		return pushtest.Reply{Status: 201}
	})
	second := h.target
	second.RegistrationID = "reg-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	h.src.targets = []Target{h.target, second}
	h.start(t)
	h.sender.Enqueue(post)
	waitFor(t, "the second wake to be sent while the first hangs", func() bool { return h.obs.wake(OutcomeSent) == 1 })
	waitFor(t, "the timeout", func() bool { return h.obs.failure(ClassTimeout) == 1 })
}

// TestSenderBounds: the queue is bounded and never blocks, drops are counted,
// and pending retries are bounded.
func TestSenderBounds(t *testing.T) {
	t.Run("a full queue drops, counts and does not block", func(t *testing.T) {
		h := newHarness(t, harnessOpts{queue: 2})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 100 {
				h.sender.Enqueue(post) // not started: nothing drains
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Enqueue blocked")
		}
		if got := h.obs.wake(OutcomeQueueFull); got != 98 {
			t.Errorf("queue_full = %d, want 98", got)
		}
	})
	t.Run("retries are bounded", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return pushtest.Reply{Status: 500} })
		var targets []Target
		for i := range MaxPendingRetries + 10 {
			tg := h.target
			tg.RegistrationID = fmt.Sprintf("reg-%043d", i)
			targets = append(targets, tg)
		}
		h.src.targets = targets
		h.start(t)
		h.sender.Enqueue(post)
		waitFor(t, "the surplus to be dropped", func() bool { return h.obs.wake(OutcomeRetryDropped) == 10 })
		if n := len(h.timers.delays()); n != MaxPendingRetries {
			t.Errorf("pending retries = %d, want %d", n, MaxPendingRetries)
		}
	})
	t.Run("a stopped sender holds no retries and ends its goroutines", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return pushtest.Reply{Status: 500} })
		h.sender.Start(context.Background())
		h.sender.Enqueue(post)
		waitFor(t, "a retry", func() bool { return len(h.timers.delays()) == 1 })
		h.sender.Stop()
		if d := h.timers.delays(); len(d) != 0 {
			t.Errorf("retries survived Stop: %v", d)
		}
		h.sender.Stop() // twice is fine
	})
}

// TestNewSenderValidation: what a sender refuses to be built with.
func TestNewSenderValidation(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ok := SenderConfig{Source: h.src, Observer: h.obs, Client: http.DefaultClient, Key: h.key}
	tests := []struct {
		name string
		edit func(*SenderConfig)
		ok   bool
	}{
		{"defaults", func(*SenderConfig) {}, true},
		{"no source", func(c *SenderConfig) { c.Source = nil }, false},
		{"no key", func(c *SenderConfig) { c.Key = nil }, false},
		{"expire negative", func(c *SenderConfig) { c.Expire = -time.Second }, false},
		{"expire 28 days", func(c *SenderConfig) { c.Expire = MaxExpire }, true},
		{"expire past 28 days", func(c *SenderConfig) { c.Expire = MaxExpire + time.Second }, false},
		{"budget negative", func(c *SenderConfig) { c.MaxPerMinute = -1 }, false},
		{"bad subject", func(c *SenderConfig) { c.Subject = "http://x" }, false},
		{"good subject", func(c *SenderConfig) { c.Subject = "mailto:a@b.c" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := ok
			tt.edit(&cfg)
			_, err := NewSender(cfg)
			if (err == nil) != tt.ok {
				t.Errorf("NewSender = %v, want ok=%v", err, tt.ok)
			}
		})
	}
}

// TestLimiter covers coalescing, the hourly cap and the budget with an
// injected clock.
func TestLimiter(t *testing.T) {
	reg := func(id string, lastWake, lastFetch time.Time) Target {
		return Target{RegistrationID: id, LastWakeAt: lastWake, LastFetchAt: lastFetch}
	}
	t.Run("coalescing: one per 30 s unless it fetched", func(t *testing.T) {
		c := newClock()
		l := newLimiter(600, c.Now)
		a := "a"
		steps := []struct {
			advance time.Duration
			fetch   time.Time
			want    string
		}{
			{0, time.Time{}, ""},
			{time.Second, time.Time{}, OutcomeCoalesced},
			{28 * time.Second, time.Time{}, OutcomeCoalesced}, // 29 s in
			{time.Second, time.Time{}, ""},                    // 30 s in
			{5 * time.Second, time.Time{}, OutcomeCoalesced},
			{0, c.Now(), ""}, // set below: a fetch after the last wake lifts it
		}
		for i, st := range steps {
			c.Advance(st.advance)
			fetch := st.fetch
			if i == len(steps)-1 {
				fetch = c.Now().Add(-time.Second) // after the wake 5 s ago
			}
			if got := l.admit(reg(a, time.Time{}, fetch)); got != st.want {
				t.Errorf("step %d: %q, want %q", i, got, st.want)
			}
		}
	})
	t.Run("coalescing survives a restart through last_wake_at", func(t *testing.T) {
		c := newClock()
		l := newLimiter(600, c.Now)
		if got := l.admit(reg("a", c.Now().Add(-10*time.Second), time.Time{})); got != OutcomeCoalesced {
			t.Errorf("a wake 10 s ago: %q", got)
		}
		if got := l.admit(reg("b", c.Now().Add(-time.Minute), time.Time{})); got != "" {
			t.Errorf("a wake a minute ago: %q", got)
		}
	})
	t.Run("hourly cap, until a fetch", func(t *testing.T) {
		c := newClock()
		l := newLimiter(1_000_000, c.Now)
		// A phone that fetches after every wake is never coalesced, so only
		// the cap bounds it.
		fetching := func() Target { return reg("a", time.Time{}, c.Now()) }
		for i := range HourlyCap {
			c.Advance(time.Second)
			if got := l.admit(fetching()); got != "" {
				t.Fatalf("wake %d: %q", i, got)
			}
		}
		c.Advance(time.Second)
		cappedAt := c.Now()
		if got := l.admit(fetching()); got != OutcomeCapped {
			t.Errorf("wake %d = %q, want capped", HourlyCap+1, got)
		}
		// Still capped later, with no fetch since: only a fetch lifts it,
		// and the hour passing does not.
		c.Advance(3 * time.Hour)
		if got := l.admit(reg("a", time.Time{}, time.Time{})); got != OutcomeCapped {
			t.Errorf("after the hour = %q, want capped until a fetch", got)
		}
		if got := l.admit(reg("a", time.Time{}, cappedAt.Add(-time.Second))); got != OutcomeCapped {
			t.Errorf("a fetch from before the cap = %q, want capped", got)
		}
		c.Advance(time.Minute)
		if got := l.admit(fetching()); got != "" {
			t.Errorf("after a fetch = %q", got)
		}
		// Another registration is unaffected.
		if got := l.admit(reg("b", time.Time{}, time.Time{})); got != "" {
			t.Errorf("another registration = %q", got)
		}
	})
	t.Run("a fetch below the cap does not clear the count", func(t *testing.T) {
		c := newClock()
		l := newLimiter(1_000_000, c.Now)
		for i := range HourlyCap {
			c.Advance(time.Second)
			if got := l.admit(reg("a", time.Time{}, c.Now())); got != "" {
				t.Fatalf("wake %d: %q", i, got)
			}
		}
		// Had each fetch cleared the count, this would be wake 1 of a new
		// run and be sent.
		c.Advance(time.Second)
		if got := l.admit(reg("a", time.Time{}, c.Now())); got != OutcomeCapped {
			t.Errorf("= %q, want capped", got)
		}
	})
	t.Run("hourly count resets after an hour without reaching the cap", func(t *testing.T) {
		c := newClock()
		l := newLimiter(1_000_000, c.Now)
		for range HourlyCap - 1 {
			l.admit(reg("a", time.Time{}, time.Time{}))
			c.Advance(CoalesceWindow)
		}
		c.Advance(time.Hour)
		for i := range 5 {
			if got := l.admit(reg("a", time.Time{}, time.Time{})); got != "" {
				t.Fatalf("wake %d of the new hour: %q", i, got)
			}
			c.Advance(CoalesceWindow)
		}
	})
	t.Run("budget: a token bucket, refusals cost nothing", func(t *testing.T) {
		c := newClock()
		l := newLimiter(60, c.Now) // one a second, burst 60
		for i := range 60 {
			if got := l.admit(reg(fmt.Sprint(i), time.Time{}, time.Time{})); got != "" {
				t.Fatalf("wake %d: %q", i, got)
			}
		}
		if got := l.admit(reg("x", time.Time{}, time.Time{})); got != OutcomeOverBudget {
			t.Errorf("past the burst = %q", got)
		}
		// The refused one was not recorded, so it may go at once when there is room.
		c.Advance(time.Second)
		if got := l.admit(reg("x", time.Time{}, time.Time{})); got != "" {
			t.Errorf("after a second = %q", got)
		}
		if got := l.admit(reg("y", time.Time{}, time.Time{})); got != OutcomeOverBudget {
			t.Errorf("one token only = %q", got)
		}
		c.Advance(time.Hour)
		for i := range 60 {
			if got := l.admit(reg(fmt.Sprint("z", i), time.Time{}, time.Time{})); got != "" {
				t.Fatalf("refill %d: %q", i, got)
			}
		}
		if got := l.admit(reg("w", time.Time{}, time.Time{})); got != OutcomeOverBudget {
			t.Errorf("the bucket refills to the burst and no further: %q", got)
		}
	})
	t.Run("coalesced and capped wakes spend no budget", func(t *testing.T) {
		c := newClock()
		l := newLimiter(2, c.Now)
		l.admit(reg("a", time.Time{}, time.Time{}))
		for range 50 {
			if got := l.admit(reg("a", time.Time{}, time.Time{})); got != OutcomeCoalesced {
				t.Fatalf("got %q", got)
			}
		}
		if got := l.admit(reg("b", time.Time{}, time.Time{})); got != "" {
			t.Errorf("the second token was spent by coalesced wakes: %q", got)
		}
	})
	t.Run("the tracked state is bounded and forgets", func(t *testing.T) {
		c := newClock()
		l := newLimiter(1_000_000_000, c.Now)
		for i := range MaxTracked {
			if got := l.admit(reg(fmt.Sprint(i), time.Time{}, time.Time{})); got != "" {
				t.Fatalf("wake %d: %q", i, got)
			}
		}
		if got := l.admit(reg("one too many", time.Time{}, time.Time{})); got != OutcomeCapped {
			t.Errorf("past the bound = %q", got)
		}
		c.Advance(2 * time.Hour)
		if got := l.admit(reg("one too many", time.Time{}, time.Time{})); got != "" {
			t.Errorf("after the state aged out = %q", got)
		}
		if len(l.regs) > MaxTracked {
			t.Errorf("tracked %d registrations", len(l.regs))
		}
	})
}

// TestSenderLimitsThroughTheSender: the limits apply to what the sender sends,
// and an over-budget wake is counted and not sent.
func TestSenderLimitsThroughTheSender(t *testing.T) {
	h := newHarness(t, harnessOpts{perMin: 1})
	h.start(t)
	h.sender.Enqueue(post)
	waitFor(t, "sent", func() bool { return h.obs.wake(OutcomeSent) == 1 })
	h.clk.Advance(time.Minute) // past the coalescing window; the budget has refilled
	h.sender.Enqueue(post)
	waitFor(t, "sent again", func() bool { return h.obs.wake(OutcomeSent) == 2 })
	h.sender.Enqueue(post) // same instant: coalesced
	waitFor(t, "coalesced", func() bool { return h.obs.wake(OutcomeCoalesced) == 1 })
	h.clk.Advance(CoalesceWindow)
	h.sender.Enqueue(post) // out of budget
	waitFor(t, "over budget", func() bool { return h.obs.wake(OutcomeOverBudget) == 1 })
	if n := h.prov.Count(); n != 2 {
		t.Errorf("the provider got %d requests, want 2", n)
	}
}

// TestSourceFailureIsNotFatal: a store error loses that post's wakes, counts
// nothing as sent, and the sender goes on.
func TestSourceFailureIsNotFatal(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.src.err = fmt.Errorf("boom")
	h.start(t)
	h.sender.Enqueue(post)
	waitFor(t, "the targets query to run", func() bool { h.src.mu.Lock(); defer h.src.mu.Unlock(); return len(h.src.posts) == 1 })
	h.src.mu.Lock()
	h.src.err = nil
	h.src.mu.Unlock()
	h.sender.Enqueue(post)
	waitFor(t, "sent", func() bool { return h.obs.wake(OutcomeSent) == 1 })
}

// --- the leak test ----------------------------------------------------------

type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// TestNoEndpointInLogsOrCounts registers marked values (a host, a path and a
// query in the endpoint, and the keys), drives every outcome through the real
// sender, and asserts that no captured log line (at debug level, so the
// per-failure line is included) and no label the observer saw holds a marker.
// It follows TestNtfyTransportErrorsDoNotNameTheTopic, without importing it.
func TestNoEndpointInLogsOrCounts(t *testing.T) {
	var logs logCapture
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	outcomes := []struct {
		name  string
		reply pushtest.Reply
		res   *fakeResolver
	}{
		{"sent", pushtest.Reply{Status: 201}, nil},
		{"rejected", pushtest.Reply{Status: 410}, nil},
		{"4xx", pushtest.Reply{Status: 400}, nil},
		{"5xx", pushtest.Reply{Status: 503}, nil},
		{"3xx", pushtest.Reply{Status: 302, Location: "https://marker-location.example/MARKER-LOC"}, nil},
		{"hang", pushtest.Reply{Hang: true}, nil},
		{"private address", pushtest.Reply{}, sequence([]string{"10.1.2.3"})},
		{"lookup failure", pushtest.Reply{}, &fakeResolver{fn: func(int, string) ([]netipAddr, error) { return nil, fmt.Errorf("marker-resolver-error") }}},
	}
	var markers []string
	for _, o := range outcomes {
		t.Run(o.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{resolver: o.res})
			h.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return o.reply })
			u, _ := url.Parse(h.target.Endpoint)
			h.src.targets[0].Endpoint = "https://example.com:" + u.Port() + u.Path + "?marker-query=MARKER-QUERY-1f2e"
			h.start(t)
			h.sender.Enqueue(post)
			total := func() int {
				n := 0
				for _, k := range Outcomes() {
					n += h.obs.wake(k)
				}
				for _, k := range Classes() {
					n += h.obs.failure(k)
				}
				return n + len(h.timers.delays())
			}
			waitFor(t, "an outcome", func() bool { return total() > 0 })
			time.Sleep(30 * time.Millisecond)
			markers = append(markers, h.target.Endpoint, u.Path, u.Port(), h.target.P256DH, h.target.Auth, "MARKER-QUERY-1f2e", "marker-location", "marker-resolver-error", "example.com")
			// Every label the observer saw is from the fixed sets.
			h.obs.mu.Lock()
			defer h.obs.mu.Unlock()
			for k := range h.obs.wakes {
				if !slices.Contains(Outcomes(), k) {
					t.Errorf("outcome label %q is not in the fixed set", k)
				}
			}
			for k := range h.obs.failures {
				if !slices.Contains(Classes(), k) {
					t.Errorf("failure label %q is not in the fixed set", k)
				}
			}
		})
	}
	out := logs.String()
	if !strings.Contains(out, "registration=") {
		t.Fatalf("the failure lines were not captured:\n%s", out)
	}
	for _, m := range markers {
		if m != "" && strings.Contains(out, m) {
			t.Errorf("a log line contains %q:\n%s", m, out)
		}
	}
	// And the registration id and class are what the line has.
	if !strings.Contains(out, `class="status 4xx"`) && !strings.Contains(out, "class=\"status 4xx\"") {
		t.Errorf("a failure line does not carry its class:\n%s", out)
	}
	_ = schema.PushWakeSchemaV1
}

// netipAddr is netip.Addr, aliased so the table above reads without an import
// of its own.
type netipAddr = netip.Addr

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// pushtestVerify checks an Authorization header as the fake provider does.
func pushtestVerify(header string) pushtest.VAPID { return pushtest.VerifyVAPID(header) }

// TestEndpointNowForbiddenIsRefusedOnEverySend: a registration whose host now
// resolves only to forbidden addresses gets "address refused" on every send,
// nothing reaches the provider, and the registration is not ended (an outage
// of this kind is the operator's to see, not the push service's rejection).
func TestEndpointNowForbiddenIsRefusedOnEverySend(t *testing.T) {
	for _, addr := range []string{"10.0.0.7", "169.254.169.254", "127.0.0.1", "192.168.1.1", "100.64.0.1"} {
		t.Run(addr, func(t *testing.T) {
			res := &fakeResolver{fn: func(int, string) ([]netipAddr, error) {
				return []netipAddr{netip.MustParseAddr(addr)}, nil
			}}
			h := newHarness(t, harnessOpts{resolver: res})
			testPermit = nil // the loopback stand-in is off: 127.0.0.1 is forbidden here
			h.start(t)
			for i := range 3 {
				h.clk.Advance(time.Minute)
				h.sender.Enqueue(post)
				want := i + 1
				waitFor(t, "a refusal", func() bool { return h.obs.failure(ClassAddressRefused) == want })
			}
			if h.prov.Count() != 0 {
				t.Errorf("%d requests reached the provider", h.prov.Count())
			}
			if len(h.src.rejected()) != 0 {
				t.Error("a refused address ended the registration")
			}
			if h.obs.wake(OutcomeFailed) != 3 {
				t.Errorf("failed = %d, want 3", h.obs.wake(OutcomeFailed))
			}
		})
	}
}
