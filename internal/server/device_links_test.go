package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The device link tests (issue #712, design §6.2) run on the recovery
// fixture: alice a member with a password, wren a member who predates
// passwords, root the operator, bot an agent. A device link is made on a
// session that holds a recent-password proof, so most tests begin with
// provenSession.

const deviceLinksPath = "/v1/sign-in-links/mine"

func ownDeviceLinkPath(id int64) string { return fmt.Sprintf("%s/%d", deviceLinksPath, id) }

// provenSession signs name in with password and proves it again on that
// session, returning its credential id and token.
func (f *recoveryFixture) provenSession(t *testing.T, name, password string) (int64, string) {
	t.Helper()
	cred, tok := f.signIn(t, name, password)
	if rec := f.reauth(t, tok, password); rec.Code != http.StatusOK {
		t.Fatalf("reauth %s = %d %s", name, rec.Code, rec.Body)
	}
	return cred, tok
}

func (f *recoveryFixture) createDeviceLink(t *testing.T, token string, req schema.CreateDeviceLinkRequestV1) *httptest.ResponseRecorder {
	t.Helper()
	return f.send(t, "POST", deviceLinksPath, token, inviteJSON(t, req), "")
}

// deviceLink makes a device link on token and fails the test unless it is
// made.
func (f *recoveryFixture) deviceLink(t *testing.T, token, label string) schema.CreateSignInLinkResponseV1 {
	t.Helper()
	rec := f.createDeviceLink(t, token, schema.CreateDeviceLinkRequestV1{Label: label})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create device link = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.CreateSignInLinkResponseV1](t, rec)
}

// ownDeviceLinks lists token's device links and fails the test unless the
// list answers.
func (f *recoveryFixture) ownDeviceLinks(t *testing.T, token string) []schema.SignInLinkV1 {
	t.Helper()
	rec := f.send(t, "GET", deviceLinksPath, token, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list device links = %d %s", rec.Code, rec.Body)
	}
	return decodeBody[schema.ListOwnSignInLinksResponseV1](t, rec).SignInLinks
}

// sameLink reports whether two links have the same wire form.
func sameLink(t *testing.T, a, b schema.SignInLinkV1) bool {
	t.Helper()
	return inviteJSON(t, a) == inviteJSON(t, b)
}

