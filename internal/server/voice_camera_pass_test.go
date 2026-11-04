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
		}, false, "", "ann2 unclaimed source=observed track_source=screen_share_audio track_type=video", []string{"ann2 camera_unclaimed"}},
		{"video under no source LiveKit names", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
				p.allSources, p.tracks = true, []fakeTrack{{sid: "TR_v3", typ: "VIDEO", source: "UNKNOWN"}}
			})
		}, false, "", "ann2 unclaimed source=observed track_source=unknown track_type=video", []string{"ann2 camera_unclaimed"}},
		{"a companion's camera, even with a claim", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.screenID("ann"), joinedMs: 4_000, cameraGranted: true, camera: true})...)
		}, true, "", "ann unclaimed source=observed track_source=camera track_type=video participant=screen", nil},
		{"a companion's camera, LiveKit refusing", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.screenID("ann"), joinedMs: 4_000, cameraGranted: true, camera: true})...)
		}, false, "screen", "ann unclaimed source=observed track_source=camera track_type=video participant=screen", []string{"ann.screen camera_unclaimed"}},
		// The screen share's sources, by kind as well (the review of #1258):
		// sound under SCREEN_SHARE, and anything but sound under
		// SCREEN_SHARE_AUDIO, is never the share, and is removed on the pass
		// that finds it, holding the share or not.
		{"audio under the screen share, no share held", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
				p.shareGranted, p.tracks = true, []fakeTrack{{sid: "TR_s1", typ: "AUDIO", source: "SCREEN_SHARE"}}
			})
		}, false, "", "ann2 unclaimed source=observed track_source=screen_share track_type=audio", []string{"ann2 camera_unclaimed"}},
		{"audio under the screen share, the share held", func(t *testing.T, f *presenceFixture, room string) {
			f.mustClaim(t, "ann")
			f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) {
				p.screen, p.tracks = true, []fakeTrack{{sid: "TR_s2", typ: "AUDIO", source: "SCREEN_SHARE"}}
			})
		}, false, "", "ann unclaimed source=observed track_source=screen_share track_type=audio", []string{"ann camera_unclaimed"}},
		{"audio under the screen share from the companion, the share held", func(t *testing.T, f *presenceFixture, room string) {
			f.mustClaim(t, "ann")
			f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.screenID("ann"), joinedMs: 4_000, shareGranted: true,
				tracks: []fakeTrack{{sid: "TR_s3", typ: "AUDIO", source: "SCREEN_SHARE"}}})...)
		}, false, "", "ann unclaimed source=observed track_source=screen_share track_type=audio participant=screen", []string{"ann.screen camera_unclaimed"}},
		{"data under the share's sound", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
				p.shareGranted, p.tracks = true, []fakeTrack{{sid: "TR_s4", typ: "DATA", source: "SCREEN_SHARE_AUDIO"}}
			})
		}, false, "", "ann2 unclaimed source=observed track_source=screen_share_audio track_type=data", []string{"ann2 camera_unclaimed"}},
		{"video under the share's sound, the share held", func(t *testing.T, f *presenceFixture, room string) {
			f.mustClaim(t, "ann")
			f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) {
				p.screen, p.tracks = true, []fakeTrack{{sid: "TR_s5", typ: "VIDEO", source: "SCREEN_SHARE_AUDIO"}}
			})
		}, false, "", "ann unclaimed source=observed track_source=screen_share_audio track_type=video", []string{"ann camera_unclaimed"}},
		// A companion's or a viewer's mismatched track: removed on the pass
		// that finds it, its per-source withdrawal not relied on.
		{"a companion's video under the microphone", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.screenID("ann"), joinedMs: 4_000, allSources: true,
				tracks: []fakeTrack{{sid: "TR_v4", typ: "VIDEO", source: "MICROPHONE"}}})...)
		}, false, "", "ann unclaimed source=observed track_source=microphone track_type=video participant=screen", []string{"ann.screen camera_unclaimed"}},
		{"a viewer's video under no source LiveKit names", func(t *testing.T, f *presenceFixture, room string) {
			f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.viewID("ann"), joinedMs: 4_000, allSources: true,
				tracks: []fakeTrack{{sid: "TR_v5", typ: "VIDEO", source: "UNKNOWN"}}})...)
		}, false, "", "ann unclaimed source=observed track_source=unknown track_type=video participant=view", []string{"ann.view camera_unclaimed"}},
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

