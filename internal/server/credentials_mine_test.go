package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Your sign-ins (issue #496): GET /v1/credentials/mine lists the caller's own
// live credentials, and DELETE /v1/credentials/mine/{id} ends any one of
// them, never someone else's.

const mineCredentialsPath = "/v1/credentials/mine"

func mineCredentialPath(id int64) string { return fmt.Sprintf("%s/%d", mineCredentialsPath, id) }

// listMine lists the caller's sign-ins and fails the test unless it is a 200
// whose body holds no token.
func (f *authFixture) listMine(t *testing.T, tok string) []schema.OwnCredentialV1 {
	t.Helper()
	rec := f.do(t, "GET", mineCredentialsPath, tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list own credentials = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), schema.CredentialTokenPrefix) {
		t.Fatalf("the list carries a token: %s", rec.Body)
	}
	return decodeBody[schema.ListOwnCredentialsResponseV1](t, rec).Credentials
}

// mineIDs returns the listed ids and the one marked current (0 for none).
func mineIDs(t *testing.T, list []schema.OwnCredentialV1) ([]int64, int64) {
	t.Helper()
	var ids []int64
	var current int64
	for _, c := range list {
		ids = append(ids, c.ID)
		if c.Current {
			if current != 0 {
				t.Errorf("two credentials marked current: %d and %d", current, c.ID)
			}
			current = c.ID
		}
	}
	return ids, current
}

// liveCredentialIDs are the ids of a principal's live credentials, newest
// first, read from the store.
func (f *authFixture) liveCredentialIDs(t *testing.T, principalID int64) []int64 {
	t.Helper()
	list, err := f.srv.store.ListLiveCredentials(context.Background(), principalID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestListOwnCredentials(t *testing.T) {
	tests := []struct {
		name string
		who  func(f *authFixture) (store.Principal, string)
	}{
		{"member", func(f *authFixture) (store.Principal, string) { return f.alice, f.aliceTok }},
		{"agent", func(f *authFixture) (store.Principal, string) { return f.bot, f.botTok }},
		{"operator", func(f *authFixture) (store.Principal, string) { return f.root, f.rootTok }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, AuthRequired)
			who, tok := tt.who(f)
			// The fixture's credential, a second one, one revoked and one
			// expired: the list holds the first two.
			first := f.liveCredentialIDs(t, who.ID)
			if len(first) != 1 {
				t.Fatalf("fixture credentials = %v", first)
			}
			secondID, secondTok := f.newCred(t, who.ID)
			revokedID, _ := f.newCred(t, who.ID)
			if err := f.srv.store.RevokeCredential(context.Background(), "system", revokedID); err != nil {
				t.Fatal(err)
			}
			exp := time.Now().Add(300 * time.Millisecond)
			if _, _, err := f.srv.store.CreateCredential(context.Background(), "system", who.ID, "brief", &exp); err != nil {
				t.Fatal(err)
			}
			// Everyone else's sign-ins, which must not be listed.
			for _, p := range []store.Principal{f.root, f.alice, f.bot} {
				if p.ID != who.ID {
					f.newCred(t, p.ID)
				}
			}
			time.Sleep(time.Until(exp) + 50*time.Millisecond)

			want := []int64{secondID, first[0]}
			ids, current := mineIDs(t, f.listMine(t, tok))
			if !slices.Equal(ids, want) || current != first[0] {
				t.Errorf("listed %v current %d, want %v current %d", ids, current, want, first[0])
			}
			// The same list from the other credential marks that one current.
			ids, current = mineIDs(t, f.listMine(t, secondTok))
			if !slices.Equal(ids, want) || current != secondID {
				t.Errorf("from the second: listed %v current %d, want %v current %d", ids, current, want, secondID)
			}
		})
	}
}

