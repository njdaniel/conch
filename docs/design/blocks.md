# Personal blocks

Issue #1832 (from the #1821 audit, gap 10, draft D8; epic #1491). Status:
**proposed, awaiting Nick's sign-off** with the additive `pkg/schema` change
(`pkg/schema/block.go`). Direct conversations (epic #2013, #2018) will consult
the same relation.

## 1. What a block is

A block is one person's preference about another principal: "I would rather
not see this person's messages or be alerted about them." It is **not a
security boundary** and not moderation.

- It removes nobody from anything, stops nobody reading anything, and exempts
  nobody from moderation. The blocked person keeps reading what the blocker
  writes in every channel they share.
- It changes only what the blocker's own clients show and what the server
  alerts the blocker about.
- The blocked person is never told, and nothing they can read (a route, a
  frame, a roster, a count or an error) differs because of it.

The confirmation says so in plain words before a block is made
(`blockExplanation` in `web/src/blocks.tsx`).

## 2. The list (D1–D3)

- **Per principal, on the server.** One row per (owner, blocked) pair
  (migration 51, table `blocks`). Stored on the server so it reaches every
  device and so push, which the server sends, can honour it.
- **Visible only to its owner.** No route names an owner: `GET /v1/blocks`
  is always the caller's own list. Another member, an operator, the owner of
  the server and the blocked person all read their own list. No audit row is
  written, because operators read the audit log and a block is not theirs to
  see.
- **Reaches all devices.** A change that took effect sends
  `conch.blocks_changed.v1` (no content) to the owner's own human events
  sockets, and every client re-reads the list on that signal and on each
  reconnect.
- **Size limit.** `schema.MaxBlocks` = 1000 per owner. One more is
  `409 block_limit_reached`; a repeat of an existing block always succeeds.
  The count and insert are one immediate transaction.

## 3. Wire shapes and routes (additive, no version bump)

| Route | Answer |
|---|---|
| `GET /v1/blocks` | `ListBlocksResponseV1 {blocks: [BlockV1], limit}`, oldest first |
| `PUT /v1/blocks/{principal_id}` | `200 BlockV1 {principal_id, name, kind, created_at}`; idempotent, the original `created_at` on a repeat |
| `DELETE /v1/blocks/{principal_id}` | `204`, whether or not there was a block, or anyone |
| events socket | `BlocksChangedV1 {schema: "conch.blocks_changed.v1"}`, owner only |

Refusals: yourself `400 invalid_request`; an id that is nobody, a hidden
principal, or one the caller may not know `404 principal_not_found`, all
alike. "May know" is the directory's rule (`knownPrincipalsSQL`): everyone
with `see_member_directory`, otherwise channel-mates and one's own joiners. So
a PUT is no probe for principals or their names. The list shows each blocked
principal's present name and kind.

Access: people with a full credential. An agent `403 human_required`, a guest
`403 guest_not_allowed`, auth off `400 read_state_require_auth`; not
previewable, not in the admin catalog, no re-authentication. Reads and writes
share the notification limiter (reads per sign-in, writes per person).

## 4. What a block does, for the blocker only

- **Log, search, threads:** a blocked person's message collapses to one row,
  "Blocked message", with Show. Show reveals it in that view only; nothing is
  stored.
- **Alerts:** no browser alert (`useNotifications`), no pending-feed entry
  (`ListPendingNotifications`) and no push wake (`PushTargets`) for their
  messages or their mentions of the blocker.
- **Voice:** muted locally by default (volume 0 on that page), once per
  person per page; the slider and an Unmute button in Voice options undo it,
  and that choice is kept. Unblocking someone still at 0 plays them again.
  Nothing is sent; nobody else hears any difference.

## 5. Never hidden (D4)

- **Decisions:** a message carrying `conch.approval.v1` or
  `conch.admin_action.v1` is never collapsed and still alerts
  (`schema.BlockExemptPayload`, `isBlockExempt`). A decision must not be
  hidden.
- **Moderation:** tombstones and moderation notices are never collapsed.
  Moderators' tools and views are unchanged: a block is not read by any
  membership, visibility or permission predicate.

## 6. Who can be blocked, and by whom (D5–D7)

- **Agents:** may be blocked. Their chat collapses; their approval and
  admin-action cards do not.
- **Operators and the owner:** may be blocked, with the same exceptions. A
  block exempts nobody from moderation in either direction.
- **Guests:** a guest keeps no list (short-lived; refused). A member may block
  a guest principal; the row outlives the visit harmlessly, since a new visit
  is a new principal.
- **Direct conversations** (#2013, #2018, not built here): blocking will
  refuse a new conversation from the blocked person, who is told only
  "unavailable". `store.IsBlocked(owner, other)` is the question to ask.

## 7. Out of scope

Blocking by name pattern, reporting (#1841), and hiding presence or typing.
