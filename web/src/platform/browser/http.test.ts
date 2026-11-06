// The browser transport must make exactly the requests the client made before
// the platform seam existed: the same address, the same init, key for key.
// These are the arguments that api.ts passed to fetch then, written out.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { call, fileQuery, queryOf } from '../operations';
import { http } from './http';
import { sockets } from './sockets';

afterEach(() => {
  vi.unstubAllGlobals();
});

function stubFetch() {
  const fetchMock = vi.fn<typeof fetch>(() => Promise.resolve(new Response('{}')));
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

describe('the browser HTTP transport', () => {
  it('a bearer GET', async () => {
    const fetchMock = stubFetch();
    await http.get(call('listMessages', { channel: 'red team' }, queryOf({ order: 'desc', limit: '50' })), 'tok');
    expect(fetchMock.mock.calls).toEqual([
      [
        '/v2/channels/red%20team/messages?order=desc&limit=50',
        { headers: { Authorization: 'Bearer tok', Accept: 'application/json' }, cache: 'no-store', credentials: 'omit' },
      ],
    ]);
  });

  it('a bearer write with a JSON body, and without one', async () => {
    const fetchMock = stubFetch();
    await http.send(call('postMessage', { channel: 'general' }), 'tok', { body: { body: 'hi' }, keepalive: false });
    await http.send(call('logout'), 'tok', { keepalive: false });
    expect(fetchMock.mock.calls).toEqual([
      [
        '/v2/channels/general/messages',
        {
          method: 'POST',
          headers: { Authorization: 'Bearer tok', Accept: 'application/json', 'Content-Type': 'application/json' },
          body: '{"body":"hi"}',
          cache: 'no-store',
          credentials: 'omit',
          keepalive: false,
        },
      ],
      [
        '/v1/credentials/current',
        { method: 'DELETE', headers: { Authorization: 'Bearer tok', Accept: 'application/json' }, body: undefined, cache: 'no-store', credentials: 'omit', keepalive: false },
      ],
    ]);
  });

  it('a keepalive DELETE, and a write with a signal', async () => {
    const fetchMock = stubFetch();
    const controller = new AbortController();
    await http.send(call('endSession', { id: 4 }), 'tok', { keepalive: true });
    await http.send(call('voiceTransmit', { channel: 'general' }), 'tok', { body: { state: 'on' }, keepalive: false, signal: controller.signal });
    expect(fetchMock.mock.calls[0]).toEqual([
      '/v1/sessions/4',
      { method: 'DELETE', headers: { Authorization: 'Bearer tok', Accept: 'application/json' }, body: undefined, cache: 'no-store', credentials: 'omit', keepalive: true },
    ]);
    expect(fetchMock.mock.calls[1]?.[1]).toEqual({
      method: 'POST',
      headers: { Authorization: 'Bearer tok', Accept: 'application/json', 'Content-Type': 'application/json' },
      body: '{"state":"on"}',
      cache: 'no-store',
      credentials: 'omit',
      keepalive: false,
      signal: controller.signal,
    });
  });

  it('an open POST, an open GET and the health read send no credential', async () => {
    const fetchMock = stubFetch();
    await http.open(call('login'), { name: 'ada', password: 'pw' });
    await http.open(call('joinInfo'));
    await http.health(call('health'));
    expect(fetchMock.mock.calls).toEqual([
      [
        '/v1/login',
        {
          method: 'POST',
          headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
          body: '{"name":"ada","password":"pw"}',
          cache: 'no-store',
          credentials: 'omit',
        },
      ],
      ['/v1/join', { method: 'GET', headers: { Accept: 'application/json' }, cache: 'no-store', credentials: 'omit' }],
      ['/healthz', { headers: { Accept: 'application/json' }, cache: 'no-store', credentials: 'omit' }],
    ]);
  });

  it('a file upload, and a file download', async () => {
    const fetchMock = stubFetch();
    const bytes = new Blob(['png']);
    const controller = new AbortController();
    await http.upload(call('uploadFile', { channel: 'red team' }, fileQuery('screen shot.png')), 'tok', 'image/png', bytes, controller.signal);
    await http.upload(call('uploadFile', { channel: 'general' }, fileQuery('x')), 'tok', '', bytes, controller.signal);
    await http.download(call('fileBytes', { id: 9 }), 'tok');
    expect(fetchMock.mock.calls[0]).toEqual([
      '/v2/channels/red%20team/files?filename=screen%20shot.png',
      {
        method: 'POST',
        headers: { Authorization: 'Bearer tok', Accept: 'application/json', 'Content-Type': 'image/png' },
        body: bytes,
        cache: 'no-store',
        credentials: 'omit',
        signal: controller.signal,
      },
    ]);
    expect(fetchMock.mock.calls[1]?.[1]?.headers).toEqual({ Authorization: 'Bearer tok', Accept: 'application/json' });
    expect(fetchMock.mock.calls[2]).toEqual(['/v2/files/9', { headers: { Authorization: 'Bearer tok' }, cache: 'no-store', credentials: 'omit' }]);
  });
});

describe('the browser socket transport', () => {
  it('opens a ticketed WebSocket on the page’s own host, in the page’s scheme', () => {
    const opened: string[] = [];
    class FakeSocket {
      readonly address: string;
      constructor(address: string) {
        this.address = address;
        opened.push(address);
      }
    }
    vi.stubGlobal('WebSocket', FakeSocket);
    sockets.open('messagesSocket', { channel: 'general', after: '5', heartbeat: '1', ticket: 'tk' });
    sockets.open('occupancySocket', { ticket: 'tk' });
    expect(opened).toEqual([`ws://${window.location.host}/v2/ws?channel=general&after=5&heartbeat=1&ticket=tk`, `ws://${window.location.host}/v1/voice/occupancy/ws?ticket=tk`]);
  });
});
