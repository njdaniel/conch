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

// Member links, the invite family's second version (ADR-013 decision 5,
// docs/design/community-model.md §2.1 and §3.3, issue #985). A member link is
// stored with the published kind 'open' and invites.link = 'member'; it has
// no channel rows unless its maker named landing channels (O5), no voice and
// no right to invite. This file makes, lists, previews and reads the joiners
// of them. It writes no membership: redemption is I6b, and until then the
// redemption transaction of invites.go reads a member link through the
// link-keyed rules there.
//
// Like the rest of the invite store, no code, hash or token appears in an
// audit row, a log line or an error string.

// inviteVisibleV2 is the condition (on the alias i) of the invites a v2 read
// answers: every member link, and every legacy invite that admits accounts. A
// legacy invite that admitted guests was closed by migration 40 and is shown
// by the v1 routes only; a link of kind 'guest', which nothing writes, is
// shown by none of the v2 routes (guests are #1004's).
const inviteVisibleV2 = `(i.link = 'member' OR (i.link = 'legacy' AND i.join_as = 'account'))`

// inviteExpiresAtV2 is the wire's expires_at for a stored expiry: absent at or
// past the NoExpiryMillis sentinel.
func inviteExpiresAtV2(ms int64) *schema.Timestamp {
	if ms >= schema.NoExpiryMillis {
		return nil
	}
	ts := schema.NewTimestamp(time.UnixMilli(ms))
	return &ts
}

// wireV2 is the invite as InviteV2 says it. landing is the rows of
// invite_channels: the channels a legacy invite still grants (Channels), or
// the landing channels of a member link (LandingChannels).
func (r inviteRow) wireV2(landing []schema.InviteChannelV1, joiners int64) schema.InviteV2 {
	inv := schema.InviteV2{
		ID:                        r.id,
		Kind:                      schema.InviteKindMember,
		CreatedBy:                 r.createdBy,
		CreatedByName:             r.creatorName,
		CreatedAt:                 schema.NewTimestamp(time.UnixMilli(r.createdAt)),
		ExpiresAt:                 inviteExpiresAtV2(r.expiresAt),
		Uses:                      r.uses,
		Joiners:                   joiners,
		ClosedAt:                  msToTimestampPtr(r.closedAt),
		CredentialLifetimeSeconds: r.lifetimeMs / 1000,
		Legacy:                    r.link == linkLegacy,
	}
	if r.maxUses.Valid {
		n := r.maxUses.Int64
		inv.MaxUses = &n
	}
	if len(landing) > 0 {
		if r.link == linkMember {
			inv.LandingChannels = landing
		} else {
			inv.Channels = landing
		}
	}
	return inv
}

