package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Preview as member, the server (issue #1515, design
// docs/design/permission-preview.md §3 to §5, §12). The world is
// directoryFixture's: root and own2 are owners, op2 an operator, alice, mgr
// and cand members, bot an agent, and a disabled, a hidden and an ended-guest
// principal who cannot be previewed.

// previewWorld is a directoryFixture under one preset, optionally with one
// permission withheld from operators.
type previewWorld struct {
	*directoryFixture
}

func newPreviewWorld(t *testing.T, preset schema.PermissionPreset, withhold schema.Permission) *previewWorld {
	t.Helper()
	w := &previewWorld{newDirectoryFixture(t)}
	ctx := context.Background()
	put := func(req schema.PutPermissionsRequestV1) {
		t.Helper()
		doc, err := w.srv.store.Permissions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		req.Revision = doc.Revision
		if _, err := w.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", w.root.ID), w.root.ID, req, time.Now()); err != nil {
			t.Fatalf("put permissions %+v: %v", req, err)
		}
	}
	put(schema.PutPermissionsRequestV1{Preset: preset})
	if withhold != "" {
		changes := []schema.PermissionChangeV1{{Permission: withhold, Role: schema.PermissionRoleOperator, Granted: false}}
		if withhold == schema.PermManageMembers {
			// act_as_members depends on manage_members.
			changes = append([]schema.PermissionChangeV1{{Permission: schema.PermActAsMembers, Role: schema.PermissionRoleOperator, Granted: false}}, changes...)
		}
		put(schema.PutPermissionsRequestV1{Changes: changes})
	}
	return w
}

// preview sends GET path with token and, when as is not empty, the header.
func (w *previewWorld) preview(t *testing.T, path, token, as string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if as != "" {
		req.Header.Set(previewHeader, as)
	}
	rec := httptest.NewRecorder()
	w.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func id(p store.Principal) string { return strconv.FormatInt(p.ID, 10) }

// answer is the part of a response a matrix row pins.
type answer struct {
	status int
	code   string
	perm   schema.Permission
}

func answerOf(t *testing.T, rec *httptest.ResponseRecorder) answer {
	t.Helper()
	a := answer{status: rec.Code}
	if rec.Code >= 400 {
		var e schema.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("status %d with a body that is not an error: %q", rec.Code, rec.Body.String())
		}
		a.code, a.perm = e.Code, e.Permission
	}
	return a
}

// TestPreviewMatrix is the server matrix of #1007: every caller against every
// target, under both presets, with the codes of design §3.1 in §4.2's order.
func TestPreviewMatrix(t *testing.T) {
	ok := answer{status: http.StatusOK}
	notFound := answer{http.StatusNotFound, "principal_not_found", ""}
	ownerOnly := answer{http.StatusForbidden, "forbidden", ""}
	self := answer{http.StatusBadRequest, "invalid_request", ""}

	for _, preset := range []schema.PermissionPreset{schema.PresetPersonal, schema.PresetGroup} {
		for _, withhold := range []schema.Permission{"", schema.PermManageMembers, schema.PermManageChannels} {
			t.Run(fmt.Sprintf("%s/withhold=%s", preset, withhold), func(t *testing.T) {
				w := newPreviewWorld(t, preset, withhold)
				type caller struct {
					name  string
					token string
					p     store.Principal
					// self: what previewing oneself answers when the caller
					// gets that far.
					// op is true for a caller subject to withhold.
					op bool
					// want overrides the answer for every target (a caller
					// refused before the target is looked at).
					want *answer
					// ownerTarget is the answer for an owner target.
					ownerTarget answer
				}
				missing := answer{http.StatusForbidden, schema.ErrorCodePermissionDenied, withhold}
				refusedAll := func(a answer) *answer { return &a }
				opWant := (*answer)(nil)
				if withhold != "" {
					opWant = refusedAll(missing)
				}
				callers := []caller{
					{name: "owner", token: w.rootTok, p: w.root, ownerTarget: ok},
					{name: "operator", token: w.op2Tok, p: w.op2, op: true, want: opWant, ownerTarget: ownerOnly},
					{name: "member", token: w.aliceTok, p: w.alice, want: refusedAll(answer{http.StatusForbidden, "forbidden", ""})},
					{name: "agent", token: w.botTok, p: w.bot, want: refusedAll(answer{http.StatusForbidden, "forbidden", ""})},
					{name: "guest", token: w.guestTok, p: w.guest, want: refusedAll(answer{http.StatusForbidden, schema.ErrorCodeGuestNotAllowed, ""})},
				}
				targets := []struct {
					name string
					as   string
					want func(c caller) answer
				}{
					{"member", id(w.cand), func(caller) answer { return ok }},
					{"manager member", id(w.mgr), func(caller) answer { return ok }},
					{"operator", id(w.op2), func(c caller) answer {
						if c.p.ID == w.op2.ID {
							return self
						}
						return ok
					}},
					{"owner", id(w.own2), func(c caller) answer { return c.ownerTarget }},
					{"self", "", func(caller) answer { return self }}, // as is the caller's own id
					{"agent", id(w.bot), func(caller) answer { return notFound }},
					{"disabled", id(w.disabled), func(caller) answer { return notFound }},
					{"hidden", id(w.hidden), func(caller) answer { return notFound }},
					{"ended guest", id(w.endedGuest), func(caller) answer { return notFound }},
					{"unknown id", "987654", func(caller) answer { return notFound }},
					{"out of sequence", "99999999999999999999999", func(caller) answer { return notFound }},
				}
				for _, c := range callers {
					for _, tg := range targets {
						want := tg.want(c)
						if c.want != nil {
							want = *c.want
						}
						as := tg.as
						if as == "" {
							as = id(c.p)
						}
						// The owner target of the operator caller is also
						// the owner's own mark: the owner previewing itself
						// is "self", handled by as.
						got := answerOf(t, w.preview(t, "/v1/preview", c.token, as))
						if got != want {
							t.Errorf("%s previewing %s (%s): %+v, want %+v", c.name, tg.name, as, got, want)
						}
					}
				}
			})
		}
	}
}

