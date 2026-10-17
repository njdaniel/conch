package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/njdaniel/conch/pkg/schema"
)

// A report as a public issue body (issue #446). The repository is public, so
// the body names no channel, net or person, links nothing and mentions no
// one:
//
//   - the names come from the server (the redact list of GET
//     /v1/reports/{id}: every channel, net and principal on it), not from what
//     the operator's own credential can list, and are replaced in everything
//     the client wrote;
//   - the tester's three answers are each in a fenced code block whose fence
//     is longer than any run of backticks in them, so a mention, an issue
//     link or HTML in them is inert text;
//   - the browser's user agent is reduced to its family and major version.
//
// The stored report keeps everything as it was sent, for the operator.

// minRedactRunes is the shortest name redacted: a one-character name would
// take that letter out of every word it stands alone in ("I", "a"), and
// leave the text unreadable without hiding anyone.
const minRedactRunes = 2

// redactor replaces names in text and counts what it replaced.
type redactor struct {
	rules []redactRule
	count int
}

type redactRule struct {
	re          *regexp.Regexp
	placeholder string
}

// newRedactor builds the rules for list: longer names first, so a name inside
// another is not left half-replaced; case does not matter; a name is matched
// whole, between characters that are not letters, digits or underscores, and
// with any # before it.
func newRedactor(list *schema.ProblemReportRedactionV1) *redactor {
	type name struct{ text, placeholder string }
	var all []name
	if list != nil {
		for _, c := range list.Channels {
			all = append(all, name{c, "[channel]"})
		}
		for _, p := range list.Principals {
			all = append(all, name{p, "[person]"})
		}
	}
	slices.SortStableFunc(all, func(a, b name) int { return len(b.text) - len(a.text) })
	x := &redactor{}
	for _, n := range all {
		if utf8.RuneCountInString(strings.TrimSpace(n.text)) < minRedactRunes {
			continue
		}
		x.rules = append(x.rules, redactRule{
			re:          regexp.MustCompile(`(?i)(^|[^\pL\pN_])#?` + regexp.QuoteMeta(n.text) + `($|[^\pL\pN_])`),
			placeholder: n.placeholder,
		})
	}
	return x
}

// redact replaces every name in s.
func (x *redactor) redact(s string) string {
	for _, r := range x.rules {
		// A match takes the separator after it, so a name right after another
		// is found on the next round; repeat until nothing is left.
		for {
			n := len(r.re.FindAllStringIndex(s, -1))
			if n == 0 {
				break
			}
			x.count += n
			s = r.re.ReplaceAllString(s, "${1}"+r.placeholder+"${2}")
		}
	}
	return s
}

// fenced is s in a fenced code block whose fence is longer than any run of
// backticks in s, so nothing in s can close it early.
func fenced(s string) string {
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
			continue
		}
		run = 0
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + s + "\n" + fence
}

// browserFamily is a user agent reduced to the browser's family and major
// version ("Firefox 131", "Chrome 130", "Safari 18"), or "other".
func browserFamily(ua string) string {
	major := func(after string) string {
		end := strings.IndexFunc(after, func(r rune) bool { return !unicode.IsDigit(r) })
		if end < 0 {
			end = len(after)
		}
		return after[:end]
	}
	for _, f := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"Firefox/", "Firefox"}, {"FxiOS/", "Firefox"},
		{"CriOS/", "Chrome"}, {"Chrome/", "Chrome"},
	} {
		if i := strings.Index(ua, f.token); i >= 0 {
			if v := major(ua[i+len(f.token):]); v != "" {
				return f.name + " " + v
			}
		}
	}
	if strings.Contains(ua, "Safari/") {
		if i := strings.Index(ua, "Version/"); i >= 0 {
			if v := major(ua[i+len("Version/"):]); v != "" {
				return "Safari " + v
			}
		}
	}
	return "other"
}

