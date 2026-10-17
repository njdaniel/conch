//go:build !unix

package adapter

import (
	"errors"
	"os/exec"
	"time"
)

// ownProcessGroup is a no-op where there are no process groups; cancelling the
// context kills only the process the adapter started.
func ownProcessGroup(*exec.Cmd) {}

func waitGroupGone(int, time.Duration) {}

// canPauseGroup is false: there is no SIGSTOP here, so the adapter does not
// declare pause and resume and the server keeps refusing them.
const canPauseGroup = false

var errNoPause = errors.New("pause is not supported on this platform")

func stopGroup(int) error     { return errNoPause }
func continueGroup(int) error { return errNoPause }
