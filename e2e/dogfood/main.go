// Command dogfood-check is the P1 release canary (ROADMAP success
// criterion, docs/adr/ADR-000-charter.md, .claude/skills/dogfood-check/):
// an agent connects via MCP, posts a typed message, requests approval; ntfy
// fires; a human resolves via conch approve with a reason; await_decision
// returns the structured outcome; the audit log shows the full chain. This
// program drives that loop against real conchd/conch binaries and asserts
// every step, then reruns the approval half with ntfy unreachable to prove
// graceful degradation. Nonzero exit on any assertion failure.
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
}

const mcpToken = "dogfood-token"

func startConchd(bin binaries, ntfyServerURL string) (*conchdProc, error) {
	dataDir, err := os.MkdirTemp("", "dogfood-data-")
	if err != nil {
		return nil, err
	}
	addr, err := freeAddr()
	if err != nil {
		return nil, err
	}
	args := []string{"serve", "--data", dataDir, "--listen", addr, "--mcp-token", mcpToken + "=1"}
	if ntfyServerURL != "" {
		args = append(args, "--ntfy-server", ntfyServerURL, "--ntfy-topic", "approvals", "--ntfy-urgent-topic", "approvals-urgent")
	}
	cmd := exec.Command(bin.conchd, args...) // #nosec G204 -- bin.conchd is a binary this program just built into a temp dir; args are local constants
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &conchdProc{cmd: cmd, baseURL: "http://" + addr, dataDir: dataDir}
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

func createChannel(baseURL, name string) (int64, error) {
	var resp schema.CreateChannelResponse
	if err := postJSON(baseURL+"/v0/channels", schema.CreateChannelRequest{Name: name}, &resp); err != nil {
		return 0, err
	}
	return resp.Channel.ID, nil
}

func createPrincipal(baseURL string, kind schema.PrincipalKind, name string) (int64, error) {
	var resp schema.CreatePrincipalResponse
	if err := postJSON(baseURL+"/v0/principals", schema.CreatePrincipalRequest{Kind: kind, Name: name}, &resp); err != nil {
		return 0, err
	}
	return resp.Principal.ID, nil
}

func restListMessages(baseURL, channel string) (schema.ListMessagesResponseV1, error) {
	var resp schema.ListMessagesResponseV1
	r, err := http.Get(baseURL + "/v1/channels/" + channel + "/messages")
	if err != nil {
		return resp, err
	}
	defer func() { _ = r.Body.Close() }()
	if r.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(r.Body)
		return resp, fmt.Errorf("GET messages status %d: %s", r.StatusCode, body)
	}
	return resp, json.NewDecoder(r.Body).Decode(&resp)
}

func postJSON(url string, body, out any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(encoded)) // #nosec G107 -- url is this test harness's own conchd instance, not external input
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s status %d: %s", url, resp.StatusCode, respBody)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ------------------------------------------------------------------ paths

func happyPath(bin binaries) error {
	ntfy := newFakeNtfy()
	defer ntfy.Close()

	proc, err := startConchd(bin, ntfy.srv.URL)
	if err != nil {
		return err
	}
	defer func() { _ = proc.Stop() }()

	channelID, err := createChannel(proc.baseURL, "ops")
	if err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	agentID, err := createPrincipal(proc.baseURL, schema.PrincipalAgent, "dogfood-agent")
	if err != nil {
		return fmt.Errorf("create agent principal: %w", err)
	}
	humanID, err := createPrincipal(proc.baseURL, schema.PrincipalHuman, "dogfood-human")
	if err != nil {
		return fmt.Errorf("create human principal: %w", err)
	}

	client := mcpclient.New(proc.baseURL, mcpToken)
	if err := client.Initialize(context.Background(), "dogfood-check"); err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
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
	posted, err := mcpclient.Decode[schema.PostMessageResponseV1](postRaw)
	if err != nil {
		return err
	}
	if posted.Message.AuthorID != agentID {
		return fmt.Errorf("posted message author = %d, want authenticated agent %d", posted.Message.AuthorID, agentID)
	}

	readRaw, err := client.CallTool(context.Background(), "read_channel", map[string]any{"channel": "ops"})
	if err != nil {
		return fmt.Errorf("read_channel: %w", err)
	}
	read, err := mcpclient.Decode[schema.ListMessagesResponseV1](readRaw)
	if err != nil {
		return err
	}
	if len(read.Messages) != 1 || read.Messages[0].ID != posted.Message.ID {
		return fmt.Errorf("read_channel = %+v, want exactly the posted message", read.Messages)
	}
	rest, err := restListMessages(proc.baseURL, "ops")
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
	listOut, err := runConch(bin.conch, proc.baseURL, "approvals", "list")
	if err != nil {
		return fmt.Errorf("conch approvals list: %w", err)
	}
	if !strings.Contains(listOut, "Deploy prod") || !strings.Contains(listOut, strconv.FormatInt(created.ID, 10)) {
		return fmt.Errorf("conch approvals list output missing the approval: %q", listOut)
	}
	if _, err := runConch(bin.conch, proc.baseURL, "approve", "--author", strconv.FormatInt(humanID, 10), "--reason", "dogfood", strconv.FormatInt(created.ID, 10)); err != nil {
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

	fmt.Println("happy path: OK (message parity, request/await/check, CLI approve, ntfy fired, audit chain in order)")
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

	proc, err := startConchd(bin, "http://"+unreachable)
	if err != nil {
		return err
	}
	defer func() { _ = proc.Stop() }()

	channelID, err := createChannel(proc.baseURL, "ops")
	if err != nil {
		return fmt.Errorf("create channel: %w", err)
	}
	if _, err := createPrincipal(proc.baseURL, schema.PrincipalAgent, "dogfood-agent"); err != nil {
		return fmt.Errorf("create agent principal: %w", err)
	}
	humanID, err := createPrincipal(proc.baseURL, schema.PrincipalHuman, "dogfood-human")
	if err != nil {
		return fmt.Errorf("create human principal: %w", err)
	}

	client := mcpclient.New(proc.baseURL, mcpToken)
	if err := client.Initialize(context.Background(), "dogfood-check"); err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
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
	if _, err := runConch(bin.conch, proc.baseURL, "approve", "--author", strconv.FormatInt(humanID, 10), "--reason", "degraded still works", strconv.FormatInt(created.ID, 10)); err != nil {
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

func runConch(binPath, serverURL string, args ...string) (string, error) {
	cmd := exec.Command(binPath, args...) // #nosec G204 -- binPath is this program's own just-built conch binary; args are local constants
	cmd.Env = append(os.Environ(), "CONCH_SERVER="+serverURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%w (output: %s)", err, out)
	}
	return string(out), nil
}
