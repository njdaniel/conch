# Everyday usability audit

**Issue:** #699. **Reviewed commit:** `06a906059a8f6d5f6d8b2dcb4d737a5ba2f1426c` (`06a9060`, `issue-699/principal`, exactly `origin/main`, “feat(web): add private image attachments (#762) (#837)”). **Date:** 2026-10-25.

This is a comparison with everyday communication-app workflows, not another general browser walkthrough. It reuses #577's [first browser audit](browser-ux-audit.md) and #626's [second browser audit](browser-ux-audit-2.md); their findings are not repeated here. In particular, #596's voice-occupancy work and the second audit's voice-state findings are incorporated by reference.

## Evidence and priority

- **Running:** this review ran the repository's real-server Chromium workflows at the reviewed commit. A check marked running is evidence from those browser interactions and API assertions, not merely from reading the test.
- **Code:** current-commit source inspection. A code-derived reproduction is identified as such and was not presented as a browser observation.
- **Earlier running:** evidence from #577/#626 at their named older builds, used only where current source still implements the same behavior. Real microphones, other browsers, and deployed-device acceptance remain #474/#718/#449 work.

Priorities below are follow-up priorities, not claims that every familiar feature is required: **P1** = strongest everyday-usability candidate after the current alpha gate; **P2** = valuable next increment; **P3** = later polish or an explicit product choice. **—** means no follow-up is indicated by this review. Statuses are **implemented and discoverable**, **backend-only**, **confusing or defective**, **missing**, and **intentionally deferred**. A row can name a deliberate deferral as rationale without hiding the present status.

### Running checks at `06a9060`

With Node 24.21.0 and the repository's pinned Playwright/Chromium, from `web/`:

```sh
npm run e2e -- 20-create-channel.spec.ts 21-channel-rename.spec.ts \
  24-channel-managers.spec.ts 11-invites.spec.ts 07-lasting-sign-in.spec.ts \
  22-guest.spec.ts 06-read-state.spec.ts 02-messages.spec.ts 09-restart.spec.ts
# 25 passed (32.3s)

npm run e2e -- 19-voice-devices.spec.ts
# 4 passed (28.8s)
```

These use disposable local servers and data. Voice checks use Chromium fake devices and the pinned LiveKit; they establish interface/device behavior, not real-hardware audio quality. The first attempted run could not start because this clean worktree had no installed Playwright; the standard pinned `npm ci --no-audit` fixed that environment problem. No test failure was skipped.

## Alpha gate: owner and ordinary member

**Result: PASS on the local reviewed build.** This is not a substitute for #449's deployed exit scenario or #718's acceptance record.

| Gate action | Result and running evidence |
| --- | --- |
| Owner invites a friend | **Pass.** `11-invites.spec.ts` makes and redeems operator invites on desktop and phone, covers no-voice and dead-link states, and makes/closes an open link. It also separately verifies an authorized ordinary member's invite. Current controls are `web/src/InvitePanel.tsx:76-105,172-208,238-252`. |
| Owner creates a channel | **Pass.** `20-create-channel.spec.ts:33-66` creates through **New channel**, opens it immediately, verifies persistence after reload and no voice join. Ordinary member gets no control and a direct request is refused (`:68-81`). |
| Owner renames a channel | **Pass.** `21-channel-rename.spec.ts:74-140` renames through the browser, updates a second live client, survives reload, and keeps later conversation live. The voice rename case at `:222-282` also passed. Ordinary member has no control and is refused server-side (`:174-190`). |
| Owner controls access | **Pass.** `24-channel-managers.spec.ts:40-110` delegates a manager; the manager adds/removes people and renames, while an ordinary member sees no administrative controls. UI and role boundaries are in `web/src/Members.tsx:177-248,272-299`; server routes are operator/channel-manager guarded at `internal/server/routes.go:54-57`. |
| Owner/friend starts a conversation | **Pass.** `02-messages.spec.ts:16-57` sends from the browser, appears once to another member and in history, and stays singular after reload. Invite redemption lands the friend in the allowed channel set in `11-invites.spec.ts`. |

