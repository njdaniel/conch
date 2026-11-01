package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// authFixture is a server with an operator ("root", id 1), two members
// ("alice" human, "bot" agent) and a channel "general", each principal holding
// one credential.
type authFixture struct {
	srv      *Server
	root     store.Principal
	alice    store.Principal
	bot      store.Principal
	rootTok  string
	aliceTok string
	botTok   string
}

func newAuthFixture(t *testing.T, mode AuthMode) *authFixture {
	t.Helper()
	return newAuthFixtureWithConfig(t, Config{AuthMode: mode})
}

// newAuthFixtureWithConfig is newAuthFixture with the server's whole Config,
// for a test that needs more than the auth mode (an ntfy server, say).
func newAuthFixtureWithConfig(t *testing.T, cfg Config) *authFixture {
	t.Helper()
	srv := newTestServerWithConfig(t, cfg)
	ctx := context.Background()
	root, _, rootTok, err := srv.store.BootstrapOperator(ctx, "root")
	if err != nil {
		t.Fatalf("BootstrapOperator: %v", err)
	}
	f := &authFixture{srv: srv, root: root, rootTok: rootTok}
	mk := func(kind store.PrincipalKind, name string) (store.Principal, string) {
		p, err := srv.store.CreatePrincipal(ctx, kind, name)
		if err != nil {
			t.Fatalf("CreatePrincipal %s: %v", name, err)
		}
		_, tok, err := srv.store.CreateCredential(ctx, "system", p.ID, "test", nil)
		if err != nil {
			t.Fatalf("CreateCredential %s: %v", name, err)
		}
		return p, tok
	}
	f.alice, f.aliceTok = mk(store.PrincipalHuman, "alice")
	f.bot, f.botTok = mk(store.PrincipalAgent, "bot")
	// A fourth principal that belongs to nothing, for membership routes.
	if _, err := srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "ghost"); err != nil {
		t.Fatalf("CreatePrincipal ghost: %v", err)
	}
	general, err := srv.store.CreateChannel(ctx, "general")
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	// Since issue #90 a new channel has no members; the route matrix below
	// treats "general" as the channel every fixture caller may use.
	for _, p := range []store.Principal{root, f.alice, f.bot} {
		if _, err := srv.store.AddChannelMember(ctx, "system", general.ID, p.ID, 0); err != nil {
			t.Fatalf("AddChannelMember %s: %v", p.Name, err)
		}
	}
	// Since issue #79 an agent also needs a manifest; the fixture agent may do
	// everything in "general".
	setAgentManifest(t, srv, f.bot.ID, nil, general.ID)
	return f
}

