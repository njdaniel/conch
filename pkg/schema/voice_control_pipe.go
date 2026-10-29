package schema

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
)

// The attach line of a managed voice helper whose events channel is an
// endpoint it connects to, rather than the inherited descriptor fd 3 (issue
// #893; docs/design/windows-desktop.md §3.4, docs/design/linux-desktop.md
// §5.2). An Electron owner cannot hand the helper a FIFO on fd 3 (Windows has
// no descriptor to inherit; libuv's fourth stdio entry on Linux is a socket),
// so it listens on an endpoint of its own (a `\\.\pipe\` name on Windows, a
// Unix socket under $XDG_RUNTIME_DIR on Linux), starts the helper as
// `conch-voice managed --events-pipe <endpoint>`, and puts a nonce in the
// helper's environment (CONCH_VOICE_ATTACH), beside CONCH_TOKEN when the
// credential is env. The first line the helper writes on that connection is
// one attached object carrying the nonce. The owner reads that line first,
// compares the nonce in constant time, and only then reads the connection as
// the control protocol: the hello (seq 1) and everything after it, unchanged
// (voice_control.go).
//
// What the line is: a spawn-to-pipe binding, defence in depth, not
// authentication of either side. It lets the owner tell the connection made
// by the process it spawned apart from one made by some other process that
// reached the endpoint without reading the environment. It authenticates
// nothing to the helper: same-user code can create a further instance of the
// owner's pipe name, or unlink the owner's socket path and bind its own, and
// so receive the attach line and every event; it can read the nonce, and
// CONCH_TOKEN, from the helper's environment just as well. That same-user
// hijack is out of scope, on both platforms, and is named here so that nobody
// later mistakes the nonce for a defence against it. Across users the
// boundary is the pipe's DACL (Windows) or the 0o700 runtime directory
// (Linux); the nonce stands behind that boundary, not in place of it.
// Authority arrives only on standard input, from the spawning process;
// nothing read from the events channel can cause a join, a publish or an
// unmute, so a stolen or failed attach costs the confidentiality of state
// lines and availability (the join fails closed), never the microphone.
//
// The nonce is the one secret-shaped value on any voice control channel. It
// is one-time: drawn for one spawn, written once, never reused. It is spent
// when read: the owner compares it and closes its listener, and the helper
// forgets it once the line is written. And it is redacted from every
// diagnostic by both sides: no error of this file repeats it, no status view
// or stderr tail shows it, and no field of any later event may carry it
// (testdata/voice-control-pipe/sensitive.json pins the names). Everything
// else the control protocol's contract says still holds: no login token,
// join token or room name is ever on the channel.
//
// The line is written exactly once, first, before the hello, and has no seq:
// it is transport preamble, not an event. A second attached object later in
// the stream reaches the owner's event reader, which refuses it as
// unknown_schema and stops the helper. The owner never writes on this
// connection. Without --events-pipe the helper behaves as it always has and
// writes no attach line. The shape is platform-neutral; the endpoints, the
// deadline and the owner's rules are the design notes'. It is read with
// ParseVoiceControlPipeAttached, which applies the framing rules of the
// control protocol (size, one object, no duplicate keys, exact field names,
// unknown fields ignored), so a Go owner and the Rust helper agree on every
// line; the fixtures are in testdata/voice-control-pipe/.

// VoiceControlPipeSchemaV1 is the versioned name the attach line carries in
// its schema field. It is not negotiated: the line precedes the hello, and
// the owner knows which helper it bundled. A change to the nonce's form, or
// a second kind, is a new schema name.
const VoiceControlPipeSchemaV1 = "conch.voice_control_pipe.v1"

