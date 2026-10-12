package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	// livekitImage is the image e2e/voice pins, by version and by the digest
	// of the multi-architecture index for it.
	livekitImage   = "livekit/livekit-server:v1.13.7@sha256:6fd3b7088874c4d119160dd688798dfec852bc014786d392caad15f6f63912a3"
	livekitVersion = "1.13.7"
	// labelKey is this program's own label, so nothing here can remove a
	// container e2e/voice (label conch-voice-check) or anyone else started.
	// Its value is made up per run, so two runs cannot remove each other's.
	labelKey = "conch-sdk-probe"
)

func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...) // #nosec G204 -- constant command; args are this program's own
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// server is the LiveKit container this run started.
type server struct {
	r      *run
	name   string
	label  string // labelKey=<this run>
	id     string // container id, once docker run has printed it
	apiURL string // http://127.0.0.1:port
	wsURL  string // ws://127.0.0.1:port
	key    string
	secret string
	frozen bool
	down   bool
}

// startLiveKit runs the pinned image as e2e/voice does: on the host network,
// signalling bound to localhost, automatic room creation off, the same room
// timeouts. (The media TCP port listens on every interface whatever
// bind_addresses says; see README "Voice".) The settings go in a file and the
// key pair in the environment of the docker command, passed on by name, so
// the secret is on no command line. The container is registered for removal
// before it is started.
func (r *run) startLiveKit() (*server, error) {
	httpPort, err := freePort()
	if err != nil {
		return nil, err
	}
	tcpPort, err := freePort()
	if err != nil {
		return nil, err
	}
	// 100 UDP ports for media, above the range e2e/voice draws from.
	base := 56000 + int(randByte()%40)*100
	run := randHex(6)
	srv := &server{
		r:      r,
		name:   "conch-sdk-probe-" + run,
		label:  labelKey + "=" + run,
		apiURL: "http://127.0.0.1:" + httpPort,
		wsURL:  "ws://127.0.0.1:" + httpPort,
		key:    "sdkprobekey",
		secret: "sdk-probe-NOT-FOR-PRODUCTION-" + randHex(24),
	}
	r.secret("the LiveKit API secret", srv.secret)
	cfg := fmt.Sprintf(`port: %s
bind_addresses: ["127.0.0.1"]
rtc:
  tcp_port: %s
  port_range_start: %d
  port_range_end: %d
  use_external_ip: false
room:
  auto_create: false
  empty_timeout: 5
  departure_timeout: 5
  enable_remote_unmute: true
`, httpPort, tcpPort, base, base+99)
	dir, err := os.MkdirTemp(r.tmp, "livekit-")
	if err != nil {
		return nil, err
	}
	// Readable beyond this user because the process in the container may not
	// run as it; the file holds settings and no secret.
	if err := os.Chmod(dir, 0o755); err != nil { // #nosec G302 -- holds only the settings file below
		return nil, err
	}
	cfgPath := filepath.Join(dir, "livekit.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil { // #nosec G306 -- settings only; the key pair is passed in the environment
		return nil, err
	}
	r.srv = srv
	cmd := exec.CommandContext(stopCtx, "docker", "run", "-d", "--name", srv.name, "--label", srv.label, "--network", "host", // #nosec G204 -- constant command; args are this program's own
		"-e", "LIVEKIT_KEYS", "-v", cfgPath+":/etc/livekit.yaml:ro",
		livekitImage, "--config", "/etc/livekit.yaml", "--node-ip", "127.0.0.1")
	cmd.Env = append(os.Environ(), "LIVEKIT_KEYS="+srv.key+": "+srv.secret)
	raw, err := cmd.CombinedOutput()
	out := strings.TrimSpace(string(raw))
	// docker prints the new container's id. A signal that arrives while docker
	// run is working can leave a container with no id known here: remove()
	// then finds it by the name and label this run gave it.
	if f := strings.Fields(out); len(f) > 0 && len(f[len(f)-1]) == 64 {
		srv.id = f[len(f)-1]
	}
	if stopCtx.Err() != nil {
		return nil, errInterrupted
	}
	if err != nil {
		return nil, fmt.Errorf("docker run: %s", r.redact(out))
	}
	if err := srv.waitUp(60 * time.Second); err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		logs, _ := docker(ctx, "logs", "--tail", "20", srv.name)
		return nil, fmt.Errorf("%w\ncontainer log:\n%s", err, r.redact(logs))
	}
	return srv, nil
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	_, port, err := net.SplitHostPort(l.Addr().String())
	return port, err
}