func (f *authFixture) do(t *testing.T, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f *authFixture) audit(t *testing.T) []store.AuditEvent {
	t.Helper()
	events, err := f.srv.store.ListAuditEvents(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// routeClass is the independently stated expectation for one route, used to
// check the route table rather than derive from it.
type routeClass string

const (
	classExempt routeClass = "exempt"
	classAuth   routeClass = "auth"
	classOp     routeClass = "operator"
	classSelf   routeClass = "operator-or-self"
	// classOwner routes need an operator holding the owner mark, and
	// classManager routes an operator or a manager of the channel (issue #790).
	classOwner   routeClass = "owner"
	classManager routeClass = "channel-manager"
)

// routeExpectation pins, per route pattern: its class, the request to send,
// and the status an authorized caller gets (the same status AuthOff gives).
type routeExpectation struct {
	class routeClass
	path  string
	body  string
	// allowed is the status an authorized caller gets, which is also what the
	// route returns with AuthOff (whoami excepted: it needs a caller).
	allowed int
}

// routeExpectations must name every pattern in the route table. {id} is the
// operator's principal id (1) so operator-or-self is exercised from a member.
var routeExpectations = map[string]routeExpectation{
	"GET /healthz":      {classExempt, "/healthz", "", 200},
	"GET /v0/ws":        {classAuth, "/v0/ws", "", 400},
	"GET /v1/ws":        {classAuth, "/v1/ws", "", 400},
	"GET /v2/ws":        {classAuth, "/v2/ws", "", 400},
	"GET /v1/whoami":    {classAuth, "/v1/whoami", "", 200},
	"POST /v0/channels": {classOp, "/v0/channels", `{}`, 400},
	"GET /v1/channels":  {classAuth, "/v1/channels", "", 200},
	// Member channel creation and channel settings (issue #981).
	"POST /v1/channels":              {classAuth, "/v1/channels", `{}`, 400},
	"PUT /v1/channels/{id}/settings": {classOp, "/v1/channels/9999/settings", `{}`, 400},
	// Both membership writes are no-ops in the fixture (alice is already a
	// member; ghost, id 4, is not), so they change nothing and audit nothing.
	"GET /v1/channels/{channel}/members":                   {classAuth, "/v1/channels/general/members", "", 200},
	"PUT /v1/channels/{channel}/members/{principal_id}":    {classManager, "/v1/channels/general/members/2", "", 204},
	"DELETE /v1/channels/{channel}/members/{principal_id}": {classManager, "/v1/channels/general/members/4", "", 204},
	// Adding a member by name (issue #790): an empty body is a 400 before
	// anyone is looked up.
	"POST /v1/channels/{channel}/members": {classManager, "/v1/channels/general/members", `{}`, 400},
	// Nets (issue #115). The net-addressed routes use a net that does not
	// exist, so they change and audit nothing.
	"POST /v1/channels/{channel}/nets":                                {classOp, "/v1/channels/general/nets", `{}`, 400},
	"GET /v1/channels/{channel}/nets":                                 {classAuth, "/v1/channels/general/nets", "", 200},
	"DELETE /v1/channels/{channel}/nets/{net}":                        {classOp, "/v1/channels/general/nets/nosuch", "", 404},
	"PUT /v1/channels/{channel}/nets/{net}/members/{principal_id}":    {classOp, "/v1/channels/general/nets/nosuch/members/2", `{"role":"member"}`, 404},
	"DELETE /v1/channels/{channel}/nets/{net}/members/{principal_id}": {classOp, "/v1/channels/general/nets/nosuch/members/2", "", 404},
	"POST /v0/principals":                                             {classOp, "/v0/principals", `{}`, 400},
	"POST /v0/channels/{channel}/messages":                            {classAuth, "/v0/channels/general/messages", `{}`, 400},
	"GET /v0/channels/{channel}/messages":                             {classAuth, "/v0/channels/general/messages", "", 200},
	"POST /v1/channels/{channel}/messages":                            {classAuth, "/v1/channels/general/messages", `{}`, 400},
	"GET /v1/channels/{channel}/messages":                             {classAuth, "/v1/channels/general/messages", "", 200},
	"POST /v2/channels/{channel}/messages":                            {classAuth, "/v2/channels/general/messages", `{}`, 400},
	"GET /v2/channels/{channel}/messages":                             {classAuth, "/v2/channels/general/messages", "", 200},
	"GET /v2/channels/{channel}/messages/search":                      {classAuth, "/v2/channels/general/messages/search?q=hi", "", 200},
	// Files (issue #23). The fixture leaves uploads disabled, so a member is
	// refused with 403 uploads_disabled; AuthOff gets 400 and an agent 403
	// (special-cased below). The file routes use an id that does not exist.
	"POST /v2/channels/{channel}/files": {classAuth, "/v2/channels/general/files?filename=a.txt", "x", 403},
	"GET /v2/files/{id}/meta":           {classAuth, "/v2/files/9999/meta", "", 404},
	"GET /v2/files/{id}":                {classAuth, "/v2/files/9999", "", 404},
	// Voice (issue #126). The fixture does not configure voice, so a member
	// gets 503 voice_not_configured; AuthOff gets 400 and an agent 403 (both
	// special-cased below).
	"POST /v1/channels/{channel}/voice/session": {classAuth, "/v1/channels/general/voice/session", "", 503},
	// Transmit reports (issue #135) check the caller as a session does, by
	// the same code: 503 for a member here, 400 under AuthOff, 403 for an
	// agent. The body is not read before those answers.
	"POST /v1/channels/{channel}/voice/transmit": {classAuth, "/v1/channels/general/voice/transmit", `{"state":"started"}`, 503},
	// Screen share (issue #443): checked as the routes above, so 503 for a
	// member here, 400 under AuthOff, 403 for an agent. Stopping lets an
	// operator through member or not, and the fixture's operator gets the same
	// 503 a member does.
	"POST /v1/channels/{channel}/voice/share":   {classAuth, "/v1/channels/general/voice/share", "", 503},
	"DELETE /v1/channels/{channel}/voice/share": {classAuth, "/v1/channels/general/voice/share", "", 503},
	// Companion and viewer participants (issue #933): checked as the session
	// route is, so 503 for a member here, 400 under AuthOff, 403 for an agent.
	"POST /v1/channels/{channel}/voice/companion":   {classAuth, "/v1/channels/general/voice/companion", `{"publish":"screen","subscribe":true}`, 503},
	"DELETE /v1/channels/{channel}/voice/companion": {classAuth, "/v1/channels/general/voice/companion", "", 503},
	// Voice presence (issue #127). Not configured is a valid snapshot, so a
	// member gets 200; AuthOff gets 400 and an agent 403 on the snapshot (both
	// special-cased below). The socket needs a channel parameter, so every
	// caller who passes authentication gets 400 before anything else.
	"GET /v1/channels/{channel}/voice": {classAuth, "/v1/channels/general/voice", "", 200},
	"GET /v1/voice/ws":                 {classAuth, "/v1/voice/ws", "", 400},
	// Voice occupancy (issue #596). Not configured is a valid document, so a
	// person with voice gets 200; the socket takes no parameter, so a plain
	// GET gets as far as the upgrade and is refused there with 426. AuthOff
	// gets 400 and an agent 403 on both (special-cased below).
	"GET /v1/voice/occupancy":    {classAuth, "/v1/voice/occupancy", "", 200},
	"GET /v1/voice/occupancy/ws": {classAuth, "/v1/voice/occupancy/ws", "", 426},
	// The events socket (issue #269) needs a channel parameter too, so every
	// caller who passes authentication gets 400 (AuthOff: sessions_require_auth).
	"GET /v1/events/ws": {classAuth, "/v1/events/ws", "", 400},
	// Sessions (issue #265). Chosen to change and audit nothing: an empty
	// create is a 400 (a client is required), the session routes use an id
	// that does not exist, and the list reads a channel every fixture caller
	// is in. AuthOff gets 400 sessions_require_auth on all four (special-cased
	// below).
	"POST /v1/sessions":                {classAuth, "/v1/sessions", `{}`, 400},
	"POST /v1/sessions/{id}/heartbeat": {classAuth, "/v1/sessions/9999/heartbeat", "", 404},
	"DELETE /v1/sessions/{id}":         {classAuth, "/v1/sessions/9999", "", 404},
	"GET /v1/sessions":                 {classAuth, "/v1/sessions?channel=general", "", 200},
	// Runs (issue #266). Chosen to change and audit nothing: every route that
	// names a run or a session uses an id that does not exist, and the list
	// reads a channel every fixture caller is in. Starting a run is for agents
	// only, so a human (the member and the operator) gets 403 agent_required
	// and the fixture agent 404 session_not_found (special-cased below).
	// AuthOff gets 400 sessions_require_auth on all six (special-cased below).
	"POST /v1/sessions/{id}/runs": {classAuth, "/v1/sessions/9999/runs", `{}`, 403},
	"POST /v1/runs/{id}/state":    {classAuth, "/v1/runs/9999/state", `{}`, 404},
	"POST /v1/runs/{id}/activity": {classAuth, "/v1/runs/9999/activity", `{}`, 404},
	"GET /v1/runs/{id}":           {classAuth, "/v1/runs/9999", "", 404},
	"GET /v1/runs":                {classAuth, "/v1/runs?channel=general", "", 200},
	"GET /v1/runs/{id}/activity":  {classAuth, "/v1/runs/9999/activity", "", 404},
	// Interventions (issue #287). Chosen to change and audit nothing: every
	// route names a run, an intervention or a session that does not exist.
	// Asking is for humans, so an agent gets 403 human_required (special-cased
	// below); the poll is an own-session call, so a human and the fixture
	// agent both get 404 session_not_found. AuthOff gets 400
	// sessions_require_auth on all five (special-cased below).
	"POST /v1/runs/{id}/interventions":    {classAuth, "/v1/runs/9999/interventions", `{"kind":"cancel"}`, 404},
	"GET /v1/runs/{id}/interventions":     {classAuth, "/v1/runs/9999/interventions", "", 404},
	"GET /v1/interventions/{id}":          {classAuth, "/v1/interventions/9999", "", 404},
	"GET /v1/sessions/{id}/interventions": {classAuth, "/v1/sessions/9999/interventions?state=requested", "", 404},
	"POST /v1/interventions/{id}/ack":     {classAuth, "/v1/interventions/9999/ack", `{"state":"applied"}`, 404},
	// Read state (issue #351): people only, so an agent gets 403 on both and
	// AuthOff 400 read_state_require_auth (both special-cased below). A mark of
	// 0 writes nothing.
	"GET /v1/read-state":                    {classAuth, "/v1/read-state", "", 200},
	"PUT /v1/channels/{channel}/read-state": {classAuth, "/v1/channels/general/read-state", `{"last_read_id":0}`, 200},
	// Reactions (issue #765): message 9999 does not exist, so an authorized
	// caller gets message_not_found; an agent is refused only at the two
	// moderation routes (humans only), and AuthOff gets 400
	// reactions_require_auth on all seven (special-cased below).
	"PUT /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}":                   {classAuth, "/v2/channels/general/messages/9999/reactions/%F0%9F%91%8D", "", 404},
	"DELETE /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}":                {classAuth, "/v2/channels/general/messages/9999/reactions/%F0%9F%91%8D", "", 404},
	"GET /v2/channels/{channel}/messages/{message_id}/reactions":                           {classAuth, "/v2/channels/general/messages/9999/reactions", "", 404},
	"GET /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}":                   {classAuth, "/v2/channels/general/messages/9999/reactions/%F0%9F%91%8D", "", 404},
	"GET /v2/channels/{channel}/reactions":                                                 {classAuth, "/v2/channels/general/reactions?messages=9999", "", 200},
	"DELETE /v2/channels/{channel}/messages/{message_id}/reactions/{emoji}/{principal_id}": {classManager, "/v2/channels/general/messages/9999/reactions/%F0%9F%91%8D/2", "", 404},
	"DELETE /v2/channels/{channel}/messages/{message_id}/reactors/{principal_id}":          {classManager, "/v2/channels/general/messages/9999/reactors/2", "", 404},
	"PUT /v2/channels/{channel}/messages/{message_id}/body":                                {classAuth, "/v2/channels/general/messages/9999/body", `{"body":"x"}`, 404},
	"GET /v2/channels/{channel}/messages/{message_id}/revisions":                           {classAuth, "/v2/channels/general/messages/9999/revisions", "", 404},
	"PUT /v1/principals/{id}/manifest":                                                     {classOp, "/v1/principals/1/manifest", `{}`, 400},
	"GET /v1/principals/{id}/manifest":                                                     {classSelf, "/v1/principals/1/manifest", "", 404},
	"PUT /v1/principals/{id}/name":                                                         {classSelf, "/v1/principals/1/name", `{}`, 400},
	"POST /v1/principals/{id}/credentials":                                                 {classOp, "/v1/principals/1/credentials", `{}`, 400},
	"GET /v1/principals/{id}/credentials":                                                  {classOp, "/v1/principals/1/credentials", "", 200},
	"POST /v1/credentials/{credential_id}/rotate":                                          {classOp, "/v1/credentials/9999/rotate", "", 404},
	"DELETE /v1/credentials/{credential_id}":                                               {classOp, "/v1/credentials/9999", "", 404},
	// Signing out (issue #439) revokes the caller's own credential, so the
	// role matrix sends it from a fixture of its own (special-cased below).
	// The fixture's operator holds the only operator credential: 409. AuthOff
	// has no credential to revoke: 401, as whoami.
	"DELETE /v1/credentials/current": {classAuth, "/v1/credentials/current", "", 409},
	// Your sign-ins (issue #496): any caller lists its own, and ending one
	// names a credential that does not exist, so it changes and audits
	// nothing. AuthOff has no caller whose they could be: 401 on both, as
	// whoami (special-cased below).
	"GET /v1/credentials/mine":         {classAuth, "/v1/credentials/mine", "", 200},
	"DELETE /v1/credentials/mine/{id}": {classAuth, "/v1/credentials/mine/9999", "", 404},
	// End visit (issue #705): nobody in the fixture is a guest, so every
	// caller is 403 not_a_guest; AuthOff is 403 accounts_require_auth.
	"POST /v1/guest/end": {classAuth, "/v1/guest/end", "", 403},
	// Unknown principal / already-enabled ghost: no-ops that write no audit
	// event, so the AuthOff audit count below is unaffected.
	"POST /v1/principals/{id}/disable":                {classOp, "/v1/principals/9999/disable", "", 404},
	"POST /v1/principals/{id}/enable":                 {classOp, "/v1/principals/4/enable", "", 204},
	"POST /v1/principals/{id}/credentials/revoke-all": {classSelf, "/v1/principals/9999/credentials/revoke-all", "", 404},
	"POST /v1/hooks":                                  {classOp, "/v1/hooks", `{}`, 400},
	"GET /v1/hooks":                                   {classOp, "/v1/hooks", "", 200},
	"DELETE /v1/hooks/{id}":                           {classOp, "/v1/hooks/9999", "", 404},
	"POST /v1/hooks/{token}":                          {classExempt, "/v1/hooks/nope", `{}`, 404},
	"POST /v1/approvals":                              {classAuth, "/v1/approvals", `{}`, 400},
	"GET /v1/approvals":                               {classAuth, "/v1/approvals", "", 200},
	"GET /v1/approvals/{id}":                          {classAuth, "/v1/approvals/9999", "", 404},
	"POST /v1/approvals/{id}/decisions":               {classAuth, "/v1/approvals/1/decisions", `{}`, 400},
	"/mcp":                                            {classExempt, "/mcp", `{}`, 401},
	// limit=0 is a 400 before anything is exported or recorded, so the AuthOff
	// audit count below is unaffected by a route that audits every success.
	"GET /v1/audit": {classOwner, "/v1/audit?limit=0", "", 400},
	// A ticket is for a credential: AuthOff gets 400 tickets_require_auth
	// (special-cased below). Issuing one writes no audit row.
	"POST /v1/ws-tickets": {classAuth, "/v1/ws-tickets", "", 201},
	// Invites (issue #437). Chosen to change and audit nothing: an empty
	// create is a 400, the close names an invite that does not exist, and
	// preview sends no code, which is the invite_invalid 404 before
	// anything is looked up or audited. The v1 redeem makes no account
	// whatever it is sent (#713): 409 password_required, for every caller
	// and under AuthOff alike. People only: an agent gets 403 and AuthOff
	// 401 on the three authenticated routes (both special-cased below).
	"POST /v1/invites":         {classAuth, "/v1/invites", `{}`, 400},
	"GET /v1/invites":          {classAuth, "/v1/invites", "", 200},
	"DELETE /v1/invites/{id}":  {classAuth, "/v1/invites/9999", "", 404},
	"POST /v1/invites/preview": {classExempt, "/v1/invites/preview", `{}`, 404},
	"POST /v1/invites/redeem":  {classExempt, "/v1/invites/redeem", `{}`, 409},
	// Guest entry (issue #710). The front door is open to anyone. Join as
	// guest and the guest redemption answer a bearer token, so they are 403
	// https_required to this fixture's plain HTTP from a test address before
	// anything is read; AuthOff, 403 accounts_require_auth (special-cased
	// below).
	"GET /v1/join":                  {classExempt, "/v1/join", "", 200},
	"POST /v1/join/guest":           {classExempt, "/v1/join/guest", `{}`, 403},
	"POST /v1/invites/redeem-guest": {classExempt, "/v1/invites/redeem-guest", `{}`, 403},
	// Accounts (issue #711) take a password: Create an account from the bare
	// address and from a visit are 403 for every caller before anything is
	// read, https_required to this fixture's plain HTTP and
	// accounts_require_auth under AuthOff. The operator's conversion permit
	// names a principal that does not exist: 404 not_a_guest, nothing
	// audited; AuthOff is 403 accounts_require_auth (special-cased below).
	"POST /v1/join/account":                  {classExempt, "/v1/join/account", `{}`, 403},
	"POST /v1/guest/convert":                 {classAuth, "/v1/guest/convert", `{}`, 403},
	"POST /v1/guests/{id}/conversion-permit": {classOp, "/v1/guests/9999/conversion-permit", `{}`, 404},
	// The operator's guest routes (issue #710). A settings change with no
	// field and a bulk end with no dry_run are 400s before anything is
	// written; the per-id end names a principal that does not exist. AuthOff
	// is 403 accounts_require_auth (special-cased below).
	"GET /v1/join/settings":    {classOp, "/v1/join/settings", "", 200},
	"PUT /v1/join/settings":    {classOwner, "/v1/join/settings", `{}`, 400},
	"GET /v1/guests":           {classOp, "/v1/guests", "", 200},
	"POST /v1/guests/{id}/end": {classOp, "/v1/guests/9999/end", "", 404},
	"POST /v1/guests/end":      {classOp, "/v1/guests/end", `{}`, 400},
	// The v2 redeem and the sign-in take a password (issue #707), so they
	// answer 403 before reading anything: https_required to this fixture's
	// plain HTTP from a test address, and accounts_require_auth under AuthOff.
	"POST /v2/invites/redeem": {classExempt, "/v2/invites/redeem", `{}`, 403},
	"POST /v1/login":          {classExempt, "/v1/login", `{}`, 403},
	// The password routes (issue #708) take a password too: the same 403
	// for every caller, before anything is read.
	"POST /v1/password/mine":  {classAuth, "/v1/password/mine", `{}`, 403},
	"PUT /v1/password/mine":   {classAuth, "/v1/password/mine", `{}`, 403},
	"POST /v1/password/reset": {classExempt, "/v1/password/reset", `{}`, 403},
	"POST /v1/reauth":         {classAuth, "/v1/reauth", `{}`, 403},
	// Operator administration of invites (issue #438). Each names an invite
	// or a principal that does not exist, or sets the pause switch to the
	// value it has, so it changes and audits nothing. Like the routes above,
	// AuthOff gets 401 (special-cased below).
	"GET /v1/invites/{id}/principals":           {classOp, "/v1/invites/9999/principals", "", 404},
	"POST /v1/invites/{id}/disable-joiners":     {classOp, "/v1/invites/9999/disable-joiners", `{"dry_run":true}`, 404},
	"POST /v1/principals/{id}/disable-invitees": {classOp, "/v1/principals/9999/disable-invitees", `{"dry_run":true}`, 404},
	"PUT /v1/principals/{id}/invite-permission": {classOp, "/v1/principals/9999/invite-permission", `{"may_invite":false}`, 404},
	"GET /v1/invites/settings":                  {classOp, "/v1/invites/settings", "", 200},
	"PUT /v1/invites/settings":                  {classOp, "/v1/invites/settings", `{"paused":false}`, 200},
	// Sign-in links (issue #493). The create names a principal that does not
	// exist, and preview and redeem send no code, which is the
	// sign_in_link_invalid 404 before anything is looked up or audited.
	// AuthOff gets 401 on the create and the close (special-cased below).
	"POST /v1/principals/{id}/sign-in-links": {classOp, "/v1/principals/9999/sign-in-links", `{}`, 404},
	"POST /v1/sign-in-links/preview":         {classExempt, "/v1/sign-in-links/preview", `{}`, 404},
	"POST /v1/sign-in-links/redeem":          {classExempt, "/v1/sign-in-links/redeem", `{}`, 404},
	// Closing one names a link that does not exist: 404, nothing audited.
	"DELETE /v1/sign-in-links/{id}": {classOp, "/v1/sign-in-links/9999", "", 404},
	// Problem reports (issue #446). An empty report is a 400 before anything
	// is stored or audited; the operator routes name a report that does not
	// exist. Sending one is for people: an agent gets 403 and AuthOff 401
	// (both special-cased below).
	"POST /v1/reports":     {classAuth, "/v1/reports", `{}`, 400},
	"GET /v1/reports":      {classOp, "/v1/reports", "", 200},
	"GET /v1/reports/{id}": {classOp, "/v1/reports/9999", "", 404},
	"PUT /v1/reports/{id}": {classOp, "/v1/reports/9999", `{"state":"filed","issue":"https://example.com/issues/1"}`, 404},
	// Operator administration of principals (issue #445). The hide names a
	// principal that does not exist, the bulk hide selects nobody (a 400
	// before anything is read), and the list only reads. The bulk hide, like
	// the bulk disables, needs a caller to leave out: AuthOff gets 401
	// (special-cased below).
	"POST /v1/principals/{id}/hide": {classOp, "/v1/principals/9999/hide", "", 404},
	"POST /v1/principals/hide":      {classOp, "/v1/principals/hide", `{"dry_run":true}`, 400},
	"GET /v1/principals":            {classOp, "/v1/principals", "", 200},
	// Device links (issue #712): a member's own. The member gets 403
	// reauth_required (no proof on the fixture's credential), an empty
	// list, and 404 for a link that does not exist; agents and operators are
	// refused, and AuthOff is 403 accounts_require_auth (special-cased below).
	"POST /v1/sign-in-links/mine":        {classAuth, "/v1/sign-in-links/mine", `{}`, 403},
	"GET /v1/sign-in-links/mine":         {classAuth, "/v1/sign-in-links/mine", "", 200},
	"DELETE /v1/sign-in-links/mine/{id}": {classAuth, "/v1/sign-in-links/mine/9999", "", 404},
	// Operator sign-in and setup (issue #709) take a code or a token, so
	// every caller gets the 403 of the password routes before anything is
	// read: https_required to this fixture's plain HTTP, and
	// accounts_require_auth under AuthOff.
	"POST /v1/login/totp":   {classExempt, "/v1/login/totp", `{}`, 403},
	"POST /v1/setup/redeem": {classExempt, "/v1/setup/redeem", `{}`, 403},
	"POST /v1/totp/enroll":  {classAuth, "/v1/totp/enroll", "", 403},
	"POST /v1/totp/confirm": {classAuth, "/v1/totp/confirm", `{}`, 403},

	// Renaming a channel (issues #727, #791): the manager class resolves the
	// id before the body, so an unknown channel is 404 and changes nothing.
	"PUT /v1/channels/{id}/name": {classManager, "/v1/channels/9999/name", `{}`, 404},

	// Changing a role or the owner mark (issue #790): an empty body is a 400
	// before the target is looked up.
	"PUT /v1/principals/{id}/role": {classOwner, "/v1/principals/1/role", `{}`, 400},
}

// isDeviceLinkRoute reports whether pattern is one of a member's own device
// link routes (issue #712), which refuse agents and operators.
func isDeviceLinkRoute(pattern string) bool {
	switch pattern {
	case "POST /v1/sign-in-links/mine", "GET /v1/sign-in-links/mine", "DELETE /v1/sign-in-links/mine/{id}":
		return true
	}
	return false
}

// isFileRoute reports whether pattern is one of the file routes (issue #23),
// which are humans-only and never anonymous.
func isFileRoute(pattern string) bool {
	return strings.Contains(pattern, "/v2/files/") || strings.HasSuffix(pattern, "/files")
}

// isInviteRoute reports whether pattern is one of the authenticated invite
// routes (issues #437 and #438); preview and redeem are exempt and not among
// them.
func isInviteRoute(pattern string) bool {
	switch pattern {
	case "POST /v1/invites", "GET /v1/invites", "DELETE /v1/invites/{id}",
		"GET /v1/invites/{id}/principals", "POST /v1/invites/{id}/disable-joiners",
		"POST /v1/principals/{id}/disable-invitees", "PUT /v1/principals/{id}/invite-permission",
		"GET /v1/invites/settings", "PUT /v1/invites/settings",
		// Making a sign-in link (issue #493) starts as the invite routes do.
		"POST /v1/principals/{id}/sign-in-links", "DELETE /v1/sign-in-links/{id}":
		return true
	}
	return false
}

// isGuestPolicyRoute reports whether pattern is one of issue #710's routes
// that answer accounts_require_auth with authentication off: every one but
// the front door; and issue #711's conversion permit, an operator guest
// route as they are.
func isGuestPolicyRoute(pattern string) bool {
	switch pattern {
	case "POST /v1/join/guest", "POST /v1/invites/redeem-guest", "GET /v1/join/settings", "PUT /v1/join/settings",
		"GET /v1/guests", "POST /v1/guests/{id}/end", "POST /v1/guests/end", "POST /v1/guests/{id}/conversion-permit":
		return true
	}
	return false
}

func routeMethod(pattern string) string {
	if method, _, ok := strings.Cut(pattern, " "); ok {
		return method
	}
	return http.MethodPost
}

// TestRouteTable ties the route table to the expectations above and to the mux
// registration, so a route added without an authentication decision fails.
func TestRouteTable(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)

	var got []string
	for _, rt := range f.srv.routes {
		got = append(got, rt.pattern)
		exp, ok := routeExpectations[rt.pattern]
		if !ok {
			t.Errorf("route %q has no entry in routeExpectations: decide its authentication class", rt.pattern)
			continue
		}
		want := map[routeClass]access{classExempt: accessExempt, classAuth: accessAuthenticated, classOp: accessOperator, classSelf: accessOperatorOrSelf, classOwner: accessOwner, classManager: accessChannelManager}[exp.class]
		if rt.access != want {
			t.Errorf("route %q access = %d, expectation says %s", rt.pattern, rt.access, exp.class)
		}
	}
	var want []string
	for p := range routeExpectations {
		want = append(want, p)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("route table and routeExpectations differ\ntable:\n%s\nexpectations:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// Nothing may register on the mux except through the table.
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(src), "mux.Handle"); n != 1 {
		t.Errorf("server.go has %d mux.Handle* calls, want exactly 1 (the loop over the route table)", n)
	}
	if n := strings.Count(string(src), "http.NewServeMux"); n != 1 {
		t.Errorf("server.go has %d http.NewServeMux calls, want 1", n)
	}
}

// TestRoutesRequireAuth walks the real route table under AuthRequired: every
// route not on the explicit exempt list answers 401 without a credential.
func TestRoutesRequireAuth(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	exemptList := map[string]bool{"GET /healthz": true, "POST /v1/hooks/{token}": true, "/mcp": true,
		"POST /v1/invites/preview": true, "POST /v1/invites/redeem": true, "POST /v2/invites/redeem": true,
		"GET /v1/join": true, "POST /v1/join/guest": true, "POST /v1/invites/redeem-guest": true, "POST /v1/join/account": true,
		"POST /v1/login": true, "POST /v1/password/reset": true,
		"POST /v1/sign-in-links/preview": true, "POST /v1/sign-in-links/redeem": true,
		"POST /v1/login/totp": true, "POST /v1/setup/redeem": true}

	for _, rt := range f.srv.routes {
		exp := routeExpectations[rt.pattern]
		t.Run(rt.pattern, func(t *testing.T) {
			rec := f.do(t, routeMethod(rt.pattern), exp.path, "", exp.body)
			if exemptList[rt.pattern] {
				if rec.Header().Get("WWW-Authenticate") == `Bearer realm="conch"` {
					t.Errorf("exempt route answered with the conch 401: %d %s", rec.Code, rec.Body)
				}
				if rec.Code != exp.allowed {
					t.Errorf("exempt route status = %d, want %d", rec.Code, exp.allowed)
				}
				return
			}
			assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="conch"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
		})
	}

	// Unmatched paths and methods are not exempt either: they fail closed.
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/nope"}, {"DELETE", "/healthz"}, {"GET", "/"}, {"GET", "/v0/../v1/whoami"},
	} {
		rec := f.do(t, c.method, c.path, "", "")
		assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
	}
}

// TestRoleMatrix sends each route as operator, member and nobody under
// AuthRequired and checks the status of each.
func TestRoleMatrix(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	for _, rt := range f.srv.routes {
		exp := routeExpectations[rt.pattern]
		if exp.class == classExempt {
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			method := routeMethod(rt.pattern)
			memberWant := exp.allowed
			if exp.class == classOp || exp.class == classSelf || exp.class == classOwner || exp.class == classManager {
				memberWant = http.StatusForbidden
			}
			if rt.pattern == "PUT /v1/channels/{id}/name" {
				memberWant = exp.allowed // a non-member cannot discover the channel id
			}
			// The approval REST routes are the human surface: an agent
			// credential is refused there and uses MCP instead (issue #79).
			agentWant := memberWant
			if rt.pattern == "PUT /v1/channels/{id}/name" {
				agentWant = http.StatusForbidden // kind is refused before channel lookup
			}
			if strings.Contains(rt.pattern, "/v1/approvals") || strings.Contains(rt.pattern, "/voice/session") || strings.Contains(rt.pattern, "/voice/transmit") || strings.Contains(rt.pattern, "/voice/share") || strings.Contains(rt.pattern, "/voice/companion") || rt.pattern == "GET /v1/channels/{channel}/voice" || strings.Contains(rt.pattern, "/v1/voice/occupancy") || isFileRoute(rt.pattern) {
				agentWant = http.StatusForbidden
			}
			// Runs are started by agents only (issue #266): the humans get the
			// 403 above, the agent gets as far as the session, which does not
			// exist.
			if rt.pattern == "POST /v1/sessions/{id}/runs" {
				agentWant = http.StatusNotFound
			}
			// Interventions are requested by humans only (issue #287): an
			// agent is refused before any run is looked at.
			if rt.pattern == "POST /v1/runs/{id}/interventions" {
				agentWant = http.StatusForbidden
			}
			// Read state is for people (issue #351): an agent is refused before
			// any channel is looked at.
			if strings.HasSuffix(rt.pattern, "/read-state") {
				agentWant = http.StatusForbidden
			}
			// The moderation routes of reactions (issue #765) administer a
			// channel: an agent is refused by the class, before any lookup.
			if exp.class == classManager && isReactionRoute(rt.pattern) {
				agentWant = http.StatusForbidden
			}
			// Invites are for people (issue #437), and so are problem
			// reports (issue #446) and message edits (issue #845: message
			// 9999 does not exist, so a person gets message_not_found, and an
			// agent is refused before any lookup).
			if isInviteRoute(rt.pattern) || rt.pattern == "POST /v1/reports" || rt.pattern == "POST /v1/channels" || isMessageEditRoute(rt.pattern) {
				agentWant = http.StatusForbidden
			}
			// Signing out (issue #439) succeeds for any member and revokes
			// the credential it was sent with, so it runs on a fixture of its
			// own and the other routes' callers stay signed in.
			g := f
			if rt.pattern == "DELETE /v1/credentials/current" {
				memberWant, agentWant = http.StatusNoContent, http.StatusNoContent
				g = newAuthFixture(t, AuthRequired)
			}
			// Device links (issue #712) are a member's own: an agent and an
			// operator are refused before anything is read.
			operatorWant := exp.allowed
			if isDeviceLinkRoute(rt.pattern) {
				agentWant, operatorWant = http.StatusForbidden, http.StatusForbidden
			}
			cases := []struct {
				who   string
				token string
				want  int
			}{
				{"unauthenticated", "", http.StatusUnauthorized},
				{"member human", g.aliceTok, memberWant},
				{"member agent", g.botTok, agentWant},
				{"operator", g.rootTok, operatorWant},
			}
			for _, c := range cases {
				rec := g.do(t, method, exp.path, c.token, exp.body)
				if rec.Code != c.want {
					t.Errorf("%s: status = %d, want %d; body %s", c.who, rec.Code, c.want, rec.Body)
				}
			}
		})
	}
}

