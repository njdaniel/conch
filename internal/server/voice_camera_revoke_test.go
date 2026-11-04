package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of the camera against revocation and the share (ADR-015 decisions 5
// and 6, issue #1241; docs/design/webcam-video.md §4.2 steps 9 to 12, §4.3,
// §4.6): a claim in flight re-reads its entitlement under the room's claims
// and every end path takes them (S3), each hook ends cameras before its
// route answers, and the share and the camera never take each other down at
// any of the four write sites (S4). The interleavings are forced with the
// share's seams; nothing sleeps.

// doOrderedBody is doOrdered with a request body: the fake's log gets an
// "http-response" entry at the moment the response begins.
func (f *presenceFixture) doOrderedBody(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(&orderWriter{ResponseWriter: rec, lk: f.lk}, req)
	return rec
}

// cameraOffBody is a PUT /v1/permissions body turning off perms in both
// columns (use_camera with use_voice, which it needs, when asked).
func (f *presenceFixture) cameraOffBody(t *testing.T, perms ...schema.Permission) string {
	t.Helper()
	req := schema.PutPermissionsRequestV1{Revision: f.revision(t)}
	for _, p := range perms {
		for _, r := range bothRoles {
			req.Changes = append(req.Changes, schema.PermissionChangeV1{Permission: p, Role: r, Granted: false})
		}
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// cameraWithdrawnBeforeAnswer reports whether, in the fake's log, an
// UpdateParticipant for who that names no camera came before the response.
func (f *presenceFixture) cameraWithdrawnBeforeAnswer(t *testing.T, who string, from int) bool {
	t.Helper()
	calls := f.lk.calls()[from:]
	f.lk.mu.Lock()
	updates := append([]fakeUpdate(nil), f.lk.updates...)
	f.lk.mu.Unlock()
	// The updates are in arrival order, as the calls are; pair them.
	ui := 0
	for _, c := range f.lk.calls()[:from] {
		if c.method == "UpdateParticipant" {
			ui++
		}
	}
	for _, c := range calls {
		switch c.method {
		case "http-response":
			return false
		case "UpdateParticipant":
			u := updates[ui]
			ui++
			if u.identity == f.identity(who) && !slices.Contains(u.sources, "CAMERA") {
				return true
			}
		}
	}
	return false
}

// cameraRevocations are the paths that take ann's place or her right to a
// camera, each as the code that runs it does: the store write that commits,
// then the hook that answers, or the route itself.
var cameraRevocations = []struct {
	name     string
	perm     bool // refused at the re-read as permission_denied
	do       func(t *testing.T, f *presenceFixture)
	rotation bool
	// setup, when set, runs on the fixture before anything is claimed.
	setup func(t *testing.T, f *presenceFixture)
}{
	{"permissions PUT/use_camera", true, func(t *testing.T, f *presenceFixture) {
		f.setCamera(t, false, schema.PermissionRoleMember, schema.PermissionRoleOperator)
		f.srv.endWithheldCameras(context.Background())
	}, false, nil},
	{"permissions PUT/use_voice", true, func(t *testing.T, f *presenceFixture) {
		f.setCells(t, []schema.Permission{schema.PermUseCamera, schema.PermShareScreen, schema.PermUseVoice}, bothRoles...)
		f.srv.voiceWithheld(context.Background())
		f.srv.endWithheldCameras(context.Background())
	}, false, nil},
	{"voice-permission route", true, func(t *testing.T, f *presenceFixture) {
		rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/principals/%d/voice-permission", f.ids["ann"]), f.rootTok, `{"voice_allowed":false}`)
		if rec.Code != http.StatusOK {
			t.Errorf("voice-permission = %d %s, want 200", rec.Code, rec.Body)
		}
	}, false, nil},
	{"membership loss", false, func(t *testing.T, f *presenceFixture) {
		if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann"]); err != nil {
			t.Error(err)
		}
		f.srv.voiceMemberRemoved(context.Background(), f.ops.ID, f.ids["ann"])
	}, true, nil},
	{"disabled", false, func(t *testing.T, f *presenceFixture) {
		if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.ids["ann"]); err != nil {
			t.Error(err)
		}
		f.srv.principalDisabled(context.Background(), f.ids["ann"])
	}, true, nil},
	{"credential revoked", false, func(t *testing.T, f *presenceFixture) {
		id := f.credID(t, "ann")
		if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
			t.Error(err)
		}
		f.srv.credentialRevoked(context.Background(), id)
	}, true, nil},
	// A role change, through the route: ann is an operator, and only the
	// operator column holds use_camera, so demoting her withholds it and the
	// route's hook (endCameraIfWithheld) ends her camera before it answers.
	{"role change", true, func(t *testing.T, f *presenceFixture) {
		rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/principals/%d/role", f.ids["ann"]), f.rootTok, `{"role":"member"}`)
		if rec.Code != http.StatusOK {
			t.Errorf("role change = %d %s, want 200", rec.Code, rec.Body)
		}
	}, false, func(t *testing.T, f *presenceFixture) {
		f.srv.credRecheckInterval = 1 << 62
		if _, err := f.srv.store.SetRole(context.Background(), fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, f.ids["ann"], "operator", nil); err != nil {
			t.Fatal(err)
		}
		f.setCamera(t, false, schema.PermissionRoleMember)
	}},
}

