package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

const promptPreamble = `Write one concise reply to the new Conch channel messages below. Output only the reply body text. Do not use markdown fences and do not add an introductory phrase.`

type botMCPClient interface {
	readChannel(context.Context, string, int64, int) (schema.ListMessagesResponseV1, error)
	postMessage(context.Context, string, string) error
}

type ClaudeRunner interface {
	Reply(context.Context, string) (string, error)
}

type botLoop struct {
	cfg      config
	mcp      botMCPClient
	claude   ClaudeRunner
	lastSeen int64
	sleep    func(context.Context, time.Duration) error
	recent   []schema.MessageV1 // rolling window of the last cfg.ContextMessages messages seen (any author), oldest first
}

func (b *botLoop) seed(ctx context.Context) error {
	messages, err := b.drain(ctx, 0)
	if err != nil {
		return err
	}
	b.advance(messages)
	b.remember(messages)
	return nil
}

func (b *botLoop) pollOnce(ctx context.Context) error {
	previous := b.lastSeen
	messages, err := b.drain(ctx, previous)
	if err != nil {
		return err
	}
	// Snapshot the rolling context buffer before folding this iteration's
	// messages into it, so "history" never includes the very messages we're
	// about to reply to.
	history := b.recentSnapshot()
	b.advance(messages)
	b.remember(messages)
	if len(messages) == 0 {
		return nil
	}

	human := make([]schema.MessageV1, 0, len(messages))
	for _, message := range messages {
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
	if err := b.mcp.postMessage(ctx, b.cfg.Channel, reply); err != nil {
		return fmt.Errorf("post reply: %w", err)
	}
	return nil
}

func (b *botLoop) drain(ctx context.Context, after int64) ([]schema.MessageV1, error) {
	var messages []schema.MessageV1
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

func (b *botLoop) advance(messages []schema.MessageV1) {
	for _, message := range messages {
		if message.ID > b.lastSeen {
			b.lastSeen = message.ID
		}
	}
}

// remember folds newly drained messages into the rolling context buffer,
// capped at cfg.ContextMessages. This replaces re-draining the channel from
// message id 0 on every poll: the bot already sees every message exactly
// once as it drains new ones, so it can keep its own bounded window instead
// of asking conchd to replay the whole channel each time it needs context.
func (b *botLoop) remember(messages []schema.MessageV1) {
	if b.cfg.ContextMessages == 0 || len(messages) == 0 {
		return
	}
	b.recent = append(b.recent, messages...)
	if len(b.recent) > b.cfg.ContextMessages {
		trimmed := make([]schema.MessageV1, b.cfg.ContextMessages)
		copy(trimmed, b.recent[len(b.recent)-b.cfg.ContextMessages:])
		b.recent = trimmed
	}
}

func (b *botLoop) recentSnapshot() []schema.MessageV1 {
	if len(b.recent) == 0 {
		return nil
	}
	out := make([]schema.MessageV1, len(b.recent))
	copy(out, b.recent)
	return out
}

func buildPrompt(history, messages []schema.MessageV1, contextLimit int) string {
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

func writeMessages(out *strings.Builder, messages []schema.MessageV1) {
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
