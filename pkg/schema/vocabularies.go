package schema

// Vocabularies is every closed vocabulary in this package: each string type
// whose values are a fixed list, keyed by the type's Go name, with its values
// in the order their constants are declared. cmd/schema-ts reads it to write
// the TypeScript union and validator for each one a generated type uses
// (issue #348), so the web client refuses exactly the values Go refuses.
//
// It is the one place the values are listed outside the type's own file.
// vocabularies_test.go reads this package's source and fails when a type's
// constants, its Valid() switch or its …Names() string disagree with the list
// here, so a value added to Go and not here fails go test.
var Vocabularies = map[string][]string{
	"ActivityKind": values(
		ActivityKindRunStarted, ActivityKindCommandStarted, ActivityKindCommandFinished,
		ActivityKindReplyPosted, ActivityKindApprovalRequested, ActivityKindApprovalResolved,
		ActivityKindInterventionReceived, ActivityKindRunEnded,
	),
	"AgentTier":       values(AgentTierC, AgentTierA, AgentTierH),
	"ApprovalState":   values(ApprovalStatePending, ApprovalStateEscalated, ApprovalStateResolved, ApprovalStateExpired),
	"AudienceKind":    values(AudienceKindNet, AudienceKindPrincipals),
	"CredentialScope": values(CredentialScopeFull, CredentialScopeSetup),
	"Capability": values(
		CapabilityMessagesRead, CapabilityMessagesPost,
		CapabilityApprovalsRequest, CapabilityApprovalsAwait, CapabilityApprovalsCheck,
		CapabilitySessionsManage, CapabilityRunsManage, CapabilityAdminAct,
		CapabilityFilesUpload,
	),
	"ChannelPermission": values(
		ChannelPermissionRead, ChannelPermissionPost, ChannelPermissionPostNet,
		ChannelPermissionWhisper, ChannelPermissionWhisperAgent, ChannelPermissionAttach,
	),
	"DiagnosticEventKind": values(
		DiagnosticSocketOpen, DiagnosticSocketClose, DiagnosticReconnect,
		DiagnosticHTTPFailure, DiagnosticVoiceState, DiagnosticDeviceError,
	),
	"DiagnosticSocket": values(DiagnosticSocketMessages, DiagnosticSocketEvents, DiagnosticSocketVoice),
	"DiagnosticVoiceStateName": values(
		DiagnosticVoiceIdle, DiagnosticVoiceConnecting, DiagnosticVoiceConnected,
		DiagnosticVoiceReconnecting, DiagnosticVoiceUnavailable, DiagnosticVoiceStopped,
	),
	"EscalationTargetKind": values(EscalationTargetPrincipal, EscalationTargetNtfyTopic),
	"GuestEndReason":       values(GuestEndVisit, GuestEndIdle, GuestEndExpired, GuestEndOperator, GuestEndRestore),
	"GuestOrigin":          values(GuestOriginAddress, GuestOriginInvite),
	"GuestState":           values(GuestStateActive, GuestStateEnded, GuestStateConverted),
	"HealthVoice":          values(HealthVoiceOK, HealthVoiceUnavailable, HealthVoiceNotConfigured),
	"IdentityClass":        values(IdentityGuest, IdentityMember, IdentityOperator, IdentityAgent),
	"InviteJoinMode":       values(InviteJoinAccount, InviteJoinGuest, InviteJoinBoth),
	"InviteKind":           values(InviteKindPersonal, InviteKindOpen),
	"InviteeDepth":         values(InviteeDepthDirect, InviteeDepthAll),
	"InterventionKind":     values(InterventionKindMessage, InterventionKindCancel, InterventionKindPause, InterventionKindResume),
	"InterventionState": values(
		InterventionStateRequested, InterventionStateAcknowledged, InterventionStateApplied, InterventionStateFailed,
	),
	"JoinWarning": values(
		JoinWarningLiveGuestsRemain, JoinWarningTrustedProxyUnset, JoinWarningOneAddress, JoinWarningReservedNamesInUse,
		JoinWarningServerNameInUse, JoinWarningNoDefaultChannel,
	),
	"LoginNext":       values(LoginNextSignedIn, LoginNextTOTP),
	"NetRole":         values(NetRoleMember, NetRoleMonitor),
	"OpenJoiningMode": values(OpenJoiningOff, OpenJoiningGuest, OpenJoiningAccount, OpenJoiningBoth),
	"OptionKind":      values(OptionKindApprove, OptionKindReject, OptionKindCustom),
	"Outcome":         values(OutcomeApproved, OutcomeRejected, OutcomeCustom, OutcomeExpired),
	"PrincipalKind":   values(PrincipalHuman, PrincipalAgent),
	"PrincipalRole":   values(RoleOperator, RoleMember),
	"PrincipalState":  values(PrincipalStateActive, PrincipalStateDisabled, PrincipalStateHidden),
	"ProblemReportOften": values(
		ProblemReportOftenEveryTime, ProblemReportOftenSometimes, ProblemReportOftenOnce,
	),
	"ProblemReportState": values(ProblemReportStateOpen, ProblemReportStateFiled),
	"SignInLinkPurpose": values(
		SignInLinkPurposeSignIn, SignInLinkPurposeDevice, SignInLinkPurposeReset,
		SignInLinkPurposeEnrollment, SignInLinkPurposeConversion,
	),
	"SocketCloseReason": values(
		SocketCloseClient, SocketCloseServerShutdown, SocketCloseKeepaliveTimeout, SocketCloseRevoked,
		SocketCloseRemoved, SocketCloseVoiceRemoved, SocketCloseSlowConsumer, SocketCloseSessionEnded,
		SocketCloseError,
	),
	"RunState": values(
		RunStateQueued, RunStateStarting, RunStateRunning, RunStateBlocked,
		RunStateWaiting, RunStateCompleted, RunStateFailed, RunStateCancelled,
	),
	"ServerMode":   values(ServerModeTemporary),
	"SessionState": values(SessionStateOnline, SessionStateStale, SessionStateOffline),
	"SessionStatus": values(
		SessionStatusOnline, SessionStatusIdle, SessionStatusRunning, SessionStatusBlocked,
		SessionStatusWaiting, SessionStatusFailed, SessionStatusStale, SessionStatusOffline,
	),
	"TradeSide": values(TradeSideBuy, TradeSideSell),
	"VoiceControlBlock": values(
		VoiceControlBlockNotConnected, VoiceControlBlockNoPublishGrant, VoiceControlBlockNoMicrophone,
		VoiceControlBlockDeafened, VoiceControlBlockMuted, VoiceControlBlockKeyDeviceLost,
		VoiceControlBlockMaxTransmit,
	),
	"VoiceControlCapture": values(VoiceControlCaptureOff, VoiceControlCaptureOn, VoiceControlCaptureUnavailable),
	"VoiceControlCommandKind": values(
		VoiceControlCommandStart, VoiceControlCommandSetMute, VoiceControlCommandSetDeafen,
		VoiceControlCommandGetState, VoiceControlCommandLeave,
		VoiceControlCommandSetDevices, VoiceControlCommandSetTalkMode,
	),
	"VoiceControlCredential": values(VoiceControlCredentialStored, VoiceControlCredentialEnv),
	"VoiceControlErrorCode": values(
		VoiceControlErrorTooLong, VoiceControlErrorMalformed, VoiceControlErrorUnknownSchema,
		VoiceControlErrorUnknownKind, VoiceControlErrorInvalid, VoiceControlErrorIDNotIncreasing,
		VoiceControlErrorNotStarted, VoiceControlErrorAlreadyStarted, VoiceControlErrorStopping,
	),
	"VoiceControlEventKind": values(
		VoiceControlEventHello, VoiceControlEventAck, VoiceControlEventState, VoiceControlEventProtocolError,
	),
	"VoiceControlPipeKind": values(VoiceControlPipeKindAttached),
	"VoiceControlPlayback": values(VoiceControlPlaybackOff, VoiceControlPlaybackOn, VoiceControlPlaybackUnavailable),
	"VoiceControlPhase": values(
		VoiceControlPhaseStarting, VoiceControlPhaseConnecting, VoiceControlPhaseConnected,
		VoiceControlPhaseReconnecting, VoiceControlPhaseStopping, VoiceControlPhaseStopped,
		VoiceControlPhaseFailed,
	),
	"VoiceControlPTT": values(
		VoiceControlPTTUnknown, VoiceControlPTTReady, VoiceControlPTTNotConfigured, VoiceControlPTTUnavailable, VoiceControlPTTLost,
	),
	"VoiceControlPTTProblem": values(
		VoiceControlPTTProblemCannotOpen, VoiceControlPTTProblemRefused, VoiceControlPTTProblemReadFailed,
		VoiceControlPTTProblemEnded, VoiceControlPTTProblemPartialRecord,
	),
	"VoiceCompanionPublish": values(VoiceCompanionPublishScreen, VoiceCompanionPublishNone),
	"VoiceShareAudioScope":  values(VoiceShareAudioScopeApplication, VoiceShareAudioScopeSystemExcludingCall),
	"VoiceControlReason": values(
		VoiceControlReasonRoomDeleted, VoiceControlReasonParticipantRemoved, VoiceControlReasonConnectionLost,
		VoiceControlReasonReconnectTimedOut, VoiceControlReasonConnectFailed, VoiceControlReasonServerUnreachable,
		VoiceControlReasonVoiceUnavailable, VoiceControlReasonNoSession, VoiceControlReasonServerShutdown,
		VoiceControlReasonLeft, VoiceControlReasonOwnerLost, VoiceControlReasonSignal,
		VoiceControlReasonNotSignedIn, VoiceControlReasonIdentityMismatch, VoiceControlReasonBadEnvironment,
		VoiceControlReasonBadConfig, VoiceControlReasonVoiceRequiresAuth, VoiceControlReasonNotAPerson,
		VoiceControlReasonVoiceNotAllowed, VoiceControlReasonNotAMember, VoiceControlReasonVoiceNotConfigured,
		VoiceControlReasonDuplicateIdentity, VoiceControlReasonVoiceRemoved, VoiceControlReasonAudioUnavailable,
		VoiceControlReasonStartTimeout, VoiceControlReasonProtocolError, VoiceControlReasonInternalError,
	),
	"VoiceControlTalkMode": values(VoiceControlTalkModePushToTalk, VoiceControlTalkModeOpen),
	"VoiceDevicesConfig": values(
		VoiceDevicesConfigOK, VoiceDevicesConfigMissing, VoiceDevicesConfigRefused, VoiceDevicesConfigNone,
	),
	"VoiceDevicesPipeWire": values(VoiceDevicesPipeWireOK, VoiceDevicesPipeWireUnavailable),
	"VoiceShareEndReason": values(
		VoiceShareEndSelf, VoiceShareEndOperator, VoiceShareEndLeft,
		VoiceShareEndAccessLost, VoiceShareEndRotated, VoiceShareEndIdle,
	),
	"VoiceCameraEndReason": values(
		VoiceCameraEndSelf, VoiceCameraEndOperator, VoiceCameraEndLeft,
		VoiceCameraEndAccessLost, VoiceCameraEndRotated, VoiceCameraEndIdle,
	),
	// The sharer's declared resolution (issue #1483): the picker's three
	// (#1488, web/src/voice/share.ts ShareResolution), exactly.
	"VoiceShareResolution": values(VoiceShareResolution720p, VoiceShareResolution1080p, VoiceShareResolutionSource),
	"VoiceTransmitState":   values(VoiceTransmitStateStarted, VoiceTransmitStateStopped),

	// Roles and permissions (ADR-014, issue #1012). Permission is in catalog
	// order, the order of Permissions. Row 12, use_camera, is ADR-015's.
	"Permission": values(
		PermCreatePublicChannels, PermCreatePrivateChannels, PermMakeMemberLinks, PermSeeMemberDirectory,
		PermUseVoice, PermShareScreen, PermManageMembers, PermActAsMembers, PermManageChannels,
		PermChangeServerSettings, PermReadAuditLog, PermUseCamera,
	),
	"PermissionRole":     values(PermissionRoleMember, PermissionRoleOperator),
	"PermissionPreset":   values(PresetPersonal, PresetGroup),
	"PermissionsWarning": values(PermissionsWarningDefaultChannelExposesMembers, PermissionsWarningOpenSignupOn),
	"IssuedKind":         values(IssuedCredential, IssuedHook),

	// Preview as member (issue #1007, docs/design/permission-preview.md §8):
	// what kind of nobody a preview answer is the view of.
	"PreviewSimulation": values(PreviewSimulationRole),

	// The community model (ADR-013, issue #978). InviteKindV2 and
	// DirectoryScope are closed and client-tolerant.
	"ChannelVisibility": values(ChannelPublic, ChannelPrivate),
	"InviteKindV2":      values(InviteKindMember),
	"DirectoryScope":    values(DirectoryScopeEveryone, DirectoryScopeSharedChannels),

	// AI administrators (issue #1326, docs/design/ai-administrators.md §3.3
	// to §3.6). AdminAction is in catalog order, the order of AdminActions.
	"AdminAction": values(
		AdminActionListMembers, AdminActionCreateMember, AdminActionRenameMember,
		AdminActionSetVoicePermission, AdminActionSetInvitePermission,
		AdminActionDisableMember, AdminActionEnableMember, AdminActionHideMember,
		AdminActionListCredentials, AdminActionRevokeCredential, AdminActionRevokeAllCredentials,
		AdminActionListHooks, AdminActionRevokeHook,
		AdminActionListChannels, AdminActionCreateChannel, AdminActionRenameChannel,
		AdminActionSetChannelDefault, AdminActionSetChannelVisibility,
		AdminActionArchiveChannel, AdminActionRestoreChannel,
		AdminActionListChannelMembers, AdminActionAddChannelMember,
		AdminActionRemoveChannelMember, AdminActionSetChannelMemberRights,
		AdminActionCreateMemberLink, AdminActionListMemberLinks, AdminActionCloseMemberLink,
		AdminActionListLinkJoiners, AdminActionDisableLinkJoiners, AdminActionDisableInvitees,
		AdminActionPauseInvites, AdminActionResumeInvites,
		AdminActionGetJoinSettings, AdminActionSetJoinSettings,
		AdminActionGetPermissions, AdminActionSetPermissions,
		AdminActionListReports, AdminActionGetReport, AdminActionUpdateReport,
		AdminActionExportAudit,
		AdminActionListChannelBans, AdminActionBanChannelMember, AdminActionUnbanChannelMember,
		AdminActionSetChannelTopic,
	),
	"AdminGate": values(AdminGateDirect, AdminGateApproval),
	"AdminTargetKind": values(
		AdminTargetPrincipal, AdminTargetChannel, AdminTargetCredential,
		AdminTargetHook, AdminTargetInvite, AdminTargetReport,
	),
	"AdminActionStatus": values(AdminActionRan, AdminActionPendingApproval),

	// Phone push and notification preferences (issue #1712,
	// docs/design/mobile-notifications.md §9.1).
	"NotificationKind":   values(NotificationKindMessage, NotificationKindMention, NotificationKindWhisper),
	"NotificationPolicy": values(NotificationPolicyOff, NotificationPolicyMentions, NotificationPolicyAll),
	"PushPlatform":       values(PushPlatformIOS, PushPlatformAndroid, PushPlatformWeb),
	"PushProvider":       values(PushProviderAPNs, PushProviderFCM),
	"PushRegistrationEndReason": values(
		PushRegistrationEndSelf, PushRegistrationEndCredentialRevoked, PushRegistrationEndCredentialExpired,
		PushRegistrationEndProviderRejected, PushRegistrationEndStale,
	),
	"PushTransport": values(PushTransportWebPush, PushTransportRelay),
}

// values lists a vocabulary by its constants, so an entry above cannot hold a
// value that is not one, nor mix two types' constants.
func values[T ~string](vs ...T) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}