// The names and numbers both sides are held to. As with the control
// protocol's constants, they are not wire shapes, but each side can only keep
// its part if the other keeps its own.
const (
	// VoiceControlPipeFlag is the helper's option naming the endpoint to
	// connect to: `conch-voice managed --events-pipe <endpoint>`. On Windows
	// a `\\.\pipe\` name, on Linux an absolute Unix-socket path; never a
	// secret. The owner passes the endpoint here and the nonce in the
	// environment, never the other way round.
	VoiceControlPipeFlag = "--events-pipe"
	// VoiceControlPipeNonceEnv is the environment variable the owner sets to
	// the nonce. The helper reads it only when VoiceControlPipeFlag is given;
	// the Go TUI supervisor never sets it, and strips it with every other
	// CONCH_ variable from the environment it hands on.
	VoiceControlPipeNonceEnv = "CONCH_VOICE_ATTACH"
	// VoiceControlPipeNonceBytes is the nonce's entropy: 32 random bytes.
	VoiceControlPipeNonceBytes = 32
	// VoiceControlPipeNonceChars is the nonce as written: 32 bytes in
	// unpadded base64url are 43 characters.
	VoiceControlPipeNonceChars = 43
)

// voiceControlPipeNonceLastChars are the characters of the base64url alphabet
// whose two low bits are zero: the only ones a 43-character encoding of 32
// bytes can end with. base64.RawURLEncoding.Strict() refuses the others; the
// Rust reader checks the last byte against this list, and the two agree.
const voiceControlPipeNonceLastChars = "AEIMQUYcgkosw048"

// VoiceControlPipeKind is what a line of this schema is. v1 has one kind.
type VoiceControlPipeKind string

// Voice control pipe kinds.
const (
	// VoiceControlPipeKindAttached is the one line of v1: this connection was
	// made by the process started with this nonce.
	VoiceControlPipeKindAttached VoiceControlPipeKind = "attached"
)

// Valid reports whether k is a recognized kind.
func (k VoiceControlPipeKind) Valid() bool {
	switch k {
	case VoiceControlPipeKindAttached:
		return true
	default:
		return false
	}
}

// VoiceControlPipeAttachedV1 is the attach line (helper to owner): the first
// line on an events endpoint, written once, before the hello. Its field list
// is exact (voice_control_pipe_test.go pins it): nothing but the nonce rides
// on it, so the endpoint, the profile and the process are never named here.
type VoiceControlPipeAttachedV1 struct {
	Schema string               `json:"schema"`
	Kind   VoiceControlPipeKind `json:"kind"`
	// Nonce is the value of CONCH_VOICE_ATTACH as the helper found it:
	// VoiceControlPipeNonceChars of canonical unpadded base64url, the
	// encoding of VoiceControlPipeNonceBytes random bytes.
	Nonce string `json:"nonce"`
}

func (VoiceControlPipeAttachedV1) voiceControlMessage() {}

// Validate reports whether the line is well-formed: the schema, the kind, and
// a nonce ValidateVoiceControlPipeNonce accepts. It says nothing about
// whether the nonce is the expected one; Matches does. The error never
// repeats the nonce.
func (a VoiceControlPipeAttachedV1) Validate() error {
	if a.Schema != VoiceControlPipeSchemaV1 {
		return fmt.Errorf("schema: voice control pipe attached schema must be %q, got %q", VoiceControlPipeSchemaV1, a.Schema)
	}
	if a.Kind != VoiceControlPipeKindAttached {
		return fmt.Errorf("schema: voice control pipe attached kind must be %q, got %q", VoiceControlPipeKindAttached, a.Kind)
	}
	return ValidateVoiceControlPipeNonce(a.Nonce)
}

// Matches reports, in constant time (crypto/subtle.ConstantTimeCompare),
// whether the line's nonce is nonce: the value the owner generated and put
// in the helper's environment. A false answer closes the connection and
// fails the join; it is never written anywhere, since the owner never writes
// on this channel. An empty line never matches: a zero value is not a nonce.
func (a VoiceControlPipeAttachedV1) Matches(nonce string) bool {
	if a.Nonce == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a.Nonce), []byte(nonce)) == 1
}

