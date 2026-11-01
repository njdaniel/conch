package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #981 (community model I3a): a member creates a public or private
// channel and manages it; an operator changes a channel's visibility and its
// default mark.

// channelV1Fixture is a restrictedFixture with the five targets the route
// tests address and the caller classes they ask as: town-square, public, with
// member-1 in it and member-2 its manager; back-porch, public, which member-1
// is not in; channel-b, private, which member-1 is in; channel-a, private,
// which member-1 is not in; and a channel id that never existed. Pat is a
// live guest of town-square and Lee an ended one.
type channelV1Fixture struct {
	*restrictedFixture
	pubJoined, pubOther store.Channel
	guestTok, endedTok  string
}

func newChannelV1Fixture(t *testing.T) *channelV1Fixture {
	t.Helper()
	ctx := context.Background()
	f := &channelV1Fixture{restrictedFixture: newRestrictedFixture(t, Config{AuthMode: AuthRequired})}
	owner := fmt.Sprintf("principal:%d", f.root.ID)
	public := schema.ChannelPublic
	mk := func(name string) store.Channel {
		t.Helper()
		ch, err := f.srv.store.CreateChannelAs(ctx, owner, name, f.root.ID)
		if err != nil {
			t.Fatal(err)
		}
		res, err := f.srv.store.ChannelSettings(ctx, owner, f.root.ID, ch.ID, store.ChannelSettingsChange{Visibility: &public}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return res.Channel
	}
	f.pubJoined, f.pubOther = mk("town-square"), mk("back-porch")
	yes := true
	if _, err := f.srv.store.AddChannelMember(ctx, owner, f.pubJoined.ID, f.member.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.PutChannelMember(ctx, owner, f.pubJoined.ID, f.member2.ID, f.root.ID, &yes, &yes); err != nil {
		t.Fatal(err)
	}
	f.guestTok = newGuest(t, f.srv, "Pat", time.Hour, 2*time.Hour, f.pubJoined.ID).Token
	ended := newGuest(t, f.srv, "Lee", time.Hour, 2*time.Hour, f.pubJoined.ID)
	if _, err := f.srv.store.EndGuestVisit(ctx, "system", ended.Principal.ID, schema.GuestEndVisit); err != nil {
		t.Fatal(err)
	}
	f.endedTok = ended.Token
	return f
}

// channelV1Caller is one caller class: its token, and whether the fixture
// first withholds manage_channels from every operator.
type channelV1Caller struct {
	name       string
	token      func(f *channelV1Fixture) string
	restricted bool
}

var channelV1Callers = []channelV1Caller{
	{"owner", func(f *channelV1Fixture) string { return f.rootTok }, false},
	{"operator", func(f *channelV1Fixture) string { return f.opTok }, false},
	{"restricted operator", func(f *channelV1Fixture) string { return f.opTok }, true},
	{"manager", func(f *channelV1Fixture) string { return f.member2Tok }, false},
	{"member", func(f *channelV1Fixture) string { return f.memberTok }, false},
	{"guest", func(f *channelV1Fixture) string { return f.guestTok }, false},
	{"ended guest replaying its token", func(f *channelV1Fixture) string { return f.endedTok }, false},
	{"agent", func(f *channelV1Fixture) string { return f.agTok }, false},
	{"unauthenticated", func(*channelV1Fixture) string { return "" }, false},
}

// channelV1Target is one target of the route tests, by name and id.
type channelV1Target struct {
	name     string
	channel  func(f *channelV1Fixture) (name string, id int64)
	existing bool
}

var channelV1Targets = []channelV1Target{
	{"public joined", func(f *channelV1Fixture) (string, int64) { return f.pubJoined.Name, f.pubJoined.ID }, true},
	{"public not joined", func(f *channelV1Fixture) (string, int64) { return f.pubOther.Name, f.pubOther.ID }, true},
	{"private joined", func(f *channelV1Fixture) (string, int64) { return f.chanB.Name, f.chanB.ID }, true},
	{"private not joined", func(f *channelV1Fixture) (string, int64) { return f.chanA.Name, f.chanA.ID }, true},
	{"nonexistent", func(*channelV1Fixture) (string, int64) { return "never-made", 9999 }, false},
}

func createChannelV1Body(name string, v schema.ChannelVisibility) string {
	return string(mustJSONString(schema.CreateChannelRequestV1{Name: name, Visibility: v}))
}

func mustJSONString(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestCreateChannelV1Refusals: POST /v1/channels by every caller class at
// every target name. A name that exists answers one byte-identical 409
// channel_exists to every caller who may create, whether it is public or
// private and whether the caller is in it; a new name is created. Agents,
// guests and the unauthenticated are refused before any name is looked at,
// with one answer per class whatever the name.
func TestCreateChannelV1Refusals(t *testing.T) {
	var exists string
	for _, c := range channelV1Callers {
		t.Run(c.name, func(t *testing.T) {
			f := newChannelV1Fixture(t)
			if c.restricted {
				f.withhold(t, schema.PermManageChannels)
			}
			refusal := map[string]struct {
				status int
				code   string
			}{
				"guest":                           {http.StatusForbidden, schema.ErrorCodeGuestNotAllowed},
				"ended guest replaying its token": {http.StatusUnauthorized, "unauthenticated"},
				"agent":                           {http.StatusForbidden, "forbidden"},
				"unauthenticated":                 {http.StatusUnauthorized, "unauthenticated"},
			}
			want, refused := refusal[c.name]
			var first string
			for _, tg := range channelV1Targets {
				name, _ := tg.channel(f)
				rec := f.do(t, http.MethodPost, "/v1/channels", c.token(f), createChannelV1Body(name, schema.ChannelPrivate))
				switch {
				case refused:
					assertErrorBody(t, rec, want.status, want.code)
					if first == "" {
						first = rec.Body.String()
					} else if rec.Body.String() != first {
						t.Errorf("%s: body %s differs from %s", tg.name, rec.Body, first)
					}
				case tg.existing:
					assertErrorBody(t, rec, http.StatusConflict, "channel_exists")
					if exists == "" {
						exists = rec.Body.String()
					} else if rec.Body.String() != exists {
						t.Errorf("%s: body %s differs from %s", tg.name, rec.Body, exists)
					}
				default:
					if rec.Code != http.StatusCreated {
						t.Fatalf("%s: create = %d %s, want 201", tg.name, rec.Code, rec.Body)
					}
				}
			}
			if refused {
				if _, err := f.srv.store.ChannelByName(context.Background(), "never-made"); err == nil {
					t.Error("a refused caller made a channel")
				}
			}
		})
	}
}

// TestChannelSettingsRefusals: PUT /v1/channels/{id}/settings by every caller
// class at every target. An operator holding manage_channels changes any
// channel and gets channel_not_found for an id that never existed; every
// other caller gets one answer whatever the id, so no answer tells a
// non-operator, or a restricted operator, that a private channel exists.
func TestChannelSettingsRefusals(t *testing.T) {
	for _, c := range channelV1Callers {
		t.Run(c.name, func(t *testing.T) {
			f := newChannelV1Fixture(t)
			if c.restricted {
				f.withhold(t, schema.PermManageChannels)
			}
			refusal := map[string]struct {
				status int
				code   string
			}{
				"restricted operator":             {http.StatusForbidden, schema.ErrorCodePermissionDenied},
				"manager":                         {http.StatusForbidden, "forbidden"},
				"member":                          {http.StatusForbidden, "forbidden"},
				"guest":                           {http.StatusForbidden, "forbidden"},
				"ended guest replaying its token": {http.StatusUnauthorized, "unauthenticated"},
				"agent":                           {http.StatusForbidden, "forbidden"},
				"unauthenticated":                 {http.StatusUnauthorized, "unauthenticated"},
			}
			want, refused := refusal[c.name]
			before := len(f.audit(t))
			var first string
			for _, tg := range channelV1Targets {
				_, id := tg.channel(f)
				rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", id), c.token(f), `{"default":false}`)
				switch {
				case refused:
					assertErrorBody(t, rec, want.status, want.code)
					if first == "" {
						first = rec.Body.String()
					} else if rec.Body.String() != first {
						t.Errorf("%s: body %s differs from %s", tg.name, rec.Body, first)
					}
				case tg.existing:
					if rec.Code != http.StatusOK {
						t.Fatalf("%s: settings = %d %s, want 200", tg.name, rec.Code, rec.Body)
					}
				default:
					assertErrorBody(t, rec, http.StatusNotFound, "channel_not_found")
				}
			}
			for _, e := range f.audit(t)[before:] {
				if e.Action == store.AuditChannelSettingsChanged {
					t.Errorf("an unchanged or refused setting was audited: %+v", e)
				}
			}
		})
	}
}

// TestCreateChannelV1Creator: the creator of a public or a private channel
// is its only member, deciding approvals and managing it; the creation is
// audited with the creator as actor, and a caller without the permission
// for the visibility asked is refused naming it.
func TestCreateChannelV1Creator(t *testing.T) {
	ctx := context.Background()
	f := newChannelV1Fixture(t)
	for _, v := range []schema.ChannelVisibility{schema.ChannelPublic, schema.ChannelPrivate} {
		name := "made-" + string(v)
		rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body(name, v))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", v, rec.Code, rec.Body)
		}
		got := decodeBody[schema.CreateChannelResponseV1](t, rec).Channel
		if got.Name != name || got.Visibility != v || !got.Manages || got.Default {
			t.Errorf("create %s answered %+v", v, got)
		}
		members, err := f.srv.store.ListChannelMembers(ctx, got.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(members) != 1 || members[0].PrincipalID != f.member.ID || !members[0].CanDecide || !members[0].Manages {
			t.Errorf("%s channel members = %+v, want only member-1 deciding and managing", v, members)
		}
		subject := fmt.Sprintf("channel:%d", got.ID)
		for _, a := range []struct{ action, detail string }{
			{store.AuditChannelCreated, fmt.Sprintf("name=%q visibility=%s limited=1", name, v)},
			{store.AuditMemberAdded, fmt.Sprintf("principal=%d", f.member.ID)},
			{store.AuditMemberManageGranted, fmt.Sprintf("principal=%d", f.member.ID)},
		} {
			found := false
			for _, e := range f.audit(t) {
				if e.Subject == subject && e.Action == a.action && e.Detail == a.detail && e.Actor == fmt.Sprintf("principal:%d", f.member.ID) {
					found = true
				}
			}
			if !found {
				t.Errorf("%s channel: no %s row by member-1 with detail %q", v, a.action, a.detail)
			}
		}
	}

	// GET /v1/channels carries the visibility, the default mark and the
	// manager grant.
	rec := f.do(t, http.MethodGet, "/v1/channels", f.memberTok, "")
	seen := map[string]schema.ChannelV0{}
	for _, ch := range decodeBody[schema.ListChannelsResponse](t, rec).Channels {
		seen[ch.Name] = ch
	}
	if ch := seen["made-public"]; ch.Visibility != schema.ChannelPublic || !ch.Manages {
		t.Errorf("listed made-public = %+v", ch)
	}
	if ch := seen["channel-b"]; ch.Visibility != schema.ChannelPrivate || ch.Manages {
		t.Errorf("listed channel-b = %+v", ch)
	}
	if !strings.Contains(rec.Body.String(), `"visibility":"public"`) {
		t.Errorf("list carries no visibility: %s", rec.Body)
	}

	// Without the permission for the visibility, refused naming it; the
	// other visibility still works.
	doc, err := f.srv.store.Permissions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, schema.PutPermissionsRequestV1{
		Revision: doc.Revision,
		Changes:  []schema.PermissionChangeV1{{Permission: schema.PermCreatePrivateChannels, Role: schema.PermissionRoleMember, Granted: false}},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec = f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body("hidden-room", schema.ChannelPrivate))
	assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodePermissionDenied)
	if p := permissionOf(t, rec.Body.Bytes()); p != schema.PermCreatePrivateChannels {
		t.Errorf("refusal names %q, want %q", p, schema.PermCreatePrivateChannels)
	}
	if rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body("open-room", schema.ChannelPublic)); rec.Code != http.StatusCreated {
		t.Errorf("public create without create_private_channels = %d %s", rec.Code, rec.Body)
	}
}

