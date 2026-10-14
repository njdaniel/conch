// Command conch-bot-check exercises conch-adapter against a real conchd. The
// adapter runs the deterministic fixture e2e/p3-local/fixtures/echo.sh, never a
// model. It requires loopback sockets and is intended for CI or a developer
// machine, not restricted sandboxes.
//
// It checks the reply loop (a message gets one reply in its own audience, a
// refused net reply is dropped, the backlog is skipped), and the adapter's
// presence on the server: its session, one completed run per reply with the
// activity of a successful reply, and a clean shutdown that ends the session
// and releases the lock. The old name conch-bot is run once as well, to show
// that it still starts, says so once, and honours the old CONCH_BOT_* names.
//
// The adapter is run a second time as the codex runtime, with CODEX_BIN naming
// testdata/codex-stub.sh in place of Codex: the default Codex command, the
// events of one recorded run read from its output, and the run's activity and
// the reply as the server shows them. Codex itself is never started.
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
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "conch-bot-check: FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("conch-bot-check: PASS")
}

// fixture is the deterministic stand-in for a model: it replies with the last
// line of the prompt, "<author id>: <message body>".
const fixture = "e2e/p3-local/fixtures/echo.sh"

// codexStub replays the events of one recorded `codex exec --json` run.
const codexStub = "e2e/conch-bot/testdata/codex-stub.sh"

type harness struct {
	dir, conchd, adapter, alias, data, lock, addr, fixture string
	server, botProc                                        *exec.Cmd
	botStderr                                              *bytes.Buffer
	opToken, humanToken                                    string
	peerToken, outsiderToken, botToken                     string
	botID, humanID, peerID, outsiderID, netID              int64
	watchNetID                                             int64
	// lastSession is the id of the newest session the adapter has had; a
	// session ended by a clean stop leaves the roster, so a new one is told
	// apart by its id.
	lastSession int64
	// opID is the operator's principal id, who decides the fixture approval.
	opID int64
	// command and fixtureOn are what the next startBot gives the adapter.
	command   string
	fixtureOn bool
	// codex runs the adapter as the codex runtime on its default command, with
	// codexStub as the program, instead of command.
	codex     bool
	codexStub string
	approval  string // path of the fixture that raises an approval
	// readers are the human's REST client and the operator's.
	human, op rest
}

