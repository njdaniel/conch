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
	if !reflect.DeepEqual(claims["video"], want) {
		t.Errorf("video grant = %v, want %v", claims["video"], want)
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
			name: "RemoveParticipant", method: "RemoveParticipant", response: `{}`,
			call:      func(c *Client) (any, error) { return nil, c.RemoveParticipant(context.Background(), "conch-a", "p7") },
			wantBody:  map[string]any{"room": "conch-a", "identity": "p7"},
			wantGrant: map[string]any{"roomAdmin": true, "room": "conch-a"},
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
	}
	for caseName, url := range failureCases(t) {
		for opName, op := range ops {
			if caseName == "malformed JSON" && (opName == "CreateRoom" || opName == "RemoveParticipant") {
				continue // these ignore the body, so there is nothing to be malformed
			}
			if caseName == "wrong JSON types" && (opName == "CreateRoom" || opName == "RemoveParticipant") {
				continue
			}
			if caseName == "HTTP 404" && opName == "RemoveParticipant" {
				continue // already gone is success; see TestRemoveParticipantTreatsNotFoundAsDone
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
		srv2, _ := fakeLiveKit(t, 200, `{}`)
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
// longer has, is success: the 404 means the outcome already holds. Every other
// call still treats 404 as unavailable, and RemoveParticipant still reports
// other statuses.
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
