package schema

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestErrorCodeVoiceCompanionBusy(t *testing.T) {
	t.Parallel()
	if ErrorCodeVoiceCompanionBusy != "voice_companion_busy" {
		t.Fatalf("ErrorCodeVoiceCompanionBusy = %q, want %q", ErrorCodeVoiceCompanionBusy, "voice_companion_busy")
	}
}

func TestVoiceCompanionGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "request, companion that subscribes", file: "voice-companion-request-v1.json", new: func() any { return new(VoiceCompanionRequestV1) }},
		{name: "request, publish-only companion", file: "voice-companion-request-v1-publish-only.json", new: func() any { return new(VoiceCompanionRequestV1) }},
		{name: "request, viewer", file: "voice-companion-request-v1-viewer.json", new: func() any { return new(VoiceCompanionRequestV1) }},
		{name: "response, companion", file: "voice-companion-response-v1.json", new: func() any { return new(VoiceCompanionResponseV1) }},
		{name: "response, viewer", file: "voice-companion-response-v1-viewer.json", new: func() any { return new(VoiceCompanionResponseV1) }},
		{name: "session, naming the companion", file: "voice-session-response-v1-companion.json", new: func() any { return new(VoiceSessionResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
			assertFixtureValidates(t, tt.file, tt.new)
		})
	}
}

// The published session fixtures do not mention the new field: it is omitted
// when empty, so every pre-existing fixture is byte-identical.
func TestVoiceSessionCompanionIdentityIsOptional(t *testing.T) {
	for _, file := range []string{"voice-session-response-v1.json", "voice-session-response-v1-net.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixed fixture names
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "companion_identity") {
			t.Errorf("%s names companion_identity; published fixtures must not change", file)
		}
	}
}

func TestVoiceCompanionRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     VoiceCompanionRequestV1
		wantErr bool
	}{
		{name: "screen, subscribing", req: VoiceCompanionRequestV1{Publish: VoiceCompanionPublishScreen, Subscribe: true}},
		{name: "screen, publish-only", req: VoiceCompanionRequestV1{Publish: VoiceCompanionPublishScreen}},
		{name: "none, subscribing", req: VoiceCompanionRequestV1{Publish: VoiceCompanionPublishNone, Subscribe: true}},
		{name: "none, not subscribing", req: VoiceCompanionRequestV1{Publish: VoiceCompanionPublishNone}, wantErr: true},
		{name: "empty publish", req: VoiceCompanionRequestV1{Subscribe: true}, wantErr: true},
		{name: "camera", req: VoiceCompanionRequestV1{Publish: "camera", Subscribe: true}, wantErr: true},
		{name: "wrong case", req: VoiceCompanionRequestV1{Publish: "Screen", Subscribe: true}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.req.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestVoiceCompanionResponseValidate(t *testing.T) {
	good := func() VoiceCompanionResponseV1 {
		return VoiceCompanionResponseV1{
			LivekitURL: "wss://voice.example", Identity: "p7.screen", Room: "room", Token: "tok",
			Publish: VoiceCompanionPublishScreen, Subscribe: true, ExpiresAt: voiceExpiresAt,
		}
	}
	tests := []struct {
		name    string
		mutate  func(*VoiceCompanionResponseV1)
		wantErr bool
	}{
		{name: "valid", mutate: func(*VoiceCompanionResponseV1) {}},
		{name: "publish-only companion", mutate: func(r *VoiceCompanionResponseV1) { r.Subscribe = false }},
		{name: "viewer", mutate: func(r *VoiceCompanionResponseV1) { r.Publish, r.Identity = VoiceCompanionPublishNone, "p7.view" }},
		{name: "viewer that cannot subscribe", mutate: func(r *VoiceCompanionResponseV1) { r.Publish, r.Subscribe = VoiceCompanionPublishNone, false }, wantErr: true},
		{name: "no url", mutate: func(r *VoiceCompanionResponseV1) { r.LivekitURL = "" }, wantErr: true},
		{name: "no identity", mutate: func(r *VoiceCompanionResponseV1) { r.Identity = "" }, wantErr: true},
		{name: "no room", mutate: func(r *VoiceCompanionResponseV1) { r.Room = "" }, wantErr: true},
		{name: "no token", mutate: func(r *VoiceCompanionResponseV1) { r.Token = "" }, wantErr: true},
		{name: "no expiry", mutate: func(r *VoiceCompanionResponseV1) { r.ExpiresAt = Timestamp{} }, wantErr: true},
		{name: "unknown publish", mutate: func(r *VoiceCompanionResponseV1) { r.Publish = "camera" }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := good()
			tt.mutate(&r)
			if err := r.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestVoiceSessionCompanionIdentityValidate(t *testing.T) {
	grant := VoiceRoomGrant{Room: "r", Token: "t", CanPublish: true, ExpiresAt: voiceExpiresAt}
	tests := []struct {
		name      string
		identity  string
		companion string
		wantErr   bool
	}{
		{name: "absent", identity: "p7"},
		{name: "derived", identity: "p7", companion: "p7.screen"},
		{name: "equal to identity", identity: "p7", companion: "p7", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := VoiceSessionResponseV1{LivekitURL: "wss://x", Identity: tt.identity, Rooms: []VoiceRoomGrant{grant}, CompanionIdentity: tt.companion}
			if err := r.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeVoiceCompanionRequestV1(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    VoiceCompanionRequestV1
		wantErr bool
	}{
		{name: "companion", body: `{"publish":"screen","subscribe":true}`, want: VoiceCompanionRequestV1{Publish: "screen", Subscribe: true}},
		{name: "viewer", body: `{"publish":"none","subscribe":true}`, want: VoiceCompanionRequestV1{Publish: "none", Subscribe: true}},
		{name: "publish-only", body: `{"publish":"screen","subscribe":false}`, want: VoiceCompanionRequestV1{Publish: "screen"}},
		{name: "unknown field", body: `{"publish":"screen","subscribe":true,"identity":"p9.screen"}`, wantErr: true},
		{name: "unknown field, principal", body: `{"publish":"screen","subscribe":true,"principal_id":9}`, wantErr: true},
		{name: "two objects", body: `{"publish":"screen","subscribe":true}{"publish":"none","subscribe":true}`, wantErr: true},
		{name: "trailing junk", body: `{"publish":"screen","subscribe":true} x`, wantErr: true},
		{name: "null", body: `null`, wantErr: true},
		{name: "empty", body: ``, wantErr: true},
		{name: "array", body: `[]`, wantErr: true},
		{name: "camera", body: `{"publish":"camera","subscribe":true}`, wantErr: true},
		{name: "missing publish", body: `{"subscribe":true}`, wantErr: true},
		{name: "none without subscribe", body: `{"publish":"none","subscribe":false}`, wantErr: true},
		{name: "subscribe not a bool", body: `{"publish":"screen","subscribe":"yes"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeVoiceCompanionRequestV1(strings.NewReader(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			if err != nil && got != (VoiceCompanionRequestV1{}) {
				t.Errorf("a refused body returned %+v", got)
			}
		})
	}
}

func TestDecodeVoiceCompanionRequestV1Bound(t *testing.T) {
	body := `{"publish":"screen","subscribe":true}` + strings.Repeat(" ", 128)
	limited := http.MaxBytesReader(nil, io.NopCloser(strings.NewReader(body)), 64)
	_, err := DecodeVoiceCompanionRequestV1(limited)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Errorf("err = %v, want an *http.MaxBytesError", err)
	}
}

func TestVoiceCompanionRequestFixturesDecodeStrictly(t *testing.T) {
	tests := []struct {
		file string
		want VoiceCompanionRequestV1
	}{
		{"voice-companion-request-v1.json", VoiceCompanionRequestV1{Publish: VoiceCompanionPublishScreen, Subscribe: true}},
		{"voice-companion-request-v1-publish-only.json", VoiceCompanionRequestV1{Publish: VoiceCompanionPublishScreen}},
		{"voice-companion-request-v1-viewer.json", VoiceCompanionRequestV1{Publish: VoiceCompanionPublishNone, Subscribe: true}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tt.file)) // #nosec G304 -- fixed fixture names
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			got, err := DecodeVoiceCompanionRequestV1(f)
			if err != nil || got != tt.want {
				t.Errorf("DecodeVoiceCompanionRequestV1 = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestVoiceCompanionPublishVocabulary(t *testing.T) {
	if got := Vocabularies["VoiceCompanionPublish"]; !slices.Equal(got, []string{"screen", "none"}) {
		t.Errorf(`Vocabularies["VoiceCompanionPublish"] = %v, want [screen none]`, got)
	}
}
