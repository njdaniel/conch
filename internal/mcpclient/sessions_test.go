package mcpclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// recordedCall is the last tools/call the server saw.
type recordedCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func recordingServer(t *testing.T, structured string, got *recordedCall) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Params recordedCall `json:"params"`
		}
		_ = json.Unmarshal(body, &req)
		*got = req.Params
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"structuredContent":`+structured+`}}`)
	}))
}

const (
	sessionDoc = `{"schema":"conch.session.v1","id":7,"principal_id":3,"kind":"agent","name":"bot","client":"conch-adapter","state":"online","status":"idle","started_at":"2026-10-14T12:00:00.000Z","last_seen_at":"2026-10-14T12:00:00.000Z","interventions":["message","cancel"],"outcome_unknown":false}`
	runDoc     = `{"schema":"conch.run.v1","id":9,"session_id":7,"principal_id":3,"channel_id":2,"message_id":5,"state":"running","task":"t","started_at":"2026-10-14T12:00:00.000Z"}`
	activity   = `{"schema":"conch.agent.activity.v1","id":1,"run_id":9,"principal_id":3,"kind":"command_started","summary":"s","at":"2026-10-14T12:00:00.000Z"}`
	ivDoc      = `{"schema":"conch.intervention.v1","id":4,"run_id":9,"kind":"message","body":"b","state":"requested","requested_by":1,"requested_at":"2026-10-14T12:00:00.000Z"}`
)

// Each typed method calls its tool with the arguments the schema request
// encodes, plus the id the request does not carry.
func TestSessionToolsSendSchemaShapes(t *testing.T) {
	ctx := context.Background()
	deadline := schema.NewTimestamp(time.Date(2026, 10, 14, 13, 0, 0, 0, time.UTC))
	tests := []struct {
		name     string
		result   string
		call     func(*Client) error
		wantTool string
		wantArgs map[string]any
	}{
		{"register_session", sessionDoc, func(c *Client) error {
			_, err := c.RegisterSession(ctx, schema.CreateSessionRequestV1{Client: "conch-adapter", Runtime: "claude-code", Model: "sonnet", Host: "h", Interventions: []schema.InterventionKind{"message", "cancel"}})
			return err
		}, "register_session", map[string]any{"client": "conch-adapter", "runtime": "claude-code", "model": "sonnet", "host": "h", "interventions": []any{"message", "cancel"}}},
		{"heartbeat", sessionDoc, func(c *Client) error { _, err := c.Heartbeat(ctx, 7); return err }, "heartbeat", map[string]any{"session_id": float64(7)}},
		{"end_session", `null`, func(c *Client) error { return c.EndSession(ctx, 7) }, "end_session", map[string]any{"session_id": float64(7)}},
		{"start_run", runDoc, func(c *Client) error {
			_, err := c.StartRun(ctx, 7, schema.CreateRunRequestV1{Channel: "ops", MessageID: 5, Task: "t"})
			return err
		}, "start_run", map[string]any{"session_id": float64(7), "channel": "ops", "message_id": float64(5), "task": "t"}},
		{"update_run", runDoc, func(c *Client) error {
			_, err := c.UpdateRun(ctx, 9, schema.SetRunStateRequestV1{State: schema.RunStateRunning})
			return err
		}, "update_run", map[string]any{"run_id": float64(9), "state": "running"}},
		{"post_activity", activity, func(c *Client) error {
			_, err := c.PostActivity(ctx, 9, schema.AppendActivityRequestV1{Kind: schema.ActivityKindCommandStarted, Summary: "s", Data: json.RawMessage(`{"n":1}`)})
			return err
		}, "post_activity", map[string]any{"run_id": float64(9), "kind": "command_started", "summary": "s", "data": map[string]any{"n": float64(1)}}},
		{"poll_interventions", `{"interventions":[` + ivDoc + `]}`, func(c *Client) error {
			_, err := c.PollInterventions(ctx, 7, schema.InterventionStateRequested, 3)
			return err
		}, "poll_interventions", map[string]any{"session_id": float64(7), "state": "requested", "after": float64(3)}},
		{"poll_interventions without a cursor", `{"interventions":[]}`, func(c *Client) error {
			_, err := c.PollInterventions(ctx, 7, "", 0)
			return err
		}, "poll_interventions", map[string]any{"session_id": float64(7)}},
		{"ack_intervention", ivDoc, func(c *Client) error {
			_, err := c.AnswerIntervention(ctx, 4, schema.AckInterventionRequestV1{State: schema.InterventionStateApplied})
			return err
		}, "ack_intervention", map[string]any{"intervention_id": float64(4), "state": "applied"}},
		{"request_approval does not send the requester", `{"id":41,"title":"T","state":"pending","deadline":"2026-10-14T13:00:00.000Z"}`, func(c *Client) error {
			_, err := c.RequestApproval(ctx, schema.CreateApprovalRequestV1{
				RequesterID: 3, ChannelID: 2, Title: "T", Body: "B", Deadline: deadline,
				Options: []schema.Option{{ID: "a", Label: "A", Kind: schema.OptionKindApprove}, {ID: "r", Label: "R", Kind: schema.OptionKindReject}},
			})
			return err
		}, "request_approval", map[string]any{
			"channel_id": float64(2), "title": "T", "body": "B", "deadline": "2026-10-14T13:00:00.000Z",
			"options": []any{map[string]any{"id": "a", "label": "A", "kind": "approve"}, map[string]any{"id": "r", "label": "R", "kind": "reject"}},
		}},
		{"await_decision", `{"state":"pending","effective_timeout_ms":1000}`, func(c *Client) error {
			_, err := c.AwaitDecision(ctx, 41, 1000)
			return err
		}, "await_decision", map[string]any{"approval_id": float64(41), "timeout_ms": float64(1000)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got recordedCall
			srv := recordingServer(t, tt.result, &got)
			defer srv.Close()
			if err := tt.call(New(srv.URL, "t")); err != nil {
				t.Fatal(err)
			}
			if got.Name != tt.wantTool || !reflect.DeepEqual(got.Arguments, tt.wantArgs) {
				t.Fatalf("called %s with %v, want %s with %v", got.Name, got.Arguments, tt.wantTool, tt.wantArgs)
			}
		})
	}
}

// A document the schema refuses is an error, not something to act on; so is a
// request the schema refuses, before anything is sent.
func TestSessionToolsValidate(t *testing.T) {
	ctx := context.Background()
	t.Run("results", func(t *testing.T) {
		tests := []struct {
			name   string
			result string
			call   func(*Client) error
		}{
			{"a session with the wrong schema", strings.Replace(sessionDoc, "conch.session.v1", "conch.run.v1", 1), func(c *Client) error { _, err := c.Heartbeat(ctx, 7); return err }},
			{"a run in no state", strings.Replace(runDoc, `"running"`, `"flying"`, 1), func(c *Client) error {
				_, err := c.UpdateRun(ctx, 9, schema.SetRunStateRequestV1{State: schema.RunStateRunning})
				return err
			}},
			{"an activity entry with no time", strings.Replace(activity, `"at":"2026-10-14T12:00:00.000Z"`, `"at":null`, 1), func(c *Client) error {
				_, err := c.PostActivity(ctx, 9, schema.AppendActivityRequestV1{Kind: schema.ActivityKindCommandStarted, Summary: "s"})
				return err
			}},
			{"an intervention in a list that is invalid", `{"interventions":[` + strings.Replace(ivDoc, `"kind":"message"`, `"kind":"reboot"`, 1) + `]}`, func(c *Client) error {
				_, err := c.PollInterventions(ctx, 7, "", 0)
				return err
			}},
			{"an approval with no id", `{"id":0,"title":"T","state":"pending"}`, func(c *Client) error {
				_, err := c.RequestApproval(ctx, schema.CreateApprovalRequestV1{
					RequesterID: 3, ChannelID: 2, Title: "T", Body: "B", Deadline: schema.NewTimestamp(time.Now().Add(time.Hour)),
					Options: []schema.Option{{ID: "a", Label: "A", Kind: schema.OptionKindApprove}, {ID: "r", Label: "R", Kind: schema.OptionKindReject}},
				})
				return err
			}},
			{"a decision in no state", `{"state":"maybe","effective_timeout_ms":1}`, func(c *Client) error { _, err := c.AwaitDecision(ctx, 1, 1); return err }},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				var got recordedCall
				srv := recordingServer(t, tt.result, &got)
				defer srv.Close()
				if err := tt.call(New(srv.URL, "t")); err == nil {
					t.Fatal("no error")
				}
			})
		}
	})
	t.Run("requests are refused before they are sent", func(t *testing.T) {
		var got recordedCall
		srv := recordingServer(t, runDoc, &got)
		defer srv.Close()
		c := New(srv.URL, "t")
		for name, call := range map[string]func() error{
			"a run with no task":          func() error { _, err := c.StartRun(ctx, 7, schema.CreateRunRequestV1{Channel: "ops"}); return err },
			"a run state that is unknown": func() error { _, err := c.UpdateRun(ctx, 9, schema.SetRunStateRequestV1{State: "flying"}); return err },
			"a multi-line summary": func() error {
				_, err := c.PostActivity(ctx, 9, schema.AppendActivityRequestV1{Kind: schema.ActivityKindCommandStarted, Summary: "a\nb"})
				return err
			},
			"a failed answer with no reason": func() error {
				_, err := c.AnswerIntervention(ctx, 4, schema.AckInterventionRequestV1{State: schema.InterventionStateFailed})
				return err
			},
		} {
			if err := call(); err == nil {
				t.Errorf("%s: no error", name)
			}
		}
		if got.Name != "" {
			t.Fatalf("a request was sent: %+v", got)
		}
	})
}
