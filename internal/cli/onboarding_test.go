package cli

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- RFC 6238's default, the authenticator a test stands in for; HMAC-SHA-1 is not broken as a MAC
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The guest, account and password commands (issue #715) against a real conchd
// handler, as the browser uses it: every route is the public one, and the
// server decides. Each test checks that no password, code or token reaches
// standard output, standard error, an error or a log line, except the one
// output whose result it is.

// onboardEnv is a server with a TOTP key, an operator holding a script
// credential, and the log lines both sides wrote.
type onboardEnv struct {
	url           string
	st            *store.Store
	operatorID    int64
	operatorToken string
	logs          *syncBuffer
	// lastStep is the newest authenticator step a code was made for: the
	// server refuses a step once used, so each code is for a later one.
	lastStep int64
}

// syncBuffer is a log sink the server's goroutines may write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newOnboardEnv(t *testing.T) *onboardEnv {
	t.Helper()
	isolateConfig(t)
	logs := &syncBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	op, _, token, err := st.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatalf("bootstrap operator: %v", err)
	}
	key, err := server.ParseTOTPKey(base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, server.TOTPKeyBytes)))
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Config{DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "vtest", TOTPKey: key}, st)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &onboardEnv{url: ts.URL, st: st, operatorID: op.ID, operatorToken: token, logs: logs}
}

// run runs conch with args and --server, token as CONCH_TOKEN ("" for the
// stored login) and stdin as the answers to its questions.
func (e *onboardEnv) run(t *testing.T, token, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("CONCH_TOKEN", token)
	return runCLI(t, stdin, append(args, "--server", e.url)...)
}

// mustRun is run that fails the test on an error.
func (e *onboardEnv) mustRun(t *testing.T, token, stdin string, args ...string) (stdout, stderr string) {
	t.Helper()
	stdout, stderr, err := e.run(t, token, stdin, args...)
	if err != nil {
		t.Fatalf("conch %s: %v\nstdout %q\nstderr %q", strings.Join(args, " "), err, stdout, stderr)
	}
	return stdout, stderr
}

// openJoining opens the bare address to accounts, into lobby, as the
// operator's script credential, which is never asked to re-authenticate.
// Guests are removed for now (issue #1005): no entry point admits them.
func (e *onboardEnv) openJoining(t *testing.T) {
	t.Helper()
	e.mustRun(t, e.operatorToken, "", "admin", "channel", "create", "lobby")
	out, _ := e.mustRun(t, e.operatorToken, "", "admin", "join", "set", "--open-joining", "account", "--open-channel", "lobby")
	if !strings.Contains(out, "open joining: account\nguests: off\n") || !strings.Contains(out, "open channels: lobby\n") {
		t.Fatalf("join set printed %q", out)
	}
}

// seedVisit starts a guest visit into lobby through the server's store, as
// `conch join guest` did before guests were removed (issue #1005), and
// stores it here as that command did. It returns the visit's tag.
func (e *onboardEnv) seedVisit(t *testing.T, label string) string {
	t.Helper()
	// As a command run with no token of its own: the stored visit is the
	// credential from here on.
	t.Setenv("CONCH_TOKEN", "")
	ctx := context.Background()
	lobby, err := e.st.ChannelByName(ctx, "lobby")
	if err != nil {
		t.Fatal(err)
	}
	g, err := e.st.CreateGuest(ctx, "system", store.NewGuest{Label: label, Origin: schema.GuestOriginAddress, Channels: []int64{lobby.ID},
		Idle: 2 * time.Hour, Absolute: 24 * time.Hour})
	if err != nil {
		t.Fatalf("CreateGuest: %v", err)
	}
	if err := saveGuestVisit(e.url, storedGuestVisit{Token: g.Token, Handle: g.Visit.Handle, ExpiresAt: g.Visit.ExpiresAt}); err != nil {
		t.Fatal(err)
	}
	return schema.GuestTag(g.Visit.Handle)
}

// totp is an authenticator's code for secret at step (RFC 6238, the server's
// parameters).
func totp(secret []byte, step int64) string {
	mac := hmac.New(sha1.New, secret)
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step)) //nolint:gosec // a step is never negative
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	v := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	return fmt.Sprintf("%0*d", schema.TOTPDigits, v%1_000_000)
}

