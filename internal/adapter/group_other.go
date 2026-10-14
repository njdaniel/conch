//go:build !unix

package adapter

import (
	"os/exec"
	"time"
)

// ownProcessGroup is a no-op where there are no process groups; cancelling the
// context kills only the process the adapter started.
func ownProcessGroup(*exec.Cmd) {}

func waitGroupGone(int, time.Duration) {}
