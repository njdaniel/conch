package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestMessageEditGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "edited message", file: "message-v2-edited.json", new: func() any { return &MessageV2{} }},
		{name: "edited message with attachments", file: "message-v2-edited-attachment.json", new: func() any { return &MessageV2{} }},
		{name: "edit request", file: "edit-message-request-v1.json", new: func() any { return &EditMessageRequestV1{} }},
		{name: "edit response", file: "edit-message-response-v1.json", new: func() any { return &EditMessageResponseV1{} }},
		{name: "edit response whisper", file: "edit-message-response-v1-whisper.json", new: func() any { return &EditMessageResponseV1{} }},
		{name: "revision", file: "message-revision-v1.json", new: func() any { return &MessageRevisionV1{} }},
		{name: "revisions", file: "list-message-revisions-response-v1.json", new: func() any { return &ListMessageRevisionsResponseV1{} }},
		{name: "revisions unedited", file: "list-message-revisions-response-v1-unedited.json", new: func() any { return &ListMessageRevisionsResponseV1{} }},
		{name: "events frame", file: "message-edited-v1.json", new: func() any { return &MessageEditedV1{} }},
		{name: "window closed", file: "error-message-edit-window-closed.json", new: func() any { return &Error{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
			if _, isError := tt.new().(*Error); !isError {
				assertFixtureValidates(t, tt.file, tt.new)
			}
		})
	}
}

func TestMessageEditErrorCodes(t *testing.T) {
	// The codes are wire values: a rename is a breaking change.
	assertErrorCodes(t, map[string]string{
		ErrorCodeMessageNotEditable:      "message_not_editable",
		ErrorCodeMessageEditWindowClosed: "message_edit_window_closed",
		ErrorCodeMessageEditLimitReached: "message_edit_limit_reached",
		ErrorCodeMessageEditRequiresAuth: "message_edit_requires_auth",
		ErrorCodeMessageNotFound:         "message_not_found",
		ErrorCodeHumanRequired:           "human_required",
	})
	if MessageEditedSchemaV1 != "conch.message_edited.v1" {
		t.Errorf("MessageEditedSchemaV1 = %q", MessageEditedSchemaV1)
	}
	if MessageEditWindowSeconds != 3600 {
		t.Errorf("MessageEditWindowSeconds = %d, want 3600 (docs/design/message-editing.md §3.2)", MessageEditWindowSeconds)
	}
}

func TestMessageV2EditedAtValidate(t *testing.T) {
	created := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *Timestamp { ts := NewTimestamp(created.Add(d)); return &ts }
	tests := []struct {
		name     string
		editedAt *Timestamp
		wantErr  string
	}{
		{name: "never edited", editedAt: nil},
		{name: "edited a millisecond later", editedAt: at(time.Millisecond)},
		{name: "edited at the end of the window", editedAt: at(MessageEditWindowSeconds * time.Second)},
		{name: "edited at creation", editedAt: at(0), wantErr: "edited_at must be after created_at"},
		{name: "edited before creation", editedAt: at(-time.Second), wantErr: "edited_at must be after created_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validMessageV2()
			m.EditedAt = tt.editedAt
			err := m.Validate()
			checkErr(t, err, tt.wantErr)
		})
	}
}

func TestMessageV2EditedAtIsOmittedUnlessSet(t *testing.T) {
	raw, err := json.Marshal(validMessageV2())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "edited_at") {
		t.Errorf("a message never edited encodes edited_at: %s", raw)
	}
}

func TestEditMessageRequestV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{name: "text", body: "fixed"},
		{name: "text with surrounding space", body: "  fixed\n"},
		{name: "empty", body: "", wantErr: "must not be empty"},
		{name: "only white space", body: " \t\n", wantErr: "must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, EditMessageRequestV1{Body: tt.body}.Validate(), tt.wantErr)
		})
	}
}

func TestEditMessageRequestV1RefusesOtherFields(t *testing.T) {
	// The server decodes with unknown fields refused; a request that tries to
	// change the payload, audience or thread must fail to decode, not be
	// silently narrowed to its body.
	for _, raw := range []string{
		`{"body":"x","payload":{"schema":"conch.approval.v1","data":{}}}`,
		`{"body":"x","audience":{"kind":"net","net_id":1}}`,
		`{"body":"x","thread_root_id":4}`,
		`{"body":"x","file_ids":[1]}`,
	} {
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		var r EditMessageRequestV1
		if err := dec.Decode(&r); err == nil {
			t.Errorf("%s decoded into EditMessageRequestV1", raw)
		}
	}
}

