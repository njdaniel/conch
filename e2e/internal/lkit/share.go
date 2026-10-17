package lkit

import (
	"context"
	"slices"
	"strings"
	"sync"
)

// The screen share (issue #443, design note docs/design/voice-control-plane.md §12)
//
// A share is published by a client that has claimed it from conchd, which then
// has LiveKit let that one connection publish the screen-share sources. lk
// publishes files and a demo video, always as a microphone and a camera, and
// always as soon as it has joined. The relay in front of it (a ShareFilter)
// makes up for both:
//
//   - it rewrites the source of named tracks in lk's add_track requests: the
//     demo video becomes a screen share, and a file called share-audio.ogg
//     its sound. To LiveKit that is a screen share like a browser's: the
//     source is what the client says it is, and the permission check is on
//     the source.
//   - it can hold back the first of those requests until the harness lets it
//     go, which is how lk joins, waits while conchd grants the share, and
//     then publishes it on the same connection, as a browser does.
//
// LiveKit itself is asked what it did throughout: the permission it holds for
// the sharer, the tracks it lists, and what it told a viewer.

// LiveKit's TrackSource numbers, as they travel in an add_track request.
const (
	sourceScreenShare      = 3
	sourceScreenShareAudio = 4
)

// ShareTrackSource says which source lk's track called name is to have: the
// demo video is the screen, share-audio.ogg its sound, anything else what lk
// gave it.
func ShareTrackSource(name string) (uint64, bool) {
	switch {
	case name == "demo":
		return sourceScreenShare, true
	case strings.HasSuffix(name, "share-audio.ogg"):
		return sourceScreenShareAudio, true
	}
	return 0, false
}

// ShareFilter is a relay filter that rewrites lk's share tracks to the
// screen-share sources. With a gate, the first share track waits until the
// gate is released (or the connection ends); Held reports, once, that one is
// waiting.
type ShareFilter struct {
	gate chan struct{} // nil: never wait
	once sync.Once
	held chan struct{}
}

// NewShareFilter makes a filter; a gated one holds the share back until
// Release.
func NewShareFilter(gated bool) *ShareFilter {
	f := &ShareFilter{held: make(chan struct{})}
	if gated {
		f.gate = make(chan struct{})
	}
	return f
}

// Release lets a held share track through.
func (f *ShareFilter) Release() {
	if f.gate != nil {
		close(f.gate)
	}
}

// Held is closed once the first share track has reached the filter.
func (f *ShareFilter) Held() <-chan struct{} { return f.held }

// Filter is the relay filter (StartRelay, Kit.StartLK).
func (f *ShareFilter) Filter(ctx context.Context, frame []byte) []byte {
	out, rewrote := RewriteAddTrackSource(frame, ShareTrackSource)
	if !rewrote {
		return frame
	}
	f.once.Do(func() { close(f.held) })
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
		}
	}
	return out
}

// RewriteAddTrackSource rewrites the source of an add_track signal request
// (SignalRequest field 4, AddTrackRequest; its name is field 2 and its source
// field 8, livekit_rtc.proto) when source(name) says to. Any other frame, or
// one it cannot read, is returned unchanged. It walks the protobuf wire
// format itself: no protobuf library is a dependency of this module.
func RewriteAddTrackSource(msg []byte, source func(name string) (uint64, bool)) ([]byte, bool) {
	for i := 0; i < len(msg); {
		key, n := uvarint(msg[i:])
		if n == 0 {
			return msg, false
		}
		field, wire := key>>3, key&7
		start := i
		i += n
		if field != 4 || wire != 2 {
			size := fieldSize(msg[i:], wire)
			if size < 0 {
				return msg, false
			}
			i += size
			continue
		}
		inner, size, ok := lengthPrefixed(msg[i:])
		if !ok {
			return msg, false
		}
		end := i + size
		name, kept := "", []byte(nil)
		for j := 0; j < len(inner); {
			k2, n2 := uvarint(inner[j:])
			if n2 == 0 {
				ok = false
				break
			}
			f2, w2 := k2>>3, k2&7
			size := fieldSize(inner[j+n2:], w2)
			if size < 0 {
				ok = false
				break
			}
			if f2 == 2 && w2 == 2 {
				value, _, _ := lengthPrefixed(inner[j+n2:])
				name = string(value)
			}
			if f2 != 8 {
				kept = append(kept, inner[j:j+n2+size]...)
			}
			j += n2 + size
		}
		to, rewrite := source(name)
		if !ok || !rewrite {
			return msg, false
		}
		kept = append(append(kept, 8<<3), putUvarint(to)...)
		out := append([]byte(nil), msg[:start]...)
		out = append(out, putUvarint(4<<3|2)...)
		out = append(out, putUvarint(uint64(len(kept)))...)
		out = append(out, kept...)
		return append(out, msg[end:]...), true
	}
	return msg, false
}

// uvarint reads a protobuf varint; n is 0 if there is none.
func uvarint(b []byte) (v uint64, n int) {
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

func putUvarint(v uint64) []byte {
	var out []byte
	for v >= 0x80 {
		out = append(out, byte(v)|0x80)
		v >>= 7
	}
	return append(out, byte(v))
}

// lengthPrefixed reads a length-delimited value at the start of b: the value,
// and how many bytes it takes with its length. ok is false when it does not
// fit in b.
func lengthPrefixed(b []byte) (value []byte, size int, ok bool) {
	l, n := uvarint(b)
	if n == 0 || l > uint64(len(b)-n) { // #nosec G115 -- n <= len(b): uvarint read n bytes of b
		return nil, 0, false
	}
	size = n + int(l) // #nosec G115 -- l <= len(b)-n, checked above
	return b[n:size], size, true
}

// fieldSize is the length of a field's value of wire type wire at the start
// of b, or -1 if it cannot be read.
func fieldSize(b []byte, wire uint64) int {
	switch wire {
	case 0:
		if _, n := uvarint(b); n > 0 {
			return n
		}
	case 1:
		if len(b) >= 8 {
			return 8
		}
	case 2:
		if _, size, ok := lengthPrefixed(b); ok {
			return size
		}
	case 5:
		if len(b) >= 4 {
			return 4
		}
	}
	return -1
}

// ShareTracks reports which screen-share sources LiveKit lists for p.
func ShareTracks(p Participant) (screen, audio bool) {
	for _, s := range p.Sources() {
		switch s {
		case "SCREEN_SHARE", "3":
			screen = true
		case "SCREEN_SHARE_AUDIO", "4":
			audio = true
		}
	}
	return screen, audio
}

// ContainsAll reports whether each string is in at least one of lines.
func ContainsAll(lines []string, each ...string) bool {
	for _, s := range each {
		if !slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, s) }) {
			return false
		}
	}
	return true
}
