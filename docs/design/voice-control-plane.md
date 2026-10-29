# Design: voice control plane

- **Status:** Adopted for V3 implementation, 2026-10-09 (governing decision: [ADR-004](../adr/ADR-004-voice-via-livekit.md), Accepted). §10 records the LiveKit behaviours it relies on, as measured.
- **Touches:** [ADR-002](../adr/ADR-002-single-binary-sqlite.md) (optional processes degrade gracefully), [ADR-003](../adr/ADR-003-multi-human-access.md) (authenticated humans, channel membership), [ADR-005](../adr/ADR-005-nets-and-whispers.md) (audiences), [ADR-006](../adr/ADR-006-rust-voice-client.md) (the client).
- **Owner:** protocol-designer (schemas), server-engineer (control plane)
- **Issues:** epic #122, this note #123; room rotation #161; screen share #443 (§12, which records its own measurements)
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
- The mapping is stored: `voice_rooms (id, channel_id, net_id, room_name, created_at, retired_at)`, `net_id` NULL for the channel-wide room. There is one **live** row (`retired_at IS NULL`) per audience; the partial unique indexes say so. A room's row is not deleted when the room is rotated (§5): it is retired, and the audience gets a new live row under a new name. A retired row is deleted by a sweep once LiveKit has not listed its room for 24 hours (issue #167). The time is counted from the last sweep that still found the room in LiveKit, or from the rotation if none did: a room that could only be deleted late (`conchd` stopped, LiveKit unreachable) had people in it, and LiveKit renewing their tokens, until then. After a day without the room no token for it can still be valid, and `conchd` creates a room only for a live row (apart from the race in §4, which deletes what it created), so there is nothing left for the row to find. This too depends on automatic room creation being off (§4): with it on, a stale token could recreate a room whose row is gone, and then nothing would delete it. The `voice_room_rotated` audit rows remain the record of rotations. The migration that introduced holders retires every room that existed before it, because nothing recorded who held tokens for those. `room_name` is unique across live and retired rows, and names are 128 random bits, so a name, once used, is never handed out again; the retired row is what lets the sweep (§6) find a room LiveKit still has to delete.
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
  *Amended 2026-10-29 (#933): one principal holds one **own** connection per room. A companion (`p<id>.screen`) or a viewer (`p<id>.view`) is a second participant conchd issues under a derived identity and folds into the person (§13); the session response names the companion identity in an optional `companion_identity`.*
- **Token grant:** join that one room; subscribe; publish a microphone track only if `can_publish`; no data publishing; no room administration.
- **The token expires 15 seconds after it is issued, and can start a connection for about 75.** LiveKit checks a token when a client connects, not afterwards, and it allows 60 seconds of leeway on both the expiry and the not-before time (§10, finding 6). So the expiry `conchd` writes is deliberately short: the leeway cannot be turned off from `conchd`, and it is added on top. Clients ask for a session immediately before connecting and again before any reconnect. **This is true of the token `conchd` signs and of no other:** once a client has connected, LiveKit sends it a ten-minute token of its own and renews it (§10, finding 9), so a short lifetime here limits how long an *unused* token is good for, not how long a person who has connected can keep rejoining.
- **Clock skew.** The same leeway means the two clocks may differ by up to about a minute in either direction before a fresh token is refused, so the not-before time is not backdated. `conchd` and LiveKit normally share a host; on separate hosts their clocks must be synchronised, and the deployment docs say so.
- **Every token is a distinct string.** A join token carries a random `jti` (96 bits). `nbf` and `exp` have one-second resolution, so without it two tokens for one principal and room issued in the same second would be identical. LiveKit ignores the claim (measured on the same server as §10: a token with `jti` joins and publishes normally); `conchd` does not track it, and it is not a replay guard.
- With nets (V5), a member of a net gets a publishing grant for its room and a monitor gets a listen-only grant. The shape above does not change; each net grant carries the `audience` that ADR-005 defines (`{"kind": "net", "net_id": 3}`), with `can_publish: false` for a monitor. The fixture `voice-session-response-v1-net.json` shows a channel grant beside a listen-only net grant.

- **The holder is recorded before the token is signed.** For each grant, `conchd` records the (room, principal, credential) the session is issued under, in the same step that finds the room, before it asks LiveKit to create the room and before a token exists. Whoever may hold a token LiveKit will renew is therefore always in the store, with no timers and no memory: LiveKit renews tokens for as long as a connection lasts, so there is no safe expiry to reason about. Recording the same triple again is a read. If the holder cannot be recorded, no token is returned. The request needs a credential to record; without one it is refused.
- **The caller is checked before the holder is recorded, and again before the token is signed.** The first check keeps a refused request from leaving a holder row behind: a caller removed while their request was in flight would otherwise be recorded, refused, and then rotate the room at the next sweep, disconnecting everyone for a session that was never issued. The second is the one that keeps a token from them. A removal that lands in the instant between the first check and the record still leaves such a row; the result is one unnecessary rotation, never a token.
- **A rotation racing a session.** The store refuses to record a holder against a retired room, and the session checks that its room is still live after the slow LiveKit call and before it signs. If the room was rotated meanwhile it starts over against the channel's new room (at most three times, then 503 `voice_unavailable`, nothing issued). A token is never signed for a room known to be retired. A rotation can also land after the holder is recorded and before `CreateRoom`: the rotation deletes the room and the request then creates it again. So a request that asked LiveKit for a room and issues no token for it (the room was rotated, the caller lost their place, LiveKit or the store failed) checks whether the room is retired and, if so, deletes it and has the poller forget it. A rotation that commits after that check deletes the room itself, after the request's `CreateRoom`. What remains is a rotation in the microseconds between the liveness check and the signature: it leaves a token for a deleted room, which LiveKit refuses.
- **LiveKit must run with `room.auto_create: false`.** Rotation depends on a deleted room staying deleted. With automatic creation on, anyone holding a token for a retired name, LiveKit's own refresh tokens included, recreates the room by joining it; `conchd` deletes it again only at the next sweep, and only while the retired row still exists (§3: a day after LiveKit last listed the room); it is not polled or shown in the meantime. Nobody entitled is sent to a retired room, so the stale holder would normally be alone there, but that is not a property to rely on. `conchd` cannot read LiveKit's configuration, so this is a deployment requirement, to be stated wherever running LiveKit beside `conchd` is documented (V6). The V4 client must never reconnect with a token LiveKit sent it: it asks `conchd` for a new session.

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

`conchd` learns who is connected and who is transmitting by **polling** LiveKit's `ListParticipants` for rooms that are in use: any live room with a session issued in the last two minutes, a participant on the previous pass, or something the transmit rule has yet to settle (below). A retired room is never polled or shown.

**The gap between passes is random, and has no memory.** While any room is in use, each gap is 100 ms plus an exponentially distributed time with mean 400 ms, and never more than 1.5 s. It is drawn afresh for every gap from the operating system's random source (never a generator seeded from the clock). The mean gap is about 490 ms; 46% of gaps are shorter than 350 ms, and 3% are cut to the 1.5 s cap. The gap is the wait between the end of one pass and the start of the next. It was a fixed 500 ms in V3 and, in the first form of #135, uniform between 350 and 650 ms.

Why this shape. From V4 the poller checks what clients report about their own transmissions ([conch-voice.md](conch-voice.md) §6), and that check is only as good as a client's ignorance of when the next pass will be. A client does learn when a pass *has* happened: presence changes the instant a pass changes anything, and a second member flipping a microphone makes every pass change something. With gaps of 350 to 650 ms that told it no pass could follow for 350 ms, and a transmission placed in those 350 ms was never looked at (found in the security review of #135). Now a known pass promises 100 ms and nothing more. After those, however long it has been since the last pass, the chance of one in the next instant is the same, so waiting teaches nothing; and no microphone goes unlooked at for more than 1.5 s. Presence is still told of a change at once: hiding when passes happen from the people in the room was not worth making presence late.

**No request brings a pass forward, and none puts one off.** Waking the loop runs a pass at once, over every room in use, and starts a new gap. A session or a transmit report wakes it only when no room at all was in use, so that the first people into an idle server are seen promptly. While any room is in use, a session or a report for another room, idle until then, wakes nothing: that room is picked up by the next pass, at most one gap away. Were it otherwise, a member of two channels could run a pass over the first whenever they liked by reporting in the second, and would then know when none could follow; with the earlier gaps that hid a microphone that was open 95% of the time (the blocker of the same review). A sweep brought forward still wakes the loop; that takes a failure of the store or of LiveKit, not a request.

**Transmissions are compared, not just observed (V4, #135).** Each pass hands what it saw of every microphone to the transmit rule, together with what each client last reported (`POST /v1/channels/{channel}/voice/transmit`). The rule, its reasons and the cases it does and does not catch are in [conch-voice.md](conch-voice.md) §6; in code it is a state machine of its own, `internal/server/voice_transmit_rule.go`, that holds no lock and reads no clock. A report is applied, and a pass is applied, under the poller's one lock, each with a time read under it, so reports and passes have one order and a pass reads the reported state as it is at that instant. A room in which someone has reported `started`, or has an unreported transmission open, or was seen unmuted on a pass not yet judged, stays in use until a pass has settled it: that is how a report from a holder who is not in the room gets looked at, and why every row that opens a transmission gets a row that closes it. Reported state is in memory, per room and principal, and is dropped with the room after everything open in it has been closed.

"In use" is held in memory, so it is rebuilt rather than trusted: **at startup, and every 30 seconds after, the poller sweeps.** It first checks the invariant (§5) from the store and rotates any room that fails it. It then asks LiveKit for its room list once, marks every live stored room that LiveKit currently has as in use for the next pass, and calls `DeleteRoom` for every *retired* room that LiveKit still lists (a room it no longer lists is left alone), noting on its row that LiveKit still had it. Last, it deletes the rows of retired rooms LiveKit has not listed for a day (§3). The room list's participant count is not used: it lags a join by seconds (§10, finding 5), whereas a room exists in LiveKit only while someone is in it or for a short timeout after, so existence is the reliable signal and `ListParticipants` gives the truth. A room joined with a token issued before a restart is therefore picked up at startup, and nothing can stay connected unobserved for more than 30 seconds. With no room in use, the sweep is the only call `conchd` makes.

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
- **Recovery.** The poller keeps what it last saw through an outage, so on recovery a participant who stayed gets no second `voice_joined`; one who left gets `voice_left`, and one who reconnected gets `voice_left` then `voice_joined`. Those times are accurate to the outage, not to one gap between passes. The transmit rule sees no passes during an outage either: a transmission it had open stays open until the first pass after it, and "two passes in a row" can then be two passes an outage apart.
- **Immediate removal** of a removed or disabled principal takes them out of presence at once, then calls `RemoveParticipant` in the channel's rooms that were not rotated. A holder's room is rotated instead (§5). The store read that says which rooms to call for is bounded (4 s), as the holder check and the LiveKit calls are; if it fails or runs out of time, nobody is removed by name in that request and the next pass removes them by entitlement. (Before #161 a revoke-all barred the principal for two minutes, in memory, as a stopgap; rotation replaced it and nothing of the bar remains.)
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
- **Sessions and reports do not speed the poller up.** Only a session issued, or a transmit report that opens a reported transmission, while no room at all was in use wakes the loop (above). Neither wakes it for a room that was idle while another was in use.
- **A room's transmit rows are written in the order the rule produced them.** Whatever changes a room's transmit state (a report, a pass, a removal by name, the room being forgotten) holds that room's order from before the change until the rows it produced are in the audit log. So a report's `voice_transmit_started` is never written after the row that closes it, and no pass reads a reported state whose row is not yet written. The order is held across the audit write and nothing else. A pass therefore waits for a report whose row is being written, but not behind the reports waiting after it: reports take their turn at a separate queue first, so at any moment only one of them is contending for the order. That write is bounded (4 s; normally well under a millisecond).
- **The reports for a room are applied one at a time.** Each is applied, and its audit row written, before the next is applied. A report whose row cannot be written is taken back and answered 500; nothing else can have touched the state in between. A report still waiting for its turn when its caller goes away is dropped unapplied, and one whose room is rotated while it waits is answered 409 `voice_no_session`.
- **If a participant's entitlement cannot be read, the transmit rule is told nothing about them on that pass**: not that they are gone, and not what the pass saw of their microphone.
- **A second connection with the same identity ends the first one's transmissions.** Whatever the transmit rule had open for the principal is closed with `reason=left` before the `voice_left` and `voice_joined`, and the new connection starts from a reported state of `stopped`.
- A participant's join time is LiveKit's; a second connection with the same identity between two passes is recorded as a leave and a join.

A snapshot is the whole state, not a delta, so a client that misses one loses nothing.

## 7. Audit

| Event | When | Detail |
|---|---|---|
| `voice_session_issued` | a session is returned | channel, rooms granted, publish or listen |
| `voice_joined`, `voice_left` | a participant appears or disappears between passes | channel, audience |
| `voice_transmit_started`, `voice_transmit_stopped` | a client reports a press or a release that changes its reported state (V4, #135). Written by the transmit endpoint, timed when `conchd` applied the report. A report that changes nothing writes nothing. | channel, audience, `source=reported` |
| `voice_transmit_unreported` | the poller sees a transmission the client did not report: two unaccounted passes in a row, or one alone that is not excused ([conch-voice.md](conch-voice.md) §6). **Timed at the first pass that saw it**, which is earlier than the row is written. | channel, audience, `source=observed` |
| `voice_transmit_stopped` | the poller closes an unreported transmission: the microphone is muted, the participant is gone, or the client caught up | channel, audience, `source=observed`, `reason=muted`, `left` or `reported` (the first that applies, in that order) |
| `voice_transmit_stopped` | the poller closes a *reported* transmission: no pass saw the microphone transmit for 2 s while the reported state was `started` (`no_stop_report`); or the participant left, was removed by name, or the room was rotated while it was `started` (`left`) | channel, audience, `source=observed`, `reason=no_stop_report` or `left` |
| `voice_report_rate_limited` | a principal goes past the bound on transmit reports (a burst of 30, refilled at 4 a second). Written for the first refused report and then at most once a minute while refusals continue, never per request. | channel, the bound, `suppressed=<refusals since the last such row that wrote nothing>` |
| `voice_participant_removed` | `conchd` removes someone by name | channel, audience, reason |
| `voice_room_rotated` | a channel's room is rotated (§5); once per rotation, written in the rotation's transaction | actor `system`, subject `channel:<id>`, detail `reason=<principal_disabled, member_removed, credential_revoked, credential_expired or not_human>`; never a room name |
| `voice_enforcement_unavailable` | LiveKit becomes unreachable (once per outage; also when no room is in use, since a failed sweep means conchd cannot tell) | `source=observed` |
| `voice_share_started` | a screen share is granted (§12), before the claim is answered | actor the sharer; `channel`, `audio` |
| `voice_share_stopped` | a share ends, once for every share that started (§12) | actor the sharer, or the operator who stopped it; `channel`, `principal` (the sharer), `reason=self`, `operator`, `left`, `access_lost`, `rotated` or `idle` |
| `voice_share_unclaimed` | a pass sees a screen-share track published by someone who does not hold the room's share (§12) | actor that principal; `channel`, `sources`, `source=observed` |

The actor is the principal. No audio and no token is ever recorded.

**What changed in V4 (#135).** In V3 the poller wrote `voice_transmit_started` and `voice_transmit_stopped` for every transmission it saw. It no longer writes `voice_transmit_started` at all. An honest client's press is one pair of `source=reported` rows and nothing from the poller; a transmission nobody reported is `voice_transmit_unreported` followed by an observed `voice_transmit_stopped`. Reading the log:

- **A transmission is opened by one row and closed by a later one.** `voice_transmit_started` (reported) is closed by the client's own `voice_transmit_stopped`, or by the poller with `reason=no_stop_report` or `reason=left`. `voice_transmit_unreported` is closed by the poller with `reason=muted`, `reason=reported` or `reason=left`. A pass writes at most one closing row for a principal: when someone leaves with both kinds open (a late `started` after an unreported transmission was opened), the one `reason=left` row closes both. The one opening row that can go unclosed is a `voice_transmit_started` written before `conchd` restarted mid-press; the restart is in the log between it and what follows.
- **Times.** A transmit row carries the time the server decided the event happened, not the time the row was written: a reported row the instant the report was applied, an observed close the pass that closed it, and `voice_transmit_unreported` the first pass that saw the transmission. So the time of a `voice_transmit_unreported` row is earlier than that of the row before it. Apart from that, a room's transmit rows are written in the order they happened (§6): read by id or by time, each opening row comes before the row that closes it.
- **`reason=reported`** means the client reported late, not that it never reported: from that pass its reports are the record again.

**Resolution limit, stated plainly:** joins, leaves and what the poller writes about transmissions are observed by polling, so their times are accurate to one gap between passes, and a transmission shorter than the gap between two passes can be missed entirely by the poller. ADR-004 says audit "records join, leave, and transmit start/stop". Polling alone is a weaker guarantee than those words read strictly, so it is not the whole answer: from V4 the client reports each press, and the poller checks the reports (§9, decision 1). Events the poller writes carry `source=observed` so that reported ones can be told apart. What is still unrecorded is stated in [conch-voice.md](conch-voice.md) §6: a burst shorter than the gap from a client that does not report it, and misreporting within about one pass of the true edges of a transmission.

## 8. Wire shapes

Canonical types live in `pkg/schema/voice.go` (#124; the transmit report was added in V4 by #179), added through the `schema-change` skill. All are new types; nothing published changed.

| Type | Wire name | Where it travels |
|---|---|---|
| `VoiceSessionResponseV1` | REST body, versioned by suffix | `POST /v1/channels/{channel}/voice/session` (§4) |
| `VoiceRoomGrant` | element of `rooms` above | one per room the caller may join |
| `VoicePresenceV1` | `conch.voice_presence.v1` (`schema.VoicePresenceSchemaV1`) | `GET /v1/channels/{channel}/voice` and the presence socket (§6) |
| `VoicePresenceRoom` | element of `rooms` in presence | one per room the caller may see: optional `audience`, `participants` |
| `VoiceParticipant` | element of `participants` | `principal_id`, `can_publish`, `transmitting`, `joined_at`; and, from #443, `sharing` and `sharing_audio`, optional and omitted when false (§12) |
| `VoiceShareV1` | REST body, versioned by suffix | `POST /v1/channels/{channel}/voice/share` (§12): `principal_id`, `started_at`, `audio` |
| `VoiceShareEnded` | optional `share_ended` of `VoicePresenceV1` | from #487, in the sharer's own presence alone: `reason` (`VoiceShareEndReason`: `self`, `operator`, `left`, `access_lost`, `rotated`, `idle`), `started_at`, `ended_at` (§12) |
| `VoiceCompanionRequestV1`, `VoiceCompanionResponseV1` | REST bodies, versioned by suffix | `POST /v1/channels/{channel}/voice/companion` (§13; #933): the request is `publish` (`VoiceCompanionPublish`: `screen`, `none`) and `subscribe`, decoded strictly |
| `VoiceTransmitReportV1` | REST request body, versioned by suffix | `POST /v1/channels/{channel}/voice/transmit` (V4; [conch-voice.md](conch-voice.md) §6): a `state` of `started` or `stopped`, optional `audience` |

Every `audience` field reuses `Audience` from the V2 schema (#114); absent means the whole channel. The error codes `voice_not_configured`, `voice_unavailable` and `voice_requires_auth` are the constants `ErrorCodeVoiceNotConfigured`, `ErrorCodeVoiceUnavailable` and `ErrorCodeVoiceRequiresAuth`, carried in the existing `Error` body.

The transmit endpoint answers a successful report with 204 and no body. There is no response type: the report changes nothing a client needs told back, and a type that carries nothing was not added. The two error codes it adds travel in the existing `Error` body: 409 `voice_no_session` (`ErrorCodeVoiceNoSession`) when the caller holds no session for the channel's current room, and 429 `voice_report_rate_limited` (`ErrorCodeVoiceReportRateLimited`) past the per-principal report bound. No rate-limit code existed in `conchd` or the schema before #179, so the code is named for this endpoint and is not a general one. The handler is #135.

What the schema enforces, so that handlers and clients do not restate it:

- Session: non-empty `livekit_url` and `identity`; at least one grant; each grant has a non-empty `room` and `token`, an `expires_at`, and a well-formed `audience` if present.
- Presence: the schema name; a positive `channel_id`; `available` only when `configured`; `rooms` empty unless `available` (participants last seen are not reported while LiveKit is unreachable); within a room, positive `principal_id`s with no principal listed twice, `joined_at` set, and no participant `transmitting` without `can_publish`.
- One grant per audience in a session and one room per audience in presence, so a reader can always tell which is the channel's room. An unknown audience kind fails the whole document, as it does for a message; a new kind is a new version of these shapes.
- Presence carries no token and no room name. `voice_test.go` asserts this on the marshalled JSON (no key `token` or `room` at any depth) and on the Go types by reflection (no field `Token`, `Room` or `RoomName` reachable from `VoicePresenceV1`), so a future field cannot smuggle one in.
- Empty `rooms` and `participants` encode as `[]`, never `null`.
- Transmit report: a `state` of exactly `started` or `stopped` (case-sensitive), and a well-formed `audience` if present; an unknown audience kind fails the report. The report carries no time, no sequence number, and no principal, room or token: `conchd` stamps the time of receipt, takes reports in arrival order, and knows the caller and the room itself.
- A transmit report is decoded strictly, unlike presence. `schema.DecodeVoiceTransmitReportV1` is the one way a request body becomes a report: it rejects any field the type does not declare, at any depth (`at`, `time`, `seq`, `principal_id`, `room`, `token`, or an extra field inside `audience`), anything after the one JSON object, and a report that does not validate. A dropped field would otherwise sit in a captured request looking as though `conchd` had honoured it. `voice_test.go` also pins the fields a report can carry to an exact list, so adding one is a deliberate act and a new version of the shape. The decoder does not bound the body; the handler does.

Golden fixtures in `pkg/schema/testdata/`: `voice-session-response-v1.json`, `voice-session-response-v1-net.json`, `voice-presence-v1.json`, `voice-presence-v1-unavailable.json`, `voice-presence-v1-not-configured.json`, `voice-transmit-report-v1-started.json`, `voice-transmit-report-v1-stopped.json`, `voice-transmit-report-v1-net.json`, from #443 `voice-presence-v1-sharing.json` and `voice-share-v1.json`, and from #487 `voice-presence-v1-share-ended.json`.

The screen share (#443) changed nothing published: `VoiceShareV1` is a new type, the two presence fields are optional and omitted when false (a document with nobody sharing is byte for byte what it was), and the error code `voice_share_busy` (`ErrorCodeVoiceShareBusy`) travels in the existing `Error` body. Presence also checks that `sharing` needs `can_publish`, that `sharing_audio` needs `sharing`, and that a room lists at most one participant sharing; no document an older server sent can fail those.

Telling the sharer why their share ended (#487) changed nothing published either: `share_ended` is optional and omitted from every document but the sharer's own, so everyone else's is byte for byte what it was; the release parameter `started_at` (`schema.VoiceShareClaimParam`) is optional, and a request without it means what it meant; and `voice_share_stale` (`ErrorCodeVoiceShareStale`) travels in the existing `Error` body. Presence checks that a `share_ended` has a known reason, both times, an end no earlier than the start, and voice configured. The reasons are a closed vocabulary, so a new one is a new version of presence.

API parity (CLAUDE.md rule 4): `conch voice status <channel>` prints the snapshot. Joining is `conch-voice`'s job in V4, using the same session endpoint.

## 9. Decisions on the open questions

Nick delegated these to the principal engineer on 2026-10-09 ("use your best judgment"). They are recorded here with their reasons, and he can reopen any of them.

1. **Transmit audit: polling in V3, plus client reports from V4.** Polling alone can miss a press shorter than the interval, which is weaker than ADR-004's "audit records transmit start/stop" read strictly. V3 has no real client, only a headless test participant, so polling is all it can use and all it needs. From V4, `conch-voice` reports each press and release to `conchd`, which audits them with exact times. The poller keeps running as the check on the client: a transmission it observes with no matching report is audited as `voice_transmit_unreported`. The result is exact records for the official client, and detection at poll resolution of anything a modified client leaves out. What remains unrecorded is a burst shorter than the interval from a client that was altered not to report it. Granting and revoking publishing on every press was rejected: it would make `conchd` exact against any client, but it adds a round trip to every push-to-talk and clips speech. The report endpoint is designed with V4.
2. **~~The rejoin window stays.~~ Reversed (#161): rooms are rotated.** The original decision was: a removed principal holding a token under 75 seconds old can rejoin for at most about a second before the poller removes them, and that removal is audited; rotating the room on every membership change would close the window by disconnecting everyone in the room each time, and a logged one-second window is consistent with ADR-005's view of scoped speech as discretion, not secrecy. It rested on the 75-second figure, which holds only for the token `conchd` signs. LiveKit sends every participant a renewable ten-minute token of its own (§10, finding 9), so the window does not close: a removed or disabled member can loop connect, hear up to a pass of audio, be removed, and reconnect indefinitely, and a connection made under a revoked or expired credential persists. Per-pass removal cannot fix that; only making the old tokens worthless can. So the room is rotated, but only when someone who may hold a token for it loses the right to be there (§5), not on every membership change, which keeps the cost the original decision objected to (everyone reconnecting) to the cases that need it.
3. **One connection per principal per room.** A second device displaces the first. Presence stays unambiguous and nobody is listed twice. Listening on two devices at once can be added later with a per-device identity suffix without changing anything stored.
   *Amended 2026-10-29 (#933): the suffix arrived, as roles rather than devices: `p<id>.screen` and `p<id>.view`, folded into the person (§13). A second device with its own connection still displaces the first.*

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

Recording; camera video; agents as voice participants; whisper mapping; anything about audio quality, echo cancellation or push-to-talk keys, which belong to the client (V4 and V5). Screen share was here until ADR-008 moved it into the private alpha; its server half is §12.

## 12. Screen share

ADR-008 §3 moves screen sharing into the private alpha: one person shares into a channel's voice room, with sound where the browser provides it, and the others in that room watch. Nothing is recorded. This section is the server half (#443); the browser half, sharing with sound, the watch view and simulcast, is #444. The rule is decision O5 of [private-alpha.md](private-alpha.md), adopted by Nick on 2026-10-16: **anyone in the room may share, one share per room at a time; the sharer can stop it, and so can an operator.**

### The claim

A join token still grants the microphone alone (§4), and `e2e/voice` still asserts exactly that. Sharing is a separate, checked step, so that conchd decides who shares as it decides who speaks.

`POST /v1/channels/{channel}/voice/share`, authenticated, no body.

- **Who may ask** is checked by the same code as a session, in the same order: a verified caller (with `--auth off`, 400 `voice_requires_auth`), a member (anyone else, operators included, the unknown-channel 404), a human (an agent, 403), voice granted (403 `voice_not_allowed`), voice configured (503 `voice_not_configured`).
- **A session** is then required, as for a transmit report: the credential the request came with must be a recorded holder of the channel's current room (§4), or 409 `voice_no_session`.
- **The room's share** is looked at last. Held by another principal: 409 `voice_share_busy`, naming nobody. Free: the caller's claim is recorded and LiveKit is asked to let the caller's connection publish the screen-share sources.
- **The answer** is 200 and a `VoiceShareV1`: the sharer, when the share started, and `audio`, whether it may carry sound. Today `audio` is always true: conchd grants a share's sound with it. The field is there so that a later rule (an invite that grants voice but not sound, say) does not need a new version.
- **Not connected.** A share is granted to a connection. A holder with no connection in the room (LiveKit answers `not_found`) gets 409 `voice_no_session` and nothing is recorded. The client connects first, then claims, then publishes.
- **LiveKit down.** If LiveKit cannot be asked, nothing is granted and nothing recorded: 503 `voice_unavailable`. Chat and everything else are unaffected (rule 1).
- **Asking again while holding it** grants the sources again and answers with the share already held, unchanged; it writes nothing. That is how a sharer whose connection was replaced by one made with a fresh join token (a full reconnect through conchd) gets the share's sources on the new connection.

**How the sources are granted.** conchd calls LiveKit's `UpdateParticipant` for the sharer's identity in the room, with the whole permission spelled out: subscribe; publish; never data; sources `MICROPHONE`, `SCREEN_SHARE`, `SCREEN_SHARE_AUDIO`; nothing hidden, recorder, agent, metadata or metrics. Taking the share away sets the same permission with `MICROPHONE` alone, which is exactly the join grant, so LiveKit treats it as no change for a participant that never shared. LiveKit replaces a permission as a whole, which is why every field is written out. This is the recommended path of #443, and the measurements below show it holds; the fallback (a second identity per share, with its own token) was not needed.

Claims and stops of one room are made one at a time, and so are the withdrawals a pass makes (below), so that none of them takes away a grant another has just made. A claim is recorded before LiveKit is asked, so a pass that sees the new grant sees its holder too, and stays *pending* until its `voice_share_started` row is written; nothing ends a pending share, and presence does not show it. A claim whose row cannot be written is undone and its sources withdrawn: a share conchd cannot account for does not start.

### Stopping

`DELETE /v1/channels/{channel}/voice/share`, authenticated.

- **An operator** is let through after a verified caller and a channel that exists, member or not, and whatever their kind or voice grant: moderation is theirs, and an operator can add themselves to any channel anyway.
- **Anyone else** is checked as on every voice route, so a non-member gets the unknown-channel 404 and learns nothing; then voice configured (503).
- **No share** (or no room): 204, nothing written. **Someone else's share**, for anyone but an operator: 403 `forbidden`, audited as `access_denied`; the share goes on.
- **A release that names its share** (#487): `?started_at=` with the `started_at` of the caller's own claim, as `VoiceShareV1` gave it. It ends that share only while it is still the room's share and the caller's (`reason=self`). Otherwise, operator or not, it is 409 `voice_share_stale` and changes nothing: no row, no withdrawal, no `access_denied`. That keeps a release sent late, by a page whose claim lapsed while its picker was open, from ending a newer share, someone else's least of all, as a moderation stop. A malformed value, or one given twice, is 400 `invalid_request`, before the channel is looked at. A request naming no share is unchanged: the sharer's stop, or an operator's stop of whoever shares. The browser always names its share.
- **Otherwise** the share ends at once: it leaves presence, `voice_share_stopped` is written (`reason=self`, or `reason=operator` with the operator as actor), and then LiveKit is asked to take the sources away. A withdrawal LiveKit cannot be asked for is retried by the passes; the stop is not refused for it.

### How a share ends by itself

A pass (§6) ends a share, with its reason:

- **`left`**: the sharer is not in the room on a pass. A connection replaced between two passes is not a leaving: the share is the principal's, not the connection's, and survives a reconnect. If the new connection was made with LiveKit's own refreshed token it already has the share's sources (finding S4 below); if it was made with a fresh join token it claims again.
- **`access_lost`**: the sharer is no longer entitled (a pass finds them not a member, disabled or not human), or is removed by name at once (§5).
- **`rotated`**: the room is rotated (§5). Rotation drops every claim of the room, writes the row before anyone's `voice_left`, and deleting the room takes the grant with it. Removing or disabling a sharer who holds a session rotates the room, so this is how their share normally ends; nothing of it survives in memory, in LiveKit or in presence, and the channel's new room has no share.
- **`idle`**: nothing has been shared for a while. 15 seconds after the claim if no screen-share track has been seen at all (time to pick a window in the browser's picker), and 5 seconds after the last pass that saw one once the share has been published. Either screen-share source counts.

Ending a share takes the sources away (unless the room is gone), and LiveKit then unpublishes the share's tracks for every viewer at once.

### Enforcement

Every pass compares every entitled participant with the room's share. Anyone but the holder who has a screen-share source **granted** (LiveKit reports each participant's permission; an empty source list counts, since LiveKit reads it as every source) or **published** has the sources withdrawn, leaving them their own publish grant and never more: the room's where conchd knows it (a share is claimed only in a channel-wide room, where every member may publish; a claim for any other room is 409 `voice_no_session`), otherwise what LiveKit reports they may publish, so a listen-only participant is never made a publisher by losing a share.

- **Audit.** A published share nobody holds is audited as `voice_share_unclaimed`. A withdrawal conchd still owes (a share just ended, a claim that failed) is quiet: what the pass sees is the share being taken away. Once LiveKit has confirmed the withdrawal, a share published again, on a later listing, is a new one and is audited, which is what a client rejoining on a token LiveKit sent it during the share looks like.
- **Eviction.** If a share is still published and LiveKit will not withdraw its sources, the participant is removed from the room instead (`voice_participant_removed`, `reason=share_unclaimed`, or `reason=share_not_withdrawn` for a share that was stopped or went idle): fail closed. A stop is therefore effective within a pass even when LiveKit refuses the withdrawal: presence says nobody is sharing at once, and nobody goes on watching. A grant with nothing published is only retried.
- **Voice withdrawn.** The poller's entitlement check includes `voice_allowed` (#437): a person without voice found in a room is removed by name (`reason=voice_not_allowed`), and a share of theirs ends with `access_lost`. No route turns voice off after an invite today, so this is a guard for when one does; such a change would also need the rotation hook (§5), which does not look at `voice_allowed`.

Through conchd an unclaimed share should be impossible: LiveKit refuses a source the permission does not name (finding S1). It happens when a client reconnects on a token LiveKit sent it while it held a share, after the share ended (finding S5), and after a restart.

**Claims are held in memory, like transmit state, and a restart drops every claim.** That was a choice: no migration, and a share is short-lived. After a restart, the first pass over a room finds any share's grant still in LiveKit with no claim behind it and withdraws it; a track still published is audited as unclaimed. The sharer claims again. Presence never shows a share conchd does not hold.

**LiveKit unreachable.** No share can be granted (503). A share already held survives the outage, and the first pass after it judges it as usual: a sharer still publishing goes on, one who is gone ends with `left`. A stop during the outage ends the share at once (row and presence), and the passes take the sources away when LiveKit answers again, or remove the sharer if LiveKit lists the room but still refuses the withdrawal.

**A claim that finds its room retired** once granted (a rotation landed meanwhile) takes the grant back at once, before ending the share as `rotated` and answering 409 `voice_no_session`: deleting the room would take it too, but a `DeleteRoom` that fails leaves the room until the sweep.

### Presence

`VoiceParticipant` gains `sharing` (the participant holds the room's share; conchd's decision, true from the moment the claim is answered) and `sharing_audio` (sharing, and a screen-share audio track was published on the last pass). Both are omitted when false. Subscribers are told at once when a share starts and stops, and when its sound appears or goes. Presence never carries a room name or a token, and the share adds none.

**Why it ended, for the sharer alone (#487).** When a share ends, for any of the reasons above, the sharer's own presence (`GET /v1/channels/{channel}/voice` and the presence socket, read as them) gains `share_ended`: the reason, the share's `started_at` and when it ended. Nobody else's document carries it, and it never says who stopped the share; the audit row does. It comes in the same frame that takes `sharing` away, so the page learns that the share ended and why at once. It is kept, in memory, until that person claims a share in the channel again, and a restart drops it; a reader matches its `started_at` with the claim it held. A sharer who lost their place (`access_lost`, or a rotation because they were removed) usually cannot read presence any more: their socket is closed with its own reason.

### Audit

`voice_share_started` (actor the sharer; detail `channel=<id> audio=<bool>`), `voice_share_stopped` (actor the sharer, or the operator for `reason=operator`; detail `channel=<id> principal=<sharer> reason=<reason>`) and `voice_share_unclaimed` (actor the publisher; detail `channel=<id> sources=<screen_share,screen_share_audio> source=observed`), all with subject `channel:<id>` (§7). Every share that started gets exactly one stopped row. No room name and no token, as for every voice row.

### Sizing (O6)

Nothing on the server limits bitrate or resolution. The publisher chooses its simulcast layers (#444), and LiveKit forwards a layer per subscriber. The bandwidth of one sharer to about 29 viewers is measured on the real droplet (plan item A15).

### LiveKit behaviours the share relies on, as measured

Measured on 2026-10-16 against LiveKit server 1.13.7 (the release binary, which reports `livekit-server version 1.13.7`; the `e2e/voice` image is the same version) with automatic room creation off, bound to localhost. The participants were `lk` 2.19.0, the version `e2e/voice` pins, joined through a relay that substitutes a token signed as conchd signs it (microphone only, 15 s). `lk` publishes a file as a microphone and its demo video as a camera, so the relay rewrote the `source` of those `add_track` requests to `SCREEN_SHARE` and `SCREEN_SHARE_AUDIO`; to LiveKit that is a screen share like a browser's, since the source is what the client declares and the permission check is on the source. To publish on the connection that was granted, the relay held the share's `add_track` back until the grant had been made. The admin calls were plain JSON over HTTP, as conchd makes them.

| # | Behaviour | Result |
|---|---|---|
| S1 | The join grant refuses the screen-share sources | **Holds.** With `canPublishSources: ["microphone"]`, LiveKit logged `no permission to publish track` for the screen and never listed it; `lk` gave up after its 10 s publish timeout (`timed out publishing track`). The same for the screen's sound. |
| S2 | `UpdateParticipant` grants them on a live connection | **Holds.** HTTP 200 in about 2 ms, answering with the participant and the new permission; the held `add_track` was then accepted, and `ListParticipants` listed `SCREEN_SHARE` and `SCREEN_SHARE_AUDIO` tracks 29 ms after it was let go. A second participant subscribed to both within 0.3 s. |
| S3 | Withdrawing the sources unpublishes the share's tracks | **Holds, at once.** After `UpdateParticipant` back to `MICROPHONE` returned (0.7 ms), `ListParticipants` no longer listed either track on the very next call (0.25 ms), the viewer was told `track unpublished` for both, and the microphone and the connection were untouched. |
| S4 | The token LiveKit refreshes for a connected participant carries the updated permission | **Holds.** Each permission change makes LiveKit send the connection a new ten-minute token of its own at once (it was sent within the same millisecond as the 200): after the grant with `canPublishSources: ["microphone","screen_share","screen_share_audio"]`, after the withdrawal with `["microphone"]`, plus `canUpdateOwnMetadata`, `canSubscribeMetrics` and `canManageAgentSession` spelled out as false. LiveKit's source (`pkg/rtc/participant.go` `SetPermission` calls back `refreshToken`) says the same. A client that reconnects on the last token it was sent therefore keeps exactly what it has: a reconnect within the SDK's window neither regains nor loses sources unexpectedly. |
| S5 | A token sent during a share still grants it after the share ended | **Yes, for its ten minutes.** Reconnecting with the token LiveKit sent during the share, after the share had been withdrawn, gave the new connection the share's sources, and it published a screen. Reconnecting with the token sent after the withdrawal gave only the microphone, and the screen was refused. The passes cover the first case: a grant or a track without a claim is withdrawn, and a track is audited (Enforcement). |
| S6 | `UpdateParticipant` for an identity not in the room | 404 with the Twirp body `{"code":"not_found","msg":"twirp error unknown: participant does not exist"}`. With a grant other than `roomAdmin` for the room, 401 `unauthenticated`. |
| S7 | What `ListParticipants` says of the permission | Every participant carries `permission` with every field written out, snake case, sources as names: `{"can_subscribe":true,"can_publish":true,"can_publish_data":false,"can_publish_sources":["MICROPHONE"],"hidden":false,...}`. A token that names no sources is listed with `"can_publish_sources":[]`, which LiveKit reads as every source. A share's tracks are listed with `"source":"SCREEN_SHARE"` (type `VIDEO`) and `"source":"SCREEN_SHARE_AUDIO"` (type `AUDIO`), and `"stream":"screen"`. |

`e2e/voice` repeats S1 to S4 on every run against the pinned image (`share.go`): the permission LiveKit reports and the token it sends are checked field by field against the join grant, with and without the share's sources.

## 13. Companion participants

*Issue #897; the poller's side is #932, the issuing side is #933 (2026-10-29). Model B was accepted by Nick on 2026-10-28 ([windows-desktop.md](windows-desktop.md) §7.2, [linux-desktop.md](linux-desktop.md) §6.4, the later word). Nothing published changed: the new types are new, the new field is optional and omitted when empty.*

A person in voice is one LiveKit participant, `p<id>`, and a second connection under that identity displaces the first (§4, §9 decision 3). A desktop whose voice is in the Rust helper and whose screen is in Electron needs a second participant that `conchd` issues under a derived identity and recognises as the same person. There are two, and one route issues both.

### Identities

| Identity | Role | May publish | Needs |
|---|---|---|---|
| `p7` | the person's own connection (helper, TUI or browser) | a microphone, and the screen while the person holds the share | a session (§4), unchanged |
| `p7.screen` | the **companion**: Electron's screen publisher | `screen_share` only | the bearer, and `p7` in the room |
| `p7.view` | the **viewer**: a browser tab watching beside native voice | nothing | the bearer, a live voice entitlement, and a room someone holds a session for |

`parseVoiceParticipant` is the one reader of identities. It accepts `p`, a positive decimal with no sign and no leading zero and at most 18 digits, then nothing, `.screen` or `.view`, exactly and in lower case. Anything else (`p7.`, `p7..screen`, `p7.Screen`, `p7.screen.screen`, `p07.screen`, `screen.p7`) names nobody and is removed by the poller as `bad_identity`. A client never chooses a suffix: the identity is the token's `sub`, and `conchd` signs it. The separator is `.`, not `:` (which already means kind-of-id in audit subjects) and not `~`. The identities are opaque to clients: the web client's `principalOf` (`^p(\d+)$`) does not match a suffixed identity, which is right, since a companion never speaks.

`VoiceSessionResponseV1` gains an optional `companion_identity` (`p<id>.screen`), present on every issued session, so that a voice client that plays remote audio (the helper, #934) knows whose screen share is its own and never plays it back. It is not a credential and admits nobody to anything. `Validate` refuses one equal to `identity`.

### The route

`POST /v1/channels/{channel}/voice/companion`, authenticated, body `VoiceCompanionRequestV1` `{"publish": "screen"|"none", "subscribe": bool}`, decoded strictly (`DecodeVoiceCompanionRequestV1`: an unknown field at any depth, trailing content, `null`, a publish set outside the vocabulary, and `none` with `subscribe: false` are all 400 `invalid_request`). The grant is fixed from the validated request and nothing else. 200 `VoiceCompanionResponseV1` `{livekit_url, identity, room, token, publish, subscribe, expires_at}`, written with `Cache-Control: no-store`; the token lives 15 s as for a session.

The checks, in this order. The order is the contract, so a non-member learns nothing, and a refusal at any step issues nothing: no holder row, no LiveKit call (except the idempotent `CreateRoom` at step 11), no audit row (an agent's denial is the one audit row, and it records the denial).

| # | Check | Answer when it fails |
|---|---|---|
| 1 | a verified caller (auth on) | 400 `voice_requires_auth` |
| 2 | the channel exists and the caller is a member (operators get no exemption; a non-member agent is audited) | 404 `channel_not_found` |
| 3 | the caller is human | 403 `forbidden`, audited `access_denied` |
| 4 | the caller holds voice | 403 `voice_not_allowed` |
| 5 | voice is configured | 503 `voice_not_configured` |
| 6 | the body is one JSON object of the declared fields, within 1 KiB, and valid | 400 `invalid_request` (413 `request_too_large`) |
| 7 | the request carries a credential to record the holder under | 500 (an authenticated request always has one) |
| 8 | the channel has a live channel-wide room that someone holds a session for (a companion joins a room, it never creates one) | 409 `voice_no_session` |
| 9 | **screen only:** the caller's own `p<id>` is in that room, as the poller saw it on its last pass and not evicted since (a person present only as a viewer does not count) | 409 `voice_no_session` |
| 10 | **screen only:** no `p<id>.screen` of the caller is in that room (poller state) | 409 `voice_companion_busy` |
| 11 | the holder (room, principal, this credential) is recorded; `CreateRoom`; the caller is still entitled and the room still live, with the session route's start-over on a rotation, at most three times | 404 as if never a member; 503 `voice_unavailable`; 409 `voice_no_session` when a rotation left no room anyone holds |
| 12 | the audit row is written before the body | 500, nothing issued |

The body is read after step 5, so a non-member gets the unknown-channel 404 whatever it sends. Steps 9 and 10 read the poller's state rather than calling LiveKit, so the request costs no LiveKit round trip beyond `CreateRoom`; the state is at most one pass gap (1.5 s) old. To close the window in which a companion that has been issued a token but has not yet connected is not listed, signing a `.screen` token notes the issue time for the room and person, and for one pass gap the poller counts that companion as present: two quick requests get one token and one 409 `voice_companion_busy`. The note is cleared by DELETE (including when LiveKit says the participant is already gone), by the audit write failing (nothing was issued), and by the gap passing. What remains is a companion that connected and left within the gap. In the same way a client that asks at once after its own helper connected may be refused `voice_no_session` for up to a pass, and retries.

Steps 9 and 10 are made against the room step 8 found. If that room is rotated while LiveKit is asked, the grant does not follow the channel to its successor, whoever holds a session there: those checks never ran in it, so the answer is 409 `voice_no_session` and nothing is signed; the client asks again.

The viewer skips 9 and 10. A second viewer displaces the first at LiveKit, as a second browser tab does for the person's own identity, because a viewer holds nothing another could take. A second companion is refused, never displaced: a second device holding the same bearer must not take the share from under the first. Whoever wants to move it removes the first (below).

`DELETE /v1/channels/{channel}/voice/companion[?publish=screen|none]` removes the caller's own companion, viewer or (without the parameter) both from the channel's room, by `RemoveParticipant` with the poller's removal timeout. Checks 1 to 5, then the parameter: any other value, or a repeated one, is 400 `invalid_request`. 204 with no body whether or not anyone was removed (idempotent: Electron's main calls it on a renderer crash without knowing what LiveKit still lists); 503 `voice_unavailable` when LiveKit cannot be asked (what it had already removed is audited all the same). It never touches `p<id>`, never another person's participants, and never ends the person's share claim: a share whose tracks have gone ends by the idle rule (§12), so a renderer that crashes and is relaunched can claim again.

### Claims

Signed by the same code as a session token (`livekit.Client.JoinToken`, whose `Companion` grant has no field for a microphone, share audio or data). The person's own token is unchanged claim for claim, and `e2e/voice` and `livekit/token_test.go` assert it.

| | `p7.screen` | `p7.view` |
|---|---|---|
| `sub` | `p7.screen` | `p7.view` |
| `roomJoin`, `room` | true, the channel-wide room | same |
| `canSubscribe` | the request's `subscribe` | true |
| `canPublish` | true | false |
| `canPublishSources` | `["screen_share"]` | `[]` (an empty array, never `null`; `canPublish: false` is what withholds) |
| `canPublishData` | false | false |
| `hidden` | not set | not set |
| `exp` | issue time + 15 s | same |
| `jti` | random | same |

Nothing in the companion's grant names a microphone or `screen_share_audio`, so LiveKit refuses either from it: server enforcement, not client honesty. **The sound of a share is not in this grant.** It is added to the connected companion by a permission update on the person's explicit request, and removed when the share ends (#898); this section does not issue it. `hidden` is not used: the companion's whole purpose is a track others subscribe to, and a viewer need not be hidden since presence folds it anyway.

`subscribe: true` (sub-model B2) lets the companion receive; it must connect with `autoSubscribe` off and take only screen tracks, which is the client's honesty, not `conchd`'s enforcement. `subscribe: false` (B1) is enforced by LiveKit: the companion cannot receive voice and cannot watch. A permission update always passes the participant's own `canSubscribe` through, so a publish-only companion never gains the right to receive by being granted or stripped of its screen.

### The companion's screen follows the claim

The companion is the person's, and so is its screen. A companion is issued with `screen_share` in its token whether or not the person holds the room's share, because the token is signed before any claim and must work the moment the person claims. **A companion issued while nothing is claimed has its screen withdrawn on the next pass** (`canPublish` off, no sources, `canSubscribe` kept), and is not removed; it keeps its connection and is **given the screen back when the person claims** (`POST …/voice/share`, §12), which sets the permission of `p<id>` as today and then of the person's `p<id>.screen` to `screen_share` alone. A claim held by someone else never reaches the companion: it is withdrawn on its pass. If LiveKit will not take the withdrawal the companion is removed (fail closed, `voice_participant_removed` reason `share_unclaimed`, `participant=screen`). So the order for a desktop is: the person's own connection, the claim, the companion's publish. A companion that connects first is parked by the next pass until the claim, and a connection that arrives with the claim already held simply works.

### Presence, occupancy and teardown

The poller (#932) groups LiveKit's participants by the principal `parseVoiceParticipant` names, checks entitlement once per person, and shows one `VoiceParticipant` per principal: the join time, the microphone and `transmitting` come from `p<id>` alone; the screen-share flags are the OR over the person's participants, so a share published by the companion is the person's share (claims, stops, idle, `access_lost` and `left` apply to the person and so to both). A viewer alone in a room is the person in the room (it hears the room, so occupancy counts it, and it is audited as the person joining and leaving); a companion alone is not. Neither is audited as joining or leaving while `p<id>` is in the room.

**Orphan grace.** A `p<id>.screen` whose `p<id>` has been absent on every pass for `voiceCompanionGrace` (one pass gap plus the removal timeout, 5.5 s) is removed. A viewer has no parent and is never orphaned. Entitlement loss removes `p<id>`, `p<id>.screen` and `p<id>.view` alike.

### Audit

- `voice_session_issued` is reused for a companion or viewer, one row per issuance, actor the person, subject `channel:<id>`, detail `channel=<id> identity=p7.screen grants=companion:screen:subscribe` (or `companion:screen:publish_only`, `companion:view`). The identity in the detail is `conchd`'s own derivation, never a token or room name. The action is reused to keep the audit vocabulary closed; the detail tells the rows apart.
- `voice_companion_removed`, written when LiveKit actually removed one: actor the person for DELETE (`reason=requested`), `system` for the poller (`reason=orphaned`). Detail `channel=<id> principal=<id> participant=screen|view reason=<reason>`.
- `voice_participant_removed` for a suffixed identity carries `participant=screen|view`.

No response field but `token` and `room`, no log line, no audit row and no metric names a token, its signature or a room name on any of these paths; `e2e/voice` scans for them.

### Amendments

§4 says a principal holds one connection per room, and §9 decision 3 anticipated "a per-device identity suffix": both are amended in place (2026-10-29, #933). A principal holds one **own** connection per room (a second device displaces the first, unchanged); a companion or viewer under a derived identity is folded into the person on every wire document, so presence still lists a principal once and the one-participant-per-principal invariant of `VoicePresenceRoom.Validate` stands.

### Verified against LiveKit

`e2e/voice` scenario `companion` (live half, against the pinned LiveKit): the companion's token claims; both connections of a person stay listed; presence shows the person once; an unclaimed companion's screen is withdrawn and restored by the claim; the companion publishes a screen; a second companion is refused 409; a viewer publishes nothing and receives the share; a publish-only companion is delivered no track and its microphone is refused; share audio is refused; DELETE removes the companion and nothing else; the person leaving removes the companion within the grace and a pass.
