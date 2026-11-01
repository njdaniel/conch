package server

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of the permissions document's routes, the permission route class
// and the live signal (issue #1015, ADR-014, docs/design/admin-roles.md §7.2,
// §8.2, §8.3 and §8.6). The fixture is A3a's: an owner (root), operator-1,
// two members and an agent; channel-a, the owner's, which operator-1 is not
// in; channel-b, which operator-1 manages.

// revisionOf reads the document's revision.
func revisionOf(t *testing.T, f *restrictedFixture) int64 {
	t.Helper()
	doc, err := f.srv.store.Permissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return doc.Revision
}

// putPermissions sends PUT /v1/permissions as tok.
func (f *restrictedFixture) putPermissions(t *testing.T, tok string, req schema.PutPermissionsRequestV1) *httpResult {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, "PUT", "/v1/permissions", tok, string(body))
	return &httpResult{code: rec.Code, body: rec.Body.Bytes()}
}

// httpResult is a recorded answer, kept after the recorder is gone.
type httpResult struct {
	code int
	body []byte
}

func (r *httpResult) errorCode(t *testing.T) string {
	t.Helper()
	var e schema.Error
	if err := json.Unmarshal(r.body, &e); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return e.Code
}

func (r *httpResult) document(t *testing.T) schema.PermissionsResponseV1 {
	t.Helper()
	var d schema.PermissionsResponseV1
	if err := json.Unmarshal(r.body, &d); err != nil {
		t.Fatalf("decode %s: %v", r.body, err)
	}
	return d
}

// cut takes manage_members, act_as_members and manage_channels from every
// operator through the real PUT, as the owner: the restricted operator of
// design §7.7.
func (f *restrictedFixture) cut(t *testing.T) {
	t.Helper()
	res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f), Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermManageMembers, Role: schema.PermissionRoleOperator, Granted: false},
		{Permission: schema.PermActAsMembers, Role: schema.PermissionRoleOperator, Granted: false},
		{Permission: schema.PermManageChannels, Role: schema.PermissionRoleOperator, Granted: false},
	}})
	if res.code != http.StatusOK {
		t.Fatalf("the owner's cut = %d %s", res.code, res.body)
	}
}

