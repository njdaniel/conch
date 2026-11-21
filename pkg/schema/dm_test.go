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
		{"dm-conversation-v1-archived.json", func() dmValidator { return new(DMConversationV1) }},
		{"open-dm-request-v1.json", func() dmValidator { return new(OpenDMRequestV1) }},
		{"open-dm-response-v1.json", func() dmValidator { return new(OpenDMResponseV1) }},
		{"open-dm-response-v1-resumed.json", func() dmValidator { return new(OpenDMResponseV1) }},
		{"open-dm-response-v1-peer-open.json", func() dmValidator { return new(OpenDMResponseV1) }},
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
		{"post-dm-message-request-v1-nonce-conflict.json", func() dmValidator { return new(PostDMMessageRequestV1) }},
		{"post-dm-message-response-v1.json", func() dmValidator { return new(PostDMMessageResponseV1) }},
		{"post-dm-message-response-v1-replayed.json", func() dmValidator { return new(PostDMMessageResponseV1) }},
		{"post-dm-message-response-v1-replayed-edited.json", func() dmValidator { return new(PostDMMessageResponseV1) }},
		{"post-dm-message-response-v1-replayed-removed.json", func() dmValidator { return new(PostDMMessageResponseV1) }},
		{"list-dm-messages-response-v1.json", func() dmValidator { return new(ListDMMessagesResponseV1) }},
		{"list-dm-messages-response-v1-catch-up.json", func() dmValidator { return new(ListDMMessagesResponseV1) }},
		{"list-dm-messages-response-v1-empty.json", func() dmValidator { return new(ListDMMessagesResponseV1) }},
		{"list-dm-changes-response-v1.json", func() dmValidator { return new(ListDMChangesResponseV1) }},
		{"list-dm-changes-response-v1-empty.json", func() dmValidator { return new(ListDMChangesResponseV1) }},
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
		{"list-pending-notifications-response-v2-empty-more.json", func() dmValidator { return new(ListPendingNotificationsResponseV2) }},
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

