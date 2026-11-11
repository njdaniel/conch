package schema

import (
	"crypto/ecdh"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Phone push (issue #1712, Mobile M11-S; docs/design/mobile-notifications.md
// §4, §5 and §9.1, shapes S1, S2, S4 and S7). A push is a wake and nothing
// more: it tells an app to fetch, with its own credential, what is new
// (notification.go has the feed and the preferences). These are the wake
// itself, the registration that says where to send one, and the server's
// answer to "which transports are on".
//
// A registration belongs to one credential, which is one sign-in, and ends
// with it. It is an address and holds no preference: the policy, the mutes
// and the rendering switches are NotificationPreferencesV1, which exists with
// or without a registration.
//
// These are new types; nothing published changes (D-8 on #1116: additive, no
// version bump). The wake is a pushed document and carries its schema name.
// The rest are REST bodies, versioned by the V1 type suffix, and carry no
// schema-name field. The relay's own shapes (S5) are not here: they wait on
// #1716.

// PushWakeSchemaV1 is the schema name of the wake.
const PushWakeSchemaV1 = "conch.push_wake.v1"

const (
	// PushRegistrationIDBytes is how many crypto/rand bytes the server puts
	// in a registration id (#1714), and PushRegistrationIDLength how long
	// that makes it: 32 bytes in unpadded base64url.
	PushRegistrationIDBytes  = 32
	PushRegistrationIDLength = 43
	// MinPushRegistrationIDBytes and MaxPushRegistrationIDBytes bound what
	// a registration id may decode to. The floor is the design's 128 bits
	// (§4.1); the ceiling leaves room for a longer id later and no room for
	// anything else in a wake.
	MinPushRegistrationIDBytes = 16
	MaxPushRegistrationIDBytes = 64
	// MaxPushRegistrationIDLength is the longest a registration id may be:
	// MaxPushRegistrationIDBytes in unpadded base64url.
	MaxPushRegistrationIDLength = 86

	// MaxPushEndpointBytes bounds a Web Push endpoint. It is UnifiedPush's
	// bound (§5.7).
	MaxPushEndpointBytes = 1000
	// MaxPushAppBytes bounds app: a bundle id, a package name or an origin.
	MaxPushAppBytes = 255
	// MaxPushTokenBytes bounds a relay registration's device token. An APNs
	// token is 64 hex characters and an FCM token runs to a few hundred;
	// neither provider publishes a ceiling, so this one is generous.
	MaxPushTokenBytes = 4096
	// PushP256DHBytes is the length of a client's Web Push public key: an
	// uncompressed P-256 point, which starts with 0x04 (RFC 8291 §3.1).
	// A server's VAPID key is the same form (RFC 8292 §3.2).
	PushP256DHBytes = 65
	// PushAuthBytes is the length of a Web Push authentication secret (RFC
	// 8291 §3.2).
	PushAuthBytes = 16
	// PushTargetFingerprintLength is the length of target_fingerprint:
	// lowercase hex.
	PushTargetFingerprintLength = 16

	// MaxPushRegistrationsPerCredential is how many active registrations
	// one credential may hold: one install, and one re-registration in
	// flight (§5.7).
	MaxPushRegistrationsPerCredential = 2
	// MaxPushRegistrationsPerPrincipal is how many one principal may hold
	// across all of their sign-ins (§5.7).
	MaxPushRegistrationsPerPrincipal = 16
)

// Error codes of the push routes (#1714), in Error.Code. The HTTP statuses
// are the ones proposed for those routes.
const (
	// ErrorCodePushNotConfigured: no push transport is on at this conchd
	// (HTTP 409). GET /v1/push/config says so first, with an empty list.
	ErrorCodePushNotConfigured = "push_not_configured"
	// ErrorCodePushTransportUnavailable: push is on, and the transport the
	// request names is not (HTTP 409).
	ErrorCodePushTransportUnavailable = "push_transport_unavailable"
	// ErrorCodePushEndpointRefused: the endpoint is well-formed and conchd
	// will not post to it: an address it resolves to is forbidden (§5.7).
	// A malformed endpoint is invalid_request instead (HTTP 400).
	ErrorCodePushEndpointRefused = "push_endpoint_refused"
	// ErrorCodeTooManyPushRegistrations: the credential already holds
	// MaxPushRegistrationsPerCredential, or its principal
	// MaxPushRegistrationsPerPrincipal; nothing stored (HTTP 409).
	ErrorCodeTooManyPushRegistrations = "too_many_push_registrations"
	// ErrorCodePushRegistrationNotFound: the calling credential has no
	// active registration of that id (HTTP 404). Another credential's
	// registration answers the same, so the id of one is no oracle.
	ErrorCodePushRegistrationNotFound = "push_registration_not_found"
)

// The audit actions of a registration (§5.4), in AuditEventV1.Action. The
// subject is the principal. The detail is the reason (a
// PushRegistrationEndReason, on an end), the transport and the registration
// id, and never a token, an endpoint or an app.
const (
	AuditActionPushRegistrationCreated = "push_registration_created"
	AuditActionPushRegistrationEnded   = "push_registration_ended"
)

// PushTransport is how a wake reaches a device. The vocabulary is closed.
type PushTransport string

const (
	// PushTransportWebPush: conchd posts the encrypted wake to the endpoint
	// the device gave (RFC 8030, 8291 and 8292). It serves a browser and a
	// UnifiedPush distributor alike.
	PushTransportWebPush PushTransport = "webpush"
	// PushTransportRelay: conchd hands the wake to a relay, which holds the
	// provider keys of a published app and forwards it to APNs or FCM.
	PushTransportRelay PushTransport = "relay"
)

// Valid reports whether t is a recognized transport.
func (t PushTransport) Valid() bool {
	switch t {
	case PushTransportWebPush, PushTransportRelay:
		return true
	default:
		return false
	}
}

// PushPlatform is what the registered app runs on. It is for the person's own
// list and for the relay; it never picks the transport. The vocabulary is
// closed.
type PushPlatform string

const (
	PushPlatformIOS     PushPlatform = "ios"
	PushPlatformAndroid PushPlatform = "android"
	PushPlatformWeb     PushPlatform = "web"
)

// Valid reports whether p is a recognized platform.
func (p PushPlatform) Valid() bool {
	switch p {
	case PushPlatformIOS, PushPlatformAndroid, PushPlatformWeb:
		return true
	default:
		return false
	}
}

// PushProvider is the push service a relay forwards to. The vocabulary is
// closed.
type PushProvider string

const (
	PushProviderAPNs PushProvider = "apns"
	PushProviderFCM  PushProvider = "fcm"
)

// Valid reports whether p is a recognized provider.
func (p PushProvider) Valid() bool {
	switch p {
	case PushProviderAPNs, PushProviderFCM:
		return true
	default:
		return false
	}
}

// PushRegistrationEndReason is why a registration ended: the reason in the
// detail of a push_registration_ended audit row (§5.4). The vocabulary is
// closed.
type PushRegistrationEndReason string

const (
	// PushRegistrationEndSelf: its own credential deleted it.
	PushRegistrationEndSelf PushRegistrationEndReason = "self"
	// PushRegistrationEndCredentialRevoked: its credential was revoked, for
	// any of the reasons one is: a sign-out, a rotation, an operator's
	// revoke, a disabled principal.
	PushRegistrationEndCredentialRevoked PushRegistrationEndReason = "credential_revoked" // #nosec G101 -- a reason name, not a credential
	// PushRegistrationEndCredentialExpired: its credential expired and the
	// sweeper reaped it. Nobody revoked anything, so this is its own
	// reason; the design's §5.4 list does not have it (decision 3 on
	// #1116).
	PushRegistrationEndCredentialExpired PushRegistrationEndReason = "credential_expired" // #nosec G101 -- a reason name, not a credential
	// PushRegistrationEndProviderRejected: the push service said the target
	// is gone (§5.6).
	PushRegistrationEndProviderRejected PushRegistrationEndReason = "provider_rejected"
	// PushRegistrationEndStale: no fetch and no update for the stale
	// window (§5.8), or the server's VAPID key was replaced.
	PushRegistrationEndStale PushRegistrationEndReason = "stale"
)

// Valid reports whether r is a recognized reason.
func (r PushRegistrationEndReason) Valid() bool {
	switch r {
	case PushRegistrationEndSelf, PushRegistrationEndCredentialRevoked,
		PushRegistrationEndCredentialExpired, PushRegistrationEndProviderRejected,
		PushRegistrationEndStale:
		return true
	default:
		return false
	}
}

// PushWakeV1 is the whole body of a push (conch.push_wake.v1, §4.1): the only
// thing a push service or a relay ever carries for conchd. Registration is
// the id the server issued when the device registered. It is random, it is
// not the credential or the principal, and it grants nothing: the app
// recognizes it as one of its own and then fetches GET
// /v1/notifications/pending with its credential.
//
// Nothing may be added to this type, in this version or a later one (§4.2).
// Every field here is read by a party that is not the server or the person:
// the push service, and the relay when there is one. A server name or URL
// would tell them which server a device belongs to. A channel, a message id,
// an author, text or a count is content, or an oracle on how busy a private
// server is. A kind would state what the timing of wakes only hints at. An
// id of the person would identify them. Anything an operator or a client
// could set would be a way to leak any of these. So every wake looks the
// same, and what is new is fetched after authentication instead.
type PushWakeV1 struct {
	Schema       string `json:"schema"`
	Registration string `json:"registration"`
}

// NewPushWakeV1 returns the wake for a registration.
func NewPushWakeV1(registration string) PushWakeV1 {
	return PushWakeV1{Schema: PushWakeSchemaV1, Registration: registration}
}

// Validate reports whether the wake is well-formed: the schema is
// conch.push_wake.v1 and registration passes ValidatePushRegistrationID.
func (w PushWakeV1) Validate() error {
	if w.Schema != PushWakeSchemaV1 {
		return fmt.Errorf("schema: push wake schema must be %q, got %q", PushWakeSchemaV1, w.Schema)
	}
	if err := ValidatePushRegistrationID(w.Registration); err != nil {
		return fmt.Errorf("schema: push wake registration: %w", err)
	}
	return nil
}

// ValidatePushRegistrationID reports whether id has the form of a
// registration id: unpadded base64url of MinPushRegistrationIDBytes to
// MaxPushRegistrationIDBytes bytes. The server issues
// PushRegistrationIDBytes; the form says nothing about whether one exists.
func ValidatePushRegistrationID(id string) error {
	if id == "" {
		return errors.New("id must not be empty")
	}
	if len(id) > MaxPushRegistrationIDLength {
		return fmt.Errorf("id must be at most %d characters", MaxPushRegistrationIDLength)
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(id)
	if err != nil {
		return errors.New("id must be unpadded base64url")
	}
	if len(raw) < MinPushRegistrationIDBytes {
		return fmt.Errorf("id must hold at least %d bytes", MinPushRegistrationIDBytes)
	}
	return nil
}

// PushRegistrationV1 is one registration as its own credential reads it: the
// answer to POST /v1/push/registrations and PUT /v1/push/registrations/{id},
// and an element of GET /v1/push/registrations/mine (§5.1, §5.3).
//
// It has no target. The endpoint, the keys and the device token are
// write-only on the wire, as a credential's token is: TargetFingerprint is
// PushTargetFingerprintLength lowercase hex characters the server derives
// from the target, so an app can tell whether the server holds the target it
// last sent, and nobody can read one back. It has no preference either:
// those are NotificationPreferencesV1.
//
// App is what the request gave. LastWakeAt is when the server last sent this
// registration a wake, and LastFetchAt when its credential last read the
// pending feed or replaced the target; each is omitted until it has
// happened. An ended registration is never listed, so there is no state.
type PushRegistrationV1 struct {
	ID                string        `json:"id"`
	Transport         PushTransport `json:"transport"`
	Platform          PushPlatform  `json:"platform"`
	App               string        `json:"app"`
	TargetFingerprint string        `json:"target_fingerprint"`
	CreatedAt         Timestamp     `json:"created_at"`
	LastWakeAt        *Timestamp    `json:"last_wake_at,omitempty"`
	LastFetchAt       *Timestamp    `json:"last_fetch_at,omitempty"`
}

// Validate reports whether the registration is well-formed: an id that
// passes ValidatePushRegistrationID, a known transport and platform, an app
// within its bounds, a fingerprint of PushTargetFingerprintLength lowercase
// hex characters and a creation time.
func (r PushRegistrationV1) Validate() error {
	if err := ValidatePushRegistrationID(r.ID); err != nil {
		return fmt.Errorf("schema: push registration id: %w", err)
	}
	if !r.Transport.Valid() {
		return errors.New("schema: push registration transport is not one of webpush, relay")
	}
	if !r.Platform.Valid() {
		return errors.New("schema: push registration platform is not one of ios, android, web")
	}
	if err := validatePushApp(r.App); err != nil {
		return fmt.Errorf("schema: push registration app %w", err)
	}
	if len(r.TargetFingerprint) != PushTargetFingerprintLength || strings.Trim(r.TargetFingerprint, "0123456789abcdef") != "" {
		return fmt.Errorf("schema: push registration target_fingerprint must be %d lowercase hex characters", PushTargetFingerprintLength)
	}
	if r.CreatedAt.IsZero() {
		return errors.New("schema: push registration created_at must be set")
	}
	return nil
}

// CreatePushRegistrationRequestV1 is the body of POST /v1/push/registrations
// (§5.3): where to send this credential's wakes. Transport, Platform and App
// are always required. The target depends on the transport, and a request
// carries exactly its transport's fields and none of the other's:
//
//   - webpush: Endpoint, P256DH and Auth, as a push subscription gives them
//     (RFC 8030, RFC 8291).
//   - relay: Provider and Token, the device token the provider's SDK gave.
//
// Endpoint is an absolute https URL with a host, no userinfo and no
// fragment, of at most MaxPushEndpointBytes bytes. Validate checks the form
// only: which addresses the host may resolve to is the server's check, at
// registration and again at every dial (§5.7), and its refusal is
// push_endpoint_refused. P256DH is base64url of a PushP256DHBytes-byte
// uncompressed P-256 point, and Auth base64url of PushAuthBytes bytes;
// padding is accepted, and Normalized drops it.
//
// App is the app's bundle id, package name or origin: 1 to MaxPushAppBytes
// bytes of ASCII letters, digits and ".", "-", "_", ":" and "/". Token is 1
// to MaxPushTokenBytes bytes of printable ASCII with no space.
type CreatePushRegistrationRequestV1 struct {
	Transport PushTransport `json:"transport"`
	Platform  PushPlatform  `json:"platform"`
	App       string        `json:"app"`
	Endpoint  string        `json:"endpoint,omitempty"`
	P256DH    string        `json:"p256dh,omitempty"`
	Auth      string        `json:"auth,omitempty"`
	Provider  PushProvider  `json:"provider,omitempty"`
	Token     string        `json:"token,omitempty"`
}

// Validate reports whether the request is well-formed, as the type's comment
// describes it.
func (r CreatePushRegistrationRequestV1) Validate() error {
	return validatePushRegistrationRequest(pushTarget(r))
}

// Normalized returns the request as the server stores it: P256DH and Auth
// without padding. Call it on a request that passed Validate.
func (r CreatePushRegistrationRequestV1) Normalized() CreatePushRegistrationRequestV1 {
	r.P256DH = strings.TrimRight(r.P256DH, "=")
	r.Auth = strings.TrimRight(r.Auth, "=")
	return r
}

// UpdatePushRegistrationRequestV1 is the body of PUT
// /v1/push/registrations/{id} (§5.3): the same fields as the create request,
// under the same rules. It replaces the registration's target, which is how
// an app reports a device token or an endpoint that changed; the id stays.
type UpdatePushRegistrationRequestV1 struct {
	Transport PushTransport `json:"transport"`
	Platform  PushPlatform  `json:"platform"`
	App       string        `json:"app"`
	Endpoint  string        `json:"endpoint,omitempty"`
	P256DH    string        `json:"p256dh,omitempty"`
	Auth      string        `json:"auth,omitempty"`
	Provider  PushProvider  `json:"provider,omitempty"`
	Token     string        `json:"token,omitempty"`
}

// Validate reports whether the request is well-formed, by the create
// request's rules.
func (r UpdatePushRegistrationRequestV1) Validate() error {
	return validatePushRegistrationRequest(pushTarget(r))
}

// Normalized returns the request as the server stores it: P256DH and Auth
// without padding. Call it on a request that passed Validate.
func (r UpdatePushRegistrationRequestV1) Normalized() UpdatePushRegistrationRequestV1 {
	r.P256DH = strings.TrimRight(r.P256DH, "=")
	r.Auth = strings.TrimRight(r.Auth, "=")
	return r
}

// pushTarget is the fields the two registration requests share, so that one
// function validates both.
type pushTarget struct {
	Transport PushTransport
	Platform  PushPlatform
	App       string
	Endpoint  string
	P256DH    string
	Auth      string
	Provider  PushProvider
	Token     string
}

func validatePushRegistrationRequest(r pushTarget) error {
	if !r.Transport.Valid() {
		return errors.New("schema: push registration transport is not one of webpush, relay")
	}
	if !r.Platform.Valid() {
		return errors.New("schema: push registration platform is not one of ios, android, web")
	}
	if err := validatePushApp(r.App); err != nil {
		return fmt.Errorf("schema: push registration app %w", err)
	}
	switch r.Transport {
	case PushTransportWebPush:
		if r.Provider != "" {
			return errors.New("schema: push registration provider is not a webpush field")
		}
		if r.Token != "" {
			return errors.New("schema: push registration token is not a webpush field")
		}
		if err := validatePushEndpoint(r.Endpoint); err != nil {
			return fmt.Errorf("schema: push registration endpoint %w", err)
		}
		if err := validatePushPoint(r.P256DH, true); err != nil {
			return fmt.Errorf("schema: push registration p256dh %w", err)
		}
		auth, err := decodePushBase64(r.Auth, true)
		if err != nil {
			return fmt.Errorf("schema: push registration auth %w", err)
		}
		if len(auth) != PushAuthBytes {
			return fmt.Errorf("schema: push registration auth must hold %d bytes", PushAuthBytes)
		}
	case PushTransportRelay:
		if r.Endpoint != "" {
			return errors.New("schema: push registration endpoint is not a relay field")
		}
		if r.P256DH != "" {
			return errors.New("schema: push registration p256dh is not a relay field")
		}
		if r.Auth != "" {
			return errors.New("schema: push registration auth is not a relay field")
		}
		if !r.Provider.Valid() {
			return errors.New("schema: push registration provider is not one of apns, fcm")
		}
		if err := validatePushToken(r.Token); err != nil {
			return fmt.Errorf("schema: push registration token %w", err)
		}
	}
	return nil
}

// validatePushApp checks an app name. Its errors start with a verb, for the
// caller to put the field's name before.
func validatePushApp(app string) error {
	if app == "" {
		return errors.New("must not be empty")
	}
	if len(app) > MaxPushAppBytes {
		return fmt.Errorf("must be at most %d bytes", MaxPushAppBytes)
	}
	for i := 0; i < len(app); i++ {
		c := app[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_', c == ':', c == '/':
		default:
			return errors.New(`may hold only ASCII letters, digits and ".", "-", "_", ":" and "/"`)
		}
	}
	return nil
}

// validatePushToken checks a relay registration's device token.
func validatePushToken(token string) error {
	if token == "" {
		return errors.New("must not be empty")
	}
	if len(token) > MaxPushTokenBytes {
		return fmt.Errorf("must be at most %d bytes", MaxPushTokenBytes)
	}
	for i := 0; i < len(token); i++ {
		if c := token[i]; c <= ' ' || c > '~' {
			return errors.New("may hold only printable ASCII, with no space")
		}
	}
	return nil
}

// validatePushEndpoint checks the form of a Web Push endpoint: an absolute
// https URL with a host, no userinfo and no fragment, within
// MaxPushEndpointBytes. It resolves nothing.
func validatePushEndpoint(endpoint string) error {
	if endpoint == "" {
		return errors.New("must not be empty")
	}
	if len(endpoint) > MaxPushEndpointBytes {
		return fmt.Errorf("must be at most %d bytes", MaxPushEndpointBytes)
	}
	for i := 0; i < len(endpoint); i++ {
		if c := endpoint[i]; c <= ' ' || c > '~' {
			return errors.New("may hold only printable ASCII, with no space")
		}
	}
	// url.Parse drops an empty fragment, so "https://host/#" is looked for
	// here.
	if strings.Contains(endpoint, "#") {
		return errors.New("must not have a fragment")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		// The parse error would repeat the endpoint, which is a secret.
		return errors.New("must be a URL")
	}
	if u.Scheme != "https" {
		return errors.New("must be an https URL")
	}
	if u.Opaque != "" || u.Hostname() == "" {
		return errors.New("must be an absolute URL with a host")
	}
	if u.User != nil {
		return errors.New("must not have userinfo")
	}
	return nil
}

// decodePushBase64 decodes base64url. With padded, trailing "=" padding is
// accepted, as a browser's subscription may carry it; without, it is refused.
func decodePushBase64(s string, padded bool) ([]byte, error) {
	if s == "" {
		return nil, errors.New("must not be empty")
	}
	if padded {
		s = strings.TrimRight(s, "=")
	}
	// The longest value decoded here is a 65-byte point: 88 characters
	// padded. Anything much longer is refused before it is decoded.
	if len(s) > 2*PushP256DHBytes {
		return nil, errors.New("is too long")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil {
		if padded {
			return nil, errors.New("must be base64url")
		}
		return nil, errors.New("must be unpadded base64url")
	}
	return raw, nil
}

// validatePushPoint checks a base64url uncompressed P-256 point: a client's
// p256dh, or a server's VAPID public key. crypto/ecdh refuses a point that is
// not on the curve, which no key agreement could use.
func validatePushPoint(s string, padded bool) error {
	raw, err := decodePushBase64(s, padded)
	if err != nil {
		return err
	}
	if len(raw) != PushP256DHBytes || raw[0] != 0x04 {
		return fmt.Errorf("must be an uncompressed P-256 point: %d bytes, the first 0x04", PushP256DHBytes)
	}
	if _, err := ecdh.P256().NewPublicKey(raw); err != nil {
		return errors.New("must be a point on the P-256 curve")
	}
	return nil
}

// ListOwnPushRegistrationsResponseV1 is the response to GET
// /v1/push/registrations/mine: the active registrations of the calling
// credential, and of no other (decision 5 on #1116). A person's other
// devices show as OwnCredentialV1.Push in GET /v1/credentials/mine.
// Producers must send an empty slice, not nil. There are at most
// MaxPushRegistrationsPerCredential.
type ListOwnPushRegistrationsResponseV1 struct {
	Registrations []PushRegistrationV1 `json:"registrations"`
}

// Validate reports whether the list is well-formed: a list, not null, of at
// most MaxPushRegistrationsPerCredential valid registrations.
func (r ListOwnPushRegistrationsResponseV1) Validate() error {
	if r.Registrations == nil {
		return errors.New("schema: push registrations must be a list, not null")
	}
	if len(r.Registrations) > MaxPushRegistrationsPerCredential {
		return fmt.Errorf("schema: push registrations must be at most %d", MaxPushRegistrationsPerCredential)
	}
	for i, reg := range r.Registrations {
		if err := reg.Validate(); err != nil {
			return fmt.Errorf("schema: push registrations[%d]: %w", i, err)
		}
	}
	return nil
}

// PushConfigV1 is the response to GET /v1/push/config (§3.6, §9.1 S4): which
// transports this conchd has on, so an app never offers a switch the server
// cannot honour. Any signed-in person may read it; nothing in it is secret.
//
// Transports is never null, and is empty when push is off. VAPIDPublicKey is
// the server's application server key (RFC 8292 §3.2), which an app passes
// when it subscribes: unpadded base64url of the PushP256DHBytes-byte
// uncompressed P-256 point. It is present exactly when webpush is on.
// RelayProviders is the providers the relay says it forwards to. It is
// omitted when empty, and is empty until the relay exists (#1716).
type PushConfigV1 struct {
	Transports     []PushTransport `json:"transports"`
	VAPIDPublicKey string          `json:"vapid_public_key,omitempty"`
	RelayProviders []PushProvider  `json:"relay_providers,omitempty"`
}

// Validate reports whether the answer is well-formed: transports is a list,
// not null, of known transports with none twice; vapid_public_key is present
// exactly when webpush is listed, and is an unpadded base64url P-256 point;
// relay_providers holds known providers, none twice, and only when relay is
// listed.
func (c PushConfigV1) Validate() error {
	if c.Transports == nil {
		return errors.New("schema: push config transports must be a list, not null")
	}
	webpush, relay := false, false
	for i, t := range c.Transports {
		if !t.Valid() {
			return fmt.Errorf("schema: push config transports[%d] %q is not one of webpush, relay", i, t)
		}
		switch t {
		case PushTransportWebPush:
			if webpush {
				return fmt.Errorf("schema: push config transports lists %q twice", t)
			}
			webpush = true
		case PushTransportRelay:
			if relay {
				return fmt.Errorf("schema: push config transports lists %q twice", t)
			}
			relay = true
		}
	}
	switch {
	case webpush && c.VAPIDPublicKey == "":
		return errors.New("schema: push config vapid_public_key is required when webpush is on")
	case !webpush && c.VAPIDPublicKey != "":
		return errors.New("schema: push config vapid_public_key must be omitted when webpush is off")
	case webpush:
		if err := validatePushPoint(c.VAPIDPublicKey, false); err != nil {
			return fmt.Errorf("schema: push config vapid_public_key %w", err)
		}
	}
	if !relay && len(c.RelayProviders) > 0 {
		return errors.New("schema: push config relay_providers must be omitted when relay is off")
	}
	seen := map[PushProvider]bool{}
	for i, p := range c.RelayProviders {
		if !p.Valid() {
			return fmt.Errorf("schema: push config relay_providers[%d] %q is not one of apns, fcm", i, p)
		}
		if seen[p] {
			return fmt.Errorf("schema: push config relay_providers lists %q twice", p)
		}
		seen[p] = true
	}
	return nil
}
