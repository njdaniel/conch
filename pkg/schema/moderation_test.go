package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestModerationGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "ban", file: "channel-ban-v1.json", new: func() any { return &ChannelBanV1{} }},
		// Written with authentication off: no created_by.
		{name: "ban without creator", file: "channel-ban-v1-no-creator.json", new: func() any { return &ChannelBanV1{} }},
		{name: "ban list", file: "list-channel-bans-response-v1.json", new: func() any { return &ListChannelBansResponseV1{} }},
		{name: "ban list empty", file: "list-channel-bans-response-v1-empty.json", new: func() any { return &ListChannelBansResponseV1{} }},
		{name: "removed message", file: "message-v2-removed.json", new: func() any { return &MessageV2{} }},
		// A removed reply keeps its thread and its guest attribution.
		{name: "removed reply", file: "message-v2-removed-reply.json", new: func() any { return &MessageV2{} }},
		{name: "events frame", file: "message-removed-v1.json", new: func() any { return &MessageRemovedV1{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
			assertFixtureValidates(t, tt.file, tt.new)
		})
	}
}

func TestModerationConstants(t *testing.T) {
	// Wire values: a change to either is a breaking change.
	if MessageRemovedSchemaV1 != "conch.message_removed.v1" {
		t.Errorf("MessageRemovedSchemaV1 = %q", MessageRemovedSchemaV1)
	}
	if MessageRemovedBody != "This message was removed by a moderator." {
		t.Errorf("MessageRemovedBody = %q", MessageRemovedBody)
	}
	// The removal route reuses the reactions code; nothing new is defined.
	assertErrorCodes(t, map[string]string{ErrorCodeMessageNotFound: "message_not_found"})
}

func validChannelBan() ChannelBanV1 {
	return ChannelBanV1{
		PrincipalID: 12,
		Name:        "pat",
		CreatedBy:   3,
		CreatedAt:   NewTimestamp(time.Date(2026, 11, 5, 14, 2, 11, 0, time.UTC)),
	}
}

func TestChannelBanV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*ChannelBanV1)
		wantErr string
	}{
		{name: "valid", mutate: func(*ChannelBanV1) {}},
		{name: "valid without creator", mutate: func(b *ChannelBanV1) { b.CreatedBy = 0 }},
		// A manager may ban themselves (§6.1).
		{name: "valid self ban", mutate: func(b *ChannelBanV1) { b.CreatedBy = b.PrincipalID }},
		{name: "valid guest handle as name", mutate: func(b *ChannelBanV1) { b.Name = goodHandle }},
		{name: "valid hidden name", mutate: func(b *ChannelBanV1) { b.Name = "hidden-12" }},

		{name: "zero principal", mutate: func(b *ChannelBanV1) { b.PrincipalID = 0 }, wantErr: "principal_id must be positive, got 0"},
		{name: "negative principal", mutate: func(b *ChannelBanV1) { b.PrincipalID = -1 }, wantErr: "principal_id must be positive, got -1"},
		{name: "empty name", mutate: func(b *ChannelBanV1) { b.Name = "" }, wantErr: "channel ban 12 name is required"},
		{name: "blank name", mutate: func(b *ChannelBanV1) { b.Name = " \t" }, wantErr: "channel ban 12 name is required"},
		{name: "negative creator", mutate: func(b *ChannelBanV1) { b.CreatedBy = -3 }, wantErr: "created_by must not be negative, got -3"},
		{name: "no created_at", mutate: func(b *ChannelBanV1) { b.CreatedAt = Timestamp{} }, wantErr: "channel ban 12 created_at is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := validChannelBan()
			tt.mutate(&b)
			checkErr(t, b.Validate(), tt.wantErr)
		})
	}
}

