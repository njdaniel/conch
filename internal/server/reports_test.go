package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/njdaniel/conch/internal/server/livekit"
	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Problem reports (issue #446).

// testReport is a valid report request; change edits it before it is sent.
func testReport(change func(r *schema.CreateProblemReportRequestV1)) schema.CreateProblemReportRequestV1 {
	at := schema.NewTimestamp(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC))
	r := schema.CreateProblemReportRequestV1{
		Did:      "Opened the page and joined voice",
		Expected: "To hear the others",
		Happened: "Voice kept reconnecting",
		Often:    schema.ProblemReportOftenSometimes,
		Client:   schema.ProblemReportClientV1{Name: "conch-web", Version: "v0.1.0-test", Platform: "Linux x86_64", UserAgent: "Mozilla/5.0 test", Screen: "1280x800", Voice: "reconnecting"},
		Diagnostics: []schema.DiagnosticEventV1{
			{At: at, Kind: schema.DiagnosticSocketClose, Socket: schema.DiagnosticSocketMessages, Code: 1006},
			{At: at, Kind: schema.DiagnosticHTTPFailure, Code: 503, Error: "voice_unavailable", RequestID: "0123456789abcdef"},
		},
	}
	if change != nil {
		change(&r)
	}
	return r
}

func reportJSON(t *testing.T, r schema.CreateProblemReportRequestV1) string {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func reportBody(t *testing.T) string { return reportJSON(t, testReport(nil)) }

// A person's report is stored with everything the server adds, audited by id
// alone, and read back whole by an operator.
func TestCreateReport(t *testing.T) {
	f := newMemberFixture(t)
	// Something for the server's part of the report: a request alice made
	// before she reported.
	earlier := f.do(t, "GET", "/v2/channels/alpha/messages", f.aliceTok, "").Header().Get(schema.RequestIDHeader)

	rec := f.do(t, "POST", "/v1/reports", f.aliceTok, reportBody(t))
	if rec.Code != http.StatusCreated {
		t.Fatalf("report = %d %s", rec.Code, rec.Body)
	}
	created := decodeBody[schema.CreateProblemReportResponseV1](t, rec)
	if created.ID <= 0 || created.CreatedAt.IsZero() {
		t.Fatalf("created = %+v", created)
	}

	rec = f.do(t, "GET", fmt.Sprintf("/v1/reports/%d", created.ID), f.rootTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s", rec.Code, rec.Body)
	}
	got := decodeBody[schema.ProblemReportV1](t, rec)
	want := testReport(nil)
	switch {
	case got.ID != created.ID, got.State != schema.ProblemReportStateOpen, got.PrincipalID != f.alice.ID:
		t.Errorf("identity = %d %s %d", got.ID, got.State, got.PrincipalID)
	case got.ServerVersion != "v1.2.3-test", got.Voice != schema.HealthVoiceNotConfigured:
		t.Errorf("server version %q, voice %q", got.ServerVersion, got.Voice)
	case got.Did != want.Did || got.Expected != want.Expected || got.Happened != want.Happened || got.Often != want.Often:
		t.Errorf("answers = %+v", got)
	case got.Client != want.Client:
		t.Errorf("client = %+v, want %+v", got.Client, want.Client)
	case len(got.Diagnostics) != 2 || got.Diagnostics[1].RequestID != "0123456789abcdef":
		t.Errorf("diagnostics = %+v", got.Diagnostics)
	case len(got.Server.Requests) != 1 || got.Server.Requests[0].RequestID != earlier || got.Server.Requests[0].Route != "GET /v2/channels/{channel}/messages":
		t.Errorf("server requests = %+v, want the one alice made before", got.Server.Requests)
	case got.Issue != "" || got.FiledAt != nil:
		t.Errorf("an open report has an issue: %+v", got)
	}

	rows := auditRows(t, f.authFixture, store.AuditProblemReported)
	if len(rows) != 1 || rows[0].Actor != fmt.Sprintf("principal:%d", f.alice.ID) || rows[0].Subject != fmt.Sprintf("report:%d", created.ID) || rows[0].Detail != "" {
		t.Errorf("audit = %+v, want one row with the id alone", rows)
	}
}

// Who may send and read reports, and what a malformed or oversized one gets.
func TestReportRefusals(t *testing.T) {
	f := newMemberFixture(t)
	big := testReport(func(r *schema.CreateProblemReportRequestV1) {
		r.Happened = strings.Repeat("€", schema.MaxProblemReportText) // three bytes each: past the body limit while every answer is in bounds
		r.Did, r.Expected = r.Happened, r.Happened
	})
	tests := []struct {
		name, method, path, token, body string
		status                          int
		code                            string
	}{
		{"an agent", "POST", "/v1/reports", f.botTok, reportBody(t), http.StatusForbidden, schema.ErrorCodeHumanRequired},
		{"no credential", "POST", "/v1/reports", "", reportBody(t), http.StatusUnauthorized, "unauthenticated"},
		{"too large", "POST", "/v1/reports", f.aliceTok, reportJSON(t, big), http.StatusRequestEntityTooLarge, "request_too_large"},
		{"not json", "POST", "/v1/reports", f.aliceTok, "hello", http.StatusBadRequest, "invalid_request"},
		{"unknown field", "POST", "/v1/reports", f.aliceTok, strings.Replace(reportBody(t), `{`, `{"token":"x",`, 1), http.StatusBadRequest, "invalid_request"},
		{"blank answer", "POST", "/v1/reports", f.aliceTok, reportJSON(t, testReport(func(r *schema.CreateProblemReportRequestV1) { r.Happened = " " })), http.StatusBadRequest, "invalid_request"},
		{"free text in a diagnostic", "POST", "/v1/reports", f.aliceTok, reportJSON(t, testReport(func(r *schema.CreateProblemReportRequestV1) {
			r.Diagnostics[1].Error = "message hello world"
		})), http.StatusBadRequest, "invalid_request"},
		{"a member lists", "GET", "/v1/reports", f.aliceTok, "", http.StatusForbidden, "forbidden"},
		{"a member reads", "GET", "/v1/reports/1", f.aliceTok, "", http.StatusForbidden, "forbidden"},
		{"a member files", "PUT", "/v1/reports/1", f.aliceTok, `{"state":"filed","issue":"https://example.com/1"}`, http.StatusForbidden, "forbidden"},
		{"no such report", "GET", "/v1/reports/9999", f.rootTok, "", http.StatusNotFound, schema.ErrorCodeProblemReportNotFound},
		{"bad id", "GET", "/v1/reports/x", f.rootTok, "", http.StatusBadRequest, "invalid_request"},
		{"file no such report", "PUT", "/v1/reports/9999", f.rootTok, `{"state":"filed","issue":"https://example.com/1"}`, http.StatusNotFound, schema.ErrorCodeProblemReportNotFound},
		{"bad limit", "GET", "/v1/reports?limit=-1", f.rootTok, "", http.StatusBadRequest, "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertErrorBody(t, f.do(t, tt.method, tt.path, tt.token, tt.body), tt.status, tt.code)
		})
	}
	if rows := auditRows(t, f.authFixture, store.AuditProblemReported); len(rows) != 0 {
		t.Errorf("refused reports were stored: %+v", rows)
	}
	denied := 0
	for _, e := range auditRows(t, f.authFixture, "access_denied") {
		if strings.Contains(e.Detail, denyAgentReports) {
			denied++
		}
	}
	if denied != 1 {
		t.Errorf("%d access_denied rows for the agent, want 1", denied)
	}

	off := newAuthFixture(t, AuthOff)
	assertErrorBody(t, off.do(t, "POST", "/v1/reports", "", reportBody(t)), http.StatusUnauthorized, "unauthenticated")
}