// TestCreateChannelV1Collisions: a public name, a private name the caller is
// not in, and look-alikes of each answer one byte-identical 409, and each
// refusal is audited without any name.
func TestCreateChannelV1Collisions(t *testing.T) {
	f := newChannelV1Fixture(t)
	before := len(f.audit(t))
	var first string
	names := []string{"back-porch", "channel-a", "CHANNEL-A", "Back-Porch", "chаnnel-a"} // the last has a Cyrillic а
	for _, name := range names {
		rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body(name, schema.ChannelPublic))
		assertErrorBody(t, rec, http.StatusConflict, "channel_exists")
		if first == "" {
			first = rec.Body.String()
		} else if rec.Body.String() != first {
			t.Errorf("%q: body %s differs from %s", name, rec.Body, first)
		}
	}
	refused := 0
	for _, e := range f.audit(t)[before:] {
		if e.Action != store.AuditChannelCreateRefused {
			t.Errorf("a collision wrote %+v", e)
			continue
		}
		refused++
		if e.Actor != fmt.Sprintf("principal:%d", f.member.ID) || e.Detail != "" {
			t.Errorf("refusal row = %+v, want member-1 and no detail", e)
		}
		row := e.Subject + e.Detail
		for _, n := range names {
			if strings.Contains(strings.ToLower(row), strings.ToLower(n)) {
				t.Errorf("refusal row %+v carries the name %q", e, n)
			}
		}
	}
	if refused != len(names) {
		t.Errorf("refusals audited = %d, want %d", refused, len(names))
	}
}

