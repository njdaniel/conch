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
// The entry routes and their policy are issue #710's (guest_policy.go);
// conversion is #711's (ConvertGuest).

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
// bound is accurate to a minute and costs no write per request. A
// credential whose idle bound is shorter than guestActivityDivisor steps
// moves it at most once a quarter of its bound instead: with a step as long
// as the bound, the bound would run out at the moment the next write was
// first allowed, and an active visit at the minimum bound would end a bound
// after it began (issue #812). So a request at least every bound less its
// step, the shorter of a minute and a quarter of the bound, always keeps a
// visit alive.
var guestActivityStep = time.Minute

// guestActivityDivisor caps a credential's activity step at this share of
// its idle bound: a quarter.
const guestActivityDivisor = 4

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

// V1 is the visit as its guest sees it. CanConvert is false here: it depends
// on the policy of the visit's origin now, which the server reads
// (GuestCanConvert) and sets.
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
	// not count. Zero is no cap.
	MaxLive int
	// MaxPerAddress, when positive, caps the live visits among AddressPeers:
	// the visits the server saw start from the caller's address (review
	// F13), counted the way MaxLive counts. The store never holds an
	// address: the server names the visits, the store counts the live ones.
	MaxPerAddress int
	AddressPeers  []int64
	// InvitedBy is the entry invite's maker. It is recorded on the principal
	// with the invite, as for an invited member, so that disabling an
	// invite's joiners ends its guests' visits too. Zero for an address
	// visit.
	InvitedBy int64
}

// CreatedGuest is a new visit: its principal, the visit, its one credential
// and that credential's token, shown only here.
type CreatedGuest struct {
	Principal  Principal
	Visit      GuestVisit
	Credential schema.CredentialV1
	Token      string
}

// CreateGuest starts a visit in one IMMEDIATE transaction (createGuestTx).
// Whether an entry point admits guests at all is policy: the entry routes
// (#710) call JoinAsGuest and RedeemInviteGuest (guest_policy.go), which
// check it in the creating transaction. CreateGuest checks no policy, only
// the caps, the bounds and the label rules.
func (s *Store) CreateGuest(ctx context.Context, actor string, g NewGuest) (CreatedGuest, error) {
	now := credentialNow().Truncate(time.Millisecond)
	if err := checkNewGuest(g); err != nil {
		return CreatedGuest{}, err
	}
	p, err := prepareGuest(g.Label, g.ServerName)
	if err != nil {
		return CreatedGuest{}, err
	}
	var out CreatedGuest
	err = s.withImmediateTx(ctx, func(tx execer) error {
		var err error
		out, err = createGuestTx(ctx, tx, actor, g, p, now)
		return err
	})
	if err != nil {
		return CreatedGuest{}, err
	}
	out.Token = p.token
	return out, nil
}

// preparedGuest is what a visit needs before its transaction: the checked
// label and a fresh token, made outside the write lock.
type preparedGuest struct {
	label, token string
}

// checkNewGuest checks g's entry point and bounds.
func checkNewGuest(g NewGuest) error {
	if !g.Origin.Valid() || (g.Origin == schema.GuestOriginInvite) != (g.InviteID > 0) {
		return errors.New("store: create guest: an invite origin, and only one, names its invite")
	}
	if g.Idle < time.Millisecond || g.Absolute < time.Millisecond || g.CredentialLifetime < 0 {
		return errors.New("store: create guest: the idle and absolute bounds must be positive")
	}
	return nil
}

// prepareGuest checks the label, as guestLabel does, and draws the visit's
// token: the work that needs no database, done before the write lock.
func prepareGuest(label, serverName string) (preparedGuest, error) {
	label, err := guestLabel(label, serverName)
	if err != nil {
		return preparedGuest{}, err
	}
	token, err := newCredentialToken()
	if err != nil {
		return preparedGuest{}, err
	}
	return preparedGuest{label: label, token: token}, nil
}

