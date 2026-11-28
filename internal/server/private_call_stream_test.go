package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func (c *personClient) callSnapshot() schema.PrivateCallSnapshotV1 {
	c.t.Helper()
	var frame schema.PrivateCallSnapshotV1
	c.callFrame(schema.PrivateCallSnapshotSchemaV1, &frame)
	return frame
}

func (c *personClient) callFrame(kind string, frame personDoc) {
	c.t.Helper()
	raw := c.raw()
	if personSchemaOf(c.t, raw) != kind {
		c.t.Fatalf("next frame = %s, want %s", raw, kind)
	}
	if err := json.Unmarshal(raw, frame); err != nil {
		c.t.Fatal(err)
	}
	if err := frame.Validate(); err != nil {
		c.t.Fatalf("invalid call frame %s: %v", raw, err)
	}
}

func (c *personClient) expectCall(kind, id string, revision int64, state schema.PrivateCallState, selected bool) {
	c.t.Helper()
	var call schema.PrivateCallV1
	switch kind {
	case schema.PrivateCallRingingSchemaV1:
		var frame schema.PrivateCallRingingV1
		c.callFrame(kind, &frame)
		call = frame.Call
	case schema.PrivateCallStateSchemaV1:
		var frame schema.PrivateCallStateV1
		c.callFrame(kind, &frame)
		call = frame.Call
	default:
		c.t.Fatalf("unexpected call frame kind %q", kind)
	}
	if call.ID != id || call.Revision != revision || call.State != state || call.SelectedHere != selected || call.Reason != "" {
		c.t.Fatalf("call frame = %+v, want %s r%d %s (selected=%t), generic reason", call, id, revision, state, selected)
	}
}

func (c *personClient) expectStopRinging(id string, state schema.PrivateCallState) {
	c.t.Helper()
	var frame schema.PrivateCallStopRingingV1
	c.callFrame(schema.PrivateCallStopRingingSchemaV1, &frame)
	if frame.CallID != id || frame.Revision != 2 || frame.State != state {
		c.t.Fatalf("stop ringing frame = %+v, want %s revision 2 %s", frame, id, state)
	}
}

func dialPrivateCallStream(t *testing.T, base, token string) (*personClient, schema.PrivateCallSnapshotV1) {
	t.Helper()
	c := dialPerson(t, base, token)
	c.openDM()
	return c, c.callSnapshot()
}

func callStreamMarker(t *testing.T, f *authFixture, principal int64, c *personClient) {
	t.Helper()
	const marker = 9000
	if err := f.srv.person.publish(principal, changedDoc(principal, marker)); err != nil {
		t.Fatal(err)
	}
	c.expectChanged(marker)
}

