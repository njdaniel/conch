// Command dogfood-check is the P1 release canary (ROADMAP success
// criterion, docs/adr/ADR-000-charter.md, .claude/skills/dogfood-check/):
// an agent connects via MCP, posts a typed message, requests approval; ntfy
// fires; a human resolves via conch approve with a reason; await_decision
// returns the structured outcome; the audit log shows the full chain. This
// program drives that loop against real conchd/conch binaries and asserts
// every step, then reruns the approval half with ntfy unreachable to prove
// graceful degradation. Nonzero exit on any assertion failure.
//
// The happy path authenticates the agent with a credential issued through
// the REST API (issues #78, #97) and ends by revoking it and asserting the
// next MCP call is refused. The degraded path keeps using the deprecated
// static --mcp-token mapping, so both mechanisms stay covered end to end.
//
// The whole run is authenticated (issue #92): conchd starts with its default,
// which requires a credential on every REST and WebSocket request. The
// harness bootstraps an operator offline, and every principal acts with its
// own credential; the human signs in with `conch login`. It also asserts the
// refusals: no credential, a decision in someone else's name, a decision or a
// read by a human who is not a member of the channel.
//
// Agents are deny-by-default (issue #79): the happy path first shows the
// agent can do nothing without a manifest, then gives it membership of the
// channel and a manifest, and also runs a second, narrowly-permitted agent
// to show it cannot reach the first agent's channel or raise approvals.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "dogfood-check: FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("dogfood-check: PASS")
}

func run() error {
	bin, err := buildBinaries()
	if err != nil {
		return fmt.Errorf("build binaries: %w", err)
	}
	defer func() { _ = os.RemoveAll(bin.dir) }()

	fmt.Println("== happy path (ntfy reachable) ==")
	if err := happyPath(bin); err != nil {
		return fmt.Errorf("happy path: %w", err)
	}

	fmt.Println("== degraded path (ntfy unreachable) ==")
	if err := degradedPath(bin); err != nil {
		return fmt.Errorf("degraded path: %w", err)
	}
	return nil
}

type binaries struct {
	dir    string
	conchd string
	conch  string
}

func buildBinaries() (binaries, error) {
	dir, err := os.MkdirTemp("", "dogfood-bin-")
	if err != nil {
		return binaries{}, err
	}
	b := binaries{dir: dir, conchd: filepath.Join(dir, "conchd"), conch: filepath.Join(dir, "conch")}
	if err := goBuild(b.conchd, "./cmd/conchd"); err != nil {
		return binaries{}, err
	}
	if err := goBuild(b.conch, "./cmd/conch"); err != nil {
		return binaries{}, err
	}
	return b, nil
}

func goBuild(out, pkg string) error {
	cmd := exec.Command("go", "build", "-o", out, pkg) // #nosec G204 -- out/pkg are this program's own constants and temp paths, not external input
	cmd.Dir = repoRoot()
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %w\n%s", pkg, err, output)
	}
	return nil
}

// repoRoot assumes this program runs via `go run ./e2e/dogfood` (or a built
// binary invoked) from the module root, matching every other script in this
// repo (scripts/schema-compat.sh, scripts/depgate.sh) and CI's working
// directory. It is not relative to this source file.
func repoRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

// ---------------------------------------------------------------- fake ntfy

// fakeNtfy captures every POST it receives so the test can assert on
// title/priority/topic without depending on a real ntfy server (ADR-002:
// ntfy is optional and this program proves the degraded path too).
type fakeNtfy struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []ntfyHit
}

type ntfyHit struct {
	Topic    string
	Title    string
	Priority string
	Body     string
}

func newFakeNtfy() *fakeNtfy {
	f := &fakeNtfy{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, ntfyHit{
			Topic:    strings.TrimPrefix(r.URL.Path, "/"),
			Title:    r.Header.Get("Title"),
			Priority: r.Header.Get("Priority"),
			Body:     string(body),
		})
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	return f
}

func (f *fakeNtfy) Close() { f.srv.Close() }

func (f *fakeNtfy) Hits() []ntfyHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ntfyHit, len(f.hits))
	copy(out, f.hits)
	return out
}

// ------------------------------------------------------------- server proc

type conchdProc struct {
	cmd     *exec.Cmd
	baseURL string
	dataDir string
	// operatorToken is the credential bootstrap-operator printed.
	operatorToken string
}

// operator returns a REST client acting as the bootstrapped operator.
func (p *conchdProc) operator() api { return api{baseURL: p.baseURL, token: p.operatorToken} }

// as returns a REST client acting with the given credential ("" for none).
func (p *conchdProc) as(token string) api { return api{baseURL: p.baseURL, token: token} }

