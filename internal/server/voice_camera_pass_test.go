package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of what the passes do with cameras (ADR-015, issue #1241;
// docs/design/webcam-video.md §4.5, §4.6): idle, unclaimed cameras, kind and
// source together (S1), the entitlement on every pass, and a restart. Over
// the scripted LiveKit with the fixture's manual clock; nothing sleeps.

// removals lists the voice_participant_removed rows of the audit log, as
// "<principal name>[.<participant>] <reason>".
func (f *presenceFixture) removals(t *testing.T) []string {
	t.Helper()
	byID := map[string]string{}
	for name, id := range f.ids {
		byID[fmt.Sprint(id)] = name
	}
	var out []string
	for _, e := range f.audits(t, store.AuditVoiceParticipantRemoved) {
		fields := map[string]string{}
		for _, kv := range strings.Fields(e.Detail) {
			if k, v, ok := strings.Cut(kv, "="); ok {
				fields[k] = v
			}
		}
		who := byID[fields["principal"]]
		if p := fields["participant"]; p != "" {
			who += "." + p
		}
		out = append(out, who+" "+fields["reason"])
	}
	return out
}

// TestVoiceCameraIdle: a claim with no live camera track ends idle 15 s after
// it was made if none was ever seen, or 5 s after the last pass that saw one;
// a muted track counts as idle, and a live one keeps the claim.
func TestVoiceCameraIdle(t *testing.T) {
	tests := []struct {
		name string
		// track runs before each pass at ms; it sets ann's camera track.
		track  func(f *presenceFixture, room string, ms int)
		passes []int
		// endsAt is the first pass at which the claim is gone, 0 for none.
		endsAt int
	}{
		{"no track ever", func(*presenceFixture, string, int) {}, []int{5_000, 15_999, 16_000}, 16_000},
		{"a muted track only", func(f *presenceFixture, room string, _ int) { f.camTrack(room, "ann", true, true) }, []int{5_000, 15_999, 16_000}, 16_000},
		{"live, then gone", func(f *presenceFixture, room string, ms int) { f.camTrack(room, "ann", ms <= 2_000, false) },
			[]int{2_000, 6_999, 7_000}, 7_000},
		{"live, then muted", func(f *presenceFixture, room string, ms int) { f.camTrack(room, "ann", true, ms > 2_000) },
			[]int{2_000, 6_999, 7_000}, 7_000},
		{"live throughout", func(f *presenceFixture, room string, _ int) { f.camTrack(room, "ann", true, false) },
			[]int{2_000, 10_000, 20_000, 40_000}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newCameraFixture(t)
			f.at(1_000)
			f.mustCamera(t, "ann", "")
			ended := 0
			for _, ms := range tt.passes {
				tt.track(f, room, ms)
				f.at(ms)
				f.pass(t)
				if ended == 0 && len(f.cameraHolders(room)) == 0 {
					ended = ms
				}
			}
			if ended != tt.endsAt {
				t.Fatalf("the claim ended at %d, want %d", ended, tt.endsAt)
			}
			if tt.endsAt == 0 {
				if got := f.cameras(t, f.ops, ""); !slices.Equal(got, []string{"ann"}) {
					t.Errorf("presence shows cameras %q, want ann's", got)
				}
				return
			}
			want := []string{"ann started", "system stopped principal=ann reason=idle"}
			if got := f.cameraRows(t); !slices.Equal(got, want) {
				t.Errorf("rows %q, want %q", got, want)
			}
			if e := f.snapAs(t, f.ops, "ann").CameraEnded; e == nil || e.Reason != schema.VoiceCameraEndIdle {
				t.Errorf("camera_ended = %+v, want idle", e)
			}
			if f.camGranted(room, "ann") {
				t.Error("an idle claim's camera is still granted")
			}
		})
	}
}

// TestVoiceCameraDisplacedConnection (S5, the server's half): another
// device of the same person takes p<id>'s place. The claim is the person's
// and survives on the new connection, which publishes no camera: it ends
// idle 5 s after the last pass that saw the old one's.
func TestVoiceCameraDisplacedConnection(t *testing.T) {
	f, room := newCameraFixture(t)
	f.at(1_000)
	f.mustCamera(t, "ann", "")
	f.camTrack(room, "ann", true, false)
	f.at(2_000)
	f.pass(t)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 50_000}, mustParticipant(t, f, room, "ann2"))
	f.at(3_000)
	f.pass(t)
	if got := f.cameras(t, f.ops, ""); !slices.Equal(got, []string{"ann"}) {
		t.Fatalf("cameras %q right after the displacement, want ann's", got)
	}
	f.at(7_000)
	f.pass(t)
	if got := f.cameraHolders(room); len(got) != 0 {
		t.Errorf("claims %q 5 s after the last camera was seen, want none", got)
	}
}

