package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// reportServer is a conchd stand-in for the report commands. It records the
// requests it was sent, by "METHOD path", with their bodies.
type reportServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests map[string][]string
	report   schema.ProblemReportV1
}

func newReportServer(t *testing.T, report schema.ProblemReportV1) *reportServer {
	t.Helper()
	s := &reportServer{requests: map[string][]string{}, report: report}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		s.mu.Lock()
		s.requests[key] = append(s.requests[key], string(body))
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		ts := schema.NewTimestamp(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
		var out any
		status := http.StatusOK
		switch key {
		case "POST /v1/reports":
			status, out = http.StatusCreated, schema.CreateProblemReportResponseV1{ID: 7, CreatedAt: ts}
		case "GET /v1/reports":
			out = schema.ListProblemReportsResponseV1{Reports: []schema.ProblemReportSummaryV1{
				{ID: 8, State: schema.ProblemReportStateOpen, CreatedAt: ts, PrincipalID: 12, ClientName: "conch", ClientVersion: "dev-1", ServerVersion: "v1", Often: schema.ProblemReportOftenOnce},
				{ID: 7, State: schema.ProblemReportStateFiled, CreatedAt: ts, PrincipalID: 12, ClientName: "conch-web", ClientVersion: "v1", ServerVersion: "v1", Often: schema.ProblemReportOftenSometimes, Issue: "https://example.com/issues/1"},
			}}
		case "GET /v1/reports/7":
			out = s.report
		case "PUT /v1/reports/7":
			var req schema.UpdateProblemReportRequestV1
			_ = json.Unmarshal(body, &req)
			filed := s.report
			filed.State, filed.Issue = req.State, req.Issue
			out = filed
		case "GET /v1/channels":
			out = schema.ListChannelsResponse{Channels: []schema.ChannelV0{{ID: 1, Name: "kestrel-ops"}, {ID: 2, Name: "general"}}}
		case "GET /v1/channels/kestrel-ops/members":
			out = schema.ListChannelMembersResponseV1{Members: []schema.ChannelMemberV1{{PrincipalID: 12, Name: "Tess"}, {PrincipalID: 13, Name: "Rook Bishop"}}}
		case "GET /v1/channels/general/members":
			out = schema.ListChannelMembersResponseV1{Members: []schema.ChannelMemberV1{{PrincipalID: 12, Name: "Tess"}, {PrincipalID: 14}}}
		default:
			status, out = http.StatusNotFound, schema.Error{Code: "not_found", Message: "no"}
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *reportServer) sent(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[key]
}

func TestReportCommand(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
		// sent is whether a report reaches the server; want, a piece of the
		// output or the error.
		sent bool
		want string
	}{
		{"all flags and --yes", []string{"--did", "ran tail", "--expected", "messages", "--happened", "it stopped", "--often", "once", "--yes"}, "", true, "sent report 7"},
		{"all flags, confirmed", []string{"--did", "ran tail", "--expected", "messages", "--happened", "it stopped", "--often", "sometimes"}, "y\n", true, "Send this report? [y/N]"},
		{"asked for every answer", nil, "ran tail\nmessages\nit stopped\nevery time\nyes\n", true, "What happened instead: "},
		{"asked for one answer", []string{"--did", "ran tail", "--expected", "messages", "--often", "3"}, "it stopped\ny\n", true, "sent report 7"},
		{"declined", []string{"--did", "a", "--expected", "b", "--happened", "c", "--often", "once"}, "n\n", false, "not sent"},
		{"no confirmation at all", []string{"--did", "a", "--expected", "b", "--happened", "c", "--often", "once"}, "", false, "not sent"},
		{"no answer to a question", []string{"--did", "a", "--often", "once"}, "", false, "no answer to"},
		{"blank answer", []string{"--did", "a", "--expected", "b", "--often", "once"}, "\n", false, "happened is required"},
		{"unknown often", []string{"--did", "a", "--expected", "b", "--happened", "c", "--often", "daily", "--yes"}, "", false, "--often must be"},
		{"positional argument", []string{"oops"}, "", false, "takes no positional arguments"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			srv := newReportServer(t, schema.ProblemReportV1{})
			stdout, _, err := runCLI(t, tt.stdin, append([]string{"report", "--server", srv.URL}, tt.args...)...)
			out := stdout
			if err != nil {
				out += err.Error()
			}
			if !strings.Contains(out, tt.want) {
				t.Errorf("output %q, want it to contain %q", out, tt.want)
			}
			bodies := srv.sent("POST /v1/reports")
			if tt.sent != (len(bodies) == 1) || len(bodies) > 1 {
				t.Fatalf("%d reports sent, want sent=%v (err %v)", len(bodies), tt.sent, err)
			}
			if !tt.sent {
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			var req schema.CreateProblemReportRequestV1
			if err := json.Unmarshal([]byte(bodies[0]), &req); err != nil {
				t.Fatal(err)
			}
			if err := req.Validate(); err != nil {
				t.Errorf("sent an invalid report: %v", err)
			}
			if req.Did != "ran tail" || req.Expected != "messages" || req.Happened != "it stopped" {
				t.Errorf("answers = %+v", req)
			}
			want := schema.ProblemReportClientV1{Name: "conch", Version: "vtest", Platform: runtime.GOOS + "/" + runtime.GOARCH}
			if req.Client != want || req.Diagnostics == nil || len(req.Diagnostics) != 0 {
				t.Errorf("client = %+v, diagnostics %v", req.Client, req.Diagnostics)
			}
			// What was shown is what was sent, before it was sent.
			if !strings.Contains(stdout, `"happened": "it stopped"`) || !strings.Contains(stdout, `"platform": "`+want.Platform+`"`) {
				t.Errorf("the preview does not show the report:\n%s", stdout)
			}
		})
	}
}

func markdownReport() schema.ProblemReportV1 {
	at := schema.NewTimestamp(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	return schema.ProblemReportV1{
		ID: 7, State: schema.ProblemReportStateOpen, CreatedAt: at, PrincipalID: 12, ServerVersion: "v0.1.0-alpha.2",
		Voice:    schema.HealthVoiceUnavailable,
		Did:      "Joined voice in #kestrel-ops with Tess and rook bishop",
		Expected: "To hear Tess",
		Happened: "Nothing in kestrel-ops; general was fine. Tessellation is a word. In #raid-night, Tess's mic cut out.",
		Often:    schema.ProblemReportOftenSometimes,
		Client: schema.ProblemReportClientV1{
			Name: "conch-web", Version: "v0.1.0-alpha.2", Platform: "Linux x86_64", Screen: "1280x800", Voice: "reconnecting",
			UserAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0",
		},
		Diagnostics: []schema.DiagnosticEventV1{
			{At: at, Kind: schema.DiagnosticSocketClose, Socket: schema.DiagnosticSocketMessages, Code: 1006},
			{At: at, Kind: schema.DiagnosticHTTPFailure, Code: 503, Error: "voice_unavailable", RequestID: "3f9a0c1d2b4e5f60"},
		},
		Server: schema.ProblemReportServerV1{
			Requests:     []schema.ServerRequestTraceV1{{At: at, RequestID: "3f9a0c1d2b4e5f60", Route: "POST /v1/channels/{channel}/voice/session", Status: 503}},
			SocketCloses: []schema.ServerSocketCloseV1{{At: at, RequestID: "a1b2c3d4e5f60718", Route: "GET /v2/ws", Reason: schema.SocketCloseKeepaliveTimeout, DurationSeconds: 312, Frames: 48}},
		},
		// raid-night is a private group's channel: the operator is not in it,
		// and their own channel listing does not have it.
		Redact: &schema.ProblemReportRedactionV1{
			Channels:   []string{"general", "kestrel-ops", "raid-night"},
			Principals: []string{"Rook Bishop", "Tess", "root"},
		},
	}
}

// show --markdown is an issue body in the bug template's sections, in its
// order, with every name of the server's redact list taken out (a channel the
// operator is not in too), the answers fenced, the browser reduced to its
// family, and nothing that names the reporter.
func TestReportsShowMarkdown(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	srv := newReportServer(t, markdownReport())
	stdout, stderr, err := runCLI(t, "", "admin", "reports", "show", "--server", srv.URL, "--markdown", "7")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"### What you did\n\n```\nJoined voice in [channel] with [person] and [person]\n```\n",
		"### What you expected\n\n```\nTo hear [person]\n```\n",
		"### What happened instead\n\n```\nNothing in [channel]; [channel] was fine. Tessellation is a word. In [channel], [person]'s mic cut out.\n```\n",
		"### Conch version\n\nconch-web v0.1.0-alpha.2 (server v0.1.0-alpha.2)\n",
		"### Device, system and browser (or client)\n\nLinux x86_64; Firefox 131; screen 1280x800\n",
		"### How often does it happen?\n\nSometimes\n",
		"### Diagnostics (optional)\n\nReported from inside Conch (report 7, 2026-10-16T09:00:00Z).\nServer voice health: unavailable. Client voice: reconnecting; screen share: -.\n",
		"```\n2026-10-16T09:00:00.000Z socket_close messages code=1006\n2026-10-16T09:00:00.000Z http_failure code=503 error=voice_unavailable request_id=3f9a0c1d2b4e5f60\n```\n",
		"```\n2026-10-16T09:00:00.000Z 3f9a0c1d2b4e5f60 503 POST /v1/channels/{channel}/voice/session\n```\n",
		"```\n2026-10-16T09:00:00.000Z a1b2c3d4e5f60718 GET /v2/ws keepalive_timeout after 312s, 48 frames\n```\n",
	}
	at := 0
	for _, w := range want {
		i := strings.Index(stdout[at:], w)
		if i < 0 {
			t.Fatalf("the markdown lacks, in order, %q:\n%s", w, stdout)
		}
		at += i + len(w)
	}
	for _, name := range []string{"kestrel", "raid", "Tess ", "Tess'", "rook", "Rook", "general", "principal", "Gecko", "X11"} {
		if strings.Contains(stdout, name) {
			t.Errorf("the markdown holds %q:\n%s", name, stdout)
		}
	}
	if strings.Count(stdout, "```") != 12 {
		t.Errorf("the code blocks are not closed:\n%s", stdout)
	}
	if !strings.Contains(stderr, "took out 8 channel and person name(s), from 3 channel and 3 principal names on the server") {
		t.Errorf("stderr = %q", stderr)
	}
	// The names come from the report, not from the operator's own listing.
	if got := srv.sent("GET /v1/channels"); len(got) != 0 {
		t.Errorf("show listed the operator's channels %d times", len(got))
	}

	// Without --markdown, the operator also sees who sent it.
	stdout, _, err = runCLI(t, "", "admin", "reports", "show", "--server", srv.URL, "7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "report 7, open, from principal 12 at 2026-10-16T09:00:00Z\n\n### What you did") {
		t.Errorf("show = %q", stdout)
	}
}

// A server that sends no names to redact still gets its markdown written,
// and the operator is told the names are in it.
func TestReportsShowMarkdownWithoutNames(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	report := markdownReport()
	report.Redact = nil
	srv := newReportServer(t, report)
	stdout, stderr, err := runCLI(t, "", "admin", "reports", "show", "--server", srv.URL, "--markdown", "7")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "### What you did") || !strings.Contains(stderr, "read the report closely") {
		t.Errorf("stdout %q, stderr %q", stdout, stderr)
	}
}

