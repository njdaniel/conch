# Design: guests under their own name, with a Guest marker

- **Status:** Proposed. Design only; nothing here is implemented, and no code, dependency or migration lands with this note. Nick decides the open points of §8; the ADR-009 amendment of §9 is his to make.
- **Issue:** [#1003](https://github.com/njdaniel/conch/issues/1003) (owner decision 2026-10-30). Parked behind [#1004](https://github.com/njdaniel/conch/issues/1004) (guests return on the community model); this note settles the contract so that #1004's slices and #1003's can be filed cold.
- **Pinned to:** `main` at `d7afd8c797d0d95fd2ec0836c6ff2ac3df254539` (2026-11-04). Every code claim in §1 was checked at that commit; line numbers drift, so search for the named function.
- **Governs:** [ADR-009](../adr/ADR-009-guests-and-open-joining.md) D2 and item 3 (the rendering and uniqueness contract of a guest's name), [guests-and-accounts.md](guests-and-accounts.md) §1, §3.3, §4.1, §4.4 and §7.1; the "(guests later)" rows of [community-model.md](community-model.md) §2.2 and §14.
- **Owner:** protocol-designer (this note, the shapes); server-engineer (the store and routes); web-engineer and cli-engineer (the marker); Nick (the ADR amendment, the decisions, the approval-path merges).

Today a guest is named `guest-<16 random characters>`, and if it typed a label it is shown everywhere as `Guest: Alice · a7k4`. Nick's decision of 2026-10-30: a guest chooses an ordinary name, is shown as **Alice** with a separate **Guest** marker, is asked for another name when Alice is taken, and keeps Alice when it creates an account. The random handle stays, but inside identity management. This note says how that works without a second naming system: the guest's chosen name becomes its principal name, claimed by the one clash check every principal name already goes through, and given back when the visit ends.

## 1. Current state at `d7afd8c7`

### 1.1 Merged behaviour

**Guests are off.** `internal/server/join.go` has `const guestsRemoved = true` (issue #1005, PR #1009): `handleJoinGuest` and `handleRedeemInviteGuest` answer 403 `guests_disabled` before reading a body; `PUT /v1/join/settings` refuses `guests_enabled: true` and the `guest` and `both` modes with 409 `join_policy_conflict` (`internal/server/store/guest_policy.go`); `store.Open` normalises stored settings to guests off. Migration 40 (`internal/server/store/community.go`, `endActiveVisitsTx`) ended every active visit through `endGuestVisitTx` with reason `operator`, actor `migration`, so a server on `main` has no active visit: every `guest_visits` row is `ended` or `converted`. Under ADR-013 the bare address never admits a guest again; when guests return (#1004) they come through operator-made guest links and `POST /v1/invites/redeem-guest`. `POST /v1/join/guest` stays refused for ever (community-model.md §0.1, §14 item 11).

**The guest's name is its handle.** `createGuestTx` (`internal/server/store/guests.go`) draws a handle with `newGuestHandleTx` (`guest-` + 16 characters of Crockford base32, 80 bits; `schema.GuestHandleLength`), checks it against `guest_visits.handle` (UNIQUE, never deleted) and against every principal through `clashTx`, and inserts the principal with `name = handle` and the handle's keys in `name_key` and `name_skel`. The optional label goes to `guest_visits.label` after `guestLabel` normalises it to NFC and checks it (`schema.ValidateGuestLabel`: 1 to 32 characters, no `·`, no whitespace run, not in the `guest` or `hidden-` namespaces, not the server's name). Labels are not unique and are not checked against principal names.

**Rendering is prescribed.** `schema.GuestDisplayName(label, handle)` (`pkg/schema/guest.go`) is `"Guest: " + label + " · " + GuestTag(handle)` with a label and the bare handle without; `GuestAttributionV1{handle, label, tag}` carries it on `ChannelMemberV1.guest`, `MessageV2.guest_author`, `ReactionV1.guest_reactor`, `GuestV1` and `GuestAdminV1`. The three clients render from those fields and never from a name's text: `web/src/guestName.ts` (`guestName`, `memberName`, `displayName`, used by `Conversation.tsx` `authorOf`, `Members.tsx`, `useLiveChannel.ts`, `voice/useVoiceNames.ts`, `voice/useOccupantNames.ts`, `Guest.tsx`); `internal/cli/tui/model.go` (`authorName`, `messageAuthor`, the status line at `m.userName`), `internal/cli/tui/help.go` ("Guests show as \"Guest: <label> · <tag>\""), `internal/cli/command.go` (`whoami` prints `guest: Guest: Pat · a7k4, a temporary visit from invite`). The desktop client is the same React app under `web/src/platform/desktop/`, so it renders what the browser renders. `conch-voice` reads only its own `WhoAmIResponseV1.name` (`voice/crates/conch-voice-api/src/types.rs`) and shows no one else, so it has no guest rendering to change.

**One clash check for every principal name.** `claimNameTx` (`internal/server/store/rename.go`) computes `names.KeysOf(name)` (`internal/server/names/key.go`: NFC, Unicode full case fold, UTS #39 confusable skeleton, default-ignorables dropped, whitespace compacted; two keys, folded and skeletal, each compared only with its own kind), refuses an empty key (`ErrNameInvalid`), the hidden namespace (`reservedName`), the guest namespace (`reservedGuestName`: either key begins with the keys of `guest`) and the server's name (`isServerName`), each as `ErrNameReserved` wrapping `ErrDuplicate`, then `clashTx` against `principals` (`name_key = ? OR name_skel = ?`, excluding `self`). Every create and rename goes through it inside `withImmediateTx`: operator create, open sign-up, invite redemption, `renamePrincipal`, `ConvertGuest` (`claimNameTx(c.Name, c.PrincipalID)`), and `newGuestHandleTx` for the handle. `CheckNameFree` is the same rule read-only, which the password routes call before hashing (`nameIsFree`, `join.go`, behind the name-probe allowance of five `principal_exists` answers an hour per address). A unique-constraint error on `setNameTx` is mapped to `ErrDuplicate` for keys computed under other Unicode tables.

**Renames and hides.** `renamePrincipal` refuses a principal with a live or ended visit (`p.guestTarget()`, `ErrGuestNotHideable`, review F10), whoever asks; `HidePrincipal` refuses a guest the same way and renames a member to `hidden-<id>` (`hiddenNameTx`). `ConvertGuest` requires a name, claims it with `self` the guest's own id, sets it with `setNameTx` (audit `principal_renamed from=<handle> to=<name>`), marks the visit converted, revokes the guest credential, issues a member credential, stores the password, and audits `guest_converted from=<handle> to=<name>`. It writes no membership.

**Attribution is immutable.** Migration 31 (`internal/server/store/store.go`) creates `message_guest_attribution (message_id, handle, label)`, filled by the `AFTER INSERT ON messages` trigger from the author's active visit and protected by no-update and no-delete triggers; `messageColumns` (`store/audience.go`) reads it by subquery into `MessageV2.guest_author`. `guest_visits_terminal` freezes `handle`, `label`, `origin`, `invite_id`, `started_at`, `expires_at` and `idle_ms` for the visit's life and refuses any change out of a terminal state.

**Ending a visit** (`endGuestVisitTx`) marks the row ended, audits `guest_ended reason=…`, removes every membership, deletes read marks and revokes the credential. It does not touch `principals.name`: an ended guest keeps its handle as its name, which is harmless today because a handle occupies nothing anyone wants.

**Existing data.** Visits made before #1005 have `label` NULL or a free-text, possibly duplicated, label; their posts carry `{handle, label}`. `reservedGuestNamesTx` lists permanent principals whose keys begin with `guest` (migration 31's preflight; `JoinWarningReservedNamesInUse` blocks `guests_enabled` while any exists).

**The marker precedent.** An agent is marked by a separate element that user text cannot produce: `<span className="ag-badge">Agent</span>` (`Members.tsx`), `[agent]` after the name in the TUI (`internal/cli/tui/agents.go` `agentBadge`; `safeName` escapes `[` in names and `messageLine` documents the three guarantees: position, one line, escape).

### 1.2 Planned, not merged

- **#1004** restores guests on guest links: `InviteKindGuest` back in `InviteKindV2` (additive), `POST /v1/invites/redeem-guest` for a guest link with the in-transaction visibility re-check, O7 conversion (bounds cleared, voice and the brake on, the private-channel assertion), the directories' refusals, the web and CLI guest UI rebuilt on guest links, the isolation phases. Not ready for dispatch until Nick decides guests return. This note changes nothing in that list and adds the name contract on top of it.
- **ADR-013 amendment of 2026-10-30** is merged (PR #1006); the design rows it marked "(guests later)" are the ones #1004 restores.
- **Nothing on `main` implements the 2026-10-30 naming decision.** `Guest.tsx` and the help text still describe `Guest: <label> · <tag>`, and `CreateAccount.tsx` still asks a converting guest for a new username.

## 2. The proposed design

### 2.1 Principle: one name, one claim, released at the end

A guest's chosen name **is its principal name** (`principals.name`), claimed in the admitting transaction by `claimNameTx` with `self = 0`, exactly as open sign-up and invite redemption claim a member's. The random handle is still drawn, still unique for ever in `guest_visits.handle`, still checked against `principals` by `newGuestHandleTx`, and is still the key of the visit's attribution; it is no longer written to `principals.name` while the visit is live. When the visit ends, the ending transaction renames the principal **to its handle**, which gives the chosen name back.

Why this and not a parallel "display name" beside the handle: every consumer of a principal's name (whoami, channel members, the authors route, audit details, the TUI's fallbacks, the web's `Person` map, mentions, voice rosters, operator lists) would need a guest branch, and uniqueness between labels and member names would need a second index and a second claim path. With the name in `principals.name`, uniqueness, case folding, confusables, the reserved namespaces, the server-name reservation, the atomicity of concurrent claims and the existence-oracle limits are all the existing code, and the Guest marker is the only client change.

### 2.2 The name-claim and uniqueness contract

| Rule | Where it already lives | Change |
| --- | --- | --- |
| Shape of a guest name | `validatePermanentName` (`pkg/schema/guest.go`): `CreatePrincipalRequest.Validate` (`ValidateDisplayName`, 100 characters, no control, bidi or invisible characters, no leading or trailing space) and not `IsReservedGuestName` | A guest name is validated as a permanent name, since conversion keeps it. `ValidateGuestLabel`'s 32-character, no-`·`, no-whitespace-run rules are not applied to a name: they protected the `label · tag` rendering, which goes. |
| Normalisation | `guestLabel` does NFC; `names.KeysOf` does NFC, fold, skeleton | The server normalises the name to NFC before validation and storage, as `guestLabel` does today and as passwords are. |
| Uniqueness | `claimNameTx` + `clashTx` over `principals.name_key`/`name_skel` | None. A guest name clashes with every member, operator, agent and **live guest**, by either key. A member's rename or creation clashes with a live guest's name the same way, with no new code. |
| Reserved names | `reservedName` (`hidden-`), `reservedGuestName` (`guest`), `isServerName` | None. A guest cannot be named `Guest`, `guest-…`, a look-alike of either, `hidden-…`, or the server's name. The `guest` namespace stays reserved for permanent names too, so no member can be named to resemble the marker (O2). |
| Ended guests | Handles only in `principals.name` | **New.** `endGuestVisitTx` sets `principals.name` back to the visit's handle through `setNameTx` (audit `principal_renamed from=<name> to=<handle>`), so an ended visit holds no ordinary name and the name is free for the next person. The handle's keys were checked free at admission and are in a namespace no later write may enter, so this rename cannot clash. |
| Converted guests | `ConvertGuest` claims with `self = pid` | The name the guest already holds is its own: the claim passes by construction, and no rename is written when the name is unchanged (`principal_renamed` is not audited; `guest_converted from=<handle> to=<name>` still is). |
| Live guest rename | `renamePrincipal` refuses `guestTarget()` | None: a guest's name is fixed for the visit, by it and by operators. A guest who wants another name ends the visit and joins again (O3). |
| Concurrency | `withImmediateTx` on every path that writes a name | None. SQLite's single writer serialises guest admission, member creation, rename and conversion; each claims inside its transaction. Two visitors asking for `Alice` at once: one gets it, the other 409. |
| Existence oracle | `nameIsFree` + `peekNameProbe` (five `principal_exists` an hour per address) | Guest admission answers a taken name with the same 409 `principal_exists` as `POST /v1/join/account` and the v2 redemption do, through the same per-address probe allowance, and spends a probe on each refusal. It says "taken or reserved", never which, and never whether the holder is a member, a guest or hidden (`hide_members`, community-model.md §13, is not widened: this is the bounded oracle ADR-013's consequences already record). |

A missing name is refused with 400 `invalid_request`, message "choose a name", before any code is checked or any use spent. There is no fallback to a random handle for a new visit, no suffix and no silent rename, ever.

### 2.3 Admission, step by step (`POST /v1/invites/redeem-guest`, once #1004 reopens it)

1. `guestsRemoved` lifted by #1004; HTTPS or loopback; per-address and global invite limits, as today.
2. Decode `RedeemInviteGuestRequestV1`; a malformed code is 404 `invite_invalid` (unchanged); an absent `name` is 400; a name that fails `validatePermanentName` is 400 `invalid_request` with the validation's words and no echo of the input.
3. `nameIsFree(addr, 0, name)`: the probe allowance, then `CheckNameFree`; a taken or reserved name is 409 `principal_exists` and spends a probe. This happens before the invite code is resolved, so the oracle learns nothing about the code and the code's rate limit is not spent on a name refusal; the ordering mirrors the v2 redemption's (name, then hash, then claim).
4. `RedeemInviteGuest` → `createGuestTx`, which now takes `NewGuest.Name`, calls `claimNameTx(name, 0)` and inserts the principal with the name and its keys; `newGuestHandleTx` runs as today for the handle; the visit row gets `name` and `label` (§5); the audit row `guest_started` gains `name=%q`.
5. A claim lost to a race between step 3 and step 4 is the same 409.

The bare address (`POST /v1/join/guest`) stays 403 for ever; `JoinGuestRequestV1` is not extended (O6).

### 2.4 Conversion (`POST /v1/guest/convert`)

The guest keeps its name. `ConvertGuestRequestV1.name` becomes optional: absent or equal to the current name keeps it; a different name is claimed as a rename with `self = pid` (allowed, but the browser pre-fills the current name and does not invite a change). `nameIsFree` runs as today with `self = pid` only when the name differs. The visit becomes `converted`, the guest credential is revoked, the member credential issued and the password stored, in the one transaction `ConvertGuest` already is. Nothing else is granted; `can_decide` stays as it was (0). The current Guest marker goes because `identity_class` becomes `member`, `ChannelMemberV1.guest` is no longer sent for the principal, and `GuestV1.state` is `converted`. Earlier posts keep their `guest_author` (§2.6).

### 2.5 Release and reuse after expiry

- **Release** is the rename to the handle in `endGuestVisitTx` (§2.2), for every end reason (`end_visit`, `idle`, `expired`, `operator`, `restore`) and for the sweeper, a revocation, a disable and a bulk end, since they all go through it. It is part of the ending transaction, so there is no window in which a visit is ended but its name held.
- **Reuse**: a later visitor, or a new member, may take the name. It is a different principal with a different id, a different handle, no membership, no credential and no read marks: nothing of the earlier visit follows the name. The earlier visit's posts keep their own `guest_author` (handle, name), and the earlier principal row stays, named by its handle, for audit and the operator's guest list.
- **Converted** names are a member's for good; a member's name is freed only by a hide (`hidden-<id>`), as today.
- **Restore** from a backup ends every restored active visit (`restore.go`) through the same path, so names are released then too.
- No reservation period after a visit ends (ADR-009's rejected alternative stands).

### 2.6 Historical attribution after conversion, ending and reuse

`message_guest_attribution` gains a `name` column, written by the insert trigger from the active visit beside `handle` and `label`, and the same no-update and no-delete triggers cover it. `MessageV2.guest_author` and `ReactionV1.guest_reactor` carry it as `GuestAttributionV1.name`. A client renders a message with `guest_author` as **name + Guest marker**, from the attribution, whatever the author's current name or state: after End visit, after conversion (the member's new posts render under the member identity with no marker; the same `author_id` is both), and after another person has taken the same name (their posts carry their own handle in the attribution, so a client that distinguishes "same person" by `guest_author.handle`, as `Conversation.tsx` does at line 342, stays right). Nothing is relabelled and no author is merged.

### 2.7 The Guest marker on every client

The marker is rendered **only** from trusted fields: `WhoAmIResponseV1.identity_class == "guest"` (self), `ChannelMemberV1.guest` (member lists, rosters, mention completion, voice occupants), `MessageV2.guest_author` (messages), `ReactionV1.guest_reactor` (reactor lists), `GuestV1`/`GuestAdminV1` (the visit panel, the operator's list). A name's text never produces it, and the reserved `guest` namespace keeps a name from imitating it.

| Client | Today | Proposed |
| --- | --- | --- |
| Web and desktop (`web/src`) | `guestName()` composes `Guest: label · tag` | `guestName()` returns the attribution's `name` (legacy: §5.4); a `<GuestBadge />` (a `<span class="guest-badge">Guest</span>` beside the name, like `ag-badge`) in `Members.tsx` rows, `Conversation.tsx` author lines, `voice/*` rosters, mention completion (`mention.ts` lists guests by their name), the reactor list, and the self panel (`Guest.tsx`: "Alice, temporary guest"). Mobile layout: the badge wraps with the name. |
| CLI (`conch whoami`, `conch admin guest list`) | `guest: Guest: Pat · a7k4, …` | `name: alice`, `class: guest`, `guest: a temporary visit from invite …`; `guestLine` shows `name` then the handle in parentheses for the operator. |
| TUI | `GuestDisplayName` in `authorName`, `messageAuthor`, status line | `name [guest]` with the same `[`-escaping as `[agent]` (`safeName`, `escapeBadge`); the help line says "Guests are marked [guest]". A message with `guest_author` renders `name [guest]` from the attribution. |
| Plain text and accessibility | none | One constant, `schema.GuestTextMarker = "[guest]"`, for every text-only surface (TUI, notifications, audit lines that name a guest, screen-reader text). In the browser the badge is real text ("Guest"), so a screen reader reads "Alice Guest"; no colour-only marking, and the badge has `title="Temporary guest"`. |
| Operator views | handle everywhere | `GuestAdminV1.name` first; the handle stays visible to operators as the identifier (`conch admin guest end` and `GET /v1/guests` keep taking the id). |

`schema.GuestDisplayName` stays as the renderer of **legacy** attributions (§5.4); a new `schema.GuestShownName(a GuestAttributionV1) string` returns `a.Name` when present and `GuestDisplayName(a.Label, a.Handle)` otherwise, so every Go client has one rule.

### 2.8 Alternatives considered

| Choice | Why not |
| --- | --- |
| Keep the handle as `principals.name`, add a unique `display_name` on `guest_visits` | A second uniqueness index that must also cover members (or members must check it), a guest branch in every consumer of `name`, a second claim path to keep atomic. Twice the surface for the same result. |
| Keep the handle as the name and add `name` only on the wire shapes | Same as above; and `ChannelAuthorV1`, audit details and the TUI's fallbacks would still say `guest-…`. |
| Chosen name stays reserved after the visit ends | Rejected by ADR-009 already: it blocks legitimate sign-up and confuses return visits. |
| Append a short suffix on collision | Nick rejected it (2026-10-30). |
| Remove the `guest` namespace reservation now that no name begins with `guest-` | The reservation is what stops a member from being named `Guest` or `guest-abc` beside real markers; it costs nothing and migration 31's preflight already exists. Kept (O2). |
| Let a guest rename itself mid-visit | Attribution is per post, so it would work, but it reopens review F10's reasoning and adds a rename path for a bounded identity. Ending the visit and rejoining is the rename (O3). |
| Drop `tag` from the wire | Required in `GuestAttributionV1.Validate` and in three published fixtures. Kept on the wire, not rendered in ordinary presentation; operators may still see it. |
| A new error code `name_unavailable` | `principal_exists` already means "taken or reserved" on every account-making route and clients already word it. Reused (O4). |
| `RedeemInviteGuestRequestV2` with a required `name` | The route cannot tell a v1 body from a v2 one, and nothing changes on the response. An additive `name` on v1 with a behaviour note is the precedent of §7.6 B2 in guests-and-accounts.md (O5). |

## 3. Security: threats and abuse

| Threat | Answer |
| --- | --- |
| **Counterfeit status.** A visitor or member names itself to look like a guest, a member or an operator. | The marker is rendered from `identity_class`, `guest`, `guest_author` and `guest_reactor`, never from text. Names beginning with `guest` or `hidden-`, their case and confusable variants, and the server's name are refused by `claimNameTx` with no operator bypass. In the TUI `safeName` escapes `[` in a name so `Bob [guest]` prints as `Bob \[guest]`; in the browser the badge is a separate element. Names are already free of control, bidi and invisible characters (`ValidateDisplayName`). Operator and role marks are not names and are unchanged. |
| **Impersonating a member.** A guest takes a member's name with a look-alike spelling. | `clashTx` compares both keys, so `AIice` and the Cyrillic `Аlice` are `Alice`; this is the same protection members have against each other (#441). |
| **Squatting.** A guest takes a name a future member wants. | Bounded by the visit (24 hours absolute, 2 hours idle, defaults) and by `max_guests`; released at the end. An operator ends a visit to free a name at once. |
| **Existence oracle.** Trying names on the guest route to learn who exists. | The same 409 for taken, reserved and server-name; the per-address name-probe allowance (five an hour) and the invite route limits; the refusal is audited as the redemption refusals are. `hide_members` is not weakened beyond the collision oracle ADR-013 already accepts. |
| **Identity inheritance through reuse.** A later `Alice` reads the earlier Alice's whispers, memberships or approvals. | Identity is the principal id. An ended visit has no credential, no membership, no read marks; the new visitor is a new principal. Nothing is keyed by name. |
| **Attribution rewrite.** Conversion, hide, end or reuse relabels old posts. | `message_guest_attribution` is write-once by trigger; `name` is added under the same triggers. Clients render `guest_author` over the member list. |
| **Privilege through conversion.** Keeping the name grants something. | `ConvertGuest` writes no membership and no right; the name claim is `self = pid`, a no-op for an unchanged name. O7's changes (bounds cleared, voice and the brake on) are #1004's and are not widened here. |
| **Race on the last place or the same name.** | Both are inside `withImmediateTx`; the loser gets 429 `guests_full` or 409 `principal_exists`. |
| **Release failure.** The rename to the handle fails and the visit stays ended with a held name. | It is in the ending transaction, so the end rolls back with it; the handle's keys cannot clash (reserved namespace, checked at admission). Test it with a seeded principal that was given the handle out of band: the end fails loudly rather than silently keeping the name. |
| **Audit.** | `guest_started name=%q origin=… invite=…`; `principal_renamed from=<name> to=<handle>` at the end; `guest_converted from=<handle> to=<name>` unchanged; redemption refusals as today. Never the client address. |

Security review before merge, on every slice that touches admission, conversion or the marker (CLAUDE.md; ADR-009's invariants).

## 4. Schema impact

All additive; no published fixture changes; `scripts/schema-compat.sh` passes with no fixture touched. A bump is Nick's call (CLAUDE.md rule 2); this note asks for none and flags the two behaviour changes below (O5).

### 4.1 New fields

| Type | Field | Meaning |
| --- | --- | --- |
| `GuestAttributionV1` | `name string` (`json:"name,omitempty"`) | The name the guest held when it was admitted or posted. Present on every visit admitted under this design; absent on a legacy visit, whose `label` and `tag` render as before. `Validate`: when present it passes `validatePermanentName`. |
| `GuestV1`, `GuestAdminV1` | `name string` (omitempty) | The same, for the self view and the operator list. |
| `GuestRefV1` | `name string` (omitempty) | For the bulk-end answer, so an operator sees whose visits are ending. |
| `RedeemInviteGuestRequestV1` | `name string` (omitempty on the wire) | The chosen name. The server requires it once guests return; `label`, when sent without `name`, is read as `name` for a client that predates this design, and a request carrying both with different values is 400. `Validate` accepts either; the server's rule is in §2.3. |
| `ConvertGuestRequestV1` | `name` becomes optional | Absent keeps the current name. `Validate` accepts an empty name; a non-empty one passes `validatePermanentName` as today. |

New helpers in `pkg/schema/guest.go`: `GuestTextMarker = "[guest]"`, `GuestShownName(GuestAttributionV1) string`. `GuestDisplayName`, `GuestTag`, `ValidateGuestLabel` and every constant stay.

New golden fixtures (new files only): `guest-attribution-v1-named.json`, `guest-v1-named.json`, `guest-admin-v1-named.json`, `message-v2-guest-named.json`, `channel-member-v1-guest-named.json`, `redeem-invite-guest-request-v1-name.json`, `convert-guest-request-v1-keep-name.json`, `end-guests-response-v1-named.json`. TypeScript regenerated by `go generate ./pkg/schema` (`ts.go` already lists every guest family); `scripts/schema-ts-check.sh` passes.

### 4.2 Unchanged shapes whose values change

- `PrincipalV0.name`, `WhoAmIResponseV1.name`, `ChannelMemberV1.name`, `ChannelAuthorV1.name`, `PrincipalAdminV1.name`: for a live guest, the chosen name instead of the handle; for an ended guest, the handle. The types' comments that say "for a guest, Name is the handle" (`whoami.go`, `JoinGuestResponseV1`) are corrected; no field moves.
- `JoinWarning`, `GuestState`, `GuestOrigin`, `GuestEndReason`, error codes: no new values.

### 4.3 Behaviour changes on published routes, for Nick (as guests-and-accounts.md §7.6 did)

- **B6.** `POST /v1/invites/redeem-guest` without a name is refused (400) once #1004 reopens it. Today the route refuses everything (403), so no working client is broken, but it is a tightening of a published request. Recommended: as written, recorded in the CHANGELOG with #1004's notice.
- **B7.** `POST /v1/guest/convert` accepts an absent `name` (keeps the current). A loosening; a client that sends a name is unaffected.

No MCP tool changes: guests cannot use MCP (`mcp_auth.go`, agents only) and no tool names a guest. Parity holds through REST (ADR-001).

## 5. Migration

One migration, additive (`ADD COLUMN`, trigger replacement), numbered after the last on `main` (41, `archiveMigration`) at the time it lands.

1. `ALTER TABLE guest_visits ADD COLUMN name TEXT` (NULL on every existing row: all are `ended` or `converted` after migration 40).
2. `ALTER TABLE message_guest_attribution ADD COLUMN name TEXT` (NULL on every existing row).
3. Replace `messages_guest_attribution` (drop and create) to copy `v.name` beside `v.handle` and `v.label`; replace `guest_visits_terminal` to freeze `name` as it freezes `label`. The no-update and no-delete triggers on the attribution table need no change: they cover every column.
4. **No data is renamed, merged or reclassified.** Legacy visits keep `label` (NULL or free text, possibly duplicated) and their handles as principal names; their posts keep `{handle, label}` and render as before (§5.4). No principal is renamed by the migration: there is no active visit to release, and an ended visit is already named by its handle. A Go step asserts this (`SELECT COUNT(*) FROM guest_visits WHERE state = 'active'` is 0, else log and release each through `setNameTx` to its handle, actor `migration`), so a database that skipped migration 40's ending, which cannot happen in order, is still left consistent.
5. The `guest` namespace preflight (`reportReservedGuestNames`) and the `reserved_names_in_use` warning stay as they are.

### 5.4 Legacy rendering

A legacy attribution has no `name`. Clients render it exactly as today, `Guest: <label> · <tag>` or the bare handle, with the Guest marker added, so two ended visits both labelled `Nick` stay distinguishable by their tags, and an unlabelled one shows its handle. This is the only place a random suffix remains visible, and it is confined to posts made before this design. No migration invents names for them (O1).

## 6. What stays as it is

ADR-009's bounds, End visit, End all guest visits, the credential rules, history bounds, `can_decide = 0`, no hide and no rename for guests, conversion's policy and permits, the audit actions, `max_guests` and the per-address cap, migration 31's preflight. ADR-013's rules for guests (public channels only, no directory, history from arrival, conversion per O7) are #1004's and untouched. `hide_members` is unchanged.

## 7. Implementation slices

Each slice is written to be filed as an executable-cold issue. Order: S1 first; S2, S4 and S5 in parallel after S1; S3 after S2 and after #1004's redemption slice; S6 last. S2 and S3 are `approval-path` by #1004's rule (they change the transactions that write a guest's memberships and conversion) and are merged by Nick; every slice gets a `security-reviewer` pass.

### S1. Schema: the guest's name on the wire (`area/schema`)

- **Context.** §2.7, §4 of this note. Additive; no bump; no fixture changes.
- **Files.** `pkg/schema/guest.go` (fields, `GuestTextMarker`, `GuestShownName`, `Validate` rules, corrected comments), `pkg/schema/invite.go` (`RedeemInviteGuestRequestV1.name`), `pkg/schema/guest_test.go`, `invite_test.go`, `golden_test.go`, new fixtures under `pkg/schema/testdata/`, `web/src/schema/` regenerated.
- **Acceptance criteria.** The fields of §4.1 exist with their `Validate` rules; `GuestShownName` returns `name` when present and `GuestDisplayName` otherwise; `ConvertGuestRequestV1.Validate` accepts an empty name; `RedeemInviteGuestRequestV1.Validate` accepts `name`, `label`, or both equal, and refuses both different; every existing fixture is byte-identical; `scripts/schema-compat.sh` and `scripts/schema-ts-check.sh` pass.
- **Required tests.** Table-driven `Validate` rows (accepting and refusing) for each changed type; golden round-trips for every new fixture; `go test ./pkg/schema/...`; `make check`.
- **Blocked by.** Nick's decisions O1, O4, O5 (§8). Not blocked by #1004.

### S2. Store: claim, release and attribution of the name (`area/server`, `approval-path`, `security`)

- **Context.** §2.2, §2.4, §2.5, §2.6, §5. The store rule applies to visits seeded through the store before any route reopens, so this slice is testable without #1004.
- **Files.** `internal/server/store/store.go` (the migration), `store/guests.go` (`NewGuest.Name`, `createGuestTx` claiming through `claimNameTx`, `endGuestVisitTx` releasing through `setNameTx`, `scanGuestVisit`, `GuestVisit.V1/Admin/Attribution`, `guestAuthorTx`, `GuestConversion.Name` optional in `ConvertGuest`), `store/audience.go` (`messageColumns` reads `name`), `store/guest_policy.go` (`GuestEntry.Name`), `store/guests_test.go`, `store/guests_removed_test.go`, `store/community_migration_test.go`, `e2e/isolation/seed.go` (seeds with names).
- **Acceptance criteria.** A seeded visit holds its name in `principals.name` and clashes with members and other live guests by both keys (exact, case, confusable); two admissions racing on one name produce one visit and one `ErrDuplicate`; a member rename or create to a live guest's name is `ErrDuplicate`; ending a visit for every reason renames the principal to its handle and audits `principal_renamed`; after the end the name is claimable by a new visit and by a member, and the new principal has no membership, credential or read mark of the old; conversion with an absent or equal name keeps it and audits no rename, with a different name claims it; posts carry `name` in `guest_author` after end, conversion and reuse; legacy rows (NULL `name`) are read back with `name` absent; the migration on a pre-upgrade database changes no name.
- **Required tests.** Table-driven store tests for each criterion; a concurrency test with two goroutines on `CreateGuest` with one name; the migration test over a fixture database with legacy labelled, unlabelled and duplicate-labelled visits; **the approval chain (CLAUDE.md rule 3), request → notify → resolve → audit, over the real server:** extend `internal/server/conversion_approval_test.go` so a converted guest under its kept name cannot decide (`decision_not_permitted`) and the approval resolves through the existing approvers; `go test -race ./internal/server/...`; `make check`.
- **Blocked by.** S1; Nick's decisions O1 and O3.

### S3. Routes: name on admission and conversion (`area/server`, `approval-path`, `security`)

- **Context.** §2.3, §2.4, §3. Lands with or after #1004's redemption slice, since the route is 403 until then.
- **Files.** `internal/server/join.go` (`handleRedeemInviteGuest`: name required, `nameIsFree` before the code, `principal_exists` mapping, `label` compatibility), `internal/server/conversion.go` (optional name), `internal/server/guests.go` (`guestWhoAmI`), `internal/server/join_test.go`, `conversion_test.go`, `guests_removed_test.go`, `e2e/isolation/routes.go`, `matrix.go` (rows: a guest naming a hidden member's name gets the same 409 as any taken name; the authors route for an ended guest's post).
- **Acceptance criteria.** The sequence of §2.3 holds; a missing name is 400 before any code check or use spent; a taken name is 409 `principal_exists` and spends a probe; nothing is revealed about the holder; conversion keeps the name by default; `GET /v1/whoami` for a guest carries the chosen name and `identity_class: guest`; the CHANGELOG carries B6 and B7.
- **Required tests.** Route tables (member name, live guest name, hidden member name, reserved names, server name, confusables, absent name, label-only body, both fields); the isolation rows and `go run ./e2e/isolation`; the approval chain test of S2 run over HTTP; `go test -race ./internal/server/...`; `make check`.
- **Blocked by.** S2; #1004's redemption slice; Nick's decisions O4, O5.

### S4. Web and desktop: name plus Guest badge (`area/web`)

- **Context.** §2.7, §5.4. Renders from fixtures, so it needs no running guest; the Playwright guest spec returns with #1004.
- **Files.** `web/src/guestName.ts`, a new `GuestBadge.tsx`, `Conversation.tsx`, `Members.tsx`, `useLiveChannel.ts`, `voice/useVoiceNames.ts`, `voice/useOccupantNames.ts`, `mention.ts`, the reactor list, `Guest.tsx` (self panel; "Create an account" pre-fills the name and says it is kept), `CreateAccount.tsx` (purpose `convert`: name pre-filled, read-only unless Nick takes O3's alternative), `nameRefusal.ts`, `app.css`, their unit tests, `web/e2e/22-guest.spec.ts` (when #1004 lands).
- **Acceptance criteria.** A member row, author line, voice occupant, mention candidate, reactor and self panel with a named attribution show the name and a `Guest` badge, and nothing of the handle or tag; a legacy attribution shows `Guest: label · tag` plus the badge; a message with `guest_author` keeps its badge after the author appears in the member list as a member; the badge is text, has a title, and is not produced by any name; mobile layout wraps correctly; no `any`, no inline style.
- **Required tests.** Unit tests with mocked fetch for each surface and for the legacy case; an accessibility assertion that the badge is in the accessible name; a negative test that a member named `Bob Guest` or `Bob [guest]` gets no badge; `make web-check`; `make web-e2e` once the guest spec is back.
- **Blocked by.** S1.

### S5. CLI and TUI: `[guest]` marker (`area/cli`)

- **Context.** §2.7. Renders from the schema helpers; the join verb returns with #1004 (I11a) and gains `--name`.
- **Files.** `internal/cli/tui/model.go` (`authorName`, `messageAuthor`, the status line), `tui/help.go`, `tui/community.go`, `tui/guest_test.go`, `internal/cli/command.go` (`whoami`), `internal/cli/admin_guest.go` (`guestLine`), `internal/cli/command_test.go`, `onboarding_test.go`.
- **Acceptance criteria.** A live guest and a guest-authored message show `name [guest]` with `[` escaped in the name; a legacy attribution shows `Guest: label · tag [guest]`; `whoami` prints the name and `class: guest`; the operator list prints the name and the handle; the help text no longer mentions tags; `--name` is required by `conch join` for a guest link when that verb exists.
- **Required tests.** Table-driven rendering tests including a name containing `[guest]`; hostile-server tests for a malformed attribution (still shown by id); `make check`.
- **Blocked by.** S1.

### S6. Docs (`docs-agent`)

- **Files.** `docs/design/guests-and-accounts.md` (a header note pointing here; §1, §3.3, §4.1 and §4.4 marked superseded by this note), `docs/design/community-model.md` §2.2 and §14 (one line each: names per guest-names.md), `README.md` (the guest guidance, once #1004 lands: no mention of tags), `CHANGELOG.md`, `docs/design/mobile-app-ux.md` (one line: the badge, when guests reach the phone).
- **Blocked by.** Nick's ADR-009 amendment (§9) merged.

## 8. Open decisions for Nick

1. **Legacy visits keep their old rendering** (`Guest: label · tag`, or the handle), with the badge added, and no migration invents a name for them. Alternative: render legacy posts as `label [guest]` and accept that two old `Nick`s look alike. **Recommendation:** keep the old rendering for legacy posts only; it is the one honest disambiguation and it fades with the history.
2. **Keep the `guest` namespace reserved for permanent names** although no guest is named `guest-…` any more. Alternative: lift the reservation and the migration 31 preflight. **Recommendation:** keep it; it is what stops a member from naming itself `Guest`, and it costs nothing.
3. **A guest's name is fixed for the visit** (no self-rename; End visit and rejoin is the rename), and **conversion keeps it by default** with a different name allowed in the request. Alternative A: conversion may not change the name at all (`name` ignored). Alternative B: allow a guest to rename itself once mid-visit. **Recommendation:** as written; the browser pre-fills and does not offer a change, so the default path is "keep", and the server stays permissive for the CLI.
4. **A taken name on the guest route answers 409 `principal_exists`**, the code every account-making route uses, through the same probe allowance. Alternative: a new code. **Recommendation:** reuse; a new code adds vocabulary for no new meaning.
5. **Additive `name` on `RedeemInviteGuestRequestV1` (label read as name for old clients) and optional `name` on `ConvertGuestRequestV1`; no version bump.** Alternative: `RedeemInviteGuestRequestV2`. **Recommendation:** additive, recorded as B6 and B7 in the CHANGELOG, the way §7.6 B1 and B2 were; the route cannot distinguish body versions anyway.
6. **`JoinGuestRequestV1` (the bare address) is left as it is** since the route is refused for ever under ADR-013. Alternative: add `name` for symmetry. **Recommendation:** leave it; a field nothing can send is a trap.
7. **ADR-009 amendment** as drafted in §9, made by Nick as an "Amended" line, the way #975's was. **Recommendation:** adopt the text in §9.1; ADR-013 needs no edit (its decision 6, "marked as guests in the channels they visit", is what this note implements).

## 9. Proposed ADR text

### 9.1 ADR-009, new "Amended" line (Nick edits the ADR)

> - **Amended:** 2026-11-04 ([#1003](https://github.com/njdaniel/conch/issues/1003), design [guest-names.md](../design/guest-names.md)): D2's name contract is replaced by Nick's decision of 2026-10-30. A guest chooses an ordinary name at admission; it is the guest's principal name, claimed by the same clash check as every member's name (case, confusables, the `guest`, `hidden-` and server-name reservations), unique among members, operators, agents and live guests, and never suffixed or silently changed: a taken name is refused and another asked for. Every surface shows the name with a separate Guest marker rendered from the stored guest state, never from text; terminals and other text-only surfaces use `[guest]`. The server-made `guest-<random suffix>` handle stays as the visit's internal, never-reused identifier and the key of its attribution; it is not shown in ordinary presentation. When a visit ends the name is released (the principal is renamed to its handle) and may be taken by anyone, with nothing of the visit following it. Conversion keeps the name for the same principal and grants nothing. Posts made during a visit keep the name and the marker they were posted under, through ending, conversion and later reuse of the name. The `Guest: <label> · <tag>` rendering remains only for visits admitted before this amendment. Item 3's "permanent usernames are never occupied by guest names" is read as "a live guest holds its name exactly as a member does, and gives it back when the visit ends". The alternatives "keep a guest's chosen normal username reserved after logout" (rejected) and "temporary names in a reserved namespace" (the handle, kept internal) stand.

### 9.2 ADR-013

No edit. Decision 6 ("marked as guests in the channels they visit") and the 2026-10-30 amendment are unchanged; #1004 restores guests and this note names them.

## 10. Verification of this note

`./scripts/invisible-chars.sh` and `./scripts/ci-docs-only-test.sh` pass; every relative link above resolves on `main` at the pinned commit. Not verified here, because nothing is implemented: that `claimNameTx` admits the handle rename at the end of a visit against a legacy principal that holds the exact handle string with different stored keys (S2's seeded test covers it), and the exact migration number (taken at landing).
