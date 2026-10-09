package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Test doubles for voice presence (issue #127): a scripted LiveKit whose rooms
// and participants a test sets, a manual clock, and a fixture wiring them to a
// server. Nothing here sleeps; the poller is driven by runPass and runSweep.

// testClock is the poller's injected clock.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeParticipant is one connection the scripted LiveKit reports.
type fakeParticipant struct {
	identity string
	joinedMs int64
	// published and muted describe the microphone track.
	published bool
	muted     bool
}

// lkCall is one request the scripted LiveKit received (or an "http-response"
// marker a test inserts), in arrival order.
type lkCall struct {
	seq      int
	method   string
	room     string
	identity string
}

// scriptedLiveKit answers LiveKit's room API from state the test sets.
type scriptedLiveKit struct {
	*httptest.Server
	mu    sync.Mutex
	seq   int
	log   []lkCall
	rooms map[string][]fakeParticipant // a key present means LiveKit has the room
	// reportedCount is what ListRooms says for num_participants. LiveKit's
	// count lags a join (design note §10, finding 5), so tests leave it 0.
	reportedCount int
	// listStatus is the status of ListRooms and ListParticipants; 0 means 200.
	listStatus int
	// removeStatus is the status of RemoveParticipant; 0 means 200.
	removeStatus int
	// keepAfterRemove makes RemoveParticipant answer 200 without dropping the
	// participant, as when LiveKit is slow to close the connection.
	keepAfterRemove bool
	// holdList, when set, makes ListParticipants wait until it is closed or
	// the request is cancelled. holdRooms does the same for ListRooms.
	holdList  chan struct{}
	holdRooms chan struct{}
	// entered receives one value each time a held request starts waiting.
	entered chan struct{}
	// inflight and maxInflight count ListParticipants requests being served.
	inflight, maxInflight int
	// onRemove runs while RemoveParticipant is being answered, before it takes
	// effect: the moment a snapshot taken mid-removal would catch.
	onRemove func(room, identity string)
}

func newScriptedLiveKit(t *testing.T) *scriptedLiveKit {
	t.Helper()
	f := &scriptedLiveKit{rooms: map[string][]fakeParticipant{}, entered: make(chan struct{}, 64)}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *scriptedLiveKit) record(method, room, identity string) {
	f.seq++
	f.log = append(f.log, lkCall{f.seq, method, room, identity})
}

func (f *scriptedLiveKit) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Name     string `json:"name"`
		Room     string `json:"room"`
		Identity string `json:"identity"`
	}
	_ = json.Unmarshal(raw, &req)
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

	f.mu.Lock()
	switch method {
	case "CreateRoom":
		f.record(method, req.Name, "")
		if _, ok := f.rooms[req.Name]; !ok {
			f.rooms[req.Name] = nil
		}
		f.mu.Unlock()
		writeFakeJSON(w, http.StatusOK, map[string]any{"sid": "RM_test", "name": req.Name})
	case "ListRooms":
		f.record(method, "", "")
		status, hold, count := f.listStatus, f.holdRooms, f.reportedCount
		names := make([]string, 0, len(f.rooms))
		for name := range f.rooms {
			names = append(names, name)
		}
		f.mu.Unlock()
		if hold != nil && !f.wait(r, hold) {
			return
		}
		if status != 0 {
			writeFakeJSON(w, status, map[string]any{"code": "unavailable"})
			return
		}
		rooms := make([]map[string]any, 0, len(names))
		for _, n := range names {
			rooms = append(rooms, map[string]any{"name": n, "num_participants": count})
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{"rooms": rooms})
	case "ListParticipants":
		f.record(method, req.Room, "")
		f.inflight++
		f.maxInflight = max(f.maxInflight, f.inflight)
		status, hold := f.listStatus, f.holdList
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.inflight--
			f.mu.Unlock()
		}()
		if hold != nil && !f.wait(r, hold) {
			return
		}
		if status != 0 {
			writeFakeJSON(w, status, map[string]any{"code": "unavailable"})
			return
		}
		f.mu.Lock()
		parts := make([]map[string]any, 0)
		for _, p := range f.rooms[req.Room] {
			part := map[string]any{"identity": p.identity, "joined_at_ms": strconv.FormatInt(p.joinedMs, 10)}
			if p.published {
				part["tracks"] = []map[string]any{{"type": "AUDIO", "source": "MICROPHONE", "muted": p.muted}}
			}
			parts = append(parts, part)
		}
		f.mu.Unlock()
		writeFakeJSON(w, http.StatusOK, map[string]any{"participants": parts})
	case "RemoveParticipant":
		f.record(method, req.Room, req.Identity)
		status, keep, hook := f.removeStatus, f.keepAfterRemove, f.onRemove
		f.mu.Unlock()
		if hook != nil {
			hook(req.Room, req.Identity)
		}
		if status != 0 {
			writeFakeJSON(w, status, map[string]any{"code": "internal"})
			return
		}
		if !keep {
			f.drop(req.Room, req.Identity)
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{})
	default:
		f.record(method, "", "")
		f.mu.Unlock()
		writeFakeJSON(w, http.StatusNotFound, map[string]any{"code": "bad_route"})
	}
}

