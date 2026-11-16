package schema

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
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

// The companion's share audio (issue #898).

var voiceAudioStartedAt = NewTimestamp(time.Date(2026, 10, 9, 12, 0, 30, 0, time.UTC))

func TestVoiceCompanionAudioGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "request, application", file: "voice-companion-audio-request-v1.json", new: func() any { return new(VoiceCompanionAudioRequestV1) }},
		{name: "request, system excluding the call", file: "voice-companion-audio-request-v1-system.json", new: func() any { return new(VoiceCompanionAudioRequestV1) }},
		{name: "response", file: "voice-companion-audio-v1.json", new: func() any { return new(VoiceCompanionAudioV1) }},
		{name: "request, publisher person", file: "voice-companion-audio-request-v1-person.json", new: func() any { return new(VoiceCompanionAudioRequestV1) }},
		{name: "response, publisher person", file: "voice-companion-audio-v1-person.json", new: func() any { return new(VoiceCompanionAudioV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
			assertFixtureValidates(t, tt.file, tt.new)
		})
	}
}

func TestVoiceShareAudioScopeValid(t *testing.T) {
	tests := []struct {
		scope VoiceShareAudioScope
		want  bool
	}{
		{VoiceShareAudioScopeApplication, true},
		{VoiceShareAudioScopeSystemExcludingCall, true},
		{"", false},
		{"none", false}, // the absence of a consent is DELETE, not a value
		{"system", false},
		{"Application", false},
		{"application ", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.scope), func(t *testing.T) {
			if got := tt.scope.Valid(); got != tt.want {
				t.Errorf("%q.Valid() = %v, want %v", tt.scope, got, tt.want)
			}
			if err := (VoiceCompanionAudioRequestV1{Scope: tt.scope}).Validate(); (err == nil) != tt.want {
				t.Errorf("request with scope %q: Validate() = %v, want error %v", tt.scope, err, !tt.want)
			}
		})
	}
}

func TestVoiceCompanionAudioValidate(t *testing.T) {
	tests := []struct {
		name    string
		audio   VoiceCompanionAudioV1
		wantErr bool
	}{
		{name: "application", audio: VoiceCompanionAudioV1{Scope: VoiceShareAudioScopeApplication, StartedAt: voiceAudioStartedAt}},
		{name: "system excluding the call", audio: VoiceCompanionAudioV1{Scope: VoiceShareAudioScopeSystemExcludingCall, StartedAt: voiceAudioStartedAt}},
		{name: "no scope", audio: VoiceCompanionAudioV1{StartedAt: voiceAudioStartedAt}, wantErr: true},
		{name: "unknown scope", audio: VoiceCompanionAudioV1{Scope: "none", StartedAt: voiceAudioStartedAt}, wantErr: true},
		{name: "no started_at", audio: VoiceCompanionAudioV1{Scope: VoiceShareAudioScopeApplication}, wantErr: true},
		{name: "publisher companion", audio: VoiceCompanionAudioV1{Scope: VoiceShareAudioScopeApplication, Publisher: VoiceShareAudioPublisherCompanion, StartedAt: voiceAudioStartedAt}},
		{name: "publisher person", audio: VoiceCompanionAudioV1{Scope: VoiceShareAudioScopeApplication, Publisher: VoiceShareAudioPublisherPerson, StartedAt: voiceAudioStartedAt}},
		{name: "unknown publisher", audio: VoiceCompanionAudioV1{Scope: VoiceShareAudioScopeApplication, Publisher: "both", StartedAt: voiceAudioStartedAt}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.audio.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeVoiceCompanionAudioRequestV1(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    VoiceCompanionAudioRequestV1
		wantErr bool
	}{
		{name: "application", body: `{"scope":"application"}`, want: VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeApplication}},
		{name: "system excluding the call", body: `{"scope":"system_excluding_call"}`, want: VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeSystemExcludingCall}},
		{name: "publisher companion", body: `{"scope":"application","publisher":"companion"}`, want: VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeApplication, Publisher: VoiceShareAudioPublisherCompanion}},
		{name: "publisher person", body: `{"scope":"system_excluding_call","publisher":"person"}`, want: VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeSystemExcludingCall, Publisher: VoiceShareAudioPublisherPerson}},
		{name: "unknown publisher", body: `{"scope":"application","publisher":"both"}`, wantErr: true},
		{name: "wrong case publisher", body: `{"scope":"application","publisher":"Person"}`, wantErr: true},
		{name: "empty publisher", body: `{"scope":"application","publisher":""}`, want: VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeApplication}},
		{name: "null publisher is absent", body: `{"scope":"application","publisher":null}`, want: VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeApplication}},
		{name: "publisher not a string", body: `{"scope":"application","publisher":1}`, wantErr: true},
		{name: "publisher person, trailing junk", body: `{"scope":"application","publisher":"person"} x`, wantErr: true},
		{name: "none is not a scope", body: `{"scope":"none"}`, wantErr: true},
		{name: "unknown scope", body: `{"scope":"system"}`, wantErr: true},
		{name: "wrong case", body: `{"scope":"Application"}`, wantErr: true},
		{name: "missing scope", body: `{}`, wantErr: true},
		{name: "scope not a string", body: `{"scope":1}`, wantErr: true},
		{name: "unknown field", body: `{"scope":"application","identity":"p9.screen"}`, wantErr: true},
		{name: "unknown field, started_at", body: `{"scope":"application","started_at":"2026-10-09T12:00:30.000Z"}`, wantErr: true},
		{name: "two objects", body: `{"scope":"application"}{"scope":"application"}`, wantErr: true},
		{name: "trailing junk", body: `{"scope":"application"} x`, wantErr: true},
		{name: "null", body: `null`, wantErr: true},
		{name: "empty", body: ``, wantErr: true},
		{name: "array", body: `[]`, wantErr: true},
		{name: "string", body: `"application"`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DecodeVoiceCompanionAudioRequestV1(strings.NewReader(tt.body))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
			if err != nil && got != (VoiceCompanionAudioRequestV1{}) {
				t.Errorf("a refused body returned %+v", got)
			}
		})
	}
}

