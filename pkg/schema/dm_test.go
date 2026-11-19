package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// dmValidator is any document of this package with a Validate method.
type dmValidator interface{ Validate() error }

// assertValidGoldenFixture round-trips the fixture byte for byte and then
// validates what it decoded.
func assertValidGoldenFixture(t *testing.T, file string, new func() dmValidator) {
	t.Helper()
	assertGoldenFixture(t, file, func() any { return new() })
	raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name comes from this test's own table
	if err != nil {
		t.Fatal(err)
	}
	v := new()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatal(err)
	}
	if err := v.Validate(); err != nil {
		t.Errorf("fixture does not validate: %v", err)
	}
}

func TestDMGoldenFixtures(t *testing.T) {
	tests := []struct {
		file string
		new  func() dmValidator
	}{
		{"dm-conversation-v1.json", func() dmValidator { return new(DMConversationV1) }},
		{"dm-conversation-v1-new.json", func() dmValidator { return new(DMConversationV1) }},
		{"dm-conversation-v1-archived-unavailable.json", func() dmValidator { return new(DMConversationV1) }},
		{"open-dm-request-v1.json", func() dmValidator { return new(OpenDMRequestV1) }},
		{"open-dm-response-v1.json", func() dmValidator { return &openDMResponseCheck{} }},
		{"open-dm-response-v1-resumed.json", func() dmValidator { return &openDMResponseCheck{} }},
		{"list-dm-conversations-response-v1.json", func() dmValidator { return new(ListDMConversationsResponseV1) }},
		{"list-dm-conversations-response-v1-empty.json", func() dmValidator { return new(ListDMConversationsResponseV1) }},
		{"dm-message-v1.json", func() dmValidator { return new(DMMessageV1) }},
		{"dm-message-v1-own-files.json", func() dmValidator { return new(DMMessageV1) }},
		{"dm-message-v1-edited.json", func() dmValidator { return new(DMMessageV1) }},
		{"dm-message-v1-removed.json", func() dmValidator { return new(DMMessageV1) }},
		{"edit-dm-message-request-v1.json", func() dmValidator { return new(EditDMMessageRequestV1) }},
		{"edit-dm-message-response-v1.json", func() dmValidator { return new(EditDMMessageResponseV1) }},
		{"dm-message-edited-v1.json", func() dmValidator { return new(DMMessageEditedV1) }},
		{"dm-message-removed-v1.json", func() dmValidator { return new(DMMessageRemovedV1) }},
		{"dm-server-settings-v1.json", func() dmValidator { return &dmServerSettingsCheck{} }},
		{"dm-server-settings-v1-default.json", func() dmValidator { return &dmServerSettingsCheck{} }},
		{"dm-permission-request-v1.json", func() dmValidator { return &dmPermissionReqCheck{} }},
		{"dm-permission-response-v1.json", func() dmValidator { return &dmPermissionRespCheck{} }},
		{"post-dm-message-request-v1.json", func() dmValidator { return new(PostDMMessageRequestV1) }},
		{"post-dm-message-request-v1-files.json", func() dmValidator { return new(PostDMMessageRequestV1) }},
		{"post-dm-message-response-v1.json", func() dmValidator { return &postDMResponseCheck{} }},
		{"post-dm-message-response-v1-replayed.json", func() dmValidator { return &postDMResponseCheck{} }},
		{"list-dm-messages-response-v1.json", func() dmValidator { return new(ListDMMessagesResponseV1) }},
		{"list-dm-messages-response-v1-catch-up.json", func() dmValidator { return new(ListDMMessagesResponseV1) }},
		{"list-dm-messages-response-v1-empty.json", func() dmValidator { return new(ListDMMessagesResponseV1) }},
		{"search-dm-messages-response-v1.json", func() dmValidator { return new(SearchDMMessagesResponseV1) }},
		{"dm-read-state-v1.json", func() dmValidator { return new(DMReadStateV1) }},
		{"dm-read-state-v1-never.json", func() dmValidator { return new(DMReadStateV1) }},
		{"set-dm-state-request-v1.json", func() dmValidator { return new(SetDMStateRequestV1) }},
		{"dm-preferences-v1.json", func() dmValidator { return new(DMPreferencesV1) }},
		{"dm-preferences-v1-default.json", func() dmValidator { return new(DMPreferencesV1) }},
		{"dm-file-v1.json", func() dmValidator { return new(DMFileV1) }},
		{"dm-changed-v1.json", func() dmValidator { return new(DMChangedV1) }},
		{"dm-snapshot-v1.json", func() dmValidator { return new(DMSnapshotV1) }},
		{"dm-snapshot-v1-empty.json", func() dmValidator { return new(DMSnapshotV1) }},
		{"notification-summary-v2-direct.json", func() dmValidator { return new(NotificationSummaryV2) }},
		{"notification-summary-v2-channel.json", func() dmValidator { return new(NotificationSummaryV2) }},
		{"list-pending-notifications-response-v2.json", func() dmValidator { return new(ListPendingNotificationsResponseV2) }},
		{"list-pending-notifications-response-v2-empty.json", func() dmValidator { return new(ListPendingNotificationsResponseV2) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) { assertValidGoldenFixture(t, tt.file, tt.new) })
	}
}

