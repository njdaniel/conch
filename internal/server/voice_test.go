package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Obviously fake credentials: the tests assert they never appear in logs or
// the audit log.
const (
	voiceTestKey    = "devkey-not-real"
	voiceTestSecret = "voice-secret-not-real-0123456789"
	voiceTestURL    = "ws://voice.test:7880"
)

var voiceRoomNameRE = regexp.MustCompile(`^conch-[a-z2-7]{26}$`)

// fakeLiveKitServer stands in for LiveKit's room service. It counts and
// records every CreateRoom it receives and answers with status.
type fakeLiveKitServer struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	rooms  []string // room name of each CreateRoom, in order
	calls  int      // every request of any kind
	// body, when set, is sent instead of the usual answer.
	body string
	// onCreateRoom, when set, runs while a CreateRoom request is being
	// answered: the moment between the handler's checks and the token.
	onCreateRoom func()
}

func newFakeLiveKit(t *testing.T, status int) *fakeLiveKitServer {
	t.Helper()
	f := &fakeLiveKitServer{status: status}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		answer := `{}`
		if strings.HasSuffix(r.URL.Path, "/CreateRoom") {
			var req struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &req)
			f.rooms = append(f.rooms, req.Name)
			// LiveKit answers CreateRoom with the room.
			answer = fmt.Sprintf(`{"sid":"RM_test","name":%q}`, req.Name)
		}
		if f.onCreateRoom != nil && strings.HasSuffix(r.URL.Path, "/CreateRoom") {
			f.onCreateRoom()
		}
		if f.body != "" {
			answer = f.body
		}
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeLiveKitServer) requests() (calls int, rooms []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls, append([]string(nil), f.rooms...)
}

