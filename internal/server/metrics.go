package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Metrics (issue #562). A small, fixed set of counts in the Prometheus text
// format, so an operator can see how the server holds up while people use it:
// how many are connected, in voice or sharing, how often sockets drop, how
// requests fare, and the Go runtime and process figures (heap, goroutines,
// CPU, open files) that size the machine.
//
// Exposure is the point to get right. Counts of people, rooms and shares are
// activity data, so:
//
//   - metrics are off unless Config.MetricsListen is set;
//   - they are served on that second listener only: /metrics is not in the
//     route table and never answers on the main listener;
//   - the address must be loopback unless Config.MetricsAllowRemote says
//     otherwise, and the bound address is checked again after the bind;
//   - the listener has no authentication of its own: loopback is the
//     boundary, as with LiveKit's own prometheus_port.
//
// Every label value comes from a fixed set in this file, or is a pattern of
// the route table: never a path, a name or anything else a client chose, so no
// request can add a series. A path that matches no route counts under
// metricsRouteUnmatched. TestMetricsLabelValues holds this.
//
// The registry is the server's own, not the client library's global one, so
// two servers in one process (the tests) never share a count.

// metricsRouteUnmatched is the route label of a request no route matched.
const metricsRouteUnmatched = "unmatched"

// metricsStoreTimeout bounds the one store read a scrape makes (who is
// online). A scrape that cannot read it leaves that gauge out and serves the
// rest.
const metricsStoreTimeout = 2 * time.Second

// metricsMaxScrapes is how many scrapes are served at once; more answer 503.
// A Prometheus server scrapes one at a time.
const metricsMaxScrapes = 2

// metricsScrapeTimeout bounds a whole scrape; one that takes longer answers
// 503. It is longer than metricsStoreTimeout, so a slow store read costs only
// its own gauge.
const metricsScrapeTimeout = 5 * time.Second

// The fixed label sets. Each is the whole of what its label may say.
var (
	// metricsCodeClasses are the status classes of conch_http_requests_total.
	// An upgraded socket is 1xx; a status outside 100–599 is "other".
	metricsCodeClasses = []string{"1xx", "2xx", "3xx", "4xx", "5xx", "other"}
	// metricsSocketKinds are the kinds of socket: the message hub's
	// (/v0/ws, /v1/ws, /v2/ws), the events socket's, and voice presence's and
	// occupancy's.
	metricsSocketKinds = []string{"hub", "events", "voice", "other"}
	// metricsPrincipalKinds are the kinds of principal.
	metricsPrincipalKinds = []string{string(store.PrincipalHuman), string(store.PrincipalAgent)}
	// metricsVoiceTokenResults: tokens issued, or the request refused.
	metricsVoiceTokenResults = []string{"ok", "refused"}
	// metricsAuthFailureReasons are why authentication refused a request:
	// no credential at all, a bearer that is not a live credential, or a
	// socket ticket that could not be used.
	metricsAuthFailureReasons = []string{"missing_credential", "invalid_credential", "invalid_ticket"}
)

// metricsSocketKind is the kind label of a socket route's pattern.
func metricsSocketKind(pattern string) string {
	switch pattern {
	case "GET /v0/ws", "GET /v1/ws", "GET /v2/ws":
		return "hub"
	case "GET /v1/events/ws":
		return "events"
	case "GET /v1/voice/ws", "GET /v1/voice/occupancy/ws":
		return "voice"
	}
	return "other"
}

// metricsCodeClass is the code label of an HTTP status.
func metricsCodeClass(status int) string {
	if status < 100 || status > 599 {
		return "other"
	}
	return metricsCodeClasses[status/100-1]
}

// metricsPrincipalKind is the kind label of a principal kind, and false for a
// kind outside the fixed set (there is none today), which is then not counted.
func metricsPrincipalKind(k store.PrincipalKind) (string, bool) {
	switch k {
	case store.PrincipalHuman, store.PrincipalAgent:
		return string(k), true
	}
	return "", false
}

// metricsDisconnectReason is the reason label of a socket close. The reasons
// are schema.SocketCloseReason's fixed set; anything else is "error", as the
// connection log says for a close that named none.
func metricsDisconnectReason(r schema.SocketCloseReason) string {
	if !r.Valid() {
		return string(schema.SocketCloseError)
	}
	return string(r)
}

// metricsDisconnectReasons is every reason label, for the label-set test and
// so that each series exists at zero from the start.
func metricsDisconnectReasons() []string {
	return []string{
		string(schema.SocketCloseClient), string(schema.SocketCloseServerShutdown),
		string(schema.SocketCloseKeepaliveTimeout), string(schema.SocketCloseRevoked),
		string(schema.SocketCloseRemoved), string(schema.SocketCloseSlowConsumer),
		string(schema.SocketCloseSessionEnded), string(schema.SocketCloseError),
	}
}

// serverMetrics is the registry and the metrics the server counts itself.
// The gauges of state the server already keeps (who is online, voice) are not
// here: stateCollector reads them at scrape time.
type serverMetrics struct {
	reg            *prometheus.Registry
	httpRequests   *prometheus.CounterVec
	httpDuration   *prometheus.HistogramVec
	wsConnections  *prometheus.GaugeVec
	wsDisconnects  *prometheus.CounterVec
	voiceTokens    *prometheus.CounterVec
	messagesPosted *prometheus.CounterVec
	authFailures   *prometheus.CounterVec
	problemReports prometheus.Counter
}

// newServerMetrics builds s's registry: its own metrics, the state collector,
// the build info, and the client library's Go and process collectors.
func newServerMetrics(s *Server) *serverMetrics {
	m := &serverMetrics{
		reg: prometheus.NewRegistry(),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conch_http_requests_total",
			Help: "HTTP requests answered, by route pattern and status class.",
		}, []string{"route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "conch_http_request_duration_seconds",
			Help:    "Time to answer an HTTP request, by route pattern. Upgraded sockets are not timed.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route"}),
		wsConnections: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "conch_ws_connections",
			Help: "Open WebSockets, by kind.",
		}, []string{"kind"}),
		wsDisconnects: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conch_ws_disconnects_total",
			Help: "WebSockets closed, by why.",
		}, []string{"reason"}),
		voiceTokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conch_voice_tokens_issued_total",
			Help: "Voice session requests, by whether tokens were issued.",
		}, []string{"result"}),
		messagesPosted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conch_messages_posted_total",
			Help: "Messages stored, by the kind of their author.",
		}, []string{"kind"}),
		authFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "conch_auth_failures_total",
			Help: "Requests refused by authentication, by why.",
		}, []string{"reason"}),
		problemReports: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "conch_problem_reports_total",
			Help: "Problem reports received.",
		}),
	}
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "conch_build_info",
		Help:        "Always 1; the version label is the build's.",
		ConstLabels: prometheus.Labels{"version": s.cfg.Version},
	})
	buildInfo.Set(1)
	m.reg.MustRegister(
		m.httpRequests, m.httpDuration, m.wsConnections, m.wsDisconnects,
		m.voiceTokens, m.messagesPosted, m.authFailures, m.problemReports,
		buildInfo, newStateCollector(s),
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	// Every fixed series exists from the start, at zero, so a dashboard and a
	// rate() see the first event rather than a series appearing from nothing.
	for _, k := range metricsSocketKinds {
		m.wsConnections.WithLabelValues(k)
	}
	for _, r := range metricsDisconnectReasons() {
		m.wsDisconnects.WithLabelValues(r)
	}
	for _, r := range metricsVoiceTokenResults {
		m.voiceTokens.WithLabelValues(r)
	}
	for _, k := range metricsPrincipalKinds {
		m.messagesPosted.WithLabelValues(k)
	}
	for _, r := range metricsAuthFailureReasons {
		m.authFailures.WithLabelValues(r)
	}
	return m
}

