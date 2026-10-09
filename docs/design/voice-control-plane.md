# Design: voice control plane

- **Status:** Adopted for V3 implementation, 2026-10-09 (governing decision: [ADR-004](../adr/ADR-004-voice-via-livekit.md), Accepted). §10 records the LiveKit behaviours it relies on, as measured.
- **Touches:** [ADR-002](../adr/ADR-002-single-binary-sqlite.md) (optional processes degrade gracefully), [ADR-003](../adr/ADR-003-multi-human-access.md) (authenticated humans, channel membership), [ADR-005](../adr/ADR-005-nets-and-whispers.md) (audiences), [ADR-006](../adr/ADR-006-rust-voice-client.md) (the client).
- **Owner:** protocol-designer (schemas), server-engineer (control plane)
- **Issues:** epic #122, this note #123; room rotation #161
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
- The mapping is stored: `voice_rooms (id, channel_id, net_id, room_name, created_at, retired_at)`, `net_id` NULL for the channel-wide room. There is one **live** row (`retired_at IS NULL`) per audience; the partial unique indexes say so. A room's row is not deleted when the room is rotated (§5): it is retired, and the audience gets a new live row under a new name. A retired row is deleted by a sweep once it is more than 24 hours old and LiveKit no longer lists its room (issue #167): by then no token for the room can still be valid and `conchd` never creates a room for a retired row, so there is nothing left for the row to find. The `voice_room_rotated` audit rows remain the record of rotations. The migration that introduced holders retires every room that existed before it, because nothing recorded who held tokens for those. `room_name` is unique across live and retired rows, and names are 128 random bits, so a name, once used, is never handed out again; the retired row is what lets the sweep (§6) find a room LiveKit still has to delete.
- A room row is created the first time a session is requested for its audience. The table covers channel and net audiences; whisper mapping is undecided (ADR-004) and will extend it when it is.
- `voice_room_holders (room_id, principal_id, credential_id, created_at)`, one row per (room, principal, credential), records every credential a session was issued under for a room (§4). The rows of a room go when it is retired.
- **`conchd` calls LiveKit's `CreateRoom` on every session request**, before it signs a token. LiveKit's rooms are not durable: it closes an empty room after a timeout and forgets all rooms when it restarts. The stored name is the durable thing, and `CreateRoom` on an existing room changes nothing.
- The room name is not a secret; the signed token is the gate. It is unguessable anyway so that a mis-set LiveKit cannot be probed by name. A retired room's name is as sensitive as a live one's until LiveKit has deleted the room: neither is ever written to a log line, an audit row, an error or a presence document.

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
- **The token expires 15 seconds after it is issued, and can start a connection for about 75.** LiveKit checks a token when a client connects, not afterwards, and it allows 60 seconds of leeway on both the expiry and the not-before time (§10, finding 6). So the expiry `conchd` writes is deliberately short: the leeway cannot be turned off from `conchd`, and it is added on top. Clients ask for a session immediately before connecting and again before any reconnect. **This is true of the token `conchd` signs and of no other:** once a client has connected, LiveKit sends it a ten-minute token of its own and renews it (§10, finding 9), so a short lifetime here limits how long an *unused* token is good for, not how long a person who has connected can keep rejoining.
- **Clock skew.** The same leeway means the two clocks may differ by up to about a minute in either direction before a fresh token is refused, so the not-before time is not backdated. `conchd` and LiveKit normally share a host; on separate hosts their clocks must be synchronised, and the deployment docs say so.
- **Every token is a distinct string.** A join token carries a random `jti` (96 bits). `nbf` and `exp` have one-second resolution, so without it two tokens for one principal and room issued in the same second would be identical. LiveKit ignores the claim (measured on the same server as §10: a token with `jti` joins and publishes normally); `conchd` does not track it, and it is not a replay guard.
- With nets (V5), a member of a net gets a publishing grant for its room and a monitor gets a listen-only grant. The shape above does not change; each net grant carries the `audience` that ADR-005 defines (`{"kind": "net", "net_id": 3}`), with `can_publish: false` for a monitor. The fixture `voice-session-response-v1-net.json` shows a channel grant beside a listen-only net grant.

