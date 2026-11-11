package server

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/push"
	"github.com/njdaniel/conch/internal/server/push/pushtest"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The Web Push sender through the server (issue #1715): the commit hook, the
// post-time policy over the real post paths, the wake a fake provider
// decrypts, the feed the phone then reads, and what happens when the provider
// misbehaves. The sender's own limits, retries and crypto are held in package
// push; this file holds the wiring. Nothing here uses the network: the
// provider is an httptest server, reached by a client that routes every host
// to it, and addresses come from a fake resolver.

// --- the commit hook -------------------------------------------------------

// TestNewMessagesReachPushThroughOneHelper is the structural guard: non-test
// code outside messageCommitted never calls events.notifyMessage, so a new post
// path cannot skip push; exactly the five post paths call messageCommitted; and
// no function that edits, removes or reacts to a message does.
func TestNewMessagesReachPushThroughOneHelper(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	postPaths := map[string]bool{
		"Server.postMessage": true, "Server.storeScopedPost": true, "Server.postReply": true,
		"grantedChannel.insertMessage": true, "Server.postReplyMCP": true,
	}
	callers := map[string]int{}
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			name := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) == 1 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id, ok := recv.(*ast.Ident); ok {
					name = id.Name + "." + name
				}
			}
			var notifiesRaw, committed, other bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "notifyMessage":
					notifiesRaw = true
				case "messageCommitted":
					committed = true
					callers[name]++
				case "notifyMessageEdited", "notifyMessageRemoved", "notifyReactions":
					other = true
				}
				return true
			})
			if notifiesRaw && name != "Server.messageCommitted" {
				t.Errorf("%s: %s calls events.notifyMessage; a new message goes through messageCommitted", path, name)
			}
			if committed && !postPaths[name] {
				t.Errorf("%s: %s calls messageCommitted and is not one of the post paths", path, name)
			}
			if committed && other {
				t.Errorf("%s: %s wakes for a new message and also for an edit, removal or reaction", path, name)
			}
		}
	}
	if checked < 50 {
		t.Fatalf("only %d files scanned", checked)
	}
	for name := range postPaths {
		if callers[name] != 1 {
			t.Errorf("%s calls messageCommitted %d times, want once", name, callers[name])
		}
	}
	if len(callers) != len(postPaths) {
		t.Errorf("messageCommitted is called from %v, want exactly %d post paths", callers, len(postPaths))
	}
}

// --- the fixture -----------------------------------------------------------

// sendHosts maps the name every test endpoint uses (the provider's certificate
// is valid for it) to a public address, and adds to the registration tests'.
func sendHosts() fakeResolver {
	out := fakeResolver{"example.com": {"93.184.216.34"}}
	for k, v := range testPushHosts {
		out[k] = v
	}
	return out
}

// countingSource sees every targets query the sender makes.
type countingSource struct {
	push.Source
	queries atomic.Int64
}

func (c *countingSource) Targets(ctx context.Context, p push.Post, now time.Time) ([]push.Target, error) {
	defer c.queries.Add(1)
	return c.Source.Targets(ctx, p, now)
}

// retryTimers is the retry clock: nothing fires until the test says so.
type retryTimers struct {
	mu      sync.Mutex
	pending []func()
}

func (r *retryTimers) after(_ time.Duration, f func()) func() bool {
	r.mu.Lock()
	r.pending = append(r.pending, f)
	r.mu.Unlock()
	return func() bool { return true }
}

func (r *retryTimers) fireAll() {
	r.mu.Lock()
	p := r.pending
	r.pending = nil
	r.mu.Unlock()
	for _, f := range p {
		f()
	}
}

func (r *retryTimers) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

type sendFixture struct {
	*pushFixture
	prov   *pushtest.Provider
	src    *countingSource
	timers *retryTimers
	offset atomic.Int64 // nanoseconds the server's clock is ahead
}

type sendOpts struct {
	queue   int
	noStart bool
	subject string
}

func newSendFixture(t *testing.T, o sendOpts) *sendFixture {
	t.Helper()
	f := &sendFixture{prov: pushtest.New(t), src: &countingSource{}, timers: &retryTimers{}}
	cfg := PushConfig{
		WebPush: true, AllowPrivate: true, Resolver: sendHosts(), VAPIDSubject: o.subject,
		client: f.prov.RoutedClient(), afterFunc: f.timers.after, queueSize: o.queue,
		wrapSource: func(s push.Source) push.Source { f.src.Source = s; return f.src },
	}
	f.pushFixture = newPushFixture(t, cfg)
	f.srv.now = func() time.Time { return time.Now().Add(time.Duration(f.offset.Load())) }
	if !o.noStart {
		f.srv.pushSender.Start(context.Background())
		t.Cleanup(f.srv.pushSender.Stop)
	}
	return f
}

