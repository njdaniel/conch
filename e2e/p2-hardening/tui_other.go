//go:build !linux

package main

// tuiChannelList is the TUI's part of the channel-list check. Driving the TUI
// needs a pseudo-terminal, which this program opens only on Linux; elsewhere
// the check stops at the client call the TUI makes, and says so.
func (h *harness) tuiChannelList(who *person, _, _ []string) error {
	h.skip("the TUI's channel list as "+who.name, "it needs a pseudo-terminal, which this program opens only on Linux; the client call the TUI makes was checked")
	return nil
}
