package schema

// The tables cmd/schema-ts reads to write the web client's wire types,
// web/src/schema/generated.ts, and the list of fixture families its test
// checks, generated.families.ts (issue #348). Everything else comes from the
// Go types themselves: field names from the json tags, and the values of a
// closed vocabulary from Vocabularies; TSVocabularyModes says which of those
// the client tolerates a value it does not know in. Run `go generate
// ./pkg/schema` after changing a table or a type listed in one;
// scripts/schema-ts-check.sh fails while the committed files are stale.

// TSType is a Go struct the web client uses, written as a TypeScript
// interface and a validator.
type TSType struct {
	// Value is a zero value of the struct.
	Value any
	// Name is the TypeScript name, when it is not the Go name: Error would
	// hide JavaScript's own.
	Name string
	// Family is the base name of the struct's golden fixtures in testdata:
	// <family>.json and every <family>-*.json. Empty for a struct that has
	// none of its own.
	Family string
	// Schema is the value every one of these documents carries in its schema
	// field. The generated type allows that value alone, so a document of
	// another schema or version is refused rather than read as this one.
	Schema string
}

// TSUnion is a Go struct that stands for a union discriminated by one field:
// which other fields it has depends on that field's value. It is written as
// one TypeScript interface per variant and a validator that picks the variant
// by the tag.
type TSUnion struct {
	// Value is a zero value of the struct.
	Value any
	// Tag is the json name of the field that picks the variant. Its Go type
	// is a vocabulary, and every value of it must have a variant.
	Tag string
	// Variants maps each value of the tag to the json names of the fields
	// that variant has besides the tag; each is required in it. Every other
	// field of the struct must be in at least one variant.
	Variants map[string][]string
}

// TSConstant is a scalar of this package the web client must agree with the
// server on: a version string or a bound. It is written as
// `export const <Name> = <value>;` so a page cannot carry its own copy of a
// number the server enforces.
type TSConstant struct {
	// Name is the TypeScript name, in the SCREAMING_SNAKE_CASE of a TypeScript
	// constant.
	Name string
	// Value is the constant: a string or a signed integer.
	Value any
}

// TSConstants is every scalar the web client reads from this package. The
// emoji catalog version and the reaction bounds (issue #953): the browser
// refuses the same keys the server refuses, and cannot ship a catalog of
// another version than the server validates against without
// scripts/schema-ts-check.sh noticing.
var TSConstants = []TSConstant{
	{Name: "EMOJI_VERSION", Value: EmojiVersion},
	{Name: "MAX_REACTION_KEY_BYTES", Value: MaxReactionKeyBytes},
	{Name: "MAX_REACTION_KEY_CODE_POINTS", Value: MaxReactionKeyCodePoints},
	{Name: "MAX_REACTION_BATCH_MESSAGES", Value: MaxReactionBatchMessages},
	{Name: "DEFAULT_REACTOR_LIMIT", Value: DefaultReactorLimit},
	{Name: "MAX_REACTOR_LIMIT", Value: MaxReactorLimit},
	// Message editing (issue #845): the page stops offering Edit when the
	// server would refuse it.
	{Name: "MESSAGE_EDIT_WINDOW_SECONDS", Value: MessageEditWindowSeconds},
	{Name: "MAX_MESSAGE_EDITS", Value: MaxMessageEdits},
	{Name: "MAX_POST_REQUEST_BYTES", Value: MaxPostRequestBytes},
	// A spoiler attachment (issue #1500): presentation only, no shape change.
	{Name: "SPOILER_FILE_PREFIX", Value: SpoilerFilePrefix},
	{Name: "MAX_FILE_NAME_BYTES", Value: MaxFileNameBytes},

	// The managed voice-helper control protocol, for the desktop owner's
	// supervisor (issue #948, Q10 of #770): the helper's command line, the
	// framing bounds, the owner's timers and the attach line's nonce. desktop/
	// imports them; the browser bundle does not.
	{Name: "VOICE_CONTROL_SUBCOMMAND", Value: VoiceControlSubcommand},
	{Name: "VOICE_CONTROL_MAX_LINE_BYTES", Value: VoiceControlMaxLineBytes},
	{Name: "VOICE_CONTROL_MAX_ID", Value: int64(VoiceControlMaxID)},
	{Name: "VOICE_CONTROL_MAX_IN_FLIGHT", Value: VoiceControlMaxInFlight},
	{Name: "VOICE_CONTROL_MAX_VERSIONS", Value: VoiceControlMaxVersions},
	{Name: "VOICE_CONTROL_MAX_HELPER_VERSION_BYTES", Value: VoiceControlMaxHelperVersionBytes},
	{Name: "VOICE_CONTROL_MAX_SERVER_BYTES", Value: VoiceControlMaxServerBytes},
	{Name: "VOICE_CONTROL_MAX_CHANNEL_BYTES", Value: VoiceControlMaxChannelBytes},
	{Name: "VOICE_CONTROL_MAX_RETRY_IN_MS", Value: VoiceControlMaxRetryInMS},
	{Name: "VOICE_CONTROL_HELLO_TIMEOUT_MS", Value: VoiceControlHelloTimeoutMS},
	{Name: "VOICE_CONTROL_ACK_TIMEOUT_MS", Value: VoiceControlAckTimeoutMS},
	{Name: "VOICE_CONTROL_GATE_SHUT_MS", Value: VoiceControlGateShutMS},
	{Name: "VOICE_CONTROL_CAPTURE_STOP_MS", Value: VoiceControlCaptureStopMS},
	{Name: "VOICE_CONTROL_OWNER_LOSS_EXIT_MS", Value: VoiceControlOwnerLossExitMS},
	{Name: "VOICE_CONTROL_LEAVE_EXIT_MS", Value: VoiceControlLeaveExitMS},
	{Name: "VOICE_CONTROL_STOP_TERM_AFTER_MS", Value: VoiceControlStopTermAfterMS},
	{Name: "VOICE_CONTROL_STOP_KILL_AFTER_MS", Value: VoiceControlStopKillAfterMS},
	{Name: "VOICE_CONTROL_DEVICE_DEFAULT", Value: VoiceControlDeviceDefault},
	{Name: "VOICE_DEVICES_MAX_NAME_BYTES", Value: VoiceDevicesMaxNameBytes},
	{Name: "VOICE_DEVICES_MAX_BYTES", Value: VoiceDevicesMaxBytes},
	{Name: "VOICE_CONTROL_PIPE_FLAG", Value: VoiceControlPipeFlag},
	{Name: "VOICE_CONTROL_PIPE_NONCE_ENV", Value: VoiceControlPipeNonceEnv},
	{Name: "VOICE_CONTROL_PIPE_NONCE_BYTES", Value: VoiceControlPipeNonceBytes},
	{Name: "VOICE_CONTROL_PIPE_NONCE_CHARS", Value: VoiceControlPipeNonceChars},

	// The community model (ADR-013, issue #978): the authors route's batch
	// size, and the default expiry the link form shows when none is typed.
	{Name: "MAX_CHANNEL_AUTHOR_IDS", Value: MaxChannelAuthorIDs},
	// The channel topic (issue #1503): the page bounds its control by the
	// number the server refuses at.
	{Name: "MAX_CHANNEL_TOPIC_LENGTH", Value: MaxChannelTopicLength},
	{Name: "DEFAULT_MEMBER_LINK_EXPIRY_SECONDS", Value: DefaultMemberLinkExpirySeconds},

	// AI administrators (issue #1326): the bounds of an agent's admin
	// settings, so the console's form refuses what the server refuses, and
	// the reason cap the approval card explains.
	{Name: "DEFAULT_ADMIN_APPROVAL_TTL_SECONDS", Value: DefaultAdminApprovalTTLSeconds},
	{Name: "MIN_ADMIN_APPROVAL_TTL_SECONDS", Value: MinAdminApprovalTTLSeconds},
	{Name: "MAX_ADMIN_APPROVAL_TTL_SECONDS", Value: MaxAdminApprovalTTLSeconds},
	{Name: "DEFAULT_ADMIN_RUN_WINDOW_SECONDS", Value: DefaultAdminRunWindowSeconds},
	{Name: "MIN_ADMIN_RUN_WINDOW_SECONDS", Value: MinAdminRunWindowSeconds},
	{Name: "MAX_ADMIN_RUN_WINDOW_SECONDS", Value: MaxAdminRunWindowSeconds},
	{Name: "DEFAULT_ADMIN_QUORUM", Value: DefaultAdminQuorum},
	{Name: "DEFAULT_ADMIN_MAX_PENDING", Value: DefaultAdminMaxPending},
	{Name: "MAX_ADMIN_MAX_PENDING", Value: MaxAdminMaxPending},
	{Name: "MAX_ADMIN_REASON_CHARS", Value: MaxAdminReasonChars},
}

