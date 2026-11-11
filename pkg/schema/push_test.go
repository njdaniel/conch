package schema

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The Web Push keys of the fixtures are the published examples of RFC 8291
// §5 (a client's key and authentication secret) and RFC 8292 §2.4 (a
// server's VAPID key), so each is a real point on the curve and nobody's
// key.
const (
	testPushP256DH = "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"
	testPushAuth   = "BTBZMqHH6r4Tts7J_aSIgg"
	testPushVAPID  = "BA1Hxzyi1RUM1b5wjxsn7nGxAszw2u61m164i3MrAIxHF6YK5h4SDYic-dRuU_RCPCfA5aq9ojSwk5Y2EmClBPs"
	// testPushID is 32 bytes in unpadded base64url, as the server issues.
	testPushID = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
)

func TestPushGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"push-wake-v1.json", func() any { return new(PushWakeV1) }},
		{"push-registration-v1.json", func() any { return new(PushRegistrationV1) }},
		{"push-registration-v1-new.json", func() any { return new(PushRegistrationV1) }},
		{"create-push-registration-request-v1-webpush.json", func() any { return new(CreatePushRegistrationRequestV1) }},
		{"create-push-registration-request-v1-relay.json", func() any { return new(CreatePushRegistrationRequestV1) }},
		{"update-push-registration-request-v1.json", func() any { return new(UpdatePushRegistrationRequestV1) }},
		{"list-own-push-registrations-response-v1.json", func() any { return new(ListOwnPushRegistrationsResponseV1) }},
		{"list-own-push-registrations-response-v1-empty.json", func() any { return new(ListOwnPushRegistrationsResponseV1) }},
		{"push-config-v1.json", func() any { return new(PushConfigV1) }},
		{"push-config-v1-off.json", func() any { return new(PushConfigV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// assertValidFixture decodes the named fixture into v and fails if Validate
// refuses it: a fixture is what a real peer sends.
func assertValidFixture(t *testing.T, file string, v validator) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name from this package's own test table
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if err := v.Validate(); err != nil {
		t.Errorf("%s: %v", file, err)
	}
}

func TestPushFixturesValidate(t *testing.T) {
	tests := []struct {
		file string
		v    validator
	}{
		{"push-wake-v1.json", new(PushWakeV1)},
		{"push-registration-v1.json", new(PushRegistrationV1)},
		{"push-registration-v1-new.json", new(PushRegistrationV1)},
		{"create-push-registration-request-v1-webpush.json", new(CreatePushRegistrationRequestV1)},
		{"create-push-registration-request-v1-relay.json", new(CreatePushRegistrationRequestV1)},
		{"update-push-registration-request-v1.json", new(UpdatePushRegistrationRequestV1)},
		{"list-own-push-registrations-response-v1.json", new(ListOwnPushRegistrationsResponseV1)},
		{"list-own-push-registrations-response-v1-empty.json", new(ListOwnPushRegistrationsResponseV1)},
		{"push-config-v1.json", new(PushConfigV1)},
		{"push-config-v1-off.json", new(PushConfigV1)},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertValidFixture(t, tt.file, tt.v)
		})
	}
}

// wantRefusal fails unless err is a refusal that names the field.
func wantRefusal(t *testing.T, err error, field string) {
	t.Helper()
	if field == "" {
		if err != nil {
			t.Errorf("Validate() = %v, want nil", err)
		}
		return
	}
	if err == nil {
		t.Errorf("Validate() = nil, want a refusal naming %q", field)
		return
	}
	if !strings.Contains(err.Error(), field) {
		t.Errorf("Validate() = %q, want it to name %q", err, field)
	}
}