// staticMCPToken is the deprecated --mcp-token mapping the degraded path uses.
const staticMCPToken = "dogfood-token"

// staticAgentID is the principal the static token maps to. The operator is
// always principal 1, so the first principal the harness creates is 2; the
// degraded path creates its agent first and checks it got this id.
const staticAgentID = 2

// startConchd bootstraps an operator into a fresh data directory, then starts
// conchd on it with its defaults — authentication required. With staticToken
// it also maps staticMCPToken to principal staticAgentID through the
// deprecated --mcp-token flag; without it the agent must authenticate with an
// issued credential.
func startConchd(bin binaries, ntfyServerURL string, staticToken bool) (*conchdProc, error) {
	dataDir, err := os.MkdirTemp("", "dogfood-data-")
	if err != nil {
		return nil, err
	}
	addr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	bootstrap := exec.Command(bin.conchd, "bootstrap-operator", "--data", dataDir, "--name", "dogfood-operator") // #nosec G204 -- bin.conchd is a binary this program just built into a temp dir; args are local constants
	bootstrap.Stderr = os.Stderr
	tokenOut, err := bootstrap.Output()
	if err != nil {
		return nil, fmt.Errorf("bootstrap-operator: %w", err)
	}
	operatorToken := strings.TrimSpace(string(tokenOut))
	if operatorToken == "" || strings.ContainsAny(operatorToken, " \n") {
		return nil, fmt.Errorf("bootstrap-operator must print the token alone on stdout, got %d bytes", len(tokenOut))
	}
	args := []string{"serve", "--data", dataDir, "--listen", addr}
	if staticToken {
		args = append(args, "--mcp-token", fmt.Sprintf("%s=%d", staticMCPToken, staticAgentID))
	}
	if ntfyServerURL != "" {
		args = append(args, "--ntfy-server", ntfyServerURL, "--ntfy-topic", "approvals", "--ntfy-urgent-topic", "approvals-urgent")
	}
	cmd := exec.Command(bin.conchd, args...) // #nosec G204 -- bin.conchd is a binary this program just built into a temp dir; args are local constants
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &conchdProc{cmd: cmd, baseURL: "http://" + addr, dataDir: dataDir, operatorToken: operatorToken}
	if err := p.waitHealthy(10 * time.Second); err != nil {
		_ = p.Stop()
		return nil, err
	}
	return p, nil
}

func freeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

func (p *conchdProc) waitHealthy(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(p.baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("conchd did not become healthy within %s", timeout)
}

func (p *conchdProc) Stop() error {
	if p.cmd.Process == nil {
		return nil
	}
	_ = p.cmd.Process.Kill()
	_ = p.cmd.Wait()
	return os.RemoveAll(p.dataDir)
}

func (p *conchdProc) auditEvents() ([]store.AuditEvent, error) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(p.dataDir, "conch.db"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()
	return st.ListAuditEvents(ctx, 0, 1000)
}

// ------------------------------------------------------------------ REST

// api is a REST client acting as one principal: every request carries its
// bearer credential (or none, when token is empty).
type api struct{ baseURL, token string }

// do sends one request and returns the status and body. A non-nil body is
// sent as JSON.
func (a api) do(method, path string, body any, headers map[string]string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, a.baseURL+path, reader) // #nosec G107 -- this harness's own conchd instance, not external input
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, respBody, nil
}

// call sends a request that must succeed (2xx) and decodes the response into
// out when out is non-nil.
func (a api) call(method, path string, body, out any) error {
	status, respBody, err := a.do(method, path, body, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("%s %s status %d: %s", method, path, status, respBody)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(respBody, out)
}

// expectStatus sends a request that must be answered with the given status
// and, when code is non-empty, that error code.
func (a api) expectStatus(what, method, path string, body any, headers map[string]string, status int, code string) error {
	got, respBody, err := a.do(method, path, body, headers)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if got != status {
		return fmt.Errorf("%s: status %d, want %d (body %s)", what, got, status, respBody)
	}
	if code != "" {
		var e schema.Error
		if err := json.Unmarshal(respBody, &e); err != nil || e.Code != code {
			return fmt.Errorf("%s: error code %q, want %q (body %s)", what, e.Code, code, respBody)
		}
	}
	return nil
}

func createChannel(a api, name string) (int64, error) {
	var resp schema.CreateChannelResponse
	if err := a.call(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: name}, &resp); err != nil {
		return 0, err
	}
	return resp.Channel.ID, nil
}

func createPrincipal(a api, kind schema.PrincipalKind, name string) (int64, error) {
	var resp schema.CreatePrincipalResponse
	if err := a.call(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: kind, Name: name}, &resp); err != nil {
		return 0, err
	}
	return resp.Principal.ID, nil
}

