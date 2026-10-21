# Guests, self-service accounts and signing in again

Issue [#694](https://github.com/njdaniel/conch/issues/694). The decision is
[ADR-009](../adr/ADR-009-guests-and-open-joining.md), which amends
[ADR-008](../adr/ADR-008-private-communications-alpha.md)'s "No public signup". This note is
the design: the flows, the model, the wire shapes to be designed through the schema-change
skill, the passkey choice, the abuse controls, the threat notes and the work in order.

**Status:** proposed with ADR-009, 2026-10-21. Nick's sign-off on the PR that adds both is the
go-ahead to file the implementation issues in §12. The wire shapes in §5 are sketches: names and
fields only, to be designed through the `schema-change` skill in the schema issue; nothing here
is `pkg/schema` code.

## 1. What this adds

Three things, each small on its own:

1. **A front door.** Someone who opens the server's bare address today gets a field for a
   credential they do not have. With this, the page says what the server offers: sign in, and,
   when an operator has turned **open joining** on, join as a guest or create an account.
2. **Guests.** A person who wants to look in, or talk once, without making an account. A guest is
   a real principal with a guest mark, narrow access, and a credential that lasts 7 days from its
   last use. A guest can keep its account at any time.
3. **A way back in.** A member can sign in again on any device with a **passkey**, or add a device
   with a one-use link they make for themselves. The operator's sign-in link (#493) stays as
   recovery.

Not added: a password (a possible later fallback), email of any kind, OIDC, a guest that can
invite, agents made by anyone but an operator, or anything that needs a process besides
`conchd`.

## 2. Decided

**By Nick** (recorded on #694, 2026-10-21, "use your recommendations"):

- **D1.** Open joining from the bare address is an operator setting, off by default. When on, it is
  rate-limited, can be paused like invites, offers guest, account or both, and every change is
  audited.
- **D2.** A guest is a real principal marked guest; its credential is kept in that one browser with a
  lifetime of 7 days of inactivity; it is scoped to what its link grants or to the server's open
  channels; voice and share-watching only if granted; it cannot invite, see members outside its
  channels or open one-on-ones; it shows with a "guest" badge; its messages stay after it
  expires; it can become a member at any time, keeping its history.
- **D3.** Passkeys (WebAuthn) plus Add a device. Password with argon2id only as a later fallback if
  wanted. The operator sign-in link stays as recovery. The WebAuthn library needs Nick's sign-off
  at an exact version. No email server is required.
- **D4.** Names are unique per server and case-insensitive, with a reserved list, and visually
  confusable names are refused.
- **D5.** Per-address rate limits (the trusted-proxy address of #609), a cap on guests at once, a
  pause-joining switch, bulk revoke of everyone from the bare address, and audit events.
- **D6.** `e2e/isolation` extends to guests and the bare-address path.

**Adopted by this note** under the same standing delegation; Nick can overrule any at review.
Each is marked *(adopted)* where it appears, and listed again in §13:

- A guest may read files in its channels but not upload them.
- A guest may rename itself under the same rules as a member.
- Passkeys and Add a device are for members. Operators keep the by-hand credential and the one-tab
  rule (D2 of #260); guests have neither. D2's "that one browser" is, in the store's terms,
  **exactly one credential**: a guest holds one, made at join, and no route makes it another.
- An expired guest that posted keeps its name, and its messages read "<name> (guest)", until the
  sweeper hides it 90 days after expiry; one that posted nothing is hidden at expiry. The hide
  frees the name as a hide always has (§4.3).
- A guest never holds the right to decide approvals, and cannot be granted it (§4.2).
- A guest from the bare address may keep its account only while open joining still offers
  accounts (§3.6).
- The QR code of Add a device is a second step behind the link itself (§7).

## 3. The flows

Every page below is the browser client at `/app/`. "The server checks" means what `conchd` does
on the route named; the routes are listed in §5. The words on screen are the intent, not the
final copy.

### 3.1 An invite link

`<origin>/app/#/invite/<code>`, as today (#440), with one more choice.

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | "You are invited to Conch": who invited you, the channels, whether voice is included, when it expires. Below: **Join as a guest**, **Create an account**, and "I already have an account: sign in". | `POST /v1/invites/preview`: per-address limit, pause switch, the code is live. Unchanged. |
| 2a | *Join as a guest*: a name field and "You can look around for as long as you keep using Conch; after 7 days away your guest visit ends. You can keep this account at any time." Join. | `POST /v1/invites/redeem` with `as: "guest"`: the same checks as today, plus the guest cap (§8). Makes a guest principal with the invite's channels and voice, `may_invite` 0, `can_decide` 0 forced, and one renewable credential with the shorter of the invite's lifetime and 7 days. |
| 2b | *Create an account*: a name field, then "Add a passkey so you can sign in again" with **Add a passkey** and "Not now". | `POST /v1/invites/redeem` (as today: a member with a 90-day renewable credential). Then, signed in, the passkey ceremony of §6.4. "Not now" leaves the account with the browser credential only; Settings keeps offering the passkey and Add a device. |
| 2c | *Sign in*: the passkey button and the credential field of §3.4. | As §3.4. |
| 3 | The lobby, on the first channel the invite granted. A guest sees the badge next to its own name and a line at the top: "You are a guest. Keep this account." | As today. |

What changes from today: step 2a is new, and `redeem` may now create a guest. Invites today always
create a full member (ADR-008 §4, `store.RedeemInvite`).

### 3.2 The bare address, open joining off

`<origin>/` redirects to `/app/`, as today.

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | "Conch at <server name>. This server is invite-only." Then **Sign in with a passkey** (only when the page can: §6.3) and "Paste a credential" (the field of today). | `GET /v1/join` (anonymous, per-address limit): answers the server's name, `open_joining: "off"` and whether passkeys are enabled. Nothing else. |
| 2 | Nothing to join. Someone with an invite or sign-in link opens that link. | A `POST /v1/join/guest` or `/v1/join/account` sent anyway answers `joining_closed`, is rate-limited like a redeem, and is audited thinned as `join_refused`. |

### 3.3 The bare address, open joining on

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | "Conch at <server name>." Then the buttons the setting offers: **Join as a guest**, **Create an account**, and under them **Sign in with a passkey** and "Paste a credential". While paused: "Joining is paused for now" and only the sign-in part. While the guest cap is reached: the guest button is replaced by "No room for more guests right now". | `GET /v1/join`: `open_joining` is `guest`, `account`, `both` or `paused`; `guests_full` when the cap is reached. |
| 2a | *Join as a guest*: a name field and the same 7-day line as §3.1. | `POST /v1/join/guest` with the name: per-address and server-wide limits (the invite buckets), the setting is on and offers guests, not paused, the guest cap, the name rules of §4.4. Makes a guest principal with `origin: address`, a member of every open channel (§4.2), voice as the setting says, one 7-day renewable credential. Audits `principal_created` with `origin=address guest=true`. |
| 2b | *Create an account*: a name field, then the passkey step of §3.1 2b. | `POST /v1/join/account`: the same limits and switches, the setting offers accounts, the name rules. Makes a member with `origin: address`, the open channels, voice as the setting says, `may_invite` 0, a 90-day renewable credential. |
| 3 | The lobby, on the first open channel. | As today. |

### 3.4 A returning person

Someone who has an account and no credential in this browser: they signed out, cleared the
browser, or are on a new device.

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | The front door of §3.2 or §3.3. **Sign in with a passkey** is shown when the server has passkeys enabled and the page is in a secure context with `PublicKeyCredential` (§6.3); otherwise the page says "Passkeys are not available here. Use Add a device from a browser where you are signed in, or ask an operator for a sign-in link." | `GET /v1/join` says whether passkeys are on. |
| 2 | *Sign in with a passkey*: the browser's own passkey prompt, with no name typed (discoverable credentials, §6.4). | `POST /v1/passkeys/login/begin` (anonymous, per-address limit): a challenge, kept server-side for 5 minutes. `POST /v1/passkeys/login/finish` (per-address limit and the passkey routes' own server-wide bucket, §8): the assertion verifies against a stored passkey; inside the immediate transaction that inserts the credential the server re-reads that the passkey is not revoked and its principal is enabled, not hidden and not a guest; sign count checked; the challenge is spent. Answers a new 90-day renewable credential, label "passkey <id>". Audits `passkey_signed_in`. A failure is one answer, `passkey_invalid`, audited thinned as `passkey_refused`. |
| 3 | *Paste a credential*: the field of today, for someone an operator gave a token. | `GET /v1/whoami`, as today. |
| 4 | *A sign-in link* (`#/signin/<code>`), from an operator (recovery, §3.7) or from their own other device (§3.5). | `POST /v1/sign-in-links/preview` and `redeem`, as today (#493). |

### 3.5 A new device: Add a device

A member signed in on device 1 wants in on device 2.

| Step | On screen (device 1) | The server checks |
| --- | --- | --- |
| 1 | Settings, "Your sign-ins" (#496): **Add a device**. "Makes a link that signs this account in once, on one other device, within 10 minutes. Anyone who opens it gets in as you: show it only to your own screen." | `POST /v1/sign-in-links/mine` (authenticated; members only, not guests, not agents, not operators): the per-principal create bucket, at most 3 open self-made links at once. Makes a sign-in link for the caller with a 10-minute expiry (max 1 hour), `created_by` the caller. Audits `sign_in_link_created` with `self=true`. |
| 2 | "Your link is ready. Anyone who sees it can sign in as you." Then **Reveal**, which shows the link, a **Copy** button and the QR code of the link (§7). While this tab is sharing its screen, Reveal is disabled and the page says why. The page never shows the code again after this view is left. | Nothing. |
| 3 | On device 2: open the link, or scan the QR. "This link signs you in as <name>, who made it for their own device. If that is not you, close this page." **Sign in**. When this browser already holds a lasting sign-in for a different person, the page asks the person to type the name `<name>` before it signs the other out and continues. | `POST /v1/sign-in-links/preview` answers the name and `self: true`; then `redeem`, exactly as #493: one use, then spent; the new credential is a 90-day renewable one with the label "added device <link id>". Audits `sign_in_link_redeemed`. |
| 4 | Device 2 is in. On device 1, "Your sign-ins" now lists "added device <id>" and the link's row is gone. | `GET /v1/credentials/mine` (as today) and `GET /v1/sign-in-links/mine`. |

On the TUI or CLI, device 2 is `conch login --link`, which reads the link or code from standard
input without echo, as `conch login` reads a token today (never from the command line, where it
would sit in the shell history and the process list), redeems it and stores the credential
(§5.4). Device 1 can also be the CLI: `conch device-link` prints the link.

### 3.6 Guest to member: Keep this account

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | The line "You are a guest. **Keep this account**" in the lobby, or the same in Settings. "Keeping it makes you a member: your name, channels and messages stay, and your sign-in no longer ends after 7 days away. Next, add a passkey so you can sign in from other devices." | Nothing yet. |
| 2 | **Keep**. | `POST /v1/principals/{id}/keep` (the caller itself, or an operator): the principal is a live guest. For a guest from the bare address keeping itself, open joining must offer accounts (`account` or `both`) and not be paused at that moment: otherwise 403 `joining_closed` or 503 `joining_paused`, audited thinned as `join_refused reason=keep_closed`. An operator's Keep bypasses that; a guest from an invite may keep freely. Clears the guest mark, keeps `origin`, channels, voice, `may_invite` 0 and `can_decide` 0 on every membership (an operator may grant it afterwards, as to any member); the current credential's renewal becomes 90 days for a guest from the address, or the invite's credential lifetime for a guest from an invite. Audits `guest_kept` with `origin`. |
| 3 | The passkey step of §3.1 2b, and "Not now". The badge is gone. | §6.4. |

A guest that expired cannot be kept: there is no credential to come back with. The person joins
again, as a new guest or a new account. A guest from the bare address that finds Keep closed sees
"This server is not taking new accounts right now; ask an operator" and stays a guest.

### 3.7 The operator's recovery link

Unchanged from #493: `conch admin principal sign-in-link <id>` makes a one-use link for a member,
the member opens it, and "Your sign-ins" shows the operator-made sign-in afterwards. It is the
way back for someone who lost every passkey and every signed-in device. The one change: a
member's own device links sit in the same table, told apart by `created_by` being the member.

## 4. The model

### 4.1 A guest principal

A row of `principals`, `kind = 'human'`, `role = 'member'`, with these additions:

| Column | Meaning |
| --- | --- |
| `guest INTEGER NOT NULL DEFAULT 0` | 1 while the principal is a guest. Cleared by Keep. |
| `origin TEXT NOT NULL DEFAULT 'operator' CHECK (origin IN ('operator', 'invite', 'address'))` | How the account was made. Existing rows: `operator`, except those with `invite_id` set, which the migration marks `invite`. |
| `guest_expired_at INTEGER` | Set by the sweeper (§4.3) when a guest's last credential ran out. Never set on a member. |

Where it fits what exists:

- `invited_by` and `invite_id` are set for a guest from an invite link, as for a member; null for a
  guest from the bare address, whose `origin` is `address`. Bulk operations select by either.
- `may_invite` is 0 and an operator cannot set it on a guest (`PUT .../invite-permission` answers
  `invalid_request` for a guest, as it does for an agent).
- `voice_allowed` is what the invite or the open-joining setting granted. It gates voice *and*
  watching a share: a guest without voice holds no session, so it cannot subscribe to the room
  (the share is a track in the room).
- `channel_members` rows are made at join with `can_decide` 0, as for invited members (#450), and
  **a guest never holds `can_decide`**: the store forces 0 on every membership insert for a
  `guest = 1` principal, whatever the caller asked (today's default in `store/members.go` is
  `decide := canDecide == nil || *canDecide`, which would grant it to a guest an operator adds by
  hand), and `PutChannelMember` / `setCanDecideTx` refuse `can_decide: true` on a guest with
  `invalid_request`, as for an agent. Keep leaves every membership at 0; an operator may grant it
  afterwards, to what is then a member, as today.
- `name`, `name_key` and `name_skel` are as for everyone (#441).

**Lifetime.** A guest holds **exactly one credential**, made at join: renewable (`renew_ms`, #439)
with a 7-day lifetime, or the invite's credential lifetime when that is shorter (the lifetime is
`min(invite lifetime, 7 days)`; a bare-address guest's is 7 days). The existing renewal rule moves
its expiry to the lifetime past each day's first use, so the credential, and with it the guest,
lasts 7 days from the last day it was used. The constants `MinCredentialRenewLifetimeSeconds` (2
days) and the 90-day default already bracket this. No second credential can be made for a guest:
Add a device, passkeys, the operator sign-in link and `POST /v1/principals/{id}/credentials` all
refuse a guest (`guest_not_allowed`).

**Expiry.** A guest is out the moment its credential is no longer live: the bearer check refuses
it, and `voiceEntitlement` requires a live credential for a `guest = 1` principal, so LiveKit
drops the guest within one poller pass (`voicePassGapMax`, 1.5 s) plus a room rotation, the same
bound the isolation check holds revocation to, not the sweeper's interval. The sweeper (§4.3) then
records the expiry. Signing out (`DELETE /v1/credentials/current`) revokes the one credential, so a
guest that signs out is gone at once; the sign-out dialog says so and offers Keep first.

### 4.2 What a guest can and cannot do (D2)

| | Guest | Member |
| --- | --- | --- |
| Read and post in its channels, threads, whispers, read state | yes | yes |
| Search | its channels | its channels |
| Read files in its channels | yes | yes |
| Upload files | **no** *(adopted)*: `guest_not_allowed` | yes |
| Voice, watch a share | if granted | if granted |
| Share a screen | if voice granted | if voice granted |
| List the members of its own channels | yes, as today for any member | yes |
| List principals, other channels' members | no, as today for any member | no |
| Make an invite | **no**, and cannot be granted the right | if granted |
| Decide approvals | **never**: `can_decide` forced 0 on every membership, and an operator cannot set it on a guest (§4.1) | no at join; an operator may grant |
| Request an approval (`POST /v1/approvals`) | **no**: `guest_not_allowed` | yes |
| Ask for an intervention, report a problem | yes | yes |
| Rename itself | yes *(adopted)*, same rules | yes |
| Your sign-ins, sign out | sees its one sign-in; signing out ends the visit (warned) | yes |
| Add a device, passkeys | no | yes |
| Be added to another channel by an operator | yes | yes |
| Keep this account | yes, while live | n/a |

"Cannot open one-on-ones": there is no DM route (D12); a one-on-one is a private channel an
operator makes. A guest cannot make channels (nobody but an operator can) and the operator UI
and CLI warn when adding a guest to a channel that is not open. Nothing more is needed.

**The badge.** Every shape that carries a principal to a client gains `guest: true` (omitted when
false, so a server that predates it reads as "not a guest"): `WhoAmIResponseV1`,
`ChannelMemberV1`, `PrincipalAdminV1`, `InviteJoinerV1`, and the invite preview's `inviter` is
never a guest. Messages carry `author_id` only, and clients name the author from the member
list, so the badge on a message comes from the member row. An expired guest stays in
`channel_members` (disabled, so it cannot act), with `expired: true` on its row, so its messages
still read "<name> (guest)" and the roster can fold expired guests away.

### 4.3 Expiry, names after expiry, and the sweeper

Being out is immediate (§4.1): a credential that is not live refuses the bearer and the voice
entitlement. A sweeper inside `conchd` (Rule 1: in-process, like the voice poller) records it
afterwards. It runs at start, and then at the earliest `expires_at` among live guest credentials
(re-read after every pass and after every guest join, and never later than 10 minutes away), so
`guest_expired` is written within seconds of the expiry rather than at a fixed interval:

1. Selects guests (`guest = 1`, `guest_expired_at IS NULL`) with no credential that is neither
   revoked nor expired.
2. For each: sets `disabled_at` and `guest_expired_at`, drops its sockets (`principalDisabled`, a
   no-op if the bearer check already did), and audits `guest_expired`. A socket an expired guest
   still holds is therefore closed within **the sweeper's wake plus the 2 s socket bound**; the
   fallback, should the sweeper be late, is the sockets' own 30 s `CredentialLive` recheck
   (`internal/server/ws.go`), which closes a socket whose credential is no longer live.
3. On the same pass, hides every guest it just expired **that has posted no message** (nothing
   to attribute, so nothing to keep): exactly as `POST /v1/principals/{id}/hide` does (#445:
   disabled, out of every channel and net, renamed `hidden-<id>`), audited `principal_hidden`
   with `reason=guest_expired`. Its name is free at once.
4. Selects guests whose `guest_expired_at` is 90 days or more ago and not yet hidden (those that
   did post), and hides each the same way.

Between expiry and the hide, a guest that posted keeps its name and its memberships, so its
messages still read "<name> (guest)" and the roster can fold it away as expired. **The name is
released only by the hide**, through the one path that has always freed names: at once for a
guest that said nothing, 90 days on for one that did; there is no second mechanism, no exception
in `claimNameTx`, and no reserved prefix beyond `hidden-`. A name a guest posted under is
therefore unavailable for up to 90 days after the guest last used Conch, which is the cost of
keeping attribution simple. *(adopted on the security review's finding S3 and re-check R1;
§13.2.)*

An operator may hide an expired guest earlier, singly or in bulk (§8). `POST
/v1/principals/{id}/enable` on an expired guest is refused (409 `invalid_request`, "an expired
guest is not re-enabled; the person joins again"), since enabling revives no credential and a
guest with no credential is nothing.

### 4.4 Names (D4)

Unchanged from #441: a name is stored and shown as typed, and two names clash when either of
their keys (case-folded, and UTS #39 skeleton) match.

Added: a **reserved list**, refused wherever a person chooses their own name (invite redemption,
join as guest, create an account, rename by oneself) and allowed only to an operator creating or
renaming a principal:

- `operator`, `operators`, `admin`, `administrator`, `root`, `system`, `conch`, `conchd`,
  `moderator`, `mod`, `staff`, `support`, `help`, `security`, `guest`, `guests`, `anonymous`,
  `nobody`, `everyone`, `here`, `channel`;
- the server's own name (the `server_name` setting of §8) and its host name;
- any name whose key starts with `hidden-` (already reserved, #445).

The comparison is by key, so `Admin`, `admın` and `ADMIN` are all reserved. The answer is 409
`name_reserved` ("that name is reserved; choose another"), distinct from `principal_exists`:
reserved words are public, so saying so is no oracle. The list lives in `pkg/schema`
(`ReservedNames`) so every client can refuse early with the same list.

### 4.5 Open channels

`channels.open_joining INTEGER NOT NULL DEFAULT 0`: 1 for a channel everyone who comes in from
the bare address, guest or account, is added to. Set by an operator (`PUT
/v1/channels/{channel}/open-joining`). Turning it off adds and removes nobody; turning it on
adds nobody already here. Open joining with no open channel is refused when turned on
(`no_open_channels`), so nobody can join into nothing. `#general` is not open by default.

### 4.6 Open joining settings

In `server_settings`, like `invites_paused`:

| Key | Default | Meaning |
| --- | --- | --- |
| `open_joining` | `off` | `off`, `guest`, `account`, `both`. |
| `joining_paused` | `false` | While true, the join routes answer `joining_paused`; the setting is kept. |
| `join_voice` | `false` | Whether someone from the bare address gets voice. |
| `max_guests` | `50` | Live guests at once, server-wide (guests not expired, not disabled). Max 10,000. |
| `server_name` | the host name | Shown on the front door and reserved as a name. |

Every change writes one `open_joining_changed` row with the old and new values; pause and resume
write `joining_paused` and `joining_resumed`.

### 4.7 Where an existing rule changes

| Today | After |
| --- | --- |
| An invite always creates a full member (ADR-008 §4). | It creates a member or a guest, at the joiner's choice. |
| Only operators and invites create principals. | The bare address can, when open joining is on. |
| A person cannot make themselves a credential. | A member can make a one-use sign-in link for themselves, and a credential by passkey sign-in. |
| A sign-in link is made by an operator only (`ErrSignInLinkNotAllowed`). | Also by a member, for themselves, with a shorter maximum life. |
| A principal is hidden only by an operator. | The sweeper hides expired guests 90 days after expiry, which frees their names as a hide always has. |
| An operator may grant `can_decide` to any human member. | Never to a guest; the store forces 0 for guests on every membership write. |
| No name is reserved but `hidden-*`. | A reserved list, for self-chosen names. |
| "No public signup" (ADR-008). | Open joining, off by default (ADR-009). |

## 5. Proposed wire shapes and routes

**To be designed through the `schema-change` skill.** Names and fields only. Every shape is
additive: a new type, or a new optional field on an existing V1 type, with no change of meaning to
anything published; the schema issue confirms this fixture by fixture. Every new string set is a
closed vocabulary registered in `vocabularies.go`, and every new shape is in `ts.go`'s table so
`web/src/schema/` is generated from it.

### 5.1 Additions to existing shapes

| Shape | New optional field | Notes |
| --- | --- | --- |
| `WhoAmIResponseV1` | `guest bool`, `origin PrincipalOrigin`, `passkeys int`, `can_add_device bool` | `passkeys` is how many the caller has; `can_add_device` is false for guests, agents and operators. |
| `ChannelMemberV1` | `guest bool`, `expired bool` | §4.2. |
| `PrincipalAdminV1` | `guest bool`, `origin`, `guest_expired_at *Timestamp`, `passkeys int` | |
| `InviteJoinerV1` | `guest bool` | |
| `RedeemInviteRequestV1` | `as JoinAs` (`member` default, `guest`) | §3.1. |
| `ListPrincipalsRequest` (query) | `?guest=true`, `?origin=address` | filters |
| `HidePrincipalsRequestV1` | `origin PrincipalOrigin`, `guest *bool`, `expired *bool` | bulk hide of expired guests, of everyone from the address. Still requires `dry_run`. |
| `OwnCredentialV1` | nothing: the label carries "passkey <id>" or "added device <id>" | as #496 does for operator links |
| `SignInLinkPreviewV1` | `self bool` | true for a link its own principal made (§7); omitted for an operator's |
| `ChannelV1` (operator views) | `open_joining bool` | |
| `ErrorV0.Code` | new codes below | |

New codes: `joining_closed` (404, open joining off), `joining_paused` (503), `guests_full` (503,
with `Retry-After`), `name_reserved` (409), `guest_not_allowed` (403: a guest asked for a member's
thing), `passkey_invalid` (404: the one answer of a failed passkey sign-in), `passkeys_unavailable`
(503: the server has no RP ID), `no_open_channels` (400), `sign_in_link_limit` (409: too many open
self-made links).

New vocabularies: `PrincipalOrigin` (`operator`, `invite`, `address`), `JoinAs` (`member`,
`guest`), `OpenJoining` (`off`, `guest`, `account`, `both`).

### 5.2 New shapes

| Shape | Fields |
| --- | --- |
| `JoinInfoV1` | `server_name`, `open_joining OpenJoining`, `paused bool`, `guests_full bool`, `passkeys bool`, `server_version` |
| `JoinRequestV1` | `name` |
| `JoinResponseV1` | `principal PrincipalV0`, `credential CredentialV1`, `token` (the invite redeem shape, reused as is; §13 asks whether to reuse `RedeemInviteResponseV1` by name) |
| `OpenJoiningSettingsV1` | `open_joining OpenJoining`, `paused bool`, `voice bool`, `max_guests int64`, `server_name string`; both the body of the change and the answer to reading it, like `InviteSettingsV1` |
| `ChannelOpenJoiningRequestV1` | `open_joining bool` |
| `KeepAccountResponseV1` | `principal PrincipalV0` |
| `PasskeyV1` | `id`, `principal_id`, `label`, `created_at`, `last_used_at *Timestamp`, `aaguid`, `backed_up bool`, `transports []string`, `added_by_credential_id int64`, `added_by_label string` (the sign-in that registered it, shown as "added from <label> on <date>", §6.5) |
| `PasskeyRegisterBeginResponseV1` | `session string` (opaque, server-side id), `options` (the WebAuthn `PublicKeyCredentialCreationOptions` JSON as the library emits it; carried as `json.RawMessage` and validated by the library, not hand-rolled) |
| `PasskeyRegisterFinishRequestV1` | `session`, `label`, `credential` (the `PublicKeyCredential` JSON from the browser, `json.RawMessage`) |
| `PasskeyRegisterFinishResponseV1` | `passkey PasskeyV1` |
| `PasskeyLoginBeginResponseV1` | `session`, `options` (`PublicKeyCredentialRequestOptions` JSON) |
| `PasskeyLoginFinishRequestV1` | `session`, `credential` |
| `PasskeyLoginFinishResponseV1` | `principal`, `credential`, `token` (the sign-in-link redeem shape) |
| `ListPasskeysResponseV1` | `passkeys []PasskeyV1` (empty slice, never null) |
| `RenamePasskeyRequestV1` | `label` |
| `CreateOwnSignInLinkRequestV1` | `expires_at *Timestamp` (default 10 min, max 1 h) |
| `ListOwnSignInLinksResponseV1` | `sign_in_links []SignInLinkV1` (open ones only) |
| `ReservedNames` | a `[]string` constant and `IsReservedName(name) bool` on the key, with a golden fixture of the list |

The WebAuthn option and credential objects are the W3C's JSON shapes, produced and parsed by the
library. They are not Conch's to version: the schema wraps them as opaque members, and the golden
fixture holds one real example of each so the TypeScript side is checked against it.

### 5.3 Routes

Access classes are those of `internal/server/routes.go`. Anonymous routes share the invite
limiter's buckets (§8).

| Route | Access | Does |
| --- | --- | --- |
| `GET /v1/join` | exempt | `JoinInfoV1`. Its own per-address bucket (§8). |
| `POST /v1/join/guest` | exempt | Join as a guest from the bare address. Redeem buckets, the name-taken allowance, guest cap. |
| `POST /v1/join/account` | exempt | Create an account from the bare address. Redeem buckets, the name-taken allowance. |
| `GET /v1/join/settings`, `PUT /v1/join/settings` | operator | `OpenJoiningSettingsV1`. |
| `GET /v1/join/principals` | operator | Everyone with `origin = address`, like `GET /v1/invites/{id}/principals`. |
| `POST /v1/join/disable-joiners` | operator | Disables everyone from the address, like `disable-joiners`; `dry_run`. |
| `PUT /v1/channels/{channel}/open-joining` | operator | §4.5. |
| `POST /v1/principals/{id}/keep` | operator or self | §3.6. |
| `POST /v1/passkeys/register/begin`, `.../finish` | authenticated; members only | §6.4. |
| `POST /v1/passkeys/login/begin`, `.../finish` | exempt | §3.4. Per-address buckets and the passkey routes' own server-wide bucket, never the join or redeem one (§8). |
| `GET /v1/passkeys/mine`, `PUT /v1/passkeys/mine/{id}`, `DELETE /v1/passkeys/mine/{id}` | authenticated | one's own; another's id is 404 `passkey_not_found`. |
| `GET /v1/principals/{id}/passkeys`, `DELETE /v1/passkeys/{id}` | operator | recovery: remove a lost passkey. |
| `POST /v1/sign-in-links/mine`, `GET /v1/sign-in-links/mine`, `DELETE /v1/sign-in-links/mine/{id}` | authenticated; members only | Add a device, §3.5. |
| `POST /v1/invites/redeem` | exempt | unchanged route; takes `as`. |
| `POST /v1/principals/hide` | operator | unchanged route; takes the new selectors. |

Existing routes that gain a refusal for guests: `POST /v2/channels/{channel}/files` and
`POST /v1/approvals` (`guest_not_allowed`), `POST /v1/invites` (`invite_not_allowed`, as for
anyone who may not), `POST /v1/principals/{id}/credentials` and
`POST /v1/principals/{id}/sign-in-links` for a guest target (`invalid_request`, as for an agent),
`PUT /v1/channels/{channel}/members/{principal_id}` with `can_decide: true` for a guest
(`invalid_request`, as for an agent), and `POST /v1/principals/{id}/enable` on an expired guest
(409 `invalid_request`). Every new authenticated route answers 401 under `AuthOff`, as the
invite routes do (`inviteCaller`): with authentication off there is nobody to act.

### 5.4 Parity (Rule 4)

| Browser action | REST | CLI/TUI |
| --- | --- | --- |
| Front door | `GET /v1/join` | `conch join info --server <url>` (no credential) |
| Join as a guest / create an account | `POST /v1/join/*` | `conch join guest|account --server <url> --name <name>`: stores the credential like `conch login` |
| Join from an invite as guest | `POST /v1/invites/redeem` | `conch join invite <url> [--as guest]` (today there is no CLI redemption; this adds it) |
| Keep this account | `POST /v1/principals/{id}/keep` | `conch account keep` |
| Add a device (make) | `POST /v1/sign-in-links/mine` | `conch device-link` |
| Add a device (use) | `POST /v1/sign-in-links/redeem` | `conch login --link`, the link or code read from standard input without echo as `runLogin` reads a token, never from an argument |
| Passkey register / sign in | `POST /v1/passkeys/*` | **not possible in a terminal** (no WebAuthn authenticator API); the TUI's way onto a new device is the device link. Said in `conch login --help`. |
| Passkeys list / rename / remove | `.../passkeys/mine` | `conch passkeys list|rename|remove` |
| Operator: open-joining settings, open channels, joiners, bulk disable, keep, passkeys of a principal | the operator routes above | `conch admin join status|set|pause|resume|joiners|disable-joiners`, `conch admin channel open-joining <name> on|off`, `conch admin principal keep <id>`, `conch admin principal list --guest --origin address`, `conch admin principal hide --guest --expired`, `conch admin passkey list <principal-id>`, `conch admin passkey remove <id>` |
| The guest badge | `guest` on member shapes | the TUI shows "(guest)" after the name |

## 6. Passkeys (D3)

### 6.1 The library

**`github.com/go-webauthn/webauthn` at `v0.18.2`** (released 2026-09-19; BSD-3-Clause). It is the
maintained successor of `duo-labs/webauthn`, implements registration and assertion ceremonies
over the W3C JSON shapes, and needs no HTTP framework. Measured on 2026-10-21 in a throwaway module
(`go mod init x && go get github.com/go-webauthn/webauthn@v0.18.2 && go mod tidy`), with a `main`
that imports `webauthn` and calls `webauthn.New`, so `go mod tidy` keeps only what the build
needs. The modules it adds to the build, each with its licence (read from the module's own
`LICENSE` file):

| Module | Version | Licence | Why it is there |
| --- | --- | --- | --- |
| `github.com/go-webauthn/webauthn` | v0.18.2 | BSD-3-Clause | the library |
| `github.com/go-webauthn/x` | v0.3.1 | BSD-3-Clause | its helpers (revocation lists, metadata) |
| `github.com/fxamacker/cbor/v2` | v2.9.4 | MIT | CBOR, the attestation and authenticator-data encoding |
| `github.com/x448/float16` | v0.8.4 | MIT | used by cbor |
| `github.com/go-viper/mapstructure/v2` | v2.5.0 | MIT | decoding attestation statements |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 | MIT | the Android SafetyNet attestation format |
| `github.com/google/go-tpm` | v0.9.8 | Apache-2.0 | the TPM attestation format |
| `github.com/tinylib/msgp` | v1.6.4 | MIT | used by go-tpm |
| `github.com/philhofer/fwd` | v1.2.0 | MIT | used by msgp |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | already in `go.mod` (indirect), same version |
| `golang.org/x/crypto` | v0.57.0 | BSD-3-Clause | **new**; `go.mod` has none today |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause | already in `go.mod` (indirect) at v0.47.0; bumps |

Twelve modules in the build graph, eleven of them new or bumped, one direct. Test-only modules of
the library (`testify`, `go-cmp`, `go.uber.org/mock`, `go.yaml.in/yaml/v3`, `go-tpm-tools`,
`golang.org/x/{mod,net,term,text,tools}`) appear in `go list -m all` but not in the build; `go mod
tidy` drops them from `go.mod`. The attestation formats (TPM, SafetyNet, Android key, Apple,
FIDO-U2F) are compiled in whether or not they are used; Conch asks for attestation `none`
(§6.4) and never evaluates them, so `go-tpm`, `jwt` and their kin are dead weight, not surface.

**Go version.** `v0.18.1` and `v0.18.2` declare `go 1.26.0`; `go.mod` says `go 1.25.0`, which
CI reads (`go-version-file`), and `.golangci-lint-version` (2.12.2) must be a release built with a
Go at least as new. Taking `v0.18.2` therefore bumps `go.mod` to `1.26.0` and may bump the lint
pin. The alternative is **`v0.18.0`** (2026-08-27, `go 1.25.0`), with the same twelve modules at
slightly older versions (`cbor` v2.9.3, `x` v0.3.0, `x/crypto` v0.55.0, `x/sys` v0.47.0, no
bumps). `v0.18.2`'s notes list two fixes that matter to a relying party: "validate session
challenge when finishing ceremonies" (#794) and "require authorization to initialize uv" (#795);
the rest concern attestation formats Conch does not evaluate. **This note names `v0.18.2`** and
the Go bump with it; if Nick prefers to stay on Go 1.25, `v0.18.0` with Conch's own challenge
check in `finish` (which §6.5 requires anyway) is the fallback. Either way, one line in
`deps-allowlist.txt` with Nick's sign-off, the version and the date.

### 6.2 What Conch asks of it

- **Attestation:** `none`. Conch does not care which authenticator made the key, keeps no metadata
  service and verifies no attestation chain. This also keeps every network fetch out: the
  library's MDS client is never constructed.
- **Discoverable credentials** (`residentKey: required`) so a person signs in by touching the
  passkey without typing a name: the authenticator presents the account.
- **User verification `preferred`**, so a platform passkey (Face ID, Windows Hello, a phone)
  verifies, and a plain security key still works. The stored `user_verified` flag is recorded per
  sign-in in the audit detail.
- **Algorithms:** ES256 and RS256, the library's defaults.
- **Timeouts:** 5 minutes for a ceremony; one challenge per session id, spent on first finish.

### 6.3 RP ID, origins and where passkeys cannot work

A passkey is bound to a **relying-party ID**, a registrable domain suffix of the page's origin,
and the browser only runs the ceremony in a **secure context**. So:

| Where the page runs | Passkeys |
| --- | --- |
| `https://conch.example.com` (the deployment of private-alpha.md O7: Caddy, one subdomain per instance) | **yes**, RP ID `conch.example.com`. |
| `http://localhost:8080` (development) | **yes**: `localhost` is a secure context and a valid RP ID. |
| `http://127.0.0.1:8080` or any bare IP | **no**: an IP address cannot be an RP ID (the spec requires a domain). |
| `http://192.168.1.10:8080`, a LAN name without TLS | **no**: not a secure context; `PublicKeyCredential` is absent. |
| Behind a proxy on another host name than the one configured | **no**, and the server refuses it rather than registering keys nobody can use again. |

`conchd` gets one setting, `--rp-id` / `CONCHD_RP_ID`, the host the browser reaches it at, and
`--rp-origins` (default `https://<rp-id>`, plus `http://localhost:<port>` when the RP ID is
`localhost`). An origin on a port other than 443 (or 80 for `localhost`) must name the port,
since the browser sends it in the origin. At start `conchd` refuses to serve when any origin's
host is neither the RP ID nor a subdomain of it, with a message that names the rule and the
origin: a key registered under a mismatched origin could never be used again. With no RP ID,
passkeys are off: `GET /v1/join` says `passkeys: false`, the
registration routes answer `passkeys_unavailable`, and the pages hide the buttons. The browser
hides them as well whenever `window.isSecureContext` is false or `PublicKeyCredential` is
undefined, with the sentence of §3.4 step 1 in their place. In every "no" row, **Add a device
and the operator's sign-in link are the ways in**, and the front door says so.

### 6.4 The ceremonies

**Register** (authenticated; a member, not a guest, not an agent, not an operator in v1):

1. `POST /v1/passkeys/register/begin`: the server loads the member's existing passkeys (to
   exclude them), makes a challenge, stores the library's `SessionData` under a session id of 32
   `crypto/rand` bytes in a bounded in-memory table (5-minute TTL; at most 4 per principal, 8 per
   client address, and 10,000 in all, the per-address cap checked before the global one so a
   flood from one network fills its own allowance first). The session records which principal
   began it and that it is a *register* session; `finish` refuses a session another principal
   began, and a login session is never accepted by register nor the other way round. It answers
   the options with the WebAuthn `user.id` set to the member's **user handle**: 32 random
   bytes made once per principal and stored in `principals.webauthn_user_handle`, never the integer
   id: the handle is stored inside the authenticator and comes back with every assertion, so it
   must not reveal the row id or the name.
2. The browser calls `navigator.credentials.create(options)`.
3. `POST /v1/passkeys/register/finish` with the session id, a label ("This laptop") and the
   credential JSON: the server looks up and deletes the session, lets the library verify the
   response (origin, RP ID hash, challenge, type, flags), refuses a credential id already stored
   for anyone, and inserts a `passkeys` row that records the bearer credential the request came
   with (`added_by_credential_id`, §6.5). Audits `passkey_added` with `passkey=<id>
   credential=<the registering credential's id>`. The label follows `ValidateDisplayName`, 100
   characters.

**Sign in** (anonymous):

1. `POST /v1/passkeys/login/begin`: per-address limit, then a challenge and a *login* session as
   above (same id size, same caps, bound to no principal) with no `allowCredentials`
   (discoverable).
2. `navigator.credentials.get(options)`.
3. `POST /v1/passkeys/login/finish`: session looked up and deleted (a register session is
   refused here); the server finds the `passkeys` row by the credential id in the response and
   the user handle the authenticator returned, requires both to name the same principal, lets the
   library verify the assertion, and applies the counter rule: with `new` the count in the
   assertion and `stored` the row's, refuse when `new <= stored` unless both are 0 (an
   authenticator that reports no counter); a refusal here is a cloned or replayed key: audit
   `passkey_refused` with `reason=counter` and leave the passkey for the person and an operator to
   see. Then, **inside the immediate transaction** that writes the result, it re-reads the passkey
   (not revoked) and its principal (enabled, not hidden, not a guest), so a disable that landed a
   moment ago wins; updates `sign_count` and `last_used_at`; and makes a 90-day renewable
   credential with the label "passkey <id>". Every failure is the one answer `passkey_invalid`.

### 6.5 Server-side storage

```text
passkeys
  id              INTEGER PRIMARY KEY
  principal_id    INTEGER NOT NULL REFERENCES principals (id)
  credential_id   BLOB    NOT NULL UNIQUE        -- the authenticator's id, raw bytes
  public_key      BLOB    NOT NULL               -- COSE key, as the library hands it
  attestation     TEXT    NOT NULL               -- 'none'
  aaguid          BLOB    NOT NULL               -- 16 bytes, often zero; shown to the person as a hint only
  sign_count      INTEGER NOT NULL DEFAULT 0
  transports      TEXT    NOT NULL DEFAULT ''    -- comma-joined hints: internal,hybrid,usb,...
  backup_eligible INTEGER NOT NULL
  backed_up       INTEGER NOT NULL
  label           TEXT    NOT NULL
  added_by_credential_id INTEGER NOT NULL REFERENCES credentials (id)  -- the sign-in that registered it
  created_at      INTEGER NOT NULL
  last_used_at    INTEGER
  revoked_at      INTEGER
  revoke_reason   TEXT                           -- 'removed', 'principal_disabled', 'principal_hidden'
principals
  webauthn_user_handle BLOB UNIQUE               -- 32 random bytes, made on first registration
```

Nothing secret is stored: a public key and a counter. Sessions (challenges) are in memory only;
a restart forgets them, and the person starts the ceremony again, as with the invite limiter's
buckets. Removing a passkey sets `revoked_at` (the row stays for the audit trail) and audits
`passkey_removed`; an operator removing one audits the same with the operator as actor.

**Disable and hide revoke passkeys** exactly as they revoke credentials and close sign-in links
today: `DisablePrincipal` and the hide set `revoked_at` and `revoke_reason=principal_disabled`
(or `principal_hidden`) on every live passkey in the same transaction and audit one
`passkey_removed` per passkey with that reason. Enable revives nothing, as it revives no
credential: the person registers a passkey again from a sign-in an operator gives them. "Your
sign-ins" shows each passkey as "added from <label of the registering sign-in> on <date>", so a
passkey added by a sign-in the person does not recognise stands out, and the sign-out-everywhere
dialog's offer to remove passkeys added in the last 24 hours lists them the same way (§9).

### 6.6 Recovery

Losing every passkey and every signed-in device means asking an operator for a sign-in link
(#493), as today. Nothing automatic exists, on purpose: an automatic recovery path needs a second
factor Conch does not hold (an email address, a phone), and Rule 1 forbids requiring the process
that would reach it. The front door says this in one sentence. An operator can also remove a
lost or cloned passkey by id.

## 7. Add a device

A member-made sign-in link (#493's machinery, `sign_in_links` table, same code prefix `conch_sil_`,
same preview and redeem routes, same one answer for every bad code).

| | Operator's link (#493) | Add a device |
| --- | --- | --- |
| Made by | an operator, for a member | a member, for themselves |
| Route | `POST /v1/principals/{id}/sign-in-links` | `POST /v1/sign-in-links/mine` |
| Default life | 24 h | **10 min** |
| Maximum life | 7 days | **1 h** |
| Open at once | the operator's create bucket | the member's create bucket, **and at most 3 open self-made links** |
| Uses | one | one |
| For a guest, agent or operator | refused | refused (a guest has one browser by D2; an operator keeps the by-hand credential) |
| Credential it makes | 90-day renewable, label "sign-in link <id>" | 90-day renewable, label "added device <id>" |
| Audit | `sign_in_link_created` (actor the operator) | `sign_in_link_created`, actor the member, detail `self=true expires_in=600` |
| Closing | `DELETE /v1/sign-in-links/{id}` (operator) | `DELETE /v1/sign-in-links/mine/{id}` (the member), and operators as before |
| Shown | once, in the CLI | once, in the browser, behind **Reveal** (link, Copy, QR), or by `conch device-link` |
| Preview says | the name | the name and `self: true`, so the page can say who made it |

The store's rule changes: `CreateSignInLink` admits a creator who is the target, with the
shorter bounds; `usableSignInLinkTx`, which today requires the creator to still be an operator,
accepts a creator who is an operator *or* whose id equals `principal_id` (a member who has since
been disabled closes their own links, as a disabled operator's are closed today);
`signInLinkTargetTx` is unchanged (members only). Redemption is otherwise byte-for-byte the #493
path, so the isolation check's existing exemptions for preview and redeem still hold.

**On the page that makes it (S7 of the security review):** the link and QR sit behind a
**Reveal** button under the warning sentence, so a glance at the Settings view shows nothing
usable. While this tab is sharing its screen (the voice state knows), Reveal is disabled and the
page says "Stop sharing your screen before revealing a sign-in link." Leaving the view hides it
again, and the code is never kept in page state longer than the view is open.

**On the page that uses it (S6):** the preview's `self: true` makes the page say "This link
signs you in as <name>, who made it for their own device. If that is not you, close this page."
When this browser already holds a lasting sign-in for a different principal, the page requires
the person to type that `<name>` before it signs the other out (`onSignOutFirst`) and redeems:
a link a stranger follows should not quietly replace the stranger's own session.

**The QR code** *(adopted, two steps)*: the first PR ships the link and a Copy button, which
every device can use (paste into the phone's browser, or type the short URL). The QR is drawn
client-side in the second step, from a small QR encoder: either a hand-written one in
`web/src/qr.ts` (the standard is small; byte mode, error level M, versions 1 to 10 cover the
~100-character link) or a dependency at an exact version with Nick's sign-off in
`web/deps-allowlist.txt`. The QR is an `<svg>` built from DOM nodes, no `innerHTML`, so the
hygiene test's rules hold. The QR carries the same link; nothing extra.

**The audit trail records**, in order: `sign_in_link_created` (self), then on use
`sign_in_link_redeemed` and `credential_created` (the label says "added device"), then, if the
member ends it, `credential_revoked`. "Your sign-ins" lists the new device with that label, which
is how a member learns that a link of theirs was used.

## 8. Abuse controls (D5) and the operator surface

**Rate limits.** The join and passkey-login routes use the invite limiter (`invite_limits.go`),
its token buckets and its thinned audit, with buckets of their own where sharing one would let
one kind of traffic starve another:

| Route | Per address | Server-wide |
| --- | --- | --- |
| `GET /v1/join` | its own `join_info_per_address` bucket (30 per 10 min), not the invite-preview one, so a page reloading the front door cannot spend an invite holder's previews | none |
| `POST /v1/join/guest`, `/v1/join/account` | the redeem-per-address bucket (10 per 10 min), shared with invite redemption: the same kind of act | the redeem server-wide bucket (20 per minute, successes only), shared with invite redemption |
| the same two, on a `principal_exists` answer | a smaller **name-taken allowance**, `join_name_taken_per_address` (5 per hour), spent only by a taken-name refusal, so the bare address cannot be used to probe which names exist; past it the answer is 429 as for any limit | none |
| `POST /v1/passkeys/login/begin` | the preview-per-address bucket (a begin also makes a session, capped at 8 per address) | none |
| `POST /v1/passkeys/login/finish` | the redeem-per-address bucket | **its own** `passkey_login_server_wide` bucket (20 per minute, successes only): a passkey flood never consumes the join bucket, and a join flood never locks members out of signing in |

Keys are the trusted-proxy client address (#609, `clientAddr`), with the same overflow behaviour
and the same rule that no address is ever audited or logged in full. **Behind a reverse proxy
with `--trusted-proxies` unset, every client is the proxy's address and shares one bucket**: the
first join or sign-in limit reached then stops everyone. The deployment README and the Caddy
file say to set it; `conchd` logs one warning at start when it serves `/app/` with no trusted
proxy and a non-loopback listener. Refusals audit `invite_rate_limited` with the limit's name,
as today; the design adds no second limiter.

**The guest cap.** `max_guests` (§4.6) counts guests that are live: `guest = 1`, not disabled, not
expired. `POST /v1/join/guest` and an invite redeemed `as: "guest"` check it inside the creating
transaction and answer `guests_full` (503, `Retry-After: 600`) past it; the refusal is audited
thinned as `guest_cap_refused`. The cap is not a rate: it is how many a server will hold at once.

**Pause.** `joining_paused` stops `POST /v1/join/*` (answer `joining_paused`) and leaves the
setting as it was, so resuming is one call. It does not stop passkey sign-in or invites, which
have their own switch. The operator's `conch admin join pause` is the CLI.

**Bulk revoke.** `POST /v1/join/disable-joiners` disables everyone with `origin = address`, dry
run first, operators and the caller never included, each through `DisablePrincipal` and
`principalDisabled` so sockets drop at once, with one `joiners_disabled` summary row with
`origin=address count=N`. `POST /v1/principals/hide` with `origin: "address"` or `guest: true`
(and `expired: true`) hides them, 500 at a time as #445 does. An invite's `disable-joiners` already
covers guests from that invite. **A kept account keeps `origin = address`**, so everyone who
came in from the bare address, guest or since-kept member, stays selectable by origin for bulk
disable and hide: Keep is not a way out of the net an operator can cast over a leaked door.

**Warnings in the operator surface** (UI and CLI alike): turning `join_voice` on says that
strangers will be able to join voice rooms of the open channels; marking a channel open when it
already holds messages says that everyone who joins from the address will read its history, and
asks for a confirmation (`--yes` in the CLI).

**Audit events**, all new names, all written in the same transaction as the change:

| Action | Actor | Subject | Detail |
| --- | --- | --- | --- |
| `open_joining_changed` | the operator | `settings:open_joining` | `from=off to=both voice=false max_guests=50` |
| `joining_paused`, `joining_resumed` | the operator | `settings:open_joining` | |
| `channel_open_joining_changed` | the operator | `channel:<id>` | `open_joining=true` |
| `principal_created` (existing) | `join:address` for a bare-address join (as `invite:<id>` is the actor of an invite's), `invite:<id>` otherwise | `principal:<id>` | gains `origin=address guest=true` |
| `guest_kept` | the guest, or an operator | `principal:<id>` | `origin=address|invite` |
| `guest_expired` | `system` | `principal:<id>` | `last_credential_expired_at=...` |
| `principal_hidden` (existing) | `system` | `principal:<id>` | gains `reason=guest_expired` when the sweeper hides |
| `guest_cap_refused` | `anonymous` | the route pattern | `max=50 count=N` (thinned) |
| `join_refused` | `anonymous`; `principal:<id>` for `reason=keep_closed`, which comes from an authenticated guest | the route pattern | `reason=closed|paused|name_reserved|name_taken|keep_closed` (thinned) |
| `joiners_disabled` (existing) | the operator | `origin:address` | `count=N` |
| `passkey_added` | the member | `principal:<id>` | `passkey=<id> credential=<registering credential id>` |
| `passkey_removed` | the member, an operator, or `system` on disable and hide | `principal:<id>` | `passkey=<id> reason=removed|principal_disabled|principal_hidden` |
| `passkey_renamed` | the member or an operator | `principal:<id>` | `passkey=<id>` |
| `passkey_signed_in` | `passkey:<id>` | `principal:<id>` | `credential=<id> user_verified=true` |
| `passkey_refused` | `anonymous` | the route pattern | `reason=invalid|counter|session` (thinned) |
| `sign_in_link_created` (existing) | the member | `principal:<id>` | gains `self=true` |

No event ever carries a code, a token, a challenge, a public key or a client address.

**The operator surface**, in the browser's operator views when they exist and in the CLI now:
the open-joining settings; the open flag on each channel; the list of people from the address
and of guests (with origin, expiry, passkey count); keep; bulk disable and hide; a member's
passkeys, with remove; and the existing invite administration unchanged.

## 9. Threat notes

Each note says the threat, what in this design answers it, and what remains. They are written
for the `security-reviewer` pass #694 asks for before the issues are filed.

**Account takeover.**
- *By passkey:* the server verifies origin, RP ID hash and challenge through the library, binds a
  session id to one challenge spent on first use, requires the credential id and the user handle
  to name the same principal, and checks the sign counter. A stolen public key is useless. What
  remains: a passkey synced to an attacker's cloud account is that account's problem, not
  Conch's; a cloned authenticator is caught by the counter on its next use and the person sees a
  sign-in they did not make in "Your sign-ins".
- *By device link:* 10 minutes, one use, shown only on the signed-in screen, the code in the URL
  fragment so no server or proxy log sees it (#440's rule), the same single-answer redeem. What
  remains: a person who shows the QR on a shared screen; the page says not to.
- *By operator link:* unchanged from #493.
- *By the bearer in the browser:* see "credential theft" below.
- *By a stolen bearer registering a passkey (S8):* a bearer taken from a browser can call
  `register`, which turns a theft that a sign-out would end into a lasting foothold.
  Mitigations: every passkey records the sign-in that added it and "Your sign-ins" shows "added
  from <label> on <date>", so a passkey added from a sign-in the person does not recognise is
  visible; `passkey_added` carries `credential=<id>`; and **sign out everywhere** (`revoke-all`)
  offers, in the same dialog, to remove every passkey added in the last 24 hours, listing each
  one it would remove by label and "added from <label> on <date>" so the person removes what they
  do not recognise and keeps what they do. What remains:
  a person who never looks at the list. A re-authentication step (touch a passkey to add a
  passkey) would close it and is not built now; it is a candidate for a later issue.

**Guest-to-member escalation.**
- A guest cannot obtain a second credential by any route (`guest_not_allowed` on credentials,
  passkeys, device links, operator links), cannot invite, cannot be granted the right, cannot
  request an approval (`guest_not_allowed` on `POST /v1/approvals`), and **never holds
  `can_decide`** (M1): the store forces 0 on every membership write for a guest, including one an
  operator makes by hand, so a guest an operator adds to a channel with approvals cannot decide
  them (`decision_not_permitted`). Keep is the one escalation, it is audited, it changes nothing
  but the mark and the renewal, and it grants no channel, no voice, no invite right and no
  deciding right. An operator adding a guest to a channel is an operator's audited act. The
  isolation check (§11) asserts each refusal.
- *Keep as a door around a closed door (M2):* if an operator turns open joining off (or to
  `guest`), guests already in could otherwise still become lasting accounts. So a bare-address
  guest's self-Keep requires that open joining offers accounts and is not paused at that
  moment; otherwise `joining_closed` / `joining_paused`, audited thinned as `join_refused
  reason=keep_closed`. An operator's Keep bypasses it, and a guest from an invite keeps freely:
  someone chose to invite them.
- What remains: Keep means a guest can become a lasting member without any operator seeing it
  happen except in the audit log and the principal list (`origin` tells where they came from,
  and stays `address` after Keep so bulk disable and hide still reach them). That is D2's
  intent; the list and `conch admin principal list --origin address` make it visible.

**Name enumeration (S2).**
- `POST /v1/join/*` answers `principal_exists` for a taken name, as invite redemption does, so the
  bare address could be used to learn which names exist. Mitigations: a separate, smaller
  per-address allowance for that answer (5 per hour, §8), audited thinned as `join_refused
  reason=name_taken`; the reserved list is public, so `name_reserved` reveals nothing. What
  remains: five probes an hour per address; the operator sees them in the trail.

**Name squatting and look-alike names.**
- Case folding and UTS #39 skeletons already refuse look-alikes (#441). The reserved list refuses
  names that impersonate the system or staff.
- A name a guest held stays locked until the guest is hidden: at once if the guest posted
  nothing, 90 days after expiry if it posted (§4.3, R1). Expired guests do not count toward
  `max_guests`, so a party can lock more names than the cap by joining, posting one message and
  letting each guest expire: 10 names per address per 10 minutes, 20 a minute server-wide, each
  locked for 97 days. The per-address limits and the server-wide limit are what bound the rate;
  **bulk hide of expired guests** (`POST /v1/principals/hide` with `guest: true, expired: true`,
  or `conch admin principal hide --guest --expired`) is the operator's lever, and frees every
  locked name in one call, 500 at a time. Open joining off, or paused, stops the flow.
- What remains: an operator who never looks. The principal list with `--guest --expired` and
  the `guest_expired` rows in the trail are where it shows.

**Mass guest creation.**
- Per-address limits (10 joins per 10 minutes, a /64 for IPv6), the server-wide 20 per minute, the
  guest cap, the pause switch, bulk disable and hide by origin, and the 7-day expiry with a 90-day
  hide. Memory is bounded as #437 bounded it. A flood shows in the audit trail as thinned
  `invite_rate_limited` and `guest_cap_refused` rows.
- What remains: with open joining on, anyone on the internet can fill the guest cap. That is why
  it is off by default and why the cap is a server-wide count, not per address.

**Credential theft from localStorage.**
- Unchanged in kind from O1 (#439): the CSP allows `script-src 'self'` only, the client ships no
  third-party script, and the hygiene test holds credential handling to `api.ts`. A guest's
  credential is the same bearer kept the same way, with a 7-day instead of 90-day renewal, so its
  exposure is shorter. Passkeys reduce the cost of theft: a member can end the stolen sign-in in
  "Your sign-ins" and sign in again without an operator. The WebAuthn ceremony itself never
  exposes a secret to the page.
- What remains: an XSS in the client would still read the bearer; the mitigations are the CSP and
  the hygiene test, as before. An HttpOnly cookie was weighed and declined in O1.

**Phishing of Add-a-device links.**
- A link signs its opener in as the member, so a member tricked into sending one away has handed
  over a sign-in. Mitigations: the life is 10 minutes; it is one use, so a used link is at once
  visible as a sign-in the member did not make ("added device <id>" in "Your sign-ins", which
  they can end); the page makes it for the member's own screen, keeps it behind Reveal, refuses
  to reveal it while the tab shares its screen (S7), and says so; a member can hold at most 3
  open; an operator can close any. The preview shows the account's name and `self: true`, and
  the page that uses it says plainly whose device link this is and to close the page if that is
  not you (S6).
- *The other direction, a link planted on a stranger (S6):* a member's link sent to someone else
  signs that someone in as the member, which, in a browser that already holds the stranger's own
  session, would quietly replace it. So when a lasting credential for a different principal is
  stored, the page requires the person to type the link's name before signing the other out.
- What remains: social engineering within the 10 minutes. The sentences on both pages and the
  "Your sign-ins" listing are the answer; a second factor is out of scope.

**Passkey sessions as a resource and as a confusion (S10).**
- Anonymous `login/begin` creates server state. The session table is bounded (8 per client
  address, checked first; 4 register sessions per principal; 10,000 in all; 5-minute TTL;
  evicted oldest-first), ids are 32 random bytes, and `begin` spends a preview token per address.
- A register session is bound to the principal that began it and is refused from any other; a
  login session is bound to none; neither kind is ever accepted by the other ceremony's `finish`.

**The cross-group isolation boundary (D6).** See §11.

**Operator settings as surface.** Every switch is operator-only in the route table, stored in
`server_settings`, audited with old and new values, and read fail-closed (an unreadable
`open_joining` means off, as `invites_paused` reads as paused).

## 10. Rule 1

Nothing here needs a process besides `conchd`: joining, guests, the expiry sweeper, passkeys
(the library verifies in-process; no metadata service, no network), device links, recovery. The
passkey ceremonies need HTTPS or `localhost`, which is a property of the deployment, not a
process; without it the design still lets everyone in by link. LiveKit stays optional and
unrelated. No mail server exists or is assumed.

## 11. The isolation check (D6)

`e2e/isolation` grows:

- **A third group, C, the guests.** Open joining on (`both`), one open channel; a guest `cora` and
  a member `carl` from the bare address; a guest `gabe` from an A invite link redeemed `as:
  "guest"`. Group B is unchanged and remains the prober; C's guests are probed too, from inside
  their own narrow access.
- **Matrix rows** for every new route (coverage fails otherwise): `GET /v1/join` (anonymous, says
  nothing of any group: an exemption with the reason), `POST /v1/join/*` while on (makes C
  principals and nothing else), the settings and open-joining routes (operator-only refusals),
  `keep` on an A guest (404 as for anyone else's principal), the passkey `mine` routes (answer
  only the caller's), `sign-in-links/mine` (the same). `POST /v1/passkeys/login/begin` and
  `finish` are exempt with this reason: "takes an assertion only a registered authenticator can
  make; B holds no passkey of A's, and every failure is the one `passkey_invalid`; the ceremony
  itself, including a B assertion offered for an A passkey, is covered by G4's
  software-authenticator tests in `internal/server/passkeys_test.go`".
- **A guest never decides (M1).** The operator adds `gabe` to a B channel that has a pending
  approval (an operator's act, audited); `gabe` reads it marked read-only and his decision answers
  `decision_not_permitted`; `PUT .../members/{gabe}` with `can_decide: true` answers
  `invalid_request`.
- **Keep follows the door (M2).** With open joining set to `guest`, `cora`'s self-Keep answers
  `joining_closed`; while paused, `joining_paused`; the operator's Keep of `cora` succeeds; `gabe`
  (invite guest) keeps freely. After Keep, `cora` still cannot invite (`invite_not_allowed`) or
  decide (`decision_not_permitted`), and `GET /v1/join/principals` still lists her by origin.
- **No approvals from guests (S5).** `cora`'s `POST /v1/approvals` answers `guest_not_allowed`.
- **A guest probes as a guest.** `gabe` (A's invite, guest) and `cora` (address, guest) each run the
  matrix against B's objects, and additionally against A's channels `gabe` was not granted: every
  answer the documented refusal, no marker of B or of A's other channel. Then `cora` tries every
  guest-refused route on its own objects: files upload, invite create, decide, device link,
  passkey register, credentials; each is `guest_not_allowed` or the route's own refusal, and
  audited.
- **The bare address while off.** Before C is built, with `open_joining = off`: `GET /v1/join`
  says `off`, `POST /v1/join/guest` and `/account` answer `joining_closed`, and the principal count
  is unchanged. Repeated while paused (`joining_paused`).
- **Revocation, timed as A14 timed it:** `cora` holding every socket variant and a LiveKit
  participant (C gets voice in the check) is disabled by `POST /v1/join/disable-joiners`; then
  `carl`'s kept session; then `gabe` by A's invite's `disable-joiners`. Each within the existing
  bounds, and each stays out. Then **expiry on the real path (S9)**: a guest is joined with a
  short credential lifetime (a `conchd` test option that lowers the guest lifetime below
  `MinCredentialRenewLifetimeSeconds` for the run, as `TestRelaxInviteLimits` relaxes the
  limiter), holding every socket variant and a LiveKit participant; the check waits for the
  expiry with no driven clock and measures that the sockets close and LiveKit stops listing the
  participant within the same bound as a revocation (one poller pass plus a room rotation), that
  the credential answers 401, that `guest_expired` is written within seconds, and that the
  guest's messages still list in the channel for `carl` with the badge.
- **The name stays until the hide (S3):** after `cora` expires, `POST /v1/join/account` with the
  name `cora` answers `principal_exists`; after the operator hides her (or the sweeper would,
  90 days on), it succeeds, and the hidden row shows as `hidden-<id>` in the operator's list.

## 12. The work, in order

Each line becomes one issue in the Private alpha milestone, filed after Nick signs ADR-009. The
drafts are in the scratchpad (`p694/issues.md`) for the principal to file.

| # | Work | Area | Size | Blocked by |
| --- | --- | --- | --- | --- |
| G1 | **Schema:** every shape, vocabulary, code and constant of §5 and §6, with golden fixtures and `ts.go` entries; `ReservedNames`; the WebAuthn JSON wrapped as opaque members with a real example fixture. protocol-designer. | schema | M | ADR-009 signed |
| G2 | **Server: guests and names.** Columns of §4.1, `origin` migration, the guest mark on every shape, guest refusals (files, approvals, `can_decide` forced 0), the reserved list, `keep` with its door rule, the expiry sweeper and 90-day hide, the live-credential voice entitlement, open channels flag. security-reviewer pass. | server | L | G1 |
| G3 | **Server: open joining.** Settings, `GET /v1/join`, join routes, rate limits, guest cap, pause, joiners list, bulk disable and hide selectors, audit events; `as: guest` on invite redemption. security-reviewer pass. | server | M | G2 |
| G4 | **Server: passkeys.** The dependency at the signed-off version (and the Go bump if `v0.18.2`), `passkeys` table, user handle, session table, ceremonies, mine and operator routes, RP ID config, audit. security-reviewer pass. | server | L | G1, Nick's dependency sign-off |
| G5 | **Server: Add a device.** Self-made sign-in links with the §7 bounds, `mine` routes, labels, audit. security-reviewer pass. | server | S | G1 |
| G6 | **Web: the front door and guests.** The bare-address page, join as guest, create an account, the invite page's guest choice, the badge, the guest line and Keep, the sign-out warning, expired guests in the roster. | web | M | G2, G3 |
| G7 | **Web: passkeys and Add a device.** Register after join and in Settings, sign in with a passkey, manage passkeys, the Add a device view with link and Copy, `conch login --link` documented on the page; the capability checks of §6.3. The QR encoder is a follow-up issue (G7b, S) with its own dependency decision. | web | M | G4, G5, G6 |
| G8 | **CLI and TUI parity.** Every row of §5.4: `conch join`, `conch login --link`, `conch device-link`, `conch passkeys`, `conch account keep`, the `conch admin join`, `channel open-joining`, `principal keep`, `passkey` groups; the "(guest)" suffix in the TUI. | cli | M | G2, G3, G4, G5 |
| G9 | **Isolation e2e (D6).** §11: group C, the rows, the guest-as-prober runs, the off and paused checks, timed revocation, expiry and name release. | e2e | M | G3, G5 (passkey login rows need G4; exempt them with a reason until G4 merges, then a follow-up row) |
| G10 | **Web e2e.** One Playwright spec per flow of §3: invite as guest, bare address off and on, passkey register and sign in (Chromium's virtual authenticator over CDP), Add a device across two contexts, Keep, operator recovery unchanged, the `(guest)` badge. | web | M | G6, G7 |

G1 first; G2, G4 and G5 can run in parallel once G1 merges; G3 after G2; G6 after G3; G7 after
G4, G5 and G6; G8 after the servers; G9 and G10 last. Every server PR is `security` and gets the
reviewer; G1 is `area/schema`; none touches the approval path.

## 13. Open points

Things this note decided under the standing delegation, or could not decide; each with the
recommendation it took. None reopens D1–D6.

1. **Go 1.26 for `go-webauthn/webauthn` v0.18.2.** Taken, with v0.18.0 on Go 1.25 as the fallback
   (§6.1). The lint pin may move with it. Nick's call with the dependency sign-off.
2. **Expired guests and their names: resolved by the security review (S3, §4.3).** D2 wants
   "<name> (guest)" on old messages; D4 wants names reusable. The first draft released the name
   at expiry with a `shown_name` column and an `expired-` prefix; the review chose the simpler
   rule, and it is adopted: the row keeps its name until the sweeper hides it 90 days after
   expiry, and the hide frees the name as it always has. The cost is that a guest's name is
   unavailable for up to 90 days after the guest last used Conch; an operator can hide earlier.
3. **Guests cannot upload files.** Adopted to bound storage abuse; one line to flip if Nick wants
   guests to share files.
4. **Passkeys for members only in v1.** Operators keep the one-tab, by-hand credential (D2 of
   #260) and the sign-in-link store already refuses operators as targets (#493). Giving operators
   passkeys later is additive.
5. **The QR code is a second step** (§7): the link and Copy ship first; the encoder is a
   hand-written `qr.ts` or a signed-off dependency.
6. **`JoinResponseV1` versus reusing `RedeemInviteResponseV1`.** The fields are identical; the
   schema issue decides whether a join answers with the invite's shape by name or with its own.
7. **Done.** `ROADMAP.md` (the alpha paragraph) and `private-alpha.md` §3 and O2 now carry a
   one-line pointer to ADR-009, edited by the principal on 2026-10-21 in the same PR.
8. **The 90-day hide of expired guests** is a default, not a setting; it keeps the principal list
   from filling with dead guests. A setting can follow if an operator asks.
9. **Guest channels and later changes:** a channel made open after a guest joined does not add
   existing guests; an operator adds them by hand. Simple and audited; automatic membership can
   come later if it is missed.
10. **Re-authentication before adding a passkey** (touch an existing passkey to add another) would
    close the residual of §9's "stolen bearer registers a passkey"; not built now, a candidate for
    a later issue.

## 14. Security review

Reviewed by the `security-reviewer` agent on 2026-10-21, on the first draft of this note; the
principal decided every finding, and all are adopted and folded into the sections above:

- **M1** (§4.1, §4.2, §9, §11): a guest never holds `can_decide`; the store forces 0 on every
  membership write for a guest and refuses `can_decide: true` on one.
- **M2** (§3.6, §8, §9, §11): a bare-address guest's self-Keep requires open joining to offer
  accounts and not be paused; operator Keep bypasses; invite guests keep freely; kept accounts
  stay selectable by origin.
- **S1** (§5.3, §8): `GET /v1/join` and passkey sign-in have buckets of their own; passkey sign-in
  never spends the join or redeem server-wide bucket.
- **S2** (§8, §9): a smaller per-address allowance for `principal_exists` on the join routes,
  audited `join_refused reason=name_taken`; the name-enumeration threat note.
- **S3** (§4.3, §4.4, §4.7, §13.2): an expired guest's name is released only by the hide, 90 days
  on; `shown_name`, the `expired-` prefix and the `claimNameTx` exception are dropped.
- **S4** (§3.4, §6.4, §6.5, §8): disable and hide revoke passkeys with a reason; enable revives
  nothing; `login/finish` re-reads the principal inside the immediate transaction.
- **S5** (§4.2, §5.3, §11): `POST /v1/approvals` is `guest_not_allowed` for a guest.
- **S6** (§3.5, §5.1, §7, §9): the preview of a self-made link carries `self: true`; the page says
  whose device link it is; typing the name before signing another person out.
- **S7** (§3.5, §7, §9): the link and QR sit behind Reveal and are not revealed while the tab
  shares its screen.
- **S8** (§5.2, §6.4, §6.5, §8, §9): a passkey records the credential that added it, shown as
  "added from <label> on <date>"; `passkey_added` carries it; sign out everywhere offers to
  remove passkeys added in the last 24 hours; the residual is stated.
- **S9** (§4.1, §4.3, §11): the voice entitlement requires a live credential for a guest, so
  LiveKit cuts an expired guest within one poller pass; the sweeper runs at the earliest live
  guest credential's expiry; the isolation check measures expiry on the real path.
- **S10** (§6.4, §9): register sessions are bound to their principal, never interchangeable with
  login sessions, ids of 32 random bytes, a per-address session cap before the global one.
- **Nits**, all adopted: `usableSignInLinkTx` accepts the self-made case (§7); guest lifetime is
  `min(invite lifetime, 7 days)` and Keep on an invite guest uses the invite's lifetime (§3.6,
  §4.1); `--rp-origins` names non-443 ports and `conchd` refuses an origin whose host is not the
  RP ID or a subdomain (§6.3); the exact counter rule (§6.4); enable on an expired guest is
  refused (§4.3, §5.3); the audit actor of a bare-address join is `join:address`, `guest_kept`
  carries `origin`, `join_refused` gains `name_taken` and `keep_closed` (§8); every new
  authenticated route answers 401 under `AuthOff` (§5.3); warnings for `join_voice` and for
  opening a channel with history (§8); D2's "one browser" is "exactly one credential" (§2,
  §4.1); the passkey login exemption in the isolation check points at G4's software-authenticator
  tests, and rows check that a kept account still cannot invite or decide (§11); `conch login
  --link` reads from standard input without echo (§3.5, §5.4); one line on `--trusted-proxies`
  unset behind a proxy (§8).

Re-checked on 2026-10-21 after the above: PASS, with one residual and four nits, all adopted:

- **R1** (§4.3, §9): an expired guest that posted no message is hidden on the same sweeper pass
  and its name freed at once; one that posted is hidden 90 days on; the name-squatting note is
  rewritten for the post-S3 rule and names bulk hide of expired guests as the operator's lever.
- **N1** (G2 files): the approval refusal lives in `internal/server/approvals_handlers.go`.
- **N2** (G4 files): the S4 hook goes into `disablePrincipalTx` and `HidePrincipal`
  (`internal/server/store/disable.go`, `hide.go`), which G2 also edits.
- **N3** (§4.3, G2, G9): the socket bound at expiry is the sweeper's wake plus 2 s, with the
  sockets' 30 s `CredentialLive` recheck in `internal/server/ws.go` as the fallback.
- **N4** (§8): `join_refused reason=keep_closed` is audited with actor `principal:<id>`.
- **N5** (§6.5, §9, G7): the "remove passkeys added in the last 24 hours" checkbox lists the
  passkeys it would remove, by label and "added from".
