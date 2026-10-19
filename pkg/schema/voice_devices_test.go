package schema

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// voiceDevicesDir is the fixture directory of the device list. It is a
// directory of its own, as voice-control is, so that the Rust check that every
// top-level voice-*.json fixture has a Rust type does not apply before #650
// adds the Rust copy.
const voiceDevicesDir = "voice-devices"

// voiceDevicesGolden is every golden fixture and what it shows.
var voiceDevicesGolden = []struct {
	name string
	file string
}{
	{name: "a full list with a default on each side and a configured input", file: "voice-devices-v1.json"},
	{name: "a virtual microphone with no description", file: "voice-devices-v1-virtual-microphone.json"},
	{name: "PipeWire unavailable", file: "voice-devices-v1-pipewire-unavailable.json"},
	{name: "a configured input absent from the list", file: "voice-devices-v1-configured-input-absent.json"},
	{name: "voice.toml refused", file: "voice-devices-v1-config-refused.json"},
	{name: "no configuration directory, and PipeWire not synced", file: "voice-devices-v1-config-none.json"},
	{name: "names not shown, counted", file: "voice-devices-v1-unshown.json"},
}

func readVoiceDevicesFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", voiceDevicesDir, filepath.FromSlash(name))) // #nosec G304 -- a fixture name from this package's own tables
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestVoiceDevicesGoldenFixtures checks each golden fixture three ways: it
// round-trips through the type byte for byte, it validates, and its compact
// form is what the reader accepts and MarshalVoiceDevices writes.
func TestVoiceDevicesGoldenFixtures(t *testing.T) {
	for _, tt := range voiceDevicesGolden {
		t.Run(tt.name, func(t *testing.T) {
			newDoc := func() any { return new(VoiceDevicesV1) }
			assertGoldenFixture(t, voiceDevicesDir+"/"+tt.file, newDoc)
			assertFixtureValidates(t, voiceDevicesDir+"/"+tt.file, newDoc)

			var compact bytes.Buffer
			if err := json.Compact(&compact, readVoiceDevicesFixture(t, tt.file)); err != nil {
				t.Fatal(err)
			}
			doc, err := ParseVoiceDevices(compact.Bytes())
			if err != nil {
				t.Fatalf("the reader refuses it: %v", err)
			}
			var decoded VoiceDevicesV1
			if err := json.Unmarshal(compact.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(doc, decoded) {
				t.Errorf("the reader read\n%+v\nencoding/json read\n%+v", doc, decoded)
			}
			line, err := MarshalVoiceDevices(doc)
			if err != nil {
				t.Fatal(err)
			}
			if want := append(compact.Bytes(), '\n'); !bytes.Equal(line, want) {
				t.Errorf("written as\n%s\nwant\n%s", line, want)
			}
		})
	}
}

func TestVoiceDevicesSchemaName(t *testing.T) {
	if VoiceDevicesSchemaV1 != "conch.voice_devices.v1" {
		t.Fatalf("VoiceDevicesSchemaV1 = %q", VoiceDevicesSchemaV1)
	}
	if VoiceDevicesSchemaV1 == VoiceControlSchemaV1 {
		t.Fatal("the device list and the control protocol share a schema name")
	}
}

func validVoiceDevicesV1() VoiceDevicesV1 {
	return VoiceDevicesV1{
		Schema:   VoiceDevicesSchemaV1,
		PipeWire: VoiceDevicesPipeWireOK,
		Synced:   true,
		Microphones: []VoiceDevice{
			{Name: "alsa_input.pci-0000_00_1f.3.analog-stereo", Description: "Built-in Audio Analog Stereo", Default: true},
			{Name: "speech_out", Description: "", Default: false},
		},
		Speakers: []VoiceDevice{
			{Name: "alsa_output.pci-0000_00_1f.3.analog-stereo", Description: "Built-in Audio Analog Stereo", Default: true},
		},
		Config:           VoiceDevicesConfigOK,
		ConfiguredInput:  "speech_out",
		ConfiguredOutput: "alsa_output.usb-Generic_USB_Audio-00.analog-stereo",
	}
}

// devices makes n distinct, well-formed devices.
func devices(n int) []VoiceDevice {
	out := make([]VoiceDevice, n)
	for i := range out {
		out[i] = VoiceDevice{Name: "device-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strings.Repeat("y", i/26)}
	}
	return out
}

func TestVoiceDevicesV1Validate(t *testing.T) {
	longName := strings.Repeat("n", VoiceDevicesMaxNameBytes)
	longDescription := strings.Repeat("d", VoiceDevicesMaxDescriptionChars)
	tests := []struct {
		name    string
		mutate  func(*VoiceDevicesV1)
		wantErr string
	}{
		{name: "valid", mutate: func(*VoiceDevicesV1) {}},
		{name: "valid, nothing configured", mutate: func(d *VoiceDevicesV1) { d.ConfiguredInput, d.ConfiguredOutput = "", "" }},
		{name: "valid, config missing", mutate: func(d *VoiceDevicesV1) {
			d.Config, d.ConfiguredInput, d.ConfiguredOutput = VoiceDevicesConfigMissing, "", ""
		}},
		{name: "valid, config refused", mutate: func(d *VoiceDevicesV1) {
			d.Config, d.ConfiguredInput, d.ConfiguredOutput = VoiceDevicesConfigRefused, "", ""
		}},
		{name: "valid, config none", mutate: func(d *VoiceDevicesV1) {
			d.Config, d.ConfiguredInput, d.ConfiguredOutput = VoiceDevicesConfigNone, "", ""
		}},
		{name: "valid, not synced", mutate: func(d *VoiceDevicesV1) { d.Synced = false }},
		{name: "valid, empty lists", mutate: func(d *VoiceDevicesV1) { d.Microphones, d.Speakers = []VoiceDevice{}, []VoiceDevice{} }},
		{name: "valid, pipewire unavailable", mutate: func(d *VoiceDevicesV1) {
			d.PipeWire, d.Synced, d.Microphones, d.Speakers = VoiceDevicesPipeWireUnavailable, false, []VoiceDevice{}, []VoiceDevice{}
		}},
		{name: "valid, the most devices", mutate: func(d *VoiceDevicesV1) {
			d.Microphones, d.Speakers = devices(VoiceDevicesMaxListed), devices(VoiceDevicesMaxListed)
		}},
		{name: "valid, counts", mutate: func(d *VoiceDevicesV1) { d.UnshownMicrophones, d.UnshownSpeakers = 3, 1 }},
		{name: "valid, the longest name and description", mutate: func(d *VoiceDevicesV1) {
			d.Microphones[0].Name, d.Microphones[0].Description = longName, longDescription
		}},
		{name: "valid, a name with spaces and characters outside ASCII", mutate: func(d *VoiceDevicesV1) {
			d.Microphones[0].Name = "Mikrofon für Sprache (USB)"
		}},
		{name: "valid, a description with a line separator (not a control character)", mutate: func(d *VoiceDevicesV1) {
			d.Microphones[0].Description = "a\u2028b"
		}},
		{name: "valid, a configured input the list does not have", mutate: func(d *VoiceDevicesV1) {
			d.ConfiguredInput = "alsa_input.usb-Generic_USB_Headset-00.mono-fallback"
		}},
		{name: "valid, two defaults on one side", mutate: func(d *VoiceDevicesV1) { d.Microphones[1].Default = true }},

		{name: "empty schema", mutate: func(d *VoiceDevicesV1) { d.Schema = "" }, wantErr: `voice devices schema must be "conch.voice_devices.v1", got ""`},
		{name: "the control protocol's schema", mutate: func(d *VoiceDevicesV1) { d.Schema = VoiceControlSchemaV1 }, wantErr: `got "conch.voice_control.v1"`},
		{name: "future schema version", mutate: func(d *VoiceDevicesV1) { d.Schema = "conch.voice_devices.v2" }, wantErr: `got "conch.voice_devices.v2"`},
		{name: "empty pipewire", mutate: func(d *VoiceDevicesV1) { d.PipeWire = "" }, wantErr: `voice devices pipewire "" is not one of ok, unavailable`},
		{name: "unknown pipewire", mutate: func(d *VoiceDevicesV1) { d.PipeWire = "starting" }, wantErr: `voice devices pipewire "starting" is not one of ok, unavailable`},
		{name: "empty config", mutate: func(d *VoiceDevicesV1) { d.Config = "" }, wantErr: `voice devices config "" is not one of ok, missing, refused, none`},
		{name: "unknown config", mutate: func(d *VoiceDevicesV1) { d.Config = "invalid" }, wantErr: `voice devices config "invalid" is not one of ok, missing, refused, none`},
		{name: "null microphones", mutate: func(d *VoiceDevicesV1) { d.Microphones = nil }, wantErr: "voice devices microphones must be an array, not null"},
		{name: "null speakers", mutate: func(d *VoiceDevicesV1) { d.Speakers = nil }, wantErr: "voice devices speakers must be an array, not null"},
		{name: "null lists while unavailable", mutate: func(d *VoiceDevicesV1) {
			d.PipeWire, d.Microphones, d.Speakers = VoiceDevicesPipeWireUnavailable, nil, nil
		}, wantErr: "voice devices microphones must be an array, not null"},
		{name: "one microphone too many", mutate: func(d *VoiceDevicesV1) { d.Microphones = devices(VoiceDevicesMaxListed + 1) }, wantErr: "voice devices microphones lists 65 devices, more than 64"},
		{name: "one speaker too many", mutate: func(d *VoiceDevicesV1) { d.Speakers = devices(VoiceDevicesMaxListed + 1) }, wantErr: "voice devices speakers lists 65 devices, more than 64"},
		{name: "empty name", mutate: func(d *VoiceDevicesV1) { d.Speakers[0].Name = "" }, wantErr: "voice devices speakers[0].name must be 1 to 255 bytes"},
		{name: "name one byte too long", mutate: func(d *VoiceDevicesV1) { d.Microphones[1].Name = longName + "n" }, wantErr: "voice devices microphones[1].name must be 1 to 255 bytes"},
		{name: "name of 255 characters in more than 255 bytes", mutate: func(d *VoiceDevicesV1) { d.Microphones[1].Name = strings.Repeat("ü", VoiceDevicesMaxNameBytes) }, wantErr: "voice devices microphones[1].name must be 1 to 255 bytes"},
		{name: "name not valid UTF-8", mutate: func(d *VoiceDevicesV1) { d.Microphones[0].Name = "alsa_input.\xff" }, wantErr: "voice devices microphones[0].name must be valid UTF-8"},
		{name: "name with a quotation mark", mutate: func(d *VoiceDevicesV1) { d.Microphones[0].Name = `alsa_input."quoted"` }, wantErr: "voice devices microphones[0].name must not contain a quotation mark or a backslash"},
		{name: "name with a backslash", mutate: func(d *VoiceDevicesV1) { d.Microphones[0].Name = `alsa_input.a\b` }, wantErr: "voice devices microphones[0].name must not contain a quotation mark or a backslash"},
		{name: "name with a line feed", mutate: func(d *VoiceDevicesV1) { d.Microphones[0].Name = "alsa_input.a\nb" }, wantErr: "voice devices microphones[0].name must not contain control characters"},
		{name: "name with an escape character", mutate: func(d *VoiceDevicesV1) { d.Microphones[0].Name = "\x1b[31malsa_input" }, wantErr: "voice devices microphones[0].name must not contain control characters"},
		{name: "name with a delete character", mutate: func(d *VoiceDevicesV1) { d.Microphones[0].Name = "alsa_input\x7f" }, wantErr: "voice devices microphones[0].name must not contain control characters"},
		{name: "name with a C1 control character", mutate: func(d *VoiceDevicesV1) { d.Microphones[0].Name = "alsa_input\u0085" }, wantErr: "voice devices microphones[0].name must not contain control characters"},
		{name: "description one character too long", mutate: func(d *VoiceDevicesV1) { d.Microphones[0].Description = longDescription + "d" }, wantErr: "voice devices microphones[0].description must be at most 80 characters"},
		{name: "description of 80 characters outside ASCII is not too long", mutate: func(d *VoiceDevicesV1) {
			d.Microphones[0].Description = strings.Repeat("ü", VoiceDevicesMaxDescriptionChars)
		}},
		{name: "description not valid UTF-8", mutate: func(d *VoiceDevicesV1) { d.Speakers[0].Description = "Built-in \xff" }, wantErr: "voice devices speakers[0].description must be valid UTF-8"},
		{name: "description with a tab", mutate: func(d *VoiceDevicesV1) { d.Speakers[0].Description = "Built-in\tAudio" }, wantErr: "voice devices speakers[0].description must not contain control characters"},
		{name: "negative unshown_microphones", mutate: func(d *VoiceDevicesV1) { d.UnshownMicrophones = -1 }, wantErr: "voice devices unshown_microphones must not be negative, got -1"},
		{name: "negative unshown_speakers", mutate: func(d *VoiceDevicesV1) { d.UnshownSpeakers = -2 }, wantErr: "voice devices unshown_speakers must not be negative, got -2"},
		{name: "microphones while unavailable", mutate: func(d *VoiceDevicesV1) {
			d.PipeWire, d.Speakers = VoiceDevicesPipeWireUnavailable, []VoiceDevice{}
		}, wantErr: "voice devices must list and count nothing while pipewire is unavailable"},
		{name: "speakers while unavailable", mutate: func(d *VoiceDevicesV1) {
			d.PipeWire, d.Microphones = VoiceDevicesPipeWireUnavailable, []VoiceDevice{}
		}, wantErr: "voice devices must list and count nothing while pipewire is unavailable"},
		{name: "a count while unavailable", mutate: func(d *VoiceDevicesV1) {
			d.PipeWire, d.Microphones, d.Speakers, d.UnshownSpeakers = VoiceDevicesPipeWireUnavailable, []VoiceDevice{}, []VoiceDevice{}, 1
		}, wantErr: "voice devices must list and count nothing while pipewire is unavailable"},
		{name: "configured_input while config missing", mutate: func(d *VoiceDevicesV1) {
			d.Config, d.ConfiguredOutput = VoiceDevicesConfigMissing, ""
		}, wantErr: `voice devices configured_input is only for config ok, got config "missing"`},
		{name: "configured_output while config refused", mutate: func(d *VoiceDevicesV1) {
			d.Config, d.ConfiguredInput = VoiceDevicesConfigRefused, ""
		}, wantErr: `voice devices configured_output is only for config ok, got config "refused"`},
		{name: "configured_input while config none", mutate: func(d *VoiceDevicesV1) {
			d.Config, d.ConfiguredOutput = VoiceDevicesConfigNone, ""
		}, wantErr: `voice devices configured_input is only for config ok, got config "none"`},
		{name: "configured_input of the value default", mutate: func(d *VoiceDevicesV1) { d.ConfiguredInput = "default" }, wantErr: `voice devices configured_input says the default by absence, not by the value "default"`},
		{name: "configured_output of the value default", mutate: func(d *VoiceDevicesV1) { d.ConfiguredOutput = "default" }, wantErr: `voice devices configured_output says the default by absence, not by the value "default"`},
		{name: "configured_input too long", mutate: func(d *VoiceDevicesV1) { d.ConfiguredInput = longName + "n" }, wantErr: "voice devices configured_input must be 1 to 255 bytes"},
		{name: "configured_output with a quotation mark", mutate: func(d *VoiceDevicesV1) { d.ConfiguredOutput = `a"b` }, wantErr: "voice devices configured_output must not contain a quotation mark or a backslash"},
		{name: "configured_input with a control character", mutate: func(d *VoiceDevicesV1) { d.ConfiguredInput = "a\x00b" }, wantErr: "voice devices configured_input must not contain control characters"},
		{name: "configured_output not valid UTF-8", mutate: func(d *VoiceDevicesV1) { d.ConfiguredOutput = "\xff" }, wantErr: "voice devices configured_output must be valid UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validVoiceDevicesV1()
			tt.mutate(&d)
			assertValidation(t, d.Validate(), tt.wantErr)
		})
	}
}

type voiceDevicesCase struct {
	Name     string  `json:"name"`
	Line     *string `json:"line"`
	LineHex  string  `json:"line_hex"`
	LineFile string  `json:"line_file"`
	Want     struct {
		Result string `json:"result"`
		Error  string `json:"error"`
	} `json:"want"`
}

func loadVoiceDevicesCases(t *testing.T) []voiceDevicesCase {
	t.Helper()
	var doc struct {
		Cases []voiceDevicesCase `json:"cases"`
	}
	if err := json.Unmarshal(readVoiceDevicesFixture(t, "cases.json"), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("no cases")
	}
	return doc.Cases
}

// input is the case's document, from whichever of its three forms it has.
func (c voiceDevicesCase) input(t *testing.T) []byte {
	t.Helper()
	forms := 0
	var data []byte
	if c.Line != nil {
		forms++
		data = []byte(*c.Line)
	}
	if c.LineHex != "" {
		forms++
		b, err := hex.DecodeString(c.LineHex)
		if err != nil {
			t.Fatal(err)
		}
		data = b
	}
	if c.LineFile != "" {
		forms++
		data = readVoiceDevicesFixture(t, c.LineFile)
	}
	if forms != 1 {
		t.Fatalf("case %q has %d forms of its document, want 1", c.Name, forms)
	}
	return data
}

// TestVoiceDevicesCases runs the reader over every case: each document is
// accepted, or refused with an error that says why and never repeats the
// input.
func TestVoiceDevicesCases(t *testing.T) {
	names := map[string]bool{}
	for _, c := range loadVoiceDevicesCases(t) {
		if names[c.Name] {
			t.Errorf("case %q appears twice", c.Name)
		}
		names[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			data := c.input(t)
			doc, err := ParseVoiceDevices(data)
			switch c.Want.Result {
			case "accept":
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if c.Want.Error != "" {
					t.Fatal("an accepted case names an error")
				}
				if err := doc.Validate(); err != nil {
					t.Errorf("read as something that does not validate: %v", err)
				}
			case "refuse":
				if err == nil {
					t.Fatalf("accepted as %+v, want refused with %q", doc, c.Want.Error)
				}
				if c.Want.Error == "" {
					t.Fatal("a refused case names no error")
				}
				if !strings.HasPrefix(err.Error(), "schema: voice devices ") {
					t.Errorf("error %q lacks the schema: voice devices prefix", err)
				}
				if !strings.Contains(err.Error(), c.Want.Error) {
					t.Errorf("refused with %q, want an error containing %q", err, c.Want.Error)
				}
				if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && strings.Contains(err.Error(), string(trimmed)) {
					t.Errorf("the error repeats the input: %v", err)
				}
			default:
				t.Fatalf("want.result %q is not accept or refuse", c.Want.Result)
			}
		})
	}
}

