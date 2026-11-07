import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import * as api from './api';
import { toggleCollapsed, addGroup, toggleFavorite } from './arrangement';
import type { ChannelV0 } from './schema/channel';
import { useArrangement } from './useArrangement';

const ch = (id: number, name: string): ChannelV0 => ({ id, name, created_at: '2026-10-18T00:00:00.000Z' });
const mine = [ch(1, 'general'), ch(2, 'games'), ch(3, 'books')];
const origin = 'http://one.test';

afterEach(() => {
  for (const [o, a] of [[origin, 1], [origin, 2], ['http://two.test', 1]] as const) api.writeArrangement(o, a, null);
});

describe('useArrangement', () => {
  it('keeps an arrangement for one account on one server and nothing bleeds to another', () => {
    const view = renderHook(({ o, a }) => useArrangement(o, a, mine, true), { initialProps: { o: origin, a: 1 } });
    act(() => { view.result.current.edit((x) => toggleFavorite(x, 2)); });
    expect(view.result.current.arrangement.favorites).toEqual([2]);
    expect(api.readArrangement(origin, 1)).toEqual({ favorites: [2], groups: [] });
    view.rerender({ o: origin, a: 2 });
    expect(view.result.current.arrangement.favorites).toEqual([]);
    view.rerender({ o: 'http://two.test', a: 1 });
    expect(view.result.current.arrangement.favorites).toEqual([]);
    view.rerender({ o: origin, a: 1 });
    expect(view.result.current.arrangement.favorites).toEqual([2]);
  });

  it('survives a reload: collapsed state is read back', () => {
    const first = renderHook(() => useArrangement(origin, 1, mine, true));
    act(() => { first.result.current.edit((x) => addGroup(x, 'Play')); });
    act(() => { first.result.current.edit((x) => toggleCollapsed(x, 'g1')); });
    first.unmount();
    const again = renderHook(() => useArrangement(origin, 1, mine, true));
    expect(again.result.current.arrangement.groups[0]).toMatchObject({ name: 'Play', collapsed: true });
  });

  it('never draws, counts or keeps a channel that is no longer held', () => {
    api.writeArrangement(origin, 1, { favorites: [2, 3], groups: [] });
    const view = renderHook(({ list }) => useArrangement(origin, 1, list, true), { initialProps: { list: mine } });
    expect(view.result.current.arrangement.favorites).toEqual([2, 3]);
    view.rerender({ list: mine.filter((c) => c.id !== 3) });
    expect(view.result.current.arrangement.favorites).toEqual([2]);
    expect(api.readArrangement(origin, 1)).toEqual({ favorites: [2], groups: [] });
  });

  it('does not prune before the channel list has been read', () => {
    api.writeArrangement(origin, 1, { favorites: [2], groups: [] });
    const view = renderHook(({ loaded }) => useArrangement(origin, 1, loaded ? mine : [], loaded), { initialProps: { loaded: false } });
    expect(api.readArrangement(origin, 1)).toEqual({ favorites: [2], groups: [] });
    view.rerender({ loaded: true });
    expect(view.result.current.arrangement.favorites).toEqual([2]);
  });

  it('reads corrupt stored data as nothing arranged and writes nothing for no account', () => {
    api.writeArrangement(origin, 1, 'not an arrangement');
    const view = renderHook(() => useArrangement(origin, 1, mine, true));
    expect(view.result.current.arrangement).toEqual({ favorites: [], groups: [] });
    const nobody = renderHook(() => useArrangement(origin, 0, mine, true));
    act(() => { nobody.result.current.edit((x) => toggleFavorite(x, 1)); });
    expect(api.readArrangement(origin, 0)).toBeNull();
  });
});
