package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/njdaniel/conch/internal/mcpclient"
	"github.com/njdaniel/conch/pkg/schema"
)

type config struct {
	Server          string
	Token           string
	PrincipalID     int64
	Channel         string
	PollInterval    time.Duration
	MaxBackoff      time.Duration
	ContextMessages int
	Model           string
	ReplyTimeout    time.Duration
	ClaudeBin       string
	LockFile        string
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "conch-bot:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig(os.LookupEnv)
	if err != nil {
		return err
	}
	release, err := acquireLock(cfg.LockFile)
	if err != nil {
		return err
	}
	defer release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	client := mcpclient.New(strings.TrimRight(cfg.Server, "/"), cfg.Token)
	if err := client.Initialize(ctx, "conch-bot"); err != nil {
		return fmt.Errorf("initialize MCP: %w", err)
	}
	loop := &botLoop{cfg: cfg, mcp: mcpAdapter{client}, claude: commandClaude{cfg: cfg}}
	return loop.run(ctx)
}

type mcpAdapter struct{ client *mcpclient.Client }

func (m mcpAdapter) readChannel(ctx context.Context, channel string, after int64, limit int) (schema.ListMessagesResponseV1, error) {
	return m.client.ReadChannel(ctx, channel, after, limit)
}

func (m mcpAdapter) postMessage(ctx context.Context, channel, body string) error {
	_, err := m.client.PostMessage(ctx, channel, body)
	return err
}

type commandClaude struct{ cfg config }

func (c commandClaude) Reply(ctx context.Context, prompt string) (string, error) {
	replyCtx, cancel := context.WithTimeout(ctx, c.cfg.ReplyTimeout)
	defer cancel()
	cmd := exec.CommandContext(replyCtx, c.cfg.ClaudeBin, "-p", prompt, "--model", c.cfg.Model, "--dangerously-skip-permissions", "--output-format", "text") // #nosec G204 -- operator explicitly configures the local Claude executable
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if errors.Is(replyCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("claude timed out after %s", c.cfg.ReplyTimeout)
		}
		return "", fmt.Errorf("claude: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

type envLookup func(string) (string, bool)

func loadConfig(lookup envLookup) (config, error) {
	get := func(key, fallback string) string {
		if value, ok := lookup(key); ok && value != "" {
			return value
		}
		return fallback
	}
	required := func(key string) (string, error) {
		value := get(key, "")
		if value == "" {
			return "", fmt.Errorf("%s is required", key)
		}
		return value, nil
	}
	var cfg config
	var err error
	cfg.Server = get("CONCH_BOT_SERVER", "http://127.0.0.1:8080")
	if cfg.Token, err = required("CONCH_BOT_TOKEN"); err != nil {
		return config{}, err
	}
	principal, err := required("CONCH_BOT_PRINCIPAL_ID")
	if err != nil {
		return config{}, err
	}
	cfg.PrincipalID, err = strconv.ParseInt(principal, 10, 64)
	if err != nil || cfg.PrincipalID <= 0 {
		return config{}, fmt.Errorf("CONCH_BOT_PRINCIPAL_ID must be a positive integer")
	}
	if cfg.Channel, err = required("CONCH_BOT_CHANNEL"); err != nil {
		return config{}, err
	}
	if strings.Contains(cfg.Channel, ",") {
		return config{}, fmt.Errorf("CONCH_BOT_CHANNEL must contain exactly one channel, not a comma-separated list")
	}
	if cfg.PollInterval, err = parseDuration(get("CONCH_BOT_POLL_INTERVAL", "5s"), "CONCH_BOT_POLL_INTERVAL"); err != nil {
		return config{}, err
	}
	if cfg.MaxBackoff, err = parseDuration(get("CONCH_BOT_MAX_BACKOFF", "60s"), "CONCH_BOT_MAX_BACKOFF"); err != nil {
		return config{}, err
	}
	contextMessages, err := strconv.Atoi(get("CONCH_BOT_CONTEXT_MESSAGES", "20"))
	if err != nil || contextMessages < 0 {
		return config{}, fmt.Errorf("CONCH_BOT_CONTEXT_MESSAGES must be a non-negative integer")
	}
	cfg.ContextMessages = contextMessages
	cfg.Model = get("CONCH_BOT_MODEL", "sonnet")
	if cfg.ReplyTimeout, err = parseDuration(get("CONCH_BOT_REPLY_TIMEOUT", "120s"), "CONCH_BOT_REPLY_TIMEOUT"); err != nil {
		return config{}, err
	}
	cfg.ClaudeBin = get("CLAUDE_BIN", "claude")
	cfg.LockFile = get("CONCH_BOT_LOCK_FILE", filepath.Join(os.TempDir(), "conch-bot.lock"))
	return cfg, nil
}

func parseDuration(value, key string) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return duration, nil
}

func acquireLock(path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator explicitly configures the lock path
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("another instance holds lock file %s", path)
		}
		return nil, fmt.Errorf("acquire lock %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return func() {
		if err := file.Close(); err != nil {
			slog.Warn("close lock file", "error", err)
		}
		if err := os.Remove(path); err != nil {
			slog.Warn("remove lock file", "error", err)
		}
	}, nil
}
