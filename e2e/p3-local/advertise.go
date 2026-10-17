package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// advertise is the address --advertise names: where conchd binds, and the URL
// every file and printed command gives to a client on another machine.
type advertise struct {
	// url is the address as the guest types it, without a trailing slash.
	url string
	// bind is host:port, what conchd is asked to listen on.
	bind string
}

// parseAdvertise checks the --advertise value. Every refusal here happens
// before anything is built or started. isLocal says whether an IP belongs to
// this host's own interfaces; it is a parameter so that a test can say.
//
// The host must be an IP literal and the port explicit and non-zero: conchd is
// bound to exactly that address, so there is no name to resolve and no port to
// pick afterwards that would leave the written files wrong.
func parseAdvertise(raw string, keep, reset bool, isLocal func(net.IP) bool) (*advertise, error) {
	if raw == "" {
		return nil, nil
	}
	switch {
	case reset:
		return nil, errors.New("--advertise cannot be used with --reset: reset only removes what a run started")
	case !keep:
		return nil, errors.New("--advertise needs --keep: the suite without --keep stays on loopback")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("--advertise %q is not a URL: %w", raw, err)
	}
	if u.Scheme == "https" {
		return nil, fmt.Errorf("--advertise %q: https needs a TLS front end on its own address, which this flag does not start; see #380 (README, Optional: TLS and a reverse proxy)", raw)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("--advertise %q: the scheme must be http", raw)
	}
	if u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("--advertise %q: give only scheme, address and port, as http://192.168.1.20:8080", raw)
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil {
		return nil, fmt.Errorf("--advertise %q: the host must be an IP address of this machine, not a name", raw)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("--advertise %q: give an explicit port from 1 to 65535", raw)
	}
	if ip.IsLoopback() {
		return nil, fmt.Errorf("--advertise %q: %s is loopback, which a guest cannot reach; give this machine's address on the network", raw, ip)
	}
	if ip.IsLinkLocalUnicast() {
		return nil, fmt.Errorf("--advertise %q: a link-local address cannot be bound without a zone; give another address of this machine", raw)
	}
	if ip.IsUnspecified() || !isLocal(ip) {
		return nil, fmt.Errorf("--advertise %q: %s is not an address of this machine", raw, ip)
	}
	bind := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	return &advertise{url: u.Scheme + "://" + bind, bind: bind}, nil
}

// hostHasIP reports whether ip is on one of this machine's interfaces.
func hostHasIP(ip net.IP) bool {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false // fail closed: an address that cannot be checked is refused
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

const advertiseClearTextWarning = `WARNING: --advertise over http://. The credentials in the files this run writes
travel in clear text between the guest and this machine: anyone on the network
between them can read and reuse them. Use it only on a network you trust, and
use only the test credentials this run creates: never a credential of your own.
conchd is bound to %s, not to loopback, for as long as the run is kept.`

// warning is what is printed, before anything else, for this address.
func (a *advertise) warning() string {
	return fmt.Sprintf(advertiseClearTextWarning, a.bind)
}

// hostIPv4 is the IPv4 address the suite binds to when it is asked to run off
// loopback (--non-loopback): the first usable one on an interface that is up.
// It is a function of the addresses so that a test can say which there are.
// Loopback, link-local and non-IPv4 addresses are skipped, as parseAdvertise
// would refuse them; with none left it is an error, never a fall back to
// 127.0.0.1, which would make the run pass without proving anything.
func hostIPv4(addrs []net.Addr) (net.IP, error) {
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := n.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			continue
		}
		return ip, nil
	}
	return nil, errors.New("--non-loopback: this machine has no non-loopback IPv4 address on an interface that is up")
}

// upInterfaceAddrs are the addresses of the interfaces that are up.
func upInterfaceAddrs() ([]net.Addr, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var all []net.Addr
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			return nil, err
		}
		all = append(all, addrs...)
	}
	return all, nil
}

// suiteAddress is the address the suite binds to under --non-loopback: this
// machine's own IPv4 and a port that was free a moment ago. It goes through
// the same fields as --advertise, so every client is given it the same way.
func suiteAddress() (*advertise, error) {
	addrs, err := upInterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("--non-loopback: list the interfaces: %w", err)
	}
	ip, err := hostIPv4(addrs)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		return nil, fmt.Errorf("--non-loopback: pick a port on %s: %w", ip, err)
	}
	bind := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return nil, err
	}
	return &advertise{url: "http://" + bind, bind: bind}, nil
}
