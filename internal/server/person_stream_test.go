package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The per-person events stream (issue #2040), against the real server and
// hubs, with no LiveKit. Every frame is decoded by its schema and validated;
// no test sleeps for timing: the re-check interval is injected, and "nothing
// arrived" is shown by a marker frame, published afterwards, being the next
// thing read.

const personPath = "/v1/events/person/ws"

// newPersonFixture supplies the provenance boundary until the shared store
// guard is available. Each test that delivers DM frames installs its own
// access rule.
func newPersonFixture(t *testing.T) *runFixture {
	t.Helper()
	f := newRunFixture(t)
	f.srv.person.ownSignIn = func(context.Context, int64, int64) (bool, error) { return true, nil }
	return f
}

func allowPersonDMFixture(context.Context, int64, int64) (bool, error) {
	return true, nil
}

// personClient is one open person socket.
type personClient struct {
	t    *testing.T
	conn *websocket.Conn
}

// tryDialPerson dials the person stream and returns the socket, or the status
// the server refused the upgrade with.
func tryDialPerson(t *testing.T, base, path, token string) (*personClient, int, string) {
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
		var body schema.Error
		if resp.Body != nil {
			defer func() { _ = resp.Body.Close() }()
			_ = json.NewDecoder(resp.Body).Decode(&body)
		}
		return nil, resp.StatusCode, body.Code
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &personClient{t: t, conn: conn}, http.StatusSwitchingProtocols, ""
}

func dialPerson(t *testing.T, base, token string) *personClient {
	t.Helper()
	c, status, code := tryDialPerson(t, base, personPath, token)
	if c == nil {
		t.Fatalf("person stream refused with %d %s", status, code)
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

// closed requires a close as the next frame.
func (c *personClient) closed() websocket.CloseError {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), eventsReadTimeout)
	defer cancel()
	_, raw, err := c.conn.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		c.t.Fatalf("read %q, error %v, want a close frame", raw, err)
	}
	return ce
}

func changedDoc(reader, conversation int64) readerDMChange {
	return readerDMChange{
		DMChangedV1: schema.DMChangedV1{Schema: schema.DMChangedSchemaV1, ConversationID: conversation, Kind: schema.DMChangeRead},
		readerID:    reader,
	}
}

// dmMessageDoc is message id of conversation conv, as of revision rev.
func dmMessageDoc(conv, id, rev int64) schema.DMMessageV1 {
	return schema.DMMessageV1{
		Schema: schema.DMMessageSchemaV1, ID: id, ConversationID: conv, AuthorID: 5, Revision: rev,
		CreatedAt: schema.NewTimestamp(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)), Body: "hi",
	}
}

func dmEditedDoc(conv, id, rev int64) schema.DMMessageEditedV1 {
	m := dmMessageDoc(conv, id, rev)
	at := schema.NewTimestamp(time.Date(2026, 11, 19, 9, 5, 0, 0, time.UTC))
	m.EditedAt = &at
	return schema.DMMessageEditedV1{Schema: schema.DMMessageEditedSchemaV1, Message: m}
}

func dmRemovedDoc(conv, id, rev int64) schema.DMMessageRemovedV1 {
	m := dmMessageDoc(conv, id, rev)
	m.Body, m.Removed = schema.MessageRemovedByAuthorBody, true
	return schema.DMMessageRemovedV1{Schema: schema.DMMessageRemovedSchemaV1, Message: m}
}

// snapshotConv is a conversation as a reader sees it,
// at revision rev with its last message lastID.
func snapshotConv(reader, peer, id, rev, lastID int64) schema.DMConversationV1 {
	at := schema.NewTimestamp(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC))
	pair := []int64{reader, peer}
	slices.Sort(pair)
	return schema.DMConversationV1{
		Schema: schema.DMConversationSchemaV1, ID: id, ParticipantIDs: pair, CreatedAt: &at,
		PeerID: peer, State: schema.DMStateOpen, Revision: rev, LastMessageID: lastID, LastMessageAt: &at,
	}
}

