package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/njdaniel/conch/e2e/internal/routetable"
	"github.com/njdaniel/conch/pkg/schema"
)

// The preview phase (issue #1516, docs/design/permission-preview.md §4.3, §5.1,
// §10 P3; the server is issue #1515). An administrator sends Conch-Preview-As
// naming a member and is answered as that member would be, on five reads and
// nowhere else. The phase proves on a real server:
//
//   - Matching views: a previewed read is byte for byte what the target's own
//     token gets, and holds nothing the administrator could not read as
//     themselves. The alice/bob world of §5.1 (channel A shared with alice,
//     channel B with bob, no default channel) runs with hiding off (the group
//     preset) and on (personal): bob's id and name, and B's, are markers that
//     must not be in alice's preview register, and alice's and A's not in
//     bob's, except where the directory is documented to show a person.
//   - Refusal everywhere else: every other route of the table, the sockets,
//     ws-tickets and the exempt routes (hooks, logins, invites) included,
//     answers 403 preview_read_only, the same bytes from each, before it looks
//     at the target and with nothing written; only /mcp ignores the header.
//   - The rules: owner protection, agents and guests refused as caller and
//     target, a malformed id, and the restricted operator's two permissions.
//   - Audit: every row the preview wrote names the administrator, never the
//     target.

// previewHeader is the request header that names the member to preview.
const previewHeader = "Conch-Preview-As"

// previewable are the five routes that admit the header, and exactly them
// (internal/server TestPreviewableRoutesAreReads holds the same list).
var previewable = []string{
	"GET /v1/preview",
	"GET /v1/channels",
	"GET /v1/directory/channels",
	"GET /v1/directory/members",
	"GET /v1/channels/{channel}/members",
}

// previewRefusal is what every route that is not previewable answers.
var previewRefusal = answer{http.StatusForbidden, schema.ErrorCodePreviewReadOnly}

// previewRow is one route that must refuse the header, with a request that
// reaches it.
type previewRow struct {
	route        string
	method, path string
}

// previewRefusalRows is a row for every route of the table that is neither
// previewable nor /mcp (which takes any method and ignores the header, and has
// its own row). token stands for the {token} segment, a real hook's in the
// run; every other segment is a thing that never existed.
func previewRefusalRows(table []routetable.Route, token string) []previewRow {
	var rows []previewRow
	for _, r := range table {
		if slices.Contains(previewable, r.Text) || r.Text == mcpRoute {
			continue
		}
		if r.Method == "" {
			// previewCoverage fails on a method-less route other than /mcp:
			// which method would exercise it is a decision for a person.
			continue
		}
		method, pattern := r.Method, r.Text
		if m, rest, ok := strings.Cut(r.Text, " "); ok {
			method, pattern = m, rest
		}
		path := pathParam.ReplaceAllStringFunc(pattern, func(seg string) string {
			if seg == "{token}" {
				return token
			}
			return strconv.Itoa(ghostPrincipalID)
		})
		rows = append(rows, previewRow{route: r.Text, method: method, path: path})
	}
	return rows
}

// mcpRoute is the one route that takes any method and ignores the header.
const mcpRoute = "/mcp"

// ghostPrincipalID is an id nothing in the run has: also the ghost path
// segment of the refusal rows.
const ghostPrincipalID = 987654321

