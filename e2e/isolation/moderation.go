package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/e2e/internal/lkit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Moderation (issue #853, docs/design/moderation.md §9): the real-server
// acceptance cases of channel membership removal, channel entry bans and
// message removal. The routes themselves have their rows in the matrix
// (matrix.go, reactionrows.go, agents.go, guestrows.go, restricted.go);
// this is what the rows cannot show: what happens to the live parts and to
// every read path of someone who was removed or banned, what a seeded
// residual membership can and cannot do, what a removed message does to
// every history, search, file and socket surface, and what a guest's ban and
// next visit look like.
//
// The scenarios run in a group of their own, "M": a manager, a plain member
// and an observer of a channel, a second channel the targets keep, a
// public one, and a net. M's private strings and ids are registered in a
// register of their own (m.marks), not in A's: the phase runs between the
// matrix and the restricted operator's rows, which read lists that would
// name M's channels. Every view that an outsider has of M's channels is
// scanned against it, as are the frames B's open sockets carried while the
// phase ran.

// modWait is how long a scenario waits for something the server does after it
// has answered.
const modWait = 30 * time.Second

// mod is the moderation phase's world.
type mod struct {
	w      *world
	h      *harness
	marks  *markers
	tag    string // in every private string of M's
	ptag   string // in every person's name of M's, so no name is a marker
	bounds voiceBounds
	bsock  *bSockets

	// The channels: main is where the moderation happens, side is one the
	// targets keep, pub is a public one for the self-join refusal.
	main, side, pub       string
	mainID, sideID, pubID int64
	opID                  int64

	mia, pat, wes, ned *member // manager of main; plain; plain; side only
	agent              *member // an agent member of main, from the gates
	netName            string
	netID              int64
	// tara is the principal banned in the ban scenario, with what she held,
	// for the residual-membership scenario that follows.
	tara     *member
	taraHeld *held

	// started is when the phase began: B's sockets are scanned from the
	// frames they had then.
	bSeen map[*feed]int

	// skip is routes the next refusals leave out: a guest is refused an upload
	// or a leave
	// upload as a guest before it is refused as a stranger.
	skip map[string]bool

	// notes are said with the scenario that made them.
	notes []string
}

// text is a private string of M's: its tag and hex, never a word.
func (m *mod) text(what string) string {
	sum := sha256.Sum256([]byte(m.tag + "/" + what))
	s := "modx-" + m.tag + "-" + hex.EncodeToString(sum[:4])
	m.marks.str(what, s)
	return s
}

// pname is a person's name in M: not a marker.
func (m *mod) pname(s string) string { return "modp-" + m.ptag + "-" + s }

func (m *mod) say(format string, args ...any) {
	m.h.say("ok   moderation "+format, args...)
}

// moderationPhase runs the scenarios in order and returns how many requests
// the matrix of refusals made.
func (w *world) moderationPhase(bounds voiceBounds, bsock *bSockets) error {
	m := &mod{w: w, h: w.h, marks: newMarkers(), bounds: bounds, bsock: bsock, bSeen: map[*feed]int{}}
	m.tag = newTag(w.tag, w.btag, w.ptag, w.qtag, w.ntag, w.utag)
	m.ptag = newTag(w.tag, w.btag, w.ptag, w.qtag, w.ntag, w.utag, m.tag)
	for _, f := range bsock.feeds {
		m.bSeen[f] = len(f.received())
	}
	if err := m.setup(); err != nil {
		return fmt.Errorf("the moderation group: %w", err)
	}
	m.say("setup: a manager, two plain members and an observer of a channel, a channel the targets keep, a public one and a net; the operator is not a member of the moderated channel")
	if err := m.gates(); err != nil {
		return fmt.Errorf("the gates: %w", err)
	}
	if err := m.removal(); err != nil {
		return fmt.Errorf("membership removal: %w", err)
	}
	if err := m.ban(); err != nil {
		return fmt.Errorf("the ban: %w", err)
	}
	if err := m.residual(); err != nil {
		return fmt.Errorf("residual membership: %w", err)
	}
	if err := m.legacyBan(); err != nil {
		return fmt.Errorf("a ban row with a membership live: %w", err)
	}
	if err := m.voiceKick(); err != nil {
		return fmt.Errorf("voice-only removal: %w", err)
	}
	if err := m.voiceRace(); err != nil {
		return fmt.Errorf("the ban and the final voice check: %w", err)
	}
	if err := m.messages(); err != nil {
		return fmt.Errorf("message removal: %w", err)
	}
	if err := m.guests(); err != nil {
		return fmt.Errorf("guests: %w", err)
	}
	if err := m.bQuiet(); err != nil {
		return err
	}
	return nil
}

// ---- setup

func (m *mod) newChannel(name string) (int64, error) {
	var resp schema.CreateChannelResponse
	if err := m.w.op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: name}, &resp); err != nil {
		return 0, err
	}
	return resp.Channel.ID, nil
}

// person makes a human by the operator's hand, with a credential and no
// right to decide approvals (a manager may remove only such a member), in
// each of channels.
func (m *mod) person(name string, channels ...string) (*member, error) {
	w := m.w
	var created schema.CreatePrincipalResponse
	if err := w.op.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: schema.PrincipalHuman, Name: name}, &created); err != nil {
		return nil, err
	}
	var cred schema.CreateCredentialResponseV1
	if err := w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", created.Principal.ID), schema.CreateCredentialRequestV1{Label: name}, &cred); err != nil {
		return nil, err
	}
	p := &member{name: name, id: created.Principal.ID, token: cred.Token, credID: cred.Credential.ID}
	p.api = w.as(p)
	w.h.secret(name+"'s credential", cred.Token)
	for _, ch := range channels {
		if err := m.put(ch, p, false, false); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// put adds p to channel as the operator, without the right to decide.
func (m *mod) put(channel string, p *member, decide, manages bool) error {
	return m.w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(channel), p.id),
		schema.PutChannelMemberRequestV1{CanDecide: &decide, Manages: &manages}, nil)
}

// altCredential has the operator issue p a second credential. It is not a
// marker of anyone's.
func (m *mod) altCredential(p *member) (*member, error) {
	var cred schema.CreateCredentialResponseV1
	if err := m.w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", p.id), schema.CreateCredentialRequestV1{Label: "alternate"}, &cred); err != nil {
		return nil, err
	}
	alt := &member{name: p.name + " (alternate credential)", id: p.id, token: cred.Token, credID: cred.Credential.ID}
	alt.api = m.w.as(alt)
	m.h.secret(alt.name, cred.Token)
	return alt, nil
}

func (m *mod) setup() error {
	w := m.w
	var err error
	// The side channel and the public one are not markers: the targets and
	// the observer keep them.
	m.main, m.side, m.pub = m.text("main channel"), "side-"+m.tag, "pub-"+m.tag
	if m.mainID, err = m.newChannel(m.main); err != nil {
		return err
	}
	if m.sideID, err = m.newChannel(m.side); err != nil {
		return err
	}
	if m.pubID, err = m.newChannel(m.pub); err != nil {
		return err
	}
	m.marks.id("channel", "the moderated channel", m.mainID)
	pub := schema.ChannelPublic
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", m.pubID), schema.ChannelSettingsRequestV1{Visibility: &pub}, nil); err != nil {
		return err
	}
	var who schema.WhoAmIResponseV1
	if err := w.op.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return err
	}
	m.opID = who.ID
	if m.mia, err = m.person(m.pname("mia"), m.main, m.side); err != nil {
		return err
	}
	if err := m.put(m.main, m.mia, false, true); err != nil {
		return err
	}
	if m.pat, err = m.person(m.pname("pat"), m.main, m.side); err != nil {
		return err
	}
	if m.wes, err = m.person(m.pname("wes"), m.main, m.side); err != nil {
		return err
	}
	if m.ned, err = m.person(m.pname("ned"), m.side); err != nil {
		return err
	}
	m.netName = m.text("net")
	var net schema.CreateNetResponseV1
	if err := w.op.call(http.MethodPost, "/v1/channels/"+url.PathEscape(m.main)+"/nets", schema.CreateNetRequestV1{Name: m.netName}, &net); err != nil {
		return err
	}
	m.netID = net.Net.ID
	m.marks.id("net", "the moderated channel's net", m.netID)
	for _, p := range []*member{m.pat, m.wes} {
		if err := m.putSeat(p); err != nil {
			return err
		}
	}
	// The operator is not a member of the moderated channel: a route of the
	// operator's class passes without it, and a message is not one an
	// operator can reach by being one (§7.1).
	if err := w.op.call(http.MethodDelete, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), m.opID), nil, nil); err != nil {
		return fmt.Errorf("take the operator out of the moderated channel: %w", err)
	}
	return nil
}

func (m *mod) putSeat(p *member) error {
	return m.w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/nets/%s/members/%d", url.PathEscape(m.main), m.netName, p.id),
		schema.PutNetMemberRequestV1{Role: schema.NetRoleMember}, nil)
}

// ---- the database, beside the server