func TestPushWakeV1Validate(t *testing.T) {
	tests := []struct {
		name string
		wake PushWakeV1
		// field is what the refusal names; empty for a wake that passes.
		field string
	}{
		{"a wake as the server sends it", NewPushWakeV1(testPushID), ""},
		{"the shortest id, 16 bytes", NewPushWakeV1(strings.Repeat("A", 22)), ""},
		{"the longest id, 64 bytes", NewPushWakeV1(strings.Repeat("A", 86)), ""},
		{"no schema", PushWakeV1{Registration: testPushID}, "schema"},
		{"another schema", PushWakeV1{Schema: "conch.push_wake.v2", Registration: testPushID}, "schema"},
		{"a summary's schema", PushWakeV1{Schema: NotificationSummarySchemaV1, Registration: testPushID}, "schema"},
		{"an empty id", NewPushWakeV1(""), "registration"},
		{"an id of 15 bytes", NewPushWakeV1(strings.Repeat("A", 20)), "at least 16 bytes"},
		{"an id of 65 bytes", NewPushWakeV1(strings.Repeat("A", 87)), "at most 86"},
		{"a padded id", NewPushWakeV1(strings.Repeat("A", 22) + "=="), "unpadded base64url"},
		{"standard base64, not url", NewPushWakeV1(strings.Repeat("A", 21) + "+"), "unpadded base64url"},
		{"a slash", NewPushWakeV1(strings.Repeat("A", 21) + "/"), "unpadded base64url"},
		{"a space", NewPushWakeV1(strings.Repeat("A", 21) + " "), "unpadded base64url"},
		{"a length no encoding has", NewPushWakeV1(strings.Repeat("A", 25)), "unpadded base64url"},
		{"trailing bits that are not zero", NewPushWakeV1(strings.Repeat("A", 21) + "B"), "unpadded base64url"},
		{"a number", NewPushWakeV1("12"), "registration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefusal(t, tt.wake.Validate(), tt.field)
		})
	}
}

