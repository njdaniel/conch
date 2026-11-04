package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestClientListChannelMembers(t *testing.T) {
	ts := schema.NewTimestamp(time.Date(2026, time.July, 13, 12, 0, 0, 0, time.UTC))
	members := schema.ListChannelMembersResponseV1{Members: []schema.ChannelMemberV1{
		{PrincipalID: 1, CreatedAt: ts, Name: "alice", Kind: schema.PrincipalHuman},
		{PrincipalID: 2, AddedBy: 1, CreatedAt: ts, Name: "bot", Kind: schema.PrincipalAgent},
		{PrincipalID: 3, CreatedAt: ts},
	}}
	tests := []struct {
		name     string
		channel  string
		status   int
		response any
		wantPath string
		want     schema.ListChannelMembersResponseV1
		wantErr  string
	}{
		{name: "members with names and kinds", channel: "general", status: http.StatusOK, response: members,
			wantPath: "/v1/channels/general/members", want: members},
		{name: "channel name is escaped", channel: "a b/c", status: http.StatusOK, response: members,
			wantPath: "/v1/channels/a%20b%2Fc/members", want: members},
		{name: "refused", channel: "general", status: http.StatusForbidden,
			response: schema.Error{Code: "forbidden", Message: "nope"},
			wantPath: "/v1/channels/general/members", wantErr: "forbidden"},
		{name: "unknown channel", channel: "nope", status: http.StatusNotFound,
			response: schema.Error{Code: "channel_not_found", Message: "no such channel"},
			wantPath: "/v1/channels/nope/members", wantErr: "channel_not_found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, got := recordingServer(t, tt.status, tt.response)
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.WithToken("tok").ListChannelMembers(context.Background(), tt.channel)
			if got.method != http.MethodGet || got.path != tt.wantPath || got.auth != "Bearer tok" || got.body != "" {
				t.Errorf("request = %s %s auth %q body %q", got.method, got.path, got.auth, got.body)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !reflect.DeepEqual(result, tt.want) {
				t.Errorf("result = %+v, want %+v", result, tt.want)
			}
		})
	}
}