// TestVoiceCameraClaimRevokedBeforeGrant is S3's first interleaving: the
// claim has been admitted (its authority read up front) and is held before
// it looks the room up, or at the room's claims, while a revocation commits
// and answers; then it goes on. It must not ask LiveKit for the camera at
// all, and must leave no claim and no row. Where the permission was revoked
// the re-read refuses it as the first read would: 403 permission_denied.
func TestVoiceCameraClaimRevokedBeforeGrant(t *testing.T) {
	for _, stage := range []string{"enter", "lock"} {
		for _, rev := range cameraRevocations {
			for _, deleteFail := range []bool{false, true} {
				if deleteFail && !rev.rotation {
					continue
				}
				name := stage + "/" + rev.name
				if deleteFail {
					name += "/delete fails"
				}
				t.Run(name, func(t *testing.T) {
					f, room := newCameraFixture(t)
					if rev.setup != nil {
						rev.setup(t, f)
					}
					if deleteFail {
						f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
					}
					admitted, proceed := make(chan struct{}), make(chan struct{})
					f.srv.voice.claimSeam = func(at string) {
						if at == stage {
							close(admitted)
							<-proceed
						}
					}
					done := make(chan wireResult, 1)
					go func() { done <- f.camera(t, http.MethodPost, "ann", "ops", "") }()
					<-admitted
					rev.do(t, f) // committed and answered
					close(proceed)
					res := <-done
					if res.status == http.StatusOK {
						t.Fatalf("a camera was granted after the revocation answered: %s", res.body)
					}
					if rev.perm && (res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodePermissionDenied) {
						t.Errorf("claim after the revocation: %d %s, want 403 permission_denied", res.status, res.body)
					}
					if n := f.cameraGrants("ann"); n != 0 {
						t.Errorf("UpdateParticipant was asked for the camera %d times after the revocation answered", n)
					}
					if f.camGranted(room, "ann") || len(f.cameraHolders(room)) != 0 {
						t.Error("a camera is granted or held after the revocation answered")
					}
					if rows := f.cameraRows(t); len(rows) != 0 {
						t.Errorf("camera rows %q, want none", rows)
					}
				})
			}
		}
	}
}

// TestVoiceCameraRevocationWaitsForClaimInFlight is S3's second: the claim
// has passed its re-read and is inside LiveKit's answer to the grant when
// the revocation commits. The revocation's stop path must find the claim and
// not answer while the grant is in flight; when it answers, nothing is
// granted and nothing held. "again" is a holder's continue.
func TestVoiceCameraRevocationWaitsForClaimInFlight(t *testing.T) {
	for _, again := range []bool{false, true} {
		for _, rev := range cameraRevocations {
			for _, deleteFail := range []bool{false, true} {
				if deleteFail && !rev.rotation {
					continue
				}
				name := fmt.Sprintf("again=%v/%s", again, rev.name)
				if deleteFail {
					name += "/delete fails"
				}
				t.Run(name, func(t *testing.T) {
					f, room := newCameraFixture(t)
					if rev.setup != nil {
						rev.setup(t, f)
					}
					if deleteFail {
						f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
					}
					query := ""
					if again {
						query = named(f.mustCamera(t, "ann", ""))
					}
					ann := f.identity("ann")
					granting, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					f.lk.config(func(l *scriptedLiveKit) {
						l.onUpdateCamera = func(_, identity string, camera bool) {
							if camera && identity == ann {
								once.Do(func() {
									close(granting)
									<-release
								})
							}
						}
					})
					// The first stop path (or hook's wait) to find a room or a
					// running claim to wait for says so here.
					found := make(chan struct{})
					var foundOnce sync.Once
					seam := func(n int) {
						if n > 0 {
							foundOnce.Do(func() { close(found) })
						}
					}
					f.srv.voice.endCamerasSeam, f.srv.voice.endSharesSeam = seam, seam
					claimed := make(chan wireResult, 1)
					go func() { claimed <- f.camera(t, http.MethodPost, "ann", "ops", query) }()
					<-granting
					claimRoom := f.srv.voice.roomNamed(room)
					revoked := make(chan struct{})
					go func() {
						defer close(revoked)
						rev.do(t, f)
					}()
					// The revocation has committed and reached its stop path,
					// which found the claim; it cannot answer while the grant
					// is held inside LiveKit.
					select {
					case <-found:
					case <-time.After(10 * time.Second):
						t.Fatalf("%s: the revocation's stop path never found the claim in flight", t.Name())
					}
					select {
					case <-revoked:
						t.Fatal("the revocation answered while the camera's grant was in flight")
					default:
					}
					close(release)
					<-revoked
					f.srv.voice.mu.Lock()
					claimer := claimRoom.claimer
					f.srv.voice.mu.Unlock()
					if claimer != 0 {
						t.Errorf("the revocation answered while a claim of %d was still running", claimer)
					}
					if f.camGranted(room, "ann") || len(f.cameraHolders(room)) != 0 {
						t.Error("the camera is still granted or held when the revocation answered")
					}
					res := <-claimed
					if !again && res.status == http.StatusOK {
						// A first claim that passed its re-read before the
						// revocation may answer 200; the camera is already
						// gone when the revocation answers, and it ended
						// with a row.
						rows := f.cameraRows(t)
						if len(rows) != 2 || !strings.Contains(rows[1], " stopped ") {
							t.Errorf("rows %q, want one start and its stop", rows)
						}
					}
					f.srv.voice.mu.Lock()
					left := len(f.srv.voice.retiring)
					f.srv.voice.mu.Unlock()
					if left != 0 {
						t.Errorf("%d retired rooms are still held for their claims", left)
					}
				})
			}
		}
	}
}

