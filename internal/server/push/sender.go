package push

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The Web Push sender (issue #1715; docs/design/mobile-notifications.md §5.6,
// §5.7, §6.2, §6.6). A post that a registered phone's sign-in should hear
// about becomes one content-free wake, sent to that phone's push endpoint. The
// sender holds the rules of how many and how often; it knows no store and no
// route, only a Source it asks for targets and tells what happened.
//
// Never slow a post. Enqueue is the post's whole involvement: a non-blocking
// send to a bounded channel, so a full queue drops the wake and counts it.
// Everything else (the targets query, the limits, the encryption, the request)
// runs in the sender's own goroutines.
//
// Bounds, all fixed:
//
//   - the post queue holds QueueSize posts (1024) and the send queue the same
//     number of wakes; past them wakes are dropped and counted (queue_full);
//   - Workers (4) goroutines send, so one slow endpoint stalls one worker and
//     never the rest;
//   - a wake is attempted once and retried at most MaxRetries (2) times, on
//     a 5xx or 429 only, all within RetryWindow (one minute) of its first
//     attempt; the retries are timers and never a sleep in a worker, at most
//     MaxPendingRetries (256) are pending, and one past that is dropped and
//     counted (retry_dropped);
//   - the per-registration state (coalescing, the hourly count, the failure
//     backoff) and the per-person budget share hold at most MaxTracked entries
//     each and forget one an hour after its last wake (limiter.go);
//   - a retry is preceded by a liveness check of the registration, and is dropped
//     silently if it has ended.
//
// What is never in a log line, an error, a metric or an audit row: an endpoint,
// a host, a path, a key, a token. A request's own error is a *url.Error whose
// text is the endpoint, so it is reduced with Classify at the one place it is
// made and no error from a send leaves that function.

// TransportWebPush is the transport label of the sender's metrics.
const TransportWebPush = "webpush"

// The outcomes of one wake, which are the metric's outcome label.
const (
	OutcomeSent             = "sent"
	OutcomeCoalesced        = "coalesced"
	OutcomeCapped           = "capped"
	OutcomeOverBudget       = "over_budget"
	OutcomeQueueFull        = "queue_full"
	OutcomeRetryDropped     = "retry_dropped"
	OutcomeProviderRejected = "provider_rejected"
	OutcomeFailed           = "failed"
	// OutcomeBackoff: the registration is backing off after a network failure,
	// and the wake was skipped.
	OutcomeBackoff = "backoff"
)

// Outcomes returns every outcome, for a metric that wants its label set up
// front.
func Outcomes() []string {
	return []string{
		OutcomeSent, OutcomeCoalesced, OutcomeCapped, OutcomeOverBudget,
		OutcomeQueueFull, OutcomeRetryDropped, OutcomeProviderRejected, OutcomeFailed, OutcomeBackoff,
	}
}

// The limits of the design (§5.6, §5.7) and the sender's bounds.
const (
	// CoalesceWindow is the least time between two wakes of one registration,
	// unless it fetched in between.
	CoalesceWindow = 30 * time.Second
	// HourlyCap is the most wakes one registration gets in an hour. Past it
	// wakes are dropped until the registration next fetches.
	HourlyCap = 120
	// DefaultMaxPerMinute is the per-server budget when none is configured.
	DefaultMaxPerMinute = 600
	// DefaultExpire is the TTL of a wake when none is configured.
	DefaultExpire = 24 * time.Hour
	// MaxExpire is the longest TTL accepted (FCM's and the Web Push services'
	// practical maximum).
	MaxExpire = 28 * 24 * time.Hour
	// QueueSize is the capacity of the post queue and of the send queue.
	QueueSize = 1024
	// Workers is the size of the send pool.
	Workers = 4
	// MaxRetries is how many times a wake is retried after its first attempt.
	MaxRetries = 2
	// RetryWindow bounds all of a wake's attempts, from the first.
	RetryWindow = time.Minute
	// MaxPendingRetries is how many retries may be waiting at once.
	MaxPendingRetries = 256
	// MaxTracked is the most registrations the sender keeps limit state for.
	MaxTracked = 20000
	// targetsTimeout bounds one targets query, and a store write after a send.
	storeTimeout = 10 * time.Second
	// warnEvery thins the outage and overflow warnings.
	warnEvery = time.Minute
)

// retryBackoff is the delay before the first and the second retry. Both fit,
// with the attempts, inside RetryWindow.
var retryBackoff = [MaxRetries]time.Duration{2 * time.Second, 10 * time.Second}

// Post names a committed message. It holds no text.
type Post struct {
	ChannelID, MessageID, AuthorID int64
}

// Target is one registration to wake, as a Source gives it.
type Target struct {
	RegistrationID string
	// PrincipalID is the person, for their share of the budget.
	PrincipalID int64
	Endpoint    string
	P256DH      string
	Auth        string
	// LastWakeAt and LastFetchAt are the registration's own times; the zero
	// time is "never".
	LastWakeAt  time.Time
	LastFetchAt time.Time
}

// Source is what the sender needs of the store.
type Source interface {
	// Targets returns the registrations a post should wake, under every rule
	// of the post-time policy (store.PushTargets).
	Targets(ctx context.Context, p Post, now time.Time) ([]Target, error)
	// MarkWake records a wake sent at now.
	MarkWake(ctx context.Context, registrationID string, now time.Time) error
	// Live reports whether the registration is still active and its
	// credential still live, which is asked before a retry.
	Live(ctx context.Context, registrationID string, now time.Time) (bool, error)
	// Reject ends a registration the push service refused (404 or 410), with
	// one audit row, once.
	Reject(ctx context.Context, registrationID string, now time.Time) error
}

// Observer receives the sender's counts. Every value is from a fixed set:
// outcomes from Outcomes, classes from Classes.
type Observer interface {
	Wake(outcome string)
	Failure(class string)
}

// SenderConfig configures a Sender.
type SenderConfig struct {
	Source   Source
	Observer Observer
	// Client sends every request. It is push.NewClient's, and never one built
	// elsewhere: the only seam is a test's, to trust a test server's
	// certificate.
	Client *http.Client
	// Key signs the VAPID token and names the application server.
	Key *VAPIDKey
	// Subject is the optional VAPID sub claim: "mailto:" or "https:".
	Subject string
	// AllowPrivate is --push-webpush-allow-private, for the port rule.
	AllowPrivate bool
	// Expire is the TTL of a wake, in (0, MaxExpire]. Zero means DefaultExpire.
	Expire time.Duration
	// MaxPerMinute is the per-server budget. Zero means DefaultMaxPerMinute.
	MaxPerMinute int

	// Now, AfterFunc, Workers and QueueSize are seams for tests; nil and zero
	// mean the clock, time.AfterFunc, Workers and QueueSize.
	Now       func() time.Time
	AfterFunc func(d time.Duration, f func()) (stop func() bool)
	Workers   int
	QueueSize int
}

// ValidateExpire checks a --push-expire value.
func ValidateExpire(d time.Duration) error {
	if d < time.Second {
		return errors.New("must be at least 1s")
	}
	if d > MaxExpire {
		return fmt.Errorf("must be at most %s", MaxExpire)
	}
	return nil
}

// ValidateMaxPerMinute checks a --push-max-per-minute value.
func ValidateMaxPerMinute(n int) error {
	if n < 1 {
		return errors.New("must be at least 1")
	}
	if n > 1_000_000 {
		return errors.New("must be at most 1000000")
	}
	return nil
}

// job is one wake on its way: its target and how far along it is.
type job struct {
	target  Target
	attempt int
	first   time.Time
}

// Sender makes and sends wakes. Make one with NewSender; it does nothing, and
// runs no goroutine, until Start.
type Sender struct {
	cfg     SenderConfig
	key     *ecdsa.PrivateKey
	pubKey  string
	ttl     string
	posts   chan Post
	jobs    chan *job
	limits  *limiter
	workers int

	queueWarn, failWarn warnLimiter

	mu      sync.Mutex
	pending map[*job]func() bool // retries waiting, with their stop functions
	stopped bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	started bool
}

// NewSender returns a Sender for cfg, or an error naming what is wrong with
// it. The errors name no value that is a secret.
func NewSender(cfg SenderConfig) (*Sender, error) {
	if cfg.Source == nil || cfg.Observer == nil || cfg.Client == nil || cfg.Key == nil {
		return nil, errors.New("push: a sender needs a source, an observer, a client and a key")
	}
	if cfg.Expire == 0 {
		cfg.Expire = DefaultExpire
	}
	if err := ValidateExpire(cfg.Expire); err != nil {
		return nil, fmt.Errorf("push: expire %w", err)
	}
	if cfg.MaxPerMinute == 0 {
		cfg.MaxPerMinute = DefaultMaxPerMinute
	}
	if err := ValidateMaxPerMinute(cfg.MaxPerMinute); err != nil {
		return nil, fmt.Errorf("push: max per minute %w", err)
	}
	if err := ValidateSubject(cfg.Subject); err != nil {
		return nil, fmt.Errorf("push: subject: %w", err)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) func() bool {
			return time.AfterFunc(d, f).Stop
		}
	}
	workers, size := cfg.Workers, cfg.QueueSize
	if workers <= 0 {
		workers = Workers
	}
	if size <= 0 {
		size = QueueSize
	}
	return &Sender{
		cfg:     cfg,
		key:     cfg.Key.Private(),
		pubKey:  cfg.Key.PublicKey(),
		ttl:     strconv.FormatInt(int64(cfg.Expire/time.Second), 10),
		posts:   make(chan Post, size),
		jobs:    make(chan *job, size),
		limits:  newLimiter(cfg.MaxPerMinute, cfg.Now),
		workers: workers,
		pending: map[*job]func() bool{},
	}, nil
}

