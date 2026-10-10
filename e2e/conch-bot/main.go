// Command conch-bot-check exercises conch-bot against real conchd and fake
// claude processes. It requires loopback sockets and is intended for CI or a
// developer machine, not restricted sandboxes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

const (
	token         = "conch-bot-e2e-token"      // #nosec G101 -- test-only local bearer token
	peerToken     = "conch-peer-e2e-token"     // #nosec G101 -- test-only local bearer token
	outsiderToken = "conch-outsider-e2e-token" // #nosec G101 -- test-only local bearer token
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "conch-bot-check: FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("conch-bot-check: PASS")
}

type harness struct {
	dir, conchd, bot, data, claude, lock, addr string
	server, botProc                            *exec.Cmd
	botID, humanID, peerID, outsiderID, netID  int64
	watchNetID                                 int64
}

func run() error {
	dir, err := os.MkdirTemp("", "conch-bot-e2e-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	h := &harness{dir: dir, conchd: filepath.Join(dir, "conchd"), bot: filepath.Join(dir, "conch-bot"), data: filepath.Join(dir, "data"), claude: filepath.Join(dir, "fake-claude"), lock: filepath.Join(dir, "bot.lock")}
	if err := build(h.conchd, "./cmd/conchd"); err != nil {
		return err
	}
	if err := build(h.bot, "./cmd/conch-bot"); err != nil {
		return err
	}
	if err := os.WriteFile(h.claude, []byte("#!/bin/sh\nprintf 'canned bot reply\\n'\n"), 0o700); err != nil { //nolint:gosec // fake Claude must be executable by this test
		return err
	}
	if err := os.MkdirAll(h.data, 0o700); err != nil {
		return err
	}
	if h.addr, err = freeAddr(); err != nil {
		return err
	}
	if err := h.startServer(""); err != nil {
		return err
	}
	channelID, err := createChannel(h.url(), "ops")
	if err != nil {
		return err
	}
	if h.botID, err = createPrincipal(h.url(), schema.PrincipalAgent, "reply-bot"); err != nil {
		return err
	}
	// Agents are deny-by-default (issue #79): the bot needs membership of the
	// channel and a manifest that lets it read and post there.
	if err := putJSON(fmt.Sprintf("%s/v1/channels/ops/members/%d", h.url(), h.botID), nil); err != nil {
		return err
	}
	manifest := schema.PutAgentManifestRequestV1{
		DisplayName:  "reply-bot",
		Tier:         schema.AgentTierC,
		Capabilities: []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost},
		Channels: []schema.ChannelGrant{{
			ChannelID:   channelID,
			Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet},
		}},
	}
	if err := putJSON(fmt.Sprintf("%s/v1/principals/%d/manifest", h.url(), h.botID), manifest); err != nil {
		return err
	}
	if err := h.setUpNet(channelID); err != nil {
		return err
	}
	if h.humanID, err = createPrincipal(h.url(), schema.PrincipalHuman, "human"); err != nil {
		return err
	}
	h.stopServer()
	if err := h.startServer(strings.Join([]string{
		token + "=" + strconv.FormatInt(h.botID, 10),
		peerToken + "=" + strconv.FormatInt(h.peerID, 10),
		outsiderToken + "=" + strconv.FormatInt(h.outsiderID, 10),
	}, ",")); err != nil {
		return err
	}
	defer h.stopServer()

	// Initial backlog is seeded past without a reply.
	if err := postHuman(h.url(), h.humanID, "initial backlog"); err != nil {
		return err
	}
	if err := h.startBot(); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	if err := postHuman(h.url(), h.humanID, "first live message"); err != nil {
		return err
	}
	if err := waitForAgentCount(h.url(), h.botID, 1, 5*time.Second); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	if err := assertAgentCount(h.url(), h.botID, 1); err != nil {
		return fmt.Errorf("self-filter: %w", err)
	}
	h.stopBot()

	// This stopped-period message must become restart backlog.
	if err := postHuman(h.url(), h.humanID, "restart backlog"); err != nil {
		return err
	}
	if err := h.startBot(); err != nil {
		return err
	}
	defer h.stopBot()
	time.Sleep(300 * time.Millisecond)
	if err := postHuman(h.url(), h.humanID, "post-restart live message"); err != nil {
		return err
	}
	if err := waitForAgentCount(h.url(), h.botID, 2, 5*time.Second); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	if err := assertAgentCount(h.url(), h.botID, 2); err != nil {
		return err
	}
	if err := h.netExchange(); err != nil {
		return err
	}
	return h.refusedNetExchange()
}

