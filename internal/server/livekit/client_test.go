package livekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// recorded is what a fake LiveKit saw of one request.
type recorded struct {
	path, auth, contentType, method string
	body                            map[string]any
}

// fakeLiveKit answers every request with status and body and records it.
func fakeLiveKit(t *testing.T, status int, body string) (*httptest.Server, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path, rec.auth, rec.method = r.URL.Path, r.Header.Get("Authorization"), r.Method
		rec.contentType = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &rec.body)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// checkAdminToken verifies the bearer token and that its video grant is
// exactly want, with no sub and a one-minute life.
func checkAdminToken(t *testing.T, rec *recorded, want map[string]any) {
	t.Helper()
	tok, ok := strings.CutPrefix(rec.auth, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want a bearer token", rec.auth)
	}
	_, claims := decodeJWT(t, tok, "s3cret-value")
	if claims["iss"] != "devkey" {
		t.Errorf("iss = %v", claims["iss"])
	}
	if _, has := claims["sub"]; has {
		t.Errorf("admin token has sub %v", claims["sub"])
	}
	if got := claims["exp"].(float64) - float64(testNow.Unix()); got != 60 {
		t.Errorf("token life = %vs, want 60", got)
	}
	// Every admin grant also writes the three participant permissions as
	// false, since LiveKit reads an absent one as true.
	full := map[string]any{"canPublish": false, "canSubscribe": false, "canPublishData": false}
	for k, v := range want {
		full[k] = v
	}
	if !reflect.DeepEqual(claims["video"], full) {
		t.Errorf("video grant = %v, want %v", claims["video"], full)
	}
}

