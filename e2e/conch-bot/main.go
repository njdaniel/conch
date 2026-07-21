// Command conch-bot-check exercises conch-bot against real conchd and fake
// claude processes. It requires loopback sockets and is intended for CI or a
// developer machine, not restricted sandboxes.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

const token = "conch-bot-e2e-token" // #nosec G101 -- test-only local bearer token

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
	botID, humanID                             int64
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
	if _, err := createChannel(h.url(), "ops"); err != nil {
		return err
	}
	if h.botID, err = createPrincipal(h.url(), schema.PrincipalAgent, "reply-bot"); err != nil {
		return err
	}
	if h.humanID, err = createPrincipal(h.url(), schema.PrincipalHuman, "human"); err != nil {
		return err
	}
	h.stopServer()
	if err := h.startServer(token + "=" + strconv.FormatInt(h.botID, 10)); err != nil {
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
	return assertAgentCount(h.url(), h.botID, 2)
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

func (h *harness) startServer(mapping string) error {
	args := []string{"serve", "--data", h.data, "--listen", h.addr}
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
