# ADR-012: The operator's authenticator as its own setting

- **Status:** Proposed; accepted only when Nick merges this PR.
- **Date:** 2026-10-27
- **Decider:** Nick
- **Proposal:** [#875](https://github.com/njdaniel/conch/issues/875)
- **Amends:** [ADR-011](ADR-011-temporary-servers.md) items 1, 2.1, 4 and 5: password-only operator sign-in becomes a setting of its own, which temporary mode turns on, instead of a part of temporary mode alone. Through it, [ADR-009](ADR-009-guests-and-open-joining.md) item 6 and [ADR-010](ADR-010-delegated-administration.md)'s re-authentication, on servers with the setting on.

## Context

ADR-011 lets an operator with no authenticator sign in with a password alone, but only on a temporary server. A temporary server also tells every user it has no backups and may be removed, and it makes off-site backup optional.

The owner wants the first half without the rest, on the long-lived alpha server that family and friend groups use. Asked to choose between this, signing in with the operator token, making alpha a temporary server, or keeping the authenticator and adding a QR code (#862), the owner picked "Password only, as its own setting" (2026-10-27).

## Decision

1. **A setting, fixed when the server starts.** `CONCHD_OPERATOR_TOTP` in the instance's env file:
   - **`required`**: an operator needs a confirmed authenticator to sign in to the browser. This is ADR-009 item 6, and today's behaviour. On a normal server it is the default: an empty or missing value means `required`.
   - **`optional`**: ADR-011 item 2.1 applies. At the same five places, an operator is asked for a code only if they have enrolled one.
   - **Anything else stops `conchd` at start.**

   Like the mode, it is read only from the environment and changes only by editing the env file and restarting. Nothing in the API, an invite or the browser sets or changes it. `conchd` logs the setting at start, in a line of its own, separate from the temporary-mode warning.

2. **Temporary mode turns it on.** Operators may sign in with a password alone when `CONCHD_MODE=temporary` **or** `CONCHD_OPERATOR_TOTP=optional`. Call that *password-only allowed*. With `CONCHD_MODE=temporary`, an empty or missing `CONCHD_OPERATOR_TOTP` means `optional`, so existing temporary instances start as before. Only an explicit `required` is refused at start, so the two settings cannot contradict each other.

3. **What the setting changes, and nothing else:**
   - **ADR-011 item 2.1** applies whenever password-only is allowed: the five places, the setup credential restricted after the password, Skip, enrolment later only with a host token, and recover leaving the operator password-only.
   - **The lock-out guard counts an owner with a password** as able to sign in while password-only is allowed. A password then really does sign them in, so an owner can sign out of their last browser session. The guard goes back to the strict rule (password and authenticator) the moment the stored flag says password-only is not allowed.

   **Every store-side check reads the stored flag.** That covers `passwordOnlyOperatorTx`, `EnrollPassword`'s restriction of the setup credential, and the lock-out guard. Each reads whether password-only is allowed from the `server_mode` row in its own transaction, not from the server's configuration and not from the mode alone. So a server and its store can never disagree in the direction of a weaker check.

   It does **not** change:
   - the banner: `server_mode` stays absent unless the server is temporary, and the setting adds no wire field;
   - backups;
   - anything else in ADR-011 item 4's list.

   A server with `optional` is a normal, long-lived server whose operators sign in with one factor. As with ADR-011's rejected per-operator choice, members cannot tell from the wire that operators use one factor. The owner accepted that for the alpha server (2026-10-27).

4. **Turning it off ends what it made.** The store records whether password-only was allowed when the server last started, in the same `server_mode` row as the mode (ADR-011's migration, extended before it ships). Any change to either value audits `server_mode`, its detail naming the old and new mode and setting, so turning password-only **on** is recorded too. Revocation happens when, and only when, password-only stops being allowed. That amends ADR-011 item 5: a temporary server restarted as normal with `optional` keeps its sessions, because a password still signs operators in. At that point `conchd` applies ADR-011 item 5 in the one transaction that records the change. It:
   - revokes every operator browser session;
   - clears every recent-authentication proof;
   - spends every pending login and every unfinished setup credential;
   - audits `server_mode`, its detail now naming both values and the counts.

   Script credentials keep working, as in ADR-011. Turning password-only on revokes nothing.

   **Owners left without an authenticator.** At the switch to not-allowed, `conchd` logs a warning naming how many enabled owners and operators have a password and no authenticator. The audit detail carries that count. It does not refuse to start: the person changing the setting is on the host, and `conchd operator setup` there re-enrols each of them (`IssueSetupToken` still treats a password-only operator's setup as unfinished). Refusing would stop a host from tightening security.

5. **The browser and the CLI key on setup state, not on the banner.**
   - The setup page's **Skip** and `conch setup`'s "Set up an authenticator now? [y/N]" appear when whoami says, for an operator, that `setup_complete` is true, `totp_enrolled` is false, and `auth.scope` is `setup`. A full session cannot enrol, so it is never offered there. Since `setup_complete` already follows the server's rule (ADR-011 item 2.1), the clients need no new field.
   - The temporary banner still keys on `server_mode`.

## Alternatives

| Choice | Reason |
| --- | --- |
| Make alpha a temporary server | Every user would be told the server has no backups and may be removed, which is false for alpha, and off-site backup would become optional. |
| Sign in with the operator token | It works today, but the token never expires and the browser keeps it per tab: a copied token is full control until revoked. A password session ends after 12 hours, or one hour unused. |
| Keep the authenticator and add a QR code (#862) | The owner's preference is not to be forced to use one at all. #862 remains for operators who choose to enrol. |
| A per-operator choice | Each operator would weaken their own account quietly, and nobody using the server could tell (ADR-011's reason). The setting is per server and set on the host. |

## Consequences

- **Security review:** the draft's review passed, with four wording fixes (empty means `optional` under temporary mode; revocation only when password-only stops; the owner warning; Skip only on a setup session) and three notes (store checks read the stored flag; turning it on is audited; the members-can't-tell trade-off). All are adopted above.
- **Security:** on a server with `optional`, an operator's password alone controls the server, including the private conversations of every group on it. Guessing is bounded by the existing per-name and per-address limits and by Argon2id's cost, but a reused or phished password is enough. The owner accepted this for the alpha server (2026-10-27). The setting is on the host only, so an attacker with the password cannot turn the authenticator requirement off, but also cannot be stopped by it. An operator who wants a second factor can still enrol one, and is then always asked for it.
- **Tests:**
  - the implementation runs ADR-011's tests for both ways of allowing password-only;
  - turning the setting to `required` ends sessions and proofs, as the switch back to normal does;
  - `CONCHD_MODE=temporary` with an explicit `CONCHD_OPERATOR_TOTP=required` stops start, and with it unset starts as optional;
  - temporary → normal with `optional` revokes nothing;
  - the switch to not-allowed logs and audits the count of owners with no authenticator;
  - with `optional` alone, `/healthz` and whoami carry no `server_mode`.
- **Work:**
  - #866 implements both settings before it merges, its migration not yet released;
  - #867 and #868 key Skip on `setup_complete` and `totp_enrolled`;
  - `docs/deploy.md` gets the setting in step 5;
  - `conchd serve`'s help, and the `operator setup` and `bootstrap` stderr text (which today say "and enrol an authenticator app"), describe the setting.
- **The alpha server** gets `CONCHD_OPERATOR_TOTP=optional` in its env file at the `v0.1.0-alpha.2` upgrade.
