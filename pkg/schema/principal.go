package schema

import (
	"errors"
	"strings"
	"time"
)

// PrincipalKind distinguishes humans from agents (ADR-000 D1).
type PrincipalKind string

const (
	PrincipalHuman PrincipalKind = "human"
	PrincipalAgent PrincipalKind = "agent"
)

// MaxPrincipalNameLength is the longest allowed principal name, in characters.
const MaxPrincipalNameLength = 100

// PrincipalV0 is the v0 wire representation of a principal.
type PrincipalV0 struct {
	ID        int64         `json:"id"`
	Kind      PrincipalKind `json:"kind"`
	Name      string        `json:"name"`
	CreatedAt time.Time     `json:"created_at"`
}

// CreatePrincipalRequest is the request body for creating a v0 principal.
type CreatePrincipalRequest struct {
	Kind PrincipalKind `json:"kind"`
	Name string        `json:"name"`
}

// Validate reports whether the create request is well-formed: the kind is
// human or agent and the name is non-blank and passes ValidateDisplayName.
// Names stored before the rule existed keep working; it is enforced only on
// creation.
func (r CreatePrincipalRequest) Validate() error {
	if r.Kind != PrincipalHuman && r.Kind != PrincipalAgent {
		return errors.New(`schema: kind must be "human" or "agent"`)
	}
	if strings.TrimSpace(r.Name) == "" {
		return errors.New("schema: principal name must not be empty")
	}
	return ValidateDisplayName("principal name", r.Name, MaxPrincipalNameLength)
}

// CreatePrincipalResponse is the response body after a principal is created.
type CreatePrincipalResponse struct {
	Principal PrincipalV0 `json:"principal"`
}
