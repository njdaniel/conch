# The community model: one community, private channels

Decision: [ADR-013](../adr/ADR-013-one-community-model.md) (proposed, awaiting Nick's
sign-off). This note is the design behind it: the model, every decision mapped to
behaviour, the wire shapes as they would appear in `pkg/schema`, the permissions, the
privacy model of each read path, the migration, what is retired, the impact on work in
flight, the implementation issues, the test plan, and the points Nick still has to settle.

**Status: proposed, 2026-10-29.** Facts about main are as of `671a16e`. No code, schema
file, migration or issue exists for this yet. Nothing here is implemented behaviour.

## 0. Why

On 2026-10-29 Nick's first real tester got lost. Nick had made a per-channel invite to
#belgium; the tester, on a phone, joined through the bare address instead. They landed in
#general only, saw no earlier messages (an address join sets a history bound), could not
see Nick (who was in voice in #belgium, a channel the tester was not in), and had no voice
(address joins carry `join_voice`, off). No route changes a person's voice after joining,
so the fix was a manual database edit.

The model built under ADR-008 and ADR-009 is "several separate groups on one server,
nobody sees anyone outside their channels, every entry point grants exactly what its maker
chose". That is the right model for a server shared by strangers and the wrong one for a
community of people who know each other. Nick decided, in session, to replace it with one
community with private channels, Discord-like. His decisions are recorded verbatim in
ADR-013 and mapped to behaviour in §2 here.

## 1. The model in one page

A server is **one community**. Everyone with an account is a **member** of the community
and can see **every other member** (people and agents, agents marked as such). Members
start in the **default channels** the operators chose, see the **public channels** in a
directory and join them themselves, and can **create channels**, public or private. A
**private channel** is visible only to its members; its managers and operators add people;
to a non-member it does not exist (the same 404 as today). Members see the **full history**
of every channel they are in, however they got there. **Voice is on for every member**;
an operator can switch it off or on for any person from the member list.

The **one way in** for members is a **member link**: reusable, with an optional expiry and
use limit, made by any member and tied to its maker, so operators can see who brought whom,
switch the link off, or remove the people it brought in. The **bare address** is a
**sign-in page**; **open sign-up** is a switch, off by default, that only operators (owners,
per ADR-010) turn on, and when it is on the address works like a member link.

**Guests** are off by default and are **visitors**: they come through a **guest link** that
only operators make, visit the public channels the link names, are marked as guests there,
see history only from their arrival, get voice only if the link says so, cannot browse the
member list, are never in private channels, and their visit is time-limited as today.

What stays: single tenancy, no DMs, no E2EE, the deployment invariant, channel membership
as the content boundary, the approval right (`can_decide`), agent manifests, owner and
channel manager (ADR-010), passwords and guest identity (ADR-009), audit of every change.

What goes: per-channel invites as the way in, the four `open_joining` modes, `join_voice`,
`may_invite` as a grant (it becomes a brake), history bounds on members.

## 2. Nick's decisions, mapped to behaviour

Numbering follows the brief and ADR-013.

### 2.1 Joining

| Decision | Behaviour |
| --- | --- |
| The member link is the one way in for members. It makes you a member and puts you in the default channels. Reusable, optional expiry and use limit. | `POST /v2/invites` with `kind: "member"`. Redeemed with the existing `POST /v2/invites/redeem` (name and password). The joiner becomes a human `member` principal with `voice_allowed = 1`, `may_invite = 1`, a membership (no history bound, `can_decide = 0`) of every channel that is a default channel **at the moment of redemption**, and a credential of the link's lifetime (default 90 days, renewable as today). `expires_at` absent means no expiry; `max_uses` absent means unlimited. The link stays usable until it expires, is used up, is closed, or its maker is disabled or loses `may_invite` (`makerStillAllowed`, as today). |
| The bare address is invite-only by default: a sign-in page only. | `GET /v1/join` answers `join.account = false`, `join.guest = false` while `open_signup` is off. The browser's lobby shows Sign in and the one line "This server is invite-only." `POST /v1/join/account` answers 403 `joining_closed`; `POST /v1/join/guest` answers 403 `guests_disabled` always (the bare address admits no guests in this model, whatever `guests_enabled` says). |
| Open sign-up is a switch, off by default, that only operators can turn on. When on, the address works like a member link. | `JoinSettingsV2.open_signup` (default false), set through `PUT /v2/join/settings`, which stays in the `accessOwner` class ADR-010 put server-wide policy in (open point O1 asks Nick whether he meant every operator). While on, `POST /v1/join/account` creates the same member a member link does: default channels, no bound, voice on, `may_invite` on, 90-day credential. It needs at least one default channel (409 `join_policy_conflict` otherwise). `joining_paused` still pauses it. |
| Any member can make member links. Each link is tied to whoever made it, so you can see who brought whom, switch the link off, or remove the people it brought in. | `POST /v2/invites` is open to every enabled, non-guest human whose `may_invite` brake is on (it is on by default for every member; an operator can switch it off per person with the existing `PUT /v1/principals/{id}/invite-permission`). The maker's id is on the link (`created_by`) and on every joiner (`principals.invited_by`, `invite_id`, as today). The maker sees their own links (`GET /v2/invites`), their joiners (`GET /v2/invites/{id}/joiners`), and closes their own (`DELETE /v1/invites/{id}`, unchanged). Operators see every link and every joiner, close any, and remove the people a link brought in with the existing `POST /v1/invites/{id}/disable-joiners` and `POST /v1/principals/{id}/disable-invitees`. Whether a **member** may remove the people their link brought in is open point O2 (recommended: no). Outstanding open member links per member are capped by the existing `--invite-allowance` (default 5). |
| Only operators make guest links, or turn on open sign-up. | `POST /v2/invites` with `kind: "guest"` is refused for a non-operator with 403 `invite_not_allowed`. Open sign-up: owner class, above. |

### 2.2 Guests

| Decision | Behaviour |
| --- | --- |
| Off by default. | `guests_enabled` stays, default false. A guest link can be made while it is off (as today, its preview and redemption say guests are unavailable). |
| Visitors only, marked as guests in the channels they visit. | A guest link names 1 to 32 **public** channels. Redemption (`POST /v1/invites/redeem-guest`, unchanged body) admits the guest to those channels and nothing else: not the default channels. Guests are rendered from `GuestAttributionV1` as today. |
| Never in private channels. | The store refuses a guest link that names a private channel (409 `channel_settings_conflict`), an operator adding a guest to a private channel (403 `guest_not_allowed`), and a channel turning private while it has guests: that change removes the guests from the channel in the same transaction, audited, and the response says how many (`ChannelSettingsResponseV1.guests_removed`). |
| They can't browse the server member list. | `GET /v1/directory/members` answers a guest 403 `guest_not_allowed`. `GET /v1/directory/channels` likewise: a guest's channels are the link's. A guest still reads the member list of a channel it visits (today's rule). |
| They see history only from when they joined. | `history_after` is set at admission, as today; every read surface keeps the bound. Unchanged. |
| They get voice only if the guest link says so. | `CreateInviteRequestV2.voice` on a guest link sets the guest's `voice_allowed`; an operator can still flip it with the new voice route (§2.5). |
| Visits stay time-limited. | Idle and absolute bounds, End visit, End all guest visits, conversion: unchanged (ADR-009). Conversion keeps the principal id and memberships and now **clears the history bounds** (decision 4; §2.4). |

### 2.3 The member list