// TestPreviewRefusalBytes pins the bytes of the refusals that design §3.1
// names, among them the 404 that is byte-identical to the manager-add
// routes' ErrNotAddable answer.
func TestPreviewRefusalBytes(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetPersonal, "")
	notFound := `{"code":"principal_not_found","message":"principal not found"}` + "\n"
	for _, tc := range []struct {
		name, token, as string
		status          int
		body            string
	}{
		{"unknown", w.rootTok, "987654", 404, notFound},
		{"hidden", w.rootTok, id(w.hidden), 404, notFound},
		{"disabled", w.rootTok, id(w.disabled), 404, notFound},
		{"agent", w.rootTok, id(w.bot), 404, notFound},
		{"ended guest", w.rootTok, id(w.endedGuest), 404, notFound},
		{"non-owner on owner", w.op2Tok, id(w.root), 403, `{"code":"forbidden","message":"only an owner may act on an owner"}` + "\n"},
		{"member caller", w.aliceTok, id(w.cand), 403, `{"code":"forbidden","message":"operator role required"}` + "\n"},
		{"self", w.rootTok, id(w.root), 400, `{"code":"invalid_request","message":"cannot preview yourself"}` + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := w.preview(t, "/v1/preview", tc.token, tc.as)
			if rec.Code != tc.status || rec.Body.String() != tc.body {
				t.Errorf("= %d %q, want %d %q", rec.Code, rec.Body.String(), tc.status, tc.body)
			}
		})
	}
	// The same bytes as the manager-add route's answer for an unknown id.
	rec := w.do(t, http.MethodPut, "/v1/channels/general/members/987654", w.mgrTok, "")
	if rec.Code != 404 || rec.Body.String() != notFound {
		t.Errorf("manager-add 404 = %d %q; the preview 404 above must equal it", rec.Code, rec.Body.String())
	}
}

