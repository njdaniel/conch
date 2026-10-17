package cli

import (
	"context"
	"net/http"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// Problem reports (issue #446), behind `conch report` and `conch admin
// reports`. Each is one request to a public route; the server decides who may
// make it.

// CreateProblemReport sends a report of a problem. The answer is its id.
func (c *Client) CreateProblemReport(ctx context.Context, request schema.CreateProblemReportRequestV1) (schema.CreateProblemReportResponseV1, error) {
	var result schema.CreateProblemReportResponseV1
	if err := c.roundTrip(ctx, "send problem report", http.MethodPost, c.resolve("v1", "reports"), request, &result); err != nil {
		return schema.CreateProblemReportResponseV1{}, err
	}
	return result, nil
}

// ListProblemReports returns the newest reports first, without their text
// (operators only).
func (c *Client) ListProblemReports(ctx context.Context, limit int) (schema.ListProblemReportsResponseV1, error) {
	endpoint := c.resolve("v1", "reports")
	if limit > 0 {
		q := endpoint.Query()
		q.Set("limit", strconv.Itoa(limit))
		endpoint.RawQuery = q.Encode()
	}
	var result schema.ListProblemReportsResponseV1
	if err := c.roundTrip(ctx, "list problem reports", http.MethodGet, endpoint, nil, &result); err != nil {
		return schema.ListProblemReportsResponseV1{}, err
	}
	return result, nil
}

// ProblemReport returns one report whole (operators only).
func (c *Client) ProblemReport(ctx context.Context, id int64) (schema.ProblemReportV1, error) {
	var result schema.ProblemReportV1
	if err := c.roundTrip(ctx, "read problem report", http.MethodGet, c.resolve("v1", "reports", strconv.FormatInt(id, 10)), nil, &result); err != nil {
		return schema.ProblemReportV1{}, err
	}
	return result, nil
}

// FileProblemReport marks a report filed as the issue at issueURL (operators
// only), and returns it as it now is.
func (c *Client) FileProblemReport(ctx context.Context, id int64, issueURL string) (schema.ProblemReportV1, error) {
	request := schema.UpdateProblemReportRequestV1{State: schema.ProblemReportStateFiled, Issue: issueURL}
	var result schema.ProblemReportV1
	if err := c.roundTrip(ctx, "file problem report", http.MethodPut, c.resolve("v1", "reports", strconv.FormatInt(id, 10)), request, &result); err != nil {
		return schema.ProblemReportV1{}, err
	}
	return result, nil
}
