package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The guest entry policy (issue #710, ADR-009 D1,
// docs/design/guests-and-accounts.md §2). The joining settings are
// server_settings rows (migration 31; an absent row is
// schema.DefaultJoinSettings), the bare address's channels are open_channels,
// and an invite's mode is invites.join_as. Effective guest joining needs
// both the server's guests_enabled switch and the entry point's guest mode.
//
// The policy is checked again inside the IMMEDIATE transaction that starts a
// visit (JoinAsGuest, RedeemInviteGuest), so a preview read a moment before,
// or a settings change racing the join, cannot admit anyone the policy at
// commit refuses. The same predicates answer the previews (AddressJoinOptions,
// PreviewInvite's join), so what a preview offers and what a write allows
// agree.
//
// An account from the bare address (JoinAsAccount, issue #711) is held to
// the same settings, in the same way: its mode is checked in the creating
// transaction, and the account exists only with its password.
//
// Guests are removed for now (issue #1005): no entry point may admit them.
// The settings can no longer turn guests on or make the bare address admit
// them (PutJoinSettings), no invite may admit them (CreateInvite), and a
// store whose rows still do is normalized when it is opened
// (NormalizeGuestSettings). The guest tables, the visits already live and
// the code that starts a visit stay, unused, for when guests come back.

// The server_settings keys of the joining settings.
const (
	settingOpenJoining          = "open_joining"
	settingGuestsEnabled        = "guests_enabled"
	settingJoiningPaused        = "joining_paused"
	settingJoinVoice            = "join_voice"
	settingMaxGuests            = "max_guests"
	settingMaxGuestsPerAddress  = "max_guests_per_address"
	settingGuestIdleSeconds     = "guest_idle_seconds"
	settingGuestAbsoluteSeconds = "guest_absolute_seconds"
)

// joinSettingKeys are the rows joinSettingsTx reads. Since migration 40
// (issue #979, design §3.4 Option B) open_joining and join_voice are not
// stored: open_joining reads from open_signup, join_voice reads true, and the
// open channels are the default channels.
var joinSettingKeys = []string{settingOpenSignup, settingMemberLinkDefaultExpirySeconds, settingGuestsEnabled, settingJoiningPaused,
	settingMaxGuests, settingMaxGuestsPerAddress, settingGuestIdleSeconds, settingGuestAbsoluteSeconds}

// Audit actions of the entry policy (design §7.3). None carries a client
// address, a token or a code.
const (
	// AuditJoinSettingsChanged records a settings change: actor the
	// operator, subject "server", detail each field that changed as
	// field=old->new, and live_guests=<n> when guests were turned off
	// (review F12).
	AuditJoinSettingsChanged = "join_settings_changed"
	// AuditOpenChannelAdded and AuditOpenChannelRemoved record a channel
	// joining or leaving the bare address's set: subject the channel.
	AuditOpenChannelAdded   = "open_channel_added"
	AuditOpenChannelRemoved = "open_channel_removed"
	// AuditGuestsEnded summarises End all guest visits: subject "server",
	// detail count=<n> remaining=<n> and the selection. Each visit it ended
	// has its own guest_ended row as well.
	AuditGuestsEnded = "guests_ended"
)

// Errors of the entry policy.
var (
	// ErrGuestsDisabled: no visit may start at this entry point now: the
	// server's guests_enabled switch is off, the entry point's mode admits no
	// guests, or the bare address has no channel to admit to.
	ErrGuestsDisabled = errors.New("store: guests cannot join here now")
	// ErrJoiningPaused: an operator paused joining from the bare address.
	ErrJoiningPaused = errors.New("store: joining is paused")
	// ErrJoinPolicyConflict and ErrJoinSettingsInvalid are the two kinds of
	// JoinSettingsError.
	ErrJoinPolicyConflict  = errors.New("store: the join settings contradict each other or the server")
	ErrJoinSettingsInvalid = errors.New("store: the join settings are not valid")
	// ErrGuestsRemoved: the request would make an entry point that admits
	// guests, and guests are removed for now (issue #1005).
	ErrGuestsRemoved = errors.New("store: guests are turned off on this server for now")
)

// guestsRemovedReason is what a refused settings change says.
const guestsRemovedReason = "guests are turned off on this server for now: guests_enabled cannot be turned on, and open_joining may be off or account"

// withoutGuests is s with every guest-admitting setting turned off, as
// guests are removed for now (issue #1005): guests disabled, a bare address
// open to guests and accounts open to accounts only, and a guest-only one
// off. Nothing else changes.
func withoutGuests(s schema.JoinSettingsV1) schema.JoinSettingsV1 {
	s.GuestsEnabled = false
	switch s.OpenJoining {
	case schema.OpenJoiningBoth:
		s.OpenJoining = schema.OpenJoiningAccount
	case schema.OpenJoiningGuest:
		s.OpenJoining = schema.OpenJoiningOff
	}
	return s
}

// settingsAdmitGuests reports whether s holds a setting withoutGuests
// would turn off.
func settingsAdmitGuests(s schema.JoinSettingsV1) bool {
	n := withoutGuests(s)
	return n.GuestsEnabled != s.GuestsEnabled || n.OpenJoining != s.OpenJoining
}

// requestAdmitsGuests reports whether a settings change asks for guests:
// guests_enabled true, or an open_joining mode that offers them.
func requestAdmitsGuests(req schema.PutJoinSettingsRequestV1) bool {
	if req.GuestsEnabled != nil && *req.GuestsEnabled {
		return true
	}
	return req.OpenJoining != nil && (*req.OpenJoining == schema.OpenJoiningGuest || *req.OpenJoining == schema.OpenJoiningBoth)
}

// JoinSettingsError is a refused settings change. Err is
// ErrJoinPolicyConflict (contradictory settings, the last open channel
// removed while joining is on, guests enabled while reserved names exist) or
// ErrJoinSettingsInvalid (a value out of bounds, a channel that does not
// exist). Reason names the rule broken, never a value sent.
type JoinSettingsError struct {
	Err    error
	Reason string
}

func (e *JoinSettingsError) Error() string { return e.Err.Error() + ": " + e.Reason }
func (e *JoinSettingsError) Unwrap() error { return e.Err }

func conflict(reason string) error {
	return &JoinSettingsError{Err: ErrJoinPolicyConflict, Reason: reason}
}

func invalidSettings(reason string) error {
	return &JoinSettingsError{Err: ErrJoinSettingsInvalid, Reason: reason}
}

// AddressAdmitsGuests reports whether the bare address admits guests under
// s, before the pause, the channels and the caps: guests are enabled and the
// address's mode offers them.
func AddressAdmitsGuests(s schema.JoinSettingsV1) bool {
	return s.GuestsEnabled && (s.OpenJoining == schema.OpenJoiningGuest || s.OpenJoining == schema.OpenJoiningBoth)
}

// InviteAdmitsGuests reports whether an invite of mode m admits guests; the
// server's guests_enabled switch still has to be on.
func InviteAdmitsGuests(m schema.InviteJoinMode) bool {
	return m == schema.InviteJoinGuest || m == schema.InviteJoinBoth
}

// admitsAccount reports whether an invite of mode m admits an account. A
// mode the store did not write is account, as on the wire.
func admitsAccount(m schema.InviteJoinMode) bool {
	return m == "" || m == schema.InviteJoinAccount || m == schema.InviteJoinBoth
}

// GuestCapReached reports whether one more visit would pass a cap: live
// visits server-wide against max_guests (0 is no cap), and addressLive, the
// live visits from the caller's address, against max_guests_per_address.
func GuestCapReached(s schema.JoinSettingsV1, live, addressLive int) bool {
	if s.MaxGuests > 0 && int64(live) >= s.MaxGuests {
		return true
	}
	return s.MaxGuestsPerAddress > 0 && int64(addressLive) >= s.MaxGuestsPerAddress
}

// AddressAdmitsAccounts reports whether the bare address's mode offers
// accounts under s, before the pause and the channels (issue #711). The
// guests_enabled switch plays no part: it is about guests only.
func AddressAdmitsAccounts(s schema.JoinSettingsV1) bool {
	return s.OpenJoining == schema.OpenJoiningAccount || s.OpenJoining == schema.OpenJoiningBoth
}

// AddressJoinOptions is what the bare address offers now: Guest when a visit
// may start, GuestsFull when only a cap stops one, Account when an account
// may be made (addressAccountPolicy), and Paused, with neither, when joining
// is paused. They are the predicates JoinAsGuest and JoinAsAccount apply.
func AddressJoinOptions(s schema.JoinSettingsV1, openChannels, live, addressLive int) schema.JoinOptionsV1 {
	var out schema.JoinOptionsV1
	if s.OpenJoining == schema.OpenJoiningOff {
		return out
	}
	if s.JoiningPaused {
		out.Paused = true
		return out
	}
	if openChannels == 0 {
		return out
	}
	out.Account = AddressAdmitsAccounts(s)
	if !AddressAdmitsGuests(s) {
		return out
	}
	if GuestCapReached(s, live, addressLive) {
		out.GuestsFull = true
	} else {
		out.Guest = true
	}
	return out
}

// addressAccountPolicy is the rule an account from the bare address is held
// to (design §2, §3.1): the address's mode offers accounts and it has a
// channel to admit to (else ErrJoiningClosed), and joining is not paused
// (ErrJoiningPaused).
func addressAccountPolicy(s schema.JoinSettingsV1) error {
	if !AddressAdmitsAccounts(s) {
		return ErrJoiningClosed
	}
	if s.JoiningPaused {
		return ErrJoiningPaused
	}
	if len(s.OpenChannels) == 0 {
		return ErrJoiningClosed
	}
	return nil
}

// AddressCapReached reports whether the per-address cap stops one more
// visit from an address whose visits are peers: what an invite preview adds
// to the server-wide cap PreviewInvite applies.
func (s *Store) AddressCapReached(ctx context.Context, peers []int64) (bool, error) {
	set, err := joinSettingsTx(ctx, s.db)
	if err != nil {
		return false, err
	}
	n, err := liveGuestsAmongTx(ctx, s.db, peers, credentialNow())
	if err != nil {
		return false, err
	}
	return GuestCapReached(schema.JoinSettingsV1{MaxGuestsPerAddress: set.MaxGuestsPerAddress}, 0, n), nil
}

// inviteJoinOptions is what an invite of mode m offers under s, with live
// visits: Account as its mode says (the existing redemption route makes the
// account), Guest while guests are on, its mode admits them and the
// server-wide cap is not reached.
func inviteJoinOptions(m schema.InviteJoinMode, s schema.JoinSettingsV1, live int) schema.JoinOptionsV1 {
	out := schema.JoinOptionsV1{Account: admitsAccount(m)}
	if s.GuestsEnabled && InviteAdmitsGuests(m) {
		if GuestCapReached(s, live, 0) {
			out.GuestsFull = true
		} else {
			out.Guest = true
		}
	}
	return out
}

// joinSettingsTx reads the joining settings. An absent row is the default; a
// row that cannot be read fails closed: a switch that admits reads as off, a
// pause as on, a mode as off, a number as its default.
func joinSettingsTx(ctx context.Context, tx execer) (schema.JoinSettingsV1, error) {
	out := schema.DefaultJoinSettings()
	signup := false
	expiry := schema.DefaultMemberLinkExpirySeconds
	args := make([]any, len(joinSettingKeys))
	marks := make([]string, len(joinSettingKeys))
	for i, k := range joinSettingKeys {
		args[i], marks[i] = k, "?"
	}
	rows, err := tx.QueryContext(ctx, // #nosec G202 -- placeholders only; every key is in args
		"SELECT key, value FROM server_settings WHERE key IN ("+strings.Join(marks, ",")+")", args...)
	if err != nil {
		return schema.JoinSettingsV1{}, fmt.Errorf("store: read join settings: %w", err)
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			_ = rows.Close()
			return schema.JoinSettingsV1{}, fmt.Errorf("store: read join settings: %w", err)
		}
		switch k {
		case settingOpenSignup:
			b, err := strconv.ParseBool(v)
			signup = err == nil && b
		case settingMemberLinkDefaultExpirySeconds:
			readInt(v, &expiry)
		case settingGuestsEnabled:
			b, err := strconv.ParseBool(v)
			out.GuestsEnabled = err == nil && b
		case settingJoiningPaused:
			b, err := strconv.ParseBool(v)
			out.JoiningPaused = err != nil || b
		case settingMaxGuests:
			readInt(v, &out.MaxGuests)
		case settingMaxGuestsPerAddress:
			readInt(v, &out.MaxGuestsPerAddress)
		case settingGuestIdleSeconds:
			readInt(v, &out.GuestIdleSeconds)
		case settingGuestAbsoluteSeconds:
			readInt(v, &out.GuestAbsoluteSeconds)
		}
	}
	err = rows.Err()
	if cerr := rows.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return schema.JoinSettingsV1{}, fmt.Errorf("store: read join settings: %w", err)
	}
	// Option B's mapping (design §3.4): the mode follows open_signup, voice
	// is always on, and the community fields are always answered.
	out.OpenSignup = &signup
	out.MemberLinkDefaultExpirySeconds = &expiry
	out.OpenJoining = schema.OpenJoiningOff
	if signup {
		out.OpenJoining = schema.OpenJoiningAccount
	}
	out.JoinVoice = true
	channels, err := openChannelsTx(ctx, tx)
	if err != nil {
		return schema.JoinSettingsV1{}, err
	}
	out.OpenChannels = make([]string, len(channels))
	for i, c := range channels {
		out.OpenChannels[i] = c.Name
	}
	return out, nil
}

