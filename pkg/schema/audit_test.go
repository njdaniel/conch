package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestAuditEventGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
	}{
		{name: "event", file: "audit-event-v1.json"},
		{name: "empty subject and detail stay present", file: "audit-event-v1-no-subject.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, func() any { return &AuditEventV1{} })
		})
	}
}

// The export puts one object on each line, so the compact form must carry the
// issue's exact field order and no newline.
func TestAuditEventWireLine(t *testing.T) {
	e := AuditEventV1{
		Schema: AuditEventSchemaV1, ID: 42, Actor: "principal:3", Action: "approval_resolved",
		Subject: "approval:7", Detail: "x",
		CreatedAt: NewTimestamp(time.Date(2026, 10, 12, 18, 0, 0, 0, time.UTC)),
	}
	got, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":"conch.audit_event.v1","id":42,"actor":"principal:3","action":"approval_resolved","subject":"approval:7","detail":"x","created_at":"2026-10-12T18:00:00.000Z"}`
	if string(got) != want {
		t.Errorf("line = %s\nwant   %s", got, want)
	}
}

func TestAuditEventValidate(t *testing.T) {
	ok := AuditEventV1{
		Schema: AuditEventSchemaV1, ID: 1, Actor: "system", Action: "x",
		CreatedAt: NewTimestamp(time.Unix(1, 0)),
	}
	tests := []struct {
		name    string
		mutate  func(*AuditEventV1)
		wantErr string
	}{
		{"valid", func(*AuditEventV1) {}, ""},
		{"empty subject and detail", func(e *AuditEventV1) { e.Subject, e.Detail = "", "" }, ""},
		{"wrong schema", func(e *AuditEventV1) { e.Schema = "conch.audit_event.v2" }, "schema"},
		{"no schema", func(e *AuditEventV1) { e.Schema = "" }, "schema"},
		{"zero id", func(e *AuditEventV1) { e.ID = 0 }, "id"},
		{"negative id", func(e *AuditEventV1) { e.ID = -3 }, "id"},
		{"no action", func(e *AuditEventV1) { e.Action = "" }, "action"},
		{"no time", func(e *AuditEventV1) { e.CreatedAt = Timestamp{} }, "created_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ok
			tt.mutate(&e)
			err := e.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate() = %v, want error mentioning %q", err, tt.wantErr)
			}
		})
	}
}
