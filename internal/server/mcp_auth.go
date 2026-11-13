package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Authentication for the /mcp endpoint (issues #97, #79). This file is the
// only part of the MCP front-end that touches the store directly; the tools in
// mcp.go reach data only through agentScope (authz.go).

// mcpIdentity is who an /mcp request authenticated as, and by which means.
type mcpIdentity struct {
	principalID int64
	// credentialID is the stored credential the request presented, or 0 when
	// it authenticated through a deprecated static --mcp-token mapping.
	credentialID int64
	// credential is what the middleware would have learned of that
	// credential, for the admin door's dispatch (issue #1331); zero for a
	// static mapping.
	credential store.RequestCredential
}

// mcpIdentityKey carries the authenticated identity from the /mcp wrapper to
// the per-request server factory, so a request is authenticated exactly once.
type mcpIdentityKey struct{}

func (s *Server) mcpHandler() http.Handler {
	if n := len(s.cfg.MCPBearerTokens); n > 0 {
		slog.Warn("mcp: static --mcp-token mappings are deprecated; issue credentials with POST /v1/principals/{id}/credentials instead", "mappings", n)
	}
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		identity, ok := r.Context().Value(mcpIdentityKey{}).(mcpIdentity)
		if !ok {
			return nil
		}
		return s.mcpServerFor(identity)
	}, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true,
		// The library refuses a non-loopback Host on a loopback listener, as
		// a guard against DNS rebinding (issue #1479). A deployed conchd
		// listens on loopback behind a proxy that keeps the public Host, so
		// that guard refused every agent. Rebinding lets a web page reach a
		// local server without credentials; every request here has already
		// presented a bearer credential (below, in every auth mode), which
		// such a page does not hold: /mcp honours no cookie or session, and
		// no CORS header lets a page send one. Revisit this if either changes.
		DisableLocalhostProtection: true,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity, ok := s.authenticateMCP(r)
		if !ok {
			// One response for every failure: missing, malformed, unknown,
			// expired, revoked, wrong principal kind, disabled, or a store error.
			w.Header().Set("WWW-Authenticate", `Bearer realm="conch-mcp"`)
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		// A request is held to the size a REST post may be (issue #237).
		// Without a bound an agent could store a message of any size, which
		// the server would then hold several times over and send to
		// subscribers as a frame none of them will read (clients read at most
		// schema.MaxMessageFrameBytes, sized for a request within this limit).
		// It is read here, after authentication and never before, so that the
		// refusal is one stable answer instead of whatever the MCP library
		// makes of a body that ends early. One caller may send more, for the
		// images of a post_message (issue #1798); readMCPBody has the rules.
		if r.Method == http.MethodPost && r.Body != nil {
			body, release, ok := s.readMCPBody(w, r, identity)
			if !ok {
				return
			}
			// A large request keeps its place among the few the server takes
			// at once until the tool has answered: the MCP library decodes
			// the body several times over, and that is the memory the place
			// stands for.
			defer release()
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), mcpIdentityKey{}, identity)))
	})
}

const (
	// mcpLargeRequestSlots is how many /mcp requests over
	// schema.MaxPostRequestBytes the server handles at once, whoever sends
	// them. One such request is held in memory several times over while it
	// is decoded (measured: 150 to 400 MiB of heap for one at the default
	// upload limit), so without a bound a handful sent together by one
	// granted agent, or with one stolen token, could have a small conchd
	// killed for memory. Requests within the ordinary bound are not counted
	// and never wait.
	mcpLargeRequestSlots = 2
	// mcpLargeRequestRetry is the Retry-After of a large request refused
	// because every place is taken: a place is held for about as long as
	// one image post takes.
	mcpLargeRequestRetry = 2 * time.Second
	// mcpLargeRequestReadTimeout is how long the body of a large request may
	// take to arrive. It holds a place while it does, and the server sets no
	// read timeout of its own, so without one two slow senders could keep
	// every other agent's images out for as long as they liked.
	mcpLargeRequestReadTimeout = 2 * time.Minute
)

// errMCPBusy answers a large /mcp request that arrives while the server is
// already handling as many as it takes at once.
var errMCPBusy = &schema.Error{Code: schema.ErrorCodeServerBusy,
	Message:           "the server is handling as many large requests as it takes at once; retry shortly",
	RetryAfterSeconds: int(mcpLargeRequestRetry / time.Second)}

