package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// P3-15: conch-adapter in relay mode (issue #387). A script that cannot hold a
// session reports a CI-like run through the relay's Unix socket, and the API
// and the Agents view show it as they show any agent's run. The row also
// checks what the relay refuses itself, what it passes back from conchd, that
// it answers a request nobody took, and what it leaves behind when it stops.

// relayTimeout is how long the relay waits for the runtime to take an
// intervention here, so that the row does not wait a minute.
const relayTimeout = "4s"

// relaySocket is the script's side: a client of one relay's Unix socket.
type relaySocket struct{ path string }

var relayHTTP = func(path string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}},
	}
}

// send sends one request through the socket. A []byte body is sent as it is,
// any other non-nil body as JSON.
func (rs relaySocket) send(method, path string, body any) (response, error) {
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		reader = bytes.NewReader(b)
	default:
		encoded, err := json.Marshal(b)
		if err != nil {
			return response{}, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(runCtx, method, "http://relay"+path, reader)
	if err != nil {
		return response{}, err
	}
	resp, err := relayHTTP(rs.path).Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, body: data}, err
}

// call sends a request that must succeed and decodes the answer into out.
func (rs relaySocket) call(method, path string, body, out any) error {
	resp, err := rs.send(method, path, body)
	if err != nil {
		return err
	}
	if resp.status < 200 || resp.status >= 300 {
		return fmt.Errorf("relay %s %s: status %d (%s)", method, path, resp.status, errorCode(resp.body))
	}
	if out == nil {
		return nil
	}
	if err := decodeStrict(resp.body, out); err != nil {
		return fmt.Errorf("relay %s %s: %w", method, path, err)
	}
	return nil
}

// refused sends a request that must be refused with this status and code, in
// the schema's error shape.
func (rs relaySocket) refused(what, method, path string, body any, status int, code string) error {
	resp, err := rs.send(method, path, body)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	var e schema.Error
	if resp.status != status || decodeStrict(resp.body, &e) != nil || e.Code != code || e.Message == "" {
		return fmt.Errorf("%s: %s %s answered %d (%s), want %d %s", what, method, path, resp.status, errorCode(resp.body), status, code)
	}
	return nil
}

// launchRelay starts a relay as part, in a socket directory of its own, and
// waits for its session and its socket. The directory is private (mode 0700)
// and short, which a socket path has to be.
func (s *suite) launchRelay(part, runtime, dir string, extra ...string) (*agent, relaySocket, error) {
	env := []string{
		"CONCH_ADAPTER_MODE=relay",
		"CONCH_ADAPTER_RUNTIME=" + runtime,
		// The default socket: $XDG_RUNTIME_DIR/conch-adapter-relay.sock.
		"XDG_RUNTIME_DIR=" + dir,
		"CONCH_ADAPTER_LOCK_FILE=" + s.r.path(part+"-relay.lock"),
	}
	a, err := s.launchAgent(agentSpec{part: part, script: "echo.sh", env: append(env, extra...)})
	if err != nil {
		return nil, relaySocket{}, err
	}
	sock := relaySocket{path: filepath.Join(dir, "conch-adapter-relay.sock")}
	if err := s.until("the relay's socket", 10*time.Second, func() (bool, error) {
		_, err := os.Lstat(sock.path)
		return err == nil, nil
	}); err != nil {
		return nil, relaySocket{}, err
	}
	return a, sock, nil
}

// relayDir makes a private directory for a socket.
func (s *suite) relayDir() (string, error) {
	dir, err := os.MkdirTemp("", "p3r")
	if err != nil {
		return "", err
	}
	s.cleanup(func() { _ = os.RemoveAll(dir) })
	return dir, nil
}

