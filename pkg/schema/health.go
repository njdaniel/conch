package schema

import (
	"fmt"
	"strings"
)

// Health is the response body of GET /healthz. It reports the running server
// version and whether the backing store is reachable. It is an operational
// wire shape, not a versioned payload schema, so it carries no version field
// of its own and is not pinned by a golden fixture.
type Health struct {
	// Status is "ok" when every dependency of the core is healthy, otherwise
	// "degraded".
	Status string `json:"status"`
	// Version is the conchd build version.
	Version string `json:"version"`
	// DB is "ok" when the store is reachable, otherwise "degraded". Failure
	// detail stays in the server log; it is never put on the wire.
	DB string `json:"db"`
	// Voice is the state of the optional voice integration (issue #446). It
	// never makes Status degraded or the answer 503: voice is optional, and
	// messaging, approvals and audit work without it (ADR-002), so a server
	// whose LiveKit is down is still a healthy server.
	Voice HealthVoice `json:"voice"`
	// ServerMode is "temporary" when the server runs in temporary mode
	// (ADR-011 item 2.3, issue #865), and absent in normal mode, which is
	// also what a server that predates the field says. It is deliberately
	// public: the sign-in page reads it here before anyone signs in, as it
	// reads Version.
	ServerMode ServerMode `json:"server_mode,omitempty"`
}

// Validate reports whether h is a well-formed health response: server_mode,
// when present, is a recognized mode.
func (h Health) Validate() error {
	return validateServerMode(h.ServerMode)
}

// ServerMode is the mode a server runs in, as /healthz and /v1/whoami tell
// everyone (ADR-011 item 1). The wire carries only "temporary": normal mode
// omits the field, so there is no constant for it, and CONCHD_MODE=normal is
// the server's configuration, not a wire value.
type ServerMode string

const (
	// ServerModeTemporary: a throwaway server with no off-site backup, whose
	// operators may sign in with a password alone. It may be removed at any
	// time.
	ServerModeTemporary ServerMode = "temporary"
)

// Valid reports whether m is a recognized server mode. The empty value is
// not one: it is the field's absence, which Validate on the carrying struct
// accepts.
func (m ServerMode) Valid() bool {
	switch m {
	case ServerModeTemporary:
		return true
	default:
		return false
	}
}

// validateServerMode accepts an absent server_mode and any recognized value,
// and refuses everything else, "normal" included: normal mode is said by
// omitting the field.
func validateServerMode(m ServerMode) error {
	if m != "" && !m.Valid() {
		return fmt.Errorf("schema: server_mode must be absent or one of: %s", strings.Join(Vocabularies["ServerMode"], ", "))
	}
	return nil
}

// Health status values.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
)

// HealthVoice is what /healthz says about voice: the presence poller's view of
// LiveKit.
type HealthVoice string

const (
	// HealthVoiceOK: LiveKit is configured and answered the last time it was
	// asked.
	HealthVoiceOK HealthVoice = "ok"
	// HealthVoiceUnavailable: LiveKit is configured but is not answering, or
	// has not answered yet since conchd started.
	HealthVoiceUnavailable HealthVoice = "unavailable"
	// HealthVoiceNotConfigured: this conchd has no voice.
	HealthVoiceNotConfigured HealthVoice = "not_configured"
)

// Valid reports whether v is a recognized voice state.
func (v HealthVoice) Valid() bool {
	switch v {
	case HealthVoiceOK, HealthVoiceUnavailable, HealthVoiceNotConfigured:
		return true
	default:
		return false
	}
}
