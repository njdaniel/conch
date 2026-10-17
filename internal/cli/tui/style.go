package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The style tokens. Colours are the terminal's own 16-colour palette, so they
// follow its theme on a dark or a light background. None of them carries a
// meaning alone: with NO_COLOR, a monochrome terminal or a pipe, lipgloss
// draws no styling at all (not even bold), and what each state means is still
// there in text: "›" for the selection, "▸" before the title of the pane the
// arrow keys move, "[agent]" and the other badges, ●◐○ with a word beside
// them, and "[Chat]" in brackets for the view on screen. docs/design/
// tui-polish.md lists them.
var (
	// borderStyle draws a pane the keys are not acting on; focusBorderStyle
	// the one they are.
	borderStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("8"))
	focusBorderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("6"))
	// activeStyle is the selection and headings that matter.
	activeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	// badgeStyle is every badge: [agent], a scope, a payload, a count.
	badgeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("3"))
	// statusStyle is secondary text: times, hints, empty states.
	statusStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	// titleStyle is a pane's title in its top border and the app name.
	titleStyle = lipgloss.NewStyle().Bold(true)
	// authorStyle is who wrote a message.
	authorStyle = lipgloss.NewStyle().Bold(true)
	// okStyle and warnStyle say a connection is up or is not.
	okStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// focusMark starts the title of the pane the arrow keys act on.
const focusMark = "▸ "

// paneBox draws lines in a rounded pane outerW cells wide with contentH rows
// inside, and title in its top border. The pane the keys act on has a border
// of another colour and focusMark before its title, so focus does not rest on
// colour. foot, when there is one, goes in the bottom border. The lines must
// already fit the inner width; more than contentH of them are cut.
func paneBox(title string, focused bool, lines []string, outerW, contentH int, foot string) string {
	if len(lines) > contentH {
		lines = lines[:contentH]
	}
	style := borderStyle
	if focused {
		style = focusBorderStyle
	}
	box := strings.Split(style.Width(outerW-2).Height(contentH).Render(strings.Join(lines, "\n")), "\n")
	edge := lipgloss.NewStyle().Foreground(style.GetBorderTopForeground())
	if focused {
		title = focusMark + title
	}
	if top := borderLabel("╭", "╮", title, outerW, true); top != nil && len(box) > 0 {
		box[0] = edge.Render(top[0]) + titleStyle.Render(top[1]) + edge.Render(top[2])
	}
	if bottom := borderLabel("╰", "╯", foot, outerW, false); bottom != nil && len(box) > 1 {
		box[len(box)-1] = edge.Render(bottom[0]) + activeStyle.Render(bottom[1]) + edge.Render(bottom[2])
	}
	return strings.Join(box, "\n")
}

// borderLabel is a border row outerW cells wide with label set into it, in
// three parts: the run before the label, the label with a space each side,
// and the run after it. A title sits at the left, a foot at the right. It is
// nil for no label, or when the pane is too narrow to show any of it.
func borderLabel(left, right, label string, outerW int, atLeft bool) []string {
	room := outerW - 4 - 2 // the corners, a rule each side and the spaces
	if label == "" || room < 1 {
		return nil
	}
	label = " " + fitCells(label, room) + " "
	rest := outerW - 2 - cellWidth(label)
	before, after := 1, rest-1
	if !atLeft {
		before, after = rest-1, 1
	}
	return []string{left + strings.Repeat("─", before), label, strings.Repeat("─", after) + right}
}

// hint is one key hint of a status line: what to press and what it does.
type hint struct {
	key, does string
	// keep orders the hints by what a narrow line keeps: lower is kept first.
	keep int
}

// hintLine is hints joined in their own order, leaving out those that do not
// fit in budget cells, the highest keep first. A hint that is left out is
// still in the help (F1).
func hintLine(hints []hint, budget int) string {
	shown := make([]bool, len(hints))
	for i := range shown {
		shown[i] = true
	}
	render := func() string {
		var parts []string
		for i, h := range hints {
			if shown[i] {
				parts = append(parts, h.key+" "+h.does)
			}
		}
		if len(parts) == 0 {
			return ""
		}
		return "  " + strings.Join(parts, " • ")
	}
	// The hint kept first is never dropped: on a line too narrow even for it,
	// the caller cuts it rather than show no way out.
	for left := len(hints); left > 1 && cellWidth(render()) > budget; left-- {
		drop := -1
		for i, h := range hints {
			if shown[i] && (drop < 0 || h.keep > hints[drop].keep) {
				drop = i
			}
		}
		shown[drop] = false
	}
	return render()
}
