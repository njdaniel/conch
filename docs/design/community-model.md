# The community model: one community, private channels

Decision: [ADR-013](../adr/ADR-013-one-community-model.md) (proposed, awaiting Nick's
sign-off). This note is the design behind it: the model, every decision mapped to
behaviour, the wire shapes as they would appear in `pkg/schema`, the permissions, the
privacy model of each read path, the migration, what is retired, the impact on work in
flight, the implementation issues, the test plan, and the points Nick still has to settle.

**Status: accepted 2026-10-30 (ADR-013, Nick: "accept recommendations", every open point as recommended); proposed 2026-10-29; revised after review round 1 (security and general
reviewers, PR #963); issue #964 (an optional setting that hides members from each other)
folded in as §13 the same day.** Facts about main are as of `671a16e`. No code, schema file,
migration or issue exists for this yet. Nothing here is implemented behaviour.

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
The model applies identically in temporary mode (ADR-011): a temporary server is one
community with the same links, directory and channels; only its lifetime differs.

What goes: per-channel invites as the way in, the four `open_joining` modes, `join_voice`,
`may_invite` as a grant (it becomes a brake), history bounds on members.

One optional setting, off by default, narrows the member list: with **hide members from
each other** on, an ordinary member sees only themself and the people they share a channel
with (§13, issue #964).

### 1.1 Terms

- **Member.** A human principal that is enabled (`disabled_at IS NULL`), not hidden
  (`hidden_at IS NULL`), and has **no `guest_visits` row in state `active` or `ended`**. A
  converted guest (state `converted`) is a member. This is the one predicate for "is a
  member of the community", written once in the store (`isMemberTx`) and used by every rule
  below: who may make links, who is in the directory, whom a manager may add, whom the
  migration touches. It is stricter than "not in an active visit", on purpose: ending a
  visit does not disable the principal, and the store's `guestTarget()` already treats an
  ended guest as a guest for credentials, links, names and hide (`queries.go:109`); an
  ended guest is a dead identity, never a member.
- **Operator**, **owner**, **channel manager**: as ADR-010. Operators are members too.
- **Guest**: a human in an active visit (ADR-009).
- **Agent**: `kind = agent`; by manifest, never a member of the community in the sense
  above, though listed in the directory as an agent.
- **Public channel, private channel, default channel**: §2.6.

## 2. Nick's decisions, mapped to behaviour

Numbering follows the brief and ADR-013. Where a behaviour depends on an open point it is
marked "(O#)"; §11 carries both reviewers' recommendations beside each.

### 2.1 Joining

| Decision | Behaviour |
| --- | --- |
| The member link is the one way in for members. It makes you a member and puts you in the default channels. Reusable, optional expiry and use limit. | `POST /v2/invites` with `kind: "member"`. Redeemed with the existing `POST /v2/invites/redeem` (name and password). The joiner becomes a human `member` principal with `voice_allowed = 1`, `may_invite = 1`, a membership (no history bound, `can_decide = 0`) of every channel that is a default channel **at the moment of redemption** (zero default channels is allowed: the joiner is a member in no channel and finds the public ones in the directory; the settings answer warns `no_default_channel`), and a credential of the link's lifetime (default 90 days, renewable as today). `max_uses` absent means unlimited. **Expiry (O3):** `expires_at` absent means the server's `member_link_default_expiry_seconds` (default 7 days, 0 means none); a link that should never expire is asked for with `never_expires: true`, which only an operator may send (403 `invite_not_allowed` otherwise). Storage: `invites.expires_at` is `NOT NULL`, so "no expiry" is stored as the sentinel `253402300799000` ms (`9999-12-31T23:59:59Z`); every reader treats `expires_at >= sentinel` as none, the v2 wire omits it, the v1 projection shows the sentinel. The link stays usable until it expires, is used up, is closed, or its maker stops being a member or loses `may_invite`. |
| The bare address is invite-only by default: a sign-in page only. | `GET /v1/join` answers `join.account = false`, `join.guest = false` while `open_signup` is off. The browser's lobby shows Sign in and the one line "This server is invite-only." `POST /v1/join/account` answers 403 `joining_closed`; `POST /v1/join/guest` answers 403 `guests_disabled` **always** (the bare address admits no guests in this model, whatever `guests_enabled` says). |
| Open sign-up is a switch, off by default, that only operators can turn on. When on, the address works like a member link. | `open_signup` (default false), set through the joining-settings route, which stays in the `accessOwner` class ADR-010 put server-wide policy in (O1). While on, `POST /v1/join/account` creates the same member a member link does: default channels, no bound, voice on, `may_invite` on, 90-day credential. It needs at least one default channel to be turned on (409 `join_policy_conflict`); if the last default channel is later unmarked, open sign-up turns off in the same transaction, audited (the open-channel precedent). `joining_paused` still pauses it. |
| Any member can make member links. Each link is tied to whoever made it, so you can see who brought whom, switch the link off, or remove the people it brought in. | `POST /v2/invites` is open to every **member** (§1.1) whose `may_invite` brake is on (on by default; an operator switches it off per person with the existing `PUT /v1/principals/{id}/invite-permission`). **Switching the brake off closes the person's open member links in the same transaction**, each audited `invite_closed` (reason `permission_revoked`, the existing `InviteClosePermissionRevoked`), exactly as `closeCreatorInvitesTx` does today for personal invites on `SetMayInvite(false)`; switching it on reopens nothing. An operator's links stand whatever the brake says, as today (an operator invites by role). The maker's id is on the link (`created_by`) and on every joiner (`principals.invited_by`, `invite_id`, as today). The maker sees their own links (`GET /v2/invites`), their joiners (`GET /v2/invites/{id}/joiners`; **anyone who is not the maker or an operator, including guests and agents, gets 404 `invite_not_found`**), and closes their own (`DELETE /v1/invites/{id}`, unchanged). Operators see every link and every joiner, close any, and remove the people a link brought in with the existing `POST /v1/invites/{id}/disable-joiners` and `POST /v1/principals/{id}/disable-invitees`. Whether a **member** may remove the people their link brought in is O2 (designed: no). Outstanding open member links per member are capped by the existing `--invite-allowance` (default 5), counted over rows with `link = 'member'`; the count treats a link with no use cap as open (`max_uses IS NULL OR uses < max_uses`), which today's personal-only query (`uses < max_uses`, `invites.go:352`) never needed. |
| Only operators make guest links, or turn on open sign-up. | `POST /v2/invites` with `kind: "guest"` is refused for a non-operator with 403 `invite_not_allowed`. Open sign-up: owner class, above. |

**How member and guest links sit in the `invites` table.** New rows are written with the
published `kind = 'open'` (reusable; the CHECK needs no rebuild) and the new column
`invites.link` (`'member'` or `'guest'`; existing rows `'legacy'`). Every rule that today
switches on `kind` switches on `link` for non-legacy rows: `makerStillAllowed` (a member
link needs a maker who is a member with the brake on; a guest link needs an operator),
`outstandingPersonalInvitesTx` (counts `link = 'member'` for the allowance),
`closeCreatorInvitesTx` (brake-off closes `link = 'member'`; demotion closes `link =
'guest'`, as it closes open links today), and `effectiveChannelsTx` (a member link has no
channel rows and grants the defaults; a guest link keeps only channels that are public at
redemption, §2.2). The v2 `kind` is read from `link`. A **legacy** row keeps today's rules
(`effectiveChannelsTx` keeps an operator's named channels and a member's still-held ones)
and, for a member-making invite, adds the default channels; one with none of its own
channels left is still `invite_invalid`, as today, rather than silently becoming a member
link its maker never made.

**What the preview reveals (O3).** `POST /v2/invites/preview` tells anyone holding a code
the inviter's name, the server name and the names of the default channels, as today's
preview tells the invite's channels. With no expiry a leaked link reveals those for ever;
that is one more reason the recommended default expiry is 7 days.

### 2.2 Guests

| Decision | Behaviour |
| --- | --- |
| Off by default. | `guests_enabled` stays, default false. A guest link can be made while it is off (as today, its preview and redemption say guests are unavailable). |
| Visitors only, marked as guests in the channels they visit. | A guest link names 1 to 32 **public** channels (private names are refused at creation, 409 `channel_settings_conflict`). Redemption (`POST /v1/invites/redeem-guest`, unchanged body) **re-checks visibility in its transaction** and admits the guest to the channels that are still public, dropping any that turned private (audited in the `invite_redeemed` detail as `dropped_channels`, the existing field); a link left with no public channel answers `invite_invalid`. The guest is in those channels and nothing else: not the default channels. Guests are rendered from `GuestAttributionV1` as today. |
| Never in private channels. | Enforced on every path: the store refuses a guest link naming a private channel; an operator adding a guest to a private channel (403 `guest_not_allowed`); and **a channel turning private** (§2.6) removes its active guests in the same transaction (`member_removed` rows, live eviction as for any removal) and **strips the channel from every open guest link and legacy guest-admitting invite that names it** (`invite_channel_dropped` rows, a new audit action; a link left with no channel is closed, `invite_closed` reason `no_public_channel`). `ConvertGuest` **asserts that no kept membership is of a private channel** before it clears bounds, and refuses with 500 and an audit row if the invariant is broken, since that could only mean a path above was missed. |
| They can't browse the server member list. | `GET /v1/directory/members` answers a guest 403 `guest_not_allowed`. `GET /v1/directory/channels` likewise (O8): a guest's channels are the link's. **Residual, recorded for Nick (§11, R1):** a guest still reads the member list of a public channel it visits (`GET /v1/channels/{channel}/members`, today's rule), so a guest learns the names of everyone in that channel. |
| They see history only from when they joined. | `history_after` is set at admission, as today; every read surface keeps the bound. Unchanged. |
| They get voice only if the guest link says so. | `CreateInviteRequestV2.voice` on a guest link sets the guest's `voice_allowed`; an operator can still flip it with the voice route (§2.5). |
| Visits stay time-limited. | Idle and absolute bounds, End visit, End all guest visits: unchanged (ADR-009). **Conversion** keeps the principal id and memberships and now (O7) sets `history_after = NULL` on every kept membership, `voice_allowed = 1` (decision 5: a member has voice) and `may_invite = 1` (decision 1: a member makes links). It still grants no channel, `can_decide` or `manages`. |

### 2.3 The member list

| Decision | Behaviour |
| --- | --- |
| Every member can see everyone on the server. | New route `GET /v1/directory/members`: every **member** (§1.1) and every enabled, non-hidden agent, as `ServerMemberV1` (id, kind, name, role, created_at). Members and operators may call it; guests and agents are refused (§5, O11). Ended guests, disabled and hidden principals are not in it. |
| Agents (bots) are listed and marked as agents. | `ServerMemberV1.kind = "agent"`. No role for agents. |
| Guests appear only in the channels they're visiting. | Excluded from the directory; present in `GET /v1/channels/{channel}/members` of their channels with `guest` set, as today. |
| A private channel's membership and contents stay members-only. | The directory says who is on the server, never which channels anyone is in. Channel membership is read only through the channel's own routes, which answer a non-member 404. |

### 2.4 History

| Decision | Behaviour |
| --- | --- |
| Members see the full history of every channel they're in, however they got there. | Every path that gives a **member** a membership writes `history_after = NULL`: member-link redemption, open sign-up, self-join of a public channel, an operator's or manager's add, migration into a default channel. Conversion of a guest sets `history_after = NULL` on every membership it keeps (O7; a change to ADR-009 D3 and to guests-and-accounts.md §3.3/§4.3; ADR-013 records it), after the private-channel assertion of §2.2. Migration 37 clears every existing member bound (§6). |
| Guests only from their join. | Unchanged. |

### 2.5 Voice

| Decision | Behaviour |
| --- | --- |
| On for every member by default. | `voice_allowed` is written 1 on every path that makes or converts a member. Migration sets it to 1 for every existing member and every disabled-but-not-hidden human (§6 explains why that is safe: no route ever set it to 0 on purpose). |
| An operator can switch it off or on for any person, at any time, from the member list. This needs a new route. | `PUT /v1/principals/{id}/voice-permission` body `VoicePermissionRequestV1{voice_allowed}`, `accessOperator`, owner-protected target (ADR-010: an operator cannot act on an owner), recent-authentication class. Audited `voice_permission_changed`. The directory carries `voice_allowed` to operators so the control sits on the member list. **Mechanism of the live effect.** The poller is the mechanism, in its two existing halves; nothing new is built. *Per pass:* `voiceEntitlement` (`voice_presence.go:468`) already reads `voice_allowed` on every pass and removes the person with reason `voice_not_allowed` (`voiceReasonVoiceOff`), and since #932 (landed) `p<id>.screen` and `p<id>.view` are folded into the person, so companions and viewers follow them. *Immediately:* after commit the route runs the poller's immediate hook for the target, as a disable does (`voicePrincipalLostAccess`, `voice_presence.go:2233`, which today evicts by name only for a disable and gains the `voice_not_allowed` case): the person leaves presence at once and is removed by name from the rooms of their channels (`voicePoller.evict`), with their companion and viewer (`evictCompanions`), so the effect does not wait for the next pass. A share the person holds goes with them, as for any removed participant. **Room rotation is not used:** `InvalidVoiceRooms` (`store/voice.go:526`) does not count `voice_allowed`, on purpose, since rotating would disconnect the whole room for one person's switch. `voiceStillEntitled` (`voice.go:218`), the re-check between a route's checks and signing, today reads membership and the credential only; it **gains `voice_allowed`** in its re-read so a session, transmit, share or companion request racing the flip signs nothing. **Bound:** a person switched off is out of every room at once in the common case, and at worst within one poller pass plus the lifetime of a join token already minted: such a token (`voiceTokenLifetime`, 15 s) may still connect after the hook ran, and since the room is in use while a session was issued in the last two minutes (`voiceSessionRecent`), passes are at most `voicePassGapMax` (1.5 s) apart and the next one removes it. Turning it on changes nothing live; the person joins again. |

### 2.6 Channels

| Decision | Behaviour |
| --- | --- |
| Any member can create a channel, and chooses public or private. | New `POST /v1/channels` body `CreateChannelRequestV1{name, visibility}`, open to **members** (agents and guests 403). The creator becomes the channel's only member **and its manager** (`manages = 1`, `can_decide = 1`) (O9): a private channel someone else must be able to add people to would otherwise need an operator for its first member. This is the one consequence of decision 6 that ADR-010 did not foresee (it rejected "creators own their channels" because creation was operator-only); ADR-013 amends it. `POST /v0/channels` stays operator-only and makes a private channel. **Names** are unique by `name_key` and `name_skel` (case and confusable skeleton, #727), so a 409 on create **is an existence oracle on private channel names** (§5); therefore creation by a non-operator runs under the rename limiter's pattern (`RenameLimits`: a per-caller daily cap on successful creations, recommended 10, and on attempts, recommended 30, the refused attempt audited `channel_create_refused` **without the name**), operators exempt; and every collision, with a public, a private or a look-alike name (and, once #844 lands, an archived one), answers the **same 409 `channel_exists` body** (today's `channelExistsMessage`). No channel name is reserved: the hidden and guest prefixes are rules about people (`channels_rename.go:20`). Creation is audited `channel_created` with the creator as actor (as `CreateChannelAs` does today). |
| Operators pick the default channels, which new members start in. | `PUT /v1/channels/{id}/settings` body `ChannelSettingsRequestV1{visibility?, default?}`, `accessOperator` (O5). A default channel must be public (409 `channel_settings_conflict`). Marking a channel default does not add existing members (operators add them, or they join); unmarking removes nobody; **leaving a default channel is allowed**. Unmarking the last default channel while `open_signup` is on turns open sign-up off in the same transaction, audited. |
| Other public channels are listed, and members can join them themselves. | New `GET /v1/directory/channels`: every public channel plus the caller's private channels, each with `joined`, `default`, `visibility`, `manages`. With `?scope=all` (operators only; 403 for anyone else) it lists **every** channel, private ones included, with a `members` count, so an operator can find a private channel nobody can reach any more (§5). `GET /v1/channels` keeps its meaning (the caller's channels) and gains `visibility` and `default`. New `POST /v1/channels/{channel}/join` (member only; public channel; `can_decide = 0`, no bound; refused by a channel ban, 403 `channel_not_joinable`, once moderation M3 lands) and `POST /v1/channels/{channel}/leave` (any member of the channel, a manager or an operator included). **The last member of a private channel cannot leave** (409 `channel_last_member`): a private channel with no member would be unreachable by anyone but an operator; the way out is an operator adding someone or, once #844 lands, archiving it. The last member of a public channel may leave, since it stays in the directory. |
| A private channel is visible only to its members, and its managers and operators add people. | The directory omits a private channel the caller is not in; every channel route answers a non-member 404 `channel_not_found`; self-join of a private channel is the same 404. **Turning a channel private keeps its existing members**, removes its guests and strips it from guest links (§2.2). Managers add people with the existing `PUT`/`POST …/members` routes. ADR-010's bound that a manager may add only someone who already shares a channel with it existed to keep the server's people hidden from managers; the directory now shows every member to every member, so the bound is dropped (O10): a manager adds any **member** (§1.1) who is not an operator or owner, by id or exact name; anyone else (hidden, disabled, ended guest, active guest, agent, operator, owner, unknown) is the same 404 `principal_not_found`, as `AddChannelMemberByName`'s `ErrNotAddable` answers today. Keeping operators and owners out of a manager's reach is ADR-010's rule and unchanged. |

### 2.7 Migration

In §6. In short: every channel in `open_channels` becomes a public default channel; every
other existing channel becomes **private** with its current members (O6, both reviewers'
recommendation; Nick's alpha flips #belgium with one call); every existing member joins
the default channels with no history bound; every existing member bound is cleared (and
kept aside in a table for the record); existing invites keep working until they expire,
except guest-admitting ones that name a now-private channel; voice and link-making are
turned on for every member.

## 3. Wire shapes, as they would appear in `pkg/schema`

Every shape below is a REST body versioned by its type suffix, carries no schema-name
field, and follows the house rules: flat structs, string enums with `Valid()`, RFC 3339
`Timestamp`, explicit `Validate()`, unknown-field tolerance on the way in. Fixtures go in
`pkg/schema/testdata/`, vocabularies in `vocabularies.go`, TypeScript emission in `ts.go`.
The `schema-change` skill governs the PR.

### 3.1 Compatibility summary (for Nick)

| Change | Kind | Needs Nick |
| --- | --- | --- |
| `InviteV2`, `CreateInviteRequestV2`, `CreateInviteResponseV2`, `ListInvitesResponseV2`, `InvitePreviewV2`, `InviteKindV2`; routes `POST/GET /v2/invites`, `POST /v2/invites/preview`, `GET /v2/invites/{id}/joiners` | **Version bump** of the invite family. Grounds: `InviteV1.expires_at` is a non-pointer `Timestamp` and cannot say "none"; `CreateInviteRequestV1.Validate` requires 1 to 32 channels and refuses `max_uses` on anything but kind `open`; `InviteV1.channels`/`voice`/`may_invite` carry a per-channel grant that member links do not have. Changing any of those in place mutates a published shape. (`InviteKind` being closed is a reason too, but not the deciding one.) `InviteV1` and its fixtures stay byte-identical; v1 routes keep answering legacy rows and project new ones (§3.3). | **Yes** (O13) |
| Joining settings: `open_signup` and `member_link_default_expiry_seconds` | **Optional bump** (O13). Option A: `JoinSettingsV2` at `GET/PUT /v2/join/settings`, v1 `PUT` refused. Option B: stay on v1 with additive fields and a documented mapping (§3.4). The general reviewer leans B. | **Yes**, the choice |
| `POST /v1/invites` refused after migration 37 (409 `invite_kind_retired`) | Behaviour change on a published route, like ADR-009's B1 | **Yes** |
| `POST /v1/join/guest` always refused (403 `guests_disabled`), whatever `guests_enabled` says | Behaviour change on a published route: the bare address admits no guests. The code and body are the published ones | **Yes** |
| `ChannelV0.visibility`, `ChannelV0.default`; `ChannelDocV1.visibility`, `.default` | Additive optional fields; existing fixtures unchanged | No |
| `CreateChannelRequestV1`, `CreateChannelResponseV1`, `ChannelVisibility`; route `POST /v1/channels` open to members | New types and route | No (route class of a **new** route) |
| `ChannelListingV1`, `ChannelDirectoryResponseV1`; `GET /v1/directory/channels` (`?scope=all` for operators) | New | No |
| `JoinChannelResponseV1`; `POST /v1/channels/{channel}/join`, `POST …/leave` | New | No |
| `ChannelSettingsRequestV1`, `ChannelSettingsResponseV1`; `PUT /v1/channels/{id}/settings` | New | No |
| `ServerMemberV1`, `ServerMembersResponseV1`; `GET /v1/directory/members` | New | No, but it reverses ADR-003's visibility boundary for *people*: that is ADR-013's decision 3 |
| `VoicePermissionRequestV1`, `VoicePermissionResponseV1`; `PUT /v1/principals/{id}/voice-permission` | New | No |
| `PrincipalAdminV1.voice_allowed`, `.may_invite` (optional) | Additive | No |
| `JoinWarning` value `no_default_channel` | Additive value on a **closed** vocabulary that is **client-tolerant** by its own contract (`guest.go`: "a client shows one it does not know by its value"); `Vocabularies["JoinWarning"]` and the generated TS union grow. No fixture changes. | No, recorded |
| Error codes `channel_not_joinable`, `channel_settings_conflict`, `channel_last_member`, `invite_kind_retired`, `join_settings_retired` (Option A only), `not_a_person` | Additive on `schema.Error.code` | No |
| `WhoAmIResponseV1.may_invite` true for every member; `invites_remaining` counts open member links | Semantics within the documented meaning ("whether the caller may make invites") | No |
| `ChannelMemberV1.history_after` absent for every member | Within the documented meaning | No |
| `JoinInfoV1.join.guest` always false; `.voice` always true | Within the documented meaning | No |
| `can_decide` on the creator of a channel (`POST /v1/channels`) and `can_decide = 0` on self-join and redemption; conversion clearing `history_after` | Membership-write transactions: **approval-path** PRs, merged by Nick | Yes (merge) |
| §13 (#964): `DirectorySettingsV1`, `PutDirectorySettingsRequestV1`, `DirectorySettingsResponseV1`, `DirectoryWarning`; routes `GET/PUT /v1/directory/settings` | New types, new closed client-tolerant vocabulary, new routes | No |
| §13: `DirectoryScope` and `ServerMembersResponseV1.scope`; `ChannelSettingsResponseV1.warnings` | New in I1; additive optional fields if I1 has merged | No |
| §13: `ChannelAuthorV1`, `ChannelAuthorsResponseV1`; route `GET /v1/channels/{channel}/authors` | New | No |
| §13: `DirectoryChangedV1` (`conch.directory_changed.v1`) | New events document, in place of I10's unpublished `conch.directory_members.v1` | No |

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
// its manager. Name is held to the create rule and is unique by how it
// reads (name_key and name_skel, as principal names are); every collision
// answers one 409 channel_exists, and a non-operator's attempts are limited.
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
// Joined). Members is the member count, present only on the operator's
// ?scope=all listing; the member view carries no counts and no names.
type ChannelListingV1 struct {
	ID         int64             `json:"id"`
	Name       string            `json:"name"`
	CreatedAt  Timestamp         `json:"created_at"`
	Visibility ChannelVisibility `json:"visibility"`
	Default    bool              `json:"default,omitempty"`
	Joined     bool              `json:"joined"`
	Manages    bool              `json:"manages,omitempty"`
	Members    *int64            `json:"members,omitempty"`
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

// ChannelSettingsResponseV1 is the channel as it now is, and what turning
// it private did: how many guests were removed and how many guest links
// (open guest links and legacy guest-admitting invites) lost the channel or
// were closed. Zero on every other change.
type ChannelSettingsResponseV1 struct {
	Channel          ChannelV0 `json:"channel"`
	GuestsRemoved    int       `json:"guests_removed"`
	LinksChanged     int       `json:"links_changed"`
	LinksClosed      int       `json:"links_closed"`
	OpenSignupTurnedOff bool   `json:"open_signup_turned_off,omitempty"`
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
// each other or the server: a private default channel, or a guest link
// naming a private channel.
const ErrorCodeChannelSettingsConflict = "channel_settings_conflict"

// ErrorCodeChannelLastMember (409): the caller is the last member of a
// private channel and may not leave it; an operator adds someone or (once
// archiving, #844, lands) archives the channel.
const ErrorCodeChannelLastMember = "channel_last_member"
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
	// DefaultMemberLinkExpirySeconds is the server setting's default: a
	// member link that names no expiry lasts this long (O3). Operators may
	// set the setting to 0 (no default expiry) and may make a link that
	// never expires.
	DefaultMemberLinkExpirySeconds = 7 * 24 * 60 * 60
	// MaxGuestLinkLifetime bounds a guest link's expiry, as open links
	// are bounded today.
	MaxGuestLinkLifetime = 7 * 24 * time.Hour
	// NoExpiryMillis is how "never expires" is stored in the NOT NULL
	// invites.expires_at column: 9999-12-31T23:59:59Z. A reader treats any
	// value at or past it as no expiry; the v2 wire omits expires_at then.
	NoExpiryMillis int64 = 253402300799000
)

// InviteV2 describes one link. It never carries the code or its hash.
// ExpiresAt and MaxUses are absent when unbounded; ClosedAt while open.
// Channels and Voice are present on a guest link only (and on a legacy
// invite, where Channels are the channels it still grants). Joiners is how
// many principals joined through it (members, and guest visits for a guest
// link), so a maker sees what their link did without a second call.
// Legacy is true for an invite made before migration 37 (kind personal or
// open under InviteV1): it is shown as a member link, or as a guest link
// when its join_as was guest, and still grants its named channels beside
// the default channels.
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
// means the server's member_link_default_expiry (member) or the guest
// default (guest); NeverExpires asks for no expiry and is operators-only.
// MaxUses absent is no cap. Channels and Voice are for a guest link only
// and refused on a member link. CredentialLifetimeSeconds as in v1.
type CreateInviteRequestV2 struct {
	Kind                      InviteKindV2 `json:"kind"`
	ExpiresAt                 *Timestamp   `json:"expires_at,omitempty"`
	NeverExpires              bool         `json:"never_expires,omitempty"`
	MaxUses                   *int64       `json:"max_uses,omitempty"`
	CredentialLifetimeSeconds *int64       `json:"credential_lifetime_seconds,omitempty"`
	Channels                  []string     `json:"channels,omitempty"`
	Voice                     bool         `json:"voice,omitempty"`
}

// Validate(now): kind known; expires_at in the future when present, and
// within MaxGuestLinkLifetime for a guest link; never_expires not with
// expires_at and not on a guest link; max_uses 1..MaxInviteMaxUses when
// present; a guest link names 1..MaxInviteChannels distinct valid channel
// names and a member link names none and asks no voice; credential
// lifetime bounds as v1. Whether the channels are public, and whether the
// caller may make this kind or ask never_expires, is the store's rule.
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
// link's channels that are public now; for a legacy invite, the channels it
// grants (its own that its maker still holds, plus the defaults for a
// member-making one). Join is what the link offers now after the server's
// switches (a guest link while guests are off offers nothing). Nothing
// else: no counts, no members, no messages.
type InvitePreviewV2 struct {
	Kind       InviteKindV2  `json:"kind"`
	ServerName string        `json:"server_name"`
	Inviter    string        `json:"inviter"`
	Channels   []string      `json:"channels"`
	Voice      bool          `json:"voice,omitempty"`
	ExpiresAt  *Timestamp    `json:"expires_at,omitempty"`
	Join       JoinOptionsV1 `json:"join"`
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
| `GET /v1/invites` | Lists legacy invites as before. A v2 link is projected: kind `open`, `channels` = the default channels now (member) or the link's public channels (guest), `voice` true (member) or the link's, `may_invite` false, `join_as` `account` or `guest`, `expires_at` = the link's or the `NoExpiryMillis` sentinel. Lossy; old clients only. |
| `POST /v1/invites/preview` | The same projection into `InvitePreviewV1`. |
| `DELETE /v1/invites/{id}` | Unchanged: maker or operator closes any link, legacy or v2. |
| `GET /v1/invites/{id}/principals`, `POST …/disable-joiners`, `POST /v1/principals/{id}/disable-invitees`, `PUT …/invite-permission`, `GET/PUT /v1/invites/settings` | Unchanged. `invite-permission` now sets the brake and closes open member links on off (§2.1). |

New v2 administration route: `GET /v2/invites/{id}/joiners` (`accessAuthenticated`;
the maker or an operator; **anyone else, guests and agents included, 404
`invite_not_found`**, the same bytes as for an id that does not exist) answers the existing
`InviteJoinersResponseV1`. This is how a member sees "who I brought". It lists members
only; a guest link's visits are in `GET /v1/guests?invite_id=`, operators only.

### 3.4 Joining settings: two options (O13)

Both options add `open_signup` (bool, default false) and
`member_link_default_expiry_seconds` (int, default `DefaultMemberLinkExpirySeconds`, 0 for
none) and retire the four modes, `join_voice` and the open-channel set. They differ in
whether a second version is published.

**Option A: a second version.**

```go
type JoinSettingsV2 struct {
	OpenSignup                    bool  `json:"open_signup"`
	MemberLinkDefaultExpirySeconds int64 `json:"member_link_default_expiry_seconds"`
	GuestsEnabled                 bool  `json:"guests_enabled"`
	JoiningPaused                 bool  `json:"joining_paused"`
	MaxGuests                     int64 `json:"max_guests"`
	MaxGuestsPerAddress           int64 `json:"max_guests_per_address"`
	GuestIdleSeconds              int64 `json:"guest_idle_seconds"`
	GuestAbsoluteSeconds          int64 `json:"guest_absolute_seconds"`
}
// PutJoinSettingsRequestV2: pointer fields, one left out keeps its value.
// JoinSettingsResponseV2 {Settings, ServerName, DefaultChannels []InviteChannelV1, LiveGuests, Warnings}.
// Routes GET/PUT /v2/join/settings. GET /v1/join/settings projects (open_joining =
// account|off, join_voice true, open_channels = the default channels' names);
// PUT /v1/join/settings answers 409 join_settings_retired.
const ErrorCodeJoinSettingsRetired = "join_settings_retired"
```

For: the shape says what the server does; nothing lies. Against: a second version of a
shape that shipped weeks ago, a refused published route, and v1 projections to maintain.

**Option B: stay on v1, additive fields, a documented mapping.**

- `JoinSettingsV1` and `PutJoinSettingsRequestV1` gain `open_signup *bool` and
  `member_link_default_expiry_seconds *int64` (optional; a server that predates them omits
  them). No fixture changes.
- `open_joining` **reads** `account` when `open_signup` is true and `off` otherwise. A
  `PUT` with `open_joining: account` or `off` sets `open_signup`; with `guest` or `both`
  it answers 409 `join_policy_conflict` (the ADR-009 B1 precedent: a published route
  refuses what the server no longer does).
- `join_voice` **reads** true; a `PUT` with `join_voice: false` answers 409
  `join_policy_conflict`.
- `open_channels` **reads** the default channels' names; a `PUT` with `open_channels`
  marks exactly those channels default and unmarks the rest (409 `channel_settings_conflict`
  if one is private), which keeps `conch admin join` and GuestSettings.tsx working
  unchanged until I9c/I11a move them to `PUT /v1/channels/{id}/settings`.
- `JoinSettingsResponseV1` gains `default_channels []InviteChannelV1` (optional).

For: no bump, no refused route, old clients keep working, the CLI and settings page move
at their own pace. Against: three fields whose names no longer describe the model, and
two values that are accepted on read but refused on write.

**The general reviewer leans B; the design works under either.** The rest of this note
says "the joining-settings route" where the option does not matter. `JoinInfoV1` keeps its
shape under both: `join.guest` is always false, `join.account` follows `open_signup` and the
pause, `voice` is true.

```go
// JoinWarningNoDefaultChannel: no channel is a default channel, so a member
// link or open sign-up would make a member who is in nothing. An addition to
// the closed, client-tolerant JoinWarning vocabulary.
const JoinWarningNoDefaultChannel JoinWarning = "no_default_channel"
```

### 3.5 The member directory

```go
// directory.go (new file)

// ServerMemberV1 is one entry of the community's member list, GET
// /v1/directory/members (ADR-013 decision 3): every member (§1.1: enabled,
// not hidden, no active or ended guest visit) and every enabled, non-hidden
// agent. Role is for humans; agents carry none. Owner, VoiceAllowed,
// InvitedBy and MayInvite are filled for an operator viewer only, so the
// controls that act on them sit on this list; a member sees id, kind, name,
// role and created_at and nothing more. Nothing says which channels anyone
// is in.
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

Whether `role` is shown to every member is O4 (designed: yes).

A live events document is a follow-up (issue I10), not required for the first slice: the
browser re-reads the directory when it opens the panel and on every `whoami` refresh.
§13.6 makes that document a content-free signal, `conch.directory_changed.v1`, rather than
the whole list first sketched here (`conch.directory_members.v1`, never published), and
makes it required before the setting of §13 ships.

### 3.6 Voice permission

```go
// voice_permission.go (new file)

// VoicePermissionRequestV1 is the body of PUT /v1/principals/{id}/voice-permission
// (operator, recent authentication; ADR-013 decision 5): whether the person
// may use voice and screen share.
type VoicePermissionRequestV1 struct {
	VoiceAllowed bool `json:"voice_allowed"`
}

// VoicePermissionResponseV1: the state now and whether it changed. The
// removals from rooms are not counted here: the immediate hook never fails
// the request and does not report back, and each removal is its own
// voice_participant_removed audit row. A join token minted before the change
// may still connect for its 15 s lifetime; the next pass removes it.
type VoicePermissionResponseV1 struct {
	PrincipalID  int64 `json:"principal_id"`
	VoiceAllowed bool  `json:"voice_allowed"`
	Changed      bool  `json:"changed"`
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
(ADR-010 D7). "Member" is §1.1's predicate. "404" is the route's not-found answer; "403" is
the stated code.

| Action | Member | Operator (owner noted) | Channel manager | Guest | Agent |
| --- | --- | --- | --- | --- | --- |
| Sign in at the bare address | yes | yes | yes | no (a visit is not an account) | no |
| Create an account at the bare address | only while `open_signup` | same | same | same | no |
| Make a member link | yes (brake `may_invite` on, allowance, default expiry) | yes, no allowance, may ask `never_expires` | as member | 403 `guest_not_allowed` | 403 |
| See / close own links, see own joiners | yes; others' joiners 404 | every link, every joiner | as member | 404 | 404 |
| Remove the people a link brought in | no (O2) | yes (`disable-joiners`, `disable-invitees`) | no | no | no |
| Make a guest link | 403 `invite_not_allowed` | yes | as member | no | no |
| Turn on open sign-up, `guests_enabled`, guest bounds, default link expiry | no | **owner** (ADR-010; O1) | no | no | no |
| Pause invites / joining; end guest visits | no | yes (brakes stay operator) | no | no | no |
| Set a person's `may_invite` brake (closes their open member links) | no | yes, not on an owner | no | no | no |
| Read the member directory | yes; while `hide_members` is on, only the people they may know (§13.2) | yes, everyone, with `owner`, `voice_allowed`, `may_invite`, `invited_by` | as member | 403 `guest_not_allowed` | 403 |
| Turn `hide_members` on or off (§13) | no | **owner** (O1's class; recent authentication) | no | no | no |
| Read the channel directory | public + own private | same; `?scope=all` lists every channel with counts | as member | 403 | 403 |
| Join a public channel | yes | yes | as member | 403 `channel_not_joinable` | 403 |
| Leave a channel | yes, unless last member of a private one (409) | yes, same rule | yes (loses the grant) | no (End visit) | no |
| Create a channel (public or private) | yes, limited per day; becomes its manager | yes, unlimited | as member | 403 | 403 |
| Set a channel's visibility / default | no | yes (O5) | no | no | no |
| Add a person to a channel | no | yes, anyone addable | yes, any member but an operator or owner; others uniform 404 | no | no |
| Add an agent to a channel | no | yes | no | no | no |
| Add a guest to a channel | no | public channels only | no | no | no |
| Remove a member | no | yes (owner protection) | ADR-010 rules | no | no |
| Rename a channel | no | yes | yes, limited | no | no |
| Set `can_decide`, `manages` | no | yes (approval-path) | no | no | no |
| Use voice / share | yes while `voice_allowed` | yes | as member | if the link said so | no |
| Switch a person's voice | no | yes, not on an owner; agents 409 | no | no | no |
| Read history of a channel they are in | all of it | all of it | all of it | from their arrival | all of it, by manifest |
| See a private channel they are not in | 404 | 404 on content routes; name and existence through `?scope=all`, settings and rename | 404 | 404 | 404 |
| Approvals | `can_decide` as granted | as today | as today | never | by manifest |

## 5. The privacy model, by read path

**The server member list** (`GET /v1/directory/members`). Says: every member's id, kind,
name, role and when they joined, and that is all. Does not say: which channels anyone is
in, who is in voice, who is a guest, who is disabled or hidden, who invited whom (operators
excepted). Guests and agents are refused: a guest is a visitor and this is the community's
list; an agent's view of people is channel content under its manifest, as today. This is
the one deliberate change to ADR-003's rule that channel membership bounds what a person
can learn: **people are community-wide, channels are not.** An operator can put the old
rule back for people with the optional `hide_members` setting; §13 gives every read path
under it.

**The channel list** (`GET /v1/channels`, `GET /v1/directory/channels`). A member sees:
every public channel by name, with whether they are in it and whether it is a default
channel; and their own private channels. A member does not see: any private channel they
are not in, how many people are in any channel, or who. The member view carries no counts
on purpose: a count on a public channel is harmless, a count on a private one would be an
oracle, and one rule is easier to hold than two. **Operators** are different and this is
said plainly (R4): an operator can learn **every** channel's existence, name and member count
(`?scope=all`), and administers every channel by id (settings, rename), while remaining a
non-member for **content** (messages, members, files, voice) of a private channel they are
not in, as today.

**A private channel's existence, membership and messages.** To a non-member nothing
exists: every route that names the channel, by name or by id, answers 404
`channel_not_found` with the same body it gives for a channel that was never made
(`writeChannelNotFound`, as today), including `POST …/join`, `PUT …/settings` for
non-operators, `GET …/members`, every message, file, voice, net and read-state route, and
the socket upgrades. The directory omits it. **Two leaks remain, both accepted and
bounded:** (1) message, file and principal ids are global sequences, so id gaps leak
activity volume (#500, accepted); (2) **channel names are global and unique, including
look-alikes**, so a member who tries to create a channel and gets 409 `channel_exists`
learns that *some* channel has that name, public, private or reserved: an existence oracle
on private names. It is bounded the way the rename oracle is (#727): one 409 body for every
collision, a per-caller daily attempt limit with an audit row that carries no name, and
operators exempt; it is recorded in the isolation matrix as a documented row (§8). **What a
non-member of a private channel can learn: nothing of its content or membership, the same
404 as today; of its existence, only what a limited, audited name collision tells them.**

**Guests' view.** The channels the link named, with history from their arrival; those
channels' member lists (today's rule: residual R1 in §11); voice there if granted;
`whoami`; their own visit. No member directory, no channel directory, no default channels,
no private channel ever, no links of their own, no account routes. Everything else is 403
`guest_not_allowed` or the member's 404, as today.

**Agents.** Unchanged: channel content, including the channel's member list, by manifest;
no directory; no joining.

## 6. Migration

One SQLite migration, number 37 at `671a16e` (the reactions and moderation designs also
claim the next number; it is assigned at merge), additive in shape, with a Go step that
reports what it did. Rollback, as for every migration (private-alpha.md §4 O7): the
previous binary refuses a database at a newer `user_version` (`store.go:1055`), so rolling
back is the previous binary plus the backup taken before the upgrade. The deploy runbook
already takes one.

**Population.** Steps 5 to 7 act on *P*: every human principal that is **not hidden** and
has **no `guest_visits` row in state `active` or `ended`** (converted guests are in *P*).
That is §1.1's member predicate without the enabled test: a human disabled at migration
time is included so that `POST /v1/principals/{id}/enable` has **nothing** to fix up later;
enable does what it does today and no more. Active and ended guests are excluded from every
step: an ended guest's bounds, voice and memberships stay as they are, it is in no default
channel and no directory.

**Preflight (Go step, logged, blocks nothing):** the number of channels, members,
memberships with a bound, invites still open by kind and `join_as`, guest-admitting invites
that name a channel about to become private, and the current `open_joining`. If
`open_joining` is `guest` or `both`, log that the bare address will stop admitting guests.

**Steps, in one transaction, in this order.** Timestamps are unix milliseconds, as every
`created_at` in the store is.

1. `ALTER TABLE channels ADD COLUMN visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('public','private'))`.
2. `ALTER TABLE channels ADD COLUMN is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0,1))`, plus **two triggers**, `channels_default_public_insert` and `channels_default_public_update`, that abort a row with `is_default = 1 AND visibility <> 'public'` (ADD COLUMN cannot carry a cross-column CHECK); the store checks it too.
3. `UPDATE channels SET visibility = 'public', is_default = 1 WHERE id IN (SELECT channel_id FROM open_channels)` (decision 7a). Every other channel **stays private** with its current members (O6, as both reviewers recommend; decision 7b's "other existing channels become public" is one call per channel for the operator afterwards, `PUT /v1/channels/{id}/settings {"visibility":"public"}`; on Nick's alpha that is #belgium). `open_channels` is left in place, unread, and dropped by a later migration (I13).
4. **Guests out of now-private channels.** Every `channel_members` row of an **active** guest on a channel that is private after step 3 is deleted, each with a `member_removed` audit row (actor `migration`); the guest's visit continues in its public channels, or ends by its bounds if none remain. Every open invite with `join_as IN ('guest','both')` that names a now-private channel loses that `invite_channels` row (`invite_channel_dropped`), and is closed (`invite_closed`, reason `no_public_channel`) if none remains. On Nick's alpha nothing matches.
5. `CREATE TABLE legacy_history_bounds (channel_id, principal_id, history_after, cleared_at, PRIMARY KEY (channel_id, principal_id))`; copy every `channel_members` row with `history_after IS NOT NULL` whose principal is in *P*; then `UPDATE channel_members SET history_after = NULL` for those rows (decision 4). The copy is the record of what was widened; nothing reads it.
6. `INSERT OR IGNORE INTO channel_members (channel_id, principal_id, created_at, can_decide, history_after) SELECT c.id, p.id, <now ms>, 0, NULL FROM channels c JOIN principals p WHERE c.is_default = 1 AND p IN P` (decision 7c). `can_decide = 0`: joining grants no approval right; operators keep whatever they held, since `OR IGNORE` leaves existing rows alone (O12).
7. `UPDATE principals SET voice_allowed = 1, may_invite = 1 WHERE id IN P` (decisions 5 and 1). Safe for voice because no route ever set `voice_allowed = 0` deliberately: the only zeros came from an invite's `voice: false` or `join_voice`, both of which this model retires. Safe for `may_invite` because it becomes a brake with a new default. Disabled humans in *P* are flipped too, so enabling them later needs nothing.
8. `ALTER TABLE invites ADD COLUMN link TEXT NOT NULL DEFAULT 'legacy' CHECK (link IN ('legacy','member','guest'))`. Existing rows are `legacy` and keep working (decision 7d): redemption grants their named channels that are still public to a guest, or their named channels **and** the default channels, no bound, voice on, to a member. New rows are written with `kind = 'open'` and `link = 'member'` or `'guest'`.
9. `server_settings`: insert `open_signup = 'true'` when `open_joining` was `account` or `both`, else `'false'`; insert `member_link_default_expiry_seconds` = 604800; delete the `open_joining` and `join_voice` rows.
10. One audit row `community_migrated`, actor `migration`, subject `server`, detail with the counts of steps 3 to 9. **This is the first migration to write `audit_events`**; it writes one row, not one per membership, so the pinned audit trails in the tests move by exactly one id, which the fixtures that pin them are updated for in the same PR.
11. Go step: log the same counts and the `open_joining` warning.

**Coupling and the upgrade notice.** After step 3 nothing reads `visibility` or
`is_default` until I3a/I3b ship, and with O6 as private the migration exposes nothing new on
its own: a server upgraded to I2 alone behaves as today, except that open channels are now
default channels and history bounds are gone. So I2 ships alone. Its acceptance criteria
include an **operator-facing upgrade notice** in CHANGELOG.md and the release notes: what
became private, what became default, that bounds were cleared, that `POST /v1/invites` is
refused and the `--invite-allowance` now counts member links.

**Nick's alpha, concretely.** #general (in `open_channels`) becomes a public default
channel; #belgium stays private with its members until Nick flips it public; every member,
the tester included, is added to #general with no bound, and once #belgium is public sees
it in the directory and joins it; the tester's bound on #general is cleared, so they see the
earlier messages; their voice is on; Nick's per-channel invite to #belgium keeps working
until it expires and now also lands its joiner in #general.

## 7. What changes or is retired

| Today | Under this design |
| --- | --- |
| `open_joining` in four modes (`off`, `guest`, `account`, `both`) | `open_signup` bool. The bare address never admits guests. Under Option B `open_joining` stays on the wire as a two-valued projection; under Option A `OpenJoiningMode` stays in `pkg/schema` for the v1 projection, marked deprecated. |
| `open_channels` (what the bare address admits to) | Default channels (`channels.is_default`), chosen per channel; what every joiner starts in. |
| `join_voice` | Gone: every member has voice. |
| Per-channel invites (`personal` single-use, `open` operator-only; channels, voice, `may_invite`, `join_as` on each) | Member links (any member; default channels; nothing else; default 7-day expiry) and guest links (operators; public channels; voice). Existing invites keep working until they expire; `POST /v1/invites` is refused. |
| `may_invite` as a grant an operator gives (default 0) | A brake an operator pulls (default 1); pulling it closes the person's open member links. The route is the same; `InvitePermissionRequestV1` is the same; `whoami.may_invite` is true for nearly everyone. |
| `voice_allowed` set once by the invite or `join_voice`, no route | Written 1 on every path that makes or converts a member; `PUT /v1/principals/{id}/voice-permission` flips it at any time, with eviction by the poller. |
| `history_after` on address joiners and on converted guests | Members never carry a bound; conversion clears it. Guests unchanged. |
| Channel creation operator-only (`POST /v0/channels`) | Any member, `POST /v1/channels`, choosing visibility, rate-limited; creator manages. v0 stays operator-only, private, for scripts. |
| Every channel is membership-only; no channel list beyond one's own | Public channels, a directory (and an operator's all-channels view), self-join and leave; private channels as before. |
| Nobody sees people outside their channels | Every member sees every member (`GET /v1/directory/members`). |
| A manager adds only people who share a channel with it | A manager adds any member; everyone else a uniform 404. |
| Invite preview lists the invite's channels | A member-link preview lists the default channels; a guest-link preview its public channels. |
| ADR-008's exit step 3 ("see nothing of the other groups") and the isolation check's "nothing of A" including names | Private channels and their content stay invisible; names and public channels are community-wide. The check's markers split into a private register (§8). |

Unchanged and worth saying so: passwords, TOTP, sign-in links, device links, guest
identity and bounds, End visit, conversion mechanics (minus the bound, plus voice and the
brake), owner and manager, `can_decide`, agent manifests, nets and whispers, reports, hide
and disable, the invite pause and rate limits, the proxy address handling, the renewal of
credentials, temporary mode (ADR-011).

## 8. Impact on work in flight

**Message reactions (docs/design/message-reactions.md; #953, #765, #766, #767).**
Visibility is the message's, through `messageVisible` and the explicit member-and-enabled
clause: unchanged, and the public/private distinction does not touch it (a public channel
is still read only by its members). Guest rules (§9 there): unchanged. Two assumptions
move: (a) "conversion widens no history" (§9) becomes "conversion clears the bound, so the
member sees every reaction with every message", which the design already says happens
"through the same condition"; one sentence to amend. (b) §6.4 "a former member's current
name is never served through a reaction" was a leak concern; a member's current name is now
served to every member by the directory, so the concern narrows to hidden principals and
ended guests, which the stored attribution and hidden rename still cover. Retention (§11.1)
is unaffected. Nothing in the reactions slices needs to wait.

**Desktop companion and voice entitlement (#897, #932, #933, #936; #770, #942).** The
companion route checks `voice_allowed` ("a person without voice is 403
`voice_not_allowed`") and #897 already requires that "entitlement loss removes all three
identities", which #932's folding of `p<id>.screen` and `p<id>.view` into the person gives
the poller. The voice route (I5) is exactly such a loss: it triggers a pass and adds
`voice_allowed` to `voiceStillEntitled`, which the companion route also calls, so a
companion or viewer request racing the flip signs nothing. I5 builds on #932 (the folding,
landed) and is tested against #933's companion route when that has landed; it does not
block #933, and #933 does not block it (if I5 lands first, #933's tests gain the
voice-off case). `VoiceSessionResponseV1.companion_identity` and the viewer grant are unaffected. With
voice on by default the 403 becomes rare, not different. #936's browser viewer is
unaffected. #942's session-only sign-in reads `whoami`; the additive fields are tolerated.

**Moderation (docs/design/moderation.md; #849–#854).** Self-join (`POST …/join`) is a new
membership-creating path and must run the shared ban predicate inside its transaction
(§6.2 there lists `PutChannelMember`, `AddChannelMemberByName`, guest admission; add this
one); a banned principal gets 403 `channel_not_joinable`. Pre-banning a non-member by id is
now a reasonable operator act, since ids are visible in the directory. Nothing else moves.

**Guests and onboarding epic (#702, #718, #821).** #718's acceptance record gains the
tester story (§10). #821's refused-join lock concern is unchanged. ADR-009 D1's bare-address
guest path is retired; the guest routes stay.

**The isolation check (e2e/isolation, #447; ADR-008 exit step 3).** Its premise "a member
of one group finds nothing of the other" splits into a **private register** and a public
remainder:

- *Private register (markers).* A's **private channel's name and id**; everything A writes
  **in it**: message bodies, thread replies, whispers and net posts, file names and bytes,
  net names, approval titles and reasons, problem-report text, run and session labels; its
  **voice room names**; its member list. All of these are markers and may appear in no
  answer, header or frame B receives.
- *Not markers.* A's principal names and ids (the directory shows them by design), the
  name of A's **public** channel, and what A writes in the public channel, which carries a
  distinct non-marker tag so the scan can tell it from a private leak.
- *Rows for the private channel* rely on ghost comparison: `GET …/members`, every message
  route, voice session/transmit/share/presence, nets, read-state, files by id, `POST …/join`,
  and `PUT …/settings` and `PUT …/name` for non-operators all answer B the bytes they answer
  for a channel that never existed.
- *Directory rows.* `GET /v1/directory/channels` as B asserts A's private channel is
  **absent** and A's public channel present; `GET /v1/directory/members` as B's member
  lists A's people and as B's guest and agent is refused; `POST /v1/channels` by B with A's
  private name documents the oracle: 409 `channel_exists` with the one body, counted and
  limited, audited without the name.
- *World.* A's group is a private channel plus one public channel; both groups join through
  member links; B's operator makes the guest link.
- *Hide members (§13.9).* A second phase with `hide_members` on, in which a principal's name
  and id become markers for every member who may not know it; its rows land with I14a.

These rows and marker changes are **distributed into the slices that add the routes**
(§9: I3a, I3b, I4, I5, I6a, I6b, I7), since `TestEveryRouteHasARow` gates every PR; the
marker split must land in or before I4, which is the first slice that makes names visible
to everyone. I6b moves `e2e/isolation`'s `world.go`, `conversion.go`, `guestrows.go` and
`matrix.go`, and `e2e/voice/managed_paths_linux.go`, from `/v1/invites` to `/v2/invites`.
I8 is then only the `e2e/community` scenario. #860 (converted guest decides an approval) is
unaffected.

**Nets and voice (ADR-005, voice-control-plane.md).** No change: rooms are per channel,
membership is the gate. Occupancy rows list the caller's channels, which now include
public channels they joined.

**Private alpha (ADR-008, private-alpha.md).** §3 "each group in its own private
channels" still holds as the way to keep groups apart; "testers may invite when granted
permission" becomes "every member may"; exit step 3 is reworded as above.

**Channel rename (#727, done).** `ChannelDocV1` gains two optional fields and is sent on a
settings change too; the rename flow is unchanged. Its limiter pattern is reused by create.

**Temporary servers (ADR-011).** Identical model; a temporary server's member links die
with it.

## 9. Implementation issues, dependency-ordered

Each is one issue, one branch, one PR, executable cold; sizes are S/M/L as the alpha plan
uses them. Security-sensitive ones get a `security-reviewer` pass; membership-write ones
are `approval-path` and Nick merges them. Every server slice that adds a route adds its
isolation-matrix rows and marker changes (`TestEveryRouteHasARow`). I1 cannot start before
ADR-013 is accepted and O13 is answered.

| # | Title | Scope (one line) | Size | Labels | Depends on |
| --- | --- | --- | --- | --- | --- |
| I1 | schema: community model shapes | `pkg/schema`: every type, constant, error code and vocabulary of §3, under O13's answer (invite v2; join settings v2 or v1 additive); fixtures; `ts.go`; `go generate`; `schema-compat.sh` green with v1 fixtures byte-identical. No behaviour. | M | area/schema | ADR-013 signed, O13 |
| I2 | store: migration 37 and the community columns | §6 steps 1–11 with the *P* population; `channels.visibility/is_default` and two triggers, `invites.link`, `legacy_history_bounds`, settings rows; store readers; `isMemberTx`; preflight report; migration tests on an alpha-shaped fixture **that includes an ended guest** (asserting it is in no default channel, keeps its bound, keeps voice/may_invite as they were, is not a member) and a disabled human (flipped; enable needs nothing); the CHANGELOG/release-notes upgrade notice. | M | approval-path, security | I1 |
| I3a | server: member channel creation and channel settings | `POST /v1/channels` (creator manages; `name_key`/`name_skel` uniqueness; one 409 body; per-member daily caps on the rename-limiter pattern; `channel_created` with creator, `channel_create_refused` without name); `PUT /v1/channels/{id}/settings` (default⇒public; private removes guests, strips guest links, may turn open sign-up off; public→private keeps members); `ChannelDocV1` on change; isolation rows incl. the oracle row. | M | approval-path, security | I2 |
| I3b | server: channel directory, self-join and leave | `GET /v1/directory/channels` (+ `?scope=all` for operators); `POST …/join` (`can_decide = 0`, no bound, ban predicate hook, guests/agents 403); `POST …/leave` (`channel_last_member` on a private channel); private 404 everywhere; isolation rows (directory absent/present, join ghost). | M | approval-path, security | I2 |
| I4 | server: the member directory | `GET /v1/directory/members` on the member predicate, paging, operator fields, guest and agent refusals; **the isolation marker split** (private register, names not markers) lands here or earlier; directory rows. | S | security | I2 |
| I5 | server: per-person voice permission | `PUT /v1/principals/{id}/voice-permission`; owner protection; `not_a_person`; the poller's immediate hook for the target (`voicePrincipalLostAccess` gains the `voice_not_allowed` case), no room rotation; `voice_allowed` added to `voiceStillEntitled`; audit; `PrincipalAdminV1` fields; tests incl. a token minted before the flip and companion/viewer identities following the person. Builds on #932 (landed); verified against #933's companion route once it exists; neither blocks the other. | M | security, voice | I2 |
| I6a | server: member and guest links (create, list, preview, joiners), the brake | `POST/GET /v2/invites`, `POST /v2/invites/preview`, `GET /v2/invites/{id}/joiners` (404 for non-makers); `invites.link` keying of `makerStillAllowed`, the allowance, `closeCreatorInvitesTx`; brake-off closes member links, audited; `never_expires` operators-only; default expiry setting; guest links public-only; `POST /v1/invites` 409; isolation rows. No membership write. | M | security | I2 |
| I6b | server: redemption to the default channels, legacy redemption, v1 projections | `POST /v2/invites/redeem` and `/v1/invites/redeem-guest` under the new rules (defaults, no bound, voice on, brake on; guest re-check of visibility and `invite_invalid` when none left; legacy adds defaults); v1 list/preview projections; move `e2e/isolation` (`world.go`, `conversion.go`, `guestrows.go`, `matrix.go`) and `e2e/voice/managed_paths_linux.go` onto `/v2/invites`; isolation rows. | M | approval-path, security | I6a, I3a |
| I7 | server: joining settings, open sign-up, conversion | Option A or B per O13; `open_signup` → default channels; bare address never admits guests (`POST /v1/join/guest` always `guests_disabled`); `no_default_channel` warning; last-default unmarking turns sign-up off; `ConvertGuest`: private-channel assertion, then `history_after = NULL`, `voice_allowed = 1`, `may_invite = 1`; isolation rows. | M | approval-path, security | I2, I3a |
| I8 | e2e: the community scenario | `e2e/community`: replays the 2026-10-29 tester story (§10) end to end against a real conchd, phone-sized client included; CI job. | S | | I3a–I7 |
| I9a | web: lobby, sign-in page and member links | Bare-address lobby is Sign in only; member-link creation for every member with expiry/uses; own links and joiners; preview v2 landing; phone layout (`web/src/phone.ts`, `12-phone.spec.ts`) required; Playwright; update `web/e2e/11-invites.spec.ts` and `web/e2e/api.ts` to v2. | M | | I1, I6a, I6b |
| I9b | web: channel directory, channels and the member panel | Directory with Join/Leave, Create channel with visibility, community member panel (agents marked, guests only in channels), phone layout; Playwright; update `web/e2e/14-isolation.spec.ts`. | L | | I1, I3a, I3b, I4 |
| I9c | web: operator controls and settings | Visibility/default per channel, voice toggle on the member list, open sign-up, default link expiry and guest links in Settings (replacing GuestSettings' open-channel set); phone layout; Playwright; update `web/e2e/30-temporary-servers.spec.ts`. | M | | I1, I5, I7 |
| I10 | web and server: live directory | The content-free `conch.directory_changed.v1` signal on the events socket to the viewers whose directory answer changed (§13.6); browser re-reads and replaces its people cache. Optional polish while `hide_members` does not exist; its server half is required by I14a, which builds it if I10 has not landed (I10 is then the browser half and follows I14a). | S | | I4, I9b |
| I11a | cli: parity | `conch admin invite` on v2 (member, guest, joiners); new top-level `conch channels` (`list [--all]`, `join`, `leave`, `create [--private]`); `conch admin channel settings`; `conch admin principal voice`; `conch admin join` on the chosen option; `conch join account` unchanged; hostile-server tests. | M | | I3a–I7 |
| I11b | tui: parity | A channel directory pane with join/leave, a community member pane, create-channel with visibility, operator voice toggle. | M | | I11a |
| I12 | docs: community model shipped | README, docs/deploy.md, **docs/alpha-tester-guide.md**, private-alpha.md and guests-and-accounts.md dated supersession notes, e2e/isolation doc comment, ADR-013 status to Accepted. | S | | I8, I9a–c, I11a–b |
| I13 | store: drop `open_channels` | Follow-up migration removing the unread table. Release condition: one tagged release after I2 has shipped and the upgrade notice has been out for a cycle. | S | | I2 shipped |
| I14a | server: hide members from each other (#964) | `hide_members` and `GET/PUT /v1/directory/settings`; the `knows` predicate in the store and its guard test; directory, manager add and joiners through it, with the uniform 404; `GET /v1/channels/{channel}/authors`; the targeted `conch.directory_changed.v1` signal (I10's server half); `default_channel_shared` warnings; the isolation phase with the setting on. In full in §13.11. | M | security | I4, I3b, I6a, O14 |
| I14b | web, cli and tui: hide members | The setting in Settings and `conch admin directory`, with §13.1's description and the warning; the member panel's scope line; caches replaced on the signal; departed authors named through the authors route. In full in §13.11. | S | | I14a, I9b, I9c, I11a |

Parallel lanes once I2 lands: {I3a, I3b} (either order, one after the other, since both
touch `channels.go` and `members.go`), {I4}, {I5}, {I6a → I6b after I3a}, {I7 after I3a},
{I14a after I4, I3b and I6a}. I9a–c can start on mocked fetch from I1's generated types.

**Note on O10's manager-add change.** The table above names no slice for dropping the
manager-add bound (§2.6, O10), though §10 tests it. This note assumes I4 carries it, as the
slice that makes everyone visible; I14a then puts the predicate in front of it.

## 10. Test plan

**Schema (I1).** Golden fixtures for every new type; `Validate` tables (each rule of §3);
strict decode (unknown field refused on requests); vocabularies emitted (`JoinWarning` grows
by one); v1 fixtures byte-identical; `scripts/schema-compat.sh`; `scripts/schema-ts-check.sh`.

**Store and migration (I2).** Migrate a database shaped like Nick's alpha (two channels,
one in `open_channels`, members with and without bounds, an operator, a disabled human, a
converted guest, **an ended guest**, an active guest in a channel that becomes private, open
`personal` and `open` invites, one guest-admitting invite naming the now-private channel)
and assert: #general public and default, the other channel private with its members;
the active guest removed from the private channel (`member_removed`) and the guest-admitting
invite stripped or closed; memberships added for every principal in *P* with `can_decide =
0` and `history_after NULL`, the converted guest included, the ended and active guests
excluded; member bounds cleared and copied to `legacy_history_bounds`, guest bounds kept;
`voice_allowed`/`may_invite` 1 for *P* (disabled human included) and unchanged for both
guests; invites `legacy`; settings mapped (`guest`→false, `account`/`both`→true, default
expiry 604800); exactly one audit row, with the pinned-trail fixtures updated; the two
triggers refuse a private default channel. `TestMessageReadsStayInTheVisibilityFunction`
still passes.

**Server routes (I3a–I7).** Table-driven, mirroring `TestVoiceSessionRefusals` and the
isolation style: for each new route, every caller class (member, operator, non-owner
operator on an owner, manager, guest, **ended guest with a replayed token**, agent,
unauthenticated) and every target class (public joined, public not joined, private joined,
private not joined, nonexistent), the status, code and body identity (a private channel a
caller is not in and a ghost answer the same bytes). Specific:
- create: creator has `manages = 1`, `can_decide = 1`; agent and guest 403; private, public, reserved and look-alike collisions answer one 409 body; the attempt limit trips at the cap with `Retry-After`, the refusal audit row has no name; operators are unlimited;
- settings: private default 409; turning private removes guests, strips guest links, closes one left empty, keeps members, and unmarking the last default turns open sign-up off, all in one transaction and all audited;
- self-join is idempotent; writes `can_decide = 0`, no bound; refused by a ban (once M3 lands); private 404 identical to a ghost;
- leave: the last member of a private channel gets 409; the last manager may leave (operators remain); leaving a default channel is allowed;
- directory: a member sees public channels and own private ones, never others' private; `joined`/`manages` correct; `?scope=all` operators only; members list excludes guests (active and ended), disabled, hidden; operator fields only for operators; guest and agent 403;
- voice permission: off removes `p<id>`, `p<id>.screen` and `p<id>.view` at once through the immediate hook (one `voice_participant_removed` row each) and a held share goes with them; with the immediate removal failing (LiveKit fake refusing), the next pass removes them; **a token minted before the flip** connects and is removed at the next pass; no room is rotated; `voiceStillEntitled` refuses a session/transmit/share/companion request after the flip; on changes nothing live; agent 409; owner target by non-owner operator refused; audit row; `whoami.voice_allowed` follows;
- `invites.link` keying: a member link and a guest link, both stored `kind = 'open'`, are told apart by every rule: the allowance counts member links only, an uncapped one included; brake-off closes member links and not an operator's guest links; demoting an operator closes their guest links and leaves their member links (brake on); `effectiveChannelsTx` gives a member link the default channels and a guest link its public channels;
- member link: any member creates within the allowance (counted on `link = 'member'`); brake off → 403 and the member's open links are closed and audited; `never_expires` by a member 403, by an operator stored as the sentinel and omitted on the wire; default expiry applied; redemption lands in the default channels as of that moment (zero defaults allowed) with no bound, voice on, `may_invite` on, `invited_by` set; maker sees own joiners, non-makers/guests/agents 404; close by maker and by operator; `makerStillAllowed` fails for a maker who became an ended guest, was disabled, or lost the brake;
- guest link: non-operator 403; private channel named 409; redemption re-checks visibility, drops a channel turned private (audited), answers `invite_invalid` when none is left; admits to the link's public channels only, bounded, voice per link; while `guests_enabled` is off the preview offers nothing;
- legacy invite: redeems to its still-held channels plus defaults, no bound, voice on; one with none of its own channels left is `invite_invalid`; a legacy guest-admitting invite drops private channels at redemption;
- manager add: any member but an operator or owner is added; hidden, disabled, ended guest, active guest, agent, operator, owner and unknown targets, by id and by name, answer one byte-identical 404;
- `POST /v1/invites` 409; v1 list/preview projections incl. the sentinel;
- joining settings under the chosen option: open sign-up needs a default channel; while on, `POST /v1/join/account` makes a member exactly as a member link does; `POST /v1/join/guest` always `guests_disabled`; Option A: v1 GET projection, v1 PUT 409; Option B: `guest`/`both`, `join_voice: false` 409, `open_channels` marks defaults;
- conversion: asserts no private kept membership, clears bounds, sets voice and brake on, changes nothing else (`can_decide = 0`, `manages = 0`).

**Approval path (I2, I3a, I3b, I6b, I7).** Each ships the mandated end-to-end test: request →
notify → resolve → audit, with a member who self-joined (cannot decide), a creator (can
decide in their channel), a converted guest (cannot), and quorum unchanged by each.

**Isolation (I3a–I7).** Each slice adds its rows; the amended `e2e/isolation` run passes in
CI with the private register and split markers; the coverage check fails on any new route
without a row; the oracle row documents the 409 and its limit.

**The tester story (I8, `e2e/community`).** Replays 2026-10-29 as it should go: Nick makes
a member link; a tester opens the bare address on a phone-sized client and gets only Sign
in; opens the link, creates an account, lands in #general, sees its full history, sees Nick
in the member list, sees #belgium in the directory (after Nick flipped it public), joins it,
sees its history, joins voice there. Then: an operator switches the tester's voice off and
the tester is out of the room within the bound and back after it is switched on; the
operator closes the link and it no longer works; a guest link admits a visitor to #belgium
only, bounded, without the member list; #belgium turned private drops the visitor.

**Browser (I9a–c).** Unit tests with mocked fetch for every new panel and control;
Playwright for the tester story's browser half, phone layout included; `make web-check`.

**CLI and TUI (I11a–b).** Hostile-server tests for every new command (as `hostile_test.go`
does); parity test that every new REST route has a CLI verb; TUI pane tests.

**Hide members (I14a, I14b).** §13.11 lists the required tests; the isolation rows are
§13.9.

## 11. Open points for Nick

**Answered 2026-10-30.** Nick: "accept recommendations". Every point below takes its recommended reading. For O13 that is the invite bump plus Option B for the joining settings (§3.4), the general reviewer's lean. ADR-013 records it.

Each is a consequence of a decision, or a gap the decision did not reach. The design above
takes the recommended reading and marks it "(O#)" wherever it bites; ADR-013's Decision
text marks the same clauses and is finalised from Nick's answers before acceptance. The
security and general reviewers' recommendations (round 1) are recorded beside each.

- **O1. "Only operators" turn on open sign-up: every operator, or owners?** ADR-010 put
  server-wide joining policy in the owner class (`PUT /v1/join/settings` is `accessOwner`
  today). Owners are operators, so "only operators" is satisfied either way; the design
  keeps the stricter class. *Reviewers: owner class.*
- **O2. May a member remove the people their link brought in?** Giving a member a route
  that disables other accounts server-wide is an escalation; the design gives members
  see-and-close and keeps removal with operators. Decision 1's "so you can see who brought
  whom, switch the link off, or remove the people it brought in" can be read as the maker;
  the design reads the removal as an operator's. *Reviewers: no.*
- **O3. A member link with no expiry.** As decided ("optional expiry"), but
  `invites.expires_at` is `NOT NULL` and a never-expiring reusable door made by any member
  needs a floor. Designed: a server setting `member_link_default_expiry_seconds` (default
  7 days) applied when a link names no expiry; `never_expires` and setting the default to 0
  are operators-only; "never" stored as the `9999-12-31` sentinel. The preview reveals the
  inviter and the default channels to anyone holding a link, for as long as it lives.
  *Reviewers: yes, 7 days.*
- **O4. Does every member see who the operators are?** The directory carries `role` to
  every member. *Reviewers: yes.*
- **O5. May a channel manager change its channel's visibility or default mark?** Designed
  as operator-only. Decision 6 says private channels "are managed by their managers and
  operators", which covers adding people for certain and visibility only arguably; the
  default mark is the operators' by the same decision. *Reviewers: operator-only.*
- **O6. Do existing non-open channels become public or private?** Decision 7b says public;
  on a server running ADR-008's model with several groups that exposes every group's channel
  with full history in one restart. Designed: **private**, with its current members; the
  operator flips the ones that should be public (one call each; #belgium on the alpha).
  *Reviewers (both): private.*
- **O7. Conversion clears the history bound.** Decision 4 says members see everything
  however they got there; a converted guest is a member. ADR-009 D3 said conversion keeps
  the bound. Under the new model a visitor is only ever in public channels, which
  `ConvertGuest` now asserts before clearing. *Reviewers: yes, with the assertion.*
- **O8. Guests and the channel directory.** Designed: guests see no directory and cannot
  self-join; their channels are the link's. *Reviewers: as designed.*
- **O9. The creator of a channel becomes its manager**, with `can_decide = 1` in it.
  *Reviewers: yes.*
- **O10. The manager-add bound is dropped**; a manager adds any member, and every other id
  or name, hidden principals included, answers one uniform 404. *Reviewers: drop, with the
  uniform 404.*
- **O11. Agents and the member directory.** Listed, cannot read it. *Reviewers: as
  designed.*
- **O12. Default channels and `can_decide = 0`** for everyone joining them, operators
  included (they set their own with the existing route). *Reviewers: yes.*
- **O13. The version bumps.** The invite family bump is needed (`InviteV1.expires_at`
  cannot say none; `CreateInviteRequestV1` requires channels). The joining-settings bump is
  **optional**: §3.4 gives Option A (v2) and Option B (v1 additive with a mapping and two
  refused values). *Reviewers: the invite bump; join settings optional, the general
  reviewer leans B.*
- **O14. Hide members from each other (#964, §13).** Added 2026-10-29, after round 1; no
  reviewer has seen it yet. Four parts:
  - *(a) Default channels while the setting is on: refuse or warn?* Everyone in a default
    channel sees everyone in it, so a default channel undoes most of the setting.
    Designed: **warn** (`default_channel_shared`) when the setting is turned on with a
    default channel and when a channel is marked default while it is on; recommend zero
    default channels in the description. Refusing would forbid open sign-up (it needs a
    default channel, §2.1), would refuse on every migrated server (open channels become
    default channels, §6), and would forbid a lobby the operator wants.
  - *(b) Does a member know the people their own links brought in?* #964's predicate says
    no: only self and shared channels. Decision 1 says a maker sees who they brought; and
    with zero default channels a joiner shares no channel with anyone, so without this the
    maker's joiner list is empty and the maker cannot add their joiner to a channel they
    manage. Designed: **yes, one way** (the maker knows the joiner; the joiner knows the
    maker only by the name the preview showed, and in the directory only through a shared
    channel), §13.2 clause 5.
  - *(c) Does the setting need O10's uniform 404?* Designed: **yes, whatever O10's answer**.
    With the setting on, "exists but you may not know it" must answer exactly as "never
    existed" on every id- or name-addressed member route (§13.4); if ADR-010's manager-add
    bound is kept instead of dropped, the bound and the predicate coincide for managers and
    the uniform body is still required.
  - *(d) Historical identity.* Designed: a message's author stays named in its channel's
    history for everyone who can read the message, through a new channel-scoped authors
    route, under the author's current name (§13.5). The alternative is today's rendering
    (`#<id>` once the author is no longer in a shared channel). Recommended as designed.

  Who may set it follows O1 (owner class under its recommendation); it asks nothing new.

**Residuals recorded, no decision asked:**

- **R1.** A guest reads the roster of a public channel it visits (`GET …/members`, today's
  rule) and so learns those members' names. Bounded to the channels the link named.
- **R2.** A name collision on create is an existence oracle on private channel names,
  bounded by the attempt limit and audit (§5).
- **R3.** Id gaps leak activity volume (#500).
- **R4.** Operators learn every channel's existence, name and member count (`?scope=all`,
  settings, rename), and none of a private channel's content they are not a member of.
- **R5.** With `hide_members` on, principal names stay unique, so a 409 `principal_exists`
  on self-rename, member-link redemption or open sign-up tells the caller that some
  principal's name reads like the one tried. Bounded by the existing rename and redemption
  limits (§13.4).
- **R6.** With `hide_members` on, the authors route shows a departed author's **current**
  name on their old messages, renames included, to everyone who can read those messages
  (§13.5).

## 12. What in the current design contradicts the decisions, or makes one costly

- **ADR-003 and ADR-010 §3.3: channel membership bounds what a person can learn about
  people.** Reversed for people by decision 3; kept for channels. The isolation check and
  ADR-008's exit step 3 encode the old rule and must be amended (§8).
- **ADR-008 §4: an invite grants only what its maker holds, named channels.** Replaced by
  member links that grant the default channels. `InviteV1.expires_at` is non-pointer and
  `CreateInviteRequestV1` requires channels; that is why v2 is a new version, not a field.
- **ADR-009 D1: bare-address modes and a guest switch across every entry point.** The modes
  collapse; the switch stays but governs guest links only.
- **ADR-009 D3 and guests-and-accounts.md §3.3/§4.3: conversion keeps the bound.**
  Contradicts decision 4; O7.
- **ADR-010 "creators own their channels: rejected" and the manager-add bound.** Both rested
  on operator-only creation and hidden people; both move (O9, O10).
- **`voice_allowed` has no route and `history_after` has no widening route.** The exact
  gaps the tester hit; filled by I5 and by clearing bounds for members.
- **The isolation check's markers treat names as secrets.** Costly: the world, markers and
  matrix change, spread over I3a–I7; unavoidable under decision 3.
- **`open_channels` and four-mode `open_joining` are wired through
  `guest_policy.go`, the settings page and the CLI.** Retiring them is most of I7 and part
  of I9c/I11a; Option B's mapping keeps old clients working meanwhile.
- **`invites.expires_at` is `NOT NULL`** and the published `InviteV1.expires_at` cannot be
  absent; "no expiry" needs a sentinel in the store and a pointer on the v2 wire.

## 13. Optional: hide members from each other (#964)

Added 2026-10-29 from issue #964, which Nick filed after §§0–12 were written. It changes
none of his decisions: the setting is off by default, so decision 3 (the member list; #964
calls it decision 4, counting the model as decision 1) is what a server does unless an
operator turns this on. Where a behaviour waits on Nick it is marked "(O14)".

### 13.1 The setting

- **Name and default.** `hide_members`, a boolean, default `false`, stored as one
  `server_settings` row. An absent row reads false, so no migration is needed and a server
  that never touches it behaves exactly as §§1–12 say.
- **Where it lives.** `GET /v1/directory/settings` (`accessOperator`) answers
  `DirectorySettingsResponseV1{settings: DirectorySettingsV1{hide_members}, warnings}`.
  `PUT /v1/directory/settings`, body `PutDirectorySettingsRequestV1{hide_members *bool}`,
  is in the class O1 settles for server-wide policy (`accessOwner` under its
  recommendation) and in `reauthAlways`, as `PUT /v1/join/settings` is. It is a route of its
  own beside the member directory, not a field of the joining settings, so it does not wait
  on O13 and is not read as joining policy.
- **Audit.** `directory_settings_changed`, actor the caller, subject `server`, detail
  `hide_members=<old>-><new>`. A PUT that changes nothing writes no row.
- **What members learn of it.** Not the settings route: the directory's own answer says
  which list it is (`ServerMembersResponseV1.scope`, §13.3).
- **Description**, the text the browser's Settings, `conch admin directory` and
  docs/deploy.md carry, in substance:

  > Hide members from each other. Members see only themselves and the people they share a
  > channel with. Operators always see everyone. Everyone in a channel sees everyone else in
  > it, so a default channel, or a public channel two people both join, shows them to each
  > other: this setting cannot keep apart one-on-one spaces whose people also share such a
  > channel. It hides people in Conch's answers; it does not make anyone anonymous and does
  > not separate anyone on the server.

### 13.2 The predicate, defined once

A viewer M **may know** a principal P when any of these holds:

1. `hide_members` is off;
2. M is an operator (the role as it is at the request);
3. P is M;
4. M and P each have a `channel_members` row in at least one common channel, public or
   private;
5. P joined through a link M made (`principals.invited_by = M`), while M is a member (O14b).

It is written once in the store, beside `isMemberTx`, as one function and one SQL fragment
(`knowsTx`, `knownPrincipalsSQL`), and every member-facing read of principals goes through
it. A structural guard test in the style of `TestMessageReadsStayInTheVisibilityFunction`
fails if a member-facing query over `principals` does not.

On top of it:

- **It narrows; it never widens.** Each route applies its own rule first (the directory: the
  §1.1 members and the enabled, non-hidden agents, guests and agents refused as readers;
  manager add: addable members only), then the predicate. Nothing a server with the setting
  off refuses becomes reachable with it on.
- **Guests.** As readers, unchanged: refused from the directory, bounded to the channels
  they visit (R1). As targets, a guest is never in the directory and is known only to the
  co-members of the channels it visits, which is today's rule.
- **Agents.** As targets, listed only to members who share a channel with them. As readers,
  unchanged: no directory (O11), channel content by manifest.
- **Operators.** Clause 2: the full directory with its operator fields, `GET /v1/principals`,
  `?scope=all`. The setting does not list operators to everyone; an operator who wants to be
  found shares a channel with the members. O4's role display applies only to the principals
  a member may know.
- **Channel visibility** is untouched. The members of a private channel know each other
  because they share it; to anyone else the channel is the same 404 as today.

**The #964 example.** The owner is in private A and B, alice only in A, bob only in B, the
setting on. alice may know {owner, alice}; bob {owner, bob}; the owner, an operator,
everyone. If alice and bob both join public C, each may know the other.

### 13.3 Every read path, with the setting on

| Path | With `hide_members` on |
| --- | --- |
| `GET /v1/directory/members` | Rows are filtered by the predicate **before** paging. `after` is always the id of a returned row and is present only when another row the viewer may know follows, so a page's size and cursor are what they would be on a server where the unknown principals did not exist. No total is carried (none is designed). `ServerMembersResponseV1` gains `scope` (`DirectoryScope`): `everyone` (setting off, or an operator) or `shared_channels` (setting on, any other member), so a client says "you see the people you share a channel with" instead of implying the list is the server. |
| Member panels, @mentions and autocomplete | No server route resolves a mention, completes a name or searches people (`routes.go` has none; `GET /v2/channels/{channel}/messages/search` is channel-scoped and matches message text). Clients draw candidates from the directory and the channel member lists, so they inherit the predicate. Any later route that looks up or searches principals goes through the predicate and answers an unknown one as §13.4 says; that is an acceptance criterion of that route. |
| `GET /v1/channels/{channel}/members`, `conch.channel_members.v1` | Unchanged. Everyone on a channel's list shares that channel with the viewer, so every row passes the predicate; the channel gate (404 to a non-member) is what bounds it. |
| Session rosters: `GET /v1/sessions?channel=`, `conch.session_roster.v1` | Unchanged, for the same reason: the roster is the sessions of the channel's members (`channelRoster`). |
| Voice presence: `GET /v1/channels/{channel}/voice`, `GET /v1/voice/ws`, `conch.voice_presence.v1` | Unchanged: participants of the room of a channel the viewer is in, who are its members. |
| Voice occupancy: `GET /v1/voice/occupancy`, `GET /v1/voice/occupancy/ws`, `conch.voice_occupancy.v1` | Unchanged: counts for the viewer's own channels only, a channel the viewer may not read absent rather than zero (`voice_occupancy.go`). |
| Join and leave notices | No channel carries a join or leave message today; the notice is `conch.channel_members.v1` to the channel's own subscribers, its co-members, and the removed member's sockets closing. Unchanged. The setting adds the directory signal of §13.6, sent only to the viewers whose answer changed. |
| Counts and pagination totals | No member-facing count of principals exists or is designed: the member view of `GET /v1/directory/channels` has none (§5), occupancy counts only the viewer's channels, the directory has no total. `?scope=all`'s `members` is operators'. `InviteV2.joiners` counts the joiners the maker may know (with O14b, all of them). `InviteV2.uses` stays: it counts redemptions of the maker's own link, which the maker needs to see a use cap being spent. |
| `GET /v2/invites/{id}/joiners` | The maker sees their link's joiners (O14b; without it, the list and `InviteV2.joiners` are filtered by the predicate). Anyone else 404 `invite_not_found` (§3.3); operators everything. |
| Invite previews, `POST /v2/invites/preview` | Unchanged: the inviter's name, the server name, the default channels. The inviter disclosed their own name by handing out the code; the preview names nobody else. |
| Manager add: `PUT /v1/channels/{channel}/members/{principal_id}`, `POST /v1/channels/{channel}/members` | A manager adds a member it may know; anyone else, by id or by name, is the one 404 `principal_not_found` (§13.4). With the setting on this is ADR-010's old bound back, by the predicate rather than a rule of its own. |
| `DELETE /v1/channels/{channel}/members/{principal_id}` | Unchanged: its targets are the channel's members, whom a manager knows. A non-member id answers the same whether or not the principal exists (tested in I14a). |
| `PUT /v1/principals/{id}/name`, `GET /v1/principals/{id}/manifest`, `POST /v1/principals/{id}/credentials/revoke-all` | Unchanged: `accessOperatorOrSelf` handlers refuse any id but the caller's own with 403 before looking it up (`handleRenamePrincipal`, `handleGetManifest`), so they carry no existence signal. |
| Whisper audiences on `POST /v2/channels/{channel}/messages` | Unchanged: an id that is not a member of the channel and one that does not exist both answer `invalid_audience` (`messages.go`). |
| Messages, search, threads, files, reactions, approvals, runs | Unchanged: channel content through `messageVisible` and the channel gate. Naming departed authors: §13.5. |
| `GET /v1/whoami` | The caller only. Unchanged. |
| Operator routes: `GET /v1/principals`, `GET /v1/guests`, `GET /v1/invites/{id}/principals`, `GET /v1/audit`, `?scope=all` | Unchanged: operators are outside the predicate. |

So the setting bites in few places. Almost every surface was already bounded by channel
membership (ADR-003); the community-wide ones are the directory, manager add, the joiner
list and, through §13.5, attribution.

### 13.4 An unknown or hidden principal

One answer for every principal a viewer may not know, whatever the reason: it never
existed, is hidden or disabled, is an active or ended guest, is an operator or owner out of
a manager's reach, or is unknown under this setting.

- **Lists** (the directory, the joiner list) omit it and change nothing else on the page
  (§13.3's cursor rule).
- **Id- and name-addressed member routes** answer the byte-identical 404
  `principal_not_found` body O10 recommends, which is what `AddChannelMemberByName`'s
  `ErrNotAddable` answers today (`members.go`), for an id beyond the sequence too.
- **The authors route** omits it (§13.5).

This is O10's uniform 404, and the setting needs it whatever O10's answer (O14c).

**Not closed, and recorded (R5).** Principal names are unique by `name_key` and `name_skel`,
so a 409 `principal_exists` on self-rename (`PUT /v1/principals/{id}/name`, limited by
`RenameLimits`), on member-link redemption (`POST /v2/invites/redeem`) or on open sign-up
(`POST /v1/join/account`, rate-limited as today) tells the caller that a principal has a
name that reads like the one tried. As for channel names (R2) it is bounded by the existing
limits. Closing it would mean names that are not unique, which this setting does not ask.

### 13.5 Historical identity

**Decided: a message's author stays named in the history of the channel it was posted in,
for everyone who can read the message, whatever the setting. Leaving the channel, or
turning the setting on, rewrites nothing.**

*Today.* `MessageV2` carries `author_id` only (and `guest_author` for a guest's message,
stored with it). Clients name authors from the channel's member list; the browser's
`nameOf` (`web/src/Conversation.tsx`) falls back to `#<id>`. So an author who has left the
channel already shows as a number, unless the viewer meets them in another channel. With
the setting off the directory would fill that gap; with it on, it would not.

*The route.* `GET /v1/channels/{channel}/authors?ids=<id>,<id>…`, at most
`MaxChannelAuthorIDs` (100) ids, behind the channel gate of `GET …/members` (404 to a
non-member; an agent needs `messages.read` with read). It answers
`ChannelAuthorsResponseV1{authors: []ChannelAuthorV1{principal_id, name, kind}}`, ordered by
id, for each asked id that authored at least one message the caller may read in that
channel, through `messageVisible` (history bound, audience, thread). Every other id, known
or not, existing or not, is absent. Name and kind only: no role, no join date, no channels,
no voice. The rule does not depend on the setting.

*Why.*

- Decision 4 gives members the full history of their channels, so a channel's history is a
  record its members share, past and future, and who said what is part of it. A message
  that reads "#41: approve the payment" to one member and "bob: approve the payment" to
  another is a worse record, and approval and report context depend on it.
- #964 asks that attribution and history stay intact. Ids alone keep it intact for
  programs, not for people.
- The route discloses only what the message disclosed to the channel: that this author
  posted here.

*What it leaks (R6).* The **current** name: a departed author's later renames show on their
old messages. And to a member who joins later, the names of everyone who posted before
them. Not leaked: whether the author is still on the server, disabled, in other channels or
in voice. A hidden author's name is already a placeholder (`HiddenPrincipalNamePrefix`,
`store/hide.go`). Storing a name with every message would close the rename leak, at the
cost of a change to every message write and a backfill; the setting does not justify it.

*Not covered.* Departed requesters and deciders of approvals, principals of runs and
uploaders of files still show as ids, as today.

### 13.6 Live refresh

- **The document.** `conch.directory_changed.v1`, `DirectoryChangedV1{schema}`: no
  principal, no count, no reason. It tells a client that its directory answer may have
  changed and that it must re-read `GET /v1/directory/members` and **replace**, never
  merge, its people cache and autocomplete candidates, dropping every name it holds that
  the new list lacks and the authors route does not answer for that channel. A document
  with no content cannot leak through a race, since the subscriber's goroutine reads
  nothing to send it; and the directory is paged, so the whole-list document I10 first
  sketched would not fit. I10 is amended to this signal.
- **The channel.** It travels on the viewer's events sockets (`GET /v1/events/ws`), the way
  read-state frames reach a principal across its memberships, coalesced under the roster's
  floor (`defaultEventsRosterFloor`, 250 ms).
- **Who gets it.** Only the viewers whose directory answer changes. A signal sent to
  everyone would itself tell alice that something happened among people she cannot see.
  - The setting toggled: every member's and operator's sockets.
  - A membership added or removed (an add, a self-join, a leave, a removal, a redemption, a
    conversion, the removal of guests from a channel turned private): with the setting on,
    the subject, each member of the channel whose knowing of the subject changed (the
    predicate after commit against before), and operators; with it off, nobody, since
    membership does not change the directory then.
  - A principal created, renamed, disabled, enabled, hidden, or its role changed: every
    viewer who may know it (with the setting off, every member).
- **No server cache to drop.** The directory is read per request, so the first answer after
  a toggle is already filtered; a client without an events socket (the CLI) gets the new
  list on its next read. Event subscriptions' membership sets are updated on removal as
  today (`setMemberships`, `DropPrincipal`).
- **The limit.** A client that ignores the signal keeps what it was sent. The server stops
  serving; it cannot make a client forget (§13.8).

### 13.7 Default and public channels

A member is in every default channel they have not left, so with a default channel the
setting hides almost nothing: everyone in #general sees everyone in #general. A public
channel does the same for those who join it.

- The setting's description says so (§13.1).
- **Recommended configuration:** no default channel while the setting is on, and a private
  channel for each space whose people should not see each other. Zero default channels is
  allowed (§2.1: the joiner lands in no channel, and the joining settings warn
  `no_default_channel`).
- **Warn; do not refuse (O14a).** Turning the setting on while a default channel exists, and
  marking a channel default while it is on, both succeed and answer the warning
  `default_channel_shared` (a new `DirectoryWarning` vocabulary, closed and client-tolerant
  as `JoinWarning` is), in `DirectorySettingsResponseV1.warnings` and in
  `ChannelSettingsResponseV1.warnings`. Refusing would interlock with open sign-up, which
  needs a default channel (§2.1); would refuse on every migrated server, whose open channels
  became default channels (§6); and would forbid a lobby the operator wants everyone in. The
  setting promises only the predicate, and the warning says what a default channel does to
  it.
- **Consequence.** With no default channel, a member-link joiner shares no channel with
  anyone. An operator adds them, the link's maker adds them to a channel the maker manages
  (O14b), or they join a public channel and are seen there.

### 13.8 What it hides and what it does not

**Hides**, from an ordinary member, while on: the existence, ids, names, roles, join dates
and agent status of the principals who share no channel with them and whom their own links
did not bring in; and, by the targeted signal, when those principals change.

**Does not hide:**

- anyone in a channel both are in, default and public channels included: joining a public
  channel shows you to its members and them to you;
- what is said in a shared channel, including about people the reader cannot see;
- the authors of the messages the reader can read, under their current names (R6);
- that a name is taken, through a collision (R5);
- how many principals, messages and files exist: ids are global sequences (R3, #500);
- the inviter's name, to anyone holding their code; how often one's own link was used;
- anything from operators: they see every principal, every channel's name and count, and
  (owners) the audit log;
- the names of public channels, in the channel directory;
- what a client was sent before a toggle or a removal.

**Promises no anonymity**: operators and the server know who everyone is, and names are what
people chose. **Promises no isolation**: one process, one database and one LiveKit serve
everyone. Groups that must not learn of each other at all still need what ADR-008 gave
them, separate private channels, and separate servers for anything stronger.

### 13.9 Isolation-matrix rows

`e2e/isolation` gains a phase with `hide_members` on. Its world: the owner (an operator) in
private A and B; alice in A only, A's manager; bob in B only; carol, who joined through
alice's member link and is in no channel; a guest visiting public C; an agent in A. With the
setting on, **bob's name and id are markers in alice's register, and alice's in bob's**,
except where a row below documents them. Every route in the table has a row in both phases
(`TestEveryRouteHasARow`). A "ghost" is the matrix's id or name that never existed.

| Row (route, what) | Viewer | Setting off | Setting on |
| --- | --- | --- | --- |
| `GET /v1/directory/members`, lists the directory | alice | owner, alice, bob, carol, agent; `scope: everyone` | owner, alice, carol (O14b), agent; `scope: shared_channels`; no bob marker |
| `GET /v1/directory/members`, lists the directory | bob | everyone, as alice | owner, bob; no alice, carol or agent |
| `GET /v1/directory/members`, lists the directory | owner | everyone, operator fields | the same, `scope: everyone` |
| `GET /v1/directory/members`, lists the directory | guest, agent | 403 | 403, same bytes |
| `GET /v1/directory/members?limit=1`, pages the directory | alice | one row a page | every `after` is a listed id; the last page has none; bob's id is in no cursor |
| `PUT /v1/channels/{channel}/members/{principal_id}`, adds bob to A by id | alice | 200 (O10) | 404 `principal_not_found`, bytes identical to a ghost id |
| `POST /v1/channels/{channel}/members`, adds bob to A by name | alice | 200 (O10) | 404, bytes identical to a ghost name |
| `PUT /v1/channels/{channel}/members/{principal_id}`, adds carol to A | alice | 200 | 200 (O14b; the ghost's 404 without it) |
| `GET /v2/invites/{id}/joiners`, alice's link | alice | carol | carol (O14b) |
| `GET /v2/invites/{id}/joiners`, bob's link | alice | 404 `invite_not_found` | the same |
| `GET /v1/channels/{channel}/authors`, a departed author of A, bob's id, a ghost id | alice | the departed author named; bob and the ghost absent | the same |
| `GET /v1/channels/{channel}/members`, public C after alice and bob join it | alice | bob listed | bob listed: documented, a shared channel shows its people |
| `GET /v1/voice/occupancy`, `GET /v1/channels/{channel}/voice` | alice | A only; B absent | the same; bob's id in no frame |
| `GET /v1/events/ws`, bob joins B | alice; owner | no signal to either | no signal to alice; one to the owner |
| `GET /v1/events/ws`, bob joins C, where alice is | alice | no signal | one signal; the re-read lists bob |
| `GET /v1/events/ws`, the owner toggles the setting | alice, bob | one each | one each; alice's re-read has no bob |
| `GET /v1/directory/settings`, `PUT /v1/directory/settings` | alice; a non-owner operator; owner | alice 403; operator GET 200, PUT per O1; owner 200 and one `directory_settings_changed` row | the same |
| `PUT /v1/channels/{id}/settings`, the owner marks C default | owner | 200, no warning | 200 with `default_channel_shared` |
| `PUT /v1/principals/{id}/name`, alice renames herself to bob's name | alice | 409 `principal_exists` | the same: documented oracle (R5), limited |
| `POST /v2/invites/redeem`, alice's link redeemed with bob's name | unauthenticated | 409 `principal_exists` | the same: documented oracle (R5) |

### 13.10 Schema

**No version bump.** Everything is new or additive:

- `DirectorySettingsV1`, `PutDirectorySettingsRequestV1`, `DirectorySettingsResponseV1`;
  `DirectoryWarning` (closed, client-tolerant, one value `default_channel_shared`): new.
- `DirectoryScope` (`everyone`, `shared_channels`) and `ServerMembersResponseV1.scope`;
  `ChannelSettingsResponseV1.warnings`: new fields of types I1 introduces; additive optional
  fields if I1 has merged.
- `ChannelAuthorV1`, `ChannelAuthorsResponseV1`, `MaxChannelAuthorIDs`: new.
- `DirectoryChangedV1`, `conch.directory_changed.v1`: a new events document, in place of
  I10's unpublished `conch.directory_members.v1`.
- No new error code.

It rides I1 if I1 has not started when ADR-013 is accepted, so there is one schema PR;
otherwise it is an additive schema commit inside I14a, under the `schema-change` skill.
O13's question is unaffected either way.

### 13.11 Issues

**Existing issues it touches.**

- **I1:** carries the §13.10 shapes if it has not started.
- **I3a:** `PUT /v1/channels/{id}/settings` answers `default_channel_shared` while the
  setting is on; channel creation's membership write emits the signal through I14a's hook.
- **I3b:** self-join and leave emit the signal.
- **I4:** the directory query goes through the predicate from its first version (true while
  the setting does not exist) and fills `scope`; and, as §9's note assumes, it drops O10's
  manager-add bound, behind which I14a puts the predicate.
- **I6a:** the joiner list and `InviteV2.joiners` go through the predicate (O14b).
- **I6b, I7:** redemption, open sign-up and conversion write memberships and emit the signal.
- **I9b:** the member panel shows the scope line, replaces its people cache on the signal,
  and names unnamed authors through the authors route.
- **I9c:** the setting in Settings, with §13.1's description and the warning.
- **I10:** amended to the content-free signal (§9).
- **I11a:** `conch admin directory settings [--hide-members on|off]`, printing the warning.
- **I12:** docs/deploy.md carries §13.1's description.

**I14a. server: hide members from each other (#964).** Size M; labels `security` (it
writes no membership, so it is not `approval-path`). Depends on ADR-013 accepted and O14
answered, then I4 (the directory and the manager-add change), I3b (self-join and leave are
the membership changes most likely to move a viewer's set) and I6a (the joiner list). It
builds I10's server half if I10 has not landed.

*Files:* `pkg/schema/directory.go`, a new `pkg/schema/channel_author.go`, `vocabularies.go`,
`ts.go`, `pkg/schema/testdata/` (unless I1 carried them); `internal/server/store/` (the
setting's reader and writer, `knowsTx` and `knownPrincipalsSQL` beside `isMemberTx`, the
authors query); `internal/server/routes.go` (two directory-settings routes, the authors
route, the reauth entry); `internal/server/directory.go` (I4's handler); `members.go`
(manager add); the v2 invite handlers (joiners); `events_ws.go` (the `eventDirectory` kind
and its targeting); `channels.go` (the warning); a new guard test beside
`visibility_guard_test.go`; `e2e/isolation/world.go`, `markers.go`, `matrix.go`.

*Acceptance criteria:*

- `hide_members` defaults to false and an absent row reads false; with it off, every answer
  is byte-identical to the server before I14a (the existing directory, members and invite
  tests pass unchanged).
- `GET /v1/directory/settings` is operators'; `PUT` is O1's class, needs recent
  authentication, writes one `directory_settings_changed` row per change and none for a
  no-op, and answers `default_channel_shared` when a default channel exists.
- The predicate is one store function and one SQL fragment; the guard test fails on a
  member-facing query over `principals` that bypasses it.
- With it on, the directory, manager add and the joiner list answer exactly as §13.3 and
  §13.4 say; `scope` is right for each viewer; every `after` is a listed id.
- Unknown and not-known principals answer one byte-identical 404 on every id- and
  name-addressed member route.
- `GET /v1/channels/{channel}/authors` answers per §13.5 and goes through `messageVisible`.
- `conch.directory_changed.v1` reaches exactly the viewers §13.6 lists, coalesced under the
  floor, and never one whose answer did not change.
- Marking a channel default while the setting is on answers `default_channel_shared`.
- The isolation phase of §13.9 passes in CI with bob's name and id as markers for alice.

*Required tests:*

- Store, table-driven: every predicate clause alone and combined; setting off; an operator
  viewer; a demoted operator (the role at the request); public and private shared channels;
  the inviter clause (O14b); disabled, hidden and guest targets excluded by the base set
  whatever the predicate says.
- Directory: the #964 example (owner, alice, bob, plus carol and the agent); paging with
  `limit=1` showing no cursor at an unlisted id; guest and agent 403; operator sees all.
- Manager add by id and by name: bob, a ghost, a hidden principal and an ended guest answer
  identical bytes; carol is added under O14b.
- Remove: a non-member id that exists and one that does not answer the same.
- Authors route: a departed author named; a whisper not to the caller does not count; a
  guest's history bound is respected; ids over the cap 400 `invalid_request`; a non-member
  gets the ghost channel's 404 bytes.
- Signal: no frame to alice when bob joins B; one when bob joins C; one to everyone on a
  toggle; frames coalesce under the floor; none with the setting off for a membership change.
- Settings route: access per O1, recent authentication, audit, no-op, warning.
- A toggle on then off returns every viewer's directory to the full list.
- The isolation phase (§13.9).

**I14b. web, cli and tui: hide members.** Size S; no special label. Depends on I14a, I9b,
I9c, I11a.

*Files:* the browser's Settings and member panel and `Conversation.tsx`'s author naming;
the events socket client; `internal/cli` (`conch admin directory settings`, author naming
in history); the TUI's member pane; their tests.

*Acceptance criteria:* the setting can be read and set from the browser and the CLI with
§13.1's description and the warning shown; the member panel says "You see the people you
share a channel with" when `scope` is `shared_channels`; on `conch.directory_changed.v1`
the browser re-reads and replaces its people cache and autocomplete candidates; departed
authors are named through the authors route in the browser, the CLI and the TUI, and fall
back to the id only when the route does not answer.

*Required tests:* unit tests with mocked fetch for the toggle, the scope line, the cache
replacement (a name no longer in the list is gone from the panel and from autocomplete)
and author naming; one Playwright test of the toggle, phone layout included; CLI
hostile-server tests for the new verb; a TUI pane test; `make web-check`.
