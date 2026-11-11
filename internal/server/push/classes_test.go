package push

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout marker-host.example" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return false }

func TestClassify(t *testing.T) {
	resp := func(code int) *http.Response { return &http.Response{StatusCode: code} }
	wrapped := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://marker-host.example/marker-path", Err: err}
	}
	tests := []struct {
		name string
		err  error
		resp *http.Response
		want string
	}{
		{"timeout via deadline", wrapped(context.DeadlineExceeded), nil, ClassTimeout},
		{"timeout via net.Error", wrapped(timeoutErr{}), nil, ClassTimeout},
		{"name lookup failed", wrapped(&net.DNSError{Name: "marker-host.example", Err: "no such host"}), nil, ClassNameLookup},
		{"name lookup failed from Check", wrapped(ErrLookupFailed), nil, ClassNameLookup},
		{"address refused", wrapped(ErrAddressRefused), nil, ClassAddressRefused},
		{"address refused through OpError", wrapped(&net.OpError{Op: "dial", Err: ErrAddressRefused}), nil, ClassAddressRefused},
		{"TLS hostname", wrapped(x509.HostnameError{Host: "marker-host.example"}), nil, ClassTLS},
		{"TLS unknown authority", wrapped(x509.UnknownAuthorityError{}), nil, ClassTLS},
		{"TLS verification", wrapped(&tls.CertificateVerificationError{Err: errors.New("x")}), nil, ClassTLS},
		{"TLS record header", wrapped(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}), nil, ClassTLS},
		{"connection refused", wrapped(&net.OpError{Op: "dial", Err: &net.OpError{Err: syscall.ECONNREFUSED}}), nil, ClassConnRefused},
		{"status 3xx", nil, resp(302), ClassStatus3xx},
		{"status 3xx 308", nil, resp(308), ClassStatus3xx},
		{"status 4xx", nil, resp(404), ClassStatus4xx},
		{"status 4xx 410", nil, resp(410), ClassStatus4xx},
		{"status 5xx", nil, resp(503), ClassStatus5xx},
		{"request failed unknown", wrapped(errors.New("marker-path boom")), nil, ClassRequestFailed},
		{"request failed cancelled", wrapped(context.Canceled), nil, ClassRequestFailed},
		{"request failed no response", nil, nil, ClassRequestFailed},
		{"request failed odd status", nil, resp(700), ClassRequestFailed},
		{"success is no class", nil, resp(201), ""},
		{"error wins over response", wrapped(ErrAddressRefused), resp(200), ClassAddressRefused},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.err, tc.resp)
			if got != tc.want {
				t.Fatalf("Classify = %q, want %q", got, tc.want)
			}
			for _, bad := range []string{"marker", "example", "https://"} {
				if strings.Contains(got, bad) {
					t.Errorf("class %q contains %q", got, bad)
				}
			}
		})
	}
}

func TestClassesAreTheVocabulary(t *testing.T) {
	want := map[string]bool{
		"timeout": true, "name lookup failed": true, "address refused": true,
		"TLS failure": true, "connection refused": true, "status 3xx": true,
		"status 4xx": true, "status 5xx": true, "request failed": true,
	}
	got := Classes()
	if len(got) != len(want) {
		t.Fatalf("Classes() = %v", got)
	}
	for _, c := range got {
		if !want[c] {
			t.Errorf("unexpected class %q", c)
		}
	}
	if fmt.Sprint(ErrAddressRefused) != "address refused" || fmt.Sprint(ErrLookupFailed) != "name lookup failed" {
		t.Error("sentinel text is not the class")
	}
}