// TestCreateChannelV1Limits: a member makes at most createsPerDay channels
// and tries at most createAttemptsPerDay times a day, answered 429 with
// Retry-After; a collision counts as an attempt; an operator holding
// manage_channels is not bounded.
func TestCreateChannelV1Limits(t *testing.T) {
	t.Run("creations", func(t *testing.T) {
		f := newChannelV1Fixture(t)
		for i := range createsPerDay {
			if rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body(fmt.Sprintf("room-%d", i), schema.ChannelPublic)); rec.Code != http.StatusCreated {
				t.Fatalf("creation %d = %d %s", i+1, rec.Code, rec.Body)
			}
		}
		rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body("one-too-many", schema.ChannelPublic))
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		if ra := rec.Header().Get("Retry-After"); ra == "" || ra == "0" {
			t.Errorf("Retry-After = %q", ra)
		}
		if _, err := f.srv.store.ChannelByName(context.Background(), "one-too-many"); err == nil {
			t.Error("a bounded creation made the channel")
		}
	})
	t.Run("attempts", func(t *testing.T) {
		f := newChannelV1Fixture(t)
		for i := range createAttemptsPerDay {
			rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body("channel-a", schema.ChannelPrivate))
			assertErrorBody(t, rec, http.StatusConflict, "channel_exists")
			_ = i
		}
		rec := f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body("channel-a", schema.ChannelPrivate))
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		if rec.Header().Get("Retry-After") == "" {
			t.Error("no Retry-After")
		}
		// A fresh name is bounded too: the attempts are spent.
		rec = f.do(t, http.MethodPost, "/v1/channels", f.memberTok, createChannelV1Body("fresh-room", schema.ChannelPrivate))
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
	})
	t.Run("operators are not bounded", func(t *testing.T) {
		f := newChannelV1Fixture(t)
		for i := range createsPerDay + 2 {
			if rec := f.do(t, http.MethodPost, "/v1/channels", f.opTok, createChannelV1Body(fmt.Sprintf("op-room-%d", i), schema.ChannelPrivate)); rec.Code != http.StatusCreated {
				t.Fatalf("operator creation %d = %d %s", i+1, rec.Code, rec.Body)
			}
		}
		for i := range createAttemptsPerDay + 2 {
			if rec := f.do(t, http.MethodPost, "/v1/channels", f.opTok, createChannelV1Body("channel-a", schema.ChannelPrivate)); rec.Code != http.StatusConflict {
				t.Fatalf("operator collision %d = %d %s", i+1, rec.Code, rec.Body)
			}
		}
		for _, e := range f.audit(t) {
			if e.Actor == fmt.Sprintf("principal:%d", f.op.ID) && (e.Action == store.AuditChannelCreateRefused || strings.HasSuffix(e.Detail, " limited=1")) {
				t.Errorf("an unbounded operator's write counted: %+v", e)
			}
		}
	})
}

