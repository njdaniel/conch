package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// A roster read cut short because its context ended is the client leaving,
// not a fault: it is returned, not logged as an error (issue #313; every TUI
// that quits with its events socket open does this). A store failure on a
// live context is still logged.
func TestChannelRosterLogsOnlyRealFailures(t *testing.T) {
	f := newSessionFixture(t)
	f.create(t, f.aliceTok, humanBody)
	r := httptest.NewRequest("GET", "/v1/sessions?channel=general", nil)

	logs := captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.srv.channelRoster(ctx, r, f.general, f.alice); err == nil {
		t.Fatal("a roster on a cancelled context succeeded; the test cannot show what it means to")
	}
	if strings.Contains(logs.buf.String(), "level=ERROR") {
		t.Errorf("a cancelled roster read was logged as an error:\n%s", logs.buf.String())
	}

	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.channelRoster(context.Background(), r, f.general, f.alice); err == nil {
		t.Fatal("a roster on a closed store succeeded")
	}
	if !strings.Contains(logs.buf.String(), "level=ERROR") || !strings.Contains(logs.buf.String(), "sessions: list failed") {
		t.Errorf("a store failure on a live context was not logged as an error:\n%s", logs.buf.String())
	}
}
