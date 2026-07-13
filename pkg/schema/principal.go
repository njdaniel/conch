package schema

import "time"

// PrincipalKind distinguishes humans from agents (ADR-000 D1).
type PrincipalKind string

const (
	PrincipalHuman PrincipalKind = "human"
	PrincipalAgent PrincipalKind = "agent"
)

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

// CreatePrincipalResponse is the response body after a principal is created.
type CreatePrincipalResponse struct {
	Principal PrincipalV0 `json:"principal"`
}
