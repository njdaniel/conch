package schema

import (
	"fmt"
	"strings"
)

// Account notices (issue #2042, child of #2013; Nick's decision B of
// 2026-11-19). A notice is something the server tells a member about their
// own account the next time they sign in with a sign-in of their own. The
// first and only kind says a password was set or reset through a link
// another principal made: act_as_members lets an operator make an
// enrolment or reset link for a member, and whoever opens it chooses the
// password, so the member is told, with the name of the principal who made
// the link and when it was used. A notice never names anyone but that
// principal and carries nothing about other members. The shapes are
// additive REST bodies with no schema-name field, versioned by the V1 type
// suffix, as the sign-in link shapes are.

// ErrorCodeNoticeNotFound (HTTP 404): DELETE /v1/notices/mine/{id} named no
// notice of the caller's. Another member's notice, an id never issued and a
// caller who may not read notices all get it, so it says nothing of whether
// the id exists.
const ErrorCodeNoticeNotFound = "notice_not_found"

// AccountNoticeKind is what an account notice says. Clients show a kind
// they do not know by its name rather than refusing the list.
type AccountNoticeKind string

const (
	// AccountNoticeKindPasswordSetByLink: the member's password was set or
	// reset through an enrolment or reset link made by another principal
	// (ByName). The audit log records it too (password_set, password_reset).
	AccountNoticeKindPasswordSetByLink AccountNoticeKind = "password_set_by_link"
)

// Valid reports whether k is a recognized kind.
func (k AccountNoticeKind) Valid() bool {
	switch k {
	case AccountNoticeKindPasswordSetByLink:
		return true
	default:
		return false
	}
}

// AccountNoticeV1 is one notice of the caller's own. ID is what
// DELETE /v1/notices/mine/{id} dismisses. ByName is the name the principal
// who made the link had when the notice was written. At is when the
// password was set.
type AccountNoticeV1 struct {
	ID     int64             `json:"id"`
	Kind   AccountNoticeKind `json:"kind"`
	ByName string            `json:"by_name"`
	At     Timestamp         `json:"at"`
}

// Validate reports whether the notice is well-formed.
func (n AccountNoticeV1) Validate() error {
	if n.ID <= 0 {
		return fmt.Errorf("schema: account notice id must be positive")
	}
	if !n.Kind.Valid() {
		return fmt.Errorf("schema: account notice kind must be one of: %s", strings.Join(Vocabularies["AccountNoticeKind"], ", "))
	}
	return nil
}

// ListAccountNoticesResponseV1 is the response to GET /v1/notices/mine: the
// caller's own notices that they have not dismissed, newest first. Another
// member's notices are never in it, and a sign-in that someone else made for
// the caller (an operator's credential or link) gets an empty list.
// Producers must send an empty slice, not nil.
type ListAccountNoticesResponseV1 struct {
	Notices []AccountNoticeV1 `json:"notices"`
}
