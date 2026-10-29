package schema

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

// voiceControlPipeDir is the fixture directory of the attach line, read by
// these tests and by conch-voice-api's control_pipe.rs.
const voiceControlPipeDir = "voice-control-pipe"

// voiceControlPipeGoldenNonce is the nonce of the golden line: this
// directory's fake secret (sensitive.json).
const voiceControlPipeGoldenNonce = "FAKE_nonce_do_not_print_0123456789abcdefghA"

func readVoiceControlPipeFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", voiceControlPipeDir, filepath.FromSlash(name))) // #nosec G304 -- a fixture name from this package's own tables
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fixtureFilesUnder lists every file under testdata/dir, by its path relative
// to it, with forward slashes.
func fixtureFilesUnder(t *testing.T, dir string) []string {
	t.Helper()
	root := filepath.Join("testdata", dir)
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

// goldenAttachedLine is the golden fixture as the helper writes it: compact,
// without its line feed.
func goldenAttachedLine(t *testing.T) []byte {
	t.Helper()
	var compact bytes.Buffer
	if err := json.Compact(&compact, readVoiceControlPipeFixture(t, "attached.json")); err != nil {
		t.Fatal(err)
	}
	return compact.Bytes()
}

// TestVoiceControlPipeGoldenFixture checks attached.json three ways: it
// round-trips through its type byte for byte, its compact form is accepted by
// the owner's reader, and MarshalVoiceControlLine writes exactly that compact
// form with one line feed.
func TestVoiceControlPipeGoldenFixture(t *testing.T) {
	assertGoldenFixture(t, voiceControlPipeDir+"/attached.json", func() any { return new(VoiceControlPipeAttachedV1) })

	line := goldenAttachedLine(t)
	attached, err := ParseVoiceControlPipeAttached(line)
	if err != nil {
		t.Fatalf("the owner's reader refuses the golden line: %v", err)
	}
	want := VoiceControlPipeAttachedV1{
		Schema: VoiceControlPipeSchemaV1, Kind: VoiceControlPipeKindAttached, Nonce: voiceControlPipeGoldenNonce,
	}
	if attached != want {
		t.Errorf("read as %+v, want %+v", attached, want)
	}
	written, err := MarshalVoiceControlLine(attached)
	if err != nil {
		t.Fatal(err)
	}
	if want := append(slices.Clone(line), '\n'); !bytes.Equal(written, want) {
		t.Errorf("written as\n%s\nwant\n%s", written, want)
	}
}

// voiceControlPipeCase is one conformance case; the same shape as the control
// protocol's, with the one direction this reader has.
type voiceControlPipeCase struct {
	Name      string  `json:"name"`
	Direction string  `json:"direction"`
	Line      *string `json:"line"`
	LineHex   string  `json:"line_hex"`
	LineFile  string  `json:"line_file"`
	Want      struct {
		Result string `json:"result"`
		Kind   string `json:"kind"`
		Code   string `json:"code"`
		ID     int64  `json:"id"`
	} `json:"want"`
}

func loadVoiceControlPipeCases(t *testing.T) []voiceControlPipeCase {
	t.Helper()
	var doc struct {
		Description string                 `json:"description"`
		Cases       []voiceControlPipeCase `json:"cases"`
	}
	if err := json.Unmarshal(readVoiceControlPipeFixture(t, "cases.json"), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Description == "" || len(doc.Cases) == 0 {
		t.Fatal("cases.json has no description or no cases")
	}
	return doc.Cases
}

// line is the case's line, from whichever of its three forms it has.
func (c voiceControlPipeCase) line(t *testing.T) []byte {
	t.Helper()
	// The control protocol's cases have the same three forms; borrow its reader.
	shared := voiceControlCase{Name: c.Name, Line: c.Line, LineHex: c.LineHex}
	if c.LineFile != "" {
		if c.Line != nil || c.LineHex != "" {
			t.Fatalf("case %q has more than one form of its line", c.Name)
		}
		return readVoiceControlPipeFixture(t, c.LineFile)
	}
	return shared.line(t)
}

// TestVoiceControlPipeCases runs the conformance cases the Rust twin runs too:
// each line is read by ParseVoiceControlPipeAttached and accepted as the kind
// given or refused with the code given, with an ID of 0 and an error that
// repeats neither the line nor any nonce.
func TestVoiceControlPipeCases(t *testing.T) {
	sensitive := loadVoiceControlPipeSensitive(t)
	names := map[string]bool{}
	for _, c := range loadVoiceControlPipeCases(t) {
		if names[c.Name] {
			t.Errorf("case %q appears twice", c.Name)
		}
		names[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			if c.Direction != "attached" {
				t.Fatalf("direction %q is not attached", c.Direction)
			}
			line := c.line(t)
			attached, err := ParseVoiceControlPipeAttached(line)
			switch c.Want.Result {
			case "accept":
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if string(attached.Kind) != c.Want.Kind {
					t.Errorf("read as %q, want %q", attached.Kind, c.Want.Kind)
				}
				if err := attached.Validate(); err != nil {
					t.Errorf("accepted but not valid: %v", err)
				}
			case "refuse":
				var vcErr *VoiceControlError
				if !errors.As(err, &vcErr) {
					t.Fatalf("accepted as %+v (error %v), want refused with %s", attached, err, c.Want.Code)
				}
				if string(vcErr.Code) != c.Want.Code || vcErr.ID != c.Want.ID {
					t.Errorf("refused with %s (id %d): %v; want %s (id %d)", vcErr.Code, vcErr.ID, err, c.Want.Code, c.Want.ID)
				}
				if attached != (VoiceControlPipeAttachedV1{}) {
					t.Errorf("a refused line came back with a value: %+v", attached)
				}
				text := err.Error()
				if len(line) > 0 && strings.Contains(text, string(line)) {
					t.Errorf("the error repeats the line: %v", err)
				}
				for _, nonce := range sensitive.FakeNonces {
					if strings.Contains(text, nonce) {
						t.Errorf("the error repeats the nonce: %v", err)
					}
				}
			default:
				t.Fatalf("want.result %q is not accept or refuse", c.Want.Result)
			}
		})
	}
}

// TestVoiceControlPipeCasesCoverEveryCode makes sure each code the reader can
// return is shown by at least one case, so that the Rust side is held to it.
func TestVoiceControlPipeCasesCoverEveryCode(t *testing.T) {
	seen := map[string]bool{}
	accepted := 0
	for _, c := range loadVoiceControlPipeCases(t) {
		switch c.Want.Result {
		case "refuse":
			seen[c.Want.Code] = true
		case "accept":
			accepted++
		}
	}
	for _, want := range []string{"too_long", "malformed", "unknown_schema", "invalid", "unknown_kind"} {
		if !seen[want] {
			t.Errorf("no case refuses with %s", want)
		}
	}
	if accepted < 3 {
		t.Errorf("only %d cases are accepted", accepted)
	}
}

// TestVoiceControlPipeLongLines pins the two long lines to the limit exactly.
func TestVoiceControlPipeLongLines(t *testing.T) {
	for file, want := range map[string]int{
		"lines/attached-at-limit.txt": VoiceControlMaxLineBytes,
		"lines/attached-too-long.txt": VoiceControlMaxLineBytes + 1,
	} {
		data := readVoiceControlPipeFixture(t, file)
		if len(data) != want {
			t.Errorf("%s is %d bytes, want %d", file, len(data), want)
		}
		if bytes.IndexByte(data, '\n') >= 0 {
			t.Errorf("%s holds a line feed", file)
		}
	}
}

// TestVoiceControlPipeTranscript walks transcript-attach-then-hello.json: the
// first step is the attach line, read by the attach reader and matching the
// golden nonce; everything after it is the control protocol, held to its
// ordering rules by the v1 transcript checker.
func TestVoiceControlPipeTranscript(t *testing.T) {
	var doc struct {
		Description string             `json:"description"`
		Steps       []voiceControlStep `json:"steps"`
	}
	if err := json.Unmarshal(readVoiceControlPipeFixture(t, "transcript-attach-then-hello.json"), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Description == "" {
		t.Error("no description")
	}
	if len(doc.Steps) < 3 {
		t.Fatalf("%d steps", len(doc.Steps))
	}
	first := doc.Steps[0]
	if first.From != "helper" || first.Message == nil {
		t.Fatalf("the first step is not a helper message: %+v", first)
	}
	var line bytes.Buffer
	if err := json.Compact(&line, first.Message); err != nil {
		t.Fatal(err)
	}
	attached, err := ParseVoiceControlPipeAttached(line.Bytes())
	if err != nil {
		t.Fatalf("step 0: the attach reader refuses it: %v", err)
	}
	if !attached.Matches(voiceControlPipeGoldenNonce) {
		t.Error("step 0: the nonce is not the one the owner generated")
	}
	// The attach line is not an event: the control readers refuse it, in
	// either version, as unknown_schema, which is how a second one is caught.
	for name, parse := range map[string]func([]byte) error{
		"v1": func(b []byte) error { _, err := ParseVoiceControlEvent(b); return err },
		"v2": func(b []byte) error { _, err := ParseVoiceControlEventV2(b); return err },
	} {
		var vcErr *VoiceControlError
		if err := parse(line.Bytes()); !errors.As(err, &vcErr) || vcErr.Code != VoiceControlErrorUnknownSchema {
			t.Errorf("the %s event reader does not refuse the attach line as unknown_schema: %v", name, err)
		}
	}
	rest := doc.Steps[1:]
	if rest[0].From != "helper" || rest[0].Message == nil {
		t.Fatal("the second step is not the hello")
	}
	checkVoiceControlTranscript(t, rest)
}

// TestVoiceControlPipeFixturesAreAllClaimed fails on a file in the fixture
// directory that no test reads, so nothing there goes unchecked.
func TestVoiceControlPipeFixturesAreAllClaimed(t *testing.T) {
	claimed := map[string]bool{
		"attached.json": true, "cases.json": true, "sensitive.json": true,
		"transcript-attach-then-hello.json": true,
	}
	for _, c := range loadVoiceControlPipeCases(t) {
		if c.LineFile != "" {
			claimed[c.LineFile] = true
		}
	}
	for _, f := range fixtureFilesUnder(t, voiceControlPipeDir) {
		if !claimed[f] {
			t.Errorf("testdata/%s/%s is read by no test", voiceControlPipeDir, f)
		}
	}
}

type voiceControlPipeSensitive struct {
	FakeNonces    []string `json:"fake_nonces"`
	Fragments     []string `json:"forbidden_field_fragments"`
	NeverInEvents []string `json:"never_in_events"`
}

func loadVoiceControlPipeSensitive(t *testing.T) voiceControlPipeSensitive {
	t.Helper()
	var sensitive voiceControlPipeSensitive
	if err := json.Unmarshal(readVoiceControlPipeFixture(t, "sensitive.json"), &sensitive); err != nil {
		t.Fatal(err)
	}
	if len(sensitive.FakeNonces) < 2 || len(sensitive.Fragments) == 0 || len(sensitive.NeverInEvents) == 0 {
		t.Fatal("sensitive.json lists too little")
	}
	return sensitive
}

// fieldNamedLike reports whether a field's JSON tag or Go name holds fragment.
func fieldNamedLike(f reflect.StructField, fragment string) bool {
	tag := strings.Split(f.Tag.Get("json"), ",")[0]
	return strings.Contains(strings.ToLower(tag), fragment) || strings.Contains(strings.ToLower(f.Name), fragment)
}

// TestVoiceControlPipeCarriesNoSecrets holds the attach line to sensitive.json:
// no field of the line is named like a credential, a room, the endpoint or the
// process; no field of any message of the control protocol, v1 or v2, is named
// like the nonce or the attach (the nonce is spent when read and never echoed);
// no fixture here holds a fake secret of the control protocol's; and the fake
// nonces, which are well-formed, appear in no fixture of the control protocol.
func TestVoiceControlPipeCarriesNoSecrets(t *testing.T) {
	sensitive := loadVoiceControlPipeSensitive(t)
	for _, nonce := range sensitive.FakeNonces {
		if err := ValidateVoiceControlPipeNonce(nonce); err != nil {
			t.Errorf("fake nonce %d is not a well-formed nonce: %v", slices.Index(sensitive.FakeNonces, nonce), err)
		}
	}
	if sensitive.FakeNonces[0] != voiceControlPipeGoldenNonce {
		t.Error("the first fake nonce is not the golden line's")
	}

	typ := reflect.TypeOf(VoiceControlPipeAttachedV1{})
	for i := 0; i < typ.NumField(); i++ {
		for _, frag := range sensitive.Fragments {
			if fieldNamedLike(typ.Field(i), frag) {
				t.Errorf("%s.%s is named like a secret or an endpoint (%q)", typ.Name(), typ.Field(i).Name, frag)
			}
		}
	}

	// Every message of the control protocol, both directions and both
	// versions: the rule is about events, and holding commands to it as well
	// costs nothing and keeps the nonce off standard input too.
	var control []VoiceControlMessage
	control = append(control, voiceControlTypes...)
	control = append(control, voiceControlV2Types...)
	if len(control) < 19 {
		t.Fatalf("%d control message types", len(control))
	}
	for _, msg := range control {
		typ := reflect.TypeOf(msg)
		for i := 0; i < typ.NumField(); i++ {
			for _, frag := range sensitive.NeverInEvents {
				if fieldNamedLike(typ.Field(i), frag) {
					t.Errorf("%s.%s is named like the attach (%q); the nonce is never echoed", typ.Name(), typ.Field(i).Name, frag)
				}
			}
		}
	}

	var controlSensitive struct {
		FakeSecrets []string `json:"fake_secrets"`
	}
	if err := json.Unmarshal(readVoiceControlFixture(t, "sensitive.json"), &controlSensitive); err != nil {
		t.Fatal(err)
	}
	if len(controlSensitive.FakeSecrets) == 0 {
		t.Fatal("voice-control/sensitive.json lists no fake secrets")
	}
	for _, name := range fixtureFilesUnder(t, voiceControlPipeDir) {
		data := readVoiceControlPipeFixture(t, name)
		for _, secret := range controlSensitive.FakeSecrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s/%s holds a fake secret of the control protocol", voiceControlPipeDir, name)
			}
		}
	}

	for _, dir := range []string{voiceControlDir, voiceControlV2Dir} {
		for _, name := range fixtureFilesUnder(t, dir) {
			data, err := os.ReadFile(filepath.Join("testdata", dir, filepath.FromSlash(name))) // #nosec G304 -- a fixture path this test listed itself
			if err != nil {
				t.Fatal(err)
			}
			for _, nonce := range sensitive.FakeNonces {
				if bytes.Contains(data, []byte(nonce)) {
					t.Errorf("%s/%s holds a fake nonce: the nonce is never on the control channel", dir, name)
				}
			}
		}
	}
}

