# Design: voice control plane

- **Status:** Adopted for V3 implementation, 2026-10-09 (governing decision: [ADR-004](../adr/ADR-004-voice-via-livekit.md), Accepted). §10 records the LiveKit behaviours it relies on, as measured.
- **Touches:** [ADR-002](../adr/ADR-002-single-binary-sqlite.md) (optional processes degrade gracefully), [ADR-003](../adr/ADR-003-multi-human-access.md) (authenticated humans, channel membership), [ADR-005](../adr/ADR-005-nets-and-whispers.md) (audiences), [ADR-006](../adr/ADR-006-rust-voice-client.md) (the client).
- **Owner:** protocol-designer (schemas), server-engineer (control plane)
- **Issues:** epic #122, this note #123
- **Evidence:** the V0 spike, summarised in ADR-004 "Spike findings".

ADR-004 decides that `conchd` is the only authority on who may speak and who may hear, that LiveKit only moves audio, and that there is one LiveKit room per net. This note says how `conchd` does that: configuration, rooms, session tokens, live enforcement, presence, and audit. It covers the server only. The client, `conch-voice`, is V4.

## 1. What V3 ships, and what it does not

V3 ships channel-wide voice control: a member of a channel asks `conchd` for a voice session, receives a short-lived token for that channel's room, and joins. `conchd` knows who is connected and who is transmitting, shows it, and logs it. Removing someone from the channel removes them from the room.

Not in V3:

- **Nets and whispers in voice.** The model below already has one room per audience, so nets become more rooms when V2 has landed and V5 turns them on. Whisper mapping is still open (ADR-004).
- **Agents in voice.** ADR-004 defers them. An agent credential is refused.
- **Any audio through `conchd`.** It never touches media and records none.

## 2. Configuration and degradation

| Setting | Source | Meaning |
|---|---|---|
| `--livekit-url` / `CONCHD_LIVEKIT_URL` | flag or env | The `ws://` or `wss://` address clients connect to. Returned to clients as is. |
| `--livekit-api-url` / `CONCHD_LIVEKIT_API_URL` | flag or env | The `http://` or `https://` address `conchd` calls for room control. Defaults to the client address with the scheme swapped. |
| `CONCHD_LIVEKIT_API_KEY`, `CONCHD_LIVEKIT_API_SECRET` | env only | The key pair `conchd` signs with. Never a flag, so it does not appear in the process list. Never logged. |

Rules:

- **All or nothing.** With none of these set, voice is *not configured*. With some but not all, `conchd` refuses to start and names what is missing.
- **Startup never waits for LiveKit.** `conchd` does not contact it until a voice request arrives.
- **Not configured:** every voice endpoint answers 503 `voice_not_configured`, but only to a caller who has already passed authentication and the channel membership check. Someone who is not a member gets the same 404 as for an unknown channel and learns nothing about voice.
- **Configured but unreachable:** the session endpoint answers 503 `voice_unavailable`; presence reports `available: false`; the poller (§6) backs off from 1 s to 30 s.
- **Nothing else changes in either case.** Messaging, approvals and audit do not depend on LiveKit (ADR-002). This is tested by running the approval dogfood with LiveKit configured and down.

`conchd` uses no LiveKit Go module. Tokens are HS256 JWTs and room control is JSON over HTTP, both from the standard library, as the spike showed.

## 3. Rooms

One LiveKit room per audience. In V3 the only audience is the whole channel.

- `conchd` names rooms. A name is `conch-` followed by 128 random bits in base32. It is never derived from a channel or net name.
- The mapping is stored: `voice_rooms (id, channel_id, net_id, room_name, created_at)`, `net_id` NULL for the channel-wide room, one live row per audience.
- A room row is created the first time a session is requested for its audience, and kept. The table covers channel and net audiences; whisper mapping is undecided (ADR-004) and will extend it when it is.
- **`conchd` calls LiveKit's `CreateRoom` on every session request**, before it signs a token. LiveKit's rooms are not durable: it closes an empty room after a timeout and forgets all rooms when it restarts. The stored name is the durable thing, and `CreateRoom` on an existing room changes nothing.
- The room name is not a secret; the signed token is the gate. It is unguessable anyway so that a mis-set LiveKit cannot be probed by name.

**Deployment requirement:** LiveKit should run with automatic room creation off, so a room exists only because `conchd` made it. `conchd` cannot verify that setting; the docs must state it.

## 4. Sessions

`POST /v1/channels/{channel}/voice/session`, authenticated.