func TestRoomCalls(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		response  string
		call      func(*Client) (any, error)
		wantBody  map[string]any
		wantGrant map[string]any
		want      any
	}{
		{
			name: "CreateRoom", method: "CreateRoom", response: `{"sid":"RM_x","name":"conch-a"}`,
			call:      func(c *Client) (any, error) { return nil, c.CreateRoom(context.Background(), "conch-a") },
			wantBody:  map[string]any{"name": "conch-a"},
			wantGrant: map[string]any{"roomCreate": true},
		},
		{
			name: "ListRooms", method: "ListRooms",
			response:  `{"rooms":[{"sid":"RM_1","name":"conch-a","numParticipants":2},{"sid":"RM_2","name":"conch-b"}]}`,
			call:      func(c *Client) (any, error) { return c.ListRooms(context.Background()) },
			wantBody:  map[string]any{},
			wantGrant: map[string]any{"roomList": true},
			want:      []Room{{Name: "conch-a", NumParticipants: 2}, {Name: "conch-b", NumParticipants: 0}},
		},
		{
			name: "ListRooms snake case", method: "ListRooms",
			response:  `{"rooms":[{"name":"conch-a","num_participants":3}]}`,
			call:      func(c *Client) (any, error) { return c.ListRooms(context.Background()) },
			wantBody:  map[string]any{},
			wantGrant: map[string]any{"roomList": true},
			want:      []Room{{Name: "conch-a", NumParticipants: 3}},
		},
		{
			name: "ListRooms empty", method: "ListRooms", response: `{}`,
			call:      func(c *Client) (any, error) { return c.ListRooms(context.Background()) },
			wantBody:  map[string]any{},
			wantGrant: map[string]any{"roomList": true},
			want:      []Room{},
		},
		{
			name: "ListParticipants string enums", method: "ListParticipants",
			response: `{"participants":[
				{"identity":"p1","joinedAt":"1791547200","joinedAtMs":"1791547200123","tracks":[{"sid":"TR_1","type":"AUDIO","source":"MICROPHONE"}]},
				{"identity":"p2","joinedAt":"1791547300","tracks":[{"type":"AUDIO","source":"MICROPHONE","muted":true}]},
				{"identity":"p3","tracks":[{"type":"VIDEO","source":"CAMERA"}]},
				{"identity":"p4"}]}`,
			call:      func(c *Client) (any, error) { return c.ListParticipants(context.Background(), "conch-a") },
			wantBody:  map[string]any{"room": "conch-a"},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want: []Participant{
				{Identity: "p1", JoinedAt: time.UnixMilli(1791547200123).UTC(), MicrophonePublished: true},
				{Identity: "p2", JoinedAt: time.Unix(1791547300, 0).UTC(), MicrophonePublished: true, MicrophoneMuted: true},
				{Identity: "p3"},
				{Identity: "p4"},
			},
		},
		{
			// protojson omits default values: AUDIO (0) and muted=false vanish.
			name: "ListParticipants numeric and omitted enums", method: "ListParticipants",
			response: `{"participants":[
				{"identity":"p1","joined_at":1791547200,"tracks":[{"source":2}]},
				{"identity":"p2","tracks":[{"type":0,"source":2,"muted":true},{"type":1,"source":1}]},
				{"identity":"p3","tracks":[{"type":2,"source":2}]},
				{"identity":"p4","tracks":[{"type":0,"source":2,"muted":true},{"source":2}]}]}`,
			call:      func(c *Client) (any, error) { return c.ListParticipants(context.Background(), "conch-a") },
			wantBody:  map[string]any{"room": "conch-a"},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want: []Participant{
				{Identity: "p1", JoinedAt: time.Unix(1791547200, 0).UTC(), MicrophonePublished: true},
				{Identity: "p2", MicrophonePublished: true, MicrophoneMuted: true},
				{Identity: "p3"},
				{Identity: "p4", MicrophonePublished: true},
			},
		},
		{
			name: "DeleteRoom", method: "DeleteRoom", response: `{}`,
			call:      func(c *Client) (any, error) { return nil, c.DeleteRoom(context.Background(), "conch-a") },
			wantBody:  map[string]any{"room": "conch-a"},
			wantGrant: map[string]any{"roomCreate": true},
		},
		{
			name: "RemoveParticipant", method: "RemoveParticipant", response: `{}`,
			call:      func(c *Client) (any, error) { return nil, c.RemoveParticipant(context.Background(), "conch-a", "p7") },
			wantBody:  map[string]any{"room": "conch-a", "identity": "p7"},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
		},
		{
			// The answer LiveKit 1.13.7 gave for a sharer (issue #443), cut
			// to the fields that matter: a screen-share track, its sound,
			// a microphone, and the permission the claim set.
			name: "ListParticipants with a screen share, as 1.13.7 answers", method: "ListParticipants",
			response: `{"participants":[{"sid":"PA_b9hhqMbkD4nb","identity":"p1","state":"ACTIVE","tracks":[
				{"sid":"TR_AsNBahxDLSoR5e","type":"AUDIO","name":"screen-audio","muted":false,"source":"SCREEN_SHARE_AUDIO","stream":"screen"},
				{"sid":"TR_AMkWnC56BPQM7j","type":"AUDIO","name":"mic","muted":true,"source":"MICROPHONE","stream":"camera"},
				{"sid":"TR_VSczCNkHQB7ACE","type":"VIDEO","name":"demo","muted":false,"source":"SCREEN_SHARE","stream":"screen"}],
				"joined_at":"1792188148","joined_at_ms":"1792188148934",
				"permission":{"can_subscribe":true,"can_publish":true,"can_publish_data":false,"can_publish_sources":["MICROPHONE","SCREEN_SHARE","SCREEN_SHARE_AUDIO"],"hidden":false,"recorder":false,"can_update_metadata":false,"agent":false,"can_subscribe_metrics":false,"can_manage_agent_session":false},
				"is_publisher":true,"kind":"STANDARD"}]}`,
			call:      func(c *Client) (any, error) { return c.ListParticipants(context.Background(), "conch-a") },
			wantBody:  map[string]any{"room": "conch-a"},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want: []Participant{{
				Identity: "p1", JoinedAt: time.UnixMilli(1792188148934).UTC(),
				MicrophonePublished: true, MicrophoneMuted: true,
				ScreenSharePublished: true, ScreenShareAudioPublished: true, ShareSourcesGranted: true, CanPublish: true, CanSubscribe: true,
			}},
		},
		{
			// Whether a participant may receive, in either spelling; false
			// when it does not or when no permission is reported.
			name: "ListParticipants subscribe permission", method: "ListParticipants",
			response: `{"participants":[
				{"identity":"p1.screen","permission":{"can_subscribe":false,"can_publish":true}},
				{"identity":"p2.screen","permission":{"can_subscribe":true,"can_publish":true}},
				{"identity":"p3.screen","permission":{"canSubscribe":true}},
				{"identity":"p4.screen"}]}`,
			call:      func(c *Client) (any, error) { return c.ListParticipants(context.Background(), "conch-a") },
			wantBody:  map[string]any{"room": "conch-a"},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want: []Participant{
				{Identity: "p1.screen", ShareSourcesGranted: true, CanPublish: true},
				{Identity: "p2.screen", ShareSourcesGranted: true, CanPublish: true, CanSubscribe: true},
				{Identity: "p3.screen", CanSubscribe: true},
				{Identity: "p4.screen"},
			},
		},
		{
			// What a share's permission can look like: granted by name, by
			// number, in camel case, or by an empty list (LiveKit reads it as
			// every source; 1.13.7 lists a token that names no sources so);
			// not granted when publishing is off, when only the microphone
			// is named, or when no permission is reported. A share source
			// counts whatever the track's type.
			name: "ListParticipants permissions and share sources", method: "ListParticipants",
			response: `{"participants":[
				{"identity":"p1","permission":{"can_publish":true,"can_publish_sources":["MICROPHONE"]}},
				{"identity":"p2","permission":{"can_publish":true,"can_publish_sources":[]}},
				{"identity":"p3","permission":{"canPublish":true,"canPublishSources":["SCREEN_SHARE_AUDIO"]}},
				{"identity":"p4","permission":{"can_publish":true,"can_publish_sources":[2,3]}},
				{"identity":"p5","permission":{"can_publish":false,"can_publish_sources":["SCREEN_SHARE"]}},
				{"identity":"p6","tracks":[{"type":"AUDIO","source":"SCREEN_SHARE"},{"type":1,"source":4}]},
				{"identity":"p7","permission":{"can_publish":true}}]}`,
			call:      func(c *Client) (any, error) { return c.ListParticipants(context.Background(), "conch-a") },
			wantBody:  map[string]any{"room": "conch-a"},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want: []Participant{
				{Identity: "p1", CanPublish: true},
				{Identity: "p2", ShareSourcesGranted: true, CanPublish: true},
				{Identity: "p3", ShareSourcesGranted: true, CanPublish: true},
				{Identity: "p4", ShareSourcesGranted: true, CanPublish: true},
				{Identity: "p5"},
				{Identity: "p6", ScreenSharePublished: true, ScreenShareAudioPublished: true},
				{Identity: "p7", ShareSourcesGranted: true, CanPublish: true},
			},
		},
		{
			// The request conchd sends to grant a share. Every field of the
			// permission is written out: LiveKit replaces it as a whole.
			name: "UpdateParticipant granting the share", method: "UpdateParticipant",
			response: `{"sid":"PA_b9hhqMbkD4nb","identity":"p7","state":"ACTIVE","permission":{"can_publish":true}}`,
			call: func(c *Client) (any, error) {
				return c.UpdateParticipant(context.Background(), "conch-a", "p7", Permission{CanPublish: true, ScreenShare: true})
			},
			wantBody: map[string]any{"room": "conch-a", "identity": "p7", "permission": map[string]any{
				"can_subscribe": true, "can_publish": true, "can_publish_data": false,
				"can_publish_sources": []any{"MICROPHONE", "SCREEN_SHARE", "SCREEN_SHARE_AUDIO"},
				"hidden":              false, "recorder": false, "can_update_metadata": false, "agent": false,
				"can_subscribe_metrics": false, "can_manage_agent_session": false,
			}},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want:      true,
		},
		{
			// And to withdraw it: exactly what the join token granted.
			name: "UpdateParticipant withdrawing the share", method: "UpdateParticipant",
			response: `{"identity":"p7"}`,
			call: func(c *Client) (any, error) {
				return c.UpdateParticipant(context.Background(), "conch-a", "p7", Permission{CanPublish: true})
			},
			wantBody: map[string]any{"room": "conch-a", "identity": "p7", "permission": map[string]any{
				"can_subscribe": true, "can_publish": true, "can_publish_data": false,
				"can_publish_sources": []any{"MICROPHONE"},
				"hidden":              false, "recorder": false, "can_update_metadata": false, "agent": false,
				"can_subscribe_metrics": false, "can_manage_agent_session": false,
			}},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want:      true,
		},
		{
			// The per-source form (issue #932): a companion's grant, never the
			// microphone-only bundle.
			name: "UpdateParticipant per source, screen only", method: "UpdateParticipant",
			response: `{"identity":"p7.screen"}`,
			call: func(c *Client) (any, error) {
				return c.UpdateParticipant(context.Background(), "conch-a", "p7.screen", SourcePermission(true, true, SourceScreenShare))
			},
			wantBody: map[string]any{"room": "conch-a", "identity": "p7.screen", "permission": map[string]any{
				"can_subscribe": true, "can_publish": true, "can_publish_data": false,
				"can_publish_sources": []any{"SCREEN_SHARE"},
				"hidden":              false, "recorder": false, "can_update_metadata": false, "agent": false,
				"can_subscribe_metrics": false, "can_manage_agent_session": false,
			}},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want:      true,
		},
		{
			// The per-source form (issue #932): a companion's grant, never the
			// microphone-only bundle.
			name: "UpdateParticipant per source, screen and its sound", method: "UpdateParticipant",
			response: `{"identity":"p7.screen"}`,
			call: func(c *Client) (any, error) {
				return c.UpdateParticipant(context.Background(), "conch-a", "p7.screen", SourcePermission(true, true, SourceScreenShare, SourceScreenShareAudio))
			},
			wantBody: map[string]any{"room": "conch-a", "identity": "p7.screen", "permission": map[string]any{
				"can_subscribe": true, "can_publish": true, "can_publish_data": false,
				"can_publish_sources": []any{"SCREEN_SHARE", "SCREEN_SHARE_AUDIO"},
				"hidden":              false, "recorder": false, "can_update_metadata": false, "agent": false,
				"can_subscribe_metrics": false, "can_manage_agent_session": false,
			}},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want:      true,
		},
		{
			// The per-source form (issue #932): a companion's grant, never the
			// microphone-only bundle.
			name: "UpdateParticipant per source, nothing", method: "UpdateParticipant",
			response: `{"identity":"p7.screen"}`,
			call: func(c *Client) (any, error) {
				return c.UpdateParticipant(context.Background(), "conch-a", "p7.screen", SourcePermission(true, false))
			},
			wantBody: map[string]any{"room": "conch-a", "identity": "p7.screen", "permission": map[string]any{
				"can_subscribe": true, "can_publish": false, "can_publish_data": false,
				"can_publish_sources": []any{},
				"hidden":              false, "recorder": false, "can_update_metadata": false, "agent": false,
				"can_subscribe_metrics": false, "can_manage_agent_session": false,
			}},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want:      true,
		},
		{
			// A publish-only companion stays publish-only: the grant writes
			// can_subscribe false, not the person's true.
			name: "UpdateParticipant per source, not subscribing", method: "UpdateParticipant",
			response: `{"identity":"p7.screen"}`,
			call: func(c *Client) (any, error) {
				return c.UpdateParticipant(context.Background(), "conch-a", "p7.screen", SourcePermission(false, true, SourceScreenShare))
			},
			wantBody: map[string]any{"room": "conch-a", "identity": "p7.screen", "permission": map[string]any{
				"can_subscribe": false, "can_publish": true, "can_publish_data": false,
				"can_publish_sources": []any{"SCREEN_SHARE"},
				"hidden":              false, "recorder": false, "can_update_metadata": false, "agent": false,
				"can_subscribe_metrics": false, "can_manage_agent_session": false,
			}},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want:      true,
		},
		{
			name: "UpdateParticipant per source, withdrawing a publish-only companion", method: "UpdateParticipant",
			response: `{"identity":"p7.screen"}`,
			call: func(c *Client) (any, error) {
				return c.UpdateParticipant(context.Background(), "conch-a", "p7.screen", SourcePermission(false, false))
			},
			wantBody: map[string]any{"room": "conch-a", "identity": "p7.screen", "permission": map[string]any{
				"can_subscribe": false, "can_publish": false, "can_publish_data": false,
				"can_publish_sources": []any{},
				"hidden":              false, "recorder": false, "can_update_metadata": false, "agent": false,
				"can_subscribe_metrics": false, "can_manage_agent_session": false,
			}},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
			want:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := fakeLiveKit(t, http.StatusOK, tt.response)
			c := testClient(t, srv.URL)
			got, err := tt.call(c)
			if err != nil {
				t.Fatal(err)
			}
			if rec.method != http.MethodPost || rec.path != "/twirp/livekit.RoomService/"+tt.method {
				t.Errorf("request = %s %s", rec.method, rec.path)
			}
			if rec.contentType != "application/json" {
				t.Errorf("Content-Type = %q", rec.contentType)
			}
			if !reflect.DeepEqual(rec.body, tt.wantBody) {
				t.Errorf("body = %v, want %v", rec.body, tt.wantBody)
			}
			checkAdminToken(t, rec, tt.wantGrant)
			if tt.want != nil {
				if p, ok := got.([]Participant); ok {
					for i := range p {
						if !p[i].JoinedAt.Equal(tt.want.([]Participant)[i].JoinedAt) {
							t.Errorf("participant %d joined %v, want %v", i, p[i].JoinedAt, tt.want.([]Participant)[i].JoinedAt)
						}
						p[i].JoinedAt = tt.want.([]Participant)[i].JoinedAt
					}
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("result = %+v, want %+v", got, tt.want)
				}
			}
		})
	}
}

