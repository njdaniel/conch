package server

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// An agent cannot upload files, so a post_message carrying a file attachment
// can never be honoured: every file it names fails the "uploaded by the
// author" check. That is the caller's mistake and must be answered as one,
// with the same code the REST post routes give, not as a server failure with
// an error line in the log that any agent with the post grant could produce at
// will (issue #240).
func TestMCPPostWithFileAttachmentIsRefusedAsInvalid(t *testing.T) {
	f := newFileFixture(t)
	// A real file, uploaded by a human and never attached: the agent must not
	// be able to attach it either.
	theirs := f.mustUpload(t, "ann", "plan.txt", []byte("ann's file"))
	f.manifest(t, "aria", nil, schema.ChannelPermissionRead, schema.ChannelPermissionPost,
		schema.ChannelPermissionPostNet, schema.ChannelPermissionWhisper)
	f.seat(t, "net1", "aria", schema.NetRoleMember)

	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	attachment := func(id int64) map[string]any {
		return map[string]any{"schema": schema.FileAttachmentV1Name, "data": map[string]any{"file_ids": []int64{id}}}
	}
	posts := map[string]map[string]any{
		"channel-wide, a file that does not exist": {"channel": "ops", "body": "x", "payload": attachment(999999)},
		"channel-wide, someone else's file":        {"channel": "ops", "body": "x", "payload": attachment(theirs.ID)},
		"to a net": {"channel": "ops", "body": "x", "payload": attachment(theirs.ID),
			"audience": map[string]any{"kind": "net", "net_id": f.net1.ID}},
		"a whisper": {"channel": "ops", "body": "x", "payload": attachment(theirs.ID),
			"audience": map[string]any{"kind": "principals", "principal_ids": []int64{f.p("ann").ID}}},
	}
	messages, audit := f.messageCount(t), len(f.audit(t))
	for name, args := range posts {
		out := f.mcpCall(t, f.p("aria").token, "post_message", args)
		if !out.isError || out.code != errInvalidFileAttachment.Code {
			t.Errorf("%s: post_message = %s, want the tool error %s (%s)", name, out.result(), errInvalidFileAttachment.Code, out.body)
		}
	}
	if got := f.messageCount(t); got != messages {
		t.Errorf("refused posts stored %d message(s)", got-messages)
	}
	if got := len(f.audit(t)); got != audit {
		t.Errorf("refused posts wrote %d audit row(s)", got-audit)
	}
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "level=ERROR") || strings.Contains(line, "level=WARN") {
			t.Errorf("a refused post logged: %s", line)
		}
	}
	// The human's file is still hers alone and still unattached.
	if _, err := f.srv.store.FileVisibleTo(t.Context(), theirs.ID, store.Reader{Scoped: true, PrincipalID: f.p("wes").ID}); err == nil {
		t.Error("another member can read the file after the agent's attempts")
	}
	if res := f.do(t, "POST", "/v2/channels/ops/messages", "ann", "application/json", strings.NewReader(attachBody("mine", "", theirs.ID))); res.status != 201 {
		t.Errorf("the uploader attaching her own file afterwards = %s, want 201", res)
	}

	// Every other post_message is as it was.
	if out := f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "an ordinary post"}); out.isError {
		t.Errorf("an ordinary post_message was refused: %s", out.body)
	}
	other := map[string]any{"schema": "test.thing.v1", "data": map[string]any{"k": "v"}}
	if out := f.mcpCall(t, f.p("aria").token, "post_message", map[string]any{"channel": "ops", "body": "with another payload", "payload": other}); out.isError {
		t.Errorf("a post_message with a payload of another kind was refused: %s", out.body)
	}
}
