package adapter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	// MaxPause is how long a run may stay paused before it is failed.
	MaxPause time.Duration
	LockFile string
	// Command is the command that makes a reply, split, with CLAUDE_BIN
	// already in place of its first word.
	Command             commandSpec
	Runtime             string
	Heartbeat           time.Duration
	MaxRepliesPerMinute int
	// Fixture turns on the fixture markers in the command's output. Never set
	// it for a real model.
	Fixture bool
	// Reader is how the command's output is read, from the runtime's profile.
	Reader readerKind
	// StreamsActivity is set when that reader reports the command's own steps
	// as activity, so the run does not add one pair for the process.
	StreamsActivity bool
	// WorkDir is the command's working directory; empty means the adapter's.
	WorkDir string
}

// Main is the program: cmd/conch-adapter, and cmd/conch-bot under its old
// name, both call it. It reads its settings from the environment and exits the
// process with status 1 on failure.
func Main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "conch-adapter:", err)
		os.Exit(1)
	}
}

func run() error {
	// First, before the token is used or any child is started.
	hardenProcess()
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
	if err := client.Initialize(ctx, "conch-adapter"); err != nil {
		return fmt.Errorf("initialize MCP: %w", err)
	}
	runner := commandRunner{spec: cfg.Command, model: cfg.Model, timeout: cfg.ReplyTimeout, fixture: cfg.Fixture, sleep: sleepContext, reader: cfg.Reader, dir: cfg.WorkDir, maxPause: cfg.MaxPause}
	loop := &botLoop{cfg: cfg, mcp: mcpAdapter{client}, claude: runner}
	return loop.run(ctx)
}

// mcpAdapter is the client with the two channel calls the loop makes in terms
// of its own types.
type mcpAdapter struct{ *mcpclient.Client }

func (m mcpAdapter) readChannel(ctx context.Context, channel string, after int64, limit int) (schema.ListMessagesResponseV2, error) {
	return m.ReadChannel(ctx, channel, after, limit)
}

func (m mcpAdapter) postMessage(ctx context.Context, channel, body string, audience *schema.Audience) error {
	_, err := m.PostMessageTo(ctx, channel, body, audience)
	return err
}

type envLookup func(string) (string, bool)

