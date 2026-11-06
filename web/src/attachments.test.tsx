import { StrictMode } from 'react';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';

const origin = 'http://127.0.0.1:8080';
const secret = 'conch_secret_token_value';
const ticket = 'conch_wst_TICKETTICKETTICKETTICKETTICKETTICKETTICKET';
const at = '2026-10-25T10:00:00Z';
const member = { id: 2, kind: 'human', name: 'ada', role: 'member', voice_allowed: false };
const channels = [{ id: 7, name: 'general', created_at: at }, { id: 8, name: 'ops', created_at: at }];
const NativeURL = URL;
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function png(): Uint8Array<ArrayBuffer> {
  return Uint8Array.from([
    0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
    0, 0, 0, 13, 0x49, 0x48, 0x44, 0x52,
    0, 0, 0, 1, 0, 0, 0, 1,
    8, 6, 0, 0, 0, 0, 0, 0, 0,
  ]);
}

function image(name: string): File {
  return new File([png()], name, { type: 'image/png' });
}

interface SentMessage {
  body: string;
  audience?: unknown;
  payload?: { schema: string; data: { file_ids: number[] } };
}

interface UploadCall {
  channel: string;
  filename: string;
  mediaType: string | null;
  bytes: Blob;
  signal: AbortSignal;
}

function storedMessage(id: number, request: SentMessage, channel_id = 7) {
  return {
    schema: 'conch.message.v2', id, channel_id, author_id: 2, created_at: at, body: request.body,
    ...(request.audience === undefined ? {} : { audience: request.audience }),
    ...(request.payload === undefined ? {} : { payload: request.payload }),
  };
}

