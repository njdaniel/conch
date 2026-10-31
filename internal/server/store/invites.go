package store

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Invites (issue #436, ADR-008 §4, docs/design/private-alpha.md §3). An
// invite is a second way to get a credential: whoever holds its code joins as
// a new human principal, with the channels and grants the invite names and
// never more than its maker held. This file is the store half; the routes are
// A6b and operator administration A6c.
//
// The code is a secret like a credential token. Only its SHA-256 is stored; the
// plaintext is returned once, by CreateInvite. No code, hash or token appears
// in an audit row, a log line or an error string.

// Audit actions written by the invite store.
const (
	AuditInviteCreated           = "invite_created"
	AuditInviteRedeemed          = "invite_redeemed"
	AuditInviteClosed            = "invite_closed"
	AuditInvitesPaused           = "invites_paused"
	AuditInvitesResumed          = "invites_resumed"
	AuditInvitePermissionChanged = "invite_permission_changed"
	// AuditJoinersDisabled and AuditInviteesDisabled summarise an operator's
	// bulk disable (issue #438); each principal it disabled has its own
	// principal_disabled row as well.
	AuditJoinersDisabled  = "joiners_disabled"
	AuditInviteesDisabled = "invitees_disabled"
	// AuditPrincipalCreated records that a principal was created. Invites
	// write it for a joiner; operator-created principals use it too (A12).
	AuditPrincipalCreated = "principal_created"
)

// Why an invite was closed, the reason= of its invite_closed row (issue
// #438): by an operator who did not make it, by its maker, because its maker
// was disabled, or because its maker lost the right to invite.
const (
	InviteCloseOperator          = "operator"
	InviteCloseCreator           = "creator"
	InviteCloseCreatorDisabled   = "creator_disabled"
	InviteClosePermissionRevoked = "permission_revoked"
)

// DefaultPersonalInviteAllowance is how many outstanding personal invites one
// inviter may hold when the caller names no other number. Operators have none.
const DefaultPersonalInviteAllowance = 5

// settingInvitesPaused is the server_settings key of the pause switch.
const settingInvitesPaused = "invites_paused"

// Errors returned by the invite store, in addition to ErrNotFound,
// ErrDuplicate and ErrPrincipalNotFound.
var (
	// ErrInviteInvalid is the single error for a code that is unknown,
	// expired, used up or closed, or whose inviter is disabled or no longer
	// allowed to invite. The cases are deliberately indistinguishable.
	ErrInviteInvalid = errors.New("store: invite invalid")
	// ErrInvitesPaused is returned while an operator has paused invites.
	ErrInvitesPaused = errors.New("store: invites are paused")
	// ErrInviteAllowanceExceeded is returned when the inviter already has as
	// many outstanding personal invites as the allowance permits.
	ErrInviteAllowanceExceeded = errors.New("store: invite allowance exceeded")
	// ErrInviteGrantExceedsInviter is returned when an invite names a channel
	// or voice its maker does not hold, or asks for the right to invite from a
	// maker who is not an operator (only operators grant it, issue #457).
	ErrInviteGrantExceedsInviter = errors.New("store: invite grants more than its maker holds")
	// ErrInviteNotAllowed is returned when the caller may not make or close
	// this invite, or may not hold the right to invite.
	ErrInviteNotAllowed = errors.New("store: invite not allowed")
	// ErrInviteRequestInvalid wraps a request the schema refuses.
	ErrInviteRequestInvalid = errors.New("store: invalid invite request")
)

// InviteState is where an invite is in its life, for listing.
type InviteState string

// Invite states. Closed wins over used, and used over expired.
const (
	InviteStateOpen    InviteState = "open"
	InviteStateClosed  InviteState = "closed"
	InviteStateUsed    InviteState = "used"
	InviteStateExpired InviteState = "expired"
)

// InviteOptions tunes CreateInvite.
type InviteOptions struct {
	// Allowance is the most outstanding personal invites one non-operator
	// inviter may hold; zero or less means DefaultPersonalInviteAllowance.
	Allowance int
}

// allowance is the per-inviter allowance these options set.
func (o InviteOptions) allowance() int {
	if o.Allowance <= 0 {
		return DefaultPersonalInviteAllowance
	}
	return o.Allowance
}

// InviteFilter selects invites for ListInvites. A zero field matches all.
type InviteFilter struct {
	CreatedBy int64
	Kind      schema.InviteKind
	State     InviteState
}

// RedeemedInvite is what redeeming an invite made.
type RedeemedInvite struct {
	Principal  Principal
	Credential schema.CredentialV1
	// Token is the credential's plaintext token, shown once.
	Token string
	// DroppedChannels are the ids of channels a personal invite named that its
	// maker had left by redemption, so the joiner was not added to them.
	DroppedChannels []int64
}

// NewInviteCode returns a fresh invite code: the "conch_inv_" prefix followed
// by 32 bytes from crypto/rand in unpadded base64url.
func NewInviteCode() (string, error) {
	b := make([]byte, schema.InviteCodeRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generate invite code: %w", err)
	}
	return schema.InviteCodePrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashInviteCode returns the lowercase hex SHA-256 of code: the only form of
// it the store keeps, and what PreviewInvite and RedeemInvite take.
func HashInviteCode(code string) string { return hashCredentialToken(code) }

func inviteSubject(id int64) string { return fmt.Sprintf("invite:%d", id) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// inviteRow is one invites row.
type inviteRow struct {
	id           int64
	kind         schema.InviteKind
	codeHash     string
	createdBy    int64
	createdAt    int64
	expiresAt    int64
	maxUses      sql.NullInt64
	uses         int64
	closedAt     sql.NullInt64
	voice        bool
	mayInvite    bool
	lifetimeMs   int64
	joinAs       schema.InviteJoinMode
	creatorName  string
	creatorOff   bool
	creatorHuman bool
	creatorMay   bool
	creatorVoice bool
	// maker is what the maker holds now, read by loadMakerTx; zero (holding
	// nothing) until it is loaded.
	maker makerRights
}

// makerRights is what an invite's maker holds now, by permission (ADR-014
// design §7.2.1, §7.3): read in the transaction that judges the invite, so a
// change to the document or to the maker takes effect at the next
// redemption, not when the invite was made.
type makerRights struct {
	// members: manage_members, which an open link and a may_invite grant need.
	members bool
	// channels: manage_channels, which keeps the channels the maker is not in.
	channels bool
	// links: make_member_links, which a personal invite needs; for a member it
	// is withheld by the may_invite brake (design §5).
	links bool
}

// makerRightsTx reads what principalID holds now. A principal with no row
// holds nothing.
func makerRightsTx(ctx context.Context, tx execer, principalID int64) (makerRights, error) {
	p, found, err := queryPrincipal(ctx, tx, principalID)
	if err != nil || !found {
		return makerRights{}, err
	}
	d, err := readPermissionDocTx(ctx, tx)
	if err != nil {
		return makerRights{}, err
	}
	return makerRights{
		members:  principalHolds(p, d, schema.PermManageMembers),
		channels: principalHolds(p, d, schema.PermManageChannels),
		links:    principalHolds(p, d, schema.PermMakeMemberLinks),
	}, nil
}

// loadMakerTx fills r.maker from the maker as it is now.
func (r *inviteRow) loadMakerTx(ctx context.Context, tx execer) error {
	m, err := makerRightsTx(ctx, tx, r.createdBy)
	if err != nil {
		return fmt.Errorf("store: invite maker rights: %w", err)
	}
	r.maker = m
	return nil
}

const inviteColumns = `i.id, i.kind, i.code_hash, i.created_by, i.created_at, i.expires_at, i.max_uses, i.uses,
	i.closed_at, i.voice, i.may_invite, i.credential_lifetime_ms, i.join_as,
	p.name, p.disabled_at IS NOT NULL, p.kind = 'human', p.may_invite, p.voice_allowed`

func scanInvite(row interface{ Scan(...any) error }) (inviteRow, error) {
	var r inviteRow
	var kind, joinAs string
	err := row.Scan(&r.id, &kind, &r.codeHash, &r.createdBy, &r.createdAt, &r.expiresAt, &r.maxUses, &r.uses,
		&r.closedAt, &r.voice, &r.mayInvite, &r.lifetimeMs, &joinAs,
		&r.creatorName, &r.creatorOff, &r.creatorHuman, &r.creatorMay, &r.creatorVoice)
	r.kind = schema.InviteKind(kind)
	r.joinAs = schema.InviteJoinMode(joinAs)
	return r, err
}

// state reports where the invite is at now.
func (r inviteRow) state(now time.Time) InviteState {
	switch {
	case r.closedAt.Valid:
		return InviteStateClosed
	case r.maxUses.Valid && r.uses >= r.maxUses.Int64:
		return InviteStateUsed
	case r.expiresAt <= now.UnixMilli():
		return InviteStateExpired
	default:
		return InviteStateOpen
	}
}

// makerStillAllowed reports whether the invite's maker may still stand behind
// it: enabled, human, and holding now the right the invite's kind needs (r.maker,
// loaded by loadMakerTx): manage_members for an open link, make_member_links
// for a personal one (ADR-014 design §7.3). Anything else fails closed, so
// taking an inviter's right away suspends the invites they left
// outstanding, and they work again if it comes back.
func (r inviteRow) makerStillAllowed() bool {
	if r.creatorOff || !r.creatorHuman {
		return false
	}
	if r.kind == schema.InviteKindPersonal {
		return r.maker.links
	}
	return r.maker.members
}

func (r inviteRow) wire(channels []schema.InviteChannelV1) schema.InviteV1 {
	inv := schema.InviteV1{
		ID:                        r.id,
		Kind:                      r.kind,
		CreatedBy:                 r.createdBy,
		CreatedAt:                 schema.NewTimestamp(time.UnixMilli(r.createdAt)),
		ExpiresAt:                 schema.NewTimestamp(time.UnixMilli(r.expiresAt)),
		Uses:                      r.uses,
		ClosedAt:                  msToTimestampPtr(r.closedAt),
		Channels:                  channels,
		Voice:                     r.voice,
		MayInvite:                 r.mayInvite,
		CredentialLifetimeSeconds: r.lifetimeMs / 1000,
		JoinAs:                    r.joinAs,
	}
	if r.maxUses.Valid {
		n := r.maxUses.Int64
		inv.MaxUses = &n
	}
	return inv
}

// inviteChannelsTx returns the channels an invite names, by channel id.
func inviteChannelsTx(ctx context.Context, tx execer, inviteID int64) ([]schema.InviteChannelV1, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT c.id, c.name FROM invite_channels ic JOIN channels c ON c.id = ic.channel_id
		 WHERE ic.invite_id = ? ORDER BY c.id`, inviteID)
	if err != nil {
		return nil, fmt.Errorf("store: invite channels: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schema.InviteChannelV1{}
	for rows.Next() {
		var c schema.InviteChannelV1
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, fmt.Errorf("store: invite channels: scan: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: invite channels: %w", err)
	}
	return out, nil
}

// effectiveChannelsTx splits the channels an invite names into those a joiner
// is added to and those dropped. While its maker holds manage_channels
// (r.maker, loaded by loadMakerTx) an invite keeps every named channel that
// exists. Otherwise it keeps only the channels its maker is still a member
// of, so an invite never outlives its maker's access (ADR-014 design §7.3).
func effectiveChannelsTx(ctx context.Context, tx execer, r inviteRow) (kept, dropped []schema.InviteChannelV1, err error) {
	all, err := inviteChannelsTx(ctx, tx, r.id)
	if err != nil {
		return nil, nil, err
	}
	if r.maker.channels {
		return all, nil, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT channel_id FROM channel_members WHERE principal_id = ?`, r.createdBy)
	if err != nil {
		return nil, nil, fmt.Errorf("store: invite maker memberships: %w", err)
	}
	defer func() { _ = rows.Close() }()
	member := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, nil, fmt.Errorf("store: invite maker memberships: scan: %w", err)
		}
		member[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("store: invite maker memberships: %w", err)
	}
	for _, c := range all {
		if member[c.ID] {
			kept = append(kept, c)
		} else {
			dropped = append(dropped, c)
		}
	}
	return kept, dropped, nil
}

func invitesPausedTx(ctx context.Context, tx execer) (bool, error) {
	var v string
	switch err := tx.QueryRowContext(ctx, "SELECT value FROM server_settings WHERE key = ?", settingInvitesPaused).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: read invites_paused: %w", err)
	}
	// Anything that is not a clear "false" counts as paused: a pause switch
	// that cannot be read must not let people in.
	paused, perr := strconv.ParseBool(v)
	return paused || perr != nil, nil
}

