package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/pkg/schema"
)

var reactionRefusals = refusalLines{
	schema.ErrorCodeMessageNotFound:      "no such message, or you cannot see it",
	schema.ErrorCodeInvalidReaction:      "name exactly one emoji, or a known shortcode such as :thumbsup:",
	schema.ErrorCodeReactionNotPermitted: "you may read this message but may not react to it",
	schema.ErrorCodeTooManyReactions:     "the message or your own reactions have reached the server's limit",
	schema.ErrorCodeReactionsRequireAuth: "reactions need a signed-in identity",
}

func runReact(ctx context.Context, args []string, stdout, stderr io.Writer, set bool) error {
	verb := "unreact"
	if set {
		verb = "react"
	}
	fs := newFlagSet(verb, stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	asJSON := fs.Bool("json", false, "print the reaction summary as JSON")
	text := fs.Bool("text", false, "print shortcodes instead of emoji glyphs")
	pos, err := reactionPositionals(fs, args)
	if err != nil {
		return fmt.Errorf("cli: %s: %w", verb, err)
	}
	if len(pos) != 3 {
		return fmt.Errorf("cli: %s: expected <channel> <message-id> <emoji-or-shortcode>", verb)
	}
	messageID, err := positiveID(pos[1])
	if err != nil {
		return fmt.Errorf("cli: %s: message id must be a positive integer", verb)
	}
	key, err := ResolveReactionKey(pos[2])
	if err != nil {
		return fmt.Errorf("cli: %s: %w", verb, err)
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	var summary schema.ReactionSummaryV1
	if set {
		summary, err = client.SetReaction(ctx, pos[0], messageID, key)
	} else {
		summary, err = client.RemoveReaction(ctx, pos[0], messageID, key)
	}
	if err != nil {
		return refused(err, reactionRefusals)
	}
	if *asJSON {
		return jsonLine(stdout, summary)
	}
	return printReactionSummary(stdout, summary, *text)
}

func runReactions(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("reactions", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	asJSON := fs.Bool("json", false, "print each reactor record as JSON Lines")
	text := fs.Bool("text", false, "print shortcodes instead of emoji glyphs")
	limit := fs.Int("limit", schema.MaxReactorLimit, "reactors per request (1 to 100)")
	pos, err := reactionPositionals(fs, args)
	if err != nil {
		return fmt.Errorf("cli: reactions: %w", err)
	}
	if len(pos) != 2 && len(pos) != 3 {
		return errors.New("cli: reactions: expected <channel> <message-id> [<emoji-or-shortcode>]")
	}
	messageID, err := positiveID(pos[1])
	if err != nil || *limit < 1 || *limit > schema.MaxReactorLimit {
		return errors.New("cli: reactions: message id must be positive and --limit must be 1 to 100")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	summary, err := client.ReactionSummary(ctx, pos[0], messageID)
	if err != nil {
		return refused(err, reactionRefusals)
	}
	if len(pos) == 3 {
		key, err := ResolveReactionKey(pos[2])
		if err != nil {
			return fmt.Errorf("cli: reactions: %w", err)
		}
		count := schema.ReactionCountV1{Emoji: key}
		for _, existing := range summary.Reactions {
			if existing.Emoji == key {
				count = existing
			}
		}
		summary.Reactions = []schema.ReactionCountV1{count}
	}
	if len(summary.Reactions) == 0 || summary.Reactions[0].Count == 0 {
		if !*asJSON {
			_, err := fmt.Fprintln(stdout, "no reactions")
			return err
		}
		return nil
	}
	names, err := client.ListChannelMembers(ctx, pos[0])
	if err != nil {
		return refused(err, reactionRefusals)
	}
	byID := make(map[int64]schema.ChannelMemberV1, len(names.Members))
	for _, member := range names.Members {
		byID[member.PrincipalID] = member
	}
	for _, count := range summary.Reactions {
		if !*asJSON {
			if _, err := fmt.Fprintf(stdout, "%s %d %s\n", ReactionLabel(count.Emoji, *text), count.Count, ownReaction(count.Me)); err != nil {
				return err
			}
		}
		page, err := client.ListReactors(ctx, pos[0], messageID, count.Emoji, 0, *limit)
		if err != nil {
			return refused(err, reactionRefusals)
		}
		if err := printReactorPages(ctx, client, stdout, pos[0], messageID, count, byID, *asJSON, *limit, page); err != nil {
			return err
		}
	}
	return nil
}

func printReactorPages(ctx context.Context, client *Client, stdout io.Writer, channel string, messageID int64, count schema.ReactionCountV1, byID map[int64]schema.ChannelMemberV1, asJSON bool, limit int, page schema.ListReactorsResponseV1) error {
	var err error
	for {
		for _, reactor := range page.Reactors {
			if asJSON {
				if err := jsonLine(stdout, reactor); err != nil {
					return err
				}
				continue
			}
			name := "hidden-" + strconv.FormatInt(reactor.PrincipalID, 10)
			if member, ok := byID[reactor.PrincipalID]; ok {
				name = member.Name
				if member.Guest != nil {
					name = schema.GuestDisplayName(member.Guest.Label, member.Guest.Handle)
				}
			}
			if reactor.GuestReactor != nil {
				name = schema.GuestDisplayName(reactor.GuestReactor.Label, reactor.GuestReactor.Handle)
			}
			if _, err := fmt.Fprintf(stdout, "  %d %s %s\n", reactor.PrincipalID, safeLine(name), formatTimestamp(reactor.CreatedAt)); err != nil {
				return err
			}
		}
		if page.NextAfter == 0 {
			return nil
		}
		page, err = client.ListReactors(ctx, channel, messageID, count.Emoji, page.NextAfter, limit)
		if err != nil {
			return refused(err, reactionRefusals)
		}
	}
}

func printReactionSummary(w io.Writer, summary schema.ReactionSummaryV1, text bool) error {
	if len(summary.Reactions) == 0 {
		_, err := fmt.Fprintf(w, "message %d: no reactions\n", summary.MessageID)
		return err
	}
	parts := make([]string, 0, len(summary.Reactions))
	for _, count := range summary.Reactions {
		parts = append(parts, fmt.Sprintf("%s x%d %s", ReactionLabel(count.Emoji, text), count.Count, ownReaction(count.Me)))
	}
	_, err := fmt.Fprintf(w, "message %d: %s\n", summary.MessageID, strings.Join(parts, "  "))
	return err
}

func ownReaction(me bool) string {
	if me {
		return "you"
	}
	return "-"
}

func reactionPositionals(fs interface {
	Parse([]string) error
	NArg() int
	Arg(int) string
	Args() []string
}, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func positiveID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("not a positive integer")
	}
	return id, nil
}

func adminReactionRemove(ctx context.Context, r *adminRun) error {
	messageID, err := positiveID(r.pos[1])
	if err != nil {
		return errors.New("cli: admin reaction remove: message-id must be a positive integer")
	}
	key := ""
	if len(r.pos) == 4 {
		key, err = ResolveReactionKey(r.pos[3])
		if err != nil {
			return fmt.Errorf("cli: admin reaction remove: %w", err)
		}
	}
	if !r.opts.reactionYes {
		question := fmt.Sprintf("Remove every reaction by principal %d on message %d?", r.id, messageID)
		if key != "" {
			question = fmt.Sprintf("Remove principal %d's %s reaction on message %d?", r.id, ReactionLabel(key, true), messageID)
		}
		ok, err := r.prompt.confirm(question + " The person may react again.")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("reaction removal not confirmed; nothing changed")
		}
	}
	summary, err := r.client.ModerateReaction(ctx, r.pos[0], messageID, r.id, key)
	if err != nil {
		return refused(err, reactionRefusals)
	}
	if r.opts.asJSON {
		return r.printJSON(summary)
	}
	return printReactionSummary(r.stdout, summary, false)
}
