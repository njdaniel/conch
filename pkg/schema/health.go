package schema

// Health is the response returned by the server's health endpoint.
type Health struct {
	Version string `json:"version"`
	DB      string `json:"db"`
}
