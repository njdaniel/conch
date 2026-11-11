package voicehelper

import "github.com/njdaniel/conch/pkg/schema"

// The commands the owner writes, in the version of the session (design note
// §20.2). v2's five shared commands carry v1's fields, so each is built from
// the same arguments; version "" is v1, the version of a session whose hello
// has not been answered.

func isV2(version string) bool { return version == schema.VoiceControlSchemaV2 }

func startMsg(version string, id int64, p Profile, channel string, muted, deafened bool) schema.VoiceControlMessage {
	if isV2(version) {
		return schema.VoiceControlStartV2{
			Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandStart, ID: id,
			Server: p.Server, Channel: channel, PrincipalID: p.PrincipalID,
			Credential: p.Credential, Muted: muted, Deafened: deafened,
		}
	}
	return schema.VoiceControlStartV1{
		Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandStart, ID: id,
		Server: p.Server, Channel: channel, PrincipalID: p.PrincipalID,
		Credential: p.Credential, Muted: muted, Deafened: deafened,
	}
}

func setMuteMsg(version string, id int64, on bool) schema.VoiceControlMessage {
	if isV2(version) {
		return schema.VoiceControlSetMuteV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandSetMute, ID: id, Muted: on}
	}
	return schema.VoiceControlSetMuteV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandSetMute, ID: id, Muted: on}
}

func setDeafenMsg(version string, id int64, on bool) schema.VoiceControlMessage {
	if isV2(version) {
		return schema.VoiceControlSetDeafenV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandSetDeafen, ID: id, Deafened: on}
	}
	return schema.VoiceControlSetDeafenV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandSetDeafen, ID: id, Deafened: on}
}

func getState(version string, id int64) schema.VoiceControlMessage {
	if isV2(version) {
		return schema.VoiceControlGetStateV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandGetState, ID: id}
	}
	return schema.VoiceControlGetStateV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandGetState, ID: id}
}

func leaveMsg(version string, id int64) schema.VoiceControlMessage {
	if isV2(version) {
		return schema.VoiceControlLeaveV2{Schema: schema.VoiceControlSchemaV2, Kind: schema.VoiceControlCommandLeave, ID: id}
	}
	return schema.VoiceControlLeaveV1{Schema: schema.VoiceControlSchemaV1, Kind: schema.VoiceControlCommandLeave, ID: id}
}
