package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"

	"github.com/njdaniel/conch/pkg/schema"
)

// hookLimitUnits are the characters the limit test builds bodies from. The
// first is the one that broke (issue #238): json.Marshal writes each of <, >
// and & as a six-byte escape.
var hookLimitUnits = map[string]string{
	"angle brackets": "<",
	"ampersands":     "&",
	"all of them":    "<>&\"\né x",
	"quotes":         "\"",
	"newlines":       "\nx", // a body of only whitespace is refused, as before
	"non-ASCII":      "é",
	"plain letters":  "x",
}

// jsonEscapedUnit writes unit the way a sender would: quotes and newlines
// escaped, everything else literal, so the sent body is exactly as long as
// the test says.
func jsonEscapedUnit(unit string) string {
	return strings.NewReplacer(`"`, `\"`, "\n", `\n`).Replace(unit)
}

// hookLimitBody returns a request body of exactly total bytes and the message
// body it must be stored as. For text/plain the two are the same; for JSON the
// body is the v1 post object {"body":"..."}.
func hookLimitBody(t *testing.T, unit string, total int, asJSON bool) (sent, want string) {
	t.Helper()
	prefix, suffix := "", ""
	if asJSON {
		prefix, suffix = `{"body":"`, `"}`
	}
	sentUnit := unit
	if asJSON {
		sentUnit = jsonEscapedUnit(unit)
	}
	room := total - len(prefix) - len(suffix)
	var sentBody, wantBody strings.Builder
	for sentBody.Len()+len(sentUnit) <= room {
		sentBody.WriteString(sentUnit)
		wantBody.WriteString(unit)
	}
	// Plain letters fill what a whole unit cannot.
	for sentBody.Len() < room {
		sentBody.WriteByte('a')
		wantBody.WriteByte('a')
	}
	sent = prefix + sentBody.String() + suffix
	if len(sent) != total {
		t.Fatalf("built a %d-byte body, want %d", len(sent), total)
	}
	return sent, wantBody.String()
}

func ingestHook(t *testing.T, srv *Server, token, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/"+token, strings.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestHookIngestSizeLimitAppliesToWhatWasSent: the limit is on the bytes the
// sender sent, not on any re-encoding of them (issue #238). A body of exactly
// schema.MaxPostRequestBytes is stored byte for byte whatever it is made of;
// one byte more is refused.
func TestHookIngestSizeLimitAppliesToWhatWasSent(t *testing.T) {
	srv := newTestServer(t)
	channel, principal := createTestChannelAndPrincipal(t, srv)
	const token = "hook-token"
	if _, err := srv.store.CreateHook(context.Background(), "system", token, channel.ID, principal.ID); err != nil {
		t.Fatalf("CreateHook: %v", err)
	}
	contentTypes := []struct {
		name   string
		header string
		asJSON bool
	}{
		{"text/plain", "text/plain; charset=utf-8", false},
		{"json", "application/json", true},
	}
	for unitName, unit := range hookLimitUnits {
		for _, ct := range contentTypes {
			t.Run(unitName+"/"+ct.name+"/at the limit", func(t *testing.T) {
				sent, want := hookLimitBody(t, unit, schema.MaxPostRequestBytes, ct.asJSON)
				rec := ingestHook(t, srv, token, ct.header, sent)
				if rec.Code != http.StatusCreated {
					t.Fatalf("status = %d, want %d; body = %.200s", rec.Code, http.StatusCreated, rec.Body.String())
				}
				var response schema.PostMessageResponseV1
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if response.Message.Body != want {
					t.Errorf("response body differs from what was sent (len %d, want %d)", len(response.Message.Body), len(want))
				}
				if response.Message.AuthorID != principal.ID {
					t.Errorf("author = %d, want the hook's principal %d", response.Message.AuthorID, principal.ID)
				}
				stored, err := srv.store.ListVisibleMessages(context.Background(), channel.ID, store.Reader{}, response.Message.ID-1, 1)
				if err != nil || len(stored) != 1 {
					t.Fatalf("ListVisibleMessages = %d messages, %v", len(stored), err)
				}
				if stored[0].Body != want {
					t.Errorf("stored body differs from what was sent (len %d, want %d)", len(stored[0].Body), len(want))
				}
			})
			t.Run(unitName+"/"+ct.name+"/one byte over", func(t *testing.T) {
				sent, _ := hookLimitBody(t, unit, schema.MaxPostRequestBytes+1, ct.asJSON)
				rec := ingestHook(t, srv, token, ct.header, sent)
				if rec.Code != http.StatusRequestEntityTooLarge {
					t.Fatalf("status = %d, want %d; body = %.200s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
				}
			})
		}
	}
}

// TestHookIngestLargestBodyFitsAFrame: the largest body the hook route stores
// (all <, the worst case for the escaping on the way out) reaches a v2
// subscriber in a frame no larger than schema.MaxMessageFrameBytes, so the
// bound clients read with still holds.
func TestHookIngestLargestBodyFitsAFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv := newTestServer(t)
	channel, principal := createTestChannelAndPrincipal(t, srv)
	const token = "hook-token"
	if _, err := srv.store.CreateHook(ctx, "system", token, channel.ID, principal.ID); err != nil {
		t.Fatalf("CreateHook: %v", err)
	}
	base := wsTestServer(t, srv)
	conn := wsDial(t, ctx, base+"/v2/ws?channel="+channel.Name)
	conn.SetReadLimit(schema.MaxMessageFrameBytes)

	sent, want := hookLimitBody(t, "<", schema.MaxPostRequestBytes, false)
	if rec := ingestHook(t, srv, token, "text/plain", sent); rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	typ, frame, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("frame type = %v, want text", typ)
	}
	if len(frame) > schema.MaxMessageFrameBytes {
		t.Errorf("frame is %d bytes, over MaxMessageFrameBytes (%d)", len(frame), schema.MaxMessageFrameBytes)
	}
	var got schema.MessageV1
	if err := json.Unmarshal(frame, &got); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if got.Body != want {
		t.Errorf("frame body differs from what was sent (len %d, want %d)", len(got.Body), len(want))
	}
}

