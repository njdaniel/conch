package cli

import "github.com/njdaniel/conch/pkg/schema"

// The TUI's community panes (issue #995) say a refusal in the same words as
// the commands do: the page's own sentence for the server's code, never the
// server's text. This file only exports those sentences; the commands in
// channels.go and admin_community.go keep their own.

// Community operations a pane can refuse. They name the line sets below.
const (
	CommunityOpChannels = "channels" // reading the channel directory
	CommunityOpJoin     = "join"
	CommunityOpLeave    = "leave"
	CommunityOpCreate   = "create"
	CommunityOpMembers  = "members" // reading the community's members
	CommunityOpVoice    = "voice"   // an operator's voice switch
)

// communityRefusals is the sentences of each operation, the command's own
// where there is one. The TUI cannot prompt for a password, so its
// reauthentication line points at the command instead.
var communityRefusals = map[string]refusalLines{
	CommunityOpChannels: channelRefusals["list"],
	CommunityOpJoin:     channelRefusals["join"],
	CommunityOpLeave:    channelRefusals["leave"],
	CommunityOpCreate:   channelRefusals["create"],
	CommunityOpMembers:  directoryRefusals,
	CommunityOpVoice: {
		schema.ErrorCodeNotAPerson: principalVoiceRefusals[schema.ErrorCodeNotAPerson],
		// A forbidden answer to an operator is the owner protection of
		// ADR-010: only an owner may act on an owner.
		"forbidden": "only an owner may change an owner's voice",
	},
}

// tuiReauth is the reauthentication sentence of the TUI.
const tuiReauth = "this needs your password first: run 'conch reauth' in another terminal (a script credential cannot; sign in with 'conch login --password'), then try again"

// CommunityRefusal turns a server refusal of op into an error that reads in
// this client's words, chosen by the refusal's code. Any other error (a
// network failure, an expired sign-in) is returned unchanged. The result
// still unwraps to the server's error, so IsServerCode tells the cause.
func CommunityRefusal(op string, err error) error {
	lines := refusalLines{schema.ErrorCodeReauthRequired: tuiReauth}
	for code, line := range communityRefusals[op] {
		lines[code] = line
	}
	return refused(err, lines)
}
