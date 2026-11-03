package server

import (
	"context"
	"log/slog"
	"slices"

	"github.com/njdaniel/conch/internal/server/store"
	"github.com/njdaniel/conch/pkg/schema"
)

// The directory signal's audience (community-model §13.6, ADR-014): the
// content-free conch.directory_changed.v1 goes only to the viewers whose
// answer to GET /v1/directory/members changed. A signal sent to everyone
// would itself tell alice that something happened among people she cannot
// see.
//
//   - the directory permission switched: every member and operator
//     (directorySettingChanged);
//   - a membership added or removed: with members holding
//     see_member_directory, nobody, since membership does not change the
//     directory then; without it, the subject, the operators, and each
//     member of the channel whose knowing of the subject changed
//     (directoryMembershipChanged);
//   - a principal created, renamed, disabled, enabled, hidden or re-roled:
//     every viewer who may know it, before or after
//     (directoryPrincipalChanged).
//
// The audiences are computed by the store from its one may-know fragment, so
// this file decides nothing about who may know whom. A failed read is logged
// and sends nothing: the frame carries no content, so staying silent costs a
// client one stale list until its next read, and a guess would tell a viewer
// something happened.

// directoryMembershipChanged tells the viewers whose directory answer a
// membership change of subject in channelID moved. Call it after the change
// committed, and only when something was added or removed.
func (s *Server) directoryMembershipChanged(ctx context.Context, channelID, subject int64) {
	everyone, ids, err := s.store.DirectoryMembershipAudience(ctx, channelID, subject)
	if err != nil {
		slog.ErrorContext(ctx, "directory: read the membership audience failed", "channel", channelID, "subject", subject, "error", err)
		return
	}
	if everyone {
		return
	}
	s.events.notifyDirectoryChanged(false, ids...)
}

// directoryWatch is who could know a principal before a change, taken before
// the change is written; a nil one is "nobody" (a principal just created).
type directoryWatch struct {
	everyone bool
	ids      []int64
	failed   bool
}

// watchDirectory takes the audience of subject as it stands. Call it before
// a write that can remove the subject from viewers' directories (hiding it,
// which drops its memberships).
func (s *Server) watchDirectory(ctx context.Context, subject int64) *directoryWatch {
	everyone, ids, err := s.store.DirectoryKnowers(ctx, subject)
	if err != nil {
		slog.ErrorContext(ctx, "directory: read the audience failed", "subject", subject, "error", err)
		return &directoryWatch{failed: true}
	}
	return &directoryWatch{everyone: everyone, ids: ids}
}

// directoryPrincipalChanged tells every viewer who may know subject, now or
// (with a watch taken before the write) before it, that the principal was
// created, renamed, disabled, enabled, hidden or re-roled.
func (s *Server) directoryPrincipalChanged(ctx context.Context, subject int64, before *directoryWatch) {
	if before != nil && before.failed {
		return
	}
	everyone, ids, err := s.store.DirectoryKnowers(ctx, subject)
	if err != nil {
		slog.ErrorContext(ctx, "directory: read the audience failed", "subject", subject, "error", err)
		return
	}
	if everyone || (before != nil && before.everyone) {
		s.events.notifyDirectoryChanged(true)
		return
	}
	if before != nil {
		ids = append(ids, before.ids...)
	}
	s.events.notifyDirectoryChanged(false, ids...)
}

// directorySettingChanged tells every member and operator that the directory
// permission was switched: their list may now be everyone, or only the
// people they may know.
func (s *Server) directorySettingChanged(changes []store.PermissionCellChange) {
	if slices.ContainsFunc(changes, func(c store.PermissionCellChange) bool { return c.Permission == schema.PermSeeMemberDirectory }) {
		s.events.notifyDirectoryChanged(true)
	}
}