// underCount counts the rows of a query on the running server's database.
func (w *world) underCount(query string, args ...any) (int, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(w.d.dataDir, "conch.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var n int
	err = db.QueryRow(query, args...).Scan(&n)
	return n, err
}

// residualMember seeds a membership row of the moderated channel for p, as a
// restore or a legacy row would leave one beside a ban, and a net seat if
// seat.
func (m *mod) residualMember(p *member, seat bool) error {
	now := time.Now().UnixMilli()
	if err := m.w.underDB("INSERT INTO channel_members (channel_id, principal_id, added_by, created_at, can_decide, manages) VALUES (?, ?, NULL, ?, 0, 0)",
		m.mainID, p.id, now); err != nil {
		return fmt.Errorf("seed %s's membership: %w", p.name, err)
	}
	if seat {
		if err := m.w.underDB("INSERT INTO net_members (net_id, principal_id, role, added_by, created_at) VALUES (?, ?, 'member', NULL, ?)",
			m.netID, p.id, now); err != nil {
			return fmt.Errorf("seed %s's net seat: %w", p.name, err)
		}
	}
	return nil
}

// residualRows counts p's rows in the moderated channel's members and nets.
func (m *mod) residualRows(p *member) (members, seats int, err error) {
	if members, err = m.w.underCount("SELECT COUNT(*) FROM channel_members WHERE channel_id = ? AND principal_id = ?", m.mainID, p.id); err != nil {
		return
	}
	seats, err = m.w.underCount("SELECT COUNT(*) FROM net_members WHERE net_id = ? AND principal_id = ?", m.netID, p.id)
	return
}

// auditCount counts the audit rows of action whose subject is subject and
// whose detail is detail ("" for any), and fails on a row that carries any
// string of M's private register or a name.
func (m *mod) auditCount(action, subject, detail string) (int, error) {
	rows, err := m.w.auditRows(action)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range rows {
		line := e.Actor + " " + e.Subject + " " + e.Detail
		if leaks := m.marks.leaks([]byte(line), asked{}); len(leaks) > 0 {
			return 0, fmt.Errorf("an audit row of %s carries %s: %+v", action, strings.Join(leaks, "; "), e)
		}
		if red := m.h.redact(line); red != line {
			return 0, fmt.Errorf("an audit row of %s carries a token, secret or room name the run registered: %+v", action, e)
		}
		for _, p := range []*member{m.mia, m.pat, m.wes, m.ned} {
			if strings.Contains(line, p.name) {
				return 0, fmt.Errorf("an audit row of %s carries the name %s: %+v", action, p.name, e)
			}
		}
		if e.Subject == subject && (detail == "" || e.Detail == detail) {
			n++
		}
	}
	return n, nil
}

// ---- requests that are refused as for something that never existed

// refusal is a request, built over a channel name, a message id and a file
// id, and the answer the route gives someone who is not a member.
type refusal struct {
	route string
	build func(ch string, msg, file int64) probe
	want  answer
	// byFile says the route names only the file: it is built for the
	// file's id and answers file_not_found.
	byFile bool
}

var fileNotFound = answer{http.StatusNotFound, "file_not_found"}

// outRefusals is every route that names a channel (or a file of it) that a
// principal who is no longer a member, or never was, is refused: the same
// answer as for something that never existed.
func outRefusals(term string) []refusal {
	chp := func(ch string) string { return "/v2/channels/" + ch }
	transmit := schema.VoiceTransmitReportV1{State: schema.VoiceTransmitStateStarted}
	companion := schema.VoiceCompanionRequestV1{Publish: schema.VoiceCompanionPublishScreen, Subscribe: true}
	companionAudio := schema.VoiceCompanionAudioRequestV1{Scope: schema.VoiceShareAudioScopeApplication}
	extra := []refusal{
		{route: "POST /v1/channels/{channel}/leave", build: func(ch string, _, _ int64) probe { return post("/v1/channels/"+ch+"/leave", nil) }},
		{route: "POST /v1/channels/{channel}/voice/transmit", build: func(ch string, _, _ int64) probe { return post("/v1/channels/"+ch+"/voice/transmit", transmit) }},
		{route: "POST /v1/channels/{channel}/voice/share", build: func(ch string, _, _ int64) probe { return post("/v1/channels/"+ch+"/voice/share", nil) }},
		{route: "POST /v1/channels/{channel}/voice/camera", build: func(ch string, _, _ int64) probe { return post("/v1/channels/"+ch+"/voice/camera", nil) }},
		{route: "POST /v1/channels/{channel}/voice/companion", build: func(ch string, _, _ int64) probe { return post("/v1/channels/"+ch+"/voice/companion", companion) }},
		{route: "PUT /v1/channels/{channel}/voice/companion/audio", build: func(ch string, _, _ int64) probe {
			return put("/v1/channels/"+ch+"/voice/companion/audio", companionAudio)
		}},
	}
	// The reaction and edit routes of the table, and the removal route (listed
	// below), built from the matrix's own tables.
	for _, r := range reactionRoutes {
		if r.route == "DELETE /v2/channels/{channel}/messages/{message_id}" {
			continue
		}
		extra = append(extra, refusal{route: r.route, build: func(ch string, msg, _ int64) probe { return r.probe(ch, msg) }})
	}
	for _, r := range editRoutes {
		extra = append(extra, refusal{route: r.route, build: func(ch string, msg, _ int64) probe { return r.probe(ch, msg) }})
	}
	return append(extra, []refusal{
		{route: "GET /v0/channels/{channel}/messages", build: func(ch string, _, _ int64) probe { return get("/v0/channels/" + ch + "/messages") }},
		{route: "GET /v1/channels/{channel}/messages", build: func(ch string, _, _ int64) probe { return get("/v1/channels/" + ch + "/messages") }},
		{route: "GET /v2/channels/{channel}/messages", build: func(ch string, _, _ int64) probe { return get(chp(ch) + "/messages?after=0&limit=100") }},
		{route: "GET /v2/channels/{channel}/messages", build: func(ch string, msg, _ int64) probe { return get(chp(ch) + "/messages?thread=" + id(msg)) }},
		{route: "GET /v2/channels/{channel}/messages/search", build: func(ch string, _, _ int64) probe {
			return get(chp(ch) + "/messages/search?" + url.Values{"q": {term}}.Encode())
		}},
		{route: "POST /v0/channels/{channel}/messages", build: func(ch string, _, _ int64) probe {
			return post("/v0/channels/"+ch+"/messages", map[string]string{"body": "x"})
		}},
		{route: "POST /v1/channels/{channel}/messages", build: func(ch string, _, _ int64) probe {
			return post("/v1/channels/"+ch+"/messages", map[string]string{"body": "x"})
		}},
		{route: "POST /v2/channels/{channel}/messages", build: func(ch string, _, _ int64) probe {
			return post(chp(ch)+"/messages", schema.PostMessageRequestV2{Body: "x"})
		}},
		{route: "POST /v2/channels/{channel}/files", build: func(ch string, _, _ int64) probe {
			return probe{method: http.MethodPost, path: chp(ch) + "/files?filename=x.txt", raw: []byte("x"), contentType: "text/plain"}
		}},
		{route: "GET /v1/channels/{channel}/members", build: func(ch string, _, _ int64) probe { return get("/v1/channels/" + ch + "/members") }},
		{route: "GET /v1/channels/{channel}/authors", build: func(ch string, _, _ int64) probe { return get("/v1/channels/" + ch + "/authors") }},
		{route: "GET /v1/channels/{channel}/nets", build: func(ch string, _, _ int64) probe { return get("/v1/channels/" + ch + "/nets") }},
		{route: "PUT /v1/channels/{channel}/read-state", build: func(ch string, msg, _ int64) probe {
			return put("/v1/channels/"+ch+"/read-state", schema.MarkReadRequestV1{LastReadID: msg})
		}},
		{route: "POST /v1/channels/{channel}/voice/session", build: func(ch string, _, _ int64) probe { return post("/v1/channels/"+ch+"/voice/session", nil) }},
		{route: "GET /v1/channels/{channel}/voice", build: func(ch string, _, _ int64) probe { return get("/v1/channels/" + ch + "/voice") }},
		{route: "GET /v1/channels/{channel}/bans", build: func(ch string, _, _ int64) probe { return get("/v1/channels/" + ch + "/bans") }},
		{route: "DELETE /v2/channels/{channel}/messages/{message_id}", build: func(ch string, msg, _ int64) probe {
			return del(chp(ch) + "/messages/" + id(msg))
		}},
		{route: "GET /v2/files/{id}/meta", byFile: true, want: fileNotFound, build: func(_ string, _, f int64) probe { return get("/v2/files/" + id(f) + "/meta") }},
		{route: "GET /v2/files/{id}", byFile: true, want: fileNotFound, build: func(_ string, _, f int64) probe { return get("/v2/files/" + id(f)) }},
	}...)
}

// scrub sets aside what a request named so two answers can be compared.
func scrub(body []byte, names []string, ids ...int64) []byte {
	out := body
	for _, n := range names {
		if n != "" {
			out = bytes.ReplaceAll(out, []byte(n), []byte("<asked>"))
		}
	}
	for _, i := range ids {
		if i != 0 {
			out = bytes.ReplaceAll(out, []byte(itoa(i)), []byte("<asked>"))
		}
	}
	return out
}

// outAs requires that a is refused every route of outRefusals about the
// moderated channel (msg and file are things of it) as it is refused for a
// channel that never existed, with nothing of M's in a body or a header.
func (m *mod) outAs(a api, what string, msg, file int64, term string) (int, error) {
	return m.outIn(m.main, a, what, msg, file, term)
}

// outIn is outAs for any channel by name.
func (m *mod) outIn(channel string, a api, what string, msg, file int64, term string) (int, error) {
	w := m.w
	var failures []string
	n := 0
	g := w.ghost()
	for _, r := range outRefusals(term) {
		if m.skip[r.route] {
			continue
		}
		want := r.want
		if want == (answer{}) {
			want = channelNotFound
		}
		real := r.build(url.PathEscape(channel), msg, file)
		if err := w.checkRoute(r.route, real); err != nil {
			return n, err
		}
		as := asked{strs: []string{channel}, ids: []kindID{{"message", msg}, {"file", file}}}
		got, err := w.send(a, real)
		n++
		if err != nil {
			failures = append(failures, fmt.Sprintf("  %s: %v", r.route, err))
			continue
		}
		if leaks := m.marks.leaks(got.body, as); len(leaks) > 0 {
			failures = append(failures, fmt.Sprintf("  %s %s: status %d and the body holds %s", what, r.route, got.status, strings.Join(leaks, "; ")))
			continue
		}
		if leaks := m.marks.headerLeaks(got.header, as); len(leaks) > 0 {
			failures = append(failures, fmt.Sprintf("  %s %s: the headers hold %s", what, r.route, strings.Join(leaks, "; ")))
			continue
		}
		if err := checkRefusal(got, want, as); err != nil {
			failures = append(failures, fmt.Sprintf("  %s %s: %v", what, r.route, err))
			continue
		}
		ghost, err := w.send(a, r.build(url.PathEscape(g.name), g.id, g.id))
		n++
		if err != nil {
			failures = append(failures, fmt.Sprintf("  %s: %v", r.route, err))
			continue
		}
		if ghost.status != got.status ||
			!bytes.Equal(scrub(ghost.body, []string{g.name}, g.id), scrub(got.body, []string{channel}, msg, file)) {
			failures = append(failures, fmt.Sprintf("  %s %s: %d %s, and for something that never existed %d %s",
				what, r.route, got.status, strings.TrimSpace(string(got.body)), ghost.status, strings.TrimSpace(string(ghost.body))))
		}
	}
	if len(failures) > 0 {
		return n, fmt.Errorf("%d routes were not refused as for a ghost:\n%s", len(failures), strings.Join(failures, "\n"))
	}
	return n, nil
}

// socketsRefused requires that every socket variant on the moderated channel
// is refused to a, 404 channel_not_found as for a channel that never
// existed, and that the same variants on the side channel are open.
func (m *mod) socketsRefused(a api, what string, sideOpen bool) (int, error) {
	g := m.w.ghost()
	n := 0
	for _, v := range socketVariants {
		f, r, err := dialSocket(a, fmt.Sprintf("%s's %s (ticket=%v)", what, v.path, v.ticket), v.path, url.Values{"channel": {m.main}}, v.ticket)
		if err != nil {
			return n, err
		}
		n++
		if f != nil {
			f.close()
			return n, fmt.Errorf("%s opened %s (ticket=%v) on the moderated channel", what, v.path, v.ticket)
		}
		got := result(r)
		as := asked{strs: []string{m.main}}
		if leaks := m.marks.leaks(got.body, as); len(leaks) > 0 {
			return n, fmt.Errorf("%s %s refused with a body that holds %s", what, v.path, strings.Join(leaks, "; "))
		}
		if leaks := m.marks.headerLeaks(got.header, as); len(leaks) > 0 {
			return n, fmt.Errorf("%s %s refused with headers that hold %s", what, v.path, strings.Join(leaks, "; "))
		}
		if err := checkRefusal(got, channelNotFound, as); err != nil {
			return n, fmt.Errorf("%s %s (ticket=%v): %w", what, v.path, v.ticket, err)
		}
		gf, gr, err := dialSocket(a, "a ghost", v.path, url.Values{"channel": {g.name}}, v.ticket)
		if err != nil {
			return n, err
		}
		n++
		if gf != nil {
			gf.close()
			return n, fmt.Errorf("%s opened %s on a channel that never existed", what, v.path)
		}
		if gr.status != got.status || !bytes.Equal(scrub(gr.body, []string{g.name}), scrub(got.body, []string{m.main})) {
			return n, fmt.Errorf("%s %s: %d %s on the moderated channel, %d %s on a ghost", what, v.path, got.status, got.body, gr.status, gr.body)
		}
		if !sideOpen {
			continue
		}
		sf, sr, err := dialSocket(a, fmt.Sprintf("%s's %s on the side channel", what, v.path), v.path, url.Values{"channel": {m.side}}, v.ticket)
		if err != nil {
			return n, err
		}
		n++
		if sf == nil {
			return n, fmt.Errorf("%s was refused %s on the side channel, which she keeps: %d %s", what, v.path, sr.status, sr.body)
		}
		sf.close()
	}
	return n, nil
}

// keepsSide requires that a can still read the side channel and is
// otherwise whole: the unrelated channel, the credential.
func (m *mod) keepsSide(a api, what string) error {
	for _, p := range []string{"/v2/channels/" + url.PathEscape(m.side) + "/messages", "/v1/channels/" + url.PathEscape(m.side) + "/members", "/v1/whoami"} {
		status, body, err := a.do(http.MethodGet, p, nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("%s: GET %s = %d %s, want 200: she keeps the unrelated channel and her credential", what, p, status, strings.TrimSpace(string(body)))
		}
	}
	return nil
}

// ---- holders

// held is what a target holds before it is removed or banned: every socket
// variant on the moderated channel on two credentials, the side channel's,
// the occupancy socket, and with LiveKit a participant and a join grant kept
// from before.
type held struct {
	p        *member
	alt      *member
	main     []*feed
	altMain  []*feed
	side     []*feed
	occ      []*feed
	room     string
	grant    schema.VoiceRoomGrant
	hasGrant bool
	// msg and file are a message and an unattached upload of the principal's
	// own in the moderated channel, made before it was lost: the ids the
	// refusals are compared on, not ids nobody issued.
	msg, file int64
	// keep is the channels whose occupancy rows the principal keeps when the
	// moderated channel is lost.
	keep []int64
}

func (m *mod) hold(p *member) (*held, error) {
	w := m.w
	h := &held{p: p, keep: []int64{m.sideID}}
	var err error
	if h.alt, err = m.altCredential(p); err != nil {
		return nil, err
	}
	if h.msg, err = m.post(p, p.name+"'s message", schema.PostMessageRequestV2{Body: m.text(p.name + "'s message")}); err != nil {
		return nil, err
	}
	if h.file, err = m.upload(p, m.text(p.name+"'s file name")+".txt", m.text(p.name+"'s file contents")); err != nil {
		return nil, err
	}
	m.marks.id("file", p.name+"'s file", h.file)
	if h.main, err = openAll(p, m.main); err != nil {
		return nil, err
	}
	if h.altMain, err = openAll(h.alt, m.main); err != nil {
		return nil, err
	}
	if h.side, err = openAll(p, m.side); err != nil {
		return nil, err
	}
	if h.occ, err = openOccupancy(p); err != nil {
		return nil, err
	}
	for _, f := range h.occ {
		if err := waitFor(f.what+" to send its document", modWait, func() (bool, string) { return len(f.received()) > 0, "no frame" }); err != nil {
			return nil, err
		}
		doc, err := parseOccupancy(f.received()[0])
		if err != nil {
			return nil, err
		}
		if err := checkRows(doc, []int64{m.mainID, m.sideID}); err != nil {
			return nil, fmt.Errorf("%s before: %w", f.what, err)
		}
	}
	if w.lk != nil {
		sess, err := w.session(p, url.PathEscape(m.main))
		if err != nil {
			return nil, err
		}
		h.room = sess.Rooms[0].Room
		if _, err := w.joinSignal(sess.Rooms[0]); err != nil {
			return nil, fmt.Errorf("%s joins the room: %w", p.name, err)
		}
		if err := waitFor("LiveKit to list "+p.name, modWait, func() (bool, string) {
			ok, _, saw := w.lk.has(h.room, p.id)
			return ok, saw
		}); err != nil {
			return nil, err
		}
		fresh, err := w.session(p, url.PathEscape(m.main))
		if err != nil {
			return nil, err
		}
		h.grant, h.hasGrant = fresh.Rooms[0], true
	}
	return h, nil
}

// closing is the sockets that must close when h's principal loses the
// channel: the moderated channel's sockets of both credentials. The side
// channel's and the occupancy sockets stay.
func (h *held) closing() []*feed { return append(append([]*feed{}, h.main...), h.altMain...) }

// closedRemoved requires that each of feeds closed with 1008 and the reason a
// membership that went is given.
func closedRemoved(feeds []*feed) error {
	return closedWith(feeds, func(*feed) []string { return []string{membershipGone} })
}

// loses runs act, timed, and requires the revocation of h: the moderated
// channel's sockets closed within the bound on both credentials, LiveKit's
// participant gone within its bound, the occupancy sockets open and without
// the channel's row, the side channel's sockets open, and the side channel
// and the credentials still good.
func (m *mod) loses(what string, h *held, act func() error) (timing, error) {
	w := m.w
	hd := holder{m: h.p, sockets: h.closing(), room: h.room}
	t, err := w.measure(what, []holder{hd}, m.bounds, act)
	if err != nil {
		return t, err
	}
	if err := closedRemoved(h.closing()); err != nil {
		return t, err
	}
	for _, f := range h.side {
		if ended, _ := f.ended(); ended {
			return t, fmt.Errorf("%s: %s closed with the other channel's access gone", what, f.what)
		}
	}
	for _, f := range h.occ {
		if err := waitFor(f.what+" to drop the moderated channel's row", modWait, func() (bool, string) {
			frames := f.received()
			doc, err := parseOccupancy(frames[len(frames)-1])
			if err != nil {
				return false, err.Error()
			}
			if slices.ContainsFunc(doc.Channels, func(c schema.VoiceChannelOccupancy) bool { return c.ChannelID == m.mainID }) {
				return false, "the latest document has the channel"
			}
			return true, ""
		}); err != nil {
			return t, err
		}
		if ended, _ := f.ended(); ended {
			return t, fmt.Errorf("%s: %s closed", what, f.what)
		}
		frames := f.received()
		doc, _ := parseOccupancy(frames[len(frames)-1])
		if err := checkRows(doc, h.keep); err != nil {
			return t, fmt.Errorf("%s: %s after: %w", what, f.what, err)
		}
	}
	for _, a := range []api{h.p.api, h.alt.api} {
		if err := m.keepsSide(a, what+" ("+a.who+")"); err != nil {
			return t, err
		}
	}
	return t, nil
}

// grantDead requires that the grant kept from before is no use: the room it
// names was rotated away, as for a revoked credential (staysOut).
func (m *mod) grantDead(what string, h *held) error {
	if m.w.lk == nil || !h.hasGrant {
		return nil
	}
	return m.joinFails(what, h.grant.Token)
}

func (m *mod) joinFails(what, token string) error {
	w := m.w
	deadline := time.Now().Add(modWait)
	for {
		ctx, cancel := context.WithTimeout(stopCtx, 15*time.Second)
		c, status, body, err := lkit.DialSignal(ctx, w.lk.srv.WSURL, token)
		cancel()
		if err == nil {
			c.Close()
			if time.Now().After(deadline) {
				return fmt.Errorf("%s: LiveKit admitted a grant signed before the loss of access", what)
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if status != http.StatusNotFound || !strings.Contains(body, "room does not exist") {
			return fmt.Errorf("%s: LiveKit answered %d %q to a grant signed before, want 404: the room was not rotated", what, status, w.h.redact(body))
		}
		return nil
	}
}

// outEverywhere is the whole of what must hold after p has lost the channel,
// on each of p's credentials.
func (m *mod) outEverywhere(what string, h *held, msg, file int64, term string, bounds voiceBounds) (int, error) {
	total := 0
	for _, a := range []api{h.p.api, h.alt.api} {
		n, err := m.outAs(a, a.who, msg, file, term)
		total += n
		if err != nil {
			return total, fmt.Errorf("%s: %w", what, err)
		}
		n, err = m.socketsRefused(a, a.who, true)
		total += n
		if err != nil {
			return total, fmt.Errorf("%s: %w", what, err)
		}
	}
	// Occupancy: a socket opened now has no row of the channel.
	feeds, err := openOccupancy(h.p)
	if err != nil {
		return total, err
	}
	for _, f := range feeds {
		err := waitFor(f.what+" to send its document", modWait, func() (bool, string) { return len(f.received()) > 0, "no frame" })
		if err == nil {
			var doc schema.VoiceOccupancyV1
			if doc, err = parseOccupancy(f.received()[0]); err == nil {
				err = checkRows(doc, h.keep)
			}
		}
		f.close()
		if err != nil {
			return total, fmt.Errorf("%s: a new occupancy socket: %w", what, err)
		}
	}
	if err := m.grantDead(what, h); err != nil {
		return total, err
	}
	if m.w.lk != nil {
		time.Sleep(bounds.pass)
		if where, err := m.w.lk.anywhere(h.p.id); err != nil {
			return total, err
		} else if where != 0 {
			return total, fmt.Errorf("%s: one poller pass later LiveKit lists %s in %d room(s)", what, h.p.name, where)
		}
	}
	return total, nil
}

// ---- frames

// liveFrame is what a message or events socket sends, as far as the
// scenarios read it.
type liveFrame struct {
	Schema  string
	ID      int64
	Body    string
	Removed bool
	Payload *schema.Payload
	Message *schema.MessageV2
}

func decodeFrame(raw []byte) liveFrame {
	var f liveFrame
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return f
	}
	f.Schema = head.Schema
	set := func(msg schema.MessageV2) {
		f.ID, f.Body, f.Removed, f.Payload = msg.ID, msg.Body, msg.Removed, msg.Payload
	}
	switch f.Schema {
	case "":
		var msg schema.MessageV0
		if err := json.Unmarshal(raw, &msg); err == nil {
			set(schema.MessageV2{ID: msg.ID, Body: msg.Body})
		}
	case schema.MessageSchemaV1:
		var msg schema.MessageV1
		if err := json.Unmarshal(raw, &msg); err == nil {
			set(schema.MessageV2FromV1(msg))
		}
	case schema.MessageSchemaV2:
		var msg schema.MessageV2
		if err := json.Unmarshal(raw, &msg); err == nil {
			set(msg)
		}
	case schema.MessageRemovedSchemaV1:
		var event schema.MessageRemovedV1
		if err := json.Unmarshal(raw, &event); err == nil {
			f.Message = &event.Message
		}
	default:
		var event struct {
			Message *schema.MessageV2 `json:"message"`
		}
		if err := json.Unmarshal(raw, &event); err == nil {
			f.Message = event.Message
		}
	}
	return f
}

// names reports whether the frame is about message id.
func (f liveFrame) names(id int64) bool {
	return (f.isMessage() && f.ID == id) || (f.Message != nil && f.Message.ID == id)
}

// isMessage reports whether the frame is a message in one of the three
// envelopes (v0 has no schema field).
func (f liveFrame) isMessage() bool {
	return f.Schema == "" || f.Schema == schema.MessageSchemaV1 || f.Schema == schema.MessageSchemaV2
}

// tombstone reports whether the frame is the tombstone of message id, in any
// envelope: a message frame with the fixed body and no payload, removed:true
// in v2, or the events socket's conch.message_removed.v1.
func (f liveFrame) tombstone(id int64) bool {
	switch {
	case f.Schema == schema.MessageRemovedSchemaV1:
		return f.Message != nil && f.Message.ID == id && f.Message.Removed && f.Message.Body == schema.MessageRemovedBody && f.Message.Payload == nil
	case f.isMessage() && f.ID == id:
		return f.Body == schema.MessageRemovedBody && f.Payload == nil && (f.Schema != schema.MessageSchemaV2 || f.Removed)
	}
	return false
}

// since is the frames of f after the first n.
func (f *feed) since(n int) [][]byte {
	fr := f.received()
	return fr[min(n, len(fr)):]
}

// feedCounts is how many frames each feed has.
func feedCounts(feeds []*feed) map[*feed]int {
	out := map[*feed]int{}
	for _, f := range feeds {
		out[f] = len(f.received())
	}
	return out
}

// ---- the gates (§9 cases 1 and 2)

func (m *mod) gates() error {
	w := m.w
	bea, bob := w.b.people[0], w.b.people[1]
	target, err := m.person(m.pname("gate"), m.main, m.side)
	if err != nil {
		return err
	}
	ban := fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.main), target.id)
	kick := fmt.Sprintf("/v1/channels/%s/voice/participants/%d", url.PathEscape(m.main), target.id)
	// A plain member of the channel: 403 on every new route, and nothing
	// happened. An agent: the human-only refusal. Neither's refusal looks the
	// target up: the answer for a target that does not exist is the same.
	agent, err := w.newAgent(m.pname("bot"), &group{channels: []string{m.main}, chanIDs: []int64{m.mainID}})
	if err != nil {
		return err
	}
	m.agent = agent
	ghostTarget := w.ghost().id
	for _, c := range []struct {
		who  string
		a    api
		want answer
	}{
		{"a plain member", m.pat.api, operatorOnly},
		{"an agent", agent.api, operatorOnly},
	} {
		for _, r := range []struct {
			route, method, path, ghost string
		}{
			{"PUT /v1/channels/{channel}/bans/{principal_id}", http.MethodPut, ban, fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.main), ghostTarget)},
			{"DELETE /v1/channels/{channel}/bans/{principal_id}", http.MethodDelete, ban, fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.main), ghostTarget)},
			{"GET /v1/channels/{channel}/bans", http.MethodGet, "/v1/channels/" + url.PathEscape(m.main) + "/bans", "/v1/channels/" + url.PathEscape(m.main) + "/bans"},
			{"DELETE /v1/channels/{channel}/voice/participants/{principal_id}", http.MethodDelete, kick, fmt.Sprintf("/v1/channels/%s/voice/participants/%d", url.PathEscape(m.main), ghostTarget)},
		} {
			got, err := w.send(c.a, probe{method: r.method, path: r.path})
			if err != nil {
				return err
			}
			if err := checkRefusal(got, c.want, asked{strs: []string{m.main}, ids: []kindID{{"principal", target.id}}}); err != nil {
				return fmt.Errorf("%s on %s: %w", c.who, r.route, err)
			}
			gh, err := w.send(c.a, probe{method: r.method, path: r.ghost})
			if err != nil {
				return err
			}
			if gh.status != got.status || !bytes.Equal(scrub(gh.body, nil, ghostTarget), scrub(got.body, nil, target.id)) {
				return fmt.Errorf("%s on %s: %d %s, and naming nobody %d %s: the refusal looked the target up", c.who, r.route, got.status, got.body, gh.status, gh.body)
			}
		}
	}
	// Someone of B (a manager of her own channel) and a plain B member: the
	// channel does not exist to them, whatever route.
	for _, who := range []*member{bea, bob} {
		for _, r := range []struct{ route, method, path string }{
			{"PUT /v1/channels/{channel}/bans/{principal_id}", http.MethodPut, ban},
			{"DELETE /v1/channels/{channel}/bans/{principal_id}", http.MethodDelete, ban},
			{"GET /v1/channels/{channel}/bans", http.MethodGet, "/v1/channels/" + url.PathEscape(m.main) + "/bans"},
			{"DELETE /v1/channels/{channel}/voice/participants/{principal_id}", http.MethodDelete, kick},
			{"DELETE /v1/channels/{channel}/members/{principal_id}", http.MethodDelete, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), target.id)},
		} {
			p := probe{method: r.method, path: r.path}
			if err := w.checkRoute(r.route, p); err != nil {
				return err
			}
			got, err := w.send(who.api, p)
			if err != nil {
				return err
			}
			as := asked{strs: []string{m.main}, ids: []kindID{{"principal", target.id}}}
			if leaks := m.marks.leaks(got.body, as); len(leaks) > 0 {
				return fmt.Errorf("%s on %s: the body holds %s", who.name, r.route, strings.Join(leaks, "; "))
			}
			if leaks := m.marks.headerLeaks(got.header, as); len(leaks) > 0 {
				return fmt.Errorf("%s on %s: the headers hold %s", who.name, r.route, strings.Join(leaks, "; "))
			}
			if err := checkRefusal(got, channelNotFound, as); err != nil {
				return fmt.Errorf("%s on %s: %w", who.name, r.route, err)
			}
			g := w.ghost()
			gp := probe{method: r.method, path: strings.NewReplacer(url.PathEscape(m.main), g.name, itoa(target.id), itoa(g.id)).Replace(r.path)}
			gh, err := w.send(who.api, gp)
			if err != nil {
				return err
			}
			if gh.status != got.status || !bytes.Equal(scrub(gh.body, []string{g.name}, g.id), scrub(got.body, []string{m.main}, target.id)) {
				return fmt.Errorf("%s on %s: %d %s, and for a ghost %d %s", who.name, r.route, got.status, got.body, gh.status, gh.body)
			}
		}
	}
	// The manager's targets (§9 case 2): she may remove or ban a plain human
	// member, and no approver, agent, operator or owner. The refusal changes
	// nothing: the approver keeps the right to decide and the membership.
	approver, err := m.person(m.pname("approver"), m.main)
	if err != nil {
		return err
	}
	yes := true
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), approver.id), schema.PutChannelMemberRequestV1{CanDecide: &yes}, nil); err != nil {
		return err
	}
	// The operator is made a member for the moment: the owner is a target no
	// manager may touch.
	if err := m.put(m.main, &member{id: m.opID}, true, false); err != nil {
		return err
	}
	for _, c := range []struct {
		what string
		id   int64
	}{{"an approver", approver.id}, {"an agent", agent.id}, {"the owner", m.opID}} {
		for _, r := range []struct{ method, path string }{
			{http.MethodPut, fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.main), c.id)},
			{http.MethodDelete, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), c.id)},
		} {
			got, err := w.send(m.mia.api, probe{method: r.method, path: r.path})
			if err != nil {
				return err
			}
			if err := checkRefusal(got, operatorOnly, asked{ids: []kindID{{"principal", c.id}}}); err != nil {
				return fmt.Errorf("the manager %s %s %s: %w", r.method, c.what, r.path, err)
			}
		}
	}
	var members schema.ListChannelMembersResponseV1
	if err := w.op.call(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/members", nil, &members); err != nil {
		return err
	}
	for _, c := range []struct {
		id   int64
		want bool
	}{{approver.id, true}} {
		found := false
		for _, mem := range members.Members {
			if mem.PrincipalID == c.id {
				found = true
				if mem.CanDecide == nil || *mem.CanDecide != c.want {
					return fmt.Errorf("a refused ban changed the approver's right to decide: %+v", mem)
				}
			}
		}
		if !found {
			return fmt.Errorf("a refused manager's ban or removal took the approver %d out of the channel", c.id)
		}
	}
	// A manager is not an owner or operator target either, and neither may a
	// non-owner operator ban an owner (issue #1468): the manager's refusals
	// are above, the operator's are in operatorBan. What the store holds of
	// both rules is internal/server/store/bans_test.go. A message removed
	// reads as a tombstone over MCP read_channel in mcpTombstone
	// (modmessages.go).
	if err := m.operatorBan(); err != nil {
		return fmt.Errorf("a non-owner operator and the owner: %w", err)
	}
	for _, p := range []*member{target, approver, {id: m.opID}} {
		if err := w.op.call(http.MethodDelete, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), p.id), nil, nil); err != nil {
			return err
		}
	}
	m.say("gates: a plain member and an agent were refused the ban, unban, list and voice-removal routes before any target was looked up, and someone of B (her own channel's manager and a plain member) was told the channel does not exist, each exactly as for a ghost; a manager's ban or removal of an approver, an agent or the owner was refused, with the approver's right to decide unchanged")
	return nil
}

