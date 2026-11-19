package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

type presenceTransport func(*http.Request) (*http.Response, error)

func (f presenceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// LiveKit may already list the participant while conchd still reports an
// empty room or only other people. The pre-cut wait must outlast that lag.
func TestRequirePresentWaitsForPoller(t *testing.T) {
	const id int64 = 42
	for _, tc := range []struct {
		name string
		lag  [][]schema.VoiceParticipant
	}{
		{name: "already observed"},
		{name: "poller has not observed the join", lag: [][]schema.VoiceParticipant{
			{},
			{{PrincipalID: id + 1}},
			{},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			old := httpClient
			t.Cleanup(func() { httpClient = old })
			httpClient = &http.Client{Transport: presenceTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/channels/test-channel/voice" {
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
				}
				participants := []schema.VoiceParticipant{{PrincipalID: id}}
				if requests < len(tc.lag) {
					participants = tc.lag[requests]
				}
				requests++
				body, err := json.Marshal(schema.VoicePresenceV1{
					Schema: schema.VoicePresenceSchemaV1, ChannelID: 1,
					Configured: true, Available: true,
					Rooms: []schema.VoicePresenceRoom{{Participants: participants}},
				})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
			})}
			w := &world{op: api{base: "http://conchd.test", h: &harness{calls: map[string]bool{}}}}
			if err := w.requirePresent("test-channel", &member{id: id}); err != nil {
				t.Fatal(err)
			}
			if want := len(tc.lag) + 1; requests != want {
				t.Fatalf("presence requests = %d, want %d; returned before conchd observed the participant", requests, want)
			}
		})
	}
}
