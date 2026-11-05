package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin channel settings`, `conch admin principal voice` and `conch
// admin directory members` (ADR-013, issue #994). Each is one or more
// requests to a public route; the server decides who may, and a refusal is
// printed in this client's words, by its code (refusal.go).
//
// `conch admin directory` is a group, not a verb, so that `settings
// [--hide-members on|off]` (issue #996) has its place beside `members`.

// communityOpts holds the flags of these commands.
type communityOpts struct {
	public, private bool
	// defaultMark is on, off or "" (not given).
	defaultMark string
	// archive and restore are the archive flags of `channel settings`
	// (issue #844): one request field, archived, true and false.
	archive, restore bool
}

func bindChannelSettings(fs *flag.FlagSet, o *adminOpts) {
	fs.BoolVar(&o.community.public, "public", false, "make the channel public: every member can find and join it")
	fs.BoolVar(&o.community.private, "private", false, "make the channel private: only its members see it; they all keep their place")
	fs.StringVar(&o.community.defaultMark, "default", "", "on or off: whether new members start in the channel (a default channel is public)")
	fs.BoolVar(&o.community.archive, "archive", false, "archive the channel: it keeps its history and members and refuses anything new until restored")
	fs.BoolVar(&o.community.restore, "restore", false, "restore an archived channel: posting, joining and voice work again")
}

var channelSettingsRefusals = refusalLines{
	"forbidden":                             operatorOnly,
	schema.ErrorCodeChannelSettingsConflict: "a private channel cannot be a default channel, and a default channel must be public: nothing was changed",
	schema.ErrorCodeChannelArchiveDefault:   "a default channel cannot be archived: turn off its default mark first (--default off): nothing was changed",
}

// channelSettingsRequest builds the change from the flags and checks it as the
// server will.
func channelSettingsRequest(o communityOpts) (schema.ChannelSettingsRequestV1, error) {
	var req schema.ChannelSettingsRequestV1
	if o.public && o.private {
		return req, errors.New("give --public or --private, not both")
	}
	switch {
	case o.public:
		v := schema.ChannelPublic
		req.Visibility = &v
	case o.private:
		v := schema.ChannelPrivate
		req.Visibility = &v
	}
	if o.archive && o.restore {
		return req, errors.New("give --archive or --restore, not both")
	}
	switch {
	case o.archive:
		v := true
		req.Archived = &v
	case o.restore:
		v := false
		req.Archived = &v
	}
	var err error
	if req.Default, err = onOff("default", o.defaultMark); err != nil {
		return req, err
	}
	if req.Visibility == nil && req.Default == nil && req.Archived == nil {
		return req, errors.New("give at least one of --public, --private, --default, --archive and --restore")
	}
	return req, req.Validate()
}

// adminChannelSettings sets a channel's visibility and default mark. The route
// takes the channel's id, so the name is looked up in the operator's directory
// listing first; a name that is not there is the 404 line a channel nobody can
// see gets.
func adminChannelSettings(ctx context.Context, r *adminRun) error {
	const cmd = "admin channel settings"
	req, err := channelSettingsRequest(r.opts.community)
	if err != nil {
		return fmt.Errorf("cli: %s: %w", cmd, err)
	}
	listing, err := r.client.ChannelDirectory(ctx, true)
	if err != nil {
		return refused(err, channelSettingsRefusals)
	}
	var id int64
	for _, c := range listing.Channels {
		if c.Name == r.pos[0] {
			id = c.ID
			break
		}
	}
	if id <= 0 {
		// The line the server's own 404 gets, so a channel that is not there
		// and one the caller cannot see read the same.
		return &refusalError{line: commonRefusals["channel_not_found"]}
	}
	resp, err := r.client.PutChannelSettings(ctx, id, req)
	if err != nil {
		return refused(err, channelSettingsRefusals)
	}
	if r.opts.asJSON {
		return r.printJSON(resp)
	}
	return r.sayChannelSettings(req, resp)
}

