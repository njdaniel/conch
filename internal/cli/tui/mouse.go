package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// mouse handles one mouse event. The mouse only selects, focuses and scrolls:
// it never sends a message, casts a decision or quits. The one command it can
// return is the one selecting a channel starts (its history and
// subscription), or the read mark that coming back to the newest message
// moves. It finds what is under the pointer with layout, which View draws
// from, so what is hit is what is drawn.
func (m Model) mouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress || m.help {
		// The help covers the panes: there is nothing under it to point at.
		return m, nil
	}
	if m.mode == modeAgents || m.mode == modeRun || m.mode == modeRuns {
		// Lists and a pane that are read with the keys; there is nothing to click.
		return m, nil
	}
	if width, height := m.size(); width < minWidth || height < minHeight {
		// View shows the too-small screen, with nothing to point at.
		return m, nil
	}
	if m.mode != modeChannels {
		// Below this size View shows tooSmall, with nothing to point at.
		if width, height := m.size(); width < minApprovalWidth || height < minApprovalHeight {
			return m, nil
		}
	}
	l := m.layout()
	switch msg.Button {
	case tea.MouseButtonLeft:
		return m.click(l, msg.X, msg.Y)
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		step := 1
		if msg.Button == tea.MouseButtonWheelDown {
			step = -1
		}
		switch m.mode {
		case modeChannels:
			if l.inRight(msg.X, msg.Y) {
				return m.scrolled(step)
			}
		case modeInbox:
			if l.inLeft(msg.X, msg.Y) {
				m.selApproval = min(max(0, m.selApproval-step), max(0, len(m.approvals)-1))
				m.bodyOffset = 0
			}
		}
	}
	return m, nil
}

// click selects what is at (x, y), as the arrow keys would. In the chat view a
// click on a pane also gives it the focus, and one on the way back to the
// newest message in the message pane's bottom border takes it.
func (m Model) click(l layout, x, y int) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeChannels:
		switch {
		case l.inLeft(x, y):
			m.focus = focusChannels
			if row, ok := l.leftContent(x, y); ok {
				if i := m.channelStart(l.contentH) + row; i < len(m.channels) {
					return m.selectChannel(i - m.selected)
				}
			}
		case l.rightFoot(x, y) && m.scroll > 0:
			m.focus = focusMessages
			return m.scrolled(-m.scroll)
		case l.inRight(x, y):
			m.focus = focusMessages
		}
	case modeInbox:
		if row, ok := l.leftContent(x, y); ok {
			if i, ok := m.approvalAt(row, l.contentH); ok {
				m.selApproval = i
				m.bodyOffset = 0
			}
		}
	case modeDecision:
		if row, ok := l.rightContent(x, y); ok {
			if d, ok := m.selectedDetails(); ok {
				if i, ok := d.optionRows[row]; ok {
					m.selOption = i
				}
			}
		}
	}
	return m, nil
}