class FakeSocket {
  static all: FakeSocket[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: ((event: { code: number }) => void) | null = null;
  constructor(readonly url: string) { FakeSocket.all.push(this); }
  close() {}
  open() { act(() => { this.onopen?.(); }); }
  drop(code: number) { act(() => { this.onclose?.({ code }); }); }
}

interface Server {
  history: Record<string, ReturnType<typeof storedMessage>[]>;
  uploads: UploadCall[];
  posts: { channel: string; request: SentMessage }[];
  upload: (call: UploadCall) => Response | Promise<Response>;
  post: (channel: string, request: SentMessage) => Response | Promise<Response>;
  whoami: () => Response;
  channels: typeof channels;
  // The channel's members as the server lists them; ada alone by default.
  members: { principal_id: number; created_at: string; name: string; kind: string }[];
  nextFile: number;
  nextMessage: number;
}

let server: Server;

function fileMetadata(call: UploadCall, id: number) {
  return {
    schema: 'conch.file.v1', id, channel_id: 7, uploader_id: 2, filename: call.filename,
    media_type: call.mediaType ?? 'application/octet-stream', size: call.bytes.size,
    sha256: 'a'.repeat(64), created_at: at,
  };
}

function install() {
  server = {
    history: { general: [], ops: [] }, uploads: [], posts: [], channels,
    members: [{ principal_id: 2, created_at: at, name: 'ada', kind: 'human' }],
    nextFile: 70, nextMessage: 100,
    upload: (call) => json(fileMetadata(call, server.nextFile++), 201),
    post: (channel, request) => {
      const message = storedMessage(server.nextMessage++, request, channel === 'general' ? 7 : 8);
      server.history[channel]?.push(message);
      return json({ message }, 201);
    },
    whoami: () => json(member),
  };
  vi.stubGlobal('WebSocket', FakeSocket);
  vi.stubGlobal('createImageBitmap', vi.fn(() => Promise.resolve({ width: 1, height: 1, close: vi.fn() })));
  let objectUrl = 1;
  class PreviewURL extends NativeURL {
    static override createObjectURL(): string { return `blob:test-${String(objectUrl++)}`; }
    static override revokeObjectURL(): void {}
  }
  vi.stubGlobal('URL', PreviewURL);
  vi.stubGlobal('fetch', vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
    const url = new NativeURL(typeof input === 'string' ? input : input instanceof NativeURL ? input.href : input.url, origin);
    if (new Headers(init?.headers).get('Authorization') !== `Bearer ${secret}`) return Promise.resolve(json({}, 401));
    const path = url.pathname;
    if (path === '/v1/whoami') return Promise.resolve(server.whoami());
    if (path === '/v1/channels') return Promise.resolve(json({ channels: server.channels }));
    if (path === '/v1/ws-tickets') return Promise.resolve(json({ ticket, expires_at: at }, 201));
    const file = /^\/v2\/files\/(\d+)(\/meta)?$/.exec(path);
    if (file !== null) {
      const id = Number(file[1]);
      const call = server.uploads[id - 70];
      if (call === undefined) return Promise.resolve(json({ code: 'file_not_found' }, 404));
      if (file[2] !== undefined) return Promise.resolve(json(fileMetadata(call, id)));
      return Promise.resolve(new Response(call.bytes, { status: 200, headers: { 'Content-Type': 'application/octet-stream' } }));
    }
    const route = /^\/v[12]\/channels\/([^/]+)\/(members|nets|messages|files)$/.exec(path);
    if (route?.[1] === undefined) return Promise.resolve(json({}, 404));
    const channel = decodeURIComponent(route[1]);
    if (route[2] === 'members') return Promise.resolve(json({ members: server.members }));
    if (route[2] === 'nets') return Promise.resolve(json({ nets: [{ id: 5, channel_id: 7, name: 'alpha', members: [{ principal_id: 2, role: 'member' }], created_at: at }] }));
    if (route[2] === 'files') {
      const call: UploadCall = {
        channel,
        filename: url.searchParams.get('filename') ?? '',
        mediaType: new Headers(init?.headers).get('Content-Type'),
        bytes: init?.body as Blob,
        signal: init?.signal as AbortSignal,
      };
      server.uploads.push(call);
      return Promise.resolve(server.upload(call));
    }
    if (init?.method === 'POST') {
      const request = JSON.parse(init.body as string) as SentMessage;
      server.posts.push({ channel, request });
      return Promise.resolve(server.post(channel, request));
    }
    const order = url.searchParams.get('order') ?? 'asc';
    const limit = Number(url.searchParams.get('limit') ?? '100');
    if (order === 'asc') {
      const after = Number(url.searchParams.get('after') ?? '0');
      const visible = (server.history[channel] ?? []).filter((message) => message.id > after);
      const page = visible.slice(0, limit);
      return Promise.resolve(json({ messages: page, ...(page.length === limit ? { next_after: page.at(-1)?.id ?? after } : {}) }));
    }
    const before = url.searchParams.get('before');
    const visible = (server.history[channel] ?? []).filter((message) => before === null || message.id < Number(before));
    return Promise.resolve(json({ messages: visible.slice(-limit).reverse() }));
  }));
}