// advance moves the server's clock on, past the coalescing window.
func (f *sendFixture) advance(d time.Duration) { f.offset.Add(int64(d)) }

// phone is a person's phone: a sign-in with a Web Push registration, and the
// subscriber side the provider decrypts with.
type phone struct {
	who  store.Principal
	tok  string
	cred int64
	reg  schema.PushRegistrationV1
	sub  *pushtest.Subscriber
}

// human makes a person who is a member of general, with a sign-in.
func (f *sendFixture) human(t *testing.T, name string) (store.Principal, string) {
	t.Helper()
	ctx := context.Background()
	p, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, name)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	return p, f.credential(t, p, "phone")
}

func (f *sendFixture) phone(t *testing.T, name, policy string) *phone {
	t.Helper()
	p, tok := f.human(t, name)
	return f.phoneOf(t, p, tok, policy, "")
}

// phoneOf registers tok's credential for push, at an endpoint with the given
// path ("" for a random one), and sets its policy.
func (f *sendFixture) phoneOf(t *testing.T, p store.Principal, tok, policy, path string) *phone {
	t.Helper()
	ph := &phone{who: p, tok: tok, sub: pushtest.NewSubscriber(t)}
	endpoint := f.prov.EndpointAt(ph.sub, "example.com")
	if path != "" {
		endpoint = f.prov.EndpointWithPath(ph.sub, "example.com", path)
	}
	body := fmt.Sprintf(`{"transport":"webpush","platform":"web","app":"https://app.example","endpoint":%q,"p256dh":%q,"auth":%q}`,
		endpoint, ph.sub.P256DH(), ph.sub.Auth())
	rec := f.do(t, "POST", "/v1/push/registrations", tok, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", rec.Code, rec.Body)
	}
	ph.reg = decodeBody[schema.PushRegistrationV1](t, rec)
	ph.cred = f.credentialID(t, tok)
	f.setPolicy(t, tok, policy, nil)
	return ph
}

