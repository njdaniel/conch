package tui

import (
	"context"
	"errors"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the full-screen terminal application and returns when the user
// quits or ctx ends.
//
// ctx is the one way to stop it from outside, and the caller owns the signals:
// to end the TUI on SIGINT or SIGTERM, cancel ctx on them. Bubble Tea's own
// signal handler is switched off. With both in play a signal cancelled ctx,
// which stops the event loop, while that handler was sending the loop a
// message nobody was left to read; Run then waited for the handler for ever
// (issue #192).
//
// mouse turns on mouse reporting: a click selects and the wheel scrolls. The
// terminal then keeps its own click-and-drag text selection for itself unless
// Shift is held, which is why callers can turn it off (CONCH_MOUSE=0). The
// mouse is only ever an addition to the keyboard.
//
// While it runs the TUI holds a session of its own (client "tui") on the
// server, which is what makes it appear in other clients' rosters. It is
// ended on the way out, signals included; if the server cannot be reached then
// it is left to the server's own staleness thresholds.
//
// voice, when not nil, gives the TUI its /voice commands (VoiceSetupFor). The
// helper they start is the TUI's own and ends with it: on the way out, by a
// key or ctx alike, Run stops it and waits, for at most the contract's
// StopKill bound and a second, before it ends the session. If the TUI is
// killed outright the helper notices its owner has gone and stops by itself.
//
// Being asked to stop is not a failure: once ctx has ended Run restores the
// terminal and returns nil, as it does when the user quits with a key.
func Run(ctx context.Context, api API, authorID int64, authenticated bool, channels []string, mouse bool, voice *VoiceSetup, input io.Reader, output io.Writer) error {
	model := NewModel(ctx, api, authorID, channels)
	if authenticated {
		model = model.WithCredential()
	}
	if voice != nil {
		model = model.WithVoice(*voice)
	}
	// The TUI's own session ends with the program, whatever ended it: a key, or
	// ctx on SIGINT or SIGTERM. It is ended after the terminal is given back,
	// with a context of its own because ctx may be the thing that ended.
	defer model.endOwnSession()
	// Voice stops first (deferred last), the same way.
	defer model.closeVoice()
	_, err := tea.NewProgram(model, programOptions(ctx, input, output, mouse)...).Run()
	if cause := ctx.Err(); cause != nil && errors.Is(err, cause) {
		// Bubble Tea reports the end of its context as "program was killed:
		// context canceled". A panic in the program is reported differently and
		// still comes back as an error.
		return nil
	}
	return err
}

// programOptions is how Run starts Bubble Tea. Mouse reporting is the only
// part that depends on mouse.
func programOptions(ctx context.Context, input io.Reader, output io.Writer, mouse bool) []tea.ProgramOption {
	options := []tea.ProgramOption{tea.WithContext(ctx), tea.WithoutSignalHandler(), tea.WithInput(input), tea.WithOutput(output), tea.WithAltScreen()}
	if mouse {
		options = append(options, tea.WithMouseCellMotion())
	}
	return options
}