// strictDMDecode decodes body into into with unknown fields refused, as the
// server does.
func strictDMDecode(body string, into any) error {
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

// TestDMRequestsRefuseChannelOnlyFields pins ADR-017 Decision 5: a DM send
// or edit has no payload, audience, thread or mention field, so the server's
// strict decoding refuses a body that carries one (400 invalid_request). Each
// case adds one forbidden field to an otherwise valid body, and the valid body
// is decoded first, so a refusal is the field's doing alone; a field the
// request type began to accept would fail its own case.
func TestDMRequestsRefuseChannelOnlyFields(t *testing.T) {
	requests := []struct {
		name      string
		base      string
		new       func() any
		forbidden []string
	}{
		{"send", `"body":"hi","client_nonce":"n"`, func() any { return new(PostDMMessageRequestV1) },
			[]string{"payload", "audience", "thread_root_id", "mentions", "channel_id", "conversation_id", "author_id", "reactions"}},
		// An edit takes the text and nothing else: not files, and not a
		// client nonce, which belongs to a send.
		{"edit", `"body":"hi"`, func() any { return new(EditDMMessageRequestV1) },
			[]string{"payload", "audience", "thread_root_id", "mentions", "channel_id", "conversation_id", "author_id", "file_ids", "client_nonce"}},
		{"open", `"peer_id":5`, func() any { return new(OpenDMRequestV1) },
			[]string{"payload", "channel_id", "conversation_id", "created", "body"}},
	}
	for _, rq := range requests {
		if err := strictDMDecode("{"+rq.base+"}", rq.new()); err != nil {
			t.Fatalf("%s: the base body does not decode: %v", rq.name, err)
		}
		for _, field := range rq.forbidden {
			t.Run(rq.name+"/"+field, func(t *testing.T) {
				if err := strictDMDecode("{"+rq.base+`,"`+field+`":1}`, rq.new()); err == nil {
					t.Errorf("%s accepts %q", rq.name, field)
				}
			})
		}
	}
	// Blocking is #1832's, not the DM contract's: the preference has no list.
	if err := strictDMDecode(`{"accept":"everyone","blocked":[1]}`, new(DMPreferencesV1)); err == nil {
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

// TestDMRefusalBodiesAreGeneric pins the privacy contract of the refusals
// (design §3.2): each DM refusal's body is exactly DMRefusalError(code), a
// fixed message with no reason in it, and the same bytes as its fixture. A
// message that said "blocked", named a preference, a switch or an account
// state, or took the input, would fail here.
func TestDMRefusalBodiesAreGeneric(t *testing.T) {
	fixtures := map[string]string{
		ErrorCodeDMNotFound:          "error-dm-not-found.json",
		ErrorCodeContactUnavailable:  "error-contact-unavailable.json",
		ErrorCodeDMsOff:              "error-dms-off.json",
		ErrorCodeOwnSignInRequired:   "error-own-sign-in-required.json",
		ErrorCodeDMNonceConflict:     "error-dm-nonce-conflict.json",
		ErrorCodeDMRequireAuth:       "error-dm-require-auth.json",
		ErrorCodeDMFileNotAttachable: "error-dm-file-not-attachable.json",
	}
	// What a refusal must never say: why a contact is refused, or who did it.
	reasons := []string{"block", "prefer", "disabled", "switch", "operator", "nobody", "hidden", "guest", "visible", "banned", "removed", "offline", "busy", "act-as", "impersonat", "exist"}
	seen := map[string]string{}
	for code, file := range fixtures {
		t.Run(code, func(t *testing.T) {
			want, ok := DMRefusalError(code)
			if !ok {
				t.Fatalf("DMRefusalError(%q) is not a DM refusal", code)
			}
			if want.Code != code || want.Message == "" || want.RetryAfterSeconds != 0 || want.Permission != "" {
				t.Errorf("DMRefusalError(%q) = %+v", code, want)
			}
			// Bodies are byte-identical to the fixture, so a changed message
			// changes a reviewed file.
			raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name from this test's own table
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.MarshalIndent(want, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != string(got)+"\n" {
				t.Errorf("fixture %s is not the pinned body\ngot:\n%s\nwant:\n%s", file, raw, got)
			}
			assertGoldenFixture(t, file, func() any { return new(Error) })
			for _, w := range reasons {
				// "exist" is only forbidden of the contact refusals: dm_not_found
				// must say one thing for "none" and "not yours".
				if strings.Contains(strings.ToLower(want.Message), w) && (w != "exist" || code != ErrorCodeDMNotFound) {
					t.Errorf("%s message %q names a reason (%q)", code, want.Message, w)
				}
			}
			if other, dup := seen[want.Message]; dup {
				t.Errorf("%s and %s share a message", code, other)
			}
			seen[want.Message] = code
		})
	}
	if _, ok := DMRefusalError("principal_not_found"); ok {
		t.Error("DMRefusalError claims a code that is not a DM refusal")
	}
}

func TestDMValidate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	later := NewTimestamp(time.Date(2026, 11, 19, 9, 5, 0, 0, time.UTC))
	conv := func() DMConversationV1 {
		return DMConversationV1{
			Schema: DMConversationSchemaV1, ID: 12, ParticipantIDs: []int64{3, 5}, CreatedAt: &at,
			PeerID: 5, State: DMStateOpen, Revision: 7,
			LastMessageID: 40, LastMessageAt: &later, LastReadID: 30, Unread: 2,
		}
	}
	msg := func(id int64) DMMessageV1 {
		return DMMessageV1{Schema: DMMessageSchemaV1, ID: id, ConversationID: 12, AuthorID: 3, CreatedAt: at, Revision: id, Body: "hi"}
	}
	empty := func() DMConversationV1 {
		c := conv()
		c.CreatedAt, c.LastMessageID, c.LastMessageAt, c.LastReadID, c.Unread, c.Revision = nil, 0, nil, 0, 0, 0
		return c
	}
	sent := msg(40)
	sent.ClientNonce = "n"
	edited := sent
	edited.EditedAt = &later
	removed := sent
	removed.Body, removed.Removed = MessageRemovedByAuthorBody, true
	tests := []struct {
		name string
		doc  dmValidator
		ok   bool
	}{
		{"conversation", conv(), true},
		{"conversation largest safe id", func() DMConversationV1 { c := conv(); c.ID = 1<<53 - 1; return c }(), true},
		{"conversation unsafe id", func() DMConversationV1 { c := conv(); c.ID = 1 << 53; return c }(), false},
		{"conversation wrong schema", func() DMConversationV1 { c := conv(); c.Schema = MessageSchemaV2; return c }(), false},
		{"conversation one participant", func() DMConversationV1 { c := conv(); c.ParticipantIDs = []int64{3}; return c }(), false},
		{"conversation three participants", func() DMConversationV1 { c := conv(); c.ParticipantIDs = []int64{3, 5, 7}; return c }(), false},
		{"conversation with oneself", func() DMConversationV1 { c := conv(); c.ParticipantIDs = []int64{5, 5}; return c }(), false},
		{"conversation pair not canonical", func() DMConversationV1 { c := conv(); c.ParticipantIDs = []int64{5, 3}; return c }(), false},
		{"conversation peer outside the pair", func() DMConversationV1 { c := conv(); c.PeerID = 9; return c }(), false},
		{"conversation unknown state", func() DMConversationV1 { c := conv(); c.State = "muted"; return c }(), false},
		{"conversation empty, no created_at", empty(), true},
		{"conversation first invite without a message", func() DMConversationV1 { c := empty(); c.CreatedAt = &at; return c }(), true},
		{"conversation without a message but with a revision", func() DMConversationV1 { c := empty(); c.Revision = 1; return c }(), false},
		{"conversation without a message but unread", func() DMConversationV1 { c := empty(); c.Unread = 1; return c }(), false},
		{"conversation with a message but no created_at", func() DMConversationV1 { c := conv(); c.CreatedAt = nil; return c }(), false},
		{"conversation with a message at revision 0", func() DMConversationV1 { c := conv(); c.Revision = 0; return c }(), false},
		{"conversation negative revision", func() DMConversationV1 { c := conv(); c.Revision = -1; return c }(), false},
		{"conversation last_message_at without id", func() DMConversationV1 { c := conv(); c.LastMessageID, c.LastReadID = 0, 0; return c }(), false},
		{"conversation read past the newest", func() DMConversationV1 { c := conv(); c.LastReadID = 41; return c }(), false},
		{"conversation unread over cap", func() DMConversationV1 { c := conv(); c.Unread = MaxUnreadCount + 1; return c }(), false},

		{"message", msg(40), true},
		{"message with channel schema", func() DMMessageV1 { m := msg(40); m.Schema = MessageSchemaV2; return m }(), false},
		{"message no conversation", func() DMMessageV1 { m := msg(40); m.ConversationID = 0; return m }(), false},
		{"message no revision", func() DMMessageV1 { m := msg(40); m.Revision = 0; return m }(), false},
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
		// A channel send and a DM edit refuse these, so a DM send does too.
		{"post whitespace only", PostDMMessageRequestV1{Body: " \t\n ", ClientNonce: "n"}, false},
		{"post whitespace only with a file", PostDMMessageRequestV1{Body: "  ", ClientNonce: "n", FileIDs: []int64{1}}, false},
		{"post text with spaces around", PostDMMessageRequestV1{Body: " hi ", ClientNonce: "n"}, true},
		{"post empty file list is none", PostDMMessageRequestV1{Body: "hi", ClientNonce: "n", FileIDs: []int64{}}, true},
		{"post response", PostDMMessageResponseV1{Message: sent}, true},
		{"post response without author's nonce", PostDMMessageResponseV1{Message: msg(40)}, false},
		{"post response edited without replay", PostDMMessageResponseV1{Message: edited}, false},
		{"post response removed without replay", PostDMMessageResponseV1{Message: removed}, false},
		{"post response replayed edit", PostDMMessageResponseV1{Message: edited, Replayed: true}, true},
		{"post response replayed removal", PostDMMessageResponseV1{Message: removed, Replayed: true}, true},

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

		// The default is the channel's: oldest first, paged with after.
		{"history oldest first", ListDMMessagesResponseV1{Order: DMOrderAsc, Messages: []DMMessageV1{msg(30), msg(40)}, NextAfter: 40}, true},
		{"history newest page", ListDMMessagesResponseV1{Order: DMOrderDesc, Messages: []DMMessageV1{msg(40), msg(30)}, NextBefore: 30}, true},
		{"history no order", ListDMMessagesResponseV1{Messages: []DMMessageV1{msg(30), msg(40)}}, false},
		{"history unknown order", ListDMMessagesResponseV1{Order: "newest", Messages: []DMMessageV1{}}, false},
		{"history null", ListDMMessagesResponseV1{Order: DMOrderAsc}, false},
		{"history asc but descending", ListDMMessagesResponseV1{Order: DMOrderAsc, Messages: []DMMessageV1{msg(40), msg(30)}}, false},
		{"history desc but ascending", ListDMMessagesResponseV1{Order: DMOrderDesc, Messages: []DMMessageV1{msg(30), msg(40)}}, false},
		{"history two cursors", ListDMMessagesResponseV1{Order: DMOrderAsc, Messages: []DMMessageV1{msg(30), msg(40)}, NextBefore: 30, NextAfter: 40}, false},
		{"history asc with next_before", ListDMMessagesResponseV1{Order: DMOrderAsc, Messages: []DMMessageV1{msg(30), msg(40)}, NextBefore: 30}, false},
		{"history desc with next_after", ListDMMessagesResponseV1{Order: DMOrderDesc, Messages: []DMMessageV1{msg(40), msg(30)}, NextAfter: 40}, false},
		{"history cursor off the edge", ListDMMessagesResponseV1{Order: DMOrderAsc, Messages: []DMMessageV1{msg(30), msg(40)}, NextAfter: 39}, false},
		{"history cursor on empty page", ListDMMessagesResponseV1{Order: DMOrderAsc, Messages: []DMMessageV1{}, NextAfter: 40}, false},
		{"history two conversations", ListDMMessagesResponseV1{Order: DMOrderAsc, Messages: []DMMessageV1{msg(30), func() DMMessageV1 { m := msg(40); m.ConversationID = 13; return m }()}}, false},
		{"history over the page", ListDMMessagesResponseV1{Order: DMOrderAsc, Messages: make([]DMMessageV1, MaxDMPageSize+1)}, false},

		// The changes route is ordered by revision, not id: an edit of an old
		// message follows a newer message.
		{"changes", ListDMChangesResponseV1{Messages: []DMMessageV1{
			func() DMMessageV1 { m := msg(50); m.Revision = 8; return m }(),
			func() DMMessageV1 { m := msg(30); m.Revision = 9; m.EditedAt = &later; return m }()}, NextAfter: 9}, true},
		{"changes empty", ListDMChangesResponseV1{Messages: []DMMessageV1{}}, true},
		{"changes null", ListDMChangesResponseV1{}, false},
		{"changes out of revision order", ListDMChangesResponseV1{Messages: []DMMessageV1{msg(40), msg(30)}}, false},
		{"changes repeat revision", ListDMChangesResponseV1{Messages: []DMMessageV1{msg(40), func() DMMessageV1 { m := msg(41); m.Revision = 40; return m }()}}, false},
		{"changes repeat a message", ListDMChangesResponseV1{Messages: []DMMessageV1{msg(40), func() DMMessageV1 { m := msg(40); m.Revision = 41; return m }()}}, false},
		{"changes cursor off the edge", ListDMChangesResponseV1{Messages: []DMMessageV1{msg(30), msg(40)}, NextAfter: 30}, false},
		{"changes over the page", ListDMChangesResponseV1{Messages: make([]DMMessageV1, MaxDMChangesPage+1)}, false},
		{"changes of two conversations", ListDMChangesResponseV1{Messages: []DMMessageV1{msg(30), func() DMMessageV1 { m := msg(40); m.ConversationID = 13; return m }()}}, false},

		{"open response", OpenDMResponseV1{Conversation: conv()}, true},
		{"open response without a conversation", OpenDMResponseV1{}, false},

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

		{"snapshot", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{conv()}, TakenAt: at}, true},
		// Archived conversations are in the snapshot: a message received while
		// offline in one is found by its revision, as in an open one.
		{"snapshot archived", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{func() DMConversationV1 { c := conv(); c.State = DMStateArchived; return c }()}, TakenAt: at}, true},
		{"snapshot hidden", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{func() DMConversationV1 { c := conv(); c.State = DMStateHidden; return c }()}, TakenAt: at}, false},
		{"snapshot twice", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{conv(), conv()}, TakenAt: at}, false},
		{"snapshot null", DMSnapshotV1{Schema: DMSnapshotSchemaV1, TakenAt: at}, false},
		{"snapshot no time", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: []DMConversationV1{}}, false},
		{"snapshot over the cap", DMSnapshotV1{Schema: DMSnapshotSchemaV1, Conversations: make([]DMConversationV1, MaxDMSnapshotConversations+1), TakenAt: at}, false},
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
		// An empty page that examined candidates carries the cursor that moves
		// past them, even with more behind it.
		{"page empty, cursor, more", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}, Cursor: "c_9", More: true}, true},
		// A nonempty page always gives the client its resume position.
		{"page with summaries, no cursor, no more", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{sum(direct, NotificationKindDirect)}}, false},
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