func TestParticipantTransmitting(t *testing.T) {
	tests := []struct {
		p    Participant
		want bool
	}{
		{Participant{}, false},
		{Participant{MicrophonePublished: true}, true},
		{Participant{MicrophonePublished: true, MicrophoneMuted: true}, false},
	}
	for _, tt := range tests {
		if got := tt.p.Transmitting(); got != tt.want {
			t.Errorf("%+v.Transmitting() = %v", tt.p, got)
		}
	}
}

// failureCases drives each way LiveKit can fail to answer.
func failureCases(t *testing.T) map[string]string {
	t.Helper()
	cases := map[string]string{}
	for name, status := range map[string]int{"401": 401, "404": 404, "500": 500, "302": 302} {
		srv, _ := fakeLiveKit(t, status, `{"code":"unauthenticated","msg":"leaky token detail"}`)
		cases["HTTP "+name] = srv.URL
	}
	bad, _ := fakeLiveKit(t, 200, `{not json`)
	cases["malformed JSON"] = bad.URL
	wrongType, _ := fakeLiveKit(t, 200, `{"rooms":"nope","participants":7}`)
	cases["wrong JSON types"] = wrongType.URL
	closed := httptest.NewServer(http.NotFoundHandler())
	cases["connection refused"] = closed.URL
	closed.Close()
	return cases
}