“Owner” in this report means the browser's operator/owner workflow. A delegated channel manager is intentionally neither an ordinary member nor a server owner. An ordinary member can chat, read, use voice, manage their own profile/sign-ins, and invite only if granted; they cannot create/rename a channel or alter membership without the appropriate grant. A temporary guest has still narrower, explicitly explained rights.

## Comparison matrix

### Channels

| Item | Status | Priority and rationale | Current evidence |
| --- | --- | --- | --- |
| Create a channel | **Implemented and discoverable** | **—.** Owner alpha gate passes; creation is operator-only and does not implicitly join voice. | **Running:** `20-create-channel.spec.ts:33-81`. Control/form: `web/src/CreateChannel.tsx:83-116`; route: `internal/server/routes.go:26`. |
| Rename a channel | **Implemented and discoverable** | **—.** Owner and delegated manager paths pass; stale concurrent renames are explained. | **Running:** `21-channel-rename.spec.ts:74-190,222-282`; manager path in `24-channel-managers.spec.ts:103-110`. `web/src/RenameChannel.tsx:117-151`. |
| Control membership/access | **Implemented and discoverable** | **—.** Add by exact name/from another channel, remove, and manager delegation work with ordinary-member restrictions. | **Running:** `24-channel-managers.spec.ts:40-110`. `web/src/Members.tsx:127-174,177-248`. |
| Channel description/topic | **Missing** | **P2.** A new joiner cannot learn a channel's purpose or rules in context; this becomes useful as invited groups multiply. No backend field exists either. | **Code:** `ChannelV0` has only id/name/created/manages (`pkg/schema/channel.go:10-21`); the conversation header renders only the name (`web/src/Conversation.tsx:351-360`). |
| Group or reorder channels | **Missing** | **P3.** Useful for large servers, not needed for the small private-alpha channel sets. | **Code:** list response is explicitly id-ascending (`pkg/schema/channel.go:41-45`) and the browser maps that flat array directly (`web/src/ChannelList.tsx:119-160`). No grouping/order preference appears in Settings (`web/src/Lobby.tsx:342-430`). |
| Archive/delete a channel | **Missing** | **P2.** Owners can create and rename but cannot retire a room, so mistakes and obsolete groups accumulate. Deletion needs audit/history policy, not just a button. This is distinct from #698's member/guest/message moderation. | **Code:** channel routes provide create/list/rename/member operations but no channel archive/delete (`internal/server/routes.go:26-37,52-64`). The only DELETE there removes a member; net archive is not channel archive. |

### Messaging

