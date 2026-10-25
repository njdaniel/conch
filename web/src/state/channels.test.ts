// The channel list's `renamed` (issue #728), and the re-keying of what the page
// holds by channel name: chat, drafts and who the next message goes to.
import { describe, expect, it } from 'vitest';
import type { ChannelV0 } from '../schema/channel';
import { channelsReducer, initialChannels, type ChannelsState } from './channels';
import { draftFor, draftsReducer, initialDrafts, initialTargets, targetFor, targetsReducer } from './drafts';
import { chatFor, chatReducer, initialChat, lastId, rekey } from './messages';

const at = '2026-10-21T00:00:00Z';
const general: ChannelV0 = { id: 1, name: 'general', created_at: at };
const ops: ChannelV0 = { id: 2, name: 'ops', created_at: at };
const dev: ChannelV0 = { id: 3, name: 'dev', created_at: at };

const listed = (selected: string | null, lost?: string): ChannelsState => ({
  channels: [general, ops, dev],
  loaded: true,
  selected,
  ...(lost === undefined ? {} : { lost }),
});

describe('channelsReducer renamed', () => {
  const cases: { name: string; from: ChannelsState; id: number; to: string; want: ChannelsState }[] = [
    {
      name: 'the open channel is renamed in place and stays open',
      from: listed('ops'),
      id: 2,
      to: 'ops-room',
      want: { channels: [general, { ...ops, name: 'ops-room' }, dev], loaded: true, selected: 'ops-room' },
    },
    {
      name: 'another channel is renamed and the open one stays',
      from: listed('general'),
      id: 3,
      to: 'engineering',
      want: { channels: [general, ops, { ...dev, name: 'engineering' }], loaded: true, selected: 'general' },
    },
    {
      name: 'a channel lost earlier is still said lost',
      from: listed('general', 'old'),
      id: 2,
      to: 'ops-room',
      want: { channels: [general, { ...ops, name: 'ops-room' }, dev], loaded: true, selected: 'general', lost: 'old' },
    },
  ];
  for (const c of cases) {
    it(c.name, () => {
      expect(channelsReducer(c.from, { type: 'renamed', id: c.id, name: c.to })).toEqual(c.want);
    });
  }

  it('changes nothing for an id not listed, or a name it already has', () => {
    const from = listed('ops');
    expect(channelsReducer(from, { type: 'renamed', id: 9, name: 'elsewhere' })).toBe(from);
    expect(channelsReducer(from, { type: 'renamed', id: 2, name: 'ops' })).toBe(from);
    expect(channelsReducer(initialChannels, { type: 'renamed', id: 2, name: 'ops-room' })).toBe(initialChannels);
  });

  it('a select of the old name afterwards opens nothing, and of the new name opens it', () => {
    const renamed = channelsReducer(listed('general'), { type: 'renamed', id: 2, name: 'ops-room' });
    expect(channelsReducer(renamed, { type: 'select', name: 'ops' }).selected).toBe('general');
    expect(channelsReducer(renamed, { type: 'select', name: 'ops-room' }).selected).toBe('ops-room');
  });

  it('a list read later that names the channel by its new name keeps it open', () => {
    const renamed = channelsReducer(listed('ops'), { type: 'renamed', id: 2, name: 'ops-room' });
    const reloaded = channelsReducer(renamed, {
      type: 'loaded',
      channels: [general, { ...ops, name: 'ops-room' }, dev],
      preferred: 'ops',
      remembered: 'ops-room',
    });
    expect(reloaded.selected).toBe('ops-room');
  });
});

describe('channelsReducer managed', () => {
  it('changes only the named channel and preserves every other standing', () => {
    const from: ChannelsState = { ...listed('general'), channels: [{ ...general, manages: false }, { ...ops, manages: true }, dev] };
    const granted = channelsReducer(from, { type: 'managed', id: 1, manages: true });
    expect(granted.channels).toEqual([{ ...general, manages: true }, { ...ops, manages: true }, dev]);
    expect(granted.selected).toBe('general');
    const revoked = channelsReducer(granted, { type: 'managed', id: 2, manages: false });
    expect(revoked.channels).toEqual([{ ...general, manages: true }, { ...ops, manages: false }, dev]);
  });

  it('returns the same state for an unknown channel or an unchanged grant', () => {
    const from = listed('general');
    expect(channelsReducer(from, { type: 'managed', id: 9, manages: true })).toBe(from);
    expect(channelsReducer(from, { type: 'managed', id: 1, manages: false })).toBe(from);
  });
});

describe('what is kept by name moves with a rename', () => {
  const message = (id: number) => ({ schema: 'conch.message.v2' as const, id, channel_id: 2, author_id: 3, created_at: at, body: `m${String(id)}` });

  it('chat: history and pending sends go under the new name, and the old name holds nothing', () => {
    let chat = chatReducer(initialChat, { type: 'historyLoaded', channel: 'ops', messages: [message(1), message(2)], more: false });
    chat = chatReducer(chat, { type: 'sending', channelId: 2, authorId: 3, channel: 'ops', body: 'hello', at: 1 });
    chat = chatReducer(chat, { type: 'historyLoaded', channel: 'general', messages: [message(5)], more: false });
    const renamed = chatReducer(chat, { type: 'renamed', from: 'ops', to: 'ops-room' });
    expect(chatFor(renamed, 'ops-room')).toBe(chatFor(chat, 'ops'));
    expect(lastId(chatFor(renamed, 'ops-room'))).toBe(2);
    expect(chatFor(renamed, 'ops-room').pending.map((p) => p.body)).toEqual(['hello']);
    expect(chatFor(renamed, 'ops').history).toBe('idle');
    expect(chatFor(renamed, 'general')).toBe(chatFor(chat, 'general'));
    // A message after the rename lands with the rest.
    const next = chatReducer(renamed, { type: 'received', channel: 'ops-room', message: message(3) });
    expect(lastId(chatFor(next, 'ops-room'))).toBe(3);
  });

  it('drafts and the next message’s audience go under the new name', () => {
    const drafts = draftsReducer(draftsReducer(initialDrafts, { type: 'edit', channel: 'ops', text: 'half a thought' }), {
      type: 'renamed',
      from: 'ops',
      to: 'ops-room',
    });
    expect(draftFor(drafts, 'ops-room')).toBe('half a thought');
    expect(draftFor(drafts, 'ops')).toBe('');
    const targets = targetsReducer(targetsReducer(initialTargets, { type: 'net', channel: 'ops', net: 4 }), {
      type: 'renamed',
      from: 'ops',
      to: 'ops-room',
    });
    expect(targetFor(targets, 'ops-room').net).toBe(4);
    expect(targetFor(targets, 'ops').net).toBeNull();
  });

  it('rekey leaves everything as it is when nothing is held under the old name, or the name is the same', () => {
    const held = { general: 1, ops: 2 };
    expect(rekey(held, 'dev', 'engineering')).toBe(held);
    expect(rekey(held, 'ops', 'ops')).toBe(held);
    expect(rekey(held, 'ops', 'ops-room')).toEqual({ general: 1, 'ops-room': 2 });
  });
});
