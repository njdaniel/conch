package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// lanIP is an IPv4 address of this machine that is not loopback, or "".
func lanIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	return ""
}

// freePort is a port nothing listens on at ip right now.
func freePort(t *testing.T, ip string) int {
	t.Helper()
	l, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Skipf("cannot listen on %s: %v", ip, err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

func TestParseAdvertise(t *testing.T) {
	local := func(ip net.IP) bool { return ip.String() == "192.168.1.20" || ip.String() == "fd00::20" }
	tests := []struct {
		name        string
		raw         string
		keep, reset bool
		wantURL     string
		wantBind    string
		wantClear   bool
		wantErr     string // a part of the message; empty means success
	}{
		{name: "unset", raw: "", keep: true},
		{name: "http on a host address", raw: "http://192.168.1.20:8080", keep: true, wantURL: "http://192.168.1.20:8080", wantBind: "192.168.1.20:8080", wantClear: true},
		{name: "a trailing slash is dropped", raw: "http://192.168.1.20:8080/", keep: true, wantURL: "http://192.168.1.20:8080", wantBind: "192.168.1.20:8080", wantClear: true},
		{name: "https is accepted", raw: "https://192.168.1.20:8443", keep: true, wantURL: "https://192.168.1.20:8443", wantBind: "192.168.1.20:8443"},
		{name: "ipv6", raw: "http://[fd00::20]:9000", keep: true, wantURL: "http://[fd00::20]:9000", wantBind: "[fd00::20]:9000", wantClear: true},
		{name: "missing --keep", raw: "http://192.168.1.20:8080", wantErr: "needs --keep"},
		{name: "with --reset", raw: "http://192.168.1.20:8080", keep: true, reset: true, wantErr: "--reset"},
		{name: "not this host", raw: "http://192.168.1.99:8080", keep: true, wantErr: "not an address of this machine"},
		{name: "wildcard", raw: "http://0.0.0.0:8080", keep: true, wantErr: "not an address of this machine"},
		{name: "a name", raw: "http://example.test:8080", keep: true, wantErr: "IP address"},
		{name: "ftp", raw: "ftp://192.168.1.20:8080", keep: true, wantErr: "http or https"},
		{name: "no scheme", raw: "192.168.1.20:8080", keep: true, wantErr: "--advertise"},
		{name: "no port", raw: "http://192.168.1.20", keep: true, wantErr: "explicit port"},
		{name: "port zero", raw: "http://192.168.1.20:0", keep: true, wantErr: "explicit port"},
		{name: "port too large", raw: "http://192.168.1.20:70000", keep: true, wantErr: "explicit port"},
		{name: "a path", raw: "http://192.168.1.20:8080/app", keep: true, wantErr: "only scheme"},
		{name: "userinfo", raw: "http://u@192.168.1.20:8080", keep: true, wantErr: "only scheme"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseAdvertise(tt.raw, tt.keep, tt.reset, local)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.raw == "" {
				if got != nil {
					t.Fatalf("got %+v, want nil", got)
				}
				return
			}
			if got.url != tt.wantURL || got.bind != tt.wantBind || got.clearText != tt.wantClear {
				t.Errorf("got %+v, want url %s bind %s clear %v", got, tt.wantURL, tt.wantBind, tt.wantClear)
			}
		})
	}
}

// TestAdvertiseWarnings: the clear-text warning is for http and says so; https
// gets the note about the TLS front end instead.
func TestAdvertiseWarnings(t *testing.T) {
	for _, tt := range []struct {
		adv  advertise
		want string
	}{
		{advertise{bind: "10.0.0.5:80", clearText: true}, "clear text"},
		{advertise{bind: "10.0.0.5:443"}, "TLS front end"},
	} {
		if got := tt.adv.warning(); !strings.Contains(got, tt.want) || !strings.Contains(got, tt.adv.bind) {
			t.Errorf("warning for %+v = %q, want it to hold %q and the bind", tt.adv, got, tt.want)
		}
	}
}

// TestAdvertiseRefusals runs the program with arguments that must be refused,
// and checks that it said so before creating anything: no root, no warning.
func TestAdvertiseRefusals(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"not this host", []string{"--keep", "--advertise", "http://192.0.2.1:8080"}},
		{"without --keep", []string{"--advertise", "http://127.0.0.1:8080"}},
		{"with --reset", []string{"--reset", "--advertise", "http://127.0.0.1:8080"}},
		{"not http", []string{"--keep", "--advertise", "ftp://127.0.0.1:8080"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newScratch(t)
			stdout, stderr, err := s.run(tt.args...)
			if err == nil {
				t.Fatalf("p3-local %v succeeded", tt.args)
			}
			if stdout != "" || !strings.Contains(stderr, "--advertise") {
				t.Errorf("stdout %q, stderr %q: want a refusal naming --advertise and nothing else printed", stdout, stderr)
			}
			if _, err := os.Lstat(s.root); err == nil {
				t.Errorf("the refusal left %s behind", s.root)
			}
		})
	}
}

