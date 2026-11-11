package push

import (
	"context"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// tlsFixture is a loopback TLS server that stands in for a public host, and a
// client built around a recording dialer that trusts only its certificate.
type tlsFixture struct {
	srv    *httptest.Server
	port   string
	client *http.Client
	rec    *recorder
	res    *fakeResolver
}

func newFixture(t *testing.T, res *fakeResolver, h http.Handler) *tlsFixture {
	t.Helper()
	permitLoopback(t)
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	rec := &recorder{next: plainDial}
	d := NewDialer(res, false)
	d.connect = rec.connect
	return &tlsFixture{srv: srv, port: u.Port(), client: newClient(d, pool), rec: rec, res: res}
}

func (f *tlsFixture) get(t *testing.T, host, path string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://"+net.JoinHostPort(host, f.port)+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := f.client.Do(req)
	if resp != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	return resp, err
}

func ok204(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

// T16 row 1: a name that was public at the first dial and answers private at
// the next is refused, and never dialled.
func TestT16Row1PublicThenPrivate(t *testing.T) {
	res := sequence([]string{"127.0.0.1"}, []string{"10.0.0.1"})
	f := newFixture(t, res, http.HandlerFunc(ok204))

	resp, err := f.get(t, "push.example.com", "/a")
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("first request: %v", err)
	}
	if got := Classify(err, resp); got != "" {
		t.Errorf("first request class = %q, want success", got)
	}
	before := len(f.rec.seen())

	_, err = f.get(t, "push.example.com", "/a")
	if got := Classify(err, nil); got != ClassAddressRefused {
		t.Fatalf("second request class = %q (err %v), want %q", got, err, ClassAddressRefused)
	}
	if after := len(f.rec.seen()); after != before {
		t.Errorf("dialled after the name turned private: %v", f.rec.seen())
	}
}

// T16 row 2: a TTL-0 name alternating public and private. Every dial
// re-resolves, and every private answer is refused.
func TestT16Row2Alternating(t *testing.T) {
	res := &fakeResolver{fn: func(call int, _ string) ([]netip.Addr, error) {
		if call%2 == 0 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("192.168.1.9")}, nil
	}}
	f := newFixture(t, res, http.HandlerFunc(ok204))

	const n = 6
	dials := 0
	for i := 0; i < n; i++ {
		resp, err := f.get(t, "flip.example.com", "/")
		if i%2 == 0 {
			if got := Classify(err, resp); err != nil || got != "" {
				t.Fatalf("request %d: class %q err %v, want success", i, got, err)
			}
			dials++
		} else if got := Classify(err, nil); got != ClassAddressRefused {
			t.Fatalf("request %d: class %q, want %q", i, got, ClassAddressRefused)
		}
		if res.calls() != i+1 {
			t.Fatalf("request %d: %d lookups, want %d (every dial re-resolves)", i, res.calls(), i+1)
		}
	}
	if got := len(f.rec.seen()); got != dials {
		t.Errorf("dials = %d, want %d (only the public answers)", got, dials)
	}
	for _, target := range f.rec.seen() {
		if ap := netip.MustParseAddrPort(target); ap.Addr() != netip.MustParseAddr("127.0.0.1") {
			t.Errorf("dialled %s", target)
		}
	}
}

// T16 row 3: a mixed answer (one public A, one private AAAA) is refused whole.
func TestT16Row3MixedAnswer(t *testing.T) {
	res := fixed("127.0.0.1", "fd00::5")
	f := newFixture(t, res, http.HandlerFunc(ok204))
	_, err := f.get(t, "mixed.example", "/")
	if got := Classify(err, nil); got != ClassAddressRefused {
		t.Fatalf("class = %q, want %q", got, ClassAddressRefused)
	}
	if len(f.rec.seen()) != 0 {
		t.Errorf("dialled the public half of a mixed answer: %v", f.rec.seen())
	}
}

