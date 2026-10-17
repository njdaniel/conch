// A test double for the window's width: whether it is wide (the lobby's
// members column beside the conversation), and a change across that width.
// jsdom has no matchMedia of its own. Imported by tests only; vi.unstubAllGlobals
// takes it away again.
import { act } from '@testing-library/react';
import { vi } from 'vitest';

export function windowOf(wide: boolean): { readonly resize: (toWide: boolean) => void } {
  const listeners = new Set<() => void>();
  const query = {
    matches: wide,
    addEventListener: (_type: string, listener: () => void) => {
      listeners.add(listener);
    },
    removeEventListener: (_type: string, listener: () => void) => {
      listeners.delete(listener);
    },
  };
  vi.stubGlobal(
    'matchMedia',
    vi.fn(() => query),
  );
  return {
    resize(toWide: boolean) {
      query.matches = toWide;
      act(() => {
        for (const listener of listeners) listener();
      });
    },
  };
}
