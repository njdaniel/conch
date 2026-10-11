package schema

// MaxHookLabelLength is the longest allowed hook label, in characters.
const MaxHookLabelLength = 100

// CreateHookRequest is the request body for provisioning a webhook ingest
// token bound to a channel and an attributed principal. Label is an optional
// operator-chosen name that helps tell hooks apart when listing them.
type CreateHookRequest struct {
	Channel   string `json:"channel"`
	Principal int64  `json:"principal"`
	Label     string `json:"label,omitempty"`
}

// Validate reports whether the optional label is acceptable. The label is
// shown in lists and written to the audit log, so it passes the display-name
// rule of ValidateDisplayName; empty stays allowed.
func (r CreateHookRequest) Validate() error {
	if r.Label == "" {
		return nil
	}
	return ValidateDisplayName("hook label", r.Label, MaxHookLabelLength)
}

// CreateHookResponse is the response body after a hook is provisioned. The
// token is shown once at creation and never again. ID is the handle for
// listing and revoking the hook; it is omitted by servers that predate it.
type CreateHookResponse struct {
	ID    int64  `json:"id,omitempty"`
	Token string `json:"token"`
}

// HookV1 describes one webhook hook. It never contains the token or its hash:
// the token is shown once, in the create response. RevokedAt is omitted while
// the hook is active.
type HookV1 struct {
	ID        int64      `json:"id"`
	Channel   string     `json:"channel"`
	Principal int64      `json:"principal"`
	Label     string     `json:"label"`
	CreatedAt Timestamp  `json:"created_at"`
	RevokedAt *Timestamp `json:"revoked_at,omitempty"`
}

// ListHooksResponseV1 is the response to listing hooks, newest first, revoked
// ones included. Producers must send an empty slice, not nil, so the wire form
// is [] rather than null.
type ListHooksResponseV1 struct {
	Hooks []HookV1 `json:"hooks"`
}
