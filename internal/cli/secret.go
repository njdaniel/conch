package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Secrets a person types (ADR-009, design §5.4): passwords, authenticator
// codes, and the codes of invite, sign-in, reset, enrolment, permit and setup
// links. They come from standard input only: on a terminal with echo off and
// a prompt on standard error, otherwise one answer a line in the order the
// command asks, so `printf '%s\n' "$password" | conch login --password --name
// alice` works. Never from the argument list, which shell history and the
// process list keep, and never from the environment. Nothing in this file or
// in a command that uses it writes a secret to standard output, standard
// error or an error, unless the command's own result is that secret.

// maxStdinAnswers bounds what a prompter reads from a standard input that is
// not a terminal, so a pipe that never ends cannot fill memory.
const maxStdinAnswers = 64 << 10

// readVisible reads one line from a terminal with echo on, for answers that
// are not secret (a name, a yes or no). A seam, as readSecret is.
var readVisible = readVisibleLine

// readVisibleLine reads a line from f a byte at a time, so nothing past the
// line is taken from the terminal. Like readSecretNoEcho it gives up on the
// read when ctx ends; the abandoned goroutine ends with the process.
func readVisibleLine(ctx context.Context, f *os.File) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type result struct {
		line string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 1)
		for b.Len() < maxStdinAnswers {
			n, err := f.Read(buf)
			if n == 1 {
				if buf[0] == '\n' {
					break
				}
				b.WriteByte(buf[0])
			}
			if err != nil {
				done <- result{b.String(), err}
				return
			}
		}
		done <- result{b.String(), nil}
	}()
	select {
	case r := <-done:
		return r.line, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// prompter asks the person for what a command needs.
type prompter struct {
	ctx    context.Context //nolint:containedctx // one command's prompts share its context
	stderr io.Writer
	// tty is standard input when it is a terminal; lines reads it otherwise.
	tty   *os.File
	lines *bufio.Reader
}

func newPrompter(ctx context.Context, stdin io.Reader, stderr io.Writer) *prompter {
	p := &prompter{ctx: ctx, stderr: stderr}
	if f, ok := stdin.(*os.File); ok && isTerminalFD(int(f.Fd())) { //nolint:gosec // fds fit in int
		p.tty = f
		return p
	}
	p.lines = bufio.NewReader(io.LimitReader(stdin, maxStdinAnswers))
	return p
}

// interactive reports whether a person at a terminal is answering.
func (p *prompter) interactive() bool { return p.tty != nil }

// nextLine is the next answer from a standard input that is not a terminal:
// its line, without the line ending. Nothing left is no answer.
func (p *prompter) nextLine() (string, error) {
	line, err := p.lines.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if errors.Is(err, io.EOF) && line == "" {
		return "", io.EOF
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

// secret asks for one secret: on a terminal with prompt and echo off,
// otherwise as the next line of standard input. what names it in errors,
// which never carry it. Only the line ending is taken off: a password may
// begin or end with a space.
func (p *prompter) secret(prompt, what string) (string, error) {
	var answer string
	if p.tty != nil {
		_, _ = fmt.Fprint(p.stderr, prompt)
		b, err := readSecret(p.ctx, int(p.tty.Fd())) //nolint:gosec // fds fit in int
		// The Enter was not echoed either, so end the prompt line.
		_, _ = fmt.Fprintln(p.stderr)
		switch {
		case errors.Is(err, io.EOF):
		case p.ctx.Err() != nil:
			return "", p.ctx.Err()
		case err != nil:
			return "", fmt.Errorf("cli: cannot read the %s without echo (%w); give it on standard input instead", what, err)
		}
		answer = strings.TrimSuffix(string(b), "\r")
	} else {
		line, err := p.nextLine()
		switch {
		case errors.Is(err, io.EOF):
		case err != nil:
			return "", fmt.Errorf("cli: read the %s: %w", what, err)
		}
		answer = line
	}
	if answer == "" {
		return "", fmt.Errorf("cli: no %s given", what)
	}
	return answer, nil
}

// code asks for a secret that is a single token (a link's code, a setup
// token, an authenticator code): surrounding white space is no part of it.
func (p *prompter) code(prompt, what string) (string, error) {
	s, err := p.secret(prompt, what)
	if err != nil {
		return "", err
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("cli: no %s given", what)
	}
	return s, nil
}

// line asks for an answer that is not secret, shown as it is typed.
func (p *prompter) line(prompt, what string) (string, error) {
	var answer string
	if p.tty != nil {
		_, _ = fmt.Fprint(p.stderr, prompt)
		s, err := readVisible(p.ctx, p.tty)
		switch {
		case errors.Is(err, io.EOF):
		case p.ctx.Err() != nil:
			return "", p.ctx.Err()
		case err != nil:
			return "", fmt.Errorf("cli: read the %s: %w", what, err)
		}
		answer = s
	} else {
		s, err := p.nextLine()
		switch {
		case errors.Is(err, io.EOF):
		case err != nil:
			return "", fmt.Errorf("cli: read the %s: %w", what, err)
		}
		answer = s
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return "", fmt.Errorf("cli: no %s given", what)
	}
	return answer, nil
}

// confirm asks a yes-or-no question and reports whether the answer was yes.
// Anything but y or yes is no.
func (p *prompter) confirm(question string) (bool, error) {
	answer, err := p.line(question+" [y/N] ", "answer")
	if err != nil {
		if p.ctx.Err() != nil {
			return false, p.ctx.Err()
		}
		return false, nil
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// newPassword asks for a password to set, checks it as the server will
// first, and on a terminal asks for it twice: a typo nobody can see would
// otherwise lock the person out. The checks name the rule, never the
// password.
func (p *prompter) newPassword(prompt string) (string, error) {
	password, err := p.secret(prompt, "new password")
	if err != nil {
		return "", err
	}
	if err := schema.ValidatePassword(password); err != nil {
		return "", fmt.Errorf("cli: %w", err)
	}
	if p.interactive() {
		again, err := p.secret("Repeat it: ", "new password")
		if err != nil {
			return "", err
		}
		if again != password {
			return "", errors.New("cli: the two passwords differ; nothing was changed")
		}
	}
	return password, nil
}

// totpCode asks for an authenticator code and checks its shape.
func (p *prompter) totpCode(prompt string) (string, error) {
	code, err := p.code(prompt, "authenticator code")
	if err != nil {
		return "", err
	}
	if err := schema.ValidateTOTPCode(code); err != nil {
		return "", fmt.Errorf("cli: %w", err)
	}
	return code, nil
}

// secretFlagNames are flags someone might reach for to pass a secret. A
// command that takes secrets refuses every one of them with a value, and
// those it does not define as a switch at all.
var secretFlagNames = map[string]bool{
	"password": true, "pass": true, "passwd": true, "pw": true,
	"code": true, "totp": true, "otp": true, "token": true, "secret": true,
	"proof": true, "permit": true, "challenge": true, "link": true, "invite": true,
	"current": true, "new-password": true,
}

// secretShaped finds a server-made secret in an argument: every one of them
// (a credential, a link's code, a challenge, a setup token) is conch_, a
// purpose, and a long run of base64url.
var secretShaped = regexp.MustCompile(`conch_[A-Za-z0-9_-]{20,}`)

// errSecretArgument refuses a secret on the command line without repeating
// it. The person learns why, and where the secret goes instead.
func errSecretArgument(command string) error {
	return fmt.Errorf("cli: %s: secrets are not taken as arguments, where shell history and the process list would keep them; conch asks for it without echo on a terminal, or reads it from standard input", command)
}

// refuseSecretArgs looks at a command's arguments before they are parsed. A
// secret-sounding flag is refused when it carries a value, or when it is not
// one of switches (the command's own on/off flags, such as login's
// --password, which mean "ask me"); an argument with a server-made secret in
// it is refused wherever it is. A password typed as an extra argument cannot
// be told by its look; every such command refuses arguments it does not
// expect without repeating them.
func refuseSecretArgs(command string, args []string, switches ...string) error {
	ok := make(map[string]bool, len(switches))
	for _, s := range switches {
		ok[s] = true
	}
	for _, a := range args {
		if a != "-" && a != "--" && strings.HasPrefix(a, "-") {
			name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
			if secretFlagNames[name] && (!ok[name] || (hasValue && value != "true" && value != "false")) {
				return errSecretArgument(command)
			}
		}
		if secretShaped.MatchString(a) {
			return errSecretArgument(command)
		}
	}
	return nil
}

// quietFlags is a flag set for a command that takes secrets: it writes
// nothing itself (the flag package prints a refused value) and parse errors
// are reported by parseQuiet.
func quietFlags(name string) *flag.FlagSet {
	return newFlagSet(name, io.Discard)
}

// flagValueError finds the flag in the flag package's "invalid value" errors.
var flagValueError = regexp.MustCompile(`invalid (?:boolean )?value .* for (?:flag )?-([A-Za-z0-9-]+)`)

// parseQuiet parses args, flags and positionals in any order, and returns the
// positionals. A refused value is reported by its flag, never by the value.
func parseQuiet(fs *flag.FlagSet, command string, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if m := flagValueError.FindStringSubmatch(err.Error()); m != nil {
				return nil, fmt.Errorf("cli: %s: a value given for -%s is not one it takes", command, m[1])
			}
			// Never the flag package's own words: a wording this does not
			// know could carry a value that was typed.
			return nil, fmt.Errorf("cli: %s: cannot parse the flags; see conch help", command)
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// requireSecureServer refuses to send a password, code or setup token, or to
// ask for a bearer token, over plain HTTP to anything but this machine
// (design §5.4). The server refuses it too, but by then the secret has
// crossed the network in clear.
func requireSecureServer(command, server string) error {
	u, err := url.Parse(server)
	if err != nil {
		return fmt.Errorf("cli: parse server URL: %w", err)
	}
	if strings.EqualFold(u.Scheme, "https") {
		return nil
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("cli: %s: will not send a password, code or sign-in token over plain http to %s; use the server's https:// address (plain http is for this machine only)", command, safeLine(u.Host))
}

// codeFromInput takes a link's code out of what a person pasted: the bare
// code, or a whole link with the code in it (…/app/#/invite/<code>). check
// is the code's own shape check; the error names what was expected and never
// what was given.
func codeFromInput(input, prefix, what string, check func(string) error) (string, error) {
	if i := strings.Index(input, prefix); i >= 0 {
		input = input[i:]
		if j := strings.IndexAny(input, " \t/?&#"); j >= 0 {
			input = input[:j]
		}
	}
	if check(input) != nil {
		return "", fmt.Errorf("cli: that is not %s", what)
	}
	return input, nil
}

// clock formats a server time for a person: local, to the minute.
func clock(ts schema.Timestamp) string {
	return ts.Time().Local().Format("2006-01-02 15:04 MST")
}

// span formats a number of seconds as the largest whole unit that fits it:
// "24h", "2h", "90m", "45s".
func span(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	}
	return fmt.Sprintf("%ds", seconds)
}
