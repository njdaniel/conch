package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procRecord is one process this program started, as pids.json lists it.
type procRecord struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
}

const pidsFile = "pids.json"

// readPIDs reads the processes a run recorded. A root with no pids.json has
// none.
func readPIDs(root string) ([]procRecord, error) {
	data, err := os.ReadFile(filepath.Join(root, pidsFile)) // #nosec G304 -- the run's own root
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var recs []procRecord
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("%s: %w", pidsFile, err)
	}
	return recs, nil
}

func writePIDs(root string, recs []procRecord) error {
	data, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(root, pidsFile), append(data, '\n'), 0o600)
}

// writeFile writes data with exactly mode, whatever the umask: a participant's
// file holds a credential and must not be readable by anyone else.
func writeFile(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// argv returns the command line of pid, or nil when it cannot be read (the
// process is gone, or this system has no /proc).
func argv(pid int) []string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline") // #nosec G304 -- a /proc path built from a number
	if err != nil || len(data) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

// ownsProcess reports whether pid runs a binary this program built into
// root/bin. That is what makes a process ours to stop: a pid that was recorded
// and then reused by an unrelated program does not qualify, and neither does a
// conchd someone started from elsewhere.
func ownsProcess(pid int, root string) bool {
	args := argv(pid)
	if len(args) == 0 {
		return false
	}
	return strings.HasPrefix(args[0], filepath.Join(root, "bin")+string(filepath.Separator))
}

// rootProcesses lists the live processes running a binary from root/bin: the
// server, and the adapters and clients started with the printed commands.
// Without /proc it finds none, and only the recorded processes are stopped.
func rootProcesses(root string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if ownsProcess(pid, root) && processRunning(pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// stopProcess asks pid to stop, and kills it if it has not within grace. It
// returns once the process is gone, or an error if it will not go.
func stopProcess(pid int, grace time.Duration) error {
	if err := signalProcess(pid, true); err != nil {
		return err
	}
	deadline := time.Now().Add(grace)
	for processRunning(pid) {
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !processRunning(pid) {
		return nil
	}
	if err := signalProcess(pid, false); err != nil {
		return err
	}
	for i := 0; i < 100 && processRunning(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if processRunning(pid) {
		return fmt.Errorf("process %d did not stop", pid)
	}
	return nil
}

// stopRootProcesses stops everything this root owns: the recorded processes
// that still run a binary of ours, and any other process running one (a
// server restarted by hand, an adapter started from a printed command).
func stopRootProcesses(root string, grace time.Duration) error {
	recs, err := readPIDs(root)
	if err != nil {
		return err
	}
	seen := make(map[int]bool)
	var errs []error
	stop := func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		if err := stopProcess(pid, grace); err != nil {
			errs = append(errs, err)
		}
	}
	for _, rec := range recs {
		if rec.PID > 0 && ownsProcess(rec.PID, root) && processRunning(rec.PID) {
			stop(rec.PID)
		}
	}
	for _, pid := range rootProcesses(root) {
		stop(pid)
	}
	return errors.Join(errs...)
}
