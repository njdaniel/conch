//go:build linux

package adapter

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHardenProcessHidesTheEnvironment runs this test binary again as a
// stand-in adapter that holds a token, and the stand-in starts a child that
// reads the stand-in's environment from /proc, as a runtime asked to find the
// token would. Not hardened, the child finds it (the control: /proc is open
// here); hardened, it must not. Each stand-in is a process of its own, so the
// rest of the package keeps a dumpable test binary.
func TestHardenProcessHidesTheEnvironment(t *testing.T) {
	const mode = "CONCH_TEST_HARDEN_MODE"
	switch os.Getenv(mode) {
	case "harden":
		hardenProcess()
		fallthrough
	case "plain":
		out, _ := exec.Command("sh", "-c", `cat /proc/$PPID/environ`).CombinedOutput() // #nosec G204 -- a fixed command
		_, _ = os.Stdout.Write(out)
		os.Exit(0)
	}
	read := func(m string) string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHardenProcessHidesTheEnvironment$") // #nosec G204 G702 -- this test binary, run again
		cmd.Env = append(os.Environ(), "CONCH_ADAPTER_TOKEN=secret-harden-probe", mode+"="+m)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("stand-in (%s): %v\n%s", m, err, out)
		}
		return string(out)
	}
	if !strings.Contains(read("plain"), "secret-harden-probe") {
		t.Skip("/proc/<ppid>/environ is not readable here even without hardening; the protection cannot be measured")
	}
	if strings.Contains(read("harden"), "secret-harden-probe") {
		t.Fatal("a child read the hardened adapter's token from /proc/<ppid>/environ")
	}
}
