package hub

import (
	"context"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func TestDropPrincipalAll(t *testing.T) {
	type subSpec struct {
		channel, principal int64
		v1                 bool
	}
	tests := []struct {
		name        string
		subs        []subSpec
		drop        int64
		wantDropped []bool // parallel to subs
		wantCount   int
	}{
		{"every channel, v0 and v1, several sockets",
			[]subSpec{{1, 7, false}, {2, 7, true}, {1, 7, true}, {3, 7, false}, {1, 8, false}, {2, 8, true}},
			7, []bool{true, true, true, true, false, false}, 4},
		{"principal zero is a no-op",
			[]subSpec{{1, 0, false}, {2, 0, true}, {1, 7, false}},
			0, []bool{false, false, false}, 0},
		{"principal without subscriptions",
			[]subSpec{{1, 8, false}},
			7, []bool{false}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New()
			v0 := map[int]*Subscription{}
			v1 := map[int]*SubscriptionV1{}
			for i, s := range tt.subs {
				if s.v1 {
					v1[i] = h.SubscribeV1(s.channel, s.principal, 4)
					defer v1[i].Cancel()
				} else {
					v0[i] = h.Subscribe(s.channel, s.principal, 4)
					defer v0[i].Cancel()
				}
			}
			if got := h.DropPrincipalAll(tt.drop); got != tt.wantCount {
				t.Errorf("DropPrincipalAll = %d, want %d", got, tt.wantCount)
			}
			for i, s := range tt.subs {
				closed := false
				if s.v1 {
					closed = isClosedV1(v1[i])
				} else {
					closed = isClosedV0(v0[i])
				}
				if closed != tt.wantDropped[i] {
					t.Errorf("sub %d closed = %v, want %v", i, closed, tt.wantDropped[i])
				}
			}
			if got := h.DropPrincipalAll(tt.drop); got != 0 {
				t.Errorf("second DropPrincipalAll = %d, want 0", got)
			}
		})
	}
}

// TestDropPrincipalAllNothingDeliveredAfter: a broadcast after the drop never
// reaches the dropped principal but still reaches others.
func TestDropPrincipalAllNothingDeliveredAfter(t *testing.T) {
	h := New()
	gone := h.Subscribe(1, 7, 4)
	goneV1 := h.SubscribeV1(2, 7, 4)
	kept := h.Subscribe(1, 8, 4)
	h.DropPrincipalAll(7)
	h.BroadcastMessage(context.Background(), schema.MessageV0{ID: 1, ChannelID: 1})
	h.BroadcastMessageV1(context.Background(), schema.MessageV1{ID: 1, ChannelID: 2})
	for _, ch := range []<-chan schema.MessageV0{gone.Messages()} {
		if m, ok := <-ch; ok {
			t.Errorf("dropped v0 sub received %+v", m)
		}
	}
	if m, ok := <-goneV1.Messages(); ok {
		t.Errorf("dropped v1 sub received %+v", m)
	}
	if m, ok := <-kept.Messages(); !ok || m.ID != 1 {
		t.Errorf("other principal's sub = %+v, %v; want message 1", m, ok)
	}
}
