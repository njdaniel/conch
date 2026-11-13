package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Agents posting images (issue #1798).
//
// An agent may upload a file only under a grant, and the grant is three
// things, all of them: its manifest holds the capability files.upload; its
// manifest holds the channel permission attach in that channel; and it is a
// member of the channel. Without any one of them the answer is the refusal
// agents had before there was a grant. The grant is for uploading and nothing
// else: every route that fetches a file refuses an agent, granted or not
// (humansOnlyFiles), because posting an image needs no read.
//
// What a granted agent may upload is narrower than what a person may:
//
//   - images only: png, jpeg and webp. The declared media type must be one of
//     them and the first bytes of the file must be that type's signature. That
//     keeps an agent from storing anything at all under an image's name; it is
//     not a proof that the rest of the file is a well-formed image, and nothing
//     relies on one: a download is never rendered by the server (files.go).
//   - a budget: Config.AgentUploadsPerHour files an hour for each agent, one
//     token bucket per agent (the bucket of invite_limits.go). A file that was
//     not stored is not counted. The manifest's own rate limit for
//     files.upload, if it declares one, is spent as well, once per call, as
//     every capability's is (ratelimit.go).
//
// There are two ways in, and both end in the same code: the bytes go through
// stageBlob and place (files.go) to the same content-addressed path, and the
// row is made by the store's insertFileTx with the same file_uploaded audit
// event, whose actor is the agent.
//
//   - REST: POST /v2/channels/{channel}/files, as for a person
//     (handleUploadFile). The agent then attaches the file to a message it
//     posts, as a person does.
//   - MCP: the attachments argument of post_message, at most
//     schema.MaxAgentAttachments items. The server uploads them and posts one
//     message that carries them, all or nothing (agentScope.prepareUploads).
//
// The grant is asked for again wherever time has passed since it was last
// decided, so that taking it away takes effect at once:
//
//   - an upload over REST is decided before the body is read and again before
//     its row is made (handleUploadFile): the body can take a while;
//   - a file an agent uploaded earlier and has not attached is its own only
//     while it holds the grant. Every post by an agent that names existing
//     files in a conch.file_attachment.v1 payload needs the grant in that
//     channel at that moment (agentAttachRefusal), over MCP and over REST.
//     Without that, an agent whose grant was removed could go on publishing
//     what it had put by while it held it.

const (
	// DefaultAgentUploadsPerHour is the upload budget of each agent conchd
	// uses when none is set.
	DefaultAgentUploadsPerHour = 20

	// agentFilesRefusal is the message of the 403 an agent gets on a file
	// route: on the two that fetch, always; on the upload, without the grant.
	agentFilesRefusal = "agents cannot use files"

	// agentUploadLimitName is what the audit detail calls the upload budget.
	agentUploadLimitName = "agent_uploads_per_hour"

	// imageSignatureBytes is how many leading bytes sniffAgentImage looks at:
	// the longest of the three signatures (webp's).
	imageSignatureBytes = 12
)

var (
	errUploadsDisabled = &schema.Error{Code: "uploads_disabled", Message: "file uploads are disabled on this server"}
	// errAgentMediaType answers an agent's upload that is not one of the
	// three image types, or is not the type it declares.
	errAgentMediaType = &schema.Error{Code: schema.ErrorCodeUnsupportedMediaType,
		Message: "an agent may upload only image/png, image/jpeg or image/webp, and the bytes must be a file of the declared type"}
)

// sniffAgentImage reports which of the media types an agent may upload the
// bytes of head begin as, by the signature each format starts with, or "" for
// none of them. head is the first imageSignatureBytes of the file, or all of
// it when it is shorter.
func sniffAgentImage(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return schema.MediaTypePNG
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return schema.MediaTypeJPEG
	case len(head) >= imageSignatureBytes && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return schema.MediaTypeWebP
	default:
		return ""
	}
}

// agentUploadsEnabled reports whether this server takes uploads from agents
// at all: uploads are on, there is a place to put them, and the agents'
// budget is not zero.
func (s *Server) agentUploadsEnabled() bool {
	return s.cfg.MaxUploadBytes > 0 && s.filesDir() != "" && s.cfg.AgentUploadsPerHour > 0
}

