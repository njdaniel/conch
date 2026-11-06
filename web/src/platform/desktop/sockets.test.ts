// The desktop's live feeds, as the client uses them: subscriptions of the
// bridge, with no ticket, and the client's own feed state machine on top.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { openEventsSocket, openOccupancySocket, openSocket, openVoiceSocket } from '../../api';
import { Feed, firstDelay, type Clock, type FeedStatus } from '../../state/feed';
import type { ConchDesktopBridge, FeedHandlers, FeedRequest } from './bridge';
import { feedOf, sockets } from './sockets';

// The client under test runs on the desktop platform, as a desktop build's
// does (vite.config.ts aliases this module there).
vi.mock('#platform-selected', () => import('./index'));

interface Subscription {
  readonly feed: FeedRequest;
  readonly handlers: FeedHandlers;
  unsubscribed: number;
}

// A bridge whose only working function is subscribe; any other call (a
// ticket request above all) fails the test.
function install(): Subscription[] {
  const subscriptions: Subscription[] = [];
  const unexpected = (name: string) => () => Promise.reject(new Error(`unexpected bridge call: ${name}`));
  const bridge: ConchDesktopBridge = {
    serverAddress: unexpected('serverAddress'),
    request: unexpected('request'),
    subscribe: (feed, handlers) => {
      const sub: Subscription = { feed, handlers, unsubscribed: 0 };
      subscriptions.push(sub);
      return () => {
        sub.unsubscribed++;
      };
    },
    restore: unexpected('restore'),
    signInWithCredential: unexpected('signInWithCredential'),
    keepCredential: unexpected('keepCredential'),
    signInWithPassword: unexpected('signInWithPassword'),
    signInWithCode: unexpected('signInWithCode'),
    redeemInvite: unexpected('redeemInvite'),
    redeemInviteAsGuest: unexpected('redeemInviteAsGuest'),
    redeemSignInLink: unexpected('redeemSignInLink'),
    redeemSetupToken: unexpected('redeemSetupToken'),
    resetPassword: unexpected('resetPassword'),
    joinAsGuest: unexpected('joinAsGuest'),
    createAccount: unexpected('createAccount'),
    signOut: unexpected('signOut'),
    profiles: unexpected('profiles'),
    profileAdd: unexpected('profileAdd'),
    profileRename: unexpected('profileRename'),
    profileRemove: unexpected('profileRemove'),
    profileSwitch: unexpected('profileSwitch'),
    storageRetry: unexpected('storageRetry'),
    profilesRecover: unexpected('profilesRecover'),
    uploadFile: unexpected('uploadFile'),
    downloadFile: unexpected('downloadFile'),
    saveFile: unexpected('saveFile'),
  };
  Object.defineProperty(window, 'conchDesktop', { value: bridge, configurable: true, writable: true });
  return subscriptions;
}

afterEach(() => {
  Reflect.deleteProperty(window, 'conchDesktop');
});

const events = () => ({ onOpen: vi.fn(), onFrame: vi.fn(), onClose: vi.fn() });

describe('the desktop feed request', () => {
  it.each<[Parameters<typeof feedOf>[0], Record<string, string>, FeedRequest | null]>([
    ['messagesSocket', { channel: 'general', after: '41', heartbeat: '1' }, { kind: 'messages', channel: 'general', after: 41 }],
    ['eventsSocket', { channel: 'general', session: '9' }, { kind: 'events', channel: 'general', session: 9 }],
    ['voiceSocket', { channel: 'general' }, { kind: 'voice', channel: 'general' }],
    ['occupancySocket', {}, { kind: 'occupancy' }],
    ['wsTicket', {}, null],
    ['listMessages', { channel: 'general' }, null],
  ])('%s %j is %j', (id, query, wanted) => {
    expect(feedOf(id, query)).toEqual(wanted);
  });

  it('asks for no ticket: main opens the socket with the credential it holds', () => {
    expect(sockets.ticket).toBe(false);
  });
});

describe('a desktop feed connection', () => {
  it('reports the subscription’s open, frames and close as a WebSocket’s events', () => {
    const subs = install();
    const connection = sockets.open('occupancySocket', {});
    const seen: string[] = [];
    connection.onopen = (e) => seen.push(e.type);
    connection.onmessage = (e) => seen.push(String(e.data));
    connection.onclose = (e) => seen.push(`close ${String(e.code)} ${e.reason}`);
    const h = subs[0]?.handlers;
    h?.onOpen();
    h?.onFrame('{"schema":"x"}');
    h?.onClose(1013, 'the app fell behind');
    h?.onFrame('late');
    expect(seen).toEqual(['open', '{"schema":"x"}', 'close 1013 the app fell behind']);
  });

  it('ends the subscription once on close(), and reports nothing after', () => {
    const subs = install();
    const connection = sockets.open('voiceSocket', { channel: 'general' });
    const onmessage = vi.fn();
    connection.onmessage = onmessage;
    connection.close();
    connection.close();
    subs[0]?.handlers.onFrame('late');
    expect(subs[0]?.unsubscribed).toBe(1);
    expect(onmessage).not.toHaveBeenCalled();
  });

  it('throws, as a socket that cannot be made, without a bridge or for an operation that is not a feed', () => {
    expect(() => sockets.open('occupancySocket', {})).toThrow(TypeError);
    install();
    expect(() => sockets.open('listChannels', {})).toThrow(TypeError);
  });
});

describe('the client on the desktop', () => {
  it('opens each feed as a subscription with its parameters, and asks for no ticket', async () => {
    const subs = install();
    for (const result of [
      await openSocket('general', 41, events()),
      await openEventsSocket('general', 9, events()),
      await openVoiceSocket('general', events()),
      await openOccupancySocket(events()),
    ]) {
      expect(result.ok).toBe(true);
    }
    expect(subs.map((s) => s.feed)).toEqual([
      { kind: 'messages', channel: 'general', after: 41 },
      { kind: 'events', channel: 'general', session: 9 },
      { kind: 'voice', channel: 'general' },
      { kind: 'occupancy' },
    ]);
  });

  // The renderer's own reconnect, unchanged: main's 1013 (fell behind) is a
  // loss like any other, retried after the backoff from the newest id held;
  // a 1008 (the sign-in ended) is not retried.
  it('resubscribes after main closes a feed 1013, from the newest id, and stops at 1008', async () => {
    const subs = install();
    const timers: (() => void)[] = [];
    const delays: number[] = [];
    const clock: Clock = {
      setTimeout: (run, ms) => {
        if (ms === firstDelay || ms === firstDelay * 2) {
          delays.push(ms);
          timers.push(run);
        }
        return run;
      },
      clearTimeout: () => undefined,
    };
    const statuses: FeedStatus[] = [];
    let newest = 41;
    const feed = new Feed({ channel: 'general', open: openSocket, clock, after: () => newest, onFrame: () => undefined, onStatus: (s) => statuses.push(s) });
    feed.start();
    await vi.waitFor(() => {
      expect(subs).toHaveLength(1);
    });
    subs[0]?.handlers.onOpen();
    newest = 57;
    subs[0]?.handlers.onClose(1013, 'the app fell behind');
    expect(delays).toEqual([firstDelay]);
    timers.shift()?.();
    await vi.waitFor(() => {
      expect(subs).toHaveLength(2);
    });
    expect(subs[1]?.feed).toEqual({ kind: 'messages', channel: 'general', after: 57 });
    subs[1]?.handlers.onClose(1008, 'signed out');
    expect(statuses).toEqual(['connecting', 'open', 'waiting', 'connecting', 'denied']);
    feed.stop();
  });
});
