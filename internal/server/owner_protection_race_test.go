package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
)

type promoteOnReadBody struct {
	io.Reader
	once    sync.Once
	promote func()
}

func (b *promoteOnReadBody) Read(p []byte) (int, error) {
	b.once.Do(b.promote)
	return b.Reader.Read(p)
}

func TestOwnerPromotionBetweenPrecheckAndBodyWriteIsForbidden(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   func(*adminFixture, int64) string
		body   string
	}{
		{
			name:   "rename",
			method: http.MethodPut,
			path: func(_ *adminFixture, id int64) string {
				return fmt.Sprintf("/v1/principals/%d/name", id)
			},
			body: `{"name":"renamed-after-promotion"}`,
		},
		{
			name:   "invite permission",
			method: http.MethodPut,
			path: func(_ *adminFixture, id int64) string {
				return fmt.Sprintf("/v1/principals/%d/invite-permission", id)
			},
			body: `{"may_invite":true}`,
		},
		{
			name:   "create sign-in link",
			method: http.MethodPost,
			path: func(_ *adminFixture, id int64) string {
				return fmt.Sprintf("/v1/principals/%d/sign-in-links", id)
			},
			body: `{}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAdminFixture(t)
			target := f.cand
			var promoteErr error
			body := &promoteOnReadBody{
				Reader: strings.NewReader(tt.body),
				promote: func() {
					mark := true
					_, promoteErr = f.srv.store.SetRole(context.Background(), fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, target.ID, store.RoleOperator, &mark)
				},
			}
			req := httptest.NewRequest(tt.method, tt.path(f, target.ID), body)
			req.Header.Set("Authorization", "Bearer "+f.op2Tok)
			before := f.deniedRows(t, f.op2.ID)
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)
			if promoteErr != nil {
				t.Fatalf("promote target after the owner precheck: %v", promoteErr)
			}
			assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
			if got := f.deniedRows(t, f.op2.ID) - before; got != 1 {
				t.Errorf("access_denied rows = %d, want 1", got)
			}
		})
	}
}

func TestDisableInviteesPromotionBeforeDisableIsForbidden(t *testing.T) {
	f := newAdminFixture(t)
	target := f.cand
	mark := true
	if _, err := f.srv.store.SetRole(context.Background(), fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, target.ID, store.RoleOperator, &mark); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/principals/%d/disable-invitees", f.alice.ID), nil)
	req.Pattern = "POST /v1/principals/{id}/disable-invitees"
	req = req.WithContext(withCaller(req.Context(), f.op2))
	done, err := f.srv.disablePrincipals(req.Context(), []int64{target.ID}, !isOwner(f.op2))
	if !errors.Is(err, store.ErrOwnerProtected) || len(done) != 0 {
		t.Fatalf("disablePrincipals = %v, %v; want no ids and ErrOwnerProtected", done, err)
	}
	before := f.deniedRows(t, f.op2.ID)
	rec := httptest.NewRecorder()
	f.srv.finishBulkDisable(rec, req, err, store.AuditInviteesDisabled, fmt.Sprintf("principal:%d", f.alice.ID), "root=1 depth=1 count=0", done)
	assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
	if got := f.deniedRows(t, f.op2.ID) - before; got != 1 {
		t.Errorf("access_denied rows = %d, want 1", got)
	}
}