// The fields of the attach line are an exact list. A new field has to be added
// here on purpose, by someone who has asked whether it may ride on the first
// line of an events endpoint and whether it could carry a secret.
func TestVoiceControlPipeFieldsAreAnExactList(t *testing.T) {
	want := []string{"schema", "kind", "nonce"}
	typ := reflect.TypeOf(VoiceControlPipeAttachedV1{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, strings.Split(typ.Field(i).Tag.Get("json"), ",")[0])
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s has fields %v, the reviewed list is %v", typ.Name(), got, want)
	}
}

// TestValidateVoiceControlPipeNonce pins the nonce rule: 43 bytes of the
// base64url alphabet whose last byte leaves the two trailing bits zero, so
// that the value decodes to exactly 32 bytes; and an error that never repeats
// the value. The Rust twin has the same table.
func TestValidateVoiceControlPipeNonce(t *testing.T) {
	golden := voiceControlPipeGoldenNonce
	tests := []struct {
		name  string
		nonce string
		ok    bool
	}{
		{"the golden nonce", golden, true},
		{"the other fake nonce", "FAKE_other_nonce_not_the_one_0123456789abcA", true},
		{"both URL-safe characters, ending canonically", "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmn-_0", true},
		{"42 characters", golden[:42], false},
		{"44 characters", golden + "A", false},
		{"a last character that is not canonical", golden[:42] + "B", false},
		{"a last character that is not canonical, upper", golden[:42] + "Z", false},
		{"+", "FAKE+nonce_do_not_print_0123456789abcdefghA", false},
		{"/", "FAKE/nonce_do_not_print_0123456789abcdefghA", false},
		{"padding", golden[:42] + "=", false},
		{"64 hex characters", strings.Repeat("0123456789abcdef", 4), false},
		{"empty", "", false},
		{"a space", "FAKE nonce_do_not_print_0123456789abcdefghA", false},
		{"a line feed, which a base64 decoder would skip", golden[:42] + "\n", false},
		{"a carriage return", golden[:42] + "\r", false},
		{"a character outside ASCII", golden[:40] + "éA", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateVoiceControlPipeNonce(tt.nonce)
			if (err == nil) != tt.ok {
				t.Fatalf("ValidateVoiceControlPipeNonce(%d bytes) = %v, want ok=%v", len(tt.nonce), err, tt.ok)
			}
			if err != nil && tt.nonce != "" && strings.Contains(err.Error(), tt.nonce) {
				t.Errorf("the error repeats the value: %v", err)
			}
		})
	}
	// The sixteen characters whose two low bits are zero are the canonical
	// last ones; every other character of the alphabet is refused there.
	const canonical = "AEIMQUYcgkosw048"
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for _, last := range alphabet {
		err := ValidateVoiceControlPipeNonce(golden[:42] + string(last))
		if ok := strings.ContainsRune(canonical, last); (err == nil) != ok {
			t.Errorf("last character %q: %v, want accepted=%v", last, err, ok)
		}
	}
}

