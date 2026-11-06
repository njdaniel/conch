// Files chosen for a message, with their local bytes kept until the message is
// sent. Drafts are in memory only and scoped to a channel and its normalized
// audience; nothing here persists bytes or makes a network request.
import { fileAttachmentPayloadSchema, type FileAttachmentV1 } from '../schema/file';
import type { Audience, Payload } from '../schema/message';

export const maxDraftAttachments = 10;

export type DraftAttachmentStatus = 'pending' | 'uploading' | 'ready' | 'failed' | 'cancelled';

export interface DraftAttachment {
  // Stable within this reducer, including across audience switches and renames.
  readonly clientId: number;
  readonly filename: string;
  readonly mediaType: string;
  readonly size: number;
  readonly bytes: Blob;
  readonly status: DraftAttachmentStatus;
  // The upload attempt that owns a later completion. Every retry advances it.
  readonly attempt: number;
  // Set once uploaded and retained thereafter, so a failed message can retry
  // without uploading the same local bytes again.
  readonly fileId?: number;
  readonly error?: string;
}

export interface NewDraftAttachment {
  readonly filename: string;
  readonly mediaType: string;
  readonly bytes: Blob;
}

// The inner key is audienceScope. Empty scopes are removed rather than kept.
export interface AttachmentDrafts {
  readonly channels: Readonly<Record<string, Readonly<Record<string, readonly DraftAttachment[]>>>>;
  readonly nextClientId: number;
  // Reset advances this so an old upload cannot complete into a new draft that
  // happens to reuse its clientId. Restores keep the current generation.
  readonly generation: number;
}

export type AttachmentDraftAction =
  | { readonly type: 'add'; readonly channel: string; readonly audience?: Audience; readonly files: readonly NewDraftAttachment[] }
  | { readonly type: 'remove'; readonly channel: string; readonly audience?: Audience; readonly clientId: number }
  | { readonly type: 'clear'; readonly channel: string; readonly audience?: Audience }
  | { readonly type: 'uploading'; readonly channel: string; readonly audience?: Audience; readonly clientId: number; readonly generation: number }
  | {
      readonly type: 'uploaded';
      readonly channel: string;
      readonly audience?: Audience;
      readonly clientId: number;
      readonly generation: number;
      readonly attempt: number;
      readonly fileId: number;
    }
  | {
      readonly type: 'uploadFailed';
      readonly channel: string;
      readonly audience?: Audience;
      readonly clientId: number;
      readonly generation: number;
      readonly attempt: number;
      readonly error?: string;
    }
  | {
      readonly type: 'cancelled';
      readonly channel: string;
      readonly audience?: Audience;
      readonly clientId: number;
      readonly generation: number;
      readonly attempt: number;
    }
  | { readonly type: 'renamed'; readonly from: string; readonly to: string }
  // Kept from the same person's expired session. Current drafts win, as with
  // text drafts; bytes and object URLs never leave this tab.
  | { readonly type: 'restore'; readonly drafts: AttachmentDrafts }
  | { readonly type: 'reset' };

export const initialAttachmentDrafts: AttachmentDrafts = { channels: {}, nextClientId: 1, generation: 0 };

export function normalizeAudience(audience: Audience | undefined): Audience | undefined {
  if (audience?.kind !== 'principals') return audience;
  return { kind: 'principals', principal_ids: [...new Set(audience.principal_ids)].sort((a, b) => a - b) };
}

export function audienceScope(audience: Audience | undefined): string {
  const normalized = normalizeAudience(audience);
  if (normalized === undefined) return 'channel';
  return normalized.kind === 'net' ? `net:${String(normalized.net_id)}` : `principals:${normalized.principal_ids.join(',')}`;
}

export function attachmentsFor(state: AttachmentDrafts, channel: string, audience?: Audience): readonly DraftAttachment[] {
  return state.channels[channel]?.[audienceScope(audience)] ?? [];
}

export function remainingAttachmentSlots(state: AttachmentDrafts, channel: string, audience?: Audience): number {
  return maxDraftAttachments - attachmentsFor(state, channel, audience).length;
}

function put(
  state: AttachmentDrafts,
  channel: string,
  audience: Audience | undefined,
  attachments: readonly DraftAttachment[],
): AttachmentDrafts {
  const scopes = state.channels[channel] ?? {};
  const scope = audienceScope(audience);
  const nextScopes =
    attachments.length === 0
      ? Object.fromEntries(Object.entries(scopes).filter(([name]) => name !== scope))
      : { ...scopes, [scope]: attachments };
  const channels =
    Object.keys(nextScopes).length === 0
      ? Object.fromEntries(Object.entries(state.channels).filter(([name]) => name !== channel))
      : { ...state.channels, [channel]: nextScopes };
  return { ...state, channels };
}

