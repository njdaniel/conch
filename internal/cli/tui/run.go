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
// Being asked to stop is not a failure: once ctx has ended Run restores the
// terminal and returns nil, as it does when the user quits with a key.
func Run(ctx context.Context, api API, authorID int64, authenticated bool, channels []string, input io.Reader, output io.Writer) error {
	model := NewModel(ctx, api, authorID, channels)
	if authenticated {
		model = model.WithCredential()
	}
	_, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithoutSignalHandler(), tea.WithInput(input), tea.WithOutput(output), tea.WithAltScreen()).Run()
	if cause := ctx.Err(); cause != nil && errors.Is(err, cause) {
		// Bubble Tea reports the end of its context as "program was killed:
		// context canceled". A panic in the program is reported differently and
		// still comes back as an error.
		return nil
	}
	return err
}
