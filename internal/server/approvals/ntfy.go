package approvals

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const defaultNotifyTimeout = 2 * time.Second

// NtfyConfig configures the optional ntfy approval notification integration.
// Empty topic fields disable delivery for that lifecycle class.
type NtfyConfig struct {
	Server         string
	ApprovalsTopic string
	UrgentTopic    string
	Timeout        time.Duration
}

// Configured reports whether any ntfy topic is configured.
func (c NtfyConfig) Configured() bool {
	return strings.TrimSpace(c.Server) != "" && (strings.TrimSpace(c.ApprovalsTopic) != "" || strings.TrimSpace(c.UrgentTopic) != "")
}

// NtfyNotifier delivers approval lifecycle notifications using ntfy's stdlib
// HTTP API. It is optional: callers record failures but never depend on them.
type NtfyNotifier struct {
	server         string
	approvalsTopic string
	urgentTopic    string
	client         *http.Client
}

func NewNtfyNotifier(cfg NtfyConfig) (*NtfyNotifier, error) {
	server := strings.TrimRight(strings.TrimSpace(cfg.Server), "/")
	if server == "" {
		return nil, nil
	}
	if _, err := url.ParseRequestURI(server); err != nil {
		return nil, fmt.Errorf("approvals: invalid ntfy server %q: %w", cfg.Server, err)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultNotifyTimeout
	}
	return &NtfyNotifier{
		server:         server,
		approvalsTopic: strings.Trim(strings.TrimSpace(cfg.ApprovalsTopic), "/"),
		urgentTopic:    strings.Trim(strings.TrimSpace(cfg.UrgentTopic), "/"),
		client:         &http.Client{Timeout: timeout},
	}, nil
}

func (n *NtfyNotifier) ApprovalCreated(ctx context.Context, a store.Approval) error {
	if n == nil || n.approvalsTopic == "" {
		return nil
	}
	body := fmt.Sprintf("%s\nRequester: principal:%d\nChannel: %d\nDeadline: %s\n\n%s",
		a.Title, a.RequesterID, a.ChannelID, a.Deadline.UTC().Format(time.RFC3339), a.Body)
	return n.post(ctx, n.approvalsTopic, "Approval requested: "+a.Title, "default", notificationBody(body, a.ID))
}

func (n *NtfyNotifier) ApprovalEscalated(ctx context.Context, a store.Approval) error {
	if n == nil || n.urgentTopic == "" {
		return nil
	}
	body := fmt.Sprintf("Deadline passed for approval %d\nRequester: principal:%d\nChannel: %d\nDeadline: %s\n\n%s",
		a.ID, a.RequesterID, a.ChannelID, a.Deadline.UTC().Format(time.RFC3339), a.Body)
	return n.post(ctx, n.urgentTopic, "URGENT approval escalated: "+a.Title, "max", notificationBody(body, a.ID))
}

func (n *NtfyNotifier) ApprovalResolved(ctx context.Context, a store.Approval, r schema.ApprovalResolutionV1) error {
	if n == nil || n.approvalsTopic == "" {
		return nil
	}
	body := fmt.Sprintf("Approval %d resolved: %s\nOption: %s\nDecisions: %d", a.ID, r.Outcome, r.OptionID, len(r.Decisions))
	return n.post(ctx, n.approvalsTopic, "Approval resolved: "+a.Title, "default", notificationBody(body, a.ID))
}

func (n *NtfyNotifier) post(ctx context.Context, topic, title, priority, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.server+"/"+url.PathEscape(topic), bytes.NewBufferString(body))
	if err != nil {
		// The error would quote the URL, topic and all.
		return errors.New("ntfy: build request")
	}
	req.Header.Set("Title", headerValue(title))
	req.Header.Set("Priority", priority)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := n.client.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: %w", transportCause(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: status %d", resp.StatusCode)
	}
	return nil
}

// maxTitleBytes caps the Title header. A title is user text of any length,
// and a header has to fit in the server's header limit with room to spare.
const maxTitleBytes = 250

// headerValue makes user text safe to send as an HTTP header value (issue
// #157). An approval's title goes into ntfy's Title header, and Go's HTTP
// client refuses to send a value containing a control character: with a
// newline in the title the request never left conchd, so anyone who could
// raise an approval could raise one nobody was pushed about. Each run of
// control characters and spaces becomes one space, so the words on either side
// stay apart and nothing can start a second header line. Non-ASCII text is
// sent as it is: ntfy reads UTF-8 in headers. The body of the notification
// carries the title unchanged.
//
// ntfy also decodes RFC 2047 encoded-words ("=?UTF-8?Q?...?=") in header
// values, after HTTP parsing. A title written that way is plain ASCII here and
// would turn into whatever it encodes on ntfy's side, control characters
// included. The "=?" that opens an encoded-word is split so the title is shown
// as it was typed.
func headerValue(s string) string {
	s = strings.ReplaceAll(s, "=?", "= ?")
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		if b.Len()+utf8.RuneLen(r) > maxTitleBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " ")
}

// maxBodyBytes caps a notification's body. ntfy treats a body over 4,096
// bytes as an attachment, and a server without attachments configured (the
// self-hosted default) answers 400: the push never happens. An approval's
// body is user text of up to a megabyte, so without a cap anyone who could
// raise an approval could raise one nobody was pushed about (issue #164).
// The cap leaves room below ntfy's limit for the mark that says it was cut.
const maxBodyBytes = 3800

// notificationBody is body made to fit one ntfy message: valid UTF-8 (ntfy
// treats anything else as an attachment too), cut on a character boundary at
// maxBodyBytes, with a last line saying so and where the rest is. A body that
// fits is returned as it is.
func notificationBody(body string, approvalID int64) string {
	body = strings.ToValidUTF8(body, "\uFFFD")
	if len(body) <= maxBodyBytes {
		return body
	}
	cut := maxBodyBytes
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + fmt.Sprintf("\n[cut here; the full text is in approval %d]", approvalID)
}

// transportCause reduces an error from the HTTP client to what went wrong,
// without where. The client's own error quotes the request URL, whose path is
// the topic: on a public ntfy server the topic is what lets someone read the
// notifications, and this error is written to the audit log and to the server
// log. A dial error likewise names the host. Neither helps an operator who
// already knows their own configuration; the cause does.
func transportCause(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		return fmt.Errorf("%s: %w", op.Op, op.Err)
	}
	return err
}
