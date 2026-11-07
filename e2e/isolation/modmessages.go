package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Message removal (issue #853, moderation.md §7 and §9 cases 7 to 9) on a
// real server: who may remove what, what each socket family is sent of it,
// what every history, search, thread and file surface says of it afterwards,
// what a replaying socket is sent while messages are posted and removed, and
// what a reconnect or another credential is told.

// sockets of one person, by what they carry.
type kind int

const (
	kindMessageV0 kind = iota // /v0/ws and /v1/ws: the safe v0 or v1 message
	kindMessageV2             // /v2/ws: the v2 envelope
	kindEvents                // /v1/events/ws
	kindVoice                 // /v1/voice/ws: no messages
)

func kindOf(f *feed) kind {
	switch {
	case strings.Contains(f.what, "/v1/events/ws"):
		return kindEvents
	case strings.Contains(f.what, "/v1/voice/ws"):
		return kindVoice
	case strings.Contains(f.what, "/v2/ws"):
		return kindMessageV2
	}
	return kindMessageV0
}

// reader is a person's open sockets on the moderated channel.
type reader struct {
	p     *member
	feeds []*feed
}

// want is what a socket is owed of a removal.
type want int

const (
	wantNothing  want = iota
	wantSafe          // exactly one tombstone: v0/v1 body, no marker
	wantEnvelope      // exactly one tombstone: v2, removed true
	wantEvent         // exactly one conch.message_removed.v1
)

// wideWant is what each socket owes of a channel-wide removal.
func wideWant(f *feed) want {
	switch kindOf(f) {
	case kindMessageV0:
		return wantSafe
	case kindMessageV2:
		return wantEnvelope
	case kindEvents:
		return wantEvent
	}
	return wantNothing
}

// scopedWant is what a recipient's sockets owe of a scoped removal: v2 only.
func scopedWant(f *feed) want {
	switch kindOf(f) {
	case kindMessageV2:
		return wantEnvelope
	case kindEvents:
		return wantEvent
	}
	return wantNothing
}

// checkRemoval waits for each feed to carry what it is owed of the removal
// of message id since base, lets anything extra arrive, and requires that
// each carried exactly that: one tombstone in its envelope, no other frame
// about the message, and no frame at all that holds any of originals.
func (m *mod) checkRemoval(what string, feeds []*feed, base map[*feed]int, id int64, owed func(*feed) want, originals ...string) error {
	for _, f := range feeds {
		if owed(f) == wantNothing {
			continue
		}
		if err := waitFor(f.what+" to carry the tombstone of "+what, modWait, func() (bool, string) {
			for _, fr := range f.since(base[f]) {
				if decodeFrame(fr).tombstone(id) {
					return true, ""
				}
			}
			return false, fmt.Sprintf("%d frames since", len(f.since(base[f])))
		}); err != nil {
			return err
		}
	}
	// Whatever else is coming has come: a duplicate, an original, a leak.
	select {
	case <-time.After(1500 * time.Millisecond):
	case <-stopCtx.Done():
		return errInterrupted
	}
	for _, f := range feeds {
		frames := f.since(base[f])
		tombs, names := 0, 0
		for _, fr := range frames {
			df := decodeFrame(fr)
			if df.tombstone(id) {
				tombs++
				switch owed(f) {
				case wantSafe:
					if bytes.Contains(fr, []byte(`"removed"`)) {
						return fmt.Errorf("%s of %s: a v0 or v1 tombstone carries a removed marker: %s", f.what, what, fr)
					}
				case wantEnvelope:
					if !df.Removed {
						return fmt.Errorf("%s of %s: a v2 tombstone without removed: %s", f.what, what, fr)
					}
				case wantEvent:
					if df.Schema != schema.MessageRemovedSchemaV1 {
						return fmt.Errorf("%s of %s: an events tombstone that is not %s: %s", f.what, what, schema.MessageRemovedSchemaV1, fr)
					}
				}
			}
			if df.names(id) {
				names++
			}
			for _, o := range originals {
				if o != "" && bytes.Contains(fr, []byte(o)) {
					return fmt.Errorf("%s of %s: a frame after the removal holds the original %q: %s", f.what, what, o, fr)
				}
			}
		}
		var wantTombs int
		switch owed(f) {
		case wantNothing:
			wantTombs = 0
		default:
			wantTombs = 1
		}
		if tombs != wantTombs || names != wantTombs {
			return fmt.Errorf("%s of %s: %d tombstones and %d frames about it, want %d of each", f.what, what, tombs, names, wantTombs)
		}
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s closed: a removal disconnects no one", f.what)
		}
	}
	return nil
}

// upload uploads a text file to the moderated channel as p.
func (m *mod) upload(p *member, name, contents string) (int64, error) {
	status, data, err := p.api.send(http.MethodPost, "/v2/channels/"+url.PathEscape(m.main)+"/files?"+url.Values{"filename": {name}}.Encode(), "text/plain", []byte(contents))
	if err != nil {
		return 0, err
	}
	if status != http.StatusCreated {
		return 0, fmt.Errorf("%s uploads a file: status %d: %s", p.name, status, m.h.redact(string(data)))
	}
	var file schema.FileV1
	if err := json.Unmarshal(data, &file); err != nil {
		return 0, err
	}
	return file.ID, nil
}

// say posts a v2 message of the moderated channel as p and registers its id
// and text.
func (m *mod) post(p *member, what string, req schema.PostMessageRequestV2) (int64, error) {
	var resp schema.PostMessageResponseV2
	if err := p.api.call(http.MethodPost, "/v2/channels/"+url.PathEscape(m.main)+"/messages", req, &resp); err != nil {
		return 0, err
	}
	m.marks.id("message", what, resp.Message.ID)
	return resp.Message.ID, nil
}

func (m *mod) remove(a api, id int64) (result, error) {
	return m.w.send(a, del("/v2/channels/"+url.PathEscape(m.main)+"/messages/"+itoa(id)))
}