async function openChat(onRender?: (view: ReturnType<typeof render>) => void, strictMode = false) {
  window.localStorage.setItem('conch.web.token', secret);
  window.location.hash = '';
  const view = render(strictMode ? <StrictMode><App origin={origin} /></StrictMode> : <App origin={origin} />);
  onRender?.(view);
  await screen.findByRole('log', { name: 'Messages' });
  await waitFor(() => { expect(FakeSocket.all.length).toBeGreaterThan(0); });
  const socket = FakeSocket.all.at(-1) as FakeSocket;
  socket.open();
  await waitFor(() => { expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').disabled).toBe(false); });
  return socket;
}

function choose(...files: File[]) {
  fireEvent.change(screen.getByLabelText('Choose files'), { target: { files } });
}

function clickSend() {
  fireEvent.click(screen.getByRole('button', { name: 'Send' }));
}

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  FakeSocket.all = [];
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('Composer and App attachment sending', () => {
  it('uploads only on Send and posts an image-only filename fallback with its payload', async () => {
    await openChat();
    choose(image('shot.png'));
    await screen.findByText('shot.png');
    expect(server.uploads).toHaveLength(0);
    expect(server.posts).toHaveLength(0);
    clickSend();
    await screen.findByText('Sent');
    expect(server.uploads).toHaveLength(1);
    expect(server.posts).toHaveLength(1);
    expect(server.posts[0]?.request).toEqual({
      body: 'shot.png',
      payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [70] } },
    });
    expect(server.uploads[0]?.mediaType).toBe('image/png');
  });

  it('shows a successful send under StrictMode', async () => {
    await openChat(undefined, true);
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'strict success' } });
    clickSend();
    await screen.findByText('Sent');
    expect(server.posts).toHaveLength(1);
  });

  it('shows a failed send under StrictMode', async () => {
    await openChat(undefined, true);
    server.post = () => json({ code: 'refused' }, 403);
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'strict failure' } });
    clickSend();
    await screen.findByText('Failed');
    expect(server.posts).toHaveLength(1);
  });

  it('preserves caption and ready ids across an upload refusal, then retries only the failed file', async () => {
    await openChat();
    let refuseSecond = true;
    server.upload = (call) => {
      if (call.filename === 'large.png' && refuseSecond) return json({ code: 'file_too_large', message: 'a file may be at most 10485760 bytes' }, 413);
      return json(fileMetadata(call, server.nextFile++), 201);
    };
    choose(image('ready.png'), image('large.png'));
    await screen.findByText('large.png');
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'caption kept' } });
    clickSend();
    await screen.findByText('large.png is too large: this server takes files up to 10 MB.');
    expect(server.posts).toHaveLength(0);
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('caption kept');

    refuseSecond = false;
    fireEvent.click(screen.getByRole('button', { name: 'Retry upload of large.png' }));
    await waitFor(() => { expect(server.uploads).toHaveLength(3); });
    expect(server.posts).toHaveLength(0);
    clickSend();
    await screen.findByText('Sent');
    expect(server.uploads.map((call) => call.filename)).toEqual(['ready.png', 'large.png', 'large.png']);
    expect(server.posts[0]?.request).toEqual({
      body: 'caption kept',
      payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [70, 71] } },
    });
  });

  it('shows honest indeterminate progress, cancels without posting, and retains the draft', async () => {
    await openChat();
    server.upload = (call) => new Promise<Response>((_resolve, reject) => {
      call.signal.addEventListener('abort', () => { reject(new DOMException('cancelled', 'AbortError')); }, { once: true });
    });
    choose(image('cancel.png'));
    await screen.findByText('cancel.png');
    clickSend();
    const progress = await screen.findByRole('progressbar', { name: 'Uploading cancel.png' });
    expect(progress.hasAttribute('value')).toBe(false);
    fireEvent.click(screen.getByRole('button', { name: 'Cancel upload of cancel.png' }));
    await screen.findByText('Upload cancelled.');
    expect(server.posts).toHaveLength(0);
    expect(screen.getByText('cancel.png')).toBeDefined();
    expect(screen.getByRole('button', { name: 'Retry upload of cancel.png' })).toBeDefined();
  });

  it('keeps text and image drafts scoped to the channel and normalized audience', async () => {
    await openChat();
    choose(image('private.png'));
    await screen.findByText('private.png');
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'general words' } });
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByLabelText('Message #ops');
    expect(screen.queryByText('private.png')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    await screen.findByText('private.png');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('general words');
    fireEvent.change(screen.getByLabelText('To:'), { target: { value: 'net:5' } });
    await screen.findByLabelText('Message net alpha');
    expect(screen.queryByText('private.png')).toBeNull();
    fireEvent.change(screen.getByLabelText('To:'), { target: { value: 'channel' } });
    expect(await screen.findByText('private.png')).toBeDefined();
  });

  it('freezes the original audience while uploading even if a target event arrives', async () => {
    await openChat();
    let finish: () => void = () => undefined;
    server.upload = (call) => new Promise<Response>((resolve) => { finish = () => { resolve(json(fileMetadata(call, 70), 201)); }; });
    choose(image('frozen.png'));
    await screen.findByText('frozen.png');
    clickSend();
    await screen.findByRole('progressbar');
    expect(screen.getByLabelText<HTMLSelectElement>('To:').disabled).toBe(true);
    fireEvent.change(screen.getByLabelText('To:'), { target: { value: 'net:5' } });
    await act(async () => { finish(); await Promise.resolve(); });
    await screen.findByText('Sent');
    expect(server.posts[0]?.channel).toBe('general');
    expect(server.posts[0]?.request.audience).toBeUndefined();
  });

  it('drops a late upload completion after logout and never posts it', async () => {
    await openChat();
    let finish: () => void = () => undefined;
    server.upload = (call) => new Promise<Response>((resolve) => { finish = () => { resolve(json(fileMetadata(call, 70), 201)); }; });
    choose(image('late.png'));
    await screen.findByText('late.png');
    clickSend();
    await screen.findByRole('progressbar');
    act(() => {
      window.localStorage.removeItem('conch.web.token');
      window.dispatchEvent(new StorageEvent('storage', { key: 'conch.web.token', oldValue: secret, newValue: null, storageArea: window.localStorage }));
    });
    await screen.findByLabelText('Credential');
    await act(async () => { finish(); await Promise.resolve(); });
    expect(server.posts).toHaveLength(0);
    expect(screen.queryByText('late.png')).toBeNull();
  });

  it('drops a late upload completion after channel access loss', async () => {
    const socket = await openChat();
    let finish: () => void = () => undefined;
    server.upload = (call) => new Promise<Response>((resolve) => { finish = () => { resolve(json(fileMetadata(call, 70), 201)); }; });
    choose(image('revoked.png'));
    await screen.findByText('revoked.png');
    clickSend();
    await screen.findByRole('progressbar');
    server.channels = channels.filter((channel) => channel.name !== 'general');
    socket.drop(1008);
    await screen.findByText('You no longer have access to #general');
    await act(async () => { finish(); await Promise.resolve(); });
    expect(server.posts).toHaveLength(0);
    expect(screen.queryByText('revoked.png')).toBeNull();
  });

  it('retains other channels’ attachment drafts when one channel is lost', async () => {
    await openChat();
    choose(image('general.png'));
    await screen.findByText('general.png');
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByLabelText('Message #ops');
    const socket = FakeSocket.all.at(-1) as FakeSocket;
    socket.open();
    await waitFor(() => { expect(screen.getByLabelText<HTMLTextAreaElement>('Message #ops').disabled).toBe(false); });
    choose(image('ops.png'));
    await screen.findByText('ops.png');
    server.channels = channels.filter((channel) => channel.name !== 'ops');
    socket.drop(1008);
    await screen.findByText('You no longer have access to #ops');
    fireEvent.click(screen.getByRole('button', { name: 'general' }));
    expect(await screen.findByText('general.png')).toBeDefined();
    expect(screen.queryByText('ops.png')).toBeNull();
  });

  it('drops a late upload completion after a server switch', async () => {
    let view: ReturnType<typeof render> | undefined;
    await openChat((rendered) => { view = rendered; });
    let finish: () => void = () => undefined;
    server.upload = (call) => new Promise<Response>((resolve) => { finish = () => { resolve(json(fileMetadata(call, 70), 201)); }; });
    choose(image('elsewhere.png'));
    await screen.findByText('elsewhere.png');
    clickSend();
    await screen.findByRole('progressbar');
    view?.rerender(<App origin="http://127.0.0.1:9090" />);
    await act(async () => { finish(); await Promise.resolve(); });
    expect(server.posts).toHaveLength(0);
    expect(screen.queryByText('elsewhere.png')).toBeNull();
  });

  it('reconciles an uncertain attachment post instead of posting or uploading twice', async () => {
    await openChat();
    server.post = (_channel, request) => {
      server.history.general?.push(storedMessage(100, request));
      return Promise.reject(new TypeError('answer lost'));
    };
    choose(image('perhaps.png'));
    await screen.findByText('perhaps.png');
    clickSend();
    await screen.findByText(/^Not confirmed/);
    expect(server.posts).toHaveLength(1);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.posts).toHaveLength(1);
    expect(server.uploads).toHaveLength(1);
  });

  it('leaves an uncertain post alone when reconciliation fails, then permits an explicit retry', async () => {
    await openChat();
    let failPost = true;
    let failHistory = true;
    server.post = (_channel, request) => {
      if (failPost) return Promise.reject(new TypeError('answer lost'));
      const message = storedMessage(server.nextMessage++, request);
      server.history.general?.push(message);
      return json({ message }, 201);
    };
    const real = globalThis.fetch;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const path = typeof input === 'string' ? input : input instanceof NativeURL ? input.pathname : input.url;
      if (failHistory && path.includes('/messages') && path.includes('general') && new NativeURL(path, origin).searchParams.get('order') === 'asc') return Promise.resolve(json({}, 500));
      return real(input, init);
    });
    choose(image('retry.png'));
    await screen.findByText('retry.png');
    clickSend();
    await screen.findByText(/^Not confirmed/);
    failPost = false;
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText(/^Not confirmed/);
    await waitFor(() => { expect(server.posts).toHaveLength(1); });
    failHistory = false;
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.posts).toHaveLength(2);
    expect(server.uploads).toHaveLength(1);
    expect(server.posts[1]?.request.payload).toEqual(server.posts[0]?.request.payload);
  });

  it('does not repost when reconciliation history is incomplete', async () => {
    await openChat();
    server.post = () => Promise.reject(new TypeError('answer lost'));
    const real = globalThis.fetch;
    let historyCalls = 0;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new NativeURL(typeof input === 'string' ? input : input instanceof NativeURL ? input.href : input.url, origin);
      if (url.pathname === '/v2/channels/general/messages' && url.searchParams.get('order') === 'asc') {
        historyCalls += 1;
        return Promise.resolve(json({ messages: [], next_after: 0 }));
      }
      return real(input, init);
    });
    choose(image('partial.png'));
    await screen.findByText('partial.png');
    clickSend();
    await screen.findByText(/^Not confirmed/);
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() => { expect(historyCalls).toBe(1); });
    await screen.findByText(/^Not confirmed/);
    expect(server.posts).toHaveLength(1);
    expect(server.uploads).toHaveLength(1);
  });

  it('restores a captioned attachment send after session loss and reconciles before reposting', async () => {
    await openChat();
    server.post = () => Promise.reject(new TypeError('answer lost'));
    const real = globalThis.fetch;
    let expireHistory = false;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new NativeURL(typeof input === 'string' ? input : input instanceof NativeURL ? input.href : input.url, origin);
      if (expireHistory && url.pathname === '/v2/channels/general/messages' && url.searchParams.get('order') === 'asc') {
        return Promise.resolve(json({}, 401));
      }
      return real(input, init);
    });
    fireEvent.change(screen.getByLabelText('To:'), { target: { value: 'net:5' } });
    await screen.findByLabelText('Message net alpha');
    choose(image('kept.png'));
    await screen.findByText('kept.png');
    fireEvent.change(screen.getByLabelText('Message net alpha'), { target: { value: 'caption kept' } });
    clickSend();
    await screen.findByText(/^Not confirmed/);
    expect(server.posts).toHaveLength(1);

    expireHistory = true;
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByLabelText('Credential');
    expireHistory = false;
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByRole('log', { name: 'Messages' });
    await screen.findByText('caption kept');
    expect(screen.getByRole('log', { name: 'Messages' }).textContent).toContain('→ net alpha');
    expect(screen.getByText(/^Not confirmed/)).toBeDefined();
    expect(server.posts).toHaveLength(1);
    expect(server.uploads).toHaveLength(1);

    server.history.general = [storedMessage(100, server.posts[0]?.request as SentMessage)];
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.posts).toHaveLength(1);
    expect(server.uploads).toHaveLength(1);
    expect(server.posts[0]?.request).toEqual({
      body: 'caption kept',
      audience: { kind: 'net', net_id: 5 },
      payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [70] } },
    });
  });

  it('restores an initial attachment POST 401 as one pending send, not a text draft', async () => {
    await openChat();
    let expired = true;
    server.post = (_channel, request) => {
      if (expired) return json({}, 401);
      const message = storedMessage(server.nextMessage++, request);
      server.history.general?.push(message);
      return json({ message }, 201);
    };
    choose(image('captioned.png'));
    await screen.findByText('captioned.png');
    fireEvent.change(screen.getByLabelText('Message #general'), { target: { value: 'caption survives 401' } });
    clickSend();
    await screen.findByLabelText('Credential');
    expired = false;
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByRole('log', { name: 'Messages' });
    await screen.findByText('Failed');
    expect(screen.getByRole('log', { name: 'Messages' }).textContent).toContain('caption survives 401');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');
    expect(screen.getAllByRole('button', { name: 'Retry' })).toHaveLength(1);

    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.uploads).toHaveLength(1);
    expect(server.posts).toHaveLength(2);
    expect(server.posts[1]?.request).toEqual(server.posts[0]?.request);
    expect(server.posts[1]?.request.payload).toEqual({ schema: 'conch.file_attachment.v1', data: { file_ids: [70] } });
  });

  it('preserves an image-only pending send and file id when its retry POST returns 401', async () => {
    await openChat();
    let initial = true;
    let expired = false;
    server.post = (_channel, request) => {
      if (initial) return Promise.reject(new TypeError('answer lost'));
      if (expired) return json({}, 401);
      const message = storedMessage(server.nextMessage++, request);
      server.history.general?.push(message);
      return json({ message }, 201);
    };
    choose(image('only.png'));
    await screen.findByText('only.png');
    clickSend();
    await screen.findByText(/^Not confirmed/);
    initial = false;
    expired = true;
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByLabelText('Credential');
    expired = false;
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByRole('log', { name: 'Messages' });
    await screen.findByText('Failed');
    expect(screen.getByRole('log', { name: 'Messages' }).textContent).toContain('only.png');
    expect(screen.getByLabelText<HTMLTextAreaElement>('Message #general').value).toBe('');

    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await screen.findByText('Sent');
    expect(server.uploads).toHaveLength(1);
    expect(server.posts).toHaveLength(3);
    expect(server.posts[2]?.request).toEqual({ body: 'only.png', payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [70] } } });
  });
});

