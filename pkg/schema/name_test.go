package schema

import (
	"strings"
	"testing"
)

// The accepted and refused sets of issue #204, exercised through
// ValidateDisplayName and through each request type that uses it.
func TestValidateDisplayName(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // empty: accepted; otherwise a substring of the error
	}{
		// Accepted: letters of any script, digits, interior spaces, ordinary
		// punctuation.
		{"accented", "Zoë Müller", ""},
		{"cjk with interior space", "山田 太郎", ""},
		{"apostrophe and hyphen", "o'brien-smith", ""},
		{"arabic", "أحمد علي", ""},
		{"hebrew", "אבי כהן", ""},
		{"hebrew with left-to-right mark", "אבי כהן\u200e", ""},
		{"arabic letter mark is a mark", "أحمد\u061c2", ""},
		{"indic joiner", "தமிழ்\u200dநாடு", ""},
		{"digits and punctuation", "build-bot 2.0 (eu-west)", ""},
		{"empty is the caller's rule", "", ""},
		// Refused: what a name can do to a log line or a terminal.
		{"escape sequence", "MARK\x1b[31mRED", "control characters"},
		{"newline", "LINE\nONE", "control characters"},
		{"carriage return", "a\rb", "control characters"},
		{"tab", "a\tb", "control characters"},
		{"DEL", "a\x7fb", "control characters"},
		{"C1 NEL", "a\u0085b", "control characters"},
		{"line separator", "a\u2028b", "control characters"},
		{"paragraph separator", "a\u2029b", "control characters"},
		{"right-to-left override", "a\u202eb", "bidi controls"},
		{"left-to-right embed", "a\u202ab", "bidi controls"},
		{"pop directional formatting", "a\u202cb", "bidi controls"},
		{"left-to-right isolate", "a\u2066b", "bidi controls"},
		{"pop directional isolate", "a\u2069b", "bidi controls"},
		{"zero-width space", "ali\u200bce", "invisible formatting"},
		{"word joiner", "a\u2060b", "invisible formatting"},
		{"zero-width no-break space", "a\ufeffb", "invisible formatting"},
		{"soft hyphen", "a\u00adb", "invisible formatting"},
		{"tag character", "a\U000e0041b", "invisible formatting"},
		{"leading space", " alice", "whitespace"},
		{"trailing tab", "alice\t", "whitespace"},
		{"trailing newline", "alice\n", "whitespace"},
		{"leading nbsp", "\u00a0alice", "whitespace"},
		{"whitespace only", "   ", "whitespace"},
		{"too long", strings.Repeat("a", 33), "at most 32 characters"},
		{"too long counts characters", strings.Repeat("é", 33), "at most 32 characters"},
		{"invalid utf-8", "bad\xff", "valid UTF-8"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDisplayName("test name", tt.input, 32)
			if tt.want == "" {
				if err != nil {
					t.Errorf("ValidateDisplayName(%q) = %v, want accepted", tt.input, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("ValidateDisplayName(%q) = %v, want an error naming %q", tt.input, err, tt.want)
			}
			// The message names the rule; it never echoes the input.
			if err != nil && tt.input != "" && strings.Contains(err.Error(), tt.input) {
				t.Errorf("error echoes the refused input: %q", err)
			}
		})
	}
}

func TestCreatePrincipalRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     CreatePrincipalRequest
		wantErr bool
	}{
		{"human", CreatePrincipalRequest{Kind: PrincipalHuman, Name: "Zoë Müller"}, false},
		{"agent", CreatePrincipalRequest{Kind: PrincipalAgent, Name: "o'brien-smith"}, false},
		{"bad kind", CreatePrincipalRequest{Kind: "robot", Name: "hal"}, true},
		{"empty name", CreatePrincipalRequest{Kind: PrincipalHuman, Name: ""}, true},
		{"blank name", CreatePrincipalRequest{Kind: PrincipalHuman, Name: "  "}, true},
		{"bidi override", CreatePrincipalRequest{Kind: PrincipalHuman, Name: "a\u202eb"}, true},
		{"over length", CreatePrincipalRequest{Kind: PrincipalHuman, Name: strings.Repeat("a", MaxPrincipalNameLength+1)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.req.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCreateChannelRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		req     CreateChannelRequest
		wantErr bool
	}{
		{"simple", CreateChannelRequest{Name: "general"}, false},
		{"unicode", CreateChannelRequest{Name: "山田 太郎"}, false},
		{"empty", CreateChannelRequest{Name: ""}, true},
		{"blank", CreateChannelRequest{Name: " "}, true},
		{"newline", CreateChannelRequest{Name: "a\nb"}, true},
		{"bidi override", CreateChannelRequest{Name: "a\u202eb"}, true},
		{"over length", CreateChannelRequest{Name: strings.Repeat("a", MaxChannelNameLength+1)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.req.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