| Decision | Behaviour |
| --- | --- |
| Every member can see everyone on the server. | New route `GET /v1/directory/members`: every enabled, non-hidden principal that is not in an active guest visit, as `ServerMemberV1` (id, kind, name, role, created_at). Members and operators may call it; guests and agents are refused (§5). |
| Agents (bots) are listed and marked as agents. | `ServerMemberV1.kind = "agent"`. No role for agents. |
| Guests appear only in the channels they're visiting. | Excluded from the directory; present in `GET /v1/channels/{channel}/members` of their channels with `guest` set, as today. |
| A private channel's membership and contents stay members-only. | The directory says who is on the server, never which channels anyone is in. Channel membership is read only through the channel's own routes, which answer a non-member 404. |

### 2.4 History

| Decision | Behaviour |
| --- | --- |
| Members see the full history of every channel they're in, however they got there. | Every path that gives a **member** a membership writes `history_after = NULL`: member-link redemption, open sign-up, self-join of a public channel, an operator's or manager's add, migration into a default channel. Conversion of a guest sets `history_after = NULL` on every membership it keeps (a change to ADR-009 D3 and to guests-and-accounts.md §3.3/§4.3; ADR-013 records it). Migration 37 clears every existing member bound (§6). |
| Guests only from their join. | Unchanged. |

### 2.5 Voice

| Decision | Behaviour |
| --- | --- |
| On for every member by default. | New members get `voice_allowed = 1` on every path. Migration sets it to 1 for every existing non-guest human (§6 explains why that is safe: no route ever set it to 0 on purpose). |
| An operator can switch it off or on for any person, at any time, from the member list. This needs a new route. | `PUT /v1/principals/{id}/voice-permission` body `VoicePermissionRequestV1{voice_allowed}`, `accessOperator`, owner-protected target (ADR-010: an operator cannot act on an owner), recent-authentication class. Turning it off evicts the person's `p<id>`, `p<id>.screen` and `p<id>.view` from every room at once through the existing entitlement path (`voiceStillEntitled`, reason `voice_not_allowed`) and ends any share they hold; turning it on changes nothing live. Audited `voice_permission_changed`. The directory carries `voice_allowed` to operators so the control sits on the member list. |

### 2.6 Channels

| Decision | Behaviour |
| --- | --- |
| Any member can create a channel, and chooses public or private. | New `POST /v1/channels` body `CreateChannelRequestV1{name, visibility}`, open to enabled non-guest humans (agents and guests 403). The creator becomes the channel's only member **and its manager** (`manages = 1`, `can_decide = 1`): a private channel someone else must be able to add people to would otherwise need an operator for its first member. This is the one consequence of decision 6 that ADR-010 did not foresee (it rejected "creators own their channels" because creation was operator-only); ADR-013 amends it. `POST /v0/channels` stays operator-only and makes a private channel. |
| Operators pick the default channels, which new members start in. | `PUT /v1/channels/{id}/settings` body `ChannelSettingsRequestV1{visibility?, default?}`, `accessOperator`. A default channel must be public (409 `channel_settings_conflict`). Marking a channel default does not add existing members (operators add them, or they join); unmarking removes nobody. At least one default channel must exist while `open_signup` is on (409 `join_policy_conflict` on the change that would leave none, as today for open channels). |
| Other public channels are listed, and members can join them themselves. | New `GET /v1/directory/channels`: every public channel plus the caller's private channels, each with `joined`, `default`, `visibility`, `manages`. `GET /v1/channels` keeps its meaning (the caller's channels) and gains `visibility` and `default`. New `POST /v1/channels/{channel}/join` (member only; public channel; `can_decide = 0`, no bound; refused by a channel ban, 403 `channel_not_joinable`, once moderation M3 lands) and `POST /v1/channels/{channel}/leave` (any member of the channel, including a manager; an operator too). |
| A private channel is visible only to its members, and its managers and operators add people. | The directory omits a private channel the caller is not in; every channel route answers a non-member 404 `channel_not_found`; self-join of a private channel is the same 404. Managers add people with the existing `PUT`/`POST …/members` routes. ADR-010's bound that a manager may add only someone who already shares a channel with it existed to keep the server's people hidden from managers; the directory now shows every member to every member, so the bound is dropped: a manager adds any enabled non-guest human member by id or exact name. Operators, owners and agents still cannot be added by a manager (unchanged). |

### 2.7 Migration

In §6. In short: every channel in `open_channels` becomes a public default channel; every
other existing channel becomes public; every existing member joins the default channels
with no history bound; every existing member bound is cleared (and kept aside in a table
for the record); existing invites keep working until they expire; voice and `may_invite`
are turned on for every member.

## 3. Wire shapes, as they would appear in `pkg/schema`

Every shape below is a REST body versioned by its type suffix, carries no schema-name
field, and follows the house rules: flat structs, string enums with `Valid()`, RFC 3339
`Timestamp`, explicit `Validate()`, unknown-field tolerance on the way in. Fixtures go in
`pkg/schema/testdata/`, vocabularies in `vocabularies.go`, TypeScript emission in `ts.go`.
The `schema-change` skill governs the PR.

### 3.1 Compatibility summary (for Nick)

| Change | Kind | Needs Nick |
| --- | --- | --- |
| `InviteV2`, `CreateInviteRequestV2`, `CreateInviteResponseV2`, `ListInvitesResponseV2`, `InvitePreviewV2`, `InviteKindV2`; routes `POST/GET /v2/invites`, `POST /v2/invites/preview`, `GET /v2/invites/{id}/joiners` | **Version bump** of the invite family: the meaning of an invite changes (no per-channel grant for members, two new kinds, optional expiry). `InviteV1` and its fixtures stay byte-identical; v1 routes keep answering legacy rows and project new ones (§3.3). | **Yes** |
| `JoinSettingsV2`, `PutJoinSettingsRequestV2`, `JoinSettingsResponseV2`; routes `GET/PUT /v2/join/settings` | **Version bump**: `open_joining`, `join_voice`, `open_channels` are retired; `open_signup` replaces the mode. v1 `GET` projects; v1 `PUT` is refused (409 `join_settings_retired`). | **Yes** |
| `POST /v1/invites` refused after migration 37 (409 `invite_kind_retired`) | Behaviour change on a published route, like ADR-009's B1 | **Yes** |
| `ChannelV0.visibility`, `ChannelV0.default`; `ChannelDocV1.visibility`, `.default` | Additive optional fields; existing fixtures unchanged | No |
| `CreateChannelRequestV1`, `CreateChannelResponseV1`, `ChannelVisibility`; route `POST /v1/channels` open to members | New types and route | No (route class of a **new** route) |
| `ChannelListingV1`, `ChannelDirectoryResponseV1`; `GET /v1/directory/channels` | New | No |
| `JoinChannelResponseV1`; `POST /v1/channels/{channel}/join`, `POST …/leave` | New | No |
| `ChannelSettingsRequestV1`, `ChannelSettingsResponseV1`; `PUT /v1/channels/{id}/settings` | New | No |
| `ServerMemberV1`, `ServerMembersResponseV1`; `GET /v1/directory/members` | New | No, but it reverses ADR-003's visibility boundary for *people*: that is ADR-013's decision 3 |
| `VoicePermissionRequestV1`, `VoicePermissionResponseV1`; `PUT /v1/principals/{id}/voice-permission` | New | No |
| `PrincipalAdminV1.voice_allowed`, `.may_invite` (optional) | Additive | No |
| `JoinWarning` value `no_default_channel` | Additive on a vocabulary documented as open ("a client shows one it does not know by its value") | No |
| Error codes `channel_not_joinable`, `channel_settings_conflict`, `invite_kind_retired`, `join_settings_retired`, `not_a_person` | Additive on `schema.Error.code` | No |
| `WhoAmIResponseV1.may_invite` true for every member; `invites_remaining` counts open member links | Semantics within the documented meaning ("whether the caller may make invites") | No |
| `ChannelMemberV1.history_after` absent for every member | Within the documented meaning | No |
| `JoinInfoV1.join.guest` always false; `.voice` always true | Within the documented meaning | No |
| `can_decide` on the creator of a channel (`POST /v1/channels`) and `can_decide = 0` on self-join; conversion clearing `history_after` | Membership-write transactions: **approval-path** PRs, merged by Nick | Yes (merge) |