// Issue #1066: an image chosen, pasted or dropped for the next whisper makes
// it a newer draft at once, so the answer to the whisper sent before it can
// never end its audience and send the image to the whole channel.
describe('images for a newer whisper while the last is on its way (issue #1066)', () => {
  const toBot = { kind: 'principals', principal_ids: [3] };
  const to = () => screen.getByLabelText<HTMLSelectElement>('To:');
  const ways: [string, (file: File) => void][] = [
    ['chosen', (file) => { choose(file); }],
    ['pasted', (file) => { fireEvent.paste(screen.getByLabelText('Whisper to bot'), { clipboardData: { files: [file] } }); }],
    ['dropped', (file) => { fireEvent.drop(document.querySelector('form.composer') as HTMLFormElement, { dataTransfer: { files: [file], types: ['Files'] } }); }],
  ];
  it.each(ways)('an image %s keeps the whisper it was added to', async (_how, add) => {
    server.members = [
      { principal_id: 2, created_at: at, name: 'ada', kind: 'human' },
      { principal_id: 3, created_at: at, name: 'bot', kind: 'agent' },
    ];
    await openChat();
    await screen.findByRole('option', { name: 'net alpha' });
    fireEvent.change(to(), { target: { value: 'whisper' } });
    fireEvent.click(await screen.findByRole('checkbox', { name: 'bot [agent]' }));
    fireEvent.change(screen.getByLabelText('Whisper to bot'), { target: { value: 'a' } });
    let answer: (response: Response) => void = () => undefined;
    const accept = server.post;
    server.post = () =>
      new Promise<Response>((resolve) => {
        answer = resolve;
      });
    clickSend();
    await waitFor(() => { expect(server.posts).toHaveLength(1); });
    // Nothing typed: only an image for the next whisper.
    add(image('next.png'));
    await screen.findByText('next.png');
    await act(async () => {
      answer(json({ message: storedMessage(server.nextMessage++, { body: 'a', audience: { kind: 'principals', principal_ids: [2, 3] } }) }, 201));
      await Promise.resolve();
    });
    await screen.findByText('Sent');
    expect(to().value).toBe('whisper');
    expect(screen.getByText('next.png')).toBeDefined();
    server.post = accept;
    clickSend();
    await waitFor(() => { expect(server.posts).toHaveLength(2); });
    expect(server.posts[1]?.request).toEqual({ body: 'next.png', audience: toBot, payload: { schema: 'conch.file_attachment.v1', data: { file_ids: [70] } } });
  });
});

