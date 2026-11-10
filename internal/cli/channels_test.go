package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// `conch channels …` (ADR-013, issue #994) against a real conchd handler.

// cli runs `conch <args> --server <url>` with token as CONCH_TOKEN.
func (e *adminEnv) cli(t *testing.T, token string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("CONCH_TOKEN", token)
	return runCLI(t, "", channelServerArgs(args, e.url)...)
}

func channelServerArgs(args []string, url string) []string {
	for i, arg := range args {
		if arg != "--" {
			continue
		}
		withServer := make([]string, 0, len(args)+2)
		withServer = append(withServer, args[:i]...)
		withServer = append(withServer, "--server", url)
		return append(withServer, args[i:]...)
	}
	return append(args, "--server", url)
}

func TestChannelsCommands(t *testing.T) {
	e := newAdminEnv(t)
	steps := []struct {
		name  string
		token string
		args  []string
		want  string
	}{
		{"a member makes a public channel", e.memberToken, []string{"channels", "create", "lounge"}, "created public channel lounge (id 1)\n"},
		{"and a private one", e.memberToken, []string{"channels", "create", "--private", "den"}, "created private channel den (id 2)\n"},
		{"flags may follow the name", e.memberToken, []string{"channels", "create", "attic", "--private"}, "created private channel attic (id 3)\n"},
		{"an operator makes one", e.operatorToken, []string{"channels", "create", "ops"}, "created public channel ops (id 4)\n"},
		{"a member's directory", e.memberToken, []string{"channels", "list"}, "1 public - joined - - lounge\n2 private - joined - - den\n3 private - joined - - attic\n4 public - - - - ops\n"},
		{"the operator sees the public ones only", e.operatorToken, []string{"channels", "list"}, "1 public - - - - lounge\n4 public - joined - - ops\n"},
		{"join", e.operatorToken, []string{"channels", "join", "lounge"}, "you are in channel lounge\n"},
		{"join again is the same answer", e.operatorToken, []string{"channels", "join", "lounge"}, "you are in channel lounge\n"},
		{"end of options applies to other verbs", e.operatorToken, []string{"channels", "join", "--", "lounge"}, "you are in channel lounge\n"},
		{"the operator sees every channel with counts", e.operatorToken, []string{"channels", "list", "--all"},
			"1 public - joined 2 - lounge\n2 private - - 1 - den\n3 private - - 1 - attic\n4 public - joined 1 - ops\n"},
		{"leave", e.operatorToken, []string{"channels", "leave", "lounge"}, "left channel lounge\n"},
	}
	for _, st := range steps {
		t.Run(st.name, func(t *testing.T) {
			stdout, stderr, err := e.cli(t, st.token, st.args...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if stdout != st.want || stderr != "" {
				t.Errorf("stdout = %q, stderr = %q; want %q and nothing", stdout, stderr, st.want)
			}
		})
	}
}

func TestChannelsRefusals(t *testing.T) {
	e := newAdminEnv(t)
	for _, args := range [][]string{{"channels", "create", "lounge"}, {"channels", "create", "--private", "den"}} {
		if _, _, err := e.cli(t, e.memberToken, args...); err != nil {
			t.Fatal(err)
		}
	}
	// An agent, which no community route lets in.
	if _, _, err := e.admin(t, e.operatorToken, "", "principal", "create", "--kind", "agent", "bot"); err != nil {
		t.Fatal(err)
	}
	agentToken, _, err := e.admin(t, e.operatorToken, "", "credential", "issue", "3", "--label", "bot")
	if err != nil {
		t.Fatal(err)
	}
	agentToken = strings.TrimSpace(agentToken)
	const agents = "only a person in the community may do this: agents and people outside it cannot"
	tests := []struct {
		name  string
		token string
		args  []string
		code  string
		line  string
	}{
		{"an agent may not create", agentToken, []string{"channels", "create", "bots"}, "forbidden", agents},
		{"an agent may not leave", agentToken, []string{"channels", "leave", "lounge"}, "forbidden", agents},
		{"an agent may not list", agentToken, []string{"channels", "list"}, "forbidden", agents},
		{"a member may not list everything", e.memberToken, []string{"channels", "list", "--all"}, "forbidden", "only an operator may do this"},
		{"a name in use", e.operatorToken, []string{"channels", "create", "lounge"}, "channel_exists", "there is already a channel with that name, perhaps one you are not in"},
		{"a private channel's name is the same line", e.operatorToken, []string{"channels", "create", "den"}, "channel_exists", "there is already a channel with that name, perhaps one you are not in"},
		{"a name that reads the same is the same line", e.operatorToken, []string{"channels", "create", "Lounge"}, "channel_exists", "there is already a channel with that name, perhaps one you are not in"},
		{"a private channel is not found", e.operatorToken, []string{"channels", "join", "den"}, "channel_not_found", "no such channel, or you cannot see it"},
		{"a channel that does not exist is the same line", e.operatorToken, []string{"channels", "join", "nowhere"}, "channel_not_found", "no such channel, or you cannot see it"},
		{"the last member of a private channel", e.memberToken, []string{"channels", "leave", "den"}, schema.ErrorCodeChannelLastMember, "you are the last member of this private channel"},
		{"leaving a channel not joined", e.operatorToken, []string{"channels", "leave", "den"}, "channel_not_found", "no such channel, or you cannot see it"},
	}
	var collisions []string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := e.cli(t, tt.token, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.line) {
				t.Fatalf("error = %v, want a line with %q", err, tt.line)
			}
			if !IsServerCode(err, tt.code) {
				t.Errorf("error = %v, want the server's %s underneath", err, tt.code)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty", stdout)
			}
			if tt.code == "channel_exists" {
				collisions = append(collisions, err.Error())
			}
		})
	}
	for _, c := range collisions {
		if c != collisions[0] {
			t.Errorf("collisions print different lines: %q and %q", collisions[0], c)
		}
	}
}

