package schema

// The tables cmd/schema-ts reads to write the web client's wire types,
// web/src/schema/generated.ts, and the list of fixture families its test
// checks, generated.families.ts (issue #348). Everything else comes from the
// Go types themselves: field names from the json tags, and the values of a
// closed vocabulary from Vocabularies. Run `go generate ./pkg/schema` after
// changing a table or a type listed in one; scripts/schema-ts-check.sh fails
// while the committed files are stale.

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

// TSTypes is every struct the web client reads or sends, and the refusal
// body. A struct a listed one contains must be listed too, here or in
// TSUnions.
var TSTypes = []TSType{
	// The caller, and the channels it can see.
	{Value: WhoAmIResponseV1{}, Family: "whoami-response-v1"},
	{Value: ChannelV0{}, Family: "channel-v0"},
	{Value: ListChannelsResponse{}, Family: "list-channels-response"},
	{Value: ChannelMemberV1{}, Family: "channel-member-v1"},
	{Value: ListChannelMembersResponseV1{}, Family: "list-channel-members-response-v1"},
	{Value: ChannelMembersV1{}, Family: "channel-members-v1", Schema: ChannelMembersSchemaV1},

	// Messages, and the ticket that opens the socket that streams them.
	{Value: Payload{}},
	{Value: MessageV2{}, Family: "message-v2", Schema: MessageSchemaV2},
	{Value: PostMessageRequestV2{}, Family: "post-message-request-v2"},
	{Value: PostMessageResponseV2{}, Family: "post-message-response-v2"},
	{Value: ListMessagesResponseV2{}, Family: "list-messages-response-v2"},
	{Value: WSTicketV1{}, Family: "ws-ticket-v1"},

	// An approval, as a message carries it in its payload.
	{Value: Option{}, Family: "approval-option-v1"},
	{Value: ApprovalV1{}, Family: "conch-approval-v1"},

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

	// Browser voice (issue #442): the session that admits the page to a room,
	// the presence snapshot its socket streams, and the transmit report.
	{Value: VoiceRoomGrant{}},
	{Value: VoiceSessionResponseV1{}, Family: "voice-session-response-v1"},
	{Value: VoiceParticipant{}},
	{Value: VoicePresenceRoom{}},
	// Why the reader's own share ended (issue #487), in their presence alone.
	{Value: VoiceShareEnded{}},
	{Value: VoicePresenceV1{}, Family: "voice-presence-v1", Schema: VoicePresenceSchemaV1},
	{Value: VoiceTransmitReportV1{}, Family: "voice-transmit-report-v1"},
	// Screen share (issue #443): the claim the page is given when the room's
	// share is its own. Presence says who shares (VoiceParticipant above).
	{Value: VoiceShareV1{}, Family: "voice-share-v1"},
	// Voice occupancy (issue #596): how many are in each channel's voice,
	// streamed on a socket of its own so the channel list can show it.
	{Value: VoiceChannelOccupancy{}},
	{Value: VoiceOccupancyV1{}, Family: "voice-occupancy-v1", Schema: VoiceOccupancySchemaV1},
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

	// The server's health, which carries its version (the sign-in page shows
	// it). Not a versioned payload, so it has no fixture family.
	{Value: Health{}},

	// Reporting a problem (issue #446): what the page sends, and the id it
	// gets back. Reading reports is for operators, in the CLI.
	{Value: ProblemReportClientV1{}},
	{Value: DiagnosticEventV1{}},
	{Value: CreateProblemReportRequestV1{}, Family: "create-problem-report-request-v1"},
	{Value: CreateProblemReportResponseV1{}, Family: "create-problem-report-response-v1"},

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
}