// TestVoiceCameraHooksEndBeforeAnswer: the PUT, a role change, #984's voice
// switch and membership loss each end the camera through the stop path,
// under the room's claims, before the route answers, and never fail the
// request, LiveKit down included.
func TestVoiceCameraHooksEndBeforeAnswer(t *testing.T) {
	tests := []struct {
		name string
		// setup runs after ann's claim; run is the route, as the owner.
		setup  func(t *testing.T, f *presenceFixture)
		method string
		path   func(f *presenceFixture) string
		body   func(t *testing.T, f *presenceFixture) string
		code   int
		reason string
	}{
		{"PUT use_camera off", nil, http.MethodPut, func(*presenceFixture) string { return "/v1/permissions" },
			func(t *testing.T, f *presenceFixture) string { return f.cameraOffBody(t, schema.PermUseCamera) }, http.StatusOK, "access_lost"},
		{"PUT use_voice off", nil, http.MethodPut, func(*presenceFixture) string { return "/v1/permissions" },
			func(t *testing.T, f *presenceFixture) string {
				return f.cameraOffBody(t, schema.PermUseCamera, schema.PermShareScreen, schema.PermUseVoice)
			}, http.StatusOK, "access_lost"},
		{"the voice switch", nil, http.MethodPut, func(f *presenceFixture) string {
			return fmt.Sprintf("/v1/principals/%d/voice-permission", f.ids["ann"])
		},
			func(*testing.T, *presenceFixture) string { return `{"voice_allowed":false}` }, http.StatusOK, "access_lost"},
		{"a role change", func(t *testing.T, f *presenceFixture) {
			// ann is an operator, and only operators hold use_camera.
			f.setCamera(t, false, schema.PermissionRoleMember)
		}, http.MethodPut, func(f *presenceFixture) string { return fmt.Sprintf("/v1/principals/%d/role", f.ids["ann"]) },
			func(*testing.T, *presenceFixture) string { return `{"role":"member"}` }, http.StatusOK, "access_lost"},
		{"membership loss", nil, http.MethodDelete, func(f *presenceFixture) string { return fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]) },
			func(*testing.T, *presenceFixture) string { return "" }, http.StatusNoContent, "rotated"},
	}
	for _, tt := range tests {
		for _, down := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/livekit down=%v", tt.name, down), func(t *testing.T) {
				f, room := newCameraFixture(t)
				f.srv.credRecheckInterval = 1 << 62
				if tt.name == "a role change" {
					ctx := context.Background()
					if _, err := f.srv.store.SetRole(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, f.ids["ann"], "operator", nil); err != nil {
						t.Fatal(err)
					}
				}
				f.at(1_000)
				f.mustCamera(t, "ann", "")
				if tt.setup != nil {
					tt.setup(t, f)
				}
				var stops []int
				f.srv.voice.endCamerasSeam = func(n int) { stops = append(stops, n) }
				if down {
					f.lk.setUpdateStatus(http.StatusServiceUnavailable)
				}
				from := len(f.lk.calls())
				rec := f.doOrderedBody(t, tt.method, tt.path(f), f.rootTok, tt.body(t, f))
				if rec.Code != tt.code {
					t.Fatalf("%s = %d %s, want %d", tt.name, rec.Code, rec.Body, tt.code)
				}
				if got := f.cameraHolders(room); len(got) != 0 {
					t.Errorf("claims %q when the route answered", got)
				}
				want := []string{"ann started", "system stopped principal=ann reason=" + tt.reason}
				if got := f.cameraRows(t); !slices.Equal(got, want) {
					t.Errorf("rows %q, want %q", got, want)
				}
				if tt.reason == "access_lost" && !slices.ContainsFunc(stops, func(n int) bool { return n > 0 }) {
					t.Errorf("the stop path found no room (%v): something else ended the camera", stops)
				}
				if !down && tt.reason == "access_lost" && !f.cameraWithdrawnBeforeAnswer(t, "ann", from) && f.lk.in(room, f.identity("ann")) {
					t.Error("ann's camera was not withdrawn before the route answered")
				}
				if !down && f.camGranted(room, "ann") {
					t.Error("ann's camera is granted after the route answered")
				}
			})
		}
	}
}

