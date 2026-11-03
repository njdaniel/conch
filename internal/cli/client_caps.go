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
	// listRowBound bounds a row that carries one name (at most 100
	// characters, so 400 bytes of UTF-8) and a few scalars: about 600 bytes at
	// worst, so 1 KiB is still about twice a real row.
	listRowBound = 1 << 10
	// guestRowBound bounds a guest row, which carries three strings (handle,
	// label and tag).
	guestRowBound = 4 << 10
	// inviteRowBound bounds one member link, which lists its channels and its
	// landing channels (each at most schema.MaxInviteChannels names): about
	// 27 KiB at worst.
	inviteRowBound = 32 << 10
	// listSlack is room for the envelope around the rows.
	listSlack = 64 << 10
	// listRowCeiling is the row count allowed from a list the server does not
	// page.
	listRowCeiling = 2000
	// maxListPages bounds how many pages a paged list is followed for, so a
	// server whose cursor always advances cannot keep the client reading, and
	// holding, for ever: 250 pages of 200 rows is 50,000, far above a
	// self-hosted community.
	maxListPages = 250

	maxChannelDirectoryBody = listRowCeiling*listRowBound + listSlack
	maxChannelMembersBody   = listRowCeiling*listRowBound + listSlack
	maxMemberLinksBody      = listRowCeiling*inviteRowBound + listSlack
	maxJoinersBody          = listRowCeiling*listRowBound + listSlack
	maxDirectoryMembersBody = schema.MaxListPrincipalsLimit*listRowBound + listSlack
	maxPrincipalsBody       = schema.MaxListPrincipalsLimit*listRowBound + listSlack
	maxGuestsBody           = schema.MaxListGuestsLimit*guestRowBound + listSlack
	// hookRowBound bounds a hook row, which carries two names (its channel
	// and its label, each at most 100 characters, escaped up to six bytes a
	// character for <, > and &) beside ids and times: about 1.3 KiB at worst.
	hookRowBound = 2 << 10
	// maxHooksBody: the hook list is not paged and keeps revoked hooks (issue
	// #1228), so it is read to listRowCeiling rows of hookRowBound.
	maxHooksBody = listRowCeiling*hookRowBound + listSlack
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
	hooksDoc  struct{ schema.ListHooksResponseV1 }
)

func (channelDirectoryDoc) maxBody() int64 { return maxChannelDirectoryBody }
func (channelMembersDoc) maxBody() int64   { return maxChannelMembersBody }
func (memberLinksDoc) maxBody() int64      { return maxMemberLinksBody }
func (joinersDoc) maxBody() int64          { return maxJoinersBody }
func (directoryMembersDoc) maxBody() int64 { return maxDirectoryMembersBody }
func (principalsDoc) maxBody() int64       { return maxPrincipalsBody }
func (guestsDoc) maxBody() int64           { return maxGuestsBody }
func (hooksDoc) maxBody() int64            { return maxHooksBody }