// expectMessage reads one frame and expects it to be a dm.message with this
// id and revision.
func (c *personClient) expectMessage(id, rev int64) {
	c.t.Helper()
	raw := c.raw()
	var m schema.DMMessageV1
	if personSchemaOf(c.t, raw) != schema.DMMessageSchemaV1 || json.Unmarshal(raw, &m) != nil || m.ID != id || m.Revision != rev {
		c.t.Fatalf("next frame is %s, want message %d at revision %d", raw, id, rev)
	}
}

// TestPersonStreamWhoMayOpen: a person's own live credential opens it, with a
// bearer or a ticket; an agent, a guest, a dead or missing credential and a
// server with authentication off do not, and nothing is upgraded for them.
func TestPersonStreamWhoMayOpen(t *testing.T) {
	f := newPersonFixture(t)
	g := newGuest(t, f.srv, "Pat", time.Hour, 2*time.Hour, f.general.ID)
	revoked := f.credentialID(t, f.aliceTok2)
	if err := f.srv.store.RevokeCredential(context.Background(), "system", revoked); err != nil {
		t.Fatal(err)
	}
	base := wsTestServer(t, f.srv)

	ticket := func(token string) string {
		rec := f.do(t, "POST", "/v1/ws-tickets", token, "")
		if rec.Code != http.StatusOK && rec.Code != http.StatusCreated {
			t.Fatalf("ticket = %d %s", rec.Code, rec.Body)
		}
		var tk schema.WSTicketV1
		if err := json.Unmarshal(rec.Body.Bytes(), &tk); err != nil {
			t.Fatal(err)
		}
		return tk.Ticket
	}

	ctx := context.Background()
	actor := fmt.Sprintf("principal:%d", f.root.ID)
	minted, mintedToken, err := f.srv.store.CreateCredential(ctx, actor, f.alice.ID, "operator issued", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	link, err := f.srv.store.CreateSignInLink(ctx, actor, f.root.ID, f.alice.ID, schema.CreateSignInLinkRequestV1{}, now)
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := f.srv.store.RedeemSignInLink(ctx, store.HashSignInLinkCode(link.Code), now)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.person.ownSignIn = func(_ context.Context, principalID, credID int64) (bool, error) {
		switch credID {
		case minted.ID, redeemed.Credential.ID:
			if principalID != f.alice.ID {
				t.Errorf("provenance checked principal %d, want %d", principalID, f.alice.ID)
			}
			return false, nil
		default:
			return true, nil
		}
	}

	tests := []struct {
		name   string
		path   string
		token  string
		status int
		code   string
	}{
		{"a person, bearer", personPath, f.aliceTok, http.StatusSwitchingProtocols, ""},
		{"another person", personPath, f.bobTok, http.StatusSwitchingProtocols, ""},
		{"a person, ticket", personPath + "?ticket=" + ticket(f.aliceTok), "", http.StatusSwitchingProtocols, ""},
		{"an operator is a person too", personPath, f.rootTok, http.StatusSwitchingProtocols, ""},
		{"an agent", personPath, f.botTok, http.StatusForbidden, "forbidden"},
		{"another agent", personPath, f.rivalTok, http.StatusForbidden, "forbidden"},
		{"a guest", personPath, g.Token, http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
		{"no credential", personPath, "", http.StatusUnauthorized, ""},
		{"a made-up credential", personPath, "not-a-token", http.StatusUnauthorized, ""},
		{"a revoked credential", personPath, f.aliceTok2, http.StatusUnauthorized, ""},
		{"operator-issued bearer", personPath, mintedToken, http.StatusForbidden, schema.ErrorCodeOwnSignInRequired},
		{"operator-issued ticket", personPath + "?ticket=" + ticket(mintedToken), "", http.StatusForbidden, schema.ErrorCodeOwnSignInRequired},
		{"other-created link bearer", personPath, redeemed.Token, http.StatusForbidden, schema.ErrorCodeOwnSignInRequired},
		{"other-created link ticket", personPath + "?ticket=" + ticket(redeemed.Token), "", http.StatusForbidden, schema.ErrorCodeOwnSignInRequired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, status, code := tryDialPerson(t, base, tt.path, tt.token)
			if status != tt.status {
				t.Fatalf("status = %d, want %d", status, tt.status)
			}
			if tt.code != "" && code != tt.code {
				t.Fatalf("code = %q, want %q", code, tt.code)
			}
			if tt.status == http.StatusSwitchingProtocols {
				c.open()
			}
		})
	}

	t.Run("missing provenance guard refuses every upgrade", func(t *testing.T) {
		f := newRunFixture(t)
		if _, status, code := tryDialPerson(t, wsTestServer(t, f.srv), personPath, f.aliceTok); status != http.StatusForbidden || code != schema.ErrorCodeOwnSignInRequired {
			t.Fatalf("status and code = %d %q, want 403 own_sign_in_required", status, code)
		}
	})

	t.Run("a disabled person", func(t *testing.T) {
		f := newPersonFixture(t)
		if _, err := f.srv.store.DisablePrincipal(context.Background(), "system", f.bob.ID); err != nil {
			t.Fatal(err)
		}
		_, status, _ := tryDialPerson(t, wsTestServer(t, f.srv), personPath, f.bobTok)
		if status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	})

	t.Run("authentication off has no person", func(t *testing.T) {
		off := newAuthFixture(t, AuthOff)
		_, status, _ := tryDialPerson(t, wsTestServer(t, off.srv), personPath, "")
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
	f := newPersonFixture(t)
	base := wsTestServer(t, f.srv)

	t.Run("two sockets of one person have two instance ids", func(t *testing.T) {
		a := dialPerson(t, base, f.aliceTok)
		b := dialPerson(t, base, f.aliceTok) // the same credential, as two tabs have
		helloA, snapA := a.open()
		helloB, _ := b.open()
		if helloA.InstanceID == helloB.InstanceID {
			t.Fatalf("both sockets were given instance id %s", helloA.InstanceID)
		}
		if len(snapA.Conversations) != 0 || snapA.DMsEnabled {
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
		f.srv.dmSnapshotSource = func(_ context.Context, id int64) (personDMSnapshot, error) {
			asked = append(asked, id)
			s, _ := f.srv.emptyDMSnapshot(context.Background(), id)
			s.DMsEnabled = true
			return personDMSnapshot{doc: s}, nil
		}
		t.Cleanup(func() { f.srv.dmSnapshotSource = nil })
		_, snap := dialPerson(t, base, f.bobTok).open()
		if len(asked) != 1 || asked[0] != f.bob.ID {
			t.Errorf("source asked for %v, want only bob %d", asked, f.bob.ID)
		}
		if !snap.DMsEnabled {
			t.Errorf("snapshot = %+v", snap)
		}
	})

	t.Run("a source that fails or lies is a 500, not an upgrade", func(t *testing.T) {
		for name, src := range map[string]dmSnapshotSource{
			"error": func(context.Context, int64) (personDMSnapshot, error) {
				return personDMSnapshot{}, errors.New("boom")
			},
			"invalid": func(context.Context, int64) (personDMSnapshot, error) { return personDMSnapshot{}, nil },
		} {
			f.srv.dmSnapshotSource = src
			_, status, _ := tryDialPerson(t, base, personPath, f.aliceTok)
			if status != http.StatusInternalServerError {
				t.Errorf("%s: status = %d, want 500", name, status)
			}
		}
		f.srv.dmSnapshotSource = nil
	})
}

func TestPersonStreamSnapshotPrivacy(t *testing.T) {
	tests := []struct {
		name   string
		rows   func(f *runFixture) (schema.DMConversationV1, map[int64]int64)
		status int
	}{
		{"peer's read projection", func(f *runFixture) (schema.DMConversationV1, map[int64]int64) {
			row := snapshotConv(f.bob.ID, f.alice.ID, 12, 1, 1)
			row.LastReadID = 1
			return row, nil
		}, http.StatusInternalServerError},
		{"nonparticipant", func(f *runFixture) (schema.DMConversationV1, map[int64]int64) {
			return snapshotConv(f.bob.ID, f.carol.ID, 12, 1, 1), nil
		}, http.StatusInternalServerError},
		{"peer's private open projected for caller", func(f *runFixture) (schema.DMConversationV1, map[int64]int64) {
			row := snapshotConv(f.alice.ID, f.bob.ID, 12, 0, 0)
			row.CreatedAt, row.LastMessageAt = nil, nil
			return row, map[int64]int64{row.ID: f.bob.ID}
		}, http.StatusInternalServerError},
		{"caller's private open", func(f *runFixture) (schema.DMConversationV1, map[int64]int64) {
			row := snapshotConv(f.alice.ID, f.bob.ID, 12, 0, 0)
			row.CreatedAt, row.LastMessageAt = nil, nil
			return row, map[int64]int64{row.ID: f.alice.ID}
		}, http.StatusSwitchingProtocols},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPersonFixture(t)
			row, openers := tt.rows(f)
			f.srv.dmSnapshotSource = func(ctx context.Context, id int64) (personDMSnapshot, error) {
				doc, err := f.srv.emptyDMSnapshot(ctx, id)
				doc.Conversations = []schema.DMConversationV1{row}
				return personDMSnapshot{doc: doc, privateOpeners: openers}, err
			}
			c, status, _ := tryDialPerson(t, wsTestServer(t, f.srv), personPath, f.aliceTok)
			if status != tt.status {
				t.Fatalf("status = %d, want %d", status, tt.status)
			}
			if c != nil {
				_, snap := c.open()
				if len(snap.Conversations) != 1 || snap.Conversations[0].ID != row.ID {
					t.Fatalf("snapshot = %+v, want the reader's private open", snap)
				}
			}
		})
	}
}

func TestPersonStreamRechecksAfterSnapshotBuild(t *testing.T) {
	f := newPersonFixture(t)
	credID := f.credentialID(t, f.aliceTok)
	f.srv.dmSnapshotSource = func(ctx context.Context, id int64) (personDMSnapshot, error) {
		// Revoke directly in the store, so there's no subscription drop.
		if err := f.srv.store.RevokeCredential(ctx, "system", credID); err != nil {
			return personDMSnapshot{}, err
		}
		doc, err := f.srv.emptyDMSnapshot(ctx, id)
		doc.Conversations = []schema.DMConversationV1{snapshotConv(id, f.bob.ID, 12, 1, 1)}
		return personDMSnapshot{doc: doc}, err
	}
	if _, status, _ := tryDialPerson(t, wsTestServer(t, f.srv), personPath, f.aliceTok); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

// TestPersonStreamSnapshotOrdering: a frame published while the snapshot is
// being taken is held and sent after it, unless the snapshot already reflects
// it (a revision at or below the one the snapshot reported for that
// conversation), so nothing falls between the two. The ordering is per
// conversation: message ids of two conversations are not compared.
func TestPersonStreamSnapshotOrdering(t *testing.T) {
	f := newPersonFixture(t)
	f.srv.dmFrameAccess = allowPersonDMFixture
	base := wsTestServer(t, f.srv)
	f.srv.dmSnapshotSource = func(ctx context.Context, id int64) (personDMSnapshot, error) {
		// Conversation 12 is at revision 41 in the snapshot, 13 at 3; 14 is not
		// listed (hidden, or past More), so all of its frames are sent.
		for _, d := range []personDoc{
			dmMessageDoc(12, 40, 40), dmMessageDoc(12, 41, 41), // reflected
			dmEditedDoc(12, 40, 41),  // an edit at the snapshot's revision: reflected
			dmRemovedDoc(13, 900, 3), // reflected, though its id is above 12's revisions
			dmMessageDoc(12, 42, 42), // above: sent
			dmEditedDoc(13, 900, 4),  // above: sent
			dmRemovedDoc(12, 40, 43), // above: sent
			dmMessageDoc(14, 7, 1),   // conversation not in the snapshot: sent
			changedDoc(id, 12),       // a signal has no revision: sent
		} {
			if err := f.srv.person.publish(id, d); err != nil {
				t.Errorf("publish: %v", err)
			}
		}
		s, _ := f.srv.emptyDMSnapshot(ctx, id)
		s.Conversations = []schema.DMConversationV1{snapshotConv(id, f.bob.ID, 12, 41, 41), snapshotConv(id, f.bob.ID, 13, 3, 900)}
		return personDMSnapshot{doc: s}, nil
	}
	c := dialPerson(t, base, f.aliceTok)
	c.open()
	c.expectMessage(42, 42)
	for _, want := range []struct {
		schema string
		id     int64
	}{{schema.DMMessageEditedSchemaV1, 900}, {schema.DMMessageRemovedSchemaV1, 40}, {schema.DMMessageSchemaV1, 7}} {
		raw := c.raw()
		var head struct {
			Schema  string `json:"schema"`
			Message struct {
				ID int64 `json:"id"`
			} `json:"message"`
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatal(err)
		}
		if got := head.ID + head.Message.ID; head.Schema != want.schema || got != want.id {
			t.Fatalf("frame = %s, want %s of message %d", raw, want.schema, want.id)
		}
	}
	c.expectChanged(12) // nothing reflected by the snapshot was sent
}

func TestPersonStreamMissingAccessGateDropsDMs(t *testing.T) {
	f := newPersonFixture(t)
	f.srv.dmSnapshotSource = func(ctx context.Context, id int64) (personDMSnapshot, error) {
		if err := f.srv.person.publish(id, dmMessageDoc(12, 1, 1)); err != nil {
			return personDMSnapshot{}, err
		}
		doc, err := f.srv.emptyDMSnapshot(ctx, id)
		return personDMSnapshot{doc: doc}, err
	}
	c := dialPerson(t, wsTestServer(t, f.srv), f.aliceTok)
	c.open()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, raw, err := c.conn.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read %q, error %v, want no frame", raw, err)
	}
}

// TestPersonStreamDeliveryNeedsCurrentAccess: the rule is asked for every DM
// frame when it is written, for the person it is written to; a frame it
// refuses is dropped (a blocked pair gets none of the pair's frames, on any
// socket of either person, the author's own other devices included); a
// failing rule closes the socket rather than letting a frame through.
func TestPersonStreamDeliveryNeedsCurrentAccess(t *testing.T) {
	f := newPersonFixture(t)
	base := wsTestServer(t, f.srv)
	blocked := map[int64]bool{} // principal -> blocked from conversation 12
	f.srv.dmFrameAccess = func(_ context.Context, principal, conv int64) (bool, error) {
		if conv == 99 {
			return false, errors.New("store down")
		}
		return conv != 12 || ((principal == f.alice.ID || principal == f.bob.ID) && !blocked[principal]), nil
	}
	alice1, alice2, bob := dialPerson(t, base, f.aliceTok), dialPerson(t, base, f.aliceTok2), dialPerson(t, base, f.bobTok)
	for _, c := range []*personClient{alice1, alice2, bob} {
		c.open()
	}

	// Open: everyone gets it.
	for _, id := range []int64{f.alice.ID, f.bob.ID} {
		if err := f.srv.person.publish(id, dmMessageDoc(12, 1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	alice1.expectMessage(1, 1)
	alice2.expectMessage(1, 1)
	bob.expectMessage(1, 1)

	carol := dialPerson(t, base, f.carolTok)
	carol.open()
	for _, doc := range []personDoc{dmMessageDoc(12, 1, 1), dmMessageDoc(13, 2, 1)} {
		if err := f.srv.person.publish(f.carol.ID, doc); err != nil {
			t.Fatal(err)
		}
	}
	carol.expectMessage(2, 1)

	// A block holds: neither side is sent a frame of the pair, the author's
	// removal of their own message included, on all of the author's sockets.
	blocked[f.alice.ID], blocked[f.bob.ID] = true, true
	for _, id := range []int64{f.alice.ID, f.bob.ID} {
		for _, d := range []personDoc{dmRemovedDoc(12, 1, 2), changedDoc(id, 12), dmMessageDoc(13, 2, 1)} {
			if err := f.srv.person.publish(id, d); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Only the other conversation's frame arrives: the block is on one pair.
	alice1.expectMessage(2, 1)
	alice2.expectMessage(2, 1)
	bob.expectMessage(2, 1)

	// The block lifts: frames flow again.
	blocked[f.alice.ID], blocked[f.bob.ID] = false, false
	if err := f.srv.person.publish(f.alice.ID, dmMessageDoc(12, 3, 3)); err != nil {
		t.Fatal(err)
	}
	alice1.expectMessage(3, 3)
	alice2.expectMessage(3, 3)

	// A rule that fails closes the socket; the other person's is untouched.
	if err := f.srv.person.publish(f.bob.ID, dmMessageDoc(99, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if ce := bob.closed(); ce.Code != websocket.StatusInternalError {
		t.Errorf("a failed access check closed with %d %q, want 1011", ce.Code, ce.Reason)
	}
	if err := f.srv.person.publish(f.alice.ID, changedDoc(f.alice.ID, 5)); err != nil {
		t.Fatal(err)
	}
	alice1.expectChanged(5)
}

// TestPersonStreamCarriesOnlyFramesAddressedToIt: a frame goes to the sockets
// of the principal it was published for, all of them, and to no one else.
func TestPersonStreamCarriesOnlyFramesAddressedToIt(t *testing.T) {
	f := newPersonFixture(t)
	f.srv.dmFrameAccess = allowPersonDMFixture
	base := wsTestServer(t, f.srv)
	alice1, alice2 := dialPerson(t, base, f.aliceTok), dialPerson(t, base, f.aliceTok2)
	bob := dialPerson(t, base, f.bobTok)
	for _, c := range []*personClient{alice1, alice2, bob} {
		c.open()
	}
	hub := f.srv.person
	if err := hub.publish(f.alice.ID, changedDoc(f.alice.ID, 1)); err != nil {
		t.Fatal(err)
	}
	if err := hub.publish(f.bob.ID, changedDoc(f.bob.ID, 2)); err != nil {
		t.Fatal(err)
	}
	if err := hub.publish(f.alice.ID, changedDoc(f.alice.ID, 3)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []*personClient{alice1, alice2} {
		c.expectChanged(1)
		c.expectChanged(3)
	}
	bob.expectChanged(2) // not 1: alice's frames never reached him

	// Addressed to no one, or to no one connected: nothing breaks.
	for _, id := range []int64{0, -1} {
		if err := hub.publish(id, changedDoc(id, 4)); err == nil {
			t.Errorf("publish to principal %d succeeded", id)
		}
	}
	if err := hub.publish(f.carol.ID, changedDoc(f.carol.ID, 5)); err != nil {
		t.Errorf("publish to a principal with no socket: %v", err)
	}
}

func TestPersonHubRejectsMisaddressedAndUnknownChanges(t *testing.T) {
	hub := newPersonHub()
	alice := hub.subscribe(1, 10, "alice")
	bob := hub.subscribe(2, 20, "bob")
	for _, kind := range []schema.DMChangeKind{schema.DMChangeOpened, schema.DMChangeRead, schema.DMChangeState, schema.DMChangeParticipant} {
		doc := changedDoc(bob.principalID, 12)
		doc.Kind = kind
		if err := hub.publish(alice.principalID, doc); err == nil {
			t.Errorf("misaddressed %q was accepted", kind)
		}
		if err := hub.publish(bob.principalID, doc.DMChangedV1); err == nil {
			t.Errorf("unbound %q was accepted", kind)
		}
		if err := hub.publish(bob.principalID, doc); err != nil {
			t.Errorf("correctly addressed %q: %v", kind, err)
		}
	}
	for _, kind := range []schema.DMChangeKind{"blocked", "preference_changed"} {
		doc := changedDoc(bob.principalID, 12)
		doc.Kind = kind
		if err := hub.publish(bob.principalID, doc); err == nil {
			t.Errorf("unknown %q was accepted", kind)
		}
	}
	if got, _ := alice.take(); len(got) != 0 {
		t.Errorf("Alice received %d private changes", len(got))
	}
	if got, _ := bob.take(); len(got) != 4 {
		t.Errorf("Bob received %d changes, want 4", len(got))
	}
}

// TestPersonStreamNeverCarriesAChannelFrame: activity in the channels the
// person is in, and every channel document, stays off it. The registry refuses
// a document that is not a DM frame, and the socket has no channel to
// subscribe to.
func TestPersonStreamNeverCarriesAChannelFrame(t *testing.T) {
	t.Run("the registry refuses channel documents", func(t *testing.T) {
		f := newPersonFixture(t)
		f.srv.dmFrameAccess = allowPersonDMFixture
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
		if err := f.srv.person.publish(f.alice.ID, readerDMChange{}); err == nil || errors.Is(err, errPersonFrameKind) {
			t.Errorf("an invalid DM frame: publish = %v, want its validation error", err)
		}
	})

	t.Run("channel activity does not reach the socket", func(t *testing.T) {
		f := newPersonFixture(t)
		f.srv.dmFrameAccess = allowPersonDMFixture
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
		if err := f.srv.person.publish(f.alice.ID, changedDoc(f.alice.ID, 77)); err != nil {
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
			f := newPersonFixture(t)
			f.srv.dmFrameAccess = allowPersonDMFixture
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
			if err := f.srv.person.publish(f.alice.ID, changedDoc(f.alice.ID, 1)); err != nil {
				t.Fatal(err)
			}
			alice.expectChanged(1)
		})
	}

	t.Run("one credential revoked leaves the person's other socket", func(t *testing.T) {
		f := newPersonFixture(t)
		f.srv.dmFrameAccess = allowPersonDMFixture
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
		if err := f.srv.person.publish(f.alice.ID, changedDoc(f.alice.ID, 1)); err != nil {
			t.Fatal(err)
		}
		one.expectChanged(1)
	})

	t.Run("revoked during the access check for a pending frame", func(t *testing.T) {
		f := newPersonFixture(t)
		credID := f.credentialID(t, f.bobTok)
		f.srv.dmFrameAccess = func(ctx context.Context, _, conv int64) (bool, error) {
			if conv == 2 {
				// No hub drop runs. Only the check immediately before writing
				// can catch the revoke, after an earlier frame in the batch.
				err := f.srv.store.RevokeCredential(ctx, "system", credID)
				return err == nil, err
			}
			return true, nil
		}
		f.srv.dmSnapshotSource = func(ctx context.Context, id int64) (personDMSnapshot, error) {
			for _, conv := range []int64{1, 2} {
				if err := f.srv.person.publish(id, dmMessageDoc(conv, conv, 1)); err != nil {
					return personDMSnapshot{}, err
				}
			}
			doc, err := f.srv.emptyDMSnapshot(ctx, id)
			return personDMSnapshot{doc: doc}, err
		}
		bob := dialPerson(t, wsTestServer(t, f.srv), f.bobTok)
		bob.open()
		bob.expectMessage(1, 1)
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
			if err := h.publish(1, changedDoc(1, int64(i+1))); err != nil {
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
		if err := h.publish(1, changedDoc(1, 1)); err != nil {
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
