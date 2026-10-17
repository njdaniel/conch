package hub

import (
	"context"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestSubscriptionV2Live(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		end  func(h *Hub, sub *SubscriptionV2)
	}{
		{"cancelled", func(_ *Hub, sub *SubscriptionV2) { sub.Cancel() }},
		{"overflowed", func(h *Hub, _ *SubscriptionV2) {
			// A buffer of 1 holds one frame; the second finds it full.
			h.BroadcastMessageV1(ctx, schema.MessageV1{ChannelID: 1, ID: 1})
			h.BroadcastMessageV1(ctx, schema.MessageV1{ChannelID: 1, ID: 2})
		}},
		{"principal dropped", func(h *Hub, _ *SubscriptionV2) { h.DropPrincipal(1, 7) }},
		{"hub closed", func(h *Hub, _ *SubscriptionV2) { h.Close() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New()
			sub := h.SubscribeV2(1, 7, 0, 1)
			defer sub.Cancel()
			other := h.SubscribeV2(2, 7, 0, 1)
			defer other.Cancel()
			if !sub.Live() {
				t.Fatal("a fresh subscription is not live")
			}
			tt.end(h, sub)
			if sub.Live() {
				t.Error("subscription still live after it was ended")
			}
		})
	}

	t.Run("one subscription's end leaves the others live", func(t *testing.T) {
		h := New()
		a, b := h.SubscribeV2(1, 7, 0, 1), h.SubscribeV2(1, 8, 0, 1)
		defer a.Cancel()
		defer b.Cancel()
		a.Cancel()
		if a.Live() || !b.Live() {
			t.Errorf("after cancelling a: a.Live = %v, b.Live = %v, want false, true", a.Live(), b.Live())
		}
	})

	t.Run("a subscription taken from a closed hub is not live", func(t *testing.T) {
		h := New()
		h.Close()
		sub := h.SubscribeV2(1, 7, 0, 1)
		defer sub.Cancel()
		if sub.Live() {
			t.Error("subscription from a closed hub is live")
		}
	})
}