// TestVoiceCameraOneTrackPerClaim (the security review of #1258): a claim
// covers one camera track, the first LiveKit lists. A second camera under the
// same claim would carry the cap's worth of cameras on one claim: it is
// audited once as unclaimed, and since withdrawing the camera would take the
// claim's own track too, its publisher is removed. The first alone is fine.
func TestVoiceCameraOneTrackPerClaim(t *testing.T) {
	f, room := newCameraFixture(t)
	f.at(100)
	c := f.mustCamera(t, "ann", "")
	f.camTrack(room, "ann", true, false)
	f.at(500)
	f.pass(t)
	if got := f.removals(t); len(got) != 0 {
		t.Fatalf("removals %q for one camera under one claim", got)
	}
	f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) {
		p.tracks = []fakeTrack{{sid: "TR_cam2", typ: "VIDEO", source: "CAMERA"}}
	})
	writes := len(f.lk.updatesFor(f.identity("ann")))
	f.at(1_000)
	f.pass(t)
	// The removal ends the claim on the same pass (reason=left), after the
	// removal's own row, and nothing was written to ann first.
	want := []string{"ann started", "ann unclaimed source=observed track_source=camera track_type=video", "system stopped principal=ann reason=left"}
	if got := f.cameraRows(t); !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
	}
	if n := len(f.lk.updatesFor(f.identity("ann"))) - writes; n != 0 {
		t.Errorf("%d UpdateParticipant requests for ann before her removal, want none", n)
	}
	if f.lk.in(room, f.identity("ann")) {
		t.Error("a second camera under one claim is still in the room")
	}
	if got := f.removals(t); !slices.Equal(got, []string{"ann camera_unclaimed"}) {
		t.Errorf("removals %q, want ann camera_unclaimed", got)
	}
	if e := f.snapAs(t, f.ops, "ann").CameraEnded; e == nil || e.Reason != schema.VoiceCameraEndLeft {
		t.Errorf("ann's camera_ended = %+v, want left", e)
	}
	// Her place under the cap is free, and a reconnect cannot continue the
	// claim she was removed with.
	if got := f.cameraHolders(room); len(got) != 0 {
		t.Errorf("claims %q after the removal, want none", got)
	}
	f.lk.setRoom(room, append(f.lk.rooms[room], fakeParticipant{identity: f.identity("ann"), joinedMs: 9_000})...)
	if res := f.camera(t, http.MethodPost, "ann", "ops", named(c)); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceCameraStale {
		t.Errorf("a continue of the claim she was removed with: %d %s, want 409 voice_camera_stale", res.status, res.body)
	}
}

// TestVoiceCameraMismatchIsRemovedOnThePassThatFindsIt (the review of
// #1258): a mismatched track that no withdrawal is known to take away (here
// video under a source LiveKit does not name) has its publisher removed on
// the pass that finds it, a claim's holder as anyone else: withdrawing first
// would only leave it published for another pass. A holder's camera is no
// shield.
func TestVoiceCameraMismatchIsRemovedOnThePassThatFindsIt(t *testing.T) {
	for _, holder := range []bool{false, true} {
		t.Run(fmt.Sprintf("holder=%v", holder), func(t *testing.T) {
			f, room := newCameraFixture(t)
			if holder {
				f.at(100)
				f.mustCamera(t, "ann", "")
				f.camTrack(room, "ann", true, false)
			}
			f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) {
				p.tracks = []fakeTrack{{sid: "TR_v9", typ: "VIDEO", source: "UNKNOWN"}}
			})
			f.at(500)
			f.pass(t)
			if f.lk.in(room, f.identity("ann")) {
				t.Error("the pass that found the mismatched track left its publisher in the room")
			}
			if got := f.removals(t); !slices.Equal(got, []string{"ann camera_unclaimed"}) {
				t.Errorf("removals %q, want ann camera_unclaimed", got)
			}
			var unclaimed []string
			for _, row := range f.cameraRows(t) {
				if strings.Contains(row, " unclaimed ") {
					unclaimed = append(unclaimed, row)
				}
			}
			if want := []string{"ann unclaimed source=observed track_source=unknown track_type=video"}; !slices.Equal(unclaimed, want) {
				t.Errorf("unclaimed rows %q, want %q", unclaimed, want)
			}
		})
	}
}

