package schema

import (
	"errors"
	"fmt"
	"sort"
)

// MessageSchemaV2 is the versioned name identifying the v2 message envelope on
// the wire. Every MessageV2 carries it in its Schema field so a decoder can
// recognize the envelope version without out-of-band context.
const MessageSchemaV2 = "conch.message.v2"

// MaxAudiencePrincipals is the most principal ids a principals audience may
// list, counting the author.
const MaxAudiencePrincipals = 64

// AudienceKind discriminates the two scoped audiences (ADR-005). The
// vocabulary is closed: a value outside it fails validation, so an audience a
// reader does not understand is never mistaken for a channel-wide message
// (fail closed). There is no "channel" kind; a channel-wide message carries no
// audience at all.
type AudienceKind string

// Audience kinds.
const (
	// AudienceKindNet scopes a message to the members and monitors of one net.
	AudienceKindNet AudienceKind = "net"
	// AudienceKindPrincipals scopes a message to an explicit list of
	// principals: a whisper.
	AudienceKindPrincipals AudienceKind = "principals"
)

// Valid reports whether k is a recognized audience kind.
func (k AudienceKind) Valid() bool {
	switch k {
	case AudienceKindNet, AudienceKindPrincipals:
		return true
	default:
		return false
	}
}

// Audience is the set of principals a scoped message is delivered to
// (ADR-005). Exactly one of NetID and PrincipalIDs is set, selected by Kind:
//
//	{"kind": "net", "net_id": 3}
//	{"kind": "principals", "principal_ids": [3, 4, 7]}
//
// A net audience carries only the net id; the recipients resolved at post time
// are in the audit log, not on the wire. A principals audience on a message
// read back is the full audience including the author, sorted ascending with
// no duplicates (see Normalize), so a client can reply in kind by reusing the
// list as posted.
type Audience struct {
	// Kind selects the audience shape: AudienceKindNet or AudienceKindPrincipals.
	Kind AudienceKind `json:"kind"`
	// NetID references the net the message is scoped to; set only for Kind net.
	NetID int64 `json:"net_id,omitempty"`
	// PrincipalIDs lists the recipient principals; set only for Kind principals.
	PrincipalIDs []int64 `json:"principal_ids,omitempty"`
}

// Validate reports whether the audience is structurally well-formed: a known
// kind, a positive net_id and no principal_ids for a net, or 1 to
// MaxAudiencePrincipals positive, distinct principal_ids and no net_id for
// principals. Order is not checked here; MessageV2.Validate additionally
// requires the normalized form.
func (a Audience) Validate() error {
	switch a.Kind {
	case AudienceKindNet:
		if a.NetID <= 0 {
			return fmt.Errorf("schema: audience net_id must be positive, got %d", a.NetID)
		}
		if len(a.PrincipalIDs) != 0 {
			return errors.New("schema: audience of kind net must not list principal_ids")
		}
		return nil
	case AudienceKindPrincipals:
		if a.NetID != 0 {
			return errors.New("schema: audience of kind principals must not carry net_id")
		}
		return validateAudiencePrincipalIDs(a.PrincipalIDs)
	default:
		return fmt.Errorf("schema: audience kind %q is not one of net, principals", a.Kind)
	}
}