// list reads the moderated channel as a, v2, by path suffix.
func (m *mod) listV2(a api, query string) ([]schema.MessageV2, []byte, error) {
	status, body, err := a.do(http.MethodGet, "/v2/channels/"+url.PathEscape(m.main)+"/messages"+query, nil)
	if err != nil {
		return nil, nil, err
	}
	if status != http.StatusOK {
		return nil, body, fmt.Errorf("GET messages%s as %s = %d %s", query, a.who, status, strings.TrimSpace(string(body)))
	}
	var page schema.ListMessagesResponseV2
	if err := json.Unmarshal(body, &page); err != nil {
		return nil, body, err
	}
	return page.Messages, body, nil
}

func (m *mod) search(a api, q string, limit int, before int64) (schema.SearchMessagesResponseV2, []byte, error) {
	return m.searchIn(m.main, a, q, limit, before)
}

func (m *mod) searchIn(channel string, a api, q string, limit int, before int64) (schema.SearchMessagesResponseV2, []byte, error) {
	v := url.Values{"q": {q}, "limit": {itoa(int64(limit))}}
	if before != 0 {
		v.Set("before", itoa(before))
	}
	status, body, err := a.do(http.MethodGet, "/v2/channels/"+url.PathEscape(channel)+"/messages/search?"+v.Encode(), nil)
	var out schema.SearchMessagesResponseV2
	if err != nil {
		return out, nil, err
	}
	if status != http.StatusOK {
		return out, body, fmt.Errorf("search as %s = %d %s", a.who, status, strings.TrimSpace(string(body)))
	}
	return out, body, json.Unmarshal(body, &out)
}

// removedState is what the scenario put in and what it removed.
type removedState struct {
	f1, f2         int64
	file1, text1   string // the removed message's file name and contents
	m1, m2, m3     int64  // wide: removed, its reply, a control
	m4, m5, m6     int64  // whispers to mia, to wes, a net message
	r1, r2, r3     string
	r4, r5, r6     string
	term           string
	s              []int64 // the messages that match the term, oldest first
	originalsWide  []string
	originalsScope []string
}