// TestCreateChannelV0StaysOperatorOnly: POST /v0/channels is an operator's
// and makes a private channel.
func TestCreateChannelV0StaysOperatorOnly(t *testing.T) {
	f := newChannelV1Fixture(t)
	assertErrorBody(t, f.do(t, http.MethodPost, "/v0/channels", f.memberTok, `{"name":"v0-room"}`), http.StatusForbidden, "forbidden")
	rec := f.do(t, http.MethodPost, "/v0/channels", f.rootTok, `{"name":"v0-room"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("v0 create = %d %s", rec.Code, rec.Body)
	}
	if got := decodeBody[schema.CreateChannelResponse](t, rec).Channel; got.Visibility != schema.ChannelPrivate {
		t.Errorf("v0 channel = %+v, want private", got)
	}
}

// putSettings sends PUT /v1/channels/{id}/settings as the owner.
func (f *channelV1Fixture) putSettings(t *testing.T, id int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", id), f.rootTok, body)
}

// TestChannelSettings: the settings rules, each change checked with its
// audit rows written at one instant, the mark of one transaction.
func TestChannelSettings(t *testing.T) {
	ctx := context.Background()
	t.Run("a request with no field is 400", func(t *testing.T) {
		f := newChannelV1Fixture(t)
		assertErrorBody(t, f.putSettings(t, f.chanA.ID, `{}`), http.StatusBadRequest, "invalid_request")
	})
	t.Run("a private default is 409", func(t *testing.T) {
		f := newChannelV1Fixture(t)
		before := len(f.audit(t))
		assertErrorBody(t, f.putSettings(t, f.chanA.ID, `{"default":true}`), http.StatusConflict, schema.ErrorCodeChannelSettingsConflict)
		assertErrorBody(t, f.putSettings(t, f.pubJoined.ID, `{"visibility":"private","default":true}`), http.StatusConflict, schema.ErrorCodeChannelSettingsConflict)
		if rec := f.putSettings(t, f.pubJoined.ID, `{"default":true}`); rec.Code != http.StatusOK {
			t.Fatalf("mark public default = %d %s", rec.Code, rec.Body)
		}
		assertErrorBody(t, f.putSettings(t, f.pubJoined.ID, `{"visibility":"private"}`), http.StatusConflict, schema.ErrorCodeChannelSettingsConflict)
		ch, err := f.srv.store.ChannelByID(ctx, f.pubJoined.ID)
		if err != nil || ch.Visibility != schema.ChannelPublic || !ch.IsDefault {
			t.Errorf("after the refusals town-square = %+v, %v", ch, err)
		}
		if a, _ := f.srv.store.ChannelByID(ctx, f.chanA.ID); a.IsDefault || a.Visibility != schema.ChannelPrivate {
			t.Errorf("after the refusal channel-a = %+v", a)
		}
		changed := 0
		for _, e := range f.audit(t)[before:] {
			if e.Action == store.AuditChannelSettingsChanged {
				changed++
			}
		}
		if changed != 1 {
			t.Errorf("settings rows = %d, want 1 (the one change made)", changed)
		}
	})
	t.Run("turning private keeps the members", func(t *testing.T) {
		f := newChannelV1Fixture(t)
		was, err := f.srv.store.ListChannelMembers(ctx, f.pubJoined.ID)
		if err != nil {
			t.Fatal(err)
		}
		before := len(f.audit(t))
		rec := f.putSettings(t, f.pubJoined.ID, `{"visibility":"private"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("turn private = %d %s", rec.Code, rec.Body)
		}
		resp := decodeBody[schema.ChannelSettingsResponseV1](t, rec)
		if resp.Channel.Visibility != schema.ChannelPrivate || resp.OpenSignupTurnedOff || len(resp.Warnings) != 0 {
			t.Errorf("answer = %+v", resp)
		}
		for _, k := range []string{"guests_removed", "links_changed", "links_closed"} {
			if strings.Contains(rec.Body.String(), k) {
				t.Errorf("answer carries guest count %s: %s", k, rec.Body)
			}
		}
		now, err := f.srv.store.ListChannelMembers(ctx, f.pubJoined.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(memberIDs(was)) != fmt.Sprint(memberIDs(now)) {
			t.Errorf("members %v became %v", memberIDs(was), memberIDs(now))
		}
		if !f.auditHas(t, f.root, store.AuditChannelSettingsChanged, "visibility=public->private") {
			t.Error("no channel_settings_changed row for the private turn")
		}
		for _, e := range f.audit(t)[before:] {
			if e.Action == store.AuditMemberRemoved {
				t.Errorf("a member was removed: %+v", e)
			}
		}
	})
	t.Run("unmarking the last default turns open sign-up off", func(t *testing.T) {
		f := newChannelV1Fixture(t)
		for _, id := range []int64{f.pubJoined.ID, f.pubOther.ID} {
			if rec := f.putSettings(t, id, `{"default":true}`); rec.Code != http.StatusOK {
				t.Fatalf("mark default = %d %s", rec.Code, rec.Body)
			}
		}
		legacyExec(t, f.srv, `INSERT INTO server_settings (key, value, updated_at) VALUES ('open_signup', 'true', 0)
			ON CONFLICT (key) DO UPDATE SET value = 'true'`)
		// One default is left: sign-up stays on.
		rec := f.putSettings(t, f.pubOther.ID, `{"default":false}`)
		if rec.Code != http.StatusOK || decodeBody[schema.ChannelSettingsResponseV1](t, rec).OpenSignupTurnedOff {
			t.Fatalf("unmark one of two = %d %s", rec.Code, rec.Body)
		}
		// The last, turned private at once: sign-up goes off with it.
		rec = f.putSettings(t, f.pubJoined.ID, `{"default":false,"visibility":"private"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("unmark the last = %d %s", rec.Code, rec.Body)
		}
		resp := decodeBody[schema.ChannelSettingsResponseV1](t, rec)
		if !resp.OpenSignupTurnedOff || resp.Channel.Default || resp.Channel.Visibility != schema.ChannelPrivate {
			t.Errorf("answer = %+v", resp)
		}
		set, _, err := f.srv.store.JoinSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if set.OpenSignup == nil || *set.OpenSignup {
			t.Errorf("open_signup = %v, want off", set.OpenSignup)
		}
		var settingsAt, signupAt time.Time
		for _, e := range f.audit(t) {
			switch {
			case e.Action == store.AuditChannelSettingsChanged && e.Subject == fmt.Sprintf("channel:%d", f.pubJoined.ID) && e.Detail == "visibility=public->private default=true->false":
				settingsAt = e.CreatedAt
			case e.Action == store.AuditJoinSettingsChanged && strings.Contains(e.Detail, "reason=last_default_unmarked"):
				if !strings.Contains(e.Detail, "open_joining=account->off") {
					t.Errorf("sign-up row detail = %q", e.Detail)
				}
				signupAt = e.CreatedAt
			}
		}
		if settingsAt.IsZero() || signupAt.IsZero() || !settingsAt.Equal(signupAt) {
			t.Errorf("settings row at %v, sign-up row at %v: want both, in one transaction", settingsAt, signupAt)
		}
		// With sign-up off, unmarking writes nothing more.
		if rec := f.putSettings(t, f.pubOther.ID, `{"default":true}`); rec.Code != http.StatusOK {
			t.Fatal(rec.Body)
		}
		if rec := f.putSettings(t, f.pubOther.ID, `{"default":false}`); decodeBody[schema.ChannelSettingsResponseV1](t, rec).OpenSignupTurnedOff {
			t.Error("sign-up already off was turned off again")
		}
	})
}

func memberIDs(ms []store.ChannelMember) []int64 {
	ids := make([]int64, len(ms))
	for i, m := range ms {
		ids[i] = m.PrincipalID
	}
	return ids
}

// TestEventsChannelSettings: a settings change sends every events socket of
// the channel one conch.channel.v1 carrying the new visibility and default
// mark.
func TestEventsChannelSettings(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	base := wsTestServer(t, f.srv)
	c := dialEvents(t, base, "/v1/events/ws?channel=ops", f.tokens["ann"])
	if first := c.next(); first.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("first frame = %v, want the roster", first)
	}
	rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/%d/settings", f.ops.ID), f.rootTok, `{"visibility":"public","default":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings = %d %s", rec.Code, rec.Body)
	}
	want := schema.ChannelDocV1{Schema: schema.ChannelSchemaV1, ChannelID: f.ops.ID, Name: f.ops.Name,
		CreatedAt: schema.NewTimestamp(f.ops.CreatedAt), Visibility: schema.ChannelPublic, Default: true}
	if fr := c.next(); fr.schema != schema.ChannelSchemaV1 || fr.channel != want {
		t.Fatalf("frame after the change = %v, want %+v", fr, want)
	}
}