// TestVoiceDevicesCasesCoverEveryRefusal makes sure each way the reader can
// refuse a document is shown by at least one case.
func TestVoiceDevicesCasesCoverEveryRefusal(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range loadVoiceDevicesCases(t) {
		if c.Want.Result == "refuse" {
			seen[c.Want.Error] = true
		}
	}
	for _, want := range []string{
		"not one JSON object", "more than one JSON value", "gives a key twice", "not valid UTF-8",
		"schema is not conch.voice_devices.v1",
	} {
		if !seen[want] {
			t.Errorf("no case refuses with %q", want)
		}
	}
	for _, field := range []string{
		"pipewire", "synced", "microphones", "speakers", "unshown_microphones", "unshown_speakers", "config", "configured_input",
	} {
		if !seen["needs "+field+" of the right type"] {
			t.Errorf("no case shows %s missing or of the wrong type", field)
		}
	}
}

// largestVoiceDevicesV1 is the largest document the type allows, with every
// character chosen so that the encoder writes it as a six-byte escape: a line
// separator, which is not a control character and so passes Validate.
func largestVoiceDevicesV1() VoiceDevicesV1 {
	const sep = "\u2028"
	name := strings.Repeat(sep, VoiceDevicesMaxNameBytes/len(sep))
	description := strings.Repeat(sep, VoiceDevicesMaxDescriptionChars)
	list := func() []VoiceDevice {
		out := make([]VoiceDevice, VoiceDevicesMaxListed)
		for i := range out {
			out[i] = VoiceDevice{Name: name, Description: description, Default: true}
		}
		return out
	}
	return VoiceDevicesV1{
		Schema: VoiceDevicesSchemaV1, PipeWire: VoiceDevicesPipeWireOK, Synced: true,
		Microphones: list(), Speakers: list(),
		UnshownMicrophones: 1 << 30, UnshownSpeakers: 1 << 30,
		Config: VoiceDevicesConfigOK, ConfiguredInput: name, ConfiguredOutput: name,
	}
}

