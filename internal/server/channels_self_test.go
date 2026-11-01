package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Issue #982 (community model I3b): the channel directory, self-join and
// leave. The route tests ask as every caller class of channelV1Fixture at
// its five targets: town-square (public; member-1 in it, member-2 its
// manager), back-porch (public; only the owner in it), channel-b (private;
// operator-1, member-1 and the owner), channel-a (private; only the owner)
// and a channel that never existed.

type selfAnswer struct {
	status int
	code   string // "" for a success
}

// joinWant is what POST /v1/channels/{channel}/join answers caller at target.
func joinWant(caller, target string) selfAnswer {
	switch caller {
	case "guest", "agent":
		return selfAnswer{http.StatusForbidden, schema.ErrorCodeChannelNotJoinable}
	case "ended guest replaying its token", "unauthenticated":
		return selfAnswer{http.StatusUnauthorized, "unauthenticated"}
	}
	in := map[string][]string{
		"owner":               {"public joined", "public not joined", "private joined", "private not joined"},
		"operator":            {"private joined"},
		"restricted operator": {"private joined"},
		"manager":             {"public joined"},
		"member":              {"public joined", "private joined"},
	}[caller]
	switch {
	case slices.Contains(in, target):
		return selfAnswer{http.StatusOK, ""}
	case target == "public joined" || target == "public not joined":
		return selfAnswer{http.StatusCreated, ""}
	default:
		return selfAnswer{http.StatusNotFound, "channel_not_found"}
	}
}

// leaveWant is what POST /v1/channels/{channel}/leave answers caller at
// target, each asked of a fresh fixture.
func leaveWant(caller, target string) selfAnswer {
	switch caller {
	case "guest":
		return selfAnswer{http.StatusForbidden, schema.ErrorCodeGuestNotAllowed}
	case "agent":
		return selfAnswer{http.StatusForbidden, "forbidden"}
	case "ended guest replaying its token", "unauthenticated":
		return selfAnswer{http.StatusUnauthorized, "unauthenticated"}
	}
	if caller == "owner" && target == "private not joined" {
		// channel-a has nobody else in it.
		return selfAnswer{http.StatusConflict, schema.ErrorCodeChannelLastMember}
	}
	in := map[string][]string{
		"owner":               {"public joined", "public not joined", "private joined"},
		"operator":            {"private joined"},
		"restricted operator": {"private joined"},
		"manager":             {"public joined"},
		"member":              {"public joined", "private joined"},
	}[caller]
	if slices.Contains(in, target) {
		return selfAnswer{http.StatusNoContent, ""}
	}
	return selfAnswer{http.StatusNotFound, "channel_not_found"}
}

// checkSelfAnswer checks rec against want, and that every refusal of one
// status and code within a caller class has the same bytes.
func checkSelfAnswer(t *testing.T, label string, got int, body string, want selfAnswer, seen map[selfAnswer]string) {
	t.Helper()
	if got != want.status {
		t.Errorf("%s = %d %s, want %d %s", label, got, body, want.status, want.code)
		return
	}
	if want.code == "" {
		return
	}
	if first, ok := seen[want]; ok && first != body {
		t.Errorf("%s answers %s, another target of the same answer %s", label, body, first)
	}
	seen[want] = body
}

// TestJoinChannelRefusals: POST /v1/channels/{channel}/join by every caller
// class at every target. A member joins a public channel (201) or gets
// their membership back (200); a private channel they are not in answers
// the bytes of one never made; a guest and an agent are refused with one
// channel_not_joinable, whatever the target.
func TestJoinChannelRefusals(t *testing.T) {
	for _, c := range channelV1Callers {
		t.Run(c.name, func(t *testing.T) {
			f := newChannelV1Fixture(t)
			if c.restricted {
				f.withhold(t, schema.PermManageChannels)
			}
			seen := map[selfAnswer]string{}
			for _, tg := range channelV1Targets {
				name, _ := tg.channel(f)
				rec := f.do(t, http.MethodPost, "/v1/channels/"+name+"/join", c.token(f), "")
				checkSelfAnswer(t, tg.name, rec.Code, rec.Body.String(), joinWant(c.name, tg.name), seen)
			}
		})
	}
}