// issueCredential creates a bearer credential for a principal through the
// REST API and returns its id and the token (shown once).
func issueCredential(a api, principalID int64, label string) (int64, string, error) {
	var resp schema.CreateCredentialResponseV1
	path := fmt.Sprintf("/v1/principals/%d/credentials", principalID)
	if err := a.call(http.MethodPost, path, schema.CreateCredentialRequestV1{Label: label}, &resp); err != nil {
		return 0, "", err
	}
	if resp.Token == "" || resp.Credential.ID == 0 {
		return 0, "", fmt.Errorf("create credential returned no token or id")
	}
	return resp.Credential.ID, resp.Token, nil
}

func revokeCredential(a api, credentialID int64) error {
	return a.call(http.MethodDelete, fmt.Sprintf("/v1/credentials/%d", credentialID), nil, nil)
}

// addMember makes a principal a member of a channel.
func addMember(a api, channel string, principalID int64) error {
	return a.call(http.MethodPut, fmt.Sprintf("/v1/channels/%s/members/%d", channel, principalID), nil, nil)
}

// putManifest writes an agent's manifest: the given capabilities, with read
// and post in each of the given channels.
func putManifest(a api, agentID int64, name string, caps []schema.Capability, channelIDs ...int64) error {
	req := schema.PutAgentManifestRequestV1{DisplayName: name, Tier: schema.AgentTierA, Capabilities: caps}
	for _, id := range channelIDs {
		req.Channels = append(req.Channels, schema.ChannelGrant{
			ChannelID:   id,
			Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost},
		})
	}
	return a.call(http.MethodPut, fmt.Sprintf("/v1/principals/%d/manifest", agentID), req, nil)
}

// provisionAgent gives an agent everything it needs to work in one channel:
// membership, and a manifest with every capability there.
func provisionAgent(a api, agentID int64, name, channel string, channelID int64) error {
	if err := addMember(a, channel, agentID); err != nil {
		return fmt.Errorf("add agent to %s: %w", channel, err)
	}
	if err := putManifest(a, agentID, name, schema.Capabilities(), channelID); err != nil {
		return fmt.Errorf("write agent manifest: %w", err)
	}
	return nil
}

// newHuman creates a human principal with a credential and, when channel is
// non-empty, membership of that channel.
func newHuman(a api, name, channel string) (int64, string, error) {
	id, err := createPrincipal(a, schema.PrincipalHuman, name)
	if err != nil {
		return 0, "", fmt.Errorf("create %s: %w", name, err)
	}
	_, token, err := issueCredential(a, id, name)
	if err != nil {
		return 0, "", fmt.Errorf("issue credential for %s: %w", name, err)
	}
	if channel != "" {
		if err := addMember(a, channel, id); err != nil {
			return 0, "", fmt.Errorf("add %s to %s: %w", name, channel, err)
		}
	}
	return id, token, nil
}

// expectToolError calls an MCP tool and requires it to fail with the given
// error code.
func expectToolError(client *mcpclient.Client, tool string, args map[string]any, code string) error {
	_, err := client.CallTool(context.Background(), tool, args)
	if err == nil {
		return fmt.Errorf("%s succeeded, want %s", tool, code)
	}
	if !strings.Contains(err.Error(), code) {
		return fmt.Errorf("%s failed with %w, want %s", tool, err, code)
	}
	return nil
}

func restListMessages(a api, channel string) (schema.ListMessagesResponseV1, error) {
	var resp schema.ListMessagesResponseV1
	err := a.call(http.MethodGet, "/v1/channels/"+channel+"/messages", nil, &resp)
	return resp, err
}

// ------------------------------------------------------------------ paths