func readInt(v string, dst *int64) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		*dst = n
	}
}

// openChannelsTx returns the bare address's channels, by id: the default
// channels (migration 40, design §3.4 Option B).
func openChannelsTx(ctx context.Context, tx execer) ([]schema.InviteChannelV1, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name FROM channels WHERE is_default = 1 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: read open channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schema.InviteChannelV1{}
	for rows.Next() {
		var c schema.InviteChannelV1
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, fmt.Errorf("store: read open channels: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read open channels: %w", err)
	}
	return out, nil
}

// AuditJoinChannelDropped records an address join that left out a listed
// open channel because whoever listed it (channels.default_listed_by, before
// migration 40 open_channels.added_by) no longer holds manage_channels
// (ADR-014 design §7.3, §8.5). Actor system, subject the channel, detail
// "reason=permission".
const AuditJoinChannelDropped = "join_channel_dropped"

// admittingOpenChannelsTx splits the bare address's channels into those an
// address join lands in now and those it drops (ADR-014 design §7.3, SF-2):
// a channel is kept only while whoever listed it is an owner or holds
// manage_channels, read in tx. A channel with no lister was listed before
// the column was filled, when only an owner could write the list, and
// counts as an owner's.
func admittingOpenChannelsTx(ctx context.Context, tx execer) (kept, dropped []schema.InviteChannelV1, err error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name, default_listed_by FROM channels WHERE is_default = 1 ORDER BY id`)
	if err != nil {
		return nil, nil, fmt.Errorf("store: read open channels: %w", err)
	}
	type listed struct {
		ch      schema.InviteChannelV1
		addedBy sql.NullInt64
	}
	var all []listed
	for rows.Next() {
		var l listed
		if err := rows.Scan(&l.ch.ID, &l.ch.Name, &l.addedBy); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("store: read open channels: %w", err)
		}
		all = append(all, l)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("store: read open channels: %w", err)
	}
	_ = rows.Close()
	kept = []schema.InviteChannelV1{}
	for _, l := range all {
		ok := !l.addedBy.Valid
		if !ok {
			if ok, err = permittedTx(ctx, tx, l.addedBy.Int64, schema.PermManageChannels); err != nil {
				return nil, nil, err
			}
		}
		if ok {
			kept = append(kept, l.ch)
		} else {
			dropped = append(dropped, l.ch)
		}
	}
	return kept, dropped, nil
}

// JoinState is the joining policy as it stands, with what an answer about
// it needs beside the settings.
type JoinState struct {
	Settings schema.JoinSettingsV1
	// LiveGuests is how many visits are live now: what max_guests counts.
	LiveGuests int
	// ReservedNames is how many permanent principals hold a name in the
	// reserved guest namespace (migration 31's preflight); while any do,
	// guests cannot be enabled.
	ReservedNames int
	// ServerNameHolders is how many permanent principals hold the server's
	// name (ServerNameHolders, issue #760). They block nothing.
	ServerNameHolders int
}

// JoinState reads the joining policy, the live visits and the reserved
// names, in one read transaction so they agree.
func (s *Store) JoinState(ctx context.Context) (JoinState, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return JoinState{}, fmt.Errorf("store: join state: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return s.joinStateTx(ctx, tx, credentialNow())
}

// JoinSettings reads the joining policy and the live visits, without the
// reserved-name scan JoinState makes: what the front door needs.
func (s *Store) JoinSettings(ctx context.Context) (schema.JoinSettingsV1, int, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return schema.JoinSettingsV1{}, 0, fmt.Errorf("store: join settings: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	set, err := joinSettingsTx(ctx, tx)
	if err != nil {
		return schema.JoinSettingsV1{}, 0, err
	}
	live, err := liveGuestsTx(ctx, tx, credentialNow())
	if err != nil {
		return schema.JoinSettingsV1{}, 0, err
	}
	return set, live, nil
}

func (s *Store) joinStateTx(ctx context.Context, tx execer, now time.Time) (JoinState, error) {
	set, err := joinSettingsTx(ctx, tx)
	if err != nil {
		return JoinState{}, err
	}
	live, err := liveGuestsTx(ctx, tx, now)
	if err != nil {
		return JoinState{}, err
	}
	reserved, err := reservedGuestNamesTx(ctx, tx)
	if err != nil {
		return JoinState{}, fmt.Errorf("store: join state: %w", err)
	}
	holders, err := s.serverNameHoldersTx(ctx, tx)
	if err != nil {
		return JoinState{}, err
	}
	return JoinState{Settings: set, LiveGuests: live, ReservedNames: len(reserved), ServerNameHolders: len(holders)}, nil
}

// PutJoinSettings applies req to the joining settings in one IMMEDIATE
// transaction (schema.PutJoinSettingsRequestV1.Apply, so turning guests off
// turns a guest-only address off too) and returns the state after it. It
// refuses, writing nothing, with a JoinSettingsError: ErrJoinPolicyConflict
// for guest-only joining with guests off, open joining with no open channel
// (removing the last one while joining is on included), or enabling guests
// while a permanent name is in the reserved namespace; ErrJoinSettingsInvalid
// for a value out of bounds or a channel that does not exist. Each field
// that changed is audited in one join_settings_changed row, with the live
// visits when guests are turned off, and each channel added or removed in
// its own row. A request that changes nothing writes nothing. Turning guests
// off ends no visit: End all guest visits does (EndGuests).
//
// Guests are removed for now (issue #1005): a request for guests_enabled
// true or an open_joining of guest or both is refused with
// ErrJoinPolicyConflict and writes nothing, and every change it makes leaves
// the settings without guests (withoutGuests), whatever the rows held.
func (s *Store) PutJoinSettings(ctx context.Context, actor string, actorID int64, req schema.PutJoinSettingsRequestV1) (JoinState, error) {
	if err := req.Validate(); err != nil {
		return JoinState{}, invalidSettings(strings.TrimPrefix(err.Error(), "schema: "))
	}
	if requestAdmitsGuests(req) {
		return JoinState{}, conflict(guestsRemovedReason)
	}
	var out JoinState
	err := s.withImmediateTx(ctx, func(tx execer) error {
		now := credentialNow().Truncate(time.Millisecond)
		cur, err := s.joinStateTx(ctx, tx, now)
		if err != nil {
			return err
		}
		next, err := communityJoinSettings(req, withoutGuests(req.Apply(cur.Settings)))
		if err != nil {
			return err
		}
		if next.OpenJoining == schema.OpenJoiningGuest && !next.GuestsEnabled {
			return conflict("the bare address cannot be guest-only while guests are disabled")
		}
		if next.OpenJoining != schema.OpenJoiningOff && len(next.OpenChannels) == 0 {
			return conflict("open joining needs at least one open channel; turn open joining off first")
		}
		if err := next.Validate(); err != nil {
			return invalidSettings(strings.TrimPrefix(err.Error(), "schema: "))
		}
		if next.GuestsEnabled && !cur.Settings.GuestsEnabled && cur.ReservedNames > 0 {
			return conflict("guests cannot be enabled while a permanent principal has a name in the reserved guest namespace; rename it first")
		}
		nextIDs := map[int64]bool{}
		for _, name := range next.OpenChannels {
			var id int64
			switch err := tx.QueryRowContext(ctx, "SELECT id FROM channels WHERE name = ?", name).Scan(&id); {
			case errors.Is(err, sql.ErrNoRows):
				return invalidSettings("an open channel names no channel on this server")
			case err != nil:
				return fmt.Errorf("store: put join settings: find channel: %w", err)
			}
			nextIDs[id] = true
		}
		curChannels, err := openChannelsTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := checkOpenChannelsWriterTx(ctx, tx, actorID, next, curChannels, nextIDs); err != nil {
			return err
		}
		if err := refreshOpenChannelsListerTx(ctx, tx, actorID, nextIDs); err != nil {
			return err
		}

		c, n := cur.Settings, next
		changes, err := writeJoinSettingsTx(ctx, tx, c, n, now)
		if err != nil {
			return err
		}
		if len(changes) > 0 {
			detail := strings.Join(changes, " ")
			if c.GuestsEnabled && !n.GuestsEnabled {
				// Turning guests off leaves the live visits to their bounds;
				// the row says how many, so the operator knows End all guest
				// visits is there (review F12).
				detail += fmt.Sprintf(" live_guests=%d", cur.LiveGuests)
			}
			if err := appendAuditEventTx(ctx, tx, actor, AuditJoinSettingsChanged, "server", detail, now); err != nil {
				return err
			}
		}
		curIDs := map[int64]bool{}
		for _, ch := range curChannels {
			curIDs[ch.ID] = true
			if nextIDs[ch.ID] {
				continue
			}
			if err := setDefaultChannelTx(ctx, tx, ch.ID, false, 0); err != nil {
				return err
			}
			if err := appendAuditEventTx(ctx, tx, actor, AuditOpenChannelRemoved, channelSubject(ch.ID), "", now); err != nil {
				return err
			}
		}
		for _, name := range next.OpenChannels {
			var id int64
			if err := tx.QueryRowContext(ctx, "SELECT id FROM channels WHERE name = ?", name).Scan(&id); err != nil {
				return fmt.Errorf("store: put join settings: find channel: %w", err)
			}
			if curIDs[id] {
				continue
			}
			if err := setDefaultChannelTx(ctx, tx, id, true, actorID); err != nil {
				return err
			}
			if err := appendAuditEventTx(ctx, tx, actor, AuditOpenChannelAdded, channelSubject(id), "", now); err != nil {
				return err
			}
		}
		out, err = s.joinStateTx(ctx, tx, now)
		return err
	})
	if err != nil {
		return JoinState{}, err
	}
	return out, nil
}

// checkOpenChannelsWriterTx holds a writer of the joining settings without
// manage_channels to the channels it is in (ADR-014 design §7.3, SF-2). Two
// rules, both refused with a PermissionError (ErrPermissionDenied, naming
// manage_channels); actor 0 (the system, or --auth off) is held to neither:
//
//   - every channel the write adds to open_channels must be one the writer
//     is in, whatever the mode. A channel enters the list only behind a
//     lister who could vouch for it then. Without this a writer could list a
//     private channel while the address is off or paused, and an owner's
//     later write (an unpause), which becomes the lister of every channel it
//     leaves listed (refreshOpenChannelsListerTx), would vouch for it
//     unawares. This rule is the smaller fix: it keeps the design's
//     owner-refresh rule whole, where narrowing the refresh to channels the
//     owner names would still be defeated by a client that sends back the
//     list it read;
//   - if next admits people (any mode but off, not paused), every listed
//     channel (nextIDs) must be one the writer is in, whether or not this
//     write touched the list: a writer may not open the address over
//     channels others listed.
func checkOpenChannelsWriterTx(ctx context.Context, tx execer, actorID int64, next schema.JoinSettingsV1, cur []schema.InviteChannelV1, nextIDs map[int64]bool) error {
	if actorID == 0 || len(nextIDs) == 0 {
		return nil
	}
	ok, err := permittedTx(ctx, tx, actorID, schema.PermManageChannels)
	if err != nil || ok {
		return err
	}
	check := nextIDs
	if next.OpenJoining == schema.OpenJoiningOff || next.JoiningPaused {
		// The address admits nobody: only what this write adds.
		listed := make(map[int64]bool, len(cur))
		for _, c := range cur {
			listed[c.ID] = true
		}
		check = map[int64]bool{}
		for id := range nextIDs {
			if !listed[id] {
				check[id] = true
			}
		}
	}
	for id := range check {
		var one int
		switch err := tx.QueryRowContext(ctx,
			"SELECT 1 FROM channel_members WHERE channel_id = ? AND principal_id = ?", id, actorID).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return &PermissionError{Err: ErrPermissionDenied, Permission: schema.PermManageChannels,
				Reason: "listing a channel you are not in, or opening the address to one, needs manage_channels"}
		case err != nil:
			return fmt.Errorf("store: put join settings: check membership: %w", err)
		}
	}
	return nil
}

// refreshOpenChannelsListerTx makes an owner's write of the joining
// settings the lister of every channel it leaves listed (ADR-014 design
// §7.3): open_channels.added_by is who stands behind each channel at every
// address join (admittingOpenChannelsTx), and an owner's write vouches for
// the whole list it leaves. A write by anyone else changes no lister but
// the channels it adds.
func refreshOpenChannelsListerTx(ctx context.Context, tx execer, actorID int64, nextIDs map[int64]bool) error {
	if actorID == 0 || len(nextIDs) == 0 {
		return nil
	}
	actor, found, err := queryPrincipal(ctx, tx, actorID)
	if err != nil || !found || !actor.IsOwner() {
		return err
	}
	for id := range nextIDs {
		if _, err := tx.ExecContext(ctx,
			"UPDATE channels SET default_listed_by = ? WHERE id = ? AND is_default = 1 AND default_listed_by IS NOT ?", actorID, id, actorID); err != nil {
			return fmt.Errorf("store: put join settings: refresh open channel lister: %w", err)
		}
	}
	return nil
}

// communityJoinSettings applies Option B's write mapping (design §3.4) to
// next, the settings req makes: open_joining account or off and open_signup
// say the same thing, so a request naming both must agree, and either one
// sets the other; join_voice is always on. The route-level refusals of a
// join_voice false and of a private open channel are I7's (issue #987);
// until then join_voice false is not stored, and listing a channel makes it
// public (setDefaultChannelTx), as migration 40 made every open channel.
func communityJoinSettings(req schema.PutJoinSettingsRequestV1, next schema.JoinSettingsV1) (schema.JoinSettingsV1, error) {
	if req.OpenSignup != nil {
		mode := schema.OpenJoiningOff
		if *req.OpenSignup {
			mode = schema.OpenJoiningAccount
		}
		if req.OpenJoining != nil && *req.OpenJoining != mode {
			return next, conflict("open_joining and open_signup disagree; name one")
		}
		next.OpenJoining = mode
	}
	signup := next.OpenJoining == schema.OpenJoiningAccount || next.OpenJoining == schema.OpenJoiningBoth
	next.OpenSignup = &signup
	next.JoinVoice = true
	return next, nil
}

// writeJoinSettingsTx writes each of the settings that differs between c and
// n, and returns them as field=old->new, in a fixed order. The mode is
// reported as open_joining, the field clients know, and stored as
// open_signup (migration 40); join_voice is no longer stored.
func writeJoinSettingsTx(ctx context.Context, tx execer, c, n schema.JoinSettingsV1, now time.Time) ([]string, error) {
	var changes []string
	expiry := func(s schema.JoinSettingsV1) string {
		if s.MemberLinkDefaultExpirySeconds == nil {
			return strconv.FormatInt(schema.DefaultMemberLinkExpirySeconds, 10)
		}
		return strconv.FormatInt(*s.MemberLinkDefaultExpirySeconds, 10)
	}
	for _, f := range []struct{ field, key, old, val, stored string }{
		{"open_joining", settingOpenSignup, string(c.OpenJoining), string(n.OpenJoining),
			strconv.FormatBool(n.OpenJoining == schema.OpenJoiningAccount || n.OpenJoining == schema.OpenJoiningBoth)},
		{settingMemberLinkDefaultExpirySeconds, settingMemberLinkDefaultExpirySeconds, expiry(c), expiry(n), expiry(n)},
		{settingGuestsEnabled, settingGuestsEnabled, strconv.FormatBool(c.GuestsEnabled), strconv.FormatBool(n.GuestsEnabled), strconv.FormatBool(n.GuestsEnabled)},
		{settingJoiningPaused, settingJoiningPaused, strconv.FormatBool(c.JoiningPaused), strconv.FormatBool(n.JoiningPaused), strconv.FormatBool(n.JoiningPaused)},
		{settingMaxGuests, settingMaxGuests, strconv.FormatInt(c.MaxGuests, 10), strconv.FormatInt(n.MaxGuests, 10), strconv.FormatInt(n.MaxGuests, 10)},
		{settingMaxGuestsPerAddress, settingMaxGuestsPerAddress, strconv.FormatInt(c.MaxGuestsPerAddress, 10), strconv.FormatInt(n.MaxGuestsPerAddress, 10), strconv.FormatInt(n.MaxGuestsPerAddress, 10)},
		{settingGuestIdleSeconds, settingGuestIdleSeconds, strconv.FormatInt(c.GuestIdleSeconds, 10), strconv.FormatInt(n.GuestIdleSeconds, 10), strconv.FormatInt(n.GuestIdleSeconds, 10)},
		{settingGuestAbsoluteSeconds, settingGuestAbsoluteSeconds, strconv.FormatInt(c.GuestAbsoluteSeconds, 10), strconv.FormatInt(n.GuestAbsoluteSeconds, 10), strconv.FormatInt(n.GuestAbsoluteSeconds, 10)},
	} {
		if f.old == f.val {
			continue
		}
		changes = append(changes, f.field+"="+f.old+"->"+f.val)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			f.key, f.stored, now.UnixMilli()); err != nil {
			return nil, fmt.Errorf("store: write join settings: %s: %w", f.key, err)
		}
	}
	return changes, nil
}

// NormalizeGuestSettings turns off every stored setting that admits guests,
// as guests are removed for now (issue #1005): guests_enabled becomes false,
// an open_joining of both becomes account and one of guest becomes off. It
// runs in one IMMEDIATE transaction, audited as actor "system" in one
// join_settings_changed row whose detail is each field that changed, the
// live visits and reason=guests_removed. Live visits are not ended: they
// last to their bounds, or an operator ends them. Settings that admit no
// guest write nothing, so it is idempotent; Open runs it on every start. It
// reports whether it changed anything.
//
// It reads the settings first, outside any transaction, and takes the write
// lock only when they still admit guests, then reads them again under it:
// opening a database that admits none, every start after the first, waits
// for no writer.
func (s *Store) NormalizeGuestSettings(ctx context.Context) (bool, error) {
	cur, err := joinSettingsTx(ctx, s.db)
	if err != nil {
		return false, fmt.Errorf("store: turn guests off: %w", err)
	}
	if !settingsAdmitGuests(cur) {
		return false, nil
	}
	changed := false
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		now := credentialNow().Truncate(time.Millisecond)
		// Again, under the lock: another process may have written since.
		locked, err := joinSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		changes, err := writeJoinSettingsTx(ctx, tx, locked, withoutGuests(locked), now)
		if err != nil || len(changes) == 0 {
			return err
		}
		live, err := liveGuestsTx(ctx, tx, now)
		if err != nil {
			return err
		}
		changed = true
		return appendAuditEventTx(ctx, tx, "system", AuditJoinSettingsChanged, "server",
			fmt.Sprintf("%s live_guests=%d reason=guests_removed", strings.Join(changes, " "), live), now)
	})
	if err != nil {
		return false, fmt.Errorf("store: turn guests off: %w", err)
	}
	return changed, nil
}

// GuestEntry is who asks to start a visit: the optional display label and
// the server's name, which no label may take, and AddressPeers, the visits
// the server saw start from the caller's address, for the per-address cap.
type GuestEntry struct {
	Label        string
	ServerName   string
	AddressPeers []int64
}

// AdmittedGuest is a visit an entry point started, with the channels it was
// admitted to.
type AdmittedGuest struct {
	CreatedGuest
	Channels []schema.InviteChannelV1
}

// JoinAsGuest starts a visit from the bare address (POST /v1/join/guest),
// in one IMMEDIATE transaction that reads the policy again: guests enabled
// and the address's mode offering them (else ErrGuestsDisabled), not paused
// (ErrJoiningPaused), at least one open channel (ErrGuestsDisabled), and the
// caps (ErrGuestsFull). The visit is admitted to every open channel with the
// settings' bounds and join_voice, as actor "system". A label the rules
// refuse is ErrGuestLabelInvalid, before anything is read.
func (s *Store) JoinAsGuest(ctx context.Context, e GuestEntry) (AdmittedGuest, error) {
	p, err := prepareGuest(e.Label, e.ServerName)
	if err != nil {
		return AdmittedGuest{}, err
	}
	var out AdmittedGuest
	err = s.withImmediateTx(ctx, func(tx execer) error {
		now := credentialNow().Truncate(time.Millisecond)
		set, err := joinSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if !AddressAdmitsGuests(set) {
			return ErrGuestsDisabled
		}
		if set.JoiningPaused {
			return ErrJoiningPaused
		}
		// The channels whose lister still stands behind them, as for an
		// account (ADR-014 design §7.3). Guests are removed for now (#1005);
		// this keeps the path from becoming a hole if they return.
		channels, dropped, err := admittingOpenChannelsTx(ctx, tx)
		if err != nil {
			return err
		}
		if len(channels) == 0 {
			return ErrGuestsDisabled
		}
		for _, c := range dropped {
			if err := appendAuditEventTx(ctx, tx, "system", AuditJoinChannelDropped, channelSubject(c.ID), "reason=permission", now); err != nil {
				return err
			}
		}
		ids := make([]int64, len(channels))
		for i, c := range channels {
			ids[i] = c.ID
		}
		g := guestUnder(set, e)
		g.Origin, g.Channels, g.VoiceAllowed = schema.GuestOriginAddress, ids, set.JoinVoice
		created, err := createGuestTx(ctx, tx, "system", g, p, now)
		if err != nil {
			return err
		}
		out = AdmittedGuest{CreatedGuest: created, Channels: channels}
		return nil
	})
	if err != nil {
		return AdmittedGuest{}, err
	}
	out.Token = p.token
	return out, nil
}

// CheckJoinAccount is what POST /v1/join/account asks before it hashes
// anything: whether the bare address could make an account now
// (addressAccountPolicy: ErrJoiningClosed, ErrJoiningPaused). JoinAsAccount
// checks it again in the creating transaction.
func (s *Store) CheckJoinAccount(ctx context.Context) error {
	set, err := joinSettingsTx(ctx, s.db)
	if err != nil {
		return err
	}
	return addressAccountPolicy(set)
}

// AdmittedAccount is an account the bare address made: its principal, its
// first credential and that credential's token, shown only here, and the
// channels it joined.
type AdmittedAccount struct {
	Principal  Principal
	Credential schema.CredentialV1
	Token      string
	Channels   []schema.InviteChannelV1
}

// AccountCredentialLabel is the label of the first credential of an
// account made from the bare address.
const AccountCredentialLabel = "account created"

// JoinAsAccount makes a member called name from the bare address (POST
// /v1/join/account, issue #711, design §3.1), with record, the encoded
// Argon2id record of the password it chose, made by the server outside this
// transaction. In one IMMEDIATE transaction it reads the policy again
// (addressAccountPolicy: ErrJoiningClosed, ErrJoiningPaused) and the open
// channels, claims the name (claimNameTx: ErrDuplicate, ErrNameReserved for
// the hidden and guest namespaces and the server's name, ErrNameInvalid),
// and makes the principal, its password, its memberships of every open
// channel and its first credential together, so a refusal or a failure leaves nothing
// behind. The account gets what a bare-address joiner gets and nothing
// more: join_voice, no right to invite, can_decide 0 in every channel, and,
// as a guest's, a history bound at each channel's last message, since the
// bare address is open to anyone and designating a channel for it does not
// publish its earlier history (design §2). The credential renews as an
// invited member's does, with an invite's default lifetime. Every row is
// attributed to "system": principal_created, password_set (via=join),
// member_added for each channel and credential_created. now is the invite
// clock, as for RedeemInvite.
func (s *Store) JoinAsAccount(ctx context.Context, name, record string, now time.Time) (AdmittedAccount, error) {
	now = now.Truncate(time.Millisecond)
	if !strings.HasPrefix(record, passwordRecordPrefix) {
		return AdmittedAccount{}, ErrPasswordRecordInvalid
	}
	if err := (schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: name}).Validate(); err != nil {
		return AdmittedAccount{}, fmt.Errorf("%w: %w", ErrNameInvalid, err)
	}
	token, err := newCredentialToken()
	if err != nil {
		return AdmittedAccount{}, err
	}
	const actor = "system"
	var out AdmittedAccount
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out = AdmittedAccount{}
		set, err := joinSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if err := addressAccountPolicy(set); err != nil {
			return err
		}
		// Only the channels whose lister still stands behind them (ADR-014
		// design §7.3); with none left the address is closed to this join.
		channels, dropped, err := admittingOpenChannelsTx(ctx, tx)
		if err != nil {
			return err
		}
		if len(channels) == 0 {
			return ErrJoiningClosed
		}
		keys, err := s.claimNameTx(ctx, tx, name, 0)
		if err != nil {
			return fmt.Errorf("store: join as an account: %w", err)
		}
		for _, c := range dropped {
			if err := appendAuditEventTx(ctx, tx, actor, AuditJoinChannelDropped, channelSubject(c.ID), "reason=permission", now); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO principals (kind, name, name_key, name_skel, role, created_at, voice_allowed)
			 VALUES ('human', ?, ?, ?, 'member', ?, ?)`,
			name, keys.Folded, keys.Skeletal, now.UnixMilli(), boolInt(set.JoinVoice))
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("store: join as an account: create principal %q: %w", name, ErrDuplicate)
		}
		if err != nil {
			return fmt.Errorf("store: join as an account: create principal: %w", err)
		}
		pid, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: join as an account: principal id: %w", err)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditPrincipalCreated, principalActor(pid), "kind=human origin=address", now); err != nil {
			return err
		}
		if _, err := setPasswordTx(ctx, tx, pid, record, now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditPasswordSet, principalActor(pid), "via=join", now); err != nil {
			return err
		}
		for _, c := range channels {
			watermark, err := historyWatermarkTx(ctx, tx, c.ID)
			if err != nil {
				return err
			}
			// A joiner from the bare address never gets the right to decide
			// the channel's approvals, as a joiner through an invite does
			// not (issue #450): the column's default of 1 is for members an
			// operator adds.
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO channel_members (channel_id, principal_id, created_at, can_decide, history_after) VALUES (?, ?, ?, 0, ?)`,
				c.ID, pid, now.UnixMilli(), watermark); err != nil {
				return fmt.Errorf("store: join as an account: add member: %w", err)
			}
			if err := appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(c.ID), memberDetail(pid), now); err != nil {
				return err
			}
		}
		lifetime := schema.DefaultInviteCredentialLifetimeSeconds * time.Second
		exp := now.Add(lifetime)
		cred, err := insertCredentialTx(ctx, tx, pid, AccountCredentialLabel, token, now, &exp, lifetime)
		if err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, "credential_created", principalActor(pid), credentialDetail(cred.ID, cred.Label), now); err != nil {
			return err
		}
		out = AdmittedAccount{
			Principal:  Principal{ID: pid, Kind: PrincipalHuman, Name: name, Role: RoleMember, CreatedAt: now, VoiceAllowed: set.JoinVoice},
			Credential: cred,
			Channels:   channels,
		}
		return nil
	})
	if err != nil {
		return AdmittedAccount{}, err
	}
	out.Token = token
	return out, nil
}

// guestUnder is a visit under the settings' bounds and caps, for e.
func guestUnder(set schema.JoinSettingsV1, e GuestEntry) NewGuest {
	return NewGuest{
		Label: e.Label, ServerName: e.ServerName,
		Idle:          time.Duration(set.GuestIdleSeconds) * time.Second,
		Absolute:      time.Duration(set.GuestAbsoluteSeconds) * time.Second,
		MaxLive:       int(set.MaxGuests),
		MaxPerAddress: int(set.MaxGuestsPerAddress),
		AddressPeers:  e.AddressPeers,
	}
}

// RedeemInviteGuest starts a visit through the invite whose code hashes to
// codeHash (POST /v1/invites/redeem-guest), in one IMMEDIATE transaction
// that checks, in this order: invites not paused (ErrInvitesPaused) and
// guests enabled (ErrGuestsDisabled), both before the code so neither says
// anything about it; the invite usable as for RedeemInvite
// (ErrInviteInvalid); its join_as admitting guests (ErrGuestsDisabled, told
// only to a holder of a good code); then it spends one use and starts the
// visit under the caps (ErrGuestsFull, which spends nothing). The visit gets
// what an account would: the channels the invite still grants and its voice,
// never more than its maker holds, and its hard deadline is the earlier of
// the absolute bound and the invite's credential lifetime. Every row is
// attributed to "invite:<id>", invite_redeemed with guest=1 among them.
// now is the invite clock, as for RedeemInvite.
func (s *Store) RedeemInviteGuest(ctx context.Context, codeHash string, e GuestEntry, now time.Time) (AdmittedGuest, error) {
	now = now.Truncate(time.Millisecond)
	p, err := prepareGuest(e.Label, e.ServerName)
	if err != nil {
		return AdmittedGuest{}, err
	}
	var out AdmittedGuest
	err = s.withImmediateTx(ctx, func(tx execer) error {
		vnow := credentialNow().Truncate(time.Millisecond)
		if paused, err := invitesPausedTx(ctx, tx); err != nil {
			return err
		} else if paused {
			return ErrInvitesPaused
		}
		set, err := joinSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if !set.GuestsEnabled {
			return ErrGuestsDisabled
		}
		r, kept, dropped, err := usableInviteTx(ctx, tx, codeHash, now)
		if err != nil {
			return err
		}
		if !InviteAdmitsGuests(r.joinAs) {
			return ErrGuestsDisabled
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE invites SET uses = uses + 1
			 WHERE id = ? AND closed_at IS NULL AND (max_uses IS NULL OR uses < max_uses)`, r.id)
		if err != nil {
			return fmt.Errorf("store: redeem invite as a guest: spend use: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: redeem invite as a guest: spend use: %w", err)
		} else if n != 1 {
			return ErrInviteInvalid
		}
		ids := make([]int64, len(kept))
		for i, c := range kept {
			ids[i] = c.ID
		}
		g := guestUnder(set, e)
		g.Origin, g.InviteID, g.InvitedBy, g.Channels = schema.GuestOriginInvite, r.id, r.createdBy, ids
		g.CredentialLifetime = time.Duration(r.lifetimeMs) * time.Millisecond
		g.VoiceAllowed = r.voice && r.creatorVoice
		actor := inviteSubject(r.id)
		created, err := createGuestTx(ctx, tx, actor, g, p, vnow)
		if err != nil {
			return err
		}
		droppedIDs := make([]string, len(dropped))
		for i, c := range dropped {
			droppedIDs[i] = strconv.FormatInt(c.ID, 10)
		}
		droppedText := "none"
		if len(droppedIDs) > 0 {
			droppedText = strings.Join(droppedIDs, ",")
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditInviteRedeemed, inviteSubject(r.id),
			fmt.Sprintf("invite=%d principal=%d uses=%d dropped_channels=%s guest=1", r.id, created.Principal.ID, r.uses+1, droppedText), vnow); err != nil {
			return err
		}
		out = AdmittedGuest{CreatedGuest: created, Channels: kept}
		return nil
	})
	if err != nil {
		return AdmittedGuest{}, err
	}
	out.Token = p.token
	return out, nil
}