// observeRequest counts one answered request. route must already be a
// pattern of the route table or metricsRouteUnmatched (Server.metricsRoute).
// An upgraded socket is counted but not timed: its duration is the life of
// the connection, which would swamp every bucket.
func (m *serverMetrics) observeRequest(route string, status int, took time.Duration) {
	m.httpRequests.WithLabelValues(route, metricsCodeClass(status)).Inc()
	if status != http.StatusSwitchingProtocols {
		m.httpDuration.WithLabelValues(route).Observe(took.Seconds())
	}
}

func (m *serverMetrics) socketOpened(pattern string) {
	m.wsConnections.WithLabelValues(metricsSocketKind(pattern)).Inc()
}

func (m *serverMetrics) socketClosed(pattern string, reason schema.SocketCloseReason) {
	m.wsConnections.WithLabelValues(metricsSocketKind(pattern)).Dec()
	m.wsDisconnects.WithLabelValues(metricsDisconnectReason(reason)).Inc()
}

func (m *serverMetrics) voiceSession(issued bool) {
	result := "refused"
	if issued {
		result = "ok"
	}
	m.voiceTokens.WithLabelValues(result).Inc()
}

func (m *serverMetrics) messagePosted(kind store.PrincipalKind) {
	if k, ok := metricsPrincipalKind(kind); ok {
		m.messagesPosted.WithLabelValues(k).Inc()
	}
}