// TestVoiceShareIgnoresMismatchedShareTracks (the review of #1258): a share
// holder's sound under SCREEN_SHARE, or data under SCREEN_SHARE_AUDIO, is not
// the share. It does not keep the share from going idle (its last-seen time
// is not moved), and presence never says the share has sound for it, even
// while LiveKit will not let its publisher be removed.
func TestVoiceShareIgnoresMismatchedShareTracks(t *testing.T) {
	f, room := newCameraFixture(t)
	f.at(1_000)
	f.mustClaim(t, "ann")
	f.publish(room, "ann", true, false)
	f.at(2_000)
	f.pass(t)
	// The screen goes; what is left is sound under the screen and data under
	// the share's sound. LiveKit refuses the removal, so ann stays listed.
	f.lk.setRemoveStatus(http.StatusInternalServerError)
	f.lk.update(room, f.identity("ann"), func(p *fakeParticipant) {
		p.screen = false
		p.tracks = []fakeTrack{{sid: "TR_m1", typ: "AUDIO", source: "SCREEN_SHARE"}, {sid: "TR_m2", typ: "DATA", source: "SCREEN_SHARE_AUDIO"}}
	})
	for _, ms := range []int{3_000, 5_000, 6_999} {
		f.at(ms)
		f.pass(t)
		if got := f.sharing(t, f.ops); got != "ann" {
			t.Fatalf("at %d presence shows sharing %q, want ann without sound", ms, got)
		}
	}
	f.at(7_000)
	f.pass(t)
	if holder := f.shareState(room); holder != 0 {
		t.Errorf("the share is still held 5 s after its last screen track: a mismatched track kept it alive")
	}
	if got := f.removals(t); len(got) != 0 {
		t.Errorf("removals %q, want none (LiveKit refused them)", got)
	}
}

// TestVoiceCameraRemovalWithdrawsAGrantFirst (the security review of #1258,
// round 4): someone without a claim, found with the camera granted and a
// mismatched track, has the camera withdrawn before their removal, so the
// token LiveKit sends with the withdrawal, not one carrying the camera, is
// what a rejoin would use.
func TestVoiceCameraRemovalWithdrawsAGrantFirst(t *testing.T) {
	f, room := newCameraFixture(t)
	f.lk.update(room, f.identity("ann2"), func(p *fakeParticipant) {
		p.cameraGranted, p.tracks = true, []fakeTrack{{sid: "TR_w1", typ: "VIDEO", source: "MICROPHONE"}}
	})
	from := len(f.lk.calls())
	f.at(500)
	f.pass(t)
	var order []string
	for _, c := range f.lk.calls()[from:] {
		if c.identity == f.identity("ann2") && (c.method == "UpdateParticipant" || c.method == "RemoveParticipant") {
			order = append(order, c.method)
		}
	}
	if !slices.Equal(order, []string{"UpdateParticipant", "RemoveParticipant"}) {
		t.Errorf("calls for ann2 %q, want the withdrawal and then the removal", order)
	}
	if got := strings.Join(f.lastUpdate(t, "ann2"), ","); got != "MICROPHONE" {
		t.Errorf("the withdrawal wrote %s, want the microphone alone", got)
	}
	if got := f.removals(t); !slices.Equal(got, []string{"ann2 camera_unclaimed"}) {
		t.Errorf("removals %q, want ann2 camera_unclaimed", got)
	}
}
