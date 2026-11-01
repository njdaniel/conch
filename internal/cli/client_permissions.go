package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/njdaniel/conch/pkg/schema"
)

// maxPermissionsBody caps the permissions document a client reads. The
// catalog has eleven rows; a server that sends more than this is not one to
// believe.
const maxPermissionsBody = 1 << 20

// permissionsDoc is the answer decoded into, with its size bounded.
type permissionsDoc struct {
	schema.PermissionsResponseV1
}

func (permissionsDoc) maxBody() int64 { return maxPermissionsBody }

// GetPermissions reads the permissions document (GET /v1/permissions, any
// operator).
func (c *Client) GetPermissions(ctx context.Context) (schema.PermissionsResponseV1, error) {
	var doc permissionsDoc
	if err := c.roundTrip(ctx, "read permissions", http.MethodGet, c.resolve("v1", "permissions"), nil, &doc); err != nil {
		return schema.PermissionsResponseV1{}, err
	}
	return doc.PermissionsResponseV1, nil
}

// PutPermissions changes the permissions document (PUT /v1/permissions). The
// server refuses a stale revision, and wants a recent proof of identity,
// which the client's reauth hook supplies.
func (c *Client) PutPermissions(ctx context.Context, req schema.PutPermissionsRequestV1) (schema.PermissionsResponseV1, error) {
	var doc permissionsDoc
	if err := c.roundTrip(ctx, "write permissions", http.MethodPut, c.resolve("v1", "permissions"), req, &doc); err != nil {
		return schema.PermissionsResponseV1{}, err
	}
	return doc.PermissionsResponseV1, nil
}

// bodyTooLargeError is a response that ran past its cap.
type bodyTooLargeError struct{ limit int64 }

func (e *bodyTooLargeError) Error() string {
	return fmt.Sprintf("the server's answer is larger than %d bytes", e.limit)
}

// cappedReader fails, rather than ends, once more than limit bytes are read,
// so an over-long document is refused and not decoded from a cut-off prefix.
type cappedReader struct {
	r     io.Reader
	left  int64
	limit int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, &bodyTooLargeError{limit: c.limit}
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left <= 0 {
		return n, &bodyTooLargeError{limit: c.limit}
	}
	return n, err
}
