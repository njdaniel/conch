package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/term"

	"github.com/njdaniel/conch/pkg/schema"
)

const defaultServer = "http://127.0.0.1:8080"

// Run dispatches a conch command using the supplied standard output streams.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, version string) error {
	return RunWithStdin(ctx, args, os.Stdin, stdout, stderr, version)
}

// RunWithStdin is Run with an explicit standard input, which `conch login`
// reads the token from.
func RunWithStdin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, version string) error {
	if len(args) == 0 {
		Usage(stderr)
		return errors.New("cli: no command given")
	}
	switch args[0] {
	case "send":
		return runSend(ctx, args[1:], stderr)
	case "tail":
		return runTail(ctx, args[1:], stdout, stderr)
	case "approvals":
		return runApprovals(ctx, args[1:], stdout, stderr)
	case "approve":
		return runApprovalsDecision(ctx, args[1:], stdout, stderr, "approve")
	case "reject":
		return runApprovalsDecision(ctx, args[1:], stdout, stderr, "reject")
	case "login":
		return runLogin(ctx, args[1:], stdin, stdout, stderr)
	case "logout":
		return runLogout(args[1:], stderr)
	case "whoami":
		return runWhoAmI(ctx, args[1:], stdout, stderr)
	case "version":
		_, err := fmt.Fprintln(stdout, version)
		return err
	case "-h", "--help", "help":
		Usage(stdout)
		return nil
	default:
		Usage(stderr)
		return fmt.Errorf("cli: unknown command %q", args[0])
	}
}

// Usage writes the command-line help text.
func Usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `conch — the Conch command-line client

Usage:
  conch send [--server <url>] [--author <id>] <channel> <text>
  conch tail [--server <url>] <channel>
  conch approvals list [--server <url>]
  conch approve [flags] <id>
  conch reject [flags] <id>
  conch login [--server <url>]     (reads the token from stdin: conch login < tokenfile)
  conch logout [--server <url>]
  conch whoami [--server <url>]
  conch version

Environment:
  CONCH_SERVER  server URL (default http://127.0.0.1:8080)
  CONCH_TOKEN   bearer token; overrides the stored login
  CONCH_AUTHOR  author ID (deprecated; with a login the server decides who you are)
  CONCH_CHANNELS optional comma-separated TUI channel override (default: all channels from the server)
`)
}

func runSend(ctx context.Context, args []string, stderr io.Writer) error {
	fs := newFlagSet("send", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	author := fs.String("author", os.Getenv("CONCH_AUTHOR"), "message author ID")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: send: %w", err)
	}
	if fs.NArg() != 2 {
		return errors.New("cli: send: expected <channel> <text>")
	}
	client, hasCredential, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	authorID, err := resolveAuthor(ctx, client, hasCredential, *author, "send", stderr)
	if err != nil {
		return err
	}
	_, err = client.Send(ctx, fs.Arg(0), authorID, fs.Arg(1))
	return err
}

func runTail(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("tail", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: tail: %w", err)
	}
	if fs.NArg() != 1 {
		return errors.New("cli: tail: expected <channel>")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	err = client.Tail(ctx, fs.Arg(0), func(message schema.MessageV0) error {
		body := strings.NewReplacer("\\", "\\\\", "\r", "\\r", "\n", "\\n").Replace(message.Body)
		_, writeErr := fmt.Fprintf(stdout, "%s %d %s\n", message.CreatedAt.Format(time.RFC3339Nano), message.AuthorID, body)
		return writeErr
	})
	if websocket.CloseStatus(err) == websocket.StatusGoingAway {
		_, _ = fmt.Fprintln(stderr, "conch: server shutting down")
		return nil
	}
	// Interrupting a tail is how a user stops it; that is not a failure.
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func serverEnvOr() string {
	if value := os.Getenv("CONCH_SERVER"); value != "" {
		return value
	}
	return defaultServer
}

func runApprovals(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cli: approvals requires a subcommand (e.g. list)")
	}
	switch args[0] {
	case "list":
		return runApprovalsList(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("cli: unknown approvals subcommand %q", args[0])
	}
}

func runApprovalsList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("approvals list", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: approvals list: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("cli: approvals list takes no positional arguments")
	}

	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}

	resp, err := client.ListApprovals(ctx)
	if err != nil {
		return err
	}

	if len(resp.Approvals) == 0 {
		return nil
	}

	_, _ = fmt.Fprintf(stdout, "%-6s | %-12s | %-20s | %-20s | %s\n", "ID", "STATE", "DEADLINE", "REQUESTER", "TITLE")
	_, _ = fmt.Fprintln(stdout, strings.Repeat("-", 90))
	for _, a := range resp.Approvals {
		deadline := a.Deadline.Time().Format(time.RFC3339)
		_, _ = fmt.Fprintf(stdout, "%-6d | %-12s | %-20s | %-20d | %s\n", a.ID, a.State, deadline, a.RequesterID, a.Title)
	}
	return nil
}