- **The holder is recorded before the token is signed.** For each grant, `conchd` records the (room, principal, credential) the session is issued under, in the same step that finds the room, before it asks LiveKit to create the room and before a token exists. Whoever may hold a token LiveKit will renew is therefore always in the store, with no timers and no memory: LiveKit renews tokens for as long as a connection lasts, so there is no safe expiry to reason about. Recording the same triple again is a read. If the holder cannot be recorded, no token is returned. The request needs a credential to record; without one it is refused.
- **The caller is checked before the holder is recorded, and again before the token is signed.** The first check keeps a refused request from leaving a holder row behind: a caller removed while their request was in flight would otherwise be recorded, refused, and then rotate the room at the next sweep, disconnecting everyone for a session that was never issued. The second is the one that keeps a token from them. A removal that lands in the instant between the first check and the record still leaves such a row; the result is one unnecessary rotation, never a token.
- **A rotation racing a session.** The store refuses to record a holder against a retired room, and the session checks that its room is still live after the slow LiveKit call and before it signs. If the room was rotated meanwhile it starts over against the channel's new room (at most three times, then 503 `voice_unavailable`, nothing issued). A token is never signed for a room known to be retired. A rotation can also land after the holder is recorded and before `CreateRoom`: the rotation deletes the room and the request then creates it again. So a request that asked LiveKit for a room and issues no token for it (the room was rotated, the caller lost their place, LiveKit or the store failed) checks whether the room is retired and, if so, deletes it and has the poller forget it. A rotation that commits after that check deletes the room itself, after the request's `CreateRoom`. What remains is a rotation in the microseconds between the liveness check and the signature: it leaves a token for a deleted room, which LiveKit refuses.
- **LiveKit must run with `room.auto_create: false`.** Rotation depends on a deleted room staying deleted. With automatic creation on, anyone holding a token for a retired name, LiveKit's own refresh tokens included, recreates the room by joining it; `conchd` deletes it again only at the next sweep, and it is not polled or shown in the meantime. Nobody entitled is sent to a retired room, so the stale holder would normally be alone there, but that is not a property to rely on. `conchd` cannot read LiveKit's configuration, so this is a deployment requirement, to be stated wherever running LiveKit beside `conchd` is documented (V6). The V4 client must never reconnect with a token LiveKit sent it: it asks `conchd` for a new session.

Each issued session writes one `voice_session_issued` audit event. The token is never logged or audited.

## 5. Live enforcement

A token cannot be revoked, an established connection outlives it, and (§10, finding 9) LiveKit gives every participant a renewable ten-minute token of its own. So removing a person from a room does not stop them coming back: whoever has connected once can rejoin for as long as they keep at it. What does stop them is that the room they hold a token for no longer exists. `conchd` therefore enforces by **rotating the room**.

**The invariant.** A room stays in use only while every credential recorded against it (§4) is still live and belongs to a current, enabled, human member of the channel. "Live" is what `CredentialLive` means: not revoked, not expired, and the principal not disabled. When the invariant fails for a room:

1. In one transaction the room row is retired, its holder rows are dropped, the channel's next room row is created under a fresh name, and one `voice_room_rotated` audit event is written (actor `system`, subject `channel:<id>`, detail `reason=<why>`, never a room name). Reasons: `principal_disabled`, `member_removed`, `credential_revoked`, `credential_expired`, `not_human`, reported in that order of precedence when a room fails in several ways. Rotation is idempotent and safe under concurrency: exactly one caller rotates a given room.
2. The poller forgets the old room: presence shows the channel's new room, empty, until people rejoin, and whoever the poller had in the old room is recorded as having left.
3. `conchd` calls `DeleteRoom` on the old name, which disconnects everyone in it. Every token for the old room, LiveKit's refresh tokens included, is now for a room that does not exist.
4. The new room is created in LiveKit by the next session request, as always (§3). Entitled people ask for a new session and rejoin. The old name is never created again.

**It is checked in two places.**