// setUpNet creates the principals and the net for the scoped exchange: a net
// holding the bot and a peer agent, and an outsider agent that is in the
// channel but not on the net. Agents are deny-by-default, so each gets a
// manifest.
func (h *harness) setUpNet(channelID int64) error {
	var err error
	if h.peerID, err = createPrincipal(h.url(), schema.PrincipalAgent, "net-peer"); err != nil {
		return err
	}
	if h.outsiderID, err = createPrincipal(h.url(), schema.PrincipalAgent, "outsider"); err != nil {
		return err
	}
	for _, id := range []int64{h.peerID, h.outsiderID} {
		if err := putJSON(fmt.Sprintf("%s/v1/channels/ops/members/%d", h.url(), id), nil); err != nil {
			return err
		}
		manifest := schema.PutAgentManifestRequestV1{
			DisplayName:  "e2e-agent",
			Tier:         schema.AgentTierC,
			Capabilities: []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost},
			Channels: []schema.ChannelGrant{{
				ChannelID:   channelID,
				Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet},
			}},
		}
		if err := putJSON(fmt.Sprintf("%s/v1/principals/%d/manifest", h.url(), id), manifest); err != nil {
			return err
		}
	}
	var created schema.CreateNetResponseV1
	if err := postJSON(h.url()+"/v1/channels/ops/nets", schema.CreateNetRequestV1{Name: "ops-net"}, &created); err != nil {
		return err
	}
	h.netID = created.Net.ID
	for _, id := range []int64{h.botID, h.peerID} {
		if err := putJSON(fmt.Sprintf("%s/v1/channels/ops/nets/ops-net/members/%d", h.url(), id), schema.PutNetMemberRequestV1{Role: schema.NetRoleMember}); err != nil {
			return err
		}
	}
	// A second net the bot only monitors: it reads what is said there and the
	// server refuses its replies.
	if err := postJSON(h.url()+"/v1/channels/ops/nets", schema.CreateNetRequestV1{Name: "watch-net"}, &created); err != nil {
		return err
	}
	h.watchNetID = created.Net.ID
	for id, role := range map[int64]schema.NetRole{h.botID: schema.NetRoleMonitor, h.peerID: schema.NetRoleMember} {
		if err := putJSON(fmt.Sprintf("%s/v1/channels/ops/nets/watch-net/members/%d", h.url(), id), schema.PutNetMemberRequestV1{Role: role}); err != nil {
			return err
		}
	}
	return nil
}