// credentialsLabelled counts id's credentials, live or not, with label.
func (f *recoveryFixture) credentialsLabelled(t *testing.T, id int64, label string) int {
	t.Helper()
	list, err := f.srv.store.ListCredentials(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range list {
		if c.Label == label {
			n++
		}
	}
	return n
}

// The whole chain over REST: a member proves their password on one session,
// makes a labelled device link, sees it listed; whoever opens it is told
// whose account it is and nothing signs them in until they redeem it; the
// redeemed credential is the member's, carries the label, is among their
// sign-ins and can be revoked; the link then works no more and is no longer
// listed.
func TestDeviceLinkEndToEnd(t *testing.T) {
	f := newRecoveryFixture(t)
	credA, tokA := f.provenSession(t, "alice", alicePassword)
	made := f.srv.now()
	rec := f.createDeviceLink(t, tokA, schema.CreateDeviceLinkRequestV1{Label: "Phone"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	link := decodeBody[schema.CreateSignInLinkResponseV1](t, rec)
	l := link.SignInLink
	if schema.ValidateSignInLinkCode(link.Code) != nil || l.PrincipalID != f.alice.ID || l.CreatedBy != f.alice.ID ||
		l.Purpose != schema.SignInLinkPurposeDevice || l.Label != "Phone" {
		t.Fatalf("link = %+v", link)
	}
	if want := made.Add(schema.DefaultRestrictedLinkLifetime).Truncate(time.Millisecond); !l.ExpiresAt.Time().Equal(want) {
		t.Errorf("expires = %v, want ten minutes on (%v)", l.ExpiresAt.Time(), want)
	}
	if got := f.ownDeviceLinks(t, tokA); len(got) != 1 || !sameLink(t, got[0], l) {
		t.Errorf("listed = %+v, want the one link", got)
	}

	before := f.credentialCount(t, f.alice.ID)
	rec = f.previewSignInLink(t, link.Code)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 || string(raw["name"]) != `"alice"` || string(raw["purpose"]) != `"device"` {
		t.Errorf("preview = %s, want the name and the purpose alone", rec.Body)
	}
	if n := f.credentialCount(t, f.alice.ID); n != before {
		t.Errorf("a preview issued a credential: %d, want %d", n, before)
	}

	rec = f.redeemSignInLink(t, link.Code)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	res := decodeBody[schema.RedeemSignInLinkResponseV1](t, rec)
	if res.Principal.ID != f.alice.ID || res.Credential.Label != "Phone" || !res.Credential.Renews {
		t.Fatalf("redeem = %+v, want a renewable credential for alice labelled Phone", res)
	}
	if who := decodeBody[schema.WhoAmIResponseV1](t, f.send(t, "GET", "/v1/whoami", res.Token, "", "")); who.ID != f.alice.ID || who.Role != schema.RoleMember {
		t.Errorf("whoami with the new credential = %+v, want alice", who)
	}
	// It is a sign-in session: the new device can prove the password too.
	if rec := f.reauth(t, res.Token, alicePassword); rec.Code != http.StatusOK {
		t.Errorf("reauth on the new device = %d %s", rec.Code, rec.Body)
	}

	// The member sees the labelled sign-in among theirs, and can end it.
	own := decodeBody[schema.ListOwnCredentialsResponseV1](t, f.send(t, "GET", "/v1/credentials/mine", tokA, "", ""))
	var listed bool
	for _, c := range own.Credentials {
		if c.ID == res.Credential.ID {
			listed = c.Label == "Phone" && !c.Current
		}
	}
	if !listed {
		t.Errorf("own credentials = %+v, want the Phone sign-in", own.Credentials)
	}

	// Used: one answer, and no longer listed.
	assertErrorBody(t, f.redeemSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	assertErrorBody(t, f.previewSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	if got := f.ownDeviceLinks(t, tokA); len(got) != 0 {
		t.Errorf("listed after use = %+v, want none", got)
	}
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/credentials/mine/%d", res.Credential.ID), tokA, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke the Phone sign-in = %d %s", rec.Code, rec.Body)
	}
	if code := f.status(t, res.Token); code != http.StatusUnauthorized {
		t.Errorf("whoami after its revoke = %d, want 401", code)
	}

	issued := auditRows(t, f.authFixture, store.AuditDeviceLinkIssued)
	if len(issued) != 1 || issued[0].Actor != fmt.Sprintf("principal:%d", f.alice.ID) || issued[0].Subject != fmt.Sprintf("principal:%d", f.alice.ID) ||
		!strings.Contains(issued[0].Detail, fmt.Sprintf("sign_in_link=%d credential=%d ", l.ID, credA)) {
		t.Errorf("device_link_issued = %+v, want one by and about alice naming the link and the session", issued)
	}
	if n := countAudit(t, f.authFixture, store.AuditSignInLinkCreated); n != 0 {
		t.Errorf("sign_in_link_created rows = %d, want none: a device link is audited as one", n)
	}
	if rows := auditRows(t, f.authFixture, store.AuditSignInLinkRedeemed); len(rows) != 1 || rows[0].Actor != fmt.Sprintf("sign_in_link:%d", l.ID) {
		t.Errorf("sign_in_link_redeemed = %+v", rows)
	}
}

// A device link with no label signs in a credential named for the link.
func TestDeviceLinkWithoutALabel(t *testing.T) {
	f := newRecoveryFixture(t)
	_, tok := f.provenSession(t, "alice", alicePassword)
	link := f.deviceLink(t, tok, "")
	if link.SignInLink.Label != "" {
		t.Errorf("label = %q, want none", link.SignInLink.Label)
	}
	rec := f.redeemSignInLink(t, link.Code)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	if got, want := decodeBody[schema.RedeemSignInLinkResponseV1](t, rec).Credential.Label, fmt.Sprintf("device link %d", link.SignInLink.ID); got != want {
		t.Errorf("credential label = %q, want %q", got, want)
	}
}

// Who may make a device link, with what, and what everyone else is told. A
// refusal writes no link, audits no device_link_issued and carries no code.
func TestCreateDeviceLinkRefusals(t *testing.T) {
	expiresIn := func(f *recoveryFixture, d time.Duration) *schema.Timestamp {
		ts := schema.NewTimestamp(f.srv.now().Add(d))
		return &ts
	}
	proven := func(t *testing.T, f *recoveryFixture) string {
		_, tok := f.provenSession(t, "alice", alicePassword)
		return tok
	}
	tests := []struct {
		name string
		// token is the credential the request is sent with.
		token  func(t *testing.T, f *recoveryFixture) string
		body   func(f *recoveryFixture) string
		status int
		code   string
	}{
		{"no proof on the session", func(t *testing.T, f *recoveryFixture) string {
			_, tok := f.signIn(t, "alice", alicePassword)
			return tok
		}, nil, http.StatusForbidden, schema.ErrorCodeReauthRequired},
		{"a proof on another session", func(t *testing.T, f *recoveryFixture) string {
			f.provenSession(t, "alice", alicePassword)
			_, tok := f.signIn(t, "alice", alicePassword)
			return tok
		}, nil, http.StatusForbidden, schema.ErrorCodeReauthRequired},
		{"a proof that ran out", func(t *testing.T, f *recoveryFixture) string {
			tok := proven(t, f)
			f.advance(schema.RecentAuthLifetimeSeconds * time.Second)
			return tok
		}, nil, http.StatusForbidden, schema.ErrorCodeReauthRequired},
		// The fixture's own credential for alice: an operator issued it for
		// a script, and a proof is never kept on one.
		{"a script credential", func(_ *testing.T, f *recoveryFixture) string { return f.aliceTok }, nil,
			http.StatusForbidden, schema.ErrorCodeReauthRequired},
		{"a member who predates passwords", func(_ *testing.T, f *recoveryFixture) string { return f.wrenTok }, nil,
			http.StatusForbidden, schema.ErrorCodeReauthRequired},
		{"an operator", func(_ *testing.T, f *recoveryFixture) string { return f.rootTok }, nil, http.StatusForbidden, "forbidden"},
		{"an agent", func(_ *testing.T, f *recoveryFixture) string { return f.botTok }, nil, http.StatusForbidden, schema.ErrorCodeHumanRequired},
		{"a guest", func(t *testing.T, f *recoveryFixture) string {
			return newGuest(t, f.srv, "", time.Hour, 2*time.Hour).Token
		}, nil, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
		{"no credential", func(*testing.T, *recoveryFixture) string { return "" }, nil, http.StatusUnauthorized, "unauthenticated"},
		{"longer than an hour", proven, func(f *recoveryFixture) string {
			return inviteJSON(t, schema.CreateDeviceLinkRequestV1{ExpiresAt: expiresIn(f, schema.MaxRestrictedLinkLifetime+time.Minute)})
		}, http.StatusBadRequest, "invalid_request"},
		{"in the past", proven, func(f *recoveryFixture) string {
			return inviteJSON(t, schema.CreateDeviceLinkRequestV1{ExpiresAt: expiresIn(f, -time.Minute)})
		}, http.StatusBadRequest, "invalid_request"},
		{"a blank label", proven, func(*recoveryFixture) string { return `{"label":"   "}` }, http.StatusBadRequest, "invalid_request"},
		{"a label too long", proven, func(*recoveryFixture) string {
			return inviteJSON(t, schema.CreateDeviceLinkRequestV1{Label: strings.Repeat("x", schema.MaxCredentialLabelLength+1)})
		}, http.StatusBadRequest, "invalid_request"},
		{"a link for someone else", proven, func(f *recoveryFixture) string {
			return fmt.Sprintf(`{"principal_id":%d}`, f.wren.ID)
		}, http.StatusBadRequest, "invalid_request"},
		{"not JSON", proven, func(*recoveryFixture) string { return "soon" }, http.StatusBadRequest, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			tok := tt.token(t, f)
			body := "{}"
			if tt.body != nil {
				body = tt.body(f)
			}
			rec := f.send(t, "POST", deviceLinksPath, tok, body, "")
			assertErrorBody(t, rec, tt.status, tt.code)
			if strings.Contains(rec.Body.String(), schema.SignInLinkCodePrefix) {
				t.Errorf("a refusal carries a code: %s", rec.Body)
			}
			if n := countAudit(t, f.authFixture, store.AuditDeviceLinkIssued); n != 0 {
				t.Errorf("a refusal audited %d device_link_issued rows", n)
			}
			if n := f.queryString(t, "SELECT COUNT(*) FROM sign_in_links WHERE purpose = 'device'"); n != "0" {
				t.Errorf("a refusal wrote %s device links", n)
			}
		})
	}
}

// The lifetime is ten minutes unless asked for, and may be asked up to an
// hour.
func TestDeviceLinkLifetimes(t *testing.T) {
	tests := []struct {
		name string
		ask  time.Duration // 0: none asked for
		want time.Duration
	}{
		{"by default", 0, schema.DefaultRestrictedLinkLifetime},
		{"half an hour", 30 * time.Minute, 30 * time.Minute},
		{"the longest", schema.MaxRestrictedLinkLifetime, schema.MaxRestrictedLinkLifetime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			_, tok := f.provenSession(t, "alice", alicePassword)
			now := f.srv.now()
			var req schema.CreateDeviceLinkRequestV1
			if tt.ask != 0 {
				ts := schema.NewTimestamp(now.Add(tt.ask))
				req.ExpiresAt = &ts
			}
			rec := f.createDeviceLink(t, tok, req)
			if rec.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", rec.Code, rec.Body)
			}
			link := decodeBody[schema.CreateSignInLinkResponseV1](t, rec)
			if want := now.Add(tt.want).Truncate(time.Millisecond); !link.SignInLink.ExpiresAt.Time().Equal(want) {
				t.Errorf("expires = %v, want %v", link.SignInLink.ExpiresAt.Time(), want)
			}
			// It works until then, and not from then on.
			f.advance(tt.want)
			assertErrorBody(t, f.redeemSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
		})
	}
}

// At most three open device links: a fourth is refused until one is closed,
// used or expired, and none of those count.
func TestDeviceLinkCap(t *testing.T) {
	f := newRecoveryFixture(t)
	_, tok := f.provenSession(t, "alice", alicePassword)
	var links []schema.CreateSignInLinkResponseV1
	for i := range schema.MaxOpenDeviceLinks {
		links = append(links, f.deviceLink(t, tok, fmt.Sprintf("device %d", i)))
	}
	refused := func(why string) {
		t.Helper()
		rec := f.createDeviceLink(t, tok, schema.CreateDeviceLinkRequestV1{})
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), schema.ErrorCodeTooManyDeviceLinks) {
			t.Fatalf("%s: create = %d %s, want 409 too_many_device_links", why, rec.Code, rec.Body)
		}
	}
	refused("three open")

	// Closing one frees its place.
	if rec := f.send(t, "DELETE", ownDeviceLinkPath(links[0].SignInLink.ID), tok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("close = %d %s", rec.Code, rec.Body)
	}
	links[0] = f.deviceLink(t, tok, "after a close")
	refused("three open again")

	// Using one frees its place.
	if rec := f.redeemSignInLink(t, links[1].Code); rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d %s", rec.Code, rec.Body)
	}
	links[1] = f.deviceLink(t, tok, "after a use")
	refused("three open once more")

	// Expired ones hold no place either; the proof has run out by then too.
	f.advance(schema.DefaultRestrictedLinkLifetime)
	if rec := f.reauth(t, tok, alicePassword); rec.Code != http.StatusOK {
		t.Fatalf("reauth = %d %s", rec.Code, rec.Body)
	}
	for i := range schema.MaxOpenDeviceLinks {
		f.deviceLink(t, tok, fmt.Sprintf("later %d", i))
	}
	refused("three open after the others expired")
	if n := len(f.ownDeviceLinks(t, tok)); n != schema.MaxOpenDeviceLinks {
		t.Errorf("listed = %d, want %d", n, schema.MaxOpenDeviceLinks)
	}
}