func TestPermissionsGet(t *testing.T) {
	f := newRestrictedFixture(t, Config{})

	owner := decodeBody[schema.PermissionsResponseV1](t, f.do(t, "GET", "/v1/permissions", f.rootTok, ""))
	if owner.Revision != 1 || len(owner.Rows) != len(schema.Permissions) {
		t.Fatalf("revision %d, %d rows; want 1 and every permission", owner.Revision, len(owner.Rows))
	}
	for i, row := range owner.Rows {
		if row.Permission != schema.Permissions[i] {
			t.Errorf("row %d is %s, want %s: rows are in catalog order", i, row.Permission, schema.Permissions[i])
		}
		if fixed, _ := row.Permission.Fixed(schema.PermissionRoleMember); fixed != row.MemberFixed {
			t.Errorf("%s: member_fixed %t, want %t", row.Permission, row.MemberFixed, fixed)
		}
	}
	if owner.Preset != schema.PresetGroup {
		t.Errorf("preset = %q, want group: the defaults are the group server", owner.Preset)
	}
	if len(owner.Presets) != len(schema.PresetMemberPermissions) {
		t.Errorf("presets = %+v, want every preset", owner.Presets)
	}
	if !slices.Equal(owner.Editable, []schema.PermissionRole{schema.PermissionRoleMember, schema.PermissionRoleOperator}) {
		t.Errorf("owner's editable = %v, want both columns", owner.Editable)
	}
	if len(owner.Warnings) != 0 || owner.IssuedToOthers != nil {
		t.Errorf("warnings %v, issued %v under the defaults", owner.Warnings, owner.IssuedToOthers)
	}

	op := decodeBody[schema.PermissionsResponseV1](t, f.do(t, "GET", "/v1/permissions", f.opTok, ""))
	if len(op.Editable) != 0 {
		t.Errorf("an operator without change_server_settings may edit %v, want none", op.Editable)
	}
	f.setOperatorCells(t, true, schema.PermChangeServerSettings)
	op = decodeBody[schema.PermissionsResponseV1](t, f.do(t, "GET", "/v1/permissions", f.opTok, ""))
	if !slices.Equal(op.Editable, []schema.PermissionRole{schema.PermissionRoleMember}) {
		t.Errorf("an operator with change_server_settings may edit %v, want the member column", op.Editable)
	}
	if op.Revision != 2 {
		t.Errorf("revision after one change = %d, want 2", op.Revision)
	}

	// Members, and agents, never see the matrix (O10).
	for _, tok := range []string{f.memberTok, f.agTok} {
		rec := f.do(t, "GET", "/v1/permissions", tok, "")
		if got := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusForbidden || got != `{"code":"forbidden","message":"operator role required"}` {
			t.Errorf("a member's read = %d %s", rec.Code, got)
		}
	}

	// open_signup_on: member links off, and the address still admits
	// accounts. make_member_links is not enforced yet (A4), so the cell is
	// set through the store, as A4's route will.
	ctx := context.Background()
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, schema.PutPermissionsRequestV1{Revision: doc.Revision,
		Changes: []schema.PermissionChangeV1{{Permission: schema.PermMakeMemberLinks, Role: schema.PermissionRoleMember, Granted: false}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := decodeBody[schema.PermissionsResponseV1](t, f.do(t, "GET", "/v1/permissions", f.rootTok, "")).Warnings; len(w) != 0 {
		t.Errorf("warnings with the address off = %v, want none", w)
	}
	general := "general"
	mode := schema.OpenJoiningAccount
	if _, err := f.srv.store.PutJoinSettings(ctx, "system", 0, schema.PutJoinSettingsRequestV1{OpenJoining: &mode, OpenChannels: &[]string{general}}); err != nil {
		t.Fatal(err)
	}
	if w := decodeBody[schema.PermissionsResponseV1](t, f.do(t, "GET", "/v1/permissions", f.rootTok, "")).Warnings; !slices.Equal(w, []schema.PermissionsWarning{schema.PermissionsWarningOpenSignupOn}) {
		t.Errorf("warnings with member links off and the address open = %v, want open_signup_on", w)
	}
}

func TestPermissionsPut(t *testing.T) {
	member := func(p schema.Permission, on bool) schema.PermissionChangeV1 {
		return schema.PermissionChangeV1{Permission: p, Role: schema.PermissionRoleMember, Granted: on}
	}
	operator := func(p schema.Permission, on bool) schema.PermissionChangeV1 {
		return schema.PermissionChangeV1{Permission: p, Role: schema.PermissionRoleOperator, Granted: on}
	}
	tests := []struct {
		name string
		// settings gives operators change_server_settings first.
		settings bool
		asOp     bool
		req      func(rev int64) schema.PutPermissionsRequestV1
		status   int
		code     string
		// changed says the revision moved on and one permissions_changed row
		// was written.
		changed bool
	}{
		{"owner patch", false, false, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Changes: []schema.PermissionChangeV1{member(schema.PermCreatePrivateChannels, false)}}
		}, http.StatusOK, "", true},
		{"owner preset group, already the document", false, false, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Preset: schema.PresetGroup}
		}, http.StatusOK, "", false},
		{"owner preset personal, which turns make_member_links off before A4 enforces it", false, false, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Preset: schema.PresetPersonal}
		}, http.StatusConflict, schema.ErrorCodePermissionsConflict, false},
		{"owner operator-column patch", false, false, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Changes: []schema.PermissionChangeV1{operator(schema.PermReadAuditLog, true)}}
		}, http.StatusOK, "", true},
		{"operator member patch without change_server_settings", false, true, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Changes: []schema.PermissionChangeV1{member(schema.PermCreatePrivateChannels, false)}}
		}, http.StatusForbidden, schema.ErrorCodePermissionDenied, false},
		{"operator member patch with change_server_settings", true, true, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Changes: []schema.PermissionChangeV1{member(schema.PermCreatePrivateChannels, false)}}
		}, http.StatusOK, "", true},
		{"operator operator-column patch", true, true, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Changes: []schema.PermissionChangeV1{operator(schema.PermReadAuditLog, true)}}
		}, http.StatusForbidden, "forbidden", false},
		{"stale revision", false, false, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev + 5, Changes: []schema.PermissionChangeV1{member(schema.PermCreatePrivateChannels, false)}}
		}, http.StatusConflict, schema.ErrorCodePermissionsStale, false},
		{"dependency breach: act_as_members without manage_members", false, false, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Changes: []schema.PermissionChangeV1{operator(schema.PermManageMembers, false)}}
		}, http.StatusConflict, schema.ErrorCodePermissionsConflict, false},
		{"a cell this server does not enforce yet", false, false, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Changes: []schema.PermissionChangeV1{member(schema.PermUseVoice, false)}}
		}, http.StatusConflict, schema.ErrorCodePermissionsConflict, false},
		{"no change", false, false, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Changes: []schema.PermissionChangeV1{member(schema.PermCreatePrivateChannels, true)}}
		}, http.StatusOK, "", false},
		{"malformed: both changes and a preset", false, false, func(rev int64) schema.PutPermissionsRequestV1 {
			return schema.PutPermissionsRequestV1{Revision: rev, Preset: schema.PresetGroup, Changes: []schema.PermissionChangeV1{member(schema.PermCreatePrivateChannels, true)}}
		}, http.StatusBadRequest, "invalid_request", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRestrictedFixture(t, Config{})
			if tt.settings {
				f.setOperatorCells(t, true, schema.PermChangeServerSettings)
			}
			tok := f.rootTok
			if tt.asOp {
				tok = f.opTok
			}
			before := revisionOf(t, f)
			rows := countAction(f.audit(t), store.AuditPermissionsChanged)
			res := f.putPermissions(t, tok, tt.req(before))
			if res.code != tt.status {
				t.Fatalf("status = %d %s, want %d", res.code, res.body, tt.status)
			}
			if tt.code != "" && res.errorCode(t) != tt.code {
				t.Fatalf("code = %q, want %q (%s)", res.errorCode(t), tt.code, res.body)
			}
			after := revisionOf(t, f)
			if moved := after != before; moved != tt.changed {
				t.Errorf("revision %d -> %d, want changed %t", before, after, tt.changed)
			}
			if got := countAction(f.audit(t), store.AuditPermissionsChanged) - rows; got != map[bool]int{true: 1, false: 0}[tt.changed] {
				t.Errorf("%d permissions_changed rows written", got)
			}
			if res.code == http.StatusOK && res.document(t).Revision != after {
				t.Errorf("the answer's revision %d is not the document's %d", res.document(t).Revision, after)
			}
			// The safety net and the precedence rule both name the permission.
			if res.code == http.StatusConflict && tt.code == schema.ErrorCodePermissionsConflict && !strings.Contains(string(res.body), "_") {
				t.Errorf("a conflict that names no permission: %s", res.body)
			}
		})
	}
}

