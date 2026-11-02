package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #981, the review of PR #1143: each of the community model's writes is
// one transaction. A failure injected at a later step (a trigger installed in
// the test database underneath the store) must leave nothing of the earlier
// ones, and the daily bounds hold when the creations race.

// injectFailure installs trigger, which must RAISE(ABORT) on the step it
// names, and drops it when the test ends or when the returned func is
// called.
func injectFailure(t *testing.T, srv *Server, name, trigger string) (drop func()) {
	t.Helper()
	legacyExec(t, srv, trigger)
	dropped := false
	drop = func() {
		if !dropped {
			dropped = true
			legacyExec(t, srv, "DROP TRIGGER IF EXISTS "+name)
		}
	}
	t.Cleanup(drop)
	return drop
}

// failOnAudit is a trigger failing the insert of an audit row of action.
func failOnAudit(name, action string) string {
	return fmt.Sprintf(`CREATE TRIGGER %s BEFORE INSERT ON audit_events WHEN NEW.action = '%s'
		BEGIN SELECT RAISE(ABORT, 'injected failure'); END`, name, action)
}

// channelExists reports whether a channel is called name.
func channelExists(t *testing.T, srv *Server, name string) bool {
	t.Helper()
	_, err := srv.store.ChannelByName(context.Background(), name)
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		return false
	default:
		t.Fatal(err)
		return false
	}
}

// TestCreateChannelUnownedIsOneTransaction: with authentication off, a public
// channel is created with its visibility and its audit row in one
// transaction. A failure at either later step leaves no channel, so the
// name is free: the retry makes the channel public, where two transactions
// left a private channel holding the name and the retry a 409.
func TestCreateChannelUnownedIsOneTransaction(t *testing.T) {
	for _, tc := range []struct{ name, trigger string }{
		{"the visibility write fails", `CREATE TRIGGER inject_vis BEFORE UPDATE OF visibility ON channels
			BEGIN SELECT RAISE(ABORT, 'injected failure'); END`},
		{"the audit row fails", failOnAudit("inject_vis", store.AuditChannelCreated)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthFixture(t, AuthOff)
			drop := injectFailure(t, f.srv, "inject_vis", tc.trigger)
			body := createChannelV1Body("town-hall", schema.ChannelPublic)
			if rec := f.do(t, http.MethodPost, "/v1/channels", "", body); rec.Code != http.StatusInternalServerError {
				t.Fatalf("create with the failure = %d %s, want 500", rec.Code, rec.Body)
			}
			if channelExists(t, f.srv, "town-hall") {
				t.Fatal("a failed create left a channel holding the name")
			}
			for _, e := range f.audit(t) {
				if e.Action == store.AuditChannelCreated && strings.Contains(e.Detail, "town-hall") {
					t.Errorf("a failed create was audited: %+v", e)
				}
			}
			drop()
			rec := f.do(t, http.MethodPost, "/v1/channels", "", body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("retry = %d %s, want 201", rec.Code, rec.Body)
			}
			if got := decodeBody[schema.CreateChannelResponseV1](t, rec).Channel; got.Visibility != schema.ChannelPublic {
				t.Errorf("retry made %+v, want public", got)
			}
			ch, err := f.srv.store.ChannelByName(context.Background(), "town-hall")
			if err != nil || ch.Visibility != schema.ChannelPublic {
				t.Errorf("stored %+v, %v; want public", ch, err)
			}
			found := false
			for _, e := range f.audit(t) {
				if e.Action == store.AuditChannelCreated && e.Detail == `name="town-hall" visibility=public` {
					found = true
				}
			}
			if !found {
				t.Error("no channel_created row naming the visibility")
			}
		})
	}
}

// TestCreateChannelV1RollsBack: a member's creation writes the channel, its
// visibility, channel_created, the creator's membership and its two audit
// rows in one transaction. A failure at any later step leaves no channel, so
// no channel is ever without its creator, and the name stays free.
func TestCreateChannelV1RollsBack(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, trigger string }{
		{"the visibility write fails", `CREATE TRIGGER inject_create BEFORE UPDATE OF visibility ON channels
			BEGIN SELECT RAISE(ABORT, 'injected failure'); END`},
		{"channel_created fails", failOnAudit("inject_create", store.AuditChannelCreated)},
		{"the creator's membership fails", `CREATE TRIGGER inject_create BEFORE INSERT ON channel_members
			BEGIN SELECT RAISE(ABORT, 'injected failure'); END`},
		{"member_added fails", failOnAudit("inject_create", store.AuditMemberAdded)},
		{"member_manage_granted fails", failOnAudit("inject_create", store.AuditMemberManageGranted)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChannelV1Fixture(t)
			before := len(f.audit(t))
			drop := injectFailure(t, f.srv, "inject_create", tc.trigger)
			body := createChannelV1Body("hearth", schema.ChannelPublic)
			if rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, body); rec.Code != http.StatusInternalServerError {
				t.Fatalf("create with the failure = %d %s, want 500", rec.Code, rec.Body)
			}
			if channelExists(t, f.srv, "hearth") {
				t.Fatal("a failed create left a channel")
			}
			for _, e := range f.audit(t)[before:] {
				t.Errorf("a failed create wrote an audit row: %+v", e)
			}
			drop()
			rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("retry = %d %s, want 201", rec.Code, rec.Body)
			}
			id := decodeBody[schema.CreateChannelResponseV1](t, rec).Channel.ID
			members, err := f.srv.store.ListChannelMembers(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if len(members) != 1 || members[0].PrincipalID != f.member.ID || !members[0].CanDecide || !members[0].Manages {
				t.Errorf("after the retry members = %+v", members)
			}
		})
	}
}