// renderReportMarkdown is a report as an issue body, in the bug template's
// sections (.github/ISSUE_TEMPLATE/bug.yml), and how many names it took out.
// It does not name the reporter, and every name of r.Redact is taken out of
// what the client wrote.
func renderReportMarkdown(r schema.ProblemReportV1) (string, int) {
	x := newRedactor(r.Redact)
	var b strings.Builder
	section := func(title, body string) {
		fmt.Fprintf(&b, "### %s\n\n%s\n\n", title, strings.TrimSpace(body))
	}
	answer := func(s string) string { return fenced(safeText(x.redact(strings.TrimSpace(s)))) }
	line := func(s string) string { return safeLine(x.redact(s)) }
	section("What you did", answer(r.Did))
	section("What you expected", answer(r.Expected))
	section("What happened instead", answer(r.Happened))
	section("Conch version", fmt.Sprintf("%s %s (server %s)", line(r.Client.Name), line(r.Client.Version), line(r.ServerVersion)))
	env := []string{line(r.Client.Platform)}
	if r.Client.UserAgent != "" {
		env = append(env, line(browserFamily(r.Client.UserAgent)))
	}
	if r.Client.Screen != "" {
		env = append(env, "screen "+line(r.Client.Screen))
	}
	section("Device, system and browser (or client)", strings.Join(env, "; "))
	section("How often does it happen?", safeLine(r.Often.Label()))

	block := func(lines []string) string {
		if len(lines) == 0 {
			lines = []string{"(none)"}
		}
		return fenced(strings.Join(lines, "\n"))
	}
	var events, requests, closes []string
	for _, e := range r.Diagnostics {
		events = append(events, x.redact(diagnosticLine(e)))
	}
	for _, q := range r.Server.Requests {
		requests = append(requests, line(fmt.Sprintf("%s %s %d %s", stamp(q.At), q.RequestID, q.Status, q.Route)))
	}
	for _, c := range r.Server.SocketCloses {
		closes = append(closes, line(fmt.Sprintf("%s %s %s %s after %ds, %d frames", stamp(c.At), c.RequestID, c.Route, c.Reason, c.DurationSeconds, c.Frames)))
	}
	var d strings.Builder
	fmt.Fprintf(&d, "Reported from inside Conch (report %d, %s).\n", r.ID, r.CreatedAt.Time().UTC().Format(time.RFC3339))
	fmt.Fprintf(&d, "Server voice health: %s. Client voice: %s; screen share: %s.\n", line(string(r.Voice)), orDash(x.redact(r.Client.Voice)), orDash(x.redact(r.Client.Share)))
	fmt.Fprintf(&d, "\nClient events, oldest first:\n\n%s\n", block(events))
	fmt.Fprintf(&d, "\nThe server's record of the reporter's recent requests:\n\n%s\n", block(requests))
	fmt.Fprintf(&d, "\nAnd of their recent socket closes:\n\n%s", block(closes))
	section("Diagnostics (optional)", d.String())
	return strings.TrimRight(b.String(), "\n") + "\n", x.count
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return safeLine(s)
}

func stamp(t schema.Timestamp) string { return t.Time().UTC().Format("2006-01-02T15:04:05.000Z") }

// diagnosticLine is one client event on one line.
func diagnosticLine(e schema.DiagnosticEventV1) string {
	parts := []string{stamp(e.At), string(e.Kind)}
	if e.Socket != "" {
		parts = append(parts, string(e.Socket))
	}
	if e.Code != 0 || e.Kind == schema.DiagnosticHTTPFailure {
		parts = append(parts, "code="+strconv.Itoa(e.Code))
	}
	if e.Error != "" {
		parts = append(parts, "error="+e.Error)
	}
	if e.RequestID != "" {
		parts = append(parts, "request_id="+e.RequestID)
	}
	if e.Attempt != 0 {
		parts = append(parts, "attempt="+strconv.Itoa(e.Attempt))
	}
	if e.State != "" {
		parts = append(parts, "state="+string(e.State))
	}
	if e.Name != "" {
		parts = append(parts, "name="+e.Name)
	}
	return safeLine(strings.Join(parts, " "))
}