// Enqueue offers a committed message to the sender. It never blocks and never
// waits on anything: if the queue is full, or the sender has stopped, the wake
// is dropped and counted (queue_full), and a warning is logged at most once a
// minute. It reads no message and does no I/O, so it is safe to call in the
// middle of a request.
func (s *Sender) Enqueue(p Post) {
	select {
	case s.posts <- p:
	default:
		s.cfg.Observer.Wake(OutcomeQueueFull)
		if n, ok := s.queueWarn.allow(s.cfg.Now()); ok {
			slog.Warn("push: the wake queue is full; wakes are being dropped", "dropped_since_last_warning", n)
		}
	}
}

// Start runs the sender: one goroutine that evaluates posts and Workers that
// send. It returns at once. Stop ends them. Start twice, or after Stop, does
// nothing.
func (s *Sender) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.stopped {
		return
	}
	s.started = true
	ctx, s.cancel = context.WithCancel(ctx)
	s.wg.Add(1 + s.workers)
	go s.evaluate(ctx)
	for range s.workers {
		go s.work(ctx)
	}
}

// Stop ends the sender and waits for its goroutines. Wakes still queued and
// retries still pending are dropped, as documented: a wake is a hint, and the
// feed is what the phone reads. It is safe to call more than once, and before
// Start.
func (s *Sender) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	cancel := s.cancel
	for j, stop := range s.pending {
		stop()
		delete(s.pending, j)
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
}