// The PUT is in the re-authentication class for everyone: a browser session
// without a proof is refused before anything is read or written.
func TestPermissionsPutNeedsAProof(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	_, session, err := f.srv.store.CreateRenewableCredential(context.Background(), "system", f.root.ID, "browser", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res := f.putPermissions(t, session, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f),
		Changes: []schema.PermissionChangeV1{{Permission: schema.PermCreatePrivateChannels, Role: schema.PermissionRoleMember, Granted: false}}})
	if res.code != http.StatusForbidden || res.errorCode(t) != schema.ErrorCodeReauthRequired {
		t.Fatalf("the owner's browser session with no proof = %d %s, want 403 reauth_required", res.code, res.body)
	}
	if revisionOf(t, f) != 1 {
		t.Error("a refused PUT changed the document")
	}
}

// The audit export needs a proof from anyone but an owner (reauthNonOwners).
func TestAuditExportNeedsAProofFromNonOwners(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	f.setOperatorCells(t, true, schema.PermReadAuditLog)
	ctx := context.Background()
	_, opSession, err := f.srv.store.CreateRenewableCredential(ctx, "system", f.op.ID, "browser", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "GET", "/v1/audit?limit=1", opSession, ""); rec.Code != http.StatusForbidden || decodeBody[schema.Error](t, rec).Code != schema.ErrorCodeReauthRequired {
		t.Errorf("an operator's session holding read_audit_log, no proof = %d %s, want 403 reauth_required", rec.Code, rec.Body)
	}
	if rec := f.do(t, "GET", "/v1/audit?limit=1", f.opTok, ""); rec.Code != http.StatusOK {
		t.Errorf("the operator's script credential = %d %s, want 200 (outside the class)", rec.Code, rec.Body)
	}
	_, ownerSession, err := f.srv.store.CreateRenewableCredential(ctx, "system", f.root.ID, "browser", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "GET", "/v1/audit?limit=1", ownerSession, ""); rec.Code != http.StatusOK {
		t.Errorf("the owner's session with no proof = %d %s, want 200 as before ADR-014", rec.Code, rec.Body)
	}
}