// TestNewVoiceControlPipeNonce draws nonces and holds each to the rule; with a
// short or failing reader there is an error and no nonce.
func TestNewVoiceControlPipeNonce(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		nonce, err := NewVoiceControlPipeNonce(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if len(nonce) != VoiceControlPipeNonceChars {
			t.Fatalf("%d characters", len(nonce))
		}
		if err := ValidateVoiceControlPipeNonce(nonce); err != nil {
			t.Fatalf("draw %d: %v", i, err)
		}
		if seen[nonce] {
			t.Fatalf("draw %d repeats an earlier one", i)
		}
		seen[nonce] = true
	}
	if nonce, err := NewVoiceControlPipeNonce(nil); err != nil || ValidateVoiceControlPipeNonce(nonce) != nil {
		t.Errorf("nil stands for crypto/rand: %q, %v", nonce, err)
	}
	for name, r := range map[string]io.Reader{
		"a short reader":   strings.NewReader("short"),
		"an empty reader":  strings.NewReader(""),
		"a failing reader": iotest.ErrReader(errors.New("no entropy")),
		"a reader that fails after a few bytes": iotest.TimeoutReader(
			iotest.OneByteReader(strings.NewReader(strings.Repeat("x", VoiceControlPipeNonceBytes))),
		),
	} {
		nonce, err := NewVoiceControlPipeNonce(r)
		if err == nil || nonce != "" {
			t.Errorf("%s: got %q, %v; want an error and no nonce", name, nonce, err)
		}
	}
	// The encoding is the one Node's Buffer.toString('base64url') writes:
	// 32 known bytes come out as the known 43 characters.
	nonce, err := NewVoiceControlPipeNonce(bytes.NewReader(bytes.Repeat([]byte{0}, VoiceControlPipeNonceBytes)))
	if err != nil || nonce != strings.Repeat("A", VoiceControlPipeNonceChars) {
		t.Errorf("32 zero bytes encode as %q, %v", nonce, err)
	}
	nonce, err = NewVoiceControlPipeNonce(bytes.NewReader(bytes.Repeat([]byte{0xff}, VoiceControlPipeNonceBytes)))
	if err != nil || nonce != strings.Repeat("_", 42)+"8" {
		t.Errorf("32 0xff bytes encode as %q, %v", nonce, err)
	}
}