| Item | Status | Priority and rationale | Current evidence |
| --- | --- | --- | --- |
| Send and receive ordinary chat | **Implemented and discoverable** | **—.** The core conversation path is reliable in the exercised workflow. | **Running:** `02-messages.spec.ts:16-57`. Rendering and composer: `web/src/Conversation.tsx:403-450`, `web/src/Composer.tsx:248-382`. |
| Edit one's own message | **Missing** | **P2.** Typos are a frequent everyday case. It needs revision/audit and live-client semantics; #698 covers moderation/removal, not self-edit. | **Code:** message routes are POST/GET plus search, with no PUT/PATCH (`internal/server/routes.go:38-46`); a rendered message has no edit control (`web/src/Conversation.tsx:111-154`). |
| Delete/remove a message | **Missing** | **P1 for moderation, tracked.** Inappropriate content currently cannot be removed. #698 already owns the design including authorization, history and audit; do not file a duplicate. | **Code:** no message DELETE route (`internal/server/routes.go:38-46`) and no message control (`web/src/Conversation.tsx:111-154`). Existing: **#698**, open in Private alpha. |
| Replies/threads | **Backend-only** | **P2.** The public API and CLI support one-level audience-preserving threads, but browser users cannot reply or open a thread. Browser threads are explicitly after-alpha in the current guide, so this is not an alpha-gate regression. | **Code:** `?thread=` listing (`internal/server/messages.go:205-230`), reply posting (`:494-496,652-724`), CLI (`internal/cli/command.go:111-114,190-198`). Browser API exposes list/post but no thread call (`web/src/api.ts:1480-1520,1575-1599`), and messages have no reply action (`web/src/Conversation.tsx:111-154`). Existing backend: **#25** (closed). |
| Mentions | **Missing** | **P2.** There is no addressed-person workflow or mention-specific unread/notification. It should be designed with notifications rather than as text highlighting alone. Reactions/mentions are later roadmap primitives. | **Code:** composer offers channel/net/whisper audiences but no mention completion (`web/src/Composer.tsx:138-160,260-312`); read state has only a single per-channel mark/count (`web/src/useReadState.ts:1-5`). Roadmap: `ROADMAP.md:323-327`. |
| Reactions | **Missing, already tracked** | **P3 / alpha-polish candidate.** Useful expression, but explicitly not a launch gate. **#752** epic; **#764** contract, **#765** backend, **#766** browser, **#767** terminal. Do not file another reaction issue. | **Code:** message rendering has body/approval/attachments only (`web/src/Conversation.tsx:111-154`). #752 calls reactions alpha-polish candidates, not launch gates. |
| Paginated history and return to latest | **Implemented and discoverable** | **—.** Older pages preserve position; new messages while reading back offer **Jump to latest**. | **Running:** history/reload assertions in `02-messages.spec.ts`; reconnect history in `09-restart.spec.ts`. **Code:** `web/src/Conversation.tsx:298-349,390-426`; paging API `web/src/api.ts:1480-1520`. |
| Search | **Backend-only** | **P2.** Long-lived invited groups need retrieval; the server and CLI already enforce scoped search. The guide explicitly says browser search is after alpha. | **Code:** authorized search route `internal/server/routes.go:46`; visibility-preserving handler `internal/server/messages.go:315-371`; CLI `internal/cli/search.go:38-57`. Browser has history calls but no search call (`web/src/api.ts:1480-1520`) and no search UI in the conversation (`web/src/Conversation.tsx:351-450`). Existing backend: **#22** (closed). |
| Unread state while channels change | **Implemented and discoverable** | **—.** Per-person counts and shared read marks clear correctly across clients. | **Running:** `06-read-state.spec.ts:6-37`. Badge: `web/src/ChannelList.tsx:11-22`; read marking: `web/src/useReadState.ts:232-294`. |
| Live unread state for non-open channels | **Confusing or defective** | **P1.** While a reader stays in one channel, new messages in another do not increment its badge. Counts refresh on initial load, another channel open, or reconnect, so the badge can say zero despite unread activity. This undermines the primary quiet-chat cue even before background notifications are added. | **Running, confirmed at `06a9060`:** a temporary Playwright spec using the existing disposable real-server fixture created #random and two browser contexts. A signed in and stayed on #general; B opened only #random and sent two messages there. After A waited 5 seconds, A's server `GET /v1/read-state` reported `unread=2` for #random while A had **zero** #random unread badges. A still had `aria-current` on #general and status `Connected`; during the wait it made **0 new `/v1/read-state` requests and opened 0 new `/v2/ws` sockets**. The focused test passed (`1 passed`, 5.7 s test / 7.7 s run) and was removed afterwards. This matches `web/src/useReadState.ts:232-261`: no socket for other channels and local +1 only for the open channel. Existing `06-read-state` tests initial/read synchronization, not live arrival. No existing issue found in unread searches. |

### Profiles and notifications