// previewCoverage requires that the preview phase has a row for every route of
// the table: the five previewable ones are in the table (else the list is
// stale), every other route has exactly one refusal row whose request the
// route serves, and /mcp is the only route without one.
func previewCoverage(table []routetable.Route) error {
	inTable := map[string]bool{}
	for _, r := range table {
		inTable[r.Text] = true
	}
	var problems []string
	for _, p := range previewable {
		if !inTable[p] {
			problems = append(problems, "the previewable route "+p+" is not in the table")
		}
	}
	seen := map[string]int{}
	for _, row := range previewRefusalRows(table, "token") {
		seen[row.route]++
		route, err := routetable.Parse(row.route)
		if err != nil {
			return err
		}
		if !route.Serves(row.method, row.path) {
			problems = append(problems, fmt.Sprintf("the preview row for %s asks %s %s, which it does not serve", row.route, row.method, row.path))
		}
	}
	for _, r := range table {
		if r.Method == "" && r.Text != mcpRoute {
			problems = append(problems, fmt.Sprintf("%s takes any method and is not %s: the preview phase has no row for it", r.Text, mcpRoute))
			continue
		}
		want := 1
		if slices.Contains(previewable, r.Text) || r.Text == mcpRoute {
			want = 0
		}
		if seen[r.Text] != want {
			problems = append(problems, fmt.Sprintf("%s has %d preview refusal rows, want %d", r.Text, seen[r.Text], want))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// exemptRoute finds a route of the exempt class (no credential): the first
// argument of rt(...) with accessExempt, and a literal route's pattern.
var exemptRoute = regexp.MustCompile(`(?:\brt\(|pattern:\s*)"((?:GET|POST|PUT|DELETE|PATCH) /[^"]*)",\s*(?:access:\s*)?accessExempt`)

// exemptRoutes reads every method-bearing route of routes.go of the exempt
// class, and cross-checks the list against the route table (checkExempt).
func exemptRoutes(repo string, table []routetable.Route) ([]string, error) {
	src, err := os.ReadFile(filepath.Join(repo, "internal", "server", "routes.go")) // #nosec G304 -- the repository this run is in
	if err != nil {
		return nil, err
	}
	return checkExempt(string(src), table)
}

// checkExempt finds the exempt routes in the text of routes.go and requires
// them to agree with the table: each is a route of the table (the regexp and
// routetable read the text separately), none is previewable, and no use of
// accessExempt in the code escaped the regexp, but the method-less ones the
// table lists (/mcp).
func checkExempt(src string, table []routetable.Route) ([]string, error) {
	var out []string
	for _, m := range exemptRoute.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("routes.go has no accessExempt route this check can read")
	}
	inTable := map[string]routetable.Route{}
	methodless := 0
	for _, r := range table {
		inTable[r.Text] = r
		if r.Method == "" {
			methodless++
		}
	}
	var problems []string
	for _, e := range out {
		if _, ok := inTable[e]; !ok {
			problems = append(problems, e+" is read as exempt but is not in the route table")
		}
		if slices.Contains(previewable, e) {
			problems = append(problems, e+" is read as exempt but is previewable")
		}
	}
	uses := 0
	for _, line := range strings.Split(src, "\n") {
		if code, _, _ := strings.Cut(line, "//"); strings.Contains(code, "accessExempt") {
			uses++
		}
	}
	// A line holds one route, and /mcp is exempt and the table's only
	// method-less route.
	if uses != len(out)+methodless {
		problems = append(problems, fmt.Sprintf("routes.go uses accessExempt on %d lines, but %d exempt routes were read and %d method-less routes are in the table: a route of the exempt class was missed", uses, len(out), methodless))
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}
	return out, nil
}

// preview is the state of the phase: how many requests it made and how many
// previews it was answered (each is an audit row).
type preview struct {
	w        *world
	requests int
	viewed   int
}

// as sends a request with Conch-Preview-As: value as a, and counts it.
func (p *preview) as(a api, method, path, value string, extra map[string]string) (result, error) {
	headers := map[string]string{previewHeader: value}
	for k, v := range extra {
		headers[k] = v
	}
	p.requests++
	status, body, header, err := a.requestWith(method, path, "", nil, headers)
	if err != nil {
		return result{}, fmt.Errorf("%s %s as %s previewing %s: %w", method, path, a.who, value, err)
	}
	res := result{status: status, body: body, header: header}
	if path == "/v1/preview" && status == http.StatusOK {
		p.viewed++
	}
	return res, nil
}

// asID is as for a principal id.
func (p *preview) asID(a api, path string, id int64) (result, error) {
	return p.as(a, http.MethodGet, path, itoa(id), nil)
}

// plain makes a request with no header and counts it.
func (p *preview) plain(a api, method, path string, contentType string, body []byte) (result, error) {
	p.requests++
	status, data, header, err := a.requestWith(method, path, contentType, body, nil)
	return result{status: status, body: data, header: header}, err
}

// mustRefuse requires the answer to be status and code, an error document and
// no more.
func mustRefuse(what string, got result, want answer) (schema.Error, error) {
	e, err := checkAnswer(got, want)
	if err != nil {
		return e, fmt.Errorf("%s: %w", what, err)
	}
	return e, nil
}

// previewRegister is the markers of the other side: the person's name and id,
// the room's name and id. people is false where the directory shows people by
// design (hiding off), so that the person is not a marker there.
func previewRegister(other *member, room string, roomID int64, people bool) *markers {
	m := newMarkers()
	m.str("the room's name", room)
	m.id("channel", "the room's id", roomID)
	if people {
		m.str("the person's name", other.name)
		m.id("principal", "the person's id", other.id)
	}
	return m
}

// previewPeople is the alice/bob world: the owner's two private channels and
// the people in them, and the operator who previews them.
type previewPeople struct {
	alice, bob   *member
	roomA, roomB string
	idA, idB     int64
	ownerID      int64
	admin        *member // an operator, not an owner, in neither channel
	hookToken    string
	goneTag      string // names the member who is disabled
}

// reads are the four reads, besides the entry read, a previewed member's
// browser makes, with room's member list.
func (pp *previewPeople) reads(room string) []string {
	return []string{"/v1/channels", "/v1/directory/channels", "/v1/directory/members", "/v1/channels/" + room + "/members"}
}

// previewPhase runs the preview phase. It answers how many requests it made.
func (w *world) previewPhase(table []routetable.Route) (int, error) {
	if err := previewCoverage(table); err != nil {
		return 0, err
	}
	p := &preview{w: w}
	ta := newTag(w.tag, w.btag, w.ptag, w.qtag)
	tb := newTag(w.tag, w.btag, w.ptag, w.qtag, ta)
	tx := newTag(w.tag, w.btag, w.ptag, w.qtag, ta, tb)
	// The people's names carry tags of their own: a run of six characters of
	// a room's name must be that room's alone (markers.go).
	tAlice := newTag(w.tag, w.btag, w.ptag, w.qtag, ta, tb, tx)
	tBob := newTag(w.tag, w.btag, w.ptag, w.qtag, ta, tb, tx, tAlice)
	pp := &previewPeople{roomA: "pva" + ta, roomB: "pvb" + tb, goneTag: tx}

	var me schema.WhoAmIResponseV1
	if err := w.op.call(http.MethodGet, "/v1/whoami", nil, &me); err != nil {
		return 0, err
	}
	pp.ownerID = me.ID
	room := func(name string) (int64, error) {
		var c schema.CreateChannelResponseV1
		if err := w.op.call(http.MethodPost, "/v1/channels", schema.CreateChannelRequestV1{Name: name, Visibility: schema.ChannelPrivate}, &c); err != nil {
			return 0, err
		}
		if err := w.op.call(http.MethodPut, "/v1/channels/"+name+"/members/"+itoa(pp.ownerID), nil, nil); err != nil {
			return 0, err
		}
		return c.Channel.ID, nil
	}
	var err error
	if pp.idA, err = room(pp.roomA); err != nil {
		return 0, err
	}
	if pp.idB, err = room(pp.roomB); err != nil {
		return 0, err
	}
	if pp.alice, err = w.newPerson("alice"+tAlice+"x", pp.roomA); err != nil {
		return 0, err
	}
	if pp.bob, err = w.newPerson("bob-"+tBob+"y", pp.roomB); err != nil {
		return 0, err
	}
	// The administrator: an operator, a member of a room of its own only.
	roomX := "pvx" + tx
	if _, err := room(roomX); err != nil {
		return 0, err
	}
	if pp.admin, err = w.newPerson("pvadmin-"+tx, roomX); err != nil {
		return 0, err
	}
	if err := w.op.call(http.MethodPut, "/v1/principals/"+itoa(pp.admin.id)+"/role", schema.RoleChangeRequestV1{Role: schema.RoleOperator}, nil); err != nil {
		return 0, err
	}
	// A hook into A, posting as alice, for the exempt writer's row.
	var hook schema.CreateHookResponse
	if err := w.op.call(http.MethodPost, "/v1/hooks", schema.CreateHookRequest{Channel: pp.roomA, Principal: pp.alice.id}, &hook); err != nil {
		return 0, fmt.Errorf("a hook into A: %w", err)
	}
	w.h.secret("a preview hook token", hook.Token)
	pp.hookToken = hook.Token

	// Hiding off, then on, then the preset the run found.
	if err := w.applyPreset(schema.PresetGroup); err != nil {
		return 0, err
	}
	if err := p.unchanged(pp, func() error { return p.aliceAndBob(pp, false) }); err != nil {
		return p.requests, fmt.Errorf("hiding off (group): %w", err)
	}
	if err := w.applyPreset(schema.PresetPersonal); err != nil {
		return p.requests, err
	}
	err = p.unchanged(pp, func() error { return p.aliceAndBob(pp, true) })
	if gerr := w.applyPreset(schema.PresetGroup); gerr != nil && err == nil {
		err = gerr
	}
	if err != nil {
		return p.requests, fmt.Errorf("hiding on (personal): %w", err)
	}

	if err := p.rules(pp); err != nil {
		return p.requests, fmt.Errorf("the rules: %w", err)
	}
	if err := p.restricted(pp); err != nil {
		return p.requests, fmt.Errorf("the restricted operator: %w", err)
	}
	if err := p.refusals(pp, table); err != nil {
		return p.requests, fmt.Errorf("refusal everywhere else: %w", err)
	}
	if err := p.audit(pp); err != nil {
		return p.requests, fmt.Errorf("the audit: %w", err)
	}
	return p.requests, nil
}

// unchanged runs f and requires that the server's state (state) is as it was:
// the previewed reads write nothing.
func (p *preview) unchanged(pp *previewPeople, f func() error) error {
	before, err := p.state(pp)
	if err != nil {
		return err
	}
	if err := f(); err != nil {
		return err
	}
	after, err := p.state(pp)
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("the previewed reads changed the server's state:\nbefore:\n%s\nafter:\n%s", p.w.h.redact(before), p.w.h.redact(after))
	}
	return nil
}

// aliceAndBob is §5.1 under one setting of hiding: for each of alice and bob,
// the five previewable reads equal the person's own, hold none of the other
// side's markers, and say what the setting says.
func (p *preview) aliceAndBob(pp *previewPeople, hiding bool) error {
	w := p.w
	for _, t := range []struct {
		who, other      *member
		room, other_    string
		roomID, otherID int64
	}{
		{pp.alice, pp.bob, pp.roomA, pp.roomB, pp.idA, pp.idB},
		{pp.bob, pp.alice, pp.roomB, pp.roomA, pp.idB, pp.idA},
	} {
		what := t.who.name
		// GET /v1/preview: the target's standing, the permissions of the
		// setting, and whoami's two brakes as the target gets them.
		res, err := p.asID(pp.admin.api, "/v1/preview", t.who.id)
		if err != nil {
			return err
		}
		if res.status != http.StatusOK {
			return fmt.Errorf("%s: GET /v1/preview = %d %s", what, res.status, w.h.redact(string(res.body)))
		}
		var pv schema.PreviewResponseV1
		if err := decodeStrict(res.body, &pv); err != nil {
			return fmt.Errorf("%s: GET /v1/preview: %w", what, err)
		}
		if err := pv.Validate(); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		var own schema.WhoAmIResponseV1
		if err := t.who.api.call(http.MethodGet, "/v1/whoami", nil, &own); err != nil {
			return err
		}
		ownVoice := own.VoiceAllowed != nil && *own.VoiceAllowed
		if pv.ID != t.who.id || pv.Name != t.who.name || pv.Kind != schema.PrincipalHuman || pv.Role != own.Role || pv.Owner ||
			pv.MayInvite != own.MayInvite || pv.VoiceAllowed != ownVoice || pv.Simulation != "" {
			return fmt.Errorf("%s: GET /v1/preview = %s, but her own whoami says %+v", what, res.body, own)
		}
		// The whole permissions list and its revision are the target's: the
		// member column of the owner's document, less the two brakes.
		var doc schema.PermissionsResponseV1
		if err := p.w.op.call(http.MethodGet, "/v1/permissions", nil, &doc); err != nil {
			return err
		}
		wantPerms := []schema.Permission{}
		for _, r := range doc.Rows {
			switch {
			case !r.Member,
				r.Permission == schema.PermMakeMemberLinks && !own.MayInvite,
				r.Permission == schema.PermUseVoice && !ownVoice:
			default:
				wantPerms = append(wantPerms, r.Permission)
			}
		}
		if !slices.Equal(pv.Permissions, wantPerms) || pv.PermissionsRevision != doc.Revision {
			return fmt.Errorf("%s: GET /v1/preview permissions %v at revision %d, want %v at %d", what, pv.Permissions, pv.PermissionsRevision, wantPerms, doc.Revision)
		}
		if has := slices.Contains(pv.Permissions, schema.PermSeeMemberDirectory); has == hiding {
			return fmt.Errorf("%s: see_member_directory in the preview is %v with hiding %v: %v", what, has, hiding, pv.Permissions)
		}
		if leaks := previewRegister(t.other, t.other_, t.otherID, true).leaks(res.body, asked{}); len(leaks) > 0 {
			return fmt.Errorf("%s: GET /v1/preview holds %s", what, strings.Join(leaks, "; "))
		}

		// The four reads: the target's own bytes, none of the other side's.
		for _, path := range pp.reads(t.room) {
			prev, err := p.asID(pp.admin.api, path, t.who.id)
			if err != nil {
				return err
			}
			ownRes, err := p.plain(t.who.api, http.MethodGet, path, "", nil)
			if err != nil {
				return err
			}
			if prev.status != http.StatusOK || prev.status != ownRes.status || !bytes.Equal(prev.body, ownRes.body) {
				return fmt.Errorf("%s: previewed %s = %d %s, but her own token gets %d %s", what, path, prev.status, w.h.redact(string(prev.body)), ownRes.status, w.h.redact(string(ownRes.body)))
			}
			// Under hiding off the directory lists everyone, by design;
			// that response alone may hold the other person.
			people := hiding || path != "/v1/directory/members"
			if leaks := previewRegister(t.other, t.other_, t.otherID, people).leaks(prev.body, asked{}); len(leaks) > 0 {
				return fmt.Errorf("%s: previewed %s holds %s", what, path, strings.Join(leaks, "; "))
			}
			if err := p.within(pp, path, prev.body); err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
			switch path {
			case "/v1/channels":
				var l schema.ListChannelsResponse
				if err := decodeStrict(prev.body, &l); err != nil {
					return err
				}
				if len(l.Channels) != 1 || l.Channels[0].Name != t.room {
					return fmt.Errorf("%s: previewed channels = %s, want %s alone", what, prev.body, t.room)
				}
			case "/v1/directory/members":
				if err := p.directoryIs(pp, t.who, t.other, prev.body, hiding); err != nil {
					return fmt.Errorf("%s: %w", what, err)
				}
			}
		}

		// The query variants: the header and a query string together. Her
		// own bytes under the same query, hers alone.
		for _, v := range []struct {
			path string
			want int // her status
		}{
			{"/v1/directory/channels?scope=all", http.StatusForbidden},
			{"/v1/directory/members?limit=200", http.StatusOK},
			{"/v1/directory/members?limit=1", http.StatusOK},
			// The cursor is "after" (the handler reads only after and
			// limit): pages that start at the owner, at her, and at the
			// other side, whose id is the boundary a preview that used the
			// administrator's view would expose.
			{"/v1/directory/members?after=" + itoa(pp.ownerID), http.StatusOK},
			{"/v1/directory/members?limit=1&after=" + itoa(pp.ownerID), http.StatusOK},
			{"/v1/directory/members?after=" + itoa(t.who.id), http.StatusOK},
			{"/v1/directory/members?limit=1&after=" + itoa(t.otherID-1), http.StatusOK},
			{"/v1/directory/members?limit=1&after=" + itoa(t.otherID), http.StatusOK},
		} {
			prev, err := p.asID(pp.admin.api, v.path, t.who.id)
			if err != nil {
				return err
			}
			ownRes, err := p.plain(t.who.api, http.MethodGet, v.path, "", nil)
			if err != nil {
				return err
			}
			if prev.status != v.want || prev.status != ownRes.status || !bytes.Equal(prev.body, ownRes.body) {
				return fmt.Errorf("%s: previewed %s = %d %s, but her own token gets %d %s (want %d)", what, v.path, prev.status, w.h.redact(string(prev.body)), ownRes.status, w.h.redact(string(ownRes.body)), v.want)
			}
			if v.want == http.StatusForbidden {
				if _, err := mustRefuse(what+": previewed "+v.path, prev, answer{http.StatusForbidden, "forbidden"}); err != nil {
					return err
				}
			}
			people := hiding || !strings.HasPrefix(v.path, "/v1/directory/members")
			if leaks := previewRegister(t.other, t.other_, t.otherID, people).leaks(prev.body, asked{}); len(leaks) > 0 {
				return fmt.Errorf("%s: previewed %s holds %s", what, v.path, strings.Join(leaks, "; "))
			}
		}

		// The other side's room: a 404, the same bytes as her own request
		// and as a room nobody made.
		path := "/v1/channels/" + t.other_ + "/members"
		prev, err := p.asID(pp.admin.api, path, t.who.id)
		if err != nil {
			return err
		}
		ownRes, err := p.plain(t.who.api, http.MethodGet, path, "", nil)
		if err != nil {
			return err
		}
		ghostPath := "/v1/channels/ghost-" + t.other_ + "/members"
		ghost, err := p.asID(pp.admin.api, ghostPath, t.who.id)
		if err != nil {
			return err
		}
		if _, err := mustRefuse(what+": the other room's members, previewed", prev, channelNotFound); err != nil {
			return err
		}
		if prev.status != ownRes.status || !bytes.Equal(prev.body, ownRes.body) {
			return fmt.Errorf("%s: the other room's members previewed = %d %s, but her own token gets %d %s", what, prev.status, prev.body, ownRes.status, ownRes.body)
		}
		if err := sameAnswer(what+": the other room's members, previewed", prev.status, normalizeName(prev.body, t.other_), ghost.status, normalizeName(ghost.body, "ghost-"+t.other_)); err != nil {
			return err
		}
	}
	return nil
}

// normalizeName sets aside a name a refusal may quote.
func normalizeName(body []byte, name string) []byte {
	return bytes.ReplaceAll(body, []byte(name), []byte("<asked>"))
}

// decodeStrict decodes JSON that must be exactly the schema's shape.
func decodeStrict(body []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("not exactly the schema's document (%w): %s", err, body)
	}
	return nil
}

