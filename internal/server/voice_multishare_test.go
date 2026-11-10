package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Tests of several screen shares at once in one room (issue #1508,
// docs/design/voice-multi-share.md §5, §9.2): the claim and its quality body,
// the cap, the release by ?principal=, ?schema= on the two presence routes,
// companion audio per sharer and the passes, over the scripted LiveKit with
// the fixture's manual clock. Everything here goes through the public routes
// or the snapshot, so it holds whatever the room's state looks like inside.

// ---------------------------------------------------------------------------
// Fixture additions

// stopShareOf is a moderator's stop: who names target's stream with
// ?principal= (design §5.2).
func (f *presenceFixture) stopShareOf(t *testing.T, who, target string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodDelete, "/v1/channels/ops/voice/share?principal="+strconv.FormatInt(f.ids[target], 10), f.tokens[who], "")
}

// mustStopOf is stopShareOf in ops, requiring 204 and no body.
func (f *presenceFixture) mustStopOf(t *testing.T, target string) {
	t.Helper()
	const who = "root"
	if res := f.stopShareOf(t, who, target); res.status != http.StatusNoContent || res.body != "" {
		t.Fatalf("%s stops %s's share: %d %q, want 204 and no body", who, target, res.status, res.body)
	}
}

// claimShareBody is claimShare with a request body.
func (f *presenceFixture) claimShareBody(t *testing.T, who, body string) wireResult {
	t.Helper()
	return f.callREST(t, http.MethodPost, "/v1/channels/ops/voice/share", f.tokens[who], body)
}

// strict decodes body into v refusing unknown fields, as the browser's
// closed validators do.
func strict(t *testing.T, body string, v any) {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("decode %s into %T: %v", body, v, err)
	}
}

// presenceV2 reads ops' presence as who in v2, strictly, and validates it.
func (f *presenceFixture) presenceV2(t *testing.T, who string) schema.VoicePresenceV2 {
	t.Helper()
	res := f.callREST(t, http.MethodGet, "/v1/channels/ops/voice?schema="+schema.VoicePresenceSchemaV2, f.tokens[who], "")
	if res.status != http.StatusOK {
		t.Fatalf("%s reads v2 presence: %d %s", who, res.status, res.body)
	}
	var doc schema.VoicePresenceV2
	strict(t, res.body, &doc)
	if err := doc.Validate(); err != nil {
		t.Fatalf("v2 presence %s does not validate: %v", res.body, err)
	}
	return doc
}

// sharers lists the sharing participants of doc, by name, in the order
// presence gives them (principal id), as "name" or "name@quality".
func (f *presenceFixture) sharers(doc schema.VoicePresenceV2) []string {
	byID := map[int64]string{}
	for name, id := range f.ids {
		byID[id] = name
	}
	var out []string
	for _, room := range doc.Rooms {
		for _, p := range room.Participants {
			if !p.Sharing {
				continue
			}
			name := byID[p.PrincipalID]
			if p.ShareQuality != nil {
				name += fmt.Sprintf("@%s/%d", p.ShareQuality.Resolution, p.ShareQuality.FrameRate)
			}
			out = append(out, name)
		}
	}
	return out
}

// v1Sharers lists who v1 presence shows sharing to who.
func (f *presenceFixture) v1Sharers(t *testing.T, who string) []string {
	t.Helper()
	byID := map[int64]string{}
	for name, id := range f.ids {
		byID[id] = name
	}
	var out []string
	for _, room := range f.snapAs(t, f.ops, who).Rooms {
		for _, p := range room.Participants {
			if p.Sharing {
				out = append(out, byID[p.PrincipalID])
			}
		}
	}
	return out
}

// addSharer adds a member name to ops, with a session, and connects them to
// room, muted. The fixture's other participants stay.
func (f *presenceFixture) addSharer(t *testing.T, room, name string, joinedMs int64) {
	t.Helper()
	ctx := context.Background()
	p, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, name)
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := f.srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", f.ops.ID, p.ID, 0); err != nil {
		t.Fatal(err)
	}
	f.tokens[name], f.ids[name] = tok, p.ID
	decodeSession(t, f.session(t, name, "ops"))
	f.lk.mu.Lock()
	f.lk.rooms[room] = append(f.lk.rooms[room], fakeParticipant{identity: f.identity(name), joinedMs: joinedMs, published: true, muted: true})
	f.lk.mu.Unlock()
}

