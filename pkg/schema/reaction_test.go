package schema

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seq builds a string from code points, so every test input here is written
// by number (the invisible-chars rule) and the code points are visible.
func seq(points ...rune) string { return string(points) }

// The §3.2 examples and K1: canonical key or refusal exactly as tabled, a
// stray selector anywhere canonicalizes to the listed key, every accepted
// output validates as canonical, and canonicalization is idempotent.
func TestCanonicalReactionKey(t *testing.T) {
	const (
		fe0f = 0xFE0F
		fe0e = 0xFE0E
		zwj  = 0x200D
	)
	tests := []struct {
		name string
		in   string
		want string // "" means refused
	}{
		{"thumbs up, fully-qualified", seq(0x1F44D), seq(0x1F44D)},
		{"red heart, unqualified", seq(0x2764), seq(0x2764, fe0f)},
		{"red heart, fully-qualified", seq(0x2764, fe0f), seq(0x2764, fe0f)},
		{"red heart, text presentation", seq(0x2764, fe0e), seq(0x2764, fe0f)},
		{"thumbs up with a stray selector", seq(0x1F44D, fe0f), seq(0x1F44D)},
		{"thumbs up with a stray text selector", seq(0x1F44D, fe0e), seq(0x1F44D)},
		{"a selector before the emoji", seq(fe0f, 0x1F44D), seq(0x1F44D)},
		{"thumbs up, medium skin tone", seq(0x1F44D, 0x1F3FD), seq(0x1F44D, 0x1F3FD)},
		{"skin tone with a stray selector", seq(0x1F44D, fe0f, 0x1F3FD), seq(0x1F44D, 0x1F3FD)},
		{"medium skin tone alone (component)", seq(0x1F3FD), ""},
		{"red hair alone (component)", seq(0x1F9B0), ""},
		{"keycap #, fully-qualified", seq(0x23, fe0f, 0x20E3), seq(0x23, fe0f, 0x20E3)},
		{"keycap #, unqualified", seq(0x23, 0x20E3), seq(0x23, fe0f, 0x20E3)},
		{"keycap 0, unqualified", seq(0x30, 0x20E3), seq(0x30, fe0f, 0x20E3)},
		{"# alone", "#", ""},
		{"rainbow flag, unqualified", seq(0x1F3F3, zwj, 0x1F308), seq(0x1F3F3, fe0f, zwj, 0x1F308)},
		{"rainbow flag, fully-qualified", seq(0x1F3F3, fe0f, zwj, 0x1F308), seq(0x1F3F3, fe0f, zwj, 0x1F308)},
		{"rainbow flag with a second selector", seq(0x1F3F3, fe0f, zwj, 0x1F308, fe0f), seq(0x1F3F3, fe0f, zwj, 0x1F308)},
		{"white flag alone", seq(0x1F3F3), seq(0x1F3F3, fe0f)},
		{"transgender flag, minimally-qualified", seq(0x1F3F3, fe0f, zwj, 0x26A7), seq(0x1F3F3, fe0f, zwj, 0x26A7, fe0f)},
		{"transgender flag, unqualified", seq(0x1F3F3, zwj, 0x26A7), seq(0x1F3F3, fe0f, zwj, 0x26A7, fe0f)},
		{"family: man, woman, girl", seq(0x1F468, zwj, 0x1F469, zwj, 0x1F467), seq(0x1F468, zwj, 0x1F469, zwj, 0x1F467)},
		{"three people without ZWJ", seq(0x1F468, 0x1F469, 0x1F467), ""},
		{"flag: France", seq(0x1F1EB, 0x1F1F7), seq(0x1F1EB, 0x1F1F7)},
		{"one regional indicator", seq(0x1F1EB), ""},
		{"flag: England (tag sequence)", seq(0x1F3F4, 0xE0067, 0xE0062, 0xE0065, 0xE006E, 0xE0067, 0xE007F), seq(0x1F3F4, 0xE0067, 0xE0062, 0xE0065, 0xE006E, 0xE0067, 0xE007F)},
		{"black flag with an unlisted tag sequence", seq(0x1F3F4, 0xE0067, 0xE0062, 0xE007F), ""},
		{"black flag alone", seq(0x1F3F4), seq(0x1F3F4)},
		{"woman shrugging, fully-qualified", seq(0x1F937, zwj, 0x2640, fe0f), seq(0x1F937, zwj, 0x2640, fe0f)},
		{"woman shrugging, minimally-qualified", seq(0x1F937, zwj, 0x2640), seq(0x1F937, zwj, 0x2640, fe0f)},
		{"person shrugging is distinct", seq(0x1F937), seq(0x1F937)},
		{"man shrugging is distinct", seq(0x1F937, zwj, 0x2642, fe0f), seq(0x1F937, zwj, 0x2642, fe0f)},
		{"longest listed: kiss with two skin tones", seq(0x1F9D1, 0x1F3FB, zwj, 0x2764, fe0f, zwj, 0x1F48B, zwj, 0x1F9D1, 0x1F3FC), seq(0x1F9D1, 0x1F3FB, zwj, 0x2764, fe0f, zwj, 0x1F48B, zwj, 0x1F9D1, 0x1F3FC)},
		{"two thumbs", seq(0x1F44D, 0x1F44D), ""},
		{"a shortcode", ":+1:", ""},
		{"words", "thumbs up", ""},
		{"a letter", "A", ""},
		{"a digit", "1", ""},
		{"a space", " ", ""},
		{"empty", "", ""},
		{"a selector alone", seq(fe0f), ""},
		{"a ZWJ alone", seq(zwj), ""},
		{"thumbs up then a bidi control", seq(0x1F44D, 0x202E), ""},
		{"thumbs up then NUL", seq(0x1F44D, 0), ""},
		{"thumbs up then a space", seq(0x1F44D, 0x20), ""},
		{"a ZWJ after thumbs up", seq(0x1F44D, zwj), ""},
		{"thumbs up then a tag terminator", seq(0x1F44D, 0xE007F), ""},
		{"a replacement character", seq(0xFFFD), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CanonicalReactionKey(tt.in)
			if tt.want == "" {
				if err == nil {
					t.Fatalf("CanonicalReactionKey(%U) = %U, want refused", []rune(tt.in), []rune(got))
				}
				if !errors.Is(err, ErrInvalidReaction) {
					t.Errorf("err = %v, want one wrapping ErrInvalidReaction", err)
				}
				if ValidateReactionKey(tt.in) == nil {
					t.Errorf("ValidateReactionKey(%U) = nil for a refused input", []rune(tt.in))
				}
				return
			}
			if err != nil {
				t.Fatalf("CanonicalReactionKey(%U) = %v, want %U", []rune(tt.in), err, []rune(tt.want))
			}
			if got != tt.want {
				t.Fatalf("CanonicalReactionKey(%U) = %U, want %U", []rune(tt.in), []rune(got), []rune(tt.want))
			}
			if err := ValidateReactionKey(got); err != nil {
				t.Errorf("ValidateReactionKey(canonical) = %v, want nil", err)
			}
			again, err := CanonicalReactionKey(got)
			if err != nil || again != got {
				t.Errorf("canonical(canonical(x)) = %U, %v; want %U", []rune(again), err, []rune(got))
			}
			if got != tt.in && ValidateReactionKey(tt.in) == nil {
				t.Errorf("ValidateReactionKey(%U) = nil for a non-canonical input", []rune(tt.in))
			}
		})
	}
}

