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

// `conch admin join …` and `conch admin guest …` (ADR-009, issues #710 and
// #711): the joining policy, the guest list, ending visits and conversion
// permits. Each is one request to an operator route; the server decides who
// may, and asks an operator signed in with a password for a recent proof,
// which these commands then ask for (askToReauth).
//
// A conversion permit's code is a secret like a sign-in link's: `guest
// permit` prints it once, alone on standard output. No other output, error
// or log line carries it.

// joinOpts holds the flags of `admin join set` and the guest commands.
type joinOpts struct {
	openJoining    string
	guests         string
	paused         string
	voice          string
	maxGuests      int64
	maxPerAddress  int64
	idle           string
	lifetime       string
	openChannels   stringList
	noOpenChannels bool
	// state, origin and inviteID select guests; yes ends them, and dryRun
	// says out loud that it must not.
	state    string
	origin   string
	inviteID int64
	yes      bool
	dryRun   bool
	expires  string
}

func bindJoinSet(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.join.openJoining, "open-joining", "", "what the bare address offers: off or account (guests are turned off for now, so the server refuses guest and both)")
	fs.StringVar(&o.join.guests, "guests", "", "on or off: whether any entry point, invites included, may start a guest visit (guests are turned off for now, so the server refuses on)")
	fs.StringVar(&o.join.paused, "paused", "", "on or off: pause joining from the bare address")
	fs.StringVar(&o.join.voice, "voice", "", "on or off: whether people who join from the bare address may use voice")
	fs.Int64Var(&o.join.maxGuests, "max-guests", -1, "the most live guest visits at once")
	fs.Int64Var(&o.join.maxPerAddress, "max-guests-per-address", -1, "the most live guest visits from one client address")
	fs.StringVar(&o.join.idle, "guest-idle", "", "how long a visit may go unused, such as 2h (a minute to its lifetime)")
	fs.StringVar(&o.join.lifetime, "guest-lifetime", "", "the most a visit lasts, such as 24h (at most 24h)")
	fs.Var(&o.join.openChannels, "open-channel", "a channel the bare address admits to (repeat for more; replaces the list)")
	fs.BoolVar(&o.join.noOpenChannels, "no-open-channels", false, "clear the list of open channels")
}

func bindGuestList(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.join.state, "state", "", "only visits in this state: active (the default), ended or converted")
	fs.StringVar(&o.join.origin, "origin", "", "only visits from this entry point: address or invite")
}

func bindGuestEndAll(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.join.origin, "origin", "", "only visits from this entry point: address or invite")
	fs.Int64Var(&o.join.inviteID, "invite", 0, "only visits that came through this invite")
	fs.BoolVar(&o.join.yes, "yes", false, "do it; without it, only say whose visits it would end")
	fs.BoolVar(&o.join.dryRun, "dry-run", false, "only say whose visits it would end (the default without --yes)")
}

func bindGuestPermit(fs *flag.FlagSet, o *adminOpts) {
	fs.StringVar(&o.join.expires, "expires", "", "when it stops working: a duration such as 10m, or an RFC 3339 time (default 10m, at most 1h)")
}

// onOff reads an on|off flag; "" is not given.
func onOff(flagName, v string) (*bool, error) {
	switch v {
	case "":
		return nil, nil
	case "on":
		b := true
		return &b, nil
	case "off":
		b := false
		return &b, nil
	}
	return nil, fmt.Errorf("--%s must be on or off", flagName)
}

