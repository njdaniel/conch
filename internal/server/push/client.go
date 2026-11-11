package push

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"time"
)

// RequestTimeout is the whole-request budget of the client NewClient returns.
const RequestTimeout = 2 * time.Second

// NewClient returns the *http.Client a push request is sent with. It
//
//   - refuses any scheme but https before resolving or dialling;
//   - sends every request through a Dialer, so each connection resolves,
//     checks and pins its address;
//   - never honours HTTP_PROXY or HTTPS_PROXY, because a proxy would make the
//     connection to the proxy and bypass the pin;
//   - never reuses a connection, so every request dials and checks;
//   - asks for no compression, so no Accept-Encoding header is sent;
//   - negotiates HTTP/2 where the server offers it;
//   - follows no redirect: a 3xx is returned as a response;
//   - gives up after RequestTimeout;
//   - verifies the server certificate against the system roots and the URL's
//     host name. Verification is never switched off.
//
// A nil resolver means the system resolver. The caller must close every
// response body.
//
// The errors this client returns come from net/http and are *url.Error values
// whose text is the full request URL; a Web Push URL's path holds the
// subscription. A caller must therefore never log, audit or expose err.Error():
// it reports only Classify(err, resp).
func NewClient(resolver Resolver, allowPrivate bool) *http.Client {
	return newClient(NewDialer(resolver, allowPrivate), nil)
}

// newClient builds the client around d. rootCAs is nil in production, which
// means the system roots; a test passes its server's certificate pool. This is
// the only seam for trust, and it can add roots but not skip verification.
func newClient(d *Dialer, rootCAs *x509.CertPool) *http.Client {
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         d.DialContext,
		DisableKeepAlives:   true,
		DisableCompression:  true, // no Accept-Encoding: a wake needs no body, and none is read
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: RequestTimeout,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    rootCAs,
		},
	}
	return &http.Client{
		Transport: httpsOnly{next: tr},
		Timeout:   RequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// httpsOnly refuses any request whose scheme is not https, before anything is
// resolved or dialled. The error is a fixed class.
type httpsOnly struct{ next http.RoundTripper }

func (h httpsOnly) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Scheme != "https" {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, classedError(ClassRequestFailed)
	}
	return h.next.RoundTrip(req)
}