// validateAudiencePrincipalIDs checks the principal list of a principals
// audience: 1 to MaxAudiencePrincipals positive, distinct ids in any order.
func validateAudiencePrincipalIDs(ids []int64) error {
	if len(ids) == 0 {
		return errors.New("schema: audience of kind principals must list at least one principal_id")
	}
	if len(ids) > MaxAudiencePrincipals {
		return fmt.Errorf("schema: audience lists %d principal_ids, more than the maximum %d", len(ids), MaxAudiencePrincipals)
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return fmt.Errorf("schema: audience principal_id must be positive, got %d", id)
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("schema: audience lists principal_id %d more than once", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// Normalize returns the audience in the form a message read back carries. For
// a principals audience the result lists authorID plus every id in
// PrincipalIDs, sorted ascending with duplicates removed; the receiver is not
// modified. A net audience is returned as is. The server calls this on a post
// request's audience before validating and storing the message.
func (a Audience) Normalize(authorID int64) Audience {
	if a.Kind != AudienceKindPrincipals {
		return a
	}
	ids := make([]int64, 0, len(a.PrincipalIDs)+1)
	ids = append(ids, a.PrincipalIDs...)
	if authorID > 0 {
		ids = append(ids, authorID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	distinct := ids[:0]
	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			distinct = append(distinct, id)
		}
	}
	return Audience{Kind: a.Kind, PrincipalIDs: distinct}
}

// validateNormalized checks the read-back form of a principals audience on top
// of Validate: sorted ascending (which, with distinctness, means strictly
// increasing), containing the author, and containing at least one principal
// other than the author. A whisper to nobody but oneself is not a whisper.
func (a Audience) validateNormalized(authorID int64) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.Kind != AudienceKindPrincipals {
		return nil
	}
	hasAuthor := false
	for i, id := range a.PrincipalIDs {
		if i > 0 && id <= a.PrincipalIDs[i-1] {
			return errors.New("schema: audience principal_ids must be sorted ascending")
		}
		if id == authorID {
			hasAuthor = true
		}
	}
	if !hasAuthor {
		return fmt.Errorf("schema: audience principal_ids must include the author %d", authorID)
	}
	if len(a.PrincipalIDs) < 2 {
		return errors.New("schema: audience principal_ids must include at least one principal other than the author")
	}
	return nil
}

// MessageV2 is the v2 wire representation of a persisted message: MessageV1
// plus an optional Audience (ADR-005). Everything else is unchanged from v1:
// the self-describing Schema discriminator, the rendered Body every message
// has (ADR-000 D8), the optional typed Payload, and the UTC/millisecond
// CreatedAt.
//
// V2 is a new type alongside MessageV1, never an edit of it. The audience is a
// new envelope version rather than an additive v1 field because it changes
// delivery semantics: a v1 client that ignored it would render a whisper as an
// open message and invite a reply in the open. An absent Audience means the
// whole channel, today's behavior.
type MessageV2 struct {
	// Schema is the envelope version discriminator; always MessageSchemaV2.
	Schema string `json:"schema"`
	// ID is the server-assigned message id.
	ID int64 `json:"id"`
	// ChannelID references the channel the message belongs to.
	ChannelID int64 `json:"channel_id"`
	// AuthorID references the authoring principal (human or agent).
	AuthorID int64 `json:"author_id"`
	// CreatedAt is when the server persisted the message (UTC, ms precision).
	CreatedAt Timestamp `json:"created_at"`
	// Body is the rendered, human-readable form of the message; always present.
	Body string `json:"body"`
	// Payload is the optional typed machine payload; nil (omitted) when absent.
	Payload *Payload `json:"payload,omitempty"`
	// Audience scopes delivery to a net or a list of principals; nil (omitted)
	// means the whole channel.
	Audience *Audience `json:"audience,omitempty"`
}

// Validate reports whether the envelope is structurally well-formed. It
// enforces every MessageV1 rule, requires the v2 schema name, and requires a
// present audience to be well-formed and, for a principals audience, in the
// normalized read-back form: sorted ascending, distinct, including the
// author, and including at least one other principal. Unknown payload schemas are tolerated by design (forward
// compatibility); unknown audience kinds are not (fail closed).
func (m MessageV2) Validate() error {
	if m.Schema != MessageSchemaV2 {
		return fmt.Errorf("schema: message schema must be %q, got %q", MessageSchemaV2, m.Schema)
	}
	if m.ID <= 0 {
		return fmt.Errorf("schema: message id must be positive, got %d", m.ID)
	}
	if m.ChannelID <= 0 {
		return fmt.Errorf("schema: message channel_id must be positive, got %d", m.ChannelID)
	}
	if m.AuthorID <= 0 {
		return fmt.Errorf("schema: message author_id must be positive, got %d", m.AuthorID)
	}
	if m.CreatedAt.IsZero() {
		return errors.New("schema: message created_at is required")
	}
	if m.Body == "" {
		return errors.New("schema: message body is required")
	}
	if m.Payload != nil {
		if err := m.Payload.Validate(); err != nil {
			return err
		}
	}
	if m.Audience != nil {
		if err := m.Audience.validateNormalized(m.AuthorID); err != nil {
			return err
		}
	}
	return nil
}

// MessageV2FromV1 converts a v1 envelope to its v2 form: the same fields under
// the v2 schema name, with no audience. Every v1 message is channel-wide, so
// the conversion is lossless; MessageV1FromV2 inverts it.
func MessageV2FromV1(m MessageV1) MessageV2 {
	return MessageV2{
		Schema:    MessageSchemaV2,
		ID:        m.ID,
		ChannelID: m.ChannelID,
		AuthorID:  m.AuthorID,
		CreatedAt: m.CreatedAt,
		Body:      m.Body,
		Payload:   m.Payload,
	}
}

// MessageV1FromV2 converts a channel-wide v2 envelope to its v1 form. A scoped
// message (one with an audience) has no v1 form and is rejected: rendering it
// through v1 would present a whisper as an open message. Callers serving v1
// readers must therefore drop scoped messages rather than downgrade them.
func MessageV1FromV2(m MessageV2) (MessageV1, error) {
	if m.Audience != nil {
		return MessageV1{}, errors.New("schema: a scoped message has no v1 form")
	}
	return MessageV1{
		Schema:    MessageSchemaV1,
		ID:        m.ID,
		ChannelID: m.ChannelID,
		AuthorID:  m.AuthorID,
		CreatedAt: m.CreatedAt,
		Body:      m.Body,
		Payload:   m.Payload,
	}, nil
}

// PostMessageRequestV2 is the request body for posting a v2 message:
// PostMessageRequestV1 plus an optional audience. The server, not the client,
// assigns the message id, channel, timestamp, and envelope schema; a request
// therefore carries none of those. Like PostMessageRequestV1 this is an API
// shape, not a persisted or registered payload.
//
// AuthorID may be zero: the server binds it to the authenticated caller. A
// principals audience may list its ids in any order and may omit the author;
// the server adds the author and sorts with Audience.Normalize before storing,
// so the stored message satisfies MessageV2.Validate. The normalized list must
// still fit within MaxAudiencePrincipals, so a request that lists 64 other
// principals and omits the author is rejected once the author is added.
type PostMessageRequestV2 struct {
	// AuthorID references the authoring principal; zero (omitted) when the
	// server binds it from the caller's credential.
	AuthorID int64 `json:"author_id,omitempty"`
	// Body is the rendered, human-readable form of the message; always present.
	Body string `json:"body"`
	// Payload is the optional typed machine payload; nil (omitted) when absent.
	Payload *Payload `json:"payload,omitempty"`
	// Audience scopes delivery to a net or a list of principals; nil (omitted)
	// means the whole channel.
	Audience *Audience `json:"audience,omitempty"`
}

// Validate reports whether the request is structurally well-formed. Its
// payload rules mirror MessageV2's. A present audience must satisfy
// Audience.Validate: a known kind, a positive net_id, or 1 to
// MaxAudiencePrincipals positive, distinct principal_ids in any order. A
// request that names its author must also list someone else.
func (r PostMessageRequestV2) Validate() error {
	if r.AuthorID < 0 {
		return fmt.Errorf("schema: post message author_id must not be negative, got %d", r.AuthorID)
	}
	if r.Body == "" {
		return errors.New("schema: post message body is required")
	}
	if r.Payload != nil {
		if err := r.Payload.Validate(); err != nil {
			return err
		}
	}
	if r.Audience != nil {
		if err := r.Audience.Validate(); err != nil {
			return err
		}
		// A whisper needs someone other than its author. When the request
		// names its author this is checkable here; when it does not, the
		// server finds out after binding the author, from MessageV2.Validate.
		if r.AuthorID > 0 && r.Audience.Kind == AudienceKindPrincipals &&
			len(r.Audience.PrincipalIDs) == 1 && r.Audience.PrincipalIDs[0] == r.AuthorID {
			return errors.New("schema: audience principal_ids must include at least one principal other than the author")
		}
	}
	return nil
}

// PostMessageResponseV2 is the response body after a v2 message is persisted.
// It embeds the full MessageV2 the server assigned, including any typed
// payload and the normalized audience.
type PostMessageResponseV2 struct {
	Message MessageV2 `json:"message"`
}

// ListMessagesResponseV2 is one forward page of v2 messages visible to the
// caller. NextAfter is the message ID to pass as the next request's after
// parameter; zero means there is no known subsequent page. This mirrors
// ListMessagesResponseV1's pagination convention, carrying MessageV2
// envelopes instead of MessageV1.
type ListMessagesResponseV2 struct {
	Messages  []MessageV2 `json:"messages"`
	NextAfter int64       `json:"next_after,omitempty"`
}