// Each listed sign-in has exactly the schema's keys: no token, no hash, no
// principal. The label says what made it, so a sign-in link that was used
// shows as one.
func TestListOwnCredentialsShape(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	rec := f.do(t, "GET", mineCredentialsPath, f.aliceTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	var doc struct {
		Credentials []map[string]json.RawMessage `json:"credentials"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil || len(doc.Credentials) != 1 {
		t.Fatalf("body = %s (%v)", rec.Body, err)
	}
	var keys []string
	for k := range doc.Credentials[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got, want := strings.Join(keys, ","), "created_at,current,id,label"; got != want {
		t.Errorf("keys = %s, want %s", got, want)
	}
}

// A sign-in link that was used shows in the person's list on their other
// devices, labelled with the link's id (the issue's second criterion).
func TestListOwnCredentialsShowsAUsedSignInLink(t *testing.T) {
	f := newInviteFixture(t, Config{})
	link := f.createSignInLink(t, f.alice.ID, schema.CreateSignInLinkRequestV1{})
	rec := f.redeemSignInLink(t, link.Code)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	redeemed := decodeBody[schema.RedeemSignInLinkResponseV1](t, rec)

	list := f.listMine(t, f.aliceTok)
	var found *schema.OwnCredentialV1
	for i := range list {
		if list[i].ID == redeemed.Credential.ID {
			found = &list[i]
		}
	}
	wantLabel := fmt.Sprintf("sign-in link %d", link.SignInLink.ID)
	if found == nil || found.Label != wantLabel || found.Current || !found.Renews {
		t.Fatalf("the link's sign-in in alice's list = %+v, want label %q, renewing, not current; list %+v", found, wantLabel, list)
	}
	// From the browser the link signed in, it is the current one.
	for _, c := range f.listMine(t, redeemed.Token) {
		if c.Current != (c.ID == redeemed.Credential.ID) {
			t.Errorf("from the link's browser, %d current = %v", c.ID, c.Current)
		}
	}
}

func TestRevokeOwnCredential(t *testing.T) {
	tests := []struct {
		name string
		// target returns the caller's token and the credential id to end.
		target func(t *testing.T, f *authFixture) (string, int64)
		want   int
		code   string
		// ends is the token that must stop working; "" when none may.
		ends func(f *authFixture) string
	}{
		{"a member ends another sign-in of theirs", func(t *testing.T, f *authFixture) (string, int64) {
			id, _ := f.newCred(t, f.alice.ID)
			return f.aliceTok, id
		}, http.StatusNoContent, "", nil},
		{"a member ends this one", func(t *testing.T, f *authFixture) (string, int64) {
			return f.aliceTok, f.liveCredentialIDs(t, f.alice.ID)[0]
		}, http.StatusNoContent, "", func(f *authFixture) string { return f.aliceTok }},
		{"an agent ends another of its own", func(t *testing.T, f *authFixture) (string, int64) {
			id, _ := f.newCred(t, f.bot.ID)
			return f.botTok, id
		}, http.StatusNoContent, "", nil},
		{"a member names another person's", func(t *testing.T, f *authFixture) (string, int64) {
			return f.aliceTok, f.liveCredentialIDs(t, f.bot.ID)[0]
		}, http.StatusNotFound, "credential_not_found", nil},
		{"a member names the operator's", func(t *testing.T, f *authFixture) (string, int64) {
			return f.aliceTok, f.liveCredentialIDs(t, f.root.ID)[0]
		}, http.StatusNotFound, "credential_not_found", nil},
		{"the operator names a member's", func(t *testing.T, f *authFixture) (string, int64) {
			return f.rootTok, f.liveCredentialIDs(t, f.alice.ID)[0]
		}, http.StatusNotFound, "credential_not_found", nil},
		{"one never issued", func(_ *testing.T, f *authFixture) (string, int64) { return f.aliceTok, 987654321 },
			http.StatusNotFound, "credential_not_found", nil},
		{"the last operator's last", func(t *testing.T, f *authFixture) (string, int64) {
			return f.rootTok, f.liveCredentialIDs(t, f.root.ID)[0]
		}, http.StatusConflict, "last_operator", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, AuthRequired)
			tok, id := tt.target(t, f)
			caller := map[string]store.Principal{f.aliceTok: f.alice, f.botTok: f.bot, f.rootTok: f.root}[tok]
			everyone := map[int64][]int64{}
			for _, p := range []store.Principal{f.root, f.alice, f.bot} {
				everyone[p.ID] = f.liveCredentialIDs(t, p.ID)
			}

			rec := f.do(t, "DELETE", mineCredentialPath(id), tok, "")
			if tt.code != "" {
				assertErrorBody(t, rec, tt.want, tt.code)
			} else if rec.Code != tt.want || rec.Body.Len() != 0 {
				t.Fatalf("end = %d %q, want %d and no body", rec.Code, rec.Body, tt.want)
			}

			rows := selfRevokedRows(t, f)
			if tt.want != http.StatusNoContent {
				// Nothing anyone holds changed, and nothing was audited.
				for p, before := range everyone {
					if after := f.liveCredentialIDs(t, p); !slices.Equal(before, after) {
						t.Errorf("principal %d credentials %v -> %v after a refusal", p, before, after)
					}
				}
				if len(rows) != 0 {
					t.Errorf("reason=self rows after a refusal = %+v", rows)
				}
				return
			}
			for p, before := range everyone {
				want := before
				if p == caller.ID {
					want = slices.DeleteFunc(slices.Clone(before), func(c int64) bool { return c == id })
				}
				if after := f.liveCredentialIDs(t, p); !slices.Equal(after, want) {
					t.Errorf("principal %d credentials %v -> %v, want %v", p, before, after, want)
				}
			}
			actor := fmt.Sprintf("principal:%d", caller.ID)
			if len(rows) != 1 || rows[0].Actor != actor || rows[0].Subject != actor || !strings.HasPrefix(rows[0].Detail, fmt.Sprintf("credential=%d ", id)) {
				t.Errorf("reason=self rows = %+v, want one by and about %s naming credential %d", rows, actor, id)
			}
			if tt.ends != nil {
				assertErrorBody(t, f.do(t, "GET", "/v1/whoami", tt.ends(f), ""), http.StatusUnauthorized, "unauthenticated")
			} else if r := f.do(t, "GET", "/v1/whoami", tok, ""); r.Code != http.StatusOK {
				t.Errorf("the caller's own credential after ending another = %d", r.Code)
			}
		})
	}
}

// Another person's credential id gets the very answer an id never issued
// does, byte for byte: the refusal tells the caller nothing.
func TestRevokeOwnCredentialAnotherIsAGhost(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	bots := f.liveCredentialIDs(t, f.bot.ID)
	theirs := f.do(t, "DELETE", mineCredentialPath(bots[0]), f.aliceTok, "")
	ghost := f.do(t, "DELETE", mineCredentialPath(987654321), f.aliceTok, "")
	if theirs.Code != ghost.Code || theirs.Body.String() != ghost.Body.String() {
		t.Errorf("another's = %d %s; a ghost = %d %s", theirs.Code, theirs.Body, ghost.Code, ghost.Body)
	}
	if r := f.do(t, "GET", "/v1/whoami", f.botTok, ""); r.Code != http.StatusOK {
		t.Errorf("bot after alice named its credential = %d", r.Code)
	}
}

func TestRevokeOwnCredentialBadID(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	for _, id := range []string{"0", "-1", "abc", "current"} {
		t.Run(id, func(t *testing.T) {
			assertErrorBody(t, f.do(t, "DELETE", mineCredentialsPath+"/"+id, f.aliceTok, ""), http.StatusBadRequest, "invalid_request")
		})
	}
}

// An operator's view of a person's credentials is unchanged: it still lists
// every credential, revoked ones included, and a sign-in the person ended
// shows there as revoked. A member still may not use it.
func TestOperatorCredentialViewUnchanged(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ended, _ := f.newCred(t, f.alice.ID)
	if rec := f.do(t, "DELETE", mineCredentialPath(ended), f.aliceTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("end = %d %s", rec.Code, rec.Body)
	}
	path := fmt.Sprintf("/v1/principals/%d/credentials", f.alice.ID)
	rec := f.do(t, "GET", path, f.rootTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("operator list = %d %s", rec.Code, rec.Body)
	}
	list := decodeBody[schema.ListCredentialsResponseV1](t, rec).Credentials
	if len(list) != 2 || list[0].ID != ended || list[0].RevokedAt == nil || list[1].RevokedAt != nil {
		t.Errorf("operator list = %+v, want the ended one revoked and the fixture's live", list)
	}
	assertErrorBody(t, f.do(t, "GET", path, f.aliceTok, ""), http.StatusForbidden, "forbidden")
}

// Ending another sign-in closes the sockets it holds, at the credential
// re-check, as an operator's revoke does; the caller's own stay open.
func TestRevokeOwnCredentialClosesItsSockets(t *testing.T) {
	f := newMemberFixture(t)
	f.srv.credRecheckInterval = 25 * time.Millisecond
	base := wsTestServer(t, f.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	otherID, otherTok := f.newCred(t, f.alice.ID)
	path := "/v1/ws?channel=alpha"
	other := startReader(ctx, dialAs(t, ctx, base, path, otherTok))
	here := startReader(ctx, dialAs(t, ctx, base, path, f.aliceTok))
	if rec := f.do(t, "DELETE", mineCredentialPath(otherID), f.aliceTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("end = %d %s", rec.Code, rec.Body)
	}
	closeErrContains(t, other.waitClosed(t), websocket.StatusPolicyViolation, reasonRevoked)
	id := f.postAs(t, f.aliceTok, "alpha", "still here")
	here.waitForID(t, id)
	select {
	case <-here.done:
		t.Errorf("the caller's own socket closed: %v", here.err)
	default:
	}
}
