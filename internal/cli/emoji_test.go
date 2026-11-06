package cli

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestResolveReactionKey(t *testing.T) {
	tests := []struct {
		name, input, want string
		wantErr           bool
	}{
		{name: "alias", input: ":+1:", want: "👍"},
		{name: "long alias", input: "thumbsup", want: "👍"},
		{name: "CLDR", input: ":thumbs_up:", want: "👍"},
		{name: "case insensitive", input: ":TADA:", want: "🎉"},
		{name: "unqualified heart", input: "❤", want: "❤️"},
		{name: "skin tone", input: "👍🏽", want: "👍🏽"},
		{name: "ZWJ", input: "👨‍👩‍👧", want: "👨‍👩‍👧"},
		{name: "flag", input: "🇫🇷", want: "🇫🇷"},
		{name: "unknown", input: ":not_an_emoji:", wantErr: true},
		{name: "hostile", input: "👍\u202e", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveReactionKey(tt.input)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("ResolveReactionKey(%q) = %q, %v; want %q, error %v", tt.input, got, err, tt.want, tt.wantErr)
			}
		})
	}
}

func TestExpandShortcodes(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"known", "nice :+1: and :tada:", "nice 👍 and 🎉"},
		{"unknown stays literal", "nice :not_an_emoji:", "nice :not_an_emoji:"},
		{"inside word", "time:tada:", "time:tada:"},
		{"URL", "https://example.test/:tada:", "https://example.test/:tada:"},
		{"opening bracket", "(:thumbs_up:)", "(👍)"},
		{"already Unicode", "👨‍👩‍👧 🇫🇷 👍🏽", "👨‍👩‍👧 🇫🇷 👍🏽"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExpandShortcodes(tt.input); got != tt.want {
				t.Errorf("ExpandShortcodes(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestReactionLabelFallback(t *testing.T) {
	tests := []struct{ name, key, want string }{
		{"alias", "👍", ":thumbsup:"},
		{"skin tone", "👍🏽", ":thumbs_up_medium_skin_tone:"},
		{"family", "👨‍👩‍👧", ":family_man_woman_girl:"},
		{"flag", "🇫🇷", ":flag_france:"},
		{"unknown is code points", "\U0001f999\u202e", "[U+1F999 U+202E]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReactionLabel(tt.key, true); got != tt.want {
				t.Errorf("ReactionLabel(%q, true) = %q, want %q", tt.key, got, tt.want)
			}
			if got := ReactionLabel(tt.key, false); got != tt.key {
				t.Errorf("Unicode label = %q, want the key unchanged", got)
			}
		})
	}
}

func TestEmojiVocabularyMatchesBrowserCatalog(t *testing.T) {
	data, err := os.ReadFile("../../web/src/emoji/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Version string `json:"version"`
		Groups  []struct {
			Subgroups []struct {
				Emoji []struct {
					Key     string   `json:"key"`
					Name    string   `json:"name"`
					Aliases []string `json:"aliases"`
				} `json:"emoji"`
			} `json:"subgroups"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatal(err)
	}
	if catalog.Version != schema.EmojiVersion {
		t.Fatalf("catalog version %q, schema %q", catalog.Version, schema.EmojiVersion)
	}
	seen := 0
	for _, group := range catalog.Groups {
		for _, subgroup := range group.Subgroups {
			for _, entry := range subgroup.Emoji {
				seen++
				if got := emojiNames[EmojiShortcode(entry.Name)]; got != entry.Key {
					t.Errorf("shortcode for %q resolves to %q, want %q", entry.Name, got, entry.Key)
				}
				for _, alias := range entry.Aliases {
					if got := emojiNames[alias]; got != entry.Key {
						t.Errorf("alias %q resolves to %q, want %q", alias, got, entry.Key)
					}
				}
			}
		}
	}
	if seen != len(schema.EmojiCatalog()) {
		t.Errorf("browser has %d entries, schema has %d", seen, len(schema.EmojiCatalog()))
	}
}