// TestPermissionGuard is the guard over every kind of caller, on a route of
// each row of the remap (design §7.2), the fixed operator class, the manager
// class and the operator-or-self class: under the defaults, and for the
// restricted operator the owner made through the real PUT.
func TestPermissionGuard(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	type want struct {
		status int
		code   string
		perm   schema.Permission
	}
	ok := func(status int) want { return want{status: status} }
	denied := func(p schema.Permission) want {
		return want{http.StatusForbidden, schema.ErrorCodePermissionDenied, p}
	}
	forbidden := want{http.StatusForbidden, "forbidden", ""}
	unauth := want{http.StatusUnauthorized, "unauthenticated", ""}
	notFound := want{http.StatusNotFound, "channel_not_found", ""}
	manifest := fmt.Sprintf("/v1/principals/%d/manifest", f.agent.ID)
	routes := []struct {
		name, method, path, body string
		// owner, operator, member, agent, unauthenticated; then the
		// restricted operator.
		want       [5]want
		restricted want
	}{
		{"manage_members", "GET", "/v1/principals", "", [5]want{ok(200), ok(200), forbidden, forbidden, unauth}, denied(schema.PermManageMembers)},
		{"act_as_members", "POST", "/v1/hooks", `{}`, [5]want{ok(400), ok(400), forbidden, forbidden, unauth}, denied(schema.PermActAsMembers)},
		{"manage_channels", "POST", "/v0/channels", `{}`, [5]want{ok(400), ok(400), forbidden, forbidden, unauth}, denied(schema.PermManageChannels)},
		{"change_server_settings", "PUT", "/v1/join/settings", `{}`, [5]want{ok(400), denied(schema.PermChangeServerSettings), forbidden, forbidden, unauth}, denied(schema.PermChangeServerSettings)},
		{"read_audit_log", "GET", "/v1/audit?limit=0", "", [5]want{ok(400), denied(schema.PermReadAuditLog), forbidden, forbidden, unauth}, denied(schema.PermReadAuditLog)},
		{"fixed operator right", "GET", "/v1/reports", "", [5]want{ok(200), ok(200), forbidden, forbidden, unauth}, ok(200)},
		{"manager class, a channel it is not in", "PUT", fmt.Sprintf("/v1/channels/channel-a/members/%d", f.member2.ID), "", [5]want{ok(204), ok(204), notFound, forbidden, unauth}, notFound},
		// The class's own code, forbidden, now naming the permission (design
		// §7.4: one code per question, whichever check refused).
		{"operator-or-self", "GET", manifest, "", [5]want{ok(404), ok(404), forbidden, ok(404), unauth}, want{http.StatusForbidden, "forbidden", schema.PermManageMembers}},
	}
	callers := []string{f.rootTok, f.opTok, f.memberTok, f.agTok, ""}
	names := []string{"owner", "operator", "member", "agent", "unauthenticated"}
	check := func(t *testing.T, tok string, method, path, body string, w want) {
		t.Helper()
		rec := f.do(t, method, path, tok, body)
		if rec.Code != w.status {
			t.Fatalf("status = %d %s, want %d", rec.Code, rec.Body, w.status)
		}
		if w.code == "" {
			return
		}
		e := decodeBody[schema.Error](t, rec)
		if e.Code != w.code || e.Permission != w.perm {
			t.Fatalf("answer = %s, want code %q permission %q", rec.Body, w.code, w.perm)
		}
	}
	for _, rt := range routes {
		for i, tok := range callers {
			t.Run(rt.name+"/"+names[i], func(t *testing.T) {
				check(t, tok, rt.method, rt.path, rt.body, rt.want[i])
			})
		}
	}
	// The manifest exists only for agents; the operator's own was looked up
	// above as an agent's. The restricted operator is told which permission
	// it lacks, and the refusal is audited with it.
	f.cut(t)
	for _, rt := range routes {
		t.Run(rt.name+"/restricted operator", func(t *testing.T) {
			before := countAction(f.audit(t), "access_denied")
			check(t, f.opTok, rt.method, rt.path, rt.body, rt.restricted)
			if rt.restricted.perm == "" {
				return
			}
			if countAction(f.audit(t), "access_denied") <= before || !f.auditHas(t, f.op, "access_denied", "permission="+string(rt.restricted.perm)) {
				t.Errorf("the refusal was not audited with permission=%s", rt.restricted.perm)
			}
		})
	}
	// The uniform 404: a channel the restricted operator is not in answers as
	// one that never existed.
	in := f.do(t, "PUT", fmt.Sprintf("/v1/channels/channel-a/members/%d", f.member2.ID), f.opTok, "")
	ghost := f.do(t, "PUT", fmt.Sprintf("/v1/channels/no-such-channel/members/%d", f.member2.ID), f.opTok, "")
	if in.Code != ghost.Code || in.Body.String() != ghost.Body.String() {
		t.Errorf("channel-a = %d %s, a channel that never existed = %d %s: they must be alike", in.Code, in.Body, ghost.Code, ghost.Body)
	}
}

