package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/njdaniel/conch/internal/server/names"
	"github.com/njdaniel/conch/pkg/schema"
)

// Temporary guests (issue #705, ADR-009, docs/design/guests-and-accounts.md
// §4). A guest is a human member principal with a guest_visits row, whose
// name is a handle the store makes ("guest-" and 80 random bits), with
// exactly one credential and the memberships of its visit, each with a
// history bound. Its lifecycle is active → ended or active → converted, and
// never back: every transition is a guarded update in an IMMEDIATE
// transaction, so of an End visit, an expiry, a revocation and a conversion
// racing on one visit exactly one wins, and the migration's trigger refuses
// any update of a visit that is no longer active.
//
// A visit's deadline is the earlier of its hard expiry (expires_at, fixed at
// admission, the earlier of the absolute bound and the entry invite's
// credential lifetime) and its idle expiry: its credential's last REST use,
// or its creation, plus the idle bound. ResolveCredentialDetail and
// CredentialLive refuse a visit past either, before any sweep has ended it,
// so authorization never waits for cleanup; SweepGuests then ends it.
//
// Every time here is the credential clock (credentialNow), the one that
// resolves credentials, so a deadline and its refusal agree.
//
// Nothing here makes a route reachable: the entry routes and their policy
// are issue #710's, conversion's route #711's.

// guestMigration is the number of the migration that adds guest visits: its
// Go step is reportReservedGuestNames.
const guestMigration = 31

// Audit actions written by guest visits (design §7.3). None carries a client
// address, a token or a code.
const (
	// AuditGuestStarted records a visit's start: actor whoever admitted it,
	// subject the guest, detail origin=address, or origin=invite invite=<id>.
	AuditGuestStarted = "guest_started"
	// AuditGuestEnded records a visit's end: subject the guest, detail
	// reason=<schema.GuestEndReason>.
	AuditGuestEnded = "guest_ended"
	// AuditGuestConverted records that a guest created an account: subject
	// the principal, detail the handle it gave up and the name it took,
	// quoted. No password or credential material.
	AuditGuestConverted = "guest_converted"
)

// GuestCredentialLabel is the label of a guest's one credential.
const GuestCredentialLabel = "guest visit"

// guestActivityStep is how often a guest's REST requests move its idle
// expiry: at most one write a minute per credential (design §4.2), so the
// bound is accurate to a minute and costs no write per request.
var guestActivityStep = time.Minute

// guestHandleTries bounds the handles one CreateGuest draws. 80 random bits
// make a second draw all but impossible; the bound only stops a broken
// random source looping for ever.
const guestHandleTries = 8

// Errors of the guest store.
var (
	// ErrNotAGuest: the principal has no guest visit, or is not a guest now.
	ErrNotAGuest = errors.New("store: not a guest")
	// ErrGuestEnded: the visit is already ended or converted; nothing
	// revives or converts it.
	ErrGuestEnded = errors.New("store: guest visit ended")
	// ErrGuestNotAllowed: the action is not for a guest: a credential, a
	// link, a rotation, or a durable right such as can_decide.
	ErrGuestNotAllowed = errors.New("store: not allowed for a guest")
	// ErrGuestNotHideable: a guest is never renamed or hidden: its name is
	// its handle and its posts' attribution is immutable (review F10).
	ErrGuestNotHideable = errors.New("store: a guest is not renamed or hidden")
	// ErrGuestsFull: the cap on live visits is reached.
	ErrGuestsFull = errors.New("store: too many live guest visits")
	// ErrGuestLabelInvalid: the display label is refused (design §4.1). The
	// error never carries the label.
	ErrGuestLabelInvalid = errors.New("store: guest label not acceptable")
)

// GuestVisit is one visit as the store holds it.
type GuestVisit struct {
	PrincipalID int64
	Handle      string
	Label       string
	Origin      schema.GuestOrigin
	// InviteID is the entry invite of an invite-origin visit, else 0.
	InviteID  int64
	State     schema.GuestState
	StartedAt time.Time
	// ExpiresAt is the hard deadline; IdleExpiresAt where the idle bound
	// stands now, never after ExpiresAt.
	ExpiresAt     time.Time
	IdleExpiresAt time.Time
	// LastActiveAt is the last REST use of the visit's credential that moved
	// the idle bound, or the visit's start.
	LastActiveAt time.Time
	EndedAt      *time.Time
	EndReason    schema.GuestEndReason
	ConvertedAt  *time.Time
}

// Attribution is how the guest is rendered.
func (v GuestVisit) Attribution() schema.GuestAttributionV1 {
	return guestAttribution(v.Handle, v.Label)
}