// makeManager makes who a manager of ops.
func (f *presenceFixture) makeManager(t *testing.T, who string) {
	t.Helper()
	no, yes := false, true
	if _, err := f.srv.store.PutChannelMember(context.Background(), "system", f.ops.ID, f.ids[who], 0, &no, &yes); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// The claim

// TestVoiceMultiShareClaims: two members both claim and both share, each with
// their own start time; a claim that declares a quality is answered
// VoiceShareV2 and says so in its started row; one that does not is answered
// VoiceShareV1, as before; a holder asking again with a body changes the
// quality (a presence frame, no row), and without one keeps it.
func TestVoiceMultiShareClaims(t *testing.T) {
	f, room := newShareFixture(t)
	sub := f.srv.voice.subscribe(f.ops.ID, f.ids["ann2"], 0)
	defer sub.cancel()
	drain := func() bool {
		select {
		case <-sub.wake:
			return true
		default:
			return false
		}
	}

	f.at(1_000)
	res := f.claimShareBody(t, "ann", `{"quality":{"resolution":"1080p","frame_rate":60}}`)
	if res.status != http.StatusOK {
		t.Fatalf("ann claims with a quality: %d %s", res.status, res.body)
	}
	var annShare schema.VoiceShareV2
	strict(t, res.body, &annShare)
	if err := annShare.Validate(); err != nil || annShare.Quality != (schema.VoiceShareQuality{Resolution: schema.VoiceShareResolution1080p, FrameRate: 60}) {
		t.Fatalf("ann's answer %s: quality %+v, %v", res.body, annShare.Quality, err)
	}

	f.at(2_000)
	ann2Share := f.mustClaim(t, "ann2") // no body: VoiceShareV1, strictly
	if !ann2Share.StartedAt.Time().After(annShare.StartedAt.Time()) {
		t.Fatalf("started_at %v and %v, want distinct, ann2's later", annShare.StartedAt.Time(), ann2Share.StartedAt.Time())
	}
	if !f.granted(t, room, "ann") || !f.granted(t, room, "ann2") {
		t.Fatal("both claims were not granted the sources")
	}
	doc := f.presenceV2(t, "ann2")
	if got, want := f.sharers(doc), []string{"ann@1080p/60", "ann2"}; !slices.Equal(got, want) {
		t.Fatalf("v2 sharers = %q, want %q", got, want)
	}
	for _, p := range doc.Rooms[0].Participants {
		switch p.PrincipalID {
		case f.ids["ann"]:
			if !p.ShareStartedAt.Time().Equal(annShare.StartedAt.Time()) {
				t.Errorf("ann's share_started_at = %v, want %v", p.ShareStartedAt.Time(), annShare.StartedAt.Time())
			}
		case f.ids["ann2"]:
			if !p.ShareStartedAt.Time().Equal(ann2Share.StartedAt.Time()) {
				t.Errorf("ann2's share_started_at = %v, want %v", p.ShareStartedAt.Time(), ann2Share.StartedAt.Time())
			}
		}
	}
	wantRows := []string{"ann started audio=true quality=1080p/60", "ann2 started audio=true"}
	if got := f.shareRows(t); !slices.Equal(got, wantRows) {
		t.Fatalf("share rows %q, want %q", got, wantRows)
	}

	// A holder's repeat with a body changes the quality: a frame, no row, the
	// same share, answered VoiceShareV2.
	drain()
	f.at(3_000)
	res = f.claimShareBody(t, "ann", `{"quality":{"resolution":"720p","frame_rate":30}}`)
	var again schema.VoiceShareV2
	strict(t, res.body, &again)
	if res.status != http.StatusOK || again.Quality.Resolution != schema.VoiceShareResolution720p || !again.StartedAt.Time().Equal(annShare.StartedAt.Time()) {
		t.Fatalf("ann's repeat with a body: %d %s, want the same share at 720p/30", res.status, res.body)
	}
	if !drain() {
		t.Error("a quality change sent no presence frame")
	}
	if got, want := f.sharers(f.presenceV2(t, "ann2")), []string{"ann@720p/30", "ann2"}; !slices.Equal(got, want) {
		t.Errorf("after the change v2 sharers = %q, want %q", got, want)
	}
	if got := f.shareRows(t); !slices.Equal(got, wantRows) {
		t.Errorf("a quality change wrote rows: %q", got)
	}

	// Without a body the declared quality stands and the answer is V1.
	res = f.claimShare(t, "ann", "ops")
	var plain schema.VoiceShareV1
	strict(t, res.body, &plain)
	if res.status != http.StatusOK || !plain.StartedAt.Time().Equal(annShare.StartedAt.Time()) {
		t.Fatalf("ann's repeat without a body: %d %s", res.status, res.body)
	}
	if got, want := f.sharers(f.presenceV2(t, "ann2")), []string{"ann@720p/30", "ann2"}; !slices.Equal(got, want) {
		t.Errorf("after a repeat without a body v2 sharers = %q, want %q", got, want)
	}
}

// TestVoiceShareClaimBody: a malformed body is refused 400 after the caller's
// checks (a caller who is refused earlier is told that, whatever the body)
// and grants and writes nothing.
func TestVoiceShareClaimBody(t *testing.T) {
	tests := []struct {
		name       string
		who        string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"an unknown field", "ann", `{"quality":{"resolution":"720p","frame_rate":30},"audio":false}`, 400, "invalid_request"},
		{"an unknown resolution", "ann", `{"quality":{"resolution":"4k","frame_rate":30}}`, 400, "invalid_request"},
		{"an unknown frame rate", "ann", `{"quality":{"resolution":"720p","frame_rate":24}}`, 400, "invalid_request"},
		{"a quality missing its frame rate", "ann", `{"quality":{"resolution":"720p"}}`, 400, "invalid_request"},
		{"null", "ann", `null`, 400, "invalid_request"},
		{"an array", "ann", `[]`, 400, "invalid_request"},
		{"not json", "ann", `not json`, 400, "invalid_request"},
		{"trailing content", "ann", `{} {}`, 400, "invalid_request"},
		{"too large", "ann", `{}` + strings.Repeat(" ", 2048), 413, "request_too_large"},
		{"a non-member, whatever the body", "bob", `not json`, 404, "channel_not_found"},
		{"a member without a session, whatever the body", "dora2", `not json`, 409, schema.ErrorCodeVoiceNoSession},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newShareFixture(t)
			if tt.who == "dora2" { // a member who was never issued a session
				ctx := context.Background()
				p, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "dora2")
				if err != nil {
					t.Fatal(err)
				}
				_, tok, err := f.srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.srv.store.AddChannelMember(ctx, "system", f.ops.ID, p.ID, 0); err != nil {
					t.Fatal(err)
				}
				f.tokens["dora2"], f.ids["dora2"] = tok, p.ID
			}
			grants := f.lk.count("UpdateParticipant")
			res := f.claimShareBody(t, tt.who, tt.body)
			if res.status != tt.wantStatus || errCode(t, res.body) != tt.wantCode {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if rows := f.shareRows(t); len(rows) != 0 {
				t.Errorf("a refused claim wrote %q", rows)
			}
			if got := f.lk.count("UpdateParticipant") - grants; got != 0 {
				t.Errorf("a refused claim made %d UpdateParticipant calls", got)
			}
			if strings.Contains(res.body, tt.body) && len(tt.body) > 8 {
				t.Errorf("the refusal echoes the body: %s", res.body)
			}
		})
	}
}