// usableInviteTx finds the invite whose code hashes to codeHash and returns it
// with the channels a joiner would get, or ErrInviteInvalid. The caller has
// already checked the pause switch. The stored hash is compared in constant
// time even though the row was found by it, as for credentials.
func usableInviteTx(ctx context.Context, tx execer, codeHash string, now time.Time) (inviteRow, []schema.InviteChannelV1, []schema.InviteChannelV1, error) {
	r, err := scanInvite(tx.QueryRowContext(ctx,
		`SELECT `+inviteColumns+` FROM invites i JOIN principals p ON p.id = i.created_by WHERE i.code_hash = ?`, codeHash))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return inviteRow{}, nil, nil, ErrInviteInvalid
	case err != nil:
		return inviteRow{}, nil, nil, fmt.Errorf("store: find invite: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(r.codeHash), []byte(codeHash)) != 1 {
		return inviteRow{}, nil, nil, ErrInviteInvalid
	}
	if err := r.loadMakerTx(ctx, tx); err != nil {
		return inviteRow{}, nil, nil, err
	}
	if r.state(now) != InviteStateOpen || !r.makerStillAllowed() {
		return inviteRow{}, nil, nil, ErrInviteInvalid
	}
	kept, dropped, err := effectiveChannelsTx(ctx, tx, r)
	if err != nil {
		return inviteRow{}, nil, nil, err
	}
	if len(kept) == 0 {
		return inviteRow{}, nil, nil, ErrInviteInvalid
	}
	return r, kept, dropped, nil
}

