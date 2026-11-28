import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { listChannels, restore } from '../api';
import { BlocksProvider, type Blocks } from '../blocks';
import { DmInbox } from './DmInbox';
import type { DMMessageV1, DMReadStateV1, ListDMMessagesResponseV1 } from '../schema/generated';
import { dmApi, type DmApi } from './api';
import { DmProvider } from './DmContext';
import { DmRoute } from './DmRoute';
import { DmTranscript } from './DmTranscript';
import { removedBody } from './model';
import { conversation, fake, fixture, later, me, mockOperationAvailability, ownMessage, peerMessage, removedMessage, snapshot, type Fake } from './testkit';

const scope = 'https://a.example|3';

function mount(f: Fake, id = 12, draft = '', where = scope) {
  const typed: string[] = [];
  const view = (conversationId: number, s: string) => (
    <DmTranscript conversationId={conversationId} api={f.api} scope={s} viewer={me} draft={draft} onDraft={(_c, text) => { typed.push(text); }} />
  );
  const result = render(view(id, where));
  return { ...result, typed, show: (conversationId: number, s = where) => { result.rerender(view(conversationId, s)); } };
}

const ready = () => screen.findByRole('log', { name: 'Messages' });
const bodies = () => within(screen.getByRole('list', { name: /oldest first/ })).getAllByRole('listitem').map((li) => li.querySelector('.dm-body')?.textContent);

beforeEach(() => {
  Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
});
afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