// agentUploadLimiter is the upload budget: one token bucket per agent.
type agentUploadLimiter struct {
	mu      sync.Mutex
	limit   inviteLimit
	buckets bucketSet
}

func newAgentUploadLimiter(perHour int) *agentUploadLimiter {
	return &agentUploadLimiter{limit: inviteLimit{name: agentUploadLimitName, burst: perHour, window: time.Hour}}
}

// take spends n uploads of principalID's budget, or none when there are not n.
func (l *agentUploadLimiter) take(principalID int64, n int, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit.burst <= 0 {
		return false, l.limit.window
	}
	// Agents are made by operators, so one shared overflow bucket is enough.
	return l.buckets.bucket(l.limit, strconv.FormatInt(principalID, 10), inviteOverflowKey, now).takeN(l.limit, n, now)
}

// refund gives back n uploads taken by take, for files that were not stored.
func (l *agentUploadLimiter) refund(principalID int64, n int, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit.burst <= 0 {
		return
	}
	l.buckets.bucket(l.limit, strconv.FormatInt(principalID, 10), inviteOverflowKey, now).refundN(l.limit, n, now)
}

// setAgentUploadsPerHour replaces the agents' upload budget and forgets every
// bucket. For tests, before the server serves.
func (s *Server) setAgentUploadsPerHour(perHour int) {
	s.cfg.AgentUploadsPerHour = perHour
	s.agentUploadLimits = newAgentUploadLimiter(perHour)
}

// agentUploadGrant decides the grant for agent principalID to upload to the
// channel named channelName, afresh from the stored manifest and roster. It
// returns the manifest it decided on and the channel, and the reason the agent
// is refused, or "" when it is granted. Every refusal is audited under subject
// as access_denied, with the capability files.upload. A store failure is an
// error and not a grant.
//
// The order is the order of every agent gate (authz.go): the manifest's
// capability, which says nothing about any channel; then the channel and the
// agent's membership of it, where an unknown channel and one the agent is not
// in are one answer (denyNotMember); then the permission in that channel. The
// caller turns the reason into its own protocol's refusal, and must go on to
// spendAgentUploads before anything is stored.
func (s *Server) agentUploadGrant(ctx context.Context, principalID int64, subject, channelName string) (schema.AgentManifestV1, store.Channel, string, error) {
	return s.decideAgentUpload(ctx, principalID, subject, func() (store.Channel, error) {
		return s.store.ChannelByName(ctx, channelName)
	})
}

// agentUploadGrantIn is agentUploadGrant for a channel the caller has already
// resolved: a post that is past its own gates, or an upload whose body has
// arrived. The manifest and the membership are read afresh all the same.
func (s *Server) agentUploadGrantIn(ctx context.Context, principalID int64, subject string, channel store.Channel) (schema.AgentManifestV1, string, error) {
	m, _, reason, err := s.decideAgentUpload(ctx, principalID, subject, func() (store.Channel, error) { return channel, nil })
	return m, reason, err
}

// decideAgentUpload is the one decision behind agentUploadGrant and
// agentUploadGrantIn. find gives the channel, or store.ErrNotFound for none; it
// is called only for an agent whose manifest holds the capability.
func (s *Server) decideAgentUpload(ctx context.Context, principalID int64, subject string, find func() (store.Channel, error)) (schema.AgentManifestV1, store.Channel, string, error) {
	deny := func(channelID int64, reason string) (schema.AgentManifestV1, store.Channel, string, error) {
		s.auditAgentDenial(ctx, principalID, subject, schema.CapabilityFilesUpload, channelID, reason)
		return schema.AgentManifestV1{}, store.Channel{}, reason, nil
	}
	fail := func(err error) (schema.AgentManifestV1, store.Channel, string, error) {
		return schema.AgentManifestV1{}, store.Channel{}, "", err
	}
	m, reason, err := s.agentManifest(ctx, principalID)
	if err != nil {
		return fail(err)
	}
	if reason == "" && !m.Allows(schema.CapabilityFilesUpload) {
		reason = denyCapability
	}
	if reason != "" {
		return deny(0, reason)
	}
	channel, err := find()
	if errors.Is(err, store.ErrNotFound) {
		return deny(0, denyNotMember)
	}
	if err != nil {
		return fail(err)
	}
	member, err := s.store.IsChannelMember(ctx, channel.ID, principalID)
	if err != nil {
		return fail(err)
	}
	if !member {
		return deny(channel.ID, denyNotMember)
	}
	if !m.AllowsChannel(channel.ID, schema.ChannelPermissionAttach) {
		return deny(channel.ID, denyChannelPermission)
	}
	return m, channel, "", nil
}

