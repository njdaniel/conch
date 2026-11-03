package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"slices"
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
	expires string
	// uses is -1 when --uses was not given.
	uses           int64
	neverExpires   bool
	credentialDays int64
	// retired are the per-channel flags of the old invite, which a member
	// link has no use for, in the order they were given.
	retired   []string
	state     string
	createdBy int64
	yes       bool
	all       bool
}

// stringList is a flag that may be given more than once.
type stringList []string

func (l *stringList) String() string { return strings.Join(*l, ",") }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

func bindInviteCreate(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.invite.expires, "expires", "", "when it stops working: a duration such as 24h or 7d, or an RFC 3339 time (default: the server's, 7d unless an operator changed it; at most a year)")
	fs.Int64Var(&o.invite.uses, "uses", -1, "how many people may join through it (default: no cap)")
	fs.BoolVar(&o.invite.neverExpires, "never-expires", false, "a link that never expires (operators)")
	fs.Int64Var(&o.invite.credentialDays, "credential-days", 0, "how many days the joiner's first credential lasts")
	// The old per-channel flags are kept only to say what replaced them.
	for _, name := range []string{"open", "personal", "voice", "may-invite"} {
		fs.BoolFunc(name, "retired: a link is a member link", func(string) error { o.invite.retired = append(o.invite.retired, "--"+name); return nil })
	}
	for _, name := range []string{"channel", "max-uses", "join-as"} {
		fs.Func(name, "retired: a link is a member link", func(string) error { o.invite.retired = append(o.invite.retired, "--"+name); return nil })
	}
}

// retiredInviteError names what replaced the old per-channel invite flags.
func retiredInviteError(flags []string) error {
	replacement := "a link is a member link: it lands the joiner in the default channels, and --expires <duration>, --uses <n> and --never-expires are its only settings"
	if slices.Contains(flags, "--max-uses") {
		replacement += " (--max-uses is --uses)"
	}
	return fmt.Errorf("cli: admin invite create: %s no longer exist; %s", strings.Join(flags, ", "), replacement)
}

func bindInviteList(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.invite.state, "state", "", "only links in this state: open, used, expired or closed")
	fs.Int64Var(&o.invite.createdBy, "created-by", 0, "only links this principal made")
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

