package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Archiving a channel (issue #844): the settings field, every refused route,
// every allowed one, and what the refusals leak. The store holds the guards
// (store/archive_test.go); these tests ask them over the wire.

const archiveHookToken = "archive-hook-token"

// archiveFixture is the file fixture (a real listener, uploads on, ann,
// wes, nora, an agent and a net in "ops") with ops made public and one live
// message, reaction, file and webhook to act on, and then archived through
// the settings route as the owner.
type archiveFixture struct {
	*fileFixture
	msg   int64 // ann's message, editable
	react string
}

const archiveEmoji = "\U0001F44D"

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	ctx := context.Background()
	f := &archiveFixture{fileFixture: newFileFixture(t), react: archiveEmoji}
	public := schema.ChannelPublic
	if _, err := f.srv.store.ChannelSettings(ctx, "system", 0, f.ops.ID, store.ChannelSettingsChange{Visibility: &public}, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.msg = f.mustPost(t, "ann", `{"body":"before the archive"}`)
	if res := f.callREST(t, http.MethodPut, rpath("ops", f.msg, archiveEmoji), f.p("wes").token, ""); res.status != http.StatusOK {
		t.Fatalf("a reaction before the archive = %d %s", res.status, res.body)
	}
	if _, err := f.srv.store.CreateHook(ctx, "system", archiveHookToken, f.ops.ID, f.p("ann").ID); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *archiveFixture) settings(t *testing.T, token string, id int64, body string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", id), token, body)
}

func (f *archiveFixture) archive(t *testing.T) {
	t.Helper()
	res := f.settings(t, f.rootTok, f.ops.ID, `{"archived":true}`)
	if res.status != http.StatusOK {
		t.Fatalf("archive = %d %s", res.status, res.body)
	}
	if got := decodeWire[schema.ChannelSettingsResponseV1](t, res.body).Channel; got.ArchivedAt == nil {
		t.Fatalf("the archive's answer has no archived_at: %s", res.body)
	}
}

func (f *archiveFixture) restore(t *testing.T) {
	t.Helper()
	res := f.settings(t, f.rootTok, f.ops.ID, `{"archived":false}`)
	if res.status != http.StatusOK {
		t.Fatalf("restore = %d %s", res.status, res.body)
	}
	if got := decodeWire[schema.ChannelSettingsResponseV1](t, res.body).Channel; got.ArchivedAt != nil {
		t.Fatalf("the restore's answer still has archived_at: %s", res.body)
	}
}

// archiveRoute is one request, as who, with the answer it must get.
type archiveRoute struct {
	name, who, method, path, ctype, body string
	status                               int
	code                                 string
}

// routes are the refused routes of decision 5, as the people the fixture
// has; ids are those of the fixture.
func (f *archiveFixture) refusedRoutes() []archiveRoute {
	const conflict = http.StatusConflict
	const code = schema.ErrorCodeChannelArchived
	js := "application/json"
	post := func(name, path, body string) archiveRoute {
		return archiveRoute{name: name, who: "ann", method: "POST", path: path, ctype: js, body: body, status: conflict, code: code}
	}
	return []archiveRoute{
		post("post v0", "/v0/channels/ops/messages", `{"body":"x"}`),
		post("post v1", "/v1/channels/ops/messages", `{"body":"x"}`),
		post("post v2", "/v2/channels/ops/messages", `{"body":"x"}`),
		post("post to a net", "/v2/channels/ops/messages", `{"body":"x",`+netAudience(f.net1.ID)+`}`),
		post("whisper", "/v2/channels/ops/messages", `{"body":"x",`+whisperAudience(f.p("ann").ID, f.p("wes").ID)+`}`),
		post("reply", "/v2/channels/ops/messages", fmt.Sprintf(`{"body":"x","thread_root_id":%d}`, f.msg)),
		{"webhook", "", "POST", "/v1/hooks/" + archiveHookToken, js, `{"body":"x"}`, conflict, code},
		{"edit", "ann", "PUT", fmt.Sprintf("/v2/channels/ops/messages/%d/body", f.msg), js, `{"body":"edited"}`, conflict, code},
		{"edit that changes nothing", "ann", "PUT", fmt.Sprintf("/v2/channels/ops/messages/%d/body", f.msg), js, `{"body":"before the archive"}`, conflict, code},
		{"set reaction", "ann", "PUT", rpath("ops", f.msg, archiveEmoji), "", "", conflict, code},
		{"remove reaction", "wes", "DELETE", rpath("ops", f.msg, archiveEmoji), "", "", conflict, code},
		{"moderate reaction", "root", "DELETE", rpath("ops", f.msg, archiveEmoji) + fmt.Sprintf("/%d", f.p("wes").ID), "", "", conflict, code},
		{"upload", "ann", "POST", uploadPath("ops", "late.txt"), "application/octet-stream", "late", conflict, code},
		{"self-join", "nora", "POST", "/v1/channels/ops/join", "", "", conflict, code},
		{"add a member by name", "root", "POST", "/v1/channels/ops/members", js, `{"name":"nora"}`, conflict, code},
		{"add a member by id", "root", "PUT", fmt.Sprintf("/v1/channels/ops/members/%d", f.p("nora").ID), "", "", conflict, code},
		{"create a member link landing in it", "root", "POST", "/v2/invites", js, `{"kind":"member","landing_channels":["ops"]}`, conflict, code},
	}
}

// allowedRoutes are what an archived channel still does (decision 6).
func (f *archiveFixture) allowedRoutes() []archiveRoute {
	js := "application/json"
	return []archiveRoute{
		{"read history v1", "ann", "GET", "/v1/channels/ops/messages", "", "", http.StatusOK, ""},
		{"read history v2", "ann", "GET", "/v2/channels/ops/messages", "", "", http.StatusOK, ""},
		{"search", "ann", "GET", "/v2/channels/ops/messages/search?q=archive", "", "", http.StatusOK, ""},
		{"read the members", "ann", "GET", "/v1/channels/ops/members", "", "", http.StatusOK, ""},
		{"read reactions", "ann", "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/reactions", f.msg), "", "", http.StatusOK, ""},
		{"read a revision list", "ann", "GET", fmt.Sprintf("/v2/channels/ops/messages/%d/revisions", f.msg), "", "", http.StatusOK, ""},
		{"mark it read", "ann", "PUT", "/v1/channels/ops/read-state", js, fmt.Sprintf(`{"last_read_id":%d}`, f.msg), http.StatusOK, ""},
		{"list my channels", "ann", "GET", "/v1/channels", "", "", http.StatusOK, ""},
		{"list the directory", "ann", "GET", "/v1/directory/channels", "", "", http.StatusOK, ""},
		{"remove a member", "root", "DELETE", fmt.Sprintf("/v1/channels/ops/members/%d", f.p("dina").ID), "", "", http.StatusNoContent, ""},
		{"leave", "olga", "POST", "/v1/channels/ops/leave", "", "", http.StatusNoContent, ""},
	}
}