// inviteJoinersTx is the one read of the people who came through invite
// inviteID, by id: members only. A visit of a legacy guest invite is not one
// (GET /v1/guests?invite_id= lists those, for operators), and neither is an
// account that was deleted. A disabled member is, so a maker and an operator
// still see who they brought. The joiner count of an InviteV2 is its length,
// so the count and the list cannot disagree, and the visibility rule of the
// member-list setting (§13, I14a) has one place to go.
func inviteJoinersTx(ctx context.Context, tx execer, inviteID int64) ([]schema.InviteJoinerV1, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT p.id, p.name, p.created_at, p.disabled_at IS NOT NULL FROM principals p
		 WHERE p.invite_id = ? AND p.kind = 'human' AND p.hidden_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM guest_visits g WHERE g.principal_id = p.id AND g.state IN ('active', 'ended'))
		 ORDER BY p.id`, inviteID)
	if err != nil {
		return nil, fmt.Errorf("store: invite joiners: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []schema.InviteJoinerV1{}
	for rows.Next() {
		var j schema.InviteJoinerV1
		var created int64
		if err := rows.Scan(&j.ID, &j.Name, &created, &j.Disabled); err != nil {
			return nil, fmt.Errorf("store: invite joiners: scan: %w", err)
		}
		j.CreatedAt = schema.NewTimestamp(time.UnixMilli(created))
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: invite joiners: %w", err)
	}
	return out, nil
}

// memberLinkDefaultExpiryTx reads the member_link_default_expiry_seconds
// setting: how long a link that names no expiry lasts. An absent row is the
// default of 7 days, and one that cannot be read, or is out of its bounds,
// reads as the default too (as joinSettingsTx reads it). 0 is no expiry.
func memberLinkDefaultExpiryTx(ctx context.Context, tx execer) (int64, error) {
	var v string
	switch err := tx.QueryRowContext(ctx,
		"SELECT value FROM server_settings WHERE key = ?", settingMemberLinkDefaultExpirySeconds).Scan(&v); {
	case errors.Is(err, sql.ErrNoRows):
		return schema.DefaultMemberLinkExpirySeconds, nil
	case err != nil:
		return 0, fmt.Errorf("store: read member link default expiry: %w", err)
	}
	return parseMemberLinkExpiry(v), nil
}

// parseMemberLinkExpiry reads the stored setting: a number of seconds within
// its bounds, else the default (a value that cannot be read must not stretch
// a link's life).
func parseMemberLinkExpiry(v string) int64 {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > schema.MaxMemberLinkExpirySeconds {
		return schema.DefaultMemberLinkExpirySeconds
	}
	return n
}

// CreateMemberLink makes a member link for creatorID and returns it with its
// code, which is shown only here. It validates req, applies the defaults for
// what it leaves out, and checks in one IMMEDIATE transaction with the insert
// that:
//
//   - invites are not paused (ErrInvitesPaused);
//   - the creator is an enabled human holding make_member_links now: a member
//     with the may_invite brake on, a restricted operator, an owner. An
//     agent, a guest (ended too), a disabled or a hidden person, and a member
//     the brake binds are refused alike (ErrInviteNotAllowed);
//   - never_expires, the one way to a link with no expiry, needs
//     manage_members (ErrInviteNotAllowed);
//   - landing channels need manage_channels. Without it any landing channel
//     is ErrInviteGrantExceedsInviter ("a channel you are not in"), decided
//     before any channel is looked up, so a name is answered the same
//     whether the channel is private, public or does not exist and the
//     request cannot probe for private names; with it, a name that is no
//     channel is ErrNotFound;
//   - without manage_members, the creator has fewer open member links than
//     the allowance (ErrInviteAllowanceExceeded; an uncapped link counts as
//     open).
//
// Each refusal is a *PermissionError wrapping its sentinel (errors.Is still
// matches), naming the permission it lacks (ADR-014 design §7.4); the handler
// tells only a caller whose role could hold it.
//
// invite_created is audited in the same transaction. The code is not.
func (s *Store) CreateMemberLink(ctx context.Context, actor string, creatorID int64, req schema.CreateInviteRequestV2, opts InviteOptions, now time.Time) (schema.CreateInviteResponseV2, error) {
	now = now.Truncate(time.Millisecond)
	if err := req.Validate(now); err != nil {
		return schema.CreateInviteResponseV2{}, fmt.Errorf("%w: %w", ErrInviteRequestInvalid, err)
	}
	allowance := opts.allowance()
	lifetimeMs := req.EffectiveCredentialLifetime().Milliseconds()
	code, err := NewInviteCode()
	if err != nil {
		return schema.CreateInviteResponseV2{}, err
	}

	var out schema.InviteV2
	err = s.withImmediateTx(ctx, func(tx execer) error {
		if paused, err := invitesPausedTx(ctx, tx); err != nil {
			return err
		} else if paused {
			return ErrInvitesPaused
		}
		creator, found, err := queryPrincipal(ctx, tx, creatorID)
		switch {
		case err != nil:
			return fmt.Errorf("store: create member link: find creator: %w", err)
		case !found:
			return ErrPrincipalNotFound
		}
		// A human, or an agent an owner granted make_member_links (issue
		// #1326): what it may make is what it holds, below.
		if creator.DisabledAt != nil || (creator.Kind != PrincipalHuman && creator.Kind != PrincipalAgent) {
			return ErrInviteNotAllowed
		}
		// What the maker may do is what it holds now, by permission (ADR-014
		// design §7.3), never its role. make_member_links already folds in
		// the may_invite brake for a member and refuses a guest.
		rights, err := makerRightsTx(ctx, tx, creatorID)
		if err != nil {
			return fmt.Errorf("store: create member link: %w", err)
		}
		if !rights.links {
			return &PermissionError{Err: ErrInviteNotAllowed, Permission: schema.PermMakeMemberLinks, Reason: "you may not make member links"}
		}
		if req.NeverExpires && !rights.members {
			return &PermissionError{Err: ErrInviteNotAllowed, Permission: schema.PermManageMembers, Reason: "only an operator makes a link that never expires"}
		}
		// Landing channels: decided on the permission alone, before any
		// lookup whose answer could differ between a private channel and one
		// that does not exist (security review of #978).
		if len(req.LandingChannels) > 0 && !rights.channels {
			return &PermissionError{Err: ErrInviteGrantExceedsInviter, Permission: schema.PermManageChannels, Reason: "a channel you are not in"}
		}
		channelIDs := make([]int64, 0, len(req.LandingChannels))
		for _, name := range req.LandingChannels {
			var id int64
			switch err := tx.QueryRowContext(ctx, "SELECT id FROM channels WHERE name = ?", name).Scan(&id); {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("store: create member link: channel %q: %w", name, ErrNotFound)
			case err != nil:
				return fmt.Errorf("store: create member link: find channel: %w", err)
			}
			// The archived guard of a link naming the channel (issue #844);
			// landing channels are named only by a holder of manage_channels.
			if err := channelNotArchivedTx(ctx, tx, id); err != nil {
				return err
			}
			channelIDs = append(channelIDs, id)
		}
		// The allowance binds anyone without manage_members, a restricted
		// operator included (ADR-014 design §7.3, N1).
		if !rights.members {
			outstanding, err := outstandingMemberLinksTx(ctx, tx, creatorID, now)
			if err != nil {
				return fmt.Errorf("store: create member link: %w", err)
			}
			if outstanding >= allowance {
				return ErrInviteAllowanceExceeded
			}
		}

		expires := schema.NoExpiryMillis
		switch {
		case req.NeverExpires:
		case req.ExpiresAt != nil:
			expires = req.ExpiresAt.Time().Truncate(time.Millisecond).UnixMilli()
		default:
			secs, err := memberLinkDefaultExpiryTx(ctx, tx)
			if err != nil {
				return err
			}
			if secs > 0 {
				expires = now.Add(time.Duration(secs) * time.Second).UnixMilli()
			}
		}
		var maxUses any
		if req.MaxUses != nil {
			maxUses = *req.MaxUses
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO invites (kind, code_hash, created_by, created_at, expires_at, max_uses, voice, may_invite, credential_lifetime_ms, join_as, link)
			 VALUES ('open', ?, ?, ?, ?, ?, 0, 0, ?, 'account', 'member')`,
			HashInviteCode(code), creatorID, now.UnixMilli(), expires, maxUses, lifetimeMs)
		if err != nil {
			return fmt.Errorf("store: create member link: insert: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create member link: id: %w", err)
		}
		for _, cid := range channelIDs {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO invite_channels (invite_id, channel_id) VALUES (?, ?)", id, cid); err != nil {
				return fmt.Errorf("store: create member link: insert channel: %w", err)
			}
		}

		r, err := scanInvite(tx.QueryRowContext(ctx,
			`SELECT `+inviteColumns+` FROM invites i JOIN principals p ON p.id = i.created_by WHERE i.id = ?`, id))
		if err != nil {
			return fmt.Errorf("store: create member link: read back: %w", err)
		}
		landing, err := inviteChannelsTx(ctx, tx, id)
		if err != nil {
			return err
		}
		out = r.wireV2(landing, 0)

		ids := make([]string, len(landing))
		for i, c := range landing {
			ids[i] = strconv.FormatInt(c.ID, 10)
		}
		uses, expiry := "none", "none"
		if out.MaxUses != nil {
			uses = strconv.FormatInt(*out.MaxUses, 10)
		}
		if out.ExpiresAt != nil {
			expiry = out.ExpiresAt.Time().Format(time.RFC3339)
		}
		detail := fmt.Sprintf("invite=%d kind=%s link=%s channels=%s expires=%s max_uses=%s",
			id, schema.InviteKindOpen, linkMember, joinIDsOrNone(ids), expiry, uses)
		return appendAuditEventTx(ctx, tx, actor, AuditInviteCreated, inviteSubject(id), detail, now)
	})
	if err != nil {
		return schema.CreateInviteResponseV2{}, err
	}
	return schema.CreateInviteResponseV2{Invite: out, Code: code}, nil
}

