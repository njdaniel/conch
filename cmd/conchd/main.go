// Command conchd is the Conch server: SQLite-backed message log, REST/WS API,
// and MCP endpoint for agents. See ROADMAP.md. P0 provides config, the serve
// command, a health endpoint, and graceful shutdown.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
)

var version = "v0.0.0-dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "conchd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no command given")
	}
	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage(w *os.File) {
	_, _ = fmt.Fprint(w, `conchd — the Conch server

Usage:
  conchd serve [--data <dir>] [--listen <addr>]
  conchd version

Flags for serve:
  --data    directory for the SQLite database (env CONCHD_DATA)
  --listen  HTTP listen address (env CONCHD_LISTEN, default :8080)
`)
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	dataDir := fs.String("data", os.Getenv("CONCHD_DATA"), "directory for the SQLite database")
	listen := fs.String("listen", envOr("CONCHD_LISTEN", ":8080"), "HTTP listen address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		return errors.New("serve: --data (or CONCHD_DATA) is required")
	}

	// The data directory is an operator-supplied path by design; conchd runs
	// with the operator's own privileges, so this is configuration, not a
	// traversal vector.
	if err := os.MkdirAll(*dataDir, 0o750); err != nil { // #nosec G301,G703 -- trusted operator path
		return fmt.Errorf("serve: create data dir: %w", err)
	}

	// Signal-aware context: the first SIGINT/SIGTERM triggers graceful
	// shutdown. Once that fires, unregister the handler so a second signal
	// gets default handling and can force-kill a stuck drain.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	context.AfterFunc(ctx, stop)

	st, err := store.Open(ctx, filepath.Join(*dataDir, "conch.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	srv := server.New(server.Config{
		DataDir: *dataDir,
		Listen:  *listen,
		Version: version,
	}, st)
	if err := srv.Listen(); err != nil {
		return err
	}

	fmt.Printf("conchd %s listening on %s (data %s)\n", version, srv.Addr(), *dataDir)
	return srv.Serve(ctx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
