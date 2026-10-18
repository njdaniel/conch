package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Problem reports on the REST API (issue #446, private alpha A13).
//
// POST /v1/reports is for people: a tester sends what happened, and the server
// stores it with what it knows itself (its version, the voice health, the
// tester's recent request ids and socket closes). Reading and filing reports
// is for operators. conchd never sends a report anywhere: the repository the
// issues go to is public, so an operator reads each one first and files it
// from their own machine (ops/report-to-issue.sh).
//
// A report is limited in size (schema.MaxProblemReportBytes) and in rate, per
// person, by a token bucket like the invite routes' (invite_limits.go):
// reportBurst reports at once, refilled over reportWindow. Refusals are
// audited as problem_report_rate_limited, thinned by writeNth. The report
// itself is audited as problem_reported with its id only. Nothing here logs a
// report's text.

const (
	// auditReportRateLimited is a report refused by the rate limit.
	auditReportRateLimited = "problem_report_rate_limited"
	// denyAgentReports is the access_denied reason of an agent that sends a
	// report.
	denyAgentReports = "agents_no_reports"
	// reportUpdateMaxBytes bounds the body that files a report.
	reportUpdateMaxBytes = 2 << 10

	defaultReportBurst  = 10
	defaultReportWindow = time.Hour
)

func defaultReportLimit() inviteLimit {
	return inviteLimit{"reports_per_principal", defaultReportBurst, defaultReportWindow}
}

// reportLimiter is the per-person report bucket and the thinning of its audit
// rows. In memory only, like every rate limit here: a restart refills it.
type reportLimiter struct {
	mu      sync.Mutex
	limit   inviteLimit
	buckets bucketSet
	audits  denialCoalescer
}

func newReportLimiter(l inviteLimit) *reportLimiter { return &reportLimiter{limit: l} }

// take spends one report of principalID.
func (l *reportLimiter) take(principalID int64, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Principals are made by operators and invites, both limited, so one
	// shared overflow bucket is enough.
	return l.buckets.bucket(l.limit, strconv.FormatInt(principalID, 10), inviteOverflowKey, now).take(l.limit, now)
}

// setReportLimit replaces the limit and forgets every bucket. For tests,
// before the server serves.
func (s *Server) setReportLimit(l inviteLimit) { s.reportLimits = newReportLimiter(l) }

// decodeReportBody reads one JSON object of at most max bytes into dst,
// rejecting unknown fields. On failure it writes 413 or 400 with a fixed
// message (the decoder's own could quote the body) and returns false.
func decodeReportBody(w http.ResponseWriter, r *http.Request, max int64, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil {
		if extra := dec.Decode(&struct{}{}); !errors.Is(extra, io.EOF) {
			err = errors.New("trailing data")
		}
	}
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("a report is at most %d bytes", max))
		return false
	}
	writeError(w, http.StatusBadRequest, "invalid_request", "request body must be one valid JSON object of the documented shape")
	return false
}

// handleCreateReport serves POST /v1/reports: a person reports a problem.
func (s *Server) handleCreateReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	caller, ok := callerFrom(ctx)
	if !ok {
		// Authentication off: there is nobody to say who reported.
		writeUnauthenticated(w)
		return
	}
	if caller.Kind != store.PrincipalHuman {
		s.auditAgentDenial(ctx, caller.ID, inviteAuditSubject(r), "", 0, denyAgentReports)
		writeError(w, http.StatusForbidden, schema.ErrorCodeHumanRequired, "problem reports are for people")
		return
	}
	if ok, retry := s.reportLimits.take(caller.ID, s.now()); !ok {
		s.refuseReportRateLimited(w, r, caller.ID, retry)
		return
	}
	var req schema.CreateProblemReportRequestV1
	if !decodeReportBody(w, r, schema.MaxProblemReportBytes, &req) {
		return
	}
	// The schema's messages name the field and the rule, never the value.
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	now := s.now()
	report, err := s.store.CreateProblemReport(ctx, auditActor(ctx), store.ProblemReportParams{
		PrincipalID:   caller.ID,
		ServerVersion: s.cfg.Version,
		Voice:         s.voice.health(),
		Request:       req,
		Server:        s.diag.snapshot(caller.ID),
		At:            now,
	})
	if err != nil {
		slog.ErrorContext(ctx, "reports: store failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
		return
	}
	slog.InfoContext(ctx, "reports: received", "principal", caller.ID, "report", report.ID)
	s.metrics.problemReported()
	writeJSON(w, http.StatusCreated, schema.CreateProblemReportResponseV1{ID: report.ID, CreatedAt: report.CreatedAt})
}

