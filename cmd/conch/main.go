// Command conch is the plain scriptable client for conchd's public REST/WS API.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/cli/termquiet"
	"github.com/njdaniel/conch/internal/cli/tui"
)

var version = "v0.0.0-dev"

func main() {
	termquiet.Restore() // after every package init; see the package comment
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	if len(os.Args) <= 1 { // an empty argument list is possible, and is not a command
		err = runTUI(ctx)
	} else {
		err = cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, version)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "conch:", err)
		os.Exit(1)
	}
}

func runTUI(ctx context.Context) error {
	// The TUI holds the terminal in raw mode on the alternate screen, so it
	// has to leave by its own steps on SIGTERM as well as on the SIGINT that
	// main already turns into the end of ctx. These contexts are the only
	// signal handling the TUI has (tui.Run says why), and they stay in place
	// until it has returned: a second signal during the shutdown is absorbed
	// and cannot stop it half-way with the terminal still raw.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stop()
	server := os.Getenv("CONCH_SERVER")
	if server == "" {
		server = "http://127.0.0.1:8080"
	}
	client, authenticated, err := cli.NewAuthClient(server)
	if err != nil {
		return err
	}
	var authorID int64
	// With a credential the server says who the user is (whoami); CONCH_AUTHOR
	// only matters for a server running with auth off.
	if raw := os.Getenv("CONCH_AUTHOR"); raw != "" && !authenticated {
		authorID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || authorID <= 0 {
			return fmt.Errorf("CONCH_AUTHOR must be a positive integer")
		}
	}
	channels := strings.Split(os.Getenv("CONCH_CHANNELS"), ",")
	return tui.Run(ctx, client, authorID, authenticated, channels, os.Stdin, os.Stdout)
}