// T16 row 4: IPv4-mapped, IPv4-compatible and NAT64 answers embedding a
// private v4 are refused (and the 6to4 and NAT64 local-use forms with them).
func TestT16Row4EmbeddedPrivateV4(t *testing.T) {
	tests := []struct{ name, answer string }{
		{"mapped", "::ffff:10.0.0.1"},
		{"mapped metadata", "::ffff:169.254.169.254"},
		{"compatible", "::10.0.0.1"},
		{"nat64", "64:ff9b::a00:1"},
		{"nat64 local-use", "64:ff9b:1::a00:1"},
		{"6to4", "2002:0a00:0001::"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixed(tc.answer), http.HandlerFunc(ok204))
			_, err := f.get(t, "embed.example", "/")
			if got := Classify(err, nil); got != ClassAddressRefused {
				t.Fatalf("class = %q, want %q", got, ClassAddressRefused)
			}
			if len(f.rec.seen()) != 0 {
				t.Errorf("dialled: %v", f.rec.seen())
			}
		})
	}
}

// T16 row 5: a 302 to a private host is not followed, is classified as
// status 3xx, and nothing is resolved or dialled for the redirect target.
func TestT16Row5RedirectNotFollowed(t *testing.T) {
	res := fixed("127.0.0.1")
	var hits atomic.Int32
	f := newFixture(t, res, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "https://internal.example:"+r.URL.Query().Get("p")+"/admin")
		w.WriteHeader(http.StatusFound)
	}))
	resp, err := f.get(t, "push.example.com", "/hop?p="+f.port)
	if err != nil {
		t.Fatalf("redirect must come back as a response, got %v", err)
	}
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got := Classify(err, resp); got != ClassStatus3xx {
		t.Errorf("class = %q, want %q", got, ClassStatus3xx)
	}
	if hits.Load() != 1 {
		t.Errorf("server saw %d requests, want 1", hits.Load())
	}
	for _, h := range res.asked() {
		if h == "internal.example" {
			t.Error("resolved the redirect target")
		}
	}
	if len(f.rec.seen()) != 1 {
		t.Errorf("dials = %v, want exactly the original", f.rec.seen())
	}
}

// T16 row 6: the peer is a checked address. Every connect target is one of the
// addresses the check returned for that dial, as a numeric address, and a
// resolver whose later answers differ is asked once per dial.
func TestT16Row6PeerIsACheckedAddress(t *testing.T) {
	res := &fakeResolver{}
	var answered [][]netip.Addr
	res.fn = func(call int, _ string) ([]netip.Addr, error) {
		// 127.0.0.2 refuses the connection (the server listens on .1), so the
		// dialler fails over to .1; the answers change on every lookup.
		a := []netip.Addr{netip.MustParseAddr("127.0.0.1")}
		if call == 0 {
			a = []netip.Addr{netip.MustParseAddr("127.0.0.2"), netip.MustParseAddr("127.0.0.1")}
		}
		answered = append(answered, a)
		return a, nil
	}
	f := newFixture(t, res, http.HandlerFunc(ok204))
	testPermit = func(a netip.Addr) bool { return a.IsLoopback() }

	for i := 0; i < 2; i++ {
		start := len(f.rec.seen())
		resp, err := f.get(t, "peer.example.com", "/")
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("request %d: %v", i, err)
		}
		if res.calls() != i+1 {
			t.Fatalf("request %d: %d lookups", i, res.calls())
		}
		targets := f.rec.seen()[start:]
		if len(targets) == 0 {
			t.Fatalf("request %d: no dial", i)
		}
		for _, target := range targets {
			ap, err := netip.ParseAddrPort(target)
			if err != nil {
				t.Fatalf("target %q is not numeric: %v", target, err)
			}
			if strconv.Itoa(int(ap.Port())) != f.port {
				t.Errorf("target %q has the wrong port", target)
			}
			found := false
			for _, a := range answered[i] {
				if a == ap.Addr() {
					found = true
				}
			}
			if !found {
				t.Errorf("request %d: target %q is not an address the check returned %v", i, target, answered[i])
			}
		}
	}
}

// T16 row 7: the host name is still verified. The test server's certificate is
// for example.com (and 127.0.0.1), so another name fails as a TLS failure and
// the right one passes.
func TestT16Row7HostnameVerified(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{"certificate name", "example.com", ""},
		{"another name", "other.example", ClassTLS},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixed("127.0.0.1"), http.HandlerFunc(ok204))
			resp, err := f.get(t, tc.host, "/")
			if got := Classify(err, resp); got != tc.want {
				t.Fatalf("class = %q (err %v), want %q", got, err, tc.want)
			}
		})
	}
}