| Item | Status | Priority and rationale | Current evidence |
| --- | --- | --- | --- |
| Display name and self-rename | **Implemented and discoverable** | **—.** Settings exposes **Change your name**; case/look-alike collisions and rate limiting have explicit feedback. | **Code:** `web/src/ChangeName.tsx:5-13,17-19,65-98`; route `internal/server/routes.go:33-35`. Existing: **#441** (closed). |
| Avatars | **Implemented and discoverable as generated initials; custom avatars missing** | **P3 only if custom images are wanted.** Names, colored initials and owner/manager cues cover identity in the alpha; uploaded avatars are not an everyday requirement worth blocking core gaps. | **Code:** deterministic initial/color avatar `web/src/Icons.tsx:78-84`, used in messages `web/src/Conversation.tsx:78-91` and account status `web/src/Lobby.tsx:223-229`. Principal shape/rename route carries no avatar upload (`internal/server/routes.go:32-37`). |
| Channel mute | **Missing** | **P2, with notifications.** A mute preference is meaningful once alerts/badges can be noisy; adding it without a notification model would be a dead control. | **Code:** Settings has account/sign-ins/server/sign-out/help only (`web/src/Lobby.tsx:342-430`); read state stores only last-read and unread count (`web/src/useReadState.ts:1-5`). |
| Background/browser notifications and preferences | **Missing** | **P1.** A member with Conch in a background tab gets no alert, and there is no mention-based alternative. The tester guide states this limitation, but discovery of quiet messages is a central everyday-chat gap. Design per-client preferences and private-content rules from UX-07. | **Code:** no notification preference in Settings (`web/src/Lobby.tsx:342-430`); only in-page unread state (`web/src/useReadState.ts`). Product documentation: `docs/alpha-tester-guide.md:241-243`; contract `docs/design/client-experience.md:96-100`. Native desktop #773/#779 and mobile/PWA push #27/#757 are related future work, not browser coverage; native and mobile push remain out of scope for this browser candidate. |
| Account/session preferences | **Implemented and discoverable** | **—.** Password, sign-ins, sign-out scope and server/guest administration are separated and explained. | **Running:** `07-lasting-sign-in.spec.ts`; guest conversion/password sign-in in `22-guest.spec.ts`. **Code:** `web/src/Settings.tsx:81-272`; `web/src/Lobby.tsx:342-430`. |

### Voice

The old F10 conclusion is stale: #658–#661 have since merged. The running device suite passed all four cases. #597 remains an open tracking issue, so no new device-selection/volume issue is needed. #596 occupancy and #626's detailed reconnect findings are reused rather than re-audited here.

| Item | Status | Priority and rationale | Current evidence |
| --- | --- | --- | --- |
| Microphone and speaker selection | **Implemented and discoverable** | **—.** Voice options lists devices, identifies the active microphone, preserves choices for the tab, and never silently substitutes a disappeared microphone. Device choice before joining was deliberately excluded by #658. | **Running:** `19-voice-devices.spec.ts:203-302,352-388`. `web/src/voice/VoicePanel.tsx:650-742,796-824`. Existing: **#658/#660**, closed; **#597** tracker open. |
| Microphone test / loopback | **Missing** | **P2.** The level meter helps only after joining and is most visible in open-microphone mode; a user cannot privately verify the selected mic/headset before talking to others. This is not covered by #658's device-selection scope. | **Code:** device controls render only while in voice (`web/src/voice/VoicePanel.tsx:497`); meter is beside open microphone (`:368-372`); Options has devices/volumes/mode/processing but no test/loopback (`:796-835`). No mic-test issue found. |
| Own mute/deafen | **Implemented and discoverable** | **—.** Clear pressed states; deafen also mutes and the talk control explains why it sends nothing. | **Code:** `web/src/voice/VoicePanel.tsx:379-394,451-457`; **earlier running:** #626 F09/F13 and #643's follow-up are in the second audit. |
| Others' mute/deafen state | **Intentionally deferred** | **P2.** Requires self-reported presence/schema work and a product decision; local mute cannot be inferred reliably from LiveKit under PTT. **#598** is the existing proposal; do not duplicate it. | **Code:** roster renders transmitting/speaking/sharing/volume, not mute/deafen (`web/src/voice/VoicePanel.tsx:547-575`). Existing: **#598**, open; #577 F11 and #626 F11. |
| Individual listener volume | **Implemented and discoverable** | **—.** Per-person local sliders persist through output changes and rejoin; another client's playback is unaffected. | **Running:** `19-voice-devices.spec.ts:390-436`. `web/src/voice/VoicePanel.tsx:590-629,809-813`. Existing: **#661**, closed. |
| Speaking/transmitting indicators | **Implemented and discoverable** | **— for fake-device UI.** Real-microphone speaking behavior remains manual acceptance, not a new issue. | **Code:** `web/src/voice/VoicePanel.tsx:88-94,568-574`; **earlier running:** #626 F21 distinguishes a Chromium fake-beep limitation from actual tone/speaking behavior. Real devices: **#474**. |
| Reconnect and unavailable states | **Implemented and discoverable** | **—.** Server reconnect preserves history and voice UI distinguishes reconnecting/unavailable/cut-off states. Token-in-console follow-up **#646** and visual polish **#647** remain separate existing work. | **Running:** `09-restart.spec.ts` passed; current voice rename/reconnect case `21-channel-rename.spec.ts:222-282` passed. **Code:** phase labels `web/src/voice/VoicePanel.tsx:48-54`, cut-off/unavailable notes `:433-492,636-638`. **Earlier running:** #626 F06/F27/N06 and subsequent #644. |