// Turning act_as_members off for operators answers what they issued to
// others, from the audit trail (design §7.5, O13): live items only, and
// never what an operator issued to itself.
func TestPermissionsPutIssuedToOthers(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	ctx := context.Background()
	opActor := fmt.Sprintf("principal:%d", f.op.ID)
	toOther, _, err := f.srv.store.CreateCredential(ctx, opActor, f.member.ID, "made by operator-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.srv.store.CreateCredential(ctx, opActor, f.op.ID, "its own", nil); err != nil {
		t.Fatal(err)
	}
	revoked, _, err := f.srv.store.CreateCredential(ctx, opActor, f.member2.ID, "revoked since", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.RevokeCredential(ctx, "system", revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.srv.store.CreateCredential(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.member2.ID, "the owner's", nil); err != nil {
		t.Fatal(err)
	}

	res := f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f),
		Changes: []schema.PermissionChangeV1{{Permission: schema.PermActAsMembers, Role: schema.PermissionRoleOperator, Granted: false}}})
	if res.code != http.StatusOK {
		t.Fatalf("PUT = %d %s", res.code, res.body)
	}
	got := res.document(t).IssuedToOthers
	if len(got) != 1 || got[0].Kind != schema.IssuedCredential || got[0].ID != toOther.ID || got[0].PrincipalID != f.member.ID || got[0].IssuedBy != f.op.ID {
		t.Fatalf("issued_to_others = %+v, want only operator-1's credential for member-1 (%d)", got, toOther.ID)
	}
	// Any other change answers without the list.
	res = f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f),
		Changes: []schema.PermissionChangeV1{{Permission: schema.PermCreatePrivateChannels, Role: schema.PermissionRoleMember, Granted: false}}})
	if res.code != http.StatusOK || res.document(t).IssuedToOthers != nil {
		t.Fatalf("another change answered issued_to_others: %d %s", res.code, res.body)
	}
}

// TestPermissionsChangedSignal: a document change owes every human socket
// one frame, an agent's none; a brake change owes its target's alone; many
// changes owe one.
func TestPermissionsChangedSignal(t *testing.T) {
	h := newEventsHub()
	ann := h.subscribeKind(1, 10, 100, true)
	annToo := h.subscribeKind(2, 10, 101, true)
	bob := h.subscribeKind(1, 20, 200, true)
	agent := h.subscribeKind(1, 30, 300, false)
	for _, s := range []*eventsSub{ann, annToo, bob, agent} {
		s.takePermissions()
	}
	h.notifyPermissionsChangedAll()
	h.notifyPermissionsChangedAll()
	for name, s := range map[string]*eventsSub{"ann": ann, "ann's second socket": annToo, "bob": bob} {
		if !s.takePermissions() {
			t.Errorf("%s was not owed the frame after a document change", name)
		}
		if s.takePermissions() {
			t.Errorf("%s was owed it twice: changes coalesce into one", name)
		}
	}
	if agent.takePermissions() {
		t.Error("an agent's socket was owed the frame")
	}
	h.notifyPermissionsChanged(20, 0)
	if !bob.takePermissions() {
		t.Error("the brake's target was not owed the frame")
	}
	if ann.takePermissions() || annToo.takePermissions() || agent.takePermissions() {
		t.Error("a brake change reached someone other than its target")
	}
}

// The routes owe the frame: the PUT to everyone, the invite brake to its
// target alone, and a refused PUT to no one.
func TestPermissionsChangedSignalFromRoutes(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	member := f.srv.events.subscribeKind(0, f.member.ID, 1, true)
	member2 := f.srv.events.subscribeKind(0, f.member2.ID, 2, true)
	member.takePermissions()
	member2.takePermissions()

	res := f.putPermissions(t, f.opTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f),
		Changes: []schema.PermissionChangeV1{{Permission: schema.PermCreatePrivateChannels, Role: schema.PermissionRoleMember, Granted: false}}})
	if res.code != http.StatusForbidden {
		t.Fatalf("operator's PUT = %d %s, want refused", res.code, res.body)
	}
	if member.takePermissions() {
		t.Error("a refused PUT owed the frame")
	}
	res = f.putPermissions(t, f.rootTok, schema.PutPermissionsRequestV1{Revision: revisionOf(t, f),
		Changes: []schema.PermissionChangeV1{{Permission: schema.PermCreatePrivateChannels, Role: schema.PermissionRoleMember, Granted: false}}})
	if res.code != http.StatusOK {
		t.Fatalf("owner's PUT = %d %s", res.code, res.body)
	}
	if !member.takePermissions() || !member2.takePermissions() {
		t.Error("a document change did not reach every person")
	}
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/invite-permission", f.member.ID), f.rootTok, `{"may_invite":true}`); rec.Code != http.StatusNoContent {
		t.Fatalf("invite permission = %d %s", rec.Code, rec.Body)
	}
	if !member.takePermissions() {
		t.Error("the brake's target was not told")
	}
	if member2.takePermissions() {
		t.Error("someone else was told of member-1's brake")
	}
}

