package hub

import (
	"context"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

// TestDropCredential: revoking one credential (issue #519) closes the
// subscriptions opened with it, every version on every channel, and none of
// the same principal's through another credential.
func TestDropCredential(t *testing.T) {
	type subSpec struct {
		channel, principal, credential int64
		version                        int // 0, 1 or 2
	}
	tests := []struct {
		name        string
		subs        []subSpec
		drop        int64
		wantDropped []bool // parallel to subs
		wantCount   int
	}{
		{"every channel and version of the one credential",
			[]subSpec{{1, 7, 70, 0}, {2, 7, 70, 1}, {1, 7, 70, 2}, {3, 7, 70, 2}, {1, 7, 71, 0}, {1, 7, 71, 2}, {1, 8, 80, 1}},
			70, []bool{true, true, true, true, false, false, false}, 4},
		{"credential zero is a no-op",
			[]subSpec{{1, 0, 0, 0}, {2, 0, 0, 1}, {1, 0, 0, 2}, {1, 7, 70, 2}},
			0, []bool{false, false, false, false}, 0},
		{"credential without subscriptions",
			[]subSpec{{1, 7, 71, 2}},
			70, []bool{false}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New()
			closed := make([]func() bool, len(tt.subs))
			for i, s := range tt.subs {
				switch s.version {
				case 0:
					sub := h.Subscribe(s.channel, s.principal, s.credential, 4)
					defer sub.Cancel()
					closed[i] = func() bool { return isClosedV0(sub) }
				case 1:
					sub := h.SubscribeV1(s.channel, s.principal, s.credential, 4)
					defer sub.Cancel()
					closed[i] = func() bool { return isClosedV1(sub) }
				default:
					sub := h.SubscribeV2(s.channel, s.principal, s.credential, 4)
					defer sub.Cancel()
					closed[i] = func() bool { return !sub.Live() }
				}
			}
			if got := h.DropCredential(tt.drop); got != tt.wantCount {
				t.Errorf("DropCredential = %d, want %d", got, tt.wantCount)
			}
			for i := range tt.subs {
				if got := closed[i](); got != tt.wantDropped[i] {
					t.Errorf("sub %d closed = %v, want %v", i, got, tt.wantDropped[i])
				}
			}
			if got := h.DropCredential(tt.drop); got != 0 {
				t.Errorf("second DropCredential = %d, want 0", got)
			}
		})
	}
}

// TestDropCredentialNothingDeliveredAfter: a broadcast after the drop never
// reaches the revoked credential's subscriptions, and still reaches the same
// principal's through another credential.
func TestDropCredentialNothingDeliveredAfter(t *testing.T) {
	ctx := context.Background()
	h := New()
	gone := h.SubscribeV2(1, 7, 70, 4)
	goneV1 := h.SubscribeV1(1, 7, 70, 4)
	kept := h.SubscribeV2(1, 7, 71, 4)
	h.DropCredential(70)
	h.BroadcastMessageV1(ctx, schema.MessageV1{ID: 1, ChannelID: 1})
	if m, ok := <-gone.Messages(); ok {
		t.Errorf("dropped v2 sub received %+v", m)
	}
	if m, ok := <-goneV1.Messages(); ok {
		t.Errorf("dropped v1 sub received %+v", m)
	}
	if m, ok := <-kept.Messages(); !ok || m.ID != 1 {
		t.Errorf("the other credential's sub = %+v, %v; want message 1", m, ok)
	}
}