// TestPushWakeV1HasTwoKeys pins the wake's key set. Nothing may be added to
// it (design §4.2): a new field is read by a push service.
func TestPushWakeV1HasTwoKeys(t *testing.T) {
	if got, want := jsonKeys(t, NewPushWakeV1(testPushID)), "registration,schema"; got != want {
		t.Errorf("PushWakeV1 keys = %s, want %s", got, want)
	}
	raw, err := json.Marshal(NewPushWakeV1(testPushID))
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"schema":"conch.push_wake.v1","registration":"` + testPushID + `"}`; string(raw) != want {
		t.Errorf("the wake encodes as %s, want %s", raw, want)
	}
}

func TestPushRegistrationIDConstants(t *testing.T) {
	if got := base64.RawURLEncoding.EncodedLen(PushRegistrationIDBytes); got != PushRegistrationIDLength {
		t.Errorf("PushRegistrationIDLength = %d, but %d bytes encode to %d", PushRegistrationIDLength, PushRegistrationIDBytes, got)
	}
	if got := base64.RawURLEncoding.EncodedLen(MaxPushRegistrationIDBytes); got != MaxPushRegistrationIDLength {
		t.Errorf("MaxPushRegistrationIDLength = %d, but %d bytes encode to %d", MaxPushRegistrationIDLength, MaxPushRegistrationIDBytes, got)
	}
	if MinPushRegistrationIDBytes*8 < 128 {
		t.Errorf("MinPushRegistrationIDBytes = %d, less than the design's 128 bits", MinPushRegistrationIDBytes)
	}
	if len(testPushID) != PushRegistrationIDLength {
		t.Errorf("the fixtures' id is %d characters, want %d", len(testPushID), PushRegistrationIDLength)
	}
}

func webpushRequest() CreatePushRegistrationRequestV1 {
	return CreatePushRegistrationRequestV1{
		Transport: PushTransportWebPush,
		Platform:  PushPlatformAndroid,
		App:       "org.example.conch",
		Endpoint:  "https://push.example.net/push/JzLQ3raZJfFBR0aqvOMsLrt54w4rJUsV",
		P256DH:    testPushP256DH,
		Auth:      testPushAuth,
	}
}

func relayRequest() CreatePushRegistrationRequestV1 {
	return CreatePushRegistrationRequestV1{
		Transport: PushTransportRelay,
		Platform:  PushPlatformIOS,
		App:       "org.example.conch",
		Provider:  PushProviderAPNs,
		Token:     strings.Repeat("0f", 32),
	}
}

// b64 is unpadded base64url of raw.
func b64(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }

func TestPushRegistrationRequestValidate(t *testing.T) {
	point, err := base64.RawURLEncoding.DecodeString(testPushP256DH)
	if err != nil {
		t.Fatal(err)
	}
	// A 65-byte value that starts 0x04 and is not on the curve.
	offCurve := append([]byte{}, point...)
	offCurve[64] ^= 0x01
	// The same point compressed: 33 bytes, the first 0x02 or 0x03.
	compressed := append([]byte{0x02 + point[64]&1}, point[1:33]...)
	wrongLead := append([]byte{}, point...)
	wrongLead[0] = 0x05
	longEndpoint := "https://push.example.net/" + strings.Repeat("a", MaxPushEndpointBytes-len("https://push.example.net/"))

	web := func(edit func(*CreatePushRegistrationRequestV1)) CreatePushRegistrationRequestV1 {
		r := webpushRequest()
		edit(&r)
		return r
	}
	relay := func(edit func(*CreatePushRegistrationRequestV1)) CreatePushRegistrationRequestV1 {
		r := relayRequest()
		edit(&r)
		return r
	}
	tests := []struct {
		name  string
		req   CreatePushRegistrationRequestV1
		field string
	}{
		{"a webpush registration", webpushRequest(), ""},
		{"a relay registration", relayRequest(), ""},
		{"a relay registration to fcm", relay(func(r *CreatePushRegistrationRequestV1) {
			r.Provider, r.Platform, r.Token = PushProviderFCM, PushPlatformAndroid, "fMEP0vJqS0:APA91bH-example_token"
		}), ""},
		{"padded keys", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH += "="; r.Auth += "==" }), ""},
		{"an endpoint at the bound", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = longEndpoint }), ""},
		{"an endpoint with a port and a query", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://ntfy.example.org:8443/upABC?up=1" }), ""},
		{"an origin as the app", web(func(r *CreatePushRegistrationRequestV1) {
			r.Platform, r.App = PushPlatformWeb, "https://chat.example.org:8443"
		}), ""},
		{"an app at the bound", web(func(r *CreatePushRegistrationRequestV1) { r.App = strings.Repeat("a", MaxPushAppBytes) }), ""},
		{"a token at the bound", relay(func(r *CreatePushRegistrationRequestV1) { r.Token = strings.Repeat("a", MaxPushTokenBytes) }), ""},

		// The three fields every registration has.
		{"no transport", web(func(r *CreatePushRegistrationRequestV1) { r.Transport = "" }), "transport"},
		{"an unknown transport", web(func(r *CreatePushRegistrationRequestV1) { r.Transport = "apns" }), "transport"},
		{"no platform", web(func(r *CreatePushRegistrationRequestV1) { r.Platform = "" }), "platform"},
		{"an unknown platform", web(func(r *CreatePushRegistrationRequestV1) { r.Platform = "windows" }), "platform"},
		{"no app", web(func(r *CreatePushRegistrationRequestV1) { r.App = "" }), "app"},
		{"an app past the bound", web(func(r *CreatePushRegistrationRequestV1) { r.App = strings.Repeat("a", MaxPushAppBytes+1) }), "app"},
		{"an app with a space", web(func(r *CreatePushRegistrationRequestV1) { r.App = "org.example conch" }), "app"},
		{"an app with a letter outside ASCII", web(func(r *CreatePushRegistrationRequestV1) { r.App = "org.example.caf" + string(rune(0xe9)) }), "app"},
		{"an app with a control character", relay(func(r *CreatePushRegistrationRequestV1) { r.App = "org.example\n.conch" }), "app"},
		{"an app with a query", relay(func(r *CreatePushRegistrationRequestV1) { r.App = "org.example?x" }), "app"},

		// webpush: exactly endpoint, p256dh and auth.
		{"webpush with a provider", web(func(r *CreatePushRegistrationRequestV1) { r.Provider = PushProviderFCM }), "provider"},
		{"webpush with a token", web(func(r *CreatePushRegistrationRequestV1) { r.Token = "abc" }), "token"},
		{"webpush without an endpoint", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "" }), "endpoint"},
		{"webpush without p256dh", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH = "" }), "p256dh"},
		{"webpush without auth", web(func(r *CreatePushRegistrationRequestV1) { r.Auth = "" }), "auth"},

		// The endpoint.
		{"an http endpoint", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "http://push.example.net/x" }), "endpoint must be an https URL"},
		{"another scheme", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "wss://push.example.net/x" }), "endpoint must be an https URL"},
		{"a relative endpoint", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "/push/x" }), "endpoint must be an https URL"},
		{"a scheme-relative endpoint", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "//push.example.net/x" }), "endpoint must be an https URL"},
		{"an endpoint with no host", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https:///push/x" }), "endpoint must be an absolute URL with a host"},
		{"an endpoint with only a port", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://:443/x" }), "endpoint must be an absolute URL with a host"},
		{"an opaque endpoint", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https:push.example.net" }), "endpoint must be an absolute URL with a host"},
		{"an endpoint with userinfo", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://user:pw@push.example.net/x" }), "endpoint must not have userinfo"},
		{"an endpoint with an empty userinfo", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://@push.example.net/x" }), "endpoint must not have userinfo"},
		{"an endpoint with a fragment", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://push.example.net/x#frag" }), "endpoint must not have a fragment"},
		{"an endpoint with an empty fragment", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://push.example.net/x#" }), "endpoint must not have a fragment"},
		{"an endpoint past the bound", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = longEndpoint + "a" }), "endpoint must be at most 1000 bytes"},
		{"an endpoint with a space", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://push.example.net/a b" }), "endpoint may hold only printable ASCII"},
		{"an endpoint with a newline", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://push.example.net/a\nb" }), "endpoint may hold only printable ASCII"},
		{"an endpoint outside ASCII", web(func(r *CreatePushRegistrationRequestV1) {
			r.Endpoint = "https://push.example.net/caf" + string(rune(0xe9))
		}), "endpoint may hold only printable ASCII"},
		{"an endpoint that does not parse", web(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://push.example.net:port/x" }), "endpoint must be a URL"},

		// The keys.
		{"p256dh that is not base64url", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH = strings.ReplaceAll(testPushP256DH, "_", "/") }), "p256dh must be base64url"},
		{"p256dh with padding in the middle", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH = "BCVx=" + testPushP256DH[4:] }), "p256dh must be base64url"},
		{"p256dh of 64 bytes", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH = b64(point[:64]) }), "p256dh must be an uncompressed P-256 point"},
		{"p256dh of 66 bytes", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH = b64(append(append([]byte{}, point...), 0)) }), "p256dh must be an uncompressed P-256 point"},
		{"a compressed point", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH = b64(compressed) }), "p256dh must be an uncompressed P-256 point"},
		{"a point whose first byte is not 0x04", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH = b64(wrongLead) }), "p256dh must be an uncompressed P-256 point"},
		{"a point that is not on the curve", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH = b64(offCurve) }), "p256dh must be a point on the P-256 curve"},
		{"a very long p256dh", web(func(r *CreatePushRegistrationRequestV1) { r.P256DH = strings.Repeat("A", 4096) }), "p256dh is too long"},
		{"auth that is not base64url", web(func(r *CreatePushRegistrationRequestV1) { r.Auth = "BTBZMqHH6r4Tts7J+aSIgg" }), "auth must be base64url"},
		{"auth of 15 bytes", web(func(r *CreatePushRegistrationRequestV1) { r.Auth = b64(make([]byte, 15)) }), "auth must hold 16 bytes"},
		{"auth of 17 bytes", web(func(r *CreatePushRegistrationRequestV1) { r.Auth = b64(make([]byte, 17)) }), "auth must hold 16 bytes"},

		// relay: exactly provider and token.
		{"relay with an endpoint", relay(func(r *CreatePushRegistrationRequestV1) { r.Endpoint = "https://push.example.net/x" }), "endpoint"},
		{"relay with p256dh", relay(func(r *CreatePushRegistrationRequestV1) { r.P256DH = testPushP256DH }), "p256dh"},
		{"relay with auth", relay(func(r *CreatePushRegistrationRequestV1) { r.Auth = testPushAuth }), "auth"},
		{"relay without a provider", relay(func(r *CreatePushRegistrationRequestV1) { r.Provider = "" }), "provider"},
		{"relay with an unknown provider", relay(func(r *CreatePushRegistrationRequestV1) { r.Provider = "hms" }), "provider"},
		{"relay without a token", relay(func(r *CreatePushRegistrationRequestV1) { r.Token = "" }), "token"},
		{"a token past the bound", relay(func(r *CreatePushRegistrationRequestV1) { r.Token = strings.Repeat("a", MaxPushTokenBytes+1) }), "token"},
		{"a token with a space", relay(func(r *CreatePushRegistrationRequestV1) { r.Token = "ab cd" }), "token"},
		{"a token with a control character", relay(func(r *CreatePushRegistrationRequestV1) { r.Token = "ab\x00cd" }), "token"},
		{"a token outside ASCII", relay(func(r *CreatePushRegistrationRequestV1) { r.Token = "ab" + string(rune(0xe9)) }), "token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefusal(t, tt.req.Validate(), tt.field)
			// The update request has the same fields and the same rules.
			wantRefusal(t, UpdatePushRegistrationRequestV1(tt.req).Validate(), tt.field)
		})
	}
}

// A refusal never repeats the target: an endpoint and a device token are
// secrets, and a refusal is logged and sent back.
func TestPushRegistrationRequestRefusalsHoldNoTarget(t *testing.T) {
	const secret = "s3cr3t-target-value"
	tests := []CreatePushRegistrationRequestV1{
		func() CreatePushRegistrationRequestV1 {
			r := webpushRequest()
			r.Endpoint = "http://push.example.net/" + secret
			return r
		}(),
		func() CreatePushRegistrationRequestV1 {
			r := webpushRequest()
			r.Endpoint = "https://push.example.net:" + secret + "/x"
			return r
		}(),
		func() CreatePushRegistrationRequestV1 {
			r := webpushRequest()
			r.Token = secret
			return r
		}(),
		func() CreatePushRegistrationRequestV1 {
			r := relayRequest()
			r.Token = secret + " with a space"
			return r
		}(),
		func() CreatePushRegistrationRequestV1 {
			r := relayRequest()
			r.Endpoint = "https://push.example.net/" + secret
			return r
		}(),
		func() CreatePushRegistrationRequestV1 {
			r := webpushRequest()
			r.Auth = secret
			return r
		}(),
		func() CreatePushRegistrationRequestV1 {
			r := webpushRequest()
			r.App = secret + " "
			return r
		}(),
	}
	for i, req := range tests {
		err := req.Validate()
		if err == nil {
			t.Errorf("case %d: Validate() = nil, want a refusal", i)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("case %d: the refusal %q repeats the target", i, err)
		}
	}
}

func TestPushRegistrationRequestNormalized(t *testing.T) {
	padded := webpushRequest()
	padded.P256DH += "="
	padded.Auth += "=="
	if err := padded.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := padded.Normalized(); got != webpushRequest() {
		t.Errorf("Normalized() = %+v, want the unpadded request", got)
	}
	if got := webpushRequest().Normalized(); got != webpushRequest() {
		t.Errorf("Normalized() changed an unpadded request: %+v", got)
	}
	if got := relayRequest().Normalized(); got != relayRequest() {
		t.Errorf("Normalized() changed a relay request: %+v", got)
	}
	update := UpdatePushRegistrationRequestV1(padded)
	if got := update.Normalized(); got != UpdatePushRegistrationRequestV1(webpushRequest()) {
		t.Errorf("the update's Normalized() = %+v, want the unpadded request", got)
	}
}

// The two requests have the same fields, so one conversion serves both and
// a field added to one and not the other does not compile.
func TestPushRegistrationRequestsHaveTheSameKeys(t *testing.T) {
	create := webpushRequest()
	create.Provider, create.Token = PushProviderAPNs, "t"
	if got, want := jsonKeys(t, UpdatePushRegistrationRequestV1(create)), jsonKeys(t, create); got != want {
		t.Errorf("the update request's keys = %s, the create request's %s", got, want)
	}
	if got, want := jsonKeys(t, create), "app,auth,endpoint,p256dh,platform,provider,token,transport"; got != want {
		t.Errorf("the request's keys = %s, want %s", got, want)
	}
}

func validRegistration() PushRegistrationV1 {
	return PushRegistrationV1{
		ID:                testPushID,
		Transport:         PushTransportWebPush,
		Platform:          PushPlatformAndroid,
		App:               "org.example.conch",
		TargetFingerprint: "5f2a9c0b71d3e846",
		CreatedAt:         NewTimestamp(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)),
	}
}

func TestPushRegistrationV1Validate(t *testing.T) {
	edit := func(f func(*PushRegistrationV1)) PushRegistrationV1 {
		r := validRegistration()
		f(&r)
		return r
	}
	seen := NewTimestamp(time.Date(2026, 11, 10, 12, 30, 0, 0, time.UTC))
	tests := []struct {
		name  string
		reg   PushRegistrationV1
		field string
	}{
		{"a new registration", validRegistration(), ""},
		{"one that has been woken and has fetched", edit(func(r *PushRegistrationV1) { r.LastWakeAt, r.LastFetchAt = &seen, &seen }), ""},
		{"no id", edit(func(r *PushRegistrationV1) { r.ID = "" }), "id"},
		{"a short id", edit(func(r *PushRegistrationV1) { r.ID = "AAAA" }), "id"},
		{"an unknown transport", edit(func(r *PushRegistrationV1) { r.Transport = "sms" }), "transport"},
		{"an unknown platform", edit(func(r *PushRegistrationV1) { r.Platform = "" }), "platform"},
		{"no app", edit(func(r *PushRegistrationV1) { r.App = "" }), "app"},
		{"no fingerprint", edit(func(r *PushRegistrationV1) { r.TargetFingerprint = "" }), "target_fingerprint"},
		{"a short fingerprint", edit(func(r *PushRegistrationV1) { r.TargetFingerprint = "5f2a9c0b" }), "target_fingerprint"},
		{"an uppercase fingerprint", edit(func(r *PushRegistrationV1) { r.TargetFingerprint = "5F2A9C0B71D3E846" }), "target_fingerprint"},
		{"a fingerprint that is not hex", edit(func(r *PushRegistrationV1) { r.TargetFingerprint = "5f2a9c0b71d3e84g" }), "target_fingerprint"},
		{"no creation time", edit(func(r *PushRegistrationV1) { r.CreatedAt = Timestamp{} }), "created_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefusal(t, tt.reg.Validate(), tt.field)
		})
	}
}

// TestPushRegistrationV1HasNoTarget pins the read shape's key set: no
// endpoint, key or token is ever read back, and no preference lives on a
// registration (design §5.1).
func TestPushRegistrationV1HasNoTarget(t *testing.T) {
	seen := NewTimestamp(time.Date(2026, 11, 10, 12, 30, 0, 0, time.UTC))
	full := validRegistration()
	full.LastWakeAt, full.LastFetchAt = &seen, &seen
	if got, want := jsonKeys(t, full), "app,created_at,id,last_fetch_at,last_wake_at,platform,target_fingerprint,transport"; got != want {
		t.Errorf("PushRegistrationV1 keys = %s, want %s", got, want)
	}
	if got, want := jsonKeys(t, validRegistration()), "app,created_at,id,platform,target_fingerprint,transport"; got != want {
		t.Errorf("a new PushRegistrationV1's keys = %s, want %s", got, want)
	}
}

func TestListOwnPushRegistrationsResponseV1Validate(t *testing.T) {
	bad := validRegistration()
	bad.ID = ""
	tests := []struct {
		name  string
		resp  ListOwnPushRegistrationsResponseV1
		field string
	}{
		{"none", ListOwnPushRegistrationsResponseV1{Registrations: []PushRegistrationV1{}}, ""},
		{"the most a credential holds", ListOwnPushRegistrationsResponseV1{Registrations: []PushRegistrationV1{validRegistration(), validRegistration()}}, ""},
		{"null", ListOwnPushRegistrationsResponseV1{}, "not null"},
		{"more than a credential holds", ListOwnPushRegistrationsResponseV1{Registrations: []PushRegistrationV1{validRegistration(), validRegistration(), validRegistration()}}, "at most 2"},
		{"a malformed registration", ListOwnPushRegistrationsResponseV1{Registrations: []PushRegistrationV1{validRegistration(), bad}}, "registrations[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefusal(t, tt.resp.Validate(), tt.field)
		})
	}
	raw, err := json.Marshal(ListOwnPushRegistrationsResponseV1{Registrations: []PushRegistrationV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"registrations":[]}` {
		t.Errorf("an empty list encodes as %s", raw)
	}
}