func (f *archiveFixture) run(t *testing.T, r archiveRoute) fileResp {
	t.Helper()
	var body *strings.Reader
	if r.body != "" {
		body = strings.NewReader(r.body)
	}
	if body == nil {
		return f.do(t, r.method, r.path, r.who, r.ctype, nil)
	}
	return f.do(t, r.method, r.path, r.who, r.ctype, body)
}

// state is every row an archived-channel write could add, as one string.
func (f *archiveFixture) state(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	msgs, err := f.srv.store.CountMessages(ctx, f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	reactions, err := f.srv.store.CountReactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.srv.store.CountFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	members, err := f.srv.store.ListChannelMembers(ctx, f.ops.ID)
	if err != nil {
		t.Fatal(err)
	}
	invites, err := f.srv.store.ListInvites(ctx, store.InviteFilter{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("messages=%d reactions=%d files=%d members=%d invites=%d audit=%d", msgs, reactions, files, len(members), len(invites), len(f.audit(t)))
}

// TestArchivedChannelRefusesEveryWrite: every write of decision 5 answers 409
// channel_archived with nothing stored and nothing audited, and the same
// request works once the channel is restored.
func TestArchivedChannelRefusesEveryWrite(t *testing.T) {
	f := newArchiveFixture(t)
	routes := f.refusedRoutes()
	f.archive(t)
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			before := f.state(t)
			res := f.run(t, r)
			if res.status != r.status || !strings.Contains(string(res.body), `"code":"`+r.code+`"`) {
				t.Fatalf("%s %s = %s, want %d %s", r.method, r.path, res, r.status, r.code)
			}
			if after := f.state(t); after != before {
				t.Errorf("a refused request wrote rows: %s -> %s", before, after)
			}
		})
	}
	// The MCP posts and reactions refuse the same way (no new tool).
	for name, call := range map[string]struct {
		tool string
		args map[string]any
	}{
		"mcp post_message":          {"post_message", map[string]any{"channel": "ops", "body": "x"}},
		"mcp post_message to a net": {"post_message", map[string]any{"channel": "ops", "body": "x", "audience": map[string]any{"kind": "net", "net_id": f.net1.ID}}},
		"mcp post_message whisper":  {"post_message", map[string]any{"channel": "ops", "body": "x", "audience": map[string]any{"kind": "principals", "principal_ids": []int64{f.p("ann").ID}}}},
		"mcp post_message reply":    {"post_message", map[string]any{"channel": "ops", "body": "x", "thread_root_id": f.msg}},
		"mcp set_reaction":          {"set_reaction", map[string]any{"channel": "ops", "message_id": f.msg, "emoji": archiveEmoji}},
		"mcp remove_reaction":       {"remove_reaction", map[string]any{"channel": "ops", "message_id": f.msg, "emoji": archiveEmoji}},
	} {
		t.Run(name, func(t *testing.T) {
			f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
			before := f.state(t)
			out := f.mcpCall(t, f.p("aria").token, call.tool, call.args)
			if out.result() != schema.ErrorCodeChannelArchived {
				t.Errorf("%s = %s (%s), want the tool error %s", call.tool, out.result(), out.body, schema.ErrorCodeChannelArchived)
			}
			if after := f.state(t); after != before {
				t.Errorf("a refused tool call wrote rows: %s -> %s", before, after)
			}
		})
	}

	// Restore: the same requests succeed (or fail for their own reasons, not
	// because of the archive), and each of them writes.
	f.restore(t)
	for _, r := range routes {
		t.Run("restored/"+r.name, func(t *testing.T) {
			res := f.run(t, r)
			if strings.Contains(string(res.body), schema.ErrorCodeChannelArchived) {
				t.Errorf("%s %s after the restore = %s, still archived", r.method, r.path, res)
			}
		})
	}
}

