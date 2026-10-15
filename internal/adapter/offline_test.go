package adapter

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

// A report refused with session_offline is followed by one heartbeat and one
// retry, in that order; any other outcome is left as it was.
func TestReportRetriesOnceOnSessionOffline(t *testing.T) {
	tooMany := &mcpclient.ToolError{Method: "heartbeat", Code: schema.ErrorCodeTooManySessions}
	tests := []struct {
		name       string
		call       string
		offline    int
		beatErr    error
		wantEvents []string
	}{
		{"update accepted after one refusal", "update_run", 1, nil,
			[]string{"update_run:waiting:refused", "heartbeat", "update_run:waiting"}},
		{"update still refused: one heartbeat, one retry", "update_run", 5, nil,
			[]string{"update_run:waiting:refused", "heartbeat", "update_run:waiting:refused"}},
		{"update with a heartbeat that cannot revive: no retry", "update_run", 5, tooMany,
			[]string{"update_run:waiting:refused", "heartbeat"}},
		{"update not refused: no heartbeat", "update_run", 0, nil,
			[]string{"update_run:waiting"}},
		{"activity accepted after one refusal", "post_activity", 1, nil,
			[]string{"activity:reply_posted:refused", "heartbeat", "activity:reply_posted"}},
		{"activity still refused", "post_activity", 5, nil,
			[]string{"activity:reply_posted:refused", "heartbeat", "activity:reply_posted:refused"}},
		{"start accepted after one refusal", "start_run", 1, nil,
			[]string{"start_run:refused", "heartbeat", "start_run"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mcp := &fakeMCP{offline: map[string]int{tt.call: tt.offline}, beatErr: tt.beatErr}
			loop := loopWithSession(mcp, &fakeClaude{}, newFakeClock())
			ctx := context.Background()
			run := &activeRun{id: 1}
			switch tt.call {
			case "update_run":
				loop.setState(ctx, run, schema.RunStateWaiting, "")
			case "post_activity":
				loop.activity(ctx, run, schema.ActivityKindReplyPosted, "posted the reply", "", nil)
			case "start_run":
				got := loop.beginRun(ctx, func() {}, &audienceGroup{}, []schema.MessageV2{message(2, 3, "q")})
				if got == nil {
					t.Fatal("beginRun returned no run after a successful retry")
				}
				// beginRun goes on to move the run to starting and running.
				mcp.events = mcp.events[:3]
			}
			if got := mcp.eventLog(); !reflect.DeepEqual(got, tt.wantEvents) {
				t.Fatalf("events = %q, want %q", got, tt.wantEvents)
			}
		})
	}
}

// Another error is not retried and no heartbeat is sent.
func TestReportErrorOtherThanOfflineIsNotRetried(t *testing.T) {
	calls, beats := 0, 0
	loop := loopWithSession(&fakeMCP{}, &fakeClaude{}, newFakeClock())
	for _, err := range []error{
		errors.New("connection reset"),
		&mcpclient.ToolError{Method: "update_run", Code: "run_not_found"},
	} {
		got := loop.reportWithRetry(context.Background(), "update_run", func(context.Context) error {
			calls++
			return err
		})
		if !errors.Is(got, err) {
			t.Errorf("error = %v, want %v", got, err)
		}
	}
	beats = countOf(loop.mcp.(*fakeMCP).eventLog(), "heartbeat")
	if calls != 2 || beats != 0 {
		t.Fatalf("calls = %d (want 2), heartbeats = %d (want 0)", calls, beats)
	}
}

// A report that keeps being refused is dropped after its one retry, and the
// run goes on: the reply is still made and the later reports still go out.
func TestRunGoesOnWhenAReportKeepsBeingRefusedOffline(t *testing.T) {
	mcp := &fakeMCP{offline: map[string]int{"post_activity": 2}}
	loop := loopWithSession(mcp, &fakeClaude{reply: "an answer"}, newFakeClock())
	if err := answer(t, loop, message(1, 3, "q")); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"start_run", "update_run:starting", "update_run:running",
		"activity:command_started:refused", "heartbeat", "activity:command_started:refused",
		"activity:command_finished", "post_message", "activity:reply_posted", "update_run:completed:replied",
	}
	if got := mcp.eventLog(); !reflect.DeepEqual(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}