// What a tester types cannot become markdown: a mention, an issue link, a
// fence of their own and HTML stay inside a fence longer than any backtick
// run in them.
func TestReportsMarkdownIsInert(t *testing.T) {
	r := markdownReport()
	r.Did = "@someone look at #12\n```\n<img src=x onerror=alert(1)>\n`````` six"
	r.Expected = "plain"
	r.Happened = "``inline`` and <b>bold</b>"
	body, _ := renderReportMarkdown(r)
	for _, want := range []string{
		"### What you did\n\n```````\n@someone look at #12\n```\n<img src=x onerror=alert(1)>\n`````` six\n```````\n\n### What you expected",
		"### What you expected\n\n```\nplain\n```\n",
		"### What happened instead\n\n```\n``inline`` and <b>bold</b>\n```\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the markdown lacks %q:\n%s", want, body)
		}
	}
	for _, s := range []string{"", "a", "```", "x````y", "`"} {
		block := fenced(s)
		fence := block[:strings.Index(block, "\n")]
		if len(fence) < 3 || strings.Contains(s, fence) || !strings.HasSuffix(block, "\n"+fence) {
			t.Errorf("fenced(%q) = %q", s, block)
		}
	}
}

func TestBrowserFamily(t *testing.T) {
	tests := []struct{ ua, want string }{
		{"Mozilla/5.0 (X11; Linux x86_64; rv:131.0) Gecko/20100101 Firefox/131.0", "Firefox 131"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.6723.58 Safari/537.36", "Chrome 130"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.2849.56", "Edge 130"},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15", "Safari 18"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/130.0.6723.90 Mobile/15E148 Safari/604.1", "Chrome 130"},
		{"Mozilla/5.0", "other"},
		{"curl/8.5.0 at 192.168.1.20", "other"},
	}
	for _, tt := range tests {
		if got := browserFamily(tt.ua); got != tt.want {
			t.Errorf("browserFamily(%q) = %q, want %q", tt.ua, got, tt.want)
		}
	}
}

