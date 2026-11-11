// Package push is the address guard for outbound push requests: the code that
// stops a member-supplied endpoint URL from making conchd post to an internal
// address (design docs/design/mobile-notifications.md, threats T10 and T16).
//
// Input: a host name or IP literal taken from an endpoint URL, a Resolver, and
// the operator's allow-private flag. Output: the checked addresses (Check), a
// DialContext that connects only to one of them (Dialer), an *http.Client that
// sends with that dialer and follows nothing (NewClient), and the fixed
// failure vocabulary a request's outcome is reduced to (Classify).
//
// The package owns the forbidden-address table and the predicate over it, the
// resolve-and-check step, the address pin (the socket is opened to an address
// that the same Check call just returned, never to a fresh resolution), the
// hardened client and the failure classes. It owns no schema, store, route or
// key, and uses only the standard library. Check and the dialer return errors that
// name nothing. The http.Client's own errors are *url.Error values and quote the
// full URL, which for Web Push holds the subscription, so a caller logs, audits
// and exposes only Classify(err, resp), never err.Error().
//
// Limit: only the well-known NAT64 prefix 64:ff9b::/96 and 6to4 are decoded for
// an embedded IPv4 address. A network-specific NAT64 prefix inside 2000::/3 is
// an ordinary global address here, so on an IPv6-only host with its own
// translator an embedded forbidden IPv4 address is not decoded. An operator
// setting for extra NAT64 prefixes is future work.
package push