// The switch bodies promise only their shape; these give them a Validate for
// the fixture test, with the embedded struct's strict decoding.
type (
	dmServerSettingsCheck struct{ DMServerSettingsV1 }
	dmPermissionReqCheck  struct{ DMPermissionRequestV1 }
	dmPermissionRespCheck struct{ DMPermissionResponseV1 }
)

func (dmServerSettingsCheck) Validate() error { return nil }
func (dmPermissionReqCheck) Validate() error  { return nil }
func (dmPermissionRespCheck) Validate() error { return nil }

// openDMResponseCheck and postDMResponseCheck give the two response bodies,
// which promise only what they embed, a Validate for the fixture test.
type openDMResponseCheck struct{ OpenDMResponseV1 }

func (r openDMResponseCheck) Validate() error { return r.Conversation.Validate() }

type postDMResponseCheck struct{ PostDMMessageResponseV1 }

func (r postDMResponseCheck) Validate() error { return r.Message.Validate() }

// TestDMRequestsRefuseChannelOnlyFields pins ADR-017 Decision 5: a DM send
// or edit has no payload, audience, thread or mention field, so the server's
// strict decoding refuses a body that carries one (400 invalid_request).
func TestDMRequestsRefuseChannelOnlyFields(t *testing.T) {
	for _, field := range []string{"payload", "audience", "thread_root_id", "mentions", "channel_id"} {
		for name, into := range map[string]any{"send": new(PostDMMessageRequestV1), "edit": new(EditDMMessageRequestV1)} {
			body := `{"body":"hi","client_nonce":"n","` + field + `":1}`
			dec := json.NewDecoder(strings.NewReader(body))
			dec.DisallowUnknownFields()
			if err := dec.Decode(into); err == nil {
				t.Errorf("%s accepts %q", name, field)
			}
		}
	}
	// Blocking is #1832's, not the DM contract's: the preference has no list.
	dec := json.NewDecoder(strings.NewReader(`{"accept":"everyone","blocked":[1]}`))
	dec.DisallowUnknownFields()
	if err := dec.Decode(new(DMPreferencesV1)); err == nil {
		t.Error("DMPreferencesV1 accepts a block list")
	}
}