// TestPendingV2CannotStall pins the progress rule: a page that says More must
// have moved the cursor past ?after=, so a client never asks the same question
// twice. The stall the review found is an empty page, from a bounded scan of
// candidates that produced no summary, answering More with the cursor it was
// given.
func TestPendingV2CannotStall(t *testing.T) {
	tests := []struct {
		name  string
		after string
		page  ListPendingNotificationsResponseV2
		ok    bool
	}{
		{"empty page advanced past muted rows", "pf_a", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}, Cursor: "pf_b", More: true}, true},
		{"empty page, same cursor, more: the stall", "pf_a", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}, Cursor: "pf_a", More: true}, false},
		{"empty page at the start, more, with a cursor", "", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}, Cursor: "pf_b", More: true}, true},
		{"nothing examined: after echoed, no more", "pf_a", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}, Cursor: "pf_a"}, true},
		{"nothing examined at the start", "", ListPendingNotificationsResponseV2{Summaries: []NotificationSummaryV2{}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.page.Validate(); err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			err := tt.page.CheckProgress(tt.after)
			if tt.ok && err != nil {
				t.Fatalf("CheckProgress() = %v, want nil", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("CheckProgress() = nil, want an error")
			}
		})
	}
}

// TestDMWireRevealsNoPriorOpenOrAccountState pins that no DM document
// carries what would tell a reader something they were not shown: a
// "created" flag or a 201, or a peer-availability flag, would reveal that
// the peer opened first, or that the peer's account is gone.
func TestDMWireRevealsNoPriorOpenOrAccountState(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*dm-*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DM fixtures found (%v)", err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file) // #nosec G304 -- a fixture path this test globbed from its own testdata
		if err != nil {
			t.Fatal(err)
		}
		var doc any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		for _, key := range []string{"created", "opened_by", "opened_at", "peer_available", "available", "blocked", "blocked_by", "reason"} {
			if hasKey(doc, key) {
				t.Errorf("%s carries %q", filepath.Base(file), key)
			}
		}
	}
	// The peer can receive the second response only after their own eligible
	// open. Both responses carry the same id and no evidence of a prior open.
	var opener, peer OpenDMResponseV1
	decodeFixture(t, "open-dm-response-v1.json", &opener)
	decodeFixture(t, "open-dm-response-v1-peer-open.json", &peer)
	if opener.Conversation.ID != peer.Conversation.ID {
		t.Error("both opens must return the same opaque conversation id")
	}
	for _, c := range []DMConversationV1{opener.Conversation, peer.Conversation} {
		if c.CreatedAt != nil || c.Revision != 0 || c.LastMessageID != 0 {
			t.Errorf("an empty open shows %+v", c)
		}
	}
}

