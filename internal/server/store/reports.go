package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Problem reports (issue #446). A person's report is stored as the document
// an operator reads, with its own audit row; an operator later marks it filed
// with the issue it became. conchd never sends a report anywhere.

// problemReportsMigration is the number of the migration that adds the
// problem_reports table. It is a constant so that renumbering it, when
// another migration lands first, is one line here and one place in the list.
const problemReportsMigration = 29

const (
	// AuditProblemReported is a person sending a report: actor the person,
	// subject report:<id>, no detail. What the report says is in the report,
	// never in the audit log.
	AuditProblemReported = "problem_reported"
	// AuditProblemReportFiled is an operator marking a report filed: actor
	// the operator, subject report:<id>, no detail.
	AuditProblemReportFiled = "problem_report_filed"
)

// MaxProblemReportList is the most reports one list returns.
const MaxProblemReportList = 200

func problemReportSubject(id int64) string { return fmt.Sprintf("report:%d", id) }

// ProblemReportParams is a report as the server assembled it: what the person
// sent and what the server adds.
type ProblemReportParams struct {
	PrincipalID   int64
	ServerVersion string
	Voice         schema.HealthVoice
	Request       schema.CreateProblemReportRequestV1
	Server        schema.ProblemReportServerV1
	At            time.Time
}

// CreateProblemReport stores a report and its problem_reported audit row in
// one transaction, and returns it as stored. The request is validated by the
// caller (schema.CreateProblemReportRequestV1.Validate).
func (s *Store) CreateProblemReport(ctx context.Context, actor string, p ProblemReportParams) (schema.ProblemReportV1, error) {
	diagnostics := p.Request.Diagnostics
	if diagnostics == nil {
		diagnostics = []schema.DiagnosticEventV1{}
	}
	doc := schema.ProblemReportV1{
		State:         schema.ProblemReportStateOpen,
		CreatedAt:     schema.NewTimestamp(p.At),
		PrincipalID:   p.PrincipalID,
		ServerVersion: p.ServerVersion,
		Voice:         p.Voice,
		Did:           p.Request.Did,
		Expected:      p.Request.Expected,
		Happened:      p.Request.Happened,
		Often:         p.Request.Often,
		Client:        p.Request.Client,
		Diagnostics:   diagnostics,
		Server:        p.Server,
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return schema.ProblemReportV1{}, fmt.Errorf("store: encode problem report: %w", err)
	}
	err = s.withImmediateTx(ctx, func(tx execer) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO problem_reports (principal_id, created_at, client_name, client_version, server_version, often, document)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			p.PrincipalID, p.At.UnixMilli(), p.Request.Client.Name, p.Request.Client.Version, p.ServerVersion, string(p.Request.Often), string(raw))
		if err != nil {
			return fmt.Errorf("store: insert problem report: %w", err)
		}
		if doc.ID, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: problem report id: %w", err)
		}
		return appendAuditEventTx(ctx, tx, actor, AuditProblemReported, problemReportSubject(doc.ID), "", p.At)
	})
	if err != nil {
		return schema.ProblemReportV1{}, err
	}
	return doc, nil
}

// ListProblemReports returns the newest reports first, at most limit of them
// (MaxProblemReportList when limit is out of range).
func (s *Store) ListProblemReports(ctx context.Context, limit int) ([]schema.ProblemReportSummaryV1, error) {
	if limit <= 0 || limit > MaxProblemReportList {
		limit = MaxProblemReportList
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, state, created_at, principal_id, client_name, client_version, server_version, often, issue
		 FROM problem_reports ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list problem reports: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schema.ProblemReportSummaryV1{}
	for rows.Next() {
		var r schema.ProblemReportSummaryV1
		var created int64
		var state, often string
		var issue sql.NullString
		if err := rows.Scan(&r.ID, &state, &created, &r.PrincipalID, &r.ClientName, &r.ClientVersion, &r.ServerVersion, &often, &issue); err != nil {
			return nil, fmt.Errorf("store: scan problem report: %w", err)
		}
		r.State, r.Often = schema.ProblemReportState(state), schema.ProblemReportOften(often)
		r.CreatedAt = schema.NewTimestamp(time.UnixMilli(created))
		r.Issue = issue.String
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list problem reports: %w", err)
	}
	return out, nil
}

// ProblemReport returns one report, or ErrNotFound.
func (s *Store) ProblemReport(ctx context.Context, id int64) (schema.ProblemReportV1, error) {
	return problemReportTx(ctx, s.db, id)
}

func problemReportTx(ctx context.Context, q execer, id int64) (schema.ProblemReportV1, error) {
	var state, document string
	var issue sql.NullString
	var filedAt sql.NullInt64
	err := q.QueryRowContext(ctx, `SELECT state, document, issue, filed_at FROM problem_reports WHERE id = ?`, id).
		Scan(&state, &document, &issue, &filedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return schema.ProblemReportV1{}, ErrNotFound
	}
	if err != nil {
		return schema.ProblemReportV1{}, fmt.Errorf("store: read problem report: %w", err)
	}
	var doc schema.ProblemReportV1
	if err := json.Unmarshal([]byte(document), &doc); err != nil {
		return schema.ProblemReportV1{}, fmt.Errorf("store: decode problem report %d: %w", id, err)
	}
	// The columns are what changes after the report is written.
	doc.ID, doc.State, doc.Issue = id, schema.ProblemReportState(state), issue.String
	doc.FiledAt = msToTimestampPtr(filedAt)
	return doc, nil
}

// ProblemReportRedaction is every name a public issue body made from a report
// must leave out: the names of all channels and nets, and of all principals,
// disabled ones too, each list sorted and without repeats. It is read when an
// operator reads a report, never stored with one.
func (s *Store) ProblemReportRedaction(ctx context.Context) (schema.ProblemReportRedactionV1, error) {
	names := func(what, query string) ([]string, error) {
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("store: list %s names: %w", what, err)
		}
		defer func() { _ = rows.Close() }()
		out := []string{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, fmt.Errorf("store: scan %s name: %w", what, err)
			}
			out = append(out, n)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("store: list %s names: %w", what, err)
		}
		return out, nil
	}
	channels, err := names("channel", `SELECT name FROM channels UNION SELECT name FROM nets ORDER BY 1`)
	if err != nil {
		return schema.ProblemReportRedactionV1{}, err
	}
	principals, err := names("principal", `SELECT DISTINCT name FROM principals ORDER BY 1`)
	if err != nil {
		return schema.ProblemReportRedactionV1{}, err
	}
	return schema.ProblemReportRedactionV1{Channels: channels, Principals: principals}, nil
}

// FileProblemReport marks report id filed as issue, with its audit row, and
// returns the report as it now is. A report already filed takes the new
// issue: the operator filed it again. It returns ErrNotFound when there is no
// such report.
func (s *Store) FileProblemReport(ctx context.Context, actor string, operatorID, id int64, issue string, at time.Time) (schema.ProblemReportV1, error) {
	var doc schema.ProblemReportV1
	err := s.withImmediateTx(ctx, func(tx execer) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE problem_reports SET state = 'filed', issue = ?, filed_at = ?, filed_by = ? WHERE id = ?`,
			issue, at.UnixMilli(), nullableID(operatorID), id)
		if err != nil {
			return fmt.Errorf("store: file problem report: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: file problem report: %w", err)
		} else if n == 0 {
			return ErrNotFound
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditProblemReportFiled, problemReportSubject(id), "", at); err != nil {
			return err
		}
		doc, err = problemReportTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return schema.ProblemReportV1{}, err
	}
	return doc, nil
}