// nextCode is the code for the earliest step the server still takes: one
// after the last used, and no earlier than the step before now (the server
// allows a step either side). Past the step after now it waits for the clock.
func (e *onboardEnv) nextCode(t *testing.T, secret []byte) string {
	t.Helper()
	for {
		now := time.Now().Unix() / schema.TOTPPeriodSeconds
		step := max(now-1, e.lastStep+1)
		if step <= now+1 {
			e.lastStep = step
			return totp(secret, step)
		}
		time.Sleep(time.Second)
	}
}

// assertNoSecrets fails when any of secrets appears in what the client
// printed or the server logged.
func (e *onboardEnv) assertNoSecrets(t *testing.T, outputs []string, secrets ...string) {
	t.Helper()
	all := strings.Join(outputs, "\n") + "\n" + e.logs.String()
	for _, s := range secrets {
		if s != "" && strings.Contains(all, s) {
			t.Errorf("a secret (%d bytes) appears in the output or the logs", len(s))
		}
	}
}

// guestFile is the temporary guest file of the isolated configuration.
func guestFile() string {
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "conch", guestVisitsFileName)
}

// memberFile is the member credentials file of the isolated configuration.
func memberFile() string {
	return filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "conch", credentialsFileName)
}

const memberPassword = "a long enough member password"

// A whole guest visit, for one live when guests were removed (issue
// #1005): this client offers no way to start one and join info offers none;
// a visit already live keeps a credential only in the temporary guest file,
// meets the server's refusals of what a guest may not do, creates an account
// that keeps the principal, and the member signs back in with the password.
func TestGuestLifecycle(t *testing.T) {
	e := newOnboardEnv(t)
	e.openJoining(t)

	info, _ := e.mustRun(t, "", "", "join", "info")
	if !strings.Contains(info, "create an account: yes\npassword sign-in: yes\n") || strings.Contains(info, "guest") {
		t.Errorf("join info = %q", info)
	}
	for _, args := range [][]string{{"join", "guest", "--label", "Pat"}, {"join", "guest", "--invite"}} {
		if _, _, err := e.run(t, "", "", args...); err == nil || !strings.Contains(err.Error(), "unknown subcommand (want info or account)") {
			t.Errorf("%v = %v, want no such command", args, err)
		}
		if _, err := os.Stat(guestFile()); !os.IsNotExist(err) {
			t.Fatalf("%v stored a visit (%v)", args, err)
		}
	}

	tag := e.seedVisit(t, "Pat")
	// The visit is stored only in its own private file.
	if _, err := os.Stat(memberFile()); !os.IsNotExist(err) {
		t.Errorf("a guest visit wrote credentials.json (%v)", err)
	}
	fi, err := os.Stat(guestFile())
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("guest file: %v, mode %v; want it, 0600", err, fi)
	}
	guestToken, err := LoadToken(e.url)
	if err != nil || !wellFormedToken(guestToken) {
		t.Fatalf("LoadToken = %v, a guest token is not loaded", err)
	}

	who, _ := e.mustRun(t, "", "", "whoami")
	for _, want := range []string{"class: guest\n", "guest: Guest: Pat · " + tag + ", a temporary visit from address\n", "visit ends: ", "create an account: open to this visit"} {
		if !strings.Contains(who, want) {
			t.Errorf("whoami = %q, want %q", who, want)
		}
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.SplitN(who, "\n", 2)[0], "id: "), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	// send takes its flags first.
	if _, _, err := runCLI(t, "", "send", "--server", e.url, "lobby", "hello from a guest"); err != nil {
		t.Fatalf("a guest posts in its channel: %v", err)
	}

	// Parity refusals: the CLI asks; the server refuses a guest.
	refusals := []struct {
		name string
		args []string
		code string
	}{
		{"invite", []string{"admin", "invite", "create", "--open", "--channel", "lobby", "--expires", "1h"}, schema.ErrorCodeGuestNotAllowed},
		{"device link", []string{"device-link", "create"}, schema.ErrorCodeGuestNotAllowed},
		{"a member sign-in over the visit", []string{"login", "--password", "--name", "root"}, "end it first"},
	}
	for _, tt := range refusals {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := e.run(t, "", memberPassword+"\n", tt.args...); err == nil || !strings.Contains(err.Error(), tt.code) {
				t.Errorf("error = %v, want %s", err, tt.code)
			}
		})
	}

	out, _ := e.mustRun(t, "", memberPassword+"\n", "account", "create", "alice")
	if want := fmt.Sprintf("created account alice (id %d) on %s and signed in", id, e.url); !strings.HasPrefix(out, want) {
		t.Errorf("account create = %q, want %q", out, want)
	}
	if _, err := os.Stat(guestFile()); !os.IsNotExist(err) {
		t.Errorf("the guest file is still there after the account was made (%v)", err)
	}
	who, _ = e.mustRun(t, "", "", "whoami")
	if !strings.HasPrefix(who, fmt.Sprintf("id: %d\nkind: human\nname: alice\nrole: member\n", id)) || !strings.Contains(who, "class: member\n") || strings.Contains(who, "guest:") {
		t.Errorf("whoami after the account = %q", who)
	}
	// The visit's credential went with the visit.
	if _, _, err := e.run(t, guestToken, "", "whoami"); err == nil {
		t.Error("the guest token still works after the account was made")
	}

	// A member comes back with the password, on a machine that has no login.
	e.mustRun(t, "", "", "logout")
	out, _ = e.mustRun(t, "", memberPassword+"\n", "login", "--password", "--name", "alice")
	if want := "logged in to " + e.url + " as alice (member)\n"; out != want {
		t.Errorf("login = %q, want %q", out, want)
	}
	if _, _, err := e.run(t, "", "", "whoami"); err != nil {
		t.Errorf("whoami after signing back in: %v", err)
	}
	if _, _, err := e.run(t, "", "the wrong member password\n", "login", "--password", "--name", "alice"); err == nil || err.Error() != errLoginFailed.Error() {
		t.Errorf("a wrong password = %v, want the one sign-in failure", err)
	}
	e.assertNoSecrets(t, []string{out, info, who}, memberPassword, guestToken)
}

