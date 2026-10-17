package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// ---- the load test's Go half (issue #448)
//
// scripts/alpha-loadtest.sh measures one screen share watched by many viewers
// on a deployment. `lk load-test` makes the viewers, but it signs each one's
// join token itself, from a LiveKit API key and secret, and has no option to
// use a token it is given. Run that way the test would bypass conchd and need
// the deployment's LiveKit secret on the load generator. So the script runs
// lk load-test through a relay, as this program's checks run `lk room join`
// (livekit.go): every connection lk opens is admitted on a token conchd issued
// to a principal of its own, asked for over the public API at that moment.
//
// It is here, as subcommands of the voice check, because it reuses that
// check's pinned lk download (fetchLK), its relay and its screen-share filter.
// Everything it says to conchd is a pkg/schema type.
//
//	voice-check fetch-lk DIR   download the pinned lk into DIR, print its path
//	voice-check loadtest ...   make the viewers, relay until a signal, hide them
//
// The operator credential comes in the environment (loadtestTokenEnv), never
// on a command line, and is removed from the environment before anything else
// runs, so no child process inherits it.

// loadtestTokenEnv names the variable the script passes the operator
// credential in.
const loadtestTokenEnv = "CONCH_LOADTEST_TOKEN" // #nosec G101 -- the name of a variable, not a credential

// loadtestPrefixRE is the shape of a run's name prefix. It must begin with
// "loadtest-" so that the clean-up, which hides by name prefix, cannot select
// a real person's account however the prefix is mistyped.
var loadtestPrefixRE = regexp.MustCompile(`^loadtest-[a-z0-9][a-z0-9-]{0,39}$`)

// maxLoadtestViewers bounds one run: one bulk hide's worth.
const maxLoadtestViewers = schema.MaxHidePrincipals - 1

// loadtestConfig is what the script passes the loadtest subcommand.
type loadtestConfig struct {
	server      string // conchd's base URL, without a trailing slash
	channel     string
	viewers     int
	prefix      string
	fakeSharer  bool
	lkBin       string // the pinned lk, for the fake sharer
	ready       string // file the relay's URL is written to once it is up
	cleanupOnly bool
	lifetime    time.Duration // how long the viewers' credentials live
}

// parseLoadtestFlags reads the loadtest subcommand's arguments.
func parseLoadtestFlags(args []string) (loadtestConfig, error) {
	var c loadtestConfig
	fs := flag.NewFlagSet("loadtest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&c.server, "server", "", "conchd's base URL")
	fs.StringVar(&c.channel, "channel", "", "the channel whose voice room is loaded")
	fs.IntVar(&c.viewers, "viewers", 0, "how many principals to make for lk load-test's participants")
	fs.StringVar(&c.prefix, "prefix", "", "the name prefix of every principal made (loadtest-...)")
	fs.BoolVar(&c.fakeSharer, "fake-sharer", false, "also share lk's demo video as a screen, with sound")
	fs.StringVar(&c.lkBin, "lk", "", "the pinned lk binary (for --fake-sharer)")
	fs.StringVar(&c.ready, "ready", "", "file to write the relay's URL to once it is up")
	fs.BoolVar(&c.cleanupOnly, "cleanup-only", false, "hide what an earlier run with this prefix left, and stop")
	fs.DurationVar(&c.lifetime, "credential-lifetime", 4*time.Hour, "how long the viewers' credentials live")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	u, err := url.Parse(c.server)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("-server must be conchd's http:// or https:// base URL")
	}
	c.server = strings.TrimSuffix(c.server, "/")
	if !loadtestPrefixRE.MatchString(c.prefix) {
		return c, fmt.Errorf("-prefix must match %s", loadtestPrefixRE)
	}
	if c.cleanupOnly {
		return c, nil
	}
	if c.channel == "" {
		return c, errors.New("-channel is required")
	}
	if c.viewers < 1 || c.viewers > maxLoadtestViewers {
		return c, fmt.Errorf("-viewers must be 1 to %d", maxLoadtestViewers)
	}
	if c.fakeSharer && c.lkBin == "" {
		return c, errors.New("-fake-sharer needs -lk")
	}
	if c.ready == "" {
		return c, errors.New("-ready is required")
	}
	if c.lifetime < time.Minute {
		return c, errors.New("-credential-lifetime must be at least a minute")
	}
	return c, nil
}

// fetchLKMain is `voice-check fetch-lk DIR`: the pinned lk, checked against
// its SHA-256, extracted into DIR. It prints the binary's path.
func fetchLKMain(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: voice-check fetch-lk DIR")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	path, err := fetchLK(ctx, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch-lk:", err)
		return 1
	}
	fmt.Println(path)
	return 0
}