// refusedNetExchange is the case the feature exists for. The peer speaks on a
// net the bot only monitors. The bot reads the message and its reply is
// refused by the real server; the bot must then post nothing anywhere, in
// particular not in the open channel, and must carry on: the next channel-wide
// message still gets its answer.
func (h *harness) refusedNetExchange() error {
	ctx := context.Background()
	peer := mcpclient.New(h.url(), peerToken)
	if err := peer.Initialize(ctx, "conch-bot-check-peer-2"); err != nil {
		return fmt.Errorf("initialize peer: %w", err)
	}
	const prompt = "WATCH-NET-PROMPT-the-bot-may-not-answer"
	posted, err := peer.PostMessageTo(ctx, "ops", prompt, &schema.Audience{Kind: schema.AudienceKindNet, NetID: h.watchNetID})
	if err != nil {
		return fmt.Errorf("peer post on the monitored net: %w", err)
	}
	// The bot polls every 50 ms; give it many polls to answer wrongly.
	time.Sleep(time.Second)
	page, err := peer.ReadChannel(ctx, "ops", posted.Message.ID, 100)
	if err != nil {
		return fmt.Errorf("peer read: %w", err)
	}
	for _, m := range page.Messages {
		if m.AuthorID == h.botID {
			return fmt.Errorf("the bot posted after a refused reply: %+v (audience %+v)", m, m.Audience)
		}
	}
	if err := assertChannelWideBotReplies(h, 2); err != nil {
		return fmt.Errorf("a refused net reply was sent to the open channel: %w", err)
	}
	// Not stuck in a retry or backoff: a channel-wide message is answered.
	if err := postHuman(h.url(), h.humanID, "after the refusal"); err != nil {
		return err
	}
	if err := waitForAgentCount(h.url(), h.botID, 3, 5*time.Second); err != nil {
		return fmt.Errorf("the bot stopped answering after a refused reply: %w", err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := assertChannelWideBotReplies(h, 3); err != nil {
		return err
	}
	raw, err := httpGet(h.url() + "/v2/channels/ops/messages")
	if err != nil {
		return err
	}
	if strings.Contains(raw, "WATCH-NET-PROMPT") {
		return fmt.Errorf("the monitored net's prompt is visible in the open channel: %s", raw)
	}
	return nil
}

// netExchange has the peer post a prompt on the net over MCP (with auth off a
// human cannot post a scoped message over REST). The bot must answer on the
// same net. The outsider, an agent in the channel reading over MCP, must see
// neither message: that is the check that matters. REST is also read, without
// a credential (this server runs with authentication off), where no scoped
// message may ever appear.
func (h *harness) netExchange() error {
	ctx := context.Background()
	netAudience := &schema.Audience{Kind: schema.AudienceKindNet, NetID: h.netID}
	peer := mcpclient.New(h.url(), peerToken)
	outsider := mcpclient.New(h.url(), outsiderToken)
	for name, client := range map[string]*mcpclient.Client{"peer": peer, "outsider": outsider} {
		if err := client.Initialize(ctx, "conch-bot-check-"+name); err != nil {
			return fmt.Errorf("initialize %s: %w", name, err)
		}
	}
	const prompt = "NET-PROMPT-for-the-bot"
	if _, err := peer.PostMessageTo(ctx, "ops", prompt, netAudience); err != nil {
		return fmt.Errorf("peer net post: %w", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var reply *schema.MessageV2
	for reply == nil && time.Now().Before(deadline) {
		page, err := peer.ReadChannel(ctx, "ops", 0, 100)
		if err != nil {
			return fmt.Errorf("peer read: %w", err)
		}
		for i, m := range page.Messages {
			if m.AuthorID == h.botID && m.Audience != nil {
				reply = &page.Messages[i]
			}
		}
		if reply == nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if reply == nil {
		return fmt.Errorf("the bot did not answer the net message")
	}
	if reply.Audience.Kind != schema.AudienceKindNet || reply.Audience.NetID != h.netID || reply.Body != "canned bot reply" {
		return fmt.Errorf("net reply = %+v (audience %+v), want the canned reply on net %d", reply, reply.Audience, h.netID)
	}
	// Give a wrongly channel-wide reply time to show up before looking.
	time.Sleep(300 * time.Millisecond)
	if err := assertChannelWideBotReplies(h, 2); err != nil {
		return fmt.Errorf("the net reply leaked into the open channel: %w", err)
	}

	page, err := outsider.ReadChannel(ctx, "ops", 0, 100)
	if err != nil {
		return fmt.Errorf("outsider read: %w", err)
	}
	if len(page.Messages) == 0 {
		return fmt.Errorf("outsider sees no messages at all; the check would prove nothing")
	}
	for _, m := range page.Messages {
		if m.Audience != nil || strings.Contains(m.Body, "NET-PROMPT") || m.ID == reply.ID {
			return fmt.Errorf("outsider sees a scoped message over MCP: %+v", m)
		}
	}
	for _, version := range []string{"v1", "v2"} {
		raw, err := httpGet(fmt.Sprintf("%s/%s/channels/ops/messages", h.url(), version))
		if err != nil {
			return err
		}
		if strings.Contains(raw, "NET-PROMPT") || strings.Contains(raw, fmt.Sprintf(`"id":%d,`, reply.ID)) {
			return fmt.Errorf("anonymous REST %s shows the net exchange: %s", version, raw)
		}
	}
	return nil
}

// assertChannelWideBotReplies checks the bot has exactly want channel-wide
// replies in the open channel.
func assertChannelWideBotReplies(h *harness, want int) error {
	return assertAgentCount(h.url(), h.botID, want)
}

func httpGet(url string) (string, error) {
	response, err := http.Get(url) // #nosec G107 -- test-local server
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	return string(data), err
}

func build(out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg) // #nosec G204 -- fixed repository packages and test temp paths
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("build %s: %w: %s", pkg, err, output)
	}
	return nil
}

func freeAddr() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().String(), nil
}

func (h *harness) url() string { return "http://" + h.addr }

// startServer runs conchd with authentication off. This check is about the
// bot's reply loop, which talks to /mcp (always authenticated with its own
// token); the harness's own REST calls play the part of an unauthenticated
// local development setup.
func (h *harness) startServer(mapping string) error {
	args := []string{"serve", "--data", h.data, "--listen", h.addr, "--auth", "off"}
	if mapping != "" {
		args = append(args, "--mcp-token", mapping)
	}
	h.server = exec.Command(h.conchd, args...) // #nosec G204 -- test-built binary and controlled arguments
	h.server.Stdout, h.server.Stderr = os.Stdout, os.Stderr
	if err := h.server.Start(); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(h.url() + "/healthz") // #nosec G107 -- test-local server
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("conchd did not become healthy")
}

func (h *harness) stopServer() { stop(&h.server) }
func (h *harness) stopBot() {
	if h.botProc == nil || h.botProc.Process == nil {
		return
	}
	_ = h.botProc.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = h.botProc.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = h.botProc.Process.Kill()
		<-done
	}
	h.botProc = nil
}

func stop(cmd **exec.Cmd) {
	if *cmd != nil && (*cmd).Process != nil {
		_ = (*cmd).Process.Kill()
		_ = (*cmd).Wait()
	}
	*cmd = nil
}

func (h *harness) startBot() error {
	h.botProc = exec.Command(h.bot) // #nosec G204 -- test-built binary
	h.botProc.Env = append(os.Environ(),
		"CONCH_BOT_SERVER="+h.url(), "CONCH_BOT_TOKEN="+token,
		"CONCH_BOT_PRINCIPAL_ID="+strconv.FormatInt(h.botID, 10), "CONCH_BOT_CHANNEL=ops",
		"CONCH_BOT_POLL_INTERVAL=50ms", "CONCH_BOT_MAX_BACKOFF=200ms",
		"CONCH_BOT_REPLY_TIMEOUT=2s", "CLAUDE_BIN="+h.claude, "CONCH_BOT_LOCK_FILE="+h.lock,
	)
	h.botProc.Stdout, h.botProc.Stderr = os.Stdout, os.Stderr
	return h.botProc.Start()
}

func createChannel(baseURL, name string) (int64, error) {
	var response schema.CreateChannelResponse
	err := postJSON(baseURL+"/v0/channels", schema.CreateChannelRequest{Name: name}, &response)
	return response.Channel.ID, err
}

func createPrincipal(baseURL string, kind schema.PrincipalKind, name string) (int64, error) {
	var response schema.CreatePrincipalResponse
	err := postJSON(baseURL+"/v0/principals", schema.CreatePrincipalRequest{Kind: kind, Name: name}, &response)
	return response.Principal.ID, err
}

func postHuman(baseURL string, authorID int64, body string) error {
	return postJSON(baseURL+"/v1/channels/ops/messages", schema.PostMessageRequestV1{AuthorID: authorID, Body: body}, nil)
}

// putJSON sends a PUT with a JSON body (or none when body is nil) and expects
// a 2xx.
func putJSON(url string, body any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(http.MethodPut, url, reader) // #nosec G107 -- test-local server
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("PUT %s status %d: %s", url, resp.StatusCode, respBody)
	}
	return nil
}