// Past the limit a person gets 429 with Retry-After; the refusals are
// audited, thinned; the bucket refills with time; another person is not
// affected.
func TestReportRateLimit(t *testing.T) {
	f := newMemberFixture(t)
	clock := newManualClock()
	f.srv.now = clock.Now
	f.srv.setReportLimit(inviteLimit{"reports_per_principal", 2, time.Hour})

	send := func(tok string) int { return f.do(t, "POST", "/v1/reports", tok, reportBody(t)).Code }
	for i := range 2 {
		if got := send(f.aliceTok); got != http.StatusCreated {
			t.Fatalf("report %d = %d", i, got)
		}
	}
	for range 30 {
		rec := f.do(t, "POST", "/v1/reports", f.aliceTok, reportBody(t))
		assertErrorBody(t, rec, http.StatusTooManyRequests, schema.ErrorCodeRateLimited)
		if rec.Header().Get("Retry-After") == "" {
			t.Fatal("no Retry-After")
		}
	}
	if got := send(f.carolTok); got != http.StatusCreated {
		t.Errorf("another person's report = %d", got)
	}
	rows := auditRows(t, f.authFixture, auditReportRateLimited)
	if len(rows) == 0 || len(rows) >= 30 {
		t.Errorf("%d rate-limit audit rows for 30 refusals, want some, thinned", len(rows))
	}
	if !strings.Contains(rows[0].Detail, "limit=reports_per_principal max=2 window_seconds=3600") {
		t.Errorf("audit detail = %q", rows[0].Detail)
	}
	if n := len(auditRows(t, f.authFixture, store.AuditProblemReported)); n != 3 {
		t.Errorf("%d reports stored, want 3", n)
	}
	clock.Advance(31 * time.Minute)
	if got := send(f.aliceTok); got != http.StatusCreated {
		t.Errorf("after the refill = %d", got)
	}
}

