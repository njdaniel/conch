# ADR-009: Temporary guests, accounts and signing in again

- **Status:** Proposed; accepted only when Nick merges this PR.
- **Date:** 2026-10-21
- **Decider:** Nick
- **Proposal:** [#694](https://github.com/njdaniel/conch/issues/694)
- **Design:** [guests-and-accounts.md](../design/guests-and-accounts.md)
- **Amended:** 2026-10-30 ([#975](https://github.com/njdaniel/conch/issues/975)): the 15-character password minimum of the design (§5.2) is removed by Nick's decision. A password to be set must be non-empty, valid UTF-8 and within the 128-code-point and 512-byte bounds after NFC; no other minimum, per-role minimum or setting replaces it, for members and operators, on normal and temporary servers. The blocklist, name and repetition checks, hashing, rate limits, recovery and operator TOTP are unchanged, and existing passwords keep working.
- **Amends:** ADR-008's invite-only/account model; ADR-000 and ROADMAP.md's placement of password login at P7; the member browser storage rule for temporary guests only. Operator browser sessions remain tab-scoped, with stronger authentication and bounded lifetime.

## Context

Opening an allowed link should be enough to join a conversation. A temporary visitor
does not need a lasting account, but someone who wants retained access, permissions or
previous private conversations must create a real account.

The original PR proposed passkeys and seven-day renewable guests. The owner's subsequent
decision is passwords first, Argon2id via x/crypto approved, and a distinct temporary guest
namespace. Those decisions replace the original D2/D3. This is a design, not implemented behavior.

## Decision

1. **Joining policy (D1).** Open joining from the bare address is off by default and can
   offer guests, accounts or both. A separate server-wide `guests_enabled` switch,
   off by default, controls guest creation through **every** entry point, including
   invitations. An invite can further restrict which account kinds it allows; it cannot
   override server policy. Existing invites remain account-only unless explicitly changed.
   Turning guest creation off leaves existing visits to their bounds; an explicit,
   separately confirmed end-all-guests action ends them immediately.
2. **Temporary guests (D2).** The server generates a unique, never-reused
   `guest-<random suffix>` name. The normalized `guest` prefix is reserved against
   permanent usernames, including case and confusable variants. A guest may add a
   non-unique display label, always rendered as `Guest: <label> · <tag>` and never used to
   sign in or to name a principal. Guest identity is not
   recoverable by retyping a name, using the same browser, or reopening an invitation.
   One tab-scoped credential survives refresh and brief reconnects. Proposed defaults
   are two hours idle and 24 hours absolute, bounded by any shorter invitation lifetime.
   Logout/End visit, revocation or expiry ends the visit and its access permanently.
   Closing a browser is not a reliable server signal; expiry is the cleanup backstop.
3. **Temporary access, retained attribution.** Guests receive only the conversation access
   needed for their current visit: text and, when granted, voice/screen sharing. They cannot
   receive durable roles, invite, administer, create private conversations, request/decide
   approvals or recover a previous visit's private access. Guest history is bounded to
   the current membership's admission point on every read surface. Messages already posted
   remain under their original guest attribution; ending a visit does not promise message
   deletion. Minimal historical identity/audit records are retained without live credentials
   or memberships. Permanent usernames are never occupied by guest names.
4. **Real accounts and conversion (D3).** Name and password are collected together.
   Account creation and conversion succeed only when a usable password is stored.
   A live guest can choose **Create an account**, select a non-guest username and password,
   and retain the same principal ID, current permitted memberships and history boundaries.
   Original guest message attribution remains identifiable. The operation grants no new
   channels, roles, history or approval/invite privileges. Cancellation, validation failure
   or a closed signup policy leaves the guest unchanged. Ended visits cannot be converted
   or recovered. Becoming a member is necessary, but not sufficient, for extra permissions.
5. **Passwords first.** Use Argon2id through `golang.org/x/crypto/argon2`.
   `golang.org/x/crypto v0.55.0` is the Go-1.25-compatible pin selected from inspected
   upstream go.mod files; implementation must verify the resolved graph, licenses,
   vulnerability status and resource use before adding the approved dependency.
   No WebAuthn dependency or Go 1.26 upgrade is included. Passkeys/OIDC are deferred.
   Members can sign in with username/password on another device; Add a device is optional.
6. **Operators and recovery.** Operators get normal username/password plus TOTP sign-in,
   tab-scoped sessions (12-hour absolute, one-hour idle), and recent reauthentication for
   sensitive changes. Offline bootstrap/recovery remains available. Member recovery uses
   an operator-issued password-reset link; self-service recovery codes or optional email
   may follow. No external mail service is required, and no claim is made that recovery
   inherently requires one. Existing script credentials are not browser MFA sessions.
7. **Abuse and isolation (D4–D6).** Name uniqueness, reserved names, bounded rate limits,
   password-work concurrency limits, guest caps, pause switches and bulk revocation are
   enforced server-side and audited. Guest creation policy, history boundaries, conversion,
   expiry, password recovery and operator authentication receive explicit end-to-end tests.

## Alternatives

| Choice | Reason |
| --- | --- |
| Temporary names in a reserved namespace | Visitors cannot occupy a permanent username or regain access by choosing an old guest name. |
| Long-lived renewable guests | Rejected: obscures the distinction between a visit and an account. |
| Keep a guest's chosen normal username reserved after logout | Rejected: prevents legitimate account signup and makes return visits confusing. |
| Passwords before passkeys | Familiar return login without a WebAuthn/toolchain change; password storage, throttling and recovery must be implemented carefully. |
| Bare-address-only guest switch | Rejected: invitation redemption would bypass the policy. |
| Delete all guest messages at logout | Rejected: other participants' conversation history and attribution must remain coherent. |

## Invariants and consequences

- No new required external process, multi-tenancy, federation, E2EE or standalone DM system.
- Existing channel permissions and whisper/net visibility remain mandatory; guest history
  boundaries are an additional restriction, not a replacement.
- REST first; schema-first wire definitions and CLI/TUI parity remain required.
- Guest restriction work touches the approval path. Implementation PRs changing approval
  creation/decision authorization carry `approval-path`, exercise request → notify →
  resolve → audit, and are merged by Nick personally.
- Signup, password login/reset, TOTP, guest lifecycle, history filtering and device links
  require security review. The earlier passkey-design review is not approval of this revision;
  this revision's own review (#703) and its adopted fixes are recorded in the design's §11.
- Guests never hold approval rights: the store forces `can_decide = 0` on a guest's
  memberships and the deciding transaction re-checks the guest flag.
- Changes are scoped to onboarding and this design. The broader roadmap restructuring is
  still a separate discussion; no deployment or release is authorized by this ADR.
