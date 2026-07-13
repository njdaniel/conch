// Command conch is the plain scriptable client for conchd's public REST/WS API.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/njdaniel/conch/internal/cli"
)

var version = "v0.0.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr, version); err != nil {
		fmt.Fprintln(os.Stderr, "conch:", err)
		os.Exit(1)
	}
}