// TestVoiceCameraUnclaimed: a camera nobody claimed is withdrawn, leaving the
// microphone (and the share, which nobody holds here); a published one writes
// voice_camera_unclaimed once per track, a grant alone writes nothing; an
// empty list of sources counts as every source; and a publisher whose camera
// LiveKit will not withdraw is removed, camera_unclaimed, or
// camera_not_withdrawn for a claim that ended.
func TestVoiceCameraUnclaimed(t *testing.T) {
	t.Run("a grant alone", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.cameraGranted = true })
		f.at(500)
		f.pass(t)
		if f.camGranted(room, "ann2") {
			t.Error("the grant was not withdrawn")
		}
		if got := f.lastUpdate(t, "ann2"); !slices.Equal(got, []string{"MICROPHONE"}) {
			t.Errorf("the withdrawal wrote %q, want the microphone", got)
		}
		if rows := f.cameraRows(t); len(rows) != 0 {
			t.Errorf("rows %q: a grant alone writes nothing", rows)
		}
	})
	t.Run("every source, by an empty list", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.allSources = true })
		f.at(500)
		f.pass(t)
		if p := mustParticipant(t, f, room, "ann2"); p.allSources || p.cameraGranted {
			t.Errorf("the empty list was not replaced: %+v", p)
		}
	})
	t.Run("a published camera, once per track", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.cameraGranted, p.camera = true, true })
		f.at(500)
		f.pass(t)
		f.at(1_000)
		f.pass(t)
		if p := mustParticipant(t, f, room, "ann2"); p.camera || p.cameraGranted {
			t.Errorf("the camera was not withdrawn: %+v", p)
		}
		want := []string{"ann2 unclaimed source=observed track_source=camera track_type=video"}
		if rows := f.cameraRows(t); !slices.Equal(rows, want) {
			t.Errorf("rows %q, want %q", rows, want)
		}
		if got := f.removals(t); len(got) != 0 {
			t.Errorf("removals %q: an honest withdrawal removes nobody", got)
		}
	})
	t.Run("LiveKit refuses the withdrawal", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.cameraGranted, p.camera = true, true })
		f.lk.setUpdateFail(f.identity("ann2"), true)
		f.lk.setRemoveStatus(http.StatusInternalServerError) // and the removal, so it is listed again
		f.at(500)
		f.pass(t)
		f.at(1_000)
		f.pass(t)
		want := []string{"ann2 unclaimed source=observed track_source=camera track_type=video"}
		if rows := f.cameraRows(t); !slices.Equal(rows, want) {
			t.Errorf("rows %q, want one row however many passes see it", rows)
		}
		f.lk.setRemoveStatus(0)
		f.at(1_500)
		f.pass(t)
		if f.lk.in(room, f.identity("ann2")) {
			t.Error("a camera LiveKit would not withdraw is still in the room")
		}
		if got := f.removals(t); !slices.Equal(got, []string{"ann2 camera_unclaimed"}) {
			t.Errorf("removals %q, want ann2 camera_unclaimed", got)
		}
	})
	t.Run("a claim that ended, LiveKit refusing", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.at(1_000)
		f.mustCamera(t, "ann", "")
		f.camTrack(room, "ann", true, false)
		f.lk.setUpdateFail(f.identity("ann"), true)
		f.mustStopCamera(t, "")
		f.at(1_500)
		f.pass(t)
		if f.lk.in(room, f.identity("ann")) {
			t.Error("an ended camera LiveKit would not withdraw is still in the room")
		}
		if got := f.removals(t); !slices.Equal(got, []string{"ann camera_not_withdrawn"}) {
			t.Errorf("removals %q, want ann camera_not_withdrawn", got)
		}
		// Its track was the claim's going away, not a camera nobody claimed.
		want := []string{"ann started", "ann stopped principal=ann reason=self"}
		if rows := f.cameraRows(t); !slices.Equal(rows, want) {
			t.Errorf("rows %q, want %q", rows, want)
		}
	})
	t.Run("a track that survives a confirmed withdrawal", func(t *testing.T) {
		f, room := newCameraFixture(t)
		f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.cameraGranted, p.camera = true, true })
		f.at(500)
		f.pass(t)
		// LiveKit answered the withdrawal, but the same track of the same
		// connection is listed again.
		f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.camera = true })
		f.at(1_000)
		f.pass(t)
		if f.lk.in(room, f.identity("ann2")) {
			t.Error("a camera that no withdrawal takes away is still in the room")
		}
		if got := f.removals(t); !slices.Equal(got, []string{"ann2 camera_unclaimed"}) {
			t.Errorf("removals %q, want ann2 camera_unclaimed", got)
		}
	})
}

