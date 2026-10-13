package reviewpost

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// postWorld is a conchd with the review channel, a human who may post there
// and is logged in, and the environment the script runs in.
type postWorld struct {
	srv   *server
	env   *scriptEnv
	token string
	// cwd is where the script is run: an empty directory, so that anything
	// the report managed to execute would leave its mark here.
	cwd string
}

func newPostWorld(t *testing.T) *postWorld {
	t.Helper()
	srv := startServer(t)
	srv.channel(reviewChannel)
	_, token := srv.principal(schema.PrincipalHuman, "reviewer", reviewChannel)
	env := newScriptEnv(t)
	env.login(srv, token)
	return &postWorld{srv: srv, env: env, token: token, cwd: t.TempDir()}
}

// assertNothingRan fails if the working directory is no longer empty: every
// command the fixtures try to smuggle in creates a file there.
func (w *postWorld) assertNothingRan(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(w.cwd)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("the report was executed: %s appeared in the working directory", entry.Name())
	}
}

// The acceptance test of `post`: a report written to be hostile to a shell
// script is summarised and the summary arrives in the channel exactly as the
// golden file has it, decisions list included.
func TestPostDeliversTheSummary(t *testing.T) {
	fixture := filepath.Join(repoRoot, "e2e", "reviewpost", "testdata", "hostile.md")
	want := strings.TrimSuffix(readFile(t, filepath.Join("testdata", "hostile.summary")), "\n")
	decisions := strings.Join(sectionBody(t, readFile(t, fixture), "Decisions needed from Nick"), "\n")

	// The locale decides how the shell and awk count characters; the bytes
	// that arrive must not depend on it.
	for _, locale := range []string{"C", "C.UTF-8"} {
		t.Run(locale, func(t *testing.T) {
			w := newPostWorld(t)
			w.env.vars["LC_ALL"] = locale
			res := w.env.run(w.cwd, "post", fixture, prURL)
			if res.code != 0 {
				t.Fatalf("post exited %d: %s", res.code, res.stderr)
			}
			got := w.srv.bodies(reviewChannel)
			if len(got) != 1 {
				t.Fatalf("channel holds %d messages, want 1", len(got))
			}
			if got[0] != want {
				t.Errorf("summary in the channel:\n%s\nwant:\n%s", got[0], want)
			}
			if !strings.Contains(got[0], "\n"+decisions+"\n") {
				t.Errorf("the decisions list did not arrive byte for byte:\n%s", got[0])
			}
			if res.stdout != "" {
				t.Errorf("post wrote to stdout: %q", res.stdout)
			}
			if line := oneLine(t, "stderr", res.stderr); !strings.Contains(line, "posted the summary") {
				t.Errorf("stderr = %q, want the one line saying it was posted", line)
			}
			w.assertNothingRan(t)
			w.env.assertNoSecret(w.token, res)
		})
	}
}

