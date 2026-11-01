import { describe, expect, it } from 'vitest';
import type { Audience } from '../schema/message';
import {
  attachmentDraftsReducer,
  attachmentPayload,
  attachmentsFor,
  audienceScope,
  initialAttachmentDrafts,
  maxDraftAttachments,
  normalizeAudience,
  remainingAttachmentSlots,
  type AttachmentDrafts,
  type NewDraftAttachment,
} from './attachments';

const whisper = (principal_ids: number[]): Audience => ({ kind: 'principals', principal_ids });
const net = (net_id: number): Audience => ({ kind: 'net', net_id });
const file = (filename: string, bytes = 'image'): NewDraftAttachment => ({
  filename,
  mediaType: 'image/png',
  bytes: new Blob([bytes], { type: 'image/png' }),
});

function add(state: AttachmentDrafts, channel: string, audience: Audience | undefined, ...files: NewDraftAttachment[]): AttachmentDrafts {
  return attachmentDraftsReducer(state, { type: 'add', channel, ...(audience === undefined ? {} : { audience }), files });
}

describe('attachment drafts of any file type', () => {
  it('keeps an image, a non-image and an image that failed the sniff under one scope, in order', () => {
    const generic = (filename: string, mediaType: string): NewDraftAttachment => ({ filename, mediaType, bytes: new Blob(['x'], { type: mediaType }) });
    const state = add(initialAttachmentDrafts, 'ops', whisper([2, 1]), file('a.png'), generic('notes.pdf', 'application/pdf'), generic('fake.png', 'application/octet-stream'));
    expect(attachmentsFor(state, 'ops', whisper([1, 2])).map((a) => [a.filename, a.mediaType, a.status])).toEqual([
      ['a.png', 'image/png', 'pending'],
      ['notes.pdf', 'application/pdf', 'pending'],
      ['fake.png', 'application/octet-stream', 'pending'],
    ]);
    // A kept file is restored under its own audience only.
    const restored = attachmentDraftsReducer(initialAttachmentDrafts, { type: 'restore', drafts: state });
    expect(attachmentsFor(restored, 'ops')).toEqual([]);
    expect(attachmentsFor(restored, 'ops', whisper([1, 2])).map((a) => a.filename)).toEqual(['a.png', 'notes.pdf', 'fake.png']);
  });
});

