// An archived channel (issue #844): it keeps its history and members and
// refuses anything new. The server says so with 409 channel_archived on every
// write; the page says it in one line, wherever a write was refused or the
// composer would have been.
export const archivedLine = 'This channel is archived.';

// The code of a refused write in an archived channel.
export const channelArchived = 'channel_archived';

// The code of an archive of a default channel, which is refused.
export const channelArchiveDefault = 'channel_archive_default';

// Whether a channel is archived: its listing carries archived_at.
export function isArchived(channel: { readonly archived_at?: string | undefined } | null | undefined): boolean {
  return channel?.archived_at !== undefined;
}
