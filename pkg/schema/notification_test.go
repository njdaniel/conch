package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNotificationGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() any
	}{
		{"notification-preferences-v1.json", func() any { return new(NotificationPreferencesV1) }},
		{"notification-preferences-v1-default.json", func() any { return new(NotificationPreferencesV1) }},
		{"put-notification-preferences-request-v1.json", func() any { return new(PutNotificationPreferencesRequestV1) }},
		{"notification-summary-v1.json", func() any { return new(NotificationSummaryV1) }},
		{"notification-summary-v1-preview.json", func() any { return new(NotificationSummaryV1) }},
		{"notification-summary-v1-unnamed.json", func() any { return new(NotificationSummaryV1) }},
		{"list-pending-notifications-response-v1.json", func() any { return new(ListPendingNotificationsResponseV1) }},
		{"list-pending-notifications-response-v1-empty.json", func() any { return new(ListPendingNotificationsResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestNotificationFixturesValidate(t *testing.T) {
	tests := []struct {
		file string
		v    validator
	}{
		{"notification-preferences-v1.json", new(NotificationPreferencesV1)},
		{"notification-preferences-v1-default.json", new(NotificationPreferencesV1)},
		{"put-notification-preferences-request-v1.json", new(PutNotificationPreferencesRequestV1)},
		{"notification-summary-v1.json", new(NotificationSummaryV1)},
		{"notification-summary-v1-preview.json", new(NotificationSummaryV1)},
		{"notification-summary-v1-unnamed.json", new(NotificationSummaryV1)},
		{"list-pending-notifications-response-v1.json", new(ListPendingNotificationsResponseV1)},
		{"list-pending-notifications-response-v1-empty.json", new(ListPendingNotificationsResponseV1)},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			assertValidFixture(t, tt.file, tt.v)
		})
	}
}

func TestDefaultNotificationPreferences(t *testing.T) {
	got := DefaultNotificationPreferences()
	want := NotificationPreferencesV1{Policy: NotificationPolicyOff, Muted: []int64{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultNotificationPreferences() = %+v, want %+v", got, want)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the default does not validate: %v", err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"policy":"off","muted":[],"preview":false,"quiet_when_unreachable":false}`; string(raw) != want {
		t.Errorf("the default encodes as %s, want %s", raw, want)
	}
	// Each call has its own list: appending to one default changes no other.
	got.Muted = append(got.Muted, 7)
	if again := DefaultNotificationPreferences(); len(again.Muted) != 0 {
		t.Errorf("a second default has muted %v", again.Muted)
	}
}

func TestNotificationPreferencesValidate(t *testing.T) {
	full := make([]int64, MaxMutedChannels)
	for i := range full {
		full[i] = int64(i + 1)
	}
	tests := []struct {
		name  string
		req   PutNotificationPreferencesRequestV1
		field string
	}{
		{"off, nothing muted", PutNotificationPreferencesRequestV1{Policy: NotificationPolicyOff, Muted: []int64{}}, ""},
		{"mentions, with mutes and both switches", PutNotificationPreferencesRequestV1{Policy: NotificationPolicyMentions, Muted: []int64{7, 12}, Preview: true, QuietWhenUnreachable: true}, ""},
		{"all", PutNotificationPreferencesRequestV1{Policy: NotificationPolicyAll, Muted: []int64{}}, ""},
		{"the same channel twice", PutNotificationPreferencesRequestV1{Policy: NotificationPolicyAll, Muted: []int64{7, 7}}, ""},
		{"as many mutes as allowed", PutNotificationPreferencesRequestV1{Policy: NotificationPolicyAll, Muted: full}, ""},
		{"no policy", PutNotificationPreferencesRequestV1{Muted: []int64{}}, "policy is required"},
		{"an unknown policy", PutNotificationPreferencesRequestV1{Policy: "some", Muted: []int64{}}, "policy"},
		{"a policy in another case", PutNotificationPreferencesRequestV1{Policy: "All", Muted: []int64{}}, "policy"},
		{"no muted list", PutNotificationPreferencesRequestV1{Policy: NotificationPolicyAll}, "muted must be a list"},
		{"one mute too many", PutNotificationPreferencesRequestV1{Policy: NotificationPolicyAll, Muted: append(append([]int64{}, full...), 300)}, "at most 256"},
		{"a zero id", PutNotificationPreferencesRequestV1{Policy: NotificationPolicyAll, Muted: []int64{7, 0}}, "muted[1]"},
		{"a negative id", PutNotificationPreferencesRequestV1{Policy: NotificationPolicyAll, Muted: []int64{-7}}, "muted[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefusal(t, tt.req.Validate(), tt.field)
			// The stored object has the same fields and the same rules.
			wantRefusal(t, NotificationPreferencesV1(tt.req).Validate(), tt.field)
		})
	}
}

// A body that leaves muted out, or sends null, is refused: the PUT replaces
// the whole object, and a missing list is not an empty one.
func TestPutNotificationPreferencesRequestDecodes(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		field string
	}{
		{"the whole object", `{"policy":"mentions","muted":[7],"preview":true,"quiet_when_unreachable":false}`, ""},
		{"the switches left out", `{"policy":"mentions","muted":[]}`, ""},
		{"muted left out", `{"policy":"mentions"}`, "muted"},
		{"muted null", `{"policy":"mentions","muted":null}`, "muted"},
		{"policy left out", `{"muted":[]}`, "policy"},
		{"an empty object", `{}`, "policy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req PutNotificationPreferencesRequestV1
			if err := json.Unmarshal([]byte(tt.body), &req); err != nil {
				t.Fatal(err)
			}
			wantRefusal(t, req.Validate(), tt.field)
		})
	}
}

func validSummary(id int64) NotificationSummaryV1 {
	return NotificationSummaryV1{
		Schema:      NotificationSummarySchemaV1,
		ChannelID:   7,
		ChannelName: "general",
		MessageID:   id,
		PostedAt:    NewTimestamp(time.Date(2026, 11, 10, 12, 30, 14, 0, time.UTC)),
		Kind:        NotificationKindMention,
		AuthorID:    3,
		AuthorName:  "Ada",
	}
}

func TestNotificationSummaryV1Validate(t *testing.T) {
	edit := func(f func(*NotificationSummaryV1)) NotificationSummaryV1 {
		s := validSummary(4211)
		f(&s)
		return s
	}
	// A character outside the basic plane is one character, four bytes.
	crab := string(rune(0x1f980))
	tests := []struct {
		name    string
		summary NotificationSummaryV1
		field   string
	}{
		{"a mention, no text", validSummary(4211), ""},
		{"a message with text", edit(func(s *NotificationSummaryV1) { s.Kind, s.Text = NotificationKindMessage, "hello" }), ""},
		{"a whisper whose author is not named", edit(func(s *NotificationSummaryV1) { s.Kind, s.AuthorName = NotificationKindWhisper, "" }), ""},
		{"text at the bound", edit(func(s *NotificationSummaryV1) { s.Text = strings.Repeat("a", MaxNotificationTextChars) }), ""},
		{"text at the bound, counted in characters", edit(func(s *NotificationSummaryV1) { s.Text = strings.Repeat(crab, MaxNotificationTextChars) }), ""},
		{"no schema", edit(func(s *NotificationSummaryV1) { s.Schema = "" }), "schema"},
		{"another schema", edit(func(s *NotificationSummaryV1) { s.Schema = "conch.notification_summary.v2" }), "schema"},
		{"a wake's schema", edit(func(s *NotificationSummaryV1) { s.Schema = PushWakeSchemaV1 }), "schema"},
		{"no channel id", edit(func(s *NotificationSummaryV1) { s.ChannelID = 0 }), "channel_id"},
		{"a negative channel id", edit(func(s *NotificationSummaryV1) { s.ChannelID = -7 }), "channel_id"},
		{"no channel name", edit(func(s *NotificationSummaryV1) { s.ChannelName = "" }), "channel_name"},
		{"no message id", edit(func(s *NotificationSummaryV1) { s.MessageID = 0 }), "message_id"},
		{"no time", edit(func(s *NotificationSummaryV1) { s.PostedAt = Timestamp{} }), "posted_at"},
		{"no kind", edit(func(s *NotificationSummaryV1) { s.Kind = "" }), "kind"},
		{"an unknown kind", edit(func(s *NotificationSummaryV1) { s.Kind = "reaction" }), "kind"},
		{"no author id", edit(func(s *NotificationSummaryV1) { s.AuthorID = 0 }), "author_id"},
		{"text past the bound", edit(func(s *NotificationSummaryV1) { s.Text = strings.Repeat("a", MaxNotificationTextChars+1) }), "text"},
		{"text past the bound, counted in characters", edit(func(s *NotificationSummaryV1) { s.Text = strings.Repeat(crab, MaxNotificationTextChars+1) }), "text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefusal(t, tt.summary.Validate(), tt.field)
		})
	}
}

// TestNotificationSummaryV1Keys pins what a summary can carry: text and the
// author's name are each omitted when the server withholds them.
func TestNotificationSummaryV1Keys(t *testing.T) {
	full := validSummary(4211)
	full.Text = "hello"
	if got, want := jsonKeys(t, full), "author_id,author_name,channel_id,channel_name,kind,message_id,posted_at,schema,text"; got != want {
		t.Errorf("NotificationSummaryV1 keys = %s, want %s", got, want)
	}
	bare := validSummary(4211)
	bare.AuthorName = ""
	if got, want := jsonKeys(t, bare), "author_id,channel_id,channel_name,kind,message_id,posted_at,schema"; got != want {
		t.Errorf("a summary without a name or text has keys %s, want %s", got, want)
	}
}

func TestListPendingNotificationsResponseV1Validate(t *testing.T) {
	page := func(ids ...int64) []NotificationSummaryV1 {
		out := make([]NotificationSummaryV1, 0, len(ids))
		for _, id := range ids {
			out = append(out, validSummary(id))
		}
		return out
	}
	fullIDs := make([]int64, MaxPendingNotificationsPage)
	for i := range fullIDs {
		fullIDs[i] = int64(100 + i)
	}
	bad := page(5, 6)
	bad[1].Kind = "reaction"
	tests := []struct {
		name  string
		resp  ListPendingNotificationsResponseV1
		field string
	}{
		{"nothing new, nothing examined: the cursor is after", ListPendingNotificationsResponseV1{Summaries: page(), Cursor: 0}, ""},
		{"nothing matched, the cursor past what was examined", ListPendingNotificationsResponseV1{Summaries: page(), Cursor: 6230, More: true}, ""},
		{"a page, the cursor its last id", ListPendingNotificationsResponseV1{Summaries: page(5, 9, 12), Cursor: 12}, ""},
		{"a page, the cursor above its last id", ListPendingNotificationsResponseV1{Summaries: page(5, 9, 12), Cursor: 40}, ""},
		{"a full page with more", ListPendingNotificationsResponseV1{Summaries: page(fullIDs...), Cursor: 119, More: true}, ""},
		{"null summaries", ListPendingNotificationsResponseV1{Cursor: 3}, "summaries must be a list"},
		{"a page past the bound", ListPendingNotificationsResponseV1{Summaries: page(append(fullIDs, 120)...), Cursor: 120}, "at most 20"},
		{"a negative cursor", ListPendingNotificationsResponseV1{Summaries: page(), Cursor: -1}, "cursor"},
		{"a cursor below the last summary", ListPendingNotificationsResponseV1{Summaries: page(5, 9, 12), Cursor: 11}, "cursor"},
		{"descending summaries", ListPendingNotificationsResponseV1{Summaries: page(9, 5), Cursor: 9}, "summaries[1]"},
		{"the same message twice", ListPendingNotificationsResponseV1{Summaries: page(5, 5), Cursor: 5}, "summaries[1]"},
		{"a malformed summary", ListPendingNotificationsResponseV1{Summaries: bad, Cursor: 6}, "summaries[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefusal(t, tt.resp.Validate(), tt.field)
		})
	}
	raw, err := json.Marshal(ListPendingNotificationsResponseV1{Summaries: []NotificationSummaryV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"summaries":[],"cursor":0,"more":false}` {
		t.Errorf("an empty page encodes as %s", raw)
	}
}

func TestNotificationConstants(t *testing.T) {
	if NotificationSummarySchemaV1 != "conch.notification_summary.v1" {
		t.Errorf("NotificationSummarySchemaV1 = %q", NotificationSummarySchemaV1)
	}
	bounds := []struct {
		name      string
		got, want int
	}{
		{"MaxMutedChannels", MaxMutedChannels, 256},
		{"MaxPendingNotificationsPage", MaxPendingNotificationsPage, 20},
		{"MaxNotificationTextChars", MaxNotificationTextChars, 160},
	}
	for _, b := range bounds {
		if b.got != b.want {
			t.Errorf("%s = %d, want %d", b.name, b.got, b.want)
		}
	}
}
