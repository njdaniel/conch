package schema

// The per-person voice switch (ADR-013 decision 4, issue #978,
// docs/design/community-model.md §3.6): voice is on for every member by
// default, and an operator holding manage_members (ADR-014) switches it off
// or on for any person at any time, with immediate effect on live media.
// Under ADR-014 the switch is a brake under the role's use_voice cell:
// switching a person on grants nothing while the cell is off. These are
// additive REST bodies with no schema-name field, versioned by the V1 type
// suffix.

// ErrorCodeNotAPerson (HTTP 409): the target of PUT
// /v1/principals/{id}/voice-permission is an agent, which never holds voice.
// A hidden or unknown id is 404 principal_not_found; an owner target for a
// non-owner operator is the owner-protection answer of ADR-010.
const ErrorCodeNotAPerson = "not_a_person"

// VoicePermissionRequestV1 is the body of PUT
// /v1/principals/{id}/voice-permission (manage_members, recent
// authentication): whether the person may use voice and screen share.
type VoicePermissionRequestV1 struct {
	VoiceAllowed bool `json:"voice_allowed"`
}

// VoicePermissionResponseV1 is the state now and whether the request changed
// it (false on an idempotent repeat, which writes no audit row). Removals
// from voice rooms are not counted here: the immediate hook never fails the
// request and does not report back, and each removal is its own
// voice_participant_removed audit row. A join token minted before the
// change may still connect for its short lifetime; the next pass removes it.
type VoicePermissionResponseV1 struct {
	PrincipalID  int64 `json:"principal_id"`
	VoiceAllowed bool  `json:"voice_allowed"`
	Changed      bool  `json:"changed"`
}