// End visit ends the visit on the server, then here; so does logout on a
// visit. A visit the server ended without this machine (an operator) is
// removed the first time the server refuses it.
func TestEndVisit(t *testing.T) {
	e := newOnboardEnv(t)
	e.openJoining(t)

	e.seedVisit(t, "")
	token, _ := LoadToken(e.url)
	out, _ := e.mustRun(t, "", "", "end-visit")
	if !strings.HasPrefix(out, "ended the visit of guest-") || !strings.HasSuffix(out, "; your messages remain\n") {
		t.Errorf("end-visit = %q", out)
	}
	if _, err := os.Stat(guestFile()); !os.IsNotExist(err) {
		t.Errorf("the guest file remains (%v)", err)
	}
	if _, _, err := e.run(t, token, "", "whoami"); err == nil {
		t.Error("the ended visit's token still works")
	}
	if _, _, err := e.run(t, "", "", "end-visit"); err == nil || !strings.Contains(err.Error(), "no guest visit is stored") {
		t.Errorf("a second end-visit = %v", err)
	}

	// logout on a visit ends it, rather than only forgetting it.
	e.seedVisit(t, "")
	token, _ = LoadToken(e.url)
	if out, _ := e.mustRun(t, "", "", "logout"); !strings.HasPrefix(out, "ended the visit of ") {
		t.Errorf("logout on a visit = %q", out)
	}
	if _, _, err := e.run(t, token, "", "whoami"); err == nil {
		t.Error("logout left the visit live on the server")
	}

	// An operator ends a visit; the next command says so and forgets it.
	e.seedVisit(t, "")
	list, _ := e.mustRun(t, e.operatorToken, "", "admin", "guest", "list")
	id := strings.Fields(list)[0]
	if out, _ := e.mustRun(t, e.operatorToken, "", "admin", "guest", "end", id); !strings.HasPrefix(out, "ended guest visit "+id+" (guest-") {
		t.Errorf("admin guest end = %q", out)
	}
	_, _, err := e.run(t, "", "", "whoami")
	if err == nil || !strings.Contains(err.Error(), "your guest visit to "+e.url+" has ended") {
		t.Errorf("whoami on an ended visit = %v", err)
	}
	if _, err := os.Stat(guestFile()); !os.IsNotExist(err) {
		t.Errorf("a visit the server refused is still stored (%v)", err)
	}
}

