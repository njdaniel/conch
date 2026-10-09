package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

const promptPreamble = `Write one concise reply to the new Conch channel messages below. Output only the reply body text. Do not use markdown fences and do not add an introductory phrase.`

type botMCPClient interface {
	readChannel(context.Context, string, int64, int) (schema.ListMessagesResponseV2, error)
	// postMessage posts body with the given audience; nil means channel-wide.
	postMessage(context.Context, string, string, *schema.Audience) error
}

type ClaudeRunner interface {
	Reply(context.Context, string) (string, error)
}

// maxAudienceWindows bounds how many per-audience context windows the bot
// keeps. Each window holds at most cfg.ContextMessages messages, so total
// memory is at most maxAudienceWindows * ContextMessages messages. When a new
// audience would exceed the cap, the least recently touched window is dropped:
// that only costs that audience some context, never discloses anything.
const maxAudienceWindows = 64

// refusalCodes are the tool error codes by which the server refuses to let the
// bot address an audience. A refused scoped reply is dropped, not widened.
var refusalCodes = map[string]bool{"forbidden": true, "net_not_found": true, "invalid_audience": true}

type contextWindow struct {
	messages []schema.MessageV2 // oldest first, at most cfg.ContextMessages
	touched  int64              // logical clock of the last write, for eviction
}

type botLoop struct {
	cfg      config
	mcp      botMCPClient
	claude   ClaudeRunner
	lastSeen int64
	sleep    func(context.Context, time.Duration) error
	// recent holds a rolling window of the last cfg.ContextMessages messages
	// seen (any author) per audience, keyed by audienceKey. A prompt only ever
	// reads the window of the audience it answers, so no scoped body can reach
	// another audience's prompt.
	recent map[string]*contextWindow
	clock  int64
}

