# Temporary guests, accounts and signing in again

Issue [#694](https://github.com/njdaniel/conch/issues/694), PR
[#701](https://github.com/njdaniel/conch/pull/701), decision
[ADR-009](../adr/ADR-009-guests-and-open-joining.md).

**Status: Proposed, revised 2026-10-21.** This replaces the passkey-first draft.
The owner selected passwords first and approved Argon2id/x/crypto in the PR discussion.
The owner then clarified temporary guests, a reserved guest prefix, and real accounts for
lasting permissions and previous private conversations. Implementation follows ADR approval.
The timings and technical defaults below are design recommendations, not previously shipped
behavior. No code, dependency or database migration is introduced by this document.

## 1. Product contract

- A visitor opens an allowed entry point, chooses **Join as guest**, and can participate
  without a password or an operator issuing a token.
- The server assigns a handle such as `guest-a7k4m2q9`. The visitor may also type a
  display label, shown as **Guest: Nick · a7k4**, never as a bare name (§4.1). A guest
  visit grants temporary, scoped access; it does not create a recoverable personal account.
- **Create an account** asks for a permanent username and password. Real accounts can
  return to their authorized conversations and can be granted additional permissions.
  Account creation alone does not grant access to other conversations or elevated roles.
- Ending a guest visit ends its credentials and memberships. A later visit is a new
  identity. Guest messages remain attributable to the original guest; they are not
  silently deleted, reassigned to a different visitor, or recoverable as that guest.
- Conversion is available only during a live visit. It preserves that principal and its
  permitted current access atomically, with no extra rights or older history granted.
- Guests can refresh/reconnect during a live visit. A network interruption is not logout.

## 2. Entry policy and settings

| Setting | Default | Meaning |
| --- | --- | --- |
| `open_joining` | `off` | Bare-address mode: `off`, `guest`, `account`, `both`. |
| `guests_enabled` | `false` | Master gate on every guest-creation path, including invite redemption. |
| `joining_paused` | `false` | Pauses bare-address signup and conversion of address-origin guests. |
| `invites_paused` | existing | Pauses invite redemption and conversion of invite-origin guests. |
| `join_voice` | `false` | Voice/share eligibility of bare-address joiners, still subject to channel access. |
| `max_guests` | `50` | Concurrent live guest visits across address and invite entry points. |
| `guest_idle_seconds` | `7200` | Two hours without authenticated activity; configurable up to 24 hours. |
| `guest_absolute_seconds` | `86400` | Hard visit bound; configurable up to 24 hours, never renewed past it. |
| `server_name` | configured host | Front-door label and a reserved permanent username. |

Invites gain `join_as: account|guest|both`; existing and newly created invites default to
`account`. Only an inviter already permitted to create invites can select these modes.
An invite never grants more channels or voice rights than its maker holds.

Effective guest joining requires BOTH the master switch and the entry point's guest mode.
Effective account creation requires that entry point's account mode. Reject contradictory
configuration (for example address guest-only while guests are disabled), and recheck policy
in each write transaction. Disabling guests while address mode is `both` leaves account
signup available; if mode is `guest`, atomically turn it `off` and audit both changes.
The server can retain guest-capable invite records while guests are disabled, but their
preview and redemption must reflect that guest joining is unavailable.

Turning guest creation off does not silently evict existing callers. The operator has
**End all guest visits**, with dry run and confirmation, to revoke existing guests.
The response to turning guests off, and its audit entry, carry the number of live guest
visits and name End all guest visits, since visits otherwise last up to 24 hours (F12).
Normal password login for existing members is independent of joining/invite switches.

A channel may be designated for bare-address admission. At least one must exist to enable
open joining. Disallow removing the last such channel while open joining is enabled, or
atomically disable open joining. Recheck the channel set during signup so a racing update
cannot create an account with no usable destination. Adding an open channel does not
retroactively add existing users. The UI states that designation alone does not publish
existing private history to guests.

Server-side checks apply to REST, CLI/TUI and the browser. Hiding a button is not enforcement.

## 3. User flows

### 3.1 Invite or address

1. Fetch front-door/invite information. Show only effective allowed actions and **Sign in**.
   An invite preview keeps the existing expiry, scope and rate-limit checks.
2. **Guest:** no permanent username or password; an optional display label (§4.1). Create an
   unpredictable server-generated guest handle, one credential and the allowed memberships
   in one transaction. Show:
   "Temporary guest. End visit signs you out permanently. Your messages remain.
   Create an account to keep access." Show the visit's absolute expiry.
3. **Account:** collect permanent username and password together. Validate/hash before the
   committing transaction, then recheck entry policy, invite use, name availability and
   grants. Insert principal, password and session atomically; consume the invite only on
   success. A failed setup creates no stranded account and does not consume the invite.
4. Show the destination before voice joins. Joining chat never activates a microphone.

An existing member signing in from an invite does not automatically redeem or acquire that
invite's channels. Existing-account invite acceptance is separate scope; give an explicit
message if the member lacks access rather than creating a duplicate account.

### 3.2 Returning member and new device

Username/password login works independently of open joining. On success, issue the existing
member credential type with its documented lifetime and show authorized conversations.
No raw bearer field is the normal human sign-in flow. Keep token login in an advanced
operator/automation path with its existing warnings.

**Add a device** remains optional for a signed-in member; password login already provides
an independent way back in. A forgotten password leads to the recovery flow in §6.

### 3.3 Create an account during a guest visit

The former **Keep this account** action is labelled **Create an account** and takes a new
permanent username plus password. A guest cannot keep the reserved guest-prefixed username.

In one transaction after password hashing:
- Re-read that the guest and credential are still live, enabled and unconverted.
- Recheck the signup policy for the recorded origin. Address-origin guests require
  `open_joining=account|both` and not paused. Invite-origin guests require their recorded
  invitation to allow accounts and not be revoked, expired or administratively paused.
  Consumed use count alone does not invalidate the guest's existing admission.
- Check the permanent name and its case/confusable keys; store the password.
- Keep the principal ID, origin, current authorized memberships and history boundaries.
  Do not restore removed channels or grant invite/approval/admin rights.
- Record the old guest attribution, change the current name and guest state, revoke the
  guest credential, issue a fresh member credential, and invalidate guest session state.
  The original hard visit deadline no longer applies to the member.
- Commit everything together; audit conversion without passwords or credential material.

Cancel, policy refusal, collision, hash failure or a concurrent logout/expiry leaves no
partial member. The UI does not remove the guest status before success. A lost response
after successful conversion is recoverable by password login, never by reusing the old
guest token. A duplicate request cannot create a second identity.

The master guest-creation switch does not by itself block conversion of an already live
guest; the account policy above decides. Operators may authorize a specific live guest
to register despite a closed entry policy through a short-lived, one-use conversion permit
bound to that principal. They cannot clear the guest flag without password setup.
No conversion permit resurrects an ended visit or grants channels/roles.

### 3.4 End visit and reconnect

**End visit** or sign out revokes the credential, marks the visit terminal and removes
active memberships in one transaction. All guest tabs/connections with that credential
and media access are revoked. Joining voice again cannot restore it.

Refresh or a short interruption can resume only with the same still-live credential.
Use tab-scoped session storage for guests, never member localStorage, and clear it on
explicit logout, expiry, authentication refusal or conversion. Do not put it in a URL.
Browser/session restoration can preserve tab storage; never claim that closing a browser
is a reliable logout. The UI provides End visit and server expiry enforces the bounds.

Closing a tab may send best-effort end notification only when it can distinguish an explicit
end from refresh/navigation; correctness must not depend on unload delivery. Leaving voice
or browsing another channel does not end the guest visit.

When the response to logout is lost, retry revocation while possible and report whether
server termination was confirmed. Clearing local storage alone is not proof of revocation.
The server expiry remains the maximum exposure bound.

## 4. Identity, history and lifecycle

### 4.1 Reserved guest names

Generate `guest-` plus at least 80 random bits encoded as a readable suffix; the short
example in §1 is illustrative. Persist a unique index and retry collisions. Never accept
a client-supplied canonical guest identity or reuse a retired guest name.

Reject permanent usernames whose normalized case-folded key OR confusable skeleton starts
with `guest`. Apply the rule on signup, conversion, member/operator rename and operator
creation/import; no operator bypass for this namespace. Existing reserved administrative
names and `hidden-` rules remain. Guests cannot rename themselves into the member namespace.
A guest label is not an authentication identifier that can be reclaimed.

**Display labels (#694's owner clarification, reconciled in #703).** At entry the visitor
may type an optional display label (for example `Nick`); without one the handle is shown.
The label is 1–32 code points after NFC, held to the #441 key and visible-character rules
(no control, format or bidi characters), refused when it contains `·` or a run of
whitespace (which could fake a second tag), when its folded key starts with `guest` (which
could fake another visit's handle), or when its folded key is a reserved administrative
name or `server_name`, and fixed for the visit. On the wire it is its own schema field
beside the handle, never `name`, and part of the attribution record of §4.4. It is **not unique**: a
later visitor may use the same label. It is never an identifier: sign-in, mentions,
membership changes, audit and every API that names a principal use the principal ID or the
handle, never the label. Every surface renders a guest as `Guest: <label> · <first four
characters of the handle suffix>` (or the handle alone), from the stored guest flag, never
from text, so two visits labelled `Nick` stay distinguishable and no permanent account can
look like a guest. Historical posts keep the label and tag they were posted under (§4.4).
On conversion the label is dropped; the member chooses a permanent username (§3.3).

Preflight existing principals before enabling this migration. Existing real names in the
reserved namespace need an explicit, audited rename plan; do not silently rename, hide or
reclassify accounts. Until conflicts are resolved, refuse enabling the new guest feature.
Never identify guest status by text prefix alone: use the stored type/flag.

### 4.2 Data and lifetime

Keep `kind=human`, `role=member` and add a guest state, durable origin, original guest name,
visit start, last authenticated activity, absolute deadline and terminal timestamp/reason.
A guest has exactly one credential; credential creation, device links, password login,
reset and operator sign-in links must refuse a guest target.

Effective expiry is the earliest of idle expiry, hard deadline and any shorter invitation
credential lifetime. Do not reuse the member once-a-day renewal logic: it cannot enforce
these shorter bounds. Read expiry from the server clock on every authenticated request,
socket delivery/subscription check and media entitlement check. Idle time is refreshed
only by a REST request carrying the guest's bearer, recorded in `last_used_at` at most once
a minute; never by an open socket, WebSocket ping, ticket redemption or the voice poller,
and never past the hard deadline. `CredentialLive` (today `store/resolve.go`, which checks
only expiry, revocation and disable) applies `max(last_used_at, created_at) + idle` to guest
and operator-session credentials, so every socket recheck (`ws.go`), ticket redemption
(`tickets.go`) and media entitlement check (`voice.go`) inherits the idle rule without
code of its own. A page that is open and in use makes requests; a stolen token kept alive
by a ping loop does not stay live. *(Security review F5.)*

The lifecycle is `active → ended` or `active → converted`, never back to active.
Serialize conversion, logout, renewal, disable and expiry so exactly one terminal outcome
wins. Operator enable or replay of a join response cannot revive a terminal guest.
A backup can predate a logout or revocation, so restore must invalidate all restored session
credentials, device/reset/enrollment links and pending authentication, and end every restored
guest visit before accepting traffic. Members sign in again normally; operators use the
normal or offline recovery flow. Merely sweeping time-expired rows is insufficient.

Use the existing credential/principal revocation hooks for sockets and LiveKit. A deadline
sweeper wakes at the next relevant deadline and on logout/revoke/policy changes; a polling
fallback is at most one second. Target the existing revocation bounds from explicit terminal commit (socket 2 seconds;
media one poller pass plus room rotation). At timed expiry allow at most the one-second
sweeper fallback plus the two-second socket bound; media entitlement must reject expiry
within its normal poller/rotation bound without waiting for the sweeper.
Authorization refuses expired guests even before cleanup completes. Ended guests do not
consume the live-guest cap. Reconnect with an expired token fails closed.

### 4.3 Temporary access and history

A guest may read/post in assigned conversation channels, join voice/watch/share when
granted, see authorized current participants, and report a problem. No uploads, invitations,
account/device credential creation, durable roles, channel creation, private conversation
creation, approval requests/decisions or agent intervention commands.

An operator may admit a guest to another current conversation, but only as temporary
membership with a fresh history boundary. They cannot attach privileged or durable roles
while the principal remains a guest. Member-only rights require successful account creation
and a separate authorized grant.

**Approval rights are enforced in the store, not by intent** *(security review F1, must-fix;
approval-path)*. Today `store.PutChannelMember` turns `can_decide` on when the field is
omitted (`store/members.go`, `decide := canDecide == nil || *canDecide`; `AddChannelMember`
passes nil), and quorum counts every `can_decide = 1` member (`store/approvals.go`). So:
the store forces `can_decide = 0` on every membership write whose principal is in guest
state (`PutChannelMember`, `setCanDecideTx`, the admission transaction and the operator
add), and refuses an explicit `can_decide: true` on a guest with `guest_not_allowed`.
`DecideAsMember` re-reads the guest flag inside the deciding transaction and refuses with
`decision_not_permitted`. Conversion does not touch `can_decide`: a converted member gains
it only through a later, audited operator grant, which is approval-path work.

**Files** *(F11)*. `humansOnlyFiles` admits any `kind = human`, so guests need an explicit
refusal on `POST /v2/channels/{channel}/files`; `GET /v2/files/{id}` and `/meta` apply the
history bound through `FileVisibleTo` (`store/audience.go`). MCP is already closed to
guests (agents only, `mcp_auth.go`); G2 records that rather than re-gating it.

Each guest membership records a server-chosen history lower bound at admission, preferably
an immutable message sequence/watermark rather than a client timestamp. Every read surface
must intersect normal ACLs, whisper/net audience rules and this bound:
- history, direct message IDs, search, threads/replies and quoted-message previews;
- files/attachments and their metadata/download routes;
- WebSocket replay, backfill, unread counts, activity and notification payloads;
- indirect references, message edits, embeds and agent/run/approval objects that could reveal
  older conversation content.

A direct ID or attachment link must not bypass the boundary. Replies to older messages may
show only permitted current content; no old quote, author detail or thread metadata leaks.
Default-deny an object without a provable allowed provenance. Old standalone files or
non-message objects cannot become visible merely because their channel admits guests.
A new upload explicitly shared in the current session may be read once its provenance and
audience checks pass. Guest media access grants no access to recordings or stored transcripts.

Conversion keeps these lower bounds and existing access. It does not automatically expose
pre-visit private history. A separately authorized member permission change may later widen
history, with an explicit audited action. That action must have a defined API and UI before
implementation; widening is not required to ship this onboarding slice.

Ending a visit removes memberships and invalidates cursors/subscriptions. A later guest
visit gets a different ID and fresh bounds, even if it uses the same browser or invitation.
It cannot reopen whispers/private conversations addressed to the old principal. Members
retain only conversations their current ACLs permit; account ownership never bypasses ACLs.

### 4.4 Attribution and cleanup

Posted messages and required audit entries remain after a guest ends. Store immutable
guest author attribution, or an equivalent retained tombstone keyed by the original
principal ID, so history renders `guest-<suffix>` without relying on live membership.
Retain it through conversion too: earlier guest posts remain identifiable as guest posts;
new member posts use the permanent identity.

Return attribution only with messages the viewer is authorized to read; do not expose
expired principal directories across groups. Ending a guest must not rename historical
messages to `hidden-<id>` or attach them to a later user.

**Guests are not hideable** *(security review F10)*. `HidePrincipal` is a rename
(`store/hide.go`) and clients render the new name on old messages, which would break the
immutable attribution above. Hide refuses a principal in guest state with
`guest_not_hideable`, and so does any rename of one (`PUT /v1/principals/{id}/name`, by an
operator or by the guest itself; the store refuses it beside the hide check, in G2): a
guest's name is its handle. End visit, the entry invite's revocation and End all guest
visits are the levers. Hiding a converted member renames the member identity as today, while posts
made during the guest visit keep their stored guest attribution.

Remove live credentials, memberships, presence and transient state at termination.
Retain minimal attribution/audit records under the server's existing retention policy.
Never promise all guest data is erased. A guest that posted nothing can have transient
profile data cleaned promptly once required audit references are preserved. There is no
90-day reservation of a permanent username and no recovery of ended guest access.

## 5. Passwords and operator authentication

### 5.1 Dependency and hashing

Use `golang.org/x/crypto/argon2`, pin `golang.org/x/crypto v0.55.0`.
The owner's package/algorithm approval is recorded in
[the PR discussion](https://github.com/njdaniel/conch/pull/701#issuecomment-6322049789).

Inspected upstream [v0.55.0 go.mod](https://github.com/golang/crypto/blob/v0.55.0/go.mod)
declares Go 1.25.0; v0.56.0 and v0.57.0 declare Go 1.26.0. This is a compatibility
inspection, not a completed local dependency build or vulnerability audit. G4 must record
resolved go.mod/go.sum changes, licenses, dependency gate entry and vulnerability results;
stop and revisit the pin if unsafe or incompatible. No WebAuthn or PBKDF2 fallback.

Proposed Argon2id baseline: memory 64 MiB, iterations 3, parallelism 1, random 16-byte salt,
32-byte result. Store an encoded algorithm/version/parameters/salt/hash record, never a
password. Bound stored parameters before verification; compare derived hashes in constant
time. Benchmark the target host and permit an explicit reviewed tuning change, never
silently below OWASP's documented minimum (19 MiB, 2 iterations, parallelism 1).

Hash-producing and hash-verifying routes use **two bounded work pools** *(security review
F3)*, so an anonymous login flood cannot stop an operator from re-authenticating to pause
joining or end guest visits. The anonymous pool serves login, signup, invite redemption and
reset completion; the authenticated pool serves conversion, password change, re-authentication
and device links. Each initially allows at most two running hashes and eight queued requests,
bounded wait, then 429/503 with Retry-After.
Acquire capacity before allocating hash memory; do not hold a SQLite write transaction
while hashing. Rate-limit before expensive work. Unknown users perform equivalent dummy
hash work within the same limits. Test queue cancellation and memory bounds under load.

### 5.2 Password and login rules

Proposed password policy: 15–128 Unicode code points, valid UTF-8, no silent truncation,
allow spaces and paste/password managers, no composition or periodic-change rules.
Normalize consistently to NFC before length checking and hashing and document that behavior.
Use a bundled, bounded common-password blocklist plus server-name/username checks; no
required online breach lookup. Bound request bodies independently (including a 512-byte
UTF-8 password bound after normalization).

This follows the length direction in
[NIST SP 800-63B](https://pages.nist.gov/800-63-4/sp800-63b.html), without claiming NIST
certification. Hashing guidance:
[OWASP Password Storage](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html).

Login failures for unknown, wrong-password, disabled, hidden, guest or legacy-unset-password
accounts return the same status/body and comparable work. Rate-limit by trusted client address
and by a **per-name window** *(security review F4)*: keyed by the folded key of the typed
name whether or not a principal exists, bounded like `bucketSet` (`invite_limits.go`), it
holds a not-before time. Inside the window the server answers 429 with Retry-After at once,
before any hashing; it never sleeps or delays a response, so the throttle itself cannot
reveal which names exist. Do not permanently lock an account through anonymous failures.
Successes are capped too *(F9)*: 30 per name key per hour, then 429 with Retry-After, so a
holder of a valid password cannot mint lasting credentials without bound.
Log safe reason codes only; no password, request body, token or full client address.

Verify outside the write transaction, then re-read enabled state and password/authentication
generation before issuing a credential. A reset/disable/password change racing login wins.
Successful changes/resets invalidate other sessions, all outstanding device/sign-in/reset
links, recent-auth grants and pending login attempts. Rotate the surviving authenticated
session after a normal password change; reset issues no general session automatically.
Reuse current socket/media revocation hooks.

Password changes require the current password; operators additionally require TOTP.
An operator's first password and TOTP enrolment are reachable **only** through the
host-made setup token (§5.3): `POST /v1/password/mine` and TOTP enrolment on an operator
bearer credential answer 403 `setup_on_host` *(security review F7)*.
First-password enrollment for legacy members uses their existing session only with a
fresh one-use enrollment proof from an operator, not possession of a long-lived bearer
alone. Show setup incomplete until done. Bootstrap-created accounts use the same rule.

### 5.3 Operators

Normal operator login requires password and a TOTP proof. Password verification yields
only a short-lived, one-use pending-auth challenge, never an operator bearer. TOTP completion
is bound to that challenge, its principal and the client address it was issued to
*(F14)*, with limited attempts; challenge lifetime five minutes.
Recheck account state/auth generation at completion. Uniform failure responses apply.

**One failure count for every factor** *(security review F2, must-fix)*. A wrong TOTP code,
a wrong re-authentication pair and a wrong password are the same account failure: each
counts in the typed name's window (§5.2) with the same not-before time. The window resets
only when a credential is issued at TOTP completion, never at the password step, so knowing
the password does not buy fresh guesses at the code. Challenge issuance is bounded per name
(it shares the window) and per address.
Operator sessions are tab-scoped with 12-hour absolute and one-hour idle expiry, checked
server-side even if browser storage remains.

TOTP follows RFC 6238 with cryptographic randomness and published test vectors; proposed
parameters are a 160-bit secret, HMAC-SHA-1, six digits, 30-second steps and a ±1-step window.
Consume accepted time steps atomically to refuse replay across concurrent login and reauth.
Enroll only after proving a code; protect pending and active secrets with authenticated
encryption under an operator-managed host key outside the database. Back up that key
separately through the operator recovery procedure; do not log/export the secret or expose
it after enrollment. Show setup material only in the enrollment view, not during a share.
A QR encoder is optional follow-up; manual authenticator setup needs no new QR dependency.

Sensitive operator actions require password+TOTP reauthentication within ten minutes,
bound to the current session and invalidated by credential/auth changes. **The class is
defined by a rule, not a list** *(security review F8)*: every operator route that creates a
principal, credential, link, permit or hook; grants a right or access (including
`PUT /v1/channels/{channel}/members/{id}`, which grants `can_decide` and is approval-path,
invite permission, open joining and manifests); or removes or ends a person (disable, hide,
bulk revoke, End all guest visits). Password and TOTP changes are in it. The route-table
test enumerates the class and fails on any operator route not classified either way.

Offline host commands bootstrap the first operator or reset operator authentication, with
a short-lived one-use setup token (ten minutes, stored hashed) bound to a restricted setup
flow. No general API access before password+TOTP enrollment succeeds. Redeeming the token
spends it at once and answers a 15-minute credential with `scope = setup`, usable only for
setting the password, TOTP enrol/confirm and `GET /v1/whoami`. The scope is checked once,
in `authMiddleware` beside expiry, never per handler, so no route can forget it
*(security review F6)*. Offline recovery
revokes existing sessions/links and is audited; never offer a public operator reset endpoint.

Existing operator bearer credentials remain explicit automation/CLI credentials under the
existing trust model. They are not eligible to manufacture an MFA/recent-auth browser session.
The browser rejects script-token login as a substitute for normal operator authentication.
Document that a stolen full-power script credential is still powerful; this design does
not retrofit MFA into unattended scripts. CLI interactive operations use the normal login
and reauth flow; automation actions remain separately identified and auditable.

### 5.4 Transport

Password, reset, enrollment and TOTP flows require HTTPS beyond loopback. Client checks are
supplementary; the server enforces the configured public origin and trusted proxy boundary,
not arbitrary forwarded headers. Loopback HTTP is a development exception.
A bare LAN HTTP page must explain how to enable TLS, not invite password submission with
only a warning. No passwords/codes in argv, query strings, logs or analytics. CLI reads
secrets from non-echoing input. No permissive credentialed CORS.

## 6. Recovery and Add a device

### 6.1 Member password recovery

A verified operator issues a target-bound password-reset link after checking the person's
identity out of band. Default ten-minute expiry, maximum one hour, one use, cryptographically
random code stored only as a hash, delivered in the URL fragment. Preview does not consume it.
The link authorizes only password replacement, not ordinary sign-in or other APIs.
Recheck issuer and target authorization, expiry and auth generation at completion.

On successful reset, atomically store the new password, consume the link, revoke sessions,
device links and other recovery links, and invalidate pending authentication. Require normal
login afterward. Disabled/hidden/guest/operator targets are refused. Audit issuance and use.
Existing operator sign-in links remain explicitly separate support tools; opening one does
not authorize a password change without current password or a valid enrollment/reset proof.

No self-service forgotten-password recovery ships in this slice. The UI states that limitation.
Recovery codes and optional email are possible later; neither is inherently incompatible with
the dependency-free core when no external service is mandatory.

### 6.2 Add a device

Members only, not guests, operators or agents. Require current-password verification (or a
recent password-auth proof bound to the session, at most ten minutes) before creation.
Reuse sign-in-link machinery with default ten-minute/max-one-hour expiry and at most three
open links per principal. One use, hashed at rest, code in fragment, revealed once.

The receiving page states whose account it signs into and requires explicit confirmation.
If another account is already signed in, require explicit account-switch confirmation before
replacing it. Links never redeem merely because a page was opened. The creating view hides
the secret behind Reveal and refuses reveal while its own tab is sharing; this cannot detect
screen capture by another application, so the warning remains.

Redemption rechecks target and creator, expiry, unused state and auth generation atomically.
Password change/reset, disable, sign-out-everywhere and revocation invalidate open links.
A stolen old device link cannot defeat a password reset. Show labeled resulting sessions
and allow revoking them. CLI uses non-echoing stdin for link redemption. Copy/link first;
QR rendering is a separately reviewed dependency or implementation decision.

## 7. Storage, schemas and API work

All wire types come from `pkg/schema`, generated TypeScript and shared fixtures.
These are proposed contracts; schema review must decide compatibility rather than calling
every behavior change additive. In particular, signup password requirements change invite
redemption; use a versioned route/shape or explicit negotiated compatibility, never let
an old passwordless member-redeem route bypass account setup.

| Area | Required contract |
| --- | --- |
| Join information | Effective guest/account modes, pause/cap state, server label, password login availability; no hidden channel/person discovery. |
| Invites | Effective join modes, credential lifetime and recorded origin; password required for member redemption. |
| Guests | Guest state, handle, display label and tag as separate fields (never `name`), original attribution, hard expiry, remaining allowed actions; no exposed password/auth secrets. |
| Conversion | New permanent name/password; atomic success or unchanged guest; explicit enrollment permit when used. |
| Passwords | Login, change, first enrollment and restricted reset; generic authentication failures. |
| Operator auth | Pending login, TOTP enrollment/finish, recent-auth proof, session expiries and offline recovery. |
| Device links | Self-create/list/revoke, restricted preview and atomic redeem. |
| Guest policy | Global gate, bounds, per-entry modes, live guest list, dry-run/confirmed termination. |
| History | Server-owned membership watermark and filtered message attribution; bounded object provenance. |

**G1 (#704) defined these contracts in `pkg/schema` (`guest.go`, `auth.go`, and additive
fields in `invite.go`, `sign_in_link.go`, `whoami.go`, `credential.go`, `channel_member.go`,
`message_v2.go`, `principal_admin.go`), with golden fixtures in `pkg/schema/testdata/` and
the generated `web/src/schema/generated.ts`.** §7.1–§7.6 below are the contract that
#705–#716 build against. Every change is additive: no published fixture changed and no
schema version was bumped; §7.6 lists the behaviour changes that still need the owner's
sign-off. Bind all ceremony/link types to their purpose, target, expiry and authentication
generation. A reset token is not a device link, an enrollment proof is not an operator
session, and no principal ID in a request replaces the authenticated caller.

### 7.1 Wire shapes

| Area | Types (`pkg/schema`) | Notes |
| --- | --- | --- |
| Front door | `JoinInfoV1`, `JoinOptionsV1` | Effective guest/account availability, cap and pause state, server label, whether password sign-in exists, visit bounds. No channel or person. |
| Settings | `JoinSettingsV1`, `PutJoinSettingsRequestV1` (partial, `Apply`), `JoinSettingsResponseV1`, `OpenJoiningMode`, `JoinWarning` | `max_guests_per_address` (F13), live count and warnings (F12, F13), `reserved_names_in_use` (preflight), `server_name_in_use` (a permanent principal holds `server_name`; #760). `Apply` turns a guest-only address off when guests are disabled (§2). |
| Invites | `InviteJoinMode` (`account` default, `guest`, `both`) on `CreateInviteRequestV1`/`InviteV1`; `InvitePreviewV1.join` (effective); `RedeemInviteRequestV2` (code, name, password); `RedeemInviteGuestRequestV1` (code, label) | Absent `join_as` is `account`, so every existing invite stays account-only. |
| Guests | `GuestState`, `GuestOrigin`, `GuestEndReason`; `GuestV1` (self view), `GuestAdminV1` (operator view), `GuestAttributionV1` (handle, label, tag); `ValidateGuestHandle`, `GuestTag`, `GuestDisplayName`, `ValidateGuestLabel`, `IsReservedGuestName` | Handle `guest-` + 16 characters of Crockford base32 (80 bits). Tag = first four of the suffix. Label rules of §4.1 in `ValidateGuestLabel`; the confusable checks stay in the server's `names` package. |
| Joining | `JoinGuestRequestV1`/`JoinGuestResponseV1`, `JoinAccountRequestV1`/`JoinAccountResponseV1` | Responses carry the admitted channels; the guest response carries the visit and its one credential. |
| Conversion | `ConvertGuestRequestV1` (name, password, optional permit), `ConvertGuestResponseV1`, `CreateConversionPermitRequestV1` (permit = sign-in link of purpose `conversion`) | Same principal id, fresh member credential, guest credential revoked. |
| Ending | `EndVisitResponseV1` (self), `EndGuestResponseV1`, `EndGuestsRequestV1` (dry run required; by origin or invite), `EndGuestsResponseV1`, `ListGuestsResponseV1` | Converted members are never selected. |
| Identity | `WhoAmIResponseV1.identity_class` (`IdentityClass`), `.guest`, `.auth` (`AuthStateV1`); `PrincipalAdminV1.guest_state` | A server that predates guests omits all three; a client falls back to kind and role. |
| History | `ChannelMemberV1.history_after` (message id watermark), `.guest`; `MessageV2.guest_author` | Immutable attribution on the message; the bound on the membership. |
| Credentials | `CredentialV1.idle_expires_at`, `.scope` (`CredentialScope`: `full` default, `setup`); `OwnCredentialV1.idle_expires_at` | Idle bound only on guest and operator browser credentials (F5). |
| Passwords | `ValidatePassword` (15–128 code points, ≤512 bytes), `LoginRequestV1`, `LoginResponseV1` (union by `next`: `signed_in` or `totp`), `TOTPLoginRequestV1`, `SetPasswordRequestV1` (optional enrolment proof), `ChangePasswordRequestV1` (current, new, optional TOTP), `ResetPasswordRequestV1`, `PasswordUpdatedResponseV1` | One failure code, `auth_failed`. |
| Operator auth | `ReauthRequestV1`/`ReauthResponseV1`, `TOTPEnrollmentV1`, `ConfirmTOTPRequestV1`, `RedeemSetupTokenRequestV1`/`RedeemSetupTokenResponseV1`; constants for the challenge (5 min, 5 tries), session (12 h / 1 h idle), re-auth (10 min), setup token (10 min) and setup credential (15 min), RFC 6238 parameters | Challenge prefix `conch_chal_`, setup token prefix `conch_setup_`: every secret has its own prefix and every check refuses the others'. |
| Links | `SignInLinkPurpose` (`sign_in` default, `device`, `reset`, `enrollment`, `conversion`) on `SignInLinkV1`, `CreateSignInLinkRequestV1`, `SignInLinkPreviewV1`; `CreateDeviceLinkRequestV1`, `ListOwnSignInLinksResponseV1` | Lifetimes per purpose: sign-in 24 h / 7 d as today; every other purpose 10 min / 1 h. Cross-purpose use is `sign_in_link_invalid`. |
| Errors | `guests_disabled`, `joining_closed`, `joining_paused`, `guests_full`, `guest_not_allowed`, `guest_not_hideable`, `guest_ended`, `not_a_guest`, `conversion_not_allowed`, `conversion_permit_invalid`, `join_policy_conflict`; `auth_failed`, `reauth_required`, `enrollment_proof_required`, `setup_on_host`, `setup_incomplete`, `password_required`, `password_unacceptable`, `password_not_set`, `password_already_set`, `totp_already_enrolled`, `totp_not_enrolled`, `setup_token_invalid`, `too_many_device_links`, `https_required`, `accounts_require_auth` | HTTP statuses in the constants' comments. `decision_not_permitted` (§4.3) is defined by G2's approval-path PR in `approval_api.go`, which G1 does not touch. |

Compatibility rules the types state in their comments: a field a server predates is
omitted, never sent as a zero value that means something; `history_after` 0 is no bound;
`join_as`, `purpose` and `scope` absent mean `account`, `sign_in` and `full`. The
`password` on a presented credential (login, re-auth, current password) is bound-checked
only; a short one fails as a wrong password, so the schema answers nothing the server
would not.

### 7.2 Route table

Access classes are the route table's (`routes.go`): `exempt` (anyone), `authenticated`
(any live credential; the identity class is checked in the handler), `operator`,
`operator-or-self`. **R** marks the re-authentication class of §5.3 (F8): the route needs
`auth.recent_auth_until` on the calling credential, or answers 403 `reauth_required`.
The route-table test (G2/G4) classifies every operator route **R** or not; the rule is:
creates a principal, credential, link, permit or hook; grants a right or access; or
removes or ends a person. Operator routes outside the class by that rule, and so pinned
"not R": channel, net, manifest reads, lists, reports, audit export, invite close and
settings reads. Every anonymous route that takes a password, code or token requires
HTTPS beyond loopback (403 `https_required`) and is rate-limited like invite redemption.

| Route | Access | Body → answer | Refusals beyond the common ones |
| --- | --- | --- | --- |
| `GET /v1/join` | exempt | → `JoinInfoV1` | — |
| `POST /v1/join/guest` | exempt | `JoinGuestRequestV1` → 201 `JoinGuestResponseV1` | `guests_disabled`, `joining_paused`, `guests_full` (429) |
| `POST /v1/join/account` | exempt | `JoinAccountRequestV1` → 201 `JoinAccountResponseV1` | `joining_closed`, `joining_paused`, `principal_exists` (taken or reserved), `password_unacceptable` |
| `POST /v1/invites/preview` (existing) | exempt | → `InvitePreviewV1` with `join` | — |
| `POST /v1/invites/redeem` (existing, v1) | exempt | `RedeemInviteRequestV1` | once migration 32 has run: 409 `password_required`, nothing made (§7.6 B1) |
| `POST /v2/invites/redeem` | exempt | `RedeemInviteRequestV2` → 201 `RedeemInviteResponseV1` | `invite_invalid`, `invites_paused`, `joining_closed` (guest-only invite), `principal_exists`, `password_unacceptable` |
| `POST /v1/invites/redeem-guest` | exempt | `RedeemInviteGuestRequestV1` → 201 `JoinGuestResponseV1` | `invite_invalid`, `invites_paused`, `guests_disabled`, `guests_full` |
| `POST /v1/login` | exempt | `LoginRequestV1` → 200 `LoginResponseV1` | 401 `auth_failed`, also for an operator whose setup is incomplete (no password, or a password but no confirmed TOTP): never `signed_in` and never a challenge; 429 `rate_limited` with Retry-After from the per-name window, before any hashing (F4) |
| `POST /v1/login/totp` | exempt | `TOTPLoginRequestV1` → 200 `LoginResponseV1` (`signed_in`) | 401 `auth_failed` (wrong code counts in the name's window, F2) |
| `POST /v1/password/reset` | exempt | `ResetPasswordRequestV1` → 200 `PasswordUpdatedResponseV1` (no credential) | 404 `sign_in_link_invalid`, `password_unacceptable` |
| `POST /v1/sign-in-links/preview` (existing) | exempt | → `SignInLinkPreviewV1` with `purpose` | `sign_in_link_invalid` for `enrollment` and `conversion` links, which have no landing page |
| `POST /v1/sign-in-links/redeem` (existing) | exempt | purposes `sign_in` and `device` only | `sign_in_link_invalid` for any other purpose |
| `POST /v1/setup/redeem` | exempt | `RedeemSetupTokenRequestV1` → 201 `RedeemSetupTokenResponseV1` | 404 `setup_token_invalid` |
| `GET /v1/whoami` (existing) | authenticated | + `identity_class`, `guest`, `auth` | the one route beside its own a `setup` credential may call |
| `POST /v1/guest/convert` | authenticated, guest | `ConvertGuestRequestV1` → 200 `ConvertGuestResponseV1` | 403 `not_a_guest`, 409 `guest_ended`, 403 `conversion_not_allowed`, 404 `conversion_permit_invalid`, `joining_paused`/`invites_paused` by origin, `principal_exists`, `password_unacceptable` |
| `POST /v1/guest/end` | authenticated, guest | → 200 `EndVisitResponseV1` | `not_a_guest`, `guest_ended` |
| `DELETE /v1/credentials/current`, `…/credentials/revoke-all` (existing) | authenticated | for a guest: End visit in the same transaction; for a member: also revoke links, pending logins and the recent-auth proof | — |
| `POST /v1/password/mine` | authenticated | `SetPasswordRequestV1` → 200 `PasswordUpdatedResponseV1` | operator on an ordinary bearer: 403 `setup_on_host` (F7); on a `setup` credential it **replaces** any existing password (the host-made token is the proof; this is operator recovery); member without a proof: 403 `enrollment_proof_required` (not `reauth_required`: there is no password to prove); a proof whose target is not the caller, or whose purpose is not `enrollment`: 404 `sign_in_link_invalid`; 409 `password_already_set` on a full-scope credential only; guest: `guest_not_allowed` |
| `PUT /v1/password/mine` | authenticated | `ChangePasswordRequestV1` → 200 `PasswordUpdatedResponseV1` (rotated token) | 401 `auth_failed` (wrong current, or an operator's missing/wrong code), 409 `password_not_set`, `guest_not_allowed` |
| `POST /v1/reauth` | authenticated | `ReauthRequestV1` → 200 `ReauthResponseV1` | 401 `auth_failed` (an operator's missing or wrong `code` included; every failure counts in the name's window, F2), 409 `password_not_set`, `guest_not_allowed` |
| `POST /v1/totp/enroll` | authenticated, `setup` scope | → 200 `TOTPEnrollmentV1` | 403 `setup_on_host` on a full-scope credential, 409 `totp_already_enrolled` |
| `POST /v1/totp/confirm` | authenticated, `setup` scope | `ConfirmTOTPRequestV1` → 204 | 401 `auth_failed`, 409 `totp_not_enrolled` |
| `POST /v1/sign-in-links/mine` | authenticated, member | `CreateDeviceLinkRequestV1` → 201 `CreateSignInLinkResponseV1` (purpose `device`) | 403 `reauth_required`, 409 `too_many_device_links`, `guest_not_allowed`; operators and agents 403 |
| `GET /v1/sign-in-links/mine` | authenticated, member | → `ListOwnSignInLinksResponseV1` | — |
| `DELETE /v1/sign-in-links/mine/{id}` | authenticated, member | → 204 | 404 `sign_in_link_not_found` |
| `GET /v1/join/settings` | operator | → `JoinSettingsResponseV1` | — |
| `PUT /v1/join/settings` | operator, **R** | `PutJoinSettingsRequestV1` → 200 `JoinSettingsResponseV1` | 409 `join_policy_conflict` (guest-only with guests off, open joining with no channel, last open channel removed while on, enabling guests with `reserved_names_in_use`), 400 `invalid_request` for an unknown channel |
| `GET /v1/guests` | operator | `?state=`(default `active`)`&origin=&before=&limit=` → `ListGuestsResponseV1` | — |
| `POST /v1/guests/{id}/end` | operator, **R** | → 200 `EndGuestResponseV1` | 404 `not_a_guest`, 409 `guest_ended` |
| `POST /v1/guests/end` | operator, **R** | `EndGuestsRequestV1` → 200 `EndGuestsResponseV1` | — |
| `POST /v1/guests/{id}/conversion-permit` | operator, **R** | `CreateConversionPermitRequestV1` → 201 `CreateSignInLinkResponseV1` (purpose `conversion`) | 404 `not_a_guest`, 409 `guest_ended` |
| `POST /v1/principals/{id}/sign-in-links` (existing) | operator, **R** | + `purpose` `reset` or `enrollment` | guest, operator, disabled or hidden target: as today for sign-in links |
| `PUT /v1/channels/{channel}/members/{id}` (existing, approval-path) | operator, **R** | a guest target: `can_decide` omitted stays false, `true` is 403 `guest_not_allowed`; admission sets `history_after` | — |
| `POST /v1/principals/{id}/hide` (existing) | operator | guest target: 409 `guest_not_hideable` | — |
| `POST /v1/principals/{id}/credentials` (existing) | operator, **R** | guest target: 403 `guest_not_allowed` | — |
| `GET /v1/principals` (existing) | operator | + `guest_state`; `?guest=active|ended|converted` filter | — |
| `PUT /v1/principals/{id}/name` (existing) | operator-or-self | a principal with an active guest visit: 409 `guest_not_hideable`, whether an operator or the guest asks; its name is its handle | — |
| Guest refusals on existing routes | authenticated | `POST /v1/invites`, `POST /v2/channels/{channel}/files`, `POST /v1/approvals`, `POST /v1/approvals/{id}/decisions` (G2, approval-path), `POST /v1/runs/{id}/interventions`, `POST /v1/sign-in-links/mine`: 403 `guest_not_allowed` | — |
| Every read and replay route | authenticated | intersects `history_after` with the existing audience rules (§4.3) | — |
| Host commands (not routes) | offline | `conchd operator bootstrap <name>` and `conchd operator recover <id>` print a setup token (10 min); recover first revokes the operator's credentials, links, pending logins and TOTP, bumps its auth generation and audits; the setup-scoped credential the token answers then **replaces** the password through `POST /v1/password/mine` and enrols TOTP afresh | names are #710's to settle |

Rate limits: §8. The approval routes' guest refusal and the `can_decide` rule are
approval-path work (G2). Bearer token credentials (CLI and scripts, not browser
sessions) are outside the R class by the S15 boundary of §5.3: the route-table test
encodes R for browser-session credentials only, and the exposure of a stolen full-power
script credential is the residual risk already recorded in the review record (§11).

### 7.3 Migrations

Both migrations are additive (`ADD COLUMN`, new tables, new rows): old rows stay readable,
no name is changed, hidden or reclassified, and a database from before either migration
opens as today. Numbers follow `store.go`'s list, whose last entry is 30 (sign-in links).

**31 — guest identities (#705).**
- `guest_visits (principal_id INTEGER PRIMARY KEY REFERENCES principals, handle TEXT NOT
  NULL UNIQUE, label TEXT, origin TEXT NOT NULL CHECK (origin IN ('address','invite')),
  invite_id INTEGER REFERENCES invites, state TEXT NOT NULL CHECK (state IN
  ('active','ended','converted')), started_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
  idle_ms INTEGER NOT NULL, ended_at INTEGER, end_reason TEXT CHECK (end_reason IS NULL OR
  end_reason IN ('end_visit','idle','expired','operator','restore')), converted_at INTEGER,
  CHECK ((state = 'ended') = (ended_at IS NOT NULL AND end_reason IS NOT NULL)), CHECK
  ((state = 'converted') = (converted_at IS NOT NULL)))`, with a partial index on
  `state = 'active'` and one on `invite_id`. Rows are never deleted, so a handle is never
  reused. A principal is a guest exactly when its row is `active`; every store check of the
  guest flag reads this row inside its transaction (F1).
- `channel_members ADD COLUMN history_after INTEGER` (NULL = no bound). Set at every
  guest admission to the channel's last message id; kept through conversion.
- `message_guest_attribution (message_id INTEGER PRIMARY KEY REFERENCES messages, handle
  TEXT NOT NULL, label TEXT)`, written in the posting transaction for a guest author and
  never updated; `MessageV2.guest_author` is read from it by a left join.
- `credentials ADD COLUMN idle_ms INTEGER` (NULL = no idle bound) and `ADD COLUMN
  last_used_at INTEGER` (NULL = never; updated by REST requests at most once a minute,
  or once a quarter of the idle bound, whichever is shorter; issue #812).
  `CredentialLive` becomes: not revoked, principal enabled, `now < expires_at` when set,
  and `idle_ms IS NULL OR now < COALESCE(last_used_at, created_at) + idle_ms` (F5).
- `open_channels (channel_id INTEGER PRIMARY KEY REFERENCES channels, added_by INTEGER
  REFERENCES principals, added_at INTEGER NOT NULL)`.
- `server_settings` rows `open_joining`, `guests_enabled`, `joining_paused`, `join_voice`,
  `max_guests`, `max_guests_per_address`, `guest_idle_seconds`, `guest_absolute_seconds`;
  an absent row is the default of `schema.DefaultJoinSettings`.
- `invites ADD COLUMN join_as TEXT NOT NULL DEFAULT 'account' CHECK (join_as IN
  ('account','guest','both'))`: every existing invite stays account-only.
- Go step, like migration 28's `reportReservedNames`: log every principal whose folded key
  or skeleton (`principals.name_key`, `name_skel`) begins with `guest`. Nothing is renamed.
  While any such principal exists, `PUT /v1/join/settings` refuses `guests_enabled: true`
  with `join_policy_conflict` and the settings answer carries `reserved_names_in_use`.
- Audit actions, written in the committing transaction: `guest_started` (entry point:
  `address` or `invite <id>`; never the client address, F13), `guest_ended` (reason),
  `guest_converted`, `guests_ended` (count, selection), `join_settings_changed` (each field,
  old and new, plus the live-guest count when guests are turned off, F12),
  `open_channel_added` / `open_channel_removed`, `conversion_permit_issued`.

**32 — passwords and operator authentication (#707).**
- `passwords (principal_id INTEGER PRIMARY KEY REFERENCES principals, hash TEXT NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`; `hash` is the encoded
  `$argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>` record of §5.1, parameters bounded before
  verification. A principal with no row has no password: a legacy member, a guest, an
  agent or an operator not yet set up.
- `principals ADD COLUMN auth_generation INTEGER NOT NULL DEFAULT 0`. Every credential,
  link, permit, pending login and setup token records the principal's generation at
  issue (`auth_generation INTEGER NOT NULL DEFAULT 0` on `credentials`, `sign_in_links`,
  `pending_logins`, `setup_tokens`) and is live only while it equals the principal's. A
  password change or reset, sign-out everywhere, disable, operator recovery and a restore
  bump it; existing rows carry 0 and the existing principals start at 0, so nothing is
  invalidated by the migration itself.
- `credentials ADD COLUMN scope TEXT NOT NULL DEFAULT 'full' CHECK (scope IN
  ('full','setup'))`, `ADD COLUMN recent_auth_until INTEGER` (the §5.3/§6.2 proof, bound
  to this row; cleared by any generation bump).
- `sign_in_links ADD COLUMN purpose TEXT NOT NULL DEFAULT 'sign_in' CHECK (purpose IN
  ('sign_in','device','reset','enrollment','conversion'))`, `ADD COLUMN label TEXT`.
- `pending_logins (id INTEGER PRIMARY KEY, challenge_hash TEXT NOT NULL UNIQUE,
  principal_id INTEGER NOT NULL REFERENCES principals, address_key TEXT NOT NULL,
  auth_generation INTEGER NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT
  NULL, attempts INTEGER NOT NULL DEFAULT 0, spent_at INTEGER)` (F14; `address_key` is
  `limitKey`'s form).
- `totp (principal_id INTEGER PRIMARY KEY REFERENCES principals, secret BLOB, enrolled_at
  INTEGER, pending_secret BLOB, pending_at INTEGER, last_step INTEGER NOT NULL DEFAULT 0)`:
  both secrets encrypted under the host key of §5.3; `last_step` is the highest accepted
  time step, for replay refusal.
- `setup_tokens (id INTEGER PRIMARY KEY, token_hash TEXT NOT NULL UNIQUE, principal_id
  INTEGER NOT NULL REFERENCES principals, auth_generation INTEGER NOT NULL, created_at
  INTEGER NOT NULL, expires_at INTEGER NOT NULL, spent_at INTEGER)`.
- Backfill: none. Existing members keep signing in with their credentials and sign-in
  links; `POST /v1/login` answers `auth_failed` for them until an operator issues an
  enrollment proof; `whoami.auth.setup_complete` is false. Existing operators keep their
  script credentials (full scope, no MFA, §5.3); their browser sign-in needs a host-made
  setup token. From this migration on, `POST /v1/invites/redeem` (v1) answers
  `password_required` (§7.6 B1).
- **Ordering.** The routes that create a member with a password, `POST /v1/join/account`,
  `POST /v2/invites/redeem` and `POST /v1/guest/convert`, are registered by #707 with this
  migration, not by #705, so no build between the two can mint a passwordless member. #705
  lands the store's reserved-`guest` refusal (§7.6 B2) in the same PR as migration 31.
- **Restore invalidation** (§4.2; the trigger is #716's): one statement, `UPDATE principals
  SET auth_generation = auth_generation + 1`, plus ending every `active` guest visit with
  reason `restore`, before the server accepts traffic. Sweeping expired rows is not
  enough and is not what this does. A restore also rolls `totp.last_step` back, so a code
  accepted before the backup could be accepted again: #716 refuses every TOTP code for the
  current step and one either side (`TOTPWindowSteps`) for the first `2 × 30 s` after the
  restored server starts, which is the whole window a replayed code can fall in.
- Audit actions: `password_set`, `password_changed`, `password_reset_issued`,
  `password_reset`, `enrollment_issued`, `login_failed` (the name key when it resolves to a
  principal, else the constant `unknown`, and the reason code; nothing else),
  `login` (credential id), `reauth`, `totp_enrolled`, `setup_token_issued` (host),
  `setup_token_redeemed`, `operator_recovered` (host), `device_link_issued`,
  `auth_generation_bumped` (cause). No password, hash, code, token or full address in any
  of them.

### 7.4 Authentication off

With `conchd` running with authentication off (`AuthMode` `off`), there is no identity to
join, sign in or prove, so:
- `GET /v1/join` answers `password_login: false` and `join: {guest: false, account:
  false}` (fixture `join-info-v1-auth-off.json`);
- every other route of §7.2 that this design adds, and the new purposes on the existing
  sign-in link routes, answer 403 `accounts_require_auth`, as the ticket, session and
  read-state routes answer their own `*_require_auth` codes today;
- `GET /v1/whoami` omits `identity_class`, `guest` and `auth`;
- existing routes behave as today; the guest refusals are moot because no guest can exist;
- migrations 31 and 32 still run, so switching authentication on later needs no further
  migration.

### 7.5 REST, CLI/TUI and browser

Rule 4: the API first; `conch` and the browser are its clients. CLI names are proposals
for G7 (#713); the browser pages for G6 (#711–#712). Secrets reach the CLI on
non-echoing stdin (`conch login < tokenfile` is the precedent), never in `argv`.

| REST | CLI/TUI | Browser |
| --- | --- | --- |
| `GET /v1/join` | `conch join info --server` | front door `/app/` when signed out: the two buttons and Sign in, from `join` |
| `POST /v1/join/guest`, `POST /v1/invites/redeem-guest` | `conch join guest [--label] [--invite <code>]` | **Join as guest** with the optional label; the token in `sessionStorage` for the tab, the visit's end time shown |
| `POST /v1/join/account`, `POST /v2/invites/redeem` | `conch join account <name> [--invite <code>]` (password prompted) | **Create an account**: name and password together |
| `POST /v1/login`, `POST /v1/login/totp` | `conch login --password` (name, password, then code prompted; today's token login stays for scripts) | Sign in page; the TOTP step when `next` is `totp`; the browser refuses a script token where a person signs in |
| `GET /v1/whoami` | `conch whoami` shows class, visit bounds, setup state | header and settings read `identity_class`, `guest`, `auth` |
| `POST /v1/guest/convert` | `conch account create <name>` | guest banner → **Create an account** |
| `POST /v1/guest/end` | `conch end-visit` | guest banner → **End visit** |
| `POST /v1/password/mine`, `PUT /v1/password/mine` | `conch password set [--proof]`, `conch password change` | Settings → Password; setup page on a `setup` credential |
| `POST /v1/password/reset` | `conch password reset <code via stdin>` | reset landing page from the link's fragment (`purpose: reset`) |
| `POST /v1/reauth` | prompted when a command answers `reauth_required`, then retried | a modal when a call answers `reauth_required`, then retried |
| `POST /v1/totp/enroll`, `POST /v1/totp/confirm` | `conch totp enroll` (prints the manual setup; no QR dependency) | setup page: secret and manual parameters, hidden while the tab shares |
| `POST /v1/setup/redeem` | `conch setup <token via stdin>` | setup landing page |
| `POST/GET/DELETE /v1/sign-in-links/mine[/{id}]` | `conch device-link create|list|close` | Settings → Your sign-ins → Add a device (Reveal, refused while sharing) |
| `GET/PUT /v1/join/settings` | `conch admin join status|set --open-joining … --guests on|off …` | Operator settings → Joining |
| `GET /v1/guests`, `POST /v1/guests/{id}/end`, `POST /v1/guests/end`, `POST /v1/guests/{id}/conversion-permit` | `conch admin guest list|end|end-all [--dry-run] [--origin] [--invite]|permit` | Operator → Guests: list, End visit, End all guest visits (dry run then confirm), Allow account |
| `POST /v1/principals/{id}/sign-in-links` with `purpose` | `conch admin principal sign-in-link --purpose reset|enrollment <id>` | Operator → principal → Reset password / Set up password |
| `POST /v1/invites` with `join_as` | `conch admin invite create --join-as account|guest|both` | invite creation: the mode selector, shown only when guests are enabled |
| host commands | `conchd operator bootstrap <name>`, `conchd operator recover <id>` | — |

### 7.6 Changes that need the owner's sign-off

No published fixture changed and no schema version was bumped (`scripts/schema-compat.sh`
passes). The following are behaviour changes on published routes or shapes, listed for
Nick's decision (ADR-000 D8); each is additive on the wire.

- **B1. `POST /v1/invites/redeem` (v1) stops creating accounts.** Once migration 32 has
  run it answers 409 `password_required` and makes nothing, so an old client cannot make
  a passwordless account. `RedeemInviteRequestV1` and its fixture are unchanged; the v2
  route takes the password. Alternative considered: an optional `password` on the v1
  shape, refused when absent, which would leave one route with two meanings. Recommended:
  B1 as written. Needs sign-off.
- **B2. The reserved guest namespace on existing routes.** After migration 31, `POST
  /v0/principals`, `PUT /v1/principals/{id}/name` and the v1 invite redemption refuse a
  name whose folded key or skeleton begins with `guest` (409 `principal_exists`, as for
  `hidden-`), enforced by the store as the hidden prefix is; the published `Validate`
  methods are unchanged. New names only; existing ones are reported, never changed.
  Needs sign-off as a tightening of published routes.
- **B3. `MessageV2` keeps version 2** with the optional `guest_author`; a client that
  predates it shows the author's current name on an old guest post, which is a rendering
  degradation and not an exposure. The alternative, `conch.message.v3`, would touch every
  message route, socket and fixture for one optional field. Recommended: stay on v2. Nick
  may overrule.
- **B4. Sign-out routes end a guest visit.** `DELETE /v1/credentials/current` and
  `revoke-all` for a guest end the visit (terminal, memberships removed), beyond what they
  did for a member. Compatible; recorded.
- **B5. `ChannelMembersV1.Validate` and `MessageV2.Validate` now refuse** a malformed
  `guest`/`guest_author` and a guest member with `can_decide: true`. Documents without
  those fields validate exactly as before.
- Not a break, for the record: the existing fixtures `channel-member-v1-can-decide.json`
  and `channel-members-v1.json` name a member `guest-a`. They are published and stay; on a
  real server such a name is a migration 31 preflight conflict, not a guest.

Store password hashes/parameters, auth generation, restricted hashed link tokens and
operator encrypted TOTP state separately from public principal shapes (§7.3 does). Guest
memberships carry watermarks; immutable attribution survives membership removal. Audit
changes in the committing transaction, including guest start/end reason, conversion,
password events, policy changes and grants. Public reports contain no credentials,
conversation content, TOTP secrets or names from unauthorized groups.

## 8. Abuse limits and security cases

- Guest cap checked in the creation transaction across address and invite paths.
  Count only live visits; recheck concurrent expiry/conversion and creation. A per-address
  cap keeps a few addresses from filling `max_guests` *(F13)*: the setting
  `max_guests_per_address`, default five, keyed like `limitKey` (`clientaddr.go`, IPv6 by
  /64). Behind an unset trusted proxy or one shared NAT every guest shares an address, so
  the response to enabling guests and `/health` warn when a trusted proxy is unset or every
  live guest arrives from one address.
- Guest churn: End visit and rejoin yields a fresh identity, so hiding or disabling one
  visit never reaches the person. The levers are pausing or revoking the entry invite,
  `guests_enabled`, `max_guests` and the per-address cap. `guest_started` audits the entry
  point, never the address, following the invite audit rule (`invite_limits.go`) (F13).
- Join defaults retain existing invite limiter behavior: per-address 10 per ten minutes,
  shared server-wide creation limit 20/minute. Name-conflict probes get a separate five/hour
  allowance, one bucket shared by `POST /v1/join/account`, `POST /v2/invites/redeem` and
  `POST /v1/guest/convert`. Guest names are generated, so guests cannot probe/reserve
  permanent names.
- Login defaults: ten **failures** per address per ten minutes (successes spend nothing),
  and the per-name window of §5.2: five failures per name key per ten minutes, then an
  immediate 429 with a not-before time, never a server-side delay; the same policy for
  nonexistent names. Wrong TOTP codes and re-authentication pairs count in the same window
  (§5.3). Only issuing a credential resets it, and success does not clear an unrelated
  attacker's throttling state. 30 successes per name key per hour. TOTP completion permits
  at most five attempts per challenge. Reset/enrollment/reauth/device links have bounded
  per-address and per-principal buckets. All password work also uses §5.1's global pool.
- Bound limiter key maps, guest metadata, pending challenges, request sizes and audit volume.
  Prefer safe errors to unbounded queues. A distributed attack cannot allocate unlimited
  Argon2 memory by choosing new names.
- Trusted proxy configuration determines client-address rate limits. Refuse untrusted
  forwarding assertions; warn/document shared limits when all callers appear as the proxy.
- Guest history bounds apply below handlers so direct REST/WS/CLI access cannot bypass UI
  filtering. Maintain existing group/net/whisper rules; test nested and indirect references.
- Reserve guest prefix on every permanent-name mutation. Do not use a displayed name as
  proof of identity. Conversion retains author attribution and cannot reset abuse origin.
- Logout/expiry/disable/reset races must reject subsequent use and close live sessions/media.
  End-all-guests excludes converted members; origin-based bulk operations list their targets
  in a dry run so converted accounts are not unexpectedly treated as active guests.
- Browser bearer storage retains XSS exposure. CSP, no third-party scripts, secret redaction
  and limited token lifetimes remain required. Password/TOTP proofs guard credential changes;
  they do not make stolen live bearers harmless.
- Link previews and confirmations mitigate account-switch phishing. Password resets revoke
  outstanding links so revoking a session is not undone by a previously minted login link.

## 9. Acceptance and tests

Required real-server isolation and browser tests:
1. Master guest gate off: guest creation denied through address, personal invite and open
   link, including direct API calls and stale cached pages; account paths still work.
2. Guest modes and account modes honored on every entry point. Guest-only cannot convert
   without the explicit enrollment permit. Paused/revoked/expired entries fail closed.
3. Generated names unique under concurrency, never reused; case/confusable guest-prefix
   permanent signup/rename/import refused, including operator writes. Two visits with the
   same display label render with different tags, neither label signs in or is accepted as
   a principal reference, and a refused label (control/bidi/confusable/reserved) is refused. Existing-name
   conflicts produce an explicit migration report without destructive renames.
4. Refresh and short disconnect retain a live visit. Logout, idle expiry, hard expiry,
   operator revoke and browser-loss cleanup terminate it. Test sockets/media against the
   stated time bounds, real clock path, server restart and a restored expired database.
5. A new visit cannot read old private content through history, direct IDs, search,
   threads, quotes, attachments, WS replay, notifications, counts or indirect objects.
   Seed pre-visit private markers and probe every route. Channel membership alone is
   insufficient. Cross-group and net/whisper isolation remain covered.
6. Posted guest messages retain original attribution after end, hide and conversion;
   other authorized members can read them, unauthorized visitors cannot.
7. Conversion requires working password and permanent name. Cancel, refused password,
   duplicate name, policy change, expired visit, duplicate request and a logout race
   never create a partial account or widen access. Lost success response permits password
   login. Verify same principal/history bounds and no new permissions.
8. Member create → logout → password login on a new browser works. All failure classes
   share response shape. Reset/change invalidate old sessions, pending auth and all links;
   wrong/expired/replayed/cross-purpose reset proofs fail. An enrollment proof made for
   another principal, or a link of any other purpose presented as one, is refused with
   `sign_in_link_invalid` and sets no password (G4). An operator with a password but no
   confirmed TOTP, and one with neither, gets `auth_failed` from `POST /v1/login`.
9. Operator password alone yields no API access. TOTP enrollment, replay prevention, idle/
   absolute expiry, recent-auth coverage and offline recovery tested. Script tokens cannot
   mint MFA sessions. Test official RFC vectors and clock-window boundaries.
10. Guests cannot invite, upload, create channels/private conversations, intervene in agents,
    create credentials or request/decide approvals, even if an operator tries to grant them
    a role. An operator adding a guest with `can_decide` omitted leaves it 0, an explicit
    `true` is refused, and a guest's decision is refused in the deciding transaction. After
    conversion `can_decide` is still 0 and no privilege appears without an authorized grant.
11. Approval-sensitive changes also run the complete authorized request → notify → resolve
    → audit scenario, alongside guest refusal tests; refusal-only tests are insufficient.
12. Argon2 concurrency/queue bounds, throttling, dummy verification, input limits, hash
    upgrades, transaction races, secret redaction and HTTPS/proxy enforcement tested.
    The sixth failure on an existing and on an unknown name gets the same immediate 429.
    Wrong TOTP codes across many challenges and addresses hit the name's window. During an
    anonymous login flood an operator re-authentication and a guest conversion complete.
    A guest token kept alive only by a socket or ping loop expires at the idle bound.
    A setup-scoped credential is refused everywhere but its three routes.
13. CLI/TUI login, guest entry/end, conversion, operator reauth and recovery administration
    have equivalent REST semantics and no secrets in process arguments.
14. Every new route has an isolation matrix row or a narrowly justified exemption and
    a concrete authentication test. Do not leave password/operator routes exempt by default.

## 10. Work breakdown

Each item becomes an executable issue after ADR approval, with Context / Files / Acceptance
criteria / Required tests / Blocked by. The table is sufficient to locate the work without
a private scratchpad. Track larger slices as parents with bounded child PRs.

| ID | Work | Likely files/areas | Depends on |
| --- | --- | --- | --- |
| G1 | Schema and compatibility; auth/session types, invite modes, guest history and attribution contracts | pkg/schema, fixtures, generated web/src/schema | ADR approval |
| G2 | Guest namespace/lifecycle, membership bounds, attribution migration; guest restrictions and approval gates | internal/server/store, authorization/read paths, approvals_handlers.go, ws.go, voice entitlement | G1 |
| G3 | Entry policy, invite/account signup, guest cap, atomic conversion and operator permits | server join/invite handlers, store transactions, policy settings | G2, G4 password storage/enrollment contract |
| G4 | Passwords, member recovery, operator password/TOTP and offline setup; split into storage/login, reset/enrollment, operator-auth child issues | server auth/store/handlers, CLI host commands, go.mod/go.sum, deps-allowlist.txt | G1, verified dependency pin |
| G5 | Add a device with recent-password proof and reset invalidation | sign-in-link store/handlers, own credentials | G4 |
| G6 | Front door, guest lifecycle, permanent signup/conversion and return login | web auth/onboarding/settings and API client | G2, G3, G4 |
| G7 | Operator auth/recovery and device-link UI; CLI/TUI parity | web settings/admin, internal/cli, TUI | G4, G5, G6 |
| G8 | Cross-route guest history/attribution and approval-path integration tests | e2e/isolation, authorization suites, approval e2e | G2, G3 |
| G9 | Password/operator/recovery isolation and abuse/resource tests | auth/store tests, e2e/isolation | G4, G5 |
| G10 | Browser/session end-to-end acceptance and documentation | web/e2e, tester/deploy guides, schema fixtures | G6, G7, G8, G9 |

G2 and G4 can start after G1, with ownership agreed for overlapping principal/store files.
G3 follows their contracts; no placeholder passwordless signup ships while G4 is missing:
`POST /v1/join/account`, `POST /v2/invites/redeem` and `POST /v1/guest/convert` are
registered by #707 together with migration 32 (§7.3), never by #705, and #705 lands the
store's reserved-`guest` refusal with migration 31.
Stage coherent slices behind disabled feature policy until their isolation tests pass.

G2 and any other child changing approval request/decision authorization are **approval-path**:
label the PR, include the full chain test and require Nick's personal merge.
Every security-sensitive implementation PR needs the existing security-reviewer pass.
No blanket statement that this work avoids the approval path is valid.

## 11. Review record and remaining gates

The original draft's security review covered passkeys and seven-day guests. It does not
approve the password/temporary-visit revision. Preserve the old review in Git history.

Review corrections in this revision:
- Passwords-first, usable account setup and normal operator login are reflected in the ADR,
  flows, schema scope, work dependencies and PR description together.
- Guest policy applies to invitations as well as the bare address.
- Generated guest names cannot reserve permanent names; historical attribution is separate
  from active access and survives terminal cleanup.
- Approval-path implementation work is classified explicitly.

**Security review of af0c3bc (#703, 2026-10-21).** An independent security-reviewer pass on
this revision returned BLOCK with two must-fixes and seven should-fixes, all adopted above:
F1 guest `can_decide` enforced in the store (§4.3), F2 one failure count for every factor
(§5.3), F3 two hashing pools (§5.1), F4 immediate per-name 429 with no sleep (§5.2), F5 idle
refreshed only by REST requests and applied in `CredentialLive` (§4.2), F6 setup scope in
`authMiddleware` (§5.3), F7 operator enrolment only through the host (§5.2), F8 re-auth class
defined by rule (§5.3), F9 cap on successful sign-ins (§5.2). Nits F10–F14 adopted: guests
not hideable (§4.4), file routes (§4.3), the guests-off response (§2), churn levers and a
per-address cap (§8), challenges bound to address (§5.3). The same pass also added #694's
display labels (§4.1). A re-check of the amended text passed after four small corrections. Full record: [docs/reviews/adr-009-security-review.md](../reviews/adr-009-security-review.md).

Before implementation/release:
- Approve this ADR; choose/adjust proposed session and password-work defaults.
- Verify the selected x/crypto pin, full resolved dependency graph and security status in G4.
- Review the password/TOTP/recovery, guest-history and migration contracts independently.
- Implement and run the gates above; no runnable feature or measured performance is claimed
  by a documentation review. Standard repository checks remain required on the eventual PRs.

### Revision validation

Codex re-reviewed this documentation revision against the four findings and the owner's
2026-10-21 guest clarification. The consistency pass also corrected backup restoration
(which must invalidate restored authentication, not just sweep expired rows) and made
expiry/revocation timing bounds explicit. Relative file targets and invisible-character
checks passed for the six edited documents before commit. These checks do not exercise
software behavior. The execution environment was unavailable, so no make check, build,
browser session, dependency install, performance measurement or independent security-agent
review was run for this revision. The earlier PASS must not be reused as approval here.
