package main

import "fmt"

// The passwords the run sends, besides the ones its people choose
// (joinPassword, convertPassword, resetPassword, operatorPassword): the
// probers' own, their guesses, a common one and a second choice. Each is
// passed to the log scan (newWorld; a password made from a name is passed
// where it is made).
const (
	proberPassword      = "a long password of the prober's own"
	proberLoginPassword = "a password of the prober's own"
	guessedPassword     = "a guess at somebody's password"
	commonPassword      = "correct horse battery staple"
	anotherPassword     = "another passphrase for pia"
)

// passwords is every fixed password the run sends.
var passwords = []string{joinPassword, convertPassword, resetPassword, operatorPassword,
	proberPassword, proberLoginPassword, guessedPassword, commonPassword, anotherPassword}

// guestPhase is the plaza and its guests (guests.go), in an order that lets
// the two expiring visits' clocks run while the rest goes on: everything
// that does not use the plaza's voice room runs before their deadlines, and
// the timed ends of the cohort's visits after, since each end rotates the
// room and takes everyone in it out. The operator's own sign-in and recovery
// revoke the credential the run has acted as the operator with, and make it
// a new one. It returns what it timed.
func (w *world) guestPhase(bounds voiceBounds) ([]timing, error) {
	h := w.h
	if err := w.buildPlaza(); err != nil {
		return nil, fmt.Errorf("the plaza: %w", err)
	}
	text, secrets, ids := w.priv.count()
	h.say("ok   the plaza: two members made accounts at the bare address, then a thread, a whisper, a net's message, a file, an agent's run, its activity and an intervention; private markers: %d pieces of text, %d secrets, %d ids", text, secrets, ids)
	exp, err := w.startExpiry()
	if err != nil {
		return nil, fmt.Errorf("the expiring visits: %w", err)
	}
	h.say("ok   expiry: two guests joined under a %d s idle and %d s hard bound, each holding every socket variant%s; their clocks run on while the rest goes on",
		expiryIdleSeconds, expiryAbsoluteSeconds, map[bool]string{true: " and a LiveKit participant", false: ""}[w.lk != nil])
	piaFeed, err := w.buildCohort()
	if err != nil {
		return nil, fmt.Errorf("the cohort: %w", err)
	}
	p := w.p
	h.say("ok   the cohort: two guests labelled Nick at the bare address, one through the guest-only invite and one who will make an account, each from an address of its own; then the members went on, some of it private")
	if err := w.checkGuestSockets(); err != nil {
		return nil, fmt.Errorf("the guests' sockets: %w", err)
	}
	h.say("ok   guests' sockets: Nick's %d sockets carried what was said for the guests since his visit began, and nothing from before it, nothing private and nothing of A's; so did a v2 socket replaying from the start, Nick's and Ola's", len(p.nickHeld.sockets))
	n := 0
	for _, g := range []*member{p.nick, p.ola} {
		k, err := w.runMatrix(g, guestRows())
		n += k
		if err != nil {
			return nil, err
		}
	}
	k, err := w.runEntryRows()
	n += k
	if err != nil {
		return nil, err
	}
	if err := w.checkMCPClosed(p.nick); err != nil {
		return nil, err
	}
	h.say("ok   guests' matrix: %d rows as each of two guests (from the bare address, through an invite) and %d entry rows as newcomers, each from an address of its own, %d requests: the plaza from the visit's bound and nothing before it, nothing of A's, every right a guest may not have refused; /mcp is closed to a guest",
		len(guestRows()), len(entryRows()), n)
	nickPost, err := w.checkIdentity(piaFeed)
	if err != nil {
		return nil, fmt.Errorf("the guests' identity: %w", err)
	}
	h.say("ok   identity: the two Nicks are two handles with two tags; neither label nor handle signs in; Nick's post came to pia's socket and history as his visit's; the operator cannot rename, hide, credential, link or let invite a guest")
	tries, err := w.runGates()
	if err != nil {
		return nil, fmt.Errorf("the entry gates: %w", err)
	}
	h.say("ok   gates: %d steps of the joining policy, %d tries, each from a newcomer's own address: guests off (accounts open, no visit evicted), guest-only (no account without a permit), closed, each invite's mode, paused, invites paused, the cap, the cap per address, a closed invite, voice off", len(gates()), tries)
	member, samPost, err := w.convertSam()
	if err != nil {
		return nil, fmt.Errorf("the conversion: %w", err)
	}
	k, err = w.runMatrix(member, historyRows())
	if err != nil {
		return nil, fmt.Errorf("the member Sam became: %w", err)
	}
	race, err := w.logoutRace()
	if err != nil {
		return nil, fmt.Errorf("a conversion racing End visit: %w", err)
	}
	h.say("ok   conversion: every refusal left Sam the guest he was; two at once made one member, the same principal, who signs in with the password, keeps the visit's bound (%d history rows, %d requests) and gains nothing; his post as a guest is still the guest's; a conversion racing End visit: %s", len(historyRows()), k, race)
	var out []timing
	t, err := w.memberResetRace()
	if err != nil {
		return nil, fmt.Errorf("a member's reset: %w", err)
	}
	out = append(out, t)
	h.say("ok   reset: %s; the session, the racing sign-in and device link, the link and the reset link are all dead after it; the old password is refused, the new one signs in; no link serves another purpose", t)
	// The operator's half waits on the server's socket re-check and on the
	// authenticator's time steps: it runs while the expiring visits' clocks
	// do. It leaves the run a new script credential for the operator.
	t, err = w.operatorAuth()
	if err != nil {
		return nil, fmt.Errorf("the operator's sign-in: %w", err)
	}
	out = append(out, t)
	h.say("ok   operator: set up through a host-made token; a password alone is a challenge, bound to its address; the session needs a proof for a sensitive route, and a code is good once; the script credential makes no proof and no link; %s; back in through the new setup token", t)
	ts, err := w.finishExpiry(exp, bounds)
	if err != nil {
		return nil, fmt.Errorf("expiry: %w", err)
	}
	for _, t := range ts {
		h.say("ok   %s", t)
	}
	out = append(out, ts...)
	ts, err = w.revokeGuests(bounds, nickPost)
	if err != nil {
		return nil, fmt.Errorf("ending visits: %w", err)
	}
	for _, t := range ts {
		h.say("ok   %s; they stay out", t)
	}
	out = append(out, ts...)
	h.say("ok   after End visit Nick's post is still his visit's, which pia reads and bea, of another group, is told does not exist; a new visit from his address under his label is someone new who reads nothing from before it")
	if err := w.hideKeepsAttribution(member, samPost, *p.sam.guest); err != nil {
		return nil, fmt.Errorf("hiding the member a guest became: %w", err)
	}
	h.say("ok   hiding the member Sam became leaves his post as a guest the guest's")
	return out, nil
}
