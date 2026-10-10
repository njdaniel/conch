package approvals

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

const defaultNotifyTimeout = 2 * time.Second

// errNoTopic is what a notification returns when the topic it is published on
// was left empty in the configuration: nothing was sent, and nothing could
// have been. The manager records it as a notification that was not attempted
// (issue #170). It used to be a silent nil, which the manager recorded as
// notify_sent.
var errNoTopic = errors.New("ntfy: no topic configured for this notification")

// NtfyConfig configures the optional ntfy approval notification integration.
// ApprovalsTopic carries the created and the resolved (or expired)
// notification, UrgentTopic the escalation. With a server set, a topic left
// empty means those notifications are not sent, and each is audited as not
// attempted.
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
		// Not the URL: it may carry credentials, and this is logged at start.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("approvals: invalid ntfy server URL: %w", err)
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

// MissingTopics names the settings left empty, by their flag: the
// notifications that use them will not be sent. It is empty when both topics
// are set.
func (n *NtfyNotifier) MissingTopics() []string {
	if n == nil {
		return nil
	}
	var missing []string
	if n.approvalsTopic == "" {
		missing = append(missing, "--ntfy-topic")
	}
	if n.urgentTopic == "" {
		missing = append(missing, "--ntfy-urgent-topic")
	}
	return missing
}

func (n *NtfyNotifier) ApprovalCreated(ctx context.Context, a store.Approval) error {
	if n == nil {
		return nil
	}
	if n.approvalsTopic == "" {
		return errNoTopic
	}
	// What conchd itself says comes first, before any text the requester
	// wrote. The body is cut to fit one ntfy message (notificationBody), and
	// the title and body are user text of any length: put first, a long title
	// could push these lines past the cut and leave only lines of its own
	// making, a forged "Requester:" among them.
	body := fmt.Sprintf("Approval %d\nRequester: principal:%d\nChannel: %d\nDeadline: %s\n\n%s\n\n%s",
		a.ID, a.RequesterID, a.ChannelID, a.Deadline.UTC().Format(time.RFC3339), a.Title, a.Body)
	return n.post(ctx, n.approvalsTopic, "Approval requested: "+a.Title, "default", notificationBody(body, a.ID))
}

func (n *NtfyNotifier) ApprovalEscalated(ctx context.Context, a store.Approval) error {
	if n == nil {
		return nil
	}
	if n.urgentTopic == "" {
		return errNoTopic
	}
	body := fmt.Sprintf("Deadline passed for approval %d\nRequester: principal:%d\nChannel: %d\nDeadline: %s\n\n%s\n\n%s",
		a.ID, a.RequesterID, a.ChannelID, a.Deadline.UTC().Format(time.RFC3339), a.Title, a.Body)
	return n.post(ctx, n.urgentTopic, "URGENT approval escalated: "+a.Title, "max", notificationBody(body, a.ID))
}

func (n *NtfyNotifier) ApprovalResolved(ctx context.Context, a store.Approval, r schema.ApprovalResolutionV1) error {
	if n == nil {
		return nil
	}
	if n.approvalsTopic == "" {
		return errNoTopic
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

// transportCause reduces an error from the HTTP client to one of a fixed set
// of causes. The client's own error quotes the request URL, whose path is the
// topic: on a public ntfy server the topic is what lets someone read the
// notifications, and this error is written to the audit log and to the server
// log. Dial, DNS and TLS errors name the host; an endpoint that misbehaves can
// reflect the path into errors of almost any shape. So nothing from the
// original text is passed through: the cause is named from a list, and
// anything not on it is "request failed". An operator knows their own
// configuration; what they need is which kind of failure it was.
func transportCause(err error) error {
	var (
		dns      *net.DNSError
		netErr   net.Error
		hostname x509.HostnameError
		unknown  x509.UnknownAuthorityError
		invalid  x509.CertificateInvalidError
		verify   *tls.CertificateVerificationError
		record   tls.RecordHeaderError
	)
	switch {
	case errors.Is(err, context.Canceled):
		return errors.New("cancelled")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return errors.New("timeout")
	case errors.As(err, &dns):
		return errors.New("name lookup failed")
	case errors.As(err, &hostname), errors.As(err, &unknown), errors.As(err, &invalid), errors.As(err, &verify), errors.As(err, &record):
		return errors.New("TLS failure")
	case errors.Is(err, syscall.ECONNREFUSED):
		return errors.New("connection refused")
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return errors.New("connection closed")
	case strings.Contains(err.Error(), "stopped after") && strings.Contains(err.Error(), "redirects"):
		return errors.New("too many redirects")
	default:
		return errors.New("request failed")
	}
}
