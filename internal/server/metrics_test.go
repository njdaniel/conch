package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of the metrics (issue #562): the listener and where it may be, that
// /metrics is never on the main listener, each count moving on its event, the
// gauges returning to zero, and that no label value ever comes from a client.

// sample is one exported series: its labels and its value (a histogram's is
// its count).
type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// gatherMetrics reads srv's registry as a scrape would.
func gatherMetrics(t *testing.T, srv *Server) []sample {
	t.Helper()
	families, err := srv.metrics.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var out []sample
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			s := sample{name: mf.GetName(), labels: map[string]string{}}
			for _, l := range m.GetLabel() {
				s.labels[l.GetName()] = l.GetValue()
			}
			switch {
			case m.Counter != nil:
				s.value = m.GetCounter().GetValue()
			case m.Gauge != nil:
				s.value = m.GetGauge().GetValue()
			case m.Histogram != nil:
				s.value = float64(m.GetHistogram().GetSampleCount())
			case m.Untyped != nil:
				s.value = m.GetUntyped().GetValue()
			}
			out = append(out, s)
		}
	}
	return out
}

// metricValue is the value of name with exactly labels (given as name, value
// pairs), or 0 when there is no such series.
func metricValue(t *testing.T, srv *Server, name string, labels ...string) float64 {
	t.Helper()
	want := map[string]string{}
	for i := 0; i+1 < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}
	for _, s := range gatherMetrics(t, srv) {
		if s.name != name || len(s.labels) != len(want) {
			continue
		}
		match := true
		for k, v := range want {
			if s.labels[k] != v {
				match = false
			}
		}
		if match {
			return s.value
		}
	}
	return 0
}

