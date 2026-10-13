package reviewpost

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// recordingSystemctl stands in for systemctl: it records its arguments and
// prints a line a test can look for. HOME is the test's own, so the unit
// files land in a temporary directory.
const recordingSystemctl = `printf 'systemctl %s\n' "$*" >>"$CALLS"
echo "stub systemctl: $*"
`

// install copies the two units into the user's systemd directory, links the
// script into ~/bin under the name the service starts, and enables the timer.
func TestInstall(t *testing.T) {
	env := newScriptEnv(t)
	env.stub("systemctl", recordingSystemctl)

	res := env.run(t.TempDir(), "install")
	if res.code != 0 {
		t.Fatalf("install exited %d: %s", res.code, res.stderr)
	}

	unitDir := filepath.Join(env.home, ".config", "systemd", "user")
	for _, unit := range []string{"conch-principal-review.service", "conch-principal-review.timer"} {
		if got, want := readFile(t, filepath.Join(unitDir, unit)), readFile(t, filepath.Join(repoRoot, "ops", "systemd", unit)); got != want {
			t.Errorf("%s was not installed as the repository has it", unit)
		}
	}
	link, err := os.Readlink(filepath.Join(env.home, "bin", "conch-principal-review"))
	if err != nil {
		t.Fatal(err)
	}
	if link != scriptPath {
		t.Errorf("~/bin/conch-principal-review points at %s, want %s", link, scriptPath)
	}
	wantCalls := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable --now conch-principal-review.timer",
		"systemctl --user list-timers conch-principal-review.timer --no-pager",
	}
	if got := env.callLog(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("calls = %q, want %q", got, wantCalls)
	}
	if !strings.Contains(res.stdout, "Logs land in "+env.logDir) {
		t.Errorf("install did not say where the logs go: %s", res.stdout)
	}
}

// The units themselves: a weekly, persistent timer, and a service that
// starts what install linked.
func TestUnits(t *testing.T) {
	timer := readFile(t, filepath.Join(repoRoot, "ops", "systemd", "conch-principal-review.timer"))
	for _, want := range []string{"\nOnCalendar=Mon *-*-* 07:00:00\n", "\nPersistent=true\n", "\nWantedBy=timers.target\n"} {
		if !strings.Contains(timer, want) {
			t.Errorf("the timer lacks %q", strings.TrimSpace(want))
		}
	}
	service := readFile(t, filepath.Join(repoRoot, "ops", "systemd", "conch-principal-review.service"))
	for _, want := range []string{"\nType=oneshot\n", "\nExecStart=%h/bin/conch-principal-review run\n"} {
		if !strings.Contains(service, want) {
			t.Errorf("the service lacks %q", strings.TrimSpace(want))
		}
	}
}

// status shows the timer and the service, then the end of the latest log.
func TestStatus(t *testing.T) {
	env := newScriptEnv(t)
	env.stub("systemctl", recordingSystemctl)
	writeFile(t, filepath.Join(env.logDir, "review-20261005-070000.log"), "last week's run\n")
	writeFile(t, filepath.Join(env.logDir, "review-20261012-070003.log"), "this week's run\nsuccess: review 2026-10-12\n")

	res := env.run(t.TempDir(), "status")
	if res.code != 0 {
		t.Fatalf("status exited %d: %s", res.code, res.stderr)
	}
	wantCalls := []string{"systemctl --user status conch-principal-review.timer conch-principal-review.service --no-pager"}
	if got := env.callLog(); !reflect.DeepEqual(got, wantCalls) {
		t.Errorf("calls = %q, want %q", got, wantCalls)
	}
	for _, want := range []string{
		"stub systemctl: --user status",
		"== last log: " + filepath.Join(env.logDir, "review-20261012-070003.log") + " ==",
		"success: review 2026-10-12",
	} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("status output lacks %q:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "last week's run") {
		t.Errorf("status showed an older log:\n%s", res.stdout)
	}
}

// Before the first run there is no log to show, which is not an error.
func TestStatusBeforeAnyRun(t *testing.T) {
	env := newScriptEnv(t)
	env.stub("systemctl", recordingSystemctl+"exit 3\n") // what systemctl status says of an inactive unit
	res := env.run(t.TempDir(), "status")
	if res.code != 0 {
		t.Fatalf("status exited %d: %s", res.code, res.stderr)
	}
	if strings.Contains(res.stdout, "last log") {
		t.Errorf("status showed a log where there is none:\n%s", res.stdout)
	}
}