// TestLeaveChannelRefusals: POST /v1/channels/{channel}/leave by every
// caller class at every target, each on a fresh fixture. Any member leaves
// (204); the last member of a private channel may not (409
// channel_last_member); a channel the caller is not in, private or public,
// answers the bytes of one never made; a guest and an agent are refused
// before any lookup.
func TestLeaveChannelRefusals(t *testing.T) {
	for _, c := range channelV1Callers {
		t.Run(c.name, func(t *testing.T) {
			seen := map[selfAnswer]string{}
			for _, tg := range channelV1Targets {
				f := newChannelV1Fixture(t)
				if c.restricted {
					f.withhold(t, schema.PermManageChannels)
				}
				name, _ := tg.channel(f)
				rec := f.do(t, http.MethodPost, "/v1/channels/"+name+"/leave", c.token(f), "")
				checkSelfAnswer(t, tg.name, rec.Code, rec.Body.String(), leaveWant(c.name, tg.name), seen)
			}
		})
	}
}

// directoryOf lists the directory as token, failing unless it is 200.
func (f *channelV1Fixture) directoryOf(t *testing.T, token, query string) map[string]schema.ChannelListingV1 {
	t.Helper()
	rec := f.do(t, http.MethodGet, "/v1/directory/channels"+query, token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("directory%s = %d %s", query, rec.Code, rec.Body)
	}
	rows := map[string]schema.ChannelListingV1{}
	var last int64
	for _, c := range decodeBody[schema.ChannelDirectoryResponseV1](t, rec).Channels {
		if c.ID <= last {
			t.Errorf("directory%s is not in id order at %d", query, c.ID)
		}
		last = c.ID
		rows[c.Name] = c
	}
	return rows
}

