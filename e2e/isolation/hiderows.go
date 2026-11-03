package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/njdaniel/conch/e2e/internal/routetable"
	"github.com/njdaniel/conch/pkg/schema"
)

// The hide-members rows (community-model §13.9, issue #988, filled by
// issue #1190). Under the personal preset the member column lacks
// see_member_directory, which is the whole of "hide members from each
// other": a member may know only themself, the people they share a channel
// with and the people their own links brought in (§13.2, O14). The world:
//
//   - the owner, an operator, in every channel below;
//   - alice, in p1 alone, p1's manager, with an agent in p1 and a departed
//     author, dep, who posted in p1 and was removed;
//   - carol, who joined through alice's member link and is in no channel of
//     alice's (O14b);
//   - bob, in p2 alone, and zed, in p3 alone: the people alice may not know.
//     Their names and ids are markers in alice's register (hideWorld.marks):
//     they come back from no route alice reads while the cell is off, except
//     where a row documents them.
//
// hidePersonalRows runs with the cell off (personal) and hideGroupRows with
// it on (group): the same requests, the answer widening.

// hideWorld is the world of §13.9.
type hideWorld struct {
	owner                              int64
	p1, p2, p3                         string
	p1ID, msgID                        int64
	alice, bob, zed, dep, carol, agent *member
	aliceLink, bobLink                 schema.CreateInviteResponseV2
	marks, marksBob                    *markers
	ghostName                          string
	// aliceF, bobF, zedF and ownerF are events sockets, open from before
	// the cell is cut: the directory signal travels on them.
	aliceF, bobF, zedF, ownerF *feed
}

