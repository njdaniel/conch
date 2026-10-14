package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/pkg/schema"
)

var runAt = schema.NewTimestamp(time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC))

func testRun(id int64) schema.RunV1 {
	return schema.RunV1{
		Schema: schema.RunSchemaV1, ID: id, SessionID: 2, PrincipalID: 3, ChannelID: 1,
		State: schema.RunStateRunning, Task: "fix", StartedAt: runAt,
	}
}

func testActivity() schema.AgentActivityV1 {
	return schema.AgentActivityV1{
		Schema: schema.AgentActivitySchemaV1, ID: 8, RunID: 5, PrincipalID: 3,
		Kind: schema.ActivityKindReplyPosted, Summary: "posted", At: runAt,
	}
}

func testIntervention() schema.InterventionV1 {
	return schema.InterventionV1{
		Schema: schema.InterventionSchemaV1, ID: 7, RunID: 5, Kind: schema.InterventionKindCancel,
		State: schema.InterventionStateRequested, RequestedBy: 9, RequestedAt: runAt,
	}
}

type eventsGot struct {
	rosters       []schema.SessionRosterV1
	runs          []schema.RunV1
	activity      []schema.AgentActivityV1
	interventions []schema.InterventionV1
}

func (g *eventsGot) handler() EventHandler {
	return EventHandler{
		Roster:       func(r schema.SessionRosterV1) error { g.rosters = append(g.rosters, r); return nil },
		Run:          func(r schema.RunV1) error { g.runs = append(g.runs, r); return nil },
		Activity:     func(a schema.AgentActivityV1) error { g.activity = append(g.activity, a); return nil },
		Intervention: func(i schema.InterventionV1) error { g.interventions = append(g.interventions, i); return nil },
	}
}

func TestClientSubscribeEventsDocuments(t *testing.T) {
	badRun := testRun(5)
	badRun.State = "sleeping"
	badActivity := testActivity()
	badActivity.Summary = ""
	badIntervention := testIntervention()
	badIntervention.State = schema.InterventionStateFailed // a failure with no reason

	tests := []struct {
		name    string
		frames  []any
		want    eventsGot
		wantErr string
	}{
		{"every document, in order", []any{testRun(5), testActivity(), testIntervention(), testRun(5)},
			eventsGot{runs: []schema.RunV1{testRun(5), testRun(5)}, activity: []schema.AgentActivityV1{testActivity()},
				interventions: []schema.InterventionV1{testIntervention()}}, ""},
		{"an unknown schema is skipped", []any{map[string]any{"schema": "conch.future.v9", "id": 1}, testRun(5)},
			eventsGot{runs: []schema.RunV1{testRun(5)}}, ""},
		{"a frame with no schema is skipped", []any{map[string]any{"id": 1}, testActivity()},
			eventsGot{activity: []schema.AgentActivityV1{testActivity()}}, ""},
		{"a session frame is skipped", []any{testSession(1), testRun(5)}, eventsGot{runs: []schema.RunV1{testRun(5)}}, ""},
		{"an invalid run ends the subscription", []any{badRun, testRun(6)}, eventsGot{}, "invalid run"},
		{"an invalid activity entry ends it", []any{badActivity}, eventsGot{}, "invalid activity"},
		{"an invalid intervention ends it", []any{badIntervention}, eventsGot{}, "invalid intervention"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				for _, f := range tt.frames {
					_ = wsjson.Write(r.Context(), conn, f)
				}
				_ = conn.Close(websocket.StatusNormalClosure, "done")
			}))
			defer server.Close()
			client, _ := NewClient(server.URL, server.Client())
			client.WithToken("tok")
			var got eventsGot
			err := client.SubscribeEvents(context.Background(), "general", 9, got.handler())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %v, want it to mention %q", err, tt.wantErr)
				}
			} else if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Errorf("err = %v, want the server's normal close", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}

	t.Run("a document with no handler is not decoded", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			// Invalid, but nobody asked for runs.
			_ = wsjson.Write(r.Context(), conn, map[string]any{"schema": schema.RunSchemaV1, "id": "x"})
			_ = conn.Close(websocket.StatusNormalClosure, "done")
		}))
		defer server.Close()
		client, _ := NewClient(server.URL, server.Client())
		err := client.SubscribeEvents(context.Background(), "general", 0, EventHandler{})
		if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Errorf("err = %v, want the server's normal close", err)
		}
	})

	t.Run("a handler's error ends it", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = conn.CloseNow() }()
			_ = wsjson.Write(r.Context(), conn, testRun(5))
			<-r.Context().Done()
		}))
		defer server.Close()
		client, _ := NewClient(server.URL, server.Client())
		stop := errors.New("stop")
		err := client.SubscribeEvents(context.Background(), "general", 0, EventHandler{Run: func(schema.RunV1) error { return stop }})
		if !errors.Is(err, stop) {
			t.Errorf("err = %v, want the handler's", err)
		}
	})
}

