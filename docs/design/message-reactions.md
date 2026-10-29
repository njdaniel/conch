# Design: Unicode message reactions

- **Status:** Proposed for principal and security review, issue [#764](https://github.com/njdaniel/conch/issues/764) (child of epic #752). No implementation is authorized by this note. The wire shapes in §5 are compatible additions; §16 lists what still needs Nick.
- **Builds on:** [ADR-001](../adr/ADR-001-mcp-native-architecture.md) (MCP tools map onto the REST/WS API; parity rule D6), [ADR-005](../adr/ADR-005-nets-and-whispers.md) and `store/audience.go` (one visibility condition), [ADR-009](../adr/ADR-009-guests-and-open-joining.md) and [guests-and-accounts.md](guests-and-accounts.md) (guest identity, history bound, attribution), [ADR-010](../adr/ADR-010-delegated-administration.md), [moderation.md](moderation.md) (message removal, proposed; §11 here is written to compose with it), [mcp-endpoint.md](mcp-endpoint.md).
- **Current-code baseline:** origin/main `407af84` (v0.1.0-alpha.5), inspected 2026-10-28 in a worktree at `89b82c5` (= `407af84` + #940's `desktop/` skeleton, no server, schema or web change). Paths and behaviour below were checked there. A search of `pkg/schema`, `internal/server`, `internal/cli` and `web/src` found **no reaction API, schema, table or client control**, and no emoji dataset yet: #763 (the browser picker) is open and not merged at this commit.
- **Implements:** this document is the contract for a catalog-and-schema slice (#953, §13 item 0: the vendored dataset, generator, `pkg/schema/reaction.go`, fixtures and TypeScript, which #763 and #765 both depend on), then #765 (server: store, REST/WS, MCP; security review), #766 (browser) and #767 (terminal). It does not move reactions into the alpha exit gate.
- **Coordination:** #699 (usability audit) owns how reactions read in the clients; this document does not duplicate it and #766/#767 take their presentation findings from there.
- **Review record:** §18. Round 1 (PR #952) adopted every security should-fix and nit and every general-review should-fix; what still gates #765 is listed there.

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

- **Visibility is the message's.** Every read, count, reactor identity, write, live frame and replay is gated by the one visibility condition in `store/audience.go` (`messageVisible`: the audience rule and the history bound for every reader; current membership and an enabled principal only for a *scoped* message, `audience.go:235-242`), **plus an explicit "the reader is, now, a member of `m.channel_id` and an enabled principal" clause** in the form `runVisibleHead` uses (`audience.go:1145-1151`), both evaluated for the caller in the statement that selects the rows. A message the caller cannot see and a message that does not exist get the same answer, 404 `message_not_found`, byte for byte once the id is set aside.
- **Writing needs participation.** A write additionally requires a current *member* seat on the net the message was said on (a monitor, or a principal taken off the net since the post, is refused, `audience.go:740-746`), and for an agent `messages.post` with `post` at the channel gate and then the permission it would need to reply in the message's audience: `post` channel-wide, `post_net` on a net, `whisper` in a whisper (the reply precedent, `messages.go:683-703`, `authz.go:603-606`). Operators get no exemption on either side.
- **Identity is the caller's.** No request names a principal; the store binds the credential's principal. Agents act as themselves under their manifest, and spend their `messages.post` budget. Nothing a reaction does triggers an agent, moves a read mark, or sends a notification.
- **One emoji key.** The key is the fully-qualified Unicode emoji sequence from the pinned `emoji-test.txt` that #763's picker uses; presentation selectors are equivalence, skin tones and ZWJ sequences are distinct; arbitrary text and concatenated emoji are refused (§3).
- **Records outlive access, and moderators can remove them.** Like messages, reactions stay when the reactor leaves a channel, is disabled, is hidden, ends a guest visit or converts to an account; they are never reassigned, and an ended identity cannot act (§9). A channel manager or operator who can see the message can remove one reaction or all of one principal's reactions on it, audited (§11.5), so a retained row cannot be used to grief a message.
- **Convergence is by whole state.** Frames and responses carry the complete summary of one message, never a delta; a client that misses one loses nothing it cannot recover from the next, and re-hydrates by the batch route on reconnect (§8).

## 2. What exists now

| Area | Where | What it gives reactions |
| --- | --- | --- |
| The one visibility function | `internal/server/store/audience.go` (`messageVisible`, `visibilityArgs`, `withinHistoryBound`, `noHistoryBound`) | Reactions embed `messageVisible` plus the explicit membership-and-enabled clause (§6.1) in their own statements. That file is the only one that may name `messages` or `message_recipients` in SQL: `TestOnlyAudienceGoReadsMessageRows` (`store/audience_test.go:493`) is a **textual** check over every other `.go` file of the package, so every reaction statement that joins `messages` lives in `audience.go`, as helpers that own the join; a `reactions.go` may hold only the statements over `message_reactions` alone. |
| Per-principal per-channel state with a socket document | `store/read_state.go`, `server/read_state.go`, `schema/read_state.go` | The nearest precedent: `ON CONFLICT` upsert, a document the events socket re-sends, refusal under auth-off (`read_state_require_auth`), human-only (reactions are not human-only). |
| Membership and agent gates | `members.go` `channelForCaller(w, r, name, capability, permission)`; `authz.go` `agentManifestCheck`, `spendAgentBudget`, `agentScope` (MCP) | The two gates every reaction route starts with: non-member → `channel_not_found`; agent refused by manifest → `forbidden`, audited. |
| Events socket | `events_ws.go`: `eventRef`, `eventsHub.notify*`, per-subscriber goroutine turns a reference into a document **with the subscriber's reader at send time**; `eventsQueueMax`, floors | The transport for live reaction frames. A reference carries no content; the document is read through the store with the socket's reader. Agents may hold an events socket (`subscribeKind(..., human=false)`). |
| Message socket | `ws.go` `/v2/ws` with `?after=` replay and `historyGate`; `hub.Hub` | Carries `MessageV2` frames and heartbeat frames. Reactions do not travel on it (§8). |
| MCP | `mcp.go` (`addAgentTool`, `mcpToolError`, tools `post_message`, `read_channel`, …), `mcp_auth.go` (agents only), the tool→capability map `mcpToolCapabilities` in `pkg/schema/agent_manifest.go` | Four new tools are plain mirrors of the REST routes; no administration tool (the moderation precedent). |
| Agent audience permissions | `messages.go:683-703` (`postReply`: `agentCallerAllowed` with `post` for a channel-wide root, `authorizeScopedPost` otherwise), `authz.go:603-606` (`authorizeScopedPostGates`: `post_net` for a net, `whisper` for a whisper; refusals audited with `denyAudienceGrant`) | The rule an agent's reaction follows (§6.2). |
| Guests | `store/guests.go` (`endGuestVisitTx` removes memberships, read marks, credential; `ConvertGuest` keeps the principal id, memberships and `history_after`), `message_guest_attribution`, `schema.GuestAttributionV1` | The lifecycle §9 follows; the attribution snapshot pattern reused for reactors. |
| Hiding, disabling | `store/hide.go` (rename to `hidden-<id>`, memberships removed, messages stay), `store/disable.go` | What happens to a reactor's rows (§11). |
| Moderation (proposed) | `docs/design/moderation.md` §7, §10: `DELETE /v2/channels/{channel}/messages/{message_id}`, `MessageV2.removed`, error `message_not_found`, table `message_removals` | §11 defines reactions on a removed message; the `message_not_found` code is defined once, by whichever PR lands first. |
| Isolation harness | `e2e/isolation/matrix.go` (every route needs a row: refusal code, no marker of A, same answer as a ghost), `guestrows.go` (history-bound rows), `routes.go` coverage check | Every new route gets rows (§14). |
| Rate limits | `ratelimit.go` (agents, per manifest capability), `invite_limits.go` `bucketSet`/`tokenBucket` (humans, keyed) | Agents spend `messages.post`; humans get a keyed bucket (§7.4). |
| Migrations | `store/store.go` `migrations` (last entry 36 at this commit; the reaction table is the next entry at merge time, since moderation may take 37); a binary refuses a database whose `user_version` is newer | §12. |
| Schema tooling | `pkg/schema/ts.go`, `vocabularies.go`, `go generate ./pkg/schema`, `scripts/schema-compat.sh`, `scripts/schema-ts-check.sh`; golden fixtures in `pkg/schema/testdata/` (303 files) | §5 types are listed in `TSTypes`; fixtures added; no published fixture changes. `ts.go` emits types and vocabularies, **no scalar constants today**: emitting `EmojiVersion` and the bounds is new work (§3.1, §13). |
| Emoji data | none at this commit; #763 will add a pinned `emoji-test.txt`, a generating script with a checked hash, and a bundled catalog for the picker | §3.1 fixes how reactions share it. |

## 3. The reaction key

### 3.1 Dataset and version

One source: the Unicode Consortium's `emoji-test.txt` for one pinned Unicode Emoji version (Unicode-3.0 licence), vendored in the repository as #763 lands it (proposed path `third_party/unicode/emoji-test.txt` with its licence beside it; #763 decides the path, this document binds the contract, not the path). One pinned SHA-256 of that file, in the generating script. Two generated outputs from the same run of the same script:

- `pkg/schema/emoji_catalog_gen.go`: the Go table the server validates and canonicalizes keys with (§3.2), and the constant `schema.EmojiVersion` (for example `"16.0"`), taken from the file's `# Version:` header.
- the web catalog (JSON) #763's picker searches, carrying the same version string.

`make check` fails when either output is stale against the vendored file or the vendored file's hash differs from the pin (the `schema-ts-check.sh` pattern). The generator also **fails** if two fully-qualified sequences collide once their selectors are stripped (§3.2 step 2): the lookup table would then be ambiguous, and a new Unicode version must not be adopted silently with such a pair. The TypeScript constant `EMOJI_VERSION` is generated from `schema.EmojiVersion` through `go generate ./pkg/schema`, so the browser cannot ship a catalog of another version than the server validates against without `scripts/schema-ts-check.sh` noticing. **This is new tooling:** `ts.go` emits types and vocabularies today and no scalar constants, so the catalog-and-schema slice extends it to emit `EmojiVersion`, `MaxReactionKeyBytes`, `MaxReactionKeyCodePoints`, `MaxReactionBatchMessages`, `DefaultReactorLimit` and `MaxReactorLimit`, covered by `scripts/schema-ts-check.sh` like the types. The terminal client (#767) uses the Go table directly: it is in `pkg/schema`, which `conch` already imports.

The vendored file, the generator, both outputs, `pkg/schema/reaction.go`, its fixtures and the generated TypeScript form **one slice, filed as its own issue** (§13 item 0), which #763 and #765 both depend on; neither of them creates the dataset itself. It adds no npm or `go.mod` dependency for the data. The vendored data file's licence is Nick's to accept once (§16).

Upgrading the pinned version is a **compatible** change: it widens the set of accepted keys. Stored keys are never validated against the catalog on read, so a server downgraded to an older catalog still serves every stored reaction (§12).

### 3.2 Canonical key

Definitions, with `emoji-test.txt` statuses: `fully-qualified`, `minimally-qualified`, `unqualified`, `component`.

1. **The canonical key is the fully-qualified sequence**, exactly as the file lists it (code points in order, including U+FE0F where the fully-qualified form has one). It is stored, compared and sent as that UTF-8 string.
2. **Lookup ignores presentation selectors.** To canonicalize an input: require valid UTF-8 first (`utf8.ValidString`; an invalid string is `invalid_reaction` before any bound or lookup); bound it (§3.3); remove every U+FE0F (VS16) and U+FE0E (VS15) wherever it occurs, so a stray selector anywhere in the input canonicalizes to the listed key; look the remainder up in the table keyed by *every listed form with its selectors removed* (fully-, minimally- and unqualified forms all strip to the same remainder); the value is the fully-qualified key. There is no other normalization: emoji code points have no canonical decompositions, so NFC would be a no-op, and no case folding applies (regional indicators and tag characters are caseless).
3. **Components alone are refused.** A skin-tone modifier (U+1F3FB..U+1F3FF) or hair component on its own has status `component` and is not a reaction.
4. **Skin tones and ZWJ sequences are distinct keys.** `👍` and `👍🏽` are two keys; `👨‍👩‍👧` is one key and is not the three people; `🏳️‍🌈` is one key distinct from `🏳️`.
5. **Everything else is refused by the failed lookup**: an empty string, a sequence not in the table after step 2, any string that holds more than one listed sequence, letters, digits, punctuation, whitespace, control characters, and anything over the bounds. There is **no pre-filter on character class**: the format characters that listed sequences contain (U+200D ZWJ, U+FE0E/U+FE0F, the tag characters U+E0020..U+E007F and U+20E3) are accepted exactly where a listed sequence has them, and a control or format character anywhere else fails because no listed sequence matches. Refusal is 400 `invalid_reaction` and stores nothing.

Where it lives: `schema.CanonicalReactionKey(s string) (key string, err error)` and `schema.ValidateReactionKey(key string) error` (true only when `key` is already canonical) in `pkg/schema/reaction.go`, over the generated table. The server canonicalizes every input (path segment or MCP argument) and answers with the canonical form; `ReactionSummaryV1.Validate` and `ReactionV1.Validate` require canonical keys, so a fixture or a frame with a non-canonical key fails validation.

Examples (code points in hex):

| Input | Steps | Result |
| --- | --- | --- |
| `👍` (1F44D) | fully-qualified as given | key `1F44D` |
| `❤` (2764) | unqualified → strip → `2764` → table | key `2764 FE0F` (`❤️`) |
| `❤️` (2764 FE0F) | strip → `2764` | key `2764 FE0F` |
| `❤︎` (2764 FE0E, text presentation) | strip → `2764` | key `2764 FE0F`: a reaction is always emoji presentation |
| `👍️` (1F44D FE0F, a stray selector the picker or a platform added) | strip → `1F44D` | key `1F44D`: a selector anywhere is removed before lookup |
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
| `\xff\xfe` (invalid UTF-8), `👍` followed by U+202E (a bidi control appended) | invalid UTF-8 fails first; the control makes the lookup fail | **refused** |
| `🤷‍♀️` (1F937 200D 2640 FE0F) | fully-qualified | one key, distinct from `🤷` and `🤷‍♂️` |

Shortcodes (`:+1:`) are a **client** vocabulary for entry and for terminals that cannot render a glyph; they never travel on the wire. `emoji-test.txt` carries CLDR short names ("thumbs up: medium skin tone"), not shortcodes; if #767 wants a shortcode table that is a separate dataset and licence question (§16).

### 3.3 Bounds

`MaxReactionKeyBytes = 64` and `MaxReactionKeyCodePoints = 16`, applied to the input after the UTF-8 check and before any lookup. The longest listed sequences (family ZWJ sequences, subdivision flags) are at most 10 code points and under 36 bytes; the headroom is for a future version. Both are `pkg/schema` constants so the three clients refuse the same inputs before sending.

## 4. Operations and errors

### 4.1 REST

The five reaction routes are `accessAuthenticated` in `routes.go`; the two moderation routes are `accessChannelManager`. The handler applies the gates of §6. `{emoji}` is the percent-encoded UTF-8 of the key in any qualification (the server canonicalizes; `http.ServeMux` decodes path values); clients **must percent-encode** the key, in particular `#` (`%23`) in the keycap sequences, which would otherwise start a fragment. Canonicalization happens **after the channel gate and before the store**, in the same place for REST and MCP, so an unknown channel is `channel_not_found` whatever the key, and a bad key never reaches a statement. `{message_id}` is the message's id. The `/v2/` prefix is deliberate: these routes answer about messages that may carry an audience, and only a v2 reader may be told of one (`store.Reader.Scoped`).

| Route | Body → answer | Refusals beyond the common ones (401, 404 `channel_not_found` for a non-member or unknown channel, 403 `forbidden` for an agent refused by its manifest at the channel gate) |
| --- | --- | --- |
| `PUT …/messages/{message_id}/reactions/{emoji}` | none → 200 `ReactionSummaryV1` (the message's whole summary after the write, `me` true for this key) | 400 `invalid_reaction`; 404 `message_not_found` (unknown, another channel's, or invisible to the caller); 403 `forbidden` for an agent that may see the message but lacks the audience permission to reply there (`post_net` on a net, `whisper` in a whisper), audited; 403 `reaction_not_permitted` (no current member seat on the message's net: a monitor, or taken off since the post; a removed message, §11); 409 `too_many_reactions`; 429 `rate_limited` with `Retry-After` and `retry_after_seconds`; 400 `reactions_require_auth` (authentication off) |
| `DELETE …/messages/{message_id}/reactions/{emoji}` | none → 200 `ReactionSummaryV1` | 400 `invalid_reaction`; 404 `message_not_found`; 403 `forbidden` for an agent without the audience permission (the same gate as `PUT`, so the two answer alike); 429; 400 `reactions_require_auth`. Removing a reaction one does not hold is a no-op that answers the summary. A monitor, a principal off the net, or a removed message gets 200: removing one's own mark never needs transmit rights. |
| `GET …/messages/{message_id}/reactions` | → 200 `ReactionSummaryV1` (an empty `reactions` list when none) | 404 `message_not_found`; 400 `reactions_require_auth` |
| `GET …/messages/{message_id}/reactions/{emoji}?after=<principal_id>&limit=<1..100>` | → 200 `ListReactorsResponseV1`, ordered by `principal_id` ascending; `next_after` present when there may be more | 400 `invalid_reaction`, 400 `invalid_request` (cursor, limit); 404 `message_not_found`. An emoji nobody used is an empty list, not an error. |
| `GET /v2/channels/{channel}/reactions?messages=<id>[,<id>…]` | → 200 `ReactionSummariesResponseV1`: one summary per listed message that the caller may see **and** that has at least one reaction; ordered by `message_id` ascending | 400 `invalid_request` (more than `MaxReactionBatchMessages` = 100 ids, a non-positive or duplicate id, or none). An invisible, unknown or unreacted id is simply absent; the three are indistinguishable, and a client treats an absent id as an **empty summary**. |
| `DELETE …/messages/{message_id}/reactions/{emoji}/{principal_id}` (moderation, §11.5) | none → 200 `ReactionSummaryV1` | class refusals (`channel_not_found` for a non-member manager or operator, `forbidden` for a plain member, the human-only refusal for an agent); 400 `invalid_reaction`; 404 `message_not_found` (the caller must be a member who can see the message, operators included); an absent reaction or unknown principal is an idempotent 200 that writes nothing |
| `DELETE …/messages/{message_id}/reactors/{principal_id}` (moderation, §11.5: every reaction of one principal on the message) | none → 200 `ReactionSummaryV1` | the same |

Recent-auth class: the five reaction routes are authenticated self-writes or reads, never an operator grant, so `TestRecentAuthClass` pins them `reauthNone`; the two moderation routes are reduce-only like membership removal and message removal (moderation.md §3.1) and are pinned `reauthNone` too. The route-table test takes a policy decision for each. `reactions/{emoji}` and `reactions/{emoji}/{principal_id}` differ in segment count and `reactors` is a literal, so no two patterns meet in the mux.

Why 200 with the summary on `PUT` and `DELETE` rather than 201/204: the caller's next render needs the authoritative state (another client of the same person may have changed it), and the socket frame may arrive before or after the response. One body shape for the write answer and the live frame keeps both clients' code to one path.

Why the batch route rather than a `reactions` field on `MessageV2`: a page of 50 messages is hydrated in one extra request with one aggregate statement, which satisfies "no N+1"; `MessageV2`, its fixtures, `/v2/ws` frames and `read_channel` output stay untouched; and an old client never sees a field it would ignore. A later `?include=reactions` on the history list would be additive if wanted.

### 4.2 MCP

Four tools, each a plain mirror of one REST route and nothing more (parity, D6): same gates, same store functions, same error codes through `mcpToolError`. Inputs name the channel by name as the other tools do; the server binds the agent's principal.

| Tool | Input | Output | Capability / channel permission |
| --- | --- | --- | --- |
| `set_reaction` | `{channel, message_id, emoji}` | `ReactionSummaryV1` | `messages.post` / `post` at the channel gate; then, once the message row is read, the audience permission to reply there (`post`, `post_net` or `whisper`), else `forbidden`, audited |
| `remove_reaction` | `{channel, message_id, emoji}` | `ReactionSummaryV1` | the same |
| `read_reactions` | `{channel, message_ids: [..≤100]}` | `ReactionSummariesResponseV1` | `messages.read` / `read` |
| `list_reactors` | `{channel, message_id, emoji, after?, limit?}` | `ListReactorsResponseV1` | `messages.read` / `read` |

Tool descriptions say what the server does and does not do: "Reacting does not post a message, does not notify anyone and does not mark anything read; the people who can see the message see the reaction." `mcpToolCapabilities` in `pkg/schema/agent_manifest.go` gets the four entries (`set_reaction`, `remove_reaction` → `messages.post`; `read_reactions`, `list_reactors` → `messages.read`), in the same schema PR as the types (the `schema-change` skill's sync rule); `docs/design/mcp-endpoint.md` lists them. No MCP-only capability is introduced; the single-message `GET …/reactions` has no tool because `read_reactions` with one id is the same call. The two moderation routes have **no tool**: as moderation.md §1 says, no operation adds an MCP administration tool, and agents are refused at the class.

### 4.3 Error codes (new, additive on `schema.Error`)

| Code | HTTP | Meaning |
| --- | --- | --- |
| `message_not_found` | 404 | The message does not exist, is in another channel, or is not visible to the caller. One answer for all three (also proposed by moderation.md §10; defined once). |
| `invalid_reaction` | 400 | The emoji is not valid UTF-8, is not one canonical reaction key (§3.2) or exceeds the bounds (§3.3). |
| `reaction_not_permitted` | 403 | The caller can see the message but may not react to it: no current *member* seat on the net it was said on (a monitor, or a principal taken off the net since the post), or a removed message. An agent lacking the audience permission gets the existing `forbidden` instead, audited. |
| `too_many_reactions` | 409 | The write would exceed `MaxReactionKindsPerMessage` or `MaxReactionsPerPrincipalPerMessage` (§7.3). Nothing stored. |
| `reactions_require_auth` | 400 | `conchd` runs with authentication off; there is no principal to hold a reaction. Same pattern as `read_state_require_auth`. |

Existing codes reused unchanged: `channel_not_found`, `forbidden`, `rate_limited`, `invalid_request`, `request_too_large`, `internal_error`.

## 5. Wire types (proposed `pkg/schema/reaction.go`)

All additive: new types, new constants, new error codes; no published type or fixture changes. `ReactionSummaryV1` carries a `schema` field because it travels on the events socket; the REST-only bodies do not, following `ChannelMemberV1`. Every type is listed in `ts.go`'s `TSTypes` with a fixture family; the vocabularies are unchanged; the scalar constants are emitted through the new `ts.go` support of §3.1.

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
	// MaxReactionKindsPerMessage is the write limit (§7.3). It may be raised
	// or lowered later without touching the wire: Validate does not use it.
	MaxReactionKindsPerMessage         = 20
	MaxReactionsPerPrincipalPerMessage = 10
	// MaxReactionKindsOnWire is the published bound a ReactionSummaryV1 is
	// validated against. It is fixed for v1: a message that already holds
	// more kinds than a later, lower write limit must still encode, or the
	// events sockets that carry it would close. Changing it is a breaking
	// change (a v2).
	MaxReactionKindsOnWire   = 100
	MaxReactionBatchMessages = 100
	DefaultReactorLimit      = 50
	MaxReactorLimit          = 100
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
// time of the earliest reaction still held with that key, then by key, so
// the order a client shows is stable while counts grow; a key may move when
// its earliest reaction is removed.
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
// MaxReactionKindsOnWire entries (never the live write limit); each Emoji
// canonical, each Count >= 1, keys distinct; AsOf non-zero.
func (s ReactionSummaryV1) Validate() error

// ReactionSummariesResponseV1 is the body of the batch read: one summary per
// requested message the caller may see that has at least one reaction,
// ordered by message_id. Always a JSON array.
type ReactionSummariesResponseV1 struct {
	Summaries []ReactionSummaryV1 `json:"summaries"`
}

// ReactionV1 is one record: who reacted with one emoji, and when. The
// reactor is named by id only, as a message names its author: a client
// resolves principal_id the way it resolves author_id (the member list, or
// hidden-<id> for a principal it cannot resolve), so a former member's
// current name is never carried here. GuestReactor is the attribution the
// reactor had when it reacted, stored with the record and never changed
// (guests-and-accounts.md §4.4), and is what renders a guest's reaction.
type ReactionV1 struct {
	PrincipalID  int64               `json:"principal_id"`
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
    {"principal_id": 7, "emoji": "👍", "created_at": "2026-10-28T13:58:02.117Z"},
    {"principal_id": 12, "emoji": "👍", "created_at": "2026-10-28T13:58:40.001Z"},
    {"principal_id": 31, "emoji": "👍", "created_at": "2026-10-28T14:01:09.550Z",
     "guest_reactor": {"handle": "guest-7h3kq2m9p4r8s6t1", "label": "Nick", "tag": "7h3k"}}
  ],
  "next_after": 31
}
```

MCP tool inputs are `mcp.go` mirror structs as today (`mcpPostMessageInput` precedent); outputs are the `pkg/schema` types above, unchanged.

## 6. Permission and visibility model

### 6.1 The two questions, asked in order

Every route and tool asks, in this order, and stops at the first refusal:

1. **May the caller use this channel?** `channelForCaller` (REST) or `agentScope.channelByName` (MCP): the channel exists and the caller is a current member, else 404 `channel_not_found`; for an agent, the manifest grants the capability and permission of the row in §4 (`messages.post`/`post` for a write, `messages.read`/`read` for a read), else 403 `forbidden`, audited; the agent's budget is spent here (`spendAgentBudget`). A human's write takes a token of the reaction bucket of §7.4 **here, when the gate passes**, before the key is canonicalized and before the store; nothing after this point refunds it (§7.4). Then the key is canonicalized (§3.2), the same for REST and MCP.
2. **May the caller see this message?** In the statement that reads or writes the reaction rows, joined to `messages m` under **two** conditions with the caller's `Reader` (`readerFor(r, apiV2)`): `messageVisible(scoped=true)` (the audience rule and the history bound; for a scoped message also membership and enabled), **and** the explicit clause `EXISTS (SELECT 1 FROM channel_members rc WHERE rc.channel_id = m.channel_id AND rc.principal_id = ?) AND EXISTS (SELECT 1 FROM principals rp WHERE rp.id = ? AND rp.disabled_at IS NULL)`, the form `runVisibleHead` uses (`audience.go:1145-1151`). The second clause is not redundant: `messageVisible` checks membership and enabled only for a scoped message (`audience.go:235-242`, `311-316`), and a channel-wide message would otherwise be readable, through its reactions, by a reader removed from the channel between the gate and the statement, or by a subscriber whose reference was flushed after a removal committed and before the drop. Otherwise 404 `message_not_found`, and nothing else is read.
3. **Writes only: may the caller transmit where it was said?** Once the message row is read (handler probe, then re-read in the transaction), a net message needs the caller's current *member* seat on that net (`resolveNetRecipientsTx`'s rule), else `reaction_not_permitted`; an agent needs the permission it would need to reply in that audience, `post` channel-wide (`agentCallerAllowed`), `post_net` or `whisper` (`authorizeScopedPostGates`), else `forbidden`, audited with `denyAudienceGrant`, as `postReply` does (`messages.go:683-703`). The budget is spent once per call, as on the reply path.

The second question is never re-asked in Go after the statement. The handler runs **one transaction: one probe, then the aggregate**. The probe is `SELECT m.audience_kind, m.net_id FROM messages m WHERE m.id = ? AND m.channel_id = ? AND <messageVisible> AND <member and enabled>`; no row is `message_not_found`. The aggregate is `SELECT r.emoji, COUNT(*), MIN(r.created_at), SUM(r.principal_id = ?) FROM message_reactions r WHERE r.message_id = ? GROUP BY r.emoji`, run only after the probe found the row, in the same transaction, so the two cannot disagree. The reactor list and the batch read follow the same shape (the batch probes every requested id in one statement, then aggregates over the ids that passed). A reader who may not see a message gets `message_not_found` or, in the batch, an omission, and cannot tell an invisible message from an unknown one. Every statement that names `messages` lives in `audience.go` (§2).

### 6.2 By operation and caller

| Caller | Read summary / reactors / batch | Set | Remove own |
| --- | --- | --- | --- |
| Member (human, non-guest) | Messages they can see | Yes, except on a net message where they hold no current member seat (a monitor, or taken off the net since the post), or on a removed message | Yes |
| Guest (active visit) | Messages after the membership's `history_after`; nothing before, replies only when the thread's root is after it (the existing bound) | Yes, on those messages, under the same net rule; the record carries `guest_reactor` | Yes |
| Agent | Messages they can see, with `messages.read` and `read` in the channel | With `messages.post` and `post` at the channel gate, then the permission it would need to reply in the message's audience: `post` channel-wide, `post_net` on a net, `whisper` in a whisper (refused → `forbidden`, audited with `denyAudienceGrant`); the net seat rule applies too; spends the `messages.post` budget once | Same as set |
| Operator or owner | No exemption: a member like any other | Same | Same; plus the moderation routes of §11.5 on messages they can see as members |
| Channel manager | Same as member | Same | Same; plus the moderation routes of §11.5 in the channel they manage, on messages they can see |
| Non-member (any role) | `channel_not_found` | `channel_not_found` | `channel_not_found` |
| Disabled principal, revoked or expired credential | Refused by authentication before any route | — | — |
| Authentication off | `reactions_require_auth` on every route | same | same |

Why an agent reacts with the permission it would need to reply: a reaction on a net message or a whisper is a small transmission into that audience, and the manifest already says, per channel, whether the agent may transmit there (`post`, `post_net`, `whisper`). Reusing the reply rule means an operator reads one grant table, a reaction is never a way to speak into a net or whisper the agent may only read, and the capability vocabulary does not grow. An agent that may only read may not react anywhere. The alternative, a new capability `reactions.write`, is in §15.

Why the net seat rule: a monitor "listens but may not transmit" (`ErrNetMonitorOnly`), and a principal taken off a net keeps what it was sent but gets nothing said on it from then on (`audience.go:740-746`: an old thread must not be a way round that). A reaction on a net message is seen by the net's recipients and is a small transmission; requiring a current *member* seat (`resolveNetRecipientsTx`'s rule: a row on the net with role `member`) keeps both halves of ADR-005. Both callers already see the message, so `reaction_not_permitted` discloses nothing. Removing one's own earlier reaction is always allowed: it transmits nothing new.

### 6.3 Events and replay

A live frame for message `m` is sent to a subscriber only if, **at send time**, the subscriber's reader may see `m` by the same statement (§8.1). A frame is never computed once and fanned out. Replay has no reaction form of its own: the reconnect read is the batch route under the same rule (§8.2). Agents' events sockets receive frames under the `messages.read` budget spent at upgrade, as every events frame is.

### 6.4 Reactor identity

A reactor is named by `principal_id` to anyone who can see the message, exactly as a message author is by `author_id`; the record carries no name and no kind. A client resolves the id as it resolves an author: from the member list it may read, falling back to the hidden-style rendering for an id it cannot resolve, so a former member's current name is never served through a reaction. A guest reactor renders from the `guest_reactor` snapshot, during and after the visit. The reactor list never includes anyone whose reaction the caller could not have seen, because reactions exist only on messages, and the caller sees the list only for a message they see. Nothing lists "every reaction by principal X" across messages or channels; there is no per-principal route.

## 7. Storage, transactions and limits

### 7.1 Table (the next migration entry in `store.go` at merge time; 37 at this commit, but moderation may take it first)

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
- Rows are never updated. They are deleted only by the owner's own `DELETE` (§4.1) or by a manager's or operator's moderation `DELETE` (§11.5). Nothing else removes them (§9, §11).
- Additive: a database from before this migration opens as today; a binary from before it refuses a database that has it (`store.migrate` refuses a newer `user_version`), which is the existing rule for every migration (§12).

### 7.2 Write transaction

`store.SetReaction(ctx, channelID, reader, messageID, key, now)` and `store.RemoveReaction(...)`, both `withImmediateTx`:

1. Re-check the reader is a current member of `channelID` and an enabled principal (`isChannelMemberTx`; the enabled check as `visibleToScopedReader` does). Not → `ErrNotChannelMember` → 404 `channel_not_found` (the same answer as the handler's gate, so a removal racing the gate is indistinguishable from never having been a member).
2. Select the message's `audience_kind`, `net_id`, `thread_root_id` and (once moderation lands) its removal, by `m.id = ? AND m.channel_id = ? AND messageVisible(scoped) AND <member and enabled>` with the reader's arguments (§6.1). No row → `ErrMessageNotFound` → 404 `message_not_found`. The handler has already probed the same row outside the transaction to apply an agent's audience permission (§6.1 step 3); the transaction re-reads it so that what is stored is what was authorized, as `InsertReplyMessage` re-reads the root.
3. **Set only:** if `audience_kind = 'net'`, read the reader's seat on the net (`SELECT role FROM net_members WHERE net_id = ? AND principal_id = ?`, the net live and in this channel); **no row, or a role other than `member`** → `ErrReactionNotPermitted` → 403. A removed message → the same error. (A reply inherits its root's audience; the reply row carries `audience_kind`/`net_id` itself, so no second read.)
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
- **Humans** get one new keyed token bucket (the `bucketSet` of `invite_limits.go`, keyed by principal id): `reactionWriteLimit` of 60 writes per minute with a burst of 20. The token is taken **when the channel gate passes, before canonicalization and before the store** (§6.1 step 1). Only a 429 `rate_limited` (with `Retry-After` and `retry_after_seconds`) spends nothing; a write refused afterwards with `invalid_reaction`, `message_not_found`, `forbidden`, `reaction_not_permitted` or `too_many_reactions` has spent its token, so probing ids or keys costs what reacting costs. There is **no refund**: the invite limiter refunds only its server-wide redeem bucket (`refundGlobal`, `invite_limits.go:230-241`) so that a shared allowance is not drained by failures; a per-principal bucket has no such sharing, and refunding on refusal would make refusals free. Reads have no new limit beyond the existing socket and request bounds; a summary is one indexed aggregate.

## 8. Events, replay and convergence

### 8.1 Live frames

A committed set or remove that changed a row (and only then) calls `events.notifyReactions(channelID, messageID)`, which pushes `eventRef{kind: eventReactions, channelID, id: messageID}` to every subscriber of that channel, human or agent, the actor included (its other clients need it). The subscriber's goroutine, at send time:

1. checks `hasMembership(channelID)` as the message notification does;
2. reads `ReactionSummaryV1` for `messageID` with `readerFor(e.r, apiV2)` (§6.1, probe then aggregate): no row because the message is invisible, **or because the reader is no longer a current, enabled member** (the explicit clause of §6.1 step 2), → sends nothing. So a reference flushed after a removal committed but before `dropPrincipal` closed the socket sends nothing, for a channel-wide message as for a scoped one;
3. encodes and writes the frame.

A moderation removal (§11.5) and, later, a message removal (§11.2) call `notifyReactions` the same way.

References for the same message coalesce in a subscriber's queue (one pending reference per message id), so a burst of ten reactions on one message costs one read when the socket drains, and a slow subscriber is closed by `eventsQueueMax` as today. There is no reaction floor: the write rate limit of §7.4 bounds how fast references can be made.

Nothing is sent on `/v2/ws`, `/v1/ws` or `/v0/ws`: those carry message envelopes (and, on `/v2/ws`, heartbeat frames), and an old client reading them must see exactly what it sees today. The browser already holds both sockets (`web/src/api.ts`: `/v2/ws` for messages, `/v1/events/ws` for documents). The terminal client opens the events socket for reactions where it wants them live (#767).

### 8.2 Snapshot and replay

There is no reaction replay. On connect or reconnect a client hydrates the messages it displays with one batch read (`GET /v2/channels/{channel}/reactions?messages=…`, up to 100 ids, which is one page of history), and does so again for each further page of history it loads. A requested id absent from the answer means an empty summary: the client clears whatever it showed for that message. The events socket sends no reaction frame on subscribe. This is the roster's model turned around: the roster is sent whole on connect because it is one document; reactions are one document per message, so the client names which it wants.

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
| Hiding a converted member | Post-conversion reactions name the principal id only, which the client now resolves to the hidden name as it does for the member's messages; visit-time reactions keep the stored guest attribution (the message rule). Hide refuses an active guest as today. |
| Cleanup of a guest "that posted nothing" (§4.4) | A reaction counts as having posted: a visit with any reaction row keeps its attribution record. Recorded in §17 as an interpretation. |

Guests may react because §4.3 lets a guest "read/post in assigned conversation channels", and a reaction is a lesser post. Guests cannot react where they cannot post (a channel they are not admitted to), and the approval surface is untouched: an approval is not a message, and reacting to a message that announces one changes no decision.

## 10. Races and revocation

| Race | Outcome |
| --- | --- |
| Two sets of the same (message, principal, emoji) from two sessions | IMMEDIATE transactions serialize; one inserts, one no-ops; both answer the same summary; one frame (coalesced) or two identical ones. |
| Set and remove of the same reaction interleaved | Each transaction sees the rows as of its start; the final state is the last committed write; both responses are true at their `as_of`; clients converge by §8.3. |
| Set racing membership removal | Before removal commits: the row commits and is retained (§11.1); the removed principal's sockets are dropped, so it receives no frame; others do. After: `ErrNotChannelMember` → `channel_not_found`, same as a stranger. |
| A reference flushed after a removal committed but before the drop closed the socket | The send-time read carries the explicit member-and-enabled clause (§6.1), so it finds no row and sends nothing, for a channel-wide message as for a scoped one; the drop then closes the socket. |
| Moderator removal racing the reactor's own remove, or a fresh set | IMMEDIATE transactions serialize; both `DELETE`s are idempotent; a set that commits after the moderator's removal stands (it is a new record, and the moderator may remove it again). Both answers are true at their `as_of`. |
| Set racing credential revocation or disable | The request either authenticated before revocation (and then the transaction's enabled check may still refuse it, since disable is also a principal state) or is refused at the door. A committed row is retained. |
| Set racing guest expiry | `CredentialLive` refuses at the door once expired; the transaction's membership check refuses if `endGuestVisitTx` committed first. |
| Read racing visibility loss (removed from channel or net, message removed) | The next read or frame computes with the new state; a frame already in flight shows at most the previous state. Dropped sockets receive nothing further. |
| Frame computed before a later write's response | Client rule §8.3(2). |
| Set racing message removal (moderation, future) | Whichever commits second sees the other: a set after removal is `reaction_not_permitted`; a removal after a set leaves the row and hides it (§11.2). |
| Restart between commit and frame | The frame is lost; the row is durable; hydration on reconnect shows it. |
| Restore of a backup | Rows restored are as consistent as the messages they attach to (foreign keys); counts are recomputed from rows, so no counter drifts. The restore procedure's credential invalidation (§4.2 of the guest design) is unchanged. |

## 11. Deleted messages, moderation, audit and notifications

### 11.1 Reactors who lose access

Membership removal, net removal, ban (moderation §6), disable, hide, guest end: **reaction rows stay**, as the principal's messages stay. The reactor cannot change them while it has no access; a member re-added later can remove its own again; a manager or operator can remove them at any time (§11.5), which is what a retained row's abuse is answered with. The alternative (deleting a principal's reactions when it leaves) was rejected: it would change counts other people have already seen, retroactively, for a reason unrelated to the message, and it would treat a reaction unlike a message, which the guest design forbids for attribution.

### 11.2 Removed messages (moderation.md §7, not yet implemented)

Written so that #765 needs nothing from the moderation PR and the moderation PR needs one line from #765:

- A removed message's summary is **empty** and it is **absent** from the batch read; its reactor lists are empty. The rows are kept (nothing deletes them) but every reaction statement adds `AND NOT EXISTS (SELECT 1 FROM message_removals x WHERE x.message_id = m.id)` once that table exists. Until then the clause is absent and the behaviour is identical because no message is removed.
- A set on a removed message is 403 `reaction_not_permitted` (the tombstone is visible, so a 404 would be a lie and a disclosure); a remove is 200 with the empty summary.
- The removal's commit calls `notifyReactions(channel, message)` so open clients drop the summary with the tombstone frame; a reconnecting client's hydration simply omits it.

### 11.3 Audit

Three actions, written in the write transaction **only when a row changed** (the moderation precedent for idempotent repeats): `reaction_set` and `reaction_removed`, actor `principal:<id>`, subject `message:<id>`, detail `channel=<id> emoji=<key>`; and `reaction_moderated` for §11.5, actor the moderator, subject `message:<id>`, detail `channel=<id> principal=<target> emoji=<key>` or `channel=<id> principal=<target> all` with the number of rows removed. The key is content in the loosest sense; the message body is never in the detail, nor any free-text reason. No-ops, reads and refusals write nothing beyond the existing agent-denial rows (`denyAudienceGrant` for an agent lacking the audience permission). The rows appear in the owner-visible `GET /v1/audit` (`accessOwner`) like every other, with no new filter. Volume: bounded by the human rate limit and agent budgets; at 60 per minute per principal it is comparable to posting.

### 11.4 Notifications and read state

A reaction **does not**: move or create a read mark; change an unread count (`notifyReadState` is not called); send the author a frame of a kind that the browser or terminal treats as a new message; call the configured `Broadcaster` seam or ntfy; create an approval, a run or an intervention; or reach any agent through anything but the events socket it already holds and the reads it already makes. An agent that wants to react to reactions must poll or subscribe explicitly; no server behaviour starts it. Notifying an author of reactions on its message is a later, additive design.

### 11.5 Moderation of reactions

Because rows outlive access (§11.1), a reaction left by someone who has since been removed, or a pile of reactions used to grief a message, needs a lever finer than removing the message. #765 therefore includes two reduce-only routes, class `accessChannelManager`, following moderation.md's pattern for message removal (§3, §7.1 there):

- `DELETE /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}/{principal_id}` removes one reaction of one principal;
- `DELETE /v2/channels/{channel}/messages/{message_id}/reactors/{principal_id}` removes every reaction of one principal on the message.

Rules:

- **Who:** owner, operator, or a manager of this channel (the class). Agents are refused before lookup. A plain member gets 403 `forbidden`; a non-member manager or operator gets 404 `channel_not_found`, as for every manager route.
- **Content check, operators included:** the caller must be a current member who can see the message by §6.1 (probe with `messageVisible` and the member-and-enabled clause), else 404 `message_not_found`, indistinguishable from an unknown id. A manager cannot strip reactions off a whisper or net message they cannot read, nor enumerate ids.
- **Target:** any principal, including an owner, operator, agent or guest. Content moderation is not an account action (moderation.md §7.1): it changes a record, not the target's rights, membership or credential. The target is named by id; nothing is looked up about it, so an unknown or absent target is an idempotent 200 that writes nothing.
- **Answer:** 200 `ReactionSummaryV1` after the write, the same shape as every other write, so the moderator's client converges without a second read. Removing nothing is 200 too, with no audit row.
- **Effect:** the rows are deleted in one IMMEDIATE transaction with the `reaction_moderated` audit row (§11.3); `notifyReactions` runs after commit, so every reader's client, the target's included if it still sees the message, drops the reaction. No socket is dropped, no credential or membership changes; the target may react again, and may be removed from the channel or banned by the existing routes if that is what is wanted.
- **Recent-auth:** `reauthNone`, reduce-only, as membership removal and message removal are.
- **No MCP tool** (moderation.md §1); the CLI and browser use the routes (D6). Client confirmation names the consequence ("Remove this reaction for everyone who can see the message; the person may react again").

The coarser levers remain and are not implied: remove the member or ban them (their past reactions stay, §11.1), or remove the message (its reactions vanish from every read, §11.2).

## 12. Compatibility, migration and rollback

- **Schema:** new types, constants and error codes only (§5). No published type or fixture changes; `scripts/schema-compat.sh` passes. Not a version bump. `MessageV2` is untouched.
- **Old clients:** a browser or terminal that predates reactions sees no reactions and loses nothing: message routes, message sockets and their frames are unchanged. The events socket sends a new `schema` value; the browser dispatches events frames by `schema` and must **ignore an unknown one** (#766 confirms with a test against the shipped client; `web/src/state/feed.ts` already discriminates by schema). The TUI's events handling, where it has any, is held to the same rule (#767).
- **Old servers:** a client that sends a reaction route to a `conchd` that predates reactions gets the mux's 404, whose body is **not a `schema.Error`** (Go's default `404 page not found` text); a 404 that *is* a `schema.Error` (`channel_not_found`, `message_not_found`) is an ordinary refusal from a server that has reactions. Clients detect a pre-reaction server by the first kind of 404 on the batch read and hide the controls; they never infer it from a `schema.Error`. Over MCP the detection is that the four tools are absent from `tools/list`; an agent does not call a tool it was not offered. No whoami flag is added.
- **Migration (the next entry at merge time):** one new table and one index; nothing backfilled, nothing rewritten; a pre-reaction database opens and runs on the new binary at once.
- **Rollback:** the existing rule for every migration applies: a binary from before the reaction migration refuses to open a database at or past its `user_version` ("newer than this binary"). Rolling the binary back therefore means restoring the pre-upgrade backup (Litestream or the operator's copy), as for any migration; nothing in this design makes that worse or better, and the release notes of the version that ships the migration say so, as they do for every migration. Turning the feature off without rolling back is not supported in this slice: there is no runtime switch (a `server_settings` row `reactions_enabled` would be an additive follow-up if an operator asks).
- **Emoji version upgrade:** regenerate from a newer `emoji-test.txt`; stored keys are never re-validated, and any key accepted before is accepted after (Unicode does not retire sequences; if it ever marked one non-emoji the generator would keep it in a `retired` set so stored and replayed keys still validate).
- **Approval path:** untouched. No file under the approval request → notify → resolve → audit chain changes; reactions do not read or write `approvals*`, do not notify, and are not labelled `approval-path`. A security-reviewer pass is still required (new authenticated write surface, new visibility statements).

## 13. Implementation checklist

### Item 0 — catalog and schema slice (#953; #763 and #765 depend on it; size S–M)

- [ ] Vendored `emoji-test.txt` + its Unicode-3.0 licence file, pinned SHA-256, and the generator writing `pkg/schema/emoji_catalog_gen.go` (sequences by stripped form → fully-qualified key, statuses, `EmojiVersion`) and the web catalog JSON (#763's shape); the generator fails on a collision of stripped fully-qualified sequences (§3.1); a `make check` step that fails on a stale output or a hash mismatch.
- [ ] `pkg/schema/reaction.go`: types, constants (including the fixed `MaxReactionKindsOnWire`), error codes, `CanonicalReactionKey` (UTF-8 check, bounds, selector stripping, lookup), `ValidateReactionKey`, `Validate` methods; `ts.go` entries for the types; **new:** `ts.go` support for scalar constants, emitting `EmojiVersion`, `MaxReactionKeyBytes`, `MaxReactionKeyCodePoints`, `MaxReactionBatchMessages`, `DefaultReactorLimit`, `MaxReactorLimit`, checked by `scripts/schema-ts-check.sh`; `mcpToolCapabilities` entries for the four tools (same PR, the `schema-change` sync rule); fixtures (`reaction-summary-v1.json`, `-empty.json`, `reaction-summaries-response-v1.json`, `reaction-v1.json`, `list-reactors-response-v1.json`, `-guest.json`); `go generate ./pkg/schema`; `scripts/schema-compat.sh` green; the `schema-change` skill followed (compatible: no sign-off step beyond the listing in §16).

### #765 — server: store, REST/WS, MCP (size L; security review)

- [ ] The migration of §7.1 (the next entry at merge time) with a migration test from the preceding version.
- [ ] Store: every statement that names `messages` or `message_recipients` lives in `audience.go` as helpers that own the join (the probe of §6.1 with `messageVisible` **and** the explicit member-and-enabled clause, the net seat read), because `TestOnlyAudienceGoReadsMessageRows` is a textual check over the other files; `reactions.go` may hold only the statements over `message_reactions` alone (aggregate, reactor page, insert, delete, moderation delete). Functions: `SetReaction`, `RemoveReaction`, `RemoveReactionsOf` (moderation), `ReactionSummary`, `ReactionSummaries` (batch), `ListReactors`; `ErrMessageNotFound`, `ErrReactionNotPermitted`, `ErrTooManyReactions`; audit rows (three actions); guest attribution read in the transaction; the `message_removals` clause left as a documented hook.
- [ ] Routes (§4.1, §11.5) in `routes.go` with `TestRouteTable` and `TestRecentAuthClass` decisions (all `reauthNone`); handlers in `reactions.go`: channel gate → human bucket token taken, no refund → canonicalization → message probe → agent audience permission as `postReply` does (`agentCallerAllowed` for channel-wide, `authorizeScopedPost` for net/whisper, budget spent once) → store; `readerFor(r, apiV2)`; auth-off refusal. The same order in the MCP tool bodies.
- [ ] Events: `eventReactions` kind, `notifyReactions`, per-message coalescing, send-time probe-then-aggregate with the subscriber's reader (§8.1), called after own writes, moderation removals and (hook) message removals; no change to `/v2/ws`.
- [ ] MCP: four tools (§4.2) over the same store functions and error mapping as REST; descriptions; `docs/design/mcp-endpoint.md`; no moderation tool.
- [ ] Isolation: matrix rows for all seven routes (A's message in A's channel → `channel_not_found`; A's message id through B's channel → `message_not_found`; ghost ids identical; the moderation routes also as a plain member → `forbidden` and as B's manager → `channel_not_found`); guest rows for the bound (pre-bound message → `message_not_found` / absent); conversion rows (same answers after conversion); agent rows (`forbidden` without `post`; with `post` but without `post_net` on a net message, and without `whisper` on a whisper); net rows (monitor, and a member taken off the net → `reaction_not_permitted`); revoke rows (no frame after drop, and nothing from a reference flushed between the commit and the drop).
- [ ] Docs: API reference section, operator note on limits, the moderation routes and audit actions, CHANGELOG line; `security-reviewer` pass before merge.

### #766 — browser (size M)

- [ ] Reaction bar under each message: counts in summary order, `me` highlighted, press to set/remove; "add" opens #763's picker (shared catalog, same `EMOJI_VERSION`); keyboard and screen-reader labels; narrow widths.
- [ ] Hydration by the batch route for each page of history; events-socket `conch.reaction_summary.v1` handling by §8.3; unknown-schema frames ignored (test); re-hydrate on reconnect.
- [ ] Reactor list on hover/focus/long-press via the reactor route, paginated; names resolved from `principal_id` as for `author_id`; guest rendering from `guest_reactor`, never from a name.
- [ ] Moderation controls for managers and operators (remove one reaction, remove all of a principal's on the message) with the confirmation text of §11.5, shown only where the caller has the class; the server is the enforcement.
- [ ] Error handling: `invalid_reaction`, `too_many_reactions`, `rate_limited` with `retry_after_seconds`, `reaction_not_permitted`, `forbidden` (control hidden after the first refusal is acceptable; the server is the enforcement).
- [ ] Feature detection against an older server: a 404 on the batch read whose body is not a `schema.Error` means no reactions; a `schema.Error` 404 is an ordinary refusal and never hides the controls. Unit tests with fixture-checked generated types; Playwright for press/unpress, two-tab convergence, reconnect.

### #767 — terminal (size M)

- [ ] Read-only first: counts under messages in the TUI view, from the batch route per page; glyph width via the existing width handling; unsupported-glyph fallback (the key's CLDR short name from the Go table, or the code points) documented.
- [ ] Set/remove: `conch react <channel> <message-id> <emoji-or-shortcode>` and `conch unreact …`; TUI key binding on the selected message; shortcode entry resolved client-side (a shortcode table is a dataset decision, §16); `conch reactions <channel> <message-id>` lists reactors, names resolved from `principal_id`; `conch admin reaction remove <channel> <message-id> <principal-id> [<emoji>]` for the moderation routes, with `--yes` for non-interactive use (moderation.md §3.2).
- [ ] Live updates through the events socket where the TUI already has one, by §8.3; otherwise refresh on the next page read; unknown schema frames ignored.
- [ ] Same error handling and feature detection as the browser (a non-`schema.Error` 404 means a pre-reaction server); table-driven tests with the golden fixtures.

## 14. Test matrix

Normative for #765 unless a row names #766/#767. "Isolation" means a row in `e2e/isolation` with the three checks (stated refusal, no marker of A, ghost-identical).

| # | Scenario | Expected | Where |
| --- | --- | --- | --- |
| K1 | Every example of §3.2, including a stray U+FE0F or U+FE0E anywhere in an otherwise listed sequence (`👍️`, `🏳️‍🌈` with a second selector) | Canonical key or `invalid_reaction` exactly as tabled; a stray selector canonicalizes to the listed key; `ValidateReactionKey(canonical(x)) == nil` for every accepted input; idempotent `canonical(canonical(x)) == canonical(x)` | `pkg/schema` table test |
| K2 | Invalid UTF-8 (`\xff`, an overlong or truncated sequence, a lone surrogate encoding); inputs at and over 64 bytes / 16 code points; NUL, controls, bidi, whitespace, `:+1:` | `invalid_reaction`; invalid UTF-8 is refused before the bounds and before any table lookup; the bounds before the lookup; the rest by the failed lookup | schema |
| K4 | Generator run over a synthetic `emoji-test.txt` where two fully-qualified sequences collide once stripped | the generator fails, nothing is written | scripts |
| K3 | Generated catalog vs vendored file hash; `EmojiVersion` vs web `EMOJI_VERSION` | `make check` fails on drift | scripts |
| W1 | Set, set again, remove, remove again (same principal) | 200 each; one row after the first set, none after the first remove; two audit rows total; summary `me` true/true/false/false | store + handler |
| W2 | 50 concurrent sets of one (message, principal, emoji) | one row; every response identical; no error | store, `-race` |
| W3 | Two sessions (two credentials) of one human set and remove interleaved | final rows match last commit; each response's `as_of` ordering is consistent with its state | handler |
| W4 | 21st distinct emoji on a message; 11th reaction by one principal on a message; joining an existing key at the kinds limit | 409, 409, 200 | store |
| W5 | Remove by someone who never reacted | 200, summary unchanged, no audit row | store |
| W6 | Human exceeds 60/min | 429 with `Retry-After` and `retry_after_seconds`; a 429 spends nothing | handler |
| W6b | Refused writes spend a token too: a human sends `invalid_reaction`, `message_not_found`, `reaction_not_permitted` and `too_many_reactions` requests up to the bucket | each refusal takes a token; the next write is 429; a `channel_not_found` before the gate passes takes none | handler |
| W7 | Restart between set and read; count after restart | counts equal rows; `COUNT(*)` matches summary for every emoji | store/restart test |
| V1 | Non-member (member of B) on A's message in A's channel: all five routes | 404 `channel_not_found`; ghost identical | isolation |
| V2 | Member of B names A's message id through B's channel: summary, reactors, set, remove | 404 `message_not_found`; batch read omits; ghost identical | isolation |
| V3 | Whisper: recipient vs non-recipient member of the same channel; set by recipient | recipient sees summary/frame; non-recipient `message_not_found`, no frame, batch omits | isolation + events test |
| V4 | Net message: member, monitor, a member removed from the net since the post, non-net member | member sets; monitor reads but set → 403 `reaction_not_permitted`, remove-own → 200; the removed member still reads the old message but set → 403 `reaction_not_permitted`, remove-own → 200; non-net member `message_not_found` | isolation |
| V5 | Reply in a thread whose root is a whisper | reactions follow the root's audience | store |
| V6 | Operator who is not a member; operator member on an invisible whisper | `channel_not_found`; `message_not_found` | isolation |
| V7 | Agent without `post`, with `read` only: set | 403 `forbidden`, audited; reads succeed; budget spent per manifest | agent rows |
| V7b | Agent with `post` but not `post_net`: set on a net message it receives; agent with `post` but not `whisper`: set on a whisper it received; agent with `post_net` as a monitor | `forbidden`, audited with `denyAudienceGrant`, nothing stored; the monitor gets `reaction_not_permitted`; the same agent sets on a channel-wide message → 200; budget spent once per call | agent rows |
| V8 | Agent without membership | `channel_not_found` | agent rows |
| V9 | Auth off | every route 400 `reactions_require_auth` | handler |
| G1 | Guest: message before `history_after`, reply to an old root, message after | `message_not_found` / absent for the first two; success for the third | guest rows |
| G2 | Guest set carries `guest_reactor`; member sees it; render rule | fixture `list-reactors-response-v1-guest.json`; UI renders `Guest: <label> · <tag>` (#766/#767) | store, web, TUI |
| G3 | Guest ends (idle, hard, End visit, operator); then set/remove/read with the old token; new visit from same browser | all refused at the door; rows retained with attribution; new principal sees nothing of them (bound) and has no `me` | expiry + revoke rows |
| G4 | Conversion with reactions in place; then set, remove, read; hide the member later | same rows, same `me`, no duplicates, bound unchanged, old rows keep guest attribution, new rows under the permanent name; hidden → `hidden-<id>` on new rows only | conversion rows |
| G5 | Guest attempts a 404'd pre-bound reaction via MCP | guests have no MCP (agents only); documented, no test needed beyond `mcp_auth` | — |
| R1 | Set racing membership removal (hook before commit, after commit) | row retained or `channel_not_found`; removed principal's socket receives nothing after the drop | store race test + events |
| R1b | A reaction reference is flushed to the removed principal's events socket after the removal committed but before `dropPrincipal` closed it, for a channel-wide message and for a scoped one | nothing is sent in either case (the member-and-enabled clause finds no row); the socket then closes | events test with the flush hook |
| R2 | Set racing credential revocation / disable / guest expiry | refused at door or by the transaction's enabled/membership check; no partial state | store |
| R3 | Moderator removal racing the reactor's own remove and a fresh set | both deletes idempotent; a later set stands; summaries true at their `as_of` | store race test |
| E1 | Events: subscriber who can see the message, who cannot, agent subscriber, the actor's other socket, a disabled subscriber whose socket has not yet closed | frame / nothing / frame / frame / nothing; each computed with that reader | events test |
| E2 | Ten sets on one message faster than the socket drains | one or few frames, final one whole and correct | events test |
| E3 | Slow consumer | closed at `eventsQueueMax`; re-hydration restores state | events test |
| E4 | No frame on `/v2/ws`, `/v1/ws`, `/v0/ws`; no `Broadcaster` call; no read-state change; no notification | asserted with recording seams | handler tests |
| E5 | Out-of-order delivery (older `as_of` after newer) | client keeps newer (#766/#767 unit tests) | web, TUI |
| E6 | Reconnect after server restart | client re-hydrates; displayed counts equal server rows | Playwright (#766), TUI integration (#767) |
| C1 | Published fixtures byte-identical; new fixtures validate; TS generated and checked | `schema-compat.sh`, `schema-ts-check.sh` green | CI |
| C2 | Old browser build against new server events socket | unknown-schema frame ignored, no error (#766 test with the frame injected) | web |
| C3 | New client against a pre-reaction server; new client getting a `schema.Error` 404 (`message_not_found`) from a current server | the mux's non-`schema.Error` 404 hides the controls, nothing else breaks; the `schema.Error` 404 is shown as a refusal and does **not** hide the controls; over MCP the tools are absent from `tools/list` | web, TUI, MCP test |
| C4 | Migration N-1 → N on a populated database; N opened by an N-1 binary | opens and serves; refused with the existing "newer than this binary" error | store |
| C5 | Catalog upgraded to a newer version with rows stored under the old | every stored key still served and still canonical | schema + store |
| C6 | A message holding more kinds than a lowered `MaxReactionKindsPerMessage` (seeded) | its summary still validates (`MaxReactionKindsOnWire`) and encodes on the events socket | schema + events |
| M1 | (when moderation lands) removed message: summary empty, batch omits, set → 403, remove → 200, frame on removal | as §11.2 | store + events |
| MD1 | Moderation routes: owner, operator and manager who can see the message remove one reaction / all of a principal's; the target's client and others get the frame; audit `reaction_moderated` with the row count; repeat → 200, no audit | as §11.5 | handler + events + isolation |
| MD2 | Moderation routes: plain member → 403 `forbidden`; agent → human-only refusal; B's manager on A's channel → `channel_not_found`; operator non-member → `channel_not_found`; manager on an invisible whisper/net/pre-bound message → `message_not_found`, ghost-identical; unknown target principal → 200, nothing written | as §11.5 | isolation |
| MD3 | Moderation removal of an owner's, an agent's and an ended guest's reaction | removed; no membership, credential or role changes; the ended guest's other rows keep their attribution | store |
| A1 | Audit rows for W1 and MD1; none for W5, reads, refusals; `denyAudienceGrant` rows for V7b | as §11.3; detail has no body and no free text | store |
| P1 | REST and MCP answer the same inputs alike for every error row above | parity table over both transports | handler + MCP tests |

## 15. Choices made

Each is a call the existing designs did not settle; the alternative is recorded so review can overrule cheaply.

1. **Canonical key = fully-qualified sequence; VS15/VS16 are equivalence.** Alternative: store the selector-stripped form. Fully-qualified renders as emoji everywhere without client fix-ups and is what the picker inserts; stripping only on lookup gives the equivalence the issue asks for. VS15 (text presentation) folded in because a reaction is a pictograph, not a glyph choice.
2. **Emoji in the path, no request body.** `PUT`/`DELETE` by resource are naturally idempotent, `DELETE` bodies are poorly supported, and the server canonicalizes whatever form arrives. Alternative: `POST …/reactions {emoji}` + `DELETE …/reactions/{emoji}`, two shapes for one thing.
3. **Write answers return the summary (200), not 201/204.** Convergence (§8.3) wants one document shape; the read-state `PUT` is the precedent.
4. **Batch route, not a `reactions` field on `MessageV2`.** Keeps a published envelope and every socket frame untouched; one aggregate statement per page.
5. **Summary carries counts and `me`, not reactor ids.** Keeps frames small and bounded; the reactor list is one further read on demand. Alternative: up to N ids per entry.
6. **Reactor list ordered by `principal_id`, cursor `after`.** One stable integer cursor, the convention of every list here; a client may sort a page by `created_at` for display.
7. **Agents react with the permission they would need to reply in that audience** (`post` channel-wide, `post_net` on a net, `whisper` in a whisper), after `messages.post`/`post` at the channel gate: the reply precedent (`postReply`, `authorizeScopedPostGates`), so one grant table governs speaking and reacting and a reaction is never a way to transmit into an audience the agent may only read. The principal's decision, recorded in §16.4. Alternative: a new `reactions.write` capability (additive, but a new manifest vocabulary every operator must learn and grant).
8. **A current member seat on the net is required to set a reaction on a net message; removing one's own is always allowed.** Both halves of ADR-005 applied literally: a monitor may not transmit, and a principal taken off a net gets nothing said on it from then on (`audience.go:740-746`).
9. **Reaction rows outlive access (leave, ban, disable, hide, guest end, conversion), and managers and operators can remove them.** Mirrors messages and the guest attribution rule; counts others saw do not change retroactively; the moderation routes of §11.5 answer griefing by a retained row.
10. **Reactions are audited on change, not on no-op.** Visible to others, so an action; the moderation precedent for idempotent repeats; volume bounded by rate limits. Alternative: no audit, as read marks (which nobody else sees).
11. **Human rate limit 60/min, burst 20, per principal; agents spend `messages.post`.** Enough for a person, far below what bloats audit or sockets. Numbers are defaults for review.
12. **Limits 20 kinds per message, 10 per principal per message.** Keeps a summary frame under about 1 KB and a bar renderable in a terminal; the first number is the more product-visible one.
13. **No live replay of reactions; hydrate by batch on (re)connect.** Replay of per-message state would need a cursor the message sockets do not have; the batch route is one request per page and is the same code as the first load.
14. **No notifications, no read-state effect, no agent trigger.** The issue's requirement, taken as a hard rule; author notification is a later additive design.
15. **Moderator removal of one reaction, or all of one principal's on a message, ships in #765** (§11.5), under `accessChannelManager` with moderation.md's content check (the caller must see the message, operators included) and any target. Adopted from the security review: without it a retained row (§15.9) has no finer answer than removing the message or the member. No MCP tool, as for every administration operation.
16. **`message_not_found` shared with moderation.md.** One code, defined once, same meaning (unknown, other channel, invisible).
17. **Auth-off refuses every reaction route.** A reaction needs a principal for uniqueness; reads could be served channel-wide, but one rule is simpler and matches read state.
18. **Rollback = backup restore, as for every migration.** No runtime switch added; listed as a possible additive follow-up.
19. **A fixed wire bound (`MaxReactionKindsOnWire` = 100) for `Validate`, separate from the write limit.** Lowering the write limit later must not make an existing message's summary fail to encode and close the events sockets that carry it. Alternative: pin that `MaxReactionKindsPerMessage` may only grow, which is a rule nobody enforces.
20. **`ReactionV1` carries the reactor's id only, no name or kind.** A message names its author by id; carrying a current name would serve a former member's name through a reaction and duplicate what the member list already gives. `guest_reactor` keeps guest rendering.
21. **Refused writes spend a human's token; only a 429 does not.** Probing ids or keys must cost what reacting costs; the invite limiter's refund is a property of its shared server-wide bucket, which a per-principal bucket does not have.
22. **The catalog and schema are their own slice** (§13 item 0), filed as #953, which #763 and #765 both depend on, so neither client nor server PR owns the dataset by accident of ordering.

## 16. Decisions needing Nick

None of the wire changes is a schema version bump, and nothing here touches the approval path. Listed for the record (ADR-000 D8, the `schema-change` skill):

1. **Compatible new schema** (normally fine without him): types `ReactionSummaryV1`, `ReactionCountV1`, `ReactionSummariesResponseV1`, `ReactionV1`, `ListReactorsResponseV1`; constants `EmojiVersion`, the limits and the fixed `MaxReactionKindsOnWire`; error codes `message_not_found`, `invalid_reaction`, `reaction_not_permitted`, `too_many_reactions`, `reactions_require_auth`; events-socket frame `conch.reaction_summary.v1`; four MCP tools mapped to existing capabilities in `mcpToolCapabilities`; `ts.go` gaining scalar-constant output. No published fixture changes. **Not a version bump.**
2. **Vendored Unicode data and its licence.** `emoji-test.txt` (Unicode-3.0 licence) checked into the repository and generated into Go and JSON by the catalog-and-schema slice (§13 item 0). Not a `go.mod` or npm dependency, but a new licence in the tree; one sign-off covers that slice, #763 and #765.
3. **Product call: reactions outlive access, plus manager removal** (§9, §11.1, §11.5), including a guest's after End visit and a removed member's, with channel managers and operators able to remove one reaction or all of one principal's on a message they can see, audited. Recommended as written: retain, as messages are retained, and give moderators the finer lever. The alternative is deleting on leave, which the design argues against.
4. **Recorded, decided by the principal: agents react with the permission they would need to reply in that audience** (`post`, `post_net`, `whisper`; §6.2, §15.7) rather than under a dedicated capability. Listed so Nick can overrule; a dedicated capability is an additive vocabulary change if preferred.
5. **If #767 wants shortcode entry in the terminal**, the shortcode table is a dataset with its own licence (for example gemoji's, MIT), which would be a new vendored data file. Not needed for #765 or #766. Flag only.
6. **Approval path:** no change claimed; #765 is labelled `security` (new authenticated write surface, new moderation routes and visibility statements) and gets a `security-reviewer` pass, but is not `approval-path`. If review finds otherwise, Nick merges.

## 17. Tensions in the guest and account design, recorded

No contradiction was found that this design had to resolve silently. Two places needed an interpretation, recorded here rather than decided in code:

1. **"Ending a visit … invalidates cursors/subscriptions" (§4.3) versus "Posted messages … remain" (§4.4).** Per-principal state is split by kind: content others see (messages, now reactions) stays; private cursors (read marks, deleted by #719) go. A reaction is treated as content. If the owner reads §4.3 as "everything the guest wrote that is not a message goes", §9 changes to delete rows at End visit; nothing else in this design moves.
2. **"A guest that posted nothing can have transient profile data cleaned promptly" (§4.4).** A guest with a reaction but no message has "posted" for this rule; its attribution record is kept. The alternative reading (only messages count) would delete the reaction's `guest_handle`/`guest_label` and render it as a bare principal id, which contradicts the immutable-attribution rule for the only thing of the guest's that other people can still see.

A third observation is not about reactions but worth a note for the guest design's owner: `ChannelMemberV1.HistoryAfter` documents "absent, or zero, is no bound", while `withinHistoryBound` makes a guest with a `NULL` bound see nothing and one with bound `0` see everything after id 0, that is, everything. The two agree in effect (0 is a bound at zero; `NULL` is "no provable bound, refuse"), but the wire comment could say so explicitly so a reader does not take `0` and absent to mean the same as `NULL` in the store.

## 18. Review record and remaining gates

Precedent: guests-and-accounts.md §11. This section says what each review round found, what was adopted, and what still gates implementation.

**Round 1 (PR #952, 2026-10-28).** Two reviews of the first draft, commit `ae27cbc`.

- *Security reviewer: changes requested.* Six should-fixes, all adopted in this revision: (1) a current member seat on the net is required to set a reaction on a net message, not only non-monitor status, so a principal taken off a net cannot react on its old messages (§1, §4.3, §6.2, §7.2, V4, §15.8); (2) agents follow the reply precedent, needing the audience permission (`post`/`post_net`/`whisper`) once the message row is read, refused with `forbidden` and `denyAudienceGrant` (§1, §4.1, §4.2, §6.1, §6.2, V7b, §15.7, §16.4); (3) a human's bucket token is taken when the channel gate passes and is never refunded, so refusals spend (§6.1, §7.4, W6, W6b, §15.21); the earlier text misread `invite_limits.go`, whose refund is the server-wide redeem bucket's only; (4) retained rows get a moderation lever: two `accessChannelManager` removal routes with their own audit action, in #765 (§1, §4.1, §6.2, §7.1, §11.1, §11.3, §11.5, §13, MD1–MD3, R3, §15.9, §15.15, §16.3); (5) `messageVisible` checks membership and enabled only for scoped messages, so every reaction statement adds the explicit member-and-enabled clause in the `runVisibleHead` form, and a reference flushed between a removal's commit and the socket's drop sends nothing (§1, §2, §6.1, §7.2, §8.1, §10, R1b, E1); (6) `ReactionV1` carries no name or kind; clients resolve `principal_id` as they resolve `author_id` (§5, §6.4, §9, §15.20). Nits adopted: the generator fails on a stripped-sequence collision (§3.1, K4); `utf8.ValidString` first (§3.2, K2); a fixed `MaxReactionKindsOnWire` for `Validate` so a lowered write limit cannot break encode (§5, C6, §15.19); the tool→capability map is `mcpToolCapabilities` in `pkg/schema/agent_manifest.go` (§2, §4.2, §13); canonicalization after the channel gate and before the store, alike for REST and MCP (§4.1, §6.1, §13); `GET /v1/audit` is owner-visible (§11.3).
- *General reviewer: approved with should-fixes*, all adopted: the enforcing test is `TestOnlyAudienceGoReadsMessageRows`, textual, so every statement naming `messages` or `message_recipients` lives in `audience.go` (§2, §6.1, §13); `ts.go` emits no scalar constants today, so emitting `EmojiVersion` and the bounds is new work (§2, §3.1, §5, §13); old-server detection is a 404 whose body is not a `schema.Error`, and for MCP the tool's absence from `tools/list` (§12, §13, C3); §3.2 rule 5 exempts the format characters inside listed sequences and refuses the rest by the failed lookup (§3.2). Nits adopted: stray selectors (§3.2, K1), the longest-sequence wording (§3.3), the `guests-and-accounts.md §4.4` reference (§5), `ChannelMembersV1` dropped from the no-schema sentence (§5), heartbeat frames on `/v2/ws` (§2, §8.1), ordering may move a key when its earliest reaction is removed (§5), clients percent-encode `#` (§4.1), one transaction with one probe then the aggregate (§6.1), an absent batch id is an empty summary (§4.1, §8.2), the migration is the next entry at merge time (§2, §7.1, §12, C4), one line on #699 (header), this section. The principal accepted the split of the catalog and schema into their own issue (header, §3.1, §13 item 0, §15.22).

**What still gates #765.**

- The security reviewer's re-read of this revision (the amended text, not the first draft, is what a PASS would cover).
- Nick's §16 items: the Unicode-3.0 data licence (16.2) and the two product calls (16.3, 16.4), recommended as written; 16.1 and 16.5 are listed for the record.
- The catalog-and-schema issue (§13 item 0) filed by the principal and landed, since #763 and #765 both build on its generated table, types and fixtures.
- Whichever of this design and moderation.md lands first defines `message_not_found` and takes the next migration number; the other adapts (§2, §4.3, §11.2).

Nothing in this revision has been exercised as software: no generator, store, route or client exists yet, and the behaviour stated here is a contract for #765–#767 to prove with the matrix of §14.