// TestKeepAdvertise keeps a run on the runner's own non-loopback address and
// checks the warning comes first, the server is bound to that address and not
// to loopback, every written file names the address, the guest commands are
// printed apart, and --reset leaves nothing.
func TestKeepAdvertise(t *testing.T) {
	ip := lanIP()
	if ip == "" {
		t.Skip("this host has no non-loopback IPv4 address")
	}
	s := newScratch(t)
	bind := net.JoinHostPort(ip, fmt.Sprint(freePort(t, ip)))
	advertised := "http://" + bind

	out := s.mustRun("--keep", "--advertise", advertised)
	runID := runIDOf(t, s.root)

	if !strings.HasPrefix(out, "WARNING: --advertise over http://") || !strings.Contains(strings.SplitN(out, "== build ==", 2)[0], "clear text") {
		t.Errorf("the warning is not the first thing printed:\n%.300s", out)
	}
	if got := printedValue(t, out, "address", ""); got != bind {
		t.Errorf("address = %s, want %s", got, bind)
	}

	// The bind: reachable on the advertised address, refused on loopback.
	resp, err := http.Get(advertised + "/healthz") // #nosec G107 -- this test's own server
	if err != nil {
		t.Fatalf("the server is not on %s: %v", advertised, err)
	}
	_ = resp.Body.Close()
	_, port, _ := net.SplitHostPort(bind)
	if c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", port)); err == nil {
		_ = c.Close()
		t.Errorf("the server also answers on loopback port %s", port)
	}

	// Every written file names the advertised URL, and none the loopback one.
	for _, f := range []string{"tui-a.env", "tui-b.env", "tui-a2.env", "agent-a.env", "agent-b.env", "operator.env", "browser.txt", "commands.txt"} {
		data, err := os.ReadFile(filepath.Join(s.root, f)) // #nosec G304 -- a path in this test's scratch
		must(t, err)
		text := string(data)
		if !strings.Contains(text, advertised) {
			t.Errorf("%s does not name %s", f, advertised)
		}
		if regexp.MustCompile(`127\.0\.0\.1|localhost`).MatchString(text) {
			t.Errorf("%s names loopback", f)
		}
	}

	// The guest's commands are printed, and use the advertised URL for what
	// they say, never a credential.
	guest := out[strings.Index(out, "# ---- on the guest ----"):]
	guest = guest[:strings.Index(guest, "values the walkthrough names")]
	for _, want := range []string{"/bin/conch\n", "/bin/conch-adapter\n", advertised + "/app/"} {
		if !strings.Contains(guest, want) {
			t.Errorf("the guest section lacks %q:\n%s", want, guest)
		}
	}
	opEnv, err := os.ReadFile(filepath.Join(s.root, "operator.env")) // #nosec G304 -- a path in this test's scratch
	must(t, err)
	if tok := regexp.MustCompile(`CONCH_TOKEN=(\S+)`).FindStringSubmatch(string(opEnv)); tok == nil || strings.Contains(out, strings.Trim(tok[1], "'")) {
		t.Error("the operator's credential is missing from operator.env or printed")
	}

	s.mustRun("--reset")
	assertGone(t, s.root, runID)
	if c, err := net.Dial("tcp", bind); err == nil {
		_ = c.Close()
		t.Errorf("something still listens on %s after --reset", bind)
	}
}