// TestVoiceCameraKindAndSource is §4.5's table through the fake LiveKit
// (S1): each track whose kind and source disagree, a camera without a claim,
// a companion's camera and a viewer's are withdrawn and, where no withdrawal
// takes the track away or LiveKit refuses, their publisher removed; each
// writes voice_camera_unclaimed once, naming the declared source and type.
func TestVoiceCameraKindAndSource(t *testing.T) {
	tests := []struct {
		name string
		// set puts the track in the room; claim says ann claims first.
		set     func(t *testing.T, f *presenceFixture, room string)
		claim   bool
		refuse  string // an identity whose UpdateParticipant fails
		row     string
		removed []string
	}{
		{"video under the microphone", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
				p.tracks = []fakeTrack{{sid: "TR_v1", typ: "VIDEO", source: "MICROPHONE"}}
			})
		}, false, "", "ann2 unclaimed source=observed track_source=microphone track_type=video", []string{"ann2 camera_unclaimed"}},
		{"audio under the camera, withdrawn", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
				p.cameraGranted, p.tracks = true, []fakeTrack{{sid: "TR_a1", typ: "AUDIO", source: "CAMERA"}}
			})
		}, false, "", "ann2 unclaimed source=observed track_source=camera track_type=audio", nil},
		{"audio under the camera, LiveKit refusing", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
				p.cameraGranted, p.tracks = true, []fakeTrack{{sid: "TR_a1", typ: "AUDIO", source: "CAMERA"}}
			})
		}, false, "ann2", "ann2 unclaimed source=observed track_source=camera track_type=audio", []string{"ann2 camera_unclaimed"}},
		{"audio under the camera of a claim", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) {
				p.tracks = []fakeTrack{{sid: "TR_a2", typ: "AUDIO", source: "CAMERA"}}
			})
		}, true, "", "ann unclaimed source=observed track_source=camera track_type=audio", []string{"ann camera_unclaimed"}},
		{"video under the camera, no claim, LiveKit refusing", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) { p.cameraGranted, p.camera = true, true })
		}, false, "ann2", "ann2 unclaimed source=observed track_source=camera track_type=video", []string{"ann2 camera_unclaimed"}},
		{"video under the share's sound", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
				p.shareGranted, p.tracks = true, []fakeTrack{{sid: "TR_v2", typ: "VIDEO", source: "SCREEN_SHARE_AUDIO"}}
			})
		}, false, "", "ann2 unclaimed source=observed track_source=screen_share_audio track_type=video", nil},
		{"video under no source LiveKit names", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
				p.allSources, p.tracks = true, []fakeTrack{{sid: "TR_v3", typ: "VIDEO", source: "UNKNOWN"}}
			})
		}, false, "", "ann2 unclaimed source=observed track_source=unknown track_type=video", nil},
		{"a companion's camera, even with a claim", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.screenID("ann"), joinedMs: 4_000, cameraGranted: true, camera: true})...)
		}, true, "", "ann unclaimed source=observed track_source=camera track_type=video participant=screen", nil},
		{"a companion's camera, LiveKit refusing", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.screenID("ann"), joinedMs: 4_000, cameraGranted: true, camera: true})...)
		}, false, "screen", "ann unclaimed source=observed track_source=camera track_type=video participant=screen", []string{"ann.screen camera_unclaimed"}},
		{"a viewer's camera", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.viewID("ann"), joinedMs: 4_000, cameraGranted: true, camera: true})...)
		}, false, "", "ann unclaimed source=observed track_source=camera track_type=video participant=view", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newCameraFixture(t)
			if tt.claim {
				f.at(100)
				f.mustCamera(t, "ann", "")
				f.camTrack(room, "ann", true, false)
			}
			tt.set(t, f, room)
			switch tt.refuse {
			case "":
			case "screen":
				f.lk.setUpdateFail(f.screenID("ann"), true)
			default:
				f.lk.setUpdateFail(f.identity(tt.refuse), true)
			}
			for _, ms := range []int{500, 1_000, 1_500} {
				f.at(ms)
				f.pass(t)
			}
			var unclaimed []string
			for _, row := range f.cameraRows(t) {
				if strings.Contains(row, " unclaimed ") {
					unclaimed = append(unclaimed, row)
				}
			}
			if !slices.Equal(unclaimed, []string{tt.row}) {
				t.Errorf("unclaimed rows %q, want exactly %q", unclaimed, tt.row)
			}
			if got := f.removals(t); !slices.Equal(got, tt.removed) {
				t.Errorf("removals %q, want %q", got, tt.removed)
			}
			// Whoever stays publishes nothing unclaimed: the withdrawal took
			// the source, and ann's own claim, if any, kept its camera.
			for _, who := range []string{f.identity("ann"), f.identity("ann2"), f.screenID("ann"), f.viewID("ann")} {
				p, here := f.lk.participant(room, who)
				if !here {
					continue
				}
				if len(p.tracks) != 0 {
					t.Errorf("%s still publishes %+v", who, p.tracks)
				}
				if who != f.identity("ann") || !tt.claim {
					if p.camera || p.cameraGranted {
						t.Errorf("%s still has a camera: %+v", who, p)
					}
				}
			}
			if tt.claim && len(tt.removed) == 0 && !f.camGranted(room, "ann") {
				t.Error("judging another participant's track took ann's claimed camera away")
			}
		})
	}
}

