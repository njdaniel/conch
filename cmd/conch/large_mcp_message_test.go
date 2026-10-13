package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The largest message an agent can post through MCP reaches a subscriber and
// leaves it connected (issue #237). Before /mcp bounded its requests an agent
// could post a message larger than any client would read, and the frame for it
// ended every subscription in the channel. The body is the worst there is:
// one byte going in, six coming back.
func TestTheLargestMCPMessageReachesASubscriber(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	channel, err := st.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatal(err)
	}
	agent, err := st.CreatePrincipal(ctx, store.PrincipalAgent, "bot")
	if err != nil {
		t.Fatal(err)
	}
	human, err := st.CreatePrincipal(ctx, store.PrincipalHuman, "nick")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddChannelMember(ctx, "system", channel.ID, agent.ID, 0); err != nil {
		t.Fatal(err)
	}
	manifest := schema.PutAgentManifestRequestV1{DisplayName: "bot", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
		Channels: []schema.ChannelGrant{{ChannelID: channel.ID, Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost}}}}
	if _, _, err := st.PutAgentManifest(ctx, "system", agent.ID, manifest); err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Config{AuthMode: server.AuthOff, DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "test",
		MCPBearerTokens: map[string]int64{"token-1": agent.ID}}, st)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	url := "http://" + srv.Addr()
	client, err := cli.NewClient(url, nil)
	if err != nil {
		t.Fatal(err)
	}

	const small = "after the large one"
	errDone := errors.New("done")
	type result struct {
		lengths []int
		err     error
	}
	results := make(chan result, 1)
	go func() {
		var lengths []int
		err := client.SubscribeV2(ctx, "general", func(m schema.MessageV2) error {
			lengths = append(lengths, len(m.Body))
			if m.Body == small {
				return errDone
			}
			return nil
		})
		results <- result{lengths, err}
	}()
	time.Sleep(300 * time.Millisecond)

	mcpPostMessage := func(body string) int {
		t.Helper()
		request := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"channel":"general","body":"` + body + `"}}}`
		req, err := http.NewRequest(http.MethodPost, url+"/mcp", bytes.NewReader([]byte(request)))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer token-1")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	envelope := len(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"post_message","arguments":{"channel":"general","body":""}}}`)
	large := strings.Repeat("<", schema.MaxPostRequestBytes-envelope)
	if code := mcpPostMessage(large + "<"); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("an MCP request one byte over the limit = %d, want 413", code)
	}
	if code := mcpPostMessage(large); code != http.StatusOK {
		t.Fatalf("an MCP request of exactly the limit = %d, want 200", code)
	}
	// A human's small message follows, over REST.
	resp, err := http.Post(url+"/v0/channels/general/messages", "application/json",
		strings.NewReader(fmt.Sprintf(`{"author_id":%d,"body":%q}`, human.ID, small)))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("posting the small message = %d", resp.StatusCode)
	}

	select {
	case r := <-results:
		if !errors.Is(r.err, errDone) {
			t.Fatalf("the subscription ended with %v, want it open until the small message", r.err)
		}
		// Only the message of exactly the limit and the small one: the
		// oversized one was never stored.
		if len(r.lengths) != 2 || r.lengths[0] != len(large) || r.lengths[1] != len(small) {
			t.Errorf("received bodies of %v bytes, want [%d %d]", r.lengths, len(large), len(small))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the subscriber never received the small message")
	}
}
