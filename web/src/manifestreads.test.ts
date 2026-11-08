// The generation that orders directory answers across views (issue #1562).
import { act, renderHook } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { useManifestReadControl } from './admin/manifestReads';
import type { ServerMemberV1 } from './schema/principal';

const agent = { id: 4, name: 'builder', kind: 'agent', role: 'member' } as unknown as ServerMemberV1;
const listed = [agent];
const hidden: ServerMemberV1[] = [];

describe('manifest read control', () => {
  it('lets a stale answer that lists a hidden agent change nothing', () => {
    const { result } = renderHook(() => useManifestReadControl(true));
    const first = result.current.beginDirectory();
    const listing = result.current.beginDirectory();
    const second = result.current.beginDirectory();
    act(() => {
      result.current.acceptDirectory(listing, listed, false);
      expect(result.current.acceptDirectory(second, hidden, false)).toBe(true);
    });
    expect(result.current.stopped.has(4)).toBe(true);
    act(() => {
      expect(result.current.acceptDirectory(first, listed, true)).toBe(false);
    });
    expect(result.current.stopped.has(4)).toBe(true);
  });

  it('lets a stale answer that omits a re-enabled agent change nothing', () => {
    const { result } = renderHook(() => useManifestReadControl(true));
    const old = result.current.beginDirectory();
    const gone = result.current.beginDirectory();
    const back = result.current.beginDirectory();
    act(() => {
      result.current.acceptDirectory(gone, hidden, false);
      result.current.acceptDirectory(back, listed, true);
    });
    expect(result.current.stopped.has(4)).toBe(false);
    act(() => {
      expect(result.current.acceptDirectory(old, hidden, false)).toBe(false);
    });
    expect(result.current.stopped.has(4)).toBe(false);
  });

  it('keeps a 404 stop through a routine answer and ends it on a recovery', () => {
    const { result } = renderHook(() => useManifestReadControl(true));
    act(() => {
      result.current.stop(4);
    });
    act(() => {
      result.current.acceptDirectory(result.current.beginDirectory(), listed, false);
    });
    expect(result.current.stopped.has(4)).toBe(true);
    act(() => {
      result.current.acceptDirectory(result.current.beginDirectory(), listed, true);
    });
    expect(result.current.stopped.has(4)).toBe(false);
  });
});