func TestValidateMetricsListen(t *testing.T) {
	tests := []struct {
		addr        string
		allowRemote bool
		wantErr     string
	}{
		{addr: "127.0.0.1:9464"},
		{addr: "127.0.0.2:9464"},
		{addr: "[::1]:9464"},
		{addr: "localhost:9464"},
		{addr: "127.0.0.1:0"},
		{addr: ":9464", wantErr: "not a loopback address"},
		{addr: "0.0.0.0:9464", wantErr: "not a loopback address"},
		{addr: "[::]:9464", wantErr: "not a loopback address"},
		{addr: "192.0.2.10:9464", wantErr: "not a loopback address"},
		{addr: "metrics.example:9464", wantErr: "not a loopback address"},
		{addr: "0.0.0.0:9464", allowRemote: true},
		{addr: ":9464", allowRemote: true},
		{addr: "127.0.0.1", wantErr: "missing port"},
		{addr: "127.0.0.1:http", wantErr: "port is not a number"},
		{addr: "127.0.0.1:70000", wantErr: "port is not a number"},
		{addr: "", wantErr: "missing port"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s remote=%v", tt.addr, tt.allowRemote), func(t *testing.T) {
			err := ValidateMetricsListen(tt.addr, tt.allowRemote)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("error = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// serveTestServer runs srv until the test ends and returns once it answers.
func serveTestServer(t *testing.T, srv *Server) {
	t.Helper()
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	// The listener is bound, so this waits in its backlog until Serve has
	// started answering.
	if code, _, _ := httpGet(t, "http://"+srv.Addr()+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
}

func httpGet(t *testing.T, url string) (int, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), string(body)
}

// TestMetricsListener: off by default; on a loopback address /metrics serves
// the Prometheus text format with every metric of the issue and the Go and
// process collectors, and nothing else; a non-loopback address stops the
// server at start, with nothing bound, unless remote access is allowed, which
// is logged as a warning.
func TestMetricsListener(t *testing.T) {
	t.Run("off by default", func(t *testing.T) {
		srv := newTestServer(t)
		serveTestServer(t, srv)
		if addr := srv.MetricsAddr(); addr != "" {
			t.Fatalf("a metrics listener is bound at %s with nothing configured", addr)
		}
	})

	t.Run("loopback", func(t *testing.T) {
		for _, addr := range []string{"127.0.0.1:0", "localhost:0"} {
			t.Run(addr, func(t *testing.T) {
				srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, MetricsListen: addr})
				serveTestServer(t, srv)
				maddr := srv.MetricsAddr()
				if maddr == "" || maddr == srv.Addr() {
					t.Fatalf("metrics address %q, main %q: want a listener of its own", maddr, srv.Addr())
				}
				// serveTestServer made one request to the main listener, so
				// the per-route series exist.
				code, ctype, body := httpGet(t, "http://"+maddr+"/metrics")
				if code != http.StatusOK || !strings.HasPrefix(ctype, "text/plain") {
					t.Fatalf("GET /metrics = %d %q", code, ctype)
				}
				for _, name := range []string{
					"conch_http_requests_total", "conch_http_request_duration_seconds",
					"conch_ws_connections", "conch_ws_disconnects_total", "conch_principals_online",
					"conch_voice_rooms_active", "conch_voice_participants", "conch_voice_shares_active",
					"conch_voice_tokens_issued_total", "conch_messages_posted_total",
					"conch_auth_failures_total", "conch_problem_reports_total", "conch_build_info",
					"go_goroutines", "go_memstats_heap_alloc_bytes", "process_cpu_seconds_total", "process_open_fds",
				} {
					if !strings.Contains(body, "\n# TYPE "+name+" ") {
						t.Errorf("/metrics has no %s", name)
					}
				}
				if !strings.Contains(body, `conch_build_info{version="v1.2.3-test"} 1`) {
					t.Error("/metrics has no build info with the server's version")
				}
				if !strings.Contains(body, `conch_http_requests_total{code="2xx",route="GET /healthz"} 1`) {
					t.Errorf("/metrics does not count the healthz request:\n%s", body)
				}
				// The metrics listener serves /metrics and nothing else.
				for _, path := range []string{"/", "/healthz", "/v1/whoami", "/metrics/x"} {
					if code, _, _ := httpGet(t, "http://"+maddr+path); code != http.StatusNotFound {
						t.Errorf("metrics listener GET %s = %d, want 404", path, code)
					}
				}
			})
		}
	})

	t.Run("not loopback", func(t *testing.T) {
		for _, addr := range []string{"0.0.0.0:0", ":0", "[::]:0"} {
			t.Run(addr, func(t *testing.T) {
				srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, MetricsListen: addr})
				err := srv.Listen()
				if err == nil || !strings.Contains(err.Error(), "--metrics-allow-remote") {
					t.Fatalf("Listen = %v, want a refusal naming --metrics-allow-remote", err)
				}
				if srv.Addr() != "" || srv.MetricsAddr() != "" {
					t.Fatalf("a refused start left %q and %q bound", srv.Addr(), srv.MetricsAddr())
				}
				// Serve refuses the same way.
				if err := srv.Serve(context.Background()); err == nil || !strings.Contains(err.Error(), "not a loopback address") {
					t.Fatalf("Serve = %v, want the refusal", err)
				}
			})
		}
	})

	t.Run("not loopback, allowed", func(t *testing.T) {
		logs := captureLogsLocked(t)
		srv := newTestServerWithConfig(t, Config{AuthMode: AuthRequired, MetricsListen: "0.0.0.0:0", MetricsAllowRemote: true})
		serveTestServer(t, srv)
		_, port, err := net.SplitHostPort(srv.MetricsAddr())
		if err != nil {
			t.Fatal(err)
		}
		if code, _, _ := httpGet(t, "http://127.0.0.1:"+port+"/metrics"); code != http.StatusOK {
			t.Fatalf("GET /metrics = %d", code)
		}
		if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "not loopback") {
			t.Errorf("no warning logged for a metrics listener that is not loopback:\n%s", out)
		}
	})
}

// TestMetricsNotOnMainListener: /metrics is no route of the main listener in
// any configuration: 404 with authentication off, and with it required for a
// caller who holds a credential, metrics listener configured or not.
func TestMetricsNotOnMainListener(t *testing.T) {
	for _, rt := range newTestServer(t).routes {
		if strings.Contains(rt.pattern, "metrics") {
			t.Errorf("route table has %q", rt.pattern)
		}
	}
	for _, metricsListen := range []string{"", "127.0.0.1:0"} {
		for _, mode := range []AuthMode{AuthOff, AuthRequired} {
			t.Run(fmt.Sprintf("auth %s, metrics %q", mode, metricsListen), func(t *testing.T) {
				f := newAuthFixtureWithConfig(t, Config{AuthMode: mode, MetricsListen: metricsListen})
				serveTestServer(t, f.srv)
				for _, path := range []string{"/metrics", "/metrics/", "/v1/metrics"} {
					if rec := f.do(t, http.MethodGet, path, f.rootTok, ""); rec.Code != http.StatusNotFound {
						t.Errorf("main handler GET %s = %d, want 404", path, rec.Code)
					}
				}
				if code, _, _ := httpGet(t, "http://"+f.srv.Addr()+"/metrics"); code == http.StatusOK {
					t.Error("the main listener served /metrics")
				}
			})
		}
	}
}

