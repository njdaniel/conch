package schema

// Health is the response body of GET /healthz. It reports the running server
// version and whether the backing store is reachable. It is an operational
// wire shape, not a versioned payload schema, so it carries no version field
// of its own and is not pinned by a golden fixture.
type Health struct {
	// Status is "ok" when every dependency is healthy, otherwise "degraded".
	Status string `json:"status"`
	// Version is the conchd build version.
	Version string `json:"version"`
	// DB is "ok" when the store is reachable, otherwise "degraded". Failure
	// detail stays in the server log; it is never put on the wire.
	DB string `json:"db"`
}

// Health status values.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
)
