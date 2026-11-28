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

func (m *fakePrivateCallMedia) ListRooms(_ context.Context) ([]livekit.Room, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rooms := make([]livekit.Room, 0, len(m.rooms))
	for name := range m.rooms {
		rooms = append(rooms, livekit.Room{Name: name})
	}
	return rooms, nil
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
	s.callMedia = m
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

func TestPrivateCallMediaRefusesActAsCredentials(t *testing.T) {
	f, bob, bobTok, _ := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer")
	path := "/v1/calls/" + call.ID + "/session"
	issuer := fmt.Sprintf("principal:%d", f.root.ID)
	_, issued, err := f.srv.store.CreateCredential(t.Context(), issuer, bob.ID, "operator issued", nil)
	if err != nil {
		t.Fatal(err)
	}
	link, err := f.srv.store.CreateSignInLink(t.Context(), issuer, f.root.ID,
		bob.ID, schema.CreateSignInLinkRequestV1{}, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := f.srv.store.RedeemSignInLink(t.Context(), store.HashSignInLinkCode(link.Code), f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, token string }{
		{"operator-issued credential", issued},
		{"operator-issued sign-in link", redeemed.Token},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, "POST", path, tc.token, "")
			if rec.Code != http.StatusForbidden {
				t.Fatalf("session returned %d, want 403 own_sign_in_required", rec.Code)
			}
			assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeOwnSignInRequired)
		})
	}
	if len(media.rooms) != 0 || len(media.signed) != 0 {
		t.Fatal("an act-as request provisioned a room or signed a grant")
	}
	privateCallSessionTest(t, f.do(t, "POST", path, bobTok, ""))
}