// V1 is the visit as its guest sees it. CanConvert is false: no route
// converts a guest yet (#711 decides it from the origin's policy).
func (v GuestVisit) V1() schema.GuestV1 {
	out := schema.GuestV1{
		State: v.State, Origin: v.Origin, Handle: v.Handle, Label: v.Label, Tag: schema.GuestTag(v.Handle),
		StartedAt:     schema.NewTimestamp(v.StartedAt),
		ExpiresAt:     schema.NewTimestamp(v.ExpiresAt),
		IdleExpiresAt: schema.NewTimestamp(v.IdleExpiresAt),
	}
	if v.State == schema.GuestStateEnded {
		out.EndedAt = timestampPtr(v.EndedAt)
		out.EndReason = v.EndReason
	}
	return out
}

// Admin is the visit as an operator lists it.
func (v GuestVisit) Admin() schema.GuestAdminV1 {
	out := schema.GuestAdminV1{
		ID: v.PrincipalID, Handle: v.Handle, Label: v.Label, Tag: schema.GuestTag(v.Handle),
		State: v.State, Origin: v.Origin,
		StartedAt:     schema.NewTimestamp(v.StartedAt),
		LastActiveAt:  schema.NewTimestamp(v.LastActiveAt),
		ExpiresAt:     schema.NewTimestamp(v.ExpiresAt),
		IdleExpiresAt: schema.NewTimestamp(v.IdleExpiresAt),
		ConvertedAt:   timestampPtr(v.ConvertedAt),
	}
	if v.InviteID > 0 {
		id := v.InviteID
		out.InviteID = &id
	}
	if v.State == schema.GuestStateEnded {
		out.EndedAt = timestampPtr(v.EndedAt)
		out.EndReason = v.EndReason
	}
	return out
}

func timestampPtr(t *time.Time) *schema.Timestamp {
	if t == nil {
		return nil
	}
	ts := schema.NewTimestamp(*t)
	return &ts
}

func guestAttribution(handle, label string) schema.GuestAttributionV1 {
	return schema.GuestAttributionV1{Handle: handle, Label: label, Tag: schema.GuestTag(handle)}
}

// guestAuthorTx returns the guest attribution the insert trigger stored for
// message id (migration 31), or nil when its author was not a guest.
func guestAuthorTx(ctx context.Context, tx execer, id int64) (*schema.GuestAttributionV1, error) {
	var handle string
	var label sql.NullString
	switch err := tx.QueryRowContext(ctx,
		"SELECT handle, label FROM message_guest_attribution WHERE message_id = ?", id).Scan(&handle, &label); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("store: read guest attribution of message %d: %w", id, err)
	}
	a := guestAttribution(handle, label.String)
	return &a, nil
}

// EndedGuest is a visit a committed transaction ended: the server closes its
// sockets, takes it out of voice and tells the rosters of its channels.
type EndedGuest struct {
	PrincipalID int64
	Reason      schema.GuestEndReason
	// Channels are the channels it was removed from.
	Channels []int64
}

// OnGuestsEnded sets the function the store calls, after the commit and never
// inside a transaction, with the visits a call ended, whichever call it was:
// End visit, a sweep, a revocation or a disable. As with OnRunsEnded there is
// one such function; set it before the store is shared.
func (s *Store) OnGuestsEnded(fn func([]EndedGuest)) { s.guestsEnded = fn }

func (s *Store) announceGuestsEnded(ended []EndedGuest) {
	if len(ended) > 0 && s.guestsEnded != nil {
		s.guestsEnded(ended)
	}
}

// NewGuest is what CreateGuest admits.
type NewGuest struct {
	// Label is the optional display label; it is normalized to NFC and
	// checked (design §4.1). ServerName, when set, is a label refused too.
	Label      string
	ServerName string
	// Origin and InviteID are the entry point; an invite origin needs its
	// invite.
	Origin   schema.GuestOrigin
	InviteID int64
	// Channels are the channels the visit is admitted to, each with a fresh
	// history bound and no right to decide approvals.
	Channels []int64
	// Idle and Absolute are the visit's bounds; CredentialLifetime, when
	// positive, is a shorter bound from the entry invite.
	Idle, Absolute, CredentialLifetime time.Duration
	// VoiceAllowed is whether the guest may use voice in its channels.
	VoiceAllowed bool
	// MaxLive, when positive, caps the live visits: the creating
	// transaction counts them and refuses with ErrGuestsFull. Ended visits,
	// and active ones past their deadline that no sweep has ended yet, do
	// not count.
	MaxLive int
}

// CreatedGuest is a new visit: its principal, the visit, its one credential
// and that credential's token, shown only here.
type CreatedGuest struct {
	Principal  Principal
	Visit      GuestVisit
	Credential schema.CredentialV1
	Token      string
}