// agentUploadChannel is the grant for a REST upload made with an agent's
// credential. A refusal is the 403 an agent got on this route before there was
// a grant, whichever part of the grant is missing and whether or not the
// channel exists, so the answer says nothing about any channel; the audit row
// says which part it was. When it reports false it has written the response.
func (s *Server) agentUploadChannel(w http.ResponseWriter, r *http.Request, agentID int64) (schema.AgentManifestV1, store.Channel, bool) {
	m, channel, reason, err := s.agentUploadGrant(r.Context(), agentID, routeSubject(r), r.PathValue("channel"))
	if err != nil {
		slog.ErrorContext(r.Context(), "files: decide agent upload grant failed", "principal", agentID, "error", err)
		writeError(w, http.StatusInternalServerError, errInternal.Code, errInternal.Message)
		return schema.AgentManifestV1{}, store.Channel{}, false
	}
	if reason != "" {
		writeError(w, http.StatusForbidden, errForbidden.Code, agentFilesRefusal)
		return schema.AgentManifestV1{}, store.Channel{}, false
	}
	return m, channel, true
}

// agentAttachRefusal decides whether a post by agent agentID in channel may
// carry payload, and returns the refusal, or nil when it may. It concerns only
// a conch.file_attachment.v1 payload, which names files that already exist:
// any other payload, and none, is not its business.
//
// Attaching publishes a file, so it takes the grant that uploading takes, at
// the moment of the post: files.upload, attach in this channel, membership.
// The store's own rule (the file is the author's, in this channel, not yet
// attached) says whose file it is; it cannot say whether the agent may still
// publish. Without the grant the answer is the one every unusable attachment
// gets, invalid_file_attachment, so the refusal says nothing a refused file
// would not; the audit row (access_denied, capability files.upload) says why.
// An agent no longer in the channel gets the unknown-channel answer, as from
// the post itself.
//
// Every path by which an agent posts calls it, after the post's own gates and
// before the insert: postMessage, postScopedMessage and postReply over REST,
// and the three ways of post_message over MCP (grantedChannel.attachRefusal,
// postReplyMCP). A post that brings its files with it (attachments) does not:
// prepareUploads has decided the same grant for it already.
func (s *Server) agentAttachRefusal(ctx context.Context, agentID int64, subject string, channel store.Channel, payload *schema.Payload) *schema.Error {
	if payload == nil || payload.Schema != schema.FileAttachmentV1Name {
		return nil
	}
	_, reason, err := s.agentUploadGrantIn(ctx, agentID, subject, channel)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "files: decide agent attach grant failed", "principal", agentID, "error", err)
		return errInternal
	case reason == denyNotMember:
		return errChannelNotFound
	case reason != "":
		return errInvalidFileAttachment
	}
	return nil
}

// callerAttachRefusal is agentAttachRefusal for a REST post: it concerns only
// a request made with an agent's own credential. It returns nil for anyone
// else; a post with no verified caller that names files is refused before it
// gets here (errFilesNeedAuth).
func (s *Server) callerAttachRefusal(r *http.Request, channel store.Channel, payload *schema.Payload) *schema.Error {
	caller, ok := callerFrom(r.Context())
	if !ok || caller.Kind != store.PrincipalAgent {
		return nil
	}
	return s.agentAttachRefusal(r.Context(), caller.ID, routeSubject(r), channel, payload)
}

// routeSubject is the audit subject of a REST request: its route pattern,
// never its path.
func routeSubject(r *http.Request) string {
	if r.Pattern == "" {
		return "<unmatched>"
	}
	return r.Pattern
}

