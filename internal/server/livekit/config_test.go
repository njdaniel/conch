package livekit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	tests := []struct {
		name                     string
		url, apiURL, key, secret string
		want                     Config
		wantErr                  []string // substrings the error must contain
		notErr                   []string // substrings the error must not contain
	}{
		{name: "none set", want: Config{}},
		{name: "only whitespace", url: "  ", want: Config{}},
		{
			name: "full, api url defaults from ws", url: "ws://lk.local:7880", key: "k", secret: "s",
			want: Config{URL: "ws://lk.local:7880", APIURL: "http://lk.local:7880", APIKey: "k", APISecret: NewSecret("s")},
		},
		{
			name: "full, api url defaults from wss", url: "wss://voice.example", key: "k", secret: "s",
			want: Config{URL: "wss://voice.example", APIURL: "https://voice.example", APIKey: "k", APISecret: NewSecret("s")},
		},
		{
			name: "full, explicit api url", url: "wss://voice.example", apiURL: "http://127.0.0.1:7880/", key: "k", secret: "s",
			want: Config{URL: "wss://voice.example", APIURL: "http://127.0.0.1:7880", APIKey: "k", APISecret: NewSecret("s")},
		},
		{name: "missing url", key: "k", secret: "s", wantErr: []string{EnvURL}, notErr: []string{EnvAPIKey, EnvAPISecret}},
		{name: "missing key", url: "ws://h", secret: "s", wantErr: []string{EnvAPIKey}, notErr: []string{EnvURL, EnvAPISecret}},
		{name: "missing secret", url: "ws://h", key: "k", wantErr: []string{EnvAPISecret}, notErr: []string{EnvURL, EnvAPIKey}},
		{name: "only url", url: "ws://h", wantErr: []string{EnvAPIKey, EnvAPISecret}, notErr: []string{EnvURL}},
		{name: "only key", key: "k", wantErr: []string{EnvURL, EnvAPISecret}, notErr: []string{EnvAPIKey}},
		{name: "only secret", secret: "topsecret", wantErr: []string{EnvURL, EnvAPIKey}, notErr: []string{EnvAPISecret, "topsecret"}},
		{name: "only api url", apiURL: "http://h", wantErr: []string{EnvURL, EnvAPIKey, EnvAPISecret}},
		{name: "client url wrong scheme", url: "http://h", key: "k", secret: "s", wantErr: []string{"client URL", "ws or wss"}},
		{name: "api url wrong scheme", url: "ws://h", apiURL: "ws://h", key: "k", secret: "s", wantErr: []string{"API URL", "http or https"}},
		{name: "no scheme", url: "lk.local:7880", key: "k", secret: "s", wantErr: []string{"client URL"}},
		{name: "no host", url: "ws://", key: "k", secret: "s", wantErr: []string{"no host"}},
		{name: "unparsable", url: "ws://a b\x7f", key: "k", secret: "s", wantErr: []string{"client URL"}, notErr: []string{"a b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseConfig(tt.url, tt.apiURL, tt.key, tt.secret)
			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("want error containing %v, got config %v", tt.wantErr, got)
				}
				for _, s := range tt.wantErr {
					if !strings.Contains(err.Error(), s) {
						t.Errorf("error %q missing %q", err, s)
					}
				}
				for _, s := range tt.notErr {
					if strings.Contains(err.Error(), s) {
						t.Errorf("error %q must not contain %q", err, s)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// Secrets are compared by value: each holds its own pointer.
			if got.URL != tt.want.URL || got.APIURL != tt.want.APIURL || got.APIKey != tt.want.APIKey ||
				got.APISecret.reveal() != tt.want.APISecret.reveal() {
				t.Errorf("config = %#v, want %#v", got, tt.want)
			}
			if got.Configured() != (tt.want != Config{}) {
				t.Errorf("Configured() = %v", got.Configured())
			}
		})
	}
}

