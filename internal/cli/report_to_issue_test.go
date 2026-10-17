package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ops/report-to-issue.sh (issue #446), run with a stand-in conch, gh and
// editor on PATH: it files a type/bug issue only after the editor and a yes,
// with the edited body, and then marks the report filed. The real gh is never
// run.

const fakeMarkdown = "### What you did\n\nJoined voice in [channel]\n\n### What happened instead\n\nSilence\n"

// fakeProgram writes an executable shell script into dir.
func fakeProgram(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o700); err != nil { // #nosec G306 -- a test's own stand-in program
		t.Fatal(err)
	}
}

func TestReportToIssueScript(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Fatal("bash is required to test ops/report-to-issue.sh")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "ops", "report-to-issue.sh"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		// editor is the stand-in editor's script; $1 is the file.
		editor string
		stdin  string
		// filed is whether gh and conch's file were run. wantErr is what the
		// output must say: why nothing was filed, or for a filed report the
		// warning it gave (none when empty).
		filed   bool
		wantErr string
	}{
		{"a web address: warned, filed on yes", `echo "see https://intranet.example/x" >> "$1"`, "Voice is silent\ny\n", true, "looks like a web address"},
		{"an email address: warned, filed on yes", `echo "mail tess@example.org" >> "$1"`, "Voice is silent\ny\n", true, "looks like an email address"},
		{"an IP address: warned, filed on yes", `echo "server at 192.168.1.20" >> "$1"`, "Voice is silent\ny\n", true, "looks like an IP address"},
		{"all three: one warning", `echo "https://x.example a@b.example 10.0.0.1" >> "$1"`, "Voice is silent\ny\n", true, "looks like a web address, an email address, an IP address."},
		{"a warning, then no", `echo "https://x.example" >> "$1"`, "Voice is silent\nn\n", false, "WARNING"},
		{"a version is not an address", `echo "v0.1.0 and 1.2.3" >> "$1"`, "Voice is silent\ny\n", true, ""},
		{"edited and confirmed", `printf '\nTrimmed by the operator.\n' >> "$1"`, "Voice is silent\ny\n", true, ""},
		{"default title", `true`, "\nyes\n", true, ""},
		{"declined", `true`, "A title\nn\n", false, "not filed"},
		{"no answer", `true`, "", false, "not filed"},
		{"editor fails", `exit 3`, "t\ny\n", false, "the editor failed"},
		{"emptied", `: > "$1"`, "t\ny\n", false, "the report is empty"},
		{"a credential left in", `echo "token conch_AbCdEfGh1234" >> "$1"`, "t\ny\n", false, "looks like a Conch credential"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := t.TempDir()
			logs := t.TempDir()
			fakeProgram(t, bin, "conch", `echo "$*" >> "`+logs+`/conch.args"
case "$*" in
"admin reports show 7 --markdown") printf '%s' '`+fakeMarkdown+`' ;;
"admin reports file 7 "*) echo "report 7 filed as $5" ;;
*) echo "unexpected: $*" >&2; exit 9 ;;
esac
`)
			fakeProgram(t, bin, "gh", `echo "$*" > "`+logs+`/gh.args"
while [ $# -gt 0 ]; do
  if [ "$1" = "--body-file" ]; then cp "$2" "`+logs+`/body.md"; fi
  shift
done
echo "Creating issue in example/conch"
echo "https://github.com/example/conch/issues/900"
`)
			fakeProgram(t, bin, "editor", tt.editor+"\n")
			cmd := exec.Command("bash", script, "7") // #nosec G204 -- the repository's own script
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "EDITOR="+filepath.Join(bin, "editor"), "CONCH=", "GH=")
			cmd.Stdin = strings.NewReader(tt.stdin)
			out, err := cmd.CombinedOutput()
			ghArgs, ghErr := os.ReadFile(filepath.Join(logs, "gh.args"))   // #nosec G304 -- the test's own temp dir
			conchArgs, _ := os.ReadFile(filepath.Join(logs, "conch.args")) // #nosec G304 -- the test's own temp dir
			if !tt.filed {
				if err == nil {
					t.Errorf("the script succeeded:\n%s", out)
				}
				if !strings.Contains(string(out), tt.wantErr) {
					t.Errorf("output %q, want %q", out, tt.wantErr)
				}
				if ghErr == nil {
					t.Errorf("gh was run: %s", ghArgs)
				}
				if strings.Contains(string(conchArgs), "reports file") {
					t.Errorf("the report was marked filed: %s", conchArgs)
				}
				return
			}
			if err != nil {
				t.Fatalf("script: %v\n%s", err, out)
			}
			if tt.wantErr != "" && !strings.Contains(string(out), tt.wantErr) {
				t.Errorf("output %q, want %q", out, tt.wantErr)
			}
			if tt.wantErr == "" && strings.Contains(string(out), "WARNING") {
				t.Errorf("a warning for a clean report: %q", out)
			}
			// The warning comes before the question, not after the filing.
			if w, q := strings.Index(string(out), "WARNING"), strings.Index(string(out), "[y/N]"); w > q {
				t.Errorf("the warning came after the question: %q", out)
			}
			title := "Voice is silent"
			if tt.name == "default title" {
				title = "Bug: report 7"
			}
			if want := "issue create --label type/bug --title " + title + " --body-file "; !strings.HasPrefix(string(ghArgs), want) {
				t.Errorf("gh %s, want %s…", ghArgs, want)
			}
			body, err := os.ReadFile(filepath.Join(logs, "body.md")) // #nosec G304 -- the test's own temp dir
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(body), fakeMarkdown) {
				t.Errorf("filed body %q, want the report's markdown", body)
			}
			if tt.name == "edited and confirmed" && !strings.HasSuffix(string(body), "Trimmed by the operator.\n") {
				t.Errorf("the operator's edit is not in the filed body: %q", body)
			}
			if want := "admin reports show 7 --markdown\nadmin reports file 7 https://github.com/example/conch/issues/900\n"; string(conchArgs) != want {
				t.Errorf("conch was run as\n%s\nwant\n%s", conchArgs, want)
			}
			if !strings.HasSuffix(string(out), "https://github.com/example/conch/issues/900\n") {
				t.Errorf("output %q does not end with the issue", out)
			}
		})
	}

	// A report id that is not a number is refused before anything runs.
	cmd := exec.Command("bash", script, "7; rm -rf /") // #nosec G204 -- the repository's own script
	if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "usage:") {
		t.Errorf("a bad id: %v %s", err, out)
	}
}
