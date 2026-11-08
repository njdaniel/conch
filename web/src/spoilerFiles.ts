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

// messagePreview is a message's body outside the message view (a system
// notification, the notification list). It never shows a spoiler: a text
// spoiler reads "[spoiler]", and an image sent with no text, whose body is the
// stored names, reads "[spoiler] {shown name}".
export function messagePreview(message: Pick<MessageV2, 'body' | 'payload'>): string {
  if (message.payload?.schema === fileAttachmentPayloadSchema) {
    const parts = message.body.split(', ');
    if (parts.some(isSpoilerName)) return parts.map((part) => (isSpoilerName(part) ? `[spoiler] ${shownName(part)}` : previewText(part))).join(', ');
  }
  return previewText(message.body);
}
