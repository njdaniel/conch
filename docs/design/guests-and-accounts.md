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
`guest_not_hideable`; End visit, the entry invite's revocation and End all guest visits are
the levers. Hiding a converted member renames the member identity as today, while posts
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

Proposed route families (final names owned by G1):
- Anonymous: join information, address guest/account creation, invite preview/redemption,
  password login, TOTP login completion, restricted reset/enrollment preview/completion.
- Self: guest conversion, password change, own sessions, reauthentication, device links,
  explicit end-visit; each requires the correct live identity class.
- Operator: joining settings, invite modes, guest termination/list, permitted membership
  changes, member enrollment/reset links and conversion permits; sensitive routes require
  recent authentication as specified in §5.3.
- Offline only: operator bootstrap/authentication recovery.
- Existing routes: guest restrictions for credentials, invitations, file upload, channel
  creation, approvals and interventions; history enforcement on every read/replay route.

Every authenticated route rejects AuthOff (401); anonymous account flows are unavailable
when the server cannot enforce identity/ACLs. Bind all ceremony/link types to their purpose,
target, expiry and authentication generation. A reset token is not a device link, an
enrollment proof is not an operator session, and no principal ID in a request replaces
the authenticated caller.

Store password hashes/parameters, auth generation, restricted hashed link tokens and
operator encrypted TOTP state separately from public principal shapes. Guest memberships
carry watermarks; immutable attribution survives membership removal. Foreign keys and
migration/backfill behavior must be specified before implementation. Audit changes in the
committing transaction, including guest start/end reason, conversion, password events,
policy changes and grants. Public reports contain no credentials, conversation content,
TOTP secrets or names from unauthorized groups.

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
  allowance. Guest names are generated, so guests cannot probe/reserve permanent names.
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
   wrong/expired/replayed/cross-purpose reset proofs fail.
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
G3 follows their contracts; no placeholder passwordless signup ships while G4 is missing.
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
