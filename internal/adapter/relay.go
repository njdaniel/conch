package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// The relay mode of conch-adapter (issue #387; docs/design/webhook-adapter.md
// sections 7 and 8). Some runtimes cannot hold a Conch session themselves: a
// script, a CI step that lasts seconds. The relay is a process beside such a
// runtime. It registers one session and heartbeats it, takes the runtime's
// reports on a Unix socket only its own user can open, and forwards them to
// conchd over REST with the pkg/schema types. It runs no command and reads no
// channel. It does not use MCP, which re-encodes activity data.

const (
	defaultRelaySocketName = "conch-adapter-relay.sock"
	defaultRelayRuntime    = "relay"
	// defaultInterventionTimeout is how long a person's request waits for the
	// runtime before the relay answers it failed.
	defaultInterventionTimeout = 60 * time.Second
	// interventionSweep is how often the relay looks for requests that ran
	// out their time.
	interventionSweep = time.Second
	// maxSocketPath is below the shortest sun_path of the systems that have
	// Unix sockets (104 on the BSDs, 108 on Linux), terminator included.
	maxSocketPath = 100
	// discoverPages bounds how many pages one look for waiting requests reads.
	discoverPages = 10
	// relayServeTimeout bounds one request on the socket, reads and writes.
	relayServeTimeout = 30 * time.Second
)

// The error codes conchd already uses for these refusals. The relay's own
// refusals say the same words, so a sender cannot tell them from conchd's.
const (
	codeChannelNotFound = "channel_not_found"
	codeInvalidRequest  = "invalid_request"
	codeTooLarge        = "request_too_large"
	codeInternal        = "internal_error"
)

// loadRelayConfig reads the settings of relay mode. cfg already holds the
// server, the token and the mode. No command, no principal id and no
// channel reads: the relay needs neither messages.read nor messages.post.
func loadRelayConfig(cfg config, get func(key, legacy, fallback string) string, lookup envLookup) (config, error) {
	if !relaySupported {
		return config{}, errors.New("CONCH_ADAPTER_MODE=relay needs Unix domain sockets, which this system does not have")
	}
	var err error
	if cfg.Channel = get("CONCH_ADAPTER_CHANNEL", "", ""); cfg.Channel == "" {
		return config{}, errors.New("CONCH_ADAPTER_CHANNEL is required")
	}
	if strings.Contains(cfg.Channel, ",") {
		return config{}, errors.New("CONCH_ADAPTER_CHANNEL must contain exactly one channel, not a comma-separated list")
	}
	// A free label here: no profile stands behind it, since no command runs.
	cfg.Runtime = get("CONCH_ADAPTER_RUNTIME", "", defaultRelayRuntime)
	cfg.Model = get("CONCH_ADAPTER_MODEL", "", "")
	if cfg.Heartbeat, err = parseDuration(get("CONCH_ADAPTER_HEARTBEAT", "", "10s"), "CONCH_ADAPTER_HEARTBEAT"); err != nil {
		return config{}, err
	}
	for _, kind := range strings.Split(get("CONCH_ADAPTER_INTERVENTIONS", "", ""), ",") {
		if kind = strings.TrimSpace(kind); kind != "" {
			cfg.Interventions = append(cfg.Interventions, schema.InterventionKind(kind))
		}
	}
	if cfg.InterventionTimeout, err = parseDuration(get("CONCH_ADAPTER_INTERVENTION_TIMEOUT", "", defaultInterventionTimeout.String()), "CONCH_ADAPTER_INTERVENTION_TIMEOUT"); err != nil {
		return config{}, err
	}
	// The labels and kinds are the session's; the schema's own rules say what
	// is wrong with them, before a session is asked for.
	if err := cfg.sessionRequest().Validate(); err != nil {
		return config{}, fmt.Errorf("CONCH_ADAPTER_RUNTIME, CONCH_ADAPTER_MODEL or CONCH_ADAPTER_INTERVENTIONS: %w", err)
	}

	socket := get("CONCH_ADAPTER_RELAY_SOCKET", "", "")
	if socket == "" {
		dir, _ := lookup("XDG_RUNTIME_DIR")
		if dir == "" {
			return config{}, errors.New("relay mode needs XDG_RUNTIME_DIR or CONCH_ADAPTER_RELAY_SOCKET: it listens on a Unix socket in a private directory and never on TCP")
		}
		socket = filepath.Join(dir, defaultRelaySocketName)
	}
	if !filepath.IsAbs(socket) {
		return config{}, errors.New("the relay socket path must be absolute")
	}
	if cfg.RelaySocket = filepath.Clean(socket); len(cfg.RelaySocket) > maxSocketPath {
		return config{}, fmt.Errorf("the relay socket path is longer than %d bytes, which a Unix socket cannot hold", maxSocketPath)
	}
	// The lock sits beside the socket, so it is the socket that is exclusive:
	// two relays on two sockets, or a relay beside a subprocess adapter, do not
	// stop each other.
	cfg.LockFile = get("CONCH_ADAPTER_LOCK_FILE", "", cfg.RelaySocket+".lock")
	return cfg, nil
}