// createGuestTx starts a visit in the caller's IMMEDIATE transaction: it
// makes the principal under a fresh handle, its visit, its one credential
// (expiring at the visit's hard deadline, with the idle bound) and its
// memberships, each with can_decide 0 and history_after the channel's last
// message id, and audits guest_started, principal_created, member_added and
// credential_created. actor is whoever admits the visit ("system", or the
// invite). The caps are counted in this transaction, the one that would
// exceed them, so two racing admissions cannot both take the last place. The
// result carries no token: the caller adds p.token once the commit is done.
func createGuestTx(ctx context.Context, tx execer, actor string, g NewGuest, p preparedGuest, now time.Time) (CreatedGuest, error) {
	if err := checkNewGuest(g); err != nil {
		return CreatedGuest{}, err
	}
	label := p.label
	deadline := now.Add(g.Absolute)
	if g.CredentialLifetime > 0 && now.Add(g.CredentialLifetime).Before(deadline) {
		deadline = now.Add(g.CredentialLifetime)
	}
	deadline = deadline.Truncate(time.Millisecond)
	idle := g.Idle.Truncate(time.Millisecond)

	if g.MaxLive > 0 {
		n, err := liveGuestsTx(ctx, tx, now)
		if err != nil {
			return CreatedGuest{}, err
		}
		if n >= g.MaxLive {
			return CreatedGuest{}, ErrGuestsFull
		}
	}
	if g.MaxPerAddress > 0 {
		n, err := liveGuestsAmongTx(ctx, tx, g.AddressPeers, now)
		if err != nil {
			return CreatedGuest{}, err
		}
		if n >= g.MaxPerAddress {
			return CreatedGuest{}, ErrGuestsFull
		}
	}
	handle, keys, err := newGuestHandleTx(ctx, tx)
	if err != nil {
		return CreatedGuest{}, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO principals (kind, name, name_key, name_skel, role, created_at, voice_allowed, invited_by, invite_id)
		 VALUES ('human', ?, ?, ?, 'member', ?, ?, ?, ?)`,
		handle, keys.Folded, keys.Skeletal, now.UnixMilli(), boolInt(g.VoiceAllowed), nullableID(g.InvitedBy), nullableID(g.InviteID))
	if err != nil {
		return CreatedGuest{}, fmt.Errorf("store: create guest: principal: %w", err)
	}
	pid, err := res.LastInsertId()
	if err != nil {
		return CreatedGuest{}, fmt.Errorf("store: create guest: principal id: %w", err)
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
		return CreatedGuest{}, fmt.Errorf("store: create guest: visit: %w", err)
	}
	cred, err := insertCredentialTx(ctx, tx, pid, GuestCredentialLabel, p.token, now, &deadline, 0)
	if err != nil {
		return CreatedGuest{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE credentials SET idle_ms = ? WHERE id = ?", idle.Milliseconds(), cred.ID); err != nil {
		return CreatedGuest{}, fmt.Errorf("store: create guest: credential idle bound: %w", err)
	}
	idleAt := minTime(now.Add(idle), deadline)
	cred.IdleExpiresAt = timestampPtr(&idleAt)

	detail := "origin=" + string(g.Origin)
	if g.InviteID > 0 {
		detail += fmt.Sprintf(" invite=%d", g.InviteID)
	}
	if err := appendAuditEventTx(ctx, tx, actor, AuditGuestStarted, principalActor(pid), detail, now); err != nil {
		return CreatedGuest{}, err
	}
	if err := appendAuditEventTx(ctx, tx, actor, AuditPrincipalCreated, principalActor(pid), "kind=human guest=1", now); err != nil {
		return CreatedGuest{}, err
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
			return CreatedGuest{}, fmt.Errorf("store: create guest: channel %d: %w", ch, ErrNotFound)
		case err != nil:
			return CreatedGuest{}, fmt.Errorf("store: create guest: check channel %d: %w", ch, err)
		}
		if err := admitGuestTx(ctx, tx, actor, ch, pid, 0, now); err != nil {
			return CreatedGuest{}, err
		}
	}
	if err := appendAuditEventTx(ctx, tx, actor, "credential_created", principalActor(pid),
		credentialDetail(cred.ID, cred.Label), now); err != nil {
		return CreatedGuest{}, err
	}
	return CreatedGuest{
		Principal: Principal{ID: pid, Kind: PrincipalHuman, Name: handle, Role: RoleMember, CreatedAt: now,
			VoiceAllowed: g.VoiceAllowed, GuestState: schema.GuestStateActive, InvitedBy: g.InvitedBy, InviteID: g.InviteID},
		Visit: GuestVisit{PrincipalID: pid, Handle: handle, Label: label, Origin: g.Origin, InviteID: g.InviteID,
			State: schema.GuestStateActive, StartedAt: now, ExpiresAt: deadline, IdleExpiresAt: idleAt, LastActiveAt: now},
		Credential: cred,
	}, nil
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

// liveGuestsAmongTx counts the live visits at now among ids.
func liveGuestsAmongTx(ctx context.Context, tx execer, ids []int64, now time.Time) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	ms := now.UnixMilli()
	args := []any{ms, ms, ms}
	marks := make([]string, len(ids))
	for i, id := range ids {
		marks[i] = "?"
		args = append(args, id)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM guest_visits v WHERE `+guestLiveSQL+ // #nosec G202 -- placeholders only; every value is in args
		` AND v.principal_id IN (`+strings.Join(marks, ",")+`)`, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count live guests of one address: %w", err)
	}
	return n, nil
}

