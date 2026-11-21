package schema

import (
	"errors"
	"fmt"
)

// Personal blocks (issue #1832; docs/design/blocks.md). A member keeps a list
// of the principals whose messages they would rather not see or be alerted
// about. It is a preference, not a security boundary: a block removes nobody
// from anything, stops nobody reading anything, and exempts nobody from
// moderation. It changes only what the blocker's own clients show and what
// the server alerts the blocker about.
//
//	GET    /v1/blocks                  -> ListBlocksResponseV1
//	PUT    /v1/blocks/{principal_id}   -> BlockV1 (idempotent)
//	DELETE /v1/blocks/{principal_id}   -> 204 (idempotent)
//
// The list belongs to one principal and is visible only to them: no route
// takes an owner, so no caller can name another's list, and nothing the
// blocked principal can read (a route, a frame, a roster, a count or an
// error) differs because of it. A change sends BlocksChangedV1 to the
// owner's own events sockets, so their other devices re-read the list
// without a reload.
//
// These are new types; nothing published changes (additive, no version bump).
// BlocksChangedV1 is an events document and carries its schema name. The
// rest are REST bodies, versioned by the V1 type suffix, and carry no
// schema-name field.

// BlocksChangedSchemaV1 is the versioned name identifying the v1 block-list
// signal on the events socket.
const BlocksChangedSchemaV1 = "conch.blocks_changed.v1"

// MaxBlocks is how many principals one person's block list may hold. A PUT
// that would add one more is refused with ErrorCodeBlockLimitReached; a PUT
// of a principal already on the list is not an addition and always succeeds.
const MaxBlocks = 1000

// ErrorCodeBlockLimitReached (HTTP 409): the caller's block list already
// holds MaxBlocks principals. The answer is about the caller's own list and
// says nothing about the principal named.
const ErrorCodeBlockLimitReached = "block_limit_reached"

// BlockV1 is one entry of a block list: the principal blocked, as they are
// now (their current name and kind, so a client can show the list without a
// directory, in which a guest or a hidden principal may not appear), and
// when the block was made. A PUT that finds the principal already blocked
// answers the existing entry, with its original CreatedAt.
type BlockV1 struct {
	PrincipalID int64         `json:"principal_id"`
	Name        string        `json:"name"`
	Kind        PrincipalKind `json:"kind"`
	CreatedAt   Timestamp     `json:"created_at"`
}

// Validate reports whether the entry is well-formed.
func (b BlockV1) Validate() error {
	if b.PrincipalID <= 0 {
		return errors.New("schema: block principal_id must be positive")
	}
	if b.Name == "" {
		return errors.New("schema: block name is required")
	}
	if b.Kind != PrincipalHuman && b.Kind != PrincipalAgent {
		return fmt.Errorf("schema: block kind %q is not human or agent", b.Kind)
	}
	if b.CreatedAt.IsZero() {
		return errors.New("schema: block created_at is required")
	}
	return nil
}

// ListBlocksResponseV1 is the response to GET /v1/blocks: the caller's whole
// list, oldest block first, never null, and the most it may hold. A caller
// with no blocks gets an empty list; so does every caller about everyone
// else's.
type ListBlocksResponseV1 struct {
	Blocks []BlockV1 `json:"blocks"`
	Limit  int       `json:"limit"`
}

// Validate reports whether the list is well-formed: present, within Limit,
// each entry valid, and no principal twice.
func (r ListBlocksResponseV1) Validate() error {
	if r.Blocks == nil {
		return errors.New("schema: blocks must be a list, not null or missing")
	}
	if r.Limit <= 0 {
		return errors.New("schema: blocks limit must be positive")
	}
	if len(r.Blocks) > r.Limit {
		return fmt.Errorf("schema: blocks holds %d entries, over its limit %d", len(r.Blocks), r.Limit)
	}
	seen := make(map[int64]struct{}, len(r.Blocks))
	for i, b := range r.Blocks {
		if err := b.Validate(); err != nil {
			return fmt.Errorf("schema: blocks[%d]: %w", i, err)
		}
		if _, dup := seen[b.PrincipalID]; dup {
			return fmt.Errorf("schema: blocks[%d]: principal %d is listed twice", i, b.PrincipalID)
		}
		seen[b.PrincipalID] = struct{}{}
	}
	return nil
}

// BlocksChangedV1 tells a client that its owner's block list may have
// changed. It is sent only to the owner's own human events sockets, never to
// the principal blocked or anyone else. It is a signal, not a delta: it names
// nobody, and a client that receives it re-reads GET /v1/blocks and replaces
// what it holds.
type BlocksChangedV1 struct {
	// Schema is the document discriminator; always BlocksChangedSchemaV1.
	Schema string `json:"schema"`
}

// Validate reports whether the frame is a v1 block-list signal.
func (b BlocksChangedV1) Validate() error {
	if b.Schema != BlocksChangedSchemaV1 {
		return fmt.Errorf("schema: blocks changed schema must be %q, got %q", BlocksChangedSchemaV1, b.Schema)
	}
	return nil
}

// BlockExemptPayload reports whether a message carrying a payload of this
// schema is shown and alerted whatever the reader's blocks: an approval
// request or an admin action is a decision, and a decision is never hidden.
// Clients apply the same rule (web/src/blocks.ts).
func BlockExemptPayload(payloadSchema string) bool {
	switch payloadSchema {
	case ApprovalV1Name, AdminActionV1Name:
		return true
	default:
		return false
	}
}
