package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// ExportAudit streams GET /v1/audit to w exactly as the server sends it: JSON
// Lines of conch.audit_event.v1, not decoded and not re-encoded. after is the
// last id already held (0 for the start); a limit of 0 means no limit, which
// the server ends at the last id when the request began. A refusal is the
// server's error and nothing is written to w.
func (c *Client) ExportAudit(ctx context.Context, after, limit int64, w io.Writer) error {
	endpoint := c.resolve("v1", "audit")
	q := url.Values{}
	if after > 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.FormatInt(limit, 10))
	}
	endpoint.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("cli: create audit export request: %w", err)
	}
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("cli: audit export: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return c.decodeError(resp)
	}
	// Copied as it arrives: the trail is never held whole.
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("cli: audit export: %w", err)
	}
	return nil
}

func runAudit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cli: audit requires a subcommand (export)")
	}
	switch args[0] {
	case "export":
		return runAuditExport(ctx, args[1:], stdout, stderr)
	default:
		return fmt.Errorf("cli: unknown audit subcommand %q", args[0])
	}
}

func runAuditExport(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("audit export", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	after := fs.Int64("after", 0, "export only events with an id above this")
	limit := fs.Int64("limit", 0, "export at most this many events")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("cli: audit export: %w", err)
	}
	if fs.NArg() != 0 {
		return errors.New("cli: audit export takes no positional arguments")
	}
	given := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if *after < 0 {
		return errors.New("cli: audit export: --after must not be negative")
	}
	// An explicit --limit 0 is a mistake, not "no limit": leave the flag out
	// for that, as the server also refuses limit=0.
	if *limit < 0 || (given["limit"] && *limit == 0) {
		return errors.New("cli: audit export: --limit must be a positive integer")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	if err := client.ExportAudit(ctx, *after, *limit, stdout); err != nil {
		return &serverText{msg: safeLine(err.Error()), err: err}
	}
	return nil
}