// joinIDsOrNone is the audit text of a list of ids: "none" when empty.
func joinIDsOrNone(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	return strings.Join(ids, ",")
}

// ListMemberLinks returns the member links, and the legacy invites that admit
// accounts, that createdBy made (every maker's when createdBy is 0), newest
// first (ties by id descending), as InviteV2. A legacy invite that admitted
// guests is left out (inviteVisibleV2). The result is never nil. The caller
// decides whose links the reader may see.
func (s *Store) ListMemberLinks(ctx context.Context, createdBy int64) ([]schema.InviteV2, error) {
	q := `SELECT ` + inviteColumns + ` FROM invites i JOIN principals p ON p.id = i.created_by WHERE ` + inviteVisibleV2
	var args []any
	if createdBy != 0 {
		q += " AND i.created_by = ?"
		args = append(args, createdBy)
	}
	q += " ORDER BY i.created_at DESC, i.id DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list member links: %w", err)
	}
	var found []inviteRow
	for rows.Next() {
		r, err := scanInvite(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: list member links: scan: %w", err)
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("store: list member links: %w", err)
	}
	_ = rows.Close()
	out := []schema.InviteV2{}
	for _, r := range found {
		landing, err := inviteChannelsTx(ctx, s.db, r.id)
		if err != nil {
			return nil, err
		}
		joiners, err := inviteJoinersTx(ctx, s.db, r.id)
		if err != nil {
			return nil, err
		}
		out = append(out, r.wireV2(landing, int64(len(joiners))))
	}
	return out, nil
}

