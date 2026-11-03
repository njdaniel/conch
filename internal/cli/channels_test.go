package cli

import (
	"encoding/json"
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
	return runCLI(t, "", append(args, "--server", e.url)...)
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
		{"a member's directory", e.memberToken, []string{"channels", "list"}, "1 public - joined - lounge\n2 private - joined - den\n3 private - joined - attic\n4 public - - - ops\n"},
		{"the operator sees the public ones only", e.operatorToken, []string{"channels", "list"}, "1 public - - - lounge\n4 public - joined - ops\n"},
		{"join", e.operatorToken, []string{"channels", "join", "lounge"}, "you are in channel lounge\n"},
		{"join again is the same answer", e.operatorToken, []string{"channels", "join", "lounge"}, "you are in channel lounge\n"},
		{"the operator sees every channel with counts", e.operatorToken, []string{"channels", "list", "--all"},
			"1 public - joined 2 lounge\n2 private - - 1 den\n3 private - - 1 attic\n4 public - joined 1 ops\n"},
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
	tests := []struct {
		name  string
		token string
		args  []string
		code  string
		line  string
	}{
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

func TestChannelsUsageErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no verb but a flag", []string{"channels"}, `unknown channels subcommand "--server" (want list, join, leave, create)`},
		{"unknown verb", []string{"channels", "frob"}, `unknown channels subcommand "frob"`},
		{"list takes nothing", []string{"channels", "list", "general"}, "takes no arguments"},
		{"join needs a channel", []string{"channels", "join"}, "expected <channel>"},
		{"leave needs a channel", []string{"channels", "leave"}, "expected <channel>"},
		{"create needs a name", []string{"channels", "create"}, "expected <name>"},
		{"create takes one name", []string{"channels", "create", "a", "b"}, "expected <name>"},
		{"a blank name", []string{"channels", "create", " "}, "name"},
		{"all is for list", []string{"channels", "join", "--all", "general"}, "flag provided but not defined: -all"},
		{"private is for create", []string{"channels", "list", "--private"}, "flag provided but not defined: -private"},
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
