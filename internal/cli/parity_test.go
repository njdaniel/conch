package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// API parity for the community model (CLAUDE.md rule 4, ADR-013, issue #994):
// every REST route of the model has a CLI verb, and the verb asks that route.
//
// The server's routes are read from its route table's source, as data, so a
// route added there without a verb here fails this test rather than going
// unnoticed; the CLI itself still imports nothing of the server.

// parityVerb is how a route is reached from the command line.
type parityVerb struct {
	// key is the command as the usage text names it, "channels join".
	key  string
	args []string
}

// communityRoutes maps each route of the model to the verb that asks it.
var communityRoutes = map[string]parityVerb{
	"GET /v1/directory/channels":               {"channels list", []string{"channels", "list"}},
	"POST /v1/channels":                        {"channels create", []string{"channels", "create", "general"}},
	"POST /v1/channels/{channel}/join":         {"channels join", []string{"channels", "join", "general"}},
	"POST /v1/channels/{channel}/leave":        {"channels leave", []string{"channels", "leave", "general"}},
	"PUT /v1/channels/{id}/settings":           {"admin channel settings", []string{"admin", "channel", "settings", "general", "--private"}},
	"GET /v1/directory/members":                {"admin directory members", []string{"admin", "directory", "members"}},
	"PUT /v1/principals/{id}/voice-permission": {"admin principal voice", []string{"admin", "principal", "voice", "9", "on"}},
	"POST /v2/invites":                         {"admin invite create", []string{"admin", "invite", "create"}},
	"GET /v2/invites":                          {"admin invite list", []string{"admin", "invite", "list"}},
	"GET /v2/invites/{id}/joiners":             {"admin invite close|joiners", []string{"admin", "invite", "joiners", "9"}},
	"DELETE /v1/invites/{id}":                  {"admin invite close|joiners", []string{"admin", "invite", "close", "9"}},
	"GET /v1/join/settings":                    {"admin join status", []string{"admin", "join", "status"}},
	"PUT /v1/join/settings":                    {"admin join set", []string{"admin", "join", "set", "--open-signup", "on"}},
	"POST /v0/channels":                        {"admin channel create", []string{"admin", "channel", "create", "general"}},
}

// communityRouteFamilies selects, from the server's route table, the routes
// the model added or changed. A route that matches and is in neither
// communityRoutes nor parityExempt fails the test.
var communityRouteFamilies = regexp.MustCompile(`^(POST /v1/channels|(GET|POST|PUT|PATCH|DELETE) (` +
	`/v1/directory/.*` +
	`|/v1/channels/\{[a-z_]+\}/(join|leave|authors)` +
	`|/v1/channels/\{[a-z_]+\}/settings` +
	`|/v1/principals/\{[a-z_]+\}/voice-permission` +
	`|/v2/invites.*` +
	`|/v1/join/settings` +
	`))$`)

// parityExempt are community routes with no verb, and why.
var parityExempt = map[string]string{
	"POST /v2/invites/preview": "exempt from authentication, for the page a link opens; `conch join account --invite` redeems without previewing (unchanged by #994)",
	"POST /v2/invites/redeem":  "`conch join account --invite` (unchanged by #994, covered in onboarding_test.go)",
}

var routePattern = regexp.MustCompile(`"((?:GET|POST|PUT|PATCH|DELETE) /v[0-9]+/[^"]*)"`)

// serverRoutes reads every route pattern out of the server's route table.
func serverRoutes(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile("../server/routes.go")
	if err != nil {
		t.Fatalf("read the server's route table: %v", err)
	}
	var routes []string
	for _, m := range routePattern.FindAllStringSubmatch(string(src), -1) {
		if !slices.Contains(routes, m[1]) {
			routes = append(routes, m[1])
		}
	}
	if len(routes) < 50 {
		t.Fatalf("read only %d routes from routes.go; the pattern no longer matches its source", len(routes))
	}
	return routes
}