// TestPreviewMalformedHeader: a header that is not a positive decimal is 400
// on a previewable route, and 403 preview_read_only on any other, before the
// value is read.
func TestPreviewMalformedHeader(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetPersonal, "")
	for _, v := range []string{" 5", "5 ", "+5", "-5", "0", "00", "abc", "1e3", "0x5", "5,6", "٣"} {
		t.Run(fmt.Sprintf("%q", v), func(t *testing.T) {
			rec := w.preview(t, "/v1/channels", w.rootTok, v)
			if got := answerOf(t, rec); got != (answer{400, "invalid_request", ""}) {
				t.Errorf("GET /v1/channels = %+v", got)
			}
			rec = w.preview(t, "/v1/whoami", w.rootTok, v)
			if got := answerOf(t, rec); got != (answer{403, schema.ErrorCodePreviewReadOnly, ""}) {
				t.Errorf("GET /v1/whoami = %+v", got)
			}
		})
	}
	t.Run("empty and repeated", func(t *testing.T) {
		for _, vals := range [][]string{{""}, {id(w.cand), id(w.mgr)}} {
			req := httptest.NewRequest(http.MethodGet, "/v1/channels", nil)
			req.Header.Set("Authorization", "Bearer "+w.rootTok)
			req.Header[http.CanonicalHeaderKey(previewHeader)] = vals
			rec := httptest.NewRecorder()
			w.srv.Handler().ServeHTTP(rec, req)
			if got := answerOf(t, rec); got != (answer{400, "invalid_request", ""}) {
				t.Errorf("%q = %+v", vals, got)
			}
		}
	})
	t.Run("entry read needs the header", func(t *testing.T) {
		rec := w.preview(t, "/v1/preview", w.rootTok, "")
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "preview target required") {
			t.Errorf("= %d %q", rec.Code, rec.Body.String())
		}
	})
}

// TestPreviewAuthOff: a server with no authentication has no caller to
// preview from, so the header is 400 wherever it is sent.
func TestPreviewAuthOff(t *testing.T) {
	srv := newTestServer(t)
	for _, path := range []string{"/v1/preview", "/v1/channels", "/v1/whoami"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set(previewHeader, "2")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "preview needs authentication") {
			t.Errorf("GET %s with the header under AuthOff = %d %q", path, rec.Code, rec.Body.String())
		}
	}
}

// TestPreviewRefusesEveryOtherRoute sends the header to every route of the
// table that is not previewable, with a real principal, an unknown id and a
// malformed value: each is 403 preview_read_only, before any target lookup
// (the unknown id and the junk are not 404 and 400), audited access_denied
// with preview_as in the detail, and changes nothing. Writes, sockets and
// POST /v1/ws-tickets are in the table because the table is the routeTable.
func TestPreviewRefusesEveryOtherRoute(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetGroup, "")
	before := dumpTables(t, w.srv)
	routes := 0
	for _, rt := range w.srv.routes {
		if rt.access == accessExempt || rt.previewable {
			continue
		}
		exp, found := routeExpectations[rt.pattern]
		if !found {
			t.Fatalf("route %q has no routeExpectation", rt.pattern)
		}
		method := strings.Fields(rt.pattern)[0]
		for _, as := range []struct{ name, value, detail string }{
			{"member", id(w.alice), "preview_as=" + id(w.alice)},
			{"unknown", "987654", "preview_as=987654"},
			{"junk", "not-an-id", "preview_as=invalid"},
		} {
			routes++
			req := httptest.NewRequest(method, exp.path, strings.NewReader(exp.body))
			req.Header.Set("Authorization", "Bearer "+w.rootTok)
			req.Header.Set(previewHeader, as.value)
			rec := httptest.NewRecorder()
			w.srv.Handler().ServeHTTP(rec, req)
			if got := answerOf(t, rec); got != (answer{403, schema.ErrorCodePreviewReadOnly, ""}) {
				t.Errorf("%s with %s: %+v", rt.pattern, as.name, got)
				continue
			}
			if !strings.Contains(rec.Body.String(), "this route cannot be read in preview") {
				t.Errorf("%s: message %q", rt.pattern, rec.Body.String())
			}
			found = false
			for _, e := range w.audit(t) {
				if e.Action == "access_denied" && e.Subject == rt.pattern && e.Actor == "principal:"+id(w.root) &&
					strings.Contains(e.Detail, "code=") == false && strings.Contains(e.Detail, schema.ErrorCodePreviewReadOnly) &&
					strings.Contains(e.Detail, as.detail) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s with %s: no access_denied row naming %s", rt.pattern, as.name, as.detail)
			}
		}
	}
	if routes < 100 {
		t.Errorf("only %d requests ran; the table walk is not reaching the routes", routes)
	}
	for _, must := range []string{"POST /v1/ws-tickets", "GET /v2/ws", "GET /v1/events/ws", "GET /v1/whoami", "POST /v1/channels/{channel}/messages"} {
		if _, found := w.srv.routeByPattern[must]; !found || w.srv.routeByPattern[must].previewable {
			t.Errorf("%s must be a route that refuses the header", must)
		}
	}
	if after := dumpTables(t, w.srv); !slices.Equal(before, after) {
		t.Errorf("a refused preview request changed the database:\n%s", diffDumps(before, after))
	}
}

