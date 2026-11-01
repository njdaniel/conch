// The browser's own session across a credential's replacement (issue #1084).
// A password change ends the session on the server if it replaces the
// credential; the hook lets go of it before the write is sent and takes up
// again, or makes, a session once every write has settled.
import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type * as Api from '../api';
import { useOwnSession } from './useAgents';

type Listener = (event: Api.ReplacingEvent) => void;

const mocks = vi.hoisted(() => ({
  listeners: new Set<(event: unknown) => void>(),
  createSession: vi.fn(),
  endSession: vi.fn(),
}));

vi.mock('../api', () => ({
  createSession: mocks.createSession,
  endSession: mocks.endSession,
  onCredentialReplacing: (listener: (event: unknown) => void) => {
    mocks.listeners.add(listener);
    return () => {
      mocks.listeners.delete(listener);
    };
  },
}));

function tell(event: Parameters<Listener>[0]): void {
  act(() => {
    for (const listener of [...mocks.listeners]) listener(event);
  });
}
const begin = (id: number): void => {
  tell({ phase: 'begin', id });
};
const end = (id: number, rotated: boolean, late = false): void => {
  tell({ phase: 'end', id, rotated, late });
};

const unauthorized = (): void => undefined;
const made = (id: number) => ({ ok: true, value: { id } });

// later is a session creation the test answers when it chooses.
function later() {
  let give: (value: unknown) => void = () => undefined;
  const answer = new Promise((resolve) => {
    give = resolve;
  });
  return { answer, give };
}

async function ready(id: number) {
  const rendered = renderHook(() => useOwnSession(true, unauthorized));
  await vi.waitFor(() => {
    expect(rendered.result.current.session).toEqual({ phase: 'ready', id });
  });
  return rendered;
}

beforeEach(() => {
  mocks.listeners.clear();
  mocks.createSession.mockReset();
  mocks.endSession.mockReset();
  mocks.endSession.mockResolvedValue(undefined);
});
afterEach(() => {
  vi.useRealTimers();
});

describe('the own session across a credential replacement', () => {
  it('lets go of the session at once, and takes it up again when the write fails', async () => {
    mocks.createSession.mockResolvedValueOnce(made(1));
    const { result } = await ready(1);
    begin(1);
    expect(result.current.session).toEqual({ phase: 'starting' });
    end(1, false);
    expect(result.current.session).toEqual({ phase: 'ready', id: 1 });
    expect(mocks.createSession).toHaveBeenCalledTimes(1);
  });

  it('makes a new session with the new credential when the write replaced it', async () => {
    mocks.createSession.mockResolvedValueOnce(made(1)).mockResolvedValueOnce(made(2));
    const { result } = await ready(1);
    begin(1);
    end(1, true);
    await vi.waitFor(() => {
      expect(result.current.session).toEqual({ phase: 'ready', id: 2 });
    });
  });

  it('makes a new session when one write fails and an overlapping one rotates', async () => {
    mocks.createSession.mockResolvedValueOnce(made(1)).mockResolvedValueOnce(made(2));
    const { result } = await ready(1);
    begin(1);
    begin(2);
    // The first fails: the second is still out, so nothing resumes yet.
    end(1, false);
    expect(result.current.session).toEqual({ phase: 'starting' });
    end(2, true);
    await vi.waitFor(() => {
      expect(result.current.session).toEqual({ phase: 'ready', id: 2 });
    });
    expect(mocks.createSession).toHaveBeenCalledTimes(2);
  });

  it('makes a new session whichever order overlapping writes settle in', async () => {
    mocks.createSession.mockResolvedValueOnce(made(1)).mockResolvedValueOnce(made(2));
    const { result } = await ready(1);
    begin(1);
    begin(2);
    end(2, true);
    end(1, false);
    await vi.waitFor(() => {
      expect(result.current.session).toEqual({ phase: 'ready', id: 2 });
    });
  });

  it('does not install a session whose creation was answered across a rotation, and makes a fresh one', async () => {
    const first = later();
    mocks.createSession.mockReturnValueOnce(first.answer).mockResolvedValueOnce(made(2));
    const { result } = renderHook(() => useOwnSession(true, unauthorized));
    expect(result.current.session).toEqual({ phase: 'starting' });
    // The password change begins while session 1's creation is out.
    begin(1);
    end(1, true);
    await vi.waitFor(() => {
      expect(result.current.session).toEqual({ phase: 'ready', id: 2 });
    });
    // The delayed answer for the revoked credential's session comes now: it is
    // not installed, and nothing is asked of the server to end it.
    first.give(made(1));
    await act(async () => {
      await first.answer;
    });
    expect(result.current.session).toEqual({ phase: 'ready', id: 2 });
    expect(mocks.endSession).not.toHaveBeenCalled();
  });

  it('makes a fresh session when a write fails while a creation was out', async () => {
    const first = later();
    mocks.createSession.mockReturnValueOnce(first.answer).mockResolvedValueOnce(made(2));
    const { result } = renderHook(() => useOwnSession(true, unauthorized));
    begin(1);
    end(1, false);
    await vi.waitFor(() => {
      expect(result.current.session).toEqual({ phase: 'ready', id: 2 });
    });
    first.give(made(1));
    await act(async () => {
      await first.answer;
    });
    expect(result.current.session).toEqual({ phase: 'ready', id: 2 });
  });

  it('takes the session up again when a write is not answered, and makes a new one if it answers later with a new credential', async () => {
    mocks.createSession.mockResolvedValueOnce(made(1)).mockResolvedValueOnce(made(2));
    const { result } = await ready(1);
    begin(1);
    expect(result.current.session).toEqual({ phase: 'starting' });
    // The deadline: the api ends the write unreplaced.
    end(1, false);
    expect(result.current.session).toEqual({ phase: 'ready', id: 1 });
    // Answered later, with a new credential.
    end(1, true, true);
    await vi.waitFor(() => {
      expect(result.current.session).toEqual({ phase: 'ready', id: 2 });
    });
  });
});
