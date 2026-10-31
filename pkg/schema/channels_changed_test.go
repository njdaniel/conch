package schema

import (
	"encoding/json"
	"testing"
)

func TestChannelsChangedV1Fixture(t *testing.T) {
	assertP3GoldenFixture(t, "channels-changed-v1.json", func() any { return new(ChannelsChangedV1) })
}

// TestChannelsChangedV1Wire pins the frame's bytes as the events socket
// writes them: the schema and nothing else (issue #1027).
func TestChannelsChangedV1Wire(t *testing.T) {
	raw, err := json.Marshal(ChannelsChangedV1{Schema: ChannelsChangedSchemaV1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `{"schema":"conch.channels_changed.v1"}`; string(raw) != want {
		t.Errorf("frame = %s, want %s", raw, want)
	}
}

// TestChannelsChangedV1UnknownFields: a reader tolerates a field it does not
// know, so a later server is not refused by an older reader.
func TestChannelsChangedV1UnknownFields(t *testing.T) {
	var doc ChannelsChangedV1
	if err := json.Unmarshal([]byte(`{"schema":"conch.channels_changed.v1","later":1}`), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestChannelsChangedV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		wantErr bool
	}{
		{name: "v1", schema: ChannelsChangedSchemaV1},
		{name: "empty", schema: "", wantErr: true},
		{name: "a read state", schema: ReadStateSchemaV1, wantErr: true},
		{name: "a future version", schema: "conch.channels_changed.v2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ChannelsChangedV1{Schema: tt.schema}.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