// LiveGuestsAmong counts the visits live now among ids: what the per-address
// cap counts.
func (s *Store) LiveGuestsAmong(ctx context.Context, ids []int64) (int, error) {
	return liveGuestsAmongTx(ctx, s.db, ids, credentialNow())
}

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
// F5). Despite its name it serves every idle-bounded credential: a guest's,
// and an operator's browser session (issue #709), whose idle bound works the
// same way. It writes only for a credential with an idle bound, still live, whose
// last recorded use is its step or more ago, the shorter of
// guestActivityStep and a quarter of its idle bound: at most one write a
// step, never for an idle-expired credential (which it cannot revive),
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
		   AND COALESCE(last_used_at, created_at) <= ? - MIN(?, idle_ms / ?)`,
		ms, credID, ms, ms, ms, guestActivityStep.Milliseconds(), guestActivityDivisor); err != nil {
		slog.WarnContext(ctx, "store: record guest activity failed; the next request tries again", "credential", credID, "error", err)
	}
}

// Errors of conversion (issue #711, design §3.3).
var (
	// ErrConversionNotAllowed: the policy of the visit's recorded origin
	// admits no account now (the bare address's mode, or the entry invite's
	// join_as, closure, expiry or maker), and no permit was given.
	ErrConversionNotAllowed = errors.New("store: this visit may not create an account now")
	// ErrConversionPermitInvalid: the permit given is unknown, used, closed,
	// expired, of an older authentication generation, another guest's, of
	// another purpose, or its maker is no longer an enabled operator. One
	// error for all of them.
	ErrConversionPermitInvalid = errors.New("store: conversion permit invalid")
)

// liveGuestTx checks, in the caller's transaction, that principalID is a
// guest whose visit is live now: active, before its hard and idle deadlines,
// holding its credential under its present authentication generation, and
// enabled. A principal that never had a visit is ErrNotAGuest; any other is
// ErrGuestEnded, a converted one included, so that nothing converts or
// permits a visit twice.
func liveGuestTx(ctx context.Context, tx execer, principalID int64) error {
	v, err := guestVisitTx(ctx, tx, principalID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrNotAGuest
	case err != nil:
		return err
	}
	live, err := guestLiveTx(ctx, tx, principalID, credentialNow())
	if err != nil {
		return err
	}
	if v.State != schema.GuestStateActive || !live {
		return ErrGuestEnded
	}
	// A credential of an older generation resolves for nobody (a restore
	// moves every generation on), so the visit has no way in left.
	var current bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM credentials c JOIN principals p ON p.id = c.principal_id
		                WHERE c.principal_id = ? AND c.revoked_at IS NULL AND c.auth_generation = p.auth_generation)`,
		principalID).Scan(&current); err != nil {
		return fmt.Errorf("store: guest %d credential generation: %w", principalID, err)
	}
	if !current {
		return ErrGuestEnded
	}
	p, err := principalByIDTx(ctx, tx, principalID)
	if err != nil {
		return err
	}
	if p.DisabledAt != nil {
		return ErrGuestEnded
	}
	return nil
}