// sessionRequest is the registration of the session the relay holds.
func (c config) sessionRequest() schema.CreateSessionRequestV1 {
	host, _ := os.Hostname() // a relay without a hostname is still a relay
	return schema.CreateSessionRequestV1{
		Client:        sessionClient,
		Runtime:       c.Runtime,
		Model:         c.Model,
		Host:          host,
		Interventions: c.Interventions,
	}
}

// relay is one running relay: its session, the ids it has handed out and the
// requests it is waiting on.
type relay struct {
	cfg config
	up  *upstream
	clk timeSource

	mu sync.Mutex
	// session is the relay's session as of the last heartbeat.
	session schema.SessionV1
	// runs are the runs this relay created. A run is not forgotten when it
	// ends, so that a retried report gets conchd's answer (409) and not ours.
	runs map[int64]struct{}
	// returned are the interventions a poll gave the runtime, which may answer
	// those and no others.
	returned map[int64]struct{}
	// due holds, for each intervention waiting on the runtime, when the relay
	// gives up and answers it failed.
	due map[int64]time.Time
}

func newRelay(cfg config, up *upstream, clk timeSource) *relay {
	return &relay{
		cfg: cfg, up: up, clk: clk,
		runs:     map[int64]struct{}{},
		returned: map[int64]struct{}{},
		due:      map[int64]time.Time{},
	}
}

// runRelay is the program in relay mode. It returns when ctx ends, after it
// has ended its session; runs left open stay as they are.
func runRelay(ctx context.Context, cfg config, clk timeSource) error {
	if err := checkSocketPath(cfg.RelaySocket); err != nil {
		return err
	}
	r := newRelay(cfg, newUpstream(cfg.Server, cfg.Token), clk)
	if err := r.register(ctx); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	// The socket exists only once the session does, so a sender that can
	// connect can ask for it.
	ln, err := listenRelay(ctx, cfg.RelaySocket)
	if err != nil {
		r.endSession(ctx)
		return err
	}
	slog.Info("conch-adapter: relay ready", "session", r.sessionID(), "channel", cfg.Channel)

	srv := &http.Server{
		Handler:           r.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       relayServeTimeout,
		WriteTimeout:      relayServeTimeout,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	background, stopBackground := context.WithCancel(ctx)
	var loops sync.WaitGroup
	loops.Go(func() { r.beat(background) })
	if len(cfg.Interventions) > 0 {
		loops.Go(func() { r.sweep(background) })
	}

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
	}
	stopBackground()
	loops.Wait()
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
	}
	r.endSession(ctx)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("serve the relay socket: %w", serveErr)
	}
	return nil
}

func (r *relay) sessionID() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session.ID
}

// register opens the relay's one session.
func (r *relay) register(ctx context.Context) error {
	session, err := r.up.registerSession(ctx, r.cfg.sessionRequest())
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.session = session
	r.mu.Unlock()
	return nil
}

// beat heartbeats the session until ctx ends, and looks for interventions the
// runtime has not been shown, so that a request is answered even if the
// runtime never polls. A failed heartbeat is logged and the next one tried.
func (r *relay) beat(ctx context.Context) {
	tick := r.clk.newTicker(r.cfg.Heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C():
			session, err := r.up.heartbeat(ctx, r.sessionID())
			switch {
			case err == nil:
				r.mu.Lock()
				r.session = session
				r.mu.Unlock()
			case ctx.Err() == nil:
				logRelayError("heartbeat", err)
			}
			if len(r.cfg.Interventions) > 0 && ctx.Err() == nil {
				r.discover(ctx)
			}
		}
	}
}