### 3.2 Channels

```go
// channel.go (additions)

// ChannelVisibility says who can find a channel (ADR-013 decision 6). The
// vocabulary is closed.
type ChannelVisibility string

const (
	// ChannelPublic: listed to every member in the directory; any member
	// may join it. Guests may be admitted to it by a guest link or an
	// operator.
	ChannelPublic ChannelVisibility = "public"
	// ChannelPrivate: visible only to its members; its managers and
	// operators add people. To anyone else it does not exist.
	ChannelPrivate ChannelVisibility = "private"
)

func (v ChannelVisibility) Valid() bool { /* public, private */ }

// ChannelV0 gains two optional fields. A server from before migration 37
// omits both; a server with it always sends visibility.
type ChannelV0 struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Manages   bool      `json:"manages,omitempty"`
	// Visibility is public or private (ADR-013). Omitted by a server that
	// predates it, which a client reads as private: every channel then was.
	Visibility ChannelVisibility `json:"visibility,omitempty"`
	// Default is true when new members start in this channel. Omitted when
	// false. A default channel is always public.
	Default bool `json:"default,omitempty"`
}

// CreateChannelRequestV1 is the body of POST /v1/channels: a member makes a
// channel and chooses its visibility. The creator is its first member and
// its manager.
type CreateChannelRequestV1 struct {
	Name       string            `json:"name"`
	Visibility ChannelVisibility `json:"visibility"`
}

// Validate: Name as CreateChannelRequest.Validate; Visibility known.
func (r CreateChannelRequestV1) Validate() error

type CreateChannelResponseV1 struct {
	Channel ChannelV0 `json:"channel"`
}

// ChannelListingV1 is one row of the directory, GET /v1/directory/channels:
// a public channel, or a private channel the caller is in. Joined says
// whether the caller is a member; Manages whether they manage it (only when
// Joined). Nothing about other members: no counts, no names.
type ChannelListingV1 struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	CreatedAt  Timestamp         `json:"created_at"`
	Visibility ChannelVisibility `json:"visibility"`
	Default    bool              `json:"default,omitempty"`
	Joined     bool              `json:"joined"`
	Manages    bool              `json:"manages,omitempty"`
}

// ChannelDirectoryResponseV1: Channels is always an array, ordered by id.
type ChannelDirectoryResponseV1 struct {
	Channels []ChannelListingV1 `json:"channels"`
}

// JoinChannelResponseV1 answers POST /v1/channels/{channel}/join: the
// channel and the caller's new (or existing, when idempotent) membership.
type JoinChannelResponseV1 struct {
	Channel ChannelV0       `json:"channel"`
	Member  ChannelMemberV1 `json:"member"`
}

// ChannelSettingsRequestV1 is the body of PUT /v1/channels/{id}/settings
// (operator): the fields to change; one left out keeps its value. At least
// one must be present.
type ChannelSettingsRequestV1 struct {
	Visibility *ChannelVisibility `json:"visibility,omitempty"`
	Default    *bool              `json:"default,omitempty"`
}

func (r ChannelSettingsRequestV1) Validate() error // one field present; visibility known

// ChannelSettingsResponseV1 is the channel as it now is, and how many
// guests the change removed (a channel turning private keeps no guests).
type ChannelSettingsResponseV1 struct {
	Channel       ChannelV0 `json:"channel"`
	GuestsRemoved int       `json:"guests_removed"`
}

// ChannelDocV1 (events socket, conch.channel.v1) gains the same two optional
// fields and is now also sent when a channel's settings change.
```

Error codes (additive on `schema.Error.code`):

```go
// ErrorCodeChannelNotJoinable (403): the caller may not join this public
// channel themselves: a guest, an agent, or a principal banned from it
// (moderation M3). A private channel is never this: it is channel_not_found.
const ErrorCodeChannelNotJoinable = "channel_not_joinable"

// ErrorCodeChannelSettingsConflict (409): the settings asked for contradict
// each other or the server: a private default channel, a guest link naming a
// private channel, or unmarking the last default channel while open sign-up
// is on (that one is join_policy_conflict, the existing code, so a client
// shows the joining settings).
const ErrorCodeChannelSettingsConflict = "channel_settings_conflict"
```

### 3.3 Invites, version 2

```go
// invite_v2.go (new file). The invite family's second version (ADR-013
// decision 1). An invite is now a link into the community: a member link
// makes a member who starts in the default channels; a guest link admits a
// visitor to the public channels it names. Neither carries a per-channel
// grant for members, voice for members, or the right to invite. InviteV1 is
// unchanged and keeps describing invites made before migration 37.

// InviteKindV2 is what a link makes. The vocabulary is closed.
type InviteKindV2 string

const (
	// InviteKindMember: makes a member. Any member may make one.
	InviteKindMember InviteKindV2 = "member"
	// InviteKindGuest: admits a temporary guest to the public channels it
	// names, while guests_enabled is on. Operators only.
	InviteKindGuest InviteKindV2 = "guest"
)

func (k InviteKindV2) Valid() bool

const (
	// DefaultMemberLinkLifetime is applied when a member link names no
	// expiry and the server's default-expiry setting is on (open point O3).
	// Under the decision as written, a member link with no expires_at never
	// expires.
	MaxGuestLinkLifetime = 7 * 24 * time.Hour // as open links today
)

// InviteV2 describes one link. It never carries the code or its hash.
// ExpiresAt and MaxUses are absent when unbounded; ClosedAt while open.
// Channels and Voice are present on a guest link only. Joiners is how many
// principals joined through it (members, and guest visits for a guest
// link), so a maker sees what their link did without a second call.
// Legacy is true for an invite made before migration 37 (kind personal or
// open under InviteV1), shown as a member link: it still grants its named
// channels, listed in Channels, beside the default channels.
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
	Voice                     bool              `json:"voice,omitempty"`
	CredentialLifetimeSeconds int64             `json:"credential_lifetime_seconds"`
	Legacy                    bool              `json:"legacy,omitempty"`
}

// CreateInviteRequestV2 is the body of POST /v2/invites. ExpiresAt absent
// is no expiry; MaxUses absent is no cap. Channels and Voice are for a guest
// link only and refused on a member link. CredentialLifetimeSeconds as in v1.
type CreateInviteRequestV2 struct {
	Kind                      InviteKindV2 `json:"kind"`
	ExpiresAt                 *Timestamp   `json:"expires_at,omitempty"`
	MaxUses                   *int64       `json:"max_uses,omitempty"`
	CredentialLifetimeSeconds *int64       `json:"credential_lifetime_seconds,omitempty"`
	Channels                  []string     `json:"channels,omitempty"`
	Voice                     bool         `json:"voice,omitempty"`
}

// Validate(now): kind known; expires_at in the future when present, and
// within MaxGuestLinkLifetime for a guest link; max_uses 1..MaxInviteMaxUses
// when present; a guest link names 1..MaxInviteChannels distinct valid
// channel names and a member link names none and asks no voice; credential
// lifetime bounds as v1. Whether the channels are public, and whether the
// caller may make this kind, is the store's rule.
func (r CreateInviteRequestV2) Validate(now time.Time) error

type CreateInviteResponseV2 struct {
	Invite InviteV2 `json:"invite"`
	Code   string   `json:"code"` // shown only here, as today
}

type ListInvitesResponseV2 struct {
	Invites []InviteV2 `json:"invites"` // never null
}

// InvitePreviewV2 is what a holder of a code learns before using it (POST
// /v2/invites/preview, exempt, rate-limited as today). For a member link,
// Channels are the default channels as they are now; for a guest link, the
// link's channels. Join is what the link offers now after the server's
// switches (a guest link while guests are off offers nothing). Nothing
// else: no counts, no members, no messages.
type InvitePreviewV2 struct {
	Kind       InviteKindV2   `json:"kind"`
	ServerName string         `json:"server_name"`
	Inviter    string         `json:"inviter"`
	Channels   []string       `json:"channels"`
	Voice      bool           `json:"voice,omitempty"`
	ExpiresAt  *Timestamp     `json:"expires_at,omitempty"`
	Join       JoinOptionsV1  `json:"join"`
}

// ErrorCodeInviteKindRetired (409): POST /v1/invites after migration 37,
// and a v2 request naming a v1 kind. The v1 list, preview and close routes
// still work.
const ErrorCodeInviteKindRetired = "invite_kind_retired"
```