// conversionPolicyTx reports whether the policy of v's recorded origin
// admits an account now (design §3.3), the rule Create an account follows
// without a permit. A bare-address visit needs open_joining account or both
// (else ErrConversionNotAllowed) and joining not paused (ErrJoiningPaused).
// An invite visit needs invites not paused (ErrInvitesPaused) and its
// recorded invite to admit accounts (join_as account or both), unclosed,
// unexpired at now and still stood behind by its maker
// (ErrConversionNotAllowed otherwise); the invite's uses spent do not
// matter, since the guest's admission has been had. The guests_enabled
// switch plays no part: it is about starting visits, not ending them in an
// account. now is the invite clock.
func conversionPolicyTx(ctx context.Context, tx execer, v GuestVisit, now time.Time) error {
	switch v.Origin {
	case schema.GuestOriginAddress:
		set, err := joinSettingsTx(ctx, tx)
		if err != nil {
			return err
		}
		if !AddressAdmitsAccounts(set) {
			return ErrConversionNotAllowed
		}
		if set.JoiningPaused {
			return ErrJoiningPaused
		}
		return nil
	case schema.GuestOriginInvite:
		if paused, err := invitesPausedTx(ctx, tx); err != nil {
			return err
		} else if paused {
			return ErrInvitesPaused
		}
		r, err := scanInvite(tx.QueryRowContext(ctx,
			`SELECT `+inviteColumns+` FROM invites i JOIN principals p ON p.id = i.created_by WHERE i.id = ?`, v.InviteID))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return ErrConversionNotAllowed
		case err != nil:
			return fmt.Errorf("store: conversion policy: entry invite: %w", err)
		}
		if !admitsAccount(r.joinAs) || r.closedAt.Valid || r.expiresAt <= now.UnixMilli() || !r.makerStillAllowed() {
			return ErrConversionNotAllowed
		}
		return nil
	}
	// A visit of an origin this code does not know is not let through.
	return ErrConversionNotAllowed
}

// conversionAdmissionTx decides, in the caller's transaction, whether the
// live visit v may become an account now. With a permit (permitHash set) the
// permit decides alone: it must be a usable conversion permit for v's
// principal (usableConversionPermitTx, else ErrConversionPermitInvalid), and
// its id is returned for the caller to spend. Without one the origin's
// policy decides (conversionPolicyTx). A permit that does not work is never
// passed over for the policy: what the guest sent is what is judged.
func conversionAdmissionTx(ctx context.Context, tx execer, v GuestVisit, permitHash string, now time.Time) (int64, error) {
	if permitHash != "" {
		return usableConversionPermitTx(ctx, tx, permitHash, v.PrincipalID, now)
	}
	return 0, conversionPolicyTx(ctx, tx, v, now)
}

// CheckConversion says, without writing anything, whether ConvertGuest would
// let principalID's visit become an account now, by everything but the name
// and the password: the visit is live (ErrNotAGuest, ErrGuestEnded) and the
// permit or the policy admits it (conversionAdmissionTx's errors). The
// server asks before it hashes, so a conversion that cannot happen costs no
// Argon2 work; ConvertGuest checks all of it again. now is the invite and
// link clock.
func (s *Store) CheckConversion(ctx context.Context, principalID int64, permitHash string, now time.Time) error {
	if err := liveGuestTx(ctx, s.db, principalID); err != nil {
		return err
	}
	v, err := guestVisitTx(ctx, s.db, principalID)
	if err != nil {
		return err
	}
	_, err = conversionAdmissionTx(ctx, s.db, v, permitHash, now.Truncate(time.Millisecond))
	return err
}