### Joining, guests, and account continuity

#694 is now closed, and its browser child #713 plus integration children have merged. Therefore the issue's original “guest coverage blocked on #694/#713” note is **stale**: this review includes running guest coverage. #718 still owns deployed acceptance.

| Item | Status | Priority and rationale | Current evidence |
| --- | --- | --- | --- |
| Invite and first join | **Implemented and discoverable** | **—.** Account invite, open/personal links, channel/voice scope, ordinary inviter grant, and dead-link behavior work. | **Running:** `11-invites.spec.ts` five relevant cases passed. `web/src/InvitePanel.tsx:76-105,184-208`; redemption API `web/src/api.ts:948-1059`. Existing foundation: **#440**; prior UX #577/#626 F18/F19. |
| Return login and sign-out recovery | **Implemented and discoverable** | **—.** Lasting member sign-in, password return, fresh-browser login, sign-out everywhere and operator sign-in-link recovery exist. | **Running:** `07-lasting-sign-in.spec.ts`; converted member signs in from fresh browser and wrong password is refused in `22-guest.spec.ts:125-183`. **Code:** front door `web/src/SignIn.tsx:1-13,135-148,202-220`; sign-out warnings `web/src/Lobby.tsx:38-47,361-420`. Existing: **#493/#707–#714**. |
| Guest identity and lifecycle | **Implemented and discoverable** | **—.** The reserved visible guest identity, tag, tab-scoped visit, refresh survival, explicit end and retained attribution are all exercised. | **Running:** `22-guest.spec.ts:65-123,211-236`. Explanations and controls `web/src/Guest.tsx:42-113,190-280`. Existing design: **#694**; implementation **#705/#710/#713/#717**. |
| Guest-to-permanent-account conversion | **Implemented and discoverable** | **—.** Conversion preserves principal/access, retains old guest attribution, replaces credentials, and supports later password login; lost-answer recovery passed. | **Running:** `22-guest.spec.ts:125-209`. `web/src/Guest.tsx:190-230`; conversion API `web/src/api.ts:1252`; route `internal/server/routes.go:136-139`. Existing: **#711/#713**. |
| Guest limits and history boundary | **Implemented and discoverable** | **—.** Guests cannot administer membership; conversion does not silently import earlier private history. Deployed cross-group/guest acceptance remains #718, not a missing feature. | **Running:** guest workflows above; member/guest administrative distinctions in `24-channel-managers.spec.ts`. **Code:** `web/src/Members.tsx:272-289`; guest conversion says “this visit’s conversations and nothing more” (`web/src/Guest.tsx:201-206`). Existing: **#706/#719/#720**. |

## Confirmed nonduplicate follow-ups

The duplicate checks covered #577, #626, #598, #596, #441, #493, #694, the full Private alpha milestone, and keyword searches for each feature. Existing #695–#698 and #702 children are intentionally excluded. The confirmed candidates were filed as the following issues; sizes are planning estimates, not commitments.

