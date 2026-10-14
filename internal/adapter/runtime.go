package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/njdaniel/conch/pkg/schema"
)

// codexCommand is what the codex profile runs when CONCH_ADAPTER_CMD is not
// set. There is no {prompt}: the prompt goes to standard input. The sandbox is
// read-only on purpose (decided on issue #343): a reply agent reads and
// answers. Anything wider is reached only by writing CONCH_ADAPTER_CMD
// yourself.
const codexCommand = "codex exec --json --model {model} --sandbox read-only --skip-git-repo-check --ephemeral --color never"

// readerKind says how a profile's command output is read.
type readerKind int

const (
	// readerText: every line is reply text (and, with the fixture on, a
	// marker).
	readerText readerKind = iota
	// readerCodexJSONL: every line is an event of `codex exec --json`.
	readerCodexJSONL
)

// runtimeProfile is what the adapter knows about one kind of runtime: how to
// run it by default and how to read what it prints.
type runtimeProfile struct {
	name    string
	command string
	// model is the default model; empty means CONCH_ADAPTER_MODEL is required,
	// because the adapter will not invent a model name for a paid service.
	model string
	// binEnv names the variable that replaces the first word of the default
	// command, and only of the default command.
	binEnv string
	// workdirFlag is the flag that hands the working directory to the default
	// command as well; empty if the command has none.
	workdirFlag string
	reader      readerKind
}

// streamsActivity reports whether the reader reports the command's own steps
// as activity, in place of the one started/finished pair of the process.
func (p runtimeProfile) streamsActivity() bool { return p.reader != readerText }

var runtimeProfiles = map[string]runtimeProfile{
	"claude-code": {name: "claude-code", command: defaultCommand, model: "sonnet", binEnv: "CLAUDE_BIN", reader: readerText},
	"codex":       {name: "codex", command: codexCommand, binEnv: "CODEX_BIN", workdirFlag: "-C", reader: readerCodexJSONL},
}

// profileFor returns the profile CONCH_ADAPTER_RUNTIME names. A name that is
// no profile is only a label: it gets Claude Code's default command and the
// text reader, as before profiles existed (the fixture runner's label
// `fixture` relies on it).
func profileFor(name string) runtimeProfile {
	if p, ok := runtimeProfiles[name]; ok {
		return p
	}
	p := runtimeProfiles["claude-code"]
	p.name = name
	return p
}

const (
	// maxNonJSONLines is how many lines of a JSONL stream may be something
	// else before the stream is taken not to be one.
	maxNonJSONLines = 100
	// maxCommandWordRunes bounds the one word an activity entry may say about
	// a command.
	maxCommandWordRunes = 64
)

// errNotJSONL ends a run whose command printed too much that was not JSON. Its
// text is the run's outcome.
var errNotJSONL = errors.New("codex output not JSONL")

// activitySink is a run that takes activity entries from the reader of a
// command's output. The run a fixtureHandler belongs to is one.
type activitySink interface {
	activity(ctx context.Context, kind schema.ActivityKind, summary, outcome string)
}

// codexEvent is the part of a line of `codex exec --json` the reader looks at.
// Codex owns this shape, not Conch: it is read here and nowhere else, and
// anything it does not name is ignored.
type codexEvent struct {
	Type string     `json:"type"`
	Item *codexItem `json:"item"`
}

type codexItem struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Command  string `json:"command"`
	Text     string `json:"text"`
	ExitCode *int   `json:"exit_code"`
	Status   string `json:"status"`
	Tool     string `json:"tool"`
}

// word is the one thing an activity entry says about a step: the command's
// first word without its directory, or a tool's name. Never an argument.
func (i codexItem) word() string {
	raw := i.Tool
	if i.Type == "command_execution" {
		raw = ""
		if fields := strings.Fields(i.Command); len(fields) > 0 {
			raw = filepath.Base(fields[0])
		}
	}
	raw = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return r
		}
		return -1
	}, raw)
	if r := []rune(raw); len(r) > maxCommandWordRunes {
		raw = string(r[:maxCommandWordRunes])
	}
	if raw == "" {
		return "command"
	}
	return raw
}

// outcome is how the step ended: the exit status of a command, else the state
// codex gives.
func (i codexItem) outcome() string {
	if i.ExitCode != nil {
		return "exit " + strconv.Itoa(*i.ExitCode)
	}
	if i.Status == "" {
		return "unknown"
	}
	return i.Status
}

// readCodex reads the events of `codex exec --json`, one per line. A command or
// tool call becomes a command_started and a command_finished entry; the last
// agent message is the reply; everything else is ignored. Fixture markers are
// never read: a model's words cannot raise an approval.
func (c commandRunner) readCodex(ctx context.Context, stdout io.Reader, fx fixtureHandler) (string, error) {
	sink, _ := fx.(activitySink)
	emit := func(kind schema.ActivityKind, summary, outcome string) {
		if sink != nil {
			sink.activity(ctx, kind, summary, outcome)
		}
	}
	var (
		reply   string
		ignored int
		open    = map[string]bool{}
	)
	reader := bufio.NewReaderSize(stdout, 64<<10)
	for {
		line, overlong, err := readBoundedLine(reader, maxReplyBytes)
		if len(line) > 0 || overlong {
			var event codexEvent
			switch {
			case overlong, json.Unmarshal(line, &event) != nil:
				if ignored++; ignored > maxNonJSONLines {
					return "", errNotJSONL
				}
			case event.Item != nil:
				item := event.Item
				switch {
				case event.Type == "item.completed" && item.Type == "agent_message":
					reply = item.Text
				case item.Type == "command_execution" || item.Type == "mcp_tool_call":
					if event.Type == "item.started" && !open[item.ID] {
						open[item.ID] = true
						emit(schema.ActivityKindCommandStarted, "started "+item.word(), "")
					}
					if event.Type == "item.completed" {
						if !open[item.ID] {
							emit(schema.ActivityKindCommandStarted, "started "+item.word(), "")
						}
						delete(open, item.ID)
						emit(schema.ActivityKindCommandFinished, "command finished", item.outcome())
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return reply, nil
			}
			return "", fmt.Errorf("read command output: %w", err)
		}
	}
}

// readBoundedLine reads one line without its line break. A line longer than
// limit is read to its end and dropped, and overlong says so: one huge tool
// output must not fail a run, and must not fill memory either.
func readBoundedLine(r *bufio.Reader, limit int) (line []byte, overlong bool, err error) {
	for {
		part, isPrefix, readErr := r.ReadLine()
		if !overlong {
			if len(line)+len(part) > limit {
				line, overlong = nil, true
			} else {
				line = append(line, part...)
			}
		}
		if readErr != nil || !isPrefix {
			return line, overlong, readErr
		}
	}
}