// GuestCanConvert reports whether principalID's visit is live and its
// origin's policy admits an account now, without a permit: GuestV1's
// can_convert. It is advice for a page; ConvertGuest decides.
func (s *Store) GuestCanConvert(ctx context.Context, principalID int64, now time.Time) (bool, error) {
	err := s.CheckConversion(ctx, principalID, "", now)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotAGuest), errors.Is(err, ErrGuestEnded), errors.Is(err, ErrConversionNotAllowed),
		errors.Is(err, ErrJoiningPaused), errors.Is(err, ErrInvitesPaused):
		return false, nil
	}
	return false, err
}

// ConvertGuest is the active → converted transition (design §3.3, issue
// #711): Create an account during a live visit. Everything happens in one
// IMMEDIATE transaction, after the server has hashed the password outside
// it, and the transaction re-reads everything the server's checks relied
// on:
//
//   - the visit is live and unconverted, and its principal enabled
//     (liveGuestTx: ErrNotAGuest, ErrGuestEnded);
//   - the permit, or without one the policy of the visit's recorded origin,
//     admits an account (conversionAdmissionTx);
//   - the permanent name passes every check a name write makes, the reserved
//     guest namespace and the server's name included (claimNameTx:
//     ErrDuplicate, ErrNameReserved, ErrNameInvalid).
//
// Then the visit becomes converted, the name is set (principal_renamed), the
// guest credential is revoked and its sessions ended, the permit, when one
// was used, is spent and any other open permit of the guest closed, a fresh
// renewable member credential is issued, and the password is stored
// (password_set via=conversion); credential_created and guest_converted are
// audited. The principal keeps its id, origin, invite and inviter, its
// memberships with their history bounds and every can_decide as it was (0:
// conversion writes no membership at all), and the guest attribution of its
// earlier posts. Nothing is granted. Anything refused or failing writes
// nothing, so the guest is left as it was; of a conversion and an End visit,
// an expiry, a disable or a second conversion racing on one visit exactly
// one wins. ConvertedGuest.Revoked names the credentials revoked, for the
// server to close their sockets.
func (s *Store) ConvertGuest(ctx context.Context, actor string, c GuestConversion) (ConvertedGuest, error) {
	now := credentialNow().Truncate(time.Millisecond)
	at := now
	if !c.Now.IsZero() {
		at = c.Now.Truncate(time.Millisecond)
	}
	if c.CredentialLifetime < 0 {
		return ConvertedGuest{}, errors.New("store: convert guest: the member credential's lifetime must not be negative")
	}
	// An account exists only with a usable password (ADR-009 D3): there is
	// no conversion without one.
	if !strings.HasPrefix(c.Record, passwordRecordPrefix) {
		return ConvertedGuest{}, ErrPasswordRecordInvalid
	}
	if err := (schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: c.Name}).Validate(); err != nil {
		return ConvertedGuest{}, fmt.Errorf("%w: %w", ErrNameInvalid, err)
	}
	label := c.CredentialLabel
	if label == "" {
		label = ConvertedCredentialLabel
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
		if err := liveGuestTx(ctx, tx, c.PrincipalID); err != nil {
			return err
		}
		v, err := guestVisitTx(ctx, tx, c.PrincipalID)
		if err != nil {
			return err
		}
		p, err := principalByIDTx(ctx, tx, c.PrincipalID)
		if err != nil {
			return err
		}
		permitID, err := conversionAdmissionTx(ctx, tx, v, c.PermitHash, at)
		if err != nil {
			return err
		}
		keys, err := s.claimNameTx(ctx, tx, c.Name, c.PrincipalID)
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
		if out.Revoked, err = idsTx(ctx, tx,
			"SELECT id FROM credentials WHERE principal_id = ? AND revoked_at IS NULL ORDER BY id", c.PrincipalID); err != nil {
			return fmt.Errorf("store: convert guest %d: list credentials: %w", c.PrincipalID, err)
		}
		if _, err := revokePrincipalCredentialsTx(ctx, tx, actor, c.PrincipalID, "guest converted", SessionEndedByRevocation, now, &runs); err != nil {
			return err
		}
		if permitID != 0 {
			if err := spendPasswordLinkTx(ctx, tx, permitID, at); err != nil {
				if errors.Is(err, ErrSignInLinkInvalid) {
					return ErrConversionPermitInvalid
				}
				return err
			}
		}
		// The guest's other permits have nothing left to permit.
		if err := closeOpenSignInLinksTx(ctx, tx, actor, c.PrincipalID, false, SignInLinkCloseConverted, at); err != nil {
			return err
		}
		lifetime := c.CredentialLifetime
		if lifetime == 0 {
			// As a sign-in link's: what the entry invite gave its joiners,
			// or an invite's default for a visit no invite made.
			if lifetime, err = signInLinkCredentialLifetimeTx(ctx, tx, c.PrincipalID); err != nil {
				return err
			}
		}
		exp := now.Add(lifetime)
		cred, err := insertCredentialTx(ctx, tx, c.PrincipalID, label, token, now, &exp, lifetime)
		if err != nil {
			return err
		}
		if _, err := setPasswordTx(ctx, tx, c.PrincipalID, c.Record, now); err != nil {
			return err
		}
		detail := "via=conversion"
		if permitID != 0 {
			detail += fmt.Sprintf(" sign_in_link=%d", permitID)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditPasswordSet, principalActor(c.PrincipalID), detail, now); err != nil {
			return err
		}
		if err := appendAuditEventTx(ctx, tx, actor, "credential_created", principalActor(c.PrincipalID),
			credentialDetail(cred.ID, cred.Label), now); err != nil {
			return err
		}
		converted := fmt.Sprintf("from=%q to=%q", v.Handle, c.Name)
		if permitID != 0 {
			converted += fmt.Sprintf(" permit=%d", permitID)
		}
		if err := appendAuditEventTx(ctx, tx, actor, AuditGuestConverted, principalActor(c.PrincipalID), converted, now); err != nil {
			return err
		}
		if c.within != nil {
			if err := c.within(ctx, tx); err != nil {
				return err
			}
		}
		if out.Channels, err = idsTx(ctx, tx,
			"SELECT channel_id FROM channel_members WHERE principal_id = ? ORDER BY channel_id", c.PrincipalID); err != nil {
			return fmt.Errorf("store: convert guest %d: list memberships: %w", c.PrincipalID, err)
		}
		p.Name = c.Name
		p.GuestState = schema.GuestStateConverted
		out.Principal, out.Credential = p, cred
		return nil
	})
	if err != nil {
		return ConvertedGuest{}, err
	}
	s.announceRunsEnded(runs)
	out.Token = token
	return out, nil
}