// An operator lists reports newest first, without their text, and files one.
func TestReportOperatorRoutes(t *testing.T) {
	f := newMemberFixture(t)
	var ids []int64
	for _, tok := range []string{f.aliceTok, f.carolTok} {
		rec := f.do(t, "POST", "/v1/reports", tok, reportBody(t))
		if rec.Code != http.StatusCreated {
			t.Fatalf("report = %d %s", rec.Code, rec.Body)
		}
		ids = append(ids, decodeBody[schema.CreateProblemReportResponseV1](t, rec).ID)
	}

	rec := f.do(t, "GET", "/v1/reports", f.rootTok, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Voice kept reconnecting") {
		t.Error("the list carries a report's text")
	}
	list := decodeBody[schema.ListProblemReportsResponseV1](t, rec)
	if len(list.Reports) != 2 || list.Reports[0].ID != ids[1] || list.Reports[1].PrincipalID != f.alice.ID ||
		list.Reports[0].ClientName != "conch-web" || list.Reports[0].ServerVersion != "v1.2.3-test" {
		t.Errorf("list = %+v", list)
	}
	if one := decodeBody[schema.ListProblemReportsResponseV1](t, f.do(t, "GET", "/v1/reports?limit=1", f.rootTok, "")); len(one.Reports) != 1 {
		t.Errorf("limit=1 gave %d", len(one.Reports))
	}

	assertErrorBody(t, f.do(t, "PUT", fmt.Sprintf("/v1/reports/%d", ids[0]), f.rootTok, `{"state":"open","issue":"https://example.com/1"}`), http.StatusBadRequest, "invalid_request")
	assertErrorBody(t, f.do(t, "PUT", fmt.Sprintf("/v1/reports/%d", ids[0]), f.rootTok, `{"state":"filed","issue":"javascript:x"}`), http.StatusBadRequest, "invalid_request")

	issue := "https://github.com/example/conch/issues/900"
	clock := newManualClock()
	f.srv.now = clock.Now
	rec = f.do(t, "PUT", fmt.Sprintf("/v1/reports/%d", ids[0]), f.rootTok, fmt.Sprintf(`{"state":"filed","issue":%q}`, issue))
	if rec.Code != http.StatusOK {
		t.Fatalf("file = %d %s", rec.Code, rec.Body)
	}
	filed := decodeBody[schema.ProblemReportV1](t, rec)
	if filed.State != schema.ProblemReportStateFiled || filed.Issue != issue || filed.FiledAt == nil || !filed.FiledAt.Time().Equal(schema.NewTimestamp(clock.Now()).Time()) {
		t.Errorf("filed = %+v", filed)
	}
	if again := decodeBody[schema.ProblemReportV1](t, f.do(t, "GET", fmt.Sprintf("/v1/reports/%d", ids[0]), f.rootTok, "")); again.State != schema.ProblemReportStateFiled || again.Did != filed.Did {
		t.Errorf("read back = %+v", again)
	}
	rows := auditRows(t, f.authFixture, store.AuditProblemReportFiled)
	if len(rows) != 1 || rows[0].Actor != fmt.Sprintf("principal:%d", f.root.ID) || rows[0].Subject != fmt.Sprintf("report:%d", ids[0]) || rows[0].Detail != "" {
		t.Errorf("filed audit = %+v", rows)
	}
	if l := decodeBody[schema.ListProblemReportsResponseV1](t, f.do(t, "GET", "/v1/reports", f.rootTok, "")); l.Reports[1].State != schema.ProblemReportStateFiled || l.Reports[1].Issue != issue {
		t.Errorf("list after filing = %+v", l.Reports[1])
	}
}