// TestVoiceCameraGuest (S6, as main has it since A4): a guest holds no voice,
// so a guest's POST is refused at check 4, 403 voice_not_allowed, or 404 in a
// channel they are not in; and a guest's camera track, if one could appear,
// goes with the guest, whom the pass removes as not entitled.
func TestVoiceCameraGuest(t *testing.T) {
	f, room := newCameraFixture(t)
	ctx := context.Background()
	g, err := f.srv.store.CreateGuest(ctx, "system", store.NewGuest{Label: "Pat", Origin: schema.GuestOriginAddress,
		Channels: []int64{f.ops.ID}, Idle: 1 << 40, Absolute: 1 << 41, VoiceAllowed: true})
	if err != nil {
		t.Fatal(err)
	}
	f.ids["guest"], f.tokens["guest"] = g.Principal.ID, g.Token
	res := f.camera(t, http.MethodPost, "guest", "ops", "")
	if res.status != http.StatusForbidden || errCode(t, res.body) != schema.ErrorCodeVoiceNotAllowed {
		t.Errorf("a guest's claim: %d %s, want 403 voice_not_allowed", res.status, res.body)
	}
	if res := f.camera(t, http.MethodPost, "guest", "ops2", ""); res.status != http.StatusNotFound {
		t.Errorf("a guest's claim outside their channels: %d %s, want 404", res.status, res.body)
	}
	f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.identity("guest"), joinedMs: 4_000, cameraGranted: true, camera: true})...)
	f.at(500)
	f.pass(t)
	if f.lk.in(room, f.identity("guest")) {
		t.Error("a guest with a camera is still in the room")
	}
	if got := f.removals(t); !slices.Equal(got, []string{"guest " + voiceReasonVoiceOff}) {
		t.Errorf("removals %q, want the guest removed as holding no voice", got)
	}
	if got := f.cameraHolders(room); len(got) != 0 {
		t.Errorf("claims %q", got)
	}
}

