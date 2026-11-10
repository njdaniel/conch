package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The bound on a channel's name and topic changes (issue #1667): one bucket
// per (principal, channel), shared by rename and topic, 429 rate_limited with
// Retry-After past it, and a refusal writes no audit row.
func TestChannelWriteLimit(t *testing.T) {
	const (
		topic  = "topic"
		rename = "rename"
	)
	type op struct {
		kind    string
		advance time.Duration // the clock moves this far before the request
		want    int
		retry   string // the Retry-After expected on a 429
	}
	rep := func(kind string, n int, want int) []op {
		ops := make([]op, n)
		for i := range ops {
			ops[i] = op{kind: kind, want: want}
		}
		return ops
	}
	join := func(parts ...[]op) []op {
		var all []op
		for _, p := range parts {
			all = append(all, p...)
		}
		return all
	}
	tests := []struct {
		name string
		ops  []op
	}{
		{"under the bound", rep(topic, channelWriteBurst-1, 200)},
		{"at the bound", rep(topic, channelWriteBurst, 200)},
		{"over the bound", join(rep(topic, channelWriteBurst, 200), []op{{kind: topic, want: 429, retry: "10"}})},
		{"rename and topic share the bound", join(
			rep(topic, channelWriteBurst-2, 200),
			rep(rename, 2, 200),
			[]op{{kind: topic, want: 429, retry: "10"}, {kind: rename, want: 429, retry: "10"}},
		)},
		{"one token back after a refill", join(
			rep(topic, channelWriteBurst, 200),
			[]op{
				{kind: topic, want: 429, retry: "10"},
				{kind: rename, advance: channelWriteRefill, want: 200},
				{kind: topic, want: 429, retry: "10"},
			},
		)},
		{"the retry shortens as time passes", join(
			rep(topic, channelWriteBurst, 200),
			[]op{{kind: topic, advance: 4 * time.Second, want: 429, retry: "6"}},
		)},
		{"after the whole window", join(
			rep(topic, channelWriteBurst, 200),
			[]op{{kind: topic, want: 429, retry: "10"}},
			[]op{{kind: topic, advance: channelWriteBurst * channelWriteRefill, want: 200}},
			rep(topic, channelWriteBurst-1, 200),
		)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newChannelV1Fixture(t)
			clock := newManualClock()
			f.srv.now = clock.Now
			ch := f.pubJoined
			name := ch.Name
			admitted := map[string]int{}
			seq := 0
			for i, o := range tt.ops {
				clock.Advance(o.advance)
				seq++
				var code int
				var retry string
				var body string
				switch o.kind {
				case topic:
					r := f.do(t, http.MethodPut, channelTopicPath(ch.ID), f.member2Tok, channelTopicBody(t, "t"+strconv.Itoa(seq)))
					code, retry, body = r.Code, r.Header().Get("Retry-After"), r.Body.String()
				case rename:
					next := fmt.Sprintf("room-%d", seq)
					r := f.do(t, http.MethodPut, channelNamePath(ch.ID), f.member2Tok, fmt.Sprintf(`{"from":%q,"name":%q}`, name, next))
					code, retry, body = r.Code, r.Header().Get("Retry-After"), r.Body.String()
					if code == http.StatusOK {
						name = next
					}
				}
				if code != o.want {
					t.Fatalf("op %d (%s) = %d %s, want %d", i, o.kind, code, body, o.want)
				}
				if code == http.StatusOK {
					admitted[o.kind]++
					continue
				}
				if retry != o.retry {
					t.Errorf("op %d: Retry-After = %q, want %q", i, retry, o.retry)
				}
				var e schema.Error
				if err := json.Unmarshal([]byte(body), &e); err != nil || e.Code != schema.ErrorCodeRateLimited || e.RetryAfterSeconds <= 0 {
					t.Errorf("op %d: body = %s, want rate_limited with retry_after_seconds", i, body)
				}
			}
			// A refusal wrote nothing: one audit row per admitted change.
			if n := len(f.auditOf(t, store.AuditChannelTopicChanged)); n != admitted[topic] {
				t.Errorf("%d channel_topic_changed rows, want %d", n, admitted[topic])
			}
			if n := len(f.auditOf(t, store.AuditChannelRenamed)); n != admitted[rename] {
				t.Errorf("%d channel_renamed rows, want %d", n, admitted[rename])
			}
		})
	}
}

// The bound is per principal and per channel: another manager, and another
// channel, have their own.
func TestChannelWriteLimitKeys(t *testing.T) {
	f := newChannelV1Fixture(t)
	clock := newManualClock()
	f.srv.now = clock.Now
	set := func(tok string, id int64) int {
		return f.do(t, http.MethodPut, channelTopicPath(id), tok, channelTopicBody(t, "x"+clock.Now().String())).Code
	}
	for range channelWriteBurst {
		if got := set(f.rootTok, f.pubJoined.ID); got != http.StatusOK {
			t.Fatalf("topic = %d", got)
		}
		clock.Advance(time.Millisecond)
	}
	if got := set(f.rootTok, f.pubJoined.ID); got != http.StatusTooManyRequests {
		t.Fatalf("over the bound = %d, want 429", got)
	}
	if got := set(f.rootTok, f.pubOther.ID); got != http.StatusOK {
		t.Errorf("another channel = %d, want 200", got)
	}
	if got := set(f.member2Tok, f.pubJoined.ID); got != http.StatusOK {
		t.Errorf("another principal = %d, want 200", got)
	}
}