// TSTypes is every struct the web client reads or sends, and the refusal
// body. A struct a listed one contains must be listed too, here or in
// TSUnions.
var TSTypes = []TSType{
	// The caller, and the channels it can see.
	{Value: WhoAmIResponseV1{}, Family: "whoami-response-v1"},
	{Value: ChannelV0{}, Family: "channel-v0"},
	{Value: ListChannelsResponse{}, Family: "list-channels-response"},
	// An operator creates a channel from the browser (issue #695).
	{Value: CreateChannelRequest{}, Family: "create-channel-request-v0"},
	{Value: CreateChannelResponse{}, Family: "create-channel-response-v0"},
	// An operator renames one (issue #727); the events socket says so to
	// every open page of it.
	{Value: RenameChannelRequestV1{}, Family: "rename-channel-request-v1"},
	{Value: RenameChannelResponseV1{}, Family: "rename-channel-response-v1"},
	{Value: ChannelDocV1{}, Family: "channel-v1", Schema: ChannelSchemaV1},
	{Value: SetChannelTopicRequestV1{}, Family: "set-channel-topic-request-v1"},
	{Value: SetChannelTopicResponseV1{}, Family: "set-channel-topic-response-v1"},
	{Value: ChannelMemberV1{}, Family: "channel-member-v1"},
	{Value: ListChannelMembersResponseV1{}, Family: "list-channel-members-response-v1"},
	{Value: ChannelMembersV1{}, Family: "channel-members-v1", Schema: ChannelMembersSchemaV1},
	{Value: PutChannelMemberRequestV1{}, Family: "put-channel-member-request-v1"},
	{Value: AddChannelMemberRequestV1{}, Family: "add-channel-member-request-v1"},
	// The channel's ban list (issue #849, docs/design/moderation.md §6).
	{Value: ChannelBanV1{}, Family: "channel-ban-v1"},
	{Value: ListChannelBansResponseV1{}, Family: "list-channel-bans-response-v1"},

	// Messages, and the ticket that opens the socket that streams them.
	{Value: Payload{}},
	{Value: MessageV2{}, Family: "message-v2", Schema: MessageSchemaV2},
	{Value: PostMessageRequestV2{}, Family: "post-message-request-v2"},
	{Value: PostMessageResponseV2{}, Family: "post-message-response-v2"},
	{Value: ListMessagesResponseV2{}, Family: "list-messages-response-v2"},
	{Value: SearchMessagesResponseV2{}, Family: "search-messages-response-v2"},
	{Value: WSTicketV1{}, Family: "ws-ticket-v1"},
	// The author edits their own recent text (issue #845); the events socket
	// says so with a frame of its own.
	{Value: EditMessageRequestV1{}, Family: "edit-message-request-v1"},
	{Value: EditMessageResponseV1{}, Family: "edit-message-response-v1"},
	{Value: MessageRevisionV1{}, Family: "message-revision-v1"},
	{Value: ListMessageRevisionsResponseV1{}, Family: "list-message-revisions-response-v1"},
	{Value: MessageEditedV1{}, Family: "message-edited-v1", Schema: MessageEditedSchemaV1},
	// A moderator removes a message (issue #849); the events socket says so
	// with a frame of its own, carrying the tombstone.
	{Value: MessageRemovedV1{}, Family: "message-removed-v1", Schema: MessageRemovedSchemaV1},

	// An approval, as a message carries it in its payload.
	{Value: Option{}, Family: "approval-option-v1"},
	{Value: ApprovalV1{}, Family: "conch-approval-v1"},
	// Deciding an admin-action approval from its card (issue #1334).
	{Value: CastDecisionRequestV1{}, Family: "cast-decision-request-v1"},
	// The open approvals, which the conversation reads to draw an AI
	// administrator's request as a card (issue #1334).
	{Value: ListApprovalsResponseV1{}, Family: "list-approvals-response-v1"},

	// Files (issue #23): the metadata the upload and metadata routes answer,
	// and the payload a message attaches them with (issue #762).
	{Value: FileV1{}, Family: "file-v1", Schema: FileSchemaV1},
	{Value: FileAttachmentV1{}, Family: "file-attachment-v1"},

	// Nets.
	{Value: NetMember{}},
	{Value: NetV1{}, Family: "net-v1"},
	{Value: ListNetsResponseV1{}, Family: "list-nets-response-v1"},

	// Agent presence: sessions, runs, activity and interventions.
	{Value: SessionV1{}, Family: "session-v1", Schema: SessionSchemaV1},
	{Value: SessionRosterV1{}, Family: "session-roster-v1", Schema: SessionRosterSchemaV1},
	{Value: HeartbeatV1{}, Family: "heartbeat-v1", Schema: HeartbeatSchemaV1},
	{Value: CreateSessionRequestV1{}, Family: "create-session-request-v1"},
	{Value: RunV1{}, Family: "run-v1", Schema: RunSchemaV1},
	{Value: ListRunsResponseV1{}, Family: "list-runs-response-v1"},
	{Value: AgentActivityV1{}, Family: "agent-activity-v1", Schema: AgentActivitySchemaV1},
	{Value: ListActivityResponseV1{}, Family: "list-activity-response-v1"},
	{Value: InterventionV1{}, Family: "intervention-v1", Schema: InterventionSchemaV1},
	{Value: CreateInterventionRequestV1{}, Family: "create-intervention-request-v1"},
	{Value: ListInterventionsResponseV1{}, Family: "list-interventions-response-v1"},

	// Read state: the person's mark in each channel and the unread counts.
	{Value: ReadStateV1{}, Family: "read-state-v1", Schema: ReadStateSchemaV1},
	{Value: MarkReadRequestV1{}, Family: "mark-read-request-v1"},
	{Value: ListReadStateResponseV1{}, Family: "list-read-state-response-v1"},
	// The channel-list signal (issue #1027): re-read GET /v1/channels.
	{Value: ChannelsChangedV1{}, Family: "channels-changed-v1", Schema: ChannelsChangedSchemaV1},

	// Browser voice (issue #442): the session that admits the page to a room,
	// the presence snapshot its socket streams, and the transmit report.
	{Value: VoiceRoomGrant{}},
	{Value: VoiceSessionResponseV1{}, Family: "voice-session-response-v1"},
	// The quality a sharer declared (issue #1483, design §3.2): on their
	// participant in presence, in the claim's body and in its answer.
	{Value: VoiceShareQuality{}},
	{Value: VoiceParticipant{}},
	{Value: VoicePresenceRoom{}},
	// Why the reader's own share ended (issue #487), in their presence alone.
	{Value: VoiceShareEnded{}},
	// Why the reader's own camera ended (ADR-015, issue #1238), likewise.
	{Value: VoiceCameraEnded{}},
	{Value: VoicePresenceV1{}, Family: "voice-presence-v1", Schema: VoicePresenceSchemaV1},
	// Presence v2 (issue #1483, design §2.2): v1 without the one-share rule,
	// which the page asks for with ?schema=conch.voice_presence.v2. Its
	// participant carries the share's start and quality; v1's is closed and
	// gains nothing (object() refuses an unknown key).
	{Value: VoiceParticipantV2{}},
	{Value: VoicePresenceRoomV2{}},
	{Value: VoicePresenceV2{}, Family: "voice-presence-v2", Schema: VoicePresenceSchemaV2},
	{Value: VoiceTransmitReportV1{}, Family: "voice-transmit-report-v1"},
	// Companion and viewer participants (issue #933).
	{Value: VoiceCompanionRequestV1{}, Family: "voice-companion-request-v1"},
	{Value: VoiceCompanionResponseV1{}, Family: "voice-companion-response-v1"},
	// The companion's share audio (issue #898): the holder's consent, by
	// scope, and the answer naming the share it belongs to.
	{Value: VoiceCompanionAudioRequestV1{}, Family: "voice-companion-audio-request-v1"},
	{Value: VoiceCompanionAudioV1{}, Family: "voice-companion-audio-v1"},
	// Screen share (issue #443): the claim the page is given when it may
	// share. Presence says who shares (VoiceParticipant above). The claim's
	// optional body declares the quality (issue #1483, design §3.1), and a
	// claim that declared one is answered with the v2 claim, which echoes it.
	{Value: VoiceShareRequestV1{}, Family: "voice-share-request-v1"},
	{Value: VoiceShareV1{}, Family: "voice-share-v1"},
	{Value: VoiceShareV2{}, Family: "voice-share-v2"},
	// The camera claim (ADR-015, issue #1238): the page may publish a camera
	// while conchd holds it. Presence says who has one (VoiceParticipant).
	{Value: VoiceCameraV1{}, Family: "voice-camera-v1"},
	// Voice occupancy (issue #596): how many are in each channel's voice,
	// streamed on a socket of its own so the channel list can show it.
	{Value: VoiceChannelOccupancy{}},
	{Value: VoiceOccupancyV1{}, Family: "voice-occupancy-v1", Schema: VoiceOccupancySchemaV1},
	// The managed voice-helper control protocol (issue #948, Q10 of #770):
	// what the desktop owner writes to conch-voice and reads back, v1 and v2,
	// and the attach line of its events endpoint. Their fixtures are in
	// testdata/voice-control*/, a directory per version, so the rows name no
	// family; desktop/tests/unit/voice-protocol.test.ts checks every one of
	// them against these validators. The browser bundle does not import them.
	{Value: VoiceControlStartV1{}, Schema: VoiceControlSchemaV1},
	{Value: VoiceControlSetMuteV1{}, Schema: VoiceControlSchemaV1},
	{Value: VoiceControlSetDeafenV1{}, Schema: VoiceControlSchemaV1},
	{Value: VoiceControlGetStateV1{}, Schema: VoiceControlSchemaV1},
	{Value: VoiceControlLeaveV1{}, Schema: VoiceControlSchemaV1},
	{Value: VoiceControlHelloV1{}, Schema: VoiceControlSchemaV1},
	{Value: VoiceControlAckV1{}, Schema: VoiceControlSchemaV1},
	{Value: VoiceControlStateV1{}, Schema: VoiceControlSchemaV1},
	{Value: VoiceControlProtocolErrorV1{}, Schema: VoiceControlSchemaV1},
	{Value: VoiceControlStartV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlSetMuteV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlSetDeafenV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlGetStateV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlLeaveV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlSetDevicesV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlSetTalkModeV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlAckV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlStateV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlProtocolErrorV2{}, Schema: VoiceControlSchemaV2},
	{Value: VoiceControlPipeAttachedV1{}, Schema: VoiceControlPipeSchemaV1},
	// The device list `conch-voice devices --json` writes (issue #1378, D3-7
	// of #771): the desktop owner reads it from a one-shot run. Its fixtures
	// are in testdata/voice-devices/, so the rows name no family;
	// desktop/tests/unit/voice-oneshot.test.ts checks every one of them and
	// every case of cases.json against this validator.
	{Value: VoiceDevice{}},
	{Value: VoiceDevicesV1{}, Schema: VoiceDevicesSchemaV1},
	// Invites: the way in (ADR-008 §4). The code is in the create response
	// alone; the new principal is in the redeem response.
	{Value: PrincipalV0{}, Family: "principal-v0"},
	{Value: CredentialV1{}, Family: "credential-v1"},
	{Value: InviteChannelV1{}},
	{Value: InviteV1{}, Family: "invite-v1"},
	{Value: CreateInviteRequestV1{}, Family: "create-invite-request-v1"},
	{Value: CreateInviteResponseV1{}, Family: "create-invite-response-v1"},
	{Value: InvitePreviewRequestV1{}, Family: "invite-preview-request-v1"},
	{Value: InvitePreviewV1{}, Family: "invite-preview-v1"},
	{Value: RedeemInviteRequestV1{}, Family: "redeem-invite-request-v1"},
	{Value: RedeemInviteResponseV1{}, Family: "redeem-invite-response-v1"},
	{Value: ListInvitesResponseV1{}, Family: "list-invites-response-v1"},
	// Operator administration of invites (issue #438).
	{Value: InviteJoinerV1{}},
	{Value: InviteJoinersResponseV1{}, Family: "invite-joiners-response-v1"},
	{Value: DisableJoinersRequestV1{}, Family: "disable-joiners-request-v1"},
	{Value: DisableJoinersResponseV1{}, Family: "disable-joiners-response-v1"},
	{Value: DisableInviteesRequestV1{}, Family: "disable-invitees-request-v1"},
	{Value: InvitePermissionRequestV1{}, Family: "invite-permission-request-v1"},
	{Value: InviteSettingsV1{}, Family: "invite-settings-v1"},
	// Sign-in links (issue #493): the landing page of a link an operator made
	// asks whose account it opens, then uses it. The page never makes one.
	{Value: SignInLinkPreviewRequestV1{}, Family: "sign-in-link-preview-request-v1"},
	{Value: SignInLinkPreviewV1{}, Family: "sign-in-link-preview-v1"},
	{Value: RedeemSignInLinkRequestV1{}, Family: "redeem-sign-in-link-request-v1"},
	{Value: RedeemSignInLinkResponseV1{}, Family: "redeem-sign-in-link-response-v1"},
	// How the browser suite's operator makes one, as `conch admin principal
	// sign-in-link` does.
	{Value: SignInLinkV1{}, Family: "sign-in-link-v1"},
	{Value: CreateSignInLinkRequestV1{}, Family: "create-sign-in-link-request-v1"},
	{Value: CreateSignInLinkResponseV1{}, Family: "create-sign-in-link-response-v1"},
	// Signing out everywhere (issue #439): the page revokes every credential
	// of its own principal. Signing out of one credential has no body.
	{Value: RevokeAllCredentialsResponseV1{}, Family: "revoke-all-credentials-response-v1"},
	// Your sign-ins (issue #496): the page lists the person's own live
	// credentials and ends one by id. Ending one has no body.
	{Value: OwnCredentialV1{}, Family: "own-credential-v1"},
	{Value: ListOwnCredentialsResponseV1{}, Family: "list-own-credentials-response-v1"},
	// How the browser suite's operator sets up a person of its own to sign
	// out everywhere (issue #439). The page itself never sends these.
	{Value: CreatePrincipalRequest{}, Family: "create-principal-request-v0"},
	{Value: CreatePrincipalResponse{}, Family: "create-principal-response-v0"},
	{Value: CreateCredentialRequestV1{}, Family: "create-credential-request-v1"},
	{Value: CreateCredentialResponseV1{}, Family: "create-credential-response-v1"},
	// Binding a hook to a person (POST /v1/hooks), which the console's member
	// menu does as acting as that person (issue #1020).
	{Value: CreateHookRequest{}, Family: "create-hook-request-v1"},
	{Value: CreateHookResponse{}, Family: "create-hook-response-v1"},
	// Changing your name (issue #441).
	{Value: RenamePrincipalRequestV1{}, Family: "rename-principal-request-v1"},
	{Value: RenamePrincipalResponseV1{}, Family: "rename-principal-response-v1"},
	// Operator administration of principals (issue #445): the list, and
	// hiding one or many.
	{Value: PrincipalAdminV1{}, Family: "principal-admin-v1"},
	{Value: ListPrincipalsResponseV1{}, Family: "list-principals-response-v1"},
	{Value: HidePrincipalResponseV1{}, Family: "hide-principal-response-v1"},
	{Value: HidePrincipalsRequestV1{}, Family: "hide-principals-request-v1"},
	{Value: HiddenPrincipalV1{}},
	{Value: HidePrincipalsResponseV1{}, Family: "hide-principals-response-v1"},
	{Value: RoleChangeRequestV1{}, Family: "role-change-request-v1"},
	{Value: RoleChangeResponseV1{}, Family: "role-change-response-v1"},

	// Temporary guests and joining (ADR-009, issue #704): the front door,
	// a visit and how it is rendered, conversion, and the operator's
	// policy and guest list.
	{Value: GuestAttributionV1{}, Family: "guest-attribution-v1"},
	{Value: GuestV1{}, Family: "guest-v1"},
	{Value: GuestAdminV1{}, Family: "guest-admin-v1"},
	{Value: ListGuestsResponseV1{}, Family: "list-guests-response-v1"},
	{Value: EndGuestResponseV1{}, Family: "end-guest-response-v1"},
	{Value: EndVisitResponseV1{}, Family: "end-visit-response-v1"},
	{Value: EndGuestsRequestV1{}, Family: "end-guests-request-v1"},
	{Value: GuestRefV1{}},
	{Value: EndGuestsResponseV1{}, Family: "end-guests-response-v1"},
	{Value: JoinOptionsV1{}},
	{Value: JoinInfoV1{}, Family: "join-info-v1"},
	{Value: JoinSettingsV1{}, Family: "join-settings-v1"},
	{Value: PutJoinSettingsRequestV1{}, Family: "put-join-settings-request-v1"},
	{Value: JoinSettingsResponseV1{}, Family: "join-settings-response-v1"},
	{Value: JoinGuestRequestV1{}, Family: "join-guest-request-v1"},
	{Value: JoinGuestResponseV1{}, Family: "join-guest-response-v1"},
	{Value: JoinAccountRequestV1{}, Family: "join-account-request-v1"},
	{Value: JoinAccountResponseV1{}, Family: "join-account-response-v1"},
	{Value: ConvertGuestRequestV1{}, Family: "convert-guest-request-v1"},
	{Value: ConvertGuestResponseV1{}, Family: "convert-guest-response-v1"},
	{Value: CreateConversionPermitRequestV1{}, Family: "create-conversion-permit-request-v1"},
	// Invites that admit guests, and account redemption with a password.
	{Value: RedeemInviteRequestV2{}, Family: "redeem-invite-request-v2"},
	{Value: RedeemInviteGuestRequestV1{}, Family: "redeem-invite-guest-request-v1"},

	// Passwords and sign-in (ADR-009 §5–6, issue #704): sign-in and its
	// TOTP step, setting, changing and resetting a password,
	// re-authentication, TOTP enrolment, the setup token, and the
	// member's own device links.
	{Value: LoginRequestV1{}, Family: "login-request-v1"},
	{Value: TOTPLoginRequestV1{}, Family: "totp-login-request-v1"},
	{Value: SetPasswordRequestV1{}, Family: "set-password-request-v1"},
	{Value: ChangePasswordRequestV1{}, Family: "change-password-request-v1"},
	{Value: ResetPasswordRequestV1{}, Family: "reset-password-request-v1"},
	{Value: PasswordUpdatedResponseV1{}, Family: "password-updated-response-v1"},
	{Value: ReauthRequestV1{}, Family: "reauth-request-v1"},
	{Value: ReauthResponseV1{}, Family: "reauth-response-v1"},
	{Value: AuthStateV1{}, Family: "auth-state-v1"},
	{Value: TOTPEnrollmentV1{}, Family: "totp-enrollment-v1"},
	{Value: ConfirmTOTPRequestV1{}, Family: "confirm-totp-request-v1"},
	{Value: RedeemSetupTokenRequestV1{}, Family: "redeem-setup-token-request-v1"},
	{Value: RedeemSetupTokenResponseV1{}, Family: "redeem-setup-token-response-v1"},
	{Value: CreateDeviceLinkRequestV1{}, Family: "create-device-link-request-v1"},
	{Value: ListOwnSignInLinksResponseV1{}, Family: "list-own-sign-in-links-response-v1"},

	// The server's health, which carries its version (the sign-in page shows
	// it). Not a versioned payload, so it has no fixture family.
	{Value: Health{}},

	// Reporting a problem (issue #446): what the page sends, and the id it
	// gets back; and, for operators (issue #1029), the list, one report as
	// read, and marking one filed.
	{Value: ProblemReportClientV1{}},
	{Value: DiagnosticEventV1{}},
	{Value: CreateProblemReportRequestV1{}, Family: "create-problem-report-request-v1"},
	{Value: CreateProblemReportResponseV1{}, Family: "create-problem-report-response-v1"},
	{Value: ServerRequestTraceV1{}},
	{Value: ServerSocketCloseV1{}},
	{Value: ProblemReportServerV1{}},
	{Value: ProblemReportRedactionV1{}},
	{Value: ProblemReportV1{}, Family: "problem-report-v1"},
	{Value: ProblemReportSummaryV1{}},
	{Value: ListProblemReportsResponseV1{}, Family: "list-problem-reports-response-v1"},
	{Value: UpdateProblemReportRequestV1{}, Family: "update-problem-report-request-v1"},

	// Message reactions (issue #953, docs/design/message-reactions.md §5):
	// a message's summary, which the events socket also sends, the batch
	// read, and one page of the principals holding one emoji.
	{Value: ReactionCountV1{}},
	{Value: ReactionSummaryV1{}, Family: "reaction-summary-v1", Schema: ReactionSummarySchemaV1},
	{Value: ReactionSummariesResponseV1{}, Family: "reaction-summaries-response-v1"},
	{Value: ReactionV1{}, Family: "reaction-v1"},
	{Value: ListReactorsResponseV1{}, Family: "list-reactors-response-v1"},

	// Roles and permissions (ADR-014, issue #1012,
	// docs/design/admin-roles.md §8): the document an operator reads and
	// the owner changes, a cell patch or a preset, and the content-free
	// signal that says to re-read whoami. The caller's own permissions are
	// in WhoAmIResponseV1.
	{Value: PermissionRowV1{}},
	{Value: PermissionPresetV1{}},
	{Value: IssuedByOperatorV1{}},
	{Value: PermissionsResponseV1{}, Family: "permissions-response-v1"},
	{Value: PermissionChangeV1{}},
	{Value: PutPermissionsRequestV1{}, Family: "put-permissions-request-v1"},
	{Value: PermissionsChangedV1{}, Family: "permissions-changed-v1", Schema: PermissionsChangedSchemaV1},

	// Preview as member (issue #1007, docs/design/permission-preview.md
	// §6.1): the entry read's answer, the previewed member's standing and
	// effective permissions, which the page feeds to the same controls it
	// keys on whoami. The other preview reads answer existing shapes.
	{Value: PreviewResponseV1{}, Family: "preview-response-v1"},
	// What an operator issued to others, on disable and on the owner's
	// revoke-all of it (issue #1024, ADR-014 O13); the demotion answer is
	// RoleChangeResponseV1 above, with the same optional list.
	{Value: DisablePrincipalResponseV1{}, Family: "disable-principal-response-v1"},
	{Value: RevokeIssuedResponseV1{}, Family: "revoke-issued-response-v1"},

	// The community model (ADR-013, issue #978,
	// docs/design/community-model.md §3): channels a member creates and
	// operators set, the channel directory and self-join, member links (v2
	// invites), the member directory and its signal, a channel's authors,
	// and the per-person voice switch. ChannelV0, ChannelDocV1,
	// PrincipalAdminV1 and the joining settings above carry the additive
	// fields.
	{Value: CreateChannelRequestV1{}, Family: "create-channel-request-v1"},
	{Value: CreateChannelResponseV1{}, Family: "create-channel-response-v1"},
	{Value: ChannelListingV1{}, Family: "channel-listing-v1"},
	{Value: ChannelDirectoryResponseV1{}, Family: "channel-directory-response-v1"},
	{Value: JoinChannelResponseV1{}, Family: "join-channel-response-v1"},
	{Value: ChannelSettingsRequestV1{}, Family: "channel-settings-request-v1"},
	{Value: ChannelSettingsResponseV1{}, Family: "channel-settings-response-v1"},
	{Value: InviteV2{}, Family: "invite-v2"},
	{Value: CreateInviteRequestV2{}, Family: "create-invite-request-v2"},
	{Value: CreateInviteResponseV2{}, Family: "create-invite-response-v2"},
	{Value: ListInvitesResponseV2{}, Family: "list-invites-response-v2"},
	{Value: InvitePreviewV2{}, Family: "invite-preview-v2"},
	{Value: ServerMemberV1{}, Family: "server-member-v1"},
	{Value: ServerMembersResponseV1{}, Family: "server-members-response-v1"},
	{Value: DirectoryChangedV1{}, Family: "directory-changed-v1", Schema: DirectoryChangedSchemaV1},
	{Value: ChannelAuthorV1{}, Family: "channel-author-v1"},
	{Value: ChannelAuthorsResponseV1{}, Family: "channel-authors-response-v1"},
	// The families are named for the route, PUT /v1/principals/{id}/voice-
	// permission, not "voice-…": the voice client's test claims every
	// voice-* fixture, and this switch is administration, not the client's.
	{Value: VoicePermissionRequestV1{}, Family: "principal-voice-permission-request-v1"},
	{Value: VoicePermissionResponseV1{}, Family: "principal-voice-permission-response-v1"},

	// AI administrators (issue #1326, docs/design/ai-administrators.md §3.4
	// to §3.6): the catalog as it applies to the caller, with an agent's
	// admin settings; what an action names; the submit body; and the payload
	// a gated action's approval carries, which the inbox renders as a card.
	// The result is a union by status (TSUnions). The manifest shapes that
	// carry the grant are not generated: no page reads a manifest yet.
	{Value: AgentAdminV1{}},
	{Value: AdminCatalogEntryV1{}},
	{Value: AdminCatalogResponseV1{}, Family: "admin-catalog-response-v1"},
	{Value: AdminTargetV1{}},
	{Value: SubmitAdminActionRequestV1{}, Family: "submit-admin-action-request-v1"},
	{Value: AdminActionV1{}, Family: "admin-action-v1"},
	{Value: ChannelGrant{}},
	{Value: RateLimit{}},
	{Value: AgentManifestV1{}, Family: "agent-manifest-v1", Schema: AgentManifestSchemaV1},
	{Value: PutAgentManifestRequestV1{}, Family: "put-agent-manifest-request-v1"},
	{Value: GetAgentManifestResponseV1{}, Family: "get-agent-manifest-response-v1"},

	// Phone push and notification preferences (issue #1712,
	// docs/design/mobile-notifications.md §9.1): the wake, a credential's
	// registrations and the server's transports, then the preferences and
	// the pending feed. OwnCredentialV1 above carries the additive push
	// field. The mention rule (Mentions) is not a type: the web client's own
	// matcher is checked against testdata/rules/mentions.json.
	{Value: PushWakeV1{}, Family: "push-wake-v1", Schema: PushWakeSchemaV1},
	{Value: PushRegistrationV1{}, Family: "push-registration-v1"},
	{Value: CreatePushRegistrationRequestV1{}, Family: "create-push-registration-request-v1"},
	{Value: UpdatePushRegistrationRequestV1{}, Family: "update-push-registration-request-v1"},
	{Value: ListOwnPushRegistrationsResponseV1{}, Family: "list-own-push-registrations-response-v1"},
	{Value: PushConfigV1{}, Family: "push-config-v1"},
	{Value: NotificationPreferencesV1{}, Family: "notification-preferences-v1"},
	{Value: PutNotificationPreferencesRequestV1{}, Family: "put-notification-preferences-request-v1"},
	{Value: NotificationSummaryV1{}, Family: "notification-summary-v1", Schema: NotificationSummarySchemaV1},
	{Value: ListPendingNotificationsResponseV1{}, Family: "list-pending-notifications-response-v1"},

	// The body of every refusal.
	{Value: Error{}, Name: "ErrorV0", Family: "error"},
}