func TestParseVoiceDevicesSize(t *testing.T) {
	t.Run("the largest document fits", func(t *testing.T) {
		largest := largestVoiceDevicesV1()
		data, err := MarshalVoiceDevices(largest)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) > VoiceDevicesMaxBytes {
			t.Fatalf("%d bytes, more than %d", len(data), VoiceDevicesMaxBytes)
		}
		doc, err := ParseVoiceDevices(data)
		if err != nil {
			t.Fatalf("the reader refuses it: %v", err)
		}
		if !reflect.DeepEqual(doc, largest) {
			t.Error("read back as something else")
		}
	})
	t.Run("exactly the most bytes", func(t *testing.T) {
		data, err := MarshalVoiceDevices(validVoiceDevicesV1())
		if err != nil {
			t.Fatal(err)
		}
		padded := append(bytes.Repeat([]byte{' '}, VoiceDevicesMaxBytes-len(data)), data...)
		if _, err := ParseVoiceDevices(padded); err != nil {
			t.Fatalf("refused at %d bytes: %v", len(padded), err)
		}
	})
	t.Run("one byte more", func(t *testing.T) {
		data, err := MarshalVoiceDevices(validVoiceDevicesV1())
		if err != nil {
			t.Fatal(err)
		}
		padded := append(bytes.Repeat([]byte{' '}, VoiceDevicesMaxBytes+1-len(data)), data...)
		_, err = ParseVoiceDevices(padded)
		if err == nil || !strings.Contains(err.Error(), "more than 262144") {
			t.Fatalf("at %d bytes: %v", len(padded), err)
		}
	})
	t.Run("one device too many is refused by the reader too", func(t *testing.T) {
		d := validVoiceDevicesV1()
		d.Speakers = devices(VoiceDevicesMaxListed + 1)
		data, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseVoiceDevices(data); err == nil || !strings.Contains(err.Error(), "speakers lists 65 devices, more than 64") {
			t.Fatalf("%v", err)
		}
	})
}

