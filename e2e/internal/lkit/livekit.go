package lkit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// ------------------------------------------------------------ admin API

// Admin asks LiveKit directly, with the API secret the harness chose, for
// ground truth that does not pass through conchd or any client: who is in a
// room, which rooms exist, and the server-side mute that stands in for a key
// press. It does not use conchd's own LiveKit client, so a defect there cannot
// make this program agree with itself.
type Admin struct {
	base   string // http://127.0.0.1:port
	key    string
	secret string
	http   *http.Client
}

func NewAdmin(base, key, secret string) *Admin {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &Admin{base: base, key: key, secret: secret, http: &http.Client{Transport: tr, Timeout: 5 * time.Second}}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// token signs an HS256 admin token carrying the given video grant.
func (a *Admin) token(video map[string]any) (string, error) {
	now := time.Now()
	body, err := json.Marshal(map[string]any{
		"iss":   a.key,
		"nbf":   now.Add(-30 * time.Second).Unix(),
		"exp":   now.Add(time.Minute).Unix(),
		"video": video,
	})
	if err != nil {
		return "", err
	}
	signing := b64([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + b64(body)
	mac := hmac.New(sha256.New, []byte(a.secret))
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil)), nil
}

func (a *Admin) call(ctx context.Context, method string, video map[string]any, req, out any) error {
	tok, err := a.token(video)
	if err != nil {
		return err
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, a.base+"/twirp/livekit.RoomService/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+tok)
	resp, err := a.http.Do(r)
	if err != nil {
		return fmt.Errorf("livekit %s: %w", method, UnwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("livekit %s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &StatusError{method: method, status: resp.StatusCode, body: Truncate(string(data), 200)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// StatusError is LiveKit answering an admin call with something other than
// 200, kept apart from a call that got no answer: "the room does not exist"
// is a fact about LiveKit, "the call failed" is not.
type StatusError struct {
	method string
	status int
	body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("livekit %s: HTTP %d: %s", e.method, e.status, e.body)
}

// RoomNotFound reports whether err is LiveKit saying the room does not exist.
func RoomNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// UnwrapURLError drops the URL from a transport error; the URL of an admin
// call is harmless but the habit keeps credentials out of messages.
func UnwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type Track struct {
	SID    string `json:"sid"`
	Type   any    `json:"type"`
	Source any    `json:"source"`
	Muted  bool   `json:"muted"`
}

// IsMicrophone is true for an audio track whose source is the microphone.
// protojson omits defaults, so an absent type is audio.
func (t Track) IsMicrophone() bool {
	typ := fmt.Sprint(t.Type)
	src := fmt.Sprint(t.Source)
	return (t.Type == nil || typ == "AUDIO" || typ == "0") && (src == "MICROPHONE" || src == "2")
}

type Participant struct {
	// SID is the id LiveKit assigns the connection (PA_...).
	SID      string  `json:"sid"`
	Identity string  `json:"identity"`
	State    any     `json:"state"`
	Tracks   []Track `json:"tracks"`
	// Permission is what LiveKit says the connection may do now: the join
	// token's grant, or what conchd last set with UpdateParticipant.
	Permission map[string]any `json:"permission"`
}

// Sources lists the sources of the participant's published tracks.
func (p Participant) Sources() []string {
	var out []string
	for _, t := range p.Tracks {
		out = append(out, fmt.Sprint(t.Source))
	}
	return out
}

// Mic returns the participant's microphone track, if it has one.
func (p Participant) Mic() (Track, bool) {
	for _, t := range p.Tracks {
		if t.IsMicrophone() {
			return t, true
		}
	}
	return Track{}, false
}

func (a *Admin) ListParticipants(ctx context.Context, room string) ([]Participant, error) {
	var resp struct {
		Participants []Participant `json:"participants"`
	}
	err := a.call(ctx, "ListParticipants", map[string]any{"roomAdmin": true, "room": room}, map[string]any{"room": room}, &resp)
	return resp.Participants, err
}

func (a *Admin) ListRooms(ctx context.Context) ([]string, error) {
	var resp struct {
		Rooms []struct {
			Name string `json:"name"`
		} `json:"rooms"`
	}
	if err := a.call(ctx, "ListRooms", map[string]any{"roomList": true}, map[string]any{}, &resp); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(resp.Rooms))
	for _, r := range resp.Rooms {
		names = append(names, r.Name)
	}
	return names, nil
}

// MintJoin signs a join token for identity into room with the API secret this
// run chose: a participant conchd never issued a token to, as someone holding a
// leaked secret could make. The token lives a minute.
func (a *Admin) MintJoin(identity, room string) (string, error) {
	now := time.Now()
	body, err := json.Marshal(map[string]any{
		"iss": a.key,
		"sub": identity,
		"nbf": now.Add(-5 * time.Second).Unix(),
		"exp": now.Add(time.Minute).Unix(),
		"video": map[string]any{
			"roomJoin": true, "room": room, "canSubscribe": true, "canPublish": false, "canPublishData": false,
		},
	})
	if err != nil {
		return "", err
	}
	signing := b64([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + b64(body)
	mac := hmac.New(sha256.New, []byte(a.secret))
	mac.Write([]byte(signing))
	return signing + "." + b64(mac.Sum(nil)), nil
}

func (a *Admin) Mute(ctx context.Context, room, identity, sid string, muted bool) error {
	return a.call(ctx, "MutePublishedTrack", map[string]any{"roomAdmin": true, "room": room},
		map[string]any{"room": room, "identity": identity, "track_sid": sid, "muted": muted}, nil)
}

// ----------------------------------------------- signalling-only client

// JWT finds a JWT inside a binary signalling frame.
var JWT = regexp.MustCompile(`eyJ[\w-]+\.[\w-]+\.[\w-]+`)

// SignalConn is a participant that has connected to LiveKit's signalling
// WebSocket with a token and does nothing else. LiveKit counts it as joined;
// it cannot publish. Its job here is to be a connected participant (the
// outsider, the removed member) and to hear the token LiveKit sends every
// connected participant of its own (design note §10, finding 9).
type SignalConn struct {
	c      *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	issued   []string // JWTs found in frames from LiveKit, in order
	closeErr error
}

// DialSignal connects to LiveKit's signalling endpoint. On a refused join it
// returns the HTTP status and body LiveKit answered with.
func DialSignal(ctx context.Context, wsBase, token string) (*SignalConn, int, string, error) {
	q := url.Values{"access_token": {token}, "protocol": {"9"}, "auto_subscribe": {"1"}}
	cctx, cancel := context.WithCancel(context.Background())
	c, resp, err := websocket.Dial(ctx, wsBase+"/rtc?"+q.Encode(), nil)
	if err != nil {
		cancel()
		status, body := 0, ""
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
				_ = resp.Body.Close()
				body = strings.TrimSpace(string(b))
			}
		}
		return nil, status, body, fmt.Errorf("signalling join refused")
	}
	c.SetReadLimit(1 << 20)
	s := &SignalConn{c: c, cancel: cancel, done: make(chan struct{})}
	go s.readLoop(cctx)
	return s, http.StatusSwitchingProtocols, "", nil
}

func (s *SignalConn) readLoop(ctx context.Context) {
	defer close(s.done)
	for {
		_, data, err := s.c.Read(ctx)
		if err != nil {
			s.mu.Lock()
			s.closeErr = err
			s.mu.Unlock()
			return
		}
		if found := JWT.FindAll(data, -1); len(found) > 0 {
			s.mu.Lock()
			for _, f := range found {
				s.issued = append(s.issued, string(f))
			}
			s.mu.Unlock()
		}
	}
}

// IssuedTokens returns the JWTs LiveKit has sent so far.
func (s *SignalConn) IssuedTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.issued...)
}

func (s *SignalConn) Closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *SignalConn) Close() {
	s.cancel()
	_ = s.c.CloseNow()
	<-s.done
}

// -------------------------------------------- token-substituting relay

// Relay is a signalling proxy between `lk` and LiveKit. `lk room join` signs
// its own join token from an API key and secret and has no option to use a
// token it is given, which would bypass conchd and prove nothing. So the
// relay sits in the middle: `lk` signs a token with a throwaway key LiveKit
// has never heard of, and the relay replaces it with the token conchd issued
// before the request reaches LiveKit. LiveKit therefore admits the
// participant, or not, on conchd's token alone. The relay also reads what
// LiveKit sends back, which is how the harness gets the token LiveKit itself
// issues a connected participant.
//
// The fields are set before ServeRelay and not changed after.
type Relay struct {
	Upstream string // host:port of LiveKit
	Secure   bool   // LiveKit is reached over TLS (wss and https), as on a deployment
	Token    string // the token conchd issued
	// Mint, when set, stands in for Token: each request the client makes is
	// admitted on the token Mint returns for the one the client presented.
	// e2e/voice's load test uses it to admit every participant that lk
	// load-test starts, through one relay, each on a token conchd issued to a
	// principal of its own.
	Mint func(ctx context.Context, clientToken string) (string, error)
	// Filter, when set, sees every frame the client sends LiveKit before
	// LiveKit does, and returns what is sent instead. It may block, which
	// holds back that frame and every one after it. The screen-share
	// scenarios use it (share.go): lk cannot say that a track is a screen
	// share, and cannot wait to publish one until conchd has granted it.
	Filter func(ctx context.Context, frame []byte) []byte

	srv *http.Server
	ln  net.Listener

	mu     sync.Mutex
	issued []string
}

// IssuedTokens returns the JWTs LiveKit sent the participant behind the relay.
func (r *Relay) IssuedTokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.issued...)
}