- The caller must be a human member of the channel. A non-member gets 404 `channel_not_found`, as everywhere else. An agent gets 403 `forbidden`, audited.
- Voice needs a verified caller. With `--auth off` the endpoint answers 400 `voice_requires_auth`.
- The response is a `VoiceSessionResponseV1` (`pkg/schema/voice.go`): the client address, the caller's LiveKit identity, and one `VoiceRoomGrant` per room the caller may join. `rooms` always has at least one grant. A grant's `audience` is the ADR-005 type; absent means the whole channel, as on messages. This is the golden fixture `pkg/schema/testdata/voice-session-response-v1.json`:

```json
{
  "livekit_url": "wss://voice.example",
  "identity": "p7",
  "rooms": [
    {
      "room": "conch-k3m7q2x9v4t1b8n6w5z0r2c4e6",
      "token": "fixture-token-channel-7-not-a-real-credential",
      "can_publish": true,
      "expires_at": "2026-10-09T12:00:45.000Z"
    }
  ]
}
```

- `room` and `token` are opaque strings: the schema requires them to be present and never inspects them. `can_publish` is always written out, so a reader never has to know that an omitted value means false.

- **Identity** is `p<principal id>`. One principal holds one connection per room; LiveKit replaces an earlier connection with the same identity. That keeps presence unambiguous. A second device takes over from the first.
- **Token grant:** join that one room; subscribe; publish a microphone track only if `can_publish`; no data publishing; no room administration.
- **The token expires 15 seconds after it is issued, and can start a connection for about 75.** LiveKit checks a token when a client connects, not afterwards, and it allows 60 seconds of leeway on both the expiry and the not-before time (§10, finding 6). So the expiry `conchd` writes is deliberately short: the leeway cannot be turned off from `conchd`, and it is added on top. Clients ask for a session immediately before connecting and again before any reconnect.
- **Clock skew.** The same leeway means the two clocks may differ by up to about a minute in either direction before a fresh token is refused, so the not-before time is not backdated. `conchd` and LiveKit normally share a host; on separate hosts their clocks must be synchronised, and the deployment docs say so.
- With nets (V5), a member of a net gets a publishing grant for its room and a monitor gets a listen-only grant. The shape above does not change; each net grant carries the `audience` that ADR-005 defines (`{"kind": "net", "net_id": 3}`), with `can_publish: false` for a monitor. The fixture `voice-session-response-v1-net.json` shows a channel grant beside a listen-only net grant.

Each issued session writes one `voice_session_issued` audit event. The token is never logged or audited.

## 5. Live enforcement

A token cannot be revoked, and an established connection outlives it. So `conchd` enforces changes itself:

- When a principal is removed from the channel, disabled, or has all credentials revoked, `conchd` calls `RemoveParticipant` for each of that principal's rooms. With nets, the same happens on leaving a net.
- The poller (§6) compares every participant in every room with current membership on each pass, and removes anyone who should not be there. This closes the gap left by a token issued in the 75 seconds before a removal.
- Enforcement does not depend on `conchd`'s memory. After a restart the poller's first act is a sweep of every stored room (§6), so someone removed while `conchd` was down, or whose removal call failed just before it stopped, is still found.

**Residual exposure, stated plainly:** a principal removed while holding a token under 75 seconds old can rejoin and hear audio until the next poll, at most about one second. Rotating the room name on every membership change would close that completely at the cost of reconnecting everyone; it is not proposed for V3.

A failed `RemoveParticipant` is retried on the next pass and logged. If LiveKit is unreachable, `conchd` cannot enforce; presence says `available: false` and an audit event `voice_enforcement_unavailable` is written once per outage.

## 6. Presence

`conchd` learns who is connected and who is transmitting by **polling** LiveKit's `ListParticipants` for rooms that are in use: any room with a session issued in the last two minutes or a participant on the previous pass. The interval is 500 ms while in use.

"In use" is held in memory, so it is rebuilt rather than trusted: **at startup, and every 30 seconds after, the poller sweeps** by asking LiveKit for its room list once and marking every stored room that LiveKit currently has as in use for the next pass. The room list's participant count is not used: it lags a join by seconds (§10, finding 5), whereas a room exists in LiveKit only while someone is in it or for a short timeout after, so existence is the reliable signal and `ListParticipants` gives the truth. A room joined with a token issued before a restart is therefore picked up at startup, and nothing can stay connected unobserved for more than 30 seconds. With no room in use, the sweep is the only call `conchd` makes.

Rooms are polled concurrently, at most eight at a time, and a pass that has not finished is not started again. This is sized for one self-hosted instance with tens of rooms, which is what Conch is (ADR-002).

Why polling and not LiveKit webhooks:

- Push-to-talk is a mute and unmute of a track that stays published. LiveKit's webhooks report publishing and unpublishing but have no mute event (its [webhook reference](https://docs.livekit.io/home/server/webhooks/) lists `track_published` and `track_unpublished` only; muting is a client-side SDK event), so they would not show who is talking. Publishing and unpublishing on every key press would, but takes long enough to clip the first word.
- A webhook receiver is another unauthenticated-by-bearer ingest route to secure, and makes LiveKit need a route back to `conchd`. Polling keeps every call in one direction.
- The same pass does the enforcement check in §5.

What `conchd` reads from each pass: the participants of the room, and for each whether its microphone track is published and unmuted. That is "transmitting".

Surfaces:

- `GET /v1/channels/{channel}/voice` returns a snapshot: whether voice is configured and available, and for each room the caller may see, its participants with `transmitting` and `joined_at`.
- `GET /v1/voice/ws?channel=` streams the same snapshot, as a `conch.voice_presence.v1` document, whenever it changes. A new route, because the existing sockets carry bare message envelopes and a client decoding those must not meet another type.

Both require an authenticated **human member** of the channel, checked exactly as on the message routes: no credential is 401, a non-member gets the unknown-channel 404, and a presence socket is closed when its subscriber loses membership. Agent callers are refused with 403 in V3. ADR-004 defers agents in voice, and whether an agent may see who is talking is part of that decision, not this one.

A snapshot never contains a room name or a token, so reading presence gives nothing that helps join a room.

A snapshot is the whole state, not a delta, so a client that misses one loses nothing.

## 7. Audit

| Event | When | Detail |
|---|---|---|
| `voice_session_issued` | a session is returned | channel, rooms granted, publish or listen |
| `voice_joined`, `voice_left` | a participant appears or disappears between passes | channel, audience |
| `voice_transmit_started`, `voice_transmit_stopped` | the microphone track becomes unmuted or muted between passes | channel, audience |
| `voice_participant_removed` | `conchd` removes someone | channel, audience, reason |
| `voice_enforcement_unavailable` | LiveKit becomes unreachable while rooms are in use | — |

The actor is the principal. No audio and no token is ever recorded.

**Resolution limit, stated plainly:** these events are observed by polling, so their times are accurate to one interval, and a transmission shorter than the interval between two passes can be missed entirely. ADR-004 says audit "records join, leave, and transmit start/stop". Polling alone is a weaker guarantee than those words read strictly, so it is not the whole answer: from V4 the client also reports each press, and the poller checks the reports (§9, decision 1). Events the poller writes carry `source=observed` so that reported ones can be told apart.

## 8. Wire shapes