// K2: invalid UTF-8 is refused before the bounds and before any lookup; the
// bounds before the lookup; the rest by the failed lookup. The wrapped
// message says which rule refused.
func TestCanonicalReactionKeyOrder(t *testing.T) {
	thumbs := seq(0x1F44D)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"a lone 0xff", "\xff", "not valid UTF-8"},
		{"0xff 0xfe", "\xff\xfe", "not valid UTF-8"},
		{"a truncated sequence", "\xf0\x9f\x91", "not valid UTF-8"},
		{"an overlong encoding of a slash", "\xc0\xaf", "not valid UTF-8"},
		{"a lone surrogate encoding", "\xed\xa0\x80", "not valid UTF-8"},
		{"invalid UTF-8 after a valid emoji", thumbs + "\xff", "not valid UTF-8"},
		// Over the byte bound and valid: 65 bytes of invalid UTF-8 report
		// the UTF-8 rule, 65 ASCII bytes the byte bound.
		{"invalid UTF-8 over the byte bound", strings.Repeat("\xff", MaxReactionKeyBytes+1), "not valid UTF-8"},
		{"65 ASCII bytes", strings.Repeat("a", MaxReactionKeyBytes+1), "longer than 64 bytes"},
		{"17 thumbs (68 bytes)", strings.Repeat(thumbs, MaxReactionKeyCodePoints+1), "longer than 64 bytes"},
		{"17 letters with the thumbs after (within bytes, over code points)", strings.Repeat("a", MaxReactionKeyCodePoints) + thumbs, "more than 16 code points"},
		{"64 ASCII bytes: within bytes, over code points", strings.Repeat("a", MaxReactionKeyBytes), "more than 16 code points"},
		{"16 letters: within both bounds, not an emoji", strings.Repeat("a", MaxReactionKeyCodePoints), "not one emoji of Unicode Emoji " + EmojiVersion},
		// 16 four-byte runes are exactly 64 bytes: at both bounds, so the
		// lookup refuses it (a rune is at most four bytes, so an input over
		// the byte bound and within the code-point bound does not exist).
		{"16 thumbs: at both bounds, not one emoji", strings.Repeat(thumbs, MaxReactionKeyCodePoints), "not one emoji"},
		{"NUL", "\x00", "not one emoji"},
		{"a control", "\x1b", "not one emoji"},
		{"a bidi control", seq(0x202E), "not one emoji"},
		{"whitespace", " \t\n", "not one emoji"},
		{"a shortcode", ":+1:", "not one emoji"},
		{"empty", "", "not one emoji"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CanonicalReactionKey(tt.in)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CanonicalReactionKey(%q) = %v, want an error containing %q", tt.in, err, tt.want)
			}
			if !errors.Is(err, ErrInvalidReaction) {
				t.Errorf("err = %v, want one wrapping ErrInvalidReaction", err)
			}
		})
	}
}