// TestVoiceCameraAndShareTogether (S4): stopping a share leaves the sharer's
// camera, and ending a camera leaves the share, at each of the four write
// sites and through a pass's withdrawal, against a real claim; and a write
// the camera makes during a rotation grants nothing back.
func TestVoiceCameraAndShareTogether(t *testing.T) {
	const (
		all    = "MICROPHONE,SCREEN_SHARE,SCREEN_SHARE_AUDIO,CAMERA"
		share  = "MICROPHONE,SCREEN_SHARE,SCREEN_SHARE_AUDIO"
		camera = "MICROPHONE,CAMERA"
	)
	last := func(t *testing.T, f *presenceFixture, who string) string {
		t.Helper()
		return strings.Join(f.lastUpdate(t, who), ",")
	}
	t.Run("a share stop leaves the camera", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.mustCamera(t, "ann", "")
		f.camTrack(room, "ann", true, false)
		f.mustClaim(t, "ann")
		if got := last(t, f, "ann"); got != all {
			t.Errorf("the share's grant wrote %s, want %s", got, all)
		}
		f.mustStop(t, "ann")
		if got := last(t, f, "ann"); got != camera {
			t.Errorf("the share's stop wrote %s, want %s", got, camera)
		}
		if p := mustParticipant(t, f, room, "ann"); !p.camera || !p.cameraGranted {
			t.Errorf("the share's stop took the camera down: %+v", p)
		}
	})
	t.Run("a camera stop on a share holder keeps the share", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.mustClaim(t, "ann")
		f.publish(room, "ann", true, true)
		c := f.mustCamera(t, "ann", "")
		if got := last(t, f, "ann"); got != all {
			t.Errorf("the camera's grant wrote %s, want %s", got, all)
		}
		f.mustStopCamera(t, named(c))
		if got := last(t, f, "ann"); got != share {
			t.Errorf("the camera's stop wrote %s, want the four-source bundle minus the camera, %s", got, share)
		}
		if p := mustParticipant(t, f, room, "ann"); !p.screen || !p.screenAudio || !p.shareGranted {
			t.Errorf("the camera's stop took the share down: %+v", p)
		}
	})
	t.Run("a camera ended by its hook keeps the share", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.mustClaim(t, "ann")
		f.publish(room, "ann", true, true)
		f.mustCamera(t, "ann", "")
		f.setCamera(t, false, schema.PermissionRoleMember)
		f.srv.endWithheldCameras(context.Background())
		if got := last(t, f, "ann"); got != share {
			t.Errorf("the hook wrote %s, want %s", got, share)
		}
		if f.shareState(room) != f.ids["ann"] || !f.granted(t, room, "ann") {
			t.Error("the camera's hook ended the share")
		}
	})
	t.Run("a pass's share withdrawal leaves a camera", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.mustCamera(t, "ann2", "")
		f.camTrack(room, "ann2", true, false)
		// A share grant ann2 does not hold the share for.
		f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.shareGranted = true })
		f.at(500)
		f.pass(t)
		if got := last(t, f, "ann2"); got != camera {
			t.Errorf("the pass's share withdrawal wrote %s, want %s", got, camera)
		}
		if p := mustParticipant(t, f, room, "ann2"); !p.camera || p.shareGranted {
			t.Errorf("after the share's withdrawal: %+v, want the camera and no share", p)
		}
	})
	t.Run("a pass's share withdrawal leaves a camera claimed after its plan", func(t *testing.T) {
		f, room := newCameraFixture(t)
		p := f.srv.voice
		r := p.roomNamed(room)
		planned := []shareWithdrawal{{principalID: f.ids["ann2"], role: voiceRolePerson, canPublish: true, sharing: true, unclaimed: true}}
		f.mustCamera(t, "ann2", "") // between the plan and the withdrawal
		p.withdrawShares(context.Background(), r, planned)
		if got := last(t, f, "ann2"); got != camera {
			t.Errorf("the planned withdrawal wrote %s, want %s", got, camera)
		}
		if !f.camGranted(room, "ann2") {
			t.Error("the planned withdrawal took the camera down")
		}
	})
	t.Run("a pass's camera withdrawal leaves a share", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.mustClaim(t, "ann")
		f.publish(room, "ann", true, false)
		f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) { p.cameraGranted = true }) // a camera nobody claimed
		f.at(500)
		f.pass(t)
		if got := last(t, f, "ann"); got != share {
			t.Errorf("the pass's camera withdrawal wrote %s, want %s", got, share)
		}
		if p := mustParticipant(t, f, room, "ann"); !p.screen || !p.shareGranted || p.cameraGranted {
			t.Errorf("after the camera's withdrawal: %+v, want the share and no camera", p)
		}
	})
	t.Run("the companion's grant and withdrawal leave the person's camera", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
			fakeParticipant{identity: f.screenID("ann"), joinedMs: 3_000})
		f.at(100)
		f.pass(t)
		f.mustCamera(t, "ann", "")
		f.camTrack(room, "ann", true, false)
		f.mustClaim(t, "ann")
		f.mustStop(t, "ann")
		for _, u := range f.lk.updatesFor(f.screenID("ann")) {
			if slices.Contains(u.sources, "CAMERA") {
				t.Errorf("the companion was written %v: a companion is never given the camera", u.sources)
			}
		}
		if n := len(f.lk.updatesFor(f.screenID("ann"))); n < 2 {
			t.Fatalf("%d companion writes, want its grant and its withdrawal", n)
		}
		if p := mustParticipant(t, f, room, "ann"); !p.camera || !p.cameraGranted {
			t.Errorf("the companion's writes took the person's camera down: %+v", p)
		}
		if got := last(t, f, "ann"); got != camera {
			t.Errorf("the person's last write is %s, want %s", got, camera)
		}
	})
	t.Run("a camera's write during a rotation grants nothing back", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.mustClaim(t, "ann")
		c := f.mustCamera(t, "ann", "")
		// A continue whose grant is followed by the room being read as
		// retired: the claim takes back what it granted and forgets the room,
		// and no write after the grant names the share or the camera.
		f.srv.voice.liveReadSeam = func(context.Context, int64) (bool, error) { return false, nil }
		before := len(f.lk.updatesFor(f.identity("ann")))
		res := f.camera(t, http.MethodPost, "ann", "ops", named(c))
		if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
			t.Fatalf("a continue in a room retired under it: %d %s, want 409 voice_no_session", res.status, res.body)
		}
		ups := f.lk.updatesFor(f.identity("ann"))[before:]
		if len(ups) < 2 || strings.Join(ups[0].sources, ",") != all {
			t.Fatalf("writes %+v, want the grant and then its taking back", ups)
		}
		for _, u := range ups[1:] {
			if slices.Contains(u.sources, "SCREEN_SHARE") || slices.Contains(u.sources, "CAMERA") {
				t.Errorf("a write during the rotation granted %v back", u.sources)
			}
		}
		if p := mustParticipant(t, f, room, "ann"); p.shareGranted || p.cameraGranted {
			t.Errorf("after the rotation ann still has %+v", p)
		}
		want := []string{"ann started", "system stopped principal=ann reason=rotated"}
		if got := f.cameraRows(t); !slices.Equal(got, want) {
			t.Errorf("rows %q, want %q", got, want)
		}
	})
}

