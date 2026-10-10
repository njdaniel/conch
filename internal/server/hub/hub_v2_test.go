package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/njdaniel/conch/pkg/schema"
)

func scopedMsg(id int64, a *schema.Audience) schema.MessageV2 {
	return schema.MessageV2{Schema: schema.MessageSchemaV2, ID: id, ChannelID: 1, AuthorID: 1, Body: "b", Audience: a}
}

func drainV2(sub *SubscriptionV2) (ids []int64, closed bool) {
	for {
		select {
		case m, ok := <-sub.Messages():
			if !ok {
				return ids, true
			}
			ids = append(ids, m.ID)
		default:
			return ids, false
		}
	}
}

func drainV1(sub *SubscriptionV1) (ids []int64) {
	for {
		select {
		case m, ok := <-sub.Messages():
			if !ok {
				return ids
			}
			ids = append(ids, m.ID)
		default:
			return ids
		}
	}
}

func ids(v ...int64) []int64 { return append([]int64{}, v...) }

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// A scoped message goes to the v2 subscriptions of its recipients and to no
// one else: not a v2 subscription of another principal, not an unidentified
// one, and never a v0 or v1 subscription.
func TestBroadcastMessageV2AudienceFiltering(t *testing.T) {
	net := &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3}
	tests := []struct {
		name       string
		msg        schema.MessageV2
		recipients []int64
		// delivered per subscriber principal id for the v2 subscribers below.
		want map[int64]bool
	}{
		{"net message", scopedMsg(10, net), []int64{5, 6}, map[int64]bool{5: true, 6: true, 7: false, 0: false}},
		{"whisper", scopedMsg(10, &schema.Audience{Kind: schema.AudienceKindPrincipals, PrincipalIDs: []int64{5, 7}}), []int64{5, 7},
			map[int64]bool{5: true, 6: false, 7: true, 0: false}},
		{"scoped with no recipients fails closed", scopedMsg(10, net), nil, map[int64]bool{5: false, 6: false, 7: false, 0: false}},
		{"recipients without an audience are still scoped", scopedMsg(10, nil), []int64{6}, map[int64]bool{5: false, 6: true, 7: false, 0: false}},
		{"a zero recipient id never matches the unidentified", scopedMsg(10, net), []int64{0, 5}, map[int64]bool{5: true, 6: false, 7: false, 0: false}},
		{"channel-wide reaches everyone", scopedMsg(10, nil), nil, map[int64]bool{5: true, 6: true, 7: true, 0: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New()
			v2 := map[int64]*SubscriptionV2{}
			for p := range tt.want {
				v2[p] = h.SubscribeV2(1, p, 4)
			}
			otherChannel := h.SubscribeV2(2, 5, 4)
			legacy0 := h.Subscribe(1, 5, 4)
			legacy1 := h.SubscribeV1(1, 5, 4)

			h.BroadcastMessageV2(context.Background(), tt.msg, tt.recipients)

			for p, want := range tt.want {
				got, closed := drainV2(v2[p])
				if closed {
					t.Errorf("principal %d subscription closed", p)
				}
				if (len(got) == 1) != want || len(got) > 1 {
					t.Errorf("principal %d received %v, want delivered=%v", p, got, want)
				}
			}
			if got, _ := drainV2(otherChannel); len(got) != 0 {
				t.Errorf("a subscriber of another channel received %v", got)
			}
			// BroadcastMessageV2 never touches v0 or v1 subscriptions.
			if len(legacy0.Messages()) != 0 || len(legacy1.Messages()) != 0 {
				t.Error("a v0 or v1 subscription received a v2 broadcast")
			}
		})
	}
}

// The recipient list steers delivery and is never part of what is sent.
func TestBroadcastMessageV2RecipientsNotOnTheWire(t *testing.T) {
	h := New()
	sub := h.SubscribeV2(1, 5, 4)
	msg := scopedMsg(10, &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3})
	h.BroadcastMessageV2(context.Background(), msg, []int64{5, 4242, 4343})
	got := <-sub.Messages()
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"4242", "4343", "recipient"} {
		if strings.Contains(string(raw), leaked) {
			t.Errorf("frame %s mentions %q", raw, leaked)
		}
	}
}

