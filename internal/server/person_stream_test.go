package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// The per-person events stream (issue #2040), against the real server and
// hubs, with no LiveKit. Every frame is decoded by its schema and validated;
// no test sleeps for timing: the re-check interval is injected, and "nothing
// arrived" is shown by a marker frame, published afterwards, being the next
// thing read.

const personPath = "/v1/events/person/ws"

// personClient is one open person socket.
type personClient struct {
	t    *testing.T
	conn *websocket.Conn
}

// tryDialPerson dials the person stream and returns the socket, or the status
// the server refused the upgrade with.
func tryDialPerson(t *testing.T, base, path, token string) (*personClient, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	var hdr http.Header
	if token != "" {
		hdr = http.Header{"Authorization": {"Bearer " + token}}
	}
	conn, resp, err := websocket.Dial(ctx, base+path, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		if resp == nil {
			t.Fatalf("dial %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		return nil, resp.StatusCode
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &personClient{t: t, conn: conn}, http.StatusSwitchingProtocols
}

func dialPerson(t *testing.T, base, token string) *personClient {
	t.Helper()
	c, status := tryDialPerson(t, base, personPath, token)
	if c == nil {
		t.Fatalf("person stream refused with %d", status)
	}
	return c
}

// raw reads one text frame.
func (c *personClient) raw() []byte {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	typ, raw, err := c.conn.Read(ctx)
	if err != nil {
		c.t.Fatalf("read a frame: %v", err)
	}
	if typ != websocket.MessageText {
		c.t.Fatalf("frame type = %v, want text", typ)
	}
	return raw
}

func personSchemaOf(t *testing.T, raw []byte) string {
	t.Helper()
	var head struct {
		Schema string `json:"schema"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		t.Fatalf("frame is not JSON: %v\n%s", err, raw)
	}
	return head.Schema
}

// open reads the two frames every connection starts with and returns them.
func (c *personClient) open() (schema.PersonStreamHelloV1, schema.DMSnapshotV1) {
	c.t.Helper()
	var hello schema.PersonStreamHelloV1
	raw := c.raw()
	if got := personSchemaOf(c.t, raw); got != schema.PersonStreamHelloSchemaV1 {
		c.t.Fatalf("first frame is %q, want the hello", got)
	}
	if err := json.Unmarshal(raw, &hello); err != nil {
		c.t.Fatal(err)
	}
	if err := hello.Validate(); err != nil {
		c.t.Fatalf("invalid hello: %v", err)
	}
	var snap schema.DMSnapshotV1
	raw = c.raw()
	if got := personSchemaOf(c.t, raw); got != schema.DMSnapshotSchemaV1 {
		c.t.Fatalf("second frame is %q, want the snapshot", got)
	}
	if err := json.Unmarshal(raw, &snap); err != nil {
		c.t.Fatal(err)
	}
	if err := snap.Validate(); err != nil {
		c.t.Fatalf("invalid snapshot: %v", err)
	}
	return hello, snap
}

// changed reads one frame and expects it to be a dm.changed for conversation.
func (c *personClient) expectChanged(conversation int64) {
	c.t.Helper()
	raw := c.raw()
	var doc schema.DMChangedV1
	if personSchemaOf(c.t, raw) != schema.DMChangedSchemaV1 {
		c.t.Fatalf("next frame is %s, want dm_changed %d", raw, conversation)
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.ConversationID != conversation {
		c.t.Fatalf("next frame is %s, want dm_changed %d (%v)", raw, conversation, err)
	}
}

// closed reads until the socket is closed and returns how.
func (c *personClient) closed() websocket.CloseError {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	for {
		_, _, err := c.conn.Read(ctx)
		if err == nil {
			continue
		}
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			c.t.Fatalf("the socket ended with %v, want a close frame", err)
		}
		return ce
	}
}

func changedDoc(conversation int64) schema.DMChangedV1 {
	return schema.DMChangedV1{Schema: schema.DMChangedSchemaV1, ConversationID: conversation, Kind: schema.DMChangeRead}
}

func dmMessageDoc(id int64) schema.DMMessageV1 {
	return schema.DMMessageV1{
		Schema: schema.DMMessageSchemaV1, ID: id, ConversationID: 12, AuthorID: 5,
		CreatedAt: schema.NewTimestamp(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)), Body: "hi",
	}
}

// TestPersonStreamWhoMayOpen: a person's own live credential opens it, with a
// bearer or a ticket; an agent, a guest, a dead or missing credential and a
// server with authentication off do not, and nothing is upgraded for them.
func TestPersonStreamWhoMayOpen(t *testing.T) {
	f := newRunFixture(t)
	g := newGuest(t, f.srv, "Pat", time.Hour, 2*time.Hour, f.general.ID)
	revoked := f.credentialID(t, f.aliceTok2)
	if err := f.srv.store.RevokeCredential(context.Background(), "system", revoked); err != nil {
		t.Fatal(err)
	}
	base := wsTestServer(t, f.srv)

	ticket := func() string {
		rec := f.do(t, "POST", "/v1/ws-tickets", f.aliceTok, "")
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("ticket = %d %s", rec.Code, rec.Body)
		}
		var tk schema.WSTicketV1
		if err := json.Unmarshal(rec.Body.Bytes(), &tk); err != nil {
			t.Fatal(err)
		}
		return tk.Ticket
	}

	tests := []struct {
		name   string
		path   string
		token  string
		status int
	}{
		{"a person, bearer", personPath, f.aliceTok, http.StatusSwitchingProtocols},
		{"another person", personPath, f.bobTok, http.StatusSwitchingProtocols},
		{"a person, ticket", personPath + "?ticket=" + ticket(), "", http.StatusSwitchingProtocols},
		{"an operator is a person too", personPath, f.rootTok, http.StatusSwitchingProtocols},
		{"an agent", personPath, f.botTok, http.StatusForbidden},
		{"another agent", personPath, f.rivalTok, http.StatusForbidden},
		{"a guest", personPath, g.Token, http.StatusForbidden},
		{"no credential", personPath, "", http.StatusUnauthorized},
		{"a made-up credential", personPath, "not-a-token", http.StatusUnauthorized},
		{"a revoked credential", personPath, f.aliceTok2, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, status := tryDialPerson(t, base, tt.path, tt.token)
			if status != tt.status {
				t.Fatalf("status = %d, want %d", status, tt.status)
			}
			if tt.status == http.StatusSwitchingProtocols {
				c.open()
			}
		})
	}

	t.Run("a disabled person", func(t *testing.T) {
		f := newRunFixture(t)
		if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.bob.ID); err != nil {
			t.Fatal(err)
		}
		_, status := tryDialPerson(t, wsTestServer(t, f.srv), personPath, f.bobTok)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	t.Run("authentication off has no person", func(t *testing.T) {
		off := newAuthFixture(t, AuthOff)
		_, status := tryDialPerson(t, wsTestServer(t, off.srv), personPath, "")
		if status != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", status)
		}
	})
}

// TestPersonStreamOpens: the first frame is the hello with an instance id of
// this socket alone, and the second is the snapshot. With no DM store
// installed the snapshot is empty and says DMs are not available; an
// installed source is what is sent.
func TestPersonStreamOpens(t *testing.T) {
	f := newRunFixture(t)
	base := wsTestServer(t, f.srv)

	t.Run("two sockets of one person have two instance ids", func(t *testing.T) {
		a := dialPerson(t, base, f.aliceTok)
		b := dialPerson(t, base, f.aliceTok) // the same credential, as two tabs have
		helloA, snapA := a.open()
		helloB, _ := b.open()
		if helloA.InstanceID == helloB.InstanceID {
			t.Fatalf("both sockets were given instance id %s", helloA.InstanceID)
		}
		if len(snapA.Conversations) != 0 || snapA.HighWater != 0 || snapA.DMsEnabled {
			t.Errorf("the default snapshot = %+v, want empty and DMs not available", snapA)
		}
		// A reconnect is a new socket: a new id.
		_ = a.conn.CloseNow()
		c := dialPerson(t, base, f.aliceTok)
		helloC, _ := c.open()
		if helloC.InstanceID == helloA.InstanceID {
			t.Error("a reconnect kept the old instance id")
		}
	})

	t.Run("the snapshot is the installed source's, for the caller", func(t *testing.T) {
		var asked []int64
		f.srv.dmSnapshotSource = func(_ context.Context, id int64) (schema.DMSnapshotV1, error) {
			asked = append(asked, id)
			s, _ := f.srv.emptyDMSnapshot(context.Background(), id)
			s.HighWater, s.DMsEnabled = 4310, true
			return s, nil
		}
		t.Cleanup(func() { f.srv.dmSnapshotSource = nil })
		_, snap := dialPerson(t, base, f.bobTok).open()
		if len(asked) != 1 || asked[0] != f.bob.ID {
			t.Errorf("source asked for %v, want only bob %d", asked, f.bob.ID)
		}
		if snap.HighWater != 4310 || !snap.DMsEnabled {
			t.Errorf("snapshot = %+v", snap)
		}
	})

	t.Run("a source that fails or lies is a 500, not an upgrade", func(t *testing.T) {
		for name, src := range map[string]dmSnapshotSource{
			"error": func(context.Context, int64) (schema.DMSnapshotV1, error) {
				return schema.DMSnapshotV1{}, errors.New("boom")
			},
			"invalid": func(context.Context, int64) (schema.DMSnapshotV1, error) { return schema.DMSnapshotV1{}, nil },
		} {
			f.srv.dmSnapshotSource = src
			_, status := tryDialPerson(t, base, personPath, f.aliceTok)
			if status != http.StatusInternalServerError {
				t.Errorf("%s: status = %d, want 500", name, status)
			}
		}
		f.srv.dmSnapshotSource = nil
	})
}

// TestPersonStreamSnapshotOrdering: a frame published while the snapshot is
// being taken is held and sent after it, unless the snapshot already holds
// it (an id at or below high_water), so nothing falls between the two.
func TestPersonStreamSnapshotOrdering(t *testing.T) {
	f := newRunFixture(t)
	base := wsTestServer(t, f.srv)
	f.srv.dmSnapshotSource = func(ctx context.Context, id int64) (schema.DMSnapshotV1, error) {
		for _, msg := range []int64{40, 41, 42} {
			if err := f.srv.person.publish(id, dmMessageDoc(msg)); err != nil {
				t.Errorf("publish: %v", err)
			}
		}
		s, _ := f.srv.emptyDMSnapshot(ctx, id)
		s.HighWater = 41 // 40 and 41 are in the snapshot; 42 is not
		return s, nil
	}
	c := dialPerson(t, base, f.aliceTok)
	c.open()
	raw := c.raw()
	var m schema.DMMessageV1
	if personSchemaOf(t, raw) != schema.DMMessageSchemaV1 || json.Unmarshal(raw, &m) != nil || m.ID != 42 {
		t.Fatalf("first frame after the snapshot = %s, want message 42", raw)
	}
	if err := f.srv.person.publish(f.alice.ID, changedDoc(9)); err != nil {
		t.Fatal(err)
	}
	c.expectChanged(9) // 40 and 41 were not sent
}

// TestPersonStreamCarriesOnlyFramesAddressedToIt: a frame goes to the sockets
// of the principal it was published for, all of them, and to no one else.
func TestPersonStreamCarriesOnlyFramesAddressedToIt(t *testing.T) {
	f := newRunFixture(t)
	base := wsTestServer(t, f.srv)
	alice1, alice2 := dialPerson(t, base, f.aliceTok), dialPerson(t, base, f.aliceTok2)
	bob := dialPerson(t, base, f.bobTok)
	for _, c := range []*personClient{alice1, alice2, bob} {
		c.open()
	}
	hub := f.srv.person
	if err := hub.publish(f.alice.ID, changedDoc(1)); err != nil {
		t.Fatal(err)
	}
	if err := hub.publish(f.bob.ID, changedDoc(2)); err != nil {
		t.Fatal(err)
	}
	if err := hub.publish(f.alice.ID, changedDoc(3)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*personClient{alice1, alice2} {
		c.expectChanged(1)
		c.expectChanged(3)
	}
	bob.expectChanged(2) // not 1: alice's frames never reached him

	// Addressed to no one, or to no one connected: nothing breaks.
	for _, id := range []int64{0, -1} {
		if err := hub.publish(id, changedDoc(4)); err == nil {
			t.Errorf("publish to principal %d succeeded", id)
		}
	}
	if err := hub.publish(f.carol.ID, changedDoc(5)); err != nil {
		t.Errorf("publish to a principal with no socket: %v", err)
	}
}

// TestPersonStreamNeverCarriesAChannelFrame: activity in the channels the
// person is in, and every channel document, stays off it. The registry refuses
// a document that is not a DM frame, and the socket has no channel to
// subscribe to.
func TestPersonStreamNeverCarriesAChannelFrame(t *testing.T) {
	t.Run("the registry refuses channel documents", func(t *testing.T) {
		f := newRunFixture(t)
		docs := map[string]personDoc{
			"message":           schema.MessageV2{},
			"channels changed":  schema.ChannelsChangedV1{Schema: schema.ChannelsChangedSchemaV1},
			"permissions":       schema.PermissionsChangedV1{Schema: schema.PermissionsChangedSchemaV1},
			"directory changed": schema.DirectoryChangedV1{Schema: schema.DirectoryChangedSchemaV1},
			"roster":            schema.SessionRosterV1{},
			"read state":        schema.ReadStateV1{},
			"run":               schema.RunV1{},
			"hello (the socket's own)": schema.PersonStreamHelloV1{
				Schema: schema.PersonStreamHelloSchemaV1, InstanceID: "0f1e2d3c4b5a69788796a5b4c3d2e1f0"},
			"snapshot (the socket's own)": schema.DMSnapshotV1{},
		}
		for name, doc := range docs {
			if err := f.srv.person.publish(f.alice.ID, doc); !errors.Is(err, errPersonFrameKind) {
				t.Errorf("%s: publish = %v, want errPersonFrameKind", name, err)
			}
		}
		if err := f.srv.person.publish(f.alice.ID, schema.DMChangedV1{}); err == nil || errors.Is(err, errPersonFrameKind) {
			t.Errorf("an invalid DM frame: publish = %v, want its validation error", err)
		}
	})

	t.Run("channel activity does not reach the socket", func(t *testing.T) {
		f := newRunFixture(t)
		base := wsTestServer(t, f.srv)
		person := dialPerson(t, base, f.aliceTok)
		person.open()
		// Alice is in #general. Activity that the channel events socket
		// would send her: a message, a new channel she is added to, a
		// session (a roster), a rename.
		post := `{"body":"hello channel"}`
		if rec := f.do(t, "POST", "/v2/channels/general/messages", f.bobTok, post); rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
			t.Fatalf("post = %d %s", rec.Code, rec.Body)
		}
		if rec := f.do(t, "POST", "/v1/channels", f.rootTok, `{"name":"newroom","visibility":"public"}`); rec.Code >= 300 {
			t.Fatalf("create channel = %d %s", rec.Code, rec.Body)
		}
		f.create(t, f.aliceTok, humanBody)
		f.srv.events.notifyChannelsChanged(f.alice.ID)
		f.srv.events.notifyPermissionsChanged(f.alice.ID)
		f.srv.events.notifyDirectoryChanged(true)
		f.srv.events.notifyRosters()
		f.srv.events.notifyMessageEdited(f.general.ID, 1)
		f.srv.events.notifyReadState(f.general.ID, f.alice.ID)

		// A channel socket beside it opens and gets its own roster: the two
		// are separate streams. Then a marker, which must be the next thing
		// on the person socket.
		ch := dialEvents(t, base, "/v1/events/ws?channel=general", f.aliceTok)
		ch.next()
		if err := f.srv.person.publish(f.alice.ID, changedDoc(77)); err != nil {
			t.Fatal(err)
		}
		person.expectChanged(77)
	})
}

// TestPersonStreamClosesWhenAccessIsLost: disabling the person, revoking the
// credential and the rest close the socket with 1008 and the credential
// reason, at once for the calls that drop sockets and within the re-check
// interval for the rest. Another person's socket is not disturbed, and
// neither is the same person's through another credential.
func TestPersonStreamClosesWhenAccessIsLost(t *testing.T) {
	tests := []struct {
		name string
		act  func(t *testing.T, f *runFixture)
	}{
		{"credential revoked through the API", func(t *testing.T, f *runFixture) {
			id := f.credentialID(t, f.bobTok)
			if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", id), f.rootTok, ""); rec.Code != http.StatusNoContent {
				t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
			}
		}},
		{"credential revoked behind the server's back, found by the re-check", func(t *testing.T, f *runFixture) {
			if err := f.srv.store.RevokeCredential(context.Background(), "system", f.credentialID(t, f.bobTok)); err != nil {
				t.Fatal(err)
			}
		}},
		{"principal disabled through the API", func(t *testing.T, f *runFixture) {
			if rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/disable", f.bob.ID), f.rootTok, ""); rec.Code != http.StatusOK {
				t.Fatalf("disable = %d %s", rec.Code, rec.Body)
			}
		}},
		{"principal disabled behind the server's back, found by the re-check", func(t *testing.T, f *runFixture) {
			if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.bob.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"every credential revoked", func(t *testing.T, f *runFixture) {
			if rec := f.do(t, "POST", fmt.Sprintf("/v1/principals/%d/credentials/revoke-all", f.bob.ID), f.rootTok, ""); rec.Code != http.StatusOK {
				t.Fatalf("revoke-all = %d %s", rec.Code, rec.Body)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRunFixture(t)
			f.srv.credRecheckInterval = 20 * time.Millisecond // nothing sleeps: the read returns on close
			base := wsTestServer(t, f.srv)
			alice, bob := dialPerson(t, base, f.aliceTok), dialPerson(t, base, f.bobTok)
			alice.open()
			bob.open()

			tt.act(t, f)
			ce := bob.closed()
			if ce.Code != websocket.StatusPolicyViolation || ce.Reason != reasonCredentialInvalid {
				t.Errorf("bob's socket closed with %d %q, want 1008 %q", ce.Code, ce.Reason, reasonCredentialInvalid)
			}
			if err := f.srv.person.publish(f.alice.ID, changedDoc(1)); err != nil {
				t.Fatal(err)
			}
			alice.expectChanged(1)
		})
	}

	t.Run("one credential revoked leaves the person's other socket", func(t *testing.T) {
		f := newRunFixture(t)
		base := wsTestServer(t, f.srv)
		one, two := dialPerson(t, base, f.aliceTok), dialPerson(t, base, f.aliceTok2)
		one.open()
		two.open()
		id := f.credentialID(t, f.aliceTok2)
		if rec := f.do(t, "DELETE", fmt.Sprintf("/v1/credentials/%d", id), f.rootTok, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
		}
		if ce := two.closed(); ce.Reason != reasonCredentialInvalid {
			t.Errorf("revoked credential's socket closed with %q", ce.Reason)
		}
		if err := f.srv.person.publish(f.alice.ID, changedDoc(1)); err != nil {
			t.Fatal(err)
		}
		one.expectChanged(1)
	})

	t.Run("a frame queued before a revoke is not sent after it", func(t *testing.T) {
		f := newRunFixture(t)
		base := wsTestServer(t, f.srv)
		bob := dialPerson(t, base, f.bobTok)
		bob.open()
		// Behind the server's back, so no drop has run; the batch re-check
		// is what must refuse.
		if err := f.srv.store.RevokeCredential(context.Background(), "system", f.credentialID(t, f.bobTok)); err != nil {
			t.Fatal(err)
		}
		if err := f.srv.person.publish(f.bob.ID, changedDoc(1)); err != nil {
			t.Fatal(err)
		}
		if ce := bob.closed(); ce.Reason != reasonCredentialInvalid {
			t.Errorf("closed with %q", ce.Reason)
		}
	})
}

// TestPersonHub: the hub on its own, as the socket uses it.
func TestPersonHub(t *testing.T) {
	t.Run("a socket that falls too far behind is dropped, the writer is not blocked", func(t *testing.T) {
		h := newPersonHub()
		slow := h.subscribe(1, 10, "slow")
		other := h.subscribe(1, 11, "other")
		for i := 0; i < personQueueMax; i++ {
			if err := h.publish(1, changedDoc(int64(i+1))); err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				other.take() // this one keeps up
			}
		}
		select {
		case <-slow.dropped:
			t.Fatal("dropped at the limit, not past it")
		default:
		}
		if err := h.publish(1, changedDoc(1)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-slow.dropped:
		default:
			t.Fatal("a full queue was not dropped")
		}
		if _, live := slow.take(); live {
			t.Error("a dropped subscription is live")
		}
		if _, live := other.take(); !live {
			t.Error("the other subscription was dropped")
		}
	})

	t.Run("drops are by principal or by credential, and a zero matches nothing", func(t *testing.T) {
		h := newPersonHub()
		a1, a2, b := h.subscribe(1, 10, "a1"), h.subscribe(1, 11, "a2"), h.subscribe(2, 12, "b")
		h.dropPrincipalAll(0)
		h.dropCredential(0)
		h.dropCredential(11)
		if _, live := a2.take(); live {
			t.Error("credential 11 not dropped")
		}
		if _, live := a1.take(); !live {
			t.Error("credential 10 dropped by 11's revoke")
		}
		h.dropPrincipalAll(1)
		if _, live := a1.take(); live {
			t.Error("principal 1 not dropped")
		}
		if _, live := b.take(); !live {
			t.Error("principal 2 dropped by principal 1's drop")
		}
	})

	t.Run("shutdown drops everyone and refuses new sockets", func(t *testing.T) {
		h := newPersonHub()
		a := h.subscribe(1, 10, "a")
		h.closeAll()
		if _, live := a.take(); live {
			t.Error("open socket survived shutdown")
		}
		late := h.subscribe(1, 10, "late")
		select {
		case <-late.dropped:
		default:
			t.Error("a socket opened after shutdown is live")
		}
		if !h.isClosed() {
			t.Error("not closed")
		}
	})
}