func TestClientDelegatedAdministration(t *testing.T) {
	yes := true
	roleResponse := schema.RoleChangeResponseV1{
		Principal: schema.PrincipalAdminV1{ID: 7, Kind: schema.PrincipalHuman, Name: "alice", Role: schema.RoleOperator, Owner: true},
		Manages:   []int64{2, 5}, InvitesClosed: 1, CredentialsRevoked: 2, HooksRevoked: 3, LiveCredentials: 4, HooksBound: 5,
	}
	member := schema.ChannelMemberV1{PrincipalID: 7, Name: "alice", Kind: schema.PrincipalHuman}
	tests := []struct {
		name       string
		status     int
		response   any
		call       func(*Client) (any, error)
		wantMethod string
		wantPath   string
		wantBody   string
		want       any
	}{
		{name: "role without an owner change", status: http.StatusOK, response: roleResponse,
			call: func(c *Client) (any, error) {
				return c.ChangePrincipalRole(context.Background(), 7, schema.RoleChangeRequestV1{Role: schema.RoleOperator})
			}, wantMethod: http.MethodPut, wantPath: "/v1/principals/7/role", wantBody: `{"role":"operator"}`, want: roleResponse},
		{name: "role with owner", status: http.StatusOK, response: roleResponse,
			call: func(c *Client) (any, error) {
				return c.ChangePrincipalRole(context.Background(), 7, schema.RoleChangeRequestV1{Role: schema.RoleOperator, Owner: &yes})
			}, wantMethod: http.MethodPut, wantPath: "/v1/principals/7/role", wantBody: `{"role":"operator","owner":true}`, want: roleResponse},
		{name: "add by name", status: http.StatusCreated, response: member,
			call: func(c *Client) (any, error) {
				return c.AddChannelMember(context.Background(), "a b/c", "Alice Liddell")
			},
			wantMethod: http.MethodPost, wantPath: "/v1/channels/a%20b%2Fc/members", wantBody: `{"name":"Alice Liddell"}`, want: member},
		{name: "grant management", status: http.StatusNoContent,
			call: func(c *Client) (any, error) {
				return nil, c.PutChannelMemberManage(context.Background(), "general", 7, true)
			},
			wantMethod: http.MethodPut, wantPath: "/v1/channels/general/members/7", wantBody: `{"manages":true}`},
		{name: "revoke management", status: http.StatusNoContent,
			call: func(c *Client) (any, error) {
				return nil, c.PutChannelMemberManage(context.Background(), "general", 7, false)
			},
			wantMethod: http.MethodPut, wantPath: "/v1/channels/general/members/7", wantBody: `{"manages":false}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, got := recordingServer(t, tt.status, tt.response)
			client, err := NewClient(server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			result, err := tt.call(client.WithToken("tok"))
			if got.method != tt.wantMethod || got.path != tt.wantPath || got.auth != "Bearer tok" || got.body != tt.wantBody {
				t.Errorf("request = %s %s auth %q body %q", got.method, got.path, got.auth, got.body)
			}
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			if !reflect.DeepEqual(result, tt.want) {
				t.Errorf("result = %+v, want %+v", result, tt.want)
			}
		})
	}
}

func TestClientChannelAuthors(t *testing.T) {
	ids := make([]int64, 0, 230)
	for i := int64(1); i <= 230; i++ {
		ids = append(ids, i+10)
	}
	var mu sync.Mutex
	var queries []string
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("ids"))
		paths = append(paths, r.URL.EscapedPath())
		mu.Unlock()
		out := schema.ChannelAuthorsResponseV1{Authors: []schema.ChannelAuthorV1{}}
		for _, part := range strings.Split(r.URL.Query().Get("ids"), ",") {
			id, _ := strconv.ParseInt(part, 10, 64)
			out.Authors = append(out.Authors, schema.ChannelAuthorV1{PrincipalID: id, Name: "p" + part, Kind: schema.PrincipalHuman})
		}
		// A hostile server adds rows nobody asked about, and repeats one.
		out.Authors = append(out.Authors, schema.ChannelAuthorV1{PrincipalID: 7, Name: "impostor", Kind: schema.PrincipalHuman}, out.Authors[0])
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.WithToken("tok").ChannelAuthors(context.Background(), "a b", append(ids, ids[:10]...))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 230 {
		t.Errorf("named %d authors, want 230 (no unasked row, no repeat)", len(got))
	}
	for _, a := range got {
		if a.PrincipalID == 7 {
			t.Error("a row for an id nobody asked about was kept")
		}
	}
	var sizes []int
	for _, q := range queries {
		sizes = append(sizes, len(strings.Split(q, ",")))
	}
	if !reflect.DeepEqual(sizes, []int{100, 100, 30}) {
		t.Errorf("batch sizes = %v, want [100 100 30]", sizes)
	}
	if paths[0] != "/v1/channels/a%20b/authors" {
		t.Errorf("path = %q", paths[0])
	}
}

func TestClientChannelAuthorsKeepsWhatOtherBatchesNamed(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(schema.ChannelAuthorsResponseV1{Authors: []schema.ChannelAuthorV1{{PrincipalID: 1, Name: "one", Kind: schema.PrincipalHuman}}})
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]int64, 150)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	got, err := client.WithToken("tok").ChannelAuthors(context.Background(), "general", ids)
	if err == nil {
		t.Error("the failed batch was not reported")
	}
	if len(got) != 1 || got[0].PrincipalID != 1 {
		t.Errorf("got %v, want the first batch's name", got)
	}
	if none, err := client.WithToken("tok").ChannelAuthors(context.Background(), "general", nil); err != nil || none != nil || calls != 2 {
		t.Errorf("no ids asked the server (%d calls) or failed: %v %v", calls, none, err)
	}
}