// The redaction test: a whole session (chat, a socket dropped, voice failing,
// an invite) and then a report. Neither the stored report nor anything logged
// on the way holds a token, a ticket, an invite code, a room name, message
// text or a channel name.
func TestReportRedaction(t *testing.T) {
	logs := captureDiagLogs(t)
	dead := newDeadURL(t)
	f := newAuthFixtureWithConfig(t, Config{AuthMode: AuthRequired, LiveKit: livekit.Config{
		URL: "ws://lk.invalid", APIURL: dead, APIKey: "devkey", APISecret: livekit.NewSecret("devsecret-that-is-long-enough"),
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	const channelName = "kestrel-ops"
	const messageText = "the falcon flies at midnight"
	if rec := f.do(t, "POST", "/v0/channels", f.rootTok, fmt.Sprintf(`{"name":%q}`, channelName)); rec.Code != http.StatusCreated {
		t.Fatalf("create channel = %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, "PUT", fmt.Sprintf("/v1/channels/%s/members/%d", channelName, f.alice.ID), f.rootTok, ""); rec.Code/100 != 2 {
		t.Fatalf("add member = %d %s", rec.Code, rec.Body)
	}
	base := wsTestServer(t, f.srv)
	secrets := []string{f.aliceTok, f.rootTok, f.botTok, channelName, messageText, "devsecret-that-is-long-enough"}

	// Chat.
	if rec := f.do(t, "POST", "/v2/channels/"+channelName+"/messages", f.aliceTok, fmt.Sprintf(`{"body":%q}`, messageText)); rec.Code != http.StatusCreated {
		t.Fatalf("post = %d %s", rec.Code, rec.Body)
	}
	// A socket opened with a ticket, then dropped.
	rec := f.do(t, "POST", "/v1/ws-tickets", f.aliceTok, "")
	ticket := decodeBody[schema.WSTicketV1](t, rec).Ticket
	secrets = append(secrets, ticket)
	conn, resp, err := openTicketSocket(ctx, base, "/v2/ws?channel="+channelName+"&after=0&ticket="+ticket)
	if err != nil {
		t.Fatalf("open socket: %v", err)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if _, _, err := conn.Read(ctx); err != nil { // the replayed message
		t.Fatalf("read: %v", err)
	}
	_ = conn.CloseNow()
	waitLines(t, logs, 1, `msg="ws: socket closed"`)
	// Voice failing: LiveKit does not answer.
	voice := f.do(t, "POST", "/v1/channels/"+channelName+"/voice/session", f.aliceTok, "")
	if voice.Code != http.StatusServiceUnavailable {
		t.Fatalf("voice session = %d %s, want 503", voice.Code, voice.Body)
	}
	if ch, err := f.srv.store.ChannelByName(ctx, channelName); err == nil {
		rooms, _ := f.srv.store.VoiceRoomsForChannel(ctx, ch.ID)
		for _, r := range rooms {
			secrets = append(secrets, r.RoomName)
		}
	}
	// An invite, made and looked at.
	rec = f.do(t, "POST", "/v1/invites", f.rootTok, fmt.Sprintf(`{"kind":"open","channels":[%q]}`, channelName))
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite = %d %s", rec.Code, rec.Body)
	}
	code := decodeBody[schema.CreateInviteResponseV1](t, rec).Code
	secrets = append(secrets, code)
	f.do(t, "POST", "/v1/invites/preview", "", fmt.Sprintf(`{"code":%q}`, code))

	// The report, with the client's own events of all that.
	at := schema.NewTimestamp(time.Now())
	report := testReport(func(r *schema.CreateProblemReportRequestV1) {
		r.Diagnostics = []schema.DiagnosticEventV1{
			{At: at, Kind: schema.DiagnosticSocketOpen, Socket: schema.DiagnosticSocketMessages},
			{At: at, Kind: schema.DiagnosticSocketClose, Socket: schema.DiagnosticSocketMessages, Code: 1006},
			{At: at, Kind: schema.DiagnosticReconnect, Socket: schema.DiagnosticSocketMessages, Attempt: 1},
			{At: at, Kind: schema.DiagnosticHTTPFailure, Code: voice.Code, Error: schema.ErrorCodeVoiceUnavailable, RequestID: voice.Header().Get(schema.RequestIDHeader)},
			{At: at, Kind: schema.DiagnosticVoiceState, State: schema.DiagnosticVoiceUnavailable},
			{At: at, Kind: schema.DiagnosticDeviceError, Name: "NotAllowedError"},
		}
	})
	rec = f.do(t, "POST", "/v1/reports", f.aliceTok, reportJSON(t, report))
	if rec.Code != http.StatusCreated {
		t.Fatalf("report = %d %s", rec.Code, rec.Body)
	}
	id := decodeBody[schema.CreateProblemReportResponseV1](t, rec).ID
	// What is stored, as the store gives it back: the operator's answer adds
	// the names to redact (every channel and person), which are the point of
	// that list and are checked below, not here.
	kept, err := f.srv.store.ProblemReport(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	stored := string(raw)
	got := decodeBody[schema.ProblemReportV1](t, f.do(t, "GET", fmt.Sprintf("/v1/reports/%d", id), f.rootTok, ""))
	got.Redact = nil
	if gotRaw, _ := json.Marshal(got); string(gotRaw) != stored {
		t.Errorf("the operator's answer differs from what is stored beyond redact:\n%s\n%s", gotRaw, stored)
	}
	if len(got.Server.Requests) != 4 || len(got.Server.SocketCloses) != 1 || got.Server.SocketCloses[0].Reason != schema.SocketCloseClient {
		t.Errorf("the server's part = %+v", got.Server)
	}
	if got.Voice != schema.HealthVoiceUnavailable {
		t.Errorf("voice = %s, want unavailable", got.Voice)
	}

	haystacks := map[string]string{"stored report": stored, "log": logs.String()}
	for name, hay := range haystacks {
		for _, s := range secrets {
			if s != "" && strings.Contains(hay, s) {
				t.Errorf("the %s holds %q", name, s)
			}
		}
		for _, prefix := range []string{schema.CredentialTokenPrefix, schema.WSTicketPrefix, schema.InviteCodePrefix} {
			if strings.Contains(hay, prefix) {
				t.Errorf("the %s holds a %q", name, prefix)
			}
		}
	}
}

// newDeadURL is the address of a server that is no longer there.
func newDeadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close()
	return u
}

// The operator's answer for one report names everything an issue body must
// leave out: every channel and net, the operator's or not, and every
// principal. No other answer carries the list, and it is never stored.
func TestReportRedactList(t *testing.T) {
	f := newMemberFixture(t)
	ctx := context.Background()
	// A private group's channel and net the operator is in neither of.
	private, err := f.srv.store.CreateChannel(ctx, "raid-night")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.AddChannelMember(ctx, "system", private.ID, f.carol.ID, 0); err != nil {
		t.Fatal(err)
	}
	if rec := f.do(t, "POST", "/v1/channels/raid-night/nets", f.rootTok, `{"name":"tanks"}`); rec.Code != http.StatusCreated {
		t.Fatalf("net = %d %s", rec.Code, rec.Body)
	}
	rec := f.do(t, "POST", "/v1/reports", f.carolTok, reportBody(t))
	id := decodeBody[schema.CreateProblemReportResponseV1](t, rec).ID

	got := decodeBody[schema.ProblemReportV1](t, f.do(t, "GET", fmt.Sprintf("/v1/reports/%d", id), f.rootTok, ""))
	want := &schema.ProblemReportRedactionV1{
		Channels:   []string{"alpha", "general", "raid-night", "tanks"},
		Principals: []string{"alice", "bot", "carol", "ghost", "root"},
	}
	if !reflect.DeepEqual(got.Redact, want) {
		t.Errorf("redact = %+v, want %+v", got.Redact, want)
	}
	stored, err := f.srv.store.ProblemReport(ctx, id)
	if err != nil || stored.Redact != nil {
		t.Errorf("the stored report carries redact: %+v %v", stored.Redact, err)
	}
	if body := f.do(t, "GET", "/v1/reports", f.rootTok, "").Body.String(); strings.Contains(body, "redact") || strings.Contains(body, "raid-night") {
		t.Errorf("the list carries names: %s", body)
	}
	filed := f.do(t, "PUT", fmt.Sprintf("/v1/reports/%d", id), f.rootTok, `{"state":"filed","issue":"https://example.com/issues/1"}`)
	if strings.Contains(filed.Body.String(), "redact") {
		t.Errorf("filing answers with the names: %s", filed.Body)
	}
}