// StartRelay starts a relay that admits its client on conchdToken alone.
// filter is nil for a relay that passes the client's frames on unchanged.
func StartRelay(upstreamHostPort, conchdToken string, filter func(context.Context, []byte) []byte) (*Relay, error) {
	return ServeRelay(&Relay{Upstream: upstreamHostPort, Token: conchdToken, Filter: filter})
}

// relayTokenKey carries the token the relay chose for a plain HTTP request
// to the reverse proxy's Rewrite.
type relayTokenKey struct{}

// ServeRelay starts r on a loopback port of its own.
func ServeRelay(r *Relay) (*Relay, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r.ln = ln
	rp := &httputil.ReverseProxy{
		// lk retries after LiveKit stops; that is expected, not output.
		ErrorLog: log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = r.scheme("http")
			pr.Out.URL.Host = r.Upstream
			pr.Out.Host = r.Upstream
			token, _ := pr.In.Context().Value(relayTokenKey{}).(string)
			r.swap(pr.Out.URL, pr.Out.Header, token)
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		token := r.Token
		if r.Mint != nil {
			var err error
			if token, err = r.Mint(req.Context(), ClientToken(req)); err != nil {
				// The error says what went wrong; it never carries a token.
				http.Error(w, "relay: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
			r.serveSocket(w, req, token)
			return
		}
		rp.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), relayTokenKey{}, token)))
	})
	r.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = r.srv.Serve(ln) }()
	return r, nil
}