// spendAgentUploads spends what n uploads by one call cost an agent the grant
// allowed: n files of its upload budget, all or none, and one use of the
// manifest's rate limit for files.upload, if m declares one. It returns nil
// when the call may proceed. A refusal has spent nothing, and is audited
// (thinned) as agent_rate_limited. The caller gives the files back with
// refundAgentUploads if it then stores none of them.
func (s *Server) spendAgentUploads(ctx context.Context, principalID int64, subject string, m schema.AgentManifestV1, n int) *rateLimitedError {
	now := s.now()
	ok, retry := s.agentUploadLimits.take(principalID, n, now)
	if !ok {
		if write, count := s.denials.admit(principalID, subject+"|"+agentUploadLimitName, now); write {
			slog.WarnContext(ctx, "files: agent upload budget exceeded", "principal", principalID)
			detail := fmt.Sprintf("capability=%s limit=%s max=%d window_seconds=3600 count=%d",
				schema.CapabilityFilesUpload, agentUploadLimitName, s.cfg.AgentUploadsPerHour, count)
			// The caller may already be gone; the refusal must still be recorded.
			actx := context.WithoutCancel(ctx)
			if _, err := s.store.AppendAuditEvent(actx, fmt.Sprintf("principal:%d", principalID), store.AuditAgentRateLimited, subject, detail); err != nil {
				slog.ErrorContext(actx, "files: audit agent_rate_limited failed", "principal", principalID, "error", err)
			}
		}
		return &rateLimitedError{retryAfter: retry}
	}
	// The manifest's limit after the budget, so that its refusal can give the
	// files back: a sliding log has no refund, a bucket has.
	if limited := s.spendAgentBudget(ctx, principalID, subject, m, schema.CapabilityFilesUpload); limited != nil {
		s.refundAgentUploads(principalID, n)
		return limited
	}
	return nil
}

// refundAgentUploads gives back n files of the agent's upload budget: the
// call that spent them stored nothing.
func (s *Server) refundAgentUploads(principalID int64, n int) {
	s.agentUploadLimits.refund(principalID, n, s.now())
}

// checkAgentUploadHead is the first part of an agent's REST upload: body must
// begin as a file of mediaType, which the caller has already found to be one
// an agent may upload. It returns a reader of the whole body, the bytes it
// looked at included. A body too short to carry a signature is a mismatch;
// any other failure to read is returned as it is, for the caller to tell a
// body over the limit from one that was interrupted.
func checkAgentUploadHead(body io.Reader, mediaType string) (io.Reader, *schema.Error, error) {
	br := bufio.NewReaderSize(body, 512)
	head, err := br.Peek(imageSignatureBytes)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("%w: %w", errUploadRead, err)
	}
	if sniffAgentImage(head) != mediaType {
		return nil, errAgentMediaType, nil
	}
	return br, nil, nil
}

// pendingUploads are the attachments of one post_message call, checked,
// paid for and staged, waiting for the message that carries them.
type pendingUploads struct {
	s           *Server
	principalID int64
	staged      []*stagedBlob
	files       []store.NewFile
	// placed says at least one file's bytes were put under their hash. From
	// then on the budget stays spent, whatever becomes of the post.
	placed bool
}

// forStore is what the store's message insert needs: the rows to make and the
// step that puts the bytes in place, last, inside its transaction. A post
// with no uploads has none (nil).
func (p *pendingUploads) forStore(ctx context.Context) *store.MessageUploads {
	if p == nil {
		return nil
	}
	return &store.MessageUploads{Files: p.files, Place: func() error {
		// The store rolls back a caller that has gone away; do not place
		// bytes for a transaction that is about to be abandoned.
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, b := range p.staged {
			if err := b.place(); err != nil {
				return err
			}
			p.placed = true
		}
		return nil
	}}
}

// finish ends the call's uploads, whatever became of the post: it removes
// every temporary file that was not placed, and gives the budget back if
// nothing was. After a post that was stored there is nothing left to do.
func (p *pendingUploads) finish() {
	for _, b := range p.staged {
		b.discard()
	}
	if !p.placed {
		p.s.refundAgentUploads(p.principalID, len(p.files))
	}
}

