package push

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"syscall"
)

// The failure classes. Each is a fixed, label-safe string: none carries a
// host, an address, a path or a URL. They are ntfy's classes plus
// ClassAddressRefused and ClassStatus3xx.
const (
	ClassTimeout        = "timeout"
	ClassNameLookup     = "name lookup failed"
	ClassAddressRefused = "address refused"
	ClassTLS            = "TLS failure"
	ClassConnRefused    = "connection refused"
	ClassStatus3xx      = "status 3xx"
	ClassStatus4xx      = "status 4xx"
	ClassStatus5xx      = "status 5xx"
	ClassRequestFailed  = "request failed"
)

// Classes returns every failure class, for a metric that wants its label set
// up front.
func Classes() []string {
	return []string{
		ClassTimeout, ClassNameLookup, ClassAddressRefused, ClassTLS,
		ClassConnRefused, ClassStatus3xx, ClassStatus4xx, ClassStatus5xx,
		ClassRequestFailed,
	}
}

// classedError is an error whose whole text is a failure class. The dialer
// returns only these, so an error that came out of it can be logged without
// leaking the address it was dialling.
type classedError string

func (e classedError) Error() string { return string(e) }

// The errors Check and the dialer return. Their text is the class.
var (
	// ErrAddressRefused means a resolved address, or an IP literal, is in a
	// forbidden range.
	ErrAddressRefused error = classedError(ClassAddressRefused)
	// ErrLookupFailed means the name did not resolve, or the lookup timed out.
	ErrLookupFailed error = classedError(ClassNameLookup)
)

// Classify maps the outcome of a request onto exactly one failure class. With
// a non-nil err it names the kind of transport failure. With a nil err it
// names the status family of resp: a 3xx, 4xx or 5xx is a class, a 2xx is a
// success and returns "". A nil error with no response is "request failed".
// Nothing from err's text is passed through, because the client's own errors
// quote the URL.
func Classify(err error, resp *http.Response) string {
	if err != nil {
		return classifyError(err)
	}
	if resp == nil {
		return ClassRequestFailed
	}
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return ""
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return ClassStatus3xx
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return ClassStatus4xx
	case resp.StatusCode >= 500 && resp.StatusCode < 600:
		return ClassStatus5xx
	default:
		return ClassRequestFailed
	}
}

func classifyError(err error) string {
	var (
		classed  classedError
		dns      *net.DNSError
		netErr   net.Error
		hostname x509.HostnameError
		unknown  x509.UnknownAuthorityError
		invalid  x509.CertificateInvalidError
		verify   *tls.CertificateVerificationError
		record   tls.RecordHeaderError
	)
	switch {
	case errors.As(err, &classed):
		return string(classed)
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return ClassTimeout
	case errors.As(err, &dns):
		return ClassNameLookup
	case errors.As(err, &hostname), errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &verify), errors.As(err, &record):
		return ClassTLS
	case errors.Is(err, syscall.ECONNREFUSED):
		return ClassConnRefused
	default:
		return ClassRequestFailed
	}
}
