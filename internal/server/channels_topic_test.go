package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Setting a channel's topic (issue #1503, docs/design/channel-topic.md):
// PUT /v1/channels/{id}/topic.

func channelTopicPath(id int64) string { return fmt.Sprintf("/v1/channels/%d/topic", id) }

func channelTopicBody(t *testing.T, topic string) string {
	t.Helper()
	return `{"topic":` + jsonString(t, topic) + `}`
}

// topicWant is the answer of the design's §3.2 to a caller class at a
// target of channelV1Fixture: town-square (public; member-1 in it, member-2
// its manager), back-porch (public; the owner only), channel-b (private;
// operator-1 manages it, member-1 is in it) and channel-a (private; the
// owner only), and an id nobody has.
func topicWant(caller, target string) selfAnswer {
	switch caller {
	case "unauthenticated", "ended guest replaying its token":
		return selfAnswer{http.StatusUnauthorized, "unauthenticated"}
	case "agent":
		return selfAnswer{http.StatusForbidden, "forbidden"}
	}
	managerOf := map[string][]string{
		"owner":               {"public joined", "public not joined", "private joined", "private not joined"},
		"operator":            {"public joined", "public not joined", "private joined", "private not joined"},
		"restricted operator": {"private joined"},
		"manager":             {"public joined"},
	}[caller]
	memberOf := map[string][]string{
		"member": {"public joined", "private joined"},
		"guest":  {"public joined"},
	}[caller]
	switch {
	case slices.Contains(managerOf, target):
		return selfAnswer{http.StatusOK, ""}
	case slices.Contains(memberOf, target):
		return selfAnswer{http.StatusForbidden, "forbidden"}
	default:
		return selfAnswer{http.StatusNotFound, "channel_not_found"}
	}
}

// Every caller class at every target, each asked of a fresh fixture: who may
// write, who is told the channel does not exist, who is told no. A refusal
// writes nothing, and a non-member's 404 is the bytes of an id nobody has.
func TestSetChannelTopicAnswers(t *testing.T) {
	for _, c := range channelV1Callers {
		t.Run(c.name, func(t *testing.T) {
			seen := map[selfAnswer]string{}
			for _, target := range channelV1Targets {
				f := newChannelV1Fixture(t)
				if c.restricted {
					f.withhold(t, schema.PermManageChannels)
				}
				_, id := target.channel(f)
				want := topicWant(c.name, target.name)
				deniedBefore := countAction(f.audit(t), "access_denied")
				topic := "topic for " + target.name
				rec := f.do(t, http.MethodPut, channelTopicPath(id), c.token(f), channelTopicBody(t, topic))
				checkSelfAnswer(t, target.name, rec.Code, rec.Body.String(), want, seen)
				if want.status == http.StatusOK {
					got := decodeBody[schema.SetChannelTopicResponseV1](t, rec).Channel
					if got.ID != id || got.Topic != topic {
						t.Errorf("%s: response channel = %+v, want id %d with topic %q", target.name, got, id, topic)
					}
				}
				stored, err := f.srv.store.ChannelByID(context.Background(), id)
				switch {
				case !target.existing:
					if err == nil {
						t.Errorf("%s: channel %d exists", target.name, id)
					}
				case err != nil:
					t.Errorf("%s: %v", target.name, err)
				case (stored.Topic != "") != (want.status == http.StatusOK):
					t.Errorf("%s: stored topic %q after a %d", target.name, stored.Topic, rec.Code)
				}
				wantDenied := want.status == http.StatusForbidden
				if after := countAction(f.audit(t), "access_denied"); (after > deniedBefore) != wantDenied {
					t.Errorf("%s: access_denied rows %d -> %d, want a new one: %v", target.name, deniedBefore, after, wantDenied)
				}
				wantRows := boolCount(want.status == http.StatusOK)
				if n := countAction(f.audit(t), store.AuditChannelTopicChanged); n != wantRows {
					t.Errorf("%s: %d channel_topic_changed rows, want %d", target.name, n, wantRows)
				}
			}
		})
	}
}