func (m *mod) messages() error {
	w := m.w
	st := &removedState{}
	st.r1, st.r2, st.r3 = m.text("removed body"), m.text("reply body"), m.text("control body")
	st.r4, st.r5, st.r6 = m.text("whisper to the manager"), m.text("whisper to wes"), m.text("net message")
	st.term = m.text("search term")
	st.file1, st.text1 = m.text("file name")+".txt", m.text("file contents")
	pat, wes, mia := m.pat, m.wes, m.mia

	// Everyone who may read the channel holds every socket variant before a
	// word is said: the originals are seen live, so the removals are updates.
	// zoe is a member who is on no net and is sent no whisper.
	zoe, err := m.person(m.pname("zoe"), m.main, m.side)
	if err != nil {
		return err
	}
	readers := []*reader{{p: pat}, {p: wes}, {p: mia}, {p: zoe}}
	for _, r := range readers {
		feeds, err := openAll(r.p, m.main)
		if err != nil {
			return err
		}
		r.feeds = feeds
	}
	sideFeeds, err := openAll(m.ned, m.side)
	if err != nil {
		return err
	}
	all := func() []*feed {
		var out []*feed
		for _, r := range readers {
			out = append(out, r.feeds...)
		}
		return out
	}

	// What is said.
	if st.f1, err = m.upload(pat, st.file1, st.text1); err != nil {
		return err
	}
	m.marks.id("file", "the removed message's file", st.f1)
	if st.f2, err = m.upload(pat, m.text("kept file")+".txt", m.text("kept file contents")); err != nil {
		return err
	}
	if st.m1, err = m.post(pat, "the message to be removed", attachment(st.r1, st.f1)); err != nil {
		return err
	}
	if st.m2, err = m.post(wes, "a reply to it", schema.PostMessageRequestV2{Body: st.r2, ThreadRootID: st.m1}); err != nil {
		return err
	}
	if st.m4, err = m.post(pat, "a whisper to the manager", schema.PostMessageRequestV2{Body: st.r4,
		Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{mia.id}}}); err != nil {
		return err
	}
	if st.m5, err = m.post(pat, "a whisper to wes", schema.PostMessageRequestV2{Body: st.r5,
		Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{wes.id}}}); err != nil {
		return err
	}
	if st.m6, err = m.post(pat, "a net's message", schema.PostMessageRequestV2{Body: st.r6,
		Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: m.netID}}); err != nil {
		return err
	}
	for i := range 4 {
		id, err := m.post(pat, fmt.Sprintf("a message with the search term %d", i), schema.PostMessageRequestV2{Body: fmt.Sprintf("%s number %d", st.term, i)})
		if err != nil {
			return err
		}
		st.s = append(st.s, id)
	}
	if st.m3, err = m.post(pat, "the control message", schema.PostMessageRequestV2{Body: st.r3}); err != nil {
		return err
	}
	// Each message socket has carried the control, and so everything before.
	for _, r := range readers {
		for _, f := range r.feeds {
			if k := kindOf(f); k == kindMessageV0 || k == kindMessageV2 {
				if err := waitFor(f.what+" to carry the control", modWait, func() (bool, string) {
					for _, fr := range f.received() {
						if bytes.Contains(fr, []byte(st.r3)) {
							return true, ""
						}
					}
					return false, fmt.Sprintf("%d frames", len(f.received()))
				}); err != nil {
					return err
				}
			}
		}
	}
	st.originalsWide = []string{st.r1, st.text1, st.file1}
	st.originalsScope = []string{st.r4}

	// The control for search: what the term finds before anything is removed.
	before, _, err := m.search(wes.api, st.term, 2, 0)
	if err != nil {
		return err
	}
	if len(before.Messages) != 2 || before.NextBefore == 0 || before.Messages[0].ID != st.s[3] || before.Messages[1].ID != st.s[2] {
		return fmt.Errorf("before any removal, search for the term (limit 2) is %+v, want the newest two with a next page", before)
	}

	// ---- who may remove what

	n := 0
	// A plain member, and an agent member, are refused before the message is
	// looked up; a manager who cannot see the message is told what she is told
	// for an id nobody issued (§9 case 7); an operator outside the channel and
	// everyone outside it are told the channel does not exist.
	ghostMsg := w.ghost().id
	for _, c := range []struct {
		who  string
		a    api
		id   int64
		want answer
	}{
		{"a plain member", pat.api, st.m1, operatorOnly},
		{"an agent member", m.agent.api, st.m1, operatorOnly},
		{"the manager, of a whisper to wes", mia.api, st.m5, messageNotFound},
		{"the manager, of a net message she has no seat in", mia.api, st.m6, messageNotFound},
		{"the operator, who is not a member", w.op, st.m1, channelNotFound},
		{"an observer of the side channel", m.ned.api, st.m1, channelNotFound},
		{"B's manager", w.b.people[0].api, st.m1, channelNotFound},
		{"a B member", w.b.people[1].api, st.m1, channelNotFound},
	} {
		got, err := m.remove(c.a, c.id)
		if err != nil {
			return err
		}
		as := asked{strs: []string{m.main}, ids: []kindID{{"message", c.id}}}
		if leaks := m.marks.leaks(got.body, as); len(leaks) > 0 {
			return fmt.Errorf("%s removing: the body holds %s", c.who, strings.Join(leaks, "; "))
		}
		if leaks := m.marks.headerLeaks(got.header, as); len(leaks) > 0 {
			return fmt.Errorf("%s removing: the headers hold %s", c.who, strings.Join(leaks, "; "))
		}
		if err := checkRefusal(got, c.want, as); err != nil {
			return fmt.Errorf("%s removing: %w", c.who, err)
		}
		g := w.ghost()
		var ghost result
		switch c.want {
		case channelNotFound:
			status, body, err := c.a.do(http.MethodDelete, "/v2/channels/"+g.name+"/messages/"+itoa(c.id), nil)
			if err != nil {
				return err
			}
			ghost = result{status: status, body: body}
			if ghost.status != got.status || !bytes.Equal(scrub(ghost.body, []string{g.name}, c.id), scrub(got.body, []string{m.main}, c.id)) {
				return fmt.Errorf("%s removing: %d %s, and in a channel that never existed %d %s", c.who, got.status, got.body, ghost.status, ghost.body)
			}
		case messageNotFound, operatorOnly:
			ghost, err = m.remove(c.a, ghostMsg)
			if err != nil {
				return err
			}
			if ghost.status != got.status || !bytes.Equal(scrub(ghost.body, nil, ghostMsg), scrub(got.body, nil, c.id)) {
				return fmt.Errorf("%s removing: %d %s, and for an id nobody issued %d %s: the difference tells her it exists", c.who, got.status, got.body, ghost.status, ghost.body)
			}
		}
		n += 2
	}
	// B's manager reaches the same message through her own channel: not hers.
	bea := w.b.people[0]
	got, err := m.w.send(bea.api, del("/v2/channels/"+url.PathEscape(w.b.main())+"/messages/"+itoa(st.m1)))
	if err != nil {
		return err
	}
	bAs := asked{strs: []string{w.b.main()}, ids: []kindID{{"message", st.m1}}}
	if leaks := m.marks.leaks(got.body, bAs); len(leaks) > 0 {
		return fmt.Errorf("the manager of B removing it through her own channel: the body holds %s", strings.Join(leaks, "; "))
	}
	if leaks := m.marks.headerLeaks(got.header, bAs); len(leaks) > 0 {
		return fmt.Errorf("the manager of B removing it through her own channel: the headers hold %s", strings.Join(leaks, "; "))
	}
	if err := checkRefusal(got, messageNotFound, bAs); err != nil {
		return fmt.Errorf("the manager of B removing it through her own channel: %w", err)
	}
	gh, err := m.w.send(bea.api, del("/v2/channels/"+url.PathEscape(w.b.main())+"/messages/"+itoa(ghostMsg)))
	if err != nil {
		return err
	}
	if gh.status != got.status || !bytes.Equal(scrub(gh.body, nil, ghostMsg), scrub(got.body, nil, st.m1)) {
		return fmt.Errorf("the manager of B removing it through her own channel: %d %s, for an id nobody issued %d %s", got.status, got.body, gh.status, gh.body)
	}
	// Cross-group: everything an outsider can ask about the channel, its
	// messages and its file, with the real ids.
	for _, o := range []struct {
		who  string
		a    api
		side bool
	}{{"bea (B)", bea.api, false}, {"bob (B)", w.b.people[1].api, false}, {"ned, of the side channel", m.ned.api, true}, {"the operator's side: ola (B)", w.b.people[2].api, false}} {
		k, err := m.outAs(o.a, o.who, st.m1, st.f1, st.term)
		n += k
		if err != nil {
			return fmt.Errorf("%s: %w", o.who, err)
		}
		k, err = m.socketsRefused(o.a, o.who, o.side)
		n += k
		if err != nil {
			return err
		}
	}
	// Nothing was removed by all that.
	if all, _, err := m.listV2(wes.api, "?after=0&limit=100"); err != nil {
		return err
	} else {
		for _, msg := range all {
			if msg.Removed {
				return fmt.Errorf("a refused removal removed message %d", msg.ID)
			}
		}
	}

	// What the agent member of the channel reads over MCP before anything is
	// removed: the channel-wide messages only, no whisper or net message it
	// has no part in.
	mcp, err := m.agentSession()
	if err != nil {
		return err
	}
	mcpBefore, err := m.mcpRead(mcp)
	if err != nil {
		return err
	}
	for _, id := range []int64{st.m4, st.m5, st.m6} {
		if _, seen := mcpBefore.byID(id); seen {
			return fmt.Errorf("before any removal the agent reads message %d over MCP, a message it has no part in", id)
		}
	}
	if _, seen := mcpBefore.byID(st.m1); !seen {
		return fmt.Errorf("before the removal the agent does not read message %d over MCP", st.m1)
	}

	// ---- a scoped message: the whisper to the manager

	base := feedCounts(all())
	res, err := m.remove(mia.api, st.m4)
	if err != nil {
		return err
	}
	if res.status != http.StatusNoContent {
		return fmt.Errorf("the manager removing the whisper sent to her: %d %s", res.status, res.body)
	}
	owedScoped := func(f *feed) want {
		for _, r := range readers {
			if (r.p == pat || r.p == mia) && slices.Contains(r.feeds, f) {
				return scopedWant(f)
			}
		}
		return wantNothing
	}
	if err := m.checkRemoval("the whisper to the manager", all(), base, st.m4, owedScoped, st.r4); err != nil {
		return err
	}
	// The notification path and the other sockets heard nothing: wes's
	// sockets of every family carried nothing about it, and B's, scanned at
	// the end, hold nothing of M's.
	rows, err := m.auditCount(store.AuditMessageRemoved, fmt.Sprintf("message:%d", st.m4), fmt.Sprintf("channel=%d", m.mainID))
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%d message_removed rows for the whisper, want 1 (subject message:id, detail channel=id)", rows)
	}

	// ---- a channel-wide message

	base = feedCounts(all())
	if res, err = m.remove(mia.api, st.m1); err != nil {
		return err
	}
	if res.status != http.StatusNoContent {
		return fmt.Errorf("the manager removing a message: %d %s", res.status, res.body)
	}
	if err := m.checkRemoval("the message with an attachment", all(), base, st.m1, func(f *feed) want { return wideWant(f) }, st.originalsWide...); err != nil {
		return err
	}
	// A repeat is 204, writes nothing and sends nothing.
	base = feedCounts(all())
	if res, err = m.remove(mia.api, st.m1); err != nil {
		return err
	}
	if res.status != http.StatusNoContent {
		return fmt.Errorf("a repeated removal: %d %s", res.status, res.body)
	}
	time.Sleep(1500 * time.Millisecond)
	for _, f := range all() {
		if got := len(f.since(base[f])); got != 0 {
			return fmt.Errorf("%s carried %d frames for a repeated removal, want none", f.what, got)
		}
	}
	if rows, err = m.auditCount(store.AuditMessageRemoved, fmt.Sprintf("message:%d", st.m1), ""); err != nil || rows != 1 {
		return fmt.Errorf("%d message_removed rows for the message after a repeat, want 1 (%v)", rows, errText(err))
	}

	// ---- the agent's MCP read

	if err := m.mcpTombstone(mcp, mcpBefore, st); err != nil {
		return fmt.Errorf("MCP read_channel after the removal: %w", err)
	}

	// ---- every surface

	k, err := m.surfaces(st)
	n += k
	if err != nil {
		return err
	}

	// ---- a reconnect and another credential

	if err := m.reconnects(st); err != nil {
		return err
	}

	// ---- replay and live, ordered

	if err := m.ordering(); err != nil {
		return err
	}

	// ---- a net's message

	if err := m.netRemoval(readers, all); err != nil {
		return err
	}

	// Observers of the side channel: none of what was said or removed.
	for _, f := range sideFeeds {
		for i, fr := range f.received() {
			if leaks := m.marks.leaks(fr, asked{}); len(leaks) > 0 {
				return fmt.Errorf("frame %d on %s holds %s", i, f.what, strings.Join(leaks, "; "))
			}
		}
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s closed", f.what)
		}
	}
	for _, f := range all() {
		f.close()
	}
	for _, f := range sideFeeds {
		f.close()
	}
	m.say("message removal: the manager removed a whisper sent to her and then a channel-wide message with an attachment while three people held every socket variant: each v2 socket of the whisper's two people got exactly one removed:true envelope, no v0, v1 or other person's socket got any frame about it, the wide message reached every v0, v1 and v2 socket as the fixed body (v2 with removed) and every events socket as %s, once, no frame after held an original, nobody was disconnected, a repeat sent nothing and wrote nothing; a plain member was refused, the manager was told message_not_found for a whisper and a net message she cannot see exactly as for an id nobody issued, and the operator outside the channel, B's manager and members, and a member of another channel were told the channel does not exist exactly as for a ghost (%d requests)", schema.MessageRemovedSchemaV1, n)
	return nil
}

