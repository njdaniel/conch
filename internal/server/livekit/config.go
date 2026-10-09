// Package livekit is conchd's standard-library client for a self-hosted
// LiveKit server: it signs HS256 join tokens and calls LiveKit's room API
// (JSON over HTTP, "Twirp") to create rooms, list them and their
// participants, and remove a participant. ADR-004 requires this without a
// LiveKit Go module.
//
// The package never contacts LiveKit until a method is called, and it never
// puts the signing secret or a token in an error or a log attribute.
package livekit

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// Environment variable names, kept here so error messages and conchd's flag
// wiring cannot drift apart.
const (
	EnvURL       = "CONCHD_LIVEKIT_URL"
	EnvAPIURL    = "CONCHD_LIVEKIT_API_URL"
	EnvAPIKey    = "CONCHD_LIVEKIT_API_KEY"    // #nosec G101 -- a variable name, not a credential
	EnvAPISecret = "CONCHD_LIVEKIT_API_SECRET" // #nosec G101 -- a variable name, not a credential
)

// Config is the LiveKit connection settings. The zero value means voice is
// not configured. Build it with ParseConfig, which enforces all-or-nothing.
//
// Config formats without its secret under every verb, so an accidental %v,
// %+v, %#v or structured-log attribute cannot leak it.
type Config struct {
	// URL is the ws:// or wss:// address clients connect to, returned to them as is.
	URL string
	// APIURL is the http:// or https:// address conchd calls for room control.
	APIURL string
	// APIKey is the key half of the signing pair (the "iss" of every token).
	APIKey string
	// APISecret signs every token. It is read from the environment only.
	APISecret string
}

// Configured reports whether voice is configured. ParseConfig guarantees a
// Config is either entirely set or entirely empty.
func (c Config) Configured() bool {
	return c.URL != "" && c.APIURL != "" && c.APIKey != "" && c.APISecret != ""
}

// String renders the config with the secret redacted.
func (c Config) String() string {
	if c == (Config{}) {
		return "livekit.Config{not configured}"
	}
	return fmt.Sprintf("livekit.Config{URL:%q APIURL:%q APIKey:%s APISecret:%s}",
		c.URL, c.APIURL, presence(c.APIKey, "[set]"), presence(c.APISecret, "[redacted]"))
}

// GoString keeps %#v from printing the struct fields.
func (c Config) GoString() string { return c.String() }

// LogValue implements slog.LogValuer with the secret redacted.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("url", c.URL),
		slog.String("api_url", c.APIURL),
		slog.String("api_key", presence(c.APIKey, "[set]")),
		slog.String("api_secret", presence(c.APISecret, "[redacted]")),
	)
}

func presence(v, set string) string {
	if v == "" {
		return "[unset]"
	}
	return set
}

// ParseConfig applies the all-or-nothing rule. With nothing set it returns
// the zero Config and no error (voice is not configured). With some but not
// all of the client address, key and secret set it returns an error naming
// exactly what is missing. An empty apiURL defaults to the client address
// with ws swapped for http and wss for https. It does not contact LiveKit.
//
// Errors never include the values, only their names and schemes.
func ParseConfig(clientURL, apiURL, apiKey, apiSecret string) (Config, error) {
	clientURL = strings.TrimSpace(clientURL)
	apiURL = strings.TrimSpace(apiURL)
	apiKey = strings.TrimSpace(apiKey)
	apiSecret = strings.TrimSpace(apiSecret)

	if clientURL == "" && apiURL == "" && apiKey == "" && apiSecret == "" {
		return Config{}, nil
	}
	var missing []string
	if clientURL == "" {
		missing = append(missing, EnvURL+" (--livekit-url)")
	}
	if apiKey == "" {
		missing = append(missing, EnvAPIKey)
	}
	if apiSecret == "" {
		missing = append(missing, EnvAPISecret)
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("livekit: incomplete configuration, missing %s", strings.Join(missing, ", "))
	}

	cu, err := parseAddr(clientURL, "client URL", "ws", "wss")
	if err != nil {
		return Config{}, err
	}
	var au *url.URL
	if apiURL == "" {
		swapped := *cu
		swapped.Scheme = map[string]string{"ws": "http", "wss": "https"}[cu.Scheme]
		au = &swapped
	} else if au, err = parseAddr(apiURL, "API URL", "http", "https"); err != nil {
		return Config{}, err
	}
	return Config{
		URL:       cu.String(),
		APIURL:    strings.TrimRight(au.String(), "/"),
		APIKey:    apiKey,
		APISecret: apiSecret,
	}, nil
}

// parseAddr validates one address. The error names the setting and the
// problem but not the value: an address may carry userinfo.
func parseAddr(raw, what string, schemes ...string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("livekit: invalid %s: not a URL", what)
	}
	ok := false
	for _, s := range schemes {
		ok = ok || u.Scheme == s
	}
	if !ok {
		return nil, fmt.Errorf("livekit: invalid %s: scheme must be %s", what, strings.Join(schemes, " or "))
	}
	if u.Host == "" {
		return nil, errors.New("livekit: invalid " + what + ": no host")
	}
	// The address is logged at startup and returned to clients, so it must
	// not be a place a credential can hide.
	if u.User != nil {
		return nil, errors.New("livekit: invalid " + what + ": must not contain a user or password")
	}
	return u, nil
}