// authFailed counts a refusal by authentication; reason is one of
// metricsAuthFailureReasons.
func (m *serverMetrics) authFailed(reason string) {
	m.authFailures.WithLabelValues(reason).Inc()
}

func (m *serverMetrics) problemReported() { m.problemReports.Inc() }

// metricsRoute is the route label of a request: the pattern the route wrapper
// recorded or, for a request refused before it reached a route (by
// authentication, say), the pattern the mux would match. Anything that is not
// a pattern of the route table, which includes every path no route matches,
// is metricsRouteUnmatched. The path itself is never the answer.
func (s *Server) metricsRoute(r *http.Request, traced string) string {
	pattern := traced
	if pattern == "" {
		_, pattern = s.mux.Handler(r)
	}
	if _, ok := s.routeByPattern[pattern]; ok {
		return pattern
	}
	return metricsRouteUnmatched
}

// stateCollector reports, at scrape time, gauges of state the server already
// keeps, rather than keeping a second count that could drift from it: who is
// online (the sessions table) and voice (the presence poller's rooms).
type stateCollector struct {
	s                                   *Server
	online, rooms, participants, shares *prometheus.Desc
}

func newStateCollector(s *Server) *stateCollector {
	return &stateCollector{
		s: s,
		online: prometheus.NewDesc("conch_principals_online",
			"Principals with a session that is online, by kind.", []string{"kind"}, nil),
		rooms: prometheus.NewDesc("conch_voice_rooms_active",
			"Voice rooms with somebody in them, as presence shows them.", nil, nil),
		participants: prometheus.NewDesc("conch_voice_participants",
			"Connections in voice rooms, as presence shows them.", nil, nil),
		shares: prometheus.NewDesc("conch_voice_shares_active",
			"Screen shares under way, as presence shows them.", nil, nil),
	}
}

func (c *stateCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.online
	ch <- c.rooms
	ch <- c.participants
	ch <- c.shares
}

func (c *stateCollector) Collect(ch chan<- prometheus.Metric) {
	rooms, participants, shares := c.s.voice.metricCounts()
	ch <- prometheus.MustNewConstMetric(c.rooms, prometheus.GaugeValue, float64(rooms))
	ch <- prometheus.MustNewConstMetric(c.participants, prometheus.GaugeValue, float64(participants))
	ch <- prometheus.MustNewConstMetric(c.shares, prometheus.GaugeValue, float64(shares))

	ctx, cancel := context.WithTimeout(context.Background(), metricsStoreTimeout)
	defer cancel()
	online, err := c.s.store.OnlinePrincipals(ctx)
	if err != nil {
		// Left out of this scrape, which a graph shows as a gap: an honest
		// "unknown", where a zero would say nobody is online.
		slog.Warn("metrics: count online principals failed", "error", err)
		return
	}
	for _, k := range metricsPrincipalKinds {
		ch <- prometheus.MustNewConstMetric(c.online, prometheus.GaugeValue, float64(online[store.PrincipalKind(k)]), k)
	}
}

