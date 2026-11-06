package main

import (
	"errors"
	"testing"

	"github.com/coder/websocket"
)

func TestClosedAllExpired(t *testing.T) {
	closed := func(what string, code websocket.StatusCode, reason string) *feed {
		return &feed{what: what, err: websocket.CloseError{Code: code, Reason: reason}}
	}
	const events, other = "GET /v1/events/ws", "GET /v1/presence/ws"
	tests := []struct {
		name       string
		feed       *feed
		wantExpiry bool // closedAllExpired accepts it
		wantStrict bool // closedAllRevoked accepts it
	}{
		{"events, credential", closed(events, websocket.StatusPolicyViolation, credentialGone), true, true},
		{"events, membership", closed(events, websocket.StatusPolicyViolation, membershipGone), true, false},
		{"events, other reason", closed(events, websocket.StatusPolicyViolation, "manifest no longer permits reading this channel"), false, false},
		{"events, wrong code", closed(events, websocket.StatusGoingAway, credentialGone), false, false},
		{"events, still open", &feed{what: events}, false, false},
		{"events, non-close error", &feed{what: events, err: errors.New("boom")}, false, false},
		{"other, credential", closed(other, websocket.StatusPolicyViolation, credentialGone), true, true},
		{"other, membership", closed(other, websocket.StatusPolicyViolation, membershipGone), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := closedAllExpired([]*feed{tt.feed}); (err == nil) != tt.wantExpiry {
				t.Errorf("closedAllExpired = %v, want accepted=%v", err, tt.wantExpiry)
			}
			if err := closedAllRevoked([]*feed{tt.feed}); (err == nil) != tt.wantStrict {
				t.Errorf("closedAllRevoked = %v, want accepted=%v", err, tt.wantStrict)
			}
		})
	}
}
