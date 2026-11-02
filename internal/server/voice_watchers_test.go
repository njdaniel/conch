package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// TestPutUseVoiceOffClosesWatchers: a member who watches voice without being
// in a room (a presence socket on a channel, the occupancy socket) is asked
// again too: when the PUT answers, their subscriptions are gone and both
// sockets are closed, while a watcher whose role keeps use_voice is not
// touched (design §7.5; the reviews of #1016).
func TestPutUseVoiceOffClosesWatchers(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	f.addPerson(t, "op1", true)
	f.srv.credRecheckInterval = time.Hour // the hook closes them, not the re-check
	base := wsTestServer(t, f.srv)
	presence, _, err := dialVoice(t, base, "ops", f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	readPresence(t, presence)
	occupancy, _, err := dialOccupancy(t, base, f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	readOccupancy(t, occupancy)
	opPresence, _, err := dialVoice(t, base, "ops", f.tokens["op1"])
	if err != nil {
		t.Fatal(err)
	}
	readPresence(t, opPresence)

	res := f.putPermissionsREST(t, schema.PutPermissionsRequestV1{Revision: f.revision(t), Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
		{Permission: schema.PermUseVoice, Role: schema.PermissionRoleMember, Granted: false},
	}})
	if res.code != http.StatusOK {
		t.Fatalf("the PUT = %d %s", res.code, res.body)
	}
	f.srv.voice.mu.Lock()
	kept := 0
	for sub := range f.srv.voice.subs {
		switch sub.principalID {
		case f.ids["ann"]:
			t.Error("ann's presence subscription outlived the PUT")
		case f.ids["op1"]:
			kept++
		}
	}
	for sub := range f.srv.voice.occSubs {
		if sub.principalID == f.ids["ann"] {
			t.Error("ann's occupancy subscription outlived the PUT")
		}
	}
	f.srv.voice.mu.Unlock()
	if kept != 1 {
		t.Errorf("the operator's presence subscriptions = %d, want 1", kept)
	}
	for name, conn := range map[string]*websocket.Conn{"presence": presence, "occupancy": occupancy} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, raw, err := conn.Read(ctx); err == nil {
			t.Errorf("ann's %s socket sent a frame after the PUT: %s", name, raw)
		}
		cancel()
	}
}

// Without LiveKit the same PUT still closes the watchers: the drops come
// before anything that needs LiveKit.
func TestPutUseVoiceOffClosesWatchersWithoutLiveKit(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired, unconfigured: true})
	sub := f.srv.voice.subscribeOccupancy(f.ids["ann"], f.credID(t, "ann"))
	if sub.gone {
		t.Fatal("setup: the subscription was refused")
	}
	res := f.putPermissionsREST(t, schema.PutPermissionsRequestV1{Revision: f.revision(t), Changes: []schema.PermissionChangeV1{
		{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
		{Permission: schema.PermUseVoice, Role: schema.PermissionRoleMember, Granted: false},
	}})
	if res.code != http.StatusOK {
		t.Fatalf("the PUT = %d %s", res.code, res.body)
	}
	f.srv.voice.mu.Lock()
	defer f.srv.voice.mu.Unlock()
	if _, ok := f.srv.voice.occSubs[sub]; ok {
		t.Error("the subscription outlived the PUT")
	}
}