// audienceKey identifies an audience for grouping and context. nil (channel-
// wide) has its own key; a net is its id; a whisper is its sorted principal
// ids, so two whispers with the same participants are one audience. An unknown
// kind gets a key of its own that includes the kind, so it is scoped, never
// merged with channel-wide, a net or a whisper.
func audienceKey(a *schema.Audience) string {
	if a == nil {
		return "channel"
	}
	ids := append([]int64(nil), a.PrincipalIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var out strings.Builder
	fmt.Fprintf(&out, "scoped:%q:net=%d:principals=", string(a.Kind), a.NetID)
	for i, id := range ids {
		if i > 0 {
			out.WriteByte(',')
		}
		fmt.Fprintf(&out, "%d", id)
	}
	return out.String()
}

// audienceGroup is the messages of one poll that share an audience.
type audienceGroup struct {
	key      string
	audience *schema.Audience // a copy of the audience as read; nil for channel-wide
	messages []schema.MessageV2
}

// groupByAudience splits messages (in id order) by audience, groups ordered by
// their first message.
func groupByAudience(messages []schema.MessageV2) []*audienceGroup {
	var groups []*audienceGroup
	byKey := map[string]*audienceGroup{}
	for _, message := range messages {
		key := audienceKey(message.Audience)
		g := byKey[key]
		if g == nil {
			g = &audienceGroup{key: key, audience: copyAudience(message.Audience)}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.messages = append(g.messages, message)
	}
	return groups
}

func copyAudience(a *schema.Audience) *schema.Audience {
	if a == nil {
		return nil
	}
	c := *a
	if a.PrincipalIDs != nil {
		c.PrincipalIDs = append([]int64(nil), a.PrincipalIDs...)
	}
	return &c
}

func (b *botLoop) seed(ctx context.Context) error {
	messages, err := b.drain(ctx, 0)
	if err != nil {
		return err
	}
	b.advance(messages)
	for _, g := range groupByAudience(messages) {
		b.remember(g.key, g.messages)
	}
	return nil
}

func (b *botLoop) pollOnce(ctx context.Context) error {
	previous := b.lastSeen
	messages, err := b.drain(ctx, previous)
	if err != nil {
		return err
	}
	// The cursor moves past every message read before any reply is attempted,
	// so a message is never read again: a refused or failed reply is not
	// retried by the next poll.
	b.advance(messages)

	// One model call and one reply per audience, in order of each audience's
	// first message. A prompt is built from that audience's messages and
	// window only.
	var errs []error
	for _, g := range groupByAudience(messages) {
		// Snapshot before folding this poll's messages in, so "history" never
		// includes the very messages we are about to reply to.
		history := b.recentSnapshot(g.key)
		b.remember(g.key, g.messages)
		if err := b.replyTo(ctx, g, history); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// replyTo answers one audience's new messages in that same audience. A refusal
// of a scoped reply is logged and dropped; there is no second attempt with a
// wider or missing audience.
func (b *botLoop) replyTo(ctx context.Context, g *audienceGroup, history []schema.MessageV2) error {
	human := make([]schema.MessageV2, 0, len(g.messages))
	for _, message := range g.messages {
		if message.AuthorID != b.cfg.PrincipalID {
			human = append(human, message)
		}
	}
	if len(human) == 0 {
		return nil
	}
	prompt := buildPrompt(history, human, b.cfg.ContextMessages)
	reply, err := b.claude.Reply(ctx, prompt)
	if err != nil {
		return fmt.Errorf("generate reply: %w", err)
	}
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return nil
	}
	if err := b.mcp.postMessage(ctx, b.cfg.Channel, reply, g.audience); err != nil {
		var toolErr *mcpclient.ToolError
		if g.audience != nil && errors.As(err, &toolErr) && refusalCodes[toolErr.Code] {
			slog.Warn("conch-bot: scoped reply refused; nothing was posted", "code", toolErr.Code, "audience_kind", string(g.audience.Kind))
			return nil
		}
		return fmt.Errorf("post reply: %w", err)
	}
	return nil
}

func (b *botLoop) drain(ctx context.Context, after int64) ([]schema.MessageV2, error) {
	var messages []schema.MessageV2
	cursor := after
	for {
		page, err := b.mcp.readChannel(ctx, b.cfg.Channel, cursor, 100)
		if err != nil {
			return nil, fmt.Errorf("read channel after %d: %w", cursor, err)
		}
		messages = append(messages, page.Messages...)
		if page.NextAfter == 0 {
			break
		}
		if page.NextAfter <= cursor {
			return nil, fmt.Errorf("read channel returned non-advancing next_after %d", page.NextAfter)
		}
		cursor = page.NextAfter
	}
	sort.SliceStable(messages, func(i, j int) bool { return messages[i].ID < messages[j].ID })
	return messages, nil
}

func (b *botLoop) advance(messages []schema.MessageV2) {
	for _, message := range messages {
		if message.ID > b.lastSeen {
			b.lastSeen = message.ID
		}
	}
}

// remember folds newly drained messages into the audience's rolling context
// window, capped at cfg.ContextMessages. This replaces re-draining the channel
// from message id 0 on every poll: the bot already sees every message exactly
// once as it drains new ones, so it can keep its own bounded window instead of
// asking conchd to replay the whole channel each time it needs context.
func (b *botLoop) remember(key string, messages []schema.MessageV2) {
	if b.cfg.ContextMessages == 0 || len(messages) == 0 {
		return
	}
	if b.recent == nil {
		b.recent = map[string]*contextWindow{}
	}
	b.clock++
	w := b.recent[key]
	if w == nil {
		if len(b.recent) >= maxAudienceWindows {
			b.evictOldest()
		}
		w = &contextWindow{}
		b.recent[key] = w
	}
	w.touched = b.clock
	w.messages = append(w.messages, messages...)
	if len(w.messages) > b.cfg.ContextMessages {
		w.messages = append([]schema.MessageV2(nil), w.messages[len(w.messages)-b.cfg.ContextMessages:]...)
	}
}

func (b *botLoop) evictOldest() {
	oldestKey, oldest, found := "", int64(0), false
	for key, w := range b.recent {
		if !found || w.touched < oldest {
			oldestKey, oldest, found = key, w.touched, true
		}
	}
	delete(b.recent, oldestKey)
}

func (b *botLoop) recentSnapshot(key string) []schema.MessageV2 {
	w := b.recent[key]
	if w == nil || len(w.messages) == 0 {
		return nil
	}
	return append([]schema.MessageV2(nil), w.messages...)
}

func buildPrompt(history, messages []schema.MessageV2, contextLimit int) string {
	historyLimit := contextLimit - len(messages)
	if historyLimit < 0 {
		historyLimit = 0
	}
	if len(history) > historyLimit {
		history = history[len(history)-historyLimit:]
	}
	var out strings.Builder
	out.WriteString(promptPreamble)
	if len(history) > 0 {
		out.WriteString("\n\nRecent context:\n")
		writeMessages(&out, history)
	}
	out.WriteString("\n\nNew messages to reply to:\n")
	writeMessages(&out, messages)
	return out.String()
}

func writeMessages(out *strings.Builder, messages []schema.MessageV2) {
	for _, message := range messages {
		fmt.Fprintf(out, "%d: %s\n", message.AuthorID, message.Body)
	}
}

func (b *botLoop) run(ctx context.Context) error {
	if b.sleep == nil {
		b.sleep = sleepContext
	}
	if err := b.seed(ctx); err != nil {
		return fmt.Errorf("seed cursor: %w", err)
	}
	delay := b.cfg.PollInterval
	for {
		err := b.pollOnce(ctx)
		if err != nil {
			slog.Error("conch-bot iteration failed", "error", err)
			delay = nextBackoff(delay, b.cfg.PollInterval, b.cfg.MaxBackoff)
		} else {
			delay = b.cfg.PollInterval
		}
		if err := b.sleep(ctx, delay); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
	}
}

func nextBackoff(current, poll, maximum time.Duration) time.Duration {
	if current < poll {
		current = poll
	}
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