// CreateGuest starts a visit in one IMMEDIATE transaction: it makes the
// principal under a fresh handle, its visit, its one credential (expiring at
// the visit's hard deadline, with the idle bound) and its memberships, each
// with can_decide 0 and history_after the channel's last message id, and
// audits guest_started, principal_created, member_added and
// credential_created. actor is whoever admits the visit ("system", or the
// invite). Whether this entry point admits guests at all is the caller's
// policy (#710); the store holds to the cap, the bounds and the label rules.
func (s *Store) CreateGuest(ctx context.Context, actor string, g NewGuest) (CreatedGuest, error) {
	now := credentialNow().Truncate(time.Millisecond)
	if !g.Origin.Valid() || (g.Origin == schema.GuestOriginInvite) != (g.InviteID > 0) {
		return CreatedGuest{}, errors.New("store: create guest: an invite origin, and only one, names its invite")
	}
	if g.Idle < time.Millisecond || g.Absolute < time.Millisecond || g.CredentialLifetime < 0 {
		return CreatedGuest{}, errors.New("store: create guest: the idle and absolute bounds must be positive")
	}
	label, err := guestLabel(g.Label, g.ServerName)
	if err != nil {
		return CreatedGuest{}, err
	}
	token, err := newCredentialToken()
	if err != nil {
		return CreatedGuest{}, err
	}
	deadline := now.Add(g.Absolute)
	if g.CredentialLifetime > 0 && now.Add(g.CredentialLifetime).Before(deadline) {
		deadline = now.Add(g.CredentialLifetime)
	}
	deadline = deadline.Truncate(time.Millisecond)
	idle := g.Idle.Truncate(time.Millisecond)

	var out CreatedGuest
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out = CreatedGuest{}
		if g.MaxLive > 0 {
			n, err := liveGuestsTx(ctx, tx, now)
			if err != nil {
				return err
			}
			if n >= g.MaxLive {
				return ErrGuestsFull
			}
		}
		handle, keys, err := newGuestHandleTx(ctx, tx)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO principals (kind, name, name_key, name_skel, role, created_at, voice_allowed)
			 VALUES ('human', ?, ?, ?, 'member', ?, ?)`,
			handle, keys.Folded, keys.Skeletal, now.UnixMilli(), boolInt(g.VoiceAllowed))
		if err != nil {
			return fmt.Errorf("store: create guest: principal: %w", err)
		}
		pid, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: create guest: principal id: %w", err)
		}
		var labelArg any
		if label != "" {
			labelArg = label
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO guest_visits (principal_id, handle, label, origin, invite_id, state, started_at, expires_at, idle_ms)
			 VALUES (?, ?, ?, ?, ?, 'active', ?, ?, ?)`,
			pid, handle, labelArg, string(g.Origin), nullableID(g.InviteID), now.UnixMilli(), deadline.UnixMilli(),
			idle.Milliseconds()); err != nil {
			return fmt.Errorf("store: create guest: visit: %w", err)
		}
		cred, err := insertCredentialTx(ctx, tx, pid, GuestCredentialLabel, token, now, &deadline, 0)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE credentials SET idle_ms = ? WHERE id = ?", idle.Milliseconds(), cred.ID); err != nil {
			return fmt.Errorf("store: create guest: credential idle bound: %w", err)
		}
		idleAt := minTime(now.Add(idle), deadline)
		cred.IdleExpiresAt = timestampPtr(&idleAt)

		detail := "origin=" + string(g.Origin)
		if g.InviteID > 0 {
			detail += fmt.Sprintf(" invite=%d", g.InviteID)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditGuestStarted, principalActor(pid), detail, now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditPrincipalCreated, principalActor(pid), "kind=human guest=1", now); err != nil {
			return err
		}
		seen := map[int64]bool{}
		for _, ch := range g.Channels {
			if seen[ch] {
				continue
			}
			seen[ch] = true
			var one int
			switch err := tx.QueryRowContext(ctx, "SELECT 1 FROM channels WHERE id = ?", ch).Scan(&one); {
			case errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("store: create guest: channel %d: %w", ch, ErrNotFound)
			case err != nil:
				return fmt.Errorf("store: create guest: check channel %d: %w", ch, err)
			}
			if err := admitGuestTx(ctx, tx, actor, ch, pid, 0, now); err != nil {
				return err
			}
		}
		if err := appendAuditEventTx(ctx, tx, actor, "credential_created", principalActor(pid),
			credentialDetail(cred.ID, cred.Label), now); err != nil {
			return err
		}
		out = CreatedGuest{
			Principal: Principal{ID: pid, Kind: PrincipalHuman, Name: handle, Role: RoleMember, CreatedAt: now,
				VoiceAllowed: g.VoiceAllowed, GuestState: schema.GuestStateActive},
			Visit: GuestVisit{PrincipalID: pid, Handle: handle, Label: label, Origin: g.Origin, InviteID: g.InviteID,
				State: schema.GuestStateActive, StartedAt: now, ExpiresAt: deadline, IdleExpiresAt: idleAt, LastActiveAt: now},
			Credential: cred,
		}
		return nil
	})
	if err != nil {
		return CreatedGuest{}, err
	}
	out.Token = token
	return out, nil
}

