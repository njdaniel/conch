package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// Preview as member (issue #1515, design docs/design/permission-preview.md
// §3 to §5): an administrator reads, under their own credential, what a named
// member sees. One request header, Conch-Preview-As, on a short allowlist of
// GET routes (route.previewable); guard swaps the caller for the target on
// those routes and the existing handlers answer as the target would.
//
// Nothing here writes anything but one audit row. Nothing runs under preview
// that writes a membership, a credential, a session or a read mark: the
// allowlist is what guarantees it (TestPreviewableRoutesAreReads).

// previewHeader carries the target's principal id, decimal.
const previewHeader = "Conch-Preview-As"

// AuditPreviewViewed is the audit action GET /v1/preview writes: actor the
// administrator, subject principal:<target>, empty detail.
const AuditPreviewViewed = "preview_viewed"

// denyAgentPreview is the audit reason of an agent asking for a preview.
const denyAgentPreview = "agents_no_preview"

// previewContext is who is previewing whom, carried in the request context
// once guard has swapped the caller.
type previewContext struct {
	Actor  store.Principal // the administrator, who authenticated
	Target store.Principal // the member whose view is read
}

type previewKey struct{}

func withPreview(ctx context.Context, pc previewContext) context.Context {
	return context.WithValue(ctx, previewKey{}, pc)
}

// previewFrom returns the preview a request runs under, if any.
func previewFrom(ctx context.Context) (previewContext, bool) {
	pc, ok := ctx.Value(previewKey{}).(previewContext)
	return pc, ok
}

// parsePreviewHeader reads the header's value: a positive decimal. ok is
// false for anything else (empty, signs, spaces, zero). A number too large
// for an id is a valid, unknown id: it is returned as MaxInt64, which
// matches no principal, so it answers the uniform 404.
func parsePreviewHeader(values []string) (id int64, ok bool) {
	if len(values) != 1 || values[0] == "" {
		return 0, false
	}
	v := values[0]
	nonZero := false
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
		if v[i] != '0' {
			nonZero = true
		}
	}
	if !nonZero {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return math.MaxInt64, true
	}
	return n, true
}

// previewNeedsAuth wraps a route under --auth off: there is no caller to
// preview from, so the header is refused (design §4.2).
func (s *Server) previewNeedsAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values(previewHeader)) > 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "preview needs authentication")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// previewStep is guard's preview step (design §4.2), run after the caller is
// authenticated and before the class check, whenever the header is present.
// Its order is fixed: route allowlist, caller, target, swap. A request
// without the header passes through untouched.
func (s *Server) previewStep(rt route, next http.Handler) http.Handler {
	// An agent is refused as on every human-only route, and a guest as on
	// every route that is not a guest's; both before the permission test.
	checked := s.humansOnlyBecause(denyAgentPreview, "agents cannot preview a member's view",
		s.membersOnly(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.previewTarget(w, r, next)
		})))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := r.Header.Values(previewHeader)
		if len(values) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		caller, ok := callerFrom(r.Context())
		if !ok {
			// Wiring bug: a non-exempt route always has a caller. Deny.
			writeUnauthenticated(w)
			return
		}
		id, valid := parsePreviewHeader(values)
		// 1. The route allowlist, before the target is looked up, so a
		// refused route learns nothing about the target. The audit names the
		// id only when it is one; the raw header is never logged.
		if !rt.previewable {
			as := "preview_as=invalid"
			if valid {
				as = fmt.Sprintf("preview_as=%d", id)
			}
			s.denyForbiddenAudited(w, r, caller.ID, schema.ErrorCodePreviewReadOnly, "this route cannot be read in preview", "", as)
			return
		}
		if !valid {
			writeError(w, http.StatusBadRequest, "invalid_request", "Conch-Preview-As must be a principal id, a positive decimal number")
			return
		}
		checked.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), previewIDKey{}, id)))
	})
}

type previewIDKey struct{}