// loadtestMain is `voice-check loadtest`. It exits 0 when the run and the
// clean-up both succeeded.
func loadtestMain(args []string) int {
	token := os.Getenv(loadtestTokenEnv)
	_ = os.Unsetenv(loadtestTokenEnv)
	cfg, err := parseLoadtestFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		return 2
	}
	if token == "" {
		fmt.Fprintf(os.Stderr, "loadtest: the operator credential must be in %s\n", loadtestTokenEnv)
		return 2
	}
	tmp, err := os.MkdirTemp("", "conch-loadtest-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		return 1
	}
	h := &harness{tmp: tmp, secrets: map[string]string{}, rooms: map[string][]string{}, jtis: map[string]bool{}}
	h.secret("the operator credential", token)
	lt := &loadtest{h: h, cfg: cfg, op: api{baseURL: cfg.server, token: token}, byLK: map[string]*person{}}

	// SIGTERM ends the run and starts the clean-up: the script sends it once,
	// when lk load-test is done or the script is stopped. A terminal's Ctrl-C
	// reaches every process of the script and is ignored here, so that lk
	// prints its summary while the viewers are still connected, and so that
	// pressing it again cannot cut the clean-up short.
	signal.Ignore(os.Interrupt)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)
	go func() {
		<-sigs
		stopCancel()
	}()

	if cfg.cleanupOnly {
		err = lt.hideAll()
		h.teardown()
		if err != nil {
			fmt.Fprintln(os.Stderr, "loadtest: FAIL:", h.redact(err.Error()))
			return 1
		}
		return 0
	}
	runErr := lt.run(stopCtx)
	// The scan reads the captured logs, which teardown removes; what the
	// clean-up prints is counts and the prefix.
	scanErr := h.scanAll()
	h.teardown()
	if runErr == nil {
		runErr = scanErr
	}
	if err := errors.Join(runErr, lt.cleanupErr); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest: FAIL:", h.redact(err.Error()))
		return 1
	}
	return 0
}

// loadtest is one run of the loadtest subcommand.
type loadtest struct {
	h   *harness
	cfg loadtestConfig
	op  api

	mu      sync.Mutex
	viewers []*person          // in the order they were made
	byLK    map[string]*person // lk's identity for a participant -> its principal

	cleanupErr error
}

// run makes the viewers, starts the relay (and the fake sharer), says where
// it is, and waits for ctx to end. The clean-up is registered before the
// first principal is made, so it runs however far the run got.
func (lt *loadtest) run(ctx context.Context) error {
	h, cfg := lt.h, lt.cfg
	// Hiding by prefix must select only what this run makes.
	already, err := lt.matching()
	if err != nil {
		return err
	}
	if already > 0 {
		return fmt.Errorf("%d principals already have names starting %q; choose another prefix, or hide them with --cleanup-only", already, cfg.prefix+"-")
	}
	h.onCleanup(func() { lt.cleanupErr = lt.hideAll() })

	expires := schema.NewTimestamp(time.Now().Add(cfg.lifetime))
	for i := 1; i <= cfg.viewers; i++ {
		if ctx.Err() != nil {
			return errInterrupted
		}
		v, err := lt.newMember(fmt.Sprintf("%s-v%03d", cfg.prefix, i), expires)
		if err != nil {
			return err
		}
		lt.viewers = append(lt.viewers, v)
	}
	h.say("made %d viewers named %s-v001..%s-v%03d, members of %s, credentials expiring %s",
		cfg.viewers, cfg.prefix, cfg.prefix, cfg.viewers, cfg.channel, expires.Time().UTC().Format(time.RFC3339))

	// One session up front: voice is set up and the channel is right before
	// lk starts, and it names LiveKit's address.
	first, err := lt.session(lt.viewers[0])
	if err != nil {
		return err
	}
	upstream, secure, err := livekitUpstream(first.LivekitURL)
	if err != nil {
		return err
	}
	if cfg.fakeSharer {
		if err := lt.startSharer(ctx, upstream, secure, expires); err != nil {
			return err
		}
	}
	rl, err := serveRelay(&relay{upstream: upstream, secure: secure, mint: lt.mint})
	if err != nil {
		return err
	}
	h.onCleanup(rl.Close)
	if err := os.WriteFile(cfg.ready, []byte(rl.url()+"\n"), 0o600); err != nil {
		return err
	}
	h.say("relay up at %s in front of LiveKit; each participant lk opens is admitted on a conchd token of its own", rl.url())
	<-ctx.Done()
	lt.mu.Lock()
	used := len(lt.byLK)
	lt.mu.Unlock()
	h.say("run over: %d of %d viewers connected through the relay", used, cfg.viewers)
	return nil
}