func TestListChannelBansResponseV1Validate(t *testing.T) {
	second := validChannelBan()
	second.PrincipalID, second.Name, second.CreatedBy = 31, goodHandle, 0
	tests := []struct {
		name    string
		bans    []ChannelBanV1
		wantErr string
	}{
		{name: "empty", bans: []ChannelBanV1{}},
		{name: "one", bans: []ChannelBanV1{validChannelBan()}},
		{name: "ascending", bans: []ChannelBanV1{validChannelBan(), second}},
		{name: "null", bans: nil, wantErr: "must be an array, not null"},
		{name: "descending", bans: []ChannelBanV1{second, validChannelBan()}, wantErr: "ascending principal_id order, without repeats"},
		{name: "repeated", bans: []ChannelBanV1{validChannelBan(), validChannelBan()}, wantErr: "ascending principal_id order, without repeats"},
		{name: "invalid entry", bans: []ChannelBanV1{{PrincipalID: 12, CreatedAt: validChannelBan().CreatedAt}}, wantErr: "name is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, ListChannelBansResponseV1{Bans: tt.bans}.Validate(), tt.wantErr)
		})
	}
}

func TestListChannelBansEmptyIsArray(t *testing.T) {
	raw, err := json.Marshal(ListChannelBansResponseV1{Bans: []ChannelBanV1{}})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"bans":[]}` {
		t.Errorf("empty list = %s, want {\"bans\":[]}", raw)
	}
}

// removedMessageV2 is the tombstone of validMessageV2.
func removedMessageV2() MessageV2 {
	m := validMessageV2()
	m.Body, m.Removed = MessageRemovedBody, true
	return m
}

func TestMessageRemovedV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		frame   MessageRemovedV1
		wantErr string
	}{
		{name: "valid", frame: MessageRemovedV1{Schema: MessageRemovedSchemaV1, Message: removedMessageV2()}},
		{name: "valid removed reply with guest author", frame: MessageRemovedV1{Schema: MessageRemovedSchemaV1, Message: func() MessageV2 {
			m := removedMessageV2()
			m.ID, m.ThreadRootID = 5, 4
			m.GuestAuthor = &GuestAttributionV1{Handle: goodHandle, Label: "Pat", Tag: "a7k4"}
			return m
		}()}},
		{name: "wrong schema", frame: MessageRemovedV1{Schema: MessageEditedSchemaV1, Message: removedMessageV2()}, wantErr: "message removed schema must be"},
		{name: "empty schema", frame: MessageRemovedV1{Message: removedMessageV2()}, wantErr: `message removed schema must be "conch.message_removed.v1", got ""`},
		{name: "not removed", frame: MessageRemovedV1{Schema: MessageRemovedSchemaV1, Message: validMessageV2()}, wantErr: "must carry a removed message"},
		{name: "invalid message", frame: MessageRemovedV1{Schema: MessageRemovedSchemaV1, Message: func() MessageV2 { m := removedMessageV2(); m.Body = "gone"; return m }()},
			wantErr: "removed message body must be"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checkErr(t, tt.frame.Validate(), tt.wantErr)
		})
	}
}

// removed is additive: a message not removed encodes with no trace of it.
func TestMessageV2RemovedIsOmittedUnlessSet(t *testing.T) {
	raw, err := json.Marshal(validMessageV2())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"removed"`) {
		t.Errorf("a message not removed encodes removed: %s", raw)
	}
	raw, err = json.Marshal(removedMessageV2())
	if err != nil || !strings.HasSuffix(string(raw), `,"removed":true}`) {
		t.Errorf("a removed message encodes %s (%v); want removed last", raw, err)
	}
}

// A v1 reader gets the fixed body and no marker (moderation.md §7.3).
func TestMessageV1FromV2DropsRemoved(t *testing.T) {
	v1, err := MessageV1FromV2(removedMessageV2())
	if err != nil {
		t.Fatal(err)
	}
	if v1.Body != MessageRemovedBody {
		t.Errorf("v1 body = %q, want %q", v1.Body, MessageRemovedBody)
	}
	if v1.Payload != nil {
		t.Errorf("v1 payload = %+v, want none", v1.Payload)
	}
	raw, err := json.Marshal(v1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"removed"`) {
		t.Errorf("a v1 message carries removed: %s", raw)
	}
	if err := v1.Validate(); err != nil {
		t.Errorf("the v1 form should validate: %v", err)
	}
}