// operatorBan is the row of issue #1468: an operator who is not an owner is
// admitted by the route guard (it holds manage_channels), and the store
// refuses it the one target it may not touch, an owner. The owner is a member
// of the channel for the moment (gates put it there). The refusal is 403
// forbidden, audited as an access_denied of the operator's, it changes
// nothing for the owner, who still reads and posts, and the same operator
// bans a plain member of the channel (the control, so the 403 is for the
// target and not for the operator).
func (m *mod) operatorBan() error {
	w := m.w
	opr, err := m.person(m.pname("operator"))
	if err != nil {
		return err
	}
	if err := w.op.call(http.MethodPut, "/v1/principals/"+itoa(opr.id)+"/role", schema.RoleChangeRequestV1{Role: schema.RoleOperator}, nil); err != nil {
		return err
	}
	ctl, err := m.person(m.pname("banned-by-operator"), m.main)
	if err != nil {
		return err
	}
	ban := func(id int64) string { return fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.main), id) }
	denied := func() (int, error) {
		rows, err := w.auditRows("access_denied")
		if err != nil {
			return 0, err
		}
		n := 0
		for _, e := range rows {
			if e.Actor == "principal:"+itoa(opr.id) && strings.Contains(e.Subject, "/bans/") {
				n++
			}
		}
		return n, nil
	}
	before, err := denied()
	if err != nil {
		return err
	}
	status, body, err := opr.api.do(http.MethodPut, ban(m.opID), nil)
	if err != nil {
		return err
	}
	if status != http.StatusForbidden {
		return fmt.Errorf("a non-owner operator banning the owner = %d %s, want 403", status, m.h.redact(string(body)))
	}
	if e, err := errorOf(body); err != nil || e.Code != "forbidden" {
		return fmt.Errorf("a non-owner operator banning the owner = %s, want the code forbidden (%v)", m.h.redact(string(body)), errText(err))
	}
	after, err := denied()
	if err != nil {
		return err
	}
	if after != before+1 {
		return fmt.Errorf("the refused ban wrote %d access_denied rows of the operator's for a ban route, want 1", after-before)
	}
	// Nothing changed for the owner: not listed as banned, still a member,
	// and it reads and posts in the channel.
	var bans schema.ListChannelBansResponseV1
	if err := w.op.call(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/bans", nil, &bans); err != nil {
		return err
	}
	for _, b := range bans.Bans {
		if b.PrincipalID == m.opID {
			return fmt.Errorf("the refused ban of the owner is listed: %+v", b)
		}
	}
	text := m.text("the owner's word after the refused ban")
	posted, err := m.post(&member{name: "the owner", api: w.op}, "the owner's post after a refused ban", schema.PostMessageRequestV2{Body: text})
	if err != nil {
		return fmt.Errorf("the owner posting after the refused ban: %w", err)
	}
	msgs, _, err := m.listV2(w.op, "?order=desc&limit=5")
	if err != nil {
		return fmt.Errorf("the owner reading after the refused ban: %w", err)
	}
	if !slices.ContainsFunc(msgs, func(x schema.MessageV2) bool { return x.ID == posted && x.Body == text }) {
		return fmt.Errorf("the owner does not read back the message %d it posted after the refused ban", posted)
	}
	// The control.
	if err := opr.api.call(http.MethodPut, ban(ctl.id), nil, nil); err != nil {
		return fmt.Errorf("the same operator banning a plain member: %w", err)
	}
	if err := opr.api.call(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/bans", nil, &bans); err != nil {
		return err
	}
	if !slices.ContainsFunc(bans.Bans, func(b schema.ChannelBanV1) bool { return b.PrincipalID == ctl.id }) {
		return fmt.Errorf("the operator's ban of a plain member is not listed: %+v", bans)
	}
	if err := w.op.call(http.MethodDelete, ban(ctl.id), nil, nil); err != nil {
		return err
	}
	m.say("operator ban: a non-owner operator was refused the ban of the owner with 403 forbidden and one access_denied row, the owner was not listed as banned and still read and posted in the channel, and the same operator banned a plain member of it")
	return nil
}