// TestVoiceCameraEndsWithRotation: a rotation ends every claim in the room
// (reason=rotated) before its route answers, and takes the grants back even
// when LiveKit's DeleteRoom fails and the old room lives on until the sweep;
// camera_ended says rotated, which is what a renewal's adoption reads.
func TestVoiceCameraEndsWithRotation(t *testing.T) {
	for _, deleteFail := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete fails=%v", deleteFail), func(t *testing.T) {
			f, room := newCameraFixture(t)
			if deleteFail {
				f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
			}
			f.mustCamera(t, "ann", "")
			f.mustCamera(t, "ann2", "")
			id := f.credID(t, "ann")
			if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
				t.Fatal(err)
			}
			f.srv.credentialRevoked(context.Background(), id)
			want := []string{"ann started", "ann2 started", "system stopped principal=ann reason=rotated", "system stopped principal=ann2 reason=rotated"}
			if got := f.cameraRows(t); !slices.Equal(got, want) {
				t.Errorf("rows %q, want %q", got, want)
			}
			if deleteFail {
				for _, who := range []string{"ann", "ann2"} {
					if f.camGranted(room, who) {
						t.Errorf("%s's camera is still granted in the old room", who)
					}
				}
			}
			for _, who := range []string{"ann", "ann2"} {
				if e := f.snapAs(t, f.ops, who).CameraEnded; e == nil || e.Reason != schema.VoiceCameraEndRotated {
					t.Errorf("%s's camera_ended = %+v, want rotated", who, e)
				}
			}
			f.srv.voice.mu.Lock()
			left := len(f.srv.voice.retiring)
			f.srv.voice.mu.Unlock()
			if left != 0 {
				t.Errorf("%d retired rooms are still held", left)
			}
		})
	}
}

// TestVoiceCameraClaimFailsClosedOnLivenessReadError is the camera's twin of
// the share's (issue #1089): if the read of the room's liveness after the
// grant fails, the claim takes back what it granted and answers 409
// voice_no_session, for a first claim and a continue. Only the camera ends:
// a share the person holds stays held and granted (the review of #1258).
func TestVoiceCameraClaimFailsClosedOnLivenessReadError(t *testing.T) {
	for _, again := range []bool{false, true} {
		for _, sharing := range []bool{false, true} {
			t.Run(fmt.Sprintf("again=%v/share held=%v", again, sharing), func(t *testing.T) {
				f, room := newCameraFixture(t)
				if sharing {
					f.mustClaim(t, "ann")
					f.publish(room, "ann", true, true)
				}
				query := ""
				if again {
					query = named(f.mustCamera(t, "ann", ""))
				}
				f.srv.voice.liveReadSeam = func(context.Context, int64) (bool, error) {
					return false, errors.New("store: disk I/O error")
				}
				res := f.camera(t, http.MethodPost, "ann", "ops", query)
				if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
					t.Fatalf("a claim whose liveness read fails: %d %s, want 409 voice_no_session", res.status, res.body)
				}
				if f.camGranted(room, "ann") || len(f.cameraHolders(room)) != 0 {
					t.Error("the camera is still granted or held after the claim was refused")
				}
				if sharing {
					if f.shareState(room) != f.ids["ann"] || !f.granted(t, room, "ann") {
						t.Error("a camera's failed liveness read ended or withdrew the share")
					}
					if got := strings.Join(f.lastUpdate(t, "ann"), ","); got != "MICROPHONE,SCREEN_SHARE,SCREEN_SHARE_AUDIO" {
						t.Errorf("the camera's withdrawal wrote %s, want the share kept", got)
					}
					if rows := f.shareRows(t); len(rows) != 1 {
						t.Errorf("share rows %q, want its start alone", rows)
					}
				}
			})
		}
	}
}