func TestDecodeVoiceCompanionAudioRequestV1Bound(t *testing.T) {
	body := `{"scope":"application"}` + strings.Repeat(" ", 128)
	limited := http.MaxBytesReader(nil, io.NopCloser(strings.NewReader(body)), 64)
	_, err := DecodeVoiceCompanionAudioRequestV1(limited)
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Errorf("err = %v, want an *http.MaxBytesError", err)
	}
}

func TestVoiceCompanionAudioRequestFixturesDecodeStrictly(t *testing.T) {
	tests := []struct {
		file string
		want VoiceCompanionAudioRequestV1
	}{
		{"voice-companion-audio-request-v1.json", VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeApplication}},
		{"voice-companion-audio-request-v1-system.json", VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeSystemExcludingCall}},
		{"voice-companion-audio-request-v1-person.json", VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeSystemExcludingCall, Publisher: VoiceShareAudioPublisherPerson}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			f, err := os.Open(filepath.Join("testdata", tt.file)) // #nosec G304 -- fixed fixture names
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = f.Close() }()
			got, err := DecodeVoiceCompanionAudioRequestV1(f)
			if err != nil || got != tt.want {
				t.Errorf("DecodeVoiceCompanionAudioRequestV1 = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestVoiceShareAudioScopeVocabulary(t *testing.T) {
	if got := Vocabularies["VoiceShareAudioScope"]; !slices.Equal(got, []string{"application", "system_excluding_call"}) {
		t.Errorf(`Vocabularies["VoiceShareAudioScope"] = %v, want [application system_excluding_call]`, got)
	}
}

func TestVoiceShareAudioPublisherVocabulary(t *testing.T) {
	if got := Vocabularies["VoiceShareAudioPublisher"]; !slices.Equal(got, []string{"companion", "person"}) {
		t.Errorf(`Vocabularies["VoiceShareAudioPublisher"] = %v, want [companion person]`, got)
	}
}

// An absent publisher is omitted on the wire, so a v1 request or response is
// byte-identical to what it always was (issue #1853: omitempty).
func TestVoiceCompanionAudioPublisherOmittedWhenAbsent(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"request", VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeApplication}, `{"scope":"application"}`},
		{"response", VoiceCompanionAudioV1{Scope: VoiceShareAudioScopeApplication, StartedAt: voiceAudioStartedAt}, `{"scope":"application","started_at":"2026-10-09T12:00:30.000Z"}`},
		{"request, person", VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeApplication, Publisher: VoiceShareAudioPublisherPerson}, `{"scope":"application","publisher":"person"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := json.Marshal(tt.v)
			if err != nil || string(b) != tt.want {
				t.Errorf("Marshal = %s, %v; want %s", b, err, tt.want)
			}
		})
	}
}

func TestVoiceCompanionAudioRequestEffectivePublisher(t *testing.T) {
	tests := []struct {
		in   VoiceShareAudioPublisher
		want VoiceShareAudioPublisher
	}{
		{"", VoiceShareAudioPublisherCompanion},
		{VoiceShareAudioPublisherCompanion, VoiceShareAudioPublisherCompanion},
		{VoiceShareAudioPublisherPerson, VoiceShareAudioPublisherPerson},
	}
	for _, tt := range tests {
		if got := (VoiceCompanionAudioRequestV1{Scope: VoiceShareAudioScopeApplication, Publisher: tt.in}).EffectivePublisher(); got != tt.want {
			t.Errorf("EffectivePublisher(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
