package server

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// An invited account (POST /v2/invites/redeem, issue #707) is held to the
// rule of issue #711 too: whatever lands between the hash and the
// redeeming transaction (the invite closed, invites paused, the invite made
// guest-only, the name taken) is seen by that transaction, and a refused
// redemption makes no account, stores no password and spends no use of the
// invite.
func TestRedeemV2RechecksAfterHashing(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name   string
		gap    func(t *testing.T, f *loginFixture, inviteID int64)
		status int
		code   string
	}{
		{"invite closed", func(t *testing.T, f *loginFixture, id int64) {
			if rec := f.send(t, "DELETE", fmt.Sprintf("/v1/invites/%d", id), f.rootTok, "", ""); rec.Code != http.StatusNoContent {
				t.Fatalf("close = %d %s", rec.Code, rec.Body)
			}
		}, http.StatusNotFound, schema.ErrorCodeInviteInvalid},
		{"invites paused", func(t *testing.T, f *loginFixture, _ int64) {
			if _, err := f.srv.store.SetInvitesPaused(ctx, "system", true); err != nil {
				t.Fatal(err)
			}
		}, http.StatusServiceUnavailable, schema.ErrorCodeInvitesPaused},
		{"invite made guest-only", func(t *testing.T, f *loginFixture, id int64) {
			f.exec(t, "UPDATE invites SET join_as = 'guest' WHERE id = ?", id)
		}, http.StatusForbidden, schema.ErrorCodeJoiningClosed},
		{"name taken", func(t *testing.T, f *loginFixture, _ int64) {
			if _, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "Wren"); err != nil {
				t.Fatal(err)
			}
		}, http.StatusConflict, schema.ErrorCodePrincipalExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newLoginFixture(t, Config{})
			inv := f.createInvite(t, f.rootTok, openLink("red"))
			f.srv.beforePasswordWrite = func() { tt.gap(t, f, inv.Invite.ID) }
			assertErrorBody(t, f.redeemWithPassword(t, inv.Code, "wren"), tt.status, tt.code)
			f.srv.beforePasswordWrite = nil
			if n := f.queryString(t, "SELECT COUNT(*) FROM principals WHERE name = 'wren'"); n != "0" {
				t.Errorf("an account was made")
			}
			if n := f.queryString(t, "SELECT COUNT(*) FROM passwords"); n != "2" {
				t.Errorf("passwords = %s, want only alice's and root's", n)
			}
			if n := f.queryString(t, fmt.Sprintf("SELECT uses FROM invites WHERE id = %d", inv.Invite.ID)); n != "0" {
				t.Errorf("the invite's uses = %s, want none spent", n)
			}
		})
	}
}
