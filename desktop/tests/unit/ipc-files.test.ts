import { describe, expect, it } from 'vitest';

import type { Feeds } from '../../main/api/feeds.ts';
import type { Service } from '../../main/api/service.ts';
import { DocumentGate } from '../../main/document.ts';
import { MAX_UPLOAD_BYTES, type Files } from '../../main/files.ts';
import { Ipc, type ContentsLike, type FrameLike, type ProfilesApi } from '../../main/ipc.ts';

function fixture() {
  const main: FrameLike = { frameToken: 'f', parent: null, detached: false, url: 'conch-app://ui/' };
  const contents: ContentsLike = { mainFrame: main, isDestroyed: () => false };
  const gate = new DocumentGate();
  gate.begin();
  gate.commit(main);
  const calls: [string, unknown[]][] = [];
  const files = {
    upload: (...a: unknown[]) => (calls.push(['upload', a]), Promise.resolve({ kind: 'none' })),
    download: (...a: unknown[]) => (calls.push(['download', a]), Promise.resolve({ kind: 'none' })),
    save: (...a: unknown[]) => (calls.push(['save', a]), Promise.resolve('cancelled')),
  } as unknown as Files;
  const input = { recent: false, asked: [] as number[] };
  const logs: string[] = [];
  const ipc = new Ipc({
    owned: () => contents,
    gate,
    service: {} as Service,
    feeds: {} as Feeds,
    files,
    profiles: {} as ProfilesApi,
    gestures: {
      take: (_c, within) => {
        input.asked.push(within);
        return input.recent ? 'pointer' : null;
      },
    },
    now: () => 1,
    log: (l) => logs.push(l),
  });
  const event = { sender: contents, senderFrame: main };
  return { ipc, event, calls, input, logs };
}

const upload = (over: Record<string, unknown> = {}) => ({ channel: 'general', filename: 'a.png', mediaType: 'image/png', data: Buffer.from([1, 2]).toString('base64'), ...over });

describe('the attachment functions', () => {
  it('uploads a well-formed file, parsed into data main shaped', async () => {
    const f = fixture();
    expect((await f.ipc.invoke(f.event, 'uploadFile', [upload()])).ok).toBe(true);
    expect(f.calls).toHaveLength(1);
    expect(f.calls[0]?.[1][0]).toMatchObject({ channel: 'general', filename: 'a.png', mediaType: 'image/png' });
  });

  it('refuses an upload one byte over the cap, before anything runs', async () => {
    const f = fixture();
    const data = Buffer.alloc(MAX_UPLOAD_BYTES + 1).toString('base64');
    expect(await f.ipc.invoke(f.event, 'uploadFile', [upload({ data })])).toEqual({ ok: false, kind: 'refused' });
    expect(f.calls).toEqual([]);
  });

  it('accepts an upload at the cap', async () => {
    const f = fixture();
    const data = Buffer.alloc(MAX_UPLOAD_BYTES).toString('base64');
    expect((await f.ipc.invoke(f.event, 'uploadFile', [upload({ data })])).ok).toBe(true);
  });

  it.each([
    ['an extra key (a URL, a header, a path)', upload({ url: 'https://evil.example/' })],
    ['a path-like channel', upload({ channel: '../x' })],
    ['bytes as a number array', upload({ data: [1, 2, 3] })],
    ['no arguments', undefined],
  ])('refuses an upload with %s', async (_n, arg) => {
    const f = fixture();
    expect(await f.ipc.invoke(f.event, 'uploadFile', arg === undefined ? [] : [arg])).toEqual({ ok: false, kind: 'refused' });
    expect(f.calls).toEqual([]);
  });

  it('downloads by file number only', async () => {
    const f = fixture();
    expect((await f.ipc.invoke(f.event, 'downloadFile', [5])).ok).toBe(true);
    for (const bad of [0, -1, 1.5, '5', null, { id: 5 }, 2 ** 60]) {
      expect(await f.ipc.invoke(f.event, 'downloadFile', [bad])).toEqual({ ok: false, kind: 'refused' });
    }
    expect(f.calls).toHaveLength(1);
  });

  it('saves only right after the person’s own input, and counts the refusal by reason', async () => {
    const f = fixture();
    expect(await f.ipc.invoke(f.event, 'saveFile', [5])).toEqual({ ok: false, kind: 'refused' });
    expect(f.calls).toEqual([]);
    expect(f.logs).toEqual(['conch-desktop: refused an IPC call (gesture); 1 so far']);
    f.input.recent = true;
    expect(await f.ipc.invoke(f.event, 'saveFile', [5])).toEqual({ ok: true, value: 'cancelled' });
    expect(f.calls).toEqual([['save', [5]]]);
  });

  it('cannot be given a path to save to: any second argument or a string is a bad shape', async () => {
    const f = fixture();
    f.input.recent = true;
    for (const args of [[5, '/home/me/.bashrc'], ['/home/me/.bashrc'], [{ id: 5, path: '/x' }], []]) {
      expect(await f.ipc.invoke(f.event, 'saveFile', args)).toEqual({ ok: false, kind: 'refused' });
    }
    expect(f.calls).toEqual([]);
  });

  it('does not ask for gesture on the shape check: a bad shape is refused as a shape', async () => {
    const f = fixture();
    await f.ipc.invoke(f.event, 'saveFile', ['x']);
    expect(f.logs[0]).toContain('(shape)');
  });
});
