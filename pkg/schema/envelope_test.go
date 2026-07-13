package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMessageV1GoldenFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		new  func() any
	}{
		{name: "envelope", file: "message-v1.json", new: func() any { return &MessageV1{} }},
		{name: "with payload", file: "message-v1-with-payload.json", new: func() any { return &MessageV1{} }},
		{name: "unknown payload", file: "message-v1-unknown-payload.json", new: func() any { return &MessageV1{} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertGoldenFixture(t, tt.file, tt.new)
		})
	}
}

// TestMessageV1UnknownPayloadTolerance proves an envelope carrying an
// unregistered payload schema survives a decode/re-encode round trip
// byte-for-byte and never surfaces as an error — the forward-compatibility
// guarantee (acceptance criterion) at the API level, beyond the golden helper.
func TestMessageV1UnknownPayloadTolerance(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "message-v1-unknown-payload.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var msg MessageV1
	if err := json.Unmarshal(golden, &msg); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if msg.Payload == nil {
		t.Fatal("expected a payload")
	}
	if msg.Payload.Known() {
		t.Fatalf("payload schema %q should be unknown", msg.Payload.Schema)
	}
	// A structurally valid envelope even though the payload schema is unknown.
	if err := msg.Validate(); err != nil {
		t.Fatalf("unknown-payload envelope should validate: %v", err)
	}
	// Decoding the unknown payload is not an error and yields no typed value.
	v, known, err := msg.Payload.Decode()
	if err != nil || known || v != nil {
		t.Fatalf("Decode() = (%v, %v, %v), want (nil, false, nil)", v, known, err)
	}

	got, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	got = append(got, '\n')
	if !bytes.Equal(got, golden) {
		t.Errorf("round trip dropped or altered unknown payload\ngot:\n%s\nwant:\n%s", got, golden)
	}
}

func TestMessageV1Validate(t *testing.T) {
	valid := func() MessageV1 {
		return MessageV1{
			Schema:    MessageSchemaV1,
			ID:        1,
			ChannelID: 2,
			AuthorID:  3,
			CreatedAt: NewTimestamp(time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)),
			Body:      "hi",
		}
	}
	tests := []struct {
		name    string
		mutate  func(*MessageV1)
		wantErr bool
	}{
		{name: "valid", mutate: func(*MessageV1) {}, wantErr: false},
		{name: "valid with known payload", mutate: func(m *MessageV1) {
			m.Payload = &Payload{Schema: LeviathanTradeSignalV1Name, Data: json.RawMessage(`{"any":"json"}`)}
		}, wantErr: false},
		{name: "valid with unknown payload", mutate: func(m *MessageV1) {
			m.Payload = &Payload{Schema: "acme.weather_alert.v3", Data: json.RawMessage(`{"x":1}`)}
		}, wantErr: false},
		{name: "wrong schema", mutate: func(m *MessageV1) { m.Schema = "conch.message.v2" }, wantErr: true},
		{name: "empty schema", mutate: func(m *MessageV1) { m.Schema = "" }, wantErr: true},
		{name: "zero id", mutate: func(m *MessageV1) { m.ID = 0 }, wantErr: true},
		{name: "zero channel", mutate: func(m *MessageV1) { m.ChannelID = 0 }, wantErr: true},
		{name: "zero author", mutate: func(m *MessageV1) { m.AuthorID = 0 }, wantErr: true},
		{name: "zero created_at", mutate: func(m *MessageV1) { m.CreatedAt = Timestamp{} }, wantErr: true},
		{name: "empty body", mutate: func(m *MessageV1) { m.Body = "" }, wantErr: true},
		{name: "payload bad name", mutate: func(m *MessageV1) {
			m.Payload = &Payload{Schema: "no_version", Data: json.RawMessage(`{}`)}
		}, wantErr: true},
		{name: "payload empty data", mutate: func(m *MessageV1) {
			m.Payload = &Payload{Schema: LeviathanTradeSignalV1Name}
		}, wantErr: true},
		{name: "payload invalid json", mutate: func(m *MessageV1) {
			m.Payload = &Payload{Schema: LeviathanTradeSignalV1Name, Data: json.RawMessage(`{not json`)}
		}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := valid()
			tt.mutate(&m)
			err := m.Validate()
			if tt.wantErr != (err != nil) {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
