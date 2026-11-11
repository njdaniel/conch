// Package pushtest is a fake Web Push service for tests: an httptest TLS
// server that receives what conchd's sender posts, decrypts it with the
// subscriber's key as a browser would (RFC 8291), verifies the VAPID
// authorization (RFC 8292), records everything it saw and answers as scripted:
// 201, 404, 410, 429 with Retry-After, 500, a hang and a 302.
//
// Input: scripted replies and subscriptions. Output: the deliveries it
// recorded. It owns the decryption and the JWT check, written apart from the
// sender's own code so each is proved against the other. It imports no other
// package of conchd but pkg/schema, so any test may use it. It is for tests
// only: nothing in conchd imports it.
package pushtest

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Subscriber is a device's half of a Web Push subscription: its key pair and
// auth secret, which the phone would give conchd when it registers.
type Subscriber struct {
	priv *ecdh.PrivateKey
	auth []byte
}

// NewSubscriber makes a subscriber with fresh keys.
func NewSubscriber(t testing.TB) *Subscriber {
	t.Helper()
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		t.Fatal(err)
	}
	return &Subscriber{priv: priv, auth: auth}
}

// P256DH is the subscriber's public key as a registration carries it:
// unpadded base64url of the 65-byte point.
func (s *Subscriber) P256DH() string {
	return base64.RawURLEncoding.EncodeToString(s.priv.PublicKey().Bytes())
}

// Auth is the subscriber's auth secret as a registration carries it.
func (s *Subscriber) Auth() string { return base64.RawURLEncoding.EncodeToString(s.auth) }

// Decrypt opens a request body as the subscriber: RFC 8188's aes128gcm with
// RFC 8291's key derivation. It returns the plaintext without its padding
// delimiter, or an error for anything that does not decrypt.
func (s *Subscriber) Decrypt(body []byte) ([]byte, error) {
	if len(body) < 21+65+16+1 {
		return nil, errors.New("short body")
	}
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	if idlen != 65 || rs < 18 || len(body) < 21+idlen {
		return nil, errors.New("bad header")
	}
	asPub, err := ecdh.P256().NewPublicKey(body[21 : 21+idlen])
	if err != nil {
		return nil, err
	}
	secret, err := s.priv.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	prkKey, err := hkdf.Extract(sha256.New, secret, s.auth)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), s.priv.PublicKey().Bytes()...), asPub.Bytes()...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(info), 32)
	if err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	rec, err := aead.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	i := len(rec) - 1
	for i >= 0 && rec[i] == 0 {
		i--
	}
	if i < 0 || rec[i] != 0x02 {
		return nil, errors.New("no last-record delimiter")
	}
	return rec[:i], nil
}

// Reply is how the provider answers one request.
type Reply struct {
	// Status is the HTTP status; zero means 201.
	Status int
	// RetryAfter is the Retry-After header, when not empty.
	RetryAfter string
	// Location is the Location header, for a 3xx.
	Location string
	// Hang holds the request open until the client gives up.
	Hang bool
}

// Delivery is one request the provider received.
type Delivery struct {
	Path   string
	Header http.Header
	Body   []byte
	// Plain is the decrypted body; DecryptErr says why it is nil.
	Plain      []byte
	DecryptErr error
	// Registration is the registration id in the decrypted wake, when it is
	// one.
	Registration string
	// VAPID is what the Authorization header held, verified.
	VAPID VAPID
}

// VAPID is a verified VAPID authorization (RFC 8292).
type VAPID struct {
	// Scheme is the Authorization scheme ("vapid").
	Scheme string
	// Key is the k parameter: the application server's public key.
	Key string
	// SigOK reports that the JWT's ES256 signature verifies under Key.
	SigOK bool
	// Header and Claims are the decoded JWT parts.
	Header map[string]any
	Claims map[string]any
	// Err says why the authorization did not parse or verify.
	Err error
}

// Provider is the fake push service.
type Provider struct {
	srv *httptest.Server

	mu         sync.Mutex
	subs       map[string]*Subscriber
	script     func(n int, d *Delivery) Reply
	deliveries []Delivery
	n          int
	done       chan struct{}
}

// New starts a provider that answers 201 to everything. It is closed with the
// test.
func New(t testing.TB) *Provider {
	t.Helper()
	p := &Provider{subs: map[string]*Subscriber{}, done: make(chan struct{})}
	p.srv = httptest.NewTLSServer(http.HandlerFunc(p.handle))
	t.Cleanup(p.Close)
	return p
}

// Close stops the provider and releases any hanging request.
func (p *Provider) Close() {
	p.mu.Lock()
	select {
	case <-p.done:
	default:
		close(p.done)
	}
	p.mu.Unlock()
	p.srv.CloseClientConnections()
	p.srv.Close()
}

// URL is the provider's base URL, https://127.0.0.1:port.
func (p *Provider) URL() string { return p.srv.URL }

// Client is an http.Client that trusts the provider's certificate. It is for
// a test that stands in for the sender's own client.
func (p *Provider) Client() *http.Client { return p.srv.Client() }

