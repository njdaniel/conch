package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func TestCreateChannel(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(`{"name":"general"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	var created schema.CreateChannelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.Channel.ID == 0 || created.Channel.Name != "general" || created.Channel.CreatedAt.IsZero() {
		t.Errorf("created channel = %+v", created.Channel)
	}

	// A fresh channel must be immediately visible to the message endpoints.
	getReq := httptest.NewRequest(http.MethodGet, "/v0/channels/general/messages", nil)
	getRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET messages status = %d, want %d; body = %s", getRec.Code, http.StatusOK, getRec.Body.String())
	}
}

func TestCreateChannelDuplicate(t *testing.T) {
	srv := newTestServer(t)

	body := `{"name":"general"}`
	first := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(body))
	firstRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(firstRec, first)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, want %d; body = %s", firstRec.Code, http.StatusCreated, firstRec.Body.String())
	}

	second := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(body))
	secondRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(secondRec, second)
	assertAPIError(t, secondRec, http.StatusConflict, "channel_exists")
}

func TestCreateChannelValidation(t *testing.T) {
	srv := newTestServer(t)

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{"name":`},
		{name: "empty name", body: `{"name":""}`},
		{name: "blank name", body: `{"name":"   "}`},
		{name: "unknown field", body: `{"name":"general","extra":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(tt.body))
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			assertAPIError(t, rec, http.StatusBadRequest, "invalid_request")
		})
	}
}

// TestCreateChannelNameRule applies issue #204's name rule to channel names:
// the accepted examples are created; names with control characters,
// separators, bidi controls or fringe whitespace are refused with 400 and a
// message naming the rule.
func TestCreateChannelNameRule(t *testing.T) {
	srv := newTestServer(t)

	for _, name := range []string{"Zoë Müller", "山田 太郎", "o'brien-smith"} {
		t.Run("accepted/"+name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v0/channels",
				bytes.NewBufferString(fmt.Sprintf(`{"name":%s}`, jsonString(t, name)))))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
			}
		})
	}

	refused := []struct {
		name    string
		input   string
		message string
	}{
		{"escape sequence", "ops\x1b[31m", "control characters"},
		{"newline", "ops\nalerts", "control characters"},
		{"paragraph separator", "a\u2029b", "control characters"},
		{"bidi override", "a\u202eb", "bidi controls"},
		{"leading space", " ops", "whitespace"},
		{"trailing space", "ops ", "whitespace"},
		{"over length", strings.Repeat("a", schema.MaxChannelNameLength+1), "at most"},
	}
	for _, tt := range refused {
		t.Run("refused/"+tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v0/channels",
				bytes.NewBufferString(fmt.Sprintf(`{"name":%s}`, jsonString(t, tt.input)))))
			assertAPIError(t, rec, http.StatusBadRequest, "invalid_request")
			// Decode the error: the wire form escapes what a raw body scan
			// would miss, so scan the message itself.
			e := decodeBody[schema.Error](t, rec)
			if !strings.Contains(e.Message, tt.message) {
				t.Errorf("message = %q, want it to name the rule (%q)", e.Message, tt.message)
			}
			if strings.Contains(e.Message, tt.input) {
				t.Errorf("message echoes the refused name: %q", e.Message)
			}
		})
	}
}

// TestCreateChannelRefusesMembers is the server half of the browser's
// operator-only control (issue #695): a member's well-formed create is
// refused with 403 forbidden and makes nothing, whatever a client shows, so
// the same name is still free for an operator afterwards.
func TestCreateChannelRefusesMembers(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	tests := []struct {
		name  string
		token string
	}{
		{"member human", f.aliceTok},
		{"member agent", f.botTok},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/v0/channels", tt.token, `{"name":"book-club"}`)
			assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
			channels, err := f.srv.store.ListChannels(context.Background())
			if err != nil {
				t.Fatalf("ListChannels: %v", err)
			}
			for _, ch := range channels {
				if ch.Name == "book-club" {
					t.Fatalf("a refused create made channel %+v", ch)
				}
			}
		})
	}
	if rec := f.do(t, http.MethodPost, "/v0/channels", f.rootTok, `{"name":"book-club"}`); rec.Code != http.StatusCreated {
		t.Fatalf("operator create status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
}

func TestListChannels(t *testing.T) {
	tests := []struct {
		name   string
		create []string
		want   []string
	}{
		{name: "empty", want: []string{}},
		{name: "several ordered by id", create: []string{"zeta", "alpha", "general"}, want: []string{"zeta", "alpha", "general"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestServer(t)
			for _, name := range tt.create {
				req := httptest.NewRequest(http.MethodPost, "/v0/channels", bytes.NewBufferString(`{"name":"`+name+`"}`))
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusCreated {
					t.Fatalf("create %q status = %d", name, rec.Code)
				}
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/channels", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
			}
			if len(tt.want) == 0 && strings.TrimSpace(rec.Body.String()) != `{"channels":[]}` {
				t.Errorf("empty body = %s, want {\"channels\":[]}", rec.Body.String())
			}
			var got schema.ListChannelsResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got.Channels) != len(tt.want) {
				t.Fatalf("channels = %+v, want %v", got.Channels, tt.want)
			}
			for i, name := range tt.want {
				if got.Channels[i].Name != name {
					t.Errorf("channels[%d] = %q, want %q", i, got.Channels[i].Name, name)
				}
				if i > 0 && got.Channels[i].ID <= got.Channels[i-1].ID {
					t.Errorf("ids not ascending: %+v", got.Channels)
				}
			}
		})
	}
}

func TestListChannelsStoreFailure(t *testing.T) {
	st, err := store.Open(context.Background(), newTestDBPath(t))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	srv := New(Config{AuthMode: AuthOff, Version: "v0", Listen: "127.0.0.1:0"}, st)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/channels", nil))
	assertAPIError(t, rec, http.StatusInternalServerError, "internal_error")
}

// Channel names are unique by how they read (issue #727): a create whose
// name differs from another channel's only by case or look-alike letters is
// 409 channel_exists, and one nobody can see is 400.
func TestCreateChannelRefusesALookAlike(t *testing.T) {
	srv := newTestServer(t)
	create := func(name string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v0/channels",
			bytes.NewBufferString(`{"name":`+jsonString(t, name)+`}`)))
		return rec
	}
	if rec := create("general"); rec.Code != http.StatusCreated {
		t.Fatalf("create general = %d %s", rec.Code, rec.Body)
	}
	tests := []struct {
		name, input string
		status      int
		code        string
	}{
		{"by spelling", "general", http.StatusConflict, "channel_exists"},
		{"by case", "General", http.StatusConflict, "channel_exists"},
		{"by look-alike", "gener\u0430l", http.StatusConflict, "channel_exists"},
		{"invisible", "\u3164", http.StatusBadRequest, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertAPIError(t, create(tt.input), tt.status, tt.code)
		})
	}
}

// Renaming a channel (issue #727): PUT /v1/channels/{id}/name.

func channelNamePath(id int64) string { return fmt.Sprintf("/v1/channels/%d/name", id) }

func channelRenameBody(t *testing.T, from, name string) string {
	t.Helper()
	return `{"name":` + jsonString(t, name) + `,"from":` + jsonString(t, from) + `}`
}

// renameFixture is the auth fixture with a second channel, "Lobby", that
// nobody is in, for the clashes.
func renameFixture(t *testing.T) (*authFixture, store.Channel, store.Channel) {
	t.Helper()
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	lobby, err := f.srv.store.CreateChannel(ctx, "Lobby")
	if err != nil {
		t.Fatal(err)
	}
	return f, general, lobby
}

// The answers of the design's §3.2, case by case. Each case runs on a fresh
// server.
func TestRenameChannelAnswers(t *testing.T) {
	tests := []struct {
		name string
		// caller picks a token from the fixture; target "general", "lobby"
		// or "nobody" (an id nobody has), or a raw path segment.
		caller, target string
		body           func(t *testing.T) string
		status         int
		code           string
		denied         bool   // an access_denied row is written
		renamed        string // general's name afterwards, when it changed
	}{
		{name: "an operator renames", caller: "root", target: "general", status: 200, renamed: "town square",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "town square") }},
		{name: "an operator changes only the case", caller: "root", target: "general", status: 200, renamed: "General",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "General") }},
		// The route class refuses a member or agent before the body is read.
		// A non-member probing an id nobody has gets the same 404 as a member
		// probing a channel that exists but is not theirs.
		{name: "a member renames her channel", caller: "alice", target: "general", status: 403, code: "forbidden", denied: true,
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "evil") }},
		{name: "an agent renames its channel", caller: "bot", target: "general", status: 403, code: "forbidden", denied: true,
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "evil") }},
		{name: "a member probes an id nobody has", caller: "alice", target: "nobody", status: 404, code: "channel_not_found",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "evil") }},
		{name: "a member sends a bad body", caller: "alice", target: "general", status: 403, code: "forbidden", denied: true,
			body: func(*testing.T) string { return `{` }},
		{name: "no credential", caller: "", target: "general", status: 401, code: "unauthenticated",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "town square") }},
		{name: "not JSON", caller: "root", target: "general", status: 400, code: "invalid_request",
			body: func(*testing.T) string { return `{"name":` }},
		{name: "an unknown field", caller: "root", target: "general", status: 400, code: "invalid_request",
			body: func(*testing.T) string { return `{"name":"x","from":"general","to":"y"}` }},
		{name: "too large", caller: "root", target: "general", status: 413, code: "request_too_large",
			body: func(t *testing.T) string { return channelRenameBody(t, strings.Repeat("a", 2<<20), "x") }},
		{name: "a name that breaks the create rule", caller: "root", target: "general", status: 400, code: "invalid_request",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "a\u202eb") }},
		{name: "a blank name", caller: "root", target: "general", status: 400, code: "invalid_request",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "  ") }},
		{name: "a blank from", caller: "root", target: "general", status: 400, code: "invalid_request",
			body: func(t *testing.T) string { return channelRenameBody(t, " ", "town square") }},
		{name: "no from", caller: "root", target: "general", status: 400, code: "invalid_request",
			body: func(*testing.T) string { return `{"name":"town square"}` }},
		{name: "an id that is not a number", caller: "root", target: "general-id", status: 400, code: "invalid_request",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "town square") }},
		{name: "an unknown id", caller: "root", target: "nobody", status: 404, code: "channel_not_found",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "town square") }},
		// The same string is answered before from is compared.
		{name: "the same name, from anything", caller: "root", target: "general", status: 200,
			body: func(t *testing.T) string { return channelRenameBody(t, "whatever", "general") }},
		{name: "a stale from", caller: "root", target: "general", status: 409, code: schema.ErrorCodeChannelNameStale,
			body: func(t *testing.T) string { return channelRenameBody(t, "town square", "plaza") }},
		{name: "a name nobody can see", caller: "root", target: "general", status: 400, code: "invalid_request",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "\u3164") }},
		{name: "another channel's name by spelling", caller: "root", target: "general", status: 409, code: "channel_exists",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "Lobby") }},
		{name: "another channel's name by case", caller: "root", target: "general", status: 409, code: "channel_exists",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "lobby") }},
		{name: "another channel's name by look-alike", caller: "root", target: "general", status: 409, code: "channel_exists",
			body: func(t *testing.T) string { return channelRenameBody(t, "general", "L\u043ebby") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, general, lobby := renameFixture(t)
			tokens := map[string]string{"root": f.rootTok, "alice": f.aliceTok, "bot": f.botTok, "": ""}
			paths := map[string]string{
				"general": channelNamePath(general.ID), "lobby": channelNamePath(lobby.ID),
				"nobody": channelNamePath(9999), "general-id": "/v1/channels/general/name",
			}
			before := countAction(f.audit(t), "access_denied")
			rec := f.do(t, http.MethodPut, paths[tt.target], tokens[tt.caller], tt.body(t))
			if tt.status != http.StatusOK {
				assertErrorBody(t, rec, tt.status, tt.code)
			} else {
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d %s", rec.Code, rec.Body)
				}
				want := general.Name
				if tt.renamed != "" {
					want = tt.renamed
				}
				got := decodeBody[schema.RenameChannelResponseV1](t, rec).Channel
				if got.ID != general.ID || got.Name != want || !got.CreatedAt.Equal(general.CreatedAt) {
					t.Errorf("response channel = %+v, want id %d named %q", got, general.ID, want)
				}
			}
			stored, err := f.srv.store.ChannelByID(context.Background(), general.ID)
			if want := cmpOr(tt.renamed, "general"); err != nil || stored.Name != want {
				t.Errorf("stored = %+v (%v), want named %q", stored, err, want)
			}
			if after := countAction(f.audit(t), "access_denied"); (after > before) != tt.denied {
				t.Errorf("access_denied rows %d -> %d; want a new one: %v", before, after, tt.denied)
			}
			if n := countAction(f.audit(t), store.AuditChannelRenamed); n != boolCount(tt.renamed != "") {
				t.Errorf("%d channel_renamed rows, want %d", n, boolCount(tt.renamed != ""))
			}
		})
	}
}

func TestRenameChannelManagerStandings(t *testing.T) {
	tests := []struct {
		name       string
		caller     string
		other      bool
		status     int
		code       string
		socket     bool
		wantDenial bool
	}{
		{name: "owner", caller: "owner", status: http.StatusOK},
		{name: "operator", caller: "operator", status: http.StatusOK},
		{name: "manager", caller: "manager", status: http.StatusOK, socket: true},
		{name: "manager of another channel", caller: "manager", other: true, status: http.StatusForbidden, code: "forbidden", wantDenial: true},
		{name: "member", caller: "member", status: http.StatusForbidden, code: "forbidden", wantDenial: true},
		{name: "non-member", caller: "non-member", status: http.StatusNotFound, code: "channel_not_found"},
		{name: "guest", caller: "guest", status: http.StatusForbidden, code: "forbidden", wantDenial: true},
		{name: "agent", caller: "agent", status: http.StatusForbidden, code: "forbidden", wantDenial: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAdminFixture(t)
			tokens := map[string]string{
				"owner": f.rootTok, "operator": f.op2Tok, "manager": f.mgrTok,
				"member": f.aliceTok, "non-member": f.outsiderTok, "guest": f.guestTok, "agent": f.botTok,
			}
			ids := map[string]int64{
				"owner": f.root.ID, "operator": f.op2.ID, "manager": f.mgr.ID,
				"member": f.alice.ID, "non-member": f.outsider.ID, "guest": f.guest.ID, "agent": f.bot.ID,
			}
			channel, from := f.general, "general"
			if tt.other {
				channel, from = f.lobby, "lobby"
			}
			var events *eventsClient
			if tt.socket {
				events = dialEvents(t, wsTestServer(t, f.srv), "/v1/events/ws?channel=general", f.mgrTok)
				if first := events.next(); first.schema != schema.SessionRosterSchemaV1 {
					t.Fatalf("first frame = %v, want roster", first)
				}
			}
			before := f.deniedRows(t, ids[tt.caller])
			rec := f.do(t, http.MethodPut, channelNamePath(channel.ID), tokens[tt.caller], channelRenameBody(t, from, "renamed"))
			if tt.status == http.StatusOK {
				if rec.Code != tt.status || decodeBody[schema.RenameChannelResponseV1](t, rec).Channel.Name != "renamed" {
					t.Fatalf("rename = %d %s", rec.Code, rec.Body)
				}
			} else {
				assertErrorBody(t, rec, tt.status, tt.code)
			}
			if tt.wantDenial && f.deniedRows(t, ids[tt.caller]) == before {
				t.Error("no access_denied row")
			}
			if tt.code == "channel_not_found" {
				ref := f.do(t, http.MethodPut, channelNamePath(999999), tokens[tt.caller], channelRenameBody(t, from, "renamed"))
				if ref.Code != rec.Code || ref.Body.String() != rec.Body.String() {
					t.Errorf("non-member = %d %s, unknown id = %d %s", rec.Code, rec.Body, ref.Code, ref.Body)
				}
			}
			if tt.socket {
				frames := events.until("the renamed channel", func(fr eventFrame) bool {
					return fr.schema == schema.ChannelSchemaV1 && fr.channel.ChannelID == channel.ID && fr.channel.Name == "renamed"
				})
				if got := frames[len(frames)-1].channel.Name; got != "renamed" {
					t.Errorf("socket channel name = %q", got)
				}
			}
		})
	}
}

func TestRenameChannelManagerLimits(t *testing.T) {
	t.Run("sixth rename", func(t *testing.T) {
		f := newAdminFixture(t)
		from := "general"
		for i := 1; i <= renamesPerDay; i++ {
			name := fmt.Sprintf("renamed-%d", i)
			if rec := f.do(t, http.MethodPut, channelNamePath(f.general.ID), f.mgrTok, channelRenameBody(t, from, name)); rec.Code != http.StatusOK {
				t.Fatalf("rename %d = %d %s", i, rec.Code, rec.Body)
			}
			from = name
		}
		rec := f.do(t, http.MethodPut, channelNamePath(f.general.ID), f.mgrTok, channelRenameBody(t, from, "one-too-many"))
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		if rec.Header().Get("Retry-After") == "" {
			t.Error("no Retry-After")
		}
		stored, err := f.srv.store.ChannelByID(context.Background(), f.general.ID)
		if err != nil || stored.Name != from {
			t.Errorf("stored = %+v, %v; want %q", stored, err, from)
		}
	})
	t.Run("21st attempt and unlimited operator", func(t *testing.T) {
		f := newAdminFixture(t)
		for i := 1; i <= renameAttemptsPerDay; i++ {
			rec := f.do(t, http.MethodPut, channelNamePath(f.general.ID), f.mgrTok, channelRenameBody(t, "general", "lobby"))
			assertErrorBody(t, rec, http.StatusConflict, "channel_exists")
		}
		rec := f.do(t, http.MethodPut, channelNamePath(f.general.ID), f.mgrTok, channelRenameBody(t, "general", "lobby"))
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		for _, event := range auditRows(t, f.authFixture, store.AuditChannelRenameRefused) {
			if event.Actor != fmt.Sprintf("principal:%d", f.mgr.ID) || event.Detail != "" {
				t.Errorf("refusal audit = %+v", event)
			}
		}
		for i := 0; i <= renameAttemptsPerDay; i++ {
			assertErrorBody(t, f.do(t, http.MethodPut, channelNamePath(f.general.ID), f.rootTok, channelRenameBody(t, "general", "lobby")), http.StatusConflict, "channel_exists")
		}
		if rec := f.do(t, http.MethodPut, channelNamePath(f.general.ID), f.rootTok, channelRenameBody(t, "general", "operator-renamed")); rec.Code != http.StatusOK {
			t.Fatalf("operator rename = %d %s", rec.Code, rec.Body)
		}
		if n := countAction(f.audit(t), store.AuditChannelRenameRefused); n != renameAttemptsPerDay {
			t.Errorf("refusal rows = %d, want %d", n, renameAttemptsPerDay)
		}
	})
	t.Run("operator history does not count after demotion", func(t *testing.T) {
		f := newAdminFixture(t)
		from := "general"
		for i := 0; i < renamesPerDay; i++ {
			name := fmt.Sprintf("operator-%d", i)
			if rec := f.do(t, http.MethodPut, channelNamePath(f.general.ID), f.rootTok, channelRenameBody(t, from, name)); rec.Code != http.StatusOK {
				t.Fatalf("operator rename %d = %d %s", i, rec.Code, rec.Body)
			}
			from = name
		}
		if rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/principals/%d/role", f.root.ID), f.own2Tok, `{"role":"member"}`); rec.Code != http.StatusOK {
			t.Fatalf("demote operator = %d %s", rec.Code, rec.Body)
		}
		if rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", from, f.root.ID), f.op2Tok, `{"manages":true}`); rec.Code != http.StatusNoContent {
			t.Fatalf("grant manager = %d %s", rec.Code, rec.Body)
		}
		if rec := f.do(t, http.MethodPut, channelNamePath(f.general.ID), f.rootTok, channelRenameBody(t, from, "first-manager-rename")); rec.Code != http.StatusOK {
			t.Fatalf("first manager rename = %d %s", rec.Code, rec.Body)
		}
	})
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func boolCount(b bool) int {
	if b {
		return 1
	}
	return 0
}

// The rename is audited with who did it, which channel and both names,
// quoted; the denial of a member names the route and the reason only.
func TestRenameChannelAudit(t *testing.T) {
	f, general, _ := renameFixture(t)
	if rec := f.do(t, http.MethodPut, channelNamePath(general.ID), f.rootTok, channelRenameBody(t, "general", `town "to=x"`)); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}
	var renames []store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == store.AuditChannelRenamed {
			renames = append(renames, e)
		}
	}
	want := store.AuditEvent{Actor: fmt.Sprintf("principal:%d", f.root.ID), Subject: fmt.Sprintf("channel:%d", general.ID), Detail: `from="general" to="town \"to=x\""`}
	if len(renames) != 1 || renames[0].Actor != want.Actor || renames[0].Subject != want.Subject || renames[0].Detail != want.Detail {
		t.Errorf("channel_renamed rows = %+v, want one %+v", renames, want)
	}

	rec := f.do(t, http.MethodPut, channelNamePath(general.ID), f.aliceTok, channelRenameBody(t, `town "to=x"`, "evil"))
	assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
	var denial store.AuditEvent
	for _, e := range f.audit(t) {
		if e.Action == "access_denied" {
			denial = e
		}
	}
	if denial.Actor != fmt.Sprintf("principal:%d", f.alice.ID) || denial.Subject != "PUT /v1/channels/{id}/name" || !strings.Contains(denial.Detail, "forbidden") || strings.Contains(denial.Detail, "evil") {
		t.Errorf("denial row = %+v", denial)
	}
}

// With authentication off the route is open like every other, and the rename
// is audited as auditActor gives it.
func TestRenameChannelAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	general, err := f.srv.store.ChannelByName(context.Background(), "general")
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, http.MethodPut, channelNamePath(general.ID), "", channelRenameBody(t, "general", "town square"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if got := decodeBody[schema.RenameChannelResponseV1](t, rec).Channel; got.ID != general.ID || got.Name != "town square" {
		t.Errorf("response channel = %+v", got)
	}
	if n := countAction(f.audit(t), store.AuditChannelRenamed); n != 1 {
		t.Errorf("%d channel_renamed rows, want 1", n)
	}
}

// After a rename, everything that was the channel's is still the channel's,
// under the new name: its messages, members, nets, invites (and their
// preview), read marks and voice room, all of which refer to it by id. The
// old name answers 404 channel_not_found on every route family that takes a
// channel by name, as a name nobody ever had does, and the new name answers
// where the old one did.
func TestRenameChannelKeepsEverythingByID(t *testing.T) {
	f := newInviteFixture(t, Config{})
	ctx := context.Background()
	general, err := f.srv.store.ChannelByName(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	must := func(rec *httptest.ResponseRecorder, want int) *httptest.ResponseRecorder {
		t.Helper()
		if rec.Code != want {
			t.Fatalf("status = %d, want %d; body %s", rec.Code, want, rec.Body)
		}
		return rec
	}

	// The channel's things, made by name before the rename.
	msg := f.postAs(t, f.aliceTok, "general", "before the rename")
	must(f.do(t, http.MethodPost, "/v1/channels/general/nets", f.rootTok, `{"name":"tanks"}`), http.StatusCreated)
	must(f.do(t, http.MethodPut, "/v1/channels/general/read-state", f.aliceTok, fmt.Sprintf(`{"last_read_id":%d}`, msg)), http.StatusOK)
	invite := f.createInvite(t, f.rootTok, openLink("general"))
	room, err := f.srv.store.ChannelVoiceRoom(ctx, general.ID)
	if err != nil {
		t.Fatal(err)
	}
	membersBefore := decodeBody[schema.ListChannelMembersResponseV1](t, must(f.do(t, http.MethodGet, "/v1/channels/general/members", f.rootTok, ""), http.StatusOK))

	must(f.do(t, http.MethodPut, channelNamePath(general.ID), f.rootTok, channelRenameBody(t, "general", "town square")), http.StatusOK)
	const renamed = "town%20square"

	t.Run("messages", func(t *testing.T) {
		list := decodeBody[schema.ListMessagesResponseV2](t, must(f.do(t, http.MethodGet, "/v2/channels/"+renamed+"/messages", f.aliceTok, ""), http.StatusOK))
		if len(list.Messages) != 1 || list.Messages[0].ID != msg || list.Messages[0].ChannelID != general.ID {
			t.Errorf("messages = %+v, want message %d in channel %d", list.Messages, msg, general.ID)
		}
	})
	t.Run("members", func(t *testing.T) {
		after := decodeBody[schema.ListChannelMembersResponseV1](t, must(f.do(t, http.MethodGet, "/v1/channels/"+renamed+"/members", f.rootTok, ""), http.StatusOK))
		if !reflect.DeepEqual(after, membersBefore) {
			t.Errorf("members = %+v, want %+v", after, membersBefore)
		}
	})
	t.Run("nets", func(t *testing.T) {
		nets := decodeBody[schema.ListNetsResponseV1](t, must(f.do(t, http.MethodGet, "/v1/channels/"+renamed+"/nets", f.rootTok, ""), http.StatusOK))
		if len(nets.Nets) != 1 || nets.Nets[0].Name != "tanks" || nets.Nets[0].ChannelID != general.ID {
			t.Errorf("nets = %+v, want tanks in channel %d", nets.Nets, general.ID)
		}
	})
	t.Run("invites", func(t *testing.T) {
		preview := decodeBody[schema.InvitePreviewV1](t, must(f.preview(t, invite.Code), http.StatusOK))
		if !reflect.DeepEqual(preview.Channels, []string{"town square"}) {
			t.Errorf("preview channels = %v, want the new name", preview.Channels)
		}
		list := decodeBody[schema.ListInvitesResponseV1](t, must(f.send(t, http.MethodGet, "/v1/invites", f.rootTok, "", ""), http.StatusOK))
		want := []schema.InviteChannelV1{{ID: general.ID, Name: "town square"}}
		if len(list.Invites) != 1 || !reflect.DeepEqual(list.Invites[0].Channels, want) {
			t.Errorf("invites = %+v, want one granting %+v", list.Invites, want)
		}
	})
	t.Run("read marks", func(t *testing.T) {
		marks := decodeBody[schema.ListReadStateResponseV1](t, must(f.do(t, http.MethodGet, "/v1/read-state", f.aliceTok, ""), http.StatusOK))
		found := false
		for _, m := range marks.ReadState {
			if m.ChannelID == general.ID {
				found = m.LastReadID == msg
			}
		}
		if !found {
			t.Errorf("read state = %+v, want channel %d marked at %d", marks.ReadState, general.ID, msg)
		}
	})
	t.Run("voice room", func(t *testing.T) {
		after, err := f.srv.store.ChannelVoiceRoom(ctx, general.ID)
		if err != nil || after.ID != room.ID || after.RoomName != room.RoomName {
			t.Errorf("voice room = %+v (%v), want %+v", after, err, room)
		}
	})

	// Every route of the table that takes a channel by name, in the path or
	// the query, from the route expectations, with a body that gets past
	// any check made before the channel is looked up.
	bodies := map[string]string{
		"POST /v1/channels/{channel}/messages": `{"body":"hi"}`,
		"POST /v2/channels/{channel}/messages": `{"body":"hi"}`,
	}
	type request struct{ pattern, path, body string }
	var requests []request
	for pattern, exp := range routeExpectations {
		if !strings.Contains(exp.path, "/channels/general/") && !strings.Contains(exp.path, "channel=general") {
			continue
		}
		body := exp.body
		if b, ok := bodies[pattern]; ok {
			body = b
		}
		requests = append(requests, request{pattern, exp.path, body})
	}
	// The sockets take the channel in the query; asked without an upgrade
	// they answer the channel check all the same.
	for _, path := range []string{"/v0/ws", "/v1/ws", "/v2/ws", "/v1/events/ws", "/v1/voice/ws"} {
		requests = append(requests, request{"GET " + path, path + "?channel=general", ""})
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].pattern < requests[j].pattern })
	if len(requests) < 25 {
		t.Fatalf("only %d routes take a channel by name; the expectations have lost some", len(requests))
	}
	for _, rq := range requests {
		t.Run("old name/"+rq.pattern, func(t *testing.T) {
			for _, name := range []string{"general", "never-was"} {
				path := strings.Replace(strings.Replace(rq.path, "/channels/general/", "/channels/"+name+"/", 1), "channel=general", "channel="+name, 1)
				rec := f.do(t, routeMethod(rq.pattern), path, f.rootTok, rq.body)
				assertErrorBody(t, rec, http.StatusNotFound, "channel_not_found")
			}
		})
		t.Run("new name/"+rq.pattern, func(t *testing.T) {
			path := strings.Replace(strings.Replace(rq.path, "/channels/general/", "/channels/"+renamed+"/", 1), "channel=general", "channel="+renamed, 1)
			rec := f.do(t, routeMethod(rq.pattern), path, f.rootTok, rq.body)
			if rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "channel_not_found") {
				t.Errorf("the new name = %d %s", rec.Code, rec.Body)
			}
		})
	}
	// A hook names its channel in the body, and is refused as it always is
	// for a name nobody has: 400 channel_not_found.
	t.Run("old name/POST /v1/hooks", func(t *testing.T) {
		for _, name := range []string{"general", "never-was"} {
			rec := f.do(t, http.MethodPost, "/v1/hooks", f.rootTok, fmt.Sprintf(`{"channel":%q,"principal":%d}`, name, f.bot.ID))
			assertErrorBody(t, rec, http.StatusBadRequest, "channel_not_found")
		}
		must(f.do(t, http.MethodPost, "/v1/hooks", f.rootTok, fmt.Sprintf(`{"channel":"town square","principal":%d}`, f.bot.ID)), http.StatusCreated)
	})
}

// An agent addresses channels by name over MCP too: after a rename the old
// name is channel_not_found to post_message and read_channel, as for a
// channel it is not in, and the new name works with the manifest it had,
// which allows the channel by id.
func TestRenameChannelOverMCP(t *testing.T) {
	f := &interventionFixture{runFixture: newRunFixture(t)}
	if rec := f.do(t, http.MethodPut, channelNamePath(f.general.ID), f.rootTok, channelRenameBody(t, "general", "town square")); rec.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", rec.Code, rec.Body)
	}
	tests := []struct {
		tool    string
		channel string
		want    string
	}{
		{"read_channel", "general", "channel_not_found"},
		{"post_message", "general", "channel_not_found"},
		{"read_channel", "town square", "ok"},
		{"post_message", "town square", "ok"},
	}
	for _, tt := range tests {
		t.Run(tt.tool+" "+tt.channel, func(t *testing.T) {
			args := map[string]any{"channel": tt.channel}
			if tt.tool == "post_message" {
				args["body"] = "hello"
			}
			if got := f.tool(t, f.botTok, tt.tool, args).code; got != tt.want {
				t.Errorf("%s(%q) = %s, want %s", tt.tool, tt.channel, got, tt.want)
			}
		})
	}
}
