package server

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// A message whose payload is conch.approval.v1 or conch.admin_action.v1 is
// client-authored (the server never posts one), so it could be a forged
// approval card naming a real approval id. Every post route refuses it with
// 400 invalid_request and stores nothing (issue #1345).
func TestReservedPayloadIsRefusedOnEveryPostRoute(t *testing.T) {
	f := newFileFixture(t)
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost,
		schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
	f.seat(t, "net1", "aria", schema.NetRoleMember)
	root := f.mustPost(t, "ann", `{"body":"root"}`)
	const hookToken = "reserved-payload-hook"
	if _, err := f.srv.store.CreateHook(t.Context(), "system", hookToken, f.ops.ID, f.p("ann").ID); err != nil {
		t.Fatal(err)
	}

	type route struct {
		name string
		// send posts the payload and returns the HTTP status and error code.
		send func(t *testing.T, payload string) (int, string)
	}
	rest := func(path, who, shape string) func(*testing.T, string) (int, string) {
		return func(t *testing.T, payload string) (int, string) {
			res := f.do(t, "POST", path, who, "application/json", strings.NewReader(fmt.Sprintf(shape, payload)))
			if res.status == http.StatusCreated {
				return res.status, ""
			}
			return res.status, errorCode(t, wireResult{status: res.status, body: string(res.body)})
		}
	}
	mcp := func(extra map[string]any) func(*testing.T, string) (int, string) {
		return func(t *testing.T, payload string) (int, string) {
			var p map[string]any
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"channel": "ops", "body": "x", "payload": p}
			for k, v := range extra {
				args[k] = v
			}
			out := f.mcpCall(t, f.p("aria").token, "post_message", args)
			if !out.isError {
				return http.StatusCreated, ""
			}
			return http.StatusBadRequest, out.code
		}
	}
	whisper := fmt.Sprintf(`"audience":{"kind":"principals","principal_ids":[%d]}`, f.p("wes").ID)
	netAud := fmt.Sprintf(`"audience":{"kind":"net","net_id":%d}`, f.net1.ID)
	routes := []route{
		{"v0", rest("/v0/channels/ops/messages", "ann", `{"body":"x","payload":%s}`)},
		{"v1", rest("/v1/channels/ops/messages", "ann", `{"body":"x","payload":%s}`)},
		{"v2", rest("/v2/channels/ops/messages", "ann", `{"body":"x","payload":%s}`)},
		{"v2 net", rest("/v2/channels/ops/messages", "ann", `{"body":"x","payload":%s,`+netAud+`}`)},
		{"v2 whisper", rest("/v2/channels/ops/messages", "ann", `{"body":"x","payload":%s,`+whisper+`}`)},
		{"v2 reply", rest("/v2/channels/ops/messages", "ann", fmt.Sprintf(`{"body":"x","payload":%%s,"thread_root_id":%d}`, root))},
		{"webhook", rest("/v1/hooks/"+hookToken, "", `{"body":"x","payload":%s}`)},
		{"mcp post_message", mcp(nil)},
		{"mcp net", mcp(map[string]any{"audience": map[string]any{"kind": "net", "net_id": f.net1.ID}})},
		{"mcp whisper", mcp(map[string]any{"audience": map[string]any{"kind": "principals", "principal_ids": []int64{f.p("wes").ID}}})},
		{"mcp reply", mcp(map[string]any{"thread_root_id": root})},
	}
	for _, name := range []string{schema.ApprovalV1Name, schema.AdminActionV1Name} {
		for _, rt := range routes {
			t.Run(rt.name+"/"+name, func(t *testing.T) {
				before := f.messageCount(t)
				status, code := rt.send(t, fmt.Sprintf(`{"schema":%q,"data":{"id":1}}`, name))
				if status != http.StatusBadRequest || code != "invalid_request" {
					t.Errorf("status, code = %d, %q; want 400, invalid_request", status, code)
				}
				if got := f.messageCount(t); got != before {
					t.Errorf("a refused post stored %d message(s)", got-before)
				}
			})
		}
	}

	// An ordinary payload still posts on the routes that take one.
	for _, rt := range routes {
		if rt.name == "v0" {
			continue
		}
		t.Run("ordinary/"+rt.name, func(t *testing.T) {
			before := f.messageCount(t)
			status, code := rt.send(t, `{"schema":"test.thing.v1","data":{"k":"v"}}`)
			if status != http.StatusCreated {
				t.Errorf("status, code = %d, %q; want 201", status, code)
			}
			if got := f.messageCount(t); got != before+1 {
				t.Errorf("stored %d message(s), want 1", got-before)
			}
		})
	}
}

// Structural guard: the refusal lives in the store's two message inserts, so a
// post route added later cannot forget it. Every function in the store that
// runs "INSERT INTO messages (" must call schema.ReservedMessagePayload.
func TestEveryMessageInsertChecksReservedPayloads(t *testing.T) {
	files, err := filepath.Glob("store/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("no store files: %v", err)
	}
	found := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file) // #nosec G304 -- a glob of this package's own store directory
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			body := string(src[fset.Position(fn.Body.Pos()).Offset:fset.Position(fn.Body.End()).Offset])
			if !strings.Contains(body, "INSERT INTO messages (") {
				continue
			}
			found++
			if !strings.Contains(body, "schema.ReservedMessagePayload(") {
				t.Errorf("%s: %s inserts a message without schema.ReservedMessagePayload", file, fn.Name.Name)
			}
		}
	}
	if found != 2 {
		t.Errorf("found %d message insert function(s), want 2; update this guard if the insert paths changed", found)
	}
}
