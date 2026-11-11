package push

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"testing"
)

// fakeResolver answers from fn, which is given the zero-based call number for
// the host, and records every host it was asked about.
type fakeResolver struct {
	mu    sync.Mutex
	fn    func(call int, host string) ([]netip.Addr, error)
	hosts []string
}

func (r *fakeResolver) LookupNetIP(ctx context.Context, _, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	call := len(r.hosts)
	r.hosts = append(r.hosts, host)
	r.mu.Unlock()
	return r.fn(call, host)
}

func (r *fakeResolver) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hosts)
}

func (r *fakeResolver) asked() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.hosts...)
}

func fixed(addrs ...string) *fakeResolver {
	out := make([]netip.Addr, len(addrs))
	for i, a := range addrs {
		out[i] = netip.MustParseAddr(a)
	}
	return &fakeResolver{fn: func(int, string) ([]netip.Addr, error) { return out, nil }}
}

// sequence answers with answers[call], repeating the last one.
func sequence(answers ...[]string) *fakeResolver {
	return &fakeResolver{fn: func(call int, _ string) ([]netip.Addr, error) {
		if call >= len(answers) {
			call = len(answers) - 1
		}
		out := make([]netip.Addr, len(answers[call]))
		for i, a := range answers[call] {
			out[i] = netip.MustParseAddr(a)
		}
		return out, nil
	}}
}

// permitLoopback makes 127.0.0.1 look public for one test, so a loopback
// httptest server can stand in for a public host. Tests that use it must not
// run in parallel.
func permitLoopback(t *testing.T) {
	t.Helper()
	loop := netip.MustParseAddr("127.0.0.1")
	testPermit = func(a netip.Addr) bool { return a == loop }
	t.Cleanup(func() { testPermit = nil })
}

// recorder is a connect function that records its targets and delegates to
// next (a plain dial of a numeric address) or, when next is nil, fails.
type recorder struct {
	mu      sync.Mutex
	targets []string
	next    func(ctx context.Context, network, address string) (net.Conn, error)
}

func (r *recorder) connect(ctx context.Context, network, address string) (net.Conn, error) {
	r.mu.Lock()
	r.targets = append(r.targets, address)
	r.mu.Unlock()
	if r.next == nil {
		return nil, &net.OpError{Op: "dial", Err: net.UnknownNetworkError("marker-dial-error")}
	}
	return r.next(ctx, network, address)
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.targets...)
}

func plainDial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, address)
}