// isReactionRoute reports whether pattern is one of the seven reaction routes
// (issue #765).
func isReactionRoute(pattern string) bool {
	return strings.Contains(pattern, "/v2/channels/{channel}/") && (strings.Contains(pattern, "/reactions") || strings.Contains(pattern, "/reactors"))
}

// isMessageEditRoute reports whether pattern is one of the two message edit
// routes (issue #845).
func isMessageEditRoute(pattern string) bool {
	return pattern == "PUT /v2/channels/{channel}/messages/{message_id}/body" ||
		pattern == "GET /v2/channels/{channel}/messages/{message_id}/revisions"
}

// TestAuthOffUnchanged runs the route table with AuthOff and no credential:
// the statuses are what the handlers gave before authentication existed, no
// route reads a credential, and no role check applies.
func TestAuthOffUnchanged(t *testing.T) {
	for _, mode := range []AuthMode{AuthOff} {
		f := newAuthFixture(t, mode)
		for _, rt := range f.srv.routes {
			exp := routeExpectations[rt.pattern]
			t.Run(fmt.Sprintf("%q/%s", mode, rt.pattern), func(t *testing.T) {
				want := exp.allowed
				if rt.pattern == "GET /v1/whoami" || rt.pattern == "DELETE /v1/credentials/current" ||
					rt.pattern == "GET /v1/credentials/mine" || rt.pattern == "DELETE /v1/credentials/mine/{id}" {
					want = http.StatusUnauthorized // no caller to report, nor a credential of its own to list or revoke
				}
				if strings.Contains(rt.pattern, "/voice/session") || strings.Contains(rt.pattern, "/voice/transmit") || strings.Contains(rt.pattern, "/voice/share") || strings.Contains(rt.pattern, "/voice/companion") || rt.pattern == "GET /v1/channels/{channel}/voice" || strings.Contains(rt.pattern, "/v1/voice/occupancy") {
					want = http.StatusBadRequest // voice_requires_auth: voice is never anonymous
				}
				if rt.pattern == "PUT /v1/channels/{id}/name" {
					want = http.StatusBadRequest // AuthOff skips the guard and reaches body validation
				}
				if isFileRoute(rt.pattern) {
					want = http.StatusBadRequest // files_require_auth: files are never anonymous
				}
				if strings.Contains(rt.pattern, "/v1/sessions") || strings.Contains(rt.pattern, "/v1/runs") || strings.Contains(rt.pattern, "/v1/interventions") {
					want = http.StatusBadRequest // sessions_require_auth: a session, and so a run, is bound to a credential
				}
				if strings.HasSuffix(rt.pattern, "/read-state") {
					want = http.StatusBadRequest // read_state_require_auth: a mark belongs to a principal
				}
				if isReactionRoute(rt.pattern) {
					want = http.StatusBadRequest // reactions_require_auth: a reaction belongs to a principal
				}
				if rt.pattern == "POST /v1/ws-tickets" {
					want = http.StatusBadRequest // tickets_require_auth: a ticket is bound to a credential
				}
				if isMessageEditRoute(rt.pattern) {
					want = http.StatusBadRequest // message_edit_requires_auth: no one to be the author
				}
				if isInviteRoute(rt.pattern) {
					want = http.StatusUnauthorized // like whoami: no one to make or own an invite
				}
				if rt.pattern == "POST /v1/reports" {
					want = http.StatusUnauthorized // like whoami: no one to say who reported
				}
				if rt.pattern == "POST /v1/principals/hide" {
					want = http.StatusUnauthorized // like the bulk disables: no caller to leave out
				}
				if isGuestPolicyRoute(rt.pattern) {
					want = http.StatusForbidden // accounts_require_auth: no guest exists, nor anyone to admit one (design §7.4)
				}
				if isDeviceLinkRoute(rt.pattern) {
					want = http.StatusForbidden // accounts_require_auth: no account to add a device to
				}
				rec := f.do(t, routeMethod(rt.pattern), exp.path, "", exp.body)
				if rec.Code != want {
					t.Errorf("status = %d, want %d; body %s", rec.Code, want, rec.Body)
				}
				// A garbage credential is not read at all under AuthOff.
				rec = f.do(t, routeMethod(rt.pattern), exp.path, "not-a-credential", exp.body)
				if rec.Code != want {
					t.Errorf("with garbage credential: status = %d, want %d", rec.Code, want)
				}
			})
		}
		// Bootstrap writes 3 audit events (principal_created among them,
		// issue #445), alice's and bot's credentials 1 each, the fixture's
		// three member_added events, and the fixture agent's
		// manifest_created; no request may add to that (in particular no
		// access_denied).
		if n := len(f.audit(t)); n != 9 {
			t.Errorf("mode %q: audit events = %d, want 9", mode, n)
		}
	}
}

