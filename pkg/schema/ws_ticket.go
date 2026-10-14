package schema

// WebSocket ticket wire shapes (issue #270; P3 epic #263, decision D2). A
// browser cannot set an Authorization header on a WebSocket, and the bearer
// token must never be put in a URL. So the browser asks for a ticket over
// REST with its bearer, and opens the socket with ?ticket=. A ticket is
// single use and lives for WSTicketLifetimeSeconds, and it carries no more
// authority than the credential that asked for it has at the moment it is
// spent. These shapes are additive and carry no schema-name field: they are
// REST bodies, versioned by the V1 type suffix.

const (
	// WSTicketPrefix starts every ticket, so a leaked one is recognisable
	// and is never mistaken for a credential token (which starts "conch_").
	WSTicketPrefix = "conch_wst_"
	// WSTicketRandomBytes is the number of crypto/rand bytes in a ticket.
	WSTicketRandomBytes = 32
	// WSTicketLength is the exact length of a ticket: the prefix plus 32
	// bytes in unpadded base64url, which is 43 characters.
	WSTicketLength = len(WSTicketPrefix) + 43
	// WSTicketLifetimeSeconds is how long a ticket can be spent after it is
	// issued. ExpiresAt in the response is the issue time plus this.
	WSTicketLifetimeSeconds = 30
	// MaxWSTicketsPerCredential is how many unspent, unexpired tickets one
	// credential may hold. The next request is refused with HTTP 429.
	MaxWSTicketsPerCredential = 8
)

// Error codes returned by POST /v1/ws-tickets in Error.Code, beside the voice
// and session codes.
const (
	// ErrorCodeTicketsRequireAuth: the caller is not a verified principal
	// (conchd runs with authentication off). A ticket is bound to the
	// credential that asked for it, so there is nothing to bind it to.
	ErrorCodeTicketsRequireAuth = "tickets_require_auth"
	// ErrorCodeTooManyTickets: the caller's credential already holds
	// MaxWSTicketsPerCredential unspent tickets (HTTP 429). A ticket is spent
	// or expires within WSTicketLifetimeSeconds, so the caller retries then.
	ErrorCodeTooManyTickets = "too_many_tickets"
)

// WSTicketV1 is the body of the 201 response to POST /v1/ws-tickets (the
// request has no body). Ticket is the secret, shown exactly once; the server
// keeps only a hash of it. ExpiresAt is the instant after which it is refused.
type WSTicketV1 struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt Timestamp `json:"expires_at"`
}
