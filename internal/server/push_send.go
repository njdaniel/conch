package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/njdaniel/conch/internal/server/push"
	"github.com/njdaniel/conch/internal/server/store"
)

// The Web Push sender's wiring (issue #1715; docs/design/mobile-notifications.md
// §5.6, §6.2). A committed message goes to two places: the events hub, which
// tells the sockets that are open, and the push sender, which tells the phones
// whose sign-ins asked to hear about it. messageCommitted is the one place both
// happen, so a new way to post cannot reach one and miss the other
// (TestNewMessagesReachPushThroughOneHelper).
//
// The post's part in push is one non-blocking send to a bounded channel
// (push.Sender.Enqueue). Whom to wake, the limits, the encryption and the
// request are the sender's goroutines'. With Web Push off there is no sender,
// the enqueue is a nil check, and no goroutine runs.

// messageCommitted is called once a new message has been committed, by every
// path that posts one, and by no path that edits, removes or reacts to one.
// It queues the message for the open sockets and for push. Neither waits:
// notifyMessage queues references for the subscribers' own goroutines, and the
// enqueue never blocks.
func (s *Server) messageCommitted(channelID, messageID, authorID int64) {
	s.events.notifyMessage(channelID, messageID, authorID)
	if s.pushSender != nil {
		s.pushSender.Enqueue(push.Post{ChannelID: channelID, MessageID: messageID, AuthorID: authorID})
	}
}

// pushSource gives the sender its targets and records what came of a send. It
// is the store behind the sender, and holds no target past a call.
type pushSource struct{ st *store.Store }

func (p pushSource) Targets(ctx context.Context, post push.Post, now time.Time) ([]push.Target, error) {
	found, err := p.st.PushTargets(ctx, post.ChannelID, post.MessageID, post.AuthorID, now)
	if err != nil {
		return nil, err
	}
	out := make([]push.Target, len(found))
	for i, f := range found {
		out[i] = push.Target{
			RegistrationID: f.RegistrationID, Endpoint: f.Endpoint, P256DH: f.P256DH, Auth: f.Auth,
			LastWakeAt: f.LastWakeAt, LastFetchAt: f.LastFetchAt,
		}
	}
	return out, nil
}

func (p pushSource) MarkWake(ctx context.Context, id string, now time.Time) error {
	return p.st.MarkPushWake(ctx, id, now)
}

func (p pushSource) Reject(ctx context.Context, id string, now time.Time) error {
	_, err := p.st.EndPushRegistrationRejected(ctx, id, now)
	return err
}

// pushObserver counts the sender's outcomes in the server's metrics.
type pushObserver struct{ m *serverMetrics }

func (o pushObserver) Wake(outcome string)  { o.m.pushWake(outcome) }
func (o pushObserver) Failure(class string) { o.m.pushFailure(class) }

// ValidatePushConfig checks the push settings that can be wrong: the TTL, the
// budget and the VAPID subject. Zero values are the defaults and are valid.
// Serve calls it before it binds anything, and conchd's flags call it so the
// start refuses with the flag's name.
func ValidatePushConfig(cfg PushConfig) error {
	if cfg.Expire != 0 {
		if err := push.ValidateExpire(cfg.Expire); err != nil {
			return fmt.Errorf("push expire: %w", err)
		}
	}
	if cfg.MaxPerMinute != 0 {
		if err := push.ValidateMaxPerMinute(cfg.MaxPerMinute); err != nil {
			return fmt.Errorf("push max per minute: %w", err)
		}
	}
	if err := push.ValidateSubject(cfg.VAPIDSubject); err != nil {
		return fmt.Errorf("push VAPID subject: %w", err)
	}
	return nil
}

// newPushSender builds the sender when Web Push is on, and returns nil when it
// is not (or when its settings are wrong, which Serve refuses before it gets
// here: a sender that cannot be made sends nothing). Nothing runs until
// startPushSender.
func (s *Server) newPushSender() *push.Sender {
	if !s.webPushOn() {
		return nil
	}
	cfg := s.cfg.Push
	client := cfg.client
	if client == nil {
		// The one client every send goes through: it pins addresses, follows
		// no redirect and honours no proxy.
		client = push.NewClient(cfg.Resolver, cfg.AllowPrivate)
	}
	var source push.Source = pushSource{st: s.store}
	if cfg.wrapSource != nil {
		source = cfg.wrapSource(source)
	}
	sender, err := push.NewSender(push.SenderConfig{
		Source:       source,
		Observer:     pushObserver{m: s.metrics},
		Client:       client,
		Key:          cfg.VAPID,
		Subject:      cfg.VAPIDSubject,
		AllowPrivate: cfg.AllowPrivate,
		Expire:       cfg.Expire,
		MaxPerMinute: cfg.MaxPerMinute,
		Now:          func() time.Time { return s.now() },
		AfterFunc:    cfg.afterFunc,
		QueueSize:    cfg.queueSize,
	})
	if err != nil {
		slog.Error("push: the sender is off because its configuration is invalid", "error", err)
		return nil
	}
	return sender
}

// startPushSender starts the sender, if there is one, and returns the function
// that stops it and waits for its goroutines. Wakes still queued when it stops
// are dropped: a wake is a hint, and the feed is what the phone reads.
func (s *Server) startPushSender(ctx context.Context) func() {
	if s.pushSender == nil {
		return func() {}
	}
	s.pushSender.Start(ctx)
	return s.pushSender.Stop
}
