package server

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/coder/websocket"
)

// ParseAllowedOrigins splits the --allowed-origins value into the extra
// origins a WebSocket upgrade may carry. Each entry is one exact origin:
// http or https, a host, an optional port, and nothing else. There are no
// wildcards. The library would take path.Match patterns, and a pattern such as
// "http://*:*" passes for nearly every origin, which is the origin check
// turned off by a typo; so anything that is not a plain origin is refused.
func ParseAllowedOrigins(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		o := strings.TrimSpace(part)
		if o == "" {
			return nil, errors.New("allowed origins: empty entry")
		}
		// The library matches an entry with path.Match, where [ opens a
		// character class; so an IPv6 literal cannot be listed safely.
		if strings.ContainsAny(o, "[]") {
			return nil, fmt.Errorf("allowed origins: %q: an IPv6 address is not supported here; use a host name or an IPv4 address", o)
		}
		if strings.ContainsAny(o, `*?\`) {
			return nil, fmt.Errorf("allowed origins: %q: wildcards are not allowed; list each origin exactly", o)
		}
		u, err := url.Parse(o)
		if err != nil {
			return nil, fmt.Errorf("allowed origins: %q is not an origin such as http://localhost:5173", o)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return nil, fmt.Errorf("allowed origins: %q is not an origin such as http://localhost:5173", o)
		}
		// Browsers send the scheme and host in lower case.
		out = append(out, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return out, nil
}

// acceptOptions is what every socket handler passes to websocket.Accept.
// With no extra origins it is nil: the library's own same-origin check, as it
// was before the flag existed.
func (s *Server) acceptOptions() *websocket.AcceptOptions {
	if len(s.cfg.AllowedOrigins) == 0 {
		return nil
	}
	return &websocket.AcceptOptions{OriginPatterns: s.cfg.AllowedOrigins}
}
