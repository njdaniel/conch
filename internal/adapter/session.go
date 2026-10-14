package adapter

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

const (
	sessionClient = "conch-adapter"

	// defaultHeartbeat is how often the session is kept alive.
	defaultHeartbeat = 10 * time.Second
	// interventionPollInterval is how often an open run looks for
	// interventions.
	interventionPollInterval = 2 * time.Second
	// shutdownTimeout bounds the calls that close things on the way out, which
	// run after the context that stopped the adapter is already done.
	shutdownTimeout = 5 * time.Second
	// reportTimeout bounds one report about a run.
	reportTimeout = 10 * time.Second
)

// timeSource is the adapter's clock. Tests replace it so that a heartbeat or
// an intervention poll fires when the test says, not after a wait.
type timeSource interface {
	now() time.Time
	newTicker(time.Duration) ticker
}

type ticker interface {
	C() <-chan time.Time
	Stop()
}

type realTime struct{}

func (realTime) now() time.Time                   { return time.Now() }
func (realTime) newTicker(d time.Duration) ticker { return realTicker{time.NewTicker(d)} }

type realTicker struct{ *time.Ticker }

func (t realTicker) C() <-chan time.Time { return t.Ticker.C }

func (b *botLoop) time() timeSource {
	if b.clk == nil {
		return realTime{}
	}
	return b.clk
}

func (b *botLoop) heartbeatInterval() time.Duration {
	if b.cfg.Heartbeat <= 0 {
		return defaultHeartbeat
	}
	return b.cfg.Heartbeat
}

// register opens this adapter's session.
func (b *botLoop) register(ctx context.Context) error {
	host, _ := os.Hostname() // an adapter without a hostname is still an adapter
	session, err := b.mcp.RegisterSession(ctx, schema.CreateSessionRequestV1{
		Client:        sessionClient,
		Runtime:       b.cfg.Runtime,
		Model:         b.cfg.Model,
		Host:          host,
		Interventions: []schema.InterventionKind{schema.InterventionKindMessage, schema.InterventionKindCancel},
	})
	if err != nil {
		return err
	}
	b.sessionID = session.ID
	return nil
}

// beat sends a heartbeat on every tick until ctx ends. A failed heartbeat is
// logged and the next one tried: the server marks a session stale after a few
// missed ones and a later heartbeat revives it.
func (b *botLoop) beat(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	tick := b.time().newTicker(b.heartbeatInterval())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C():
			if _, err := b.mcp.Heartbeat(ctx, b.sessionID); err != nil && ctx.Err() == nil {
				slog.Warn("conch-adapter: heartbeat failed", "error", err)
			}
		}
	}
}

// endSession ends the session. It runs after the adapter was told to stop, so
// it does not use that context.
func (b *botLoop) endSession(ctx context.Context) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := b.mcp.EndSession(closeCtx, b.sessionID); err != nil {
		slog.Warn("conch-adapter: end session failed", "error", err)
	}
}

// allowReply applies the replies-per-minute cap. It counts the replies begun in
// the last minute; over the cap the reply is dropped, which is what stops two
// adapters from answering each other without end. Zero means no cap.
func (b *botLoop) allowReply() bool {
	if b.cfg.MaxRepliesPerMinute <= 0 {
		return true
	}
	now := b.time().now()
	kept := b.replies[:0]
	for _, at := range b.replies {
		if now.Sub(at) < time.Minute {
			kept = append(kept, at)
		}
	}
	b.replies = kept
	if len(b.replies) >= b.cfg.MaxRepliesPerMinute {
		return false
	}
	b.replies = append(b.replies, now)
	return true
}

// logReportError is the one place a failed report is logged: the call, and the
// refusal's code or the error, which never quotes a tool result.
func logReportError(call string, err error) {
	var toolErr *mcpclient.ToolError
	if errors.As(err, &toolErr) && toolErr.Code != "" {
		slog.Warn("conch-adapter: report failed", "call", call, "code", toolErr.Code)
		return
	}
	slog.Warn("conch-adapter: report failed", "call", call, "error", err)
}
