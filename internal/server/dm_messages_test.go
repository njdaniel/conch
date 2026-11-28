package server

import (
	"database/sql"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func TestDMMessageRouteAccess(t *testing.T) {
	f := newDMFixture(t)
	a, b := f.join(t), f.join(t)
	conv := f.mustOpen(t, a, b.p.ID)
	ctx := t.Context()
	msg, _, err := f.srv.store.InsertDMMessage(ctx, store.DMPost{
		ConversationID: conv.ID, AuthorID: a.p.ID, Body: "original", ClientNonce: "seed",
	}, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	_, actAs, err := f.srv.store.CreateCredential(ctx, fmt.Sprintf("principal:%d", f.root.ID), a.p.ID, "act as", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Use the peer's revoked token for the 401 cases. The author still needs
	// a live credential to exercise the blocked pair.
	_, revoked, err := f.srv.store.ResolveCredentialDetail(ctx, b.tok)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.RevokeCredential(ctx, "system", revoked); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.srv.store.PutBlock(ctx, b.p.ID, a.p.ID, false, f.srv.now()); err != nil {
		t.Fatal(err)
	}

	base := fmt.Sprintf("/v1/dms/%d/messages", conv.ID)
	item := fmt.Sprintf("%s/%d", base, msg.ID)
	for _, route := range []struct {
		method, path, body string
		blockedStatus      int
	}{
		{http.MethodGet, base, "", http.StatusOK},
		{http.MethodPost, base, `{"body":"next","client_nonce":"next"}`, http.StatusForbidden},
		{http.MethodPatch, item, `{"body":"changed"}`, http.StatusForbidden},
		{http.MethodDelete, item, "", http.StatusNoContent},
	} {
		t.Run(route.method, func(t *testing.T) {
			for _, actor := range []struct {
				name, token, code string
				status            int
			}{
				{"unrelated operator", f.rootTok, schema.ErrorCodeDMNotFound, http.StatusNotFound},
				{"act as author", actAs, schema.ErrorCodeOwnSignInRequired, http.StatusForbidden},
				{"revoked session", b.tok, "unauthenticated", http.StatusUnauthorized},
				{"blocked pair", a.tok, schema.ErrorCodeContactUnavailable, route.blockedStatus},
			} {
				t.Run(actor.name, func(t *testing.T) {
					rec := f.do(t, route.method, route.path, actor.token, route.body)
					if actor.status >= 400 {
						f.wantError(t, rec, actor.status, actor.code)
					} else if rec.Code != actor.status {
						t.Fatalf("status %d %s, want %d", rec.Code, rec.Body, actor.status)
					}
				})
			}
		})
	}
}

func TestDMMessageRateLimits(t *testing.T) {
	f := newDMFixture(t)
	a, b := f.join(t), f.join(t)
	conv := f.mustOpen(t, a, b.p.ID)
	now := time.Now()
	f.srv.now = func() time.Time { return now }
	base := fmt.Sprintf("/v1/dms/%d/messages", conv.ID)
	const body = `{"body":"original","client_nonce":"once"}`
	sent := f.do(t, http.MethodPost, base, a.tok, body)
	if sent.Code != http.StatusCreated {
		t.Fatalf("send: %d %s", sent.Code, sent.Body)
	}
	var out schema.PostDMMessageResponseV1
	dmDecode(t, sent, &out)
	item := fmt.Sprintf("%s/%d", base, out.Message.ID)

	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("missing fixture database path")
	}
	db, err := sql.Open("sqlite", "file:"+path.(string)+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, tt := range []struct {
		method, path, body string
		remaining          int
		success            int
		retryAfter         string
	}{
		{http.MethodPost, base, body, 19, http.StatusOK, "2"},
		{http.MethodPatch, item, `{"body":"original"}`, 10, http.StatusOK, "6"},
		{http.MethodDelete, item, "", 10, http.StatusNoContent, "6"},
	} {
		t.Run(tt.method, func(t *testing.T) {
			for range tt.remaining {
				if rec := f.do(t, tt.method, tt.path, a.tok, tt.body); rec.Code != tt.success {
					t.Fatalf("before limit: %d %s, want %d", rec.Code, rec.Body, tt.success)
				}
			}
			checkLimit := func(t *testing.T) {
				t.Helper()
				rec := f.do(t, tt.method, tt.path, a.tok, tt.body)
				f.wantError(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
				if got := rec.Header().Get("Retry-After"); got != tt.retryAfter {
					t.Errorf("Retry-After = %q, want %q", got, tt.retryAfter)
				}
			}
			t.Run("with concurrent writer", func(t *testing.T) {
				tx, err := db.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback() }()
				checkLimit(t)
			})
			checkLimit(t)
		})
	}
}

func TestDMMessageSendAndHistory(t *testing.T) {
	f := newDMFixture(t)
	a, b, c := f.join(t), f.join(t), f.join(t)
	conv := f.mustOpen(t, a, b.p.ID)
	other := f.mustOpen(t, a, c.p.ID)
	base := fmt.Sprintf("/v1/dms/%d/messages", conv.ID)
	now := time.Now()
	f.srv.now = func() time.Time { return now }
	post := func(path, body string, status int) schema.PostDMMessageResponseV1 {
		t.Helper()
		rec := f.do(t, http.MethodPost, path, a.tok, body)
		if rec.Code != status {
			t.Fatalf("send: %d %s, want %d", rec.Code, rec.Body, status)
		}
		var out schema.PostDMMessageResponseV1
		dmDecode(t, rec, &out)
		if err := out.Validate(); err != nil {
			t.Fatal(err)
		}
		return out
	}

	const firstBody = `{"body":"first message","client_nonce":"first"}`
	first := post(base, firstBody, http.StatusCreated)
	foreign := post(fmt.Sprintf("/v1/dms/%d/messages", other.ID), firstBody, http.StatusCreated)
	last := post(base, `{"body":"second message","client_nonce":"second"}`, http.StatusCreated)
	if first.Replayed || first.Message.Body != "first message" || first.Message.AuthorID != a.p.ID ||
		first.Message.ConversationID != conv.ID || first.Message.ClientNonce != "first" {
		t.Fatalf("unexpected first send: %+v", first)
	}
	if first.Message.CreatedAt != last.Message.CreatedAt || first.Message.ID >= last.Message.ID {
		t.Fatalf("equal-time messages must have ordered ids: %+v, %+v", first, last)
	}

	path, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("missing fixture database path")
	}
	cfg := f.srv.cfg
	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(t.Context(), path.(string))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.srv = New(cfg, reopened)
	f.srv.now = func() time.Time { return now }

	retry := post(base, firstBody, http.StatusOK)
	if !retry.Replayed || retry.Message.ID != first.Message.ID || retry.Message.Body != first.Message.Body {
		t.Fatalf("retry after restart = %+v, original %+v", retry, first)
	}
	f.wantError(t, f.do(t, http.MethodPost, base, a.tok, `{"body":"different","client_nonce":"first"}`),
		http.StatusConflict, schema.ErrorCodeDMNonceConflict)
	if inbox := f.list(t, b, ""); len(inbox.Conversations) != 1 || inbox.Conversations[0].LastMessageID != last.Message.ID ||
		inbox.Conversations[0].Unread != 2 || inbox.Conversations[0].Revision != 2 {
		t.Fatalf("peer's inbox after sends and retry = %+v", inbox)
	}

	for _, tt := range []struct {
		query  string
		want   int64
		after  int64
		before int64
	}{
		{"?limit=1", first.Message.ID, first.Message.ID, 0},
		{fmt.Sprintf("?limit=1&after=%d", first.Message.ID), last.Message.ID, 0, 0},
		{"?order=desc&limit=1", last.Message.ID, 0, last.Message.ID},
		{fmt.Sprintf("?order=desc&limit=1&before=%d", last.Message.ID), first.Message.ID, 0, 0},
	} {
		rec := f.do(t, http.MethodGet, base+tt.query, b.tok, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("history %s: %d %s", tt.query, rec.Code, rec.Body)
		}
		var page schema.ListDMMessagesResponseV1
		dmDecode(t, rec, &page)
		if err := page.Validate(); err != nil {
			t.Fatal(err)
		}
		if len(page.Messages) != 1 || page.Messages[0].ID != tt.want || page.Messages[0].ClientNonce != "" ||
			page.NextAfter != tt.after || page.NextBefore != tt.before {
			t.Errorf("history %s = %+v", tt.query, page)
		}
	}
	for _, query := range []string{
		fmt.Sprintf("?after=%d", foreign.Message.ID),
		fmt.Sprintf("?order=desc&before=%d", foreign.Message.ID),
	} {
		// The author can read both conversations but cannot mix their cursors.
		f.wantError(t, f.do(t, http.MethodGet, base+query, a.tok, ""), http.StatusBadRequest, "invalid_request")
	}
	legacy := f.do(t, http.MethodGet, "/v1/channels/general/messages", a.tok, "")
	if legacy.Code != http.StatusOK {
		t.Fatalf("legacy history: %d %s", legacy.Code, legacy.Body)
	}
	var channelPage schema.ListMessagesResponseV1
	dmDecode(t, legacy, &channelPage)
	if slices.ContainsFunc(channelPage.Messages, func(m schema.MessageV1) bool {
		return m.Body == first.Message.Body || m.Body == last.Message.Body
	}) {
		t.Fatalf("legacy history contains a DM: %s", legacy.Body)
	}
}

func TestDMMessageEditAndRemoval(t *testing.T) {
	f := newDMFixture(t)
	a, b, c := f.join(t), f.join(t), f.join(t)
	conv := f.mustOpen(t, a, b.p.ID)
	other := f.mustOpen(t, a, c.p.ID)
	base := fmt.Sprintf("/v1/dms/%d/messages", conv.ID)
	msg, _, err := f.srv.store.InsertDMMessage(t.Context(), store.DMPost{
		ConversationID: conv.ID, AuthorID: a.p.ID, Body: "original", ClientNonce: "seed",
	}, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	item := fmt.Sprintf("%s/%d", base, msg.ID)

	for _, tt := range []struct{ method, path, body string }{
		{http.MethodPost, base, `{"body":"text","client_nonce":"typed","payload":{"type":"approval"}}`},
		{http.MethodPost, base, `{"body":"text","client_nonce":"audience","audience":null}`},
		{http.MethodPost, base, `{"body":"text","client_nonce":"thread","thread_root_id":1}`},
		{http.MethodPost, base, `{"body":"  ","client_nonce":"empty"}`},
		{http.MethodPost, base, `{"body":"no nonce"}`},
		{http.MethodPost, base, `{"body":"` + strings.Repeat("x", schema.MaxPostRequestBytes) + `","client_nonce":"large"}`},
		{http.MethodPatch, item, `{"body":"text","payload":null}`},
	} {
		f.wantError(t, f.do(t, tt.method, tt.path, a.tok, tt.body), http.StatusBadRequest, "invalid_request")
	}
	f.wantError(t, f.do(t, http.MethodPatch, item, b.tok, `{"body":"peer edit"}`),
		http.StatusForbidden, errForbidden.Code)
	f.wantError(t, f.do(t, http.MethodDelete, item, b.tok, ""), http.StatusForbidden, errForbidden.Code)

	t.Run("other conversation", func(t *testing.T) {
		wrongItem := fmt.Sprintf("/v1/dms/%d/messages/%d", other.ID, msg.ID)
		for _, tt := range []struct{ method, body string }{
			{http.MethodPatch, `{"body":"wrong conversation"}`},
			{http.MethodDelete, ""},
		} {
			t.Run(tt.method, func(t *testing.T) {
				f.wantError(t, f.do(t, tt.method, wrongItem, a.tok, tt.body),
					http.StatusNotFound, errMessageNotFound.Code)
			})
		}
	})

	rec := f.do(t, http.MethodPatch, item, a.tok, `{"body":"edited"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	var edit schema.EditDMMessageResponseV1
	dmDecode(t, rec, &edit)
	if err := edit.Validate(); err != nil || edit.Message.ID != msg.ID || edit.Message.Body != "edited" ||
		edit.Message.EditedAt == nil || edit.Message.Revision != 2 {
		t.Fatalf("edit = %+v, validation: %v", edit, err)
	}
	if _, _, err := f.srv.store.PutBlock(t.Context(), a.p.ID, b.p.ID, false, f.srv.now()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if rec := f.do(t, http.MethodDelete, item, a.tok, ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("author removal: %d %s", rec.Code, rec.Body)
		}
	}
	rec = f.do(t, http.MethodGet, base, b.tok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("peer history after reconnect: %d %s", rec.Code, rec.Body)
	}
	var history schema.ListDMMessagesResponseV1
	dmDecode(t, rec, &history)
	if err := history.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(history.Messages) != 1 || history.Messages[0].ID != msg.ID || !history.Messages[0].Removed ||
		history.Messages[0].Revision != 3 || history.Messages[0].ClientNonce != "" {
		t.Fatalf("peer history = %+v, want one tombstone after the edit and first removal", history)
	}
}
