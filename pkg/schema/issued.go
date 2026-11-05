package schema

// What an operator issued to others, on the three events that end its power
// to act as people (issue #1024, ADR-014 O13, docs/design/admin-roles.md
// §7.5 and §12 A12). The credentials and hooks an operator issued to
// principals other than itself stay live when act_as_members is taken away,
// when the operator is demoted and when it is disabled: they belong to
// their targets. Each of those events answers with the list, in the one
// shape IssuedByOperatorV1, so the owner can revoke them, one by one through
// the credential and hook routes or all at once through
// POST /v1/principals/{id}/issued/revoke-all. The shapes here are additive
// REST bodies with no schema-name field, versioned by the V1 type suffix.

// DisablePrincipalResponseV1 is the body of POST /v1/principals/{id}/disable.
// IssuedToOthers is filled when the call disabled the principal and it had
// issued live credentials or hooks to principals other than itself: those
// stay live, and the owner may revoke them. Omitted otherwise, on a repeat
// call (which disables nothing), and when there are none. A server before
// this field answered 204 with no body; a client treats any 2xx as done.
type DisablePrincipalResponseV1 struct {
	IssuedToOthers []IssuedByOperatorV1 `json:"issued_to_others,omitempty"`
}

// RevokeIssuedResponseV1 is the body of
// POST /v1/principals/{id}/issued/revoke-all (owner only, recent
// authentication): how many of the credentials and hooks that principal id
// issued to principals other than itself the call revoked. Both are counts
// of rows this call revoked; a repeat answers zeros. An unrevoked credential
// that had expired is revoked and counted too, so the counts can exceed the
// list the owner was shown, which has live items only.
type RevokeIssuedResponseV1 struct {
	CredentialsRevoked int `json:"credentials_revoked"`
	HooksRevoked       int `json:"hooks_revoked"`
}