func TestPrivateCallStreamDismissesRinging(t *testing.T) {
	for _, tc := range []struct {
		action string
		want   schema.PrivateCallState
	}{
		{"accept", schema.PrivateCallStateAccepted},
		{"decline", schema.PrivateCallStateDeclined},
		{"cancel", schema.PrivateCallStateCancelled},
	} {
		t.Run(tc.action, func(t *testing.T) {
			f, bob, phoneToken, laptopToken := privateCallHTTPFixture(t)
			f.srv.dmFrameAccess = allowPersonDMFixture
			base := wsTestServer(t, f.srv)
			caller, _ := dialPrivateCallStream(t, base, f.aliceTok)
			phone, _ := dialPrivateCallStream(t, base, phoneToken)
			laptop, _ := dialPrivateCallStream(t, base, laptopToken)
			observer, snap := dialPrivateCallStream(t, base, f.rootTok)
			if snap.Call != nil {
				t.Fatalf("observer snapshot = %+v", snap.Call)
			}

			const invite = `{"action_id":"ring"}`
			call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair, f.aliceTok, invite), http.StatusCreated).Call
			caller.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 1, schema.PrivateCallStateRinging, true)
			for _, tab := range []*personClient{phone, laptop} {
				tab.expectCall(schema.PrivateCallRingingSchemaV1, call.ID, 1, schema.PrivateCallStateRinging, false)
			}

			_ = laptop.conn.CloseNow()
			laptop, snap = dialPrivateCallStream(t, base, laptopToken)
			if snap.Call == nil || snap.Call.ID != call.ID || snap.Call.Revision != 1 || snap.Call.Role != schema.PrivateCallRoleCallee {
				t.Fatalf("reconnected ring = %+v", snap.Call)
			}
			replay := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair, f.aliceTok, invite), http.StatusOK)
			if !replay.Replayed {
				t.Fatalf("invite retry = %+v", replay)
			}
			if err := f.srv.person.publish(bob.ID, changedDoc(bob.ID, 9000)); err != nil {
				t.Fatal(err)
			}
			phone.expectChanged(9000)
			laptop.expectChanged(9000)

			token := phoneToken
			if tc.action == "cancel" {
				token = f.aliceTok
			}
			decodePrivateCallResponse(t, f.do(t, "POST", "/v1/calls/"+call.ID+"/"+tc.action,
				token, privateCallTestCommand(call, tc.action)), http.StatusOK)
			caller.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 2, tc.want, tc.action == "accept")
			for i, tab := range []*personClient{phone, laptop} {
				tab.expectStopRinging(call.ID, tc.want)
				tab.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 2, tc.want, i == 0 && tc.action == "accept")
			}
			// A duplicate of the current revision followed by a late old
			// revision leaves the socket at the revision it already received.
			for _, revision := range []int64{2, 1} {
				frame := personFrame{callID: call.ID, callRevision: revision}
				f.srv.person.enqueue(bob.ID, frame)
				// Even an incorrectly addressed reference must pass the
				// persisted membership check before it becomes a document.
				f.srv.person.enqueue(f.root.ID, frame)
			}
			if err := f.srv.person.publish(bob.ID, changedDoc(bob.ID, 9000)); err != nil {
				t.Fatal(err)
			}
			phone.expectChanged(9000)
			laptop.expectChanged(9000)
			callStreamMarker(t, f, f.root.ID, observer)
		})
	}
}