// A key with selectors stripped is at most as long as the input, so a
// canonical key fits the bounds whenever its input did; but an input inside
// the bounds whose canonical form is longer (a missing selector added back)
// is still accepted: the bounds are on the input (§3.3).
func TestCanonicalReactionKeyBoundsAreOnTheInput(t *testing.T) {
	in := seq(0x1F3F3, 0x200D, 0x26A7) // unqualified transgender flag, 3 code points
	got, err := CanonicalReactionKey(in)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got)); n != 5 {
		t.Errorf("canonical form has %d code points, want 5 (two selectors added back)", n)
	}
}

func TestValidateReactionKey(t *testing.T) {
	if err := ValidateReactionKey(seq(0x2764, 0xFE0F)); err != nil {
		t.Errorf("a canonical key: %v", err)
	}
	err := ValidateReactionKey(seq(0x2764))
	if err == nil || !strings.Contains(err.Error(), "not the canonical key") || !errors.Is(err, ErrInvalidReaction) {
		t.Errorf("an unqualified key: %v, want 'not the canonical key' wrapping ErrInvalidReaction", err)
	}
	if err := ValidateReactionKey("\xff"); err == nil || !strings.Contains(err.Error(), "not valid UTF-8") {
		t.Errorf("invalid UTF-8: %v", err)
	}
}