// TestVoiceShareCap: voiceShareCap claims are granted; the next is 409
// voice_share_full, names nobody, writes nothing and asks LiveKit nothing; a
// holder's repeat at the cap is not a new claim; a place freed by a stop is
// taken by the next claim.
func TestVoiceShareCap(t *testing.T) {
	f, room := newShareFixture(t)
	names := []string{"ann", "ann2"}
	for i := 3; i <= voiceShareCap+1; i++ {
		name := fmt.Sprintf("m%d", i)
		f.addSharer(t, room, name, int64(i)*1_000)
		names = append(names, name)
	}
	f.at(10_000)
	f.pass(t)
	for i, name := range names[:voiceShareCap] {
		f.at(11_000 + i*100)
		f.mustClaim(t, name)
	}
	rows := len(f.shareRows(t))
	if rows != voiceShareCap {
		t.Fatalf("%d started rows, want %d", rows, voiceShareCap)
	}
	seventh := names[voiceShareCap]
	grants := f.lk.count("UpdateParticipant")
	for _, body := range []string{"", `{"quality":{"resolution":"720p","frame_rate":15}}`} {
		res := f.claimShareBody(t, seventh, body)
		if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceShareFull {
			t.Fatalf("the claim past the cap (body %q): %d %s, want 409 voice_share_full", body, res.status, res.body)
		}
		for _, name := range names {
			if strings.Contains(res.body, name) {
				t.Errorf("voice_share_full names %s: %s", name, res.body)
			}
		}
	}
	if got := f.shareRows(t); len(got) != rows {
		t.Errorf("a refused claim wrote %q", got[rows:])
	}
	if got := f.lk.count("UpdateParticipant") - grants; got != 0 {
		t.Errorf("a refused claim made %d UpdateParticipant calls", got)
	}
	if f.granted(t, room, seventh) {
		t.Error("the refused claimant was granted the sources")
	}
	if got := len(f.sharers(f.presenceV2(t, "ann2"))); got != voiceShareCap {
		t.Errorf("v2 shows %d sharers, want %d", got, voiceShareCap)
	}

	// A holder asking again at the cap is not a new claim.
	if res := f.claimShare(t, names[0], "ops"); res.status != http.StatusOK {
		t.Errorf("a holder's repeat at the cap: %d %s, want 200", res.status, res.body)
	}

	// After one stop the seventh succeeds, and the room is full again.
	f.mustStop(t, names[1])
	f.mustClaim(t, seventh)
	if res := f.claimShare(t, names[1], "ops"); res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceShareFull {
		t.Errorf("the stopped sharer claims into a full room: %d %s, want 409 voice_share_full", res.status, res.body)
	}
}

// ---------------------------------------------------------------------------
// The release

