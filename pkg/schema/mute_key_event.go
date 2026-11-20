package schema

// MuteKeyEventSchemaV1 is the desktop mute-key watcher's private stdout protocol.
const MuteKeyEventSchemaV1 = "conch.mute_key_event.v1"

// MuteKeyEventV1 reports one press of the configured mute binding.
// Press is always true. The watcher sends no releases or physical key codes.
type MuteKeyEventV1 struct {
	Schema string `json:"schema"`
	Press  bool   `json:"press"`
}