func runApprovalsDecision(ctx context.Context, args []string, stdout, stderr io.Writer, defaultOption string) error {
	fs := newFlagSet(defaultOption, stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	author := fs.String("author", os.Getenv("CONCH_AUTHOR"), "human principal ID")
	reason := fs.String("reason", "", "reason for decision")
	optionID := fs.String("option", defaultOption, "option ID to select")

	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: %s: %w", defaultOption, err)
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("cli: %s: expected <id>", defaultOption)
	}

	approvalID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || approvalID <= 0 {
		return fmt.Errorf("cli: %s: id must be a positive integer", defaultOption)
	}

	if *reason == "" {
		return fmt.Errorf("cli: %s: --reason is required", defaultOption)
	}

	client, hasCredential, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	principalID, err := resolveAuthor(ctx, client, hasCredential, *author, defaultOption, stderr)
	if err != nil {
		return err
	}

	resp, err := client.CastDecision(ctx, approvalID, schema.CastDecisionRequestV1{
		PrincipalID: principalID,
		OptionID:    *optionID,
		Reason:      *reason,
	})
	if err != nil {
		// Rewrite into a single clear message rather than writing to stderr
		// directly: cmd/conch's Run wrapper already prints the returned
		// error, so writing here as well would print it twice.
		if strings.Contains(err.Error(), "invalid_state") || strings.Contains(err.Error(), "terminal") {
			return fmt.Errorf("approval %d is no longer open", approvalID)
		}
		if strings.Contains(err.Error(), "approval_not_found") || strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("approval %d not found", approvalID)
		}
		return err
	}

	if resp.Resolution != nil {
		_, _ = fmt.Fprintf(stdout, "conch: approval %d resolved (%s)\n", approvalID, resp.State)
	} else {
		_, _ = fmt.Fprintf(stdout, "conch: approval %d decision recorded (state: %s)\n", approvalID, resp.State)
	}

	return nil
}

// NewAuthClient builds a client for server that carries the stored or
// CONCH_TOKEN credential when there is one. The bool reports whether a
// credential is in use.
func NewAuthClient(server string) (*Client, bool, error) {
	client, err := NewClient(server, nil)
	if err != nil {
		return nil, false, err
	}
	token, err := LoadToken(server)
	if err != nil {
		return nil, false, err
	}
	client.WithToken(token)
	return client, token != "", nil
}

// resolveAuthor decides which principal an action is performed as. With a
// credential the server's whoami answer is authoritative and --author may only
// repeat it; without one the legacy --author / CONCH_AUTHOR rules apply.
func resolveAuthor(ctx context.Context, client *Client, hasCredential bool, flagValue, command string, stderr io.Writer) (int64, error) {
	var asserted int64
	if flagValue != "" {
		id, err := strconv.ParseInt(flagValue, 10, 64)
		if err != nil || id <= 0 {
			return 0, fmt.Errorf("cli: %s: author must be a positive integer", command)
		}
		asserted = id
	}
	if !hasCredential {
		if asserted == 0 {
			return 0, fmt.Errorf("cli: %s: --author (or CONCH_AUTHOR) is required", command)
		}
		return asserted, nil
	}
	who, err := client.WhoAmI(ctx)
	if err != nil {
		return 0, err
	}
	if asserted != 0 {
		if asserted != who.ID {
			return 0, fmt.Errorf("cli: %s: --author %d does not match your login (principal %d)", command, asserted, who.ID)
		}
		_, _ = fmt.Fprintln(stderr, "warning: --author is deprecated; identity comes from your login")
	}
	return who.ID, nil
}

