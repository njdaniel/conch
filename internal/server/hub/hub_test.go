package hub

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func msg(channelID, id int64) schema.MessageV0 {
	return schema.MessageV0{ID: id, ChannelID: channelID, AuthorID: 1, Body: fmt.Sprintf("m%d", id)}
}

// receiveOne asserts exactly one message is immediately available (broadcasts
// are synchronous, so delivered messages are already buffered).
func receiveOne(t *testing.T, sub *Subscription) schema.MessageV0 {
	t.Helper()
	select {
	case m, ok := <-sub.Messages():
		if !ok {
			t.Fatal("subscription closed, want a message")
		}
		return m
	default:
		t.Fatal("no message buffered, want one")
	}
	panic("unreachable")
}

func assertEmpty(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case m, ok := <-sub.Messages():
		if ok {
			t.Fatalf("unexpected message %+v", m)
		}
		t.Fatal("subscription unexpectedly closed")
	default:
	}
}

func TestBroadcastFanout(t *testing.T) {
	h := New()
	sub1 := h.Subscribe(1, 4)
	defer sub1.Cancel()
	sub2 := h.Subscribe(1, 4)
	defer sub2.Cancel()
	other := h.Subscribe(2, 4)
	defer other.Cancel()

	h.BroadcastMessage(context.Background(), msg(1, 10))

	for i, sub := range []*Subscription{sub1, sub2} {
		if got := receiveOne(t, sub); got.ID != 10 {
			t.Errorf("subscriber %d got message %d, want 10", i+1, got.ID)
		}
	}
	assertEmpty(t, other)

	h.BroadcastMessage(context.Background(), msg(2, 20))
	if got := receiveOne(t, other); got.ID != 20 {
		t.Errorf("channel-2 subscriber got message %d, want 20", got.ID)
	}
	assertEmpty(t, sub1)
}

func TestSlowConsumerIsDroppedWithoutStallingOthers(t *testing.T) {
	h := New()
	slow := h.Subscribe(1, 1)
	defer slow.Cancel()
	fast := h.Subscribe(1, 4)
	defer fast.Cancel()

	// First fills slow's buffer; second overflows it and must drop slow while
	// still delivering to fast.
	h.BroadcastMessage(context.Background(), msg(1, 1))
	h.BroadcastMessage(context.Background(), msg(1, 2))

	if got := receiveOne(t, slow); got.ID != 1 {
		t.Errorf("slow subscriber buffered message %d, want 1", got.ID)
	}
	if _, ok := <-slow.Messages(); ok {
		t.Error("slow subscriber channel still open, want closed after overflow")
	}

	for want := int64(1); want <= 2; want++ {
		if got := receiveOne(t, fast); got.ID != want {
			t.Errorf("fast subscriber got message %d, want %d", got.ID, want)
		}
	}
}

func TestCancelStopsDeliveryAndIsIdempotent(t *testing.T) {
	h := New()
	sub := h.Subscribe(1, 4)
	sub.Cancel()
	sub.Cancel() // must not panic (double close)

	h.BroadcastMessage(context.Background(), msg(1, 1))
	if _, ok := <-sub.Messages(); ok {
		t.Error("cancelled subscription received a message")
	}
}

func TestCloseDropsAllAndClosesFutureSubscriptions(t *testing.T) {
	h := New()
	sub := h.Subscribe(1, 4)
	h.Close()

	if _, ok := <-sub.Messages(); ok {
		t.Error("subscription still open after hub Close")
	}
	sub.Cancel() // must not panic after Close already dropped it

	late := h.Subscribe(1, 4)
	if _, ok := <-late.Messages(); ok {
		t.Error("subscription on a closed hub is open, want closed")
	}
}

// TestConcurrentBroadcastSubscribe exercises the hub under the race detector:
// subscribers churn while broadcasts run.
func TestConcurrentBroadcastSubscribe(t *testing.T) {
	h := New()
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 100 {
				h.BroadcastMessage(context.Background(), msg(1, int64(i)))
			}
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				sub := h.Subscribe(1, 1)
				sub.Cancel()
			}
		}()
	}
	wg.Wait()
	h.Close()
}
