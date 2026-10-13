package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// MaxMessageFrameBytes claims that no message a server accepted can be sent
// back as a frame longer than it. The worst case is a body of characters that
// are one byte in the request and six once encoded again. Each envelope
// version is checked with a body as long as a whole request may be, which is
// more than any real body, so the claim has room to spare.
func TestTheLargestMessageFitsTheFrameLimit(t *testing.T) {
	at := NewTimestamp(time.Date(2026, time.October, 13, 12, 0, 0, 0, time.UTC))
	for _, worst := range []string{"<", ">", "&", "\"", "\\", "\n", "\x01", string(rune(0x2028)), "é", "x"} {
		body := strings.Repeat(worst, MaxPostRequestBytes/len(worst))
		ids := make([]int64, 1000)
		for i := range ids {
			ids[i] = int64(1_000_000_000 + i)
		}
		frames := map[string]any{
			"v0": MessageV0{ID: 1 << 60, ChannelID: 1 << 60, AuthorID: 1 << 60, Body: body, CreatedAt: at.Time()},
			"v1": MessageV1{Schema: MessageSchemaV1, ID: 1 << 60, ChannelID: 1 << 60, AuthorID: 1 << 60, CreatedAt: at, Body: body},
			"v2": MessageV2{Schema: MessageSchemaV2, ID: 1 << 60, ChannelID: 1 << 60, AuthorID: 1 << 60, CreatedAt: at, Body: body,
				Audience: &Audience{Kind: AudienceKindPrincipals, PrincipalIDs: ids}},
		}
		for version, frame := range frames {
			encoded, err := json.Marshal(frame)
			if err != nil {
				t.Fatal(err)
			}
			if len(encoded) > MaxMessageFrameBytes {
				t.Errorf("%s frame of %q bodies is %d bytes, over MaxMessageFrameBytes (%d)", version, worst, len(encoded), MaxMessageFrameBytes)
			}
		}
	}
	// And the limit is not simply enormous: it is within a small margin of
	// what the worst body needs.
	if MaxMessageFrameBytes > 6*MaxPostRequestBytes+1<<20 {
		t.Errorf("MaxMessageFrameBytes = %d is far more than six times a request", MaxMessageFrameBytes)
	}
}