// agentSession opens the gates' agent member of the moderated channel on /mcp.
func (m *mod) agentSession() (*mcpclient.Client, error) {
	ctx, cancel := context.WithTimeout(stopCtx, time.Minute)
	defer cancel()
	c := mcpclient.New(m.w.d.baseURL, m.agent.token)
	m.h.called(http.MethodPost, "/mcp")
	if err := c.Initialize(ctx, "isolation-check"); err != nil {
		return nil, fmt.Errorf("MCP initialize as the moderated channel's agent: %w", err)
	}
	return c, nil
}

// mcpPage is every message one MCP read_channel read of the moderated channel
// returned, and the text of the answers.
type mcpPage struct {
	messages []schema.MessageV2
	raw      []byte
}

func (p mcpPage) byID(id int64) (schema.MessageV2, bool) {
	for _, msg := range p.messages {
		if msg.ID == id {
			return msg, true
		}
	}
	return schema.MessageV2{}, false
}

// mcpRead reads the moderated channel from the start, page by page, with the
// read_channel tool.
func (m *mod) mcpRead(c *mcpclient.Client) (mcpPage, error) {
	ctx, cancel := context.WithTimeout(stopCtx, time.Minute)
	defer cancel()
	var out mcpPage
	var after int64
	for range 50 {
		raw, err := c.CallTool(ctx, "read_channel", map[string]any{"channel": m.main, "after": after, "limit": 100})
		if err != nil {
			return out, fmt.Errorf("read_channel as the agent: %w", err)
		}
		page, err := mcpclient.Decode[schema.ListMessagesResponseV2](raw)
		if err != nil {
			return out, err
		}
		out.raw = append(out.raw, raw...)
		out.messages = append(out.messages, page.Messages...)
		if page.NextAfter == 0 {
			return out, nil
		}
		after = page.NextAfter
	}
	return out, fmt.Errorf("read_channel never reached the end of the channel")
}