func run() error {
	dir, err := os.MkdirTemp("", "conch-bot-e2e-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	fixturePath, err := filepath.Abs(fixture)
	if err != nil {
		return err
	}
	// The adapter and its old name are two separate binaries; the old name
	// does not depend on the other being there.
	h := &harness{dir: dir, conchd: filepath.Join(dir, "conchd"), adapter: filepath.Join(dir, "conch-adapter"), alias: filepath.Join(dir, "conch-bot"),
		data: filepath.Join(dir, "data"), lock: filepath.Join(dir, "adapter.lock"), fixture: fixturePath, command: fixturePath,
		approval: filepath.Join(filepath.Dir(fixturePath), "approval.sh")}
	for out, pkg := range map[string]string{h.conchd: "./cmd/conchd", h.adapter: "./cmd/conch-adapter", h.alias: "./cmd/conch-bot"} {
		if err := build(out, pkg); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(h.data, 0o700); err != nil {
		return err
	}
	if h.codexStub, err = filepath.Abs(codexStub); err != nil {
		return err
	}
	if h.addr, err = freeAddr(); err != nil {
		return err
	}
	// The server runs with authentication required: sessions belong to an
	// issued credential, which a static MCP token is not.
	if h.opToken, err = h.bootstrapOperator(); err != nil {
		return err
	}
	if err := h.startServer(); err != nil {
		return err
	}
	defer h.stopServer()
	defer h.stopBot()
	if err := h.setUp(); err != nil {
		return err
	}

	// Initial backlog is seeded past without a reply.
	if err := h.postHuman("initial backlog"); err != nil {
		return err
	}
	if err := h.startBot(h.adapter, false); err != nil {
		return err
	}
	// The adapter's session is its first sign of life. It reads the backlog
	// right after registering, so the pause below lets that finish before the
	// next message is posted as a live one.
	if err := h.waitNewSession(); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	first, err := h.postHumanMessage("first live message")
	if err != nil {
		return err
	}
	if err := h.waitForReplies(h.humanReply("first live message")); err != nil {
		return err
	}
	// Waits for the run to be over and the session idle, so what follows is not
	// a race with the adapter finishing its report.
	if err := h.checkSessionAndRun(first); err != nil {
		return err
	}
	if err := h.assertReplies(h.humanReply("first live message")); err != nil {
		return fmt.Errorf("self-filter: %w", err)
	}
	if err := h.stopBotClean(); err != nil {
		return err
	}
	if err := h.checkSessionEnded(); err != nil {
		return err
	}

	// This stopped-period message must become restart backlog.
	if err := h.postHuman("restart backlog"); err != nil {
		return err
	}
	if err := h.startBot(h.adapter, false); err != nil {
		return err
	}
	if err := h.waitNewSession(); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	if err := h.postHuman("post-restart live message"); err != nil {
		return err
	}
	both := []string{h.humanReply("first live message"), h.humanReply("post-restart live message")}
	if err := h.waitForReplies(both...); err != nil {
		return err
	}
	time.Sleep(250 * time.Millisecond)
	if err := h.assertReplies(both...); err != nil {
		return err
	}
	if err := h.netExchange(both); err != nil {
		return err
	}
	if err := h.refusedNetExchange(both); err != nil {
		return err
	}
	if err := h.stopBotClean(); err != nil {
		return err
	}
	if err := h.fixtureApproval(); err != nil {
		return err
	}
	if err := h.codexProfile(); err != nil {
		return err
	}
	return h.aliasStillStarts()
}

// bootstrapOperator creates the first operator offline and returns its token.
func (h *harness) bootstrapOperator() (string, error) {
	cmd := exec.Command(h.conchd, "bootstrap-operator", "--data", h.data, "--name", "e2e-operator") // #nosec G204 -- test-built binary and controlled arguments
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("bootstrap-operator: %w", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" || strings.ContainsAny(token, " \n") {
		return "", fmt.Errorf("bootstrap-operator must print the token alone on stdout")
	}
	return token, nil
}

// setUp creates the channel, the human, the bot, the peer and the outsider
// with credentials, membership and manifests, and the nets.
func (h *harness) setUp() error {
	h.op = rest{base: h.url(), token: h.opToken}
	var me schema.WhoAmIResponseV1
	if err := h.op.send(http.MethodGet, "/v1/whoami", nil, &me); err != nil {
		return err
	}
	h.opID = me.ID
	channelID, err := createChannel(h.op, "ops")
	if err != nil {
		return err
	}
	manifest := func(name string, capabilities ...schema.Capability) schema.PutAgentManifestRequestV1 {
		return schema.PutAgentManifestRequestV1{
			DisplayName:  name,
			Tier:         schema.AgentTierC,
			Capabilities: capabilities,
			Channels: []schema.ChannelGrant{{
				ChannelID:   channelID,
				Permissions: []schema.ChannelPermission{schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionPostNet},
			}},
		}
	}
	messaging := []schema.Capability{schema.CapabilityMessagesRead, schema.CapabilityMessagesPost}
	// approvals.request and approvals.await are for the fixture that raises an
	// approval (approval.sh); echo.sh never uses them.
	adapter := append([]schema.Capability{schema.CapabilitySessionsManage, schema.CapabilityRunsManage, schema.CapabilityApprovalsRequest, schema.CapabilityApprovalsAwait}, messaging...)
	for _, p := range []struct {
		id    *int64
		token *string
		kind  schema.PrincipalKind
		name  string
		caps  []schema.Capability
	}{
		{&h.humanID, &h.humanToken, schema.PrincipalHuman, "human", nil},
		{&h.botID, &h.botToken, schema.PrincipalAgent, "reply-bot", adapter},
		{&h.peerID, &h.peerToken, schema.PrincipalAgent, "net-peer", messaging},
		{&h.outsiderID, &h.outsiderToken, schema.PrincipalAgent, "outsider", messaging},
	} {
		if *p.id, err = createPrincipal(h.op, p.kind, p.name); err != nil {
			return err
		}
		var cred schema.CreateCredentialResponseV1
		if err := h.op.send(http.MethodPost, fmt.Sprintf("/v1/principals/%d/credentials", *p.id), schema.CreateCredentialRequestV1{Label: p.name}, &cred); err != nil {
			return err
		}
		*p.token = cred.Token
		if err := h.op.send(http.MethodPut, fmt.Sprintf("/v1/channels/ops/members/%d", *p.id), nil, nil); err != nil {
			return err
		}
		if p.kind == schema.PrincipalAgent {
			// Agents are deny-by-default (issue #79): a manifest lets each one
			// read and post in the channel.
			if err := h.op.send(http.MethodPut, fmt.Sprintf("/v1/principals/%d/manifest", *p.id), manifest(p.name, p.caps...), nil); err != nil {
				return err
			}
		}
	}
	h.human = rest{base: h.url(), token: h.humanToken}
	return h.setUpNets()
}

// setUpNets creates a net holding the bot and the peer, and a net the bot only
// monitors: it reads what is said there and the server refuses its replies.
func (h *harness) setUpNets() error {
	var created schema.CreateNetResponseV1
	if err := h.op.send(http.MethodPost, "/v1/channels/ops/nets", schema.CreateNetRequestV1{Name: "ops-net"}, &created); err != nil {
		return err
	}
	h.netID = created.Net.ID
	for _, id := range []int64{h.botID, h.peerID} {
		if err := h.op.send(http.MethodPut, fmt.Sprintf("/v1/channels/ops/nets/ops-net/members/%d", id), schema.PutNetMemberRequestV1{Role: schema.NetRoleMember}, nil); err != nil {
			return err
		}
	}
	if err := h.op.send(http.MethodPost, "/v1/channels/ops/nets", schema.CreateNetRequestV1{Name: "watch-net"}, &created); err != nil {
		return err
	}
	h.watchNetID = created.Net.ID
	for id, role := range map[int64]schema.NetRole{h.botID: schema.NetRoleMonitor, h.peerID: schema.NetRoleMember} {
		if err := h.op.send(http.MethodPut, fmt.Sprintf("/v1/channels/ops/nets/watch-net/members/%d", id), schema.PutNetMemberRequestV1{Role: role}, nil); err != nil {
			return err
		}
	}
	return nil
}

// humanReply is what the fixture answers to the human's message: the last
// line of the prompt, "<author id>: <body>".
func (h *harness) humanReply(body string) string { return fmt.Sprintf("%d: %s", h.humanID, body) }

// checkSessionAndRun looks at what the adapter has told the server so far,
// through the API a client uses: one online session with what it declared,
// and one completed run for the reply, answering the message it was asked
// about, with the activity of a successful reply in order.
func (h *harness) checkSessionAndRun(asked schema.MessageV1) error {
	// The terminal condition is part of the wait: the session is idle with no
	// run open, and the run for the message is completed.
	sessions, err := h.waitSessions(func(s []schema.SessionV1) bool {
		return len(s) == 1 && s[0].Status == schema.SessionStatusIdle && s[0].CurrentRun == 0
	})
	if err != nil {
		return err
	}
	session := sessions[0]
	wantKinds := []schema.InterventionKind{schema.InterventionKindMessage, schema.InterventionKindCancel}
	if session.Client != "conch-adapter" || session.PrincipalID != h.botID || session.State != schema.SessionStateOnline ||
		session.Runtime != "claude-code" || session.Model != "sonnet" || session.Host == "" || !reflect.DeepEqual(session.Interventions, wantKinds) {
		return fmt.Errorf("the adapter's session = %+v", session)
	}
	run, err := h.waitRun(asked.ID, func(r schema.RunV1) bool { return r.State == schema.RunStateCompleted })
	if err != nil {
		return err
	}
	if run.SessionID != session.ID || run.Outcome != "replied" || run.Task != "first live message" {
		return fmt.Errorf("the run = %+v, want completed 'replied' answering message %d", run, asked.ID)
	}
	kinds, err := h.activityKinds(run.ID, "first live message")
	if err != nil {
		return err
	}
	want := []schema.ActivityKind{schema.ActivityKindCommandStarted, schema.ActivityKindCommandFinished, schema.ActivityKindReplyPosted}
	if !reflect.DeepEqual(kinds, want) {
		return fmt.Errorf("activity kinds = %v, want %v", kinds, want)
	}
	return nil
}

// waitRun polls for the run that answers a message until ok is true of it.
func (h *harness) waitRun(messageID int64, ok func(schema.RunV1) bool) (schema.RunV1, error) {
	deadline := time.Now().Add(5 * time.Second)
	var last *schema.RunV1
	for {
		var runs schema.ListRunsResponseV1
		if err := h.human.send(http.MethodGet, "/v1/runs?channel=ops&limit=100", nil, &runs); err != nil {
			return schema.RunV1{}, err
		}
		for i, r := range runs.Runs {
			if r.MessageID == messageID {
				last = &runs.Runs[i]
				if ok(r) {
					return r, nil
				}
			}
		}
		if time.Now().After(deadline) {
			return schema.RunV1{}, fmt.Errorf("the run answering message %d never became what the check wants: %+v", messageID, last)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// activityKinds is the kinds of a run's activity in order. quoted is text no
// entry may contain.
func (h *harness) activityKinds(runID int64, quoted string) ([]schema.ActivityKind, error) {
	var activity schema.ListActivityResponseV1
	if err := h.human.send(http.MethodGet, fmt.Sprintf("/v1/runs/%d/activity", runID), nil, &activity); err != nil {
		return nil, err
	}
	var kinds []schema.ActivityKind
	for _, entry := range activity.Activity {
		kinds = append(kinds, entry.Kind)
		if strings.Contains(entry.Summary+entry.Outcome, quoted) {
			return nil, fmt.Errorf("an activity entry quotes the message: %+v", entry)
		}
	}
	return kinds, nil
}

// fixtureApproval runs the adapter on approval.sh with the fixture markers on:
// the run goes to waiting while a person decides the approval it raised,
// through the API, and then completes. No model is involved.
func (h *harness) fixtureApproval() error {
	h.command, h.fixtureOn = h.approval, true
	defer func() { h.command, h.fixtureOn = h.fixture, false }()
	if err := h.startBot(h.adapter, false); err != nil {
		return err
	}
	if err := h.waitNewSession(); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	asked, err := h.postHumanMessage("please approve this")
	if err != nil {
		return err
	}
	run, err := h.waitRun(asked.ID, func(r schema.RunV1) bool { return r.State == schema.RunStateWaiting })
	if err != nil {
		return fmt.Errorf("the run never went to waiting: %w", err)
	}
	// The approval it raised is open, and nobody has decided it.
	var open schema.ListApprovalsResponseV1
	deadline := time.Now().Add(5 * time.Second)
	for len(open.Approvals) == 0 {
		if err := h.op.send(http.MethodGet, "/v1/approvals", nil, &open); err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no approval was raised")
		}
		time.Sleep(25 * time.Millisecond)
	}
	approval := open.Approvals[0]
	if approval.Title != "Approve the fixture action" || approval.RequesterID != h.botID {
		return fmt.Errorf("the approval = %+v", approval)
	}
	if err := h.op.send(http.MethodPost, fmt.Sprintf("/v1/approvals/%d/decisions", approval.ID),
		schema.CastDecisionRequestV1{PrincipalID: h.opID, OptionID: "approve", Reason: "e2e fixture"}, nil); err != nil {
		return fmt.Errorf("decide the approval: %w", err)
	}
	run, err = h.waitRun(asked.ID, func(r schema.RunV1) bool { return r.State == schema.RunStateCompleted })
	if err != nil {
		return fmt.Errorf("the run did not complete after the approval: %w", err)
	}
	kinds, err := h.activityKinds(run.ID, "please approve this")
	if err != nil {
		return err
	}
	want := []schema.ActivityKind{
		schema.ActivityKindCommandStarted, schema.ActivityKindApprovalRequested, schema.ActivityKindApprovalResolved,
		schema.ActivityKindCommandFinished, schema.ActivityKindReplyPosted,
	}
	if !reflect.DeepEqual(kinds, want) {
		return fmt.Errorf("activity kinds = %v, want %v", kinds, want)
	}
	if err := h.waitForReplies(append(h.botRepliesSoFar(), h.humanReply("please approve this"))...); err != nil {
		return err
	}
	if err := h.stopBotClean(); err != nil {
		return err
	}
	return h.checkSessionEnded()
}

// codexProfile runs the adapter as the codex runtime on its default command,
// with the stub for Codex: the session says codex and the model it was given,
// the run's activity is the one command of the recorded run, and the reply is
// the recorded run's last agent message. No prompt text reaches the activity.
func (h *harness) codexProfile() error {
	h.codex = true
	defer func() { h.codex = false }()
	if err := h.startBot(h.adapter, false); err != nil {
		return err
	}
	if err := h.waitNewSession(); err != nil {
		return err
	}
	time.Sleep(300 * time.Millisecond)
	asked, err := h.postHumanMessage("codex live message")
	if err != nil {
		return err
	}
	want := append(h.botRepliesSoFar(), h.humanReply("please approve this"), "done")
	if err := h.waitForReplies(want...); err != nil {
		return err
	}
	sessions, err := h.waitSessions(func(s []schema.SessionV1) bool {
		return len(s) == 1 && s[0].Status == schema.SessionStatusIdle && s[0].CurrentRun == 0
	})
	if err != nil {
		return err
	}
	if s := sessions[0]; s.Runtime != "codex" || s.Model != codexModel {
		return fmt.Errorf("the adapter's session = %+v, want runtime codex and model %s", s, codexModel)
	}
	run, err := h.waitRun(asked.ID, func(r schema.RunV1) bool { return r.State == schema.RunStateCompleted })
	if err != nil {
		return err
	}
	var activity schema.ListActivityResponseV1
	if err := h.human.send(http.MethodGet, fmt.Sprintf("/v1/runs/%d/activity", run.ID), nil, &activity); err != nil {
		return err
	}
	var got []string
	for _, entry := range activity.Activity {
		got = append(got, string(entry.Kind)+"|"+entry.Summary+"|"+entry.Outcome)
	}
	wantActivity := []string{"command_started|started bash|", "command_finished|command finished|exit 0", "reply_posted|posted the reply|"}
	if run.Outcome != "replied" || !reflect.DeepEqual(got, wantActivity) {
		return fmt.Errorf("the codex run = %+v with activity %q, want 'replied' and %q", run, got, wantActivity)
	}
	for _, entry := range activity.Activity {
		if strings.Contains(entry.Summary+entry.Outcome+string(entry.Data), "codex live message") {
			return fmt.Errorf("an activity entry quotes the message: %+v", entry)
		}
	}
	if err := h.stopBotClean(); err != nil {
		return err
	}
	return h.checkSessionEnded()
}

// codexModel is the model the check gives the codex runtime (required there).
const codexModel = "codex-stub"

// botRepliesSoFar is the channel-wide replies the earlier steps expect.
func (h *harness) botRepliesSoFar() []string {
	return []string{
		h.humanReply("first live message"), h.humanReply("post-restart live message"), h.humanReply("after the refusal"),
	}
}

// waitNewSession waits for the adapter to have one online session that is
// newer than any before it.
func (h *harness) waitNewSession() error {
	sessions, err := h.waitSessions(func(s []schema.SessionV1) bool {
		return len(s) == 1 && s[0].ID > h.lastSession && s[0].State == schema.SessionStateOnline
	})
	if err != nil {
		return err
	}
	h.lastSession = sessions[0].ID
	return nil
}

// checkSessionEnded checks that after a stop the adapter's session has ended
// (an ended session is no longer in the roster), no run is left open, and the
// lock file is gone.
func (h *harness) checkSessionEnded() error {
	if _, err := h.waitSessions(func(s []schema.SessionV1) bool { return len(s) == 0 }); err != nil {
		return fmt.Errorf("the adapter's session did not end: %w", err)
	}
	if _, err := os.Stat(h.lock); err == nil {
		return fmt.Errorf("lock file %s is still there after the adapter stopped", h.lock)
	}
	var runs schema.ListRunsResponseV1
	if err := h.human.send(http.MethodGet, "/v1/runs?channel=ops", nil, &runs); err != nil {
		return err
	}
	for _, run := range runs.Runs {
		if !schema.RunStateTerminal(run.State) {
			return fmt.Errorf("run %d is still %s after the adapter stopped", run.ID, run.State)
		}
	}
	return nil
}

// waitSessions polls the roster as the operator (who sees runtime and host)
// until ok is true, and returns the adapter's sessions, oldest first.
func (h *harness) waitSessions(ok func([]schema.SessionV1) bool) ([]schema.SessionV1, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var roster schema.SessionRosterV1
		if err := h.op.send(http.MethodGet, "/v1/sessions?channel=ops", nil, &roster); err != nil {
			return nil, err
		}
		var mine []schema.SessionV1
		for _, s := range roster.Sessions {
			if s.PrincipalID == h.botID {
				mine = append(mine, s)
			}
		}
		if ok(mine) {
			return mine, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the adapter's sessions never became what the check wants: %+v", mine)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// aliasStillStarts runs conch-bot once, with the old CONCH_BOT_* names only.
func (h *harness) aliasStillStarts() error {
	if err := h.startBot(h.alias, true); err != nil {
		return err
	}
	if err := h.waitNewSession(); err != nil {
		return fmt.Errorf("conch-bot did not start: %w", err)
	}
	if err := h.stopBotClean(); err != nil {
		return err
	}
	if got := strings.Count(h.botStderr.String(), "deprecated"); got != 1 {
		return fmt.Errorf("conch-bot said it is deprecated %d times, want once; stderr:\n%s", got, h.botStderr.String())
	}
	return h.checkSessionEnded()
}

// refusedNetExchange is the case the feature exists for. The peer speaks on a
// net the bot only monitors. The bot reads the message and its reply is
// refused by the real server; the bot must then post nothing anywhere, in
// particular not in the open channel, and must carry on: the next channel-wide
// message still gets its answer.
func (h *harness) refusedNetExchange(before []string) error {
	ctx := context.Background()
	peer := mcpclient.New(h.url(), h.peerToken)
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
	if err := h.assertReplies(before...); err != nil {
		return fmt.Errorf("a refused net reply was sent to the open channel: %w", err)
	}
	// Not stuck in a retry or backoff: a channel-wide message is answered.
	if err := h.postHuman("after the refusal"); err != nil {
		return err
	}
	after := append(append([]string(nil), before...), h.humanReply("after the refusal"))
	if err := h.waitForReplies(after...); err != nil {
		return fmt.Errorf("the bot stopped answering after a refused reply: %w", err)
	}
	time.Sleep(300 * time.Millisecond)
	if err := h.assertReplies(after...); err != nil {
		return err
	}
	raw, err := httpGet(h.human, "/v2/channels/ops/messages")
	if err != nil {
		return err
	}
	if strings.Contains(raw, "WATCH-NET-PROMPT") {
		return fmt.Errorf("the monitored net's prompt is visible in the open channel: %s", raw)
	}
	// The server does not let a monitor start a run on that net either, so the
	// adapter has none for this message; the end of the program checks that no
	// run was left open.
	return nil
}

// netExchange has the peer post a prompt on the net over MCP (a human cannot
// post a scoped message over REST). The bot must answer on the same net. The
// outsider, an agent in the channel reading over MCP, must see neither
// message: that is the check that matters. REST is also read as the human, who
// is not on the net, where no scoped message may ever appear.
func (h *harness) netExchange(replies []string) error {
	ctx := context.Background()
	netAudience := &schema.Audience{Kind: schema.AudienceKindNet, NetID: h.netID}
	peer := mcpclient.New(h.url(), h.peerToken)
	outsider := mcpclient.New(h.url(), h.outsiderToken)
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
	want := fmt.Sprintf("%d: %s", h.peerID, prompt)
	if reply.Audience.Kind != schema.AudienceKindNet || reply.Audience.NetID != h.netID || reply.Body != want {
		return fmt.Errorf("net reply = %+v (audience %+v), want %q on net %d", reply, reply.Audience, want, h.netID)
	}
	// Give a wrongly channel-wide reply time to show up before looking.
	time.Sleep(300 * time.Millisecond)
	if err := h.assertReplies(replies...); err != nil {
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
		raw, err := httpGet(h.human, fmt.Sprintf("/%s/channels/ops/messages", version))
		if err != nil {
			return err
		}
		if strings.Contains(raw, "NET-PROMPT") || strings.Contains(raw, fmt.Sprintf(`"id":%d,`, reply.ID)) {
			return fmt.Errorf("REST %s shows the net exchange to someone who is not on the net: %s", version, raw)
		}
	}
	return nil
}

// assertReplies checks the bot's channel-wide replies, in order, are exactly
// want.
func (h *harness) assertReplies(want ...string) error {
	var result schema.ListMessagesResponseV1
	if err := h.human.send(http.MethodGet, "/v1/channels/ops/messages", nil, &result); err != nil {
		return err
	}
	var got []string
	for _, message := range result.Messages {
		if message.AuthorID == h.botID {
			got = append(got, message.Body)
		}
	}
	if !reflect.DeepEqual(got, want) {
		return fmt.Errorf("the bot's replies = %q, want %q", got, want)
	}
	return nil
}

func (h *harness) waitForReplies(want ...string) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := h.assertReplies(want...); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return h.assertReplies(want...)
}

func httpGet(r rest, path string) (string, error) {
	status, body, err := r.do(http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("GET %s status %d", path, status)
	}
	return string(body), nil
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

func (h *harness) startServer() error {
	h.server = exec.Command(h.conchd, "serve", "--data", h.data, "--listen", h.addr) // #nosec G204 -- test-built binary and controlled arguments
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

// stopBotClean sends SIGTERM and requires the adapter to exit by itself, with
// status 0, which is what ending its session and releasing its lock takes.
func (h *harness) stopBotClean() error {
	if h.botProc == nil || h.botProc.Process == nil {
		return nil
	}
	proc := h.botProc
	h.botProc = nil
	if err := proc.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("the adapter did not exit cleanly on SIGTERM: %w", err)
		}
		return nil
	case <-time.After(5 * time.Second):
		_ = proc.Process.Kill()
		<-done
		return fmt.Errorf("the adapter did not exit within 5s of SIGTERM")
	}
}

// stopBot is the deferred cleanup: kill whatever is still running.
func (h *harness) stopBot() {
	if h.botProc != nil && h.botProc.Process != nil {
		_ = h.botProc.Process.Kill()
		_ = h.botProc.Wait()
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

// startBot starts the adapter, or with legacy its old name, configured with the
// CONCH_ADAPTER_* names or with legacy only the old CONCH_BOT_* ones.
func (h *harness) startBot(binary string, legacy bool) error {
	prefix := "CONCH_ADAPTER_"
	if legacy {
		prefix = "CONCH_BOT_"
	}
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "CONCH_") || strings.HasPrefix(kv, "CLAUDE_BIN=") || strings.HasPrefix(kv, "CODEX_BIN=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		prefix+"SERVER="+h.url(), prefix+"TOKEN="+h.botToken,
		prefix+"PRINCIPAL_ID="+strconv.FormatInt(h.botID, 10), prefix+"CHANNEL=ops",
		prefix+"POLL_INTERVAL=50ms", prefix+"MAX_BACKOFF=200ms",
		prefix+"REPLY_TIMEOUT=5s", prefix+"LOCK_FILE="+h.lock,
		"CONCH_ADAPTER_HEARTBEAT=200ms",
	)
	if h.codex {
		// The default Codex command, with the stub in place of codex.
		env = append(env, "CONCH_ADAPTER_RUNTIME=codex", "CONCH_ADAPTER_MODEL="+codexModel, "CODEX_BIN="+h.codexStub)
	} else {
		// The fixture takes the prompt on standard input; CLAUDE_BIN is not set.
		env = append(env, "CONCH_ADAPTER_CMD="+h.command)
	}
	if h.fixtureOn {
		env = append(env, "CONCH_ADAPTER_FIXTURE=1")
	}
	h.botStderr = &bytes.Buffer{}
	h.botProc = exec.Command(binary) // #nosec G204 -- test-built binary
	h.botProc.Env = env
	h.botProc.Stdout = os.Stdout
	h.botProc.Stderr = io.MultiWriter(os.Stderr, h.botStderr)
	return h.botProc.Start()
}

// rest is a REST client for one credential.
type rest struct{ base, token string }

func (r rest) do(method, path string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequest(method, r.base+path, reader) // #nosec G107 -- test-local server
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

// send does a request that must answer 2xx and decodes the answer into out
// when it is not nil. A failure names the status, never the body.
func (r rest) send(method, path string, body, out any) error {
	status, data, err := r.do(method, path, body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("%s %s status %d", method, path, status)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func createChannel(r rest, name string) (int64, error) {
	var response schema.CreateChannelResponse
	err := r.send(http.MethodPost, "/v0/channels", schema.CreateChannelRequest{Name: name}, &response)
	return response.Channel.ID, err
}

func createPrincipal(r rest, kind schema.PrincipalKind, name string) (int64, error) {
	var response schema.CreatePrincipalResponse
	err := r.send(http.MethodPost, "/v0/principals", schema.CreatePrincipalRequest{Kind: kind, Name: name}, &response)
	return response.Principal.ID, err
}

func (h *harness) postHuman(body string) error {
	_, err := h.postHumanMessage(body)
	return err
}

func (h *harness) postHumanMessage(body string) (schema.MessageV1, error) {
	var posted schema.PostMessageResponseV1
	err := h.human.send(http.MethodPost, "/v1/channels/ops/messages", schema.PostMessageRequestV1{AuthorID: h.humanID, Body: body}, &posted)
	return posted.Message, err
}
