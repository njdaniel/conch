package main

import (
	"context"
	"errors"
)

// stopCtx is cancelled by the first SIGINT or SIGTERM. Every child process is
// started under it and every wait in this program watches it, so a signal
// makes the run unwind through its normal exit path, in which teardown runs
// once and after nothing is in flight. Teardown is never run from the signal
// handler, because a child being started at that moment would escape it.
// Children are started in process groups of their own (lkit.GroupCommand,
// lkit.GroupStart) and killed as groups (lkit.KillGroup).
var stopCtx, stopCancel = context.WithCancel(context.Background())

// errInterrupted is what a wait returns once stopCtx is cancelled.
var errInterrupted = errors.New("interrupted by a signal")
