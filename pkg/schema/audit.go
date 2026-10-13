package schema

import "fmt"

// AuditEventSchemaV1 is the schema name carried by every exported audit event.
const AuditEventSchemaV1 = "conch.audit_event.v1"

// AuditEventV1 is one entry of the append-only audit trail as GET /v1/audit
// exports it: one object per line of an application/x-ndjson stream, in
// ascending id order. Rows are exported as stored. Subject and Detail are
// always present, empty when the row has none.
type AuditEventV1 struct {
	Schema    string    `json:"schema"`
	ID        int64     `json:"id"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Subject   string    `json:"subject"`
	Detail    string    `json:"detail"`
	CreatedAt Timestamp `json:"created_at"`
}

// Validate reports whether the event is structurally well-formed: the v1
// schema name, a positive id, an action and a time. Actor, subject and detail
// are free text the trail keeps as written, so their content is not judged.
func (e AuditEventV1) Validate() error {
	if e.Schema != AuditEventSchemaV1 {
		return fmt.Errorf("schema: audit event schema must be %q, got %q", AuditEventSchemaV1, e.Schema)
	}
	if e.ID <= 0 {
		return fmt.Errorf("schema: audit event id must be positive, got %d", e.ID)
	}
	if e.Action == "" {
		return fmt.Errorf("schema: audit event %d has no action", e.ID)
	}
	if e.CreatedAt.IsZero() {
		return fmt.Errorf("schema: audit event %d has no created_at", e.ID)
	}
	return nil
}