// TestPreviewRefusalNamesTheAdministrator: a refusal on a non-previewable
// route is audited with the administrator as actor, never the target.
func TestPreviewRefusalCallerIsAudited(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetPersonal, "")
	for _, tc := range []struct {
		name  string
		token string
		who   store.Principal
	}{{"member", w.aliceTok, w.alice}, {"agent", w.botTok, w.bot}, {"operator", w.op2Tok, w.op2}} {
		rec := w.preview(t, "/v1/whoami", tc.token, id(w.cand))
		if got := answerOf(t, rec); got != (answer{403, schema.ErrorCodePreviewReadOnly, ""}) {
			t.Fatalf("%s: %+v", tc.name, got)
		}
		found := false
		for _, e := range w.audit(t) {
			if e.Action == "access_denied" && e.Subject == "GET /v1/whoami" && e.Actor == "principal:"+id(tc.who) {
				found = true
			}
			if e.Actor == "principal:"+id(w.cand) {
				t.Errorf("an audit row names the target as actor: %+v", e)
			}
		}
		if !found {
			t.Errorf("%s: no access_denied row for the caller", tc.name)
		}
	}
}

// TestPreviewMCPIsInert: /mcp is exempt from guard and admits agents only; it
// never reads the header, so it answers exactly as without it.
func TestPreviewMCPIsInert(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetPersonal, "")
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	for _, tc := range []struct{ name, token string }{{"agent", w.botTok}, {"operator", w.rootTok}, {"anonymous", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			do := func(as string) (int, string) {
				req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Accept", "application/json, text/event-stream")
				if tc.token != "" {
					req.Header.Set("Authorization", "Bearer "+tc.token)
				}
				if as != "" {
					req.Header.Set(previewHeader, as)
				}
				rec := httptest.NewRecorder()
				w.srv.Handler().ServeHTTP(rec, req)
				return rec.Code, rec.Body.String()
			}
			codeA, bodyA := do("")
			codeB, bodyB := do(id(w.cand))
			if codeA != codeB || bodyA != bodyB {
				t.Errorf("/mcp with the header = %d %q, without = %d %q", codeB, bodyB, codeA, bodyA)
			}
		})
	}
}

// TestPreviewReadsAnswerAsTheTarget: the four existing previewable reads
// return the same bytes with the target's own token as with the
// administrator's token and the header, under both presets.
func TestPreviewReadsAnswerAsTheTarget(t *testing.T) {
	for _, preset := range []schema.PermissionPreset{schema.PresetPersonal, schema.PresetGroup} {
		t.Run(string(preset), func(t *testing.T) {
			w := newPreviewWorld(t, preset, "")
			// A private channel the target is not in, one it is in and
			// manages, so the answers differ by target.
			if _, err := w.srv.store.CreateUnownedChannel(context.Background(), "system", "secret", schema.ChannelPrivate); err != nil {
				t.Fatal(err)
			}
			targets := []struct {
				name  string
				p     store.Principal
				token string
			}{
				{"member", w.cand, w.candTok},
				{"manager", w.mgr, w.mgrTok},
				{"alice", w.alice, w.aliceTok},
				{"operator", w.op2, w.op2Tok},
			}
			paths := []string{
				"/v1/channels",
				"/v1/directory/channels",
				"/v1/directory/channels?scope=all",
				"/v1/directory/channels?scope=bogus",
				"/v1/directory/members",
				"/v1/directory/members?limit=2",
				"/v1/channels/general/members",
				"/v1/channels/lobby/members",
				"/v1/channels/secret/members",
				"/v1/channels/never-made/members",
			}
			for _, admin := range []struct{ name, token string }{{"owner", w.rootTok}} {
				for _, tg := range targets {
					for _, path := range paths {
						own := w.preview(t, path, tg.token, "")
						viaHeader := w.preview(t, path, admin.token, id(tg.p))
						if own.Code != viaHeader.Code || own.Body.String() != viaHeader.Body.String() {
							t.Errorf("%s as %s: own token = %d %q, %s with the header = %d %q",
								path, tg.name, own.Code, own.Body.String(), admin.name, viaHeader.Code, viaHeader.Body.String())
						}
					}
				}
			}
		})
	}
}

