package adapter

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func envOf(values map[string]string) envLookup {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

// Every CONCH_ADAPTER_* name falls back to its CONCH_BOT_* name; the new name
// wins when both are set.
func TestLoadConfigNamesAndFallbacks(t *testing.T) {
	required := map[string]string{"CONCH_ADAPTER_TOKEN": "t", "CONCH_ADAPTER_PRINCIPAL_ID": "7", "CONCH_ADAPTER_CHANNEL": "ops"}
	legacyRequired := map[string]string{"CONCH_BOT_TOKEN": "t", "CONCH_BOT_PRINCIPAL_ID": "7", "CONCH_BOT_CHANNEL": "ops"}
	merge := func(maps ...map[string]string) map[string]string {
		out := map[string]string{}
		for _, m := range maps {
			for k, v := range m {
				out[k] = v
			}
		}
		return out
	}
	tests := []struct {
		name  string
		env   map[string]string
		check func(config) bool
	}{
		{"legacy names alone", merge(legacyRequired, map[string]string{
			"CONCH_BOT_SERVER": "http://s", "CONCH_BOT_POLL_INTERVAL": "1s", "CONCH_BOT_MAX_BACKOFF": "2s", "CONCH_BOT_CONTEXT_MESSAGES": "3",
			"CONCH_BOT_MODEL": "opus", "CONCH_BOT_REPLY_TIMEOUT": "4s", "CONCH_BOT_LOCK_FILE": "/tmp/l",
		}), func(c config) bool {
			return c.Server == "http://s" && c.Token == "t" && c.PrincipalID == 7 && c.Channel == "ops" && c.PollInterval == time.Second &&
				c.MaxBackoff == 2*time.Second && c.ContextMessages == 3 && c.Model == "opus" && c.ReplyTimeout == 4*time.Second && c.LockFile == "/tmp/l"
		}},
		{"new names alone", merge(required, map[string]string{
			"CONCH_ADAPTER_SERVER": "http://n", "CONCH_ADAPTER_MODEL": "haiku", "CONCH_ADAPTER_LOCK_FILE": "/tmp/n", "CONCH_ADAPTER_POLL_INTERVAL": "3s",
		}), func(c config) bool {
			return c.Server == "http://n" && c.Model == "haiku" && c.LockFile == "/tmp/n" && c.PollInterval == 3*time.Second
		}},
		{"the new name wins", merge(required, legacyRequired, map[string]string{
			"CONCH_ADAPTER_MODEL": "new", "CONCH_BOT_MODEL": "old", "CONCH_ADAPTER_CHANNEL": "newchan", "CONCH_BOT_CHANNEL": "oldchan",
		}), func(c config) bool { return c.Model == "new" && c.Channel == "newchan" }},
		{"an empty new name falls back", merge(legacyRequired, map[string]string{"CONCH_ADAPTER_MODEL": "", "CONCH_BOT_MODEL": "old"}),
			func(c config) bool { return c.Model == "old" }},
		{"defaults", required, func(c config) bool {
			return c.Runtime == "claude-code" && c.Heartbeat == 10*time.Second && c.MaxRepliesPerMinute == 10 && !c.Fixture &&
				c.Model == "sonnet" && reflect.DeepEqual(c.Command.args, strings.Fields(defaultCommand)) && strings.HasSuffix(c.LockFile, "conch-adapter.lock")
		}},
		{"the new settings", merge(required, map[string]string{
			"CONCH_ADAPTER_RUNTIME": "fixture", "CONCH_ADAPTER_HEARTBEAT": "250ms", "CONCH_ADAPTER_MAX_REPLIES_PER_MINUTE": "3",
			"CONCH_ADAPTER_FIXTURE": "1", "CONCH_ADAPTER_CMD": `sh -c 'cat' sh`,
		}), func(c config) bool {
			return c.Runtime == "fixture" && c.Heartbeat == 250*time.Millisecond && c.MaxRepliesPerMinute == 3 && c.Fixture &&
				reflect.DeepEqual(c.Command.args, []string{"sh", "-c", "cat", "sh"})
		}},
		{"the pause bound defaults to the reply timeout", merge(required, map[string]string{"CONCH_ADAPTER_REPLY_TIMEOUT": "45s"}),
			func(c config) bool { return c.MaxPause == 45*time.Second }},
		{"the pause bound is its own setting", merge(required, map[string]string{"CONCH_ADAPTER_REPLY_TIMEOUT": "45s", "CONCH_ADAPTER_MAX_PAUSE": "10m"}),
			func(c config) bool { return c.MaxPause == 10*time.Minute && c.ReplyTimeout == 45*time.Second }},
		{"agents are not answered by default", required, func(c config) bool { return !c.ReplyToAgents }},
		{"reply to agents on for 1", merge(required, map[string]string{"CONCH_ADAPTER_REPLY_TO_AGENTS": "1"}), func(c config) bool { return c.ReplyToAgents }},
		{"reply to agents is only on for 1", merge(required, map[string]string{"CONCH_ADAPTER_REPLY_TO_AGENTS": "true"}), func(c config) bool { return !c.ReplyToAgents }},
		{"fixture is only on for 1", merge(required, map[string]string{"CONCH_ADAPTER_FIXTURE": "true"}), func(c config) bool { return !c.Fixture }},
		{"CLAUDE_BIN replaces the first word of the default", merge(required, map[string]string{"CLAUDE_BIN": "/x/claude"}),
			func(c config) bool { return c.Command.args[0] == "/x/claude" && c.Command.args[1] == "-p" }},
		// A configured command is run as written: a CLAUDE_BIN left over from
		// conch-bot must not turn a fixture script into the real model.
		{"CLAUDE_BIN leaves CONCH_ADAPTER_CMD alone", merge(required, map[string]string{"CLAUDE_BIN": "/x/claude", "CONCH_ADAPTER_CMD": "/x/fixture.sh --flag", "CONCH_ADAPTER_FIXTURE": "1"}),
			func(c config) bool { return reflect.DeepEqual(c.Command.args, []string{"/x/fixture.sh", "--flag"}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := loadConfig(envOf(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(cfg) {
				t.Fatalf("unexpected config: %+v", cfg)
			}
		})
	}
}

func TestLoadConfigRefusals(t *testing.T) {
	base := map[string]string{"CONCH_ADAPTER_TOKEN": "t", "CONCH_ADAPTER_PRINCIPAL_ID": "7", "CONCH_ADAPTER_CHANNEL": "ops"}
	tests := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"unterminated quote", map[string]string{"CONCH_ADAPTER_CMD": `claude -p "{prompt}`}, "CONCH_ADAPTER_CMD"},
		{"bad heartbeat", map[string]string{"CONCH_ADAPTER_HEARTBEAT": "soon"}, "CONCH_ADAPTER_HEARTBEAT"},
		{"zero heartbeat", map[string]string{"CONCH_ADAPTER_HEARTBEAT": "0s"}, "CONCH_ADAPTER_HEARTBEAT"},
		{"bad cap", map[string]string{"CONCH_ADAPTER_MAX_REPLIES_PER_MINUTE": "many"}, "CONCH_ADAPTER_MAX_REPLIES_PER_MINUTE"},
		{"bad pause bound", map[string]string{"CONCH_ADAPTER_MAX_PAUSE": "long"}, "CONCH_ADAPTER_MAX_PAUSE"},
		{"zero pause bound", map[string]string{"CONCH_ADAPTER_MAX_PAUSE": "0s"}, "CONCH_ADAPTER_MAX_PAUSE"},
		{"zero cap", map[string]string{"CONCH_ADAPTER_MAX_REPLIES_PER_MINUTE": "0"}, "CONCH_ADAPTER_MAX_REPLIES_PER_MINUTE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := map[string]string{}
			for k, v := range base {
				values[k] = v
			}
			for k, v := range tt.set {
				values[k] = v
			}
			_, err := loadConfig(envOf(values))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want one naming %s", err, tt.want)
			}
		})
	}
	t.Run("a token is never in an error", func(t *testing.T) {
		values := map[string]string{"CONCH_ADAPTER_TOKEN": "SECRET-TOKEN", "CONCH_ADAPTER_PRINCIPAL_ID": "nope", "CONCH_ADAPTER_CHANNEL": "ops"}
		if _, err := loadConfig(envOf(values)); err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
			t.Fatalf("error = %v", err)
		}
	})
}