// buildHideWorld makes the people, the channels, the links and carol. It
// runs with the member column as it is by default, since the links are made
// by members. mtag is bob's and zed's own tag, held by no one else's name,
// so that a run of six of their characters is theirs alone.
func (w *world) buildHideWorld(name func(string) string, mtag, atag string) (*hideWorld, error) {
	hw := &hideWorld{marks: newMarkers(), marksBob: newMarkers(), ghostName: "ghost-" + mtag}
	var who schema.WhoAmIResponseV1
	if err := w.op.call(http.MethodGet, "/v1/whoami", nil, &who); err != nil {
		return nil, err
	}
	hw.owner = who.ID
	yes := true
	channel := func(label string) (string, int64, error) {
		n := name(label)
		var c schema.CreateChannelResponse
		if err := w.op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: n}, &c); err != nil {
			return "", 0, err
		}
		if err := w.op.call(http.MethodPut, "/v1/channels/"+n+"/members/"+itoa(hw.owner), nil, nil); err != nil {
			return "", 0, err
		}
		return n, c.Channel.ID, nil
	}
	var err error
	if hw.p1, hw.p1ID, err = channel("alices-room"); err != nil {
		return nil, err
	}
	if hw.p2, _, err = channel("bobs-room"); err != nil {
		return nil, err
	}
	if hw.p3, _, err = channel("zeds-room"); err != nil {
		return nil, err
	}
	person := func(n, channel string) (*member, error) {
		m, err := w.newPerson(n, channel)
		if err != nil {
			return nil, err
		}
		// The person's own brake is open, so that the column decides.
		if err := w.op.call(http.MethodPut, "/v1/principals/"+itoa(m.id)+"/invite-permission", schema.InvitePermissionRequestV1{MayInvite: true}, nil); err != nil {
			return nil, err
		}
		return m, nil
	}
	if hw.alice, err = person("alice"+atag, hw.p1); err != nil {
		return nil, err
	}
	if err := w.op.call(http.MethodPut, "/v1/channels/"+hw.p1+"/members/"+itoa(hw.alice.id), schema.PutChannelMemberRequestV1{Manages: &yes}, nil); err != nil {
		return nil, err
	}
	if hw.dep, err = person(name("departed"), hw.p1); err != nil {
		return nil, err
	}
	if hw.bob, err = person("bob-"+mtag, hw.p2); err != nil {
		return nil, err
	}
	if hw.zed, err = person("zed-"+mtag, hw.p3); err != nil {
		return nil, err
	}
	hw.marks.str("bob's name", hw.bob.name)
	hw.marks.str("zed's name", hw.zed.name)
	hw.marks.id("principal", "bob's id", hw.bob.id)
	hw.marks.id("principal", "zed's id", hw.zed.id)
	if hw.agent, err = w.newAgent("agent"+atag, &group{channels: []string{hw.p1}, chanIDs: []int64{hw.p1ID}}); err != nil {
		return nil, err
	}
	// dep wrote in p1 and left it; alice read p1 from before.
	var posted schema.PostMessageResponseV1
	if err := hw.dep.api.call(http.MethodPost, "/v1/channels/"+hw.p1+"/messages", schema.PostMessageRequestV1{AuthorID: hw.dep.id, Body: "a word before leaving"}, &posted); err != nil {
		return nil, err
	}
	hw.msgID = posted.Message.ID
	if err := w.op.call(http.MethodDelete, "/v1/channels/"+hw.p1+"/members/"+itoa(hw.dep.id), nil, nil); err != nil {
		return nil, err
	}
	// Alice's member link, redeemed by carol; bob's, redeemed by no one.
	if err := hw.alice.api.call(http.MethodPost, "/v2/invites", schema.CreateInviteRequestV2{Kind: schema.InviteKindMember}, &hw.aliceLink); err != nil {
		return nil, fmt.Errorf("alice makes a member link: %w", err)
	}
	w.h.secret("alice's member link code", hw.aliceLink.Code)
	if err := hw.bob.api.call(http.MethodPost, "/v2/invites", schema.CreateInviteRequestV2{Kind: schema.InviteKindMember}, &hw.bobLink); err != nil {
		return nil, fmt.Errorf("bob makes a member link: %w", err)
	}
	w.h.secret("bob's member link code", hw.bobLink.Code)
	cname := "carol" + atag
	var resp schema.RedeemInviteResponseV1
	if err := w.newcomer(cname+" (joining)").call(http.MethodPost, "/v2/invites/redeem",
		schema.RedeemInviteRequestV2{Code: hw.aliceLink.Code, Name: cname, Password: joinPassword}, &resp); err != nil {
		return nil, fmt.Errorf("carol joins through alice's link: %w", err)
	}
	w.h.secret(cname+"'s credential", resp.Token)
	hw.carol = &member{name: cname, id: resp.Principal.ID, token: resp.Token, credID: resp.Credential.ID, password: true}
	hw.carol.api = w.as(hw.carol)
	// Alice's, carol's and the agent's names and ids are markers in bob's
	// register (§13.9).
	for _, m := range []*member{hw.alice, hw.carol, hw.agent} {
		hw.marksBob.str(m.name+"'s name", m.name)
		hw.marksBob.id("principal", m.name+"'s id", m.id)
	}
	// Sockets, open before the cell is cut.
	open := func(a api, what, channel string) (*feed, error) {
		f, r, err := dialSocket(a, what, "/v1/events/ws", url.Values{"channel": {channel}}, false)
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, fmt.Errorf("%s: status %d %s", what, r.status, w.h.redact(string(r.body)))
		}
		return f, waitFor(what+" to send its roster", 30*time.Second, func() (bool, string) { return len(f.received()) > 0, "no frame" })
	}
	if hw.aliceF, err = open(hw.alice.api, "alice's events socket", hw.p1); err != nil {
		return nil, err
	}
	if hw.bobF, err = open(hw.bob.api, "bob's events socket", hw.p2); err != nil {
		return nil, err
	}
	if hw.zedF, err = open(hw.zed.api, "zed's events socket", hw.p3); err != nil {
		return nil, err
	}
	if hw.ownerF, err = open(w.op, "the owner's events socket", hw.p1); err != nil {
		return nil, err
	}
	// Everything that building the world signalled has had its floor.
	select {
	case <-stopCtx.Done():
		return nil, errInterrupted
	case <-time.After(ccQuiet):
	}
	return hw, nil
}