// previewTarget is the caller and target steps of previewStep, for a human
// that is not a guest.
func (s *Server) previewTarget(w http.ResponseWriter, r *http.Request, next http.Handler) {
	ctx := r.Context()
	caller, _ := callerFrom(ctx)
	id, _ := ctx.Value(previewIDKey{}).(int64)
	as := fmt.Sprintf("preview_as=%d", id)
	fail := func(err error, what string) {
		slog.ErrorContext(ctx, "preview: "+what+" failed", "principal", caller.ID, "error", err)
		writeInternalError(w)
	}

	// 2. The caller holds both permissions, re-read on every request. The
	// first one missing, in catalog order, is the one named.
	for _, perm := range []schema.Permission{schema.PermManageMembers, schema.PermManageChannels} {
		allowed, err := s.can(ctx, caller, perm)
		if err != nil {
			fail(err, "permission check")
			return
		}
		if allowed {
			continue
		}
		if named := refusedPermission(caller, perm); named != "" {
			s.denyForbiddenAudited(w, r, caller.ID, schema.ErrorCodePermissionDenied,
				fmt.Sprintf("this needs the %s permission, which you do not hold on this server", named), named, as)
			return
		}
		s.denyForbiddenAudited(w, r, caller.ID, "forbidden", "operator role required", "", as)
		return
	}

	// 3. The target. Both reads run whatever the first answer is, so an
	// unknown id and a hidden one cost the same.
	target, err := s.store.PrincipalByID(ctx, id)
	found := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		fail(err, "find target")
		return
	}
	member, err := s.store.IsCommunityMember(ctx, id)
	if err != nil {
		fail(err, "member check")
		return
	}
	// Owner protection comes before the member test, so an owner id gets
	// the honest 403 rather than a 404 that could be probed.
	if found && target.IsOwner() && !caller.IsOwner() {
		s.denyForbiddenAudited(w, r, caller.ID, "forbidden", "only an owner may act on an owner", "", as)
		return
	}
	if !found || !member {
		writeError(w, http.StatusNotFound, "principal_not_found", "principal not found")
		return
	}
	if target.ID == caller.ID {
		writeError(w, http.StatusBadRequest, "invalid_request", "cannot preview yourself")
		return
	}

	// 4. The swap. The credential in the context stays the administrator's.
	ctx = withPreview(withCaller(ctx, target), previewContext{Actor: caller, Target: target})
	next.ServeHTTP(w, r.WithContext(ctx))
}

// handlePreview serves GET /v1/preview: the target's standing and effective
// permissions, PreviewResponseV1, and the one audited preview read.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pv, ok := previewFrom(ctx)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid_request", "preview target required")
		return
	}
	target := pv.Target
	perms, revision, err := s.store.EffectivePermissions(ctx, target.ID)
	if err != nil {
		slog.ErrorContext(ctx, "preview: read permissions failed", "principal", pv.Actor.ID, "error", err)
		writeInternalError(w)
		return
	}
	resp := schema.PreviewResponseV1{
		ID:                  target.ID,
		Kind:                schema.PrincipalKind(target.Kind),
		Name:                target.Name,
		Role:                schema.PrincipalRole(target.Role),
		Owner:               target.IsOwner(),
		Permissions:         perms,
		PermissionsRevision: revision,
		MayInvite:           slices.Contains(perms, schema.PermMakeMemberLinks),
		VoiceAllowed:        voiceAllowed(target) && slices.Contains(perms, schema.PermUseVoice),
	}
	if err := resp.Validate(); err != nil {
		slog.ErrorContext(ctx, "preview: invalid answer", "principal", pv.Actor.ID, "error", err)
		writeInternalError(w)
		return
	}
	// Audit first, and fail closed: a preview that cannot be recorded is not
	// given. The actor is the administrator, never the target.
	if _, err := s.store.AppendAuditEvent(ctx, auditActor(ctx), AuditPreviewViewed, fmt.Sprintf("principal:%d", target.ID), ""); err != nil {
		slog.ErrorContext(ctx, "preview: audit failed", "principal", pv.Actor.ID, "error", err)
		writeInternalError(w)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
