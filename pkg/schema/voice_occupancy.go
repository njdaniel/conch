package schema

import (
	"errors"
	"fmt"
)

// Voice occupancy (issue #596; design note docs/design/voice-occupancy.md):
// how many people are in each channel's voice, for every channel the reader
// may see voice in, so that someone told "meet in voice" can see where the
// others are. It is a new type; nothing published changes.
//
// It is presence reduced to one number per channel. It names nobody: who is
// there is in that channel's own presence (VoicePresenceV1), one request
// away for a reader who may read it. Like presence it carries no token and no
// room name, and voice_occupancy_test.go asserts that.

// VoiceOccupancySchemaV1 is the versioned name identifying the v1 voice
// occupancy document on the wire. It is carried in VoiceOccupancyV1.Schema so
// the occupancy socket's reader can recognize the document, and tell it from
// a presence document, without out-of-band context.
const VoiceOccupancySchemaV1 = "conch.voice_occupancy.v1"

// VoiceChannelOccupancy is one channel the reader may see voice in, and how
// many people are in its channel-wide room.
type VoiceChannelOccupancy struct {
	// ChannelID references the channel.
	ChannelID int64 `json:"channel_id"`
	// InVoice is how many distinct people are in the channel's channel-wide
	// room (the room with no audience), as the channel's presence would list
	// them: two connections by one person are one person, and the reader
	// counts themself. Rooms with an audience (a net's) are never counted, for
	// anyone, so the number means the same to everyone who sees it. Zero is
	// written out: it says "nobody", where an absent channel says nothing.
	InVoice int `json:"in_voice"`
}

// VoiceOccupancyV1 is the whole voice occupancy of the server as one reader
// may see it: the body of GET /v1/voice/occupancy and every frame of its
// socket, GET /v1/voice/occupancy/ws. Each is the whole state, never a delta,
// so a reader that misses a frame loses nothing.
//
// Channels is always a JSON array, ordered by channel_id. When voice is
// available it has one row for every channel the reader may see voice in,
// zeros included, and no other: a channel the reader may not read is absent,
// never a zero. When voice is not configured or LiveKit cannot be reached it
// is empty, as presence reports no rooms then: a count last seen is not
// reported.
type VoiceOccupancyV1 struct {
	// Schema is the document discriminator; always VoiceOccupancySchemaV1.
	Schema string `json:"schema"`
	// Configured is whether this conchd has LiveKit configured at all.
	Configured bool `json:"configured"`
	// Available is whether LiveKit answered on the last pass. Always false
	// when Configured is false.
	Available bool `json:"available"`
	// Channels lists the channels the reader may see voice in, with their
	// counts.
	Channels []VoiceChannelOccupancy `json:"channels"`
}

// Validate reports whether the document is structurally well-formed: the v1
// schema name; available only when configured; channels an array (never
// null), empty unless available; every channel_id positive, in ascending
// order and listed once; and no count negative.
func (o VoiceOccupancyV1) Validate() error {
	if o.Schema != VoiceOccupancySchemaV1 {
		return fmt.Errorf("schema: voice occupancy schema must be %q, got %q", VoiceOccupancySchemaV1, o.Schema)
	}
	if !o.Configured && o.Available {
		return errors.New("schema: voice occupancy cannot be available when not configured")
	}
	if o.Channels == nil {
		return errors.New("schema: voice occupancy channels must be an array, not null")
	}
	if !o.Available && len(o.Channels) != 0 {
		return errors.New("schema: voice occupancy must list no channels when not available")
	}
	var prev int64
	for _, c := range o.Channels {
		if c.ChannelID <= 0 {
			return fmt.Errorf("schema: voice occupancy channel_id must be positive, got %d", c.ChannelID)
		}
		if c.ChannelID == prev {
			return fmt.Errorf("schema: voice occupancy lists channel %d more than once", c.ChannelID)
		}
		if c.ChannelID < prev {
			return fmt.Errorf("schema: voice occupancy channels must be in ascending channel_id order, got %d after %d", c.ChannelID, prev)
		}
		if c.InVoice < 0 {
			return fmt.Errorf("schema: voice occupancy in_voice of channel %d must not be negative, got %d", c.ChannelID, c.InVoice)
		}
		prev = c.ChannelID
	}
	return nil
}