// mcpTombstone is the row of issue #1468: the message with an attachment has
// been removed, and the agent reads it over MCP as the tombstone REST returns
// for it, field for field: the fixed body, the author kept, no payload (so no
// attachment) and nothing of the original anywhere in the answer. What the
// agent could not read before it still cannot, and the rest of the channel
// is what it was.
func (m *mod) mcpTombstone(c *mcpclient.Client, before mcpPage, st *removedState) error {
	after, err := m.mcpRead(c)
	if err != nil {
		return err
	}
	for _, o := range st.originalsWide {
		if bytes.Contains(after.raw, []byte(o)) {
			return fmt.Errorf("the answer holds the removed message's %q", o)
		}
	}
	if bytes.Contains(after.raw, []byte(st.r4)) {
		return fmt.Errorf("the answer holds the removed whisper's text")
	}
	for _, id := range []int64{st.m4, st.m5, st.m6} {
		if _, seen := after.byID(id); seen {
			return fmt.Errorf("the agent reads message %d, which it could not read before", id)
		}
	}
	tomb, ok := after.byID(st.m1)
	if !ok {
		return fmt.Errorf("the agent reads no tombstone for message %d", st.m1)
	}
	if !tomb.Removed || tomb.Body != schema.MessageRemovedBody || tomb.Payload != nil || tomb.AuthorID != m.pat.id {
		return fmt.Errorf("the tombstone is %+v: want the fixed body, no payload, the author kept", tomb)
	}
	if err := tomb.Validate(); err != nil {
		return fmt.Errorf("the tombstone does not validate: %w", err)
	}
	rest, _, err := m.listV2(m.agent.api, "?after=0&limit=100")
	if err != nil {
		return err
	}
	var viaREST schema.MessageV2
	for _, msg := range rest {
		if msg.ID == st.m1 {
			viaREST = msg
		}
	}
	if viaREST.ID == 0 {
		return fmt.Errorf("REST returns the agent no tombstone for message %d", st.m1)
	}
	if !reflect.DeepEqual(tomb, viaREST) {
		return fmt.Errorf("MCP returns the tombstone as %+v, and REST as %+v", tomb, viaREST)
	}
	// The same messages, in the same order, as REST gives the agent; and the
	// same ones as before but for the removed one.
	if len(after.messages) != len(rest) {
		return fmt.Errorf("MCP returns %d messages and REST %d", len(after.messages), len(rest))
	}
	for i, msg := range after.messages {
		if msg.ID != rest[i].ID {
			return fmt.Errorf("MCP returns message %d in place %d, and REST %d", msg.ID, i, rest[i].ID)
		}
	}
	if len(after.messages) != len(before.messages) {
		return fmt.Errorf("the agent read %d messages before the removals and %d after", len(before.messages), len(after.messages))
	}
	for i, msg := range after.messages {
		if msg.ID != before.messages[i].ID {
			return fmt.Errorf("the agent's read changed: message %d where %d was", msg.ID, before.messages[i].ID)
		}
	}
	m.say("mcp tombstone: the agent member of the channel read the removed message over MCP read_channel as the tombstone REST returns for it (the fixed body, the author kept, no payload and so no attachment, nothing of the original in the answer), the whisper to the manager, the whisper to wes and the net message were no more visible to it after than before, and the channel read as it did before but for that one message")
	return nil
}

// do2 is do, for a request with no body, answering a result.
func (a api) do2(method, path string) (result, error) {
	status, body, err := a.do(method, path, nil)
	return result{status: status, body: body}, err
}