// TestVoiceCameraHolderInARetiredRoomEndsRotated: a holder's continue whose
// re-read finds the room retired loses the camera as the rotation would end
// it, reason=rotated, not access_lost (the review of #1258).
func TestVoiceCameraHolderInARetiredRoomEndsRotated(t *testing.T) {
	f, room := newCameraFixture(t)
	c := f.mustCamera(t, "ann", "")
	// The rotation commits after the holder check (check 7) and before the
	// re-read: only the re-read can find it.
	f.srv.voice.claimSeam = func(at string) {
		if at == "claimed" {
			f.rotateInStoreOnly(t)
		}
	}
	res := f.camera(t, http.MethodPost, "ann", "ops", named(c))
	f.srv.voice.claimSeam = nil
	if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
		t.Fatalf("a continue in a retired room: %d %s, want 409 voice_no_session", res.status, res.body)
	}
	want := []string{"ann started", "system stopped principal=ann reason=rotated"}
	if got := f.cameraRows(t); !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
	}
	if f.camGranted(room, "ann") {
		t.Error("the camera is still granted")
	}
}

// TestVoiceCameraReReadRefusalOwesNothing (the security review of #1258,
// NIT 2): a claim refused by its re-read under the lock asked LiveKit for
// nothing, so no withdrawal is owed, and the person's next unclaimed camera
// is audited as one.
func TestVoiceCameraReReadRefusalOwesNothing(t *testing.T) {
	f, room := newCameraFixture(t)
	f.srv.voice.claimSeam = func(at string) {
		if at == "claimed" {
			f.setCamera(t, false, schema.PermissionRoleMember)
		}
	}
	res := f.camera(t, http.MethodPost, "ann", "ops", "")
	f.srv.voice.claimSeam = nil
	if res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodePermissionDenied {
		t.Fatalf("a claim refused at the re-read: %d %s, want 403 permission_denied", res.status, res.body)
	}
	if f.cameraGrants("ann") != 0 {
		t.Fatal("the refused claim asked LiveKit for the camera")
	}
	f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) { p.cameraGranted, p.camera = true, true })
	f.at(500)
	f.pass(t)
	want := []string{"ann unclaimed source=observed track_source=camera track_type=video"}
	if got := f.cameraRows(t); !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
	}
}

// TestVoiceCameraWithdrawalSparesAClaimMadeAfterThePlan: a pass plans the
// removal of ann2's camera (it outlived a confirmed withdrawal), and ann2
// claims a camera before the withdrawal is made under the room's claims. The
// claim stands: no write and no removal. A track that is never a claim's (a
// mismatch) is still removed (the review of #1258).
func TestVoiceCameraWithdrawalSparesAClaimMadeAfterThePlan(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprintf("mismatch=%v", mismatch), func(t *testing.T) {
			f, room := newCameraFixture(t)
			p := f.srv.voice
			r := p.roomNamed(room)
			planned := []cameraWithdrawal{{principalID: f.ids["ann2"], role: voiceRolePerson, published: true, evict: true, mismatch: mismatch, reason: voiceReasonCameraUnclaimed}}
			f.mustCamera(t, "ann2", "") // between the plan and the withdrawal
			before := len(f.lk.updatesFor(f.identity("ann2")))
			p.withdrawCameras(context.Background(), r, planned)
			removed := !f.lk.in(room, f.identity("ann2"))
			if removed != mismatch {
				t.Errorf("ann2 removed = %v, want %v", removed, mismatch)
			}
			if !mismatch {
				if n := len(f.lk.updatesFor(f.identity("ann2"))); n != before {
					t.Errorf("the planned withdrawal wrote %d requests after the claim", n-before)
				}
				if !f.camGranted(room, "ann2") {
					t.Error("the planned withdrawal took the claimed camera down")
				}
			}
		})
	}
}

// TestVoiceCameraWithdrawalKeepsThePublisher (the review of #1258): the
// pass's camera withdrawal writes the microphone whatever LiveKit's listing
// says of canPublish, since every member publishes in a channel-wide room; a
// listing that came without a permission (read as no canPublish) does not
// make the person listen-only, or drop the share they hold.
func TestVoiceCameraWithdrawalKeepsThePublisher(t *testing.T) {
	f, room := newCameraFixture(t)
	f.mustClaim(t, "ann")
	f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) { p.camera, p.noPermission = true, true })
	f.at(500)
	f.pass(t)
	u := f.lk.updatesFor(f.identity("ann"))
	last := u[len(u)-1]
	if !last.canPublish || strings.Join(last.sources, ",") != "MICROPHONE,SCREEN_SHARE,SCREEN_SHARE_AUDIO" {
		t.Errorf("the camera's withdrawal wrote pub=%v %v, want the microphone and the share", last.canPublish, last.sources)
	}
}