| Issue | Priority / size | Why it is not a duplicate; acceptance direction and dependencies |
| --- | --- | --- |
| **#839** Keep unread badges live for every visible channel | **P1 / M** | #351/#373 built stored marks and static badges, while `06-read-state` does not test background-channel arrival. Acceptance: B posts to a non-open channel and A's badge increments promptly without channel switch/reload; read marks remain authoritative, own messages do not increment, revocation/rename/reconnect stay correct. Needs an authorized cross-channel message feed or explicit polling design; preserve guest history and scoped-message isolation. |
| **#841** Browser message search using existing scoped API | **P2 / M** | #22 is closed backend/CLI work, not browser delivery. Acceptance: search current channel, paginate, open result context, no widening of net/whisper/guest visibility, reconnect/error states. Depends only on existing API; add web API/UI/tests and real-server isolation coverage. |
| **#842** Browser one-level replies using existing thread API | **P2 / M–L** | #25 is closed backend/CLI work; no browser issue found. Acceptance: reply from keyboard/touch, root/reply view, audience inherited and unchangeable, unread/history/reconnect behavior, guest and scoped-audience boundaries. Depends on accepted presentation/unread semantics; do not invent nested threads. |
| **#840** Browser notifications, mentions, and per-channel mute/preferences | **P1 / L** | #773/#779 are future native desktop work and #27/#757 cover mobile/PWA push, not this browser notification candidate. Design one coherent feature rather than independent cosmetic @ highlighting or dead mute controls. Acceptance: permission/off states, mention and channel policies, no private content after access loss, background-tab behavior and accessible in-app fallback. Depends on #839's live unread/activity delivery and UX-07 privacy rules. Native desktop and native/mobile push are explicitly out of scope. |
| **#843** Channel description/topic | **P2 / S–M** | #695/#696 cover create/rename only. Acceptance: authorized owner/manager edits optional bounded topic, joiners see it in channel context, live/reload update, audit and safe rendering. Requires additive schema/server design before UI and Nick's schema sign-off. |
| **#844** Retire (archive first) a channel | **P2 / M** | #698 concerns member/guest moderation and message removal, not channel lifecycle. Acceptance: operator archives/restores with clear membership/read/post/voice behavior, no silent history destruction, live clients update, audit and confirmation. Deletion can remain a later separate decision. |
| **#845** Edit one's own recent message | **P2 / M–L** | #698 covers moderation/removal, not self-editing. Acceptance: author-only edit with visible edited state, live/history consistency, audit/revision policy, immutable approval semantics, scoped visibility preserved. Needs product decision on time window/history and additive schema; coordinate with #698. |
| **#846** Private microphone/loopback test | **P2 / M** | #658–#661 implement selection and volume, not a prejoin test; #597's F10 tracker does not specify loopback. Acceptance: choose and hear/test mic without joining a room or transmitting to others, obvious stop/cleanup, denied/missing-device handling, no accidental recording/upload. Should share device lifecycle code with Voice options. |
| **#847** Channel grouping/reordering | **P3 / M** | No existing issue found and not needed for the current small alpha. If pursued: per-user ordering, keyboard/phone parity, persistence and rename stability. Defer until actual channel counts justify it. |

**Not proposed:** message deletion (#698), reactions (#752/#764–#767), other people's mute/deafen (#598), device selection/volume (#597 and merged #658–#661), custom avatars (generated identity is adequate for now), or new guest/return-login tasks (#702 children and #718).

## Limits and handoff

- Running evidence is local Chromium at `06a9060`; fake voice devices do not validate real microphones, echo, mobile backgrounding, Firefox/Safari, or deployed onboarding. Those remain #474, #449 and #718.
- The non-open-channel unread defect is confirmed by the focused two-client runtime reproduction above at `06a9060`: server unread 2, browser badge absent after 5 seconds, with no channel switch, reconnect, new read-state request or new message socket. The P1 candidate verdict is unchanged.
- The alpha gate result is a local interface/API gate, not a production or deployed-release claim.
- This review changed no product code. The confirmed nonduplicate follow-ups are #839–#847, linked above; the only repository deliverable is this document.
