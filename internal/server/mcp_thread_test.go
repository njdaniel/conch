package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// A reply over MCP (issue #1171) is the reply over REST: one level, the root's
// audience, the root's visibility, and one answer, "thread not found", for a
// root the agent cannot see, one that does not exist, and one in another
// channel.

func (f *scopedFixture) mcpReply(t *testing.T, who string, root int64, text string) (mcpOutcome, schema.MessageV2) {
	t.Helper()
	out := f.mcpCall(t, f.p(who).token, "post_message", map[string]any{"channel": "ops", "body": text, "thread_root_id": root})
	if out.status != http.StatusOK || out.isError {
		return out, schema.MessageV2{}
	}
	var resp struct {
		Result struct {
			Structured schema.PostMessageResponseV2 `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out.body), &resp); err != nil {
		t.Fatalf("decode post_message %s: %v", out.body, err)
	}
	return out, resp.Result.Structured.Message
}

func TestMCPThreadReplyMatchesREST(t *testing.T) {
	f := newScopedFixture(t)
	f.add(t, "gboth", store.PrincipalAgent, true)
	f.manifest(t, "gboth", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost,
		schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
	f.seat(t, "net1", "gboth", schema.NetRoleMember)
	f.add(t, "gmon", store.PrincipalAgent, true)
	f.manifest(t, "gmon", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost,
		schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
	f.seat(t, "net1", "gmon", schema.NetRoleMonitor)
	other, err := f.srv.store.CreateChannel(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	otherMsg, err := f.srv.store.InsertMessageV1(context.Background(), other.ID, f.p("ann").ID, "elsewhere", nil)
	if err != nil {
		t.Fatal(err)
	}

	wide := f.mustPost(t, "ann", `{"body":"wide"}`)
	netRoot := f.mustPost(t, "ann", `{"body":"net",`+netAudience(f.net1.ID)+`}`)
	net2Root := f.mustPost(t, "nina", `{"body":"net2",`+netAudience(f.net2.ID)+`}`)
	toGboth := f.mustPost(t, "ann", `{"body":"whisper",`+whisperAudience(f.p("gboth").ID)+`}`)
	toWes := f.mustPost(t, "ann", `{"body":"private",`+whisperAudience(f.p("wes").ID)+`}`)
	reply := f.mustReply(t, "ann", wide)

	const (
		ok       = "ok"
		noThread = "thread_not_found"
	)
	tests := []struct {
		name string
		who  string
		root int64
		want string
		// reason is the access_denied reason the refusal writes, "" for none.
		reason string
	}{
		{"channel-wide root", "gboth", wide, ok, ""},
		{"channel-wide root, post grant only", "aria", wide, ok, ""},
		{"net root, on the net", "gboth", netRoot, ok, ""},
		{"net root, no post_net grant", "aria", netRoot, "forbidden", denyAudienceGrant},
		{"net root, monitor seat", "gmon", netRoot, "forbidden", denyNetMonitorOnly},
		{"net root, net the agent is not on", "gboth", net2Root, noThread, ""},
		{"whisper root, addressed to the agent", "gboth", toGboth, ok, ""},
		{"whisper root, not addressed to the agent", "gboth", toWes, noThread, ""},
		{"whisper root, post grant only, not addressed", "aria", toWes, noThread, ""},
		{"a reply is not a root", "gboth", reply, noThread, ""},
		{"root that does not exist", "gboth", 999999, noThread, ""},
		{"root in another channel", "gboth", otherMsg.ID, noThread, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := f.messageCount(t)
			beforeDenials := len(f.denials(t))
			viaMCP, mcpMsg := f.mcpReply(t, tt.who, tt.root, "re")
			afterMCP := f.messageCount(t)
			// Exactly the denial the refusal writes, and none for an answer
			// that is a success or a plain "thread not found".
			events := f.denials(t)[beforeDenials:]
			wantEvents := 0
			if tt.reason != "" {
				wantEvents = 1
			}
			if len(events) != wantEvents {
				t.Fatalf("access_denied events = %d, want %d: %+v", len(events), wantEvents, events)
			}
			if tt.reason != "" && (events[0].Subject != "mcp:post_message" || !strings.Contains(events[0].Detail, tt.reason)) {
				t.Errorf("denial = %+v, want subject mcp:post_message and reason %s", events[0], tt.reason)
			}
			rest := f.postReply(t, tt.who, tt.root)

			if got := viaMCP.result(); got != tt.want {
				t.Fatalf("MCP answers %q, want %q (%s)", got, tt.want, viaMCP.body)
			}
			if tt.want == ok {
				if rest.status != http.StatusCreated {
					t.Fatalf("REST answers %d %s", rest.status, rest.body)
				}
				var restResp schema.PostMessageResponseV2
				if err := json.Unmarshal([]byte(rest.body), &restResp); err != nil {
					t.Fatal(err)
				}
				if mcpMsg.ThreadRootID != tt.root {
					t.Errorf("MCP reply thread_root_id = %d, want %d", mcpMsg.ThreadRootID, tt.root)
				}
				if !reflect.DeepEqual(mcpMsg.Audience, restResp.Message.Audience) {
					t.Errorf("audience: MCP %+v, REST %+v", mcpMsg.Audience, restResp.Message.Audience)
				}
				if err := mcpMsg.Validate(); err != nil {
					t.Errorf("MCP reply is not a valid v2 message: %v", err)
				}
				if afterMCP != before+1 {
					t.Errorf("MCP reply stored %d messages", afterMCP-before)
				}
				return
			}
			if rest.status < 400 || errorCode(t, rest) != viaMCP.code {
				t.Errorf("MCP answers %q, REST %d %s", viaMCP.code, rest.status, rest.body)
			}
			if afterMCP != before {
				t.Errorf("a refused reply stored %d messages", afterMCP-before)
			}
		})
	}

	t.Run("audience with a thread root", func(t *testing.T) {
		before := f.messageCount(t)
		for _, root := range []int64{wide, 999999} {
			out := f.mcpCall(t, f.p("gboth").token, "post_message", map[string]any{
				"channel": "ops", "body": "x", "thread_root_id": root, "audience": mcpNet(f.net1.ID),
			})
			if out.result() != "thread_audience_conflict" {
				t.Errorf("root %d: MCP answers %s", root, out.result())
			}
		}
		if f.messageCount(t) != before {
			t.Error("a refused reply stored a message")
		}
	})

	t.Run("negative root", func(t *testing.T) {
		out := f.mcpCall(t, f.p("gboth").token, "post_message", map[string]any{"channel": "ops", "body": "x", "thread_root_id": -1})
		rest := f.postV2(t, "gboth", `{"body":"x","thread_root_id":-1}`)
		if out.result() != "invalid_request" || errorCode(t, rest) != "invalid_request" {
			t.Errorf("MCP %s, REST %d %s", out.result(), rest.status, rest.body)
		}
	})

	t.Run("non-member and unknown channel", func(t *testing.T) {
		for _, ch := range []string{"nowhere", "other"} {
			out := f.mcpCall(t, f.p("gboth").token, "post_message", map[string]any{"channel": ch, "body": "x", "thread_root_id": wide})
			if out.result() != "channel_not_found" {
				t.Errorf("%s: MCP answers %s", ch, out.result())
			}
		}
	})

	t.Run("read_channel shows the thread and the reply stays in it", func(t *testing.T) {
		_, msg := f.mcpReply(t, "gboth", wide, "visible in the read")
		messages, _ := f.mcpRead(t, "gboth", 0, 100)
		var found bool
		for _, m := range messages {
			if m.ID == msg.ID {
				found = true
				if m.ThreadRootID != wide {
					t.Errorf("read_channel thread_root_id = %d, want %d", m.ThreadRootID, wide)
				}
			}
		}
		if !found {
			t.Fatalf("read_channel did not return reply %d; got %v", msg.ID, fmt.Sprint(messageIDs(messages)))
		}
	})
}