func TestFailuresAreUnavailable(t *testing.T) {
	ops := map[string]func(*Client) error{
		"CreateRoom":        func(c *Client) error { return c.CreateRoom(context.Background(), "r") },
		"ListRooms":         func(c *Client) error { _, err := c.ListRooms(context.Background()); return err },
		"ListParticipants":  func(c *Client) error { _, err := c.ListParticipants(context.Background(), "r"); return err },
		"RemoveParticipant": func(c *Client) error { return c.RemoveParticipant(context.Background(), "r", "p1") },
		"DeleteRoom":        func(c *Client) error { return c.DeleteRoom(context.Background(), "r") },
		"UpdateParticipant": func(c *Client) error {
			_, err := c.UpdateParticipant(context.Background(), "r", "p1", Permission{CanPublish: true})
			return err
		},
	}
	for caseName, url := range failureCases(t) {
		for opName, op := range ops {
			if caseName == "malformed JSON" && (opName == "RemoveParticipant" || opName == "DeleteRoom") {
				continue // it ignores the body, so there is nothing to be malformed
			}
			if caseName == "wrong JSON types" && (opName == "RemoveParticipant" || opName == "DeleteRoom") {
				continue
			}
			t.Run(caseName+"/"+opName, func(t *testing.T) {
				c := testClient(t, url)
				err := op(c)
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("err = %v, want ErrUnavailable", err)
				}
				if strings.Contains(err.Error(), "leaky") || strings.Contains(err.Error(), url) {
					t.Errorf("error leaks response body or address: %v", err)
				}
				if !strings.Contains(err.Error(), opName) {
					t.Errorf("error %q does not name the method", err)
				}
			})
		}
	}
}