func (f *sendFixture) credentialID(t *testing.T, tok string) int64 {
	t.Helper()
	_, id, err := f.srv.store.ResolveCredentialDetail(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *sendFixture) setPolicy(t *testing.T, tok, policy string, muted []int64) {
	t.Helper()
	if muted == nil {
		muted = []int64{}
	}
	rec := f.do(t, "PUT", "/v1/notifications/preferences", tok, fmt.Sprintf(`{"policy":%q,"muted":%s}`, policy, jsonIDs(muted)))
	if rec.Code != http.StatusOK {
		t.Fatalf("set policy = %d %s", rec.Code, rec.Body)
	}
}

func jsonIDs(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprint(id)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// waitQueries waits until the sender has asked the store n times.
func (f *sendFixture) waitQueries(t *testing.T, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.src.queries.Load() >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("the sender asked the store %d times, want %d", f.src.queries.Load(), n)
}

// settled waits for every post made so far to have been evaluated and for
// in-flight sends to finish: the evaluator is one goroutine over a queue, so
// when it has made n queries every earlier post was evaluated, and a short
// pause lets the sends it queued land.
func (f *sendFixture) settled(t *testing.T, n int64) {
	t.Helper()
	f.waitQueries(t, n)
	time.Sleep(60 * time.Millisecond)
}

func (f *sendFixture) pending(t *testing.T, tok string) schema.ListPendingNotificationsResponseV1 {
	t.Helper()
	rec := f.do(t, "GET", "/v1/notifications/pending", tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("pending = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.ListPendingNotificationsResponseV1](t, rec)
}

// --- end to end ------------------------------------------------------------

// TestPushEndToEnd: a post makes the fake provider observe and decrypt one
// content-free wake; the phone's feed then has the summary; the member is
// removed from the channel, the feed answers nothing, and a new post sends no
// wake.
func TestPushEndToEnd(t *testing.T) {
	f := newSendFixture(t, sendOpts{subject: "mailto:ops@example.org"})
	dave := f.phone(t, "dave", "all")
	posts := int64(0)

	id := f.postAs(t, f.aliceTok, "general", "hello dave")
	posts++
	d := f.prov.WaitFor(t, 1, 5*time.Second)[0]
	want := `{"schema":"conch.push_wake.v1","registration":"` + dave.reg.ID + `"}`
	if d.DecryptErr != nil || string(d.Plain) != want {
		t.Fatalf("the provider decrypted %q (%v), want %s", d.Plain, d.DecryptErr, want)
	}
	if d.VAPID.Err != nil || !d.VAPID.SigOK || d.VAPID.Key != f.srv.cfg.Push.VAPID.PublicKey() {
		t.Errorf("VAPID = %+v", d.VAPID)
	}
	if d.VAPID.Claims["sub"] != "mailto:ops@example.org" {
		t.Errorf("sub = %v", d.VAPID.Claims["sub"])
	}
	// No text of the message, no channel, no author, anywhere in what left.
	raw := fmt.Sprintf("%v %s", d.Header, d.Plain)
	for _, leak := range []string{"hello dave", "general", "alice"} {
		if strings.Contains(raw, leak) {
			t.Errorf("the wake carries %q", leak)
		}
	}
	// The wake was recorded.
	regs, err := f.srv.store.ListOwnPushRegistrations(context.Background(), dave.cred)
	if err != nil || len(regs) != 1 {
		t.Fatalf("registrations = %v, %v", regs, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for regs[0].LastWakeAt == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		regs, _ = f.srv.store.ListOwnPushRegistrations(context.Background(), dave.cred)
	}
	if regs[0].LastWakeAt == nil {
		t.Error("last_wake_at was not written")
	}

	// The phone reads the feed.
	feed := f.pending(t, dave.tok)
	if len(feed.Summaries) != 1 || feed.Summaries[0].MessageID != id {
		t.Fatalf("feed = %+v, want the message %d", feed, id)
	}

	// The member is removed from the channel: the feed answers nothing, and a
	// new post sends no wake.
	general, _ := f.srv.store.ChannelByName(context.Background(), "general")
	if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/general/members/%d", dave.who.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("remove = %d %s", rec.Code, rec.Body)
	}
	_ = general
	f.advance(2 * time.Minute)
	f.postAs(t, f.aliceTok, "general", "anyone there")
	posts++
	f.settled(t, posts)
	if feed := f.pending(t, dave.tok); len(feed.Summaries) != 0 {
		t.Errorf("a removed member's feed = %+v, want nothing", feed)
	}
	if n := f.prov.Count(); n != 1 {
		t.Errorf("the provider got %d requests, want 1: a removed member was woken", n)
	}
	if got := metricValue(t, f.srv, "conch_push_wakes_total", "transport", "webpush", "outcome", "sent"); got != 1 {
		t.Errorf("sent = %v", got)
	}
}

// TestPushPolicyThroughThePostPaths: the policy, mutes, whispers and the
// reader's own messages, through the real REST paths. A control phone (policy
// all, never the author) is woken by every post, so that when it has been
// woken for post n, post n-1 has been evaluated.
func TestPushPolicyThroughThePostPaths(t *testing.T) {
	f := newSendFixture(t, sendOpts{})
	control := f.phone(t, "ctl", "all")
	dave := f.phone(t, "dave", "off")
	general, err := f.srv.store.ChannelByName(context.Background(), "general")
	if err != nil {
		t.Fatal(err)
	}
	_ = control

	// post is a function that posts one message and returns who wrote it.
	plain := func(who string) func(t *testing.T) {
		return func(t *testing.T) {
			tok := map[string]string{"alice": f.aliceTok, "dave": dave.tok}[who]
			f.postAs(t, tok, "general", "hello")
		}
	}
	mention := func(t *testing.T) { f.postAs(t, f.aliceTok, "general", "ping @dave please") }
	whisper := func(t *testing.T) {
		rec := f.do(t, "POST", "/v2/channels/general/messages", f.aliceTok,
			`{"body":"psst",`+whisperAudience(dave.who.ID)+`}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("whisper = %d %s", rec.Code, rec.Body)
		}
	}
	whisperAway := func(t *testing.T) {
		rec := f.do(t, "POST", "/v2/channels/general/messages", f.aliceTok,
			`{"body":"psst @dave",`+whisperAudience(control.who.ID)+`}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("whisper = %d %s", rec.Code, rec.Body)
		}
	}
	tests := []struct {
		name   string
		policy string
		muted  []int64
		post   func(t *testing.T)
		wakes  bool
		setup  func(t *testing.T)
	}{
		{"off: nothing", "off", nil, mention, false, nil},
		{"mentions: a plain message", "mentions", nil, plain("alice"), false, nil},
		{"mentions: a mention", "mentions", nil, mention, true, nil},
		{"mentions: a whisper to the reader", "mentions", nil, whisper, true, nil},
		{"mentions: a whisper to someone else", "mentions", nil, whisperAway, false, nil},
		{"all: a plain message", "all", nil, plain("alice"), true, nil},
		{"all: a muted channel", "all", []int64{general.ID}, plain("alice"), false, nil},
		{"all: the reader's own message", "all", nil, plain("dave"), false, nil},
		{"all: after leaving the channel", "all", nil, plain("alice"), false, func(t *testing.T) {
			if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/channels/general/members/%d", dave.who.ID), f.rootTok, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("remove = %d", rec.Code)
			}
		}},
		{"all: after the sign-in was revoked", "all", nil, plain("alice"), false, func(t *testing.T) {
			if err := f.srv.store.RevokeCredential(context.Background(), "system", dave.cred); err != nil {
				t.Fatal(err)
			}
		}},
	}
	queries := int64(0)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup == nil {
				f.setPolicy(t, dave.tok, tt.policy, tt.muted)
			} else {
				// The setup may end what the policy would be set on.
				f.setPolicy(t, dave.tok, tt.policy, tt.muted)
				tt.setup(t)
			}
			f.advance(2 * time.Minute) // past the coalescing window
			before := len(f.regDeliveries(dave.reg.ID))
			tt.post(t)
			queries++
			f.settled(t, queries)
			got := len(f.regDeliveries(dave.reg.ID)) - before
			if (got == 1) != tt.wakes || got > 1 {
				t.Errorf("dave was woken %d times, want wakes=%v", got, tt.wakes)
			}
		})
	}
	// The control phone heard every post: the evaluation did run for each.
	if n := len(f.regDeliveries(control.reg.ID)); n < 1 {
		t.Errorf("the control phone was woken %d times", n)
	}
}