func TestCommunityRoutesHaveVerbs(t *testing.T) {
	routes := serverRoutes(t)
	for _, r := range routes {
		if !communityRouteFamilies.MatchString(r) {
			continue
		}
		_, hasVerb := communityRoutes[r]
		_, exempt := parityExempt[r]
		switch {
		case hasVerb && exempt:
			t.Errorf("%s is both given a verb and exempt", r)
		case !hasVerb && !exempt:
			t.Errorf("route %s is part of the community model but no CLI verb asks it: add one, or exempt it in parityExempt with the reason", r)
		}
	}
	// The tables name routes the server has, so neither can go stale.
	for r := range communityRoutes {
		if !slices.Contains(routes, r) {
			t.Errorf("communityRoutes names %s, which the server does not serve", r)
		}
	}
	for r := range parityExempt {
		if !slices.Contains(routes, r) {
			t.Errorf("parityExempt names %s, which the server does not serve", r)
		}
	}
}

// routeMatches reports whether the request line "GET /v1/channels/general/join"
// is an instance of the pattern "GET /v1/channels/{channel}/join".
func routeMatches(pattern, request string) bool {
	re := regexp.MustCompile("^" + regexp.MustCompile(`\\\{[a-z_]+\\\}`).ReplaceAllString(regexp.QuoteMeta(pattern), `[^/]+`) + "$")
	return re.MatchString(request)
}

func TestRouteMatches(t *testing.T) {
	tests := []struct {
		pattern, request string
		want             bool
	}{
		{"GET /v1/channels", "GET /v1/channels", true},
		{"GET /v1/channels", "GET /v1/channels/general", false},
		{"POST /v1/channels/{channel}/join", "POST /v1/channels/general/join", true},
		{"POST /v1/channels/{channel}/join", "POST /v1/channels/general/leave", false},
		{"PUT /v1/channels/{id}/settings", "PUT /v1/channels/7/settings", true},
		{"PUT /v1/channels/{id}/settings", "GET /v1/channels/7/settings", false},
	}
	for _, tt := range tests {
		if got := routeMatches(tt.pattern, tt.request); got != tt.want {
			t.Errorf("routeMatches(%q, %q) = %v, want %v", tt.pattern, tt.request, got, tt.want)
		}
	}
}

// Each verb asks its route, and only public ones: the recording server sees
// every request it makes.
func TestCommunityVerbsAskTheirRoutes(t *testing.T) {
	usage := usageCommands(t)
	for route, verb := range communityRoutes {
		t.Run(route, func(t *testing.T) {
			if !slices.Contains(usage, verb.key) {
				t.Fatalf("the usage text lists no %q", verb.key)
			}
			isolateConfig(t)
			t.Setenv("CONCH_TOKEN", "tok")
			var mu sync.Mutex
			var seen []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				seen = append(seen, r.Method+" "+r.URL.Path)
				mu.Unlock()
				switch r.Method + " " + r.URL.Path {
				case "GET /v1/directory/channels":
					// What `channel settings` looks the channel's id up in.
					_ = json.NewEncoder(w).Encode(schema.ChannelDirectoryResponseV1{Channels: []schema.ChannelListingV1{{ID: 7, Name: "general", Visibility: schema.ChannelPublic}}})
				case "GET /v1/whoami":
					_ = json.NewEncoder(w).Encode(schema.WhoAmIResponseV1{ID: 1, Kind: schema.PrincipalHuman, Name: "root", Role: schema.RoleOperator})
				default:
					_, _ = w.Write([]byte(`{}`))
				}
			}))
			defer srv.Close()
			// A command may fail on the empty answers; the route it asked is
			// what is checked.
			_ = RunWithStdin(context.Background(), append(slices.Clone(verb.args), "--server", srv.URL), strings.NewReader(""), discard{}, discard{}, "vtest")
			mu.Lock()
			defer mu.Unlock()
			if !slices.ContainsFunc(seen, func(request string) bool { return routeMatches(route, request) }) {
				t.Errorf("%v never asked %s; it asked %q", verb.args, route, seen)
			}
			for _, request := range seen {
				if request != "GET /v1/whoami" && !routeKnown(request, communityRoutes) {
					t.Errorf("%v asked %q, which is no route of the community model's table", verb.args, request)
				}
			}
		})
	}
}

// routeKnown reports whether the request is an instance of a route in table.
func routeKnown(request string, table map[string]parityVerb) bool {
	for pattern := range table {
		if routeMatches(pattern, request) {
			return true
		}
	}
	return false
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