// TestMetricsCounts: each count moves by exactly its event.
func TestMetricsCounts(t *testing.T) {
	type row struct {
		name   string
		act    func(t *testing.T, f *authFixture)
		metric string
		labels []string
		want   float64
	}
	post := func(t *testing.T, f *authFixture, tok, path, body string) {
		t.Helper()
		if rec := f.do(t, http.MethodPost, path, tok, body); rec.Code != http.StatusCreated {
			t.Fatalf("POST %s = %d %s", path, rec.Code, rec.Body)
		}
	}
	tests := []row{
		{"a request", func(t *testing.T, f *authFixture) { f.do(t, http.MethodGet, "/healthz", "", "") },
			"conch_http_requests_total", []string{"route", "GET /healthz", "code", "2xx"}, 1},
		{"its duration", func(t *testing.T, f *authFixture) { f.do(t, http.MethodGet, "/healthz", "", "") },
			"conch_http_request_duration_seconds", []string{"route", "GET /healthz"}, 1},
		{"a refusal is counted under its route", func(t *testing.T, f *authFixture) { f.do(t, http.MethodGet, "/v1/whoami", "", "") },
			"conch_http_requests_total", []string{"route", "GET /v1/whoami", "code", "4xx"}, 1},
		{"a path no route matches", func(t *testing.T, f *authFixture) { f.do(t, http.MethodGet, "/nope", f.aliceTok, "") },
			"conch_http_requests_total", []string{"route", metricsRouteUnmatched, "code", "4xx"}, 1},
		{"a method no route matches", func(t *testing.T, f *authFixture) { f.do(t, http.MethodPatch, "/healthz", "", "") },
			"conch_http_requests_total", []string{"route", metricsRouteUnmatched, "code", "4xx"}, 1},
		{"no credential", func(t *testing.T, f *authFixture) { f.do(t, http.MethodGet, "/v1/whoami", "", "") },
			"conch_auth_failures_total", []string{"reason", "missing_credential"}, 1},
		{"a bearer that is no credential", func(t *testing.T, f *authFixture) { f.do(t, http.MethodGet, "/v1/whoami", "conch_nope", "") },
			"conch_auth_failures_total", []string{"reason", "invalid_credential"}, 1},
		{"a ticket that is no ticket", func(t *testing.T, f *authFixture) { f.do(t, http.MethodGet, "/v2/ws?ticket=nope", "", "") },
			"conch_auth_failures_total", []string{"reason", "invalid_ticket"}, 1},
		{"a person's post", func(t *testing.T, f *authFixture) {
			post(t, f, f.aliceTok, "/v1/channels/general/messages", `{"body":"hi"}`)
		}, "conch_messages_posted_total", []string{"kind", "human"}, 1},
		{"a person's v0 post", func(t *testing.T, f *authFixture) {
			post(t, f, f.aliceTok, "/v0/channels/general/messages", `{"body":"hi"}`)
		}, "conch_messages_posted_total", []string{"kind", "human"}, 1},
		{"an agent's post", func(t *testing.T, f *authFixture) {
			post(t, f, f.botTok, "/v1/channels/general/messages", `{"body":"hi"}`)
		}, "conch_messages_posted_total", []string{"kind", "agent"}, 1},
		{"a person's whisper", func(t *testing.T, f *authFixture) {
			post(t, f, f.aliceTok, "/v2/channels/general/messages", fmt.Sprintf(`{"body":"psst","audience":{"kind":"principals","principal_ids":[%d]}}`, f.root.ID))
		}, "conch_messages_posted_total", []string{"kind", "human"}, 1},
		{"an agent's whisper", func(t *testing.T, f *authFixture) {
			grantWhisper(t, f.srv, f.bot.ID, "general")
			post(t, f, f.botTok, "/v2/channels/general/messages", fmt.Sprintf(`{"body":"psst","audience":{"kind":"principals","principal_ids":[%d]}}`, f.alice.ID))
		}, "conch_messages_posted_total", []string{"kind", "agent"}, 1},
		{"a post and a reply", func(t *testing.T, f *authFixture) {
			root := f.postAs(t, f.aliceTok, "general", "root")
			post(t, f, f.aliceTok, "/v2/channels/general/messages", fmt.Sprintf(`{"body":"re","thread_root_id":%d}`, root))
		}, "conch_messages_posted_total", []string{"kind", "human"}, 2},
		{"a refused post", func(t *testing.T, f *authFixture) {
			f.do(t, http.MethodPost, "/v1/channels/general/messages", f.aliceTok, `{"body":"  "}`)
		}, "conch_messages_posted_total", []string{"kind", "human"}, 0},
		{"a problem report", func(t *testing.T, f *authFixture) { post(t, f, f.aliceTok, "/v1/reports", reportBody(t)) },
			"conch_problem_reports_total", nil, 1},
		{"a refused problem report", func(t *testing.T, f *authFixture) { f.do(t, http.MethodPost, "/v1/reports", f.botTok, reportBody(t)) },
			"conch_problem_reports_total", nil, 0},
		{"a voice session with voice off", func(t *testing.T, f *authFixture) {
			f.do(t, http.MethodPost, "/v1/channels/general/voice/session", f.aliceTok, "")
		}, "conch_voice_tokens_issued_total", []string{"result", "refused"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newAuthFixture(t, AuthRequired)
			before := metricValue(t, f.srv, tt.metric, tt.labels...)
			tt.act(t, f)
			if got := metricValue(t, f.srv, tt.metric, tt.labels...) - before; got != tt.want {
				t.Errorf("%s%v moved by %v, want %v", tt.metric, tt.labels, got, tt.want)
			}
		})
	}
}