func TestDMDefaultPreferences(t *testing.T) {
	if err := DefaultDMPreferences().Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(DefaultDMPreferences())
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"accept":"everyone"}` {
		t.Fatalf("default preferences encode as %s", raw)
	}
}

func TestDMDefaultServerSettings(t *testing.T) {
	if !DefaultDMServerSettings().DMsEnabled {
		t.Fatal("direct messages must be on by default (ADR-017 Decision 9)")
	}
}

func TestDMErrorCodes(t *testing.T) {
	assertErrorCodes(t, map[string]string{
		ErrorCodeDMNotFound:          "dm_not_found",
		ErrorCodeContactUnavailable:  "contact_unavailable",
		ErrorCodeDMsOff:              "dms_off",
		ErrorCodeOwnSignInRequired:   "own_sign_in_required",
		ErrorCodeDMNonceConflict:     "dm_nonce_conflict",
		ErrorCodeDMRequireAuth:       "dm_require_auth",
		ErrorCodeDMFileNotAttachable: "dm_file_not_attachable",
	})
}

func TestDMValidate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	later := NewTimestamp(time.Date(2026, 11, 19, 9, 5, 0, 0, time.UTC))
	conv := func() DMConversationV1 {
		return DMConversationV1{
			Schema: DMConversationSchemaV1, ID: 12, ParticipantIDs: []int64{3, 5}, CreatedAt: at,
			PeerID: 5, PeerAvailable: true, State: DMStateOpen,
			LastMessageID: 40, LastMessageAt: &later, LastReadID: 30, Unread: 2,
		}
	}
	msg := func(id int64) DMMessageV1 {
		return DMMessageV1{Schema: DMMessageSchemaV1, ID: id, ConversationID: 12, AuthorID: 3, CreatedAt: at, Body: "hi"}
	}
	tests := []struct {
		name string
		doc  dmValidator
		ok   bool
	}{
		{"conversation", conv(), true},
		{"conversation wrong schema", func() DMConversationV1 { c := conv(); c.Schema = MessageSchemaV2; return c }(), false},
		{"conversation one participant", func() DMConversationV1 { c := conv(); c.ParticipantIDs = []int64{3}; return c }(), false},
		{"conversation three participants", func() DMConversationV1 { c := conv(); c.ParticipantIDs = []int64{3, 5, 7}; return c }(), false},
		{"conversation with oneself", func() DMConversationV1 { c := conv(); c.ParticipantIDs = []int64{5, 5}; return c }(), false},
		{"conversation pair not canonical", func() DMConversationV1 { c := conv(); c.ParticipantIDs = []int64{5, 3}; return c }(), false},
		{"conversation peer outside the pair", func() DMConversationV1 { c := conv(); c.PeerID = 9; return c }(), false},
		{"conversation unknown state", func() DMConversationV1 { c := conv(); c.State = "muted"; return c }(), false},
		{"conversation last_message_at without id", func() DMConversationV1 { c := conv(); c.LastMessageID, c.LastReadID = 0, 0; return c }(), false},
		{"conversation read past the newest", func() DMConversationV1 { c := conv(); c.LastReadID = 41; return c }(), false},
		{"conversation unread over cap", func() DMConversationV1 { c := conv(); c.Unread = MaxUnreadCount + 1; return c }(), false},

		{"message", msg(40), true},
		{"message with channel schema", func() DMMessageV1 { m := msg(40); m.Schema = MessageSchemaV2; return m }(), false},
		{"message no conversation", func() DMMessageV1 { m := msg(40); m.ConversationID = 0; return m }(), false},
		{"message no body", func() DMMessageV1 { m := msg(40); m.Body = ""; return m }(), false},
		{"message duplicate files", func() DMMessageV1 { m := msg(40); m.FileIDs = []int64{4, 4}; return m }(), false},
		{"message empty file list", func() DMMessageV1 { m := msg(40); m.FileIDs = []int64{}; return m }(), false},
		{"message edited", func() DMMessageV1 { m := msg(40); m.EditedAt = &later; return m }(), true},
		{"message edited before it was posted", func() DMMessageV1 { m := msg(40); m.EditedAt = &at; return m }(), false},
		{"message removed", func() DMMessageV1 { m := msg(40); m.Body, m.Removed = MessageRemovedByAuthorBody, true; return m }(), true},
		{"message removed with the moderator body", func() DMMessageV1 { m := msg(40); m.Body, m.Removed = MessageRemovedBody, true; return m }(), false},
		{"message removed with its text", func() DMMessageV1 { m := msg(40); m.Removed = true; return m }(), false},
		{"message removed with files", func() DMMessageV1 {
			m := msg(40)
			m.Body, m.Removed, m.FileIDs = MessageRemovedByAuthorBody, true, []int64{4}
			return m
		}(), false},
		{"message removed and edited", func() DMMessageV1 {
			m := msg(40)
			m.Body, m.Removed, m.EditedAt = MessageRemovedByAuthorBody, true, &later
			return m
		}(), false},
		{"message bad nonce", func() DMMessageV1 { m := msg(40); m.ClientNonce = "has space"; return m }(), false},

		{"post", PostDMMessageRequestV1{Body: "hi", ClientNonce: "abc_DEF-123"}, true},
		{"post without nonce", PostDMMessageRequestV1{Body: "hi"}, false},
		{"post nonce too long", PostDMMessageRequestV1{Body: "hi", ClientNonce: strings.Repeat("a", MaxDMClientNonceBytes+1)}, false},
		{"post nonce at the bound", PostDMMessageRequestV1{Body: "hi", ClientNonce: strings.Repeat("a", MaxDMClientNonceBytes)}, true},
		{"post too many files", PostDMMessageRequestV1{Body: "hi", ClientNonce: "n", FileIDs: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11}}, false},
		{"post no body", PostDMMessageRequestV1{ClientNonce: "n"}, false},
		{"post empty file list is none", PostDMMessageRequestV1{Body: "hi", ClientNonce: "n", FileIDs: []int64{}}, true},

		{"edit", EditDMMessageRequestV1{Body: "hi"}, true},
		{"edit blank", EditDMMessageRequestV1{Body: "  \n"}, false},
		{"edit response", EditDMMessageResponseV1{Message: func() DMMessageV1 { m := msg(40); m.EditedAt = &later; return m }()}, true},
		{"edit response without a message", EditDMMessageResponseV1{}, false},
		{"edited frame", DMMessageEditedV1{Schema: DMMessageEditedSchemaV1, Message: func() DMMessageV1 { m := msg(40); m.EditedAt = &later; return m }()}, true},
		{"edited frame never edited", DMMessageEditedV1{Schema: DMMessageEditedSchemaV1, Message: msg(40)}, false},
		{"edited frame wrong schema", DMMessageEditedV1{Schema: DMMessageRemovedSchemaV1, Message: func() DMMessageV1 { m := msg(40); m.EditedAt = &later; return m }()}, false},
		{"removed frame", DMMessageRemovedV1{Schema: DMMessageRemovedSchemaV1, Message: func() DMMessageV1 { m := msg(40); m.Body, m.Removed = MessageRemovedByAuthorBody, true; return m }()}, true},
		{"removed frame not removed", DMMessageRemovedV1{Schema: DMMessageRemovedSchemaV1, Message: msg(40)}, false},
		{"removed frame wrong schema", DMMessageRemovedV1{Schema: DMMessageEditedSchemaV1, Message: func() DMMessageV1 { m := msg(40); m.Body, m.Removed = MessageRemovedByAuthorBody, true; return m }()}, false},

		{"open", OpenDMRequestV1{PeerID: 5}, true},
		{"open no peer", OpenDMRequestV1{}, false},

		{"history newest page", ListDMMessagesResponseV1{Messages: []DMMessageV1{msg(30), msg(40)}, NextBefore: 30}, true},
		{"history null", ListDMMessagesResponseV1{}, false},
		{"history descending", ListDMMessagesResponseV1{Messages: []DMMessageV1{msg(40), msg(30)}}, false},
		{"history two cursors", ListDMMessagesResponseV1{Messages: []DMMessageV1{msg(30), msg(40)}, NextBefore: 30, NextAfter: 40}, false},
		{"history cursor off the edge", ListDMMessagesResponseV1{Messages: []DMMessageV1{msg(30), msg(40)}, NextBefore: 29}, false},
		{"history cursor on empty page", ListDMMessagesResponseV1{Messages: []DMMessageV1{}, NextAfter: 40}, false},
		{"history two conversations", ListDMMessagesResponseV1{Messages: []DMMessageV1{msg(30), func() DMMessageV1 { m := msg(40); m.ConversationID = 13; return m }()}}, false},
		{"history over the page", ListDMMessagesResponseV1{Messages: make([]DMMessageV1, MaxDMPageSize+1)}, false},

		{"search", SearchDMMessagesResponseV1{Messages: []DMMessageV1{msg(40), msg(30)}, NextBefore: 30}, true},
		{"search ascending", SearchDMMessagesResponseV1{Messages: []DMMessageV1{msg(30), msg(40)}}, false},
		{"search cursor not the last", SearchDMMessagesResponseV1{Messages: []DMMessageV1{msg(40), msg(30)}, NextBefore: 40}, false},

		{"list", ListDMConversationsResponseV1{Conversations: []DMConversationV1{conv()}, NextCursor: "abc"}, true},
		{"list null", ListDMConversationsResponseV1{}, false},
		{"list cursor not URL-safe", ListDMConversationsResponseV1{Conversations: []DMConversationV1{}, NextCursor: "a/b"}, false},
		{"list cursor too long", ListDMConversationsResponseV1{Conversations: []DMConversationV1{}, NextCursor: strings.Repeat("a", MaxDMCursorBytes+1)}, false},

		{"read state", DMReadStateV1{Schema: DMReadStateSchemaV1, ConversationID: 12, LastReadID: 40, UpdatedAt: &at}, true},
		{"read state channel schema", DMReadStateV1{Schema: ReadStateSchemaV1, ConversationID: 12}, false},
		{"read state never marked with updated_at", DMReadStateV1{Schema: DMReadStateSchemaV1, ConversationID: 12, UpdatedAt: &at}, false},

		{"state", SetDMStateRequestV1{State: DMStateHidden}, true},
		{"state unknown", SetDMStateRequestV1{State: "deleted"}, false},

		{"preferences", DMPreferencesV1{Accept: DMAcceptNobody}, true},
		{"preferences unknown policy", DMPreferencesV1{Accept: "friends"}, false},
		{"preferences empty", DMPreferencesV1{}, false},

		{"file no conversation", DMFileV1{Schema: DMFileSchemaV1, ID: 1, UploaderID: 3, Filename: "a", MediaType: "text/plain", SHA256: strings.Repeat("a", 64), CreatedAt: at}, false},
		{"file bad name", DMFileV1{Schema: DMFileSchemaV1, ID: 1, ConversationID: 12, UploaderID: 3, Filename: "../a", MediaType: "text/plain", SHA256: strings.Repeat("a", 64), CreatedAt: at}, false},
		{"file channel schema", DMFileV1{Schema: FileSchemaV1, ID: 1, ConversationID: 12, UploaderID: 3, Filename: "a", MediaType: "text/plain", SHA256: strings.Repeat("a", 64), CreatedAt: at}, false},

		{"changed unknown kind tolerated", DMChangedV1{Schema: DMChangedSchemaV1, ConversationID: 12, Kind: "later"}, true},
		{"changed no kind", DMChangedV1{Schema: DMChangedSchemaV1, ConversationID: 12}, false},
		{"changed no conversation", DMChangedV1{Schema: DMChangedSchemaV1, Kind: DMChangeRead}, false},

		{"snapshot", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{conv()}, HighWater: 40, TakenAt: at}, true},
		{"snapshot below a conversation", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{conv()}, HighWater: 39, TakenAt: at}, false},
		{"snapshot archived", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{func() DMConversationV1 { c := conv(); c.State = DMStateArchived; return c }()}, HighWater: 40, TakenAt: at}, false},
		{"snapshot twice", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{conv(), conv()}, HighWater: 40, TakenAt: at}, false},
		{"snapshot null", DMSnapshotV1{Schema: DMSnapshotSchemaV1, TakenAt: at}, false},
		{"snapshot no time", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.doc.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
		})
	}
}

func TestNotificationSummaryV2Validate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	direct := MessageContext{Kind: MessageContextDirect, ConversationID: 12}
	channel := MessageContext{Kind: MessageContextChannel, ChannelID: 7, ChannelName: "general"}
	sum := func(c MessageContext, k NotificationKind) NotificationSummaryV2 {
		return NotificationSummaryV2{Schema: NotificationSummarySchemaV2, Context: c, MessageID: 9, PostedAt: at, Kind: k, AuthorID: 3}
	}
	tests := []struct {
		name string
		doc  dmValidator
		ok   bool
	}{
		{"direct", sum(direct, NotificationKindDirect), true},
		{"channel mention", sum(channel, NotificationKindMention), true},
		{"direct context, channel kind", sum(direct, NotificationKindWhisper), false},
		{"channel context, direct kind", sum(channel, NotificationKindDirect), false},
		{"v1 schema", func() NotificationSummaryV2 {
			s := sum(direct, NotificationKindDirect)
			s.Schema = NotificationSummarySchemaV1
			return s
		}(), false},
		{"direct with a channel id", sum(MessageContext{Kind: MessageContextDirect, ConversationID: 12, ChannelID: 7}, NotificationKindDirect), false},
		{"channel with a conversation id", sum(MessageContext{Kind: MessageContextChannel, ChannelID: 7, ChannelName: "g", ConversationID: 12}, NotificationKindMention), false},
		{"channel without a name", sum(MessageContext{Kind: MessageContextChannel, ChannelID: 7}, NotificationKindMention), false},
		{"unknown context", sum(MessageContext{Kind: "group", ConversationID: 12}, NotificationKindDirect), false},
		{"text too long", func() NotificationSummaryV2 {
			s := sum(direct, NotificationKindDirect)
			s.Text = strings.Repeat("x", MaxNotificationTextChars+1)
			return s
		}(), false},
		{"page", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{sum(direct, NotificationKindDirect)}, Cursor: "c_9"}, true},
		// Channel message 9 and DM message 9 are different messages: ids are
		// separate spaces, so the same number in two contexts is no repeat.
		{"page same id in two contexts", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{sum(channel, NotificationKindMention), sum(direct, NotificationKindDirect)}, Cursor: "c_9"}, true},
		{"page repeats a message", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{sum(direct, NotificationKindDirect), sum(direct, NotificationKindDirect)}, Cursor: "c_9"}, false},
		{"page out of time order", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{
			func() NotificationSummaryV2 {
				s := sum(direct, NotificationKindDirect)
				s.PostedAt = NewTimestamp(at.Time().Add(time.Minute))
				return s
			}(), sum(channel, NotificationKindMention)}, Cursor: "c_9"}, false},
		{"page cursor not URL-safe", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}, Cursor: "a/b"}, false},
		{"page cursor too long", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}, Cursor: strings.Repeat("a", MaxDMCursorBytes+1)}, false},
		{"page empty, no cursor", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}}, true},
		{"page null", ListPendingNotificationsResponseV2{}, false},
		{"page more without a cursor", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}, More: true}, false},
		// v1 never carries a DM: its kind "direct" is refused there.
		{"v1 refuses direct", NotificationSummaryV1{
			Schema: NotificationSummarySchemaV1, ChannelID: 7, ChannelName: "general", MessageID: 9,
			PostedAt: at, Kind: NotificationKindDirect, AuthorID: 3,
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.doc.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("Validate() = nil, want an error")
			}
		})
	}
}

// hasKey reports whether the JSON document v holds key at any depth.
func hasKey(v any, key string) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == key || hasKey(e, key) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasKey(e, key) {
				return true
			}
		}
	}
	return false
}

// TestDMAndChannelIdentitiesStayApart pins the acceptance criterion that a
// channel id and a conversation id are never in the same envelope: no DM
// fixture carries channel_id, no channel fixture carries conversation_id,
// and the one family that holds both kinds (notification summary v2) holds
// each only inside its context. A DM message is not a channel message to a
// strict decoder either.
func TestDMAndChannelIdentitiesStayApart(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	var dm, channel int
	for _, file := range files {
		base := filepath.Base(file)
		raw, err := os.ReadFile(file) // #nosec G304 -- a fixture path this test globbed from its own testdata
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue // a fixture that is not one JSON document is another test's
		}
		isDM := strings.Contains(base, "dm-")
		switch {
		case isDM:
			dm++
			if hasKey(doc, "channel_id") || hasKey(doc, "channel_name") {
				t.Errorf("%s is a DM fixture and carries a channel", base)
			}
		case strings.Contains(base, "notification-summary-v2") || strings.Contains(base, "pending-notifications-response-v2"):
			// Checked by context below.
		default:
			if hasKey(doc, "conversation_id") {
				t.Errorf("%s is a channel fixture and carries conversation_id", base)
			}
			channel++
		}
	}
	if dm == 0 || channel == 0 {
		t.Fatalf("found %d DM and %d channel fixtures; the glob is wrong", dm, channel)
	}

	for _, file := range []string{"dm-message-v1.json", "dm-message-v1-own-files.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name from this test's own list
		if err != nil {
			t.Fatal(err)
		}
		var m MessageV2
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err == nil && m.Validate() == nil {
			t.Errorf("%s reads as a channel message", file)
		}
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "message-v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d DMMessageV1
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.Validate() == nil {
		t.Error("message-v2.json reads as a DM message")
	}
}

// TestChannelFixturesUnchangedByDMs checks the published channel-side
// documents DMs sit beside still decode and validate as they did: the v1
// pending feed, the channel read mark and file, and the channel message.
func TestChannelFixturesUnchangedByDMs(t *testing.T) {
	tests := []struct {
		file string
		new  func() dmValidator
	}{
		{"notification-summary-v1.json", func() dmValidator { return new(NotificationSummaryV1) }},
		{"notification-summary-v1-preview.json", func() dmValidator { return new(NotificationSummaryV1) }},
		{"notification-summary-v1-unnamed.json", func() dmValidator { return new(NotificationSummaryV1) }},
		{"list-pending-notifications-response-v1.json", func() dmValidator { return new(ListPendingNotificationsResponseV1) }},
		{"read-state-v1.json", func() dmValidator { return new(ReadStateV1) }},
		{"file-v1.json", func() dmValidator { return new(FileV1) }},
		{"message-v2.json", func() dmValidator { return new(MessageV2) }},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) { assertValidGoldenFixture(t, tt.file, tt.new) })
	}
}
