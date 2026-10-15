package schema

import "testing"

func TestHeartbeatV1Fixture(t *testing.T) {
	assertGoldenFixture(t, "heartbeat-v1.json", func() any { return new(HeartbeatV1) })
}

func TestHeartbeatV1Validate(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		wantErr bool
	}{
		{name: "v1", schema: HeartbeatSchemaV1},
		{name: "empty", schema: "", wantErr: true},
		{name: "a message", schema: MessageSchemaV2, wantErr: true},
		{name: "a future version", schema: "conch.heartbeat.v2", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := HeartbeatV1{Schema: tt.schema}.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHeartbeatTimings(t *testing.T) {
	// A client must survive one lost frame before it gives up.
	if HeartbeatTimeoutSeconds < 2*HeartbeatIntervalSeconds {
		t.Errorf("timeout %ds is under two intervals of %ds", HeartbeatTimeoutSeconds, HeartbeatIntervalSeconds)
	}
}