// TestMetricsMCPPost: an agent's post over MCP counts as an agent's.
func TestMetricsMCPPost(t *testing.T) {
	f := newMCPAuthFixture(t, Config{AuthMode: AuthRequired})
	_, tok := f.issue(t, f.agent.ID, nil)
	grantWhisper(t, f.srv, f.agent.ID, "general")
	if status, isError := f.callTool(t, tok, "post_message", map[string]any{"channel": "general", "body": "hello"}); status != http.StatusOK || isError {
		t.Fatalf("post_message = %d, error %v", status, isError)
	}
	whisper := map[string]any{"channel": "general", "body": "psst", "audience": map[string]any{"kind": "principals", "principal_ids": []int64{f.other.ID}}}
	if status, isError := f.callTool(t, tok, "post_message", whisper); status != http.StatusOK || isError {
		t.Fatalf("post_message with an audience = %d, error %v", status, isError)
	}
	if got := metricValue(t, f.srv, "conch_messages_posted_total", "kind", "agent"); got != 2 {
		t.Errorf("agent posts counted %v, want 2", got)
	}
	if got := metricValue(t, f.srv, "conch_messages_posted_total", "kind", "human"); got != 0 {
		t.Errorf("human posts counted %v, want 0", got)
	}
}

// TestMetricsAuthOffPost: with authentication off a post is counted by the
// kind of the author its body names.
func TestMetricsAuthOffPost(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	for _, post := range []struct{ path, body string }{
		{"/v1/channels/general/messages", fmt.Sprintf(`{"author_id":%d,"body":"hi"}`, f.bot.ID)},
		{"/v2/channels/general/messages", fmt.Sprintf(`{"author_id":%d,"body":"hi"}`, f.bot.ID)},
		{"/v1/channels/general/messages", fmt.Sprintf(`{"author_id":%d,"body":"hi"}`, f.alice.ID)},
	} {
		if rec := f.do(t, http.MethodPost, post.path, "", post.body); rec.Code != http.StatusCreated {
			t.Fatalf("post %s = %d %s", post.body, rec.Code, rec.Body)
		}
	}
	if got := metricValue(t, f.srv, "conch_messages_posted_total", "kind", "agent"); got != 2 {
		t.Errorf("agent posts counted %v, want 2", got)
	}
	if got := metricValue(t, f.srv, "conch_messages_posted_total", "kind", "human"); got != 1 {
		t.Errorf("human posts counted %v, want 1", got)
	}
}