// TestVoiceCameraRevocationWaitsForARetiringRoomsWithdrawal (agy's
// SHOULD-FIX 1 on #1258): a rotation (ann2's credential revoked) forgets the
// room and ends ann's camera there, and its withdrawal of ann's grant is held
// inside LiveKit. The room is in neither rooms nor, under ann's id, retiring.
// A PUT that withholds use_camera meanwhile must still find ann and wait for
// that withdrawal before it answers (ADR-015 decision 6).
func TestVoiceCameraRevocationWaitsForARetiringRoomsWithdrawal(t *testing.T) {
	f, room := newCameraFixture(t)
	f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
	f.mustCamera(t, "ann", "")
	ann := f.identity("ann")
	withdrawing, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	f.lk.setOnUpdate(func(_, identity string, _ bool) {
		if identity == ann {
			once.Do(func() {
				close(withdrawing)
				<-release
			})
		}
	})
	// However the test ends, the held withdrawal is let go before the fake
	// LiveKit is closed (cleanups run last registered first).
	var releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	rotated := make(chan struct{})
	go func() {
		defer close(rotated)
		id := f.credID(t, "ann2")
		if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
			t.Error(err)
		}
		f.srv.credentialRevoked(context.Background(), id)
	}()
	select {
	case <-withdrawing:
	case <-time.After(10 * time.Second):
		t.Fatal("the rotation never withdrew ann's camera")
	}
	f.srv.voice.mu.Lock()
	inRooms := f.srv.voice.rooms[room] != nil
	f.srv.voice.mu.Unlock()
	if inRooms {
		t.Fatal("the rotated room is still in rooms: the interleaving was not reached")
	}
	f.setCamera(t, false, schema.PermissionRoleMember, schema.PermissionRoleOperator)
	found := make(chan int, 1)
	f.srv.voice.endCamerasWaitSeam = func(n int) {
		select {
		case found <- n:
		default:
		}
	}
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		f.srv.endWithheldCameras(context.Background())
	}()
	select {
	case n := <-found:
		if n == 0 {
			t.Fatal("the PUT's stop path found nothing to wait for while ann's grant was being withdrawn")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the PUT's stop path never ran for ann")
	}
	select {
	case <-answered:
		t.Fatal("the PUT answered while ann's camera grant was still being withdrawn")
	case <-time.After(50 * time.Millisecond):
	}
	letGo()
	<-answered
	if f.camGranted(room, "ann") {
		t.Error("ann's camera is still granted in the old room when the PUT answered")
	}
	<-rotated
}

// heldAnswer reports that answered stays open for a moment: what a test
// calls while it holds the withdrawal the revocation must wait for.
func heldAnswer(t *testing.T, answered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-answered:
		t.Fatalf("%s: the use_camera revocation answered while ann's camera grant was still in flight", what)
	case <-time.After(100 * time.Millisecond):
	}
}

