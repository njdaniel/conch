package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The community member directory (issue #983, ADR-013 decision 1, design
// §3.5, §5): GET /v1/directory/members.

// directoryFixture is the administration fixture with a row of every kind
// the directory lists or leaves out.
type directoryFixture struct {
	*adminFixture
	// ghost is the authFixture's fourth human, in no channel.
	disabled, hidden, hiddenAgent, disabledAgent store.Principal
	converted, endedGuest                        store.Principal
	endedTok                                     string
}

func newDirectoryFixture(t *testing.T) *directoryFixture {
	t.Helper()
	ctx := context.Background()
	f := &directoryFixture{adminFixture: newAdminFixture(t)}
	st := f.srv.store
	mk := func(kind store.PrincipalKind, name string) store.Principal {
		t.Helper()
		p, err := st.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	f.disabled = mk(store.PrincipalHuman, "dir-disabled")
	if _, err := st.DisablePrincipal(ctx, "system", f.disabled.ID); err != nil {
		t.Fatal(err)
	}
	f.hidden = mk(store.PrincipalHuman, "dir-hidden")
	if _, err := st.HidePrincipal(ctx, "system", f.hidden.ID, store.HideOptions{}); err != nil {
		t.Fatal(err)
	}
	f.disabledAgent = mk(store.PrincipalAgent, "dir-disabled-agent")
	if _, err := st.DisablePrincipal(ctx, "system", f.disabledAgent.ID); err != nil {
		t.Fatal(err)
	}
	f.hiddenAgent = mk(store.PrincipalAgent, "dir-hidden-agent")
	if _, err := st.HidePrincipal(ctx, "system", f.hiddenAgent.ID, store.HideOptions{}); err != nil {
		t.Fatal(err)
	}

	// A guest that converted is a member like any other. Guests are seeded
	// through the store: no route starts a visit now (issue #1005).
	legacySettingRows(t, f.srv, map[string]string{"open_joining": string(schema.OpenJoiningAccount)}, f.general)
	conv := newGuest(t, f.srv, "", time.Hour, time.Hour, f.general.ID)
	converted, err := st.ConvertGuest(ctx, "system", store.GuestConversion{PrincipalID: conv.Principal.ID, Name: "dir-converted",
		Record: hashedRecord(t, f.srv, testJoinPassword), CredentialLabel: "member", CredentialLifetime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	f.converted = converted.Principal

	// An ended guest is a dead identity. Its token is replayed: the
	// credential the end revoked is made live again underneath the store.
	ended := newGuest(t, f.srv, "", time.Hour, time.Hour, f.general.ID)
	if _, err := st.EndGuestVisit(ctx, "system", ended.Principal.ID, schema.GuestEndOperator); err != nil {
		t.Fatal(err)
	}
	legacyExec(t, f.srv, "UPDATE credentials SET revoked_at = NULL WHERE principal_id = ?", ended.Principal.ID)
	f.endedGuest, f.endedTok = ended.Principal, ended.Token
	return f
}

func (f *directoryFixture) list(t *testing.T, token, query string) (*httptest.ResponseRecorder, schema.ServerMembersResponseV1) {
	t.Helper()
	rec := f.do(t, "GET", "/v1/directory/members"+query, token, "")
	var page schema.ServerMembersResponseV1
	if rec.Code == http.StatusOK {
		page = decodeBody[schema.ServerMembersResponseV1](t, rec)
		if err := page.Validate(); err != nil {
			t.Fatalf("the page is not well-formed: %v", err)
		}
	}
	return rec, page
}

func directoryNames(page schema.ServerMembersResponseV1) []string {
	out := make([]string, 0, len(page.Members))
	for _, m := range page.Members {
		out = append(out, m.Name)
	}
	return out
}

// TestDirectoryViewers: who may ask. A member, an operator and an owner get
// the list; a guest (active, or ended with a replayed token) gets 403
// guest_not_allowed, an agent 403, and nobody 401.
func TestDirectoryViewers(t *testing.T) {
	f := newDirectoryFixture(t)
	for _, tc := range []struct {
		who    string
		token  string
		status int
		code   string
	}{
		{"member", f.aliceTok, http.StatusOK, ""},
		{"manager", f.mgrTok, http.StatusOK, ""},
		{"operator", f.op2Tok, http.StatusOK, ""},
		{"owner", f.rootTok, http.StatusOK, ""},
		{"second owner", f.own2Tok, http.StatusOK, ""},
		{"active guest", f.guestTok, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
		// An ended guest cannot authenticate at all, even with its credential
		// row made live again: the middleware answers 401 before the route.
		{"ended guest, token replayed", f.endedTok, http.StatusUnauthorized, "unauthenticated"},
		{"agent", f.botTok, http.StatusForbidden, "forbidden"},
		{"unauthenticated", "", http.StatusUnauthorized, "unauthenticated"},
	} {
		t.Run(tc.who, func(t *testing.T) {
			rec := f.do(t, "GET", "/v1/directory/members", tc.token, "")
			if tc.code == "" {
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.status, rec.Body)
				}
				return
			}
			assertErrorBody(t, rec, tc.status, tc.code)
		})
	}
}

// An ended guest the middleware let through (it never does) is still refused
// by the route itself: guest_not_allowed, as an active guest is.
func TestDirectoryRefusesAnEndedGuestCaller(t *testing.T) {
	f := newDirectoryFixture(t)
	ended := f.endedGuest
	ended.GuestState = schema.GuestStateEnded
	req := httptest.NewRequest("GET", "/v1/directory/members", nil).WithContext(withCaller(context.Background(), ended))
	req.Pattern = "GET /v1/directory/members"
	rec := httptest.NewRecorder()
	f.srv.directoryRoute(http.HandlerFunc(f.srv.handleListDirectoryMembers)).ServeHTTP(rec, req)
	assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed)
}

// TestDirectoryRows: exactly the section 1.1 members and the enabled,
// non-hidden agents are listed, ordered by id, whoever asks; scope is
// everyone.
func TestDirectoryRows(t *testing.T) {
	f := newDirectoryFixture(t)
	// The authFixture's "ghost", a human in no channel, is listed too: it is
	// a member (design section 1.1) whatever channels it is in.
	listed := []store.Principal{f.root, f.alice, f.bot, f.op2, f.own2, f.mgr, f.cand, f.outsider, f.converted}
	const ghosts = 1
	notListed := map[string]store.Principal{
		"active guest": f.guest, "ended guest": f.endedGuest,
		"disabled human": f.disabled, "hidden human": f.hidden,
		"disabled agent": f.disabledAgent, "hidden agent": f.hiddenAgent,
	}
	for _, viewer := range []struct{ who, token string }{
		{"member", f.aliceTok}, {"operator", f.op2Tok}, {"owner", f.rootTok},
	} {
		t.Run(viewer.who, func(t *testing.T) {
			rec, page := f.list(t, viewer.token, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body %s", rec.Code, rec.Body)
			}
			if page.Scope != schema.DirectoryScopeEveryone {
				t.Errorf("scope = %q, want everyone", page.Scope)
			}
			if page.After != 0 {
				t.Errorf("after = %d, want none: the whole list is one page", page.After)
			}
			byID := map[int64]schema.ServerMemberV1{}
			var ids []int64
			for _, m := range page.Members {
				byID[m.ID] = m
				ids = append(ids, m.ID)
			}
			if !slices.IsSorted(ids) {
				t.Errorf("ids %v are not ascending", ids)
			}
			for _, p := range listed {
				m, ok := byID[p.ID]
				if !ok {
					t.Errorf("%s (%d) is not listed", p.Name, p.ID)
					continue
				}
				if m.Name != p.Name || m.Kind != schema.PrincipalKind(p.Kind) {
					t.Errorf("%s listed as %+v", p.Name, m)
				}
				if p.Kind == store.PrincipalHuman && m.Role != schema.PrincipalRole(p.Role) {
					t.Errorf("%s role = %q, want %q", p.Name, m.Role, p.Role)
				}
				if p.Kind == store.PrincipalAgent && m.Role != "" {
					t.Errorf("agent %s carries role %q", p.Name, m.Role)
				}
			}
			if len(page.Members) != len(listed)+ghosts || !slices.Contains(directoryNames(page), "ghost") {
				t.Errorf("listed %v, want exactly the %d people above and ghost", directoryNames(page), len(listed))
			}
			for what, p := range notListed {
				if _, ok := byID[p.ID]; ok {
					t.Errorf("%s (%s) is listed", what, p.Name)
				}
			}
		})
	}
}

// TestDirectoryOperatorFields: voice_allowed, may_invite, invited_by and
// owner are filled for a viewer holding manage_members and absent from the
// bytes for everyone else; an agent carries none of them.
func TestDirectoryOperatorFields(t *testing.T) {
	f := newDirectoryFixture(t)
	for _, tc := range []struct {
		who      string
		token    string
		operator bool
	}{
		{"member", f.aliceTok, false},
		{"manager", f.mgrTok, false},
		{"operator", f.op2Tok, true},
		{"owner", f.rootTok, true},
	} {
		t.Run(tc.who, func(t *testing.T) {
			rec, page := f.list(t, tc.token, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			for _, field := range []string{`"voice_allowed"`, `"may_invite"`, `"owner"`} {
				if has := strings.Contains(rec.Body.String(), field); has != tc.operator {
					t.Errorf("%s present = %v, want %v", field, has, tc.operator)
				}
			}
			for _, m := range page.Members {
				filled := m.VoiceAllowed != nil && m.MayInvite != nil
				switch {
				case m.Kind == schema.PrincipalAgent:
					if filled || m.Owner || m.InvitedBy != nil {
						t.Errorf("agent %s carries a person's fields: %+v", m.Name, m)
					}
				case filled != tc.operator:
					t.Errorf("%s: operator fields present = %v, want %v", m.Name, filled, tc.operator)
				case m.Owner != (tc.operator && (m.ID == f.root.ID || m.ID == f.own2.ID)):
					t.Errorf("%s: owner = %v", m.Name, m.Owner)
				}
			}
		})
	}

	t.Run("an operator without manage_members is a member here", func(t *testing.T) {
		doc, err := f.srv.store.Permissions(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.srv.store.PutPermissions(context.Background(), fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, schema.PutPermissionsRequestV1{
			Revision: doc.Revision,
			Changes: []schema.PermissionChangeV1{
				{Permission: schema.PermActAsMembers, Role: schema.PermissionRoleOperator, Granted: false},
				{Permission: schema.PermManageMembers, Role: schema.PermissionRoleOperator, Granted: false},
			},
		}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		rec, page := f.list(t, f.op2Tok, "")
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"voice_allowed"`) || len(page.Members) == 0 {
			t.Errorf("operator without manage_members: %d %s", rec.Code, rec.Body)
		}
		if _, page := f.list(t, f.rootTok, ""); page.Members[0].VoiceAllowed == nil {
			t.Error("the owner lost the operator fields")
		}
	})
}

// TestDirectoryPaging: ?limit=1 walks the whole list, one row a page, in id
// order; after is present only while more may follow and is the id of the
// last row returned.
func TestDirectoryPaging(t *testing.T) {
	f := newDirectoryFixture(t)
	_, whole := f.list(t, f.aliceTok, "")
	var got []int64
	after := int64(0)
	pages := 0
	for {
		q := "?limit=1"
		if after != 0 {
			q += fmt.Sprintf("&after=%d", after)
		}
		rec, page := f.list(t, f.aliceTok, q)
		if rec.Code != http.StatusOK || len(page.Members) != 1 {
			t.Fatalf("page %d: %d %s", pages, rec.Code, rec.Body)
		}
		got = append(got, page.Members[0].ID)
		pages++
		if page.After == 0 {
			break
		}
		if page.After != page.Members[0].ID {
			t.Fatalf("after = %d, want the returned row %d", page.After, page.Members[0].ID)
		}
		after = page.After
		if pages > len(whole.Members) {
			t.Fatal("paging does not end")
		}
	}
	var want []int64
	for _, m := range whole.Members {
		want = append(want, m.ID)
	}
	if !slices.Equal(got, want) {
		t.Errorf("paged ids %v, want %v", got, want)
	}

	// A page that ends exactly at the end of the list carries no cursor.
	rec, page := f.list(t, f.aliceTok, fmt.Sprintf("?limit=%d", len(whole.Members)))
	if rec.Code != http.StatusOK || page.After != 0 || len(page.Members) != len(whole.Members) {
		t.Errorf("a full page: %d after=%d rows=%d", rec.Code, page.After, len(page.Members))
	}

	for _, q := range []string{"?limit=0", "?limit=-1", "?limit=x", fmt.Sprintf("?limit=%d", schema.MaxListPrincipalsLimit+1), "?after=-1", "?after=x"} {
		assertErrorBody(t, f.do(t, "GET", "/v1/directory/members"+q, f.aliceTok, ""), http.StatusBadRequest, "invalid_request")
	}
	if _, page := f.list(t, f.aliceTok, fmt.Sprintf("?after=%d", 1<<40)); len(page.Members) != 0 || page.After != 0 {
		t.Errorf("past the end: %+v", page)
	}
}

// TestDirectoryHonoursSeeMemberDirectory: the owner switches the directory
// off for members (ADR-014 design section 9). A member then sees only
// themself, with scope shared_channels, as the narrowest answer the may-know
// predicate gives (I14a, #988, widens it to the member's channels); an
// operator still sees everyone.
func TestDirectoryHonoursSeeMemberDirectory(t *testing.T) {
	f := newDirectoryFixture(t)
	ctx := context.Background()
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, schema.PutPermissionsRequestV1{
		Revision: doc.Revision,
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermSeeMemberDirectory, Role: schema.PermissionRoleMember, Granted: false}},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Without the directory a member knows themself and the members of their
	// channels (design §13.2, clauses 1 and 2), and nobody else.
	rec, page := f.list(t, f.aliceTok, "")
	if rec.Code != http.StatusOK || page.Scope != schema.DirectoryScopeSharedChannels {
		t.Fatalf("member with the directory off: %d scope=%q", rec.Code, page.Scope)
	}
	mine, err := f.srv.store.ListChannelsForPrincipal(ctx, f.alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	sharesWithAlice := func(id int64) bool {
		for _, c := range mine {
			if in, err := f.srv.store.IsChannelMember(ctx, c.ID, id); err == nil && in {
				return true
			}
		}
		return false
	}
	self := false
	for _, m := range page.Members {
		if m.ID == f.alice.ID {
			self = true
			continue
		}
		if !sharesWithAlice(m.ID) {
			t.Errorf("member with the directory off lists %s, who shares no channel with her", m.Name)
		}
	}
	if !self || slices.Contains(directoryNames(page), "ghost") {
		t.Errorf("member with the directory off: %v, want herself and her channel-mates, not ghost", directoryNames(page))
	}
	if _, page := f.list(t, f.op2Tok, ""); page.Scope != schema.DirectoryScopeEveryone || len(page.Members) < 10 {
		t.Errorf("operator with the directory off for members: scope=%q %v", page.Scope, directoryNames(page))
	}
}
