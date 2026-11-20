package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const privateCallTestSecret = "private-call-secret-not-real-0123456789"

type fakePrivateCallMedia struct {
	mu           sync.Mutex
	rooms        map[string][]livekit.Participant
	signer       *livekit.Client
	signed       []livekit.JoinParams
	createHook   func()
	listHook     func()
	deleted      chan struct{}
	deleteFailed bool
}

func (m *fakePrivateCallMedia) CreateRoom(_ context.Context, name string) error {
	if m.createHook != nil {
		m.createHook()
	}
	m.mu.Lock()
	if _, ok := m.rooms[name]; !ok {
		m.rooms[name] = nil
	}
	m.mu.Unlock()
	return nil
}

func (m *fakePrivateCallMedia) JoinToken(p livekit.JoinParams) (string, error) {
	m.signed = append(m.signed, p)
	return m.signer.JoinToken(p)
}

func (m *fakePrivateCallMedia) ListParticipantsIfRoomExists(_ context.Context, name string) ([]livekit.Participant, error) {
	if m.listHook != nil {
		m.listHook()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rooms[name], nil
}

func (m *fakePrivateCallMedia) RemoveParticipant(_ context.Context, name, identity string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, part := range m.rooms[name] {
		if part.Identity == identity {
			// ListParticipants' result is a snapshot owned by the caller.
			parts := append([]livekit.Participant(nil), m.rooms[name][:i]...)
			m.rooms[name] = append(parts, m.rooms[name][i+1:]...)
			break
		}
	}
	return nil
}

func (m *fakePrivateCallMedia) DeleteRoom(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleteFailed {
		return errors.New("test delete failed")
	}
	delete(m.rooms, name)
	if m.deleted != nil {
		close(m.deleted)
		m.deleted = nil
	}
	return nil
}

func addFakePrivateCallMedia(t *testing.T, s *Server) *fakePrivateCallMedia {
	t.Helper()
	s.cfg.LiveKit = voiceConfig(t, "http://unused.test")
	s.cfg.LiveKit.APISecret = livekit.NewSecret(privateCallTestSecret)
	signer, err := livekit.New(s.cfg.LiveKit)
	if err != nil {
		t.Fatal(err)
	}
	m := &fakePrivateCallMedia{signer: signer, rooms: make(map[string][]livekit.Participant)}
	s.voice.callMedia = m
	return m
}

func invitePrivateCallMediaTest(t *testing.T, f *authFixture, id string) schema.PrivateCallV1 {
	t.Helper()
	return decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair, f.aliceTok,
		`{"action_id":"invite-`+id+`"}`), http.StatusCreated).Call
}

func answerPrivateCallMediaTest(t *testing.T, f *authFixture, call schema.PrivateCallV1, token, action string) schema.PrivateCallV1 {
	t.Helper()
	return decodePrivateCallResponse(t, f.do(t, "POST", "/v1/calls/"+call.ID+"/accept",
		token, privateCallTestCommand(call, action)), http.StatusOK).Call
}

func privateCallSessionTest(t *testing.T, rec *httptest.ResponseRecorder) schema.PrivateCallSessionV1 {
	t.Helper()
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("session response: %d, cache control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	var session schema.PrivateCallSessionV1
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatal("session did not decode")
	}
	if err := session.Validate(); err != nil {
		t.Fatal(err)
	}
	return session
}

