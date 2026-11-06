package schema

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// The generated catalog's own properties: the version string, the bounds
// every key sits inside, no two keys that are one once stripped, and every
// key its own canonical form.
func TestEmojiCatalog(t *testing.T) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+$`).MatchString(EmojiVersion) {
		t.Errorf("EmojiVersion = %q, want <major>.<minor>", EmojiVersion)
	}
	all := EmojiCatalog()
	if len(all) < 3000 {
		t.Fatalf("EmojiCatalog() has %d entries, want the whole RGI set", len(all))
	}
	if len(all) != len(emojiByStripped) {
		t.Errorf("%d entries but %d stripped forms: two keys collide once stripped", len(all), len(emojiByStripped))
	}
	seen := map[string]bool{}
	groups := map[string]bool{}
	for _, e := range all {
		if seen[e.Key] {
			t.Errorf("key %q is listed twice", e.Key)
		}
		seen[e.Key] = true
		if e.Name == "" || e.Group == "" || e.Subgroup == "" {
			t.Errorf("entry %q lacks a name, group or subgroup", e.Key)
		}
		groups[e.Group] = true
		if len(e.Key) > MaxReactionKeyBytes || utf8.RuneCountInString(e.Key) > MaxReactionKeyCodePoints {
			t.Errorf("key %q is outside the §3.3 bounds", e.Key)
		}
		if err := ValidateReactionKey(e.Key); err != nil {
			t.Errorf("ValidateReactionKey(%q) = %v, want nil: every catalog key is canonical", e.Key, err)
		}
	}
	if groups["Component"] {
		t.Error("the Component group is in the catalog; components alone are not emoji")
	}
	for _, want := range []string{"Smileys & Emotion", "People & Body", "Flags"} {
		if !groups[want] {
			t.Errorf("no group %q", want)
		}
	}
	// A skin-tone modifier alone is a component, not a key.
	for r := rune(0x1F3FB); r <= 0x1F3FF; r++ {
		if _, ok := emojiByStripped[string(r)]; ok {
			t.Errorf("component U+%X is in the lookup table", r)
		}
	}
}

// EmojiCatalog returns a copy: a caller cannot change the table.
func TestEmojiCatalogIsACopy(t *testing.T) {
	first := EmojiCatalog()
	first[0].Name = "changed"
	if again := EmojiCatalog(); again[0].Name == "changed" {
		t.Error("changing the returned slice changed the catalog")
	}
}

// The catalog is in the file's order, which starts with the grinning face.
func TestEmojiCatalogOrder(t *testing.T) {
	all := EmojiCatalog()
	if all[0].Key != string(rune(0x1F600)) || all[0].Name != "grinning face" {
		t.Errorf("first entry = %+v, want the grinning face", all[0])
	}
	if !strings.HasPrefix(all[len(all)-1].Group, "Flags") {
		t.Errorf("last entry = %+v, want a flag", all[len(all)-1])
	}
}

func TestStripPresentationSelectors(t *testing.T) {
	tests := []struct {
		in, want []rune
	}{
		{[]rune{0x1F44D}, []rune{0x1F44D}},
		{[]rune{0x2764, 0xFE0F}, []rune{0x2764}},
		{[]rune{0x2764, 0xFE0E}, []rune{0x2764}},
		{[]rune{0xFE0F, 0x1F44D, 0xFE0F, 0xFE0E}, []rune{0x1F44D}},
		{[]rune{0x1F3F3, 0xFE0F, 0x200D, 0x26A7, 0xFE0F}, []rune{0x1F3F3, 0x200D, 0x26A7}},
		{[]rune{}, []rune{}},
	}
	for _, tt := range tests {
		if got := stripPresentationSelectors(string(tt.in)); got != string(tt.want) {
			t.Errorf("strip(%U) = %U, want %U", tt.in, []rune(got), tt.want)
		}
	}
}
