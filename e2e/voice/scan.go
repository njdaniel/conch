package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/njdaniel/conch/internal/server/store"
)

// What must never reach a reader, and how the program looks for it.
//
// A room name is not a credential, but conchd promises never to write one to
// a log line, an audit row, an error or a presence document (design note §3).
// A token or the API secret must not appear anywhere. Both are searched for in
// everything conchd sends to a client (headers and bodies, below), in its log,
// in its audit log, in lk's output and in this program's own.
//
// A JWT carries the room name inside its payload, base64url-encoded. A JWT
// anywhere it should not be is already a failure (the scan flags any
// JWT-shaped string), but a payload segment alone has no dots. So for each
// room name the program also searches for its base64 and base64url encodings
// at each of the three byte alignments it can have inside a longer base64
// string. (The other choice, decoding every JWT-payload-shaped string found,
// needs a notion of "shaped like" that a bare payload does not satisfy.)

// roomNeedles returns the strings whose presence in text means the room name
// is in it: the name, and the interior of its base64 and base64url encodings
// at the three alignments. The first and last characters of each encoding are
// dropped, because they also depend on the bytes next to the name.
func roomNeedles(name string) []string {
	needles := []string{name}
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		for pad := 0; pad < 3; pad++ {
			s := enc.EncodeToString([]byte(strings.Repeat("x", pad) + name))
			// pad bytes of prefix occupy ceil(pad*8/6) characters, the last of
			// which is shared with the name.
			skip := (pad*8 + 5) / 6
			if skip+2 >= len(s) {
				continue
			}
			needles = append(needles, s[skip+1:len(s)-1])
		}
	}
	return needles
}

// scanResponse checks what conchd sent back: every header, and the body unless
// the body is one that carries a token or a room name by design (a 200
// answer from the session endpoint, a credential just created). what names the
// request for the message.
func (h *harness) scanResponse(what string, header http.Header, body []byte, bodyExempt bool) error {
	var b strings.Builder
	for k, vs := range header {
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(strings.Join(vs, ", "))
		b.WriteString("\n")
	}
	if err := h.scan("the response headers of "+what, b.String()); err != nil {
		return err
	}
	if h.hasRoom(b.String()) {
		return fmt.Errorf("a room name appears in the response headers of %s", what)
	}
	if bodyExempt {
		return nil
	}
	if err := h.scan("the response body of "+what, string(body)); err != nil {
		return err
	}
	if h.hasRoom(string(body)) {
		return fmt.Errorf("a room name appears in the response body of %s", what)
	}
	return nil
}

// bodyCarriesCredentials reports whether a response is one whose body holds a
// secret on purpose: a session (the token and room), or a credential just
// issued or rotated (its token, shown once).
func bodyCarriesCredentials(method, path string, status int) bool {
	if status/100 != 2 {
		return false
	}
	p := strings.SplitN(path, "?", 2)[0]
	switch {
	case method == http.MethodPost && strings.HasSuffix(p, "/voice/session"):
		return true
	case method == http.MethodPost && strings.HasPrefix(p, "/v1/principals/") && strings.HasSuffix(p, "/credentials"):
		return true
	case method == http.MethodPost && strings.HasPrefix(p, "/v1/credentials/") && strings.HasSuffix(p, "/rotate"):
		return true
	}
	return false
}

// registerStoreRooms registers every room name this conchd has stored, live
// and retired, so that names the program never saw in a session (the room of a
// channel nobody joined, a room already retired) are covered by the scans.
func (p *conchdProc) registerStoreRooms() error {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(p.dataDir, "conch.db"))
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	live, err := st.ListVoiceRooms(ctx)
	if err != nil {
		return err
	}
	retired, err := st.ListRetiredVoiceRooms(ctx)
	if err != nil {
		return err
	}
	for _, r := range append(live, retired...) {
		p.h.room(r.RoomName)
	}
	return nil
}

// voiceRooms returns the names of the live rooms this conchd has stored for a
// channel. V4 has one: the channel's own.
func (p *conchdProc) voiceRooms(channelID int64) ([]string, error) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(p.dataDir, "conch.db"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	rooms, err := st.VoiceRoomsForChannel(ctx, channelID)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(rooms))
	for _, r := range rooms {
		names = append(names, r.RoomName)
	}
	return names, nil
}
