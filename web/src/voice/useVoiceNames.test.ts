// The roster's names while voice is in another channel than the one on screen
// (issue #588, F05): read from the joined channel's members, once, and again
// only for someone new; nothing read otherwise. The member read is a fake the
// test answers. No network.
import { act, renderHook } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ApiResult } from '../api';
import type { ChannelMemberV1 } from '../schema/channel';
import type { VoicePresenceV1 } from '../schema/voice';
import type { Person } from '../useLiveChannel';

// One member read the hook asked for, and how to answer it.
interface Read {
  readonly channel: string;
  readonly answer: (result: ApiResult<ChannelMemberV1[]>) => void;
}

const fake = vi.hoisted(() => ({ reads: [] as Read[] }));

vi.mock('../api', () => ({
  listMembers: (channel: string) =>
    new Promise<ApiResult<ChannelMemberV1[]>>((resolve) => {
      fake.reads.push({ channel, answer: resolve });
    }),
}));

import { useVoiceNames } from './useVoiceNames';

const presence = (people: number[]): VoicePresenceV1 => ({
  schema: 'conch.voice_presence.v1',
  channel_id: 1,
  configured: true,
  available: true,
  rooms: [{ participants: people.map((principal_id) => ({ principal_id, can_publish: true, transmitting: false, joined_at: '2026-10-18T00:00:00Z' })) }],
});

const member = (principal_id: number, name?: string): ChannelMemberV1 => ({
  principal_id,
  created_at: '2026-10-18T00:00:00Z',
  ...(name === undefined ? {} : { name, kind: 'human' as const }),
});

// The people of the channel on screen: the viewer (1) and the operator (9).
const viewedPeople: ReadonlyMap<number, Person> = new Map([
  [1, { name: 'alice', kind: 'human' }],
  [9, { name: 'operator', kind: 'human' }],
]);

interface Props {
  readonly joined: string | null;
  readonly viewed: string | null;
  readonly presence: VoicePresenceV1 | null;
}

function hook(initial: Props) {
  return renderHook((p: Props) => useVoiceNames({ ...p, people: viewedPeople }), { initialProps: initial });
}

async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 10; i++) await Promise.resolve();
  });
}

async function answer(i: number, result: ApiResult<ChannelMemberV1[]>): Promise<void> {
  const read = fake.reads[i];
  if (read === undefined) throw new Error(`no read ${String(i)}`);
  read.answer(result);
  await flush();
}

beforeEach(() => {
  fake.reads.length = 0;
});

describe('useVoiceNames', () => {
  it('reads nothing while not in voice', async () => {
    const { result } = hook({ joined: null, viewed: 'book-club', presence: presence([1, 2]) });
    await flush();
    expect(fake.reads).toEqual([]);
    expect(result.current).toBe(viewedPeople);
  });

  it('reads nothing while joined in the channel on screen', async () => {
    const { result, rerender } = hook({ joined: 'game-night', viewed: 'game-night', presence: presence([1, 2]) });
    await flush();
    rerender({ joined: 'game-night', viewed: 'game-night', presence: presence([1, 2, 3]) });
    await flush();
    expect(fake.reads).toEqual([]);
    expect(result.current).toBe(viewedPeople);
  });

  it('reads the joined channel once while joined elsewhere, and names its people', async () => {
    const { result, rerender } = hook({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2]) });
    await flush();
    expect(fake.reads.map((r) => r.channel)).toEqual(['game-night']);
    await answer(0, { ok: true, value: [member(1, 'alice'), member(2, 'bob')] });
    expect(result.current.get(2)?.name).toBe('bob');
    expect(result.current.get(9)?.name).toBe('operator');
    // The same people again, another channel on screen, a share starting: no
    // new read.
    rerender({ joined: 'game-night', viewed: 'film-club', presence: presence([1, 2]) });
    rerender({ joined: 'game-night', viewed: 'book-club', presence: presence([2, 1]) });
    await flush();
    expect(fake.reads).toHaveLength(1);
    expect(result.current.get(2)?.name).toBe('bob');
  });

  it('reads again for a principal not yet known, and not a second time while one is in flight', async () => {
    const { result, rerender } = hook({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2]) });
    await flush();
    await answer(0, { ok: true, value: [member(1, 'alice'), member(2, 'bob')] });

    rerender({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2, 3]) });
    await flush();
    expect(fake.reads).toHaveLength(2);
    // Someone else arrives while that read is out: no second one yet.
    rerender({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2, 3, 4]) });
    await flush();
    expect(fake.reads).toHaveLength(2);
    // The read in flight knew carol only; dave, who came during it, is read
    // for once it is back.
    await answer(1, { ok: true, value: [member(1, 'alice'), member(2, 'bob'), member(3, 'carol')] });
    expect(result.current.get(3)?.name).toBe('carol');
    expect(fake.reads).toHaveLength(3);
    await answer(2, { ok: true, value: [member(1, 'alice'), member(2, 'bob'), member(3, 'carol'), member(4, 'dave')] });
    expect(result.current.get(4)?.name).toBe('dave');

    // At most one read for each new principal: one the list does not name
    // (or does not list) is not read for again.
    rerender({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2, 3, 4, 5]) });
    await flush();
    expect(fake.reads).toHaveLength(4);
    await answer(3, { ok: true, value: [member(1, 'alice'), member(2, 'bob'), member(3, 'carol'), member(4, 'dave')] });
    rerender({ joined: 'game-night', viewed: 'book-club', presence: presence([5, 4, 3, 2, 1]) });
    await flush();
    expect(fake.reads).toHaveLength(4);
    expect(result.current.has(5)).toBe(false);
  });

  it('falls back when the read fails: nothing named, and the rest unchanged', async () => {
    const { result, rerender } = hook({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2]) });
    await flush();
    await answer(0, { ok: false, failure: 'network' });
    expect(result.current.has(2)).toBe(false);
    expect(result.current.get(1)?.name).toBe('alice');
    expect(result.current.get(9)?.name).toBe('operator');
    // Not retried for the same people.
    rerender({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2]) });
    await flush();
    expect(fake.reads).toHaveLength(1);
  });

  it('drops the names on leave, and reads afresh on the next join', async () => {
    const { result, rerender } = hook({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2]) });
    await flush();
    await answer(0, { ok: true, value: [member(1, 'alice'), member(2, 'bob')] });
    expect(result.current.get(2)?.name).toBe('bob');

    rerender({ joined: null, viewed: 'book-club', presence: presence([1]) });
    await flush();
    expect(result.current).toBe(viewedPeople);

    rerender({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2]) });
    await flush();
    expect(result.current.has(2)).toBe(false);
    expect(fake.reads).toHaveLength(2);
  });

  it('drops a read for a channel left before it answered', async () => {
    const { result, rerender } = hook({ joined: 'game-night', viewed: 'book-club', presence: presence([1, 2]) });
    await flush();
    rerender({ joined: null, viewed: 'book-club', presence: null });
    await flush();
    await answer(0, { ok: true, value: [member(1, 'alice'), member(2, 'bob')] });
    expect(result.current).toBe(viewedPeople);
  });
});