// Bytes a text file can hold that a shell script is likely to lose: leading
// and trailing whitespace, carriage returns, control characters, backslash
// sequences, format directives, and blank lines inside the list.
func TestPostKeepsAwkwardBytes(t *testing.T) {
	lines := []string{
		"\ta line led by a tab",
		"three trailing spaces   ",
		"a carriage return at the end\r",
		"a bell \x07 and an escape \x1b[31m sequence",
		"line separators " + string(rune(0x2028)) + " and " + string(rune(0x2029)) + " inside",
		`backslashes \n \t \\ \x41 \0 \c and a trailing one \`,
		"%s %d %n %% %5$s",
		"-",
		"",
		"   ",
		"$IFS $@ $* $0 $1 ${!x} $(( )) $[1] !! !$",
		"wide: 🎉 日本語 ñ — and a quote \" and an apostrophe '",
		strings.Repeat("long ", 1000) + "end",
		"the last line",
	}
	report := "# Awkward bytes\n\n## Decisions needed from Nick\n\n\n" + strings.Join(lines, "\n") + "\n\n\n"
	want := "Awkward bytes\n\nSections:\n- Decisions needed from Nick: no list items\n\n" +
		"Decisions needed from Nick:\n" + strings.Join(lines, "\n") + "\n\nPR: " + prURL

	w := newPostWorld(t)
	path := filepath.Join(t.TempDir(), "awkward.md")
	writeFile(t, path, report)
	res := w.env.run(w.cwd, "post", path, prURL)
	if res.code != 0 {
		t.Fatalf("post exited %d: %s", res.code, res.stderr)
	}
	got := w.srv.bodies(reviewChannel)
	if len(got) != 1 {
		t.Fatalf("channel holds %d messages, want 1", len(got))
	}
	if got[0] != want {
		t.Errorf("summary in the channel:\n%q\nwant:\n%q", got[0], want)
	}
	w.assertNothingRan(t)
}

// A report path and a channel name are arguments too, and a report with no
// title is summarised under its file name, so the text itself then starts
// with the dash. None of the three may be read as an option, by the script
// or by conch.
func TestPostWithDashLedNames(t *testing.T) {
	w := newPostWorld(t)
	const channel = "-net"
	w.srv.channel(channel)
	// The reviewer is principal 2: the operator is 1.
	w.srv.member(channel, 2)
	w.env.vars["CONCH_REVIEW_CHANNEL"] = channel
	writeFile(t, filepath.Join(w.cwd, "-n.md"), "## Decisions needed from Nick\n\n- one\n")

	res := w.env.run(w.cwd, "post", "-n.md")
	if res.code != 0 {
		t.Fatalf("post exited %d: %s", res.code, res.stderr)
	}
	want := "-n.md\n\nSections:\n- Decisions needed from Nick: 1 item\n\nDecisions needed from Nick:\n- one"
	if got := w.srv.bodies(channel); len(got) != 1 || got[0] != want {
		t.Errorf("channel %q holds %q, want [%q]", channel, got, want)
	}
	if got := w.srv.bodies(reviewChannel); len(got) != 0 {
		t.Errorf("the default channel got %d messages", len(got))
	}
}

// The two kinds of byte that cannot arrive as they are, and what happens to
// them instead: a NUL cannot be in an argument and is dropped, and a byte
// that is not UTF-8 cannot be in JSON, where conch writes U+FFFD for it.
// Neither stops the post. A report with no decisions section says so.
func TestPostOfBytesThatCannotTravel(t *testing.T) {
	w := newPostWorld(t)
	path := filepath.Join(t.TempDir(), "bytes.md")
	writeFile(t, path, "# Bytes\n\n## Merged work\n\n- a NUL \x00 and a Latin-1 e-acute \xe9 here\n")
	res := w.env.run(w.cwd, "post", path)
	if res.code != 0 {
		t.Fatalf("post exited %d: %s", res.code, res.stderr)
	}
	want := "Bytes\n\nSections:\n- Merged work: 1 item\n\nDecisions needed from Nick: the report has no such section"
	if got := w.srv.bodies(reviewChannel); len(got) != 1 || got[0] != want {
		t.Errorf("channel holds %q, want [%q]", got, want)
	}

	writeFile(t, path, "# Bytes\n\n## Decisions needed from Nick\n\n- a NUL \x00 and a Latin-1 e-acute \xe9 here\n\n## Empty decisions follow\n")
	if res := w.env.run(w.cwd, "post", path); res.code != 0 {
		t.Fatalf("post exited %d: %s", res.code, res.stderr)
	}
	want = "Bytes\n\nSections:\n- Decisions needed from Nick: 1 item\n- Empty decisions follow: no list items\n\n" +
		"Decisions needed from Nick:\n- a NUL  and a Latin-1 e-acute \ufffd here"
	if got := w.srv.bodies(reviewChannel); len(got) != 2 || got[1] != want {
		t.Errorf("channel holds %q, want the second to be %q", got, want)
	}

	// A section that is there but empty is not the same as one that is missing.
	writeFile(t, path, "# Bytes\n\n## Decisions needed from Nick\n\n\n")
	if res := w.env.run(w.cwd, "post", path); res.code != 0 {
		t.Fatalf("post exited %d: %s", res.code, res.stderr)
	}
	want = "Bytes\n\nSections:\n- Decisions needed from Nick: no list items\n\nDecisions needed from Nick:\n(none listed)"
	if got := w.srv.bodies(reviewChannel); len(got) != 3 || got[2] != want {
		t.Errorf("channel holds %q, want the third to be %q", got, want)
	}
}

// The three ways a post fails. Each prints one line saying which, exits with
// the one documented status, and leaves the channel as it was.
func TestPostFailures(t *testing.T) {
	fixture := filepath.Join(repoRoot, "e2e", "reviewpost", "testdata", "hostile.md")
	const private = "ops-private"

	for _, tc := range []struct {
		name string
		// arrange changes the world before the post and returns what must be
		// undone before the channels are read back.
		arrange func(t *testing.T, w *postWorld) (restore func())
		want    string
	}{
		{
			name: "conchd unreachable",
			arrange: func(_ *testing.T, w *postWorld) func() {
				w.srv.stop()
				return w.srv.start
			},
			want: "conchd unreachable at ",
		},
		{
			name: "no stored login",
			arrange: func(t *testing.T, w *postWorld) func() {
				w.env.vars["XDG_CONFIG_HOME"] = t.TempDir()
				return nil
			},
			want: "no stored login for ",
		},
		{
			name: "a channel the principal is not a member of",
			arrange: func(_ *testing.T, w *postWorld) func() {
				w.env.vars["CONCH_REVIEW_CHANNEL"] = private
				return nil
			},
			want: "cannot post to channel 'ops-private'",
		},
		{
			name: "a channel that does not exist",
			arrange: func(_ *testing.T, w *postWorld) func() {
				w.env.vars["CONCH_REVIEW_CHANNEL"] = "no-such-channel"
				return nil
			},
			want: "cannot post to channel 'no-such-channel'",
		},
		{
			name: "an agent whose manifest lets it read the channel but not post",
			arrange: func(_ *testing.T, w *postWorld) func() {
				id, token := w.srv.principal(schema.PrincipalAgent, "read-only-agent", private)
				w.srv.call(w.srv.operatorToken, "PUT", fmt.Sprintf("/v1/principals/%d/manifest", id),
					schema.PutAgentManifestRequestV1{
						DisplayName:  "read-only-agent",
						Tier:         schema.AgentTierA,
						Capabilities: []schema.Capability{schema.CapabilityMessagesRead},
						Channels: []schema.ChannelGrant{{
							ChannelID:   2, // ops-private: the second channel made
							Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead},
						}},
					}, nil)
				w.env.login(w.srv, token)
				w.token = token
				w.env.vars["CONCH_REVIEW_CHANNEL"] = private
				return nil
			},
			want: "cannot post to channel 'ops-private'",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newPostWorld(t)
			if id := w.srv.channel(private); id != 2 {
				t.Fatalf("%s has id %d, the manifest case assumes 2", private, id)
			}
			w.srv.post(reviewChannel, "already here")
			restore := tc.arrange(t, w)

			res := w.env.run(w.cwd, "post", fixture, prURL)
			if restore != nil {
				restore()
			}
			if res.code != exitNotPosted {
				t.Errorf("exit status %d, want %d", res.code, exitNotPosted)
			}
			if res.stdout != "" {
				t.Errorf("stdout = %q, want nothing", res.stdout)
			}
			line := oneLine(t, "stderr", res.stderr)
			if !strings.Contains(line, "not posted: "+tc.want) {
				t.Errorf("stderr = %q, want it to say %q", line, tc.want)
			}
			if !strings.Contains(w.env.logs(), tc.want) {
				t.Errorf("the reason is not in the log:\n%s", w.env.logs())
			}
			if got := w.srv.bodies(reviewChannel); len(got) != 1 || got[0] != "already here" {
				t.Errorf("%s holds %q, want only what was there before", reviewChannel, got)
			}
			if got := w.srv.bodies(private); len(got) != 0 {
				t.Errorf("%s holds %q, want nothing", private, got)
			}
			w.assertNothingRan(t)
			w.env.assertNoSecret(w.token, res)
		})
	}
}

// An agent may be the poster too, given membership and a manifest that
// grants messages.post in the channel.
func TestPostAsAnAgent(t *testing.T) {
	w := newPostWorld(t)
	id, token := w.srv.principal(schema.PrincipalAgent, "review-bot", reviewChannel)
	w.srv.call(w.srv.operatorToken, "PUT", fmt.Sprintf("/v1/principals/%d/manifest", id), schema.PutAgentManifestRequestV1{
		DisplayName:  "review-bot",
		Tier:         schema.AgentTierA,
		Capabilities: []schema.Capability{schema.CapabilityMessagesPost},
		Channels: []schema.ChannelGrant{{
			ChannelID:   1, // principal-review: the first channel made
			Permissions: []schema.ChannelPermission{schema.ChannelPermissionPost},
		}},
	}, nil)
	w.env.login(w.srv, token)

	res := w.env.run(w.cwd, "post", filepath.Join(repoRoot, "e2e", "reviewpost", "testdata", "hostile.md"), prURL)
	if res.code != 0 {
		t.Fatalf("post exited %d: %s", res.code, res.stderr)
	}
	messages := w.srv.messages(reviewChannel)
	if len(messages) != 1 || messages[0].AuthorID != id {
		t.Fatalf("channel holds %d messages, want one from the agent (principal %d)", len(messages), id)
	}
	w.env.assertNoSecret(token, res)
}

// Without conch on PATH the post fails the same way, and says so.
func TestPostWithoutConchOnPath(t *testing.T) {
	w := newPostWorld(t)
	w.env.vars["CONCH_BIN"] = "conch-that-is-not-installed"
	res := w.env.run(w.cwd, "post", filepath.Join(repoRoot, "e2e", "reviewpost", "testdata", "hostile.md"))
	if res.code != exitNotPosted {
		t.Errorf("exit status %d, want %d", res.code, exitNotPosted)
	}
	if line := oneLine(t, "stderr", res.stderr); !strings.Contains(line, "is not on PATH") {
		t.Errorf("stderr = %q", line)
	}
	if got := w.srv.bodies(reviewChannel); len(got) != 0 {
		t.Errorf("channel holds %q, want nothing", got)
	}
}

func TestPostUsage(t *testing.T) {
	w := newPostWorld(t)
	for _, args := range [][]string{
		{"post"},
		{"post", filepath.Join(w.cwd, "no-such-report.md")},
		{"post", scriptPath, "a url with spaces"},
		{"post", scriptPath, prURL, "one too many"},
		{"summarise"},
	} {
		res := w.env.run(w.cwd, args...)
		if res.code != 2 {
			t.Errorf("%q exited %d, want 2 (stderr %q)", args, res.code, res.stderr)
		}
	}
	if got := w.srv.bodies(reviewChannel); len(got) != 0 {
		t.Errorf("channel holds %q, want nothing", got)
	}
}

// The three reports already in docs/reports. Each summary must name every
// section with its item count, carry the report's own decisions list whole,
// and fit the budget.
func TestSummariesOfTheCommittedReports(t *testing.T) {
	w := newPostWorld(t)
	for _, tc := range []struct {
		file      string
		title     string
		sections  []string
		decisions string
	}{
		{
			file:  "2026-07-12-session-1.md",
			title: "Session 1 report — bootstrap",
			sections: []string{
				"- What was created: no list items",
				"- Decisions taken on delegated open items (§9): 5 items",
				"- Deviations from the handoff: 2 items",
				"- Implementation order (ready for cold sessions): no list items",
				"- Nick's action items: 4 items",
			},
			// The first report predates the skill's name for the section.
			decisions: "Nick's action items",
		},
		{
			file:  "2026-07-15-principal-review.md",
			title: "Principal review — P1 close-out",
			sections: []string{
				"- Merged work (28 PRs since the last report): 16 items",
				"- Notable dispatch-loop findings: 4 items",
				"- Invariant status: 4 items",
				"- Backlog delta: 5 items",
				"- Decisions needed from Nick: 4 items",
			},
			decisions: "Decisions needed from Nick",
		},
		{
			file:  "2026-07-21-principal-review.md",
			title: "Principal review — P2 process refresh close-out",
			sections: []string{
				"- Merged work (2 PRs): 2 items",
				"- Invariant status: 4 items",
				"- Backlog delta: 5 items",
				"- Decisions needed from Nick: 4 items",
				"- Note on step 6 (post report into Conch / raise release approvals): no list items",
			},
			decisions: "Decisions needed from Nick",
		},
	} {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join(repoRoot, "docs", "reports", tc.file)
			before := len(w.srv.bodies(reviewChannel))
			res := w.env.run(w.cwd, "post", path, prURL)
			if res.code != 0 {
				t.Fatalf("post exited %d: %s", res.code, res.stderr)
			}
			bodies := w.srv.bodies(reviewChannel)
			if len(bodies) != before+1 {
				t.Fatalf("channel holds %d messages, want %d", len(bodies), before+1)
			}
			got := bodies[before]
			decisions := sectionBody(t, readFile(t, path), tc.decisions)
			if len(decisions) != 4 {
				t.Fatalf("the report has %d decision lines, this test expects its 4 items", len(decisions))
			}
			want := tc.title + "\n\nSections:\n" + strings.Join(tc.sections, "\n") + "\n\n" +
				tc.decisions + ":\n" + strings.Join(decisions, "\n") + "\n\nPR: " + prURL
			if got != want {
				t.Errorf("summary:\n%s\nwant:\n%s", got, want)
			}
			if n := encodedLen(t, got); n > summaryBudget {
				t.Errorf("summary takes %d bytes as JSON, over the budget of %d", n, summaryBudget)
			}
		})
	}
}

// A decisions section far larger than a message. The summary is cut on a
// line boundary, says so, keeps its PR link, and fits; a `conch tail` that
// was watching the channel receives it and stays connected, which is what
// the budget is for.
func TestSummaryIsCutToFit(t *testing.T) {
	const sectionBytes = 100 * 1024
	for _, tc := range []struct {
		name string
		// line makes decision number i.
		line func(i int) string
	}{
		{"plain lines", func(i int) string {
			return fmt.Sprintf("%d. decide whether item %d ships this week or waits for the next milestone", i, i)
		}},
		// Six bytes each once encoded, so a count of raw bytes would let six
		// times the budget through.
		{"lines that grow sixfold when encoded", func(i int) string {
			return fmt.Sprintf("%d. %s", i, strings.Repeat("<&>", 20))
		}},
		{"one line longer than a message", func(i int) string {
			if i == 3 {
				return "3. " + strings.Repeat("x", sectionBytes)
			}
			return fmt.Sprintf("%d. short", i)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var decisions []string
			for size := 0; size < sectionBytes; {
				line := tc.line(len(decisions) + 1)
				decisions = append(decisions, line)
				size += len(line) + 1
			}
			report := "# A very long week\n\n## Merged work\n\n- #1 one\n\n## Decisions needed from Nick\n\n" +
				strings.Join(decisions, "\n") + "\n"
			path := filepath.Join(t.TempDir(), "long.md")
			writeFile(t, path, report)

			w := newPostWorld(t)
			tail := startTail(t, w)

			res := w.env.run(w.cwd, "post", path, prURL)
			if res.code != 0 {
				t.Fatalf("post exited %d: %s", res.code, res.stderr)
			}
			bodies := w.srv.bodies(reviewChannel)
			got := bodies[len(bodies)-1]
			if n := encodedLen(t, got); n > summaryBudget || n > serverBodyLimit {
				t.Errorf("summary takes %d bytes as JSON, over the budget of %d", n, summaryBudget)
			}

			head := "A very long week\n\nSections:\n- Merged work: 1 item\n" +
				fmt.Sprintf("- Decisions needed from Nick: %d items\n\nDecisions needed from Nick:\n", len(decisions))
			rest, ok := strings.CutPrefix(got, head)
			if !ok {
				t.Fatalf("summary does not start with its head:\n%.300s", got)
			}
			rest, ok = strings.CutSuffix(rest, "\n\nPR: "+prURL)
			if !ok {
				t.Fatalf("summary does not end with the PR link:\n...%s", got[max(0, len(got)-300):])
			}
			// What is left is whole decision lines, in order from the first,
			// and then the notice counting the ones that did not fit.
			lines := strings.Split(rest, "\n")
			kept, notice := lines[:len(lines)-1], lines[len(lines)-1]
			if len(kept) >= len(decisions) {
				t.Fatalf("nothing was cut: %d lines kept of %d", len(kept), len(decisions))
			}
			for i, line := range kept {
				if line != decisions[i] {
					t.Fatalf("kept line %d is %.80q, want the whole of %.80q", i+1, line, decisions[i])
				}
			}
			wantNotice := fmt.Sprintf("[cut here to fit one Conch message; lines left in the report: %d]", len(decisions)-len(kept))
			if notice != wantNotice {
				t.Errorf("notice = %q, want %q", notice, wantNotice)
			}
			// As much as fits is kept: the next line would have gone over.
			if len(kept) < len(decisions) {
				withNext := encodedLen(t, got) + encodedLen(t, decisions[len(kept)]+"\n")
				if withNext <= summaryBudget-200 {
					t.Errorf("cut too early: one more line would take %d of %d bytes", withNext, summaryBudget)
				}
			}

			tail.sawAndSurvived(t, "lines left in the report")
		})
	}
}

// tailProc is a `conch tail` on the review channel, as the reviewer.
type tailProc struct {
	w   *postWorld
	out *lockedBuffer
	err *lockedBuffer
}

// startTail starts the subscriber and returns once it is receiving.
func startTail(t *testing.T, w *postWorld) *tailProc {
	t.Helper()
	p := &tailProc{w: w, out: &lockedBuffer{}, err: &lockedBuffer{}}
	cmd := exec.Command(conchBin, "tail", reviewChannel) // #nosec G204 -- a binary this test built
	cmd.Env = w.env.environ()
	cmd.Stdout = p.out
	cmd.Stderr = p.err
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	// There is no signal for "subscribed", so knock until it answers.
	knocks := 0
	eventually(t, "conch tail to subscribe", func() bool {
		knocks++
		w.srv.post(reviewChannel, fmt.Sprintf("knock %d", knocks))
		return strings.Contains(p.out.String(), "knock")
	})
	return p
}

// sawAndSurvived asserts the subscriber printed a message containing marker
// and still receives what is posted after it.
func (p *tailProc) sawAndSurvived(t *testing.T, marker string) {
	t.Helper()
	eventually(t, "conch tail to print the summary", func() bool {
		return strings.Contains(p.out.String(), marker)
	})
	p.w.srv.post(reviewChannel, "still listening")
	eventually(t, "conch tail to print a message after the summary (stderr: "+p.err.String()+")", func() bool {
		return strings.Contains(p.out.String(), "still listening")
	})
}
