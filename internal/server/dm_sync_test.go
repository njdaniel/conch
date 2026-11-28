package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func mustDMReply(t *testing.T, f *dmFixture, who dmMember, method, path, body string, status int, dst personDoc) {
	t.Helper()
	rec := f.do(t, method, path, who.tok, body)
	if rec.Code != status {
		t.Fatalf("%s %s: %d %s, want %d", method, path, rec.Code, rec.Body, status)
	}
	if dst != nil {
		dmDecode(t, rec, dst)
		if err := dst.Validate(); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
	}
}

func TestDMSyncLiveAndRecovery(t *testing.T) {
	f := newDMFixture(t)
	author, reader, outsider := f.join(t), f.join(t), f.join(t)
	conv := f.mustOpen(t, author, reader.p.ID)
	other := f.mustOpen(t, outsider, author.p.ID)
	_, readerCred, err := f.srv.store.ResolveCredentialDetail(t.Context(), reader.tok)
	if err != nil {
		t.Fatal(err)
	}
	// Give the existing credential the proof needed to mint a device link.
	if _, err := privateCallFixtureDB(t, f.authFixture).ExecContext(t.Context(),
		"UPDATE credentials SET recent_auth_until = ? WHERE id = ?", f.srv.now().Add(time.Minute).UnixMilli(), readerCred); err != nil {
		t.Fatal(err)
	}
	link, err := f.srv.store.CreateDeviceLink(t.Context(), fmt.Sprintf("principal:%d", reader.p.ID), reader.p.ID, readerCred,
		schema.CreateDeviceLinkRequestV1{}, f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	device, err := f.srv.store.RedeemSignInLink(t.Context(), store.HashSignInLinkCode(link.Code), f.srv.now())
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/v1/dms/%d", conv.ID)
	base := wsTestServer(t, f.srv)
	from, to, second, outside := dialPerson(t, base, author.tok), dialPerson(t, base, reader.tok),
		dialPerson(t, base, device.Token), dialPerson(t, base, outsider.tok)
	for _, client := range []*personClient{from, to, second, outside} {
		_, snap := client.open()
		if client == to && len(snap.Conversations) != 0 {
			t.Fatalf("private open is visible to the peer: %+v", snap)
		}
	}

	const post = `{"body":"one","client_nonce":"author-nonce"}`
	var first schema.PostDMMessageResponseV1
	mustDMReply(t, f, author, http.MethodPost, path+"/messages", post, http.StatusCreated, &first)
	readFrame := func(client *personClient, dst personDoc) {
		t.Helper()
		raw := client.raw()
		if err := json.Unmarshal(raw, dst); err != nil {
			t.Fatalf("frame %s: %v", raw, err)
		}
		if err := dst.Validate(); err != nil {
			t.Fatalf("frame %s: %v", raw, err)
		}
	}
	for _, client := range []*personClient{from, to, second} {
		var m schema.DMMessageV1
		readFrame(client, &m)
		wantNonce := ""
		if client == from {
			wantNonce = "author-nonce"
		}
		if m.ID != first.Message.ID || m.Revision != 1 || m.ClientNonce != wantNonce {
			t.Fatalf("live message = %+v; want first message with nonce %q", m, wantNonce)
		}
	}
	// A retry must not insert a second frame ahead of the next change.
	mustDMReply(t, f, author, http.MethodPost, path+"/messages", post, http.StatusOK, nil)
	mark := fmt.Sprintf(`{"last_read_id":%d}`, first.Message.ID)
	var state schema.DMReadStateV1
	mustDMReply(t, f, reader, http.MethodPut, path+"/read-state", mark, http.StatusOK, &state)
	for _, client := range []*personClient{to, second} {
		var change schema.DMChangedV1
		readFrame(client, &change)
		if change.Kind != schema.DMChangeRead || change.ConversationID != conv.ID {
			t.Fatalf("read signal = %+v", change)
		}
	}
	if state.LastReadID != first.Message.ID || state.Unread != 0 {
		t.Fatalf("reader's mark = %+v", state)
	}
	var again schema.DMReadStateV1
	mustDMReply(t, f, reader, http.MethodPut, path+"/read-state", mark, http.StatusOK, &again)
	if again.LastReadID != state.LastReadID || again.UpdatedAt == nil || state.UpdatedAt == nil || !again.UpdatedAt.Time().Equal(state.UpdatedAt.Time()) {
		t.Fatalf("repeat mark changed %+v to %+v", state, again)
	}

	_ = second.conn.CloseNow()
	item := fmt.Sprintf("%s/messages/%d", path, first.Message.ID)
	mustDMReply(t, f, author, http.MethodPatch, item, `{"body":"edited"}`, http.StatusOK, nil)
	for _, client := range []*personClient{from, to} {
		var edit schema.DMMessageEditedV1
		readFrame(client, &edit)
		if edit.Message.Revision != 2 || edit.Message.Body != "edited" {
			t.Fatalf("live edit = %+v", edit)
		}
	}
	var removed schema.PostDMMessageResponseV1
	mustDMReply(t, f, author, http.MethodPost, path+"/messages", `{"body":"remove","client_nonce":"remove"}`, http.StatusCreated, &removed)
	to.expectMessage(removed.Message.ID, 3)
	mustDMReply(t, f, author, http.MethodDelete, fmt.Sprintf("%s/messages/%d", path, removed.Message.ID), "", http.StatusNoContent, nil)
	var tombstone schema.DMMessageRemovedV1
	readFrame(to, &tombstone)
	if !tombstone.Message.Removed || tombstone.Message.Revision != 4 {
		t.Fatalf("live tombstone = %+v", tombstone)
	}
	var own schema.PostDMMessageResponseV1
	mustDMReply(t, f, reader, http.MethodPost, path+"/messages", `{"body":"own","client_nonce":"own"}`, http.StatusCreated, &own)
	to.expectMessage(own.Message.ID, 5)

	var outsideMessage schema.PostDMMessageResponseV1
	mustDMReply(t, f, outsider, http.MethodPost, fmt.Sprintf("/v1/dms/%d/messages", other.ID),
		`{"body":"other conversation","client_nonce":"other"}`, http.StatusCreated, &outsideMessage)
	outside.expectMessage(outsideMessage.Message.ID, 1)

	_, snap := dialPerson(t, base, device.Token).open()
	if len(snap.Conversations) != 1 {
		t.Fatalf("reconnect snapshot = %+v", snap)
	}
	c := snap.Conversations[0]
	if c.Revision != 5 || c.LastReadID != first.Message.ID || c.Unread != 0 {
		t.Fatalf("reconnected conversation = %+v", c)
	}
	var page, tail schema.ListDMChangesResponseV1
	mustDMReply(t, f, reader, http.MethodGet, path+"/changes?after=1&limit=2", "", http.StatusOK, &page)
	if len(page.Messages) != 2 || page.Messages[0].Body != "edited" || !page.Messages[1].Removed || page.NextAfter != 4 {
		t.Fatalf("changes after dropped socket = %+v", page)
	}
	mustDMReply(t, f, reader, http.MethodGet, path+"/changes?after=4", "", http.StatusOK, &tail)
	if len(tail.Messages) != 1 || tail.Messages[0].ID != own.Message.ID || tail.NextAfter != 0 {
		t.Fatalf("final page of changes = %+v", tail)
	}
}

func TestDMSnapshotRejectsPeersPrivateOpen(t *testing.T) {
	f := newDMFixture(t)
	opener, peer := f.join(t), f.join(t)
	conv := f.mustOpen(t, opener, peer.p.ID)
	f.srv.dmSnapshotSource = func(ctx context.Context, reader int64) (personDMSnapshot, error) {
		rows, enabled, err := f.srv.store.DMSnapshotFor(ctx, reader)
		if err != nil {
			return personDMSnapshot{}, err
		}
		if reader == peer.p.ID {
			// Simulate an overbroad visibility query returning the opener's
			// empty conversation even though the reader has no viewer row.
			rows = append(rows, store.DMConversation{
				ID:       conv.ID,
				Lo:       conv.ParticipantIDs[0],
				Hi:       conv.ParticipantIDs[1],
				PeerID:   opener.p.ID,
				PeerName: opener.p.Name,
				State:    schema.DMStateOpen,
			})
		}
		return f.srv.buildDMSnapshot(reader, rows, enabled), nil
	}
	base := wsTestServer(t, f.srv)
	_, snap := dialPerson(t, base, opener.tok).open()
	if len(snap.Conversations) != 1 || snap.Conversations[0].ID != conv.ID || snap.Conversations[0].CreatedAt != nil {
		t.Fatalf("opener's snapshot = %+v, want the empty conversation %d", snap, conv.ID)
	}
	if _, status, _ := tryDialPerson(t, base, personPath, peer.tok); status != http.StatusInternalServerError {
		t.Fatalf("peer's socket upgrade = %d, want %d", status, http.StatusInternalServerError)
	}
}

func TestDMSyncSnapshotAndCursors(t *testing.T) {
	f := newDMFixture(t)
	a, b, c := f.join(t), f.join(t), f.join(t)
	ab := f.mustOpen(t, b, a.p.ID)
	ac := f.mustOpen(t, c, a.p.ID)
	path := fmt.Sprintf("/v1/dms/%d", ab.ID)
	otherPath := fmt.Sprintf("/v1/dms/%d", ac.ID)
	for _, route := range []struct{ method, suffix, body string }{
		{http.MethodGet, "/changes?after=1", ""},
		{http.MethodPut, "/read-state", `{"last_read_id":1}`},
	} {
		for _, member := range []dmMember{a, c} {
			unknown := f.do(t, route.method, "/v1/dms/1"+route.suffix, member.tok, route.body)
			f.wantError(t, unknown, http.StatusNotFound, schema.ErrorCodeDMNotFound)
			rec := f.do(t, route.method, path+route.suffix, member.tok, route.body)
			if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
				t.Fatalf("%s %s: private or foreign conversation differs from the unknown id: %d %s", route.method, route.suffix, rec.Code, rec.Body)
			}
		}
	}
	now := time.Now()
	f.srv.now = func() time.Time { return now }
	send := func(who dmMember, path, nonce string) schema.DMMessageV1 {
		t.Helper()
		now = now.Add(time.Millisecond)
		var reply schema.PostDMMessageResponseV1
		mustDMReply(t, f, who, http.MethodPost, path+"/messages", fmt.Sprintf(`{"body":"hello","client_nonce":%q}`, nonce), http.StatusCreated, &reply)
		return reply.Message
	}
	first := send(b, path, "first")
	foreign := send(c, otherPath, "foreign")
	send(a, path, "own")
	last := send(b, path, "last")
	mustDMReply(t, f, a, http.MethodPut, path+"/state", `{"state":"archived"}`, http.StatusOK, nil)
	if _, _, err := f.srv.store.PutBlock(t.Context(), b.p.ID, a.p.ID, false, f.srv.now()); err != nil {
		t.Fatal(err)
	}
	var snapshot schema.DMSnapshotV1
	mustDMReply(t, f, a, http.MethodGet, "/v1/dms/snapshot", "", http.StatusOK, &snapshot)
	if rows := snapshot.Conversations; !snapshot.DMsEnabled || snapshot.More || len(rows) != 2 ||
		rows[0].ID != ab.ID || rows[0].State != schema.DMStateArchived || rows[0].Unread != 2 || rows[1].ID != ac.ID {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	var empty schema.ListDMChangesResponseV1
	mustDMReply(t, f, a, http.MethodGet, path+"/changes?after=3", "", http.StatusOK, &empty)
	if len(empty.Messages) != 0 || empty.NextAfter != 0 {
		t.Fatalf("changes at current revision = %+v", empty)
	}
	f.wantError(t, f.do(t, http.MethodGet, path+"/changes?after=4", a.tok, ""), http.StatusBadRequest, "invalid_request")
	f.wantError(t, f.do(t, http.MethodPut, path+"/read-state", a.tok, fmt.Sprintf(`{"last_read_id":%d}`, foreign.ID)), http.StatusBadRequest, "invalid_request")
	var mark, lower, clamped schema.DMReadStateV1
	mustDMReply(t, f, a, http.MethodPut, path+"/read-state", fmt.Sprintf(`{"last_read_id":%d}`, first.ID), http.StatusOK, &mark)
	mustDMReply(t, f, a, http.MethodPut, path+"/read-state", `{"last_read_id":0}`, http.StatusOK, &lower)
	mustDMReply(t, f, a, http.MethodPut, path+"/read-state", fmt.Sprintf(`{"last_read_id":%d}`, last.ID+1000), http.StatusOK, &clamped)
	if mark.LastReadID != first.ID || mark.Unread != 1 || lower.LastReadID != first.ID || lower.Unread != mark.Unread ||
		clamped.LastReadID != last.ID || clamped.Unread != 0 {
		t.Fatalf("first = %+v, lower = %+v, clamped = %+v", mark, lower, clamped)
	}
}

func TestDMReadLimitPrecedesWriteLock(t *testing.T) {
	f := newDMFixture(t)
	a, b := f.join(t), f.join(t)
	conv := f.mustOpen(t, a, b.p.ID)
	now := time.Now()
	f.srv.now = func() time.Time { return now }
	path := fmt.Sprintf("/v1/dms/%d/read-state", conv.ID)
	const mark = `{"last_read_id":0}`
	for range 20 {
		mustDMReply(t, f, a, http.MethodPut, path, mark, http.StatusOK, nil)
	}
	dbPath, ok := testDBPaths.Load(f.srv)
	if !ok {
		t.Fatal("missing fixture database path")
	}
	db, err := sql.Open("sqlite", "file:"+dbPath.(string)+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	rec := f.do(t, http.MethodPut, path, a.tok, mark)
	f.wantError(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
}

func TestDMQueuedDeliveryRechecksAccess(t *testing.T) {
	for _, reason := range []string{"block", "disable", "switched off"} {
		t.Run(reason, func(t *testing.T) {
			testDMQueuedDelivery(t, reason)
		})
	}
}

func testDMQueuedDelivery(t *testing.T, reason string) {
	t.Helper()
	f := newDMFixture(t)
	a, b, c := f.join(t), f.join(t), f.join(t)
	ab := f.mustOpen(t, a, b.p.ID)
	cb := f.mustOpen(t, c, b.p.ID)
	entered, unblockCheck := make(chan struct{}), make(chan struct{})
	checked := make(chan struct{})
	access := f.srv.dmFrameAccess
	f.srv.dmFrameAccess = func(ctx context.Context, reader, conv int64) (bool, error) {
		if reader == b.p.ID && conv == ab.ID {
			defer close(checked)
			close(entered)
			select {
			case <-unblockCheck:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		return access(ctx, reader, conv)
	}
	client := dialPerson(t, wsTestServer(t, f.srv), b.tok)
	client.open()
	var queued schema.PostDMMessageResponseV1
	mustDMReply(t, f, a, http.MethodPost, fmt.Sprintf("/v1/dms/%d/messages", ab.ID),
		`{"body":"queued","client_nonce":"queued"}`, http.StatusCreated, &queued)
	select {
	case <-entered:
	case <-time.After(eventsReadTimeout):
		t.Fatal("queued frame did not reach the delivery check")
	}
	if reason == "disable" {
		rec := f.do(t, http.MethodPost, fmt.Sprintf("/v1/principals/%d/disable", b.p.ID), f.rootTok, "")
		close(unblockCheck)
		if rec.Code != http.StatusOK {
			t.Fatalf("disable: %d %s", rec.Code, rec.Body)
		}
		if closed := client.closed(); closed.Reason != reasonCredentialInvalid {
			t.Fatalf("disabled reader's socket closed with %q", closed.Reason)
		}
		return
	}
	if reason == "switched off" {
		setAllowed := func(allowed bool) {
			t.Helper()
			changed, err := f.srv.store.SetDMAllowed(t.Context(), "system", f.root.ID, b.p.ID, allowed, f.srv.now())
			if err != nil || !changed {
				t.Fatalf("set reader's DMs to %t: changed=%t, err=%v", allowed, changed, err)
			}
		}
		setAllowed(false)
		close(unblockCheck)
		select {
		case <-checked:
		case <-time.After(eventsReadTimeout):
			t.Fatal("queued frame's delivery check did not finish")
		}
		// The check has captured its decision. Re-enable DMs so a later
		// message can mark the next frame expected on the same socket.
		setAllowed(true)
	} else {
		_, _, err := f.srv.store.PutBlock(t.Context(), a.p.ID, b.p.ID, false, f.srv.now())
		close(unblockCheck)
		if err != nil {
			t.Fatal(err)
		}
		mustDMReply(t, f, a, http.MethodDelete, fmt.Sprintf("/v1/dms/%d/messages/%d", ab.ID, queued.Message.ID), "", http.StatusNoContent, nil)
	}
	var msg schema.PostDMMessageResponseV1
	mustDMReply(t, f, c, http.MethodPost, fmt.Sprintf("/v1/dms/%d/messages", cb.ID),
		`{"body":"still allowed","client_nonce":"allowed"}`, http.StatusCreated, &msg)
	client.expectMessage(msg.Message.ID, msg.Message.Revision)
}