// TestPreviewNeverWidens is design §3.2: what the target sees is inside what
// the operator may read for itself. The operator's own ?scope=all view lists
// every channel, so the target's channel list is a subset of it.
func TestPreviewNeverWidens(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetGroup, "")
	names := func(rec *httptest.ResponseRecorder, key string) []string {
		var doc map[string][]struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, c := range doc[key] {
			out = append(out, c.Name)
		}
		return out
	}
	all := names(w.preview(t, "/v1/directory/channels?scope=all", w.op2Tok, ""), "channels")
	for _, tg := range []store.Principal{w.cand, w.mgr, w.alice} {
		for _, n := range names(w.preview(t, "/v1/channels", w.op2Tok, id(tg)), "channels") {
			if !slices.Contains(all, n) {
				t.Errorf("target %s sees channel %q, which the previewing operator cannot list", tg.Name, n)
			}
		}
	}
}

// TestPreviewEntryRead: GET /v1/preview answers PreviewResponseV1 with
// EffectivePermissions(target) and writes exactly one preview_viewed row.
func TestPreviewEntryRead(t *testing.T) {
	for _, preset := range []schema.PermissionPreset{schema.PresetPersonal, schema.PresetGroup} {
		t.Run(string(preset), func(t *testing.T) {
			w := newPreviewWorld(t, preset, "")
			for _, tg := range []store.Principal{w.cand, w.op2, w.own2} {
				before := len(w.audit(t))
				rec := w.preview(t, "/v1/preview", w.rootTok, id(tg))
				if rec.Code != 200 {
					t.Fatalf("%s: %d %s", tg.Name, rec.Code, rec.Body.String())
				}
				var got schema.PreviewResponseV1
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				perms, rev, err := w.srv.store.EffectivePermissions(context.Background(), tg.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := schema.PreviewResponseV1{ID: tg.ID, Kind: schema.PrincipalKind(tg.Kind), Name: tg.Name,
					Role: schema.PrincipalRole(tg.Role), Owner: tg.IsOwner(), Permissions: perms, PermissionsRevision: rev,
					MayInvite:    slices.Contains(perms, schema.PermMakeMemberLinks),
					VoiceAllowed: voiceAllowed(tg) && slices.Contains(perms, schema.PermUseVoice)}
				if got.ID != want.ID || got.Name != want.Name || got.Role != want.Role || got.Owner != want.Owner ||
					got.Kind != want.Kind || !slices.Equal(got.Permissions, want.Permissions) ||
					got.PermissionsRevision != want.PermissionsRevision || got.MayInvite != want.MayInvite ||
					got.VoiceAllowed != want.VoiceAllowed || got.Simulation != "" {
					t.Errorf("%s: %+v, want %+v", tg.Name, got, want)
				}
				if strings.Contains(rec.Body.String(), `"permissions":null`) {
					t.Errorf("permissions is null")
				}
				rows := w.audit(t)[before:]
				if len(rows) != 1 || rows[0].Action != AuditPreviewViewed || rows[0].Actor != "principal:"+id(w.root) ||
					rows[0].Subject != "principal:"+id(tg) || rows[0].Detail != "" {
					t.Errorf("%s: audit rows %+v, want one preview_viewed by the administrator about the target", tg.Name, rows)
				}
			}
			// The other four reads write no audit row.
			before := len(w.audit(t))
			for _, path := range []string{"/v1/channels", "/v1/directory/channels", "/v1/directory/members", "/v1/channels/lobby/members"} {
				if rec := w.preview(t, path, w.rootTok, id(w.cand)); rec.Code != 200 {
					t.Fatalf("%s: %d", path, rec.Code)
				}
			}
			if rows := w.audit(t)[before:]; len(rows) != 0 {
				t.Errorf("the four plain reads audited %+v", rows)
			}
		})
	}
}

// TestPreviewSideEffects diffs every table before and after a full preview
// run: only the audit log grows, and no row of it names the target as an
// actor.
func TestPreviewSideEffects(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetGroup, "")
	// A session and a read mark for the target exist beforehand, so a
	// preview that touched them would show.
	legacyExec(t, w.srv, "INSERT INTO read_marks (principal_id, channel_id, last_read_id, updated_at) VALUES (?, ?, 0, 1)", w.cand.ID, w.general.ID)
	before := dumpTables(t, w.srv)
	auditBefore := len(w.audit(t))
	for _, tg := range []store.Principal{w.cand, w.mgr, w.op2, w.own2} {
		for _, path := range []string{"/v1/preview", "/v1/channels", "/v1/directory/channels", "/v1/directory/channels?scope=all",
			"/v1/directory/members", "/v1/channels/lobby/members"} {
			w.preview(t, path, w.rootTok, id(tg))
		}
	}
	after := dumpTables(t, w.srv)
	if !slices.Equal(before, after) {
		t.Errorf("a preview run changed the database outside the audit log:\n%s", diffDumps(before, after))
	}
	for _, e := range w.audit(t)[auditBefore:] {
		for _, tg := range []store.Principal{w.cand, w.mgr, w.op2, w.own2} {
			if e.Actor == "principal:"+id(tg) {
				t.Errorf("audit row names the target %s as actor: %+v", tg.Name, e)
			}
		}
		if e.Action != AuditPreviewViewed && e.Action != "access_denied" {
			t.Errorf("a preview run wrote an audit row %q", e.Action)
		}
	}
}

