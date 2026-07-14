package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// Payload is the optional machine-readable part of a message envelope. Schema
// is a versioned payload name (e.g. "leviathan.trade_signal.v1"); Data is the
// payload body, preserved verbatim so a payload whose schema this build does
// not recognize survives a decode/re-encode round trip unchanged.
type Payload struct {
	// Schema is the versioned payload name, "<name>.v<N>".
	Schema string `json:"schema"`
	// Data is the raw payload body, preserved opaquely for forward compatibility.
	Data json.RawMessage `json:"data"`
}

// payloadNamePattern matches a versioned payload name: one or more
// dot-separated lowercase segments followed by a ".v<N>" version suffix, e.g.
// "leviathan.trade_signal.v1".
var payloadNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*\.v[1-9][0-9]*$`)

// ValidPayloadName reports whether name is a well-formed versioned payload name.
func ValidPayloadName(name string) bool {
	return payloadNamePattern.MatchString(name)
}

// Validate reports whether the payload is structurally well-formed: a valid
// versioned name and syntactically valid JSON data. It does not require the
// schema to be registered.
func (p Payload) Validate() error {
	if !ValidPayloadName(p.Schema) {
		return fmt.Errorf("schema: payload schema %q is not a valid versioned name", p.Schema)
	}
	if len(p.Data) == 0 {
		return errors.New("schema: payload data is required")
	}
	if !json.Valid(p.Data) {
		return errors.New("schema: payload data is not valid JSON")
	}
	return nil
}

// Known reports whether this payload's schema is registered in this build.
func (p Payload) Known() bool {
	_, ok := LookupPayload(p.Schema)
	return ok
}

// Decode decodes the payload data into its registered Go type. If the schema is
// not registered it returns (nil, false, nil): an unrecognized payload is not
// an error, and the caller can still use Data verbatim. If the schema is
// registered it returns (typedPointer, true, err). Decoding rejects unknown
// fields so a registered payload carrying unexpected fields fails loudly rather
// than silently dropping them.
func (p Payload) Decode() (value any, known bool, err error) {
	s, ok := LookupPayload(p.Schema)
	if !ok {
		return nil, false, nil
	}
	v := s.New()
	dec := json.NewDecoder(bytes.NewReader(p.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return nil, true, fmt.Errorf("schema: decode %s payload: %w", p.Schema, err)
	}
	return v, true, nil
}

// PayloadSchema is a registered payload type: its versioned Name and a New
// constructor returning a fresh pointer to the Go type the payload decodes into.
type PayloadSchema struct {
	// Name is the versioned payload name this schema is registered under.
	Name string
	// New returns a fresh pointer value to decode this payload's data into.
	New func() any
}

// payloadRegistry maps versioned payload names to their registered schema. It
// is populated by RegisterPayload, called from the init of each payload's file.
var payloadRegistry = map[string]PayloadSchema{}

// RegisterPayload registers a payload schema by name. It panics on a missing
// name, an invalid versioned name, a nil constructor, or a duplicate
// registration — each a programmer error meant to surface at init time.
func RegisterPayload(s PayloadSchema) {
	if s.Name == "" || s.New == nil {
		panic("schema: RegisterPayload requires a name and constructor")
	}
	if !ValidPayloadName(s.Name) {
		panic(fmt.Sprintf("schema: invalid payload name %q", s.Name))
	}
	if _, dup := payloadRegistry[s.Name]; dup {
		panic(fmt.Sprintf("schema: payload %q already registered", s.Name))
	}
	payloadRegistry[s.Name] = s
}

// LookupPayload returns the registered schema for name and whether it exists.
func LookupPayload(name string) (PayloadSchema, bool) {
	s, ok := payloadRegistry[name]
	return s, ok
}

// RegisteredPayloads returns the sorted names of all registered payload schemas.
func RegisteredPayloads() []string {
	names := make([]string, 0, len(payloadRegistry))
	for name := range payloadRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