func TestMarshalVoiceDevices(t *testing.T) {
	t.Run("one line feed, at the end, and HTML characters as themselves", func(t *testing.T) {
		d := validVoiceDevicesV1()
		d.Microphones[1].Description = "a<b>&c"
		line, err := MarshalVoiceDevices(d)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(line, []byte{'\n'}) != 1 || line[len(line)-1] != '\n' {
			t.Fatalf("%q", line)
		}
		if !bytes.Contains(line, []byte(`"a<b>&c"`)) {
			t.Errorf("HTML characters escaped: %s", line)
		}
		if bytes.Contains(line, []byte("configured_input\":\"\"")) {
			t.Errorf("an absent configured device written as empty: %s", line)
		}
	})
	t.Run("an invalid document is not written", func(t *testing.T) {
		d := validVoiceDevicesV1()
		d.Config = VoiceDevicesConfigMissing
		if line, err := MarshalVoiceDevices(d); err == nil {
			t.Errorf("wrote %s", line)
		}
	})
}

// TestVoiceDevicesFixturesAreAllClaimed fails on a file in the fixture
// directory that no test reads, so nothing there goes unchecked.
func TestVoiceDevicesFixturesAreAllClaimed(t *testing.T) {
	claimed := map[string]bool{"cases.json": true, "sensitive.json": true}
	for _, g := range voiceDevicesGolden {
		claimed[g.file] = true
	}
	for _, c := range loadVoiceDevicesCases(t) {
		if c.LineFile != "" {
			claimed[c.LineFile] = true
		}
	}
	for _, f := range voiceDevicesFixtureFiles(t) {
		if !claimed[f] {
			t.Errorf("testdata/%s/%s is read by no test", voiceDevicesDir, f)
		}
	}
}

