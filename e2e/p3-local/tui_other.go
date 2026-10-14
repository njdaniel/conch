//go:build !linux

package main

import "errors"

var errNoTerminal = errors.New("this system has no pseudo-terminal the suite can use")

// startTUI cannot start a terminal client here: the suite drives it on a
// Linux pseudo-terminal.
func (s *suite) startTUI(string) (*tui, error) { return nil, errNoTerminal }