// T16 row 8: allowPrivate admits a LAN address, and the pinning of rows 1, 2
// and 6 still applies. No real connection is made: the recording connect
// function fails, and the test reads its target.
func TestT16Row8AllowPrivate(t *testing.T) {
	t.Run("admits a LAN address and dials exactly it", func(t *testing.T) {
		rec := &recorder{}
		d := NewDialer(fixed("192.168.1.10"), true)
		d.connect = rec.connect
		_, err := d.DialContext(context.Background(), "tcp", "lan.example:8443")
		if got := Classify(err, nil); got != ClassRequestFailed {
			t.Fatalf("class = %q, want the connect failure, not a refusal", got)
		}
		if got := rec.seen(); len(got) != 1 || got[0] != "192.168.1.10:8443" {
			t.Fatalf("targets = %v", got)
		}
	})
	t.Run("row 1 public then loopback", func(t *testing.T) {
		rec := &recorder{}
		d := NewDialer(sequence([]string{"192.168.1.10"}, []string{"127.0.0.1"}), true)
		d.connect = rec.connect
		_, _ = d.DialContext(context.Background(), "tcp", "lan.example:443")
		_, err := d.DialContext(context.Background(), "tcp", "lan.example:443")
		if got := Classify(err, nil); got != ClassAddressRefused {
			t.Fatalf("class = %q, want %q", got, ClassAddressRefused)
		}
		if len(rec.seen()) != 1 {
			t.Errorf("dialled loopback: %v", rec.seen())
		}
	})
	t.Run("row 2 alternating LAN and metadata", func(t *testing.T) {
		res := &fakeResolver{fn: func(call int, _ string) ([]netip.Addr, error) {
			if call%2 == 0 {
				return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
		}}
		rec := &recorder{}
		d := NewDialer(res, true)
		d.connect = rec.connect
		for i := 0; i < 4; i++ {
			_, err := d.DialContext(context.Background(), "tcp", "lan.example:443")
			want := ClassRequestFailed
			if i%2 == 1 {
				want = ClassAddressRefused
			}
			if got := Classify(err, nil); got != want {
				t.Fatalf("dial %d: class %q, want %q", i, got, want)
			}
		}
		if res.calls() != 4 || len(rec.seen()) != 2 {
			t.Errorf("lookups %d, dials %v", res.calls(), rec.seen())
		}
	})
	t.Run("row 3 LAN plus loopback is refused whole", func(t *testing.T) {
		rec := &recorder{}
		d := NewDialer(fixed("192.168.1.10", "127.0.0.1"), true)
		d.connect = rec.connect
		_, err := d.DialContext(context.Background(), "tcp", "lan.example:443")
		if got := Classify(err, nil); got != ClassAddressRefused || len(rec.seen()) != 0 {
			t.Fatalf("class %q, dials %v", got, rec.seen())
		}
	})
	t.Run("row 6 peer is a checked address", func(t *testing.T) {
		rec := &recorder{}
		d := NewDialer(fixed("fd00::1", "172.16.0.4"), true)
		d.connect = rec.connect
		_, _ = d.DialContext(context.Background(), "tcp", "lan.example:443")
		want := []string{"[fd00::1]:443", "172.16.0.4:443"}
		got := rec.seen()
		if len(got) != len(want) {
			t.Fatalf("targets = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("target %d = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

func TestClientProperties(t *testing.T) {
	c := NewClient(fixed("93.184.216.34"), false)
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", c.Transport)
	}
	tests := []struct {
		name string
		ok   bool
	}{
		{"no proxy", tr.Proxy == nil},
		{"no keep-alives", tr.DisableKeepAlives},
		{"HTTP/2 attempted", tr.ForceAttemptHTTP2},
		{"dials through the pinned dialer", tr.DialContext != nil && tr.DialTLSContext == nil},
		{"2s timeout", c.Timeout == 2*time.Second && RequestTimeout == 2*time.Second},
		{"certificates verified", tr.TLSClientConfig != nil && !tr.TLSClientConfig.InsecureSkipVerify},
		{"TLS 1.2 minimum", tr.TLSClientConfig != nil && tr.TLSClientConfig.MinVersion >= 0x0303},
		{"no server name override", tr.TLSClientConfig != nil && tr.TLSClientConfig.ServerName == ""},
		{"uses system roots", tr.TLSClientConfig != nil && tr.TLSClientConfig.RootCAs == nil},
		{"no cookie jar", c.Jar == nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.ok {
				t.Fatal("property does not hold")
			}
		})
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://x.example/", nil)
	if err := c.CheckRedirect(req, []*http.Request{req}); err != http.ErrUseLastResponse { //nolint:errorlint // the sentinel is returned as is
		t.Errorf("CheckRedirect = %v, want http.ErrUseLastResponse", err)
	}
}

// HTTPS_PROXY in the environment is ignored: the request goes to the pinned
// address, and the proxy listener sees nothing.
func TestClientIgnoresProxyEnvironment(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	var proxied atomic.Int32
	go func() {
		for {
			c, err := proxy.Accept()
			if err != nil {
				return
			}
			proxied.Add(1)
			_ = c.Close()
		}
	}()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY"} {
		t.Setenv(k, "http://"+proxy.Addr().String())
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	f := newFixture(t, fixed("127.0.0.1"), http.HandlerFunc(ok204))
	resp, err := f.get(t, "example.com", "/")
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("request: %v", err)
	}
	if proxied.Load() != 0 {
		t.Errorf("proxy saw %d connections", proxied.Load())
	}
	if len(f.rec.seen()) != 1 {
		t.Errorf("dials = %v, want the pinned address", f.rec.seen())
	}
}

// Two requests make two dials, and two lookups: no connection is reused.
func TestClientReusesNoConnection(t *testing.T) {
	res := fixed("127.0.0.1")
	f := newFixture(t, res, http.HandlerFunc(ok204))
	for i := 0; i < 2; i++ {
		resp, err := f.get(t, "example.com", "/")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if got := len(f.rec.seen()); got != 2 {
		t.Errorf("dials = %d, want 2", got)
	}
	if res.calls() != 2 {
		t.Errorf("lookups = %d, want 2", res.calls())
	}
}

// A slow server hits the request timeout and is classified as one.
func TestClientTimeout(t *testing.T) {
	release := make(chan struct{})
	f := newFixture(t, fixed("127.0.0.1"), http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release) })
	f.client.Timeout = 100 * time.Millisecond

	_, err := f.get(t, "example.com", "/")
	if got := Classify(err, nil); got != ClassTimeout {
		t.Fatalf("class = %q (err %v), want %q", got, err, ClassTimeout)
	}
}

// Status families come back as classes from a real exchange.
func TestClientStatusClasses(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{http.StatusCreated, ""},
		{http.StatusMovedPermanently, ClassStatus3xx},
		{http.StatusGone, ClassStatus4xx},
		{http.StatusBadGateway, ClassStatus5xx},
	}
	for _, tc := range tests {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			f := newFixture(t, fixed("127.0.0.1"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://nowhere.example/")
				w.WriteHeader(tc.code)
			}))
			resp, err := f.get(t, "example.com", "/")
			if got := Classify(err, resp); got != tc.want {
				t.Fatalf("class = %q (err %v), want %q", got, err, tc.want)
			}
		})
	}
}