// Rendered text that is not valid UTF-8 is stored as valid UTF-8, each stray
// byte replaced with U+FFFD: what the JSON routes' decoder does to the same
// bytes, and what this route did when it re-encoded the post before storing
// it (issue #238 removed the re-encoding, not this).
func TestHookIngestStoresValidUTF8(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff})
	channel, principal := createTestChannelAndPrincipal(t, srv)
	created := postJSON(t, srv, "/v1/hooks", `{"channel":"`+channel.Name+`","principal":`+strconvItoa(principal.ID)+`}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create hook = %d: %s", created.Code, created.Body.String())
	}
	var hook struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &hook); err != nil || hook.Token == "" {
		t.Fatalf("hook token missing from %s (%v)", created.Body.String(), err)
	}
	// 0xff is never valid; 0xc3 starts a sequence it does not finish; then two
	// stray bytes in a row, which must become two replacements, not one; and
	// 0xe2 followed by a byte that cannot continue it.
	stray := string([]byte{'a', 0xff, 'b', 0xc3, 'c', 0xff, 0xfe, 'd', 0xe2, '(', 'e'})
	rec := ingestHook(t, srv, hook.Token, "text/plain", stray)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest = %d: %s", rec.Code, rec.Body.String())
	}
	replacement := string(rune(0xfffd))
	want := "a" + replacement + "b" + replacement + "c" + replacement + replacement + "d" + replacement + "(e"
	// The reference is what the JSON decoder makes of the same bytes inside a
	// string: the two routes must agree.
	var viaJSON string
	if err := json.Unmarshal([]byte(`"`+stray+`"`), &viaJSON); err != nil || viaJSON != want {
		t.Fatalf("the JSON decoder gives %q (%v), want %q: the test's own expectation is wrong", viaJSON, err, want)
	}
	msgs, err := srv.store.ListVisibleMessages(context.Background(), channel.ID, store.ChannelWideOnly, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("stored %d messages, want 1", len(msgs))
	}
	if msgs[0].Body != want {
		t.Fatalf("stored body = %q, want %q", msgs[0].Body, want)
	}
}

// A JSON hook body is one v1 post object and nothing else. The second decode
// the old path did (in handlePostMessageV1) is gone; the first one is what
// refuses these, and must keep doing so.
func TestHookIngestRefusesWhatIsNotOnePostObject(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{AuthMode: AuthOff})
	channel, principal := createTestChannelAndPrincipal(t, srv)
	created := postJSON(t, srv, "/v1/hooks", `{"channel":"`+channel.Name+`","principal":`+strconvItoa(principal.ID)+`}`)
	var hook struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &hook); err != nil || hook.Token == "" {
		t.Fatalf("hook token missing from %s (%v)", created.Body.String(), err)
	}
	for name, body := range map[string]string{
		"trailing object":  `{"body":"x"}{"body":"y"}`,
		"trailing garbage": `{"body":"x"}x`,
		"an array":         `[{"body":"x"}]`,
		"a string":         `"x"`,
		"an unknown field": `{"body":"x","extra":1}`,
		"author as string": `{"body":"x","author_id":"1"}`,
		"payload a string": `{"body":"x","payload":"p"}`,
	} {
		rec := ingestHook(t, srv, hook.Token, "application/json", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: ingest = %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	msgs, err := srv.store.ListVisibleMessages(context.Background(), channel.ID, store.ChannelWideOnly, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("refused bodies stored %d message(s)", len(msgs))
	}
}

func strconvItoa(n int64) string { return strconv.FormatInt(n, 10) }