// End all guest visits is a dry run until --yes, and lists whom it ends; a
// conversion permit lets a guest of a closed entry point make an account.
func TestAdminGuestEndAllAndPermit(t *testing.T) {
	e := newOnboardEnv(t)
	e.openJoining(t)
	e.seedVisit(t, "")
	dry, _ := e.mustRun(t, e.operatorToken, "", "admin", "guest", "end-all")
	if !strings.HasSuffix(dry, "would end 1 guest visit(s); run again with --yes to do it\n") {
		t.Errorf("dry run = %q", dry)
	}
	if _, _, err := e.run(t, "", "", "whoami"); err != nil {
		t.Errorf("a dry run ended the visit: %v", err)
	}

	// Close account creation at the address: the guest needs a permit.
	e.mustRun(t, e.operatorToken, "", "admin", "join", "set", "--open-joining", "off")
	if _, _, err := e.run(t, "", memberPassword+"\n", "account", "create", "carol"); err == nil || !strings.Contains(err.Error(), "conversion permit") {
		t.Fatalf("account create with the policy closed = %v", err)
	}
	list, _ := e.mustRun(t, e.operatorToken, "", "admin", "guest", "list")
	id := strings.Fields(list)[0]
	permit, notes := e.mustRun(t, e.operatorToken, "", "admin", "guest", "permit", id)
	code := strings.TrimSpace(permit)
	if schema.ValidateSignInLinkCode(code) != nil || strings.Contains(notes, code) {
		t.Fatalf("permit printed %q, notes %q", permit, notes)
	}
	out, stderr := e.mustRun(t, "", code+"\n"+memberPassword+"\n", "account", "create", "carol", "--permit")
	if !strings.HasPrefix(out, "created account carol (id "+id+")") {
		t.Errorf("account create --permit = %q", out)
	}
	e.assertNoSecrets(t, []string{out, stderr, list}, code, memberPassword)

	e.mustRun(t, "", "", "logout")
	e.seedVisit(t, "")
	done, _ := e.mustRun(t, e.operatorToken, "", "admin", "guest", "end-all", "--yes")
	if !strings.HasSuffix(done, "ended 1 guest visit(s)\n") {
		t.Errorf("end-all --yes = %q", done)
	}
}

// setupDriver is standard output for `conch setup` that answers like the
// person: when the enrolment's secret is shown it types the app's code.
type setupDriver struct {
	t     *testing.T
	e     *onboardEnv
	mu    sync.Mutex
	out   bytes.Buffer
	stdin *io.PipeWriter
	// secret is the enrolled secret, once seen.
	secret []byte
}

var secretLine = regexp.MustCompile(`\n  secret:    ([A-Z2-7]+)\n`)

func (d *setupDriver) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.out.Write(p)
	if d.secret == nil {
		if m := secretLine.FindStringSubmatch(d.out.String()); m != nil {
			secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(m[1])
			if err != nil {
				d.t.Errorf("the secret is not base32: %v", err)
				return len(p), nil
			}
			d.secret = secret
			code := d.e.nextCode(d.t, secret)
			// Not from inside this Write: the command reads the answer
			// after it returns.
			go func() { _, _ = io.WriteString(d.stdin, code+"\n") }()
		}
	}
	return len(p), nil
}

func (d *setupDriver) String() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.out.String()
}

const operatorPassword = "an operator password long enough"

// setupOperator runs `conch setup` with a host-made token, as an operator
// does after `conchd operator setup`, and returns the enrolled secret.
func (e *onboardEnv) setupOperator(t *testing.T) []byte {
	t.Helper()
	tok, err := e.st.IssueSetupToken(context.Background(), e.operatorID, time.Now())
	if err != nil {
		t.Fatalf("IssueSetupToken: %v", err)
	}
	t.Setenv("CONCH_TOKEN", "")
	in, feed := io.Pipe()
	t.Cleanup(func() { _ = feed.Close() })
	go func() { _, _ = io.WriteString(feed, tok.Token+"\n"+operatorPassword+"\n") }()
	d := &setupDriver{t: t, e: e, stdin: feed}
	var stderr bytes.Buffer
	if err := RunWithStdin(context.Background(), []string{"setup", "--server", e.url}, in, d, &stderr, "vtest"); err != nil {
		t.Fatalf("setup: %v\nstdout %q\nstderr %q", err, d.String(), stderr.String())
	}
	if want := "setup complete for root; sign in with: conch login --password --name 'root' --server " + e.url + "\n"; !strings.HasSuffix(d.String(), want) {
		t.Errorf("setup stdout = %q, want it to end %q", d.String(), want)
	}
	if !strings.HasPrefix(stderr.String(), "Setting up root: a new password, then an authenticator app where the server asks for one.\n") {
		t.Errorf("setup stderr = %q", stderr.String())
	}
	e.assertNoSecrets(t, []string{stderr.String()}, tok.Token, operatorPassword)
	if strings.Contains(d.String(), tok.Token) || strings.Contains(d.String(), operatorPassword) {
		t.Error("setup printed the token or the password")
	}
	return d.secret
}

