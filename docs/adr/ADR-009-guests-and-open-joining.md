# ADR-009: Guests, open joining and signing in again

- **Status:** Proposed. It is accepted when Nick merges the PR that adds it.
- **Date:** 2026-10-21
- **Decider:** Nick (Tier-H)
- **Proposal:** [#694](https://github.com/njdaniel/conch/issues/694)
- **Amends:**
  - [ADR-008](ADR-008-private-communications-alpha.md)'s "No public signup" (its *Unchanged* list): an operator may turn on joining from the server's bare address. It is off by default.
  - ADR-008 §4's rule that an invite always creates a full member: an invite may now create a guest instead, at the joiner's choice.
  - [docs/design/private-alpha.md](../design/private-alpha.md) §3 ("Invites are the way in. There is no public signup.") and its O2 ("there is no separate guest kind"): both are superseded by this ADR.
  - [ADR-000](ADR-000-charter.md)'s private-alpha amendment, which keeps "password and OIDC login" at P7: self-service sign-in comes forward now, as passkeys and a member-made device link. Password login stays at P7 and is at most a later fallback; OIDC stays at P7.

  It changes neither single-tenancy, nor the deployment invariant, nor the operator's one-tab sign-in (D2 of #260), nor the lasting member sign-in (O1, #439).
- **Design:** [docs/design/guests-and-accounts.md](../design/guests-and-accounts.md)

## Context

The alpha's testers get in through invites only (ADR-008 §4). Two gaps showed up once people used the local stack (#694, 2026-10-21):

- Nick wants someone who has the server's address, with or without an invite, to be able to open it and either **join as a guest** or **create an account**.
- A member who signs out, or opens Conch on a new browser or device, **cannot get back in by themselves**. Conch has no password, passkey or email sign-in. After sign-out, an operator must make a one-use sign-in link (#493), or the member pastes a raw credential token they have never seen.

Nick took six decisions on #694 on 2026-10-21 ("use your recommendations"). This ADR records them; the design doc turns them into flows, a model and issues.

## Decision

1. **Open joining is an operator setting, off by default (D1).** With it off, the bare address offers only *Sign in*. With it on, the bare address offers *Join as a guest*, *Create an account*, or both, as the operator chose. Joining is rate-limited per client address and server-wide, can be paused like invites, and every change of the setting is an audit event.
2. **A guest is a real principal, marked guest (D2).**
   - It holds exactly one credential, kept in the browser it joined from, which lasts 7 days from its last use (or the invite's shorter lifetime); a guest that stays away for 7 days expires, and is out the moment its credential is.
   - It sees only what its invite link granted, or the server's *open channels* when it came from the bare address. It gets voice, and may watch a screen share, only if that was granted.
   - It cannot invite, cannot request or decide approvals (it never holds the deciding right, and an operator cannot grant it one), cannot see members outside its channels, and cannot open one-on-ones.
   - It is shown with a "guest" badge. Its messages stay after it expires, attributed to "<name> (guest)"; its name is freed when the server hides the expired guest: at once if it posted nothing, 90 days on if it did.
   - It can become a member ("Keep this account") and keeps its name, channels and history. A guest from the bare address may keep its account only while open joining still offers accounts and is not paused; a guest from an invite may at any time; an operator may keep any guest.
3. **Signing in again is passkeys plus Add a device (D3).**
   - A member may register one or more **passkeys** (WebAuthn) and sign in with one on any device that has it. The server needs a Go WebAuthn library; the design names it at an exact version for Nick's sign-off (depgate).
   - **Add a device:** a signed-in member makes a one-use link, or a QR code of it, for their own next device. It is the operator's sign-in link of #493, made by the member for themselves, with a short life.
   - The operator's sign-in link stays, as recovery for someone who lost every passkey and device.
   - A password (argon2id) is not built now; it stays a possible later fallback. Rule 1 holds: no mail server, and no other process, is needed to join or sign in.
4. **Names are unique per server, case-insensitively, with look-alikes refused and a reserved list (D4).** The first two already hold (#441); this adds the reserved list (operator, admin, system, the server's own name and their kin), applied wherever a person chooses their own name.
5. **Abuse controls (D5):** per-address rate limits on every anonymous route that creates something, using the trusted-proxy client address of #609; a cap on live guests; a pause-joining switch; bulk revocation of everyone who came in from the bare address; and audit events for every guest, account, passkey, device link and setting change.
6. **The isolation guarantee extends to guests and to the bare-address path (D6).** `e2e/isolation` adds a guest from an invite link and guests from the bare address, and checks that they find nothing of any group they are not in, that revoking them is as fast as revoking a tester, and that the bare address lets nobody in while joining is off.

## Alternatives considered

| Option | For | Against |
| --- | --- | --- |
| **Open joining as an off-by-default operator switch (chosen, D1)** | The alpha keeps its invite-only default; turning it on is one audited operator decision; it can be paused like invites. | One more setting to document and test. |
| Invite only, always | Nothing new to defend. | Does not give Nick what he asked for: someone with just the address cannot get in. |
| Always open | Simplest landing page. | A public server with no way to close the door; contradicts the alpha's private character. |
| **A guest as a real principal with a guest mark (chosen, D2)** | Every existing check (membership, audit, revocation, isolation) applies as it is; history stays attributable; becoming a member is a flag change. | A second kind of human to carry a badge for, in every client. |
| Guest as an anonymous, unstored session | No new rows. | No audit trail, no revocation by id, no history, and nothing to keep when the guest wants to stay. |
| **Passkeys plus Add a device (chosen, D3)** | No secret on the server to leak; works on phones; Add a device covers every browser with no dependency at all. | A Go dependency with a sizeable graph; passkeys need a secure context and a stable host name, so a bare IP or a LAN address without TLS cannot use them (Add a device and the operator link cover those). |
| Password with argon2id | Universal; one small dependency (`golang.org/x/crypto`). | A secret to store, to reset and to phish; no reset path without a mail server, which Rule 1 forbids requiring. Kept as a possible later fallback. |
| Email magic link | Familiar. | Requires a mail server for the only sign-in path, which Rule 1 forbids; could only ever be optional. |
| HttpOnly session cookie instead of the bearer in localStorage | The script cannot read it. | Needs CSRF protection on every write route, which Conch does not have (O1 of private-alpha.md weighed this already). |

## Unchanged

- No end-to-end encryption, no federation, no multi-tenancy, no recording.
- No standalone human DMs before P7; a one-on-one is still a two-member private channel, and a guest cannot make one.
- An invite grants only what its maker holds, and never operator rights. Agents are still made only by an operator; neither an invite nor the bare address ever makes one.
- The operator's sign-in lasts one tab (D2, #260); a member's lasts until sign-out (O1, #439).
- **Rule 1:** `conchd` needs no other process to let someone join, sign in, add a device or recover an account.
- **Rule 4 (API parity):** every new action is a REST route first; the browser, the CLI and the TUI use that. Where a platform cannot do a thing at all (a terminal cannot run a WebAuthn ceremony), the design says so and gives the terminal its own way in (the device link).
- `pkg/schema` is the source of every new wire shape, through the schema-change skill.

## Consequences

- **New security surface.** Three anonymous routes that create or sign in something (join as a guest, create an account, passkey sign-in) join the two that exist (invite and sign-in-link redemption), plus self-service device links and passkey registration. Every implementation PR gets a `security-reviewer` pass; the design's threat notes get one before the issues are filed.
- **A new Go dependency, with Nick's sign-off at an exact version.** The design measures `github.com/go-webauthn/webauthn` and lists every module it brings and each one's licence. Its current release requires Go 1.26, one minor above `go.mod`'s 1.25; the design says what that costs and what the alternative is.
- **Schema additions, all compatible:** a guest mark and an origin on principals and on the shapes that carry them, an open-joining flag on channels, open-joining settings, join routes, passkey shapes, self-service sign-in links, and new audit actions. No published shape changes meaning; the schema-change skill decides each case.
- **Storage:** new columns on `principals` and `channels`, a `passkeys` table, new `server_settings` keys, and a guest-expiry sweeper inside `conchd`.
- **Clients:** the browser gets a landing page for the bare address, a guest badge, "Keep this account", passkey registration and sign-in, and Add a device. The CLI gets the operator's side of all of it and `conch login` from a device link; the TUI shows the badge.
- **The isolation check grows a third group** (guests) and new matrix rows; its coverage check will fail until every new route has a row or a stated exemption.
- **Documents that still say "no public signup"** (ROADMAP.md's alpha paragraph, private-alpha.md §3 and O2) get a one-line pointer to this ADR in the PR that adds it.
