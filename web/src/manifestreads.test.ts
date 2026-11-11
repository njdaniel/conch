// The generation that orders directory answers across views (issue #1562).
import { act, renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { useManifestReadControl } from './admin/manifestReads';
import type { ServerMemberV1 } from './schema/principal';

const agent = { id: 4, name: 'builder', kind: 'agent', role: 'member' } as unknown as ServerMemberV1;
const listed = [agent];
const hidden: ServerMemberV1[] = [];

function control() {
  return renderHook(({ changes }) => useManifestReadControl(true, changes), { initialProps: { changes: 0 } });
}

describe('manifest read control', () => {
  it('lets a stale answer that lists a hidden agent change nothing', () => {
    const { result } = control();
    const first = result.current.beginDirectory();
    const listing = result.current.beginDirectory();
    const second = result.current.beginDirectory();
    act(() => {
      result.current.acceptDirectory(listing, listed);
      expect(result.current.acceptDirectory(second, hidden)).toBe(true);
    });
    expect(result.current.mayRead(4)).toBe(false);
    act(() => {
      expect(result.current.acceptDirectory(first, listed)).toBe(false);
    });
    expect(result.current.mayRead(4)).toBe(false);
  });

  it('lets a stale answer that omits a re-enabled agent change nothing', () => {
    const { result } = control();
    const old = result.current.beginDirectory();
    const gone = result.current.beginDirectory();
    const back = result.current.beginDirectory();
    act(() => {
      result.current.acceptDirectory(gone, hidden);
      result.current.acceptDirectory(back, listed);
    });
    expect(result.current.mayRead(4)).toBe(true);
    act(() => {
      expect(result.current.acceptDirectory(old, hidden)).toBe(false);
    });
    expect(result.current.mayRead(4)).toBe(true);
  });

  it('keeps a 404 stop through a routine answer and ends it on an answer sent after a change', () => {
    const { result, rerender } = control();
    act(() => {
      result.current.stop(4, result.current.generation());
    });
    act(() => {
      result.current.acceptDirectory(result.current.beginDirectory(), listed);
    });
    expect(result.current.mayRead(4)).toBe(false);
    rerender({ changes: 1 });
    act(() => {
      result.current.acceptDirectory(result.current.beginDirectory(), listed);
    });
    expect(result.current.mayRead(4)).toBe(true);
  });

  it('ends the stop for a later change even when an older change\'s answer lands first', () => {
    const { result, rerender } = control();
    act(() => {
      result.current.stop(4, result.current.generation());
    });
    rerender({ changes: 1 });
    const hide = result.current.beginDirectory();
    rerender({ changes: 2 });
    const routine = result.current.beginDirectory();
    act(() => {
      result.current.acceptDirectory(hide, hidden);
      result.current.acceptDirectory(routine, listed);
    });
    expect(result.current.mayRead(4)).toBe(true);
  });

  it('never reads an agent the first applied answer omits, though an older answer lists it', () => {
    const { result } = control();
    const older = result.current.beginDirectory();
    const newer = result.current.beginDirectory();
    act(() => {
      result.current.acceptDirectory(newer, hidden);
    });
    act(() => {
      expect(result.current.acceptDirectory(older, listed)).toBe(false);
    });
    expect(result.current.mayRead(4)).toBe(false);
  });

  it('ignores a 404 sent before a recovery that has since cleared the stops', () => {
    const { result, rerender } = control();
    const sent = result.current.generation();
    rerender({ changes: 1 });
    act(() => {
      result.current.acceptDirectory(result.current.beginDirectory(), listed);
    });
    act(() => {
      result.current.stop(4, sent);
    });
    expect(result.current.mayRead(4)).toBe(true);
  });
});