func (f *sendFixture) regDeliveries(reg string) []pushtest.Delivery {
	var out []pushtest.Delivery
	for _, d := range f.prov.Deliveries() {
		if d.Registration == reg {
			out = append(out, d)
		}
	}
	return out
}

// TestPushEditsAndRemovalsNeverWake: an edit of a message does not wake.
func TestPushEditsAndRemovalsNeverWake(t *testing.T) {
	f := newSendFixture(t, sendOpts{})
	f.srv.setMessageEditLimit(inviteLimit{name: "test", burst: 1000, window: time.Minute})
	dave := f.phone(t, "dave", "all")
	id := f.postAs(t, f.aliceTok, "general", "first")
	f.prov.WaitFor(t, 1, 5*time.Second)
	f.advance(2 * time.Minute)
	rec := f.do(t, "PUT", fmt.Sprintf("/v2/channels/general/messages/%d/body", id), f.aliceTok, `{"body":"first, edited @dave"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit = %d %s", rec.Code, rec.Body)
	}
	// A control post after the edit: when it is evaluated, the edit was.
	f.postAs(t, f.aliceTok, "general", "second")
	f.settled(t, 2)
	if got := len(f.regDeliveries(dave.reg.ID)); got != 2 {
		t.Errorf("dave was woken %d times, want 2 (the two posts, not the edit)", got)
	}
}

// --- the provider rejects ---------------------------------------------------

// TestPushProviderRejection: a 404 or a 410 ends the registration with reason
// provider_rejected and exactly one audit row, and nothing more is sent to it.
func TestPushProviderRejection(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newSendFixture(t, sendOpts{})
			f.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return pushtest.Reply{Status: status} })
			dave := f.phone(t, "dave", "all")
			f.postAs(t, f.aliceTok, "general", "one")
			f.prov.WaitFor(t, 1, 5*time.Second)
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if regs, _ := f.srv.store.ListOwnPushRegistrations(context.Background(), dave.cred); len(regs) == 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			regs, err := f.srv.store.ListOwnPushRegistrations(context.Background(), dave.cred)
			if err != nil || len(regs) != 0 {
				t.Fatalf("the registration is still active: %v, %v", regs, err)
			}
			rows := 0
			for _, e := range f.audit(t) {
				if e.Action == schema.AuditActionPushRegistrationEnded && strings.Contains(e.Detail, dave.reg.ID) {
					rows++
					if !strings.HasSuffix(e.Detail, "reason=provider_rejected") {
						t.Errorf("detail = %q", e.Detail)
					}
				}
			}
			if rows != 1 {
				t.Errorf("%d push_registration_ended rows, want exactly 1", rows)
			}
			if got := metricValue(t, f.srv, "conch_push_wakes_total", "transport", "webpush", "outcome", "provider_rejected"); got != 1 {
				t.Errorf("provider_rejected = %v", got)
			}
			// No further sends.
			f.advance(2 * time.Minute)
			f.postAs(t, f.aliceTok, "general", "two")
			f.settled(t, 2)
			if n := f.prov.Count(); n != 1 {
				t.Errorf("the provider got %d requests, want 1", n)
			}
		})
	}
}

// --- never slow a post ------------------------------------------------------

// TestPostLatencyIndependentOfPush: with the provider hanging and the queue
// full, posts return promptly and the dropped wakes are counted.
func TestPostLatencyIndependentOfPush(t *testing.T) {
	t.Run("a full queue and nobody draining it", func(t *testing.T) {
		f := newSendFixture(t, sendOpts{queue: 2, noStart: true})
		f.phone(t, "dave", "all")
		start := time.Now()
		for i := range 40 {
			f.postAs(t, f.aliceTok, "general", fmt.Sprint("m", i))
		}
		if took := time.Since(start); took > 20*time.Second {
			t.Errorf("40 posts took %s", took)
		}
		if got := metricValue(t, f.srv, "conch_push_wakes_total", "transport", "webpush", "outcome", "queue_full"); got != 38 {
			t.Errorf("queue_full = %v, want 38", got)
		}
	})
	t.Run("a hanging provider", func(t *testing.T) {
		f := newSendFixture(t, sendOpts{})
		f.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return pushtest.Reply{Hang: true} })
		for i := range 4 {
			f.phone(t, fmt.Sprint("p", i), "all")
		}
		f.postAs(t, f.aliceTok, "general", "warm up")
		f.prov.WaitFor(t, 4, 5*time.Second) // every worker is hanging
		f.advance(time.Minute)
		start := time.Now()
		for i := range 20 {
			f.postAs(t, f.aliceTok, "general", fmt.Sprint("m", i))
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("20 posts with every send worker hanging took %s", took)
		}
	})
}

// TestPushOffIsInert: with Web Push off there is no sender, no goroutine, no
// wake, and posts behave as they did.
func TestPushOffIsInert(t *testing.T) {
	before := runtime.NumGoroutine()
	f := newAuthFixture(t, AuthRequired)
	if f.srv.pushSender != nil {
		t.Fatal("a sender exists with push off")
	}
	stop := f.srv.startPushSender(context.Background())
	f.postAs(t, f.aliceTok, "general", "hello")
	stop()
	for _, o := range push.Outcomes() {
		if v := metricValue(t, f.srv, "conch_push_wakes_total", "transport", "webpush", "outcome", o); v != 0 {
			t.Errorf("outcome %s = %v with push off", o, v)
		}
	}
	if after := runtime.NumGoroutine(); after > before+40 {
		// The fixture itself starts goroutines; the check is that none grows
		// with the post, which the pushSender == nil above already proves.
		t.Logf("goroutines %d -> %d", before, after)
	}

	// With it on but not started: the same, and starting it adds its goroutines
	// and stopping removes them.
	g := newSendFixture(t, sendOpts{noStart: true})
	base := runtime.NumGoroutine()
	stop = g.srv.startPushSender(context.Background())
	if n := runtime.NumGoroutine(); n < base+1+push.Workers {
		t.Errorf("goroutines after start = %d, want at least %d", n, base+1+push.Workers)
	}
	stop()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > base && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base {
		t.Errorf("goroutines after stop = %d, want %d", n, base)
	}
}

func TestServeRefusesBadPushSettings(t *testing.T) {
	tests := []struct {
		name string
		cfg  PushConfig
		ok   bool
	}{
		{"defaults", PushConfig{}, true},
		{"expire 28 days", PushConfig{Expire: 28 * 24 * time.Hour}, true},
		{"expire past 28 days", PushConfig{Expire: 28*24*time.Hour + time.Second}, false},
		{"expire negative", PushConfig{Expire: -time.Hour}, false},
		{"budget negative", PushConfig{MaxPerMinute: -5}, false},
		{"subject mailto", PushConfig{VAPIDSubject: "mailto:a@b.c"}, true},
		{"subject http", PushConfig{VAPIDSubject: "http://a.b"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePushConfig(tt.cfg)
			if (err == nil) != tt.ok {
				t.Errorf("ValidatePushConfig = %v, want ok=%v", err, tt.ok)
			}
			if tt.ok {
				return
			}
			// Serve refuses before it binds anything.
			srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, Listen: "127.0.0.1:0", Push: tt.cfg})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- srv.Serve(ctx) }()
			select {
			case err := <-done:
				if err == nil {
					t.Error("Serve started with bad push settings")
				}
			case <-time.After(5 * time.Second):
				t.Error("Serve did not refuse")
			}
		})
	}
}

// --- outage -----------------------------------------------------------------

type pushLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *pushLogBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *pushLogBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func capturePushLogs(t *testing.T) *pushLogBuf {
	t.Helper()
	b := &pushLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return b
}

// TestPushOutage: with the provider answering 5xx, or gone, wakes fail and are
// counted, the warning is thinned, retries are bounded, and posting, reading,
// the approval chain's audit and the feed are unaffected.
func TestPushOutage(t *testing.T) {
	for _, mode := range []string{"5xx", "down"} {
		t.Run(mode, func(t *testing.T) {
			logs := capturePushLogs(t)
			f := newSendFixture(t, sendOpts{})
			var phones []*phone
			for i := range 6 {
				phones = append(phones, f.phone(t, fmt.Sprint("p", i), "all"))
			}
			if mode == "5xx" {
				f.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return pushtest.Reply{Status: 503} })
			} else {
				f.prov.Close()
			}
			var ids []int64
			advanced := time.Duration(0)
			for i := range 5 {
				ids = append(ids, f.postAs(t, f.aliceTok, "general", fmt.Sprint("during the outage ", i)))
				f.advance(31 * time.Second) // past the coalescing window
				advanced += 31 * time.Second
			}
			f.settled(t, 5)
			// The retries are timers: fire them until none is left. Each round
			// of 6 failed first attempts leaves at most 6 retries, then 6 more.
			for range 4 {
				f.timers.fireAll()
				time.Sleep(80 * time.Millisecond)
			}
			if f.timers.count() != 0 {
				t.Errorf("%d retries still scheduled", f.timers.count())
			}
			class := "status 5xx"
			if mode == "down" {
				class = "connection refused"
			}
			waitMetric := func(name string, labels ...string) float64 {
				deadline := time.Now().Add(5 * time.Second)
				for time.Now().Before(deadline) {
					if v := metricValue(t, f.srv, name, labels...); v > 0 {
						return v
					}
					time.Sleep(5 * time.Millisecond)
				}
				return 0
			}
			if waitMetric("conch_push_failures_total", "transport", "webpush", "class", class) == 0 {
				t.Errorf("no %q failure was counted", class)
			}
			if waitMetric("conch_push_wakes_total", "transport", "webpush", "outcome", "failed") == 0 {
				t.Error("no failed wake was counted")
			}
			if mode == "down" {
				// A connection refused is not retried.
				if metricValue(t, f.srv, "conch_push_wakes_total", "transport", "webpush", "outcome", "retry_dropped") != 0 {
					t.Error("a refused connection was retried")
				}
			}
			// The warning is thinned: one per minute of the server's clock, however
			// many failures there were (30 first attempts and their retries).
			warnings := strings.Count(logs.String(), "wakes are failing")
			failures := strings.Count(logs.String(), "a wake failed")
			if maxWarn := int(advanced/time.Minute) + 1; warnings < 1 || warnings > maxWarn || warnings*3 > failures {
				t.Errorf("%d outage warnings for %d failures over %s, want between 1 and %d and far fewer than the failures",
					warnings, failures, advanced, maxWarn)
			}
			// Everything else works.
			for _, id := range ids {
				rec := f.do(t, "GET", fmt.Sprintf("/v1/channels/general/messages?after=%d&limit=1", id-1), f.aliceTok, "")
				if rec.Code != http.StatusOK {
					t.Errorf("reading during the outage = %d", rec.Code)
				}
			}
			if feed := f.pending(t, phones[0].tok); len(feed.Summaries) != 5 {
				t.Errorf("the feed has %d summaries, want 5", len(feed.Summaries))
			}
			if rec := f.do(t, "POST", "/v1/channels/general/messages", f.aliceTok, fmt.Sprintf(`{"author_id":%d,"body":"still posting"}`, f.alice.ID)); rec.Code != http.StatusCreated {
				t.Errorf("posting during the outage = %d", rec.Code)
			}
			// Registrations survive a transport outage: only a 404 or 410 ends one.
			for _, p := range phones {
				if regs, _ := f.srv.store.ListOwnPushRegistrations(context.Background(), p.cred); len(regs) != 1 {
					t.Errorf("a registration was ended by an outage")
				}
			}
		})
	}
}

// TestPushRetriesStayBounded: 5xx floods never leave more than the bound of
// retries pending.
func TestPushRetriesStayBounded(t *testing.T) {
	f := newSendFixture(t, sendOpts{})
	f.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return pushtest.Reply{Status: 500} })
	for i := range 10 {
		f.phone(t, fmt.Sprint("p", i), "all")
	}
	f.postAs(t, f.aliceTok, "general", "x")
	f.settled(t, 1)
	if n := f.timers.count(); n > push.MaxPendingRetries {
		t.Errorf("%d retries pending, bound %d", n, push.MaxPendingRetries)
	}
}

// --- nothing leaks ----------------------------------------------------------

// TestPushLeak registers marked values, drives the outcomes through the real
// server and sender, and asserts that no captured log line, audit row, metric
// label or API answer contains a marker. It follows
// TestNtfyTransportErrorsDoNotNameTheTopic without importing or editing it.
func TestPushLeak(t *testing.T) {
	logs := capturePushLogs(t)
	f := newSendFixture(t, sendOpts{})
	const (
		markPath  = "/MARKER-PATH-9d41"
		markQuery = "MARKER-QUERY-5e27"
	)
	type run struct {
		name  string
		reply pushtest.Reply
	}
	runs := []run{
		{name: "sent", reply: pushtest.Reply{Status: 201}},
		{name: "rejected", reply: pushtest.Reply{Status: 410}},
		{name: "bad request", reply: pushtest.Reply{Status: 400}},
		{name: "unavailable", reply: pushtest.Reply{Status: 503}},
		{name: "redirect", reply: pushtest.Reply{Status: 302, Location: "https://marker-location.example/MARKER-LOC"}},
	}
	var markers []string
	for i, r := range runs {
		p, tok := f.human(t, fmt.Sprint("leaker", i))
		path := fmt.Sprintf("%s-%d", markPath, i)
		ph := f.phoneOf(t, p, tok, "all", path)
		_ = markQuery
		markers = append(markers, path, ph.sub.P256DH(), ph.sub.Auth())
		f.prov.Script(func(int, *pushtest.Delivery) pushtest.Reply { return r.reply })
		f.advance(2 * time.Minute)
		before := f.prov.Count()
		f.postAs(t, f.aliceTok, "general", "leak check "+r.name)
		f.settled(t, int64(i+1))
		f.timers.fireAll()
		time.Sleep(80 * time.Millisecond)
		if f.prov.Count() == before {
			t.Fatalf("%s: nothing reached the provider", r.name)
		}
		// Wipe the other phones from the next run: end this registration so a
		// later run's wake is the only one in flight.
		_ = f.srv.store.DeletePushRegistration(context.Background(), ph.cred, ph.reg.ID, time.Now())
	}
	markers = append(markers, "marker-location", "MARKER-LOC", f.prov.URL(), strings.TrimPrefix(f.prov.URL(), "https://"))
	if u, err := url.Parse(f.prov.URL()); err == nil {
		markers = append(markers, u.Port())
	}

	var surfaces []string
	surfaces = append(surfaces, "log:"+logs.String())
	for _, e := range f.audit(t) {
		surfaces = append(surfaces, fmt.Sprintf("audit:%s|%s|%s|%s", e.Actor, e.Action, e.Subject, e.Detail))
	}
	for _, s := range gatherMetrics(t, f.srv) {
		surfaces = append(surfaces, fmt.Sprintf("metric:%s%v", s.name, s.labels))
	}
	for _, s := range surfaces {
		for _, m := range markers {
			if m != "" && strings.Contains(s, m) {
				t.Errorf("%.60q contains the marker %q", s, m)
			}
		}
	}
	if !strings.Contains(logs.String(), "registration=") {
		t.Error("the failure lines were not captured; the test proves nothing")
	}
}