// TestArchivedChannelAllowsReadingLeavingAndRemoving: decision 6, route by
// route.
func TestArchivedChannelAllowsReadingLeavingAndRemoving(t *testing.T) {
	f := newArchiveFixture(t)
	routes := f.allowedRoutes()
	f.archive(t)
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			if res := f.run(t, r); res.status != r.status {
				t.Fatalf("%s %s = %s, want %d", r.method, r.path, res, r.status)
			}
		})
	}
	// The history is all there.
	res := f.callREST(t, http.MethodGet, "/v2/channels/ops/messages", f.p("ann").token, "")
	if !strings.Contains(res.body, "before the archive") {
		t.Errorf("history of an archived channel = %s", res.body)
	}
}

// TestArchiveListing: GET /v1/channels carries archived_at for a member; the
// directory omits it for a non-member and scope=all shows it to a holder of
// manage_channels (decision 9); restoring takes the mark off.
func TestArchiveListing(t *testing.T) {
	f := newArchiveFixture(t)
	f.archive(t)
	mine := decodeWire[schema.ListChannelsResponse](t, f.callREST(t, http.MethodGet, "/v1/channels", f.p("ann").token, "").body)
	if len(mine.Channels) != 1 || mine.Channels[0].ArchivedAt == nil {
		t.Errorf("ann's channels = %+v, want ops with archived_at", mine.Channels)
	}
	list := func(who, query string) []schema.ChannelListingV1 {
		t.Helper()
		res := f.callREST(t, http.MethodGet, "/v1/directory/channels"+query, f.p(who).token, "")
		if res.status != http.StatusOK {
			t.Fatalf("directory%s as %s = %d %s", query, who, res.status, res.body)
		}
		return decodeWire[schema.ChannelDirectoryResponseV1](t, res.body).Channels
	}
	if rows := list("ann", ""); len(rows) != 1 || rows[0].ArchivedAt == nil || !rows[0].Joined {
		t.Errorf("a member's directory = %+v, want ops marked archived", rows)
	}
	if rows := list("nora", ""); len(rows) != 0 {
		t.Errorf("a non-member's directory = %+v, want nothing: the channel cannot be joined", rows)
	}
	if rows := list("root", "?scope=all"); len(rows) != 1 || rows[0].ArchivedAt == nil {
		t.Errorf("scope=all = %+v, want ops marked archived", rows)
	}
	f.restore(t)
	if rows := list("nora", ""); len(rows) != 1 || rows[0].ArchivedAt != nil {
		t.Errorf("after the restore, a non-member's directory = %+v, want ops live", rows)
	}
}

