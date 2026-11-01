package schema

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// The invite family's second version (ADR-013 decision 5, issue #978,
// docs/design/community-model.md §3.3). An invite is now a link into the
// community: a member link makes a member who starts in the default
// channels. It carries no per-channel grant, no voice and no right to
// invite. The bump is signed off by ADR-013 (Nick, 2026-10-30): InviteV1's
// expires_at cannot say "none" and CreateInviteRequestV1 requires named
// channels, so neither can describe a member link without mutating a
// published shape. InviteV1 and its requests are unchanged and keep
// describing invites made before the community migration; the v1 routes
// keep answering legacy rows and project v2 links into v1 shapes.
//
// No guests for now (ADR-013 amendment of 2026-10-30; design §0.1): the
// guest-link variant of v2 is not declared, reserved or refused by name.
// InviteKindV2 has the one value member, and no v2 shape has a field only a
// guest link uses. A request naming kind "guest" is 400 invalid_request, as
// for any value outside the vocabulary. The later issue (#1004) adds the
// kind and its optional fields as additions, with no version bump.
//
// Like the v1 shapes, these are additive REST bodies with no schema-name
// field, versioned by the V2 type suffix. Redemption bodies are unchanged:
// RedeemInviteRequestV2 at POST /v2/invites/redeem answers
// RedeemInviteResponseV1 for a member link and for a legacy invite alike.

// InviteKindV2 is what a link makes. The vocabulary is closed and
// client-tolerant: a client shows a value it does not know by its value, so
// a later kind is an addition, not a bump.
type InviteKindV2 string

const (
	// InviteKindMember: makes a member, who starts in the default channels
	// (or the link's landing channels). Any member holding make_member_links
	// (ADR-014; on by default) with the may_invite brake on may make one.
	InviteKindMember InviteKindV2 = "member"
)

// Valid reports whether k is a recognized v2 invite kind.
func (k InviteKindV2) Valid() bool {
	switch k {
	case InviteKindMember:
		return true
	default:
		return false
	}
}

const (
	// DefaultMemberLinkExpirySeconds is the default of the server's
	// member_link_default_expiry_seconds setting (JoinSettingsV1): a member
	// link that names no expiry lasts this long, 7 days (O3). The setting
	// may be 0, no default expiry, and a caller holding manage_members may
	// make a link that never expires (CreateInviteRequestV2.NeverExpires).
	DefaultMemberLinkExpirySeconds int64 = 7 * 24 * 60 * 60
	// MaxMemberLinkExpirySeconds bounds how far ahead a member link's
	// expires_at may be set, and the member_link_default_expiry_seconds
	// setting: one year. A link that should outlive it is a never_expires
	// link, which only a caller holding manage_members may ask for, so a
	// far-future expires_at cannot stand in for one (security review of
	// #978, S1). The design names no bound; a year is the choice made here.
	MaxMemberLinkExpirySeconds int64 = 365 * 24 * 60 * 60
	// NoExpiryMillis is how "never expires" is stored in the NOT NULL
	// invites.expires_at column: 9999-12-31T23:59:59Z in Unix milliseconds.
	// A reader treats any value at or past it as no expiry; the v2 wire
	// omits expires_at then, and the v1 projection shows the sentinel.
	NoExpiryMillis int64 = 253402300799000
)

// ErrorCodeInviteKindRetired (HTTP 409): POST /v1/invites after the
// community migration, and a v2 request naming a v1 kind. Nothing is made;
// the v1 list, preview and close routes still work (community-model §3.3).
const ErrorCodeInviteKindRetired = "invite_kind_retired"