// surfaces reads every history, thread, search and file surface as each
// reader and as someone who was never told of the whisper.
func (m *mod) surfaces(st *removedState) (int, error) {
	n := 0
	alt, err := m.altCredential(m.wes)
	if err != nil {
		return n, err
	}
	type who struct {
		name    string
		a       api
		scoped  bool // may read the whisper to the manager
		reader  *member
		fileOwn bool
	}
	people := []who{
		{"wes", m.wes.api, false, m.wes, false},
		{"wes on another credential", alt.api, false, m.wes, false},
		{"pat", m.pat.api, true, m.pat, true},
		{"the manager", m.mia.api, true, m.mia, false},
	}
	for _, p := range people {
		tomb := func(msgs []schema.MessageV2, id int64) (schema.MessageV2, bool) {
			for _, msg := range msgs {
				if msg.ID == id {
					return msg, true
				}
			}
			return schema.MessageV2{}, false
		}
		// v2: forward, backward, the thread.
		for _, q := range []string{"?after=0&limit=100", "?order=desc&limit=100", "?thread=" + itoa(st.m1)} {
			msgs, body, err := m.listV2(p.a, q)
			n++
			if err != nil {
				return n, err
			}
			for _, o := range st.originalsWide {
				if bytes.Contains(body, []byte(o)) {
					return n, fmt.Errorf("%s: GET messages%s holds the removed message's %q", p.name, q, o)
				}
			}
			if bytes.Contains(body, []byte(st.r4)) {
				return n, fmt.Errorf("%s: GET messages%s holds the removed whisper's text", p.name, q)
			}
			one, ok := tomb(msgs, st.m1)
			if !ok {
				return n, fmt.Errorf("%s: GET messages%s has no tombstone for the removed message", p.name, q)
			}
			if !one.Removed || one.Body != schema.MessageRemovedBody || one.Payload != nil || one.AuthorID != m.pat.id {
				return n, fmt.Errorf("%s: the tombstone is %+v: want the fixed body, no payload, the author kept", p.name, one)
			}
			if err := one.Validate(); err != nil {
				return n, fmt.Errorf("%s: the tombstone does not validate: %w", p.name, err)
			}
			if q != "?thread="+itoa(st.m1) {
				if _, ok := tomb(msgs, st.m2); !ok {
					return n, fmt.Errorf("%s: the reply to the removed message is gone", p.name)
				}
			} else if reply, ok := tomb(msgs, st.m2); !ok || reply.Removed || reply.Body != st.r2 {
				return n, fmt.Errorf("%s: the thread of the removed root does not keep its reply: %+v", p.name, reply)
			}
			// The whisper: a tombstone for its two people, nothing at all
			// for anyone else.
			wh, seen := tomb(msgs, st.m4)
			switch {
			case q == "?thread="+itoa(st.m1):
			case p.scoped && (!seen || !wh.Removed || wh.Body != schema.MessageRemovedBody):
				return n, fmt.Errorf("%s: GET messages%s has no tombstone for the whisper she was sent: %+v", p.name, q, wh)
			case !p.scoped && (seen || bytes.Contains(body, []byte(itoa(st.m4)+","))):
				return n, fmt.Errorf("%s: GET messages%s names a whisper she was not sent: %s", p.name, q, body)
			}
			// The two others she may read are as they were.
			if q != "?thread="+itoa(st.m1) {
				if c, ok := tomb(msgs, st.m3); !ok || c.Removed || c.Body != st.r3 {
					return n, fmt.Errorf("%s: the control message is %+v", p.name, c)
				}
			}
		}
		// v0 and v1: the fixed body on the wide message's own id, no marker,
		// and no scoped message at all.
		for _, v := range []string{"/v0/channels/", "/v1/channels/"} {
			status, body, err := p.a.do(http.MethodGet, v+url.PathEscape(m.main)+"/messages?limit=100", nil)
			n++
			if err != nil {
				return n, err
			}
			if status != http.StatusOK {
				return n, fmt.Errorf("%s: GET %smessages = %d", p.name, v, status)
			}
			for _, o := range append(append([]string{}, st.originalsWide...), st.r4) {
				if bytes.Contains(body, []byte(o)) {
					return n, fmt.Errorf("%s: GET %smessages holds %q", p.name, v, o)
				}
			}
			var msgs []schema.MessageV1
			switch v {
			case "/v0/channels/":
				var page schema.ListMessagesResponse
				if err := json.Unmarshal(body, &page); err != nil {
					return n, fmt.Errorf("%s: decode GET %smessages: %w", p.name, v, err)
				}
				for _, msg := range page.Messages {
					msgs = append(msgs, schema.MessageV1{ID: msg.ID, Body: msg.Body})
				}
			case "/v1/channels/":
				var page schema.ListMessagesResponseV1
				if err := json.Unmarshal(body, &page); err != nil {
					return n, fmt.Errorf("%s: decode GET %smessages: %w", p.name, v, err)
				}
				msgs = page.Messages
			}
			for _, scoped := range []int64{st.m4, st.m5, st.m6} {
				if slices.ContainsFunc(msgs, func(msg schema.MessageV1) bool { return msg.ID == scoped }) {
					return n, fmt.Errorf("%s: GET %smessages names scoped message %d", p.name, v, scoped)
				}
			}
			found := false
			for _, msg := range msgs {
				if msg.ID == st.m1 {
					found = true
					if msg.Body != schema.MessageRemovedBody || msg.Payload != nil {
						return n, fmt.Errorf("%s: GET %smessages has message %d as %+v, want the fixed body and no payload", p.name, v, st.m1, msg)
					}
				}
			}
			if !found {
				return n, fmt.Errorf("%s: GET %smessages has no tombstone for removed message %d", p.name, v, st.m1)
			}
			if bytes.Contains(body, []byte(`"removed"`)) {
				return n, fmt.Errorf("%s: GET %smessages carries a removed marker an older reader did not ask for", p.name, v)
			}
		}
		// Search: the removed words find nothing, and the term's pages are
		// whole (limit and cursor never see the removed row).
		for _, q := range []string{st.r1, st.text1, st.r1 + " " + st.r2} {
			res, body, err := m.search(p.a, q, 20, 0)
			n++
			if err != nil {
				return n, err
			}
			for _, msg := range res.Messages {
				if msg.ID == st.m1 {
					return n, fmt.Errorf("%s: search for %q found the removed message", p.name, q)
				}
			}
			if bytes.Contains(body, []byte(st.r1)) {
				return n, fmt.Errorf("%s: search for %q holds the removed text", p.name, q)
			}
		}
		if p.scoped {
			res, body, err := m.search(p.a, st.r4, 20, 0)
			n++
			if err != nil {
				return n, err
			}
			if len(res.Messages) != 0 || bytes.Contains(body, []byte(st.r4)) {
				return n, fmt.Errorf("%s: search for the removed whisper found %s", p.name, body)
			}
		}
		// Files: the removed message's attachment is 404 file_not_found for
		// everyone, the uploader included, exactly as for a file nobody
		// uploaded; the unattached upload is the uploader's still.
		ghost := m.w.ghost().id
		for _, route := range []string{"/v2/files/%d/meta", "/v2/files/%d"} {
			got, err := p.a.do2(http.MethodGet, fmt.Sprintf(route, st.f1))
			n++
			if err != nil {
				return n, err
			}
			if err := checkRefusal(got, fileNotFound, asked{ids: []kindID{{"file", st.f1}}}); err != nil {
				return n, fmt.Errorf("%s: GET %s of the removed message's file: %w", p.name, route, err)
			}
			if bytes.Contains(got.body, []byte(st.text1)) {
				return n, fmt.Errorf("%s: the refusal of the removed file holds its contents", p.name)
			}
			gh, err := p.a.do2(http.MethodGet, fmt.Sprintf(route, ghost))
			if err != nil {
				return n, err
			}
			if gh.status != got.status || !bytes.Equal(scrub(gh.body, nil, ghost), scrub(got.body, nil, st.f1)) {
				return n, fmt.Errorf("%s: the removed file is %d %s, a file nobody uploaded %d %s", p.name, got.status, got.body, gh.status, gh.body)
			}
		}
		if p.fileOwn {
			status, body, err := p.a.do(http.MethodGet, fmt.Sprintf("/v2/files/%d", st.f2), nil)
			n++
			if err != nil || status != http.StatusOK || len(body) == 0 {
				return n, fmt.Errorf("the uploader's unattached file: %d %v", status, errText(err))
			}
		}
		// A new reply to the removed root: thread_not_found.
		reply, err := m.replyTo(p.a, st.m1)
		n++
		if err != nil {
			return n, err
		}
		if err := checkRefusal(reply, answer{http.StatusNotFound, "thread_not_found"}, asked{ids: []kindID{{"message", st.m1}}}); err != nil {
			return n, fmt.Errorf("%s replying to the removed root: %w", p.name, err)
		}
	}
	// The search pages: remove the newest match and the pages are the same as
	// if it had never been there.
	wes := m.wes.api
	if res, err := m.remove(m.mia.api, st.s[3]); err != nil || res.status != http.StatusNoContent {
		return n, fmt.Errorf("removing the newest match: %d %v", res.status, errText(err))
	}
	p1, _, err := m.search(wes, st.term, 2, 0)
	if err != nil {
		return n, err
	}
	if len(p1.Messages) != 2 || p1.Messages[0].ID != st.s[2] || p1.Messages[1].ID != st.s[1] || p1.NextBefore != st.s[1] {
		return n, fmt.Errorf("after the newest match was removed, page 1 of the term (limit 2) is %+v, want the next two (%d, %d) with a next page %d", p1, st.s[2], st.s[1], st.s[1])
	}
	p2, _, err := m.search(wes, st.term, 2, p1.NextBefore)
	if err != nil {
		return n, err
	}
	if len(p2.Messages) != 1 || p2.Messages[0].ID != st.s[0] || p2.NextBefore != 0 {
		return n, fmt.Errorf("after the newest match was removed, page 2 is %+v, want %d alone and no next page", p2, st.s[0])
	}
	// The unseen messages stay what they were for the others: wes still reads
	// the whisper to him and the net message, and the manager neither.
	wmsgs, _, err := m.listV2(wes, "?after=0&limit=100")
	if err != nil {
		return n, err
	}
	have := map[int64]schema.MessageV2{}
	for _, msg := range wmsgs {
		have[msg.ID] = msg
	}
	if x := have[st.m5]; x.Removed || x.Body != st.r5 {
		return n, fmt.Errorf("the whisper to wes, which the manager could not remove, is %+v", x)
	}
	if x := have[st.m6]; x.Removed || x.Body != st.r6 {
		return n, fmt.Errorf("the net message, which the manager could not remove, is %+v", x)
	}
	mmsgs, _, err := m.listV2(m.mia.api, "?after=0&limit=100")
	if err != nil {
		return n, err
	}
	for _, msg := range mmsgs {
		if msg.ID == st.m5 || msg.ID == st.m6 {
			return n, fmt.Errorf("the manager can read message %d, which was said to others", msg.ID)
		}
	}
	return n, nil
}