// ---- membership removal

func (m *mod) removal() error {
	tom, err := m.person(m.pname("tom"), m.main, m.side)
	if err != nil {
		return err
	}
	if err := m.putSeat(tom); err != nil {
		return err
	}
	h, err := m.hold(tom)
	if err != nil {
		return err
	}
	// The manager removes him: a membership removal, not a ban.
	t, err := m.loses("the manager removes tom from the channel", h, func() error {
		return m.mia.api.call(http.MethodDelete, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), tom.id), nil, nil)
	})
	if err != nil {
		return err
	}
	n, err := m.outEverywhere("after tom was removed", h, h.msg, h.file, m.text("term"), m.bounds)
	if err != nil {
		return err
	}
	if removed, err := m.auditCount(store.AuditMemberRemoved, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tom.id)); err != nil {
		return err
	} else if removed != 1 {
		return fmt.Errorf("%d member_removed rows for tom, want 1", removed)
	}
	if seats, err := m.auditCount(store.AuditNetMemberRemoved, fmt.Sprintf("net:%d", m.netID), ""); err != nil {
		return err
	} else if seats < 1 {
		return fmt.Errorf("no net_member_removed row for tom's seat")
	}
	// It is not a ban: nothing is listed, and the manager adds him back.
	var bans schema.ListChannelBansResponseV1
	if err := m.mia.api.call(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/bans", nil, &bans); err != nil {
		return err
	}
	if len(bans.Bans) != 0 {
		return fmt.Errorf("a membership removal left bans: %+v", bans)
	}
	if err := m.mia.api.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), tom.id), nil, nil); err != nil {
		return fmt.Errorf("the manager adding back the member she removed: %w", err)
	}
	for _, a := range []api{tom.api, h.alt.api} {
		status, _, err := a.do(http.MethodGet, "/v2/channels/"+url.PathEscape(m.main)+"/messages", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("after the manager added tom back, %s reads the channel: %d", a.who, status)
		}
	}
	m.say("removal: tom, holding every socket variant on the channel on two credentials, the occupancy socket, a LiveKit participant and a kept join grant, was removed by the channel's manager: %s; every route of the table and every socket variant refused him and his other credential as for a ghost (%d requests), the side channel and both credentials stayed good, no ban was made, and the manager could add him back", t, n)
	return nil
}