// admitGuestTx makes guest pid a member of channelID with no right to decide
// its approvals and a history bound at the channel's last message, and
// audits member_added. addedBy is the acting principal, or 0.
func admitGuestTx(ctx context.Context, tx execer, actor string, channelID, pid, addedBy int64, now time.Time) error {
	watermark, err := historyWatermarkTx(ctx, tx, channelID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide, history_after)
		 VALUES (?, ?, ?, ?, 0, ?)`,
		channelID, pid, nullableID(addedBy), now.UnixMilli(), watermark); err != nil {
		return fmt.Errorf("store: admit guest %d to channel %d: %w", pid, channelID, err)
	}
	return appendAuditEventTx(ctx, tx, actor, AuditMemberAdded, channelSubject(channelID),
		memberDetail(pid)+" can_decide=0 guest=1", now)
}

// guestLabel normalizes label to NFC and checks it (design §4.1): the schema's
// rules, and keys outside the guest and hidden namespaces and unlike the
// server's name. Empty is no label.
func guestLabel(label, serverName string) (string, error) {
	if label == "" {
		return "", nil
	}
	label = norm.NFC.String(label)
	if err := schema.ValidateGuestLabel(label); err != nil {
		return "", ErrGuestLabelInvalid
	}
	keys := names.KeysOf(label)
	if keys.Empty() || reservedGuestName(keys) || reservedName(keys) {
		return "", ErrGuestLabelInvalid
	}
	if serverName != "" {
		if sk := names.KeysOf(serverName); keys.Folded == sk.Folded || keys.Skeletal == sk.Skeletal {
			return "", ErrGuestLabelInvalid
		}
	}
	return label, nil
}

// newGuestHandleTx draws a handle no principal and no visit, past or
// present, has, and returns it with its keys.
func newGuestHandleTx(ctx context.Context, tx execer) (string, names.Keys, error) {
	for range guestHandleTries {
		handle, err := newGuestHandle()
		if err != nil {
			return "", names.Keys{}, err
		}
		var taken int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM guest_visits WHERE handle = ?", handle).Scan(&taken); err != nil {
			return "", names.Keys{}, fmt.Errorf("store: create guest: check handle: %w", err)
		}
		if taken > 0 {
			continue
		}
		keys := names.KeysOf(handle)
		switch err := clashTx(ctx, tx, handle, keys, 0); {
		case err == nil:
			return handle, keys, nil
		case !errors.Is(err, ErrDuplicate):
			return "", names.Keys{}, err
		}
	}
	return "", names.Keys{}, errors.New("store: create guest: no free handle after several draws")
}

// newGuestHandle returns "guest-" and schema.GuestHandleRandomBytes from
// crypto/rand in schema.GuestHandleAlphabet, five bits a character.
func newGuestHandle() (string, error) {
	b := make([]byte, schema.GuestHandleRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generate guest handle: %w", err)
	}
	var sb strings.Builder
	sb.WriteString(schema.GuestNamePrefix)
	var acc uint32
	bits := 0
	for _, c := range b {
		acc = acc<<8 | uint32(c)
		bits += 8
		for bits >= 5 {
			bits -= 5
			sb.WriteByte(schema.GuestHandleAlphabet[(acc>>bits)&31])
		}
	}
	return sb.String(), nil
}

// guestVisitColumns is what scanGuestVisit reads, over guest_visits v and its
// unrevoked credential c (LEFT JOINed: a terminal visit has none).
const guestVisitColumns = `v.principal_id, v.handle, v.label, v.origin, v.invite_id, v.state, v.started_at, v.expires_at,
	v.idle_ms, v.ended_at, v.end_reason, v.converted_at, c.created_at, c.last_used_at`

const guestVisitFrom = ` FROM guest_visits v
	LEFT JOIN credentials c ON c.principal_id = v.principal_id AND c.revoked_at IS NULL AND c.idle_ms IS NOT NULL`

func scanGuestVisit(row rowScanner) (GuestVisit, error) {
	var (
		v                        GuestVisit
		label, endReason         sql.NullString
		origin, state            string
		invite, ended, converted sql.NullInt64
		started, expires, idleMs int64
		credCreated, credUsed    sql.NullInt64
	)
	if err := row.Scan(&v.PrincipalID, &v.Handle, &label, &origin, &invite, &state, &started, &expires,
		&idleMs, &ended, &endReason, &converted, &credCreated, &credUsed); err != nil {
		return GuestVisit{}, err
	}
	v.Label = label.String
	v.Origin = schema.GuestOrigin(origin)
	v.InviteID = invite.Int64
	v.State = schema.GuestState(state)
	v.StartedAt = time.UnixMilli(started)
	v.ExpiresAt = time.UnixMilli(expires)
	v.EndReason = schema.GuestEndReason(endReason.String)
	if ended.Valid {
		t := time.UnixMilli(ended.Int64)
		v.EndedAt = &t
	}
	if converted.Valid {
		t := time.UnixMilli(converted.Int64)
		v.ConvertedAt = &t
	}
	v.LastActiveAt = v.StartedAt
	switch {
	case credUsed.Valid:
		v.LastActiveAt = time.UnixMilli(credUsed.Int64)
	case credCreated.Valid:
		v.LastActiveAt = time.UnixMilli(credCreated.Int64)
	}
	v.IdleExpiresAt = minTime(v.LastActiveAt.Add(time.Duration(idleMs)*time.Millisecond), v.ExpiresAt)
	if v.State != schema.GuestStateActive {
		// A terminal visit's bounds stand where they were when it ended.
		end := v.EndedAt
		if end == nil {
			end = v.ConvertedAt
		}
		if end != nil && end.Before(v.IdleExpiresAt) {
			v.IdleExpiresAt = *end
		}
	}
	return v, nil
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// GuestVisitOf returns principalID's visit, whatever its state, or
// ErrNotAGuest when it never had one.
func (s *Store) GuestVisitOf(ctx context.Context, principalID int64) (GuestVisit, error) {
	v, err := guestVisitTx(ctx, s.db, principalID)
	if errors.Is(err, sql.ErrNoRows) {
		return GuestVisit{}, ErrNotAGuest
	}
	return v, err
}

func guestVisitTx(ctx context.Context, tx execer, principalID int64) (GuestVisit, error) {
	v, err := scanGuestVisit(tx.QueryRowContext(ctx,
		`SELECT `+guestVisitColumns+guestVisitFrom+` WHERE v.principal_id = ?`, principalID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return GuestVisit{}, fmt.Errorf("store: read guest visit %d: %w", principalID, err)
	}
	return v, err
}

// guestLiveSQL is the condition, over guest_visits v, that a visit is live at
// a time: active, before its hard deadline, and holding a credential that is
// unrevoked, unexpired and not idle past its bound. It takes the time three
// times.
const guestLiveSQL = `v.state = 'active' AND v.expires_at > ? AND EXISTS (
	SELECT 1 FROM credentials c WHERE c.principal_id = v.principal_id AND c.revoked_at IS NULL
	  AND (c.expires_at IS NULL OR c.expires_at > ?)
	  AND (c.idle_ms IS NULL OR COALESCE(c.last_used_at, c.created_at) + c.idle_ms > ?))`

// liveGuestsTx counts the live visits at now.
func liveGuestsTx(ctx context.Context, tx execer, now time.Time) (int, error) {
	ms := now.UnixMilli()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM guest_visits v WHERE `+guestLiveSQL, ms, ms, ms).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count live guests: %w", err)
	}
	return n, nil
}

