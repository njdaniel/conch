package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

var moderationRefusals = refusalLines{
	schema.ErrorCodeVoiceUnavailable: "voice removal is incomplete; the participant may not have been removed; retry the command",
	schema.ErrorCodeMessageNotFound:  "no such message, or you cannot see it; nothing was changed",
}

func confirmModeration(prompt *prompter, yes bool, question string) error {
	if yes {
		return nil
	}
	if !prompt.interactive() {
		return errors.New("cli: moderation needs confirmation on a terminal, or --yes for non-interactive use; nothing changed")
	}
	ok, err := prompt.confirm(question)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("moderation not confirmed; nothing changed")
	}
	return nil
}

func runVoiceKick(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlagSet("voice kick", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	yes := fs.Bool("yes", false, "remove without asking")
	pos, err := reactionPositionals(fs, args)
	if err != nil {
		return fmt.Errorf("cli: voice kick: %w", err)
	}
	if len(pos) != 2 {
		return errors.New("cli: voice kick: expected <channel> <principal-id>")
	}
	principalID, err := positiveID(pos[1])
	if err != nil {
		return errors.New("cli: voice kick: principal-id must be a positive integer")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	channel := safeLine(pos[0])
	question := fmt.Sprintf("Remove principal %d from voice in %s now. They can rejoin; chat access is unchanged.", principalID, channel)
	if err := confirmModeration(newPrompter(ctx, stdin, stderr), *yes, question); err != nil {
		return err
	}
	if err := client.KickVoiceParticipant(ctx, pos[0], principalID); err != nil {
		return refused(err, moderationRefusals)
	}
	_, err = fmt.Fprintf(stdout, "voice removal completed for principal %d in %s (they may already have been absent); they can rejoin; chat access is unchanged\n", principalID, channel)
	return err
}

func adminBanList(ctx context.Context, r *adminRun) error {
	result, err := r.client.ListChannelBans(ctx, r.pos[0])
	if err != nil {
		return refused(err, moderationRefusals)
	}
	if r.opts.asJSON {
		return r.printJSON(result)
	}
	if len(result.Bans) == 0 {
		return r.say("no bans in %s\n", safeLine(r.pos[0]))
	}
	for _, ban := range result.Bans {
		createdBy := "unknown"
		if ban.CreatedBy > 0 {
			createdBy = strconv.FormatInt(ban.CreatedBy, 10)
		}
		if err := r.say("%d %s banned-by %s at %s\n", ban.PrincipalID, safeLine(ban.Name), createdBy, ban.CreatedAt.Time().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}

func adminBanAdd(ctx context.Context, r *adminRun) error {
	channel := safeLine(r.pos[0])
	question := fmt.Sprintf("Remove principal %d from %s and block this principal from being added again. Other channels are unaffected. A new account or guest visit is not identified by this ban. If this is you, you will lose access and need an operator to unban and re-add you.", r.id, channel)
	if err := confirmModeration(r.prompt, r.opts.moderationYes, question); err != nil {
		return err
	}
	if err := r.client.BanChannelMember(ctx, r.pos[0], r.id); err != nil {
		return refused(err, moderationRefusals)
	}
	return r.say("ban is in force for principal %d in %s (it may already have been); membership and nets were removed; other channels are unaffected\n", r.id, channel)
}

func adminBanRemove(ctx context.Context, r *adminRun) error {
	channel := safeLine(r.pos[0])
	question := fmt.Sprintf("Lift the ban on principal %d in %s. This does not re-add them or restore any rights.", r.id, channel)
	if err := confirmModeration(r.prompt, r.opts.moderationYes, question); err != nil {
		return err
	}
	if err := r.client.UnbanChannelMember(ctx, r.pos[0], r.id); err != nil {
		return refused(err, moderationRefusals)
	}
	return r.say("principal %d is unbanned or was already not banned in %s; membership and rights are not restored\n", r.id, channel)
}

func adminMessageRemove(ctx context.Context, r *adminRun) error {
	channel := safeLine(r.pos[0])
	question := fmt.Sprintf("Remove message %d in %s for everyone in its audience. Future reads show a tombstone; server backups and copies already received cannot be recalled.", r.id, channel)
	if err := confirmModeration(r.prompt, r.opts.moderationYes, question); err != nil {
		return err
	}
	if err := r.client.RemoveMessage(ctx, r.pos[0], r.id); err != nil {
		return refused(err, moderationRefusals)
	}
	return r.say("message %d in %s is removed or was already removed; future reads show a tombstone and received copies are not erased\n", r.id, channel)
}