// waitOr waits for c, or fails the test naming what it waited for.
func waitOr(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestVoiceCameraRevocationFindsARoomForgottenAfterItsLook (the security
// review of #1258, round 4): the revocation's stop path looks while the room
// is live, with ann's claim in it, and is held at the room's claims. A
// rotation then forgets the room (ann's camera ends, rotated) and is held
// before retireRoom takes the claims to withdraw ann's grant. The stop path
// gets the claims first and finds no claim to end; it must still wait for
// the withdrawal before the revocation answers.
func TestVoiceCameraRevocationFindsARoomForgottenAfterItsLook(t *testing.T) {
	f, room := newCameraFixture(t)
	f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
	f.mustCamera(t, "ann", "")
	r := f.srv.voice.roomNamed(room)
	r.claims.Lock()
	unlocked := false
	t.Cleanup(func() {
		if !unlocked {
			r.claims.Unlock()
		}
	})
	looked := make(chan struct{})
	var lookOnce sync.Once
	f.srv.voice.endCamerasSeam = func(n int) {
		if n > 0 {
			lookOnce.Do(func() { close(looked) })
		}
	}
	atRetire, goRetire := make(chan struct{}), make(chan struct{})
	var retireOnce, goOnce sync.Once
	letRetire := func() { goOnce.Do(func() { close(goRetire) }) }
	t.Cleanup(letRetire)
	f.srv.voice.claimSeam = func(at string) {
		if at == "retire" {
			retireOnce.Do(func() { close(atRetire) })
			<-goRetire
		}
	}
	f.setCamera(t, false, schema.PermissionRoleMember, schema.PermissionRoleOperator)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		f.srv.endWithheldCameras(context.Background())
	}()
	waitOr(t, looked, "the stop path to find ann's room")
	rotated := make(chan struct{})
	go func() {
		defer close(rotated)
		id := f.credID(t, "ann2")
		if err := f.srv.store.RevokeCredential(context.Background(), "system", id); err != nil {
			t.Error(err)
		}
		f.srv.credentialRevoked(context.Background(), id)
	}()
	waitOr(t, atRetire, "the rotation to forget the room")
	unlocked = true
	r.claims.Unlock() // the stop path takes the claims before retireRoom
	heldAnswer(t, answered, "forgotten after the stop path's look")
	letRetire()
	waitOr(t, answered, "the revocation to answer")
	if f.camGranted(room, "ann") {
		t.Error("ann's camera is still granted in the old room when the revocation answered")
	}
	waitOr(t, rotated, "the rotation's hook")
}

// TestVoiceCameraRevocationWaitsWhenAClaimEndsBeforeRetireRoom (the security
// review of #1258, round 4): a rotation forgets the room while ann2's claim
// is inside LiveKit, so the room is retiring under ann2's id, and ends ann's
// camera there. ann2's claim then returns before retireRoom has looked at the
// room again. A use_camera revocation of ann made in between must still wait
// for retireRoom's withdrawal of ann's grant.
func TestVoiceCameraRevocationWaitsWhenAClaimEndsBeforeRetireRoom(t *testing.T) {
	f, room := newCameraFixture(t)
	f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
	f.mustCamera(t, "ann", "")
	ann2 := f.identity("ann2")
	granting, release := make(chan struct{}), make(chan struct{})
	var grantOnce, releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	f.lk.config(func(l *scriptedLiveKit) {
		l.onUpdateCamera = func(_, identity string, camera bool) {
			if camera && identity == ann2 {
				grantOnce.Do(func() {
					close(granting)
					<-release
				})
			}
		}
	})
	atRetire, goRetire := make(chan struct{}), make(chan struct{})
	var retireOnce, goOnce sync.Once
	letRetire := func() { goOnce.Do(func() { close(goRetire) }) }
	t.Cleanup(letRetire)
	f.srv.voice.claimSeam = func(at string) {
		if at == "retire" {
			retireOnce.Do(func() { close(atRetire) })
			<-goRetire
		}
	}
	claimed := make(chan wireResult, 1)
	go func() { claimed <- f.camera(t, http.MethodPost, "ann2", "ops", "") }()
	waitOr(t, granting, "ann2's grant to reach LiveKit")
	rotated := make(chan struct{})
	go func() {
		defer close(rotated)
		f.rotateInStoreOnly(t)
		if _, err := f.srv.voice.rotateInvalid(context.Background()); err != nil {
			t.Error(err)
		}
		old, err := f.srv.store.ListRetiredVoiceRooms(context.Background())
		if err != nil {
			t.Error(err)
			return
		}
		for _, o := range old {
			if o.RoomName == room {
				f.srv.voice.retireRoom(context.Background(), o)
			}
		}
	}()
	waitOr(t, atRetire, "the rotation to forget the room")
	letGo()
	select {
	case res := <-claimed:
		if res.status == http.StatusOK {
			t.Errorf("ann2's claim in a retired room was granted: %s", res.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ann2's claim never returned")
	}
	f.setCamera(t, false, schema.PermissionRoleMember, schema.PermissionRoleOperator)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		f.srv.voice.endCamerasOf(context.Background(), f.ids["ann"], 0, voiceCameraStopAccessLost)
	}()
	heldAnswer(t, answered, "a claim ended between the forget and retireRoom")
	letRetire()
	waitOr(t, answered, "the revocation to answer")
	if f.camGranted(room, "ann") {
		t.Error("ann's camera is still granted in the old room when the revocation answered")
	}
	waitOr(t, rotated, "the rotation")
}

// TestVoiceCameraRevocationWaitsForAPendingGrantAcrossARotation (Kimi, round
// 4): ann's first claim has passed its re-read and is inside LiveKit's grant
// when a rotation retires the room with ann as its claimer; the claim is
// pending, so the forget ends no camera of hers. A use_camera revocation,
// with nothing of the share's to wait on, must still find the claim and wait
// for it, so the grant is taken back before the revocation answers.
func TestVoiceCameraRevocationWaitsForAPendingGrantAcrossARotation(t *testing.T) {
	f, room := newCameraFixture(t)
	f.lk.config(func(l *scriptedLiveKit) { l.deleteFail = map[string]bool{room: true} })
	ann := f.identity("ann")
	granting, release := make(chan struct{}), make(chan struct{})
	var grantOnce, releaseOnce sync.Once
	letGo := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(letGo)
	f.lk.config(func(l *scriptedLiveKit) {
		l.onUpdateCamera = func(_, identity string, camera bool) {
			if camera && identity == ann {
				grantOnce.Do(func() {
					close(granting)
					<-release
				})
			}
		}
	})
	claimed := make(chan wireResult, 1)
	go func() { claimed <- f.camera(t, http.MethodPost, "ann", "ops", "") }()
	waitOr(t, granting, "ann's grant to reach LiveKit")
	old, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.rotateInStoreOnly(t)
	f.srv.voice.retireRoom(context.Background(), old)
	f.setCamera(t, false, schema.PermissionRoleMember, schema.PermissionRoleOperator)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		f.srv.endWithheldCameras(context.Background())
	}()
	heldAnswer(t, answered, "a pending grant across a rotation")
	letGo()
	waitOr(t, answered, "the revocation to answer")
	if f.camGranted(room, "ann") {
		t.Error("ann's camera is granted after the revocation answered")
	}
	if res := <-claimed; res.status == http.StatusOK {
		t.Errorf("ann's claim in a retired room was granted: %s", res.body)
	}
}