func TestParseAuthMode(t *testing.T) {
	tests := []struct {
		in      string
		want    AuthMode
		wantErr bool
	}{
		{"off", AuthOff, false},
		{"required", AuthRequired, false},
		{"", "", true},
		{"on", "", true},
		{"Required", "", true},
		{"true", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseAuthMode(tt.in)
			if (err != nil) != tt.wantErr || got != tt.want {
				t.Errorf("ParseAuthMode(%q) = %q, %v", tt.in, got, err)
			}
		})
	}
}

// An unrecognized mode that bypasses ParseAuthMode must not open the server.
func TestUnknownAuthModeFailsClosed(t *testing.T) {
	f := newAuthFixture(t, AuthMode("bogus"))
	assertErrorBody(t, f.do(t, "GET", "/v1/channels", "", ""), http.StatusUnauthorized, "unauthenticated")
}

func TestCredentialFailureModes(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()

	revoked, revokedTok := mustCredential(t, f, f.alice.ID, nil)
	if err := f.srv.store.RevokeCredential(ctx, "system", revoked.ID); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(150 * time.Millisecond)
	_, expiredTok := mustCredential(t, f, f.alice.ID, &soon)
	time.Sleep(300 * time.Millisecond)
	unknownTok := schema.CredentialTokenPrefix + strings.Repeat("A", 43)

	tests := []struct {
		name   string
		header string
		url    string
		cookie string
	}{
		{"missing header", "", "/v1/whoami", ""},
		{"wrong scheme", "Basic " + f.aliceTok, "/v1/whoami", ""},
		{"token without scheme", f.aliceTok, "/v1/whoami", ""},
		{"empty bearer", "Bearer ", "/v1/whoami", ""},
		{"malformed token", "Bearer hello", "/v1/whoami", ""},
		{"truncated token", "Bearer " + f.aliceTok[:20], "/v1/whoami", ""},
		{"padded token", "Bearer " + f.aliceTok + " x", "/v1/whoami", ""},
		{"unknown token", "Bearer " + unknownTok, "/v1/whoami", ""},
		{"expired token", "Bearer " + expiredTok, "/v1/whoami", ""},
		{"revoked token", "Bearer " + revokedTok, "/v1/whoami", ""},
		{"valid token in query only", "", "/v1/whoami?token=" + f.aliceTok + "&access_token=" + f.aliceTok, ""},
		{"valid token in cookie only", "", "/v1/whoami", "conch_token=" + f.aliceTok},
	}
	var first string
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.url, nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			if tt.cookie != "" {
				req.Header.Set("Cookie", tt.cookie)
			}
			rec := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(rec, req)
			assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="conch"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
			if first == "" {
				first = rec.Body.String()
			} else if rec.Body.String() != first {
				t.Errorf("body differs from the first failure mode:\n got %s\nwant %s", rec.Body, first)
			}
		})
	}

	// The valid token works in the header, with any scheme case.
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req := httptest.NewRequest("GET", "/v1/whoami", nil)
		req.Header.Set("Authorization", scheme+" "+f.aliceTok)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("scheme %q: status = %d", scheme, rec.Code)
		}
	}
	if n := countAction(f.audit(t), "access_denied"); n != 0 {
		t.Errorf("401s wrote %d access_denied audit events, want 0", n)
	}
}