// directoryIs checks the directory page of who: hiding on lists the owner and
// who alone (scope shared_channels); hiding off lists everyone, other included
// (scope everyone).
func (p *preview) directoryIs(pp *previewPeople, who, other *member, body []byte, hiding bool) error {
	var page schema.ServerMembersResponseV1
	if err := decodeStrict(body, &page); err != nil {
		return err
	}
	listed := map[int64]bool{}
	for _, m := range page.Members {
		listed[m.ID] = true
	}
	if hiding {
		if page.Scope != schema.DirectoryScopeSharedChannels || len(listed) != 2 || !listed[pp.ownerID] || !listed[who.id] {
			return fmt.Errorf("the directory with hiding on = %s, want {owner, %s} with scope %s", body, who.name, schema.DirectoryScopeSharedChannels)
		}
		return nil
	}
	if page.Scope != schema.DirectoryScopeEveryone || !listed[pp.ownerID] || !listed[who.id] || !listed[other.id] {
		return fmt.Errorf("the directory with hiding off = %s, want everyone, %s included", body, other.name)
	}
	return nil
}

// within requires that a previewed answer holds nothing the administrator
// could not read as themselves: every channel of the previewed lists is in the
// administrator's own ?scope=all directory, every person in its own.
func (p *preview) within(pp *previewPeople, path string, body []byte) error {
	switch path {
	case "/v1/channels", "/v1/directory/channels":
		own, err := p.plain(pp.admin.api, http.MethodGet, "/v1/directory/channels?scope=all", "", nil)
		if err != nil {
			return err
		}
		if own.status != http.StatusOK {
			return fmt.Errorf("the administrator's own directory of every channel = %d %s", own.status, own.body)
		}
		var all schema.ChannelDirectoryResponseV1
		if err := decodeStrict(own.body, &all); err != nil {
			return err
		}
		known := map[int64]bool{}
		for _, c := range all.Channels {
			known[c.ID] = true
		}
		var ids []int64
		if path == "/v1/channels" {
			var l schema.ListChannelsResponse
			if err := decodeStrict(body, &l); err != nil {
				return err
			}
			for _, c := range l.Channels {
				ids = append(ids, c.ID)
			}
		} else {
			var d schema.ChannelDirectoryResponseV1
			if err := decodeStrict(body, &d); err != nil {
				return err
			}
			for _, c := range d.Channels {
				ids = append(ids, c.ID)
			}
		}
		for _, id := range ids {
			if !known[id] {
				return fmt.Errorf("previewed %s names channel %d, which the administrator cannot list as themselves", path, id)
			}
		}
	case "/v1/directory/members":
		own, err := p.plain(pp.admin.api, http.MethodGet, "/v1/directory/members", "", nil)
		if err != nil {
			return err
		}
		if own.status != http.StatusOK {
			return fmt.Errorf("the administrator's own directory = %d %s", own.status, own.body)
		}
		var mine, theirs schema.ServerMembersResponseV1
		if err := decodeStrict(own.body, &mine); err != nil {
			return err
		}
		if err := decodeStrict(body, &theirs); err != nil {
			return err
		}
		known := map[int64]bool{}
		for _, m := range mine.Members {
			known[m.ID] = true
		}
		for _, m := range theirs.Members {
			if !known[m.ID] {
				return fmt.Errorf("the previewed directory lists %s (%d), whom the administrator cannot list as themselves", m.Name, m.ID)
			}
		}
	}
	return nil
}