// TestPreviewAuditActor: auditActor under preview is the administrator, and a
// denial forced under preview names the administrator and the target.
func TestPreviewAuditActor(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetPersonal, "")
	ctx := withPreview(withCaller(context.Background(), w.cand), previewContext{Actor: w.root, Target: w.cand})
	if got, want := auditActor(ctx), "principal:"+id(w.root); got != want {
		t.Errorf("auditActor under preview = %q, want %q", got, want)
	}
	if got, want := auditActor(withCaller(context.Background(), w.cand)), "principal:"+id(w.cand); got != want {
		t.Errorf("auditActor without preview = %q, want %q", got, want)
	}
	// ?scope=all as a member target is a handler-level denial of the target.
	rec := w.preview(t, "/v1/directory/channels?scope=all", w.rootTok, id(w.cand))
	if rec.Code != 403 {
		t.Fatalf("scope=all as a member = %d %s", rec.Code, rec.Body.String())
	}
	var found bool
	for _, e := range w.audit(t) {
		if e.Action == "access_denied" && e.Subject == "GET /v1/directory/channels" {
			found = true
			if e.Actor != "principal:"+id(w.root) || !strings.Contains(e.Detail, "preview_as="+id(w.cand)) {
				t.Errorf("denial row = %+v, want the administrator as actor and preview_as=%s", e, id(w.cand))
			}
		}
		if e.Actor == "principal:"+id(w.cand) {
			t.Errorf("an audit row names the target as actor: %+v", e)
		}
	}
	if !found {
		t.Error("the forced denial wrote no access_denied row")
	}
}

