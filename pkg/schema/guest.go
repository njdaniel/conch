package schema

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Temporary guest wire shapes (ADR-009, docs/design/guests-and-accounts.md;
// issue #704). A guest is a human principal in guest state: it holds a
// server-made handle in the reserved guest namespace, exactly one credential,
// and memberships bounded to the current visit. These shapes are additive and
// carry no schema-name field: they are REST bodies, versioned by the V1 type
// suffix. They are contracts for #705–#716; this file adds no behaviour.

const (
	// GuestNamePrefix starts every guest handle: "guest-" and a suffix.
	GuestNamePrefix = "guest-"
	// ReservedGuestNameKeyPrefix is the start of a name key that no permanent
	// principal may have (design §4.1): a name whose folded key or confusable
	// skeleton begins with it is refused on signup, conversion, rename and
	// operator creation, with no operator bypass. IsReservedGuestName is the
	// cheap folded check; the server's names package adds the skeleton.
	ReservedGuestNameKeyPrefix = "guest"
	// GuestHandleRandomBytes is the number of crypto/rand bytes in a handle's
	// suffix: 80 bits, so a handle is unguessable and never reused.
	GuestHandleRandomBytes = 10
	// GuestHandleSuffixLength is the exact length of the suffix: 80 bits in
	// GuestHandleAlphabet, five bits a character.
	GuestHandleSuffixLength = 16
	// GuestHandleLength is the exact length of a handle.
	GuestHandleLength = len(GuestNamePrefix) + GuestHandleSuffixLength
	// GuestHandleAlphabet is the suffix's alphabet: Crockford base32 in lower
	// case, which has no i, l, o or u, so a handle read aloud or retyped is
	// not mistaken for another.
	GuestHandleAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	// GuestTagLength is how many characters of the suffix a guest is shown
	// with beside its label: "Guest: Pat · a7k4".
	GuestTagLength = 4
	// MaxGuestLabelLength is the longest display label, in characters.
	MaxGuestLabelLength = 32
	// GuestLabelSeparator is what every surface puts between a guest's label
	// and its tag, and what a label may not contain, so a label cannot fake
	// a second tag.
	GuestLabelSeparator = "·"

	// DefaultGuestIdleSeconds is how long a visit lasts without a request
	// carrying its bearer: two hours. DefaultGuestAbsoluteSeconds is the hard
	// bound of a visit: a day. Both are settings; MaxGuestLifetimeSeconds
	// caps each, and MinGuestIdleSeconds floors the idle bound at a minute.
	// The server records activity at most once a minute, or once a quarter
	// of the idle bound, whichever is shorter.
	DefaultGuestIdleSeconds     = 2 * 60 * 60
	DefaultGuestAbsoluteSeconds = 24 * 60 * 60
	MinGuestIdleSeconds         = 60
	MaxGuestLifetimeSeconds     = 24 * 60 * 60
	// DefaultMaxGuests is the default cap on live visits across every entry
	// point; DefaultMaxGuestsPerAddress the default per client address
	// (security review F13). MaxMaxGuests bounds both settings.
	DefaultMaxGuests           = 50
	DefaultMaxGuestsPerAddress = 5
	MaxMaxGuests               = 10_000
	// MaxOpenChannels is the most channels the bare address may admit to.
	MaxOpenChannels = MaxInviteChannels

	// MaxEndGuests is the most visits one bulk end acts on; past it the
	// answer says how many more match and the operator repeats it.
	MaxEndGuests = 500
	// MaxListGuestsLimit and DefaultListGuestsLimit page the guest list.
	MaxListGuestsLimit     = 200
	DefaultListGuestsLimit = 100
)

// Error codes of the guest and joining routes (ErrorV0.Code).
const (
	// ErrorCodeGuestsDisabled (HTTP 403): no visit may start at this entry
	// point now: the server's guests_enabled switch is off, or the entry
	// point's mode admits no guests. While guests are removed (issue #1005)
	// POST /v1/join/guest and POST /v1/invites/redeem-guest always answer it.
	ErrorCodeGuestsDisabled = "guests_disabled"
	// ErrorCodeJoiningClosed (HTTP 403): no account may be made at this
	// entry point: open_joining is off or guest-only, or the invite admits
	// guests only.
	ErrorCodeJoiningClosed = "joining_closed"
	// ErrorCodeJoiningPaused (HTTP 403): an operator has paused bare-address
	// joining; it pauses conversion of address-origin guests too.
	ErrorCodeJoiningPaused = "joining_paused"
	// ErrorCodeGuestsFull (HTTP 429, no Retry-After): max_guests live visits
	// exist, or max_guests_per_address from the caller's address. Visits end
	// on their own; the caller tries again later.
	ErrorCodeGuestsFull = "guests_full"
	// ErrorCodeGuestNotAllowed (HTTP 403): the caller is a guest and the
	// action is for members: credentials, invites, uploads, channel and net
	// creation, agent interventions, approval requests and decisions, or an
	// operator asked to give a guest a durable right such as can_decide.
	ErrorCodeGuestNotAllowed = "guest_not_allowed"
	// ErrorCodeGuestNotHideable (HTTP 409): hide and rename are refused for
	// a guest, by an operator and by the guest itself: its name is its
	// handle and its attribution is immutable; End visit is the lever
	// (review F10).
	ErrorCodeGuestNotHideable = "guest_not_hideable"
	// ErrorCodeGuestEnded (HTTP 409): the visit is already ended or
	// converted; a terminal visit cannot be converted, ended again or revived.
	ErrorCodeGuestEnded = "guest_ended"
	// ErrorCodeNotAGuest (HTTP 403 on a self route, 404 on an operator
	// route): the principal is not in guest state.
	ErrorCodeNotAGuest = "not_a_guest"
	// ErrorCodeConversionNotAllowed (HTTP 403): the policy of the visit's
	// recorded origin admits no accounts now and no permit was given.
	ErrorCodeConversionNotAllowed = "conversion_not_allowed"
	// ErrorCodeConversionPermitInvalid (HTTP 404): the permit is unknown,
	// expired, used, for another principal, or not a conversion permit at
	// all; one answer for every case, as for sign-in links.
	ErrorCodeConversionPermitInvalid = "conversion_permit_invalid"
	// ErrorCodeJoinPolicyConflict (HTTP 409): the settings asked for
	// contradict each other or the server's state: guest-only joining with
	// guests disabled, open joining with no open channel, or enabling guests
	// while a permanent principal still has a name in the reserved namespace.
	// While guests are removed (issue #1005) it also answers any request for
	// an entry point that would admit guests: PUT /v1/join/settings with
	// guests_enabled true or open_joining guest or both, and POST
	// /v1/invites with join_as guest or both.
	ErrorCodeJoinPolicyConflict = "join_policy_conflict"
)

// GuestState is where a visit is in its life. The vocabulary is closed and
// the lifecycle is active → ended or active → converted, never back.
type GuestState string

const (
	// GuestStateActive: the visit is live.
	GuestStateActive GuestState = "active"
	// GuestStateEnded: the visit ended; its credential, memberships and
	// cursors are gone, and nothing recovers it.
	GuestStateEnded GuestState = "ended"
	// GuestStateConverted: the guest created an account and is now a member
	// under the same principal id.
	GuestStateConverted GuestState = "converted"
)

// Valid reports whether s is a recognized guest state.
func (s GuestState) Valid() bool {
	switch s {
	case GuestStateActive, GuestStateEnded, GuestStateConverted:
		return true
	default:
		return false
	}
}

// GuestOrigin is the entry point a visit came through. It is durable: the
// policy that decides whether the guest may create an account is the policy
// of its origin, and End all guest visits can select by it.
type GuestOrigin string

const (
	// GuestOriginAddress: the bare address, under open_joining.
	GuestOriginAddress GuestOrigin = "address"
	// GuestOriginInvite: an invite whose join_as admits guests.
	GuestOriginInvite GuestOrigin = "invite"
)

// Valid reports whether o is a recognized origin.
func (o GuestOrigin) Valid() bool {
	switch o {
	case GuestOriginAddress, GuestOriginInvite:
		return true
	default:
		return false
	}
}

// GuestEndReason says why a visit ended, as its record and audit row carry.
type GuestEndReason string

const (
	// GuestEndVisit: the guest chose End visit or signed out.
	GuestEndVisit GuestEndReason = "end_visit"
	// GuestEndIdle: no request carried the bearer for the idle bound.
	GuestEndIdle GuestEndReason = "idle"
	// GuestEndExpired: the absolute deadline passed.
	GuestEndExpired GuestEndReason = "expired"
	// GuestEndOperator: an operator ended it: End visit on the guest, End
	// all guest visits, disabling the guest or the entry invite's joiners.
	GuestEndOperator GuestEndReason = "operator"
	// GuestEndRestore: a backup was restored, which ends every restored
	// visit before the server accepts traffic.
	GuestEndRestore GuestEndReason = "restore"
)

// Valid reports whether r is a recognized end reason.
func (r GuestEndReason) Valid() bool {
	switch r {
	case GuestEndVisit, GuestEndIdle, GuestEndExpired, GuestEndOperator, GuestEndRestore:
		return true
	default:
		return false
	}
}

// OpenJoiningMode is what the bare address offers (design §2).
type OpenJoiningMode string

const (
	// OpenJoiningOff: the bare address offers Sign in only. The default.
	OpenJoiningOff OpenJoiningMode = "off"
	// OpenJoiningGuest: Join as guest only. It needs guests_enabled.
	OpenJoiningGuest OpenJoiningMode = "guest"
	// OpenJoiningAccount: Create an account only.
	OpenJoiningAccount OpenJoiningMode = "account"
	// OpenJoiningBoth: both.
	OpenJoiningBoth OpenJoiningMode = "both"
)

// Valid reports whether m is a recognized mode.
func (m OpenJoiningMode) Valid() bool {
	switch m {
	case OpenJoiningOff, OpenJoiningGuest, OpenJoiningAccount, OpenJoiningBoth:
		return true
	default:
		return false
	}
}

// JoinWarning is a condition the operator should know when reading or
// changing the joining settings. The vocabulary is closed; a client shows
// one it does not know by its value.
type JoinWarning string

const (
	// JoinWarningLiveGuestsRemain: guests were turned off while visits are
	// live; they last up to their bounds unless ended with End all guest
	// visits (review F12).
	JoinWarningLiveGuestsRemain JoinWarning = "live_guests_remain"
	// JoinWarningTrustedProxyUnset: no trusted proxy is configured, so every
	// caller may share one address and the per-address cap binds them all
	// together (review F13).
	JoinWarningTrustedProxyUnset JoinWarning = "trusted_proxy_unset"
	// JoinWarningOneAddress: every live guest arrived from one address.
	JoinWarningOneAddress JoinWarning = "one_address"
	// JoinWarningReservedNamesInUse: a permanent principal has a name in the
	// reserved guest namespace, found by migration 31's preflight; guests
	// cannot be enabled until it is renamed, by an explicit audited rename.
	JoinWarningReservedNamesInUse JoinWarning = "reserved_names_in_use"
	// JoinWarningServerNameInUse: a permanent principal holds the server's
	// name, or one that looks the same, from before it was reserved or
	// before the server's name changed to it (issue #760). It is kept, and
	// blocks nothing; an operator may rename it.
	JoinWarningServerNameInUse JoinWarning = "server_name_in_use"
)

// Valid reports whether w is a recognized warning.
func (w JoinWarning) Valid() bool {
	switch w {
	case JoinWarningLiveGuestsRemain, JoinWarningTrustedProxyUnset, JoinWarningOneAddress, JoinWarningReservedNamesInUse,
		JoinWarningServerNameInUse:
		return true
	default:
		return false
	}
}

// ValidateGuestHandle reports whether handle has the exact shape of a guest
// handle: GuestNamePrefix and GuestHandleSuffixLength characters of
// GuestHandleAlphabet. The error never echoes the input.
func ValidateGuestHandle(handle string) error {
	if len(handle) != GuestHandleLength || !strings.HasPrefix(handle, GuestNamePrefix) {
		return errors.New("schema: malformed guest handle")
	}
	for _, c := range []byte(handle[len(GuestNamePrefix):]) {
		if !strings.ContainsRune(GuestHandleAlphabet, rune(c)) {
			return errors.New("schema: malformed guest handle")
		}
	}
	return nil
}

// GuestTag is the tag a guest is shown with: the first GuestTagLength
// characters of a well-formed handle's suffix, or "" for anything else.
func GuestTag(handle string) string {
	if ValidateGuestHandle(handle) != nil {
		return ""
	}
	return handle[len(GuestNamePrefix) : len(GuestNamePrefix)+GuestTagLength]
}

// GuestDisplayName is how every surface renders a guest (design §4.1):
// "Guest: <label> · <tag>" when it has a label, the handle alone when not.
// It is rendered from the stored guest attribution, never from a name's
// text, so a permanent account cannot look like a guest.
func GuestDisplayName(label, handle string) string {
	if label == "" {
		return handle
	}
	return "Guest: " + label + " " + GuestLabelSeparator + " " + GuestTag(handle)
}

// IsReservedGuestName reports whether name begins with
// ReservedGuestNameKeyPrefix under simple case folding: "Guest", "GUEST-x"
// and "guests" are reserved, "gues" is not. It is the cheap wire check; the
// server also refuses a name whose confusable skeleton begins with the
// prefix (a Cyrillic or Latin look-alike), through its name keys.
func IsReservedGuestName(name string) bool {
	n := len(ReservedGuestNameKeyPrefix)
	if utf8.RuneCountInString(name) < n {
		return false
	}
	runes := []rune(name)
	return strings.EqualFold(string(runes[:n]), ReservedGuestNameKeyPrefix)
}

// ValidateGuestLabel reports whether label is acceptable as a guest's
// display label (design §4.1): 1 to MaxGuestLabelLength characters that pass
// ValidateDisplayName, with no GuestLabelSeparator and no run of whitespace
// (either could fake a second tag), and not beginning with the reserved
// prefix (which could fake another visit's handle). The server additionally
// refuses a label whose folded key is a reserved administrative name or the
// server's name, and one whose confusable skeleton breaks these rules. NFC
// never lengthens a string in characters, so the bounds hold after the
// server normalizes. Errors never echo the input.
func ValidateGuestLabel(label string) error {
	if !utf8.ValidString(label) {
		return errors.New("schema: guest label must be valid UTF-8")
	}
	if n := utf8.RuneCountInString(label); n < 1 || n > MaxGuestLabelLength {
		return fmt.Errorf("schema: guest label must be 1 to %d characters", MaxGuestLabelLength)
	}
	if err := ValidateDisplayName("guest label", label, MaxGuestLabelLength); err != nil {
		return err
	}
	if strings.Contains(label, GuestLabelSeparator) {
		return errors.New("schema: guest label must not contain the separator ·")
	}
	prevSpace := false
	for _, c := range label {
		space := unicode.IsSpace(c)
		if space && prevSpace {
			return errors.New("schema: guest label must not contain a run of whitespace")
		}
		prevSpace = space
	}
	if IsReservedGuestName(label) {
		return errors.New("schema: guest label must not begin with the reserved guest prefix")
	}
	return nil
}

// GuestAttributionV1 is how a guest is rendered wherever a principal is
// shown: a channel member while the visit is live, and the author of every
// message posted during it, for ever. It is stored at the moment of
// admission or posting and never changes: a label and tag a post was made
// under stay on it through End visit, conversion and hide. Tag is derived
// from Handle by GuestTag and sent so that no client derives it differently.
type GuestAttributionV1 struct {
	Handle string `json:"handle"`
	Label  string `json:"label,omitempty"`
	Tag    string `json:"tag"`
}

// Validate reports whether the attribution is well-formed: a guest handle,
// its own tag, and a label that passes ValidateGuestLabel when present.
func (a GuestAttributionV1) Validate() error {
	if err := ValidateGuestHandle(a.Handle); err != nil {
		return err
	}
	if a.Tag != GuestTag(a.Handle) {
		return errors.New("schema: guest attribution tag must be the handle's tag")
	}
	if a.Label != "" {
		return ValidateGuestLabel(a.Label)
	}
	return nil
}

// GuestV1 is a visit as its own guest sees it: in GET /v1/whoami while the
// visit is live, in the join responses, and in the answer to End visit.
// ExpiresAt is the absolute deadline: the earlier of the server's hard
// bound and the entry invite's credential lifetime, fixed at admission.
// IdleExpiresAt is where the idle bound stands now; a REST request carrying
// the bearer moves it, at most once a minute, or once a quarter of the idle
// bound, whichever is shorter, and never past ExpiresAt. The visit ends at the earlier of the two. EndedAt and EndReason are present
// only once the visit is ended. CanConvert says whether Create an account is
// open to this visit now under its origin's policy, without a permit; a
// client offers the action when it is true, and the server decides again in
// the converting transaction.
type GuestV1 struct {
	State         GuestState     `json:"state"`
	Origin        GuestOrigin    `json:"origin"`
	Handle        string         `json:"handle"`
	Label         string         `json:"label,omitempty"`
	Tag           string         `json:"tag"`
	StartedAt     Timestamp      `json:"started_at"`
	ExpiresAt     Timestamp      `json:"expires_at"`
	IdleExpiresAt Timestamp      `json:"idle_expires_at"`
	EndedAt       *Timestamp     `json:"ended_at,omitempty"`
	EndReason     GuestEndReason `json:"end_reason,omitempty"`
	CanConvert    bool           `json:"can_convert"`
}

// Validate reports whether the visit is well-formed: a known state and
// origin, a handle with its own tag and an acceptable label, the three
// times, an end time and reason exactly when ended, and no end reason on a
// converted visit.
func (g GuestV1) Validate() error {
	if !g.State.Valid() {
		return fmt.Errorf("schema: guest state must be one of: %s", strings.Join(Vocabularies["GuestState"], ", "))
	}
	if !g.Origin.Valid() {
		return fmt.Errorf("schema: guest origin must be one of: %s", strings.Join(Vocabularies["GuestOrigin"], ", "))
	}
	if err := (GuestAttributionV1{Handle: g.Handle, Label: g.Label, Tag: g.Tag}).Validate(); err != nil {
		return err
	}
	if g.StartedAt.IsZero() || g.ExpiresAt.IsZero() || g.IdleExpiresAt.IsZero() {
		return errors.New("schema: guest started_at, expires_at and idle_expires_at are required")
	}
	if g.IdleExpiresAt.Time().After(g.ExpiresAt.Time()) {
		return errors.New("schema: guest idle_expires_at must not be after expires_at")
	}
	switch g.State {
	case GuestStateEnded:
		if g.EndedAt == nil || !g.EndReason.Valid() {
			return errors.New("schema: an ended guest carries ended_at and a known end_reason")
		}
	default:
		if g.EndedAt != nil || g.EndReason != "" {
			return fmt.Errorf("schema: a guest in state %s carries no ended_at or end_reason", g.State)
		}
	}
	return nil
}

// GuestAdminV1 is one visit as an operator lists it. InviteID is present for
// an invite-origin visit. LastActiveAt is the last REST request that carried
// the bearer, which is what the idle bound counts from. ConvertedAt is
// present once the guest created an account; the principal keeps its id, so
// the operator finds the member in the principal list by ID.
type GuestAdminV1 struct {
	ID            int64          `json:"id"`
	Handle        string         `json:"handle"`
	Label         string         `json:"label,omitempty"`
	Tag           string         `json:"tag"`
	State         GuestState     `json:"state"`
	Origin        GuestOrigin    `json:"origin"`
	InviteID      *int64         `json:"invite_id,omitempty"`
	StartedAt     Timestamp      `json:"started_at"`
	LastActiveAt  Timestamp      `json:"last_active_at"`
	ExpiresAt     Timestamp      `json:"expires_at"`
	IdleExpiresAt Timestamp      `json:"idle_expires_at"`
	EndedAt       *Timestamp     `json:"ended_at,omitempty"`
	EndReason     GuestEndReason `json:"end_reason,omitempty"`
	ConvertedAt   *Timestamp     `json:"converted_at,omitempty"`
}

// ListGuestsResponseV1 is one page of GET /v1/guests, newest first, filtered
// by ?state= (default active) and ?origin=. NextBefore is the id to pass as
// the next request's before parameter; present only when there may be more.
// Producers must send an empty slice, not nil.
type ListGuestsResponseV1 struct {
	Guests     []GuestAdminV1 `json:"guests"`
	NextBefore int64          `json:"next_before,omitempty"`
}

// EndGuestResponseV1 is the answer to an operator ending one visit: the
// visit as it now is.
type EndGuestResponseV1 struct {
	Guest GuestAdminV1 `json:"guest"`
}

// EndVisitResponseV1 is the answer to a guest's own End visit: the visit as
// it now is, ended, as the confirmation that the server terminated it. The
// credential the request came with no longer works.
type EndVisitResponseV1 struct {
	Guest GuestV1 `json:"guest"`
}

// EndGuestsRequestV1 is End all guest visits: it ends every live visit, or
// those of one origin, or those that came through one invite. DryRun is
// required, as for a bulk hide: true only lists them, false ends them, and a
// request that leaves it out is refused. Converted members are never
// selected: they are not guests. (The tag says omitempty only so a nil
// pointer is left out rather than sent as null; Validate requires it.)
type EndGuestsRequestV1 struct {
	DryRun   *bool       `json:"dry_run,omitempty"`
	Origin   GuestOrigin `json:"origin,omitempty"`
	InviteID int64       `json:"invite_id,omitempty"`
}

// Validate reports whether the request says whether it is a dry run, and
// that its selection, when it has one, is a known origin or a positive
// invite id; an invite id implies the invite origin.
func (r EndGuestsRequestV1) Validate() error {
	if r.DryRun == nil {
		return errors.New("schema: dry_run is required: true to list whose visits it would end, false to end them")
	}
	if r.Origin != "" && !r.Origin.Valid() {
		return fmt.Errorf("schema: origin must be one of: %s", strings.Join(Vocabularies["GuestOrigin"], ", "))
	}
	if r.InviteID < 0 {
		return errors.New("schema: invite_id must be a positive integer")
	}
	if r.InviteID > 0 && r.Origin == GuestOriginAddress {
		return errors.New("schema: invite_id selects invite-origin visits; origin cannot be address")
	}
	return nil
}

// GuestRefV1 names one visit a bulk end selected.
type GuestRefV1 struct {
	ID     int64  `json:"id"`
	Handle string `json:"handle"`
}

// EndGuestsResponseV1 is the answer of End all guest visits: how many visits
// were ended and which, or with DryRun how many and which would be. At most
// MaxEndGuests are acted on; Remaining is how many more matched, for the
// operator to repeat the call. Producers must send an empty slice, not nil.
type EndGuestsResponseV1 struct {
	DryRun    bool         `json:"dry_run"`
	Count     int          `json:"count"`
	Guests    []GuestRefV1 `json:"guests"`
	Remaining int          `json:"remaining"`
}

// JoinOptionsV1 is what an entry point offers right now, after every switch
// is applied: Guest is true when a visit may start here (guests_enabled, the
// entry point's mode admits guests, not paused, caps not reached) and
// Account when an account may be made here. GuestsFull says Guest is false
// only because a cap is reached, so a page can say "try later"; Paused says
// both are false because joining is paused. It carries no channel or person.
type JoinOptionsV1 struct {
	Guest      bool `json:"guest"`
	Account    bool `json:"account"`
	GuestsFull bool `json:"guests_full,omitempty"`
	Paused     bool `json:"paused,omitempty"`
}

// JoinInfoV1 is the body of GET /v1/join, the bare address's front door,
// open to anyone: the server's label, what the address offers now, whether
// password sign-in exists on this server, and the bounds a visit would
// have. Voice says whether bare-address joiners may use voice, still subject
// to channel access. Nothing else: no channel, member count or message.
type JoinInfoV1 struct {
	ServerName           string        `json:"server_name"`
	Join                 JoinOptionsV1 `json:"join"`
	PasswordLogin        bool          `json:"password_login"`
	Voice                bool          `json:"voice"`
	GuestIdleSeconds     int64         `json:"guest_idle_seconds"`
	GuestAbsoluteSeconds int64         `json:"guest_absolute_seconds"`
}

// JoinSettingsV1 is the server's joining policy (design §2), as GET
// /v1/join/settings answers it and PutJoinSettingsRequestV1 changes it.
// OpenChannels names the channels the bare address admits to; it must not
// be empty while OpenJoining is on. Producers send an empty slice, not nil.
type JoinSettingsV1 struct {
	OpenJoining          OpenJoiningMode `json:"open_joining"`
	GuestsEnabled        bool            `json:"guests_enabled"`
	JoiningPaused        bool            `json:"joining_paused"`
	JoinVoice            bool            `json:"join_voice"`
	MaxGuests            int64           `json:"max_guests"`
	MaxGuestsPerAddress  int64           `json:"max_guests_per_address"`
	GuestIdleSeconds     int64           `json:"guest_idle_seconds"`
	GuestAbsoluteSeconds int64           `json:"guest_absolute_seconds"`
	OpenChannels         []string        `json:"open_channels"`
}

// DefaultJoinSettings is the policy of a server nobody has changed: the
// bare address offers Sign in only and no entry point admits guests.
func DefaultJoinSettings() JoinSettingsV1 {
	return JoinSettingsV1{
		OpenJoining:          OpenJoiningOff,
		MaxGuests:            DefaultMaxGuests,
		MaxGuestsPerAddress:  DefaultMaxGuestsPerAddress,
		GuestIdleSeconds:     DefaultGuestIdleSeconds,
		GuestAbsoluteSeconds: DefaultGuestAbsoluteSeconds,
		OpenChannels:         []string{},
	}
}

// Validate reports whether the settings are consistent: a known mode;
// guest-only joining only with guests enabled; open joining only with at
// least one open channel; max_guests from 0 to MaxMaxGuests and the
// per-address cap from 1 to it; the idle bound from MinGuestIdleSeconds to
// the absolute bound, and the absolute bound at most MaxGuestLifetimeSeconds;
// and at most MaxOpenChannels distinct, valid channel names. Whether the
// channels exist, and whether a permanent name sits in the reserved
// namespace, is the server's to decide (ErrorCodeJoinPolicyConflict).
func (s JoinSettingsV1) Validate() error {
	if !s.OpenJoining.Valid() {
		return fmt.Errorf("schema: open_joining must be one of: %s", strings.Join(Vocabularies["OpenJoiningMode"], ", "))
	}
	if s.OpenJoining == OpenJoiningGuest && !s.GuestsEnabled {
		return errors.New("schema: open_joining cannot be guest while guests_enabled is false")
	}
	if s.OpenJoining != OpenJoiningOff && len(s.OpenChannels) == 0 {
		return errors.New("schema: open_joining needs at least one open channel")
	}
	if s.MaxGuests < 0 || s.MaxGuests > MaxMaxGuests {
		return fmt.Errorf("schema: max_guests must be between 0 and %d", MaxMaxGuests)
	}
	if s.MaxGuestsPerAddress < 1 || s.MaxGuestsPerAddress > MaxMaxGuests {
		return fmt.Errorf("schema: max_guests_per_address must be between 1 and %d", MaxMaxGuests)
	}
	if s.GuestAbsoluteSeconds < MinGuestIdleSeconds || s.GuestAbsoluteSeconds > MaxGuestLifetimeSeconds {
		return fmt.Errorf("schema: guest_absolute_seconds must be between %d and %d", MinGuestIdleSeconds, MaxGuestLifetimeSeconds)
	}
	if s.GuestIdleSeconds < MinGuestIdleSeconds || s.GuestIdleSeconds > s.GuestAbsoluteSeconds {
		return fmt.Errorf("schema: guest_idle_seconds must be between %d and guest_absolute_seconds", MinGuestIdleSeconds)
	}
	return validateChannelNames("open channel", s.OpenChannels, MaxOpenChannels)
}

// validateChannelNames checks a list of at most max distinct, valid channel
// names.
func validateChannelNames(field string, names []string, max int) error {
	if len(names) > max {
		return fmt.Errorf("schema: at most %d %ss, got %d", max, field, len(names))
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("schema: %s name must not be blank", field)
		}
		if err := ValidateDisplayName(field+" name", name, MaxChannelNameLength); err != nil {
			return err
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("schema: %s named twice", field)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// PutJoinSettingsRequestV1 is the body of PUT /v1/join/settings: the fields
// to change; one left out keeps its value. OpenChannels replaces the whole
// list when present; an empty list clears it. The server applies it with
// Apply, validates the result with JoinSettingsV1.Validate, and audits each
// field that changed, in one transaction.
type PutJoinSettingsRequestV1 struct {
	OpenJoining          *OpenJoiningMode `json:"open_joining,omitempty"`
	GuestsEnabled        *bool            `json:"guests_enabled,omitempty"`
	JoiningPaused        *bool            `json:"joining_paused,omitempty"`
	JoinVoice            *bool            `json:"join_voice,omitempty"`
	MaxGuests            *int64           `json:"max_guests,omitempty"`
	MaxGuestsPerAddress  *int64           `json:"max_guests_per_address,omitempty"`
	GuestIdleSeconds     *int64           `json:"guest_idle_seconds,omitempty"`
	GuestAbsoluteSeconds *int64           `json:"guest_absolute_seconds,omitempty"`
	OpenChannels         *[]string        `json:"open_channels,omitempty"`
}

// Validate reports whether the request changes something and every value it
// gives is of the right shape; the bounds and the consistency of the result
// are JoinSettingsV1.Validate's, on the applied settings.
func (r PutJoinSettingsRequestV1) Validate() error {
	if r.OpenJoining == nil && r.GuestsEnabled == nil && r.JoiningPaused == nil && r.JoinVoice == nil &&
		r.MaxGuests == nil && r.MaxGuestsPerAddress == nil && r.GuestIdleSeconds == nil &&
		r.GuestAbsoluteSeconds == nil && r.OpenChannels == nil {
		return errors.New("schema: the request changes no setting")
	}
	if r.OpenJoining != nil && !r.OpenJoining.Valid() {
		return fmt.Errorf("schema: open_joining must be one of: %s", strings.Join(Vocabularies["OpenJoiningMode"], ", "))
	}
	if r.OpenChannels != nil {
		return validateChannelNames("open channel", *r.OpenChannels, MaxOpenChannels)
	}
	return nil
}

// Apply returns the settings with the request's changes made. Turning
// guests off while the bare address is guest-only turns the address off as
// well, in the same change, as the design says (§2): the operator meant to
// stop guests, not to be refused; the server audits both. The receiver's
// OpenChannels slice is never modified.
func (r PutJoinSettingsRequestV1) Apply(s JoinSettingsV1) JoinSettingsV1 {
	out := s
	out.OpenChannels = append([]string(nil), s.OpenChannels...)
	if r.OpenJoining != nil {
		out.OpenJoining = *r.OpenJoining
	}
	if r.GuestsEnabled != nil {
		out.GuestsEnabled = *r.GuestsEnabled
	}
	if r.JoiningPaused != nil {
		out.JoiningPaused = *r.JoiningPaused
	}
	if r.JoinVoice != nil {
		out.JoinVoice = *r.JoinVoice
	}
	if r.MaxGuests != nil {
		out.MaxGuests = *r.MaxGuests
	}
	if r.MaxGuestsPerAddress != nil {
		out.MaxGuestsPerAddress = *r.MaxGuestsPerAddress
	}
	if r.GuestIdleSeconds != nil {
		out.GuestIdleSeconds = *r.GuestIdleSeconds
	}
	if r.GuestAbsoluteSeconds != nil {
		out.GuestAbsoluteSeconds = *r.GuestAbsoluteSeconds
	}
	if r.OpenChannels != nil {
		out.OpenChannels = append([]string{}, (*r.OpenChannels)...)
	}
	if r.GuestsEnabled != nil && !*r.GuestsEnabled && r.OpenJoining == nil && out.OpenJoining == OpenJoiningGuest {
		out.OpenJoining = OpenJoiningOff
	}
	if out.OpenChannels == nil {
		out.OpenChannels = []string{}
	}
	return out
}

// JoinSettingsResponseV1 is the answer of GET and PUT /v1/join/settings: the
// settings, the server's label, how many visits are live, and the warnings
// that apply (review F12, F13). Producers must send an empty slice, not nil.
type JoinSettingsResponseV1 struct {
	Settings   JoinSettingsV1 `json:"settings"`
	ServerName string         `json:"server_name"`
	LiveGuests int64          `json:"live_guests"`
	Warnings   []JoinWarning  `json:"warnings"`
}

// JoinGuestRequestV1 is the body of POST /v1/join/guest: Join as guest from
// the bare address. Label is the optional display label (design §4.1). No
// name: the server makes the handle. The request is rate-limited as invite
// redemption is, and refused while guests_enabled is off or the address
// admits no guests (ErrorCodeGuestsDisabled).
type JoinGuestRequestV1 struct {
	Label string `json:"label,omitempty"`
}

// Validate reports whether the label, when given, passes ValidateGuestLabel.
func (r JoinGuestRequestV1) Validate() error {
	if r.Label == "" {
		return nil
	}
	return ValidateGuestLabel(r.Label)
}

// JoinGuestResponseV1 is the response to starting a visit, from the bare
// address or an invite: the new principal (its name is the handle), the
// visit, its one credential, and the channels it was admitted to. Token is
// the plaintext bearer token, shown only here; a client keeps it in
// tab-scoped storage, never in a URL or member storage.
type JoinGuestResponseV1 struct {
	Principal  PrincipalV0       `json:"principal"`
	Guest      GuestV1           `json:"guest"`
	Credential CredentialV1      `json:"credential"`
	Token      string            `json:"token"`
	Channels   []InviteChannelV1 `json:"channels"`
}

// JoinAccountRequestV1 is the body of POST /v1/join/account: Create an
// account from the bare address, name and password together (design §3.1).
// Refused while open_joining admits no accounts (ErrorCodeJoiningClosed) or
// is paused (ErrorCodeJoiningPaused).
type JoinAccountRequestV1 struct {
	Name     string `json:"name"`
	Password string `json:"password"`
}

// Validate reports whether the name is acceptable as a permanent human
// principal name, outside the reserved guest namespace, and the password
// passes ValidatePassword.
func (r JoinAccountRequestV1) Validate() error {
	if err := validatePermanentName(r.Name); err != nil {
		return err
	}
	return ValidatePassword(r.Password)
}

// validatePermanentName is the rule for a name a person chooses for a
// permanent account: a human principal name that is not reserved for
// guests. That it is not taken, not hidden-reserved and not a confusable of
// a guest name is the store's to decide.
func validatePermanentName(name string) error {
	if err := (CreatePrincipalRequest{Kind: PrincipalHuman, Name: name}).Validate(); err != nil {
		return err
	}
	if IsReservedGuestName(name) {
		return errors.New("schema: a principal name must not begin with the reserved guest prefix")
	}
	return nil
}

// JoinAccountResponseV1 is the response to creating an account from the
// bare address: the new principal, its first credential and the channels it
// joined. Token is the plaintext bearer token, shown only here.
type JoinAccountResponseV1 struct {
	Principal  PrincipalV0       `json:"principal"`
	Credential CredentialV1      `json:"credential"`
	Token      string            `json:"token"`
	Channels   []InviteChannelV1 `json:"channels"`
}

// ConvertGuestRequestV1 is the body of POST /v1/guest/convert: Create an
// account during a live visit (design §3.3). Name is the permanent username
// the guest chooses; Password its password. Permit is an operator-issued
// conversion permit (a sign-in link of purpose conversion, bound to this
// principal), given when the origin's policy is closed; without one the
// policy decides. The caller must be the guest: an operator cannot convert
// someone else.
type ConvertGuestRequestV1 struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Permit   string `json:"permit,omitempty"`
}

// Validate reports whether the name and password are acceptable, as for
// JoinAccountRequestV1, and the permit, when given, has a sign-in link
// code's shape.
func (r ConvertGuestRequestV1) Validate() error {
	if err := validatePermanentName(r.Name); err != nil {
		return err
	}
	if err := ValidatePassword(r.Password); err != nil {
		return err
	}
	if r.Permit != "" {
		if err := ValidateSignInLinkCode(r.Permit); err != nil {
			return errors.New("schema: malformed conversion permit")
		}
	}
	return nil
}

// CreateConversionPermitRequestV1 is the body of POST
// /v1/guests/{id}/conversion-permit: an operator lets one live guest create
// an account although its entry point's policy is closed (design §3.3).
// The permit is a sign-in link of purpose conversion, bound to the guest;
// the response is CreateSignInLinkResponseV1, whose code the operator
// passes to the guest, who sends it as ConvertGuestRequestV1.Permit. It
// clears nothing: the guest still sets a name and password. ExpiresAt is
// optional: the server applies EffectiveExpiry. A principal that is not a
// live guest is refused (ErrorCodeNotAGuest, ErrorCodeGuestEnded).
type CreateConversionPermitRequestV1 struct {
	ExpiresAt *Timestamp `json:"expires_at,omitempty"`
}

// EffectiveExpiry is the expiry the permit gets: the requested one, or
// DefaultRestrictedLinkLifetime from now.
func (r CreateConversionPermitRequestV1) EffectiveExpiry(now time.Time) time.Time {
	if r.ExpiresAt != nil {
		return r.ExpiresAt.Time()
	}
	return now.Add(DefaultRestrictedLinkLifetime)
}

// Validate reports whether the expiry, when present, is in the future and
// within MaxRestrictedLinkLifetime.
func (r CreateConversionPermitRequestV1) Validate(now time.Time) error {
	return validateLinkExpiry(r.ExpiresAt, now, MaxRestrictedLinkLifetime)
}

// ConvertGuestResponseV1 is the response to a conversion: the principal,
// same id, under its permanent name, and the fresh member credential that
// replaces the guest's. Token is the plaintext bearer token, shown only
// here; the guest token no longer works. The guest's memberships, with
// their history bounds, are kept and nothing is added; GET /v1/whoami then
// shows a member.
type ConvertGuestResponseV1 struct {
	Principal  PrincipalV0  `json:"principal"`
	Credential CredentialV1 `json:"credential"`
	Token      string       `json:"token"`
}