// An operator's whole interactive path: setup from a host-made token, sign-in
// with password and code, and a sensitive command that answers
// reauth_required, proved and sent again. A script credential is never asked.
func TestOperatorSetupLoginAndReauth(t *testing.T) {
	e := newOnboardEnv(t)
	secret := e.setupOperator(t)

	code := e.nextCode(t, secret)
	out, stderr := e.mustRun(t, "", operatorPassword+"\n"+code+"\n", "login", "--password", "--name", "root")
	if want := "logged in to " + e.url + " as root (operator)\n"; out != want {
		t.Errorf("login = %q, want %q", out, want)
	}
	if !strings.HasPrefix(stderr, "this sign-in lasts until ") || !strings.Contains(stderr, "ends sooner if unused for 1h") {
		t.Errorf("login stderr = %q", stderr)
	}
	who, _ := e.mustRun(t, "", "", "whoami")
	if !strings.Contains(who, "class: operator\nsetup: complete (password set, authenticator enrolled)\nsign-in ends: ") {
		t.Errorf("whoami = %q", who)
	}

	// The session holds no recent proof yet: the server asks, the command
	// asks the person, and the change is made on the second send.
	reauthCode := e.nextCode(t, secret)
	set, asked := e.mustRun(t, "", operatorPassword+"\n"+reauthCode+"\n", "admin", "join", "set", "--max-guests", "7")
	if asked != "This needs a recent proof that it is you: your password and an authenticator code.\n" {
		t.Errorf("stderr = %q, want the one question", asked)
	}
	if !strings.Contains(set, "max guests: 7 ") {
		t.Errorf("join set = %q", set)
	}
	who, _ = e.mustRun(t, "", "", "whoami")
	if !strings.Contains(who, "re-authenticated until: ") {
		t.Errorf("whoami after the proof = %q", who)
	}
	// Within the proof's ten minutes no question is asked.
	if _, asked := e.mustRun(t, "", "", "admin", "join", "set", "--guests", "off"); asked != "" {
		t.Errorf("asked again within the proof: %q", asked)
	}
	// The script credential is outside the class and is never asked.
	if _, asked := e.mustRun(t, e.operatorToken, "", "admin", "join", "set", "--max-guests", "8"); asked != "" {
		t.Errorf("a script credential was asked to re-authenticate: %q", asked)
	}
	e.assertNoSecrets(t, []string{out, stderr, who, set, asked}, operatorPassword, code, reauthCode)
}

// `conch admin join set` refuses what would let guests in with the
// server's answer, and changes nothing; the rest still work (issue #1005).
func TestAdminJoinSetRefusesGuests(t *testing.T) {
	e := newOnboardEnv(t)
	e.openJoining(t)
	for _, tt := range []struct {
		args []string
		want string // "" for a change made
	}{
		{[]string{"--guests", "on"}, schema.ErrorCodeJoinPolicyConflict},
		{[]string{"--open-joining", "guest"}, schema.ErrorCodeJoinPolicyConflict},
		{[]string{"--open-joining", "both"}, schema.ErrorCodeJoinPolicyConflict},
		{[]string{"--open-joining", "both", "--guests", "off"}, schema.ErrorCodeJoinPolicyConflict},
		{[]string{"--guests", "off"}, ""},
		{[]string{"--open-joining", "off"}, ""},
		{[]string{"--open-joining", "account"}, ""},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			before, _ := e.mustRun(t, e.operatorToken, "", "admin", "join", "status")
			out, _, err := e.run(t, e.operatorToken, "", append([]string{"admin", "join", "set"}, tt.args...)...)
			if tt.want == "" {
				if err != nil || !strings.Contains(out, "guests: off\n") {
					t.Errorf("join set = %q, %v", out, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "guests are turned off") {
				t.Errorf("join set = %v, want the server's %s and its reason", err, tt.want)
			}
			if after, _ := e.mustRun(t, e.operatorToken, "", "admin", "join", "status"); after != before {
				t.Errorf("a refused change changed the settings: %q -> %q", before, after)
			}
		})
	}
}

