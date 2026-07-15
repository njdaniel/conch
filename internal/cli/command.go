package cli

import (
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

	"github.com/njdaniel/conch/pkg/schema"
)

const defaultServer = "http://127.0.0.1:8080"

// Run dispatches a conch command using the supplied standard output streams.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer, version string) error {
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
  conch version

Environment:
  CONCH_SERVER  server URL (default http://127.0.0.1:8080)
  CONCH_AUTHOR  author ID for send
  CONCH_CHANNELS comma-separated TUI channels (default general)
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
	if *author == "" {
		return errors.New("cli: send: --author (or CONCH_AUTHOR) is required")
	}
	authorID, err := strconv.ParseInt(*author, 10, 64)
	if err != nil || authorID <= 0 {
		return errors.New("cli: send: author must be a positive integer")
	}
	client, err := NewClient(*server, nil)
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
	client, err := NewClient(*server, nil)
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

	client, err := NewClient(*server, nil)
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

	if *author == "" {
		return fmt.Errorf("cli: %s: --author (or CONCH_AUTHOR) is required", defaultOption)
	}
	principalID, err := strconv.ParseInt(*author, 10, 64)
	if err != nil || principalID <= 0 {
		return fmt.Errorf("cli: %s: author must be a positive integer", defaultOption)
	}

	if *reason == "" {
		return fmt.Errorf("cli: %s: --reason is required", defaultOption)
	}

	client, err := NewClient(*server, nil)
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