func postJSON(url string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	response, err := http.Post(url, "application/json", bytes.NewReader(encoded)) // #nosec G107 -- test-local server
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(response.Body)
		return fmt.Errorf("POST status %d: %s", response.StatusCode, data)
	}
	if out != nil {
		return json.NewDecoder(response.Body).Decode(out)
	}
	return nil
}

func messages(baseURL string) (schema.ListMessagesResponseV1, error) {
	var result schema.ListMessagesResponseV1
	response, err := http.Get(baseURL + "/v1/channels/ops/messages") // #nosec G107 -- test-local server
	if err != nil {
		return result, err
	}
	defer func() { _ = response.Body.Close() }()
	return result, json.NewDecoder(response.Body).Decode(&result)
}

func waitForAgentCount(baseURL string, agentID int64, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := assertAgentCount(baseURL, agentID, want); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return assertAgentCount(baseURL, agentID, want)
}

func assertAgentCount(baseURL string, agentID int64, want int) error {
	result, err := messages(baseURL)
	if err != nil {
		return err
	}
	count := 0
	for _, message := range result.Messages {
		if message.AuthorID == agentID {
			count++
			if message.Body != "canned bot reply" {
				return fmt.Errorf("reply body = %q", message.Body)
			}
		}
	}
	if count != want {
		return fmt.Errorf("agent replies = %d, want %d", count, want)
	}
	return nil
}