// A channel-wide v1 broadcast (what every v0, v1, MCP and webhook post makes)
// reaches v2 subscriptions as the v2 envelope, so they hear all open traffic.
func TestBroadcastMessageV1ReachesV2Subscribers(t *testing.T) {
	h := New()
	v2 := h.SubscribeV2(1, 5, 4)
	anon := h.SubscribeV2(1, 0, 4)
	v1 := h.SubscribeV1(1, 5, 4)
	h.BroadcastMessageV1(context.Background(), schema.MessageV1{Schema: schema.MessageSchemaV1, ID: 9, ChannelID: 1, AuthorID: 1, Body: "open"})
	for name, sub := range map[string]*SubscriptionV2{"identified": v2, "anonymous": anon} {
		got, _ := drainV2(sub)
		if !equalIDs(got, ids(9)) {
			t.Errorf("%s v2 subscriber received %v, want [9]", name, got)
		}
	}
	if got := drainV1(v1); !equalIDs(got, ids(9)) {
		t.Errorf("v1 subscriber received %v", got)
	}
}

// A v2 subscriber that stops reading is dropped, as for v0 and v1, without
// blocking the broadcast or its neighbours; a drop of one recipient does not
// stop delivery to the others.
func TestBroadcastMessageV2SlowConsumer(t *testing.T) {
	h := New()
	slow := h.SubscribeV2(1, 5, 1)
	fast := h.SubscribeV2(1, 6, 8)
	outsider := h.SubscribeV2(1, 7, 1)
	net := &schema.Audience{Kind: schema.AudienceKindNet, NetID: 3}

	for id := int64(1); id <= 3; id++ {
		h.BroadcastMessageV2(context.Background(), scopedMsg(id, net), []int64{5, 6})
	}
	if got, closed := drainV2(fast); closed || !equalIDs(got, ids(1, 2, 3)) {
		t.Errorf("fast recipient got %v closed=%v, want [1 2 3]", got, closed)
	}
	got, closed := drainV2(slow)
	if !closed || !equalIDs(got, ids(1)) {
		t.Errorf("slow recipient got %v closed=%v, want [1] then closed", got, closed)
	}
	// The outsider was never sent anything, so it never filled up.
	if got, closed := drainV2(outsider); closed || len(got) != 0 {
		t.Errorf("outsider got %v closed=%v, want nothing and still open", got, closed)
	}
}

// Removing a principal from a channel and disabling it close its v2 sockets
// like v0 and v1; closing the hub closes them all.
func TestV2SubscriptionsAreDropped(t *testing.T) {
	h := New()
	a := h.SubscribeV2(1, 5, 4)
	b := h.SubscribeV2(1, 6, 4)
	c := h.SubscribeV2(2, 5, 4)
	anon := h.SubscribeV2(1, 0, 4)
	if n := h.DropPrincipal(1, 5); n != 1 {
		t.Errorf("DropPrincipal = %d, want 1", n)
	}
	if h.DropPrincipal(1, 0) != 0 {
		t.Error("a zero principal must match nothing")
	}
	if _, closed := drainV2(a); !closed {
		t.Error("dropped v2 subscription still open")
	}
	for name, sub := range map[string]*SubscriptionV2{"other principal": b, "other channel": c, "anonymous": anon} {
		if _, closed := drainV2(sub); closed {
			t.Errorf("%s subscription was dropped", name)
		}
	}
	if n := h.DropPrincipalAll(5); n != 1 {
		t.Errorf("DropPrincipalAll = %d, want 1 (the one on channel 2)", n)
	}
	if _, closed := drainV2(c); !closed {
		t.Error("DropPrincipalAll left a v2 subscription open")
	}
	h.Close()
	if _, closed := drainV2(b); !closed {
		t.Error("Close left a v2 subscription open")
	}
	late := h.SubscribeV2(1, 5, 4)
	if _, closed := drainV2(late); !closed {
		t.Error("a v2 subscription taken from a closed hub is open")
	}
	b.Cancel() // idempotent after a drop
}