// evaluate turns posts into wakes: ask the source who is to be woken, apply
// the limits, and queue what passes for the workers.
func (s *Sender) evaluate(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-s.posts:
			s.evaluatePost(ctx, p)
		}
	}
}

func (s *Sender) evaluatePost(ctx context.Context, p Post) {
	qctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	now := s.cfg.Now()
	targets, err := s.cfg.Source.Targets(qctx, p, now)
	if err != nil {
		if ctx.Err() == nil {
			// The store's error names a statement and never a target.
			if n, ok := s.failWarn.allow(now); ok {
				slog.Warn("push: could not work out whom to wake; those wakes are lost", "failures_since_last_warning", n, "error", err)
			}
		}
		return
	}
	for _, t := range targets {
		outcome, undo := s.limits.admitUndo(t)
		if outcome != "" {
			s.cfg.Observer.Wake(outcome)
			continue
		}
		select {
		case s.jobs <- &job{target: t, first: now}:
		default:
			// Dropped before it was sent: it takes back what it spent.
			undo()
			s.cfg.Observer.Wake(OutcomeQueueFull)
		}
	}
}

// work sends queued wakes until ctx ends.
func (s *Sender) work(ctx context.Context) {
	defer s.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case j := <-s.jobs:
			s.send(ctx, j)
		}
	}
}

// result is what one attempt came to. It carries a class and never an error:
// the client's errors quote the endpoint.
type result struct {
	status     int           // the response status, 0 when there was none
	class      string        // "" for success, else one of Classes()
	retryAfter time.Duration // from the response's Retry-After, 0 when absent
}