// TestMetricsSockets: conch_ws_connections follows sockets of each kind as
// they open and close, whoever closes them, and is back at zero once they are
// all gone; each close is counted once, by its reason.
func TestMetricsSockets(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	base := wsTestServer(t, f.srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gauge := func(kind string) float64 { return metricValue(t, f.srv, "conch_ws_connections", "kind", kind) }
	closes := func(reason string) float64 {
		return metricValue(t, f.srv, "conch_ws_disconnects_total", "reason", reason)
	}
	waitFor := func(kind string, want float64) {
		t.Helper()
		waitUntil(t, fmt.Sprintf("%s sockets = %v", kind, want), func() bool { return gauge(kind) == want })
	}

	paths := []struct{ kind, path string }{
		{"hub", "/v0/ws?channel=general"},
		{"hub", "/v1/ws?channel=general"},
		{"hub", "/v2/ws?channel=general"},
		{"events", "/v1/events/ws?channel=general"},
		{"voice", "/v1/voice/ws?channel=general"},
		{"voice", "/v1/voice/occupancy/ws"},
	}
	// The client closes: root's sockets.
	var conns []*websocket.Conn
	for _, p := range paths {
		conns = append(conns, dialAs(t, ctx, base, p.path, f.rootTok))
	}
	waitFor("hub", 3)
	waitFor("events", 1)
	waitFor("voice", 2)
	for _, c := range conns {
		_ = c.CloseNow()
	}
	for _, kind := range metricsSocketKinds {
		waitFor(kind, 0)
	}
	if got := closes("client"); got != float64(len(paths)) {
		t.Errorf("client closes = %v, want %d", got, len(paths))
	}

	// The server closes: alice's sockets, as the operator revokes her
	// credential.
	for _, p := range paths {
		// Reading and dropping every frame, so the server's close handshake
		// is answered at once.
		c := dialAs(t, ctx, base, p.path, f.aliceTok)
		go func() {
			for {
				if _, _, err := c.Read(ctx); err != nil {
					return
				}
			}
		}()
	}
	waitFor("hub", 3)
	waitFor("events", 1)
	waitFor("voice", 2)
	_, credID, err := f.srv.store.ResolveCredentialDetail(ctx, f.aliceTok)
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, http.MethodDelete, fmt.Sprintf("/v1/credentials/%d", credID), f.rootTok, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", rec.Code, rec.Body)
	}
	for _, kind := range metricsSocketKinds {
		waitFor(kind, 0)
	}
	if got := closes("revoked"); got != float64(len(paths)) {
		t.Errorf("revoked closes = %v, want %d", got, len(paths))
	}
}

// TestMetricsSocketKinds: every socket route of the table has a kind of its
// own, so a new socket route is not counted as "other" by accident.
func TestMetricsSocketKinds(t *testing.T) {
	for _, rt := range newTestServer(t).routes {
		if strings.HasSuffix(rt.pattern, "/ws") {
			if kind := metricsSocketKind(rt.pattern); kind == "other" {
				t.Errorf("socket route %q has no kind", rt.pattern)
			}
		}
	}
}

// TestMetricsPrincipalsOnline: the gauge is read from the sessions: a
// session that starts counts its principal once, however many it has, by
// kind; one that goes stale or ends does not; nobody is left once all have
// ended.
func TestMetricsPrincipalsOnline(t *testing.T) {
	f := newSessionFixture(t)
	online := func(kind string) float64 { return metricValue(t, f.srv, "conch_principals_online", "kind", kind) }
	check := func(step string, human, agent float64) {
		t.Helper()
		if got := online("human"); got != human {
			t.Errorf("%s: humans online = %v, want %v", step, got, human)
		}
		if got := online("agent"); got != agent {
			t.Errorf("%s: agents online = %v, want %v", step, got, agent)
		}
	}
	check("nobody", 0, 0)
	a1 := f.create(t, f.aliceTok, humanBody)
	a2 := f.create(t, f.aliceTok2, humanBody)
	b := f.create(t, f.bobTok, humanBody)
	bot := f.create(t, f.botTok, agentBody)
	check("all connected", 2, 1)

	// Bob goes quiet past the stale threshold; the others keep beating.
	f.clock.Advance(DefaultSessionStale + time.Second)
	for _, s := range []struct {
		tok string
		id  int64
	}{{f.aliceTok, a1.ID}, {f.botTok, bot.ID}} {
		f.heartbeat(t, s.tok, s.id)
	}
	f.srv.sweepSessions(context.Background())
	check("bob stale", 1, 1)

	for _, s := range []struct {
		tok string
		id  int64
	}{{f.aliceTok, a1.ID}, {f.aliceTok2, a2.ID}, {f.bobTok, b.ID}, {f.botTok, bot.ID}} {
		if rec := f.do(t, http.MethodDelete, sessionPath(s.id, ""), s.tok, ""); rec.Code != http.StatusNoContent {
			t.Fatalf("end session %d = %d %s", s.id, rec.Code, rec.Body)
		}
	}
	check("all ended", 0, 0)
}