func TestPrivateCallMediaSelectedEndpoints(t *testing.T) {
	f, bob, bobTok, sibling := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	call := invitePrivateCallMediaTest(t, f, "first")
	path := "/v1/calls/" + call.ID + "/session"
	for _, test := range []struct {
		name, token string
		status      int
	}{
		{"anonymous", "", http.StatusUnauthorized},
		{"outsider", f.rootTok, http.StatusNotFound},
		{"agent", f.botTok, http.StatusForbidden},
		{"pre-answer caller", f.aliceTok, http.StatusConflict},
		{"pre-answer callee", bobTok, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := f.do(t, "POST", path, test.token, ""); got.Code != test.status {
				t.Fatalf("status = %d, want %d", got.Code, test.status)
			}
		})
	}
	call = answerPrivateCallMediaTest(t, f, call, bobTok, "answer")
	refused := f.do(t, "POST", path, sibling, "")
	if refused.Code != http.StatusForbidden || errCode(t, refused.Body.String()) != schema.ErrorCodeCallWrongDevice {
		t.Fatalf("sibling device: %d, %s", refused.Code, refused.Body)
	}
	if len(media.rooms) != 0 || len(media.signed) != 0 {
		t.Fatal("an invite, answer or refusal contacted the media service")
	}
	aliceSession := privateCallSessionTest(t, f.do(t, "POST", path, f.aliceTok, ""))
	bobSession := privateCallSessionTest(t, f.do(t, "POST", path, bobTok, ""))
	if aliceSession.Room != bobSession.Room || !voiceRoomNameRE.MatchString(aliceSession.Room) ||
		aliceSession.Identity != voiceIdentity(f.alice.ID) || bobSession.Identity != voiceIdentity(bob.ID) {
		t.Fatal("selected endpoints must receive distinct identities in one fresh room")
	}
	for _, session := range []schema.PrivateCallSessionV1{aliceSession, bobSession} {
		claims := decodeVoiceJWT(t, session.Token, privateCallTestSecret)
		want := map[string]any{"roomJoin": true, "room": session.Room, "canSubscribe": true,
			"canPublish": true, "canPublishData": false, "canPublishSources": []any{"microphone"}}
		if !reflect.DeepEqual(claims["video"], want) || claims["sub"] != session.Identity ||
			claims["exp"].(float64)-claims["nbf"].(float64) != voiceTokenLifetime.Seconds() {
			t.Fatal("unexpected private call claims")
		}
	}
	media.rooms[aliceSession.Room] = []livekit.Participant{
		{Identity: aliceSession.Identity}, {Identity: bobSession.Identity + voiceSuffixScreen},
		{Identity: bobSession.Identity + voiceSuffixView},
		{Identity: bobSession.Identity, Mismatched: []livekit.Track{{Type: "video", Source: "microphone"}}},
	}
	f.srv.voice.runPrivateCallMediaPass(t.Context())
	if len(media.rooms[aliceSession.Room]) != 1 {
		t.Fatal("companions and video published as microphone must be evicted")
	}
	current := f.do(t, "GET", "/v1/calls/current", f.aliceTok, "")
	var snapshot schema.PrivateCallSnapshotV1
	if err := json.Unmarshal(current.Body.Bytes(), &snapshot); err != nil || snapshot.Call == nil ||
		snapshot.Call.State != schema.PrivateCallStateAccepted {
		t.Fatalf("a removed participant counted as an endpoint: %s", current.Body)
	}
	media.rooms[aliceSession.Room] = append(media.rooms[aliceSession.Room], livekit.Participant{Identity: bobSession.Identity})
	f.srv.voice.runPrivateCallMediaPass(t.Context())
	current = f.do(t, "GET", "/v1/calls/current", f.aliceTok, "")
	if err := json.Unmarshal(current.Body.Bytes(), &snapshot); err != nil || snapshot.Call == nil ||
		snapshot.Call.State != schema.PrivateCallStateActive {
		t.Fatalf("selected endpoints did not activate the attempt: %s", current.Body)
	}
	f.do(t, "POST", "/v1/calls/"+call.ID+"/hangup", f.aliceTok, privateCallTestCommand(*snapshot.Call, "hangup"))
	ended := f.do(t, "POST", path, f.aliceTok, "")
	if ended.Code != http.StatusConflict || len(media.signed) != 2 {
		t.Fatal("ended call could receive a grant")
	}
}