// A non-member is told what a caller is told of an id nobody has, byte for
// byte, so the route is no way to find a private channel's id.
func TestSetChannelTopicNonMemberCannotTellFromNoChannel(t *testing.T) {
	f := newChannelV1Fixture(t)
	body := channelTopicBody(t, "probe")
	unknown := f.do(t, http.MethodPut, channelTopicPath(9999), f.member2Tok, body)
	for _, id := range []int64{f.chanA.ID, f.chanB.ID} {
		rec := f.do(t, http.MethodPut, channelTopicPath(id), f.member2Tok, body)
		assertErrorBody(t, rec, http.StatusNotFound, "channel_not_found")
		if rec.Body.String() != unknown.Body.String() {
			t.Errorf("private channel %d: %s, an id nobody has: %s", id, rec.Body, unknown.Body)
		}
	}
	// The class answers before the body is read: a body that is not JSON
	// gets the same 404.
	assertErrorBody(t, f.do(t, http.MethodPut, channelTopicPath(f.chanA.ID), f.member2Tok, `{`), http.StatusNotFound, "channel_not_found")
}

// An operator without manage_channels is a member: 404 where it is not in
// the channel, 403 where it is in without manages, 200 where it manages.
func TestSetChannelTopicRestrictedOperator(t *testing.T) {
	f := newChannelV1Fixture(t)
	f.withhold(t, schema.PermManageChannels)
	if _, err := f.srv.store.AddChannelMember(context.Background(), "system", f.pubJoined.ID, f.op.ID, 0); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		id   int64
		want selfAnswer
	}{
		{"a channel it is not in", f.chanA.ID, selfAnswer{http.StatusNotFound, "channel_not_found"}},
		{"a channel it is in without manages", f.pubJoined.ID, selfAnswer{http.StatusForbidden, "forbidden"}},
		{"a channel it manages", f.chanB.ID, selfAnswer{http.StatusOK, ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPut, channelTopicPath(tt.id), f.opTok, channelTopicBody(t, "restricted"))
			checkSelfAnswer(t, tt.name, rec.Code, rec.Body.String(), tt.want, map[selfAnswer]string{})
		})
	}
}