// LiveGuests counts the visits live now: what the cap counts.
func (s *Store) LiveGuests(ctx context.Context) (int, error) {
	return liveGuestsTx(ctx, s.db, credentialNow())
}

// GuestLive reports whether principalID is a guest whose visit is live now.
// The voice poller asks it of a guest in a room, so media is refused at the
// visit's deadline without waiting for the sweep.
func (s *Store) GuestLive(ctx context.Context, principalID int64) (bool, error) {
	ms := credentialNow().UnixMilli()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM guest_visits v WHERE v.principal_id = ? AND `+guestLiveSQL,
		principalID, ms, ms, ms).Scan(&n); err != nil {
		return false, fmt.Errorf("store: guest %d live: %w", principalID, err)
	}
	return n > 0, nil
}

// EndGuestVisit ends principalID's active visit for reason, in one IMMEDIATE
// transaction (endGuestVisitTx), and returns it as it now is. A principal
// with no visit is ErrNotAGuest; a visit already ended or converted is
// ErrGuestEnded, and nothing is written.
func (s *Store) EndGuestVisit(ctx context.Context, actor string, principalID int64, reason schema.GuestEndReason) (GuestVisit, error) {
	now := credentialNow().Truncate(time.Millisecond)
	var (
		out   GuestVisit
		ended EndedGuest
		runs  []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		runs = nil
		var err error
		if ended, err = endGuestVisitTx(ctx, tx, actor, principalID, reason, now, &runs); err != nil {
			return err
		}
		out, err = guestVisitTx(ctx, tx, principalID)
		return err
	})
	if err != nil {
		return GuestVisit{}, err
	}
	s.announceRunsEnded(runs)
	s.announceGuestsEnded([]EndedGuest{ended})
	return out, nil
}

// endGuestVisitTx ends principalID's active visit in the caller's IMMEDIATE
// transaction, and does everything that goes with it:
// guest_ended is audited, the guest leaves every channel and net (each
// removal audited), and its credential is revoked (audited, its sessions
// ended). It returns ErrNotAGuest for a principal with no visit and
// ErrGuestEnded, writing nothing, for one already terminal: the guarded
// update decides, so of two racing transitions only one does anything.
func endGuestVisitTx(ctx context.Context, tx execer, actor string, principalID int64, reason schema.GuestEndReason, now time.Time, runs *[]EndedRun) (EndedGuest, error) {
	if !reason.Valid() {
		return EndedGuest{}, fmt.Errorf("store: end guest visit: unknown reason %q", reason)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE guest_visits SET state = 'ended', ended_at = ?, end_reason = ? WHERE principal_id = ? AND state = 'active'`,
		now.UnixMilli(), string(reason), principalID)
	if err != nil {
		return EndedGuest{}, fmt.Errorf("store: end guest visit %d: %w", principalID, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return EndedGuest{}, fmt.Errorf("store: end guest visit %d: %w", principalID, err)
	} else if n != 1 {
		var state string
		switch err := tx.QueryRowContext(ctx, "SELECT state FROM guest_visits WHERE principal_id = ?", principalID).Scan(&state); {
		case errors.Is(err, sql.ErrNoRows):
			return EndedGuest{}, ErrNotAGuest
		case err != nil:
			return EndedGuest{}, fmt.Errorf("store: end guest visit %d: %w", principalID, err)
		}
		return EndedGuest{}, ErrGuestEnded
	}
	if err := appendAuditEventTx(ctx, tx, actor, AuditGuestEnded, principalActor(principalID), "reason="+string(reason), now); err != nil {
		return EndedGuest{}, err
	}
	channels, err := idsTx(ctx, tx,
		"SELECT channel_id FROM channel_members WHERE principal_id = ? ORDER BY channel_id", principalID)
	if err != nil {
		return EndedGuest{}, fmt.Errorf("store: list memberships of guest %d: %w", principalID, err)
	}
	if err := removeAllMembershipsTx(ctx, tx, actor, principalID, now); err != nil {
		return EndedGuest{}, err
	}
	// The visit's read marks go with its memberships (issue #719).
	if err := deleteReadMarksTx(ctx, tx, principalID); err != nil {
		return EndedGuest{}, err
	}
	if _, err := revokePrincipalCredentialsTx(ctx, tx, actor, principalID, "guest visit ended", SessionEndedByRevocation, now, runs); err != nil {
		return EndedGuest{}, err
	}
	return EndedGuest{PrincipalID: principalID, Reason: reason, Channels: channels}, nil
}