// prepareUploads does everything an agent's post_message with attachments
// needs before the message is posted, and nothing that outlasts a refusal:
//
//  1. the grant (agentUploadGrant): files.upload, membership, attach;
//  2. that the server takes agents' uploads at all;
//  3. every item: a valid name, one of the three media types, standard base64,
//     the decoded sizes within one MaxUploadBytes in total, and bytes that are
//     the type they declare;
//  4. the budget (spendAgentUploads), for all the items or none;
//  5. the bytes of each item written to a temporary file (stageBlob).
//
// The tool hands the result to the scope's message insert, which gives the
// store forStore(), and calls finish() when the post has ended, either way.
//
// All or nothing follows from where things are written: no row exists outside
// the insert's transaction, and no file is under its hash until that
// transaction's last step (Place). So a refusal at any point, here or in the
// insert, leaves no file row, no message and no stored file, only temporary
// files that finish removes; one a crash leaves is removed at the next start
// (removeStaleUploads). The one way bytes can be left under a hash with no row
// is a commit that fails after Place, as for the upload route's own orphan
// (files.go), and the budget for them stays spent.
func (a *agentScope) prepareUploads(ctx context.Context, channelName string, items []mcpAttachmentInput) (*pendingUploads, *schema.Error) {
	s, principalID := a.s, a.identity.principalID
	m, _, reason, err := s.agentUploadGrant(ctx, principalID, a.subject, channelName)
	switch {
	case err != nil:
		slog.ErrorContext(ctx, "mcp: decide agent upload grant failed", "principal", principalID, "error", err)
		return nil, errInternal
	case reason == denyNotMember:
		// As for every tool: an unknown channel and one the agent is not in.
		return nil, errChannelNotFound
	case reason != "":
		return nil, errForbidden
	}
	if !s.agentUploadsEnabled() {
		return nil, errUploadsDisabled
	}
	limit := s.cfg.MaxUploadBytes
	p := &pendingUploads{s: s, principalID: principalID}
	contents := make([][]byte, 0, len(items))
	var total int64
	for i, item := range items {
		if err := schema.ValidateFileName(item.Name); err != nil {
			return nil, &schema.Error{Code: "invalid_filename", Message: fmt.Sprintf("attachment %d: %s", i+1, err)}
		}
		if !schema.IsAgentUploadMediaType(item.MediaType) {
			return nil, errAgentMediaType
		}
		// The encoded length bounds the decoded one from above (within two
		// bytes), so anything this lets through is decoded into at most the
		// limit plus a little; the exact sum is checked after.
		if int64(base64.StdEncoding.DecodedLen(len(item.DataBase64))) > limit-total+2 {
			return nil, attachmentsTooLarge(limit)
		}
		data, err := base64.StdEncoding.Strict().DecodeString(item.DataBase64)
		if err != nil {
			return nil, &schema.Error{Code: "invalid_request", Message: fmt.Sprintf("attachment %d: data_base64 must be standard base64 with padding", i+1)}
		}
		if total += int64(len(data)); total > limit {
			return nil, attachmentsTooLarge(limit)
		}
		if sniffAgentImage(data[:min(len(data), imageSignatureBytes)]) != item.MediaType {
			return nil, errAgentMediaType
		}
		contents = append(contents, data)
		p.files = append(p.files, store.NewFile{Filename: item.Name, MediaType: item.MediaType, Size: int64(len(data))})
	}
	if limited := s.spendAgentUploads(ctx, principalID, a.subject, m, len(items)); limited != nil {
		return nil, limited.schemaError()
	}
	for i, data := range contents {
		b, err := s.stageBlob(bytes.NewReader(data))
		if err != nil {
			slog.ErrorContext(ctx, "mcp: stage attachment failed", "principal", principalID, "error", err)
			p.finish()
			return nil, errInternal
		}
		p.staged = append(p.staged, b)
		p.files[i].SHA256 = b.sum
	}
	return p, nil
}

// attachmentsTooLarge is the refusal of a call whose attachments, decoded,
// are more than one upload may be.
func attachmentsTooLarge(limit int64) *schema.Error {
	return &schema.Error{Code: "file_too_large", Message: fmt.Sprintf("the attachments of one call may total at most %d bytes", limit)}
}
