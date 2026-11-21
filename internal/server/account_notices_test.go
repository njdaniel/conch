package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// Account notices (issue #2042, decision B on #2013): a password set or reset
// through a link another principal made is recorded for the member, shown to
// the member on a sign-in of their own, dismissed by them, and visible to no
// one else.

func (f *recoveryFixture) notices(t *testing.T, token string) []schema.AccountNoticeV1 {
	t.Helper()
	rec := f.do(t, "GET", "/v1/notices/mine", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/notices/mine = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.ListAccountNoticesResponseV1](t, rec).Notices
}

func (f *recoveryFixture) dismissNotice(t *testing.T, token string, id int64) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, "DELETE", fmt.Sprintf("/v1/notices/mine/%d", id), token, "")
}

// After each kind of link an operator made, the member's next password
// sign-in shows one notice naming that operator; the session the link was
// used on (made by another) shows none, and dismissing is the member's.
func TestAccountNoticeAfterEachLinkKind(t *testing.T) {
	tests := []struct {
		name string
		// run uses a link of the kind made by root and returns the member's
		// name and password afterward, and the token of a session that was
		// not signed in with the password.
		run func(t *testing.T, f *recoveryFixture) (name, password, otherTok string)
	}{
		{"enrolment link", func(t *testing.T, f *recoveryFixture) (string, string, string) {
			link := f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID)
			rec := f.enroll(t, f.wrenTok, wrenPassword, link.Code)
			if rec.Code != http.StatusOK {
				t.Fatalf("enroll = %d %s", rec.Code, rec.Body)
			}
			return "wren", wrenPassword, decodeBody[schema.PasswordUpdatedResponseV1](t, rec).Token
		}},
		{"reset link", func(t *testing.T, f *recoveryFixture) (string, string, string) {
			link := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID)
			if rec := f.reset(t, link.Code, newAlicePassword); rec.Code != http.StatusOK {
				t.Fatalf("reset = %d %s", rec.Code, rec.Body)
			}
			return "alice", newAlicePassword, ""
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			name, password, otherTok := tt.run(t, f)
			if otherTok != "" {
				if got := f.notices(t, otherTok); len(got) != 0 {
					t.Errorf("a session the link made = %+v, want no notices: it was not the member's own sign-in", got)
				}
			}
			_, tok := f.signIn(t, name, password)
			got := f.notices(t, tok)
			if len(got) != 1 {
				t.Fatalf("notices after a password sign-in = %+v, want one", got)
			}
			n := got[0]
			if n.Kind != schema.AccountNoticeKindPasswordSetByLink || n.ByName != f.root.Name || n.ID <= 0 || n.At.Time().IsZero() {
				t.Errorf("notice = %+v, want %s by %q with an id and a time", n, schema.AccountNoticeKindPasswordSetByLink, f.root.Name)
			}
			if err := n.Validate(); err != nil {
				t.Errorf("Validate: %v", err)
			}
			if rec := f.dismissNotice(t, tok, n.ID); rec.Code != http.StatusNoContent {
				t.Fatalf("dismiss = %d %s", rec.Code, rec.Body)
			}
			if got := f.notices(t, tok); len(got) != 0 {
				t.Errorf("notices after dismissing = %+v, want none", got)
			}
			// Dismissing twice is harmless; an id never issued is a 404.
			if rec := f.dismissNotice(t, tok, n.ID); rec.Code != http.StatusNoContent {
				t.Errorf("dismiss again = %d, want 204", rec.Code)
			}
			assertErrorBody(t, f.dismissNotice(t, tok, n.ID+1000), http.StatusNotFound, schema.ErrorCodeNoticeNotFound)
			// A later sign-in shows nothing either.
			_, again := f.signIn(t, name, password)
			if got := f.notices(t, again); len(got) != 0 {
				t.Errorf("notices on a later sign-in = %+v, want none", got)
			}
		})
	}
}

// A password the member changes themselves, and a sign-in with no link in
// it, leave no notice.
func TestAccountNoticeNotAfterOwnChange(t *testing.T) {
	f := newRecoveryFixture(t)
	_, tok := f.provenSession(t, "alice", alicePassword)
	if rec := f.change(t, tok, alicePassword, newAlicePassword); rec.Code != http.StatusOK {
		t.Fatalf("change = %d %s", rec.Code, rec.Body)
	}
	_, tok = f.signIn(t, "alice", newAlicePassword)
	if got := f.notices(t, tok); len(got) != 0 {
		t.Errorf("notices after the member's own change = %+v, want none", got)
	}
}

// Notices are the member's own: another member, an operator, an agent, a
// credential someone else made for the member, and a previewing
// administrator all see none, cannot dismiss, and learn nothing about whether
// the id exists; the notice is still the member's afterward.
func TestAccountNoticeVisibleOnlyToTheMember(t *testing.T) {
	f := newRecoveryFixture(t)
	link := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID)
	if rec := f.reset(t, link.Code, newAlicePassword); rec.Code != http.StatusOK {
		t.Fatalf("reset = %d %s", rec.Code, rec.Body)
	}
	_, aliceTok := f.signIn(t, "alice", newAlicePassword)
	mine := f.notices(t, aliceTok)
	if len(mine) != 1 {
		t.Fatalf("alice's notices = %+v, want one", mine)
	}
	id := mine[0].ID

	// wren signs in with a password of their own, so the only thing keeping
	// them from alice's notice is that it is not theirs.
	wrenLink := f.link(t, schema.SignInLinkPurposeEnrollment, f.wren.ID)
	if rec := f.enroll(t, f.wrenTok, wrenPassword, wrenLink.Code); rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body)
	}
	_, wrenTok := f.signIn(t, "wren", wrenPassword)
	// A credential an operator issued for alice (act_as_members): a bearer
	// of alice's identity, but not her own sign-in.
	_, actAs, err := f.srv.store.CreateCredential(t.Context(), fmt.Sprintf("principal:%d", f.root.ID), f.alice.ID, "act as", nil)
	if err != nil {
		t.Fatal(err)
	}

	others := []struct{ name, tok string }{
		{"another member", wrenTok},
		{"an operator", f.rootTok},
		{"an agent", f.botTok},
		{"a credential made for alice by another", actAs},
	}
	for _, o := range others {
		t.Run(o.name, func(t *testing.T) {
			for _, n := range f.notices(t, o.tok) {
				if n.ID == id || n.ByName == "" {
					t.Errorf("sees alice's notice %+v", n)
				}
			}
			if o.tok != wrenTok {
				if got := f.notices(t, o.tok); len(got) != 0 {
					t.Errorf("notices = %+v, want none", got)
				}
			}
			assertErrorBody(t, f.dismissNotice(t, o.tok, id), http.StatusNotFound, schema.ErrorCodeNoticeNotFound)
		})
	}
	t.Run("a previewing administrator", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/v1/notices/mine", strings.NewReader(""))
		req.Header.Set("Authorization", "Bearer "+f.rootTok)
		req.Header.Set("Conch-Preview-As", fmt.Sprint(f.alice.ID))
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK && len(decodeBody[schema.ListAccountNoticesResponseV1](t, rec).Notices) != 0 {
			t.Errorf("a preview read alice's notices: %s", rec.Body)
		}
	})
	t.Run("no credential", func(t *testing.T) {
		assertErrorBody(t, f.do(t, "GET", "/v1/notices/mine", "", ""), http.StatusUnauthorized, "unauthenticated")
	})
	if got := f.notices(t, aliceTok); len(got) != 1 || got[0].ID != id {
		t.Errorf("alice's notices after the others tried = %+v, want her one, undismissed", got)
	}
}
