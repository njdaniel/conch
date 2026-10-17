package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func testProblemReport(principalID int64, at time.Time) ProblemReportParams {
	return ProblemReportParams{
		PrincipalID:   principalID,
		ServerVersion: "v1.0.0",
		Voice:         schema.HealthVoiceUnavailable,
		Request: schema.CreateProblemReportRequestV1{
			Did: "did", Expected: "expected", Happened: "happened", Often: schema.ProblemReportOftenOnce,
			Client: schema.ProblemReportClientV1{Name: "conch", Version: "dev-1", Platform: "linux/amd64"},
		},
		Server: schema.ProblemReportServerV1{
			Requests:     []schema.ServerRequestTraceV1{{At: schema.NewTimestamp(at), RequestID: "0123456789abcdef", Route: "GET /v1/channels", Status: 200}},
			SocketCloses: []schema.ServerSocketCloseV1{},
		},
		At: at,
	}
}

// A report is stored with its audit row, read back whole, listed newest first
// and filed once an operator has made it an issue.
func TestProblemReportStore(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "tess")
	if err != nil {
		t.Fatal(err)
	}
	op, _, _, err := s.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)

	first, err := s.CreateProblemReport(ctx, principalActor(p.ID), testProblemReport(p.ID, t0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateProblemReport(ctx, principalActor(p.ID), testProblemReport(p.ID, t0.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID <= 0 || second.ID <= first.ID || first.State != schema.ProblemReportStateOpen {
		t.Fatalf("created %+v, %+v", first, second)
	}
	if first.Diagnostics == nil {
		t.Error("diagnostics are null, want an empty list")
	}

	got, err := s.ProblemReport(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID || got.Did != "did" || got.Voice != schema.HealthVoiceUnavailable || got.ServerVersion != "v1.0.0" ||
		len(got.Server.Requests) != 1 || !got.CreatedAt.Time().Equal(t0) {
		t.Errorf("read back = %+v", got)
	}
	if _, err := s.ProblemReport(ctx, 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown report: %v, want ErrNotFound", err)
	}

	list, err := s.ListProblemReports(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != second.ID || list[1].ClientName != "conch" || list[1].Often != schema.ProblemReportOftenOnce {
		t.Errorf("list = %+v", list)
	}
	if one, _ := s.ListProblemReports(ctx, 1); len(one) != 1 {
		t.Errorf("limit 1 gave %d", len(one))
	}

	filedAt := t0.Add(time.Hour)
	filed, err := s.FileProblemReport(ctx, principalActor(op.ID), op.ID, first.ID, "https://example.com/issues/1", filedAt)
	if err != nil {
		t.Fatal(err)
	}
	if filed.State != schema.ProblemReportStateFiled || filed.Issue != "https://example.com/issues/1" || filed.FiledAt == nil || !filed.FiledAt.Time().Equal(filedAt) || filed.Did != "did" {
		t.Errorf("filed = %+v", filed)
	}
	if _, err := s.FileProblemReport(ctx, principalActor(op.ID), op.ID, 9999, "https://example.com/issues/1", filedAt); !errors.Is(err, ErrNotFound) {
		t.Errorf("filing an unknown report: %v, want ErrNotFound", err)
	}

	events, err := s.ListAuditEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var reported, filedRows int
	for _, e := range events {
		switch e.Action {
		case AuditProblemReported:
			reported++
			if e.Actor != principalActor(p.ID) || e.Detail != "" {
				t.Errorf("reported row = %+v", e)
			}
		case AuditProblemReportFiled:
			filedRows++
			if e.Subject != fmt.Sprintf("report:%d", first.ID) || e.Detail != "" {
				t.Errorf("filed row = %+v", e)
			}
		}
	}
	if reported != 2 || filedRows != 1 {
		t.Errorf("audit rows: %d reported, %d filed", reported, filedRows)
	}
}

// The migration on a database from before it: it adds the table and nothing
// else changes; the table refuses a filed report without its issue.
func TestProblemReportsMigration(t *testing.T) {
	const pre = problemReportsMigration - 1
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "conch.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < pre; i++ {
		for _, stmt := range migrations[i] {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("migration %d: %v", i+1, err)
			}
		}
		if step := migrationSteps[i+1]; step != nil {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := step(ctx, tx); err != nil {
				t.Fatalf("migration step %d: %v", i+1, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, pre)); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = 'problem_reports'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("problem_reports before the migration: %d, %v", tables, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open (migrate): %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != len(migrations) {
		t.Fatalf("user_version = %d, %v; want %d", version, err, len(migrations))
	}
	p, err := s.CreatePrincipal(ctx, PrincipalHuman, "tess")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateProblemReport(ctx, principalActor(p.ID), testProblemReport(p.ID, time.Now())); err != nil {
		t.Fatalf("a report after the migration: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE problem_reports SET state = 'filed'`); err == nil {
		t.Error("a filed report without an issue was accepted")
	}
	if _, err := s.db.Exec(`INSERT INTO problem_reports (principal_id, created_at, client_name, client_version, server_version, often, document)
		VALUES (9999, 1, 'x', 'x', 'x', 'once', '{}')`); err == nil {
		t.Error("a report by nobody was accepted")
	}
}
