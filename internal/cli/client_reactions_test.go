package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

var reactionTestTime = schema.NewTimestamp(time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC))

func reactionTestSummary(messageID int64, counts ...schema.ReactionCountV1) schema.ReactionSummaryV1 {
	if counts == nil {
		counts = []schema.ReactionCountV1{}
	}
	return schema.ReactionSummaryV1{Schema: schema.ReactionSummarySchemaV1, ChannelID: 3, MessageID: messageID, Reactions: counts, AsOf: reactionTestTime}
}

func TestReactionClientRoutes(t *testing.T) {
	compound := "🏳️‍🌈"
	keycap := "#️⃣"
	tests := []struct {
		name   string
		method string
		path   string
		query  string
		call   func(*Client) error
	}{
		{name: "set", method: http.MethodPut, path: "/v2/channels/team%2Fops/messages/42/reactions/" + url.PathEscape(compound), call: func(c *Client) error {
			_, err := c.SetReaction(context.Background(), "team/ops", 42, compound)
			return err
		}},
		{name: "remove canonicalizes", method: http.MethodDelete, path: "/v2/channels/general/messages/42/reactions/❤️", call: func(c *Client) error {
			_, err := c.RemoveReaction(context.Background(), "general", 42, "❤")
			return err
		}},
		{name: "keycap escapes fragment", method: http.MethodPut, path: "/v2/channels/general/messages/42/reactions/%23%EF%B8%8F%E2%83%A3", call: func(c *Client) error {
			_, err := c.SetReaction(context.Background(), "general", 42, keycap)
			return err
		}},
		{name: "summary", method: http.MethodGet, path: "/v2/channels/general/messages/42/reactions", call: func(c *Client) error {
			_, err := c.ReactionSummary(context.Background(), "general", 42)
			return err
		}},
		{name: "batch", method: http.MethodGet, path: "/v2/channels/general/reactions", query: "messages=42%2C43", call: func(c *Client) error {
			_, err := c.ReactionSummaries(context.Background(), "general", []int64{42, 43})
			return err
		}},
		{name: "reactors", method: http.MethodGet, path: "/v2/channels/general/messages/42/reactions/👍", query: "after=7&limit=100", call: func(c *Client) error {
			_, err := c.ListReactors(context.Background(), "general", 42, "👍", 7, 100)
			return err
		}},
		{name: "moderate one", method: http.MethodDelete, path: "/v2/channels/general/messages/42/reactions/👍/7", call: func(c *Client) error {
			_, err := c.ModerateReaction(context.Background(), "general", 42, 7, "👍")
			return err
		}},
		{name: "moderate all", method: http.MethodDelete, path: "/v2/channels/general/messages/42/reactors/7", call: func(c *Client) error {
			_, err := c.ModerateReaction(context.Background(), "general", 42, 7, "")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || r.URL.EscapedPath() != tt.path && r.URL.Path != tt.path || r.URL.RawQuery != tt.query {
					t.Errorf("request = %s %s?%s, want %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery, tt.method, tt.path, tt.query)
				}
				switch {
				case strings.HasSuffix(r.URL.Path, "/reactions") && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/messages/"):
					_ = json.NewEncoder(w).Encode(reactionTestSummary(42))
				case r.URL.Path == "/v2/channels/general/reactions":
					_ = json.NewEncoder(w).Encode(schema.ReactionSummariesResponseV1{Summaries: []schema.ReactionSummaryV1{reactionTestSummary(42), reactionTestSummary(43)}})
				case r.Method == http.MethodGet:
					_ = json.NewEncoder(w).Encode(schema.ListReactorsResponseV1{Reactors: []schema.ReactionV1{}})
				default:
					_ = json.NewEncoder(w).Encode(reactionTestSummary(42))
				}
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.call(client); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReactionClientRefusals(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		code        string
		unsupported bool
		want        string
	}{
		{name: "denied access", status: 404, body: `{"code":"channel_not_found","message":"channel not found"}`, code: "channel_not_found", want: "channel_not_found"},
		{name: "guest history restriction", status: 404, body: `{"code":"message_not_found","message":"message not found"}`, code: schema.ErrorCodeMessageNotFound, want: "message_not_found"},
		{name: "permission", status: 403, body: `{"code":"reaction_not_permitted","message":"you may not react"}`, code: schema.ErrorCodeReactionNotPermitted, want: "reaction_not_permitted"},
		{name: "old server", status: 404, body: "404 page not found\n", unsupported: true, want: "404"},
		{name: "hostile error escaped", status: 403, body: `{"code":"forbidden","message":"bad\u001b[31m\u202e"}`, code: "forbidden", want: `\x1b[31m\u202e`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.SetReaction(context.Background(), "general", 42, "👍")
			if err == nil || !strings.Contains(err.Error(), tt.want) || IsServerCode(err, "forbidden") != (tt.code == "forbidden") {
				t.Fatalf("error = %q, want %q and code %q", err, tt.want, tt.code)
			}
			if tt.code != "" && !IsServerCode(err, tt.code) {
				t.Errorf("error code does not match %q: %v", tt.code, err)
			}
			if got := ReactionsUnsupported(err); got != tt.unsupported {
				t.Errorf("ReactionsUnsupported = %v, want %v", got, tt.unsupported)
			}
			if strings.Contains(err.Error(), "\x1b") || strings.Contains(err.Error(), "\u202e") {
				t.Errorf("raw hostile server text reached error %q", err)
			}
		})
	}
}

func TestReactionClientInputBounds(t *testing.T) {
	client, err := NewClient("http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		call func() error
	}{
		{"invalid emoji", func() error { _, err := client.SetReaction(context.Background(), "general", 1, ":+1:"); return err }},
		{"invalid message", func() error { _, err := client.ReactionSummary(context.Background(), "general", 0); return err }},
		{"empty batch", func() error { _, err := client.ReactionSummaries(context.Background(), "general", nil); return err }},
		{"duplicate batch", func() error {
			_, err := client.ReactionSummaries(context.Background(), "general", []int64{1, 1})
			return err
		}},
		{"negative cursor", func() error {
			_, err := client.ListReactors(context.Background(), "general", 1, "👍", -1, 0)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); err == nil {
				t.Fatal("invalid input was sent")
			}
		})
	}
}

func TestReactionEventDispatch(t *testing.T) {
	frame, err := json.Marshal(reactionTestSummary(42, schema.ReactionCountV1{Emoji: "👍", Count: 3, Me: true}))
	if err != nil {
		t.Fatal(err)
	}
	var got schema.ReactionSummaryV1
	if err := dispatchEvent(frame, EventHandler{Reaction: func(summary schema.ReactionSummaryV1) error {
		got = summary
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	if got.MessageID != 42 || len(got.Reactions) != 1 || !got.Reactions[0].Me {
		t.Errorf("summary = %+v", got)
	}
	if err := dispatchEvent(json.RawMessage(`{"schema":"conch.future.v1"}`), EventHandler{}); err != nil {
		t.Fatalf("unknown frame was not ignored: %v", err)
	}
	stop := errors.New("stop")
	if err := dispatchEvent(frame, EventHandler{Reaction: func(schema.ReactionSummaryV1) error { return stop }}); !errors.Is(err, stop) {
		t.Errorf("handler error = %v, want stop", err)
	}
}