// permissionGates are the functions that must ask a permission for it to be
// enforced (design §7.2 and §7.3): each names it. vacuousGates are the
// routes that will gate a permission enforced only because nothing uses it
// yet; while none is in the table, the claim holds.
var permissionGates = map[schema.Permission][][2]string{
	schema.PermManageMembers: {
		{"routes.go", "Server.routeTable"},
		{"invites.go", "Server.handleListInvites"},
		{"sessions.go", "Server.handleListSessions"},
		{"principals.go", "Server.handleRenamePrincipal"},
		{"disable.go", "Server.handleRevokeAllCredentials"},
		{"store/invites.go", "makerRightsTx"},
		{"store/invites.go", "Store.CloseInvite"},
	},
	schema.PermActAsMembers: {
		{"routes.go", "Server.routeTable"},
		{"store/sign_in_links.go", "makerMayActAsTx"},
	},
	schema.PermManageChannels: {
		{"routes.go", "Server.routeTable"},
		{"auth.go", "Server.guard"},
		{"members.go", "Server.handleListChannelMembers"},
		{"nets.go", "Server.handleListNets"},
		{"store/members.go", "memberActorStandingTx"},
		{"store/channels_rename.go", "channelRenameStandingTx"},
		{"store/invites.go", "makerRightsTx"},
		{"store/guest_policy.go", "admittingOpenChannelsTx"},
		{"store/guest_policy.go", "checkOpenChannelsWriterTx"},
		// The name filters of A3a (other_hooks, other_open_channels,
		// other_channels), through channelSightFor.
		{"auth.go", "Server.channelSightFor"},
		{"hooks.go", "Server.handleListHooks"},
		{"join.go", "Server.joinSettingsResponse"},
		{"invites.go", "Server.handleListInvites"},
		// A hook or a manifest into a channel the caller is not in (the
		// security review of #1015).
		{"hooks.go", "Server.handleCreateHook"},
		{"manifests.go", "Server.handlePutManifest"},
		// Channel settings, and the bound on a creator who does not hold
		// it (issue #981).
		{"channels.go", "Server.handleChannelSettings"},
		{"store/queries.go", "Store.ChannelSettings"},
		{"store/members.go", "channelCreatorStandingTx"},
	},
	// Member channel creation (issue #981): the store decides the
	// permission for the visibility asked, under its write lock.
	schema.PermCreatePublicChannels:  {{"store/members.go", "channelCreatePermission"}},
	schema.PermCreatePrivateChannels: {{"store/members.go", "channelCreatePermission"}},
	schema.PermChangeServerSettings:  {{"routes.go", "Server.routeTable"}},
	schema.PermReadAuditLog:          {{"routes.go", "Server.routeTable"}},
	// A4's (#1016): listed so that adding one of these to
	// enforcedPermissions fails until its gates read it.
	schema.PermMakeMemberLinks: {
		{"store/invites.go", "makerRightsTx"},
		{"store/invites.go", "makerStillAllowedTx"},
	},
	schema.PermUseVoice: {
		{"voice.go", "Server.handleVoiceSession"},
		{"voice_presence.go", "Server.voiceEntitlement"},
	},
	schema.PermShareScreen: {
		{"voice_share.go", "Server.handleVoiceShareStart"},
		{"voice_share.go", "voicePoller.shareEntitled"},
	},
}

var vacuousGates = map[schema.Permission][]string{
	schema.PermSeeMemberDirectory: {"GET /v1/directory/members"},
}

// TestEnforcedPermissionsAreHonest is the safety net's own check (SF-3):
// every permission in enforcedPermissions is asked by each of its gates, or
// gates nothing yet; a permission whose gates do not all ask it is not
// listed.
func TestEnforcedPermissionsAreHonest(t *testing.T) {
	names := map[schema.Permission]string{}
	for name, p := range permissionConsts {
		names[p] = name
	}
	// fns maps "file fn" to the permission constants each names.
	fns := map[string]map[string]bool{}
	authoritySources(t, func(_ *token.FileSet, rel string, file *ast.File) {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			key := rel + " " + funcName(fd)
			named := map[string]bool{}
			ast.Inspect(fd, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "schema" {
						named[sel.Sel.Name] = true
					}
					// channelSightFor is manage_channels asked for the
					// channel names an answer may show.
					if sel.Sel.Name == "channelSightFor" {
						named["PermManageChannels"] = true
					}
				}
				return true
			})
			fns[key] = named
		}
	})
	f := newAuthFixture(t, AuthRequired)
	table := map[string]bool{}
	for _, rt := range f.srv.routes {
		table[rt.pattern] = true
	}
	for _, p := range enforcedPermissions {
		if routes, vacuous := vacuousGates[p]; vacuous {
			for _, r := range routes {
				if table[r] {
					t.Errorf("%s is enforced vacuously, but %s now exists: make it ask can(%s), list it in permissionGates, and drop the vacuous claim", p, r, p)
				}
			}
			continue
		}
		gates, ok := permissionGates[p]
		if !ok {
			t.Errorf("%s is in enforcedPermissions with no gates listed in permissionGates", p)
			continue
		}
		for _, g := range gates {
			named, found := fns[g[0]+" "+g[1]]
			switch {
			case !found:
				t.Errorf("%s's gate %s %s is not in the code: correct permissionGates", p, g[0], g[1])
			case !named[names[p]]:
				t.Errorf("%s is enforced, but its gate %s %s does not ask it: gate it, or take %s out of enforcedPermissions", p, g[0], g[1], p)
			}
		}
	}
}