// TestVoiceControlPipeMatches pins the comparison: equal nonces match; one
// differing only in its last byte, and one of another length, do not; and the
// line's own nonce must be the generated one, not merely well-formed.
func TestVoiceControlPipeMatches(t *testing.T) {
	golden := voiceControlPipeGoldenNonce
	attached := VoiceControlPipeAttachedV1{Schema: VoiceControlPipeSchemaV1, Kind: VoiceControlPipeKindAttached, Nonce: golden}
	tests := []struct {
		name  string
		nonce string
		want  bool
	}{
		{"the same nonce", golden, true},
		{"the last byte differs, still canonical", golden[:42] + "E", false},
		{"the first byte differs", "G" + golden[1:], false},
		{"one byte shorter", golden[:42], false},
		{"one byte longer", golden + "A", false},
		{"empty", "", false},
		{"the other fake nonce", "FAKE_other_nonce_not_the_one_0123456789abcA", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := attached.Matches(tt.nonce); got != tt.want {
				t.Errorf("Matches = %v, want %v", got, tt.want)
			}
		})
	}
	if (VoiceControlPipeAttachedV1{}).Matches("") {
		t.Error("an empty line matches an empty nonce; a zero value must never match")
	}
}

// TestMarshalVoiceControlPipeLine checks that the writer refuses what a reader
// must refuse: a wrong schema or kind, and every malformed nonce of the table.
func TestMarshalVoiceControlPipeLine(t *testing.T) {
	good := VoiceControlPipeAttachedV1{Schema: VoiceControlPipeSchemaV1, Kind: VoiceControlPipeKindAttached, Nonce: voiceControlPipeGoldenNonce}
	if _, err := MarshalVoiceControlLine(good); err != nil {
		t.Fatalf("the golden value is not written: %v", err)
	}
	bad := map[string]VoiceControlPipeAttachedV1{
		"the control protocol's schema": {Schema: VoiceControlSchemaV1, Kind: VoiceControlPipeKindAttached, Nonce: voiceControlPipeGoldenNonce},
		"no schema":                     {Kind: VoiceControlPipeKindAttached, Nonce: voiceControlPipeGoldenNonce},
		"another kind":                  {Schema: VoiceControlPipeSchemaV1, Kind: "hello", Nonce: voiceControlPipeGoldenNonce},
		"no kind":                       {Schema: VoiceControlPipeSchemaV1, Nonce: voiceControlPipeGoldenNonce},
		"no nonce":                      {Schema: VoiceControlPipeSchemaV1, Kind: VoiceControlPipeKindAttached},
		"a short nonce":                 {Schema: VoiceControlPipeSchemaV1, Kind: VoiceControlPipeKindAttached, Nonce: voiceControlPipeGoldenNonce[:42]},
		"a non-canonical nonce":         {Schema: VoiceControlPipeSchemaV1, Kind: VoiceControlPipeKindAttached, Nonce: voiceControlPipeGoldenNonce[:42] + "B"},
		"the zero value":                {},
	}
	for name, msg := range bad {
		if line, err := MarshalVoiceControlLine(msg); err == nil {
			t.Errorf("%s: wrote %s", name, line)
		} else if strings.Contains(err.Error(), voiceControlPipeGoldenNonce) {
			t.Errorf("%s: the error repeats the nonce: %v", name, err)
		}
	}
}