// sayChannelSettings says what a change did: the channel as it now is, that a
// channel turned private kept every member, and whether the change turned open
// sign-up off. It prints only what the server's answer and the request say.
func (r *adminRun) sayChannelSettings(req schema.ChannelSettingsRequestV1, resp schema.ChannelSettingsResponseV1) error {
	name := safeLine(resp.Channel.Name)
	if req.Visibility != nil {
		if resp.Channel.Visibility == schema.ChannelPrivate {
			if err := r.say("channel %s is now private; its members were kept and still see it\n", name); err != nil {
				return err
			}
		} else {
			if err := r.say("channel %s is now %s\n", name, safeLine(string(resp.Channel.Visibility))); err != nil {
				return err
			}
		}
	}
	if req.Default != nil {
		if err := r.say("channel %s is %s\n", name, either(resp.Channel.Default, "now a default channel: new members start in it", "no longer a default channel")); err != nil {
			return err
		}
	}
	if req.Archived != nil {
		if err := r.say("channel %s is %s\n", name, either(resp.Channel.ArchivedAt != nil, "now archived: it keeps its history and members and refuses anything new until it is restored", "restored: posting, joining and voice work again")); err != nil {
			return err
		}
	}
	if resp.OpenSignupTurnedOff {
		if err := r.say("open sign-up is now off: no channel is a default channel\n"); err != nil {
			return err
		}
	}
	for _, w := range resp.Warnings {
		if err := r.say("%s\n", warningLine(w)); err != nil {
			return err
		}
	}
	return nil
}

var principalVoiceRefusals = refusalLines{
	"forbidden":                voiceForbidden,
	schema.ErrorCodeNotAPerson: "agents do not use voice: only a person has a voice switch",
}

// adminPrincipalVoice turns one person's voice on or off. The server asks an
// operator signed in with a password for a recent proof; askToReauth gives it.
func adminPrincipalVoice(ctx context.Context, r *adminRun) error {
	var allowed bool
	switch r.pos[1] {
	case "on":
		allowed = true
	case "off":
	default:
		return errors.New("cli: admin principal voice: the second argument must be on or off")
	}
	resp, err := r.client.SetVoicePermission(ctx, r.id, allowed)
	if err != nil {
		return refused(err, principalVoiceRefusals)
	}
	if r.opts.asJSON {
		return r.printJSON(resp)
	}
	state := either(allowed, "on", "off")
	if !resp.Changed {
		return r.say("voice was already %s for %d; nothing changed\n", state, r.id)
	}
	return r.say("voice is now %s for %d\n", state, r.id)
}

// maxDirectoryPages bounds how many pages `directory members` follows (see
// maxListPages).
const maxDirectoryPages = maxListPages

var directoryRefusals = refusalLines{"forbidden": communityOnly}

// adminDirectoryMembers lists the community, following the server's pages to
// the end. It prints
// `<id> <kind> <role|-> <owner|-> created <t> voice <on|off|-> may-invite <on|off|-> invited-by <id|-> <name>`
// per member; the operator fields are what the server sends a viewer holding
// manage_members, and "-" where it sent none. The name is last because it may
// contain spaces.
func adminDirectoryMembers(ctx context.Context, r *adminRun) error {
	all := []schema.ServerMemberV1{}
	var scope schema.DirectoryScope
	var after int64
	for page := 0; ; page++ {
		if page == maxDirectoryPages {
			return errors.New("cli: admin directory members: the server's pages do not end")
		}
		resp, err := r.client.DirectoryMembers(ctx, after, schema.MaxListPrincipalsLimit)
		if err != nil {
			return refused(err, directoryRefusals)
		}
		all = append(all, resp.Members...)
		scope = resp.Scope
		if resp.After == 0 {
			break
		}
		// Pages go up by id; one that does not would never end.
		if resp.After <= after {
			return errors.New("cli: admin directory members: the server's pages do not move forward")
		}
		after = resp.After
	}
	if r.opts.asJSON {
		return r.printJSON(schema.ServerMembersResponseV1{Members: all, Scope: scope})
	}
	for _, m := range all {
		if err := r.say("%d %s %s %s created %s voice %s may-invite %s invited-by %s %s\n", m.ID, safeLine(string(m.Kind)),
			dashIfEmpty(safeLine(string(m.Role))), either(m.Owner, "owner", "-"), formatTimestamp(m.CreatedAt),
			optionalOnOff(m.VoiceAllowed), optionalOnOff(m.MayInvite), optionalID(m.InvitedBy), safeLine(m.Name)); err != nil {
			return err
		}
	}
	return nil
}

// optionalOnOff is a switch the server may not have sent: "-" then.
func optionalOnOff(b *bool) string {
	if b == nil {
		return "-"
	}
	return onOffText(*b)
}

// optionalID is an id the server may not have sent: "-" then.
func optionalID(id *int64) string {
	if id == nil {
		return "-"
	}
	return strconv.FormatInt(*id, 10)
}