// TestMiddlewareNeverAnswersAuthFailed pins that the authentication
// middleware answers a dead credential on every authenticated route with
// unauthenticated, never auth_failed (#814). Several authenticated routes
// (the password change, the re-authentication, the second factor) answer
// 401 auth_failed for a wrong secret, and the browser and the CLI tell that
// from "signed out" by the code alone: the check is sound only while no
// credential failure ever carries auth_failed.
func TestMiddlewareNeverAnswersAuthFailed(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	ctx := context.Background()

	revoked, revokedTok := mustCredential(t, f, f.alice.ID, nil)
	if err := f.srv.store.RevokeCredential(ctx, "system", revoked.ID); err != nil {
		t.Fatal(err)
	}
	soon := time.Now().Add(150 * time.Millisecond)
	_, expiredTok := mustCredential(t, f, f.alice.ID, &soon)
	// A generation bump ends every credential of the principal's, so the
	// stale one belongs to a principal of its own; it is not revoked.
	carol, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "carol")
	if err != nil {
		t.Fatal(err)
	}
	_, staleTok := mustCredential(t, f, carol.ID, nil)
	if _, err := f.srv.store.BumpAuthGeneration(ctx, "system", carol.ID, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A disabled principal's credential, likewise of a principal of its own.
	dave, err := f.srv.store.CreatePrincipal(ctx, store.PrincipalHuman, "dave")
	if err != nil {
		t.Fatal(err)
	}
	_, disabledTok := mustCredential(t, f, dave.ID, nil)
	if _, err := f.srv.store.DisablePrincipal(ctx, "system", dave.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)

	creds := []struct {
		name   string
		header string
		query  string
		ticket bool // only on a route that accepts ?ticket=
	}{
		{"no credential", "", "", false},
		{"an expired credential", "Bearer " + expiredTok, "", false},
		{"a revoked credential", "Bearer " + revokedTok, "", false},
		{"a credential of a past generation", "Bearer " + staleTok, "", false},
		{"a disabled principal's credential", "Bearer " + disabledTok, "", false},
		{"a wrong scheme", "Basic " + f.aliceTok, "", false},
		{"an empty bearer", "Bearer ", "", false},
		{"an unknown ticket", "", "ticket=" + strings.Repeat("A", 43), true},
	}
	routes := 0
	for _, rt := range f.srv.routes {
		if rt.access == accessExempt {
			continue
		}
		routes++
		exp := routeExpectations[rt.pattern]
		for _, c := range creds {
			if c.ticket && !rt.acceptsTicket {
				continue
			}
			t.Run(rt.pattern+"/"+c.name, func(t *testing.T) {
				path := exp.path
				if c.query != "" {
					sep := "?"
					if strings.Contains(path, "?") {
						sep = "&"
					}
					path += sep + c.query
				}
				req := httptest.NewRequest(routeMethod(rt.pattern), path, strings.NewReader(exp.body))
				if c.header != "" {
					req.Header.Set("Authorization", c.header)
				}
				rec := httptest.NewRecorder()
				f.srv.Handler().ServeHTTP(rec, req)
				if rec.Code == http.StatusUnauthorized && decodeBody[schema.Error](t, rec).Code == schema.ErrorCodeAuthFailed {
					t.Fatalf("%s was answered %s, the code for a wrong secret: %s", c.name, schema.ErrorCodeAuthFailed, rec.Body)
				}
				assertErrorBody(t, rec, http.StatusUnauthorized, "unauthenticated")
				if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="conch"` {
					t.Errorf("WWW-Authenticate = %q", got)
				}
			})
		}
	}
	if routes == 0 {
		t.Fatal("no authenticated route in the table")
	}
}

func mustCredential(t *testing.T, f *authFixture, principalID int64, exp *time.Time) (schema.CredentialV1, string) {
	t.Helper()
	c, tok, err := f.srv.store.CreateCredential(context.Background(), "system", principalID, "extra", exp)
	if err != nil {
		t.Fatal(err)
	}
	return c, tok
}

func countAction(events []store.AuditEvent, action string) int {
	n := 0
	for _, e := range events {
		if e.Action == action {
			n++
		}
	}
	return n
}

func TestMiddlewarePassesResolvedPrincipal(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	tests := []struct {
		name  string
		token string
		want  store.Principal
	}{
		{"operator", f.rootTok, f.root},
		{"member human", f.aliceTok, f.alice},
		{"member agent", f.botTok, f.bot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got store.Principal
			var seen bool
			marker := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, seen = callerFrom(r.Context())
			})
			req := httptest.NewRequest("GET", "/v1/whoami", nil)
			req.Header.Set("Authorization", "Bearer "+tt.token)
			f.srv.authMiddleware(marker).ServeHTTP(httptest.NewRecorder(), req)
			if !seen || got.ID != tt.want.ID || got.Role != tt.want.Role || got.Kind != tt.want.Kind {
				t.Errorf("principal = %+v (seen %v), want %+v", got, seen, tt.want)
			}
		})
	}
}

// A database failure while resolving a credential is a 500, never an
// authenticated request, and the downstream handler is not reached.
func TestResolveStoreFailureIs500(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	if err := f.srv.store.Close(); err != nil {
		t.Fatal(err)
	}
	reached := false
	marker := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	req := httptest.NewRequest("GET", "/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+f.aliceTok) // well-formed, so it reaches the database
	rec := httptest.NewRecorder()
	f.srv.authMiddleware(marker).ServeHTTP(rec, req)
	assertErrorBody(t, rec, http.StatusInternalServerError, "internal_error")
	if reached {
		t.Error("handler reached after a store failure")
	}
	if rec.Header().Get("WWW-Authenticate") != "" {
		t.Error("a 500 must not look like a credential rejection")
	}
}

func TestAuthorBinding(t *testing.T) {
	tests := []struct {
		name   string
		body   func(f *authFixture) string
		status int
		code   string
		author func(f *authFixture) int64
	}{
		{"absent", func(*authFixture) string { return `{"body":"hi"}` }, 201, "", func(f *authFixture) int64 { return f.alice.ID }},
		{"zero", func(*authFixture) string { return `{"author_id":0,"body":"hi"}` }, 201, "", func(f *authFixture) int64 { return f.alice.ID }},
		{"matching", func(f *authFixture) string { return fmt.Sprintf(`{"author_id":%d,"body":"hi"}`, f.alice.ID) }, 201, "", func(f *authFixture) int64 { return f.alice.ID }},
		{"mismatched", func(f *authFixture) string { return fmt.Sprintf(`{"author_id":%d,"body":"hi"}`, f.root.ID) }, 403, "author_mismatch", nil},
		{"negative", func(*authFixture) string { return `{"author_id":-1,"body":"hi"}` }, 403, "author_mismatch", nil},
	}
	for _, version := range []string{"v0", "v1"} {
		for _, tt := range tests {
			t.Run(version+"/"+tt.name, func(t *testing.T) {
				f := newAuthFixture(t, AuthRequired)
				rec := f.do(t, "POST", "/"+version+"/channels/general/messages", f.aliceTok, tt.body(f))
				if tt.code != "" {
					assertErrorBody(t, rec, tt.status, tt.code)
					msgs, err := f.srv.store.ListVisibleMessages(context.Background(), 1, store.ChannelWideOnly, 0, 10)
					if err != nil || len(msgs) != 0 {
						t.Errorf("mismatched post stored %d messages (%v)", len(msgs), err)
					}
					var denied []store.AuditEvent
					for _, e := range f.audit(t) {
						if e.Action == "access_denied" {
							denied = append(denied, e)
						}
					}
					wantSubject := "POST /" + version + "/channels/{channel}/messages"
					if len(denied) != 1 || denied[0].Actor != fmt.Sprintf("principal:%d", f.alice.ID) ||
						denied[0].Subject != wantSubject || denied[0].Detail != "author_mismatch" {
						t.Errorf("denial audit = %+v, want one event for alice on %q", denied, wantSubject)
					}
					return
				}
				if rec.Code != tt.status {
					t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
				}
				var author int64
				if version == "v0" {
					author = decodeBody[schema.PostMessageResponse](t, rec).Message.AuthorID
				} else {
					author = decodeBody[schema.PostMessageResponseV1](t, rec).Message.AuthorID
				}
				if author != tt.author(f) {
					t.Errorf("stored author = %d, want %d", author, tt.author(f))
				}
			})
		}
	}
}

// Under AuthOff the author is still taken from the body, as before.
func TestAuthorFromBodyWhenAuthOff(t *testing.T) {
	f := newAuthFixture(t, AuthOff)
	rec := f.do(t, "POST", "/v0/channels/general/messages", "", fmt.Sprintf(`{"author_id":%d,"body":"hi"}`, f.bot.ID))
	if rec.Code != 201 || decodeBody[schema.PostMessageResponse](t, rec).Message.AuthorID != f.bot.ID {
		t.Errorf("status %d body %s", rec.Code, rec.Body)
	}
	assertErrorBody(t, f.do(t, "POST", "/v0/channels/general/messages", "", `{"body":"hi"}`), 400, "invalid_request")
}

func TestWhoAmI(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	tests := []struct {
		name  string
		token string
		want  schema.WhoAmIResponseV1
	}{
		// An operator may always invite (issue #437); a member may not until
		// granted the right, and an agent never. People hold voice unless an
		// invite withheld it; an agent never does (issue #468). Each is
		// classed (issue #705); a guest's case is in guests_test.go. People
		// are told how they are authenticated (issue #708): neither has a
		// password yet, so neither has finished setting up.
		{"operator", f.rootTok, schema.WhoAmIResponseV1{ID: f.root.ID, Kind: schema.PrincipalHuman, Name: "root", Role: schema.RoleOperator, ServerVersion: "v1.2.3-test", MayInvite: true, VoiceAllowed: boolPtr(true), IdentityClass: schema.IdentityOperator,
			Auth: &schema.AuthStateV1{Scope: schema.CredentialScopeFull}, Owner: true}}, // the bootstrap operator is the owner (issue #790)
		{"member human", f.aliceTok, schema.WhoAmIResponseV1{ID: f.alice.ID, Kind: schema.PrincipalHuman, Name: "alice", Role: schema.RoleMember, ServerVersion: "v1.2.3-test", VoiceAllowed: boolPtr(true), IdentityClass: schema.IdentityMember,
			Auth: &schema.AuthStateV1{Scope: schema.CredentialScopeFull}}},
		{"member agent", f.botTok, schema.WhoAmIResponseV1{ID: f.bot.ID, Kind: schema.PrincipalAgent, Name: "bot", Role: schema.RoleMember, ServerVersion: "v1.2.3-test", VoiceAllowed: boolPtr(false), IdentityClass: schema.IdentityAgent}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, "GET", "/v1/whoami", tt.token, "")
			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := decodeBody[schema.WhoAmIResponseV1](t, rec); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("whoami = %s, want %s", rec.Body, inviteJSON(t, tt.want))
			}
		})
	}
}

// TestVersionAgrees checks that /healthz and whoami report the one version the
// server was configured with (issue #425).
func TestVersionAgrees(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	tests := []struct {
		name, path, token string
		version           func(t *testing.T, rec *httptest.ResponseRecorder) string
	}{
		{"healthz", "/healthz", "", func(t *testing.T, rec *httptest.ResponseRecorder) string {
			return decodeBody[schema.Health](t, rec).Version
		}},
		{"whoami", "/v1/whoami", f.aliceTok, func(t *testing.T, rec *httptest.ResponseRecorder) string {
			return decodeBody[schema.WhoAmIResponseV1](t, rec).ServerVersion
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := f.do(t, "GET", tt.path, tt.token, "")
			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := tt.version(t, rec); got != "v1.2.3-test" {
				t.Errorf("version = %q, want v1.2.3-test", got)
			}
		})
	}
}

// TestOperatorOnlyRoutesDeny checks each operator-only route actually denies a
// member with the forbidden error and records the denial, and that nothing was
// changed by the denied request.
func TestOperatorOnlyRoutesDeny(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	for _, rt := range f.srv.routes {
		exp := routeExpectations[rt.pattern]
		if exp.class != classOp && exp.class != classSelf {
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			before := countAction(f.audit(t), "access_denied")
			rec := f.do(t, routeMethod(rt.pattern), exp.path, f.aliceTok, exp.body)
			assertErrorBody(t, rec, http.StatusForbidden, "forbidden")
			var last store.AuditEvent
			for _, e := range f.audit(t) {
				if e.Action == "access_denied" {
					last = e
				}
			}
			if countAction(f.audit(t), "access_denied") != before+1 {
				t.Fatalf("access_denied events did not grow by one")
			}
			if last.Actor != fmt.Sprintf("principal:%d", f.alice.ID) || last.Subject != rt.pattern || last.Detail != "forbidden" {
				t.Errorf("audit = %+v, want actor alice, subject %q, detail forbidden", last, rt.pattern)
			}
		})
	}
	// A denied member cannot have minted anything.
	creds, err := f.srv.store.ListCredentials(context.Background(), f.root.ID)
	if err != nil || len(creds) != 1 {
		t.Errorf("operator credentials = %d (%v), want the one bootstrap credential", len(creds), err)
	}
}

func TestManifestOperatorOrSelf(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	other, err := f.srv.store.CreatePrincipal(context.Background(), store.PrincipalAgent, "other")
	if err != nil {
		t.Fatal(err)
	}
	_, otherTok, err := f.srv.store.CreateCredential(context.Background(), "system", other.ID, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	put := f.do(t, "PUT", fmt.Sprintf("/v1/principals/%d/manifest", f.bot.ID), f.rootTok,
		`{"display_name":"Bot","tier":"A","capabilities":[],"channels":[],"rate_limits":[]}`)
	if put.Code != 201 && put.Code != 200 {
		t.Fatalf("operator PUT manifest = %d %s", put.Code, put.Body)
	}
	path := fmt.Sprintf("/v1/principals/%d/manifest", f.bot.ID)
	tests := []struct {
		name   string
		token  string
		path   string
		status int
	}{
		{"operator reads agent's", f.rootTok, path, 200},
		{"agent reads own", f.botTok, path, 200},
		{"other agent denied", otherTok, path, 403},
		{"other agent denied unknown id", otherTok, "/v1/principals/9999/manifest", 403},
		{"human member denied", f.aliceTok, path, 403},
		{"agent cannot write own", f.botTok, path, 403},
		{"unauthenticated", "", path, 401},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := "GET"
			if strings.HasPrefix(tt.name, "agent cannot write") {
				method = "PUT"
			}
			rec := f.do(t, method, tt.path, tt.token, `{}`)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d; body %s", rec.Code, tt.status, rec.Body)
			}
		})
	}
}

func TestCreatePrincipalAlwaysMember(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	for _, body := range []string{
		`{"kind":"human","name":"carol"}`,
	} {
		rec := f.do(t, "POST", "/v0/principals", f.rootTok, body)
		if rec.Code != 201 {
			t.Fatalf("status = %d %s", rec.Code, rec.Body)
		}
		var raw map[string]map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatal(err)
		}
		if _, ok := raw["principal"]["role"]; ok {
			t.Error("PrincipalV0 wire shape gained a role field")
		}
		p, err := f.srv.store.PrincipalByID(context.Background(), decodeBody[schema.CreatePrincipalResponse](t, rec).Principal.ID)
		if err != nil || p.Role != store.RoleMember {
			t.Errorf("role = %q (%v), want member", p.Role, err)
		}
	}
	// A client-supplied role is an unknown field and is rejected.
	assertErrorBody(t, f.do(t, "POST", "/v0/principals", f.rootTok, `{"kind":"human","name":"dave","role":"operator"}`), 400, "invalid_request")
}

// Webhook ingest authenticates with its URL token only; an Authorization
// header on it is neither required nor interpreted.
func TestHookIngestExemptUnderRequired(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	rec := f.do(t, "POST", "/v1/hooks", f.rootTok, fmt.Sprintf(`{"channel":"general","principal":%d}`, f.bot.ID))
	if rec.Code != 201 {
		t.Fatalf("create hook = %d %s", rec.Code, rec.Body)
	}
	hook := decodeBody[schema.CreateHookResponse](t, rec).Token
	for _, bearer := range []string{"", f.aliceTok} {
		rec := f.do(t, "POST", "/v1/hooks/"+hook, bearer, `{"body":"from hook"}`)
		if rec.Code != 201 {
			t.Fatalf("ingest (bearer %v) = %d %s", bearer != "", rec.Code, rec.Body)
		}
		if got := decodeBody[schema.PostMessageResponseV1](t, rec).Message.AuthorID; got != f.bot.ID {
			t.Errorf("hook author = %d, want the hook's principal %d", got, f.bot.ID)
		}
	}
}

func TestWebSocketAuth(t *testing.T) {
	for _, version := range []string{"v0", "v1"} {
		t.Run(version, func(t *testing.T) {
			f := newAuthFixture(t, AuthRequired)
			base := wsTestServer(t, f.srv)
			url := base + "/" + version + "/ws?channel=general"
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			for _, tc := range []struct {
				name   string
				header http.Header
				url    string
			}{
				{"no credential", nil, url},
				{"bad credential", http.Header{"Authorization": {"Bearer nope"}}, url},
				{"token in query", nil, url + "&token=" + f.aliceTok},
			} {
				conn, resp, err := websocket.Dial(ctx, tc.url, &websocket.DialOptions{HTTPHeader: tc.header})
				if err == nil {
					_ = conn.CloseNow()
					t.Fatalf("%s: upgrade succeeded", tc.name)
				}
				if resp == nil || resp.StatusCode != http.StatusUnauthorized {
					t.Fatalf("%s: response = %v (%v), want 401", tc.name, resp, err)
				}
				if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="conch"` {
					t.Errorf("%s: WWW-Authenticate = %q", tc.name, got)
				}
				_ = resp.Body.Close()
			}

			// With a credential the upgrade succeeds and messages flow.
			conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{ //nolint:bodyclose // upgraded
				HTTPHeader: http.Header{"Authorization": {"Bearer " + f.aliceTok}},
			})
			if err != nil {
				t.Fatalf("authenticated dial: %v", err)
			}
			defer func() { _ = conn.CloseNow() }()
			httpBase := "http" + strings.TrimPrefix(base, "ws")
			req, _ := http.NewRequestWithContext(ctx, "POST", httpBase+"/"+version+"/channels/general/messages", strings.NewReader(`{"body":"hello"}`))
			req.Header.Set("Authorization", "Bearer "+f.aliceTok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != 201 {
				t.Fatalf("post status = %d", resp.StatusCode)
			}
			var m schema.MessageV0
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				t.Fatalf("read: %v", err)
			}
			if m.Body != "hello" || m.AuthorID != f.alice.ID {
				t.Errorf("message = %+v", m)
			}
		})
	}
}