// voiceConfig points a livekit.Config at apiURL.
func voiceConfig(t *testing.T, apiURL string) livekit.Config {
	t.Helper()
	cfg, err := livekit.ParseConfig(voiceTestURL, apiURL, voiceTestKey, voiceTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// voiceFixture is a server with channels "ops" and "ops2" and these people:
//
//	ann, ann2    human members of ops (ann also of ops2)
//	bob          human, a member of nothing
//	root         the operator, a member of nothing
//	robo         an agent that is a member of ops with a full manifest
//	dora         a human member of ops who is then disabled
type voiceFixture struct {
	*authFixture
	lk     *fakeLiveKitServer
	logs   *logCapture
	ops    store.Channel
	ops2   store.Channel
	tokens map[string]string
	ids    map[string]int64
}

type voiceOpts struct {
	auth         AuthMode
	configured   bool
	livekitState int  // fake LiveKit status; 0 means 200
	unreachable  bool // point at a closed port instead
}

func newVoiceFixture(t *testing.T, o voiceOpts) *voiceFixture {
	t.Helper()
	logs := captureLogs(t)
	status := o.livekitState
	if status == 0 {
		status = http.StatusOK
	}
	lk := newFakeLiveKit(t, status)
	cfg := Config{AuthMode: o.auth}
	if o.configured {
		apiURL := lk.URL
		if o.unreachable {
			// A listener that was closed: connections are refused.
			dead := httptest.NewServer(http.NotFoundHandler())
			apiURL = dead.URL
			dead.Close()
		}
		cfg.LiveKit = voiceConfig(t, apiURL)
	}
	srv := newTestServerWithConfig(t, cfg)
	ctx := context.Background()
	root, _, rootTok, err := srv.store.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	f := &voiceFixture{
		authFixture: &authFixture{srv: srv, root: root, rootTok: rootTok},
		lk:          lk, logs: logs,
		tokens: map[string]string{"root": rootTok},
		ids:    map[string]int64{"root": root.ID},
	}
	if f.ops, err = srv.store.CreateChannel(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	if f.ops2, err = srv.store.CreateChannel(ctx, "ops2"); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		name string
		kind store.PrincipalKind
		in   []store.Channel
	}{
		{"ann", store.PrincipalHuman, []store.Channel{f.ops, f.ops2}},
		{"ann2", store.PrincipalHuman, []store.Channel{f.ops}},
		{"bob", store.PrincipalHuman, nil},
		{"robo", store.PrincipalAgent, []store.Channel{f.ops}},
		{"dora", store.PrincipalHuman, []store.Channel{f.ops}},
	} {
		p, err := srv.store.CreatePrincipal(ctx, spec.kind, spec.name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.tokens[spec.name], f.ids[spec.name] = tok, p.ID
		for _, ch := range spec.in {
			if _, err := srv.store.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	setAgentManifest(t, srv, f.ids["robo"], nil, f.ops.ID)
	if _, err := srv.store.DisablePrincipal(ctx, "system", f.ids["dora"]); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *voiceFixture) session(t *testing.T, who, channel string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPost, "/v1/channels/"+channel+"/voice/session", f.tokens[who], "")
}

func (f *voiceFixture) rowCount(t *testing.T) int {
	t.Helper()
	n, err := f.srv.store.CountVoiceRooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *voiceFixture) audits(t *testing.T, action string) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// decodeVoiceJWT checks the HS256 signature of a token against secret and
// returns its claims.
func decodeVoiceJWT(t *testing.T, token, secret string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)); !hmac.Equal([]byte(want), []byte(parts[2])) {
		t.Fatal("token signature does not verify against the configured secret")
	}
	var header map[string]any
	rawHeader, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(rawHeader, &header) != nil || header["alg"] != "HS256" {
		t.Fatalf("token header %q (%v) is not HS256", rawHeader, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

func decodeSession(t *testing.T, res wireResult) schema.VoiceSessionResponseV1 {
	t.Helper()
	if res.status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", res.status, res.body)
	}
	var resp schema.VoiceSessionResponseV1
	if err := json.Unmarshal([]byte(res.body), &resp); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	if err := resp.Validate(); err != nil {
		t.Fatalf("response does not validate: %v; body %s", err, res.body)
	}
	return resp
}

func errCode(t *testing.T, body string) string {
	t.Helper()
	var e schema.Error
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("decode error body %q: %v", body, err)
	}
	return e.Code
}

// TestVoiceSessionRefusals runs every refusal and checks that each one issues
// nothing: no session audit event, no token in the body, and (except where a
// room row or a LiveKit call is the point of the row) no room row and no
// request to LiveKit.
func TestVoiceSessionRefusals(t *testing.T) {
	tests := []struct {
		name       string
		opts       voiceOpts
		who        string
		channel    string
		wantStatus int
		wantCode   string
		// wantRows and wantLKCalls are 0 for every refusal made before conchd
		// reaches LiveKit. When LiveKit itself fails, the room row has been
		// created (the name is the durable thing, so it is stored before
		// CreateRoom is asked to make the room) and exactly one CreateRoom
		// was attempted; no token is signed.
		wantRows    int
		wantLKCalls int
		wantDenied  int // access_denied audit rows
	}{
		{"non-member", voiceOpts{auth: AuthRequired, configured: true}, "bob", "ops", 404, "channel_not_found", 0, 0, 0},
		{"unknown channel", voiceOpts{auth: AuthRequired, configured: true}, "ann", "nosuch", 404, "channel_not_found", 0, 0, 0},
		{"operator who is not a member", voiceOpts{auth: AuthRequired, configured: true}, "root", "ops", 404, "channel_not_found", 0, 0, 0},
		{"agent member", voiceOpts{auth: AuthRequired, configured: true}, "robo", "ops", 403, "forbidden", 0, 0, 1},
		{"agent not in channel", voiceOpts{auth: AuthRequired, configured: true}, "robo", "ops2", 404, "channel_not_found", 0, 0, 1},
		{"disabled principal", voiceOpts{auth: AuthRequired, configured: true}, "dora", "ops", 401, "unauthenticated", 0, 0, 0},
		{"unauthenticated", voiceOpts{auth: AuthRequired, configured: true}, "", "ops", 401, "unauthenticated", 0, 0, 0},
		{"auth off", voiceOpts{auth: AuthOff, configured: true}, "", "ops", 400, "voice_requires_auth", 0, 0, 0},
		{"auth off, unknown channel", voiceOpts{auth: AuthOff, configured: true}, "", "nosuch", 400, "voice_requires_auth", 0, 0, 0},
		{"not configured", voiceOpts{auth: AuthRequired}, "ann", "ops", 503, "voice_not_configured", 0, 0, 0},
		{"livekit errors", voiceOpts{auth: AuthRequired, configured: true, livekitState: 500}, "ann", "ops", 503, "voice_unavailable", 1, 1, 0},
		{"livekit unreachable", voiceOpts{auth: AuthRequired, configured: true, unreachable: true}, "ann", "ops", 503, "voice_unavailable", 1, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVoiceFixture(t, tt.opts)
			res := f.session(t, tt.who, tt.channel)
			if res.status != tt.wantStatus || errCode(t, res.body) != tt.wantCode {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if strings.Contains(res.body, `"token"`) || strings.Contains(res.body, `"rooms"`) {
				t.Errorf("a refusal carries session fields: %s", res.body)
			}
			if got := f.rowCount(t); got != tt.wantRows {
				t.Errorf("voice_rooms rows = %d, want %d", got, tt.wantRows)
			}
			if calls, _ := f.lk.requests(); calls != tt.wantLKCalls {
				t.Errorf("requests reaching LiveKit = %d, want %d", calls, tt.wantLKCalls)
			}
			if got := len(f.audits(t, store.AuditVoiceSessionIssued)); got != 0 {
				t.Errorf("voice_session_issued events = %d, want 0", got)
			}
			if got := len(f.audits(t, "access_denied")); got != tt.wantDenied {
				t.Errorf("access_denied events = %d, want %d", got, tt.wantDenied)
			}
		})
	}
}

// TestVoiceSessionNotFoundIsIdentical: a non-member, an unknown channel and an
// operator who is not a member all get the same bytes.
func TestVoiceSessionNotFoundIsIdentical(t *testing.T) {
	f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
	want := f.session(t, "ann", "nosuch")
	for _, c := range []struct{ who, channel string }{{"bob", "ops"}, {"root", "ops"}, {"bob", "nosuch"}} {
		got := f.session(t, c.who, c.channel)
		if got != want {
			t.Errorf("%s on %s: %+v differs from the unknown-channel answer %+v", c.who, c.channel, got, want)
		}
	}
	if want.status != 404 || errCode(t, want.body) != "channel_not_found" {
		t.Errorf("unknown channel answer = %+v", want)
	}
}

// TestVoiceSessionMembershipComesFirst: a non-member gets the same 404
// whether or not voice is configured and whether or not LiveKit is up, and
// nothing reaches LiveKit.
func TestVoiceSessionMembershipComesFirst(t *testing.T) {
	var first wireResult
	i := 0
	for _, o := range []voiceOpts{
		{auth: AuthRequired, configured: true},                    // configured, up
		{auth: AuthRequired, configured: true, livekitState: 500}, // configured, erroring
		{auth: AuthRequired, configured: true, unreachable: true}, // configured, down
		{auth: AuthRequired, configured: false},                   // not configured
	} {
		f := newVoiceFixture(t, o)
		res := f.session(t, "bob", "ops")
		if res.status != 404 || errCode(t, res.body) != "channel_not_found" {
			t.Fatalf("opts %+v: got %d %s", o, res.status, res.body)
		}
		if i == 0 {
			first = res
		} else if res != first {
			t.Errorf("opts %+v: %+v differs from %+v", o, res, first)
		}
		i++
		if calls, _ := f.lk.requests(); calls != 0 {
			t.Errorf("opts %+v: %d requests reached LiveKit", o, calls)
		}
		if f.rowCount(t) != 0 {
			t.Errorf("opts %+v: a room row was created for a non-member", o)
		}
	}
}

func TestVoiceSessionGrantsTheMember(t *testing.T) {
	f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
	before := time.Now()
	res := f.session(t, "ann", "ops")
	after := time.Now()
	resp := decodeSession(t, res)

	if resp.LivekitURL != voiceTestURL {
		t.Errorf("livekit_url = %q, want %q", resp.LivekitURL, voiceTestURL)
	}
	wantIdentity := "p" + itoa(f.ids["ann"])
	if resp.Identity != wantIdentity {
		t.Errorf("identity = %q, want %q", resp.Identity, wantIdentity)
	}
	if len(resp.Rooms) != 1 {
		t.Fatalf("grants = %d, want 1", len(resp.Rooms))
	}
	g := resp.Rooms[0]
	stored, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.Room != stored.RoomName || !voiceRoomNameRE.MatchString(g.Room) {
		t.Errorf("room = %q, stored %q", g.Room, stored.RoomName)
	}
	if !g.CanPublish || g.Audience != nil {
		t.Errorf("grant = %+v, want a publishing channel-wide grant", g)
	}

	claims := decodeVoiceJWT(t, g.Token, voiceTestSecret)
	video, _ := claims["video"].(map[string]any)
	nbf, exp := int64(claims["nbf"].(float64)), int64(claims["exp"].(float64))
	checks := []struct {
		name string
		ok   bool
	}{
		{"iss is the configured key", claims["iss"] == voiceTestKey},
		{"sub is the identity", claims["sub"] == wantIdentity},
		{"room join for the stored room", video["roomJoin"] == true && video["room"] == stored.RoomName},
		{"can publish and subscribe", video["canPublish"] == true && video["canSubscribe"] == true},
		{"no data publishing", video["canPublishData"] == false},
		{"microphone only", len(video["canPublishSources"].([]any)) == 1 && video["canPublishSources"].([]any)[0] == "microphone"},
		{"expires 15 seconds after issue", exp-nbf == 15},
		{"not-before is the issue time, not backdated", nbf >= before.Unix() && nbf <= after.Unix()},
		{"expiry is within the call window", exp >= before.Unix()+15 && exp <= after.Unix()+15},
		{"expires_at is never later than the token's expiry", g.ExpiresAt.Time().Unix() <= exp && exp-g.ExpiresAt.Time().Unix() <= 1},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s: claims %v, expires_at %v", c.name, claims, g.ExpiresAt)
		}
	}
	if _, rooms := f.lk.requests(); len(rooms) != 1 || rooms[0] != stored.RoomName {
		t.Errorf("CreateRoom calls = %v, want one for %q", rooms, stored.RoomName)
	}
}

func itoa(n int64) string { return voiceIdentity(n)[1:] }

// TestVoiceSessionRooms: repeated calls share a room and each calls
// CreateRoom; tokens differ; another channel gets another room.
func TestVoiceSessionRooms(t *testing.T) {
	f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
	// No pause between the calls: a token carries a random id, so two issued
	// in the same second for the same principal and room still differ.
	a1 := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0]
	a2 := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0]
	b := decodeSession(t, f.session(t, "ann2", "ops")).Rooms[0]
	other := decodeSession(t, f.session(t, "ann", "ops2")).Rooms[0]

	if a1.Room != a2.Room || a1.Room != b.Room {
		t.Errorf("rooms for one channel differ: %q %q %q", a1.Room, a2.Room, b.Room)
	}
	if a1.Token == a2.Token {
		t.Error("two calls returned the same token")
	}
	if a1.Token == b.Token {
		t.Error("two principals got the same token")
	}
	if other.Room == a1.Room {
		t.Errorf("two channels share room %q", other.Room)
	}
	if f.rowCount(t) != 2 {
		t.Errorf("rows = %d, want 2", f.rowCount(t))
	}
	_, rooms := f.lk.requests()
	if want := []string{a1.Room, a1.Room, a1.Room, other.Room}; strings.Join(rooms, ",") != strings.Join(want, ",") {
		t.Errorf("CreateRoom calls = %v, want %v", rooms, want)
	}
	if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 4 {
		t.Errorf("voice_session_issued = %d, want 4 (one per response)", n)
	}
}

