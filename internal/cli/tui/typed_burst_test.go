package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Several characters typed within one read of the terminal reach the model as
// one KeyRunes message whose String() is the characters. Words that are also
// the names of keys ("up", "end", "tab", "esc"...) must still be text: issue
// #1156 was a draft, "p309interrupted", that lost its "up" and moved the
// selection to the channel above while it was typed.
func TestTypedBurstIsTextNotAKeyName(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"up", "up"},
		{"down", "down"},
		{"left", "left"},
		{"right", "right"},
		{"home", "home"},
		{"end", "end"},
		{"pgup", "pgup"},
		{"pgdown", "pgdown"},
		{"tab", "tab"},
		{"esc", "esc"},
		{"enter", "enter"},
		{"backspace", "backspace"},
		{"ctrl+c", "ctrl+c"},
		{"a word with up in it", "p309interrupted"},
		{"a name with a key after it", "type tab then end"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, _, _ := resubModel(t)
			m, _ = update(m, tea.KeyMsg{Type: tea.KeyDown}) // ops, the second channel
			m.input = ""
			if m.current() != "ops" {
				t.Fatalf("setup: channel = %q, want ops", m.current())
			}
			mode := m.mode
			m, cmd := update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tt.text)})
			if m.current() != "ops" {
				t.Errorf("typing %q moved the selection to %q", tt.text, m.current())
			}
			if m.mode != mode {
				t.Errorf("typing %q changed the view from %v to %v", tt.text, mode, m.mode)
			}
			if m.input != tt.text {
				t.Errorf("draft after typing %q = %q", tt.text, m.input)
			}
			if cmd != nil {
				t.Errorf("typing %q started a command (a quit, or a channel load)", tt.text)
			}
		})
	}
}

// A real arrow key is still a key, and text pasted as a unit is still text.
func TestTypedBurstLeavesKeysAndPastesAlone(t *testing.T) {
	m, _, _ := resubModel(t)
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyDown})
	if m.current() != "ops" {
		t.Fatalf("down: channel = %q, want ops", m.current())
	}
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("up"), Paste: true})
	if m.current() != "ops" || m.input != "up" {
		t.Errorf("pasted \"up\": channel %q, draft %q; want ops and the text", m.current(), m.input)
	}
	m, _ = update(m, tea.KeyMsg{Type: tea.KeyUp})
	if m.current() != "general" {
		t.Errorf("the up arrow left the selection on %q, want general", m.current())
	}
}