func TestPrivateCallMediaRevocationDuringCreate(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer")
	path := "/v1/calls/" + call.ID + "/session"
	alice := privateCallSessionTest(t, f.do(t, "POST", path, f.aliceTok, ""))
	media.rooms[alice.Room] = []livekit.Participant{{Identity: alice.Identity}}
	media.createHook = func() {
		doc, err := f.srv.store.Permissions(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.srv.store.PutPermissions(t.Context(), fmt.Sprintf("principal:%d", f.root.ID), f.root.ID,
			schema.PutPermissionsRequestV1{Revision: doc.Revision, Changes: []schema.PermissionChangeV1{
				{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
				{Permission: schema.PermUseCamera, Role: schema.PermissionRoleMember, Granted: false},
				{Permission: schema.PermUseVoice, Role: schema.PermissionRoleMember, Granted: false},
			}}, f.srv.now())
		if err != nil {
			t.Fatal(err)
		}
	}
	// Bob passed authentication before CreateRoom. The write revokes voice
	// for both endpoints before the signer is called again.
	rec := f.do(t, "POST", path, bobTok, "")
	if rec.Code != http.StatusForbidden || errCode(t, rec.Body.String()) != schema.ErrorCodeVoiceNotAllowed ||
		len(media.signed) != 1 {
		t.Fatalf("revoked voice: %d, %s", rec.Code, rec.Body)
	}
	f.srv.voice.runPrivateCallMediaPass(t.Context())
	if len(media.rooms) != 0 {
		t.Fatal("revoked room and its connected endpoint survived cleanup")
	}
}

func TestPrivateCallMediaPollSeesLatestHolders(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer")
	path := "/v1/calls/" + call.ID + "/session"
	alice := privateCallSessionTest(t, f.do(t, "POST", path, f.aliceTok, ""))
	media.listHook = func() {
		bob := privateCallSessionTest(t, f.do(t, "POST", path, bobTok, ""))
		media.rooms[alice.Room] = []livekit.Participant{{Identity: alice.Identity}, {Identity: bob.Identity}}
	}
	// Take the worklist snapshot, then issue Bob's first grant and connect
	// both endpoints while ListParticipants is in progress.
	rooms, err := f.srv.store.PrivateCallMediaRooms(t.Context())
	if err != nil || len(rooms) != 1 {
		t.Fatalf("media worklist: %d rooms, %v", len(rooms), err)
	}
	if err := f.srv.voice.reconcilePrivateCallRoom(t.Context(), rooms[0]); err != nil {
		t.Fatal(err)
	}
	if len(media.rooms[alice.Room]) != 2 {
		t.Fatal("poll evicted a newly granted endpoint")
	}
}

func TestPrivateCallMediaDeletionWaitsForCreate(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer")
	path := "/v1/calls/" + call.ID
	session := privateCallSessionTest(t, f.do(t, "POST", path+"/session", f.aliceTok, ""))
	creating, finish := make(chan struct{}), make(chan struct{})
	deleted := make(chan struct{})
	media.deleted = deleted
	media.createHook = func() {
		close(creating)
		<-finish
	}
	result := make(chan int, 1)
	go func() { result <- f.do(t, "POST", path+"/session", bobTok, "").Code }()
	<-creating
	hangup := f.do(t, "POST", path+"/hangup", f.aliceTok, privateCallTestCommand(call, "hangup"))
	cleanup := make(chan struct{})
	go func() {
		f.srv.voice.runPrivateCallMediaPass(t.Context())
		close(cleanup)
	}()
	select {
	case <-deleted:
		t.Error("deletion raced ahead of an in-flight create")
	case <-time.After(25 * time.Millisecond):
	}
	close(finish)
	if status := <-result; hangup.Code != http.StatusOK || status != http.StatusConflict {
		t.Errorf("hangup = %d, in-flight session = %d", hangup.Code, status)
	}
	<-cleanup
	if _, ok := media.rooms[session.Room]; ok {
		t.Error("the in-flight create left a retired room in LiveKit")
	}
}

// Real-service verification (LiveKit 1.13.7, synchronized clocks):
// Record grant-to-join, revocation-commit-to-disconnect and stale-rejoin-to-
// deletion. Existing measurements in docs/design/voice-control-plane.md §10
// found that an unused grant was accepted 55 s past exp and refused at 65 s
// past exp. With our 15 s grant, expect about 75 s from minting. Measure the
// refresh token separately: LiveKit issued a 600 s refresh token on connect,
// so the original token's expiry does not bound a client that already joined.
// The earlier channel-room revocation test (§10, finding 10) measured that
// with auto-create disabled, LiveKit refused its own refresh token after
// DeleteRoom. Expect the same for private calls. With auto-create enabled,
// either token can recreate the retired room and get another refresh token. Expect each recreation to be disconnected
// by the next successful room sweep (30 s cadence plus RPC/queue time).
// For a healthy, unsaturated service, expect retirement to begin within the
// 1 s call state sweep and 1 s media pass, plus up to 4 s for room control.
// Repeat with both endpoints connected, media unavailable at revocation and
// restored later, and with conchd restarted before cleanup can succeed.
func TestPrivateCallMediaRetirementRetriesAndStaleRejoin(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer-first")
	old := privateCallSessionTest(t, f.do(t, "POST", "/v1/calls/"+call.ID+"/session", f.aliceTok, ""))
	media.rooms[old.Room] = []livekit.Participant{{Identity: old.Identity}}
	if err := f.srv.store.RestartPrivateCalls(t.Context(), f.srv.now()); err != nil {
		t.Fatal(err)
	}
	media.deleteFailed = true
	f.srv.voice.runPrivateCallMediaPass(t.Context())
	if len(media.rooms[old.Room]) != 1 {
		t.Fatal("test did not leave a room to retry")
	}
	// A fresh poller has only the durable worklist from the prior server.
	path, _ := testDBPaths.Load(f.srv)
	reopened, err := store.Open(t.Context(), path.(string))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := New(f.srv.cfg, reopened)
	poller := restarted.voice
	poller.callMedia = media
	media.deleteFailed = false
	poller.runPrivateCallMediaPass(t.Context())
	if _, ok := media.rooms[old.Room]; ok {
		t.Fatal("cleanup retry did not disconnect the endpoint and remove the room")
	}
	if rooms, err := f.srv.store.PrivateCallMediaRooms(t.Context()); err != nil || len(rooms) != 0 {
		t.Fatalf("completed deletion is still pending: %d rooms, %v", len(rooms), err)
	}
	for _, st := range []*store.Store{f.srv.store, reopened} {
		if err := st.SetVoiceRoomPrefix("replacement-"); err != nil {
			t.Fatal(err)
		}
	}
	next := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "next"), bobTok, "answer-next")
	fresh := privateCallSessionTest(t, f.do(t, "POST", "/v1/calls/"+next.ID+"/session", f.aliceTok, ""))
	if fresh.Room == old.Room {
		t.Fatal("new attempt reused the retired room")
	}
	// Simulate LiveKit auto-creating the old room on a stale token. Its old
	// prefix no longer matches, so only the tombstone can find it.
	media.rooms[old.Room] = []livekit.Participant{{Identity: old.Identity}}
	lk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/twirp/livekit.RoomService/ListRooms" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rooms": []livekit.Room{{Name: old.Room}, {Name: fresh.Room}}})
	}))
	defer lk.Close()
	restarted.lk, err = livekit.New(voiceConfig(t, lk.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !poller.runSweep(t.Context()) {
		t.Fatal("orphan sweep did not run")
	}
	if _, oldExists := media.rooms[old.Room]; oldExists {
		t.Fatal("sweep left a recreated retired room")
	}
	if _, newExists := media.rooms[fresh.Room]; !newExists {
		t.Fatal("sweep removed the fresh room")
	}
}

func TestPrivateCallMediaWithoutLiveKit(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer")
	rec := f.do(t, "POST", "/v1/calls/"+call.ID+"/session", f.aliceTok, "")
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec.Body.String()) != schema.ErrorCodeVoiceNotConfigured {
		t.Fatalf("session without LiveKit: %d, %s", rec.Code, rec.Body)
	}
	if rooms, err := f.srv.store.PrivateCallMediaRooms(t.Context()); err != nil || len(rooms) != 0 {
		t.Fatalf("unconfigured call media reserved a room: %d, %v", len(rooms), err)
	}
	rec = f.do(t, "POST", "/v1/channels/general/messages", f.aliceTok, `{"body":"messaging still works"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("messaging without LiveKit: %d, %s", rec.Code, rec.Body)
	}
}