// inviteCreateRequest builds the member-link request from the flags and
// checks it as the server would, so a mistake costs no request.
func inviteCreateRequest(o inviteOpts, now time.Time) (schema.CreateInviteRequestV2, error) {
	req := schema.CreateInviteRequestV2{Kind: schema.InviteKindMember, NeverExpires: o.neverExpires}
	if len(o.retired) > 0 {
		return req, retiredInviteError(o.retired)
	}
	if o.expires != "" {
		if o.neverExpires {
			return req, errors.New("give --expires or --never-expires, not both")
		}
		exp, err := parseInviteExpiry(o.expires, now)
		if err != nil {
			return req, err
		}
		ts := schema.NewTimestamp(exp)
		req.ExpiresAt = &ts
	}
	if o.uses != -1 {
		n := o.uses
		req.MaxUses = &n
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

var inviteRefusals = refusalLines{
	schema.ErrorCodeInviteKindRetired:         "per-channel invites are retired: make a member link with 'conch admin invite create'",
	schema.ErrorCodeInvitesPaused:             "invites are paused; an operator resumes them with 'conch admin invite resume'",
	schema.ErrorCodeInviteNotAllowed:          "you may not make invite links; ask an operator",
	schema.ErrorCodeInviteAllowanceExceeded:   "you have made as many invite links as you may; close one or ask an operator",
	schema.ErrorCodeInviteGrantExceedsInviter: "that link would give more than you have; leave out what you cannot give",
	schema.ErrorCodeInviteNotFound:            "no such link, or it is not yours to see",
}

func adminInviteCreate(ctx context.Context, r *adminRun) error {
	req, err := inviteCreateRequest(r.opts.invite, time.Now())
	if err != nil {
		if len(r.opts.invite.retired) > 0 {
			return err
		}
		return fmt.Errorf("cli: admin invite create: %w", err)
	}
	resp, err := r.client.CreateMemberLink(ctx, req)
	if err != nil {
		return refused(err, inviteRefusals)
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
	_, err = fmt.Fprintf(r.stderr, "invite %d %s %s expires %s\n", resp.Invite.ID, safeLine(string(resp.Invite.Kind)),
		usesText(resp.Invite), linkExpiryText(resp.Invite))
	return err
}

// expiryText is when a link stops working: "never" for one with no expiry.
func linkExpiryText(inv schema.InviteV2) string {
	if inv.ExpiresAt == nil || inv.ExpiresAt.Time().UnixMilli() >= schema.NoExpiryMillis {
		return "never"
	}
	return formatTimestamp(*inv.ExpiresAt)
}

// inviteState is where a link is, as the server's states are decided: closed
// before used, used before expired.
func inviteState(inv schema.InviteV2, now time.Time) string {
	switch {
	case inv.ClosedAt != nil:
		return "closed"
	case inv.MaxUses != nil && inv.Uses >= *inv.MaxUses:
		return "used"
	case linkExpiryText(inv) != "never" && !inv.ExpiresAt.Time().After(now):
		return "expired"
	default:
		return "open"
	}
}

func usesText(inv schema.InviteV2) string {
	if inv.MaxUses == nil {
		return fmt.Sprintf("uses %d", inv.Uses)
	}
	return fmt.Sprintf("uses %d/%d", inv.Uses, *inv.MaxUses)
}

// adminInviteList prints
// `<id> <kind> <state> uses <n>[/<max>] joiners <n> expires <time|never> by <principal-id> other <n> channels <name,...>`
// per link, newest first. Channels are the ones a link names (a legacy
// invite's, or the landing channels an operator chose), "-" for a link that
// lands in the default channels; other is how many more the server left out
// because the caller is not in them. Channel names are last because they may
// contain spaces.
func adminInviteList(ctx context.Context, r *adminRun) error {
	o := r.opts.invite
	if o.createdBy < 0 {
		return errors.New("cli: admin invite list: --created-by must be a positive integer")
	}
	switch o.state {
	case "", "open", "used", "expired", "closed":
	default:
		return errors.New("cli: admin invite list: --state must be open, used, expired or closed")
	}
	resp, err := r.client.ListMemberLinks(ctx)
	if err != nil {
		return refused(err, inviteRefusals)
	}
	now := time.Now()
	shown := make([]schema.InviteV2, 0, len(resp.Invites))
	for _, inv := range resp.Invites {
		if (o.createdBy > 0 && inv.CreatedBy != o.createdBy) || (o.state != "" && inviteState(inv, now) != o.state) {
			continue
		}
		shown = append(shown, inv)
	}
	if r.opts.asJSON {
		return r.printJSON(schema.ListInvitesResponseV2{Invites: shown})
	}
	for _, inv := range shown {
		chans := inv.LandingChannels
		if len(chans) == 0 {
			chans = inv.Channels
		}
		names := make([]string, len(chans))
		for i, c := range chans {
			names[i] = safeLine(c.Name)
		}
		if _, err := fmt.Fprintf(r.stdout, "%d %s %s %s joiners %d expires %s by %d other %d channels %s\n",
			inv.ID, safeLine(string(inv.Kind)), inviteState(inv, now), usesText(inv), inv.Joiners, linkExpiryText(inv),
			inv.CreatedBy, inv.OtherChannels, dashIfEmpty(strings.Join(names, ","))); err != nil {
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
	resp, err := r.client.MemberLinkJoiners(ctx, r.id)
	if err != nil {
		return refused(err, inviteRefusals)
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

// Sign-in links (issue #493). A sign-in link's code is a secret like an
// invite's: `principal sign-in-link` prints it once, inside the link, alone
// on standard output, so that LINK=$(conch admin principal sign-in-link 7)
// works. No other output, error or log line carries it.

func bindSignInLink(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.principal.expires, "expires", "", "when it stops working: a duration such as 24h or 2d, or an RFC 3339 time (default 24h, at most 7d; a reset or enrollment link 10m, at most 1h)")
	fs.StringVar(&o.principal.purpose, "purpose", "", "what it is for: sign_in (the default), reset (a forgotten password) or enrollment (a first password)")
}

// signInLink is the address the person opens to sign back in. The code is in
// the fragment, which a browser never sends to a server.
func (r *adminRun) signInLink(code string) string {
	return r.client.resolve("app").String() + "/#/signin/" + code
}

// adminPrincipalSignInLink makes a one-time link that signs a member back
// into their own account, and prints it. With --purpose reset it is a link
// that only replaces a forgotten password (issue #715; the same landing page
// tells the two apart), and with --purpose enrollment a code that lets a
// member from before passwords set a first one: it has no page, so the code
// alone is printed, for `conch password set --proof`.
func adminPrincipalSignInLink(ctx context.Context, r *adminRun) error {
	req := schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurpose(r.opts.principal.purpose)}
	now := time.Now()
	if r.opts.principal.expires != "" {
		exp, err := parseInviteExpiry(r.opts.principal.expires, now)
		if err != nil {
			return fmt.Errorf("cli: admin principal sign-in-link: %w", err)
		}
		ts := schema.NewTimestamp(exp)
		req.ExpiresAt = &ts
	}
	// The check the server makes, so a link too long-lived costs no request.
	if err := req.Validate(now); err != nil {
		return fmt.Errorf("cli: admin principal sign-in-link: %w", err)
	}
	resp, err := r.client.CreateSignInLink(ctx, r.id, req)
	if err != nil {
		return err
	}
	// The code is the one server string printed raw, so it is checked first.
	// The error says nothing of what was received.
	if schema.ValidateSignInLinkCode(resp.Code) != nil {
		return errors.New("cli: admin principal sign-in-link: the server returned a code in an unexpected form; it was not printed")
	}
	if r.opts.asJSON {
		return r.printJSON(resp)
	}
	what, out := "sign-in link", r.signInLink(resp.Code)
	switch req.EffectivePurpose() {
	case schema.SignInLinkPurposeReset:
		what = "password reset link"
	case schema.SignInLinkPurposeEnrollment:
		what, out = "enrollment code", resp.Code
	}
	if _, err := fmt.Fprintln(r.stdout, out); err != nil {
		return err
	}
	_, err = fmt.Fprintf(r.stderr, "%s %d for principal %d, one use, expires %s\n",
		what, resp.SignInLink.ID, resp.SignInLink.PrincipalID, formatTimestamp(resp.SignInLink.ExpiresAt))
	return err
}

// adminSignInLinkClose closes a sign-in link so it no longer works: what an
// operator does when one leaks. A link already used or closed is left as it
// is, and the answer is the same.
func adminSignInLinkClose(ctx context.Context, r *adminRun) error {
	if err := r.client.CloseSignInLink(ctx, r.id); err != nil {
		return err
	}
	return r.say("closed sign-in link %d\n", r.id)
}