func TestVoiceSessionConcurrentFirstCalls(t *testing.T) {
	f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
	const n = 20
	results := make([]wireResult, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// callREST uses t.Fatalf on a setup failure only; the request itself is plain.
			results[i] = f.callREST(t, http.MethodPost, "/v1/channels/ops/voice/session", f.tokens["ann"], "")
		}()
	}
	wg.Wait()
	room := ""
	for i, res := range results {
		g := decodeSession(t, res).Rooms[0]
		if i == 0 {
			room = g.Room
		}
		if g.Room != room {
			t.Errorf("call %d named room %q, call 0 named %q", i, g.Room, room)
		}
	}
	if got := f.rowCount(t); got != 1 {
		t.Errorf("voice_rooms rows = %d, want exactly 1", got)
	}
}

// TestVoiceSessionAuditAndLogs: exactly one voice_session_issued with the
// channel and the grant, and no token or secret in any audit row or log line.
func TestVoiceSessionAuditAndLogs(t *testing.T) {
	f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
	resp := decodeSession(t, f.session(t, "ann", "ops"))
	g := resp.Rooms[0]

	events := f.audits(t, store.AuditVoiceSessionIssued)
	if len(events) != 1 {
		t.Fatalf("voice_session_issued events = %d, want 1", len(events))
	}
	e := events[0]
	if e.Actor != "principal:"+itoa(f.ids["ann"]) || e.Subject != "channel:"+itoa(f.ops.ID) {
		t.Errorf("event actor/subject = %q / %q", e.Actor, e.Subject)
	}
	for _, want := range []string{"channel=" + itoa(f.ops.ID), "identity=" + resp.Identity, "grants=channel:publish"} {
		if !strings.Contains(e.Detail, want) {
			t.Errorf("detail %q lacks %q", e.Detail, want)
		}
	}

	sig := g.Token[strings.LastIndex(g.Token, ".")+1:]
	secrets := []struct{ what, text string }{
		{"token", g.Token}, {"token signature", sig}, {"API secret", voiceTestSecret},
	}
	logs := f.logs.buf.String()
	if !strings.Contains(logs, "voice: session issued") || !strings.Contains(logs, "voice: configured") {
		t.Fatalf("log capture is not seeing the voice log lines:\n%s", logs)
	}
	for _, s := range secrets {
		if strings.Contains(logs, s.text) {
			t.Errorf("a log line contains the %s", s.what)
		}
		for _, ev := range f.audit(t) {
			if strings.Contains(ev.Actor+ev.Action+ev.Subject+ev.Detail, s.text) {
				t.Errorf("audit event %d contains the %s", ev.ID, s.what)
			}
		}
	}
}