// A PUT that withholds share_screen ends a live share before it answers
// (the security review of #1061, S1): the voice passes read the person's
// brake, not the document, so nothing else would. share_screen joins the
// safety net in A4 (#1016); this test lists it for its own run, as A4 will.
func TestPermissionsPutEndsAWithheldShare(t *testing.T) {
	saved := enforcedPermissions
	enforcedPermissions = append(slices.Clone(saved), schema.PermShareScreen)
	t.Cleanup(func() { enforcedPermissions = saved })

	f, room := newShareFixture(t)
	f.mustClaim(t, "ann")
	if !f.granted(t, room, "ann") {
		t.Fatal("ann's share was not granted")
	}
	doc, err := f.srv.store.Permissions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(schema.PutPermissionsRequestV1{Revision: doc.Revision, Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleOperator, Granted: false},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// The PUT's own stop path runs, once, for ann's one room: nothing else
	// (a poller pass) may be what ended the share. The seam is set before
	// the request and read after it, and nothing runs a pass meanwhile.
	var stops []int
	f.srv.voice.endSharesSeam = func(rooms int) { stops = append(stops, rooms) }
	if rec := f.do(t, "PUT", "/v1/permissions", f.rootTok, string(body)); rec.Code != http.StatusOK {
		t.Fatalf("the owner's PUT = %d %s", rec.Code, rec.Body)
	}
	if !slices.Equal(stops, []int{1}) {
		t.Errorf("the PUT ran the stop path %v times (rooms each), want once for ann's one room", stops)
	}
	// Read at the moment the PUT has answered.
	if f.granted(t, room, "ann") {
		t.Error("the screen is still granted when the PUT answered")
	}
	if holder := f.shareState(room); holder != 0 {
		t.Errorf("the poller still holds a share for %d", holder)
	}
}

// Without manage_channels a hook or a manifest reaches only channels the
// caller is in, and a channel that never existed is answered alike, so
// neither route tells private channels' names or ids (the security review
// of #1015). Under the defaults operators hold manage_channels and nothing
// changes.
func TestHookAndManifestAreNotChannelOracles(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	hook := func(channel string) *httpResult {
		t.Helper()
		body, err := json.Marshal(schema.CreateHookRequest{Channel: channel, Principal: f.agent.ID})
		if err != nil {
			t.Fatal(err)
		}
		rec := f.do(t, "POST", "/v1/hooks", f.opTok, string(body))
		return &httpResult{code: rec.Code, body: rec.Body.Bytes()}
	}
	manifest := func(channelID int64) *httpResult {
		t.Helper()
		body, err := json.Marshal(schema.PutAgentManifestRequestV1{DisplayName: "agent-1", Tier: schema.AgentTierA, Capabilities: schema.Capabilities(),
			Channels: []schema.ChannelGrant{{ChannelID: channelID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead}}}})
		if err != nil {
			t.Fatal(err)
		}
		rec := f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/manifest", f.agent.ID), f.opTok, string(body))
		return &httpResult{code: rec.Code, body: rec.Body.Bytes()}
	}
	// The defaults: channel-a, which operator-1 is not in, is reachable.
	if res := hook("channel-a"); res.code != http.StatusCreated {
		t.Fatalf("a hook into channel-a under the defaults = %d %s", res.code, res.body)
	}
	if res := hook("no-such-channel"); res.code != http.StatusBadRequest || res.errorCode(t) != "channel_not_found" {
		t.Fatalf("a hook into no channel under the defaults = %d %s", res.code, res.body)
	}

	f.withhold(t, schema.PermManageChannels)
	private, ghost := hook("channel-a"), hook("no-such-channel")
	if private.code != http.StatusForbidden || private.errorCode(t) != schema.ErrorCodePermissionDenied || !strings.Contains(string(private.body), `"permission":"manage_channels"`) {
		t.Fatalf("a hook into channel-a without manage_channels = %d %s, want 403 permission_denied manage_channels", private.code, private.body)
	}
	if ghost.code != private.code || string(ghost.body) != string(private.body) {
		t.Errorf("a hook into no channel = %d %s, into channel-a = %d %s: they must be alike", ghost.code, ghost.body, private.code, private.body)
	}
	if res := hook("channel-b"); res.code != http.StatusCreated {
		t.Errorf("a hook into its own channel = %d %s, want 201", res.code, res.body)
	}

	private, ghost = manifest(f.chanA.ID), manifest(987654321)
	if private.code != http.StatusForbidden || private.errorCode(t) != schema.ErrorCodePermissionDenied {
		t.Fatalf("a manifest granting channel-a without manage_channels = %d %s, want 403 permission_denied", private.code, private.body)
	}
	if ghost.code != private.code || string(ghost.body) != string(private.body) {
		t.Errorf("a manifest granting no channel = %d %s, granting channel-a = %d %s: they must be alike", ghost.code, ghost.body, private.code, private.body)
	}
	if res := manifest(f.chanB.ID); res.code != http.StatusOK && res.code != http.StatusCreated {
		t.Errorf("a manifest granting its own channel = %d %s", res.code, res.body)
	}
}