// endSession ends the session. It runs after the relay was told to stop, so it
// does not use that context. Open runs are left as they are: the relay does
// not know how an external run ended and does not invent an outcome.
func (r *relay) endSession(ctx context.Context) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := r.up.endSession(closeCtx, r.sessionID()); err != nil {
		logRelayError("end session", err)
	}
}

// logRelayError logs a failed call to conchd: the call, and conchd's status and
// code or the transport's error. Never a body, a token or a path.
func logRelayError(call string, err error) {
	var ref *refusal
	if errors.As(err, &ref) {
		slog.Warn("conch-adapter: relay call refused", "call", call, "status", ref.Status, "code", ref.Code)
		return
	}
	slog.Warn("conch-adapter: relay call failed", "call", call, "error", err)
}

// ----------------------------------------------------------- interventions

// watch records the interventions of a poll that are waiting on the runtime.
func (r *relay) watch(list []schema.InterventionV1, shown bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, iv := range list {
		if shown {
			r.returned[iv.ID] = struct{}{}
		}
		if _, known := r.due[iv.ID]; !known && iv.State == schema.InterventionStateRequested {
			r.due[iv.ID] = r.clk.now().Add(r.cfg.InterventionTimeout)
		}
	}
}

// discover reads what is waiting on the session and starts the clock on what
// the relay has not seen. It does not show these to the runtime: only a poll
// of the runtime's own does that.
func (r *relay) discover(ctx context.Context) {
	query := url.Values{"state": {string(schema.InterventionStateRequested)}, "limit": {"100"}}
	for range discoverPages {
		list, err := r.up.listInterventions(ctx, r.sessionID(), query)
		if err != nil {
			if ctx.Err() == nil {
				logRelayError("list interventions", err)
			}
			return
		}
		r.watch(list.Interventions, false)
		if len(list.Interventions) < 100 {
			return
		}
		query.Set("after", strconv.FormatInt(list.Interventions[len(list.Interventions)-1].ID, 10))
	}
}

// sweep answers failed the requests the runtime did not take in time, once a
// second until ctx ends.
func (r *relay) sweep(ctx context.Context) {
	tick := r.clk.newTicker(interventionSweep)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C():
			r.expire(ctx)
		}
	}
}

// expire answers failed every intervention whose time is up. A person who
// asks gets an answer. A late answer from the runtime then meets conchd's 409.
func (r *relay) expire(ctx context.Context) {
	now := r.clk.now()
	r.mu.Lock()
	var overdue []int64
	for id, at := range r.due {
		if !now.Before(at) {
			overdue = append(overdue, id)
		}
	}
	r.mu.Unlock()
	for _, id := range overdue {
		err := r.up.ack(ctx, id, schema.AckInterventionRequestV1{State: schema.InterventionStateFailed, Reason: r.timeoutReason()})
		var ref *refusal
		switch {
		case err == nil, errors.As(err, &ref) && (ref.Status == http.StatusConflict || ref.Status == http.StatusNotFound):
			// Answered, by us or by the runtime a moment before, or gone.
			r.forget(id)
		case ctx.Err() != nil:
		case errors.As(err, &ref) && ref.Status == http.StatusTooManyRequests:
			// Not before conchd says, so the rate budget is not spent on it.
			logRelayError("answer an overdue intervention", err)
			r.retryAt(id, now.Add(retryDelay(ref.RetryAfter)))
		case errors.As(err, &ref) && ref.Status >= 400 && ref.Status < 500:
			// A revoked credential or a lost capability will not come back by
			// asking again: say so once and give up on this one.
			logRelayError("answer an overdue intervention (given up)", err)
			r.forget(id)
		default:
			logRelayError("answer an overdue intervention", err) // tried again next second
		}
	}
}

// retryAt moves the time an overdue intervention is next tried.
func (r *relay) retryAt(id int64, at time.Time) {
	r.mu.Lock()
	if _, ok := r.due[id]; ok {
		r.due[id] = at
	}
	r.mu.Unlock()
}