func TestClientRunReads(t *testing.T) {
	var gotReq []string
	var gotBody schema.CreateInterventionRequestV1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = append(gotReq, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/5":
			_ = json.NewEncoder(w).Encode(testRun(5))
		case r.URL.Path == "/v1/runs/5/activity":
			_ = json.NewEncoder(w).Encode(schema.ListActivityResponseV1{Activity: []schema.AgentActivityV1{testActivity()}, NextAfter: 8})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/5/interventions":
			_ = json.NewEncoder(w).Encode(schema.ListInterventionsResponseV1{Interventions: []schema.InterventionV1{testIntervention()}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/5/interventions":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(testIntervention())
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(schema.Error{Code: schema.ErrorCodeRunNotFound, Message: "no"})
		}
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, server.Client())
	client.WithToken("tok")
	ctx := context.Background()

	run, err := client.GetRun(ctx, 5)
	if err != nil || !reflect.DeepEqual(run, testRun(5)) {
		t.Errorf("GetRun = %+v, %v", run, err)
	}
	page, err := client.ListActivity(ctx, 5, 3, 100)
	if err != nil || len(page.Activity) != 1 || page.NextAfter != 8 {
		t.Errorf("ListActivity = %+v, %v", page, err)
	}
	list, err := client.ListInterventions(ctx, 5, 0, 50)
	if err != nil || len(list.Interventions) != 1 {
		t.Errorf("ListInterventions = %+v, %v", list, err)
	}
	iv, err := client.CreateIntervention(ctx, 5, schema.CreateInterventionRequestV1{Kind: schema.InterventionKindCancel})
	if err != nil || iv.ID != 7 || gotBody.Kind != schema.InterventionKindCancel {
		t.Errorf("CreateIntervention = %+v, %v (sent %+v)", iv, err, gotBody)
	}
	want := []string{
		"GET /v1/runs/5", "GET /v1/runs/5/activity?after=3&limit=100", "GET /v1/runs/5/interventions?after=0&limit=50",
		"POST /v1/runs/5/interventions",
	}
	if !reflect.DeepEqual(gotReq, want) {
		t.Errorf("requests = %v, want %v", gotReq, want)
	}

	var se *ServerError
	if _, err := client.GetRun(ctx, 6); !errors.As(err, &se) || se.Code != schema.ErrorCodeRunNotFound {
		t.Errorf("GetRun of a missing run: %v, want the server's run_not_found", err)
	}
}

func TestClientRunReadsRefuseInvalidDocuments(t *testing.T) {
	badRun := testRun(5)
	badRun.Task = ""
	badActivity := testActivity()
	badActivity.Kind = "nap"
	badIntervention := testIntervention()
	badIntervention.RunID = 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/runs/5":
			_ = json.NewEncoder(w).Encode(badRun)
		case strings.HasSuffix(r.URL.Path, "/activity"):
			_ = json.NewEncoder(w).Encode(schema.ListActivityResponseV1{Activity: []schema.AgentActivityV1{badActivity}})
		case r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(badIntervention)
		default:
			_ = json.NewEncoder(w).Encode(schema.ListInterventionsResponseV1{Interventions: []schema.InterventionV1{badIntervention}})
		}
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, server.Client())
	ctx := context.Background()
	checks := map[string]error{}
	_, checks["run"] = client.GetRun(ctx, 5)
	_, checks["activity"] = client.ListActivity(ctx, 5, 0, 10)
	_, checks["interventions"] = client.ListInterventions(ctx, 5, 0, 10)
	_, checks["created"] = client.CreateIntervention(ctx, 5, schema.CreateInterventionRequestV1{Kind: schema.InterventionKindCancel})
	for name, err := range checks {
		if err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%s: err = %v, want an invalid-document error", name, err)
		}
	}
}
