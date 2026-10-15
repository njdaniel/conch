package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadStateGoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "marked", file: "read-state-v1.json", new: func() any { return new(ReadStateV1) }},
		{name: "never marked, at the cap", file: "read-state-v1-never.json", new: func() any { return new(ReadStateV1) }},
		{name: "mark request", file: "mark-read-request-v1.json", new: func() any { return new(MarkReadRequestV1) }},
		{name: "list", file: "list-read-state-response-v1.json", new: func() any { return new(ListReadStateResponseV1) }},
		{name: "list, empty", file: "list-read-state-response-v1-empty.json", new: func() any { return new(ListReadStateResponseV1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The list has no Validate of its own (it promises an array), so
			// it is round-tripped here and each entry is validated below.
			if _, isList := tt.new().(*ListReadStateResponseV1); isList {
				assertGoldenFixture(t, tt.file, tt.new)
				return
			}
			assertP3GoldenFixture(t, tt.file, tt.new)
		})
	}
}

func TestReadStateListFixturesValidate(t *testing.T) {
	for _, file := range []string{"list-read-state-response-v1.json", "list-read-state-response-v1-empty.json"} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", file)) // #nosec G304 -- fixture name comes from this test's own table
			if err != nil {
				t.Fatal(err)
			}
			var list ListReadStateResponseV1
			if err := json.Unmarshal(raw, &list); err != nil {
				t.Fatal(err)
			}
			if list.ReadState == nil {
				t.Fatal("read_state decoded as null, want an array")
			}
			for _, r := range list.ReadState {
				if err := r.Validate(); err != nil {
					t.Errorf("entry for channel %d: %v", r.ChannelID, err)
				}
			}
		})
	}
}

func TestReadStateSchemaName(t *testing.T) {
	if ReadStateSchemaV1 != "conch.read_state.v1" {
		t.Fatalf("ReadStateSchemaV1 = %q", ReadStateSchemaV1)
	}
	if MaxUnreadCount != 1000 {
		t.Fatalf("MaxUnreadCount = %d", MaxUnreadCount)
	}
}

func TestReadStateErrorCodes(t *testing.T) {
	assertErrorCodes(t, map[string]string{
		ErrorCodeReadStateRequireAuth: "read_state_require_auth",
		ErrorCodeHumanRequired:        "human_required",
	})
}

func TestReadStateValidate(t *testing.T) {
	at := NewTimestamp(time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC))
	zero := Timestamp{}
	good := func() ReadStateV1 {
		return ReadStateV1{Schema: ReadStateSchemaV1, ChannelID: 7, LastReadID: 120, Unread: 5, UpdatedAt: &at}
	}
	tests := []struct {
		name    string
		mutate  func(*ReadStateV1)
		wantErr string
	}{
		{name: "valid", mutate: func(*ReadStateV1) {}},
		{name: "never marked", mutate: func(r *ReadStateV1) { r.LastReadID, r.UpdatedAt = 0, nil }},
		{name: "at the cap", mutate: func(r *ReadStateV1) { r.Unread = MaxUnreadCount }},
		{name: "wrong schema", mutate: func(r *ReadStateV1) { r.Schema = "conch.session.v1" }, wantErr: "read state schema must be"},
		{name: "no channel", mutate: func(r *ReadStateV1) { r.ChannelID = 0 }, wantErr: "channel_id must be positive"},
		{name: "negative mark", mutate: func(r *ReadStateV1) { r.LastReadID = -1 }, wantErr: "last_read_id must not be negative"},
		{name: "negative unread", mutate: func(r *ReadStateV1) { r.Unread = -1 }, wantErr: "unread must be between"},
		{name: "above the cap", mutate: func(r *ReadStateV1) { r.Unread = MaxUnreadCount + 1 }, wantErr: "unread must be between"},
		{name: "zero updated_at", mutate: func(r *ReadStateV1) { r.UpdatedAt = &zero }, wantErr: "updated_at must not be zero"},
		{name: "updated_at without a mark", mutate: func(r *ReadStateV1) { r.LastReadID = 0 }, wantErr: "never marked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := good()
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

func TestMarkReadRequestValidate(t *testing.T) {
	tests := []struct {
		name    string
		id      int64
		wantErr bool
	}{
		{name: "zero", id: 0},
		{name: "positive", id: 125},
		{name: "negative", id: -1, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := MarkReadRequestV1{LastReadID: tt.id}.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestReadStateOmitsUpdatedAtWhenNeverMarked(t *testing.T) {
	raw, err := json.Marshal(ReadStateV1{Schema: ReadStateSchemaV1, ChannelID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "updated_at") {
		t.Fatalf("never-marked state encodes as %s", raw)
	}
}
