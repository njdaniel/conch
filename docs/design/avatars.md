# Design: user avatars (profile pictures)

- **Status:** Proposed by the protocol-designer, 2026-11-04, for Nick's review. Issue [#974](https://github.com/njdaniel/conch/issues/974) ("Web: user profile pictures and avatar tokens"), which was not groomed; this note is its grooming. No implementation is authorized by this note. §7 lists what needs Nick; §8 is the slice plan, each slice written to be filed as an executable-cold issue.
- **Builds on:**
  - [ADR-001](../adr/ADR-001-mcp-native-architecture.md) (parity: the CLI is a client of the public API; no MCP-only capability);
  - [ADR-007](../adr/ADR-007-client-interfaces.md) (the browser client, served by `conchd` with its CSP);
  - [ADR-009](../adr/ADR-009-guests-and-open-joining.md) and [ADR-013](../adr/ADR-013-one-community-model.md) (one community; the member directory; guests off for now, data kept);
  - [ADR-014](../adr/ADR-014-fixed-roles-configurable-permissions.md) and [admin-roles.md](admin-roles.md) (`manage_members`, `see_member_directory`, the route classes of §7.2);
  - [community-model.md](community-model.md) §13 (who may know whom: the predicate, the authors route, the directory signal);
  - [membership-live.md](membership-live.md) (content-free signals on the events socket);
  - [browser-image-attachments.md](browser-image-attachments.md) (the browser's raster validation and the "fetch, validate, `blob:`" rendering contract);
  - [moderation.md](moderation.md) §3 (server-side authorization, confirmation, audit rules for an operator's removal).
- **Owner:** protocol-designer (this note and the schema slice); server-engineer, web-engineer and cli-engineer for the slices of §8.
- **Verified against:** `origin/main` at **`d7afd8c7`** (merge of #1304), inspected 2026-11-04 in a worktree on that commit. Every path, function and line below was checked there. Line numbers drift; names do not.

## 0. The design in five lines

1. A person sets, replaces or removes **their own** avatar with `PUT` / `DELETE /v1/principals/{id}/avatar`; an operator holding `manage_members` may `DELETE` anyone's (owner-protected). Nobody else writes it, and no MCP tool is added.
2. The server **decodes** the upload with stdlib `image/png` and `image/jpeg` only, refuses everything else by content, **re-encodes** it as one square PNG of at most 256 px, which strips every metadata chunk, and stores those bytes in a new SQLite table beside the principal.
3. The avatar appears on the wire as **one optional field, `avatar_sha256`**, on the four documents that already name a principal to a viewer (`ServerMemberV1`, `ChannelMemberV1`, `ChannelAuthorV1`, `WhoAmIResponseV1`); the bytes come from `GET /v1/principals/{id}/avatar`, behind **the same visibility rule as the name**. No version bump: every change is additive.
4. A change propagates through the two signals a rename already sends (`notifyRosters` and `conch.directory_changed.v1`); clients key their image cache on `(id, sha256)` and fall back to today's initial.
5. Clients render a fetched, validated, `blob:` image in the existing `Avatar` component; the TUI keeps the name.

## 1. Current state at `d7afd8c7`

### 1.1 Merged behaviour (what exists)

| Area | What exists | Where (file, function) |
| --- | --- | --- |
| Identity on the wire | A principal has `id`, `kind`, `name`, `created_at` and nothing visual. No display-name, bio or avatar column: `name` is the only display name. | `pkg/schema/principal.go` `PrincipalV0`; `internal/server/store/store.go` migration 1 (`principals`), later columns `role`, `disabled_at`, `invited_by`, `invite_id`, `may_invite`, `voice_allowed`, `name_key`, `name_skel`, `hidden_at`, `auth_generation`, `owner` |
| The four documents that name a principal to a viewer | `ServerMemberV1` (directory), `ChannelMemberV1` (member list, REST and the `conch.channel_members.v1` frame), `ChannelAuthorV1` (departed authors), `WhoAmIResponseV1` (self). Messages carry `author_id` only. | `pkg/schema/directory.go`, `channel_member.go`, `channel_author.go`, `whoami.go`; `pkg/schema/message_v2.go` `MessageV2.AuthorID` |
| Self-profile route | Rename only: `PUT /v1/principals/{id}/name`, class `accessOperatorOrSelf`; a holder of `manage_members` renames anyone (owner-protected), anyone else only themself; self-renames limited to `renamesPerDay = 5`, `renameAttemptsPerDay = 20`, counted from audit rows. No `/v1/me`, no `PATCH`. | `internal/server/routes.go` (route table); `internal/server/principals.go` `handleRenamePrincipal`, consts at the top of the file; `internal/server/store/rename.go` `RenamePrincipalAs`, `setNameTx`, `renameLimitTx`, `RecordRenameRefused` |
| How a rename propagates | After commit: `s.events.notifyRosters()` (every events socket rebuilds its roster and member list and sends `conch.channel_members.v1` if the bytes changed) and `s.directoryPrincipalChanged(ctx, id, nil)` (`conch.directory_changed.v1` to every viewer who may know the principal). | `internal/server/principals.go` end of `handleRenamePrincipal`; `internal/server/events_ws.go` `notifyRosters`, `membersFrame`, `sendMembers`; `internal/server/directory_signal.go` `directoryPrincipalChanged`; `internal/server/store/directory.go` `DirectoryKnowers` |
| Who may know whom | One predicate: `knowsCond` / `knownPrincipalsSQL` (self, a shared channel, one's own invitees), bypassed by `see_member_directory` (on for members by default, fixed on for operators). The old `hide_members` setting was mapped once onto that member cell by `initializePermissions`; no runtime code reads it. `mayKnowTx` gates a manager's add. | `internal/server/store/directory.go` `knowsCond`, `knownPrincipalsSQL`, `mayKnowTx`, `ListDirectory`; `internal/server/store/permissions.go` `initializePermissions`; `pkg/schema/permission.go` `PermSeeMemberDirectory` |
| Departed authors | `GET /v1/channels/{channel}/authors?ids=` names any asked id that authored a message the caller may read in that channel, whatever the directory says (community-model §13.5). | `internal/server/members.go` `handleListChannelAuthors`; `internal/server/store/audience.go` `ListChannelAuthors`, and its inverse `AuthorReaders` |
| Hidden principals | Hiding disables, removes memberships and renames to `hidden-<id>`; the directory lists enabled, non-hidden principals only. | `internal/server/store/hide.go` `HidePrincipal`; `pkg/schema/principal_admin.go` `HiddenPrincipalNamePrefix` |
| File uploads | `POST /v2/channels/{channel}/files?filename=` (raw body, humans only, members only, channel membership and post right), `GET /v2/files/{id}/meta`, `GET /v2/files/{id}`. Bytes on disk at `<DataDir>/files/<sha256[:2]>/<sha256>`, content-deduplicated, written through a temp file and `os.Rename`; metadata in table `files` with `channel_id NOT NULL`, `uploader_id`, `filename`, `media_type`, `size`, `sha256`, `message_id`. A file is attached to at most one message. **Nothing deletes or garbage-collects a file or a blob.** | `internal/server/files.go` `handleUploadFile`, `storeBlob`, `blobPath`, `filesDir`, `removeStaleUploads`, `fileForRequest`, `handleGetFile`; `internal/server/store/files.go` `InsertFile`; `internal/server/store/store.go` migration 16; `internal/server/store/audience.go` `FileVisibleTo`, `attachFilesTx` |
| Upload limits | `DefaultMaxUploadBytes = 10 << 20`; `--max-upload-bytes` / `CONCHD_MAX_UPLOAD_BYTES`, 0 disables uploads (403 `uploads_disabled`); enforced against `Content-Length` and by `http.MaxBytesReader` (413 `file_too_large`). No content-type restriction: the declared type is normalized and stored, never trusted. No per-person upload rate limit. | `internal/server/files.go` consts and `handleUploadFile`; `cmd/conchd/main.go` `parseServeArgs`; `internal/server/server.go` `Config.MaxUploadBytes`; `pkg/schema/file.go` `NormalizeMediaType` |
| File download contract | Always `Content-Type: application/octet-stream`, `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`, `Cache-Control: private, no-store`; no ETag. "A stored file is never rendered." | `internal/server/files.go` `handleGetFile`; `pkg/schema/file.go` `FileDownloadMediaType` |
| Image decoding on the server | **None.** No import of `image`, `image/png`, `image/jpeg`, `image/gif` or `golang.org/x/image` anywhere under `internal/`, `pkg/` or `cmd/` outside tests; `go.mod` carries no image dependency; no `http.DetectContentType`. | `go.mod`, `deps-allowlist.txt` |
| Audit | `audit_events` is append-only; actions are plain string constants beside their feature (`principal_renamed`, `principal_rename_refused`, `file_uploaded`, `principal_hidden`, ...), written inside the state-changing transaction by `appendAuditEventTx`. `AuditEventV1.Action` is an open string: a new action is additive. | `internal/server/store/approvals.go` `appendAuditEventTx`; `internal/server/store/rename.go`, `files.go`, `hide.go` consts; `pkg/schema/audit.go` |
| Route classes and helpers | `accessAuthenticated`, `accessOperator`, `accessOwner`, `accessChannelManager`, `accessOperatorOrSelf`, `accessPermission` (`.needs(perm)`), `accessExempt`; `guard`; `can`, `canOrFail`, `ownerProtected`, `denyForbidden`, `denyForbiddenFor`; wrappers `humansOnly`, `humansOnlyFiles`, `membersOnly`. `recentAuthRoutes` lists the routes that need a recent proof; rename is not in it. | `internal/server/auth.go`; `internal/server/authz.go`; `internal/server/routes.go` |
| Guests | Off: `guestsRemoved = true`; no route starts a visit. The data and the v1 guest shapes stay (`GuestAttributionV1` on `ChannelMemberV1.Guest` and `MessageV2.GuestAuthor`). A guest's name is its handle and cannot be renamed. | `internal/server/join.go`; `pkg/schema/guest.go`; community-model §0.1 and §14 |
| Browser CSP | `default-src 'self'; connect-src 'self'; img-src 'self' data: blob:; style-src 'self'; script-src 'self'; object-src 'none'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`. `blob:` in `img-src` exists for attachment previews (#762). Desktop: `img-src 'self' blob: data:` where `'self'` is the `conch-app:` scheme. | `internal/server/static.go` `webCSP`, `webCSPFor`; `desktop/main/csp.ts` |
| Browser auth | Bearer header only, `credentials: 'omit'`, `cache: 'no-store'`; the token is never put in a URL; the server has no cookie auth. So a plain `<img src="/v1/...">` cannot authenticate. On desktop the renderer never holds the token: downloads go through the main-process bridge, which allows exactly `uploadFile` and `fileBytes`. | `web/src/platform/browser/http.ts`; `web/src/platform/desktop/http.ts`; `desktop/main/files.ts`, `desktop/main/ipc.ts` |
| Browser image handling | `validateImage(blob)` (PNG and JPEG by content; header parse, then `createImageBitmap` with `imageOrientation: 'none'`; caps `maxImageAxis = 8192`, `maxImagePixels = 25_000_000`, `maxImageEncodedBytes = 20 MiB`). **No resize, crop, canvas or re-encode anywhere in `web/src`.** Attachments render as fetch, validate, `URL.createObjectURL`, `<img src="blob:...">`, revoked on cleanup. | `web/src/image.ts`; `web/src/ImageAttachment.tsx`; `web/src/api.ts` `uploadFile`, `getFileMeta`, `fetchFileBytes` |
| Browser avatar component | **An `Avatar` component exists and draws an initial only:** `<span class="avatar hue-{id % 6}" data-initial="X" aria-hidden>`; the letter comes from CSS `attr(data-initial)`, the colour from `principal_id % 6`. Used beside every message (`Line`), in the channel people list and the community list, the top-bar status, the Settings drawer "Signed in as", the voice roster and the camera-off tile. The agents panel row shows no avatar. | `web/src/Icons.tsx` `Avatar`; `web/src/Conversation.tsx` `Line`, `nameOf`, `authorOf`; `web/src/Members.tsx` `PeopleGroup`, `CommunityRow`; `web/src/Lobby.tsx`; `web/src/voice/VoicePanel.tsx` `Roster`; `web/src/voice/CallView.tsx` `CameraOff`; `web/src/app.css` `.avatar`, `.hue-N` |
| Browser people cache | `Person = { name, kind }` built from the channel member list (`useLiveChannel`), departed authors filled by `useAuthorNames` through the authors route in batches of 100 and dropped whenever the member list is re-read. `directory_changed` is coalesced 250 ms and re-reads members, the community list, the add-people picker and the admin console; `members` frames replace the list in place. | `web/src/useLiveChannel.ts`; `web/src/useAuthorNames.ts`; `web/src/useDirectoryChanged.ts`; `web/src/agents/frames.ts` `parseEventFrame`; `web/src/agents/useAgents.ts`; `web/src/App.tsx` |
| Browser settings | The drawer's Account section holds "Who can administer", `NotificationSettings`, `ChangeName` (toggle, form, `api.renamePrincipal`, `ok` / `unauthorized` / `network` / `invalid` with a code-to-text map), then `PasswordSettings`. | `web/src/Lobby.tsx` drawer; `web/src/App.tsx` the `account` prop; `web/src/ChangeName.tsx` |
| Generated TS | `web/src/schema/generated.ts` from `pkg/schema/ts.go` (`TSTypes` rows with fixture families); `principal.ts`, `channel.ts`, `file.ts` re-export the types above. None has an avatar field. | `pkg/schema/ts.go`; `web/src/schema/` |
| CLI and TUI | Plain CLI is a `switch args[0]` in `RunWithStdin` with `flag.FlagSet`s; `whoami` and `rename` exist, `admin principal rename` too; file upload is `UploadFile` (raw body). The TUI shows `[scope] HH:MM name [agent]: body`, bold author, no colour by author (nothing rests on colour); people lists are glyph, name, state. The CLI events handler knows `Roster`, `Run`, `Activity`, `Intervention`, `PermissionsChanged` only. | `internal/cli/command.go` `RunWithStdin`, `runWhoAmI`, `runRename`, `Usage`; `internal/cli/admin.go`; `internal/cli/client.go` `RenamePrincipal`; `internal/cli/files.go` `UploadFile`; `internal/cli/tui/history.go` `head`; `internal/cli/tui/view.go` `peopleLines`; `internal/cli/tui/community.go` `memberLine`; `internal/cli/tui/authors.go` |
| Dependencies | Web: react, react-dom, livekit-client and the build/test tools; no image library. Go: no image library. | `web/deps-allowlist.txt`, `web/package.json`, `deps-allowlist.txt` |
| Prior mentions | `docs/reviews/everyday-usability-audit.md` rates custom avatars "P3 only if custom images are wanted"; `docs/design/mobile-browser-layout.md` notes "no server display name or picture"; `browser-ui-polish.md` specifies the initial, the hues and the contrast. No "gravatar" anywhere. | as named |

### 1.2 Planned or designed, not built

These are referenced below and are **not** on `main` at `d7afd8c7`:

- **Moderation routes** of [moderation.md](moderation.md) (voice kick, bans, message removal; M1–M6): designed, not implemented (`selfJoinBarredTx` always answers false; no `message_removals`). This note borrows that document's authorization and audit rules for the operator's remove, and adds nothing to its routes.
- **Guests** (community-model §14): off; the rows below marked "(guests later)" are notes for the issue that restores them.
- **Agent avatars**: nothing designed. Agents are principals and are drawn with the same initial; §7 O7 asks whether an operator may set one later. Nothing here forecloses it.
- **Tabletop character tokens** (the issue's last line): out of scope and kept distinct by name: this is the *account's* picture, one per principal, not a per-channel or per-game token. A later token feature is its own design and must not reuse these routes.
- **File garbage collection**: none exists (browser-image-attachments §5). The storage choice of §3.1 is made so that avatars never need it.

## 2. Decisions in one page

| Operation | REST | Who | Answer |
| --- | --- | --- | --- |
| Set or replace own avatar | `PUT /v1/principals/{id}/avatar`, raw PNG or JPEG body | The principal itself: a human member or operator (an agent is refused; (guests later) a guest is refused) | 200 `PrincipalAvatarV1` |
| Remove own avatar | `DELETE /v1/principals/{id}/avatar` | The principal itself | 204, idempotent |
| Remove someone's avatar (moderation) | the same `DELETE` | A holder of `manage_members`, owner-protected | 204, idempotent, audited with actor ≠ subject |
| Fetch an avatar | `GET /v1/principals/{id}/avatar[?v=<sha256>]` | Any human who may know the principal, or may read a message it authored (§3.6) | 200 `image/png`; 404 `avatar_not_found` for everything else |
| Learn that a principal has one | `avatar_sha256` on `ServerMemberV1`, `ChannelMemberV1` (REST and the members frame), `ChannelAuthorV1`, `WhoAmIResponseV1` | Whoever already receives those documents | absent when there is none |
| Learn that it changed | `conch.channel_members.v1` (the members frame now differs) and `conch.directory_changed.v1`, exactly as for a rename | the same recipients as a rename | no new events document |

No MCP tool. Setting a picture of oneself is a human act; `read_channel` returns `author_id` as today and an agent that wants a picture has the REST route (ADR-001). No new permission: `manage_members` already means "administer people without speaking as them" (admin-roles §4.1 row 7), which a moderation remove is. No new server setting and no new flag.

## 3. The contracts

Each contract states the decision, the alternatives considered, and why.

### 3.1 Storage

**Decision.** A new table, bytes in SQLite:

```sql
CREATE TABLE IF NOT EXISTS principal_avatars (
  principal_id INTEGER PRIMARY KEY REFERENCES principals (id),
  sha256       TEXT    NOT NULL CHECK (length(sha256) = 64),
  media_type   TEXT    NOT NULL,            -- always 'image/png' in v1
  width        INTEGER NOT NULL CHECK (width  > 0),
  height       INTEGER NOT NULL CHECK (height > 0),
  size         INTEGER NOT NULL CHECK (size  > 0),
  bytes        BLOB    NOT NULL,
  created_at   INTEGER NOT NULL             -- unix ms, when this rendition was stored
);
```

One row per principal, replaced by `INSERT OR REPLACE` in the same `IMMEDIATE` transaction as the audit row; removed by `DELETE`. `sha256` is the hash of `bytes` as stored (the re-encoded PNG), not of the upload. `HidePrincipal` deletes the row in its transaction. Disabling keeps the row (re-enabling restores the picture); the read route refuses while disabled (§3.6).

**Alternatives.**

| Option | Why not |
| --- | --- |
| Reuse table `files` and the file routes | `files.channel_id` is `NOT NULL` and `InsertFile` checks channel membership and archive state: an avatar belongs to no channel. A file row is a single-use attachment (`attachFilesTx`) with a download contract that says "never rendered" (`application/octet-stream`, `Content-Disposition: attachment`), the opposite of an avatar. Visibility is `FileVisibleTo` (the attaching message), not the person. Bending any of this changes published behaviour of `conch.file.v1`. |
| Reuse the on-disk blob directory (`storeBlob`, `blobPath`) with a new table pointing at it | Blobs are content-deduplicated and nothing deletes them. Removing or replacing an avatar could not delete its blob without reference counting across `files` and the new table, which does not exist; abandoned avatars would accumulate as the abandoned uploads of browser-image-attachments §5 do. The re-encoded rendition is at most a few hundred kilobytes, well inside what SQLite stores efficiently; one transaction replaces picture and audit row atomically, and the ordinary database backup (Litestream or a copy) carries it with no second directory to keep in step. |
| A column `principals.avatar_sha256` in addition to the table | Saves a join on the four reads of §3.4. Rejected for v1: one source of truth and one transaction is simpler; a `LEFT JOIN principal_avatars` or a correlated subquery on four queries is cheap and keeps `principals` untouched. Can be added later without a wire change if the join shows up in profiles. |
| Bytes inline on the wire (`data:` URI in the directory document) | Fifty members at 100 KB each in every directory page and every members frame. No. |

### 3.2 Accepted input and validation (server side)

**Decision.** `PUT /v1/principals/{id}/avatar` takes the raw body (no multipart, no JSON, as file upload does). The server:

1. Bounds the body: `Content-Length` and `http.MaxBytesReader` against `schema.MaxAvatarUploadBytes = 2 << 20` (2 MiB); over it, 413 `file_too_large` with `Connection: close`, the existing code and behaviour of `handleUploadFile`. This cap is a constant, **independent of `--max-upload-bytes`**: avatars are bounded by construction and re-encoded, so an operator who disabled attachments has no reason to lose pictures (O5).
2. Reads the whole body into memory (it is at most 2 MiB).
3. **Identifies the format by content, not by header.** `image.DecodeConfig` over a reader of the bytes; the returned format name must be `"png"` or `"jpeg"`. The `Content-Type` header is ignored. A mismatch between header and bytes is not an error; bytes rule. Any other format name, or an error, is 400 `avatar_invalid` ("not a PNG or JPEG image"). Note: Go registers decoders by import; the server must import exactly `image/png` and `image/jpeg` and must still check the name, because an `image/gif` import anywhere in the binary would otherwise register GIF silently. A structural test pins the imports.
4. Bounds the declared dimensions **before** decoding pixels: both axes in `[1, schema.MaxAvatarSourceAxis = 4096]` and `width*height <= schema.MaxAvatarSourcePixels = 12_000_000`; otherwise 400 `avatar_invalid` ("image too large: at most 4096 by 4096 and 12 million pixels"). A 4000 by 3000 phone photo passes; a decompression bomb does not reach the decoder.
5. Decodes with `image.Decode` under a **server-wide semaphore of 2** concurrent decodes (a buffered channel on `Server`), so two uploads cannot together hold more than about 2 × 96 MiB (16-bit PNG at 4096 by 2929 decodes to `NRGBA64`, 8 bytes a pixel); a third waits on the semaphore with the request context, and gives 503 `avatar_busy` (new code) with `Retry-After: 2` if the context ends first. A decode error after `DecodeConfig` succeeded (a truncated or corrupt file) is 400 `avatar_invalid`.
6. Reads the JPEG EXIF orientation tag (O4), applies it, centre-crops to a square, downscales to at most `schema.AvatarSize = 256` on each side (never upscales), encodes PNG, hashes, stores (§3.3).
7. Counts the attempt against the limits of §3.8 before any of the above, exactly as `renameLimitTx` does, so a refused upload still costs an attempt.

The route is humans only (`humansOnly`-style wrapper, 403 `agents_no_avatar`, the pattern of `humansOnlyFiles` / `denyAgentFiles`) and members only (`membersOnly`; (guests later) a guest is refused).

**Alternatives.**

| Option | Why not |
| --- | --- |
| Trust `Content-Type` and store the upload as sent | The issue's security section forbids it, and the attachments design's rule stands: a name or header is not evidence. A stored SVG, HTML or polyglot would be served as an image by this route. |
| Accept WebP, GIF, AVIF | No stdlib decoder. Each is a new `go.mod` dependency (`golang.org/x/image/webp` for WebP decoding only; nothing in x/image encodes it) and so Nick's sign-off and a `deps-allowlist.txt` line. GIF is animated and would need frame selection. The browser crops and re-encodes to PNG before upload (§3.9), so a WebP or HEIC phone picture still arrives as PNG from the browser; only the CLI would refuse it. O3. |
| `http.DetectContentType` as the sniff | It recognizes more formats than the server decodes and says nothing about dimensions; `DecodeConfig` is the right tool and already registered. |
| Reject on EXIF presence rather than read it | Every phone JPEG has EXIF; the user would see "invalid". Reading one tag is bounded (§3.3) and the re-encode drops everything else. |
| Validate in the client only | The client's `validateImage` stays as defence in depth and for a good error before the round trip, but the CLI, curl and a hostile client reach the route directly. |

### 3.3 Re-encoding, sizes served, metadata

**Decision.**

- **One rendition**, square, `min(256, shorter source side)` pixels a side, PNG (`image/png`), colour model `NRGBA` (alpha kept: a transparent logo stays transparent; the circle mask is the client's). The server never upscales: a 100 by 100 source stays 100 by 100.
- **Centre crop** to a square when the source is not square (the browser lets the person choose the crop before upload, §3.9; the server crop is for the CLI and for a non-square body from any client).
- **Downscale** with a hand-written box filter (area averaging) in `internal/server/avatar/` (or wherever the server-engineer places it), stdlib only: for each destination pixel, average the source pixels it covers, in linear 8-bit per channel, alpha-premultiplied during the average and un-premultiplied after. About sixty lines, table-tested against known inputs. Nearest-neighbour is refused for quality; bilinear is unnecessary for a pure downscale with integer-ish ratios where a box filter is both simpler and better.
- **Orientation (O4).** A bounded reader of the JPEG EXIF orientation tag (APP1 segment, TIFF header, IFD0 tag `0x0112`, values 1–8), reading at most the first 64 KiB of the file and the first APP1 segment, in `internal/server/avatar/exif.go`, stdlib only, with a table test over all eight values built bytewise. The transform (rotate 90/180/270, mirror) is applied on the decoded image before cropping. PNG carries no orientation. If Nick prefers not to, the server stores the picture as decoded and the browser (which already bakes orientation into its crop) is the only client that uploads phone photos correctly.
- **Metadata stripping is a property of the re-encode, not a separate pass.** `image/png.Encode` writes `IHDR`, optionally `PLTE`/`tRNS`, `IDAT`, `IEND` and nothing else: no `tEXt`, `iTXt`, `eXIf`, `iCCP`, `pHYs`, `tIME`. The output carries no EXIF, no GPS, no colour profile, no comment, no timestamp. A test asserts the chunk list of a stored avatar is exactly `IHDR [PLTE] [tRNS] IDAT* IEND`, with a source that carries EXIF GPS, a `tEXt` comment and an ICC profile.
- **Why PNG (O2)**: lossless, so a replace-with-itself is a byte-identical no-op (the sha does not change and no signal is sent, §3.7); alpha preserved; one encoder; no quality knob to document. The cost is size: a 256-pixel photo is roughly 100–180 KB as PNG against 20–30 KB as JPEG q85. With the client cache of §3.7 that is a first-load cost of a few megabytes for a fifty-member server, paid once per browser per picture. `media_type` is stored and declared on the wire (`PrincipalAvatarV1.media_type`) so that a later switch to JPEG for opaque sources is additive: the client already validates both.
- **Sizes served (O2).** One. The clients draw it at 36 px (messages, lists), 28 px (top bar), and larger in the camera-off tile and the Settings preview; browsers downscale 256 px to those well. A second small rendition would double the storage and routes for a few kilobytes of bandwidth per avatar that the cache already amortizes. If a 64-pixel rendition is wanted later, `GET .../avatar?size=64` is additive.

### 3.4 Wire shapes

All in `pkg/schema`, additive, no schema-name field on the REST bodies (versioned by the `V1` suffix, as the directory and whoami shapes are).

```go
// Avatars (issue #974, docs/design/avatars.md). A principal may carry one
// square PNG; the four documents that name a principal to a viewer carry
// its content hash, and GET /v1/principals/{id}/avatar serves the bytes
// behind the same visibility rule as the name.

const (
	// MaxAvatarUploadBytes bounds the body of PUT /v1/principals/{id}/avatar.
	MaxAvatarUploadBytes = 2 << 20
	// MaxAvatarSourceAxis and MaxAvatarSourcePixels bound the declared
	// dimensions of an upload, checked before any pixel is decoded.
	MaxAvatarSourceAxis   = 4096
	MaxAvatarSourcePixels = 12_000_000
	// AvatarSize is the longest side of a stored avatar: the server crops to
	// a square and downscales to at most this, never up.
	AvatarSize = 256
	// AvatarMediaType is the only media type a v1 server stores and serves.
	AvatarMediaType = "image/png"

	ErrorCodeAvatarInvalid  = "avatar_invalid"   // 400: not a PNG or JPEG, or too large to decode
	ErrorCodeAvatarNotFound = "avatar_not_found" // 404: no avatar, or none the caller may see
	ErrorCodeAvatarBusy     = "avatar_busy"      // 503: decoder slots taken; Retry-After set
)

// PrincipalAvatarV1 describes a stored avatar: the body of a successful PUT.
// SHA256 is the hash of the stored bytes (the re-encoded PNG), the value the
// naming documents carry as avatar_sha256 and the cache key a client uses.
type PrincipalAvatarV1 struct {
	PrincipalID int64     `json:"principal_id"`
	SHA256      string    `json:"sha256"`
	MediaType   string    `json:"media_type"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Size        int64     `json:"size"`
	CreatedAt   Timestamp `json:"created_at"`
}
// Validate: positive id; ValidSHA256Hex; media_type passes ValidateMediaType;
// 0 < width, height <= AvatarSize; size > 0; created_at set.
```

And one optional field, identically named and documented, on four published types. Each is `omitempty`, so **every existing fixture is byte-identical** and a server or client that predates the field sees nothing:

```go
// AvatarSHA256 is the content hash of the principal's avatar (issue #974),
// the cache key for GET /v1/principals/{id}/avatar?v=<sha256>. Absent when
// the principal has none, and from a server that predates avatars; a
// client then shows its initial. Never derived from the name.
AvatarSHA256 string `json:"avatar_sha256,omitempty"`
```

| Type | Where it is filled | Not filled |
| --- | --- | --- |
| `ServerMemberV1` (`pkg/schema/directory.go`) | every row, every viewer | an agent row (v1: agents have none) |
| `ChannelMemberV1` (`pkg/schema/channel_member.go`), REST and `conch.channel_members.v1` | every human row | an agent row; a guest row ((guests later) a guest has none) |
| `ChannelAuthorV1` (`pkg/schema/channel_author.go`) | every human author listed | an agent author; a hidden author (its row was deleted on hide) |
| `WhoAmIResponseV1` (`pkg/schema/whoami.go`) | the caller's own | a guest's, an agent's |

`Validate` on each: when present, `ValidSHA256Hex`; an agent row that carries it is invalid (as an agent row carrying `role` is). `PrincipalV0`, `MessageV2`, `SessionRosterV1`, `VoicePresenceV1`, `VoiceOccupancyV1` and `GuestAttributionV1` are **unchanged**: they carry ids or names, and the clients already resolve both from the member list, which now carries the hash.

**Alternatives.**

| Option | Why not |
| --- | --- |
| An `avatar_url` string | A URL on the wire invites a client to put it in `<img src>`, which cannot authenticate (bearer only) and would tempt a token-in-URL or an unauthenticated route. The hash says "there is one, and this is its cache key"; the route is fixed and documented. |
| A nested `avatar: {sha256, media_type, width, height}` object on each document | Four more fields on four documents for no client decision: the client validates the bytes anyway, and the `Content-Type` of the response says the type. `PrincipalAvatarV1` carries them for the PUT answer and for a later `GET .../avatar/meta` if one is wanted. Flat is the house style. |
| A version integer instead of a hash | A hash is a cache key that is also a correctness check (the client can verify what it fetched); an integer is not. The file routes already use sha256 hex. |
| A new events document `conch.avatar_changed.v1` | The two existing signals already reach exactly the right viewers (§3.7). A new frame with a principal id would be a leak vector the content-free signals were designed to avoid. |

### 3.5 Routes and authorization

| Route | Class | Handler rule |
| --- | --- | --- |
| `PUT /v1/principals/{id}/avatar` | `accessOperatorOrSelf` (the guard rename uses), then `humansOnly`-style and `membersOnly` wrappers | The handler refuses any `{id}` but the caller's own with 403 `forbidden`, "only the principal itself may set its avatar" (audited `access_denied`, as `denyForbidden` does). An operator cannot set another person's picture (O1). Not in `recentAuthRoutes`: rename is not either; the route grants nothing. Limits of §3.8. |
| `DELETE /v1/principals/{id}/avatar` | `accessOperatorOrSelf` | Self: always, 204. Anyone else: `canOrFail(manage_members)` then `ownerProtected(id)` (a non-owner cannot touch an owner's), the pattern of `handleRenamePrincipal`; the store re-checks `manage_members` inside the transaction (`ErrManageMembersRequired`), as rename does, so a right revoked after the guard cannot race the write. 404 `principal_not_found` for an id that does not exist, is an agent, or (guests later) is a guest; the same 404 whether or not the caller may know the principal, since only self and `manage_members` reach it. `reauthNone` (reduce-only, moderation §3.1). Idempotent: no row, 204, no audit row. |
| `GET /v1/principals/{id}/avatar` | `accessAuthenticated` + `humansOnly`-style wrapper (an agent gets 403, as on files) | The visibility rule of §3.6. Answers `200`, `Content-Type: image/png`, `Content-Length`, `X-Content-Type-Options: nosniff`, `ETag: "<sha256>"`, `Cache-Control` per §3.7, and the bytes. `If-None-Match` matching the current hash answers 304. Every refusal and every absence is one byte-identical 404 `avatar_not_found`. The optional `?v=<sha256>` is a cache key only: the server serves the current bytes whatever `v` says (a stale `v` means a stale client, which the signals fix). `HEAD` as `GET` without a body. |

No `GET .../avatar/meta` in v1: the naming documents already say whether there is one, and the PUT answer carries the rest. Additive if wanted.

**Alternatives.**

| Option | Why not |
| --- | --- |
| `PATCH /v1/me` or `/v1/profile` with a JSON body carrying base64 | No `/v1/me` exists; the self routes are `/v1/principals/{id}/...` with `accessOperatorOrSelf`, and raw-body upload is the file precedent. Base64 in JSON would hit `MaxPostRequestBytes` (1 MiB). |
| Let an operator *set* another person's avatar | The issue says own account only; an operator setting someone's face is impersonation-adjacent and `act_as_members` territory. Remove is enough for moderation (O1). |
| A new permission `manage_avatars` | `manage_members` already covers rename and hide of other people; a moderation remove is smaller than either. ADR-014's rule for a new permission (its default must reproduce prior behaviour) would make it a copy of `manage_members` anyway. |
| Expose the avatar through `GET /v2/files/{id}` | That route's contract is `application/octet-stream` and `attachment` for a channel's file. Changing it per file is the "sometimes rendered" case the attachments design forbids. |
| A public, unauthenticated avatar route (so `<img src>` works) | Leaks every picture to anyone holding an id, and defeats `see_member_directory` off. No. |
| A capability URL (`/v1/avatars/<sha256>` with no auth) | 256 bits is unguessable, but the sha of a well-known image is known, and any viewer who learns a hash could hand it on; and the bytes would be cacheable by any proxy. Keep the bearer and the predicate. |

### 3.6 Visibility

**Decision.** A viewer V may fetch P's avatar exactly when V could learn P's name from the server:

1. V may know P under the directory predicate (`knowsCond`: `see_member_directory` held, or V is P, or a shared channel, or P joined through V's link), **or**
2. P authored a message V may read, in any channel (the rule of the authors route, community-model §13.5; `AuthorReaders` is its inverse and the SQL is the same `messageVisible` fragment turned around).

Both are written once in the store as `avatarVisibleTx(viewer, target)` beside `mayKnowTx`, and the structural guard test that keeps member-facing principal reads inside the predicate (`TestMessageReadsStayInTheVisibilityFunction`'s pattern, community-model §13.2) gains the avatar query. The base set applies first, as everywhere: an enabled, non-hidden human principal with a row. So:

| Target P | Answer to V |
| --- | --- |
| Known to V (predicate), has an avatar | 200 |
| Departed author of a message V may read, no shared channel, `see_member_directory` off for members | 200, as the authors route names them |
| Not known and never an author V may read | 404 `avatar_not_found`, byte-identical to "no avatar" |
| Disabled | 404 (outside the base set, as the directory omits it); the row is kept for re-enable |
| Hidden | 404; the row was deleted by `HidePrincipal` |
| An agent | 404 in v1 (O7) |
| (guests later) a guest | 404: a guest never has one. A guest *viewer* fetches co-members' avatars under clause 1, bounded to its visited channels, as its member lists are. An ended guest is outside the base set as a viewer and as a target. |
| V with `see_member_directory` (default) or an operator | everyone with a row |

**The hash is only ever on documents the viewer may already have.** `avatar_sha256` rides the four naming documents, which pass the predicate or the channel gate today; it is never on a document that reaches a viewer who may not know the principal. The `conch.directory_changed.v1` signal stays content-free.

**Alternatives.**

| Option | Why not |
| --- | --- |
| Predicate only, no authored-message clause | With `see_member_directory` off for members, a departed author's name is shown (authors route) but their picture would 404: an inconsistent rendering, and the client would need to know why. The authored clause discloses exactly what the message disclosed. |
| `?channel=<id>` context on the GET, applying the authors rule per channel | One route rule without a parameter is simpler for every client, and "any message V may read" is what `AuthorReaders` already computes for the hide signal. |
| Everyone with a credential may fetch any avatar | Breaks `see_member_directory` off and ADR-013 decision 9 ("enforced in every server answer"). |

### 3.7 Caching and propagation

**Propagation.** After a successful set (when the hash changed) or remove (when a row was deleted), the handler calls exactly what rename calls after `res.Changed`:

- `s.events.notifyRosters()`: every events socket rebuilds its member list; `memberDoc` now fills `avatar_sha256`, so the `conch.channel_members.v1` bytes differ and the frame is sent to the subscribers of every channel P is in (coalesced under the 250 ms floor, sent only when bytes changed, as today).
- `s.directoryPrincipalChanged(ctx, id, nil)`: `conch.directory_changed.v1` to every viewer who may know P. The browser re-reads the directory, the member list, the add-people picker and the admin console on it, as it does for a rename.

A replace that produces the same hash (the same picture again) changes nothing and sends nothing; a remove of nothing sends nothing. A hide already sends both signals and now also drops the row. A disable sends what it sends today; the next read answers 404 and the viewer's cache entry is keyed by a hash it will no longer be given.

**Client cache.** The browser keeps one module-level map `"<id>:<sha256>"` → object URL (with the `Blob`), filled by `fetchAvatar(id, sha256)` (bearer header; `cache: 'default'`, not `no-store`, see below), validated by `validateImage` before `URL.createObjectURL`, evicted LRU above a small bound (say 200 entries) and **cleared, with every URL revoked, on logout, access loss and server or account change**, the attachment rule. A hash that no document carries any more is unreachable and ages out. The TUI has no cache (no images).

**HTTP caching (O6).** The response carries `ETag: "<sha256>"` and `Cache-Control: private, max-age=604800, immutable` when the request names the current hash in `?v=`, and `Cache-Control: private, no-store` when it does not. The bytes behind a given hash never change, so an immutable week on a URL that embeds the hash is correct, and a reload does not refetch fifty pictures. `private` keeps shared caches out; a browser cache stores the response under the request's `Authorization`-bearing URL only for that origin. The alternative, `no-store` everywhere as the file route does, costs a full refetch per page load and is listed in §7 for the security reviewer's judgement; the in-memory map works either way.

**What a cached picture reveals after access changes.** The same as a cached name (community-model §13.8, "what a client was sent before a toggle or a removal"): the server stops serving; it cannot make a browser forget. The attachments design chose `no-store` for *content*; an avatar is identity the viewer was shown, and the hash is already in the viewer's directory copy. If Nick prefers `no-store` throughout, the change is one header and no shape.

### 3.8 Audit and limits

| Action | Actor | Subject | Detail | When |
| --- | --- | --- | --- | --- |
| `avatar_set` | `principal:<id>` | `principal:<id>` | `sha256=<hex> size=<bytes> width=<w> height=<h>` | a stored rendition replaced or created with a different hash; a same-hash replace writes no row |
| `avatar_removed` | the caller (`principal:<caller>`) | `principal:<id>` | empty for self; `moderation=true` when actor ≠ subject | a row deleted; nothing to delete writes no row |
| `avatar_set_refused` | `principal:<id>` | `principal:<id>` | `reason=invalid|too_large|busy` | a refused upload (counted against the attempt limit; the pattern of `principal_rename_refused`) |
| `access_denied` (existing) | the caller | the route | as today | a non-self PUT, a DELETE without `manage_members`, an owner-protected target |

No filename, no bytes, no media type as sent, no client address: the detail is what the server computed (moderation §3.3).

**Limits** (`avatarSetsPerDay = 10`, `avatarAttemptsPerDay = 30`, a rolling day, counted from the audit rows in the store as `renameLimitTx` does so they survive a restart): over either, 429 `rate_limited` with `Retry-After` and `retry_after_seconds`, the rename message pattern. Operators' moderation removes are not limited. Decoding concurrency is the semaphore of §3.2.

### 3.9 Clients

**Browser (`web/`).**

- `Person` gains `avatar: string | null` (the hash); `useLiveChannel` fills it from `ChannelMemberV1.avatar_sha256`, `useAuthorNames` from `ChannelAuthorV1.avatar_sha256`; the community list from `ServerMemberV1`; the top bar and drawer from `WhoAmIResponseV1`.
- `Avatar({ id, name, avatar? })`: with a hash, a hook `useAvatar(id, avatar)` returns the object URL from the cache (fetching on first use) and the component renders `<img class="avatar" src="blob:..." alt="">` inside the same circle, falling back to today's initial while loading and on any failure (`unauthorized` ends the credential as elsewhere; 404 and `invalid` fall back silently and remember the failure for that hash so a bad hash is fetched once). `aria-hidden` stays: the name beside it is the accessible text, the issue's "accessible labels" requirement being that and `alt=""`.
- Settings: `AvatarSettings` after `ChangeName` in the Account section. Pick a file (`accept="image/png,image/jpeg"` as a hint only), `validateImage`, then **crop**: draw to a `<canvas>` through `createImageBitmap(blob, { imageOrientation: 'from-image' })` (orientation baked), a square selection the person can move and zoom, `canvas.toBlob('image/png')` at 512 px (so the server's downscale starts from a clean square), then `api.putAvatar(id, blob, signal)`. Preview before Send; Remove with a confirmation line; the same `ok` / `unauthorized` / `network` / `refused`-with-code result shape as `uploadFile`, and a code-to-text map covering `avatar_invalid`, `file_too_large`, `avatar_busy`, `rate_limited`. On success, dispatch the new hash into the auth state so the top bar changes without a reload. No new dependency: `canvas` and `createImageBitmap` are the platform.
- CSP: unchanged. `blob:` is already in `img-src` on web and desktop; the comment in `static.go` that says `blob:` is for attachment previews gains "and avatars" in the server slice.
- Desktop: the renderer never holds the token, so `desktop/main/files.ts` and `ipc.ts` gain `avatarBytes(id, sha)` and `putAvatar(id, bytes)` operations beside `fileBytes` / `uploadFile`, with the same size bound and timeout. Until that lands, desktop shows initials (the fallback) and the feature is still correct.
- Members, voice roster, camera-off tile and the community list change nothing but the prop they pass; `.message.cont` keeps hiding the avatar on follow-on messages.

**CLI and TUI (`internal/cli`).**

- `conch avatar set <file>` (reads the file, sends it raw; refuses a file over `MaxAvatarUploadBytes` before sending; prints the `PrincipalAvatarV1` or `--json`), `conch avatar remove`, `conch avatar show --out <file>` (fetches own bytes; useful to verify what others see), all as `case "avatar":` beside `whoami` and `rename` in `RunWithStdin`, with Usage lines. `conch admin principal avatar-remove <id>` for the moderation remove, with the `--yes` rule of moderation §3.2 in non-interactive use.
- The TUI renders no images (ADR-007: nothing rests on colour, and a terminal has no raster). It changes nothing in v1; parity holds through the REST routes (ADR-001). If a glyph for "has a picture" is ever wanted it is a view change, not a wire one.
- `internal/cli/client.go` gains `PutAvatar`, `DeleteAvatar`, `GetAvatar`.

## 4. Security: threats and abuse

| Threat | Where it bites | Control |
| --- | --- | --- |
| **Hostile image bytes** (crafted PNG/JPEG, decompression bomb, polyglot that is also HTML/SVG/script) | the server decoder; every browser that renders the stored bytes | Format by content, not header (§3.2 step 3). `DecodeConfig` dimension caps before pixels (step 4). Concurrency semaphore and a 2 MiB body (step 5). Go's decoders are memory-safe; a panic in them is caught by the existing handler recovery and answers 500, never a partial store. **The stored bytes are never the uploaded bytes**: the re-encode yields a fresh PNG from decoded pixels, so a polyglot does not survive. The GET answers `nosniff` and `image/png`. The browser still runs `validateImage` before an object URL, as for attachments. |
| **Metadata leakage** (GPS, device, timestamps, name in a comment, colour profile) | privacy of the uploader | The re-encode writes no ancillary chunks (§3.3); tested by chunk list. The audit detail carries the computed hash and size, never the filename or the declared type. |
| **Existence oracle** under `see_member_directory` off | `GET .../avatar/{id}`, `DELETE` by an operator | One byte-identical 404 `avatar_not_found` for unknown, disabled, hidden, agent, no-avatar and not-visible (§3.6), the §13.4 rule. `DELETE` is reachable only by self and `manage_members`, who see everyone. The hash travels only on documents already gated. `?v=` does not change the answer. |
| **Impersonation** (a member uploads another person's photo or a logo to pass as them; an operator putting a picture on someone) | social | Pictures are only ever self-set (O1): a picture is the person's own claim, like their name, and names already carry that risk (community-model R5). Moderation remove with `manage_members`, audited with actor ≠ subject. `ownerProtected` keeps a non-owner operator off an owner. |
| **Abusive content** (an offensive picture) | everyone who sees it | The same remove, confirmed in the client ("Remove `<name>`'s picture for everyone"), moderation §3.2; `avatar_removed` with `moderation=true` in the trail; the subject can set another, and is then limited by §3.8 (10 a day), so a war of re-uploads is bounded and audited; hide or disable (existing) ends it. |
| **Resource exhaustion** (many uploads, large decodes, many fetches) | CPU, memory, storage | Body 2 MiB; decode semaphore 2; per-person 10 sets / 30 attempts a day from audit rows (restart-safe); one row per principal, so storage is bounded by members × ~200 KB and never grows with repeats; fetches are cheap blob reads with ETag/304 and an immutable week on hash URLs. Unauthenticated callers never reach the decoder (the guard runs first). |
| **Token in a URL** | browser history, logs, proxies | None: bearer header, as everywhere; `?v=` is a public hash. The route is not usable as a bare `<img src>` and the design says so, so nobody adds a token to make it work. |
| **Cache after revocation or a toggle** | shared machine | The in-memory map is cleared on logout and access loss; the HTTP cache keeps at most a week of pictures the viewer was shown, like any name they were sent (O6 offers `no-store`). |
| **Agents** | MCP, agent credentials | Agents cannot set, remove or fetch avatars (403, as for files); no MCP tool; an agent manifest cannot grant it. The avatar never reaches `read_channel`. |
| **Guests (later)** | guest links | A guest cannot set one (`membersOnly`), never has one, and fetches under the predicate bounded to its visited channels; a hidden or ended guest answers 404. |
| **Hide and disable** | moderation | `HidePrincipal` deletes the row in its transaction and already sends both signals; the placeholder name and no picture are consistent. Disable keeps the row and refuses reads. |
| **SVG, HTML, GIF, WebP, AVIF, HEIC bodies** | decoder surface | Refused by content (`DecodeConfig` names only `png` and `jpeg`), with a structural test that the binary imports exactly those two decoders. |

Every implementing PR that touches `internal/server` gets a `security-reviewer` pass (CLAUDE.md: authz and an upload surface). None touches the approval path.

## 5. Schema impact

**No version bump is needed; every change is additive.** Nick confirms that reading (O8), since the call is his under CLAUDE.md rule 2.

| Item | Kind | Existing fixtures |
| --- | --- | --- |
| `PrincipalAvatarV1` and its `Validate` | new type | none change; new `principal-avatar-v1.json` |
| `MaxAvatarUploadBytes`, `MaxAvatarSourceAxis`, `MaxAvatarSourcePixels`, `AvatarSize`, `AvatarMediaType` | new constants, exported to TS through `ts.go` `TSConsts` | none |
| `ErrorCodeAvatarInvalid`, `ErrorCodeAvatarNotFound`, `ErrorCodeAvatarBusy` | new error codes (open string, as every code is) | none |
| `ServerMemberV1.AvatarSHA256` | new optional field | `server-member-v1*.json` unchanged; new `server-member-v1-avatar.json` |
| `ChannelMemberV1.AvatarSHA256` | new optional field; the members frame inherits it | `channel-member-v1*.json`, `channel-members-v1*.json` unchanged; new `channel-member-v1-avatar.json` |
| `ChannelAuthorV1.AvatarSHA256` | new optional field | `channel-author-v1.json` unchanged; new `channel-author-v1-avatar.json` |
| `WhoAmIResponseV1.AvatarSHA256` | new optional field | the fourteen `whoami-response-v1*.json` unchanged; new `whoami-response-v1-avatar.json` |
| `Validate` additions | an agent row carrying `avatar_sha256` is invalid; a present hash must be `ValidSHA256Hex` | none |
| `pkg/schema/ts.go` | one `TSTypes` row (`principal-avatar-v1`), the constants, regenerated `web/src/schema/generated.ts` and `.families.ts`; `web/src/schema/principal.ts` re-exports `PrincipalAvatarV1` | `scripts/schema-ts-check.sh` must pass |
| `scripts/schema-compat.sh` | must pass byte-for-byte on every published fixture | |
| MCP tool definitions | **unchanged**: no tool reads or writes an avatar | |
| `docs/design/approval-object.md` | unchanged: no approval shape is touched | |

Not changed, deliberately: `PrincipalV0`, `MessageV2`, `SessionRosterV1`, `VoicePresenceV1`, `VoiceOccupancyV1`, `GuestAttributionV1`, `FileV1`, `conch.directory_changed.v1`, `conch.channels_changed.v1`.

## 6. Migration

One additive migration (the next number after the last in `store.go`): `CREATE TABLE IF NOT EXISTS principal_avatars (...)` as in §3.1. No backfill: nobody has an avatar. No audit row. Rollback is the previous binary plus the pre-upgrade backup, as for every migration; the previous binary ignores the table. Backups: the table is inside the database, so Litestream and a file copy carry it without change to `docs/deploy.md`'s backup list (which also names `files/`, untouched here).

No change to `files`, `principals`, `server_settings` or the permissions document.

## 7. Open decisions for Nick

Numbered; each with the recommendation the slices assume. "Accept recommendations" takes all of them.

1. **May an operator set someone else's avatar?** *Recommend no.* Own account only; operators get remove (`manage_members`, owner-protected). Setting another person's face is closer to `act_as_members` than to administration. Additive later if wanted.
2. **Rendition: one 256-pixel square, PNG.** *Recommend yes.* Lossless, alpha kept, one stdlib encoder, idempotent replace. The alternative is JPEG q85 for opaque sources (five times smaller, a quality knob, alpha flattened). `media_type` is on the wire so switching later is additive; a second size is additive too.
3. **Accepted inputs: PNG and JPEG only, stdlib decoders, no new dependency.** *Recommend yes.* WebP needs `golang.org/x/image/webp` (decode only) and your sign-off plus `deps-allowlist.txt`; the browser re-encodes to PNG before upload so WebP/HEIC phone pictures still work from the browser. If you want WebP from the CLI, say so and the server slice adds the dependency under the depgate.
4. **EXIF orientation: a bounded in-tree reader of one tag (stdlib only), applied before the crop.** *Recommend yes.* Without it a CLI upload of a phone JPEG can arrive rotated; the browser is unaffected either way. About 80 lines plus a bytewise table test; no dependency.
5. **Avatar size cap independent of `--max-upload-bytes`.** *Recommend yes*: a fixed 2 MiB constant, no flag, and avatars stay available when an operator disables attachments. The alternative ties `--max-upload-bytes 0` to "no avatars either" and adds a flag to document.
6. **HTTP caching: `private, max-age=604800, immutable` on hash-keyed URLs (`?v=`), `no-store` otherwise.** *Recommend yes*, with the security reviewer free to push for `no-store` throughout (one header, no shape change). The in-memory client cache makes either correct within a session; the difference is a refetch per page load.
7. **Agent avatars.** *Recommend not in v1.* Agents keep the initial; an agent row never carries `avatar_sha256` (validated). If wanted later, "an operator with `manage_members` sets an agent's picture" is additive on the same routes.
8. **Schema reading: additive, no version bump.** *Recommend you confirm* §5 as read: four optional fields, one new type, constants and codes; every published fixture byte-identical.
9. **Moderation remove needs no confirmation dialog beyond the client's.** *Recommend yes*: the server cannot prove a dialog (moderation §3.2); the browser shows "Remove `<name>`'s picture for everyone" and the CLI needs `--yes` non-interactively.

## 8. Implementation slices

Five slices, each filed later as its own issue in the template's sections. Dependency order: A1 first (schema); A2 (server) depends on A1; A3 (web), A4 (desktop bridge) and A5 (CLI) depend on A2 and can be dispatched in parallel, in disjoint file areas. A2 is the security-sensitive one.

### A1. Schema: avatar shapes and fixtures (`area/schema`)

**Context.** [docs/design/avatars.md](avatars.md) §3.4, §5. Additive: one new type, four optional fields, constants and error codes. No version bump (O8 confirmed). The `schema-change` skill governs.

**Files.** `pkg/schema/avatar.go` (new: constants, codes, `PrincipalAvatarV1`, `Validate`), `pkg/schema/avatar_test.go`; `pkg/schema/directory.go`, `channel_member.go`, `channel_author.go`, `whoami.go` (the field and its `Validate` rule); `pkg/schema/testdata/principal-avatar-v1.json`, `server-member-v1-avatar.json`, `channel-member-v1-avatar.json`, `channel-author-v1-avatar.json`, `whoami-response-v1-avatar.json`; `pkg/schema/ts.go` (one `TSTypes` row, the constants); `web/src/schema/generated.ts`, `generated.families.ts` (regenerated by `go generate ./pkg/schema`), `web/src/schema/principal.ts` (re-export).

**Acceptance criteria.**
- [ ] `PrincipalAvatarV1` round-trips its fixture; `Validate` refuses a bad hash, a non-`type/subtype` media type, a side over `AvatarSize` or under 1, a zero size, a zero `created_at`.
- [ ] Each of the four types carries `avatar_sha256,omitempty`; a present value must pass `ValidSHA256Hex`; an agent row carrying it fails `Validate` on `ServerMemberV1`, `ChannelMemberV1` (and so `ChannelMembersV1`) and `ChannelAuthorV1`.
- [ ] Every existing fixture is byte-identical (`./scripts/schema-compat.sh` passes); `go test ./pkg/schema/...` passes; `scripts/schema-ts-check.sh` passes after `go generate`.
- [ ] The constants are exported to TS (`MAX_AVATAR_UPLOAD_BYTES`, `AVATAR_SIZE`, ...).

**Required tests.** Golden fixture tests for the five new fixtures; table tests for every `Validate` rule above; the compat script.

**Blocked by.** Nick's answers to §7 O2, O7, O8 (the fixture values depend on O2's media type only in `principal-avatar-v1.json`).

### A2. Server: avatar routes, store, re-encoding, visibility, signals (`area/server`, security review)

**Context.** [docs/design/avatars.md](avatars.md) §3.1–§3.3, §3.5–§3.8, §4, §6. The server stores one re-encoded PNG per principal, serves it behind the name's visibility rule, and propagates changes through the rename signals.

**Files.** `internal/server/store/store.go` (migration), `internal/server/store/avatars.go` (new: `SetAvatar`, `RemoveAvatar`, `AvatarFor`, `avatarVisibleTx`, limits from audit rows, audit consts), `internal/server/store/directory.go` (the `avatar_sha256` join in `ListDirectory`), `internal/server/store/queries.go` / `members.go` (member rows), `internal/server/store/audience.go` (`ListChannelAuthors`), `internal/server/store/hide.go` (delete the row in `HidePrincipal`); `internal/server/avatar/` (new, stdlib only: `decode.go` with the format check and dimension caps, `resize.go` box filter, `exif.go` orientation (O4), `encode.go`), `internal/server/avatars.go` (new handlers), `internal/server/routes.go` (three routes; `DELETE` in `recentAuthRoutes` as `reauthNone`), `internal/server/members.go` `memberDoc` and `handleListChannelAuthors`, `internal/server/directory.go`, `internal/server/auth.go` `handleWhoAmI`, `internal/server/server.go` (the decode semaphore), `internal/server/static.go` (the `blob:` comment); `docs/deploy.md` if any operator-facing note is needed (none expected).

**Acceptance criteria.**
- [ ] `PUT /v1/principals/{id}/avatar`: self only (403 `forbidden` for any other id, audited `access_denied`); agents 403; (guests later) guests 403; body over 2 MiB is 413 `file_too_large` with `Connection: close`; a body that is not PNG or JPEG by content (SVG, GIF, WebP, HTML, a JPEG renamed `.png` passes, a PNG with a `Content-Type: text/html` header passes) is 400 `avatar_invalid`; declared dimensions over 4096 a side or 12 M pixels are 400 before any pixel decode (a crafted 20000 by 20000 PNG header with a tiny body is refused in microseconds); a truncated file is 400; 200 `PrincipalAvatarV1` with `width, height <= 256`, `media_type = image/png`, `sha256` equal to the hash of the bytes `GET` returns.
- [ ] The stored PNG's chunk list is exactly `IHDR [PLTE] [tRNS] IDAT* IEND` for a source that carries EXIF (with GPS), a `tEXt` comment and an ICC profile; a transparent PNG source stays transparent; a 4000 by 3000 JPEG becomes 256 by 256 centre-cropped; a 100 by 100 source stays 100 by 100; each of the eight EXIF orientations yields the same pixels as the unrotated reference (O4).
- [ ] Replacing with the same picture answers 200 with the same hash, writes no audit row and sends no signal; replacing with a different one writes `avatar_set` and sends `notifyRosters` and `directoryPrincipalChanged`.
- [ ] `DELETE`: self 204 and `avatar_removed`; a second `DELETE` 204 with no row; a holder of `manage_members` 204 with `avatar_removed … moderation=true`, 403 for a non-owner on an owner, 403 `permission_denied` for an operator without `manage_members` (the store re-check too); a member on another id 403; a missing or agent id 404 `principal_not_found`.
- [ ] `GET`: `Content-Type: image/png`, `nosniff`, `ETag`, 304 on `If-None-Match`, `Cache-Control` per §3.7 with and without a matching `?v=`; a stale `?v=` serves the current bytes; `HEAD` works; an agent 403; every refusal and absence is one byte-identical 404 `avatar_not_found`: unknown id, disabled, hidden, agent, no avatar, and a principal the caller may not know with `see_member_directory` off for members.
- [ ] Visibility: with `see_member_directory` off for members, alice (in A only) fetches the owner's and her invitee's avatars and not bob's (B only); after bob posts in public C that alice is in and leaves C, alice fetches bob's (authored clause); the structural guard test covers the avatar query.
- [ ] `ServerMemberV1`, `ChannelMemberV1` (REST and the members frame), `ChannelAuthorV1` and `WhoAmIResponseV1` carry `avatar_sha256` for a human with one and omit it otherwise; agent rows never carry it.
- [ ] `HidePrincipal` deletes the row; after re-enable of a disabled principal the picture is back.
- [ ] Limits: the 11th set in a day and the 31st attempt answer 429 with `Retry-After`; refused uploads write `avatar_set_refused` and count; a restart does not reset the count.
- [ ] Concurrency: with the semaphore at 2 and two decodes held, a third upload whose context ends answers 503 `avatar_busy` with `Retry-After`.
- [ ] A structural test asserts the server binary's image decoders are exactly `png` and `jpeg` (no `image/gif` or `golang.org/x/image` import anywhere under `internal/`, `cmd/`).
- [ ] `golangci-lint` clean; no new `go.mod` dependency (or, if O3 chooses WebP, the dependency is on `deps-allowlist.txt` with Nick's sign-off recorded on the issue).

**Required tests.** Handler tests for every bullet (table-driven), with fixture images generated in the test (stdlib encoders plus hand-built EXIF/`tEXt`/ICC chunks: no binary fixtures checked in unless tiny); store tests for limits and `avatarVisibleTx`; an events test that a set changes the members frame and sends the directory signal to the right viewers and not to others; an `e2e/isolation` phase with the avatar rows of §3.6 in both settings; `TestRouteTable` and `TestRecentAuthClass` rows for the three routes.

**Blocked by.** A1; Nick's answers to O1, O3–O6, O9.

### A3. Web: avatar rendering, cache and Settings (`area/web`)

**Context.** [docs/design/avatars.md](avatars.md) §3.9 (browser), §3.7 (client cache), §4. The browser shows fetched, validated `blob:` pictures in the existing `Avatar` component and lets a person set, preview, crop and remove their own.

**Files.** `web/src/Icons.tsx` (`Avatar` with an `avatar` prop), `web/src/useAvatar.ts` (new: the cache and hook), `web/src/avatarCache.ts` (new, or inside the hook: the map, LRU, revoke-all), `web/src/api.ts` (`fetchAvatar`, `putAvatar`, `deleteAvatar`; `web/src/platform/operations.ts` entries), `web/src/useLiveChannel.ts` and `useAuthorNames.ts` (`Person.avatar`), `web/src/Conversation.tsx`, `Members.tsx`, `Lobby.tsx`, `voice/VoicePanel.tsx`, `voice/CallView.tsx` (pass the prop), `web/src/AvatarSettings.tsx` (new: pick, validate, crop, preview, send, remove), `web/src/App.tsx` (the panel after `ChangeName`; clear the cache on logout and account change; the `avatar_sha256` in auth state), `web/src/app.css` (`.avatar > img`, the crop control), tests beside each.

**Acceptance criteria.**
- [ ] A member with `avatar_sha256` on their member row shows the picture beside their messages, in the people list, the community list, the voice roster and the camera-off tile; one without shows today's initial; a picture that fails to fetch, validate or is 404 falls back to the initial without a console error and is not refetched for that hash.
- [ ] One fetch per `(id, sha256)` per page load however many places draw it; object URLs are revoked on eviction, logout, access loss and server/account change (the attachment rule); the request uses the bearer header and never a token in a URL.
- [ ] A `members` frame or `directory_changed` re-read that carries a new hash replaces the picture without a reload; a removed one returns to the initial.
- [ ] Settings: choosing a non-image, an oversized or a corrupt file shows the refusal before any request; a JPEG with EXIF orientation previews the right way up; the crop is square and the sent blob is a 512 by 512 PNG; `file_too_large`, `avatar_invalid`, `avatar_busy` and `rate_limited` show their lines; `unauthorized` ends the credential; Remove asks once and then calls `DELETE`; the top bar and drawer change on success.
- [ ] Phone width: the panel and crop work at 320 px with no horizontal scroll (mobile-browser-layout rules).
- [ ] `tsc`, `eslint`, `vitest` clean; no new dependency; no inline style or script; CSP unchanged.

**Required tests.** Unit tests (no network): the cache (dedupe, LRU, revoke-all), the `Avatar` fallback paths, `AvatarSettings` refusals and the result map, `Person.avatar` plumbing through `useLiveChannel` and `useAuthorNames`; a Playwright test (its own job) that uploads a generated PNG, sees it beside a posted message in a second browser, removes it and sees the initial return.

**Blocked by.** A2 (the routes), A1 (the generated types).

### A4. Desktop: avatar bridge operations (`area/desktop`)

**Context.** [docs/design/avatars.md](avatars.md) §3.9. The desktop renderer never holds the token; file bytes cross the main-process bridge, which allows only `uploadFile` and `fileBytes`. Avatars need two more operations or the desktop shows initials only.

**Files.** `desktop/main/files.ts`, `desktop/main/ipc.ts` (`avatarBytes(id, v)`, `putAvatar(id, bytes)`, `deleteAvatar(id)` with the download bound and timeout of `fileBytes`), `web/src/platform/desktop/http.ts` (route the three operations through the bridge), tests beside each.

**Acceptance criteria.**
- [ ] On desktop, avatars render and Settings can set and remove one, through the bridge, with no token in the renderer.
- [ ] The bridge refuses a body over `MaxAvatarUploadBytes` and a download over the attachment bound; unknown operations stay refused.
- [ ] Without A4, the web build on desktop still falls back to initials (A3 must not throw when the platform lacks the operation).

**Required tests.** Bridge unit tests for the three operations and their bounds; the existing desktop CSP test unchanged.

**Blocked by.** A3.

### A5. CLI: `conch avatar` and the admin remove (`area/cli`)

**Context.** [docs/design/avatars.md](avatars.md) §3.9 (CLI). Parity for setting, removing and fetching one's own avatar, and the operator's remove, over the REST routes. The TUI changes nothing.

**Files.** `internal/cli/command.go` (`case "avatar":`, Usage), `internal/cli/avatar.go` (new: `runAvatar` with `set`, `remove`, `show`), `internal/cli/admin.go` (`principal avatar-remove <id>`, `--yes`), `internal/cli/client.go` or `client_avatar.go` (`PutAvatar`, `DeleteAvatar`, `GetAvatar`), tests beside each; `README.md` / the CLI reference if one lists commands.

**Acceptance criteria.**
- [ ] `conch avatar set photo.jpg` sends the raw bytes and prints the result (`--json` prints `PrincipalAvatarV1`); a file over 2 MiB is refused locally with the limit named; the server's `avatar_invalid`, `file_too_large`, `rate_limited` (with the wait) and `avatar_busy` are printed as one line each.
- [ ] `conch avatar remove` is 204-idempotent and says "removed" or "none set"; `conch avatar show --out me.png` writes the bytes after verifying the hash matches the `ETag`.
- [ ] `conch admin principal avatar-remove <id>` requires `--yes` non-interactively and confirms interactively, naming the principal; a 403 prints the permission.
- [ ] Usage and `conch help` list the commands; `golangci-lint` clean.

**Required tests.** Table-driven command tests against an `httptest` server for every bullet, including the local size refusal and the hash check on `show`.

**Blocked by.** A2.

## 9. Proposed ADR text

No ADR decision changes. Two accepted ADRs would gain a sentence each if Nick wants the record explicit; the text is proposed here and **not** edited into either ADR (ADR changes are Nick's).

**ADR-013, Consequences**, a bullet after "The optional `hide_members` setting (#964)":

> - **Avatars (#974, [docs/design/avatars.md](../design/avatars.md))** add one optional field, `avatar_sha256`, to the four documents that name a principal to a viewer and one route that serves the bytes under the same rule as the name: the directory predicate, or authorship of a message the viewer may read (§13.5). No new events document: a change rides the rename's signals. No version bump.

**ADR-014, Consequences** (or wherever the catalog's "no new permission" notes live), one sentence:

> An operator's removal of another person's avatar is `manage_members` (row 7, "administer people without speaking as them"); setting one is self-only and needs no permission. No row is added to the catalog.

If Nick prefers no ADR text at all, this note and the issue are the record; nothing in either ADR's decision table moves.

## 10. Notes and what was not verified

- **Not run:** no code was built or executed for this note; the claims of §1 were read from the source at `d7afd8c7`. Memory figures in §3.2 (decoded image sizes) are from the stdlib's documented colour models, not measured.
- **Desktop bridge limits** (`MAX_DOWNLOAD_BYTES`, timeouts in `desktop/main/files.ts`) were seen by name; A4 reuses them without this note fixing their values.
- **`accessOperatorOrSelf`** is, by its own comment in `internal/server/auth.go`, "authenticated; the handler additionally requires `manage_members` or that the caller is the principal named in the path". So the class admits every authenticated caller and the handler decides, which is what §3.5 relies on: the PUT handler's self-only check and the DELETE handler's `manage_members` branch are the rule, not the class.
- **The authored-message clause's cost** (an `EXISTS` over `messages` by `author_id` under `messageVisible`) is not measured. The indexes on `messages` in `store.go` are `messages_by_channel (channel_id, id)` and `messages_by_thread (thread_root_id, id)`; none is by `author_id`. A2 adds `messages_by_author (author_id, id)` in its migration if the route needs it.
- **Line numbers** in §1 were deliberately left out; names were checked and are stable across the inventory's two independent reads.
