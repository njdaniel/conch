package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/pkg/schema"
)

// participantEnv are the files written for the people and agents: name, the
// principal they sign in as, and what they are for.
var participants = []struct{ name, principal, what string }{
	{"tui-a", "nick-test", "terminal 1: nick-test"},
	{"tui-b", "guest-test", "terminal 2: guest-test"},
	{"tui-a2", "nick-test", "terminal 3: nick-test again, with a second credential"},
	{"agent-a", "worker-test", "worker-test, the scripted agent that can post"},
	{"agent-b", "observer-test", "observer-test, the scripted agent that can only read"},
}

var safeWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shq quotes s for a POSIX shell, unless it needs none.
func shq(s string) string {
	if safeWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func ensureDir(path string) error { return os.MkdirAll(path, 0o700) }

// envLines renders KEY=value pairs as a file a shell can source.
func envLines(header string, pairs ...string) []byte {
	var b strings.Builder
	b.WriteString("# " + header + "\n")
	for i := 0; i+1 < len(pairs); i += 2 {
		b.WriteString(pairs[i] + "=" + shq(pairs[i+1]) + "\n")
	}
	return []byte(b.String())
}

func (r *runner) path(parts ...string) string {
	return filepath.Join(append([]string{r.root}, parts...)...)
}

// writeParticipantFiles writes one file per participant, each readable by its
// owner alone, each with a configuration directory of its own so that no
// participant's login can touch another's or the user's.
func (r *runner) writeParticipantFiles() error {
	fx := r.fx
	if err := r.copyFixtureScripts(); err != nil {
		return err
	}
	common := func(part string, token string) []string {
		return []string{
			"CONCH_CONFIG_DIR", r.path("config", part),
			"CONCH_SERVER", r.baseURL(),
			"CONCH_TOKEN", token,
		}
	}
	for _, p := range participants {
		c := fx.creds[p.name]
		if err := ensureDir(r.path("config", p.name)); err != nil {
			return err
		}
		pairs := common(p.name, c.token)
		if strings.HasPrefix(p.name, "agent-") {
			pairs = append(pairs, r.adapterVars(p.name, c)...)
		}
		header := p.name + ": " + p.what + ". Source it, then run the program; it holds a credential."
		if err := writeFile(r.path(p.name+".env"), envLines(header, pairs...), 0o600); err != nil {
			return err
		}
	}
	if err := writeFile(r.path("operator.env"),
		envLines("operator: for the admin commands of the walkthrough only. No participant uses it.",
			common("operator", fx.operatorToken)...), 0o600); err != nil {
		return err
	}
	browser := fmt.Sprintf("Address: %s/app/\nSign in as browser-test: paste the credential on the next line into the sign-in field.\n%s\n",
		r.baseURL(), fx.creds["browser"].token)
	if err := writeFile(r.path("browser.txt"), []byte(browser), 0o600); err != nil {
		return err
	}
	step("wrote an environment file per participant, operator.env and browser.txt, mode 0600")
	return nil
}

// adapterVars are the settings of a scripted agent. In fixture mode the command
// is a script from the root and the fixture markers are on; with --real-adapter
// agent-a gets neither, so conch-adapter runs its default command (with
// --runtime codex it names the codex runtime, whose default command is Codex's).
func (r *runner) adapterVars(part string, c credential) []string {
	vars := []string{
		"CONCH_ADAPTER_SERVER", r.baseURL(),
		"CONCH_ADAPTER_TOKEN", c.token,
		"CONCH_ADAPTER_PRINCIPAL_ID", id(r.fx.principals[c.principal]),
		"CONCH_ADAPTER_CHANNEL", chGeneral,
		"CONCH_ADAPTER_POLL_INTERVAL", "1s",
		"CONCH_ADAPTER_LOCK_FILE", r.path(part + ".lock"),
	}
	if part == "agent-a" && r.realAdapter {
		if r.runtime == "codex" {
			// The profile's default command is Codex's; the model has no default
			// there, so it is the caller's own if they set one.
			vars = append(vars, "CONCH_ADAPTER_RUNTIME", "codex")
			if model := os.Getenv("CONCH_ADAPTER_MODEL"); model != "" {
				vars = append(vars, "CONCH_ADAPTER_MODEL", model)
			}
		}
		return vars
	}
	return append(vars,
		"CONCH_ADAPTER_RUNTIME", "fixture",
		"CONCH_ADAPTER_MODEL", "fixture",
		"CONCH_ADAPTER_CMD", r.adapterCmd("echo.sh"),
		"CONCH_ADAPTER_FIXTURE", "1")
}

// adapterCmd is the value of CONCH_ADAPTER_CMD for a fixture script: the path,
// quoted the way the adapter reads a command line (not through a shell).
func (r *runner) adapterCmd(script string) string {
	return shq(r.path("fixtures", script))
}

var fixtureNames = []string{"echo.sh", "approval.sh", "slow.sh", "fail.sh", "hang.sh"}

func (r *runner) copyFixtureScripts() error {
	for _, n := range fixtureNames {
		data, err := fixtureScripts.ReadFile("fixtures/" + n)
		if err != nil {
			return err
		}
		if err := writeFile(r.path("fixtures", n), data, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// verifyFiles checks what the acceptance criteria say of the files: every one
// is mode 0600, and no participant's holds the operator's credential.
func (r *runner) verifyFiles() error {
	op := r.fx.operatorToken
	files := []string{"operator.env", "browser.txt"}
	for _, p := range participants {
		files = append(files, p.name+".env")
	}
	for _, f := range files {
		info, err := os.Stat(r.path(f))
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0o600 {
			return fmt.Errorf("%s has mode %o, want 600", f, info.Mode().Perm())
		}
		data, err := os.ReadFile(r.path(f)) // #nosec G304 -- a file this run just wrote
		if err != nil {
			return err
		}
		if has := strings.Contains(string(data), op); has != (f == "operator.env") {
			return fmt.Errorf("%s: the operator credential is %s", f, map[bool]string{true: "in it", false: "missing"}[has])
		}
	}
	step("every participant file is mode 0600 and none holds the operator's credential")
	return nil
}

// ----------------------------------------------------------------- commands

// source is the start of every printed command: the participant's environment.
func (r *runner) source(file string) string {
	return "set -a; . " + shq(r.path(file)) + "; set +a; "
}

// asConch is the command that runs conch with a participant's environment.
func (r *runner) asConch(part string, args ...string) string {
	return strings.TrimSpace(r.source(part+".env") + shq(r.bin.conch) + " " + strings.Join(args, " "))
}

func (r *runner) tuiCommand(part string) string { return r.asConch(part) }

func (r *runner) agentCommand(part string) string {
	return r.source(part+".env") + shq(r.bin.adapter)
}

func (r *runner) agentWith(part, script string) string {
	return r.source(part+".env") + "CONCH_ADAPTER_CMD=" + shq(r.adapterCmd(script)) + " " + shq(r.bin.adapter)
}

// selfCommand is how to run this program again, with CONCH_P3_ROOT when the
// root is not the default one.
func (r *runner) selfCommand(flags string) string {
	cmd := "go run ./e2e/p3-local " + flags
	if def, err := defaultRoot(); err != nil || def != r.root {
		cmd = "CONCH_P3_ROOT=" + shq(r.root) + " " + cmd
	}
	return cmd
}

// printCommands prints what to run, one command per line, and the values the
// walkthrough names (ids, the address). The same text goes to commands.txt.
func (r *runner) printCommands() error {
	fx := r.fx
	var b strings.Builder
	say := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	cmd := func(comment, command string) {
		say("# %s", comment)
		say("%s", command)
	}
	pid := func(name string) string { return id(fx.principals[name]) }
	opAdmin := func(args ...string) string {
		return r.source("operator.env") + shq(r.bin.conch) + " admin " + strings.Join(args, " ")
	}

	say("%s: ready. conchd is serving %s (pid %d); its log is %s", name, r.baseURL(), r.server.pid(), shq(r.path("logs", "conchd.log")))
	say("Run each command in a window of its own. Nothing below is started for you.")
	say("")
	for _, p := range participants {
		command := r.tuiCommand(p.name)
		if strings.HasPrefix(p.name, "agent-") {
			command = r.agentCommand(p.name)
		}
		cmd(p.name+": "+p.what, command)
	}
	say("# browser: browser-test. Open %s/app/ in a browser and paste the credential from this file", r.baseURL())
	say("cat %s", shq(r.path("browser.txt")))
	say("")
	if r.realAdapter {
		say("# agent-a is the real %s command (--real-adapter): see the warning above.", r.realProgramName())
	} else {
		say("# The next three replace agent-a's script. Stop the running agent-a first (ctrl-c).")
		for _, s := range []struct{ file, what string }{
			{"approval.sh", "each run asks for an approval, then replies"},
			{"slow.sh", "each run waits 30 seconds before it replies: time to cancel it"},
			{"fail.sh", "each run fails with exit 3"},
		} {
			cmd("agent-a with "+s.file+": "+s.what, r.agentWith("agent-a", s.file))
		}
	}
	cmd("freeze both agents, so that their heartbeats stop (their sessions go stale, then offline)", "pkill -STOP -f "+shq(r.bin.adapter))
	cmd("let both agents go on", "pkill -CONT -f "+shq(r.bin.adapter))
	say("")
	cmd("nick-test posts to the red net of nets-demo", r.asConch("tui-a", "send", "--net", "red", chNets, "'hello red'"))
	cmd("nick-test whispers to worker-test", r.asConch("tui-a", "send", "--to", pid("worker-test"), chGeneral, "'hello, a whisper'"))
	cmd("guest-test follows nets-demo (it monitors the red net; ctrl-c ends it)", r.asConch("tui-b", "tail", chNets))
	cmd("guest-test looks for a word in general", r.asConch("tui-b", "search", chGeneral, "hello"))
	cmd("guest-test is not in test-private: the CLI is refused", r.asConch("tui-b", "tail", chPrivate))
	cmd("guest-test is refused by the API itself (prints the status code)",
		r.source("tui-b.env")+`curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $CONCH_TOKEN" `+shq(r.baseURL()+"/v2/channels/"+chPrivate+"/messages"))
	cmd("who is this credential", r.asConch("tui-a", "whoami"))
	cmd("voice is not configured here: no LiveKit", r.asConch("tui-a", "voice", "status", chGeneral))
	cmd("approvals waiting for a decision", r.asConch("tui-a", "approvals", "list"))
	say("")
	cmd("operator: list nick-test's credentials", opAdmin("credential", "list", pid("nick-test")))
	cmd("operator: revoke the credential of terminal 3 (tui-a2) only", opAdmin("credential", "revoke", id(fx.creds["tui-a2"].id)))
	cmd("operator: take nick-test out of test-private", opAdmin("member", "remove", chPrivate, pid("nick-test")))
	cmd("operator: put nick-test back into test-private", opAdmin("member", "add", chPrivate, pid("nick-test")))
	say("")
	cmd("stop the server (pid of conchd)", fmt.Sprintf("kill %d", r.server.pid()))
	cmd("start it again on the same address and data, in a free window", fmt.Sprintf("%s serve --listen %s --web-dir %s --data %s",
		shq(r.bin.conchd), r.addr, shq(r.webDir), shq(r.path("data"))))
	say("")
	cmd("start again after a reset", r.selfCommand("--keep"))
	cmd("stop everything and remove "+r.root, r.selfCommand("--reset"))
	say("")
	say("values the walkthrough names:")
	say("  root %s", r.root)
	say("  address %s", r.addr)
	say("  webdir %s", r.webDir)
	say("  pid conchd %d", r.server.pid())
	for _, n := range everyone {
		say("  principal %s %s", n, pid(n))
	}
	for _, spec := range credSpecs {
		say("  credential %s %d", spec.participant, fx.creds[spec.participant].id)
	}

	text := b.String()
	if err := writeFile(r.path("commands.txt"), []byte(text), 0o600); err != nil {
		return err
	}
	fmt.Print(text)
	return nil
}

// defaultRoot is where the root is without CONCH_P3_ROOT.
func defaultRoot() (string, error) {
	if state := os.Getenv("XDG_STATE_HOME"); state != "" {
		return filepath.Join(state, "conch-p3"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "conch-p3"), nil
}

// ------------------------------------------------------------------ checks

// shell runs a printed command through sh, as a user's shell would, with the
// run's isolated HOME. It returns the combined output.
func (r *runner) shell(command string) (string, error) {
	out, err := r.output("", r.repo, r.isolatedEnv(), "sh", "-c", command+" 2>&1")
	return string(out), err
}

// checkParticipants is the run without --keep: each participant connects and
// sends, using the same commands it would be given.
func (r *runner) checkParticipants() error {
	fx := r.fx
	sent := make(map[string]int64) // body -> author id, to look for afterwards
	for _, part := range []string{"tui-a", "tui-b", "tui-a2"} {
		who, err := r.shell(r.asConch(part, "whoami"))
		if err != nil {
			return fmt.Errorf("%s: whoami: %w", part, err)
		}
		principal := fx.creds[part].principal
		if !strings.Contains(who, "name: "+principal+"\n") {
			return fmt.Errorf("%s: whoami does not say %s", part, principal)
		}
		body := "check: hello from " + part
		if _, err := r.shell(r.asConch(part, "send", chGeneral, shq(body))); err != nil {
			return fmt.Errorf("%s: send: %w", part, err)
		}
		sent[body] = fx.principals[principal]
		step("%s signed in as %s and sent a message", part, principal)
	}

	bc, err := r.client(fx.creds["browser"].token)
	if err != nil {
		return err
	}
	body := "check: hello from browser"
	if _, err := bc.PostMessageV2(runCtx, chGeneral, 0, body, nil); err != nil {
		return fmt.Errorf("browser: send: %w", err)
	}
	sent[body] = fx.principals["browser-test"]
	if err := r.checkWebApp(); err != nil {
		return err
	}
	step("browser-test sent a message with its own credential, and the web client is served at /app/")

	// Every sender must be seen by someone else, once, with the right author.
	reader, err := r.client(fx.creds["tui-b"].token)
	if err != nil {
		return err
	}
	list, err := reader.ListMessagesV2(runCtx, chGeneral, 0, 100)
	if err != nil {
		return err
	}
	for body, author := range sent {
		n := 0
		for _, m := range list.Messages {
			if m.Body == body {
				n++
				if m.AuthorID != author {
					return fmt.Errorf("%q was posted by %d, want %d", body, m.AuthorID, author)
				}
			}
		}
		if n != 1 {
			return fmt.Errorf("guest-test sees %q %d times, want once", body, n)
		}
	}
	step("guest-test reads each of those messages once, with the right author")

	if err := r.checkAgentA(); err != nil {
		return err
	}
	return r.checkAgentB()
}

func (r *runner) checkWebApp() error {
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(r.baseURL() + "/app/") // #nosec G107 -- this run's own loopback conchd
	if err != nil {
		return fmt.Errorf("GET /app/: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	page, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK || !strings.Contains(strings.ToLower(string(page)), "<html") {
		return fmt.Errorf("GET /app/: status %d, and not a web page", resp.StatusCode)
	}
	return nil
}

// startAgent starts an adapter with the printed command and waits until its
// session shows in the roster of general.
func (r *runner) startAgent(part string) (*child, error) {
	c, err := r.spawn(part+"-check", "sh", []string{"-c", r.agentCommand(part)}, r.isolatedEnv())
	if err != nil {
		return nil, err
	}
	principal := r.fx.creds[part].principal
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !c.running() {
			return nil, fmt.Errorf("%s exited at once; its log is %s%s", part, c.log, r.logTail(c.log))
		}
		sessions, err := r.sessions(r.fx.creds["tui-a"].token, chGeneral)
		if err != nil {
			return nil, err
		}
		for _, s := range sessions {
			if s.Name == principal && s.State == schema.SessionStateOnline {
				return c, nil
			}
		}
		if err := pause(100 * time.Millisecond); err != nil {
			return nil, err
		}
	}
	c.kill()
	return nil, fmt.Errorf("%s did not open a session within 20s; its log is %s%s", part, c.log, r.logTail(c.log))
}

// logTailLines is how much of a child's log a failure prints.
const logTailLines = 40

// logTail is the end of the log at path, redacted, for a failure message: the
// log of a CI run is gone by the time anyone looks. It is empty when the log
// cannot be read.
func (r *runner) logTail(path string) string {
	data, err := os.ReadFile(path) // #nosec G304 -- a log this run's child wrote
	if err != nil {
		return ""
	}
	return "\n" + r.redact(tailLines(strings.TrimRight(string(data), "\n"), logTailLines))
}

// sessions is the roster of channel as the holder of token sees it.
func (r *runner) sessions(token, channel string) ([]schema.SessionV1, error) {
	req, err := http.NewRequestWithContext(runCtx, http.MethodGet, r.baseURL()+"/v1/sessions?channel="+url.QueryEscape(channel), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second, Transport: newLoggingTransport()}).Do(req) // #nosec G107 -- this run's own loopback conchd
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /v1/sessions: status %d", resp.StatusCode)
	}
	var roster schema.SessionRosterV1
	if err := json.NewDecoder(resp.Body).Decode(&roster); err != nil {
		return nil, fmt.Errorf("GET /v1/sessions: %w", err)
	}
	return roster.Sessions, nil
}

// stopAgent stops an adapter the way ctrl-c does and checks that its session
// ended and its lock file went.
func (r *runner) stopAgent(part string, c *child) error {
	c.stop()
	principal := r.fx.creds[part].principal
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		sessions, err := r.sessions(r.fx.creds["tui-a"].token, chGeneral)
		if err != nil {
			return err
		}
		live := false
		for _, s := range sessions {
			if s.Name == principal && s.State == schema.SessionStateOnline {
				live = true
			}
		}
		if !live {
			return nil
		}
		if err := pause(100 * time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("%s's session is still online after it was stopped", part)
}

func (r *runner) checkAgentA() error {
	if r.realAdapter {
		fmt.Printf("NOT RUN  agent-a: --real-adapter would start the real %s command, and this check never does\n", r.realProgramName())
		return nil
	}
	c, err := r.startAgent("agent-a")
	if err != nil {
		return err
	}
	step("agent-a (worker-test) opened a session")
	text := "check: please answer"
	author, err := r.client(r.fx.creds["tui-a"].token)
	if err != nil {
		c.kill()
		return err
	}
	if _, err := author.PostMessageV2(runCtx, chGeneral, 0, text, nil); err != nil {
		c.kill()
		return err
	}
	worker := r.fx.principals["worker-test"]
	deadline := time.Now().Add(30 * time.Second)
	for {
		list, err := author.ListMessagesV2(runCtx, chGeneral, 0, 100)
		if err != nil {
			c.kill()
			return err
		}
		for _, m := range list.Messages {
			if m.AuthorID == worker && strings.Contains(m.Body, text) {
				step("agent-a replied in general")
				return r.stopAgent("agent-a", c)
			}
		}
		if time.Now().After(deadline) {
			c.kill()
			return fmt.Errorf("agent-a did not reply within 30s; its log is %s%s", c.log, r.logTail(c.log))
		}
		if err := pause(250 * time.Millisecond); err != nil {
			c.kill()
			return err
		}
	}
}

func (r *runner) checkAgentB() error {
	c, err := r.startAgent("agent-b")
	if err != nil {
		return err
	}
	step("agent-b (observer-test) opened a session")
	oc, err := r.client(r.fx.creds["agent-b"].token)
	if err != nil {
		c.kill()
		return err
	}
	_, err = oc.PostMessageV2(runCtx, chGeneral, 0, "check: the observer must not be able to post", nil)
	var refused *cli.ServerError
	switch {
	case err == nil:
		c.kill()
		return fmt.Errorf("observer-test posted a message, and its manifest does not allow it")
	case !errors.As(err, &refused) || refused.Status != http.StatusForbidden:
		// Only the manifest's refusal counts: a network error or a 500 is not
		// the observer being kept from posting.
		c.kill()
		return fmt.Errorf("observer-test's post failed, but not with the manifest's 403: %v", r.redact(err.Error()))
	}
	step("observer-test is refused when it posts, as its manifest says")
	return r.stopAgent("agent-b", c)
}

// realProgramName is how --real-adapter's command is called to a person.
func (r *runner) realProgramName() string {
	if r.runtime == "codex" {
		return "Codex"
	}
	return "Claude Code"
}