func happyPath(bin binaries) error {
	ntfy := newFakeNtfy()
	defer ntfy.Close()

	proc, err := startConchd(bin, ntfy.srv.URL, false)
	if err != nil {
		return err
	}
	defer func() { _ = proc.Stop() }()

	channelID, err := createChannel(proc.operator(), "ops")
	if err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	agentID, err := createPrincipal(proc.operator(), schema.PrincipalAgent, "dogfood-agent")
	if err != nil {
		return fmt.Errorf("create agent principal: %w", err)
	}
	humanID, humanToken, err := newHuman(proc.operator(), "dogfood-human", "ops")
	if err != nil {
		return err
	}
	// The human signs in with the real CLI: the token goes in on stdin and is
	// stored under an isolated config directory, never the user's own.
	human, err := newConchUser(bin.conch, proc.baseURL)
	if err != nil {
		return err
	}
	defer human.cleanup()
	if out, err := human.run(humanToken+"\n", "login"); err != nil {
		return fmt.Errorf("conch login: %w", err)
	} else if !strings.Contains(out, "dogfood-human") {
		return fmt.Errorf("conch login output = %q, want it to name the principal", out)
	}

	// Step 1: the agent authenticates with a credential issued through the
	// REST API; no static token is configured on this conchd.
	credentialID, agentToken, err := issueCredential(proc.operator(), agentID, "dogfood")
	if err != nil {
		return fmt.Errorf("issue agent credential: %w", err)
	}
	if err := mcpclient.New(proc.baseURL, staticMCPToken).Initialize(context.Background(), "dogfood-check"); err == nil {
		return fmt.Errorf("mcp accepted the static token although none is configured")
	}
	client := mcpclient.New(proc.baseURL, agentToken)
	if err := client.Initialize(context.Background(), "dogfood-check"); err != nil {
		return fmt.Errorf("mcp initialize with issued credential: %w", err)
	}

	// Deny by default: authenticated, but with no manifest the agent can do
	// nothing. Membership and a manifest are what let it work in "ops".
	if err := expectToolError(client, "post_message", map[string]any{"channel": "ops", "body": "too early"}, "forbidden"); err != nil {
		return fmt.Errorf("agent without a manifest: %w", err)
	}
	if err := provisionAgent(proc.operator(), agentID, "dogfood-agent", "ops", channelID); err != nil {
		return err
	}

	// Step 2: post a typed message via MCP; verify via read_channel and REST
	// (parity, ADR-001).
	postRaw, err := client.CallTool(context.Background(), "post_message", map[string]any{
		"channel": "ops",
		"body":    "deploy candidate ready",
		"payload": map[string]any{"schema": "leviathan.deploy.v1", "data": map[string]any{"env": "prod"}},
	})
	if err != nil {
		return fmt.Errorf("post_message: %w", err)
	}
	// The MCP tools speak the v2 envelope (issue #117); a message posted
	// without an audience is channel-wide and carries none.
	posted, err := mcpclient.Decode[schema.PostMessageResponseV2](postRaw)
	if err != nil {
		return err
	}
	if err := posted.Message.Validate(); err != nil {
		return fmt.Errorf("post_message returned an invalid v2 message: %w", err)
	}
	if posted.Message.Audience != nil {
		return fmt.Errorf("a channel-wide post_message returned audience %+v", posted.Message.Audience)
	}
	if posted.Message.AuthorID != agentID {
		return fmt.Errorf("posted message author = %d, want authenticated agent %d", posted.Message.AuthorID, agentID)
	}

	readRaw, err := client.CallTool(context.Background(), "read_channel", map[string]any{"channel": "ops"})
	if err != nil {
		return fmt.Errorf("read_channel: %w", err)
	}
	read, err := mcpclient.Decode[schema.ListMessagesResponseV2](readRaw)
	if err != nil {
		return err
	}
	if len(read.Messages) != 1 || read.Messages[0].ID != posted.Message.ID {
		return fmt.Errorf("read_channel = %+v, want exactly the posted message", read.Messages)
	}
	rest, err := restListMessages(proc.operator(), "ops")
	if err != nil {
		return fmt.Errorf("REST list messages: %w", err)
	}
	if len(rest.Messages) != 1 || rest.Messages[0].ID != posted.Message.ID {
		return fmt.Errorf("REST/MCP parity mismatch: REST=%+v MCP=%+v", rest.Messages, read.Messages)
	}

	// Step 3: request_approval, then await_decision (blocking) and
	// check_decision (polling) against the same approval.
	deadline := time.Now().Add(time.Hour).Format(time.RFC3339)
	reqRaw, err := client.CallTool(context.Background(), "request_approval", map[string]any{
		"channel_id": channelID, "title": "Deploy prod", "body": "Ship release 42",
		"options": []map[string]any{
			{"id": "approve", "kind": "approve", "label": "Approve"},
			{"id": "reject", "kind": "reject", "label": "Reject"},
		},
		"deadline": deadline,
	})
	if err != nil {
		return fmt.Errorf("request_approval: %w", err)
	}
	created, err := mcpclient.Decode[schema.RequestApprovalOutput](reqRaw)
	if err != nil {
		return err
	}
	if created.ID == 0 || created.State != schema.ApprovalStatePending {
		return fmt.Errorf("request_approval = %+v, want a pending approval id", created)
	}

	checkRaw, err := client.CallTool(context.Background(), "check_decision", map[string]any{"approval_id": created.ID})
	if err != nil {
		return fmt.Errorf("check_decision (pending): %w", err)
	}
	pendingCheck, err := mcpclient.Decode[schema.CheckDecisionOutput](checkRaw)
	if err != nil {
		return err
	}
	if pendingCheck.State != schema.ApprovalStatePending || pendingCheck.Resolution != nil {
		return fmt.Errorf("check_decision before resolution = %+v, want pending with no resolution", pendingCheck)
	}

	type awaitResult struct {
		out schema.AwaitDecisionOutput
		err error
	}
	awaitCh := make(chan awaitResult, 1)
	go func() {
		raw, err := client.CallTool(context.Background(), "await_decision", map[string]any{"approval_id": created.ID, "timeout_ms": 5000})
		if err != nil {
			awaitCh <- awaitResult{err: err}
			return
		}
		out, err := mcpclient.Decode[schema.AwaitDecisionOutput](raw)
		awaitCh <- awaitResult{out: out, err: err}
	}()
	// Give await_decision time to actually start blocking before resolving,
	// so this genuinely exercises the unblock path rather than a race where
	// the approval resolves before await_decision's first poll.
	time.Sleep(300 * time.Millisecond)

	// Step 5: resolve as a human via the real conch CLI: list, then approve.
	// Step 4b: before anyone decides, everything that must be refused is.
	if err := authNegatives(proc, created.ID, humanID); err != nil {
		return fmt.Errorf("authentication and membership refusals: %w", err)
	}

	listOut, err := human.run("", "approvals", "list")
	if err != nil {
		return fmt.Errorf("conch approvals list: %w", err)
	}
	if !strings.Contains(listOut, "Deploy prod") || !strings.Contains(listOut, strconv.FormatInt(created.ID, 10)) {
		return fmt.Errorf("conch approvals list output missing the approval: %q", listOut)
	}
	// No --author: the decider is whoever is logged in.
	if _, err := human.run("", "approve", "--reason", "dogfood", strconv.FormatInt(created.ID, 10)); err != nil {
		return fmt.Errorf("conch approve: %w", err)
	}

	// Step 6: await_decision unblocked with the structured resolution;
	// check_decision sees the identical resolution.
	awaited := <-awaitCh
	if awaited.err != nil {
		return fmt.Errorf("await_decision: %w", awaited.err)
	}
	if awaited.out.State != schema.ApprovalStateResolved || awaited.out.Resolution == nil {
		return fmt.Errorf("await_decision result = %+v, want resolved with a resolution", awaited.out)
	}
	if awaited.out.Resolution.OptionID != "approve" || len(awaited.out.Resolution.Decisions) != 1 ||
		awaited.out.Resolution.Decisions[0].PrincipalID != humanID || awaited.out.Resolution.Decisions[0].Reason != "dogfood" {
		return fmt.Errorf("resolution = %+v, want approve by %d with reason %q", awaited.out.Resolution, humanID, "dogfood")
	}
	checkRaw, err = client.CallTool(context.Background(), "check_decision", map[string]any{"approval_id": created.ID})
	if err != nil {
		return fmt.Errorf("check_decision (resolved): %w", err)
	}
	resolvedCheck, err := mcpclient.Decode[schema.CheckDecisionOutput](checkRaw)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(resolvedCheck.Resolution, awaited.out.Resolution) {
		return fmt.Errorf("await/check resolutions differ: await=%+v check=%+v", awaited.out.Resolution, resolvedCheck.Resolution)
	}

	// Step 4: the ntfy notification fired.
	hits := ntfy.Hits()
	if len(hits) < 2 {
		return fmt.Errorf("ntfy fake server saw %d posts, want at least 2 (created + resolved)", len(hits))
	}
	if hits[0].Topic != "approvals" || !strings.Contains(hits[0].Title, "Deploy prod") {
		return fmt.Errorf("first ntfy post = %+v, want the approval-created notification", hits[0])
	}

	// Step 7: audit chain, in order.
	if err := assertAuditChain(proc, created.ID, []string{
		store.AuditApprovalCreated, approvals.AuditNotifySent, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifySent,
	}); err != nil {
		return err
	}

	// Step 7b: a second agent, permitted only to exchange messages in another
	// channel, cannot reach "ops" or raise approvals anywhere.
	if err := isolationCheck(proc, created.ID); err != nil {
		return fmt.Errorf("two-agent isolation: %w", err)
	}

	// Step 8: revoke the agent's credential; the very next MCP call is
	// refused, with no restart.
	if err := revokeCredential(proc.operator(), credentialID); err != nil {
		return fmt.Errorf("revoke agent credential: %w", err)
	}
	if _, err := client.CallTool(context.Background(), "check_decision", map[string]any{"approval_id": created.ID}); err == nil {
		return fmt.Errorf("mcp call succeeded with a revoked credential")
	} else if !strings.Contains(err.Error(), "status 401") {
		return fmt.Errorf("mcp call with a revoked credential: want a 401, got: %w", err)
	}
	if err := assertCredentialAudit(proc, agentID, agentToken); err != nil {
		return err
	}

	fmt.Println("happy path: OK (authenticated by default, conch login, refusals asserted, issued credential, deny by default, message parity, request/await/check, CLI approve, ntfy fired, audit chain in order, two-agent isolation, revoked credential refused)")
	return nil
}