// closeSockets closes the world's sockets.
func (hw *hideWorld) closeSockets() {
	for _, f := range []*feed{hw.aliceF, hw.bobF, hw.zedF, hw.ownerF} {
		if f != nil {
			f.close()
		}
	}
}

// directoryChangedWire is the frame of the directory signal, exactly.
const directoryChangedWire = `{"schema":"conch.directory_changed.v1"}`

// directoryFrames counts f's conch.directory_changed.v1 frames, and fails on
// one that is not exactly the wire form.
func directoryFrames(f *feed) (int, error) {
	n := 0
	for _, fr := range f.received() {
		if !strings.Contains(string(fr), schema.DirectoryChangedSchemaV1) {
			continue
		}
		if string(fr) != directoryChangedWire {
			return n, fmt.Errorf("%s carried %s, want exactly %s", f.what, fr, directoryChangedWire)
		}
		n++
	}
	return n, nil
}

// signalStep runs act, waits for each of want's sockets to carry exactly one
// more directory signal, waits ccQuiet, and requires that every other socket
// of the world carried none and that none closed.
func (hw *hideWorld) signalStep(row string, act func() error, want ...*feed) error {
	all := []*feed{hw.aliceF, hw.bobF, hw.zedF, hw.ownerF}
	base := map[*feed]int{}
	for _, f := range all {
		n, err := directoryFrames(f)
		if err != nil {
			return err
		}
		base[f] = n
	}
	if err := act(); err != nil {
		return fmt.Errorf("%s: %w", row, err)
	}
	for _, f := range want {
		if err := waitFor(f.what+" to carry directory_changed ("+row+")", 30*time.Second, func() (bool, string) {
			n, err := directoryFrames(f)
			if err != nil {
				return true, ""
			}
			return n > base[f], fmt.Sprintf("%d frames, %d of them directory_changed", len(f.received()), n)
		}); err != nil {
			return err
		}
	}
	select {
	case <-stopCtx.Done():
		return errInterrupted
	case <-time.After(ccQuiet):
	}
	for _, f := range all {
		n, err := directoryFrames(f)
		if err != nil {
			return fmt.Errorf("%s: %w", row, err)
		}
		expected := 0
		if slices.Contains(want, f) {
			expected = 1
		}
		if got := n - base[f]; got != expected {
			return fmt.Errorf("%s: %s carried %d directory_changed frames, want %d", row, f.what, got, expected)
		}
		if ended, _ := f.ended(); ended {
			return fmt.Errorf("%s: %s closed", row, f.what)
		}
	}
	return nil
}

// scan requires that a response holds no marker of marks.
func (hw *hideWorld) scan(marks *markers, what string, body []byte) error {
	if leaks := marks.leaks(body, asked{}); len(leaks) > 0 {
		return fmt.Errorf("%s holds %s", what, strings.Join(leaks, "; "))
	}
	return nil
}

// directoryOf pages the directory as a, limit rows a page, and answers the
// members listed, the scope, the number of pages, and every page's body. It
// fails on a page that is not a valid one, a scope that changes, or a
// cursor that is not the last row of its page.
func directoryOf(a api, limit int) (listed map[int64]schema.ServerMemberV1, scope schema.DirectoryScope, pages int, bodies [][]byte, err error) {
	listed = map[int64]schema.ServerMemberV1{}
	var after int64
	for {
		path := fmt.Sprintf("/v1/directory/members?limit=%d", limit)
		if after != 0 {
			path += fmt.Sprintf("&after=%d", after)
		}
		status, body, derr := a.do(http.MethodGet, path, nil)
		if derr != nil {
			return nil, "", 0, nil, derr
		}
		if status != http.StatusOK {
			return nil, "", 0, nil, fmt.Errorf("GET %s as %s = %d %s", path, a.who, status, body)
		}
		var page schema.ServerMembersResponseV1
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, "", 0, nil, err
		}
		if err := page.Validate(); err != nil {
			return nil, "", 0, nil, fmt.Errorf("the directory page of %s: %w", a.who, err)
		}
		if pages > 0 && page.Scope != scope {
			return nil, "", 0, nil, fmt.Errorf("the directory's scope changed from %q to %q between pages", scope, page.Scope)
		}
		scope = page.Scope
		pages++
		bodies = append(bodies, body)
		for _, m := range page.Members {
			if _, dup := listed[m.ID]; dup {
				return nil, "", 0, nil, fmt.Errorf("the directory lists %d twice", m.ID)
			}
			listed[m.ID] = m
		}
		if page.After == 0 {
			return listed, scope, pages, bodies, nil
		}
		if len(page.Members) == 0 || page.Members[len(page.Members)-1].ID != page.After {
			return nil, "", 0, nil, fmt.Errorf("the directory's cursor %d is not the last row of its page", page.After)
		}
		after = page.After
		if pages > 5000 {
			return nil, "", 0, nil, fmt.Errorf("the directory has no end")
		}
	}
}