func TestPrivateCallStreamSelectedLogout(t *testing.T) {
	f, bob, phoneToken, laptopToken := privateCallHTTPFixture(t)
	base := wsTestServer(t, f.srv)
	caller, _ := dialPrivateCallStream(t, base, f.aliceTok)
	phone, _ := dialPrivateCallStream(t, base, phoneToken)
	laptop, _ := dialPrivateCallStream(t, base, laptopToken)
	call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"ring"}`), http.StatusCreated).Call
	caller.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 1, schema.PrivateCallStateRinging, true)
	for _, tab := range []*personClient{phone, laptop} {
		tab.expectCall(schema.PrivateCallRingingSchemaV1, call.ID, 1, schema.PrivateCallStateRinging, false)
	}
	decodePrivateCallResponse(t, f.do(t, "POST", "/v1/calls/"+call.ID+"/accept",
		phoneToken, privateCallTestCommand(call, "pick-phone")), http.StatusOK)
	caller.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 2, schema.PrivateCallStateAccepted, true)
	for i, tab := range []*personClient{phone, laptop} {
		tab.expectStopRinging(call.ID, schema.PrivateCallStateAccepted)
		tab.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 2, schema.PrivateCallStateAccepted, i == 0)
	}
	ctx := context.Background()
	_, phoneCred, err := f.srv.store.ResolveCredentialDetail(ctx, phoneToken)
	if err != nil {
		t.Fatal(err)
	}
	_, callerCred, err := f.srv.store.ResolveCredentialDetail(ctx, f.aliceTok)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.ObservePrivateCallEndpoints(ctx, call.ID, 2, callerCred, phoneCred, time.Now); err != nil {
		t.Fatal(err)
	}
	caller.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 3, schema.PrivateCallStateActive, true)
	phone.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 3, schema.PrivateCallStateActive, true)
	laptop.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 3, schema.PrivateCallStateActive, false)

	_, laptopCred, err := f.srv.store.ResolveCredentialDetail(ctx, laptopToken)
	if err != nil {
		t.Fatal(err)
	}
	logout := func(token string, id int64) {
		t.Helper()
		if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/mine/%d", id), token, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("logout = %d %s", rec.Code, rec.Body)
		}
	}
	logout(laptopToken, laptopCred)
	_ = laptop.closed()
	assertStoredCallState(t, f, call.ID, schema.PrivateCallStateActive, 3)

	successor, successorToken, err := f.srv.store.CreateCredential(ctx, fmt.Sprintf("principal:%d", bob.ID), bob.ID, "other tab", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := privateCallFixtureDB(t, f).Exec("UPDATE credentials SET self_made = 1 WHERE id = ?", successor.ID); err != nil {
		t.Fatal(err)
	}
	// Reconnect the sibling before the selected credential is revoked.
	sibling, snap := dialPrivateCallStream(t, base, successorToken)
	if snap.Call == nil || snap.Call.SelectedHere || snap.Call.State != schema.PrivateCallStateActive {
		t.Fatalf("sibling snapshot = %+v", snap.Call)
	}
	logout(phoneToken, phoneCred)
	_ = phone.closed()
	assertStoredCallState(t, f, call.ID, schema.PrivateCallStateEnded, 4)
	sibling.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 4, schema.PrivateCallStateEnded, false)
	caller.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 4, schema.PrivateCallStateEnded, false)
}

func assertStoredCallState(t *testing.T, f *authFixture, id string, want schema.PrivateCallState, revision int64) {
	t.Helper()
	var state schema.PrivateCallState
	var rev int64
	err := privateCallFixtureDB(t, f).QueryRow("SELECT state, revision FROM private_calls WHERE id = ?", id).Scan(&state, &rev)
	if err != nil || state != want || rev != revision {
		t.Fatalf("persisted call = %s r%d (%v), want %s r%d", state, rev, err, want, revision)
	}
}

func TestPrivateCallStreamBlockDiscardsQueuedRing(t *testing.T) {
	f, bob, phoneToken, laptopToken := privateCallHTTPFixture(t)
	gate := make(chan struct{})
	held := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
	})
	f.srv.dmFrameAccess = func(ctx context.Context, principal, conversation int64) (bool, error) {
		if principal == bob.ID && conversation == 9001 {
			close(held)
			select {
			case <-gate:
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
		return true, nil
	}
	base := wsTestServer(t, f.srv)
	caller, _ := dialPrivateCallStream(t, base, f.aliceTok)
	phone, _ := dialPrivateCallStream(t, base, phoneToken)
	if err := f.srv.person.publish(bob.ID, changedDoc(bob.ID, 9001)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held:
	case <-time.After(eventsReadTimeout):
		t.Fatal("callee socket did not pause on its queued frame")
	}
	// This socket is free to see the same ring before the block.
	laptop, _ := dialPrivateCallStream(t, base, laptopToken)
	call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"ring"}`), http.StatusCreated).Call
	caller.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 1, schema.PrivateCallStateRinging, true)
	laptop.expectCall(schema.PrivateCallRingingSchemaV1, call.ID, 1, schema.PrivateCallStateRinging, false)
	path := fmt.Sprintf("/v1/blocks/%d", f.alice.ID)
	for _, method := range []string{"PUT", "DELETE"} {
		if rec := f.do(t, method, path, laptopToken, ""); rec.Code != http.StatusOK && rec.Code != http.StatusNoContent {
			t.Fatalf("%s block = %d %s", method, rec.Code, rec.Body)
		}
	}
	assertStoredCallState(t, f, call.ID, schema.PrivateCallStateCancelled, 2)
	caller.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 2, schema.PrivateCallStateCancelled, false)
	laptop.expectStopRinging(call.ID, schema.PrivateCallStateCancelled)
	laptop.expectCall(schema.PrivateCallStateSchemaV1, call.ID, 2, schema.PrivateCallStateCancelled, false)
	close(gate)
	phone.expectChanged(9001)
	callStreamMarker(t, f, bob.ID, phone)
}