func degradedPath(bin binaries) error {
	// An address nothing listens on: connection refused, fast and
	// deterministic, unlike a routable-but-filtered address that would
	// depend on OS-level timeout behavior.
	unreachable, err := freeAddr()
	if err != nil {
		return err
	}

	proc, err := startConchd(bin, "http://"+unreachable, true)
	if err != nil {
		return err
	}
	defer func() { _ = proc.Stop() }()

	channelID, err := createChannel(proc.operator(), "ops")
	if err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	agentID, err := createPrincipal(proc.operator(), schema.PrincipalAgent, "dogfood-agent")
	if err != nil {
		return fmt.Errorf("create agent principal: %w", err)
	}
	if agentID != staticAgentID {
		return fmt.Errorf("agent principal id = %d, want %d (the static token is mapped to it)", agentID, staticAgentID)
	}
	if err := provisionAgent(proc.operator(), agentID, "dogfood-agent", "ops", channelID); err != nil {
		return err
	}
	_, humanToken, err := newHuman(proc.operator(), "dogfood-human", "ops")
	if err != nil {
		return err
	}
	// This human authenticates the scripting way: CONCH_TOKEN, no login.
	human, err := newConchUser(bin.conch, proc.baseURL)
	if err != nil {
		return err
	}
	defer human.cleanup()
	human.env = append(human.env, "CONCH_TOKEN="+humanToken)

	client := mcpclient.New(proc.baseURL, staticMCPToken)
	if err := client.Initialize(context.Background(), "dogfood-check"); err != nil {
		return fmt.Errorf("mcp initialize with the deprecated static token: %w", err)
	}

	deadline := time.Now().Add(time.Hour).Format(time.RFC3339)
	reqRaw, err := client.CallTool(context.Background(), "request_approval", map[string]any{
		"channel_id": channelID, "title": "Deploy staging", "body": "ntfy is down, must still work",
		"options": []map[string]any{
			{"id": "approve", "kind": "approve", "label": "Approve"},
			{"id": "reject", "kind": "reject", "label": "Reject"},
		},
		"deadline": deadline,
	})
	if err != nil {
		return fmt.Errorf("request_approval with ntfy unreachable: %w", err)
	}
	created, err := mcpclient.Decode[schema.RequestApprovalOutput](reqRaw)
	if err != nil {
		return err
	}

	// The approval must still be resolvable even though every ntfy POST
	// will fail (ADR-002: ntfy is optional, never blocking).
	if _, err := human.run("", "approve", "--reason", "degraded still works", strconv.FormatInt(created.ID, 10)); err != nil {
		return fmt.Errorf("conch approve with ntfy unreachable: %w", err)
	}

	checkRaw, err := client.CallTool(context.Background(), "check_decision", map[string]any{"approval_id": created.ID})
	if err != nil {
		return fmt.Errorf("check_decision: %w", err)
	}
	checked, err := mcpclient.Decode[schema.CheckDecisionOutput](checkRaw)
	if err != nil {
		return err
	}
	if checked.State != schema.ApprovalStateResolved || checked.Resolution == nil {
		return fmt.Errorf("degraded-path resolution = %+v, want resolved despite ntfy being unreachable", checked)
	}

	if err := assertAuditChain(proc, created.ID, []string{
		store.AuditApprovalCreated, approvals.AuditNotifyFailed, store.AuditDecisionCast, store.AuditApprovalResolved, approvals.AuditNotifyFailed,
	}); err != nil {
		return err
	}

	fmt.Println("degraded path: OK (approval resolved with ntfy unreachable, notify_failed audited)")
	return nil
}

