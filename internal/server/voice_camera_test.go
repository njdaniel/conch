package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of the camera claim (ADR-015, issue #1241; docs/design/webcam-video.md
// §4, §5): the two routes, over the scripted LiveKit with the fixture's manual
// clock. They prove the server's decisions on one machine, not a browser's
// capture or real video. The passes are voice_camera_pass_test.go's, the
// revocations voice_camera_revoke_test.go's.

// ---------------------------------------------------------------------------
// Fixture additions

// newCameraFixture is the share fixture (ann and ann2 in ops, connected,
// seen by a pass) with use_camera turned on for operators and members, as an
// owner turns it on: both cells in one PUT (precedence, admin-roles §4.4.1).
func newCameraFixture(t *testing.T) (*presenceFixture, string) {
	t.Helper()
	f, room := newShareFixture(t)
	f.setCamera(t, true, schema.PermissionRoleOperator, schema.PermissionRoleMember)
	return f, room
}

// setCamera sets use_camera's cell to on in the given columns, as the owner,
// through the store: the write the permissions PUT makes, without its hook.
func (f *presenceFixture) setCamera(t *testing.T, on bool, roles ...schema.PermissionRole) {
	t.Helper()
	ctx := context.Background()
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	req := schema.PutPermissionsRequestV1{Revision: doc.Revision}
	for _, r := range roles {
		req.Changes = append(req.Changes, schema.PermissionChangeV1{Permission: schema.PermUseCamera, Role: r, Granted: on})
	}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, req, time.Now()); err != nil {
		t.Fatalf("set use_camera %v in %v: %v", on, roles, err)
	}
}

// camera sends a camera request as who: method POST or DELETE, query the
// part after "?" (or "").
func (f *presenceFixture) camera(t *testing.T, method, who, channel, query string) wireResult {
	t.Helper()
	path := "/v1/channels/" + channel + "/voice/camera"
	if query != "" {
		path += "?" + query
	}
	return f.callREST(t, method, path, f.tokens[who], "")
}

// mustCamera claims a camera in ops as who, with query, and requires it
// granted to who.
func (f *presenceFixture) mustCamera(t *testing.T, who, query string) schema.VoiceCameraV1 {
	t.Helper()
	res := f.camera(t, http.MethodPost, who, "ops", query)
	if res.status != http.StatusOK {
		t.Fatalf("%s claims a camera (%q): %d %s, want 200", who, query, res.status, res.body)
	}
	return decodeCamera(t, res.body, f.ids[who])
}

func decodeCamera(t *testing.T, body string, want int64) schema.VoiceCameraV1 {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	var c schema.VoiceCameraV1
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("camera %s does not validate: %v", body, err)
	}
	if c.PrincipalID != want {
		t.Fatalf("camera = %+v, want principal %d's", c, want)
	}
	return c
}

// named is the query naming claim c.
func named(c schema.VoiceCameraV1) string {
	return schema.VoiceCameraClaimParam + "=" + c.StartedAt.Time().Format(time.RFC3339Nano)
}

// mustStopCamera releases ann's camera in ops with query and requires 204.
func (f *presenceFixture) mustStopCamera(t *testing.T, query string) {
	t.Helper()
	if res := f.camera(t, http.MethodDelete, "ann", "ops", query); res.status != http.StatusNoContent || res.body != "" {
		t.Fatalf("ann stops her camera (%q): %d %q, want 204 and no body", query, res.status, res.body)
	}
}

// cameraRows lists the camera rows of the audit log in order, as "<actor>
// <started|stopped|unclaimed> <rest of the detail>", names for principals,
// checking on the way that each has the channel as subject and begins its
// detail with it.
func (f *presenceFixture) cameraRows(t *testing.T) []string {
	t.Helper()
	names := map[string]string{"system": "system"}
	for name, id := range f.ids {
		names[fmt.Sprintf("principal:%d", id)] = name
	}
	var out []string
	for _, e := range f.audit(t) {
		if !strings.HasPrefix(e.Action, "voice_camera_") {
			continue
		}
		who, ok := names[e.Actor]
		if !ok {
			t.Errorf("%s has actor %q", e.Action, e.Actor)
		}
		if want := fmt.Sprintf("channel:%d", f.ops.ID); e.Subject != want {
			t.Errorf("%s has subject %q, want %q", e.Action, e.Subject, want)
		}
		rest, ok := strings.CutPrefix(e.Detail, fmt.Sprintf("channel=%d", f.ops.ID))
		if !ok {
			t.Errorf("%s has detail %q, want it to begin with the channel", e.Action, e.Detail)
		}
		for name, id := range f.ids {
			rest = strings.ReplaceAll(rest, fmt.Sprintf("principal=%d ", id), "principal="+name+" ")
		}
		out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s %s", who, strings.TrimPrefix(e.Action, "voice_camera_"), strings.TrimSpace(rest))))
	}
	return out
}

// cameraHolders names who holds a camera claim in room, in id order, with a
// "*" for a pending one.
func (f *presenceFixture) cameraHolders(room string) []string {
	byID := map[int64]string{}
	for name, id := range f.ids {
		byID[id] = name
	}
	p := f.srv.voice
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.rooms[room]
	if r == nil {
		return nil
	}
	var out []string
	for _, pid := range sortedCameraHolders(r) {
		name := byID[pid]
		if r.cameras[pid].pending {
			name += "*"
		}
		out = append(out, name)
	}
	return out
}

// camGranted reports whether the fake LiveKit lets who's connection to room
// publish a camera.
func (f *presenceFixture) camGranted(room, who string) bool {
	p, ok := f.lk.participant(room, f.identity(who))
	return ok && p.cameraGranted
}

// camTrack sets who's camera track in room: published, and muted or not.
func (f *presenceFixture) camTrack(room, who string, published, muted bool) {
	f.lk.update(room, f.identity(who), func(p *fakeParticipant) { p.camera, p.cameraMuted = published, muted })
}