// ValidateMetricsListen checks the metrics listener's address before anything
// is bound: host:port, and unless allowRemote a loopback host (a loopback IP,
// or localhost). An empty host, which binds every interface, is not loopback.
// A host name other than localhost is refused too, since it may resolve to
// anything; the address actually bound is checked again (Server.Listen).
func ValidateMetricsListen(addr string, allowRemote bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("metrics address %q: %w", addr, err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("metrics address %q: the port is not a number from 0 to 65535", addr)
	}
	if allowRemote || host == "localhost" {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("metrics address %q is not a loopback address; metrics count people, rooms and shares, so they are served on loopback only unless --metrics-allow-remote is given", addr)
}

// listenMetrics binds the metrics listener when one is configured. It refuses
// an address whose bound IP is not loopback unless remote access was allowed,
// and says in the log when it was.
func (s *Server) listenMetrics() (net.Listener, error) {
	addr := s.cfg.MetricsListen
	if err := ValidateMetricsListen(addr, s.cfg.MetricsAllowRemote); err != nil {
		return nil, fmt.Errorf("server: %w", err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("server: metrics listen %s: %w", addr, err)
	}
	bound, _ := ln.Addr().(*net.TCPAddr)
	loopback := bound != nil && bound.IP.IsLoopback()
	switch {
	case !loopback && !s.cfg.MetricsAllowRemote:
		// localhost that does not resolve to loopback, say.
		_ = ln.Close()
		return nil, fmt.Errorf("server: metrics address %q bound %s, which is not loopback; give --metrics-allow-remote to serve metrics there", addr, ln.Addr())
	case !loopback:
		slog.Warn("metrics: serving /metrics on an address that is not loopback, without authentication: anyone who can reach it can read how many people are online, in voice and sharing",
			"addr", ln.Addr().String())
	default:
		slog.Info("metrics: serving /metrics", "addr", ln.Addr().String())
	}
	return ln, nil
}

// metricsHandler serves GET /metrics and nothing else. A collector that fails
// costs only its own series: the rest of the scrape is still served. Scrapes
// are few at a time and bounded in time, since each one reads the store and
// takes presence's lock: a scrape flood answers 503 rather than taking store
// connections and CPU from the main listener.
func (s *Server) metricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.reg, promhttp.HandlerOpts{
		ErrorLog:            metricsErrorLog{},
		ErrorHandling:       promhttp.ContinueOnError,
		MaxRequestsInFlight: metricsMaxScrapes,
		Timeout:             metricsScrapeTimeout,
	}))
	return mux
}

// serveMetrics serves the metrics listener until ctx ends. Metrics are
// optional: a failure is logged and the server goes on serving without them.
// It returns a function that stops the listener and waits until it has.
func (s *Server) serveMetrics(ctx context.Context) (stop func()) {
	if s.metricsLn == nil {
		return func() {}
	}
	// One small response per request, so tight timeouts are safe: an idle or
	// slow-reading client cannot hold a connection open for long.
	srv := &http.Server{
		Handler:           s.metricsHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(s.metricsLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("metrics: listener stopped; the server goes on without metrics", "error", err)
		}
	}()
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		<-done
	}
}

// metricsErrorLog sends the metrics handler's errors to the log.
type metricsErrorLog struct{}

func (metricsErrorLog) Println(v ...any) {
	slog.Warn("metrics: " + fmt.Sprint(v...))
}