// ---- a ban

func (m *mod) ban() error {
	w := m.w
	tara, err := m.person(m.pname("tara"), m.main, m.side)
	if err != nil {
		return err
	}
	if err := m.putSeat(tara); err != nil {
		return err
	}
	h, err := m.hold(tara)
	if err != nil {
		return err
	}
	ban := fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.main), tara.id)
	if err := w.checkRoute("PUT /v1/channels/{channel}/bans/{principal_id}", put(ban, nil)); err != nil {
		return err
	}
	// A plain member's ban changes nothing: everything tara holds is still
	// open (the gate).
	if status, _, err := m.pat.api.do(http.MethodPut, ban, nil); err != nil || status != http.StatusForbidden {
		return fmt.Errorf("a plain member's ban: status %d, %v", status, errText(err))
	}
	for _, f := range h.closing() {
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("a refused ban closed %s", f.what)
		}
	}
	t, err := m.loses("the manager bans tara", h, func() error { return m.mia.api.call(http.MethodPut, ban, nil, nil) })
	if err != nil {
		return err
	}
	n, err := m.outEverywhere("after tara was banned", h, h.msg, h.file, m.text("term"), m.bounds)
	if err != nil {
		return err
	}
	// Every path that adds an existing principal refuses the ban: the same
	// 404 principal_not_found for the operator and the manager, by id and by
	// name, and a self-join of a public channel (banned there) is refused.
	for _, c := range []struct {
		what string
		a    api
		p    probe
	}{
		{"the operator's PUT", w.op, put(fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), tara.id), nil)},
		{"the manager's PUT", m.mia.api, put(fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), tara.id), nil)},
		{"the operator's add by name", w.op, post("/v1/channels/"+url.PathEscape(m.main)+"/members", schema.AddChannelMemberRequestV1{Name: tara.name})},
		{"the manager's add by name", m.mia.api, post("/v1/channels/"+url.PathEscape(m.main)+"/members", schema.AddChannelMemberRequestV1{Name: tara.name})},
		{"the manager's add by id", m.mia.api, post("/v1/channels/"+url.PathEscape(m.main)+"/members", schema.AddChannelMemberRequestV1{ID: tara.id})},
	} {
		got, err := w.send(c.a, c.p)
		if err != nil {
			return err
		}
		if err := checkRefusal(got, answer{http.StatusNotFound, "principal_not_found"}, asked{ids: []kindID{{"principal", tara.id}}, strs: []string{tara.name}}); err != nil {
			return fmt.Errorf("%s re-adding banned tara: %w", c.what, err)
		}
	}
	if err := m.putSeatRefused(tara); err != nil {
		return err
	}
	// The list names her; the manager's own ban.
	var bans schema.ListChannelBansResponseV1
	if err := m.mia.api.call(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/bans", nil, &bans); err != nil {
		return err
	}
	if err := bans.Validate(); err != nil {
		return err
	}
	if len(bans.Bans) != 1 || bans.Bans[0].PrincipalID != tara.id || bans.Bans[0].Name != tara.name || bans.Bans[0].CreatedBy != m.mia.id {
		return fmt.Errorf("the ban list is %+v, want tara's, by the manager", bans)
	}
	if status, _, err := m.pat.api.do(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/bans", nil); err != nil || status != http.StatusForbidden {
		return fmt.Errorf("a plain member's ban list: %d %v", status, errText(err))
	}
	// A repeat is 204 and writes nothing new.
	if err := m.mia.api.call(http.MethodPut, ban, nil, nil); err != nil {
		return fmt.Errorf("a repeated ban: %w", err)
	}
	added, err := m.auditCount(store.AuditChannelBanAdded, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tara.id))
	if err != nil {
		return err
	}
	removed, err := m.auditCount(store.AuditMemberRemoved, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tara.id))
	if err != nil {
		return err
	}
	if added != 1 || removed != 1 {
		return fmt.Errorf("the audit has %d channel_ban_added and %d member_removed rows for tara, want 1 and 1 (a repeat writes none)", added, removed)
	}
	// The public channel: an operator may pre-ban a principal who is not a
	// member, and a self-join is then refused; lifting restores nothing.
	if err := w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.pub), tara.id), nil, nil); err != nil {
		return fmt.Errorf("pre-ban a non-member: %w", err)
	}
	got, err := w.send(tara.api, post("/v1/channels/"+url.PathEscape(m.pub)+"/join", nil))
	if err != nil {
		return err
	}
	if got.status < 400 || got.status > 499 {
		return fmt.Errorf("tara's self-join of the public channel she is banned from: %d %s, want a refusal", got.status, got.body)
	}
	if err := checkRefusal(got, answer{http.StatusForbidden, "channel_not_joinable"}, asked{strs: []string{m.pub}}); err != nil {
		return fmt.Errorf("tara's self-join of a public channel she is banned from: %w", err)
	}
	if err := w.op.call(http.MethodDelete, fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.pub), tara.id), nil, nil); err != nil {
		return err
	}
	if err := tara.api.call(http.MethodPost, "/v1/channels/"+url.PathEscape(m.pub)+"/join", nil, nil); err != nil {
		return fmt.Errorf("tara joining the public channel once the ban was lifted: %w", err)
	}
	h.keep = append(h.keep, m.pubID)
	m.tara, m.taraHeld = tara, h
	m.say("ban: tara, holding every socket variant on the channel on two credentials, the occupancy socket, a LiveKit participant and a kept join grant, was banned by the channel's manager: %s; every route of the table and every socket variant refused her and her other credential as for a ghost (%d requests) and the kept grant was dead, the side channel and both credentials stayed good, every path that adds an existing principal refused the ban (404 principal_not_found), the list named her by the manager, a repeat wrote nothing, and a ban pre-made by the operator on a non-member refused her self-join (403 channel_not_joinable) and lifted restored it", t, n)
	return nil
}