// ConvertedCredentialLabel is the label of the member credential a
// conversion issues when the caller names none.
const ConvertedCredentialLabel = "account created from a guest visit"

// GuestConversion is what ConvertGuest makes of a guest.
type GuestConversion struct {
	PrincipalID int64
	// Name is the permanent name, valid as a human principal's.
	Name string
	// Record is the encoded Argon2id record of the password the guest
	// chose, made by the server outside the transaction. Required.
	Record string
	// PermitHash is the hash of an operator's conversion permit
	// (HashSignInLinkCode), when the guest gave one; empty lets the origin's
	// policy decide.
	PermitHash string
	// Now is the invite and link clock the policy and the permit are judged
	// by; zero is the credential clock.
	Now time.Time
	// CredentialLabel and CredentialLifetime are the fresh member
	// credential's; it renews as an invited member's does. An empty label is
	// ConvertedCredentialLabel; a zero lifetime is the one a sign-in link
	// would give the principal (its entry invite's, or the default).
	CredentialLabel    string
	CredentialLifetime time.Duration
	// within runs last in the converting transaction: the tests' seam for a
	// failure after every other write.
	within func(ctx context.Context, tx execer) error
}

// ConvertedGuest is the principal after a conversion, under its permanent
// name, with its new credential and that credential's token, the guest
// credentials the conversion revoked and the channels it is still in.
type ConvertedGuest struct {
	Principal  Principal
	Credential schema.CredentialV1
	Token      string
	Revoked    []int64
	Channels   []int64
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