// TestVoiceCameraRestart (K5): claims are in memory and a restart drops them,
// with their camera_ended. The first pass after it finds the camera granted
// and published with no claim: it withdraws it and audits the track as
// unclaimed, and removes nobody, since an honest client on a refreshed token
// lets LiveKit take it away.
func TestVoiceCameraRestart(t *testing.T) {
	f, room := newCameraFixture(t)
	f.at(1_000)
	f.mustCamera(t, "ann", "")
	f.camTrack(room, "ann", true, false)
	f.at(1_500)
	f.pass(t)
	f.restart()
	f.at(5_000)
	f.sweep(t)
	f.pass(t)
	if p := mustParticipant(t, f, room, "ann"); p.camera || p.cameraGranted {
		t.Errorf("after the restart LiveKit still lets ann publish a camera: %+v", p)
	}
	want := []string{"ann started", "ann unclaimed source=observed track_source=camera track_type=video"}
	if got := f.cameraRows(t); !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
	}
	if got := f.removals(t); len(got) != 0 {
		t.Errorf("removals %q: the first pass withdraws without removing", got)
	}
	if got := f.cameras(t, f.ops, "ann"); len(got) != 0 {
		t.Errorf("presence shows cameras %q after the restart", got)
	}
	if e := f.snapAs(t, f.ops, "ann").CameraEnded; e != nil {
		t.Errorf("camera_ended %+v survived the restart", e)
	}
}

// TestVoiceCameraRefreshedToken (K5): a token LiveKit sent the connection
// during a claim still grants the camera after the claim ended. A client
// that rejoins on it and publishes is a new connection with a new track: the
// pass withdraws it and audits it, and removes nobody.
func TestVoiceCameraRefreshedToken(t *testing.T) {
	f, room := newCameraFixture(t)
	f.at(1_000)
	f.mustCamera(t, "ann", "")
	f.camTrack(room, "ann", true, false)
	f.at(1_500)
	f.pass(t)
	f.mustStopCamera(t, "")
	f.at(2_000)
	f.pass(t)
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann"), joinedMs: 30_000, published: true, muted: true, cameraGranted: true, camera: true},
		mustParticipant(t, f, room, "ann2"))
	f.at(3_000)
	f.pass(t)
	if p := mustParticipant(t, f, room, "ann"); p.camera || p.cameraGranted {
		t.Errorf("the refreshed token's camera was not withdrawn: %+v", p)
	}
	want := []string{"ann started", "ann stopped principal=ann reason=self", "ann unclaimed source=observed track_source=camera track_type=video"}
	if got := f.cameraRows(t); !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
	}
	if got := f.removals(t); len(got) != 0 {
		t.Errorf("removals %q", got)
	}
}

// TestVoiceCameraEntitlementEveryPass: every pass reads use_camera for every
// claim's holder, so a change made without its hook (the store edited, a
// failed call) ends the claim access_lost within one pass: the role's cell
// turned off, the person's brake, and use_voice turned off.
func TestVoiceCameraEntitlementEveryPass(t *testing.T) {
	tests := []struct {
		name    string
		revoke  func(t *testing.T, f *presenceFixture)
		removed []string
	}{
		{"the role's cell", func(t *testing.T, f *presenceFixture) { f.setCamera(t, false, schema.PermissionRoleMember) }, nil},
		{"the brake", func(t *testing.T, f *presenceFixture) { f.brake(t, "ann") }, []string{"ann " + voiceReasonVoiceOff}},
		{"use_voice", func(t *testing.T, f *presenceFixture) {
			f.setCells(t, []schema.Permission{schema.PermUseCamera, schema.PermShareScreen, schema.PermUseVoice}, schema.PermissionRoleMember)
		}, []string{"ann " + voiceReasonVoiceOff, "ann2 " + voiceReasonVoiceOff}}, // the column: ann2 too
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newCameraFixture(t)
			f.at(1_000)
			f.mustCamera(t, "ann", "")
			f.camTrack(room, "ann", true, false)
			tt.revoke(t, f)
			f.at(1_500)
			f.pass(t)
			if got := f.cameraHolders(room); len(got) != 0 {
				t.Errorf("claims %q after the pass, want none", got)
			}
			want := []string{"ann started", "system stopped principal=ann reason=access_lost"}
			if got := f.cameraRows(t); !slices.Equal(got, want) {
				t.Errorf("rows %q, want %q", got, want)
			}
			if got := f.removals(t); !slices.Equal(got, tt.removed) {
				t.Errorf("removals %q, want %q", got, tt.removed)
			}
			if p, here := f.lk.participant(room, f.identity("ann")); here && (p.camera || p.cameraGranted) {
				t.Errorf("ann still has a camera: %+v", p)
			}
		})
	}
}