// replyTo posts a reply to root as a.
func (m *mod) replyTo(a api, root int64) (result, error) {
	status, body, err := a.do(http.MethodPost, "/v2/channels/"+url.PathEscape(m.main)+"/messages", schema.PostMessageRequestV2{Body: m.text("a reply to a removed root"), ThreadRootID: root})
	return result{status: status, body: body}, err
}

// reconnects: new sockets, and another credential, are told the same
// tombstone through history and replay, never the original, and a person who
// was never sent the whisper is told nothing of it.
func (m *mod) reconnects(st *removedState) error {
	for _, c := range []struct {
		p      *member
		scoped bool
		alt    bool
	}{{m.wes, false, false}, {m.wes, false, true}, {m.pat, true, false}, {m.mia, true, false}} {
		a := c.p.api
		if c.alt {
			alt, err := m.altCredential(c.p)
			if err != nil {
				return err
			}
			a = alt.api
		}
		var feeds []*feed
		for _, v := range socketVariants {
			if v.path == "/v1/voice/ws" {
				continue
			}
			q := url.Values{"channel": {m.main}}
			if v.path == "/v2/ws" {
				q.Set("after", "0")
			}
			f, r, err := dialSocket(a, fmt.Sprintf("%s's reconnect %s (ticket=%v)", a.who, v.path, v.ticket), v.path, q, v.ticket)
			if err != nil {
				return err
			}
			if f == nil {
				return fmt.Errorf("%s's reconnect on %s: %d %s", a.who, v.path, r.status, r.body)
			}
			feeds = append(feeds, f)
		}
		for _, f := range feeds {
			if kindOf(f) != kindMessageV2 {
				continue
			}
			if err := waitFor(f.what+" to replay up to the control", modWait, func() (bool, string) {
				for _, fr := range f.received() {
					if bytes.Contains(fr, []byte(st.r3)) {
						return true, ""
					}
				}
				return false, fmt.Sprintf("%d frames", len(f.received()))
			}); err != nil {
				return err
			}
		}
		select {
		case <-time.After(time.Second):
		case <-stopCtx.Done():
			return errInterrupted
		}
		for _, f := range feeds {
			tombs, wh, whTombs := 0, 0, 0
			for i, fr := range f.received() {
				df := decodeFrame(fr)
				for _, o := range append(append([]string{}, st.originalsWide...), st.r4, fmt.Sprintf("%s number 3", st.term)) {
					if bytes.Contains(fr, []byte(o)) {
						return fmt.Errorf("frame %d on %s holds the original %q", i, f.what, o)
					}
				}
				if df.tombstone(st.m1) {
					tombs++
				}
				if df.names(st.m4) {
					wh++
				}
				if df.tombstone(st.m4) {
					whTombs++
				}
			}
			switch kindOf(f) {
			case kindMessageV2:
				if tombs != 1 {
					return fmt.Errorf("%s replayed the removed message %d times, want one tombstone", f.what, tombs)
				}
				if c.scoped && (wh != 1 || whTombs != 1) || !c.scoped && wh != 0 {
					return fmt.Errorf("%s replayed the removed whisper in %d frames (it may read it: %v)", f.what, wh, c.scoped)
				}
			default:
				if wh != 0 {
					return fmt.Errorf("%s was sent the removed whisper", f.what)
				}
			}
			f.close()
		}
	}
	return nil
}

