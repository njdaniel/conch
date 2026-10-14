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

func testSession(id int64) schema.SessionV1 {
	at := schema.NewTimestamp(time.Date(2026, time.October, 14, 12, 0, 0, 0, time.UTC))
	return schema.SessionV1{
		Schema: schema.SessionSchemaV1, ID: id, PrincipalID: 3, Kind: schema.PrincipalHuman, Name: "nick",
		Client: "tui", State: schema.SessionStateOnline, Status: schema.SessionStatusOnline,
		StartedAt: at, LastSeenAt: at, Interventions: []schema.InterventionKind{},
	}
}

func TestClientCreateAndEndSession(t *testing.T) {
	var gotReq schema.CreateSessionRequestV1
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.Method {
		case http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&gotReq)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(testSession(9))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer server.Close()
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.WithToken("tok")
	got, err := client.CreateSession(context.Background(), schema.CreateSessionRequestV1{Client: "tui"})
	if err != nil || got.ID != 9 {
		t.Fatalf("CreateSession = %+v, %v", got, err)
	}
	if gotReq.Client != "tui" {
		t.Errorf("request = %+v, want client tui", gotReq)
	}
	if err := client.EndSession(context.Background(), 9); err != nil {
		t.Fatalf("EndSession: %v", err)
	}
	if want := []string{"POST /v1/sessions", "DELETE /v1/sessions/9"}; !reflect.DeepEqual(methods, want) {
		t.Errorf("requests = %v, want %v", methods, want)
	}
}

func TestClientCreateSessionRefusals(t *testing.T) {
	invalid := testSession(9)
	invalid.Status = schema.SessionStatusRunning // a human is never running
	tests := []struct {
		name   string
		status int
		body   any
		check  func(error) bool
	}{
		{"auth off", http.StatusBadRequest, schema.Error{Code: schema.ErrorCodeSessionsRequireAuth, Message: "no"},
			func(err error) bool {
				var se *ServerError
				return errors.As(err, &se) && se.Code == schema.ErrorCodeSessionsRequireAuth
			}},
		{"unauthenticated", http.StatusUnauthorized, schema.Error{Code: "unauthenticated"},
			func(err error) bool { return errors.Is(err, ErrUnauthenticated) }},
		{"a session that contradicts itself is not returned", http.StatusCreated, invalid,
			func(err error) bool { return err != nil && strings.Contains(err.Error(), "invalid session") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_ = json.NewEncoder(w).Encode(tt.body)
			}))
			defer server.Close()
			client, _ := NewClient(server.URL, server.Client())
			got, err := client.CreateSession(context.Background(), schema.CreateSessionRequestV1{Client: "tui"})
			if !tt.check(err) {
				t.Errorf("err = %v", err)
			}
			if got.ID != 0 {
				t.Errorf("returned %+v with the error", got)
			}
		})
	}
}

func TestClientListRuns(t *testing.T) {
	tests := []struct {
		name          string
		session       int64
		page          RunsPage
		wantQuery     string
		wantNextAfter int64
	}{
		{"all runs", 0, RunsPage{After: 7}, "after=7&channel=ops&limit=50", 12},
		{"one session's", 4, RunsPage{After: 7}, "after=7&channel=ops&limit=50&session=4", 12},
		{"the newest", 4, RunsPage{NewestFirst: true}, "channel=ops&limit=50&order=desc&session=4", 12},
		{"back from a run", 0, RunsPage{NewestFirst: true, Before: 6}, "before=6&channel=ops&limit=50&order=desc", 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/runs" || r.URL.RawQuery != tt.wantQuery {
					t.Errorf("request = %s?%s, want /v1/runs?%s", r.URL.Path, r.URL.RawQuery, tt.wantQuery)
				}
				_ = json.NewEncoder(w).Encode(schema.ListRunsResponseV1{Runs: []schema.RunV1{}, NextAfter: 12})
			}))
			defer server.Close()
			client, _ := NewClient(server.URL, server.Client())
			got, err := client.ListRuns(context.Background(), "ops", tt.session, tt.page, 50)
			if err != nil || got.NextAfter != tt.wantNextAfter {
				t.Errorf("ListRuns = %+v, %v", got, err)
			}
		})
	}
}