// ValidateVoiceControlPipeNonce reports whether nonce is well-formed:
// VoiceControlPipeNonceChars bytes of the base64url alphabet (A-Z, a-z, 0-9,
// - and _), unpadded and canonical, so that it decodes to exactly
// VoiceControlPipeNonceBytes bytes with the two trailing bits zero. That is
// what base64.RawURLEncoding.Strict() accepts, and what Node's
// Buffer.toString('base64url') writes. The text of the error never repeats
// the value.
func ValidateVoiceControlPipeNonce(nonce string) error {
	if len(nonce) != VoiceControlPipeNonceChars {
		return fmt.Errorf("schema: voice control pipe nonce must be %d characters of unpadded base64url, got %d", VoiceControlPipeNonceChars, len(nonce))
	}
	// The alphabet by hand first: the decoder below would skip a line feed
	// or a carriage return, and the Rust reader checks these same bytes.
	for i := 0; i < len(nonce); i++ {
		c := nonce[i]
		alphabet := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !alphabet {
			return fmt.Errorf("schema: voice control pipe nonce character %d is not in the base64url alphabet", i)
		}
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(nonce)
	if err != nil || len(raw) != VoiceControlPipeNonceBytes {
		return fmt.Errorf("schema: voice control pipe nonce must be the canonical encoding of %d bytes (its last character one of %s)", VoiceControlPipeNonceBytes, voiceControlPipeNonceLastChars)
	}
	return nil
}

// NewVoiceControlPipeNonce draws VoiceControlPipeNonceBytes from random and
// encodes them with base64.RawURLEncoding: VoiceControlPipeNonceChars of
// canonical unpadded base64url, which ValidateVoiceControlPipeNonce accepts.
// A nil random is crypto/rand.Reader. A short or failing reader is an error
// and no nonce. For a Go owner; Node's owner draws
// crypto.randomBytes(32).toString('base64url'), which is the same encoding.
func NewVoiceControlPipeNonce(random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	raw := make([]byte, VoiceControlPipeNonceBytes)
	if _, err := io.ReadFull(random, raw); err != nil {
		return "", fmt.Errorf("schema: draw voice control pipe nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ParseVoiceControlPipeAttached reads the first line of an events endpoint,
// without its line feed, by the framing rules of the control protocol
// (decodeVoiceControlObject). It is how the owner's side is specified. It
// refuses, in this order, with these codes in a *VoiceControlError whose ID
// is 0 (the line has none):
//
//   - too_long, malformed: as ParseVoiceControlEvent;
//   - unknown_schema: schema missing, not a string, or not
//     VoiceControlPipeSchemaV1. A hello arriving first is this, so an owner
//     facing a helper that wrote no attach line sees a deterministic
//     refusal;
//   - invalid: kind missing, null, not a string or empty; nonce missing,
//     null, not a string, or failing ValidateVoiceControlPipeNonce;
//   - unknown_kind: kind a non-empty string other than attached.
//
// Field names are matched exactly, case included. A field the kind does not
// declare is ignored and never repeated. The error never contains the line
// or the nonce. Whether the nonce is the owner's is Matches's to say.
func ParseVoiceControlPipeAttached(line []byte) (VoiceControlPipeAttachedV1, error) {
	fields, err := decodeVoiceControlObject(line)
	if err != nil {
		return VoiceControlPipeAttachedV1{}, err
	}
	if err := checkVoiceControlSchemaIs(fields, VoiceControlPipeSchemaV1); err != nil {
		return VoiceControlPipeAttachedV1{}, err
	}
	kindText, ok := fieldString(fields, "kind")
	if !ok || kindText == "" {
		return VoiceControlPipeAttachedV1{}, voiceControlErr(VoiceControlErrorInvalid, 0, "kind must be a string")
	}
	nonce, ok := fieldString(fields, "nonce")
	if !ok {
		return VoiceControlPipeAttachedV1{}, voiceControlErr(VoiceControlErrorInvalid, 0, "nonce must be a string")
	}
	if err := ValidateVoiceControlPipeNonce(nonce); err != nil {
		return VoiceControlPipeAttachedV1{}, &VoiceControlError{Code: VoiceControlErrorInvalid, detail: err.Error()}
	}
	kind := VoiceControlPipeKind(kindText)
	if !kind.Valid() {
		return VoiceControlPipeAttachedV1{}, voiceControlErr(VoiceControlErrorUnknownKind, 0, "not a kind of %s", VoiceControlPipeSchemaV1)
	}
	attached := VoiceControlPipeAttachedV1{Schema: VoiceControlPipeSchemaV1, Kind: kind, Nonce: nonce}
	if err := attached.Validate(); err != nil {
		return VoiceControlPipeAttachedV1{}, &VoiceControlError{Code: VoiceControlErrorInvalid, detail: err.Error()}
	}
	return attached, nil
}
