package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
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

// stopFailure is a stop command's refusal as the one line it prints, in the
// client's words only. A server refusal is worded by lines or the common
// sentences; the answers that carry no error document (a redirect, whose
// Location is the server's, a refusal whose reason phrase is the server's, a
// 2xx that is not the 204 a stop gets) are told by their numeric status alone.
func stopFailure(err error, lines refusalLines) error {
	var (
		redirect   *redirectError
		status     *statusError
		unexpected *unexpectedStatusError
	)
	var line string
	switch {
	case badRedirect(err):
		line = "the server redirected the request to a location this client cannot follow; set --server to the final URL"
	case errors.As(err, &redirect):
		line = fmt.Sprintf("the server redirected the request (HTTP %d); set --server to the final URL", redirect.status)
	case errors.As(err, &status):
		line = fmt.Sprintf("the server refused this (HTTP %d)", status.status)
	case errors.As(err, &unexpected):
		line = fmt.Sprintf("the server answered unexpectedly (HTTP %d); the stop may not have happened", unexpected.status)
	default:
		return voiceFailure(refused(err, lines))
	}
	return &serverText{msg: line, err: err}
}

// badRedirect reports whether err is net/http refusing a redirect whose
// Location it could not parse. The client fails then, before it hands the
// response over, with the Location quoted in the error: no type tells it from
// other *url.Error failures (the cause is a plain error), so it is the
// *url.Error and the standard library's own prefix together.
func badRedirect(err error) bool {
	var ue *url.Error
	return errors.As(err, &ue) && ue.Err != nil && strings.HasPrefix(ue.Err.Error(), "failed to parse Location header")
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
		return stopFailure(err, shareStopRefusals)
	}
	return nil
}
