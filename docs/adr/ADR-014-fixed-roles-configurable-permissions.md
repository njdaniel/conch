# ADR-014: Fixed roles with configurable permissions

- **Status:** Proposed; accepted only when Nick merges the PR that adds it. Implementation issues stay blocked until then. Revised 2026-10-30 after the security review of PR #1008 (design §16).
- **Date:** 2026-10-30
- **Decider:** Nick (Tier-H)
- **Design:** [docs/design/admin-roles.md](../design/admin-roles.md)
- **Amends:**
  - [ADR-010](ADR-010-delegated-administration.md):
    - **D2.** An operator's rights become permissions of the operator role, all on by default except two:
      - administering people is `manage_members`;
      - speaking as people (credentials, sign-in links and hooks for someone else) is `act_as_members`;
      - administering channels is `manage_channels`;
      - D2's two exclusions, server-wide policy and the audit export, become `change_server_settings` and `read_audit_log`, off by default.

      Owner protection and the role route stay owner-only, unchanged.
    - **D7.** The precedence order is amended, not kept:
      - owner ⊇ operator ⊇ member still holds for the permissions document, and every write checks it;
      - **operator ⊇ manager now holds only for an operator holding `manage_channels`**. An operator without it has manager standing in a channel where it holds `manages`, and member standing elsewhere, in the route class, the handlers and the store alike;
      - D7's "every check in the handler and again in the store" now covers every store function that gives an operator standing.
  - [ADR-013](ADR-013-one-community-model.md):
    - **Decisions 2, 4 and 5:** member channel creation, voice and member links are the defaults of named member permissions, not fixed rights. Every "operator" in link creation and redemption becomes a permission.
    - **Decision 9:** the `hide_members` setting is replaced by the member permission `see_member_directory`.
    - **O1 and O5:** "owner" and "operators" there become the permissions that hold those rights.
    - The predicate, the privacy rules and the rest of ADR-013 stand.

  Accepted ADRs are not edited; this one records what changes. Nothing in [ADR-000](ADR-000-charter.md)'s locked table moves:
  - single tenancy;
  - agent identity by manifest and capabilities (D10);
  - API parity (D6);
  - the deployment invariant.

## Context

On 2026-10-30, Nick asked for "settings for the admin to be able to control that, members roles, server settings", and said: "for admin lets have fixed roles with configurable permissions." He plans two servers:

- **alpha.gankbears.com**, a personal server: separate private conversations with different people, who should not see each other.
- **chat.gankbears.com**, later, a group server.

ADR-013 gives every member the same fixed rights: create channels, make member links, see the member directory, use voice. Only one optional setting, `hide_members`, narrows any of them. That fits the group server. On the personal server, a member could create channels, bring in strangers, or see everyone once the directory ships.

ADR-010's rule is that openings and policy are the owner's, but it gives the owner no switch for any of these.