const (
	defaultRetryDelay = 5 * time.Second
	maxRetryDelay     = 5 * time.Minute
)

// retryDelay reads a Retry-After in seconds, capped; anything else is the
// default.
func retryDelay(header string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(header))
	if err != nil || seconds <= 0 {
		return defaultRetryDelay
	}
	return min(time.Duration(seconds)*time.Second, maxRetryDelay)
}

func (r *relay) forget(id int64) {
	r.mu.Lock()
	delete(r.due, id)
	r.mu.Unlock()
}

// timeoutReason is the fixed reason of an answer the relay gives.
func (r *relay) timeoutReason() string {
	wait := r.cfg.InterventionTimeout.String()
	if r.cfg.InterventionTimeout%time.Second == 0 {
		wait = strconv.FormatInt(int64(r.cfg.InterventionTimeout/time.Second), 10) + "s"
	}
	reason := fmt.Sprintf("not taken by %s within %s", r.cfg.Runtime, wait)
	if (schema.AckInterventionRequestV1{State: schema.InterventionStateFailed, Reason: reason}).Validate() != nil {
		return "not taken within " + wait
	}
	return reason
}

// ---------------------------------------------------------------- the socket

// handler serves the routes of the socket: conchd's, with the session made
// implicit.
func (r *relay) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/session", r.handleSession)
	mux.HandleFunc("POST /v1/runs", r.handleStartRun)
	mux.HandleFunc("POST /v1/runs/{id}/state", r.handleRunState)
	mux.HandleFunc("POST /v1/runs/{id}/activity", r.handleActivity)
	mux.HandleFunc("GET /v1/interventions", r.handleInterventions)
	mux.HandleFunc("POST /v1/interventions/{id}/ack", r.handleAck)
	return mux
}

// refuse is the relay's own refusal. It logs the code, never a body.
func refuse(w http.ResponseWriter, route string, status int, code, message string) {
	slog.Warn("conch-adapter: relay refused a request", "route", route, "status", status, "code", code)
	writeRelayJSON(w, status, schema.Error{Code: code, Message: message})
}

func writeRelayJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

// pass sends conchd's answer on as it came: status, Retry-After and body.
func pass(w http.ResponseWriter, rep reply) {
	w.Header().Set("Content-Type", "application/json")
	if rep.retryAfter != "" {
		w.Header().Set("Retry-After", rep.retryAfter)
	}
	w.WriteHeader(rep.status)
	_, _ = w.Write(rep.body)
}

// ask sends one call to conchd. When conchd cannot be reached it answers the
// sender itself and reports false.
func (r *relay) ask(w http.ResponseWriter, req *http.Request, route, method, path string, query url.Values, body []byte) (reply, bool) {
	rep, err := r.up.do(req.Context(), method, path, query, body)
	if err != nil {
		logRelayError(route, err)
		refuse(w, route, http.StatusBadGateway, codeInternal, "conchd could not be reached")
		return reply{}, false
	}
	return rep, true
}

// askReport is ask for a report about a run. A session that went offline
// refuses a report until it is heartbeaten, and the runtime cannot heartbeat
// the relay's session, so a report refused with session_offline is followed by
// one heartbeat and one retry, and the retry's answer is the one passed back.
// There is no loop: if the heartbeat fails, the original refusal stands and the
// ticker remains the steady-state path. The code is read from the error body,
// never the message.
func (r *relay) askReport(w http.ResponseWriter, req *http.Request, route, method, path string, body []byte) (reply, bool) {
	rep, ok := r.ask(w, req, route, method, path, nil, body)
	if !ok || !isSessionOffline(rep) {
		return rep, ok
	}
	session, err := r.up.heartbeat(req.Context(), r.sessionID())
	if err != nil {
		// Typically too_many_sessions: the session cannot be revived, so the
		// retry would be refused again.
		logRelayError("heartbeat for a refused report", err)
		return rep, true
	}
	r.mu.Lock()
	r.session = session
	r.mu.Unlock()
	return r.ask(w, req, route, method, path, nil, body)
}

