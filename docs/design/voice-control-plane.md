# Design: voice control plane

- **Status:** Draft for V3 implementation (governing decision: [ADR-004](../adr/ADR-004-voice-via-livekit.md), Accepted).
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
- The response carries the client address, the caller's LiveKit identity, and one grant per room the caller may join:

```jsonc
{
  "livekit_url": "wss://voice.example",
  "identity": "p7",
  "rooms": [
    // audience absent = the whole channel, as on messages (ADR-005)
    {"room": "conch-…", "token": "<jwt>", "can_publish": true, "expires_at": "2026-10-09T12:00:45.000Z"}
  ]
}
```

- **Identity** is `p<principal id>`. One principal holds one connection per room; LiveKit replaces an earlier connection with the same identity. That keeps presence unambiguous. A second device takes over from the first.
- **Token grant:** join that one room; subscribe; publish a microphone track only if `can_publish`; no data publishing; no room administration.
- **Lifetime is 60 seconds.** LiveKit checks a token when a client connects, not afterwards, so the lifetime only bounds how long a token can be used to *start* a connection. Clients ask for a session immediately before connecting and again before any reconnect.
- **Clock skew.** The token's not-before time is set 30 seconds in the past, so a LiveKit clock that runs behind `conchd`'s does not reject it. A LiveKit clock more than about 50 seconds *ahead* would see the token as expired. `conchd` and LiveKit normally share a host; on separate hosts their clocks must be synchronised, and the deployment docs say so.
- With nets (V5), a member of a net gets a publishing grant for its room and a monitor gets a listen-only grant. The shape above does not change; each grant gains the `audience` that ADR-005 defines.

Each issued session writes one `voice_session_issued` audit event. The token is never logged or audited.

## 5. Live enforcement

A token cannot be revoked, and an established connection outlives it. So `conchd` enforces changes itself:

- When a principal is removed from the channel, disabled, or has all credentials revoked, `conchd` calls `RemoveParticipant` for each of that principal's rooms. With nets, the same happens on leaving a net.
- The poller (§6) compares every participant in every room with current membership on each pass, and removes anyone who should not be there. This closes the gap left by a token issued in the last 60 seconds before a removal.
- Enforcement does not depend on `conchd`'s memory. After a restart the poller's first act is a sweep of every stored room (§6), so someone removed while `conchd` was down, or whose removal call failed just before it stopped, is still found.

**Residual exposure, stated plainly:** a principal removed while holding a token under 60 seconds old can rejoin and hear audio until the next poll, at most about one second. Rotating the room name on every membership change would close that completely at the cost of reconnecting everyone; it is not proposed for V3.

A failed `RemoveParticipant` is retried on the next pass and logged. If LiveKit is unreachable, `conchd` cannot enforce; presence says `available: false` and an audit event `voice_enforcement_unavailable` is written once per outage.

## 6. Presence

`conchd` learns who is connected and who is transmitting by **polling** LiveKit's `ListParticipants` for rooms that are in use: any room with a session issued in the last two minutes or a participant on the previous pass. The interval is 500 ms while in use.

"In use" is held in memory, so it is rebuilt rather than trusted: **at startup, and every 30 seconds after, the poller sweeps** by asking LiveKit for its room list once and marking every stored room that has participants as in use. A room joined with a token issued before a restart is therefore picked up at startup, and nothing can stay connected unobserved for more than 30 seconds. With no room in use, the sweep is the only call `conchd` makes.

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

**Resolution limit, stated plainly:** these events are observed by polling, so their times are accurate to one interval, and a transmission shorter than the interval between two passes can be missed entirely. ADR-004 says audit "records join, leave, and transmit start/stop". This is a weaker guarantee than those words read strictly, and it is Nick's call whether it meets them. See §9, question 1.

## 8. Wire shapes

Canonical types land in `pkg/schema` through the `schema-change` skill: the session response and room grant, the presence document (`conch.voice_presence.v1`), and the error codes `voice_not_configured`, `voice_unavailable`, `voice_requires_auth`. They reuse `Audience` from the V2 schema (#114). All are new types; nothing published changes.

API parity (CLAUDE.md rule 4): `conch voice status <channel>` prints the snapshot. Joining is `conch-voice`'s job in V4, using the same session endpoint.

## 9. Open questions for Nick

1. **Does half-second audit resolution for transmit events satisfy ADR-004?** A press shorter than the poll interval can go unrecorded. Exact start and stop would need either the client to report each press, which a modified client can skip, or `conchd` to grant and revoke publishing per press, which adds a round trip to every push-to-talk and clips speech. The proposal is to ship polling, with the limit documented. If Nick reads the ADR as requiring every transmission, #127 must not start until one of the alternatives is chosen.
2. **Is the one-second rejoin window in §5 acceptable**, or should a membership change rotate the room?
3. **One connection per principal per room** means a second device displaces the first. Acceptable for V3?

## 10. Assumptions not yet verified

The spike verified token minting, `ListParticipants`, `RemoveParticipant` and `UpdateParticipant` against LiveKit 1.13.7. This note also relies on five behaviours it did **not** exercise. The first implementation issue (#125) must check each against a real server and record the result; if one is false, this note is corrected before work continues.

1. `ListParticipants` shows whether a published microphone track is muted, and reflects a change within one poll interval.
2. `CreateRoom` on a name that already exists succeeds without disturbing the room.
3. With automatic room creation off, a valid token for a room that does not exist cannot be used to join.
4. A second connection with the same identity replaces the first.
5. `ListRooms` reports participant counts accurately enough for the 30-second sweep, and a room LiveKit closed for being empty is recreated by `CreateRoom` under the same name.

Checked against LiveKit's documentation on 2026-10-09, not against a server: its webhooks have no mute event (§6).

## 11. Out of scope

Recording; video and screen share (P9); agents as voice participants; whisper mapping; anything about audio quality, echo cancellation or push-to-talk keys, which belong to the client (V4 and V5).