// RoutedClient is an http.Client that trusts the provider's certificate and
// connects to the provider whatever host the URL names, so an endpoint can
// carry a host name (example.com, which the certificate is valid for) that a
// test resolver maps to a public address, and the request still lands here.
// It follows no redirect. It stands in for the sender's own client where the
// test cannot reach the address guard's loopback exemption (which only the push
// package's own tests have); the guard is tested there.
func (p *Provider) RoutedClient() *http.Client {
	pool := x509.NewCertPool()
	pool.AddCert(p.srv.Certificate())
	addr := p.srv.Listener.Addr().String()
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// EndpointAt is Endpoint with the host replaced: https://host:port/push/....
func (p *Provider) EndpointAt(sub *Subscriber, host string) string {
	u, err := url.Parse(p.Endpoint(sub))
	if err != nil {
		return ""
	}
	return "https://" + host + ":" + u.Port() + u.Path
}

// EndpointWithPath registers sub under path (which must start with "/") and
// returns https://host:port/path, for a test that plants a marker in the path.
func (p *Provider) EndpointWithPath(sub *Subscriber, host, path string) string {
	u, err := url.Parse(p.srv.URL)
	if err != nil {
		return ""
	}
	p.mu.Lock()
	p.subs[path] = sub
	p.mu.Unlock()
	return "https://" + host + ":" + u.Port() + path
}

// Certificate is the provider's certificate, for a test that builds its own
// pool.
func (p *Provider) Certificate() *x509.Certificate { return p.srv.Certificate() }

// Endpoint registers sub under a new path of the provider and returns the
// endpoint URL a phone would register with.
func (p *Provider) Endpoint(sub *Subscriber) string {
	raw := make([]byte, 24)
	_, _ = rand.Read(raw)
	path := "/push/" + base64.RawURLEncoding.EncodeToString(raw)
	p.mu.Lock()
	p.subs[path] = sub
	p.mu.Unlock()
	return p.srv.URL + path
}

// Script sets how the provider answers: f gets the request's number (from 1)
// and the delivery, and returns the reply. Nil restores 201.
func (p *Provider) Script(f func(n int, d *Delivery) Reply) {
	p.mu.Lock()
	p.script = f
	p.mu.Unlock()
}

// Deliveries returns what the provider received, oldest first.
func (p *Provider) Deliveries() []Delivery {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Delivery(nil), p.deliveries...)
}

// Count is how many requests the provider has received.
func (p *Provider) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.deliveries)
}

// WaitFor waits until the provider has received n requests or the timeout
// passes, and returns them.
func (p *Provider) WaitFor(t testing.TB, n int, timeout time.Duration) []Delivery {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := p.Deliveries(); len(got) >= n {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := p.Deliveries()
	t.Fatalf("the provider received %d requests in %s, want %d", len(got), timeout, n)
	return got
}

func (p *Provider) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	d := Delivery{Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
	p.mu.Lock()
	sub := p.subs[r.URL.Path]
	p.mu.Unlock()
	if sub == nil {
		d.DecryptErr = errors.New("no subscriber for the path")
	} else {
		d.Plain, d.DecryptErr = sub.Decrypt(body)
	}
	if d.Plain != nil {
		var wake schema.PushWakeV1
		if json.Unmarshal(d.Plain, &wake) == nil {
			d.Registration = wake.Registration
		}
	}
	d.VAPID = VerifyVAPID(r.Header.Get("Authorization"))

	p.mu.Lock()
	p.deliveries = append(p.deliveries, d)
	p.n++
	n, script := p.n, p.script
	p.mu.Unlock()

	reply := Reply{}
	if script != nil {
		reply = script(n, &d)
	}
	if reply.Hang {
		select {
		case <-r.Context().Done():
		case <-p.done:
		}
		return
	}
	if reply.RetryAfter != "" {
		w.Header().Set("Retry-After", reply.RetryAfter)
	}
	if reply.Location != "" {
		w.Header().Set("Location", reply.Location)
	}
	status := reply.Status
	if status == 0 {
		status = http.StatusCreated
	}
	w.WriteHeader(status)
}

// VerifyVAPID parses "vapid t=<jwt>, k=<key>" and checks the JWT's ES256
// signature under k, as a push service does.
func VerifyVAPID(header string) VAPID {
	var v VAPID
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok {
		v.Err = errors.New("no parameters")
		return v
	}
	v.Scheme = scheme
	var jwt string
	for _, part := range strings.Split(rest, ",") {
		k, val, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch k {
		case "t":
			jwt = val
		case "k":
			v.Key = val
		}
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		v.Err = errors.New("the token is not a JWT")
		return v
	}
	enc := base64.RawURLEncoding
	for i, dst := range []*map[string]any{&v.Header, &v.Claims} {
		raw, err := enc.DecodeString(parts[i])
		if err != nil || json.Unmarshal(raw, dst) != nil {
			v.Err = errors.New("the token does not decode")
			return v
		}
	}
	pub, err := enc.DecodeString(v.Key)
	if err != nil {
		v.Err = errors.New("the key does not decode")
		return v
	}
	pk, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pub)
	if err != nil {
		v.Err = errors.New("the key is not a P-256 point")
		return v
	}
	sig, err := enc.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		v.Err = errors.New("the signature is not 64 bytes")
		return v
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	der, err := asn1.Marshal(struct{ R, S *big.Int }{new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])})
	if err != nil {
		v.Err = errors.New("the signature does not encode")
		return v
	}
	v.SigOK = ecdsa.VerifyASN1(pk, digest[:], der)
	return v
}
