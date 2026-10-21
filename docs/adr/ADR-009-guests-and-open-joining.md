# ADR-009: Guests, open joining and signing in again

- **Status:** Proposed. It is accepted when Nick merges the PR that adds it.
- **Date:** 2026-10-21 (revised the same day after Nick's review of PR #701: passwords first)
- **Decider:** Nick (Tier-H)
- **Proposal:** [#694](https://github.com/njdaniel/conch/issues/694)
- **Amends:**
  - [ADR-008](ADR-008-private-communications-alpha.md)'s "No public signup" (its *Unchanged* list): an operator may turn on joining from the server's bare address. It is off by default.
  - ADR-008 §4's rule that an invite always creates a full member: an invite may now create a guest instead, at the joiner's choice; and a member it creates has a password from the start.
  - [docs/design/private-alpha.md](../design/private-alpha.md) §3 ("Invites are the way in. There is no public signup.") and its O2 ("there is no separate guest kind"): both are superseded by this ADR.
  - [ADR-000](ADR-000-charter.md)'s private-alpha amendment, which keeps "password and OIDC login" at P7: password login comes forward now, for members and operators. OIDC stays at P7.
  - The P3 plan's D2 (#260) for operators only in how they get their one-tab credential: by name, password and a one-time code, rather than by pasting a token. The one-tab rule itself is kept.

  It changes neither single-tenancy, nor the deployment invariant, nor the lasting member sign-in (O1, #439).
- **Design:** [docs/design/guests-and-accounts.md](../design/guests-and-accounts.md)

## Context

The alpha's testers get in through invites only (ADR-008 §4). Two gaps showed up once people used
the local stack (#694, 2026-10-21):

- Nick wants someone who has the server's address, with or without an invite, to be able to open
  it and either **join as a guest** or **create an account**.
- A member who signs out, or opens Conch on a new browser or device, **cannot get back in by
  themselves**. Conch has no password, passkey or email sign-in. After sign-out, an operator must
  make a one-use sign-in link (#493), or the member pastes a raw credential token they have never
  seen. An operator's own sign-in is the same pasted token.

Nick took six decisions on #694 on 2026-10-21 ("use your recommendations"). The first design
chose passkeys. Nick reviewed it on PR #701 the same day and asked for three things: that no
account ever be called ready without a usable way back in; that operators get a normal,
repeatable sign-in with stronger requirements; and that the unavailable and recovery experience
be spelt out. He then decided "passwords being probably an easier option than passkeys. so lets
just start with passwords first", and approved the hashing dependency. This ADR records the
decisions as they now stand; the design doc turns them into flows, a model and issues.

## Decision

1. **Open joining is an operator setting, off by default (D1).** With it off, the bare address offers only *Sign in*. With it on, the bare address offers *Join as a guest*, *Create an account*, or both, as the operator chose. Joining is rate-limited per client address and server-wide, can be paused like invites, and every change of the setting is an audit event.
2. **A guest is a real principal, marked guest (D2).**
   - It holds exactly one credential, kept in the browser it joined from, which lasts 7 days from its last use (or the invite's shorter lifetime); a guest that stays away for 7 days expires, and is out the moment its credential is.
   - It sees only what its invite link granted, or the server's *open channels* when it came from the bare address. It gets voice, and may watch a screen share, only if that was granted.
   - It cannot invite, cannot request or decide approvals (it never holds the deciding right, and an operator cannot grant it one), cannot see members outside its channels, and cannot open one-on-ones.
   - It is shown with a "guest" badge. Its messages stay after it expires, attributed to "<name> (guest)"; its name is freed when the server hides the expired guest: at once if it posted nothing, 90 days on if it did.
   - It can become a member ("Keep this account") by setting a password, and keeps its name, channels and history. A guest from the bare address may keep its account only while open joining still offers accounts and is not paused; a guest from an invite may at any time; an operator may keep any guest, and then hands the person a reset link to set their password.
3. **Signing in again is a password, plus Add a device (D3, revised 2026-10-21).**
   - **Every account has a password from the moment it exists.** Creating an account, from an invite or the bare address, takes the name and the password in one request; keeping a guest account sets the password in the same request. Nothing says an account is ready before the server has stored its password. Accounts that exist today are told to set one, with a banner until they do.
   - Passwords are hashed with **argon2id** through `golang.org/x/crypto`, which **Nick approved on 2026-10-21** at the version the design measured, the newest on Go 1.25 (v0.55.0). The rules follow NIST SP 800-63B: 12 characters or more for a member, 15 for an operator, up to 128, any character, no composition rules, a common-password list and the account's own name refused. A failed sign-in has one answer and one timing; limits slow an account down and never lock it.
   - A member changes their password with the current one, and changing or resetting it ends every other sign-in of theirs.
   - **Add a device:** a signed-in member makes a one-use link, or a QR code of it, for their own next device. It is the operator's sign-in link of #493, made by the member for themselves, with a short life, and it never resets a password.
   - **The operator's sign-in link stays as a member's recovery**, and now lets its opener set a new password on the way in.
   - **Passkeys are deferred** to a later phase. Nothing in this phase depends on WebAuthn or a Go toolchain bump; the design keeps what was measured and reviewed for then. Recovery codes are evaluated and also left for later.
   - Rule 1 holds: no mail server, and no other process, is needed to join or sign in.
4. **Operators sign in with name, password and a one-time code (RFC 6238 TOTP, standard library only).** Their browser sign-in stays one tab (D2 of #260), in a session of at most 12 hours, ended after 1 hour idle, idle being measured by requests and never kept alive by an open socket. Every operator action that creates a principal, a credential, a link or a hook, or grants a right or access, or removes a person, needs re-authentication within the last 10 minutes; a wrong code or re-authentication counts as a failed sign-in like a wrong password. An operator's password and code are set only through a setup link made on the server's host (`conchd bootstrap-operator`, `setup-link`, `reset-operator`), never online from a bearer token; recovery is the same host, and the data directory is the root of trust. Bearer tokens stay, unchanged, for scripts and the CLI; they are exempt from the session bounds and re-authentication, issuing one for an operator needs re-authentication, and the operator's sign-in list shows them first with their age.
5. **Names are unique per server, case-insensitively, with look-alikes refused and a reserved list (D4).** The first two already hold (#441); this adds the reserved list (operator, admin, system, the server's own name and their kin), applied wherever a person chooses their own name.
6. **Abuse controls (D5):** per-address rate limits on every anonymous route that creates or signs in something, using the trusted-proxy client address of #609, with sign-in on buckets of its own; a cap on live guests; a pause-joining switch; bulk revocation of everyone who came in from the bare address; and audit events for every guest, account, password, code, device link, session and setting change.
7. **The isolation guarantee extends to guests and to the bare-address path (D6).** `e2e/isolation` adds a guest from an invite link and guests from the bare address, and checks that they find nothing of any group they are not in, that revoking them is as fast as revoking a tester, that passwords do not cross the group boundary, and that the bare address lets nobody in while joining is off.
8. **The unavailable and recovery experience is stated, not implied.** A password works on every browser and origin; over plain http it crosses the network in clear, the page warns when the connection is not secure and not loopback, and the deployment runbook requires TLS beyond localhost or a trusted LAN. A new device is the password or Add a device; a forgotten password is an operator's reset link; an operator who loses both factors goes to the host. **The limitation that remains:** there is no self-service reset, because that needs a channel to the person that Rule 1 forbids requiring.

## Alternatives considered

| Option | For | Against |
| --- | --- | --- |
| **Open joining as an off-by-default operator switch (chosen, D1)** | The alpha keeps its invite-only default; turning it on is one audited operator decision; it can be paused like invites. | One more setting to document and test. |
| Invite only, always | Nothing new to defend. | Does not give Nick what he asked for: someone with just the address cannot get in. |
| Always open | Simplest landing page. | A public server with no way to close the door; contradicts the alpha's private character. |
| **A guest as a real principal with a guest mark (chosen, D2)** | Every existing check (membership, audit, revocation, isolation) applies as it is; history stays attributable; becoming a member is a flag change plus a password. | A second kind of human to carry a badge for, in every client. |
| Guest as an anonymous, unstored session | No new rows. | No audit trail, no revocation by id, no history, and nothing to keep when the guest wants to stay. |
| **Passwords with argon2id, plus Add a device (chosen, D3 revised)** | Works on every browser and origin, including a bare IP or a LAN without TLS; a terminal can do it too, so the CLI has full parity; one small, approved dependency; the person owns the secret, so no operator is needed to come back. | A secret to guess, to phish and to forget; no self-service reset without a mail server, which Rule 1 forbids requiring (the operator's reset link covers it); hashing costs memory and time, which the design bounds. |
| Passkeys (WebAuthn) first (the first design, now deferred) | No secret on the server to leak; phishing-resistant; works on phones. | A Go dependency with twelve modules and a Go 1.26 bump; impossible on a bare IP or a LAN name without TLS; a terminal cannot run the ceremony; losing every passkey and device means an operator anyway. Nick: start with passwords. |
| PBKDF2 (`crypto/pbkdf2`, HMAC-SHA-256, 600,000 iterations) | In the standard library since Go 1.24: no dependency at all. | Not memory-hard: a stolen database is cracked on GPUs at a fraction of argon2id's cost. Not taken: Nick approved `x/crypto`. |
| Email magic link or email reset | Familiar; self-service recovery. | Requires a mail server for the sign-in or recovery path, which Rule 1 forbids; could only ever be optional. |
| Recovery codes now | Self-service recovery within Rule 1. | A second secret most people lose; for operators it bypasses or duplicates the second factor; more setup and surface. Evaluated in the design; later, with passkeys. |
| Operators keep the pasted token (the first design) | Nothing new. | Nick's own confusion generating tokens was part of the problem; a repeatable sign-in with a second factor and bounded sessions is stronger, not weaker. |
| HttpOnly session cookie instead of the bearer in localStorage | The script cannot read it. | Needs CSRF protection on every write route, which Conch does not have (O1 of private-alpha.md weighed this already). |

## Unchanged

- No end-to-end encryption, no federation, no multi-tenancy, no recording.
- No standalone human DMs before P7; a one-on-one is still a two-member private channel, and a guest cannot make one.
- An invite grants only what its maker holds, and never operator rights. Agents are still made only by an operator; neither an invite nor the bare address ever makes one.
- The operator's browser sign-in lasts one tab (D2, #260); a member's lasts until sign-out (O1, #439).
- The bearer-token path, for scripts, the CLI and bootstrap.
- **Rule 1:** `conchd` needs no other process to let someone join, sign in, add a device, reset a password or recover an operator.
- **Rule 4 (API parity):** every new action is a REST route first; the browser, the CLI and the TUI use that. With passwords, nothing in this phase is browser-only.
- `pkg/schema` is the source of every new wire shape, through the schema-change skill.

## Consequences

- **New security surface.** Anonymous routes that create or sign in something: join as a guest, create an account, password sign-in and its code step, a reset through a sign-in link; and self-service device links, password change and re-authentication. Every implementation PR gets a `security-reviewer` pass; the design's threat notes, revised for passwords, get one before the issues are filed.
- **One new Go dependency, approved:** `golang.org/x/crypto` v0.55.0 (BSD-3-Clause; it adds no other module, since `x/sys` is already in `go.mod` at the same version). Go stays at 1.25.0. The `deps-allowlist.txt` line lands with the server password issue and names the approval.
- **Schema additions, all compatible in shape:** a guest mark and an origin on principals and the shapes that carry them, an open-joining flag on channels, open-joining settings, join routes, sign-in and re-authentication shapes, TOTP shapes, password rules and a policy in the front-door answer, self-service sign-in links with a purpose, a credential kind, and new audit actions. One published route's rule tightens: a member redemption of an invite requires a password; the design flags it for the schema-change decision.
- **Storage:** new columns on `principals` (password hash as a PHC string, TOTP secret and counter, guest mark, origin, expiry), `credentials` (kind, idle and re-authentication times) and `channels`; a purpose on sign-in links; new `server_settings` keys; an embedded common-password list; a guest-expiry sweeper, a hashing semaphore and a login-ticket table inside `conchd`; two offline `conchd` commands.
- **Clients:** the browser gets a front door with a name-and-password form and an http warning, a guest badge, "Keep this account" with a password, account creation with a password, the operator's code step, session end and re-authentication dialog, change password, the setup and reset pages, a banner for accounts without a password, and Add a device. The CLI gets `conch login --password`, `conch passwd`, `conch totp`, the operator's side of all of it and `conchd reset-operator`; the TUI shows the badge and the banner line.
- **The isolation check grows a third group** (guests), rows for passwords across the boundary, and new matrix rows; its coverage check will fail until every new route has a row or a stated exemption.
- **Documents that said "no public signup"** (ROADMAP.md's alpha paragraph, private-alpha.md §3 and O2) carry a one-line pointer to this ADR in the same PR.
