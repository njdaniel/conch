package approvals

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

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
	return n.post(ctx, n.approvalsTopic, "Approval requested: "+a.Title, "default", body)
}

func (n *NtfyNotifier) ApprovalEscalated(ctx context.Context, a store.Approval) error {
	if n == nil || n.urgentTopic == "" {
		return nil
	}
	body := fmt.Sprintf("Deadline passed for approval %d\nRequester: principal:%d\nChannel: %d\nDeadline: %s\n\n%s",
		a.ID, a.RequesterID, a.ChannelID, a.Deadline.UTC().Format(time.RFC3339), a.Body)
	return n.post(ctx, n.urgentTopic, "URGENT approval escalated: "+a.Title, "max", body)
}

func (n *NtfyNotifier) ApprovalResolved(ctx context.Context, a store.Approval, r schema.ApprovalResolutionV1) error {
	if n == nil || n.approvalsTopic == "" {
		return nil
	}
	body := fmt.Sprintf("Approval %d resolved: %s\nOption: %s\nDecisions: %d", a.ID, r.Outcome, r.OptionID, len(r.Decisions))
	return n.post(ctx, n.approvalsTopic, "Approval resolved: "+a.Title, "default", body)
}

func (n *NtfyNotifier) post(ctx context.Context, topic, title, priority, body string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.server+"/"+url.PathEscape(topic), bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", priority)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: status %d", resp.StatusCode)
	}
	return nil
}
