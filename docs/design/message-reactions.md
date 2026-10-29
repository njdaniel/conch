# Design: Unicode message reactions

- **Status:** Proposed for principal and security review, issue [#764](https://github.com/njdaniel/conch/issues/764) (child of epic #752). No implementation is authorized by this note. The wire shapes in §5 are compatible additions; §16 lists what still needs Nick.
- **Builds on:** [ADR-001](../adr/ADR-001-mcp-native-architecture.md) (MCP tools map onto the REST/WS API; parity rule D6), [ADR-005](../adr/ADR-005-nets-and-whispers.md) and `store/audience.go` (one visibility condition), [ADR-009](../adr/ADR-009-guests-and-open-joining.md) and [guests-and-accounts.md](guests-and-accounts.md) (guest identity, history bound, attribution), [ADR-010](../adr/ADR-010-delegated-administration.md), [moderation.md](moderation.md) (message removal, proposed; §11 here is written to compose with it), [mcp-endpoint.md](mcp-endpoint.md).
- **Current-code baseline:** origin/main `407af84` (v0.1.0-alpha.5), inspected 2026-10-28 in a worktree at `89b82c5` (= `407af84` + #940's `desktop/` skeleton, no server, schema or web change). Paths and behaviour below were checked there. A search of `pkg/schema`, `internal/server`, `internal/cli` and `web/src` found **no reaction API, schema, table or client control**, and no emoji dataset yet: #763 (the browser picker) is open and not merged at this commit.
- **Implements:** this document is the contract for #765 (server: store, REST/WS, MCP; security review), #766 (browser) and #767 (terminal). It does not move reactions into the alpha exit gate.

## 1. Decisions in one page

A reaction is a durable record **(message, principal, emoji)**: one principal has reacted to one message with one canonical emoji key, at one time. Counts are always derived from those records. There is no toggle: setting a reaction you already hold and removing one you do not hold are idempotent no-ops that answer the current state.

| Operation | REST | MCP tool | Who | Answer |
| --- | --- | --- | --- | --- |
| Set own reaction | `PUT /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}` | `set_reaction` | A member who can see the message and may transmit where it was said | 200 `ReactionSummaryV1` |
| Remove own reaction | `DELETE /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}` | `remove_reaction` | Same | 200 `ReactionSummaryV1` |
| Summary of one message | `GET /v2/channels/{channel}/messages/{message_id}/reactions` | (via `read_reactions`) | A member who can see the message | 200 `ReactionSummaryV1` |
| Reactors of one emoji | `GET /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}?after=&limit=` | `list_reactors` | Same | 200 `ListReactorsResponseV1` |
| Batch summaries for a page of history | `GET /v2/channels/{channel}/reactions?messages=1,2,3` (≤ 100 ids) | `read_reactions` | A member; each message held to visibility | 200 `ReactionSummariesResponseV1` |
| Live update | `conch.reaction_summary.v1` frame on `GET /v1/events/ws` | — | Every subscriber who can see the message, at send time | whole state of that message's reactions |

Rules that hold everywhere:

- **Visibility is the message's.** Every read, count, reactor identity, write, live frame and replay is gated by the one visibility condition in `store/audience.go` (`messageVisible`: audience, current membership, enabled principal, history bound), evaluated for the caller in the statement that selects the rows. A message the caller cannot see and a message that does not exist get the same answer, 404 `message_not_found`, byte for byte once the id is set aside.
- **Writing needs participation.** A write additionally requires that the caller is, now, a member of the channel, an enabled principal, not a monitor of the net the message was said on, and (for an agent) holds `messages.post` with `post` in the channel. Operators get no exemption on either side.
- **Identity is the caller's.** No request names a principal; the store binds the credential's principal. Agents act as themselves under their manifest, and spend their `messages.post` budget. Nothing a reaction does triggers an agent, moves a read mark, or sends a notification.
- **One emoji key.** The key is the fully-qualified Unicode emoji sequence from the pinned `emoji-test.txt` that #763's picker uses; presentation selectors are equivalence, skin tones and ZWJ sequences are distinct; arbitrary text and concatenated emoji are refused (§3).
- **Records outlive access.** Like messages, reactions stay when the reactor leaves a channel, is disabled, is hidden, ends a guest visit or converts to an account; they are never reassigned, and an ended identity cannot act (§9).
- **Convergence is by whole state.** Frames and responses carry the complete summary of one message, never a delta; a client that misses one loses nothing it cannot recover from the next, and re-hydrates by the batch route on reconnect (§8).

## 2. What exists now

| Area | Where | What it gives reactions |
| --- | --- | --- |
| The one visibility function | `internal/server/store/audience.go` (`messageVisible`, `visibilityArgs`, `withinHistoryBound`, `noHistoryBound`) | Reactions embed `messageVisible` in their own statements. That file is the only one that may read `messages`; `TestMessageReadsStayInTheVisibilityFunction` enforces it, so the reaction store's message checks live there or call into it. |
| Per-principal per-channel state with a socket document | `store/read_state.go`, `server/read_state.go`, `schema/read_state.go` | The nearest precedent: `ON CONFLICT` upsert, a document the events socket re-sends, refusal under auth-off (`read_state_require_auth`), human-only (reactions are not human-only). |
| Membership and agent gates | `members.go` `channelForCaller(w, r, name, capability, permission)`; `authz.go` `agentManifestCheck`, `spendAgentBudget`, `agentScope` (MCP) | The two gates every reaction route starts with: non-member → `channel_not_found`; agent refused by manifest → `forbidden`, audited. |
| Events socket | `events_ws.go`: `eventRef`, `eventsHub.notify*`, per-subscriber goroutine turns a reference into a document **with the subscriber's reader at send time**; `eventsQueueMax`, floors | The transport for live reaction frames. A reference carries no content; the document is read through the store with the socket's reader. Agents may hold an events socket (`subscribeKind(..., human=false)`). |
| Message socket | `ws.go` `/v2/ws` with `?after=` replay and `historyGate`; `hub.Hub` | Carries `MessageV2` frames only. Reactions do not travel on it (§8). |
| MCP | `mcp.go` (`addAgentTool`, `mcpToolError`, tools `post_message`, `read_channel`, …), `mcp_auth.go` (agents only), tool→capability map in `authz.go` | Four new tools are plain mirrors of the REST routes. |
| Guests | `store/guests.go` (`endGuestVisitTx` removes memberships, read marks, credential; `ConvertGuest` keeps the principal id, memberships and `history_after`), `message_guest_attribution`, `schema.GuestAttributionV1` | The lifecycle §9 follows; the attribution snapshot pattern reused for reactors. |
| Hiding, disabling | `store/hide.go` (rename to `hidden-<id>`, memberships removed, messages stay), `store/disable.go` | What happens to a reactor's rows (§11). |
| Moderation (proposed) | `docs/design/moderation.md` §7, §10: `DELETE /v2/channels/{channel}/messages/{message_id}`, `MessageV2.removed`, error `message_not_found`, table `message_removals` | §11 defines reactions on a removed message; the `message_not_found` code is defined once, by whichever PR lands first. |
| Isolation harness | `e2e/isolation/matrix.go` (every route needs a row: refusal code, no marker of A, same answer as a ghost), `guestrows.go` (history-bound rows), `routes.go` coverage check | Every new route gets rows (§14). |
| Rate limits | `ratelimit.go` (agents, per manifest capability), `invite_limits.go` `bucketSet`/`tokenBucket` (humans, keyed) | Agents spend `messages.post`; humans get a keyed bucket (§7.4). |
| Migrations | `store/store.go` `migrations` (last entry 36); a binary refuses a database whose `user_version` is newer | §12. |
| Schema tooling | `pkg/schema/ts.go`, `vocabularies.go`, `go generate ./pkg/schema`, `scripts/schema-compat.sh`, `scripts/schema-ts-check.sh`; golden fixtures in `pkg/schema/testdata/` (303 files) | §5 types are listed in `TSTypes`; fixtures added; no published fixture changes. |
| Emoji data | none at this commit; #763 will add a pinned `emoji-test.txt`, a generating script with a checked hash, and a bundled catalog for the picker | §3.1 fixes how reactions share it. |

## 3. The reaction key

### 3.1 Dataset and version

One source: the Unicode Consortium's `emoji-test.txt` for one pinned Unicode Emoji version (Unicode-3.0 licence), vendored in the repository as #763 lands it (proposed path `third_party/unicode/emoji-test.txt` with its licence beside it; #763 decides the path, this document binds the contract, not the path). One pinned SHA-256 of that file, in the generating script. Two generated outputs from the same run of the same script:

- `pkg/schema/emoji_catalog_gen.go`: the Go table the server validates and canonicalizes keys with (§3.2), and the constant `schema.EmojiVersion` (for example `"16.0"`), taken from the file's `# Version:` header.
- the web catalog (JSON) #763's picker searches, carrying the same version string.

`make check` fails when either output is stale against the vendored file or the vendored file's hash differs from the pin (the `schema-ts-check.sh` pattern). The TypeScript constant `EMOJI_VERSION` is generated from `schema.EmojiVersion` through `go generate ./pkg/schema`, so the browser cannot ship a catalog of another version than the server validates against without `scripts/schema-ts-check.sh` noticing. The terminal client (#767) uses the Go table directly: it is in `pkg/schema`, which `conch` already imports.

Which PR creates the script is a sequencing question, not a contract one: if #763 lands first, #765 adds the Go output to its script; if #765 lands first, it creates the script with the Go output and #763 adds the JSON. Neither adds an npm or `go.mod` dependency for the data. The vendored data file's licence is Nick's to accept once (§16).

Upgrading the pinned version is a **compatible** change: it widens the set of accepted keys. Stored keys are never validated against the catalog on read, so a server downgraded to an older catalog still serves every stored reaction (§12).

### 3.2 Canonical key

Definitions, with `emoji-test.txt` statuses: `fully-qualified`, `minimally-qualified`, `unqualified`, `component`.

1. **The canonical key is the fully-qualified sequence**, exactly as the file lists it (code points in order, including U+FE0F where the fully-qualified form has one). It is stored, compared and sent as that UTF-8 string.
2. **Lookup ignores presentation selectors.** To canonicalize an input: bound it (§3.3); remove every U+FE0F (VS16) and U+FE0E (VS15); look the remainder up in the table keyed by *every listed form with its selectors removed* (fully-, minimally- and unqualified forms all strip to the same remainder); the value is the fully-qualified key. There is no other normalization: emoji code points have no canonical decompositions, so NFC would be a no-op, and no case folding applies (regional indicators and tag characters are caseless).
3. **Components alone are refused.** A skin-tone modifier (U+1F3FB..U+1F3FF) or hair component on its own has status `component` and is not a reaction.
4. **Skin tones and ZWJ sequences are distinct keys.** `👍` and `👍🏽` are two keys; `👨‍👩‍👧` is one key and is not the three people; `🏳️‍🌈` is one key distinct from `🏳️`.
5. **Everything else is refused**: an empty string, a sequence not in the table after step 2, any string that holds more than one listed sequence, letters, digits, punctuation, whitespace, control or format characters, and anything over the bounds. Refusal is 400 `invalid_reaction` and stores nothing.

Where it lives: `schema.CanonicalReactionKey(s string) (key string, err error)` and `schema.ValidateReactionKey(key string) error` (true only when `key` is already canonical) in `pkg/schema/reaction.go`, over the generated table. The server canonicalizes every input (path segment or MCP argument) and answers with the canonical form; `ReactionSummaryV1.Validate` and `ReactionV1.Validate` require canonical keys, so a fixture or a frame with a non-canonical key fails validation.

Examples (code points in hex):

| Input | Steps | Result |
| --- | --- | --- |
| `👍` (1F44D) | fully-qualified as given | key `1F44D` |
| `❤` (2764) | unqualified → strip → `2764` → table | key `2764 FE0F` (`❤️`) |
| `❤️` (2764 FE0F) | strip → `2764` | key `2764 FE0F` |
| `❤︎` (2764 FE0E, text presentation) | strip → `2764` | key `2764 FE0F`: a reaction is always emoji presentation |
| `👍🏽` (1F44D 1F3FD) | fully-qualified | key `1F44D 1F3FD`, distinct from `👍` |
| `🏽` (1F3FD) alone | status `component` | **refused** |
| `#️⃣` (0023 FE0F 20E3) | fully-qualified | key `0023 FE0F 20E3` |
| `#⃣` (0023 20E3) | unqualified → strip → `0023 20E3` | key `0023 FE0F 20E3` |
| `#` (0023) | not in table | **refused** |
| `🏳‍🌈` (1F3F3 200D 1F308) | unqualified → strip | key `1F3F3 FE0F 200D 1F308` (`🏳️‍🌈`) |
| `👨‍👩‍👧` (1F468 200D 1F469 200D 1F467) | fully-qualified | one key |
| `👨👩👧` (no ZWJ) | three sequences | **refused**: not one listed sequence |
| `🇫🇷` (1F1EB 1F1F7) | listed flag | key `1F1EB 1F1F7` |
| `🏴󠁧󠁢󠁥󠁮󠁧󠁿` (1F3F4 + tags + E007F) | listed subdivision flag | one key |
| `👍👍`, `:+1:`, `thumbs up`, `A`, `1`, ` ` | not one listed sequence | **refused** |
| `🤷‍♀️` (1F937 200D 2640 FE0F) | fully-qualified | one key, distinct from `🤷` and `🤷‍♂️` |

Shortcodes (`:+1:`) are a **client** vocabulary for entry and for terminals that cannot render a glyph; they never travel on the wire. `emoji-test.txt` carries CLDR short names ("thumbs up: medium skin tone"), not shortcodes; if #767 wants a shortcode table that is a separate dataset and licence question (§16).

### 3.3 Bounds

`MaxReactionKeyBytes = 64` and `MaxReactionKeyCodePoints = 16`, applied to the input before any lookup. The longest listed sequences (family ZWJ sequences, subdivision flags) are under 36 bytes and 10 code points; the headroom is for a future version. Both are `pkg/schema` constants so the three clients refuse the same inputs before sending.

## 4. Operations and errors

### 4.1 REST

All routes are `accessAuthenticated` in `routes.go`; the handler applies the gates of §6. `{emoji}` is the percent-encoded UTF-8 of the key in any qualification (the server canonicalizes; `http.ServeMux` decodes path values). `{message_id}` is the message's id. The `/v2/` prefix is deliberate: these routes answer about messages that may carry an audience, and only a v2 reader may be told of one (`store.Reader.Scoped`).

| Route | Body → answer | Refusals beyond the common ones (401, 404 `channel_not_found` for a non-member or unknown channel, 403 `forbidden` for an agent refused by its manifest) |
| --- | --- | --- |
| `PUT …/messages/{message_id}/reactions/{emoji}` | none → 200 `ReactionSummaryV1` (the message's whole summary after the write, `me` true for this key) | 400 `invalid_reaction`; 404 `message_not_found` (unknown, another channel's, or invisible to the caller); 403 `reaction_not_permitted` (a net monitor; a removed message, §11); 409 `too_many_reactions`; 429 `rate_limited` with `Retry-After` and `retry_after_seconds`; 400 `reactions_require_auth` (authentication off) |
| `DELETE …/messages/{message_id}/reactions/{emoji}` | none → 200 `ReactionSummaryV1` | 400 `invalid_reaction`; 404 `message_not_found`; 429; 400 `reactions_require_auth`. Removing a reaction one does not hold is a no-op that answers the summary. A monitor or a removed message gets 200 too: removing one's own mark never needs transmit rights. |
| `GET …/messages/{message_id}/reactions` | → 200 `ReactionSummaryV1` (an empty `reactions` list when none) | 404 `message_not_found`; 400 `reactions_require_auth` |
| `GET …/messages/{message_id}/reactions/{emoji}?after=<principal_id>&limit=<1..100>` | → 200 `ListReactorsResponseV1`, ordered by `principal_id` ascending; `next_after` present when there may be more | 400 `invalid_reaction`, 400 `invalid_request` (cursor, limit); 404 `message_not_found`. An emoji nobody used is an empty list, not an error. |
| `GET /v2/channels/{channel}/reactions?messages=<id>[,<id>…]` | → 200 `ReactionSummariesResponseV1`: one summary per listed message that the caller may see **and** that has at least one reaction; ordered by `message_id` ascending | 400 `invalid_request` (more than `MaxReactionBatchMessages` = 100 ids, a non-positive or duplicate id, or none). An invisible, unknown or unreacted id is simply absent; the three are indistinguishable. |

Recent-auth class: every route here is an authenticated self-write or read, never an operator grant, so `TestRecentAuthClass` pins them `reauthNone`. The route-table test takes a policy decision for each.

Why 200 with the summary on `PUT` and `DELETE` rather than 201/204: the caller's next render needs the authoritative state (another client of the same person may have changed it), and the socket frame may arrive before or after the response. One body shape for the write answer and the live frame keeps both clients' code to one path.

Why the batch route rather than a `reactions` field on `MessageV2`: a page of 50 messages is hydrated in one extra request with one aggregate statement, which satisfies "no N+1"; `MessageV2`, its fixtures, `/v2/ws` frames and `read_channel` output stay untouched; and an old client never sees a field it would ignore. A later `?include=reactions` on the history list would be additive if wanted.

### 4.2 MCP

Four tools, each a plain mirror of one REST route and nothing more (parity, D6): same gates, same store functions, same error codes through `mcpToolError`. Inputs name the channel by name as the other tools do; the server binds the agent's principal.

| Tool | Input | Output | Capability / channel permission |
| --- | --- | --- | --- |
| `set_reaction` | `{channel, message_id, emoji}` | `ReactionSummaryV1` | `messages.post` / `post` |
| `remove_reaction` | `{channel, message_id, emoji}` | `ReactionSummaryV1` | `messages.post` / `post` |
| `read_reactions` | `{channel, message_ids: [..≤100]}` | `ReactionSummariesResponseV1` | `messages.read` / `read` |
| `list_reactors` | `{channel, message_id, emoji, after?, limit?}` | `ListReactorsResponseV1` | `messages.read` / `read` |

Tool descriptions say what the server does and does not do: "Reacting does not post a message, does not notify anyone and does not mark anything read; the people who can see the message see the reaction." The tool→capability map in `authz.go` gets the four entries; `docs/design/mcp-endpoint.md` lists them. No MCP-only capability is introduced; the single-message `GET …/reactions` has no tool because `read_reactions` with one id is the same call.

### 4.3 Error codes (new, additive on `schema.Error`)

| Code | HTTP | Meaning |
| --- | --- | --- |
| `message_not_found` | 404 | The message does not exist, is in another channel, or is not visible to the caller. One answer for all three (also proposed by moderation.md §10; defined once). |
| `invalid_reaction` | 400 | The emoji is not one canonical reaction key (§3.2) or exceeds the bounds (§3.3). |
| `reaction_not_permitted` | 403 | The caller can see the message but may not react to it: a monitor of the net it was said on, or a removed message. |
| `too_many_reactions` | 409 | The write would exceed `MaxReactionKindsPerMessage` or `MaxReactionsPerPrincipalPerMessage` (§7.3). Nothing stored. |
| `reactions_require_auth` | 400 | `conchd` runs with authentication off; there is no principal to hold a reaction. Same pattern as `read_state_require_auth`. |

Existing codes reused unchanged: `channel_not_found`, `forbidden`, `rate_limited`, `invalid_request`, `request_too_large`, `internal_error`.

## 5. Wire types (proposed `pkg/schema/reaction.go`)

All additive: new types, new constants, new error codes; no published type or fixture changes. `ReactionSummaryV1` carries a `schema` field because it travels on the events socket; the REST-only bodies do not, following `ChannelMemberV1`/`ChannelMembersV1`. Every type is listed in `ts.go`'s `TSTypes` with a fixture family; the vocabularies are unchanged.

```go
package schema

// ReactionSummarySchemaV1 names the v1 reaction summary: every reaction on
// one message, aggregated, as the caller may see it. It is the body of the
// reaction reads and writes and the frame the events socket sends.
const ReactionSummarySchemaV1 = "conch.reaction_summary.v1"

// EmojiVersion is the Unicode Emoji version of the generated catalog that
// CanonicalReactionKey validates against (emoji_catalog_gen.go). The browser
// catalog is generated from the same file and carries the same string.
const EmojiVersion = "16.0" // generated; illustrative here

const (
	MaxReactionKeyBytes                = 64
	MaxReactionKeyCodePoints           = 16
	MaxReactionKindsPerMessage         = 20
	MaxReactionsPerPrincipalPerMessage = 10
	MaxReactionBatchMessages           = 100
	DefaultReactorLimit                = 50
	MaxReactorLimit                    = 100
)

const (
	ErrorCodeMessageNotFound       = "message_not_found"       // 404
	ErrorCodeInvalidReaction       = "invalid_reaction"        // 400
	ErrorCodeReactionNotPermitted  = "reaction_not_permitted"  // 403
	ErrorCodeTooManyReactions      = "too_many_reactions"      // 409
	ErrorCodeReactionsRequireAuth  = "reactions_require_auth"  // 400
)

// CanonicalReactionKey returns the canonical key for s (§3.2) or an error
// that the caller answers as invalid_reaction.
func CanonicalReactionKey(s string) (string, error)

// ValidateReactionKey reports whether key is already canonical.
func ValidateReactionKey(key string) error

// ReactionCountV1 is one emoji on one message: how many principals the
// caller may know of hold it, and whether the caller is one of them.
type ReactionCountV1 struct {
	// Emoji is the canonical key.
	Emoji string `json:"emoji"`
	// Count is the number of durable records with this key on the message;
	// at least 1 (a key nobody holds is not listed).
	Count int `json:"count"`
	// Me is whether the caller holds this reaction. Omitted when false.
	Me bool `json:"me,omitempty"`
}

// ReactionSummaryV1 is the whole reaction state of one message for one
// reader. Reactions is always a JSON array (empty when none), ordered by the
// time the key first appeared on the message, then by key, so the order a
// client shows is stable while counts change.
type ReactionSummaryV1 struct {
	Schema    string            `json:"schema"`
	ChannelID int64             `json:"channel_id"`
	MessageID int64             `json:"message_id"`
	Reactions []ReactionCountV1 `json:"reactions"`
	// AsOf is the server time the summary was read. A client keeps the
	// latest it has seen for a message (§8.3).
	AsOf Timestamp `json:"as_of"`
}

// Validate: schema name; positive ids; Reactions non-nil; at most
// MaxReactionKindsPerMessage entries; each Emoji canonical, each Count >= 1,
// keys distinct; AsOf non-zero.
func (s ReactionSummaryV1) Validate() error

// ReactionSummariesResponseV1 is the body of the batch read: one summary per
// requested message the caller may see that has at least one reaction,
// ordered by message_id. Always a JSON array.
type ReactionSummariesResponseV1 struct {
	Summaries []ReactionSummaryV1 `json:"summaries"`
}

// ReactionV1 is one record: who reacted with one emoji, and when. Name and
// Kind are the principal's now (a hidden principal shows hidden-<id>, as
// on its messages); GuestReactor is the attribution the reactor had when
// it reacted, stored with the record and never changed (ADR-009 §4.4).
type ReactionV1 struct {
	PrincipalID  int64               `json:"principal_id"`
	Name         string              `json:"name"`
	Kind         PrincipalKind       `json:"kind"`
	Emoji        string              `json:"emoji"`
	CreatedAt    Timestamp           `json:"created_at"`
	GuestReactor *GuestAttributionV1 `json:"guest_reactor,omitempty"`
}

// ListReactorsResponseV1 is one page of the principals holding one emoji on
// one message, ordered by principal_id ascending. NextAfter is the id to pass
// as the next request's after; zero (omitted) means no known further page.
type ListReactorsResponseV1 struct {
	Reactors  []ReactionV1 `json:"reactors"`
	NextAfter int64        `json:"next_after,omitempty"`
}
```

JSON examples (fixture families `reaction-summary-v1`, `reaction-summaries-response-v1`, `reaction-v1`, `list-reactors-response-v1`):

```json
{
  "schema": "conch.reaction_summary.v1",
  "channel_id": 3,
  "message_id": 1042,
  "reactions": [
    {"emoji": "👍", "count": 3, "me": true},
    {"emoji": "❤️", "count": 1},
    {"emoji": "👍🏽", "count": 1}
  ],
  "as_of": "2026-10-28T14:03:11.204Z"
}
```

```json
{
  "schema": "conch.reaction_summary.v1",
  "channel_id": 3,
  "message_id": 1043,
  "reactions": [],
  "as_of": "2026-10-28T14:03:11.204Z"
}
```

```json
{
  "summaries": [
    {"schema": "conch.reaction_summary.v1", "channel_id": 3, "message_id": 1040,
     "reactions": [{"emoji": "🎉", "count": 2}], "as_of": "2026-10-28T14:03:11.204Z"},
    {"schema": "conch.reaction_summary.v1", "channel_id": 3, "message_id": 1042,
     "reactions": [{"emoji": "👍", "count": 3, "me": true}], "as_of": "2026-10-28T14:03:11.204Z"}
  ]
}
```

```json
{
  "reactors": [
    {"principal_id": 7, "name": "nick", "kind": "human", "emoji": "👍",
     "created_at": "2026-10-28T13:58:02.117Z"},
    {"principal_id": 12, "name": "leviathan", "kind": "agent", "emoji": "👍",
     "created_at": "2026-10-28T13:58:40.001Z"},
    {"principal_id": 31, "name": "guest-7h3kq2m9p4r8s6t1", "kind": "human", "emoji": "👍",
     "created_at": "2026-10-28T14:01:09.550Z",
     "guest_reactor": {"handle": "guest-7h3kq2m9p4r8s6t1", "label": "Nick", "tag": "7h3k"}}
  ],
  "next_after": 31
}
```

MCP tool inputs are `mcp.go` mirror structs as today (`mcpPostMessageInput` precedent); outputs are the `pkg/schema` types above, unchanged.

## 6. Permission and visibility model

### 6.1 The two questions, asked in order

Every route and tool asks, in this order, and stops at the first refusal:

1. **May the caller use this channel?** `channelForCaller` (REST) or `agentScope.channelByName` (MCP): the channel exists and the caller is a current member, else 404 `channel_not_found`; for an agent, the manifest grants the capability and permission of the row in §4, else 403 `forbidden`, audited; the agent's budget is spent here (`spendAgentBudget`). Humans spend the reaction bucket of §7.4 here for writes.
2. **May the caller see this message?** In the statement that reads or writes the reaction rows, joined to `messages m` under `messageVisible(scoped=true)` with the caller's `Reader` (`readerFor(r, apiV2)`): the message is in this channel, channel-wide or addressed to the caller, the caller is a member and enabled, and it is after the caller's history bound. Otherwise 404 `message_not_found`, and nothing else is read.

The second question is never asked in Go after the first statement: the summary is `SELECT emoji, COUNT(*), MIN(created_at), SUM(principal_id = ?) FROM message_reactions r JOIN messages m ON m.id = r.message_id WHERE r.message_id = ? AND m.channel_id = ? AND <messageVisible> GROUP BY emoji`; a reader who may not see the message gets no rows and cannot tell an invisible message from an unknown one (the handler asks `rootSQL("1", …)`-style whether the message is visible first only to pick between `message_not_found` and an empty summary, in the same transaction). The reactor list and the batch read are the same join. The batch read's omissions are indistinguishable by construction.

### 6.2 By operation and caller

| Caller | Read summary / reactors / batch | Set | Remove own |
| --- | --- | --- | --- |
| Member (human, non-guest) | Messages they can see | Yes, except as a net monitor on that net's message, or on a removed message | Yes |
| Guest (active visit) | Messages after the membership's `history_after`; nothing before, replies only when the thread's root is after it (the existing bound) | Yes, on those messages; the record carries `guest_reactor` | Yes |
| Agent | Messages they can see, with `messages.read` and `read` in the channel | With `messages.post` and `post` in the channel; the net rule applies; spends the `messages.post` budget | Same as set |
| Operator or owner | No exemption: a member like any other | Same | Same |
| Channel manager | Same as member; no removal of others' reactions in this slice (§11) | Same | Same |
| Non-member (any role) | `channel_not_found` | `channel_not_found` | `channel_not_found` |
| Disabled principal, revoked or expired credential | Refused by authentication before any route | — | — |
| Authentication off | `reactions_require_auth` on every route | same | same |

Why `post` for an agent whatever the message's audience: a reaction is seen by exactly the people who see the message, never more, so it widens nothing an agent's `post_net`/`whisper` grants govern; and an operator who lets an agent speak in a channel has let it react there. An agent that may only read may not react. The alternative, a new capability `reactions.write`, is in §15.

Why the net monitor rule: a monitor "listens but may not transmit" (`ErrNetMonitorOnly`). A reaction on a net message is seen by the net's recipients and is a small transmission; refusing it keeps the monitor's role exactly what ADR-005 says. The monitor already sees the message, so `reaction_not_permitted` discloses nothing. Removing one's own earlier reaction is always allowed: it transmits nothing new.

### 6.3 Events and replay

A live frame for message `m` is sent to a subscriber only if, **at send time**, the subscriber's reader may see `m` by the same statement (§8.1). A frame is never computed once and fanned out. Replay has no reaction form of its own: the reconnect read is the batch route under the same rule (§8.2). Agents' events sockets receive frames under the `messages.read` budget spent at upgrade, as every events frame is.

### 6.4 Reactor identity

A reactor's `principal_id` and current `name` are shown to anyone who can see the message, exactly as a message author's are. A hidden reactor shows `hidden-<id>`; an ended guest shows its handle and `guest_reactor` snapshot. The reactor list never includes anyone whose reaction the caller could not have seen, because reactions exist only on messages, and the caller sees the list only for a message they see. Nothing lists "every reaction by principal X" across messages or channels; there is no per-principal route.

## 7. Storage, transactions and limits

### 7.1 Table (migration 37, the next entry in `store.go`)

```sql
CREATE TABLE message_reactions (
	message_id   INTEGER NOT NULL REFERENCES messages (id),
	emoji        TEXT    NOT NULL,
	principal_id INTEGER NOT NULL REFERENCES principals (id),
	channel_id   INTEGER NOT NULL REFERENCES channels (id),
	created_at   INTEGER NOT NULL,
	guest_handle TEXT,
	guest_label  TEXT,
	PRIMARY KEY (message_id, emoji, principal_id)
) WITHOUT ROWID;
CREATE INDEX message_reactions_by_principal ON message_reactions (message_id, principal_id);
```

- The primary key is the uniqueness rule **(message, principal, emoji)** in the order the reads want it: the summary groups by `emoji` within a message, the reactor list ranges over `(message_id, emoji)` by `principal_id`. The second index serves the per-principal-per-message limit.
- `channel_id` is denormalized and checked against the message in the write transaction, so a channel-scoped statement never needs to join `messages` to know the channel, and the join to `messages` is only for visibility.
- `emoji` is the canonical key (§3.2); the check constraint is in Go (`ValidateReactionKey`), not SQL, since SQLite cannot express it.
- `guest_handle`/`guest_label` are filled when the reactor is in an active guest visit at write time and never updated (the `message_guest_attribution` pattern, inlined because a reaction has no id of its own to key a side table by).
- Rows are never updated. They are deleted only by the owner's own `DELETE` (§4.1). Nothing else removes them (§9, §11).
- Additive: a database from before this migration opens as today; a binary from before it refuses a database that has it (`store.migrate` refuses a newer `user_version`), which is the existing rule for every migration (§12).

### 7.2 Write transaction

`store.SetReaction(ctx, channelID, reader, messageID, key, now)` and `store.RemoveReaction(...)`, both `withImmediateTx`:

1. Re-check the reader is a current member of `channelID` and an enabled principal (`isChannelMemberTx`; the enabled check as `visibleToScopedReader` does). Not → `ErrNotChannelMember` → 404 `channel_not_found` (the same answer as the handler's gate, so a removal racing the gate is indistinguishable from never having been a member).
2. Select the message's `audience_kind`, `net_id`, `thread_root_id` and (once moderation lands) its removal, by `m.id = ? AND m.channel_id = ? AND messageVisible(scoped)` with the reader's arguments. No row → `ErrMessageNotFound` → 404 `message_not_found`.
3. **Set only:** if `audience_kind = 'net'`, read the reader's seat on the net; a `monitor` → `ErrReactionNotPermitted` → 403. A removed message → the same error. (A reply inherits its root's audience; the reply row carries `audience_kind`/`net_id` itself, so no second read.)
4. **Set:** `INSERT … ON CONFLICT (message_id, emoji, principal_id) DO NOTHING`. If a row was inserted, check the limits (§7.3) with two counts; on breach, roll back → `ErrTooManyReactions` → 409. If no row was inserted (already held), nothing further is written.
   **Remove:** `DELETE … WHERE message_id = ? AND emoji = ? AND principal_id = ?`.
5. If a row changed, append the audit event (§11.3) in the transaction.
6. Read the summary for the reader (§6.1 statement) in the same transaction and return it with `AsOf = now`.

The transaction is IMMEDIATE, so two concurrent sets of the same (message, principal, emoji) serialize: the second finds the row and writes nothing; two concurrent sets by different principals both insert; a set racing a membership removal sees either the membership (and the row commits, retained per §9) or its absence (and refuses). Two sessions of one person pressing at once converge to one row and two identical responses.

### 7.3 Limits

| Limit | Value | Where | On breach |
| --- | --- | --- | --- |
| Key size | 64 bytes, 16 code points | input, before lookup | 400 `invalid_reaction` |
| Distinct emoji per message | `MaxReactionKindsPerMessage` = 20 | set, when inserting a key not yet on the message | 409 `too_many_reactions`; joining an existing key is always allowed |
| Reactions per principal per message | `MaxReactionsPerPrincipalPerMessage` = 10 | set | 409 `too_many_reactions` |
| Reactor page | default 50, max 100 | list | 400 `invalid_request` |
| Batch hydration | `MaxReactionBatchMessages` = 100 ids, distinct, positive | batch read | 400 `invalid_request` |
| Summary size | bounded by the kinds limit: ≤ 20 entries, each one key and two integers | — | — |
| Count | exact, no cap: a `COUNT(*)` over the primary-key range of one (message, emoji), bounded by the channel's membership | — | — |

Counts are derived from rows on every read; nothing increments or caches a counter, so a crash, a restart or a restored backup can never leave a count that disagrees with the records.

### 7.4 Rate limits

- **Agents** spend their manifest's `messages.post` budget on `set_reaction`/`remove_reaction` (and on the REST writes with an agent credential) and `messages.read` on the reads, through the existing `agentManifestCheck`/`spendAgentBudget` pair, so an operator bounds an agent's reactions where they bound its posts. Denials are audited as every agent denial is.
- **Humans** get one new keyed token bucket (the `bucketSet` of `invite_limits.go`, keyed by principal id): `reactionWriteLimit` of 60 writes per minute with a burst of 20, checked after the channel gate and before the store. Refusal is 429 `rate_limited` with `Retry-After` and `retry_after_seconds`, and spends nothing. Reads have no new limit beyond the existing socket and request bounds; a summary is one indexed aggregate.
- A refused write (any 4xx) spends nothing of the human bucket; the bucket is taken only when the store is reached, and refunded on a store refusal, as the invite limiter refunds.

## 8. Events, replay and convergence

### 8.1 Live frames

A committed set or remove that changed a row (and only then) calls `events.notifyReactions(channelID, messageID)`, which pushes `eventRef{kind: eventReactions, channelID, id: messageID}` to every subscriber of that channel, human or agent, the actor included (its other clients need it). The subscriber's goroutine, at send time:

1. checks `hasMembership(channelID)` as the message notification does;
2. reads `ReactionSummaryV1` for `messageID` with `readerFor(e.r, apiV2)` (§6.1): no row because invisible → sends nothing;
3. encodes and writes the frame.

References for the same message coalesce in a subscriber's queue (one pending reference per message id), so a burst of ten reactions on one message costs one read when the socket drains, and a slow subscriber is closed by `eventsQueueMax` as today. There is no reaction floor: the write rate limit of §7.4 bounds how fast references can be made.

Nothing is sent on `/v2/ws`, `/v1/ws` or `/v0/ws`: those carry message envelopes, and an old client reading them must see exactly what it sees today. The browser already holds both sockets (`web/src/api.ts`: `/v2/ws` for messages, `/v1/events/ws` for documents). The terminal client opens the events socket for reactions where it wants them live (#767).

### 8.2 Snapshot and replay

There is no reaction replay. On connect or reconnect a client hydrates the messages it displays with one batch read (`GET /v2/channels/{channel}/reactions?messages=…`, up to 100 ids, which is one page of history), and does so again for each further page of history it loads. The events socket sends no reaction frame on subscribe. This is the roster's model turned around: the roster is sent whole on connect because it is one document; reactions are one document per message, so the client names which it wants.

### 8.3 Convergence rules for clients

1. **Frames and write responses are the same document.** A client applies a `ReactionSummaryV1` by replacing its state for that message; it never adds or subtracts counts locally. Optimistic display of one's own press is allowed only until the response or the frame arrives, whichever first, and is then replaced.
2. **Newest wins by `as_of`.** Two documents for one message may arrive out of order (a frame computed before a write response, or two sockets). The client keeps the one with the later `as_of`; on equal `as_of` the later arrival. A stale document can therefore show a state at most milliseconds old, and the next change or hydration corrects it.
3. **Missing a frame loses nothing.** Every document is whole state; a missed frame is corrected by the next one for that message or by hydration.
4. **Reconnect re-hydrates.** After a socket closes (slow consumer, credential re-check, server restart) the client re-hydrates what it shows (§8.2). Counts shown before and after are both derived from rows, so they agree except for changes in between.
5. **A frame never arrives for a message the client cannot see,** so a client never has to decide whether to show it.

### 8.4 Reordering, stale events and duplicates

The server never sends an older state after a newer one on the same socket for the same message, because each frame is read at send time and one goroutine sends in order. Across sockets (two tabs) rule 2 above decides. A duplicate frame (two changes, two reads, same resulting state) is harmless by rule 1.

## 9. Guest lifecycle

Consistent with guests-and-accounts.md §1, §4.2–§4.4 and the shipped #705/#711/#719 code:

| Moment | Effect on reactions |
| --- | --- |
| Admission to a channel with `history_after` | The guest may read and set reactions only on messages after the bound (and replies whose root is after it): the bound is in `messageVisible`, which every reaction statement embeds. Reactions on earlier messages are invisible to it: not in summaries, not in the batch read (absent, indistinguishable), not in frames, and a set on one answers `message_not_found`. |
| Setting a reaction during the visit | The row carries `guest_handle`/`guest_label` from the active `guest_visits` row, read in the transaction. Everyone who sees the message sees `guest_reactor` with the handle, label and tag of §4.1, rendered as `Guest: <label> · <tag>` from the stored flag, never from the name's text. |
| Idle or hard expiry, End visit, operator end, revocation, disable | `endGuestVisitTx` removes memberships and the credential as today. **Reaction rows are kept**, as the visit's messages are (§4.4 "Posted messages … remain"): historic attribution. The former guest cannot act: its credential is dead, it is no member, and a later request with its token fails closed. Its events sockets are dropped by the existing hooks, so it receives no further frame. Other readers keep seeing the count and the `guest_reactor` attribution. |
| A later visit from the same browser or invite | A new principal with a new handle and fresh bounds. It cannot see, remove or "reclaim" the old visit's reactions: the rows name the old principal id, the new one has no row and (bound) likely cannot see the message. Nothing maps the two. |
| Conversion to an account (`ConvertGuest`) | The principal id, memberships and `history_after` are kept, so every reaction row already belongs to the member: **no duplicate, no reassignment, no migration**. The rows written during the visit keep `guest_handle`/`guest_label`, so they render as the guest's (the message rule, §4.4 "earlier guest posts remain identifiable"); rows written after conversion carry none and render under the permanent name. Visibility is unchanged: the bound stays, so conversion widens no history and reveals no pre-visit reactions. A later, audited widening of a member's history (not in scope, §4.3) would widen reactions with the messages, through the same condition. |
| Hiding a converted member | The name renders `hidden-<id>` on post-conversion reactions; visit-time reactions keep the stored guest attribution (the message rule). Hide refuses an active guest as today. |
| Cleanup of a guest "that posted nothing" (§4.4) | A reaction counts as having posted: a visit with any reaction row keeps its attribution record. Recorded in §17 as an interpretation. |

Guests may react because §4.3 lets a guest "read/post in assigned conversation channels", and a reaction is a lesser post. Guests cannot react where they cannot post (a channel they are not admitted to), and the approval surface is untouched: an approval is not a message, and reacting to a message that announces one changes no decision.

## 10. Races and revocation

| Race | Outcome |
| --- | --- |
| Two sets of the same (message, principal, emoji) from two sessions | IMMEDIATE transactions serialize; one inserts, one no-ops; both answer the same summary; one frame (coalesced) or two identical ones. |
| Set and remove of the same reaction interleaved | Each transaction sees the rows as of its start; the final state is the last committed write; both responses are true at their `as_of`; clients converge by §8.3. |
| Set racing membership removal | Before removal commits: the row commits and is retained (§11.1); the removed principal's sockets are dropped, so it receives no frame; others do. After: `ErrNotChannelMember` → `channel_not_found`, same as a stranger. |
| Set racing credential revocation or disable | The request either authenticated before revocation (and then the transaction's enabled check may still refuse it, since disable is also a principal state) or is refused at the door. A committed row is retained. |
| Set racing guest expiry | `CredentialLive` refuses at the door once expired; the transaction's membership check refuses if `endGuestVisitTx` committed first. |
| Read racing visibility loss (removed from channel or net, message removed) | The next read or frame computes with the new state; a frame already in flight shows at most the previous state. Dropped sockets receive nothing further. |
| Frame computed before a later write's response | Client rule §8.3(2). |
| Set racing message removal (moderation, future) | Whichever commits second sees the other: a set after removal is `reaction_not_permitted`; a removal after a set leaves the row and hides it (§11.2). |
| Restart between commit and frame | The frame is lost; the row is durable; hydration on reconnect shows it. |
| Restore of a backup | Rows restored are as consistent as the messages they attach to (foreign keys); counts are recomputed from rows, so no counter drifts. The restore procedure's credential invalidation (§4.2 of the guest design) is unchanged. |

## 11. Deleted messages, moderation, audit and notifications

### 11.1 Reactors who lose access

Membership removal, net removal, ban (moderation §6), disable, hide, guest end: **reaction rows stay**, as the principal's messages stay. The reactor cannot change them while it has no access; a member re-added later can remove its own again. The alternative (deleting a principal's reactions when it leaves) was rejected: it would change counts other people have already seen, retroactively, for a reason unrelated to the message, and it would treat a reaction unlike a message, which the guest design forbids for attribution.

### 11.2 Removed messages (moderation.md §7, not yet implemented)

Written so that #765 needs nothing from the moderation PR and the moderation PR needs one line from #765:

- A removed message's summary is **empty** and it is **absent** from the batch read; its reactor lists are empty. The rows are kept (nothing deletes them) but every reaction statement adds `AND NOT EXISTS (SELECT 1 FROM message_removals x WHERE x.message_id = m.id)` once that table exists. Until then the clause is absent and the behaviour is identical because no message is removed.
- A set on a removed message is 403 `reaction_not_permitted` (the tombstone is visible, so a 404 would be a lie and a disclosure); a remove is 200 with the empty summary.
- The removal's commit calls `notifyReactions(channel, message)` so open clients drop the summary with the tombstone frame; a reconnecting client's hydration simply omits it.

### 11.3 Audit

Two actions, written in the write transaction **only when a row changed** (the moderation precedent for idempotent repeats): `reaction_set` and `reaction_removed`, actor `principal:<id>`, subject `message:<id>`, detail `channel=<id> emoji=<key>`. The key is content in the loosest sense; the message body is never in the detail. No-ops, reads and refusals write nothing beyond the existing agent-denial rows. Reactions do not appear in `GET /v1/audit` filters in any new way. Volume: bounded by the human rate limit and agent budgets; at 60 per minute per principal it is comparable to posting.

### 11.4 Notifications and read state

A reaction **does not**: move or create a read mark; change an unread count (`notifyReadState` is not called); send the author a frame of a kind that the browser or terminal treats as a new message; call the configured `Broadcaster` seam or ntfy; create an approval, a run or an intervention; or reach any agent through anything but the events socket it already holds and the reads it already makes. An agent that wants to react to reactions must poll or subscribe explicitly; no server behaviour starts it. Notifying an author of reactions on its message is a later, additive design.

### 11.5 Moderation of reactions

Not in this slice: no route removes another principal's reaction. The levers are the existing ones: remove the member or ban them (their past reactions stay, §11.1), or remove the message (its reactions vanish from every read, §11.2). If a per-reaction moderator removal is wanted later it is one additive route under `accessChannelManager` with the moderation design's target rules, and a new audit action.

## 12. Compatibility, migration and rollback

- **Schema:** new types, constants and error codes only (§5). No published type or fixture changes; `scripts/schema-compat.sh` passes. Not a version bump. `MessageV2` is untouched.
- **Old clients:** a browser or terminal that predates reactions sees no reactions and loses nothing: message routes, message sockets and their frames are unchanged. The events socket sends a new `schema` value; the browser dispatches events frames by `schema` and must **ignore an unknown one** (#766 confirms with a test against the shipped client; `web/src/state/feed.ts` already discriminates by schema). The TUI's events handling, where it has any, is held to the same rule (#767).
- **Old servers:** a client that sends the new routes to an older `conchd` gets 404 from the mux (no route) or an MCP "unknown tool"; clients feature-detect by that answer and hide the controls. No whoami flag is added.
- **Migration 37:** one new table and one index; nothing backfilled, nothing rewritten; a pre-reaction database opens and runs on the new binary at once.
- **Rollback:** the existing rule for every migration applies: a binary from before 37 refuses to open a database at `user_version` 37 ("newer than this binary"). Rolling the binary back therefore means restoring the pre-upgrade backup (Litestream or the operator's copy), as for any migration; nothing in this design makes that worse or better, and the release notes of the version that ships 37 say so, as they do for every migration. Turning the feature off without rolling back is not supported in this slice: there is no runtime switch (a `server_settings` row `reactions_enabled` would be an additive follow-up if an operator asks).
- **Emoji version upgrade:** regenerate from a newer `emoji-test.txt`; stored keys are never re-validated, and any key accepted before is accepted after (Unicode does not retire sequences; if it ever marked one non-emoji the generator would keep it in a `retired` set so stored and replayed keys still validate).
- **Approval path:** untouched. No file under the approval request → notify → resolve → audit chain changes; reactions do not read or write `approvals*`, do not notify, and are not labelled `approval-path`. A security-reviewer pass is still required (new authenticated write surface, new visibility statements).

## 13. Implementation checklist

### #765 — server: store, REST/WS, MCP (size L; security review)

- [ ] Emoji catalog: vendored `emoji-test.txt` + licence (or reuse #763's), pinned hash, generator writing `pkg/schema/emoji_catalog_gen.go` (sequences by stripped form → fully-qualified key, statuses, `EmojiVersion`) and, if #763 has not, the web JSON; `make check` step that fails on a stale output or a hash mismatch.
- [ ] `pkg/schema/reaction.go`: types, constants, error codes, `CanonicalReactionKey`, `ValidateReactionKey`, `Validate` methods; `ts.go` entries; fixtures (`reaction-summary-v1.json`, `-empty.json`, `-guest.json` via `list-reactors-response-v1-guest.json`, `reaction-summaries-response-v1.json`, `reaction-v1.json`, `list-reactors-response-v1.json`); `go generate ./pkg/schema`; `scripts/schema-compat.sh` green; the `schema-change` skill followed (compatible: no sign-off step beyond the listing in §16).
- [ ] Migration 37 (§7.1) with a migration test from version 36.
- [ ] Store, in `audience.go` (or a `reactions.go` that reaches `messages` only through helpers exported from `audience.go`, so `TestMessageReadsStayInTheVisibilityFunction` keeps holding): `SetReaction`, `RemoveReaction`, `ReactionSummary`, `ReactionSummaries` (batch, one statement), `ListReactors`; `ErrMessageNotFound`, `ErrReactionNotPermitted`, `ErrTooManyReactions`; audit rows; guest attribution read in the transaction; the `message_removals` clause left as a documented hook.
- [ ] Routes (§4.1) in `routes.go` with `TestRouteTable` and `TestRecentAuthClass` decisions; handlers in `reactions.go`; `readerFor(r, apiV2)`; human bucket `reactionWriteLimit`; auth-off refusal.
- [ ] Events: `eventReactions` kind, `notifyReactions`, per-message coalescing, send-time read with the subscriber's reader (§8.1); no change to `/v2/ws`.
- [ ] MCP: four tools (§4.2), tool→capability map entries, descriptions, `docs/design/mcp-endpoint.md`; REST and MCP share the store functions and the error mapping.
- [ ] Isolation: matrix rows for all five routes (A's message in A's channel → `channel_not_found`; A's message id through B's channel → `message_not_found`; ghost ids identical); guest rows for the bound (pre-bound message → `message_not_found` / absent); conversion rows (same answers after conversion); agent rows (`forbidden` without `post`); revoke rows (no frame after drop).
- [ ] Docs: API reference section, operator note on limits and audit actions, CHANGELOG line; `security-reviewer` pass before merge.

### #766 — browser (size M)

- [ ] Reaction bar under each message: counts in summary order, `me` highlighted, press to set/remove; "add" opens #763's picker (shared catalog, same `EMOJI_VERSION`); keyboard and screen-reader labels; narrow widths.
- [ ] Hydration by the batch route for each page of history; events-socket `conch.reaction_summary.v1` handling by §8.3; unknown-schema frames ignored (test); re-hydrate on reconnect.
- [ ] Reactor list on hover/focus/long-press via the reactor route, paginated; guest rendering from `guest_reactor`, never from the name.
- [ ] Error handling: `invalid_reaction`, `too_many_reactions`, `rate_limited` with `retry_after_seconds`, `reaction_not_permitted` (control hidden for monitors after the first refusal is acceptable; the server is the enforcement).
- [ ] Feature detection against an older server (404 → no controls). Unit tests with fixture-checked generated types; Playwright for press/unpress, two-tab convergence, reconnect.

### #767 — terminal (size M)

- [ ] Read-only first: counts under messages in the TUI view, from the batch route per page; glyph width via the existing width handling; unsupported-glyph fallback (the key's CLDR short name from the Go table, or the code points) documented.
- [ ] Set/remove: `conch react <channel> <message-id> <emoji-or-shortcode>` and `conch unreact …`; TUI key binding on the selected message; shortcode entry resolved client-side (a shortcode table is a dataset decision, §16); `conch reactions <channel> <message-id>` lists reactors.
- [ ] Live updates through the events socket where the TUI already has one, by §8.3; otherwise refresh on the next page read; unknown schema frames ignored.
- [ ] Same error handling and feature detection as the browser; table-driven tests with the golden fixtures.

## 14. Test matrix

Normative for #765 unless a row names #766/#767. "Isolation" means a row in `e2e/isolation` with the three checks (stated refusal, no marker of A, ghost-identical).

| # | Scenario | Expected | Where |
| --- | --- | --- | --- |
| K1 | Every example of §3.2 | Canonical key or `invalid_reaction` exactly as tabled; `ValidateReactionKey(canonical(x)) == nil` for every accepted input; idempotent `canonical(canonical(x)) == canonical(x)` | `pkg/schema` table test |
| K2 | Inputs at and over 64 bytes / 16 code points; NUL, controls, bidi, whitespace, `:+1:` | `invalid_reaction` before any table lookup | schema |
| K3 | Generated catalog vs vendored file hash; `EmojiVersion` vs web `EMOJI_VERSION` | `make check` fails on drift | scripts |
| W1 | Set, set again, remove, remove again (same principal) | 200 each; one row after the first set, none after the first remove; two audit rows total; summary `me` true/true/false/false | store + handler |
| W2 | 50 concurrent sets of one (message, principal, emoji) | one row; every response identical; no error | store, `-race` |
| W3 | Two sessions (two credentials) of one human set and remove interleaved | final rows match last commit; each response's `as_of` ordering is consistent with its state | handler |
| W4 | 21st distinct emoji on a message; 11th reaction by one principal on a message; joining an existing key at the kinds limit | 409, 409, 200 | store |
| W5 | Remove by someone who never reacted | 200, summary unchanged, no audit row | store |
| W6 | Human exceeds 60/min | 429 with `Retry-After` and `retry_after_seconds`; refused writes spend nothing | handler |
| W7 | Restart between set and read; count after restart | counts equal rows; `COUNT(*)` matches summary for every emoji | store/restart test |
| V1 | Non-member (member of B) on A's message in A's channel: all five routes | 404 `channel_not_found`; ghost identical | isolation |
| V2 | Member of B names A's message id through B's channel: summary, reactors, set, remove | 404 `message_not_found`; batch read omits; ghost identical | isolation |
| V3 | Whisper: recipient vs non-recipient member of the same channel; set by recipient | recipient sees summary/frame; non-recipient `message_not_found`, no frame, batch omits | isolation + events test |
| V4 | Net message: member, monitor, non-net member | member sets; monitor reads but set → 403 `reaction_not_permitted`, remove-own → 200; non-net member `message_not_found` | isolation |
| V5 | Reply in a thread whose root is a whisper | reactions follow the root's audience | store |
| V6 | Operator who is not a member; operator member on an invisible whisper | `channel_not_found`; `message_not_found` | isolation |
| V7 | Agent without `post`, with `read` only: set | 403 `forbidden`, audited; reads succeed; budget spent per manifest | agent rows |
| V8 | Agent without membership | `channel_not_found` | agent rows |
| V9 | Auth off | every route 400 `reactions_require_auth` | handler |
| G1 | Guest: message before `history_after`, reply to an old root, message after | `message_not_found` / absent for the first two; success for the third | guest rows |
| G2 | Guest set carries `guest_reactor`; member sees it; render rule | fixture `list-reactors-response-v1-guest.json`; UI renders `Guest: <label> · <tag>` (#766/#767) | store, web, TUI |
| G3 | Guest ends (idle, hard, End visit, operator); then set/remove/read with the old token; new visit from same browser | all refused at the door; rows retained with attribution; new principal sees nothing of them (bound) and has no `me` | expiry + revoke rows |
| G4 | Conversion with reactions in place; then set, remove, read; hide the member later | same rows, same `me`, no duplicates, bound unchanged, old rows keep guest attribution, new rows under the permanent name; hidden → `hidden-<id>` on new rows only | conversion rows |
| G5 | Guest attempts a 404'd pre-bound reaction via MCP | guests have no MCP (agents only); documented, no test needed beyond `mcp_auth` | — |
| R1 | Set racing membership removal (hook before commit, after commit) | row retained or `channel_not_found`; removed principal's socket receives nothing after the drop | store race test + events |
| R2 | Set racing credential revocation / disable / guest expiry | refused at door or by the transaction's enabled/membership check; no partial state | store |
| E1 | Events: subscriber who can see the message, who cannot, agent subscriber, the actor's other socket | frame / nothing / frame / frame; each computed with that reader | events test |
| E2 | Ten sets on one message faster than the socket drains | one or few frames, final one whole and correct | events test |
| E3 | Slow consumer | closed at `eventsQueueMax`; re-hydration restores state | events test |
| E4 | No frame on `/v2/ws`, `/v1/ws`, `/v0/ws`; no `Broadcaster` call; no read-state change; no notification | asserted with recording seams | handler tests |
| E5 | Out-of-order delivery (older `as_of` after newer) | client keeps newer (#766/#767 unit tests) | web, TUI |
| E6 | Reconnect after server restart | client re-hydrates; displayed counts equal server rows | Playwright (#766), TUI integration (#767) |
| C1 | Published fixtures byte-identical; new fixtures validate; TS generated and checked | `schema-compat.sh`, `schema-ts-check.sh` green | CI |
| C2 | Old browser build against new server events socket | unknown-schema frame ignored, no error (#766 test with the frame injected) | web |
| C3 | New client against pre-37 server | 404 → controls hidden; nothing else breaks | web, TUI |
| C4 | Migration 36 → 37 on a populated database; 37 opened by a 36 binary | opens and serves; refused with the existing "newer than this binary" error | store |
| C5 | Catalog upgraded to a newer version with rows stored under the old | every stored key still served and still canonical | schema + store |
| M1 | (when moderation lands) removed message: summary empty, batch omits, set → 403, remove → 200, frame on removal | as §11.2 | store + events |
| A1 | Audit rows for W1; none for W5, reads, refusals | as §11.3; detail has no body | store |
| P1 | REST and MCP answer the same inputs alike for every error row above | parity table over both transports | handler + MCP tests |

## 15. Choices made

Each is a call the existing designs did not settle; the alternative is recorded so review can overrule cheaply.

1. **Canonical key = fully-qualified sequence; VS15/VS16 are equivalence.** Alternative: store the selector-stripped form. Fully-qualified renders as emoji everywhere without client fix-ups and is what the picker inserts; stripping only on lookup gives the equivalence the issue asks for. VS15 (text presentation) folded in because a reaction is a pictograph, not a glyph choice.
2. **Emoji in the path, no request body.** `PUT`/`DELETE` by resource are naturally idempotent, `DELETE` bodies are poorly supported, and the server canonicalizes whatever form arrives. Alternative: `POST …/reactions {emoji}` + `DELETE …/reactions/{emoji}`, two shapes for one thing.
3. **Write answers return the summary (200), not 201/204.** Convergence (§8.3) wants one document shape; the read-state `PUT` is the precedent.
4. **Batch route, not a `reactions` field on `MessageV2`.** Keeps a published envelope and every socket frame untouched; one aggregate statement per page.
5. **Summary carries counts and `me`, not reactor ids.** Keeps frames small and bounded; the reactor list is one further read on demand. Alternative: up to N ids per entry.
6. **Reactor list ordered by `principal_id`, cursor `after`.** One stable integer cursor, the convention of every list here; a client may sort a page by `created_at` for display.
7. **Agents react under `messages.post` + `post`, not a new capability.** A reaction is strictly less than a post and is seen by no one who does not see the message; one grant to understand; the post budget bounds reaction spam. Alternative: `reactions.write` as an additive `Capability` value (not breaking, but a new manifest vocabulary every operator must learn and grant).
8. **Net monitors may not set reactions; may remove their own.** Transmission rule of ADR-005 applied literally.
9. **Reaction rows outlive access (leave, ban, disable, hide, guest end, conversion).** Mirrors messages and the guest attribution rule; counts others saw do not change retroactively.
10. **Reactions are audited on change, not on no-op.** Visible to others, so an action; the moderation precedent for idempotent repeats; volume bounded by rate limits. Alternative: no audit, as read marks (which nobody else sees).
11. **Human rate limit 60/min, burst 20, per principal; agents spend `messages.post`.** Enough for a person, far below what bloats audit or sockets. Numbers are defaults for review.
12. **Limits 20 kinds per message, 10 per principal per message.** Keeps a summary frame under about 1 KB and a bar renderable in a terminal; the first number is the more product-visible one.
13. **No live replay of reactions; hydrate by batch on (re)connect.** Replay of per-message state would need a cursor the message sockets do not have; the batch route is one request per page and is the same code as the first load.
14. **No notifications, no read-state effect, no agent trigger.** The issue's requirement, taken as a hard rule; author notification is a later additive design.
15. **No moderator removal of a single reaction in this slice.** Message removal and membership tools cover abuse; a per-reaction route would need the moderation design's target rules, which are not yet accepted.
16. **`message_not_found` shared with moderation.md.** One code, defined once, same meaning (unknown, other channel, invisible).
17. **Auth-off refuses every reaction route.** A reaction needs a principal for uniqueness; reads could be served channel-wide, but one rule is simpler and matches read state.
18. **Rollback = backup restore, as for every migration.** No runtime switch added; listed as a possible additive follow-up.

## 16. Decisions needing Nick

None of the wire changes is a schema version bump, and nothing here touches the approval path. Listed for the record (ADR-000 D8, the `schema-change` skill):

1. **Compatible new schema** (normally fine without him): types `ReactionSummaryV1`, `ReactionCountV1`, `ReactionSummariesResponseV1`, `ReactionV1`, `ListReactorsResponseV1`; constants `EmojiVersion` and the limits; error codes `message_not_found`, `invalid_reaction`, `reaction_not_permitted`, `too_many_reactions`, `reactions_require_auth`; events-socket frame `conch.reaction_summary.v1`; four MCP tools mapped to existing capabilities. No published fixture changes. **Not a version bump.**
2. **Vendored Unicode data and its licence.** `emoji-test.txt` (Unicode-3.0 licence) checked into the repository and generated into Go and JSON. Not a `go.mod` or npm dependency, but a new licence in the tree; one sign-off covers #763 and #765 (if #763 has already recorded it, nothing more is needed).
3. **Product call: reactions outlive access** (§9, §11.1), including a guest's after End visit and a removed member's. Recommended: retain, as messages are retained. The alternative is deleting on leave, which the design argues against.
4. **Product call: agents react under `post`** rather than a dedicated capability (§15.7). Recommended as written; a dedicated capability is an additive vocabulary change if preferred.
5. **If #767 wants shortcode entry in the terminal**, the shortcode table is a dataset with its own licence (for example gemoji's, MIT), which would be a new vendored data file. Not needed for #765 or #766. Flag only.
6. **Approval path:** no change claimed; #765 is labelled `security` (new authenticated write surface and visibility statements) and gets a `security-reviewer` pass, but is not `approval-path`. If review finds otherwise, Nick merges.

## 17. Tensions in the guest and account design, recorded

No contradiction was found that this design had to resolve silently. Two places needed an interpretation, recorded here rather than decided in code:

1. **"Ending a visit … invalidates cursors/subscriptions" (§4.3) versus "Posted messages … remain" (§4.4).** Per-principal state is split by kind: content others see (messages, now reactions) stays; private cursors (read marks, deleted by #719) go. A reaction is treated as content. If the owner reads §4.3 as "everything the guest wrote that is not a message goes", §9 changes to delete rows at End visit; nothing else in this design moves.
2. **"A guest that posted nothing can have transient profile data cleaned promptly" (§4.4).** A guest with a reaction but no message has "posted" for this rule; its attribution record is kept. The alternative reading (only messages count) would delete the reaction's `guest_handle`/`guest_label` and render it as a bare principal id, which contradicts the immutable-attribution rule for the only thing of the guest's that other people can still see.

A third observation is not about reactions but worth a note for the guest design's owner: `ChannelMemberV1.HistoryAfter` documents "absent, or zero, is no bound", while `withinHistoryBound` makes a guest with a `NULL` bound see nothing and one with bound `0` see everything after id 0, that is, everything. The two agree in effect (0 is a bound at zero; `NULL` is "no provable bound, refuse"), but the wire comment could say so explicitly so a reader does not take `0` and absent to mean the same as `NULL` in the store.
