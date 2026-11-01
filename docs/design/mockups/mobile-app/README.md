# Conch phone app: illustrative flow prototype

**Issue:** #1107 (child of #757). **Spec:** [`../../mobile-app-ux.md`](../../mobile-app-ux.md).

A self-contained mockup of the phone app's screens and states: plain HTML, CSS and
JavaScript, with no dependency, no build step and no network. It opens from disk.

## Open it

Open [`index.html`](index.html) in any current browser (double-click it, or
`xdg-open index.html`, or drag it onto a browser window). Nothing needs to be served.
A local server is not needed and nothing is fetched: the three files here are all it uses.

## What it is, and is not

**Illustrative.** The people, channels, messages, servers and the invite code are
invented. There is no server, no audio, no microphone, no notification, no storage
and no sign-in. "Hold to talk" changes a label and a state line; it does not
capture or send sound. The system permission dialogs are stand-ins drawn by the
page, not the real ones. It shows interaction and layout. **It is not evidence that
any phone, OS, media path or accessibility service works**; those are M4, M5 and
the private-alpha records (#1109, #1110, #1119, #1120).

## Controls (left of the phone)

| Control | What it does |
| --- | --- |
| Screen | Jumps to any screen or preset state (the routes below). Also `index.html#/<route>`. |
| Size and orientation | 360 × 740 and 390 × 844 portrait; 740 × 360 and 844 × 390 landscape. |
| Text size | 100%, 130%, 160%, 200%: stands in for Dynamic Type and Android font scale. |
| Motion | Follow the computer's reduced-motion setting, force reduced, or force allowed. |
| Apply / Draw safe areas | The notch, status bar and home indicator insets, and a red overlay that shows them. |
| On-screen keyboard open | A stand-in keyboard (290 px portrait, 150 px landscape) that shrinks the screen. |
| Scenario | App installed or not; connection (connected, reconnecting, offline); microphone and notification permission; the server's voice grant (may speak or listen only); make the next send fail; simulate a lock, call or app switch. |

Tab and Shift+Tab move through the phone in reading order. Hold to talk is held with the
pointer, or with Space or Enter held down. Escape closes a sheet and returns focus to what opened it.

## Routes

Every route is in the Screen list and works as `index.html#/<route>`.

| Route | Shows |
| --- | --- |
| `link` | The invite link opened in a web browser: Open in Conch, Get the app, Copy link, Continue in browser. Toggle "App is installed". |
| `paste` | In-app Add a server: paste the invite (the manual fallback), with validation. |
| `confirm` | Confirm the server: address, who invited you, channels. |
| `account` | Create your account (name, password twice). |
| `signin` | Sign in, for a returning member. Type the password `wrong` for the refusal. |
| `channels` | Channel list; choosing one opens Chat and never joins voice. |
| `chat`, `chat-trouble`, `chat-voice-elsewhere` | A conversation; failed and not-confirmed sends; chat while voice is in another channel. |
| `voice`, `voice-joined`, `voice-elsewhere`, `voice-listen`, `mic-denied` | Voice not joined; joined; joined in another channel; listen only; microphone denied. |
| `servers` | Switch server or account (asks first while in voice). |
| `settings` | Account, notifications (asked only from here), voice options, sign out. |
| `gone` | A channel that is not available (stale link or notification). |
| `revoked` | Signed out by the server. |

## Checked

Run on one machine in headless Chromium (Linux), by script: every route at the four sizes,
at 100% and 200% text, with and without the keyboard. No script error and no request outside
`file:`; no horizontal overflow; every button and field at least 44 × 44 px at 100% text;
Send reachable by a tap at its centre. A scripted pass of the invite, join, voice, switch,
send-failure and offline paths, with the focus moves described in the spec. That is a layout
and logic check of a mockup, not a test of a phone, a screen reader or a real keyboard.
Not run: VoiceOver, TalkBack, Switch Control, a real on-screen keyboard, a real notch,
iOS Safari, Android Chrome.