func (s *suite) p315Relay() error {
	if err := s.stopAgent(s.worker); err != nil {
		return err
	}
	s.worker = nil
	if err := s.stopAgent(s.observer); err != nil {
		return err
	}
	s.observer = nil

	// conchd's refusal comes back through the relay as it is: observer-test's
	// manifest has no runs.manage, so its relay holds a session and cannot
	// start a run.
	if err := s.relayPassThrough(); err != nil {
		return err
	}

	dir, err := s.relayDir()
	if err != nil {
		return err
	}
	a, sock, err := s.launchRelay(partWorker, "github-actions", dir, "CONCH_ADAPTER_INTERVENTIONS=cancel", "CONCH_ADAPTER_INTERVENTION_TIMEOUT="+relayTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = s.stopAgent(a) }() // a no-op once it is stopped below
	session := a.session

	// The socket and its directory belong to this user and to no one else.
	for path, want := range map[string]os.FileMode{dir: 0o700, sock.path: 0o600} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode().Perm() != want || (path == sock.path && info.Mode()&os.ModeSocket == 0) {
			return fmt.Errorf("%s is %v, want mode %04o (a socket for the second)", path, info.Mode(), want)
		}
	}
	var own schema.SessionV1
	if err := sock.call(http.MethodGet, "/v1/session", nil, &own); err != nil {
		return err
	}
	if err := own.Validate(); err != nil {
		return fmt.Errorf("GET /v1/session: %w", err)
	}
	if own.ID != session || own.Runtime != "github-actions" {
		return fmt.Errorf("GET /v1/session = %+v, want session #%d of the github-actions runtime", own, session)
	}
	step("P3-15 the relay holds session #%d; its socket is mode 0600 in a 0700 directory", session)

	t := s.tuiA
	if err := t.selectChannel(chGeneral); err != nil {
		return err
	}
	if err := s.openAgentsView(t); err != nil {
		return err
	}
	if err := s.refold(t, session); err != nil {
		return err
	}
	if err := s.agree(t, session, schema.SessionStatusIdle, 0, ""); err != nil {
		return err
	}

	// A CI job: the steps are separate commands of a script, and none of them
	// holds a session. Each report is one request on the socket.
	var run schema.RunV1
	if err := sock.call(http.MethodPost, "/v1/runs", schema.CreateRunRequestV1{Channel: chGeneral, Task: "p315 nightly build", Link: "https://ci.example.com/jobs/7421"}, &run); err != nil {
		return err
	}
	if err := run.Validate(); err != nil {
		return fmt.Errorf("the run the relay returned: %w", err)
	}
	if run.SessionID != session {
		return fmt.Errorf("the run is %+v, want one of session #%d", run, session)
	}
	report := func(state schema.RunState, outcome string) error {
		return sock.call(http.MethodPost, "/v1/runs/"+id(run.ID)+"/state", schema.SetRunStateRequestV1{State: state, Outcome: outcome}, nil)
	}
	activity := func(kind schema.ActivityKind, summary, outcome string, data json.RawMessage) error {
		return sock.call(http.MethodPost, "/v1/runs/"+id(run.ID)+"/activity", schema.AppendActivityRequestV1{Kind: kind, Summary: summary, Outcome: outcome, Data: data}, nil)
	}
	if err := s.agree(t, session, schema.SessionStatusRunning, run.ID, schema.RunStateQueued); err != nil {
		return err
	}
	const stepData = `{"exit":0,"junit":[1,2.50,12345678901234567890]}`
	reports := []struct {
		do     func() error
		status schema.SessionStatus
		state  schema.RunState
	}{
		{func() error { return report(schema.RunStateStarting, "") }, schema.SessionStatusRunning, schema.RunStateStarting},
		{func() error { return report(schema.RunStateRunning, "") }, schema.SessionStatusRunning, schema.RunStateRunning},
		{func() error { return activity(schema.ActivityKindCommandStarted, "go test ./...", "", nil) }, schema.SessionStatusRunning, schema.RunStateRunning},
		{func() error {
			return activity(schema.ActivityKindCommandFinished, "go test ./...", "passed", json.RawMessage(stepData))
		}, schema.SessionStatusRunning, schema.RunStateRunning},
		{func() error { return report(schema.RunStateWaiting, "") }, schema.SessionStatusWaiting, schema.RunStateWaiting},
		{func() error { return report(schema.RunStateRunning, "") }, schema.SessionStatusRunning, schema.RunStateRunning},
		{func() error { return activity(schema.ActivityKindRunEnded, "build finished", "passed", nil) }, schema.SessionStatusRunning, schema.RunStateRunning},
		{func() error { return report(schema.RunStateCompleted, "passed") }, schema.SessionStatusIdle, schema.RunStateCompleted},
	}
	for i, rep := range reports {
		if err := rep.do(); err != nil {
			return fmt.Errorf("report %d of the CI run: %w", i+1, err)
		}
		if err := s.agree(t, session, rep.status, run.ID, rep.state); err != nil {
			return fmt.Errorf("after report %d of the CI run: %w", i+1, err)
		}
	}
	done, err := s.getRun(run.ID)
	if err != nil {
		return err
	}
	if done.Task != "p315 nightly build" || done.Outcome != "passed" || done.PrincipalID != s.pid("worker-test") || done.Link != "https://ci.example.com/jobs/7421" {
		return fmt.Errorf("the finished run is %+v", done)
	}
	var acts schema.ListActivityResponseV1
	if err := s.as(partNick).call(http.MethodGet, "/v1/runs/"+id(run.ID)+"/activity", nil, &acts); err != nil {
		return err
	}
	var kinds []schema.ActivityKind
	for _, a := range acts.Activity {
		kinds = append(kinds, a.Kind)
	}
	if want := []schema.ActivityKind{schema.ActivityKindCommandStarted, schema.ActivityKindCommandFinished, schema.ActivityKindRunEnded}; !slices.Equal(kinds, want) {
		return fmt.Errorf("the run has the activity %v, want %v", kinds, want)
	}
	// The relay does not re-encode what it carries: the numbers and their
	// order come out as they went in (MCP would sort the keys and make the
	// large integer a float).
	if got := string(acts.Activity[1].Data); got != stepData {
		return fmt.Errorf("the step's data came back as %s, want %s", got, stepData)
	}
	step("P3-15 a CI run reported through the socket: the API and the Agents view agree at each of its %d reports, and its activity data is byte-for-byte as sent", len(reports)+1)

	// What the relay refuses itself, end to end. None of it is a request to
	// conchd, so none of it is in the audit log.
	refusals := []struct {
		what, method, path string
		body               any
		status             int
		code               string
	}{
		{"another channel", http.MethodPost, "/v1/runs", schema.CreateRunRequestV1{Channel: chPrivate, Task: "p315 elsewhere"}, http.StatusNotFound, "channel_not_found"},
		{"a run the relay did not create", http.MethodPost, "/v1/runs/" + id(run.ID+1000) + "/state", schema.SetRunStateRequestV1{State: schema.RunStateRunning}, http.StatusNotFound, schema.ErrorCodeRunNotFound},
		{"an intervention it did not return", http.MethodPost, "/v1/interventions/999999/ack", schema.AckInterventionRequestV1{State: schema.InterventionStateApplied}, http.StatusNotFound, schema.ErrorCodeInterventionNotFound},
		{"an unknown field", http.MethodPost, "/v1/runs/" + id(run.ID) + "/state", []byte(`{"state":"running","extra":1}`), http.StatusBadRequest, "invalid_request"},
		{"a body over 1 MiB", http.MethodPost, "/v1/runs/" + id(run.ID) + "/activity", []byte(`{"kind":"command_started","summary":"` + strings.Repeat("x", schema.MaxPostRequestBytes) + `"}`), http.StatusRequestEntityTooLarge, "request_too_large"},
	}
	for _, rf := range refusals {
		if err := sock.refused(rf.what, rf.method, rf.path, rf.body, rf.status, rf.code); err != nil {
			return err
		}
	}
	// A state conchd refuses comes back as conchd said it.
	if err := sock.refused("a move out of a final state", http.MethodPost, "/v1/runs/"+id(run.ID)+"/state", schema.SetRunStateRequestV1{State: schema.RunStateRunning}, http.StatusConflict, schema.ErrorCodeInvalidTransition); err != nil {
		return err
	}
	if err := s.closeAgentsView(t); err != nil {
		return err
	}
	step("P3-15 the relay refuses another channel, a foreign run, a foreign intervention, an unknown field and 1 MiB itself; conchd's 409 comes back as it was")

	// An intervention nobody takes: the runtime sees it, does not answer, and
	// the relay answers failed after the timeout. The late answer is conchd's
	// 409.
	var open schema.RunV1
	if err := sock.call(http.MethodPost, "/v1/runs", schema.CreateRunRequestV1{Channel: chGeneral, Task: "p315 left open"}, &open); err != nil {
		return err
	}
	if err := sock.call(http.MethodPost, "/v1/runs/"+id(open.ID)+"/state", schema.SetRunStateRequestV1{State: schema.RunStateRunning}, nil); err != nil {
		return err
	}
	var asked schema.InterventionV1
	if err := s.as(partNick).call(http.MethodPost, "/v1/runs/"+id(open.ID)+"/interventions", schema.CreateInterventionRequestV1{Kind: schema.InterventionKindCancel}, &asked); err != nil {
		return err
	}
	var polled schema.ListInterventionsResponseV1
	if err := sock.call(http.MethodGet, "/v1/interventions?state=requested", nil, &polled); err != nil {
		return err
	}
	if len(polled.Interventions) != 1 || polled.Interventions[0].ID != asked.ID || polled.Interventions[0].Kind != schema.InterventionKindCancel {
		return fmt.Errorf("the poll through the socket returned %+v, want the cancel #%d", polled, asked.ID)
	}
	var answered schema.InterventionV1
	if err := s.until("the relay to answer the cancel nobody took", 30*time.Second, func() (bool, error) {
		if err := s.as(partNick).call(http.MethodGet, "/v1/interventions/"+id(asked.ID), nil, &answered); err != nil {
			return false, err
		}
		return answered.State != schema.InterventionStateRequested, nil
	}); err != nil {
		return err
	}
	if answered.State != schema.InterventionStateFailed || answered.Reason != "not taken by github-actions within "+relayTimeout {
		return fmt.Errorf("the unanswered cancel is %q with the reason %q, want failed and the relay's reason", answered.State, answered.Reason)
	}
	if err := sock.refused("a late answer", http.MethodPost, "/v1/interventions/"+id(asked.ID)+"/ack", schema.AckInterventionRequestV1{State: schema.InterventionStateApplied}, http.StatusConflict, schema.ErrorCodeInvalidTransition); err != nil {
		return err
	}
	r, err := s.getRun(open.ID)
	if err != nil {
		return err
	}
	if r.State != schema.RunStateRunning {
		return fmt.Errorf("answering the cancel moved run %d: %+v", open.ID, r)
	}
	step("P3-15 a cancel nobody took is answered failed by the relay after %s, and the late answer meets 409", relayTimeout)

	// SIGTERM: the session ends, conchd fails the run it left open (issue #402),
	// the socket goes.
	if err := s.stopAgent(a); err != nil {
		return err
	}
	if _, err := os.Lstat(sock.path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("the socket is still there after SIGTERM (Lstat: %w)", err)
	}
	left, err := s.getRun(open.ID)
	if err != nil {
		return err
	}
	if left.State != schema.RunStateFailed || left.EndedAt == nil || left.Outcome != "session ended" {
		return fmt.Errorf("the relay stopped and run %d is %+v, want it failed with outcome %q", open.ID, left, "session ended")
	}
	step("P3-15 SIGTERM ended the session, took the socket away and conchd failed run #%d as %q", open.ID, "session ended")

	return s.relayDies()
}

