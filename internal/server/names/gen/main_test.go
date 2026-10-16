package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseConfusables(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []prototype
		wantErr string
	}{
		{
			name: "rows sorted by source, comments and a byte order mark ignored",
			in: "\xef\xbb\xbf# confusables.txt\n# Version: 18.0.0\n\n" +
				"FB01 ;\t0066 0069 ;\tMA\t# ( ﬁ → fi )\n" +
				"0430 ;\t0061 ;\tMA\t# ( а → a )\n",
			want: []prototype{{0x0430, "a"}, {0xFB01, "fi"}},
		},
		{name: "two code points as a source", in: "0041 0042 ; 0043 ; MA\n", wantErr: "not one code point"},
		{name: "a source listed twice", in: "0430 ; 0061 ; MA\n0430 ; 0062 ; MA\n", wantErr: "listed twice"},
		{name: "not hexadecimal", in: "ZZZZ ; 0061 ; MA\n", wantErr: "bad code point"},
		{name: "a surrogate", in: "D800 ; 0061 ; MA\n", wantErr: "bad code point"},
		{name: "one field", in: "0430\n", wantErr: "at least two fields"},
		{name: "nothing in it", in: "# only comments\n", wantErr: "no mappings"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseConfusables([]byte(tt.in))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseDefaultIgnorable(t *testing.T) {
	in := "# Derived Property: Alphabetic\n" +
		"0041..005A    ; Alphabetic # L&  [26]\n" +
		"200B..200F    ; Default_Ignorable_Code_Point # Cf   [5]\n" +
		"00AD          ; Default_Ignorable_Code_Point # Cf\n" +
		"2060..2064    ; Default_Ignorable_Code_Point # Cf   [5]\n" +
		"2065          ; Default_Ignorable_Code_Point # Cn\n"
	got, err := parseDefaultIgnorable([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []runeRange{{0x00AD, 0x00AD}, {0x200B, 0x200F}, {0x2060, 0x2065}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (sorted, adjacent ranges merged, other properties ignored)", got, want)
	}
	if _, err := parseDefaultIgnorable([]byte("0041 ; Alphabetic\n")); err == nil {
		t.Error("a file without the property was accepted")
	}
}

// A data file whose hash is not the pin is refused; the pinned one is read.
func TestFetchChecksThePin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	data := []byte("0430 ; 0061 ; MA\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	pin := hex.EncodeToString(sum[:])
	if got, err := fetch("https://example.invalid/data.txt", path, pin); err != nil || string(got) != string(data) {
		t.Fatalf("fetch with the right pin = %q, %v", got, err)
	}
	other := strings.Repeat("0", 64)
	if _, err := fetch("https://example.invalid/data.txt", path, other); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("fetch with another pin = %v, want a refusal", err)
	}
}

// The output depends on nothing but its input: the same rows render to the
// same bytes, and the source parses as Go.
func TestRenderIsDeterministic(t *testing.T) {
	p := []prototype{{0x0430, "a"}, {0xFB01, "fi"}}
	r := []runeRange{{0x200B, 0x200F}}
	a, err := render(p, r)
	if err != nil {
		t.Fatal(err)
	}
	b, err := render(p, r)
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("two renders of the same rows differ")
	}
	for _, want := range []string{"DO NOT EDIT", confusablesURL, confusablesSHA256, derivedSHA256, "UNICODE LICENSE V3", `{0xFB01, "fi"}`} {
		if !strings.Contains(string(a), want) {
			t.Errorf("the table does not contain %q", want)
		}
	}
}