// The cap holds under concurrency: of many asks at once, exactly three make a
// link.
func TestDeviceLinkCapRace(t *testing.T) {
	f := newRecoveryFixture(t)
	cred, _ := f.provenSession(t, "alice", alicePassword)
	const asks = 12
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		made    int
		capped  int
		failure []error
	)
	for range asks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.srv.store.CreateDeviceLink(context.Background(), fmt.Sprintf("principal:%d", f.alice.ID), f.alice.ID, cred,
				schema.CreateDeviceLinkRequestV1{}, f.srv.now())
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				made++
			case errors.Is(err, store.ErrTooManyDeviceLinks):
				capped++
			default:
				failure = append(failure, err)
			}
		}()
	}
	wg.Wait()
	if len(failure) > 0 || made != schema.MaxOpenDeviceLinks || capped != asks-schema.MaxOpenDeviceLinks {
		t.Fatalf("made %d, capped %d, failed %v; want %d made", made, capped, failure, schema.MaxOpenDeviceLinks)
	}
	if n := f.queryString(t, "SELECT COUNT(*) FROM sign_in_links WHERE purpose = 'device'"); n != fmt.Sprint(schema.MaxOpenDeviceLinks) {
		t.Errorf("device links = %s, want %d", n, schema.MaxOpenDeviceLinks)
	}
}