// TestMetricsVoice: the voice gauges follow presence through joins, a leave,
// a share's start and stop, and a rotation, and are zero once everyone has
// left; the session endpoint counts tokens issued.
func TestMetricsVoice(t *testing.T) {
	f, room := newShareFixture(t)
	check := func(step string, rooms, participants, shares float64) {
		t.Helper()
		for _, c := range []struct {
			name string
			want float64
		}{{"conch_voice_rooms_active", rooms}, {"conch_voice_participants", participants}, {"conch_voice_shares_active", shares}} {
			if got := metricValue(t, f.srv, c.name); got != c.want {
				t.Errorf("%s: %s = %v, want %v", step, c.name, got, c.want)
			}
		}
	}
	if got := metricValue(t, f.srv, "conch_voice_tokens_issued_total", "result", "ok"); got != 2 {
		t.Errorf("tokens issued = %v, want the fixture's 2", got)
	}
	check("ann and ann2 joined", 1, 2, 0)

	f.at(1_000)
	f.mustClaim(t, "ann")
	check("ann shares", 1, 2, 1)
	f.at(1_500)
	f.mustStop(t, "ann")
	check("ann stopped", 1, 2, 0)

	f.lk.setRoom(room, mustParticipant(t, f, room, "ann"))
	f.at(2_000)
	f.pass(t)
	check("ann2 left", 1, 1, 0)

	f.at(2_500)
	f.mustClaim(t, "ann")
	check("ann shares again", 1, 1, 1)
	// Removing ann rotates the room: its share and its people go with it.
	if code := f.doOrdered(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann"]), f.rootTok); code != http.StatusNoContent {
		t.Fatalf("remove ann = %d", code)
	}
	check("room rotated", 0, 0, 0)

	next := decodeSession(t, f.session(t, "ann2", "ops")).Rooms[0].Room
	if next == room {
		t.Fatal("the room was not rotated")
	}
	f.lk.setRoom(next, fakeParticipant{identity: f.identity("ann2"), joinedMs: 5_000})
	f.at(3_000)
	f.pass(t)
	check("ann2 in the new room", 1, 1, 0)
	if got := metricValue(t, f.srv, "conch_voice_tokens_issued_total", "result", "ok"); got != 3 {
		t.Errorf("tokens issued = %v, want 3", got)
	}

	f.lk.setRoom(next)
	f.at(3_500)
	f.pass(t)
	check("everyone left", 0, 0, 0)
}

