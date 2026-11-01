# Design: optional push delivery and notification privacy for the phone app (M3, issue #1108)

- **Status:** Proposed by the protocol-designer, 2026-11-01. Design only: no code, no dependency, no schema edit. Every wire need is named as a future `pkg/schema` issue (§9). The decisions that need the owner are collected in §11 and nothing in this note is effective until he answers them.
- **Issues:** parent #757 (Mobile applications); this note is M3. It feeds #1116 (server push registration and delivery adapter) and #1117 (phone permissions, registration and tap routing), which stay blocked on it and on the M1 stack decision (#1106, #1111).
- **Sibling notes, by path only (other workers, in flight):** [mobile-app.md](mobile-app.md) and the proposed mobile ADR (M1, #1106); [mobile-app-ux.md](mobile-app-ux.md) and its mockups (M2, #1107). Where this note depends on a choice they make (the stack, the server-profile screen, the settings screen), it says so and does not make the choice.
- **Governed by:** [ADR-000](../adr/ADR-000-charter.md) D3 (deployment invariant) and D7 (approval notifications go to ntfy; decisions only via `conch`; unchanged here); [ADR-002](../adr/ADR-002-single-binary-sqlite.md) (integrations are optional and degrade gracefully); [ADR-013](../adr/ADR-013-one-community-model.md) decisions 1 and 9 (private channels are a 404 to non-members; `hide_members`); [ADR-005](../adr/ADR-005-nets-and-whispers.md) (every read path applies the audience predicate; "one miss leaks whispers"); [ADR-007](../adr/ADR-007-client-interfaces.md) (clients are clients of the public API; platform adapters for notifications); [ADR-008](../adr/ADR-008-private-communications-alpha.md); [client-experience.md](client-experience.md) §4 ("No private message content in a notification after the relevant access is lost").
- **Builds on:** the browser notification preferences of #840 (`web/src/useNotifications.ts`, `web/src/NotificationSettings.tsx`, `web/src/api.ts`); the approval notifier (`internal/server/approvals/ntfy.go`, [approval-object.md](approval-object.md) §5); credentials, sign-outs and device links (`internal/server/credentials.go`, `internal/server/store/credentials.go`, [guests-and-accounts.md](guests-and-accounts.md)); the content-free events documents (`conch.channels_changed.v1`, [membership-live.md](membership-live.md); `conch.directory_changed.v1`, [community-model.md §13.6](community-model.md)); the visibility predicates (`internal/server/store/audience.go`, community-model.md §5 and §13.2).
- **Verified against:** `main` at `7472b65` (2026-11-01). Each claim about the repository below is marked **[verified: path]**; each claim about a provider is marked **[source]** with the page it was read from on 2026-11-01 (§14); anything neither is marked **[assumption]** and says what would settle it. Nothing here was run against a real provider or a physical phone.

A person on a phone should learn that someone mentioned them, or wrote in a channel they care about, while the app is not on screen, without the server, Apple, Google or anyone in between learning more than that a push happened. That has to hold on a managed Conch server and on one somebody runs at home with nothing but `conchd`, and it has to cost nothing when the operator sets nothing up: the app then works exactly as the browser does today, catching up when it is opened.

## 0. The design in one table

| # | Question | Decision | Section |
| --- | --- | --- | --- |
| N1 | Is push required for anything? | No. Push is an **optional integration** like ntfy and LiveKit. Off by default, nothing in messaging, approvals or audit depends on it, and a provider outage changes nothing but the phone's promptness. The always-available path is **fetch on open**. | §2, §6.6 |
| N2 | What does a push carry? | A **wake**: an opaque registration id and nothing else. No server address, channel, message id, author, text, count or kind. The phone, holding a credential, fetches what it may see and renders locally. | §4 |
| N3 | Which transports? | Two outbound transports in `conchd`, both one small HTTPS POST: **Web Push** (RFC 8030/8291/8292; serves UnifiedPush distributors and browser push services) and the **Conch push relay** (a separate program that holds APNs and FCM keys; run by the project as a managed relay, or by an operator who signs their own app build). Direct APNs/FCM inside `conchd` is deferred. | §3, §3.6 |
| N4 | Who holds the provider keys? | Whoever signs the app build: provider keys bind to the app publisher, not to the server operator. That is why a relay exists at all. An independently self-hosted server never needs provider keys. | §3.2 |
| N5 | What is a registration? | One row per (app install, server, account), **bound to the credential** that made it. It dies with that credential: sign-out, revoke, rotate, expiry, revoke-all, disable. Nothing survives a sign-out. | §5 |
| N6 | Where do preferences live? | **On the server, per credential (per sign-in), independent of any push registration**: off / mentions / all, channel mutes, a text-preview switch (off) and a quiet switch (off). A phone on a server with no push transport reads and writes the same object for its in-app alerts; a registration is only an address to wake. The server decides whether to send; the phone decides nothing about visibility. The browser's preferences stay where they are (#840) and are not migrated here. | §6.1 |
| N7 | How is content kept inside its boundaries? | Nothing is queued per device. The pending feed is computed **at fetch time** through the same store predicates the REST routes use (audience, membership, `hide_members`); a lost membership means an empty answer. The wake is never enriched server-side. | §4.3, §8 |
| N8 | Recall and offline delivery? | Stated as limits, not promises: a wake delivered before access was lost shows at most a generic line; a summary rendered locally cannot be recalled with certainty; providers store at most one collapsed wake per app for a bounded time. | §4.5, §7 |
| N9 | Approvals? | Unchanged. D7's ntfy topics stay the approval channel and no approval decision happens on the phone. A later approval-path issue may add an approval *kind* to the feed; it is not in this design. | §2.2 |

## 1. What exists today

Everything in this section was read in the tree at `7472b65`.

### 1.1 Browser notifications (#840): the semantics to reuse

- **Policy** is `'off' | 'mentions' | 'all'`, **default `off`** ("Notifications are off until you choose a policy") **[verified: web/src/api.ts `NotificationPolicy`, `emptyNotificationPreferences`; web/src/NotificationSettings.tsx]**.
- **Channel mutes** are a list of channel ids. "Channel mutes hide alerts, not unread badges" **[verified: NotificationSettings.tsx hint; useNotifications.ts `deliver`]**.
- **Mention** is `@<display name>` as a token: preceded by start of text or whitespace/punctuation, followed by the same or the end, case-insensitive; an empty name never matches **[verified: useNotifications.ts `mentions()`]**. The composer's completion offers only names already on the page **[verified: web/src/mention.ts]**.
- **A message from the reader themself never notifies**, nor does one in a channel the page does not hold; an **edit never notifies** and only updates an alert in place; delivery is deduplicated by `origin:account:message id` and capped at ten alerts **[verified: useNotifications.ts]**.
- **Native (OS) notifications** show only when the tab is hidden, after an explicit "Enable browser notifications" click that asks the browser's permission; the title is `Mention in #name` or `New message in #name` and the body is a 160-character snippet of the text **[verified: useNotifications.ts `deliver`, `snippet`]**. That is, the browser today **does** put message text in an OS notification, on the person's own machine, which never leaves it.
- **Catch-up:** on reconnect the hook re-reads each channel from its last seen id and delivers what arrived meanwhile **[verified: useNotifications.ts, the `eventsLive` effect]**. The phone's fetch-on-open (§4.3) is this behaviour, named.
- **Preferences are per client**, keyed `conch.web.notifications.v1.<origin>.<account>` in the platform's preference store, never sent to the server **[verified: api.ts `notificationPreferencesKey`, `writeNotificationPreferences`]**. There is **no server-side notification preference and no server-side mention matcher**: `grep -ri mention internal/server pkg/schema` finds only `_test.go` files (fixture text and comments), no non-test matcher **[verified]**.
- Access loss closes alerts: an alert for a channel no longer in the list is closed, and a read-state frame with `unread: 0` closes the channel's alerts **[verified: useNotifications.ts]**.

### 1.2 The approval notifier (D7): the integration pattern to copy

- ntfy is configured by `--ntfy-server`, `--ntfy-topic`, `--ntfy-urgent-topic` (env `CONCHD_NTFY_*`); with no server set, `NewNtfyNotifier` returns nil and nothing is sent **[verified: cmd/conchd/main.go; internal/server/approvals/ntfy.go]**.
- A notification is one HTTP POST with a 2 s default timeout; failures are reduced to a fixed vocabulary of causes so that no URL or topic reaches a log or audit row (`transportCause`); header and body are sanitised and the body is cut at 3,800 bytes (`maxBodyBytes`, `ntfy.go:207`), below ntfy's 4,096-byte attachment threshold, with a line saying it was cut **[verified: ntfy.go]**.
- Every transition gets an audit row, `notify_sent` or `notify_failed`, and a start catches up what a stopped process left undone **[verified: approvals.go; approval-object.md §5]**. Approval notifications **carry content** (title, body, requester, channel id) to a topic the operator chose; that is D7's trade, made for approvals, and this note does not reuse it for chat (§3.5).

### 1.3 Credentials, sign-outs and the things that die with them

- A credential is one bearer token for one principal; the token is shown once; a credential may expire, renew on use, or carry an idle bound **[verified: pkg/schema/credential.go]**.
- Sign-out is `DELETE /v1/credentials/current`; a person lists and ends their own sign-ins at `/v1/credentials/mine`; an operator revokes, rotates, or revokes all **[verified: internal/server/credentials.go; pkg/schema/revoke_all.go]**.
- Revoking a credential, in the same transaction, closes the principal's open **device links**, "a sign-out ends the ways in it would otherwise leave behind" (#712); afterwards every socket opened with it is dropped and its voice session rotated **[verified: store/credentials.go `revokeCredential`, `RevokeOwnCredential`; credentials.go `credentialRevoked`]**. A push registration is one more such way in (§5.4).
- A browser gets a socket with a single-use 30 s ticket because a bearer never goes in a URL **[verified: pkg/schema/ws_ticket.go]**.

### 1.4 Visibility, and the content-free documents

- Messages, files, runs and activity are read only through `ListVisibleMessages` and its siblings, which take a `Reader` and apply the audience predicate (channel-wide, the reader's nets, the reader's whispers) **[verified: internal/server/store/audience.go]**. The events socket builds every frame in the subscriber's goroutine with those same functions, so "a document the reader could not fetch over REST then is not sent" **[verified: internal/server/events_ws.go header]**.
- A private channel answers the same 404 to a non-member as a channel that never existed; `hide_members` adds one predicate, `knowsTx`, that every member-facing read of principals goes through, and "an unknown principal gets the same answer as one that never existed" **[verified by reading: community-model.md §5, §13.2; ADR-013 decision 9]**. Whether `knowsTx` exists under that name on main was not checked; the rule is what this note relies on.
- `conch.channels_changed.v1` is `{"schema":"conch.channels_changed.v1"}` and nothing more, because "a frame with no content cannot leak a name, through a race or otherwise" **[verified: pkg/schema/channels_changed.go; membership-live.md §1]**. The wake of §4 is the same idea sent through someone else's network.
- Read state belongs to the principal, not to a session or credential, and the unread count is of messages the reader may see **[verified: pkg/schema/read_state.go]**.

### 1.5 Dependencies and tooling facts that bound the design

- `go 1.25.0`; `golang.org/x/crypto` is already on the allowlist **[verified: go.mod; deps-allowlist.txt]**. Web Push encryption (RFC 8291: ECDH P-256, HKDF, AES-128-GCM), the VAPID JWT (ES256) and the APNs JWT (ES256) are all reachable with the standard library (`crypto/ecdh`, `crypto/hkdf`, `crypto/ecdsa`, `crypto/aes`, `net/http` with HTTP/2) **[assumption: to be confirmed by #1116 when it is written; any import outside the standard library and the allowlist goes through `scripts/depgate.sh` and needs Nick's sign-off]**.
- Rate limiting of unauthenticated and account routes exists (`internal/server/ratelimit.go`) **[verified: file present; its API not read]**; registrations reuse it (§5.7).

## 2. Scope and the three paths that must stay distinct

### 2.1 Goals

1. A member whose phone app is not on screen learns, promptly, that something they chose to be told about happened, on any Conch server they are signed into.
2. No party outside the server and the phone learns anything of the content, the channel, the people or the server from the push.
3. A server with no push configured, a provider outage, a phone with no Google services, a phone with push denied: in each case the app works, and tells the person what they will not get.
4. Nothing an operator must run, buy or register to have a working server.

### 2.2 Non-goals (this design, or at all)

- **Approval decisions on the phone**, or any change to D7. The ntfy approval topics continue exactly as today. Adding an approval kind to the phone's feed is a later, separate, `approval-path` issue (§10, deferred D-4).
- **Replacing the browser's preferences** or moving them server-side (§6.1, deferred D-5).
- **Rich previews by default**, images, inline reply, badges, or notification actions (§10).
- **Guaranteed delivery.** No provider offers it (§7), and this note does not claim it anywhere.
- **E2EE of message content.** ADR-000's non-goal stands; the wake carries no content to encrypt (Web Push bodies are encrypted anyway because the RFC requires it).

### 2.3 Three paths

| Path | When | Carries | Governed by | Changes here |
| --- | --- | --- | --- | --- |
| Foreground events socket (`/v1/events/ws`, `/v2/ws`) | app on screen, connected | full documents, under the reader's predicates | ADR-005, events_ws.go | none; the phone uses it as the browser does |
| Approval notifications (ntfy) | approval created / escalated / resolved / expired | title, body, requester, deadline, to an operator topic | D7, approval-object.md §5 | none |
| **Mobile push (this note)** | app suspended, terminated or force-stopped, and a registration exists | a content-free wake | §4 | new, optional |

The phone must never use push while the socket is live, and the server must not send a wake to a registration whose credential currently holds an open events socket for that channel, except that this optimisation is **not** a privacy control and may be skipped in the first slice (§5.6).

## 3. Options compared

### 3.1 The candidates

| Option | How it works | Who operates what | Trust boundary and what each party sees | Dependencies and cost | iOS | Android |
| --- | --- | --- | --- | --- | --- | --- |
| **O1 No push; fetch on open** | The app re-reads channels and read state when opened or resumed, as the browser does on reconnect. | Nobody. | Server and phone only. | None. | yes | yes |
| **O2 Direct APNs/FCM from `conchd`, operator-owned keys** | `conchd` holds an APNs token key (`.p8`) and an FCM service-account key and posts to Apple and Google itself. | The operator, who must also be the app's publisher: an APNs key signs for the team's topics (bundle ids), a Firebase project is tied to the app's package. | Apple/Google see the operator's IP (which names the server), the device token, timing, size, and the payload (opaque under §4). The operator holds provider secrets in the data dir. | Apple Developer Program, US$99/year **[source: Apple membership pages via search, §14]**; Google Play developer account US$25 once **[source: Play Console help]**; FCM itself has no per-message charge **[assumption: firebase.google.com/pricing not fetched]**; an operator-built, operator-signed app on every phone. | only for operators who sign their own build | same |
| **O3 Managed relay (project-run)** | `conchd` posts the wake to a relay the project runs; the relay holds the keys for the store-published app and forwards to APNs/FCM. The Matrix push gateway is the precedent **[source: Matrix push gateway API]**. | The Conch project (Nick) operates the relay and the developer accounts; the operator only sets a relay URL. | The relay sees: which servers push (by relay token and source IP), device tokens, timing, counts. It never sees content, channel, people or the server's address unless the operator's IP is one. Apple/Google see the relay's IP, not the server's. | A hosted service, its availability and abuse handling, the two developer accounts, a relay-token enrolment flow. | yes | yes (Play builds) |
| **O4 Operator-run relay** | The same relay program as O3, run by an operator beside `conchd` with their own keys. | The operator, who signs their own build. | As O2, with the keys in a separate process. | As O2, plus running one optional process. | as O2 | as O2 |
| **O5 UnifiedPush (Android)** | The app registers with a distributor the person chose (ntfy, NextPush, Sunup, …) and gets an RFC 8030 endpoint; `conchd` posts an RFC 8291-encrypted message to it with a VAPID key. The push server may be the person's own. **[source: UnifiedPush Android spec AND_3.1.0; UnifiedPush FAQ]** | The person (distributor app) and whoever runs the push server, which may be nobody but them. | The distributor's push server sees timing, size and the endpoint; the body is encrypted to the app. No Google. | None for the operator beyond turning the transport on; a VAPID key `conchd` makes itself. | **no**: "running a UnifiedPush distributor won't be possible without jailbreaking or Apple's approval" **[source: UnifiedPush FAQ]** | yes, Google-free included |
| **O6 Web Push to a browser (PWA)** | If M1 picks a Home-Screen web app, the same RFC 8030 transport reaches Safari's and Chrome's push services. | Apple/Google/Mozilla run the push services. | They see the subscription endpoint, timing and size; body encrypted. | None beyond O5's. iOS 16.4+ Home Screen apps only; Safari "doesn't support invisible push notifications" and revokes permission if a push shows nothing **[source: Apple web push page; WebKit blog]**. | partial | yes |
| **O7 ntfy topic per person (reuse D7's integration)** | `conchd` publishes to a per-member topic on an ntfy server; the person subscribes in the ntfy app. | The operator (ntfy server) or ntfy.sh. | A topic is a capability: anyone who learns it reads everything on it; on iOS a self-hosted ntfy needs ntfy.sh upstream, which receives "only the message ID … and the SHA256 checksum of the topic URL" and shows `New message` **[source: ntfy config docs]**; ntfy.sh limits 250 messages/visitor/day **[source: ntfy publish docs]**. | ntfy app on the phone; no Conch app integration; no per-device binding to a credential. | via ntfy.sh | yes |
| **O8 Expo push service** | If M1 picks React Native with Expo, `exp.host` relays to APNs/FCM. | Expo. | Expo sees everything O3's relay sees, plus the payload; a third company in the path. Expo documents a direct APNs/FCM path as an alternative **[source: Expo push docs]**. | A vendor account; no Conch control over retention. | yes | yes |

### 3.2 The fact that shapes everything: provider keys follow the app publisher

An APNs authentication key is a team's and signs for that team's topics (bundle ids) **[source: Apple token-based connection page: team-scoped and topic-specific keys]**; an FCM registration token is issued to an app instance of a Firebase project's app **[source: FCM manage-tokens page]**. So a server operator can only push to the official store build through whoever published it. An independently self-hosted server therefore has exactly three honest options for an iPhone running the store app: a relay the publisher designates (O3), building and signing their own app (O2/O4), or no push (O1). This is not a design choice; it is Apple's and Google's model, and the note says it plainly so that nobody later expects "operator-owned credentials" to cover the store app.

### 3.3 Rejected

- **O7 ntfy topics for chat.** A topic is a shared secret, not a credential: it cannot be bound to a sign-in, revoked with it, or scoped by what the person may see at fetch time; and reaching iOS routes through ntfy.sh. Right for approvals under D7 (content the operator chose to publish), wrong for a person's private channels and whispers. Kept as what it is.
- **O8 Expo's service.** One more party that sees tokens and timing, chosen by a build tool rather than by the operator; if M1 picks Expo, #1116/#1117 use the direct APNs/FCM path Expo documents, through the relay.
- **Content in the payload** (even "just the channel name", even encrypted-to-device). §4.2 says why.
- **A per-device queue on the server.** It would let a removed member's queue keep a channel's messages until a fetch, and it is more state to get wrong; the feed is computed at fetch time instead (§4.3).

### 3.4 Deferred

- **O2 direct APNs/FCM inside `conchd`.** The relay program (O3/O4) contains exactly that code; folding it into `conchd` later is a flag, not a redesign, and it is deferred so that `conchd`'s first push slice has one outbound shape per transport and no provider SDK or credential file. Decision D-3 in §11 asks Nick whether he wants it in the first slice regardless.
- **O6 Web Push to a browser.** The transport is the same as O5's, so it costs nothing in `conchd`; whether a web client ever subscribes is M1's call and the browser's own later issue.

### 3.5 Chosen: O1 always; O5 and O3/O4 as the two optional transports

- **O1** is the floor and ships first: #1117's fetch-on-open with no registration at all.
- **Transport `webpush`** (O5, and O6 if ever): `conchd` posts RFC 8030/8291/8292 messages to an endpoint the phone supplied. Serves every UnifiedPush distributor and every browser push service with one code path. An operator turns it on with one flag; `conchd` makes the VAPID key itself (§3.6). No external process is required for it to work, so it is the one transport an independently self-hosted server can offer with nothing else.
- **Transport `relay`** (O3 and O4 are the same program): `conchd` posts the wake plus the device token to a relay URL with a bearer the operator configured; the relay speaks APNs and FCM. The managed relay is Nick's decision (D-1); an operator-run relay is the same binary with their own keys (D-2).

What the phone does with this: an Android build offers UnifiedPush first and FCM-through-relay where a relay is configured; an iOS build offers APNs-through-relay or nothing. The app shows which it has (§6.6).

### 3.6 Operator configuration (proposed flags; names are for #1116 to settle)

| Flag / env | Meaning | Default |
| --- | --- | --- |
| `--push-webpush` / `CONCHD_PUSH_WEBPUSH` | enable the Web Push transport; on first use `conchd` generates a VAPID P-256 key pair into the data directory and reuses it | off |
| `--push-webpush-allow-private` | permit endpoints that resolve to loopback, link-local, RFC 1918 or other non-public addresses (a self-hosted distributor on the LAN); otherwise refused at registration **and at every dial** (§5.7) | off |
| `--push-relay-url`, `--push-relay-token-file` | the relay transport; the token is read from a file, never from the command line, and never logged | unset |
| `--push-expire` | how long a provider may hold an undelivered wake | 24h |

Push is **off** unless one transport is configured; `GET /v1/push/config` (§9) says which are on, so a phone never offers a switch the server cannot honour.

## 4. The privacy model

### 4.1 What a push may carry

The push body is one document, `conch.push_wake.v1` (§9), and the whole of it is:

```json
{"schema":"conch.push_wake.v1","registration":"<opaque id>"}
```

- `registration` is an opaque, random (≥128-bit, base64url) id the server issued at registration (§5.1). It is not the credential, not the principal id, not sequential, and grants nothing: presenting it to the server does nothing without the credential.
- Per transport, the provider envelope adds only what the provider requires to show or deliver anything: for APNs an `aps` with a fixed, server-independent `alert` (`"Conch"` / `"New activity"`), `mutable-content: 1` so the service extension may replace it, `apns-push-type: alert`, `apns-priority: 10`, `apns-collapse-id: <registration id>`, `apns-expiration: now + --push-expire`; for FCM a **data** message (no `notification` key, so nothing is shown that the app did not render) with `collapse_key: "wake"` (one constant key; the next bullet says why that is safe), high priority (because it must wake the app and will always produce a visible notification, §7.2), `ttl: --push-expire`; for Web Push the encrypted body, `TTL: --push-expire`, `Topic: wake`, `Urgency: normal`.
- **Collapse is per app on the device, not per server, and the phone is designed for that.** APNs "stores only one notification per bundle ID" whatever the `apns-collapse-id` says **[source: Apple APNs request page]**, so a phone signed into two servers and offline for an hour gets one wake back, whichever server sent it. Rather than pretend otherwise, **a wake means "sweep every profile"**: on any wake the phone fetches the pending feed of every server and account it is registered with (§4.3 step 1), so server B's wake replacing server A's loses nothing. Given that, the FCM and Web Push keys are deliberately constant: a per-registration key would not help on iOS at all, would run into FCM's bound on distinct collapse keys per app per device (four at a time in Google's older documentation **[assumption: not re-verified on the current page]**), and would only make the sweep less likely to be exercised.

### 4.2 What a push may not carry, and why

| Never in a push | Why not |
| --- | --- |
| The server's URL or name | A relay or provider would learn which server a device belongs to; the phone already knows it from the registration id. |
| A channel id or name | A channel name is content (ADR-013: a private channel's existence is members-only); an id is an oracle on activity volume. |
| A message id, author, text, or a count | Content, and content that could outlive the person's access (§4.5). Counts across channels tell a provider how busy a private server is. |
| The kind of event (mention / message / whisper) | With a `mentions` policy the *timing* of wakes already correlates with mentions; a kind field would state it outright. |
| The principal id, credential id, or anything that identifies the person | The registration id is enough for the phone and useless to anyone else. |
| Anything an operator or client could customise | A fixed shape has nothing to leak through configuration. |

The consequence, accepted: every wake looks the same. A provider learns **that** this app on this device received a push at this time from this source. That is the metadata floor of every push system; it is not reducible except by sending nothing.

### 4.3 Fetch after authentication: the pending feed

On a wake the phone (the iOS notification service extension, the Android FCM/UnifiedPush receiver) does, with the credential of the registration:

1. Recognises the wake's registration id: a live local profile's, or a **tombstone** (an id of a registration the phone itself retired by signing out, deleting it, or seeing a 401, kept for recognition only for 30 days, §5.2). Either starts the sweep; an id that is neither is dropped. Tombstones close the hole that collapse opens: with wakes for B and then A queued while offline, a sign-out of A on the phone, and only A's wake surviving, B's news would otherwise go unannounced. Then, because providers collapse wakes per app (§4.1), it fetches **for every profile and account it holds a registration for**, not only the named one, in parallel and within the extension's budget: `GET /v1/notifications/pending?after=<cursor>` (§9; the route name is for #1116) with each profile's own credential and each profile's own cursor. The feed is scoped by the credential that calls it; it takes no registration id.
2. The server resolves the caller as `Reader` exactly as the REST message routes do and computes, **now**, the messages that are: visible to the reader under the audience predicate; in channels the reader is in now; newer than both `after` and the reader's read mark; not authored by the reader; not in the credential's muted list; matching the credential's policy (§6.1; `mentions`: the §6.2 rule, or a whisper to the reader; `all`: any). It answers them **oldest first, in ascending message id, a page of at most twenty**, with `cursor` (the id of the last summary in the page) and `more` (whether matches newer than that exist). The cursor relies on message ids being **globally monotonic across channels**, which they are today: one `messages` table with `id INTEGER PRIMARY KEY` (`internal/server/store/store.go:44`) **[verified]**; a move to per-channel ids would break the single cursor and would need a per-channel one, which is why S3 names the assumption. Each is a `conch.notification_summary.v1` (§9): channel id and name, message id, posted-at, kind, author name as `hide_members` permits (an author stays named in a channel the reader is in, ADR-013 decision 9 / R6), and the message text **only if** the credential's `preview` switch is on (§6.3).
3. The phone **drains before it advances**: it renders the page, persists `cursor` as that profile's high-water mark, and asks again with it while `more` is true, up to five pages (one hundred summaries) per wake. Because pages are ascending and the mark moves only past pages actually received, nothing older is ever skipped, and a message that arrives while the loop runs is newer than the mark and comes in the next page, the next wake or the next open. If `more` is still true after the fifth page, the phone stops, shows one line for that profile ("More new messages on <profile>", the local name), and the rest is drained at the next wake or open; the feed is a catch-up, not the inbox, and the unread badge on open stays the truth. It renders one OS notification per summary, keyed by server profile and message id (so a second wake for the same message replaces, never duplicates); how many a channel gets before they are grouped into one is M2's and the OS's, not the protocol's.
4. Two outcomes are not summaries, and the platforms treat them differently:
   - **The fetch fails** (offline, timeout within the extension's budget, server down). With `quiet_when_unreachable` **false** (the default, §6.3) the phone shows the generic fallback: on iOS the fixed `"Conch" / "New activity"` line the payload already carries; on Android a generic line under the **server profile's local name** (the phone's own label, never from the payload). With it **true** the phone suppresses where the platform allows: always on Android (a data message shows nothing the app does not post); on iOS only with the filtering entitlement below, otherwise the fixed line still shows.
   - **The fetch returns nothing** (every profile's feed empty: a stale or collapsed wake, access lost since the wake, a spurious or forged wake, a 401 because the credential is gone). On Android the app posts nothing, whatever the switch. On iOS the extension can suppress the alert **only with** `com.apple.developer.usernotifications.filtering`, which "allows a notification service extension to receive remote notifications without displaying the notification to the user" by returning an empty `UNNotificationContent`, and which is obtained by application to Apple ("To apply for this entitlement, see Request Notification Service Entitlement") **[source: Apple entitlement page]**. **Without it, an empty fetch on iOS shows the fixed `"Conch" / "New activity"` line.** **Empties happen in ordinary traffic**, not only in edge cases: step 2 bounds the feed by the reader's read mark, which belongs to the principal, not to a device (`pkg/schema/read_state.go`), so a person reading at a desk advances it before the phone's extension fetches, and a baseline iOS phone on `all` shows "New activity" with nothing behind it for every message already read elsewhere. **The read-mark bound is kept on purpose** (the alternative is discussed in §10): without it, every message read at the desk would become a phone notification *with content* on both platforms, cleared only at the next open, which is worse than a content-free line on one platform, and it would make the feed disagree with what "unread" means everywhere else. The remaining empties (access lost since the wake, revocation, staleness, collapse, spurious wakes) are rarer. The consequences: the baseline iOS settings copy names this case (§6.3); D-9 recommends applying for the entitlement at once rather than eventually; and Q4 (suppress a wake when the principal is active elsewhere) becomes a worthwhile reduction of the common case, though never a correctness rule. If Apple grants the entitlement, the extension suppresses empties and the baseline rows in §7 become the fallback. A 401 additionally marks the local registration a tombstone and the profile signed out.

Nothing is stored on the server for step 2 that is not already stored: messages, memberships, read marks, the credential's preferences. A membership removed one second before the fetch yields an empty answer, by construction, because the predicates are the routes' predicates. This is N7.

### 4.4 What each party can learn

| Party | Learns | Does not learn |
| --- | --- | --- |
| **Apple / Google** (APNs, FCM, their Web Push services) | that app X on device Y received a push at time T; the sender's IP (the relay's under O3; the operator's under O2/O4/O6); size (constant); the device token or endpoint | server, channel, people, text, counts, kind, whether the person is a member of anything |
| **A UnifiedPush push server** (ntfy, NextPush, …) | the endpoint, timing, constant size, the posting server's IP | the body (RFC 8291 encrypted to the app), and everything above |
| **The managed relay** (O3) | which servers push (relay token, IP), device tokens, timing, counts per token | content, channel, people; the server's URL unless the operator's IP is one |
| **The server operator** | everything they already know (ADR-000: server-trust model), plus the registrations: which of a person's sign-ins have push, which transport, when last woken and fetched; **and, because a wake sweeps every profile (§4.3 step 1), the timing of other servers' wakes to that phone**: server B sees a feed fetch whenever server A woke the device, so each operator learns when the person's other servers had something to say, though not which server, what, or how many | nothing new about content; the cross-server timing is a cost of the sweep, accepted because the alternative (fetch only the named registration) loses notifications under collapse |
| **Someone who steals the device token or endpoint** | can cause wakes (bounded, §5.7); each wake makes the phone fetch and find nothing new | content; a wake grants no read |
| **Someone who steals the registration id** | nothing: it is a label, not a credential | — |
| **A person with the phone unlocked** | the summaries on the lock screen / shade: channel name, author, kind; text only with `preview` on | what the OS does not show them |

### 4.5 Limits on recall and on offline delivery

Stated, not promised:

- **An OS notification already shown cannot be reliably withdrawn.** On iOS an app removes delivered notifications only while it runs; a background push to wake it for that is "low priority … the system doesn't guarantee their delivery … don't try to send more than two or three per hour", a force-quit discards a held background push, and the system "discards the older notification and only holds the newest one" **[source: Apple background-updates page]**. On Android a high-priority data message can wake the app to cancel a notification, subject to Doze and to FCM deprioritising senders whose messages do not produce visible notifications **[source: FCM message-priority page]**. Therefore **no design can honour "No private message content in a notification after the relevant access is lost" for content already rendered**; this design honours it by rendering as little as possible (summaries by default; text opt-in) and by reconciling on the next open: the app re-reads `GET /v1/channels` and cancels every notification whose channel is gone or whose message it can no longer read. The gap between access loss and that open is a documented limit. A `webpush` endpoint or FCM data path *may* be used to attempt an earlier cancel; it is best effort and is not claimed.
- **Offline devices get at most the latest wake.** APNs "stores only one notification per bundle ID … In most cases, the latest" and "may store the notification for 30 days or less, depending on … `apns-expiration`" **[source: Apple APNs request page]**. FCM keeps a collapsible message's latest instance, discards when more than 100 messages are pending or the device has not connected for over a month, and then calls `onDeletedMessages`, on which the app "should perform a full sync" **[source: FCM Android receive page]**; default lifetime four weeks **[source: FCM message-types / concept pages via search]**. Web Push servers hold a message for its `TTL` and are "not obligated to store messages indefinitely" **[source: RFC 8030]**. With `--push-expire` at 24 h, a phone off for longer sees nothing from push and everything from fetch-on-open. That is the intended behaviour: a wake is a hint, the feed is the truth.
- **A wake can arrive late or out of order.** APNs "may reorder notifications you send to the same device token … may also get throttled, saved in storage, and in some cases, not delivered" **[source: Apple APNs request page]**. Because every wake is the same, order does not matter; a stale one produces an empty fetch, which shows nothing on Android and, on iOS without the filtering entitlement, the fixed generic line (§4.3 step 4). The APNs envelope already uses `apns-push-type: alert`, which the entitlement requires for suppression to work at all ("Otherwise, the system always displays the notification banner") **[source: Apple entitlement page]**.
- **A wake can be lost.** No path claims otherwise. The read-state badge on open is the backstop, as it is in the browser.

## 5. Registration lifecycle

### 5.1 The object

A **push registration** is one row: `id` (opaque random id, §4.1), the owning `credential_id` (hence principal), `transport` (`webpush` | `relay`), `platform` (`ios` | `android` | `web`), `app` (bundle / package / origin, bounded string, for the relay's provider choice and for the person's own list), the **target** (`webpush`: endpoint URL, `p256dh`, `auth`; `relay`: provider `apns` | `fcm` and the device token), `created_at`, `last_wake_at`, `last_fetch_at`, `state` (`active` | `expired`). **It holds no preference:** policy, mutes and the rendering switches are the credential's (§6.1), exist with or without a registration, and a registration is only an address. The target is **write-only on the wire**: list and read responses show the transport, platform, app and a short fingerprint of the target, never the token or endpoint (as `CredentialV1` never carries the token).

Why bind to the credential and not the principal: the credential is the sign-in, and everything else that belongs to a sign-in (device links, sockets, the voice session) already ends with it (§1.3). A person with two phones has two credentials and two registrations, each listed under its sign-in at `/v1/credentials/mine` (a later additive `push` field on `OwnCredentialV1`, §9).

### 5.2 Per-server, per-account scoping

The phone keeps, per saved server profile and account, the registration id it was issued. The id's job is **recognition, not selection**: a wake whose id matches a live local profile, **or a tombstone**, is one of ours and starts the sweep of every live profile (§4.3 step 1); a wake whose id matches neither is dropped silently (a reinstalled app, an id older than the tombstone window, or someone else's); nothing is shown and no server is contacted. The lookup is local. **A tombstone** is the id of a registration this phone retired (sign-out, delete, 401), kept for recognition only, never for a fetch, for **30 days** from retirement: the longest a provider may still hold a wake sent before the retirement (APNs "30 days or less"; FCM's four-week default lifetime; §4.5), whatever `--push-expire` a given server chose. With no live profile left, a tombstoned wake is dropped. Tombstones live beside the profile map in secure storage and are pruned on launch. Two servers can never confuse each other's wakes even if their internal ids collide, because the registration id is random and server-issued, not a counter.

### 5.3 Creating, updating, rotating the target

- `POST /v1/push/registrations` with the credential: creates, answers the full object once (the id included). Refused when push is off (`push_not_configured`), when the transport is off, when the endpoint is not `https`, when any address it resolves to is forbidden (§5.7; the same check runs again at every dial, because a name can change after registration), or when it exceeds the size bounds; when the credential already holds the per-credential maximum (§5.7); for agents and for setup-scope credentials (humans only, like read state).
- `PUT /v1/push/registrations/{id}`: the same body; replaces the target. This is how a **provider token change** is handled: FCM says the SDK "does not automatically manage refreshes on routine syncs", recommends saving the token "alongside a timestamp", and treats a registration as stale after a month of silence **[source: FCM manage-tokens page]**; APNs tokens are opaque and may change across restores and reinstalls **[assumption: Apple's registration page not fetched; standard guidance]**. The app re-sends the target on every launch when it differs from what it last sent, and at least monthly otherwise, which also refreshes `last_fetch_at`.
- **Credential rotation** (`PUT /v1/password/mine` rotates the token; operator rotate) revokes the old credential, so the registration ends with it (§5.4) and the app, which now holds a new credential, registers again. Simpler than migrating the row, and it means a rotated-away credential can never be woken.

### 5.4 Logout and revocation

In the same transaction that revokes a credential, for every reason there is today (self sign-out, ending another sign-in by id, operator revoke, rotate, revoke-all, disable, expiry when it is reaped, idle expiry), every registration of that credential is set `expired` with that reason and is never woken again. This is the device-link rule of #712 applied once more, and it is the only rule: there is no "keep notifications after logout". The app, on its side, deletes the registration (`DELETE /v1/push/registrations/{id}`) before it revokes the credential when it can, does not depend on it, and keeps the id as a tombstone (§5.2) so that a wake already in flight still sweeps the profiles that remain.

Audit: `push_registration_created` and `push_registration_ended` (detail: reason `self` | `credential_revoked` | `provider_rejected` | `stale`, the transport and the registration id; never a token, endpoint or app id string), on the principal as subject. Wake attempts are **not** audit rows (§5.6).

### 5.5 Channel access loss, mutes, hide_members

- No per-channel state exists in a registration at all; the muted list is the credential's preference (§6.1). Removal from a channel, a channel going private, a ban: the next policy evaluation (post time) does not match, and the next feed (fetch time) does not include, by the routes' own predicates. No frame, no cleanup, no race window beyond §4.5's rendered-notification limit.
- Muted ids are stored as given, bounded in count (256) and unvalidated against membership: validating would make `PUT /v1/notifications/preferences` a second place where "not yours" and "not a channel" must answer alike, for nothing, because an id the person cannot see never matches anyway.
- `hide_members` affects the feed only through the existing author-name predicate; the policy evaluation never names a person to the phone, and the wake names no one.

### 5.6 Sending, retries, coalescing, deduplication

- **When:** after a message commits, the handler enqueues one reference per active registration whose principal may read it and whose credential's preferences match, exactly where it enqueues events-socket references today; a separate goroutine with a bounded queue sends. A full queue drops the wake and logs at WARN with a counter; it never slows the post (the events hub's slow-consumer rule).
- **Coalescing:** at most one wake per registration per `--push-coalesce` (proposed 30 s) **unless** the registration has fetched since the last wake; a wake within the window is folded (the provider collapses it anyway). A registration whose credential's policy is `mentions` gets a wake per mention, subject to the same floor.
- **Retries:** one attempt, 2 s timeout (ntfy's), then on 5xx or 429 up to two retries with backoff inside one minute; never on 4xx that names the target: APNs `410 Unregistered` ("There is no need to send further pushes to the same device token") and `400 BadDeviceToken` ("Don't retry …") **[source: Apple response page]**; FCM `UNREGISTERED` (404) means "it is safe to delete your record of this registration" **[source: FCM manage-tokens page]**; Web Push 404/410 likewise; the relay passes these through. Any of them sets the registration `expired`, reason `provider_rejected`, audited once.
- **Deduplication:** the wake has no identity, so there is nothing to deduplicate on the server. The phone deduplicates summaries by (profile, message id), as the browser does by `origin:account:message id`.
- **Observability:** counters at `/metrics` per transport and outcome class (the `transportCause` vocabulary, reused: `timeout`, `name lookup failed`, `TLS failure`, `connection refused`, `status 4xx/5xx`, `request failed`); a DEBUG log line per failure with the registration id and the class; never an endpoint, token or URL. Not audit: a busy server would write thousands of rows a day that record nothing about authority.

### 5.7 Abuse limits

| Limit | Proposed bound | Why |
| --- | --- | --- |
| Registrations per credential | 2 | one install plus one re-registration in flight; a credential is one device |
| Registrations per principal | 16 | many sign-ins is already visible in `/v1/credentials/mine` |
| Registration writes | the account routes' rate limiter (`internal/server/ratelimit.go`) | a registration names an outbound URL |
| Wakes per registration | 1 per 30 s coalesced; 120 per hour hard cap, then dropped until the next fetch | a stolen token or endpoint can be made to buzz, not to read |
| Wakes per server per minute | an operator flag, default 600 | an outbound budget for the whole process |
| Endpoint | `https` only; ≤ 1,000 bytes (UnifiedPush's bound **[source: UnifiedPush spec]**); no redirects followed; response body not read past the status; and **the address check is enforced in the dialer, on every connection and every retry**, not only at registration: the host is resolved, **every** A and AAAA answer must lie outside the forbidden set (unspecified, loopback, link-local including `169.254.169.254`, RFC 1918, CGNAT `100.64.0.0/10`, ULA `fc00::/7`, multicast, and the IPv4-mapped, IPv4-compatible and NAT64 IPv6 forms of any of these), a single forbidden answer refuses the whole dial, and the TCP connection is made to one of the addresses just checked while TLS `ServerName` stays the hostname, so certificate verification is unchanged; the HTTP client never re-resolves between check and connect. The registration-time check exists so the person sees the refusal; the dial-time check is the control. `--push-webpush-allow-private` relaxes the range test only, never the pinning. The operator-set relay URL goes through the same dialer with private ranges permitted, because the operator chose it | the endpoint is a URL a member chose, and `conchd` will POST to it: this is an SSRF surface and #1116 gets a `security-reviewer` pass for it |
| Feed page | 20 summaries, text cut to 160 characters when `preview` is on (the browser's snippet) | bounded work in a 30 s extension |

A member cannot prove they own an endpoint (Web Push has no such step), so a member could register someone else's endpoint and make `conchd` post wakes to it. The bounds above make that a nuisance, not a leak: the body is opaque and encrypted to a key the victim does not hold, and the rate is capped. Recorded as an accepted, bounded abuse.

### 5.8 Stale and offline devices

- A registration with no fetch and no `PUT` for 35 days (FCM's "a month" with slack) is set `expired`, reason `stale`, by the same reaper that expires credentials **[verified: credentials expire; the reaper's name not checked]**. The app, finding its registration gone at next launch (`GET /v1/push/registrations/mine` answers without it), registers again if the policy is still on.
- A device that is offline when wakes are sent gets at most the provider's latest stored wake within `--push-expire` (§4.5), and the feed on open.

## 6. Preferences, prompts, taps, many servers, nothing configured

### 6.1 Where preferences live, and what stays as it is

- Notification preferences are **one server-side object per credential**, `NotificationPreferencesV1` (§9.1 S8), read and written by its owner at `GET`/`PUT /v1/notifications/preferences` (the caller's own, as `/v1/credentials/mine` is; no id in the path, so nobody can name another's). Fields: `policy` (`off` | `mentions` | `all`, default `off`), `muted` (channel ids, default none), `preview` (default off), `quiet_when_unreachable` (default off). It exists **with or without a push registration**, and that is the point: on a server with no push transport (O1) the phone reads and writes exactly this object and applies it to the in-app alerts it builds from the events socket, as the browser applies its local preferences; when a transport exists, the server applies the same object to decide whether to wake the credential's registration(s), and the feed applies it too. A registration is an address; the preferences are the policy; neither needs the other.
- **Why per credential and not per principal:** a credential is one device's sign-in. People want their phone on `mentions` and their desk on `all`, and a mute on a phone should not silence a desktop; that is the browser's per-client model (#840), made server-side so the server can act on it. A per-principal object was considered and rejected (§10). The object dies with its credential like everything else of a sign-in (§1.3); after a credential rotation the phone, which keeps a local mirror for offline display anyway, re-`PUT`s it under the new credential.
- The browser's `conch.web.notifications.v1.*` client-side preferences are untouched; a person's browser and phone may therefore disagree, as two browsers may today. Adopting the same object in the browser is deferred (D-5).
- The vocabulary is the browser's, verbatim: `off` | `mentions` | `all`, muted channel ids, and the hint text's meaning ("Channel mutes hide alerts, not unread badges"). M2's settings screen owns the words; this note owns the semantics.

### 6.2 off / mentions / all, mutes, whispers, edits, own messages

- `off` (default): no wake is sent and no in-app alert is shown. The phone deletes its registration when it has one; the server, independently, sends nothing to a registration whose credential's policy is `off`, so the two cannot disagree.
- `mentions`: a wake for a message whose body mentions the reader's **current display name** under the browser's rule (§1.1: `@name`, token-bounded, case-insensitive; an empty name never matches), **or a whisper addressed to the reader** (it is addressed to them; the browser notifies whispers under `all` only because it has no separate rule, and this note proposes the stricter reading; see open question Q3). The rule is implemented once on the server (§9 lists it as shared-rule work with a golden table so the web client's matcher and the server's agree).
- `all`: a wake for every message the reader may see that they did not write.
- Mutes apply to both. A message edit never wakes. The reader's own messages never wake. Reactions never wake (deferred, §10).

### 6.3 Preview and quiet switches

- `preview` (off): summaries carry no message text. On: a 160-character snippet, the browser's. The settings copy must say that text shown on a lock screen stays there until dismissed and cannot be recalled by the server.
- `quiet_when_unreachable` (`false` by default): governs the **fetch-failed** case only (§4.3 step 4). `false`: show the generic fallback (iOS: the fixed `"Conch" / "New activity"` line; Android: a generic line under the profile's local name), so a person offline or behind a dead server still learns something happened; the default favours not missing things. `true`: suppress where the platform allows: always on Android; on iOS only with the filtering entitlement, otherwise the fixed line shows anyway and the settings copy must say so. The **fetch-empty** case is not governed by this switch: the phone always suppresses it where it can (Android always; iOS with the entitlement), and on iOS without the entitlement the fixed line shows regardless. The commonest empty is a message read on another device between the wake and the fetch (§4.3 step 4), so the baseline iOS copy says plainly: "If you read a message elsewhere first, this phone may still show 'New activity' with nothing new behind it." The renderer in §9.4 and the rows in §7 use exactly these meanings.

### 6.4 Prompt timing

- The OS permission is asked **only when the person moves the policy off `off`**, from the settings screen, never at first launch or sign-in: Android 13's `POST_NOTIFICATIONS` is a runtime permission, "notifications are off by default" on new installs, and the guidance is to ask "in the correct context, so that it's explicitly clear what the notifications are used for" **[source: Android notification-permission page]**; Apple's is to "make the request in a context that helps people understand why" and warns that "subsequent authorization requests don't prompt the person" **[source: Apple asking-permission page]**. iOS **provisional** (quiet) authorisation is not used: a mention that lands silently in Notification Center is a missed mention.
- A denied OS permission makes the phone delete its registration (no wakes to a device that cannot show them) and leaves the credential's preferences as they are, since they still govern in-app alerts; it shows, on this device, the browser's equivalent line: "Notifications are off for this app in Settings; alerts stay in the app."
- Where the server offers no transport (§6.6), the policy and mute controls still work, for in-app alerts; the settings screen says that nothing arrives while the app is closed, and why, as client-experience.md requires of a control that does less than it looks.

### 6.5 Tap routing

1. The notification carries, locally, the profile, the account and (when a summary was rendered) the channel id and message id. Nothing in it came from the payload except the registration id, already resolved.
2. On tap: if the app holds a live credential for that profile and account, open that server, that channel, scrolled to that message if it is still readable; if the channel is no longer in `GET /v1/channels`, open the server's channel list with the existing line "You no longer have access to #name" (`Lobby.tsx`, `channels.lost`); if the credential is gone, open that profile's sign-in screen and discard the notification. **Never** open a different server, whatever the ids.
3. A tap on the generic line (fetch had failed) opens the profile and lets the ordinary open-time catch-up run.
4. A tap must not itself fetch with a notification-held token or carry a deep link that bypasses the app's own sign-in state; the registration id is not an authenticator.

### 6.6 Many servers; nothing configured; provider outage

- **Many servers:** one registration per (profile, account); each server chooses its own transport; the phone may hold a UnifiedPush endpoint for one server and an FCM token through a relay for another. A wake from any of them sweeps all of them (§4.3 step 1), so a collapsed wake while offline costs nothing. Notifications group by profile. The policy and mutes are per server, because they live on that server under that sign-in.
- **No push on the server** (`GET /v1/push/config` lists no transport): the preferences work as everywhere else, for in-app alerts; the settings screen says "This server can't reach your phone while the app is closed. Your mention and mute settings still apply when the app is open, and you'll see new messages when you open it." The badge on open (read state) is unchanged. Nothing else differs.
- **Push on the server, no transport on the phone** (iOS with no relay configured; Android with neither a distributor nor a relay; a de-Googled phone): same line, with the reason, and, on Android, a pointer to install a UnifiedPush distributor.
- **Provider or relay outage:** wakes fail, counters rise, the server log says so at WARN once per minute at most, nothing else changes; the next open catches up. If the outage outlasts `--push-expire`, wakes sent during it are simply never delivered, and that is fine (§4.5).
- **Foreground:** when the app is on screen with a live socket it renders alerts from the socket under the credential's preferences, as the browser does under its own, with or without push, and ignores wakes (it may still fetch to advance the cursor).

## 7. Failure scenarios by app state

No row claims guaranteed delivery. "Expected" is what the cited documentation says; "verify" names the real-device row (M4 #1109 / M5 #1110, then #1117) that must observe it.

### 7.1 iOS

| State | Alert push with `mutable-content` (this design) | Background push (`content-available`; not used by this design) | Verify |
| --- | --- | --- | --- |
| Foreground | delivered to the app; the app shows nothing from push and relies on its socket | n/a | socket wins; no duplicate |
| Suspended (in background, in memory) | the notification service extension runs "about 30 seconds" to fetch and replace the fixed line; if it does not finish, "the system displays the original contents" (the fixed line) **[source: Apple modifying-content page]** | low priority, may be throttled, not guaranteed **[source]** | extension fetch within budget on cellular |
| Terminated by the system | same as suspended: the extension is a separate process **[assumption: Apple's page does not condition the extension on the app being alive; verify]** | discarded when the app is force-quit **[source]** | notification after memory pressure |
| Force-quit by the person | the extension is expected still to run for alert pushes **[assumption; verify on device: this is the row most likely to surprise]** | discarded **[source]** | swipe-kill, then a mention |
| Empty, stale or rejected wake (the sweep finds nothing: access lost, credential gone, collapsed duplicate, spurious wake) | **without the filtering entitlement: the fixed `"Conch" / "New activity"` line is shown and cannot be prevented**; with it: the extension returns empty content and nothing is shown **[source: Apple entitlement page]** | n/a | remove the member from the channel after the wake is sent and before the phone fetches: baseline build shows the generic line and nothing else; entitled build shows nothing; both: the next open shows no content for that channel |
| Fetch fails (offline, timeout, server down) | `quiet_when_unreachable=false`: the fixed line; `true`: the fixed line still, unless the entitlement is held (§6.3) | n/a | airplane mode on the phone's data while leaving Wi‑Fi off after the wake was queued is hard to stage; stage instead with the server stopped between wake and fetch |
| Offline, then online within 24 h | the latest stored wake arrives (one per bundle id) **[source]** | — | airplane mode across several mentions: exactly one notification, then the feed shows all |
| Offline longer than `--push-expire` | nothing from push; the feed on open | — | — |
| Notification permission off | nothing shown; the extension is not employed when alerts are disabled **[source: modifying-content page]**; the server keeps sending until the app deletes its registration on seeing permission denied (§6.4) | — | deny, then re-enable |

### 7.2 Android

| State | FCM data message, high priority (relay) | UnifiedPush message (distributor) | Verify |
| --- | --- | --- | --- |
| Foreground | `onMessageReceived`; the app ignores it (socket live) **[source: FCM receive page]** | delivered by the distributor's broadcast; same | no duplicate |
| Background / cached | `onMessageReceived`; the app fetches and posts a notification; "high priority messages … should result in user-facing notifications" or FCM may deprioritise the sender after seven days of pattern **[source: FCM priority page]**: hence the app always posts something unless the feed is empty | distributor's foreground service or connection delivers; latency depends on the distributor and battery settings **[source: ntfy phone page: instant delivery is a foreground service]** | Doze: wake arrives; notification within seconds |
| Force-stopped (the person used "Force stop") | no delivery until the person opens the app again **[assumption: Android's stopped-state rule for broadcasts; verify on device]** | same, by the same rule | force stop, then a mention: nothing until opened; then the feed |
| Empty, stale or rejected wake | `onMessageReceived` runs, the sweep finds nothing, the app posts nothing; no entitlement is involved (a data message shows only what the app posts) | same | remove the member between wake and fetch: nothing shown |
| Fetch fails (offline, timeout, server down) | `quiet_when_unreachable=false`: a generic line under the profile's local name; `true`: nothing | same | server stopped between wake and fetch, both settings |
| Offline, then online | collapsed to the latest within `ttl`; over 100 pending or over a month silent: `onDeletedMessages`, full sync **[source: FCM receive page]** | the push server holds within `TTL` | airplane mode test as iOS |
| No Google services | FCM unavailable; UnifiedPush is the path | works | GrapheneOS or an emulator without Play |
| Battery optimisation restricting the app | high-priority FCM may still wake; normal would not; distributors need an exemption for their service **[assumption for the distributor row]** | | restricted bucket test |
| Notification permission denied (Android 13+) | the message arrives; the app can show nothing; the app deletes its registration (§6.4) and keeps its in-app alerts | same | deny, then a mention: no wake after the delete |

### 7.3 Both

- **Credential revoked while a wake is in flight:** the fetch answers 401; the phone marks the profile signed out and the registration a tombstone, sweeps its other profiles, and shows nothing for this one on Android, or on iOS with the entitlement; the baseline iOS build shows the fixed line (§4.3 step 4). The registration is already `expired` server-side.
- **Membership lost while a wake is in flight:** the feed is empty for that channel; nothing is shown for it on Android or on entitled iOS; the baseline iOS build shows the fixed line if no other profile had news. Test both builds (§7.1 row).
- **Server down:** fetch fails; generic or nothing per §6.3; the next open retries.
- **Two servers, offline across mentions on both:** on return, at most one wake survives collapse (certainly on iOS, §4.1; likely on Android); the sweep fetches both profiles' feeds and both sets of summaries appear, each under its own profile. Verify on both OSs: two profiles, airplane mode, a mention on each, one wake observed, two notifications shown.
- **Two servers, offline, then a local sign-out of one:** wakes for B then A are queued while offline; the person signs out of A on the phone (A's registration becomes a tombstone); on return only A's wake survives collapse; the tombstone is recognised, B is swept, B's summaries appear, A is not contacted. Then the same with the sign-out older than 30 days (tombstone pruned): the wake is dropped, and B's news waits for the next wake or open, which is the accepted edge of the window. Verify on both OSs.
- **More than twenty pending, with new messages arriving during the fetch:** the drain loop (§4.3 step 3) delivers every match exactly once across pages, in order, and the arrivals come in a later page, wake or open; a page cap hit shows the "More new messages" line and nothing is lost. Verify with a scripted server: 45 pending plus 3 posted mid-drain; 48 summaries, no duplicate, no gap; then with 120 pending: 100 shown, the line, and the remainder at the next open.
- **Read elsewhere between wake and fetch:** a desk client marks the channel read after the wake was sent and before the phone fetches; the feed is empty; Android and entitled iOS show nothing; the baseline iOS build shows the fixed line. Verify on both OSs with a browser session advancing the read mark; it is the commonest empty and the one the baseline copy names (§6.3).
- **Clock skew, duplicate wakes, reordered wakes:** all harmless by construction (§4.5).

## 8. Threat model

| # | Threat | Where it would bite | Control | Residual |
| --- | --- | --- | --- | --- |
| T1 | **Notification disclosure to a provider or relay** (Apple, Google, a push server, the managed relay reads the push) | payload | the payload is a registration id and a constant envelope (§4.1); Web Push bodies encrypted per RFC 8291 regardless | timing, counts and source IP (§4.2, §4.4) |
| T2 | **Disclosure through a wake's timing under `mentions`** | metadata | accepted and stated; a `mentions` registration reveals when mentions happen, not what or by whom | yes, stated |
| T3 | **Content shown after access is lost** | rendered notifications | summaries only by default; text opt-in; reconcile and cancel on next open; nothing content-bearing in a stored queue | the window until the next open (§4.5) |
| T4 | **Revoked membership keeps producing notifications** | server | policy at post time and the feed at fetch time both use the routes' predicates; no per-device queue | none beyond T3 |
| T5 | **Whisper or private channel leaks through the feed** | server | the feed is one more read path under ADR-005's rule: it reads through `ListVisibleMessages`-class functions and gets a row in the leak test over every read path, plus the `hide_members` isolation rows (community-model §13.9) | a missed predicate: caught by the structural guard tests, security review mandatory |
| T6 | **Token reassignment** (an FCM token or APNs token comes to belong to another app instance; the same phone reinstalled; a different person signs in on the same phone) | registration | a wake grants nothing; the new app instance holds no credential for the old registration id, so a fetch is impossible and the id matches no local profile, so it is dropped (§5.2); the old credential still exists only if the person did not sign out, in which case it is their sign-in to end at `/v1/credentials/mine`; provider `410`/`UNREGISTERED` expires the row | a phone handed over without sign-out keeps receiving contentless wakes until the credential is ended or the registration goes stale (35 d) |
| T7 | **Cross-server tap** (a notification from server A opens channel N on server B) | phone | routing by local profile from the registration id; ids are server-issued random strings; message ids are never used across profiles (§6.5) | none |
| T8 | **Relay compromise** | relay | learns tokens and timing; can send spurious wakes (empty fetches: nothing on Android, the fixed generic line on iOS without the filtering entitlement) and can drop wakes; cannot read content or impersonate a server to the phone (the phone never trusts a push, only its fetch) | denial of promptness; metadata as T1 |
| T9 | **Server database theft** | server | tokens and endpoints let the thief send wakes, nothing more; registrations carry no secrets the credential table does not already | as the rest of the database |
| T10 | **SSRF through a member-supplied endpoint** | server | `https` only; the address check in the dialer on every connection and retry, all answers checked, connection pinned to a checked address with TLS hostname verification kept (§5.7); no redirects, fixed small body, status only, timeouts, per-registration and per-server budgets; operator opt-in for private ranges | an operator who allows private ranges takes that on knowingly |
| T16 | **DNS change or rebinding after registration** (a public name at registration that later answers a private address; a TTL-0 name alternating public/private; a name with one public A and one private AAAA; an IPv4-mapped or NAT64 IPv6 answer for a private v4) | server | the same dial-time check: resolved immediately before connecting, every answer must pass, the socket is opened to a checked address and never to a fresh resolution; so a name that turns private after registration is refused at the next wake, and a mixed answer is refused whole. Tests in #1116: public-then-private, alternating, mixed A/AAAA, mapped and NAT64 forms, and a redirect to a private host (not followed) | none beyond the operator's own opt-in |
| T11 | **Spamming someone else's endpoint through a registration** | server | the same budgets; encrypted opaque body | nuisance (§5.7) |
| T12 | **Forged pushes to the phone** | phone | Web Push requires VAPID and the body is encrypted to the app's key; APNs/FCM authenticate the sender (the relay); and in every case the phone shows only what its authenticated fetch returned | a forged wake = one empty fetch, and on iOS without the filtering entitlement one content-free "New activity" line |
| T13 | **Enumeration** (does registration X exist? is principal P registered?) | server | another's registration id is 404 like one never issued; the list is own-only; no route names another's registrations | none |
| T14 | **Logs and audit leaking endpoints or tokens** | server | the `transportCause` rule from ntfy: fixed vocabularies, never the URL; audit details carry registration ids and reasons only | none if held; a lint test as ntfy has |
| T15 | **A hidden member (`hide_members`) named in a summary** | server | author names go through the same predicate the message routes apply; an author in a shared channel is already visible there (ADR-013 R6) | none beyond the setting's own stated limits |

## 9. Schema and API work, split by owner

Nothing below exists. Each line is a future issue; the shapes are proposals for those issues, written in the house style (flat structs, string enums, RFC 3339 timestamps, a `schema` field on every streamed or pushed document, additive and unknown-field tolerant). None touches a published shape, so none needs a version bump; the `schema-change` skill still governs fixtures and the TypeScript generation.

### 9.1 `pkg/schema` (protocol-designer)

| Future issue | Shape(s) | Notes |
| --- | --- | --- |
| S1 the wake | `PushWakeV1` (`conch.push_wake.v1`): `schema`, `registration` | the only thing a provider ever carries; fixture `push-wake-v1.json` |
| S2 registrations | `PushTransport` (`webpush`, `relay`), `PushPlatform` (`ios`, `android`, `web`), `PushProvider` (`apns`, `fcm`); `PushRegistrationV1` (read shape, target fingerprinted, **no preference fields**); `CreatePushRegistrationRequestV1` / `UpdatePushRegistrationRequestV1` (target write-only: `endpoint`, `p256dh`, `auth` for `webpush`; `provider`, `token` for `relay`); `ListOwnPushRegistrationsResponseV1`; error codes `push_not_configured`, `push_transport_unavailable`, `push_endpoint_refused`, `too_many_push_registrations`, `push_registration_not_found` | bounds in constants (`MaxPushEndpointBytes` 1000, `MaxPushRegistrationsPerCredential` 2) |
| S3 the feed | `NotificationSummaryV1` (`conch.notification_summary.v1`): `schema`, `channel_id`, `channel_name`, `message_id`, `posted_at`, `kind` (`message`, `mention`, `whisper`), `author_id`, `author_name`, `text` (omitted unless `preview`); `ListPendingNotificationsResponseV1` with `summaries` (ascending by `message_id`, at most `MaxPendingNotificationsPage` 20), `cursor` and `more` | `kind` closed, client-tolerant like `JoinWarning`; the request is `after` only, no registration id; **the single `cursor` assumes globally monotonic message ids** (one `messages` table, `store.go:44`), and the shape's comment must say so, so that a future per-channel id scheme knows it owes this route a per-channel cursor |
| S8 preferences | `NotificationPolicy` (`off`, `mentions`, `all`; the browser's vocabulary, now a schema vocabulary so `vocabularies.go` generates it for `web/src/schema/`); `NotificationPreferencesV1` (`policy`, `muted`, `preview`, `quiet_when_unreachable`); `PutNotificationPreferencesRequestV1` (the same fields, whole-object replace) | per credential, independent of S2; `MaxMutedChannels` 256; the one object the O1 path uses |
| S4 server capability | `PushConfigV1`: `transports`, `vapid_public_key` (omitted when `webpush` is off), `relay_providers` (`apns`, `fcm`, as the relay reports) | public to any authenticated caller; nothing secret |
| S5 the relay protocol | `PushRelayWakeRequestV1` (`conch.push_relay_wake.v1`): `provider`, `app`, `token`, `wake` (S1), `collapse`, `expires_at`; `PushRelayWakeResponseV1`: `outcome` (`sent`, `rejected`, `retry`), `reason` | the relay's own shapes live here too so that conchd and the relay cannot drift; the relay is a second program, not a second schema |
| S6 the shared mention rule | `schema.Mentions(body, name string) bool` with a golden table in `testdata/` that the web client's `mentions()` is checked against (as `ValidateDisplayName` is the one rule for names) | a rule, not a wire shape; needed so that "mentions" means one thing on both sides |
| S7 additive fields | `OwnCredentialV1.Push` (bool; this sign-in has an active registration); `AuditEvent` actions `push_registration_created`, `push_registration_ended` | additive, no fixture change for existing files |

### 9.2 Server (#1116, server-engineer; `security-reviewer` pass required: new outbound requests, a member-supplied URL, a new read path)

- Store: `push_registrations` table and the `expired` transitions tied into `revokeCredential` and the reaper; the stale sweep.
- Routes: `GET`/`PUT /v1/notifications/preferences` (the caller's credential's; works with push off, and is the first slice to land because O1 needs it); `GET /v1/push/config`; `POST`/`GET`/`PUT`/`DELETE /v1/push/registrations[/mine|/{id}]`; `GET /v1/notifications/pending` (ascending, paged, `after` only). All human-only, all behind the account rate limiter.
- The policy evaluator at post time (membership + audience + the credential's preferences + S6), the bounded sender queue, the two transports with the pinned-address dialer (§5.7), the budgets, the metrics and the fixed-vocabulary failure classes.
- Flags (§3.6), the VAPID key generation into the data dir.
- Tests: a leak test row for the feed in the read-path matrix (ADR-005); `hide_members` rows (community-model §13.9); a structural guard that the feed reads only through the visibility functions; revoke-ends-registration and revoke-ends-preferences in the same transaction; endpoint refusal at registration (http, private, oversize) and **at dial time** (T16: a name that was public at registration and answers private at send; TTL-0 alternation; mixed A/AAAA with one private; IPv4-mapped and NAT64 forms; a redirect to a private host, not followed; and that the socket's peer is an address the check saw, with the hostname still verified); feed pagination (more than twenty matches plus arrivals during the drain: every match once, ascending, no gap, `more` correct on each page; the read mark and `after` both bound the start); budget and coalescing; provider-rejection expiry; nothing in logs or audit matches an endpoint or token (the ntfy test pattern). E2E: post → wake request observed at a fake transport → feed answers under a removed membership with nothing.
- Explicitly not: any approval notification; any change to ntfy.

### 9.3 Relay (a new program; only if D-1 or D-2 is yes)

A small Go service (its own issue, its own dependency decisions): accepts S5 with a per-server bearer, speaks APNs (HTTP/2, ES256 JWT refreshed "no more than once every 20 minutes and no less than once every 60 minutes" **[source: Apple token page]**) and FCM HTTP v1 (OAuth 2.0 from a service-account key with scope `https://www.googleapis.com/auth/firebase.messaging` **[source: FCM auth page]**), maps provider rejections to `rejected`, keeps no message content (it has none) and no logs beyond counters per server. Enrolment of servers (how an operator gets a relay token) is open (Q1).

### 9.4 Phone (#1117, after #1111/#1112/#1113; the stack is M1's)

- The preferences object (S8) read, written, mirrored locally and applied to in-app alerts from the socket; fetch-on-open. Both work with no push at all and ship first.
- Registration lifecycle: create on policy-on where a transport exists, `PUT` on token change and monthly, delete on sign-out and on OS permission denied, re-register when the server has forgotten it; local profile ↔ registration id map in secure storage.
- The wake handler: recognise the id (live profile or tombstone), sweep every live profile, drain each feed ascending with the page cap, persist each profile's cursor per page (§4.3); tombstones written on sign-out, delete and 401, pruned at 30 days (§5.2). The renderer's two non-summary outcomes exactly as §6.3 defines them: fetch-failed under `quiet_when_unreachable`; fetch-empty always suppressed where the platform allows. Device rows: two profiles offline across mentions on both (one wake, two notifications); the same with a local sign-out of one before reconnecting (tombstone, the other swept); 45 pending plus arrivals mid-drain (no gap, no duplicate); 120 pending (the cap line, the rest at open); member removed between wake and fetch, on the baseline iOS build (fixed line only) and on Android (nothing).
- iOS builds come in two flavours until D-9 is settled: the **baseline** (no filtering entitlement; empties show the fixed line) and, if Apple grants it, the **entitled** extension (`com.apple.developer.usernotifications.filtering` in the extension target; empties suppressed). The settings copy for the baseline says that a "New activity" line can appear with nothing new behind it.
- iOS: notification service extension that runs the sweep and drain within budget and renders summaries; the fixed fallback line; cancel-on-open reconciliation. Android: FCM receiver and UnifiedPush connector, the same renderer; `POST_NOTIFICATIONS` in context.
- Tap routing (§6.5); settings copy (M2); the "not available here" states (§6.6).
- Device evidence rows of §7, run on physical devices; emulator results are not evidence (#757).

### 9.5 Real-provider testing prerequisites (no keys, no private content)

- **Apple:** a Developer Program membership (US$99/year), an app id with the push capability, and, only if D-9 is yes and Apple grants it, the notification filtering entitlement on the extension's provisioning profile (it is requested through Apple's form, not switched on); an APNs **sandbox** key, a development-signed build on a physical iPhone, TestFlight for the alpha. The sandbox host is `api.sandbox.push.apple.com` **[source: Apple APNs request page]**. Keys live in the tester's keychain or a CI secret, never in the repository, fixtures or issue text.
- **Google:** a Play developer account (US$25 once) for Play builds; a Firebase project whose service-account JSON is a CI secret; a physical Android phone with Play services, and one without (or an emulator image without Play, for UnifiedPush only).
- **UnifiedPush:** the ntfy app (F-Droid flavour) as distributor against a self-hosted ntfy on a LAN with `--push-webpush-allow-private`, and against a public one; no ntfy.sh account needed.
- **Fixtures and transcripts:** test servers carry synthetic channels and names; screenshots of lock screens are taken on test accounts only; a device receipt records provider, OS version, app state and outcome, never a token.

## 10. Rejected and deferred alternatives

| Alternative | Verdict | Why |
| --- | --- | --- |
| Channel name or kind in the payload, encrypted to the device | rejected | content that outlives access; the fetch costs one request and is needed anyway for the summary |
| Count/badge in the payload | rejected for now | a count across channels is metadata about a private server; a badge can be set from the feed inside the extension later (deferred D-6) |
| Server-side per-device queue | rejected | a copy of content with its own lifecycle; the feed at fetch time has none |
| ntfy topics for chat (O7) | rejected | §3.3 |
| Expo's push service (O8) | rejected | §3.3 |
| Direct APNs/FCM in `conchd` (O2) | **deferred D-3** | §3.4 |
| Approval kind in the feed; approval wakes | **deferred D-4** | approval-path, D7 unchanged; its own issue with the full request → notify → resolve → audit test and Nick's merge |
| Browser preferences moved server-side | **deferred D-5** | its own web issue; this note does not change #840 |
| Badge counts, notification actions, inline reply, images | **deferred D-6** | each is content or a write from a notification; after the alpha |
| Reaction and membership-change notifications | deferred | not in the browser either |
| Provisional (quiet) iOS authorisation | rejected | a quiet mention is a missed mention |
| Audit row per wake attempt | rejected | volume without authority content; metrics and DEBUG instead; registration lifecycle is audited |
| Binding a registration to the principal, surviving credential rotation | rejected | the device-link rule is "a sign-out ends the ways in it would leave behind"; re-registering is one request |
| Preferences stored on the push registration | rejected (was the first draft) | a phone on a server with no transport has no registration and would have had nowhere to keep its policy and mutes for in-app alerts; the preferences are the credential's (§6.1) and a registration is only an address |
| Preferences per principal (one policy for all of a person's devices) | rejected | a phone wants `mentions` where a desk wants `all`, and a mute on one device should not silence another; it is also not the browser's model, which is per client |
| Per-registration FCM collapse keys so that one server's wake cannot replace another's | rejected | does not help on iOS, where APNs keeps one notification per bundle id regardless; runs into FCM's bound on distinct keys per app per device; the sweep of every profile on any wake (§4.3) makes collapse harmless instead |
| Fetching only the registration the wake names | rejected | with collapse per app, the wake that survives may not be the one whose server has news; the sweep costs one small authenticated request per profile |
| A newest-first feed of twenty, cursor advanced to the newest | rejected (was the first draft) | permanently skips older matches when more than twenty are pending; replaced by ascending pages drained before the mark moves, with a stated cap and line (§4.3 step 3) |
| Dropping the read-mark bound from the feed (notify regardless, cancel on open or on `unread: 0`, as the browser does) | rejected | it trades a content-free generic line on baseline iOS for a content-bearing notification on every platform for every message read elsewhere first, cleared only at the next open; and the feed would stop meaning what read state means everywhere else. The bound stays; the empties it causes are named in §4.3, §6.3 and D-9 |

## 11. Decisions for Nick

Nothing here is a dependency or a service until he says so. Each is phrased so that "yes" or "no" is a complete answer.

| # | Decision | Recommendation | What "yes" commits to |
| --- | --- | --- | --- |
| **D-1** | **Operate a managed Conch push relay** for the store-published apps (O3): a hosted service, an Apple Developer Program membership (US$99/year), a Google Play developer account (US$25 once), a Firebase project, and a way to hand out relay tokens to operators. | Yes, eventually: without it an iPhone on an independently self-hosted server has no push. It can wait until the iOS alpha (M15) needs it; the Android alpha can start on UnifiedPush. | a new external service the project runs; its own issue, threat model and abuse policy; the first recurring cost in the project |
| **D-2** | **Ship the relay as an operator-runnable program** too (O4), for operators who sign their own build. | Yes, it is the same program; the cost is documentation. | one more optional binary in the release |
| **D-3** | **Put direct APNs/FCM into `conchd`** behind flags in the first slice, instead of only through the relay. | No for the first slice (§3.4). | provider auth code and key files inside `conchd` |
| **D-4** | **Allow a later issue to add approval kinds** to the phone's feed and wakes. | Defer the decision itself; nothing now. | an approval-path issue later, Nick-merged |
| **D-5** | **Move the browser's notification preferences server-side** so phone and browser share one policy per server. | Later; not needed for the phone. | a web issue after the alpha |
| **D-6** | **Badges, actions, images, inline reply.** | After the alpha. | each its own issue |
| **D-7** | **Dependencies.** Confirm that `conchd`'s push slice must use only the standard library and the current allowlist (`golang.org/x/crypto` included) and that any other import is refused at `depgate`. | Yes. | #1116 reports if it cannot, before adding anything |
| **D-8** | **Schema issues S1–S8** (§9.1): all additive, no version bump; opening them is the protocol-designer's; this row records that they create wire shapes for a new, optional feature. | Approve opening them when #1111 (stack) is accepted. | nothing published changes |
| **D-9** | **Apply to Apple for the notification filtering entitlement** (`com.apple.developer.usernotifications.filtering`) so the iOS extension can show nothing when a wake finds nothing (§4.3 step 4). Without it, an empty wake shows a content-free "New activity" line on iOS, and empties are **ordinary**, not rare: the feed is bounded by the principal's read mark, so a message read at a desk first leaves the phone an empty fetch (§4.3 step 4). | **Design for the baseline without it, and apply as soon as the iOS app id exists, before the iOS alpha.** The design must be correct under Apple's discretion, not dependent on it; the generic line leaks nothing; the application costs a form, not a dependency; and because the empties are everyday ones for anyone who also uses a desk client, the baseline is acceptable for the alpha only with the §6.3 copy, not as the product. If granted, the entitled flavour in §9.4 replaces the baseline rows. | an application to Apple in the project's name; the extension target carries the entitlement; the §7.1 baseline rows stay documented for builds that lack it |

## 12. Open questions

- **Q1 Relay enrolment.** If D-1 is yes: how does an operator obtain a relay token (a sign-up page on the project's site; a token minted by Nick by hand for the alpha; anonymous with per-token rate limits)? The alpha can run on hand-minted tokens.
- **Q2 Which app ids may a managed relay serve?** Only the project's store builds, or also TestFlight/internal-testing builds of forks? Affects the relay's key set and its abuse surface.
- **Q3 Whispers under `mentions`.** This note treats a whisper to the reader as a mention (§6.2). The browser does not. Keep the stricter reading and later align the browser, or match the browser?
- **Q4 Should a live socket suppress wakes** server-side (§2.3)? Cheap to add, saves pushes; a slight timing signal if a provider could observe both, which it cannot. Proposed: yes, in the first slice if it falls out of the hub's bookkeeping, otherwise later. Since round 3 this has a second purpose: the commonest empty fetch is a message read at a desk first (§4.3 step 4), and "do not wake this principal's phones while one of their other credentials has a live events socket on that channel" would remove most of those before they are sent. It stays a heuristic (a desk socket does not mean the person is looking), never the correctness rule, and whether the principal-wide form is in the first slice is for #1116 to propose.
- **Q5 The idle-bounded credentials** (an operator's browser session has an idle bound; a phone's credential presumably will not, per M1's same-principal device policy). If the phone's credential is idle-bounded, a fetch from the extension must count as use or the registration would die under the person; which of the two is for M1 to say.
- **Q6 Force-quit behaviour on iOS** for the service extension is the one row this note could not settle from documentation; M5 (#1110) should include it even though push is #1117's, because it decides whether "force-quit means no notifications" must be in the settings copy.
- **Q7 The relay's own retention.** Counters per server only, or per-token delivery outcomes for support? The former is proposed.

## 13. Traceability: #1108 acceptance criteria to this note

| Criterion | Where |
| --- | --- |
| Compare OS push-provider and relay options, operator-owned credentials and optional managed relay; who operates each component, trust boundaries, metadata exposure, exact dependency/cost assumptions from primary sources | §3.1 table, §3.2, §4.4, §14 (each cost or limit carries its source or is marked assumption) |
| Device registration, per-server/account scoping, token rotation, logout/revocation, channel access loss, retries/deduplication, abuse limits, stale/offline devices | §5.1–§5.8 |
| Default to minimal opaque notifications; fetch authorized content after authentication; limits on recalling delivered OS notifications and on delivery while offline | §4.1–§4.3, §4.5 |
| off/mentions/all and channel mute behaviour, prompt timing, tap routing, multi-server behaviour, no push setup or provider outage | §6.1–§6.6 (preferences per credential, usable with no transport; a wake sweeps every profile; taps route by local profile, never across servers) |
| Schema/API work; backend versus mobile tasks; real-provider testing prerequisites without keys or private content | §9.1–§9.5 |
| Record the chosen design and owner approvals before closing; rejected/deferred alternatives; no provisioning, production messages or dependency | §0, §3.5, §10, §11 (approvals pending: this note is Proposed until Nick answers §11); nothing was provisioned or sent, and no dependency was added |
| Required tests: threat-model notification disclosure, revoked membership, token reassignment, cross-server taps | §8 T1–T7 |
| Required tests: failure scenarios for suspended, terminated and force-stopped apps separately, without claiming guaranteed delivery | §7.1–§7.3 |
| Required tests: validate references to current preference semantics | §1.1 (each semantic cites the file and symbol at `7472b65`) |

## 14. Sources

Repository, at `main` `7472b65` (2026-11-01): `web/src/useNotifications.ts`, `web/src/NotificationSettings.tsx`, `web/src/api.ts` (lines 1912–1943), `web/src/mention.ts`, `web/src/platform/types.ts`; `internal/server/approvals/ntfy.go`, `internal/server/approvals/approvals.go`; `internal/server/credentials.go`, `internal/server/store/credentials.go`, `internal/server/events_ws.go`, `internal/server/store/audience.go` (function list), `internal/server/ratelimit.go` (presence); `pkg/schema/credential.go`, `channels_changed.go`, `read_state.go`, `ws_ticket.go`, `auth.go`, `sign_in_link.go`, `revoke_all.go`, `doc.go`, `name.go`; `cmd/conchd/main.go` (ntfy flags); `go.mod`, `deps-allowlist.txt`; docs: ADR-000, ADR-002, ADR-005, ADR-007, ADR-008, ADR-013, `approval-object.md` §5, `client-experience.md` §4, `community-model.md` §5, §13.2, §13.8, `membership-live.md` §1, `guests-and-accounts.md` (device-link routes), `linux-desktop.md` and `windows-desktop.md` (notification adapters, for format only).

Providers and standards, read 2026-11-01:

- Apple, [Sending notification requests to APNs](https://developer.apple.com/documentation/usernotifications/sending-notification-requests-to-apns): 4 KB payload; `apns-collapse-id`, `apns-expiration`, `apns-priority`, `apns-push-type`; "stores only one notification per bundle ID"; "30 days or less"; best effort; hosts and HTTP/2.
- Apple, [Handling notification responses from APNs](https://developer.apple.com/documentation/usernotifications/handling-notification-responses-from-apns): 410 `Unregistered` with timestamp; do not retry `BadDeviceToken`, `Unregistered`; 429 `TooManyRequests`.
- Apple, [Establishing a token-based connection to APNs](https://developer.apple.com/documentation/usernotifications/establishing-a-token-based-connection-to-apns): `.p8` key, ES256, 20–60 minute refresh, team-scoped and topic-specific keys.
- Apple, [Modifying content in newly delivered notifications](https://developer.apple.com/documentation/usernotifications/modifying-content-in-newly-delivered-notifications): `mutable-content: 1`, "about 30 seconds", original shown on timeout, not employed when alerts are disabled.
- Apple, [com.apple.developer.usernotifications.filtering](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.developer.usernotifications.filtering): "receive remote notifications without displaying the notification to the user"; empty `UNNotificationContent`; requires `apns-push-type: alert`; "To apply for this entitlement, see Request Notification Service Entitlement".
- Apple, [Pushing background updates to your app](https://developer.apple.com/documentation/usernotifications/pushing-background-updates-to-your-app): not guaranteed, "two or three per hour", force-quit discards, only the newest held.
- Apple, [Asking permission to use notifications](https://developer.apple.com/documentation/usernotifications/asking-permission-to-use-notifications): one prompt; provisional; ask in context.
- Apple, [Sending web push notifications in web apps and browsers](https://developer.apple.com/documentation/usernotifications/sending-web-push-notifications-in-web-apps-and-browsers) and WebKit, [Web Push for Web Apps on iOS and iPadOS](https://webkit.org/blog/13878/web-push-for-web-apps-on-ios-and-ipados/): iOS 16.4 Home Screen web apps; no invisible pushes; user gesture.
- Apple Developer Program fee, US$99 per membership year: Apple's [Choosing a Membership](https://developer.apple.com/support/compare-memberships) and [enrollment help](https://developer.apple.com/support/enrollment/) pages, located through search on 2026-11-01; the comparison page's text did not render the figure to the fetcher, so this is **[source via search summary]**, to be re-checked when the account is opened.
- Google, [Manage FCM registration tokens](https://firebase.google.com/docs/cloud-messaging/manage-tokens): stale after a month; `UNREGISTERED`; save with a timestamp; refresh monthly.
- Google, [Receive messages in an Android app](https://firebase.google.com/docs/cloud-messaging/android/receive): foreground/background handling; `onDeletedMessages` at 100 pending or a month silent.
- Google, [Message priority on Android](https://firebase.google.com/docs/cloud-messaging/android/message-priority): Doze; deprioritisation of non-visible high-priority pushes.
- Google, [Authorize send requests](https://firebase.google.com/docs/cloud-messaging/auth-server): service-account JSON, scope.
- Google, [FCM message types](https://firebase.google.com/docs/cloud-messaging/customize-messages/set-message-type) and [About FCM messages](https://firebase.google.com/docs/cloud-messaging/concept-options): 4096 bytes; four-week default lifetime; collapse keys (read through search excerpts; the concept page answered 404 to a direct fetch).
- Google Play Console help, [Register for a Google Play Developer account](https://support.google.com/googleplay/android-developer/answer/6112435): "US$25 one-time registration fee".
- UnifiedPush, [Android specification AND_3.1.0](https://unifiedpush.org/developers/spec/android/) and [FAQ](https://unifiedpush.org/users/faq/): roles, RFC 8030/8291/8292, 4096-byte message, 1000-byte endpoint, no iOS.
- IETF, [RFC 8030](https://www.rfc-editor.org/rfc/rfc8030.html): TTL, Urgency, Topic, 4096-byte floor, capability URLs, "a push service can inspect and modify the message content" without end-to-end protection; RFC 8291 and RFC 8292 cited by number.
- Matrix, [Push Gateway API](https://spec.matrix.org/latest/push-gateway-api/): the gateway role, `event_id_only`, rejected pushkeys.
- ntfy, [Configuration](https://docs.ntfy.sh/config/) (iOS upstream: "only the message ID … and the SHA256 checksum of the topic URL"; FCM only for ntfy.sh in the Play flavour), [Publishing](https://docs.ntfy.sh/publish/) (4,096 bytes; ntfy.sh 250 messages/day; 12 h cache), [Phone apps](https://docs.ntfy.sh/subscribe/phone/) (instant delivery is a foreground service; F-Droid flavour has no Firebase).
- Expo, [Sending notifications with Expo's push API](https://docs.expo.dev/push-notifications/sending-notifications/): `exp.host` relay, 4 KiB, receipts, `DeviceNotRegistered`, direct APNs/FCM alternative.
- Android, [Notification runtime permission](https://developer.android.com/develop/ui/views/notifications/notification-permission): Android 13 `POST_NOTIFICATIONS`, off by default, ask in context.
