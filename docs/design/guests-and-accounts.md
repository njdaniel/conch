# Guests, self-service accounts and signing in again

Issue [#694](https://github.com/njdaniel/conch/issues/694). The decision is
[ADR-009](../adr/ADR-009-guests-and-open-joining.md), which amends
[ADR-008](../adr/ADR-008-private-communications-alpha.md)'s "No public signup". This note is
the design: the flows, the model, the wire shapes to be designed through the schema-change
skill, the password and operator sign-in choices, the abuse controls, the threat notes and the
work in order.

**Status:** proposed with ADR-009, 2026-10-21; revised the same day after Nick's review of PR
#701 (§16). Nick's sign-off on the PR that adds both is the go-ahead to file the implementation
issues in §13. The wire shapes in §5 are sketches: names and fields only, to be designed through
the `schema-change` skill in the schema issue; nothing here is `pkg/schema` code.

## 1. What this adds

Three things, each small on its own:

1. **A front door.** Someone who opens the server's bare address today gets a field for a
   credential they do not have. With this, the page says what the server offers: sign in with a
   name and password, and, when an operator has turned **open joining** on, join as a guest or
   create an account.
2. **Guests.** A person who wants to look in, or talk once, without making an account. A guest is
   a real principal with a guest mark, narrow access, and a credential that lasts 7 days from its
   last use. A guest can keep its account, by setting a password, while it is live.
3. **A way back in.** Every account has a **password** from the moment it exists, so a member
   signs in again on any browser or device with their name and password, or adds a device with a
   one-use link they make for themselves. Operators sign in with name, password and a one-time
   code, and re-authenticate for sensitive actions. The operator's sign-in link (#493) stays as
   the member's recovery; the server's host is the operator's.

Not added: passkeys (deferred to a later phase, §15), recovery codes (evaluated in §6.8, later),
email of any kind, OIDC, a guest that can invite, agents made by anyone but an operator, or
anything that needs a process besides `conchd`.

## 2. Decided

**By Nick** (recorded on #694, 2026-10-21, "use your recommendations"; D3 revised on PR #701
the same day, "passwords being probably an easier option than passkeys. so lets just start with
passwords first"):

- **D1.** Open joining from the bare address is an operator setting, off by default. When on, it is
  rate-limited, can be paused like invites, offers guest, account or both, and every change is
  audited.
- **D2.** A guest is a real principal marked guest; its credential is kept in that one browser with a
  lifetime of 7 days of inactivity; it is scoped to what its link grants or to the server's open
  channels; voice and share-watching only if granted; it cannot invite, see members outside its
  channels or open one-on-ones; it shows with a "guest" badge; its messages stay after it
  expires; it can become a member, keeping its history.
- **D3 (revised 2026-10-21).** **Passwords are the primary way to sign in again**, hashed with
  argon2id through `golang.org/x/crypto`, which Nick approved the same day at the version §6.1
  measures. **Add a device** stays. The operator's sign-in link stays as a member's recovery.
  **Passkeys are deferred** to a later phase; nothing in this phase depends on WebAuthn or a Go
  toolchain bump. No email server is required.
- **D4.** Names are unique per server and case-insensitive, with a reserved list, and visually
  confusable names are refused.
- **D5.** Per-address rate limits (the trusted-proxy address of #609), a cap on guests at once, a
  pause-joining switch, bulk revoke of everyone from the bare address, and audit events.
- **D6.** `e2e/isolation` extends to guests and the bare-address path.

**Nick's three review points on PR #701** (2026-10-21), decided by the principal and binding here:

1. **No account is ever "ready" without a usable way back in.** Creating an account, from the
   bare address or an invite, takes the name and the password in one request. Keeping a guest
   account sets the password in the same request, and the guest mark clears only when the
   password is stored; a cancelled or refused Keep leaves the guest a guest, with its session,
   and the page says setup is incomplete. Nothing says "saved" before the server confirms.
   Accounts that exist today without a password are told to set one, with a banner until they
   do. There is no "Not now".
2. **Operators get a normal, repeatable sign-in with stronger requirements:** name, password and
   a TOTP code; a tab-scoped session of at most 12 hours and 1 hour idle; re-authentication
   within 10 minutes for sensitive administration; recovery offline on the server's host; bearer
   tokens unchanged for scripts and the CLI; the first operator gets a one-time setup link from
   bootstrap.
3. **The unavailable and recovery experience is spelt out:** passwords work on every browser and
   origin, with a warning on an unencrypted, non-loopback connection; a new device is the
   password or Add a device; a forgotten password is an operator's reset link; the remaining
   limitation, no self-service reset without a mail server, is stated; recovery codes are
   evaluated and left for later.

**Adopted by this note** under the standing delegation; Nick can overrule any at review. Each is
marked *(adopted)* where it appears, and listed again in §14:

- A guest may read files in its channels but not upload them.
- A guest may rename itself under the same rules as a member.
- Add a device is for members. Operators sign in by password and code and keep the one-tab rule
  (D2 of #260); guests have one credential and nothing else. D2's "that one browser" is, in the
  store's terms, **exactly one credential**: a guest holds one, made at join, and no route makes
  it another.
- An expired guest that posted keeps its name, and its messages read "<name> (guest)", until the
  sweeper hides it 90 days after expiry; one that posted nothing is hidden at expiry. The hide
  frees the name as a hide always has (§4.3).
- A guest never holds the right to decide approvals, and cannot be granted it (§4.2).
- A guest from the bare address may keep its account only while open joining still offers
  accounts (§3.6).
- The QR code of Add a device is a second step behind the link itself (§8).
- The argon2id parameters, the common-password list and its size, the slowdown rule, and the
  operator session bounds are as §6 and §7 give them.
- Re-authentication binds the credentials that password sign-in makes; a by-hand bearer token
  (bootstrap, `conch admin credential issue`) is the scripts' path and is exempt (§7.4).
- An operator's Keep of a guest answers with a reset link for the person, so the kept account
  gets its password from its owner, not a blank (§3.6).

## 3. The flows

Every page below is the browser client at `/app/`. "The server checks" means what `conchd` does
on the route named; the routes are listed in §5. The words on screen are the intent, not the
final copy. "Password rules" means §6.3.

### 3.1 An invite link

`<origin>/app/#/invite/<code>`, as today (#440), with one more choice and a password.

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | "You are invited to Conch": who invited you, the channels, whether voice is included, when it expires. Below: **Join as a guest**, **Create an account**, and "I already have an account: sign in". | `POST /v1/invites/preview`: per-address limit, pause switch, the code is live. Unchanged. |
| 2a | *Join as a guest*: a name field and "You can look around for as long as you keep using Conch; after 7 days away your guest visit ends. You can keep this account, with a password, at any time while you are here." Join. | `POST /v1/invites/redeem` with `as: "guest"`: the same checks as today, plus the guest cap (§9). Makes a guest principal with the invite's channels and voice, `may_invite` 0, `can_decide` 0 forced, and one renewable credential with the shorter of the invite's lifetime and 7 days. |
| 2b | *Create an account*: a name field and a password field with "At least 12 characters; anything you like, spaces included. You will sign in with your name and this password." **Create account** stays disabled until both are filled; the page checks the length and shows the server's refusal line otherwise. | `POST /v1/invites/redeem` with `as: "member"` (the default) and `password`: the invite checks of today, the name rules, the password rules; the hash is stored in the same transaction that creates the principal, so no member row ever exists without one. Answers a 90-day renewable credential as today. Audits `principal_created` and `password_set`. |
| 2c | *Sign in*: the name-and-password form of §3.4. | As §3.4. |
| 3 | The lobby, on the first channel the invite granted. A new member sees, once, "Your account is ready. You can sign in again anywhere with your name and password. Want to check? **Sign out and try signing in**." A guest sees the badge next to its own name and a line at the top: "You are a guest. Keep this account." | As today. |

What changes from today: step 2a is new, `redeem` may now create a guest, and a member
redemption **requires a password** (an optional field in the shape, required by the server for
`as: "member"`: §5.1 and §14.6). Invites today always create a full member with no password
(ADR-008 §4, `store.RedeemInvite`).

### 3.2 The bare address, open joining off

`<origin>/` redirects to `/app/`, as today.

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | "Conch at <server name>. This server is invite-only." Then the **Sign in** form: name and password (§3.4), and under it "Sign in with a token" for someone an operator gave one. | `GET /v1/join` (anonymous, its own per-address limit): answers the server's name, `open_joining: "off"`, the password policy's lengths. Nothing else. |
| 2 | Nothing to join. Someone with an invite, a sign-in link or a setup link opens that link. | A `POST /v1/join/guest` or `/v1/join/account` sent anyway answers `joining_closed`, is rate-limited like a redeem, and is audited thinned as `join_refused`. |

If the page is not in a secure context and the host is not loopback, a line above the form says:
"This connection is not encrypted. Your password would cross the network where others may read
it. Use an https address, or ask your operator." The form still works: a LAN is a legitimate
place for Conch, and the operator's runbook, not the page, decides (§6.6).

### 3.3 The bare address, open joining on

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | "Conch at <server name>." Then the buttons the setting offers: **Join as a guest**, **Create an account**, and under them the **Sign in** form. While paused: "Joining is paused for now" and only the sign-in part. While the guest cap is reached: the guest button is replaced by "No room for more guests right now". | `GET /v1/join`: `open_joining` is `guest`, `account`, `both` or `paused`; `guests_full` when the cap is reached. |
| 2a | *Join as a guest*: a name field and the same 7-day line as §3.1. | `POST /v1/join/guest` with the name: per-address and server-wide limits (the invite buckets), the setting is on and offers guests, not paused, the guest cap, the name rules of §4.4. Makes a guest principal with `origin: address`, a member of every open channel (§4.5), voice as the setting says, one 7-day renewable credential. Audits `principal_created` with `origin=address guest=true`. |
| 2b | *Create an account*: the name and password fields of §3.1 2b. | `POST /v1/join/account` with `name` and `password`: the same limits and switches, the setting offers accounts, the name rules, the password rules. Makes a member with `origin: address`, the open channels, voice as the setting says, `may_invite` 0, the password hash in the same transaction, a 90-day renewable credential. Audits `principal_created` and `password_set`. |
| 3 | The lobby, on the first open channel, with the "account is ready" line of §3.1 for a new member. | As today. |

### 3.4 A returning person

Someone who has an account and no credential in this browser: they signed out, cleared the
browser, or are on a new device.

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | The front door of §3.2 or §3.3: **Name**, **Password**, **Sign in**. Under it: "Forgot your password? Ask an operator for a sign-in link; it lets you set a new one." and "Sign in with a token". | `GET /v1/join` for the policy lengths, so the page refuses an impossible password before sending it. |
| 2 | *Sign in*: the two fields. A failure shows one line, "That name and password did not match", whatever the reason, and the fields keep what was typed. After repeated failures on a name the server refuses at once for a while (§6.4); the page shows "Please wait" with the `Retry-After` the server gave. | `POST /v1/auth/login` (anonymous; its own per-address bucket of failures and the per-name window, §9): first the per-name not-before check, answered 429 at once with no hashing if the name is still in its wait; then finds the principal by `name_key` (the case fold, never the skeleton); if none, or disabled, hidden, a guest, an agent, or without a password, verifies the password against a **dummy hash** anyway so the time taken is the same; else verifies against the stored hash under the anonymous hashing semaphore (§6.2). A member gets a 90-day renewable credential labelled "password sign-in", stored in `localStorage` as O1 says; an operator is asked for the code (§3.8). Every failure is the one answer 401 `login_failed`, audited thinned as `password_refused`; success audits `password_signed_in`. If the stored hash's parameters are older than the current policy, the server rehashes on this success (§6.2). |
| 3 | *Sign in with a token*: the field of today, for a token an operator issued or for an account that predates passwords (§3.10). | `GET /v1/whoami`, as today. |
| 4 | *A sign-in link* (`#/signin/<code>`), from an operator (recovery, §3.7) or from their own other device (§3.5). | `POST /v1/sign-in-links/preview` and `redeem`, as today (#493), with the reset step of §3.7 for an operator's link. |

Over the CLI, `conch login` gains a password mode (§5.4): it prompts for the name, the password
and, for an operator, the code, and stores the credential it gets as it stores a token today.

### 3.5 A new device: Add a device

A member signed in on device 1 wants in on device 2. The password of §3.4 is the first way; this
is the way that types nothing on device 2.

| Step | On screen (device 1) | The server checks |
| --- | --- | --- |
| 1 | Settings, "Your sign-ins" (#496): **Add a device**. "Makes a link that signs this account in once, on one other device, within 10 minutes. Anyone who opens it gets in as you: show it only to your own screen." | `POST /v1/sign-in-links/mine` (authenticated; members only, not guests, not agents, not operators): the per-principal create bucket, at most 3 open self-made links at once. Makes a sign-in link for the caller with a 10-minute expiry (max 1 hour), `created_by` the caller, `purpose: sign_in`. Audits `sign_in_link_created` with `self=true`. |
| 2 | "Your link is ready. Anyone who sees it can sign in as you." Then **Reveal**, which shows the link, a **Copy** button and the QR code of the link (§8). While this tab is sharing its screen, Reveal is disabled and the page says why. The page never shows the code again after this view is left. | Nothing. |
| 3 | On device 2: open the link, or scan the QR. "This link signs you in as <name>, who made it for their own device. If that is not you, close this page." **Sign in**. When this browser already holds a lasting sign-in for a different person, the page asks the person to type the name `<name>` before it signs the other out and continues. | `POST /v1/sign-in-links/preview` answers the name and `self: true`; then `redeem`, exactly as #493: one use, then spent; the new credential is a 90-day renewable one with the label "added device <link id>". A `new_password` sent with a self-made link is refused (`invalid_request`): a device link never resets anything. Audits `sign_in_link_redeemed`. |
| 4 | Device 2 is in. On device 1, "Your sign-ins" now lists "added device <id>" and the link's row is gone. | `GET /v1/credentials/mine` (as today) and `GET /v1/sign-in-links/mine`. |

On the TUI or CLI, device 2 is `conch login --link`, which reads the link or code from standard
input without echo, as `conch login` reads a token today (never from the command line, where it
would sit in the shell history and the process list), redeems it and stores the credential
(§5.4). Device 1 can also be the CLI: `conch device-link` prints the link.

### 3.6 Guest to member: Keep this account

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | The line "You are a guest. **Keep this account**" in the lobby, or the same in Settings. "Keeping it makes you a member: your name, channels and messages stay, and your sign-in no longer ends after 7 days away. Choose a password: you will sign in with your name and it." A password field (rules of §6.3) and **Keep**, disabled until the field is long enough; and **Cancel**. | Nothing yet. |
| 2 | **Keep**. The page shows "Saving…" and nothing else until the server answers. | `POST /v1/principals/{id}/keep` by the guest itself with `password`: the principal is a live guest. For a guest from the bare address, open joining must offer accounts (`account` or `both`) and not be paused at that moment: otherwise 403 `joining_closed` or 503 `joining_paused`, audited thinned as `join_refused reason=keep_closed`. A guest from an invite may keep freely. The password rules apply; then, **in one transaction**: the hash is stored, the guest mark cleared, `origin`, channels, voice, `may_invite` 0 and `can_decide` 0 kept on every membership (an operator may grant it afterwards, as to any member), the current credential's renewal set to 90 days for a guest from the address or the invite's credential lifetime for one from an invite. If the password is refused, nothing changes and the answer is 400 `password_weak` with the reason. Audits `guest_kept` with `origin` and `password_set`. |
| 3 | On success: "Your account is ready. You can sign in again anywhere with your name and password. **Sign out and try signing in** (optional)." The badge is gone. On a refusal: the line the server gave ("That password is too common" and the like), the field keeps what was typed, the guest stays a guest with its session, and the heading says "Setup is not finished: you are still a guest." Cancel does the same without a request. | |

An operator may also keep a guest (`POST /v1/principals/{id}/keep` with no password): the
guest mark clears, and the answer carries a **reset link** (§3.7) for the operator to hand to the
person, who sets their own password with it. Until they do, the account is a member without a
password and shows the banner of §3.10. *(adopted.)*

A guest that expired cannot be kept: there is no credential to come back with. The person joins
again, as a new guest or a new account. A guest from the bare address that finds Keep closed sees
"This server is not taking new accounts right now; ask an operator" and stays a guest.

### 3.7 Forgotten password: the operator's reset link

`conch admin principal sign-in-link <id>` (#493) makes a one-use link for a member, as today.
What is new is what the member can do with it: an operator-made link lets its opener **set a new
password** on the way in.

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | `#/signin/<code>`: "This link signs you in as <name>. An operator made it for you. Choose a new password." A password field and **Set password and sign in**. | `POST /v1/sign-in-links/preview`: the name, `self: false`, `purpose: reset`. |
| 2 | **Set password and sign in**. | `POST /v1/sign-in-links/redeem` with the code and `new_password`: the link is live and operator-made, the password rules; in one transaction the hash is replaced, **every other credential of the principal is revoked** (the attacker who knew the old password, and every stale browser, are out), a new 90-day renewable credential is made with the label "sign-in link <id>", and `sign_in_link_redeemed`, `password_reset` and one `credential_revoked` per credential are audited. The link is spent whether or not a password was sent; a reset link redeemed **without** `new_password` signs the person in as today and changes no password (an operator may make one for someone who merely lost their browser). |

"Your sign-ins" shows the operator-made sign-in afterwards, as #496 does. An operator cannot be
the target of a sign-in link (`ErrSignInLinkTarget`, unchanged): an operator's recovery is §3.9.
A member's own device links sit in the same table, told apart by `purpose: sign_in` and
`created_by` being the member.

**The limitation that remains, said plainly:** there is no self-service password reset. Someone
who forgets their password and has no signed-in device asks an operator. A self-service reset
needs a second channel the server can reach the person on (email, a phone), and Rule 1 forbids
requiring the process that would provide it. Recovery codes, which would close part of this gap
without a second channel, are evaluated in §6.8 and left for later.

### 3.8 An operator signs in

Operators keep the one-tab rule (D2 of #260: a stolen admin session is bounded by the tab) but
no longer paste a token someone generated for them.

| Step | On screen | The server checks |
| --- | --- | --- |
| 1 | The front door's name and password. | `POST /v1/auth/login`: the password verifies (§3.4); the principal is an operator with TOTP enrolled. Answers 200 with `totp_required: true` and a short-lived **login ticket** (an opaque id, 5 minutes, one use, bound to the principal and the client address), not a credential. An operator without TOTP enrolled cannot sign in by password at all: the answer is the one `login_failed`, since an operator always enrols TOTP when setting a password (§3.9). |
| 2 | "Enter the 6-digit code from your authenticator app." **Sign in**. | `POST /v1/auth/login/totp` with the ticket and the code (the same per-address bucket and per-name window as the password step; the ticket is spent on the first try, right or wrong, so a code cannot be guessed against one ticket): RFC 6238 with the stored secret, ±1 step, the counter not reused (§7.2). **A wrong code is an account failure exactly like a wrong password** (M3): it counts in the name's window and brings the same slowdown, and the window resets only here, when a credential is issued, never at the password step. On success: a credential of kind `session`, `expires_at` now + 12 h, `idle_ms` 1 h, `reauth_at` now, labelled "operator sign-in", returned with the one-tab warning the web shows today (`OperatorWarning.tsx`) and kept in `sessionStorage`. Audits `operator_signed_in`. The `totp_required: true` answer of step 1 tells the caller the password was right; that is how every two-step sign-in works and is accepted (§10). |
| 3 | The lobby. The page shows the session's end time in Settings. After 1 hour idle or 12 hours in all the next request answers 401 `session_expired` and the page returns to the front door with "Your operator session ended. Sign in again." | Every authenticated request on a `session` credential bumps `last_used_at` (written at most once a minute), refuses when idle past `idle_ms` or past `expires_at`, and audits `session_expired` once. |

### 3.9 The first operator, enrolment and recovery

- **Bootstrap.** `conchd bootstrap-operator --data <dir> --name <name>` creates the first operator
  as today and prints, instead of only a bearer token, a **one-time setup link**
  (`<origin>/app/#/setup/<code>`, 24 hours, one use) and, with `--token`, a bearer token as well
  for scripts. The setup page asks for a password (15 characters or more for an operator) and
  sends it with the redeem, which **spends the link at once** (S11) and answers a `session`
  credential **scoped to setup**: it may call only `/v1/totp/mine/*` and `GET /v1/whoami`, lasts
  15 minutes, and carries `reauth_at` now. The page then enrols TOTP: it shows the secret as text
  and the `otpauth://` URI as text (a QR of it is a later step, with Add a device's QR), asks for
  one code to confirm, and on confirmation signs the operator in as §3.8 step 2 does. Password
  sign-in for this operator is refused (the one `login_failed`) until the TOTP is confirmed.
  Leaving the page before confirming abandons the setup: the link is spent, the scoped session
  expires, and the way on is to run the host command again. The link is a `sign_in_links` row
  with `purpose: setup` and, by exception to `ErrSignInLinkTarget`, an operator target: only the
  host commands make one.
- **Enrolment later, and only from the host (S12).** An operator who exists today signs in by
  token as before and sees the banner of §3.10, which for an operator reads "Ask for a setup link
  on the server host to sign in without a token." An operator's password and TOTP are **never
  set through `POST /v1/password/mine` or `POST /v1/totp/mine/enrol` on a bearer**: for an
  operator without a password both answer 403 `setup_on_host`, so a stolen operator token cannot
  become a password and a second factor. The host command is `conchd setup-link --data <dir>
  --name <operator>`, which makes a setup link for an existing operator and revokes nothing, or
  `conchd reset-operator --keep-credentials`, the same for a reset. Changing a password that exists
  (`PUT`, with the current one and re-authentication) and replacing a confirmed TOTP (with
  re-authentication) stay online.
- **Recovery is offline, on the host.** `conchd reset-operator --data <dir> --name <name>` clears
  the operator's password and TOTP, revokes every credential of theirs (with `--keep-credentials`
  it leaves the credentials), and prints a new setup link. It ignores the last-operator guard
  (`ErrLastOperator`): the host is above it, and the setup link is the way back. Access to the
  data directory is the root of trust, as for bootstrap. Each host command writes its audit rows
  (`operator_reset`, `operator_setup_link_created`, actor `host:<command>`) into the database in
  the same transaction, and `conchd` logs one line at its next start naming every offline change
  made since its last run, so a reset done on the host is seen by whoever reads the log. There is
  no online operator reset: an operator who forgets both the password and the authenticator, and
  has no session, goes to the host.
- **The bearer path stays.** `conch login` with a token, `conch admin credential issue`, the
  bootstrap `--token`: unchanged, for scripts and the CLI. Such a credential is kind `token`, has
  no idle timeout and no re-authentication (§7.4), and is the thing an operator should keep few
  of. "Your sign-ins" shows each with its kind.

### 3.10 Accounts that exist today

Every human principal made before this ships has no password. They are not locked out: a token
or an operator's link still signs them in. Once in:

- `GET /v1/whoami` says `password_set: false`, and the web shows a **persistent banner** above the
  channel list: "Set a password so you can sign in again on any device." with **Set password**.
  It cannot be dismissed for longer than the tab; it goes away when the server confirms the
  password is stored. The TUI prints the same line at start and `conch login` prints it after a
  token sign-in.
- **Set password** (`POST /v1/password/mine` with `new_password`, allowed only while none is set,
  members only) stores the hash and, as a change does, revokes every other credential of the
  principal, so a bearer that leaked before the password existed is out; audits `password_set`
  with `first=true`. For an operator the route answers 403 `setup_on_host`: operators are set up
  only through a setup link made on the host (§3.9, S12), and their banner says so.
- A member without a password cannot use `POST /v1/auth/login` (the one `login_failed`), cannot
  change a password they do not have (`password_not_set`), and can still Add a device.

### 3.11 Changing a password

Settings, "Password": the current password, the new one twice. `PUT /v1/password/mine` with
`current_password` and `new_password`: the current one verifies (the dummy-hash timing rule does
not apply, the caller is known), the rules apply to the new one, the hash is replaced, **every
other credential of the principal is revoked** (this browser's stays), and `password_changed`
and the `credential_revoked` rows are audited. The page then says "Password changed. Other
devices were signed out." The CLI is `conch passwd`.

### 3.12 Re-authentication for sensitive administration

An operator on a `session` credential who does anything that **creates a principal, a
credential, a link or a hook, or grants a right or access, or removes a person** must have
re-authenticated within the last 10 minutes (the class is defined positively in §7.4, with the
route list). The page asks for the password and the code in a dialog ("Confirm it is you") the
first time in a 10-minute window; `POST /v1/auth/reauth` verifies both and sets the credential's
`reauth_at`; a wrong pair is an account failure like a wrong password (M3). A sensitive route
called without it answers 403 `reauth_required`, and the client retries after the dialog. The
CLI prompts the same way on a `session` credential. A `token` credential is exempt, with the
guards of §7.4 (S15).

## 4. The model

### 4.1 A guest principal

A row of `principals`, `kind = 'human'`, `role = 'member'`, with these additions:

| Column | Meaning |
| --- | --- |
| `guest INTEGER NOT NULL DEFAULT 0` | 1 while the principal is a guest. Cleared by Keep, in the transaction that stores the password. |
| `origin TEXT NOT NULL DEFAULT 'operator' CHECK (origin IN ('operator', 'invite', 'address'))` | How the account was made. Existing rows: `operator`, except those with `invite_id` set, which the migration marks `invite`. |
| `guest_expired_at INTEGER` | Set by the sweeper (§4.3) when a guest's last credential ran out. Never set on a member. |

Where it fits what exists:

- `invited_by` and `invite_id` are set for a guest from an invite link, as for a member; null for a
  guest from the bare address, whose `origin` is `address`. Bulk operations select by either.
- `may_invite` is 0 and an operator cannot set it on a guest (`PUT .../invite-permission` answers
  `invalid_request` for a guest, as it does for an agent).
- `voice_allowed` is what the invite or the open-joining setting granted. It gates voice *and*
  watching a share: a guest without voice holds no session, so it cannot subscribe to the room
  (the share is a track in the room).
- `channel_members` rows are made at join with `can_decide` 0, as for invited members (#450), and
  **a guest never holds `can_decide`**: the store forces 0 on every membership insert for a
  `guest = 1` principal, whatever the caller asked (today's default in `store/members.go` is
  `decide := canDecide == nil || *canDecide`, which would grant it to a guest an operator adds by
  hand), and `PutChannelMember` / `setCanDecideTx` refuse `can_decide: true` on a guest with
  `invalid_request`, as for an agent. Keep leaves every membership at 0; an operator may grant it
  afterwards, to what is then a member, as today.
- `name`, `name_key` and `name_skel` are as for everyone (#441).
- A guest has no password (`password_hash` null) and cannot set one except through Keep.

**Lifetime.** A guest holds **exactly one credential**, made at join: renewable (`renew_ms`, #439)
with a 7-day lifetime, or the invite's credential lifetime when that is shorter (the lifetime is
`min(invite lifetime, 7 days)`; a bare-address guest's is 7 days). The existing renewal rule moves
its expiry to the lifetime past each day's first use, so the credential, and with it the guest,
lasts 7 days from the last day it was used. The constants `MinCredentialRenewLifetimeSeconds` (2
days) and the 90-day default already bracket this. No second credential can be made for a guest:
Add a device, password sign-in (a guest has no password), the operator sign-in link and
`POST /v1/principals/{id}/credentials` all refuse a guest (`guest_not_allowed`).

**Expiry.** A guest is out the moment its credential is no longer live: the bearer check refuses
it, and `voiceEntitlement` requires a live credential for a `guest = 1` principal, so LiveKit
drops the guest within one poller pass (`voicePassGapMax`, 1.5 s) plus a room rotation, the same
bound the isolation check holds revocation to, not the sweeper's interval. The sweeper (§4.3) then
records the expiry. Signing out (`DELETE /v1/credentials/current`) revokes the one credential, so a
guest that signs out is gone at once; the sign-out dialog says so and offers Keep first.

### 4.2 What a guest can and cannot do (D2)

| | Guest | Member |
| --- | --- | --- |
| Read and post in its channels, threads, whispers, read state | yes | yes |
| Search | its channels | its channels |
| Read files in its channels | yes | yes |
| Upload files | **no** *(adopted)*: `guest_not_allowed` | yes |
| Voice, watch a share | if granted | if granted |
| Share a screen | if voice granted | if voice granted |
| List the members of its own channels | yes, as today for any member | yes |
| List principals, other channels' members | no, as today for any member | no |
| Make an invite | **no**, and cannot be granted the right | if granted |
| Decide approvals | **never**: `can_decide` forced 0 on every membership, and an operator cannot set it on a guest (§4.1) | no at join; an operator may grant |
| Request an approval (`POST /v1/approvals`) | **no**: `guest_not_allowed` | yes |
| Ask for an intervention, report a problem | yes | yes |
| Rename itself | yes *(adopted)*, same rules | yes |
| Your sign-ins, sign out | sees its one sign-in; signing out ends the visit (warned) | yes |
| Password sign-in, change password, Add a device | no (no password; `guest_not_allowed`) | yes |
| Be added to another channel by an operator | yes | yes |
| Keep this account | yes, while live, by setting a password | n/a |

"Cannot open one-on-ones": there is no DM route (D12); a one-on-one is a private channel an
operator makes. A guest cannot make channels (nobody but an operator can) and the operator UI
and CLI warn when adding a guest to a channel that is not open. Nothing more is needed.

**The badge.** Every shape that carries a principal to a client gains `guest: true` (omitted when
false, so a server that predates it reads as "not a guest"): `WhoAmIResponseV1`,
`ChannelMemberV1`, `PrincipalAdminV1`, `InviteJoinerV1`, and the invite preview's `inviter` is
never a guest. Messages carry `author_id` only, and clients name the author from the member
list, so the badge on a message comes from the member row. An expired guest stays in
`channel_members` (disabled, so it cannot act), with `expired: true` on its row, so its messages
still read "<name> (guest)" and the roster can fold expired guests away.

### 4.3 Expiry, names after expiry, and the sweeper

Being out is immediate (§4.1): a credential that is not live refuses the bearer and the voice
entitlement. A sweeper inside `conchd` (Rule 1: in-process, like the voice poller) records it
afterwards. It runs at start, and then at the earliest `expires_at` among live guest credentials
(re-read after every pass and after every guest join, and never later than 10 minutes away), so
`guest_expired` is written within seconds of the expiry rather than at a fixed interval:

1. Selects guests (`guest = 1`, `guest_expired_at IS NULL`) with no credential that is neither
   revoked nor expired.
2. For each: sets `disabled_at` and `guest_expired_at`, drops its sockets (`principalDisabled`, a
   no-op if the bearer check already did), and audits `guest_expired`. A socket an expired guest
   still holds is therefore closed within **the sweeper's wake plus the 2 s socket bound**; the
   fallback, should the sweeper be late, is the sockets' own 30 s `CredentialLive` recheck
   (`internal/server/ws.go`), which closes a socket whose credential is no longer live.
3. On the same pass, hides every guest it just expired **that has posted no message** (nothing
   to attribute, so nothing to keep): exactly as `POST /v1/principals/{id}/hide` does (#445:
   disabled, out of every channel and net, renamed `hidden-<id>`), audited `principal_hidden`
   with `reason=guest_expired`. Its name is free at once.
4. Selects guests whose `guest_expired_at` is 90 days or more ago and not yet hidden (those that
   did post), and hides each the same way.

Between expiry and the hide, a guest that posted keeps its name and its memberships, so its
messages still read "<name> (guest)" and the roster can fold it away as expired. **The name is
released only by the hide**, through the one path that has always freed names: at once for a
guest that said nothing, 90 days on for one that did; there is no second mechanism, no exception
in `claimNameTx`, and no reserved prefix beyond `hidden-`. A name a guest posted under is
therefore unavailable for up to 90 days after the guest last used Conch, which is the cost of
keeping attribution simple. *(adopted on the security review's finding S3 and re-check R1;
§14.2.)*

An operator may hide an expired guest earlier, singly or in bulk (§9). `POST
/v1/principals/{id}/enable` on an expired guest is refused (409 `invalid_request`, "an expired
guest is not re-enabled; the person joins again"), since enabling revives no credential and a
guest with no credential is nothing.

### 4.4 Names (D4)

Unchanged from #441: a name is stored and shown as typed, and two names clash when either of
their keys (case-folded, and UTS #39 skeleton) match.

Added: a **reserved list**, refused wherever a person chooses their own name (invite redemption,
join as guest, create an account, rename by oneself) and allowed only to an operator creating or
renaming a principal:

- `operator`, `operators`, `admin`, `administrator`, `root`, `system`, `conch`, `conchd`,
  `moderator`, `mod`, `staff`, `support`, `help`, `security`, `guest`, `guests`, `anonymous`,
  `nobody`, `everyone`, `here`, `channel`;
- the server's own name (the `server_name` setting of §4.6) and its host name;
- any name whose key starts with `hidden-` (already reserved, #445).

The comparison is by key, so `Admin`, `admın` and `ADMIN` are all reserved. The answer is 409
`name_reserved` ("that name is reserved; choose another"), distinct from `principal_exists`:
reserved words are public, so saying so is no oracle. The list lives in `pkg/schema`
(`ReservedNames`) so every client can refuse early with the same list.

### 4.5 Open channels

`channels.open_joining INTEGER NOT NULL DEFAULT 0`: 1 for a channel everyone who comes in from
the bare address, guest or account, is added to. Set by an operator (`PUT
/v1/channels/{channel}/open-joining`). Turning it off adds and removes nobody; turning it on
adds nobody already here. Open joining with no open channel is refused when turned on
(`no_open_channels`), so nobody can join into nothing. `#general` is not open by default.

### 4.6 Open joining settings

In `server_settings`, like `invites_paused`:

| Key | Default | Meaning |
| --- | --- | --- |
| `open_joining` | `off` | `off`, `guest`, `account`, `both`. |
| `joining_paused` | `false` | While true, the join routes answer `joining_paused`; the setting is kept. |
| `join_voice` | `false` | Whether someone from the bare address gets voice. |
| `max_guests` | `50` | Live guests at once, server-wide (guests not expired, not disabled). Max 10,000. |
| `server_name` | the host name | Shown on the front door and reserved as a name. |

Every change writes one `open_joining_changed` row with the old and new values; pause and resume
write `joining_paused` and `joining_resumed`.

### 4.7 Passwords, codes and sessions in the store

```text
principals
  password_hash      TEXT                     -- PHC string, §6.2; NULL for guests, agents and accounts not yet set up
  password_set_at    INTEGER
  totp_secret        BLOB                     -- 20 random bytes; operators only in this phase
  totp_enrolled_at   INTEGER
  totp_last_counter  INTEGER NOT NULL DEFAULT 0   -- the last accepted 30 s step, so a code is never accepted twice
credentials
  kind               TEXT NOT NULL DEFAULT 'token' CHECK (kind IN ('token', 'session'))
  scope              TEXT NOT NULL DEFAULT 'full' CHECK (scope IN ('full', 'setup'))  -- setup: /v1/totp/mine/* and GET /v1/whoami only (S11)
  idle_ms            INTEGER                  -- NULL for a token; 3,600,000 for an operator session; 900,000 for a setup session
  last_used_at       INTEGER                  -- bumped by requests at most once a minute; never by an open socket (S16)
  reauth_at          INTEGER                  -- when the password and code were last presented on this credential
sign_in_links
  purpose            TEXT NOT NULL DEFAULT 'sign_in' CHECK (purpose IN ('sign_in', 'reset', 'setup'))
```

- `password_hash` holds a PHC-format string (`$argon2id$v=19$m=65536,t=3,p=1$<salt>$<hash>`), so
  the parameters travel with the hash and can be raised later: a sign-in whose stored parameters
  are below the current policy is rehashed on success (§6.2). It is the one secret-derived value
  in the database and is never read by any route, listed in any shape, or written to any log or
  audit row; the store exposes only `VerifyPassword` and `SetPassword`.
- `totp_secret` is stored as it is. The database is the root of trust (Rule 1: no key service to
  wrap it with), as the credential hashes already are; a stolen database yields the secret, which
  is why an operator's password is also required and why the offline reset exists. It is never
  shown again after enrolment.
- A `session` credential is what password sign-in makes for an operator: an absolute end
  (`expires_at`, 12 h, never renewed), an idle end (`idle_ms` against
  `max(last_used_at, created_at)`) and a re-authentication time. The idle rule is applied by
  `CredentialLive` itself for `kind = session` (S16), so the bearer check, the WebSocket ticket
  route, the sockets' 30 s recheck and the voice re-check all refuse an idle-expired session the
  same way, and an open socket never keeps a session alive: only requests bump `last_used_at`. A
  setup session (`scope = setup`, 15 min) is the same row with the scope narrowed. A member's
  password sign-in makes an ordinary renewable `token` credential, as an invite does today, so
  O1's lasting browser sign-in is unchanged for members.
- Every existing `credentials` row is kind `token`; nothing about today's bearer path changes.
- `sign_in_links.purpose`: `sign_in` for a member's device link, `reset` for an operator-made
  link (the default for `POST /v1/principals/{id}/sign-in-links`), `setup` for the offline
  commands' link, the only kind that may target an operator.

### 4.8 Where an existing rule changes

| Today | After |
| --- | --- |
| An invite always creates a full member (ADR-008 §4). | It creates a member or a guest, at the joiner's choice. |
| A member is created with no password; Conch has no password login. | A member is created with a password in the same request, and signs in with it. |
| Only operators and invites create principals. | The bare address can, when open joining is on. |
| A person cannot make themselves a credential. | A member can make a one-use sign-in link for themselves, and a credential by password sign-in. |
| A sign-in link is made by an operator only (`ErrSignInLinkNotAllowed`), signs in and nothing more. | Also by a member, for themselves, with a shorter maximum life; an operator's link may set a new password on the way in; the offline commands make a setup link for an operator. |
| An operator signs in only by pasting a bearer token. | Name, password and code, in a bounded session with re-authentication; the token stays for scripts. |
| A principal is hidden only by an operator. | The sweeper hides expired guests: at once with no messages, 90 days after expiry with. |
| An operator may grant `can_decide` to any human member. | Never to a guest; the store forces 0 for guests on every membership write. |
| No name is reserved but `hidden-*`. | A reserved list, for self-chosen names. |
| "No public signup" (ADR-008). | Open joining, off by default (ADR-009). |

## 5. Proposed wire shapes and routes

**To be designed through the `schema-change` skill.** Names and fields only. Every shape is
additive: a new type, or a new optional field on an existing V1 type, with no change of meaning to
anything published; the schema issue confirms this fixture by fixture, and §14.6 names the one
route whose server-side rule tightens. Every new string set is a closed vocabulary registered in
`vocabularies.go`, and every new shape is in `ts.go`'s table so `web/src/schema/` is generated
from it. **No request or response shape ever carries a password hash, a TOTP secret after
enrolment, or a token outside the one create response that already shows it**, and every
response that carries a token, a login ticket or a TOTP secret is written with `writeSecretJSON`
(no-store), as the invite and sign-in-link responses are today.

### 5.1 Additions to existing shapes

| Shape | New optional field | Notes |
| --- | --- | --- |
| `WhoAmIResponseV1` | `guest bool`, `origin PrincipalOrigin`, `password_set bool`, `totp_enrolled bool`, `can_add_device bool`, `session_expires_at *Timestamp`, `reauth_until *Timestamp`, `setup_only bool` | the timestamps only on a `session` credential; `setup_only` on a setup-scoped one (S11); `can_add_device` is false for guests, agents and operators. |
| `ChannelMemberV1` | `guest bool`, `expired bool` | §4.2. |
| `PrincipalAdminV1` | `guest bool`, `origin`, `guest_expired_at *Timestamp`, `password_set bool`, `totp_enrolled bool` | |
| `InviteJoinerV1` | `guest bool` | |
| `RedeemInviteRequestV1` | `as JoinAs` (`member` default, `guest`), `password string` | optional in the shape; the server requires `password` for `as: "member"` (§14.6). |
| `RedeemSignInLinkRequestV1` | `new_password string` | accepted only on a `reset` link (§3.7). |
| `SignInLinkPreviewV1` | `self bool`, `purpose SignInLinkPurpose` | §3.5, §3.7. |
| `SignInLinkV1` | `purpose SignInLinkPurpose` | |
| `CredentialV1`, `OwnCredentialV1` | `kind CredentialKind`, `expires_at` already there; `reauth_until *Timestamp` on `OwnCredentialV1` for the current one | |
| `ListPrincipalsRequest` (query) | `?guest=true`, `?origin=address`, `?password_set=false` | filters |
| `HidePrincipalsRequestV1` | `origin PrincipalOrigin`, `guest *bool`, `expired *bool` | bulk hide of expired guests, of everyone from the address. Still requires `dry_run`. |
| `ChannelV1` (operator views) | `open_joining bool` | |
| `ErrorV0.Code` | new codes below | |

New codes: `joining_closed` (404, open joining off), `joining_paused` (503), `guests_full` (503,
with `Retry-After`), `name_reserved` (409), `guest_not_allowed` (403: a guest asked for a member's
thing), `login_failed` (401: the one answer of a failed password or code sign-in),
`password_weak` (400, with `reason` one of `too_short`, `too_long`, `common`, `contains_name`),
`password_required` (400: a member redemption or join without one), `password_not_set` (409),
`totp_required` (409: an operator setting a password without a confirmed enrolment),
`totp_invalid` (400: the one answer of a failed enrolment confirmation), `reauth_required`
(403), `session_expired` (401), `setup_on_host` (403: an operator asked to set a first password
or enrol TOTP online, S12), `setup_only` (403: a setup-scoped session asked for anything but
TOTP enrolment, S11; the scope is enforced once, in `authMiddleware` beside the session idle and absolute checks, never per handler, so no route can forget it), `no_open_channels` (400), `sign_in_link_limit` (409: too many open
self-made links), `busy` (503, `Retry-After`: the hashing semaphore is full).

New vocabularies: `PrincipalOrigin` (`operator`, `invite`, `address`), `JoinAs` (`member`,
`guest`), `OpenJoining` (`off`, `guest`, `account`, `both`), `SignInLinkPurpose` (`sign_in`,
`reset`, `setup`), `CredentialKind` (`token`, `session`), `PasswordWeakReason` (`too_short`,
`too_long`, `common`, `contains_name`).

### 5.2 New shapes

| Shape | Fields |
| --- | --- |
| `JoinInfoV1` | `server_name`, `open_joining OpenJoining`, `paused bool`, `guests_full bool`, `password_policy PasswordPolicyV1`, `server_version` |
| `PasswordPolicyV1` | `min_length int` (12), `operator_min_length int` (15), `max_length int` (128): the lengths, so a client refuses early with the server's numbers |
| `JoinGuestRequestV1` | `name` |
| `JoinAccountRequestV1` | `name`, `password` |
| `JoinResponseV1` | `principal PrincipalV0`, `credential CredentialV1`, `token` (the invite redeem shape, reused as is; §14 asks whether to reuse `RedeemInviteResponseV1` by name) |
| `OpenJoiningSettingsV1` | `open_joining OpenJoining`, `paused bool`, `voice bool`, `max_guests int64`, `server_name string`; both the body of the change and the answer to reading it, like `InviteSettingsV1` |
| `ChannelOpenJoiningRequestV1` | `open_joining bool` |
| `KeepAccountRequestV1` | `password string` (required when the caller is the guest; absent for an operator) |
| `KeepAccountResponseV1` | `principal PrincipalV0`, `reset_link *CreateSignInLinkResponseV1` (only for an operator's Keep, §3.6) |
| `LoginRequestV1` | `name`, `password` |
| `LoginResponseV1` | either `principal`, `credential`, `token` (a member, or an operator in a later phase without TOTP), or `totp_required: true`, `ticket string` (an operator) |
| `LoginTotpRequestV1` | `ticket`, `code` |
| `LoginTotpResponseV1` | `principal`, `credential`, `token` |
| `SetPasswordRequestV1` | `new_password` (first password of an account that has none) |
| `ChangePasswordRequestV1` | `current_password`, `new_password` |
| `PasswordChangedResponseV1` | `revoked int` (other credentials ended) |
| `ReauthRequestV1` | `password`, `code` (code required for an operator) |
| `ReauthResponseV1` | `reauth_until Timestamp` |
| `TotpEnrolResponseV1` | `secret` (base32, shown once), `otpauth_uri`, `issuer`, `account`, `digits` (6), `period` (30) |
| `TotpConfirmRequestV1` | `code` |
| `TotpStatusV1` | `enrolled bool`, `enrolled_at *Timestamp` |
| `CreateOwnSignInLinkRequestV1` | `expires_at *Timestamp` (default 10 min, max 1 h) |
| `ListOwnSignInLinksResponseV1` | `sign_in_links []SignInLinkV1` (open ones only) |
| `ReservedNames` | a `[]string` constant and `IsReservedName(name) bool` on the key, with a golden fixture of the list |
| `PasswordRules` | the constants `MinPasswordLength` 12, `MinOperatorPasswordLength` 15, `MaxPasswordLength` 128, and `ValidatePasswordShape(pw, name, operator)` (length and name rules only; the common list is the server's, §6.3) |

### 5.3 Routes

Access classes are those of `internal/server/routes.go`. Anonymous routes share the invite
limiter's machinery with buckets of their own (§9).

| Route | Access | Does |
| --- | --- | --- |
| `GET /v1/join` | exempt | `JoinInfoV1`. Its own per-address bucket (§9). |
| `POST /v1/join/guest` | exempt | Join as a guest from the bare address. Redeem buckets, the name-taken allowance, guest cap. |
| `POST /v1/join/account` | exempt | Create an account from the bare address, with a password. Redeem buckets, the name-taken allowance. |
| `POST /v1/auth/login` | exempt (404 under `AuthOff`, where nobody can be signed in) | §3.4, §3.8 step 1. Its own per-address bucket of failures and the per-name window, the not-before check, the anonymous hashing semaphore. |
| `POST /v1/auth/login/totp` | exempt (404 under `AuthOff`) | §3.8 step 2: the ticket and the code. The same bucket and window as login; a wrong code counts like a wrong password (M3). |
| `POST /v1/auth/reauth` | authenticated; human | §3.12. The per-name window; a wrong pair counts like a wrong password (M3). |
| `POST /v1/password/mine` | authenticated; member, not a guest | first password (§3.10); an operator gets 403 `setup_on_host` (S12). |
| `PUT /v1/password/mine` | authenticated; human, not a guest; re-auth for an operator | change (§3.11). |
| `POST /v1/totp/mine/enrol`, `POST /v1/totp/mine/confirm`, `GET /v1/totp/mine` | authenticated; operator in this phase; `enrol` only on a setup-scoped session or re-authenticated for a replacement | §3.9. Enrol answers the secret once; confirm takes the first code and shares `totp_last_counter` with login; an operator token with no password gets 403 `setup_on_host` (S12). |
| `GET /v1/join/settings` | operator | `OpenJoiningSettingsV1`. |
| `PUT /v1/join/settings` | operator, re-auth | the change. |
| `GET /v1/join/principals` | operator | Everyone with `origin = address`, like `GET /v1/invites/{id}/principals`. |
| `POST /v1/join/disable-joiners` | operator, re-auth | Disables everyone from the address, like `disable-joiners`; `dry_run`. |
| `PUT /v1/channels/{channel}/open-joining` | operator, re-auth (it grants access, S13) | §4.5. |
| `POST /v1/principals/{id}/keep` | self; or operator, re-auth (it makes a link, S13) | §3.6; the body differs by caller. |
| `POST /v1/sign-in-links/mine`, `GET /v1/sign-in-links/mine`, `DELETE /v1/sign-in-links/mine/{id}` | authenticated; members only | Add a device, §3.5. |
| `POST /v1/principals/{id}/sign-in-links` | operator, re-auth | unchanged route; the link is now `purpose: reset`. |
| `POST /v1/sign-in-links/redeem` | exempt | unchanged route; takes `new_password` on a reset link. |
| `POST /v1/invites/redeem` | exempt | unchanged route; takes `as` and `password`. |
| `POST /v1/principals/hide` | operator, re-auth | unchanged route; takes the new selectors. |
| the operator routes of §7.4's list (`POST /v0/principals`, `POST /v1/principals/{id}/credentials`, `POST /v1/credentials/{id}/rotate`, `PUT /v1/channels/{channel}/members/{principal_id}`, `PUT /v1/principals/{id}/invite-permission`, `PUT /v1/principals/{id}/manifest`, `POST /v1/hooks`, `/disable`, `/hide`, `disable-joiners`, `disable-invitees`, the `revoke-all` of another, any change to another operator) | operator, re-auth | existing routes that now require a fresh re-authentication on a `session` credential (§7.4, S13). |

Existing routes that gain a refusal for guests: `POST /v2/channels/{channel}/files` and
`POST /v1/approvals` (`guest_not_allowed`), `POST /v1/invites` (`invite_not_allowed`, as for
anyone who may not), `POST /v1/principals/{id}/credentials` and
`POST /v1/principals/{id}/sign-in-links` for a guest target (`invalid_request`, as for an agent),
`PUT /v1/channels/{channel}/members/{principal_id}` with `can_decide: true` for a guest
(`invalid_request`, as for an agent), `POST /v1/principals/{id}/enable` on an expired guest
(409 `invalid_request`), and the password and TOTP routes (`guest_not_allowed`). Every new
authenticated route answers 401 under `AuthOff`, as the invite routes do (`inviteCaller`): with
authentication off there is nobody to act.

### 5.4 Parity (Rule 4)

| Browser action | REST | CLI/TUI |
| --- | --- | --- |
| Front door | `GET /v1/join` | `conch join info --server <url>` (no credential) |
| Join as a guest / create an account | `POST /v1/join/*` | `conch join guest --server <url> --name <name>`, `conch join account --server <url> --name <name>` (the password prompted without echo, twice): stores the credential like `conch login` |
| Join from an invite | `POST /v1/invites/redeem` | `conch join invite [--as guest]`, the link read from standard input; a member join prompts for the password (today there is no CLI redemption; this adds it) |
| Sign in with name and password | `POST /v1/auth/login` (+ `/totp`) | `conch login --password`: prompts name, password and, for an operator, the code, all without echo; stores the credential; says when it is a bounded operator session |
| Sign in with a token | `GET /v1/whoami` | `conch login` as today |
| Keep this account | `POST /v1/principals/{id}/keep` | `conch account keep` (password prompted twice) |
| Set or change the password | `POST`/`PUT /v1/password/mine` | `conch passwd` |
| Enrol TOTP | `/v1/totp/mine/*` | `conch login --link` with the host-made setup link (which yields the setup-scoped session), then `conch totp enrol` (prints the secret and URI as text) and `conch totp confirm <code>`; `conch totp status` any time; `enrol` on an ordinary operator token is the server's `setup_on_host` refusal (S12) |
| Re-authenticate | `POST /v1/auth/reauth` | prompted by any `conch admin` command that gets `reauth_required` on a session credential |
| Add a device (make) | `POST /v1/sign-in-links/mine` | `conch device-link` |
| Add a device (use) | `POST /v1/sign-in-links/redeem` | `conch login --link`, the link or code read from standard input without echo as `runLogin` reads a token, never from an argument |
| Reset link for a member | `POST /v1/principals/{id}/sign-in-links` | `conch admin principal sign-in-link <id>` (unchanged; the link now resets) |
| Operator Keep | `POST /v1/principals/{id}/keep` | `conch admin principal keep <id>` prints the reset link for the person |
| Operator: open-joining settings, open channels, joiners, bulk disable, passwords and TOTP of a principal (status, clear TOTP) | the operator routes above | `conch admin join status|set|pause|resume|joiners|disable-joiners`, `conch admin channel open-joining <name> on|off`, `conch admin principal list --guest --origin address --no-password`, `conch admin principal hide --guest --expired`, `conch admin principal totp-clear <id>` (members, in a later phase; operators go to the host) |
| First operator setup, operator recovery | none: offline | `conchd bootstrap-operator` prints the setup link (and `--token` a token); `conchd reset-operator --data <dir> --name <name>` |
| The guest badge | `guest` on member shapes | the TUI shows "(guest)" after the name |

Every row has a REST route first, and the CLI calls only that route. Nothing is browser-only in
this phase (the passkey ceremony, which a terminal cannot run, is the deferred phase's problem).

## 6. Passwords (D3, revised)

### 6.1 The library: `golang.org/x/crypto` at `v0.55.0`, approved

**Approved by Nick on 2026-10-21** ("use argon2id, x/crypto approved", recorded on PR #701), at
the exact version measured here; the `deps-allowlist.txt` line lands with the server password
issue (G4). `v0.55.0` is the newest release whose `go` directive is `1.25.0`, which is `go.mod`'s;
`v0.56.0` and later declare `go 1.26.0` and are not taken.

| Version | `go` directive | Released |
| --- | --- | --- |
| v0.58.0 | 1.26.0 | 2026-10-09 |
| v0.57.0 | 1.26.0 | 2026-09-08 |
| v0.56.0 | 1.26.0 | 2026-09-02 |
| **v0.55.0** | **1.25.0** | **2026-08-11** |
| v0.54.0 | 1.25.0 | 2026-07-08 |

Measured on 2026-10-21 in a throwaway module (`go mod init x && go mod edit -go=1.25.0 && go get
golang.org/x/crypto@v0.55.0 && go mod tidy`) with a `main` that imports `golang.org/x/crypto/argon2`
and calls `argon2.IDKey`, so `go mod tidy` keeps only what the build needs. The build graph:

| Module | Version | Licence | Note |
| --- | --- | --- | --- |
| `golang.org/x/crypto` | v0.55.0 | BSD-3-Clause | **new**, direct; `go.mod` has none today |
| `golang.org/x/sys` | v0.47.0 | BSD-3-Clause | already in `go.mod` (indirect) at exactly this version; no bump |

Two modules, one of them new. `argon2` itself depends only on `x/sys/cpu` and `x/crypto/blake2b`
(the same module). No toolchain change: Go 1.25.0 as today, the lint pin untouched.

### 6.2 Hashing: argon2id, parameters and bounds

- **Algorithm:** argon2id (RFC 9106), the memory-hard choice OWASP lists first.
- **Parameters** *(adopted)*: **m = 64 MiB, t = 3, p = 1**, salt 16 bytes from `crypto/rand`, tag
  32 bytes. OWASP's floor is m = 19 MiB, t = 2, p = 1; RFC 9106's second recommended setting is
  m = 64 MiB, t = 3, p = 4. The design takes the RFC's memory and passes with one lane, since the
  droplet of private-alpha.md O7 has two vCPUs and the lanes would compete with everything else
  `conchd` does. On that droplet one hash takes about 150–300 ms, which is what a sign-in costs
  and what an attacker with the database pays per guess.
- **Bounded concurrency, in two pools** *(adopted; S14)*: a semaphore of **4** concurrent hashes
  for the anonymous routes (`POST /v1/auth/login` and `/login/totp`, the dummy hash included) and
  a second semaphore of **2** for every authenticated or creating path (join, invite redemption,
  Keep, set, change, reset, re-authentication), `--password-hash-concurrency` setting the first
  and the second being half of it (at least 1). The peak is 384 MiB on a 4 GB droplet whatever the
  traffic, and a flood of sign-ins can never stop a person who is already in from keeping, joining
  or changing a password. A request that waits more than 5 s for a slot answers 503 `busy` with
  `Retry-After: 5`, audited thinned as `password_hash_busy`. Within a pool, verification and the
  dummy hash take the same slot, so timing is uniform under load too.
- **Storage** is the PHC string of §4.7. The parameters are read back from it on verify, so
  raising the policy later (a bigger droplet, a slower attacker) is one constant change: a
  sign-in whose string carries lower parameters than the policy is **rehashed on success** with
  the password just verified, in the same transaction, audited `password_rehashed`. Nothing has to
  be migrated by hand.
- **No pepper.** A server-side secret mixed into the hash would need to live outside the database
  to help, which Rule 1's one-directory deployment does not give; the memory-hard hash is the
  defence against a stolen database.
- **Normalisation:** the password is NFC-normalised (`golang.org/x/text/unicode/norm`, already a
  dependency) and nothing else before hashing, so the same password typed on two keyboards
  matches and no character class is stripped. Length is counted in code points after NFC.

### 6.3 Password rules (NIST SP 800-63B)

- **Length:** at least **12** characters for a member, **15** for an operator, at most **128**,
  counted in Unicode code points after NFC. Any character is allowed, spaces and emoji included;
  there are no composition rules (no "one digit and one capital"), no periodic expiry, and no
  hints.
- **Refused:** a password equal to the account's name or the server's name (always), or
  containing either when that name's key is **4 characters or more** (a two-letter name would
  otherwise forbid most passwords); both compared by `names.KeysOf`, so case and look-alikes
  count; and a password on the **embedded common-password list**.
- **The list** *(adopted)*: the xato-net 10-million-password corpus's top 1,000,000, as published
  in SecLists (`Passwords/Common-Credentials/xato-net-10-million-passwords-1000000.txt`, MIT
  licence, Daniel Miessler), **filtered to entries of 12 or more characters** at generation time,
  since shorter ones are refused by the length rule anyway. Measured on 2026-10-21: **46,296
  entries, 677 KB** as text, embedded with `go:embed` in `internal/server/passwords/` as a sorted
  list of NFC, case-folded entries and searched by binary search; the generator pins the source
  file's SHA-256 as `internal/server/names/gen` pins the Unicode tables. (The top 100,000 of the
  same corpus leaves only 488 entries past the length rule, and NCSC's top 100,000 only 1,274:
  too few to be a blocklist.) The comparison is case-folded, so `Password1234` is refused with
  `password1234`. The licence file travels with the generated list.
- **The answer** to a refused password is 400 `password_weak` with `reason`, so the page can say
  "That password is on a list of common ones; choose another" without the server echoing the
  password. Checking is done before hashing, so a refused password costs no semaphore slot.
- **Nothing is logged** about a password but its refusal reason; no length, no fragment, no hash.

### 6.4 Signing in: uniform failure, limits and slowdown

- **One answer.** `POST /v1/auth/login` answers 401 `login_failed` with the one message "that
  name and password did not match" for an unknown name, a disabled, hidden, guest or agent
  principal, an account with no password, a wrong password, and an operator without TOTP. The
  only other answers are 429 (a limit) and 503 `busy`.
- **Uniform timing.** The lookup is by `name_key` (the case fold) only, never the confusable
  skeleton, so it is one indexed read whatever the name. When the name resolves to no principal
  that may sign in, the server still verifies the password against a **dummy hash**, under the
  same semaphore, so the time to answer does not say whether the name exists. The dummy hash is
  made at start, and after every rehash, with **the lowest parameters still present in
  `principals.password_hash`** (so while old hashes remain, an unknown name costs what the
  cheapest real one costs); when every hash is at the policy, it is the policy's.
- **Limits** (§9): a per-address bucket of its own (`password_login_per_address`, 10 **failures**
  per 10 minutes; a success spends nothing, so a shared office address is not locked out by
  people signing in), a cap on **successes** per name key (30 per hour, then 429 with `Retry-After`), so a party holding a valid password cannot mint lasting credentials without bound, and a per-name window (§M4 below).
- **The per-name window (M4)**, keyed by **the typed name's folded key, whether or not a
  principal exists**, so probing unknown names costs the same as probing real ones and the window
  itself is no oracle. The table is bounded exactly as `bucketSet` is (`maxInviteLimitKeys`, then
  the overflow buckets by wide network, then one overflow bucket), so an attacker cannot grow it
  by inventing names. Each key holds a failure count and a **not-before time**.
- **Slowdown, not lockout** *(adopted)*: after 5 failures on one name within an hour, the key's
  not-before time is set `min(2^(failures-5), 30)` seconds ahead (1, 2, 4, 8, 16, 30, 30, …); an
  attempt before it is **answered 429 with `Retry-After` at once, with no hashing and no server-
  side sleep**, and does not itself count as a failure. The name is never locked: its owner with
  the right password and code gets in after the wait, so an attacker cannot lock someone else out
  by guessing at their name. **A failed TOTP step and a failed re-authentication count as failures
  exactly like a wrong password (M3)**, in the same window; the window resets **only when a
  credential is issued** (the member's sign-in, the operator's code step, or re-authentication
  succeeding), never at the password step, and an hour after the last failure. Refusals are
  audited thinned as `password_refused` with `reason=invalid|wait|ticket`, actor `anonymous`, the
  route pattern as subject, `principal=<id>` when the name resolved, and never the name.
- **Operator two-step.** An operator's first step answers a login ticket, not a credential (§3.8):
  32 random bytes, 5 minutes, one use, bound to the principal and the client address, in a
  bounded in-memory table (8 per address, 1,000 in all). The ticket and the code go to
  `POST /v1/auth/login/totp`, which spends the ticket before checking the code.
- **Success** makes the credential of §4.7 and audits `password_signed_in` (actor `principal:<id>`,
  detail `credential=<id> kind=token|session`); an operator's also `operator_signed_in`.

### 6.5 Changing and resetting

- **Change** (§3.11) needs the current password; a `session` credential also needs a fresh
  re-authentication for an operator. It revokes every other credential of the principal and
  closes their open self-made device links; this credential stays.
- **Reset** (§3.7) is an operator-made sign-in link (`purpose: reset`, **24 hours at most**, the
  default too) redeemed with `new_password`: it replaces the hash, revokes every credential of the
  principal (the new one is made after), closes their open device links, and audits
  `password_reset` with the link id. An operator cannot reset their own or another operator's
  password online; operators go to the host (§3.9).
- **Disable and hide** revoke every credential, as today, close every sign-in link, as today, and
  leave the password hash in place (a re-enabled member signs in with the password they had);
  password sign-in on a disabled or hidden principal is the one `login_failed`, re-read inside the
  transaction that would make the credential, so a disable that lands between verify and insert
  wins. A hidden principal's hash is cleared by the hide, since the account is gone.
- **An operator's reset of a member** is the same `POST /v1/principals/{id}/sign-in-links` route
  as #493, now a re-authenticated one.

### 6.6 Where passwords work, and the connection

A password works on every browser and every origin: `https://conch.example.com`, `http://localhost`,
`http://127.0.0.1:8080`, `http://192.168.1.10:8080`, a self-hosted name without TLS. Nothing in
the browser needs a secure context for a form.

What a plain `http://` origin does not give is confidentiality: **over plain http a password
crosses the network in clear**, readable by anyone on the path. So:

- The **front door warns** (§3.2) whenever `window.isSecureContext` is false and the host is not
  loopback (`localhost`, `127.0.0.1`, `::1`): one sentence above the form, not a block.
- The **deployment runbook** (private-alpha.md O7, the README's deployment section, the Caddy
  file) **requires TLS for anything beyond localhost or a trusted LAN**, and says that a LAN is
  the operator's call.
- `conchd` logs one warning at start when it serves `/app/` on a non-loopback listener with no
  trusted proxy (the same line §9 asks for the rate-limit keys), since that is the shape of an
  unencrypted public deployment.

### 6.7 A new device, and a forgotten password

| Situation | What the person does | What stands behind it |
| --- | --- | --- |
| New browser or device, password known | Front door: name and password. | §3.4 |
| New device, another device signed in, nothing to type | Add a device on the signed-in one, open the link on the new one. | §3.5 |
| Signed out, password known | Name and password. | §3.4 |
| Password forgotten, another device still signed in | Settings: change the password (needs the old one: no), so instead Add a device to keep access and ask an operator for a reset link. | §3.5, §3.7 |
| Password forgotten, nothing signed in | Ask an operator for a reset link; set a new password on the way in. | §3.7 |
| Operator: forgot password or lost the authenticator | On the host: `conchd reset-operator`, then the setup link. | §3.9 |
| Operator: session ended (idle or 12 h) | Name, password, code again. | §3.8 |
| Account from before passwords | Token or an operator's link, then the banner: set a password. | §3.10 |

**What remains operator-assisted, explicitly:** a member who forgets the password and has no
signed-in device needs an operator; an operator who loses both factors needs the host. Conch
cannot offer a self-service reset without a channel to the person that the server controls
(email, SMS), and Rule 1 forbids requiring the process that provides one. This is the designed
limit of this phase, not an oversight.

### 6.8 Recovery codes, evaluated

One-time **recovery codes**, made at account setup (say ten of 10 random bytes each, shown once,
stored hashed), would let a person who forgot the password sign in once and set a new one with
no operator and no second channel.

| For | Against |
| --- | --- |
| Self-service recovery within Rule 1: nothing but `conchd`. | They are a second password that most people never store safely; the alpha's testers would lose them, and the operator would be asked anyway. |
| Standard, understood, cheap to hash (SHA-256 is enough: the codes are random, not chosen). | For an operator they would bypass TOTP, or need TOTP too, which defeats the purpose when the authenticator is what was lost. |
| A good answer for a member whose operator is unreachable. | More surface: a setup step everyone must complete, a page that shows secrets, a redeem route with its own limits and audit, and a "regenerate codes" flow with re-authentication. |

**Recommendation:** **later.** The alpha's population is small and operator-reachable, the
operator's reset link already covers the member case, and the operator case is covered by the
host. Recovery codes are the right next step if the operator-assisted limitation of §6.7 bites in
practice; they belong in the same later phase as passkeys (§15), where both become optional
second ways in. The principal leans the same way. *(adopted.)*

## 7. Operators

### 7.1 What changes for an operator

| Today | After |
| --- | --- |
| Paste a bearer token someone generated. | Name, password (15+), authenticator code. |
| The credential lasts until revoked (unless an expiry was set). | A session of at most 12 hours, ended after 1 hour idle. |
| One tab (D2 of #260). | One tab, unchanged. |
| Nothing asks again. | Re-authenticate within 10 minutes for sensitive actions. |
| Recovery: another operator issues a token, or bootstrap on the host. | Recovery: on the host only (`conchd reset-operator`), and a setup link from it. |
| Scripts use a token. | Scripts use a token, unchanged. |

### 7.2 TOTP (RFC 6238), stdlib only

- Secret: 20 random bytes, shown once as base32 and in an `otpauth://totp/Conch:<name>?secret=…&
  issuer=Conch%20<server name>&algorithm=SHA1&digits=6&period=30` URI, as text (a QR is a later
  step, alongside Add a device's). HMAC-SHA-1 from `crypto/hmac` and `crypto/sha1`, 6 digits, a
  30-second step: the parameters every authenticator app supports.
- Verification accepts the current step and one on either side (±30 s of clock drift) and
  **never the same or an earlier step twice**: `totp_last_counter` holds the last accepted step and
  a code at or below it is refused, so a code seen over a shoulder cannot be replayed within its
  window. Sign-in, re-authentication and enrolment confirmation **share that counter**: a code
  accepted by any one of them is spent for the others.
- Enrolment is confirmed by one correct code before it counts; until then the principal has no
  TOTP and the old one, if any, still applies. Replacing a confirmed enrolment needs a fresh
  re-authentication. An operator cannot remove their TOTP while they have a password: the two
  come and go together, so an operator never has a password login without the second factor.
- No dependency: `crypto/hmac`, `crypto/sha1`, `encoding/base32`, `crypto/rand`.
- Members do not enrol TOTP in this phase; the routes refuse them (`operator_required`). Making it
  optional for members is additive, later.

### 7.3 The operator session

A `session` credential (§4.7): absolute end 12 hours after sign-in, never renewed; idle end 1
hour after `max(last_used_at, created_at)`; `last_used_at` bumped by requests at most once a
minute so a busy session does not write on every request, and **never by an open socket** (S16):
a tab left open on a channel idles out like one left closed. The idle rule lives in
`CredentialLive` for `kind = session`, so the WebSocket ticket route, the sockets' 30 s recheck
and the voice re-check refuse an idle-expired session as they refuse a revoked credential, and
the socket closes within the recheck's 30 s. One `session_expired` audit row when either end is
reached. The web keeps it in `sessionStorage` (one tab, as today) and shows when it ends. A
second tab signs in again. `conch login --password` as an operator gets the same bounded
credential and says so; an operator who wants a long-lived token for scripts issues one
(`conch admin credential issue`, re-authenticated) and keeps it as the by-hand thing it is
(§7.4).

### 7.4 Re-authentication

**The class, defined positively (S13):** every operator route that **creates a principal, a
credential, a sign-in link or a hook, or grants a right or access, or removes a person**, needs
a re-authentication within the last **10 minutes** on a `session` credential. That is:

- creates a principal: `POST /v0/principals`, the operator's `POST /v1/principals/{id}/keep` (it
  makes a reset link);
- creates a credential or link: `POST /v1/principals/{id}/credentials`,
  `POST /v1/credentials/{id}/rotate`, `POST /v1/principals/{id}/sign-in-links`;
- creates a hook: `POST /v1/hooks`;
- grants a right or access: `PUT /v1/channels/{channel}/members/{principal_id}` (it adds a
  member and may grant `can_decide`, so it is **approval-path**: G2 and G4 carry the label and
  Nick merges), `PUT /v1/principals/{id}/invite-permission`, `PUT /v1/principals/{id}/manifest`
  (capabilities), `PUT /v1/channels/{channel}/open-joining`, `PUT /v1/join/settings`, any change
  to another operator (role, name, enable, disable, TOTP);
- removes a person: `POST /v1/principals/{id}/disable`, `/hide`, `POST /v1/principals/hide`,
  `POST /v1/join/disable-joiners`, `POST /v1/invites/{id}/disable-joiners`,
  `POST /v1/principals/{id}/disable-invitees`, `POST /v1/principals/{id}/credentials/revoke-all`
  for another principal;
- and replacing one's own TOTP or changing one's own password.

Checked in one helper the route table marks (`accessOperatorReauth`), and **the route-table test
is the guard**: it lists the class by the rule above and fails when an operator route that
creates or grants is marked otherwise, so a route added later cannot forget it. The answer
without it is 403 `reauth_required`; `POST /v1/auth/reauth` with the password and the code sets
`reauth_at` and audits `reauthenticated`; a wrong pair audits `reauth_refused` and counts as an
account failure in the per-name window of §6.4 (M3).

**A `token` credential is exempt** from re-authentication and from the session bounds *(decided
by the principal under Nick's delegation, S15)*: it has no password behind it to re-present, it
is the scripts' and the CLI's path, and bootstrap makes one. Two guards go with the exemption:
**issuing a token for an operator needs re-authentication**, and the create response says in
words that the token has no idle, re-authentication or absolute bound; and the operator's **"Your
sign-ins" lists tokens first, each with its age**, so a forgotten one is the first thing seen.
The offline reset revokes all of them.

### 7.5 Recovery and the host

`conchd bootstrap-operator`, `conchd setup-link` and `conchd reset-operator` run on the host with
`--data`, as bootstrap does today, and write their audit rows themselves (`actor
host:bootstrap-operator`, `host:setup-link`, `host:reset-operator`); `conchd` logs at next start
what was done offline since it last ran. **An operator's password and TOTP are only ever first
set through a link one of these made** (S12): `setup-link` for an existing operator who has
none, `reset-operator` (clearing both and, unless `--keep-credentials`, every credential;
ignoring `ErrLastOperator`) for one who lost them. Nothing online sets up or resets an operator.
The data directory is the root of trust; the deployment runbook already treats it so (backups,
permissions).

## 8. Add a device

A member-made sign-in link (#493's machinery, `sign_in_links` table, same code prefix `conch_sil_`,
same preview and redeem routes, same one answer for every bad code).

| | Operator's link (#493), now `purpose: reset` | Add a device, `purpose: sign_in` |
| --- | --- | --- |
| Made by | an operator, for a member | a member, for themselves |
| Route | `POST /v1/principals/{id}/sign-in-links` (re-authenticated) | `POST /v1/sign-in-links/mine` |
| Default life | 24 h | **10 min** |
| Maximum life | **24 h** (was 7 days: a link that can set a password lives a day at most) | **1 h** |
| Open at once | the operator's create bucket | the member's create bucket, **and at most 3 open self-made links** |
| Uses | one | one |
| May set a new password on the way in | yes (§3.7) | **no**: `new_password` is refused |
| For a guest, agent or operator | refused | refused (a guest has one credential by D2; an operator signs in by password and code) |
| Credential it makes | 90-day renewable, label "sign-in link <id>" | 90-day renewable, label "added device <id>" |
| Audit | `sign_in_link_created` (actor the operator) | `sign_in_link_created`, actor the member, detail `self=true expires_in=600` |
| Closing | `DELETE /v1/sign-in-links/{id}` (operator) | `DELETE /v1/sign-in-links/mine/{id}` (the member), and operators as before; a password change or reset closes them all |
| Shown | once, in the CLI | once, in the browser, behind **Reveal** (link, Copy, QR), or by `conch device-link` |
| Preview says | the name, `purpose: reset` | the name, `self: true`, `purpose: sign_in` |

The store's rule changes: `CreateSignInLink` admits a creator who is the target, with the
shorter bounds; `usableSignInLinkTx`, which today requires the creator to still be an operator,
accepts a creator who is an operator *or* whose id equals `principal_id` (a member who has since
been disabled closes their own links, as a disabled operator's are closed today);
`signInLinkTargetTx` is unchanged (members only) except for the offline `setup` link. Redemption
is otherwise byte-for-byte the #493 path, so the isolation check's existing exemptions for
preview and redeem still hold.

**On the page that makes it (S7 of the security review):** the link and QR sit behind a
**Reveal** button under the warning sentence, so a glance at the Settings view shows nothing
usable. While this tab is sharing its screen (the voice state knows), Reveal is disabled and the
page says "Stop sharing your screen before revealing a sign-in link." Leaving the view hides it
again, and the code is never kept in page state longer than the view is open.

**On the page that uses it (S6):** the preview's `self: true` makes the page say "This link
signs you in as <name>, who made it for their own device. If that is not you, close this page."
When this browser already holds a lasting sign-in for a different principal, the page requires
the person to type that `<name>` before it signs the other out (`onSignOutFirst`) and redeems:
a link a stranger follows should not quietly replace the stranger's own session.

**The QR code** *(adopted, two steps)*: the first PR ships the link and a Copy button, which
every device can use (paste into the phone's browser, or type the short URL). The QR is drawn
client-side in the second step, from a small QR encoder: either a hand-written one in
`web/src/qr.ts` (the standard is small; byte mode, error level M, versions 1 to 10 cover the
~100-character link and the `otpauth://` URI alike) or a dependency at an exact version with
Nick's sign-off in `web/deps-allowlist.txt`. The QR is an `<svg>` built from DOM nodes, no
`innerHTML`, so the hygiene test's rules hold. The QR carries the same link; nothing extra.

**The audit trail records**, in order: `sign_in_link_created` (self), then on use
`sign_in_link_redeemed` and `credential_created` (the label says "added device"), then, if the
member ends it, `credential_revoked`. "Your sign-ins" lists the new device with that label, which
is how a member learns that a link of theirs was used.

## 9. Abuse controls (D5) and the operator surface

**Rate limits.** The join and sign-in routes use the invite limiter (`invite_limits.go`), its
token buckets and its thinned audit, with buckets of their own where sharing one would let one
kind of traffic starve another:

| Route | Per address | Per account | Server-wide |
| --- | --- | --- | --- |
| `GET /v1/join` | its own `join_info_per_address` bucket (30 per 10 min), not the invite-preview one | | none |
| `POST /v1/join/guest`, `/v1/join/account` | the redeem-per-address bucket (10 per 10 min), shared with invite redemption: the same kind of act | | the redeem server-wide bucket (20 per minute, successes only), shared with invite redemption |
| the same two, on a `principal_exists` answer | a smaller **name-taken allowance**, `join_name_taken_per_address` (5 per hour), spent only by a taken-name refusal, so the bare address cannot be used to probe which names exist; past it the answer is 429 as for any limit | | none |
| `POST /v1/auth/login`, `/login/totp` | **its own** `password_login_per_address` (10 **failures** per 10 min; successes spend nothing), never the join or redeem one: a join flood never locks members out of signing in, and the reverse | the per-name window of §6.4 (M4): keyed by the typed name's folded key whether or not it exists, bounded like `bucketSet`, a not-before time answered 429 at once with no hashing; a wrong code counts like a wrong password (M3) | none (the anonymous hashing semaphore, 4 at once, is the global bound) |
| `POST /v1/auth/reauth`, `PUT /v1/password/mine` | | the same per-name window; a wrong pair counts (M3) | the authenticated hashing semaphore (2) |
| `POST /v1/sign-in-links/redeem` with `new_password` | the redeem buckets, as today | | as today |

Keys are the trusted-proxy client address (#609, `clientAddr`), with the same overflow behaviour
and the same rule that no address is ever audited or logged in full. **Behind a reverse proxy
with `--trusted-proxies` unset, every client is the proxy's address and shares one bucket**: the
first join or sign-in limit reached then stops everyone. The deployment README and the Caddy
file say to set it; `conchd` logs one warning at start when it serves `/app/` with no trusted
proxy and a non-loopback listener. Refusals audit `invite_rate_limited` with the limit's name,
as today; the design adds no second limiter.

**The guest cap.** `max_guests` (§4.6) counts guests that are live: `guest = 1`, not disabled, not
expired. `POST /v1/join/guest` and an invite redeemed `as: "guest"` check it inside the creating
transaction and answer `guests_full` (503, `Retry-After: 600`) past it; the refusal is audited
thinned as `guest_cap_refused`. The cap is not a rate: it is how many a server will hold at once.

**Pause.** `joining_paused` stops `POST /v1/join/*` (answer `joining_paused`) and leaves the
setting as it was, so resuming is one call. It does not stop password sign-in or invites, which
have their own switch. The operator's `conch admin join pause` is the CLI.

**Bulk revoke.** `POST /v1/join/disable-joiners` disables everyone with `origin = address`, dry
run first, operators and the caller never included, each through `DisablePrincipal` and
`principalDisabled` so sockets drop at once, with one `joiners_disabled` summary row with
`origin=address count=N`. `POST /v1/principals/hide` with `origin: "address"` or `guest: true`
(and `expired: true`) hides them, 500 at a time as #445 does. An invite's `disable-joiners` already
covers guests from that invite. **A kept account keeps `origin = address`**, so everyone who
came in from the bare address, guest or since-kept member, stays selectable by origin for bulk
disable and hide: Keep is not a way out of the net an operator can cast over a leaked door.

**Warnings in the operator surface** (UI and CLI alike): turning `join_voice` on says that
strangers will be able to join voice rooms of the open channels; marking a channel open when it
already holds messages says that everyone who joins from the address will read its history, and
asks for a confirmation (`--yes` in the CLI).

**Audit events**, all new names, all written in the same transaction as the change:

| Action | Actor | Subject | Detail |
| --- | --- | --- | --- |
| `open_joining_changed` | the operator | `settings:open_joining` | `from=off to=both voice=false max_guests=50` |
| `joining_paused`, `joining_resumed` | the operator | `settings:open_joining` | |
| `channel_open_joining_changed` | the operator | `channel:<id>` | `open_joining=true` |
| `principal_created` (existing) | `join:address` for a bare-address join (as `invite:<id>` is the actor of an invite's), `invite:<id>` otherwise | `principal:<id>` | gains `origin=address guest=true` |
| `guest_kept` | the guest, or an operator | `principal:<id>` | `origin=address|invite by=self|operator` |
| `guest_expired` | `system` | `principal:<id>` | `last_credential_expired_at=...` |
| `principal_hidden` (existing) | `system` | `principal:<id>` | gains `reason=guest_expired` when the sweeper hides |
| `guest_cap_refused` | `anonymous` | the route pattern | `max=50 count=N` (thinned) |
| `join_refused` | `anonymous`; `principal:<id>` for `reason=keep_closed`, which comes from an authenticated guest | the route pattern | `reason=closed|paused|name_reserved|name_taken|keep_closed` (thinned) |
| `joiners_disabled` (existing) | the operator | `origin:address` | `count=N` |
| `password_set`, `password_changed`, `password_reset`, `password_rehashed` | `principal:<id>`, or `sign_in_link:<id>` for a reset or a setup | `principal:<id>` | `revoked=<n>` other credentials; `first=true` on a first password; a rehash says `from=m=19456,t=2 to=m=65536,t=3` |
| `password_signed_in`, `operator_signed_in` | `principal:<id>` | `principal:<id>` | `credential=<id> kind=token|session scope=full|setup` |
| `password_refused` | `anonymous` | the route pattern | `reason=invalid|wait|ticket principal=<id>` (the id only when the name resolved; thinned); never the name |
| `password_hash_busy` | `anonymous` | the route pattern | `waiting=N` (thinned) |
| `totp_enrolled`, `totp_replaced`, `totp_cleared` | `principal:<id>`, or `host:reset-operator` | `principal:<id>` | |
| `reauthenticated`, `reauth_refused` | `principal:<id>` | `credential:<id>` | |
| `session_expired` | `system` | `credential:<id>` | `reason=idle|absolute` |
| `operator_setup_link_created`, `operator_reset` | `host:bootstrap-operator`, `host:reset-operator` | `principal:<id>` | `link=<id>` |
| `sign_in_link_created` (existing) | the member | `principal:<id>` | gains `self=true` and `purpose=` |

No event ever carries a code, a token, a password, a hash, a TOTP secret or a client address.

**The operator surface**, in the browser's operator views when they exist and in the CLI now:
the open-joining settings; the open flag on each channel; the list of people from the address
and of guests (with origin, expiry, whether a password is set); keep, with the reset link it
returns; bulk disable and hide; reset links; and the existing invite administration unchanged.

## 10. Threat notes

Each note says the threat, what in this design answers it, and what remains. They were written
for the `security-reviewer` pass #694 asks for, revised for passwords, and are to be re-reviewed
(§16).

**Account takeover.**
- *By guessing a password online:* the per-address bucket of failures, the per-name window with
  its not-before time (M4: keyed by the typed name whether it exists or not, bounded like the
  invite limiter's tables, answered at once without hashing), the hashing cost, the 12-character
  floor, the common-password list and the name rule; a wrong code or re-authentication counts
  like a wrong password (M3), so the second factor cannot be guessed at a different rate from
  the first. A credential-stuffing run with a leaked list gets ten failures per address per ten
  minutes and a growing wait per name. The `totp_required: true` answer tells an attacker the
  password was right before the code is asked; that is how every two-step sign-in works, the
  code step is held to the same window, and it is accepted. What remains: a weak but uncommon
  12-character password on a patient attacker's list; the person sees the sign-in in "Your
  sign-ins" and the operator sees `password_refused` rows.
- *By a stolen database (the hashes offline):* argon2id at 64 MiB and 3 passes makes each guess
  cost what a sign-in costs; the salt is per password; the parameters are in the string and can be
  raised. What remains: a common password is found anyway, which is what the list is for; a TOTP
  secret in the same database is readable, which is why the operator's password is also needed
  and why the reset is on the host.
- *By a stolen bearer (S8, password form):* a bearer taken from a browser cannot change the
  password without the current one (§3.11), cannot make a reset link (operators only, and
  re-authenticated), and is ended by the owner changing the password, which revokes every other
  credential. What remains: an account that predates passwords has no current password yet, so
  a stolen bearer could set the first one (§3.10) before its owner does; the set revokes every
  other credential, which signs the owner out and tells them, and the banner urges owners to set
  theirs now. The window closes the moment the owner does.
- *By device link:* 10 minutes, one use, shown only on the signed-in screen, the code in the URL
  fragment so no server or proxy log sees it (#440's rule), the same single-answer redeem, and no
  password reset through it. What remains: a person who shows the QR on a shared screen; the page
  says not to.
- *By reset link:* an operator's act, re-authenticated and audited; one use; 24 hours; the
  redeem revokes every other credential, so whoever else had the account is out. What remains: an
  operator who sends the link to the wrong person; the old owner notices only by being signed
  out, and asks.
- *By the bearer in the browser:* see "credential theft" below.

**Operator session theft.**
- One tab (D2 of #260), 12 hours absolute, 1 hour idle, re-authentication within 10 minutes for
  everything that grants access or removes people, a second factor on sign-in and on
  re-authentication, every sensitive act audited with the credential id. What remains: a `token`
  credential has none of this (§7.4) and is as exposed as today; operators are told to hold few,
  and the listing shows them.

**Guest-to-member escalation.**
- A guest cannot obtain a second credential by any route (`guest_not_allowed` on credentials,
  passwords, device links, operator links), cannot invite, cannot be granted the right, cannot
  request an approval (`guest_not_allowed` on `POST /v1/approvals`), and **never holds
  `can_decide`** (M1): the store forces 0 on every membership write for a guest, including one an
  operator makes by hand, so a guest an operator adds to a channel with approvals cannot decide
  them (`decision_not_permitted`). Keep is the one escalation, it is audited, it changes nothing
  but the mark, the password and the renewal, and it grants no channel, no voice, no invite right
  and no deciding right. An operator adding a guest to a channel is an operator's audited act. The
  isolation check (§12) asserts each refusal.
- *Keep as a door around a closed door (M2):* if an operator turns open joining off (or to
  `guest`), guests already in could otherwise still become lasting accounts. So a bare-address
  guest's self-Keep requires that open joining offers accounts and is not paused at that
  moment; otherwise `joining_closed` / `joining_paused`, audited thinned as `join_refused
  reason=keep_closed`. An operator's Keep bypasses it, and a guest from an invite keeps freely:
  someone chose to invite them.
- What remains: Keep means a guest can become a lasting member without any operator seeing it
  happen except in the audit log and the principal list (`origin` tells where they came from,
  and stays `address` after Keep so bulk disable and hide still reach them). That is D2's
  intent; the list and `conch admin principal list --origin address` make it visible.

**Name enumeration (S2).**
- `POST /v1/join/*` answers `principal_exists` for a taken name, as invite redemption does, so the
  bare address could be used to learn which names exist. Mitigations: a separate, smaller
  per-address allowance for that answer (5 per hour, §9), audited thinned as `join_refused
  reason=name_taken`; the reserved list is public, so `name_reserved` reveals nothing. The login
  route reveals nothing: one answer, uniform timing by the dummy hash. What remains: five probes
  an hour per address on the join routes; the operator sees them in the trail.

**Name squatting and look-alike names.**
- Case folding and UTS #39 skeletons already refuse look-alikes (#441). The reserved list refuses
  names that impersonate the system or staff.
- A name a guest held stays locked until the guest is hidden: at once if the guest posted
  nothing, 90 days after expiry if it posted (§4.3, R1). Expired guests do not count toward
  `max_guests`, so a party can lock more names than the cap by joining, posting one message and
  letting each guest expire: 10 names per address per 10 minutes, 20 a minute server-wide, each
  locked for 97 days. The per-address limits and the server-wide limit are what bound the rate;
  **bulk hide of expired guests** (`POST /v1/principals/hide` with `guest: true, expired: true`,
  or `conch admin principal hide --guest --expired`) is the operator's lever, and frees every
  locked name in one call, 500 at a time. Open joining off, or paused, stops the flow.
- What remains: an operator who never looks. The principal list with `--guest --expired` and
  the `guest_expired` rows in the trail are where it shows.

**Mass guest creation.**
- Per-address limits (10 joins per 10 minutes, a /64 for IPv6), the server-wide 20 per minute, the
  guest cap, the pause switch, bulk disable and hide by origin, and the 7-day expiry with a 90-day
  hide. Memory is bounded as #437 bounded it. A flood shows in the audit trail as thinned
  `invite_rate_limited` and `guest_cap_refused` rows.
- What remains: with open joining on, anyone on the internet can fill the guest cap. That is why
  it is off by default and why the cap is a server-wide count, not per address.

**Hashing as a denial-of-service lever.**
- Every password verification costs 64 MiB and 150–300 ms. Two semaphores (S14: 4 for anonymous
  sign-in, 2 for every authenticated or creating path) bound memory at 384 MiB and turn a flood
  into `busy` answers rather than swap; the per-address login bucket bounds how many
  verifications one network can ask for; the per-name not-before check refuses repeat guesses
  with no hashing at all; the dummy hash on unknown names goes through the sign-in semaphore, so
  a flood of unknown names costs the same and no more; and because the pools are separate, a
  sign-in flood never stops a Keep, a join or a password change from completing. What remains:
  **a flood from a large range of addresses degrades sign-in to `busy`** for everyone while it
  lasts; the levers are `--trusted-proxies` set correctly (so the per-address bucket sees real
  clients, §9) and a rate limit upstream in the proxy, and on a bigger host a higher
  `--password-hash-concurrency`.

**Credential theft from localStorage.**
- Unchanged in kind from O1 (#439): the CSP allows `script-src 'self'` only, the client ships no
  third-party script, and the hygiene test holds credential handling to `api.ts`. A guest's
  credential is the same bearer kept the same way, with a 7-day instead of 90-day renewal, so its
  exposure is shorter. Passwords reduce the cost of theft: a member can end the stolen sign-in in
  "Your sign-ins", or change the password, which ends every other sign-in, without an operator.
  The password itself is never stored by the page; the browser's own password manager may keep
  it, as for any site.
- What remains: an XSS in the client would still read the bearer; the mitigations are the CSP and
  the hygiene test, as before. An HttpOnly cookie was weighed and declined in O1.

**A password in clear on the network.**
- Over plain http the password is readable on the path. The page warns on an insecure,
  non-loopback origin; the runbook requires TLS beyond localhost or a trusted LAN; `conchd` warns
  at start on the unencrypted public shape. What remains: an operator who ignores all three; the
  design does not refuse to serve, since a LAN deployment is legitimate (§6.6).

**TOTP replay and theft.**
- A code is accepted once (`totp_last_counter`), within ±1 step; the login ticket is spent on the
  first try, so a ticket cannot be brute-forced across many codes; the secret is shown once at
  enrolment, never again. What remains: the secret is in the database in clear (§4.7), so a
  database thief with the password hash cracked has both; argon2id is the defence, and the host
  reset the recovery.

**Phishing of Add-a-device links.**
- A link signs its opener in as the member, so a member tricked into sending one away has handed
  over a sign-in. Mitigations: the life is 10 minutes; it is one use, so a used link is at once
  visible as a sign-in the member did not make ("added device <id>" in "Your sign-ins", which
  they can end); the page makes it for the member's own screen, keeps it behind Reveal, refuses
  to reveal it while the tab shares its screen (S7), and says so; a member can hold at most 3
  open; an operator can close any; a device link cannot reset the password. The preview shows the
  account's name, `self: true` and `purpose`, and the page that uses it says plainly whose device
  link this is and to close the page if that is not you (S6).
- *The other direction, a link planted on a stranger (S6):* a member's link sent to someone else
  signs that someone in as the member, which, in a browser that already holds the stranger's own
  session, would quietly replace it. So when a lasting credential for a different principal is
  stored, the page requires the person to type the link's name before signing the other out.
- What remains: social engineering within the 10 minutes. The sentences on both pages and the
  "Your sign-ins" listing are the answer; a second factor is out of scope for members.

**Login tickets and sessions as a resource (S10, password form).**
- Anonymous `POST /v1/auth/login` for an operator creates a ticket. The ticket table is bounded (8
  per client address, checked first; 1,000 in all; 5-minute TTL; evicted oldest-first), ids are
  32 random bytes, a ticket is bound to its principal and the client address, is spent on the
  first use, and is never accepted by any route but `login/totp`.

**The cross-group isolation boundary (D6).** See §12.

**Operator settings as surface.** Every switch is operator-only in the route table, stored in
`server_settings`, audited with old and new values, and read fail-closed (an unreadable
`open_joining` means off, as `invites_paused` reads as paused), and the ones that open a door
need re-authentication.

## 11. Rule 1

Nothing here needs a process besides `conchd`: joining, guests, the expiry sweeper, passwords
(argon2id in-process), TOTP (stdlib), device links, reset links, the offline operator commands.
TLS is a property of the deployment (Caddy in private-alpha.md O7), not a process the core
requires: without it everything still works, in clear, and the page says so. LiveKit stays
optional and unrelated. No mail server exists or is assumed, and the design says where that
leaves a person (§6.7).

## 12. The isolation check (D6)

`e2e/isolation` grows:

- **A third group, C, the guests.** Open joining on (`both`), one open channel; a guest `cora` and
  a member `carl` (with a password) from the bare address; a guest `gabe` from an A invite link
  redeemed `as: "guest"`. Group B is unchanged and remains the prober; C's guests are probed too,
  from inside their own narrow access. Every member the world makes by invite now sends a
  password, since a member redemption requires one.
- **Matrix rows** for every new route (coverage fails otherwise): `GET /v1/join` (anonymous, says
  nothing of any group: an exemption with the reason), `POST /v1/join/*` while on (makes C
  principals and nothing else), the settings and open-joining routes (operator-only refusals, and
  `reauth_required` on a session credential without a fresh re-authentication), `keep` on an A
  guest (404 as for anyone else's principal), `sign-in-links/mine` (answer only the caller's),
  the password and TOTP `mine` routes (the caller's own only; B cannot change A's password by any
  route, since no route takes another's id). `POST /v1/auth/login` and `/login/totp` are exempt
  with this reason: "takes a name and a password; B knows no password of A's, and every failure
  is the one `login_failed` with uniform timing; the uniform-answer, dummy-hash and slowdown rules,
  including B's password offered with A's name, are covered by G4's tests in
  `internal/server/passwords_test.go` (`TestLoginUniformAnswer`, `TestLoginUniformTiming`)".
- **A guest never decides (M1).** The operator adds `gabe` to a B channel that has a pending
  approval (an operator's act, audited); `gabe` reads it marked read-only and his decision answers
  `decision_not_permitted`; `PUT .../members/{gabe}` with `can_decide: true` answers
  `invalid_request`.
- **Keep follows the door (M2), and sets a password.** With open joining set to `guest`, `cora`'s
  self-Keep answers `joining_closed`; while paused, `joining_paused`; a Keep with a common
  password answers `password_weak` and leaves her a guest; the operator's Keep of `cora` succeeds
  and answers a reset link; `gabe` (invite guest) keeps freely with a password and then signs in
  with it from a fresh client. After Keep, `cora` still cannot invite (`invite_not_allowed`) or
  decide (`decision_not_permitted`), and `GET /v1/join/principals` still lists her by origin.
- **No approvals from guests (S5).** `cora`'s `POST /v1/approvals` answers `guest_not_allowed`.
- **A guest probes as a guest.** `gabe` (A's invite, guest) and `cora` (address, guest) each run the
  matrix against B's objects, and additionally against A's channels `gabe` was not granted: every
  answer the documented refusal, no marker of B or of A's other channel. Then `cora` tries every
  guest-refused route on its own objects: files upload, invite create, decide, device link,
  password set and change, TOTP, credentials; each is `guest_not_allowed` or the route's own
  refusal, and audited.
- **The bare address while off.** Before C is built, with `open_joining = off`: `GET /v1/join`
  says `off`, `POST /v1/join/guest` and `/account` answer `joining_closed`, and the principal count
  is unchanged. Repeated while paused (`joining_paused`).
- **Passwords across the boundary.** B signs in by password from a fresh client and sees only
  B's objects; a B member's password change revokes B's other credentials and no one else's (A's
  sockets stay open, timed); an A reset link made by the operator, redeemed with a new password,
  signs in as A alone and B's attempt to redeem it answers the one `sign_in_link_invalid` once it
  is spent.
- **Revocation, timed as A14 timed it:** `cora` holding every socket variant and a LiveKit
  participant (C gets voice in the check) is disabled by `POST /v1/join/disable-joiners`; then
  `carl`'s kept session; then `gabe` by A's invite's `disable-joiners`. Each within the existing
  bounds, and each stays out, and a disabled `carl`'s password answers `login_failed`. Then
  **expiry on the real path (S9)**: a guest is joined with a short credential lifetime (a
  `conchd` test option that lowers the guest lifetime below `MinCredentialRenewLifetimeSeconds`
  for the run, as `TestRelaxInviteLimits` relaxes the limiter), holding every socket variant and
  a LiveKit participant; the check waits for the expiry with no driven clock and measures that
  LiveKit stops listing the participant within the revocation bound (one poller pass plus a room
  rotation), that the sockets close within the sweeper's wake plus the 2 s socket bound (the 30 s
  `CredentialLive` recheck being the fallback the check does not rely on), that the credential
  answers 401, that `guest_expired` is written within seconds, and that the guest's messages
  still list in the channel for `carl` with the badge; a second guest that expires having posted
  nothing is hidden on the same pass and its name is reusable at once (R1).
- **The name stays until the hide (S3):** after `cora` expires, `POST /v1/join/account` with the
  name `cora` answers `principal_exists`; after the operator hides her (or the sweeper would,
  90 days on), it succeeds, and the hidden row shows as `hidden-<id>` in the operator's list.

## 13. The work, in order

Each line becomes one issue in the Private alpha milestone, filed after Nick signs ADR-009. The
drafts are in the scratchpad (`p694/issues.md`) for the principal to file.

| # | Work | Area | Size | Blocked by |
| --- | --- | --- | --- | --- |
| G1 | **Schema:** every shape, vocabulary, code and constant of §5, with golden fixtures and `ts.go` entries; `ReservedNames`; `PasswordRules`. protocol-designer. | schema | M | ADR-009 signed |
| G2 | **Server: guests and names.** Columns of §4.1, `origin` migration, the guest mark on every shape, guest refusals (files, approvals, `can_decide` forced 0, the password routes), the reserved list, `keep` with its door rule and its password (self) or reset link (operator), the expiry sweeper and 90-day hide, the live-credential voice entitlement, open channels flag; the member redemption's password. security-reviewer pass. | server | L | G1, G4 (for the password store) |
| G3 | **Server: open joining.** Settings, `GET /v1/join`, join routes (account with password), rate limits, guest cap, pause, joiners list, bulk disable and hide selectors, audit events; `as: guest` on invite redemption. security-reviewer pass. | server | M | G2 |
| G4 | **Server: passwords, operator sign-in, TOTP and re-authentication.** The dependency line (`x/crypto` v0.55.0, approved), the columns of §4.7, argon2id with the PHC string and rehash, the common list and generator, the rules, `login` with the uniform answer, dummy hash, limits and slowdown, the operator ticket and TOTP step, `session` credentials with idle and absolute ends, `reauth` and the `accessOperatorReauth` class, set and change password, the reset-on-redeem, `bootstrap-operator`'s setup link and `reset-operator`. security-reviewer pass. | server | L | G1 |
| G5 | **Server: Add a device.** Self-made sign-in links with the §8 bounds, `purpose`, `mine` routes, labels, audit. security-reviewer pass. | server | S | G1 |
| G6 | **Web: the front door, guests and Keep.** The bare-address page with the sign-in form and the http warning, join as guest, create an account with a password, the invite page's guest choice and password, the badge, the guest line and Keep with its password and incomplete-setup state, the "account is ready" line with "Sign out and try signing in", the sign-out warning, expired guests in the roster, the set-a-password banner. | web | L | G2, G3, G4 |
| G7 | **Web: sign-in, operator sign-in, passwords and Add a device.** Name-and-password sign-in with the one failure line and `Retry-After`, the operator's code step and session end, the re-authentication dialog, change password, the setup page (password, TOTP secret and URI as text, confirm), the reset-link page, the Add a device view with link and Copy, `conch login --link` documented on the page. The QR encoder is a follow-up issue (G7b, S) with its own dependency decision, serving both the device link and the `otpauth://` URI. | web | L | G4, G5, G6 |
| G8 | **CLI and TUI parity.** Every row of §5.4: `conch join`, `conch login --password` and `--link`, `conch passwd`, `conch totp`, `conch device-link`, `conch account keep`, the re-authentication prompt, the `conch admin join`, `channel open-joining`, `principal keep` groups, `conchd reset-operator`; the "(guest)" suffix and the set-a-password line in the TUI. | cli | M | G2, G3, G4, G5 |
| G9 | **Isolation e2e (D6).** §12: group C, the rows, the guest-as-prober runs, the off and paused checks, passwords across the boundary, timed revocation, expiry and name release. | e2e | M | G3, G4, G5 |
| G10 | **Web e2e.** One Playwright spec per flow of §3, with Nick's cases: invite as guest and as member with a password, bare address off and on, sign in by password, a refused password, a cancelled Keep, Keep then sign out and sign back in with identity, channels and history kept, operator sign-in with TOTP (a test authenticator from the shown secret), the re-authentication dialog, session end, change password ending another context, the reset link, the banner on a pre-password account, Add a device across two contexts, the `(guest)` badge. | web | L | G6, G7 |

G1 first; G4 and G5 can run in parallel once G1 merges; G2 after G4's store (its password
routines), G3 after G2; G6 after G3 and G4; G7 after G4, G5 and G6; G8 after the servers; G9
and G10 last. Every server PR is `security` and gets the reviewer; G1 is `area/schema`; none
touches the approval path. G4 is the one with the dependency line, and the PR that adds it
carries Nick's recorded approval.

## 14. Open points

Things this note decided under the standing delegation, or could not decide; each with the
recommendation it took. None reopens D1–D6 or Nick's three review points.

1. **`x/crypto` v0.55.0 is approved**, and Go stays at 1.25.0. Nothing is open here; the later
   passkey phase (§15) will have to face the Go 1.26 question again, since `x/crypto` v0.56.0+
   and `go-webauthn` v0.18.1+ both require it.
2. **Expired guests and their names: resolved by the security review (S3, R1, §4.3).** D2 wants
   "<name> (guest)" on old messages; D4 wants names reusable. The first draft released the name
   at expiry with a `shown_name` column and an `expired-` prefix; the review chose the simpler
   rule, and it is adopted: a guest that posted keeps its name until the sweeper hides it 90 days
   after expiry; one that posted nothing is hidden at once. An operator can hide earlier.
3. **Guests cannot upload files.** Adopted to bound storage abuse; one line to flip if Nick wants
   guests to share files.
4. **TOTP is operators-only in this phase.** Members have a password and Add a device; making
   TOTP optional for members is additive, later.
5. **The QR code is a second step** (§8): the link and Copy ship first, and the TOTP secret and
   URI are shown as text; the encoder is a hand-written `qr.ts` or a signed-off dependency, and
   serves both.
6. **A member redemption now requires a password.** The `password` field is optional in
   `RedeemInviteRequestV1` (compatible), but the server refuses `as: "member"` without it
   (`password_required`): a tightening of a published route's rule, not of its shape. The
   schema-change skill counts a semantic change as breaking; this note's position is that the
   route gains a precondition clients can see in the policy and the error, with the CLI, the web
   and the e2e harnesses updated in the same PRs, and asks Nick to confirm with the ADR. The
   alternative, a `POST /v2/invites/redeem`, is one route more for no behavioural gain.
7. **Done.** `ROADMAP.md` (the alpha paragraph) and `private-alpha.md` §3 and O2 now carry a
   one-line pointer to ADR-009, edited by the principal on 2026-10-21 in the same PR.
8. **The 90-day hide of expired guests** is a default, not a setting; it keeps the principal list
   from filling with dead guests. A setting can follow if an operator asks.
9. **Decided (S15, principal under Nick's delegation, 2026-10-21):** `token` credentials stay
   exempt from re-authentication and the session bounds (§7.4), with two guards: issuing a token
   for an operator needs re-authentication and the response says the token has no bound, and the
   operator's "Your sign-ins" lists tokens first with their age. Closed.
10. **The pre-password window** (§10, "by a stolen bearer"): an account that predates passwords
    can have its first password set by whoever holds a live bearer for it. The set revokes every
    other credential, which is the owner's signal. The alternative, requiring an operator's reset
    link for the first password of every old account, is more work for the operator than the
    alpha's handful of accounts warrants; one line to flip.
11. **Guest channels and later changes:** a channel made open after a guest joined does not add
    existing guests; an operator adds them by hand. Simple and audited; automatic membership can
    come later if it is missed.
12. **Recovery codes: later** (§6.8), with passkeys.
13. **`JoinResponseV1` versus reusing `RedeemInviteResponseV1`.** The fields are identical; the
    schema issue decides whether a join answers with the invite's shape by name or with its own.

## 15. Later: passkeys

Deferred by Nick on 2026-10-21. Kept here so the later phase starts from what was measured and
reviewed, not from scratch. Nothing in §1–§14 depends on it.

**The library, measured on 2026-10-21:** `github.com/go-webauthn/webauthn` v0.18.2 (2026-09-19,
BSD-3-Clause), the maintained successor of `duo-labs/webauthn`. Measured in a throwaway module
with a `main` that calls `webauthn.New`, after `go mod tidy`:

| Module | Version | Licence | Why it is there |
| --- | --- | --- | --- |
| `github.com/go-webauthn/webauthn` | v0.18.2 | BSD-3-Clause | the library |
| `github.com/go-webauthn/x` | v0.3.1 | BSD-3-Clause | its helpers (revocation lists, metadata) |
| `github.com/fxamacker/cbor/v2` | v2.9.4 | MIT | CBOR, the attestation and authenticator-data encoding |
| `github.com/x448/float16` | v0.8.4 | MIT | used by cbor |
| `github.com/go-viper/mapstructure/v2` | v2.5.0 | MIT | decoding attestation statements |
| `github.com/golang-jwt/jwt/v5` | v5.3.1 | MIT | the Android SafetyNet attestation format |
| `github.com/google/go-tpm` | v0.9.8 | Apache-2.0 | the TPM attestation format |
| `github.com/tinylib/msgp` | v1.6.4 | MIT | used by go-tpm |
| `github.com/philhofer/fwd` | v1.2.0 | MIT | used by msgp |
| `github.com/google/uuid` | v1.6.0 | BSD-3-Clause | already in `go.mod` (indirect), same version |
| `golang.org/x/crypto` | v0.57.0 | BSD-3-Clause | now direct at v0.55.0; would bump |
| `golang.org/x/sys` | v0.48.0 | BSD-3-Clause | already in `go.mod` (indirect) at v0.47.0; would bump |

Twelve modules; the attestation formats are compiled in whether or not used (Conch would ask for
attestation `none`). `v0.18.1` and `v0.18.2` declare `go 1.26.0`, so taking either bumps `go.mod`
to 1.26.0 and may bump the lint pin; `v0.18.0` (2026-08-27) stays on 1.25.0 but lacks the
"validate session challenge when finishing ceremonies" fix (#794), which the server would then
have to do itself. That decision is the later phase's.

**What was designed and reviewed, to carry over:** discoverable credentials with user
verification `preferred` and attestation `none`; a 32-byte random user handle per principal,
never the row id; sessions of 32 random bytes bound to the principal that began them, a register
session never accepted by login nor the reverse, capped per address (8) before per principal (4)
and global (10,000), 5-minute TTL (S10); the counter rule "refuse when `new <= stored` unless both
are 0"; `login/finish` re-reading the passkey and principal inside the immediate transaction
(S4); disable and hide revoking passkeys with a reason, enable reviving none (S4); every passkey
recording the credential that registered it, shown as "added from <label> on <date>", with
sign-out-everywhere offering to remove passkeys added in the last 24 hours and listing them
(S8, N5); passkey sign-in on its own server-wide bucket (S1); an RP ID setting with origins that
name non-443 ports, refused at start when an origin's host is not the RP ID or a subdomain; and
the places passkeys cannot work (a bare IP cannot be an RP ID; a LAN name without TLS is not a
secure context), where the password and Add a device remain the ways in. With passwords in
place, a passkey becomes a second, phishing-resistant way to sign in, and re-authentication
before adding one ("touch a passkey to add a passkey") is the residual to close then.

## 16. Security review and revisions

**Reviewed** by the `security-reviewer` agent on 2026-10-21, on the first draft of this note; the
principal decided every finding, and all are adopted and folded into the sections above:

- **M1** (§4.1, §4.2, §10, §12): a guest never holds `can_decide`; the store forces 0 on every
  membership write for a guest and refuses `can_decide: true` on one.
- **M2** (§3.6, §9, §10, §12): a bare-address guest's self-Keep requires open joining to offer
  accounts and not be paused; operator Keep bypasses; invite guests keep freely; kept accounts
  stay selectable by origin.
- **S1** (§5.3, §9): `GET /v1/join` and sign-in have buckets of their own; sign-in never spends
  the join or redeem server-wide bucket. (Written for passkey sign-in; applied to password
  sign-in.)
- **S2** (§9, §10): a smaller per-address allowance for `principal_exists` on the join routes,
  audited `join_refused reason=name_taken`; the name-enumeration threat note.
- **S3** (§4.3, §4.4, §4.8, §14.2): an expired guest's name is released only by the hide;
  `shown_name`, the `expired-` prefix and the `claimNameTx` exception are dropped.
- **S4** (§6.5, §15): written as "disable and hide revoke passkeys; login re-reads the principal
  in the transaction"; its password form is in §6.5 (disable and hide revoke every credential, a
  disabled principal's password login is refused inside the transaction) and its passkey form is
  kept in §15.
- **S5** (§4.2, §5.3, §12): `POST /v1/approvals` is `guest_not_allowed` for a guest.
- **S6** (§3.5, §5.1, §8, §10): the preview of a self-made link carries `self: true`; the page
  says whose device link it is; typing the name before signing another person out.
- **S7** (§3.5, §8, §10): the link and QR sit behind Reveal and are not revealed while the tab
  shares its screen.
- **S8** (§10, §15): written as "a passkey records the credential that added it"; its password
  form is "a stolen bearer cannot change the password without the current one" (§3.11, §10, with
  the pre-password window as §14.10); its passkey form is kept in §15.
- **S9** (§4.1, §4.3, §12): the voice entitlement requires a live credential for a guest, so
  LiveKit cuts an expired guest within one poller pass; the sweeper runs at the earliest live
  guest credential's expiry; the isolation check measures expiry on the real path.
- **S10** (§6.4, §10, §15): written for passkey sessions; its password form is the operator login
  ticket (32 random bytes, bound to principal and address, spent on first use, capped per address
  before global); its passkey form is kept in §15.
- **Nits**, all adopted: `usableSignInLinkTx` accepts the self-made case (§8); guest lifetime is
  `min(invite lifetime, 7 days)` and Keep on an invite guest uses the invite's lifetime (§3.6,
  §4.1); the RP-origins and counter-rule nits are kept with passkeys in §15; enable on an
  expired guest is refused (§4.3, §5.3); the audit actor of a bare-address join is
  `join:address`, `guest_kept` carries `origin`, `join_refused` gains `name_taken` and
  `keep_closed` (§9); every new authenticated route answers 401 under `AuthOff` (§5.3); warnings
  for `join_voice` and for opening a channel with history (§9); D2's "one browser" is "exactly
  one credential" (§2, §4.1); the sign-in exemption in the isolation check points at G4's tests,
  and rows check that a kept account still cannot invite or decide (§12); `conch login --link`
  reads from standard input without echo (§3.5, §5.4); one line on `--trusted-proxies` unset
  behind a proxy (§9).

**Re-checked** on 2026-10-21 after the above: PASS, with one residual and four nits, all adopted:

- **R1** (§4.3, §10): an expired guest that posted no message is hidden on the same sweeper pass
  and its name freed at once; one that posted is hidden 90 days on; the name-squatting note is
  rewritten for the post-S3 rule and names bulk hide of expired guests as the operator's lever.
- **N1** (G2 files): the approval refusal lives in `internal/server/approvals_handlers.go`.
- **N2** (G4 files): the revocation hook goes into `disablePrincipalTx` and `HidePrincipal`
  (`internal/server/store/disable.go`, `hide.go`), which G2 also edits.
- **N3** (§4.3, G2, G9): the socket bound at expiry is the sweeper's wake plus 2 s, with the
  sockets' 30 s `CredentialLive` recheck in `internal/server/ws.go` as the fallback.
- **N4** (§9): `join_refused reason=keep_closed` is audited with actor `principal:<id>`.
- **N5** (§15): the "remove passkeys added in the last 24 hours" checkbox lists the passkeys it
  would remove, by label and "added from" (kept with passkeys).

**Revision 2026-10-21 (owner review of PR #701).** Nick asked for (1) no account without a usable
return login, (2) a repeatable operator sign-in with stronger requirements, and (3) the
unavailable and recovery experience spelt out, and then decided "passwords first". The principal
turned that into D3 revised and the three points of §2, and this note was rewritten to match:
passwords (§6) replace passkeys as the primary method, passkeys move to §15, operators get
§3.8–§3.9 and §7, every account is created or kept with a password (§3.1, §3.3, §3.6), accounts
from before get the banner (§3.10), the reset link (§3.7), change (§3.11), re-authentication
(§3.12), the connection warning and the stated limitation (§6.6, §6.7), recovery codes evaluated
(§6.8), and G1–G10 reshaped (§13). Nick approved `golang.org/x/crypto` the same day (§6.1).
Passkey-specific security fixes (S4, S8, S10, the counter rule, the RP-origins nits, N5) are
kept in §15 and given their password equivalents in §6.4, §6.5 and §10.

**Security re-review of the passwords revision, 2026-10-21.** Two blocking findings, six
should-fixes and a set of nits; the principal adopted all, as the reviewer proposed:

- **M3** (§3.8, §3.12, §6.4, §7.4, §9): a failed TOTP step and a failed re-authentication count
  as account failures exactly like a wrong password, with the same slowdown; the window resets
  only when a credential is issued, never at the password step.
- **M4** (§3.4, §6.4, §9, §10): the per-name window is keyed by the typed name's folded key
  whether or not a principal exists, bounded like `bucketSet`, and holds a not-before time
  answered 429 with `Retry-After` at once and without hashing; G4's uniform-answer and timing
  tests run the sixth attempt on an existing and an unknown name.
- **S11** (§3.9, §4.7, §5.3): redeeming a setup link with `new_password` spends it at once and
  answers a 15-minute `session` scoped to `/v1/totp/mine/*` and `GET /v1/whoami`, `reauth_at`
  now; password sign-in stays refused until TOTP is confirmed; an abandoned setup means running
  the host command again.
- **S12** (§3.9, §3.10, §5.3, §7.5): operators enrol only through a setup link made on the host
  (`conchd setup-link`, or `reset-operator --keep-credentials`); `POST /v1/password/mine` and
  `/v1/totp/mine/enrol` on a bearer answer `setup_on_host` for an operator without a password.
- **S13** (§3.12, §5.3, §7.4): the re-authentication class is defined positively (every operator
  route that creates a principal, credential, link or hook, or grants a right or access, or
  removes a person) and gains `PUT .../members/{principal_id}` (approval-path, since it grants
  `can_decide`), `invite-permission`, `open-joining`, `POST /v0/principals`, `manifest`,
  `POST /v1/hooks` and the operator's `keep`; the route-table test is the guard.
- **S14** (§6.2, §10): two hashing semaphores, anonymous sign-in on one and every authenticated
  or creating path on the other; §10 says a large-range sign-in flood degrades sign-in to `busy`
  and names `--trusted-proxies` and an upstream rate limit as the levers; G4's flood test asserts
  a Keep completes during a sign-in flood.
- **S15** (§7.4, §14.9): operator `token` credentials stay exempt, with two guards: issuing one
  needs re-authentication and the response says it has no bound; "Your sign-ins" lists tokens
  first with their age. Decided by the principal under Nick's delegation.
- **S16** (§4.7, §7.3): `CredentialLive` applies the idle rule for `kind = session`; an open
  socket does not bump `last_used_at`; WS tickets and the voice re-check refuse an idle-expired
  session.
- **Nits**, all adopted: `reset-operator` ignores `ErrLastOperator` (§3.9, §7.5); login looks up
  by `name_key` only (§3.4, §6.4); the dummy hash uses the lowest parameters still present in
  `principals` (§6.4); "contains the name" applies only when the name key is 4 characters or
  more, "equals" always (§6.3); `password_refused` carries `principal=<id>` when the name
  resolved and `password_set` carries `first=true` (§9); every response carrying a token, ticket
  or TOTP secret goes through `writeSecretJSON` (§5); re-authentication and enrolment
  confirmation share `totp_last_counter` with sign-in (§7.2); `purpose: reset` links have a 24 h
  maximum (§6.5, §8); the per-address login bucket counts failures only (§6.4, §9); under
  `AuthOff`, `POST /v1/auth/login` answers 404 (§5.3); the idle check uses
  `max(last_used_at, created_at)` (§4.7, §7.3); G4's timing test sits behind an integration flag;
  §10 notes that `totp_required: true` reveals the password was right, which is standard and
  accepted.

**Final check, 2026-10-21: PASS.** Two residual nits, adopted by the principal: the setup scope is enforced in `authMiddleware`, never per handler (§5.3); successful sign-ins are capped at 30 per hour per name key (§6.4).
