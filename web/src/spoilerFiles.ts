import { previewText } from './Linkified';
import { fileAttachmentPayloadSchema, MAX_FILE_NAME_BYTES, SPOILER_FILE_PREFIX } from './schema/file';
import type { MessageV2 } from './schema/message';

// A spoiler attachment is a file whose stored name starts with the prefix of
// pkg/schema (issue #1500). It is presentation only: it never grants or
// withholds access to the file.

const encoder = new TextEncoder();

function byteLength(text: string): number {
  return encoder.encode(text).length;
}

// isSpoilerName is an exact, case-sensitive prefix match.
export function isSpoilerName(name: string): boolean {
  return name.startsWith(SPOILER_FILE_PREFIX);
}

// shownName is the name people read: the stored name without the prefix.
export function shownName(name: string): string {
  return isSpoilerName(name) ? name.slice(SPOILER_FILE_PREFIX.length) : name;
}

// trimTo keeps whole code points of text within a byte budget.
function trimTo(text: string, budget: number): string {
  let out = '';
  let used = 0;
  for (const point of text) {
    const size = byteLength(point);
    if (used + size > budget) break;
    out += point;
    used += size;
  }
  return out;
}

// spoilerName is the stored name of a file marked as a spoiler. It stays
// within the longest name the server accepts by trimming the base name before
// its extension, at a character boundary, and it leaves a name that already
// has the prefix alone.
export function spoilerName(name: string): string {
  if (isSpoilerName(name)) return name;
  const full = SPOILER_FILE_PREFIX + name;
  if (byteLength(full) <= MAX_FILE_NAME_BYTES) return full;
  const dot = name.lastIndexOf('.');
  const extension = dot > 0 ? name.slice(dot) : '';
  const room = MAX_FILE_NAME_BYTES - byteLength(SPOILER_FILE_PREFIX);
  // An extension that leaves no room for a base name is cut with the rest.
  if (extension === '' || byteLength(extension) >= room) return SPOILER_FILE_PREFIX + trimTo(name, room);
  return SPOILER_FILE_PREFIX + trimTo(name.slice(0, dot), room - byteLength(extension)) + extension;
}

// storedName is the name a draft is uploaded under, and the body an image-only
// message carries.
export function storedName(draft: { readonly filename: string; readonly spoiler: boolean }): string {
  return draft.spoiler ? spoilerName(draft.filename) : draft.filename;
}

// canSpoilerName is whether a file can be marked without renaming its
// extension: a name whose extension alone leaves no room for the prefix and a
// base name is refused, never cut into.
export function canSpoilerName(name: string): boolean {
  if (isSpoilerName(name) || byteLength(SPOILER_FILE_PREFIX + name) <= MAX_FILE_NAME_BYTES) return true;
  const dot = name.lastIndexOf('.');
  const extension = dot > 0 ? name.slice(dot) : '';
  return byteLength(extension) < MAX_FILE_NAME_BYTES - byteLength(SPOILER_FILE_PREFIX);
}

// attachmentNames is the stored names an image-only message carries as its
// body (the composer joins them with ", "), or null when the body is more than
// that. It must have exactly one part per attached file and hold no "||", so a
// caption (a text spoiler, commas inside it) is never taken for names.
export function attachmentNames(message: Pick<MessageV2, 'body' | 'payload'>): readonly string[] | null {
  const payload = message.payload;
  if (payload?.schema !== fileAttachmentPayloadSchema || message.body.includes('||')) return null;
  const ids = (payload.data as { file_ids?: unknown }).file_ids;
  if (!Array.isArray(ids)) return null;
  const parts = message.body.split(', ');
  return parts.length === ids.length && parts.every((part) => part !== '') ? parts : null;
}

// displayBody is a message's body for people: an image-only message's stored
// names without the spoiler prefix, anything else as it is.
export function displayBody(message: Pick<MessageV2, 'body' | 'payload'>): string {
  const names = attachmentNames(message);
  return names !== null && names.some(isSpoilerName) ? names.map(shownName).join(', ') : message.body;
}

// messagePreview is a message's body outside the message view (a system
// notification, the notification list). It never shows a spoiler: a text
// spoiler reads "[spoiler]", and an image sent with no text, whose body is the
// stored names, reads "[spoiler] {shown name}" for each marked one.
export function messagePreview(message: Pick<MessageV2, 'body' | 'payload'>): string {
  const names = attachmentNames(message);
  if (names !== null && names.some(isSpoilerName)) return names.map((name) => (isSpoilerName(name) ? `[spoiler] ${shownName(name)}` : name)).join(', ');
  return previewText(message.body);
}
