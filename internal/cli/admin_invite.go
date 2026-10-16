package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch admin invite …` and the principal commands that administer invites
// (issue #438). Each is one request to the REST API.
//
// An invite's code is a secret like a credential token. `invite create`
// prints it once, inside the link, alone on standard output, so that
// LINK=$(conch admin invite create ...) works. No other output, error or log
// line of this file carries it.

// inviteOpts holds the flags of the invite commands.
type inviteOpts struct {
	open, personal bool
	channels       stringList
	expires        string
	// maxUses is -1 when --max-uses was not given.
	maxUses        int64
	voice          bool
	mayInvite      bool
	credentialDays int64
	state          string
	createdBy      int64
	yes            bool
	all            bool
}

// stringList is a flag that may be given more than once.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func bindInviteCreate(fs *flag.FlagSet, o *adminOpts) {
	fs.BoolVar(&o.invite.open, "open", false, "an open link: anyone with it may join until it expires")
	fs.BoolVar(&o.invite.personal, "personal", false, "a personal invite: one person, once")
	fs.Var(&o.invite.channels, "channel", "a channel the joiner is added to (repeat for more)")
	fs.StringVar(&o.invite.expires, "expires", "", "when it stops working: a duration such as 24h or 7d, or an RFC 3339 time")
	fs.Int64Var(&o.invite.maxUses, "max-uses", -1, "for an open link, how many may join through it")
	fs.BoolVar(&o.invite.voice, "voice", false, "the joiner may use voice")
	fs.BoolVar(&o.invite.mayInvite, "may-invite", false, "the joiner may invite others in turn")
	fs.Int64Var(&o.invite.credentialDays, "credential-days", 0, "how many days the joiner's first credential lasts")
}

func bindInviteList(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.invite.state, "state", "", "only invites in this state: open, used, expired or closed")
	fs.StringVar(&o.kind, "kind", "", "only invites of this kind: personal or open")
	fs.Int64Var(&o.invite.createdBy, "created-by", 0, "only invites this principal made")
}

func bindYes(fs *flag.FlagSet, o *adminOpts) {
	fs.BoolVar(&o.invite.yes, "yes", false, "do it; without it, only say how many it would disable")
}

func bindDisableInvitees(fs *flag.FlagSet, o *adminOpts) {
	fs.BoolVar(&o.invite.all, "all", false, "everyone under the principal, not only whom they invited themselves")
	bindYes(fs, o)
}

// parseInviteExpiry reads --expires: a Go duration (24h), a number of days
// (7d), or an RFC 3339 time. The error names the forms, not the value.
func parseInviteExpiry(s string, now time.Time) (time.Time, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		if n, err := strconv.Atoi(days); err == nil && n > 0 {
			return now.Add(time.Duration(n) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("--expires must be a duration such as 24h or 7d, or an RFC 3339 time such as 2026-12-31T00:00:00Z")
}

// inviteCreateRequest builds the request from the flags and checks it as the
// server would, so a mistake costs no request.
func inviteCreateRequest(o inviteOpts, now time.Time) (schema.CreateInviteRequestV1, error) {
	var req schema.CreateInviteRequestV1
	switch {
	case o.open == o.personal:
		return req, errors.New("give exactly one of --open and --personal")
	case o.open:
		req.Kind = schema.InviteKindOpen
	default:
		req.Kind = schema.InviteKindPersonal
	}
	req.Channels = append([]string(nil), o.channels...)
	exp, err := parseInviteExpiry(o.expires, now)
	if err != nil {
		return req, err
	}
	ts := schema.NewTimestamp(exp)
	req.ExpiresAt = &ts
	if o.maxUses != -1 {
		n := o.maxUses
		req.MaxUses = &n
	}
	req.Voice = o.voice
	if o.mayInvite {
		yes := true
		req.MayInvite = &yes
	}
	if o.credentialDays != 0 {
		if o.credentialDays < 0 || o.credentialDays > schema.MaxInviteCredentialLifetimeSeconds/86400 {
			return req, fmt.Errorf("--credential-days must be between 1 and %d", schema.MaxInviteCredentialLifetimeSeconds/86400)
		}
		secs := o.credentialDays * 86400
		req.CredentialLifetimeSeconds = &secs
	}
	return req, req.Validate(now)
}

// inviteLink is the address a person opens to use an invite. The code is in
// the fragment, which a browser never sends to a server.
func (r *adminRun) inviteLink(code string) string {
	return r.client.resolve("app").String() + "/#/invite/" + code
}

func adminInviteCreate(ctx context.Context, r *adminRun) error {
	req, err := inviteCreateRequest(r.opts.invite, time.Now())
	if err != nil {
		return fmt.Errorf("cli: admin invite create: %w", err)
	}
	resp, err := r.client.CreateInvite(ctx, req)
	if err != nil {
		return err
	}
	// The code is the one server string printed raw, so it is checked first.
	// The error says nothing of what was received.
	if schema.ValidateInviteCode(resp.Code) != nil {
		return errors.New("cli: admin invite create: the server returned a code in an unexpected form; it was not printed")
	}
	if r.opts.asJSON {
		return r.printJSON(resp)
	}
	if _, err := fmt.Fprintln(r.stdout, r.inviteLink(resp.Code)); err != nil {
		return err
	}
	_, err = fmt.Fprintf(r.stderr, "invite %d %s %s %s\n", resp.Invite.ID, safeLine(string(resp.Invite.Kind)),
		usesText(resp.Invite), "expires "+formatTimestamp(resp.Invite.ExpiresAt))
	return err
}

// inviteState is where an invite is, as the server's list filter decides it:
// closed before used, used before expired.
func inviteState(inv schema.InviteV1, now time.Time) string {
	switch {
	case inv.ClosedAt != nil:
		return "closed"
	case inv.MaxUses != nil && inv.Uses >= *inv.MaxUses:
		return "used"
	case !inv.ExpiresAt.Time().After(now):
		return "expired"
	default:
		return "open"
	}
}

func usesText(inv schema.InviteV1) string {
	if inv.MaxUses == nil {
		return fmt.Sprintf("uses %d", inv.Uses)
	}
	return fmt.Sprintf("uses %d/%d", inv.Uses, *inv.MaxUses)
}

// adminInviteList prints
// `<id> <kind> <state> uses <n>[/<max>] expires <time> by <principal-id> channels <name,...>`
// per invite, newest first. Channel names are last because they may contain
// spaces.
func adminInviteList(ctx context.Context, r *adminRun) error {
	if r.opts.invite.createdBy < 0 {
		return errors.New("cli: admin invite list: --created-by must be a positive integer")
	}
	resp, err := r.client.ListInvites(ctx, InviteListFilter{Kind: r.opts.kind, State: r.opts.invite.state, CreatedBy: r.opts.invite.createdBy})
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		if resp.Invites == nil {
			resp.Invites = []schema.InviteV1{}
		}
		return r.printJSON(resp)
	}
	now := time.Now()
	for _, inv := range resp.Invites {
		names := make([]string, len(inv.Channels))
		for i, c := range inv.Channels {
			names[i] = safeLine(c.Name)
		}
		if _, err := fmt.Fprintf(r.stdout, "%d %s %s %s expires %s by %d channels %s\n",
			inv.ID, safeLine(string(inv.Kind)), inviteState(inv, now), usesText(inv), formatTimestamp(inv.ExpiresAt),
			inv.CreatedBy, dashIfEmpty(strings.Join(names, ","))); err != nil {
			return err
		}
	}
	return nil
}

func adminInviteClose(ctx context.Context, r *adminRun) error {
	if err := r.client.CloseInvite(ctx, r.id); err != nil {
		return err
	}
	return r.say("closed invite %d\n", r.id)
}

// adminInviteJoiners prints `<id> <enabled|disabled> <joined> <name>` per
// person who joined through the invite.
func adminInviteJoiners(ctx context.Context, r *adminRun) error {
	resp, err := r.client.InviteJoiners(ctx, r.id)
	if err != nil {
		return err
	}
	if r.opts.asJSON {
		if resp.Joiners == nil {
			resp.Joiners = []schema.InviteJoinerV1{}
		}
		return r.printJSON(resp)
	}
	for _, j := range resp.Joiners {
		status := "enabled"
		if j.Disabled {
			status = "disabled"
		}
		if _, err := fmt.Fprintf(r.stdout, "%d %s %s %s\n", j.ID, status, formatTimestamp(j.CreatedAt), safeLine(j.Name)); err != nil {
			return err
		}
	}
	return nil
}

// sayBulkDisable reports a bulk disable, or with a dry run what it would do.
func (r *adminRun) sayBulkDisable(resp schema.DisableJoinersResponseV1, also string) error {
	if r.opts.asJSON {
		if resp.PrincipalIDs == nil {
			resp.PrincipalIDs = []int64{}
		}
		return r.printJSON(resp)
	}
	ids := make([]string, len(resp.PrincipalIDs))
	for i, id := range resp.PrincipalIDs {
		ids[i] = strconv.FormatInt(id, 10)
	}
	list := dashIfEmpty(strings.Join(ids, ","))
	if resp.DryRun {
		return r.say("would disable %d principal(s): %s; run again with --yes to do it%s\n", resp.Count, list, also)
	}
	return r.say("disabled %d principal(s): %s\n", resp.Count, list)
}

// adminInviteDisableJoiners disables everyone who joined through the invite
// and closes it. Without --yes it only asks the server how many that is.
func adminInviteDisableJoiners(ctx context.Context, r *adminRun) error {
	resp, err := r.client.DisableJoiners(ctx, r.id, schema.DisableJoinersRequestV1{DryRun: !r.opts.invite.yes})
	if err != nil {
		return err
	}
	if err := r.sayBulkDisable(resp, fmt.Sprintf(" and close invite %d", r.id)); err != nil {
		return err
	}
	if !resp.DryRun {
		return r.say("closed invite %d\n", r.id)
	}
	return nil
}

// adminPrincipalDisableInvitees disables the people the principal invited,
// or with --all everyone under them. Without --yes it only asks the server
// how many that is.
func adminPrincipalDisableInvitees(ctx context.Context, r *adminRun) error {
	req := schema.DisableInviteesRequestV1{DryRun: !r.opts.invite.yes}
	if r.opts.invite.all {
		req.Depth = schema.InviteeDepthAll
	}
	resp, err := r.client.DisableInvitees(ctx, r.id, req)
	if err != nil {
		return err
	}
	return r.sayBulkDisable(resp, "")
}

// adminPrincipalInvitePermission grants (on) or takes away (off) a person's
// right to make personal invites.
func adminPrincipalInvitePermission(ctx context.Context, r *adminRun) error {
	var may bool
	switch r.pos[1] {
	case "on":
		may = true
	case "off":
	default:
		return errors.New("cli: admin principal invite-permission: the second argument must be on or off")
	}
	if err := r.client.SetInvitePermission(ctx, r.id, may); err != nil {
		return err
	}
	if may {
		return r.say("%d may invite\n", r.id)
	}
	return r.say("%d may no longer invite; their open personal invites are closed\n", r.id)
}

func (r *adminRun) sayInviteSettings(s schema.InviteSettingsV1) error {
	if r.opts.asJSON {
		return r.printJSON(s)
	}
	if s.Paused {
		return r.say("invites are paused\n")
	}
	return r.say("invites are open\n")
}

func adminInvitePause(ctx context.Context, r *adminRun) error {
	s, err := r.client.PutInviteSettings(ctx, schema.InviteSettingsV1{Paused: true})
	if err != nil {
		return err
	}
	return r.sayInviteSettings(s)
}

func adminInviteResume(ctx context.Context, r *adminRun) error {
	s, err := r.client.PutInviteSettings(ctx, schema.InviteSettingsV1{Paused: false})
	if err != nil {
		return err
	}
	return r.sayInviteSettings(s)
}

func adminInviteStatus(ctx context.Context, r *adminRun) error {
	s, err := r.client.InviteSettings(ctx)
	if err != nil {
		return err
	}
	return r.sayInviteSettings(s)
}