// send makes one attempt of j and decides what comes next.
func (s *Sender) send(ctx context.Context, j *job) {
	if j.attempt > 0 && !s.stillLive(ctx, j.target.RegistrationID) {
		// It ended, or its sign-in did, since the first attempt: drop the retry
		// without a word. The stored target has been cleared, and the person
		// has nothing to be told.
		return
	}
	res := s.attempt(ctx, j.target)
	if ctx.Err() != nil {
		return
	}
	if res.class != "" && res.status == 0 {
		// No answer at all: a timeout or another network failure, and not a
		// status. The registration is skipped for a growing interval.
		s.limits.networkFailed(j.target.RegistrationID, s.cfg.Now())
	}
	switch {
	case res.class == "":
		s.cfg.Observer.Wake(OutcomeSent)
		s.markWake(ctx, j.target.RegistrationID)
		return
	case res.status == http.StatusNotFound || res.status == http.StatusGone:
		// The push service forgot the subscription: that is not an outage, so
		// it is counted and logged at debug but does not raise the warning.
		s.failedQuietly(j.target.RegistrationID, res.class)
		s.cfg.Observer.Wake(OutcomeProviderRejected)
		s.reject(ctx, j.target.RegistrationID)
		return
	}
	s.failed(j.target.RegistrationID, res.class)
	if res.status == http.StatusTooManyRequests || res.status >= 500 && res.status < 600 {
		if s.scheduleRetry(j, res.retryAfter) {
			return
		}
		if j.attempt < MaxRetries && s.retryFits(j, res.retryAfter) {
			// A retry was owed and could not be held.
			s.cfg.Observer.Wake(OutcomeRetryDropped)
			return
		}
	}
	s.cfg.Observer.Wake(OutcomeFailed)
}

// stillLive asks the source whether a registration may still be retried. A
// failed question is a no.
func (s *Sender) stillLive(ctx context.Context, id string) bool {
	sctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	ok, err := s.cfg.Source.Live(sctx, id, s.cfg.Now())
	return err == nil && ok
}

// failedQuietly counts and logs one failed attempt: the class and the
// registration's id, nothing else, at debug level.
func (s *Sender) failedQuietly(registration, class string) {
	s.cfg.Observer.Failure(class)
	slog.Debug("push: a wake failed", "registration", registration, "class", class)
}

// failed is failedQuietly and, during an outage, the warning, thinned to one
// a minute.
func (s *Sender) failed(registration, class string) {
	s.failedQuietly(registration, class)
	if n, ok := s.failWarn.allow(s.cfg.Now()); ok {
		slog.Warn("push: wakes are failing; phones are not being woken until the push service answers", "class", class, "failures_since_last_warning", n)
	}
}