// InviteV2 describes one link. It never carries the code or its hash.
//
// ExpiresAt and MaxUses are absent when unbounded; ClosedAt while the link
// is open. Uses counts redemptions; Joiners is how many members joined
// through it and are still known to the reader, so a maker sees what their
// link did without a second call. CreatedByName is the maker's name as it is
// now.
//
// Channels is present on a legacy invite only: the channels it still grants
// beside the default channels. LandingChannels is present on a member link
// whose maker named the channels it lands in, in place of the default
// channels (O5, ADR-014: manage_channels). Either list leaves out the
// channels the reader is not in when the reader does not hold
// manage_channels, and OtherChannels then says how many were left out, so
// the link does not seem to grant less than it does; it is omitted when 0.
//
// Legacy is true for an invite made before the community migration (kind
// personal or open under InviteV1) whose join_as is account; it is shown as
// a member link. A legacy invite that admitted guests was closed by the
// migration and appears in no v2 answer; the v1 routes still show it.
type InviteV2 struct {
	ID                        int64             `json:"id"`
	Kind                      InviteKindV2      `json:"kind"`
	CreatedBy                 int64             `json:"created_by"`
	CreatedByName             string            `json:"created_by_name"`
	CreatedAt                 Timestamp         `json:"created_at"`
	ExpiresAt                 *Timestamp        `json:"expires_at,omitempty"`
	MaxUses                   *int64            `json:"max_uses,omitempty"`
	Uses                      int64             `json:"uses"`
	Joiners                   int64             `json:"joiners"`
	ClosedAt                  *Timestamp        `json:"closed_at,omitempty"`
	Channels                  []InviteChannelV1 `json:"channels,omitempty"`
	LandingChannels           []InviteChannelV1 `json:"landing_channels,omitempty"`
	OtherChannels             int64             `json:"other_channels,omitempty"`
	CredentialLifetimeSeconds int64             `json:"credential_lifetime_seconds"`
	Legacy                    bool              `json:"legacy,omitempty"`
}

// CreateInviteRequestV2 is the body of POST /v2/invites. Kind is required.
// ExpiresAt absent means the server's member_link_default_expiry_seconds
// counted from now (none when that setting is 0); when present it is at
// most MaxMemberLinkExpirySeconds ahead, so it can never say "never".
// NeverExpires asks for no expiry: the store must refuse it unless the
// caller holds manage_members (ADR-014), since Validate cannot know the
// caller; it is the one way to a link without an expiry. MaxUses absent is
// no cap. CredentialLifetimeSeconds is as in v1: the joiner's first
// credential lives this long, DefaultInviteCredentialLifetimeSeconds when
// absent.
//
// LandingChannels, when present, names the channels the link lands its
// joiner in, in place of the default channels (O5): 1 to MaxInviteChannels
// distinct channel names, of any visibility. Only a caller holding
// manage_channels (ADR-014) may set it; the server checks the names and the
// permission at creation and again at redemption, and a landing membership
// never carries can_decide or manages. It must not become an oracle on
// private channel names: a caller without manage_channels who names a
// channel they are not in gets the same answer whether that channel is
// private or does not exist (invite_grant_exceeds_inviter, "a channel you
// are not in", admin-roles §7.3), decided before any lookup whose answer
// could differ between the two. It is not the v1 channels grant: a member
// link grants nothing per channel. (The name channels is kept for the
// guest-link field of #1004, which means something else.)
//
// The server decodes the body strictly, so a channels or voice field, which
// no member link has, is refused as unknown.
type CreateInviteRequestV2 struct {
	Kind                      InviteKindV2 `json:"kind"`
	ExpiresAt                 *Timestamp   `json:"expires_at,omitempty"`
	NeverExpires              bool         `json:"never_expires,omitempty"`
	MaxUses                   *int64       `json:"max_uses,omitempty"`
	CredentialLifetimeSeconds *int64       `json:"credential_lifetime_seconds,omitempty"`
	LandingChannels           []string     `json:"landing_channels,omitempty"`
}