// TestMetricsLabelValues is the cardinality rule: after hostile and unknown
// paths, in both authentication modes, every label of every conch_ series is
// a known label of its metric and every value is in that label's fixed set or
// is a pattern of the route table.
func TestMetricsLabelValues(t *testing.T) {
	long := "/v1/channels/" + strings.Repeat("a", 8000) + "/messages"
	hostile := []struct{ method, path string }{
		{http.MethodGet, "/v1/channels/%3Cscript%3Ealert(1)%3C%2Fscript%3E/messages"},
		{http.MethodPost, "/v1/channels/%3Cscript%3E/messages"},
		{http.MethodGet, long},
		{http.MethodGet, "/" + strings.Repeat("x/", 2000)},
		{http.MethodGet, "/no/such/route"},
		{http.MethodGet, "/v1/channels/general/messages/../../../../etc/passwd"},
		{http.MethodGet, "/metrics"},
		{http.MethodDelete, "/healthz"},
		{"BREW", "/v1/whoami"},
		{http.MethodGet, "/v1/hooks/secret-token-value"},
		{http.MethodPost, "/v1/hooks/secret-token-value"},
		{http.MethodGet, "/v2/ws?ticket=%3Cscript%3E"},
		{http.MethodGet, "/v1/principals/%3Cscript%3E/manifest"},
		{http.MethodGet, "/app/%3Cscript%3E"},
	}
	for _, mode := range []AuthMode{AuthOff, AuthRequired} {
		t.Run(string(mode), func(t *testing.T) {
			f := newAuthFixture(t, mode)
			for _, h := range hostile {
				for _, tok := range []string{"", f.aliceTok, "conch_<script>"} {
					f.do(t, h.method, h.path, tok, `{"body":"<script>"}`)
				}
			}
			if rec := f.do(t, http.MethodPost, "/v1/channels/general/messages", f.aliceTok, fmt.Sprintf(`{"author_id":%d,"body":"<script>"}`, f.alice.ID)); rec.Code != http.StatusCreated {
				t.Fatalf("post = %d %s", rec.Code, rec.Body)
			}

			routes := map[string]bool{metricsRouteUnmatched: true}
			for _, rt := range f.srv.routes {
				routes[rt.pattern] = true
			}
			set := func(values ...string) map[string]bool {
				m := map[string]bool{}
				for _, v := range values {
					m[v] = true
				}
				return m
			}
			allowed := map[string]map[string]map[string]bool{
				"conch_http_requests_total":           {"route": routes, "code": set(metricsCodeClasses...)},
				"conch_http_request_duration_seconds": {"route": routes},
				"conch_ws_connections":                {"kind": set(metricsSocketKinds...)},
				"conch_ws_disconnects_total":          {"reason": set(metricsDisconnectReasons()...)},
				"conch_principals_online":             {"kind": set(metricsPrincipalKinds...)},
				"conch_voice_rooms_active":            {},
				"conch_voice_participants":            {},
				"conch_voice_shares_active":           {},
				"conch_voice_tokens_issued_total":     {"result": set(metricsVoiceTokenResults...)},
				"conch_messages_posted_total":         {"kind": set(metricsPrincipalKinds...)},
				"conch_auth_failures_total":           {"reason": set(metricsAuthFailureReasons...)},
				"conch_problem_reports_total":         {},
				"conch_build_info":                    {"version": set("v1.2.3-test")},
			}
			seen := map[string]bool{}
			for _, s := range gatherMetrics(t, f.srv) {
				if !strings.HasPrefix(s.name, "conch_") {
					continue
				}
				seen[s.name] = true
				labels, ok := allowed[s.name]
				if !ok {
					t.Errorf("unexpected metric %s", s.name)
					continue
				}
				for k, v := range s.labels {
					if k == "le" && s.name == "conch_http_request_duration_seconds" {
						continue
					}
					values, ok := labels[k]
					if !ok {
						t.Errorf("%s has an unexpected label %s=%q", s.name, k, v)
						continue
					}
					if !values[v] {
						t.Errorf("%s{%s=%.80q} is not in the label's allowed set", s.name, k, v)
					}
				}
			}
			for name := range allowed {
				if !seen[name] {
					t.Errorf("metric %s was not exported", name)
				}
			}
			// The hostile paths all counted under the one fixed value.
			if got := metricValue(t, f.srv, "conch_http_requests_total", "route", metricsRouteUnmatched, "code", "4xx"); got == 0 {
				t.Error("no request was counted as unmatched")
			}
		})
	}
}

// TestMetricsCodeClass: every status maps into the fixed set.
func TestMetricsCodeClass(t *testing.T) {
	tests := []struct {
		status int
		want   string
	}{
		{101, "1xx"}, {200, "2xx"}, {204, "2xx"}, {301, "3xx"}, {404, "4xx"}, {429, "4xx"},
		{500, "5xx"}, {599, "5xx"}, {0, "other"}, {99, "other"}, {600, "other"}, {999, "other"},
	}
	for _, tt := range tests {
		if got := metricsCodeClass(tt.status); got != tt.want || !slices.Contains(metricsCodeClasses, got) {
			t.Errorf("metricsCodeClass(%d) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

// grantWhisper gives agentID a manifest that may whisper to people and agents
// in the named channel, besides reading and posting there.
func grantWhisper(t *testing.T, srv *Server, agentID int64, channel string) {
	t.Helper()
	ch, err := srv.store.ChannelByName(context.Background(), channel)
	if err != nil {
		t.Fatal(err)
	}
	req := schema.PutAgentManifestRequestV1{
		DisplayName: "test agent", Tier: schema.AgentTierC, Capabilities: schema.Capabilities(),
		Channels: []schema.ChannelGrant{{ChannelID: ch.ID, Permissions: []schema.ChannelPermission{
			schema.ChannelPermissionRead, schema.ChannelPermissionPost, schema.ChannelPermissionWhisper, schema.ChannelPermissionWhisperAgent,
		}}},
	}
	if _, _, err := srv.store.PutAgentManifest(context.Background(), "system", agentID, req); err != nil {
		t.Fatalf("PutAgentManifest: %v", err)
	}
}