// TestChannelSettingsRollsBack: unmarking the last default channel, turning
// it private and turning open sign-up off are one transaction. A failure at
// a later step (the sign-up write, its audit row, or the settings row)
// leaves the channel public and default and sign-up on, with no audit row.
func TestChannelSettingsRollsBack(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, trigger string }{
		{"the sign-up write fails", `CREATE TRIGGER inject_settings BEFORE UPDATE ON server_settings WHEN NEW.key = 'open_signup'
			BEGIN SELECT RAISE(ABORT, 'injected failure'); END`},
		{"the sign-up row fails", failOnAudit("inject_settings", store.AuditJoinSettingsChanged)},
		{"the settings row fails", failOnAudit("inject_settings", store.AuditChannelSettingsChanged)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newChannelV1Fixture(t)
			if rec := f.putSettings(t, f.pubJoined.ID, `{"default":true}`); rec.Code != http.StatusOK {
				t.Fatalf("mark default = %d %s", rec.Code, rec.Body)
			}
			legacyExec(t, f.srv, `INSERT INTO server_settings (key, value, updated_at) VALUES ('open_signup', 'true', 0)
				ON CONFLICT (key) DO UPDATE SET value = 'true'`)
			before := len(f.audit(t))
			drop := injectFailure(t, f.srv, "inject_settings", tc.trigger)
			body := `{"default":false,"visibility":"private"}`
			if rec := f.putSettings(t, f.pubJoined.ID, body); rec.Code != http.StatusInternalServerError {
				t.Fatalf("settings with the failure = %d %s, want 500", rec.Code, rec.Body)
			}
			ch, err := f.srv.store.ChannelByID(ctx, f.pubJoined.ID)
			if err != nil || ch.Visibility != schema.ChannelPublic || !ch.IsDefault {
				t.Errorf("after the failure town-square = %+v, %v; want public and default", ch, err)
			}
			set, _, err := f.srv.store.JoinSettings(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if set.OpenSignup == nil || !*set.OpenSignup {
				t.Error("after the failure open sign-up is off")
			}
			for _, e := range f.audit(t)[before:] {
				t.Errorf("a failed change wrote an audit row: %+v", e)
			}
			drop()
			rec := f.putSettings(t, f.pubJoined.ID, body)
			if rec.Code != http.StatusOK || !decodeBody[schema.ChannelSettingsResponseV1](t, rec).OpenSignupTurnedOff {
				t.Fatalf("retry = %d %s, want 200 turning sign-up off", rec.Code, rec.Body)
			}
		})
	}
}

// TestCreateChannelV1ConcurrentBounds: a capped member's creations racing
// each other are bounded as sequential ones are. Of 25 parallel creations of
// new names, createsPerDay succeed; of 40 parallel collisions,
// createAttemptsPerDay are answered 409 and recorded, and the rest are 429.
func TestCreateChannelV1ConcurrentBounds(t *testing.T) {
	race := func(t *testing.T, f *channelV1Fixture, n int, name func(i int) string) map[int]int {
		t.Helper()
		var mu sync.Mutex
		statuses := map[int]int{}
		var wg sync.WaitGroup
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body(name(i), schema.ChannelPublic))
				mu.Lock()
				statuses[rec.Code]++
				mu.Unlock()
			}()
		}
		wg.Wait()
		return statuses
	}
	actor := func(f *channelV1Fixture) string { return fmt.Sprintf("principal:%d", f.member.ID) }
	t.Run("creations", func(t *testing.T) {
		f := newChannelV1Fixture(t)
		got := race(t, f, 25, func(i int) string { return fmt.Sprintf("race-room-%d", i) })
		if got[http.StatusCreated] != createsPerDay || got[http.StatusTooManyRequests] != 25-createsPerDay {
			t.Errorf("statuses = %v, want %d created and %d refused 429", got, createsPerDay, 25-createsPerDay)
		}
		made := 0
		for _, e := range f.audit(t) {
			if e.Actor == actor(f) && e.Action == store.AuditChannelCreated {
				made++
			}
		}
		if made != createsPerDay {
			t.Errorf("channel_created rows = %d, want %d", made, createsPerDay)
		}
	})
	t.Run("attempts", func(t *testing.T) {
		f := newChannelV1Fixture(t)
		got := race(t, f, 40, func(int) string { return "channel-a" })
		if got[http.StatusConflict] != createAttemptsPerDay || got[http.StatusTooManyRequests] != 40-createAttemptsPerDay {
			t.Errorf("statuses = %v, want %d 409 and %d 429", got, createAttemptsPerDay, 40-createAttemptsPerDay)
		}
		refused := 0
		for _, e := range f.audit(t) {
			if e.Actor == actor(f) && e.Action == store.AuditChannelCreateRefused {
				refused++
			}
		}
		if refused != createAttemptsPerDay {
			t.Errorf("channel_create_refused rows = %d, want %d", refused, createAttemptsPerDay)
		}
	})
}
