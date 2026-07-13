package schema

import (
	"errors"
	"strconv"
	"time"
)

// wireTimeFormat is the single canonical wire encoding for every Conch
// timestamp from v1 onward: RFC 3339 in UTC with fixed millisecond precision
// and a literal "Z" zone. Formatting a UTC time with this layout always yields
// e.g. "2026-07-13T12:34:56.789Z".
const wireTimeFormat = "2006-01-02T15:04:05.000Z07:00"

// Timestamp is a wire timestamp pinned to UTC with millisecond precision.
//
// Every timestamp on a v1-or-later wire shape uses this type instead of
// time.Time. It exists to make three classes of bug caught in the P0 review
// structurally impossible on the wire: local-timezone timestamps,
// nanosecond/millisecond precision drift, and zone offsets other than "Z". The
// underlying instant is unexported and can only be set through NewTimestamp or
// JSON decoding, both of which normalize to UTC truncated to the millisecond;
// there is no way to hold a non-canonical value.
type Timestamp struct {
	t time.Time
}

// NewTimestamp returns t normalized to the canonical wire form: UTC, truncated
// to millisecond precision. The zero time.Time yields the zero Timestamp.
func NewTimestamp(t time.Time) Timestamp {
	if t.IsZero() {
		return Timestamp{}
	}
	return Timestamp{t: t.UTC().Truncate(time.Millisecond)}
}

// Time returns the underlying instant in UTC.
func (ts Timestamp) Time() time.Time { return ts.t }

// IsZero reports whether the timestamp is the zero value.
func (ts Timestamp) IsZero() bool { return ts.t.IsZero() }

// MarshalJSON encodes the timestamp as an RFC 3339 UTC string with fixed
// millisecond precision. It always emits three fractional digits and a "Z"
// zone, regardless of how the value was constructed.
func (ts Timestamp) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, len(wireTimeFormat)+2)
	b = append(b, '"')
	b = ts.t.UTC().Truncate(time.Millisecond).AppendFormat(b, wireTimeFormat)
	b = append(b, '"')
	return b, nil
}

// UnmarshalJSON accepts any RFC 3339 timestamp and normalizes it to the
// canonical wire form (UTC, millisecond precision). Input of finer precision or
// a non-UTC offset is tolerated on decode but canonicalized, so a
// decode/re-encode round trip is always stable.
func (ts *Timestamp) UnmarshalJSON(data []byte) error {
	s, err := strconv.Unquote(string(data))
	if err != nil {
		return errors.New("schema: timestamp must be a JSON string")
	}
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*ts = NewTimestamp(parsed)
	return nil
}