// The body and the topic, case by case, by the owner of an existing channel.
func TestSetChannelTopicBodies(t *testing.T) {
	tests := []struct {
		name   string
		before string
		path   func(id int64) string
		body   func(t *testing.T) string
		status int
		code   string
		topic  string // the stored topic afterwards
		rows   int    // channel_topic_changed rows afterwards
	}{
		{name: "a topic", body: func(t *testing.T) string { return channelTopicBody(t, "Release planning") }, status: 200, topic: "Release planning", rows: 1},
		{name: "the limit", body: func(t *testing.T) string {
			return channelTopicBody(t, strings.Repeat("a", schema.MaxChannelTopicLength))
		}, status: 200,
			topic: strings.Repeat("a", schema.MaxChannelTopicLength), rows: 1},
		{name: "the same string", before: "same", body: func(t *testing.T) string { return channelTopicBody(t, "same") }, status: 200, topic: "same", rows: 1},
		{name: "a clear", before: "old", body: func(t *testing.T) string { return channelTopicBody(t, "") }, status: 200, topic: "", rows: 2},
		{name: "an absent field clears", before: "old", body: func(*testing.T) string { return `{}` }, status: 200, topic: "", rows: 2},
		{name: "clearing a clear topic", body: func(t *testing.T) string { return channelTopicBody(t, "") }, status: 200, topic: "", rows: 0},
		{name: "one over the limit", body: func(t *testing.T) string {
			return channelTopicBody(t, strings.Repeat("a", schema.MaxChannelTopicLength+1))
		},
			status: 400, code: "invalid_request", topic: "", rows: 0},
		{name: "a newline", before: "keep", body: func(t *testing.T) string { return channelTopicBody(t, "a\nb") }, status: 400, code: "invalid_request", topic: "keep", rows: 1},
		{name: "a tab", before: "keep", body: func(t *testing.T) string { return channelTopicBody(t, "a\tb") }, status: 400, code: "invalid_request", topic: "keep", rows: 1},
		{name: "a bidi override", before: "keep", body: func(t *testing.T) string { return channelTopicBody(t, "a\u202eb") }, status: 400, code: "invalid_request", topic: "keep", rows: 1},
		{name: "a zero-width space", before: "keep", body: func(t *testing.T) string { return channelTopicBody(t, "a\u200bb") }, status: 400, code: "invalid_request", topic: "keep", rows: 1},
		{name: "fringe whitespace", before: "keep", body: func(t *testing.T) string { return channelTopicBody(t, " a ") }, status: 400, code: "invalid_request", topic: "keep", rows: 1},
		{name: "not JSON", body: func(*testing.T) string { return `{"topic":` }, status: 400, code: "invalid_request"},
		{name: "an unknown field", body: func(*testing.T) string { return `{"topic":"x","extra":1}` }, status: 400, code: "invalid_request"},
		{name: "a number for the topic", body: func(*testing.T) string { return `{"topic":5}` }, status: 400, code: "invalid_request"},
		{name: "too large", body: func(t *testing.T) string { return channelTopicBody(t, strings.Repeat("a", 2<<20)) }, status: 413, code: "request_too_large"},
		{name: "an id that is not a number", path: func(int64) string { return "/v1/channels/general/topic" },
			body: func(t *testing.T) string { return channelTopicBody(t, "x") }, status: 400, code: "invalid_request"},
		{name: "an unknown id", path: func(int64) string { return channelTopicPath(9999) },
			body: func(t *testing.T) string { return channelTopicBody(t, "x") }, status: 404, code: "channel_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newChannelV1Fixture(t)
			id := f.pubJoined.ID
			if tt.before != "" {
				if rec := f.do(t, http.MethodPut, channelTopicPath(id), f.rootTok, channelTopicBody(t, tt.before)); rec.Code != http.StatusOK {
					t.Fatalf("seed topic = %d %s", rec.Code, rec.Body)
				}
			}
			path := channelTopicPath(id)
			if tt.path != nil {
				path = tt.path(id)
			}
			rec := f.do(t, http.MethodPut, path, f.rootTok, tt.body(t))
			if tt.status != http.StatusOK {
				assertErrorBody(t, rec, tt.status, tt.code)
				// The error names the rule, not the text.
				if strings.Contains(rec.Body.String(), "a\\nb") || strings.Contains(rec.Body.String(), "keep") {
					t.Errorf("the refusal echoes the topic: %s", rec.Body)
				}
			} else if rec.Code != http.StatusOK {
				t.Fatalf("status = %d %s", rec.Code, rec.Body)
			} else {
				got := decodeBody[schema.SetChannelTopicResponseV1](t, rec).Channel
				if got.ID != id || got.Topic != tt.topic {
					t.Errorf("response channel = %+v, want topic %q", got, tt.topic)
				}
				if tt.topic == "" && strings.Contains(rec.Body.String(), `"topic"`) {
					t.Errorf("a cleared topic is in the response: %s", rec.Body)
				}
			}
			if tt.path != nil {
				return // addressed to no channel of the fixture
			}
			stored, err := f.srv.store.ChannelByID(context.Background(), id)
			if err != nil || stored.Topic != tt.topic {
				t.Errorf("stored = %+v (%v), want topic %q", stored, err, tt.topic)
			}
			if n := countAction(f.audit(t), store.AuditChannelTopicChanged); n != tt.rows {
				t.Errorf("%d channel_topic_changed rows, want %d", n, tt.rows)
			}
		})
	}
}

