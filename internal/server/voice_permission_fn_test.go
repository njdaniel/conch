package server

import (
	"reflect"
	"testing"

	"github.com/njdaniel/conch/internal/server/livekit"
)

// TestPublishPermission walks the rows of docs/design/webcam-video.md §4.3
// for a person, a companion and a viewer. The camera term is forced true here
// only: nothing in conchd sets it yet.
func TestPublishPermission(t *testing.T) {
	scr := livekit.SourceScreenShare
	tests := []struct {
		name   string
		role   voiceRole
		in     publishInput
		camera bool
		want   livekit.Permission
	}{
		// A person.
		{"listen-only", voiceRolePerson, publishInput{}, false, livekit.Permission{}},
		{"listen-only holding the share is still listen-only", voiceRolePerson, publishInput{share: true}, false, livekit.Permission{}},
		{"listen-only with a camera claim is still listen-only", voiceRolePerson, publishInput{share: true}, true, livekit.Permission{}},
		{"in voice", voiceRolePerson, publishInput{canPublish: true}, false, livekit.Permission{CanPublish: true}},
		{"holds the share", voiceRolePerson, publishInput{canPublish: true, share: true}, false, livekit.Permission{CanPublish: true, ScreenShare: true}},
		{"holds a camera", voiceRolePerson, publishInput{canPublish: true}, true, livekit.Permission{CanPublish: true, Camera: true}},
		{"both", voiceRolePerson, publishInput{canPublish: true, share: true}, true, livekit.Permission{CanPublish: true, ScreenShare: true, Camera: true}},
		{"a withdrawal is never handed canSubscribe", voiceRolePerson, publishInput{canPublish: true, canSubscribe: true}, false, livekit.Permission{CanPublish: true}},
		// A companion: the screen while the person holds the share, and its
		// own canSubscribe, known or not (false).
		{"companion, share held, subscribes", voiceRoleScreen, publishInput{canSubscribe: true, share: true}, false, livekit.SourcePermission(true, true, scr)},
		{"companion, share held, publish-only or unknown", voiceRoleScreen, publishInput{share: true}, false, livekit.SourcePermission(false, true, scr)},
		{"companion, no share, subscribes", voiceRoleScreen, publishInput{canSubscribe: true}, false, livekit.SourcePermission(true, false)},
		{"companion, no share, publish-only or unknown", voiceRoleScreen, publishInput{}, false, livekit.SourcePermission(false, false)},
		{"companion never gets the camera: the camera does not add to its share", voiceRoleScreen, publishInput{canSubscribe: true, share: true}, true, livekit.SourcePermission(true, true, scr)},
		{"companion never gets the camera: nor without the share", voiceRoleScreen, publishInput{canSubscribe: true}, true, livekit.SourcePermission(true, false)},
		{"companion is never handed canPublish by a person's input", voiceRoleScreen, publishInput{canPublish: true}, false, livekit.SourcePermission(false, false)},
		// A viewer publishes nothing, whatever its person holds.
		{"viewer, subscribes", voiceRoleView, publishInput{canSubscribe: true}, false, livekit.SourcePermission(true, false)},
		{"viewer, publish-only or unknown", voiceRoleView, publishInput{}, false, livekit.SourcePermission(false, false)},
		{"viewer, share and camera held", voiceRoleView, publishInput{canSubscribe: true, canPublish: true, share: true}, true, livekit.SourcePermission(true, false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := publishPermission(tt.role, tt.in, tt.camera); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("publishPermission(%v, %+v, camera=%v) = %+v, want %+v", tt.role, tt.in, tt.camera, got, tt.want)
			}
		})
	}
}