Canonical types live in `pkg/schema/voice.go` (#124), added through the `schema-change` skill. All are new types; nothing published changed.

| Type | Wire name | Where it travels |
|---|---|---|
| `VoiceSessionResponseV1` | REST body, versioned by suffix | `POST /v1/channels/{channel}/voice/session` (§4) |
| `VoiceRoomGrant` | element of `rooms` above | one per room the caller may join |
| `VoicePresenceV1` | `conch.voice_presence.v1` (`schema.VoicePresenceSchemaV1`) | `GET /v1/channels/{channel}/voice` and the presence socket (§6) |
| `VoicePresenceRoom` | element of `rooms` in presence | one per room the caller may see: optional `audience`, `participants` |
| `VoiceParticipant` | element of `participants` | `principal_id`, `can_publish`, `transmitting`, `joined_at` |

Both `audience` fields reuse `Audience` from the V2 schema (#114); absent means the whole channel. The error codes `voice_not_configured`, `voice_unavailable` and `voice_requires_auth` are the constants `ErrorCodeVoiceNotConfigured`, `ErrorCodeVoiceUnavailable` and `ErrorCodeVoiceRequiresAuth`, carried in the existing `Error` body.

What the schema enforces, so that handlers and clients do not restate it:

- Session: non-empty `livekit_url` and `identity`; at least one grant; each grant has a non-empty `room` and `token`, an `expires_at`, and a well-formed `audience` if present.
- Presence: the schema name; a positive `channel_id`; `available` only when `configured`; `rooms` empty unless `available` (participants last seen are not reported while LiveKit is unreachable); within a room, positive `principal_id`s with no principal listed twice, `joined_at` set, and no participant `transmitting` without `can_publish`.
- One grant per audience in a session and one room per audience in presence, so a reader can always tell which is the channel's room. An unknown audience kind fails the whole document, as it does for a message; a new kind is a new version of these shapes.
- Presence carries no token and no room name. `voice_test.go` asserts this on the marshalled JSON (no key `token` or `room` at any depth) and on the Go types by reflection (no field `Token`, `Room` or `RoomName` reachable from `VoicePresenceV1`), so a future field cannot smuggle one in.
- Empty `rooms` and `participants` encode as `[]`, never `null`.

Golden fixtures in `pkg/schema/testdata/`: `voice-session-response-v1.json`, `voice-session-response-v1-net.json`, `voice-presence-v1.json`, `voice-presence-v1-unavailable.json`, `voice-presence-v1-not-configured.json`.

API parity (CLAUDE.md rule 4): `conch voice status <channel>` prints the snapshot. Joining is `conch-voice`'s job in V4, using the same session endpoint.

## 9. Decisions on the open questions

Nick delegated these to the principal engineer on 2026-10-09 ("use your best judgment"). They are recorded here with their reasons, and he can reopen any of them.

1. **Transmit audit: polling in V3, plus client reports from V4.** Polling alone can miss a press shorter than the interval, which is weaker than ADR-004's "audit records transmit start/stop" read strictly. V3 has no real client, only a headless test participant, so polling is all it can use and all it needs. From V4, `conch-voice` reports each press and release to `conchd`, which audits them with exact times. The poller keeps running as the check on the client: a transmission it observes with no matching report is audited as `voice_transmit_unreported`. The result is exact records for the official client, and detection at poll resolution of anything a modified client leaves out. What remains unrecorded is a burst shorter than the interval from a client that was altered not to report it. Granting and revoking publishing on every press was rejected: it would make `conchd` exact against any client, but it adds a round trip to every push-to-talk and clips speech. The report endpoint is designed with V4.
2. **The rejoin window stays.** A removed principal holding a token under 75 seconds old can rejoin for at most about a second before the poller removes them, and that removal is audited. Rotating the room on every membership change would close the window by disconnecting everyone in the room each time. ADR-005 describes scoped speech as discretion, not secrecy; a logged one-second window is consistent with that, and a reconnect for the whole room is not worth it.
3. **One connection per principal per room.** A second device displaces the first. Presence stays unambiguous and nobody is listed twice. Listening on two devices at once can be added later with a per-device identity suffix without changing anything stored.

## 10. LiveKit behaviours this note relies on, as measured

Measured on 2026-10-09 against LiveKit server 1.13.7 (the `livekit/livekit-server` image, automatic room creation off, bound to localhost), with the spike's client and with `internal/server/livekit` (#125). The spike had already verified token minting, `ListParticipants`, `RemoveParticipant` and `UpdateParticipant`.

| # | Behaviour | Result |
|---|---|---|
| 1 | `ListParticipants` shows whether a microphone track is muted | **Holds.** A mute and an unmute were each visible about 25 ms after the client made them. |
| 2 | `CreateRoom` on an existing name leaves the room alone | **Holds.** Same room id returned; the participant in it was undisturbed. |
| 3 | With automatic creation off, a token for a room that does not exist cannot join | **Holds.** The join is refused with 404, "requested room does not exist". |
| 4 | A second connection with the same identity replaces the first | **Holds.** LiveKit drops the first with reason `DUPLICATE_IDENTITY`. |
| 5 | The room list's participant count is usable for the sweep | **Does not hold as assumed.** The count read zero for about two seconds after a join. The sweep uses room existence instead (§6). An empty room is closed after its timeout and drops out of the list; `CreateRoom` brings the name back with a new room id; `ListParticipants` on a room LiveKit does not have is an empty list, not an error. |
| 6 | A token is refused once expired | **Holds with 60 seconds of leeway**, which this note had not assumed. A token was accepted 55 seconds past its expiry and refused at 65, and the same for a not-before time in the future. §4 sets the lifetime with that in mind. |
| 7 | An established connection outlives its token | **Holds.** A client that joined on an 8-second token was still connected 75 seconds later. This is why §5 enforces by removal. |
| 8 | Removing a participant who has already left | Answers 404. `internal/server/livekit` treats that as success, so a participant leaving just before their removal is not mistaken for an outage. |

Wire details observed: field names are snake case (`joined_at_ms`, `num_participants`), 64-bit integers are strings, and enums are names (`"MICROPHONE"`, `"AUDIO"`). The token's `canPublishSources: ["microphone"]` is honoured. Each room call was accepted with only the grant it needs.

Checked against LiveKit's documentation, not a server: its webhooks have no mute event (§6).

## 11. Out of scope

Recording; video and screen share (P9); agents as voice participants; whisper mapping; anything about audio quality, echo cancellation or push-to-talk keys, which belong to the client (V4 and V5).