func writeMCPRequestTooLarge(w http.ResponseWriter) {
	writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the maximum size")
}

// takeMCPLargeSlot takes one of the places for a large /mcp request, without
// waiting. It reports false when all are taken.
func (s *Server) takeMCPLargeSlot() bool {
	select {
	case s.mcpLargeSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// readMCPBody reads the body of an authenticated /mcp request and returns it
// with a function to call when the request has been answered. When it reports
// false it has written the response.
//
// Every request is held to schema.MaxPostRequestBytes, read as it always was,
// with one exception (issue #1798): an agent whose manifest grants the larger
// limit (mcpRequestLimit) may send a post_message whose attachments make it
// larger. The limit is chosen after the caller is known, and the manifest is
// read only for a request that says it is larger than the bound, or does not
// say. For that agent:
//
//   - a request that declares more than the larger limit is refused (413)
//     before any of it is read;
//   - the server handles only mcpLargeRequestSlots requests over the bound at
//     once. One more is refused before its body is read: all of it when the
//     length is declared, and all but the first MaxPostRequestBytes+1 bytes
//     when it is not (those are read to learn that it is over the bound; a
//     request without a declared length that turns out to be small is an
//     ordinary one and takes no place). The refusal is 503 with Retry-After
//     and the usual error body, not 429: the limit is the server's, shared by
//     every agent, so it says nothing about the caller's own rate, is not the
//     caller's to wait out by its own budget, and is not audited as the
//     caller's rate limit;
//   - a body over the bound must be what the larger limit is for
//     (mcpOversizeIsAttachments), or it is refused (413) before the MCP
//     library sees it.
func (s *Server) readMCPBody(w http.ResponseWriter, r *http.Request, identity mcpIdentity) ([]byte, func(), bool) {
	base := int64(schema.MaxPostRequestBytes)
	limit := base
	if r.ContentLength > base || r.ContentLength < 0 {
		limit = s.mcpRequestLimit(r.Context(), identity)
	}
	release := func() {}
	tooLarge := func() { writeMCPRequestTooLarge(w) }
	unreadable := func() {
		writeError(w, http.StatusBadRequest, "invalid_request", "request body could not be read")
	}
	// refuse answers a request that goes no further, and gives its place
	// back if it had taken one.
	refuse := func(answer func()) {
		release()
		answer()
	}
	var head []byte
	if limit > base {
		// A refusal sent without reading the rest of the body closes the
		// connection instead of waiting for what it has refused.
		unread := func(answer func()) {
			w.Header().Set("Connection", "close")
			refuse(answer)
		}
		if r.ContentLength > limit {
			unread(tooLarge)
			return nil, nil, false
		}
		if r.ContentLength < 0 {
			var err error
			if head, err = io.ReadAll(io.LimitReader(r.Body, base+1)); err != nil {
				refuse(unreadable)
				return nil, nil, false
			}
		}
		if r.ContentLength > base || int64(len(head)) > base {
			if !s.takeMCPLargeSlot() {
				unread(func() {
					w.Header().Set("Retry-After", strconv.Itoa(errMCPBusy.RetryAfterSeconds))
					writeJSON(w, http.StatusServiceUnavailable, errMCPBusy)
				})
				return nil, nil, false
			}
			release = func() { <-s.mcpLargeSlots }
			// Not every ResponseWriter can set a deadline (a test's cannot);
			// a real connection can, and that is the one a slow sender is on.
			rc := http.NewResponseController(w)
			if rc.SetReadDeadline(time.Now().Add(mcpLargeRequestReadTimeout)) == nil {
				defer func() { _ = rc.SetReadDeadline(time.Time{}) }()
			}
		}
	}
	rest, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit-int64(len(head))))
	if err != nil {
		var over *http.MaxBytesError
		if errors.As(err, &over) {
			refuse(tooLarge)
		} else {
			refuse(unreadable)
		}
		return nil, nil, false
	}
	body := rest
	if len(head) > 0 {
		body = append(head, rest...)
	}
	if int64(len(body)) > base && !mcpOversizeIsAttachments(body, s.cfg.MaxUploadBytes) {
		refuse(tooLarge)
		return nil, nil, false
	}
	return body, release, true
}