func TestDMNonceRetryFixtures(t *testing.T) {
	var original, conflict PostDMMessageRequestV1
	decodeFixture(t, "post-dm-message-request-v1-files.json", &original)
	decodeFixture(t, "post-dm-message-request-v1-nonce-conflict.json", &conflict)
	if conflict.ClientNonce != original.ClientNonce || conflict.Body == original.Body {
		t.Fatal("the conflicting retry must reuse the nonce and change the original body")
	}

	var first, edited, removed PostDMMessageResponseV1
	decodeFixture(t, "post-dm-message-response-v1.json", &first)
	decodeFixture(t, "post-dm-message-response-v1-replayed-edited.json", &edited)
	decodeFixture(t, "post-dm-message-response-v1-replayed-removed.json", &removed)
	if first.Replayed || edited.Message.EditedAt == nil || !removed.Message.Removed {
		t.Fatal("expected a first send, an edited replay and a tombstone replay")
	}
	for _, r := range []PostDMMessageResponseV1{edited, removed} {
		if !r.Replayed || r.Message.ID != first.Message.ID || r.Message.ConversationID != first.Message.ConversationID ||
			r.Message.ClientNonce != original.ClientNonce || r.Message.Revision <= first.Message.Revision {
			t.Errorf("expected the current copy of the original send, got %+v", r)
		}
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
		// Private calls name a conversation, never a channel, as DMs do.
		isDM := strings.Contains(base, "dm-") || strings.Contains(base, "private-call")
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
