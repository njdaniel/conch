package lkit

import (
	"bytes"
	"strings"
	"testing"
)

// protobuf field encoders for the frames below.
func pbBytes(field uint64, b []byte) []byte {
	out := append(putUvarint(field<<3|2), putUvarint(uint64(len(b)))...)
	return append(out, b...)
}

func pbVarint(field, v uint64) []byte {
	return append(putUvarint(field<<3), putUvarint(v)...)
}

// noSource asks addTrack for a request with no source field, as protobuf
// writes TrackSource UNKNOWN.
const noSource = ^uint64(0)

// addTrack builds a SignalRequest carrying an AddTrackRequest: cid, name,
// type, an optional source, and a layer list long enough that the request's
// length takes two bytes.
func addTrack(name string, source uint64) []byte {
	var req []byte
	req = append(req, pbBytes(1, []byte("TR_cid"))...)
	req = append(req, pbBytes(2, []byte(name))...)
	req = append(req, pbVarint(3, 1)...)
	if source != noSource {
		req = append(req, pbVarint(8, source)...)
	}
	req = append(req, pbBytes(9, bytes.Repeat([]byte{0x08, 0x01}, 80))...)
	return pbBytes(4, req)
}

// TestRewriteAddTrackSource: the relay turns lk's demo video into a screen
// share and share-audio.ogg into its sound, keeps every other field, and
// leaves every other track and frame byte for byte as lk sent it.
func TestRewriteAddTrackSource(t *testing.T) {
	offer := pbBytes(1, pbBytes(2, []byte(strings.Repeat("v=0 ", 50))))
	for _, tt := range []struct {
		name    string
		in      []byte
		want    []byte
		rewrote bool
	}{
		{"the demo video becomes a screen share", addTrack("demo", 1), addTrackMoved("demo", 3), true},
		{"share-audio.ogg becomes the screen's sound", addTrack("/tmp/x/share-audio.ogg", 2), addTrackMoved("/tmp/x/share-audio.ogg", 4), true},
		{"a share track with no source is given one", addTrack("demo", noSource), addTrackMoved("demo", 3), true},
		{"the microphone stays a microphone", addTrack("/tmp/x/silence.ogg", 2), addTrack("/tmp/x/silence.ogg", 2), false},
		{"another request", offer, offer, false},
		{"a truncated request", addTrack("demo", 1)[:20], addTrack("demo", 1)[:20], false},
		{"nothing", nil, nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, rewrote := RewriteAddTrackSource(tt.in, ShareTrackSource)
			if rewrote != tt.rewrote || !bytes.Equal(got, tt.want) {
				t.Errorf("rewrite = %x, %v; want %x, %v", got, rewrote, tt.want, tt.rewrote)
			}
		})
	}
}

// addTrackMoved is addTrack with its source moved to the end of the request,
// where the rewrite puts it: protobuf fields may come in any order.
func addTrackMoved(name string, source uint64) []byte {
	var req []byte
	req = append(req, pbBytes(1, []byte("TR_cid"))...)
	req = append(req, pbBytes(2, []byte(name))...)
	req = append(req, pbVarint(3, 1)...)
	req = append(req, pbBytes(9, bytes.Repeat([]byte{0x08, 0x01}, 80))...)
	req = append(req, pbVarint(8, source)...)
	return pbBytes(4, req)
}