// endIfGuestTx ends principalID's visit for reason if it is active, and
// reports it; for anyone else it does nothing. Revoking a guest's credential
// and disabling a guest end its visit this way, in their own transaction,
// since a guest has exactly one credential and no way back.
func endIfGuestTx(ctx context.Context, tx execer, actor string, principalID int64, reason schema.GuestEndReason, now time.Time, runs *[]EndedRun, ended *[]EndedGuest) error {
	e, err := endGuestVisitTx(ctx, tx, actor, principalID, reason, now, runs)
	switch {
	case errors.Is(err, ErrNotAGuest), errors.Is(err, ErrGuestEnded):
		return nil
	case err != nil:
		return err
	}
	*ended = append(*ended, e)
	return nil
}

// SweepGuests ends, with actor "system", every active visit that is no longer
// live now: reason expired when its hard deadline has passed, idle
// otherwise. It is the deadline sweeper's pass; access was refused at the
// deadline already. It returns what it ended, and has told OnGuestsEnded.
func (s *Store) SweepGuests(ctx context.Context) ([]EndedGuest, error) {
	now := credentialNow().Truncate(time.Millisecond)
	ms := now.UnixMilli()
	var (
		ended []EndedGuest
		runs  []EndedRun
	)
	err := s.withImmediateTx(ctx, func(tx execer) error {
		ended, runs = nil, nil
		rows, err := tx.QueryContext(ctx,
			`SELECT v.principal_id, v.expires_at <= ? OR NOT EXISTS (
				SELECT 1 FROM credentials c WHERE c.principal_id = v.principal_id AND c.revoked_at IS NULL
				  AND (c.expires_at IS NULL OR c.expires_at > ?))
			 FROM guest_visits v WHERE v.state = 'active' AND NOT (`+guestLiveSQL+`) ORDER BY v.principal_id`,
			ms, ms, ms, ms, ms)
		if err != nil {
			return fmt.Errorf("store: sweep guests: %w", err)
		}
		type due struct {
			id      int64
			expired bool
		}
		var all []due
		for rows.Next() {
			var d due
			if err := rows.Scan(&d.id, &d.expired); err != nil {
				_ = rows.Close()
				return fmt.Errorf("store: sweep guests: %w", err)
			}
			all = append(all, d)
		}
		err = rows.Err()
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("store: sweep guests: %w", err)
		}
		for _, d := range all {
			reason := schema.GuestEndIdle
			if d.expired {
				reason = schema.GuestEndExpired
			}
			if err := endIfGuestTx(ctx, tx, "system", d.id, reason, now, &runs, &ended); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.announceRunsEnded(runs)
	s.announceGuestsEnded(ended)
	return ended, nil
}

// NextGuestDeadline returns the earliest deadline of an active visit, hard
// or idle, and false when there is no active visit. A deadline already past
// is returned as it is: the sweep is due.
func (s *Store) NextGuestDeadline(ctx context.Context) (time.Time, bool, error) {
	var next sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(MIN(v.expires_at, COALESCE(c.expires_at, v.expires_at),
		                COALESCE(c.last_used_at, c.created_at, v.started_at) + COALESCE(c.idle_ms, v.idle_ms)))
		 FROM guest_visits v
		 LEFT JOIN credentials c ON c.principal_id = v.principal_id AND c.revoked_at IS NULL
		 WHERE v.state = 'active'`).Scan(&next)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: next guest deadline: %w", err)
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(next.Int64), true, nil
}

// NoteGuestUse records that a REST request authenticated with
// credential credID just now, which moves its idle expiry (design §4.2, review
// F5). It writes only for a credential with an idle bound, still live, whose
// last recorded use is guestActivityStep or more ago: at most one write a
// minute, never for an idle-expired credential (which it cannot revive),
// and never past the hard deadline (the idle expiry is capped by it). Only
// REST requests call it: never a socket, a ping, a ticket or the voice
// poller. A failed write is logged, not returned: the request has already
// authenticated, and the next one tries again.
func (s *Store) NoteGuestUse(ctx context.Context, credID int64) {
	ms := credentialNow().UnixMilli()
	ctx = context.WithoutCancel(ctx)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE credentials SET last_used_at = ?
		 WHERE id = ? AND idle_ms IS NOT NULL AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > ?)
		   AND COALESCE(last_used_at, created_at) + idle_ms > ?
		   AND COALESCE(last_used_at, created_at) <= ?`,
		ms, credID, ms, ms, ms-guestActivityStep.Milliseconds()); err != nil {
		slog.WarnContext(ctx, "store: record guest activity failed; the next request tries again", "credential", credID, "error", err)
	}
}

// ConvertGuest is the active → converted transition (design §3.3), in one
// IMMEDIATE transaction: the visit must be live and unconverted, the
// permanent name passes every check a name write makes (the reserved guest
// namespace included), the principal keeps its id, memberships and their
// history bounds, the guest credential is revoked and a fresh member
// credential issued, and guest_converted is audited. within, when set, runs
// last in the same transaction: it is where the password is stored (#711),
// so that a refused password leaves the guest as it was. It returns
// ErrNotAGuest, ErrGuestEnded (ended, converted, or past its deadline), or a
// name error, writing nothing.
func (s *Store) ConvertGuest(ctx context.Context, actor string, c GuestConversion) (ConvertedGuest, error) {
	now := credentialNow().Truncate(time.Millisecond)
	if c.CredentialLifetime <= 0 {
		return ConvertedGuest{}, errors.New("store: convert guest: the member credential needs a lifetime")
	}
	token, err := newCredentialToken()
	if err != nil {
		return ConvertedGuest{}, err
	}
	var (
		out  ConvertedGuest
		runs []EndedRun
	)
	err = s.withImmediateTx(ctx, func(tx execer) error {
		out, runs = ConvertedGuest{}, nil
		v, err := guestVisitTx(ctx, tx, c.PrincipalID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotAGuest
		case err != nil:
			return err
		}
		live, err := guestLiveTx(ctx, tx, c.PrincipalID, now)
		if err != nil {
			return err
		}
		if v.State != schema.GuestStateActive || !live {
			return ErrGuestEnded
		}
		p, err := principalByIDTx(ctx, tx, c.PrincipalID)
		if err != nil {
			return err
		}
		if p.DisabledAt != nil {
			return ErrGuestEnded
		}
		keys, err := claimNameTx(ctx, tx, c.Name, c.PrincipalID)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE guest_visits SET state = 'converted', converted_at = ? WHERE principal_id = ? AND state = 'active'`,
			now.UnixMilli(), c.PrincipalID)
		if err != nil {
			return fmt.Errorf("store: convert guest %d: %w", c.PrincipalID, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("store: convert guest %d: %w", c.PrincipalID, err)
		} else if n != 1 {
			return ErrGuestEnded
		}
		if err := setNameTx(ctx, tx, actor, p, c.Name, keys, now); err != nil {
			return err
		}
		if _, err := revokePrincipalCredentialsTx(ctx, tx, actor, c.PrincipalID, "guest converted", SessionEndedByRevocation, now, &runs); err != nil {
			return err
		}
		exp := now.Add(c.CredentialLifetime)
		cred, err := insertCredentialTx(ctx, tx, c.PrincipalID, c.CredentialLabel, token, now, &exp, c.CredentialLifetime)
		if err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, "credential_created", principalActor(c.PrincipalID),
			credentialDetail(cred.ID, cred.Label), now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditGuestConverted, principalActor(c.PrincipalID),
			fmt.Sprintf("from=%q to=%q", v.Handle, c.Name), now); err != nil {
			return err
		}
		if c.within != nil {
			if err := c.within(ctx, tx); err != nil {
				return err
			}
		}
		p.Name = c.Name
		p.GuestState = schema.GuestStateConverted
		out = ConvertedGuest{Principal: p, Credential: cred}
		return nil
	})
	if err != nil {
		return ConvertedGuest{}, err
	}
	s.announceRunsEnded(runs)
	out.Token = token
	return out, nil
}

// GuestConversion is what ConvertGuest makes of a guest.
type GuestConversion struct {
	PrincipalID int64
	// Name is the permanent name, valid as a human principal's.
	Name string
	// CredentialLabel and CredentialLifetime are the fresh member
	// credential's; it renews as an invited member's does.
	CredentialLabel    string
	CredentialLifetime time.Duration
	// within runs last in the converting transaction (the password, #711).
	within func(ctx context.Context, tx execer) error
}

// ConvertedGuest is the principal after a conversion, under its permanent
// name, with its new credential and that credential's token.
type ConvertedGuest struct {
	Principal  Principal
	Credential schema.CredentialV1
	Token      string
}

func guestLiveTx(ctx context.Context, tx execer, principalID int64, now time.Time) (bool, error) {
	ms := now.UnixMilli()
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM guest_visits v WHERE v.principal_id = ? AND `+guestLiveSQL,
		principalID, ms, ms, ms).Scan(&n); err != nil {
		return false, fmt.Errorf("store: guest %d live: %w", principalID, err)
	}
	return n > 0, nil
}

// idleExpirySQL is where a credential's idle bound stands, over the
// credentials table: its last use, or its creation, plus idle_ms, never past
// its expiry; NULL for a credential with no idle bound.
const idleExpirySQL = `CASE WHEN idle_ms IS NULL THEN NULL
	ELSE MIN(COALESCE(last_used_at, created_at) + idle_ms, COALESCE(expires_at, COALESCE(last_used_at, created_at) + idle_ms)) END`

// idleBoundColumns are what ResolveCredentialDetail and CredentialLive
// read, over credentials c, for credentialIdle: the credential's idle bound
// and its principal's guest visit.
const idleBoundColumns = `c.created_at, c.idle_ms, c.last_used_at,
	(SELECT v.state FROM guest_visits v WHERE v.principal_id = c.principal_id),
	(SELECT v.expires_at FROM guest_visits v WHERE v.principal_id = c.principal_id)`

// credentialIdle is a credential's idle bound and its principal's visit.
type credentialIdle struct {
	createdAt          int64
	idleMs, lastUsedAt sql.NullInt64
	guestState         sql.NullString
	guestExpiresAt     sql.NullInt64
}

// live reports whether, at now, the credential is within its idle bound
// (COALESCE(last_used_at, created_at) + idle_ms, review F5) and its principal
// is not a guest whose visit has ended or passed its hard deadline. The
// checks of revocation, expiry and disable are the caller's.
func (c credentialIdle) live(now time.Time) bool {
	ms := now.UnixMilli()
	if c.idleMs.Valid {
		last := c.createdAt
		if c.lastUsedAt.Valid {
			last = c.lastUsedAt.Int64
		}
		if ms >= last+c.idleMs.Int64 {
			return false
		}
	}
	switch schema.GuestState(c.guestState.String) {
	case schema.GuestStateEnded:
		return false
	case schema.GuestStateActive:
		return !c.guestExpiresAt.Valid || ms < c.guestExpiresAt.Int64
	}
	return true
}

// ReservedGuestNames returns, by id, the permanent principals whose names are
// in the reserved guest namespace: names written before the reservation,
// which migration 31 reports and never changes. While there are any, guests
// cannot be enabled (#710); an operator renames them, by an explicit,
// audited rename.
func (s *Store) ReservedGuestNames(ctx context.Context) ([]int64, error) {
	return reservedGuestNamesTx(ctx, s.db)
}

func reservedGuestNamesTx(ctx context.Context, tx execer) ([]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, name_key, name_skel FROM principals p
		 WHERE NOT EXISTS (SELECT 1 FROM guest_visits v WHERE v.principal_id = p.id AND v.state <> 'converted')
		 ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read principal names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var (
			id        int64
			key, skel sql.NullString
		)
		if err := rows.Scan(&id, &key, &skel); err != nil {
			return nil, fmt.Errorf("scan principal name: %w", err)
		}
		if reservedGuestName(names.Keys{Folded: key.String, Skeletal: skel.String}) {
			out = append(out, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read principal names: %w", err)
	}
	return out, nil
}

// reportReservedGuestNames is migration 31's Go step: it logs, by id, every
// principal whose stored folded key or skeleton begins with the guest
// prefix's. Nothing is renamed, hidden or reclassified (design §4.1).
func reportReservedGuestNames(ctx context.Context, tx *sql.Tx) error {
	ids, err := reservedGuestNamesTx(ctx, tx)
	if err != nil {
		return err
	}
	if len(ids) > 0 {
		slog.WarnContext(ctx, "store: some existing principal names are in the namespace now reserved for guests; they are kept as they are, guests cannot be enabled until an operator renames them, and none of them may be renamed to another such name",
			"prefix", schema.ReservedGuestNameKeyPrefix, "principals", ids)
	}
	return nil
}