func TestStatusIsNamedAndRefusedHasCause(t *testing.T) {
	srv, _ := fakeLiveKit(t, 503, ``)
	err := testClient(t, srv.URL).CreateRoom(context.Background(), "r")
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Errorf("err = %v, want it to name HTTP 503", err)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	err = testClient(t, closed.URL).CreateRoom(context.Background(), "r")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("refused: err = %v", err)
	}
	var tmp interface{ Unwrap() []error }
	if !errors.As(err, &tmp) || len(tmp.Unwrap()) != 2 {
		t.Errorf("refused error should wrap ErrUnavailable and the cause: %v", err)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer target.Close()
	redir := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer redir.Close()
	err := testClient(t, redir.URL).CreateRoom(context.Background(), "r")
	if !errors.Is(err, ErrUnavailable) || followed {
		t.Errorf("err = %v, followed = %v", err, followed)
	}
}

func TestContextDeadlineHonoured(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)

	t.Run("caller deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		err := testClient(t, srv.URL).CreateRoom(ctx, "r")
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ErrUnavailable wrapping DeadlineExceeded", err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("took %v", d)
		}
	})
	t.Run("default timeout when the context has none", func(t *testing.T) {
		c := testClient(t, srv.URL)
		if c.timeout != 2*time.Second {
			t.Fatalf("default timeout = %v, want 2s", c.timeout)
		}
		c.timeout = 50 * time.Millisecond // shortened so the test is fast
		err := c.CreateRoom(context.Background(), "r")
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want ErrUnavailable wrapping DeadlineExceeded", err)
		}
	})
	t.Run("caller deadline longer than default is kept", func(t *testing.T) {
		srv2, _ := fakeLiveKit(t, 200, `{"name":"r"}`)
		c := testClient(t, srv2.URL)
		c.timeout = time.Nanosecond
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.CreateRoom(ctx, "r"); err != nil {
			t.Fatalf("err = %v; the default must not override a caller deadline", err)
		}
	})
}

