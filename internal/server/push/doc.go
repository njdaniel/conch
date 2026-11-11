// Package push is the address guard for outbound push requests: the code that
// stops a member-supplied endpoint URL from making conchd post to an internal
// address (design docs/design/mobile-notifications.md, threats T10 and T16).
//
// It is also the Web Push sender (issue #1715): the post-time queue, the
// coalescing, the hourly cap and the per-server budget, the RFC 8291 message
// encryption and RFC 8292 VAPID signing written with the standard library, the
// retries, and the metrics counts. The sender sends only through NewClient.
//
// Input: a host name or IP literal taken from an endpoint URL, a Resolver, and
// the operator's allow-private flag; and, for the sender, committed posts, a
// Source that says whom a post wakes, and the server's VAPID key. Output: the
// checked addresses (Check), a DialContext that connects only to one of them
// (Dialer), an *http.Client that sends with that dialer and follows nothing
// (NewClient), the fixed failure vocabulary a request's outcome is reduced to
// (Classify), and one content-free wake per registration the Source names.
//
// The package owns the forbidden-address table and the predicate over it, the
// resolve-and-check step, the address pin (the socket is opened to an address
// that the same Check call just returned, never to a fresh resolution), the
// hardened client and the failure classes; the endpoint port rule; the wake's
// encryption and its VAPID token; and the sender's bounded queues and limits.
// It owns no store, route or key file logic beyond the VAPID key's, and takes
// its one wire shape, the wake, from pkg/schema. It uses only the standard
// library. Check and the dialer return errors that
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