// TestArchiveNameStaysTaken: creating or renaming to an archived channel's
// name is the existing 409 channel_exists (decision 12).
func TestArchiveNameStaysTaken(t *testing.T) {
	f := newArchiveFixture(t)
	f.archive(t)
	other, err := f.srv.store.CreateChannel(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	if res := f.callREST(t, http.MethodPost, "/v1/channels", f.p("ann").token, createChannelV1Body("OPS", schema.ChannelPublic)); res.status != http.StatusConflict || !strings.Contains(res.body, "channel_exists") {
		t.Errorf("create with the archived name = %d %s, want 409 channel_exists", res.status, res.body)
	}
	body := `{"from":"other","name":"ops"}`
	if res := f.callREST(t, http.MethodPut, fmt.Sprintf("/v1/channels/%d/name", other.ID), f.rootTok, body); res.status != http.StatusConflict || !strings.Contains(res.body, "channel_exists") {
		t.Errorf("rename to the archived name = %d %s, want 409 channel_exists", res.status, res.body)
	}
}

// TestArchiveDefaultChannel: archiving a default channel, or in the same
// request marking it default, is 409 channel_archive_default and changes
// nothing, audited nowhere (decision 4).
func TestArchiveDefaultChannel(t *testing.T) {
	f := newArchiveFixture(t)
	yes := true
	if _, err := f.srv.store.ChannelSettings(context.Background(), "system", 0, f.ops.ID, store.ChannelSettingsChange{Default: &yes}, time.Now()); err != nil {
		t.Fatal(err)
	}
	other, err := f.srv.store.CreateChannel(context.Background(), "side")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		id   int64
		body string
	}{
		{"a default channel", f.ops.ID, `{"archived":true}`},
		{"unmarking in the same request", f.ops.ID, `{"archived":true,"default":false}`},
		{"marking default in the same request", other.ID, `{"archived":true,"default":true,"visibility":"public"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := len(f.audit(t))
			res := f.settings(t, f.rootTok, tt.id, tt.body)
			assertWire(t, res, http.StatusConflict, schema.ErrorCodeChannelArchiveDefault)
			if len(f.audit(t)) != before {
				t.Error("a refused archive was audited")
			}
			ch, err := f.srv.store.ChannelByID(context.Background(), tt.id)
			if err != nil || ch.Archived() {
				t.Errorf("channel after the refusal = %+v, %v; want live", ch, err)
			}
		})
	}
	if ch, _ := f.srv.store.ChannelByID(context.Background(), f.ops.ID); !ch.IsDefault {
		t.Error("the refused archive unmarked the default channel")
	}
	// Stopping it being a default first, then archiving, works: a deliberate
	// two-step, and open sign-up going off is the first step's doing.
	if res := f.settings(t, f.rootTok, f.ops.ID, `{"default":false}`); res.status != http.StatusOK {
		t.Fatalf("unmark = %d %s", res.status, res.body)
	}
	f.archive(t)
}

func assertWire(t *testing.T, res wireResult, status int, code string) {
	t.Helper()
	if res.status != status || !strings.Contains(res.body, `"code":"`+code+`"`) {
		t.Errorf("answer = %d %s, want %d %s", res.status, res.body, status, code)
	}
}

// TestArchiveAuditRows: archive and restore are audited as
// channel_settings_changed with archived=false->true and true->false, by the
// caller; an archive of an archived channel writes nothing.
func TestArchiveAuditRows(t *testing.T) {
	f := newArchiveFixture(t)
	before := len(f.audit(t))
	f.archive(t)
	f.archive(t) // changes nothing
	f.restore(t)
	var got []string
	for _, e := range f.audit(t)[before:] {
		if e.Action == store.AuditChannelSettingsChanged {
			got = append(got, e.Actor+" "+e.Subject+" "+e.Detail)
		}
	}
	actor, subject := fmt.Sprintf("principal:%d", f.root.ID), fmt.Sprintf("channel:%d", f.ops.ID)
	want := []string{actor + " " + subject + " archived=false->true", actor + " " + subject + " archived=true->false"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("channel_settings_changed rows = %q, want %q", got, want)
	}
}

// TestArchivePermissionMatrix: who may archive and restore with PUT
// /v1/channels/{id}/settings. The owner and an operator holding
// manage_channels do; an operator without it, a channel manager, a member and
// a member who is not in the channel are refused, and a refusal is the same
// bytes whether the channel exists, is archived, or does not (the refusal
// comes before the id is read, so it tells a stranger nothing).
func TestArchivePermissionMatrix(t *testing.T) {
	for _, c := range channelV1Callers {
		t.Run(c.name, func(t *testing.T) {
			f := newChannelV1Fixture(t)
			if c.restricted {
				f.withhold(t, schema.PermManageChannels)
			}
			want := map[string]struct {
				status int
				code   string
			}{
				"restricted operator":             {http.StatusForbidden, schema.ErrorCodePermissionDenied},
				"manager":                         {http.StatusForbidden, "forbidden"},
				"member":                          {http.StatusForbidden, "forbidden"},
				"guest":                           {http.StatusForbidden, "forbidden"},
				"ended guest replaying its token": {http.StatusUnauthorized, "unauthenticated"},
				"agent":                           {http.StatusForbidden, "forbidden"},
				"unauthenticated":                 {http.StatusUnauthorized, "unauthenticated"},
			}[c.name]
			allowed := want.status == 0
			target := f.pubJoined // member and the manager are in it; the operator is not
			put := func(token string, id int64, archived bool) string {
				t.Helper()
				rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", id), token, fmt.Sprintf(`{"archived":%t}`, archived))
				if allowed {
					if rec.Code != http.StatusOK {
						t.Fatalf("settings archived=%t = %d %s, want 200", archived, rec.Code, rec.Body)
					}
					return rec.Body.String()
				}
				assertErrorBody(t, rec, want.status, want.code)
				return rec.Body.String()
			}
			archivedBody := put(c.token(f), target.ID, true)
			var ghost, elsewhere string
			if !allowed {
				ghost = put(c.token(f), 9999, true)
				elsewhere = put(c.token(f), f.pubOther.ID, true)
			}
			ch, err := f.srv.store.ChannelByID(context.Background(), target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if allowed {
				if !ch.Archived() {
					t.Fatal("an allowed archive did not archive")
				}
				_ = put(c.token(f), target.ID, false)
				if ch, _ := f.srv.store.ChannelByID(context.Background(), target.ID); ch.Archived() {
					t.Error("an allowed restore did not restore")
				}
				return
			}
			if ch.Archived() {
				t.Error("a refused archive archived")
			}
			// Archived out of band, a refusal still reads the same, so the
			// answer cannot tell a stranger the channel is archived.
			archived := true
			if _, err := f.srv.store.ChannelSettings(context.Background(), "system", 0, target.ID, store.ChannelSettingsChange{Archived: &archived}, time.Now()); err != nil {
				t.Fatal(err)
			}
			again := put(c.token(f), target.ID, false)
			for name, body := range map[string]string{"a ghost id": ghost, "another channel": elsewhere, "an archived channel": again} {
				if body != archivedBody {
					t.Errorf("%s answers %s, want the bytes %s", name, body, archivedBody)
				}
			}
			if ch, _ := f.srv.store.ChannelByID(context.Background(), target.ID); !ch.Archived() {
				t.Error("a refused restore restored")
			}
		})
	}
}

// TestArchivedChannelLeaksNothingToANonMember: a caller outside a private
// archived channel gets the answers a channel that does not exist gets, on
// every route that names it; the archived check comes after the membership
// check, never before.
func TestArchivedChannelLeaksNothingToANonMember(t *testing.T) {
	f := newArchiveFixture(t)
	private := schema.ChannelPrivate
	if _, err := f.srv.store.ChannelSettings(context.Background(), "system", 0, f.ops.ID, store.ChannelSettingsChange{Visibility: &private}, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.archive(t)
	js := "application/json"
	ghost := func(path string) string { return strings.Replace(path, "/ops", "/nowhere", 1) }
	for _, r := range []archiveRoute{
		{path: "/v0/channels/ops/messages", method: "POST", body: `{"body":"x"}`, ctype: js},
		{path: "/v1/channels/ops/messages", method: "POST", body: `{"body":"x"}`, ctype: js},
		{path: "/v2/channels/ops/messages", method: "POST", body: `{"body":"x"}`, ctype: js},
		{path: "/v2/channels/ops/messages", method: "POST", body: `{"body":"x","thread_root_id":1}`, ctype: js},
		{path: uploadPath("ops", "x.txt"), method: "POST", body: "x", ctype: "application/octet-stream"},
		{path: "/v1/channels/ops/join", method: "POST"},
		{path: "/v1/channels/ops/voice/session", method: "POST"},
		{path: "/v1/channels/ops/voice/share", method: "POST"},
		{path: "/v1/channels/ops/voice/camera", method: "POST"},
		{path: "/v1/channels/ops/voice/companion", method: "POST", body: `{"publish":"none","subscribe":true}`, ctype: js},
		{path: fmt.Sprintf("/v2/channels/ops/messages/%d/body", f.msg), method: "PUT", body: `{"body":"x"}`, ctype: js},
		{path: rpath("ops", f.msg, archiveEmoji), method: "PUT"},
		{path: rpath("ops", f.msg, archiveEmoji), method: "DELETE"},
		{path: "/v1/channels/ops/members", method: "POST", body: `{"name":"nora"}`, ctype: js},
		{path: "/v1/channels/ops/read-state", method: "PUT", body: `{"last_read_id":1}`, ctype: js},
		{path: "/v1/channels/ops/leave", method: "POST"},
	} {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			r.who = "nora"
			got := f.run(t, r)
			r.path = ghost(r.path)
			want := f.run(t, r)
			if got.status != want.status || string(got.body) != string(want.body) {
				t.Errorf("a non-member's %s %s = %s, a channel that does not exist gives %s", r.method, r.path, got, want)
			}
			if strings.Contains(string(got.body), schema.ErrorCodeChannelArchived) {
				t.Errorf("the answer names the archive: %s", got)
			}
		})
	}
	// An agent outside the channel gets the same through MCP.
	f.manifest(t, "nomad", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost)
	out := f.mcpCall(t, f.p("nomad").token, "post_message", map[string]any{"channel": "ops", "body": "x"})
	if out.result() != errChannelNotFound.Code {
		t.Errorf("an agent outside the archived channel posting = %s, want %s", out.result(), errChannelNotFound.Code)
	}
}

// TestArchiveKeepsHistoryAndReadMarks: nothing about the channel's content is
// touched by archive and restore, read through the wire.
func TestArchiveKeepsHistoryAndReadMarks(t *testing.T) {
	f := newArchiveFixture(t)
	if res := f.callREST(t, http.MethodPut, "/v1/channels/ops/read-state", f.p("ann").token, fmt.Sprintf(`{"last_read_id":%d}`, f.msg)); res.status != http.StatusOK {
		t.Fatalf("mark read = %d %s", res.status, res.body)
	}
	read := func() string {
		return f.callREST(t, http.MethodGet, "/v1/read-state", f.p("ann").token, "").body
	}
	history := func() string {
		return f.callREST(t, http.MethodGet, "/v2/channels/ops/messages", f.p("ann").token, "").body
	}
	members := func() string {
		return f.callREST(t, http.MethodGet, "/v1/channels/ops/members", f.p("ann").token, "").body
	}
	before := [3]string{read(), history(), members()}
	f.archive(t)
	f.restore(t)
	if after := [3]string{read(), history(), members()}; after != before {
		t.Errorf("archive and restore changed what is read:\nbefore %q\nafter  %q", before, after)
	}
}

// TestArchiveWritesNoApprovalRows: the settings change itself writes no
// approval row. What an archived channel does to approvals is issue #1296's
// (approvals_archive_test.go).
func TestArchiveWritesNoApprovalRows(t *testing.T) {
	f := newArchiveFixture(t)
	f.archive(t)
	f.restore(t)
	for _, e := range f.audit(t) {
		if strings.HasPrefix(e.Action, "approval") {
			t.Errorf("archive wrote an approval audit row: %+v", e)
		}
	}
}

// TestArchivedChannelEncodesNothingForLive: a live channel's documents carry no
// archived_at at all (omitted, not null), on every route that carries it.
func TestLiveChannelHasNoArchivedAt(t *testing.T) {
	f := newArchiveFixture(t)
	for _, path := range []string{"/v1/channels", "/v1/directory/channels", "/v1/directory/channels?scope=all"} {
		tok := f.p("root").token
		if body := f.callREST(t, http.MethodGet, path, tok, "").body; strings.Contains(body, "archived_at") {
			t.Errorf("%s of a live channel = %s, want no archived_at", path, body)
		}
	}
	res := f.settings(t, f.rootTok, f.ops.ID, `{"visibility":"public"}`)
	if strings.Contains(res.body, "archived_at") {
		t.Errorf("a settings answer for a live channel carries archived_at: %s", res.body)
	}
}

// decodeWire decodes a response body captured by callREST.
func decodeWire[T any](t *testing.T, body string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return v
}
