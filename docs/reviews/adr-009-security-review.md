# ADR-009 security review (issue #703)

- **Reviewed:** commit af0c3bc on PR #701 (the owner's passwords-first, temporary-guest
  revision): ADR-009, docs/design/guests-and-accounts.md, and the touched lines of ADR-000,
  ADR-008, docs/design/private-alpha.md and ROADMAP.md.
- **Reviewer:** security-reviewer agent (Claude Fable 5.1), read-only, against main at the
  time. Resolutions by the principal on the owner's delegation. A re-check of the amended
  text passed, with four small corrections, all applied: the per-address cap is a setting
  with a proxy warning, label separator and `guest` prefix refused, the label is its own
  wire field, and `guest_started` does not audit the address.
- **Date:** 2026-10-21. This is a design review. No code exists yet, and nothing here is a
  runtime verification.
- **Verdict on af0c3bc:** BLOCK on two must-fixes. **After the amendments below:** the design
  is ready for the owner's acceptance. Each implementation PR still gets its own review.

The passkey-first draft's review (PASS) does not carry over; it is kept in Git history only.

## Findings and resolutions

| ID | Severity | Finding | Resolution (design section) |
| --- | --- | --- | --- |
| F1 | must-fix, approval-path | Guest `can_decide` was intent only. `PutChannelMember` turns it on when omitted (`store/members.go`), quorum counts any `can_decide = 1` member (`store/approvals.go`), and conversion would carry it into the member. | The store forces 0 on every guest membership write and refuses `true`; `DecideAsMember` re-checks the guest flag; conversion leaves it unchanged (§4.3, §9 test 10, ADR invariant). |
| F2 | must-fix | Second factor guessable across challenges: five tries per challenge, wrong codes outside the account window, reset point unspecified. | Wrong code, wrong re-auth and wrong password are one account failure in the name's window; only credential issue resets it; challenge issuance bounded (§5.3, §8). |
| F3 | should-fix | One hashing pool lets an anonymous flood block operator re-auth and conversion. | Two pools, anonymous and authenticated (§5.1). |
| F4 | should-fix | "Escalating delays" invites a sleep and a timing oracle on name existence. | Per-name window keyed by the typed name whether or not it exists; immediate 429, no sleep (§5.2, §8). |
| F5 | should-fix | Background keepalives defeat the idle bound; a stolen tab token lives 24 h. `CredentialLive` checks no idle rule today (`store/resolve.go`). | Idle refreshed only by REST requests; `CredentialLive` applies it, so sockets, tickets and media inherit it (§4.2). |
| F6 | should-fix | Setup token scope not centrally enforced. | Spent on redeem; 15-minute `scope = setup` credential checked in `authMiddleware` (§5.3). |
| F7 | should-fix | Online operator enrolment on an ordinary bearer not explicitly refused. | `setup_on_host` refusal (§5.2). |
| F8 | should-fix | Re-auth class was an "initial catalog". | Defined by rule; route-table test fails on unclassified operator routes (§5.3). |
| F9 | should-fix | No cap on successful sign-ins per name. | 30 per name key per hour (§5.2, §8). |
| F10 | nit | Hide is a rename (`store/hide.go`) and would rewrite guest attribution. | Guests are not hideable (§4.4). |
| F11 | nit | `humansOnlyFiles` admits guests. | Explicit upload refusal; watermark through `FileVisibleTo`; MCP already agents-only (§4.3). |
| F12 | nit | Turning guests off leaves visits for up to 24 h silently. | Response and audit carry the live count and name End all guest visits (§2). |
| F13 | nit | Guest churn defeats per-visit levers; few addresses can fill `max_guests`. | Levers named; `max_guests_per_address` (default five) with a warning when a trusted proxy is unset; `guest_started` audits the entry point, not the address (§8). |
| F14 | nit | Pending challenge not bound to client address. | Bound to address (§5.3). |

## Earlier findings re-checked

The principal's unadopted alternative revision (branch `issue-694/principal-rev2`) had been
reviewed twice. Each of its findings against af0c3bc: **a** already handled, **b** still
needed (now fixed above), **c** no longer applies.

| Label | Status | Note |
| --- | --- | --- |
| M1 guest `can_decide` | b → F1 | |
| M2 Keep as a door around a closed door | a | Conversion follows the recorded origin's account policy (§3.3). |
| M3 TOTP/re-auth failures count | b → F2 | |
| M4 per-name window, no sleep | b → F4 | |
| S1 separate login limits | a | §8 |
| S2 name-conflict probes | a | §8 |
| S3 name release | c | Generated names are never reused. |
| S4 recheck in the issuing transaction | a | §5.2 |
| S5 approval membership check | a | Mechanism lands in G2 (`approvals_handlers.go` checks membership only today). |
| S6, S7 device-link pages | a | §6.2 |
| S8 stolen bearer cannot change password | a | §5.2 |
| S9 media entitlement on expiry | a | §4.2; G2 adds the credential check to voice entitlement. |
| S10 challenge binding | a, plus F14 | |
| S11 setup scope | b → F6 | |
| S12 enrolment only on host | b → F7 | |
| S13 re-auth class by rule | b → F8 | |
| S14 two hashing pools | b → F3 | |
| S15 script-token exemption | a | §5.3 keeps token credentials outside browser MFA. |
| S16 idle in `CredentialLive` | b → F5 | |
| R1 silent guests hidden | c | No name reservation to release. |
| N1–N4 | a | |
| N5 | c | Passkey-only. |
| 30 successes per name per hour | b → F9 | |

## Claims checked against main

Invite limiter 10 per 10 min per address and 20 per minute server-wide
(`invite_limits.go`); member once-a-day renewal not reusable for guests (`store/resolve.go`);
operator sign-in links refuse operator targets (`store/sign_in_links.go`); operator
`sessionStorage` and member `localStorage` (`web/src/api.ts`); trusted-proxy client address
(`clientaddr.go`); decider binding (`approvals_handlers.go`). The ADR-000, ADR-008,
private-alpha and roadmap edits keep the alpha boundary (invite-only by default,
`open_joining = off`, `guests_enabled = false`).

## Contract decisions recorded here (#703)

- **Display labels.** #694's owner clarification ("Guest: Nick") is reconciled with the
  generated handle: an optional, non-unique label, always rendered with the `Guest:` prefix
  and a four-character tag from the handle, never an identifier (§4.1). The reserved
  namespace is the folded or confusable-skeleton **prefix** `guest` on permanent usernames,
  not every incidental occurrence of the substring.
- **Guest limits.** The two-hour idle and 24-hour absolute defaults stand; both stay
  configurable up to 24 hours. Per-address cap five, `max_guests` 50.

## Residual risks

- A bearer in browser storage is still exposed to XSS for its lifetime; CSP, no third-party
  scripts and short lifetimes limit it, and password/TOTP proofs guard only credential changes.
- Full-power operator script tokens remain powerful without MFA, by design (§5.3).
- No self-service recovery of a forgotten password in this slice; an operator issues a reset link.
- Loss of the TOTP host key locks operators out until offline recovery; it is backed up
  separately (§5.3).
- A restore can bring back state older than a revocation; #716 owns the restore boundary.
