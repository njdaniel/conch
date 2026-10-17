package cli

import (
	"context"
	"flag"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin reports …` (issue #446): the operator's side of problem
// reports. list and show read them; show --markdown renders one as an issue
// body in the bug template's sections, with channel and person names left out;
// file records the issue a report became. ops/report-to-issue.sh runs show
// --markdown, has the operator read and trim it, files it with gh and then
// runs file. conchd itself never reaches GitHub.

func bindReportsList(fs *flag.FlagSet, o *adminOpts) {
	fs.IntVar(&o.limit, "limit", 0, "at most this many reports, newest first")
}

func bindReportsShow(fs *flag.FlagSet, o *adminOpts) {
	fs.BoolVar(&o.markdown, "markdown", false, "print the report as an issue body, without channel or person names")
}

// adminReportsList prints one line per report, newest first:
//
//	<id> <open|filed> <created> principal <id> <client> <client version> server <version> <often> [issue]
func adminReportsList(ctx context.Context, r *adminRun) error {
	if r.opts.limit < 0 {
		return fmt.Errorf("cli: admin reports list: --limit must not be negative")
	}
	resp, err := r.client.ListProblemReports(ctx, r.opts.limit)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(resp)
	}
	if len(resp.Reports) == 0 {
		return r.say("no reports\n")
	}
	for _, p := range resp.Reports {
		line := fmt.Sprintf("%d %s %s principal %d %s %s server %s %s", p.ID, safeLine(string(p.State)), p.CreatedAt.Time().UTC().Format(time.RFC3339),
			p.PrincipalID, safeLine(p.ClientName), safeLine(p.ClientVersion), safeLine(p.ServerVersion), safeLine(string(p.Often)))
		if p.Issue != "" {
			line += " " + safeLine(p.Issue)
		}
		if err := r.say("%s\n", line); err != nil {
			return err
		}
	}
	return nil
}

// adminReportsShow prints one report: for the operator, who sent it and where
// it stands, then the body; with --markdown the body alone, ready to file.
func adminReportsShow(ctx context.Context, r *adminRun) error {
	report, err := r.client.ProblemReport(ctx, r.id)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(report)
	}
	body, redacted := renderReportMarkdown(report)
	// Said on stderr, so --markdown's stdout is the body alone.
	if report.Redact == nil {
		_, _ = fmt.Fprintln(r.stderr, "warning: the server sent no names to take out, so channel and person names are left in; read the report closely")
	} else {
		_, _ = fmt.Fprintf(r.stderr, "took out %d channel and person name(s), from %d channel and %d principal names on the server\n",
			redacted, len(report.Redact.Channels), len(report.Redact.Principals))
	}
	if r.opts.markdown {
		_, err := fmt.Fprint(r.stdout, body)
		return err
	}
	header := fmt.Sprintf("report %d, %s, from principal %d at %s\n", report.ID, safeLine(string(report.State)), report.PrincipalID, report.CreatedAt.Time().UTC().Format(time.RFC3339))
	if report.Issue != "" {
		header += "filed as " + safeLine(report.Issue) + "\n"
	}
	_, err = fmt.Fprintf(r.stdout, "%s\n%s", header, body)
	return err
}

// adminReportsFile records that report <id> became the issue at <issue-url>.
func adminReportsFile(ctx context.Context, r *adminRun) error {
	req := schema.UpdateProblemReportRequestV1{State: schema.ProblemReportStateFiled, Issue: r.pos[1]}
	// The server's own check first, so an address it would refuse costs no
	// request.
	if err := req.Validate(); err != nil {
		return fmt.Errorf("cli: admin reports file: %w", err)
	}
	report, err := r.client.FileProblemReport(ctx, r.id, req.Issue)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		return r.printJSON(report)
	}
	return r.say("report %d filed as %s\n", report.ID, safeLine(report.Issue))
}