func TestClientSubscribeV2After(t *testing.T) {
	tests := []struct {
		name      string
		call      func(*Client, func(schema.MessageV2) error) error
		wantQuery string
	}{
		{"live only", func(c *Client, f func(schema.MessageV2) error) error {
			return c.SubscribeV2(context.Background(), "general", f)
		}, "channel=general"},
		{"from an id", func(c *Client, f func(schema.MessageV2) error) error {
			return c.SubscribeV2After(context.Background(), "general", 41, nil, f)
		}, "after=41&channel=general"},
		{"from the start", func(c *Client, f func(schema.MessageV2) error) error {
			return c.SubscribeV2After(context.Background(), "general", 0, nil, f)
		}, "after=0&channel=general"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v2/ws" || r.URL.RawQuery != tt.wantQuery {
					t.Errorf("request = %s?%s, want /v2/ws?%s", r.URL.Path, r.URL.RawQuery, tt.wantQuery)
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer func() { _ = conn.CloseNow() }()
				_ = wsjson.Write(r.Context(), conn, schema.MessageV2{Schema: schema.MessageSchemaV2, ID: 42})
			}))
			defer server.Close()
			client, _ := NewClient(server.URL, server.Client())
			stop := errors.New("stop")
			var got int64
			err := tt.call(client, func(m schema.MessageV2) error { got = m.ID; return stop })
			if !errors.Is(err, stop) || got != 42 {
				t.Errorf("err = %v, id = %d", err, got)
			}
		})
	}
}

func TestClientSubscribeEvents(t *testing.T) {
	agent := testSession(2)
	agent.Kind, agent.Name = schema.PrincipalAgent, "worker"
	agent.Status, agent.CurrentRun, agent.Task = schema.SessionStatusRunning, 41, "fix"
	roster := schema.SessionRosterV1{Schema: schema.SessionRosterSchemaV1, ChannelID: 1, Sessions: []schema.SessionV1{testSession(1), agent}}
	contradictory := roster
	contradictory.Sessions = []schema.SessionV1{testSession(1), testSession(1)} // one id twice

	tests := []struct {
		name      string
		session   int64
		frames    []any
		wantQuery string
		wantGot   []schema.SessionRosterV1
		wantErr   string
	}{
		{"a roster, with a session", 9, []any{roster}, "channel=general&session=9", []schema.SessionRosterV1{roster}, ""},
		{"a roster, without one", 0, []any{roster}, "channel=general", []schema.SessionRosterV1{roster}, ""},
		{"other documents are skipped", 9, []any{
			schema.RunV1{Schema: schema.RunSchemaV1, ID: 5}, roster, schema.AgentActivityV1{Schema: schema.AgentActivitySchemaV1},
		}, "channel=general&session=9", []schema.SessionRosterV1{roster}, ""},
		{"a roster that contradicts itself ends the subscription", 9, []any{contradictory, roster},
			"channel=general&session=9", nil, "invalid session roster"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/events/ws" || r.URL.RawQuery != tt.wantQuery {
					t.Errorf("request = %s?%s, want /v1/events/ws?%s", r.URL.Path, r.URL.RawQuery, tt.wantQuery)
				}
				if r.Header.Get("Authorization") != "Bearer tok" {
					t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
				}
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
			var got []schema.SessionRosterV1
			err := client.SubscribeEvents(context.Background(), "general", tt.session, EventHandler{Roster: func(r schema.SessionRosterV1) error {
				got = append(got, r)
				return nil
			}})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("err = %v, want it to mention %q", err, tt.wantErr)
				}
			} else if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
				t.Errorf("err = %v, want the server's normal close", err)
			}
			if !reflect.DeepEqual(got, tt.wantGot) {
				t.Errorf("rosters = %+v, want %+v", got, tt.wantGot)
			}
		})
	}

	t.Run("a refusal is the server's error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(schema.Error{Code: schema.ErrorCodeSessionNotFound, Message: "no"})
		}))
		defer server.Close()
		client, _ := NewClient(server.URL, server.Client())
		err := client.SubscribeEvents(context.Background(), "general", 9, EventHandler{Roster: func(schema.SessionRosterV1) error { return nil }})
		var se *ServerError
		if !errors.As(err, &se) || se.Status != http.StatusNotFound || se.Code != schema.ErrorCodeSessionNotFound {
			t.Errorf("err = %v, want the 404 session_not_found", err)
		}
	})
}
