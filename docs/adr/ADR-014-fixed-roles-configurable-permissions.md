# ADR-014: Fixed roles with configurable permissions

- **Status:** Proposed; accepted only when Nick merges the PR that adds it. Implementation issues stay blocked until then.
- **Date:** 2026-10-30
- **Decider:** Nick (Tier-H)
- **Design:** [docs/design/admin-roles.md](../design/admin-roles.md)
- **Amends:**
  - [ADR-010](ADR-010-delegated-administration.md):
    - **D2:** an operator's exclusions from server-wide policy and from the audit export become two operator permissions, off by default. The owner-protection rule and the role route stay owner-only.
    - **D7:** the precedence order stands, and it now also bounds a permissions document: a member can never hold a permission that an operator lacks.
  - [ADR-013](ADR-013-one-community-model.md):
    - **Decisions 2, 4 and 5:** member channel creation, voice and member links are the defaults of named member permissions, not fixed rights.
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

## Decision

1. **The roles are fixed.**
   - Server level: owner, operator, member.
   - Per channel: channel manager.
   - They are stored as ADR-010 stores them: `role` is `operator` or `member`, plus the owner mark and `manages`.
   - No custom roles and no new value in `principals.role`.
   - Agents are a principal kind. They hold no role and no permission; their rights stay their manifest's capabilities and channel grants (ADR-000 D10).

2. **There is a closed catalog of ten permissions**, each with a member cell and an operator cell:
   - `create_public_channels`
   - `create_private_channels`
   - `make_member_links`
   - `see_member_directory`
   - `use_voice`
   - `share_screen`
   - `manage_members`
   - `manage_channels`
   - `change_server_settings`
   - `read_audit_log`

   The defaults reproduce what a server does today and what ADR-013 decided:
   - every operator holds all but the last two;
   - members hold the first six.

   Owners hold every permission and have no column.

   Fixed cells:
   - members can never hold the four operator-only permissions;
   - operators always see the member directory.

   A permission added later must default to the behaviour from before it existed. Everything not in the catalog is a fixed right, listed in the design §4.2:
   - posting and reacting where one is a member;
   - joining public channels;
   - the creator managing what they create;
   - the channel manager's rights;
   - moderation;
   - one's own account;
   - reports.

3. **Who configures:**
   - The owner sets both columns.
   - An operator holding `change_server_settings` sets the member column only.
   - On every write, the store checks two rules. Members never exceed operators. `share_screen` needs `use_voice`.
   - Grants never exceed the granter (ADR-010 D4), and no setting can lock an owner out.

4. **Per-person switches only take away.** A person holds a permission when their role holds it and no brake on them withholds it. The two existing brakes stay and no new one is added:
   - `voice_allowed` withholds `use_voice` and `share_screen`;
   - `may_invite` withholds `make_member_links`.

5. **Presets set the member column and nothing else.**
   - "Personal server" turns off channel creation, member links and the directory, and keeps voice and share on.
   - "Group server" equals the defaults.
   - A preset is applied once. It is not a stored mode; the settings answer reports which preset the column matches.
   - When the directory is off and a default channel exists, or member links are off and open sign-up is on, the answer warns. It does not refuse.

6. **Enforcement is in the server, through one helper.**
   - `can(principal, permission)`, and `permittedTx` inside every gated write, re-checked under the write lock (ADR-010 D7).
   - The helper reads the document per request.
   - One new route class, `accessPermission`, names the route's permission. Today's 35 operator routes map onto `manage_members`, `manage_channels` or fixed operator rights. `PUT /v1/join/settings` maps onto `change_server_settings`, and `GET /v1/audit` onto `read_audit_log`. The role route stays `accessOwner`.
   - Member permissions are checked in their handlers.

   A refusal is 403 `permission_denied`, with the permission named. Existing codes keep answering where they already apply: `forbidden` for a member on an operator route, `voice_not_allowed`, and `invite_not_allowed`.

   The isolation check gains a restricted-operator prober and a personal-preset phase. A coverage test requires a refusal row for every permission-gated route.

7. **The wire is additive; no version bump.** Additions:
   - new closed vocabularies, which clients tolerate when they hold a value they do not know: `Permission`, `PermissionRole`, `PermissionPreset`, `PermissionsWarning`;
   - `GET /v1/permissions` (any operator) and `PUT /v1/permissions`. The PUT is a patch of cells or a preset, carries a revision, and is in the recent-authentication class;
   - `whoami.permissions`, the caller's effective list;
   - an optional `permission` field on `schema.Error`;
   - the audit rows `permissions_changed` and `permissions_initialized`;
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

- **ADR-013's unbuilt shapes shrink.** The directory-settings shapes and routes of #978 and #988 are dropped before publication. #992 is superseded by three console issues, and #988, #984, #985, #981, #982, #983 and #987 each gain one line. The design's §11 and §13 list them for the designer amending ADR-013.
- **Ordering is a safety requirement.** The permission core must be released no later than the member directory (#983). Otherwise Nick's personal server would show its members to each other before he can turn that off.
- **New security surface:** the permission class, the remapping of every operator route, the permissions PUT, and the role-level live effects (voice eviction, share stop, link suspension). Every implementing PR gets a `security-reviewer` pass.
- **Approval path.** The PR that remaps the routes changes who may set `can_decide`. It is labelled `approval-path`, ships the full end-to-end approval test, and is merged by Nick.
- **What fixed roles cost.** "This one member may also create channels" has no answer short of making them an operator. That is accepted: the matrix stays the truth, and every role keeps ADR-010's protections.
- **A gap for the personal server**, raised as an open point (O5): a member link lands in the default channels, of which a personal server has none. The design recommends that an operator's link may name its landing channels, a field on `CreateInviteRequestV2` before it is published.
- **Guests (later, #1004)** fit as a third column. The vocabulary gains a value and rows gain an optional cell, with most guest cells fixed off. No existing shape changes.
- **Open points O1–O12** are in the design §14, each with a recommendation; this ADR takes the recommended reading of each.

## Alternatives considered

| Option | Why not |
| --- | --- |
| Custom roles (Discord-style: named roles, any set of permissions, many roles per person) | Each new role would reopen ADR-010's owner protection, last-owner guard and who-may-touch-whom. Both of Nick's servers fit three fixed roles and ten switches. |
| Keep ADR-013's fixed rights and add one setting per need (`hide_members`, then `members_create_channels`, then …) | That makes one route, one audit action and one shape per switch, with no common rule for who sets them or how they combine with roles. The catalog is the same idea, made once. |
| Person-level grants (inherit, allow, deny per person) | The matrix would become a default rather than the truth. It needs a column per permission and a rule for granting above the role. Brakes already cover the two per-person needs that exist. |
| A preset as a stored server mode | A mode needs rules for "customised" and a migration whenever a preset changes. Applying values once, and reporting which preset they match, is simpler and says the same. |
| Store each column as a whole set and PUT it whole | A client from before a later permission would clear that permission on every save. A patch of named cells fails safe. |