func TestPushConfigV1Validate(t *testing.T) {
	tests := []struct {
		name  string
		conf  PushConfigV1
		field string
	}{
		{"push off", PushConfigV1{Transports: []PushTransport{}}, ""},
		{"webpush on", PushConfigV1{Transports: []PushTransport{PushTransportWebPush}, VAPIDPublicKey: testPushVAPID}, ""},
		{"relay on, before it reports providers", PushConfigV1{Transports: []PushTransport{PushTransportRelay}}, ""},
		{"both on", PushConfigV1{
			Transports:     []PushTransport{PushTransportWebPush, PushTransportRelay},
			VAPIDPublicKey: testPushVAPID,
			RelayProviders: []PushProvider{PushProviderAPNs, PushProviderFCM},
		}, ""},
		{"null transports", PushConfigV1{}, "transports"},
		{"an unknown transport", PushConfigV1{Transports: []PushTransport{"sms"}}, "transports[0]"},
		{"a transport twice", PushConfigV1{Transports: []PushTransport{PushTransportRelay, PushTransportRelay}}, "twice"},
		{"webpush twice", PushConfigV1{Transports: []PushTransport{PushTransportWebPush, PushTransportWebPush}, VAPIDPublicKey: testPushVAPID}, "twice"},
		{"webpush without its key", PushConfigV1{Transports: []PushTransport{PushTransportWebPush}}, "vapid_public_key is required"},
		{"a key with push off", PushConfigV1{Transports: []PushTransport{}, VAPIDPublicKey: testPushVAPID}, "vapid_public_key must be omitted"},
		{"a key with only relay on", PushConfigV1{Transports: []PushTransport{PushTransportRelay}, VAPIDPublicKey: testPushVAPID}, "vapid_public_key must be omitted"},
		{"a padded key", PushConfigV1{Transports: []PushTransport{PushTransportWebPush}, VAPIDPublicKey: testPushVAPID + "="}, "vapid_public_key must be unpadded base64url"},
		{"a key that is not a point", PushConfigV1{Transports: []PushTransport{PushTransportWebPush}, VAPIDPublicKey: testPushAuth}, "vapid_public_key must be an uncompressed P-256 point"},
		{"providers with relay off", PushConfigV1{Transports: []PushTransport{}, RelayProviders: []PushProvider{PushProviderAPNs}}, "relay_providers must be omitted"},
		{"an unknown provider", PushConfigV1{Transports: []PushTransport{PushTransportRelay}, RelayProviders: []PushProvider{"hms"}}, "relay_providers[0]"},
		{"a provider twice", PushConfigV1{Transports: []PushTransport{PushTransportRelay}, RelayProviders: []PushProvider{PushProviderFCM, PushProviderFCM}}, "twice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefusal(t, tt.conf.Validate(), tt.field)
		})
	}
	// Push off is an empty list on the wire, never null, and nothing else.
	raw, err := json.Marshal(PushConfigV1{Transports: []PushTransport{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"transports":[]}` {
		t.Errorf("push off encodes as %s", raw)
	}
}

// The wire values are stable: clients match on the codes, and the audit log
// and its readers on the actions.
func TestPushConstants(t *testing.T) {
	tests := []struct {
		got, want string
	}{
		{ErrorCodePushNotConfigured, "push_not_configured"},
		{ErrorCodePushTransportUnavailable, "push_transport_unavailable"},
		{ErrorCodePushEndpointRefused, "push_endpoint_refused"},
		{ErrorCodeTooManyPushRegistrations, "too_many_push_registrations"},
		{ErrorCodePushRegistrationNotFound, "push_registration_not_found"},
		{AuditActionPushRegistrationCreated, "push_registration_created"},
		{AuditActionPushRegistrationEnded, "push_registration_ended"},
		{PushWakeSchemaV1, "conch.push_wake.v1"},
		{string(PushRegistrationEndSelf), "self"},
		{string(PushRegistrationEndCredentialRevoked), "credential_revoked"},
		{string(PushRegistrationEndCredentialExpired), "credential_expired"},
		{string(PushRegistrationEndProviderRejected), "provider_rejected"},
		{string(PushRegistrationEndStale), "stale"},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("got %q, want %q", tt.got, tt.want)
		}
	}
	bounds := []struct {
		name      string
		got, want int
	}{
		{"MaxPushEndpointBytes", MaxPushEndpointBytes, 1000},
		{"MaxPushRegistrationsPerCredential", MaxPushRegistrationsPerCredential, 2},
		{"MaxPushRegistrationsPerPrincipal", MaxPushRegistrationsPerPrincipal, 16},
		{"PushP256DHBytes", PushP256DHBytes, 65},
		{"PushAuthBytes", PushAuthBytes, 16},
	}
	for _, b := range bounds {
		if b.got != b.want {
			t.Errorf("%s = %d, want %d", b.name, b.got, b.want)
		}
	}
}

// OwnCredentialV1.Push is additive (issue #1712): a sign-in without a
// registration encodes exactly as it did before the field existed.
func TestOwnCredentialV1PushIsAdditive(t *testing.T) {
	ts := NewTimestamp(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	raw, err := json.Marshal(OwnCredentialV1{ID: 4, Label: "laptop", CreatedAt: ts})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":4,"label":"laptop","created_at":"2026-10-08T12:00:00.000Z"}`; string(raw) != want {
		t.Errorf("a sign-in without push encodes as %s, want %s", raw, want)
	}
	raw, err = json.Marshal(OwnCredentialV1{ID: 4, Label: "laptop", CreatedAt: ts, Push: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":4,"label":"laptop","created_at":"2026-10-08T12:00:00.000Z","push":true}`; string(raw) != want {
		t.Errorf("a sign-in with push encodes as %s, want %s", raw, want)
	}
}
