// Test support for the DM views: the Go golden fixtures of pkg/schema/testdata
// (the contract the server will be built to), and an API double whose every
// method answers from them unless a test says otherwise. Imported by tests
// only; it needs no network, no browser and no running conchd.
import { vi } from 'vitest';
import type { DMAcceptPolicy, DMConversationV1, DMFileV1, DMMessageV1, DMPreferencesV1, DMReadStateV1, DMSnapshotV1, DMState, ListDMMessagesResponseV1, SearchDMMessagesResponseV1 } from '../schema/generated';
import type { DmApi, DmFrame } from './api';

const files = import.meta.glob<unknown>('../../../pkg/schema/testdata/*.json', { eager: true, import: 'default' });

// The caller names the fixture's type; the loader cannot know it.
// eslint-disable-next-line @typescript-eslint/no-unnecessary-type-parameters
export function fixture<T>(name: string): T {
  const found = files[`../../../pkg/schema/testdata/${name}.json`];
  if (found === undefined) throw new Error(`no fixture ${name}`);
  return structuredClone(found) as T;
}

export const conversation = (): DMConversationV1 => fixture('dm-conversation-v1');
export const snapshot = (): DMSnapshotV1 => fixture('dm-snapshot-v1');
export const peerMessage = (): DMMessageV1 => fixture('dm-message-v1');
export const ownMessage = (): DMMessageV1 => fixture('dm-message-v1-own-files');
export const removedMessage = (): DMMessageV1 => fixture('dm-message-v1-removed');
export const file = (): DMFileV1 => fixture('dm-file-v1');

// The viewer of the fixtures is principal 3; the peer, 5 (Grace).
export const me = 3;

export interface Fake {
  readonly api: DmApi;
  readonly frames: (frame: DmFrame) => void;
  readonly listeners: Set<(frame: DmFrame) => void>;
}

// A deferred answer, for the tests of what arrives late or out of order.
export function later<T>(): { readonly promise: Promise<T>; readonly resolve: (value: T) => void } {
  let resolve: (value: T) => void = () => undefined;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

export function fake(over: Partial<DmApi> = {}): Fake {
  const listeners = new Set<(frame: DmFrame) => void>();
  const history = fixture<ListDMMessagesResponseV1>('list-dm-messages-response-v1');
  const api: DmApi = {
    snapshot: vi.fn(() => Promise.resolve({ ok: true as const, value: snapshot() })),
    list: vi.fn(() => Promise.resolve({ ok: true as const, value: { conversations: [] } })),
    open: vi.fn(() => Promise.resolve({ ok: true as const, value: conversation() })),
    get: vi.fn(() => Promise.resolve({ ok: true as const, value: conversation() })),
    history: vi.fn(() => Promise.resolve({ ok: true as const, value: structuredClone(history) })),
    changes: vi.fn(() => Promise.resolve({ ok: true as const, value: { messages: [] } })),
    send: vi.fn(() => Promise.resolve({ ok: true as const, message: ownMessage() })),
    edit: vi.fn(() => Promise.resolve({ ok: true as const, value: fixture<DMMessageV1>('dm-message-v1-edited') })),
    remove: vi.fn(() => Promise.resolve({ ok: true as const, value: null })),
    markRead: vi.fn((id: number, last: number) => Promise.resolve({ ok: true as const, value: { ...fixture<DMReadStateV1>('dm-read-state-v1'), conversation_id: id, last_read_id: last, unread: 0 } })),
    setState: vi.fn((id: number, state: DMState) => Promise.resolve({ ok: true as const, value: { ...conversation(), id, state } })),
    search: vi.fn(() => Promise.resolve({ ok: true as const, value: fixture<SearchDMMessagesResponseV1>('search-dm-messages-response-v1') })),
    fileMeta: vi.fn(() => Promise.resolve({ ok: true as const, value: file() })),
    uploadFile: vi.fn(() => Promise.resolve({ ok: true as const, value: file() })),
    fileBytes: vi.fn(() => Promise.resolve({ ok: true as const, value: new Blob(['x']) })),
    preferences: vi.fn(() => Promise.resolve({ ok: true as const, value: fixture<DMPreferencesV1>('dm-preferences-v1') })),
    setPreferences: vi.fn((accept: DMAcceptPolicy) => Promise.resolve({ ok: true as const, value: { accept } })),
    live: vi.fn((listener: (frame: DmFrame) => void) => {
      listeners.add(listener);
      return () => {
        listeners.delete(listener);
      };
    }),
    ...over,
  };
  return {
    api,
    listeners,
    frames: (frame) => {
      for (const listener of [...listeners]) listener(frame);
    },
  };
}
