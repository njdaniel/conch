package push

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"
)

// connectTimeout bounds a single connect attempt.
const connectTimeout = 2 * time.Second

// Dialer opens connections only to addresses that pass Check. It is the
// address pin: on every dial it resolves the host, checks every answer, and
// connects to one of the addresses that same Check call returned, as a
// numeric address and port. The host name is never handed to a system dialer,
// so nothing resolves a second time between the check and the connect.
type Dialer struct {
	resolver     Resolver
	allowPrivate bool
	// connect opens the socket to a numeric "ip:port". Tests replace it to
	// record targets; production uses connectNumeric.
	connect func(ctx context.Context, network, address string) (net.Conn, error)
}

// NewDialer returns a Dialer. A nil resolver means DefaultResolver.
func NewDialer(resolver Resolver, allowPrivate bool) *Dialer {
	d := &Dialer{resolver: resolver, allowPrivate: allowPrivate}
	d.connect = d.connectNumeric
	return d
}

// connectNumeric is the real connect. Its Control hook re-checks the address
// the kernel is about to connect to, so a path that handed this dialer a name
// would still be stopped.
func (d *Dialer) connectNumeric(ctx context.Context, network, address string) (net.Conn, error) {
	nd := net.Dialer{
		Timeout: connectTimeout,
		Control: func(_, addr string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(addr)
			if err != nil || ForbiddenWith(ap.Addr(), d.allowPrivate) {
				return ErrAddressRefused
			}
			return nil
		},
	}
	return nd.DialContext(ctx, network, address)
}

// DialContext is for http.Transport.DialContext. It resolves and checks the
// host of address, then connects to one of the checked addresses with
// address's port. It tries the checked addresses in turn. Errors are fixed
// class strings: ErrAddressRefused if the check refused, ErrLookupFailed if
// the name did not resolve, otherwise the class of the last connect failure.
func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, classedError(ClassRequestFailed)
	}
	host, portStr, err := net.SplitHostPort(address)
	if err != nil {
		return nil, classedError(ClassRequestFailed)
	}
	port, err := strconv.ParseUint(portStr, 10, 16)
	if err != nil || port == 0 {
		return nil, classedError(ClassRequestFailed)
	}

	addrs, err := Check(ctx, host, d.resolver, d.allowPrivate)
	if err != nil {
		return nil, err
	}

	last := error(classedError(ClassRequestFailed))
	for _, a := range addrs {
		if (network == "tcp4" && !a.Is4()) || (network == "tcp6" && !a.Is6()) {
			continue
		}
		conn, err := d.connect(ctx, network, netip.AddrPortFrom(a, uint16(port)).String())
		if err == nil {
			return conn, nil
		}
		last = connectClass(ctx, err)
		if ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// connectClass reduces a connect failure to its class, so the address the
// system dialer put in its error text is dropped.
func connectClass(ctx context.Context, err error) error {
	if errors.Is(err, ErrAddressRefused) {
		return ErrAddressRefused
	}
	if ctx.Err() != nil {
		return ctxClass(ctx.Err())
	}
	return classedError(classifyError(err))
}