// TestVoiceControlPipeKind pins the vocabulary to its one value.
func TestVoiceControlPipeKind(t *testing.T) {
	if !VoiceControlPipeKindAttached.Valid() || VoiceControlPipeKind("").Valid() || VoiceControlPipeKind("hello").Valid() {
		t.Error("Valid is not exactly attached")
	}
	if got := Vocabularies["VoiceControlPipeKind"]; !slices.Equal(got, []string{"attached"}) {
		t.Errorf("Vocabularies[VoiceControlPipeKind] = %q", got)
	}
}

// TestVoiceControlPipeConstants pins the numbers and names the owner and the
// helper are held to, as the Rust twin pins them against this file.
func TestVoiceControlPipeConstants(t *testing.T) {
	if VoiceControlPipeSchemaV1 != "conch.voice_control_pipe.v1" ||
		VoiceControlPipeFlag != "--events-pipe" ||
		VoiceControlPipeNonceEnv != "CONCH_VOICE_ATTACH" ||
		VoiceControlPipeNonceBytes != 32 ||
		VoiceControlPipeNonceChars != 43 {
		t.Error("a constant is not the design's")
	}
	// 32 bytes in unpadded base64 are ceil(32*8/6) characters.
	if (VoiceControlPipeNonceBytes*8+5)/6 != VoiceControlPipeNonceChars {
		t.Error("the character count does not follow from the byte count")
	}
	if !strings.HasPrefix(VoiceControlPipeNonceEnv, "CONCH_") {
		t.Error("the nonce variable must be one the Go supervisor's CONCH_ filter strips")
	}
}

// TestVoiceControlPipeDoc keeps the words the design requires in the Go doc
// comment, and the fixture directory named in the design note.
func TestVoiceControlPipeDoc(t *testing.T) {
	src, err := os.ReadFile("voice_control_pipe.go")
	if err != nil {
		t.Fatal(err)
	}
	// The comment as prose: its line breaks and markers taken out.
	prose := strings.Join(strings.Fields(strings.ReplaceAll(string(src), "//", " ")), " ")
	for _, phrase := range []string{
		"a spawn-to-pipe binding, defence in depth, not authentication of either side",
		"same-user",
		"out of scope",
		"the one secret-shaped value on any voice control channel",
		"one-time",
		"spent when read",
		"redacted from every diagnostic",
	} {
		if !strings.Contains(prose, phrase) {
			t.Errorf("the doc comment does not say %q", phrase)
		}
	}
	doc, err := os.ReadFile(voiceControlDoc)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"`pkg/schema/testdata/" + voiceControlPipeDir + "/`",
		"`" + VoiceControlPipeSchemaV1 + "`",
		"windows-desktop.md", "linux-desktop.md",
	} {
		if !strings.Contains(string(doc), want) {
			t.Errorf("the design note does not name %s", want)
		}
	}
}