func randByte() byte {
	var b [1]byte
	_, _ = rand.Read(b[:])
	return b[0]
}

// waitUp waits until LiveKit answers its health request.
func (s *server) waitUp(within time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(within)
	var last string
	for {
		resp, err := client.Get(s.apiURL) // #nosec G107 -- the container this run started
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			last = resp.Status
		} else {
			last = "no answer"
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("LiveKit did not answer within %s (last: %s)", within, last)
		}
		if err := pause(100 * time.Millisecond); err != nil {
			return err
		}
	}
}

// act runs one docker command on the container and reports when it was
// started and when it returned: the change it makes happened in between.
func (s *server) act(verb string) (called, returned time.Time, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	called = time.Now()
	out, err := docker(ctx, verb, s.name)
	returned = time.Now()
	if err != nil {
		return called, returned, fmt.Errorf("docker %s: %s", verb, s.r.redact(out))
	}
	return called, returned, nil
}

// freeze suspends every process in the container (docker pause): the server
// stops answering, but its sockets stay open and nothing is closed.
func (s *server) freeze() (called, returned time.Time, err error) {
	called, returned, err = s.act("pause")
	s.frozen = err == nil
	return called, returned, err
}

func (s *server) thaw() (called, returned time.Time, err error) {
	called, returned, err = s.act("unpause")
	if err == nil {
		s.frozen = false
	}
	return called, returned, err
}

// stop is `docker stop`: SIGTERM, then SIGKILL after Docker's default grace
// period of ten seconds.
func (s *server) stop() (called, returned time.Time, err error) {
	called, returned, err = s.act("stop")
	s.down = err == nil
	return called, returned, err
}

// kill is `docker kill`: SIGKILL at once, so the server says nothing first.
func (s *server) kill() (called, returned time.Time, err error) {
	called, returned, err = s.act("kill")
	s.down = err == nil
	return called, returned, err
}

// shutDown sends LiveKit SIGTERM twice. The first asks it to stop once its
// participants have left, which it waits for; the second makes it stop now,
// and tell them so. It returns when the first signal was sent, and how long
// the process took to exit after it.
func (s *server) shutDown() (called time.Time, took time.Duration, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	called = time.Now()
	for i := 0; i < 2; i++ {
		if out, err := docker(ctx, "kill", "--signal", "TERM", s.name); err != nil {
			return called, 0, fmt.Errorf("docker kill --signal TERM: %s", s.r.redact(out))
		}
		if err := pause(200 * time.Millisecond); err != nil {
			return called, 0, err
		}
	}
	for time.Since(called) < 30*time.Second {
		out, err := docker(ctx, "inspect", "--format", "{{.State.Running}}", s.name)
		if err != nil {
			return called, 0, fmt.Errorf("docker inspect: %s", s.r.redact(out))
		}
		if out == "false" {
			s.down = true
			return called, time.Since(called), nil
		}
		if err := pause(100 * time.Millisecond); err != nil {
			return called, 0, err
		}
	}
	// It did not go: make sure it is down, so the measurement is of an outage.
	_, _, err = s.kill()
	return called, time.Since(called), err
}

// restart starts the stopped container again: same address, same key pair,
// no rooms.
func (s *server) restart() error {
	if _, _, err := s.act("start"); err != nil {
		return err
	}
	s.down = false
	return s.waitUp(60 * time.Second)
}

// heal puts the server back in service after a measurement that froze or
// stopped it ended early.
func (s *server) heal() error {
	if s.frozen {
		if _, _, err := s.thaw(); err != nil {
			return err
		}
	}
	if s.down {
		return s.restart()
	}
	return s.waitUp(30 * time.Second)
}

// remove removes the container, running, frozen or stopped. It is safe to
// call twice. When docker run never reported an id, the container is looked
// up by the name and label this run gave it.
func (s *server) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	id := s.id
	if id == "" {
		out, _ := docker(ctx, "ps", "-aq", "--no-trunc", "--filter", "name=^/"+s.name+"$", "--filter", "label="+s.label)
		if f := strings.Fields(out); len(f) > 0 {
			id = f[len(f)-1]
		}
	}
	if id != "" {
		_, _ = docker(ctx, "rm", "-f", id)
	}
}
