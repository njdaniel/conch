package tui

import (
	"context"
	"io"

	tea "github.com/charmbracelet/bubbletea"
)

// Run starts the full-screen terminal application.
func Run(ctx context.Context, api API, authorID int64, channels []string, input io.Reader, output io.Writer) error {
	model := NewModel(ctx, api, authorID, channels)
	_, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(input), tea.WithOutput(output), tea.WithAltScreen()).Run()
	return err
}
