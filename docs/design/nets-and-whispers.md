# Design: nets and whispers — audience-scoped speaking

- **Status:** V2 schema landed (#114): `conch.message.v2`, the audience, net, and manifest-grant shapes are in `pkg/schema`. Store, visibility predicate, REST, CLI/TUI and MCP are the rest of V2 (governing decision: [ADR-005](../adr/ADR-005-nets-and-whispers.md), Accepted).
- **Touches:** D11 (no team abstraction), D12 (no DMs before P6/P7), D8 (typed messages), D10 (capabilities) in [ADR-000](../adr/ADR-000-charter.md); the nets-and-voice track in [ROADMAP.md](../../ROADMAP.md).
- **Owner:** protocol-designer (schemas), server-engineer (visibility enforcement)
- **Issue:** #82

Today every message in a channel goes to everyone in the channel. This note proposes that every utterance — a text message or an audio track — carries an **audience**: the whole channel, a named subset (a **net**), or an ad-hoc list of principals (a **whisper**). The model is military radio nets: you monitor several nets at once and transmit on exactly one.

## 1. Problem

The motivating case: two teams of four in one operation. Each team needs its own squad comms, the two leads need a command layer, and anyone may need to say something to one specific person. Discord has no layering; TeamSpeak and Mumble have whisper targets but they are per-user hotkey configuration, not a property of the room.

For Conch the same shape appears with agents before it appears with voice:

- **P4 assemblies** put several agents on one shared log. With whole-channel audiences only, every agent reads every other agent's turns (context cost grows with participants × turns), and two teams cannot deliberate in parallel with leads reporting up.
- **Directed instruction** to one agent means either a separate channel or telling the whole room.
- **Voice:** nobody can usefully sit in three voice rooms at once, so the "separate channel per team" workaround does not survive the move to voice.

Separate channels express *separation*. They do not express *layering*: one participant monitoring several scopes, transmitting on one, with one log, one floor/spend account, and one audit trail.

## 2. Model

| Term | Meaning |
|---|---|
| **Audience** | The set of principals an utterance is delivered to. The author is always in it. |
| **Net** | A named, persistent subset of a channel's participants. Belongs to exactly one channel. |
| **Member** | A principal on a net who listens and may transmit. |
| **Monitor** | A principal on a net who listens only. |
| **Whisper** | An utterance whose audience is an explicit list of principals, with no net. |
| **Transmit target** | Where a principal's next utterance goes: the channel, one net, or a whisper list. Exactly one at a time. |

Nets are **flat sets**, not a tree. Layering comes from overlapping membership:

```text
channel: op            (a1 a2 a3 a4 b1 b2 b3 b4)
  net: alpha           (a1 a2 a3 a4)
  net: bravo           (b1 b2 b3 b4)
  net: command         (a1 b1)          ← the two leads bridge
```

`a1` hears `op`, `alpha`, and `command`, and picks one to speak on. `a3` whispering to `b2` needs no net at all.

**Visibility is fixed at post time.** Joining a net later does not reveal its history; leaving does not hide what was already received. This matches voice (nobody hears retroactively), makes the audit record exact, and avoids membership edits silently changing who can read old messages.

## 3. Wire shape

Canonical Go types live in `pkg/schema/message_v2.go` and `pkg/schema/net.go`; fixtures are `pkg/schema/testdata/message-v2*`, `post-message-request-v2*`, `post-message-response-v2.json`, `list-messages-response-v2.json`, `net-v1*`, `create-net-*`, `list-nets-*`, `put-net-member-request-v1.json`, and `agent-manifest-v1-nets.json`. This section is the semantic contract they satisfy.

### 3.1 Audience

Type `Audience`, field `audience` on the envelope and the post request. `kind` (`AudienceKind`) is a closed enum of two values; exactly one of the other fields is set, selected by the kind. **Absent means the whole channel** — today's behavior. There is no `"kind": "channel"` on the wire.

```json
{"kind": "net", "net_id": 3}
{"kind": "principals", "principal_ids": [3, 4, 7]}
```

| Rule | Where enforced |
|---|---|
| `kind` is `net` or `principals`; anything else fails validation. An audience a reader does not understand must never widen delivery (fail closed). | `Audience.Validate` |
| `net` carries a positive `net_id` and no `principal_ids`. The recipients resolved at post time are in the audit log, not on the wire. | `Audience.Validate` |
| `principals` carries 1 to 64 (`MaxAudiencePrincipals`) positive, distinct `principal_ids` and no `net_id`. | `Audience.Validate` |
| On a message read back, `principal_ids` is the **full audience including the author**, sorted ascending, no duplicates, with at least one principal other than the author. A client replies in kind by reusing the list. | `MessageV2.Validate` |
| On a post request the list may be in any order and may omit the author; the server adds the author and sorts with `Audience.Normalize(authorID)` before validating and storing. The normalized list must still fit in 64. | `PostMessageRequestV2.Validate`, then the server |

### 3.2 Message envelope `conch.message.v2`

Type `MessageV2` (`MessageSchemaV2`). It is `conch.message.v1` plus the optional `audience`, placed last; every other field, its order, and its rule is unchanged. A new version rather than an additive v1 field: audience changes delivery semantics, and a v1 client that ignored the field would render a whisper as an ordinary message and invite a reply in the open. `MessageV1` and its fixtures are not edited.

```json
{
  "schema": "conch.message.v2",
  "id": 45,
  "channel_id": 7,
  "author_id": 3,
  "created_at": "2026-10-08T12:00:01.000Z",
  "body": "a word to the two of you",
  "audience": {"kind": "principals", "principal_ids": [3, 4, 7]}
}
```

A channel-wide v2 message is the v1 document with only the schema name changed. `MessageV2FromV1` performs that conversion and `MessageV1FromV2` inverts it; the latter rejects a scoped message, because it has no v1 form. A v1 decoder given a v2 document fails `Validate` on the schema name.

| Shape | Type | Notes |
|---|---|---|
| `POST /v1/channels/{channel}/messages` body | `PostMessageRequestV2` | `PostMessageRequestV1` plus `audience`. `author_id` may be omitted; the server binds it to the authenticated caller. |
| its response | `PostMessageResponseV2` | `{"message": MessageV2}` with the normalized audience. |
| `GET /v1/channels/{channel}/messages` | `ListMessagesResponseV2` | `{"messages": [...], "next_after": N}`, only messages visible to the caller (§4). |

The v1 message routes are unchanged. Whether v2 replaces them at the same paths or lands beside them is a routing decision for the server slice; the shapes do not depend on it.

### 3.3 Nets

Type `NetV1`. A net has a server-assigned `id` (the value `audience.net_id` references), belongs to exactly one `channel_id`, and has a `name` unique within its channel. `members` is always an array, ordered by `principal_id`; each entry is `{principal_id, role}` with `role` (`NetRole`) one of `member` (listens and transmits) or `monitor` (listens only). Nets are flat; there is no parent field.

```json
{
  "id": 3,
  "channel_id": 7,
  "name": "alpha",
  "members": [
    {"principal_id": 3, "role": "member"},
    {"principal_id": 4, "role": "member"},
    {"principal_id": 9, "role": "monitor"}
  ],
  "created_at": "2026-10-08T11:00:00.000Z"
}
```

**Net names** are 1 to 32 characters from `a-z`, `0-9`, `-` and `_`, starting with a letter or digit (`ValidateNetName`). They appear in URL paths, CLI flags and the TUI prompt, so nothing else is accepted. Channel names are not governed by this rule.

| Shape | Type | Notes |
|---|---|---|
| `POST /v1/channels/{channel}/nets` body | `CreateNetRequestV1` | `{"name": "alpha"}`. A net starts with nobody on it; members are added one request at a time, so each change is audited on its own. |
| its response | `CreateNetResponseV1` | `{"net": NetV1}` |
| `GET /v1/channels/{channel}/nets` | `ListNetsResponseV1` | `{"nets": [...]}`, always an array, ordered by id. |
| `PUT /v1/channels/{channel}/nets/{net}/members/{principal_id}` body | `PutNetMemberRequestV1` | `{"role": "monitor"}`; the principal is in the URL, as for channel members. Adds or changes the role. |

Removing a member is a `DELETE` on the same path with no body. Nets have no schema-name field; like the channel-member shapes they are REST bodies versioned by the `V1` type suffix.

### 3.4 Manifest grants

Three new `ChannelPermission` values in `conch.agent_manifest.v1`, granted per channel ([agent-manifest.md §4](agent-manifest.md#4-channel-permissions)): `post_net`, `whisper`, `whisper_agent`. The existing `post` keeps meaning a channel-wide post; reading a scoped message needs only `read` plus being in the audience. This is a compatible addition — every existing manifest fixture is byte-identical.

## 4. Visibility enforcement

Server-side and fail closed, in the manner of D10: a principal outside the audience does not receive the message on any path. This is the main cost of the proposal and its main risk — one unfiltered read path leaks whispers.

The filter belongs in **one store-level predicate** ("visible to principal P"), not in each handler. Paths that must use it:

| Path | Today | Change |
|---|---|---|
| REST message list | Unfiltered, unauthenticated | Filter by caller |
| WS hub fan-out (`internal/server/hub`) | Per-channel subscription, no principal identity | Subscriptions carry a principal; broadcast checks audience |
| MCP `read_channel` | Any token reads any channel | Filter by the token's principal |
| FTS search (#22), audit export (#21), threads (#25) | Not built; scheduled for P2 | Must be retrofitted — they land before this |
| Webhook and ntfy fan-out | Channel-wide | Scoped messages are not pushed outside their audience |

**Hard prerequisite:** authenticated readers. Without local auth (#24) and bound agent credentials with channel enforcement (#77–#79), an audience is a rendering hint, not a boundary. Nothing here starts before those close.

## 5. Audit and oversight

- **Discretion, not secrecy.** There is no E2EE (standing non-goal). Every scoped message is in the audit log with its audience and its resolved recipient list. Clients must say so wherever a whisper is composed.
- **Agent-only audiences are allowed.** A net or whisper may consist entirely of agents, so layered comms can be exercised with AI participants alone. Oversight is the audit log, not a human in every audience — which makes a way to *read* the audit log (export, #21) a practical prerequisite.
- **Human-to-human scoping is real.** With several humans on an instance ([ADR-003](../adr/ADR-003-multi-human-access.md)), a human outside the audience does not receive the utterance. The audit log remains the complete record.
- **What cannot be enforced:** the server controls who *receives* a message, not what an agent repeats afterward. A whisper to an agent is discretion only.

## 6. Capabilities

Expressed in the D10 manifest, granted per channel. The vocabulary is fixed in `pkg/schema/agent_manifest.go` (§3.4, [agent-manifest.md §4](agent-manifest.md#4-channel-permissions)):

- `post_net` — transmit on a net in the channel that the agent is a member (not merely a monitor) of.
- `whisper` — post to an explicit list of human principals; `whisper_agent` additionally allows agents on the list. Agent-to-agent whispering is a separate grant because it is the same risk the roadmap's `reply-to-agent` capability exists to gate.
- Create or edit nets: humans only in the first cut; there is no agent permission for it.

Denials are protocol errors and are audited, the same as any other capability denial (#79).

## 7. Assemblies (P4)

- In `moderated` and `assembly` modes, **each net has its own floor token**, so two teams take bounded turns in parallel.
- The round cap is per net; the spend cap is assembly-wide, summed across nets.
- A lead reporting on `command` takes a turn on that net's floor, not its squad's.
- Agents may not whisper while an assembly is in session, except the facilitator.

## 8. Voice

Governed by [ADR-004](../adr/ADR-004-voice-via-livekit.md). The client is `conch-voice` ([ADR-006](../adr/ADR-006-rust-voice-client.md)).

- A net is one LiveKit room; only its members hold tokens for it (ADR-004, spike findings). How a whisper maps onto rooms is still open.
- One push-to-talk key per transmit target (squad, command, whisper).
- Simultaneous monitoring needs priority ducking (command over squad) and per-net volume. Per-net stereo placement — squad in one ear, command in the other — is the usual radio answer and is cheap to add.
- Transcripts posted as typed messages carry the same audience as the audio they transcribe.

## 9. Client surface

API parity (D6): nets and audiences exist in REST/WS first.

- **REST:** create, list, and edit nets on a channel; the audience field on post and read.
- **CLI:** `conch send --net alpha ops "..."`, `conch send --to 4,7 ops "..."`.
- **TUI:** `/net alpha` sets the transmit target; `/w @name ...` whispers once. The current target is always shown in the input prompt, and scoped messages carry a badge — mis-sending to the wrong scope is the classic failure of this feature.
- **MCP:** `post_message` and `read_channel` gain the audience. Adapters default to replying in kind.

## 10. Alternatives considered

| Option | Why not |
|---|---|
| Separate channels per team plus a command channel | Works today at zero schema cost. Splits the log, floor, and spend accounting; no single view; does not carry to voice. |
| Threads as nets (#25) | Threads scope a *topic*, not an audience — everyone in the channel can read them. |
| DMs and group DMs (P6/P7) | Detached from the channel's context and floor; no layering. |
| Unit tree with derived nets | More expressive, more to get wrong. Flat sets cover the motivating case; a tree can be layered on as presentation later. |

## 11. Phasing

Track phases from [ROADMAP.md](../../ROADMAP.md):

1. **V1 (prerequisite):** authenticated principals and channel membership. No work on this design.
2. **V2:** nets, whispers, the store-level visibility predicate, REST → CLI/TUI → MCP.
3. **V5:** the same audiences in voice.
4. **P4:** per-net floors in assemblies.

## 12. Decided questions

Recorded in ADR-005: flat nets; visibility fixed at post time; new envelope version; whispers ship with nets; no agent whispers during an assembly except the facilitator.

## 13. Out of scope

- Audience-scoped approval objects. Approvals stay channel-wide; this proposal does not touch the approval path.
- Any confidentiality guarantee against the server operator or the audit log.
- Cross-channel nets. A net lives in one channel.
- Positional (in-world 3D) audio.
