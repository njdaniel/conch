package schema

// CreateHookRequest is the request body for provisioning a webhook ingest
// token bound to a channel and an attributed principal.
type CreateHookRequest struct {
	Channel   string `json:"channel"`
	Principal int64  `json:"principal"`
}

// CreateHookResponse is the response body after a hook is provisioned. The
// token is shown once at creation and never again.
type CreateHookResponse struct {
	Token string `json:"token"`
}