function changeOne(
  state: AttachmentDrafts,
  action: Extract<AttachmentDraftAction, { readonly clientId: number; readonly channel: string }>,
  change: (attachment: DraftAttachment) => DraftAttachment,
): AttachmentDrafts {
  const current = attachmentsFor(state, action.channel, action.audience);
  const index = current.findIndex((attachment) => attachment.clientId === action.clientId);
  if (index < 0) return state;
  const attachment = current[index];
  if (attachment === undefined) return state;
  const changed = change(attachment);
  if (changed === attachment) return state;
  const next = [...current];
  next[index] = changed;
  return put(state, action.channel, action.audience, next);
}

function ownsAttempt(action: Extract<AttachmentDraftAction, { readonly generation: number }>, state: AttachmentDrafts): boolean {
  return action.generation === state.generation;
}

export function attachmentDraftsReducer(state: AttachmentDrafts, action: AttachmentDraftAction): AttachmentDrafts {
  switch (action.type) {
    case 'add': {
      const count = Math.min(action.files.length, remainingAttachmentSlots(state, action.channel, action.audience));
      if (count === 0) return state;
      const added = action.files.slice(0, count).map((file, index): DraftAttachment => ({
        clientId: state.nextClientId + index,
        filename: file.filename,
        mediaType: file.mediaType,
        size: file.bytes.size,
        bytes: file.bytes,
        status: 'pending',
        attempt: 0,
      }));
      return {
        ...put(state, action.channel, action.audience, [...attachmentsFor(state, action.channel, action.audience), ...added]),
        nextClientId: state.nextClientId + count,
      };
    }
    case 'remove':
      return put(state, action.channel, action.audience, attachmentsFor(state, action.channel, action.audience).filter((a) => a.clientId !== action.clientId));
    case 'clear':
      return put(state, action.channel, action.audience, []);
    case 'uploading':
      if (!ownsAttempt(action, state)) return state;
      return changeOne(state, action, (attachment) =>
        attachment.status === 'ready' || attachment.status === 'uploading'
          ? attachment
          : { ...attachment, status: 'uploading', attempt: attachment.attempt + 1, error: undefined },
      );
    case 'uploaded':
      if (!ownsAttempt(action, state)) return state;
      return changeOne(state, action, (attachment) =>
        attachment.status === 'uploading' && attachment.attempt === action.attempt
          ? { ...attachment, status: 'ready', fileId: action.fileId, error: undefined }
          : attachment,
      );
    case 'uploadFailed':
      if (!ownsAttempt(action, state)) return state;
      return changeOne(state, action, (attachment) =>
        attachment.status === 'uploading' && attachment.attempt === action.attempt
          ? { ...attachment, status: 'failed', error: action.error }
          : attachment,
      );
    case 'cancelled':
      if (!ownsAttempt(action, state)) return state;
      return changeOne(state, action, (attachment) =>
        attachment.status === 'uploading' && attachment.attempt === action.attempt
          ? { ...attachment, status: 'cancelled', error: undefined }
          : attachment,
      );
    case 'renamed': {
      const moved = state.channels[action.from];
      if (action.from === action.to || moved === undefined) return state;
      const channels: Record<string, Readonly<Record<string, readonly DraftAttachment[]>>> = {};
      for (const [name, scopes] of Object.entries(state.channels)) if (name !== action.from) channels[name] = scopes;
      channels[action.to] = moved;
      return { ...state, channels };
    }
    case 'restore': {
      let nextClientId = Math.max(state.nextClientId, action.drafts.nextClientId);
      const channels: Record<string, Readonly<Record<string, readonly DraftAttachment[]>>> = {};
      const names = new Set([...Object.keys(action.drafts.channels), ...Object.keys(state.channels)]);
      for (const name of names) {
        const restored = Object.fromEntries(
          Object.entries(action.drafts.channels[name] ?? {}).map(([scope, attachments]) => [
            scope,
            attachments.map((attachment) =>
              attachment.status === 'uploading' ? { ...attachment, status: 'pending' as const, error: undefined } : attachment,
            ),
          ]),
        );
        const current = state.channels[name] ?? {};
        const scopes: Record<string, readonly DraftAttachment[]> = { ...restored, ...current };
        for (const attachments of Object.values(scopes)) {
          for (const attachment of attachments) nextClientId = Math.max(nextClientId, attachment.clientId + 1);
        }
        if (Object.keys(scopes).length > 0) channels[name] = scopes;
      }
      return { channels, nextClientId, generation: state.generation };
    }
    case 'reset':
      return { ...initialAttachmentDrafts, generation: state.generation + 1 };
  }
}

// attachmentPayload freezes uploaded ids in composer order. It is undefined for
// an empty draft or one that is not completely ready; callers must not turn the
// latter into a text-only post by accident.
export function attachmentPayload(attachments: readonly DraftAttachment[]): Payload | undefined {
  if (attachments.length === 0 || attachments.some((attachment) => attachment.status !== 'ready' || attachment.fileId === undefined)) return undefined;
  const data: FileAttachmentV1 = { file_ids: attachments.map((attachment) => attachment.fileId as number) };
  return { schema: fileAttachmentPayloadSchema, data };
}
