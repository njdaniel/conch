package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The device list (issue #649, parent #662): what `conch-voice devices --json`
// writes, so that a native client (the TUI) can offer the microphones and
// speakers PipeWire has, by the names `[audio] input` and `output` in
// voice.toml take, instead of asking the person to edit the file by hand. The
// Rust helper writes it (issue #650) and the TUI reads it (issue #652): a wire
// shape between two languages, so it lives here (CLAUDE.md rule 2).
//
// It is one JSON object, written on one line, read whole: a one-shot command,
// not a control channel. It carries device-supplied text (names and
// descriptions), which is why it is not on the text-free control channel
// (VoiceControlStateV1) and why every string in it is bounded on the reader's
// side as well as the writer's. It carries no path, no token, no server
// address and no free text of conch-voice's own; voice_devices_test.go
// asserts that no field could.
//
// A device is listed only under a name that can be written into voice.toml
// between quotes as it stands: not empty, one line, without `"` or `\`. Every
// other name is counted in unshown_*, never shown. The limitation that only
// nodes whose media.class is exactly Audio/Source are listed as microphones
// (not Audio/Source/Virtual) is the lister's, not this shape's.
//
// This is a new type; nothing published changes. Like the control protocol it
// is read with a reader of its own, ParseVoiceDevices, which applies the
// framing rules (size, one object, no duplicate keys, exact case-sensitive
// field names, unknown fields ignored). The golden fixtures and the reader's
// cases are in testdata/voice-devices/.

// VoiceDevicesSchemaV1 is the versioned name the document carries in its
// schema field.
const VoiceDevicesSchemaV1 = "conch.voice_devices.v1"

// The numbers both sides are held to. They are not wire shapes, but a reader
// that accepts less than a writer may write cannot read the document, so they
// live with it.
const (
	// VoiceDevicesMaxBytes is the most ParseVoiceDevices reads. The largest
	// valid document (VoiceDevicesMaxListed devices on each side, each name
	// VoiceDevicesMaxNameBytes long and each description
	// VoiceDevicesMaxDescriptionChars long, every character written as a
	// six-byte escape) is under it; voice_devices_test.go checks that, so a
	// writer cannot produce what a reader must refuse.
	VoiceDevicesMaxBytes = 256 << 10
	// VoiceDevicesMaxListed is the most devices either list holds. Any beyond
	// it are counted in unshown_*.
	VoiceDevicesMaxListed = 64
	// VoiceDevicesMaxNameBytes is the longest device name, in bytes: a
	// PipeWire node.name, which is also what voice.toml holds.
	VoiceDevicesMaxNameBytes = 255
	// VoiceDevicesMaxDescriptionChars is the longest description, in
	// characters: the writer cuts a longer one, as the text form does.
	VoiceDevicesMaxDescriptionChars = 80
)

// VoiceDevicesPipeWire is whether PipeWire could be reached to list devices.
type VoiceDevicesPipeWire string

// Voice devices PipeWire states.
const (
	// VoiceDevicesPipeWireOK: PipeWire was reached and the lists are its.
	VoiceDevicesPipeWireOK VoiceDevicesPipeWire = "ok"
	// VoiceDevicesPipeWireUnavailable: PipeWire could not be reached. Both
	// lists are empty and both counts are zero.
	VoiceDevicesPipeWireUnavailable VoiceDevicesPipeWire = "unavailable"
)

// Valid reports whether p is a recognized PipeWire state.
func (p VoiceDevicesPipeWire) Valid() bool {
	switch p {
	case VoiceDevicesPipeWireOK, VoiceDevicesPipeWireUnavailable:
		return true
	default:
		return false
	}
}

// VoiceDevicesConfig is what became of voice.toml, read by the same rules
// `conch-voice join` reads it by (docs/design/conch-voice.md §8).
type VoiceDevicesConfig string

// Voice devices configuration states.
const (
	// VoiceDevicesConfigOK: the file was read. configured_input and
	// configured_output say what its [audio] section names.
	VoiceDevicesConfigOK VoiceDevicesConfig = "ok"
	// VoiceDevicesConfigMissing: there is a configuration directory and no
	// voice.toml in it, which is the default configuration.
	VoiceDevicesConfigMissing VoiceDevicesConfig = "missing"
	// VoiceDevicesConfigRefused: voice.toml is there and was refused: not a
	// regular file, writable by group or others, or not valid.
	VoiceDevicesConfigRefused VoiceDevicesConfig = "refused"
	// VoiceDevicesConfigNone: there is no configuration directory (neither
	// CONCH_CONFIG_DIR, XDG_CONFIG_HOME nor HOME is set), so there is no file
	// to read.
	VoiceDevicesConfigNone VoiceDevicesConfig = "none"
)

// Valid reports whether c is a recognized configuration state.
func (c VoiceDevicesConfig) Valid() bool {
	switch c {
	case VoiceDevicesConfigOK, VoiceDevicesConfigMissing, VoiceDevicesConfigRefused, VoiceDevicesConfigNone:
		return true
	default:
		return false
	}
}

// VoiceDevice is one microphone or speaker, in the order PipeWire lists them.
type VoiceDevice struct {
	// Name is the PipeWire node.name, which is what [audio] input or output
	// in voice.toml takes: 1 to VoiceDevicesMaxNameBytes bytes of UTF-8, no
	// control character, no `"` and no `\`, so that it can be written into
	// the file between quotes as it stands.
	Name string `json:"name"`
	// Description is the device's own description, which PipeWire supplies
	// and the device chose (a marketing name, usually): 0 to
	// VoiceDevicesMaxDescriptionChars characters, no control character.
	// Always written; empty when the device has none worth showing.
	Description string `json:"description"`
	// Default is whether this is PipeWire's default for its kind. Always
	// written.
	Default bool `json:"default"`
}

// VoiceDevicesV1 is the device list as `conch-voice devices --json` writes
// it: the microphones and speakers PipeWire has, whether the list is
// complete, and what voice.toml names today.
type VoiceDevicesV1 struct {
	// Schema is the document discriminator; always VoiceDevicesSchemaV1.
	Schema string `json:"schema"`
	// PipeWire is whether PipeWire could be reached.
	PipeWire VoiceDevicesPipeWire `json:"pipewire"`
	// Synced is whether PipeWire finished listing its devices within the
	// wait. When false the lists may be incomplete.
	Synced bool `json:"synced"`
	// Microphones is every source fit to show, at most VoiceDevicesMaxListed.
	// Always a JSON array; empty when PipeWire is unavailable.
	Microphones []VoiceDevice `json:"microphones"`
	// Speakers is every sink fit to show, at most VoiceDevicesMaxListed.
	// Always a JSON array; empty when PipeWire is unavailable.
	Speakers []VoiceDevice `json:"speakers"`
	// UnshownMicrophones is how many sources are not in Microphones: those
	// whose names break the rule above, plus any beyond VoiceDevicesMaxListed.
	UnshownMicrophones int `json:"unshown_microphones"`
	// UnshownSpeakers is the same count for sinks.
	UnshownSpeakers int `json:"unshown_speakers"`
	// Config is what became of voice.toml.
	Config VoiceDevicesConfig `json:"config"`
	// ConfiguredInput is [audio] input when Config is ok and the key names a
	// device; absent means PipeWire's default, and the writer normalizes the
	// value "default" to absent. Under the same rule as a Name, so what the
	// file holds can always be shown and compared with the list. Absent
	// whenever Config is not ok.
	ConfiguredInput string `json:"configured_input,omitempty"`
	// ConfiguredOutput is [audio] output, by the same rules.
	ConfiguredOutput string `json:"configured_output,omitempty"`
}

// voiceDevicesDefaultName is the value of [audio] input or output that means
// PipeWire's default. The document says it by absence, never by this value.
const voiceDevicesDefaultName = "default"

// Validate reports whether the document is well-formed and consistent:
//
//   - the v1 schema name, and recognized pipewire and config values;
//   - both lists arrays (never null) of at most VoiceDevicesMaxListed, each
//     device under the name and description rules, and no count negative;
//   - nothing listed or counted while PipeWire is unavailable;
//   - configured_input and configured_output only while config is ok, under
//     the name rule, and never the value "default".
func (d VoiceDevicesV1) Validate() error {
	if d.Schema != VoiceDevicesSchemaV1 {
		return fmt.Errorf("schema: voice devices schema must be %q, got %q", VoiceDevicesSchemaV1, d.Schema)
	}
	if !d.PipeWire.Valid() {
		return fmt.Errorf("schema: voice devices pipewire %q is not one of ok, unavailable", d.PipeWire)
	}
	if !d.Config.Valid() {
		return fmt.Errorf("schema: voice devices config %q is not one of ok, missing, refused, none", d.Config)
	}
	if err := validateVoiceDeviceList("microphones", d.Microphones); err != nil {
		return err
	}
	if err := validateVoiceDeviceList("speakers", d.Speakers); err != nil {
		return err
	}
	if d.UnshownMicrophones < 0 {
		return fmt.Errorf("schema: voice devices unshown_microphones must not be negative, got %d", d.UnshownMicrophones)
	}
	if d.UnshownSpeakers < 0 {
		return fmt.Errorf("schema: voice devices unshown_speakers must not be negative, got %d", d.UnshownSpeakers)
	}
	if d.PipeWire == VoiceDevicesPipeWireUnavailable &&
		(len(d.Microphones) != 0 || len(d.Speakers) != 0 || d.UnshownMicrophones != 0 || d.UnshownSpeakers != 0) {
		return errors.New("schema: voice devices must list and count nothing while pipewire is unavailable")
	}
	for _, c := range []struct {
		field string
		value string
	}{{"configured_input", d.ConfiguredInput}, {"configured_output", d.ConfiguredOutput}} {
		if c.value == "" {
			continue
		}
		if d.Config != VoiceDevicesConfigOK {
			return fmt.Errorf("schema: voice devices %s is only for config ok, got config %q", c.field, d.Config)
		}
		if err := validateVoiceDeviceName(c.field, c.value); err != nil {
			return err
		}
		if c.value == voiceDevicesDefaultName {
			return fmt.Errorf("schema: voice devices %s says the default by absence, not by the value %q", c.field, voiceDevicesDefaultName)
		}
	}
	return nil
}

// validateVoiceDeviceList checks one list: an array, not too long, every
// device well-formed.
func validateVoiceDeviceList(field string, list []VoiceDevice) error {
	if list == nil {
		return fmt.Errorf("schema: voice devices %s must be an array, not null", field)
	}
	if len(list) > VoiceDevicesMaxListed {
		return fmt.Errorf("schema: voice devices %s lists %d devices, more than %d", field, len(list), VoiceDevicesMaxListed)
	}
	for i, dev := range list {
		if err := validateVoiceDeviceName(fmt.Sprintf("%s[%d].name", field, i), dev.Name); err != nil {
			return err
		}
		if err := validateVoiceDeviceDescription(fmt.Sprintf("%s[%d].description", field, i), dev.Description); err != nil {
			return err
		}
	}
	return nil
}

// validateVoiceDeviceName is the rule for a name that can be written into
// voice.toml between quotes as it stands. It does not echo the name.
func validateVoiceDeviceName(field, name string) error {
	if name == "" || len(name) > VoiceDevicesMaxNameBytes {
		return fmt.Errorf("schema: voice devices %s must be 1 to %d bytes", field, VoiceDevicesMaxNameBytes)
	}
	if !utf8.ValidString(name) {
		return fmt.Errorf("schema: voice devices %s must be valid UTF-8", field)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("schema: voice devices %s must not contain control characters", field)
	}
	if strings.ContainsAny(name, `"\`) {
		return fmt.Errorf("schema: voice devices %s must not contain a quotation mark or a backslash", field)
	}
	return nil
}

// validateVoiceDeviceDescription is the rule for device-supplied text that a
// reader will show: bounded, one line. It does not echo the description.
func validateVoiceDeviceDescription(field, description string) error {
	if !utf8.ValidString(description) {
		return fmt.Errorf("schema: voice devices %s must be valid UTF-8", field)
	}
	if utf8.RuneCountInString(description) > VoiceDevicesMaxDescriptionChars {
		return fmt.Errorf("schema: voice devices %s must be at most %d characters", field, VoiceDevicesMaxDescriptionChars)
	}
	if strings.ContainsFunc(description, unicode.IsControl) {
		return fmt.Errorf("schema: voice devices %s must not contain control characters", field)
	}
	return nil
}

// ParseVoiceDevices reads the document as the helper wrote it: the whole of
// its standard output, line feed included. It is how the TUI's side is
// specified. It refuses, in this order:
//
//   - more than VoiceDevicesMaxBytes;
//   - not valid UTF-8, not exactly one JSON object (white space around it is
//     allowed), or a key given twice, at the top level or in a device;
//   - schema missing, not a string, or not VoiceDevicesSchemaV1: a document
//     of another version is not read as this one;
//   - a declared field missing, null or of the wrong JSON type, except
//     configured_input and configured_output, which may be absent;
//   - the document failing Validate.
//
// Field names are matched exactly, case included. A field this version does
// not declare is ignored, at the top level and in a device, so that a later
// writer can add one. The error never repeats the input.
func ParseVoiceDevices(data []byte) (VoiceDevicesV1, error) {
	if len(data) > VoiceDevicesMaxBytes {
		return VoiceDevicesV1{}, fmt.Errorf("schema: voice devices document is %d bytes, more than %d", len(data), VoiceDevicesMaxBytes)
	}
	if !utf8.Valid(data) {
		return VoiceDevicesV1{}, errors.New("schema: voice devices document is not valid UTF-8")
	}
	fields, err := decodeVoiceDevicesObject(data)
	if err != nil {
		return VoiceDevicesV1{}, err
	}
	if s, ok := fieldString(fields, "schema"); !ok || s != VoiceDevicesSchemaV1 {
		return VoiceDevicesV1{}, fmt.Errorf("schema: voice devices schema is not %s", VoiceDevicesSchemaV1)
	}
	r := fieldReader{fields: fields}
	d := VoiceDevicesV1{Schema: VoiceDevicesSchemaV1}
	d.PipeWire = VoiceDevicesPipeWire(r.str("pipewire"))
	d.Synced = r.bool("synced")
	if d.Microphones, err = r.devices("microphones"); err != nil {
		return VoiceDevicesV1{}, err
	}
	if d.Speakers, err = r.devices("speakers"); err != nil {
		return VoiceDevicesV1{}, err
	}
	d.UnshownMicrophones = r.count("unshown_microphones")
	d.UnshownSpeakers = r.count("unshown_speakers")
	d.Config = VoiceDevicesConfig(r.str("config"))
	d.ConfiguredInput = r.optStr("configured_input")
	d.ConfiguredOutput = r.optStr("configured_output")
	if r.missing != "" {
		return VoiceDevicesV1{}, fmt.Errorf("schema: voice devices document needs %s of the right type", r.missing)
	}
	if err := d.Validate(); err != nil {
		return VoiceDevicesV1{}, err
	}
	return d, nil
}

// count reads an integer field into the int the counts are declared as.
func (r *fieldReader) count(key string) int {
	var n int
	r.take(key, &n, true)
	return n
}

// devices reads a list of devices, each an object under the same rules as the
// document: no key twice, exact names, unknown fields ignored, no null. A
// device that is not an object, or a field of one missing or of the wrong
// type, is recorded as the list missing; a key given twice in a device is the
// framing error it is at the top level.
func (r *fieldReader) devices(key string) ([]VoiceDevice, error) {
	var raws []json.RawMessage
	if !r.take(key, &raws, true) {
		return nil, nil
	}
	list := make([]VoiceDevice, 0, len(raws))
	for _, raw := range raws {
		fields, err := decodeVoiceDevicesObject(raw)
		if errors.Is(err, errVoiceDevicesKeyTwice) {
			return nil, err
		}
		if err != nil {
			if r.missing == "" {
				r.missing = key
			}
			return nil, nil
		}
		entry := fieldReader{fields: fields}
		dev := VoiceDevice{
			Name:        entry.str("name"),
			Description: entry.str("description"),
			Default:     entry.bool("default"),
		}
		if entry.missing != "" {
			if r.missing == "" {
				r.missing = key + "." + entry.missing
			}
			return nil, nil
		}
		list = append(list, dev)
	}
	return list, nil
}

// MarshalVoiceDevices encodes the document as the helper writes it: compact
// JSON on one line, and a line feed. It refuses a document that fails
// Validate, so a writer (a Go fake helper, in tests) cannot write what a
// reader must refuse. "<", ">" and "&" are written as themselves, as the Rust
// side writes them.
func MarshalVoiceDevices(d VoiceDevicesV1) ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("schema: encode voice devices: %w", err)
	}
	// Encode ends the value with a line feed of its own.
	return buf.Bytes(), nil
}

// The framing errors, never repeating the input.
var (
	errVoiceDevicesNotAnObject = errors.New("schema: voice devices document is not one JSON object")
	errVoiceDevicesKeyTwice    = errors.New("schema: voice devices document gives a key twice")
	errVoiceDevicesTwoValues   = errors.New("schema: voice devices document holds more than one JSON value")
)

// decodeVoiceDevicesObject reads exactly one JSON object, white space around
// it allowed, and returns its fields unparsed. A key given twice is refused,
// where encoding/json would keep the last.
func decodeVoiceDevicesObject(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errVoiceDevicesNotAnObject
	}
	fields := make(map[string]json.RawMessage)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errVoiceDevicesNotAnObject
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errVoiceDevicesNotAnObject
		}
		if _, dup := fields[key]; dup {
			return nil, errVoiceDevicesKeyTwice
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, errVoiceDevicesNotAnObject
		}
		fields[key] = value
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, errVoiceDevicesNotAnObject
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errVoiceDevicesTwoValues
	}
	return fields, nil
}
