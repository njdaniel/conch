package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/njdaniel/conch/pkg/schema"
)

// shareFields is the trailing key=value fields of a sharer's line in voice
// status: when the share started, the quality the sharer declared, and
// whether it carries sound. The quality is checked again here, so a value
// outside the picker's is never shown.
func shareFields(p schema.VoiceParticipantV2) string {
	var b strings.Builder
	if p.ShareStartedAt != nil {
		b.WriteString(" share_since=" + p.ShareStartedAt.Time().UTC().Format(time.RFC3339))
	}
	if q := p.ShareQuality; q != nil && q.Validate() == nil {
		b.WriteString(" quality=" + string(q.Resolution) + "@" + strconv.Itoa(q.FrameRate))
	}
	if p.SharingAudio {
		b.WriteString(" sound")
	}
	return b.String()
}

// shareStopRefusals are voice share stop's own sentences for a refusal, by
// the server's code; what the server wrote is never printed.
var shareStopRefusals = refusalLines{
	schema.ErrorCodeVoiceNotConfigured: errVoiceNotConfigured.Error(),
	schema.ErrorCodeVoiceUnavailable:   errVoiceUnavailable.Error(),
	// A plain member is told forbidden; an operator without the permission is
	// told permission_denied. Either way, say who may.
	"forbidden":                      "only a manager of the channel, or an operator, may stop someone else's share",
	schema.ErrorCodePermissionDenied: "only a manager of the channel, or an operator, may stop someone else's share",
}

func runVoiceShare(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("cli: voice share requires a subcommand (stop)")
	}
	if args[0] != "stop" {
		return fmt.Errorf("cli: unknown voice share subcommand %q", args[0])
	}
	return runVoiceShareStop(ctx, args[1:], stdout, stderr)
}

// runVoiceShareStop ends one person's screen share: DELETE
// /v1/channels/{channel}/voice/share?principal=<id>. It prints nothing on
// success, which also does not say whether that person was sharing (the route
// never says whether the person exists).
func runVoiceShareStop(ctx context.Context, args []string, _, stderr io.Writer) error {
	fs := newFlagSet("voice share stop", stderr)
	server := fs.String("server", serverEnvOr(), "conchd HTTP URL")
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return fmt.Errorf("cli: voice share stop: %w", err)
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 2 {
		return errors.New("cli: voice share stop: expected <channel> <person-id>")
	}
	id, err := strconv.ParseInt(positional[1], 10, 64)
	if err != nil || id <= 0 || positional[1] != strconv.FormatInt(id, 10) {
		return errors.New("cli: voice share stop: <person-id> must be a principal id (a positive number)")
	}
	client, _, err := NewAuthClient(*server)
	if err != nil {
		return err
	}
	if err := client.StopVoiceShare(ctx, positional[0], id); err != nil {
		return voiceFailure(refused(err, shareStopRefusals))
	}
	return nil
}