- **At once, in the same request**, before the HTTP response is written, when a member is removed from the channel, a principal is disabled, all of a principal's credentials are revoked, or one credential is revoked or replaced (rotating a credential revokes the old one). The hook checks the invariant for every stored room, rotates what fails and deletes the old rooms. It never fails the request: LiveKit being down or refusing `DeleteRoom` is logged and left to the sweep; a failing store read is the same.
- **At every sweep** (§6), for every stored room. That covers credential expiry, a restart, a rotation whose `DeleteRoom` failed or was cut short by a restart, and any change made to the store without going through a hook. The check needs only the store, so it also runs while LiveKit is down; deleting the old rooms then waits for LiveKit.

A member who was never issued a session for the current room rotates nothing and disconnects nobody. Rotation disconnects everyone in a room, so it is done only when a holder is no longer entitled, not on every membership change.

**What is still done by name.** The per-pass comparison of every participant with current membership stays (§6), as does the immediate removal of a removed or disabled principal from the channel's rooms that were not rotated. They are defence against an identity `conchd` never issued a token to (a leaked API secret, say) and against anything rotation missed. A revoke-all removes nobody by name any more: the rooms its credentials held are rotated.

**Residual exposure.** Between a change and the check that sees it there is nothing: the hooks run before the response. For a change that does not go through a hook (expiry, the store edited by hand) the window is one sweep, 30 seconds. A credential that is not recorded as a holder cannot have been given a token for the current room by `conchd`; anything else in the room is found by the pass.

If `DeleteRoom` fails, the retired row stays and the next sweep is brought forward to one second away, and again each time the delete fails: until the room is gone, the people in it, the one who lost their place included, are still connected and appear in no presence. Removing them by name instead would not keep them out (§10, finding 9). The sweep deletes a retired room on sight for as long as LiveKit still lists it. If LiveKit is unreachable, `conchd` cannot delete or enforce by name; presence says `available: false` and an audit event `voice_enforcement_unavailable` is written once per outage. Rotation in the store still happens, so no new session names the old room.

## 6. Presence

`conchd` learns who is connected and who is transmitting by **polling** LiveKit's `ListParticipants` for rooms that are in use: any live room with a session issued in the last two minutes or a participant on the previous pass. A retired room is never polled or shown. The interval is 500 ms while in use.

"In use" is held in memory, so it is rebuilt rather than trusted: **at startup, and every 30 seconds after, the poller sweeps.** It first checks the invariant (§5) from the store and rotates any room that fails it. It then asks LiveKit for its room list once, marks every live stored room that LiveKit currently has as in use for the next pass, and calls `DeleteRoom` for every *retired* room that LiveKit still lists (a room it no longer lists is left alone). The room list's participant count is not used: it lags a join by seconds (§10, finding 5), whereas a room exists in LiveKit only while someone is in it or for a short timeout after, so existence is the reliable signal and `ListParticipants` gives the truth. A room joined with a token issued before a restart is therefore picked up at startup, and nothing can stay connected unobserved for more than 30 seconds. With no room in use, the sweep is the only call `conchd` makes.

Rooms are polled concurrently, at most eight at a time, and a pass that has not finished is not started again. This is sized for one self-hosted instance with tens of rooms, which is what Conch is (ADR-002).

Why polling and not LiveKit webhooks:

- Push-to-talk is a mute and unmute of a track that stays published. LiveKit's webhooks report publishing and unpublishing but have no mute event (its [webhook reference](https://docs.livekit.io/home/server/webhooks/) lists `track_published` and `track_unpublished` only; muting is a client-side SDK event), so they would not show who is talking. Publishing and unpublishing on every key press would, but takes long enough to clip the first word.
- A webhook receiver is another unauthenticated-by-bearer ingest route to secure, and makes LiveKit need a route back to `conchd`. Polling keeps every call in one direction.
- The same pass does the enforcement check in §5.

What `conchd` reads from each pass: the participants of the room, and for each whether its microphone track is published and unmuted. That is "transmitting".

Surfaces:

- `GET /v1/channels/{channel}/voice` returns a snapshot: whether voice is configured and available, and for each room the caller may see, its participants with `transmitting` and `joined_at`.
- `GET /v1/voice/ws?channel=` streams the same snapshot, as a `conch.voice_presence.v1` document, whenever it changes. A new route, because the existing sockets carry bare message envelopes and a client decoding those must not meet another type.

Both require an authenticated **human member** of the channel, checked exactly as on the message routes: no credential is 401, a non-member gets the unknown-channel 404, and a presence socket is closed when its subscriber loses membership. Agent callers are refused with 403 in V3. With `--auth off` both answer 400 `voice_requires_auth` before the channel is looked at, as the session endpoint does: voice is never anonymous. A server with voice not configured still answers a member with a snapshot (`configured: false`, `available: false`, no rooms), and the socket sends that document. ADR-004 defers agents in voice, and whether an agent may see who is talking is part of that decision, not this one.

A snapshot never contains a room name or a token, so reading presence gives nothing that helps join a room.

How the poller behaves where the text above leaves room (issue #127):

- **Available** means LiveKit has answered at least once and the last call succeeded. Until the first sweep after a start has been answered, presence is `available: false`.
- **Outage.** Any failed `ListRooms` or `ListParticipants` is an outage (a failed `RemoveParticipant` is not: it is logged and the next pass tries again). The first failure writes one `voice_enforcement_unavailable` event, including when no room is in use, because a failed sweep means conchd cannot tell. Retries wait 1 s, doubling to 30 s, and a recovery call that succeeds ends the outage.
- **Recovery.** The poller keeps what it last saw through an outage, so on recovery a participant who stayed gets no second `voice_joined`; one who left gets `voice_left`, and one who reconnected gets `voice_left` then `voice_joined`. Those times are accurate to the outage, not to one interval.
- **Immediate removal** of a removed or disabled principal takes them out of presence at once, then calls `RemoveParticipant` in the channel's rooms that were not rotated. A holder's room is rotated instead (§5). (Before #161 a revoke-all barred the principal for two minutes, in memory, as a stopgap; rotation replaced it and nothing of the bar remains.)
- **If the invariant check cannot read the store**, the error is logged, the rest of the sweep still runs, and the next sweep is tried after one second rather than thirty. A hook that cannot read it does the same: it brings the next sweep forward.
- **A rotation is in memory only as a retired row.** After a restart the sweep finds the retired rooms in the store and deletes any LiveKit still has; nothing is lost between the rotation and the `DeleteRoom`.
- **A removal is audited per connection.** Someone who rejoins on an old token and is removed again gets a second `voice_participant_removed`; the same connection listed twice because LiveKit was slow to drop it gets one. Connections are told apart by their join time.
- **A session wakes the poller.** With no room in use the loop sleeps until the next sweep; issuing a session interrupts that wait, so the first people to join an idle server are seen, and checked, within a pass. (Measured against a real LiveKit: 1.0 s from joining to presence; before this the wait was up to 30 s.)
- **If a participant's entitlement cannot be read** (a store error), that participant keeps the state they had, or stays unseen if new, and is not removed; everyone else in the room is handled on that pass as usual.
- **If LiveKit answers a sweep but the stored rooms cannot be read**, the sweep is retried after one second, not on every tick.
- **A room the last pass could not clear stays polled.** If a removal failed, or someone's entitlement could not be read, the room is polled again on the next pass whatever its state says, rather than dropping out of use until the next sweep.
- **One unreadable room is not an outage.** If LiveKit answers for some rooms and not for another, enforcement continues everywhere else; the unreadable room shows nobody and is retried every pass. Only when no room can be read is it an outage.
- **Nothing last-seen is shown until it has been read again.** After an outage, or for a room that could not be read, presence shows nobody in that room until a pass has read it, even though `available` is true again.
- **A removal that lands during a pass is not undone by it.** The pass leaves out anyone evicted after it asked LiveKit.
- **A removal is audited when LiveKit actually disconnected someone**, whether or not a pass had seen them, and not when there was nobody to disconnect.
- **Sessions do not speed the poller up.** Only a session for a room that was not being watched wakes the loop.
- A participant's join time is LiveKit's; a second connection with the same identity between two passes is recorded as a leave and a join.

A snapshot is the whole state, not a delta, so a client that misses one loses nothing.

## 7. Audit

| Event | When | Detail |
|---|---|---|
| `voice_session_issued` | a session is returned | channel, rooms granted, publish or listen |
| `voice_joined`, `voice_left` | a participant appears or disappears between passes | channel, audience |
| `voice_transmit_started`, `voice_transmit_stopped` | the microphone track becomes unmuted or muted between passes | channel, audience |
| `voice_participant_removed` | `conchd` removes someone by name | channel, audience, reason |
| `voice_room_rotated` | a channel's room is rotated (§5); once per rotation, written in the rotation's transaction | actor `system`, subject `channel:<id>`, detail `reason=<principal_disabled, member_removed, credential_revoked, credential_expired or not_human>`; never a room name |
| `voice_enforcement_unavailable` | LiveKit becomes unreachable (once per outage; also when no room is in use, since a failed sweep means conchd cannot tell) | `source=observed` |

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
2. **~~The rejoin window stays.~~ Reversed (#161): rooms are rotated.** The original decision was: a removed principal holding a token under 75 seconds old can rejoin for at most about a second before the poller removes them, and that removal is audited; rotating the room on every membership change would close the window by disconnecting everyone in the room each time, and a logged one-second window is consistent with ADR-005's view of scoped speech as discretion, not secrecy. It rested on the 75-second figure, which holds only for the token `conchd` signs. LiveKit sends every participant a renewable ten-minute token of its own (§10, finding 9), so the window does not close: a removed or disabled member can loop connect, hear up to a pass of audio, be removed, and reconnect indefinitely, and a connection made under a revoked or expired credential persists. Per-pass removal cannot fix that; only making the old tokens worthless can. So the room is rotated, but only when someone who may hold a token for it loses the right to be there (§5), not on every membership change, which keeps the cost the original decision objected to (everyone reconnecting) to the cases that need it.
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
| 9 | A join token is the only token that admits a principal to a room | **Does not hold.** LiveKit sends each participant a token of its own when they connect and every five minutes after: same identity, same room grant, valid ten minutes, signed with the server's key. There is no setting to turn it off (`pkg/service/roommanager.go`, `refreshToken`). Measured: a client that connected on a 15-second token was sent a 600-second one in its first frames; 145 seconds later the original was refused and LiveKit's own token admitted the client, which received audio. §4 and §5 were written before this was known and are corrected in place; the fix is #161. |
| 10 | Deleting a room makes every token for it useless, LiveKit's own included | **Holds.** With the room rotated and the old one deleted, a client presenting the token LiveKit had sent it for the old room was refused: `404 Not Found - requested room does not exist`. Measured for each way a holder loses entitlement (removed from the channel, disabled, all credentials revoked, the one credential revoked, the credential expiring with no request at all: the sweep rotated the room). In each, the old room was gone from `ListRooms`, the channel had a new room, and a bystander connected to the old room could get a session for the new one. Removing a member who had never been issued a session rotated nothing and left the bystander connected. `auto_create` was off; with it on, a stale token would recreate the old name, but nobody entitled is ever sent there. |
| 11 | `DeleteRoom` | Needs the `roomCreate` grant (`roomAdmin` alone is refused, 401). A room LiveKit does not have answers 404 with the Twirp body `{"code":"not_found"}`, which the client treats as done. |

Wire details observed: field names are snake case (`joined_at_ms`, `num_participants`), 64-bit integers are strings, and enums are names (`"MICROPHONE"`, `"AUDIO"`). The token's `canPublishSources: ["microphone"]` is honoured. Each room call was accepted with only the grant it needs.

Checked against LiveKit's documentation, not a server: its webhooks have no mute event (§6).

## 11. Out of scope

Recording; video and screen share (P9); agents as voice participants; whisper mapping; anything about audio quality, echo cancellation or push-to-talk keys, which belong to the client (V4 and V5).