// A wrong proof is refused, and the command it guarded does nothing.
func TestOperatorReauthRefused(t *testing.T) {
	e := newOnboardEnv(t)
	secret := e.setupOperator(t)
	e.mustRun(t, "", operatorPassword+"\n"+e.nextCode(t, secret)+"\n", "login", "--password", "--name", "root")
	_, _, err := e.run(t, "", "not the operator password\n"+e.nextCode(t, secret)+"\n", "admin", "join", "set", "--guests", "on")
	if err == nil || !strings.Contains(err.Error(), "the password or the code is not right") {
		t.Fatalf("a wrong proof = %v", err)
	}
	status, _ := e.mustRun(t, e.operatorToken, "", "admin", "join", "status")
	if !strings.Contains(status, "guests: off\n") {
		t.Errorf("the refused command changed the settings: %q", status)
	}
}

// A member changes a password with the current one, an operator's reset link
// replaces a forgotten one, and a member from before passwords sets a first
// one with an operator's enrollment code.
func TestMemberPasswords(t *testing.T) {
	e := newOnboardEnv(t)
	e.openJoining(t)
	e.mustRun(t, "", memberPassword+"\n", "join", "account", "bob")
	who, _ := e.mustRun(t, "", "", "whoami")
	bob := strings.TrimPrefix(strings.SplitN(who, "\n", 2)[0], "id: ")

	const changed = "the member password, changed"
	out, _ := e.mustRun(t, "", memberPassword+"\n"+changed+"\n", "password", "change")
	if out != "password changed; 0 other sign-in(s) signed out; this sign-in was renewed and stored\n" {
		t.Errorf("password change = %q", out)
	}
	if _, _, err := e.run(t, "", "", "whoami"); err != nil {
		t.Errorf("the renewed sign-in does not work: %v", err)
	}

	link, notes := e.mustRun(t, e.operatorToken, "", "admin", "principal", "sign-in-link", "--purpose", "reset", bob)
	if !signInLinkPattern.MatchString(link) || !strings.HasPrefix(notes, "password reset link ") {
		t.Fatalf("reset link = %q, notes %q", link, notes)
	}
	e.mustRun(t, "", "", "logout")
	const reset = "the member password, reset"
	out, stderr := e.mustRun(t, "", link+reset+"\n", "password", "reset")
	if !strings.HasPrefix(out, "password reset for bob; ") || stderr != "Resetting the password of bob.\n" {
		t.Errorf("password reset = %q, stderr %q", out, stderr)
	}
	e.mustRun(t, "", reset+"\n", "login", "--password", "--name", "bob")

	// A member made before passwords, signed in with an operator's credential.
	legacy, err := e.st.CreatePrincipal(context.Background(), store.PrincipalHuman, "dora")
	if err != nil {
		t.Fatal(err)
	}
	_, legacyToken, err := e.st.CreateCredential(context.Background(), "system", legacy.ID, "laptop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.run(t, legacyToken, memberPassword+"\n", "password", "set"); err == nil || !strings.Contains(err.Error(), "--purpose enrollment") {
		t.Errorf("password set without a proof = %v", err)
	}
	enrol, notes := e.mustRun(t, e.operatorToken, "", "admin", "principal", "sign-in-link", "--purpose", "enrollment", strconv.FormatInt(legacy.ID, 10))
	proof := strings.TrimSpace(enrol)
	if schema.ValidateSignInLinkCode(proof) != nil || !strings.HasPrefix(notes, "enrollment code ") {
		t.Fatalf("enrollment = %q, notes %q", enrol, notes)
	}
	renewed, stderr := e.mustRun(t, legacyToken, proof+"\n"+memberPassword+"\n", "password", "set", "--proof")
	// CONCH_TOKEN carried the request: the renewed credential is handed back.
	if !wellFormedToken(strings.TrimSpace(renewed)) || !strings.Contains(stderr, "CONCH_TOKEN no longer works") {
		t.Errorf("password set --proof = %q, stderr %q", renewed, stderr)
	}
	e.mustRun(t, "", "", "logout")
	e.mustRun(t, "", memberPassword+"\n", "login", "--password", "--name", "dora")
	e.assertNoSecrets(t, []string{out, stderr, notes}, memberPassword, changed, reset, proof)
}

