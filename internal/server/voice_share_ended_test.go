package server

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of issue #487: the sharer, and only the sharer, is told in their own
// presence why the server ended their share; and a release that names its
// share cannot end anybody else's, or a newer one.

// presenceAs reads GET /v1/channels/ops/voice as who and returns the body and
// the decoded document, requiring 200 and a valid document.
func (f *presenceFixture) presenceAs(t *testing.T, who string) (string, schema.VoicePresenceV1) {
	t.Helper()
	res := f.callREST(t, http.MethodGet, "/v1/channels/ops/voice", f.tokens[who], "")
	if res.status != http.StatusOK {
		t.Fatalf("%s reads presence: %d %s", who, res.status, res.body)
	}
	var doc schema.VoicePresenceV1
	if err := json.Unmarshal([]byte(res.body), &doc); err != nil {
		t.Fatalf("decode %s: %v", res.body, err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("presence %s does not validate: %v", res.body, err)
	}
	return res.body, doc
}

// release sends DELETE .../voice/share as who, naming the share by query
// (already encoded, without the "?"), or none when query is empty.
func (f *presenceFixture) release(t *testing.T, who, channel, query string) wireResult {
	t.Helper()
	path := "/v1/channels/" + channel + "/voice/share"
	if query != "" {
		path += "?" + query
	}
	return f.callREST(t, http.MethodDelete, path, f.tokens[who], "")
}

// claimQuery is the query naming share.
func claimQuery(share schema.VoiceShareV1) string {
	raw, err := json.Marshal(share.StartedAt)
	if err != nil {
		panic(err)
	}
	return schema.VoiceShareClaimParam + "=" + url.QueryEscape(strings.Trim(string(raw), `"`))
}

// TestVoiceShareEndedTellsTheSharer: one case per way the server ends a
// share. Afterwards the sharer's own presence says why, with the share's
// started_at and the moment it ended, and names nobody; everyone else's
// presence, over the same route, carries no reason and is exactly the
// sharer's without it. A person who lost their place in the channel can no
// longer read presence at all, so that case is checked on the snapshot the
// sharer's socket would be sent.
func TestVoiceShareEndedTellsTheSharer(t *testing.T) {
	tests := []struct {
		name string
		// end runs from 1 s, with ann's share claimed at 1 s, and ends it at
		// endAt.
		end      func(t *testing.T, f *presenceFixture, room string)
		endAt    time.Duration
		want     schema.VoiceShareEndReason
		readable bool // ann may still read the channel's presence
	}{
		{
			name:     "the sharer stops it",
			end:      func(t *testing.T, f *presenceFixture, _ string) { f.at(3_000); f.mustStop(t, "ann") },
			endAt:    3 * time.Second,
			want:     schema.VoiceShareEndSelf,
			readable: true,
		},
		{
			name:     "an operator stops it",
			end:      func(t *testing.T, f *presenceFixture, _ string) { f.at(3_000); f.mustStop(t, "root") },
			endAt:    3 * time.Second,
			want:     schema.VoiceShareEndOperator,
			readable: true,
		},
		{
			name: "the sharer leaves the room",
			end: func(t *testing.T, f *presenceFixture, room string) {
				f.lk.drop(room, f.identity("ann"))
				f.at(3_000)
				f.pass(t)
			},
			endAt:    3 * time.Second,
			want:     schema.VoiceShareEndLeft,
			readable: true,
		},
		{
			name: "the sharer loses access and a pass finds it",
			end: func(t *testing.T, f *presenceFixture, _ string) {
				if _, err := f.srv.store.RemoveChannelMember(context.Background(), "system", f.ops.ID, f.ids["ann"]); err != nil {
					t.Fatal(err)
				}
				f.at(3_000)
				f.pass(t)
			},
			endAt: 3 * time.Second,
			want:  schema.VoiceShareEndAccessLost,
		},
		{
			// Someone else who holds a session is removed: the room is
			// rotated, and ann, still a member, loses her share with it.
			name: "the room is rotated",
			end: func(t *testing.T, f *presenceFixture, _ string) {
				f.at(3_000)
				if code := f.doOrdered(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["ann2"]), f.rootTok); code != http.StatusNoContent {
					t.Fatalf("remove ann2: status %d", code)
				}
			},
			endAt:    3 * time.Second,
			want:     schema.VoiceShareEndRotated,
			readable: true,
		},
		{
			name:     "nothing is shared in time",
			end:      func(t *testing.T, f *presenceFixture, _ string) { f.at(16_000); f.pass(t) },
			endAt:    16 * time.Second,
			want:     schema.VoiceShareEndIdle,
			readable: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			f.at(1_000)
			share := f.mustClaim(t, "ann")
			if got := f.snapAs(t, f.ops, "ann").ShareEnded; got != nil {
				t.Fatalf("while sharing ann's presence says the share ended: %+v", got)
			}
			tt.end(t, f, room)

			want := &schema.VoiceShareEnded{Reason: tt.want, StartedAt: share.StartedAt, EndedAt: schema.NewTimestamp(ruleT0.Add(tt.endAt))}
			if got := f.snapAs(t, f.ops, "ann").ShareEnded; !reflect.DeepEqual(got, want) {
				t.Errorf("ann's snapshot says %+v, want %+v", got, want)
			}
			for _, other := range []string{"", "ann2", "root"} {
				if got := f.snapAs(t, f.ops, other).ShareEnded; got != nil {
					t.Errorf("the snapshot as %q says why ann's share ended: %+v", other, got)
				}
			}
			if !tt.readable {
				return
			}
			annBody, annDoc := f.presenceAs(t, "ann")
			if !reflect.DeepEqual(annDoc.ShareEnded, want) {
				t.Errorf("ann reads %s, want share_ended %+v", annBody, want)
			}
			// The reason names nobody: it has the three fields and no more.
			var raw struct {
				ShareEnded map[string]json.RawMessage `json:"share_ended"`
			}
			if err := json.Unmarshal([]byte(annBody), &raw); err != nil {
				t.Fatal(err)
			}
			if got := slices.Sorted(maps.Keys(raw.ShareEnded)); !slices.Equal(got, []string{"ended_at", "reason", "started_at"}) {
				t.Errorf("ann's share_ended carries %q", got)
			}
			// Everyone else reads what they read before #487: the same
			// document without the reason. (The rotation removed ann2.)
			annDoc.ShareEnded = nil
			stripped, err := json.Marshal(annDoc)
			if err != nil {
				t.Fatal(err)
			}
			anyone, err := json.Marshal(f.snap(t, f.ops))
			if err != nil {
				t.Fatal(err)
			}
			if string(anyone) != string(stripped) {
				t.Errorf("the snapshot for anyone else is\n%s\nwant ann's document without share_ended\n%s", anyone, stripped)
			}
			if tt.want != schema.VoiceShareEndRotated {
				if otherBody, _ := f.presenceAs(t, "ann2"); strings.TrimSpace(otherBody) != string(stripped) {
					t.Errorf("ann2 reads\n%s\nwant ann's document without share_ended\n%s", otherBody, stripped)
				}
			}

			// A new claim makes the old end old news.
			if cur := f.room(t, f.ops); !f.lk.in(cur, f.identity("ann")) {
				decodeSession(t, f.session(t, "ann", "ops"))
				f.lk.setRoom(cur, fakeParticipant{identity: f.identity("ann"), joinedMs: 9_000})
				f.at(30_000)
				f.pass(t)
			}
			f.at(40_000)
			f.mustClaim(t, "ann")
			if _, doc := f.presenceAs(t, "ann"); doc.ShareEnded != nil {
				t.Errorf("after a new claim ann's presence still says %+v", doc.ShareEnded)
			}
		})
	}
}

