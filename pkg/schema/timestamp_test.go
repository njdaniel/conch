package schema

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTimestampMarshalCanonicalizes(t *testing.T) {
	// A fixed zone east of UTC, to prove local offsets are converted to "Z".
	kolkata := time.FixedZone("IST", 5*3600+30*60)
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			name: "utc millisecond",
			in:   time.Date(2026, 7, 13, 12, 34, 56, 789_000_000, time.UTC),
			want: `"2026-07-13T12:34:56.789Z"`,
		},
		{
			name: "nanoseconds truncated to millis",
			in:   time.Date(2026, 7, 13, 12, 34, 56, 789_999_999, time.UTC),
			want: `"2026-07-13T12:34:56.789Z"`,
		},
		{
			name: "non-utc offset converted to Z",
			in:   time.Date(2026, 7, 13, 18, 4, 56, 0, kolkata),
			want: `"2026-07-13T12:34:56.000Z"`,
		},
		{
			name: "whole second gets .000",
			in:   time.Date(2026, 7, 13, 12, 34, 56, 0, time.UTC),
			want: `"2026-07-13T12:34:56.000Z"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(NewTimestamp(tt.in))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestTimestampRoundTripStable(t *testing.T) {
	// Decoding a canonical string and re-encoding must be byte-identical.
	const canonical = `"2026-07-13T12:34:56.789Z"`
	var ts Timestamp
	if err := json.Unmarshal([]byte(canonical), &ts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != canonical {
		t.Errorf("round trip drifted: got %s, want %s", got, canonical)
	}
}

func TestTimestampUnmarshalCanonicalizesInput(t *testing.T) {
	// Finer precision and a non-UTC offset on input decode to the same instant
	// and re-encode canonically.
	var ts Timestamp
	if err := json.Unmarshal([]byte(`"2026-07-13T18:04:56.789999+05:30"`), &ts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := json.Marshal(ts)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `"2026-07-13T12:34:56.789Z"`; string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestTimestampUnmarshalRejectsNonString(t *testing.T) {
	var ts Timestamp
	if err := json.Unmarshal([]byte(`1752410096`), &ts); err == nil {
		t.Fatal("expected error decoding a numeric timestamp, got nil")
	}
}

func TestTimestampZero(t *testing.T) {
	if !NewTimestamp(time.Time{}).IsZero() {
		t.Error("NewTimestamp(zero) should be zero")
	}
	if (Timestamp{}).IsZero() != true {
		t.Error("zero Timestamp should report IsZero")
	}
}