func TestPrivateCallStreamAfterRestart(t *testing.T) {
	f, _, phoneToken, _ := privateCallHTTPFixture(t)
	base := wsTestServer(t, f.srv)
	call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
		f.aliceTok, `{"action_id":"ring"}`), http.StatusCreated).Call
	first, snap := dialPrivateCallStream(t, base, phoneToken)
	if snap.Call == nil || snap.Call.ID != call.ID {
		t.Fatalf("initial call snapshot = %+v", snap.Call)
	}
	_ = first.conn.CloseNow()
	if err := f.srv.store.RestartPrivateCalls(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_, snap = dialPrivateCallStream(t, base, phoneToken)
	if snap.Call != nil {
		t.Fatalf("restart restored a live call: %+v", snap.Call)
	}
	assertStoredCallState(t, f, call.ID, schema.PrivateCallStateFailed, 2)
}

func TestPrivateCallAuthorityLossCommitsTerminalState(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		change func(f *authFixture, peerID int64) error
	}{
		{"voice off", func(f *authFixture, peerID int64) error {
			_, err := f.srv.store.SetVoiceAllowed(ctx, "system", 0, peerID, false)
			return err
		}},
		{"DMs off for a participant", func(f *authFixture, peerID int64) error {
			_, err := f.srv.store.SetDMAllowed(ctx, "system", 0, peerID, false, f.srv.now())
			return err
		}},
		{"DMs off for the server", func(f *authFixture, _ int64) error {
			_, err := f.srv.store.SetDMServerSettings(ctx, "system", 0, schema.DMServerSettingsV1{}, f.srv.now())
			return err
		}},
		{"participant disabled", func(f *authFixture, peerID int64) error {
			_, err := f.srv.store.DisablePrincipal(ctx, "system", peerID)
			return err
		}},
		{"all callee credentials revoked", func(f *authFixture, peerID int64) error {
			_, err := f.srv.store.RevokeAllCredentials(ctx, "system", peerID)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, active := range []bool{false, true} {
				t.Run(fmt.Sprintf("active=%t", active), func(t *testing.T) {
					f, bob, phoneToken, _ := privateCallHTTPFixture(t)
					call := decodePrivateCallResponse(t, f.do(t, "POST", privateCallTestPair,
						f.aliceTok, `{"action_id":"ring"}`), http.StatusCreated).Call
					if active {
						decodePrivateCallResponse(t, f.do(t, "POST", "/v1/calls/"+call.ID+"/accept",
							phoneToken, privateCallTestCommand(call, "accept")), http.StatusOK)
						_, caller, err := f.srv.store.ResolveCredentialDetail(ctx, f.aliceTok)
						if err != nil {
							t.Fatal(err)
						}
						_, callee, err := f.srv.store.ResolveCredentialDetail(ctx, phoneToken)
						if err != nil {
							t.Fatal(err)
						}
						if err := f.srv.store.ObservePrivateCallEndpoints(ctx, call.ID, 2, caller, callee, f.srv.now); err != nil {
							t.Fatal(err)
						}
					}
					if err := tc.change(f, bob.ID); err != nil {
						t.Fatal(err)
					}
					// Read the persisted row directly so a later request or
					// sweep cannot perform the revocation for this assertion.
					if active {
						assertStoredCallState(t, f, call.ID, schema.PrivateCallStateEnded, 4)
					} else {
						assertStoredCallState(t, f, call.ID, schema.PrivateCallStateCancelled, 2)
					}
				})
			}
		})
	}
}