// TestVoiceShareEndedOnTheSocket: the frame the operator's stop sends the
// sharer carries the reason, and the frame the same change sends another
// member is byte for byte the sharer's without it.
func TestVoiceShareEndedOnTheSocket(t *testing.T) {
	f, room := newShareFixture(t)
	f.at(1_000)
	share := f.mustClaim(t, "ann")
	f.publish(room, "ann", true, true)
	f.at(1_500)
	f.pass(t)
	base := wsTestServer(t, f.srv)
	sockets := map[string]*websocket.Conn{}
	for _, who := range []string{"ann", "ann2"} {
		conn, _, err := dialVoice(t, base, "ops", f.tokens[who])
		if err != nil {
			t.Fatalf("%s dials: %v", who, err)
		}
		sockets[who] = conn
		if first := readRawPresence(t, conn); strings.Contains(first, "share_ended") {
			t.Fatalf("%s's first frame says a share ended: %s", who, first)
		}
	}
	f.at(2_000)
	f.mustStop(t, "root")
	annFrame := readRawPresence(t, sockets["ann"])
	otherFrame := readRawPresence(t, sockets["ann2"])
	var doc schema.VoicePresenceV1
	if err := json.Unmarshal([]byte(annFrame), &doc); err != nil {
		t.Fatal(err)
	}
	want := &schema.VoiceShareEnded{Reason: schema.VoiceShareEndOperator, StartedAt: share.StartedAt, EndedAt: schema.NewTimestamp(ruleT0.Add(2 * time.Second))}
	if !reflect.DeepEqual(doc.ShareEnded, want) {
		t.Errorf("ann's frame %s, want share_ended %+v", annFrame, want)
	}
	if slices.ContainsFunc(doc.Rooms[0].Participants, func(p schema.VoiceParticipant) bool { return p.Sharing }) {
		t.Errorf("ann's frame still shows a share: %s", annFrame)
	}
	doc.ShareEnded = nil
	stripped, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if otherFrame != string(stripped) {
		t.Errorf("ann2's frame\n%s\nwant ann's without share_ended\n%s", otherFrame, stripped)
	}
}