// idsOf is the sorted ids of listed.
func idsOf(listed map[int64]schema.ServerMemberV1) []int64 {
	out := make([]int64, 0, len(listed))
	for id := range listed {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// sameIDs requires that listed holds exactly the ids of want.
func sameIDs(what string, listed map[int64]schema.ServerMemberV1, want ...*member) error {
	ids := make([]int64, 0, len(want))
	for _, m := range want {
		ids = append(ids, m.id)
	}
	slices.Sort(ids)
	if got := idsOf(listed); !slices.Equal(got, ids) {
		names := make([]string, 0, len(listed))
		for _, id := range idsOf(listed) {
			names = append(names, fmt.Sprintf("%s (%d)", listed[id].Name, id))
		}
		return fmt.Errorf("%s lists %s, want ids %v", what, strings.Join(names, ", "), ids)
	}
	return nil
}

// hideRead is a route alice reads that can name a principal.
type hideRead struct{ route, path string }

// reads are the routes alice reads that can name a principal and need no
// object but her own channel and link: each is asked as alice and its
// answer scanned for bob's and zed's markers. The route is the table's text
// (checked against the table by TestHideReadsAreRoutes).
func (hw *hideWorld) reads(p1 string, linkID, authorID int64) []hideRead {
	c := "/v1/channels/" + p1
	return []hideRead{
		{"GET /v1/whoami", "/v1/whoami"},
		{"GET /v1/channels", "/v1/channels"},
		{"GET /v1/directory/channels", "/v1/directory/channels"},
		{"GET /v1/directory/members", "/v1/directory/members"},
		{"GET /v1/channels/{channel}/members", c + "/members"},
		{"GET /v1/channels/{channel}/authors", fmt.Sprintf("%s/authors?ids=%d", c, authorID)},
		{"GET /v1/channels/{channel}/nets", c + "/nets"},
		{"GET /v1/channels/{channel}/voice", c + "/voice"},
		{"GET /v1/voice/occupancy", "/v1/voice/occupancy"},
		{"GET /v1/channels/{channel}/messages", c + "/messages"},
		{"GET /v2/channels/{channel}/messages", "/v2/channels/" + p1 + "/messages"},
		{"GET /v2/channels/{channel}/messages/search", "/v2/channels/" + p1 + "/messages/search?q=word"},
		{"GET /v2/channels/{channel}/reactions", fmt.Sprintf("/v2/channels/%s/reactions?messages=%d", p1, hw.msgID)},
		{"GET /v1/sessions", "/v1/sessions?channel=" + url.QueryEscape(p1)},
		{"GET /v1/runs", "/v1/runs?channel=" + url.QueryEscape(p1)},
		{"GET /v1/read-state", "/v1/read-state"},
		{"GET /v1/approvals", "/v1/approvals"},
		{"GET /v1/invites", "/v1/invites"},
		{"GET /v2/invites", "/v2/invites"},
		{"GET /v2/invites/{id}/joiners", fmt.Sprintf("/v2/invites/%d/joiners", linkID)},
		{"GET /v1/credentials/mine", "/v1/credentials/mine"},
	}
}

// checkReads asks every read of who (alice or bob, over their own channel,
// link and an author id) and requires 200 and no marker of marks, in the
// body or in the frames f, who's events socket, has carried.
func (w *world) checkReads(hw *hideWorld, phase string, who *member, marks *markers, f *feed, p1 string, linkID, authorID int64) (int, error) {
	n := 0
	for _, rd := range hw.reads(p1, linkID, authorID) {
		status, body, err := who.api.do(http.MethodGet, rd.path, nil)
		if err != nil {
			return n, err
		}
		if status != http.StatusOK {
			return n, fmt.Errorf("%s: %s reads %s = %d %s, want 200", phase, who.name, rd.path, status, w.h.redact(string(body)))
		}
		if err := hw.scan(marks, phase+": the read of "+rd.route+" as "+who.name, body); err != nil {
			return n, err
		}
		n++
	}
	for i, fr := range f.received() {
		if err := hw.scan(marks, fmt.Sprintf("%s: frame %d of %s", phase, i, f.what), fr); err != nil {
			return n, err
		}
	}
	return n + 1, nil
}

// checkHideReadsAreRoutes requires that every read of the world is a request
// its route serves, and that the route is in the table.
func checkHideReadsAreRoutes(table []routetable.Route) error {
	hw := &hideWorld{}
	for _, rd := range hw.reads("p1", 1, 1) {
		r, err := routetable.Parse(rd.route)
		if err != nil {
			return err
		}
		path, _, _ := strings.Cut(rd.path, "?")
		if !r.Serves(http.MethodGet, path) {
			return fmt.Errorf("the hide-members read %s is not one %q serves", rd.path, rd.route)
		}
		if !slices.ContainsFunc(table, func(t routetable.Route) bool { return t.Text == rd.route }) {
			return fmt.Errorf("the hide-members read %q is not a route of the table", rd.route)
		}
	}
	return nil
}

// hidePersonalRows runs the §13.9 rows with the cell off. It answers how
// many rows were run.
func (w *world) hidePersonalRows(hw *hideWorld) (int, error) {
	const dirRow = "the directory of a member without see_member_directory"
	rows := 0
	owner := &member{id: hw.owner}
	// The directory, per viewer.
	listed, scope, _, bodies, err := directoryOf(hw.alice.api, 100)
	if err != nil {
		return rows, err
	}
	if scope != schema.DirectoryScopeSharedChannels {
		return rows, fmt.Errorf("%s: alice's scope is %q, want %q", dirRow, scope, schema.DirectoryScopeSharedChannels)
	}
	if err := sameIDs(dirRow+": alice", listed, hw.alice, hw.agent, hw.carol, owner); err != nil {
		return rows, err
	}
	for _, b := range bodies {
		if err := hw.scan(hw.marks, dirRow+": alice's page", b); err != nil {
			return rows, err
		}
	}
	rows++
	for _, v := range []struct {
		who  *member
		want []*member
	}{
		{hw.bob, []*member{hw.bob, owner}},
		{hw.zed, []*member{hw.zed, owner}},
	} {
		l, s, _, _, err := directoryOf(v.who.api, 100)
		if err != nil {
			return rows, err
		}
		if s != schema.DirectoryScopeSharedChannels {
			return rows, fmt.Errorf("%s: %s's scope is %q", dirRow, v.who.name, s)
		}
		if err := sameIDs(dirRow+": "+v.who.name, l, v.want...); err != nil {
			return rows, err
		}
		rows++
	}
	// The owner is an operator: everyone, with the operator fields.
	ol, os, _, _, err := directoryOf(w.op, 200)
	if err != nil {
		return rows, err
	}
	if os != schema.DirectoryScopeEveryone {
		return rows, fmt.Errorf("%s: the owner's scope is %q, want everyone", dirRow, os)
	}
	for _, m := range []*member{hw.alice, hw.bob, hw.zed, hw.carol, hw.agent} {
		if _, ok := ol[m.id]; !ok {
			return rows, fmt.Errorf("%s: the owner's directory lacks %s", dirRow, m.name)
		}
	}
	if ol[hw.alice.id].VoiceAllowed == nil {
		return rows, fmt.Errorf("%s: the owner sees none of the operator fields", dirRow)
	}
	rows++
	// An agent is refused, the same under both phases.
	status, body, err := hw.agent.api.do(http.MethodGet, "/v1/directory/members", nil)
	if err != nil {
		return rows, err
	}
	if _, err := checkAnswer(result{status: status, body: body}, answer{http.StatusForbidden, "forbidden"}); err != nil {
		return rows, fmt.Errorf("%s: an agent: %w", dirRow, err)
	}
	rows++
	// Paging: a row a page, every cursor a listed id (directoryOf checks
	// each is the last row of its page), the last page has none; the same
	// set as one page, and no marker in any page.
	pl, _, pages, pbodies, err := directoryOf(hw.alice.api, 1)
	if err != nil {
		return rows, err
	}
	if pages != len(pl) || !slices.Equal(idsOf(pl), idsOf(listed)) {
		return rows, fmt.Errorf("%s: paging a row at a time gave %d pages and %v, want %d pages and %v", dirRow, pages, idsOf(pl), len(listed), idsOf(listed))
	}
	for _, b := range pbodies {
		if err := hw.scan(hw.marks, dirRow+": alice's page of one row", b); err != nil {
			return rows, err
		}
	}
	rows++

	// Every route alice reads names neither bob nor zed.
	n, err := w.checkReads(hw, "personal", hw.alice, hw.marks, hw.aliceF, hw.p1, hw.aliceLink.Invite.ID, hw.dep.id)
	if err != nil {
		return rows, err
	}
	rows += n
	// And the other way (§13.9): alice's, carol's and the agent's names and
	// ids are markers in bob's register, over the same routes.
	n, err = w.checkReads(hw, "personal", hw.bob, hw.marksBob, hw.bobF, hw.p2, hw.bobLink.Invite.ID, hw.bob.id)
	if err != nil {
		return rows, err
	}
	rows += n

	// R5, accepted and pinned: a name taken is told as taken, by the
	// rename and by the redemption, for a person alice may not know.
	status, body, err = hw.alice.api.do(http.MethodPut, "/v1/principals/"+itoa(hw.alice.id)+"/name", schema.RenamePrincipalRequestV1{Name: hw.bob.name})
	if err != nil {
		return rows, err
	}
	if _, err := checkAnswer(result{status: status, body: body}, answer{http.StatusConflict, "principal_exists"}); err != nil {
		return rows, fmt.Errorf("R5: alice renames herself to bob's name: %w", err)
	}
	rows++

	// A manager adds by id and by name: bob and a ghost are one 404.
	add := func(path string) (int, []byte, error) { return hw.alice.api.do(http.MethodPut, path, nil) }
	s1, b1, err := add("/v1/channels/" + hw.p1 + "/members/" + itoa(hw.bob.id))
	if err != nil {
		return rows, err
	}
	s2, b2, err := add("/v1/channels/" + hw.p1 + "/members/987654321")
	if err != nil {
		return rows, err
	}
	if _, err := checkAnswer(result{status: s1, body: b1}, answer{http.StatusNotFound, "principal_not_found"}); err != nil {
		return rows, fmt.Errorf("alice adds bob to her channel by id: %w", err)
	}
	if err := sameAnswer("alice adds bob to her channel by id", s1, b1, s2, b2); err != nil {
		return rows, err
	}
	rows++
	s1, b1, err = hw.alice.api.do(http.MethodPost, "/v1/channels/"+hw.p1+"/members", schema.AddChannelMemberRequestV1{Name: hw.bob.name})
	if err != nil {
		return rows, err
	}
	s2, b2, err = hw.alice.api.do(http.MethodPost, "/v1/channels/"+hw.p1+"/members", schema.AddChannelMemberRequestV1{Name: hw.ghostName})
	if err != nil {
		return rows, err
	}
	if _, err := checkAnswer(result{status: s1, body: b1}, answer{http.StatusNotFound, "principal_not_found"}); err != nil {
		return rows, fmt.Errorf("alice adds bob to her channel by name: %w", err)
	}
	if err := sameAnswer("alice adds bob to her channel by name", s1, bytes.ReplaceAll(b1, []byte(hw.bob.name), []byte(hw.ghostName)), s2, b2); err != nil {
		return rows, err
	}
	rows++
	// Carol is known through alice's link: the add works (O14b).
	if err := hw.alice.api.call(http.MethodPut, "/v1/channels/"+hw.p1+"/members/"+itoa(hw.carol.id), nil, nil); err != nil {
		return rows, fmt.Errorf("alice adds carol, who joined through her link, to her channel: %w", err)
	}
	rows++

	// The joiners of alice's link name carol; those of bob's link are a 404
	// like a link that never existed.
	var joiners schema.InviteJoinersResponseV1
	if err := hw.alice.api.call(http.MethodGet, fmt.Sprintf("/v2/invites/%d/joiners", hw.aliceLink.Invite.ID), nil, &joiners); err != nil {
		return rows, err
	}
	if len(joiners.Joiners) != 1 || joiners.Joiners[0].ID != hw.carol.id {
		return rows, fmt.Errorf("the joiners of alice's link = %+v, want carol alone", joiners.Joiners)
	}
	rows++
	s1, b1, err = hw.alice.api.do(http.MethodGet, fmt.Sprintf("/v2/invites/%d/joiners", hw.bobLink.Invite.ID), nil)
	if err != nil {
		return rows, err
	}
	s2, b2, err = hw.alice.api.do(http.MethodGet, "/v2/invites/987654321/joiners", nil)
	if err != nil {
		return rows, err
	}
	if _, err := checkAnswer(result{status: s1, body: b1}, answer{http.StatusNotFound, schema.ErrorCodeInviteNotFound}); err != nil {
		return rows, fmt.Errorf("alice reads the joiners of bob's link: %w", err)
	}
	if err := sameAnswer("alice reads the joiners of bob's link", s1, b1, s2, b2); err != nil {
		return rows, err
	}
	rows++

	// Authors: the departed author is named; bob, zed and a ghost are not.
	if err := w.checkAuthors(hw, "personal"); err != nil {
		return rows, err
	}
	rows++

	// The signal: bob joins another private channel that alice is not in
	// (the owner and bob hear of it, alice does not); then a public channel
	// alice is in, which bob joins himself (alice hears of it and her
	// directory then lists him), and her member list there names him: a
	// shared channel shows its people, documented.
	other := hw.p2 + "-second"
	if err := w.op.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: other}, nil); err != nil {
		return rows, err
	}
	if err := hw.signalStep("bob joins a private channel alice is not in",
		func() error {
			return w.op.call(http.MethodPut, "/v1/channels/"+other+"/members/"+itoa(hw.bob.id), nil, nil)
		},
		hw.bobF, hw.ownerF); err != nil {
		return rows, err
	}
	rows++
	pub := hw.p2 + "-public"
	if err := w.op.call(http.MethodPost, "/v1/channels", schema.CreateChannelRequestV1{Name: pub, Visibility: schema.ChannelPublic}, nil); err != nil {
		return rows, err
	}
	if err := hw.signalStep("alice joins a public channel",
		func() error { return hw.alice.api.call(http.MethodPost, "/v1/channels/"+pub+"/join", nil, nil) },
		hw.aliceF, hw.ownerF); err != nil {
		return rows, err
	}
	if err := hw.signalStep("bob joins the public channel alice is in",
		func() error { return hw.bob.api.call(http.MethodPost, "/v1/channels/"+pub+"/join", nil, nil) },
		hw.bobF, hw.aliceF, hw.ownerF); err != nil {
		return rows, err
	}
	rows += 2
	listed, _, _, _, err = directoryOf(hw.alice.api, 100)
	if err != nil {
		return rows, err
	}
	if _, ok := listed[hw.bob.id]; !ok {
		return rows, fmt.Errorf("%s: after bob joined the public channel alice is in, her directory still lacks him", dirRow)
	}
	if _, ok := listed[hw.zed.id]; ok {
		return rows, fmt.Errorf("%s: alice's directory lists zed, who shares nothing with her", dirRow)
	}
	var members schema.ListChannelMembersResponseV1
	if err := hw.alice.api.call(http.MethodGet, "/v1/channels/"+pub+"/members", nil, &members); err != nil {
		return rows, err
	}
	if !slices.ContainsFunc(members.Members, func(m schema.ChannelMemberV1) bool { return m.PrincipalID == hw.bob.id }) {
		return rows, fmt.Errorf("alice's member list of the public channel does not name bob, whom a shared channel shows")
	}
	rows += 2
	return rows, nil
}