None of ADR-013 is built yet (#978–#998 are open). The rule can therefore be built in rather than retrofitted. Guests are removed for now (#1005), so they are outside this decision.

The security review of the first draft found two things on main that any restriction of operators has to fix first. Both decide by role:

- the store's standing functions, and link creation and redemption, grant operator standing by role, so a restricted operator would keep it wherever it manages a channel or makes a link;
- an operator can already become any non-owner, through a credential, a sign-in link or a hook.

## Decision

1. **The roles are fixed.**
   - Server level: owner, operator, member.
   - Per channel: channel manager.
   - They are stored as ADR-010 stores them: `role` is `operator` or `member`, plus the owner mark and `manages`.
   - No custom roles and no new value in `principals.role`.
   - Agents are a principal kind. They hold no role and no permission; their rights stay their manifest's capabilities and channel grants (ADR-000 D10).

2. **There is a closed catalog of eleven permissions**, each with a member cell and an operator cell:
   - `create_public_channels`
   - `create_private_channels`
   - `make_member_links`
   - `see_member_directory`
   - `use_voice`
   - `share_screen`
   - `manage_members`
   - `act_as_members`
   - `manage_channels`
   - `change_server_settings`
   - `read_audit_log`

   `act_as_members` means "can sign in as, and post as, any member who isn't an owner", and the console and the CLI say so in those words.

   `read_audit_log` reveals every member and every private channel's name, so a non-owner needs recent authentication to use it.

   The defaults reproduce what a server does today and what ADR-013 decided:
   - every operator holds all but the last two;
   - members hold the first six.

   Owners hold every permission, skip brakes, and have no column.

   Fixed cells are decided in code, never read from storage:
   - members can never hold the five operator-only permissions;
   - operators always see the member directory.

   Two permissions depend on others:
   - `share_screen` needs `use_voice`;
   - `act_as_members` needs `manage_members`.

   A permission added later must default to the behaviour from before it existed. Everything not in the catalog is a fixed right, listed in the design §4.2:
   - posting and reacting where one is a member;
   - joining public channels;
   - the creator managing what they create;
   - the channel manager's rights;
   - moderation;
   - one's own account;
   - reports;
   - target protection;
   - sign-in strength.

3. **Who configures:**
   - The owner sets both columns.
   - An operator holding `change_server_settings` sets the member column only. It may turn a member cell on only if it holds that permission itself, its own brakes included.
   - On every write, the store checks, inside the PUT's transaction, that members never exceed operators, the dependencies, the fixed cells and the writer rule.
   - Grants never exceed the granter (ADR-010 D4), and no setting can lock an owner out.

4. **Per-person switches only take away.** A person holds a permission when their role holds it and no brake on them withholds it. The two existing brakes stay and no new one is added:
   - `voice_allowed` withholds `use_voice` and `share_screen`;
   - `may_invite` withholds `make_member_links`.

5. **Presets set the member column and nothing else.**
   - "Personal server" turns off channel creation, member links and the directory, and keeps voice and share on.
   - "Group server" equals the defaults.
   - A preset is applied once. It is not a stored mode; the settings answer reports which preset the column matches.
   - When the directory is off and a default channel exists, or member links are off and open sign-up is on, the answer warns. It does not refuse.

6. **Enforcement is in the server, through one helper, for every check on an actor's authority.**
   - `can(principal, permission)` in handlers, and `permittedTx` inside every gated write, re-checked under the write lock.
   - This covers the store's own standing decisions:
     - the channel-manager standing functions;
     - link creation and redemption (channels beyond the maker's own need `manage_channels`; open links and `may_invite` grants need `manage_members`);
     - `open_channels`;
     - sign-in links (`act_as_members`, re-checked at redemption);
     - closing another's invite.
   - Checks on a *target's* role (who is protected) and on sign-in strength stay keyed on role, as listed exceptions. A structural test over handlers and store holds that list.
   - One new route class, `accessPermission`, names the route's permission. Today's 35 operator routes split into 21 under `manage_members`, 4 under `act_as_members`, 5 under `manage_channels`, and 5 that stay open to every operator.
   - `PUT /v1/join/settings` needs `change_server_settings`, and `GET /v1/audit` needs `read_audit_log`. The role route stays `accessOwner`.
   - Operator branches inside authenticated routes that today reveal private channels or other people's invites and sessions are mapped onto `manage_channels` or `manage_members`, and otherwise give the uniform 404.

   A refusal is 403 `permission_denied`, with the permission named. Where an existing code already answers the question, it is used whichever check refused, still with the permission named:
   - `forbidden` for a member on an operator route;
   - `voice_not_allowed`;
   - `invite_not_allowed`.

   The isolation check gains a restricted-operator prober, including one that manages a channel of its own, and a personal-preset phase. A coverage test requires a refusal row for every permission-gated route.

7. **The wire is additive; no version bump.** Additions:
   - new closed vocabularies, which clients tolerate when they hold a value they do not know: `Permission`, `PermissionRole`, `PermissionPreset`, `PermissionsWarning`;
   - `GET /v1/permissions` (any operator) and `PUT /v1/permissions`. The PUT is a patch of cells or a preset, carries a revision, and is in the recent-authentication class;
   - `whoami.permissions`, the caller's effective list;
   - an optional `permission` field on `schema.Error`;
   - the audit rows `permissions_changed` and `permissions_initialized`, plus suspension rows;
   - a content-free events document, `conch.permissions_changed.v1`.

   No MCP tool is added: administration is a human act, and parity holds through REST.

8. **Existing servers keep their behaviour.** One migration creates an empty `role_permissions` table, where an absent row reads as the default. If a `hide_members` row exists, the migration maps it onto `see_member_directory`. It writes one audit row.

9. **The admin console** is one Settings area for operators, with four sections:
   - Server;
   - Roles and permissions, with the presets and the matrix;
   - Members, including #972's menu;
   - Channels.

   Each control maps onto a REST route, and `conch admin permissions show|set|preset` gives the CLI the same reach. It works in the phone layout.

## Consequences

- **ADR-013's unbuilt shapes shrink.** The directory-settings shapes and routes of #978 and #988 are dropped before publication. #992 is superseded by three console issues. The community issues #981, #982, #983, #984, #985, #986, #987 and #988 each gain a line. The design's §11 and §13 list them for the designer amending ADR-013.
- **Ordering is a safety requirement, twice over:**
  - The store's authority fix (design issue A3a) must merge before the route remap and the permissions PUT (A3b). The remap alone would let a restricted operator keep operator standing in a channel it manages.
  - A3b must be released no later than the member directory (#983). Otherwise Nick's personal server would show its members to each other before he can turn that off.
- **New security surface:** the permission class; the remapping of every operator route and of the operator branches in handlers and the store; the permissions PUT; the role-level live effects (voice eviction, share stop, link and sign-in-link suspension). Every implementing PR gets a `security-reviewer` pass.
- **Approval path.** A3a and A3b change who may set `can_decide` and who writes memberships. Both are labelled `approval-path` and ship the full end-to-end approval test, including a restricted operator-manager refused `can_decide` and an agent add. Nick merges both.
- **A limit is recorded (O13):**
  - credentials and hooks an operator issued stay live after `act_as_members` is taken away, because no table records who issued them;
  - unredeemed links are suspended;
  - the owner ends credentials and hooks with sign-out-everywhere or a hook revoke.
- **Owners skip brakes.** A brake stored on an owner stops counting. The CHANGELOG says so.
- **What fixed roles cost.** "This one member may also create channels" has no answer short of making them an operator. That is accepted: the matrix stays the truth, and every role keeps ADR-010's protections.
- **A gap for the personal server**, raised as an open point (O5): a member link lands in the default channels, of which a personal server has none. The design recommends that a link made by a holder of `manage_channels` may name its landing channels, checked again at redemption, without `can_decide` or `manages`. That is a field on `CreateInviteRequestV2`, which must be added before it is published.
- **Guests (later, #1004)** fit as a third column. The vocabulary gains a value and rows gain an optional cell, with most guest cells fixed off. No existing shape changes.
- **Open points O1–O13** are in the design §14, each with a recommendation; this ADR takes the recommended reading of each.

## Alternatives considered

| Option | Why not |
| --- | --- |
| Custom roles (Discord-style: named roles, any set of permissions, many roles per person) | Each new role would reopen ADR-010's owner protection, last-owner guard and who-may-touch-whom. Both of Nick's servers fit three fixed roles and eleven switches. |
| Keep ADR-013's fixed rights and add one setting per need (`hide_members`, then `members_create_channels`, then …) | That makes one route, one audit action and one shape per switch, with no common rule for who sets them or how they combine with roles. The catalog is the same idea, made once. |
| Person-level grants (inherit, allow, deny per person) | The matrix would become a default rather than the truth. It needs a column per permission and a rule for granting above the role. Brakes already cover the two per-person needs that exist. |
| A preset as a stored server mode | A mode needs rules for "customised" and a migration whenever a preset changes. Applying values once, and reporting which preset they match, is simpler and says the same. |
| Store each column as a whole set and PUT it whole | A client from before a later permission would clear that permission on every save. A patch of named cells fails safe. |
| Keep credential, sign-in link and hook issuing inside `manage_members` | Those let the holder become someone, which is a different power from administering a roster. Folding them in would hide that power behind a name that does not say it. |
| Restrict operators at the route class only | The handlers and the store decide standing by role. A restricted operator would still get operator standing wherever it manages a channel or makes a link. |