// readRawPresence reads one presence frame as it was sent.
func readRawPresence(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(raw)
}

// TestVoiceShareRelease: a release that names its share (started_at) ends it
// only if it is still the room's share and the caller's; otherwise 409
// voice_share_stale, nothing written, nothing withdrawn, no denial recorded,
// whoever asks. Naming nothing is the stop it always was (TestVoiceShareStop).
func TestVoiceShareRelease(t *testing.T) {
	type setup struct {
		f     *presenceFixture
		room  string
		ann   schema.VoiceShareV1 // ann's current share
		older schema.VoiceShareV1 // a share of ann's that ended before it
	}
	tests := []struct {
		name       string
		who        string
		channel    string
		query      func(s setup) string
		wantStatus int
		wantCode   string
		stopped    bool
	}{
		{"the sharer names the share she holds", "ann", "ops", func(s setup) string { return claimQuery(s.ann) }, 204, "", true},
		{"the sharer names it in another offset", "ann", "ops", func(s setup) string {
			return schema.VoiceShareClaimParam + "=" + url.QueryEscape(s.ann.StartedAt.Time().In(time.FixedZone("", 2*3600)).Format(time.RFC3339Nano))
		}, 204, "", true},
		{"the sharer names a share of hers that ended", "ann", "ops", func(s setup) string { return claimQuery(s.older) }, 409, schema.ErrorCodeVoiceShareStale, false},
		{"an operator names ann's share", "root", "ops", func(s setup) string { return claimQuery(s.ann) }, 409, schema.ErrorCodeVoiceShareStale, false},
		{"another member names ann's share", "ann2", "ops", func(s setup) string { return claimQuery(s.ann) }, 409, schema.ErrorCodeVoiceShareStale, false},
		{"a share in a channel with no room", "ann", "ops2", func(s setup) string { return claimQuery(s.ann) }, 409, schema.ErrorCodeVoiceShareStale, false},
		{"not a time", "ann", "ops", func(setup) string { return schema.VoiceShareClaimParam + "=soon" }, 400, "invalid_request", false},
		{"empty", "ann", "ops", func(setup) string { return schema.VoiceShareClaimParam + "=" }, 400, "invalid_request", false},
		{"given twice", "ann", "ops", func(s setup) string { return claimQuery(s.ann) + "&" + claimQuery(s.ann) }, 400, "invalid_request", false},
		{"a non-member", "bob", "ops", func(s setup) string { return claimQuery(s.ann) }, 404, "channel_not_found", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			f.at(1_000)
			older := f.mustClaim(t, "ann")
			f.at(1_500)
			f.mustStop(t, "ann")
			f.at(2_000)
			ann := f.mustClaim(t, "ann")
			f.publish(room, "ann", true, false)
			s := setup{f: f, room: room, ann: ann, older: older}
			rows := len(f.shareRows(t))
			denied := len(f.audits(t, "access_denied"))
			res := f.release(t, tt.who, tt.channel, tt.query(s))
			if res.status != tt.wantStatus || errCodeIfAny(t, res.body) != tt.wantCode {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			wantRows := rows
			if tt.stopped {
				wantRows++
			}
			if got := f.shareRows(t); len(got) != wantRows {
				t.Errorf("share rows %q, want %d", got, wantRows)
			} else if tt.stopped && got[len(got)-1] != "ann stopped principal=ann reason=self" {
				t.Errorf("the release is recorded as %q", got[len(got)-1])
			}
			if got := len(f.audits(t, "access_denied")) - denied; got != 0 {
				t.Errorf("%d access_denied rows, want none", got)
			}
			if got, want := f.sharing(t, f.ops) == "-", tt.stopped; got != want {
				t.Errorf("nobody sharing = %v, want %v", got, want)
			}
			if got := f.granted(t, room, "ann"); got == tt.stopped {
				t.Errorf("ann's sources granted = %v, want %v", got, !tt.stopped)
			}
			if strings.Contains(res.body, f.identity("ann")) || strings.Contains(res.body, "principal") {
				t.Errorf("the answer names the sharer: %s", res.body)
			}
		})
	}

	// A malformed parameter is refused before the channel is looked at, so
	// a non-member learns nothing from it.
	t.Run("a malformed parameter says nothing about the channel", func(t *testing.T) {
		f, _ := newShareFixture(t)
		a := f.release(t, "bob", "ops", schema.VoiceShareClaimParam+"=soon")
		b := f.release(t, "bob", "nosuch", schema.VoiceShareClaimParam+"=soon")
		if a != b || a.status != http.StatusBadRequest {
			t.Errorf("answers differ or are not 400:\n%+v\n%+v", a, b)
		}
	})
}

