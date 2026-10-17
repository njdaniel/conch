package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"unicode"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch report` (issue #446): a tester reports a problem to the server's
// operator, with the client's version and platform, in the bug template's
// words. With every answer given as a flag it asks nothing but the
// confirmation; with any missing it asks for each on standard input. Either
// way it shows what will be sent and sends it only on "y" (or --yes).
//
// The terminal client keeps no diagnostic history of its own, so it sends
// none: the server adds the reporter's recent request ids and socket closes
// itself, which cover the TUI's sockets as well.

const reportUsage = "conch report [--server <url>] [--did <text>] [--expected <text>] [--happened <text>] [--often every_time|sometimes|once] [--yes] [--json]"

// reportOftenAnswers maps what a person may type to the vocabulary.
var reportOftenAnswers = map[string]schema.ProblemReportOften{
	"every_time": schema.ProblemReportOftenEveryTime, "every time": schema.ProblemReportOftenEveryTime, "always": schema.ProblemReportOftenEveryTime, "1": schema.ProblemReportOftenEveryTime,
	"sometimes": schema.ProblemReportOftenSometimes, "2": schema.ProblemReportOftenSometimes,
	"once": schema.ProblemReportOftenOnce, "once so far": schema.ProblemReportOftenOnce, "3": schema.ProblemReportOftenOnce,
}

func runReport(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, version string) error {
	fs := newFlagSet("report", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	did := fs.String("did", "", "what you did")
	expected := fs.String("expected", "", "what you expected")
	happened := fs.String("happened", "", "what happened instead")
	often := fs.String("often", "", "how often it happens: every_time, sometimes or once")
	yes := fs.Bool("yes", false, "send without asking")
	asJSON := fs.Bool("json", false, "print the server's response document")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: report: %w (usage: %s)", err, reportUsage)
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("cli: report takes no positional arguments (usage: %s)", reportUsage)
	}
	in := bufio.NewReader(stdin)
	ask := func(question string, into *string) error {
		if strings.TrimSpace(*into) != "" {
			return nil
		}
		if _, err := fmt.Fprintf(stdout, "%s: ", question); err != nil {
			return err
		}
		line, err := in.ReadString('\n')
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return fmt.Errorf("cli: report: no answer to %q; give it with a flag (usage: %s)", question, reportUsage)
		}
		*into = strings.TrimSpace(line)
		return nil
	}
	for _, q := range []struct {
		question string
		into     *string
	}{
		{"What you did", did},
		{"What you expected", expected},
		{"What happened instead", happened},
		{"How often does it happen? (every_time, sometimes, once)", often},
	} {
		if err := ask(q.question, q.into); err != nil {
			return err
		}
	}
	answer, ok := reportOftenAnswers[strings.ToLower(strings.TrimSpace(*often))]
	if !ok {
		return fmt.Errorf("cli: report: --often must be every_time, sometimes or once")
	}
	req := schema.CreateProblemReportRequestV1{
		Did: *did, Expected: *expected, Happened: *happened, Often: answer,
		Client:      reportClient(version),
		Diagnostics: []schema.DiagnosticEventV1{},
	}
	// The server's own check first, so a report it would refuse costs no
	// request.
	if err := req.Validate(); err != nil {
		return fmt.Errorf("cli: report: %w", err)
	}
	if err := writeReportPreview(stdout, req); err != nil {
		return err
	}
	if !*yes {
		if _, err := fmt.Fprint(stdout, "Send this report? [y/N] "); err != nil {
			return err
		}
		line, _ := in.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			_, err := fmt.Fprintln(stdout, "not sent")
			return err
		}
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	resp, err := client.CreateProblemReport(ctx, req)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(resp)
	}
	_, err = fmt.Fprintf(stdout, "sent report %d; thank you. Quote its number if you are asked about it.\n", resp.ID)
	return err
}

// reportClient is what the terminal client says about itself.
func reportClient(version string) schema.ProblemReportClientV1 {
	v := strings.TrimSpace(version)
	if v == "" || strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r > '~' }) {
		v = "unknown"
	}
	return schema.ProblemReportClientV1{Name: "conch", Version: v, Platform: runtime.GOOS + "/" + runtime.GOARCH}
}

// writeReportPreview shows the person exactly what will be sent.
func writeReportPreview(w io.Writer, req schema.CreateProblemReportRequestV1) error {
	raw, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "\nThis is what will be sent to the server's operator, who reads it before it goes anywhere else.\nThe server adds its version, the time, and the ids of your recent requests.\n\n%s\n\n", safeText(string(raw)))
	return err
}

// safeText is safeLine for text of several lines: newlines are kept, every
// other character that is not printable becomes a visible escape, so a report
// cannot drive the operator's terminal or its editor.
func safeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || unicode.IsPrint(r) {
			b.WriteRune(r)
			continue
		}
		q := strconv.QuoteToASCII(string(r))
		b.WriteString(q[1 : len(q)-1])
	}
	return b.String()
}