// isSessionOffline reports whether conchd refused a call because the session
// is offline.
func isSessionOffline(rep reply) bool {
	if rep.status != http.StatusConflict {
		return false
	}
	var e schema.Error
	return json.Unmarshal(rep.body, &e) == nil && e.Code == schema.ErrorCodeSessionOffline
}

// decodeRequest reads the body of a request as one JSON object of type T at
// most schema.MaxPostRequestBytes long, refuses an unknown field, and checks
// it with the type's own Validate. It returns the body encoded again from the
// type, which is what is forwarded: nothing the type does not name goes on.
// It answers the sender and reports false when the body is refused.
func decodeRequest[T interface{ Validate() error }](w http.ResponseWriter, req *http.Request, route string) (T, []byte, bool) {
	var dst T
	req.Body = http.MaxBytesReader(w, req.Body, schema.MaxPostRequestBytes)
	decoder := json.NewDecoder(req.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&dst)
	if err == nil {
		var extra any
		if err = decoder.Decode(&extra); errors.Is(err, io.EOF) {
			err = nil
		} else if err == nil {
			err = errors.New("request body must contain one JSON object")
		}
	}
	if err == nil {
		err = dst.Validate()
	}
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			refuse(w, route, http.StatusRequestEntityTooLarge, codeTooLarge, "request body exceeds the maximum size")
		} else {
			refuse(w, route, http.StatusBadRequest, codeInvalidRequest, err.Error())
		}
		var zero T
		return zero, nil, false
	}
	encoded, err := json.Marshal(dst)
	if err != nil {
		refuse(w, route, http.StatusInternalServerError, codeInternal, "internal server error")
		var zero T
		return zero, nil, false
	}
	return dst, encoded, true
}