// putSeatRefused requires that a net seat cannot be given to a banned
// principal either: the net routes take only members, and the answer is the
// one for an existing principal who was never a member of the channel (ned).
func (m *mod) putSeatRefused(p *member) error {
	seat := func(id int64) probe {
		return put(fmt.Sprintf("/v1/channels/%s/nets/%s/members/%d", url.PathEscape(m.main), m.netName, id),
			schema.PutNetMemberRequestV1{Role: schema.NetRoleMember})
	}
	notMember := answer{http.StatusBadRequest, "not_a_channel_member"}
	got, err := m.w.send(m.w.op, seat(p.id))
	if err != nil {
		return err
	}
	if err := checkRefusal(got, notMember, asked{ids: []kindID{{"principal", p.id}}}); err != nil {
		return fmt.Errorf("a net seat for %s, banned from the channel: %w", p.name, err)
	}
	gh, err := m.w.send(m.w.op, seat(m.ned.id))
	if err != nil {
		return err
	}
	if gh.status != got.status || !bytes.Equal(scrub(gh.body, nil, m.ned.id), scrub(got.body, nil, p.id)) {
		return fmt.Errorf("a net seat for the banned %s is %d %s, for a principal who was never a member %d %s", p.name, got.status, got.body, gh.status, gh.body)
	}
	return nil
}

// ---- residual membership (§6.2, §9 case 5)

func (m *mod) residual() error {
	w := m.w
	tara, h := m.tara, m.taraHeld
	// A row the ban's cleanup did not reach, and a net seat: as a restore
	// from a backup, or a legacy row, leaves them.
	if err := m.residualMember(tara, true); err != nil {
		return err
	}
	if mem, seats, err := m.residualRows(tara); err != nil || mem != 1 || seats != 1 {
		return fmt.Errorf("the seeded rows are %d membership(s), %d seat(s), %v", mem, seats, errText(err))
	}
	// Invisible through every raw content path: a message posted now is read
	// by no one of hers, the member list does not name her, and nothing she
	// holds works.
	text := m.text("said while tara had a residual row")
	if err := m.pat.api.call(http.MethodPost, "/v2/channels/"+url.PathEscape(m.main)+"/messages", schema.PostMessageRequestV2{Body: text}, nil); err != nil {
		return err
	}
	if _, err := m.outEverywhere("with a residual membership", h, h.msg, h.file, text, m.bounds); err != nil {
		return err
	}
	var members schema.ListChannelMembersResponseV1
	if err := m.mia.api.call(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/members", nil, &members); err != nil {
		return err
	}
	for _, mem := range members.Members {
		if mem.PrincipalID == tara.id {
			return fmt.Errorf("the member list names tara, who is banned and has a residual row")
		}
	}
	var nets schema.ListNetsResponseV1
	if err := m.pat.api.call(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/nets", nil, &nets); err != nil {
		return err
	}
	seen := false
	for _, n := range nets.Nets {
		if n.ID == m.netID {
			seen = true
		}
		for _, seat := range n.Members {
			if seat.PrincipalID == tara.id {
				return fmt.Errorf("the net %d lists tara's seat, which is residual beside a ban", n.ID)
			}
		}
	}
	if !seen {
		return fmt.Errorf("the net list as pat does not hold the net: the check of its seats is vacuous")
	}
	// Her own channel list omits the channel, on both credentials.
	for _, a := range []api{tara.api, h.alt.api} {
		var list schema.ListChannelsResponse
		if err := a.call(http.MethodGet, "/v1/channels", nil, &list); err != nil {
			return err
		}
		for _, c := range list.Channels {
			if c.ID == m.mainID || c.Name == m.main {
				return fmt.Errorf("GET /v1/channels as %s lists the moderated channel, which she is banned from", a.who)
			}
		}
	}
	// A net message, said while her seat is residual, is sent to the net's
	// members who are members: no recipient row is made for her.
	netText := m.text("a net message while tara's seat is residual")
	netMsg, err := m.post(m.pat, "a net message while a seat is residual", schema.PostMessageRequestV2{Body: netText,
		Audience: &schema.Audience{Kind: schema.AudienceKindNet, NetID: m.netID}})
	if err != nil {
		return err
	}
	for _, c := range []struct {
		p    *member
		want int
	}{{tara, 0}, {m.pat, 1}, {m.wes, 1}} {
		got, err := m.w.underCount("SELECT COUNT(*) FROM message_recipients WHERE message_id = ? AND principal_id = ?", netMsg, c.p.id)
		if err != nil || got != c.want {
			return fmt.Errorf("the net message has %d recipient row(s) for %s, want %d (%s)", got, c.p.name, c.want, errText(err))
		}
	}
	// A whisper cannot be addressed to her: a recipient must be a member now.
	whisper, err := w.send(m.pat.api, post("/v2/channels/"+url.PathEscape(m.main)+"/messages", schema.PostMessageRequestV2{Body: m.text("a whisper to a banned principal"),
		Audience: &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{tara.id}}}))
	if err != nil {
		return err
	}
	if err := checkRefusal(whisper, answer{http.StatusBadRequest, "invalid_audience"}, asked{ids: []kindID{{"principal", tara.id}}}); err != nil {
		return fmt.Errorf("a whisper to tara, banned with a residual row: %w", err)
	}
	// A repeat ban cleans the row and the seat, audits it, and revokes: it
	// writes no new ban row.
	beforeRemoved, err := m.auditCount(store.AuditMemberRemoved, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tara.id))
	if err != nil {
		return err
	}
	beforeSeats, err := m.auditCount(store.AuditNetMemberRemoved, fmt.Sprintf("net:%d", m.netID), "")
	if err != nil {
		return err
	}
	ban := fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.main), tara.id)
	if err := m.mia.api.call(http.MethodPut, ban, nil, nil); err != nil {
		return err
	}
	if mem, seats, err := m.residualRows(tara); err != nil || mem != 0 || seats != 0 {
		return fmt.Errorf("after the repeat ban the rows are %d membership(s), %d seat(s), %v, want none", mem, seats, errText(err))
	}
	afterRemoved, err := m.auditCount(store.AuditMemberRemoved, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tara.id))
	if err != nil {
		return err
	}
	afterSeats, err := m.auditCount(store.AuditNetMemberRemoved, fmt.Sprintf("net:%d", m.netID), "")
	if err != nil {
		return err
	}
	if afterRemoved != beforeRemoved+1 || afterSeats != beforeSeats+1 {
		return fmt.Errorf("the repeat ban's cleanup wrote %d member_removed and %d net_member_removed rows, want 1 and 1", afterRemoved-beforeRemoved, afterSeats-beforeSeats)
	}
	added, err := m.auditCount(store.AuditChannelBanAdded, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tara.id))
	if err != nil {
		return err
	}
	if added != 1 {
		return fmt.Errorf("%d channel_ban_added rows for tara after a repeat, want 1", added)
	}
	// Seeded again, and the unban: the residual row goes first, the ban last,
	// and nothing is restored.
	if err := m.residualMember(tara, true); err != nil {
		return err
	}
	if _, err := m.outEverywhere("with a second residual membership", h, h.msg, h.file, text, m.bounds); err != nil {
		return err
	}
	if err := m.mia.api.call(http.MethodDelete, ban, nil, nil); err != nil {
		return fmt.Errorf("the manager lifting her own ban: %w", err)
	}
	if mem, seats, err := m.residualRows(tara); err != nil || mem != 0 || seats != 0 {
		return fmt.Errorf("after the unban the rows are %d membership(s), %d seat(s), %v, want none: cleanup comes before the delete", mem, seats, errText(err))
	}
	if status, _, err := tara.api.do(http.MethodGet, "/v2/channels/"+url.PathEscape(m.main)+"/messages", nil); err != nil || status != http.StatusNotFound {
		return fmt.Errorf("tara reading the channel after the unban: %d %v, want 404: unban restores nothing", status, errText(err))
	}
	if _, err := m.outEverywhere("after the unban", h, h.msg, h.file, text, m.bounds); err != nil {
		return err
	}
	removedRows, err := m.auditCount(store.AuditChannelBanRemoved, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tara.id))
	if err != nil {
		return err
	}
	if removedRows != 1 {
		return fmt.Errorf("%d channel_ban_removed rows for tara, want 1", removedRows)
	}
	// A second unban is a no-op that writes nothing.
	if err := m.mia.api.call(http.MethodDelete, ban, nil, nil); err != nil {
		return err
	}
	if again, err := m.auditCount(store.AuditChannelBanRemoved, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tara.id)); err != nil || again != 1 {
		return fmt.Errorf("a repeated unban wrote a row: %d, %v", again, errText(err))
	}
	// Only an explicit add admits her, and then it is as a new member: she
	// reads the channel and has no manager grant or net seat she once had.
	if err := m.mia.api.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", url.PathEscape(m.main), tara.id), nil, nil); err != nil {
		return fmt.Errorf("the manager adding tara after the unban: %w", err)
	}
	if status, _, err := tara.api.do(http.MethodGet, "/v2/channels/"+url.PathEscape(m.main)+"/messages", nil); err != nil || status != http.StatusOK {
		return fmt.Errorf("tara reading the channel once added: %d %v", status, errText(err))
	}
	if seats, err := m.w.underCount("SELECT COUNT(*) FROM net_members WHERE net_id = ? AND principal_id = ?", m.netID, tara.id); err != nil || seats != 0 {
		return fmt.Errorf("an unban and an add restored a net seat: %d %v", seats, errText(err))
	}
	m.say("residual: a membership row and a net seat seeded beside tara's ban were invisible on every route, socket and the occupancy and voice paths and in the member list; a repeat ban removed both (audited member_removed and net_member_removed, no new channel_ban_added), seeded again the unban removed them before the ban row, nothing was restored (404 until the manager added her), the second unban wrote nothing, and the add gave her no net seat")
	return nil
}

