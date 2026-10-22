# ADR-010: Delegated administration and channel managers

- **Status:** Proposed; accepted only when the owner merges the PR that adds it. Implementation issues stay blocked until then.
- **Date:** 2026-10-21
- **Decider:** Nick (Tier-H)
- **Proposal:** [#697](https://github.com/njdaniel/conch/issues/697)
- **Design:** [docs/design/delegated-administration.md](../design/delegated-administration.md)
- **Amends:**
  - [ADR-003](ADR-003-multi-human-access.md)'s decision that human principals have "their own local account and session" and that channel membership is the visibility boundary: it adds two standings above a plain member (an owner mark on operators, and a per-channel manager grant) and keeps the boundary.
  - [ADR-008](ADR-008-private-communications-alpha.md) Decision 4, "an invite … never grants operator rights": extended to the owner mark and the manager grant, and to every grant (nobody grants a right they lack).
  - [docs/design/private-alpha.md](../design/private-alpha.md) §3, "No per-group moderators in the alpha. Moderation stays operator-only": superseded by the channel manager.
  - Nothing in [ADR-009](ADR-009-guests-and-open-joining.md): guests hold no role, mark or grant, as item 3 says; operator re-authentication for sensitive changes (item 6) covers the new role route by its rule.

  Accepted ADRs are not edited; this one records what changes.

## Context

One role administers everything. The alpha puts several separate groups on one server, and the person who runs it is the only one who can add a tester to a channel, rename a channel, stop a share, or clean up after an invite. Delegating any of that today means making someone an operator, which hands them the whole server, including the power to disable the person who runs it (the lock-out guard keeps one operator able to sign in, not a particular one).

Facts on main (900b43a): `principals.role` is `operator` or `member` and no route changes it; 33 routes are operator-only by route class and 3 more are operator-or-self; three narrower grants exist (`may_invite`, `voice_allowed` per person; `can_decide` per membership); agents are permitted by manifests, separately; the last-operator guard runs in three store paths.

## Decision

1. **Two server roles stay, plus an owner mark.** `role` keeps its two values. `principals.owner` marks the operator who runs the server; the migration marks every existing operator, and bootstrap makes an owner. A **delegated administrator** is an operator without the mark. Setting the mark revokes the target's live credentials and the hooks bound to them, so no token another operator issued becomes owner-grade.
2. **Only an owner changes roles or the mark** (`PUT /v1/principals/{id}/role`, owner-only, in the recent-re-authentication class by ADR-009's rule). An operator keeps every operator right except: it cannot act on an owner's account, credentials, sign-in links, hooks, channel and net memberships, or `can_decide`; cannot export the audit trail; and cannot change server-wide policy (ADR-009's joining settings when they exist). Brakes (invite pause, ending all guest visits) stay operator-level.
3. **A channel manager is a per-membership grant** (`channel_members.manages`), set only by operators. In its channel a manager adds people who already share a channel with it, removes members who are not operators, owners or agents and hold no `can_decide` (so it cannot change an approval's quorum), renames it, stops a share, and does what the moderation design (#698) assigns to it. It cannot set `can_decide` or `manages`, create channels, or touch nets, agents, credentials, invites beyond `may_invite`, or anything server-wide. A manager finds people only among members it already sees, or by exact name at a bounded, audited rate; any other id or name gets one uniform 404.
4. **Grants never exceed the granter.** Only owners make owners and operators; only operators make managers; managers grant nothing; invites carry neither role, mark nor grant.
5. **The last owner who can sign in is never locked out.** The last-operator guard becomes a last-owner guard on the same store paths (disable, hide, revoke-all, single revoke) and on the role change; an owner demotes themselves only if another owner who can sign in remains. Demotion ends nothing else but the invites and sign-in links that were rights of the role.
6. **Agents and guests stay outside.** No agent is an owner, operator or manager; no guest holds a role, mark or grant; both refused in the store.
7. **Precedence is additive and server-enforced**: owner ⊇ operator ⊇ manager (in its channel) ⊇ member ⊇ guest, deny by default. Two route classes are added (`accessOwner`, `accessChannelManager`); every other check is in the handler and re-checked in the store's write transaction. A non-member of a channel is answered as if the channel did not exist, on the manager class as on every content route.
8. **The browser hides and explains; it enforces nothing.** `whoami` says `owner`; the channel list says `manages`; the member list shows `manages` to members and `role` to those who may act on it. All additive; no schema version bump.

## Consequences

- Two migrations (one if landed together): `principals.owner`, `channel_members.manages`. No audit rows, so the pinned trails do not move.
- New security surface: the role route, the manager class, owner protection on seventeen routes and the hook list, the by-name add. Every implementation PR gets a `security-reviewer` pass; the membership-write transaction also writes `can_decide`, so that PR is **approval-path** and merged by the owner.
- Three isolation rows change from 403 to 404 (the two membership routes and #696's rename), the stronger answer.
- The `last_operator` error code keeps its name with a stricter meaning; a rename to `last_owner` is the owner's call.
- The role model is now three documents: ADR-003 (membership), ADR-008 §4 (invites), this ADR (administration). #698 (moderation) and the ADR-009 implementation issues build on it and must not add a fourth standing without amending this ADR.
- Unchanged: the deployment invariant, single tenancy, no MCP tool for administration (parity: the browser and CLI use the same REST routes), the approval right (`can_decide` alone), agent manifests.

## Alternatives considered

| Option | Why not |
| --- | --- |
| A third role value (`admin`) in `principals.role` | A CHECK-constraint table rebuild, a new value in a published enum that every client switches on, and the same questions about who may touch whom. The mark answers them with one column and no new wire value. |
| A server-wide `moderator` role | Moderation is per group on a shared server; a server-wide role would see every group. The grant is on the membership, where the boundary already is. |
| Channel creators own their channels | Creation is operator-only and stays so (#695); an "owner of a channel" would be a second ownership concept with its own lock-out rules. The manager grant is explicit and revocable instead. |
| Leave moderation operator-only through the alpha (private-alpha.md §3) | Thirty people in several groups on one server cannot all wait on the person who runs it; and every delegate would have to be a full operator, which this ADR exists to avoid. |