// cameraGrants counts the UpdateParticipant requests that granted who the
// camera.
func (f *presenceFixture) cameraGrants(who string) int {
	n := 0
	for _, u := range f.lk.updatesFor(f.identity(who)) {
		if slices.Contains(u.sources, "CAMERA") {
			n++
		}
	}
	return n
}

// cameras names who presence, as viewer reads it, shows with a camera in ch,
// in id order.
func (f *presenceFixture) cameras(t *testing.T, ch store.Channel, viewer string) []string {
	t.Helper()
	byID := map[int64]string{}
	for name, id := range f.ids {
		byID[id] = name
	}
	var out []string
	for _, r := range f.snapAs(t, ch, viewer).Rooms {
		for _, p := range r.Participants {
			if p.Camera {
				out = append(out, byID[p.PrincipalID])
			}
		}
	}
	return out
}

// lastUpdate is the sources of the last UpdateParticipant request for who.
func (f *presenceFixture) lastUpdate(t *testing.T, who string) []string {
	t.Helper()
	ups := f.lk.updatesFor(f.identity(who))
	if len(ups) == 0 {
		t.Fatalf("no UpdateParticipant for %s", who)
	}
	return ups[len(ups)-1].sources
}

// ---------------------------------------------------------------------------
// The claim

// TestVoiceCameraClaimRefusals: §4.2's checks, in order, with what each
// refusal leaves behind: no claim, no LiveKit call (but where LiveKit's own
// answer is the refusal), no camera row, and an access_denied row only where
// the shared voice checks write one (the agent, voice withheld). The
// permission refusal of check 5 is in the answer, not the log (§4.7).
func TestVoiceCameraClaimRefusals(t *testing.T) {
	connected := func(t *testing.T, f *presenceFixture) {
		room := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
		f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 1_000})
	}
	cameraOn := func(t *testing.T, f *presenceFixture) {
		f.setCamera(t, true, schema.PermissionRoleOperator, schema.PermissionRoleMember)
	}
	// A new server has the camera on (issue #1361); these rows turn it off,
	// the member cell first.
	cameraOff := func(t *testing.T, f *presenceFixture) {
		f.setCamera(t, false, schema.PermissionRoleMember, schema.PermissionRoleOperator)
	}
	both := func(fns ...func(*testing.T, *presenceFixture)) func(*testing.T, *presenceFixture) {
		return func(t *testing.T, f *presenceFixture) {
			for _, fn := range fns {
				fn(t, f)
			}
		}
	}
	full := func(t *testing.T, f *presenceFixture) {
		cameraOn(t, f)
		connected(t, f)
		room := f.room(t, f.ops)
		parts := []fakeParticipant{mustParticipant(t, f, room, "ann")}
		for i := 0; i < voiceCameraCap; i++ {
			name := fmt.Sprintf("member-%d", i+1)
			f.addPerson(t, name, false)
			decodeSession(t, f.session(t, name, "ops"))
			parts = append(parts, fakeParticipant{identity: f.identity(name), joinedMs: int64(2_000 + i)})
		}
		f.lk.setRoom(room, parts...)
		for i := 0; i < voiceCameraCap; i++ {
			f.mustCamera(t, fmt.Sprintf("member-%d", i+1), "")
		}
	}
	tests := []struct {
		name       string
		opts       presenceOpts
		setup      func(*testing.T, *presenceFixture)
		who        string
		channel    string
		query      string
		wantStatus int
		wantCode   string
		wantPerm   schema.Permission
		wantDenied string // reason of the one access_denied row, if any
		wantGrants int    // UpdateParticipant calls the refusal may make
	}{
		{"no credential", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "", "ops", "", 401, "unauthenticated", "", "", 0},
		{"authentication off", presenceOpts{auth: AuthOff}, nil, "", "ops", "", 400, schema.ErrorCodeVoiceRequiresAuth, "", "", 0},
		{"participant, before the desktop phase", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "ann", "ops", "participant=screen", 400, "invalid_request", "", "", 0},
		{"participant, by a non-member", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "bob", "ops", "participant=screen", 400, "invalid_request", "", "", 0},
		{"a malformed started_at", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "ann", "ops", "started_at=yesterday", 400, "invalid_request", "", "", 0},
		{"started_at twice", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "ann", "ops", "started_at=2020-01-01T00:00:00Z&started_at=2020-01-01T00:00:00Z", 400, "invalid_request", "", "", 0},
		{"a non-member", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "bob", "ops", "", 404, "channel_not_found", "", "", 0},
		{"an unknown channel", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "ann", "nosuch", "", 404, "channel_not_found", "", "", 0},
		{"an operator who is not a member", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "root", "ops", "", 404, "channel_not_found", "", "", 0},
		{"an agent member", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "robo", "ops", "", 403, "forbidden", "", denyAgentVoice, 0},
		{"a member without voice", presenceOpts{auth: AuthRequired}, cameraOn, "vic", "ops", "", 403, schema.ErrorCodeVoiceNotAllowed, schema.PermUseVoice, schema.ErrorCodeVoiceNotAllowed, 0},
		{"a member, voice not configured", presenceOpts{auth: AuthRequired, unconfigured: true}, cameraOn, "ann", "ops", "", 503, schema.ErrorCodeVoiceNotConfigured, "", "", 0},
		{"a member without use_camera (the cells turned off)", presenceOpts{auth: AuthRequired}, both(cameraOff, connected), "ann", "ops", "", 403, schema.ErrorCodePermissionDenied, schema.PermUseCamera, "", 0},
		{"an operator member without use_camera", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) { cameraOff(t, f); f.addPerson(t, "operator-1", true) }, "operator-1", "ops", "", 403, schema.ErrorCodePermissionDenied, schema.PermUseCamera, "", 0},
		{"a member who was never issued a session", presenceOpts{auth: AuthRequired}, cameraOn, "ann", "ops", "", 409, schema.ErrorCodeVoiceNoSession, "", "", 0},
		{"a session in another channel", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "ann", "ops2", "", 409, schema.ErrorCodeVoiceNoSession, "", "", 0},
		{"a session under another credential", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) {
				cameraOn(t, f)
				connected(t, f)
				_, tok := f.credentialWithExpiry(t, "ann", time.Now().Add(time.Hour))
				f.tokens["ann-other"] = tok
			}, "ann-other", "ops", "", 409, schema.ErrorCodeVoiceNoSession, "", "", 0},
		{"a named claim that is not held", presenceOpts{auth: AuthRequired}, both(cameraOn, connected), "ann", "ops", "started_at=2020-01-01T00:00:00Z", 409, schema.ErrorCodeVoiceCameraStale, "", "", 0},
		{"the room is full", presenceOpts{auth: AuthRequired}, full, "ann", "ops", "", 409, schema.ErrorCodeVoiceCameraFull, "", "", 0},
		// A camera is granted to a connection; a holder with none is told
		// they have no session.
		{"a holder who is not connected", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) { cameraOn(t, f); decodeSession(t, f.session(t, "ann", "ops")) },
			"ann", "ops", "", 409, schema.ErrorCodeVoiceNoSession, "", "", 1},
		{"LiveKit does not answer", presenceOpts{auth: AuthRequired},
			func(t *testing.T, f *presenceFixture) {
				cameraOn(t, f)
				connected(t, f)
				f.lk.setUpdateStatus(http.StatusServiceUnavailable)
			}, "ann", "ops", "", 503, schema.ErrorCodeVoiceUnavailable, "", "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPresenceFixture(t, tt.opts)
			f.joinWithoutVoice(t)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			room := f.room(t, f.ops)
			rows := len(f.cameraRows(t))
			grants := f.lk.count("UpdateParticipant")
			denied := len(f.audits(t, "access_denied"))
			holders := f.cameraHolders(room)

			res := f.camera(t, http.MethodPost, tt.who, tt.channel, tt.query)
			if res.status != tt.wantStatus || errCode(t, res.body) != tt.wantCode {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if tt.wantPerm != "" {
				if got := permissionOf(t, []byte(res.body)); got != tt.wantPerm {
					t.Errorf("the refusal names %q, want %q", got, tt.wantPerm)
				}
			}
			if got := f.cameraRows(t); len(got) != rows {
				t.Errorf("a refused claim wrote camera rows: %q", got[rows:])
			}
			if got := f.lk.count("UpdateParticipant") - grants; got != tt.wantGrants {
				t.Errorf("the refusal made %d UpdateParticipant calls, want %d", got, tt.wantGrants)
			}
			if now := f.cameraHolders(room); !slices.Equal(now, holders) {
				t.Errorf("claims %q after a refused claim, were %q", now, holders)
			}
			denials := f.audits(t, "access_denied")[denied:]
			switch {
			case tt.wantDenied == "" && len(denials) != 0:
				t.Errorf("access_denied rows = %+v, want none", denials)
			case tt.wantDenied != "" && (len(denials) != 1 || !strings.Contains(denials[0].Detail, tt.wantDenied)):
				t.Errorf("access_denied rows = %+v, want one for %s", denials, tt.wantDenied)
			}
		})
	}
}