// TestNoSecretOrTokenInLogsOrErrors drives failing calls with every log level
// captured and asserts neither the secret nor any token text appears.
func TestNoSecretOrTokenInLogsOrErrors(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	var seenTokens []string
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		seenTokens = append(seenTokens, tok)
		// A hostile or buggy server that echoes the credential back.
		http.Error(w, "bad token "+tok, http.StatusUnauthorized)
	}))
	defer echo.Close()

	c := testClient(t, echo.URL)
	slog.Info("voice", "livekit", c.cfg)
	joinTok, err := c.JoinToken(JoinParams{Identity: "p1", Room: "r", Lifetime: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	errs := []error{
		c.CreateRoom(context.Background(), "r"),
		c.RemoveParticipant(context.Background(), "r", "p1"),
	}
	_, e := c.ListRooms(context.Background())
	errs = append(errs, e)
	_, e = c.ListParticipants(context.Background(), "r")
	errs = append(errs, e)

	if len(seenTokens) != 4 {
		t.Fatalf("server saw %d tokens", len(seenTokens))
	}
	haystack := logs.String()
	for _, err := range errs {
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("err = %v", err)
		}
		haystack += "\n" + err.Error() + "\n" + fmt.Sprintf("%+v", err)
	}
	needles := append([]string{"s3cret-value", joinTok}, seenTokens...)
	for _, tok := range append([]string{joinTok}, seenTokens...) {
		// Each JWT segment is also token text.
		needles = append(needles, strings.Split(tok, ".")[1:]...)
	}
	for _, n := range needles {
		if n != "" && strings.Contains(haystack, n) {
			t.Errorf("logs or errors contain %q", n[:min(len(n), 12)]+"...")
		}
	}
}

// Removing a participant who has already left, or from a room LiveKit no
// longer has, is success: LiveKit's own not_found answer means the outcome
// already holds. Every other call still treats 404 as unavailable, and
// RemoveParticipant still reports other statuses and any other kind of 404.
func TestRemoveParticipantTreatsNotFoundAsDone(t *testing.T) {
	ctx := context.Background()
	gone, _ := fakeLiveKit(t, 404, `{"code":"not_found","msg":"participant does not exist"}`)
	c := testClient(t, gone.URL)
	if err := c.RemoveParticipant(ctx, "r", "p7"); err != nil {
		t.Errorf("RemoveParticipant on 404 = %v, want nil", err)
	}
	if err := c.CreateRoom(ctx, "r"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("CreateRoom on 404 = %v, want ErrUnavailable", err)
	}
	if _, err := c.ListParticipants(ctx, "r"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("ListParticipants on 404 = %v, want ErrUnavailable", err)
	}
	for _, status := range []int{401, 403, 500, 503} {
		srv, _ := fakeLiveKit(t, status, ``)
		if err := testClient(t, srv.URL).RemoveParticipant(ctx, "r", "p7"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("RemoveParticipant on %d = %v, want ErrUnavailable", status, err)
		}
	}
}

// Only LiveKit's own "not_found" makes a 404 mean "already gone". A wrong
// route, a proxy's 404 page, or a 404 with no body must stay a failure:
// calling those success would record a removal that never happened. The
// bodies are the ones LiveKit 1.13.7 returned for each case.
func TestRemoveParticipantOnlyTrustsLiveKitsNotFound(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		wantDone   bool
	}{
		{"participant does not exist", `{"code":"not_found","msg":"twirp error unknown: participant does not exist"}`, 404, true},
		{"unknown method", `{"code":"bad_route","msg":"no handler for path","meta":{"twirp_invalid_route":"POST /x"}}`, 404, false},
		{"unknown path", "404 page not found\n", 404, false},
		{"proxy page", "<html><body>Not Found</body></html>", 404, false},
		{"empty body", "", 404, false},
		{"not_found code on another status", `{"code":"not_found"}`, 500, false},
		{"code of the wrong type", `{"code":404}`, 404, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeLiveKit(t, tt.status, tt.body)
			err := testClient(t, srv.URL).RemoveParticipant(context.Background(), "r", "p7")
			if tt.wantDone {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
			if strings.Contains(err.Error(), "bad_route") || strings.Contains(err.Error(), "Not Found") || strings.Contains(err.Error(), "no handler") {
				t.Errorf("error repeats the response body: %v", err)
			}
		})
	}
}

// An admin grant with no room named would be wider than any call needs, and a
// removal with no identity names nobody. Neither is signed or sent.
func TestRoomCallsRejectEmptyNames(t *testing.T) {
	srv, rec := fakeLiveKit(t, 200, `{}`)
	c := testClient(t, srv.URL)
	ctx := context.Background()
	calls := map[string]func() error{
		"CreateRoom":                 func() error { return c.CreateRoom(ctx, "") },
		"ListParticipants":           func() error { _, err := c.ListParticipants(ctx, ""); return err },
		"DeleteRoom no room":         func() error { return c.DeleteRoom(ctx, "") },
		"RemoveParticipant no room":  func() error { return c.RemoveParticipant(ctx, "", "p7") },
		"RemoveParticipant no ident": func() error { return c.RemoveParticipant(ctx, "r", "") },
	}
	for name, call := range calls {
		if err := call(); err == nil || errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err = %v, want a refusal that is not ErrUnavailable", name, err)
		}
	}
	if rec.path != "" {
		t.Errorf("a request was sent to %s", rec.path)
	}
}

// Room calls carry a bearer token; a proxy named in the environment must not
// see them.
func TestClientIgnoresEnvironmentProxy(t *testing.T) {
	c := testClient(t, "http://127.0.0.1:1")
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Fatalf("transport = %T with a proxy function set; want an *http.Transport with none", c.http.Transport)
	}
}

