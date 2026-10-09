package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/approvals"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// With no ntfy server configured, conchd has no notifier: the start row says
// notifications are off and an approval's transitions get no notify rows.
// The server used to hand the manager a nil *NtfyNotifier inside the Notifier
// interface, which is not nil: the log then said notifications=on and
// recorded notify_sent for deliveries that were never made (issue #158).
func TestUnconfiguredNtfyRecordsNoNotifications(t *testing.T) {
	for _, tt := range []struct {
		name string
		ntfy approvals.NtfyConfig
	}{
		{"no ntfy settings", approvals.NtfyConfig{}},
		{"an invalid ntfy server", approvals.NtfyConfig{Server: "not a url", ApprovalsTopic: "approvals"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			st, err := store.Open(ctx, filepath.Join(t.TempDir(), "conch.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = st.Close() }()
			srv := New(Config{AuthMode: AuthOff, DataDir: t.TempDir(), Listen: "127.0.0.1:0", Version: "test", Ntfy: tt.ntfy}, st)
			serveCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- srv.Serve(serveCtx) }()
			defer func() {
				stop()
				if err := <-done; err != nil {
					t.Errorf("Serve: %v", err)
				}
			}()

			started := func() string {
				e, err := st.LastAuditEvent(ctx, approvals.AuditApprovalsStarted)
				if err != nil {
					return ""
				}
				return e.Detail
			}
			deadline := time.Now().Add(10 * time.Second)
			for started() == "" && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if got := started(); !strings.HasPrefix(got, "notifications=off") {
				t.Fatalf("approvals_started detail = %q, want notifications=off", got)
			}

			ch, err := st.CreateChannel(ctx, "ops")
			if err != nil {
				t.Fatal(err)
			}
			agent, err := st.CreatePrincipal(ctx, store.PrincipalAgent, "bot")
			if err != nil {
				t.Fatal(err)
			}
			human, err := st.CreatePrincipal(ctx, store.PrincipalHuman, "ann")
			if err != nil {
				t.Fatal(err)
			}
			a, err := srv.approvals.Create(ctx, store.ApprovalParams{
				RequesterID: agent.ID, ChannelID: ch.ID, Title: "Deploy", Body: "now",
				Options: []schema.Option{
					{ID: "approve", Label: "Approve", Kind: schema.OptionKindApprove},
					{ID: "reject", Label: "Reject", Kind: schema.OptionKindReject},
				},
				Deadline: time.Now().Add(time.Hour), Quorum: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, r, err := srv.approvals.Decide(ctx, a.ID, human.ID, "approve", "fine"); err != nil || r == nil {
				t.Fatalf("Decide = %+v, %v", r, err)
			}
			events, err := st.ListAuditEvents(ctx, 0, 1000)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range events {
				if e.Action == approvals.AuditNotifySent || e.Action == approvals.AuditNotifyFailed {
					t.Errorf("audit row %s %s %q with no notifier configured: nothing was sent", e.Action, e.Subject, e.Detail)
				}
			}
		})
	}
}