// mcpRequestLimit is the most bytes of an /mcp request body the server reads
// from identity: schema.MaxPostRequestBytes, and, for an agent whose manifest
// could let it post an image somewhere, on a server that takes agents'
// uploads, that plus the base64 length of one upload (issue #1798). The
// attachments of one call share one MaxUploadBytes in total, so one call can
// never carry more than one upload by a person can.
//
// "Could post an image somewhere" is the capability files.upload and the
// permission attach in at least one channel. The whole grant cannot be decided
// here: which channel the call is for is in the body, and so is not known
// until it is read, and membership goes with the channel. The tool decides the
// grant in full, and audits a refusal; nothing is audited here. An agent with
// the capability and no channel to attach in can post no image anywhere, so
// it gets no larger limit to fill.
//
// Everything that is not a clear grant is the smaller limit: no manifest, an
// invalid one, a store failure.
func (s *Server) mcpRequestLimit(ctx context.Context, identity mcpIdentity) int64 {
	base := int64(schema.MaxPostRequestBytes)
	if !s.agentUploadsEnabled() {
		return base
	}
	m, reason, err := s.agentManifest(ctx, identity.principalID)
	if err != nil {
		slog.ErrorContext(ctx, "mcp: load manifest for the request limit failed", "principal", identity.principalID, "error", err)
		return base
	}
	if reason != "" || !m.Allows(schema.CapabilityFilesUpload) || !grantsAttachSomewhere(m) {
		return base
	}
	return base + base64Len(s.cfg.MaxUploadBytes)
}

// grantsAttachSomewhere reports whether m grants the channel permission attach
// in at least one channel.
func grantsAttachSomewhere(m schema.AgentManifestV1) bool {
	for _, g := range m.Channels {
		if m.AllowsChannel(g.ChannelID, schema.ChannelPermissionAttach) {
			return true
		}
	}
	return false
}

// base64Len is the length of n bytes in padded base64, held to a value that
// cannot overflow whatever n is configured.
func base64Len(n int64) int64 {
	const most = 1 << 60
	if n > most/2 {
		return most
	}
	return (n + 2) / 3 * 4
}