// A refused TLS peer and a closed port are classified without naming either.
func TestClientConnectionRefused(t *testing.T) {
	permitLoopback(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	_ = l.Close()

	d := NewDialer(fixed("127.0.0.1"), false)
	c := newClient(d, nil)
	_, err = c.Get("https://refused.example:" + port + "/marker-path") //nolint:noctx,bodyclose // a failing request has no body
	if got := Classify(err, nil); got != ClassConnRefused {
		t.Fatalf("class = %q (err %v), want %q", got, err, ClassConnRefused)
	}
}

// Nothing Classify reports, for any outcome above, carries the marker values
// used for the host, the address and the path.
func TestClassifyCarriesNoMarkers(t *testing.T) {
	permitLoopback(t)
	res := &fakeResolver{fn: func(call int, h string) ([]netip.Addr, error) {
		switch call {
		case 0:
			return []netip.Addr{netip.MustParseAddr("10.9.8.7")}, nil
		case 1:
			return nil, &net.DNSError{Name: h, Err: "no such host", Server: "203.0.113.53"}
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}}
	d := NewDialer(res, false)
	d.connect = (&recorder{}).connect
	c := newClient(d, nil)
	for i := 0; i < 3; i++ {
		resp, err := c.Get("https://marker-host.example:4433/marker-path") //nolint:noctx // test
		if resp != nil {
			_ = resp.Body.Close()
		}
		class := Classify(err, resp)
		known := false
		for _, k := range Classes() {
			known = known || k == class
		}
		if !known {
			t.Errorf("request %d: %q is not a class", i, class)
		}
	}
}