// URL is where a client reaches the relay.
func (r *Relay) URL() string { return "ws://" + r.ln.Addr().String() }

// scheme is the upstream scheme for plain, "http" or "ws": its TLS form when
// LiveKit is reached over TLS.
func (r *Relay) scheme(plain string) string {
	if r.Secure {
		return plain + "s"
	}
	return plain
}

// ClientToken is the token the client presented, in the query or the
// Authorization header, where LiveKit reads it; "" when there is none.
func ClientToken(req *http.Request) string {
	if t := req.URL.Query().Get("access_token"); t != "" {
		return t
	}
	if t, ok := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer "); ok {
		return t
	}
	return ""
}

// swap puts token where the client put its own.
func (r *Relay) swap(u *url.URL, h http.Header, token string) {
	q := u.Query()
	if q.Has("access_token") {
		q.Set("access_token", token)
		u.RawQuery = q.Encode()
	}
	if h.Get("Authorization") != "" {
		h.Set("Authorization", "Bearer "+token)
	}
}

func (r *Relay) serveSocket(w http.ResponseWriter, req *http.Request, token string) {
	out := req.URL
	hdr := http.Header{}
	if a := req.Header.Get("Authorization"); a != "" {
		hdr.Set("Authorization", a)
	}
	u := *out
	u.Scheme, u.Host = r.scheme("ws"), r.Upstream
	r.swap(&u, hdr, token)
	up, resp, err := websocket.Dial(req.Context(), u.String(), &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		status := http.StatusBadGateway
		var body []byte
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<10))
				_ = resp.Body.Close()
			}
		}
		http.Error(w, string(body), status)
		return
	}
	// lk is not a browser and sends no Origin header; the default check then
	// has nothing to refuse, and a request that does carry one must be from
	// the loopback.
	down, err := websocket.Accept(w, req, &websocket.AcceptOptions{OriginPatterns: []string{"127.0.0.1:*", "localhost:*", "[::1]:*"}})
	if err != nil {
		_ = up.CloseNow()
		return
	}
	up.SetReadLimit(1 << 20)
	down.SetReadLimit(1 << 20)
	ctx, cancel := context.WithCancel(context.Background())
	pump := func(from, to *websocket.Conn, capture bool) {
		defer cancel()
		for {
			typ, data, err := from.Read(ctx)
			if err != nil {
				return
			}
			if !capture && r.Filter != nil {
				data = r.Filter(ctx, data)
			}
			if capture {
				if found := JWT.FindAll(data, -1); len(found) > 0 {
					r.mu.Lock()
					for _, f := range found {
						r.issued = append(r.issued, string(f))
					}
					r.mu.Unlock()
				}
			}
			if err := to.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}
	go pump(up, down, true)
	go pump(down, up, false)
	<-ctx.Done()
	_ = up.CloseNow()
	_ = down.CloseNow()
}

// Close stops the relay.
func (r *Relay) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.srv.Shutdown(ctx)
	_ = r.srv.Close()
}
