// Command conch-bot is the old name of conch-adapter, kept for one release. It
// says so on stderr and runs the same program, compiled in: it never looks for
// another binary.
package main

import (
	"fmt"
	"os"

	"github.com/njdaniel/conch/internal/adapter"
)

func main() {
	fmt.Fprintln(os.Stderr, "conch-bot: deprecated, use conch-adapter; this name will be removed in the next release")
	adapter.Main()
}