// Two devices redeem the one link at once: one gets a credential, every
// other the one invalid answer, and only one credential is made.
func TestDeviceLinkConcurrentRedeem(t *testing.T) {
	f := newRecoveryFixture(t)
	_, tok := f.provenSession(t, "alice", alicePassword)
	link := f.deviceLink(t, tok, "Tablet")
	const tries = 8
	codes := make([]int, tries)
	var wg sync.WaitGroup
	for i := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = f.redeemSignInLink(t, link.Code).Code
		}()
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			won++
		case http.StatusNotFound:
		default:
			t.Errorf("a redeem answered %d", c)
		}
	}
	if won != 1 {
		t.Errorf("redeems that won = %d, want 1 (%v)", won, codes)
	}
	if n := f.credentialsLabelled(t, f.alice.ID, "Tablet"); n != 1 {
		t.Errorf("credentials labelled Tablet = %d, want 1", n)
	}
}

// Every change that ends the member's authentication, every way to close a
// link, and every way the member stops being one who may be signed in by
// link ends an open device link: a code from before cannot be previewed or
// redeemed, and issues nothing. In particular a stolen device link cannot
// defeat a password reset or a sign-out everywhere, and cannot become an
// operator's session.
func TestDeviceLinkInvalidation(t *testing.T) {
	tests := []struct {
		name string
		// end ends the link; tokA is the session that made it, tokB another
		// of alice's.
		end func(t *testing.T, f *recoveryFixture, link schema.CreateSignInLinkResponseV1, tokA, tokB string)
		// closed is the reason= of the sign_in_link_closed row the end
		// writes, if it writes one.
		closed string
	}{
		{"a password change", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, tokA, _ string) {
			if rec := f.change(t, tokA, alicePassword, newAlicePassword); rec.Code != http.StatusOK {
				t.Fatalf("change = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseAuthChanged},
		{"a password reset", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, _, _ string) {
			if rec := f.reset(t, f.link(t, schema.SignInLinkPurposeReset, f.alice.ID).Code, newAlicePassword); rec.Code != http.StatusOK {
				t.Fatalf("reset = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseAuthChanged},
		{"a sign-out everywhere", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, _, tokB string) {
			if rec := f.send(t, "POST", f.revokeAllPath(f.alice.ID), tokB, "", ""); rec.Code != http.StatusOK {
				t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseAuthChanged},
		{"an operator's revoke-all", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, _, _ string) {
			if rec := f.send(t, "POST", f.revokeAllPath(f.alice.ID), f.rootTok, "", ""); rec.Code != http.StatusOK {
				t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseAuthChanged},
		{"a disable", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, _, _ string) {
			if rec := f.send(t, "POST", f.disablePath(f.alice.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("disable = %d %s", rec.Code, rec.Body)
			}
		}, ""},
		{"a hide", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, _, _ string) {
			if _, err := f.srv.store.HidePrincipal(context.Background(), "system", f.alice.ID, store.HideOptions{}); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"signing out of the session that made it", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, tokA, _ string) {
			if rec := f.send(t, "DELETE", "/v1/credentials/current", tokA, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseSignedOut},
		{"signing out of another session", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, _, tokB string) {
			if rec := f.send(t, "DELETE", "/v1/credentials/current", tokB, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseSignedOut},
		// A bearer stolen inside the proof window made the link; the member
		// revokes that sign-in from another device, or an operator does.
		{"the member revoking the session that made it by id", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, tokA, tokB string) {
			path := fmt.Sprintf("/v1/credentials/mine/%d", f.credentialID(t, tokA))
			if rec := f.send(t, "DELETE", path, tokB, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("revoke by id = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseCredentialRevoked},
		{"an operator revoking the session that made it by id", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, tokA, _ string) {
			path := fmt.Sprintf("/v1/credentials/%d", f.credentialID(t, tokA))
			if rec := f.send(t, "DELETE", path, f.rootTok, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("operator revoke by id = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseCredentialRevoked},
		{"the member closing it", func(t *testing.T, f *recoveryFixture, link schema.CreateSignInLinkResponseV1, _, tokB string) {
			if rec := f.send(t, "DELETE", ownDeviceLinkPath(link.SignInLink.ID), tokB, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("close = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseSelf},
		{"an operator closing it", func(t *testing.T, f *recoveryFixture, link schema.CreateSignInLinkResponseV1, _, _ string) {
			if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/sign-in-links/%d", link.SignInLink.ID), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("operator close = %d %s", rec.Code, rec.Body)
			}
		}, store.SignInLinkCloseOperator},
		{"the member made an operator since", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, _, _ string) {
			f.exec(t, "UPDATE principals SET role = 'operator' WHERE id = ?", f.alice.ID)
		}, ""},
		{"every generation moved on, as a restore does", func(t *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, _, _ string) {
			f.exec(t, "UPDATE principals SET auth_generation = auth_generation + 1")
		}, ""},
		{"its expiry", func(_ *testing.T, f *recoveryFixture, _ schema.CreateSignInLinkResponseV1, _, _ string) {
			f.advance(schema.DefaultRestrictedLinkLifetime)
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			_, tokA := f.provenSession(t, "alice", alicePassword)
			_, tokB := f.signIn(t, "alice", alicePassword)
			link := f.deviceLink(t, tokA, "Stolen")
			tt.end(t, f, link, tokA, tokB)

			assertErrorBody(t, f.previewSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
			assertErrorBody(t, f.redeemSignInLink(t, link.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
			if n := f.credentialsLabelled(t, f.alice.ID, "Stolen"); n != 0 {
				t.Errorf("an ended link issued %d credentials", n)
			}
			// A promotion closes nothing (the row stays, unusable, and an
			// operator is refused the listing route); every other end leaves
			// nothing listed.
			links, err := f.srv.store.ListOwnDeviceLinks(context.Background(), f.alice.ID, f.srv.now())
			if promoted := strings.Contains(tt.name, "operator since"); err != nil || (len(links) != 0) != promoted {
				t.Errorf("listed after %s = %+v, %v", tt.name, links, err)
			}
			if tt.closed != "" {
				want := fmt.Sprintf("sign_in_link=%d reason=%s", link.SignInLink.ID, tt.closed)
				var found bool
				for _, e := range auditRows(t, f.authFixture, store.AuditSignInLinkClosed) {
					found = found || e.Detail == want
				}
				if !found {
					t.Errorf("no sign_in_link_closed row %q", want)
				}
			}
		})
	}
}

// A single sign-out closes the member's own device links and nothing else:
// another member's stay, and so does a sign-in link an operator made for the
// member, which is the operator's support tool for a person who has lost
// their way in (design §6.1), not something the session opened.
func TestSignOutClosesOnlyOwnDeviceLinks(t *testing.T) {
	f := newRecoveryFixture(t)
	f.setPassword(t, f.wren.ID, wrenPassword)
	_, aliceTok := f.provenSession(t, "alice", alicePassword)
	_, wrenTok := f.provenSession(t, "wren", wrenPassword)
	alices := f.deviceLink(t, aliceTok, "Alice's phone")
	wrens := f.deviceLink(t, wrenTok, "Wren's phone")
	support := f.link(t, schema.SignInLinkPurposeSignIn, f.alice.ID)

	if rec := f.send(t, "DELETE", "/v1/credentials/current", aliceTok, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("sign out = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.redeemSignInLink(t, alices.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	for name, code := range map[string]string{"wren's device link": wrens.Code, "the operator's sign-in link": support.Code} {
		if rec := f.redeemSignInLink(t, code); rec.Code != http.StatusCreated {
			t.Errorf("%s after alice signed out = %d %s, want it to work", name, rec.Code, rec.Body)
		}
	}
	// One sign_in_link_closed row, for alice's device link only.
	rows := auditRows(t, f.authFixture, store.AuditSignInLinkClosed)
	if len(rows) != 1 || rows[0].Detail != fmt.Sprintf("sign_in_link=%d reason=%s", alices.SignInLink.ID, store.SignInLinkCloseSignedOut) ||
		rows[0].Subject != fmt.Sprintf("principal:%d", f.alice.ID) {
		t.Errorf("sign_in_link_closed = %+v, want alice's device link alone", rows)
	}
	// Ending another of the member's sign-ins by id closes their device
	// links too (design §6.2: revocation), whichever session made them: the
	// revoked one may be a stolen bearer that made them.
	credC, tokC := f.signIn(t, "alice", alicePassword)
	_, tokD := f.provenSession(t, "alice", alicePassword)
	gone := f.deviceLink(t, tokD, "Gone")
	if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/credentials/mine/%d", credC), tokD, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke another sign-in = %d %s", rec.Code, rec.Body)
	}
	if code := f.status(t, tokC); code != http.StatusUnauthorized {
		t.Errorf("the revoked sign-in still works: %d", code)
	}
	if got := f.ownDeviceLinks(t, tokD); len(got) != 0 {
		t.Errorf("listed = %+v, want none", got)
	}
	assertErrorBody(t, f.redeemSignInLink(t, gone.Code), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	want := fmt.Sprintf("sign_in_link=%d reason=%s", gone.SignInLink.ID, store.SignInLinkCloseCredentialRevoked)
	var found bool
	for _, e := range auditRows(t, f.authFixture, store.AuditSignInLinkClosed) {
		found = found || e.Detail == want
	}
	if !found {
		t.Errorf("no sign_in_link_closed row %q", want)
	}
}

// credentialID is the id of the credential whose token is tok.
func (f *recoveryFixture) credentialID(t *testing.T, tok string) int64 {
	t.Helper()
	// A credential's hash is the same SHA-256 hex a link code's is.
	var id int64
	s := f.queryString(t, fmt.Sprintf("SELECT id FROM credentials WHERE token_hash = '%s'", store.HashSignInLinkCode(tok)))
	if _, err := fmt.Sscan(s, &id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A device link is good for signing in and nothing else, and is found only
// as one: its code is no reset or enrollment proof, a link of another
// purpose is no device link of the member's, and a device-purpose row made
// by anyone but its own member (which no route makes) signs nobody in,
// whoever it is for.
func TestDeviceLinkCrossPurpose(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	_, tok := f.provenSession(t, "alice", alicePassword)
	device := f.deviceLink(t, tok, "Laptop")

	assertErrorBody(t, f.reset(t, device.Code, newAlicePassword), http.StatusNotFound, schema.ErrorCodeSignInLinkInvalid)
	if err := f.srv.store.CheckEnrollmentProof(ctx, store.HashSignInLinkCode(device.Code), f.alice.ID, f.srv.now()); !errors.Is(err, store.ErrSignInLinkInvalid) {
		t.Errorf("a device link as an enrollment proof = %v, want ErrSignInLinkInvalid", err)
	}

	// The operator's links for alice are not hers to list or close.
	reset := f.link(t, schema.SignInLinkPurposeReset, f.alice.ID)
	signIn := f.link(t, schema.SignInLinkPurposeSignIn, f.alice.ID)
	if got := f.ownDeviceLinks(t, tok); len(got) != 1 || got[0].ID != device.SignInLink.ID {
		t.Errorf("listed = %+v, want the device link alone", got)
	}
	for _, id := range []int64{reset.SignInLink.ID, signIn.SignInLink.ID} {
		assertErrorBody(t, f.send(t, "DELETE", ownDeviceLinkPath(id), tok, "", ""), http.StatusNotFound, schema.ErrorCodeSignInLinkNotFound)
	}
	// An operator cannot make a device link through the operator route.
	rec := f.send(t, "POST", fmt.Sprintf("/v1/principals/%d/sign-in-links", f.alice.ID), f.rootTok,
		inviteJSON(t, schema.CreateSignInLinkRequestV1{Purpose: schema.SignInLinkPurposeDevice}), "")
	assertErrorBody(t, rec, http.StatusBadRequest, "invalid_request")

	// None of the wrong uses spent it.
	if rec := f.redeemSignInLink(t, device.Code); rec.Code != http.StatusCreated {
		t.Fatalf("redeem after the wrong uses = %d %s", rec.Code, rec.Body)
	}

	// Rows no route makes: a device link made by the operator for alice,
	// and one the operator made for themselves.
	for name, row := range map[string]struct{ principal, creator int64 }{
		"made by someone else": {f.alice.ID, f.root.ID},
		"for an operator":      {f.root.ID, f.root.ID},
	} {
		code, err := store.NewSignInLinkCode()
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, `INSERT INTO sign_in_links (code_hash, principal_id, created_by, created_at, expires_at, purpose)
			VALUES (?, ?, ?, ?, ?, 'device')`, store.HashSignInLinkCode(code), row.principal, row.creator,
			f.srv.now().UnixMilli(), f.srv.now().Add(time.Hour).UnixMilli())
		before := f.credentialCount(t, row.principal)
		if rec := f.redeemSignInLink(t, code); rec.Code != http.StatusNotFound {
			t.Errorf("%s: redeem = %d %s, want 404", name, rec.Code, rec.Body)
		}
		if n := f.credentialCount(t, row.principal); n != before {
			t.Errorf("%s: a credential was issued", name)
		}
	}
}

// Closing: one's own open link is closed and audited once; an id that is
// not a device link of the caller's is the one 404, whatever it is.
func TestCloseOwnDeviceLink(t *testing.T) {
	f := newRecoveryFixture(t)
	f.setPassword(t, f.wren.ID, wrenPassword)
	_, tok := f.provenSession(t, "alice", alicePassword)
	_, wrenTok := f.provenSession(t, "wren", wrenPassword)
	mine := f.deviceLink(t, tok, "Mine")
	wrens := f.deviceLink(t, wrenTok, "Wren's")

	tests := []struct {
		name   string
		path   string
		status int
		code   string
		// closes is the number of sign_in_link_closed rows after it.
		closes int
	}{
		{"her own", ownDeviceLinkPath(mine.SignInLink.ID), http.StatusNoContent, "", 1},
		{"her own again", ownDeviceLinkPath(mine.SignInLink.ID), http.StatusNoContent, "", 1},
		{"another member's", ownDeviceLinkPath(wrens.SignInLink.ID), http.StatusNotFound, schema.ErrorCodeSignInLinkNotFound, 1},
		{"one never issued", ownDeviceLinkPath(9999), http.StatusNotFound, schema.ErrorCodeSignInLinkNotFound, 1},
		{"not an id", deviceLinksPath + "/first", http.StatusBadRequest, "invalid_request", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.send(t, "DELETE", tt.path, tok, "", "")
			if tt.status == http.StatusNoContent {
				if rec.Code != http.StatusNoContent {
					t.Fatalf("close = %d %s", rec.Code, rec.Body)
				}
			} else {
				assertErrorBody(t, rec, tt.status, tt.code)
			}
			if n := countAudit(t, f.authFixture, store.AuditSignInLinkClosed); n != tt.closes {
				t.Errorf("sign_in_link_closed rows = %d, want %d", n, tt.closes)
			}
		})
	}
	if rec := f.redeemSignInLink(t, wrens.Code); rec.Code != http.StatusCreated {
		t.Errorf("wren's link after alice's tries = %d, want it to work", rec.Code)
	}
}

// The list: the caller's open device links, newest first, an empty list
// as [], and nobody else's.
func TestListOwnDeviceLinks(t *testing.T) {
	f := newRecoveryFixture(t)
	f.setPassword(t, f.wren.ID, wrenPassword)
	_, tok := f.provenSession(t, "alice", alicePassword)
	_, wrenTok := f.provenSession(t, "wren", wrenPassword)

	rec := f.send(t, "GET", deviceLinksPath, tok, "", "")
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"sign_in_links":[]}` {
		t.Fatalf("empty list = %d %s", rec.Code, rec.Body)
	}
	first := f.deviceLink(t, tok, "First")
	f.advance(time.Second)
	second := f.deviceLink(t, tok, "Second")
	f.deviceLink(t, wrenTok, "Wren's")

	rec = f.send(t, "GET", deviceLinksPath, tok, "", "")
	if strings.Contains(rec.Body.String(), schema.SignInLinkCodePrefix) || strings.Contains(rec.Body.String(), "Wren") {
		t.Errorf("the list holds a code or another's link: %s", rec.Body)
	}
	got := decodeBody[schema.ListOwnSignInLinksResponseV1](t, rec).SignInLinks
	if len(got) != 2 || !sameLink(t, got[0], second.SignInLink) || !sameLink(t, got[1], first.SignInLink) {
		t.Errorf("listed = %+v, want second then first", got)
	}
}

// With authentication off there is no account: every device link route
// answers accounts_require_auth.
func TestDeviceLinksAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	for _, c := range []struct{ method, path, body string }{
		{"POST", deviceLinksPath, "{}"},
		{"GET", deviceLinksPath, ""},
		{"DELETE", ownDeviceLinkPath(1), ""},
	} {
		assertErrorBody(t, f.do(t, c.method, c.path, "", c.body), http.StatusForbidden, schema.ErrorCodeAccountsRequireAuth)
	}
}

// Nothing the device link routes write or log holds a code, its hash or a
// token.
func TestDeviceLinkSecretsStayOut(t *testing.T) {
	logs := captureLogs(t)
	f := newRecoveryFixture(t)
	_, tok := f.provenSession(t, "alice", alicePassword)
	link := f.deviceLink(t, tok, "Phone")
	f.previewSignInLink(t, link.Code)
	rec := f.redeemSignInLink(t, link.Code)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem = %d", rec.Code)
	}
	newTok := decodeBody[schema.RedeemSignInLinkResponseV1](t, rec).Token
	f.redeemSignInLink(t, link.Code)
	closed := f.deviceLink(t, tok, "Closed")
	f.send(t, "DELETE", ownDeviceLinkPath(closed.SignInLink.ID), tok, "", "")
	f.send(t, "DELETE", "/v1/credentials/current", tok, "", "")

	secrets := []string{link.Code, closed.Code, store.HashSignInLinkCode(link.Code), store.HashSignInLinkCode(closed.Code), tok, newTok}
	for _, e := range f.audit(t) {
		for _, s := range secrets {
			if strings.Contains(e.Actor+e.Action+e.Subject+e.Detail, s) {
				t.Errorf("audit %s holds a secret", e.Action)
			}
		}
	}
	text := logs.buf.String()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Error("the log holds a secret")
		}
	}
}
