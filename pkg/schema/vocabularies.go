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
		CapabilitySessionsManage, CapabilityRunsManage,
	),
	"ChannelPermission": values(
		ChannelPermissionRead, ChannelPermissionPost, ChannelPermissionPostNet,
		ChannelPermissionWhisper, ChannelPermissionWhisperAgent,
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
		SocketCloseRemoved, SocketCloseSlowConsumer, SocketCloseSessionEnded, SocketCloseError,
	),
	"RunState": values(
		RunStateQueued, RunStateStarting, RunStateRunning, RunStateBlocked,
		RunStateWaiting, RunStateCompleted, RunStateFailed, RunStateCancelled,
	),
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
	"VoiceControlReason": values(
		VoiceControlReasonRoomDeleted, VoiceControlReasonParticipantRemoved, VoiceControlReasonConnectionLost,
		VoiceControlReasonReconnectTimedOut, VoiceControlReasonConnectFailed, VoiceControlReasonServerUnreachable,
		VoiceControlReasonVoiceUnavailable, VoiceControlReasonNoSession, VoiceControlReasonServerShutdown,
		VoiceControlReasonLeft, VoiceControlReasonOwnerLost, VoiceControlReasonSignal,
		VoiceControlReasonNotSignedIn, VoiceControlReasonIdentityMismatch, VoiceControlReasonBadEnvironment,
		VoiceControlReasonBadConfig, VoiceControlReasonVoiceRequiresAuth, VoiceControlReasonNotAPerson,
		VoiceControlReasonVoiceNotAllowed, VoiceControlReasonNotAMember, VoiceControlReasonVoiceNotConfigured,
		VoiceControlReasonDuplicateIdentity, VoiceControlReasonAudioUnavailable, VoiceControlReasonStartTimeout,
		VoiceControlReasonProtocolError, VoiceControlReasonInternalError,
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
	"VoiceTransmitState": values(VoiceTransmitStateStarted, VoiceTransmitStateStopped),
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
