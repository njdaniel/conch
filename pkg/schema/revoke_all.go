package schema

// RevokeAllCredentialsResponseV1 is the body of
// POST /v1/principals/{id}/credentials/revoke-all: how many live credentials
// the call revoked. It is additive and carries no schema-name field; it is
// versioned by its V1 suffix.
type RevokeAllCredentialsResponseV1 struct {
	Revoked int `json:"revoked"`
}
