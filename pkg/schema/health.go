package schema

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
