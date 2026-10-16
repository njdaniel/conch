# ADR-008: A private communications alpha

- **Status:** Proposed. It is accepted when Nick merges the PR that adds it.
- **Date:** 2026-10-15
- **Decider:** Nick (Tier-H)
- **Proposal:** [#420](https://github.com/njdaniel/conch/issues/420)
- **Amends:**
  - [ADR-000](ADR-000-charter.md)'s non-goals: the full multi-human GUI before P7, and video or screen sharing before P9;
  - [ADR-003](ADR-003-multi-human-access.md)'s clients (the TUI/CLI and `conch-voice` only);
  - ROADMAP.md's rule that people sign in only with a credential an operator issues, until P7 (the P2 exit and P7 paragraphs);
  - the P3 plan's D2 ([#260](https://github.com/njdaniel/conch/issues/260)), that the browser keeps its credential per tab, *if* the plan's open decision O1 is taken;
  - [ADR-004](ADR-004-voice-via-livekit.md)'s "no browser client" and screen sharing at P9;
  - [ADR-007](ADR-007-client-interfaces.md)'s bound of the browser to a P3 prototype.

  It changes neither [ADR-006](ADR-006-rust-voice-client.md) (the native Linux client) nor single-tenancy. ADR-004's gate, that #87's by-hand checks close before V4, stays on the native client. Browser voice is held to the alpha's own exit instead (plan item A16).
- **Design:** [docs/design/private-alpha.md](../design/private-alpha.md)

## Context

P3 is done (#423). Nick now wants Conch used by people, not only by him and his agents:
- invited testers in several separate groups, about 30 online at once;
- chat, talk in voice, share a screen, and survive a dropped connection;
- on one server he deploys on DigitalOcean.

The testers run Linux, Windows, macOS and phones. Today voice exists only as `conch-voice`, a native Linux binary built from source. Screen sharing does not exist and the charter places it at P9. A person can get in only with a bearer credential that an operator issues and hands over by hand.

Nick's decisions, recorded on #420 (2026-10-15):
- **Clients:** every platform above.
- **Push-to-talk:** a talk key that works while a game has focus is *not* needed for the alpha.
- **Screen share:** needs the shared computer's sound where possible.
- **Invites:** testers may invite testers, and an operator may put out a time-limited open link.
- **One server:** each group lives in its own private channels.

## Decision

1. **The browser is the alpha's client on every platform.** It is the React client, served by `conchd` itself (`-tags conchweb`), and does chat, voice and screen share. It is pulled forward from P7 for invited testers.
   - The TUI stays first-class.
   - `conch-voice` stays the native Linux voice client (ADR-006), and the only one with a talk key that works while another app has focus.
   - A desktop app with a global talk key (Electron, ADR-007) comes after the alpha.
2. **Browser voice uses the LiveKit JavaScript SDK**, as ADR-007 named it, in the same rooms and under the same server rules as `conch-voice`.
   - The browser offers an open microphone with voice detection, mute and deafen, and push-to-talk while the tab has focus.
   - It cannot offer a talk key while another app has focus; the alpha says so plainly.
3. **Screen sharing moves into the alpha from P9.** One person shares into a channel's voice room and the others in that room watch it.
   - The share carries sound where the browser provides it. As the browsers document it today, to be verified in plan item A11:
     - Chrome and Edge share a tab's sound on every desktop OS, and the whole system's sound on Windows. macOS may have a whole-screen path in recent Chrome.
     - Firefox and Safari share no sound.
   - Phones can watch. iOS cannot share; Android Chrome may be able to, which A11 checks.
   - Recording stays out.
4. **Invite links are a second way to get a credential**, beside an operator issuing one. There are two kinds:
   - **Personal invites:** single-use, made by testers an operator lets invite.
   - **Open links:** made by operators. Anyone with the link may join until it expires, with an optional use cap.

   Rules for both:
   - An invite grants only what its maker holds: named channels, voice, and whether the joiner may invite in turn. It never grants operator rights.
   - Every account records the invite it came through.
   - An invite always creates a human principal; agents are still made by an operator.
   - Redemption is rate-limited, and an operator can pause all invites.

   Invites are new security surface: their implementation gets a `security-reviewer` pass, and their wire shapes go through the schema-change skill.
5. **One server, one org, still.** Separate groups are separate private channels on one server; a one-on-one is a private channel with two members (D12 holds: no standalone DMs). A group that needs harder separation gets a second server; the deployment is laid out so that is one more instance.

## Alternatives considered

| Option | For | Against |
| --- | --- | --- |
| **The browser everywhere (chosen)** | One client for every platform, phones included. `conchd` already serves it. Nothing to install. LiveKit JS is mature. | No talk key while another app has focus. Share sound depends on the browser. |
| Native clients per OS now | A global talk key; full system sound | Windows, macOS and phone clients would each be new work. `conch-voice` is Linux/PipeWire only (ADR-006) and built from source. Far longer before anyone tests. |
| Electron desktop app now (ADR-007) | A global talk key and system sound on desktops, sharing the React code | Packaging, signing and updating for three OSes before the first tester, and no phones. It belongs after the alpha, once the browser client works. |

## Unchanged

- No public signup.
- No end-to-end encryption.
- No federation.
- No multi-tenancy.
- No recording.
- No standalone human DMs before P7.
- **Rule 1:** `conchd` needs no other process for messaging, approvals or audit. LiveKit stays optional:
  - without it, chat and invites work, and voice and screen share say they are unavailable;
  - with it, losing it fails media closed and leaves chat working.
- **Rule 4 (API parity):** invites and their administration are in the REST API first, and the clients use that.
- `pkg/schema` is the source of every new wire shape.
- The TUI and CLI remain supported clients.

## Consequences

- **The web client gets real media code.** These on `main` forbid it today and change with this decision. The security review checks each change.
  - The hygiene test (`web/src/hygiene.test.ts`) forbids:
    - `getUserMedia`, `mediaDevices` and `getDisplayMedia`;
    - any `http://` or `https://` literal;
    - `localStorage` in the built bundle;
    - credential and `sessionStorage` handling outside `api.ts`.
  - Its CSP allows `connect-src 'self'` only.
  - The voice e2e asserts a microphone-only grant.

  The LiveKit JavaScript SDK is a new npm dependency and needs Nick's sign-off.
- **The server's LiveKit grant gains screen-share sources,** with a server rule for who may publish them. Screen share sizes the deployment: one sharer to about 29 viewers is roughly 30–90 Mbps out. That figure is measured before the alpha opens.
- **Invites add schema, storage, routes, audit events, a landing page and rate limits.** Per-address limits behind a reverse proxy need the proxy's client address. Today `conchd` reads no client address at all, neither `RemoteAddr` nor `X-Forwarded-For`.
- **The browser's sign-in has to last beyond one tab** for testers to use it day to day. Today it is per tab, by the P3 plan's D2 (#260, approved by Nick). The design note's O1 proposes reversing it, which needs Nick's yes.
- **The voice track's native exit (#185, #189) stays open** for the Linux client. The alpha does not wait for it, since its testers use the browser.
- **ROADMAP.md is reordered.** The alpha comes first. P4/P5 (assemblies, deliberation) are deferred, not removed. Screen sharing moves out of P9. Onboarding moves out of P7. Live transcription follows the alpha.