// Issue #1067: images kept across a sign-in that ended by itself follow the
// channel's id and the audience they were added for, as kept text does
// (issue #1039): never to a channel that only has the old name, never to a
// wider audience than the draft's.
describe('images kept across a sign-in that ended (issue #1067)', () => {
  const bot = { principal_id: 3, created_at: at, name: 'bot', kind: 'agent' };
  const ada = { principal_id: 2, created_at: at, name: 'ada', kind: 'human' };
  const to = () => screen.getByLabelText<HTMLSelectElement>('To:');
  const admitted = new WeakSet<FakeSocket>();

  async function letIn() {
    await waitFor(() => {
      for (const socket of FakeSocket.all) {
        if (admitted.has(socket)) continue;
        admitted.add(socket);
        socket.open();
      }
      expect(document.querySelector<HTMLTextAreaElement>('textarea')?.disabled).toBe(false);
    });
  }

  // The credential stops working when ops is opened; the person signs back in
  // with the same one. meanwhile runs while they are signed out; netsHeld,
  // when given, holds every nets answer after that until it resolves.
  async function expireThenSignIn(meanwhile: () => void = () => {}, netsHeld?: Promise<void>) {
    const real = globalThis.fetch;
    let expired = true;
    vi.stubGlobal('fetch', (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new NativeURL(typeof input === 'string' ? input : input instanceof NativeURL ? input.href : input.url, origin);
      if (expired && url.pathname === '/v2/channels/ops/messages') return Promise.resolve(json({}, 401));
      if (!expired && netsHeld !== undefined && /\/nets$/.test(url.pathname)) return netsHeld.then(() => real(input, init));
      return real(input, init);
    });
    fireEvent.click(screen.getByRole('button', { name: 'ops' }));
    await screen.findByLabelText('Credential');
    expired = false;
    meanwhile();
    FakeSocket.all = [];
    fireEvent.change(screen.getByLabelText('Credential'), { target: { value: secret } });
    fireEvent.click(screen.getByRole('button', { name: 'Use credential' }));
    await screen.findByRole('log', { name: 'Messages' });
  }

  async function openChannel(name: string) {
    fireEvent.click(await screen.findByRole('button', { name }));
    await letIn();
  }

  async function whisperToBot(withNet = false) {
    server.members = [ada, bot];
    await openChat();
    await screen.findByRole('option', { name: 'net alpha' });
    if (withNet) fireEvent.change(to(), { target: { value: 'net:5' } });
    fireEvent.change(to(), { target: { value: 'whisper' } });
    fireEvent.click(await screen.findByRole('checkbox', { name: 'bot [agent]' }));
    choose(image('secret.png'));
    await screen.findByText('secret.png');
  }

  it('an image kept for channel 7 is offered only there after a rename and a new channel of the old name', async () => {
    await openChat();
    choose(image('kept.png'));
    await screen.findByText('kept.png');
    await expireThenSignIn(() => {
      server.channels = [
        { id: 9, name: 'general', created_at: at },
        { id: 7, name: 'original', created_at: at },
        { id: 8, name: 'ops', created_at: at },
      ];
      server.history.original = [];
    });
    await openChannel('general');
    expect(screen.queryByText('kept.png')).toBeNull();
    await openChannel('original');
    await screen.findByText('kept.png');
    clickSend();
    await waitFor(() => { expect(server.posts).toHaveLength(1); });
    expect(server.uploads.map((u) => u.channel)).toEqual(['original']);
    expect(server.posts.map((p) => p.channel)).toEqual(['original']);
  });

  it('an image kept for a channel that is gone is dropped', async () => {
    await openChat();
    choose(image('gone.png'));
    await screen.findByText('gone.png');
    await expireThenSignIn(() => {
      server.channels = [{ id: 8, name: 'ops', created_at: at }, { id: 9, name: 'general', created_at: at }];
    });
    await openChannel('general');
    expect(screen.queryByText('gone.png')).toBeNull();
    expect(document.body.textContent).not.toContain('gone.png');
  });

  it('an image kept from a whisper draft comes back held, then goes to the same people and never to the channel', async () => {
    await whisperToBot();
    let release = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    await expireThenSignIn(() => {}, held);
    await openChannel('general');
    await screen.findByText(/Checking who this draft goes to/);
    expect(screen.queryByText('secret.png')).toBeNull();
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Send' }).disabled).toBe(true);
    await act(async () => {
      release();
      await Promise.resolve();
    });
    await screen.findByText('secret.png');
    await waitFor(() => {
      expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Send' }).disabled).toBe(false);
    });
    clickSend();
    await screen.findByText('Sent');
    expect(server.posts).toHaveLength(1);
    expect(server.posts[0]?.request.audience).toEqual({ kind: 'principals', principal_ids: [3] });
    expect(server.posts[0]?.request.payload).toEqual({ schema: 'conch.file_attachment.v1', data: { file_ids: [70] } });
  });

  it('an image kept from a whisper whose recipient is gone stays unsendable, and the channel never gets it', async () => {
    await whisperToBot();
    await expireThenSignIn(() => {
      server.members = [ada];
    });
    await openChannel('general');
    await screen.findByText(/no longer available/);
    expect(screen.queryByText('secret.png')).toBeNull();
    expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Send' }).disabled).toBe(true);
    fireEvent.change(to(), { target: { value: 'channel' } });
    expect(screen.queryByText('secret.png')).toBeNull();
    expect(server.uploads).toHaveLength(0);
    expect(server.posts).toHaveLength(0);
  });

  it('an image kept under a whisper that sits on a net keeps the net under it', async () => {
    await whisperToBot(true);
    await expireThenSignIn();
    await openChannel('general');
    await screen.findByText('secret.png');
    expect(JSON.parse(window.sessionStorage.getItem('conch.web.drafts') ?? '{}')).toMatchObject({
      targets: { '7': { net: 5, whisper: [3], protected: true } },
    });
    clickSend();
    await screen.findByText('Sent');
    expect(server.posts[0]?.request.audience).toEqual({ kind: 'principals', principal_ids: [3] });
  });
});
