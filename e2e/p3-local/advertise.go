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
	// clearText is true for http://, where the bearer travels unprotected.
	clearText bool
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
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("--advertise %q: the scheme must be http or https", raw)
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
	if ip.IsUnspecified() || !isLocal(ip) {
		return nil, fmt.Errorf("--advertise %q: %s is not an address of this machine", raw, ip)
	}
	bind := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	return &advertise{url: u.Scheme + "://" + bind, bind: bind, clearText: u.Scheme == "http"}, nil
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

const advertiseHTTPSNote = `NOTE: --advertise with https://. conchd itself speaks plain HTTP: it is bound to
%s, so the https:// address works only if a TLS front end you run answers
for it. Without one, clients will fail to connect.`

// warning is what is printed, before anything else, for this address.
func (a *advertise) warning() string {
	if a.clearText {
		return fmt.Sprintf(advertiseClearTextWarning, a.bind)
	}
	return fmt.Sprintf(advertiseHTTPSNote, a.bind)
}
