package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/cli"
	"github.com/njdaniel/conch/internal/server"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The largest message a real conchd accepts must reach every kind of
// subscriber and leave it connected (issue #231). With the WebSocket library's
// default read limit a message over 32 KiB ended the subscription of everyone
// in the channel. The body here is the worst there is: as long as a request
// may be, and made of a character that is one byte going in and six coming
// back.
func TestTheLargestMessageReachesEverySubscriber(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{AuthMode: server.AuthOff, DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "test"}, st)
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	url := "http://" + srv.Addr()
	if _, err := st.CreateChannel(ctx, "general"); err != nil {
		t.Fatal(err)
	}
	author, err := st.CreatePrincipal(ctx, store.PrincipalHuman, "nick")
	if err != nil {
		t.Fatal(err)
	}
	client, err := cli.NewClient(url, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Each subscriber reports the length of every body it receives, and stops
	// after the small message that follows the large one.
	const small = "after the large one"
	errDone := errors.New("done")
	type result struct {
		version string
		lengths []int
		err     error
	}
	results := make(chan result, 3)
	collect := func(version string, subscribe func(func(string) error) error) {
		var lengths []int
		err := subscribe(func(body string) error {
			lengths = append(lengths, len(body))
			if body == small {
				return errDone
			}
			return nil
		})
		results <- result{version, lengths, err}
	}
	go collect("v0", func(got func(string) error) error {
		return client.Tail(ctx, "general", func(m schema.MessageV0) error { return got(m.Body) })
	})
	go collect("v1", func(got func(string) error) error {
		return client.Subscribe(ctx, "general", func(m schema.MessageV1) error { return got(m.Body) })
	})
	go collect("v2", func(got func(string) error) error {
		return client.SubscribeV2(ctx, "general", func(m schema.MessageV2) error { return got(m.Body) })
	})
	// The subscriptions are live once the server has registered them; a
	// message posted before that would simply be missed, so wait briefly.
	time.Sleep(300 * time.Millisecond)

	post := func(body string) int {
		t.Helper()
		request := fmt.Sprintf(`{"author_id":%d,"body":%q}`, author.ID, body)
		resp, err := http.Post(url+"/v0/channels/general/messages", "application/json", bytes.NewReader([]byte(request)))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	envelope := len(fmt.Sprintf(`{"author_id":%d,"body":""}`, author.ID))
	large := strings.Repeat("<", schema.MaxPostRequestBytes-envelope)
	if code := post(large); code != http.StatusCreated {
		t.Fatalf("posting a request of exactly the limit = %d, want 201", code)
	}
	if code := post(large + "<"); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("posting one byte over the limit = %d, want 413: the test no longer sends the largest message", code)
	}
	if code := post(small); code != http.StatusCreated {
		t.Fatalf("posting the small message = %d", code)
	}

	for range 3 {
		select {
		case r := <-results:
			if !errors.Is(r.err, errDone) {
				t.Errorf("%s subscription ended with %v, want it open until the small message", r.version, r.err)
				continue
			}
			if len(r.lengths) != 2 || r.lengths[0] != len(large) || r.lengths[1] != len(small) {
				t.Errorf("%s received bodies of %v bytes, want [%d %d]", r.version, r.lengths, len(large), len(small))
			}
		case <-time.After(30 * time.Second):
			t.Fatal("a subscriber never received the small message")
		}
	}
}