func TestChannelsJSON(t *testing.T) {
	e := newAdminEnv(t)
	strict := func(t *testing.T, raw string, into any) {
		t.Helper()
		dec := json.NewDecoder(strings.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(into); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
		if dec.More() {
			t.Fatalf("extra data after the document in %q", raw)
		}
	}
	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, out string)
	}{
		{"create", []string{"channels", "create", "lounge"}, func(t *testing.T, out string) {
			var r schema.CreateChannelResponseV1
			strict(t, out, &r)
			if r.Channel.Name != "lounge" || r.Channel.Visibility != schema.ChannelPublic {
				t.Errorf("response = %+v", r)
			}
		}},
		{"list", []string{"channels", "list"}, func(t *testing.T, out string) {
			var r schema.ChannelDirectoryResponseV1
			strict(t, out, &r)
			if len(r.Channels) != 1 {
				t.Errorf("channels = %+v", r.Channels)
			}
		}},
		{"join", []string{"channels", "join", "lounge"}, func(t *testing.T, out string) {
			var r schema.JoinChannelResponseV1
			strict(t, out, &r)
			if r.Channel.Name != "lounge" {
				t.Errorf("response = %+v", r)
			}
		}},
		{"leave has no document", []string{"channels", "leave", "lounge"}, func(t *testing.T, out string) {
			if out != "" {
				t.Errorf("stdout = %q, want empty", out)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, _, err := e.cli(t, e.operatorToken, append(tt.args, "--json")...)
			if err != nil {
				t.Fatalf("%v: %v", tt.args, err)
			}
			tt.check(t, stdout)
		})
	}
}