func TestReactionConstants(t *testing.T) {
	if ReactionSummarySchemaV1 != "conch.reaction_summary.v1" {
		t.Errorf("ReactionSummarySchemaV1 = %q", ReactionSummarySchemaV1)
	}
	// The wire bounds are published values: the web client is generated with
	// them (TSConstants) and the three clients refuse the same inputs.
	for name, got := range map[string]struct{ got, want int }{
		"MaxReactionKeyBytes":                {MaxReactionKeyBytes, 64},
		"MaxReactionKeyCodePoints":           {MaxReactionKeyCodePoints, 16},
		"MaxReactionKindsPerMessage":         {MaxReactionKindsPerMessage, 20},
		"MaxReactionsPerPrincipalPerMessage": {MaxReactionsPerPrincipalPerMessage, 10},
		"MaxReactionKindsOnWire":             {MaxReactionKindsOnWire, 100},
		"MaxReactionBatchMessages":           {MaxReactionBatchMessages, 100},
		"DefaultReactorLimit":                {DefaultReactorLimit, 50},
		"MaxReactorLimit":                    {MaxReactorLimit, 100},
	} {
		if got.got != got.want {
			t.Errorf("%s = %d, want %d", name, got.got, got.want)
		}
	}
	if MaxReactionKindsOnWire < MaxReactionKindsPerMessage {
		t.Error("the wire bound is below the write limit: a full message could not encode")
	}
	if DefaultReactorLimit > MaxReactorLimit {
		t.Error("the default reactor page is larger than the largest allowed")
	}
	// Every scalar the web client is generated with is here, with the value
	// it has.
	want := map[string]any{
		"EMOJI_VERSION":                EmojiVersion,
		"MAX_REACTION_KEY_BYTES":       MaxReactionKeyBytes,
		"MAX_REACTION_KEY_CODE_POINTS": MaxReactionKeyCodePoints,
		"MAX_REACTION_BATCH_MESSAGES":  MaxReactionBatchMessages,
		"DEFAULT_REACTOR_LIMIT":        DefaultReactorLimit,
		"MAX_REACTOR_LIMIT":            MaxReactorLimit,
		// Message editing (issue #845).
		"MESSAGE_EDIT_WINDOW_SECONDS": MessageEditWindowSeconds,
		"MAX_MESSAGE_EDITS":           MaxMessageEdits,
		// The voice-control protocol, for the desktop owner (issue #948).
		"VOICE_CONTROL_SUBCOMMAND":               VoiceControlSubcommand,
		"VOICE_CONTROL_MAX_LINE_BYTES":           VoiceControlMaxLineBytes,
		"VOICE_CONTROL_MAX_ID":                   int64(VoiceControlMaxID),
		"VOICE_CONTROL_MAX_IN_FLIGHT":            VoiceControlMaxInFlight,
		"VOICE_CONTROL_MAX_VERSIONS":             VoiceControlMaxVersions,
		"VOICE_CONTROL_MAX_HELPER_VERSION_BYTES": VoiceControlMaxHelperVersionBytes,
		"VOICE_CONTROL_MAX_SERVER_BYTES":         VoiceControlMaxServerBytes,
		"VOICE_CONTROL_MAX_CHANNEL_BYTES":        VoiceControlMaxChannelBytes,
		"VOICE_CONTROL_MAX_RETRY_IN_MS":          VoiceControlMaxRetryInMS,
		"VOICE_CONTROL_HELLO_TIMEOUT_MS":         VoiceControlHelloTimeoutMS,
		"VOICE_CONTROL_ACK_TIMEOUT_MS":           VoiceControlAckTimeoutMS,
		"VOICE_CONTROL_GATE_SHUT_MS":             VoiceControlGateShutMS,
		"VOICE_CONTROL_CAPTURE_STOP_MS":          VoiceControlCaptureStopMS,
		"VOICE_CONTROL_OWNER_LOSS_EXIT_MS":       VoiceControlOwnerLossExitMS,
		"VOICE_CONTROL_LEAVE_EXIT_MS":            VoiceControlLeaveExitMS,
		"VOICE_CONTROL_STOP_TERM_AFTER_MS":       VoiceControlStopTermAfterMS,
		"VOICE_CONTROL_STOP_KILL_AFTER_MS":       VoiceControlStopKillAfterMS,
		"VOICE_CONTROL_DEVICE_DEFAULT":           VoiceControlDeviceDefault,
		"VOICE_DEVICES_MAX_NAME_BYTES":           VoiceDevicesMaxNameBytes,
		"VOICE_CONTROL_PIPE_FLAG":                VoiceControlPipeFlag,
		"VOICE_CONTROL_PIPE_NONCE_ENV":           VoiceControlPipeNonceEnv,
		"VOICE_CONTROL_PIPE_NONCE_BYTES":         VoiceControlPipeNonceBytes,
		"VOICE_CONTROL_PIPE_NONCE_CHARS":         VoiceControlPipeNonceChars,
	}
	if len(TSConstants) != len(want) {
		t.Errorf("TSConstants has %d entries, want %d", len(TSConstants), len(want))
	}
	for _, c := range TSConstants {
		if v, ok := want[c.Name]; !ok || v != c.Value {
			t.Errorf("TSConstants %s = %v, want %v", c.Name, c.Value, v)
		}
	}
}

func TestReactionErrorCodes(t *testing.T) {
	assertErrorCodes(t, map[string]string{
		ErrorCodeMessageNotFound:      "message_not_found",
		ErrorCodeInvalidReaction:      "invalid_reaction",
		ErrorCodeReactionNotPermitted: "reaction_not_permitted",
		ErrorCodeTooManyReactions:     "too_many_reactions",
		ErrorCodeReactionsRequireAuth: "reactions_require_auth",
	})
}

