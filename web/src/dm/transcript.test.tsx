import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { DMMessageV1 } from '../schema/generated';
import type { DmApi } from './api';
import { DmTranscript } from './DmTranscript';
import { removedBody } from './model';
import { conversation, fake, fixture, later, me, ownMessage, peerMessage, removedMessage, type Fake } from './testkit';

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
afterEach(cleanup);

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

  it('marks the newest message read, once', async () => {
    const f = fake();
    mount(f);
    await ready();
    await waitFor(() => { expect(f.api.markRead).toHaveBeenCalledWith(12, 4310); });
    expect(f.api.markRead).toHaveBeenCalledTimes(1);
  });

  it('reads files and searches through the conversation’s own endpoints', async () => {
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

  it('ignores a frame for another conversation', async () => {
    const f = fake();
    mount(f);
    await ready();
    act(() => { f.frames({ type: 'message', message: { ...peerMessage(), conversation_id: 99, id: 1, revision: 7, body: 'elsewhere' } }); });
    expect(bodies()).not.toContain('elsewhere');
  });
});