// wait blocks a held request until released or cancelled and reports whether
// it was released.
func (f *scriptedLiveKit) wait(r *http.Request, hold chan struct{}) bool {
	f.entered <- struct{}{}
	select {
	case <-hold:
		return true
	case <-r.Context().Done():
		return false
	}
}

func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func (f *scriptedLiveKit) setRoom(room string, parts ...fakeParticipant) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rooms[room] = parts
}

// config changes the fake's behaviour under its lock: the server's goroutines
// read these fields while serving.
func (f *scriptedLiveKit) config(fn func(*scriptedLiveKit)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *scriptedLiveKit) setListStatus(s int) {
	f.config(func(l *scriptedLiveKit) { l.listStatus = s })
}
func (f *scriptedLiveKit) setRemoveStatus(s int) {
	f.config(func(l *scriptedLiveKit) { l.removeStatus = s })
}
func (f *scriptedLiveKit) setKeepAfterRemove(k bool) {
	f.config(func(l *scriptedLiveKit) { l.keepAfterRemove = k })
}
func (f *scriptedLiveKit) setHoldList(c chan struct{}) {
	f.config(func(l *scriptedLiveKit) { l.holdList = c })
}
func (f *scriptedLiveKit) setOnRemove(fn func(room, identity string)) {
	f.config(func(l *scriptedLiveKit) { l.onRemove = fn })
}

// in reports whether identity is connected to room.
func (f *scriptedLiveKit) in(room, identity string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.rooms[room] {
		if p.identity == identity {
			return true
		}
	}
	return false
}

func (f *scriptedLiveKit) drop(room, identity string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.rooms[room][:0:0]
	for _, p := range f.rooms[room] {
		if p.identity != identity {
			kept = append(kept, p)
		}
	}
	f.rooms[room] = kept
}

// update changes one participant in place.
func (f *scriptedLiveKit) update(room, identity string, fn func(*fakeParticipant)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := append([]fakeParticipant(nil), f.rooms[room]...)
	for i := range parts {
		if parts[i].identity == identity {
			fn(&parts[i])
		}
	}
	f.rooms[room] = parts
}

func (f *scriptedLiveKit) mark(label string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(label, "", "")
}

// calls returns a copy of the log.
func (f *scriptedLiveKit) calls() []lkCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]lkCall(nil), f.log...)
}

// count returns how many logged calls are of method.
func (f *scriptedLiveKit) count(method string) int {
	n := 0
	for _, c := range f.calls() {
		if c.method == method {
			n++
		}
	}
	return n
}

func (f *scriptedLiveKit) maxConcurrent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInflight
}

func (f *scriptedLiveKit) currentInflight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inflight
}

// presenceFixture is a server wired to a scripted LiveKit and a manual clock.
//
//	ann, ann2  human members of ops (ann also of ops2)
//	bob        human, a member of nothing
//	root       the operator, a member of nothing
//	robo       an agent member of ops with a full manifest
//	dora       a human member of ops who is disabled
type presenceFixture struct {
	*authFixture
	lk     *scriptedLiveKit
	clock  *testClock
	logs   *logCapture
	ops    store.Channel
	ops2   store.Channel
	tokens map[string]string
	ids    map[string]int64
}

type presenceOpts struct {
	auth         AuthMode
	unconfigured bool
}