// seconds reads a duration flag as whole seconds; "" is not given.
func seconds(flagName, v string) (*int64, error) {
	if v == "" {
		return nil, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 || d%time.Second != 0 {
		return nil, fmt.Errorf("--%s must be a whole number of seconds or more, such as 90m or 2h", flagName)
	}
	n := int64(d / time.Second)
	return &n, nil
}

// joinSetRequest builds the settings change from the flags and checks its
// shape as the server will; the result's consistency is the server's to
// decide, against the settings it holds.
func joinSetRequest(o joinOpts) (schema.PutJoinSettingsRequestV1, error) {
	var req schema.PutJoinSettingsRequestV1
	var err error
	if o.openJoining != "" {
		mode := schema.OpenJoiningMode(o.openJoining)
		req.OpenJoining = &mode
	}
	if req.GuestsEnabled, err = onOff("guests", o.guests); err != nil {
		return req, err
	}
	if req.JoiningPaused, err = onOff("paused", o.paused); err != nil {
		return req, err
	}
	if req.JoinVoice, err = onOff("voice", o.voice); err != nil {
		return req, err
	}
	if o.maxGuests != -1 {
		n := o.maxGuests
		req.MaxGuests = &n
	}
	if o.maxPerAddress != -1 {
		n := o.maxPerAddress
		req.MaxGuestsPerAddress = &n
	}
	if req.GuestIdleSeconds, err = seconds("guest-idle", o.idle); err != nil {
		return req, err
	}
	if req.GuestAbsoluteSeconds, err = seconds("guest-lifetime", o.lifetime); err != nil {
		return req, err
	}
	switch {
	case o.noOpenChannels && len(o.openChannels) > 0:
		return req, errors.New("give --open-channel or --no-open-channels, not both")
	case o.noOpenChannels:
		req.OpenChannels = &[]string{}
	case len(o.openChannels) > 0:
		list := append([]string(nil), o.openChannels...)
		req.OpenChannels = &list
	}
	return req, req.Validate()
}

// warningText says what a joining warning means for the operator.
func warningText(w schema.JoinWarning, live int64) string {
	switch w {
	case schema.JoinWarningLiveGuestsRemain:
		return fmt.Sprintf("%d guest visit(s) are still live and last to their bounds; end them with 'conch admin guest end-all'", live)
	case schema.JoinWarningTrustedProxyUnset:
		return "no trusted proxy is configured, so every caller may share one address and the per-address guest cap binds them together"
	case schema.JoinWarningOneAddress:
		return "every live guest arrived from one address"
	case schema.JoinWarningReservedNamesInUse:
		return "a lasting account has a name in the reserved guest namespace; rename it before guests can be turned on"
	}
	return safeLine(string(w))
}

// sayJoinSettings prints the joining policy, one "key: value" line each,
// then each warning.
func (r *adminRun) sayJoinSettings(resp schema.JoinSettingsResponseV1) error {
	if r.opts.asJSON {
		if resp.Settings.OpenChannels == nil {
			resp.Settings.OpenChannels = []string{}
		}
		if resp.Warnings == nil {
			resp.Warnings = []schema.JoinWarning{}
		}
		return r.printJSON(resp)
	}
	s := resp.Settings
	channels := make([]string, len(s.OpenChannels))
	for i, c := range s.OpenChannels {
		channels[i] = safeLine(c)
	}
	if err := r.say("server name: %s\nopen joining: %s\nguests: %s\njoining paused: %s\nvoice for joiners: %s\nmax guests: %d (%d per address)\na guest visit lasts: at most %s, and ends after %s unused\nopen channels: %s\nlive guests: %d\n",
		safeLine(resp.ServerName), safeLine(string(s.OpenJoining)), either(s.GuestsEnabled, "on", "off"), yesNo(s.JoiningPaused), yesNo(s.JoinVoice),
		s.MaxGuests, s.MaxGuestsPerAddress, span(s.GuestAbsoluteSeconds), span(s.GuestIdleSeconds), dashIfEmpty(strings.Join(channels, ", ")), resp.LiveGuests); err != nil {
		return err
	}
	for _, w := range resp.Warnings {
		if err := r.say("warning: %s\n", warningText(w, resp.LiveGuests)); err != nil {
			return err
		}
	}
	return nil
}

func adminJoinStatus(ctx context.Context, r *adminRun) error {
	resp, err := r.client.JoinSettings(ctx)
	if err != nil {
		return olderServer(err, "joining settings")
	}
	return r.sayJoinSettings(resp)
}

// adminJoinSet changes the joining policy: only the fields its flags give.
// The answer is the policy as it now is, with the warnings that apply:
// turning guests off names the live visits that remain, since they are not
// ended by it.
func adminJoinSet(ctx context.Context, r *adminRun) error {
	req, err := joinSetRequest(r.opts.join)
	if err != nil {
		return fmt.Errorf("cli: admin join set: %w", err)
	}
	resp, err := r.client.PutJoinSettings(ctx, req)
	if err != nil {
		return olderServer(err, "joining settings")
	}
	return r.sayJoinSettings(resp)
}

// guestLine is one visit as `guest list` prints it:
// `<id> <handle> <state> <origin> invite <id|-> started <t> ends <t> unused <t> <label|->`.
// The label is last because it may contain spaces.
func guestLine(g schema.GuestAdminV1) string {
	invite := "-"
	if g.InviteID != nil {
		invite = strconv.FormatInt(*g.InviteID, 10)
	}
	return fmt.Sprintf("%d %s %s %s invite %s started %s ends %s unused %s %s", g.ID, safeLine(g.Handle), safeLine(string(g.State)), safeLine(string(g.Origin)),
		invite, formatTimestamp(g.StartedAt), formatTimestamp(g.ExpiresAt), formatTimestamp(g.IdleExpiresAt), dashIfEmpty(safeLine(g.Label)))
}

// adminGuestList prints the guest visits, newest first, following the
// server's pages to the end.
func adminGuestList(ctx context.Context, r *adminRun) error {
	o := r.opts.join
	if o.state != "" && !schema.GuestState(o.state).Valid() {
		return errors.New("cli: admin guest list: --state must be active, ended or converted")
	}
	if o.origin != "" && !schema.GuestOrigin(o.origin).Valid() {
		return errors.New("cli: admin guest list: --origin must be address or invite")
	}
	filter := GuestListFilter{State: o.state, Origin: o.origin, Limit: schema.MaxListGuestsLimit}
	all := []schema.GuestAdminV1{}
	for {
		page, err := r.client.ListGuests(ctx, filter)
		if err != nil {
			return olderServer(err, "guest visits")
		}
		all = append(all, page.Guests...)
		if page.NextBefore == 0 {
			break
		}
		// Pages go down by id; one that does not would never end.
		if filter.Before != 0 && page.NextBefore >= filter.Before {
			return errors.New("cli: admin guest list: the server's pages do not move forward")
		}
		filter.Before = page.NextBefore
	}
	if r.opts.asJSON {
		return r.printJSON(schema.ListGuestsResponseV1{Guests: all})
	}
	for _, g := range all {
		if err := r.say("%s\n", guestLine(g)); err != nil {
			return err
		}
	}
	return nil
}

// adminGuestEnd ends one visit at once: its credential, memberships and
// sockets go, and its messages stay.
func adminGuestEnd(ctx context.Context, r *adminRun) error {
	resp, err := r.client.EndGuest(ctx, r.id)
	if err != nil {
		return olderServer(err, "guest visits")
	}
	if r.opts.asJSON {
		return r.printJSON(resp)
	}
	return r.say("ended guest visit %d (%s)\n", resp.Guest.ID, safeLine(resp.Guest.Handle))
}

// adminGuestEndAll is End all guest visits, or those of one origin or
// invite. Without --yes it is a dry run: the server lists whose visits it
// would end, and nothing changes.
func adminGuestEndAll(ctx context.Context, r *adminRun) error {
	o := r.opts.join
	if o.yes && o.dryRun {
		return errors.New("cli: admin guest end-all: give --yes or --dry-run, not both")
	}
	dryRun := !o.yes
	req := schema.EndGuestsRequestV1{DryRun: &dryRun, Origin: schema.GuestOrigin(o.origin), InviteID: o.inviteID}
	if err := req.Validate(); err != nil {
		return fmt.Errorf("cli: admin guest end-all: %w", err)
	}
	resp, err := r.client.EndGuests(ctx, req)
	if err != nil {
		return olderServer(err, "guest visits")
	}
	if r.opts.asJSON {
		if resp.Guests == nil {
			resp.Guests = []schema.GuestRefV1{}
		}
		return r.printJSON(resp)
	}
	for _, g := range resp.Guests {
		if err := r.say("%d %s\n", g.ID, safeLine(g.Handle)); err != nil {
			return err
		}
	}
	more := ""
	if resp.Remaining > 0 {
		more = fmt.Sprintf("; %d more match past the limit of %d a call", resp.Remaining, schema.MaxEndGuests)
	}
	if resp.DryRun {
		return r.say("would end %d guest visit(s)%s; run again with --yes to do it\n", resp.Count, more)
	}
	if resp.Remaining > 0 {
		more += ", so run it again"
	}
	return r.say("ended %d guest visit(s)%s\n", resp.Count, more)
}

// adminGuestPermit lets one live guest create an account although its entry
// point's policy is closed, and prints the permit's code alone on standard
// output. The guest gives it to `conch account create <name> --permit`.
func adminGuestPermit(ctx context.Context, r *adminRun) error {
	var req schema.CreateConversionPermitRequestV1
	now := time.Now()
	if r.opts.join.expires != "" {
		exp, err := parseInviteExpiry(r.opts.join.expires, now)
		if err != nil {
			return fmt.Errorf("cli: admin guest permit: %w", err)
		}
		ts := schema.NewTimestamp(exp)
		req.ExpiresAt = &ts
	}
	if err := req.Validate(now); err != nil {
		return fmt.Errorf("cli: admin guest permit: %w", err)
	}
	resp, err := r.client.CreateConversionPermit(ctx, r.id, req)
	if err != nil {
		return olderServer(err, "conversion permits")
	}
	// The code is the one server string printed raw, so it is checked first.
	if schema.ValidateSignInLinkCode(resp.Code) != nil {
		return errors.New("cli: admin guest permit: the server returned a code in an unexpected form; it was not printed")
	}
	if r.opts.asJSON {
		return r.printJSON(resp)
	}
	if _, err := fmt.Fprintln(r.stdout, resp.Code); err != nil {
		return err
	}
	_, err = fmt.Fprintf(r.stderr, "conversion permit %d for guest %d, one use, expires %s; the guest runs 'conch account create <name> --permit' and gives it\n",
		resp.SignInLink.ID, resp.SignInLink.PrincipalID, formatTimestamp(resp.SignInLink.ExpiresAt))
	return err
}