// TestVoiceSessionLiveKitFailureLogsNoSecret covers the failure path's logs.
func TestVoiceSessionLiveKitFailureLogsNoSecret(t *testing.T) {
	for _, o := range []voiceOpts{
		{auth: AuthRequired, configured: true, livekitState: 500},
		{auth: AuthRequired, configured: true, unreachable: true},
	} {
		f := newVoiceFixture(t, o)
		res := f.session(t, "ann", "ops")
		if res.status != 503 {
			t.Fatalf("status = %d", res.status)
		}
		if !strings.Contains(f.logs.buf.String(), "voice: livekit unavailable") {
			t.Errorf("failure was not logged:\n%s", f.logs.buf.String())
		}
		for _, s := range []string{voiceTestSecret, "eyJ"} {
			if strings.Contains(f.logs.buf.String(), s) {
				t.Errorf("failure logs contain %q", s)
			}
		}
	}
}

// A caller who stops being entitled while LiveKit is being asked gets no
// token. CreateRoom can take seconds; a removal, a disable or a sign-out that
// lands in that time must be answered like one that came before the request.
func TestVoiceSessionRechecksBeforeSigning(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		lose func(f *voiceFixture) error
	}{
		{"removed from the channel", func(f *voiceFixture) error {
			_, err := f.srv.store.RemoveChannelMember(ctx, "system", f.ops.ID, f.ids["ann"])
			return err
		}},
		{"principal disabled", func(f *voiceFixture) error {
			_, err := f.srv.store.DisablePrincipal(ctx, "system", f.ids["ann"])
			return err
		}},
		{"all credentials revoked", func(f *voiceFixture) error {
			_, err := f.srv.store.RevokeAllCredentials(ctx, "system", f.ids["ann"])
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
			f.lk.onCreateRoom = func() {
				if err := tt.lose(f); err != nil {
					t.Errorf("lose entitlement: %v", err)
				}
			}
			res := f.session(t, "ann", "ops")
			if res.status != http.StatusNotFound || errCode(t, res.body) != "channel_not_found" {
				t.Fatalf("answer = %d %s, want the unknown-channel 404", res.status, res.body)
			}
			if strings.Contains(res.body, "token") || strings.Contains(res.body, "eyJ") {
				t.Errorf("the refusal carries a token: %s", res.body)
			}
			if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 0 {
				t.Errorf("voice_session_issued = %d, want 0", n)
			}
			// The answer is the same bytes a non-member gets.
			if other := f.session(t, "bob", "ops"); other.body != res.body {
				t.Errorf("differs from a non-member's answer:\n %s\n %s", res.body, other.body)
			}
		})
	}
}