func assertAuditChain(proc *conchdProc, approvalID int64, want []string) error {
	events, err := proc.auditEvents()
	if err != nil {
		return fmt.Errorf("read audit events: %w", err)
	}
	subject := fmt.Sprintf("approval:%d", approvalID)
	var got []string
	for _, e := range events {
		if e.Subject == subject {
			got = append(got, e.Action)
		}
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("audit chain for %s = %v, want %v", subject, got, want)
	}
	return nil
}

// isolationCheck runs a second agent with a deliberately narrow grant —
// messages plus approvals.check, in its own channel "lab" — and asserts what
// it cannot do: see "ops" or the approval raised there while not a member,
// post there or observe that approval once it is a member but its manifest
// still says no, or raise an approval even where it may post. Every refusal
// must be audited.
func isolationCheck(proc *conchdProc, opsApprovalID int64) error {
	labID, err := createChannel(proc.operator(), "lab")
	if err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	observerID, err := createPrincipal(proc.operator(), schema.PrincipalAgent, "dogfood-observer")
	if err != nil {
		return fmt.Errorf("create observer principal: %w", err)
	}
	if err := addMember(proc.operator(), "lab", observerID); err != nil {
		return err
	}
	narrow := []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost, schema.CapabilityApprovalsCheck}
	if err := putManifest(proc.operator(), observerID, "dogfood-observer", narrow, labID); err != nil {
		return err
	}
	_, token, err := issueCredential(proc.operator(), observerID, "dogfood-observer")
	if err != nil {
		return fmt.Errorf("issue observer credential: %w", err)
	}
	observer := mcpclient.New(proc.baseURL, token)
	if err := observer.Initialize(context.Background(), "dogfood-observer"); err != nil {
		return fmt.Errorf("observer initialize: %w", err)
	}

	// What it is allowed to do works.
	if _, err := observer.CallTool(context.Background(), "post_message", map[string]any{"channel": "lab", "body": "observer online"}); err != nil {
		return fmt.Errorf("observer post in its own channel: %w", err)
	}
	approvalArgs := func(channelID int64) map[string]any {
		return map[string]any{
			"channel_id": channelID, "title": "should never exist", "body": "raised by an agent without the capability",
			"options": []map[string]any{
				{"id": "approve", "kind": "approve", "label": "Approve"},
				{"id": "reject", "kind": "reject", "label": "Reject"},
			},
			"deadline": time.Now().Add(time.Hour).Format(time.RFC3339),
		}
	}
	refusals := []struct {
		what string
		tool string
		args map[string]any
		code string
	}{
		{"read a channel it is not in", "read_channel", map[string]any{"channel": "ops"}, "channel_not_found"},
		{"post to a channel it is not in", "post_message", map[string]any{"channel": "ops", "body": "intrusion"}, "channel_not_found"},
		{"raise an approval without the capability", "request_approval", approvalArgs(labID), "forbidden"},
		// It holds approvals.check, so this is the membership rule alone: an
		// approval in a channel it is not in looks like it does not exist.
		{"observe an approval in a channel it is not in", "check_decision", map[string]any{"approval_id": opsApprovalID}, "approval_not_found"},
	}
	for _, r := range refusals {
		if err := expectToolError(observer, r.tool, r.args, r.code); err != nil {
			return fmt.Errorf("observer must not %s: %w", r.what, err)
		}
	}
	// Membership alone is not enough: once in "ops", its manifest still has
	// no grant there.
	if err := addMember(proc.operator(), "ops", observerID); err != nil {
		return err
	}
	memberRefusals := []struct {
		what string
		tool string
		args map[string]any
	}{
		{"post", "post_message", map[string]any{"channel": "ops", "body": "intrusion"}},
		{"observe an approval", "check_decision", map[string]any{"approval_id": opsApprovalID}},
	}
	for _, r := range memberRefusals {
		if err := expectToolError(observer, r.tool, r.args, "forbidden"); err != nil {
			return fmt.Errorf("observer must not %s where its manifest has no grant: %w", r.what, err)
		}
	}

	list, err := restListMessages(proc.operator(), "ops")
	if err != nil {
		return err
	}
	for _, m := range list.Messages {
		if m.AuthorID == observerID {
			return fmt.Errorf("the observer posted message %d into ops", m.ID)
		}
	}
	events, err := proc.auditEvents()
	if err != nil {
		return fmt.Errorf("read audit events: %w", err)
	}
	actor := fmt.Sprintf("principal:%d", observerID)
	denials := 0
	for _, e := range events {
		if strings.Contains(e.Actor+e.Action+e.Subject+e.Detail, token) {
			return fmt.Errorf("audit event %q contains the observer's token", e.Action)
		}
		if e.Actor == actor && e.Action == "access_denied" {
			denials++
		}
	}
	if want := len(refusals) + len(memberRefusals); denials != want {
		return fmt.Errorf("access_denied audit events for the observer = %d, want %d", denials, want)
	}
	return nil
}

