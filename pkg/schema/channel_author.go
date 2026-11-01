package schema

// A channel's authors (issue #978, docs/design/community-model.md §13.5): a
// message's author stays named in the history of the channel it was posted
// in, for everyone who can read the message, whatever the member directory
// shows. Clients name authors from the channel's member list; this route
// names the ones who have left it, or whom the reader may not otherwise
// know. These are additive REST bodies with no schema-name field, versioned
// by the V1 type suffix.

// MaxChannelAuthorIDs is the most ids one GET
// /v1/channels/{channel}/authors?ids= may ask about.
const MaxChannelAuthorIDs = 100

// ChannelAuthorV1 is one author of GET /v1/channels/{channel}/authors: the
// principal's current name and kind, and nothing more: no role, no join
// date, no channels, no voice, and nothing that says whether the principal
// is still on the server. A hidden author's name is its placeholder
// (HiddenPrincipalNamePrefix and its id), as everywhere.
type ChannelAuthorV1 struct {
	PrincipalID int64         `json:"principal_id"`
	Name        string        `json:"name"`
	Kind        PrincipalKind `json:"kind"`
}

// ChannelAuthorsResponseV1 is the body of GET
// /v1/channels/{channel}/authors?ids=<id>,<id>…, behind the channel gate of
// the member list (404 to a non-member; an agent needs messages.read). It
// names each asked id that authored at least one message the caller may
// read in that channel; every other id, known or not, existing or not, is
// absent. Authors is ordered by principal_id and is always a JSON array,
// never null: producers send an empty slice.
type ChannelAuthorsResponseV1 struct {
	Authors []ChannelAuthorV1 `json:"authors"`
}
