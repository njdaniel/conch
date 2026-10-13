# ADR-007: Client interfaces and a local multi-client P3 prototype

- **Status:** Accepted (Nick, with the merge of #261)
- **Date:** 2026-10-13
- **Decider:** Nick (Tier-H)
- **Proposal:** [#259](https://github.com/njdaniel/conch/issues/259)
- **Amends:** ADR-000's early-GUI non-goal and D1/D5 client scope;
  the P3/P7 boundary in ROADMAP.md. Extends ADR-006's client lifecycle without
  changing its Linux/PipeWire implementation or silently approving other audio backends.

## Context

Nick wants the generated Conch designs to guide implementation, and P3 planning
to produce a prototype he can test on his own computer: several terminal clients,
a browser client, and AI participants connected to the same server. Different
identities and repeated logins as the same identity must both be testable. VMs
are an optional extension, not a prerequisite for trying the product.

The current charter allows only a read-only dashboard before P7. That is too
narrow for this concrete P3 interoperability test: the browser must participate
in chat and expose agent activity. A bounded prototype can establish the shared
UI foundation without pulling every P7 or media feature into P3.

## Decision

| Surface | Stack / boundary | Scope |
| --- | --- | --- |
| Terminal | Go, Bubble Tea and Lip Gloss | Keep the existing TUI and plain CLI; no Rust TUI rewrite. |
| Browser | React, TypeScript, Vite | Client-rendered web application using the public REST/WebSocket API. |
| Desktop | Electron hosting the same React application | Target Linux, Windows and macOS; add narrow native integrations behind an adapter. Packaging is not a P3 exit requirement. |
| Native media and input | Rust `conch-voice` | Managed by native clients; preserve ADR-006. OS-specific backends require later design and validation. |
| Browser media | LiveKit JavaScript SDK; React media components as useful building blocks | Browser permission and capture limits apply. No requirement to install the Rust helper to use browser text chat. |
| AI | MCP and public APIs, runtime adapters | A protocol surface with explicit agent identities, not a fourth visual frontend. |
| Server | Existing Go `conchd`, embedded SQLite | Remains authoritative for identity, audience, permissions and durable records. |

Build one graphical design system and most graphical views once. Browser and
desktop supply different platform adapters for media, credentials, notifications,
and native input. TUI shares behavior and protocol contracts, not React components.
`pkg/schema` remains the source of wire shapes; TypeScript binding generation or
fixture checks must be selected in P3 planning. Do not invent a second protocol.

Vite is a build/development tool, not a required production server. The prototype
must include a repeatable route to serve its static browser build from `conchd`
without Node running. The exact asset layout/embedding is a P3 implementation
decision. A development server may be optional. No mandatory cloud service,
Redis, separate database, or paid model is introduced.

### Client lifecycle

The user starts one client, selects a server, and explicitly joins voice when
desired. A native client starts, monitors and stops its bundled voice component;
the normal experience never requires manually launching a second executable.
Keep standalone `conch-voice` commands for development/diagnostics.

Specify a versioned local control contract before implementing supervision.
Scope a helper to its owning client session/profile; do not use a global singleton
that mixes credentials between concurrently running clients. Helper failure leaves
text chat available and fails transmission closed. Cleanup must not kill another
client's helper. No credentials in child arguments, logs or activity streams.

Browser and native media are alternative implementations of the media adapter:
one owner captures/publishes a given microphone stream per session. Do not publish
through both LiveKit JS and Rust accidentally. Native screen-video rendering,
system audio and Rust voice coordination require a measured integration design;
choosing Electron does not resolve that bridge by itself.

### P3 boundary

P3 retains agent identity/session presence, basic runs/activity, and an adapter.
Add a small read/write browser lobby, text chat, agent view, and isolated local
multi-client test setup beside the TUI. This is an explicit exception to the
early read-only GUI restriction, not delivery of the full P7 product.

The baseline is two independently configured TUI processes, one browser session,
and two deterministic agent sessions. An opt-in real runtime/model adapter is
also exercised. Repeated login as one identity is tested separately from separate
users. VM/LAN and optional voice tests extend this baseline; they do not replace it.
See the [P3 planning brief](../design/p3-local-prototype.md).

Full desktop distribution, Windows/macOS native audio, translated captions,
screen sharing, assemblies, human DMs, OIDC, and recording do not become P3 gates.
V4–V6 continue in parallel with their existing exit criteria.

## Alternatives and tradeoffs

| Alternative | Why not selected as the starting point |
| --- | --- |
| Tauri + shared React | Attractive Rust integration and smaller distribution; system WebView differences add media validation work across Linux, Windows and macOS. Keep it as an alternative if an Electron spike fails the requirements. |
| Separate native desktop UI | Adds a second graphical implementation and extra work to maintain browser parity. |
| Rewrite the TUI in Rust/Ratatui | Does not solve the user need and discards the existing Go TUI investment. |
| Browser-only product | Useful prototype, but cannot promise native global PTT/device integration while another app has focus. |
| Keep all interactive browser work until P7 | Prevents the explicitly requested mixed browser/TUI P3 test. |

Electron is chosen as the working desktop direction for its browser-media and
desktop capture integration. It carries a larger runtime, resource cost and
ongoing update responsibility. This is a tradeoff, not a guarantee of equivalent
capture support on all operating systems. React/TypeScript adds another toolchain.
No new dependency or version pin is installed by this documentation PR.

Electron must keep Node integration out of the renderer, enable context isolation
and sandboxing, and expose only a narrow validated preload bridge. Loading a
server's remote web content into a privileged desktop renderer is not the design.
Web authentication storage, origin/CSRF policy and WebSocket authentication need
an explicit security design in P3; an operator token must not become browser defaults.

## Validation before desktop/media commitments

| Experiment | Evidence to retain |
| --- | --- |
| Linux global PTT on Nick's desktop | Press and release, Mouse 4, other-window focus, unplug/replug, grabs/lost release; no stuck transmission. Keyboard shortcuts alone are insufficient proof. |
| Capture video and system audio | Screen/window selection, denied permission, source closure, audio selection and stop behavior on each supported OS. Measure achievable resolution/FPS, latency and CPU; do not promise 1080p60 from a mockup. |
| Native helper lifecycle | Two concurrent clients with isolated profiles; no duplicate capture, shared credentials, orphan process or interference on exit. |
| Browser behavior | Text baseline and clear unsupported-media states; secure-context/permission behavior and background-tab recovery for each tested browser. |
| Packaging | Bundled helper compatibility, clean-machine startup and upgrades per OS; defer release claims until measured. |

Run Linux first. Record browser/OS versions and results; mark untested Windows/macOS
capabilities unverified. A failed experiment prompts a documented design revision,
not an automatic framework switch or weakening of PTT behavior.

## Sources and related documents

Framework references consulted 2026-10-13; exact versions are chosen and pinned
when implementation starts.

- [Bubble Tea](https://github.com/charmbracelet/bubbletea) and [Lip Gloss](https://github.com/charmbracelet/lipgloss)
- [LiveKit React building blocks](https://docs.livekit.io/reference/components/react/concepts/building-blocks/)
- [Electron desktop capture](https://www.electronjs.org/docs/latest/api/desktop-capturer) and [global shortcuts](https://www.electronjs.org/docs/latest/api/global-shortcut)
- [Tauri WebView versions](https://v2.tauri.app/reference/webview-versions/)
- [UX specification](../design/client-experience.md), [mockups](../design/mockups/README.md)
- [ADR-000](ADR-000-charter.md), [ADR-006](ADR-006-rust-voice-client.md), [roadmap](../../ROADMAP.md)

## Acceptance record

Nick authorized the stack, UX and P3 local prototype direction on 2026-10-13. The
same day he approved the P3 plan built on this ADR, with its eleven decisions, on
[#260](https://github.com/njdaniel/conch/issues/260). This text is accepted by the
merge of #261, made on his instruction; the charter and roadmap were updated in the
same pull request.

Accepting this ADR does not mean any of it is built. Dependency pins, wire changes
and media release gates remain separate decisions, each signed off where it lands.