// The response carries bearer tokens, so it must not be cached; refusals carry
// nothing and need no such header.
func TestVoiceSessionResponseIsNotCacheable(t *testing.T) {
	f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
	rec := f.do(t, http.MethodPost, "/v1/channels/ops/voice/session", f.tokens["ann"], "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("status %d, Cache-Control %q; want 200 and no-store", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

// A 200 from LiveKit's address that does not name the room is not LiveKit
// agreeing the room exists: no token is issued.
func TestVoiceSessionNeedsLiveKitToNameTheRoom(t *testing.T) {
	for _, body := range []string{`<html>welcome</html>`, `{}`, `{"name":"conch-someone-elses-room"}`} {
		f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
		f.lk.body = body
		res := f.session(t, "ann", "ops")
		if res.status != http.StatusServiceUnavailable || errCode(t, res.body) != schema.ErrorCodeVoiceUnavailable {
			t.Errorf("answer to LiveKit body %q = %d %s, want 503 voice_unavailable", body, res.status, res.body)
		}
		if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 0 {
			t.Errorf("LiveKit body %q: voice_session_issued = %d, want 0", body, n)
		}
	}
}

// A caller who hangs up cancels the LiveKit call. That is not an outage, and
// any member could otherwise fill the log with lines that look like one.
func TestVoiceSessionCancelledCallerIsNotLoggedAsAnOutage(t *testing.T) {
	f := newVoiceFixture(t, voiceOpts{auth: AuthRequired, configured: true})
	ctx, cancel := context.WithCancel(context.Background())
	f.lk.onCreateRoom = cancel
	req := httptest.NewRequest(http.MethodPost, "/v1/channels/ops/voice/session", nil).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.tokens["ann"])
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	cancel()
	if strings.Contains(rec.Body.String(), "eyJ") {
		t.Errorf("a cancelled request was answered with a token: %s", rec.Body)
	}
	if n := len(f.audits(t, store.AuditVoiceSessionIssued)); n != 0 {
		t.Errorf("voice_session_issued = %d, want 0", n)
	}
	if logs := f.logs.buf.String(); strings.Contains(logs, "livekit unavailable") {
		t.Errorf("a cancelled request is logged as an outage:\n%s", logs)
	}
	// And the store is intact: the next caller is served.
	if res := f.session(t, "ann2", "ops"); res.status != http.StatusOK {
		t.Errorf("the next session = %d %s", res.status, res.body)
	}
}