// errCodeIfAny is the error code of a refusal, or "" for an empty body.
func errCodeIfAny(t *testing.T, body string) string {
	t.Helper()
	if body == "" {
		return ""
	}
	return errCode(t, body)
}

// TestVoiceShareLateReleaseKeepsANewerShare is the case the conditional
// release exists for (#444's security review): an operator's page claims, its
// claim lapses while the picker is open, someone else shares, and the page's
// release arrives late. Naming its own claim, the release changes nothing and
// is not audited as a moderation stop. The same request naming nothing is
// still the operator's stop.
func TestVoiceShareLateReleaseKeepsANewerShare(t *testing.T) {
	f, room := newShareFixture(t)
	if _, err := f.srv.store.AddChannelMember(context.Background(), "system", f.ops.ID, f.ids["root"], 0); err != nil {
		t.Fatal(err)
	}
	decodeSession(t, f.session(t, "root", "ops"))
	f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
		fakeParticipant{identity: f.identity("root"), joinedMs: 3_000, published: true, muted: true})
	f.at(500)
	f.pass(t)

	f.at(1_000)
	res := f.claimShare(t, "root", "ops")
	if res.status != http.StatusOK {
		t.Fatalf("root claims: %d %s", res.status, res.body)
	}
	var lapsed schema.VoiceShareV1
	if err := json.Unmarshal([]byte(res.body), &lapsed); err != nil {
		t.Fatal(err)
	}
	// The picker stays open: nothing is published, and the claim lapses.
	f.at(16_000)
	f.pass(t)
	f.at(17_000)
	f.mustClaim(t, "ann")
	f.publish(room, "ann", true, false)
	f.at(17_500)
	f.pass(t)

	f.at(18_000)
	if res := f.release(t, "root", "ops", claimQuery(lapsed)); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceShareStale {
		t.Fatalf("root's late release: %d %s, want 409 %s", res.status, res.body, schema.ErrorCodeVoiceShareStale)
	}
	if got := f.sharing(t, f.ops); got != "ann" {
		t.Errorf("after root's late release presence shows %q sharing, want ann", got)
	}
	if !f.granted(t, room, "ann") {
		t.Error("root's late release took ann's sources")
	}
	want := []string{"root started audio=true", "root stopped principal=root reason=idle", "ann started audio=true"}
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}

	// The deliberate stop, naming nothing, is unchanged.
	f.mustStop(t, "root")
	want = append(want, "root stopped principal=ann reason=operator")
	if got := f.shareRows(t); !slices.Equal(got, want) {
		t.Errorf("share rows %q, want %q", got, want)
	}
	if got := f.snapAs(t, f.ops, "ann").ShareEnded; got == nil || got.Reason != schema.VoiceShareEndOperator {
		t.Errorf("ann's presence says %+v, want operator", got)
	}
	if got := f.snapAs(t, f.ops, "root").ShareEnded; got == nil || got.Reason != schema.VoiceShareEndIdle {
		t.Errorf("root's presence says %+v, want the end of root's own share, idle", got)
	}
	if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 0 {
		t.Errorf("%d unclaimed rows", n)
	}
}
