# P3 planning brief: a local multi-client prototype

**Status:** Planning brief; no commands or prototype claimed built. The plan made from it is on #260.
**Plan:** [P3 planning issue #260](https://github.com/njdaniel/conch/issues/260).
**Owner request:** Nick, 2026-10-13: test on his computer with multiple TUI clients,
a browser, and AI or VM participants.
**Decision:** [ADR-007](../adr/ADR-007-client-interfaces.md).
**Behavior:** [client experience](client-experience.md).

## 1. Outcome

From a clean checkout on Nick's Linux computer, start one local `conchd` and a
repeatable test community. Open two real TUI processes and a browser client at once.
Exchange messages, see authorized presence and agent work, and exercise restart,
reconnect and permissions. This is a usable end-to-end slice, not just static
screens. Preserve P3's agent presence/session/run purpose.

Default tests need no VM, paid API, live microphone or cloud dependency. VMs and
real models extend the tests after the deterministic baseline works. Do not wait
for Electron packages, translation or screen sharing to reach this milestone.

## 2. Minimum topology

| Participant | Identity/session | What it proves |
| --- | --- | --- |
| Server | One disposable local instance/data directory | All clients see one authoritative community. |
| TUI A | Human `nick-test`, credential A, isolated profile | Normal terminal interaction. |
| TUI B | Human `guest-test`, credential B, different profile | Another human, independent navigation and permissions. |
| Browser | Human `browser-test`, dedicated browser profile | Real browser/TUI read-write interoperability. |
| Scripted agent A | Agent `worker-test`, own credential/manifest | Registration, heartbeat, task progress, deterministic response. |
| Scripted agent B | Agent `observer-test`, restricted manifest | Different audience/capabilities, idle/offline transitions and denied actions. |
| Repeat-login variant | Browser or extra TUI as `nick-test`, separate credential/session | One principal with several sessions; closing/revoking one does not incorrectly erase the other. |

Fixture channels: a shared `general`, a restricted `test-private`, and a channel
with two overlapping text nets. Fixtures distinguish channel membership, net
member/monitor roles and agent capabilities. Do not reuse the operator credential
as an ordinary participant or give every fixture identical broad permissions.

One browser profile's tabs generally share auth; use separate profiles/contexts to
test different humans. P3 must expose a documented per-client config path/profile
mechanism and test it. Do not prescribe a fictional `--profile` flag before it exists.

## 3. P3 minimum versus subsequent work

| Required P3 prototype slice | Follow-on / not an exit blocker |
| --- | --- |
| Existing issued-credential authentication with a usable local setup path | Password login/OIDC, public onboarding and polished invitations |
| TUI + small React browser lobby, channel selection and send/history/live chat | Full desktop installer and complete P7 graphical feature parity |
| Authorized presence, durable agent IDs, separate live sessions and heartbeat expiry | Every advanced presence preference or social feature |
| Basic run lifecycle and timestamped activity; agent inspection in both clients | Rich timelines, assemblies and deliberation |
| Two deterministic agents and one opt-in real runtime/model integration | Simultaneous fully featured Claude, Codex and local adapters |
| Documented isolation, reset, logs without secrets and restart tests | Large-scale hosting and production performance claims |
| Text/nets permission parity through existing server rules | V5 layered voice, translated captions, screen sharing and recording |

The original roadmap's multi-runtime demonstration remains the wider P3 objective.
Plan the above minimum as the first testable slice, then enumerate what remains to
complete P3. Do not call the entire phase complete just because the first demo runs.

## 4. Required test scenarios

| ID | Action | Observable acceptance |
| --- | --- | --- |
| P3-01 | Start server, 2 TUIs, browser and 2 scripted agents | All connect with separate credentials; real views show the intended authorized principals/sessions, not hard-coded mockup entries. |
| P3-02 | Send from each human client and permitted agent | Each authorized peer sees one ordered message; history agrees after reload, no optimistic/live-event duplicate. |
| P3-03 | Browse different channels concurrently | Each session keeps its own selection and draft; one client's navigation affects no other client. |
| P3-04 | Repeat login as one human in two clients | One person with distinct sessions; disconnecting one leaves the other active; separate-credential revocation affects only its scope. |
| P3-05 | Post in restricted channel, nets and whispers | Authorized clients see the event/history/search result; outsiders cannot fetch or subscribe to it, even by direct API call. |
| P3-06 | Drive deterministic agent through idle/running/waiting/completed and failed runs | TUI and browser expose real session/run IDs, current task, outcome and activity in agreement. |
| P3-07 | Stop an agent or its heartbeat | Session becomes stale/offline within the documented threshold; remote task outcome is not invented; reconnection does not duplicate durable identity. |
| P3-08 | Send supported instruction/cancel to an agent | Adapter acknowledges and enforces the supported operation; unsupported operations are absent/disabled with a reason. |
| P3-09 | Drop client connection, then restart server | Stale status, bounded retries and restored authorized history; no silent replay of an ambiguously delivered send. |
| P3-10 | Remove channel membership/revoke a fixture credential | Live access ends; unauthorized read/write/reconnect fails; other participants remain usable. |
| P3-11 | Run without LiveKit or a model service | Human chat, deterministic agents, approvals and audit core remain usable; optional features show unavailable. |
| P3-12 | Reset the test environment | Only its owned processes/data are removed; repeated runs are isolated and do not touch personal client credentials or another Conch instance. |
| P3-13 | Opt in to one real runtime/local model adapter | A real task and activity stream appear with declared capabilities; provider unavailable is explicit, no hidden paid fallback. |

Retain protocol integration tests, real TUI PTY interaction tests, and browser
automation against a running server. Pure REST tests do not establish client UX.
Use deterministic fixture timing and recorded heartbeat thresholds. Test messages,
identity, audience and outcomes; pixel snapshots alone are insufficient.

The operator-assisted walkthrough must run from documented commands on a clean
checkout. P3 planning chooses actual commands and scripts; this brief deliberately
does not present unimplemented launcher names as working instructions.

## 5. AI participants and test autonomy

Scripted agents use the same permitted interfaces, credentials and manifests as
real agents. They generate predictable messages and status transitions, including
errors; they cannot bypass authorization through test-only server routes.
Bound response counts, execution duration and retries so agents cannot echo forever.

An optional real adapter can use an existing local model/runtime. Credentials stay
local and user-provided; paid provider access is opt-in. A simulated agent verifies
plumbing, not language-model quality or real runtime cancellation. Record those
claims separately. P3 planning chooses one reference integration first and defines
its exact intervention capabilities rather than promising all runtimes at once.

## 6. Optional VM/LAN extension

After the local baseline, run a Linux guest connected to the same test server.
Document the host address, explicit bind/listen settings, guest networking and
firewall requirements. A guest's `localhost` refers to the guest, not the host.
Keep test credentials scoped; localhost is the default, LAN exposure is deliberate.
For non-loopback deployments use an appropriate TLS/reverse-proxy setup. Browser
media secure-context restrictions are separate from text connectivity.

Repeat P3-02, P3-05, P3-09 and P3-10 across host/guest. Record host/guest OS, browser,
network mode and disconnect/recovery behavior. Containers help run server/agent
processes but do not prove native desktop input, sound hardware or another OS.
Windows/macOS testing is later platform work, not inferred from a Linux VM pass.

## 7. Optional voice extension

When the V4 prerequisites are ready, add self-hosted LiveKit and headless clients
with tone/WAV sources and counting/virtual sinks. Keep those independent of real
microphone access. Verify PTT gating, mute/deafen, disconnects and permissions.
Only explicit interactive runs open real devices; default tests must not record
the room or play sound. Use headphones for multi-client hardware testing.

Synthetic participants can test routing and silence while PTT is released. They
cannot replace V4's three-person, real-device/LAN exit session (#189), nor prove
browser voice before a browser media path exists. Preserve V4/V5/V6 gates.

## 8. Next planning session

1. Review/accept ADR-007 and the scoped P3 roadmap amendment.
2. Inventory current APIs and client code; identify gaps for human/session presence,
   agent runs/activity, browser authentication, schema bindings and profile isolation.
3. Decide same-principal session semantics, heartbeat timing, disconnect/reconnect
   cursors, intervention acknowledgments, and metadata visibility.
4. Define browser authentication/storage and origins without weakening current auth.
5. Select the minimal real adapter and static browser asset-serving approach.
6. Break work into an epic and cold-executable child issues in dependency order:
   contracts/auth first, server lifecycle next, adapters and client views, fixture
   runner, then interop/permissions tests and the clean-checkout walkthrough.
7. Link each issue to UX IDs and P3 test rows, required tests, dependencies and a
   concrete phase exit. Keep future media screens out of the first implementation slice.

This is a planning handoff, not the implementation issue breakdown. The first
demo should let Nick use Conch himself from several clients, then support an
evidence-based decision about the next slice.