// The audit row: actor, subject, and both topics quoted.
func TestSetChannelTopicAudit(t *testing.T) {
	f := newChannelV1Fixture(t)
	f.srv.relaxChannelWriteLimit() // six writes and repeats: more than the burst
	ch := f.pubJoined
	steps := []struct{ topic, detail string }{
		{"First", `from="" to="First"`},
		{`Say "hi" to=you`, `from="First" to="Say \"hi\" to=you"`},
		{"", `from="Say \"hi\" to=you" to=""`},
	}
	for _, step := range steps {
		if rec := f.do(t, http.MethodPut, channelTopicPath(ch.ID), f.member2Tok, channelTopicBody(t, step.topic)); rec.Code != http.StatusOK {
			t.Fatalf("set %q = %d %s", step.topic, rec.Code, rec.Body)
		}
		// The same string again is no change and writes nothing.
		if rec := f.do(t, http.MethodPut, channelTopicPath(ch.ID), f.member2Tok, channelTopicBody(t, step.topic)); rec.Code != http.StatusOK {
			t.Fatalf("repeat %q = %d %s", step.topic, rec.Code, rec.Body)
		}
	}
	rows := f.auditOf(t, store.AuditChannelTopicChanged)
	if len(rows) != len(steps) {
		t.Fatalf("%d channel_topic_changed rows, want %d: %+v", len(rows), len(steps), rows)
	}
	for i, step := range steps {
		got := rows[i]
		if got.Actor != fmt.Sprintf("principal:%d", f.member2.ID) || got.Subject != fmt.Sprintf("channel:%d", ch.ID) || got.Detail != step.detail {
			t.Errorf("row %d = %+v, want actor principal:%d, subject channel:%d, detail %s", i, got, f.member2.ID, ch.ID, step.detail)
		}
	}
}

// An archived channel takes the write: a topic is administration, not
// something new in the channel.
func TestSetChannelTopicArchivedChannel(t *testing.T) {
	f := newChannelV1Fixture(t)
	archived := true
	if _, err := f.srv.store.ChannelSettings(context.Background(), "system", 0, f.pubJoined.ID, store.ChannelSettingsChange{Archived: &archived}, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, http.MethodPut, channelTopicPath(f.pubJoined.ID), f.member2Tok, channelTopicBody(t, "kept for the record"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if got := decodeBody[schema.SetChannelTopicResponseV1](t, rec).Channel; got.Topic != "kept for the record" || got.ArchivedAt == nil {
		t.Errorf("response channel = %+v, want the topic on an archived channel", got)
	}
}

func TestSetChannelTopicAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	general, err := f.srv.store.ChannelByName(context.Background(), "general")
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, http.MethodPut, channelTopicPath(general.ID), "", channelTopicBody(t, "open house"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	rows := f.auditOf(t, store.AuditChannelTopicChanged)
	if len(rows) != 1 || rows[0].Actor != "system" {
		t.Errorf("audit = %+v, want one row by system", rows)
	}
}

// After a topic change the channel is the same channel: its id, name,
// members, messages, read marks and voice room are as they were.
func TestSetChannelTopicChangesNothingElse(t *testing.T) {
	f := newChannelV1Fixture(t)
	ctx := context.Background()
	ch := f.pubJoined
	msg := f.postAs(t, f.memberTok, ch.Name, "before the topic")
	if rec := f.do(t, http.MethodPut, "/v1/channels/"+ch.Name+"/read-state", f.memberTok, fmt.Sprintf(`{"last_read_id":%d}`, msg)); rec.Code != http.StatusOK {
		t.Fatalf("read state = %d %s", rec.Code, rec.Body)
	}
	room, err := f.srv.store.ChannelVoiceRoom(ctx, ch.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		for name, path := range map[string]string{
			"members":  "/v1/channels/" + ch.Name + "/members",
			"messages": "/v2/channels/" + ch.Name + "/messages",
			"marks":    "/v1/read-state",
		} {
			rec := f.do(t, http.MethodGet, path, f.rootTok, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d %s", name, rec.Code, rec.Body)
			}
			out[name] = rec.Body.String()
		}
		return out
	}
	before := snapshot()
	if rec := f.do(t, http.MethodPut, channelTopicPath(ch.ID), f.member2Tok, channelTopicBody(t, "a topic")); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body)
	}
	after := snapshot()
	for name := range before {
		if before[name] != after[name] {
			t.Errorf("%s changed:\nbefore %s\nafter  %s", name, before[name], after[name])
		}
	}
	stored, err := f.srv.store.ChannelByID(ctx, ch.ID)
	if err != nil || stored.ID != ch.ID || stored.Name != ch.Name || !stored.CreatedAt.Equal(ch.CreatedAt) || stored.Visibility != ch.Visibility {
		t.Errorf("stored = %+v (%v), want %+v with a topic", stored, err, ch)
	}
	if got, err := f.srv.store.ChannelVoiceRoom(ctx, ch.ID); err != nil || got.ID != room.ID || got.RoomName != room.RoomName {
		t.Errorf("voice room = %+v (%v), want %+v", got, err, room)
	}
}

