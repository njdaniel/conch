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