// ---- a ban row with a live membership: what is left of one that the
// ban's own cleanup did not reach, and the voice poller as the backstop

func (m *mod) legacyBan() error {
	w := m.w
	tess, err := m.person(m.pname("tess"), m.main, m.side)
	if err != nil {
		return err
	}
	h, err := m.hold(tess)
	if err != nil {
		return err
	}
	// The ban row alone, as a restore beside live data would have it: no
	// route made it, no socket was dropped, no room rotated. Watch LiveKit
	// from before the insert so the refusal requests below are not charged to
	// the poller's eviction time.
	type disappearance struct {
		after time.Duration
		err   error
	}
	var departed chan disappearance
	start := time.Now()
	if w.lk != nil {
		departed = make(chan disappearance, 1)
		go func() {
			deadline := start.Add(m.bounds.total() + modWait)
			for time.Now().Before(deadline) && stopCtx.Err() == nil {
				if gone, _ := w.lk.gone(h.room, tess.id); gone {
					departed <- disappearance{after: time.Since(start)}
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
			_, saw := w.lk.gone(h.room, tess.id)
			departed <- disappearance{err: fmt.Errorf("LiveKit still lists tess after %s: %s", m.bounds.total()+modWait, saw)}
		}()
	}
	if err := w.underDB("INSERT INTO channel_bans (channel_id, principal_id, created_by, created_at) VALUES (?, ?, NULL, ?)", m.mainID, tess.id, time.Now().UnixMilli()); err != nil {
		return err
	}
	// Nothing new works for her at once, on a credential or the other.
	for _, a := range []api{tess.api, h.alt.api} {
		if _, err := m.outAs(a, a.who, h.msg, h.file, m.text("term")); err != nil {
			return fmt.Errorf("a ban row beside a live membership: %w", err)
		}
		if _, err := m.socketsRefused(a, a.who, true); err != nil {
			return err
		}
	}
	// The voice poller is the backstop for what no route revoked: LiveKit
	// stops listing her within one pass and a room rotation.
	if w.lk != nil {
		d := <-departed
		if d.err != nil {
			return d.err
		}
		took := d.after
		if took > m.bounds.total() {
			return fmt.Errorf("the voice poller took %s to remove a banned participant, bound %s", took.Round(time.Millisecond), m.bounds.total())
		}
		m.notes = append(m.notes, fmt.Sprintf("the poller removed the participant after %s", took.Round(10*time.Millisecond)))
	}
	// The ban's own repeat is the cleanup: membership gone, sockets closed
	// on both credentials within the bound.
	// The operator repeats it: a manager repeats only a ban of her own, and
	// this one has no maker.
	t, err := m.loses("a repeat ban cleans tess's residual membership", h, func() error {
		return w.op.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/bans/%d", url.PathEscape(m.main), tess.id), nil, nil)
	})
	if err != nil {
		return err
	}
	if mem, _, err := m.residualRows(tess); err != nil || mem != 0 {
		return fmt.Errorf("tess's membership after the repeat ban: %d %v", mem, errText(err))
	}
	added, err := m.auditCount(store.AuditChannelBanAdded, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tess.id))
	if err != nil {
		return err
	}
	removed, err := m.auditCount(store.AuditMemberRemoved, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d", tess.id))
	if err != nil {
		return err
	}
	if added != 0 || removed != 1 {
		return fmt.Errorf("a repeat ban over a ban row wrote %d channel_ban_added and %d member_removed rows, want 0 and 1", added, removed)
	}
	if _, err := m.outEverywhere("after the repeat ban", h, h.msg, h.file, m.text("term"), m.bounds); err != nil {
		return err
	}
	m.say("residual ban row: a ban row written beside tess's live membership (her sockets on two credentials, the occupancy socket and a participant held) refused every new request and socket at once%s; a repeat ban closed her sockets and cleaned the membership: %s, audited member_removed with no new channel_ban_added", noteOf(m.notes), t)
	return nil
}

func noteOf(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	return " (" + strings.Join(notes, "; ") + ")"
}

// ---- the ban and the final voice entitlement check (§6.2)

// voiceRace asks for voice sessions as fast as it can while the manager bans
// the asker. Each request is answered one way or the other: a refusal (the
// final check saw the ban, no token was signed), or a grant (the check came
// first, and the ban's rotation must make it no use). After the ban, a
// session is refused, and no grant that was signed lets its holder into
// LiveKit.
func (m *mod) voiceRace() error {
	w := m.w
	if w.lk == nil {
		return nil
	}
	rex, err := m.person(m.pname("rex"), m.main, m.side)
	if err != nil {
		return err
	}
	ch := url.PathEscape(m.main)
	// sent is when the request left, at when its answer came back.
	type outcome struct {
		status   int
		code     string
		grant    schema.VoiceRoomGrant
		sent, at time.Time
	}
	var mu sync.Mutex
	var outs []outcome
	var failed []error
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(3 * time.Millisecond):
				}
				sent := time.Now()
				status, body, err := rex.api.do(http.MethodPost, "/v1/channels/"+ch+"/voice/session", nil)
				if err != nil {
					mu.Lock()
					failed = append(failed, err)
					mu.Unlock()
					return
				}
				o := outcome{status: status, code: errorCode(body), sent: sent, at: time.Now()}
				if status == http.StatusOK {
					var s schema.VoiceSessionResponseV1
					if json.Unmarshal(body, &s) == nil && len(s.Rooms) == 1 {
						o.grant = s.Rooms[0]
						w.h.secret("a voice join token", o.grant.Token)
						w.h.hide("a voice room name", o.grant.Room)
					}
				}
				mu.Lock()
				outs = append(outs, o)
				mu.Unlock()
			}
		}()
	}
	// Enough grants before the ban and enough refusals after it, whatever the
	// runner's speed: the ban waits for the first, the end for the second.
	const enough = 20
	count := func(match func(outcome) bool) int {
		mu.Lock()
		defer mu.Unlock()
		n := 0
		for _, o := range outs {
			if match(o) {
				n++
			}
		}
		return n
	}
	before := waitFor("session grants before the ban", modWait, func() (bool, string) {
		n := count(func(o outcome) bool { return o.status == http.StatusOK })
		return n >= enough, fmt.Sprintf("%d grants", n)
	})
	banSent := time.Now()
	if before == nil {
		err = m.mia.api.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/bans/%d", ch, rex.id), nil, nil)
	}
	banned := time.Now()
	after := error(nil)
	if before == nil && err == nil {
		after = waitFor("session refusals after the ban", modWait, func() (bool, string) {
			n := count(func(o outcome) bool { return o.sent.After(banned) && o.status == http.StatusNotFound })
			return n >= enough, fmt.Sprintf("%d refusals", n)
		})
	}
	close(stop)
	wg.Wait()
	if before != nil {
		return before
	}
	if err != nil {
		return err
	}
	if after != nil {
		return after
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d session requests failed in transport, the first: %w", len(failed), failed[0])
	}
	// Exact, not timed: the ban has committed when its answer arrives, so a
	// request sent after that must be refused; and nothing can see the ban
	// before its request was sent. A grant answered late is not a failure: its
	// check may have come first on a slow runner.
	var granted, refused, other, beforeBan, afterBan int
	for _, o := range outs {
		switch o.status {
		case http.StatusOK:
			granted++
			if o.sent.After(banned) {
				afterBan++
			}
		case http.StatusNotFound:
			if o.code != "channel_not_found" {
				other++
				continue
			}
			refused++
			if o.at.Before(banSent) {
				beforeBan++
			}
		default:
			other++
		}
	}
	if other > 0 {
		return fmt.Errorf("%d session requests were answered neither 200 nor 404 channel_not_found", other)
	}
	if granted < enough || refused < enough {
		return fmt.Errorf("the race had %d grants and %d refusals: it did not hit both sides of the ban", granted, refused)
	}
	if afterBan > 0 {
		return fmt.Errorf("%d grants answered requests sent after the ban's response", afterBan)
	}
	if beforeBan > 0 {
		return fmt.Errorf("%d session requests were refused before the ban was sent", beforeBan)
	}
	// Every grant ever signed is dead once the ban's rotation has run; the
	// ones signed before the ban committed have no room to join.
	for i, o := range outs {
		if o.status != http.StatusOK || o.grant.Token == "" {
			continue
		}
		if err := m.joinFails(fmt.Sprintf("grant %d signed %s before the ban's answer", i, banned.Sub(o.at).Round(time.Millisecond)), o.grant.Token); err != nil {
			return err
		}
	}
	if err := expectRefusalAs(rex.api, http.MethodPost, "/v1/channels/"+ch+"/voice/session", channelNotFound); err != nil {
		return fmt.Errorf("a session after the ban: %w", err)
	}
	time.Sleep(m.bounds.pass)
	if where, err := w.lk.anywhere(rex.id); err != nil || where != 0 {
		return fmt.Errorf("LiveKit lists rex in %d rooms a poller pass after the ban: %v", where, errText(err))
	}
	m.say("voice race: rex asked for sessions on four connections while the manager banned him: %d were refused (the final entitlement check saw the ban, no token signed) and %d were granted (the check came first), none after the ban's answer; every grant signed was refused by LiveKit afterwards (404, the room was rotated), a session after the ban is 404, and LiveKit listed him nowhere a poller pass later", refused, granted)
	return nil
}