// TSUnions is every struct in TSTypes' fields that is a union.
var TSUnions = []TSUnion{
	// A message's audience: a net, or a list of principals (ADR-005).
	{
		Value: Audience{},
		Tag:   "kind",
		Variants: map[string][]string{
			string(AudienceKindNet):        {"net_id"},
			string(AudienceKindPrincipals): {"principal_ids"},
		},
	},
	// Who an approval escalates to: a principal, or an ntfy topic, never
	// both (EscalationTarget.Validate).
	{
		Value: EscalationTarget{},
		Tag:   "kind",
		Variants: map[string][]string{
			string(EscalationTargetPrincipal): {"principal_id"},
			string(EscalationTargetNtfyTopic): {"topic"},
		},
	},
	// What a sign-in answers: a credential, or a challenge for the TOTP
	// step (LoginResponseV1.Validate).
	{
		Value: LoginResponseV1{},
		Tag:   "next",
		Variants: map[string][]string{
			string(LoginNextSignedIn): {"principal", "credential", "token"},
			string(LoginNextTOTP):     {"challenge", "challenge_expires_at"},
		},
	},
	// What the admin door answers: the dispatched route's answer, or the
	// approval it raised instead (AdminActionResultV1.Validate, issue #1326).
	{
		Value: AdminActionResultV1{},
		Tag:   "status",
		Variants: map[string][]string{
			string(AdminActionRan):             {"http_status", "result"},
			string(AdminActionPendingApproval): {"approval_id", "approval", "deadline"},
		},
	},
}