// TestPreviewRechecksPermissions: the caller's permissions are read per
// request, so withholding one ends preview on the next read.
func TestPreviewRechecksPermissions(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetPersonal, "")
	if rec := w.preview(t, "/v1/channels", w.op2Tok, id(w.cand)); rec.Code != 200 {
		t.Fatalf("before = %d %s", rec.Code, rec.Body.String())
	}
	doc, err := w.srv.store.Permissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.srv.store.PutPermissions(context.Background(), "principal:"+id(w.root), w.root.ID, schema.PutPermissionsRequestV1{
		Revision: doc.Revision, Changes: []schema.PermissionChangeV1{{Permission: schema.PermManageChannels, Role: schema.PermissionRoleOperator, Granted: false}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	got := answerOf(t, w.preview(t, "/v1/channels", w.op2Tok, id(w.cand)))
	if got != (answer{403, schema.ErrorCodePermissionDenied, schema.PermManageChannels}) {
		t.Errorf("after = %+v", got)
	}
}

// dumpTables is every table's rows but the audit log's, one string per
// table, for a before and after comparison.
func dumpTables(t *testing.T, srv *Server) []string {
	t.Helper()
	path, ok := testDBPaths.Load(srv)
	if !ok {
		t.Fatal("the fixture's database path is not recorded")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name <> 'audit_events' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	_ = rows.Close()
	var out []string
	for _, n := range names {
		r, err := db.QueryContext(ctx, "SELECT * FROM "+n) //nolint:gosec // n is a table name read from sqlite_master
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := r.Columns()
		var b strings.Builder
		b.WriteString(n + ":")
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "\n  %v", vals)
		}
		_ = r.Close()
		out = append(out, b.String())
	}
	return out
}

func diffDumps(a, b []string) string {
	var out []string
	for i := range a {
		if i < len(b) && a[i] != b[i] {
			out = append(out, "before: "+a[i], "after:  "+b[i])
		}
	}
	return strings.Join(out, "\n")
}

// TestPreviewRefusedOnExemptRoutes: every route that takes no credential
// refuses the header too, but /mcp, which never reads it. A hook ingest with
// a valid token and the header posts nothing; each other exempt route is 403
// preview_read_only, audited without a principal, and changes nothing.
func TestPreviewRefusedOnExemptRoutes(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetGroup, "")
	created := w.do(t, http.MethodPost, "/v1/hooks", w.rootTok,
		fmt.Sprintf(`{"channel":"general","principal":%d,"label":"preview"}`, w.alice.ID))
	if created.Code != http.StatusCreated {
		t.Fatalf("create hook: %d %s", created.Code, created.Body)
	}
	token := decodeBody[schema.CreateHookResponse](t, created).Token
	before := dumpTables(t, w.srv)
	auditBefore := len(w.audit(t))

	send := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(previewHeader, id(w.cand))
		rec := httptest.NewRecorder()
		w.srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	checked := 0
	for _, rt := range w.srv.routes {
		if rt.access != accessExempt || rt.pattern == "/mcp" {
			continue
		}
		method := strings.Fields(rt.pattern)[0]
		var path, body string
		exp, ok := routeExpectations[rt.pattern]
		switch {
		case rt.pattern == "POST /v1/hooks/{token}":
			path, body = "/v1/hooks/"+token, `{"body":"from a preview"}`
		case ok:
			path, body = exp.path, exp.body
		default:
			continue // a web asset route has no entry; covered below
		}
		checked++
		rec := send(method, path, body)
		if got := answerOf(t, rec); got != (answer{403, schema.ErrorCodePreviewReadOnly, ""}) {
			t.Errorf("%s with the header: %+v", rt.pattern, got)
		}
	}
	if checked < 10 {
		t.Errorf("only %d exempt routes checked", checked)
	}
	if after := dumpTables(t, w.srv); !slices.Equal(before, after) {
		t.Errorf("an exempt route with the header changed the database:\n%s", diffDumps(before, after))
	}
	for _, e := range w.audit(t)[auditBefore:] {
		if e.Action != "access_denied" || !strings.Contains(e.Detail, schema.ErrorCodePreviewReadOnly) {
			t.Errorf("unexpected audit row %+v", e)
		}
		if strings.Contains(e.Actor, token) || strings.Contains(e.Subject, token) || strings.Contains(e.Detail, token) {
			t.Errorf("the hook token reached the audit log: %+v", e)
		}
	}
}

// TestPreviewLogsNameTheAdministrator: an error logged under preview names
// the administrator as the requester, never the member being read as.
func TestPreviewLogsNameTheAdministrator(t *testing.T) {
	w := newPreviewWorld(t, schema.PresetPersonal, "")
	logs := captureServerLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the store read fails
	ctx = withPreview(withCaller(ctx, w.cand), previewContext{Actor: w.root, Target: w.cand})
	req := httptest.NewRequest(http.MethodGet, "/v1/channels", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	if _, ok := w.srv.canOrFail(rec, req, w.cand, schema.PermManageChannels); ok {
		t.Fatal("the permission read did not fail")
	}
	line := logs.String()
	if !strings.Contains(line, "permission check failed") {
		t.Fatalf("no error logged: %q", line)
	}
	if !strings.Contains(line, "principal="+id(w.root)) || !strings.Contains(line, "preview_as="+id(w.cand)) {
		t.Errorf("log %q must name principal=%s and preview_as=%s", line, id(w.root), id(w.cand))
	}
	if strings.Contains(line, "principal="+id(w.cand)) {
		t.Errorf("log names the target as the requester: %q", line)
	}
}