// backoff is the delay before retry number j.attempt+1, honouring a
// Retry-After that is longer.
func backoff(j *job, retryAfter time.Duration) time.Duration {
	d := retryBackoff[j.attempt]
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// retryFits reports whether j may be retried at all: it has retries left, and
// the delay (a Retry-After that is longer than the window leaves, gives
// the retry up) ends inside RetryWindow of the first attempt.
func (s *Sender) retryFits(j *job, retryAfter time.Duration) bool {
	if j.attempt >= MaxRetries {
		return false
	}
	return s.cfg.Now().Add(backoff(j, retryAfter)).Sub(j.first) <= RetryWindow
}

// scheduleRetry holds j for another attempt after its backoff, and reports
// whether it did. It is a timer, never a sleep. It refuses, and the caller
// counts, when no retry is owed or fits, when MaxPendingRetries are already
// waiting, or when the sender has stopped.
func (s *Sender) scheduleRetry(j *job, retryAfter time.Duration) bool {
	if !s.retryFits(j, retryAfter) {
		return false
	}
	next := &job{target: j.target, attempt: j.attempt + 1, first: j.first}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || len(s.pending) >= MaxPendingRetries {
		return false
	}
	s.pending[next] = s.cfg.AfterFunc(backoff(j, retryAfter), func() { s.fire(next) })
	return true
}

// fire puts a retry back on the send queue when its time comes. A full queue
// drops it, counted.
func (s *Sender) fire(j *job) {
	s.mu.Lock()
	_, held := s.pending[j]
	delete(s.pending, j)
	stopped := s.stopped
	s.mu.Unlock()
	if !held || stopped {
		return
	}
	select {
	case s.jobs <- j:
	default:
		s.cfg.Observer.Wake(OutcomeRetryDropped)
	}
}

// markWake and reject write to the store after a send. Their errors are the
// store's, which name no target; a failure is a debug line, because a wake that
// was sent is sent.
func (s *Sender) markWake(ctx context.Context, id string) {
	sctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	now := s.cfg.Now()
	s.limits.sent(id, now)
	if err := s.cfg.Source.MarkWake(sctx, id, now); err != nil && ctx.Err() == nil {
		slog.Debug("push: could not record a wake", "registration", id)
	}
}

func (s *Sender) reject(ctx context.Context, id string) {
	sctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	s.limits.forget(id)
	if err := s.cfg.Source.Reject(sctx, id, s.cfg.Now()); err != nil && ctx.Err() == nil {
		slog.Warn("push: could not end a registration the push service refused", "registration", id)
	}
}

// wakeBody is the plaintext of every wake: the PushWakeV1 of the
// registration, which has a constant size because the id has a fixed length.
func wakeBody(registration string) ([]byte, error) {
	return json.Marshal(schema.NewPushWakeV1(registration))
}

// attempt makes one request for target and reduces its outcome to a result.
// It is where an endpoint is used, and nothing it returns or logs contains
// one: the request's error is turned into a class by Classify and dropped.
func (s *Sender) attempt(ctx context.Context, t Target) result {
	// The port rule again, though the registration passed it, so that a
	// rule that moves is applied to what is stored. The address rule is the
	// dialer's, on every connection.
	if !EndpointPortAllowed(t.Endpoint, s.cfg.AllowPrivate) {
		return result{class: ClassAddressRefused}
	}
	aud, ok := Origin(t.Endpoint)
	if !ok {
		return result{class: ClassRequestFailed}
	}
	p256dh, err1 := base64.RawURLEncoding.DecodeString(t.P256DH)
	auth, err2 := base64.RawURLEncoding.DecodeString(t.Auth)
	if err1 != nil || err2 != nil {
		return result{class: ClassRequestFailed}
	}
	plain, err := wakeBody(t.RegistrationID)
	if err != nil {
		return result{class: ClassRequestFailed}
	}
	body, err := Encrypt(plain, p256dh, auth)
	if err != nil {
		return result{class: ClassRequestFailed}
	}
	jwt, err := SignVAPID(s.key, aud, s.cfg.Subject, s.cfg.Now().Add(JWTLifetime))
	if err != nil {
		return result{class: ClassRequestFailed}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.Endpoint, bytes.NewReader(body))
	if err != nil {
		return result{class: ClassRequestFailed}
	}
	req.Header.Set("Authorization", AuthorizationHeader(jwt, s.pubKey))
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", s.ttl)
	req.Header.Set("Topic", "wake")
	req.Header.Set("Urgency", "normal")
	// No User-Agent: the empty value stops net/http from adding its own.
	req.Header.Set("User-Agent", "")

	resp, err := s.cfg.Client.Do(req)
	if err != nil {
		// err is a *url.Error quoting the endpoint: only its class leaves.
		return result{class: Classify(err, nil)}
	}
	// The status is all that is read: the body is closed unread.
	defer func() { _ = resp.Body.Close() }()
	return result{
		status:     resp.StatusCode,
		class:      Classify(nil, resp),
		retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), s.cfg.Now()),
	}
}

// parseRetryAfter reads a Retry-After header (RFC 9110 section 10.2.3): a
// number of seconds or an HTTP date. It is zero for a header that is absent,
// malformed or in the past, and is capped at a day, so no value can overflow.
func parseRetryAfter(v string, now time.Time) time.Duration {
	if v == "" {
		return 0
	}
	const limit = 24 * time.Hour
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		if secs > int64(limit/time.Second) {
			return limit
		}
		return time.Duration(secs) * time.Second
	}
	if at, err := http.ParseTime(v); err == nil {
		d := at.Sub(now)
		if d <= 0 {
			return 0
		}
		return min(d, limit)
	}
	return 0
}

// warnLimiter lets a warning through at most once per warnEvery, and counts
// what it suppressed.
type warnLimiter struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int
}

// allow reports whether to log now, and how many occurrences since the last
// time were not logged.
func (w *warnLimiter) allow(now time.Time) (suppressed int, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.last.IsZero() && now.Sub(w.last) < warnEvery {
		w.suppressed++
		return 0, false
	}
	w.last = now
	suppressed, w.suppressed = w.suppressed, 0
	return suppressed, true
}