Redemption bodies are unchanged: `RedeemInviteRequestV2` (code, name, password) at
`POST /v2/invites/redeem` for a member link or a legacy invite; `RedeemInviteGuestRequestV1`
at `POST /v1/invites/redeem-guest` for a guest link (or a legacy invite whose `join_as`
admits guests). `RedeemInviteResponseV1` and `JoinGuestResponseV1` are unchanged;
`JoinGuestResponseV1.channels` lists the channels admitted to.

**What the v1 routes do after migration 37.**

| Route | Behaviour |
| --- | --- |
| `POST /v1/invites` | 409 `invite_kind_retired`, nothing made. |
| `GET /v1/invites` | Lists legacy invites as before. A v2 link is projected: kind `open`, `channels` = the default channels now (member) or the link's (guest), `voice` true (member) or the link's, `may_invite` false, `join_as` `account` or `guest`, `expires_at` = the link's or, for an unbounded link, the far-future sentinel `9999-12-31T00:00:00Z`. Lossy; old clients only. |
| `POST /v1/invites/preview` | The same projection into `InvitePreviewV1`. |
| `DELETE /v1/invites/{id}` | Unchanged: maker or operator closes any link, legacy or v2. |
| `GET /v1/invites/{id}/principals`, `POST …/disable-joiners`, `POST /v1/principals/{id}/disable-invitees`, `PUT …/invite-permission`, `GET/PUT /v1/invites/settings` | Unchanged. `invite-permission` now sets the brake (§7). |

New v2 administration route: `GET /v2/invites/{id}/joiners` (`accessAuthenticated`;
the maker or an operator; anyone else 404 `invite_not_found`) answers the existing
`InviteJoinersResponseV1`. This is how a member sees "who I brought". It lists members
only; a guest link's visits are in `GET /v1/guests?invite_id=`, operators only.

### 3.4 Joining settings, version 2

```go
// guest.go (additions)

// JoinSettingsV2 is the server's joining policy under ADR-013. OpenSignup
// replaces OpenJoiningMode: the bare address either is a sign-in page or
// makes members as a member link does. The bare address never admits
// guests; guests come through guest links. There is no join_voice (every
// member has voice) and no open channel set (the default channels, set per
// channel, are what a joiner starts in).
type JoinSettingsV2 struct {
	OpenSignup           bool  `json:"open_signup"`
	GuestsEnabled        bool  `json:"guests_enabled"`
	JoiningPaused        bool  `json:"joining_paused"`
	MaxGuests            int64 `json:"max_guests"`
	MaxGuestsPerAddress  int64 `json:"max_guests_per_address"`
	GuestIdleSeconds     int64 `json:"guest_idle_seconds"`
	GuestAbsoluteSeconds int64 `json:"guest_absolute_seconds"`
}

func DefaultJoinSettingsV2() JoinSettingsV2 // open_signup false, guests false, bounds as v1
func (s JoinSettingsV2) Validate() error    // the v1 bounds; open_signup needs a default channel (store)

// PutJoinSettingsRequestV2: pointer fields, one left out keeps its value;
// the request must change something.
type PutJoinSettingsRequestV2 struct {
	OpenSignup           *bool  `json:"open_signup,omitempty"`
	GuestsEnabled        *bool  `json:"guests_enabled,omitempty"`
	JoiningPaused        *bool  `json:"joining_paused,omitempty"`
	MaxGuests            *int64 `json:"max_guests,omitempty"`
	MaxGuestsPerAddress  *int64 `json:"max_guests_per_address,omitempty"`
	GuestIdleSeconds     *int64 `json:"guest_idle_seconds,omitempty"`
	GuestAbsoluteSeconds *int64 `json:"guest_absolute_seconds,omitempty"`
}

// JoinSettingsResponseV2 adds the default channels, which the settings page
// shows beside the switch they condition, and the live-guest count and
// warnings of v1.
type JoinSettingsResponseV2 struct {
	Settings        JoinSettingsV2    `json:"settings"`
	ServerName      string            `json:"server_name"`
	DefaultChannels []InviteChannelV1 `json:"default_channels"`
	LiveGuests      int64             `json:"live_guests"`
	Warnings        []JoinWarning     `json:"warnings"`
}

// JoinWarningNoDefaultChannel: no channel is a default channel, so a member
// link or open sign-up would make a member who is in nothing.
const JoinWarningNoDefaultChannel JoinWarning = "no_default_channel"

// ErrorCodeJoinSettingsRetired (409): PUT /v1/join/settings after migration
// 37. GET /v1/join/settings still answers, as a projection.
const ErrorCodeJoinSettingsRetired = "join_settings_retired"
```

`GET /v1/join/settings` projects: `open_joining` = `account` when `open_signup`, else
`off`; `join_voice` true; `open_channels` = the default channels' names; the rest as is.
`JoinInfoV1` keeps its shape: `join.guest` is always false, `join.account` follows
`open_signup` and the pause, `voice` is true.

### 3.5 The member directory

```go
// directory.go (new file)

// ServerMemberV1 is one entry of the community's member list, GET
// /v1/directory/members (ADR-013 decision 3): every enabled, non-hidden
// principal that is not in a guest visit, people and agents. Role is for
// humans; agents carry none. Owner, VoiceAllowed, InvitedBy and MayInvite
// are filled for an operator viewer only, so the controls that act on them
// sit on this list; a member sees id, kind, name, role and created_at and
// nothing more. Nothing says which channels anyone is in.
type ServerMemberV1 struct {
	ID           int64         `json:"id"`
	Kind         PrincipalKind `json:"kind"`
	Name         string        `json:"name"`
	Role         PrincipalRole `json:"role,omitempty"`
	CreatedAt    Timestamp     `json:"created_at"`
	Owner        bool          `json:"owner,omitempty"`
	VoiceAllowed *bool         `json:"voice_allowed,omitempty"`
	MayInvite    *bool         `json:"may_invite,omitempty"`
	InvitedBy    *int64        `json:"invited_by,omitempty"`
}

// ServerMembersResponseV1 is one page, ordered by id ascending; After is
// the id to pass as the next request's ?after=, present only when there may
// be more. ?limit= defaults to DefaultListPrincipalsLimit and is capped at
// MaxListPrincipalsLimit. Members is never null.
type ServerMembersResponseV1 struct {
	Members []ServerMemberV1 `json:"members"`
	After   int64            `json:"after,omitempty"`
}

func (d ServerMembersResponseV1) Validate() error // ids positive and distinct; names present; kinds known; agents carry no role
```