// mcpOversizeIsAttachments reports whether body, a request larger than
// schema.MaxPostRequestBytes, is one the larger limit is for: a single
// tools/call of post_message, in which
//
//   - there are at most schema.MaxAgentAttachments attachments;
//   - each data_base64 is a JSON string of padded standard base64 and nothing
//     else: no escape, no line break, no byte outside the alphabet;
//   - the decoded lengths of those strings come to at most maxUpload, what one
//     upload may be;
//   - everything else in the request is within schema.MaxPostRequestBytes.
//
// Anything else that large is refused as it always was, before the MCP library
// decodes it: another tool, a batch, a post whose text or payload is what
// makes it large, and a post whose "image data" is not data at all. So the
// larger limit is room for image bytes and for nothing else, and no message
// can be stored from more than MaxPostRequestBytes of request
// (schema.MaxMessageFrameBytes depends on it). The alphabet matters for
// memory: a string of bytes that are not text grows threefold when it is
// decoded, and the library decodes the request more than once.
//
// Why bytes let through here can never become a message's text. The argument
// is about where the bytes are, not about which of two repeated keys wins.
// Every byte this subtracts from the request is part of a JSON value that
// stands at params.arguments.attachments[i].data_base64, each value counted
// once (json.RawMessage holds the value as it stands in the request, not
// decoded, which could lengthen it). Every other byte of the request is held
// to MaxPostRequestBytes. The MCP library reads the same document, and a value
// at that position is to it an attachment's data or nothing: its decoder
// matches keys exactly and the tool's schema refuses a key it does not know,
// so it can read such a value as data_base64 of an attachment, or skip it, or
// refuse the call, and never as body or payload, which stand elsewhere.
//
// This holds however keys are repeated, and that matters, because the two
// decoders do not agree on a repeated key. encoding/json, used here, decodes
// a second "attachments" list into the elements of the first, so with
// "attachments":[{"data_base64":"..."}],"attachments":[{}] the first list's
// data is still counted; it also matches keys without regard to case. Both
// only ever make this count values at the position above that the library
// will go on to skip. They cannot make it count bytes that stand anywhere
// else. post_message bounds its text again for itself.
func mcpOversizeIsAttachments(body []byte, maxUpload int64) bool {
	var call struct {
		Method string `json:"method"`
		Params struct {
			Name      string `json:"name"`
			Arguments struct {
				Attachments []struct {
					Data json.RawMessage `json:"data_base64"`
				} `json:"attachments"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &call); err != nil {
		return false
	}
	if call.Method != "tools/call" || call.Params.Name != "post_message" {
		return false
	}
	attachments := call.Params.Arguments.Attachments
	if len(attachments) > schema.MaxAgentAttachments {
		return false
	}
	rest := len(body)
	var decoded int64
	for _, a := range attachments {
		n, ok := base64StringLen(a.Data)
		if !ok {
			return false
		}
		if decoded += n; decoded > maxUpload {
			return false
		}
		rest -= len(a.Data)
	}
	return rest <= schema.MaxPostRequestBytes
}

// base64StringLen reports how many bytes raw decodes to, when raw is a JSON
// string, as it stands in a request, that holds padded standard base64 (RFC
// 4648 section 4) and nothing else: a quote, groups of four characters of the
// alphabet with at most two "=" at the very end, and a quote. ok is false for
// anything else, an absent value included.
func base64StringLen(raw []byte) (n int64, ok bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return 0, false
	}
	text := raw[1 : len(raw)-1]
	if len(text)%4 != 0 {
		return 0, false
	}
	pad := 0
	for pad < 2 && pad < len(text) && text[len(text)-1-pad] == '=' {
		pad++
	}
	for _, c := range text[:len(text)-pad] {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '+', c == '/':
		default:
			return 0, false
		}
	}
	return int64(len(text)/4*3 - pad), true
}

// authenticateMCP resolves the request's bearer token to an agent principal.
// Stored credentials (issue #78) are tried first; the static --mcp-token map
// is a deprecated fallback. It never logs the token.
func (s *Server) authenticateMCP(r *http.Request) (mcpIdentity, bool) {
	raw := r.Header.Get("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		return mcpIdentity{}, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(raw, "Bearer "))
	if token == "" {
		return mcpIdentity{}, false
	}
	ctx := r.Context()

	principal, cred, err := s.store.ResolveFullCredential(ctx, token)
	switch {
	case err == nil:
		// MCP is the agent front-end: a human's credential is not accepted here.
		if principal.Kind != store.PrincipalAgent {
			return mcpIdentity{}, false
		}
		return mcpIdentity{principalID: principal.ID, credentialID: cred.ID, credential: cred}, true
	case !errors.Is(err, store.ErrCredentialInvalid):
		// The store failed. Fail closed rather than fall through to the map.
		slog.ErrorContext(ctx, "mcp: resolve credential failed", "error", err)
		return mcpIdentity{}, false
	}

	// A token shaped like a stored credential is never honoured through the
	// static map, so configuration cannot bring a revoked or expired
	// credential back to life.
	if strings.HasPrefix(token, schema.CredentialTokenPrefix) {
		return mcpIdentity{}, false
	}
	principalID, ok := s.cfg.MCPBearerTokens[token]
	if !ok || principalID <= 0 {
		return mcpIdentity{}, false
	}
	live, err := s.staticAgentUsable(ctx, principalID)
	if err != nil {
		slog.ErrorContext(ctx, "mcp: authenticate principal failed", "error", err)
		return mcpIdentity{}, false
	}
	if !live {
		return mcpIdentity{}, false
	}
	return mcpIdentity{principalID: principalID}, true
}

// staticAgentUsable reports whether a principal reached through a static
// --mcp-token mapping may use MCP: it must exist, be an agent, and not be
// disabled. A static mapping is configuration, not a credential row, so
// disabling the principal (issue #101) revokes nothing there; this is the
// explicit check that keeps a disabled agent out.
func (s *Server) staticAgentUsable(ctx context.Context, principalID int64) (bool, error) {
	p, err := s.store.PrincipalByID(ctx, principalID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return p.Kind == store.PrincipalAgent && p.DisabledAt == nil, nil
}

// mcpStillAuthenticated re-checks an identity that authenticated earlier. A
// long-running tool (await_decision) calls it on every poll, so that revoking
// or expiring the credential, or disabling the principal, ends the call
// instead of letting it run to its timeout.
func (s *Server) mcpStillAuthenticated(ctx context.Context, identity mcpIdentity) (bool, error) {
	if identity.credentialID > 0 {
		return s.store.CredentialLive(ctx, identity.credentialID)
	}
	return s.staticAgentUsable(ctx, identity.principalID)
}
