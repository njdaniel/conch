# ADR-011: Temporary servers

- **Status:** Proposed; accepted only when Nick merges this PR.
- **Date:** 2026-10-27
- **Decider:** Nick
- **Proposal:** [#863](https://github.com/njdaniel/conch/issues/863)
- **Amends:** [ADR-009](ADR-009-guests-and-open-joining.md) item 6, for servers in temporary mode only: an operator signs in to the browser with a password alone. [ADR-010](ADR-010-delegated-administration.md)'s re-authentication for the role route and the owner mark, by the same rule. [docs/deploy.md](../deploy.md)'s rule that an off-site backup is not optional, for those servers only.

## Context

The alpha runs on one droplet, and several Conch servers can share it, one per instance (ADR-008, plan §3). The owner wants to run many **short-lived** servers there, for tests, one-off groups or trying a build. The long-lived `alpha` server, which family and friend groups will use, stays alongside them.

The first deployment (#567, 2026-10-27) showed what each server costs today:

- **An operator account in two factors.** The owner runs a host command for a setup token, redeems it, chooses a password, then types a 32-character authenticator key into a phone app (ADR-009 item 6; no QR code yet, #862). Each server is a separate account and a separate authenticator entry.
- **Backups.** A versioned Space, two keys, two encryption passwords in a password manager, a nightly timer that fails loudly without an off-site copy, and a restore drill before anyone joins.
- **Seven manual steps** to add the server (#861).

That is right for a server that holds people's private conversations for months. For a server that lives a few days and holds nothing anyone needs to keep, it is most of the work and none of the value. The owner's words: "quite tedious for an alpha deployment especially since i want to deploy many and temporary".

## Decision

1. **A server-wide mode, fixed when the server starts.** `CONCHD_MODE=temporary` in the instance's env file puts a server in temporary mode. The accepted values are exactly `temporary` and `normal`, lower case. An empty or missing value is the normal mode, so every existing server is unchanged, and any other value stops `conchd` at start. The mode is never set or changed through the API, an invite or the browser. It changes only by editing the env file on the host and restarting. `conchd` keeps the mode it last started in as a row in the store (not by reading audit rows back). It logs the mode at start, and audits `server_mode` when the mode differs from that row (item 5). On the very first start there is no row yet: `conchd` writes it and audits `server_mode` once, so every server's audit trail says which mode it began in.

2. **What temporary mode changes:**
   1. **An operator's second factor is asked for only when one is enrolled.** These five places read whether an operator has an authenticator. In temporary mode each of them asks for a code if one is enrolled, and goes on without one otherwise:
      - **browser sign-in** (`internal/server/login.go`, the operator branch): a password-only operator is signed in without a TOTP challenge;
      - **recent re-authentication** for sensitive actions (`internal/server/password_recovery.go`), including ADR-010's role route and the owner mark, so in temporary mode the owner too proves with a password alone;
      - **password change** (`password_recovery.go` and the store's `ChangePassword` second-factor check);
      - **setup complete** in `whoami.auth.setup_complete` (`password_recovery.go`), which the web Setup page and `conch setup` wait on: password set, and authenticator enrolled only if the mode requires it;
      - **the setup credential's end** (the store's `EnrollPassword`, `internal/server/store/passwords.go`): in temporary mode, setting the password completes setup. In the same transaction the setup credential loses the right to set a password: it keeps only the authenticator routes (`POST /v1/totp/enroll`, `POST /v1/totp/confirm`) and its own end. It ends on **Skip** (`DELETE /v1/credentials/current`), on a confirmed enrolment, or at its 15-minute expiry, whichever comes first. So nothing left behind can replace the password, and the authenticator step stays on offer.

      Two store checks also read whether an operator has an authenticator, and are deliberately unchanged in temporary mode. The **lock-out guard** (`internal/server/store/disable.go`) keeps counting only owners with a password and an authenticator as able to sign in: it is stricter than temporary mode needs, so it fails safe. **`IssueSetupToken`** (`internal/server/store/operator_auth.go`) still treats only password and authenticator together as a finished setup, which is why a password-only operator can always get a new setup token. Nothing else reads it. Enrolling later needs a new setup token from the host (`conchd operator setup`): a full session cannot enrol. An operator who has enrolled is asked for the code everywhere above, so the mode never undoes an enrolment. `conchd operator recover` removes the authenticator as it does today, so a recovered operator is password-only afterwards in temporary mode. A temporary instance still gets a `CONCHD_TOTP_KEY`, or the offered step would fail. Without the key, an operator who has enrolled cannot sign in at all, in either mode: failing closed is intended (`checkTOTPKey`). Session lifetimes are unchanged: 12 hours absolute, one hour idle.
   2. **Off-site backup is optional.** The nightly backup timer is not enabled for a temporary instance by default. When it is, the backup unit, which starts as root, reads `CONCHD_MODE` from the instance's env file (`conch-backup` itself cannot read that file) and passes `--local-only`, so the existing local-only path runs and exits 0. No new warning path is added, and a normal instance keeps failing loudly, with exit 3, when it has no off-site copy. The upgrade script's pre-upgrade backup, kept on the droplet, is unchanged, so a rollback still works. No restore drill is required before testers join.
   3. **Everyone is told.** `GET /healthz` and `GET /v1/whoami` carry an optional `"server_mode": "temporary"` field, absent in normal mode: an additive schema change, no version bump. The field is deliberately public. The sign-in page reads it from `/healthz` before anyone signs in, as it already reads the build version there. The web client shows a fixed line on the sign-in page and in the lobby: "Temporary server: no backups, and it may be removed. Don't keep anything here you need." `conch whoami` prints the same.
   4. **One-command lifecycle** (#861):
      - **`add-instance.sh <name> <domain> --temporary`** writes `CONCHD_MODE=temporary` and a `CONCHD_TOTP_KEY`, skips the backup timer, and prints a one-time setup link (item 3) for the first operator. The link goes only to the invoking terminal, never to a log or a file.
      - **`remove-instance.sh <name>`** stops and deletes the instance: its unit, user, data, env file, LiveKit key and Caddy block. Its guardrails:
        - It validates `<name>` against the same character rule as `add-instance.sh` before anything else, and refuses a symlinked data directory or env file.
        - It reads the mode from the instance's env file only. A missing or unreadable file counts as normal. It refuses a normal instance unless `--force-normal` is given. It asks for the name to be typed back in every case. There is no `--yes` for removal.
        - It stops and disables the unit and the timer, then confirms no `conchd` for that instance is still running before deleting anything.
        - It takes a final local archive (`conch-backup.sh <name> --local-only`) unless `--no-backup` is given. That archive and the instance's pre-upgrade archives stay in `/var/backups/conch/<name>` for 14 days. Once the instance's timer is gone, a host-level `systemd-tmpfiles` rule that `add-instance.sh` installs (`/etc/tmpfiles.d/conch-removed.conf`, age 14 days, on `/var/backups/conch/removed/`) deletes them; removal moves the directory there. `--purge-backups` deletes them at once.
        - It edits the Caddyfile and LiveKit's key file, validates both before reloading, and rolls back on failure, so no other instance is taken down.
        - `userdel` touches only `conch-<name>`, never the shared `conch-backup` or `conch-drill` users.
        - It prints the DNS records to delete.

3. **A setup link for every server, not only temporary ones.** `conchd operator setup --link` (and `operator bootstrap --link`) prints `https://<instance>/app/#/setup/<token>` instead of the bare token. It is opt-in, printed once, and not offered for `conchd operator recover`. The page takes the token from the address and clears it at once, exactly as invite and sign-in links are handled today (`takeLinkFromAddress` in `web/src/route.ts`), and the hygiene test will cover `#/setup/` beside `#/invite/` and `#/signin/`. **It redeems only on a click, never on load**, so a link previewer or anything else that opens the page and runs its scripts cannot spend the token. Its limits are unchanged: one use, ten minutes, HTTPS only. A setup token can replace an operator's existing password, and in temporary mode yields a complete operator, so the only new exposure, the browser's history and the navigation-timing entry that `route.ts` already accepts for invites, is bounded by those ten minutes and the single use.

4. **What temporary mode never changes.** Guest policy and its switches, invites and their expiry, channel isolation and guest history limits, approvals and `can_decide`, rate limits on sign-in and invites, Argon2id password storage and the password rules, script credentials, the operator lock-out guard, delegated administration (ADR-010) other than its re-authentication as item 2.1 says, and every audit row. Members and guests sign in exactly as they do on a normal server. Only the operator's second factor and the backup requirement move.

5. **Switching back to normal ends what temporary mode made.** At start, when the mode in the env file is normal and the stored mode is temporary, `conchd` does all of this in the one transaction that records the new mode:
   - it revokes every browser session of every operator, whether or not they have since enrolled an authenticator, since any of them may have signed in with a password alone (signing in again is cheap on a switch that happens rarely);
   - it clears every recent-authentication proof;
   - it spends every pending login and every unfinished setup credential;
   - it audits `server_mode` with the counts.

   This is one-directional: going from normal to temporary weakens nothing already stored, so nothing is revoked then. It does not bump auth generations wholesale, because those cover script credentials too, which keep working in both modes. From then on, an operator without an enrolled authenticator cannot sign in to the browser with a password until they finish setup with a token from the host (`conchd operator setup`), which is today's rule for an operator whose setup is incomplete. A backup restored anywhere is safe whatever mode it came from, because the restore already bumps every generation and clears every proof (`InvalidateRestored`, `internal/server/store/restore.go`). It then runs in the mode of the server it is restored into.

## Alternatives

| Choice | Reason |
| --- | --- |
| Make the authenticator optional per operator, on any server | Rejected: it weakens long-lived servers quietly, one operator at a time, and nobody using the server can tell. The mode is per server and shown to everyone. |
| Let the operator token sign in to the browser | Rejected: a script credential has no expiry and no second factor, and the browser would have to store it. ADR-009 keeps script credentials out of browser sessions. |
| Administer temporary servers from the CLI only, with the token | Works today, but every test of the browser then needs a second, member account. That is more tedious, not less. |
| A QR code alone (#862) | Shortens enrolment but keeps one authenticator entry per throwaway server, and leaves the backups. Still worth doing for normal servers. |
| Skip backups by not setting a destination | Today that fails the nightly unit on purpose, so a missing off-site copy is never silent. Temporary mode keeps that loud failure for normal servers. |

## Consequences

- **Security review:** the draft's review is on the PR. It blocked one point, that switching back to normal left password-only sessions and recent-authentication proofs alive, which item 5 now answers. Its other findings are in items 2.1, 2.3, 2.4 and 3.
- **Security:** on a temporary server, an operator's password alone controls the server. Guessing is bounded by the existing per-name and per-address limits and by Argon2id's cost, but a reused or phished password is enough. That is acceptable only for servers holding nothing sensitive, which is what the banner says. The long-lived `alpha` server stays in normal mode. The implementation gets a `security-reviewer` pass on the mode switch, the login and re-authentication branches, and the setup link.
- **Approval path:** untouched. Approvals, `can_decide` and the deciding transaction behave the same in both modes, and the dogfood and isolation chains run unchanged. A test asserts this in temporary mode.
- **Deployment invariant:** unchanged. Temporary mode removes a recommended integration (the off-site copy); it adds no required process.
- **Schema:** one optional field in two responses, through the `schema-change` procedure, with no version bump.
- **Tests the implementation adds:**
  - in `e2e/isolation`, or a new `e2e/temporary`: an operator sets up and signs in with a password alone in temporary mode, and the same operator is refused a password-only sign-in after a restart in normal mode;
  - a browser session, a recent-authentication proof, a pending login and a setup credential made in temporary mode are all dead after a restart in normal mode, while script credentials still work;
  - an enrolled authenticator is still demanded in temporary mode, at each of item 2.1's five places;
  - setting the password in temporary mode ends the setup credential, and Skip does too;
  - re-authentication and password change in both modes;
  - an unknown `CONCHD_MODE` value stops `conchd` at start;
  - the banner fields;
  - the setup link's token leaves the address on load, and the link redeems only on a click;
  - `add-instance --temporary` and `remove-instance` in `make deploy-test`.
- **Work after acceptance:** server (mode, login, re-authentication, setup completion, audit), schema field, web (banner, skippable authenticator step, setup link), CLI (`whoami`), ops (#861 widened: `--temporary`, `remove-instance.sh`, the backup default), and docs (`docs/deploy.md`, the tester guide's note on temporary servers).