// MemberLinkJoiners returns the members who came through invite inviteID, by
// id, to callerID: its maker, or a holder of manage_members. Anyone else
// (another member, a guest, an agent, a disabled or unknown caller) gets
// ErrNotFound, as for an id that does not exist or one a v2 read does not
// show, so ids are not probed. The result is never nil.
func (s *Store) MemberLinkJoiners(ctx context.Context, callerID, inviteID int64) ([]schema.InviteJoinerV1, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("store: member link joiners: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var maker int64
	switch err := tx.QueryRowContext(ctx,
		`SELECT i.created_by FROM invites i WHERE i.id = ? AND `+inviteVisibleV2, inviteID).Scan(&maker); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("store: invite %d: %w", inviteID, ErrNotFound)
	case err != nil:
		return nil, fmt.Errorf("store: member link joiners: find: %w", err)
	}
	// An operator by permission, read now; the maker as a member, which an
	// agent, a guest and a disabled person are not.
	allowed, err := permittedTx(ctx, tx, callerID, schema.PermManageMembers)
	if err != nil {
		return nil, err
	}
	if !allowed && callerID == maker {
		if allowed, err = isMemberTx(ctx, tx, callerID); err != nil {
			return nil, err
		}
	}
	if !allowed {
		return nil, fmt.Errorf("store: invite %d: %w", inviteID, ErrNotFound)
	}
	return inviteJoinersTx(ctx, tx, inviteID)
}

// PreviewMemberLink says what the invite with this code hash would offer
// without using it, as InvitePreviewV2: who made it, the channels a joiner
// would land in as they are now (a member link's default channels, or its
// landing channels; for a legacy invite, the channels it grants), when it
// expires, and what the server's switches offer. ServerName is the caller's
// to fill: the store does not hold the label. It returns ErrInvitesPaused
// while invites are paused (checked before the code) and ErrInviteInvalid for
// a code that is unknown, expired, used up, closed, a legacy invite that
// admits guests, or whose maker may no longer stand behind it. A landing
// channel's name is shown to whoever holds the code even when the channel is
// private: by design, since its maker put it on the link. It writes nothing.
func (s *Store) PreviewMemberLink(ctx context.Context, codeHash string, now time.Time) (schema.InvitePreviewV2, error) {
	if paused, err := s.InvitesPaused(ctx); err != nil {
		return schema.InvitePreviewV2{}, err
	} else if paused {
		return schema.InvitePreviewV2{}, ErrInvitesPaused
	}
	r, kept, _, err := usableInviteTx(ctx, s.db, codeHash, now)
	if err != nil {
		return schema.InvitePreviewV2{}, err
	}
	// v2 shows no invite that admits guests, which v1 may still preview.
	if r.link != linkMember && r.joinAs != schema.InviteJoinAccount {
		return schema.InvitePreviewV2{}, ErrInviteInvalid
	}
	settings, err := joinSettingsTx(ctx, s.db)
	if err != nil {
		return schema.InvitePreviewV2{}, err
	}
	live, err := liveGuestsTx(ctx, s.db, credentialNow())
	if err != nil {
		return schema.InvitePreviewV2{}, err
	}
	names := make([]string, len(kept))
	for i, c := range kept {
		names[i] = c.Name
	}
	return schema.InvitePreviewV2{
		Kind:      schema.InviteKindMember,
		Inviter:   r.creatorName,
		Channels:  names,
		ExpiresAt: inviteExpiresAtV2(r.expiresAt),
		Join:      inviteJoinOptions(r.joinAs, settings, live),
	}, nil
}