// newMember makes a human principal with a credential that expires, and adds
// it to the channel.
func (lt *loadtest) newMember(name string, expires schema.Timestamp) (*person, error) {
	var created schema.CreatePrincipalResponse
	if err := lt.op.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: name}, &created); err != nil {
		return nil, fmt.Errorf("create %s: %w", name, err)
	}
	var cred schema.CreateCredentialResponseV1
	if err := lt.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", created.Principal.ID),
		schema.CreateCredentialRequestV1{Label: name, ExpiresAt: &expires}, &cred); err != nil {
		return nil, fmt.Errorf("issue a credential for %s: %w", name, err)
	}
	lt.h.secret(name+"'s credential", cred.Token)
	if err := lt.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(lt.cfg.channel), created.Principal.ID), nil, nil); err != nil {
		return nil, fmt.Errorf("add %s to %s: %w", name, lt.cfg.channel, err)
	}
	return &person{name: name, id: created.Principal.ID, token: cred.Token, credID: cred.Credential.ID,
		api: api{baseURL: lt.cfg.server, token: cred.Token}}, nil
}

// session asks conchd for p's voice session in the channel, exactly as a
// client does, and registers its tokens as secrets.
func (lt *loadtest) session(p *person) (schema.VoiceSessionResponseV1, error) {
	var resp schema.VoiceSessionResponseV1
	status, data, err := p.api.do(http.MethodPost, "/v1/channels/"+url.PathEscape(lt.cfg.channel)+"/voice/session", nil)
	if err != nil {
		return resp, err
	}
	if status != http.StatusOK {
		var e schema.Error
		_ = json.Unmarshal(data, &e)
		return resp, fmt.Errorf("%s asked for a voice session in %s: status %d, code %q", p.name, lt.cfg.channel, status, e.Code)
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return resp, fmt.Errorf("decode the voice session: %w", err)
	}
	for _, g := range resp.Rooms {
		lt.h.secret("a voice join token", g.Token)
		lt.h.room(g.Room)
	}
	if err := resp.Validate(); err != nil {
		return resp, fmt.Errorf("the voice session does not satisfy the schema: %w", err)
	}
	return resp, nil
}

// mint is the relay's token source: the participant lk names in the token it
// signed itself gets a viewer of its own, the same one each time it connects,
// and a fresh conchd session for it.
func (lt *loadtest) mint(_ context.Context, lkToken string) (string, error) {
	id, err := lkIdentity(lkToken)
	if err != nil {
		return "", err
	}
	v, err := lt.assign(id)
	if err != nil {
		return "", err
	}
	s, err := lt.session(v)
	if err != nil {
		return "", err
	}
	// The channel's room is the whole channel's (V3): the only grant.
	return s.Rooms[0].Token, nil
}

// assign returns the viewer for lk's participant id, choosing the next unused
// one the first time id is seen.
func (lt *loadtest) assign(id string) (*person, error) {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if v, ok := lt.byLK[id]; ok {
		return v, nil
	}
	if len(lt.byLK) >= len(lt.viewers) {
		return nil, fmt.Errorf("lk started more participants than the %d viewers made", len(lt.viewers))
	}
	v := lt.viewers[len(lt.byLK)]
	lt.byLK[id] = v
	return v, nil
}

// lkIdentity reads the participant identity (the sub claim) from a token lk
// signed itself with a key LiveKit has never heard of. It is not verified:
// it only tells lk's participants apart.
func lkIdentity(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("the client presented no token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("the client's token is not a JWT")
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Sub == "" {
		return "", errors.New("the client's token names no participant")
	}
	return claims.Sub, nil
}

// livekitUpstream is where the relay reaches LiveKit, from the address conchd
// gives clients: host:port, and whether over TLS. LiveKit at a path below the
// host is not supported by the relay.
func livekitUpstream(raw string) (hostPort string, secure bool, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false, errors.New("conchd's LiveKit address is not a URL")
	}
	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		return "", false, errors.New("conchd's LiveKit address has a path; the relay supports LiveKit at the root of its host only")
	}
	switch u.Scheme {
	case "ws", "http":
		return u.Host, false, nil
	case "wss", "https":
		return u.Host, true, nil
	}
	return "", false, fmt.Errorf("conchd's LiveKit address has scheme %q, want ws or wss", u.Scheme)
}