// TestChannelDirectory: a member sees every public channel and their own
// private ones, with joined, default, visibility and manages, and no
// counts; ?scope=all lists every channel with its member count to a holder
// of manage_channels and is refused to anyone else; guests and agents are
// refused the directory.
func TestChannelDirectory(t *testing.T) {
	ctx := context.Background()
	f := newChannelV1Fixture(t)
	owner := fmt.Sprintf("principal:%d", f.root.ID)
	yes := true
	if _, err := f.srv.store.ChannelSettings(ctx, owner, f.root.ID, f.pubJoined.ID, store.ChannelSettingsChange{Default: &yes}, time.Now()); err != nil {
		t.Fatal(err)
	}

	member := f.directoryOf(t, f.memberTok, "")
	if _, ok := member[f.chanA.Name]; ok {
		t.Errorf("member-1's directory lists channel-a, a private channel she is not in: %+v", member)
	}
	for name, want := range map[string]schema.ChannelListingV1{
		f.pubJoined.Name: {Visibility: schema.ChannelPublic, Default: true, Joined: true},
		f.pubOther.Name:  {Visibility: schema.ChannelPublic},
		f.chanB.Name:     {Visibility: schema.ChannelPrivate, Joined: true},
	} {
		got, ok := member[name]
		if !ok {
			t.Errorf("member-1's directory lacks %s", name)
			continue
		}
		if got.Visibility != want.Visibility || got.Default != want.Default || got.Joined != want.Joined || got.Manages {
			t.Errorf("member-1's %s = %+v, want %+v", name, got, want)
		}
	}
	for name, row := range member {
		if row.Members != nil {
			t.Errorf("member-1's directory counts %s's members", name)
		}
	}
	if got := f.directoryOf(t, f.member2Tok, "")[f.pubJoined.Name]; !got.Joined || !got.Manages {
		t.Errorf("member-2's town-square = %+v, want joined and managed", got)
	}

	all := f.directoryOf(t, f.opTok, "?scope=all")
	for _, c := range []store.Channel{f.pubJoined, f.pubOther, f.chanA, f.chanB} {
		members, err := f.srv.store.ListChannelMembers(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := all[c.Name]
		if !ok || got.Members == nil || *got.Members != int64(len(members)) {
			t.Errorf("?scope=all %s = %+v (listed %v), want %d members", c.Name, got, ok, len(members))
		}
	}
	if got := all[f.chanB.Name]; !got.Joined || !got.Manages {
		t.Errorf("operator-1's channel-b = %+v, want joined and managed", got)
	}
	if got := all[f.chanA.Name]; got.Joined || got.Manages {
		t.Errorf("operator-1's channel-a = %+v, want neither joined nor managed", got)
	}

	assertErrorBody(t, f.do(t, http.MethodGet, "/v1/directory/channels?scope=all", f.memberTok, ""), http.StatusForbidden, "forbidden")
	assertErrorBody(t, f.do(t, http.MethodGet, "/v1/directory/channels?scope=all", f.member2Tok, ""), http.StatusForbidden, "forbidden")
	assertErrorBody(t, f.do(t, http.MethodGet, "/v1/directory/channels?scope=everything", f.memberTok, ""), http.StatusBadRequest, "invalid_request")
	assertErrorBody(t, f.do(t, http.MethodGet, "/v1/directory/channels", f.guestTok, ""), http.StatusForbidden, schema.ErrorCodeGuestNotAllowed)
	assertErrorBody(t, f.do(t, http.MethodGet, "/v1/directory/channels", f.agTok, ""), http.StatusForbidden, "forbidden")
	assertErrorBody(t, f.do(t, http.MethodGet, "/v1/directory/channels", f.endedTok, ""), http.StatusUnauthorized, "unauthenticated")

	f.withhold(t, schema.PermManageChannels)
	rec := f.do(t, http.MethodGet, "/v1/directory/channels?scope=all", f.opTok, "")
	assertErrorBody(t, rec, http.StatusForbidden, schema.ErrorCodePermissionDenied)
	if e := decodeBody[schema.Error](t, rec); e.Permission != schema.PermManageChannels {
		t.Errorf("restricted operator's refusal names %q, want manage_channels", e.Permission)
	}
	if _, ok := f.directoryOf(t, f.opTok, "")[f.chanA.Name]; ok {
		t.Error("a restricted operator's member view lists channel-a, which they are not in")
	}
}

// TestJoinChannel: a self-join writes can_decide = 0 and no history bound,
// shows the whole history, is audited with the member as actor, and is
// idempotent: a second join returns the same membership and writes
// nothing.
func TestJoinChannel(t *testing.T) {
	ctx := context.Background()
	f := newChannelV1Fixture(t)
	if rec := f.do(t, http.MethodPost, "/v2/channels/"+f.pubOther.Name+"/messages", f.rootTok, `{"body":"before anyone joined"}`); rec.Code != http.StatusCreated {
		t.Fatalf("owner's post = %d %s", rec.Code, rec.Body)
	}
	before := len(f.audit(t))

	rec := f.do(t, http.MethodPost, "/v1/channels/"+f.pubOther.Name+"/join", f.memberTok, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("join = %d %s", rec.Code, rec.Body)
	}
	first := decodeBody[schema.JoinChannelResponseV1](t, rec)
	if first.Channel.ID != f.pubOther.ID || first.Channel.Manages || first.Channel.Visibility != schema.ChannelPublic {
		t.Errorf("joined channel = %+v", first.Channel)
	}
	if m := first.Member; m.PrincipalID != f.member.ID || m.CanDecide == nil || *m.CanDecide || (m.Manages != nil && *m.Manages) || m.HistoryAfter != 0 {
		t.Errorf("membership = %+v, want member-1, can_decide false, no manager grant, no bound", m)
	}
	if can, err := f.srv.store.CanDecideApprovals(ctx, f.pubOther.ID, f.member.ID); err != nil || can {
		t.Errorf("CanDecideApprovals = %v, %v; want false", can, err)
	}
	if bounded, err := f.srv.store.HistoryBounded(ctx, f.pubOther.ID, f.member.ID); err != nil || bounded {
		t.Errorf("HistoryBounded = %v, %v; want false", bounded, err)
	}
	rec = f.do(t, http.MethodGet, "/v2/channels/"+f.pubOther.Name+"/messages", f.memberTok, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "before anyone joined") {
		t.Errorf("history after joining = %d %s, want the earlier message", rec.Code, rec.Body)
	}
	added := f.audit(t)[before:]
	if len(added) != 1 || added[0].Action != store.AuditMemberAdded || added[0].Actor != fmt.Sprintf("principal:%d", f.member.ID) ||
		added[0].Subject != fmt.Sprintf("channel:%d", f.pubOther.ID) {
		t.Errorf("audit rows of the join = %+v, want one member_added by member-1", added)
	}

	after := len(f.audit(t))
	rec = f.do(t, http.MethodPost, "/v1/channels/"+f.pubOther.Name+"/join", f.memberTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second join = %d %s", rec.Code, rec.Body)
	}
	if again := decodeBody[schema.JoinChannelResponseV1](t, rec); string(mustJSONString(again)) != string(mustJSONString(first)) {
		t.Errorf("second join = %s, want %s", mustJSONString(again), mustJSONString(first))
	}
	if n := len(f.audit(t)); n != after {
		t.Errorf("the second join wrote %d audit rows", n-after)
	}

	// A decider's own membership is returned unchanged, can_decide and all.
	rec = f.do(t, http.MethodPost, "/v1/channels/"+f.chanB.Name+"/join", f.memberTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("join of channel-b = %d %s", rec.Code, rec.Body)
	}
	if m := decodeBody[schema.JoinChannelResponseV1](t, rec).Member; m.CanDecide == nil || !*m.CanDecide {
		t.Errorf("member-1's channel-b membership = %+v, want can_decide kept", m)
	}
}

// TestLeaveChannel: a manager who leaves loses the grant (a rejoin is a
// plain member's); a member of a default channel may leave it; the last
// member of a public channel may leave it; the last member of a private
// channel may not, and stays a member; a leave is audited with the member as
// actor.
func TestLeaveChannel(t *testing.T) {
	ctx := context.Background()
	f := newChannelV1Fixture(t)
	owner := fmt.Sprintf("principal:%d", f.root.ID)
	yes := true
	if _, err := f.srv.store.ChannelSettings(ctx, owner, f.root.ID, f.pubJoined.ID, store.ChannelSettingsChange{Default: &yes}, time.Now()); err != nil {
		t.Fatal(err)
	}

	before := len(f.audit(t))
	if rec := f.do(t, http.MethodPost, "/v1/channels/"+f.pubJoined.Name+"/leave", f.member2Tok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("manager's leave of the default channel = %d %s", rec.Code, rec.Body)
	}
	rows := f.audit(t)[before:]
	if len(rows) == 0 || rows[0].Action != store.AuditMemberRemoved || rows[0].Actor != fmt.Sprintf("principal:%d", f.member2.ID) {
		t.Errorf("audit rows of the leave = %+v, want member_removed by member-2", rows)
	}
	if in, err := f.srv.store.IsChannelMember(ctx, f.pubJoined.ID, f.member2.ID); err != nil || in {
		t.Fatalf("member-2 still in town-square: %v, %v", in, err)
	}
	rec := f.do(t, http.MethodPost, "/v1/channels/"+f.pubJoined.Name+"/join", f.member2Tok, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("rejoin = %d %s", rec.Code, rec.Body)
	}
	if m := decodeBody[schema.JoinChannelResponseV1](t, rec).Member; m.Manages != nil && *m.Manages {
		t.Errorf("rejoined membership = %+v, want no manager grant", m)
	}

	if rec := f.do(t, http.MethodPost, "/v1/channels/"+f.pubOther.Name+"/leave", f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Errorf("last member's leave of public back-porch = %d %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.do(t, http.MethodPost, "/v1/channels/"+f.chanA.Name+"/leave", f.rootTok, ""), http.StatusConflict, schema.ErrorCodeChannelLastMember)
	if in, err := f.srv.store.IsChannelMember(ctx, f.chanA.ID, f.root.ID); err != nil || !in {
		t.Errorf("the owner left channel-a after the refusal: %v, %v", in, err)
	}
	// Once someone else is in channel-a, the owner may leave it.
	if _, err := f.srv.store.AddChannelMember(ctx, owner, f.chanA.ID, f.member.ID, f.root.ID); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, http.MethodPost, "/v1/channels/"+f.chanA.Name+"/leave", f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Errorf("owner's leave of channel-a with member-1 in it = %d %s", rec.Code, rec.Body)
	}
}