// TestVoiceShareReleaseByPrincipal: DELETE …/voice/share is the caller's own
// stream; ?principal= names someone else's, for a holder of manage_channels
// or a manager of the channel, and for no one else; and one stop never ends
// another person's stream.
func TestVoiceShareReleaseByPrincipal(t *testing.T) {
	tests := []struct {
		name       string
		who        string
		query      func(f *presenceFixture, ann, ann2 schema.VoiceShareV1) string
		channel    string
		wantStatus int
		wantCode   string
		// ended is who is no longer sharing afterwards; rows what was written.
		ended      []string
		wantRows   []string
		wantDenied bool
	}{
		{name: "the sharer, bare", who: "ann", wantStatus: 204, ended: []string{"ann"},
			wantRows: []string{"ann stopped principal=ann reason=self"}},
		{name: "the sharer, naming themselves", who: "ann",
			query: func(f *presenceFixture, _, _ schema.VoiceShareV1) string {
				return "principal=" + strconv.FormatInt(f.ids["ann"], 10)
			},
			wantStatus: 204, ended: []string{"ann"}, wantRows: []string{"ann stopped principal=ann reason=self"}},
		{name: "the sharer, with their started_at", who: "ann2",
			query: func(_ *presenceFixture, _, ann2 schema.VoiceShareV1) string {
				return "started_at=" + ann2.StartedAt.Time().UTC().Format("2006-01-02T15:04:05.000Z")
			},
			wantStatus: 204, ended: []string{"ann2"}, wantRows: []string{"ann2 stopped principal=ann2 reason=self"}},
		{name: "the sharer, with another stream's started_at", who: "ann2",
			query: func(_ *presenceFixture, ann, _ schema.VoiceShareV1) string {
				return "started_at=" + ann.StartedAt.Time().UTC().Format("2006-01-02T15:04:05.000Z")
			},
			wantStatus: 409, wantCode: schema.ErrorCodeVoiceShareStale},
		{name: "a manager names a sharer", who: "mgr",
			query: func(f *presenceFixture, _, _ schema.VoiceShareV1) string {
				return "principal=" + strconv.FormatInt(f.ids["ann"], 10)
			},
			wantStatus: 204, ended: []string{"ann"}, wantRows: []string{"mgr stopped principal=ann reason=operator"}},
		{name: "an operator who is not a member", who: "root",
			query: func(f *presenceFixture, _, _ schema.VoiceShareV1) string {
				return "principal=" + strconv.FormatInt(f.ids["ann2"], 10)
			},
			wantStatus: 204, ended: []string{"ann2"}, wantRows: []string{"root stopped principal=ann2 reason=operator"}},
		{name: "a plain member names a sharer", who: "plain",
			query: func(f *presenceFixture, _, _ schema.VoiceShareV1) string {
				return "principal=" + strconv.FormatInt(f.ids["ann"], 10)
			},
			wantStatus: 403, wantCode: "forbidden", wantDenied: true},
		{name: "a plain member names a person who shares nothing", who: "plain",
			query: func(f *presenceFixture, _, _ schema.VoiceShareV1) string {
				return "principal=" + strconv.FormatInt(f.ids["mgr"], 10)
			},
			wantStatus: 403, wantCode: "forbidden", wantDenied: true},
		{name: "a plain member names an id that never existed", who: "plain",
			query:      func(*presenceFixture, schema.VoiceShareV1, schema.VoiceShareV1) string { return "principal=99999" },
			wantStatus: 403, wantCode: "forbidden", wantDenied: true},
		{name: "a manager names a person who shares nothing", who: "mgr",
			query: func(f *presenceFixture, _, _ schema.VoiceShareV1) string {
				return "principal=" + strconv.FormatInt(f.ids["plain"], 10)
			},
			wantStatus: 204},
		{name: "an operator names an id that never existed", who: "root",
			query:      func(*presenceFixture, schema.VoiceShareV1, schema.VoiceShareV1) string { return "principal=99999" },
			wantStatus: 204},
		{name: "started_at with someone else's principal", who: "mgr",
			query: func(f *presenceFixture, ann, _ schema.VoiceShareV1) string {
				return "principal=" + strconv.FormatInt(f.ids["ann"], 10) + "&started_at=" + ann.StartedAt.Time().UTC().Format("2006-01-02T15:04:05.000Z")
			},
			wantStatus: 400, wantCode: "invalid_request"},
		{name: "a non-member names a sharer", who: "bob",
			query: func(f *presenceFixture, _, _ schema.VoiceShareV1) string {
				return "principal=" + strconv.FormatInt(f.ids["ann"], 10)
			},
			wantStatus: 404, wantCode: "channel_not_found"},
		{name: "a non-member names an id that never existed", who: "bob",
			query:      func(*presenceFixture, schema.VoiceShareV1, schema.VoiceShareV1) string { return "principal=99999" },
			wantStatus: 404, wantCode: "channel_not_found"},
		{name: "a malformed principal is 400 before the channel", who: "bob", channel: "nosuch",
			query:      func(*presenceFixture, schema.VoiceShareV1, schema.VoiceShareV1) string { return "principal=abc" },
			wantStatus: 400, wantCode: "invalid_request"},
		{name: "a zero principal", who: "mgr",
			query:      func(*presenceFixture, schema.VoiceShareV1, schema.VoiceShareV1) string { return "principal=0" },
			wantStatus: 400, wantCode: "invalid_request"},
		{name: "a negative principal", who: "mgr",
			query:      func(*presenceFixture, schema.VoiceShareV1, schema.VoiceShareV1) string { return "principal=-3" },
			wantStatus: 400, wantCode: "invalid_request"},
		{name: "a principal with a leading zero", who: "mgr",
			query:      func(*presenceFixture, schema.VoiceShareV1, schema.VoiceShareV1) string { return "principal=02" },
			wantStatus: 400, wantCode: "invalid_request"},
		{name: "a repeated principal", who: "root",
			query: func(*presenceFixture, schema.VoiceShareV1, schema.VoiceShareV1) string {
				return "principal=2&principal=3"
			},
			wantStatus: 400, wantCode: "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			f.addSharer(t, room, "mgr", 3_000)
			f.addSharer(t, room, "plain", 4_000)
			f.makeManager(t, "mgr")
			f.at(5_000)
			f.pass(t)
			f.at(6_000)
			ann := f.mustClaim(t, "ann")
			f.at(7_000)
			ann2 := f.mustClaim(t, "ann2")
			for _, who := range []string{"ann", "ann2"} {
				f.publish(room, who, true, false)
			}
			f.at(7_500)
			f.pass(t)
			rows := len(f.shareRows(t))
			denied := len(f.audits(t, "access_denied"))
			grants := f.lk.count("UpdateParticipant")

			query := ""
			if tt.query != nil {
				query = tt.query(f, ann, ann2)
			}
			channel := tt.channel
			if channel == "" {
				channel = "ops"
			}
			res := f.release(t, tt.who, channel, query)
			if res.status != tt.wantStatus || (tt.wantCode != "" && errCode(t, res.body) != tt.wantCode) {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if got := f.shareRows(t)[rows:]; !slices.Equal(got, tt.wantRows) {
				t.Errorf("rows written %q, want %q", got, tt.wantRows)
			}
			if got := len(f.audits(t, "access_denied")) - denied; (got == 1) != tt.wantDenied || got > 1 {
				t.Errorf("access_denied rows = %d, want one: %v", got, tt.wantDenied)
			}
			if len(tt.ended) == 0 && f.lk.count("UpdateParticipant") != grants {
				t.Error("a request that ended nothing asked LiveKit to change a permission")
			}
			for _, who := range []string{"ann", "ann2"} {
				gone := slices.Contains(tt.ended, who)
				if f.granted(t, room, who) == gone {
					t.Errorf("%s: granted = %v after the request, want %v", who, !gone, !gone)
				}
				p, _ := f.lk.participant(room, f.identity(who))
				if p.screen == gone {
					t.Errorf("%s: screen published = %v after the request, want %v", who, p.screen, !gone)
				}
			}
			want := 2 - len(tt.ended)
			if got := len(f.sharers(f.presenceV2(t, "ann2"))); got != want {
				t.Errorf("v2 shows %d sharers, want %d", got, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Presence versions

// TestVoicePresenceSchemaParam: ?schema= picks the version on the GET route:
// v1 by default and by name, v2 on request, 400 for anything else, decided
// before the channel is looked at (so it says nothing about the channel).
func TestVoicePresenceSchemaParam(t *testing.T) {
	f, _ := newShareFixture(t)
	f.at(1_000)
	f.mustClaim(t, "ann")
	f.at(2_000)
	f.mustClaim(t, "ann2")
	tests := []struct {
		name       string
		who        string
		path       string
		wantStatus int
		wantSchema string
	}{
		{"absent", "ann", "/v1/channels/ops/voice", 200, schema.VoicePresenceSchemaV1},
		{"v1 by name", "ann", "/v1/channels/ops/voice?schema=conch.voice_presence.v1", 200, schema.VoicePresenceSchemaV1},
		{"v2", "ann", "/v1/channels/ops/voice?schema=conch.voice_presence.v2", 200, schema.VoicePresenceSchemaV2},
		{"bogus", "ann", "/v1/channels/ops/voice?schema=bogus", 400, ""},
		{"empty", "ann", "/v1/channels/ops/voice?schema=", 400, ""},
		{"a future version", "ann", "/v1/channels/ops/voice?schema=conch.voice_presence.v3", 400, ""},
		{"repeated", "ann", "/v1/channels/ops/voice?schema=conch.voice_presence.v2&schema=conch.voice_presence.v2", 400, ""},
		{"bogus before the channel: unknown channel", "ann", "/v1/channels/nosuch/voice?schema=bogus", 400, ""},
		{"bogus before the channel: a non-member", "bob", "/v1/channels/ops/voice?schema=bogus", 400, ""},
		{"v2 for a non-member is the v1 refusal", "bob", "/v1/channels/ops/voice?schema=conch.voice_presence.v2", 404, ""},
		{"v2 for an unknown channel is the v1 refusal", "ann", "/v1/channels/nosuch/voice?schema=conch.voice_presence.v2", 404, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := f.callREST(t, http.MethodGet, tt.path, f.tokens[tt.who], "")
			if res.status != tt.wantStatus {
				t.Fatalf("%d %s, want %d", res.status, res.body, tt.wantStatus)
			}
			switch {
			case tt.wantStatus == 400:
				if errCode(t, res.body) != "invalid_request" || strings.Contains(res.body, "ops") {
					t.Errorf("the refusal %s says more than invalid_request", res.body)
				}
			case tt.wantSchema != "":
				var doc struct {
					Schema string `json:"schema"`
				}
				if err := json.Unmarshal([]byte(res.body), &doc); err != nil || doc.Schema != tt.wantSchema {
					t.Errorf("schema = %q, %v, want %q", doc.Schema, err, tt.wantSchema)
				}
			}
		})
	}
}

// TestVoicePresenceDegradedV1: with several streams a v1 document lists
// exactly one sharer (the reader's own, otherwise the earliest-started), with
// v1's fields and no more, and validates; v2 lists them all. With one stream
// and no quality v1 is what it always was.
func TestVoicePresenceDegradedV1(t *testing.T) {
	f, room := newShareFixture(t)
	f.addSharer(t, room, "viewer", 3_000)
	f.at(4_000)
	f.pass(t)

	// One undeclared stream: v1 is the published document, no new key.
	f.at(5_000)
	f.mustClaim(t, "ann2")
	res := f.callREST(t, http.MethodGet, "/v1/channels/ops/voice", f.tokens["viewer"], "")
	var v1 schema.VoicePresenceV1
	strict(t, res.body, &v1)
	if strings.Contains(res.body, "share_") || !slices.Equal(f.v1Sharers(t, "viewer"), []string{"ann2"}) {
		t.Fatalf("v1 with one stream: %s", res.body)
	}

	// Two streams, ann2's earlier: the lowest id (ann) is not the earliest.
	f.at(6_000)
	f.mustClaimQuality(t, "ann", `{"quality":{"resolution":"source","frame_rate":60}}`)
	tests := []struct {
		reader string
		want   []string
	}{
		{"viewer", []string{"ann2"}}, // the earliest-started
		{"ann2", []string{"ann2"}},   // their own
		{"ann", []string{"ann"}},     // their own, though they started later
	}
	for _, tt := range tests {
		t.Run("v1 as "+tt.reader, func(t *testing.T) {
			if got := f.v1Sharers(t, tt.reader); !slices.Equal(got, tt.want) {
				t.Errorf("v1 shows %q sharing, want %q", got, tt.want)
			}
			res := f.callREST(t, http.MethodGet, "/v1/channels/ops/voice", f.tokens[tt.reader], "")
			var doc schema.VoicePresenceV1
			strict(t, res.body, &doc)
			if err := doc.Validate(); err != nil || strings.Contains(res.body, "share_started_at") || strings.Contains(res.body, "share_quality") {
				t.Errorf("v1 document %s: %v", res.body, err)
			}
		})
	}
	if got, want := f.sharers(f.presenceV2(t, "viewer")), []string{"ann@source/60", "ann2"}; !slices.Equal(got, want) {
		t.Errorf("v2 sharers = %q, want %q", got, want)
	}

	// When the earliest ends, the next-earliest appears.
	f.mustStop(t, "ann2")
	if got := f.v1Sharers(t, "viewer"); !slices.Equal(got, []string{"ann"}) {
		t.Errorf("after ann2 stopped v1 shows %q, want ann", got)
	}
}

// mustClaimQuality claims as who with body and requires 200.
func (f *presenceFixture) mustClaimQuality(t *testing.T, who, body string) {
	t.Helper()
	if res := f.claimShareBody(t, who, body); res.status != http.StatusOK {
		t.Fatalf("%s claims with %s: %d %s", who, body, res.status, res.body)
	}
}

// TestVoicePresenceSocketSchema: the socket honours ?schema= as the GET route
// does, and refuses a bad value with 400 before anything else.
func TestVoicePresenceSocketSchema(t *testing.T) {
	f, _ := newShareFixture(t)
	f.at(1_000)
	f.mustClaim(t, "ann")
	f.at(2_000)
	f.mustClaimQuality(t, "ann2", `{"quality":{"resolution":"720p","frame_rate":30}}`)
	base := wsTestServer(t, f.srv)

	conn, _, err := dialVoice(t, base, "ops&schema="+schema.VoicePresenceSchemaV2, f.tokens["ann2"])
	if err != nil {
		t.Fatalf("dial v2: %v", err)
	}
	var v2 schema.VoicePresenceV2
	strict(t, readRawPresence(t, conn), &v2)
	if err := v2.Validate(); err != nil || !slices.Equal(f.sharers(v2), []string{"ann", "ann2@720p/30"}) {
		t.Errorf("v2 frame: sharers %q, %v", f.sharers(v2), err)
	}
	conn, _, err = dialVoice(t, base, "ops", f.tokens["ann2"])
	if err != nil {
		t.Fatalf("dial v1: %v", err)
	}
	var v1 schema.VoicePresenceV1
	raw := readRawPresence(t, conn)
	strict(t, raw, &v1)
	if err := v1.Validate(); err != nil || strings.Contains(raw, "share_started_at") {
		t.Errorf("v1 frame %s: %v", raw, err)
	}

	for _, channel := range []string{"ops&schema=bogus", "nosuch&schema=bogus", "ops&schema=conch.voice_presence.v2&schema=conch.voice_presence.v1"} {
		_, resp, err := dialVoice(t, base, channel, f.tokens["ann2"])
		if err == nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
			t.Errorf("dial %q: %v %v, want a 400 refusal", channel, resp, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Companion audio, per sharer

// TestVoiceCompanionAudioPerSharer: a sharer's consent reaches their own
// companion and no one else's; a caller who holds no stream is 409
// voice_no_session; each stream's end closes its own consent first.
func TestVoiceCompanionAudioPerSharer(t *testing.T) {
	f, room := newShareFixture(t)
	f.lk.setRoom(room, mustParticipant(t, f, room, "ann"), mustParticipant(t, f, room, "ann2"),
		fakeParticipant{identity: f.screenID("ann"), joinedMs: 3_000},
		fakeParticipant{identity: f.screenID("ann2"), joinedMs: 3_500})
	f.at(100)
	f.pass(t)
	f.at(1_000)
	f.mustClaim(t, "ann")
	f.at(1_100)
	f.mustClaim(t, "ann2")

	audio := func(id string) bool {
		for _, u := range f.lk.updatesFor(id) {
			if slices.Contains(u.sources, "SCREEN_SHARE_AUDIO") {
				return true
			}
		}
		return false
	}
	// Neither companion has the sound until its own holder asks.
	if audio(f.screenID("ann")) || audio(f.screenID("ann2")) {
		t.Fatal("a companion was granted the sound by a claim")
	}
	if res := f.companionAudio(t, "ann", audioApplication); res.status != http.StatusOK {
		t.Fatalf("ann consents: %d %s", res.status, res.body)
	}
	if !audio(f.screenID("ann")) || audio(f.screenID("ann2")) {
		t.Fatalf("ann's consent: ann's companion has the sound = %v, ann2's = %v, want true, false", audio(f.screenID("ann")), audio(f.screenID("ann2")))
	}
	// A caller with no stream: voice_no_session, nothing issued.
	f.addSharer(t, room, "idle", 4_000)
	before := f.lk.count("UpdateParticipant")
	res := f.companionAudio(t, "idle", audioApplication)
	if res.status != http.StatusConflict || errCode(t, res.body) != schema.ErrorCodeVoiceNoSession {
		t.Errorf("a caller with no stream: %d %s, want 409 voice_no_session", res.status, res.body)
	}
	if f.lk.count("UpdateParticipant") != before {
		t.Error("a refused consent updated a participant")
	}
	// ann2 consents too; then ann2's stream ends: ann2's consent is closed
	// first and ann's stands.
	if res := f.companionAudio(t, "ann2", audioSystem); res.status != http.StatusOK {
		t.Fatalf("ann2 consents: %d %s", res.status, res.body)
	}
	f.at(2_000)
	f.mustStopOf(t, "ann2")
	var got []string
	for _, r := range f.shareRows(t) {
		if strings.Contains(r, "audio_") || strings.Contains(r, "stopped") {
			got = append(got, r)
		}
	}
	want := []string{"ann audio_started scope=application", "ann2 audio_started scope=system_excluding_call",
		"root audio_stopped principal=ann2 reason=operator", "root stopped principal=ann2 reason=operator"}
	if !slices.Equal(got, want) {
		t.Errorf("rows %q, want %q", got, want)
	}
	// ann stops her consent; ann2's companion is not touched by it.
	n := len(f.lk.updatesFor(f.screenID("ann2")))
	if res := f.stopCompanionAudio(t, "ann"); res.status != http.StatusNoContent {
		t.Fatalf("ann withdraws: %d %s", res.status, res.body)
	}
	if len(f.lk.updatesFor(f.screenID("ann2"))) != n {
		t.Error("ann's withdrawal updated ann2's companion")
	}
}

// ---------------------------------------------------------------------------
// The passes

// TestVoiceMultiSharePasses: every way a stream ends ends one person's and no
// other's, with its rows, and rotation ends every stream.
func TestVoiceMultiSharePasses(t *testing.T) {
	setup := func(t *testing.T) (*presenceFixture, string) {
		f, room := newShareFixture(t)
		f.at(1_000)
		f.mustClaim(t, "ann")
		f.at(1_100)
		f.mustClaim(t, "ann2")
		f.publish(room, "ann", true, false)
		f.publish(room, "ann2", true, false)
		f.at(1_500)
		f.pass(t)
		return f, room
	}
	standing := func(t *testing.T, f *presenceFixture, room, who string) {
		t.Helper()
		p, _ := f.lk.participant(room, f.identity(who))
		if !p.shareGranted || !p.screen {
			t.Errorf("%s's stream was touched: %+v", who, p)
		}
		if !slices.Contains(f.sharers(f.presenceV2(t, "ann2")), who) {
			t.Errorf("%s is not listed as sharing", who)
		}
	}
	t.Run("one leaves", func(t *testing.T) {
		f, room := setup(t)
		f.lk.setRoom(room, mustParticipant(t, f, room, "ann2"))
		f.at(2_000)
		f.pass(t)
		if got, want := f.shareRows(t)[2:], []string{"ann stopped principal=ann reason=left"}; !slices.Equal(got, want) {
			t.Errorf("rows %q, want %q", got, want)
		}
		standing(t, f, room, "ann2")
	})
	t.Run("one loses share_screen", func(t *testing.T) {
		f, room := setup(t)
		// root holds share_screen as an operator; ann and ann2 hold it as
		// members. Revoking the member cell takes it from them and not from
		// root, whose stream stands.
		ctx := context.Background()
		if _, err := f.srv.store.AddChannelMember(ctx, "system", f.ops.ID, f.root.ID, 0); err != nil {
			t.Fatal(err)
		}
		decodeSession(t, f.session(t, "root", "ops"))
		f.lk.mu.Lock()
		f.lk.rooms[room] = append(f.lk.rooms[room], fakeParticipant{identity: f.identity("root"), joinedMs: 3_000, published: true, muted: true})
		f.lk.mu.Unlock()
		f.at(1_800)
		f.pass(t)
		f.mustClaim(t, "root")
		doc, err := f.srv.store.Permissions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		req := schema.PutPermissionsRequestV1{Revision: doc.Revision, Changes: []schema.PermissionChangeV1{
			{Permission: schema.PermShareScreen, Role: schema.PermissionRoleMember, Granted: false},
		}}
		if _, err := f.srv.store.PutPermissions(ctx, fmt.Sprintf("principal:%d", f.root.ID), f.root.ID, req, f.clock.Now()); err != nil {
			t.Fatal(err)
		}
		f.at(2_000)
		f.pass(t)
		want := []string{"ann stopped principal=ann reason=access_lost", "ann2 stopped principal=ann2 reason=access_lost"}
		if got := f.shareRows(t)[3:]; !slices.Equal(got, want) {
			t.Errorf("rows %q, want %q", got, want)
		}
		if got := f.sharers(f.presenceV2(t, "ann2")); !slices.Equal(got, []string{"root"}) {
			t.Errorf("v2 sharers = %q, want root alone", got)
		}
		if f.granted(t, room, "ann") || f.granted(t, room, "ann2") || !f.granted(t, room, "root") {
			t.Error("the grants are not ann and ann2 withdrawn, root's standing")
		}
	})
	t.Run("the voice switch of one", func(t *testing.T) {
		f, room := setup(t)
		rec := f.do(t, http.MethodPut, fmt.Sprintf("/v1/principals/%d/voice-permission", f.ids["ann"]), f.rootTok, `{"voice_allowed":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("voice-permission = %d %s", rec.Code, rec.Body)
		}
		if got, want := f.shareRows(t)[2:], []string{"ann stopped principal=ann reason=access_lost"}; !slices.Equal(got, want) {
			t.Errorf("rows %q, want %q", got, want)
		}
		if f.granted(t, room, "ann") {
			t.Error("ann's sources are still granted")
		}
		standing(t, f, room, "ann2")
		if f.srv.voice.roomNamed(room) == nil || f.room(t, f.ops) != room {
			t.Error("a voice switch rotated the room")
		}
	})
	t.Run("idle, per stream", func(t *testing.T) {
		f, room := setup(t)
		f.publish(room, "ann", false, false)
		f.at(4_000)
		f.pass(t) // inside voiceShareTrackGone
		f.at(7_000)
		f.pass(t)
		if got, want := f.shareRows(t)[2:], []string{"ann stopped principal=ann reason=idle"}; !slices.Equal(got, want) {
			t.Errorf("rows %q, want %q", got, want)
		}
		standing(t, f, room, "ann2")
	})
	t.Run("an unclaimed track beside two holders", func(t *testing.T) {
		f, room := setup(t)
		f.addSharer(t, room, "stray", 3_000)
		f.lk.update(room, f.identity("stray"), func(p *fakeParticipant) { p.screen = true })
		f.at(2_000)
		f.pass(t)
		if n := len(f.audits(t, store.AuditVoiceShareUnclaimed)); n != 1 {
			t.Errorf("%d unclaimed rows, want one", n)
		}
		if f.granted(t, room, "stray") {
			t.Error("the stray's screen was left granted")
		}
		if got := f.shareRows(t); len(got) != 3 || !strings.Contains(got[2], "unclaimed") {
			t.Errorf("rows %q, want the two starts and one unclaimed", got)
		}
		standing(t, f, room, "ann")
		standing(t, f, room, "ann2")
	})
	t.Run("rotation ends every stream", func(t *testing.T) {
		f, room := setup(t)
		f.addSharer(t, room, "leaver", 3_000)
		f.at(1_800)
		f.pass(t)
		if code := f.doOrdered(t, http.MethodDelete, fmt.Sprintf("/v1/channels/ops/members/%d", f.ids["leaver"]), f.rootTok); code != http.StatusNoContent {
			t.Fatalf("remove status = %d", code)
		}
		want := []string{"ann stopped principal=ann reason=rotated", "ann2 stopped principal=ann2 reason=rotated"}
		if got := f.shareRows(t)[2:]; !slices.Equal(got, want) {
			t.Errorf("rows %q, want %q", got, want)
		}
		if got := f.sharers(f.presenceV2(t, "ann2")); len(got) != 0 {
			t.Errorf("v2 still shows %q sharing", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Follow-ups of the #1543 review (issue #1546)

// makeAgentOperator gives the agent robo a manifest that holds
// manage_channels, and requires that it does: the tests below are about what
// a holder of the permission that is an agent may not do.
func (f *presenceFixture) makeAgentOperator(t *testing.T) {
	t.Helper()
	req := schema.PutAgentManifestRequestV1{
		DisplayName:  "robo",
		Tier:         schema.AgentTierA,
		Capabilities: []schema.Capability{schema.CapabilityAdminAct, schema.CapabilityMessagesRead},
		Channels: []schema.ChannelGrant{{ChannelID: f.ops.ID, Permissions: []schema.ChannelPermission{
			schema.ChannelPermissionRead, schema.ChannelPermissionPost,
		}}},
		Permissions: []schema.Permission{schema.PermManageChannels},
		Admin:       &schema.AgentAdminV1{ApprovalChannelID: f.ops.ID},
	}
	ctx := context.Background()
	if _, _, err := f.srv.store.PutAgentManifest(ctx, "system", f.ids["robo"], req); err != nil {
		t.Fatal(err)
	}
	held, err := f.srv.store.Permitted(store.WithAdminDispatch(ctx), f.ids["robo"], schema.PermManageChannels)
	if err != nil || !held {
		t.Fatalf("robo holds manage_channels = %v, %v; the test needs it held", held, err)
	}
}

// stopAsDispatchedAgent calls handler as who for a DELETE with query, with
// the context marked as the admin door's. That is the only context in which
// an agent holds a permission at all (store.WithAdminDispatch), and so the
// only one in which the operator branch could admit one; the public routes
// authenticate with a token and never carry it.
func (f *presenceFixture) stopAsDispatchedAgent(t *testing.T, handler http.HandlerFunc, who, query string) wireResult {
	t.Helper()
	caller, err := f.srv.store.PrincipalByID(context.Background(), f.ids[who])
	if err != nil {
		t.Fatal(err)
	}
	ctx := store.WithAdminDispatch(withCaller(context.Background(), caller))
	req := httptest.NewRequest(http.MethodDelete, "/v1/channels/ops/voice?"+query, nil).WithContext(ctx)
	req.SetPathValue("channel", "ops")
	rec := httptest.NewRecorder()
	handler(rec, req)
	return wireResult{rec.Code, rec.Body.String(), rec.Header().Get("Content-Type")}
}

// TestVoiceShareAgentCannotModerate: an agent holding manage_channels gets
// the 403 forbidden a plain member gets when it names someone else's share,
// and the share goes on; a human operator and the agent's own stop are
// unchanged.
func TestVoiceShareAgentCannotModerate(t *testing.T) {
	tests := []struct {
		name       string
		who        string
		agentOp    bool
		wantStatus int
		wantCode   string
		wantEnded  bool
	}{
		{name: "an agent with manage_channels names a sharer", who: "robo", agentOp: true, wantStatus: 403, wantCode: "forbidden"},
		{name: "a plain member names a sharer", who: "plain", wantStatus: 403, wantCode: "forbidden"},
		{name: "a human operator names a sharer", who: "root", wantStatus: 204, wantEnded: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, room := newShareFixture(t)
			f.addSharer(t, room, "plain", 4_000)
			if tt.agentOp {
				f.makeAgentOperator(t)
			}
			f.at(5_000)
			f.pass(t)
			f.at(6_000)
			f.mustClaim(t, "ann")
			query := "principal=" + strconv.FormatInt(f.ids["ann"], 10)
			var res wireResult
			if tt.agentOp {
				res = f.stopAsDispatchedAgent(t, f.srv.handleVoiceShareStop, tt.who, query)
			} else {
				res = f.stopShareOf(t, tt.who, "ann")
			}
			if res.status != tt.wantStatus || (tt.wantCode != "" && errCode(t, res.body) != tt.wantCode) {
				t.Fatalf("got %d %s, want %d %s", res.status, res.body, tt.wantStatus, tt.wantCode)
			}
			if got := !slices.Contains(f.sharers(f.presenceV2(t, "ann2")), "ann"); got != tt.wantEnded {
				t.Errorf("ann's share ended = %v, want %v", got, tt.wantEnded)
			}
			if got := f.granted(t, room, "ann"); got == tt.wantEnded {
				t.Errorf("ann granted = %v after the stop, want %v", got, !tt.wantEnded)
			}
		})
	}
}

// TestVoiceShareQualityBound: a holder's repeat claim that changes the
// quality is bounded per principal, three in three seconds; the first change
// is never refused, a repeat that leaves the quality alone is not a change,
// a refused change leaves the quality as it was and tells when to retry, and
// another sharer's allowance is their own.
func TestVoiceShareQualityBound(t *testing.T) {
	const (
		q720  = `{"quality":{"resolution":"720p","frame_rate":30}}`
		q1080 = `{"quality":{"resolution":"1080p","frame_rate":60}}`
		src   = `{"quality":{"resolution":"source","frame_rate":30}}`
	)
	type step struct {
		atMs       int
		who        string
		body       string
		wantStatus int
		wantLabel  string // ann's quality in presence afterwards, when set
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"the first change is not throttled", []step{
			{0, "ann", q1080, 200, "ann@1080p/60"},
		}},
		{"a burst of three, then the fourth is refused", []step{
			{0, "ann", q720, 200, "ann@720p/30"},
			{100, "ann", q1080, 200, "ann@1080p/60"},
			{200, "ann", q720, 200, "ann@720p/30"},
			{300, "ann", q1080, 200, "ann@1080p/60"},
			{400, "ann", q720, 429, "ann@1080p/60"},
		}},
		{"a repeat that leaves the quality alone is not a change", []step{
			{0, "ann", q720, 200, "ann@720p/30"},
			{100, "ann", q1080, 200, "ann@1080p/60"},
			{200, "ann", q720, 200, "ann@720p/30"},
			{300, "ann", q1080, 200, "ann@1080p/60"},
			{400, "ann", q1080, 200, "ann@1080p/60"},
			{500, "ann", "", 200, "ann@1080p/60"},
		}},
		{"the allowance comes back as the window slides", []step{
			{0, "ann", q720, 200, ""},
			{100, "ann", q1080, 200, ""},
			{200, "ann", q720, 200, ""},
			{300, "ann", q1080, 200, ""},
			{400, "ann", src, 429, "ann@1080p/60"},
			{3_150, "ann", src, 200, "ann@source/30"},
		}},
		{"another sharer's allowance is their own", []step{
			{0, "ann", q720, 200, ""},
			{100, "ann", q1080, 200, ""},
			{200, "ann", q720, 200, ""},
			{300, "ann", q1080, 200, ""},
			{400, "ann", q720, 429, ""},
			{450, "ann2", q1080, 200, ""},
			{500, "ann2", q720, 200, ""},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, _ := newShareFixture(t)
			f.at(0)
			// Both hold a share with the 720p quality declared, so every step
			// that names another one is a change.
			f.mustClaimQuality(t, "ann", q720)
			f.mustClaimQuality(t, "ann2", q720)
			for i, s := range tt.steps {
				f.at(s.atMs)
				before := f.lk.count("UpdateParticipant")
				res := f.claimShareBody(t, s.who, s.body)
				if res.status != s.wantStatus {
					t.Fatalf("step %d (%s at %d ms): %d %s, want %d", i, s.who, s.atMs, res.status, res.body, s.wantStatus)
				}
				if res.status == http.StatusTooManyRequests {
					var e schema.Error
					strict(t, res.body, &e)
					if e.Code != schema.ErrorCodeRateLimited || e.RetryAfterSeconds < 1 {
						t.Errorf("step %d: refusal %s, want rate_limited with a retry_after_seconds of at least 1", i, res.body)
					}
					if got := f.lk.count("UpdateParticipant") - before; got != 0 {
						t.Errorf("step %d: a refused change made %d UpdateParticipant calls", i, got)
					}
				}
				if s.wantLabel != "" {
					if got := f.sharers(f.presenceV2(t, "ann2")); !slices.Contains(got, s.wantLabel) {
						t.Errorf("step %d: sharers %q, want %q among them", i, got, s.wantLabel)
					}
				}
			}
			if rows := f.shareRows(t); len(rows) != 2 {
				t.Errorf("rows %q: a quality change, refused or not, writes none", rows)
			}
		})
	}
}