// ordering posts and removes messages while sockets that replay from the
// start connect and run: each socket sees, for every message, an original
// then its tombstone once, or the tombstone alone, never a tombstone and
// then the original, and never a second tombstone.
func (m *mod) ordering() error {
	const count = 30
	wes := m.wes
	alt, err := m.altCredential(wes)
	if err != nil {
		return err
	}
	var mu sync.Mutex // guards ids and texts
	ids := make([]int64, 0, count)
	texts := map[int64]string{}
	errc := make(chan error, 1)
	go func() {
		for i := range count {
			body := m.text(fmt.Sprintf("ordered message %d", i))
			id, err := m.post(m.pat, "an ordered message", schema.PostMessageRequestV2{Body: body})
			if err != nil {
				errc <- err
				return
			}
			mu.Lock()
			ids = append(ids, id)
			texts[id] = body
			mu.Unlock()
			if i%3 != 2 {
				continue
			}
			// Three in each three are removed at once, newest first, so that
			// a socket reading a page sees removals landing behind it.
			mu.Lock()
			batch := slices.Clone(ids[len(ids)-3:])
			mu.Unlock()
			for j := len(batch) - 1; j >= 0; j-- {
				if res, err := m.remove(m.mia.api, batch[j]); err != nil || res.status != http.StatusNoContent {
					errc <- fmt.Errorf("removing %d: %d %v", batch[j], res.status, errText(err))
					return
				}
			}
		}
		errc <- nil
	}()
	var feeds []*feed
	for _, c := range []struct {
		a      api
		ticket bool
	}{{wes.api, false}, {wes.api, true}, {alt.api, false}} {
		for k := 0; k < 2; k++ {
			time.Sleep(time.Duration(k*40) * time.Millisecond)
			f, r, err := dialSocket(c.a, fmt.Sprintf("%s's replaying /v2/ws #%d (ticket=%v)", c.a.who, k, c.ticket), "/v2/ws", url.Values{"channel": {m.main}, "after": {"0"}}, c.ticket)
			if err != nil {
				return err
			}
			if f == nil {
				return fmt.Errorf("a replaying socket was refused: %d %s", r.status, r.body)
			}
			feeds = append(feeds, f)
		}
	}
	if err := <-errc; err != nil {
		return err
	}
	// Every message is removed: count is a multiple of three.
	for _, f := range feeds {
		if err := waitFor(f.what+" to carry every tombstone", modWait, func() (bool, string) {
			seen := 0
			for _, fr := range f.received() {
				df := decodeFrame(fr)
				mu.Lock()
				_, ours := texts[df.ID]
				mu.Unlock()
				if ours && df.Body == schema.MessageRemovedBody {
					seen++
				}
			}
			return seen >= count, fmt.Sprintf("%d of %d tombstones", seen, count)
		}); err != nil {
			return err
		}
	}
	time.Sleep(time.Second)
	for _, f := range feeds {
		state := map[int64]string{} // "" none, "orig", "tomb"
		for i, fr := range f.received() {
			df := decodeFrame(fr)
			mu.Lock()
			text, ours := texts[df.ID]
			mu.Unlock()
			if !ours {
				continue
			}
			switch {
			case df.tombstone(df.ID):
				if state[df.ID] == "tomb" {
					return fmt.Errorf("%s: frame %d is a second tombstone for message %d", f.what, i, df.ID)
				}
				state[df.ID] = "tomb"
			case df.Body == text:
				if state[df.ID] != "" {
					return fmt.Errorf("%s: frame %d is the original of message %d after its %s", f.what, i, df.ID, state[df.ID])
				}
				state[df.ID] = "orig"
			default:
				return fmt.Errorf("%s: frame %d about message %d is neither its original nor its tombstone: %s", f.what, i, df.ID, fr)
			}
		}
		for _, id := range ids {
			if state[id] != "tomb" {
				return fmt.Errorf("%s ended with message %d in state %q, want its tombstone", f.what, id, state[id])
			}
		}
		f.close()
	}
	m.say("ordering: %d messages posted and removed in threes while six v2 sockets that replay from the start (on two credentials, with a bearer and a ticket) connected and ran: each saw every message as an original and then its tombstone once, or the tombstone alone, never a tombstone before an original or twice", count)
	return nil
}

// netRemoval: the manager takes a seat on the net, a net message is said and
// removed by her. It reaches the v2 sockets of the net's people and, as the
// update it is, their events sockets, and nothing of anyone else's, the
// member who is on no net included; a replaying socket of a seat is told the
// tombstone once and one of the member off the net is told nothing.
func (m *mod) netRemoval(readers []*reader, all func() []*feed) error {
	if err := m.putSeat(m.mia); err != nil {
		return err
	}
	r7 := m.text("net message to be removed")
	m7, err := m.post(m.pat, "a net message to be removed", schema.PostMessageRequestV2{Body: r7,
		Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: m.netID}})
	if err != nil {
		return err
	}
	onNet := map[*member]bool{m.pat: true, m.wes: true, m.mia: true}
	for _, r := range readers {
		for _, f := range r.feeds {
			// An events socket is told of a new message of someone else's, not
			// of its own person's.
			if k := kindOf(f); (k != kindMessageV2 && (k != kindEvents || r.p == m.pat)) || !onNet[r.p] {
				continue
			}
			if err := waitFor(f.what+" to carry the net message", modWait, func() (bool, string) {
				for _, fr := range f.received() {
					if bytes.Contains(fr, []byte(r7)) {
						return true, ""
					}
				}
				return false, fmt.Sprintf("%d frames", len(f.received()))
			}); err != nil {
				return err
			}
		}
	}
	base := feedCounts(all())
	if res, err := m.remove(m.mia.api, m7); err != nil || res.status != http.StatusNoContent {
		return fmt.Errorf("the manager removing a net message she has a seat for: %d %s", res.status, errText(err))
	}
	owed := func(f *feed) want {
		for _, r := range readers {
			if onNet[r.p] && slices.Contains(r.feeds, f) {
				return scopedWant(f)
			}
		}
		return wantNothing
	}
	if err := m.checkRemoval("a net's message", all(), base, m7, owed, r7); err != nil {
		return err
	}
	sentinel := m.text("channel-wide replay sentinel after the net removal")
	if _, err := m.post(m.pat, "the replay sentinel after the net removal", schema.PostMessageRequestV2{Body: sentinel}); err != nil {
		return err
	}
	for _, c := range []struct {
		p     *member
		onNet bool
	}{{m.wes, true}, {m.pat, true}, {readers[3].p, false}} {
		f, r, err := dialSocket(c.p.api, c.p.name+"'s replay after the net message", "/v2/ws", url.Values{"channel": {m.main}, "after": {"0"}}, false)
		if err != nil || f == nil {
			return fmt.Errorf("a replaying socket: %s %d %s", errText(err), r.status, r.body)
		}
		err = waitFor(f.what+" to replay up to the post-removal sentinel", modWait, func() (bool, string) {
			for _, fr := range f.received() {
				if bytes.Contains(fr, []byte(sentinel)) {
					return true, ""
				}
			}
			return false, fmt.Sprintf("%d frames", len(f.received()))
		})
		tombs, names := 0, 0
		for _, fr := range f.received() {
			df := decodeFrame(fr)
			if df.tombstone(m7) {
				tombs++
			}
			if df.names(m7) {
				names++
			}
			if bytes.Contains(fr, []byte(r7)) {
				err = fmt.Errorf("a replayed frame holds the removed net message's text")
			}
		}
		f.close()
		if err != nil {
			return err
		}
		if c.onNet && (tombs != 1 || names != 1) || !c.onNet && names != 0 {
			return fmt.Errorf("%s replayed the removed net message in %d frames (%d tombstones), on the net: %v", c.p.name, names, tombs, c.onNet)
		}
	}
	return nil
}
