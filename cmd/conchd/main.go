// Command conchd is the Conch server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/njdaniel/conch/internal/server"
)

var version = "v0.0.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: conchd <serve|version>")
	}

	switch args[0] {
	case "version":
		if len(args) != 1 {
			return errors.New("usage: conchd version")
		}
		_, err := fmt.Fprintf(stdout, "conchd %s\n", version)
		return err
	case "serve":
		return runServe(ctx, args[1:], stderr)
	default:
		return fmt.Errorf("unknown command %q; usage: conchd <serve|version>", args[0])
	}
}

func runServe(ctx context.Context, args []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dataDir := flags.String("data", "./data", "directory containing conchd data")
	listenAddr := flags.String("listen", "127.0.0.1:8080", "HTTP listen address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve: unexpected arguments: %v", flags.Args())
	}

	srv, err := server.New(ctx, server.Config{DataDir: *dataDir, Version: version})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", *listenAddr)
	if err != nil {
		_ = srv.Close()
		return fmt.Errorf("listen on %s: %w", *listenAddr, err)
	}
	if _, err := fmt.Fprintf(stderr, "conchd %s listening on %s\n", version, listener.Addr()); err != nil {
		_ = listener.Close()
		_ = srv.Close()
		return fmt.Errorf("write startup message: %w", err)
	}
	return srv.Serve(ctx, listener)
}