// CreateRoom is believed only when the answer names the room that was asked
// for. A 200 from something that is not LiveKit (a proxy's page, another
// service on the address) must not pass for a room that exists.
func TestCreateRoomRequiresTheRoomInTheAnswer(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"the room, as LiveKit answers", `{"sid":"RM_x","name":"conch-a","empty_timeout":300}`, false},
		{"an empty object", `{}`, true},
		{"another room", `{"name":"conch-b"}`, true},
		{"an HTML page", `<html><body>welcome</body></html>`, true},
		{"an empty body", ``, true},
		{"a name of the wrong type", `{"name":7}`, true},
		{"null", `null`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeLiveKit(t, 200, tt.body)
			err := testClient(t, srv.URL).CreateRoom(context.Background(), "conch-a")
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want an error: %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrUnavailable) {
				t.Errorf("err = %v, want it to wrap ErrUnavailable", err)
			}
		})
	}
}

// Evict tells a removal from "there was nobody to remove".
func TestEvictReportsWhetherSomeoneWasRemoved(t *testing.T) {
	ctx := context.Background()
	present, _ := fakeLiveKit(t, 200, `{}`)
	if removed, err := testClient(t, present.URL).Evict(ctx, "r", "p7"); err != nil || !removed {
		t.Errorf("Evict of a connected participant = %v, %v; want true, nil", removed, err)
	}
	absent, _ := fakeLiveKit(t, 404, `{"code":"not_found","msg":"participant does not exist"}`)
	if removed, err := testClient(t, absent.URL).Evict(ctx, "r", "p7"); err != nil || removed {
		t.Errorf("Evict of an absent participant = %v, %v; want false, nil", removed, err)
	}
	down, _ := fakeLiveKit(t, 500, `{"code":"internal"}`)
	if removed, err := testClient(t, down.URL).Evict(ctx, "r", "p7"); !errors.Is(err, ErrUnavailable) || removed {
		t.Errorf("Evict when LiveKit fails = %v, %v; want false, ErrUnavailable", removed, err)
	}
	if removed, err := testClient(t, present.URL).Evict(ctx, "", "p7"); err == nil || removed {
		t.Errorf("Evict without a room = %v, %v", removed, err)
	}
}

// UpdateParticipant tells "nobody by that identity" (LiveKit's own not_found,
// as 1.13.7 answers it) from a change made, and believes a 200 only when it
// names the participant. Everything else is ErrUnavailable and repeats
// neither the body nor the secret.
func TestUpdateParticipantAnswers(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		wantFound  bool
		wantErr    bool
	}{
		{"changed", `{"sid":"PA_x","identity":"p7","permission":{}}`, 200, true, false},
		{"participant not in the room", `{"code":"not_found","msg":"twirp error unknown: participant does not exist"}`, 404, false, false},
		{"another participant named", `{"identity":"p8"}`, 200, false, true},
		{"an empty object", `{}`, 200, false, true},
		{"an HTML page", `<html>ok</html>`, 200, false, true},
		{"unknown method", `{"code":"bad_route","msg":"no handler for path"}`, 404, false, true},
		{"empty 404", ``, 404, false, true},
		{"room admin refused", `{"code":"unauthenticated","msg":"permissions denied"}`, 401, false, true},
		{"server error", `{"code":"internal","msg":"leaky detail"}`, 500, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeLiveKit(t, tt.status, tt.body)
			found, err := testClient(t, srv.URL).UpdateParticipant(context.Background(), "conch-a", "p7", Permission{CanPublish: true, ScreenShare: true})
			if found != tt.wantFound || (err != nil) != tt.wantErr {
				t.Fatalf("UpdateParticipant = %v, %v; want found %v, error %v", found, err, tt.wantFound, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("err = %v, want ErrUnavailable", err)
			}
			for _, leak := range []string{"leaky", "bad_route", "s3cret-value", "conch-a"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error repeats %q: %v", leak, err)
				}
			}
		})
	}
	srv, rec := fakeLiveKit(t, 200, `{"identity":"p7"}`)
	c := testClient(t, srv.URL)
	for name, call := range map[string]func() (bool, error){
		"no room":     func() (bool, error) { return c.UpdateParticipant(context.Background(), "", "p7", Permission{}) },
		"no identity": func() (bool, error) { return c.UpdateParticipant(context.Background(), "r", "", Permission{}) },
	} {
		if found, err := call(); found || err == nil || errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v, %v; want a refusal that is not ErrUnavailable", name, found, err)
		}
	}
	if rec.path != "" {
		t.Errorf("a request was sent to %s", rec.path)
	}
}