// relayPassThrough starts a relay with observer-test's credential, which has no
// runs.manage, and shows that conchd's refusal of a run comes back unchanged.
func (s *suite) relayPassThrough() error {
	dir, err := s.relayDir()
	if err != nil {
		return err
	}
	a, sock, err := s.launchRelay(partObserver, "audit-script", dir)
	if err != nil {
		return err
	}
	defer func() { _ = s.stopAgent(a) }()
	if err := sock.refused("a start without runs.manage", http.MethodPost, "/v1/runs", schema.CreateRunRequestV1{Channel: chGeneral, Task: "p315 not allowed"}, http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	// The same refusal, asked of conchd directly, is the same answer.
	direct := s.as(partObserver)
	if err := direct.refused("the same start, direct", http.MethodPost, "/v1/sessions/"+id(a.session)+"/runs", schema.CreateRunRequestV1{Channel: chGeneral, Task: "p315 not allowed"}, http.StatusNotFound, "channel_not_found"); err != nil {
		return err
	}
	step("P3-15 a relay whose manifest has no runs.manage gets conchd's refusal of a run, passed back unchanged")
	return nil
}

// relayDies kills a relay in the middle of a run, which leaves the run open on
// a session nobody hears from: the roster shows it offline with the outcome
// unknown, and a new relay on the same socket path starts over the stale
// socket and lock file.
func (s *suite) relayDies() error {
	dir, err := s.relayDir()
	if err != nil {
		return err
	}
	a, sock, err := s.launchRelay(partWorker, "github-actions", dir)
	if err != nil {
		return err
	}
	var run schema.RunV1
	if err := sock.call(http.MethodPost, "/v1/runs", schema.CreateRunRequestV1{Channel: chGeneral, Task: "p315 dies"}, &run); err != nil {
		return err
	}
	if err := sock.call(http.MethodPost, "/v1/runs/"+id(run.ID)+"/state", schema.SetRunStateRequestV1{State: schema.RunStateRunning}, nil); err != nil {
		return err
	}
	a.c.kill() // SIGKILL: nothing ends the session
	if err := s.until("the session of the dead relay to be offline with the outcome unknown", 40*time.Second, func() (bool, error) {
		roster, err := s.roster(partNick, chGeneral)
		if err != nil {
			return false, err
		}
		sess, ok := sessionOf(roster, a.session)
		return ok && sess.Status == schema.SessionStatusOffline && sess.OutcomeUnknown && sess.CurrentRun == run.ID, nil
	}); err != nil {
		return err
	}
	dead, err := s.getRun(run.ID)
	if err != nil {
		return err
	}
	if dead.State != schema.RunStateRunning || dead.EndedAt != nil || dead.Outcome != "" {
		return fmt.Errorf("run %d after its relay died: %+v; want it left running", run.ID, dead)
	}
	step("P3-15 a relay that died mid-run: the roster shows its session offline, outcome unknown, on run #%d", run.ID)

	// The dead relay left its socket and lock file; a new one takes them over.
	if _, err := os.Lstat(sock.path); err != nil {
		return fmt.Errorf("the dead relay's socket should be left behind: %w", err)
	}
	next, nextSock, err := s.launchRelay(partWorker, "github-actions", dir)
	if err != nil {
		return fmt.Errorf("a relay over its dead predecessor's socket: %w", err)
	}
	var own schema.SessionV1
	if err := nextSock.call(http.MethodGet, "/v1/session", nil, &own); err != nil {
		return err
	}
	if own.ID != next.session {
		return fmt.Errorf("the new relay answers %+v, want session #%d", own, next.session)
	}
	if err := s.stopAgent(next); err != nil {
		return err
	}
	step("P3-15 a new relay started over the stale socket and lock file of the dead one")
	return nil
}