// syncBuffer collects slog output for the redaction test; slog handlers
// serialize writes, and the test reads only after requests complete.
type syncBuffer struct {
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) { return b.buf.Write(p) }

// TestNoTokenLeaks drives failures and successes and asserts no token (or any
// 20+ character piece of one) appears in logs, response bodies, response
// headers other than a create response's own body, or audit events.
func TestNoTokenLeaks(t *testing.T) {
	var logs syncBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newAuthFixture(t, AuthRequired)
	unknown := schema.CredentialTokenPrefix + strings.Repeat("Z", 43)
	secrets := []string{f.rootTok, f.aliceTok, f.botTok, unknown, "tokenlike-garbage-value"}

	var responses []string
	record := func(rec *httptest.ResponseRecorder) {
		responses = append(responses, rec.Body.String())
		for k, v := range rec.Header() {
			responses = append(responses, k+": "+strings.Join(v, ","))
		}
	}
	for _, c := range []struct{ method, path, token, body string }{
		{"GET", "/v1/whoami", unknown, ""},
		{"GET", "/v1/whoami", "tokenlike-garbage-value", ""},
		{"GET", "/v1/whoami", "", ""},
		{"POST", "/v0/channels", f.aliceTok, `{"name":"x"}`},
		{"POST", "/v1/principals/1/credentials", f.aliceTok, `{"label":"x"}`},
		{"GET", "/v1/principals/1/manifest", f.botTok, ""},
		{"POST", "/v1/channels/general/messages", f.aliceTok, fmt.Sprintf(`{"author_id":%d,"body":"x"}`, f.root.ID)},
		{"GET", "/v1/whoami", f.aliceTok, ""},
		{"POST", "/v1/hooks/" + f.aliceTok, f.aliceTok, `{}`},
	} {
		record(f.do(t, c.method, c.path, c.token, c.body))
	}
	// A store failure path too.
	_ = f.srv.store.Close() // deliberately breaking the store
	record(f.do(t, "GET", "/v1/whoami", f.aliceTok, ""))

	haystacks := map[string]string{"logs": logs.buf.String(), "responses": strings.Join(responses, "\n")}
	// Audit events are covered by TestNoTokenInAuditEvents.
	for name, hay := range haystacks {
		for _, s := range secrets {
			if strings.Contains(hay, s) {
				t.Errorf("%s contain a token", name)
			}
			for i := 0; i+20 <= len(s) && len(s) > 20; i += 10 {
				if strings.Contains(hay, s[i:i+20]) {
					t.Errorf("%s contain a fragment of a token", name)
				}
			}
		}
	}
}