// The joining settings' open_channels are not an oracle on private names
// (the review of #1014): to a writer without manage_channels a name that is
// no channel gets the answer the owner's private channel gets, byte for
// byte.
func TestJoinSettingsNameOracleClosed(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	f.setOperatorCells(t, true, schema.PermChangeServerSettings)
	f.cut(t)
	put := func(tok, name string) *httpResult {
		t.Helper()
		body, err := json.Marshal(schema.PutJoinSettingsRequestV1{OpenChannels: &[]string{"channel-b", name}})
		if err != nil {
			t.Fatal(err)
		}
		rec := f.do(t, "PUT", "/v1/join/settings", tok, string(body))
		return &httpResult{code: rec.Code, body: rec.Body.Bytes()}
	}
	private, ghost := put(f.opTok, "channel-a"), put(f.opTok, "no-such-channel")
	if private.code != http.StatusForbidden || private.errorCode(t) != schema.ErrorCodePermissionDenied {
		t.Fatalf("the owner's private channel = %d %s, want 403 permission_denied", private.code, private.body)
	}
	if ghost.code != private.code || string(ghost.body) != string(private.body) {
		t.Errorf("a channel that never existed = %d %s, the owner's private one = %d %s: they must be alike", ghost.code, ghost.body, private.code, private.body)
	}
	var e schema.Error
	if err := json.Unmarshal(private.body, &e); err != nil || e.Permission != schema.PermManageChannels {
		t.Errorf("the refusal names %q, want manage_channels", e.Permission)
	}
	// The owner is told the name is no channel, as before.
	if res := put(f.rootTok, "no-such-channel"); res.code != http.StatusBadRequest {
		t.Errorf("the owner naming no channel = %d %s, want 400", res.code, res.body)
	}
}

// Invite refusals name the permission they lack, to a caller whose role
// could hold it (design §7.4): the same codes as before.
func TestInviteRefusalsNameThePermission(t *testing.T) {
	f := newRestrictedFixture(t, Config{})
	f.cut(t)
	yes := true
	for _, c := range []struct {
		name string
		tok  string
		req  schema.CreateInviteRequestV1
		code string
		perm schema.Permission
	}{
		{"an open link without manage_members", f.opTok, schema.CreateInviteRequestV1{Kind: schema.InviteKindOpen, Channels: []string{"channel-b"}},
			schema.ErrorCodeInviteNotAllowed, schema.PermManageMembers},
		{"granting may_invite without manage_members", f.opTok, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-b"}, MayInvite: &yes},
			schema.ErrorCodeInviteGrantExceedsInviter, schema.PermManageMembers},
		{"a channel it is not in, without manage_channels", f.opTok, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-a"}},
			schema.ErrorCodeInviteGrantExceedsInviter, schema.PermManageChannels},
		// A member is never told of an operator permission; its own right
		// to make links (its brake) it is told.
		{"a member who may not invite", f.memberTok, schema.CreateInviteRequestV1{Kind: schema.InviteKindPersonal, Channels: []string{"channel-b"}},
			schema.ErrorCodeInviteNotAllowed, schema.PermMakeMemberLinks},
	} {
		t.Run(c.name, func(t *testing.T) {
			body, err := json.Marshal(c.req)
			if err != nil {
				t.Fatal(err)
			}
			rec := f.do(t, "POST", "/v1/invites", c.tok, string(body))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("invite = %d %s, want 403", rec.Code, rec.Body)
			}
			if e := decodeBody[schema.Error](t, rec); e.Code != c.code || e.Permission != c.perm {
				t.Errorf("refusal = %s, want code %q permission %q", rec.Body, c.code, c.perm)
			}
		})
	}
}