// GET /v1/channels carries the topic on the caller's channels.
func TestListChannelsCarriesTopic(t *testing.T) {
	f := newChannelV1Fixture(t)
	if rec := f.do(t, http.MethodPut, channelTopicPath(f.pubJoined.ID), f.member2Tok, channelTopicBody(t, "Town square rules")); rec.Code != http.StatusOK {
		t.Fatalf("set = %d %s", rec.Code, rec.Body)
	}
	rec := f.do(t, http.MethodGet, "/v1/channels", f.memberTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d %s", rec.Code, rec.Body)
	}
	found := false
	for _, c := range decodeBody[schema.ListChannelsResponse](t, rec).Channels {
		if c.ID == f.pubJoined.ID {
			found = c.Topic == "Town square rules"
		}
		if c.ID != f.pubJoined.ID && c.Topic != "" {
			t.Errorf("channel %q carries topic %q", c.Name, c.Topic)
		}
	}
	if !found {
		t.Errorf("list = %s, want the topic on town-square", rec.Body)
	}
}

// The directory carries a topic where it carries the channel: on public rows
// for every member, on private rows for members and the ?scope=all operator,
// and a non-member's response holds neither a private channel's name nor
// its topic.
func TestDirectoryTopicVisibility(t *testing.T) {
	f := newChannelV1Fixture(t)
	topics := map[int64]string{
		f.pubJoined.ID: "PUBLIC-JOINED-TOPIC",
		f.pubOther.ID:  "PUBLIC-OTHER-TOPIC",
		f.chanB.ID:     "PRIVATE-B-TOPIC",
		f.chanA.ID:     "PRIVATE-A-TOPIC",
	}
	for id, topic := range topics {
		if rec := f.do(t, http.MethodPut, channelTopicPath(id), f.rootTok, channelTopicBody(t, topic)); rec.Code != http.StatusOK {
			t.Fatalf("set %q = %d %s", topic, rec.Code, rec.Body)
		}
	}
	tests := []struct {
		name  string
		token func() string
		query string
		// sees lists the channels whose rows, with their topics, the
		// response holds; every other topic and private name is absent.
		sees []int64
	}{
		{"a non-member of both private channels", func() string { return f.member2Tok }, "", []int64{f.pubJoined.ID, f.pubOther.ID}},
		{"a member of one private channel", func() string { return f.memberTok }, "", []int64{f.pubJoined.ID, f.pubOther.ID, f.chanB.ID}},
		{"the owner", func() string { return f.rootTok }, "", []int64{f.pubJoined.ID, f.pubOther.ID, f.chanA.ID, f.chanB.ID}},
		{"an operator, scope all", func() string { return f.opTok }, "?scope=all", []int64{f.pubJoined.ID, f.pubOther.ID, f.chanA.ID, f.chanB.ID}},
	}
	names := map[int64]string{f.chanA.ID: f.chanA.Name, f.chanB.ID: f.chanB.Name}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodGet, "/v1/directory/channels"+tt.query, tt.token(), "")
			if rec.Code != http.StatusOK {
				t.Fatalf("directory = %d %s", rec.Code, rec.Body)
			}
			rows := map[int64]schema.ChannelListingV1{}
			for _, row := range decodeBody[schema.ChannelDirectoryResponseV1](t, rec).Channels {
				rows[row.ID] = row
			}
			for id, topic := range topics {
				seen := slices.Contains(tt.sees, id)
				row, listed := rows[id]
				if listed != seen {
					t.Errorf("channel %d listed = %v, want %v", id, listed, seen)
				}
				if seen && row.Topic != topic {
					t.Errorf("channel %d topic = %q, want %q", id, row.Topic, topic)
				}
				if !seen {
					if strings.Contains(rec.Body.String(), topic) {
						t.Errorf("the response holds the topic %q of channel %d it may not see: %s", topic, id, rec.Body)
					}
					if name, private := names[id]; private && strings.Contains(rec.Body.String(), name) {
						t.Errorf("the response holds the name %q of channel %d it may not see: %s", name, id, rec.Body)
					}
				}
			}
		})
	}
}