// A member's device link: made after a fresh proof of the password, used on
// another machine with the link asked for and the account confirmed, and
// listed and closed.
func TestDeviceLinks(t *testing.T) {
	e := newOnboardEnv(t)
	e.openJoining(t)
	e.mustRun(t, "", memberPassword+"\n", "join", "account", "erin")
	e.mustRun(t, "", "", "logout")
	// A password sign-in is a session; the account's first credential, too,
	// but the proof is asked for either way when it is missing.
	e.mustRun(t, "", memberPassword+"\n", "login", "--password", "--name", "erin")
	link, notes := e.mustRun(t, "", memberPassword+"\n", "device-link", "create", "--label", "phone")
	if !signInLinkPattern.MatchString(link) {
		t.Fatalf("device link = %q", link)
	}
	if !strings.HasPrefix(notes, "This needs a recent proof that it is you: your password.\ndevice link ") {
		t.Errorf("stderr = %q", notes)
	}
	list, _ := e.mustRun(t, "", "", "device-link", "list")
	if lines := strings.Split(strings.TrimSpace(list), "\n"); len(lines) != 1 || !strings.HasSuffix(lines[0], " phone") {
		t.Errorf("device-link list = %q", list)
	}

	// The other machine: its own configuration, no login.
	t.Setenv(configDirEnv, filepath.Join(t.TempDir(), "other"))
	if _, _, err := e.run(t, "", link, "login", "--link"); err == nil || !strings.Contains(err.Error(), "give --yes") {
		t.Errorf("a link from a pipe without --yes = %v", err)
	}
	// At a terminal the link is typed without echo and the account it opens
	// is confirmed; anything but yes leaves the link unused.
	stdin, _ := fakeTerminalInput(t, []string{strings.TrimSpace(link)}, []string{"n"})
	var said bytes.Buffer
	err := RunWithStdin(context.Background(), []string{"login", "--link", "--server", e.url}, stdin, io.Discard, &said, "vtest")
	if err == nil || !strings.Contains(err.Error(), "the link was not used") {
		t.Errorf("declined = %v", err)
	}
	if want := "Sign-in link or code: \nSign in to " + e.url + " as erin? [y/N] "; said.String() != want {
		t.Errorf("stderr = %q, want %q", said.String(), want)
	}
	out, _ := e.mustRun(t, "", link, "login", "--link", "--yes")
	if out != "logged in to "+e.url+" as erin (member)\n" {
		t.Errorf("login --link = %q", out)
	}
	if _, _, err := e.run(t, "", link, "login", "--link", "--yes"); err == nil || !strings.Contains(err.Error(), "that link does not work") {
		t.Errorf("a used link = %v", err)
	}
	t.Setenv(configDirEnv, "")

	second, _ := e.mustRun(t, "", "", "device-link", "create")
	_ = second
	list, _ = e.mustRun(t, "", "", "device-link", "list")
	id := strings.Fields(list)[0]
	if out, _ := e.mustRun(t, "", "", "device-link", "close", id); out != "closed device link "+id+"\n" {
		t.Errorf("device-link close = %q", out)
	}
	if list, _ := e.mustRun(t, "", "", "device-link", "list"); list != "" {
		t.Errorf("after closing, list = %q", list)
	}
	e.assertNoSecrets(t, []string{notes, out}, memberPassword, strings.TrimSpace(link))
}