// C1: the fixtures round-trip byte for byte and validate.
func TestReactionGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"reaction-summary-v1.json", func() any { return new(ReactionSummaryV1) }},
		{"reaction-summary-v1-empty.json", func() any { return new(ReactionSummaryV1) }},
		{"reaction-summaries-response-v1.json", func() any { return new(ReactionSummariesResponseV1) }},
		{"reaction-summaries-response-v1-empty.json", func() any { return new(ReactionSummariesResponseV1) }},
		{"reaction-v1.json", func() any { return new(ReactionV1) }},
		{"reaction-v1-guest.json", func() any { return new(ReactionV1) }},
		{"list-reactors-response-v1.json", func() any { return new(ListReactorsResponseV1) }},
		{"list-reactors-response-v1-guest.json", func() any { return new(ListReactorsResponseV1) }},
		{"list-reactors-response-v1-empty.json", func() any { return new(ListReactorsResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
			raw, err := os.ReadFile(filepath.Join("testdata", tt.file)) // #nosec G304 -- fixture name comes from this test's own table
			if err != nil {
				t.Fatal(err)
			}
			value := tt.new()
			if err := json.Unmarshal(raw, value); err != nil {
				t.Fatal(err)
			}
			var validate func() error
			switch v := value.(type) {
			case *ReactionSummaryV1:
				validate = v.Validate
				if v.Reactions == nil {
					t.Error("reactions decoded as null, want an array")
				}
			case *ReactionSummariesResponseV1:
				validate = v.Validate
				if v.Summaries == nil {
					t.Error("summaries decoded as null, want an array")
				}
			case *ReactionV1:
				validate = v.Validate
			case *ListReactorsResponseV1:
				validate = v.Validate
				if v.Reactors == nil {
					t.Error("reactors decoded as null, want an array")
				}
			}
			if err := validate(); err != nil {
				t.Errorf("fixture does not validate: %v", err)
			}
		})
	}
}