// outstandingPersonalInvitesTx counts creatorID's personal invites that are
// still open at now: not closed, not used up and not expired. It is the one
// rule the allowance is counted by, for CreateInvite and InvitesRemaining
// alike, so what whoami says is left and what CreateInvite allows cannot
// disagree.
func outstandingPersonalInvitesTx(ctx context.Context, tx execer, creatorID int64, now time.Time) (int, error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM invites
		 WHERE created_by = ? AND kind = 'personal' AND closed_at IS NULL AND expires_at > ? AND uses < max_uses`,
		creatorID, now.UnixMilli()).Scan(&n); err != nil {
		return 0, fmt.Errorf("count outstanding personal invites: %w", err)
	}
	return n, nil
}

// InvitesRemaining reports how many more personal invites principalID may
// make at now under opts: the allowance less their open personal invites,
// never below zero (an operator may have lowered the allowance since). It
// does not ask whether they may invite at all, and operators have no
// allowance: the caller decides whether the number applies.
func (s *Store) InvitesRemaining(ctx context.Context, principalID int64, opts InviteOptions, now time.Time) (int, error) {
	outstanding, err := outstandingPersonalInvitesTx(ctx, s.db, principalID, now.Truncate(time.Millisecond))
	if err != nil {
		return 0, fmt.Errorf("store: invites remaining: %w", err)
	}
	return max(opts.allowance()-outstanding, 0), nil
}

// CreateInvite makes an invite for creatorID and returns it with its code,
// which is shown only here. It validates req, applies the defaults for what it
// leaves out, and checks in one IMMEDIATE transaction with the insert that:
//
//   - invites are not paused (ErrInvitesPaused);
//   - the creator is an enabled human who may make this kind, by what it
//     holds now (ADR-014 design §7.3): a personal invite needs
//     make_member_links (an operator, or a member with the right to invite),
//     an open link needs manage_members (ErrInviteNotAllowed);
//   - an invite grants only what its maker holds: without manage_channels
//     the maker must be a member of every channel named; voice needs
//     voice_allowed; only a holder of manage_members grants the right to
//     invite, issue #457 (ErrInviteGrantExceedsInviter; a channel that does
//     not exist is the same answer without manage_channels, and ErrNotFound
//     with it);
//   - without manage_members, the creator has fewer outstanding personal
//     invites than the allowance (ErrInviteAllowanceExceeded).
//
// invite_created is audited in the same transaction. The code is not.
func (s *Store) CreateInvite(ctx context.Context, actor string, creatorID int64, req schema.CreateInviteRequestV1, opts InviteOptions, now time.Time) (schema.CreateInviteResponseV1, error) {
	now = now.Truncate(time.Millisecond)
	if err := req.Validate(now); err != nil {
		return schema.CreateInviteResponseV1{}, fmt.Errorf("%w: %w", ErrInviteRequestInvalid, err)
	}
	allowance := opts.allowance()
	expires := req.EffectiveExpiry(now).Truncate(time.Millisecond)
	mayInvite := req.EffectiveMayInvite()
	lifetimeMs := req.EffectiveCredentialLifetime().Milliseconds()
	joinAs := req.EffectiveJoinAs()
	// Guests are removed for now (issue #1005): an invite admits accounts
	// only. Refused before anything is read, so the answer says nothing else.
	if InviteAdmitsGuests(joinAs) {
		return schema.CreateInviteResponseV1{}, fmt.Errorf("%w: an invite may admit accounts only", ErrGuestsRemoved)
	}
	code, err := NewInviteCode()
	if err != nil {
		return schema.CreateInviteResponseV1{}, err
	}

	var out schema.InviteV1
	err = s.withImmediateTx(ctx, func(tx execer) error {
		if paused, err := invitesPausedTx(ctx, tx); err != nil {
			return err
		} else if paused {
			return ErrInvitesPaused
		}

		creator, found, err := queryPrincipal(ctx, tx, creatorID)
		switch {
		case err != nil:
			return fmt.Errorf("store: create invite: find creator: %w", err)
		case !found:
			return ErrPrincipalNotFound
		}
		if creator.DisabledAt != nil || creator.Kind != PrincipalHuman {
			return ErrInviteNotAllowed
		}
		// What the maker may do is what it holds now, by permission (ADR-014
		// design §7.2.1, §7.3), never its role: manage_members for an open
		// link, its allowance and a may_invite grant; manage_channels for a
		// channel it is not in; make_member_links (with a member's may_invite
		// brake) for a personal invite.
		rights, err := makerRightsTx(ctx, tx, creatorID)
		if err != nil {
			return fmt.Errorf("store: create invite: %w", err)
		}
		switch req.Kind {
		case schema.InviteKindOpen:
			if !rights.members {
				return fmt.Errorf("%w: only an operator makes an open link", ErrInviteNotAllowed)
			}
		default:
			if !rights.links {
				return fmt.Errorf("%w: you may not invite", ErrInviteNotAllowed)
			}
		}
		if req.Voice && !creator.VoiceAllowed {
			return fmt.Errorf("%w: you do not hold voice", ErrInviteGrantExceedsInviter)
		}
		// Only a holder of manage_members grants the right to invite (issue
		// #457): a tester who may invite still cannot pass the right on.
		if mayInvite && !rights.members {
			return fmt.Errorf("%w: only an operator grants the right to invite", ErrInviteGrantExceedsInviter)
		}

		channelIDs := make([]int64, 0, len(req.Channels))
		for _, name := range req.Channels {
			var id int64
			switch err := tx.QueryRowContext(ctx, "SELECT id FROM channels WHERE name = ?", name).Scan(&id); {
			case errors.Is(err, sql.ErrNoRows):
				if rights.channels {
					return fmt.Errorf("store: create invite: channel %q: %w", name, ErrNotFound)
				}
				// Not told apart from a channel the creator is not in, so
				// naming channels does not reveal which exist.
				return fmt.Errorf("%w: a channel you are not in", ErrInviteGrantExceedsInviter)
			case err != nil:
				return fmt.Errorf("store: create invite: find channel: %w", err)
			}
			if !rights.channels {
				var one int
				switch err := tx.QueryRowContext(ctx,
					"SELECT 1 FROM channel_members WHERE channel_id = ? AND principal_id = ?", id, creatorID).Scan(&one); {
				case errors.Is(err, sql.ErrNoRows):
					return fmt.Errorf("%w: a channel you are not in", ErrInviteGrantExceedsInviter)
				case err != nil:
					return fmt.Errorf("store: create invite: check membership: %w", err)
				}
			}
			channelIDs = append(channelIDs, id)
		}

		// The allowance binds anyone without manage_members, a restricted
		// operator included (ADR-014 design §7.3, N1).
		if req.Kind == schema.InviteKindPersonal && !rights.members {
			outstanding, err := outstandingPersonalInvitesTx(ctx, tx, creatorID, now)
			if err != nil {
				return fmt.Errorf("store: create invite: %w", err)
			}
			if outstanding >= allowance {
				return ErrInviteAllowanceExceeded
			}
		}

		var maxUses any
		switch {
		case req.Kind == schema.InviteKindPersonal:
			maxUses = 1
		case req.MaxUses != nil:
			maxUses = *req.MaxUses
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO invites (kind, code_hash, created_by, created_at, expires_at, max_uses, voice, may_invite, credential_lifetime_ms, join_as)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(req.Kind), HashInviteCode(code), creatorID, now.UnixMilli(), expires.UnixMilli(), maxUses,
			boolInt(req.Voice), boolInt(mayInvite), lifetimeMs, string(joinAs))
		if err != nil {
			return fmt.Errorf("store: create invite: insert: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create invite: id: %w", err)
		}
		for _, cid := range channelIDs {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO invite_channels (invite_id, channel_id) VALUES (?, ?)", id, cid); err != nil {
				return fmt.Errorf("store: create invite: insert channel: %w", err)
			}
		}

		r, err := scanInvite(tx.QueryRowContext(ctx,
			`SELECT `+inviteColumns+` FROM invites i JOIN principals p ON p.id = i.created_by WHERE i.id = ?`, id))
		if err != nil {
			return fmt.Errorf("store: create invite: read back: %w", err)
		}
		channels, err := inviteChannelsTx(ctx, tx, id)
		if err != nil {
			return err
		}
		out = r.wire(channels)

		ids := make([]string, len(channels))
		for i, c := range channels {
			ids[i] = strconv.FormatInt(c.ID, 10)
		}
		uses := "none"
		if out.MaxUses != nil {
			uses = strconv.FormatInt(*out.MaxUses, 10)
		}
		detail := fmt.Sprintf("invite=%d kind=%s channels=%s voice=%t may_invite=%t expires=%s max_uses=%s",
			id, req.Kind, strings.Join(ids, ","), req.Voice, mayInvite, out.ExpiresAt.Time().Format(time.RFC3339), uses)
		// Said only when it is not the default, so an account invite's row
		// reads exactly as it did before invites had modes.
		if joinAs != schema.InviteJoinAccount {
			detail += " join_as=" + string(joinAs)
		}
		return appendAuditEventTx(ctx, tx, actor, AuditInviteCreated, inviteSubject(id), detail, now)
	})
	if err != nil {
		return schema.CreateInviteResponseV1{}, err
	}
	return schema.CreateInviteResponseV1{Invite: out, Code: code}, nil
}

// PreviewInvite says what the invite with this code hash would grant, without
// using it: who made it and the channels a joiner would be added to. It
// returns ErrInvitesPaused while invites are paused (checked before the code,
// so a pause says nothing about any code) and ErrInviteInvalid for a code that
// is unknown, expired, used up, closed, or whose maker may no longer stand
// behind it. Join is what the invite offers now, by the rule redemption
// applies (inviteJoinOptions): its join_as under the server's guest switch
// and the cap on live visits; the per-address cap is the server's to apply.
// It writes nothing.
func (s *Store) PreviewInvite(ctx context.Context, codeHash string, now time.Time) (schema.InvitePreviewV1, error) {
	if paused, err := s.InvitesPaused(ctx); err != nil {
		return schema.InvitePreviewV1{}, err
	} else if paused {
		return schema.InvitePreviewV1{}, ErrInvitesPaused
	}
	// A read, not a write transaction: this is the unauthenticated route, and
	// it must not queue behind (or hold up) writers.
	r, kept, _, err := usableInviteTx(ctx, s.db, codeHash, now)
	if err != nil {
		return schema.InvitePreviewV1{}, err
	}
	settings, err := joinSettingsTx(ctx, s.db)
	if err != nil {
		return schema.InvitePreviewV1{}, err
	}
	live, err := liveGuestsTx(ctx, s.db, credentialNow())
	if err != nil {
		return schema.InvitePreviewV1{}, err
	}
	join := inviteJoinOptions(r.joinAs, settings, live)
	names := make([]string, len(kept))
	for i, c := range kept {
		names[i] = c.Name
	}
	return schema.InvitePreviewV1{
		Kind:      r.kind,
		Inviter:   r.creatorName,
		Channels:  names,
		Voice:     r.voice && r.creatorVoice,
		ExpiresAt: schema.NewTimestamp(time.UnixMilli(r.expiresAt)),
		Join:      &join,
	}, nil
}

// RedeemInvite uses the invite with this code hash to make a new human member
// called name, in one IMMEDIATE transaction: it checks the invite (as
// PreviewInvite does), spends one use, creates the principal with the invite's
// grants and the invite and inviter it came through, adds its memberships,
// issues its first credential (label "invite <id>", renewable with the
// invite's credential lifetime) and audits invite_redeemed, principal_created,
// member_added for each channel and credential_created. If anything fails
// nothing is written. A name already taken is ErrDuplicate, found only after
// the invite proved good, so a name is never probed without a valid code.
//
// A personal invite adds the joiner only to the channels its maker is still in;
// the rest are dropped and named in the invite_redeemed detail. Two concurrent
// redemptions of a personal invite serialize on the write lock: one wins.
//
// All audit rows are attributed to "invite:<id>": the joiner has no identity
// before the redemption commits.
//
// The account it makes has no password. No route calls it any more: POST
// /v1/invites/redeem answers password_required (#713) and POST
// /v2/invites/redeem uses RedeemInviteWithPassword (issue #707). It stays
// for tests that need a member made through an invite without a password.
func (s *Store) RedeemInvite(ctx context.Context, codeHash, name string, now time.Time) (RedeemedInvite, error) {
	return s.redeemInvite(ctx, codeHash, name, "", now)
}

// RedeemInviteWithPassword is RedeemInvite for an account made with its
// password (POST /v2/invites/redeem, issue #707): record is the encoded
// Argon2id record the server made, outside this transaction, of the
// password the joiner chose. It is stored with the principal in the same
// transaction, and password_set is audited after principal_created, so the
// account exists only with a usable password, and a redemption that fails
// stores neither and does not use the invite. A record that is not one is
// ErrPasswordRecordInvalid, before anything is read.
func (s *Store) RedeemInviteWithPassword(ctx context.Context, codeHash, name, record string, now time.Time) (RedeemedInvite, error) {
	if !strings.HasPrefix(record, passwordRecordPrefix) {
		return RedeemedInvite{}, ErrPasswordRecordInvalid
	}
	return s.redeemInvite(ctx, codeHash, name, record, now)
}

// ErrJoiningClosed is an entry point asked to make an account it does not
// offer (ErrorCodeJoiningClosed, design §7.2): an invite that admits guests
// only (join_as guest), or the bare address while open_joining is off or
// guest-only (JoinAsAccount, issue #711).
var ErrJoiningClosed = errors.New("store: this entry point makes no account now")

// inviteAdmitsAccountTx returns ErrJoiningClosed when invite id admits
// guests only. Absent, as every invite from before guests is, join_as is
// account.
func inviteAdmitsAccountTx(ctx context.Context, tx execer, id int64) error {
	var joinAs string
	if err := tx.QueryRowContext(ctx, "SELECT join_as FROM invites WHERE id = ?", id).Scan(&joinAs); err != nil {
		return fmt.Errorf("store: invite join mode: %w", err)
	}
	if schema.InviteJoinMode(joinAs) == schema.InviteJoinGuest {
		return ErrJoiningClosed
	}
	return nil
}

// CheckInviteForAccount is what POST /v2/invites/redeem asks before it
// hashes anything: whether the invite with this code hash could make an
// account now. It reads as PreviewInvite does (ErrInvitesPaused,
// ErrInviteInvalid) and then refuses an invite that admits guests only with
// ErrJoiningClosed, so an invalid code is never told apart by its mode. The
// redeeming transaction checks all of it again.
func (s *Store) CheckInviteForAccount(ctx context.Context, codeHash string, now time.Time) error {
	if paused, err := s.InvitesPaused(ctx); err != nil {
		return err
	} else if paused {
		return ErrInvitesPaused
	}
	r, _, _, err := usableInviteTx(ctx, s.db, codeHash, now)
	if err != nil {
		return err
	}
	return inviteAdmitsAccountTx(ctx, s.db, r.id)
}

// redeemInvite is RedeemInvite, storing record as the new account's
// password when it is not empty.
func (s *Store) redeemInvite(ctx context.Context, codeHash, name, record string, now time.Time) (RedeemedInvite, error) {
	now = now.Truncate(time.Millisecond)
	if err := (schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: name}).Validate(); err != nil {
		return RedeemedInvite{}, fmt.Errorf("%w: %w", ErrInviteRequestInvalid, err)
	}
	token, err := newCredentialToken()
	if err != nil {
		return RedeemedInvite{}, err
	}
	var out RedeemedInvite
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out = RedeemedInvite{}
		if paused, err := invitesPausedTx(ctx, tx); err != nil {
			return err
		} else if paused {
			return ErrInvitesPaused
		}
		r, kept, dropped, err := usableInviteTx(ctx, tx, codeHash, now)
		if err != nil {
			return err
		}
		// An invite for guests only makes no account, whichever caller asks:
		// a guest invite must not mint a member through it (#710). POST
		// /v1/invites/redeem no longer reaches here (#713). Said only
		// once the code proved good, so the mode is told only to its holder.
		if !admitsAccount(r.joinAs) {
			return ErrJoiningClosed
		}
		actor := inviteSubject(r.id)

		// The guarded update, not the read above, decides who gets the use.
		res, err := tx.ExecContext(ctx,
			`UPDATE invites SET uses = uses + 1
			 WHERE id = ? AND closed_at IS NULL AND (max_uses IS NULL OR uses < max_uses)`, r.id)
		if err != nil {
			return fmt.Errorf("store: redeem invite: spend use: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: redeem invite: spend use: %w", err)
		} else if n != 1 {
			return ErrInviteInvalid
		}

		voice := r.voice && r.creatorVoice
		// Only a holder of manage_members grants the right to invite (issue
		// #457, ADR-014 design §7.3). Checked again here, against the maker as
		// they are now (usableInviteTx loaded r.maker in this transaction), so
		// the rule does not rest on what an invite row says.
		mayInvite := r.mayInvite && r.maker.members
		// A name that clashes with another by case or look-alike
		// characters is taken (issue #441).
		keys, err := s.claimNameTx(ctx, tx, name, 0)
		if err != nil {
			return fmt.Errorf("store: redeem invite: %w", err)
		}
		res, err = tx.ExecContext(ctx,
			`INSERT INTO principals (kind, name, name_key, name_skel, role, created_at, invited_by, invite_id, may_invite, voice_allowed)
			 VALUES ('human', ?, ?, ?, 'member', ?, ?, ?, ?, ?)`,
			name, keys.Folded, keys.Skeletal, now.UnixMilli(), r.createdBy, r.id, boolInt(mayInvite), boolInt(voice))
		if isUniqueConstraintErr(err) {
			return fmt.Errorf("store: redeem invite: create principal %q: %w", name, ErrDuplicate)
		}
		if err != nil {
			return fmt.Errorf("store: redeem invite: create principal: %w", err)
		}
		pid, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: redeem invite: principal id: %w", err)
		}
		out.Principal = Principal{
			ID: pid, Kind: PrincipalHuman, Name: name, Role: RoleMember, CreatedAt: now,
			InvitedBy: r.createdBy, InviteID: r.id, MayInvite: mayInvite, VoiceAllowed: voice,
		}

		for _, c := range kept {
			// A member who joins through an invite never gets the right to
			// decide the channel's approvals (issue #450): the column's
			// default of 1 is for members an operator adds.
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide) VALUES (?, ?, ?, ?, 0)",
				c.ID, pid, r.createdBy, now.UnixMilli()); err != nil {
				return fmt.Errorf("store: redeem invite: add member: %w", err)
			}
		}

		// The joiner's credential renews by being used (issue #439): its
		// expiry moves to the invite's lifetime past each day's first use.
		lifetime := time.Duration(r.lifetimeMs) * time.Millisecond
		exp := now.Add(lifetime)
		cred, err := insertCredentialTx(ctx, tx, pid, fmt.Sprintf("invite %d", r.id), token, now, &exp, lifetime)
		if err != nil {
			return err
		}
		out.Credential = cred

		droppedIDs := make([]string, len(dropped))
		for i, c := range dropped {
			droppedIDs[i] = strconv.FormatInt(c.ID, 10)
			out.DroppedChannels = append(out.DroppedChannels, c.ID)
		}
		droppedText := "none"
		if len(droppedIDs) > 0 {
			droppedText = strings.Join(droppedIDs, ",")
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditInviteRedeemed, inviteSubject(r.id),
			fmt.Sprintf("invite=%d principal=%d uses=%d dropped_channels=%s", r.id, pid, r.uses+1, droppedText), now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditPrincipalCreated, principalActor(pid),
			fmt.Sprintf("kind=human invite=%d inviter=%d", r.id, r.createdBy), now); err != nil {
			return err
		}
		if record != "" {
			if _, err := setPasswordTx(ctx, tx, pid, record, now); err != nil {
				return err
			}
			if err := appendAuditEventTx(ctx, tx, actor, AuditPasswordSet, principalActor(pid),
				fmt.Sprintf("via=invite invite=%d", r.id), now); err != nil {
				return err
			}
		}
		for _, c := range kept {
			if err := appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(c.ID), memberDetail(pid), now); err != nil {
				return err
			}
		}
		return appendAuditEventTx(ctx, tx, actor, "credential_created", principalActor(pid),
			credentialDetail(cred.ID, cred.Label), now)
	})
	if err != nil {
		return RedeemedInvite{}, err
	}
	out.Token = token
	return out, nil
}

// CloseInvite closes an invite so it can no longer be used; uses already made
// stand. closerID must be the invite's maker or hold manage_members, both
// enabled;
// anyone else gets ErrNotFound, as for an id that does not exist, so ids are
// not probed. Closing a closed invite succeeds, changes nothing and writes no
// audit row (changed is false). invite_closed is audited in the same
// transaction.
func (s *Store) CloseInvite(ctx context.Context, actor string, closerID, inviteID int64, now time.Time) (changed bool, err error) {
	now = now.Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		r, err := scanInvite(tx.QueryRowContext(ctx,
			`SELECT `+inviteColumns+` FROM invites i JOIN principals p ON p.id = i.created_by WHERE i.id = ?`, inviteID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("store: close invite %d: %w", inviteID, ErrNotFound)
		case err != nil:
			return fmt.Errorf("store: close invite: find: %w", err)
		}
		_, _, disabled, err := principalState(ctx, tx, closerID)
		switch {
		case errors.Is(err, ErrPrincipalNotFound):
			return fmt.Errorf("store: close invite %d: %w", inviteID, ErrNotFound)
		case err != nil:
			return err
		}
		if disabled {
			return fmt.Errorf("store: close invite %d: %w", inviteID, ErrNotFound)
		}
		// Someone else's invite needs manage_members (ADR-014 design §7.3).
		if closerID != r.createdBy {
			ok, err := permittedTx(ctx, tx, closerID, schema.PermManageMembers)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("store: close invite %d: %w", inviteID, ErrNotFound)
			}
		}
		if r.closedAt.Valid {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE invites SET closed_at = ?, closed_by = ? WHERE id = ? AND closed_at IS NULL",
			now.UnixMilli(), closerID, inviteID); err != nil {
			return fmt.Errorf("store: close invite: %w", err)
		}
		changed = true
		reason := InviteCloseCreator
		if closerID != r.createdBy {
			reason = InviteCloseOperator
		}
		return appendAuditEventTx(ctx, tx, actor, AuditInviteClosed, inviteSubject(inviteID),
			inviteClosedDetail(inviteID, r.kind, r.uses, reason), now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func inviteClosedDetail(id int64, kind schema.InviteKind, uses int64, reason string) string {
	return fmt.Sprintf("invite=%d kind=%s uses=%d reason=%s", id, kind, uses, reason)
}

// closeCreatorInvitesTx closes every open invite creatorID made, of kind (or
// of every kind when kind is empty), writing one invite_closed row each with
// reason. Open means usable at now: not closed, not used up and not expired;
// the others need no closing. closed_by is left empty, since no principal
// closed them by hand. The caller holds the write lock.
func closeCreatorInvitesTx(ctx context.Context, tx execer, actor string, creatorID int64, kind schema.InviteKind, reason string, now time.Time) error {
	q := `SELECT id, kind, uses FROM invites
		 WHERE created_by = ? AND closed_at IS NULL AND expires_at > ? AND (max_uses IS NULL OR uses < max_uses)`
	args := []any{creatorID, now.UnixMilli()}
	if kind != "" {
		q += " AND kind = ?"
		args = append(args, string(kind))
	}
	rows, err := tx.QueryContext(ctx, q+" ORDER BY id", args...)
	if err != nil {
		return fmt.Errorf("store: list open invites: %w", err)
	}
	type open struct {
		id   int64
		kind schema.InviteKind
		uses int64
	}
	var found []open
	for rows.Next() {
		var o open
		var k string
		if err := rows.Scan(&o.id, &k, &o.uses); err != nil {
			_ = rows.Close()
			return fmt.Errorf("store: list open invites: scan: %w", err)
		}
		o.kind = schema.InviteKind(k)
		found = append(found, o)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("store: list open invites: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: list open invites: %w", err)
	}
	for _, o := range found {
		if _, err := tx.ExecContext(ctx,
			"UPDATE invites SET closed_at = ? WHERE id = ? AND closed_at IS NULL", now.UnixMilli(), o.id); err != nil {
			return fmt.Errorf("store: close invite %d: %w", o.id, err)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditInviteClosed, inviteSubject(o.id),
			inviteClosedDetail(o.id, o.kind, o.uses, reason), now); err != nil {
			return err
		}
	}
	return nil
}

// ListInvites returns the invites matching f, newest first (ties by id
// descending). The result is never nil.
func (s *Store) ListInvites(ctx context.Context, f InviteFilter, now time.Time) ([]schema.InviteV1, error) {
	q := `SELECT ` + inviteColumns + ` FROM invites i JOIN principals p ON p.id = i.created_by WHERE 1 = 1`
	var args []any
	if f.CreatedBy != 0 {
		q += " AND i.created_by = ?"
		args = append(args, f.CreatedBy)
	}
	if f.Kind != "" {
		q += " AND i.kind = ?"
		args = append(args, string(f.Kind))
	}
	q += " ORDER BY i.created_at DESC, i.id DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list invites: %w", err)
	}
	var found []inviteRow
	for rows.Next() {
		r, err := scanInvite(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: list invites: scan: %w", err)
		}
		if f.State == "" || r.state(now) == f.State {
			found = append(found, r)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("store: list invites: %w", err)
	}
	_ = rows.Close()
	out := []schema.InviteV1{}
	for _, r := range found {
		channels, err := inviteChannelsTx(ctx, s.db, r.id)
		if err != nil {
			return nil, err
		}
		out = append(out, r.wire(channels))
	}
	return out, nil
}

// InviteJoiners returns the principals that came through invite inviteID, by
// id. The result is never nil. An unknown invite is ErrNotFound.
func (s *Store) InviteJoiners(ctx context.Context, inviteID int64) ([]Principal, error) {
	var one int
	switch err := s.db.QueryRowContext(ctx, "SELECT 1 FROM invites WHERE id = ?", inviteID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("store: invite %d: %w", inviteID, ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: invite joiners: %w", err)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM principals WHERE invite_id = ? ORDER BY id`, inviteID)
	if err != nil {
		return nil, fmt.Errorf("store: invite joiners: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: invite joiners: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("store: invite joiners: %w", err)
	}
	_ = rows.Close()
	out := []Principal{}
	for _, id := range ids {
		p, err := s.PrincipalByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Invitees returns the principals rootID invited, by following
// principals.invited_by down from it, in the order the walk meets them (by
// generation, then by id). depth 1 is the people rootID invited themselves;
// depth 0 or less is everyone under them, however deep. rootID itself is never
// in the result. The result is never nil. An unknown rootID is
// ErrPrincipalNotFound.
//
// No cycle should exist, since an invite always makes a new principal; the
// walk still keeps a visited set, so a cycle written some other way ends it
// rather than looping, and no principal is returned twice.
func (s *Store) Invitees(ctx context.Context, rootID int64, depth int) ([]Principal, error) {
	var one int
	switch err := s.db.QueryRowContext(ctx, "SELECT 1 FROM principals WHERE id = ?", rootID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrPrincipalNotFound
	case err != nil:
		return nil, fmt.Errorf("store: invitees: find root: %w", err)
	}
	visited := map[int64]bool{rootID: true}
	var ids []int64
	frontier := []int64{rootID}
	for level := 1; len(frontier) > 0 && (depth <= 0 || level <= depth); level++ {
		var next []int64
		for _, parent := range frontier {
			children, err := s.invitedBy(ctx, parent)
			if err != nil {
				return nil, err
			}
			for _, id := range children {
				if visited[id] {
					continue
				}
				visited[id] = true
				next = append(next, id)
			}
		}
		ids = append(ids, next...)
		frontier = next
	}
	out := []Principal{}
	for _, id := range ids {
		p, err := s.PrincipalByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// invitedBy returns the ids of the principals whose invited_by is parent.
func (s *Store) invitedBy(ctx context.Context, parent int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM principals WHERE invited_by = ? ORDER BY id", parent)
	if err != nil {
		return nil, fmt.Errorf("store: invitees: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: invitees: scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: invitees: %w", err)
	}
	return ids, nil
}

// InvitesPaused reports whether an operator has paused invites.
func (s *Store) InvitesPaused(ctx context.Context) (bool, error) {
	return invitesPausedTx(ctx, s.db)
}

// SetInvitesPaused pauses or resumes invites. While paused, no invite can be
// made, previewed or redeemed; accounts already made are untouched. It audits
// invites_paused or invites_resumed in the same transaction, and does nothing
// (changed is false, no audit row) when the switch already has that value.
func (s *Store) SetInvitesPaused(ctx context.Context, actor string, paused bool) (changed bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		cur, err := invitesPausedTx(ctx, tx)
		if err != nil {
			return err
		}
		if cur == paused {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO server_settings (key, value, updated_at) VALUES (?, ?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			settingInvitesPaused, strconv.FormatBool(paused), now.UnixMilli()); err != nil {
			return fmt.Errorf("store: set invites_paused: %w", err)
		}
		changed = true
		action := AuditInvitesResumed
		if paused {
			action = AuditInvitesPaused
		}
		return appendAuditEventTx(ctx, tx, actor, action, "server", "", now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// SetMayInvite grants or takes away a human's right to make personal invites.
// Operators can always invite whatever it says. An agent cannot hold it
// (ErrInviteNotAllowed). It audits invite_permission_changed in the same
// transaction and does nothing when the value already stands. Taking the right
// away from someone who is not an operator also closes the personal invites
// they left open, in the same transaction, each audited as invite_closed with
// reason=permission_revoked (issue #438): giving the right back does not
// revive them. It returns ErrPrincipalNotFound for an unknown id.
func (s *Store) SetMayInvite(ctx context.Context, actor string, principalID int64, may bool, forbidOwner ...bool) (changed bool, err error) {
	now := time.Now().Truncate(time.Millisecond)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		changed = false
		if err := ownerProtectedTx(ctx, tx, principalID, len(forbidOwner) > 0 && forbidOwner[0]); err != nil {
			return err
		}
		var (
			kind string
			cur  bool
		)
		switch err := tx.QueryRowContext(ctx, "SELECT kind, may_invite FROM principals WHERE id = ?", principalID).Scan(&kind, &cur); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrPrincipalNotFound
		case err != nil:
			return fmt.Errorf("store: set may_invite: find principal: %w", err)
		}
		if PrincipalKind(kind) != PrincipalHuman {
			return fmt.Errorf("%w: only a person may invite", ErrInviteNotAllowed)
		}
		// A guest holds no durable right while it is a guest, not even one an
		// operator grants (issue #706, design §4.3). Clearing is allowed: it
		// grants nothing.
		if may {
			var guest int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM principals p WHERE p.id = ? AND `+activeGuestSQL, principalID).Scan(&guest); err != nil {
				return fmt.Errorf("store: set may_invite: read guest state: %w", err)
			}
			if guest > 0 {
				return ErrGuestNotAllowed
			}
		}
		if cur == may {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE principals SET may_invite = ? WHERE id = ?", boolInt(may), principalID); err != nil {
			return fmt.Errorf("store: set may_invite: %w", err)
		}
		changed = true
		if err := appendAuditEventTx(ctx, tx, actor, AuditInvitePermissionChanged, principalActor(principalID),
			fmt.Sprintf("may_invite=%t", may), now); err != nil {
			return err
		}
		if may {
			return nil
		}
		// The flag is a brake on make_member_links for members only (ADR-014
		// design §5): an operator's or an owner's invites stand. Whether the
		// brake binds is asked alone, not whether the principal holds the
		// permission now: a permission the document switched off suspends
		// invites (reversible) and must not be taken for the brake, which
		// closes them for good.
		binds, err := mayInviteBrakeBindsTx(ctx, tx, principalID)
		if err != nil || !binds {
			return err
		}
		return closeCreatorInvitesTx(ctx, tx, actor, principalID, schema.InviteKindPersonal, InviteClosePermissionRevoked, now)
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}