func TestPrivateCallMediaUnavailableAttemptIsAbsent(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	block := f.do(t, "PUT", fmt.Sprintf("/v1/blocks/%d", f.alice.ID), bobTok, "")
	if block.Code != http.StatusOK {
		t.Fatalf("block: %d, %s", block.Code, block.Body)
	}
	call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"blocked"}`), http.StatusOK).Call
	if call.State != schema.PrivateCallStateUnavailable {
		t.Fatalf("invite state = %s, want unavailable", call.State)
	}
	for _, tc := range []struct{ role, token string }{{"caller", f.aliceTok}, {"callee", bobTok}} {
		t.Run(tc.role, func(t *testing.T) {
			rec := f.do(t, "POST", "/v1/calls/"+call.ID+"/session", tc.token, "")
			if rec.Code != http.StatusNotFound {
				t.Fatalf("session returned %d, want 404 call_not_found", rec.Code)
			}
			assertErrorBody(t, rec, http.StatusNotFound, schema.ErrorCodeCallNotFound)
		})
	}
	if len(media.rooms) != 0 || len(media.signed) != 0 {
		t.Fatal("an unavailable attempt provisioned a room or signed a grant")
	}
}

func TestPrivateCallMediaRechecksPairDuringCreate(t *testing.T) {
	for _, change := range []string{"block", "member switch", "server switch"} {
		t.Run(change, func(t *testing.T) {
			f, bob, bobTok, _ := privateCallHTTPFixture(t)
			media := addFakePrivateCallMedia(t, f.srv)
			call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer")
			media.createHook = func() {
				var err error
				switch change {
				case "block":
					block := f.do(t, "PUT", fmt.Sprintf("/v1/blocks/%d", f.alice.ID), bobTok, "")
					if block.Code != http.StatusOK {
						t.Fatalf("block: %d, %s", block.Code, block.Body)
					}
				case "member switch":
					_, err = f.srv.store.SetDMAllowed(t.Context(), "system", 0, bob.ID, false, f.srv.now())
				case "server switch":
					_, err = f.srv.store.SetDMServerSettings(t.Context(), "system", 0,
						schema.DMServerSettingsV1{DMsEnabled: false}, f.srv.now())
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			// The pair became ineligible after the first check, so the final
			// check must refuse the grant and retire the newly provisioned room.
			path := "/v1/calls/" + call.ID + "/session"
			rec := f.do(t, "POST", path, f.aliceTok, "")
			if rec.Code != http.StatusConflict {
				t.Fatalf("session returned %d, want 409 call_state_changed", rec.Code)
			}
			if ended := privateCallTestConflict(t, rec, schema.PrivateCallStateEnded); ended.Reason != "" {
				t.Fatalf("session exposed the revocation reason %q", ended.Reason)
			}
			if len(media.signed) != 0 || len(media.rooms) != 1 {
				t.Fatal("expected a room created during the request and no grant signed")
			}
			f.srv.voice.runPrivateCallMediaPass(t.Context())
			if len(media.rooms) != 0 {
				t.Fatal("room survived the media pass after the pair became ineligible")
			}
		})
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

func TestPrivateCallMediaRevocationDuringActiveCall(t *testing.T) {
	for _, action := range []string{"disable principal", "revoke credential"} {
		t.Run(action, func(t *testing.T) {
			f, bob, bobTok, _ := privateCallHTTPFixture(t)
			media := addFakePrivateCallMedia(t, f.srv)
			call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer")
			path := "/v1/calls/" + call.ID + "/session"
			aliceSession := privateCallSessionTest(t, f.do(t, "POST", path, f.aliceTok, ""))
			bobSession := privateCallSessionTest(t, f.do(t, "POST", path, bobTok, ""))
			media.rooms[aliceSession.Room] = []livekit.Participant{
				{Identity: aliceSession.Identity}, {Identity: bobSession.Identity},
			}
			f.srv.voice.runPrivateCallMediaPass(t.Context())
			current := f.do(t, "GET", "/v1/calls/current", f.aliceTok, "")
			var snapshot schema.PrivateCallSnapshotV1
			if err := json.Unmarshal(current.Body.Bytes(), &snapshot); current.Code != http.StatusOK ||
				err != nil || snapshot.Call == nil || snapshot.Call.State != schema.PrivateCallStateActive {
				t.Fatalf("call did not become active: %d, %s", current.Code, current.Body)
			}

			if action == "disable principal" {
				rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", bob.ID), f.rootTok, "")
				if rec.Code != http.StatusOK {
					t.Fatalf("disable principal: %d, %s", rec.Code, rec.Body)
				}
			} else {
				rec := f.do(t, "DELETE", "/v1/credentials/current", bobTok, "")
				if rec.Code != http.StatusNoContent {
					t.Fatalf("revoke credential: %d, %s", rec.Code, rec.Body)
				}
			}

			// Drive the lifecycle and media passes without background timers.
			if err := f.srv.store.SweepPrivateCalls(t.Context(), f.srv.now); err != nil {
				t.Fatal(err)
			}
			f.srv.voice.runPrivateCallMediaPass(t.Context())
			if _, exists := media.rooms[aliceSession.Room]; exists {
				t.Fatal("revocation left both endpoints' room in LiveKit")
			}
			if rec := f.do(t, "POST", path, bobTok, ""); rec.Code != http.StatusUnauthorized {
				t.Fatalf("revoked endpoint session: %d, %s", rec.Code, rec.Body)
			}
			rec := f.do(t, "POST", path, f.aliceTok, "")
			var conflict schema.PrivateCallConflictV1
			if err := json.Unmarshal(rec.Body.Bytes(), &conflict); rec.Code != http.StatusConflict || err != nil ||
				conflict.Code != schema.ErrorCodeCallStateChanged || conflict.Call.State != schema.PrivateCallStateEnded {
				t.Fatalf("peer session after revocation: %d, %s", rec.Code, rec.Body)
			}
			if len(media.signed) != 2 {
				t.Fatal("session signed a grant following revocation")
			}
		})
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
	rooms, err := f.srv.store.PrivateCallMediaRooms(t.Context(), f.srv.now())
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

func TestPrivateCallMediaSessionWaitsBehindDeletion(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer")
	path := "/v1/calls/" + call.ID
	session := privateCallSessionTest(t, f.do(t, "POST", path+"/session", f.aliceTok, ""))

	preparing := make(chan struct{})
	now := f.srv.now()
	clockReads := 0
	f.srv.now = func() time.Time {
		clockReads++
		// The request reads the clock for rate limiting, then again inside
		// the preparation transaction, where it holds SQLite's write lock.
		if clockReads == 2 {
			close(preparing)
		}
		return now
	}
	unlock := sync.OnceFunc(f.srv.voice.lockPrivateCallRoom(session.Room))
	done := make(chan struct{})
	var rec *httptest.ResponseRecorder
	go func() {
		defer close(done)
		rec = f.do(t, "POST", path+"/session", bobTok, "")
	}()
	defer func() {
		unlock()
		<-done
	}()
	select {
	case <-preparing:
	case <-done:
		t.Fatalf("session returned %d before preparing the room", rec.Code)
	case <-time.After(5 * time.Second):
		t.Fatal("session did not reach the preparation transaction")
	}
	// Hangup must serialize after preparation. Finish deletion under the
	// room lock before the prepared request can proceed to CreateRoom.
	decodePrivateCallResponse(t, f.do(t, "POST", path+"/hangup", f.aliceTok,
		privateCallTestCommand(call, "hangup")), http.StatusOK)
	if err := media.DeleteRoom(t.Context(), session.Room); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.MarkPrivateCallMediaDeleted(t.Context(), session.Room); err != nil {
		t.Fatal(err)
	}
	unlock()
	<-done
	privateCallTestConflict(t, rec, schema.PrivateCallStateEnded)
	if _, exists := media.rooms[session.Room]; exists {
		t.Error("the waiting session recreated the retired room")
	}
	if len(media.signed) != 1 {
		t.Error("the waiting session signed a grant for the retired room")
	}
}

// Private-call integration measurements to take (LiveKit 1.13.7,
// synchronized clocks):
// Record grant-to-join, revocation-commit-to-disconnect and stale-rejoin-to-
// deletion. Existing measurements in docs/design/voice-control-plane.md §10
// found that an unused grant was accepted 55 s past exp and refused at 65 s
// past exp. With our 15 s grant, expect about 75 s from minting. Measure the
// refresh token separately: LiveKit issued a 600 s refresh token on connect,
// so the original token's expiry does not bound a client that already joined.
// The earlier channel-room revocation test (§10, finding 10) measured that
// with auto-create disabled, LiveKit refused its own refresh token after
// DeleteRoom. Verify the same for private calls. Auto-create must be disabled
// in production. If enabled, either token can recreate the retired room and
// get another refresh token.
// For 11 minutes from retirement, expect a listed recreation to be deleted
// on the next successful 1 s media pass, plus ListRooms/DeleteRoom RPC and
// queue time. Later recreations wait for the 30 s sweep plus RPC/queue time.
// On a healthy, unsaturated service, expect initial revocation to take up to
// 1 s for the call-state sweep and 1 s for the media pass to pick it up, plus
// any in-flight room creation, list/delete RPC latency and queue time.
// Repeat with both endpoints connected, media unavailable at revocation and
// restored later, and with conchd restarted before cleanup can succeed.
func TestPrivateCallMediaRetirementRetriesAndStaleRejoin(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	now := f.srv.now().Truncate(time.Millisecond)
	f.srv.now = func() time.Time { return now }
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
	restarted.now = f.srv.now
	poller := restarted.voice
	restarted.callMedia = media
	media.deleteFailed = false
	poller.runPrivateCallMediaPass(t.Context())
	if _, ok := media.rooms[old.Room]; ok {
		t.Fatal("cleanup retry did not disconnect the endpoint and remove the room")
	}
	rooms, err := reopened.PrivateCallMediaRooms(t.Context(), now)
	if err != nil || len(rooms) != 1 || !rooms[0].Retired || rooms[0].DeletePending {
		t.Fatalf("confirmed deletion should leave a watched tombstone: %+v, %v", rooms, err)
	}
	for _, st := range []*store.Store{f.srv.store, reopened} {
		if err := st.SetVoiceRoomPrefix("replacement-"); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate LiveKit auto-creating the old room on a stale token. Its old
	// prefix no longer matches, so only the tombstone can find it.
	now = now.Add(11*time.Minute - time.Millisecond)
	media.rooms[old.Room] = []livekit.Participant{{Identity: old.Identity}}
	poller.runPrivateCallMediaPass(t.Context())
	if _, exists := media.rooms[old.Room]; exists {
		t.Fatal("next media pass left a recreated room within the refresh window")
	}
	now = now.Add(time.Millisecond)
	media.rooms[old.Room] = []livekit.Participant{{Identity: old.Identity}}
	poller.runPrivateCallMediaPass(t.Context())
	if _, exists := media.rooms[old.Room]; !exists {
		t.Fatal("media pass deleted a recreated room after the refresh window")
	}
	next := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "next"), bobTok, "answer-next")
	fresh := privateCallSessionTest(t, f.do(t, "POST", "/v1/calls/"+next.ID+"/session", f.aliceTok, ""))
	if fresh.Room == old.Room {
		t.Fatal("new attempt reused the retired room")
	}
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

func TestPrivateCallMediaRestoreQueuesRoomCleanup(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	media := addFakePrivateCallMedia(t, f.srv)
	call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "invite"), bobTok, "answer")
	session := privateCallSessionTest(t, f.do(t, "POST", "/v1/calls/"+call.ID+"/session", f.aliceTok, ""))
	media.rooms[session.Room] = []livekit.Participant{{Identity: session.Identity}}
	if _, err := f.srv.store.InvalidateRestored(t.Context(), f.srv.now()); err != nil {
		t.Fatal(err)
	}
	holders, err := f.srv.store.PrivateCallMediaHolders(t.Context(), call.ID)
	if err != nil || len(holders) != 0 {
		t.Fatalf("retired room retained media holders: %v, %v", holders, err)
	}
	f.srv.voice.runPrivateCallMediaPass(t.Context())
	if _, ok := media.rooms[session.Room]; ok {
		t.Fatal("restored private call's room survived cleanup")
	}
}

func TestPrivateCallMediaWithoutLiveKit(t *testing.T) {
	f, _, bobTok, _ := privateCallHTTPFixture(t)
	call := answerPrivateCallMediaTest(t, f, invitePrivateCallMediaTest(t, f, "first"), bobTok, "answer")
	rec := f.do(t, "POST", "/v1/calls/"+call.ID+"/session", f.aliceTok, "")
	if rec.Code != http.StatusServiceUnavailable || errCode(t, rec.Body.String()) != schema.ErrorCodeVoiceNotConfigured {
		t.Fatalf("session without LiveKit: %d, %s", rec.Code, rec.Body)
	}
	if rooms, err := f.srv.store.PrivateCallMediaRooms(t.Context(), f.srv.now()); err != nil || len(rooms) != 0 {
		t.Fatalf("unconfigured call media reserved a room: %d, %v", len(rooms), err)
	}
	rec = f.do(t, "POST", "/v1/channels/general/messages", f.aliceTok, `{"body":"messaging still works"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("messaging without LiveKit: %d, %s", rec.Code, rec.Body)
	}
}