// rules are owner protection, agents and guests, malformed ids and unknown
// targets.
func (p *preview) rules(pp *previewPeople) error {
	w := p.w
	// Owner protection: a non-owner operator naming the owner is told so,
	// honestly, not 404; an id nobody has is a 404, and an owner is a 403 on
	// each previewable read, not only on the entry read.
	for _, path := range []string{"/v1/preview", "/v1/channels", "/v1/directory/channels", "/v1/directory/members", "/v1/channels/" + pp.roomA + "/members"} {
		res, err := p.asID(pp.admin.api, path, pp.ownerID)
		if err != nil {
			return err
		}
		e, err := mustRefuse("an operator previews the owner: "+path, res, answer{http.StatusForbidden, "forbidden"})
		if err != nil {
			return err
		}
		if e.Message != "only an owner may act on an owner" || e.Permission != "" {
			return fmt.Errorf("an operator previews the owner: %s = %s, want the owner-protection text", path, res.body)
		}
	}
	ghost, err := p.asID(pp.admin.api, "/v1/preview", ghostPrincipalID)
	if err != nil {
		return err
	}
	if _, err := mustRefuse("an operator previews an id nobody has", ghost, answer{http.StatusNotFound, "principal_not_found"}); err != nil {
		return err
	}
	// The owner previewing themselves, and the owner previewing a member.
	if res, err := p.asID(w.op, "/v1/preview", pp.ownerID); err != nil {
		return err
	} else if _, err := mustRefuse("the owner previews themselves", res, answer{http.StatusBadRequest, "invalid_request"}); err != nil {
		return err
	}
	byOwner, err := p.asID(w.op, "/v1/channels", pp.alice.id)
	if err != nil {
		return err
	}
	ownAlice, err := p.plain(pp.alice.api, http.MethodGet, "/v1/channels", "", nil)
	if err != nil {
		return err
	}
	if byOwner.status != http.StatusOK || !bytes.Equal(byOwner.body, ownAlice.body) {
		return fmt.Errorf("the owner previews alice's channels = %d %s, want her own %s", byOwner.status, byOwner.body, ownAlice.body)
	}

	// Agents and guests, as target: not members of the community, so the
	// answer is the one an id nobody has gets, byte for byte.
	// The guest is a visit started beside the server, as the guests' phase
	// does (no route starts one), still live.
	guest, err := w.seedVisit(w.newcomer("a guest who is previewed"), "Preview", nil)
	if err != nil {
		return err
	}
	for _, t := range []*member{w.b.agent, guest} {
		res, err := p.asID(pp.admin.api, "/v1/preview", t.id)
		if err != nil {
			return err
		}
		if err := sameAnswer("an operator previews "+t.name, res.status, res.body, ghost.status, ghost.body); err != nil {
			return err
		}
	}
	// Agents and guests, as caller.
	res, err := p.asID(w.b.agent.api, "/v1/preview", pp.alice.id)
	if err != nil {
		return err
	}
	if _, err := mustRefuse("an agent previews alice", res, answer{http.StatusForbidden, "forbidden"}); err != nil {
		return err
	}
	res, err = p.asID(guest.api, "/v1/preview", pp.alice.id)
	if err != nil {
		return err
	}
	if _, err := mustRefuse("a guest previews alice", res, answer{http.StatusForbidden, schema.ErrorCodeGuestNotAllowed}); err != nil {
		return err
	}
	// A plain member naming anyone: the operator role, not the
	// target, decides first.
	for _, id := range []int64{pp.bob.id, ghostPrincipalID} {
		res, err := p.asID(pp.alice.api, "/v1/preview", id)
		if err != nil {
			return err
		}
		if _, err := mustRefuse("a member previews "+itoa(id), res, operatorOnly); err != nil {
			return err
		}

	}
	// A malformed id is a 400 on a previewable route, whoever the caller.
	for _, bad := range []string{"abc", "0", "-3", "1.5", "1e3", "+7", "7 8"} {
		res, err := p.as(pp.admin.api, http.MethodGet, "/v1/preview", bad, nil)
		if err != nil {
			return err
		}
		if _, err := mustRefuse("a malformed Conch-Preview-As "+strconv.Quote(bad), res, answer{http.StatusBadRequest, "invalid_request"}); err != nil {
			return err
		}
	}
	// The header sent twice is not one id.
	p.requests++
	req, err := http.NewRequest(http.MethodGet, w.d.baseURL+"/v1/preview", nil) // #nosec G107 -- this run's own conchd
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+pp.admin.token)
	req.Header.Add(previewHeader, itoa(pp.alice.id))
	req.Header.Add(previewHeader, itoa(pp.bob.id))
	w.h.called(http.MethodGet, "/v1/preview")
	dup, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = dup.Body.Close() }()
	dupBody, err := io.ReadAll(io.LimitReader(dup.Body, 1<<20))
	if err != nil {
		return err
	}
	if _, err := mustRefuse("the header sent twice, for alice and for bob", result{status: dup.StatusCode, body: dupBody, header: dup.Header}, answer{http.StatusBadRequest, "invalid_request"}); err != nil {
		return err
	}
	return p.departed(pp, ghost)
}