// TestNoTokenInAuditEvents reads the audit log after denials and failures.
func TestNoTokenInAuditEvents(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	f.do(t, "POST", "/v0/channels", f.aliceTok, `{"name":"x"}`)
	f.do(t, "POST", "/v1/channels/general/messages", f.aliceTok, fmt.Sprintf(`{"author_id":%d,"body":"x"}`, f.root.ID))
	f.do(t, "GET", "/v1/whoami", "conch_"+strings.Repeat("Q", 43), "")
	var all strings.Builder
	for _, e := range f.audit(t) {
		fmt.Fprintf(&all, "%s|%s|%s|%s\n", e.Actor, e.Action, e.Subject, e.Detail)
	}
	for _, s := range []string{f.rootTok, f.aliceTok, f.botTok, "QQQQQQQQQQ", hashOf(f.aliceTok)} {
		if strings.Contains(all.String(), s) {
			t.Errorf("audit log contains secret material %.8s...", s)
		}
	}
}

func hashOf(token string) string { return sha256Hex(token) }

// Requests that try to reach a protected handler through path or method
// tricks must all get the standard 401 under AuthRequired. This pins the
// middleware's exemption lookup to the mux's own matching.
func TestMuxEdgeCasesRequireAuth(t *testing.T) {
	f := newAuthFixture(t, AuthRequired)
	for _, c := range []struct{ method, target string }{
		{"GET", "//v1/whoami"},
		{"GET", "/v1//whoami"},
		{"GET", "/v1%2Fwhoami"},
		{"GET", "/healthz/../v1/whoami"},
		{"GET", "/mcp/../v1/whoami"},
		{"GET", "/v1/whoami/"},
		{"GET", "/mcp/"},
		{"GET", "/mcp/v1/whoami"},
		{"GET", "/healthz/"},
		{"HEAD", "/v1/whoami"},
		{"OPTIONS", "/v1/channels"},
		{"POST", "/healthz"},
		{"GET", "/v1/whoami?token=anything"},
		{"GET", "/v1/whoami?access_token=anything"},
	} {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			rec := f.do(t, c.method, c.target, "", "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (body %s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="conch"` {
				t.Errorf("WWW-Authenticate = %q", got)
			}
		})
	}
}