// Validate reports whether the request is well-formed at the instant now:
// the kind is known (so "guest" is refused as any unknown value is);
// expires_at, when present, is in the future, at most
// MaxMemberLinkExpirySeconds ahead and before the NoExpiryMillis sentinel,
// so that only never_expires can ask for a link without an expiry;
// never_expires is not combined with expires_at; max_uses, when present, is
// 1 to MaxInviteMaxUses; the credential lifetime, when present, is 1 to
// MaxInviteCredentialLifetimeSeconds, as in v1; and landing_channels, when
// present, is 1 to MaxInviteChannels distinct, valid channel names. Whether
// the caller may ask for never_expires (manage_members) or name landing
// channels (manage_channels), and whether those channels exist, is the
// store's rule.
func (r CreateInviteRequestV2) Validate(now time.Time) error {
	if !r.Kind.Valid() {
		return fmt.Errorf("schema: invite kind must be one of: %s", strings.Join(Vocabularies["InviteKindV2"], ", "))
	}
	if r.ExpiresAt != nil {
		if r.NeverExpires {
			return errors.New("schema: never_expires cannot be combined with expires_at")
		}
		exp := r.ExpiresAt.Time()
		if !exp.After(now) {
			return errors.New("schema: invite expires_at must be in the future")
		}
		// The sentinel first: a time at or past it reads as no expiry, and
		// the message must not suggest a nearer date would be "never".
		if exp.UnixMilli() >= NoExpiryMillis {
			return errors.New("schema: invite expires_at cannot mean never; ask never_expires")
		}
		if exp.After(now.Add(time.Duration(MaxMemberLinkExpirySeconds) * time.Second)) {
			return fmt.Errorf("schema: invite expires_at must be within %d seconds", MaxMemberLinkExpirySeconds)
		}
	}
	if r.MaxUses != nil {
		if *r.MaxUses < 1 || *r.MaxUses > MaxInviteMaxUses {
			return fmt.Errorf("schema: invite max_uses must be between 1 and %d", MaxInviteMaxUses)
		}
	}
	if r.CredentialLifetimeSeconds != nil {
		if s := *r.CredentialLifetimeSeconds; s < 1 || s > MaxInviteCredentialLifetimeSeconds {
			return fmt.Errorf("schema: invite credential_lifetime_seconds must be between 1 and %d", MaxInviteCredentialLifetimeSeconds)
		}
	}
	if r.LandingChannels != nil {
		if len(r.LandingChannels) == 0 {
			return fmt.Errorf("schema: landing_channels names 1 to %d channels, got 0", MaxInviteChannels)
		}
		if err := validateChannelNames("landing channel", r.LandingChannels, MaxInviteChannels); err != nil {
			return err
		}
	}
	return nil
}

// EffectiveCredentialLifetime is how long the joiner's first credential
// lives: the requested lifetime, or DefaultInviteCredentialLifetimeSeconds.
func (r CreateInviteRequestV2) EffectiveCredentialLifetime() time.Duration {
	if r.CredentialLifetimeSeconds != nil {
		return time.Duration(*r.CredentialLifetimeSeconds) * time.Second
	}
	return DefaultInviteCredentialLifetimeSeconds * time.Second
}

// CreateInviteResponseV2 is the response to POST /v2/invites. Code is the
// plaintext invite code, shown only here, as in v1.
type CreateInviteResponseV2 struct {
	Invite InviteV2 `json:"invite"`
	Code   string   `json:"code"`
}

// ListInvitesResponseV2 is the body of GET /v2/invites: a maker's own links,
// or every link for a caller holding manage_members. Invites is always a
// JSON array, never null: producers send an empty slice.
type ListInvitesResponseV2 struct {
	Invites []InviteV2 `json:"invites"`
}

// InvitePreviewV2 is what a holder of a code learns before using it (POST
// /v2/invites/preview, exempt from authentication and rate-limited as the
// v1 preview is). For a member link, Channels are the channels the joiner
// would land in as they are now: the default channels, or the link's
// landing channels; for a legacy invite, the channels it grants (its own
// that its maker still holds, plus the defaults). A landing channel may be
// private, and its name is then shown to whoever holds the code: by design,
// since the maker put it on the link and redemption admits the holder to it
// anyway; a code is handed out as a secret. ExpiresAt is absent when
// the link never expires. Join is what the link offers now after the
// server's switches (Join.Guest is always false while guests are removed).
// Nothing else: no counts, no members, no messages.
type InvitePreviewV2 struct {
	Kind       InviteKindV2  `json:"kind"`
	ServerName string        `json:"server_name"`
	Inviter    string        `json:"inviter"`
	Channels   []string      `json:"channels"`
	ExpiresAt  *Timestamp    `json:"expires_at,omitempty"`
	Join       JoinOptionsV1 `json:"join"`
}