// TestVoiceCameraClaim: a claim is granted to the caller's own connection
// through the one permission function (the microphone and the camera), is
// answered with a VoiceCameraV1 and written once, and shows in presence from
// its answer. The join grant is still the microphone alone.
func TestVoiceCameraClaim(t *testing.T) {
	f, room := newCameraFixture(t)
	if p := mustParticipant(t, f, room, "ann"); p.cameraGranted {
		t.Fatal("ann may publish a camera before any claim")
	}
	f.at(1_000)
	c := f.mustCamera(t, "ann", "")
	if !c.StartedAt.Time().Equal(ruleT0.Add(time.Second)) {
		t.Errorf("started_at = %v, want the claim's time", c.StartedAt)
	}
	if got := f.lastUpdate(t, "ann"); !slices.Equal(got, []string{"MICROPHONE", "CAMERA"}) {
		t.Errorf("the grant wrote %q, want the microphone and the camera", got)
	}
	if !f.camGranted(room, "ann") || f.camGranted(room, "ann2") || f.cameraGrants("ann2") != 0 {
		t.Error("the camera is not granted to ann alone")
	}
	if got := f.cameraRows(t); !slices.Equal(got, []string{"ann started"}) {
		t.Errorf("camera rows %q, want ann's start", got)
	}
	if got := f.cameras(t, f.ops, "ann2"); !slices.Equal(got, []string{"ann"}) {
		t.Errorf("presence shows cameras %q, want ann's", got)
	}
	// An un-named request while holding: the held claim, nothing written.
	f.at(2_000)
	again := f.mustCamera(t, "ann", "")
	if !again.StartedAt.Time().Equal(c.StartedAt.Time()) {
		t.Errorf("an un-named repeat answered %v, want the held claim %v", again.StartedAt, c.StartedAt)
	}
	if got := f.cameraRows(t); len(got) != 1 {
		t.Errorf("an un-named repeat wrote rows: %q", got)
	}
	// The join grant is unchanged: a session's token names the microphone.
	tok := decodeSession(t, f.session(t, "ann2", "ops")).Rooms[0].Token
	parts := strings.Split(tok, ".")
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Video struct {
			CanPublishSources []string `json:"canPublishSources"`
		} `json:"video"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(claims.Video.CanPublishSources, []string{"microphone"}) {
		t.Errorf("a join token grants %v, want the microphone alone", claims.Video.CanPublishSources)
	}
}

// TestVoiceCameraNamedClaim (S2): a named POST continues the caller's
// current claim (granted again, nothing written), adopts once, within the
// window and in the same channel, a claim the room's rotation ended, and is
// otherwise stale with nothing changed; it never starts a claim otherwise.
func TestVoiceCameraNamedClaim(t *testing.T) {
	type endFn func(t *testing.T, f *presenceFixture, room string, c schema.VoiceCameraV1)
	stopSelf := func(t *testing.T, f *presenceFixture, _ string, c schema.VoiceCameraV1) {
		f.mustStopCamera(t, named(c))
	}
	stopOp := func(t *testing.T, f *presenceFixture, _ string, _ schema.VoiceCameraV1) {
		if res := f.callREST(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/voice/camera?principal=%d", f.ids["ann"]), f.rootTok, ""); res.status != http.StatusNoContent {
			t.Fatalf("operator stop: %d %s", res.status, res.body)
		}
	}
	idle := func(t *testing.T, f *presenceFixture, _ string, _ schema.VoiceCameraV1) { f.at(20_000); f.pass(t) }
	left := func(t *testing.T, f *presenceFixture, room string, _ schema.VoiceCameraV1) {
		f.lk.drop(room, f.identity("ann"))
		f.at(1_500)
		f.pass(t)
	}
	accessLost := func(t *testing.T, f *presenceFixture, _ string, _ schema.VoiceCameraV1) {
		f.setCamera(t, false, schema.PermissionRoleMember)
		f.srv.endWithheldCameras(context.Background())
	}
	tests := []struct {
		name string
		end  endFn
	}{
		{"ended self", stopSelf}, {"ended operator", stopOp}, {"ended idle", idle},
		{"ended left", left}, {"ended access_lost", accessLost},
	}
	for _, tt := range tests {
		t.Run(tt.name+": stale", func(t *testing.T) {
			f, room := newCameraFixture(t)
			f.at(1_000)
			c := f.mustCamera(t, "ann", "")
			tt.end(t, f, room, c)
			if tt.name == "ended access_lost" {
				f.setCamera(t, true, schema.PermissionRoleMember)
			}
			if tt.name == "ended left" {
				f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 9_000}, mustParticipant(t, f, room, "ann2"))
			}
			rows, grants := f.cameraRows(t), f.cameraGrants("ann")
			res := f.camera(t, http.MethodPost, "ann", "ops", named(c))
			if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceCameraStale {
				t.Fatalf("a named claim after it %s: %d %s, want 409 voice_camera_stale", tt.name, res.status, res.body)
			}
			if got := f.cameraRows(t); !slices.Equal(got, rows) {
				t.Errorf("rows %q after a stale claim, were %q", got, rows)
			}
			if f.cameraGrants("ann") != grants || len(f.cameraHolders(room)) != 0 {
				t.Error("a stale claim granted a camera or left a claim")
			}
		})
	}

	t.Run("continue", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.at(1_000)
		c := f.mustCamera(t, "ann", "")
		// The connection was replaced through a full reconnect: a join token,
		// so the microphone alone.
		f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 5_000}, mustParticipant(t, f, room, "ann2"))
		f.at(3_000)
		got := f.mustCamera(t, "ann", named(c))
		if !got.StartedAt.Time().Equal(c.StartedAt.Time()) {
			t.Errorf("a continue answered %v, want the held claim's %v", got.StartedAt, c.StartedAt)
		}
		if !f.camGranted(room, "ann") {
			t.Error("a continue did not grant the camera again")
		}
		if rows := f.cameraRows(t); !slices.Equal(rows, []string{"ann started"}) {
			t.Errorf("rows %q, want the one start", rows)
		}
	})

	adoptSetup := func(t *testing.T) (*presenceFixture, schema.VoiceCameraV1, string) {
		t.Helper()
		f, _ := newCameraFixture(t)
		f.at(1_000)
		c := f.mustCamera(t, "ann", "")
		// A rotation ends the claim (reason=rotated) and the channel gets a
		// new room, which ann joins.
		old, err := f.srv.store.ChannelVoiceRoom(context.Background(), f.ops.ID)
		if err != nil {
			t.Fatal(err)
		}
		f.rotateInStoreOnly(t)
		f.srv.voice.retireRoom(context.Background(), old)
		next := decodeSession(t, f.session(t, "ann", "ops")).Rooms[0].Room
		f.lk.setRoom(next, fakeParticipant{identity: f.identity("ann"), joinedMs: 20_000})
		return f, c, next
	}
	t.Run("adoption within the window", func(t *testing.T) {
		f, c, next := adoptSetup(t)
		f.at(1_000 + int(voiceCameraAdoptWindow/time.Millisecond) - 100)
		got := f.mustCamera(t, "ann", named(c))
		if got.StartedAt.Time().Equal(c.StartedAt.Time()) {
			t.Error("an adoption answered the old claim's started_at, want a new one")
		}
		if !f.camGranted(next, "ann") {
			t.Error("the adoption granted no camera")
		}
		want := []string{"ann started", "system stopped principal=ann reason=rotated", "ann started"}
		if rows := f.cameraRows(t); !slices.Equal(rows, want) {
			t.Errorf("rows %q, want %q", rows, want)
		}
		// The record is consumed by the adoption: ann's own document no longer
		// carries it, and once the adopted claim has ended too, a second
		// adoption of the first claim is stale.
		if e := f.snapAs(t, f.ops, "ann").CameraEnded; e != nil {
			t.Errorf("the rotation's end record %+v survived its adoption", e)
		}
		f.mustStopCamera(t, named(got))
		if res := f.camera(t, http.MethodPost, "ann", "ops", named(c)); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceCameraStale {
			t.Errorf("a second adoption: %d %s, want 409 voice_camera_stale", res.status, res.body)
		}
	})
	t.Run("a failed adoption leaves the record, and a retry adopts", func(t *testing.T) {
		f, c, next := adoptSetup(t)
		f.at(2_000)
		f.lk.setUpdateStatus(http.StatusServiceUnavailable)
		if res := f.camera(t, http.MethodPost, "ann", "ops", named(c)); res.status != http.StatusServiceUnavailable {
			t.Fatalf("an adoption with LiveKit down: %d %s, want 503", res.status, res.body)
		}
		if e := f.snapAs(t, f.ops, "ann").CameraEnded; e == nil || e.Reason != schema.VoiceCameraEndRotated {
			t.Fatalf("after a failed adoption ann's camera_ended = %+v, want the rotation's", e)
		}
		f.lk.setUpdateStatus(0)
		f.at(3_000)
		got := f.mustCamera(t, "ann", named(c))
		if got.StartedAt.Time().Equal(c.StartedAt.Time()) || !f.camGranted(next, "ann") {
			t.Errorf("the retry answered %v and granted=%v, want a new claim granted", got.StartedAt, f.camGranted(next, "ann"))
		}
		want := []string{"ann started", "system stopped principal=ann reason=rotated", "ann started"}
		if rows := f.cameraRows(t); !slices.Equal(rows, want) {
			t.Errorf("rows %q, want %q", rows, want)
		}
	})
	t.Run("adoption past the window", func(t *testing.T) {
		f, c, next := adoptSetup(t)
		f.at(1_000 + int(voiceCameraAdoptWindow/time.Millisecond) + 100)
		if res := f.camera(t, http.MethodPost, "ann", "ops", named(c)); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceCameraStale {
			t.Errorf("an adoption past the window: %d %s, want 409 voice_camera_stale", res.status, res.body)
		}
		if f.camGranted(next, "ann") || len(f.cameraHolders(next)) != 0 {
			t.Error("a stale adoption granted a camera")
		}
	})
	t.Run("adoption in another channel", func(t *testing.T) {
		f, c, _ := adoptSetup(t)
		f.setCamera(t, true, schema.PermissionRoleMember)
		other := decodeSession(t, f.session(t, "ann", "ops2")).Rooms[0].Room
		f.lk.setRoom(other, fakeParticipant{identity: f.identity("ann"), joinedMs: 21_000})
		f.at(2_000)
		if res := f.camera(t, http.MethodPost, "ann", "ops2", named(c)); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceCameraStale {
			t.Errorf("an adoption in another channel: %d %s, want 409 voice_camera_stale", res.status, res.body)
		}
		if f.camGranted(other, "ann") {
			t.Error("a stale adoption granted a camera")
		}
	})
}

// TestVoiceCameraCap: the tenth claim in a room is 409 voice_camera_full,
// naming nobody; a holder's repeat is not refused by the cap; and two claims
// racing for the ninth place yield exactly one.
func TestVoiceCameraCap(t *testing.T) {
	f, room := newCameraFixture(t)
	parts := []fakeParticipant{mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2")}
	var names []string
	for i := 1; i <= voiceCameraCap; i++ {
		name := fmt.Sprintf("member-%d", i)
		names = append(names, name)
		f.addPerson(t, name, false)
		decodeSession(t, f.session(t, name, "ops"))
		parts = append(parts, fakeParticipant{identity: f.identity(name), joinedMs: int64(3_000 + i)})
	}
	f.lk.setRoom(room, parts...)
	for _, name := range names[:voiceCameraCap-1] {
		f.mustCamera(t, name, "")
	}
	// Two race for the ninth place: ann and ann2, each held just before the
	// room's claims (the "lock" seam) until both are there, then let go
	// together.
	var arrived sync.WaitGroup
	arrived.Add(2)
	f.srv.voice.claimSeam = func(at string) {
		if at == "lock" {
			arrived.Done()
			arrived.Wait()
		}
	}
	var wg sync.WaitGroup
	results := make([]wireResult, 2)
	for i, who := range []string{"ann", "ann2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = f.camera(t, http.MethodPost, who, "ops", "")
		}()
	}
	wg.Wait()
	ok, refused := 0, 0
	for _, res := range results {
		switch {
		case res.status == http.StatusOK:
			ok++
		case res.status == http.StatusConflict && errCode(t, res.body) == schema.ErrorCodeVoiceCameraFull:
			refused++
			if strings.Contains(res.body, "ann") || strings.Contains(res.body, "member") {
				t.Errorf("the refusal names someone: %s", res.body)
			}
		default:
			t.Errorf("a racing claim: %d %s", res.status, res.body)
		}
	}
	f.srv.voice.claimSeam = nil
	if ok != 1 || refused != 1 {
		t.Fatalf("racing for the ninth place: %d granted, %d full; want one of each", ok, refused)
	}
	if n := len(f.cameraHolders(room)); n != voiceCameraCap {
		t.Errorf("%d claims, want the cap, %d", n, voiceCameraCap)
	}
	// A holder's repeat is answered, not refused by the cap.
	f.mustCamera(t, names[0], "")
	// The last of the ten is refused.
	if res := f.camera(t, http.MethodPost, names[voiceCameraCap-1], "ops", ""); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceCameraFull {
		t.Errorf("the tenth claim: %d %s, want 409 voice_camera_full", res.status, res.body)
	}
	if got := len(f.cameraRows(t)); got != voiceCameraCap {
		t.Errorf("%d camera rows, want one start per claim (%d)", got, voiceCameraCap)
	}
}

// TestVoiceCameraBound (N5, O12): claims are bounded per principal, a burst
// of voiceCameraBurst refilled at one every voiceCameraRefill. A continue and
// an un-named repeat count; past it the answer is 429 voice_camera_rate_limited
// and nothing reaches LiveKit; a release is never refused by it.
func TestVoiceCameraBound(t *testing.T) {
	f, room := newCameraFixture(t)
	f.at(1_000)
	c := f.mustCamera(t, "ann", "")
	for i := 1; i < voiceCameraBurst; i++ {
		q := ""
		if i%2 == 0 {
			q = named(c) // continues count as well as un-named repeats
		}
		f.mustCamera(t, "ann", q)
	}
	grants := f.lk.count("UpdateParticipant")
	res := f.camera(t, http.MethodPost, "ann", "ops", named(c))
	if res.status != http.StatusTooManyRequests || errCode(t, res.body) != schema.ErrorCodeVoiceCameraRateLimited {
		t.Fatalf("past the burst: %d %s, want 429 voice_camera_rate_limited", res.status, res.body)
	}
	if f.lk.count("UpdateParticipant") != grants {
		t.Error("a refused claim reached LiveKit")
	}
	// Not audited (§4.7): no row of any kind for the refusal.
	if rows := f.cameraRows(t); !slices.Equal(rows, []string{"ann started"}) {
		t.Errorf("rows %q, want the one start", rows)
	}
	// Someone else is not bounded by ann's claims.
	f.mustCamera(t, "ann2", "")
	// A release is never refused.
	f.mustStopCamera(t, named(c))
	if len(f.cameraHolders(room)) != 1 {
		t.Error("the release did not end ann's camera")
	}
	// One claim's worth comes back after voiceCameraRefill.
	f.at(1_000 + int(voiceCameraRefill/time.Millisecond))
	f.mustCamera(t, "ann", "")
	if res := f.camera(t, http.MethodPost, "ann", "ops", ""); res.status != http.StatusTooManyRequests {
		t.Errorf("a second claim after one refill: %d, want 429", res.status)
	}
}

// ---------------------------------------------------------------------------
// The release and the operator stop

// TestVoiceCameraRelease: the person's own DELETE ends their claim
// (reason=self), out of presence and written before LiveKit is asked; a name
// that is not the current claim is 409 voice_camera_stale and changes
// nothing; no claim is 204 and writes nothing; LiveKit down still ends the
// claim, and the pass withdraws it once LiveKit answers.
func TestVoiceCameraRelease(t *testing.T) {
	t.Run("own, named", func(t *testing.T) {
		f, room := newCameraFixture(t)
		c := f.mustCamera(t, "ann", "")
		f.mustStopCamera(t, named(c))
		if f.camGranted(room, "ann") || len(f.cameraHolders(room)) != 0 {
			t.Error("ann's camera is still granted or held")
		}
		if got := f.lastUpdate(t, "ann"); !slices.Equal(got, []string{"MICROPHONE"}) {
			t.Errorf("the stop wrote %q, want the microphone alone", got)
		}
		if rows := f.cameraRows(t); !slices.Equal(rows, []string{"ann started", "ann stopped principal=ann reason=self"}) {
			t.Errorf("rows %q", rows)
		}
	})
	t.Run("stale", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.at(1_000)
		c := f.mustCamera(t, "ann", "")
		f.mustStopCamera(t, "")
		f.at(2_000)
		newer := f.mustCamera(t, "ann", "")
		rows := f.cameraRows(t)
		res := f.camera(t, http.MethodDelete, "ann", "ops", named(c))
		if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceCameraStale {
			t.Fatalf("a stale release: %d %s, want 409 voice_camera_stale", res.status, res.body)
		}
		if !slices.Equal(f.cameraRows(t), rows) || !f.camGranted(room, "ann") {
			t.Error("a stale release changed something")
		}
		f.mustStopCamera(t, named(newer))
	})
	t.Run("nothing held", func(t *testing.T) {
		f, _ := newCameraFixture(t)
		f.mustStopCamera(t, "")
		if rows := f.cameraRows(t); len(rows) != 0 {
			t.Errorf("rows %q, want none", rows)
		}
		if res := f.camera(t, http.MethodDelete, "ann", "ops", "started_at=2020-01-01T00:00:00Z"); res.status != http.StatusConflict {
			t.Errorf("a named release with nothing held: %d, want 409", res.status)
		}
	})
	t.Run("malformed parameters, before the channel", func(t *testing.T) {
		f, _ := newCameraFixture(t)
		for _, q := range []string{"started_at=nope", "started_at=2020-01-01T00:00:00Z&started_at=2020-01-01T00:00:00Z", "principal=x", "principal=0", "principal=-3", "principal=07", "principal=1&principal=1"} {
			if res := f.camera(t, http.MethodDelete, "bob", "nosuch", q); res.status != http.StatusBadRequest || errCode(t, res.body) != "invalid_request" {
				t.Errorf("DELETE ?%s: %d %s, want 400 invalid_request", q, res.status, res.body)
			}
		}
	})
	t.Run("LiveKit down", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.mustCamera(t, "ann", "")
		f.lk.setUpdateStatus(http.StatusServiceUnavailable)
		f.mustStopCamera(t, "")
		if len(f.cameraHolders(room)) != 0 || len(f.cameras(t, f.ops, "")) != 0 {
			t.Error("the claim outlived its stop while LiveKit was down")
		}
		if !f.camGranted(room, "ann") {
			t.Fatal("the fake withdrew a camera it refused to")
		}
		f.lk.setUpdateStatus(0)
		f.at(500)
		f.pass(t)
		if f.camGranted(room, "ann") {
			t.Error("the pass did not withdraw the camera once LiveKit answered")
		}
		if rows := f.cameraRows(t); !slices.Equal(rows, []string{"ann started", "ann stopped principal=ann reason=self"}) {
			t.Errorf("rows %q: a grant alone writes nothing", rows)
		}
	})
}

// TestVoiceCameraOperatorStop (N1, N4): stopping someone else's camera is
// open to manage_channels alone, an owner's and an operator's, member or not,
// audited operator with the operator as the actor; an operator without it
// and a manager of the channel are refused 403 permission_denied, audited
// access_denied; a non-member without standing gets the uniform 404; an
// unknown principal is 204 to a caller with standing; started_at with
// principal naming someone else is 400.
func TestVoiceCameraOperatorStop(t *testing.T) {
	setup := func(t *testing.T) (*presenceFixture, string) {
		t.Helper()
		f, room := newCameraFixture(t)
		ctx := context.Background()
		restricted, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "operator-2")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.store.SetRole(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, restricted.ID, store.RoleOperator, nil); err != nil {
			t.Fatal(err)
		}
		_, tok, err := f.srv.store.CreateCredential(ctx, "system", restricted.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.ids["operator-2"], f.tokens["operator-2"] = restricted.ID, tok
		if _, err := f.srv.store.AddChannelMember(ctx, "system", f.ops.ID, restricted.ID, 0); err != nil {
			t.Fatal(err)
		}
		// operator-2 is a restricted operator: the column's manage_channels
		// is off. The owner (root, not a member) holds it whatever the cells
		// say; an operator who holds it is the subtest after the table.
		f.setCells(t, []schema.Permission{schema.PermManageChannels}, schema.PermissionRoleOperator)
		f.addPerson(t, "manager-1", false)
		yes := true
		if _, err := f.srv.store.PutChannelMember(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.ops.ID, f.ids["manager-1"], f.root.ID, nil, &yes); err != nil {
			t.Fatal(err)
		}
		f.mustCamera(t, "ann", "")
		return f, room
	}
	tests := []struct {
		name       string
		who        string
		query      string
		channel    string
		wantStatus int
		wantCode   string
		wantPerm   schema.Permission
		stopped    bool
		denied     bool
	}{
		{"the owner, not a member", "root", "principal=%[1]d", "ops", 204, "", "", true, false},
		{"an operator without manage_channels", "operator-2", "principal=%[1]d", "ops", 403, schema.ErrorCodePermissionDenied, schema.PermManageChannels, false, true},
		{"a manager of the channel", "manager-1", "principal=%[1]d", "ops", 403, schema.ErrorCodePermissionDenied, "", false, true},
		{"a member", "ann2", "principal=%[1]d", "ops", 403, schema.ErrorCodePermissionDenied, "", false, true},
		{"a non-member without standing", "bob", "principal=%[1]d", "ops", 404, "channel_not_found", "", false, false},
		{"an unknown channel, with standing", "root", "principal=%[1]d", "nosuch", 404, "channel_not_found", "", false, false},
		{"an unknown principal, with standing", "root", "principal=9999%[1]d", "ops", 204, "", "", false, false},
		{"started_at with someone else", "root", "principal=%[1]d&started_at=2020-01-01T00:00:00Z", "ops", 400, "invalid_request", "", false, false},
		{"started_at with someone else, without standing", "bob", "principal=%[1]d&started_at=2020-01-01T00:00:00Z", "ops", 400, "invalid_request", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := setup(t)
			denied := len(f.audits(t, "access_denied"))
			res := f.camera(t, http.MethodDelete, tt.who, tt.channel, fmt.Sprintf(tt.query, f.ids["ann"]))
			if res.status != tt.wantStatus || (tt.wantCode != "" && errCode(t, res.body) != tt.wantCode) {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if tt.wantStatus == http.StatusForbidden {
				if got := permissionOf(t, []byte(res.body)); got != tt.wantPerm {
					t.Errorf("the refusal names %q, want %q", got, tt.wantPerm)
				}
			}
			want := []string{"ann started"}
			if tt.stopped {
				want = append(want, tt.who+" stopped principal=ann reason=operator")
			}
			if got := f.cameraRows(t); !slices.Equal(got, want) {
				t.Errorf("rows %q, want %q", got, want)
			}
			if f.camGranted(room, "ann") == tt.stopped {
				t.Errorf("ann's camera granted = %v after the stop, want %v", f.camGranted(room, "ann"), !tt.stopped)
			}
			if n := len(f.audits(t, "access_denied")) - denied; (n == 1) != tt.denied || n > 1 {
				t.Errorf("%d access_denied rows, want denied=%v", n, tt.denied)
			}
		})
	}
	t.Run("an operator with manage_channels, not a member", func(t *testing.T) {
		f, room := newCameraFixture(t)
		ctx := context.Background()
		op, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "operator-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.srv.store.SetRole(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, op.ID, store.RoleOperator, nil); err != nil {
			t.Fatal(err)
		}
		_, tok, err := f.srv.store.CreateCredential(ctx, "system", op.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.ids["operator-1"], f.tokens["operator-1"] = op.ID, tok
		f.mustCamera(t, "ann", "")
		if res := f.camera(t, http.MethodDelete, "operator-1", "ops", fmt.Sprintf("principal=%d", f.ids["ann"])); res.status != http.StatusNoContent {
			t.Fatalf("operator stop: %d %s, want 204", res.status, res.body)
		}
		want := []string{"ann started", "operator-1 stopped principal=ann reason=operator"}
		if got := f.cameraRows(t); !slices.Equal(got, want) {
			t.Errorf("rows %q, want %q", got, want)
		}
		if f.camGranted(room, "ann") {
			t.Error("ann's camera is still granted")
		}
		// ann's own presence says an operator ended it; ann2's says nothing.
		if e := f.snapAs(t, f.ops, "ann").CameraEnded; e == nil || e.Reason != schema.VoiceCameraEndOperator {
			t.Errorf("ann's camera_ended = %+v, want operator", e)
		}
	})
}

// ---------------------------------------------------------------------------
// Presence, audit, redaction

// TestVoiceCameraEndedOnlyInOwnPresence: camera is true from the claim's
// answer until it ends, for everyone who may read presence; camera_ended
// comes in the same frame that takes camera away, in the person's own
// document alone, kept until they claim again, and dropped by a restart.
func TestVoiceCameraEndedOnlyInOwnPresence(t *testing.T) {
	f, _ := newCameraFixture(t)
	f.at(1_000)
	c := f.mustCamera(t, "ann", "")
	for _, viewer := range []string{"ann", "ann2", ""} {
		if got := f.cameras(t, f.ops, viewer); !slices.Equal(got, []string{"ann"}) {
			t.Errorf("%q sees cameras %q, want ann's", viewer, got)
		}
	}
	f.at(2_000)
	f.mustStopCamera(t, named(c))
	own := f.snapAs(t, f.ops, "ann")
	if own.CameraEnded == nil || own.CameraEnded.Reason != schema.VoiceCameraEndSelf || !own.CameraEnded.StartedAt.Time().Equal(c.StartedAt.Time()) {
		t.Fatalf("ann's camera_ended = %+v, want self for the claim", own.CameraEnded)
	}
	if len(f.cameras(t, f.ops, "ann")) != 0 {
		t.Error("ann's own document still shows a camera beside camera_ended")
	}
	for _, viewer := range []string{"ann2", ""} {
		if e := f.snapAs(t, f.ops, viewer).CameraEnded; e != nil {
			t.Errorf("%q's document carries camera_ended %+v", viewer, e)
		}
	}
	// Another channel's document does not carry it either.
	if e := f.snapAs(t, f.ops2, "ann").CameraEnded; e != nil {
		t.Errorf("ann's ops2 document carries camera_ended %+v", e)
	}
	// A new claim makes it old news.
	f.at(3_000)
	f.mustCamera(t, "ann", "")
	if e := f.snapAs(t, f.ops, "ann").CameraEnded; e != nil {
		t.Errorf("camera_ended %+v survived a new claim", e)
	}
}

// TestVoiceCameraRowsPerClaim: every claim that started has exactly one
// stopped row, whatever ended it, and the actor is the person for self, the
// operator for operator and the system for every end conchd decided.
func TestVoiceCameraRowsPerClaim(t *testing.T) {
	tests := []struct {
		reason string
		end    func(t *testing.T, f *presenceFixture, room string)
		actor  string
	}{
		{"self", func(t *testing.T, f *presenceFixture, _ string) { f.mustStopCamera(t, "") }, "ann"},
		{"operator", func(t *testing.T, f *presenceFixture, _ string) {
			if res := f.callREST(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/voice/camera?principal=%d", f.ids["ann"]), f.rootTok, ""); res.status != 204 {
				t.Fatalf("%d %s", res.status, res.body)
			}
		}, "root"},
		{"idle", func(t *testing.T, f *presenceFixture, _ string) { f.at(16_000); f.pass(t) }, "system"},
		{"left", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.drop(room, f.identity("ann"))
			f.at(1_500)
			f.pass(t)
		}, "system"},
		{"access_lost", func(t *testing.T, f *presenceFixture, _ string) {
			f.setCamera(t, false, schema.PermissionRoleMember)
			f.at(1_500)
			f.pass(t)
		}, "system"},
		{"rotated", func(t *testing.T, f *presenceFixture, _ string) {
			if code := f.doOrdered(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok); code != http.StatusNoContent {
				t.Fatalf("remove ann: %d", code)
			}
		}, "system"},
	}
	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			f, room := newCameraFixture(t)
			f.at(1_000)
			f.mustCamera(t, "ann", "")
			tt.end(t, f, room)
			// A further pass, and a further end of anything, writes nothing more.
			f.at(40_000)
			f.pass(t)
			f.srv.voice.endCamerasOf(context.Background(), f.ids["ann"], 0, voiceCameraStopAccessLost)
			want := []string{"ann started", tt.actor + " stopped principal=ann reason=" + tt.reason}
			if got := f.cameraRows(t); !slices.Equal(got, want) {
				t.Errorf("rows %q, want %q", got, want)
			}
		})
	}
}

// TestVoiceCameraNamesNoSecret: no token, API secret or room name in any
// camera row, log line or answer, across claims, stops, refusals and passes.
func TestVoiceCameraNamesNoSecret(t *testing.T) {
	f, room := newCameraFixture(t)
	var bodies []string
	keep := func(res wireResult) { bodies = append(bodies, res.body) }
	keep(f.camera(t, http.MethodPost, "ann", "ops", ""))
	keep(f.camera(t, http.MethodPost, "ann", "ops", "started_at=2020-01-01T00:00:00Z"))
	keep(f.camera(t, http.MethodPost, "ann", "ops", "participant=screen"))
	f.camTrack(room, "ann2", true, false) // unclaimed
	f.at(500)
	f.pass(t)
	f.lk.setUpdateStatus(http.StatusServiceUnavailable)
	keep(f.camera(t, http.MethodPost, "ann2", "ops", ""))
	f.lk.setUpdateStatus(0)
	keep(f.camera(t, http.MethodDelete, "ann2", "ops", fmt.Sprintf("principal=%d", f.ids["ann"])))
	keep(f.camera(t, http.MethodDelete, "ann", "ops", ""))
	var rows []string
	for _, e := range f.audit(t) {
		rows = append(rows, e.Actor+" "+e.Action+" "+e.Subject+" "+e.Detail)
	}
	text := strings.Join(rows, "\n") + f.logs.buf.String() + strings.Join(bodies, "\n")
	for what, secret := range map[string]string{
		"api secret": voiceTestSecret, "api key": voiceTestKey, "room name": room,
		"ann's login token": f.tokens["ann"], "ann2's login token": f.tokens["ann2"], "a track id": "TR_cam_",
	} {
		if strings.Contains(text, secret) {
			t.Errorf("%s appears in an audit row, log line or answer", what)
		}
	}
	for _, exercised := range []string{store.AuditVoiceCameraStarted, store.AuditVoiceCameraStopped, store.AuditVoiceCameraUnclaimed, schema.ErrorCodeVoiceCameraStale, schema.ErrorCodeVoiceUnavailable} {
		if !strings.Contains(text, exercised) {
			t.Errorf("the run did not exercise %q", exercised)
		}
	}
}