// loadConfig reads the settings. Each CONCH_ADAPTER_* name falls back to the
// CONCH_BOT_* name conch-bot used, so a setup written for conch-bot keeps
// working; the new name wins when both are set.
func loadConfig(lookup envLookup) (config, error) {
	get := func(key, legacy, fallback string) string {
		if value, ok := lookup(key); ok && value != "" {
			return value
		}
		if legacy != "" {
			if value, ok := lookup(legacy); ok && value != "" {
				return value
			}
		}
		return fallback
	}
	required := func(key, legacy string) (string, error) {
		value := get(key, legacy, "")
		if value == "" {
			return "", fmt.Errorf("%s (or %s) is required", key, legacy)
		}
		return value, nil
	}
	var cfg config
	var err error
	cfg.Server = get("CONCH_ADAPTER_SERVER", "CONCH_BOT_SERVER", "http://127.0.0.1:8080")
	if cfg.Token, err = required("CONCH_ADAPTER_TOKEN", "CONCH_BOT_TOKEN"); err != nil {
		return config{}, err
	}
	principal, err := required("CONCH_ADAPTER_PRINCIPAL_ID", "CONCH_BOT_PRINCIPAL_ID")
	if err != nil {
		return config{}, err
	}
	cfg.PrincipalID, err = strconv.ParseInt(principal, 10, 64)
	if err != nil || cfg.PrincipalID <= 0 {
		return config{}, fmt.Errorf("CONCH_ADAPTER_PRINCIPAL_ID must be a positive integer")
	}
	if cfg.Channel, err = required("CONCH_ADAPTER_CHANNEL", "CONCH_BOT_CHANNEL"); err != nil {
		return config{}, err
	}
	if strings.Contains(cfg.Channel, ",") {
		return config{}, fmt.Errorf("CONCH_ADAPTER_CHANNEL must contain exactly one channel, not a comma-separated list")
	}
	if cfg.PollInterval, err = parseDuration(get("CONCH_ADAPTER_POLL_INTERVAL", "CONCH_BOT_POLL_INTERVAL", "5s"), "CONCH_ADAPTER_POLL_INTERVAL"); err != nil {
		return config{}, err
	}
	if cfg.MaxBackoff, err = parseDuration(get("CONCH_ADAPTER_MAX_BACKOFF", "CONCH_BOT_MAX_BACKOFF", "60s"), "CONCH_ADAPTER_MAX_BACKOFF"); err != nil {
		return config{}, err
	}
	contextMessages, err := strconv.Atoi(get("CONCH_ADAPTER_CONTEXT_MESSAGES", "CONCH_BOT_CONTEXT_MESSAGES", "20"))
	if err != nil || contextMessages < 0 {
		return config{}, fmt.Errorf("CONCH_ADAPTER_CONTEXT_MESSAGES must be a non-negative integer")
	}
	cfg.ContextMessages = contextMessages
	cfg.Runtime = get("CONCH_ADAPTER_RUNTIME", "", "claude-code")
	profile, knownProfile := profileFor(cfg.Runtime)
	cfg.Reader, cfg.StreamsActivity = profile.reader, profile.streamsActivity()
	if cfg.Model = get("CONCH_ADAPTER_MODEL", "CONCH_BOT_MODEL", profile.model); cfg.Model == "" {
		return config{}, fmt.Errorf("CONCH_ADAPTER_MODEL (or CONCH_BOT_MODEL) is required when CONCH_ADAPTER_RUNTIME is %s", profile.name)
	}
	if cfg.ReplyTimeout, err = parseDuration(get("CONCH_ADAPTER_REPLY_TIMEOUT", "CONCH_BOT_REPLY_TIMEOUT", "120s"), "CONCH_ADAPTER_REPLY_TIMEOUT"); err != nil {
		return config{}, err
	}
	if cfg.MaxPause, err = parseDuration(get("CONCH_ADAPTER_MAX_PAUSE", "", cfg.ReplyTimeout.String()), "CONCH_ADAPTER_MAX_PAUSE"); err != nil {
		return config{}, err
	}
	cfg.LockFile = get("CONCH_ADAPTER_LOCK_FILE", "CONCH_BOT_LOCK_FILE", filepath.Join(os.TempDir(), "conch-adapter.lock"))
	if dir := get("CONCH_ADAPTER_WORKDIR", "", ""); dir != "" {
		// Absolute, so that it means the same to the adapter and to the command.
		if cfg.WorkDir, err = filepath.Abs(dir); err != nil {
			return config{}, fmt.Errorf("CONCH_ADAPTER_WORKDIR: %w", err)
		}
	}
	// The profile's binary variable (CLAUDE_BIN, CODEX_BIN) replaces the first
	// word of the default command only. A command someone configured is run as
	// written: otherwise a CLAUDE_BIN left over from conch-bot would turn a
	// fixture script into the real model, with the fixture markers on (security
	// review of #275). The reader stays the profile's either way.
	line, binary := get("CONCH_ADAPTER_CMD", "", ""), ""
	isDefault := line == ""
	if isDefault {
		if !knownProfile {
			return config{}, fmt.Errorf("CONCH_ADAPTER_RUNTIME %q is no runtime profile (%s); set CONCH_ADAPTER_CMD to use it as a label", cfg.Runtime, profileNames())
		}
		line, binary = profile.command, get(profile.binEnv, "", "")
		if binary != "" && strings.ContainsRune(binary, filepath.Separator) && !filepath.IsAbs(binary) {
			// A relative path would be read against the workdir, not the
			// directory it was written for.
			return config{}, fmt.Errorf("%s must be a bare program name or an absolute path", profile.binEnv)
		}
	}
	if cfg.Command, err = parseCommand(line, binary); err != nil {
		return config{}, fmt.Errorf("CONCH_ADAPTER_CMD: %w", err)
	}
	if isDefault && cfg.WorkDir != "" && profile.workdirFlag != "" {
		cfg.Command.args = append(cfg.Command.args, profile.workdirFlag, cfg.WorkDir)
	}
	if cfg.Heartbeat, err = parseDuration(get("CONCH_ADAPTER_HEARTBEAT", "", "10s"), "CONCH_ADAPTER_HEARTBEAT"); err != nil {
		return config{}, err
	}
	perMinute, err := strconv.Atoi(get("CONCH_ADAPTER_MAX_REPLIES_PER_MINUTE", "", "10"))
	if err != nil || perMinute < 1 {
		return config{}, fmt.Errorf("CONCH_ADAPTER_MAX_REPLIES_PER_MINUTE must be a positive integer")
	}
	cfg.MaxRepliesPerMinute = perMinute
	cfg.Fixture = get("CONCH_ADAPTER_FIXTURE", "", "") == "1"
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