// idParam reads a positive id from the path.
func idParam(req *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(req.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

func (r *relay) handleSession(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	session := r.session
	r.mu.Unlock()
	writeRelayJSON(w, http.StatusOK, session)
}

func (r *relay) handleStartRun(w http.ResponseWriter, req *http.Request) {
	const route = "POST /v1/runs"
	body, encoded, ok := decodeRequest[schema.CreateRunRequestV1](w, req, route)
	if !ok {
		return
	}
	// One channel per relay; the same refusal as a channel the sender is not
	// in, and conchd is not asked.
	if body.Channel != r.cfg.Channel {
		refuse(w, route, http.StatusNotFound, codeChannelNotFound, "channel not found")
		return
	}
	rep, ok := r.askReport(w, req, route, http.MethodPost, fmt.Sprintf("/v1/sessions/%d/runs", r.sessionID()), encoded)
	if !ok {
		return
	}
	if rep.status >= 200 && rep.status <= 299 {
		var run schema.RunV1
		if err := json.Unmarshal(rep.body, &run); err != nil || run.ID <= 0 {
			// The run exists on conchd and the relay cannot name it, so the
			// sender could never report on it. Say so rather than hide it.
			refuse(w, route, http.StatusBadGateway, codeInternal, "conchd answered a run the relay could not read")
			return
		}
		r.mu.Lock()
		r.runs[run.ID] = struct{}{}
		r.mu.Unlock()
	}
	pass(w, rep)
}

// ownRun reads the run id of the path and refuses one this relay did not
// create. Two relays on one credential could otherwise report on each other's
// runs, which conchd cannot tell apart: it checks the credential, not the
// session.
func (r *relay) ownRun(w http.ResponseWriter, req *http.Request, route string) (int64, bool) {
	id, ok := idParam(req)
	if ok {
		r.mu.Lock()
		_, ok = r.runs[id]
		r.mu.Unlock()
	}
	if !ok {
		refuse(w, route, http.StatusNotFound, schema.ErrorCodeRunNotFound, "run not found")
		return 0, false
	}
	return id, true
}

func (r *relay) handleRunState(w http.ResponseWriter, req *http.Request) {
	const route = "POST /v1/runs/{id}/state"
	id, ok := r.ownRun(w, req, route)
	if !ok {
		return
	}
	_, encoded, ok := decodeRequest[schema.SetRunStateRequestV1](w, req, route)
	if !ok {
		return
	}
	if rep, ok := r.askReport(w, req, route, http.MethodPost, fmt.Sprintf("/v1/runs/%d/state", id), encoded); ok {
		pass(w, rep)
	}
}

func (r *relay) handleActivity(w http.ResponseWriter, req *http.Request) {
	const route = "POST /v1/runs/{id}/activity"
	id, ok := r.ownRun(w, req, route)
	if !ok {
		return
	}
	_, encoded, ok := decodeRequest[schema.AppendActivityRequestV1](w, req, route)
	if !ok {
		return
	}
	if rep, ok := r.askReport(w, req, route, http.MethodPost, fmt.Sprintf("/v1/runs/%d/activity", id), encoded); ok {
		pass(w, rep)
	}
}

func (r *relay) handleInterventions(w http.ResponseWriter, req *http.Request) {
	const route = "GET /v1/interventions"
	// Only the three parameters of the route go on; conchd checks their values.
	query := url.Values{}
	for _, key := range []string{"state", "after", "limit"} {
		if values, ok := req.URL.Query()[key]; ok {
			query[key] = values[:1]
		}
	}
	rep, ok := r.ask(w, req, route, http.MethodGet, fmt.Sprintf("/v1/sessions/%d/interventions", r.sessionID()), query, nil)
	if !ok {
		return
	}
	if rep.status >= 200 && rep.status <= 299 {
		var list schema.ListInterventionsResponseV1
		if err := json.Unmarshal(rep.body, &list); err != nil {
			refuse(w, route, http.StatusBadGateway, codeInternal, "conchd answered a list the relay could not read")
			return
		}
		r.watch(list.Interventions, true)
	}
	pass(w, rep)
}

func (r *relay) handleAck(w http.ResponseWriter, req *http.Request) {
	const route = "POST /v1/interventions/{id}/ack"
	id, ok := idParam(req)
	if ok {
		r.mu.Lock()
		_, ok = r.returned[id]
		r.mu.Unlock()
	}
	if !ok {
		refuse(w, route, http.StatusNotFound, schema.ErrorCodeInterventionNotFound, "intervention not found")
		return
	}
	_, encoded, ok := decodeRequest[schema.AckInterventionRequestV1](w, req, route)
	if !ok {
		return
	}
	rep, ok := r.ask(w, req, route, http.MethodPost, fmt.Sprintf("/v1/interventions/%d/ack", id), nil, encoded)
	if !ok {
		return
	}
	// Answered, or already answered, or gone: the relay has nothing to wait
	// for. Any other refusal leaves the clock running.
	if rep.status/100 == 2 || rep.status == http.StatusConflict || rep.status == http.StatusNotFound {
		r.forget(id)
	}
	pass(w, rep)
}

// --------------------------------------------------------- the socket file

// listenRelay listens on the Unix socket at path, mode 0600. The directory
// must already be private (checkSocketPath); a stale socket in the way is
// removed, and a live one is not.
func listenRelay(ctx context.Context, path string) (net.Listener, error) {
	if err := checkSocketPath(path); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		conn, err := (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext(ctx, "unix", path)
		if err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s is the socket of a process that is running", path)
		}
		// Only a refusal means nobody listens. A timeout or anything else may
		// be a busy relay, which must keep its socket.
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%s may be the socket of a process that is running: %w", path, err)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove the stale socket %s: %w", path, err)
		}
	}
	var ln net.Listener
	var err error
	privateUmask(func() { ln, err = (&net.ListenConfig{}).Listen(ctx, "unix", path) })
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// Born 0600 under the umask; this makes it certain.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("set the mode of %s: %w", path, err)
	}
	return ln, nil
}

// checkSocketPath refuses a socket path whose directory is not the relay
// user's own, mode 0700, or that is taken by something that is not a socket.
// It changes nothing.
func checkSocketPath(path string) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("the socket directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("the socket directory %s is not a directory", dir)
	}
	if !ownedByUs(info) {
		return fmt.Errorf("the socket directory %s is not owned by the user running the relay", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("the socket directory %s has mode %04o, want 0700", dir, info.Mode().Perm())
	}
	switch existing, err := os.Lstat(path); {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("the socket path: %w", err)
	case existing.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("%s exists and is not a socket; the relay will not replace it", path)
	}
	return nil
}
