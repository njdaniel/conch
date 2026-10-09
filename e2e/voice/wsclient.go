package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// presenceSocket is a client of GET /v1/voice/ws?channel=: the presence
// stream. It keeps the latest document, decodes every frame strictly (an
// extra field fails the run), and scans every frame for a token or a room
// name.
type presenceSocket struct {
	c      *websocket.Conn
	cancel context.CancelFunc
	done   chan struct{}

	mu    sync.Mutex
	last  *schema.VoicePresenceV1
	count int
	err   error // why the socket ended
	bad   error // a frame that failed a check
}

// openPresenceSocket connects to bridge's presence stream as p. It returns the HTTP status when the
// upgrade is refused.
func (h *harness) openPresenceSocket(ctx context.Context, p *person) (*presenceSocket, int, error) {
	u := strings.Replace(p.d.baseURL, "http://", "ws://", 1) + "/v1/voice/ws?channel=" + "bridge"
	hdr := http.Header{"Authorization": {"Bearer " + p.token}}
	c, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
			if resp.Body != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
				_ = resp.Body.Close()
			}
		}
		return nil, status, fmt.Errorf("presence socket refused")
	}
	c.SetReadLimit(1 << 20)
	rctx, cancel := context.WithCancel(context.Background())
	s := &presenceSocket{c: c, cancel: cancel, done: make(chan struct{})}
	go s.read(rctx, h, p.name)
	h.onCleanup(s.Close)
	return s, http.StatusSwitchingProtocols, nil
}

func (s *presenceSocket) read(ctx context.Context, h *harness, who string) {
	defer close(s.done)
	for {
		_, data, err := s.c.Read(ctx)
		if err != nil {
			s.mu.Lock()
			if s.err == nil {
				s.err = err
			}
			s.mu.Unlock()
			return
		}
		fail := func(err error) {
			s.mu.Lock()
			if s.bad == nil {
				s.bad = err
			}
			s.mu.Unlock()
		}
		if err := h.scan("a presence socket frame ("+who+")", string(data)); err != nil {
			fail(err)
			return
		}
		if h.hasRoom(string(data)) {
			fail(fmt.Errorf("a room name appears in a presence socket frame (%s)", who))
			return
		}
		var doc schema.VoicePresenceV1
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&doc); err != nil {
			fail(fmt.Errorf("presence socket frame (%s) does not decode strictly: %w", who, err))
			return
		}
		if doc.Schema != schema.VoicePresenceSchemaV1 {
			fail(fmt.Errorf("presence socket frame (%s) has schema %q", who, doc.Schema))
			return
		}
		s.mu.Lock()
		s.last = &doc
		s.count++
		s.mu.Unlock()
	}
}

// latest returns the last document received, if any, and the number received.
func (s *presenceSocket) latest() (*schema.VoicePresenceV1, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last, s.count
}

// problem returns the first frame that failed a check.
func (s *presenceSocket) problem() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bad
}

// closeStatus reports whether the socket has ended and how the server closed it.
func (s *presenceSocket) closeStatus() (ended bool, status websocket.StatusCode) {
	select {
	case <-s.done:
	default:
		return false, -1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return true, websocket.CloseStatus(s.err)
}

// waitIDs waits until the latest document lists exactly the given principals
// (in any order) as connected.
func (s *presenceSocket) waitIDs(what string, within time.Duration, want ...int64) error {
	return waitFor(what, within, func() (bool, string) {
		if err := s.problem(); err != nil {
			return false, err.Error()
		}
		doc, n := s.latest()
		if doc == nil {
			return false, "no frame yet"
		}
		got := presenceIDs(*doc)
		return sameIDs(got, want), fmt.Sprintf("%d frames, last lists %v, available=%v", n, got, doc.Available)
	})
}

func (s *presenceSocket) Close() {
	s.cancel()
	_ = s.c.CloseNow()
	<-s.done
}

// presenceIDs lists the principals a document shows as connected.
func presenceIDs(doc schema.VoicePresenceV1) []int64 {
	var ids []int64
	for _, room := range doc.Rooms {
		for _, p := range room.Participants {
			ids = append(ids, p.PrincipalID)
		}
	}
	return ids
}

func sameIDs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[int64]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}