// An empty directory is an empty JSON array, never null.
func TestChannelsListEmptyJSON(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"channels":null}`)) }))
	defer srv.Close()
	stdout, _, err := runCLI(t, "", "channels", "list", "--json", "--server", srv.URL)
	if err != nil || stdout != "{\"channels\":[]}\n" {
		t.Errorf("stdout = %q, err = %v; want an empty array", stdout, err)
	}
}

func TestChannelsTopic(t *testing.T) {
	const managerOnly = "only an owner, an operator holding manage_channels, or a channel manager may set the topic"
	tests := []struct {
		name         string
		args         []string
		currentTopic string
		write        bool
		wantTopic    string
		wantBody     string
		status       int
		code         string
		wantOut      string
		wantErr      string
		wantRequests int32
	}{
		{name: "print", args: []string{"channels", "topic", "ops"}, currentTopic: "Incidents and their follow-ups", wantOut: "Incidents and their follow-ups\n", wantRequests: 1},
		{name: "print nothing when unset", args: []string{"channels", "topic", "ops"}, wantRequests: 1},
		{name: "print through safeLine", args: []string{"channels", "topic", "ops"}, currentTopic: "bad\n\u202e", wantOut: "bad\\n\\u202e\n", wantRequests: 1},
		{name: "set", args: []string{"channels", "topic", "ops", "Incidents and their follow-ups"}, write: true, wantTopic: "Incidents and their follow-ups", wantOut: "channel ops topic: Incidents and their follow-ups\n", wantRequests: 2},
		{name: "clear", args: []string{"channels", "topic", "ops", "--clear"}, write: true, wantOut: "channel ops topic cleared\n", wantRequests: 2},
		{name: "literal clear after end of options", args: []string{"channels", "topic", "--", "ops", "--clear"}, write: true, wantTopic: "--clear", wantBody: `{"topic":"--clear"}`, wantOut: "channel ops topic: --clear\n", wantRequests: 2},
		{name: "literal single dash text after end of options", args: []string{"channels", "topic", "--", "ops", "-maintenance"}, write: true, wantTopic: "-maintenance", wantBody: `{"topic":"-maintenance"}`, wantOut: "channel ops topic: -maintenance\n", wantRequests: 2},
		{name: "clear before end of options", args: []string{"channels", "topic", "--clear", "--", "ops"}, write: true, wantBody: `{"topic":""}`, wantOut: "channel ops topic cleared\n", wantRequests: 2},
		{name: "blank text is refused", args: []string{"channels", "topic", "ops", "  \t"}, wantErr: "use --clear"},
		{name: "empty text is refused", args: []string{"channels", "topic", "ops", ""}, wantErr: "use --clear"},
		{name: "usage mentions end of options", args: []string{"channels", "topic", "ops", "a", "b"}, wantErr: "put -- before"},
		{name: "clear with text is a usage error", args: []string{"channels", "topic", "ops", "text", "--clear"}, wantErr: "--clear cannot be used with text"},
		{name: "json", args: []string{"channels", "topic", "ops", "new topic", "--json"}, write: true, wantTopic: "new topic", wantOut: "{\"channel\":{\"id\":42,\"name\":\"ops\",\"created_at\":\"0001-01-01T00:00:00Z\",\"topic\":\"new topic\"}}\n", wantRequests: 2},
		{name: "invalid request refusal", args: []string{"channels", "topic", "ops", "bad"}, write: true, wantTopic: "bad", status: http.StatusBadRequest, code: "invalid_request", wantErr: "the server did not accept the request", wantRequests: 2},
		{name: "forbidden refusal", args: []string{"channels", "topic", "ops", "bad"}, write: true, wantTopic: "bad", status: http.StatusForbidden, code: "forbidden", wantErr: managerOnly, wantRequests: 2},
		{name: "write not found refusal", args: []string{"channels", "topic", "ops", "bad"}, write: true, wantTopic: "bad", status: http.StatusNotFound, code: "channel_not_found", wantErr: "no such channel, or you cannot see it", wantRequests: 2},
		{name: "unknown channel", args: []string{"channels", "topic", "nowhere"}, wantErr: "no such channel, or you cannot see it", wantRequests: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			var requests atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch r.URL.Path {
				case "/v1/channels":
					if r.Method != http.MethodGet {
						t.Errorf("list method = %s, want GET", r.Method)
					}
					_ = json.NewEncoder(w).Encode(schema.ListChannelsResponse{Channels: []schema.ChannelV0{{ID: 42, Name: "ops", Topic: tt.currentTopic}}})
				case "/v1/channels/42/topic":
					if !tt.write {
						t.Errorf("unexpected write: %s %s", r.Method, r.URL.Path)
					}
					if r.Method != http.MethodPut {
						t.Errorf("write method = %s, want PUT", r.Method)
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read write: %v", err)
					}
					if tt.wantBody != "" && string(body) != tt.wantBody {
						t.Errorf("body = %q, want %q", body, tt.wantBody)
					}
					var request schema.SetChannelTopicRequestV1
					if err := json.Unmarshal(body, &request); err != nil {
						t.Errorf("decode write: %v", err)
					}
					if request.Topic != tt.wantTopic {
						t.Errorf("topic = %q, want %q", request.Topic, tt.wantTopic)
					}
					if tt.status >= http.StatusBadRequest {
						w.WriteHeader(tt.status)
						_ = json.NewEncoder(w).Encode(schema.Error{Code: tt.code, Message: "server detail"})
						return
					}
					_ = json.NewEncoder(w).Encode(schema.SetChannelTopicResponseV1{Channel: schema.ChannelV0{ID: 42, Name: "ops", Topic: request.Topic}})
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer fake.Close()
			stdout, _, err := runCLI(t, "", channelServerArgs(tt.args, fake.URL)...)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("run: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
			if stdout != tt.wantOut {
				t.Errorf("stdout = %q, want %q", stdout, tt.wantOut)
			}
			if got := requests.Load(); got != tt.wantRequests {
				t.Errorf("requests = %d, want %d", got, tt.wantRequests)
			}
		})
	}
}

func TestChannelsListTopicOutput(t *testing.T) {
	const document = `{"channels":[{"id":42,"name":"ops","created_at":"2026-11-07T00:00:00Z","visibility":"public","joined":true,"topic":"Incidents and their follow-ups"}]}`
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "text format is unchanged", args: []string{"channels", "list"}, want: "42 public - joined - - ops\n"},
		{name: "json carries topic", args: []string{"channels", "list", "--json"}, want: `{"channels":[{"id":42,"name":"ops","created_at":"2026-11-07T00:00:00.000Z","visibility":"public","joined":true,"topic":"Incidents and their follow-ups"}]}` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(document))
			}))
			defer fake.Close()
			stdout, _, err := runCLI(t, "", append(tt.args, "--server", fake.URL)...)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if stdout != tt.want {
				t.Errorf("stdout = %q, want %q", stdout, tt.want)
			}
		})
	}
}

func TestChannelsUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no verb but a flag", []string{"channels"}, `unknown channels subcommand "--server" (want list, join, leave, create, topic)`},
		{"unknown verb", []string{"channels", "frob"}, `unknown channels subcommand "frob"`},
		{"list takes nothing", []string{"channels", "list", "general"}, "takes no arguments"},
		{"join needs a channel", []string{"channels", "join"}, "expected <channel>"},
		{"leave needs a channel", []string{"channels", "leave"}, "expected <channel>"},
		{"create needs a name", []string{"channels", "create"}, "expected <name>"},
		{"create takes one name", []string{"channels", "create", "a", "b"}, "expected <name>"},
		{"a blank name", []string{"channels", "create", " "}, "name"},
		{"topic needs a channel", []string{"channels", "topic"}, "expected <channel> [<text>]"},
		{"topic takes one text", []string{"channels", "topic", "ops", "a", "b"}, "expected <channel> [<text>]"},
		{"all is for list", []string{"channels", "join", "--all", "general"}, "flag provided but not defined: -all"},
		{"private is for create", []string{"channels", "list", "--private"}, "flag provided but not defined: -private"},
		{"clear is for topic", []string{"channels", "list", "--clear"}, "flag provided but not defined: -clear"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			var requests atomic.Int32
			fake := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
			defer fake.Close()
			_, _, err := runCLI(t, "", append(tt.args, "--server", fake.URL)...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if requests.Load() != 0 {
				t.Errorf("%d requests were sent, want none", requests.Load())
			}
		})
	}
}

// A topic read with --json prints the element channels list --json carries
// for that channel, byte for byte.
func TestChannelsTopicJSONMatchesListElement(t *testing.T) {
	const document = `{"channels":[` +
		`{"id":41,"name":"general","created_at":"2026-11-06T00:00:00Z","visibility":"public","joined":true},` +
		`{"id":42,"name":"ops","created_at":"2026-11-07T00:00:00Z","visibility":"private","joined":false,"members":3,"managers":[{"id":7,"name":"nick"}],"topic":"Incidents"}]}`
	tests := []struct {
		channel string
		wantErr string
	}{
		{channel: "ops"},
		{channel: "general"},
		{channel: "nowhere", wantErr: "no such channel, or you cannot see it"},
	}
	for _, tt := range tests {
		t.Run(tt.channel, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/directory/channels" {
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
				_, _ = w.Write([]byte(document))
			}))
			defer fake.Close()
			listOut, _, err := runCLI(t, "", "channels", "list", "--json", "--server", fake.URL)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			var list schema.ChannelDirectoryResponseV1
			if err := json.Unmarshal([]byte(listOut), &list); err != nil {
				t.Fatalf("decode list: %v", err)
			}
			topicOut, _, err := runCLI(t, "", "channels", "topic", tt.channel, "--json", "--server", fake.URL)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("topic: %v", err)
			}
			var want string
			for _, c := range list.Channels {
				if c.Name == tt.channel {
					b, _ := json.Marshal(c)
					want = string(b) + "\n"
				}
			}
			if topicOut != want {
				t.Errorf("topic --json = %q, want the list element %q", topicOut, want)
			}
		})
	}
}