describe('the transcript', () => {
  it('shows the peer and the history oldest first, each message under its author', async () => {
    const f = fake();
    mount(f);
    await ready();
    expect(screen.getByRole('heading', { name: 'Grace' })).toBeDefined();
    expect(bodies()).toEqual(['Sure. Here is the draft.', 'Thanks, reading it now.']);
    const rows = screen.getAllByRole('listitem');
    expect(rows[0]?.textContent).toContain('You');
    expect(rows[1]?.textContent).toContain('Grace');
  });

  it('keeps the name of a historical author as the conversation holds it, and says Former member when it holds none', async () => {
    const f = fake({ get: vi.fn(() => Promise.resolve({ ok: true as const, value: { ...conversation(), peer_name: undefined } })) });
    mount(f);
    await ready();
    expect(screen.getByRole('heading', { name: 'Former member' })).toBeDefined();
    expect(screen.getAllByRole('listitem')[1]?.textContent).toContain('Former member');
  });

  it('pages older history and merges it without a duplicate', async () => {
    const older = { ...peerMessage(), id: 4300, revision: 1, body: 'Earlier' };
    const history = vi.fn((_c: number, q: { before?: number }) =>
      Promise.resolve({ ok: true as const, value: q.before === undefined ? fixture<never>('list-dm-messages-response-v1') : { order: 'desc' as const, messages: [older, ownMessage()] } }),
    );
    const f = fake({ history: history as unknown as DmApi['history'] });
    mount(f);
    await ready();
    fireEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }));
    await waitFor(() => { expect(bodies()).toEqual(['Earlier', 'Sure. Here is the draft.', 'Thanks, reading it now.']); });
    expect(history).toHaveBeenLastCalledWith(12, { order: 'desc', before: 4305 });
    expect(screen.queryByRole('button', { name: 'Load earlier messages' })).toBeNull();
  });

  it('does not scroll to the newest message when loading earlier history', async () => {
    const earlier = { ...peerMessage(), id: 4300, revision: 1, body: 'Earlier' };
    const history = vi.fn<DmApi['history']>()
      .mockResolvedValueOnce({ ok: true, value: { order: 'desc', messages: [peerMessage()], next_before: 4310 } })
      .mockResolvedValueOnce({ ok: true, value: { order: 'desc', messages: [earlier] } });
    mount(fake({ history }));
    const log = await ready();
    const scrollTo = vi.fn();
    Object.defineProperty(log, 'scrollTo', { value: scrollTo });
    fireEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }));
    await screen.findByText('Earlier');
    expect(scrollTo).not.toHaveBeenCalled();
  });

  it('moves focus into the last page of history when the focused paging button disappears', async () => {
    mount(fake({ history: vi.fn<DmApi['history']>()
      .mockResolvedValueOnce({ ok: true, value: { order: 'desc', messages: [ownMessage()], next_before: 4305 } })
      .mockResolvedValueOnce({ ok: true, value: { order: 'desc', messages: [{ ...peerMessage(), body: 'Earlier' }] } }),
    }));
    await ready();
    const older = screen.getByRole('button', { name: 'Load earlier messages' });
    older.focus();
    fireEvent.click(older);
    const result = await screen.findByText('Earlier');
    expect(document.activeElement).toBe(result.closest('li'));
  });

  it('retries a transient read-mark failure after a successful poll with no new messages', async () => {
    const markRead = vi.fn<DmApi['markRead']>()
      .mockResolvedValueOnce({ ok: false, failure: 'network' })
      .mockResolvedValue({ ok: true, value: { ...fixture<DMReadStateV1>('dm-read-state-v1'), last_read_id: 4310 } });
    const f = fake({ markRead });
    mount(f);
    await ready();
    expect(markRead.mock.calls).toEqual([[12, 4310]]);
    await act(async () => {
      f.frames({ type: 'poll' });
      await Promise.resolve();
    });
    expect(markRead.mock.calls).toEqual([[12, 4310], [12, 4310]]);
    await act(async () => {
      f.frames({ type: 'poll' });
      await Promise.resolve();
    });
    expect(markRead).toHaveBeenCalledTimes(2);
  });

  it('keeps history and removal after a local block while hiding write controls and Retry', async () => {
    const f = fake({ send: vi.fn<DmApi['send']>().mockResolvedValue({ ok: false, failure: 'unconfirmed' }) });
    const view = (ids: number[]) => {
      const blocks: Blocks = {
        blocked: new Set(ids), list: [], loaded: true,
        block: () => Promise.resolve(false), unblock: () => Promise.resolve(false), signal: () => undefined,
      };
      return (
        <BlocksProvider value={blocks}>
          <DmTranscript conversationId={12} api={f.api} scope={scope} viewer={me} draft="hello" onDraft={() => undefined} />
        </BlocksProvider>
      );
    };
    const { rerender } = render(view([]));
    await ready();
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByRole('button', { name: 'Retry' });
    rerender(view([conversation().peer_id]));
    expect(screen.queryByRole('button', { name: 'Send' })).toBeNull();
    expect(screen.queryByRole('button', { name: /^Edit your message/ })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    expect(screen.getByRole('button', { name: /^Remove your message/ })).toBeDefined();
    expect(bodies()).toContain('Thanks, reading it now.');
  });

  it('never offers Retry for a definitively refused send after reopening the transcript', async () => {
    const f = fake({ send: vi.fn<DmApi['send']>().mockResolvedValue({ ok: false, failure: 'unavailable' }) });
    const view = (id: number) => <DmProvider api={f.api} scope={scope} me={me}><DmRoute conversationId={id} /></DmProvider>;
    const { rerender } = render(view(12));
    await ready();
    fireEvent.change(screen.getByRole('textbox', { name: 'Message to Grace' }), { target: { value: 'hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByRole('button', { name: 'Discard' });
    rerender(view(13));
    rerender(view(12));
    await screen.findByRole('button', { name: 'Discard' });
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
  });

  it('queues a DM with a secure nonce when randomUUID is unavailable on plain HTTP', async () => {
    vi.stubGlobal('crypto', { getRandomValues: globalThis.crypto.getRandomValues.bind(globalThis.crypto) });
    try {
      const f = fake();
      mount(f, 12, 'hello');
      await ready();
      fireEvent.click(screen.getByRole('button', { name: 'Send' }));
      const sent = vi.mocked(f.api.send).mock.calls[0];
      expect(sent?.[0]).toBe(12);
      expect(sent?.[1].body).toBe('hello');
      expect(sent?.[1].client_nonce).toMatch(/^[A-Za-z0-9_-]{32,}$/);
      await act(async () => {});
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it('uses the native save dialog for a DM file when available', async () => {
    mockOperationAvailability({ dmFileMeta: true, dmFileBytes: true });
    const saveFile = vi.fn(() => Promise.resolve({ ok: true as const, value: null }));
    const base = fake({ fileBytes: vi.fn<DmApi['fileBytes']>().mockResolvedValue({ ok: false, failure: 'network' }) });
    mount({ ...base, api: { ...base.api, saveFile } });
    const download = await screen.findByRole('button', { name: /^Download / });
    fireEvent.click(download);
    expect(saveFile).toHaveBeenCalledExactlyOnceWith(12, 41);
    expect(base.api.fileBytes).not.toHaveBeenCalled();
    await act(async () => {});
  });

  it('focuses the message editor or safe removal answer and restores the opening control on Escape', async () => {
    mount(fake());
    await ready();
    const edit = screen.getByRole('button', { name: /^Edit your message/ });
    edit.focus();
    fireEvent.click(edit);
    const box = screen.getByRole('textbox', { name: 'Edit message' });
    expect(document.activeElement).toBe(box);
    fireEvent.keyDown(box, { key: 'Escape' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: /^Edit your message/ }));
    fireEvent.click(screen.getByRole('button', { name: /^Remove your message/ }));
    const keep = screen.getByRole('button', { name: 'Keep' });
    expect(document.activeElement).toBe(keep);
    fireEvent.keyDown(keep, { key: 'Escape' });
    expect(document.activeElement).toBe(screen.getByRole('button', { name: /^Remove your message/ }));
  });

  it('refuses a whitespace-only message as channels do, and sends text with its nonce', async () => {
    const f = fake();
    const { typed } = mount(f);
    await ready();
    const box = screen.getByRole('textbox', { name: 'Message to Grace' });
    fireEvent.change(box, { target: { value: '   ' } });
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Send' }).disabled).toBe(true);
    fireEvent.keyDown(box, { key: 'Enter' });
    expect(f.api.send).not.toHaveBeenCalled();
    fireEvent.change(box, { target: { value: 'hello' } });
    fireEvent.keyDown(box, { key: 'Enter' });
    await waitFor(() => { expect(f.api.send).toHaveBeenCalledTimes(1); });
    const request = vi.mocked(f.api.send).mock.calls[0]?.[1];
    expect(request?.body).toBe('hello');
    expect(request?.client_nonce).toMatch(/^[A-Za-z0-9_-]+$/);
    expect(typed.at(-1)).toBe('');
  });

  it('shows one row when the live copy arrives before the send’s own answer (concurrent send and reconnect)', async () => {
    const answer = later<{ ok: true; message: DMMessageV1 }>();
    const mine = { ...ownMessage(), id: 4311, revision: 7, file_ids: undefined, body: 'hello' };
    const f = fake({ send: vi.fn(() => answer.promise) as unknown as DmApi['send'] });
    mount(f);
    await ready();
    fireEvent.change(screen.getByRole('textbox', { name: 'Message to Grace' }), { target: { value: 'hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    const nonce = vi.mocked(f.api.send).mock.calls[0]?.[1].client_nonce;
    expect(screen.getByText('Sending…')).toBeDefined();
    // The socket comes back with the author's copy first.
    act(() => { f.frames({ type: 'message', message: { ...mine, client_nonce: nonce } }); });
    expect(screen.queryByText('Sending…')).toBeNull();
    await act(async () => { answer.resolve({ ok: true, message: { ...mine, client_nonce: nonce } }); await answer.promise; });
    expect(bodies().filter((b) => b === 'hello')).toHaveLength(1);
  });

  it('keeps an unconfirmed send with Retry, which sends the same nonce again', async () => {
    const send = vi.fn<DmApi['send']>()
      .mockResolvedValueOnce({ ok: false, failure: 'unconfirmed' })
      .mockResolvedValueOnce({ ok: true, message: { ...ownMessage(), id: 4311, revision: 7, body: 'hello' } });
    const f = fake({ send });
    mount(f);
    await ready();
    fireEvent.change(screen.getByRole('textbox', { name: 'Message to Grace' }), { target: { value: 'hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect((await screen.findByRole('alert')).textContent).toContain('Not confirmed');
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => { expect(send).toHaveBeenCalledTimes(2); });
    expect(send.mock.calls[1]?.[1].client_nonce).toBe(send.mock.calls[0]?.[1].client_nonce);
    await waitFor(() => { expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull(); });
    expect(bodies().filter((b) => b === 'hello')).toHaveLength(1);
  });

  it('keeps the send and its attachments when the reply arrives after switching routes', async () => {
    mockOperationAvailability({ uploadDmFile: true });
    const answer = later<Awaited<ReturnType<DmApi['send']>>>();
    const send = vi.fn<DmApi['send']>()
      .mockReturnValueOnce(answer.promise)
      .mockResolvedValueOnce({ ok: true, message: { ...ownMessage(), id: 4311, revision: 7, body: 'draft.pdf' } });
    const f = fake({ send });
    const page = (id: number) => <DmProvider api={f.api} scope={scope} me={me}><DmRoute conversationId={id} /></DmProvider>;
    const { rerender } = render(page(12));
    await ready();
    fireEvent.change(screen.getByLabelText('Attach a file'), { target: { files: [new File(['x'], 'draft.pdf')] } });
    await screen.findByRole('list', { name: 'Attached files' });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    rerender(page(14));
    await ready();
    await act(async () => { answer.resolve({ ok: false, failure: 'unconfirmed' }); await answer.promise; });
    expect(screen.queryByRole('button', { name: 'Retry' })).toBeNull();
    rerender(page(12));
    fireEvent.click(await screen.findByRole('button', { name: 'Retry' }));
    await waitFor(() => { expect(send).toHaveBeenCalledTimes(2); });
    expect(send.mock.calls[1]).toEqual(send.mock.calls[0]);
    expect(send.mock.calls[1]?.[1].file_ids).toEqual([41]);
  });

  it('says one generic line when the peer cannot be reached, whatever the reason, and stops offering the composer', async () => {
    const f = fake({ send: vi.fn(() => Promise.resolve({ ok: false as const, failure: 'unavailable' as const })) });
    mount(f);
    await ready();
    fireEvent.change(screen.getByRole('textbox', { name: 'Message to Grace' }), { target: { value: 'hi' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => { expect(screen.queryByRole('textbox', { name: 'Message to Grace' })).toBeNull(); });
    const lines = screen.getAllByText("Can't send a message to this person");
    expect(lines.length).toBeGreaterThan(0);
    expect(document.body.textContent).not.toMatch(/block|preference|disabled|switch/i);
    // History stays readable.
    expect(bodies()).toContain('Thanks, reading it now.');
  });

  it.each([true, false])('restores the composer after the peer unblocks the viewer (peer replied: %s)', async (replied) => {
    const reply = { ...peerMessage(), id: 4311, revision: 7, body: 'Ready to talk again' };
    const sent = { ...ownMessage(), id: 4312, revision: replied ? 8 : 7, body: 'Welcome back' };
    const f = fake({
      changes: vi.fn<DmApi['changes']>().mockResolvedValue({ ok: true, value: { messages: replied ? [reply] : [] } }),
      send: vi.fn<DmApi['send']>()
        .mockResolvedValueOnce({ ok: false, failure: 'unavailable' })
        .mockResolvedValueOnce({ ok: true, message: sent }),
    });
    mount(f, 12, 'Hello');
    const log = await ready();
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByRole('button', { name: 'Discard' });
    expect(screen.queryByRole('textbox', { name: 'Message to Grace' })).toBeNull();

    await act(async () => {
      f.frames({ type: 'poll' });
      await Promise.resolve();
    });
    expect(bodies().includes(reply.body)).toBe(replied);
    expect(screen.getByRole('log', { name: 'Messages' })).toBe(log);
    fireEvent.change(screen.getByRole('textbox', { name: 'Message to Grace' }), { target: { value: sent.body } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => { expect(bodies()).toContain(sent.body); });
  });

  it.each([true, false])('restores the composer when the snapshot reports DMs back on (polled while off: %s)', async (polledWhileOff) => {
    let enabled = true;
    const f = fake({
      snapshot: vi.fn<DmApi['snapshot']>(() => Promise.resolve({ ok: true, value: { ...snapshot(), dms_enabled: enabled } })),
      send: vi.fn<DmApi['send']>().mockResolvedValue({ ok: false, failure: 'dms_off' }),
    });
    render(<DmProvider api={f.api} scope={scope} me={me}><DmRoute conversationId={12} /></DmProvider>);
    const log = await ready();
    enabled = false;
    fireEvent.change(screen.getByRole('textbox', { name: 'Message to Grace' }), { target: { value: 'Hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByText('Direct messages are off.', { selector: '.banner' });
    expect(screen.queryByRole('textbox', { name: 'Message to Grace' })).toBeNull();
    if (polledWhileOff) {
      await act(async () => {
        f.frames({ type: 'poll' });
        await Promise.resolve();
      });
      expect(screen.queryByRole('textbox', { name: 'Message to Grace' })).toBeNull();
    }

    enabled = true;
    await act(async () => {
      f.frames({ type: 'poll' });
      await Promise.resolve();
    });
    expect(screen.queryByText('Direct messages are off.', { selector: '.banner' })).toBeNull();
    expect(screen.getByRole('textbox', { name: 'Message to Grace' })).toBeDefined();
    expect(screen.getByRole('log', { name: 'Messages' })).toBe(log);
  });

  it.each(['snapshot', 'list', 'history', 'read mark'] as const)('recovers on the next refresh after a stalled %s', async (kind) => {
    const stalled = later<Response>();
    const calls = vi.fn<typeof fetch>();
    const row = conversation();
    const mark = { ...fixture<DMReadStateV1>('dm-read-state-v1'), last_read_id: row.last_message_id, unread: 0 };
    let snapshots = 0;
    let reads = 0;
    let recovering = false;
    let enabled = true;
    const fetchDm: typeof fetch = (path, init) => {
      const url = new URL(path instanceof Request ? path.url : path, window.location.href);
      switch (`${init?.method ?? 'GET'} ${url.pathname}`) {
        case 'GET /v1/whoami':
          return Promise.resolve(Response.json({ id: me, kind: 'human', name: 'Ada', role: 'member' }));
        case 'GET /v1/dms/snapshot':
          snapshots++;
          if (kind === 'snapshot' && snapshots === 2) return stalled.promise;
          return Promise.resolve(Response.json({ ...snapshot(), dms_enabled: enabled, conversations: kind === 'list' ? [] : [row], more: kind === 'list' }));
        case 'GET /v1/dms':
          if (!recovering) return stalled.promise;
          return Promise.resolve(Response.json({ conversations: url.searchParams.get('state') === 'open' ? [row] : [] }));
        case 'GET /v1/dms/12':
          return Promise.resolve(Response.json(row));
        case 'GET /v1/dms/12/messages':
          if (kind === 'history' && !recovering) return stalled.promise;
          return Promise.resolve(Response.json(fixture<ListDMMessagesResponseV1>('list-dm-messages-response-v1')));
        case 'GET /v1/dms/12/changes':
          return Promise.resolve(Response.json({ messages: [] }));
        case 'PUT /v1/dms/12/read-state':
          if (kind === 'read mark' && ++reads === 1) return stalled.promise;
          return Promise.resolve(Response.json(mark));
        case 'POST /v1/dms/12/messages':
          return Promise.resolve(Response.json({ code: 'dms_off', message: 'Direct messages are off.' }, { status: 403 }));
        default:
          throw new Error(`Unexpected request: ${url.pathname}`);
      }
    };
    vi.stubGlobal('fetch', calls.mockImplementation(fetchDm));
    window.sessionStorage.setItem('conch.web.token', 'conch_member_secret');
    try {
      expect((await restore()).ok).toBe(true);
      vi.useFakeTimers();
      await act(async () => {
        render(<DmProvider api={dmApi} scope={scope} me={me}><DmInbox /><DmRoute conversationId={12} /></DmProvider>);
        await Promise.resolve();
      });
      if (kind === 'snapshot') {
        enabled = false;
        fireEvent.change(screen.getByRole('textbox', { name: 'Message to Grace' }), { target: { value: 'Hello' } });
        await act(async () => {
          fireEvent.click(screen.getByRole('button', { name: 'Send' }));
          await Promise.resolve();
        });
        expect(screen.queryByRole('textbox', { name: 'Message to Grace' })).toBeNull();
        await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
        expect(snapshots).toBe(2);
      }

      recovering = true;
      enabled = true;
      // Inbox refreshes already queued by focus should run when the deadline
      // expires, even before the next interval tick.
      fireEvent.focus(window);
      await act(async () => { await vi.advanceTimersByTimeAsync(kind === 'snapshot' || kind === 'list' ? 10_000 : 15_000); });

      if (kind === 'list') expect(screen.getByRole('button', { name: /^Grace/ })).toBeDefined();
      if (kind === 'history') expect(screen.getByRole('log', { name: 'Messages' })).toBeDefined();
      if (kind === 'read mark') {
        const readCalls = calls.mock.calls.filter(([path]) => path === '/v1/dms/12/read-state');
        expect(readCalls.map(([, init]) => init?.body)).toEqual(Array(2).fill(JSON.stringify({ last_read_id: mark.last_read_id })));
      }
      if (kind === 'snapshot') {
        expect(screen.getByRole('textbox', { name: 'Message to Grace' })).toBeDefined();
        expect(screen.getByText('Not sent: direct messages were off.')).toBeDefined();
        await act(async () => {
          stalled.resolve(Response.json({ ...snapshot(), dms_enabled: false }));
          await stalled.promise;
        });
        expect(screen.getByRole('textbox', { name: 'Message to Grace' })).toBeDefined();
      }
    } finally {
      cleanup();
      vi.useRealTimers();
      vi.unstubAllGlobals();
      window.sessionStorage.clear();
      await listChannels();
    }
  });

  it('edits and removes only the viewer’s own messages, and shows the server’s tombstone', async () => {
    const own = ownMessage();
    const gone = { ...own, revision: 8, removed: true, body: removedBody, file_ids: undefined };
    const changes = vi.fn(() => Promise.resolve({ ok: true as const, value: { messages: [gone] } }));
    const f = fake({ changes });
    mount(f);
    await ready();
    expect(screen.getAllByRole('button', { name: /^Edit your message/ })).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: /^Edit your message/ }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Edit message' }), { target: { value: 'Sure. Draft attached.' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => { expect(f.api.edit).toHaveBeenCalledWith(12, 4305, 'Sure. Draft attached.'); });
    fireEvent.click(await screen.findByRole('button', { name: /^Remove your message/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Yes, remove' }));
    await waitFor(() => { expect(f.api.remove).toHaveBeenCalledWith(12, 4305); });
    await waitFor(() => { expect(screen.getByText(removedBody)).toBeDefined(); });
    expect(screen.queryByRole('button', { name: /^Remove your message/ })).toBeNull();
  });

  it('shows the tombstone the server holds when the conversation is read again after a reconnect', async () => {
    const history = { order: 'desc' as const, messages: [removedMessage(), peerMessage()].sort((a, b) => b.id - a.id) };
    const f = fake({ history: vi.fn(() => Promise.resolve({ ok: true as const, value: history })) });
    mount(f);
    await ready();
    expect(screen.getByText(removedBody)).toBeDefined();
  });

  it('applies a polled edit and replaces a removed message on an earlier loaded page', async () => {
    const earlier = { ...peerMessage(), id: 4300, revision: 3, body: 'An earlier message' };
    const removed = { ...earlier, revision: 7, removed: true, body: removedBody };
    const edited = { ...peerMessage(), revision: 8, body: 'An edited reply' };
    const latest = { order: 'desc' as const, messages: [peerMessage()], next_before: 4310 };
    const history = vi.fn<DmApi['history']>()
      .mockResolvedValueOnce({ ok: true, value: latest })
      .mockResolvedValueOnce({ ok: true, value: { order: 'desc', messages: [earlier] } })
      .mockResolvedValue({ ok: true, value: { ...latest, messages: [edited] } });
    const changes = vi.fn<DmApi['changes']>().mockResolvedValue({ ok: true, value: { messages: [removed, edited] } });
    const f = fake({ history, changes });
    mount(f);
    const log = await ready();
    fireEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }));
    await screen.findByText(earlier.body);
    await act(async () => {
      f.frames({ type: 'poll' });
      await Promise.resolve();
    });
    expect(screen.getByRole('log', { name: 'Messages' })).toBe(log);
    expect(bodies()).toEqual([removedBody, edited.body]);
    expect(changes).toHaveBeenCalledExactlyOnceWith(12, 6);
    expect(history).toHaveBeenCalledTimes(2);
    expect(screen.queryByRole('button', { name: 'Load earlier messages' })).toBeNull();
  });

  it('catches all 51 replies received in the background and can still load earlier history', async () => {
    const initial = fixture<ListDMMessagesResponseV1>('list-dm-messages-response-v1');
    const initialBodies = [...initial.messages].reverse().map((message) => message.body);
    const earlier = { ...peerMessage(), id: 4300, revision: 3, body: 'Earlier history' };
    const replies = Array.from({ length: 51 }, (_, i) => ({ ...peerMessage(), id: 4311 + i, revision: 7 + i, body: `Reply ${String(i + 1)}` }));
    const newestPage = replies.slice(-50).reverse();
    let inBackground = false;
    const history = vi.fn<DmApi['history']>().mockImplementation((_id, { before }) => Promise.resolve({
      ok: true,
      value: before !== undefined
        ? { order: 'desc', messages: [earlier] }
        : inBackground ? { order: 'desc', messages: newestPage, next_before: newestPage.at(-1)?.id } : initial,
    }));
    const changes = vi.fn<DmApi['changes']>()
      .mockResolvedValueOnce({ ok: true, value: { messages: replies.slice(0, 50), next_after: 56 } })
      .mockResolvedValueOnce({ ok: true, value: { messages: replies.slice(50) } });
    const f = fake({ history, changes });
    mount(f);
    await ready();
    Object.defineProperty(document, 'visibilityState', { value: 'hidden', configurable: true });
    fireEvent(document, new Event('visibilitychange'));
    inBackground = true;
    Object.defineProperty(document, 'visibilityState', { value: 'visible', configurable: true });
    await act(async () => {
      fireEvent(document, new Event('visibilitychange'));
      await Promise.resolve();
    });
    const caughtUp = [...initialBodies, ...replies.map((reply) => reply.body)];
    expect(bodies()).toEqual(caughtUp);
    expect(changes.mock.calls).toEqual([[12, 6], [12, 56]]);
    expect(history).toHaveBeenCalledExactlyOnceWith(12, { order: 'desc' });
    fireEvent.click(screen.getByRole('button', { name: 'Load earlier messages' }));
    await screen.findByText(earlier.body);
    expect(history).toHaveBeenLastCalledWith(12, { order: 'desc', before: 4305 });
    expect(bodies()).toEqual([earlier.body, ...caughtUp]);
    expect(screen.queryByRole('button', { name: 'Load earlier messages' })).toBeNull();
  });

  it.each([
    ['Search', () => screen.queryByRole('button', { name: 'Search' })],
    ['Attach a file', () => screen.queryByLabelText('Attach a file')],
    ['Download', () => screen.queryByRole('button', { name: 'Download draft.pdf' })],
  ] as const)('hides %s while its route is unserved', async (_label, query) => {
    mount(fake());
    await ready();
    expect(screen.getByRole('textbox', { name: 'Message to Grace' })).toBeDefined();
    expect(query()).toBeNull();
  });

  it('marks the newest message read, once', async () => {
    const f = fake();
    mount(f);
    await ready();
    await waitFor(() => { expect(f.api.markRead).toHaveBeenCalledWith(12, 4310); });
    expect(f.api.markRead).toHaveBeenCalledTimes(1);
  });

  it('reads files and searches through the conversation’s own endpoints', async () => {
    mockOperationAvailability({ searchDmMessages: true, dmFileMeta: true, dmFileBytes: true });
    const f = fake();
    mount(f);
    await ready();
    expect(await screen.findByRole('button', { name: 'Download draft.pdf' })).toBeDefined();
    expect(f.api.fileMeta).toHaveBeenCalledWith(12, 41);
    fireEvent.click(screen.getByRole('button', { name: 'Search' }));
    fireEvent.change(screen.getByRole('searchbox', { name: 'Search this conversation' }), { target: { value: 'draft' } });
    fireEvent.click(within(screen.getByRole('search')).getByRole('button', { name: 'Search' }));
    expect(await screen.findByRole('list', { name: 'Search results' })).toBeDefined();
    expect(f.api.search).toHaveBeenCalledWith(12, 'draft', undefined);
  });

  it('attaches an uploaded file by its id', async () => {
    mockOperationAvailability({ uploadDmFile: true });
    const f = fake();
    mount(f);
    await ready();
    const input = screen.getByLabelText('Attach a file');
    fireEvent.change(input, { target: { files: [new File(['x'], 'draft.pdf', { type: 'application/pdf' })] } });
    await screen.findByRole('list', { name: 'Attached files' });
    expect(f.api.uploadFile).toHaveBeenCalledWith(12, 'draft.pdf', 'application/pdf', expect.any(File), expect.any(AbortSignal));
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => { expect(f.api.send).toHaveBeenCalled(); });
    expect(vi.mocked(f.api.send).mock.calls[0]?.[1].file_ids).toEqual([41]);
  });

  it('says it is no longer available when the server answers not found, with no composer', async () => {
    const f = fake({ get: vi.fn(() => Promise.resolve({ ok: false as const, failure: 'not_found' as const })), history: vi.fn(() => Promise.resolve({ ok: false as const, failure: 'not_found' as const })) });
    mount(f);
    expect((await screen.findByRole('alert')).textContent).toContain('no longer available');
    expect(screen.queryByRole('textbox')).toBeNull();
  });

  it('clears displayed history and composer when a refresh loses authorization', async () => {
    const changes = vi.fn<DmApi['changes']>().mockResolvedValue({ ok: false, failure: 'unauthorized' });
    const f = fake({ changes });
    mount(f);
    await ready();
    act(() => { f.frames({ type: 'poll' }); });
    expect((await screen.findByRole('alert')).textContent).toContain('no longer available');
    expect(screen.queryByRole('log')).toBeNull();
    expect(screen.queryByRole('textbox')).toBeNull();
  });

  it('offers Try again when the load fails', async () => {
    const history = vi.fn()
      .mockResolvedValueOnce({ ok: false, failure: 'network' })
      .mockResolvedValue({ ok: true, value: fixture('list-dm-messages-response-v1') });
    const f = fake({ history: history as unknown as DmApi['history'] });
    mount(f);
    fireEvent.click(await screen.findByRole('button', { name: 'Try again' }));
    await ready();
    expect(bodies()).toHaveLength(2);
  });

  it('drops an answer that arrives after switching conversations', async () => {
    const slow = later<{ ok: true; value: unknown }>();
    const other = { ...conversation(), id: 14, peer_id: 9, peer_name: 'Linus' };
    const f = fake({
      get: vi.fn((id: number) => (id === 12 ? slow.promise : Promise.resolve({ ok: true as const, value: other }))) as unknown as DmApi['get'],
      history: vi.fn((id: number) => (id === 12 ? slow.promise : Promise.resolve({ ok: true as const, value: { order: 'desc' as const, messages: [] } }))) as unknown as DmApi['history'],
    });
    const { show } = mount(f, 12);
    show(14);
    expect(await screen.findByRole('heading', { name: 'Linus' })).toBeDefined();
    await act(async () => {
      slow.resolve({ ok: true, value: { ...conversation(), messages: [peerMessage()], order: 'desc' } });
      await slow.promise;
    });
    expect(screen.getByRole('heading', { name: 'Linus' })).toBeDefined();
    expect(screen.queryByText('Got a minute after standup?')).toBeNull();
    expect(f.api.markRead).not.toHaveBeenCalledWith(12, expect.anything());
  });

  it('shows nothing of the old account when the scope changes under the same conversation id', async () => {
    const f = fake();
    const { show } = mount(f, 12, '', 'https://a.example|3');
    await ready();
    expect(bodies()).toHaveLength(2);
    const slow = later<never>();
    vi.mocked(f.api.history).mockReturnValue(slow.promise);
    vi.mocked(f.api.get).mockReturnValue(slow.promise);
    show(12, 'https://b.example|3');
    expect(screen.queryByText('Thanks, reading it now.')).toBeNull();
    expect(screen.getByText('Loading conversation…')).toBeDefined();
  });

  it('catches up with the changes route when a frame skips a revision', async () => {
    const missed = { ...peerMessage(), id: 4312, revision: 9, body: 'missed one' };
    const f = fake({ changes: vi.fn(() => Promise.resolve({ ok: true as const, value: { messages: [missed] } })) });
    mount(f);
    await ready();
    act(() => { f.frames({ type: 'message', message: { ...peerMessage(), id: 4313, revision: 10, body: 'live one' } }); });
    await waitFor(() => { expect(bodies()).toContain('missed one'); });
    expect(f.api.changes).toHaveBeenCalledWith(12, 6);
    expect(bodies().slice(-2)).toEqual(['missed one', 'live one']);
  });

  it('recovers a missed removal after a higher send response and a failed changes request', async () => {
    const removal = { ...ownMessage(), revision: 7, removed: true, body: removedBody, file_ids: undefined };
    const changes = vi.fn<DmApi['changes']>()
      .mockResolvedValueOnce({ ok: false, failure: 'network' })
      .mockResolvedValueOnce({ ok: true, value: { messages: [removal] } });
    const f = fake({
      changes,
      send: vi.fn<DmApi['send']>().mockResolvedValue({ ok: true, message: { ...ownMessage(), id: 4311, revision: 8, body: 'new message' } }),
    });
    mount(f);
    await ready();
    fireEvent.change(screen.getByRole('textbox', { name: 'Message to Grace' }), { target: { value: 'new message' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await screen.findByText('new message');
    await act(async () => {
      f.frames({ type: 'poll' });
      await Promise.resolve();
    });
    expect(changes.mock.calls).toEqual([[12, 6], [12, 6]]);
    expect(await screen.findByText(removedBody)).toBeDefined();
  });

  it('retries a failed live gap from the same revision', async () => {
    const missed = { ...peerMessage(), id: 4312, revision: 7, body: 'missed one' };
    const changes = vi.fn<DmApi['changes']>()
      .mockResolvedValueOnce({ ok: false, failure: 'network' })
      .mockResolvedValueOnce({ ok: true, value: { messages: [missed] } });
    const f = fake({ changes });
    mount(f);
    await ready();
    await act(async () => {
      f.frames({ type: 'message', message: { ...peerMessage(), id: 4313, revision: 8, body: 'live one' } });
      await Promise.resolve();
    });
    await act(async () => {
      f.frames({ type: 'poll' });
      await Promise.resolve();
    });
    expect(changes.mock.calls).toEqual([[12, 6], [12, 6]]);
    expect(bodies().slice(-2)).toEqual(['missed one', 'live one']);
  });

  it.each(['headers', 'body'])('times out stalled browser changes %s and catches up from the same revision', async (stall) => {
    const reply = { ...peerMessage(), id: 4311, revision: 7, body: 'After reconnecting' };
    const changes = vi.fn<typeof fetch>()
      .mockImplementationOnce(() => stall === 'body'
        ? Promise.resolve(new Response(new ReadableStream(), { status: 403 }))
        : later<Response>().promise)
      .mockResolvedValueOnce(Response.json({ messages: [reply] }))
      .mockResolvedValueOnce(Response.json({ messages: [] }));
    vi.stubGlobal('fetch', vi.fn<typeof fetch>((path, init) => {
      if (path === '/v1/whoami') return Promise.resolve(Response.json({ id: me, kind: 'human', name: 'Ada', role: 'member' }));
      if (typeof path === 'string' && path.startsWith('/v1/dms/12/changes?')) return changes(path, init);
      throw new Error('Unexpected request');
    }));
    window.sessionStorage.setItem('conch.web.token', 'conch_member_secret');
    try {
      expect((await restore()).ok).toBe(true);
      vi.useFakeTimers();
      await act(async () => {
        mount(fake({ changes: dmApi.changes, live: dmApi.live }));
        await Promise.resolve();
      });
      expect(screen.getByRole('log', { name: 'Messages' })).toBeDefined();

      await act(async () => { await vi.advanceTimersByTimeAsync(15_000); });
      expect(changes).toHaveBeenCalledTimes(1);
      if (stall === 'body') {
        // The queued focus refresh can take over as soon as the deadline expires.
        fireEvent.focus(window);
        expect(changes).toHaveBeenCalledTimes(1);
      }
      await act(async () => { await vi.advanceTimersByTimeAsync(stall === 'body' ? 10_000 : 15_000); });
      expect(changes.mock.calls.map(([path]) => path)).toEqual([
        '/v1/dms/12/changes?after=6', '/v1/dms/12/changes?after=6',
      ]);
      expect(changes.mock.calls[0]?.[1]?.signal?.aborted).toBe(true);
      expect(bodies()).toContain(reply.body);

      await act(async () => {
        fireEvent.focus(window);
        await Promise.resolve();
      });
      expect(changes.mock.calls[2]?.[0]).toBe('/v1/dms/12/changes?after=7');
    } finally {
      cleanup();
      vi.useRealTimers();
      vi.unstubAllGlobals();
      window.sessionStorage.clear();
      await listChannels();
    }
  });

  it('ignores a frame for another conversation', async () => {
    const f = fake();
    mount(f);
    await ready();
    act(() => { f.frames({ type: 'message', message: { ...peerMessage(), conversation_id: 99, id: 1, revision: 7, body: 'elsewhere' } }); });
    expect(bodies()).not.toContain('elsewhere');
  });
});