// Deleting a room LiveKit no longer has is success, but only on LiveKit's own
// not_found answer; every other failure is ErrUnavailable and repeats neither
// the response body nor the secret.
func TestDeleteRoomTreatsNotFoundAsDone(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		wantDone   bool
	}{
		{"ok", `{}`, 200, true},
		{"room already gone", `{"code":"not_found","msg":"requested room does not exist"}`, 404, true},
		{"unknown method", `{"code":"bad_route","msg":"no handler for path"}`, 404, false},
		{"proxy page", "<html>Not Found</html>", 404, false},
		{"empty 404", "", 404, false},
		{"server error", `{"code":"internal","msg":"leaky detail"}`, 500, false},
		{"unauthenticated", `{"code":"unauthenticated"}`, 401, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := fakeLiveKit(t, tt.status, tt.body)
			err := testClient(t, srv.URL).DeleteRoom(context.Background(), "conch-a")
			if tt.wantDone {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want ErrUnavailable", err)
			}
			for _, leak := range []string{"leaky", "bad_route", "Not Found", "s3cret-value"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("error repeats %q: %v", leak, err)
				}
			}
		})
	}
}

// TestUpdateParticipantRefusesWideSourcePermissions: a per-source permission
// that may publish with no sources (LiveKit reads that as every source), or
// that names anything but the screen-share sources, is refused before any
// request is made.
func TestUpdateParticipantRefusesWideSourcePermissions(t *testing.T) {
	srv, rec := fakeLiveKit(t, 200, `{"identity":"p7.screen"}`)
	c := testClient(t, srv.URL)
	for name, perm := range map[string]Permission{
		"publish with no sources":         SourcePermission(true, true),
		"publish with no sources, no sub": SourcePermission(false, true),
		"the microphone":                  SourcePermission(true, true, Source("MICROPHONE")),
		"an empty source":                 SourcePermission(true, true, Source("")),
		"a lower-case source":             SourcePermission(true, true, Source("screen_share")),
		"a good source beside a bad one":  SourcePermission(true, true, SourceScreenShare, Source("CAMERA")),
		"a bad source, not publishing":    SourcePermission(true, false, Source("CAMERA")),
	} {
		found, err := c.UpdateParticipant(context.Background(), "conch-a", "p7.screen", perm)
		if found || err == nil || errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v, %v; want a refusal that is not ErrUnavailable", name, found, err)
		}
	}
	if rec.path != "" {
		t.Errorf("a request was sent to %s", rec.path)
	}
}

// TestGetParticipantAnswers: one participant looked up by name; nobody by that
// name is not an error; an answer that is not LiveKit's, or names another, is.
func TestGetParticipantAnswers(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		want       Participant
		wantFound  bool
		wantErr    bool
	}{
		{"publish-only companion", `{"identity":"p7.screen","permission":{"can_subscribe":false,"can_publish":true,"can_publish_sources":["SCREEN_SHARE"]},"tracks":[{"type":"VIDEO","source":"SCREEN_SHARE"}]}`, 200,
			Participant{Identity: "p7.screen", ScreenSharePublished: true, ShareSourcesGranted: true, CanPublish: true}, true, false},
		{"subscribing", `{"identity":"p7.screen","permission":{"can_subscribe":true}}`, 200, Participant{Identity: "p7.screen", CanSubscribe: true}, true, false},
		{"not in the room", `{"code":"not_found","msg":"participant does not exist"}`, 404, Participant{}, false, false},
		{"another participant named", `{"identity":"p8"}`, 200, Participant{}, false, true},
		{"an empty object", `{}`, 200, Participant{}, false, true},
		{"an HTML page", `<html>ok</html>`, 200, Participant{}, false, true},
		{"unknown method", `{"code":"bad_route"}`, 404, Participant{}, false, true},
		{"server error", `{"code":"internal","msg":"leaky detail"}`, 500, Participant{}, false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := fakeLiveKit(t, tt.status, tt.body)
			got, found, err := testClient(t, srv.URL).GetParticipant(context.Background(), "conch-a", "p7.screen")
			if found != tt.wantFound || (err != nil) != tt.wantErr || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("GetParticipant = %+v, %v, %v; want %+v, %v, error %v", got, found, err, tt.want, tt.wantFound, tt.wantErr)
			}
			if rec.path != "/twirp/livekit.RoomService/GetParticipant" {
				t.Errorf("request path = %s", rec.path)
			}
			if err != nil && (!errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "leaky") || strings.Contains(err.Error(), "conch-a")) {
				t.Errorf("err = %v", err)
			}
		})
	}
	if _, found, err := testClient(t, "http://127.0.0.1:1").GetParticipant(context.Background(), "", "p7"); found || err == nil || errors.Is(err, ErrUnavailable) {
		t.Errorf("no room: %v, %v; want a refusal that is not ErrUnavailable", found, err)
	}
}