// startSharer makes one more member, joins it with lk publishing its demo
// video and a silent sound file, claims the channel's share for it and lets
// the relay pass those tracks as the screen and its sound (share.go): a
// stand-in for a browser sharing a screen, for a rehearsal of the run or a
// local proof. The real runs share from a browser.
func (lt *loadtest) startSharer(ctx context.Context, upstream string, secure bool, expires schema.Timestamp) error {
	h := lt.h
	sharer, err := lt.newMember(lt.cfg.prefix+"-sharer", expires)
	if err != nil {
		return err
	}
	s, err := lt.session(sharer)
	if err != nil {
		return err
	}
	dir, err := h.mkdir("share-")
	if err != nil {
		return err
	}
	audio := filepath.Join(dir, "share-audio.ogg")
	if err := writeSilentOpus(audio, 3600); err != nil {
		return err
	}
	gate := newShareFilter(true)
	rl, err := serveRelay(&relay{upstream: upstream, secure: secure, token: s.Rooms[0].Token, filter: gate.filter})
	if err != nil {
		return err
	}
	hl, err := h.startLKVia(lt.cfg.lkBin, rl, "--publish-demo", "--publish", audio)
	if err != nil {
		rl.Close()
		return err
	}
	// Stopped before the viewers' relay closes and before the hide, which
	// would end the share anyway; stopping it first says so in the audit.
	h.onCleanup(func() {
		if err := sharer.api.call(http.MethodDelete, "/v1/channels/"+url.PathEscape(lt.cfg.channel)+"/voice/share", nil, nil); err != nil {
			h.say("note: stopping the fake share: %s", h.redact(err.Error()))
		}
	})
	select {
	case <-gate.held:
	case <-hl.done:
		return errors.New("the fake sharer's lk exited: " + hl.tail(h))
	case <-ctx.Done():
		return errInterrupted
	case <-time.After(60 * time.Second):
		return errors.New("the fake sharer's lk did not try to publish within 60 s: " + hl.tail(h))
	}
	before := len(rl.issuedTokens())
	share, status, code, err := claimShare(sharer, lt.cfg.channel)
	if err != nil {
		return fmt.Errorf("claim the share: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("claim the share: status %d code %q", status, code)
	}
	// LiveKit sends the connection a token carrying the share; publishing
	// before it arrives is refused.
	if err := waitFor("LiveKit to send the fake sharer a token carrying the share", 15*time.Second, func() (bool, string) {
		return len(rl.issuedTokens()) > before, "no token since the claim"
	}); err != nil {
		return err
	}
	gate.release()
	h.say("fake sharer %s is sharing lk's demo video as a screen (sound=%v)", sharer.name, share.Audio)
	return nil
}

// matching is how many principals a hide by this run's prefix would select.
func (lt *loadtest) matching() (int, error) {
	dry := true
	var resp schema.HidePrincipalsResponseV1
	if err := lt.op.call(http.MethodPost, "/v1/principals/hide", lt.hideRequest(&dry), &resp); err != nil {
		return 0, fmt.Errorf("count the principals named %s-*: %w", lt.cfg.prefix, err)
	}
	return resp.Count + resp.Remaining, nil
}

// hideRequest selects every human named with this run's prefix and a dash,
// so that loadtest-a never selects loadtest-ab's principals.
func (lt *loadtest) hideRequest(dryRun *bool) schema.HidePrincipalsRequestV1 {
	return schema.HidePrincipalsRequestV1{NamePrefix: lt.cfg.prefix + "-", Kind: schema.PrincipalHuman, DryRun: dryRun}
}

// hideAll hides every principal this run's prefix selects, with the bulk hide
// by name prefix (#445), a page at a time, and then checks none is left. A
// hidden principal is disabled, leaves every channel and gives up its name.
func (lt *loadtest) hideAll() error {
	wet := false
	hidden := 0
	for range 20 {
		var resp schema.HidePrincipalsResponseV1
		if err := lt.op.call(http.MethodPost, "/v1/principals/hide", lt.hideRequest(&wet), &resp); err != nil {
			return fmt.Errorf("hide the principals named %s-*: %w (run again with --cleanup-only)", lt.cfg.prefix, err)
		}
		hidden += resp.Count
		if resp.Remaining == 0 {
			break
		}
	}
	left, err := lt.matching()
	if err != nil {
		return err
	}
	if left > 0 {
		return fmt.Errorf("%d principals named %s-* are still not hidden (run again with --cleanup-only)", left, lt.cfg.prefix)
	}
	lt.h.say("cleanup: hid %d principals named %s-*; none is left", hidden, lt.cfg.prefix)
	return nil
}
