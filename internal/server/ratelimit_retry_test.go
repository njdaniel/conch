package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// toolErrorBody is the schema.Error an MCP tool error carries as JSON in its
// text content, read out of the raw JSON-RPC response, together with the raw
// text so a test can also say which keys are absent.
func toolErrorBody(t *testing.T, raw string) (schema.Error, string) {
	t.Helper()
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("decode MCP response %s: %v", raw, err)
	}
	if !response.Result.IsError || len(response.Result.Content) == 0 {
		t.Fatalf("not a tool error: %s", raw)
	}
	text := response.Result.Content[0].Text
	var e schema.Error
	if err := json.Unmarshal([]byte(text), &e); err != nil {
		t.Fatalf("tool error text %q is not a schema.Error: %v", text, err)
	}
	return e, text
}

// A rate-limited call says how long to wait as a number, in the error itself
// (issue #228): an MCP caller gets no Retry-After header, and should not have
// to read the delay out of a sentence. Over REST the body and the header
// agree. No other error gains the field.
func TestRateLimitedErrorCarriesItsRetryDelay(t *testing.T) {
	const window = 120
	f := newRateFixture(t)
	f.limit(t, "aria", rl(schema.CapabilityMessagesPost, 1, window), rl(schema.CapabilityApprovalsRequest, 1, window))

	t.Run("MCP post_message", func(t *testing.T) {
		post := func() mcpOutcome {
			return f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "x"})
		}
		if out := post(); out.isError {
			t.Fatalf("first post refused: %s", out.body)
		}
		checkRetryDelay(t, post(), window)
	})
	t.Run("MCP request_approval", func(t *testing.T) {
		args := approvalMCPArguments(f.ops.ID, time.Now().Add(time.Hour))
		request := func() mcpOutcome { return f.mcpCall(t, f.p("aria").token, "request_approval", args) }
		if out := request(); out.isError {
			t.Fatalf("first request refused: %s", out.body)
		}
		checkRetryDelay(t, request(), window)
	})
	t.Run("REST body and header agree", func(t *testing.T) {
		// The post budget went in the first subtest; part of the window later
		// the delay is shorter, and body and header must still say the same.
		f.clock.Advance(50 * time.Second)
		rec := f.restPost(t, "aria", `{"body":"x"}`)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("post = %d, want 429: %s", rec.Code, rec.Body.String())
		}
		var e schema.Error
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		header, err := strconv.Atoi(rec.Header().Get("Retry-After"))
		if err != nil {
			t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
		}
		if e.Code != schema.ErrorCodeRateLimited || e.RetryAfterSeconds != header || header != window-50 {
			t.Errorf("body %+v, Retry-After %d: want code rate_limited and both delays %d", e, header, window-50)
		}
	})
	t.Run("no other error has the field", func(t *testing.T) {
		// A tool error that is not a rate limit: arlo may post but not read.
		out := f.mcpCall(t, f.p("arlo").token, "read_channel", map[string]any{"channel": "ops"})
		e, text := toolErrorBody(t, out.body)
		if e.Code == schema.ErrorCodeRateLimited || e.RetryAfterSeconds != 0 || strings.Contains(text, "retry_after_seconds") {
			t.Errorf("a %s tool error carries a retry delay: %s", e.Code, text)
		}
		// REST refusals of the other kinds: not a member, bad body, no credential.
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"404 not a member": f.restPost(t, "nomad", `{"body":"x"}`),
			"400 bad body":     f.do(t, http.MethodPost, "/v2/channels/ops/messages", f.p("ann").token, `{"body":`),
			"401 no token":     f.do(t, http.MethodGet, "/v1/channels/ops/messages", "", ""),
			"403 not operator": f.do(t, http.MethodGet, "/v1/hooks", f.p("ann").token, ""),
		} {
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("%s: decode %q: %v", name, rec.Body.String(), err)
			}
			if _, has := raw["retry_after_seconds"]; has || rec.Code == http.StatusTooManyRequests {
				t.Errorf("%s (status %d) carries retry_after_seconds: %s", name, rec.Code, rec.Body.String())
			}
			if _, has := raw["code"]; !has {
				t.Errorf("%s: not an error body: %s", name, rec.Body.String())
			}
		}
	})
}

func checkRetryDelay(t *testing.T, out mcpOutcome, window int) {
	t.Helper()
	e, text := toolErrorBody(t, out.body)
	if e.Code != schema.ErrorCodeRateLimited {
		t.Fatalf("tool error = %s, want %s", text, schema.ErrorCodeRateLimited)
	}
	if e.RetryAfterSeconds < 1 || e.RetryAfterSeconds > window {
		t.Errorf("retry_after_seconds = %d, want within 1..%d", e.RetryAfterSeconds, window)
	}
	if want := "retry in " + strconv.Itoa(e.RetryAfterSeconds) + " seconds"; !strings.Contains(e.Message, want) {
		t.Errorf("message %q and retry_after_seconds %d disagree", e.Message, e.RetryAfterSeconds)
	}
}