// GuestFilter selects visits for ListGuests. State is required; Origin
// narrows it when set; Before, when positive, lists only visits whose
// principal id is below it; Limit is the page size.
type GuestFilter struct {
	State  schema.GuestState
	Origin schema.GuestOrigin
	Before int64
	Limit  int
}

// ListGuests returns one page of visits matching f, newest first (by
// principal id, which grows with each visit), and whether more may follow.
func (s *Store) ListGuests(ctx context.Context, f GuestFilter) ([]GuestVisit, bool, error) {
	if !f.State.Valid() || (f.Origin != "" && !f.Origin.Valid()) || f.Limit < 1 {
		return nil, false, errors.New("store: list guests: a known state, a known origin and a positive limit")
	}
	q := `SELECT ` + guestVisitColumns + guestVisitFrom + ` WHERE v.state = ?`
	args := []any{string(f.State)}
	if f.Origin != "" {
		q += " AND v.origin = ?"
		args = append(args, string(f.Origin))
	}
	if f.Before > 0 {
		q += " AND v.principal_id < ?"
		args = append(args, f.Before)
	}
	q += " ORDER BY v.principal_id DESC LIMIT ?"
	args = append(args, f.Limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, false, fmt.Errorf("store: list guests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []GuestVisit{}
	for rows.Next() {
		v, err := scanGuestVisit(rows)
		if err != nil {
			return nil, false, fmt.Errorf("store: list guests: %w", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("store: list guests: %w", err)
	}
	more := len(out) > f.Limit
	if more {
		out = out[:f.Limit]
	}
	return out, more, nil
}

// GuestSelector is whom End all guest visits selects: every active visit, or
// those of one origin, or those that came through one invite. Converted and
// ended visits are never selected.
type GuestSelector struct {
	Origin   schema.GuestOrigin
	InviteID int64
}

func (sel GuestSelector) where() (string, []any) {
	q := "v.state = 'active'"
	var args []any
	if sel.Origin != "" {
		q += " AND v.origin = ?"
		args = append(args, string(sel.Origin))
	}
	if sel.InviteID > 0 {
		q += " AND v.invite_id = ?"
		args = append(args, sel.InviteID)
	}
	return q, args
}

func (sel GuestSelector) detail() string {
	out := "origin=any"
	if sel.Origin != "" {
		out = "origin=" + string(sel.Origin)
	}
	if sel.InviteID > 0 {
		out += fmt.Sprintf(" invite=%d", sel.InviteID)
	}
	return out
}

// GuestRef names one visit End all guest visits selected.
type GuestRef struct {
	ID     int64
	Handle string
}

// guestTargetsTx returns the first limit active visits sel selects, oldest
// first, and how many more it selects.
func guestTargetsTx(ctx context.Context, tx execer, sel GuestSelector, limit int) ([]GuestRef, int, error) {
	where, args := sel.where()
	var total int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM guest_visits v WHERE "+where, args...).Scan(&total); err != nil { // #nosec G202 -- where joins constants; every value is in args
		return nil, 0, fmt.Errorf("store: guest targets: %w", err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT v.principal_id, v.handle FROM guest_visits v WHERE "+where+ // #nosec G202 -- as above
		" ORDER BY v.principal_id LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, 0, fmt.Errorf("store: guest targets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []GuestRef{}
	for rows.Next() {
		var g GuestRef
		if err := rows.Scan(&g.ID, &g.Handle); err != nil {
			return nil, 0, fmt.Errorf("store: guest targets: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("store: guest targets: %w", err)
	}
	return out, max(0, total-len(out)), nil
}

// EndGuests is End all guest visits: it ends, with reason operator, the first
// limit active visits sel selects, and returns them with how many more it
// selects. With dryRun it only lists them and writes nothing. Otherwise every
// visit ends in one IMMEDIATE transaction, each as EndGuestVisit ends one
// (guest_ended, its memberships and its credential), and guests_ended
// summarises them; OnGuestsEnded is told after the commit, which closes
// their sockets and takes them out of voice.
func (s *Store) EndGuests(ctx context.Context, actor string, sel GuestSelector, limit int, dryRun bool) ([]GuestRef, int, error) {
	if (sel.Origin != "" && !sel.Origin.Valid()) || sel.InviteID < 0 || limit < 1 {
		return nil, 0, errors.New("store: end guests: a known origin, an invite id and a positive limit")
	}
	if dryRun {
		return guestTargetsTx(ctx, s.db, sel, limit)
	}
	var (
		refs      []GuestRef
		remaining int
		ended     []EndedGuest
		runs      []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		ended, runs = nil, nil
		now := credentialNow().Truncate(time.Millisecond)
		var err error
		if refs, remaining, err = guestTargetsTx(ctx, tx, sel, limit); err != nil {
			return err
		}
		for _, g := range refs {
			e, err := endGuestVisitTx(ctx, tx, actor, g.ID, schema.GuestEndOperator, now, &runs)
			if err != nil {
				return err
			}
			ended = append(ended, e)
		}
		return appendAuditEventTx(ctx, tx, actor, AuditGuestsEnded, "server",
			fmt.Sprintf("count=%d remaining=%d %s", len(refs), remaining, sel.detail()), now)
	})
	if err != nil {
		return nil, 0, err
	}
	s.announceRunsEnded(runs)
	s.announceGuestsEnded(ended)
	return refs, remaining, nil
}