// TSVocabularyMode is what a vocabulary's generated validator does with a
// value it does not know (issue #1051). Every vocabulary is closed on the
// server, which refuses an unknown value; the modes differ in what a client
// built before a value shipped does with a document that carries it.
type TSVocabularyMode int

const (
	// TSStrict: the validator refuses the value, as Go's Valid() does, so the
	// whole document is refused and the page says so, rather than acting on
	// a state, kind or reason it cannot act on. The TypeScript type is the
	// union of the known values. Adding a value to a strict vocabulary is a
	// change an older client refuses: the type's own contract decides whether
	// that is an addition or a new version.
	TSStrict TSVocabularyMode = iota
	// TSTolerant: the validator keeps the value, so a bundle built before it
	// shipped still reads the document, and the page shows the value by its
	// name. Only for a type whose own contract says that a client does so,
	// and never for a union's tag, which picks a variant. The TypeScript
	// type is the known values' union or any other string, and the generated
	// Known<Name> is the known values alone, for a page's labels.
	TSTolerant
)

// TSVocabularyModes classifies every vocabulary in Vocabularies, with the
// reason, so a value added to a tolerant one is an addition an older client
// reads (ADR-013 §3.3, ADR-014 decision 7), and one added to a strict one is
// refused by every older client, as Go refuses it. cmd/schema-ts refuses a
// vocabulary that is not here; ts_test.go fails on one that is here and not
// in Vocabularies. A new vocabulary is strict unless its type's contract
// says a client shows a value it does not know by its name.
var TSVocabularyModes = map[string]TSVocabularyMode{
	// Tolerant: the Go type's contract says a client shows a value it does
	// not know by its name, so the vocabulary grows without a version bump.
	// Roles and permissions (permission.go, ADR-014 decision 7): a twelfth
	// permission, a guest column, a later preset, warning or issued kind.
	"Permission":         TSTolerant,
	"PermissionRole":     TSTolerant,
	"PermissionPreset":   TSTolerant,
	"PermissionsWarning": TSTolerant,
	"IssuedKind":         TSTolerant,
	// Joining and the community model (guest.go, invite_v2.go, ADR-013):
	// a later warning, link kind or directory scope.
	"JoinWarning":    TSTolerant,
	"InviteKindV2":   TSTolerant,
	"DirectoryScope": TSTolerant,
	// AI administrators (admin_action.go, issue #1326): a later catalog row,
	// and a later kind of thing one acts on, which the page shows by name.
	"AdminAction":     TSTolerant,
	"AdminTargetKind": TSTolerant,
	// The pending feed (notification.go, issue #1712): a later reason for a
	// message to be in it. A phone still renders the summary.
	"NotificationKind": TSTolerant,

	// Strict: a union's tag. tagged() picks the variant by it and has no
	// shape for a value it does not know.
	"AudienceKind":         TSStrict,
	"EscalationTargetKind": TSStrict,
	"LoginNext":            TSStrict,
	"AdminActionStatus":    TSStrict,
	// Strict: the page switches on an action's gate class to say whether a
	// person will be asked.
	"AdminGate": TSStrict,
	// Strict: the page switches on it to pick the preview banner (design
	// §8); a simulation it does not know it cannot label honestly, and must
	// never show as a person's view.
	"PreviewSimulation": TSStrict,

	// Strict: a state, kind, mode, role, outcome or reason the page switches
	// on to decide what to show or offer. A value it does not know it cannot
	// act on, so the document is refused and the page says so, rather than
	// read as some known value. PrincipalRole in particular: ADR-010 refused
	// a third value because every client switches on it.
	"ActivityKind":          TSStrict,
	"ApprovalState":         TSStrict,
	"ChannelVisibility":     TSStrict,
	"CredentialScope":       TSStrict,
	"GuestEndReason":        TSStrict,
	"GuestOrigin":           TSStrict,
	"GuestState":            TSStrict,
	"HealthVoice":           TSStrict,
	"IdentityClass":         TSStrict,
	"InterventionKind":      TSStrict,
	"InterventionState":     TSStrict,
	"InviteJoinMode":        TSStrict,
	"InviteKind":            TSStrict,
	"InviteeDepth":          TSStrict,
	"NetRole":               TSStrict,
	"OpenJoiningMode":       TSStrict,
	"OptionKind":            TSStrict,
	"Outcome":               TSStrict,
	"PrincipalKind":         TSStrict,
	"PrincipalRole":         TSStrict,
	"PrincipalState":        TSStrict,
	"ProblemReportOften":    TSStrict,
	"ProblemReportState":    TSStrict,
	"RunState":              TSStrict,
	"ServerMode":            TSStrict,
	"SessionState":          TSStrict,
	"SessionStatus":         TSStrict,
	"SignInLinkPurpose":     TSStrict,
	"SocketCloseReason":     TSStrict,
	"VoiceCompanionPublish": TSStrict,
	// Strict: a consent the desktop writes (issue #898); it sends only a
	// scope it knows, and a value it does not know it cannot have asked for.
	"VoiceShareAudioScope": TSStrict,
	// A new share end reason is a new version of presence, by its contract
	// (voice.go; webcam-video.md §10 keeps the camera's reasons the same way).
	"VoiceShareEndReason":  TSStrict,
	"VoiceCameraEndReason": TSStrict,
	// Strict: the sharer's declared resolution (issue #1483, design §3.2) is
	// the picker's closed vocabulary; a new value is a new version of
	// presence, as a new end reason is, and the page sends only what its
	// picker offers.
	"VoiceShareResolution": TSStrict,
	"VoiceTransmitState":   TSStrict,

	// Strict: written by the page itself in a problem report (issue #446);
	// the server validates what it is sent, and the page sends only what it
	// knows.
	"DiagnosticEventKind":      TSStrict,
	"DiagnosticSocket":         TSStrict,
	"DiagnosticVoiceStateName": TSStrict,

	// Strict: the managed voice-helper control protocol (issue #948). The
	// desktop owner and the conch-voice it bundles ship together, the
	// protocol is versioned by its schema name, and a value either side does
	// not know is a protocol error, not something to show.
	"VoiceControlBlock":       TSStrict,
	"VoiceControlCapture":     TSStrict,
	"VoiceControlCommandKind": TSStrict,
	"VoiceControlCredential":  TSStrict,
	"VoiceControlErrorCode":   TSStrict,
	"VoiceControlEventKind":   TSStrict,
	"VoiceControlPipeKind":    TSStrict,
	"VoiceControlPlayback":    TSStrict,
	"VoiceControlPhase":       TSStrict,
	"VoiceControlPTT":         TSStrict,
	"VoiceControlPTTProblem":  TSStrict,
	"VoiceControlReason":      TSStrict,
	"VoiceControlTalkMode":    TSStrict,
	"VoiceDevicesConfig":      TSStrict,
	"VoiceDevicesPipeWire":    TSStrict,

	// Phone push and notification preferences (issue #1712). Strict: the
	// policy and the platform, which a client writes itself, and the end
	// reason, an audit detail that no generated type carries. Tolerant: the
	// transport and the provider, which a client also reads in
	// PushConfigV1, so that a later server offering a new one (the relay,
	// #1716) costs an older client only that one entry, not the whole
	// config.
	"NotificationPolicy":        TSStrict,
	"PushPlatform":              TSStrict,
	"PushProvider":              TSTolerant,
	"PushRegistrationEndReason": TSStrict,
	"PushTransport":             TSTolerant,

	// Strict: agent manifests and the Leviathan payload, which no generated
	// client reads today. Strict is the default; a vocabulary becomes
	// tolerant when a client reads it and its contract says so.
	"AgentTier":         TSStrict,
	"Capability":        TSStrict,
	"ChannelPermission": TSStrict,
	"TradeSide":         TSStrict,
}
