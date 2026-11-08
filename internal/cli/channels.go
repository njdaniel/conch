package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch channels list|join|leave|create` (ADR-013, issue #994): the channel
// directory and the community's own channel routes. The server decides who
// may, and an ordinary refusal is printed in this file's words (refusal.go).
// A preview refusal keeps the server's code, message and permission.

var channelRefusals = map[string]refusalLines{
	"list": {"forbidden": communityOnly},
	"join": {
		schema.ErrorCodeChannelNotJoinable: "you cannot join this channel: only a person in the community can, and not one removed from it",
	},
	"leave": {
		"forbidden":                       communityOnly,
		schema.ErrorCodeChannelLastMember: "you are the last member of this private channel: someone else must be added before you can leave",
	},
	// One line for every collision: a name in use, one that reads the same,
	// and one in a channel the caller cannot see (community model R2).
	"create": {
		"forbidden":      communityOnly,
		"channel_exists": "there is already a channel with that name, perhaps one you are not in; choose another name",
	},
}

func runChannels(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	const verbs = "list, join, leave, create"
	if len(args) == 0 {
		return fmt.Errorf("cli: channels requires a subcommand (%s)", verbs)
	}
	verb := args[0]
	if _, ok := channelRefusals[verb]; !ok {
		return fmt.Errorf("cli: unknown channels subcommand %q (want %s)", verb, verbs)
	}
	fs := newFlagSet("channels "+verb, stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	asJSON := fs.Bool("json", false, "print the server's response document")
	var all, private *bool
	var previewAs *string
	switch verb {
	case "list":
		all = fs.Bool("all", false, "every channel with its member count, private ones too (operators)")
		previewAs = fs.String("preview-as", "", "read as this member (name or principal id)")
	case "create":
		private = fs.Bool("private", false, "make a private channel: only its members see it (the default is public)")
	}
	// Flags may follow the positionals, as they may for `nets`.
	var pos []string
	rest := args[1:]
	for {
		if err := fs.Parse(rest); err != nil {
			return fmt.Errorf("cli: channels %s: %w", verb, err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	want := 1
	if verb == "list" {
		want = 0
	}
	if len(pos) != want {
		if want == 0 {
			return fmt.Errorf("cli: channels list: takes no arguments (usage: %s)", channelsUsage[verb])
		}
		return fmt.Errorf("cli: channels %s: expected <%s> (usage: %s)", verb, channelsArgName(verb), channelsUsage[verb])
	}
	preview := false
	fs.Visit(func(f *flag.Flag) { preview = preview || f.Name == "preview-as" })
	if preview && *previewAs == "" {
		return fmt.Errorf("cli: channels list: --preview-as requires a member name or id")
	}
	var request schema.CreateChannelRequestV1
	if verb == "create" {
		request = schema.CreateChannelRequestV1{Name: pos[0], Visibility: schema.ChannelPublic}
		if *private {
			request.Visibility = schema.ChannelPrivate
		}
		// The check the server makes, so a name it would refuse costs no request.
		if err := request.Validate(); err != nil {
			return fmt.Errorf("cli: channels create: %w", err)
		}
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	var previewID int64
	if preview {
		previewID, err = resolvePreviewMember(ctx, client, *previewAs)
		if err != nil {
			return err
		}
	}
	out := channelsOutput{stdout: stdout, asJSON: *asJSON}
	lines := channelRefusals[verb]
	if verb == "list" && *all {
		// ?scope=all is an operator's listing.
		lines = refusalLines{"forbidden": operatorOnly}
	}
	switch verb {
	case "list":
		var resp schema.ChannelDirectoryResponseV1
		if preview {
			resp, err = client.ChannelDirectoryPreview(ctx, *all, previewID)
		} else {
			resp, err = client.ChannelDirectory(ctx, *all)
		}
		if err != nil {
			if preview {
				return err
			}
			return refused(err, lines)
		}
		return out.list(resp, *all)
	case "join":
		resp, err := client.JoinChannel(ctx, pos[0])
		if err != nil {
			return refused(err, lines)
		}
		return out.joined(resp)
	case "leave":
		if err := client.LeaveChannel(ctx, pos[0]); err != nil {
			return refused(err, lines)
		}
		return out.say("left channel %s\n", safeLine(pos[0]))
	default: // create
		resp, err := client.CreateChannelV1(ctx, request)
		if err != nil {
			return refused(err, lines)
		}
		return out.created(resp)
	}
}

var channelsUsage = map[string]string{
	"list":   "conch channels list [--server <url>] [--json] [--all] [--preview-as <member>]",
	"join":   "conch channels join [--server <url>] [--json] <channel>",
	"leave":  "conch channels leave [--server <url>] [--json] <channel>",
	"create": "conch channels create [--server <url>] [--json] [--private] <name>",
}

func channelsArgName(verb string) string {
	if verb == "create" {
		return "name"
	}
	return "channel"
}

// channelsOutput prints what the channel verbs answer.
type channelsOutput struct {
	stdout io.Writer
	asJSON bool
}

func (o channelsOutput) say(format string, a ...any) error {
	if o.asJSON {
		return nil
	}
	_, err := fmt.Fprintf(o.stdout, format, a...)
	return err
}

func (o channelsOutput) json(v any) error {
	return jsonLine(o.stdout, v)
}

// list prints `<id> <visibility> <default|-> <joined|-> <members|-> <archived|-> <name>`
// per channel in the server's order, id ascending. archived marks a channel
// that was archived (issue #844), added before the name so the fields before
// it keep their places. Members is a count only an operator's --all
// listing carries. The name is last because it may contain spaces.
func (o channelsOutput) list(resp schema.ChannelDirectoryResponseV1, all bool) error {
	if o.asJSON {
		if resp.Channels == nil {
			resp.Channels = []schema.ChannelListingV1{}
		}
		return o.json(resp)
	}
	for _, c := range resp.Channels {
		members := "-"
		if all && c.Members != nil {
			members = fmt.Sprintf("%d", *c.Members)
		}
		if err := o.say("%d %s %s %s %s %s %s\n", c.ID, safeLine(string(c.Visibility)),
			either(c.Default, "default", "-"), either(c.Joined, "joined", "-"), members, either(c.ArchivedAt != nil, "archived", "-"), safeLine(c.Name)); err != nil {
			return err
		}
	}
	return nil
}

func (o channelsOutput) joined(resp schema.JoinChannelResponseV1) error {
	if o.asJSON {
		return o.json(resp)
	}
	return o.say("you are in channel %s\n", safeLine(resp.Channel.Name))
}

func (o channelsOutput) created(resp schema.CreateChannelResponseV1) error {
	if o.asJSON {
		return o.json(resp)
	}
	kind := ""
	if v := resp.Channel.Visibility; v != "" {
		kind = safeLine(string(v)) + " "
	}
	return o.say("created %schannel %s (id %d)\n", kind, safeLine(resp.Channel.Name), resp.Channel.ID)
}

// jsonLine writes v as one JSON document and a newline.
func jsonLine(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}
