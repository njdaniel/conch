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
  conch version

Environment:
  CONCH_SERVER  server URL (default http://127.0.0.1:8080)
  CONCH_AUTHOR  author ID for send
  CONCH_CHANNELS comma-separated TUI channels (default general)
`)
}

func runSend(ctx context.Context, args []string, stderr io.Writer) error {
	fs := newFlagSet("send", stderr)
	server := fs.String("server", envOr("CONCH_SERVER", defaultServer), "conchd HTTP URL")
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
	server := fs.String("server", envOr("CONCH_SERVER", defaultServer), "conchd HTTP URL")
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

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