// refuseReportRateLimited answers 429 with Retry-After, and audits the refusal
// if it is one to write a row for.
func (s *Server) refuseReportRateLimited(w http.ResponseWriter, r *http.Request, principalID int64, retry time.Duration) {
	l := s.reportLimits
	if write, count := l.audits.admit(principalID, auditReportRateLimited, s.now()); write {
		slog.WarnContext(r.Context(), "reports: rate limit reached", "principal", principalID)
		detail := fmt.Sprintf("limit=%s max=%d window_seconds=%d count=%d", l.limit.name, l.limit.burst, int(l.limit.window/time.Second), count)
		// The caller may already be gone; the refusal must still be recorded.
		actx := context.WithoutCancel(r.Context())
		if _, err := s.store.AppendAuditEvent(actx, fmt.Sprintf("principal:%d", principalID), auditReportRateLimited, inviteAuditSubject(r), detail); err != nil {
			slog.ErrorContext(actx, "reports: audit problem_report_rate_limited failed", "error", err)
		}
	}
	writeRateLimited(w, &rateLimitedError{retryAfter: retry})
}

// handleListReports serves GET /v1/reports[?limit=]: operators only (the
// route table), newest first, without the reports' text.
func (s *Server) handleListReports(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, err := nonNegativeQueryInt64(r, "limit", store.MaxProblemReportList)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	reports, err := s.store.ListProblemReports(ctx, int(min(limit, store.MaxProblemReportList)))
	if err != nil {
		slog.ErrorContext(ctx, "reports: list failed", "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, schema.ListProblemReportsResponseV1{Reports: reports})
}

func writeReportNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, schema.ErrorCodeProblemReportNotFound, "problem report not found")
}

// handleGetReport serves GET /v1/reports/{id}: operators only. The answer
// carries redact, every channel, net and principal name on the server, for
// the client that turns the report into a public issue body.
func (s *Server) handleGetReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "report")
	if !ok {
		return
	}
	report, err := s.store.ProblemReport(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeReportNotFound(w)
		return
	case err != nil:
		slog.ErrorContext(ctx, "reports: read failed", "report", id, "error", err)
		writeInternalError(w)
		return
	}
	// The names an issue made from it must leave out, from the whole server:
	// the operator's own channels are not enough when groups have private
	// ones. Fail closed: a report without them could be filed unredacted.
	redact, err := s.store.ProblemReportRedaction(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "reports: read names to redact failed", "report", id, "error", err)
		writeInternalError(w)
		return
	}
	report.Redact = &redact
	writeJSON(w, http.StatusOK, report)
}

// handleUpdateReport serves PUT /v1/reports/{id}: an operator marks a report
// filed, with the issue it became.
func (s *Server) handleUpdateReport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(w, r, "id", "report")
	if !ok {
		return
	}
	var req schema.UpdateProblemReportRequestV1
	if !decodeReportBody(w, r, reportUpdateMaxBytes, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var operatorID int64
	if caller, ok := callerFrom(ctx); ok {
		operatorID = caller.ID
	}
	report, err := s.store.FileProblemReport(ctx, auditActor(ctx), operatorID, id, req.Issue, s.now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeReportNotFound(w)
	case err != nil:
		slog.ErrorContext(ctx, "reports: file failed", "report", id, "error", err)
		writeInternalError(w)
	default:
		slog.InfoContext(ctx, "reports: filed", "report", id)
		writeJSON(w, http.StatusOK, report)
	}
}
