package push

import (
	"net/url"
	"strconv"
)

// EndpointPortAllowed is the port rule for a Web Push endpoint, in one place so
// the registration check (#1714) and the send (#1715) cannot drift apart.
//
// Without allowPrivate (--push-webpush-allow-private) only 443, the port an
// https URL has when it names none, is accepted. Every public push service
// speaks on 443: Google's, Mozilla's and Apple's, and a UnifiedPush
// distributor behind a public name does too. Any other port is a service a
// member picked on a public host, and conchd would be posting to it from the
// server's address: restricting the port narrows what a hostile endpoint can
// reach to the port a push service has. With allowPrivate the operator has
// said their distributor lives on their own network, where it may be on any
// port, and any port from 1 to 65535 is accepted.
//
// An endpoint that is not a URL, or names a port that is not a number in
// range, is refused. The address rule is Check's and is not applied here.
func EndpointPortAllowed(endpoint string, allowPrivate bool) bool {
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	port := 443
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
		port = n
	}
	return port == 443 || allowPrivate
}
