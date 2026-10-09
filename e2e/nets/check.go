package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// pageSize is deliberately small, so every list is read over several pages.
const pageSize = 4

type idSet map[int64]bool

func idsOf(msgs []schema.MessageV2) idSet {
	out := make(idSet, len(msgs))
	for _, m := range msgs {
		out[m.ID] = true
	}
	return out
}

// diffSets is the ids in a that are not in b, ascending.
func diffSets(a, b idSet) []int64 {
	var out []int64
	for id := range a {
		if !b[id] {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// want is the exact set of ids a participant may see: on a v2 surface every
// message whose audience includes it, on a v1 surface (which cannot express
// an audience) only the channel-wide messages.
func (h *harness) want(name string, v2 bool) idSet {
	out := make(idSet)
	for _, m := range h.posted {
		if !slices.Contains(m.viewers, name) {
			continue
		}
		if v2 || !m.scoped() {
			out[m.id] = true
		}
	}
	return out
}

// compare fails on any id a participant sees that it should not, and any id it
// should see and does not, naming the participant, the surface and the id.
func (h *harness) compare(who, surface string, got, want idSet) error {
	var problems []string
	for _, id := range diffSets(got, want) {
		problems = append(problems, fmt.Sprintf("%s sees message %d (%s) on %s, outside its audience", who, id, h.label(id), surface))
	}
	for _, id := range diffSets(want, got) {
		problems = append(problems, fmt.Sprintf("%s is missing message %d (%s) on %s", who, id, h.label(id), surface))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// messageKeys and audienceKeys are every field a v2 message, and the audience
// in it, may have on the wire. They are written out here, not taken from
// pkg/schema, on purpose: this program is built with the same schema package
// as the server, so a field added to the type there (a message's resolved
// recipients, say, which only the audit log may hold) would pass any decoder
// that asks the type what is allowed.
var (
	messageKeys  = map[string]bool{"schema": true, "id": true, "channel_id": true, "author_id": true, "created_at": true, "body": true, "payload": true, "audience": true}
	audienceKeys = map[string]bool{"kind": true, "net_id": true, "principal_ids": true}
)

// closedMessage fails when one v2 message, as it came off the wire, has a
// field outside those lists. The error names the field, never its content.
func closedMessage(raw json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("a v2 message is not a JSON object: %w", err)
	}
	for key := range fields {
		if !messageKeys[key] {
			return fmt.Errorf("a v2 message on the wire has a field %q, which the v2 envelope does not have", key)
		}
	}
	if aud, ok := fields["audience"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(aud, &inner); err != nil {
			return fmt.Errorf("a v2 message's audience is not a JSON object: %w", err)
		}
		for key := range inner {
			if !audienceKeys[key] {
				return fmt.Errorf("a v2 message's audience on the wire has a field %q, which an audience does not have", key)
			}
		}
	}
	return nil
}

// closedMessages applies closedMessage to a response body that holds v2
// messages under key: an array for a list, one object for a post.
func closedMessages(body []byte, key string) error {
	var outer map[string]json.RawMessage
	if err := json.Unmarshal(body, &outer); err != nil {
		return err
	}
	raw, ok := outer[key]
	if !ok {
		return nil
	}
	var many []json.RawMessage
	if json.Unmarshal(raw, &many) != nil {
		many = []json.RawMessage{raw}
	}
	for _, one := range many {
		if err := closedMessage(one); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------- list reads

// ascending checks one page of a list read: its ids rise strictly from the
// cursor it was asked for. A page that does not move the cursor would make
// the reader below loop for ever, and a repeated id would vanish in a set.
func ascending(what string, after int64, ids []int64) error {
	for _, id := range ids {
		if id <= after {
			return fmt.Errorf("%s: a page asked for after id %d holds id %d (ids must rise; this one repeats or goes back)", what, after, id)
		}
		after = id
	}
	return nil
}

func idsV2(msgs []schema.MessageV2) []int64 {
	out := make([]int64, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

// mcpRead reads the whole channel through read_channel, page by page.
func (h *harness) mcpRead(p *person) ([]schema.MessageV2, error) {
	var all []schema.MessageV2
	var after int64
	for {
		page, err := p.mcp.ReadChannel(context.Background(), channelName, after, pageSize)
		if err != nil {
			return nil, fmt.Errorf("read_channel: %w", err)
		}
		if len(page.Messages) == 0 {
			return all, nil
		}
		for _, m := range page.Messages {
			if err := m.Validate(); err != nil {
				return nil, fmt.Errorf("read_channel returned an invalid message: %w", err)
			}
		}
		if err := ascending("read_channel", after, idsV2(page.Messages)); err != nil {
			return nil, err
		}
		all = append(all, page.Messages...)
		after = page.Messages[len(page.Messages)-1].ID
	}
}

// restV2 reads the whole channel from GET /v2/channels/ops/messages.
func (h *harness) restV2(p *person) ([]schema.MessageV2, error) {
	var all []schema.MessageV2
	var after int64
	for {
		var page schema.ListMessagesResponseV2
		path := fmt.Sprintf("/v2/channels/%s/messages?limit=%d&after=%d", channelName, pageSize, after)
		raw, err := h.as(p).callRaw(http.MethodGet, path, nil, &page)
		if err != nil {
			return nil, err
		}
		if err := closedMessages(raw, "messages"); err != nil {
			return nil, fmt.Errorf("REST v2 list: %w", err)
		}
		if len(page.Messages) == 0 {
			return all, nil
		}
		for _, m := range page.Messages {
			if err := m.Validate(); err != nil {
				return nil, fmt.Errorf("GET %s returned an invalid message: %w", path, err)
			}
		}
		if err := ascending("REST v2 list", after, idsV2(page.Messages)); err != nil {
			return nil, err
		}
		all = append(all, page.Messages...)
		after = page.Messages[len(page.Messages)-1].ID
	}
}

// restV1 reads the whole channel from GET /v1/channels/ops/messages.
func (h *harness) restV1(p *person) ([]schema.MessageV1, error) {
	var all []schema.MessageV1
	var after int64
	for {
		var page schema.ListMessagesResponseV1
		path := fmt.Sprintf("/v1/channels/%s/messages?limit=%d&after=%d", channelName, pageSize, after)
		if err := h.as(p).call(http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		if len(page.Messages) == 0 {
			return all, nil
		}
		ids := make([]int64, len(page.Messages))
		for i, m := range page.Messages {
			ids[i] = m.ID
		}
		if err := ascending("REST v1 list", after, ids); err != nil {
			return nil, err
		}
		all = append(all, page.Messages...)
		after = page.Messages[len(page.Messages)-1].ID
	}
}

// restV0 reads the whole channel from GET /v0/channels/ops/messages.
func (h *harness) restV0(p *person) (idSet, error) {
	out := make(idSet)
	var after int64
	for {
		var page schema.ListMessagesResponse
		path := fmt.Sprintf("/v0/channels/%s/messages?limit=%d&after=%d", channelName, pageSize, after)
		if err := h.as(p).call(http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		if len(page.Messages) == 0 {
			return out, nil
		}
		ids := make([]int64, len(page.Messages))
		for i, m := range page.Messages {
			ids[i] = m.ID
			out[m.ID] = true
		}
		if err := ascending("REST v0 list", after, ids); err != nil {
			return nil, err
		}
		after = page.Messages[len(page.Messages)-1].ID
	}
}

// checkAudiences fails when a message read back does not carry the audience
// its table row addresses: a scoped message without one reads as channel-wide.
func (h *harness) checkAudiences(who, surface string, msgs []schema.MessageV2) error {
	for _, got := range msgs {
		for _, m := range h.posted {
			if m.id != got.ID {
				continue
			}
			if err := h.checkEcho(m, got.Audience); err != nil {
				return fmt.Errorf("%s on %s: %w", who, surface, err)
			}
		}
	}
	return nil
}

// checkReads compares every participant's list reads with the table, once
// everything is posted.
func (h *harness) checkReads() error {
	for _, p := range h.people {
		want2, want1 := h.want(p.name, true), h.want(p.name, false)
		if p.agent {
			msgs, err := h.mcpRead(p)
			if err != nil {
				return fmt.Errorf("%s: MCP read_channel: %w", p.name, err)
			}
			if err := h.compare(p.name, "MCP read_channel", idsOf(msgs), want2); err != nil {
				return err
			}
			if err := h.checkAudiences(p.name, "MCP read_channel", msgs); err != nil {
				return err
			}
		}
		msgs, err := h.restV2(p)
		if err != nil {
			return fmt.Errorf("%s: REST v2 list: %w", p.name, err)
		}
		if err := h.compare(p.name, "REST v2 list", idsOf(msgs), want2); err != nil {
			return err
		}
		if err := h.checkAudiences(p.name, "REST v2 list", msgs); err != nil {
			return err
		}
		got0, err := h.restV0(p)
		if err != nil {
			return fmt.Errorf("%s: REST v0 list: %w", p.name, err)
		}
		if err := h.compare(p.name, "REST v0 list", got0, want1); err != nil {
			return err
		}
		v1, err := h.restV1(p)
		if err != nil {
			return fmt.Errorf("%s: REST v1 list: %w", p.name, err)
		}
		got1 := make(idSet, len(v1))
		for _, m := range v1 {
			got1[m.ID] = true
		}
		if err := h.compare(p.name, "REST v1 list", got1, want1); err != nil {
			return err
		}
	}
	// The ids this run was given are 1..n with none missing. Message ids are
	// handed out in order, so a refused post that was stored after all, with
	// an audience nobody is in, would have taken one: the "nothing stored"
	// checks read as every participant and could not see such a row.
	ids := make([]int64, 0, len(h.posted))
	for _, m := range h.posted {
		ids = append(ids, m.id)
	}
	slices.Sort(ids)
	for i, id := range ids {
		if id != int64(i+1) {
			return fmt.Errorf("message ids of the %d messages this run posted are %v: id %d is missing, so the server stored a message no participant can see (a refused post?)", len(ids), ids, i+1)
		}
	}
	step("exact id sets on every list surface: MCP read_channel (agents), REST v2, v1 and v0 list (all %d participants), each scoped message with its audience; ids 1..%d with no gap", len(h.people), len(ids))
	return nil
}

// --------------------------------------------------------------- sockets

type sock struct {
	who     *person
	version string // "v0", "v1" or "v2"
	conn    *websocket.Conn
	got     idSet
	frames  []schema.MessageV2
	err     error
	done    chan struct{}
}

func (s *sock) surface() string { return "WebSocket " + s.version }

// lastBody is the body of the last message of the run. A socket is read until
// it arrives: nothing posted before it can still be on its way.
const lastBody = "MSG-sentinel-2-text"

func (h *harness) dialWS(p *person, version string) (*sock, error) {
	url := "ws" + strings.TrimPrefix(h.proc.baseURL, "http") + "/" + version + "/ws?channel=" + channelName
	ctx, cancel := withDeadline()
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + p.token}}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return nil, fmt.Errorf("%s: subscribe to the %s socket: status %d: %w", p.name, version, status, err)
	}
	s := &sock{who: p, version: version, conn: conn, got: make(idSet), done: make(chan struct{})}
	go s.collect()
	return s, nil
}

// collect reads frames until the last message of the run arrives. Each frame
// must be exactly the message shape of its protocol version, with no field
// besides: a v0 or v1 frame has no audience to carry, and no frame has
// anywhere to put a recipient list. A frame delivered twice is an error.
func (s *sock) collect() {
	defer close(s.done)
	ctx, cancel := withDeadline()
	defer cancel()
	for {
		_, data, err := s.conn.Read(ctx)
		if err != nil {
			s.err = fmt.Errorf("%s on %s: the last message never arrived: %w", s.who.name, s.surface(), err)
			return
		}
		var id int64
		var body string
		switch s.version {
		case "v2":
			var m schema.MessageV2
			if err = decodeStrict(data, &m); err == nil {
				err = m.Validate()
			}
			if err == nil {
				err = closedMessage(data)
			}
			id, body = m.ID, m.Body
			s.frames = append(s.frames, m)
		case "v1":
			var m schema.MessageV1
			err = decodeStrict(data, &m)
			id, body = m.ID, m.Body
		default:
			var m schema.MessageV0
			err = decodeStrict(data, &m)
			id, body = m.ID, m.Body
		}
		if err != nil {
			s.err = fmt.Errorf("%s on %s: a frame is not a %s message and nothing else: %w", s.who.name, s.surface(), s.version, err)
			return
		}
		if s.got[id] {
			s.err = fmt.Errorf("%s on %s: message %d was delivered twice", s.who.name, s.surface(), id)
			return
		}
		s.got[id] = true
		if body == lastBody {
			return
		}
	}
}

func (h *harness) openSockets() ([]*sock, error) {
	var socks []*sock
	for _, p := range h.people {
		for _, version := range []string{"v0", "v1", "v2"} {
			s, err := h.dialWS(p, version)
			if err != nil {
				closeSockets(socks)
				return nil, err
			}
			socks = append(socks, s)
		}
	}
	return socks, nil
}

// openLateSockets subscribes the two humans after most of the run: the socket
// replays nothing, so what they collect is only what is posted from now.
func (h *harness) openLateSockets() ([]*sock, error) {
	var socks []*sock
	for _, name := range []string{"operator", "lead"} {
		s, err := h.dialWS(h.person(name), "v2")
		if err != nil {
			closeSockets(socks)
			return nil, err
		}
		socks = append(socks, s)
	}
	return socks, nil
}

func closeSockets(socks []*sock) {
	for _, s := range socks {
		_ = s.conn.CloseNow()
	}
}

func (h *harness) checkSockets(early, late []*sock) error {
	for _, s := range early {
		<-s.done
		if s.err != nil {
			return s.err
		}
		if err := h.compare(s.who.name, s.surface()+" (live)", s.got, h.want(s.who.name, s.version == "v2")); err != nil {
			return err
		}
		if err := h.checkAudiences(s.who.name, s.surface()+" (live)", s.frames); err != nil {
			return err
		}
	}
	// A subscription that starts late must not be sent history: only the
	// last message, which was posted after it began.
	lastID := h.row("sentinel-2").id
	for _, s := range late {
		<-s.done
		if s.err != nil {
			return s.err
		}
		if err := h.compare(s.who.name, s.surface()+" (subscribed late, no replay)", s.got, idSet{lastID: true}); err != nil {
			return err
		}
	}
	step("exact id sets on %d live WebSocket subscriptions (v0, v1 and v2, every participant) and %d late ones, read until the sentinel; every v2 frame carries its audience", len(early), len(late))
	return nil
}

// ------------------------------------------------------------- conch tail

type tailProc struct {
	cmd    *exec.Cmd
	once   sync.Once
	cancel context.CancelFunc
	lines  chan string
	stderr *bytes.Buffer
	got    []string
	// firstID is the first message the tail displayed. `conch tail` starts
	// at the moment it connects, so what is expected is from there on.
	firstID int64
}

func (t *tailProc) stop() {
	t.once.Do(func() {
		t.cancel()
		_ = t.cmd.Wait()
	})
}

// startTail runs `conch tail ops` as lead, and posts channel-wide probes
// until the tail shows one: only then is it known to be subscribed. The
// probes are ordinary table rows, visible to everyone.
func (h *harness) startTail() (*tailProc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, h.bin.conch, "tail", channelName) // #nosec G204 -- h.bin.conch is this program's own just-built conch binary; args are local constants
	cmd.Env = h.cli.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start conch tail: %w", err)
	}
	t := &tailProc{cmd: cmd, cancel: cancel, lines: make(chan string, 1024), stderr: &stderr}
	go func() {
		defer close(t.lines)
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			t.lines <- sc.Text()
		}
	}()

	for i := 1; t.firstID == 0; i++ {
		if i > 40 {
			t.stop()
			return nil, fmt.Errorf("conch tail showed none of 40 probes (stderr: %s)", t.stderr.String())
		}
		probe := &message{key: fmt.Sprintf("tail-probe-%d", i), author: "operator", viewers: h.names()}
		if err := h.post(probe); err != nil {
			t.stop()
			return nil, err
		}
		timeout := time.After(300 * time.Millisecond)
	wait:
		for {
			select {
			case line, ok := <-t.lines:
				if !ok {
					t.stop()
					return nil, fmt.Errorf("conch tail exited (stderr: %s)", t.stderr.String())
				}
				t.got = append(t.got, line)
				for _, m := range h.posted {
					if strings.HasPrefix(m.key, "tail-probe-") && strings.HasSuffix(line, m.body()) {
						t.firstID = m.id
						break wait
					}
				}
			case <-timeout:
				break wait
			}
		}
	}
	step("conch tail is subscribed (first probe shown: message %d)", t.firstID)
	return t, nil
}

// tailLine is the line `conch tail` prints for a message, minus its timestamp.
func (h *harness) tailLine(m *message) string {
	marker := ""
	switch {
	case m.net != "":
		marker = "[net:" + m.net + "] "
	case m.to != nil:
		marker = "[whisper:" + joinIDs(h.ids(m.viewers)) + "] "
	}
	return fmt.Sprintf("%d %s%s", h.person(m.author).id, marker, m.body())
}

func (h *harness) checkTail(t *tailProc) error {
	defer t.stop()
	deadline := time.After(20 * time.Second)
	seenLast := false
	for _, l := range t.got {
		seenLast = seenLast || strings.HasSuffix(l, lastBody)
	}
	for !seenLast {
		select {
		case line, ok := <-t.lines:
			if !ok {
				return fmt.Errorf("lead on conch tail: it exited before the last message (stderr: %s)", t.stderr.String())
			}
			t.got = append(t.got, line)
			seenLast = strings.HasSuffix(line, lastBody)
		case <-deadline:
			return fmt.Errorf("lead on conch tail: the last message never arrived")
		}
	}
	want := make(map[string]*message)
	for _, m := range h.posted {
		if m.id >= t.firstID && slices.Contains(m.viewers, "lead") {
			want[h.tailLine(m)] = m
		}
	}
	got := make(map[string]bool)
	var problems []string
	for _, line := range t.got {
		fields := strings.SplitN(line, " ", 3)
		if len(fields) != 3 {
			problems = append(problems, fmt.Sprintf("lead on conch tail printed a line that is not a message: %q", line))
			continue
		}
		key := fields[1] + " " + fields[2]
		got[key] = true
		if _, ok := want[key]; !ok {
			problems = append(problems, fmt.Sprintf("lead sees %q (%s) on conch tail, outside its audience", key, h.labelByLine(key)))
		}
	}
	for key, m := range want {
		if !got[key] {
			problems = append(problems, fmt.Sprintf("lead is missing message %d (%s) on conch tail", m.id, m.key))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	step("conch tail as lead printed exactly the %d messages lead may see since it connected, with the right net and whisper markers", len(want))
	return nil
}

func (h *harness) labelByLine(line string) string {
	for _, m := range h.posted {
		if strings.HasSuffix(line, m.body()) {
			return fmt.Sprintf("message %d, %s", m.id, m.key)
		}
	}
	return "not posted by this run"
}

// ------------------------------------------------------------------ audit

// checkAudit reads the audit log the way e2e/dogfood does, from the database
// after the run. Every scoped message has exactly one message_scoped event
// with the recipients the design note says are recorded; no message body and
// no token is anywhere in the log.
func (h *harness) checkAudit() error {
	h.proc.halt()
	// The files are searched as the server left them, before this program
	// opens the database itself (which may fold the write-ahead log in).
	files, err := filepath.Glob(filepath.Join(h.proc.dataDir, "conch.db*"))
	if err != nil {
		return fmt.Errorf("audit: list the database files: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("audit: found no database files under the data directory")
	}
	for _, f := range files {
		data, err := os.ReadFile(f) // #nosec G304 -- the database of this harness's own conchd, under its temp directory
		if err != nil {
			return err
		}
		for i, tok := range h.secrets {
			if bytes.Contains(data, []byte(tok)) {
				return fmt.Errorf("audit: credential %d of this run is stored in plain text in %s", i, filepath.Base(f))
			}
		}
	}
	events, err := h.proc.auditEvents()
	if err != nil {
		return fmt.Errorf("read the audit log: %w", err)
	}
	scoped := 0
	for _, m := range h.posted {
		if !m.scoped() {
			continue
		}
		scoped++
		subject := fmt.Sprintf("message:%d", m.id)
		var hits []string
		for _, e := range events {
			if e.Action == "message_scoped" && e.Subject == subject {
				hits = append(hits, e.Detail)
			}
		}
		want := fmt.Sprintf("channel=%d audience=%s", h.channelID, map[bool]string{true: "net", false: "principals"}[m.net != ""])
		if m.net != "" {
			want += fmt.Sprintf(" net=%d", h.netIDs[m.net])
		}
		want += " recipients=" + joinIDs(h.ids(m.viewers))
		if len(hits) != 1 || hits[0] != want {
			return fmt.Errorf("audit: message %d (%s) has message_scoped events %q, want exactly one: %q", m.id, m.key, hits, want)
		}
	}
	count := 0
	var dump strings.Builder
	for _, e := range events {
		if e.Action == "message_scoped" {
			count++
		}
		fmt.Fprintf(&dump, "%s\t%s\t%s\t%s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	if count != scoped {
		return fmt.Errorf("audit: %d message_scoped events for %d scoped messages (a refused post left a record, or one is missing)", count, scoped)
	}
	// An agent's refused post is itself in the log (issue #149), with why it
	// was refused and nothing the agent chose: b3's two net refusals leave the
	// same row, subject and detail, so the log does not say which of the two
	// nets exists either, and holds no number the agent picked.
	denied := make(map[string][]string)
	for _, e := range events {
		if e.Action == "access_denied" {
			denied[e.Actor] = append(denied[e.Actor], e.Subject+" "+e.Detail)
		}
	}
	wantDenied := map[string][]string{
		fmt.Sprintf("principal:%d", h.person("b3").id): {
			fmt.Sprintf("mcp:post_message capability=messages.post target=channel:%d reason=net_not_on", h.channelID),
			fmt.Sprintf("mcp:post_message capability=messages.post target=channel:%d reason=net_not_on", h.channelID),
		},
		fmt.Sprintf("principal:%d", h.person("a1").id): {
			fmt.Sprintf("mcp:post_message capability=messages.post target=channel:%d reason=audience_not_granted", h.channelID),
		},
	}
	if len(denied) != len(wantDenied) {
		return fmt.Errorf("audit: access_denied events are %q, want %q", denied, wantDenied)
	}
	for actor, want := range wantDenied {
		if !slices.Equal(denied[actor], want) {
			return fmt.Errorf("audit: %s has access_denied events %q, want %q", actor, denied[actor], want)
		}
	}
	text := dump.String()
	if strings.Contains(text, "MSG-") {
		return fmt.Errorf("audit: a message body is in the audit log")
	}
	for i, tok := range h.secrets {
		if strings.Contains(text, tok) {
			return fmt.Errorf("audit: credential %d of this run is in the audit log", i)
		}
	}
	step("audit: %d message_scoped events, one per scoped message, with their recipients; the 3 refused agent posts recorded with their reasons; no body and no token among %d events", count, len(events))
	return nil
}
