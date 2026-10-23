package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The server's name as a reserved permanent name (issue #760).

// Every route that writes a permanent name refuses the server's name, the
// default one and a configured one, in any case and in look-alike spellings,
// with 409 principal_exists and a message that names the rule and not the
// name. A guest label of it is refused as before (#710). A person's own
// refused rename is not counted as a try: it says nothing of who is here.
func TestServerNameRefused(t *testing.T) {
	tests := []struct {
		server string
		cfg    Config
		// names are the server's name exactly, in another case and with a
		// look-alike character: a zero for an o, a Cyrillic о.
		names []string
		// free is a name that is not this server's, though it is another's.
		free string
	}{
		{"default", Config{}, []string{"conch", "Conch", "c0nch"}, "Harbour"},
		{"configured", Config{ServerName: "Harbour"}, []string{"Harbour", "HARBOUR", "Harbоur"}, "conch"},
	}
	for _, tt := range tests {
		for _, name := range tt.names {
			t.Run(fmt.Sprintf("%s/%+q", tt.server, name), func(t *testing.T) {
				f := newInviteFixture(t, tt.cfg)
				f.mustPutJoin(t, openBoth("red"))
				guest := mustJoined(t, f.joinGuest(t, "", ""))
				link := f.createInvite(t, f.rootTok, openLink("red"))
				principals := len(listPrincipals(t, f))
				routes := []struct {
					route string
					rec   *httptest.ResponseRecorder
				}{
					{"create", f.do(t, "POST", "/v0/principals", f.rootTok, fmt.Sprintf(`{"kind":"human","name":%s}`, jsonString(t, name)))},
					{"create agent", f.do(t, "POST", "/v0/principals", f.rootTok, fmt.Sprintf(`{"kind":"agent","name":%s}`, jsonString(t, name)))},
					{"own rename", f.do(t, "PUT", renamePath(f.alice.ID), f.aliceTok, renameBody(t, name))},
					{"operator rename", f.do(t, "PUT", renamePath(f.bot.ID), f.rootTok, renameBody(t, name))},
					{"v1 invite redemption", f.redeem(t, link.Code, name)},
					{"v2 invite redemption", f.redeemWithPassword(t, link.Code, name)},
					{"account from the bare address", f.joinAccount(t, name, "")},
					{"guest conversion", f.convert(t, guest.Token, name, "")},
				}
				for _, r := range routes {
					assertErrorBody(t, r.rec, http.StatusConflict, schema.ErrorCodePrincipalExists)
					msg := decodeBody[schema.Error](t, r.rec).Message
					if msg != serverNameReservedMessage {
						t.Errorf("%s: message = %q, want %q", r.route, msg, serverNameReservedMessage)
					}
					if strings.Contains(strings.ToLower(msg), strings.ToLower(name)) {
						t.Errorf("%s: message %q repeats the name", r.route, msg)
					}
				}
				if n := len(listPrincipals(t, f)); n != principals {
					t.Errorf("%d principals after the refusals, want %d", n, principals)
				}
				if n := countAction(f.audit(t), store.AuditPrincipalRenameRefused); n != 0 {
					t.Errorf("principal_rename_refused rows = %d, want 0 for the server's name", n)
				}
				assertErrorBody(t, f.joinGuest(t, name, ""), http.StatusBadRequest, "invalid_request")
				// The name that is not this server's is anyone's.
				if rec := f.do(t, "PUT", renamePath(f.alice.ID), f.aliceTok, renameBody(t, tt.free)); rec.Code != http.StatusOK {
					t.Errorf("rename to %q = %d %s, want 200", tt.free, rec.Code, rec.Body)
				}
			})
		}
	}
}

// listPrincipals is every principal the store holds.
func listPrincipals(t *testing.T, f *inviteFixture) []schema.PrincipalAdminV1 {
	t.Helper()
	all, err := f.srv.store.ListPrincipals(context.Background(), store.PrincipalFilter{Limit: 1000}, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// A principal that already holds the server's name when conchd starts, here
// after the name was changed to its own, is a warning and not a failure: one
// line at start, server_name_in_use in the joining settings, and the holder
// keeps its name, signs in and may rename itself away, after which the
// warning is gone.
func TestServerNameHeldAtStart(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, newTestDBPath(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	root, _, rootTok, err := st.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatal(err)
	}
	holder, err := st.CreatePrincipal(ctx, store.PrincipalHuman, "Harbour")
	if err != nil {
		t.Fatal(err)
	}
	_, holderTok, err := st.CreateCredential(ctx, "system", holder.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}

	logs := captureLogs(t)
	srv := New(Config{AuthMode: AuthRequired, Listen: "127.0.0.1:0", DataDir: t.TempDir(), ServerName: "harbour"}, st)
	f := &authFixture{srv: srv, root: root, rootTok: rootTok}
	if n := strings.Count(logs.buf.String(), "hold the server's name"); n != 1 {
		t.Errorf("start-up log = %q, want one warning about the server's name", logs.buf.String())
	}
	if want := fmt.Sprintf("principals=[%d]", holder.ID); !strings.Contains(logs.buf.String(), want) {
		t.Errorf("start-up log = %q, want it to name %s", logs.buf.String(), want)
	}
	if strings.Contains(logs.buf.String(), "Harbour") {
		t.Errorf("start-up log = %q, want ids only", logs.buf.String())
	}

	settings := func() []schema.JoinWarning {
		t.Helper()
		rec := f.do(t, "GET", "/v1/join/settings", f.rootTok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("join settings = %d %s", rec.Code, rec.Body)
		}
		return decodeBody[schema.JoinSettingsResponseV1](t, rec).Warnings
	}
	if got := settings(); !slices.Contains(got, schema.JoinWarningServerNameInUse) || slices.Contains(got, schema.JoinWarningReservedNamesInUse) {
		t.Errorf("warnings = %v, want %s and not %s", got, schema.JoinWarningServerNameInUse, schema.JoinWarningReservedNamesInUse)
	}
	// Not a block: guests can still be enabled.
	f.mustPutJoin(t, schema.PutJoinSettingsRequestV1{GuestsEnabled: boolPtr(true)})

	rec := f.do(t, "GET", "/v1/whoami", holderTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("the holder's whoami = %d %s", rec.Code, rec.Body)
	}
	if got := decodeBody[schema.WhoAmIResponseV1](t, rec); got.Name != "Harbour" {
		t.Errorf("the holder = %+v, want its name kept", got)
	}
	assertErrorBody(t, f.do(t, "PUT", renamePath(holder.ID), holderTok, renameBody(t, "HARBOUR")), http.StatusConflict, schema.ErrorCodePrincipalExists)
	if rec := f.do(t, "PUT", renamePath(holder.ID), holderTok, renameBody(t, "harbourmaster")); rec.Code != http.StatusOK {
		t.Fatalf("rename away = %d %s", rec.Code, rec.Body)
	}
	if got := settings(); slices.Contains(got, schema.JoinWarningServerNameInUse) {
		t.Errorf("warnings after the rename = %v, want no %s", got, schema.JoinWarningServerNameInUse)
	}

	// The next start finds nobody holding it, and says nothing.
	logs.buf.Reset()
	New(Config{AuthMode: AuthRequired, Listen: "127.0.0.1:0", DataDir: t.TempDir(), ServerName: "harbour"}, st)
	if strings.Contains(logs.buf.String(), "hold the server's name") {
		t.Errorf("start-up log = %q, want no warning", logs.buf.String())
	}
}