// departed: a target who was disabled is no member of the community, and is
// answered as an id nobody has, byte for byte, on each of the five reads.
func (p *preview) departed(pp *previewPeople, ghost result) error {
	gone, err := p.w.newPerson("pvgone-"+pp.goneTag, pp.roomA)
	if err != nil {
		return err
	}
	if err := p.w.op.call(http.MethodPost, fmt.Sprintf("/v1/principals/%d/disable", gone.id), nil, nil); err != nil {
		return fmt.Errorf("disable %s: %w", gone.name, err)
	}
	for _, path := range append([]string{"/v1/preview"}, pp.reads(pp.roomA)...) {
		res, err := p.asID(pp.admin.api, path, gone.id)
		if err != nil {
			return err
		}
		what := "an operator previews a disabled member: " + path
		if _, err := mustRefuse(what, res, answer{http.StatusNotFound, "principal_not_found"}); err != nil {
			return err
		}
		if err := sameAnswer(what, res.status, res.body, ghost.status, ghost.body); err != nil {
			return err
		}
	}
	return nil
}

// restricted cuts the operator column's two permissions in turn: the entry
// read and each previewable read name the missing one (admin-roles §7.7), and
// a route that is not previewable still says so.
func (p *preview) restricted(pp *previewPeople) (err error) {
	w := p.w
	for _, cut := range [][]schema.Permission{
		{schema.PermManageChannels},
		// act_as_members needs manage_members (the permissions document
		// refuses the cut alone), so the two go together.
		{schema.PermManageMembers, schema.PermActAsMembers},
	} {
		perm := cut[0]
		if err := w.setOperatorCells(false, cut...); err != nil {
			return fmt.Errorf("the owner cuts %s: %w", perm, err)
		}
		cerr := func() error {
			for _, path := range []string{"/v1/preview", "/v1/channels", "/v1/directory/channels", "/v1/directory/members", "/v1/channels/" + pp.roomA + "/members"} {
				res, err := p.asID(pp.admin.api, path, pp.alice.id)
				if err != nil {
					return err
				}
				if err := refusedNaming("a restricted operator previews alice: "+path+" without "+string(perm), res.status, res.body, nil, schema.ErrorCodePermissionDenied, perm); err != nil {
					return err
				}
			}
			// The allowlist runs first: a route that is not previewable is
			// preview_read_only, whatever the operator holds.
			res, err := p.asID(pp.admin.api, "/v1/whoami", pp.alice.id)
			if err != nil {
				return err
			}
			_, err = mustRefuse("a restricted operator sends the header to whoami", res, previewRefusal)
			return err
		}()
		if rerr := w.setOperatorCells(true, cut...); rerr != nil && cerr == nil {
			cerr = rerr
		}
		if cerr != nil {
			return cerr
		}
	}
	// Given back, the operator previews again.
	res, err := p.asID(pp.admin.api, "/v1/preview", pp.alice.id)
	if err != nil {
		return err
	}
	if res.status != http.StatusOK {
		return fmt.Errorf("with both permissions given back the operator's preview = %d %s", res.status, res.body)
	}
	return nil
}

