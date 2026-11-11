package cli

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestModerationEventDispatch(t *testing.T) {
	ts := schema.NewTimestamp(time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC))
	members := schema.ChannelMembersV1{Schema: schema.ChannelMembersSchemaV1, ChannelID: 1, Members: []schema.ChannelMemberV1{{PrincipalID: 7, Name: "person", Kind: schema.PrincipalHuman, CreatedAt: ts}}}
	removed := schema.MessageRemovedV1{Schema: schema.MessageRemovedSchemaV1, Message: schema.MessageV2{Schema: schema.MessageSchemaV2, ID: 42, ChannelID: 1, AuthorID: 7, CreatedAt: ts, Body: schema.MessageRemovedBody, Removed: true}}
	frame, err := json.Marshal(members)
	if err != nil {
		t.Fatal(err)
	}
	var gotMembers schema.ChannelMembersV1
	if err := dispatchEvent(frame, EventHandler{Members: func(got schema.ChannelMembersV1) error { gotMembers = got; return nil }}); err != nil {
		t.Fatal(err)
	}
	if gotMembers.ChannelID != 1 || len(gotMembers.Members) != 1 || gotMembers.Members[0].PrincipalID != 7 {
		t.Fatalf("members = %+v", gotMembers)
	}
	frame, err = json.Marshal(removed)
	if err != nil {
		t.Fatal(err)
	}
	var gotRemoved schema.MessageRemovedV1
	if err := dispatchEvent(frame, EventHandler{MessageRemoved: func(got schema.MessageRemovedV1) error { gotRemoved = got; return nil }}); err != nil {
		t.Fatal(err)
	}
	if gotRemoved.Message.ID != 42 || !gotRemoved.Message.Removed || gotRemoved.Message.Body != schema.MessageRemovedBody {
		t.Fatalf("removal = %+v", gotRemoved)
	}
	stop := errors.New("stop")
	if err := dispatchEvent(frame, EventHandler{MessageRemoved: func(schema.MessageRemovedV1) error { return stop }}); !errors.Is(err, stop) {
		t.Fatalf("handler error = %v", err)
	}
	invalid := removed
	invalid.Message.Removed = false
	frame, err = json.Marshal(invalid)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatchEvent(frame, EventHandler{MessageRemoved: func(schema.MessageRemovedV1) error { return nil }}); err == nil {
		t.Fatal("a non-removal frame passed validation")
	}
	if err := dispatchEvent(frame, EventHandler{}); err != nil {
		t.Fatalf("an unwanted frame was not skipped: %v", err)
	}
}