// The guest fixtures carry the attribution a guest reacted under, and the
// summary fixture shows the three distinctions of §3.2 in one document: a
// bare emoji, one whose canonical form carries a selector, and a skin tone.
func TestReactionFixtureContents(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "reaction-summary-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s ReactionSummaryV1
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, r := range s.Reactions {
		keys = append(keys, r.Emoji)
	}
	want := []string{seq(0x1F44D), seq(0x2764, 0xFE0F), seq(0x1F44D, 0x1F3FD)}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("summary keys = %q, want %q", keys, want)
	}
	if !s.Reactions[0].Me || s.Reactions[1].Me {
		t.Error("me is true on the first count alone")
	}

	raw, err = os.ReadFile(filepath.Join("testdata", "list-reactors-response-v1-guest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var l ListReactorsResponseV1
	if err := json.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	last := l.Reactors[len(l.Reactors)-1]
	if last.GuestReactor == nil || last.GuestReactor.Label != "Nick" || last.GuestReactor.Tag != GuestTag(last.GuestReactor.Handle) {
		t.Errorf("guest reactor = %+v, want Nick with the handle's tag", last.GuestReactor)
	}
	if l.NextAfter != last.PrincipalID {
		t.Errorf("next_after = %d, want the last principal_id %d", l.NextAfter, last.PrincipalID)
	}
}

func goodSummary() ReactionSummaryV1 {
	return ReactionSummaryV1{
		Schema: ReactionSummarySchemaV1, ChannelID: 3, MessageID: 1042,
		Reactions: []ReactionCountV1{{Emoji: seq(0x1F44D), Count: 3, Me: true}, {Emoji: seq(0x2764, 0xFE0F), Count: 1}},
		AsOf:      NewTimestamp(time.Date(2026, 10, 28, 14, 3, 11, 204_000_000, time.UTC)),
	}
}

// manyKinds returns n distinct canonical keys from the catalog.
func manyKinds(n int) []ReactionCountV1 {
	all := EmojiCatalog()
	out := make([]ReactionCountV1, n)
	for i := range out {
		out[i] = ReactionCountV1{Emoji: all[i].Key, Count: 1}
	}
	return out
}

func TestReactionSummaryValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ReactionSummaryV1)
		wantErr string
	}{
		{name: "valid", mutate: func(*ReactionSummaryV1) {}},
		{name: "empty", mutate: func(s *ReactionSummaryV1) { s.Reactions = []ReactionCountV1{} }},
		// C6: a message holding more kinds than the write limit still
		// validates, up to the fixed wire bound.
		{name: "over the write limit, within the wire bound", mutate: func(s *ReactionSummaryV1) { s.Reactions = manyKinds(MaxReactionKindsPerMessage + 1) }},
		{name: "at the wire bound", mutate: func(s *ReactionSummaryV1) { s.Reactions = manyKinds(MaxReactionKindsOnWire) }},
		{name: "over the wire bound", mutate: func(s *ReactionSummaryV1) { s.Reactions = manyKinds(MaxReactionKindsOnWire + 1) }, wantErr: "holds 101 kinds, at most 100"},
		{name: "wrong schema", mutate: func(s *ReactionSummaryV1) { s.Schema = ReadStateSchemaV1 }, wantErr: "reaction summary schema must be"},
		{name: "no channel", mutate: func(s *ReactionSummaryV1) { s.ChannelID = 0 }, wantErr: "channel_id must be positive"},
		{name: "no message", mutate: func(s *ReactionSummaryV1) { s.MessageID = -1 }, wantErr: "message_id must be positive"},
		{name: "null reactions", mutate: func(s *ReactionSummaryV1) { s.Reactions = nil }, wantErr: "must be an array, not null"},
		{name: "a non-canonical key", mutate: func(s *ReactionSummaryV1) { s.Reactions[1].Emoji = seq(0x2764) }, wantErr: "not the canonical key"},
		{name: "a stray selector", mutate: func(s *ReactionSummaryV1) { s.Reactions[0].Emoji = seq(0x1F44D, 0xFE0F) }, wantErr: "not the canonical key"},
		{name: "not an emoji", mutate: func(s *ReactionSummaryV1) { s.Reactions[0].Emoji = ":+1:" }, wantErr: "not one emoji"},
		{name: "a count of zero", mutate: func(s *ReactionSummaryV1) { s.Reactions[0].Count = 0 }, wantErr: "count must be at least 1"},
		{name: "a key twice", mutate: func(s *ReactionSummaryV1) { s.Reactions[1].Emoji = s.Reactions[0].Emoji }, wantErr: "twice"},
		{name: "zero as_of", mutate: func(s *ReactionSummaryV1) { s.AsOf = Timestamp{} }, wantErr: "as_of is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := goodSummary()
			tt.mutate(&s)
			err := s.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				// C6, the other half: it encodes.
				if _, err := json.Marshal(s); err != nil {
					t.Fatalf("Marshal: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestReactionSummariesResponseValidate(t *testing.T) {
	second := goodSummary()
	second.MessageID = 1050
	tests := []struct {
		name    string
		body    ReactionSummariesResponseV1
		wantErr string
	}{
		{name: "valid", body: ReactionSummariesResponseV1{Summaries: []ReactionSummaryV1{goodSummary(), second}}},
		{name: "empty", body: ReactionSummariesResponseV1{Summaries: []ReactionSummaryV1{}}},
		{name: "null", body: ReactionSummariesResponseV1{}, wantErr: "must be an array, not null"},
		{name: "out of order", body: ReactionSummariesResponseV1{Summaries: []ReactionSummaryV1{second, goodSummary()}}, wantErr: "ascending message_id order"},
		{name: "a message twice", body: ReactionSummariesResponseV1{Summaries: []ReactionSummaryV1{goodSummary(), goodSummary()}}, wantErr: "ascending message_id order"},
		{name: "an invalid summary", body: ReactionSummariesResponseV1{Summaries: []ReactionSummaryV1{{Schema: ReactionSummarySchemaV1}}}, wantErr: "channel_id must be positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.body.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func goodReaction(id int64) ReactionV1 {
	return ReactionV1{PrincipalID: id, Emoji: seq(0x1F44D), CreatedAt: NewTimestamp(time.Date(2026, 10, 28, 13, 58, 2, 117_000_000, time.UTC))}
}

func TestReactionValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ReactionV1)
		wantErr string
	}{
		{name: "valid", mutate: func(*ReactionV1) {}},
		{name: "a guest", mutate: func(r *ReactionV1) {
			r.GuestReactor = &GuestAttributionV1{Handle: "guest-7h3kq2m9p4r8s6t1", Label: "Nick", Tag: "7h3k"}
		}},
		{name: "a guest without a label", mutate: func(r *ReactionV1) {
			r.GuestReactor = &GuestAttributionV1{Handle: "guest-7h3kq2m9p4r8s6t1", Tag: "7h3k"}
		}},
		{name: "no principal", mutate: func(r *ReactionV1) { r.PrincipalID = 0 }, wantErr: "principal_id must be positive"},
		{name: "a non-canonical key", mutate: func(r *ReactionV1) { r.Emoji = seq(0x1F44D, 0xFE0F) }, wantErr: "not the canonical key"},
		{name: "a component", mutate: func(r *ReactionV1) { r.Emoji = seq(0x1F3FD) }, wantErr: "not one emoji"},
		{name: "zero created_at", mutate: func(r *ReactionV1) { r.CreatedAt = Timestamp{} }, wantErr: "created_at is required"},
		{name: "a guest with the wrong tag", mutate: func(r *ReactionV1) {
			r.GuestReactor = &GuestAttributionV1{Handle: "guest-7h3kq2m9p4r8s6t1", Label: "Nick", Tag: "zzzz"}
		}, wantErr: "tag must be the handle's tag"},
		{name: "a guest with a bad handle", mutate: func(r *ReactionV1) {
			r.GuestReactor = &GuestAttributionV1{Handle: "nick", Tag: ""}
		}, wantErr: "malformed guest handle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := goodReaction(7)
			tt.mutate(&r)
			err := r.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestListReactorsResponseValidate(t *testing.T) {
	otherEmoji := goodReaction(12)
	otherEmoji.Emoji = seq(0x2764, 0xFE0F)
	tests := []struct {
		name    string
		body    ListReactorsResponseV1
		wantErr string
	}{
		{name: "valid, last page", body: ListReactorsResponseV1{Reactors: []ReactionV1{goodReaction(7), goodReaction(12)}}},
		{name: "valid, more pages", body: ListReactorsResponseV1{Reactors: []ReactionV1{goodReaction(7), goodReaction(12)}, NextAfter: 12}},
		{name: "empty", body: ListReactorsResponseV1{Reactors: []ReactionV1{}}},
		{name: "null", body: ListReactorsResponseV1{}, wantErr: "must be an array, not null"},
		{name: "out of order", body: ListReactorsResponseV1{Reactors: []ReactionV1{goodReaction(12), goodReaction(7)}}, wantErr: "ascending principal_id order"},
		{name: "a principal twice", body: ListReactorsResponseV1{Reactors: []ReactionV1{goodReaction(7), goodReaction(7)}}, wantErr: "ascending principal_id order"},
		{name: "two emoji", body: ListReactorsResponseV1{Reactors: []ReactionV1{goodReaction(7), otherEmoji}}, wantErr: "one emoji"},
		{name: "an invalid record", body: ListReactorsResponseV1{Reactors: []ReactionV1{goodReaction(0)}}, wantErr: "principal_id must be positive"},
		{name: "negative next_after", body: ListReactorsResponseV1{Reactors: []ReactionV1{goodReaction(7)}, NextAfter: -1}, wantErr: "must not be negative"},
		{name: "next_after on an empty page", body: ListReactorsResponseV1{Reactors: []ReactionV1{}, NextAfter: 7}, wantErr: "last principal_id of the page"},
		{name: "next_after that is not the last id", body: ListReactorsResponseV1{Reactors: []ReactionV1{goodReaction(7), goodReaction(12)}, NextAfter: 7}, wantErr: "last principal_id of the page"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.body.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// A summary encodes an empty list as [] and omits me when false, so a client
// never sees null or an explicit false.
func TestReactionSummaryEncoding(t *testing.T) {
	s := goodSummary()
	s.Reactions = []ReactionCountV1{{Emoji: seq(0x1F44D), Count: 1}}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), `"me"`) {
		t.Errorf("me false is encoded: %s", out)
	}
	s.Reactions = []ReactionCountV1{}
	out, err = json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"reactions":[]`) {
		t.Errorf("an empty list is not []: %s", out)
	}
}