// state is what a refused sweep, or the five previewed reads, must leave as
// they found it: the owner's view of the channels, the people (the principals,
// the directory, the members of A and of B), the permissions, the invites, the
// sessions, the guests, the join settings, the hooks and the messages of A and
// B. The sockets' tickets are not a state.
func (p *preview) state(pp *previewPeople) (string, error) {
	var b strings.Builder
	for _, path := range []string{"/v1/channels", "/v1/directory/members?limit=200", "/v1/directory/channels?scope=all", "/v1/permissions", "/v1/hooks",
		"/v1/principals?limit=200", "/v1/invites", "/v1/sessions?channel=" + pp.roomA, "/v1/sessions?channel=" + pp.roomB, "/v1/guests", "/v1/join/settings",
		"/v2/channels/" + pp.roomA + "/messages", "/v2/channels/" + pp.roomB + "/messages",
		"/v1/channels/" + pp.roomA + "/members", "/v1/channels/" + pp.roomB + "/members"} {
		res, err := p.plain(p.w.op, http.MethodGet, path, "", nil)
		if err != nil {
			return "", err
		}
		if res.status != http.StatusOK {
			return "", fmt.Errorf("the state: GET %s = %d %s", path, res.status, res.body)
		}
		fmt.Fprintf(&b, "%s\n%s\n", path, res.body)
	}
	return b.String(), nil
}