func TestConfigRedactsSecret(t *testing.T) {
	cfg := Config{URL: "ws://h", APIURL: "http://h", APIKey: "key-id", APISecret: NewSecret("do-not-leak")}
	var sb strings.Builder
	h := slog.NewTextHandler(&sb, nil)
	slog.New(h).Info("cfg", "livekit", cfg)
	outputs := map[string]string{
		"%v":    fmt.Sprintf("%v", cfg),
		"%+v":   fmt.Sprintf("%+v", cfg),
		"%#v":   fmt.Sprintf("%#v", cfg),
		"%s":    fmt.Sprint(cfg),
		"&%v":   fmt.Sprintf("%v", &cfg),
		"slog":  sb.String(),
		"empty": Config{}.String(),
	}
	for verb, out := range outputs {
		if strings.Contains(out, "do-not-leak") {
			t.Errorf("%s output leaks the secret: %s", verb, out)
		}
	}
	if !strings.Contains(outputs["%v"], "[redacted]") || !strings.Contains(outputs["slog"], "[redacted]") {
		t.Errorf("redaction marker missing: %v / %v", outputs["%v"], outputs["slog"])
	}
}

// An address is logged at startup and handed to clients, so it may not carry
// a user or password, and the error must not repeat them.
func TestParseConfigRejectsCredentialsInAddresses(t *testing.T) {
	for _, tt := range []struct{ name, url, api string }{
		{"client address", "wss://user:hunter2@voice.example", ""},
		{"client address, user only", "ws://user@voice.example", ""},
		{"api address", "wss://voice.example", "https://user:hunter2@voice.example"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseConfig(tt.url, tt.api, "k", "s")
			if err == nil || !strings.Contains(err.Error(), "must not contain a user or password") {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "user@") || strings.Contains(err.Error(), "user:") {
				t.Errorf("error repeats the credential: %v", err)
			}
		})
	}
}

// The room-API path is appended to the API address, so a query or fragment in
// either address would swallow it.
func TestParseConfigRejectsQueryAndFragment(t *testing.T) {
	for _, tt := range []struct{ name, url, api string }{
		{"client query", "wss://voice.example/?project=1", ""},
		{"client fragment", "wss://voice.example/#frag", ""},
		{"client bare question mark", "wss://voice.example/?", ""},
		{"api query", "wss://voice.example", "https://voice.example/?x=1"},
		{"api fragment", "wss://voice.example", "https://voice.example/#frag"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ParseConfig(tt.url, tt.api, "k", "s"); err == nil || !strings.Contains(err.Error(), "query or fragment") {
				t.Fatalf("err = %v, want a refusal", err)
			}
		})
	}
	// A path is fine: LiveKit behind a prefix on a reverse proxy.
	cfg, err := ParseConfig("wss://voice.example/lk/", "", "k", "s")
	if err != nil || cfg.APIURL != "https://voice.example/lk" {
		t.Fatalf("config = %v, %v; want API address https://voice.example/lk", cfg, err)
	}
}

// The secret cannot be printed by any route: not by formatting the config,
// the client, or a struct that holds either in an unexported field (where fmt
// cannot call a String method), not by a text or JSON log handler, and not by
// encoding/json.
func TestSecretCannotBePrinted(t *testing.T) {
	const secret = "do-not-leak-0123456789"
	cfg := Config{URL: "ws://h", APIURL: "http://h", APIKey: "key-id", APISecret: NewSecret(secret)}
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	type holder struct {
		cfg    Config
		client *Client
		byVal  Client
		secret Secret
	}
	h := holder{cfg: cfg, client: client, byVal: *client, secret: cfg.APISecret} //nolint:govet // copying the client is the point: a copy must not leak either
	values := map[string]any{
		"config": cfg, "*config": &cfg, "secret": cfg.APISecret, "*secret": &cfg.APISecret,
		"*client": client, "client by value": *client, "holder": h, "*holder": &h, //nolint:govet // as above
	}
	check := func(what, out string) {
		t.Helper()
		if strings.Contains(out, secret) || strings.Contains(out, secret[:10]) {
			t.Errorf("%s leaks the secret: %s", what, out)
		}
	}
	for name, v := range values {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
			check(name+" "+verb, fmt.Sprintf(verb, v))
		}
		var text, js strings.Builder
		slog.New(slog.NewTextHandler(&text, nil)).Info("x", "v", v)
		slog.New(slog.NewJSONHandler(&js, nil)).Info("x", "v", v)
		check(name+" slog text", text.String())
		check(name+" slog json", js.String())
		if raw, err := json.Marshal(v); err == nil {
			check(name+" json", string(raw))
		}
	}
	if got := fmt.Sprintf("%v", client); !strings.Contains(got, "[redacted]") {
		t.Errorf("client prints as %q, want the redaction marker", got)
	}
	if NewSecret("").IsSet() || !NewSecret("x").IsSet() || NewSecret("").String() != "[unset]" {
		t.Error("Secret set/unset reporting is wrong")
	}
}
