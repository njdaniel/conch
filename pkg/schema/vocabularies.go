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
	"AgentTier":     values(AgentTierC, AgentTierA, AgentTierH),
	"ApprovalState": values(ApprovalStatePending, ApprovalStateEscalated, ApprovalStateResolved, ApprovalStateExpired),
	"AudienceKind":  values(AudienceKindNet, AudienceKindPrincipals),
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
	"HealthVoice":          values(HealthVoiceOK, HealthVoiceUnavailable, HealthVoiceNotConfigured),
	"InviteKind":           values(InviteKindPersonal, InviteKindOpen),
	"InviteeDepth":         values(InviteeDepthDirect, InviteeDepthAll),
	"InterventionKind":     values(InterventionKindMessage, InterventionKindCancel, InterventionKindPause, InterventionKindResume),
	"InterventionState": values(
		InterventionStateRequested, InterventionStateAcknowledged, InterventionStateApplied, InterventionStateFailed,
	),
	"NetRole":        values(NetRoleMember, NetRoleMonitor),
	"OptionKind":     values(OptionKindApprove, OptionKindReject, OptionKindCustom),
	"Outcome":        values(OutcomeApproved, OutcomeRejected, OutcomeCustom, OutcomeExpired),
	"PrincipalKind":  values(PrincipalHuman, PrincipalAgent),
	"PrincipalRole":  values(RoleOperator, RoleMember),
	"PrincipalState": values(PrincipalStateActive, PrincipalStateDisabled, PrincipalStateHidden),
	"ProblemReportOften": values(
		ProblemReportOftenEveryTime, ProblemReportOftenSometimes, ProblemReportOftenOnce,
	),
	"ProblemReportState": values(ProblemReportStateOpen, ProblemReportStateFiled),
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
	"TradeSide":          values(TradeSideBuy, TradeSideSell),
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