// refusals sends the header to every other route, as an operator, as the
// owner, anonymously where the route takes no credential, and malformed; each
// answer is the same 403, byte for byte, and nothing is written.
func (p *preview) refusals(pp *previewPeople, table []routetable.Route) error {
	w := p.w
	before, err := p.state(pp)
	if err != nil {
		return err
	}
	rows := previewRefusalRows(table, pp.hookToken)
	if err := previewCoverage(table); err != nil {
		return err
	}
	exempt, err := exemptRoutes(w.h.repo, table)
	if err != nil {
		return err
	}
	var want []byte
	check := func(what string, res result, err error) error {
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		if _, err := mustRefuse(what, res, previewRefusal); err != nil {
			return err
		}
		if want == nil {
			want = res.body
		} else if !bytes.Equal(res.body, want) {
			return fmt.Errorf("%s: %s, but another route answered %s: the refusal differs between routes", what, res.body, want)
		}
		if leaks := w.leaks(res.body, asked{}); len(leaks) > 0 {
			return fmt.Errorf("%s: holds %s", what, strings.Join(leaks, "; "))
		}
		return nil
	}
	upgrade := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}
	for _, r := range rows {
		extra := map[string]string(nil)
		if strings.HasSuffix(r.path, "/ws") {
			extra = upgrade
		}
		for _, who := range []api{pp.admin.api, w.op} {
			res, err := p.as(who, r.method, r.path, itoa(pp.alice.id), extra)
			if err := check(r.route+" as "+who.who, res, err); err != nil {
				return err
			}
		}
		// A malformed id is no matter: the allowlist runs before it is read.
		res, err := p.as(pp.admin.api, r.method, r.path, "not-an-id", extra)
		if err := check(r.route+" with a malformed header", res, err); err != nil {
			return err
		}
	}
	// The routes that take no credential, with none, and with a bearer.
	anon := api{h: w.h, base: w.d.baseURL, who: "nobody"}
	for _, e := range exempt {
		if e == "POST /v1/hooks/{token}" || e == "/mcp" {
			continue
		}
		for _, r := range rows {
			if r.route != e {
				continue
			}
			res, err := p.as(anon, r.method, r.path, itoa(pp.alice.id), nil)
			if err := check(r.route+" with no credential", res, err); err != nil {
				return err
			}
		}
	}
	// A ticket, the socket it opens and the ticket route: no socket runs with
	// a swapped principal, and a ticket is no way round the allowlist.
	ticket, err := pp.admin.api.ticket()
	if err != nil {
		return err
	}
	for _, path := range []string{"/v2/ws?channel=" + url.QueryEscape(pp.roomA) + "&ticket=" + url.QueryEscape(ticket)} {
		res, err := p.as(anon, http.MethodGet, path, itoa(pp.alice.id), upgrade)
		if err := check("GET /v2/ws with a ticket", res, err); err != nil {
			return err
		}
	}
	// The hook: with the header nothing is posted; without, the same token
	// posts, so the refusal was the header's.
	hookPath := "/v1/hooks/" + pp.hookToken
	res, err := p.as(anon, http.MethodPost, hookPath, itoa(pp.alice.id), map[string]string{"Content-Type": "text/plain"})
	if err := check("POST /v1/hooks/{token} with its real token", res, err); err != nil {
		return err
	}
	after, err := p.state(pp)
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("the refused requests changed the server's state:\nbefore:\n%s\nafter:\n%s", w.h.redact(before), w.h.redact(after))
	}
	posted, err := p.plain(anon, http.MethodPost, hookPath, "text/plain", []byte("a hook post"))
	if err != nil {
		return err
	}
	if posted.status < 200 || posted.status > 299 {
		return fmt.Errorf("the hook's token without the header = %d %s, want it to post: the refusal above would prove nothing", posted.status, posted.body)
	}

	// /mcp ignores the header: an agent's answer is the one it gets without.
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"isolation-check","version":"0"}}}`)
	p.requests += 2
	accept := map[string]string{"Accept": "application/json, text/event-stream"}
	s1, b1, _, err := w.b.agent.api.requestWith(http.MethodPost, "/mcp", "application/json", initialize, accept)
	if err != nil {
		return err
	}
	withHeader := map[string]string{"Accept": accept["Accept"], previewHeader: itoa(pp.alice.id)}
	s2, b2, _, err := w.b.agent.api.requestWith(http.MethodPost, "/mcp", "application/json", initialize, withHeader)
	if err != nil {
		return err
	}
	if s1 != http.StatusOK || !bytes.Contains(b1, []byte(`"result"`)) {
		return fmt.Errorf("/mcp initialize without the header = %d %s, want a success: the comparison would prove nothing", s1, w.h.redact(string(b1)))
	}
	if s1 != s2 || !bytes.Equal(b1, b2) {
		return fmt.Errorf("/mcp with the header = %d %s, without = %d %s: it must ignore the header", s2, w.h.redact(string(b2)), s1, w.h.redact(string(b1)))
	}
	return nil
}

// previewAs finds the target a denial's detail names.
var previewAs = regexp.MustCompile(`preview_as=(\d+)`)

// audit requires that every row the previews wrote names the administrator:
// each preview_viewed row has an actor that is not its subject, and each
// access_denied row that names a preview target has an actor that is not it.
func (p *preview) audit(pp *previewPeople) error {
	w := p.w
	viewed, err := w.auditRows("preview_viewed")
	if err != nil {
		return err
	}
	if len(viewed) != p.viewed {
		return fmt.Errorf("%d preview_viewed rows, but %d previews were answered", len(viewed), p.viewed)
	}
	admins := map[string]bool{"principal:" + itoa(pp.admin.id): true, "principal:" + itoa(pp.ownerID): true}
	for _, e := range viewed {
		if !admins[e.Actor] || e.Actor == e.Subject || (e.Subject != "principal:"+itoa(pp.alice.id) && e.Subject != "principal:"+itoa(pp.bob.id)) {
			return fmt.Errorf("a preview_viewed row has actor %q and subject %q: the actor must be an administrator and the subject the target", e.Actor, e.Subject)
		}
	}
	denied, err := w.auditRows("access_denied")
	if err != nil {
		return err
	}
	n := 0
	exempt, err := exemptDenials(denied, []int64{pp.alice.id, pp.bob.id})
	if err != nil {
		return err
	}
	for _, e := range denied {
		if isExemptDenial(e) {
			continue
		}
		m := previewAs.FindStringSubmatch(e.Detail)
		if m == nil {
			continue
		}
		n++
		if e.Actor == "principal:"+m[1] {
			return fmt.Errorf("an access_denied row names the target %s as its actor: %+v", m[1], e)
		}
		if strings.Contains(e.Detail, "alice") || strings.Contains(e.Detail, "bob-") {
			return fmt.Errorf("an access_denied row names a target: %+v", e)
		}
	}
	if n == 0 {
		return fmt.Errorf("no access_denied row records a refused preview: the denials are not audited")
	}
	if exempt == 0 {
		return fmt.Errorf("no access_denied row with actor anonymous and detail %s records a refused preview of an exempt route", schema.ErrorCodePreviewReadOnly)
	}
	return nil
}

// isExemptDenial: the row is a refusal of an exempt route under a preview
// header, picked by its detail and its subject (the route pattern, which
// begins with a method), never by its actor, so a wrongly attributed row is
// still selected.
func isExemptDenial(e schema.AuditEventV1) bool {
	return strings.HasPrefix(e.Detail, schema.ErrorCodePreviewReadOnly) && !strings.Contains(e.Detail, "preview_as=") && exemptSubject.MatchString(e.Subject)
}

// exemptSubject is a route pattern: a method, a space and a path.
var exemptSubject = regexp.MustCompile(`^(GET|POST|PUT|DELETE|PATCH) /`)

// exemptDenials counts the exempt-route denials among rows and requires every
// one to have the actor "anonymous" and to name none of the targets.
func exemptDenials(rows []schema.AuditEventV1, targets []int64) (int, error) {
	n := 0
	for _, e := range rows {
		if !isExemptDenial(e) {
			continue
		}
		n++
		if e.Actor != "anonymous" {
			return n, fmt.Errorf("an exempt route's preview_read_only denial has actor %q, want anonymous: %+v", e.Actor, e)
		}
		for _, id := range targets {
			if regexp.MustCompile(`\b` + itoa(id) + `\b`).MatchString(e.Actor + " " + e.Subject + " " + e.Detail) {
				return n, fmt.Errorf("an exempt route's preview_read_only denial names the target %d: %+v", id, e)
			}
		}
	}
	return n, nil
}