func revisionsAt(n int) ListMessageRevisionsResponseV1 {
	created := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	revs := make([]MessageRevisionV1, n)
	for i := range revs {
		revs[i] = MessageRevisionV1{Revision: i + 1, Body: "v", CreatedAt: NewTimestamp(created.Add(time.Duration(i) * time.Minute))}
	}
	return ListMessageRevisionsResponseV1{ChannelID: 7, MessageID: 42, Revisions: revs}
}

func TestListMessageRevisionsResponseV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		list    ListMessageRevisionsResponseV1
		wantErr string
	}{
		{name: "one", list: revisionsAt(1)},
		{name: "the most", list: revisionsAt(MaxMessageEdits + 1)},
		{name: "none", list: revisionsAt(0), wantErr: "must list 1 to"},
		{name: "too many", list: revisionsAt(MaxMessageEdits + 2), wantErr: "must list 1 to"},
		{name: "no channel", list: func() ListMessageRevisionsResponseV1 { l := revisionsAt(1); l.ChannelID = 0; return l }(),
			wantErr: "channel_id must be positive"},
		{name: "no message", list: func() ListMessageRevisionsResponseV1 { l := revisionsAt(1); l.MessageID = 0; return l }(),
			wantErr: "message_id must be positive"},
		{name: "a gap", list: func() ListMessageRevisionsResponseV1 { l := revisionsAt(2); l.Revisions[1].Revision = 3; return l }(),
			wantErr: "numbered from 1 in order"},
		{name: "not starting at one", list: func() ListMessageRevisionsResponseV1 { l := revisionsAt(1); l.Revisions[0].Revision = 2; return l }(),
			wantErr: "numbered from 1 in order"},
		{name: "same time twice", list: func() ListMessageRevisionsResponseV1 {
			l := revisionsAt(2)
			l.Revisions[1].CreatedAt = l.Revisions[0].CreatedAt
			return l
		}(), wantErr: "must be later than revision 1"},
		{name: "empty body", list: func() ListMessageRevisionsResponseV1 { l := revisionsAt(1); l.Revisions[0].Body = ""; return l }(),
			wantErr: "revision body is required"},
		{name: "no time", list: func() ListMessageRevisionsResponseV1 {
			l := revisionsAt(1)
			l.Revisions[0].CreatedAt = Timestamp{}
			return l
		}(), wantErr: "revision created_at is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.list.Validate(), tt.wantErr)
		})
	}
}

func TestMessageEditedV1Validate(t *testing.T) {
	edited := validMessageV2()
	ts := NewTimestamp(edited.CreatedAt.Time().Add(time.Minute))
	edited.EditedAt = &ts
	tests := []struct {
		name    string
		frame   MessageEditedV1
		wantErr string
	}{
		{name: "valid", frame: MessageEditedV1{Schema: MessageEditedSchemaV1, Message: edited}},
		{name: "wrong schema", frame: MessageEditedV1{Schema: MessageSchemaV2, Message: edited}, wantErr: "message edited schema must be"},
		{name: "not edited", frame: MessageEditedV1{Schema: MessageEditedSchemaV1, Message: validMessageV2()}, wantErr: "must carry a message with edited_at"},
		{name: "invalid message", frame: MessageEditedV1{Schema: MessageEditedSchemaV1, Message: func() MessageV2 { m := edited; m.Body = ""; return m }()},
			wantErr: "message body is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.frame.Validate(), tt.wantErr)
		})
	}
}

func TestMessageV1FromV2DropsEditedAt(t *testing.T) {
	m := validMessageV2()
	ts := NewTimestamp(m.CreatedAt.Time().Add(time.Minute))
	m.EditedAt = &ts
	v1, err := MessageV1FromV2(m)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Body != m.Body {
		t.Errorf("v1 body = %q, want the current text %q", v1.Body, m.Body)
	}
	raw, err := json.Marshal(v1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "edited_at") {
		t.Errorf("a v1 message carries edited_at: %s", raw)
	}
}

// checkErr fails unless err is nil when want is empty, or contains want.
func checkErr(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Errorf("unexpected error: %v", err)
	case want != "" && err == nil:
		t.Errorf("want error containing %q, got nil", want)
	case want != "" && !strings.Contains(err.Error(), want):
		t.Errorf("error %q does not contain %q", err, want)
	}
}
