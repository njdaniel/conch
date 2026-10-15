package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestRealAdapterPlan(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/claude", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	for _, tc := range []struct {
		name     string
		flag     bool
		lookPath func(string) (string, error)
		wantRun  bool
	}{
		{"not asked for, claude there", false, found, false},
		{"not asked for, claude missing", false, missing, false},
		{"asked for, claude missing", true, missing, false},
		{"asked for, claude there", true, found, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, why := realAdapterPlan(tc.flag, tc.lookPath)
			if run != tc.wantRun {
				t.Errorf("run = %v, want %v", run, tc.wantRun)
			}
			if !run && why == "" {
				t.Error("a row that is not run says why")
			}
		})
	}
}

// TestRealAdapterPlanNeverLooksWithoutTheFlag: without --real-adapter the
// program is not even looked for, so nothing about the machine can start it.
func TestRealAdapterPlanNeverLooksWithoutTheFlag(t *testing.T) {
	looked := false
	run, _ := realAdapterPlan(false, func(string) (string, error) { looked = true; return "claude", nil })
	if run || looked {
		t.Errorf("run = %v, looked = %v, want neither", run, looked)
	}
}

func TestRetryWaves(t *testing.T) {
	base := time.Now()
	at := func(ms ...int) []time.Time {
		var out []time.Time
		for _, m := range ms {
			out = append(out, base.Add(time.Duration(m)*time.Millisecond))
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		times []time.Time
		want  []int // wave sizes
	}{
		{"none", nil, nil},
		{"one", at(0), []int{1}},
		{"one wave", at(0, 10, 30), []int{3}},
		{"doubling", at(0, 900, 905, 2900, 2910, 2920), []int{1, 2, 3}},
		{"a wave drifting past the gap is still one", at(0, 300, 600, 900), []int{4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []int
			for _, w := range retryWaves(tc.times) {
				got = append(got, len(w))
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("wave sizes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBoundedRetries(t *testing.T) {
	base := time.Now()
	at := func(ms ...int) []time.Time {
		var out []time.Time
		for _, m := range ms {
			out = append(out, base.Add(time.Duration(m)*time.Millisecond))
		}
		return out
	}
	many := []int{0}
	for i := range 20 {
		many = append(many, 1000+i)
	}
	for _, tc := range []struct {
		name     string
		attempts []time.Time
		down     time.Duration
		wantErr  bool
	}{
		{"doubling delays", at(0, 900, 2900), 4 * time.Second, false},
		{"no retry at all", at(0), 12 * time.Second, true},
		{"no retry at all, many connections in the first wave", at(0, 10, 20), 30 * time.Second, true},
		{"a window too short to see a second wave", at(0), 4 * time.Second, false},
		{"a window too short, nothing at all", nil, 2 * time.Second, false},
		{"a late second wave on a starved machine", at(0, 2500, 7500), 9 * time.Second, false},
		{"a retry every half second", at(0, 600, 1200, 1800, 2400, 3000, 3600, 4200, 4800, 5400, 6000), 6 * time.Second, true},
		{"delays that shrink", at(0, 2000, 3000, 3600), 6 * time.Second, true},
		{"a retry every second, flat", at(0, 1000, 2000), 3600 * time.Millisecond, true},
		{"a retry every two seconds, flat", at(0, 2000, 4000), 5 * time.Second, true},
		{"no retry, exactly at the grace", at(0), retryGrace, true},
		{"too many at once", at(many...), 4 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := boundedRetries(tc.attempts, tc.down); (err != nil) != tc.wantErr {
				t.Errorf("error = %v, want one: %v", err, tc.wantErr)
			}
		})
	}
}

func TestChannelsLoaded(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []string
		want bool
	}{
		{"still loading", []string{"╭────────╮╭──────╮", "│ loading… ││      │", "╰────────╯╰──────╯"}, false},
		{"nothing", nil, false},
		{"loaded", []string{"│› general ││ msg │", "│  nets-demo ││     │"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := channelsLoaded(tc.rows); got != tc.want {
				t.Errorf("channelsLoaded = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChannelList(t *testing.T) {
	rows := []string{
		"╭────────────────╮╭──────╮",
		"│  general       ││ msg  │",
		"│› nets-demo     ││      │",
		"│  test-private  ││      │",
		"│                ││      │",
		"╰────────────────╯╰──────╯",
		"online: nobody",
	}
	names, marked := channelList(rows, suiteChannels)
	if !slices.Equal(names, []string{"general", "nets-demo", "test-private"}) || marked != 1 {
		t.Errorf("channelList = %v, %d", names, marked)
	}
	if got := selectedChannel(rows); got != "nets-demo" {
		t.Errorf("selectedChannel = %q", got)
	}
	if got := selectedChannel(nil); got != "" {
		t.Errorf("selectedChannel of nothing = %q", got)
	}
}

func TestCountRows(t *testing.T) {
	rows := []string{"12:00 nick-test: hello", "12:00 worker-test [agent]: 2: hello", "online: nick-test"}
	for _, tc := range []struct {
		parts []string
		want  int
	}{
		{[]string{"hello"}, 2},
		{[]string{"nick-test", "hello"}, 1},
		{[]string{"worker-test [agent]", "hello"}, 1},
		{[]string{"absent"}, 0},
	} {
		if got := countRows(rows, tc.parts...); got != tc.want {
			t.Errorf("countRows(%v) = %d, want %d", tc.parts, got, tc.want)
		}
	}
}

func TestWithBody(t *testing.T) {
	msgs := []schema.MessageV2{{ID: 1, Body: "alpha beta"}, {ID: 2, Body: "gamma"}, {ID: 3, Body: "beta"}}
	var ids []int64
	for _, m := range withBody(msgs, "beta") {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []int64{1, 3}) {
		t.Errorf("ids = %v, want [1 3]", ids)
	}
}

func TestSessionsNamed(t *testing.T) {
	roster := []schema.SessionV1{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}, {ID: 3, Name: "a"}}
	if got := sessionsNamed(roster, "a"); len(got) != 2 || got[0].ID != 1 || got[1].ID != 3 {
		t.Errorf("sessionsNamed = %v", got)
	}
	if s, ok := sessionOf(roster, 2); !ok || s.Name != "b" {
		t.Errorf("sessionOf = %v, %v", s, ok)
	}
	if _, ok := sessionOf(roster, 9); ok {
		t.Error("sessionOf found a session that is not there")
	}
}

func TestUnlistedRoutes(t *testing.T) {
	routes := make([]routePattern, 0, 4)
	for _, text := range []string{"GET /healthz", "POST /v1/sessions/{id}/heartbeat", "GET /v2/ws", "/mcp"} {
		p, err := parseRoutePattern(text)
		if err != nil {
			t.Fatal(err)
		}
		routes = append(routes, p)
	}
	for _, tc := range []struct {
		call string
		ok   bool
	}{
		{"GET /healthz", true},
		{"POST /healthz", false},
		{"POST /v1/sessions/12/heartbeat", true},
		{"POST /v1/sessions/12/heartbeat/extra", false},
		{"POST /v1/sessions//heartbeat", false},
		{"GET /v2/ws", true},
		{"POST /mcp", true},
		{"GET /v1/test-only", false},
		{"GET /v1/sessions/12", false},
	} {
		t.Run(tc.call, func(t *testing.T) {
			bad := unlisted(routes, []string{tc.call})
			if (len(bad) == 0) != tc.ok {
				t.Errorf("unlisted = %v, want the call listed: %v", bad, tc.ok)
			}
		})
	}
}

// TestRoutesTableIsReadable: the routes the check reads out of routes.go are
// the ones the suite relies on, and there are enough of them to believe the
// pattern found the table.
func TestRoutesTableIsReadable(t *testing.T) {
	routes, err := publicRoutes(repoDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(routes) < 40 {
		t.Fatalf("read %d routes from routes.go, want the whole table", len(routes))
	}
	for _, call := range []string{"GET /healthz", "POST /v1/ws-tickets", "GET /v2/ws", "GET /v1/events/ws", "POST /v1/runs/7/interventions", "GET /v2/channels/general/messages/search", "DELETE /v1/sessions/3", "GET /v1/audit"} {
		if bad := unlisted(routes, []string{call}); len(bad) != 0 {
			t.Errorf("%s is a public route and the check does not find it", call)
		}
	}
}

func TestRealAdapterPlanFor(t *testing.T) {
	found := func(string) (string, error) { return "/usr/bin/x", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	for _, tc := range []struct {
		name     string
		flag     bool
		program  string
		model    string
		lookPath func(string) (string, error)
		wantRun  bool
		wantWhy  string
	}{
		{"codex, not asked for", false, "codex", "m", found, false, "real Codex command"},
		{"codex missing", true, "codex", "m", missing, false, "the codex program is not on PATH"},
		{"codex without a model", true, "codex", "", found, false, "CONCH_ADAPTER_MODEL"},
		{"codex there with a model", true, "codex", "m", found, true, ""},
		{"claude needs no model of its own", true, "claude", "sonnet", found, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, why := realAdapterPlanFor(tc.flag, tc.program, tc.model, tc.lookPath)
			if run != tc.wantRun || !strings.Contains(why, tc.wantWhy) {
				t.Errorf("run = %v, why = %q; want %v, %q", run, why, tc.wantRun, tc.wantWhy)
			}
		})
	}
}

func TestChannelListIgnoresUnreadCounts(t *testing.T) {
	rows := []string{
		"╭────────────────╮╭──────╮",
		"│› general 2     ││ msg  │",
		"│  nets-demo 999+││      │",
		"│  test-private  ││      │",
		"╰────────────────╯╰──────╯",
	}
	names, marked := channelList(rows, suiteChannels)
	if !slices.Equal(names, []string{"general", "nets-demo", "test-private"}) || marked != 0 {
		t.Errorf("channelList = %v, %d", names, marked)
	}
}

func TestChannelListKnowsNamesThatEndInANumber(t *testing.T) {
	rows := []string{
		"│› room 2        ││      │",
		"│  room 2 5      ││      │",
		"│  room          ││      │",
	}
	names, _ := channelList(rows, []string{"room 2", "room"})
	if !slices.Equal(names, []string{"room 2", "room 2", "room"}) {
		t.Errorf("channelList = %v", names)
	}
}

// The parser tells a count from a name by the known names, but the suite's own
// names must also never look like one.
func TestSuiteChannelNamesDoNotEndInACount(t *testing.T) {
	for _, name := range suiteChannels {
		if _, last, ok := strings.Cut(name, " "); ok && unreadCount(last) {
			t.Errorf("channel %q ends like an unread count", name)
		}
	}
}
