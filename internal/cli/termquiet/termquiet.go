// Package termquiet keeps the plain conch commands from querying the terminal.
//
// Bubble Tea v1 has a package init that calls lipgloss.HasDarkBackground. That
// makes termenv write an OSC 11 background query and a cursor-position query
// (CSI 6 n) to the terminal and wait up to five seconds for the replies. The
// cmd/conch binary links Bubble Tea, so the init runs for every command, not
// only the TUI: on a terminal that does not answer, "conch whoami" stalls, a
// late reply lands in the input "conch login" reads, and Ctrl-C during the
// stall leaves the terminal without echo.
//
// The query is skipped when TERM is dumb, screen*, or tmux*, and termenv reads
// TERM at query time. So this package's init sets TERM=dumb for any
// invocation that is not the TUI, and main calls Restore as its first act,
// after every package init has run.
//
// This relies on Go's package initialization order: a package is initialized
// once its dependencies are, taking the first ready package in import-path
// order. This package imports only os, so it is ready from the start, and
// Bubble Tea depends on packages that sort after this one's path (log,
// os/signal, text/template), so this init always runs before Bubble Tea's.
// What would break it: giving this package an import that depends on Bubble
// Tea, Lip Gloss, or termenv, or a Bubble Tea release that queries elsewhere
// than in a package init. The pseudo-terminal tests in cmd/conch
// (TestPlainCommandsDoNotQueryTheTerminal and friends) pin this.
//
// The TUI (no arguments) is left alone, so it still learns the real
// background through the same query, exactly as before.
package termquiet

import "os"

var (
	savedTerm   string
	termWasSet  bool
	needRestore bool
)

func init() {
	if len(os.Args) <= 1 {
		return // the TUI: it wants the real answer
	}
	savedTerm, termWasSet = os.LookupEnv("TERM")
	needRestore = true
	_ = os.Setenv("TERM", "dumb")
}

// Restore puts TERM back as the user had it. Call it first thing in main.
func Restore() {
	if !needRestore {
		return
	}
	needRestore = false
	if termWasSet {
		_ = os.Setenv("TERM", savedTerm)
	} else {
		_ = os.Unsetenv("TERM")
	}
}
