package cli

import "github.com/njdaniel/conch/pkg/schema"

// Size caps for the list reads (issue #1198). A server the client does not
// trust can send an endless body, so each list response is read no further
// than its cap and an over-long one is refused (see cappedReader), never
// decoded from a cut-off prefix.
//
// A cap is rows x a generous per-row bound + envelope slack. Where the schema
// gives a page limit the rows are that limit; the routes with no page limit
// (the channel directory, member links, joiners and channel members all
// answer whole) use listRowCeiling, far above any real community.
const (
	// listRowBound bounds one row: names are at most 100 characters, and a row
	// holds a few more fields. 4 KiB is many times a real row.
	listRowBound = 4 << 10
	// inviteRowBound bounds one member link, which also lists its channels
	// (at most schema.MaxInviteChannels names).
	inviteRowBound = 16 << 10
	// listSlack is room for the envelope around the rows.
	listSlack = 64 << 10
	// listRowCeiling is the row count allowed from a list the server does not
	// page.
	listRowCeiling = 5000

	maxChannelDirectoryBody = listRowCeiling*listRowBound + listSlack
	maxChannelMembersBody   = listRowCeiling*listRowBound + listSlack
	maxMemberLinksBody      = listRowCeiling*inviteRowBound + listSlack
	maxJoinersBody          = listRowCeiling*listRowBound + listSlack
	maxDirectoryMembersBody = schema.MaxListPrincipalsLimit*listRowBound + listSlack
	maxPrincipalsBody       = schema.MaxListPrincipalsLimit*listRowBound + listSlack
	maxGuestsBody           = schema.MaxListGuestsLimit*listRowBound + listSlack
)

// The list responses, each with its size bounded. They decode as the schema
// type; the wrapper only carries the cap.
type (
	channelDirectoryDoc struct {
		schema.ChannelDirectoryResponseV1
	}
	channelMembersDoc struct {
		schema.ListChannelMembersResponseV1
	}
	memberLinksDoc      struct{ schema.ListInvitesResponseV2 }
	joinersDoc          struct{ schema.InviteJoinersResponseV1 }
	directoryMembersDoc struct{ schema.ServerMembersResponseV1 }
	principalsDoc       struct {
		schema.ListPrincipalsResponseV1
	}
	guestsDoc struct{ schema.ListGuestsResponseV1 }
)

func (channelDirectoryDoc) maxBody() int64 { return maxChannelDirectoryBody }
func (channelMembersDoc) maxBody() int64   { return maxChannelMembersBody }
func (memberLinksDoc) maxBody() int64      { return maxMemberLinksBody }
func (joinersDoc) maxBody() int64          { return maxJoinersBody }
func (directoryMembersDoc) maxBody() int64 { return maxDirectoryMembersBody }
func (principalsDoc) maxBody() int64       { return maxPrincipalsBody }
func (guestsDoc) maxBody() int64           { return maxGuestsBody }
