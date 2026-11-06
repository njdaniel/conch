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
	maxChannelBansBody      = listRowCeiling*listRowBound + listSlack
	maxMemberLinksBody      = listRowCeiling*inviteRowBound + listSlack
	maxJoinersBody          = listRowCeiling*listRowBound + listSlack
	maxChannelAuthorsBody   = schema.MaxChannelAuthorIDs*listRowBound + listSlack
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

// Size caps for the remaining list reads (issue #1203), built as above.
//
// The reads that take a limit are capped by the page the client asks for:
// rows = the limit asked, clamped to the most the server serves a page (it
// refuses more), times a per-row bound. A limit of zero leaves the page size
// to the server, so it is read as the most. The reads with no page are capped
// at listRowCeiling rows, or at a stated ceiling of their own.
const (
	// maxMessagePage and maxRunPage are the most rows the server returns for
	// the message reads (list, thread, search) and the run reads (runs,
	// activity, interventions).
	maxMessagePage = schema.MaxListMessagesLimit
	maxRunPage     = schema.MaxListRunsLimit
	// maxReportPage is the most reports one list returns.
	maxReportPage = schema.MaxListReportsLimit

	// messageRowBound is a message row. A message has a schema maximum, so a
	// full page of long ones must still read: the stored message as the
	// server sends it, at most schema.MaxMessageFrameBytes.
	messageRowBound = schema.MaxMessageFrameBytes
	// runRowBound bounds a run: a task, a link and an outcome of 512
	// characters each (up to 6 bytes a character once JSON-escaped) and a
	// few scalars, about 10 KiB at worst.
	runRowBound = 16 << 10
	// activityRowBound bounds an activity entry: a summary and an outcome of
	// 512 characters, up to 8 KiB of data and a few scalars, about 15 KiB at
	// worst.
	activityRowBound = 16 << 10
	// interventionRowBound bounds an intervention: a body of 4000 characters
	// and a reason of 512 (up to 6 bytes a character once escaped), about
	// 27 KiB at worst.
	interventionRowBound = 32 << 10
	// reportRowBound bounds a report summary: ids, a state and short
	// strings, and an issue address of at most 512 bytes (3 KiB escaped).
	reportRowBound = 4 << 10
	// netCeiling is the most nets a channel is read with, and netMemberBound
	// the bytes of one member (a principal id and a role). A net lists at
	// most listRowCeiling members, so a net is bounded by that many members
	// and a name; the whole answer is netCeiling of them.
	netCeiling     = 200
	netMemberBound = 128
	netRowBound    = listRowCeiling*netMemberBound + listRowBound

	maxChannelListBody    = listRowCeiling*listRowBound + listSlack
	maxReadStateBody      = listRowCeiling*listRowBound + listSlack
	maxCredentialsBody    = listRowCeiling*listRowBound + listSlack
	maxDeviceLinksBody    = listRowCeiling*listRowBound + listSlack
	maxVoiceOccupancyBody = listRowCeiling*listRowBound + listSlack
	maxNetsBody           = netCeiling*netRowBound + listSlack
)

// pageRows is how many rows a read that asked for limit may get back: limit,
// or max when it asked for none or for more than the server serves.
func pageRows(limit, max int) int {
	if limit < 1 || limit > max {
		return max
	}
	return limit
}

// pageBody is the cap of a page of rows rows of at most bound bytes.
func pageBody(rows int, bound int64) int64 {
	return int64(rows)*bound + listSlack
}

// The list responses, each with its size bounded. They decode as the schema
// type; the wrapper only carries the cap.
type (
	channelDirectoryDoc struct {
		schema.ChannelDirectoryResponseV1
	}
	channelMembersDoc struct {
		schema.ListChannelMembersResponseV1
	}
	channelBansDoc struct {
		schema.ListChannelBansResponseV1
	}
	memberLinksDoc    struct{ schema.ListInvitesResponseV2 }
	channelAuthorsDoc struct {
		schema.ChannelAuthorsResponseV1
	}
	joinersDoc          struct{ schema.InviteJoinersResponseV1 }
	directoryMembersDoc struct{ schema.ServerMembersResponseV1 }
	principalsDoc       struct {
		schema.ListPrincipalsResponseV1
	}
	guestsDoc struct{ schema.ListGuestsResponseV1 }
	hooksDoc  struct{ schema.ListHooksResponseV1 }

	channelListDoc struct{ schema.ListChannelsResponse }
	readStateDoc   struct{ schema.ListReadStateResponseV1 }
	credentialsDoc struct {
		schema.ListCredentialsResponseV1
	}
	deviceLinksDoc struct {
		schema.ListOwnSignInLinksResponseV1
	}
	voiceOccupancyDoc struct{ schema.VoiceOccupancyV1 }
	netsDoc           struct{ schema.ListNetsResponseV1 }

	// The paged reads carry the number of rows asked for.
	messagesV1Doc struct {
		schema.ListMessagesResponseV1
		rows int
	}
	messagesV2Doc struct {
		schema.ListMessagesResponseV2
		rows int
	}
	searchDoc struct {
		schema.SearchMessagesResponseV2
		rows int
	}
	reportsDoc struct {
		schema.ListProblemReportsResponseV1
		rows int
	}
	runsDoc struct {
		schema.ListRunsResponseV1
		rows int
	}
	activityDoc struct {
		schema.ListActivityResponseV1
		rows int
	}
	interventionsDoc struct {
		schema.ListInterventionsResponseV1
		rows int
	}
)

func (channelDirectoryDoc) maxBody() int64 { return maxChannelDirectoryBody }
func (channelMembersDoc) maxBody() int64   { return maxChannelMembersBody }
func (channelBansDoc) maxBody() int64      { return maxChannelBansBody }
func (memberLinksDoc) maxBody() int64      { return maxMemberLinksBody }
func (channelAuthorsDoc) maxBody() int64   { return maxChannelAuthorsBody }
func (joinersDoc) maxBody() int64          { return maxJoinersBody }
func (directoryMembersDoc) maxBody() int64 { return maxDirectoryMembersBody }
func (principalsDoc) maxBody() int64       { return maxPrincipalsBody }
func (guestsDoc) maxBody() int64           { return maxGuestsBody }
func (hooksDoc) maxBody() int64            { return maxHooksBody }

func (channelListDoc) maxBody() int64    { return maxChannelListBody }
func (readStateDoc) maxBody() int64      { return maxReadStateBody }
func (credentialsDoc) maxBody() int64    { return maxCredentialsBody }
func (deviceLinksDoc) maxBody() int64    { return maxDeviceLinksBody }
func (voiceOccupancyDoc) maxBody() int64 { return maxVoiceOccupancyBody }
func (netsDoc) maxBody() int64           { return maxNetsBody }

func (d messagesV1Doc) maxBody() int64 { return pageBody(d.rows, messageRowBound) }
func (d messagesV2Doc) maxBody() int64 { return pageBody(d.rows, messageRowBound) }
func (d searchDoc) maxBody() int64     { return pageBody(d.rows, messageRowBound) }
func (d reportsDoc) maxBody() int64    { return pageBody(d.rows, reportRowBound) }
func (d runsDoc) maxBody() int64       { return pageBody(d.rows, runRowBound) }
func (d activityDoc) maxBody() int64   { return pageBody(d.rows, activityRowBound) }
func (d interventionsDoc) maxBody() int64 {
	return pageBody(d.rows, interventionRowBound)
}