// voiceDevicesFixtureFiles lists every file under the fixture directory, by
// its path relative to it, with forward slashes.
func voiceDevicesFixtureFiles(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("testdata", voiceDevicesDir)
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestVoiceDevicesCarriesNoSecrets holds the document to sensitive.json: no
// field of it or of a device is named like a credential, a room, a server or
// a place on disk. Its strings are device names, descriptions and what
// voice.toml names, and nothing else; which strings those are is pinned by
// TestVoiceDevicesFieldsAreAnExactList.
func TestVoiceDevicesCarriesNoSecrets(t *testing.T) {
	var sensitive struct {
		Fragments []string `json:"forbidden_field_fragments"`
	}
	if err := json.Unmarshal(readVoiceDevicesFixture(t, "sensitive.json"), &sensitive); err != nil {
		t.Fatal(err)
	}
	if len(sensitive.Fragments) == 0 {
		t.Fatal("sensitive.json lists nothing")
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(VoiceDevicesV1{}), reflect.TypeOf(VoiceDevice{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			for _, frag := range sensitive.Fragments {
				if strings.Contains(strings.ToLower(tag), frag) || strings.Contains(strings.ToLower(f.Name), frag) {
					t.Errorf("%s.%s (%q) is named like a secret (%q)", typ.Name(), f.Name, tag, frag)
				}
			}
		}
	}
}

// The fields are an exact list: the field list is final once merged, and a
// new field has to be added here on purpose, by someone who has asked whether
// it could carry a secret or text a terminal would act on.
func TestVoiceDevicesFieldsAreAnExactList(t *testing.T) {
	want := map[string][]string{
		"VoiceDevicesV1": {
			"schema", "pipewire", "synced", "microphones", "speakers", "unshown_microphones", "unshown_speakers",
			"config", "configured_input", "configured_output",
		},
		"VoiceDevice": {"name", "description", "default"},
	}
	for _, typ := range []reflect.Type{reflect.TypeOf(VoiceDevicesV1{}), reflect.TypeOf(VoiceDevice{})} {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
		}
		if !slices.Equal(got, want[typ.Name()]) {
			t.Errorf("%s has fields %v, the reviewed list is %v", typ.Name(), got, want[typ.Name()])
		}
	}
}

// TestVoiceDevicesDoc keeps the design note and this package together: the
// note names the schema, this file and the fixture directory.
func TestVoiceDevicesDoc(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "design", "conch-voice.md")) // #nosec G304 -- this repository's own design note, by a fixed path
	if err != nil {
		t.Fatal(err)
	}
	doc := string(data)
	for _, want := range []string{"`" + VoiceDevicesSchemaV1 + "`", "pkg/schema/voice_devices.go", "pkg/schema/testdata/" + voiceDevicesDir + "/"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the design note does not name %s", want)
		}
	}
}
