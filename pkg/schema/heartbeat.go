package schema

import "fmt"

// The socket heartbeat (issue #428, private alpha A4). A browser answers a
// WebSocket ping on its own but never shows it to the page, so a page cannot
// tell a quiet socket from a dead one. A socket that was asked for heartbeats
// carries this tiny frame every HeartbeatIntervalSeconds, and a client that
// sees nothing for HeartbeatTimeoutSeconds treats the connection as lost and
// reconnects. It is additive: a new type, and no existing frame changes.

// HeartbeatSchemaV1 is the versioned name identifying the v1 heartbeat frame.
const HeartbeatSchemaV1 = "conch.heartbeat.v1"

const (
	// HeartbeatQueryParam is the query parameter that asks /v2/ws for
	// heartbeat frames (heartbeat=1). Every client of /v2/ws before this
	// frame existed decodes each frame as a message, so a frame it did not
	// ask for would reach it as an empty one. The events socket sends the
	// frame to everyone: its readers already skip a schema they do not know.
	HeartbeatQueryParam = "heartbeat"
	// HeartbeatIntervalSeconds is how often the server sends the frame.
	HeartbeatIntervalSeconds = 25
	// HeartbeatTimeoutSeconds is how long a client waits for any frame
	// before it treats the connection as lost.
	HeartbeatTimeoutSeconds = 60
)

// HeartbeatV1 is the frame itself. It carries nothing but its schema.
type HeartbeatV1 struct {
	// Schema is the document discriminator; always HeartbeatSchemaV1.
	Schema string `json:"schema"`
}

// Validate reports whether the frame is a v1 heartbeat.
func (h HeartbeatV1) Validate() error {
	if h.Schema != HeartbeatSchemaV1 {
		return fmt.Errorf("schema: heartbeat schema must be %q, got %q", HeartbeatSchemaV1, h.Schema)
	}
	return nil
}
