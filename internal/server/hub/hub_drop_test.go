package hub

import (
	"context"
	"sync"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func isClosedV0(sub *Subscription) bool {
	select {
	case _, ok := <-sub.Messages():
		return !ok
	default:
		return false
	}
}

func isClosedV1(sub *SubscriptionV1) bool {
	select {
	case _, ok := <-sub.Messages():
		return !ok
	default:
		return false
	}
}

func TestDropPrincipal(t *testing.T) {
	type subSpec struct {
		channel, principal int64
		v1                 bool
	}
	tests := []struct {
		name          string
		subs          []subSpec
		dropChannel   int64
		dropPrincipal int64
		wantDropped   []bool // parallel to subs
		wantCount     int
	}{
		{"v0 and v1 and several sockets on the channel",
			[]subSpec{{1, 7, false}, {1, 7, true}, {1, 7, false}, {1, 8, false}, {1, 8, true}},
			1, 7, []bool{true, true, true, false, false}, 3},
		{"other channel of the same principal survives",
			[]subSpec{{1, 7, false}, {2, 7, false}, {2, 7, true}},
			1, 7, []bool{true, false, false}, 1},
		{"no caller subscriptions are never targeted",
			[]subSpec{{1, 0, false}, {1, 0, true}},
			1, 0, []bool{false, false}, 0},
		{"principal without subscriptions",
			[]subSpec{{1, 8, false}},
			1, 7, []bool{false}, 0},
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
			if got := h.DropPrincipal(tt.dropChannel, tt.dropPrincipal); got != tt.wantCount {
				t.Errorf("DropPrincipal = %d, want %d", got, tt.wantCount)
			}
			for i, s := range tt.subs {
				var closed bool
				if s.v1 {
					closed = isClosedV1(v1[i])
				} else {
					closed = isClosedV0(v0[i])
				}
				if closed != tt.wantDropped[i] {
					t.Errorf("sub %d closed = %v, want %v", i, closed, tt.wantDropped[i])
				}
			}
			// A second drop is a no-op, and Cancel after a drop is safe.
			if got := h.DropPrincipal(tt.dropChannel, tt.dropPrincipal); got != 0 {
				t.Errorf("second DropPrincipal = %d, want 0", got)
			}
		})
	}
}

// TestDropPrincipalRacesBroadcast: once DropPrincipal has returned, no
// broadcast reaches the dropped principal, however many run concurrently.
// Run with -race.
func TestDropPrincipalRacesBroadcast(t *testing.T) {
	for range 50 {
		h := New()
		sub := h.Subscribe(1, 7, 1000)
		subV1 := h.SubscribeV1(1, 7, 1000)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := int64(1); ; i++ {
				select {
				case <-stop:
					return
				default:
					h.BroadcastMessage(context.Background(), schema.MessageV0{ID: i, ChannelID: 1})
					h.BroadcastMessageV1(context.Background(), schema.MessageV1{ID: i, ChannelID: 1})
				}
			}
		}()
		h.DropPrincipal(1, 7)
		// Draining to close proves the channel is closed; anything sent after
		// the drop would panic (send on closed channel) under -race.
		for range sub.Messages() {
		}
		for range subV1.Messages() {
		}
		close(stop)
		wg.Wait()
	}
}
