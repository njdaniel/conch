package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestModerationClientRoutes(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		call   func(*Client) error
	}{
		{name: "voice kick", method: http.MethodDelete, path: "/v1/channels/team%2Fops/voice/participants/7", call: func(c *Client) error {
			return c.KickVoiceParticipant(context.Background(), "team/ops", 7)
		}},
		{name: "list bans", method: http.MethodGet, path: "/v1/channels/general/bans", call: func(c *Client) error {
			_, err := c.ListChannelBans(context.Background(), "general")
			return err
		}},
		{name: "ban", method: http.MethodPut, path: "/v1/channels/general/bans/7", call: func(c *Client) error {
			return c.BanChannelMember(context.Background(), "general", 7)
		}},
		{name: "unban", method: http.MethodDelete, path: "/v1/channels/general/bans/7", call: func(c *Client) error {
			return c.UnbanChannelMember(context.Background(), "general", 7)
		}},
		{name: "remove message", method: http.MethodDelete, path: "/v2/channels/general/messages/42", call: func(c *Client) error {
			return c.RemoveMessage(context.Background(), "general", 42)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != tt.method || r.URL.EscapedPath() != tt.path {
					t.Errorf("request = %s %s, want %s %s", r.Method, r.URL.EscapedPath(), tt.method, tt.path)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer tok" {
					t.Errorf("Authorization = %q", got)
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || len(body) != 0 {
					t.Errorf("body = %q, %v; want empty", body, err)
				}
				if tt.method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(schema.ListChannelBansResponseV1{Bans: []schema.ChannelBanV1{}})
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.call(client.WithToken("tok")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestModerationClientInputBounds(t *testing.T) {
	client, err := NewClient("http://127.0.0.1:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		call func() error
	}{
		{"voice channel", func() error { return client.KickVoiceParticipant(context.Background(), "", 1) }},
		{"voice id", func() error { return client.KickVoiceParticipant(context.Background(), "general", 0) }},
		{"list channel", func() error { _, err := client.ListChannelBans(context.Background(), ""); return err }},
		{"ban id", func() error { return client.BanChannelMember(context.Background(), "general", -1) }},
		{"unban channel", func() error { return client.UnbanChannelMember(context.Background(), "", 1) }},
		{"message id", func() error { return client.RemoveMessage(context.Background(), "general", 0) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(); err == nil {
				t.Fatal("invalid input was sent")
			}
		})
	}
}

func TestModerationClientPreservesRefusals(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
		call   func(*Client) error
	}{
		{"voice unavailable", http.StatusServiceUnavailable, schema.ErrorCodeVoiceUnavailable, func(c *Client) error {
			return c.KickVoiceParticipant(context.Background(), "general", 7)
		}},
		{"invisible message", http.StatusNotFound, schema.ErrorCodeMessageNotFound, func(c *Client) error {
			return c.RemoveMessage(context.Background(), "general", 42)
		}},
		{"member forbidden", http.StatusForbidden, "forbidden", func(c *Client) error {
			return c.BanChannelMember(context.Background(), "general", 7)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"code":` + strconvQuote(tt.code) + `,"message":"server words"}`))
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			err = tt.call(client)
			var serverErr *ServerError
			if !errors.As(err, &serverErr) || serverErr.Status != tt.status || !IsServerCode(err, tt.code) {
				t.Fatalf("error = %#v, want HTTP %d %s", err, tt.status, tt.code)
			}
		})
	}
}

func TestModerationClientValidatesBanList(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"null list", `{"bans":null}`},
		{"invalid row", `{"bans":[{"principal_id":0,"name":"bad","created_at":"2026-11-06T12:00:00Z"}]}`},
		{"out of order", `{"bans":[{"principal_id":2,"name":"two","created_at":"2026-11-06T12:00:00Z"},{"principal_id":1,"name":"one","created_at":"2026-11-06T12:00:00Z"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			client, err := NewClient(srv.URL, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListChannelBans(context.Background(), "general"); err == nil || !strings.Contains(err.Error(), "invalid channel ban list") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestModerationClientReturnsBanList(t *testing.T) {
	stamp := schema.NewTimestamp(time.Date(2026, time.November, 6, 12, 0, 0, 0, time.UTC))
	want := schema.ListChannelBansResponseV1{Bans: []schema.ChannelBanV1{{PrincipalID: 7, Name: "person", CreatedBy: 2, CreatedAt: stamp}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()
	client, err := NewClient(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ListChannelBans(context.Background(), "general")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Bans) != 1 || got.Bans[0] != want.Bans[0] {
		t.Fatalf("bans = %+v, want %+v", got.Bans, want.Bans)
	}
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
