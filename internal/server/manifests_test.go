package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// manifestFixture creates channels "c1".."c3", an agent, and a human.
type manifestFixture struct {
	srv   *Server
	agent store.Principal
	human store.Principal
}

func newManifestFixture(t *testing.T) manifestFixture {
	t.Helper()
	srv := newTestServer(t)
	ctx := context.Background()
	for _, name := range []string{"c1", "c2", "c3"} {
		if _, err := srv.store.CreateChannel(ctx, name); err != nil {
			t.Fatalf("CreateChannel: %v", err)
		}
	}
	agent, err := srv.store.CreatePrincipal(ctx, store.PrincipalAgent, "leviathan")
	if err != nil {
		t.Fatal(err)
	}
	human, err := srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "nick")
	if err != nil {
		t.Fatal(err)
	}
	return manifestFixture{srv: srv, agent: agent, human: human}
}

func (f manifestFixture) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f manifestFixture) manifestPath(id int64) string {
	return fmt.Sprintf("/v1/principals/%d/manifest", id)
}

func (f manifestFixture) auditActions(t *testing.T) []string {
	t.Helper()
	events, err := f.srv.store.ListAuditEvents(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range events {
		out = append(out, e.Action+" "+e.Subject)
	}
	return out
}

const fullManifestBody = `{"display_name":"Leviathan","tier":"A",` +
	`"capabilities":["messages.post","messages.read"],` +
	`"channels":[{"channel_id":2,"permissions":["post","read"]},{"channel_id":1,"permissions":["read"]}],` +
	`"rate_limits":[{"capability":"messages.post","max":60,"window_seconds":60}]}`

func TestPutGetManifestCreateReplaceRoundTrip(t *testing.T) {
	f := newManifestFixture(t)
	path := f.manifestPath(f.agent.ID)

	rec := f.do(t, http.MethodGet, path, "")
	assertAPIError(t, rec, http.StatusNotFound, "manifest_not_found")

	rec = f.do(t, http.MethodPut, path, fullManifestBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d; body = %s", rec.Code, rec.Body.String())
	}
	var created schema.PutAgentManifestResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if err := created.Manifest.Validate(); err != nil {
		t.Errorf("response manifest invalid: %v", err)
	}
	if created.Manifest.PrincipalID != f.agent.ID {
		t.Errorf("principal_id = %d", created.Manifest.PrincipalID)
	}

	rec = f.do(t, http.MethodGet, path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d; body = %s", rec.Code, rec.Body.String())
	}
	var got schema.GetAgentManifestResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Manifest, created.Manifest) {
		t.Errorf("GET = %+v, want PUT result %+v", got.Manifest, created.Manifest)
	}
	// Stored as given: order preserved.
	if got.Manifest.Capabilities[0] != schema.CapabilityMessagesPost || got.Manifest.Channels[0].ChannelID != 2 {
		t.Errorf("lists reordered: %+v", got.Manifest)
	}

	rec = f.do(t, http.MethodPut, path, `{"display_name":"Leviathan v2","tier":"C"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace status = %d; body = %s", rec.Code, rec.Body.String())
	}
	var replaced schema.PutAgentManifestResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &replaced); err != nil {
		t.Fatal(err)
	}
	if replaced.Manifest.CreatedAt != created.Manifest.CreatedAt {
		t.Errorf("created_at changed: %v -> %v", created.Manifest.CreatedAt, replaced.Manifest.CreatedAt)
	}
	if !replaced.Manifest.UpdatedAt.Time().After(created.Manifest.UpdatedAt.Time()) {
		t.Errorf("updated_at not advanced: %v -> %v", created.Manifest.UpdatedAt, replaced.Manifest.UpdatedAt)
	}
	// Full replacement: a PUT omitting all three lists stores a deny-all manifest.
	m := replaced.Manifest
	if m.DisplayName != "Leviathan v2" || m.Tier != schema.AgentTierC ||
		len(m.Capabilities) != 0 || len(m.Channels) != 0 || len(m.RateLimits) != 0 {
		t.Errorf("replacement not total: %+v", m)
	}
	for _, c := range schema.Capabilities() {
		if m.Allows(c) {
			t.Errorf("deny-all manifest allows %q", c)
		}
	}
	rec = f.do(t, http.MethodGet, path, "")
	var again schema.GetAgentManifestResponseV1
	if err := json.Unmarshal(rec.Body.Bytes(), &again); err != nil || !reflect.DeepEqual(again.Manifest, m) {
		t.Errorf("GET after replace = %+v (%v), want %+v", again.Manifest, err, m)
	}

	want := []string{
		fmt.Sprintf("manifest_created principal:%d", f.agent.ID),
		fmt.Sprintf("manifest_replaced principal:%d", f.agent.ID),
	}
	if got := f.auditActions(t); !reflect.DeepEqual(got, want) {
		t.Errorf("audit = %v, want %v", got, want)
	}
}

func TestPutManifestErrors(t *testing.T) {
	tooLarge := `{"display_name":"` + strings.Repeat("a", maxMessageBodyBytes+1) + `","tier":"A"}`
	tests := []struct {
		name       string
		path       func(f manifestFixture) string
		body       string
		wantStatus int
		wantCode   string
		wantMsg    string
	}{
		{"malformed JSON", agentPath, `{"display_name":`, 400, "invalid_request", ""},
		{"unknown field", agentPath, `{"display_name":"x","tier":"A","admin":true}`, 400, "invalid_request", ""},
		{"body too large", agentPath, tooLarge, 413, "request_too_large", ""},
		{"blank display name", agentPath, `{"display_name":" ","tier":"A"}`, 400, "invalid_manifest", "display_name"},
		{"bad tier", agentPath, `{"display_name":"x","tier":"Z"}`, 400, "invalid_manifest", "tier"},
		{"wildcard capability", agentPath, `{"display_name":"x","tier":"A","capabilities":["*"]}`, 400, "invalid_manifest", "capability"},
		{"duplicate channel", agentPath, `{"display_name":"x","tier":"A","channels":[{"channel_id":1,"permissions":["read"]},{"channel_id":1,"permissions":["post"]}]}`, 400, "invalid_manifest", "channel 1"},
		{"zero rate limit", agentPath, `{"display_name":"x","tier":"A","rate_limits":[{"capability":"messages.post","max":0,"window_seconds":1}]}`, 400, "invalid_manifest", "max"},
		{"non-numeric id", fixedPath("/v1/principals/abc/manifest"), `{"display_name":"x","tier":"A"}`, 400, "invalid_request", ""},
		{"zero id", fixedPath("/v1/principals/0/manifest"), `{"display_name":"x","tier":"A"}`, 400, "invalid_request", ""},
		{"negative id", fixedPath("/v1/principals/-3/manifest"), `{"display_name":"x","tier":"A"}`, 400, "invalid_request", ""},
		{"principal missing", fixedPath("/v1/principals/999/manifest"), `{"display_name":"x","tier":"A"}`, 404, "principal_not_found", ""},
		{"principal is human", func(f manifestFixture) string { return f.manifestPath(f.human.ID) }, `{"display_name":"x","tier":"A"}`, 409, "principal_not_agent", ""},
		{"channel missing", agentPath, `{"display_name":"x","tier":"A","channels":[{"channel_id":42,"permissions":["read"]}]}`, 400, "channel_not_found", "42"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newManifestFixture(t)
			rec := f.do(t, http.MethodPut, tt.path(f), tt.body)
			assertAPIError(t, rec, tt.wantStatus, tt.wantCode)
			if tt.wantMsg != "" {
				var e schema.Error
				_ = json.Unmarshal(rec.Body.Bytes(), &e)
				if !strings.Contains(e.Message, tt.wantMsg) {
					t.Errorf("message = %q, want it to contain %q", e.Message, tt.wantMsg)
				}
			}
			if got := f.auditActions(t); len(got) != 0 {
				t.Errorf("audit events after failed PUT = %v, want none", got)
			}
			if rec := f.do(t, http.MethodGet, f.manifestPath(f.agent.ID), ""); rec.Code != http.StatusNotFound {
				t.Errorf("manifest stored after failed PUT: status %d", rec.Code)
			}
		})
	}
}

func agentPath(f manifestFixture) string { return f.manifestPath(f.agent.ID) }

func fixedPath(p string) func(manifestFixture) string {
	return func(manifestFixture) string { return p }
}

func TestGetManifestErrors(t *testing.T) {
	tests := []struct {
		name       string
		path       func(f manifestFixture) string
		wantStatus int
		wantCode   string
	}{
		{"non-numeric id", fixedPath("/v1/principals/abc/manifest"), 400, "invalid_request"},
		{"zero id", fixedPath("/v1/principals/0/manifest"), 400, "invalid_request"},
		{"no manifest stored", agentPath, 404, "manifest_not_found"},
		{"unknown principal", fixedPath("/v1/principals/999/manifest"), 404, "manifest_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newManifestFixture(t)
			assertAPIError(t, f.do(t, http.MethodGet, tt.path(f), ""), tt.wantStatus, tt.wantCode)
		})
	}
}

// A manifest read failure (which includes a stored row that fails validation;
// that case is covered at the store layer) is a 500 with no manifest in the body.
func TestGetManifestReadFailureIsInternalError(t *testing.T) {
	f := newManifestFixture(t)
	if rec := f.do(t, http.MethodPut, f.manifestPath(f.agent.ID), fullManifestBody); rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d", rec.Code)
	}
	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	rec := f.do(t, http.MethodGet, f.manifestPath(f.agent.ID), "")
	assertAPIError(t, rec, http.StatusInternalServerError, "internal_error")
	if bytes.Contains(rec.Body.Bytes(), []byte("capabilities")) {
		t.Errorf("error body leaks manifest: %s", rec.Body.String())
	}
}

var timestampFields = regexp.MustCompile(`"(created_at|updated_at)":"[^"]*"`)

// TestManifestGoldenResponse checks the GET response encodes to the schema
// fixture bytes, modulo the server-assigned timestamps. Fixture ids (principal
// 3, channels 7 and 9) are reproduced by creating enough rows first.
func TestManifestGoldenResponse(t *testing.T) {
	srv := newTestServer(t)
	f := manifestFixture{srv: srv}
	ctx := context.Background()
	for i := 1; i <= 9; i++ {
		if _, err := srv.store.CreateChannel(ctx, fmt.Sprintf("c%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []struct {
		kind store.PrincipalKind
		name string
	}{{store.PrincipalHuman, "h1"}, {store.PrincipalHuman, "h2"}, {store.PrincipalAgent, "leviathan"}} {
		if _, err := srv.store.CreatePrincipal(ctx, p.kind, p.name); err != nil {
			t.Fatal(err)
		}
	}
	reqBody, err := os.ReadFile("../../pkg/schema/testdata/put-agent-manifest-request-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	golden, err := os.ReadFile("../../pkg/schema/testdata/get-agent-manifest-response-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, http.MethodPut, f.manifestPath(3), string(reqBody)); rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d; body = %s", rec.Code, rec.Body.String())
	}
	rec := f.do(t, http.MethodGet, f.manifestPath(3), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d", rec.Code)
	}
	// Normalise timestamps to the fixture's, then compare as indented JSON.
	fixtureTimes := timestampFields.FindAll(bytes.ReplaceAll(golden, []byte(": "), []byte(":")), -1)
	i := 0
	body := timestampFields.ReplaceAllFunc(rec.Body.Bytes(), func([]byte) []byte {
		defer func() { i++ }()
		return fixtureTimes[i]
	})
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(pretty.String()), strings.TrimSpace(string(golden)); got != want {
		t.Errorf("GET response differs from fixture\n got: %s\nwant: %s", got, want)
	}
}
