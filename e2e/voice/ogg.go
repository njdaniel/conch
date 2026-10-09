package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
)

// The headless participant publishes audio with `lk room join --publish`,
// which reads an Ogg Opus file. A real recording is not needed (the program
// asserts who is connected and whether a microphone track is unmuted, never
// what is heard), so the file is written here: a stretch of Opus silence.
// That keeps a binary out of the repository.

const (
	oggSerial       = 0x636f6e63 // arbitrary stream serial
	opusFrameSample = 960        // samples per 20 ms frame at 48 kHz
	opusPreSkip     = 312
	packetsPerPage  = 50
)

// silentOpusFrame is a 20 ms CELT fullband mono frame of silence: the TOC
// byte and two bytes of range-coded payload, the same silence WebRTC sends.
var silentOpusFrame = []byte{0xf8, 0xff, 0xfe}

// oggCRCTable is the Ogg CRC-32: polynomial 0x04c11db7, MSB first, no
// reflection, zero initial value, no final xor.
var oggCRCTable = func() [256]uint32 {
	var t [256]uint32
	for i := range t {
		r := uint32(i) << 24 // #nosec G115 -- i < 256
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return t
}()

func oggCRC(b []byte) uint32 {
	var crc uint32
	for _, c := range b {
		crc = crc<<8 ^ oggCRCTable[byte(crc>>24)^c]
	}
	return crc
}

// oggPage encodes one page. Every packet here is shorter than 255 bytes, so
// each takes one lacing value and none continues across pages.
func oggPage(headerType byte, granule int64, seq uint32, packets [][]byte) []byte {
	var b bytes.Buffer
	b.WriteString("OggS")
	b.WriteByte(0)
	b.WriteByte(headerType)
	_ = binary.Write(&b, binary.LittleEndian, granule)
	_ = binary.Write(&b, binary.LittleEndian, uint32(oggSerial))
	_ = binary.Write(&b, binary.LittleEndian, seq)
	_ = binary.Write(&b, binary.LittleEndian, uint32(0)) // CRC, filled in below
	b.WriteByte(byte(len(packets)))                      // #nosec G115 -- at most packetsPerPage
	for _, p := range packets {
		b.WriteByte(byte(len(p))) // #nosec G115 -- under 255 by construction
	}
	for _, p := range packets {
		b.Write(p)
	}
	out := b.Bytes()
	binary.LittleEndian.PutUint32(out[22:26], oggCRC(out))
	return out
}

// silentOpusOgg returns an Ogg Opus stream of the given length in seconds.
func silentOpusOgg(seconds int) []byte {
	head := new(bytes.Buffer)
	head.WriteString("OpusHead")
	head.WriteByte(1) // version
	head.WriteByte(1) // channels
	_ = binary.Write(head, binary.LittleEndian, uint16(opusPreSkip))
	_ = binary.Write(head, binary.LittleEndian, uint32(48000))
	_ = binary.Write(head, binary.LittleEndian, uint16(0)) // gain
	head.WriteByte(0)                                      // channel mapping family 0

	tags := new(bytes.Buffer)
	tags.WriteString("OpusTags")
	vendor := "conch voice-check"
	_ = binary.Write(tags, binary.LittleEndian, uint32(len(vendor))) // #nosec G115 -- constant, short
	tags.WriteString(vendor)
	_ = binary.Write(tags, binary.LittleEndian, uint32(0)) // no comments

	var out bytes.Buffer
	out.Write(oggPage(0x02, 0, 0, [][]byte{head.Bytes()}))
	out.Write(oggPage(0x00, 0, 1, [][]byte{tags.Bytes()}))
	total := seconds * 1000 / 20
	seq := uint32(2)
	for done := 0; done < total; {
		n := min(packetsPerPage, total-done)
		pk := make([][]byte, n)
		for i := range pk {
			pk[i] = silentOpusFrame
		}
		done += n
		kind := byte(0x00)
		if done == total {
			kind = 0x04 // end of stream
		}
		out.Write(oggPage(kind, int64(done)*opusFrameSample+opusPreSkip, seq, pk))
		seq++
	}
	return out.Bytes()
}

func writeSilentOpus(path string, seconds int) error {
	if err := os.WriteFile(path, silentOpusOgg(seconds), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