func runLogin(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("login", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: login: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("cli: login takes no positional arguments; the token is read from stdin")
	}
	client, err := NewClient(*server, nil)
	if err != nil {
		return err
	}
	token, err := readToken(ctx, stdin, stderr)
	if err != nil {
		return err
	}
	who, err := client.WithToken(token).WhoAmI(ctx)
	if errors.Is(err, ErrUnauthenticated) {
		return errors.New("login failed: token rejected (or the server is not running with --auth required)")
	}
	if err != nil {
		return err
	}
	if err := SaveToken(*server, token); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "logged in to %s as %s (%s)\n", client.Server(), who.Name, who.Role)
	return err
}

// Seams for the terminal branch of readToken, so tests can drive it without a
// real terminal. isTerminalFD reports whether fd is a terminal; readSecret
// reads one line from it with echo off, and must restore the terminal before it
// returns.
var (
	isTerminalFD = term.IsTerminal
	readSecret   = readSecretNoEcho
)

// readSecretNoEcho reads a line from the terminal fd with echo disabled.
// term.ReadPassword restores the terminal when it returns, but it does not
// notice an interrupt: main turns SIGINT into a context cancel, so Ctrl-C would
// be swallowed and the prompt would hang. On cancel the saved state is put back
// here and the blocked read is abandoned; the process is about to exit, so the
// goroutine does not outlive anything that matters.
//
// The abandoned goroutine can still write the terminal state once: turning
// echo off is ReadPassword's first act. If the cancel arrived before that
// happened, restoring at once would be undone a moment later and the user's
// shell would be left without echo. So the cancel branch waits until the
// goroutine has reached ReadPassword, allows it a moment to apply the change,
// and only then restores. A context that is already cancelled never starts
// the read at all.
func readSecretNoEcho(ctx context.Context, fd int) ([]byte, error) {
	state, err := term.GetState(fd)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		secret []byte
		err    error
	}
	done := make(chan result, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		secret, err := term.ReadPassword(fd)
		done <- result{secret, err}
	}()
	select {
	case r := <-done:
		return r.secret, r.err
	case <-ctx.Done():
		select {
		case <-started:
		case <-time.After(echoSettle * 10):
		}
		time.Sleep(echoSettle)
		_ = term.Restore(fd, state)
		return nil, ctx.Err()
	}
}

// echoSettle is how long the cancel branch of readSecretNoEcho lets the reader
// goroutine apply its echo-off before restoring the terminal over it. The
// change is a single ioctl made immediately after the goroutine starts.
const echoSettle = 20 * time.Millisecond

// readToken reads one line from stdin. On a terminal it prompts on stderr and
// reads without echo; it never falls back to reading with echo on, because the
// token is the whole credential and would land in scrollback and recordings.
func readToken(ctx context.Context, stdin io.Reader, stderr io.Writer) (string, error) {
	var line string
	if f, ok := stdin.(*os.File); ok && isTerminalFD(int(f.Fd())) { //nolint:gosec // fds fit in int
		_, _ = fmt.Fprint(stderr, "Token: ")
		secret, err := readSecret(ctx, int(f.Fd())) //nolint:gosec // fds fit in int
		// The user's Enter was not echoed either, so end the prompt line.
		_, _ = fmt.Fprintln(stderr)
		switch {
		case errors.Is(err, io.EOF):
			// Ctrl-D on an empty line: same as an empty stdin.
		case ctx.Err() != nil:
			return "", ctx.Err()
		case err != nil:
			return "", fmt.Errorf("cli: login: cannot read the token without echo (%w); use 'conch login < tokenfile'", err)
		}
		line = string(secret)
	} else {
		var err error
		line, err = bufio.NewReader(io.LimitReader(stdin, 64<<10)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("cli: login: read token: %w", err)
		}
	}
	token := strings.TrimSpace(line)
	if token == "" {
		return "", errors.New("cli: login: no token provided on stdin")
	}
	return token, nil
}

func runLogout(args []string, stderr io.Writer) error {
	fs := newFlagSet("logout", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: logout: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("cli: logout takes no positional arguments")
	}
	return DeleteToken(*server)
}

func runWhoAmI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("whoami", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: whoami: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("cli: whoami takes no positional arguments")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	who, err := client.WhoAmI(ctx)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "id: %d\nkind: %s\nname: %s\nrole: %s\n", who.ID, who.Kind, who.Name, who.Role)
	return err
}
