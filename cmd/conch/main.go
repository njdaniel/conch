// Command conch is the plain scriptable client for conchd's public REST/WS API.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/cli/tui"
)

var version = "v0.0.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var err error
	if len(os.Args) == 1 {
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
	server := os.Getenv("CONCH_SERVER")
	if server == "" {
		server = "http://127.0.0.1:8080"
	}
	client, err := cli.NewClient(server, nil)
	if err != nil {
		return err
	}
	var authorID int64
	if raw := os.Getenv("CONCH_AUTHOR"); raw != "" {
		authorID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || authorID <= 0 {
			return fmt.Errorf("CONCH_AUTHOR must be a positive integer")
		}
	}
	channels := strings.Split(os.Getenv("CONCH_CHANNELS"), ",")
	return tui.Run(ctx, client, authorID, channels, os.Stdin, os.Stdout)
}