Whether `role` is shown to every member is open point O4 (recommended: yes, so people know
whom to ask; an operator's name is not a secret in a community).

A live `conch.directory_members.v1` events document (the whole list, like the roster) is a
follow-up (issue I10), not required for the first slice: the browser re-reads the
directory when it opens the panel and on every `whoami` refresh.

### 3.6 Voice permission

```go
// voice_permission.go (new file)

// VoicePermissionRequestV1 is the body of PUT /v1/principals/{id}/voice-permission
// (operator, recent authentication; ADR-013 decision 5): whether the person
// may use voice and screen share.
type VoicePermissionRequestV1 struct {
	VoiceAllowed bool `json:"voice_allowed"`
}

// VoicePermissionResponseV1: the state now, whether it changed, and how many
// LiveKit identities of the person (p<id>, p<id>.screen, p<id>.view) the
// change removed from rooms.
type VoicePermissionResponseV1 struct {
	PrincipalID  int64 `json:"principal_id"`
	VoiceAllowed bool  `json:"voice_allowed"`
	Changed      bool  `json:"changed"`
	Evicted      int   `json:"evicted"`
}

// ErrorCodeNotAPerson (409): the target is an agent, which never holds voice.
// A hidden or unknown id is 404 principal_not_found; an owner target for a
// non-owner operator is the owner-protection answer of ADR-010.
const ErrorCodeNotAPerson = "not_a_person"
```

`PrincipalAdminV1` gains `VoiceAllowed *bool` and `MayInvite *bool` (additive, operator
list), so `conch admin principal list` shows both.

### 3.7 MCP

No new tool. ADR-001's minimum set and the session tools are unchanged; administration,
joining and the directory are human acts and have no MCP form (parity holds: the browser,
CLI and TUI use the REST routes above). `read_channel` and `post_message` are unaffected:
membership remains the gate, and an agent is put into a channel by an operator as today.

## 4. Permissions

Owner ⊇ operator ⊇ manager (in its channel) ⊇ member ⊇ guest; agents by manifest; deny
by default; every check in the handler and again in the store's write transaction
(ADR-010 D7). "404" is the route's not-found answer; "403" is the stated code.

| Action | Member | Operator (owner noted) | Channel manager | Guest | Agent |
| --- | --- | --- | --- | --- | --- |
| Sign in at the bare address | yes | yes | yes | no (a visit is not an account) | no |
| Create an account at the bare address | only while `open_signup` | same | same | same | no |
| Make a member link | yes (brake `may_invite` on, allowance) | yes, no allowance | as member | 403 `guest_not_allowed` | 403 |
| See / close own links, see own joiners | yes | every link, every joiner | as member | no | no |
| Remove the people a link brought in | no (O2) | yes (`disable-joiners`, `disable-invitees`) | no | no | no |
| Make a guest link | 403 `invite_not_allowed` | yes | as member | no | no |
| Turn on open sign-up, `guests_enabled`, guest bounds | no | **owner** (ADR-010; O1) | no | no | no |
| Pause invites / joining; end guest visits | no | yes (brakes stay operator) | no | no | no |
| Set a person's `may_invite` brake | no | yes, not on an owner | no | no | no |
| Read the member directory | yes | yes, with `owner`, `voice_allowed`, `may_invite`, `invited_by` | as member | 403 `guest_not_allowed` | 403 |
| Read the channel directory | public + own private | same (an operator sees no private channel it is not in, as today) | as member | 403 | 403 |
| Join a public channel | yes | yes | as member | 403 `channel_not_joinable` | 403 |
| Leave a channel | yes | yes | yes (loses the grant) | no (End visit) | no |
| Create a channel (public or private) | yes; becomes its manager | yes | as member | 403 | 403 |
| Set a channel's visibility / default | no | yes | no (O5) | no | no |
| Add a person to a channel | no | yes, anyone addable | yes, any enabled non-guest human member; not operators, owners, agents | no | no |
| Add an agent to a channel | no | yes | no | no | no |
| Add a guest to a channel | no | public channels only | no | no | no |
| Remove a member | no | yes (owner protection) | ADR-010 rules | no | no |
| Rename a channel | no | yes | yes, limited | no | no |
| Set `can_decide`, `manages` | no | yes (approval-path) | no | no | no |
| Use voice / share | yes while `voice_allowed` | yes | as member | if the link said so | no |
| Switch a person's voice | no | yes, not on an owner; agents 409 | no | no | no |
| Read history of a channel they are in | all of it | all of it | all of it | from their arrival | all of it, by manifest |
| See a private channel they are not in | 404 | 404 | 404 | 404 | 404 |
| Approvals | `can_decide` as granted | as today | as today | never | by manifest |

## 5. The privacy model, by read path

**The server member list** (`GET /v1/directory/members`). Says: every member's id, kind,
name, role and when they joined, and that is all. Does not say: which channels anyone is
in, who is in voice, who is a guest, who is disabled or hidden, who invited whom (operators
excepted). Guests and agents are refused: a guest is a visitor and this is the community's
list; an agent's view of people is channel content under its manifest, as today. This is
the one deliberate change to ADR-003's rule that channel membership bounds what a person
can learn: **people are community-wide, channels are not.**

**The channel list** (`GET /v1/channels`, `GET /v1/directory/channels`). A member sees:
every public channel by name, with whether they are in it and whether it is a default
channel; and their own private channels. A member does not see: any private channel they
are not in, how many people are in any channel, or who. The directory carries no counts
on purpose: a count on a public channel is harmless, a count on a private one would be an
oracle, and one rule is easier to hold than two.

**A private channel's existence, membership and messages.** To a non-member nothing
exists: every route that names the channel, by name or by id, answers 404
`channel_not_found` with the same body it gives for a channel that was never made
(`writeChannelNotFound`, as today), including `POST …/join`, `PUT …/settings`,
`GET …/members`, every message, file, voice, net and read-state route, and the socket
upgrades. The directory omits it. Message, file and principal ids remain global sequences,
so id gaps still leak activity volume (#500, accepted). An operator who is not a member is
a non-member on content routes (today's rule) and sees the channel in no list; the
administration routes that take an id (`settings`, `name`) answer an operator for any
channel, as rename does today, because operators administer every channel. **What a
non-member of a private channel can learn: nothing, the same 404 as today.**

**Guests' view.** The channels the link named, with history from their arrival; those
channels' member lists (today's rule); voice there if granted; `whoami`; their own visit.
No member directory, no channel directory, no default channels, no private channel ever,
no links of their own, no account routes. Everything else is 403 `guest_not_allowed` or
the member's 404, as today.

**Agents.** Unchanged: channel content, including the channel's member list, by manifest;
no directory; no joining.

## 6. Migration

One SQLite migration, number 37 at `671a16e` (the reactions and moderation designs also
claim the next number; it is assigned at merge), additive in shape, with a Go step that
reports what it did. Rollback, as for every migration (private-alpha.md §4 O7): the
previous binary refuses a database at a newer `user_version`
(`store.go:1055`), so rolling back is the previous binary plus the backup taken before the
upgrade. The deploy runbook already takes one.

**Preflight (Go step, logged, blocks nothing):** the number of channels, members,
memberships with a bound, invites still open by kind, and the current `open_joining`. If
`open_joining` is `guest` or `both`, log that the bare address will stop admitting guests.

**Steps, in one transaction, in this order.**

1. `ALTER TABLE channels ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('public','private'))`.
2. `ALTER TABLE channels ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0,1))`, plus a trigger `channels_default_public` that aborts any row with `is_default = 1 AND visibility <> 'public'` (ADD COLUMN cannot carry a cross-column CHECK); the store checks it too.
3. `UPDATE channels SET visibility = 'public'`: every existing channel becomes public (decision 7b; open point O6 asks Nick to confirm this for servers with more than one group).
4. `UPDATE channels SET is_default = 1 WHERE id IN (SELECT channel_id FROM open_channels)` (decision 7a). `open_channels` is left in place, unread, and dropped by a later migration once the release that reads `is_default` has shipped.
5. `CREATE TABLE legacy_history_bounds (channel_id, principal_id, history_after, cleared_at, PRIMARY KEY (channel_id, principal_id))`; copy every `channel_members` row with `history_after IS NOT NULL` whose principal is **not** in an active guest visit; then `UPDATE channel_members SET history_after = NULL` for those rows (decision 4). The copy is the record of what was widened; nothing reads it.
6. `INSERT OR IGNORE INTO channel_members (channel_id, principal_id, created_at, can_decide, history_after) SELECT c.id, p.id, <now>, 0, NULL FROM channels c JOIN principals p WHERE c.is_default = 1 AND p.kind = 'human' AND p.disabled_at IS NULL AND p.hidden_at IS NULL AND NOT EXISTS (active guest visit for p)` (decision 7c). `can_decide = 0`: joining grants no approval right; operators keep whatever they held, since `OR IGNORE` leaves existing rows alone.
7. `UPDATE principals SET voice_allowed = 1, may_invite = 1 WHERE kind = 'human' AND NOT EXISTS (active guest visit)` (decisions 5 and 1). Safe for voice because no route ever set `voice_allowed = 0` deliberately: the only zeros came from an invite's `voice: false` or `join_voice`, both of which this model retires. Safe for `may_invite` because it becomes a brake with a new default.
8. `ALTER TABLE invites ADD COLUMN link TEXT NOT NULL DEFAULT 'legacy' CHECK (link IN ('legacy','member','guest'))`. Existing rows are `legacy` and keep working (decision 7d): redemption grants their named channels **and** the default channels, no bound, voice on. New rows are written with `kind = 'open'` (the published CHECK needs no rebuild) and `link = 'member'` or `'guest'`; the v2 `kind` is read from `link`.
9. `server_settings`: insert `open_signup = 'true'` when `open_joining` was `account` or `both`, else `'false'`; delete the `open_joining` and `join_voice` rows.
10. One audit row `community_migrated`, actor `migration`, subject `server`, detail with the counts of steps 3 to 9. Not one row per membership: the pinned audit trails in the tests would move, and the counts are the record.
11. Go step: log the same counts and the `open_joining` warning.

**Order of work around it.** The release that carries migration 37 must also carry the
server changes that read `visibility`, `is_default` and `link`, since the migration makes
every channel public and the directory and self-join must therefore enforce the private
rule from the same binary. That is why issues I2 and I3 (§9) are one release.

**Nick's alpha, concretely.** #general (in `open_channels`) becomes a public default
channel; #belgium becomes public; every member, the tester included, is added to #general
with no bound, and can see #belgium in the directory and join it; the tester's bound on
#general is cleared, so they see the earlier messages; their voice is on; Nick's
per-channel invite to #belgium keeps working until it expires and now also lands its
joiner in #general.

## 7. What changes or is retired

| Today | Under this design |
| --- | --- |
| `open_joining` in four modes (`off`, `guest`, `account`, `both`) | `open_signup` bool. The bare address never admits guests. `OpenJoiningMode` stays in `pkg/schema` for the v1 projection and is marked deprecated. |
| `open_channels` (what the bare address admits to) | Default channels (`channels.is_default`), chosen per channel; what every joiner starts in. |
| `join_voice` | Gone: every member has voice. |
| Per-channel invites (`personal` single-use, `open` operator-only; channels, voice, `may_invite`, `join_as` on each) | Member links (any member; default channels; nothing else) and guest links (operators; public channels; voice). Existing invites keep working until they expire; `POST /v1/invites` is refused. |
| `may_invite` as a grant an operator gives (default 0) | A brake an operator pulls (default 1). The route is the same; `InvitePermissionRequestV1` is the same; `whoami.may_invite` is true for nearly everyone. |
| `voice_allowed` set once by the invite, no route | Default 1; `PUT /v1/principals/{id}/voice-permission` flips it at any time, with live eviction. |
| `history_after` on address joiners and on converted guests | Members never carry a bound; conversion clears it. Guests unchanged. |
| Channel creation operator-only (`POST /v0/channels`) | Any member, `POST /v1/channels`, choosing visibility; creator manages. v0 stays operator-only, private, for scripts. |
| Every channel is membership-only; no channel list beyond one's own | Public channels, a directory, self-join and leave; private channels as before. |
| Nobody sees people outside their channels | Every member sees every member (`GET /v1/directory/members`). |
| A manager adds only people who share a channel with it | A manager adds any enabled non-guest human member. |
| Invite preview lists the invite's channels | A member-link preview lists the default channels; a guest-link preview its channels. |
| ADR-008's exit step 3 ("see nothing of the other groups") and the isolation check's "nothing of A" including names | Private channels and their content stay invisible; names and public channels are community-wide. The check's markers split (§8). |

Unchanged and worth saying so: passwords, TOTP, sign-in links, device links, guest
identity and bounds, End visit, conversion mechanics (minus the bound), owner and manager,
`can_decide`, agent manifests, nets and whispers, reports, hide and disable, the invite
pause and rate limits, the proxy address handling, the renewal of credentials.

## 8. Impact on work in flight

**Message reactions (docs/design/message-reactions.md; #953, #765, #766, #767).**
Visibility is the message's, through `messageVisible` and the explicit member-and-enabled
clause: unchanged, and the public/private distinction does not touch it (a public channel
is still read only by its members). Guest rules (§9 there): unchanged. Two assumptions
move: (a) "conversion widens no history" (§9, last-but-two row) becomes "conversion clears
the bound, so the member sees every reaction with every message", which the design already
says happens "through the same condition"; one sentence to amend. (b) §6.4 "a former
member's current name is never served through a reaction" was a leak concern; a member's
current name is now served to every member by the directory, so the concern narrows to
hidden principals and ended guests, which the stored attribution and hidden rename still
cover. Retention (§11.1) is unaffected. Nothing in the reactions slices needs to wait.

**Desktop companion and voice entitlement (#897, #933, #936; #770, #942).** The companion
route checks `voice_allowed` ("a person without voice is 403 `voice_not_allowed`") and
#897 already requires that "entitlement loss removes all three identities". The new voice
route is exactly such a loss and must call the same eviction path; #933's
`TestVoiceCompanion*` tests give I5 its pattern. `VoiceSessionResponseV1.companion_identity`
and the viewer grant are unaffected. With voice on by default the 403 becomes rare, not
different. #936's browser viewer is unaffected. #942's session-only sign-in reads `whoami`;
the additive fields are tolerated.

**Moderation (docs/design/moderation.md; #849–#854).** Self-join (`POST …/join`) is a new
membership-creating path and must run the shared ban predicate inside its transaction
(§6.2 there lists `PutChannelMember`, `AddChannelMemberByName`, guest admission; add this
one); a banned principal gets 403 `channel_not_joinable`. Pre-banning a non-member by id is
now a reasonable operator act, since ids are visible in the directory. Nothing else moves.

**Guests and onboarding epic (#702, #718, #821).** #718's acceptance record gains the
tester story (§10). #821's refused-join lock concern is unchanged. ADR-009 D1's bare-address
guest path is retired; the guest routes stay.

**The isolation check (e2e/isolation, #447; ADR-008 exit step 3).** Its premise "a member
of one group finds nothing of the other" must split into "nothing of the other's **private
channels**: no name, id, message, file, member list, voice room or occupancy" and "the
other's **people and public channels** are visible, by design". Concretely: the markers
(`markers.go`) stop treating A's principal names as markers; new matrix rows for
`GET /v1/directory/channels` (B sees A's public channel and not A's private one),
`GET /v1/directory/members` (B sees A's people; B's guest and agent are refused),
`POST /v1/channels/{A-private}/join` (404 identical to a ghost), `POST /v1/channels/{A-public}/join`
(succeeds, and B then sees the full history), `PUT /v1/channels/{id}/settings`,
`PUT /v1/principals/{id}/voice-permission` (B's member 403; B's operator on A's owner the
owner answer); the world (`world.go`) makes A's group a private channel and gives the server
one public channel. The `open-link`/`personal` world joins move to member links. #860
(converted guest decides an approval) is unaffected.

**Nets and voice (ADR-005, voice-control-plane.md).** No change: rooms are per channel,
membership is the gate. Occupancy rows list the caller's channels, which now include
public channels they joined.

**Private alpha (ADR-008, private-alpha.md).** §3 "each group in its own private
channels" still holds as the way to keep groups apart; "testers may invite when granted
permission" becomes "every member may"; exit step 3 is reworded as above.

**Channel rename (#727, done).** `ChannelDocV1` gains two optional fields and is sent on a
settings change too; the rename flow is unchanged.

## 9. Implementation issues, dependency-ordered

Each is one issue, one branch, one PR, executable cold; sizes are S/M/L as the alpha plan
uses them. Security-sensitive ones get a `security-reviewer` pass; membership-write ones
are `approval-path` and Nick merges them. I1 cannot start before ADR-013 is accepted and
the two version bumps signed.

| # | Title | Scope (one line) | Size | Depends on |
| --- | --- | --- | --- | --- |
| I1 | schema: community model shapes (invites v2, join settings v2, channel visibility, directory, voice permission) | `pkg/schema`: every type, constant, error code and vocabulary of §3; fixtures; `ts.go`; `go generate`; `schema-compat.sh` green with v1 fixtures byte-identical. No behaviour. | M | ADR-013 signed |
| I2 | store: migration 37 and the community columns | §6 steps 1–11; `channels.visibility/is_default`, `invites.link`, `legacy_history_bounds`, settings rows; store readers of each; preflight report; migration tests on a copy of an alpha-shaped database. **approval-path** (step 6 writes `channel_members`). Security review. | M | I1 |
| I3 | server: channel directory, self-join and leave, member channel creation, channel settings | `GET /v1/directory/channels`, `POST /v1/channels`, `POST …/join`, `POST …/leave`, `PUT /v1/channels/{id}/settings`; private-channel 404 on every new route; creator manages; guests removed when a channel turns private; `ChannelDocV1` on settings change. **approval-path** (join and create write memberships with `can_decide`). Security review. Ships in the same release as I2. | L | I2 |
| I4 | server: the member directory | `GET /v1/directory/members`, paging, operator fields, guest and agent refusals; route-table and isolation rows. Security review. | S | I2 |
| I5 | server: per-person voice permission with live eviction | `PUT /v1/principals/{id}/voice-permission`; owner protection; eviction of `p<id>`, `p<id>.screen`, `p<id>.view` and share withdrawal through the existing path; audit; `PrincipalAdminV1` fields. Security review; `voice` label. | M | I2 (I5 can run beside I3/I4) |
| I6 | server: member links and guest links (invites v2) | `POST/GET /v2/invites`, `POST /v2/invites/preview`, `GET /v2/invites/{id}/joiners`; redemption to default channels with no bound and voice on; guest links to public channels only; `may_invite` as a brake; legacy redemption adds defaults; v1 create refused, v1 list/preview projection. **approval-path** (redemption writes memberships). Security review. | L | I2 |
| I7 | server: joining settings v2, open sign-up, conversion clears bounds | `GET/PUT /v2/join/settings`; v1 GET projection, v1 PUT refused; `open_signup` → default channels; bare address admits no guest; `no_default_channel` warning; `ConvertGuest` sets `history_after = NULL`. **approval-path** (conversion touches membership rows). Security review. | M | I2, I3 |
| I8 | e2e: the community scenario and the isolation check's new premise | `e2e/isolation` world and matrix changes of §8; a new `e2e/community` scenario that replays the 2026-10-29 tester story (§10) end to end against a real conchd; CI job. | M | I3–I7 |
| I9 | web: the community client | Lobby: sign-in page at the bare address; member-link creation for every member; the channel directory with Join and Leave, Create channel with visibility; the member panel (community list beside the channel list, agents marked, guests only in channels); operator controls: visibility and default per channel, voice toggle on the member list, open sign-up and guest links in Settings (v2 shapes); generated types only. Playwright. | L | I1 (types), I3–I7 (server) |
| I10 | web and server: live directory | `conch.directory_members.v1` on the events socket when the member list changes; browser subscribes. Optional polish. | S | I4, I9 |
| I11 | cli/tui: parity | `conch admin invite` on v2 (member, guest); `conch channels list --all`, `conch channels join/leave`, `conch channels create --private`; `conch admin channel settings`; `conch admin principal voice`; `conch admin join` on v2; TUI channel directory and member list; `conch join account` unchanged. | M | I3–I7 |
| I12 | docs: community model shipped | README, deploy and onboarding docs, private-alpha.md and guests-and-accounts.md dated supersession notes, e2e/isolation README, ADR-013 status to Accepted, `open_channels` drop scheduled. | S | I8, I9, I11 |

Parallel lanes once I2 lands: {I3} , {I4, I5}, {I6}, {I7 after I3}. I9 can start on
mocked fetch from I1's generated types.

## 10. Test plan

**Schema (I1).** Golden fixtures for every new type; `Validate` tables (each rule of §3);
strict decode (unknown field refused on requests); vocabularies emitted; v1 fixtures
byte-identical; `scripts/schema-compat.sh`; `scripts/schema-ts-check.sh`.

**Store and migration (I2).** Migrate a database shaped like Nick's alpha (two channels,
one in `open_channels`, members with and without bounds, open `personal` and `open`
invites, one active guest) and assert: channels public, #general default, memberships
added with `can_decide = 0` and `history_after NULL`, member bounds cleared and copied to
`legacy_history_bounds`, the guest's bound kept, `voice_allowed`/`may_invite` 1 for members
and unchanged for the guest, invites `legacy`, settings mapped (`guest`→false,
`account`/`both`→true), one audit row. The trigger refuses a private default channel.
`TestMessageReadsStayInTheVisibilityFunction` still passes.

**Server routes (I3–I7).** Table-driven, mirroring `TestVoiceSessionRefusals` and the
isolation style: for each new route, every caller class (member, operator, non-owner
operator on an owner, manager, guest, agent, unauthenticated) and every target class
(public joined, public not joined, private joined, private not joined, nonexistent), the
status, code and body identity (a private channel a caller is not in and a ghost answer the
same bytes). Specific:
- self-join is idempotent; writes `can_decide = 0`, no bound; refused by a ban (once M3 lands);
- leave by the last manager leaves the channel manager-less (operators remain);
- creator of a channel has `manages = 1`, `can_decide = 1`; agent and guest creation 403;
- turning a channel private removes its guests in the same transaction and says how many; marking a private channel default is 409;
- directory: a member sees public channels and own private ones, never others' private; `joined`/`manages` correct; members list excludes guests, disabled, hidden; operator fields only for operators; guest and agent 403;
- voice permission: off evicts all three identities within the existing bound and ends a held share; on changes nothing live; agent 409; owner target by non-owner operator refused; audit row; `whoami.voice_allowed` follows;
- member link: any member creates within the allowance; brake off → 403; expiry absent → none; redemption lands in the default channels as of that moment with no bound, voice on, `may_invite` on, `invited_by` set; maker sees own joiners, not others'; close by maker and by operator;
- guest link: non-operator 403; private channel named 409; redemption admits to the link's channels only, bounded, voice per link; while `guests_enabled` is off the preview offers nothing;
- legacy invite: redeems to its channels plus defaults, no bound, voice on;
- `POST /v1/invites` 409; v1 list/preview projections;
- join settings v2: open sign-up needs a default channel; while on, `POST /v1/join/account` makes a member exactly as a member link does; `POST /v1/join/guest` always `guests_disabled`; v1 GET projection; v1 PUT 409;
- conversion clears bounds and nothing else changes (memberships, `can_decide = 0`, `manages = 0`).

**Approval path (I2, I3, I6, I7).** Each ships the mandated end-to-end test: request →
notify → resolve → audit, with a member who self-joined (cannot decide), a creator (can
decide in their channel), a converted guest (cannot), and quorum unchanged by each.

**Isolation (I8).** The amended `e2e/isolation` run passes in CI with the new rows and
split markers; the coverage check fails on any new route without a row.

**The tester story (I8, `e2e/community`).** Replays 2026-10-29 as it should go: Nick makes
a member link; a tester opens the bare address on a phone-sized client and gets only Sign
in; opens the link, creates an account, lands in #general, sees its full history, sees Nick
in the member list, sees #belgium in the directory, joins it, sees its history, joins voice
there. Then: an operator switches the tester's voice off and the tester is out of the room
within the bound and back after it is switched on; the operator closes the link and it no
longer works; a guest link admits a visitor to #belgium only, bounded, without the member
list.

**Browser (I9).** Unit tests with mocked fetch for every new panel and control; Playwright
for the tester story's browser half; `make web-check`.

**CLI (I11).** Hostile-server tests for every new command (as `hostile_test.go` does);
parity test that every new REST route has a CLI verb.

## 11. Open points for Nick

Each is a consequence of a decision, or a gap the decision did not reach. The design above
takes the recommended reading and flags it; none changes a decision.

- **O1. "Only operators" turn on open sign-up: every operator, or owners?** ADR-010 put
  server-wide joining policy in the owner class (`PUT /v1/join/settings` is `accessOwner`
  today). Owners are operators, so "only operators" is satisfied either way; the design
  keeps the stricter class. Say if you meant every operator.
- **O2. May a member remove the people their link brought in?** The decision says a link's
  maker can "remove the people it brought in". For operators that is the existing
  `disable-joiners`. Giving a member a route that disables other accounts server-wide is
  an escalation (a member invites a now-valued person and later removes them), so the
  design gives members see-and-close and keeps removal with operators. Recommended: as
  designed; a member asks an operator.
- **O3. A member link with no expiry never expires.** As decided ("optional expiry"). Any
  member can therefore mint a permanent reusable door into the community; closing it is the
  only brake. Today's open links are capped at 7 days. Recommended: a server setting
  `member_link_default_expiry` (default 7 days, 0 for none) applied when a link names no
  expiry, which keeps the decision's shape and gives operators a floor. Not designed in
  until you say.
- **O4. Does every member see who the operators are?** The directory carries `role` to
  every member. Discord shows roles; the alternative is `role` for operators only.
  Recommended: show it.
- **O5. May a channel manager change its channel's visibility or default mark?** Designed
  as operator-only (default is a server-wide matter; visibility flips the channel's
  exposure). A creator who made a private channel and wants it public asks an operator.
  Recommended: operator-only now; a manager-may-set-visibility rule is an additive change
  later.
- **O6. Every existing channel becomes public.** Decision 7b, exact on your alpha
  (#belgium). On a server running ADR-008's model with several separate groups, the same
  migration makes every group's channel visible and joinable by everyone, with full
  history, in one restart. Recommended for the general case: make non-open channels
  **private** in the migration and let the operator flip the ones that should be public
  (one call each); on your alpha that is one call for #belgium. Say which you want; the
  migration step is one word either way.
- **O7. Conversion clears the history bound.** Decision 4 says members see everything
  however they got there; a converted guest is a member. ADR-009 D3 said conversion keeps
  the bound so that a visitor cannot gain old private history by making an account. Under
  the new model a visitor is only ever in public channels, so what they gain is the public
  history they could have had by joining as a member. Recommended: clear it, as designed.
  ADR-013 records the amendment.
- **O8. Guests and the channel directory.** Not covered by the decisions. Designed: guests
  see no directory and cannot self-join; their channels are the link's, and operators may
  add them to other public channels. The alternative is letting guests join any public
  channel. Recommended: as designed; a guest link says where the visit is.
- **O9. The creator of a channel becomes its manager.** The only workable reading of
  "any member can create a channel … its managers and operators add people"; ADR-010 had
  rejected creator ownership only because creation was operator-only. The creator also
  gets `can_decide = 1` in their channel (as an operator-added member does today); say if
  a created channel should start with nobody able to decide.
- **O10. The manager-add bound is dropped.** ADR-010 let a manager add only people who
  already share a channel with it, to keep the server's people hidden from managers. The
  directory shows everyone to everyone, so the bound protects nothing and is removed. Say
  if you want it kept anyway.
- **O11. Agents and the member directory.** Agents are listed in it (decision 3) but
  designed as unable to read it (403), since an agent's view of people is channel content
  under its manifest and there is no MCP tool for a directory. Say if agents should read it.
- **O12. Default channels and `can_decide`.** Members joining a default channel (by link,
  sign-up or migration) get `can_decide = 0`, as invite joiners do today. Operators who
  were not in #general get a row with `can_decide = 0` too, and set their own with the
  existing route. Fine for the alpha; flagged because it is the approval path.
- **O13. The two version bumps.** `InviteV1 → InviteV2` and `JoinSettingsV1 → JoinSettingsV2`
  (and refusing `POST /v1/invites` and `PUT /v1/join/settings`) need your sign-off before
  I1 is written (CLAUDE.md rule 2, ADR-000). The alternatives (new values in the closed
  `InviteKind` vocabulary; relaxing `CreateInviteRequestV1.Validate`'s 1-to-32-channels
  rule) change the meaning of published shapes in place, which the skill forbids.

## 12. What in the current design contradicts the decisions, or makes one costly

- **ADR-003 and ADR-010 §3.3: channel membership bounds what a person can learn about
  people.** Reversed for people by decision 3; kept for channels. The isolation check and
  ADR-008's exit step 3 encode the old rule and must be amended (§8).
- **ADR-008 §4: an invite grants only what its maker holds, named channels.** Replaced by
  member links that grant the default channels. The existing `CreateInviteRequestV1`
  requires channels and is operator-only for reusable links; that is why v2 is a new
  version, not a field.
- **ADR-009 D1: bare-address modes and a guest switch across every entry point.** The modes
  collapse; the switch stays but governs guest links only.
- **ADR-009 D3 and guests-and-accounts.md §3.3/§4.3: conversion keeps the bound.**
  Contradicts decision 4; O7.
- **ADR-010 "creators own their channels: rejected" and the manager-add bound.** Both rested
  on operator-only creation and hidden people; both move (O9, O10).
- **`voice_allowed` has no route and `history_after` has no widening route.** The exact
  gaps the tester hit; filled by I5 and by clearing bounds for members.
- **The isolation check's markers treat names as secrets.** Costly: the world, markers and
  matrix change (I8, size M); unavoidable under decision 3.
- **`open_channels` and the four-mode `open_joining` are wired through
  `guest_policy.go`, the settings page and the CLI.** Retiring them is most of I7 and part
  of I9/I11; the v1 projections keep old clients working meanwhile.
- **The `InviteKind`, `InviteJoinMode` and `OpenJoiningMode` vocabularies are closed.**
  Adding values would change published enums in place; hence `InviteKindV2` and `open_signup`.