// hideGroupRows runs the rows that widen once the cell is on again: the
// directory is everyone, alice's add of bob works, and the authors route is
// as it was. It answers how many rows were run.
func (w *world) hideGroupRows(hw *hideWorld) (int, error) {
	rows := 0
	listed, scope, _, _, err := directoryOf(hw.alice.api, 200)
	if err != nil {
		return rows, err
	}
	if scope != schema.DirectoryScopeEveryone {
		return rows, fmt.Errorf("with the cell on alice's directory scope is %q, want everyone", scope)
	}
	for _, m := range []*member{hw.alice, hw.bob, hw.zed, hw.carol, hw.agent} {
		if _, ok := listed[m.id]; !ok {
			return rows, fmt.Errorf("with the cell on alice's directory lacks %s", m.name)
		}
	}
	rows++
	if err := w.checkAuthors(hw, "group"); err != nil {
		return rows, err
	}
	rows++
	// R5, accepted and pinned: alice's link, valid again with the column
	// back (under personal its maker no longer holds make_member_links, so
	// it is judged invalid), tells a newcomer that bob's name is taken.
	status, body, err := w.newcomer("a newcomer redeeming alice's link").do(http.MethodPost, "/v2/invites/redeem",
		schema.RedeemInviteRequestV2{Code: hw.aliceLink.Code, Name: hw.bob.name, Password: joinPassword})
	if err != nil {
		return rows, err
	}
	if _, err := checkAnswer(result{status: status, body: body}, answer{http.StatusConflict, "principal_exists"}); err != nil {
		return rows, fmt.Errorf("R5: alice's link redeemed with bob's name: %w", err)
	}
	rows++
	// The add that was one 404 now works (O10).
	if err := hw.alice.api.call(http.MethodPut, "/v1/channels/"+hw.p1+"/members/"+itoa(hw.bob.id), nil, nil); err != nil {
		return rows, fmt.Errorf("with the cell on alice adds bob to her channel: %w", err)
	}
	rows++
	return rows, nil
}

// checkAuthors asks the authors route for dep, bob, zed and a ghost, as
// alice: the departed author is named, the rest are absent, in either
// phase.
func (w *world) checkAuthors(hw *hideWorld, phase string) error {
	path := fmt.Sprintf("/v1/channels/%s/authors?ids=%d,%d,%d,987654321", hw.p1, hw.dep.id, hw.bob.id, hw.zed.id)
	var authors schema.ChannelAuthorsResponseV1
	status, body, err := hw.alice.api.do(http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK || json.Unmarshal(body, &authors) != nil {
		return fmt.Errorf("%s: the authors of dep, bob, zed and a ghost = %d %s, want 200", phase, status, body)
	}
	if len(authors.Authors) != 1 || authors.Authors[0].PrincipalID != hw.dep.id || authors.Authors[0].Name != hw.dep.name {
		return fmt.Errorf("%s: the authors of dep, bob, zed and a ghost = %+v, want the departed author alone", phase, authors.Authors)
	}
	return nil
}