// Running without authentication is a deliberate, loud choice: the server
// says so once at startup, and says nothing of the kind when it is required —
// which is also what an unset mode means.
func TestAuthOffWarnsAtStartup(t *testing.T) {
	for _, tt := range []struct {
		mode AuthMode
		want int
	}{{AuthOff, 1}, {"", 0}, {AuthRequired, 0}} {
		logs := captureLogs(t)
		newTestServerWithConfig(t, Config{AuthMode: tt.mode})
		if got := strings.Count(logs.buf.String(), "authentication is OFF"); got != tt.want {
			t.Errorf("mode %q: warnings logged = %d, want %d", tt.mode, got, tt.want)
		}
	}
}

// A Config that never set AuthMode requires credentials (issue #92): the
// zero value must not be an open server, and neither may a mode nobody
// recognises. Only an explicit AuthOff opens it.
func TestUnsetAuthModeRequiresCredentials(t *testing.T) {
	for _, tt := range []struct {
		mode AuthMode
		want int
	}{
		{"", http.StatusUnauthorized},
		{"Off", http.StatusUnauthorized},
		{"none", http.StatusUnauthorized},
		{AuthRequired, http.StatusUnauthorized},
		{AuthOff, http.StatusOK},
	} {
		t.Run(fmt.Sprintf("%q", tt.mode), func(t *testing.T) {
			srv := newTestServerWithConfig(t, Config{AuthMode: tt.mode})
			rec := httptest.NewRecorder()
			srv.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/channels", nil))
			if rec.Code != tt.want {
				t.Errorf("GET /v1/channels with no credential = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}
