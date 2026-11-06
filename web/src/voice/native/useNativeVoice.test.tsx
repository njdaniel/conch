import { act, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import type { NativeVoiceView } from '../../platform/desktop/bridge';
import type { NativeVoice } from '../../platform/types';
import { useNativeVoice } from './useNativeVoice';

function store(initial: NativeVoiceView) {
  let view = initial;
  const ls = new Set<() => void>();
  const nv = {
    snapshot: () => view,
    subscribe: (l: () => void) => {
      ls.add(l);
      return () => {
        ls.delete(l);
      };
    },
  } as unknown as NativeVoice;
  return { nv, set: (v: NativeVoiceView) => {
      view = v;
      for (const l of ls) l();
    } };
}

function Probe({ source, binding }: { source: NativeVoice | undefined; binding?: string }) {
  const s = useNativeVoice(source, binding);
  return <p>{`${s.present ? 'present' : 'absent'}|${s.words.text}|${s.words.retry ? 'retry' : ''}`}</p>;
}

describe('useNativeVoice', () => {
  it('renders from the store and follows it', () => {
    const s = store({ status: 'idle', generation: 0, channel: null, retryable: false });
    render(<Probe source={s.nv} />);
    expect(screen.getByText('present|voice off|')).toBeTruthy();
    act(() => {
      s.set({ status: 'failed', generation: 1, channel: 'general', retryable: true, problem: 'connect_failed' });
    });
    expect(screen.getByText('present|voice failed: could not connect|retry')).toBeTruthy();
  });

  it('is absent without a native voice', () => {
    render(<Probe source={undefined} />);
    expect(screen.getByText(/^absent\|voice is not available here/)).toBeTruthy();
  });
});
