package cli

import (
	"context"
	"net/http"
	"reflect"
	"strings"
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