// The client's own fields go through the names too, and the full user agent
// stays in the stored report only.
func TestReportsMarkdownRedactsClientFields(t *testing.T) {
	r := markdownReport()
	r.Client.Platform = "Tess's laptop"
	r.Client.Version = "raid-night-build"
	body, n := renderReportMarkdown(r)
	if strings.Contains(body, "Tess's") || strings.Contains(body, "raid-night") || !strings.Contains(body, "[person]'s laptop") || n != 10 {
		t.Errorf("%d taken out:\n%s", n, body)
	}
}

func TestReportsListAndFile(t *testing.T) {
	isolateConfig(t)
	t.Setenv("CONCH_TOKEN", "tok")
	srv := newReportServer(t, markdownReport())
	stdout, _, err := runCLI(t, "", "admin", "reports", "list", "--server", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := "8 open 2026-10-16T09:00:00Z principal 12 conch dev-1 server v1 once\n" +
		"7 filed 2026-10-16T09:00:00Z principal 12 conch-web v1 server v1 sometimes https://example.com/issues/1\n"
	if stdout != want {
		t.Errorf("list =\n%s\nwant\n%s", stdout, want)
	}

	stdout, _, err = runCLI(t, "", "admin", "reports", "file", "--server", srv.URL, "7", "https://example.com/issues/2")
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "report 7 filed as https://example.com/issues/2\n" {
		t.Errorf("file = %q", stdout)
	}
	puts := srv.sent("PUT /v1/reports/7")
	if len(puts) != 1 || !strings.Contains(puts[0], `"state":"filed"`) || !strings.Contains(puts[0], `"issue":"https://example.com/issues/2"`) {
		t.Errorf("PUT bodies = %q", puts)
	}
	// An address the server would refuse costs no request.
	if _, _, err := runCLI(t, "", "admin", "reports", "file", "--server", srv.URL, "7", "not-a-url"); err == nil || !strings.Contains(err.Error(), "http or https") {
		t.Errorf("bad url: %v", err)
	}
	if n := len(srv.sent("PUT /v1/reports/7")); n != 1 {
		t.Errorf("%d PUTs, want 1", n)
	}
}