// A restricted operator cannot widen its view to ?scope=all, and its own
// directory shows the private topic only of a channel it is in.
func TestDirectoryTopicRestrictedOperator(t *testing.T) {
	f := newChannelV1Fixture(t)
	f.withhold(t, schema.PermManageChannels)
	for _, id := range []int64{f.chanA.ID, f.chanB.ID} {
		if rec := f.do(t, http.MethodPut, channelTopicPath(id), f.rootTok, channelTopicBody(t, "SECRET-TOPIC")); rec.Code != http.StatusOK {
			t.Fatalf("set = %d %s", rec.Code, rec.Body)
		}
	}
	rec := f.do(t, http.MethodGet, "/v1/directory/channels?scope=all", f.opTok, "")
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "SECRET-TOPIC") {
		t.Errorf("scope=all = %d %s, want a refusal without the topic", rec.Code, rec.Body)
	}
	rec = f.do(t, http.MethodGet, "/v1/directory/channels", f.opTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("directory = %d %s", rec.Code, rec.Body)
	}
	var withTopic []string
	for _, row := range decodeBody[schema.ChannelDirectoryResponseV1](t, rec).Channels {
		if row.Topic != "" {
			withTopic = append(withTopic, row.Name)
		}
	}
	if len(withTopic) != 1 || withTopic[0] != f.chanB.Name {
		t.Errorf("channels with a topic in the directory = %v, want only %s", withTopic, f.chanB.Name)
	}
}

// The wire carries "topic" only when there is one.
func TestChannelWireOmitsEmptyTopic(t *testing.T) {
	f := newChannelV1Fixture(t)
	rec := f.do(t, http.MethodGet, "/v1/channels", f.rootTok, "")
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `"topic"`) {
		t.Errorf("channels with no topic = %d %s", rec.Code, rec.Body)
	}
}