// assertCredentialAudit checks the agent's credential was audited as created
// then revoked, and that the token itself appears nowhere in the audit log.
func assertCredentialAudit(proc *conchdProc, agentID int64, token string) error {
	events, err := proc.auditEvents()
	if err != nil {
		return fmt.Errorf("read audit events: %w", err)
	}
	subject := fmt.Sprintf("principal:%d", agentID)
	var got []string
	for _, e := range events {
		if strings.Contains(e.Actor+e.Action+e.Subject+e.Detail, token) {
			return fmt.Errorf("audit event %q contains the agent's token", e.Action)
		}
		if e.Subject == subject && strings.HasPrefix(e.Action, "credential_") {
			got = append(got, e.Action)
		}
	}
	if want := []string{"credential_created", "credential_revoked"}; !reflect.DeepEqual(got, want) {
		return fmt.Errorf("credential audit for %s = %v, want %v", subject, got, want)
	}
	return nil
}

// conchUser runs the conch CLI as one person: its own config directory (so
// the real user's is never touched) and no inherited identity.
type conchUser struct {
	bin       string
	configDir string
	env       []string
}

func newConchUser(binPath, serverURL string) (*conchUser, error) {
	dir, err := os.MkdirTemp("", "dogfood-conch-config-")
	if err != nil {
		return nil, err
	}
	var env []string
	for _, kv := range os.Environ() {
		// Nothing about who the caller is may leak in from the environment.
		if strings.HasPrefix(kv, "CONCH_") || strings.HasPrefix(kv, "XDG_CONFIG_HOME=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "CONCH_SERVER="+serverURL, "XDG_CONFIG_HOME="+dir)
	return &conchUser{bin: binPath, configDir: dir, env: env}, nil
}

func (u *conchUser) cleanup() { _ = os.RemoveAll(u.configDir) }

// run executes one conch subcommand with stdin as its standard input.
func (u *conchUser) run(stdin string, args ...string) (string, error) {
	cmd := exec.Command(u.bin, args...) // #nosec G204 -- u.bin is this program's own just-built conch binary; args are local constants
	cmd.Env = u.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w (output: %s)", err, out)
	}
	return string(out), nil
}

// authNegatives asserts what authentication and membership must refuse while
// an approval in "ops" is still pending: a request with no credential, a
// decision cast in someone else's name, and — for a human who is not a member
// of the channel — deciding, reading, posting and subscribing, each answered
// exactly as if the approval or channel did not exist. It also shows two
// humans with separate credentials using the same instance. Nothing here may
// resolve the approval.
func authNegatives(proc *conchdProc, approvalID, deciderID int64) error {
	op := proc.operator()
	_, colleagueToken, err := newHuman(op, "dogfood-colleague", "ops")
	if err != nil {
		return err
	}
	_, outsiderToken, err := newHuman(op, "dogfood-outsider", "")
	if err != nil {
		return err
	}
	anonymous, colleague, outsider := proc.as(""), proc.as(colleagueToken), proc.as(outsiderToken)
	decisions := fmt.Sprintf("/v1/approvals/%d/decisions", approvalID)
	own := map[string]any{"option_id": "approve", "reason": "should be refused"}
	forged := map[string]any{"principal_id": deciderID, "option_id": "approve", "reason": "should be refused"}
	upgrade := map[string]string{"Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}

	checks := []error{
		anonymous.expectStatus("request with no credential", http.MethodGet, "/v1/channels", nil, nil, http.StatusUnauthorized, "unauthenticated"),
		anonymous.expectStatus("decision with no credential", http.MethodPost, decisions, forged, nil, http.StatusUnauthorized, "unauthenticated"),
		colleague.expectStatus("decision in another principal's name", http.MethodPost, decisions, forged, nil, http.StatusForbidden, "principal_mismatch"),
		outsider.expectStatus("decision by a non-member", http.MethodPost, decisions, own, nil, http.StatusNotFound, "approval_not_found"),
		outsider.expectStatus("read by a non-member", http.MethodGet, "/v1/channels/ops/messages", nil, nil, http.StatusNotFound, "channel_not_found"),
		outsider.expectStatus("post by a non-member", http.MethodPost, "/v1/channels/ops/messages", map[string]any{"body": "intrusion"}, nil, http.StatusNotFound, "channel_not_found"),
		outsider.expectStatus("WebSocket subscribe by a non-member", http.MethodGet, "/v1/ws?channel=ops", nil, upgrade, http.StatusNotFound, "channel_not_found"),
	}
	for _, err := range checks {
		if err != nil {
			return err
		}
	}

	// Two humans, two credentials, one instance: the colleague sees the
	// pending approval in the channel they share; the outsider sees none.
	var mine, theirs schema.ListApprovalsResponseV1
	if err := colleague.call(http.MethodGet, "/v1/approvals", nil, &mine); err != nil {
		return err
	}
	if len(mine.Approvals) != 1 || mine.Approvals[0].ID != approvalID || mine.Approvals[0].State != schema.ApprovalStatePending {
		return fmt.Errorf("the colleague's approval list = %+v, want the one pending approval", mine.Approvals)
	}
	if err := outsider.call(http.MethodGet, "/v1/approvals", nil, &theirs); err != nil {
		return err
	}
	if len(theirs.Approvals) != 0 {
		return fmt.Errorf("a non-member's approval list has %d entries, want none", len(theirs.Approvals))
	}
	return nil
}