describe('attachment drafts', () => {
  it('keeps local bytes and metadata under stable client ids', async () => {
    let state = add(initialAttachmentDrafts, 'ops', undefined, file('one.png', 'abc'), file('two.png'));
    const attachments = attachmentsFor(state, 'ops');
    expect(attachments.map((a) => a.clientId)).toEqual([1, 2]);
    expect(attachments.map((a) => [a.filename, a.mediaType, a.size, a.status, a.attempt])).toEqual([
      ['one.png', 'image/png', 3, 'pending', 0],
      ['two.png', 'image/png', 5, 'pending', 0],
    ]);
    await expect(attachments[0]?.bytes.text()).resolves.toBe('abc');

    state = attachmentDraftsReducer(state, { type: 'remove', channel: 'ops', clientId: 1 });
    state = add(state, 'ops', undefined, file('three.png'));
    expect(attachmentsFor(state, 'ops').map((a) => a.clientId)).toEqual([2, 3]);
  });

  it('scopes drafts by channel and normalized audience', () => {
    let state = add(initialAttachmentDrafts, 'ops', whisper([5, 3, 5]), file('whisper.png'));
    state = add(state, 'ops', whisper([3, 5]), file('same-whisper.png'));
    state = add(state, 'ops', net(4), file('net.png'));
    state = add(state, 'ops', undefined, file('channel.png'));
    state = add(state, 'dev', whisper([3, 5]), file('other-channel.png'));

    expect(attachmentsFor(state, 'ops', whisper([5, 3])).map((a) => a.filename)).toEqual(['whisper.png', 'same-whisper.png']);
    expect(attachmentsFor(state, 'ops', net(4)).map((a) => a.filename)).toEqual(['net.png']);
    expect(attachmentsFor(state, 'ops').map((a) => a.filename)).toEqual(['channel.png']);
    expect(attachmentsFor(state, 'dev', whisper([3, 5])).map((a) => a.filename)).toEqual(['other-channel.png']);
    expect(normalizeAudience(whisper([5, 3, 5]))).toEqual(whisper([3, 5]));
    expect(audienceScope(whisper([5, 3]))).toBe('principals:3,5');
  });

  it('bounds each channel and audience to ten files without consuming ids for excess files', () => {
    let state = add(initialAttachmentDrafts, 'ops', undefined, ...Array.from({ length: 12 }, (_, i) => file(`${String(i)}.png`)));
    expect(attachmentsFor(state, 'ops')).toHaveLength(maxDraftAttachments);
    expect(remainingAttachmentSlots(state, 'ops')).toBe(0);
    expect(state.nextClientId).toBe(11);

    state = add(state, 'ops', net(4), file('other-audience.png'));
    expect(attachmentsFor(state, 'ops', net(4))).toHaveLength(1);
    state = attachmentDraftsReducer(state, { type: 'remove', channel: 'ops', clientId: 1 });
    state = add(state, 'ops', undefined, file('fits-now.png'), file('still-too-many.png'));
    expect(attachmentsFor(state, 'ops').at(-1)?.filename).toBe('fits-now.png');
    expect(state.nextClientId).toBe(13);
  });

  it('moves through pending, uploading and ready, retaining bytes and building an ordered payload', () => {
    let state = add(initialAttachmentDrafts, 'ops', undefined, file('first.png'), file('second.png'));
    for (const [clientId, fileId] of [[1, 90], [2, 42]] as const) {
      state = attachmentDraftsReducer(state, { type: 'uploading', channel: 'ops', clientId, generation: 0 });
      expect(attachmentsFor(state, 'ops').find((a) => a.clientId === clientId)).toMatchObject({ status: 'uploading', attempt: 1 });
      state = attachmentDraftsReducer(state, { type: 'uploaded', channel: 'ops', clientId, generation: 0, attempt: 1, fileId });
    }
    const attachments = attachmentsFor(state, 'ops');
    expect(attachments.map((a) => [a.status, a.fileId])).toEqual([
      ['ready', 90],
      ['ready', 42],
    ]);
    expect(attachments[0]?.bytes).toBeInstanceOf(Blob);
    expect(attachmentPayload(attachments)).toEqual({
      schema: 'conch.file_attachment.v1',
      data: { file_ids: [90, 42] },
    });
  });

  it('fails with an explanation, retries with a new attempt, and ignores the old attempt', () => {
    let state = add(initialAttachmentDrafts, 'ops', undefined, file('one.png'));
    state = attachmentDraftsReducer(state, { type: 'uploading', channel: 'ops', clientId: 1, generation: 0 });
    state = attachmentDraftsReducer(state, { type: 'uploadFailed', channel: 'ops', clientId: 1, generation: 0, attempt: 1, error: 'Too large' });
    expect(attachmentsFor(state, 'ops')[0]).toMatchObject({ status: 'failed', error: 'Too large' });

    state = attachmentDraftsReducer(state, { type: 'uploading', channel: 'ops', clientId: 1, generation: 0 });
    state = attachmentDraftsReducer(state, { type: 'uploaded', channel: 'ops', clientId: 1, generation: 0, attempt: 1, fileId: 70 });
    expect(attachmentsFor(state, 'ops')[0]).toMatchObject({ status: 'uploading', attempt: 2, error: undefined });
    state = attachmentDraftsReducer(state, { type: 'uploaded', channel: 'ops', clientId: 1, generation: 0, attempt: 2, fileId: 71 });
    expect(attachmentsFor(state, 'ops')[0]).toMatchObject({ status: 'ready', fileId: 71 });
  });

  it('cancels only the active upload and ignores its later completion', () => {
    let state = add(initialAttachmentDrafts, 'ops', undefined, file('one.png'));
    state = attachmentDraftsReducer(state, { type: 'uploading', channel: 'ops', clientId: 1, generation: 0 });
    state = attachmentDraftsReducer(state, { type: 'cancelled', channel: 'ops', clientId: 1, generation: 0, attempt: 1 });
    expect(attachmentsFor(state, 'ops')[0]?.status).toBe('cancelled');
    state = attachmentDraftsReducer(state, { type: 'uploaded', channel: 'ops', clientId: 1, generation: 0, attempt: 1, fileId: 8 });
    expect(attachmentsFor(state, 'ops')[0]).toMatchObject({ status: 'cancelled' });
  });

  it('does not upload twice or accept completion after removal or clear', () => {
    let state = add(initialAttachmentDrafts, 'ops', undefined, file('one.png'), file('two.png'));
    state = attachmentDraftsReducer(state, { type: 'uploading', channel: 'ops', clientId: 1, generation: 0 });
    const uploading = attachmentDraftsReducer(state, { type: 'uploading', channel: 'ops', clientId: 1, generation: 0 });
    expect(uploading).toBe(state);
    state = attachmentDraftsReducer(state, { type: 'remove', channel: 'ops', clientId: 1 });
    state = attachmentDraftsReducer(state, { type: 'uploaded', channel: 'ops', clientId: 1, generation: 0, attempt: 1, fileId: 8 });
    expect(attachmentsFor(state, 'ops').map((a) => a.clientId)).toEqual([2]);
    state = attachmentDraftsReducer(state, { type: 'clear', channel: 'ops' });
    expect(attachmentsFor(state, 'ops')).toEqual([]);
    expect(state.channels.ops).toBeUndefined();
  });

  it('reset clears bytes and prevents a reused client id from accepting an old completion', () => {
    let state = add(initialAttachmentDrafts, 'ops', undefined, file('old.png'));
    state = attachmentDraftsReducer(state, { type: 'uploading', channel: 'ops', clientId: 1, generation: 0 });
    state = attachmentDraftsReducer(state, { type: 'reset' });
    expect(state.channels).toEqual({});
    expect(state.generation).toBe(1);

    state = add(state, 'ops', undefined, file('new.png'));
    state = attachmentDraftsReducer(state, { type: 'uploaded', channel: 'ops', clientId: 1, generation: 0, attempt: 1, fileId: 9 });
    expect(attachmentsFor(state, 'ops')[0]).toMatchObject({ filename: 'new.png', status: 'pending' });
    state = attachmentDraftsReducer(state, { type: 'uploading', channel: 'ops', clientId: 1, generation: 1 });
    state = attachmentDraftsReducer(state, { type: 'uploaded', channel: 'ops', clientId: 1, generation: 1, attempt: 1, fileId: 10 });
    expect(attachmentsFor(state, 'ops')[0]).toMatchObject({ status: 'ready', fileId: 10 });
  });

  it('moves every audience with a channel rename and leaves the old name empty', () => {
    let state = add(initialAttachmentDrafts, 'ops', whisper([2, 1]), file('one.png'));
    state = add(state, 'dev', whisper([2, 1]), file('other.png'));
    state = attachmentDraftsReducer(state, { type: 'renamed', from: 'ops', to: 'ops-room' });
    expect(attachmentsFor(state, 'ops-room', whisper([1, 2])).map((a) => a.filename)).toEqual(['one.png']);
    expect(attachmentsFor(state, 'ops', whisper([1, 2]))).toEqual([]);
    expect(attachmentsFor(state, 'dev', whisper([1, 2])).map((a) => a.filename)).toEqual(['other.png']);
  });

  it('restores kept drafts with current choices winning and fresh ids that cannot collide', () => {
    let kept = add(initialAttachmentDrafts, 'ops', undefined, file('kept.png'));
    kept = add(kept, 'dev', undefined, file('dev.png'));
    let state = attachmentDraftsReducer(kept, { type: 'reset' });
    state = add(state, 'ops', undefined, file('current.png'));
    const current = attachmentsFor(state, 'ops')[0];
    state = attachmentDraftsReducer(state, { type: 'restore', drafts: kept });

    expect(attachmentsFor(state, 'ops')).toEqual([current]);
    expect(attachmentsFor(state, 'dev').map((a) => a.filename)).toEqual(['dev.png']);
    expect(state.generation).toBe(1);
    state = add(state, 'ops', undefined, file('next.png'));
    expect(attachmentsFor(state, 'ops').at(-1)?.clientId).toBe(3);
  });

  it('restores an interrupted upload as pending so it can be retried in the new generation', () => {
    let kept = add(initialAttachmentDrafts, 'ops', undefined, file('kept.png'));
    kept = attachmentDraftsReducer(kept, { type: 'uploading', channel: 'ops', clientId: 1, generation: 0 });
    let state = attachmentDraftsReducer(kept, { type: 'reset' });
    state = attachmentDraftsReducer(state, { type: 'restore', drafts: kept });
    expect(attachmentsFor(state, 'ops')[0]).toMatchObject({ status: 'pending', attempt: 1 });

    state = attachmentDraftsReducer(state, { type: 'uploading', channel: 'ops', clientId: 1, generation: 1 });
    state = attachmentDraftsReducer(state, { type: 'uploaded', channel: 'ops', clientId: 1, generation: 0, attempt: 1, fileId: 8 });
    expect(attachmentsFor(state, 'ops')[0]).toMatchObject({ status: 'uploading', attempt: 2 });
    state = attachmentDraftsReducer(state, { type: 'uploaded', channel: 'ops', clientId: 1, generation: 1, attempt: 2, fileId: 9 });
    expect(attachmentsFor(state, 'ops')[0]).toMatchObject({ status: 'ready', fileId: 9 });
  });

  it('never turns an incomplete or empty attachment list into a payload', () => {
    expect(attachmentPayload([])).toBeUndefined();
    const state = add(initialAttachmentDrafts, 'ops', undefined, file('one.png'));
    expect(attachmentPayload(attachmentsFor(state, 'ops'))).toBeUndefined();
  });
});