// expectRefusalAs requires the refusal want of a request as a.
func expectRefusalAs(a api, method, path string, want answer) error {
	status, body, err := a.do(method, path, nil)
	if err != nil {
		return err
	}
	return checkRefusal(result{status: status, body: body}, want, asked{})
}

// ---- B's sockets, at the end of the phase

// bQuiet requires that none of the frames B's open sockets carried while the
// phase ran holds a private string of M's or an id of its channels.
func (m *mod) bQuiet() error {
	// The barrier of the other checks of B's sockets: B says something on
	// every channel and every socket must carry it, so that what was in
	// flight while the phase ran has arrived and is counted.
	c, err := m.w.controlB(3)
	if err != nil {
		return fmt.Errorf("the control of B after the moderation phase: %w", err)
	}
	if err := m.bsock.checkQuiet(m.w, c); err != nil {
		return fmt.Errorf("the sockets of B after the moderation phase: %w", err)
	}
	n := 0
	for _, f := range m.bsock.feeds {
		for i, fr := range f.since(m.bSeen[f]) {
			n++
			if leaks := m.marks.leaks(fr, asked{}); len(leaks) > 0 {
				return fmt.Errorf("frame %d on %s, carried during the moderation phase, holds %s", i, f.what, strings.Join(leaks, "; "))
			}
		}
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s closed during the moderation phase", f.what)
		}
	}
	m.say("B's sockets: after B's own control had reached all of them, the %d frames B's %d open sockets carried while the phase ran hold nothing of M's", n, len(m.bsock.feeds))
	return nil
}

// errText is err for a message about a check that failed on its value or its
// error, either of which may be nil.
func errText(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}

// ---- voice-only removal (§4, §9 case 3)

// voiceKick: the manager removes one person from voice. Only that person's
// participant and voice sockets go; their message, events and other sockets
// stay open, they stay a member, another in the room is not disturbed, and
// both of their credentials can be given a session and join again at once.
func (m *mod) voiceKick() error {
	w := m.w
	if w.lk == nil {
		return nil
	}
	vic, err := m.person(m.pname("vic"), m.main, m.side)
	if err != nil {
		return err
	}
	wil, err := m.person(m.pname("wil"), m.main, m.side)
	if err != nil {
		return err
	}
	hv, err := m.hold(vic)
	if err != nil {
		return err
	}
	hw, err := m.hold(wil)
	if err != nil {
		return err
	}
	kick := fmt.Sprintf("/v1/channels/%s/voice/participants/%d", url.PathEscape(m.main), vic.id)
	if err := w.checkRoute("DELETE /v1/channels/{channel}/voice/participants/{principal_id}", del(kick)); err != nil {
		return err
	}
	start := time.Now()
	if err := m.mia.api.call(http.MethodDelete, kick, nil, nil); err != nil {
		return fmt.Errorf("the manager removing vic from voice: %w", err)
	}
	if err := waitFor("LiveKit to stop listing vic", m.bounds.total(), func() (bool, string) { return w.lk.gone(hv.room, vic.id) }); err != nil {
		return err
	}
	took := time.Since(start)
	if ok, _, saw := w.lk.has(hw.room, wil.id); !ok {
		return fmt.Errorf("removing vic from voice took wil out of the room: %s", saw)
	}
	// Her voice sockets close; every other socket stays open, on both
	// credentials.
	var voice, others []*feed
	for _, f := range hv.closing() {
		if kindOf(f) == kindVoice {
			voice = append(voice, f)
		} else {
			others = append(others, f)
		}
	}
	for _, f := range voice {
		if err := waitFor(f.what+" to close", modWait, func() (bool, string) { ended, _ := f.ended(); return ended, "still open" }); err != nil {
			return err
		}
	}
	for _, f := range append(others, append(hv.side, hv.occ...)...) {
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s closed when vic was removed from voice only", f.what)
		}
	}
	for _, f := range hw.closing() {
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s of the person left in the room closed", f.what)
		}
	}
	// Still a member, still reading, still posting; no ban.
	var members schema.ListChannelMembersResponseV1
	if err := m.mia.api.call(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/members", nil, &members); err != nil {
		return err
	}
	if !slices.ContainsFunc(members.Members, func(c schema.ChannelMemberV1) bool { return c.PrincipalID == vic.id }) {
		return fmt.Errorf("vic is not a member after a voice-only removal")
	}
	for _, a := range []api{vic.api, hv.alt.api} {
		if _, err := m.post2(a, m.text("said after a voice removal")); err != nil {
			return err
		}
	}
	var bans schema.ListChannelBansResponseV1
	if err := m.mia.api.call(http.MethodGet, "/v1/channels/"+url.PathEscape(m.main)+"/bans", nil, &bans); err != nil {
		return err
	}
	for _, b := range bans.Bans {
		if b.PrincipalID == vic.id {
			return fmt.Errorf("a voice-only removal made a ban")
		}
	}
	// A plain member may not, and nothing changed for it.
	if got, err := m.pat.api.do2(http.MethodDelete, kick); err != nil {
		return err
	} else if err := checkRefusal(got, operatorOnly, asked{ids: []kindID{{"principal", vic.id}}}); err != nil {
		return fmt.Errorf("a plain member removing vic from voice: %w", err)
	}
	// Back in at once, on either credential.
	for _, a := range []api{vic.api, hv.alt.api} {
		sess, err := w.session(&member{name: a.who, id: vic.id, api: a}, url.PathEscape(m.main))
		if err != nil {
			return fmt.Errorf("%s asking for a session after being removed from voice: %w", a.who, err)
		}
		if _, err := w.joinSignal(sess.Rooms[0]); err != nil {
			return fmt.Errorf("%s joining after being removed from voice: %w", a.who, err)
		}
		if err := waitFor("LiveKit to list vic again", modWait, func() (bool, string) {
			ok, _, saw := w.lk.has(sess.Rooms[0].Room, vic.id)
			return ok, saw
		}); err != nil {
			return err
		}
	}
	n, err := m.auditCount(store.AuditVoiceParticipantKicked, fmt.Sprintf("channel:%d", m.mainID), fmt.Sprintf("principal=%d result=completed", vic.id))
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%d voice_participant_kicked rows for vic, want 1", n)
	}
	m.say("voice-only removal: the manager removed vic from voice: LiveKit stopped listing only her within %s, her voice sockets closed and her %d message, events, side-channel and occupancy sockets on two credentials stayed open, wil stayed in the room, she stayed a member who could still post and no ban was made, a plain member was refused, and she was given a session and joined again at once on either credential (audited voice_participant_kicked, completed)", took.Round(time.Millisecond), len(others)+len(hv.side)+len(hv.occ))
	return nil
}

// post2 posts a channel-wide v2 message of the moderated channel as a.
func (m *mod) post2(a api, body string) (int64, error) {
	var resp schema.PostMessageResponseV2
	if err := a.call(http.MethodPost, "/v2/channels/"+url.PathEscape(m.main)+"/messages", schema.PostMessageRequestV2{Body: body}, &resp); err != nil {
		return 0, err
	}
	return resp.Message.ID, nil
}