func newPresenceFixture(t *testing.T, o presenceOpts) *presenceFixture {
	t.Helper()
	logs := captureLogs(t)
	lk := newScriptedLiveKit(t)
	cfg := Config{AuthMode: o.auth}
	if !o.unconfigured {
		cfg.LiveKit = voiceConfig(t, lk.URL)
	}
	srv := newTestServerWithConfig(t, cfg)
	clock := newTestClock()
	srv.voice.now = clock.Now
	ctx := context.Background()
	root, _, rootTok, err := srv.store.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	f := &presenceFixture{
		authFixture: &authFixture{srv: srv, root: root, rootTok: rootTok},
		lk:          lk, clock: clock, logs: logs,
		tokens: map[string]string{"root": rootTok},
		ids:    map[string]int64{"root": root.ID},
	}
	if f.ops, err = srv.store.CreateChannel(ctx, "ops"); err != nil {
		t.Fatal(err)
	}
	if f.ops2, err = srv.store.CreateChannel(ctx, "ops2"); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct {
		name string
		kind store.PrincipalKind
		in   []store.Channel
	}{
		{"ann", store.PrincipalHuman, []store.Channel{f.ops, f.ops2}},
		{"ann2", store.PrincipalHuman, []store.Channel{f.ops}},
		{"bob", store.PrincipalHuman, nil},
		{"robo", store.PrincipalAgent, []store.Channel{f.ops}},
		{"dora", store.PrincipalHuman, []store.Channel{f.ops}},
	} {
		p, err := srv.store.CreatePrincipal(ctx, spec.kind, spec.name)
		if err != nil {
			t.Fatal(err)
		}
		_, tok, err := srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.tokens[spec.name], f.ids[spec.name] = tok, p.ID
		for _, ch := range spec.in {
			if _, err := srv.store.AddChannelMember(ctx, "system", ch.ID, p.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	setAgentManifest(t, srv, f.ids["robo"], nil, f.ops.ID)
	if _, err := srv.store.DisablePrincipal(ctx, "system", f.ids["dora"]); err != nil {
		t.Fatal(err)
	}
	return f
}

// identity is the LiveKit identity of a named principal.
func (f *presenceFixture) identity(who string) string { return voiceIdentity(f.ids[who]) }

// room returns the stored room name of ch, creating the row, without
// telling the poller (a restart has no memory of sessions).
func (f *presenceFixture) room(t *testing.T, ch store.Channel) string {
	t.Helper()
	r, err := f.srv.store.ChannelVoiceRoom(context.Background(), ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r.RoomName
}

// inUse creates ch's room and tells the poller a session was just issued.
func (f *presenceFixture) inUse(t *testing.T, ch store.Channel) string {
	t.Helper()
	r, err := f.srv.store.ChannelVoiceRoom(context.Background(), ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.voice.noteSession(r)
	return r.RoomName
}

func (f *presenceFixture) pass(t *testing.T) {
	t.Helper()
	if !f.srv.voice.runPass(context.Background()) {
		t.Fatal("pass did not run")
	}
}

func (f *presenceFixture) sweep(t *testing.T) {
	t.Helper()
	if !f.srv.voice.runSweep(context.Background()) {
		t.Fatal("sweep did not run")
	}
}

// snap returns ch's snapshot after checking it validates.
func (f *presenceFixture) snap(t *testing.T, ch store.Channel) schema.VoicePresenceV1 {
	t.Helper()
	s := f.srv.voice.snapshot(ch.ID)
	if err := s.Validate(); err != nil {
		t.Fatalf("snapshot does not validate: %v", err)
	}
	return s
}

// who lists the principals in ch's channel-wide room, as names, with a "*"
// after those transmitting.
func (f *presenceFixture) who(t *testing.T, ch store.Channel) string {
	t.Helper()
	s := f.snap(t, ch)
	if len(s.Rooms) == 0 {
		return "-"
	}
	byID := map[int64]string{}
	for name, id := range f.ids {
		byID[id] = name
	}
	var out []string
	for _, p := range s.Rooms[0].Participants {
		n := byID[p.PrincipalID]
		if p.Transmitting {
			n += "*"
		}
		out = append(out, n)
	}
	return strings.Join(out, ",")
}

// voiceAudit lists the voice_* audit events in order as "action actor".
func (f *presenceFixture) voiceAudit(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, e := range f.audit(t) {
		if strings.HasPrefix(e.Action, "voice_") && e.Action != store.AuditVoiceSessionIssued {
			out = append(out, e.Action+" "+e.Actor)
		}
	}
	return out
}

func (f *presenceFixture) audits(t *testing.T, action string) []store.AuditEvent {
	t.Helper()
	var out []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func (f *presenceFixture) actor(who string) string { return fmt.Sprintf("principal:%d", f.ids[who]) }

func (f *presenceFixture) session(t *testing.T, who, channel string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPost, "/v1/channels/"+channel+"/voice/session", f.tokens[who], "")
}

// loopGoroutines counts running poller loops.
func loopGoroutines() int {
	var sb strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&sb, 2)
	return strings.Count(sb.String(), "(*voicePoller).loop")
}

// waitUntil spins, yielding the processor, until cond holds or a generous
// deadline passes. It is for observing another goroutine's progress (a request
// reaching a fake), never for waiting out a poller interval.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// orderWriter records, in the fake's log, the moment the server first writes
// a response.
type orderWriter struct {
	http.ResponseWriter
	lk   *scriptedLiveKit
	done bool
}

func (w *orderWriter) note() {
	if !w.done {
		w.done = true
		w.lk.mark("http-response")
	}
}

func (w *orderWriter) WriteHeader(code int) {
	w.note()
	w.ResponseWriter.WriteHeader(code)
}

func (w *orderWriter) Write(b []byte) (int, error) {
	w.note()
	return w.ResponseWriter.Write(b)
}

// doOrdered sends a request through the server and returns the status; the
// fake's log gets an "http-response" entry at the moment the response begins.
func (f *presenceFixture) doOrdered(t *testing.T, method, path, token string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(""))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(&orderWriter{ResponseWriter: rec, lk: f.lk}, req)
	return rec.Code
}