// An invite: join account takes its code on standard input, never as an
// argument, and makes an account through the v2 route with its password.
// Guests are removed for now (issue #1005): an invite that would admit them
// is refused with the server's answer.
func TestJoinThroughInvite(t *testing.T) {
	e := newOnboardEnv(t)
	e.openJoining(t)
	for _, mode := range []string{"both", "guest"} {
		if _, _, err := e.run(t, e.operatorToken, "", "admin", "invite", "create", "--open", "--channel", "lobby", "--expires", "1h", "--join-as", mode); err == nil ||
			!strings.Contains(err.Error(), schema.ErrorCodeJoinPolicyConflict) {
			t.Errorf("invite create --join-as %s = %v, want the server's %s", mode, err, schema.ErrorCodeJoinPolicyConflict)
		}
	}
	link, _ := e.mustRun(t, e.operatorToken, "", "admin", "invite", "create", "--open", "--channel", "lobby", "--expires", "1h")
	out, _ := e.mustRun(t, "", link+memberPassword+"\n", "join", "account", "frank", "--invite")
	if !strings.HasPrefix(out, "created account frank (id ") {
		t.Errorf("join account --invite = %q", out)
	}
	list, _ := e.mustRun(t, e.operatorToken, "", "admin", "invite", "list")
	if !strings.Contains(list, " uses 1 ") {
		t.Errorf("the invite's uses = %q", list)
	}
	e.assertNoSecrets(t, []string{out, list}, strings.TrimSpace(link), memberPassword)
}

// Any secret on the command line is refused before anything is sent or
// asked, and the refusal does not repeat it.
func TestSecretArgumentsRefused(t *testing.T) {
	sil := schema.SignInLinkCodePrefix + strings.Repeat("s", 43)
	inv := schema.InviteCodePrefix + strings.Repeat("i", 43)
	setup := schema.SetupTokenPrefix + strings.Repeat("t", 43)
	const password = "hunter2hunter2hunter2"
	tests := []struct {
		name   string
		args   []string
		secret string
	}{
		{"password as a flag value", []string{"login", "--password=" + password}, password},
		{"password after the switch", []string{"login", "--password", password}, password},
		{"a code flag", []string{"login", "--password", "--code", "123456"}, "123456"},
		{"a token flag", []string{"login", "--token", fakeToken}, fakeToken},
		{"an invite code argument", []string{"join", "account", "bob", inv}, inv},
		{"an invite code flag value", []string{"join", "account", "bob", "--invite=" + inv}, inv},
		{"an invite link argument", []string{"join", "account", "bob", "https://x.example/app/#/invite/" + inv}, inv},
		{"a password flag on join", []string{"join", "account", "bob", "--password", password}, password},
		{"a permit argument", []string{"account", "create", "bob", "--permit", sil}, sil},
		{"a proof flag value", []string{"password", "set", "--proof=" + sil}, sil},
		{"a reset code argument", []string{"password", "reset", sil}, sil},
		{"a current password flag", []string{"password", "change", "--current", password}, password},
		{"a setup token argument", []string{"setup", setup}, setup},
		{"a setup token flag", []string{"setup", "--token=" + setup}, setup},
		{"a link argument", []string{"login", "--link", sil}, sil},
		{"a secret flag on device links", []string{"device-link", "create", "--secret", password}, password},
		{"a password to reauth", []string{"reauth", password}, password},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests int
			var mu sync.Mutex
			fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				mu.Lock()
				requests++
				mu.Unlock()
			}))
			defer fake.Close()
			t.Setenv("CONCH_TOKEN", fakeToken)
			stdout, stderr, err := runCLI(t, "an answer nobody asked for\n", append(tt.args, "--server", fake.URL)...)
			if err == nil {
				t.Fatal("a secret on the command line was taken")
			}
			for _, s := range []string{stdout, stderr, err.Error()} {
				if strings.Contains(s, tt.secret) {
					t.Errorf("the refusal repeats the secret: %q", s)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if requests != 0 {
				t.Errorf("%d requests were sent", requests)
			}
		})
	}
}

// A secret is never sent over plain http to another machine: the refusal
// comes before anything is asked or sent.
func TestSecretsNeedHTTPS(t *testing.T) {
	tests := [][]string{
		{"login", "--password", "--name", "bob"},
		{"login", "--link"},
		{"join", "account", "bob"},
		{"account", "create", "bob"},
		{"password", "set"},
		{"password", "change"},
		{"password", "reset"},
		{"reauth"},
		{"setup"},
		{"device-link", "create"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", fakeToken)
			_, _, err := runCLI(t, memberPassword+"\n", append(args, "--server", "http://conch.example:8080")...)
			if err == nil || !strings.Contains(err.Error(), "over plain http to conch.example:8080") {
				t.Errorf("error = %v, want the https refusal", err)
			}
		})
	}
}