// A topic change is sent to the sockets that follow the channel (issue
// #1503, design §5.3): every events socket of the channel gets exactly one
// conch.channel.v1 carrying the topic, and stays open; every human member's
// events socket gets conch.channels_changed.v1; the sockets of another
// channel get no channel document; the message and presence sockets get
// nothing.
func TestEventsChannelTopicChanged(t *testing.T) {
	f := newPresenceFixture(t, presenceOpts{auth: AuthRequired})
	ctx := context.Background()
	base := wsTestServer(t, f.srv)
	room := f.inUse(t, f.ops)

	var socks []*eventsClient
	for _, who := range []string{"ann", "ann", "ann2"} {
		c := dialEvents(t, base, "/v1/events/ws?channel=ops", f.tokens[who])
		if first := c.next(); first.schema != schema.SessionRosterSchemaV1 {
			t.Fatalf("%s on ops: first frame = %v, want the roster", who, first)
		}
		socks = append(socks, c)
	}
	other := dialEvents(t, base, "/v1/events/ws?channel=ops2", f.tokens["ann"])
	if first := other.next(); first.schema != schema.SessionRosterSchemaV1 {
		t.Fatalf("ops2: first frame = %v, want the roster", first)
	}
	live := dialAs(t, ctx, base, "/v2/ws?channel=ops", f.tokens["ann"])
	voice, _, err := dialVoice(t, base, "ops", f.tokens["ann"])
	if err != nil {
		t.Fatal(err)
	}
	readPresence(t, voice)

	const topic = "Ops: the on-call rota"
	rec := f.do(t, http.MethodPut, channelTopicPath(f.ops.ID), f.rootTok, channelTopicBody(t, topic))
	if rec.Code != http.StatusOK {
		t.Fatalf("set topic = %d %s", rec.Code, rec.Body)
	}

	want := schema.ChannelDocV1{Schema: schema.ChannelSchemaV1, ChannelID: f.ops.ID, Name: "ops", CreatedAt: schema.NewTimestamp(f.ops.CreatedAt), Visibility: schema.ChannelPrivate, Topic: topic}
	for i, c := range socks {
		var docs []schema.ChannelDocV1
		changed := 0
		c.until("the topic and the list change", func(fr eventFrame) bool {
			switch fr.schema {
			case schema.ChannelSchemaV1:
				docs = append(docs, fr.channel)
			case schema.ChannelsChangedSchemaV1:
				changed++
			}
			return len(docs) > 0 && changed > 0
		})
		if len(docs) != 1 || docs[0] != want {
			t.Fatalf("socket %d: channel documents = %+v, want exactly %+v", i, docs, want)
		}
	}

	// Exactly one, and the sockets still work: the next change to ops is the
	// next thing each is sent, with no second channel document before it.
	if rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["bob"]), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("add bob to ops = %d %s", rec.Code, rec.Body)
	}
	for i, c := range socks {
		seen := c.until("bob added to ops", isMembers(func(d schema.ChannelMembersV1) bool {
			_, ok := listsMember(d, f.ids["bob"])
			return ok
		}))
		if n := countFrames(seen, schema.ChannelSchemaV1); n != 0 {
			t.Errorf("socket %d: %d more channel documents: %v", i, n, seen)
		}
	}

	// The socket of ops2 gets no channel document: only the content-free
	// list change its owner is told of, before its next member list.
	if rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/ops2/members/%d", f.ids["bob"]), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("add bob to ops2 = %d %s", rec.Code, rec.Body)
	}
	seen := other.until("ops2's member list", isMembers(func(d schema.ChannelMembersV1) bool { return d.ChannelID == f.ops2.ID }))
	for _, fr := range seen[:len(seen)-1] {
		if fr.schema != schema.ChannelsChangedSchemaV1 {
			t.Errorf("ops2: frame before its member list = %v, want only %s: %v", fr, schema.ChannelsChangedSchemaV1, seen)
		}
	}

	// The message socket's next frame is the next message; the presence
	// socket's next frame is the next presence.
	posted := f.postAs(t, f.tokens["ann2"], "ops", "after the topic")
	rctx, cancel := context.WithTimeout(ctx, eventsReadTimeout)
	defer cancel()
	_, raw, err := live.Read(rctx)
	if err != nil {
		t.Fatalf("the message socket after the topic: %v", err)
	}
	var m schema.MessageV2
	if err := json.Unmarshal(raw, &m); err != nil || m.Validate() != nil || m.ID != posted {
		t.Errorf("the message socket's next frame = %s, want message %d", raw, posted)
	}
	f.lk.setRoom(room, fakeParticipant{identity: f.identity("ann2"), joinedMs: 1_000, published: true, muted: true})
	f.pass(t)
	if s := readPresence(t, voice); !s.Available || len(s.Rooms) != 1 || len(s.Rooms[0].Participants) != 1 {
		t.Errorf("the presence socket's next frame = %+v, want ann2 in the room", s)
	}

	// A same-string write sends nothing: clearing the topic is the next
	// change, and it is sent once, with the topic gone.
	if rec := f.do(t, http.MethodPut, channelTopicPath(f.ops.ID), f.rootTok, channelTopicBody(t, topic)); rec.Code != http.StatusOK {
		t.Fatalf("repeat = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodPut, channelTopicPath(f.ops.ID), f.rootTok, channelTopicBody(t, "")); rec.Code != http.StatusOK {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body)
	}
	seen = socks[0].until("the cleared topic", func(fr eventFrame) bool { return fr.schema == schema.ChannelSchemaV1 })
	if got := seen[len(seen)-1].channel; got.Topic != "" || got.ChannelID != f.ops.ID {
		t.Errorf("channel document after the clear = %+v, want no topic", got)
	}
}
