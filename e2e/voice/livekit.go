package main

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

// lkAdmin asks LiveKit directly, with the API secret the harness chose, for
// ground truth that does not pass through conchd or any client: who is in a
// room, which rooms exist, and the server-side mute that stands in for a key
// press. It does not use conchd's own LiveKit client, so a defect there cannot
// make this program agree with itself.
type lkAdmin struct {
	base   string // http://127.0.0.1:port
	key    string
	secret string
	http   *http.Client
}

func newLKAdmin(base, key, secret string) *lkAdmin {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &lkAdmin{base: base, key: key, secret: secret, http: &http.Client{Transport: tr, Timeout: 5 * time.Second}}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// token signs an HS256 admin token carrying the given video grant.
func (a *lkAdmin) token(video map[string]any) (string, error) {
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

func (a *lkAdmin) call(ctx context.Context, method string, video map[string]any, req, out any) error {
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
		return fmt.Errorf("livekit %s: %w", method, unwrapURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("livekit %s: %w", method, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &lkStatusError{method: method, status: resp.StatusCode, body: truncate(string(data), 200)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// lkStatusError is LiveKit answering an admin call with something other than
// 200, kept apart from a call that got no answer: "the room does not exist"
// is a fact about LiveKit, "the call failed" is not.
type lkStatusError struct {
	method string
	status int
	body   string
}

func (e *lkStatusError) Error() string {
	return fmt.Sprintf("livekit %s: HTTP %d: %s", e.method, e.status, e.body)
}

// roomNotFound reports whether err is LiveKit saying the room does not exist.
func roomNotFound(err error) bool {
	var se *lkStatusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// unwrapURLError drops the URL from a transport error; the URL of an admin
// call is harmless but the habit keeps credentials out of messages.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

type lkTrack struct {
	SID    string `json:"sid"`
	Type   any    `json:"type"`
	Source any    `json:"source"`
	Muted  bool   `json:"muted"`
}

// isMicrophone is true for an audio track whose source is the microphone.
// protojson omits defaults, so an absent type is audio.
func (t lkTrack) isMicrophone() bool {
	typ := fmt.Sprint(t.Type)
	src := fmt.Sprint(t.Source)
	return (t.Type == nil || typ == "AUDIO" || typ == "0") && (src == "MICROPHONE" || src == "2")
}

type lkParticipant struct {
	Identity string    `json:"identity"`
	State    any       `json:"state"`
	Tracks   []lkTrack `json:"tracks"`
}

// mic returns the participant's microphone track, if it has one.
func (p lkParticipant) mic() (lkTrack, bool) {
	for _, t := range p.Tracks {
		if t.isMicrophone() {
			return t, true
		}
	}
	return lkTrack{}, false
}

func (a *lkAdmin) listParticipants(ctx context.Context, room string) ([]lkParticipant, error) {
	var resp struct {
		Participants []lkParticipant `json:"participants"`
	}
	err := a.call(ctx, "ListParticipants", map[string]any{"roomAdmin": true, "room": room}, map[string]any{"room": room}, &resp)
	return resp.Participants, err
}

func (a *lkAdmin) listRooms(ctx context.Context) ([]string, error) {
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

// mintJoin signs a join token for identity into room with the API secret this
// run chose: a participant conchd never issued a token to, as someone holding a
// leaked secret could make. The token lives a minute.
func (a *lkAdmin) mintJoin(identity, room string) (string, error) {
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

func (a *lkAdmin) mute(ctx context.Context, room, identity, sid string, muted bool) error {
	return a.call(ctx, "MutePublishedTrack", map[string]any{"roomAdmin": true, "room": room},
		map[string]any{"room": room, "identity": identity, "track_sid": sid, "muted": muted}, nil)
}

// ----------------------------------------------- signalling-only client

// jwtRE finds a JWT inside a binary signalling frame.
var jwtRE = regexp.MustCompile(`eyJ[\w-]+\.[\w-]+\.[\w-]+`)

// signalConn is a participant that has connected to LiveKit's signalling
// WebSocket with a token and does nothing else. LiveKit counts it as joined;
// it cannot publish. Its job here is to be a connected participant (the
// outsider, the removed member) and to hear the token LiveKit sends every
// connected participant of its own (design note §10, finding 9).
type signalConn struct {
	c      *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}

	mu       sync.Mutex
	issued   []string // JWTs found in frames from LiveKit, in order
	closeErr error
}

// dialSignal connects to LiveKit's signalling endpoint. On a refused join it
// returns the HTTP status and body LiveKit answered with.
func dialSignal(ctx context.Context, wsBase, token string) (*signalConn, int, string, error) {
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
	s := &signalConn{c: c, cancel: cancel, done: make(chan struct{})}
	go s.readLoop(cctx)
	return s, http.StatusSwitchingProtocols, "", nil
}

func (s *signalConn) readLoop(ctx context.Context) {
	defer close(s.done)
	for {
		_, data, err := s.c.Read(ctx)
		if err != nil {
			s.mu.Lock()
			s.closeErr = err
			s.mu.Unlock()
			return
		}
		if found := jwtRE.FindAll(data, -1); len(found) > 0 {
			s.mu.Lock()
			for _, f := range found {
				s.issued = append(s.issued, string(f))
			}
			s.mu.Unlock()
		}
	}
}

// issuedTokens returns the JWTs LiveKit has sent so far.
func (s *signalConn) issuedTokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.issued...)
}

func (s *signalConn) closed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *signalConn) Close() {
	s.cancel()
	_ = s.c.CloseNow()
	<-s.done
}

// -------------------------------------------- token-substituting relay

// relay is a signalling proxy between `lk` and LiveKit. `lk room join` signs
// its own join token from an API key and secret and has no option to use a
// token it is given, which would bypass conchd and prove nothing. So the
// relay sits in the middle: `lk` signs a token with a throwaway key LiveKit
// has never heard of, and the relay replaces it with the token conchd issued
// before the request reaches LiveKit. LiveKit therefore admits the
// participant, or not, on conchd's token alone. The relay also reads what
// LiveKit sends back, which is how the harness gets the token LiveKit itself
// issues a connected participant.
type relay struct {
	srv      *http.Server
	ln       net.Listener
	upstream string // host:port of LiveKit
	token    string // the token conchd issued

	mu     sync.Mutex
	issued []string
}

// issuedTokens returns the JWTs LiveKit sent the participant behind the relay.
func (r *relay) issuedTokens() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.issued...)
}

func startRelay(upstreamHostPort, conchdToken string) (*relay, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	r := &relay{ln: ln, upstream: upstreamHostPort, token: conchdToken}
	rp := &httputil.ReverseProxy{
		// lk retries after LiveKit stops; that is expected, not output.
		ErrorLog: log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = r.upstream
			pr.Out.Host = r.upstream
			r.swap(pr.Out.URL, pr.Out.Header)
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		if strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
			r.serveSocket(w, req)
			return
		}
		rp.ServeHTTP(w, req)
	})
	r.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = r.srv.Serve(ln) }()
	return r, nil
}

func (r *relay) url() string { return "ws://" + r.ln.Addr().String() }

// swap puts conchd's token where the client put its own.
func (r *relay) swap(u *url.URL, h http.Header) {
	q := u.Query()
	if q.Has("access_token") {
		q.Set("access_token", r.token)
		u.RawQuery = q.Encode()
	}
	if h.Get("Authorization") != "" {
		h.Set("Authorization", "Bearer "+r.token)
	}
}

func (r *relay) serveSocket(w http.ResponseWriter, req *http.Request) {
	out := req.URL
	hdr := http.Header{}
	if a := req.Header.Get("Authorization"); a != "" {
		hdr.Set("Authorization", a)
	}
	u := *out
	u.Scheme, u.Host = "ws", r.upstream
	r.swap(&u, hdr)
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
			if capture {
				if found := jwtRE.FindAll(data, -1); len(found) > 0 {
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

func (r *relay) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = r.srv.Shutdown(ctx)
	_ = r.srv.Close()
}
