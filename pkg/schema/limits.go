package schema

// Sizes that the server and its clients must agree on. They are not wire
// shapes, but a client that reads less than a server may send cannot read the
// protocol, so they live with it.

const (
	// MaxPostRequestBytes is the largest JSON request body conchd reads when
	// a message is posted: the whole request, not the message text.
	MaxPostRequestBytes = 1 << 20

	// MaxMessageFrameBytes is the largest WebSocket frame a server sends for
	// a message that was posted in a request of at most MaxPostRequestBytes,
	// and so the least a client must be willing to read. The frame is the
	// stored message encoded again, and encoding can lengthen it: a "<", ">"
	// or "&" that arrived as one byte leaves as a six-byte Unicode escape
	// (a backslash, a "u" and four hex digits), and nothing grows by more
	// than that. The rest of the envelope (ids, a timestamp, the audience)
	// is far inside the margin.
	//
	// A client reading with a smaller limit is disconnected by any message
	// over it, which any member of the channel can send (issue #231).
	//
	// Every way a message is posted holds its request to MaxPostRequestBytes:
	// the REST post routes, the webhook route, and the MCP endpoint (issue
	// #237). A new one must too, or it reopens the hole.
	MaxMessageFrameBytes = 6*MaxPostRequestBytes + 64<<10
)
